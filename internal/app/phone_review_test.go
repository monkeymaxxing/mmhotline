package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// Regression tests for phone calls and failure fallback.

func (r *phoneRig) savedEmail(t *testing.T, p Principal, subscriptions ...string) ContactMethod {
	t.Helper()
	email, err := r.a.SaveContactMethod(context.Background(), p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "barbara@example.test"}, Subscriptions: subscriptions, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}

	sender := &recordingEmailSender{}
	if _, err := dispatchWith(r.a, r.a.WebURL, sender); err != nil {
		t.Fatal(err)
	}
	r.a.ContactMethodTypes = map[string]ContactMethodType{"automated_voice_call": &AutomatedVoiceCallContactMethodType{SendingService: r.sendingServices}}
	return email
}

func fallbacks(t *testing.T, r *phoneRig, contactMethodID string) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, d := range deliveriesOf(t, r.a, contactMethodID) {
		if strings.HasPrefix(d.NotificationKey, "sos_fallback:") {
			got[d.IncidentID]++
		}
	}
	return got
}

func TestAnsweredCallReleasesACallWaitingForThisOne(t *testing.T) {
	r := newPhoneRig(t)
	r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	first := r.lastCallTo(t, barbaraNumber)
	r.advance(10 * time.Second)
	r.sos(t, "Disk full on web-1")
	r.dispatch(t)
	if _, err := r.fake.AnswerCall(first.IDFromSendingService, fakesendingservices.CallAnswered); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.dispatch(t)
	if calls := r.callsTo(barbaraNumber); len(calls) != 2 || !strings.Contains(calls[1].Said[0], "Disk full on web-1") {
		t.Fatalf("the waiting call did not go when the first call ended: %d calls", len(calls))
	}
}

func TestFallbackForEveryIncidentOfAFailedCall(t *testing.T) {
	r := newPhoneRig(t)
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	email := r.savedEmail(t, p, "info", "warn", "sos")
	r.fake.SetNumberBehavior(barbaraNumber, fakesendingservices.NumberInvalid)
	a := r.sos(t, "Alpha down")
	b := r.sos(t, "Bravo down")
	r.dispatch(t)
	r.minuteLater(t, time.Minute)
	got := fallbacks(t, r, email.ID)
	if got[a.IncidentID] != 1 || got[b.IncidentID] != 1 {
		t.Fatalf("fallback for Alpha: %d, for Bravo: %d", got[a.IncidentID], got[b.IncidentID])
	}
	// A call that joined the failed call must not be labelled sent.
	for _, d := range deliveriesOf(t, r.a, call.ID) {
		if d.Purpose == "sos_escalation" && strings.HasPrefix(d.Reason, "Sent together") {
			t.Fatalf("a joined call says it was sent: %+v", d)
		}
	}
}

func TestFallbackAfterCallsFailOncePerEscalationStep(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	r.addPhone(t, p, barbaraNumber)
	email := r.savedEmail(t, p, "info", "warn", "sos")
	r.fake.SetNumberBehavior(barbaraNumber, fakesendingservices.NumberInvalid)
	e := r.sos(t, "Database down")
	r.dispatch(t)
	if fallbacks(t, r, email.ID)[e.IncidentID] != 1 {
		t.Fatal("every SOS call failed, and no fallback went out")
	}
	// Once per step: the repeats add nothing; the next step (a new target
	// window after escalation by hand) sends it again.
	r.minuteLater(t, time.Minute)
	if n := fallbacks(t, r, email.ID)[e.IncidentID]; n != 1 {
		t.Fatalf("fallbacks in one step: %d", n)
	}
	incident, _ := r.a.GetIncident(ctx, p, e.IncidentID)
	if _, err := r.a.EscalateIncident(ctx, p, incident.ID, memberID(t, r.a, p), incident.Revision); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Second)
	r.dispatch(t)
	if n := fallbacks(t, r, email.ID)[e.IncidentID]; n != 2 {
		t.Fatalf("fallbacks after a second step: %d", n)
	}
}

func TestNoFallbackWhileTheCallReachedThePhone(t *testing.T) {
	r := newPhoneRig(t)
	p := r.s.Principal
	r.addPhone(t, p, barbaraNumber)
	email := r.savedEmail(t, p, "info", "warn", "sos")
	r.sos(t, "Database down")
	r.dispatch(t)
	if len(r.callsTo(barbaraNumber)) != 1 {
		t.Fatal("the call was not placed")
	}
	if len(fallbacks(t, r, email.ID)) != 0 {
		t.Fatal("fallback after accepted call")
	}
}

func TestAClaimedIncidentIsNotListedInTheWaitingCall(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	r.addPhone(t, p, barbaraNumber)
	r.sos(t, "Alpha down")
	r.dispatch(t)
	first := r.lastCallTo(t, barbaraNumber)
	r.advance(10 * time.Second)
	r.sos(t, "Bravo down")
	c := r.sos(t, "Charlie down")
	r.dispatch(t)
	incident, _ := r.a.GetIncident(ctx, p, c.IncidentID)
	if _, err := r.a.ClaimIncident(ctx, p, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fake.AnswerCall(first.IDFromSendingService, fakesendingservices.CallNoAnswer); err != nil {
		t.Fatal(err)
	}
	r.advance(50 * time.Second)
	r.dispatch(t)
	if next := r.lastCallTo(t, barbaraNumber); strings.Contains(next.Said[0], "Charlie") || next.IDFromSendingService == first.IDFromSendingService {
		t.Fatalf("the call lists Charlie, claimed before the call: %q", next.Said[0])
	}
}

func TestOnlyAutomatedVoiceCallsReadTheTeamName(t *testing.T) {
	r := newPhoneRig(t)
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	email := r.savedEmail(t, p, "info", "warn", "sos")
	sender := &recordingEmailSender{}
	r.a.ContactMethodTypes["email"] = sender
	r.sos(t, "Database down")
	// A missing team name does not stop email; the call records a definite,
	// retryable failure instead of an unknown outcome.
	if _, err := r.a.Store.db.Exec(`DELETE FROM team_info`); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	if len(sender.messages()) == 0 {
		t.Fatal("the SOS email required the team name")
	}
	d := lastEscalation(t, deliveriesOf(t, r.a, call.ID))
	if d.State != "failed" || d.NextAttempt.IsZero() || !strings.Contains(d.Reason, "team's name") {
		t.Fatalf("call without the team name: %+v", d)
	}
	_ = email
}
