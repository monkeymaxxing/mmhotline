package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type workflowRig struct {
	t       *testing.T
	dir     string
	app     *App
	server  *httptest.Server
	handler atomic.Value
	clock   atomic.Int64
}

func newWorkflowRig(t *testing.T) *workflowRig {
	t.Helper()
	h := &workflowRig{t: t, dir: t.TempDir()}
	now := time.Now().UTC()
	h.clock.Store(time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).Unix())
	if err := Setup(context.Background(), h.dir, "Fixture team"); err != nil {
		t.Fatal(err)
	}
	h.open()
	recordCommitSigningKeyForTest(t, h.app)
	recordEmailSetUpForTest(t, h.app.Store)
	h.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/alert-intake/") || strings.HasPrefix(r.URL.Path, "/check-in/") {
			h.app.IntakeHandler().ServeHTTP(w, r)
		} else {
			h.handler.Load().(http.Handler).ServeHTTP(w, r)
		}
	}))
	t.Cleanup(func() { h.server.Close(); h.app.Store.Close() })
	return h
}

func (h *workflowRig) open() {
	h.t.Helper()
	store, err := Open(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	h.app = New(store)
	configureAccessSignIn(h.t, h.app)
	h.app.Now = func() time.Time { return time.Unix(h.clock.Load(), 0).UTC() }
	h.app.ContactMethodTypes = map[string]ContactMethodType{"email": EmailContactMethodType{}}
	h.handler.Store(h.app.Handler())
}

func (h *workflowRig) restart() {
	h.t.Helper()
	if err := h.app.Store.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open()
}

type workflowBrowser struct {
	h      *workflowRig
	client *http.Client
}

func (h *workflowRig) browser() *workflowBrowser {
	clientValue := *h.server.Client()
	client := &clientValue
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	return &workflowBrowser{h: h, client: client}
}

func (b *workflowBrowser) get(path string, want int) string {
	b.h.t.Helper()
	res, err := b.client.Get(b.h.server.URL + path)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		b.h.t.Fatal(err)
	}
	if res.StatusCode != want {
		b.h.t.Fatalf("GET %s: got %d, want %d: %s", path, res.StatusCode, want, body)
	}
	return string(body)
}

func (b *workflowBrowser) post(path string, form url.Values, want int) string {
	b.h.t.Helper()
	res, err := b.client.PostForm(b.h.server.URL+path, form)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		b.h.t.Fatal(err)
	}
	if res.StatusCode != want {
		b.h.t.Fatalf("POST %s: got %d, want %d: %s", path, res.StatusCode, want, body)
	}
	return string(body)
}

func workflowField(t *testing.T, body, name string) string {
	t.Helper()
	match := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("field %s not in page", name)
	}
	return html.UnescapeString(match[1])
}

func (b *workflowBrowser) addPerson(signInEmailAddress, name string) Principal {
	b.h.t.Helper()
	p := memberFixture(b.h.t, b.h.app, signInEmailAddress).Principal
	if _, err := b.h.app.ChangePerson(context.Background(), Principal{UnixUser: "fixture"}, signInEmailAddress, &name, nil); err != nil {
		b.h.t.Fatal(err)
	}
	b.identify(signInEmailAddress)
	return p
}
func (b *workflowBrowser) identify(signInEmailAddress string) {
	useSignInEmailAddress(b.client, signInEmailAddress)
}

type workflowEmailSink struct {
	EmailContactMethodType
	mu       sync.Mutex
	messages []ContactMethodSend
}

func (s *workflowEmailSink) Send(_ context.Context, m ContactMethodSend) DeliveryResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	return DeliveryResult{State: "accepted", IDFromSendingService: "fixture-" + m.DeliveryID}
}
func (s *workflowEmailSink) snapshot() []ContactMethodSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ContactMethodSend{}, s.messages...)
}

func (h *workflowRig) drain(sink *workflowEmailSink) {
	h.t.Helper()
	for i := 0; i < 25; i++ {
		work, err := dispatchWith(h.app, h.server.URL, sink)
		if err != nil {
			h.t.Fatal(err)
		}
		if !work {
			return
		}
	}
	h.t.Fatal("outbox did not become idle after 25 bounded attempts")
}

type workflowReceipt struct {
	ID         string `json:"mmhotline_alert_id"`
	IncidentID string `json:"mmhotline_incident_id"`
	Duplicate  bool   `json:"already_received"`
}

func (h *workflowRig) intake(source, secret string, body map[string]string) workflowReceipt {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	key, err := hex.DecodeString(secret)
	if err != nil {
		h.t.Fatal(err)
	}
	stamp := strconv.FormatInt(h.clock.Load(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(stamp + "."))
	mac.Write(raw)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/alert-intake/"+source, bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mmhotline-Signature", "timestamp_unix_seconds="+stamp+",hmac_sha256_hex="+hex.EncodeToString(mac.Sum(nil)))
	res, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	response, err := io.ReadAll(res.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if res.StatusCode != 200 {
		h.t.Fatalf("signed intake: %d %s", res.StatusCode, response)
	}
	var receipt workflowReceipt
	if err = json.Unmarshal(response, &receipt); err != nil {
		h.t.Fatal(err)
	}
	if receipt.ID == "" {
		h.t.Fatal("intake omitted committed alert identity")
	}
	return receipt
}

func TestPublicWorkflowEmailSOSClaimResolutionAndRestart(t *testing.T) {
	h := newWorkflowRig(t)
	ctx := context.Background()
	owner := h.browser()
	p := owner.addPerson("owner@example.com", "Alex <Admin>")
	created, err := createAlertSource(ctx, h.app, p, "Fleet source")
	if err != nil {
		t.Fatal(err)
	}
	sourceID, secret := created.Source.ID, created.Secret
	if strings.Contains(owner.get("/alert-sources", 200), secret) {
		t.Fatal("source secret was exposed again on listing")
	}
	forged, err := http.NewRequest(http.MethodPost, h.server.URL+"/alert-intake/"+sourceID, strings.NewReader(`{"title":"forged","message":""}`))
	if err != nil {
		t.Fatal(err)
	}
	forged.Header.Set("Content-Type", "application/json")
	forged.Header.Set("Mmhotline-Signature", "timestamp_unix_seconds="+strconv.FormatInt(h.clock.Load(), 10)+",hmac_sha256_hex="+strings.Repeat("0", 64))
	response, err := h.server.Client().Do(forged)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if page := owner.get("/alert-sources", 200); response.StatusCode != 401 || !strings.Contains(page, "Rejected requests in the last 24 hours: 1") || !strings.Contains(page, "Wrong signature: 1") {
		t.Fatalf("rejected request panel after a forged request (%d)", response.StatusCode)
	}
	page := owner.get("/contact-methods", 200)
	owner.post("/contact-methods/save", url.Values{"csrf": {workflowField(t, page, "csrf")}, "contact_method_type": {"email"}, "email_address": {"responder@example.com"}, "send_while_unavailable_or_muted": {"true"}, "severity": {"sos"}}, 303)
	owner.get("/contact-methods", 200)
	contactMethods, err := h.app.ContactMethods(ctx, p)
	if err != nil || len(contactMethods) != 1 {
		t.Fatalf("contact method creation: %+v %v", contactMethods, err)
	}
	sink := &workflowEmailSink{}
	h.drain(sink)
	messages := sink.snapshot()
	if len(messages) != 1 || messages[0].Purpose != "contact_method_test" || !strings.Contains(messages[0].Text, "Alex <Admin> added this contact method") {
		t.Fatalf("save test: %+v", messages)
	}

	title := "Storage <script>alert(1)</script> & cold backup"
	alert := map[string]string{"sender_alert_id": "alert-sos-1", "sender_incident_key": "disk-pressure", "severity": "sos", "title": title, "message": "<img src=x onerror=alert(1)> Disk full on worker A"}
	receipt := h.intake(sourceID, secret, alert)
	if receipt.IncidentID == "" || receipt.Duplicate {
		t.Fatalf("initial SOS: %+v", receipt)
	}
	duplicate := h.intake(sourceID, secret, alert)
	if !duplicate.Duplicate || duplicate.ID != receipt.ID || duplicate.IncidentID != receipt.IncidentID {
		t.Fatalf("idempotent receipt: %+v %+v", receipt, duplicate)
	}
	h.drain(sink)
	messages = sink.snapshot()
	if len(messages) != 2 || messages[1].SentTo != "responder@example.com" || !strings.Contains(messages[1].Text, "/incidents/"+receipt.IncidentID) {
		t.Fatalf("SOS email dispatch: %+v", messages)
	}
	for _, path := range []string{"/dashboard", "/alerts/" + receipt.ID, "/incidents/" + receipt.IncidentID} {
		page = owner.get(path, 200)
		if strings.Contains(page, "<script>alert(1)</script>") || !strings.Contains(page, "Storage &lt;script&gt;") {
			t.Fatalf("alert title not escaped at %s", path)
		}
	}
	page = owner.get("/alerts/"+receipt.ID, 200)
	if strings.Contains(page, "<img src=x") || !strings.Contains(page, "&lt;img src=x") {
		t.Fatal("alert message is not escaped")
	}
	page = owner.get("/incidents/"+receipt.IncidentID, 200)
	csrf1, revision := workflowField(t, page, "csrf"), workflowField(t, page, "revision")
	second := h.browser()
	second.identify("owner@example.com")
	otherPage := second.get("/incidents/"+receipt.IncidentID, 200)
	csrf2 := workflowField(t, otherPage, "csrf")
	if workflowField(t, otherPage, "revision") != revision {
		t.Fatal("browsers did not observe the same initial incident")
	}
	owner.post("/incidents/"+receipt.IncidentID+"/claim", url.Values{"csrf": {csrf1}, "revision": {revision}}, 303)
	second.post("/incidents/"+receipt.IncidentID+"/claim", url.Values{"csrf": {csrf2}, "revision": {revision}}, 409)
	incident, err := h.app.GetIncident(ctx, p, receipt.IncidentID)
	if err != nil || incident.Status != "claimed" {
		t.Fatalf("claim not retained: %+v %v", incident, err)
	}
	ownerID := incident.OwnerMemberID
	// The person who claimed it is not offered to take it over from
	// themselves, and claiming their own claim changes nothing.
	if page = owner.get("/incidents/"+receipt.IncidentID, 200); strings.Contains(page, "Take over this incident") || strings.Contains(page, "Claim this incident") || !strings.Contains(page, "Resolve") {
		t.Fatal("the claimer is offered to claim or take over their own incident")
	}
	again, err := h.app.ClaimIncident(ctx, p, receipt.IncidentID, incident.Revision, "")
	if err != nil || again.Revision != incident.Revision {
		t.Fatalf("claiming your own claim: %+v %v", again, err)
	}

	h.restart()
	h.clock.Add(int64((2 * time.Hour).Seconds()))
	if err = h.app.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	incident, err = h.app.GetIncident(ctx, p, receipt.IncidentID)
	if err != nil || incident.Status != "claimed" || incident.OwnerMemberID != ownerID {
		t.Fatalf("claimed incident changed on recovery: %+v %v", incident, err)
	}
	h.drain(sink)
	if len(sink.snapshot()) != 2 {
		t.Fatal("restart reannounced a claimed SOS")
	}
	if !strings.Contains(owner.get("/incidents/"+receipt.IncidentID, 200), "Claimed") {
		t.Fatal("Sign-in access or claimed page failed after restart")
	}
	resolution := map[string]string{"sender_alert_id": "alert-resolved-1", "sender_incident_key": "disk-pressure", "severity": "resolved-sos", "title": "Disk recovered", "message": "Space available"}
	resolved := h.intake(sourceID, secret, resolution)
	repeated := h.intake(sourceID, secret, resolution)
	if resolved.IncidentID != receipt.IncidentID || !repeated.Duplicate || repeated.ID != resolved.ID {
		t.Fatalf("resolution identity: %+v %+v", resolved, repeated)
	}
	h.drain(sink)
	messages = sink.snapshot()
	if len(messages) != 3 || !strings.HasPrefix(messages[2].Subject, "[Resolved SOS]") {
		t.Fatalf("resolution was not notified exactly once: %+v", messages)
	}
	h.restart()
	h.clock.Add(int64((2 * time.Hour).Seconds()))
	if err = h.app.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	h.drain(sink)
	incident, err = h.app.GetIncident(ctx, p, receipt.IncidentID)
	if err != nil || incident.Status != "resolved" || len(sink.snapshot()) != 3 {
		t.Fatalf("resolved recovery: %+v sends=%d %v", incident, len(sink.snapshot()), err)
	}
	if !strings.HasPrefix(liveSectionText(owner.get("/dashboard", 200), "all-clear"), "All clear Nothing needs a person.") {
		t.Fatal("resolved incident stayed on dashboard")
	}
	owner.post("/incidents/"+receipt.IncidentID+"/claim", url.Values{"csrf": {csrf1}, "revision": {revision}}, 409)
	date := h.app.Now().Format("2006-01-02")
	page = owner.get("/alerts?"+url.Values{"q": {"Disk recovered"}, "severity": {"resolved-sos"}, "from": {date}, "until": {date}}.Encode(), 200)
	if !strings.Contains(page, "/alerts/"+resolved.ID) || strings.Contains(page, "/alerts/"+receipt.ID) {
		t.Fatal("history text/severity/date filter returned the wrong alerts")
	}
	page = owner.get("/alerts?"+url.Values{"from": {h.app.Now().AddDate(0, 0, 1).Format("2006-01-02")}}.Encode(), 200)
	if !strings.Contains(page, "No alerts match") {
		t.Fatal("future date filter returned old alerts")
	}
	owner.get("/alerts?from=not-a-date", 400)
}
