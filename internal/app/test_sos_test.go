package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// sendTestSOS runs `incidents send-test-sos` and returns the test incident's
// ID; the command returns at once and the client waits for the claim.
func (r *triageHoldRig) sendTestSOS(t *testing.T) string {
	t.Helper()
	result, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "send-test-sos"}, 200)
	if result.TestSOS == nil || result.TestSOS.IncidentID == "" || !strings.HasSuffix(result.TestSOS.IncidentPageURL, "/incidents/"+result.TestSOS.IncidentID) {
		t.Fatalf("send-test-sos answered %+v", result.TestSOS)
	}
	return result.TestSOS.IncidentID
}

func (r *triageHoldRig) status(t *testing.T, id, want string) {
	t.Helper()
	if i, err := r.a.GetIncident(context.Background(), r.s.Principal, id); err != nil || i.Status != want {
		t.Fatalf("incident %s: %+v, %v; want %s", id, i, err, want)
	}
}

// The test SOS wakes the person on call as a real SOS does, with the same
// contact methods, repeats, and backup timing, but at once: the triage agent
// has nothing to look into, so it neither waits nor runs. A real SOS from an
// alert source waits 5 minutes for the triage agent in this rig.
func TestTestSOSWakesThePersonOnCallAtOnceAndSkipsTheTriageAgent(t *testing.T) {
	r := newTriageHoldRig(t)
	id := r.sendTestSOS(t)
	backgroundRounds(t, r.a)
	incident, err := r.a.GetIncident(context.Background(), r.s.Principal, id)
	if err != nil || incident.Status != "open" || incident.Step == triageAgentStep || incident.Title != "Test of the alerts. Nothing is wrong: acknowledge this." {
		t.Fatalf("the test incident: %+v, %v", incident, err)
	}
	if r.emergencies(id, fakesendingservices.PushoverUserKey) != 1 || len(r.callsTo(onCallNumber)) != 1 {
		t.Fatalf("the person on call was not woken at once: %d emergencies, %d calls", r.emergencies(id, fakesendingservices.PushoverUserKey), len(r.callsTo(onCallNumber)))
	}
	select {
	case run := <-r.runs:
		t.Fatalf("the triage agent ran for a test: %+v", run)
	default:
	}
	// Nobody claims: the backup is woken when the time to claim ends, as for
	// a real SOS.
	if r.emergencies(id, fakesendingservices.PushoverOtherUserKey) != 0 {
		t.Fatal("the backup was woken at once")
	}
	r.advance(5*time.Minute + time.Second)
	backgroundRounds(t, r.a)
	if r.emergencies(id, fakesendingservices.PushoverOtherUserKey) != 1 {
		t.Fatalf("the backup was not woken after the time to claim: %v", r.history(t, id))
	}
}

// Whoever claims the test closes it: acknowledging in Pushover, claiming on
// the incident page, or any other claim. MMHotline resolves the test at once
// and sends nobody a message that it is resolved. A real SOS stays claimed.
func TestClaimingTheTestSOSResolvesIt(t *testing.T) {
	r := newTriageHoldRig(t)
	id := r.sendTestSOS(t)
	backgroundRounds(t, r.a)
	var receipt string
	for _, m := range r.fake.PushoverMessages() {
		if m.Priority == 2 && m.UserKey == fakesendingservices.PushoverUserKey && strings.HasSuffix(m.URL, "/incidents/"+id) {
			receipt = m.Receipt
		}
	}
	if err := r.fake.PushoverAcknowledge(receipt); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	backgroundRounds(t, r.a)
	incident, err := r.a.GetIncident(context.Background(), r.s.Principal, id)
	if err != nil || incident.Status != "resolved" || incident.OwnerMemberID == "" {
		t.Fatalf("after the Pushover acknowledgement: %+v, %v", incident, err)
	}
	if !inOrder(r.history(t, id), "Claimed by Responder, who acknowledged the alert in Pushover", "Resolved by MMHotline after the claim, because this incident was a test") {
		t.Fatalf("history: %q", r.history(t, id))
	}
	for _, line := range r.pushoverAbout(id, fakesendingservices.PushoverUserKey) {
		if strings.Contains(line, "esolved") && !strings.Contains(line, "This incident is already resolved") && !strings.Contains(line, "closed the test") {
			t.Errorf("Pushover was told about the end of the test: %s", line)
		}
	}
	// What the command prints: who, how, and when.
	shown, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "show", Incidents: IncidentsCommandRequest{IncidentID: id}}, 200)
	test := CommandTestSOS{IncidentID: id, IncidentPageURL: "page"}
	if !test.Update(*shown.Incident) || test.ClaimedBy != "Responder" || test.ClaimedHow != "by acknowledging the alert in Pushover" || test.ClaimedAt.IsZero() || test.ResolvedAt.IsZero() {
		t.Fatalf("the claim as the command reads it: %+v", test)
	}
	if text := test.Text(); !strings.Contains(text, "Responder claimed the test by acknowledging the alert in Pushover, at ") || !strings.Contains(text, "MMHotline resolved the test after the claim, at ") {
		t.Fatalf("text:\n%s", text)
	}
	// A claim on the incident page closes it as well.
	second := r.sendTestSOS(t)
	opened, err := r.a.GetIncident(context.Background(), r.s.Principal, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.s.Principal, second, opened.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.status(t, second, "resolved")
	// A real SOS stays claimed.
	realSOS := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	realOpened, err := r.a.GetIncident(context.Background(), r.s.Principal, realSOS.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.s.Principal, realSOS.IncidentID, realOpened.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.status(t, realSOS.IncidentID, "claimed")
}

// A test nobody claimed stays open, and the next test closes it, so a second
// test never leaves two tests ringing a phone.
func TestANewTestSOSClosesTheUnclaimedEarlierTest(t *testing.T) {
	r := newTriageHoldRig(t)
	first := r.sendTestSOS(t)
	second := r.sendTestSOS(t)
	r.status(t, first, "resolved")
	r.status(t, second, "open")
	if !inOrder(r.history(t, first), "Resolved by MMHotline because a newer test replaced it") {
		t.Fatalf("history: %q", r.history(t, first))
	}
}
