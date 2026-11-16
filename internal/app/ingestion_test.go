package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func alertFixture(t *testing.T) (*App, Principal, CreatedSource) {
	t.Helper()
	a := foundationApp(t)
	a.Now = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	s := memberFixture(t, a, "intake@example.test")
	source, err := createAlertSource(context.Background(), a, s.Principal, "Backup")
	if err != nil {
		t.Fatal(err)
	}
	return a, s.Principal, source
}
func alertSignature(secret string, at time.Time, body []byte) string {
	key, err := hex.DecodeString(secret)
	if err != nil {
		panic(err)
	}
	timestamp := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "timestamp_unix_seconds=" + timestamp + ",hmac_sha256_hex=" + hex.EncodeToString(mac.Sum(nil))
}
func intakeHTTP(a *App, p Principal, source CreatedSource, body string, at time.Time) *httptest.ResponseRecorder {
	mux := a.IntakeHandler()
	r := httptest.NewRequest("POST", "/alert-intake/"+source.Source.ID, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Mmhotline-Signature", alertSignature(source.Secret, at, []byte(body)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
func alertReceipt(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("intake: %d %s", w.Code, w.Body.String())
	}
	var v struct {
		ID string `json:"mmhotline_alert_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}
func scalarTeam(t *testing.T, a *App, query string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow(query, args...).Scan(&v) }); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestSignedIntakeValidationReplayAndRotation(t *testing.T) {
	a, p, s := alertFixture(t)
	now := a.Now()
	body := `{"sender_alert_id":"one","title":"Backup","message":""}`
	id := alertReceipt(t, intakeHTTP(a, p, s, body, now))
	alert, err := a.GetAlert(context.Background(), p, id)
	if err != nil || alert.Severity != "warn" || alert.Notes != missingSeverityNote {
		t.Fatalf("normalization: %+v %v", alert, err)
	}
	if got := alertReceipt(t, intakeHTTP(a, p, s, body, now)); got != id {
		t.Fatal("duplicate got new identity")
	}
	keyless := `{"title":"Keyless","message":"body"}`
	k := alertReceipt(t, intakeHTTP(a, p, s, keyless, now))
	if alertReceipt(t, intakeHTTP(a, p, s, keyless, now)) != k {
		t.Fatal("signature replay duplicated")
	}
	invalid := []string{`{"title":"x","message":"","severity":""}`, `{"title":"x","message":"","severity":"other"}`, `{"title":"x","message":null}`, `{"title":"x","message":"","sender_alert_id":""}`, `{"title":"x","message":"","sender_incident_key":""}`, `{"Title":"x","message":""}`, `{"title":"x","title":"y","message":""}`, `{"title":"x","message":"","other":1}`, `{"title":"x","message":""} {}`, `null`}
	for _, body := range invalid {
		if w := intakeHTTP(a, p, s, body, now); w.Code != 400 {
			t.Errorf("invalid %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := intakeHTTP(a, p, s, `{"title":"x","message":"`+strings.Repeat("x", MaxAlertBody)+`"}`, now); w.Code != 413 || w.Header().Get("Mmhotline-Receipt-Signature") != "" {
		t.Fatalf("oversize: %d", w.Code)
	}
	if w := intakeHTTP(a, p, s, body, now.Add(-301*time.Second)); w.Code != 401 {
		t.Fatalf("expired: %d", w.Code)
	}
	bad := s
	bad.Secret = strings.Repeat("00", 32)
	if w := intakeHTTP(a, p, bad, body, now); w.Code != 401 {
		t.Fatalf("bad signature: %d", w.Code)
	}
	newKey, err := addSigningSecret(context.Background(), a, p, s.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	alertReceipt(t, intakeHTTP(a, p, newKey, `{"sender_alert_id":"newkey","title":"rotated","message":""}`, now))
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"oldkey","title":"old","message":""}`, now))
	if _, err = addSigningSecret(context.Background(), a, p, s.Source.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("third key: %v", err)
	}
	if err = revokeSigningSecret(context.Background(), a, p, s.Source.ID, s.SigningSecretID); err != nil {
		t.Fatal(err)
	}
	if w := intakeHTTP(a, p, s, body, now); w.Code != 401 {
		t.Fatalf("revoked key: %d", w.Code)
	}
	if err = disableAlertSource(context.Background(), a, p, s.Source.ID); err != nil {
		t.Fatal(err)
	}
	if w := intakeHTTP(a, p, newKey, body, now); w.Code != 401 {
		t.Fatalf("revoked source: %d", w.Code)
	}
	if got := scalarTeam(t, a, `SELECT count(*) FROM alerts`); got != 4 {
		t.Fatalf("rejected input mutated alerts: %d", got)
	}
}
func TestSOSCorrelationAndImmutableHistory(t *testing.T) {
	a, p, s := alertFixture(t)
	giveWakingContactMethod(t, a, p.SignInEmailAddress)
	ctx := context.Background()
	now := a.Now()
	firstID := alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"failure1","sender_incident_key":"backup","severity":"sos","title":"Failure","message":"disk"}`, now))
	first, err := a.GetAlert(ctx, p, firstID)
	if err != nil {
		t.Fatal(err)
	}
	secondID := alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"failure2","sender_incident_key":"backup","severity":"sos","title":"Still failed","message":"disk"}`, now))
	second, _ := a.GetAlert(ctx, p, secondID)
	if second.IncidentID != first.IncidentID {
		t.Fatal("correlation created a second incident")
	}
	i, err := a.GetIncident(ctx, p, first.IncidentID)
	if err != nil || i.RepeatGeneration != 1 {
		t.Fatalf("repeat on correlated alert: %+v %v", i, err)
	}
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"recovered","sender_incident_key":"backup","severity":"resolved-sos","title":"Recovery","message":"fixed_person"}`, now))
	i, _ = a.GetIncident(ctx, p, first.IncidentID)
	if i.Status != "resolved" {
		t.Fatal("source resolution failed")
	}
	freshID := alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"failure3","sender_incident_key":"backup","severity":"sos","title":"Again","message":"disk"}`, now))
	fresh, _ := a.GetAlert(ctx, p, freshID)
	if fresh.IncidentID == first.IncidentID {
		t.Fatal("resolved incident reopened")
	}
	orphanID := alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"orphan","sender_incident_key":"missing","severity":"resolved-sos","title":"No match","message":""}`, now))
	orphan, _ := a.GetAlert(ctx, p, orphanID)
	if orphan.IncidentID != "" || !strings.Contains(orphan.Notes, "missing") {
		t.Fatalf("unmatched: %+v", orphan)
	}
	audit, err := a.IncidentAudit(ctx, p, first.IncidentID)
	if err != nil || len(audit) != 4 || audit[0].Kind != "opened" || audit[3].Kind != "resolved" {
		t.Fatalf("audit: %+v %v", audit, err)
	}
	alerts, err := a.ListAlerts(ctx, p, AlertFilter{Query: "failure", Limit: 1})
	if err != nil || len(alerts) != 1 || alerts[0].ID != first.ID {
		t.Fatalf("query: %+v %v", alerts, err)
	}
	alerts, err = a.ListAlerts(ctx, p, AlertFilter{Before: fresh.Sequence, Severity: "sos", From: now, Until: now.Add(time.Second)})
	if err != nil || len(alerts) != 2 {
		t.Fatalf("paging/date: %+v %v", alerts, err)
	}
	dir := a.Store.Dir
	if err = a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	a.Store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := a.GetAlert(ctx, p, first.ID)
	if err != nil || after.ID != first.ID {
		t.Fatalf("restart: %+v %v", after, err)
	}
}
func TestIntakeRollsBackAlertIncidentAndRateWithOutbox(t *testing.T) {
	a, p, s := alertFixture(t)
	ctx := context.Background()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		member, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		id := NewID()
		if _, err = tx.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) VALUES(?,?,'email','{"email_address":"person@example.test"}','person@example.test',1,1,0)`, id, member.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,'sos')`, id); err != nil {
			return err
		}
		_, err = tx.Exec(`CREATE TRIGGER fail_delivery BEFORE INSERT ON deliveries BEGIN SELECT RAISE(ABORT,'fixture failed write'); END;`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	w := intakeHTTP(a, p, s, `{"sender_alert_id":"atomic","severity":"sos","title":"SOS","message":""}`, a.Now())
	if w.Code != 503 || w.Header().Get("Mmhotline-Receipt-Signature") != "" {
		t.Fatalf("failed write status: %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"alerts", "incidents", "incident_actions", "source_rates", "deliveries"} {
		if got := scalarTeam(t, a, `SELECT count(*) FROM `+table); got != 0 {
			t.Fatalf("partial admission in %s: %d", table, got)
		}
	}
}
func TestSourceRateLimitPreservesAcceptedRetry(t *testing.T) {
	a, p, s := alertFixture(t)
	var first string
	for n := 0; n < SourceAlertsPerMinute; n++ {
		body := fmt.Sprintf(`{"sender_alert_id":"%d","title":"subscribed_alert","message":""}`, n)
		id := alertReceipt(t, intakeHTTP(a, p, s, body, a.Now()))
		if n == 0 {
			first = id
		}
	}
	if w := intakeHTTP(a, p, s, `{"sender_alert_id":"extra","title":"subscribed_alert","message":""}`, a.Now()); w.Code != 429 {
		t.Fatalf("limit: %d", w.Code)
	}
	if id := alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"0","title":"subscribed_alert","message":""}`, a.Now())); id != first {
		t.Fatal("accepted retry denied or changed")
	}
}

// The Alert sources page escapes an alert source's name and never shows a
// signing secret.
func TestAlertSourcesPageEscapesNamesAndShowsNoSecret(t *testing.T) {
	a, p, _ := alertFixture(t)
	source, err := createAlertSource(context.Background(), a, p, "<script>not code</script>")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, p.SignInEmailAddress)
	resp, err := client.Get(server.URL + "/alert-sources")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("listing: %d %v", resp.StatusCode, err)
	}
	if bytes.Contains(body, []byte("<script>not code</script>")) || !bytes.Contains(body, []byte("&lt;script&gt;not code&lt;/script&gt;")) || bytes.Contains(body, []byte(source.Secret)) || bytes.Contains(body, []byte("<form")) {
		t.Fatalf("listing: %s", body)
	}
}

func TestIntakeSignatureNamesAndReceiptFields(t *testing.T) {
	a, p, source := alertFixture(t)
	body := `{"sender_alert_id":"one","sender_incident_key":"disk","severity":"sos","title":"Disk full","message":""}`
	valid := alertSignature(source.Secret, a.Now(), []byte(body))
	parts := strings.Split(valid, ",")
	for _, signature := range []string{
		strings.ReplaceAll(strings.ReplaceAll(valid, "timestamp_unix_seconds=", "t="), "hmac_sha256_hex=", "v1="),
		parts[1] + "," + parts[0], parts[0] + "," + parts[0], parts[1] + "," + parts[1], valid + "," + parts[1], " " + valid,
	} {
		request := httptest.NewRequest("POST", "/alert-intake/"+source.Source.ID, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Mmhotline-Signature", signature)
		response := httptest.NewRecorder()
		a.IntakeHandler().ServeHTTP(response, request)
		if response.Code != 401 || response.Body.String() != "source authentication failed\n" {
			t.Fatalf("signature %q: %d %s", signature, response.Code, response.Body.String())
		}
	}
	var firstAlertID, firstIncidentID string
	for _, alreadyReceived := range []bool{false, true} {
		response := intakeHTTP(a, p, source, body, a.Now())
		var receipt map[string]any
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil {
			t.Fatalf("receipt: %d %s", response.Code, response.Body.String())
		}
		if len(receipt) != 3 || receipt["already_received"] != alreadyReceived || receipt["mmhotline_alert_id"] == "" || receipt["mmhotline_incident_id"] == "" {
			t.Fatalf("receipt: %+v", receipt)
		}
		if alreadyReceived && (receipt["mmhotline_alert_id"] != firstAlertID || receipt["mmhotline_incident_id"] != firstIncidentID) {
			t.Fatal("duplicate changed identities")
		}
		firstAlertID = receipt["mmhotline_alert_id"].(string)
		firstIncidentID = receipt["mmhotline_incident_id"].(string)
	}
	recorded, err := a.GetAlert(context.Background(), p, firstAlertID)
	if err != nil || recorded.SenderAlertID != "one" || recorded.SenderIncidentKey != "disk" {
		t.Fatalf("sender fields: %+v %v", recorded, err)
	}
	ordinary := intakeHTTP(a, p, source, `{"sender_alert_id":"ordinary","title":"Notice","message":""}`, a.Now())
	var receipt map[string]any
	if json.Unmarshal(ordinary.Body.Bytes(), &receipt) != nil || len(receipt) != 2 || receipt["mmhotline_incident_id"] != nil {
		t.Fatalf("ordinary receipt: %s", ordinary.Body.String())
	}
	for _, body := range []string{`{"id":"old","title":"x","message":""}`, `{"incident":"old","title":"x","message":""}`} {
		if response := intakeHTTP(a, p, source, body, a.Now()); response.Code != 400 {
			t.Fatalf("retired request fields accepted: %s", body)
		}
	}
	for _, field := range []string{"sender_alert_id", "sender_incident_key"} {
		response := intakeHTTP(a, p, source, fmt.Sprintf(`{"%s":"","title":"x","message":""}`, field), a.Now())
		if response.Code != 400 || !strings.Contains(response.Body.String(), "sender_alert_id and sender_incident_key must be nonempty when present") {
			t.Fatalf("field error: %d %s", response.Code, response.Body.String())
		}
	}
	for _, severity := range []string{`"invalid"`, `""`, `null`, `123`, `true`, `[]`, `{}`} {
		response := intakeHTTP(a, p, source, fmt.Sprintf(`{"severity":%s,"title":"x","message":""}`, severity), a.Now())
		if response.Code != 400 || !strings.Contains(response.Body.String(), "severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos") {
			t.Fatalf("severity %s error: %d %s", severity, response.Code, response.Body.String())
		}
	}
}

func TestSenderRoutesUseInstallationTeam(t *testing.T) {
	a, _, source := alertFixture(t)
	handler := a.IntakeHandler()
	for _, item := range []struct{ route, body string }{
		{"alert-intake", `{"title":"Route test","message":""}`},
		{"check-in", `{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"}`},
	} {
		for _, oldRoute := range []bool{false, true} {
			path := "/" + item.route + "/" + source.Source.ID
			want := http.StatusOK
			if oldRoute {
				path = "/" + item.route + "/extra/" + source.Source.ID
				want = http.StatusNotFound
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(item.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Mmhotline-Signature", alertSignature(source.Secret, a.Now(), []byte(item.body)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != want {
				t.Fatalf("%s: %d %s, want %d", path, response.Code, response.Body.String(), want)
			}
			if !oldRoute && response.Header().Get("Mmhotline-Receipt-Signature") == "" {
				t.Fatalf("unsigned receipt for %s", path)
			}
		}
	}
}
