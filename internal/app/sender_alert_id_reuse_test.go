package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// A sender's retry of the same alert is received once. A different alert
// that reuses its sender_alert_id is refused, never answered as received.
func TestReusedSenderAlertIDForADifferentAlertIsRefused(t *testing.T) {
	a, p, s := alertFixture(t)
	now := a.Now()
	first := `{"sender_alert_id":"disk_full","severity":"warn","title":"Disk full","message":"/var is 95% full","happened_at":"2026-09-29T11:59:00Z"}`
	id := alertReceipt(t, intakeHTTP(a, p, s, first, now))
	// A retry may set happened_at again; it is still the same alert.
	retry := `{"sender_alert_id":"disk_full","severity":"warn","title":"Disk full","message":"/var is 95% full","happened_at":"2026-09-29T11:59:30Z"}`
	w := intakeHTTP(a, p, s, retry, now)
	if alertReceipt(t, w) != id || !strings.Contains(w.Body.String(), `"already_received":true`) {
		t.Fatalf("retry: %s", w.Body.String())
	}
	for _, different := range []string{
		`{"sender_alert_id":"disk_full","severity":"warn","title":"Disk full","message":"/srv is 97% full"}`,
		`{"sender_alert_id":"disk_full","severity":"info","title":"Disk full","message":"/var is 95% full"}`,
		`{"sender_alert_id":"disk_full","severity":"warn","title":"Disk nearly full","message":"/var is 95% full"}`,
		`{"sender_alert_id":"disk_full","severity":"warn","sender_incident_key":"disk","title":"Disk full","message":"/var is 95% full"}`,
	} {
		w := intakeHTTP(a, p, s, different, now)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "sender_alert_id already used for a different alert") {
			t.Errorf("reuse %s: %d %s", different, w.Code, w.Body.String())
		}
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts`); n != 1 {
		t.Fatalf("alerts: %d", n)
	}
	// An alert received without severity was stored as warn; its retry
	// without severity is the same alert.
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"no_severity","title":"Queue slow","message":""}`, now))
	if w := intakeHTTP(a, p, s, `{"sender_alert_id":"no_severity","title":"Queue slow","message":""}`, now); !strings.Contains(w.Body.String(), `"already_received":true`) {
		t.Fatalf("retry without severity: %d %s", w.Code, w.Body.String())
	}
}

// A refused SOS never vanishes: reusing a sender_alert_id for a new SOS opens
// MMHotline's own SOS about the refused SOS.
func TestReusedSenderAlertIDOnAnSOSOpensMmhotlineSOS(t *testing.T) {
	a, p, s := alertFixture(t)
	now := a.Now()
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"1","severity":"warn","title":"Backup slow","message":""}`, now))
	w := intakeHTTP(a, p, s, `{"sender_alert_id":"1","severity":"sos","title":"Backup failed","message":"no backup for 2 days"}`, now)
	if w.Code != 400 {
		t.Fatalf("reused id on an SOS: %d %s", w.Code, w.Body.String())
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.source_id='system' AND i.status='open' AND e.title LIKE 'MMHotline is refusing SOS alerts from “Backup”: %sender_alert_id already used for a different alert%'`); n != 1 {
		t.Fatalf("MMHotline SOS incidents about the refused SOS: %d", n)
	}
}

// An SOS that joins a claimed incident goes to the person who claimed it,
// once, through their waking contact methods; the history says so.
func TestSOSJoiningAClaimedIncidentWakesTheClaimer(t *testing.T) {
	r := newPushoverRig(t)
	ctx := context.Background()
	push := r.add(t, fakesendingservices.PushoverUserKey)
	source, err := createAlertSource(ctx, r.a, r.principal, "Backup")
	if err != nil {
		t.Fatal(err)
	}
	firstID := alertReceipt(t, intakeHTTP(r.a, r.principal, source, `{"sender_alert_id":"one","sender_incident_key":"disk","severity":"sos","title":"Disk full","message":""}`, r.a.Now()))
	first, err := r.a.GetAlert(ctx, r.principal, firstID)
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	i, err := r.a.GetIncident(ctx, r.principal, first.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if i, err = r.a.ClaimIncident(ctx, r.principal, i.ID, i.Revision, ""); err != nil || i.Status != "claimed" {
		t.Fatalf("claim: %+v %v", i, err)
	}
	r.dispatch(t)
	before := len(r.fake.PushoverMessages())
	again := `{"sender_alert_id":"two","sender_incident_key":"disk","severity":"sos","title":"Disk full again","message":"/var is full"}`
	secondID := alertReceipt(t, intakeHTTP(r.a, r.principal, source, again, r.a.Now()))
	second, err := r.a.GetAlert(ctx, r.principal, secondID)
	if err != nil || second.IncidentID != i.ID {
		t.Fatalf("joined: %+v %v", second, err)
	}
	// The sender's retry of the same SOS does not notify again.
	alertReceipt(t, intakeHTTP(r.a, r.principal, source, again, r.a.Now()))
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM deliveries WHERE alert_id=? AND contact_method_id=? AND state!='suppressed'`, secondID, push.ID); n != 1 {
		t.Fatalf("deliveries to the claimer about the joining SOS: %d", n)
	}
	r.dispatch(t)
	sent := r.fake.PushoverMessages()[before:]
	if len(sent) != 1 || !strings.Contains(sent[0].Title, "Disk full again") || sent[0].Priority != 2 {
		t.Fatalf("Pushover messages after the joining SOS: %+v", sent)
	}
	// The incident is already claimed, so the message does not ask to claim it.
	var message string
	if err := r.a.Store.db.QueryRowContext(ctx, `SELECT message FROM deliveries WHERE notification_key LIKE 'related_alert_sent_to_claimer:%' AND state!='suppressed'`).Scan(&message); err != nil || !strings.Contains(message, "Someone has claimed this SOS incident.") || strings.Contains(message, "Open the incident to claim it") {
		t.Fatalf("message to the claimer %q, %v", message, err)
	}
	audit, err := r.a.IncidentAudit(ctx, r.principal, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := audit[len(audit)-1]; last.Kind != "related_alert_sent_to_claimer" || last.RelatedAlertID != secondID {
		t.Fatalf("history: %+v", audit)
	}
	if still, _ := r.a.GetIncident(ctx, r.principal, i.ID); still.Status != "claimed" {
		t.Fatalf("incident: %+v", still)
	}
	// The emergency keeps ringing until the claimer acknowledges it; the
	// claim before it does not cancel it. Resolving the incident does.
	receiptOf := func() fakesendingservices.PushoverReceipt {
		t.Helper()
		for _, receipt := range r.fake.PushoverReceipts() {
			if receipt.Receipt == sent[0].Receipt {
				return receipt
			}
		}
		t.Fatalf("no receipt %s", sent[0].Receipt)
		return fakesendingservices.PushoverReceipt{}
	}
	for range 3 {
		r.advance(5 * time.Second)
		r.poll(t)
	}
	if receipt := receiptOf(); receipt.Cancelled || len(receipt.Polls) == 0 {
		t.Fatalf("the claimer's emergency: %+v", receipt)
	}
	still, err := r.a.GetIncident(ctx, r.principal, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ResolveIncident(ctx, r.principal, i.ID, still.Revision, ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r.poll(t)
	}
	if receipt := receiptOf(); !receipt.Cancelled {
		t.Fatalf("the claimer's emergency after the resolution: %+v", receipt)
	}
}
