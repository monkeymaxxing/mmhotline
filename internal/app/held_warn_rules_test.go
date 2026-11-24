package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

// The hold rules every held warn follows, walked through MMHotline's own
// paths: the alert intake, the background loop, a fake sandbox, a stand-in
// MMX, and a fake Pushover. Each held warn ends once: taken over by a report
// or a playbook run request that names it, from a run that carried it;
// resolved; or delivered, when its run ends without naming it, when people
// are told everything, or at its deadline.

const (
	app001Warning = "app001: CVE-2026-123158 openssh is known to be exploited"
	db01Warning   = "db01: CVE-2026-123158 openssh is known to be exploited"
	diskWarning   = "Disk of db001 is filling"
)

// agentRequest sends one of the agent's requests and returns MMHotline's
// answer.
func agentRequest(t *testing.T, run fakeTriageRun, answers *json.Decoder, event triageagent.Event) triageagent.Answer {
	t.Helper()
	sendTriageEvent(t, run, event)
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	var control triageagent.Control
	if err := answers.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != event.RequestID {
		t.Fatalf("answer: %+v %v", control, err)
	}
	return *control.Answer
}

// reportEvent is the agent's report, about the alerts named.
func reportEvent(requestID, severity, text string, about ...string) triageagent.Event {
	return triageagent.Event{Event: "report", RequestID: requestID, AlertSeverity: severity, ReportText: text, AboutAlertIDs: about}
}

// rebootEvent is the agent's request to run the reboot playbook on
// production, about the alerts named.
func rebootEvent(requestID string, about ...string) triageagent.Event {
	return triageagent.Event{Event: "playbook_run_requested", RequestID: requestID, Playbook: rebootPlaybook, Inventory: "production", Diagnosis: testDiagnosis, AboutAlertIDs: about}
}

// warnsArrive admits warns from the alert source, title and key in turn,
// before the background loop reads any of them, so one run carries them
// all; the test's clock starts.
func (r *heldWarnRig) warnsArrive(t *testing.T, titlesAndKeys ...string) []Alert {
	t.Helper()
	r.start = r.a.Now()
	var alerts []Alert
	for index := 0; index < len(titlesAndKeys); index += 2 {
		alerts = append(alerts, admitSignedAlert(t, r.a, r.source, "warn", titlesAndKeys[index], titlesAndKeys[index+1]))
	}
	return alerts
}

// holdOf reads the hold of a warn: its outcome, whether it ended, and
// whether MMHotline marked it for delivery.
func (r *heldWarnRig) holdOf(t *testing.T, alertID string) (outcome, reason string, ended, released bool) {
	t.Helper()
	var endedAt, releasedAt int64
	var outcomeReason, releaseReason string
	if err := r.a.Store.db.QueryRow(`SELECT outcome,outcome_reason,release_reason,ended_at,release_requested_at FROM held_warn_alerts WHERE alert_id=?`, alertID).Scan(&outcome, &outcomeReason, &releaseReason, &endedAt, &releasedAt); err != nil {
		t.Fatalf("hold of %s: %v", alertID, err)
	}
	if endedAt == 0 {
		return outcome, releaseReason, false, releasedAt > 0
	}
	return outcome, outcomeReason, true, releasedAt > 0
}

// The db01 case from monkeymaxxing.com: app001's warning starts a session,
// db01's arrives during the first run and joins it. Each run's report covers
// its own warning, so no warning reaches a phone. When the first run fails
// instead, app001's warning goes out at the failure, and db01's still waits
// for the run that carries it.
func TestHeldWarnTheDB01Replay(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("each run reports on its own warning", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		app001 := r.warnArrives(t, app001Warning, "app001-cve")
		run1 := r.runStarted(t)
		sendTriageEvent(t, run1, triageagent.Event{Event: "session", SessionID: "session-one"})
		waitSessionIDSaved(t, r.a, "session-one")
		db01 := r.joins(t, 60*time.Second, db01Warning, "db01-cve")
		r.at(t, 66*time.Second)
		// The first run carried one alert, so its report covers it without naming it.
		report(t, run1, json.NewDecoder(run1.connection), "0000000000000001", "info", "# app001 runs a fixed openssh\n")
		run2 := r.resumedRun(t, run1)
		if !strings.Contains(run2.request.Prompt, "Alert ID: "+db01.ID) || strings.Contains(run2.request.Prompt, "Alert ID: "+app001.ID) {
			t.Fatalf("the second run's prompt does not carry db01's warning alone:\n%s", run2.request.Prompt)
		}
		r.at(t, 95*time.Second)
		agentRequest(t, run2, json.NewDecoder(run2.connection), reportEvent("0000000000000001", "info", "# db01 runs a fixed openssh\n", db01.ID))
		sendTriageEvent(t, run2, triageagent.Event{Event: "finished", Answer: "Reported."})
		waitNoTriageRun(t, r.a)
		r.at(t, 30*time.Minute)
		for _, title := range []string{app001Warning, db01Warning} {
			if got := r.warnPushovers(title); got != 0 {
				t.Errorf("the warning %q reached people: %d messages", title, got)
			}
		}
		for _, alert := range []Alert{app001, db01} {
			if outcome, _, ended, _ := r.holdOf(t, alert.ID); outcome != "taken_over" || !ended {
				t.Errorf("hold of %q: %q %v", alert.Title, outcome, ended)
			}
		}
	})
	t.Run("the first run fails", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		app001 := r.warnArrives(t, app001Warning, "app001-cve")
		run1 := r.runStarted(t)
		sendTriageEvent(t, run1, triageagent.Event{Event: "session", SessionID: "session-one"})
		waitSessionIDSaved(t, r.a, "session-one")
		db01 := r.joins(t, 60*time.Second, db01Warning, "db01-cve")
		r.at(t, 66*time.Second)
		sendTriageEvent(t, run1, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
		waitNoTriageRun(t, r.a)
		run2 := r.runStarted(t)
		r.reachedSomeone(t, app001Warning)
		if outcome, reason, _, _ := r.holdOf(t, app001.ID); outcome != "delivered" || !strings.HasPrefix(reason, "The triage agent's run failed: ") {
			t.Fatalf("app001's hold: %q %q", outcome, reason)
		}
		if _, _, ended, released := r.holdOf(t, db01.ID); ended || released {
			t.Fatal("db01's warning, which the failed run never carried, was released")
		}
		agentRequest(t, run2, json.NewDecoder(run2.connection), reportEvent("0000000000000001", "info", "# db01 runs a fixed openssh\n"))
		r.at(t, 30*time.Minute)
		if got := r.warnPushovers(app001Warning); got != 2 {
			t.Errorf("%d messages about app001's warning; want one to each of 2 people", got)
		}
		if got := r.warnPushovers(db01Warning); got != 0 {
			t.Errorf("db01's warning reached people: %d messages", got)
		}
	})
}

// A run that carried warns about two problems sends one report naming one
// of them: that warn is taken over, the other is delivered when the run
// ends. A report or a playbook run request that names no alert in such a
// run covers nothing, and its answer tells the agent to name the alerts.
func TestHeldWarnOneReportAboutOneOfTwoProblems(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)
	prepareApprovals(t, r.a, "")
	warns := r.warnsArrive(t, loadWarning, "app002-load", diskWarning, "db001-disk")
	run := r.runStarted(t)
	answers := json.NewDecoder(run.connection)
	nameThem := "This run carried 2 alerts, and your report named none of them with --about-alert-id, so it covers no warning."
	if answer := agentRequest(t, run, answers, reportEvent("0000000000000001", "info", "# Looked at app002 and db001\n")); answer.Outcome != triageagent.ReportRecorded || !strings.Contains(answer.Message, nameThem) {
		t.Fatalf("the report without --about-alert-id: %+v", answer)
	}
	if answer := agentRequest(t, run, answers, rebootEvent("0000000000000002")); answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "This run carried 2 alerts, and your playbook run request named none of them") {
		t.Fatalf("the request without --about-alert-id: %+v", answer)
	}
	for _, warn := range warns {
		if outcome, _, ended, released := r.holdOf(t, warn.ID); ended || released {
			t.Fatalf("a report or request that names nothing ended the hold of %q: %q", warn.Title, outcome)
		}
	}
	if answer := agentRequest(t, run, answers, reportEvent("0000000000000003", "info", "# The load on app002 is normal again\n", warns[0].ID)); answer.Outcome != triageagent.ReportRecorded || strings.Contains(answer.Message, "--about-alert-id") {
		t.Fatalf("the report naming app002's warn: %+v", answer)
	}
	if outcome, _, _, _ := r.holdOf(t, warns[0].ID); outcome != "taken_over" {
		t.Fatalf("the named warn: %q", outcome)
	}
	r.at(t, time.Minute)
	if got := r.warnPushovers(diskWarning); got != 0 {
		t.Fatalf("db001's warning went out while the run went on: %d", got)
	}
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	waitNoTriageRun(t, r.a)
	r.at(t, 90*time.Second)
	r.sentOnce(t, diskWarning, 0)
	if outcome, reason, _, _ := r.holdOf(t, warns[1].ID); outcome != "delivered" || reason != "The triage agent's run ended, and no report or playbook run request of it named the warning." {
		t.Fatalf("db001's hold: %q %q", outcome, reason)
	}
	if got := r.warnPushovers(loadWarning); got != 0 {
		t.Fatalf("app002's warning reached people: %d", got)
	}
	page := r.get(t, "/alerts/"+warns[1].ID)
	if !strings.Contains(page, "The triage agent's run ended, and no report or playbook run request of it named the warning.") {
		t.Errorf("the alert page of the delivered warn:\n%s", page)
	}
}

// A report at sos that names a warn-only problem opens an incident of its
// own, which wakes the person on call, even when its run also carried a
// claimed SOS incident of another problem.
func TestHeldWarnAnSOSReportAboutAWarnOnlyProblemOpensItsOwnIncident(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)
	sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
	warn := admitSignedAlert(t, r.a, r.source, "warn", loadWarning, "app002-load")
	incident, err := r.a.GetIncident(context.Background(), r.s.Principal, sos.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.s.Principal, sos.IncidentID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	run := r.runStarted(t)
	if answer := agentRequest(t, run, json.NewDecoder(run.connection), reportEvent("0000000000000001", "sos", "# app002 is overloaded and needs a person now\n", warn.ID)); answer.Outcome != triageagent.ReportRecorded {
		t.Fatalf("answer: %+v", answer)
	}
	var reportIncidentID string
	if err := r.a.Store.db.QueryRow(`SELECT incident_id FROM alerts WHERE actor='triage_agent'`).Scan(&reportIncidentID); err != nil {
		t.Fatal(err)
	}
	if reportIncidentID == "" || reportIncidentID == sos.IncidentID {
		t.Fatalf("the report went with incident %q; want one of its own, not the claimed SOS's %q", reportIncidentID, sos.IncidentID)
	}
	r.at(t, time.Minute)
	if !r.woken(reportIncidentID) {
		t.Fatal("the report's own incident woke nobody")
	}
	if outcome, _, _, _ := r.holdOf(t, warn.ID); outcome != "taken_over" {
		t.Fatalf("the named warn: %q", outcome)
	}
	if history := r.history(t, sos.IncidentID); inOrder(history, "app002 is overloaded") {
		t.Errorf("the claimed SOS's history records the report about another problem: %q", history)
	}
}

// Without --about-alert-id, a run that carried two warns of one problem
// covers neither: one key is not one alert, as two affected versions of one
// CVE share it. An SOS report without the option in such a run still goes
// with the run's incident and wakes its waiting SOS, as before.
func TestHeldWarnNoImplicitCoverageWithSeveralAlerts(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("two warns of one key", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		warns := r.warnsArrive(t, app001Warning, "openssh-cve", "app001: CVE-2026-123158 openssh 9.6 is known to be exploited", "openssh-cve")
		run := r.runStarted(t)
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "info", "# Both are fixed\n")
		sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		waitNoTriageRun(t, r.a)
		r.at(t, time.Minute)
		for _, warn := range warns {
			if outcome, _, _, _ := r.holdOf(t, warn.ID); outcome != "delivered" {
				t.Errorf("hold of %q: %q", warn.Title, outcome)
			}
		}
	})
	t.Run("an SOS report goes with the run's incident", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
		admitSignedAlert(t, r.a, r.source, "warn", loadWarning, "app002-load")
		run := r.runStarted(t)
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "sos", "# db02 needs a person\n")
		var reportIncidentID string
		if err := r.a.Store.db.QueryRow(`SELECT incident_id FROM alerts WHERE actor='triage_agent'`).Scan(&reportIncidentID); err != nil || reportIncidentID != sos.IncidentID {
			t.Fatalf("the report went with %q, want the SOS's incident: %v", reportIncidentID, err)
		}
		r.at(t, time.Minute)
		if !r.woken(sos.IncidentID) {
			t.Fatal("the waiting SOS was not woken")
		}
	})
}

// The other ways a held warn ends.
func TestHeldWarnOtherEndings(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("a person takes a warn-only session over", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		warn := r.warnArrives(t, loadWarning, "app002-load")
		r.runStarted(t)
		r.at(t, time.Minute)
		if err := r.a.TakeOverTriageSession(context.Background(), r.s.Principal, openTriageSessionID(t, r.a)); err != nil {
			t.Fatal(err)
		}
		backgroundRounds(t, r.a)
		r.reachedSomeone(t, loadWarning)
		if outcome, reason, _, _ := r.holdOf(t, warn.ID); outcome != "delivered" || reason != "Responder took the triage session over from the triage agent." {
			t.Fatalf("hold: %q %q", outcome, reason)
		}
		// A warn that joins the session a person took over is not held.
		later := admitSignedAlert(t, r.a, r.source, "warn", diskWarning, "db001-disk")
		backgroundRounds(t, r.a)
		if n := countRows(t, r.a, `SELECT count(*) FROM held_warn_alerts WHERE alert_id=?`, later.ID); n != 0 {
			t.Fatal("a warn that joined a session a person took over was held")
		}
		r.sentOnce(t, loadWarning, 0)
		r.sentOnce(t, diskWarning, 0)
	})
	t.Run("its alert resolves while it is held", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		warn := r.warnArrives(t, loadWarning, "app002-load")
		r.runStarted(t)
		r.resolves(t, 2*time.Minute, loadWarning, "app002-load")
		r.at(t, 30*time.Minute)
		for _, m := range r.fake.PushoverMessages() {
			if strings.Contains(m.Title, loadWarning) {
				t.Errorf("a message about the warn reached people: %q", m.Title)
			}
		}
		if outcome, reason, _, _ := r.holdOf(t, warn.ID); outcome != "resolved" || reason != warnResolvedWhileHeldReason {
			t.Fatalf("hold: %q %q", outcome, reason)
		}
	})
	t.Run("a server replacement moves its deadline", func(t *testing.T) {
		r := &heldWarnRig{replacementRig(t, false)}
		r.setWaits(t, 3, 7, 3)
		r.sosArrives(t, "The server db02 stopped answering", "db02")
		warn := admitSignedAlert(t, r.a, r.source, "warn", loadWarning, "app002-load")
		run := r.runStarted(t)
		r.at(t, time.Minute)
		answer := agentRequest(t, run, json.NewDecoder(run.connection), triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: replaceFailedServers, Inventory: "production", Diagnosis: testDiagnosis})
		if answer.Outcome != triageagent.RanAndSucceeded {
			t.Fatalf("replace_failed_servers: %+v", answer)
		}
		var heldUntil int64
		if err := r.a.Store.db.QueryRow(`SELECT held_until FROM held_warn_alerts WHERE alert_id=?`, warn.ID).Scan(&heldUntil); err != nil || heldUntil != r.start.Add(8*time.Minute).Unix() {
			t.Fatalf("the warn is held until %s, want the replacement's limit %s: %v", time.Unix(heldUntil, 0).UTC(), r.start.Add(8*time.Minute).UTC(), err)
		}
		r.at(t, 5*time.Minute)
		if got := r.warnPushovers(loadWarning); got != 0 {
			t.Fatalf("the warn went out at its own deadline: %d", got)
		}
	})
}

// The outcomes of a playbook run request. Only a request for which
// MMHotline tells a person now, a new warned run or approval request, takes
// over the warns it names; a playbook that ran, or that an earlier approval
// covers, tells nobody and takes nothing over. A request still waiting for
// its MMX run keeps only the warns it names held when its run ends.
func TestHeldWarnPlaybookOutcomes(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("a new approval request takes over only the warn it names", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		prepareApprovals(t, r.a, "")
		warns := r.warnsArrive(t, loadWarning, "app002-load", diskWarning, "db001-disk")
		run := r.runStarted(t)
		if answer := agentRequest(t, run, json.NewDecoder(run.connection), rebootEvent("0000000000000001", warns[0].ID)); answer.Outcome != triageagent.WaitsForAPerson {
			t.Fatalf("answer: %+v", answer)
		}
		if outcome, _, _, _ := r.holdOf(t, warns[0].ID); outcome != "taken_over" {
			t.Fatalf("the named warn: %q", outcome)
		}
		if _, _, ended, _ := r.holdOf(t, warns[1].ID); ended {
			t.Fatal("the warn the request did not name ended")
		}
	})
	t.Run("a playbook that ran takes nothing over", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		prepareApprovals(t, r.a, "echo 'PLAY RECAP app002 ok=1 changed=1'\n")
		if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: actThenReport}); err != nil {
			t.Fatal(err)
		}
		warn := r.warnArrives(t, loadWarning, "app002-load")
		run := r.runStarted(t)
		if answer := agentRequest(t, run, json.NewDecoder(run.connection), rebootEvent("0000000000000001", warn.ID)); answer.Outcome != triageagent.RanAndSucceeded {
			t.Fatalf("answer: %+v", answer)
		}
		if _, _, ended, _ := r.holdOf(t, warn.ID); ended {
			t.Fatal("a playbook that ran took the warn over")
		}
		sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Rebooted."})
		waitNoTriageRun(t, r.a)
		r.at(t, time.Minute)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("a playbook an earlier approval covers takes nothing over", func(t *testing.T) {
		// The rig applies no warn wait, so the default of 15 minutes holds.
		r, _ := approvalCoversRig(t, "echo 'PLAY RECAP db01 ok=1 changed=1'\n")
		sos := r.alert(t, "sos", "The server db01 stopped answering", "db01")
		warn := r.alert(t, "warn", hubWarningApp021, "app021-hub")
		r.drive(t)
		run := r.nextRun(t)
		answers := json.NewDecoder(run.connection)
		replace := triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: replaceFailedServers, Inventory: "production", Diagnosis: testDiagnosis, AboutAlertIDs: []string{sos.ID}}
		if answer := agentRequest(t, run, answers, replace); answer.Outcome != triageagent.WaitsForAPerson {
			t.Fatalf("replace_failed_servers: %+v", answer)
		}
		if err := r.a.ApprovePlaybookRun(context.Background(), r.p, waitingRows(t, r.a)[0].id); err != nil {
			t.Fatal(err)
		}
		runApprovedPlaybooks(t, r.a)
		everyServer := triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000002", Playbook: listedEveryServer, Inventory: "production", Diagnosis: testDiagnosis, AboutAlertIDs: []string{warn.ID}}
		if answer := agentRequest(t, run, answers, everyServer); answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "without asking a person again") {
			t.Fatalf("every_server: %+v", answer)
		}
		if open := countRows(t, r.a, `SELECT count(*) FROM held_warn_alerts WHERE alert_id=? AND ended_at=0`, warn.ID); open != 1 {
			t.Fatal("a covered playbook run took the warn over")
		}
	})
	t.Run("a request still waiting for its MMX run keeps only its warns held", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		prepareApprovals(t, r.a, "echo 'PLAY RECAP app002 ok=1 changed=1'\n")
		if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: actThenReport}); err != nil {
			t.Fatal(err)
		}
		warns := r.warnsArrive(t, loadWarning, "app002-load", diskWarning, "db001-disk")
		run := r.runStarted(t)
		agentRequest(t, run, json.NewDecoder(run.connection), rebootEvent("0000000000000001", warns[0].ID))
		// The run ends while its request still waits for MMX: a Stop or the
		// run's time limit can end a run during an MMX run, which outlasts it.
		if _, err := r.a.Store.db.Exec(`UPDATE triage_agent_requests SET outcome='',answered_at=0 WHERE request_id='0000000000000001'`); err != nil {
			t.Fatal(err)
		}
		sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Asked for a reboot."})
		waitNoTriageRun(t, r.a)
		r.at(t, time.Minute)
		if _, _, ended, released := r.holdOf(t, warns[0].ID); ended || released {
			t.Fatal("the warn the waiting request names was released at the run's end")
		}
		if outcome, _, _, _ := r.holdOf(t, warns[1].ID); outcome != "delivered" {
			t.Fatalf("the warn the request does not name: %q", outcome)
		}
	})
}

// An SOS wakes the person on call while the ordinary delivery queue is full
// and the SOS's alert source is at its share of SOS deliveries. The wake
// commits; the held warning and the SOS's email, which waited with the
// first waking step, stay marked through a restart, a later report cannot
// take the warning over, and both go out once the queue has room.
func TestHeldWarnReleaseWithAFullDeliveryQueue(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)
	r.addEmail(t)
	sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
	warn := admitSignedAlert(t, r.a, r.source, "warn", loadWarning, "app002-load")
	run := r.runStarted(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	if states := r.emailStates(t, sos.IncidentID); len(states) != 0 {
		t.Fatalf("the SOS's email went out before the agent looked: %q", states)
	}
	// Room for no ordinary delivery, and the alert source at its share of
	// SOS deliveries, with deliveries due long after the test.
	var emailID, memberID string
	if err := r.a.Store.db.QueryRow(`SELECT id,member_id FROM contact_methods WHERE contact_method_type='email'`).Scan(&emailID, &memberID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Store.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO deliveries(id,notification_key,alert_id,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,created_at,next_attempt)
SELECT 'filler'||i,'filler:'||i,CASE WHEN i<=? THEN ? ELSE '' END,?,1,?,'email','x','subscribed_alert',CASE WHEN i<=? THEN 'sos' ELSE 'info' END,'s','m','queued',?,? FROM n`,
		maxPendingDeliveries-sosReservedPendingDeliveries, maxPendingSOSDeliveriesPerAlertSource, sos.ID, emailID, memberID, maxPendingSOSDeliveriesPerAlertSource, r.a.Now().Unix(), r.a.Now().Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	r.at(t, 5*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the full queue held up the wake")
	}
	if _, _, ended, released := r.holdOf(t, warn.ID); ended || !released {
		t.Fatalf("the warn after the wake: ended %v, released %v; want released and waiting for room", ended, released)
	}
	if page := r.get(t, "/alerts/"+warn.ID); !strings.Contains(page, "MMHotline is sending this warning now. An SOS woke the person on call: The triage agent's time ran out.") {
		t.Errorf("the alert page of a released warn:\n%s", page)
	}
	r.advance(time.Minute)
	r.a.CloseTriageAgent()
	r.a = restartApp(t, r.a)
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
		t.Fatal(err)
	}
	r.runs = triageRunFixture(t, r.a)
	// The restart cut the run short, so the next run carries the warn again.
	resumed := r.runStarted(t)
	if got := r.warnPushovers(loadWarning); got != 0 {
		t.Fatalf("the warn went out with the queue full: %d", got)
	}
	if states := r.emailStates(t, sos.IncidentID); len(states) != 0 {
		t.Fatalf("the SOS's email was queued past its source's share: %q", states)
	}
	if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM deliveries WHERE id LIKE 'filler%'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The run that carries the warn reports on it before the background
	// loop's next visit: the report reaches people, and the released warn
	// still goes out.
	if answer := agentRequest(t, resumed, json.NewDecoder(resumed.connection), reportEvent("0000000000000001", "info", "# app002 is fine\n", warn.ID)); answer.Outcome != triageagent.ReportRecorded {
		t.Fatalf("the report: %+v", answer)
	}
	if outcome, _, ended, _ := r.holdOf(t, warn.ID); ended {
		t.Fatalf("a report took over a released warn: %q", outcome)
	}
	backgroundRounds(t, r.a)
	// Pushover may combine the warn with the report into one message.
	if got := len(r.pushovers(loadWarning)); got != 2 {
		t.Fatalf("%d messages about the warn once the queue had room; want one to each of 2 people", got)
	}
	if states := r.emailStates(t, sos.IncidentID); len(states) == 0 || states[0] != "sos not suppressed" {
		t.Fatalf("the SOS's email after the queue had room: %q", states)
	}
	r.at(t, 30*time.Minute)
	if got := len(r.pushovers(loadWarning)); got != 2 {
		t.Fatalf("%d messages about the warn; want exactly one to each of 2 people", got)
	}
}

// An SOS that resolves while it waits for the agent, with nobody claiming
// it, sends its email to nobody: nobody was woken either.
func TestHeldSOSEmailWaitsWithTheWakingStep(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("sent when the person on call is woken", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		r.addEmail(t)
		sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
		r.runStarted(t)
		r.at(t, 5*time.Minute-time.Second)
		if states := r.emailStates(t, sos.IncidentID); len(states) != 0 {
			t.Fatalf("the email went out during the wait: %q", states)
		}
		r.at(t, 5*time.Minute)
		if states := r.emailStates(t, sos.IncidentID); len(states) != 1 || states[0] != "sos not suppressed" {
			t.Fatalf("the email at the wake: %q", states)
		}
	})
	t.Run("never sent when it resolves during the wait", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		r.addEmail(t)
		sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
		r.runStarted(t)
		r.at(t, 2*time.Minute)
		admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
		r.at(t, 10*time.Minute)
		for _, state := range r.emailStates(t, sos.IncidentID) {
			if state != "resolved-sos suppressed" {
				t.Fatalf("an email about an SOS the agent's work ended: %q", state)
			}
		}
	})
	// The claim ends the wait, and the resolve comes before the background
	// step queues the email: the email still goes, and its end follows it.
	t.Run("sent with its end when it resolves before the email is queued", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		r.addEmail(t)
		sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
		r.runStarted(t)
		r.at(t, 2*time.Minute)
		ctx := context.Background()
		i, err := r.a.GetIncident(ctx, r.s.Principal, sos.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		if i, err = r.a.ClaimIncident(ctx, r.s.Principal, i.ID, i.Revision, ""); err != nil {
			t.Fatal(err)
		}
		if _, err = r.a.ResolveIncident(ctx, r.s.Principal, i.ID, i.Revision, ""); err != nil {
			t.Fatal(err)
		}
		r.at(t, 3*time.Minute)
		states := r.emailStates(t, sos.IncidentID)
		if len(states) < 2 || states[len(states)-2] != "sos not suppressed" || states[len(states)-1] != "resolved-sos not suppressed" {
			t.Fatalf("emails %q; want the SOS, then its end", states)
		}
	})
}

// A run carries only the alerts its prompt shows; the rest wait for the
// next run. With 120 long alerts the first prompt shows some of them, and
// the next run carries the next ones.
func TestTriageSessionRunCarriesOnlyTheAlertsItsPromptShows(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 30)
	r.start = r.a.Now()
	long := strings.Repeat("The disk fills. ", 200)
	var alerts []Alert
	for n := range 120 {
		if n == 60 {
			// An alert source may send 120 alerts a minute.
			r.advance(time.Minute)
		}
		body := fmt.Sprintf(`{"severity":"warn","title":"Disk %d is filling","message":%q,"sender_incident_key":"disk-%d"}`, n+1, long, n+1)
		alert, _, _, err := r.a.AdmitGeneric(context.Background(), r.source.Source.ID, alertSignature(r.source.Secret, r.a.Now(), []byte(body)), []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		alerts = append(alerts, alert)
	}
	run := r.runStarted(t)
	if !strings.Contains(run.request.Prompt, triageSessionAboutAlertsText) {
		t.Fatal("the session's task does not ask the agent to name the alerts it covers")
	}
	shown := regexp.MustCompile(`(?m)^Alert ID: ([0-9a-f]{32})$`).FindAllStringSubmatch(run.request.Prompt, -1)
	carried := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE sent_in_run_id!=''`)
	if len(shown) == 0 || len(shown) >= 120 || carried != len(shown) {
		t.Fatalf("the prompt shows %d alerts and the run carries %d; want the same number, fewer than 120", len(shown), carried)
	}
	if !strings.Contains(run.request.Prompt, fmt.Sprintf("And %d more alerts, which MMHotline left out", 120-len(shown))) {
		t.Fatalf("the prompt does not say how many alerts it left out:\n%.300s", run.request.Prompt[len(run.request.Prompt)-600:])
	}
	for _, match := range shown {
		if n := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=? AND sent_in_run_id!=''`, match[1]); n != 1 {
			t.Fatalf("an alert the prompt shows is not carried: %s", match[1])
		}
	}
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	next := r.resumedRun(t, run)
	if !strings.Contains(next.request.Prompt, "Alert ID: "+alerts[len(shown)].ID) || strings.Contains(next.request.Prompt, "Alert ID: "+alerts[0].ID) {
		t.Fatal("the next run does not carry the alerts the first one left out")
	}
}

// A prompt that results and notes fill still shows, and carries, the first
// waiting alert, its message cut to fit.
func TestTriageSessionPromptFullOfResultsStillCarriesOneAlert(t *testing.T) {
	t.Parallel()
	alerts := []triageSessionPromptAlert{
		{ID: "0123456789abcdef0123456789abcdef", SourceName: "Monitoring", Severity: "warn", Title: "Disk of db001 is filling", Message: strings.Repeat("x", 1500) + "END", AlertURL: "https://mmhotline.example.com/alerts/1", Waiting: true},
		{ID: "fedcba9876543210fedcba9876543210", SourceName: "Monitoring", Severity: "warn", Title: "Load on app002 is high", Message: "load 40", AlertURL: "https://mmhotline.example.com/alerts/2", Waiting: true},
	}
	results := []string{strings.Repeat("r", maximumTriageSessionPromptBytes-1000)}
	prompt, carried := triageSessionResumePrompt("", "", results, nil, alerts)
	if len(carried) != 1 || carried[0] != alerts[0].ID {
		t.Fatalf("carried %q; want the first waiting alert", carried)
	}
	if !strings.Contains(prompt, "Alert 1 of 2: warn from Monitoring\nAlert ID: "+alerts[0].ID) || !strings.Contains(prompt, " (cut)") || strings.Contains(prompt, "END") {
		t.Fatalf("the prompt does not show the first alert cut:\n%s", prompt[len(prompt)-800:])
	}
	if !strings.HasSuffix(prompt, "\n\nAnd 1 more alerts, which MMHotline left out to keep this prompt short.") {
		t.Fatalf("the prompt ends %q", prompt[len(prompt)-120:])
	}
	if size := jsonTextSize(prompt); size > maximumTriageSessionPromptBytes {
		t.Fatalf("the prompt is %d bytes in JSON", size)
	}
}
