package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

func TestPublicInternetAddress(t *testing.T) {
	t.Parallel()
	for address, public := range map[string]bool{
		"93.184.216.34": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "255.255.255.255": false,
		"224.0.0.1": false, "::1": false, "fe80::1": false, "fd00::1": false, "::": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "64:ff9b::a00:1": false, "2002:a00:1::": false,
		"198.18.0.1": false, "192.0.2.1": false,
		"::127.0.0.1": false, "::ffff:0:7f00:1": false, "fec0::1": false, "100::1": false, "3fff::1": false, "5f00::1": false,
	} {
		if got := publicInternetAddress(netip.MustParseAddr(address)); got != public {
			t.Errorf("%s: public=%v, want %v", address, got, public)
		}
	}
}

func TestWebhookSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	check := func(fields map[string]string) (ContactMethodSettings, error) {
		return OutgoingWebhookContactMethodType{}.CheckSettings(ctx, ContactMethodSettingsInput{Fields: fields})
	}
	for _, bad := range []string{"", "http://example.com/hook", "https://user:pw@example.com/", "https://127.0.0.1/hook", "https://[::1]/hook", "https://10.0.0.5/hook", "ftp://example.com", "https://example.com/#x", "example.com/hook"} {
		if _, err := check(map[string]string{"outgoing_webhook_url": bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if _, err := check(map[string]string{"outgoing_webhook_url": "https://example.com/hook", "outgoing_webhook_bearer_token": "has space"}); !errors.Is(err, ErrInvalid) {
		t.Error("bearer token with a space accepted")
	}
	s, err := check(map[string]string{"outgoing_webhook_url": "https://example.com/hook?token=secret", "outgoing_webhook_bearer_token": "abc123"})
	if err != nil || s.SentTo != "https://example.com/…" || strings.Contains(s.JSON, "abc123") || !strings.Contains(s.Secret, "abc123") {
		t.Fatalf("settings %+v %v", s, err)
	}
	var secrets webhookSecrets
	if json.Unmarshal([]byte(s.Secret), &secrets) != nil || len(secrets.SigningSecrets) != 1 || len(secrets.SigningSecrets[0].SecretHex) != 64 {
		t.Fatalf("signing secret: %+v", secrets)
	}
	fake := OutgoingWebhookContactMethodType{AllowedPrivateURLPrefix: "http://127.0.0.1:8090/webhook-receiver/"}
	if _, err := fake.CheckSettings(ctx, ContactMethodSettingsInput{Fields: map[string]string{"outgoing_webhook_url": "http://127.0.0.1:8090/webhook-receiver/ops"}}); err != nil {
		t.Fatalf("fake receiver refused: %v", err)
	}
	if _, err := fake.CheckSettings(ctx, ContactMethodSettingsInput{Fields: map[string]string{"outgoing_webhook_url": "http://127.0.0.1:8091/other"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("private address outside the fake receiver accepted")
	}
}

// webhookRig is a team with one webhook contact method pointing at the fake
// webhook receiver.
type webhookRig struct {
	a                 *App
	s                 personFixture
	fake              *fakesendingservices.Server
	contactMethodType OutgoingWebhookContactMethodType
	method            ContactMethod
	secret            string
	advance           func(time.Duration)
}

func newWebhookRig(t *testing.T, subscriptions ...string) *webhookRig {
	t.Helper()
	fake := fakesendingservices.New()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	contactMethodType := OutgoingWebhookContactMethodType{AllowedPrivateURLPrefix: server.URL + "/webhook-receiver/"}
	a.ContactMethodTypes = map[string]ContactMethodType{"outgoing_webhook": contactMethodType}
	a.WebURL = "https://mmhotline.example.com"
	s := memberFixture(t, a, "webhook@example.test")
	c, err := a.SaveContactMethod(context.Background(), s.Principal, ContactMethodInput{ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": server.URL + "/webhook-receiver/ops", "outgoing_webhook_bearer_token": "bearer-abc"}, Shared: true, Subscriptions: subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	finishSaveTest(t, a, s.Principal, contactMethodType)
	shown := webhookSecretShownOnce(c)
	if shown == nil {
		t.Fatalf("new webhook: %+v", c)
	}
	return &webhookRig{a: a, s: s, fake: fake, contactMethodType: contactMethodType, method: c, secret: shown.SecretHex, advance: advance}
}

func (r *webhookRig) dispatch(t *testing.T) {
	t.Helper()
	if _, err := r.a.DispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookDeliverySignedBodyAndHeaders(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "info", "warn", "sos")
	e, err := r.a.ManualSOS(context.Background(), r.s.Principal, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	requests := r.fake.WebhookRequests()
	if len(requests) != 1 {
		t.Fatalf("requests: %+v", requests)
	}
	got := requests[0]
	if !fakesendingservices.VerifyWebhookSignature(r.secret, got.Signature, got.Body) || got.Authorization != "Bearer bearer-abc" || got.DeliveryID == "" {
		t.Fatalf("headers: %+v", got)
	}
	var body map[string]string
	if err = json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"mmhotline_delivery_id": got.DeliveryID, "delivery_purpose": "subscribed_alert", "severity": "sos", "title": "Database down", "message": "Connection refused", "mmhotline_alert_id": e.ID, "mmhotline_incident_id": e.IncidentID, "incident_status": "open", "incident_url": "https://mmhotline.example.com/incidents/" + e.IncidentID, "alert_url": "https://mmhotline.example.com/alerts/" + e.ID, "alert_received_at": r.a.Now().UTC().Format(time.RFC3339)}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("%s = %q, want %q", key, body[key], value)
		}
	}
	if _, present := body["notes_from_mmhotline"]; present || len(body) != len(want) {
		t.Errorf("unexpected fields: %v", body)
	}
	var d Delivery
	deliveries, err := r.a.Deliveries(context.Background(), r.s.Principal, 10)
	for _, candidate := range deliveries {
		if candidate.ContactMethodType == "outgoing_webhook" && candidate.Purpose == "subscribed_alert" {
			d = candidate
		}
	}
	if err != nil || d.State != "accepted" || d.OutcomeLabel() != "Accepted by the webhook" || d.Reason != "HTTP 200" {
		t.Fatalf("delivery %+v %v", d, err)
	}
}

// A team without Cloudflare opens MMHotline through an SSH forward, so its web
// URL is http://localhost; alerts must still go out, with links to that URL.
func TestDeliveryWithLocalhostWebURL(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "sos")
	r.a.WebURL = "http://localhost:8480"
	e, err := r.a.ManualSOS(context.Background(), r.s.Principal, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	requests := r.fake.WebhookRequests()
	if len(requests) != 1 {
		t.Fatalf("requests: %+v", requests)
	}
	var body map[string]string
	if err = json.Unmarshal([]byte(requests[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["incident_url"] != "http://localhost:8480/incidents/"+e.IncidentID {
		t.Fatalf("incident_url = %q", body["incident_url"])
	}
}

func TestWebhookRetriesSameBodyAndStopsOnRefusal(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn")
	r.fake.SetWebhookAnswer("ops", 503, false)
	queueDelivery(t, r.a, r.s.Principal, "retried", "warn")
	r.dispatch(t)
	d := findDelivery(t, r.a, r.s.Principal, "retried")
	if d.State != "failed" || d.NextAttempt.IsZero() || d.Reason != "The webhook answered HTTP 503" {
		t.Fatalf("after 503: %+v", d)
	}
	r.fake.SetWebhookAnswer("ops", 200, false)
	r.advance(5 * time.Second)
	r.dispatch(t)
	requests := r.fake.WebhookRequests()
	if len(requests) != 2 || requests[0].Body != requests[1].Body || requests[0].DeliveryID != requests[1].DeliveryID {
		t.Fatalf("retry changed the body: %+v", requests)
	}
	if d = findDelivery(t, r.a, r.s.Principal, "retried"); d.State != "accepted" {
		t.Fatalf("after retry: %+v", d)
	}
	r.fake.SetWebhookAnswer("ops", 404, false)
	queueDelivery(t, r.a, r.s.Principal, "refused", "warn")
	r.dispatch(t)
	if d = findDelivery(t, r.a, r.s.Principal, "refused"); d.State != "failed" || !d.NextAttempt.IsZero() || d.Reason != "Refused by the webhook (HTTP 404)" {
		t.Fatalf("after 404: %+v", d)
	}
}

func TestWebhookNeverConnectsToPrivateAddresses(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	receiver := httptest.NewServer(fakesendingservices.New())
	defer receiver.Close()
	a.ContactMethodTypes = map[string]ContactMethodType{"outgoing_webhook": OutgoingWebhookContactMethodType{}}
	a.WebURL = "https://mmhotline.example.com"
	s := memberFixture(t, a, "private@example.test")
	// localhost passes the settings check (it is a name), but resolves to a
	// loopback address, which the connection refuses.
	port := receiver.URL[strings.LastIndex(receiver.URL, ":"):]
	if _, err := a.SaveContactMethod(context.Background(), s.Principal, ContactMethodInput{ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": "https://localhost" + port + "/webhook-receiver/ops"}, Subscriptions: []string{"warn"}}); err != nil {
		t.Fatal(err)
	}
	finishSaveTest(t, a, s.Principal, a.ContactMethodTypes["outgoing_webhook"])
	queueDelivery(t, a, s.Principal, "private", "warn")
	if _, err := a.DispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := findDelivery(t, a, s.Principal, "private"); d.State != "failed" || !d.NextAttempt.IsZero() || d.Reason != "Not sent: the webhook address is not a public internet address" {
		t.Fatalf("private address: %+v", d)
	}
}

func TestFailingWebhookKeepsOnlyNewestAndTellsAdministratorsOnce(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn")
	r.fake.SetWebhookAnswer("ops", 500, false)
	for _, key := range []string{"one", "two", "three", "four"} {
		queueDelivery(t, r.a, r.s.Principal, key, "warn")
		r.advance(time.Second)
	}
	for range 3 {
		r.dispatch(t)
	}
	for _, key := range []string{"one", "two", "three"} {
		if d := findDelivery(t, r.a, r.s.Principal, key); d.State != "cancelled" || !strings.HasPrefix(d.Reason, "Not sent: the contact method has been failing since ") {
			t.Fatalf("%s: %+v", key, d)
		}
	}
	if d := findDelivery(t, r.a, r.s.Principal, "four"); d.State != "queued" {
		t.Fatalf("newest was not kept: %+v", d)
	}
	notices := func() int {
		var n int
		err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT count(*) FROM alerts WHERE source_id='system' AND title LIKE 'MMHotline cannot deliver alerts to %'`).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if notices() != 1 {
		t.Fatalf("administrator notices: %d", notices())
	}
	var toWebhook int
	if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM deliveries d JOIN alerts a ON a.id=d.alert_id WHERE a.source_id='system' AND d.contact_method_type='outgoing_webhook'`).Scan(&toWebhook)
	}); err != nil || toWebhook != 0 {
		t.Fatalf("a notice about the webhook went to a webhook: %d %v", toWebhook, err)
	}
	r.advance(5 * time.Minute)
	r.dispatch(t)
	if notices() != 1 {
		t.Fatal("administrators told twice about one failing streak")
	}
	r.fake.SetWebhookAnswer("ops", 200, false)
	r.advance(5 * time.Minute)
	r.dispatch(t)
	if d := findDelivery(t, r.a, r.s.Principal, "four"); d.State != "accepted" {
		t.Fatalf("recovered webhook: %+v", d)
	}
	c, err := r.a.ContactMethods(context.Background(), r.s.Principal)
	if err != nil {
		t.Fatal(err)
	}
	var failures int
	if err = r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT failures_in_a_row FROM contact_methods WHERE id=?`, c[0].ID).Scan(&failures)
	}); err != nil || failures != 0 {
		t.Fatalf("failure count not reset: %d %v", failures, err)
	}
}

func TestWebhookSigningSecretRotationAndBearerToken(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn")
	ctx := context.Background()
	p := r.s.Principal
	ids, err := r.a.WebhookSigningSecretIDs(ctx, p, r.method.ID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("ids %v %v", ids, err)
	}
	if err = r.a.RevokeWebhookSigningSecret(ctx, p, r.method.ID, ids[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked the only secret: %v", err)
	}
	second, err := r.a.AddWebhookSigningSecret(ctx, p, r.method.ID)
	if err != nil || len(second.SecretHex) != 64 {
		t.Fatal(err)
	}
	if _, err = r.a.AddWebhookSigningSecret(ctx, p, r.method.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("a third secret was added")
	}
	queueDelivery(t, r.a, p, "during-rotation", "warn")
	r.dispatch(t)
	if err = r.a.RevokeWebhookSigningSecret(ctx, p, r.method.ID, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err = r.a.SetWebhookBearerToken(ctx, p, r.method.ID, ""); err != nil {
		t.Fatal(err)
	}
	queueDelivery(t, r.a, p, "after-rotation", "warn")
	r.dispatch(t)
	requests := r.fake.WebhookRequests()
	if len(requests) != 2 {
		t.Fatalf("requests %+v", requests)
	}
	if !fakesendingservices.VerifyWebhookSignature(r.secret, requests[0].Signature, requests[0].Body) || requests[0].Authorization != "Bearer bearer-abc" {
		t.Fatal("the oldest secret did not sign during rotation")
	}
	if !fakesendingservices.VerifyWebhookSignature(second.SecretHex, requests[1].Signature, requests[1].Body) || requests[1].Authorization != "" {
		t.Fatalf("after rotation: %+v", requests[1])
	}
	for _, key := range []string{"during-rotation", "after-rotation"} {
		if d := findDelivery(t, r.a, p, key); d.State != "accepted" {
			t.Fatalf("%s: %+v", key, d)
		}
	}
}

func TestWebhookLimitPerTeam(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn")
	for i := 1; i < 20; i++ {
		if _, err := r.a.SaveContactMethod(context.Background(), r.s.Principal, ContactMethodInput{ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": r.contactMethodType.AllowedPrivateURLPrefix + "more"}, Subscriptions: []string{"warn"}}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := r.a.SaveContactMethod(context.Background(), r.s.Principal, ContactMethodInput{ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": r.contactMethodType.AllowedPrivateURLPrefix + "more"}, Subscriptions: []string{"warn"}}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("21st webhook: %v", err)
	}
}

func TestAdministratorNoticesNeverGoToWebhooks(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn")
	ctx := context.Background()
	personal, err := r.a.SaveContactMethod(ctx, r.s.Principal, ContactMethodInput{ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": r.contactMethodType.AllowedPrivateURLPrefix + "personal"}, Subscriptions: []string{"warn"}})
	if err != nil {
		t.Fatal(err)
	}
	email := savedEmail(t, r.a, r.s.Principal, true)
	if err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return RecordAdministratorNotice(ctx, tx, "warn", "Something about MMHotline", "Details", r.a.Now())
	}); err != nil {
		t.Fatal(err)
	}
	var contactMethodTypes []string
	if err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT d.contact_method_id FROM deliveries d JOIN alerts a ON a.id=d.alert_id WHERE a.title='Something about MMHotline'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return err
			}
			contactMethodTypes = append(contactMethodTypes, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(contactMethodTypes) != 1 || contactMethodTypes[0] != email.ID || contactMethodTypes[0] == personal.ID || contactMethodTypes[0] == r.method.ID {
		t.Fatalf("notice went to %v", contactMethodTypes)
	}
}

func TestWebhookThroughTheContactMethodsPage(t *testing.T) {
	t.Parallel()
	h := newWorkflowRig(t)
	fake := fakesendingservices.New()
	server := httptest.NewServer(fake)
	defer server.Close()
	h.app.ContactMethodTypes["outgoing_webhook"] = OutgoingWebhookContactMethodType{AllowedPrivateURLPrefix: server.URL + "/webhook-receiver/"}
	owner := h.browser()
	owner.addPerson("owner@example.com", "Alex")
	page := owner.get("/contact-methods", 200)
	page = owner.post("/contact-methods/save", url.Values{"csrf": {workflowField(t, page, "csrf")}, "contact_method_type": {"outgoing_webhook"}, "outgoing_webhook_url": {server.URL + "/webhook-receiver/ops?token=hidden"}, "outgoing_webhook_bearer_token": {"bearer-xyz"}, "shared_with_team": {"true"}, "severity": {"warn"}}, 200)
	secret := regexp.MustCompile(`Webhook signing secret \(hex\): ([a-f0-9]{64})`).FindStringSubmatch(pageText(page))
	if len(secret) != 2 || !strings.Contains(pageText(page), "Save your webhook signing secret now") {
		t.Fatal("the new webhook's signing secret was not shown")
	}
	page = owner.get("/contact-methods", 200)
	// The full URL stays in the edit form for the person who may edit it; the
	// destination shown to everyone is the host only, and the signing secret
	// and bearer token are never shown again.
	if !strings.Contains(pageText(page), server.URL+"/…") {
		t.Fatal("the webhook's destination does not hide its path and query")
	}
	for _, hidden := range []string{secret[1], "bearer-xyz"} {
		if strings.Contains(page, hidden) {
			t.Fatalf("%q shown again", hidden)
		}
	}
	for _, text := range []string{"Outgoing webhook", "(signs requests)", "Add a second webhook signing secret (for rotation)", "Webhook bearer token (optional)"} {
		if !strings.Contains(page, text) {
			t.Fatalf("missing %q", text)
		}
	}
	page = owner.post("/contact-methods/save", url.Values{"csrf": {workflowField(t, page, "csrf")}, "contact_method_type": {"outgoing_webhook"}, "outgoing_webhook_url": {"https://example.com/hook"}, "severity": {"warn"}}, 400)
	if !strings.Contains(page, "can only point at the fake webhook receiver") {
		t.Fatal("development mode accepted a real webhook URL")
	}
}

func TestWebhookAnswersThatRetryAndThatFail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		state  string
		retry  bool
		reason string
	}{
		{"redirect", 302, "failed", false, "Refused by the webhook (HTTP 302)"},
		{"request timeout", 408, "failed", true, "The webhook answered HTTP 408"},
		{"too many requests", 429, "failed", true, "The webhook answered HTTP 429"},
		{"server error", 500, "failed", true, "The webhook answered HTTP 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWebhookRig(t, "warn")
			r.fake.SetWebhookAnswer("ops", tc.status, false)
			queueDelivery(t, r.a, r.s.Principal, "answer", "warn")
			r.dispatch(t)
			d := findDelivery(t, r.a, r.s.Principal, "answer")
			if d.State != tc.state || d.NextAttempt.IsZero() == tc.retry || d.Reason != tc.reason {
				t.Fatalf("%+v", d)
			}
		})
	}
}

func TestWebhookIncidentStatusIsCopiedWhenQueued(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "sos")
	r.fake.SetWebhookAnswer("ops", 503, false)
	e, err := r.a.ManualSOS(context.Background(), r.s.Principal, "Database down", "")
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	i, err := r.a.GetIncident(context.Background(), r.s.Principal, e.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.s.Principal, i.ID, i.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.fake.SetWebhookAnswer("ops", 200, false)
	r.advance(5 * time.Second)
	r.dispatch(t)
	requests := r.fake.WebhookRequests()
	if len(requests) != 2 || requests[0].Body != requests[1].Body || !strings.Contains(requests[1].Body, `"incident_status":"open"`) {
		t.Fatalf("the retry did not send the copied body: %+v", requests)
	}
}

func TestFailingWebhookNoticeNeverBlocksTheResult(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn")
	ctx := context.Background()
	long := r.contactMethodType.AllowedPrivateURLPrefix + "ops?padding=" + strings.Repeat("x", 300)
	c, err := r.a.SaveContactMethod(ctx, r.s.Principal, ContactMethodInput{ID: r.method.ID, ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": long}, Shared: true, ExpectedRevision: r.method.Revision, Subscriptions: []string{"warn"}})
	if err != nil {
		t.Fatal(err)
	}
	finishSaveTest(t, r.a, r.s.Principal, r.contactMethodType)
	r.fake.SetWebhookAnswer("ops", 404, false)
	for n := range 3 {
		queueDelivery(t, r.a, r.s.Principal, fmt.Sprintf("long-%d", n), "warn")
		r.dispatch(t)
	}
	for n := range 3 {
		if d := findDelivery(t, r.a, r.s.Principal, fmt.Sprintf("long-%d", n)); d.State != "failed" || !d.NextAttempt.IsZero() {
			t.Fatalf("a refused request was not saved as failed: %+v", d)
		}
	}
	host := strings.TrimPrefix(strings.Split(r.contactMethodType.AllowedPrivateURLPrefix, "/webhook-receiver/")[0], "http://")
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM alerts WHERE title=?`, "MMHotline cannot deliver alerts to "+host); n != 1 {
		t.Fatalf("notice with the host only: %d", n)
	}
	// A notice that cannot be recorded never undoes the delivery's result.
	if err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE contact_methods SET failures_in_a_row=0,failing_since=0 WHERE id=?; CREATE TRIGGER no_notices BEFORE INSERT ON alerts BEGIN SELECT RAISE(ABORT,'injected notice failure'); END;`, c.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for n := range 3 {
			if err := QueueNotification(ctx, tx, Notification{Key: fmt.Sprintf("blocked-%d", n), Severity: "warn", Title: "Blocked notice", Purpose: "subscribed_alert", At: r.a.Now()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.dispatch(t)
	}
	for n := range 3 {
		if d := findDelivery(t, r.a, r.s.Principal, fmt.Sprintf("blocked-%d", n)); d.State != "failed" {
			t.Fatalf("result lost when the notice failed: %+v", d)
		}
	}
}

func TestFailureStreakFollowsTheCurrentURL(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn", "sos")
	ctx := context.Background()
	r.fake.SetWebhookAnswer("ops", 500, false)
	for n := range 2 {
		queueDelivery(t, r.a, r.s.Principal, fmt.Sprintf("old-%d", n), "warn")
		r.dispatch(t)
		r.advance(time.Second)
	}
	c, err := r.a.SaveContactMethod(ctx, r.s.Principal, ContactMethodInput{ID: r.method.ID, ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": r.contactMethodType.AllowedPrivateURLPrefix + "new"}, Shared: true, ExpectedRevision: r.method.Revision, Subscriptions: []string{"warn", "sos"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := scalarTeam(t, r.a, `SELECT failures_in_a_row FROM contact_methods WHERE id=?`, c.ID); n != 0 {
		t.Fatalf("new URL inherited %d failures", n)
	}
	r.fake.SetWebhookAnswer("new", 500, false)
	queueDelivery(t, r.a, r.s.Principal, "sos-kept", "sos")
	r.advance(time.Minute)
	if err = r.a.SendContactMethodTest(ctx, r.s.Principal, c.ID); err != nil {
		t.Fatal(err)
	}
	for n := range 3 {
		r.advance(time.Second)
		queueDelivery(t, r.a, r.s.Principal, fmt.Sprintf("new-%d", n), "warn")
	}
	for range 3 {
		r.dispatch(t)
	}
	if n := scalarTeam(t, r.a, `SELECT failures_in_a_row FROM contact_methods WHERE id=?`, c.ID); n != 3 {
		t.Fatalf("failures on the new URL: %d", n)
	}
	var cancelled []string
	if err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT notification_key FROM deliveries WHERE contact_method_id=? AND state='cancelled' AND reason LIKE 'Not sent: the contact method has been failing%' ORDER BY notification_key`, c.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				return err
			}
			cancelled = append(cancelled, key)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	// The SOS alert and the test were tried first and are never dropped; of the
	// ordinary alerts only the newest, new-2, is kept.
	if strings.Join(cancelled, ",") != "new-0,new-1" {
		t.Fatalf("cancelled %v", cancelled)
	}
}

func TestInterruptedSendRetriesOnlyWhereTheContactMethodTypeAllows(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "interrupted@example.test")
	m, err := a.CurrentMember(context.Background(), s.Principal)
	if err != nil {
		t.Fatal(err)
	}
	// This process has worked on the team before, so the reservation below
	// belongs to this process rather than the process before the restart.
	if _, err = dispatchWith(a, "", nil); err != nil {
		t.Fatal(err)
	}
	if err = a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted) VALUES('waking',?,'test_waking','{}','phone',1,1)`, m.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO deliveries(id,notification_key,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,attempts,first_attempt,reserved_until,created_at,next_attempt) VALUES('call','call','waking',1,?,'test_waking','phone','sos_escalation','sos','SOS','SOS','sending',1,?,?,?,0)`, m.ID, a.Now().Unix(), a.Now().Unix(), a.Now().Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	advance(time.Minute)
	if _, err = dispatchWith(a, "", nil); err != nil {
		t.Fatal(err)
	}
	if d := findDelivery(t, a, s.Principal, "call"); d.State != "uncertain" || !d.NextAttempt.IsZero() {
		t.Fatalf("a contact method type that must never resend was scheduled again: %+v", d)
	}
}
