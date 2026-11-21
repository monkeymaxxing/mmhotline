package app

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

// prepareWarnedRun makes the reboot playbook wait on production with Warn,
// wait, then act, for minutes, and otherwise prepares as prepareApprovals
// does. It returns the commit.
func prepareWarnedRun(t *testing.T, a *App, runMMX string, minutes int) string {
	t.Helper()
	judged := prepareApprovals(t, a, runMMX)
	if err := setAgentPlaybookRule(context.Background(), a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: warnWaitThenAct, WarningWaitMinutes: minutes}); err != nil {
		t.Fatal(err)
	}
	return judged
}

type warnedRunRecord struct {
	id, commitID, incidentID, state, result, resultForTheAgent, stoppedByMemberID, stopReason string
}

// warnedRunRecords lists the warned runs, oldest first.
func warnedRunRecords(t *testing.T, a *App) []warnedRunRecord {
	t.Helper()
	rows, err := a.Store.db.Query(`SELECT id,commit_id,incident_id,state,result_message,result_for_the_agent,stopped_by_member_id,stop_reason FROM triage_agent_playbook_runs_waiting_for_a_person WHERE rule='warn_wait_then_act' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []warnedRunRecord
	for rows.Next() {
		var r warnedRunRecord
		if err := rows.Scan(&r.id, &r.commitID, &r.incidentID, &r.state, &r.result, &r.resultForTheAgent, &r.stoppedByMemberID, &r.stopReason); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

func onlyWarnedRunRecord(t *testing.T, a *App) warnedRunRecord {
	t.Helper()
	records := warnedRunRecords(t, a)
	if len(records) != 1 {
		t.Fatalf("%d warned runs: %+v", len(records), records)
	}
	return records[0]
}

// warningTitle is the warning's title for the reboot playbook on production,
// which changes app002.
func warningTitle(at string) string {
	return "MMHotline will run " + rebootPlaybook + " in the inventory “production” for the triage agent at " + at + " unless someone presses Stop: it can change app002"
}

// A playbook that waits with Warn, wait, then act makes MMHotline warn the
// team at warn, with its run time: the warning goes with the session's
// incident, wakes nobody, starts no session, and resumes nobody. The
// playbook does not run before its time; at its time it runs with MMX at
// the judged commit, and the result resumes the agent's session. Without an
// incident, the warning links the Approvals page, and the background loop
// comes back at the run time by itself.
func TestWarnedPlaybookRun(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("with the session's incident", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		records := t.TempDir()
		judged := prepareWarnedRun(t, r.a, fakeRunMMX(records, "echo 'PLAY RECAP app002 ok=1 changed=1'\n"), 10)
		sos, run, answer := approvalSession(t, r)
		// The clock stands at 12:00 UTC, so the run time is 12:10 UTC.
		if answer.Outcome != triageagent.WaitsForAPerson || !strings.HasSuffix(answer.Message, "Warn, wait 10 minutes, then act, so the run waits for a person. MMHotline warned the team that MMHotline runs the playbook at 12:10 UTC unless someone presses Stop. When the playbook run ends or someone stops the playbook run, MMHotline resumes your triage session with the result. Commit nothing more until MMHotline resumes you: a newer commit can make the warning stale, and the run then waits again. End your turn now and report.") {
			t.Fatalf("answer: %+v", answer)
		}
		var alertID, severity, incidentID, message string
		if err := r.a.Store.db.QueryRow(`SELECT id,severity,incident_id,message FROM alerts WHERE title=?`, warningTitle("12:10 UTC")).Scan(&alertID, &severity, &incidentID, &message); err != nil {
			t.Fatal(err)
		}
		if severity != "warn" || incidentID != sos.IncidentID || !strings.Contains(message, "\n\nMMHotline runs the playbook at 12:10 UTC unless someone presses Stop before then. The rule is Warn, wait 10 minutes, then act.\n\nThe incident page shows a summary of the playbook run, with a Stop button. Press Stop on the playbook run page: "+strings.TrimRight(r.a.WebURL, "/")+"/approvals/"+onlyWarnedRunRecord(t, r.a).id) {
			t.Fatalf("warning: %s with incident %q: %s", severity, incidentID, message)
		}
		incident, err := r.a.GetIncident(context.Background(), r.p, sos.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		if incident.Step != triageAgentStep {
			t.Fatalf("the warning woke the person on call: step %s", incident.Step)
		}
		if history := incidentHistory(t, r.a, r.p, sos.IncidentID); !inOrder(history, "MMHotline runs "+rebootPlaybook+" in the inventory “production”, at commit "+judged+", at 12:10 UTC unless someone presses Stop: it can change app002.") {
			t.Fatalf("history: %q", history)
		}
		if sessions, joined := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`), countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=?`, alertID); sessions != 1 || joined != 0 {
			t.Fatalf("%d sessions, the warning joined %d times", sessions, joined)
		}
		// The run ends; MMHotline's own warning carries nothing to the agent.
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		r.drive(t)
		r.noRunStarts(t)

		r.advance(10*time.Minute - time.Second)
		runApprovedPlaybooks(t, r.a)
		if fileExists(filepath.Join(records, "arguments"))() {
			t.Fatal("the playbook ran before its time")
		}
		r.advance(time.Second)
		runApprovedPlaybooks(t, r.a)
		if arguments := readTestFile(t, filepath.Join(records, "arguments")); arguments != mmxRunArguments(r.a, "2026-11-03_reboot_app002")+"\n" {
			t.Fatalf("MMX's arguments: %q", arguments)
		}
		want := "Nobody stopped " + rebootPlaybook + " in the inventory “production” before 12:10 UTC. The run used commit " + judged + ", the one MMHotline checked. MMHotline ran the playbook with MMX, and the run succeeded.\n\nPlaybook run output:\nPLAY RECAP app002 ok=1 changed=1\n"
		if record := onlyWarnedRunRecord(t, r.a); record.state != "ran_and_succeeded" || record.result != want || record.resultForTheAgent != "waits_for_the_next_run" {
			t.Fatalf("record: %+v", record)
		}
		if history := incidentHistory(t, r.a, r.p, sos.IncidentID); !inOrder(history, "unless someone presses Stop: it can change app002.", want) {
			t.Fatalf("history: %q", history)
		}
		r.drive(t)
		resume := r.nextRun(t)
		if resume.request.Action != "resume" || resume.request.SessionID != "session-one" || !strings.Contains(resume.request.Prompt, "with the result of a playbook run you asked for, which waited for a person.") || !strings.Contains(resume.request.Prompt, "Result 1 of 1:\nRecorded at ") || !strings.Contains(resume.request.Prompt, " UTC.\n"+want) || !strings.Contains(resume.request.Prompt, "Times, all in UTC: it is now ") || !strings.Contains(resume.request.Prompt, "- run 1: started ") {
			t.Fatalf("resume: action %q, session %q:\n%s", resume.request.Action, resume.request.SessionID, resume.request.Prompt)
		}
	})
	t.Run("without an incident", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareWarnedRun(t, r.a, "", 3)
		runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		r.a.triageAgent.playbookRunner = runner
		r.alert(t, "warn", "app002 answers slowly", "")
		r.drive(t)
		run := r.nextRun(t)
		requestPlaybookRun(t, run, json.NewDecoder(run.connection), "0000000000000001")
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		var incidentID, message string
		if err := r.a.Store.db.QueryRow(`SELECT incident_id,message FROM alerts WHERE title=?`, warningTitle("12:03 UTC")).Scan(&incidentID, &message); err != nil {
			t.Fatal(err)
		}
		if incidentID != "" || onlyWarnedRunRecord(t, r.a).incidentID != "" || !strings.HasSuffix(message, "\n\nPress Stop on the playbook run page: "+strings.TrimRight(r.a.WebURL, "/")+"/approvals/"+onlyWarnedRunRecord(t, r.a).id) {
			t.Fatalf("warning with incident %q: %s", incidentID, message)
		}
		backgroundRounds(t, r.a)
		r.a.triageAgent.approvedPlaybookRuns.Wait()
		noRunnerCall(t, runner)
		// Nothing else is due at 12:03: the run time alone brings the loop back.
		r.advance(3 * time.Minute)
		backgroundRounds(t, r.a)
		r.a.triageAgent.approvedPlaybookRuns.Wait()
		if len(runner.calls) != 1 {
			t.Fatalf("%d runs at the run time; want one", len(runner.calls))
		}
	})
	t.Run("a session of warn alerts only stays open until its result", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareWarnedRun(t, r.a, "", 40)
		r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		r.alert(t, "warn", "app002 answers slowly", "")
		r.drive(t)
		run := r.nextRun(t)
		sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
		requestPlaybookRun(t, run, json.NewDecoder(run.connection), "0000000000000001")
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		// Past the 30 quiet minutes, the warned run still waits for 12:40.
		r.advance(35 * time.Minute)
		r.drive(t)
		if open := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`); open != 1 {
			t.Fatal("the session ended while its warned run waits")
		}
		r.advance(5 * time.Minute)
		runApprovedPlaybooks(t, r.a)
		r.drive(t)
		if resume := r.nextRun(t); resume.request.Action != "resume" || !strings.Contains(resume.request.Prompt, "Nobody stopped "+rebootPlaybook+" in the inventory “production” before 12:40 UTC.") {
			t.Fatalf("resume: %s", resume.request.Prompt)
		}
	})
}

// Stop before the run time ends a warned run: nothing runs, the incident's
// history records who and why, and the agent hears who stopped it. From the
// run time on, Stop is refused and the playbook runs.
func TestStoppedWarnedPlaybookRun(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("before its time", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareWarnedRun(t, r.a, "", 10)
		runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		r.a.triageAgent.playbookRunner = runner
		sos, run, _ := approvalSession(t, r)
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		r.advance(5 * time.Minute)
		const reason = "Reboot after the nightly backup finishes."
		if err := r.a.StopWarnedPlaybookRun(context.Background(), r.p, onlyWarnedRunRecord(t, r.a).id, reason); err != nil {
			t.Fatal(err)
		}
		responder, err := r.a.CurrentMember(context.Background(), r.p)
		if err != nil {
			t.Fatal(err)
		}
		want := "Responder stopped " + rebootPlaybook + " in the inventory “production”, so nothing ran. Responder's reason: " + reason
		record := onlyWarnedRunRecord(t, r.a)
		if record.state != "stopped" || record.stoppedByMemberID != responder.ID || record.stopReason != reason || record.result != want {
			t.Fatalf("record: %+v", record)
		}
		if history := incidentHistory(t, r.a, r.p, sos.IncidentID); !inOrder(history, "Stopped "+rebootPlaybook+" in the inventory “production”: "+reason) {
			t.Fatalf("history: %q", history)
		}
		if err := r.a.StopWarnedPlaybookRun(context.Background(), r.p, record.id, ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("a second Stop: %v", err)
		}
		r.advance(5 * time.Minute)
		runApprovedPlaybooks(t, r.a)
		noRunnerCall(t, runner)
		r.drive(t)
		if resume := r.nextRun(t); resume.request.Action != "resume" || !strings.Contains(resume.request.Prompt, want) {
			t.Fatalf("resume: %s", resume.request.Prompt)
		}
	})
	t.Run("at its time it is refused", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareWarnedRun(t, r.a, "", 10)
		runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		r.a.triageAgent.playbookRunner = runner
		_, run, _ := approvalSession(t, r)
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		// The background loop has not started it yet; the time alone decides.
		r.advance(10 * time.Minute)
		err := r.a.StopWarnedPlaybookRun(context.Background(), r.p, onlyWarnedRunRecord(t, r.a).id, "Too late?")
		if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "the playbook run can no longer be stopped because the scheduled run time, 2026-09-29 12:10:00 UTC, has arrived") {
			t.Fatalf("a Stop at the run time: %v", err)
		}
		if record := onlyWarnedRunRecord(t, r.a); record.state != "open" || record.stoppedByMemberID != "" {
			t.Fatalf("record after the refused Stop: %+v", record)
		}
		runApprovedPlaybooks(t, r.a)
		if len(runner.calls) != 1 {
			t.Fatalf("%d runs; want one", len(runner.calls))
		}
	})
}

// A restart before the run time keeps it; a restart after the run time runs
// the playbook once.
func TestWarnedPlaybookRunAcrossARestart(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name                      string
		restartAfter, thenAdvance time.Duration
	}{{"restarted before its time", 5 * time.Minute, 4 * time.Minute}, {"restarted after its time", 20 * time.Minute, 0}} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			prepareWarnedRun(t, r.a, "", 10)
			_, run, _ := approvalSession(t, r)
			r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
			r.advance(test.restartAfter)
			restarted := restartApp(t, r.a)
			if err := restarted.InitializeTriageAgent(context.Background(), "", "", ""); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
			restarted.triageAgent.playbookRunner = runner
			if test.thenAdvance > 0 {
				// 12:09: still a minute before the run time.
				r.advance(test.thenAdvance)
				runApprovedPlaybooks(t, restarted)
				noRunnerCall(t, runner)
				r.advance(time.Minute)
			}
			runApprovedPlaybooks(t, restarted)
			runApprovedPlaybooks(t, restarted)
			if len(runner.calls) != 1 {
				t.Fatalf("%d runs after the restart; want exactly one", len(runner.calls))
			}
			if state := onlyWarnedRunRecord(t, restarted).state; state != "ran_and_succeeded" {
				t.Fatalf("state %s", state)
			}
		})
	}
}

// When MMHotline's fetch moved the copy past the judged commit, nothing ran and
// the warning is stale: MMHotline judges the playbook again at the new commit
// and follows the new judgment, a new warning with a new run time, or a run
// when the rules for running the agent's playbooks now let it run without a person.
func TestStaleWarnedPlaybookRun(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name       string
		actWithout bool
	}{{"it warns again", false}, {"the new judgment lets it run", true}} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			records := t.TempDir()
			judged := prepareWarnedRun(t, r.a, miniRunMMX(records), 10)
			approvalSession(t, r)
			newer := pushToOriginOnly(t, r.a)
			if test.actWithout {
				if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: actThenReport}); err != nil {
					t.Fatal(err)
				}
			}
			r.advance(10 * time.Minute)
			runApprovedPlaybooks(t, r.a)
			warned := warnedRunRecords(t, r.a)
			stale := warned[0]
			if stale.state != "stale" || !strings.Contains(stale.result, "Nobody stopped "+rebootPlaybook+" in the inventory “production” before 12:10 UTC. Other commits reached MMHotline's copy of the systems repository before the run, so nothing ran at commit "+judged+". The warning does not cover the newer commits: the newer commits change README.md, outside the triage agent's notes. MMHotline checked the playbook again at the newer commit: ") {
				t.Fatalf("stale record: %+v", stale)
			}
			calls := readMMXCalls(t, records)
			if !test.actWithout {
				// Judged again at 12:10, the new commit runs at 12:20 unless stopped.
				if len(warned) != 2 || warned[1].state != "open" || warned[1].commitID != newer || stale.resultForTheAgent != "none" || !strings.HasSuffix(stale.result, "MMHotline warned the team again: it runs the playbook at 12:20 UTC unless someone presses Stop.") {
					t.Fatalf("records: %+v", warned)
				}
				if count := countRows(t, r.a, `SELECT count(*) FROM alerts WHERE title=?`, warningTitle("12:20 UTC")); count != 1 {
					t.Fatalf("%d warnings for the new commit", count)
				}
				if len(calls) != 0 {
					t.Fatalf("MMX calls: %q", calls)
				}
				return
			}
			if len(warned) != 1 || stale.resultForTheAgent != "waits_for_the_next_run" || !strings.Contains(stale.result, "the rules for running the agent's playbooks let MMHotline run the playbook without a person. MMHotline ran the playbook with MMX, and the run succeeded.") {
				t.Fatalf("records: %+v", warned)
			}
			if len(calls) != 1 || calls[0] != mmxRunArguments(r.a, "2026-11-03_reboot_app002") {
				t.Fatalf("MMX calls: %q", calls)
			}
		})
	}
}

// A warned run's own page says when MMHotline runs it by itself, with a
// countdown, and offers Stop until then, never Approve; the Approvals list
// gives its run time and Stop, and the incident page shows it in short, with
// Stop and Review first. A forged form is refused, a warned run cannot
// be approved, and a member's Stop goes through the operation without a
// reason. A Stop from an incident page returns there; the run's page then
// says who stopped it, and offers no Stop.
func TestWarnedPlaybookRunCard(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	judged := prepareWarnedRun(t, r.a, "", 10)
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	sos, _, _ := approvalSession(t, r)
	record := onlyWarnedRunRecord(t, r.a)
	r.advance(3 * time.Minute)
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	responder := approvalBrowsers(t, server)(r.p.SignInEmailAddress)
	requestPage := "/approvals/" + record.id
	page := responder.get(requestPage, 200)
	if text := approvalPageText(page); !phrasesInOrder(text, "Runs in 7 minutes", "Run “Reboot app002” on app002?",
		"MMHotline runs the playbook at 12:10, in 7 minutes, unless someone presses Stop.", "The playbook run needs no approval because the rule is Warn, wait 10 minutes, then act.", "Stop",
		"Reboot a server", "The playbook's text, and any files it copies, at commit "+judged[:7], "Why MMHotline warns first") {
		t.Errorf("the warned run's page: %s", text)
	}
	if !offers(page, requestPage+"/stop") || offers(page, requestPage+"/approve") || offers(page, requestPage+"/refuse") {
		t.Error("the warned run's page offers no Stop, or offers Approve or Refuse")
	}
	if t.Failed() {
		t.Fatal(page)
	}
	// A warned run goes ahead by itself, so the menu counts no request that
	// waits for a person; the list gives its run time and Stop.
	approvals := responder.get("/approvals", 200)
	if !phrasesInOrder(liveSectionText(approvals, "warned-playbook-runs"), "Run “Reboot app002” on app002?", "Runs at 12:10, in 7 minutes", "Stop") || !offers(approvals, requestPage+"/stop") || menuLinkText(approvals, "/approvals") != "Approvals" {
		t.Fatalf("the Approvals page: %s", approvals)
	}
	// The incident page shows the warned run in short, with its run time,
	// the time left, Stop, which returns there, and Review first, which leads
	// to the run's page.
	incident := html.UnescapeString(responder.get("/incidents/"+sos.IncidentID, 200))
	panel := liveSectionText(incident, "incident-approvals")
	for _, want := range []string{
		"unless someone stops it Runs in 7 minutes", "Run “Reboot app002” on app002?", "The playbook can change app002, in the inventory “production”.", "Reboot a server",
		"The playbook run's rule is “Warn, wait 10 minutes, then act”, so the playbook runs automatically unless someone stops the playbook run.", "Reason for stopping (optional)", "Stop Review first",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("the incident's warned run lacks %q: %q", want, panel)
		}
	}
	for _, want := range []string{`id="approval-` + record.id + `"`, `action="` + requestPage + `/stop"`, `name="return_to_incident_id" value="` + sos.IncidentID + `"`, `href="` + requestPage + `">Review first</a>`} {
		if !strings.Contains(incident, want) {
			t.Errorf("the incident page lacks %q", want)
		}
	}
	if offers(incident, requestPage+"/approve") {
		t.Error("the incident's warned run offers Approve")
	}
	if t.Failed() {
		t.Fatal(incident)
	}

	csrf := workflowField(t, page, "csrf")
	// A form another site sends carries no matching CSRF token.
	responder.post(requestPage+"/stop", url.Values{"csrf": {strings.Repeat("A", 43)}}, 403)
	// Approving would run it before its time; a warned run takes only Stop.
	responder.post(requestPage+"/approve", url.Values{"csrf": {csrf}}, 404)
	if state := onlyWarnedRunRecord(t, r.a).state; state != "open" {
		t.Fatalf("state %s after the refused forms", state)
	}
	if next := postForRedirect(t, responder, requestPage+"/stop", url.Values{"csrf": {csrf}, "reason": {""}, "return_to_incident_id": {sos.IncidentID}}); !strings.HasPrefix(next, "/incidents/"+sos.IncidentID) {
		t.Fatalf("a Stop from the incident page sent the browser to %q", next)
	}
	if record = onlyWarnedRunRecord(t, r.a); record.state != "stopped" || record.stopReason != "" {
		t.Fatalf("record after the page's Stop: %+v", record)
	}
	page = responder.get(requestPage, 200)
	if outcome := liveSectionText(page, "approval-decision"); !strings.Contains(outcome, "Stopped by Responder at 12:03") || liveSectionText(page, "approval-state") != "Stopped" || offers(page, requestPage+"/stop") {
		t.Fatalf("the page after the Stop: %q", outcome)
	}
	incident = html.UnescapeString(responder.get("/incidents/"+sos.IncidentID, 200))
	if happened := liveSectionText(incident, "incident-audit"); !strings.Contains(happened, "You stopped "+rebootPlaybook+" in the inventory “production”.") || offers(incident, requestPage+"/stop") {
		t.Fatalf("the incident page after the Stop: %q", happened)
	}
}

// The card's countdown and the warning's time read plainly, with the date
// when the run time falls on another day.
func TestWarnedRunTimeTexts(t *testing.T) {
	t.Parallel()
	for left, want := range map[time.Duration]string{
		10 * time.Minute: "Runs in 10 minutes", 9*time.Minute + 30*time.Second: "Runs in 10 minutes", 30 * time.Second: "Runs in 1 minute",
		time.Hour: "Runs in 1 hour", 90 * time.Minute: "Runs in 1 hour 30 minutes", 24 * time.Hour: "Runs in 24 hours",
	} {
		if got := runsInText(left); got != want {
			t.Errorf("runsInText(%s) = %q, want %q", left, got, want)
		}
	}
	warned := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if got := warnedRunTimeText(warned.Add(10*time.Minute), warned); got != "12:10 UTC" {
		t.Errorf("the same day: %q", got)
	}
	if got := warnedRunTimeText(warned.Add(24*time.Hour), warned); got != "12:00 UTC on 2026-09-30" {
		t.Errorf("the next day: %q", got)
	}
}
