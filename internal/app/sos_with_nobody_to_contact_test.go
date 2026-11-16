package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"
)

// A correctly signed SOS is never refused because nobody can be contacted
// yet: on 2026-10-27 a fresh util01 with no members refused a real SOS with
// 400, and its sender dropped it. The incident stays open, says why nobody is
// contacted, and the first person added is contacted at the next visit.
func TestSOSIsKeptWhenTheTeamHasNoMembers(t *testing.T) {
	a := foundationApp(t)
	now := time.Date(2026, 10, 27, 17, 50, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	root := Principal{UnixUser: "root"}
	source, err := createAlertSource(context.Background(), a, root, "Monitoring tool on util01")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldLog) })

	sos := `{"sender_alert_id":"a98e90acf60941a35c9f83047c67f593","sender_incident_key":"5839438af46baef400c87df8015b3536","severity":"sos","title":"util01: public_sites/reference_app_answers_at_its_public_host_name failed","message":"expected HTTP 200, received 530\nfailing since 17:50 UTC"}`
	w := intakeHTTP(a, root, source, sos, now)
	if w.Code != 200 {
		t.Fatalf("SOS with no members: HTTP %d %s", w.Code, w.Body.String())
	}
	var receipt struct {
		IncidentID string `json:"mmhotline_incident_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.IncidentID == "" {
		t.Fatalf("receipt: %s", w.Body.String())
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='sos_escalation_due' AND message LIKE 'Nobody to contact yet%'`, receipt.IncidentID); n != 1 {
		t.Fatalf("history lines saying nobody is contacted: %d", n)
	}
	// Later visits repeat nothing in the history, and never fail.
	for range 3 {
		now = now.Add(time.Minute)
		if err := a.ReconcileIncidents(context.Background()); err != nil {
			t.Fatalf("reconcile with nobody to contact: %v", err)
		}
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='sos_escalation_due'`, receipt.IncidentID); n != 1 {
		t.Fatalf("history lines after repeats: %d", n)
	}
	// The first person added is contacted at the next visit.
	memberFixture(t, a, "first@example.test")
	giveWakingContactMethod(t, a, "first@example.test")
	now = now.Add(time.Minute)
	if err := a.ReconcileIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='sos_escalation_due' AND message LIKE 'Contacting %'`, receipt.IncidentID); n != 1 {
		t.Fatalf("the first person added was not contacted: %d", n)
	}
	if strings.Contains(logs.String(), "refused") {
		t.Fatalf("an accepted SOS logged a refusal: %s", logs.String())
	}
}

// Every refused alert leaves one journal line with the sender's alert ID and
// the reason, and a correctly signed SOS refused as invalid opens MMHotline's
// own SOS, even when the field that broke a rule comes before severity.
func TestRefusedSOSIsLoggedAndOpensMMHotlinesOwnSOS(t *testing.T) {
	a, p, source := alertFixture(t)
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldLog) })
	body := `{"sender_alert_id":"refused-1","unknown_field":"x","severity":"sos","title":"t","message":"m"}`
	if w := intakeHTTP(a, p, source, body, a.Now()); w.Code != 400 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(logs.String(), `refused-1`) || !strings.Contains(logs.String(), `unknown or duplicate field`) {
		t.Fatalf("journal line: %q", logs.String())
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM incidents WHERE source_id='system' AND sender_incident_key=?`, "refusing-sos:"+source.Source.ID); n != 1 {
		t.Fatalf("MMHotline's own SOS about the refusal: %d", n)
	}
	// A field of the wrong type before severity, refused as invalid and with
	// the sender's clock off, found by FuzzAlertSourceRequestBody: each
	// refusal joins MMHotline's own SOS.
	refusals := `SELECT count(*) FROM alerts WHERE source_id='system' AND title LIKE 'MMHotline is refusing SOS alerts from%'`
	before := scalarTeam(t, a, refusals)
	wrongType := `{"sender_alert_id":5,"severity":"sos","title":"t","message":"m"}`
	if w := intakeHTTP(a, p, source, wrongType, a.Now()); w.Code != 400 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	if w := intakeHTTP(a, p, source, wrongType, a.Now().Add(-10*time.Minute)); w.Code != 401 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	if after := scalarTeam(t, a, refusals); after != before+2 {
		t.Fatalf("refusals of an SOS with a field of the wrong type joined MMHotline's own SOS %d times, not 2", after-before)
	}
}
