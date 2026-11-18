package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

func (r *phoneRig) lastCallTo(t *testing.T, number string) fakesendingservices.PhoneCall {
	t.Helper()
	calls := r.callsTo(number)
	if len(calls) == 0 {
		t.Fatalf("no call to %s", number)
	}
	return calls[len(calls)-1]
}

const oneIncidentScript = "Hello. This is Hotline. An SOS incident needs someone now. Team: Fixture team. Database down. Open Hotline and claim it. Claiming makes you responsible and stops the calls."

func TestSOSCallSaysTheMessageTwiceAndEnds(t *testing.T) {
	for _, pushover := range []bool{false, true} {
		r := newPhoneRig(t)
		r.addPhone(t, r.s.Principal, barbaraNumber)
		if pushover {
			if _, err := r.a.SetUpSendingService(context.Background(), r.s.Principal, "pushover", map[string]string{"pushover_application_token": fakesendingservices.PushoverApplicationToken}); err != nil {
				t.Fatal(err)
			}
		}
		event := r.sos(t, "Database down")
		r.dispatch(t)
		call := r.lastCallTo(t, barbaraNumber)
		want := oneIncidentScript
		if pushover {
			want = strings.Replace(want, "Open Hotline and claim it.", "Open Hotline and claim it, or acknowledge the alert in Pushover, which also claims it.", 1)
		}
		if len(call.Said) != 2 || call.Said[0] != want || call.Said[1] != want || strings.Contains(call.Markup, "Gather") || !strings.HasSuffix(call.Markup, "<Hangup/></Response>") {
			t.Fatalf("call: %+v", call)
		}
		if call.RingSeconds != 30 || call.TimeLimitSeconds != 120 || call.MachineDetection != "" || call.From != fakesendingservices.TwilioPhoneNumberToCallFrom {
			t.Fatalf("settings: %+v", call)
		}
		if _, err := r.fake.AnswerCall(call.IDFromSendingService, fakesendingservices.CallAnswered); err != nil {
			t.Fatal(err)
		}
		r.poll(t, 5*time.Second)
		incident, _ := r.a.GetIncident(context.Background(), r.s.Principal, event.IncidentID)
		if incident.Status != "open" {
			t.Fatal("answering claimed incident")
		}
	}
}

func TestOneCallAboutEveryWaitingIncident(t *testing.T) {
	r := newPhoneRig(t)
	r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	first := r.lastCallTo(t, barbaraNumber)
	r.sos(t, "Disk full on web-1")
	r.dispatch(t)
	if len(r.callsTo(barbaraNumber)) != 1 {
		t.Fatal("calls overlapped")
	}
	if _, err := r.fake.AnswerCall(first.IDFromSendingService, fakesendingservices.CallAnswered); err != nil {
		t.Fatal(err)
	}
	r.minuteLater(t, time.Minute)
	r.dispatch(t)
	second := r.lastCallTo(t, barbaraNumber)
	want := "Hello. This is Hotline. 2 SOS incidents need someone now. Team: Fixture team. First: Disk full on web-1. Second: Database down. Open Hotline and claim them. Claiming makes you responsible and stops the calls."
	if len(r.callsTo(barbaraNumber)) != 2 || second.Said[0] != want {
		t.Fatalf("second call: %+v", second)
	}
}

func TestACallWithoutAFinalStatusBlocksTheNumberForThreeMinutes(t *testing.T) {
	r := newPhoneRig(t)
	r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	for range 2 {
		r.minuteLater(t, time.Minute)
	}
	if n := len(r.callsTo(barbaraNumber)); n != 1 {
		t.Fatalf("calls within 3 minutes of an active call: %d", n)
	}
	r.minuteLater(t, time.Minute)
	if n := len(r.callsTo(barbaraNumber)); n != 2 {
		t.Fatalf("calls after 3 minutes: %d", n)
	}
}

func TestSOSCallsNeverRetryAfterAnUnknownOutcome(t *testing.T) {
	r := newPhoneRig(t)
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	r.fake.SetNextSendingServiceAction("twilio", fakesendingservices.LoseNextResponse)
	// The lost response has no call ID, so it does not hold up the next
	// repeat; the repeat is the retry.
	r.sos(t, "Database down")
	r.dispatch(t)
	d := lastEscalation(t, deliveriesOf(t, r.a, call.ID))
	if d.State != "uncertain" || !d.NextAttempt.IsZero() {
		t.Fatalf("after a lost response: %+v", d)
	}
	r.minuteLater(t, time.Minute)
	if n := len(r.callsTo(barbaraNumber)); n != 2 {
		t.Fatalf("calls: %d", n)
	}
}

// twilioJustTookACall uses Twilio's current request slot outside MMHotline pacing.
func (r *phoneRig) twilioJustTookACall(t *testing.T) {
	t.Helper()
	if answer := r.sendingServices.createCall(context.Background(), "+15555550199", callMarkup("A call to someone else")); answer.State != "accepted" {
		t.Fatalf("Twilio's occupied slot: %+v", answer)
	}
}

func TestACallThatTwilioRefusesWithHTTP429IsMadeAgainASecondLater(t *testing.T) {
	r := newPhoneRig(t)
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	r.twilioJustTookACall(t)
	r.sos(t, "Database down")
	r.dispatch(t)
	d := lastEscalation(t, deliveriesOf(t, r.a, call.ID))
	if d.State != "failed" || d.Attempts != 1 || !d.NextAttempt.Equal(r.a.Now().Add(time.Second)) || refusedForRate(t, r.a) != 1 {
		t.Fatalf("the call that Twilio refused with HTTP 429: %+v", d)
	}
	if calls := r.callsTo(barbaraNumber); len(calls) != 0 {
		t.Fatalf("calls that Twilio refused: %+v", calls)
	}
	// Not at the next repeat, a minute later.
	r.advance(time.Second)
	r.dispatch(t)
	calls := r.callsTo(barbaraNumber)
	if len(calls) != 1 {
		t.Fatalf("calls a second after Twilio refused: %+v", calls)
	}
	if d := lastEscalation(t, deliveriesOf(t, r.a, call.ID)); d.State != "accepted" || d.Attempts != 2 {
		t.Fatalf("the call after it was made again: %+v", d)
	}
}

func TestACallThatTwilioKeepsRefusingWithHTTP429IsTriedEightTimes(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	email, err := r.a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "barbara@example.test"}, Subscriptions: []string{"info", "warn", "sos"}, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}

	fallbacks := func() int {
		count := 0
		for _, d := range deliveriesOf(t, r.a, email.ID) {
			if strings.HasPrefix(d.NotificationKey, "sos_fallback:") {
				count++
			}
		}
		return count
	}
	// Twilio takes one call an hour.
	r.fake.SetSendingServiceRateLimit("twilio", "call", 1.0/3600)
	r.twilioJustTookACall(t)
	start := r.a.Now()
	r.sos(t, "Database down")
	r.dispatch(t)
	d := lastEscalation(t, deliveriesOf(t, r.a, call.ID))
	for !d.NextAttempt.IsZero() && r.a.Now().Sub(start) < time.Minute {
		// While the call will be tried again, it has not failed for good:
		// Barbara's other contact methods do not get the SOS yet.
		if n := fallbacks(); n != 0 {
			t.Fatalf("after %d attempts the email contact method got the SOS %d times", d.Attempts, n)
		}
		r.advance(time.Second)
		r.dispatch(t)
		d = lastEscalation(t, deliveriesOf(t, r.a, call.ID))
	}
	if d.State != "failed" || d.Attempts != maxDeliveryAttempts || refusedForRate(t, r.a) != maxDeliveryAttempts {
		t.Fatalf("the call that Twilio kept refusing: %+v", d)
	}
	if took := r.a.Now().Sub(start); took > 10*time.Second {
		t.Fatalf("the %d attempts took %s", d.Attempts, took)
	}
	if calls := r.callsTo(barbaraNumber); len(calls) != 0 {
		t.Fatalf("calls that Twilio refused: %+v", calls)
	}
	if n := fallbacks(); n != 1 {
		t.Fatalf("after the last attempt the email contact method got the SOS %d times", n)
	}
}

// Within one process, a call reserved with no result after 30
// seconds may have been made: only its result could not be saved. It is not
// made again; the next repeat is the retry. After mmhotline serve starts,
// it is made again (TestAfterARestartTheCallThatWasReservedIsMade).
func TestAReservedCallWithoutAResultWaitsForTheNextRepeat(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	r.sos(t, "Database down")
	if d, _, err := r.a.reserveDelivery(ctx, "https://mmhotline.example.com", []string{"automated_voice_call"}); err != nil || d.ID == "" {
		t.Fatal(err)
	}
	r.advance(31 * time.Second)
	r.dispatch(t)
	if d := lastEscalation(t, deliveriesOf(t, r.a, call.ID)); d.State != "uncertain" || !d.NextAttempt.IsZero() || d.Reason != reservationExpiredReason {
		t.Fatalf("the reserved call 31 seconds later: %+v", d)
	}
	if n := len(r.callsTo(barbaraNumber)); n != 0 {
		t.Fatalf("calls before the next repeat: %d", n)
	}
	r.minuteLater(t, 29*time.Second)
	if n := len(r.callsTo(barbaraNumber)); n != 1 {
		t.Fatalf("calls after the next repeat: %d", n)
	}
}

func TestVoicemailPacingAfterThirtyMinutes(t *testing.T) {
	r := newPhoneRig(t)
	r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	answer := func(outcome string) {
		t.Helper()
		if _, err := r.fake.AnswerCall(r.lastCallTo(t, barbaraNumber).IDFromSendingService, outcome); err != nil {
			t.Fatal(err)
		}
	}
	// Every minute during the first 30 minutes (calls at minutes 0 to 29),
	// even when voicemail answers.
	for range 29 {
		answer(fakesendingservices.CallVoicemail)
		r.minuteLater(t, time.Minute)
	}
	if n := len(r.callsTo(barbaraNumber)); n != 30 {
		t.Fatalf("calls in the first 30 minutes: %d", n)
	}
	// Then an answered call slows the next to 5 minutes after it.
	answer(fakesendingservices.CallVoicemail)
	for range 4 {
		r.minuteLater(t, time.Minute)
	}
	if n := len(r.callsTo(barbaraNumber)); n != 30 {
		t.Fatalf("calls 4 minutes after voicemail: %d", n)
	}
	r.minuteLater(t, time.Minute)
	if n := len(r.callsTo(barbaraNumber)); n != 31 {
		t.Fatalf("calls 5 minutes after voicemail: %d", n)
	}
	// A call that rings unanswered costs nothing: every minute again.
	answer(fakesendingservices.CallNoAnswer)
	r.minuteLater(t, time.Minute)
	if n := len(r.callsTo(barbaraNumber)); n != 32 {
		t.Fatalf("calls a minute after no answer: %d", n)
	}
	// A call with no final status counts as answered.
	for range 5 {
		r.minuteLater(t, time.Minute)
	}
	if n := len(r.callsTo(barbaraNumber)); n != 33 {
		t.Fatalf("calls 5 minutes after a call without a final status: %d", n)
	}
}

func TestCallLabels(t *testing.T) {
	r := newPhoneRig(t)
	call := r.addPhone(t, r.s.Principal, barbaraNumber)
	labels := map[string]string{
		fakesendingservices.CallAnswered: "Answered (by a person or voicemail)",
		fakesendingservices.CallNoAnswer: "No answer",
		fakesendingservices.CallBusy:     "Busy",
		fakesendingservices.CallFailed:   "Call failed",
	}
	for outcome, want := range labels {
		r.sos(t, "Database down "+outcome)
		r.dispatch(t)
		if _, err := r.fake.AnswerCall(r.lastCallTo(t, barbaraNumber).IDFromSendingService, outcome); err != nil {
			t.Fatal(err)
		}
		r.poll(t, 5*time.Second)
		d := lastEscalation(t, deliveriesOf(t, r.a, call.ID))
		if d.OutcomeLabel() != want {
			t.Fatalf("%s: %q, want %q", outcome, d.OutcomeLabel(), want)
		}
		incident, _ := r.a.GetIncident(context.Background(), r.s.Principal, d.IncidentID)
		if _, err := r.a.ResolveIncident(context.Background(), r.s.Principal, incident.ID, incident.Revision, ""); err != nil {
			t.Fatal(err)
		}
		r.advance(time.Minute)
	}
}

func TestTestCallNeverClaims(t *testing.T) {
	r := newPhoneRig(t)
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	if err := r.a.SendContactMethodTest(context.Background(), p, call.ID); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	c := r.lastCallTo(t, barbaraNumber)
	if !strings.Contains(c.Said[0], testCallMessage) {
		t.Fatalf("test call: %q", c.Said)
	}
	if _, err := r.fake.AnswerCall(c.IDFromSendingService, fakesendingservices.CallAnswered); err != nil {
		t.Fatal(err)
	}
	r.poll(t, 5*time.Second)
	ds := deliveriesOf(t, r.a, call.ID)
	if d := ds[len(ds)-1]; d.Purpose != "contact_method_test" || d.OutcomeLabel() != "Answered (by a person or voicemail)" {
		t.Fatalf("test: %+v", d)
	}

}

func TestWhenAutomatedVoiceCallFailsOtherContactMethodsGetTheSOS(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	r.addPhone(t, p, barbaraNumber)
	email, err := r.a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "barbara@example.test"}, Subscriptions: []string{"info", "warn", "sos"}, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}

	r.fake.SetNumberBehavior(barbaraNumber, fakesendingservices.NumberInvalid)
	e := r.sos(t, "Database down")
	r.dispatch(t)
	var fallback []Delivery
	for _, d := range deliveriesOf(t, r.a, email.ID) {
		if strings.HasPrefix(d.NotificationKey, "sos_fallback:") {
			fallback = append(fallback, d)
		}
	}
	if len(fallback) != 1 || fallback[0].IncidentID != e.IncidentID || fallback[0].NotesFromMmhotline != sosFallbackNote {
		t.Fatalf("fallback SOS: %+v", fallback)
	}
	// Once per escalation step: repeats in the step add nothing.
	r.minuteLater(t, time.Minute)
	count := 0
	for _, d := range deliveriesOf(t, r.a, email.ID) {
		if strings.HasPrefix(d.NotificationKey, "sos_fallback:") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("fallback SOS per step: %d", count)
	}
}

func TestClaimingTheIncidentAWaitingCallWasForKeepsTheOthersWaiting(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	r.addPhone(t, p, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	first := r.lastCallTo(t, barbaraNumber)
	b := r.sos(t, "Disk full on web-1")
	r.sos(t, "Queue backed up")
	r.dispatch(t)
	// Claiming B must leave C waiting for a call.
	incident, _ := r.a.GetIncident(ctx, p, b.IncidentID)
	if _, err := r.a.ClaimIncident(ctx, p, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fake.AnswerCall(first.IDFromSendingService, fakesendingservices.CallNoAnswer); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	r.dispatch(t)
	next := r.lastCallTo(t, barbaraNumber)
	if next.IDFromSendingService == first.IDFromSendingService || !strings.Contains(next.Said[0], "Queue backed up") || strings.Contains(next.Said[0], "Disk full") {
		t.Fatalf("call after B was claimed: %q", next.Said)
	}

}

// A voice mangles "MMWatchdog" and "MMHotline", so every call says
// "Watchdog" and "Hotline", in built-in sentences and in an alert's own text
// alike. Written text keeps the full names.
func TestCallsSpeakProductNamesWithoutTheLeadingMM(t *testing.T) {
	markup := callMarkup("This is MMHotline. MMWatchdog: disk full on web01 (mmwatchdog check). The MMHotline-down alarm. MMX run failed. HAMMHotline stays.")
	want := "This is Hotline. Watchdog: disk full on web01 (Watchdog check). The Hotline-down alarm. MMX run failed. HAMMHotline stays."
	if !strings.Contains(markup, "<Say>"+want+"</Say>") {
		t.Fatalf("the spoken text is not %q:\n%s", want, markup)
	}
	if !strings.Contains(sosCallMessage("Ops", []string{"MMWatchdog: web01 is down"}, false), "MMWatchdog: web01 is down") {
		t.Fatal("the message text itself changed; only the spoken markup drops the MM")
	}
}
