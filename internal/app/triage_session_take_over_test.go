package app

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

// openTriageSessionID is the one open session's ID.
func openTriageSessionID(t *testing.T, a *App) string {
	t.Helper()
	var id string
	if err := a.Store.db.QueryRow(`SELECT id FROM triage_sessions WHERE ended_at=0`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// triageSessionEndedBecause is why the session ended, or "" while it is open.
func triageSessionEndedBecause(t *testing.T, a *App, id string) string {
	t.Helper()
	var because string
	if err := a.Store.db.QueryRow(`SELECT ended_because FROM triage_sessions WHERE id=?`, id).Scan(&because); err != nil {
		t.Fatal(err)
	}
	return because
}

// readStop reads the next control line of a run and fails unless it is a stop.
func readStop(t *testing.T, run fakeTriageRun, controls *json.Decoder) {
	t.Helper()
	var control triageagent.Control
	run.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := controls.Decode(&control); err != nil || !control.Stop {
		t.Fatalf("the run was not stopped: %+v %v", control, err)
	}
}

// actThenReportReboot lets MMHotline run the reboot playbook on production
// without a person, through a runner that records each run, so a playbook
// MMHotline runs shows.
func actThenReportReboot(t *testing.T, a *App) *fakeRunner {
	t.Helper()
	prepareApprovals(t, a, "")
	if err := setAgentPlaybookRule(context.Background(), a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: actThenReport}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	a.triageAgent.playbookRunner = runner
	return runner
}

// A member presses "Take over from the agent" on the incident page. The
// run stops after the agent's current step, and the page says so; MMHotline
// runs no playbook for the agent from then on. MMHotline then resumes the
// agent once for its hand-over, with no new alert: the agent may report, but
// a playbook it asks for is refused. When the hand-over ends, the session
// ends, and the next alert starts a new one.
func TestTakingOverStopsTheRunAndHandsOver(t *testing.T) {
	r := newTriageSessionRig(t, true)
	runner := actThenReportReboot(t, r.a)
	sos := r.alert(t, "sos", "Database on db001 is down", "db001")
	r.drive(t)
	run := r.nextRun(t)
	controls := json.NewDecoder(run.connection)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	sendTriageEvent(t, run, triageagent.Event{Event: "step", Step: &triageagent.Step{Kind: "command", Text: "pg_lsclusters on db001"}})
	waitForCondition(t, "the step", func() bool {
		return countRows(t, r.a, `SELECT count(*) FROM triage_agent_run_steps WHERE text='pg_lsclusters on db001'`) == 1
	})
	sessionID := openTriageSessionID(t, r.a)
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	responder := (&workflowRig{t: t, server: server}).browser()
	responder.identify(r.p.SignInEmailAddress)
	incidentPage := func() string { return html.UnescapeString(responder.get("/incidents/"+sos.IncidentID, 200)) }
	page := incidentPage()
	if panel := liveSectionText(page, "triage-agent"); !strings.Contains(panel, "The triage agent is looking into the incident now.") || !strings.Contains(panel, "Take over from the agent") {
		t.Errorf("the incident's triage agent panel: %q", panel)
	}
	if steps := liveSectionText(page, "triage-agent-steps"); !strings.Contains(steps, "command pg_lsclusters on db001") {
		t.Errorf("the agent's steps: %q", steps)
	}
	if !strings.Contains(page, `action="/triage-sessions/`+sessionID+`/stop"`) {
		t.Error("the incident page offers no take-over")
	}
	if t.Failed() {
		t.Fatal(page)
	}
	csrf := workflowField(t, page, "csrf")
	// A form another site sends carries no matching CSRF token.
	responder.post("/triage-sessions/"+sessionID+"/stop", url.Values{"csrf": {strings.Repeat("A", 43)}}, 403)
	// A removed member, and the local command, which names no person, are refused.
	ben := addPerson(t, r.a, "ben@example.test", "Ben", RoleMember)
	if err := removePerson(t, r.a, ben.SignInEmailAddress); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{{SignInEmailAddress: ben.SignInEmailAddress}, {UnixUser: "fixture"}} {
		if err := r.a.TakeOverTriageSession(context.Background(), p, sessionID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("a Stop by %+v: %v", p, err)
		}
	}
	if taken := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE taken_over_by_member_id!=''`); taken != 0 {
		t.Fatal("a refused Stop took the session over")
	}
	r.advance(time.Minute)
	stoppedAt := displayTime(r.a.Now())
	responder.post("/triage-sessions/"+sessionID+"/stop", url.Values{"csrf": {csrf}, "return_to_incident_id": {sos.IncidentID}}, 303)
	r.drive(t)
	readStop(t, run, controls)
	page = incidentPage()
	if panel := liveSectionText(page, "triage-agent"); !strings.Contains(panel, "Stopping: the triage agent is finishing the current step.") {
		t.Errorf("after the Stop the triage agent panel says %q", panel)
	}
	if happened := liveSectionText(page, "incident-audit"); !strings.Contains(happened, "You took over from the triage agent.") {
		t.Errorf("after the Stop What happened says %q", happened)
	}
	if strings.Contains(page, "/triage-sessions/"+sessionID+"/stop") {
		t.Error("the page still offers Stop")
	}
	if again := responder.post("/triage-sessions/"+sessionID+"/stop", url.Values{"csrf": {csrf}}, 409); !strings.Contains(again, "the triage agent is already stopping: Responder pressed Stop at "+stoppedAt) {
		t.Errorf("a second Stop: %s", again)
	}
	// The agent's current step is its request: MMHotline runs nothing for it now.
	refused := "MMHotline refused the request: Responder took this triage session over from the triage agent at " + r.a.Now().UTC().Format("15:04") + " UTC, so MMHotline runs no more playbooks in it."
	if answer := requestPlaybookRun(t, run, controls, "0000000000000001"); answer.Outcome != triageagent.Refused || !strings.Contains(answer.Message, refused) {
		t.Fatalf("a request after the Stop: %+v", answer)
	}
	// An alert that arrives now is for the person who took over.
	r.alert(t, "warn", "Disk of db001 is 97% full", "")
	r.end(t, run, triageagent.Event{Event: "stopped", SessionID: "session-one"})
	r.drive(t)
	handOver := r.nextRun(t)
	if handOver.request.Action != "resume" || handOver.request.SessionID != "session-one" {
		t.Fatalf("the hand-over: action %q, session %q", handOver.request.Action, handOver.request.SessionID)
	}
	for _, want := range []string{"for a hand-over", "Responder pressed Stop at " + stoppedAt, "what you found, what you did, and what you were about to do", "mmhotline triage-agent report", "MMHotline refuses every playbook run you ask for in this run"} {
		if !strings.Contains(handOver.request.Prompt, want) {
			t.Errorf("the hand-over prompt lacks %q", want)
		}
	}
	if strings.Contains(handOver.request.Prompt, "Disk of db001 is 97% full") {
		t.Error("the hand-over carries the alert that arrived after the Stop")
	}
	if t.Failed() {
		t.Fatal(handOver.request.Prompt)
	}
	handOverControls := json.NewDecoder(handOver.connection)
	if answer := requestPlaybookRun(t, handOver, handOverControls, "0000000000000001"); answer.Outcome != triageagent.Refused || !strings.Contains(answer.Message, refused) {
		t.Fatalf("a request in the hand-over: %+v", answer)
	}
	noRunnerCall(t, runner)
	if waiting := countRows(t, r.a, `SELECT count(*) FROM triage_agent_playbook_runs_waiting_for_a_person`); waiting != 0 {
		t.Fatalf("%d playbook runs wait for a person after the Stop", waiting)
	}
	if answer := report(t, handOver, handOverControls, "0000000000000002", "info", "# Hand-over: db001's database is down\nI found the cluster stopped and was about to start it.\n"); answer.Outcome != triageagent.ReportRecorded {
		t.Fatalf("the hand-over's report: %+v", answer)
	}
	page = incidentPage()
	for _, want := range []string{"What the triage agent found Handing over I found the cluster stopped and was about to start it.", "Hand-over: db001's database is down", "Handing over: the triage agent writes a report and a note about what the triage agent found, did, and was about to do"} {
		if panel := liveSectionText(page, "triage-agent"); !strings.Contains(panel, want) {
			t.Errorf("during the hand-over the triage agent panel lacks %q: %q", want, panel)
		}
	}
	if t.Failed() {
		t.Fatal(page)
	}
	r.end(t, handOver, triageagent.Event{Event: "finished", Answer: "Handed over."})
	r.drive(t)
	ended := "A person took over: Responder pressed Stop at " + stoppedAt + "."
	if because := triageSessionEndedBecause(t, r.a, sessionID); because != ended {
		t.Fatalf("the session ended because %q", because)
	}
	if page = incidentPage(); !strings.Contains(page, ended) {
		t.Fatalf("the ended session on the page: %s", page)
	}
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.drive(t)
	if next := r.nextRun(t); next.request.Action != "start" || next.request.SessionID != "" {
		t.Fatalf("the next alert must start a new session: action %q, session %q", next.request.Action, next.request.SessionID)
	}
}

// Stop ends the wait of an SOS for the triage agent: the agent no longer
// works on it, so MMHotline wakes the person on call at once. An SOS that
// arrives before the session ends never waits for the agent.
func TestTakingOverWakesAnSOSThatWaitsForTheAgent(t *testing.T) {
	r := newTriageHoldRig(t)
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	r.runStarted(t)
	r.at(t, time.Minute)
	if r.woken(sos.IncidentID) {
		t.Fatal("woken before the Stop")
	}
	barbara := Principal{SignInEmailAddress: r.barbara.SignInEmailAddress}
	if err := r.a.TakeOverTriageSession(context.Background(), barbara, openTriageSessionID(t, r.a)); err != nil {
		t.Fatal(err)
	}
	r.at(t, time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the Stop did not wake the person on call")
	}
	held := "Held for the triage agent until " + r.start.Add(5*time.Minute).UTC().Format("15:04") + " UTC"
	if history := r.history(t, sos.IncidentID); !inOrder(history, held, "The triage agent finishes its current step", "Barbara stopped the triage agent; waking the person on call", "Contacting Responder (on call)") {
		t.Fatalf("history: %q", history)
	}
	later := admitSignedAlert(t, r.a, r.source, "sos", "Disk of db002 is 99% full", "db002-disk")
	r.at(t, 2*time.Minute)
	incident, err := r.a.GetIncident(context.Background(), r.s.Principal, later.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Step == triageAgentStep || !r.woken(later.IncidentID) {
		t.Fatalf("an SOS after the Stop waits for the agent: step %q", incident.Step)
	}
}

// A Stop while no run is going hands over at once. When the agent never
// began, nothing is handed over: the session ends at once.
func TestTakingOverWhileNoRunIsGoing(t *testing.T) {
	t.Run("the agent hands over at once", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		sos := r.alert(t, "sos", "Database on db001 is down", "db001")
		r.drive(t)
		run := r.nextRun(t)
		sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		request := httptest.NewRequest("GET", "https://mmhotline.example.com/incidents/"+sos.IncidentID, nil)
		addAccessToken(request, r.p.SignInEmailAddress)
		response := httptest.NewRecorder()
		r.a.Handler().ServeHTTP(response, request)
		if panel := liveSectionText(response.Body.String(), "triage-agent"); !strings.Contains(panel, "Waiting: the triage agent is not running.") || !strings.Contains(panel, "Take over from the agent") {
			t.Fatalf("the waiting session on the page: %q", panel)
		}
		if err := r.a.TakeOverTriageSession(context.Background(), r.p, openTriageSessionID(t, r.a)); err != nil {
			t.Fatal(err)
		}
		r.drive(t)
		if handOver := r.nextRun(t); handOver.request.Action != "resume" || handOver.request.SessionID != "session-one" || !strings.Contains(handOver.request.Prompt, "for a hand-over") {
			t.Fatalf("the hand-over: action %q, session %q:\n%s", handOver.request.Action, handOver.request.SessionID, handOver.request.Prompt)
		}
	})
	t.Run("a software review gives the hand-over the run", func(t *testing.T) {
		r := newSoftwareReviewRig(t)
		r.alert(t, "sos", "Database on db001 is down", "db001")
		r.drive(t)
		run := r.nextRun(t)
		sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		r.rounds(t)
		review := r.nextReview(t)
		controls := json.NewDecoder(review.connection)
		if err := r.a.TakeOverTriageSession(context.Background(), r.p, openTriageSessionID(t, r.a)); err != nil {
			t.Fatal(err)
		}
		r.rounds(t)
		readStop(t, review, controls)
		r.end(t, review, triageagent.Event{Event: "stopped", SessionID: "review-session"})
		r.rounds(t)
		if handOver := r.nextRun(t); handOver.request.Action != "resume" || handOver.request.SessionID != "session-one" || !strings.Contains(handOver.request.Prompt, "for a hand-over") {
			t.Fatalf("the hand-over: action %q, session %q:\n%s", handOver.request.Action, handOver.request.SessionID, handOver.request.Prompt)
		}
	})
	t.Run("an agent that never began has nothing to hand over", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		r.alert(t, "sos", "Database on db001 is down", "db001")
		r.drive(t)
		r.end(t, r.nextRun(t), triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the credential."})
		sessionID := openTriageSessionID(t, r.a)
		if err := r.a.TakeOverTriageSession(context.Background(), r.p, sessionID); err != nil {
			t.Fatal(err)
		}
		r.drive(t)
		r.noRunStarts(t)
		if because := triageSessionEndedBecause(t, r.a, sessionID); !strings.HasSuffix(because, ", before the triage agent began, so it had nothing to hand over.") {
			t.Fatalf("the session ended because %q", because)
		}
		if err := r.a.TakeOverTriageSession(context.Background(), r.p, sessionID); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "this triage session already ended") {
			t.Fatalf("a Stop of an ended session: %v", err)
		}
	})
}

// A Stop stops the session's warned runs before their run time, so no fix
// of the agent's runs unattended after a person took over, and the hand-over
// tells the agent. Its approval requests stay open: nothing runs without a
// person's approval anyway.
func TestTakingOverStopsWarnedRunsAndKeepsApprovals(t *testing.T) {
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	r.a.triageAgent.playbookRunner = runner
	sos, run, approval := approvalSession(t, r)
	if approval.Outcome != triageagent.WaitsForAPerson {
		t.Fatalf("the first request: %+v", approval)
	}
	if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: warnWaitThenAct, WarningWaitMinutes: 10}); err != nil {
		t.Fatal(err)
	}
	controls := json.NewDecoder(run.connection)
	if warned := requestPlaybookRun(t, run, controls, "0000000000000002"); warned.Outcome != triageagent.WaitsForAPerson {
		t.Fatalf("the second request: %+v", warned)
	}
	if err := r.a.TakeOverTriageSession(context.Background(), r.p, openTriageSessionID(t, r.a)); err != nil {
		t.Fatal(err)
	}
	member, err := r.a.CurrentMember(context.Background(), r.p)
	if err != nil {
		t.Fatal(err)
	}
	warned := onlyWarnedRunRecord(t, r.a)
	if warned.state != "stopped" || warned.stoppedByMemberID != member.ID || warned.stopReason != triageAgentTakeOverStopReason {
		t.Fatalf("the warned run after the Stop: %+v", warned)
	}
	if state := onlyApprovalRecord(t, r.a).state; state != "open" {
		t.Fatalf("the approval request after the Stop: %s", state)
	}
	if history := incidentHistory(t, r.a, r.p, sos.IncidentID); !inOrder(history, "The triage agent finishes its current step", "Stopped "+rebootPlaybook+" in the inventory “production”: "+triageAgentTakeOverStopReason) {
		t.Fatalf("history: %q", history)
	}
	r.drive(t)
	readStop(t, run, controls)
	r.end(t, run, triageagent.Event{Event: "stopped", SessionID: "session-one"})
	r.drive(t)
	handOver := r.nextRun(t)
	if want := "Responder stopped " + rebootPlaybook + " in the inventory “production”, so nothing ran. Responder's reason: " + triageAgentTakeOverStopReason; !strings.Contains(handOver.request.Prompt, want) {
		t.Fatalf("the hand-over prompt lacks %q:\n%s", want, handOver.request.Prompt)
	}
	// Past the warned run's time, nothing runs.
	r.advance(15 * time.Minute)
	runApprovedPlaybooks(t, r.a)
	noRunnerCall(t, runner)
}

// A hand-over that MMHotline's own stop or restart cuts short is resumed, as
// every run so cut is, and the session ends after it.
func TestHandOverAfterARestart(t *testing.T) {
	for _, test := range restartsDuringARun {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			r.alert(t, "sos", "Database on db001 is down", "db001")
			r.drive(t)
			run := r.nextRun(t)
			sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
			waitSessionIDSaved(t, r.a, "session-one")
			sessionID := openTriageSessionID(t, r.a)
			if err := r.a.TakeOverTriageSession(context.Background(), r.p, sessionID); err != nil {
				t.Fatal(err)
			}
			r.drive(t)
			readStop(t, run, json.NewDecoder(run.connection))
			r.end(t, run, triageagent.Event{Event: "stopped", SessionID: "session-one"})
			r.drive(t)
			r.nextRun(t)
			r.a = test.stop(t, r.a)
			r.runs = triageRunFixture(t, r.a)
			r.drive(t)
			resumed := r.nextRun(t)
			if resumed.request.Action != "resume" || resumed.request.SessionID != "session-one" {
				t.Fatalf("after the restart: action %q, session %q", resumed.request.Action, resumed.request.SessionID)
			}
			for _, want := range []string{"for a hand-over", "Your last run ended before the work finished: " + test.reason} {
				if !strings.Contains(resumed.request.Prompt, want) {
					t.Fatalf("the prompt lacks %q:\n%s", want, resumed.request.Prompt)
				}
			}
			if because := triageSessionEndedBecause(t, r.a, sessionID); because != "" {
				t.Fatalf("the session ended at the restart: %q", because)
			}
			r.end(t, resumed, triageagent.Event{Event: "finished", Answer: "Handed over."})
			r.drive(t)
			if because := triageSessionEndedBecause(t, r.a, sessionID); !strings.HasPrefix(because, "A person took over: Responder pressed Stop at ") {
				t.Fatalf("the session ended because %q", because)
			}
		})
	}
}

// A Stop that comes while MMHotline reads the requested playbook, as the
// review agent's minutes would allow, still runs nothing: the decision
// checks the session again.
func TestStopWhileMMHotlineReadsThePlaybook(t *testing.T) {
	r := newTriageSessionRig(t, true)
	runner := actThenReportReboot(t, r.a)
	// mmx explain waits until the test releases it.
	directory := t.TempDir()
	calls, release := filepath.Join(directory, "calls"), filepath.Join(directory, "release")
	explained := filepath.Join(directory, "explained.json")
	if err := os.WriteFile(explained, []byte(rebootExplained), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" + waitForFileInShell(release) + "cat '" + explained + "'\n"
	if err := os.WriteFile(filepath.Join(directory, "mmx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r.a.triageAgent.mmxSearchPath = directory
	r.alert(t, "sos", "app002 stopped answering", "app002")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: rebootPlaybook, Inventory: "production", Diagnosis: testDiagnosis})
	waitForCondition(t, "mmx explain", fileExists(calls))
	if err := r.a.TakeOverTriageSession(context.Background(), r.p, openTriageSessionID(t, r.a)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var control triageagent.Control
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	if err := json.NewDecoder(run.connection).Decode(&control); err != nil || control.Answer == nil || control.Answer.Outcome != triageagent.Refused || !strings.Contains(control.Answer.Message, "Responder took this triage session over from the triage agent") {
		t.Fatalf("answer: %+v %v", control, err)
	}
	noRunnerCall(t, runner)
}

// A person presses Stop while a playbook the agent asked for runs without a
// person. The agent's request gets no answer, as its run is stopping, but
// MMHotline still records how the playbook run went, and the hand-over
// tells the agent, so its note and report say whether the fix ran.
func TestHandOverTellsTheResultOfAPlaybookRunTheStopInterrupted(t *testing.T) {
	r := newTriageSessionRig(t, true)
	actThenReportReboot(t, r.a)
	r.a.triageAgent.playbookRunner = r.a
	directory := t.TempDir()
	started, release := filepath.Join(directory, "started"), filepath.Join(directory, "release")
	installFakeMMXRun(t, r.a, "#!/bin/sh\ntouch '"+started+"'\n"+waitForFileInShell(release)+"echo 'PLAY RECAP app002 ok=1'\nexit 0\n")
	r.alert(t, "sos", "app002 stopped answering", "app002")
	r.drive(t)
	run := r.nextRun(t)
	controls := json.NewDecoder(run.connection)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: rebootPlaybook, Inventory: "production", Diagnosis: testDiagnosis})
	waitForCondition(t, "mmx run", fileExists(started))
	if err := r.a.TakeOverTriageSession(context.Background(), r.p, openTriageSessionID(t, r.a)); err != nil {
		t.Fatal(err)
	}
	r.drive(t)
	readStop(t, run, controls)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The run's relay has stopped listening, so this answer reaches nobody.
	var control triageagent.Control
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	if err := controls.Decode(&control); err != nil || control.Answer == nil || control.Answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("the answer: %+v %v", control, err)
	}
	r.end(t, run, triageagent.Event{Event: "stopped", SessionID: "session-one"})
	r.drive(t)
	handOver := r.nextRun(t)
	for _, want := range []string{"for a hand-over", "MMHotline's record of that request, to run " + playbookInInventoryText(rebootPlaybook, "production"), "MMHotline answered at ", control.Answer.Message, "MMX exited with status 0.", "PLAY RECAP app002 ok=1"} {
		if !strings.Contains(handOver.request.Prompt, want) {
			t.Fatalf("the hand-over prompt lacks %q:\n%s", want, handOver.request.Prompt)
		}
	}
}
