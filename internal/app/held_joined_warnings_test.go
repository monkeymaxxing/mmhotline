package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// A warn that joins a triage session while one of the session's SOS incidents
// waits for the agent is held too, for the settings file's
// triage_agent.warn_waits_for_the_agent_minutes, here 3. These tests walk it
// through MMHotline's own paths, as the held warn tests do.

const (
	hubWarningApp021 = "app021 stopped reporting to the fleet hub"
	hubWarningApp022 = "app022 stopped reporting to the fleet hub"
	caddyWarningLB02 = "Caddy check failed on lb02"
	caddyWarningLB03 = "Caddy check failed on lb03"
)

// warnPushovers counts the Pushover messages that carry the alert as a
// warning, to both people. A resolved message about it is not one.
func (r *heldWarnRig) warnPushovers(title string) int {
	count := 0
	for _, m := range r.fake.PushoverMessages() {
		if m.Title == "[Warning] "+title {
			count++
		}
	}
	return count
}

// joins admits a warn from the alert source, d after the SOS arrived, and
// lets the background loop read it into the session.
func (r *heldWarnRig) joins(t *testing.T, d time.Duration, title, key string) Alert {
	t.Helper()
	r.at(t, d)
	alert := admitSignedAlert(t, r.a, r.source, "warn", title, key)
	backgroundRounds(t, r.a)
	return alert
}

// resolves lets the alert source say that the warn's problem resolved.
func (r *heldWarnRig) resolves(t *testing.T, d time.Duration, title, key string) {
	t.Helper()
	r.at(t, d)
	admitSignedAlert(t, r.a, r.source, "resolved-warn", title, key)
	backgroundRounds(t, r.a)
}

// setWaits applies the SOS's wait, the server replacement's wait, and the
// warn's wait, in minutes.
func (r *heldWarnRig) setWaits(t *testing.T, sos, replacement, warn int) {
	t.Helper()
	outcomes, err := r.a.applySettingsParts(context.Background(), Settings{TriageAgent: &SettingsTriageAgent{SOSWaitsForTheAgentMinutes: sos, SOSWaitsForAServerReplacementMinutes: replacement, WarnWaitsForTheAgentMinutes: warn}}, nil, []settingsFilePart{settingsSOSWaitsForTheAgentPart, settingsSOSWaitsForAServerReplacementPart, settingsWarnWaitsForTheAgentPart})
	if err != nil || outcomes[0].State != "applied" || outcomes[1].State != "applied" || outcomes[2].State != "applied" {
		t.Fatalf("applying the waits: %+v %v", outcomes, err)
	}
}

// resumedRun ends the run, as an agent that finished, and returns the run
// that resumes the session with the alerts that waited.
func (r *heldWarnRig) resumedRun(t *testing.T, run fakeTriageRun) fakeTriageRun {
	t.Helper()
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Done."})
	waitNoTriageRun(t, r.a)
	return r.runStarted(t)
}

// The real sequence from a phone test: an SOS starts the session, the agent
// starts a server replacement, and four warnings from the same outage join.
// Two resolve on their own, the SOS resolves, and the agent's report covers
// the other two. The person hears one warning, the replacement's own.
func TestJoinedWarnsOfAnOutageTheAgentFixesAreNeverSent(t *testing.T) {
	parallelStartingProcesses(t)
	r := &heldWarnRig{replacementRig(t, false)}
	r.setWaits(t, 3, 7, 3)
	sos, run, answers, ask := replacementSession(t, r.triageHoldRig)
	r.at(t, time.Minute)
	if answer := ask(replaceFailedServers); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("replace_failed_servers: %+v", answer)
	}
	first := r.joins(t, 70*time.Second, hubWarningApp021, "app021-hub")
	r.joins(t, 2*time.Minute, hubWarningApp022, "app022-hub")
	third := r.joins(t, 150*time.Second, caddyWarningLB02, "lb02-caddy")
	fourth := r.joins(t, 210*time.Second, caddyWarningLB03, "lb03-caddy")
	for _, title := range []string{hubWarningApp021, hubWarningApp022, caddyWarningLB02, caddyWarningLB03} {
		if got := r.warnPushovers(title); got != 0 {
			t.Fatalf("the warning %q reached people at once: %d messages", title, got)
		}
	}
	heldText := "MMHotline holds this warning with the SOS the triage agent is handling (The server db02 stopped answering); it reaches people if the person on call is woken, or at " + r.start.Add(8*time.Minute).UTC().Format("15:04") + ", unless the agent's report covers it or it resolves first."
	if page := r.get(t, "/alerts/"+first.ID); !strings.Contains(page, heldText) {
		t.Errorf("the alert page while held:\n%s", page)
	}
	if dashboard := r.get(t, "/dashboard"); !strings.Contains(liveSectionText(dashboard, "held-warnings"), heldText) {
		t.Errorf("the dashboard while held: %q", liveSectionText(dashboard, "held-warnings"))
	}
	r.resolves(t, 4*time.Minute, hubWarningApp021, "app021-hub")
	r.resolves(t, 4*time.Minute, hubWarningApp022, "app022-hub")
	run2 := r.resumedRun(t, run)
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	// The end message goes at once, not after the one-a-minute grouping.
	r.at(t, r.a.Now().Sub(r.start)+time.Second)
	if !r.pushoverTitled("[Resolved SOS] Your site works again: The server db02 stopped answering") {
		t.Error("the end message waited for the grouping")
	}
	r.at(t, 6*time.Minute)
	report(t, run2, json.NewDecoder(run2.connection), "0000000000000001", "info", "# app021, app022, lb02 and lb03 are back\n")
	_ = answers
	// The caddy warns the report covered resolve later.
	r.resolves(t, 7*time.Minute, caddyWarningLB02, "lb02-caddy")
	r.resolves(t, 7*time.Minute, caddyWarningLB03, "lb03-caddy")
	r.at(t, 30*time.Minute)
	// Nobody hears that warnings they never heard ended.
	for _, m := range r.fake.PushoverMessages() {
		if strings.HasPrefix(m.Title, "[Resolved warning]") {
			t.Errorf("a resolved message of a warning never sent reached people: %q", m.Title)
		}
	}
	for _, title := range []string{hubWarningApp021, hubWarningApp022, caddyWarningLB02, caddyWarningLB03} {
		if got := r.warnPushovers(title); got != 0 {
			t.Errorf("the warning %q was sent: %d messages", title, got)
		}
	}
	// The one warning anybody heard is the replacement's own.
	for _, m := range r.fake.PushoverMessages() {
		if strings.HasPrefix(m.Title, "[Warning]") && m.Title != "[Warning] Your site is down: The server db02 stopped answering" {
			t.Errorf("an unexpected warning reached people: %q", m.Title)
		}
	}
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 7)
	if r.woken(sos.IncidentID) {
		t.Error("the person on call was woken")
	}
	for _, id := range []string{first.ID} {
		if outcome, _, ended := r.hold(t, id); outcome != "resolved" || !ended {
			t.Errorf("hold of the first warn: %q %v", outcome, ended)
		}
	}
	for _, id := range []string{third.ID, fourth.ID} {
		if outcome, link, ended := r.hold(t, id); outcome != "taken_over" || !ended || !strings.HasPrefix(link, "/alerts/") {
			t.Errorf("hold of a warn the report covers: %q %q %v", outcome, link, ended)
		}
	}
	if page := r.get(t, "/alerts/"+first.ID); !strings.Contains(page, "MMHotline sent nothing for this warning. The warning's alert resolved while the triage agent handled the outage.") {
		t.Errorf("the alert page after the alert resolved:\n%s", page)
	}
}

// The run that carried the warn reports at warn: only the report sounds.
func TestJoinedWarnTheCarryingRunReportsOnIsNotSent(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 3)
	r.sosArrives(t, "The server db02 stopped answering", "db02")
	run := r.runStarted(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	joined := r.joins(t, time.Minute, loadWarning, "app002-load")
	run2 := r.resumedRun(t, run)
	r.at(t, 2*time.Minute)
	report(t, run2, json.NewDecoder(run2.connection), "0000000000000001", "warn", "# app002 needs a person to look at its disks\n")
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 10*time.Minute)
	if got := r.warnPushovers(loadWarning); got != 0 {
		t.Fatalf("the joined warn was sent: %d messages", got)
	}
	r.sentOnce(t, "app002 needs a person to look at its disks", 0)
	if outcome, link, _ := r.hold(t, joined.ID); outcome != "taken_over" || !strings.HasPrefix(link, "/alerts/") {
		t.Fatalf("hold: %q %q", outcome, link)
	}
}

// A report from a run that did not carry the joined warn does not take the
// warn over; the report of the run that carries it does.
func TestJoinedWarnAnUnrelatedReportDoesNotTakeItOver(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 3)
	r.sosArrives(t, "The server db02 stopped answering", "db02")
	run := r.runStarted(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	joined := r.joins(t, time.Minute, loadWarning, "app002-load")
	r.at(t, 90*time.Second)
	report(t, run, json.NewDecoder(run.connection), "0000000000000001", "info", "# db02 is back, and nothing else looked at\n")
	r.at(t, 2*time.Minute)
	if outcome, _, ended := r.hold(t, joined.ID); ended {
		t.Fatalf("an unrelated report ended the hold: %q", outcome)
	}
	if got := r.warnPushovers(loadWarning); got != 0 {
		t.Fatalf("the joined warn was sent: %d messages", got)
	}
	run2 := r.resumedRun(t, run)
	r.at(t, 150*time.Second)
	report(t, run2, json.NewDecoder(run2.connection), "0000000000000002", "info", "# The load on app002 is normal\n")
	if outcome, _, ended := r.hold(t, joined.ID); outcome != "taken_over" || !ended {
		t.Fatalf("the carrying run's report left the hold: %q %v", outcome, ended)
	}
}

// An SOS of the session wakes the person: the joined warns are delivered then,
// once, because the person is told everything.
func TestJoinedWarnIsDeliveredOnceWhenAnSOSWakesThePerson(t *testing.T) {
	parallelStartingProcesses(t)
	r := &heldWarnRig{replacementRig(t, true)}
	r.setWaits(t, 3, 7, 3)
	_, _, _, ask := replacementSession(t, r.triageHoldRig)
	r.at(t, time.Minute)
	if answer := ask(replaceFailedServers); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("replace_failed_servers: %+v", answer)
	}
	joined := r.joins(t, 2*time.Minute, loadWarning, "app002-load")
	if got := r.warnPushovers(loadWarning); got != 0 {
		t.Fatalf("the joined warn was sent before the wake: %d messages", got)
	}
	r.at(t, 3*time.Minute)
	if answer := ask(listedEveryServer); answer.Outcome != triageagent.RanAndFailed {
		t.Fatalf("every_server: %+v", answer)
	}
	r.at(t, 4*time.Minute)
	r.at(t, 8*time.Minute)
	if got := r.warnPushovers(loadWarning); got != 2 {
		t.Fatalf("%d messages about the joined warn after the wake; want one to each of 2 people", got)
	}
	r.at(t, 40*time.Minute)
	if got := r.warnPushovers(loadWarning); got != 2 {
		t.Fatalf("%d messages about the joined warn later; want still 2", got)
	}
	if outcome, _, _ := r.hold(t, joined.ID); outcome != "delivered" {
		t.Fatalf("hold: %q", outcome)
	}
}

// With nobody waking anyone, the agent silent, and the SOS resolved: the
// joined warn is delivered once at its deadline, the later of its own wait
// and the SOS's deadline, and not before.
func TestJoinedWarnIsDeliveredOnceAtItsDeadline(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 3)
	r.sosArrives(t, "The server db02 stopped answering", "db02")
	r.runStarted(t)
	joined := r.joins(t, 30*time.Second, loadWarning, "app002-load")
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, time.Minute)
	wantHeld := "or at " + r.start.Add(5*time.Minute).UTC().Format("15:04") + ","
	if page := r.get(t, "/alerts/"+joined.ID); !strings.Contains(page, wantHeld) {
		t.Errorf("the alert page while held lacks %q:\n%s", wantHeld, page)
	}
	r.at(t, 5*time.Minute-time.Second)
	if got := r.warnPushovers(loadWarning); got != 0 {
		t.Fatalf("delivered before the deadline: %d messages", got)
	}
	r.at(t, 5*time.Minute)
	r.sentOnce(t, loadWarning, 0)
	r.at(t, 20*time.Minute)
	if got := r.warnPushovers(loadWarning); got != 2 {
		t.Fatalf("%d messages; want exactly one to each of 2 people", got)
	}
	if outcome, _, _ := r.hold(t, joined.ID); outcome != "delivered" {
		t.Fatalf("hold: %q", outcome)
	}
}

// A warn that joins while no SOS waits for the agent is delivered at once, and
// with the setting at 0 nothing is held.
func TestJoinedWarnWithNoSOSWaitingIsDeliveredAtOnce(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("the SOS resolved already", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		r.sosArrives(t, "The server db02 stopped answering", "db02")
		r.runStarted(t)
		admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
		joined := r.joins(t, time.Minute, loadWarning, "app002-load")
		r.sentOnce(t, loadWarning, 0)
		if outcome, _, _ := r.hold(t, joined.ID); outcome != "delivered" {
			t.Fatalf("hold: %q", outcome)
		}
	})
	t.Run("the setting is 0", func(t *testing.T) {
		r := newHeldWarnRig(t, 0)
		r.sosArrives(t, "The server db02 stopped answering", "db02")
		r.runStarted(t)
		joined := r.joins(t, time.Minute, loadWarning, "app002-load")
		r.sentOnce(t, loadWarning, 0)
		if n := countRows(t, r.a, `SELECT count(*) FROM held_warn_alerts WHERE alert_id=?`, joined.ID); n != 0 {
			t.Fatal("a warn was recorded as held with the setting at 0")
		}
	})
}

// A restart keeps the hold of a joined warn, which then ends exactly once.
func TestJoinedWarnSurvivesARestart(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 3)
	r.sosArrives(t, "The server db02 stopped answering", "db02")
	run := r.runStarted(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	joined := r.joins(t, 30*time.Second, loadWarning, "app002-load")
	r.advance(r.start.Add(time.Minute).Sub(r.a.Now()))
	r.a.CloseTriageAgent()
	r.a = restartApp(t, r.a)
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
		t.Fatal(err)
	}
	r.runs = triageRunFixture(t, r.a)
	r.runStarted(t)
	if outcome, _, ended := r.hold(t, joined.ID); ended {
		t.Fatalf("the restart ended the hold: %q", outcome)
	}
	r.at(t, 5*time.Minute-time.Second)
	if got := r.warnPushovers(loadWarning); got != 0 {
		t.Fatalf("delivered before the deadline: %d messages", got)
	}
	r.at(t, 5*time.Minute)
	r.at(t, 12*time.Minute)
	r.at(t, 30*time.Minute)
	if got := r.warnPushovers(loadWarning); got != 2 {
		t.Fatalf("%d messages; want exactly one to each of 2 people", got)
	}
	if outcome, _, _ := r.hold(t, joined.ID); outcome != "delivered" {
		t.Fatalf("hold: %q", outcome)
	}
}

// pushoverTitled reports whether Pushover got a message with the title.
func (r *heldWarnRig) pushoverTitled(title string) bool {
	for _, m := range r.fake.PushoverMessages() {
		if m.Title == title {
			return true
		}
	}
	return false
}
