package app

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

const (
	rebootPlaybook     = "one_time_playbooks/2026-11-03_reboot_app002.yml"
	rebootPlaybookText = "# Reboot app002, whose kernel hangs.\nfleet_stages:\n  - id: reboot_app002\n    nodes: app002\n    node_tasks:\n      - reboot: {}\n"
	// rebootApprovalTitle names the one server the reboot playbook changes.
	rebootApprovalTitle = "Approval needed to run " + rebootPlaybook + " in the inventory “production”: it can change app002"
)

// miniRunMMX records only MMX calls; MMHotline owns fetching and locking.
func miniRunMMX(records string) string {
	return "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + records + "/calls'\necho 'PLAY RECAP app002 ok=1 changed=1'\n"
}

// prepareApprovals makes the reboot playbook wait for one approval on
// production: mmx explain's stand-in reads it as Reboot a server, which the
// recommended preset asks a person about. It commits the playbook, and
// installs runMMX outside the copy, and returns the playbook commit.
func prepareApprovals(t *testing.T, a *App, runMMX string) string {
	t.Helper()
	installFakeMMX(t, a, map[string]string{"2026-11-03_reboot_app002": rebootExplained})
	if err := applyAgentPlaybookRulesPreset(context.Background(), a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{rebootPlaybook: rebootPlaybookText}
	if runMMX != "" {
		installFakeMMXRun(t, a, runMMX)
	}
	return commitToCopyAndOrigin(t, a, files)
}

// commitToCopyAndOrigin commits files to MMHotline's copy and pushes them, as
// a person's commits arrive, and returns the commit.
func commitToCopyAndOrigin(t *testing.T, a *App, files map[string]string) string {
	t.Helper()
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	for path, content := range files {
		mode := os.FileMode(0o644)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, path), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, directory, "add", "--all")
	testGit(t, directory, "commit", "--quiet", "-m", "Change the servers")
	testGit(t, directory, "push", "--quiet", "origin", "main")
	return testGit(t, directory, "rev-parse", "HEAD")
}

// pushToOriginOnly pushes a commit to origin that MMHotline's copy has not
// fetched, as a person's push between judging and running, and returns it.
func pushToOriginOnly(t *testing.T, a *App) string {
	t.Helper()
	origin := testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "remote", "get-url", "origin")
	clone := filepath.Join(t.TempDir(), "clone")
	testGit(t, filepath.Dir(clone), "clone", "--quiet", origin, clone)
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("The servers after a new push.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, clone, "add", "README.md")
	testGit(t, clone, "commit", "--quiet", "-m", "Describe the servers")
	testGit(t, clone, "push", "--quiet", "origin", "main")
	return testGit(t, clone, "rev-parse", "HEAD")
}

// requestPlaybookRun sends the agent's request for the reboot playbook on
// production and returns MMHotline's answer.
func requestPlaybookRun(t *testing.T, run fakeTriageRun, answers *json.Decoder, requestID string) triageagent.Answer {
	t.Helper()
	sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: requestID, Playbook: rebootPlaybook, Inventory: "production", Diagnosis: testDiagnosis})
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	var control triageagent.Control
	if err := answers.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != requestID {
		t.Fatalf("answer: %+v %v", control, err)
	}
	return *control.Answer
}

type approvalRecord struct {
	id, commitID, incidentID, state, result, resultForTheAgent, resultSentInRunID string
}

// approvalRecords lists the approval requests, oldest first.
func approvalRecords(t *testing.T, a *App) []approvalRecord {
	t.Helper()
	rows, err := a.Store.db.Query(`SELECT id,commit_id,incident_id,state,result_message,result_for_the_agent,result_sent_in_run_id FROM triage_agent_playbook_runs_waiting_for_a_person WHERE rule='get_permission_then_act' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []approvalRecord
	for rows.Next() {
		var r approvalRecord
		if err := rows.Scan(&r.id, &r.commitID, &r.incidentID, &r.state, &r.result, &r.resultForTheAgent, &r.resultSentInRunID); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

func onlyApprovalRecord(t *testing.T, a *App) approvalRecord {
	t.Helper()
	records := approvalRecords(t, a)
	if len(records) != 1 {
		t.Fatalf("%d approval requests: %+v", len(records), records)
	}
	return records[0]
}

// runApprovedPlaybooks is the background loop's turn for approved runs,
// waiting until the runs it started end.
func runApprovedPlaybooks(t *testing.T, a *App) {
	t.Helper()
	if err := a.DriveApprovedPlaybookRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.triageAgent.approvedPlaybookRuns.Wait()
}

func noRunnerCall(t *testing.T, runner *fakeRunner) {
	t.Helper()
	select {
	case call := <-runner.calls:
		t.Fatalf("the playbook ran: %+v", call)
	default:
	}
}

// approvalSession starts a triage session with an SOS, has the agent report
// its coding agent session and ask for the reboot playbook, and returns the
// SOS and the session's run, still active.
func approvalSession(t *testing.T, r *triageSessionRig) (Alert, fakeTriageRun, triageagent.Answer) {
	t.Helper()
	sos := r.alert(t, "sos", "app002 stopped answering", "app002")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	answer := requestPlaybookRun(t, run, json.NewDecoder(run.connection), "0000000000000001")
	return sos, run, answer
}

// A playbook that waits with Get permission, then act makes MMHotline ask
// for approval itself, at sos: the alert joins the session's open incident,
// whose escalation wakes the person on call at once, and it starts no
// session. Without an open incident, it opens one of its own, which never
// waits for the triage agent.
func TestApprovalNeededAsksAtSOS(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("it joins the session's incident and wakes the person on call", func(t *testing.T) {
		r := newTriageHoldRig(t)
		judged := prepareApprovals(t, r.a, "")
		sos := r.sosArrives(t, "app002 stopped answering", "app002")
		run := r.runStarted(t)
		r.at(t, time.Minute)
		answer := requestPlaybookRun(t, run, json.NewDecoder(run.connection), "0000000000000001")
		if answer.Outcome != triageagent.WaitsForAPerson || !strings.HasSuffix(answer.Message, "so the run waits for a person."+approvalAskedText(true)) {
			t.Fatalf("answer: %+v", answer)
		}
		r.at(t, time.Minute)
		var alertID, incidentID string
		if err := r.a.Store.db.QueryRow(`SELECT id,incident_id FROM alerts WHERE severity='sos' AND title=?`, rebootApprovalTitle).Scan(&alertID, &incidentID); err != nil {
			t.Fatal(err)
		}
		if incidentID != sos.IncidentID {
			t.Fatalf("the approval alert went with incident %q, not the session's %q", incidentID, sos.IncidentID)
		}
		// A minute after the SOS, well before its 5 minutes: the ask woke them.
		if !r.woken(sos.IncidentID) {
			t.Fatal("the approval request did not wake the person on call")
		}
		held := "Held for the triage agent until " + r.start.Add(5*time.Minute).UTC().Format("15:04") + " UTC"
		if history := r.history(t, sos.IncidentID); !inOrder(history, held, "Approval needed to run "+rebootPlaybook+" in the inventory “production”, at commit "+judged+": it can change app002. The playbook run needs approval from 1 person.", "MMHotline asked for approval of a playbook run; waking the person on call", "Contacting Responder (on call)") {
			t.Fatalf("history: %q", history)
		}
		if sessions, joined := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`), countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=?`, alertID); sessions != 1 || joined != 0 {
			t.Fatalf("%d sessions, approval alert joined %d times", sessions, joined)
		}
		// The run ends; MMHotline's own alert carries nothing to the agent.
		sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		waitNoTriageRun(t, r.a)
		backgroundRounds(t, r.a)
		select {
		case started := <-r.runs:
			t.Fatalf("a run started for the approval alert: %s", started.request.Prompt)
		default:
		}
	})
	t.Run("without an open incident it opens one that does not wait", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareApprovals(t, r.a, "")
		r.alert(t, "warn", "app002 answers slowly", "")
		r.drive(t)
		run := r.nextRun(t)
		requestPlaybookRun(t, run, json.NewDecoder(run.connection), "0000000000000001")
		record := onlyApprovalRecord(t, r.a)
		incident, err := r.a.GetIncident(context.Background(), r.p, record.incidentID)
		if err != nil {
			t.Fatal(err)
		}
		if incident.Title != rebootApprovalTitle || incident.Step == triageAgentStep {
			t.Fatalf("incident: %+v", incident)
		}
		if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND contact_method_type='test_waking'`, incident.ID); count == 0 {
			t.Fatal("the approval's incident does not wake the person on call")
		}
		if sessions, sent := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`), countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=? AND sent_in_run_id=?`, incident.OpeningAlertID, lastRunID(t, r.a)); sessions != 1 || sent != 1 {
			t.Fatalf("%d sessions; the approval's alert must join the session as already sent (%d)", sessions, sent)
		}
	})
}

// A task MMHotline does not recognize makes the run wait, and the agent's
// answer, the approval alert, the incident's history, and the card each say
// why, so the agent can write the task in a form MMHotline recognizes and the
// person who approves sees what is unusual. Here a restart also enables the
// service at boot, as an agent wrote it on a real server.
func TestApprovalSaysWhyATaskIsUnrecognized(t *testing.T) {
	parallelStartingProcesses(t)
	const playbook = "one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml"
	const why = "The task MMHotline did not recognize, and why: restart_caddy/task_1 (systemd_service): systemd_service.start_at_boot is not an allowed argument for Start or restart a service (name, state, restart_now, run_only_once_marker_file, in_systemd_of_user_account)."
	r := newTriageSessionRig(t, true)
	installFakeMMX(t, r.a, map[string]string{"2026-11-03_restart_caddy_on_app002": `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"restart_caddy","selection":"app002","nodes":["app002"],"node_tasks":[
{"path":"restart_caddy/task_1","node":"app002","action":"systemd_service","module_kind":"core","status":"ready","arguments":[
 {"field":"systemd_service.name","value":"caddy","status":"resolved"},
 {"field":"systemd_service.restart_now","value":true,"status":"literal"},
 {"field":"systemd_service.run_only_once_marker_file","value":"/var/lib/mmx/commands_run_once/2026-11-03_restart_caddy","status":"resolved"},
 {"field":"systemd_service.start_at_boot","value":true,"status":"literal"},
 {"field":"systemd_service.state","value":"running","status":"literal"}]}]}]}`})
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{playbook: "fleet_stages: []\n"})
	sos := r.alert(t, "sos", "Caddy on app002 stopped answering", "app002")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: playbook, Inventory: "production", Diagnosis: testDiagnosis})
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	var control triageagent.Control
	if err := json.NewDecoder(run.connection).Decode(&control); err != nil || control.Answer == nil {
		t.Fatalf("answer: %+v %v", control, err)
	}
	if answer := control.Answer; answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "does: Unrecognized task. "+why+" ") {
		t.Errorf("the answer to the agent: %+v", answer)
	}
	var message string
	if err := r.a.Store.db.QueryRow(`SELECT message FROM alerts WHERE severity='sos' AND title LIKE 'Approval needed%'`).Scan(&message); err != nil || !strings.Contains(message, "1 of the playbook's tasks does (the task uses systemd_service)") || strings.Contains(message, "restart_caddy/task_1") {
		t.Errorf("the approval alert's message: %q %v", message, err)
	}
	if history := incidentHistory(t, r.a, r.p, sos.IncidentID); !slices.ContainsFunc(history, func(entry string) bool {
		return strings.HasPrefix(entry, "Approval needed to run "+playbook) && strings.Contains(entry, "1 of the playbook's tasks does (the task uses systemd_service)") && !strings.Contains(entry, "restart_caddy/task_1")
	}) {
		t.Errorf("history: %q", history)
	}
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	browser := (&workflowRig{t: t, server: server}).browser()
	browser.identify(r.p.SignInEmailAddress)
	// The request's page warns of the step, with why, where it says why a person decides.
	if text := approvalPageText(browser.get("/approvals/"+onlyApprovalRecord(t, r.a).id, 200)); !phrasesInOrder(text, "Steps MMHotline did not recognize", "MMHotline cannot check what the unrecognized steps do, so a person decides.",
		"restart_caddy/task_1 (systemd_service): systemd_service.start_at_boot is not an allowed argument for Start or restart a service (name, state, restart_now, run_only_once_marker_file, in_systemd_of_user_account)", "Kinds of change Unrecognized task") {
		t.Errorf("the approval request's page: %s", text)
	}
	// The incident page says it in short and links the request's page.
	page := html.UnescapeString(browser.get("/incidents/"+sos.IncidentID, 200))
	if panel := liveSectionText(page, "incident-approvals"); !strings.Contains(panel, "MMHotline can't check what 1 task does.") || !strings.Contains(page, `href="/approvals/`+onlyApprovalRecord(t, r.a).id+`">Review first</a>`) {
		t.Errorf("the incident's approval: %q", panel)
	}
	// The command shows what the card shows.
	var result CommandResult
	if err := r.a.approvalRequestsCommand(context.Background(), r.p, "show", ApprovalRequestsCommandRequest{ApprovalRequestID: onlyApprovalRecord(t, r.a).id}, &result); err != nil || result.ApprovalRequest == nil {
		t.Fatalf("approval-requests show: %+v %v", result, err)
	}
	reason := "systemd_service.start_at_boot is not an allowed argument for Start or restart a service (name, state, restart_now, run_only_once_marker_file, in_systemd_of_user_account)"
	if shown := result.ApprovalRequest; !slices.Equal(shown.UnrecognizedTasks, []CommandUnrecognizedTask{{Path: "restart_caddy/task_1", Module: "systemd_service", Reason: reason}}) ||
		!strings.Contains(shown.Text(), "Tasks MMHotline did not recognize, and why:\n  restart_caddy/task_1 (systemd_service): "+reason+"\n") || !strings.Contains(shown.Text(), playbook+" in the inventory “production”. Waiting for approval.") {
		t.Errorf("approval-requests show: %+v\n%s", shown.UnrecognizedTasks, shown.Text())
	}
}

// One approval runs the playbook with MMX at the commit MMHotline
// judged, and the result resumes the agent's session. With 2 approvals
// needed, one approval runs nothing, the same person's second counts
// nothing, and a second person's runs it.
func TestApprovedPlaybookRuns(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("one approval", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		records := t.TempDir()
		judged := prepareApprovals(t, r.a, fakeRunMMX(records, "echo 'PLAY RECAP app002 ok=1 changed=1'\n"))
		_, run, _ := approvalSession(t, r)
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		r.drive(t)
		r.noRunStarts(t)
		record := onlyApprovalRecord(t, r.a)
		if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
			t.Fatal(err)
		}
		runApprovedPlaybooks(t, r.a)
		if arguments := readTestFile(t, filepath.Join(records, "arguments")); arguments != mmxRunArguments(r.a, "2026-11-03_reboot_app002")+"\n" {
			t.Fatalf("MMX's arguments: %q", arguments)
		}
		want := "Responder approved " + rebootPlaybook + " in the inventory “production”. The run used commit " + judged + ", the one MMHotline checked. MMHotline ran the playbook with MMX, and the run succeeded.\n\nPlaybook run output:\nPLAY RECAP app002 ok=1 changed=1\n"
		if record = onlyApprovalRecord(t, r.a); record.state != "ran_and_succeeded" || record.result != want || record.resultForTheAgent != "waits_for_the_next_run" {
			t.Fatalf("record: %+v", record)
		}
		if history := incidentHistory(t, r.a, r.p, record.incidentID); !inOrder(history, "Approved "+rebootPlaybook+" in the inventory “production” (1 of 1 approvals). MMHotline starts the run.", want) {
			t.Fatalf("history: %q", history)
		}
		r.drive(t)
		resume := r.nextRun(t)
		if resume.request.Action != "resume" || resume.request.SessionID != "session-one" || !strings.Contains(resume.request.Prompt, "with the result of a playbook run you asked for, which waited for a person.") || !strings.Contains(resume.request.Prompt, "Result 1 of 1:\nRecorded at ") || !strings.Contains(resume.request.Prompt, " UTC.\n"+want) || !strings.Contains(resume.request.Prompt, "Times, all in UTC: it is now ") || !strings.Contains(resume.request.Prompt, "- run 1: started ") {
			t.Fatalf("resume: action %q, session %q:\n%s", resume.request.Action, resume.request.SessionID, resume.request.Prompt)
		}
		if record = onlyApprovalRecord(t, r.a); record.resultForTheAgent != "sent" || record.resultSentInRunID != lastRunID(t, r.a) {
			t.Fatalf("record after the resume: %+v", record)
		}
	})
	t.Run("two approvals from two people", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareApprovals(t, r.a, "")
		runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		r.a.triageAgent.playbookRunner = runner
		if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 2}); err != nil {
			t.Fatal(err)
		}
		approvalSession(t, r)
		record := onlyApprovalRecord(t, r.a)
		if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
			t.Fatal(err)
		}
		runApprovedPlaybooks(t, r.a)
		noRunnerCall(t, runner)
		if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "you already approved the playbook run") {
			t.Fatalf("the same person's second approval: %v", err)
		}
		runApprovedPlaybooks(t, r.a)
		noRunnerCall(t, runner)
		if state := onlyApprovalRecord(t, r.a).state; state != "open" {
			t.Fatalf("after one person's approvals: %s", state)
		}
		ben := addPerson(t, r.a, "ben@example.test", "Ben", RoleMember)
		if err := r.a.ApprovePlaybookRun(context.Background(), Principal{SignInEmailAddress: ben.SignInEmailAddress}, record.id); err != nil {
			t.Fatal(err)
		}
		runApprovedPlaybooks(t, r.a)
		select {
		case call := <-runner.calls:
			if call.commitID != record.commitID || call.request.Playbook != rebootPlaybook {
				t.Fatalf("runner's call: %+v", call)
			}
		default:
			t.Fatal("two people's approvals did not run it")
		}
		if result := onlyApprovalRecord(t, r.a).result; !strings.HasPrefix(result, "Responder and Ben approved "+rebootPlaybook+" in the inventory “production”.") {
			t.Fatalf("result: %q", result)
		}
	})
}

// One refusal ends the request with its reason: nothing runs, nobody can
// approve it any more, and the reason resumes the agent's session.
func TestRefusedPlaybookRun(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	r.a.triageAgent.playbookRunner = runner
	_, run, _ := approvalSession(t, r)
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	record := onlyApprovalRecord(t, r.a)
	if err := r.a.RefusePlaybookRun(context.Background(), r.p, record.id, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a refusal without a reason: %v", err)
	}
	const reason = "Reboot after the nightly backup finishes."
	if err := r.a.RefusePlaybookRun(context.Background(), r.p, record.id, reason); err != nil {
		t.Fatal(err)
	}
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving a refused request: %v", err)
	}
	runApprovedPlaybooks(t, r.a)
	noRunnerCall(t, runner)
	want := "Responder refused " + rebootPlaybook + " in the inventory “production”, so nothing ran. Responder's reason: " + reason
	if record = onlyApprovalRecord(t, r.a); record.state != "refused" || record.result != want {
		t.Fatalf("record: %+v", record)
	}
	if count := countRows(t, r.a, `SELECT count(*) FROM playbook_run_approval_decisions WHERE approval_request_id=? AND decision='refuse' AND reason=?`, record.id, reason); count != 1 {
		t.Fatal("the refusal's reason was not recorded")
	}
	if history := incidentHistory(t, r.a, r.p, record.incidentID); !inOrder(history, "Refused "+rebootPlaybook+" in the inventory “production”: "+reason) {
		t.Fatalf("history: %q", history)
	}
	r.drive(t)
	if resume := r.nextRun(t); resume.request.Action != "resume" || !strings.Contains(resume.request.Prompt, want) {
		t.Fatalf("resume: %s", resume.request.Prompt)
	}
}

// When MMHotline's fetch moved the copy past the judged commit, nothing ran and
// the approval is stale: MMHotline judges the playbook again at the new
// commit and asks for approval again, or runs it when the rules for running the agent's playbooks now
// let it run without a person.
func TestStaleApproval(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name       string
		actWithout bool
	}{{"it still needs permission", false}, {"the new judgment lets it run", true}} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			records := t.TempDir()
			judged := prepareApprovals(t, r.a, miniRunMMX(records))
			approvalSession(t, r)
			record := onlyApprovalRecord(t, r.a)
			if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
				t.Fatal(err)
			}
			newer := pushToOriginOnly(t, r.a)
			if test.actWithout {
				if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: actThenReport}); err != nil {
					t.Fatal(err)
				}
			}
			runApprovedPlaybooks(t, r.a)
			records2 := approvalRecords(t, r.a)
			stale := records2[0]
			movedPast := "Other commits reached MMHotline's copy of the systems repository before the run, so nothing ran at commit " + judged + ". The approval does not cover the newer commits: the newer commits change README.md, outside the triage agent's notes. MMHotline checked the playbook again at the newer commit: "
			if stale.state != "stale" || !strings.Contains(stale.result, movedPast) {
				t.Fatalf("stale record: %+v", stale)
			}
			calls := readMMXCalls(t, records)
			if !test.actWithout {
				if len(records2) != 2 || records2[1].state != "open" || records2[1].commitID != newer || stale.resultForTheAgent != "none" || !strings.HasSuffix(stale.result, "MMHotline asked for approval again.") {
					t.Fatalf("records: %+v", records2)
				}
				if asked := countRows(t, r.a, `SELECT count(*) FROM alerts WHERE title=?`, rebootApprovalTitle); asked != 2 {
					t.Fatalf("%d approval alerts; the new commit needs its own approval", asked)
				}
				if len(calls) != 0 {
					t.Fatalf("MMX calls: %q", calls)
				}
				return
			}
			if len(records2) != 1 || stale.resultForTheAgent != "waits_for_the_next_run" || !strings.Contains(stale.result, "the rules for running the agent's playbooks let MMHotline run the playbook without a person. MMHotline ran the playbook with MMX, and the run succeeded.") || !strings.Contains(stale.result, "PLAY RECAP app002 ok=1 changed=1") {
				t.Fatalf("records: %+v", records2)
			}
			if len(calls) != 1 || calls[0] != mmxRunArguments(r.a, "2026-11-03_reboot_app002") {
				t.Fatalf("MMX calls: %q", calls)
			}
		})
	}
}

// An approval whose run had not started when MMHotline restarted runs once
// after the start; a run that had started is never run again, and its record
// says why.
func TestApprovedRunAcrossARestart(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("approved, not started", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareApprovals(t, r.a, "")
		_, run, _ := approvalSession(t, r)
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		record := onlyApprovalRecord(t, r.a)
		if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
			t.Fatal(err)
		}
		restarted := restartApp(t, r.a)
		if err := restarted.InitializeTriageAgent(context.Background(), "", "", ""); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		restarted.triageAgent.playbookRunner = runner
		runApprovedPlaybooks(t, restarted)
		runApprovedPlaybooks(t, restarted)
		if len(runner.calls) != 1 {
			t.Fatalf("%d runs after the restart; want exactly one", len(runner.calls))
		}
		if state := onlyApprovalRecord(t, restarted).state; state != "ran_and_succeeded" {
			t.Fatalf("state %s", state)
		}
	})
	t.Run("started", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareApprovals(t, r.a, "")
		started, release := make(chan struct{}), make(chan struct{})
		r.a.triageAgent.playbookRunner = blockingRunner{started, release}
		_, run, _ := approvalSession(t, r)
		r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		record := onlyApprovalRecord(t, r.a)
		if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
			t.Fatal(err)
		}
		if err := r.a.DriveApprovedPlaybookRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		<-started
		previous := r.a
		// The process ended during the run; MMX ended with it.
		t.Cleanup(func() { close(release); previous.triageAgent.approvedPlaybookRuns.Wait() })
		restarted := restartApp(t, r.a)
		if err := restarted.InitializeTriageAgent(context.Background(), "", "", ""); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		restarted.triageAgent.playbookRunner = runner
		runApprovedPlaybooks(t, restarted)
		noRunnerCall(t, runner)
		record = onlyApprovalRecord(t, restarted)
		if record.state != "ran_and_failed" || !strings.Contains(record.result, "MMHotline restarted while the run was going, so the playbook may have stopped partway or not started. MMHotline does not run the playbook again") {
			t.Fatalf("record: %+v", record)
		}
	})
}

// blockingRunner stands in for an MMX run that goes on until released.
type blockingRunner struct{ started, release chan struct{} }

func (runner blockingRunner) RunPlaybook(context.Context, TriageAgentPlaybookRunRequest, string) TriageAgentPlaybookRunDecision {
	close(runner.started)
	<-runner.release
	return TriageAgentPlaybookRunDecision{Outcome: triageagent.RanAndSucceeded, Message: "MMHotline ran the playbook with MMX, and the run succeeded."}
}

// An approval request whose session ended stays open; deciding it still
// runs it, and its incident records that the result could not reach the
// agent. While an approved run goes, its session stays open, and the quiet
// time counts from the result.
func TestApprovalAfterItsSessionEnded(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	r.a.triageAgent.playbookRunner = runner
	sos, run, _ := approvalSession(t, r)
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	incident, err := r.a.GetIncident(context.Background(), r.p, sos.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ResolveIncident(context.Background(), r.p, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	open := func() int { return countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`) }
	// Approved 40 minutes after the last alert: the session stays open while
	// the approved run goes, and 30 quiet minutes count from its result.
	r.advance(40 * time.Minute)
	record := onlyApprovalRecord(t, r.a)
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
		t.Fatal(err)
	}
	r.drive(t)
	if open() != 1 {
		t.Fatal("the session ended while an approved run waits")
	}
	runApprovedPlaybooks(t, r.a)
	r.drive(t)
	resume := r.nextRun(t)
	if !strings.Contains(resume.request.Prompt, "Responder approved "+rebootPlaybook) {
		t.Fatalf("resume:\n%s", resume.request.Prompt)
	}
	r.end(t, resume, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.advance(29 * time.Minute)
	r.drive(t)
	if open() != 1 {
		t.Fatal("the session ended within 30 minutes of the result")
	}
	r.advance(2 * time.Minute)
	r.drive(t)
	if open() != 0 {
		t.Fatal("the session did not end 30 quiet minutes after the result")
	}

	// A new request in a new session, which then ends before anyone decides.
	r.alert(t, "sos", "app002 stopped answering again", "app002-again")
	r.drive(t)
	run = r.nextRun(t)
	requestPlaybookRun(t, run, json.NewDecoder(run.connection), "0000000000000001")
	if _, err := r.a.Store.db.Exec(`DELETE FROM triage_agent_setup`); err != nil {
		t.Fatal(err)
	}
	r.drive(t)
	if open() != 0 {
		t.Fatal("the session did not end without a setup")
	}
	records := approvalRecords(t, r.a)
	last := records[len(records)-1]
	if last.state != "open" {
		t.Fatalf("the request of an ended session: %+v", last)
	}
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, last.id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	records = approvalRecords(t, r.a)
	if last = records[len(records)-1]; last.state != "ran_and_succeeded" || last.resultForTheAgent != "could_not_reach_the_agent" {
		t.Fatalf("record: %+v", last)
	}
	if history := incidentHistory(t, r.a, r.p, last.incidentID); !inOrder(history, "Responder approved "+rebootPlaybook, "could not reach the triage agent: the playbook request's triage session had ended") {
		t.Fatalf("history: %q", history)
	}
}

// approvalPageText is what a person reads in a page's main part: its text
// without tags, in one line.
func approvalPageText(page string) string {
	if start := strings.Index(page, "<main"); start >= 0 {
		page = page[start:]
	}
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(page, " ")
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}

// phrasesInOrder says whether text holds each phrase, one after another.
func phrasesInOrder(text string, phrases ...string) bool {
	for _, phrase := range phrases {
		index := strings.Index(text, phrase)
		if index < 0 {
			return false
		}
		text = text[index+len(phrase):]
	}
	return true
}

// offers says whether the page has a form that posts to action, such as
// /approvals/ID/approve: a decision the person can make there.
func offers(page, action string) bool {
	return strings.Contains(page, `action="`+action+`"`)
}

// approvalBrowsers returns a function that signs a browser in to server.
func approvalBrowsers(t *testing.T, server *httptest.Server) func(signInEmailAddress string) *workflowBrowser {
	return func(signInEmailAddress string) *workflowBrowser {
		b := (&workflowRig{t: t, server: server}).browser()
		if signInEmailAddress != "" {
			b.identify(signInEmailAddress)
		}
		return b
	}
}

// postForRedirect posts a page's form and returns where MMHotline sends the
// browser next.
func postForRedirect(t *testing.T, b *workflowBrowser, path string, form url.Values) string {
	t.Helper()
	response, err := b.client.PostForm(b.h.server.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s: %d, want 303", path, response.StatusCode)
	}
	return response.Header.Get("Location")
}

// An approval request's own page asks its question, says what an approval
// does, and offers Approve and Refuse while it waits, with what the person
// decides on: why the triage agent asks, each step, the rule, the
// review, and, folded, the playbook's text at the commit MMHotline checked,
// all in plain words. The incident page shows the request in short, with
// Approve and run and Review first, and the Approvals list leads to it. A
// forged form, a removed member, a person who is not a member, and a person
// who is not signed in are refused. After a decision the person lands back
// on the page, which shows what happened and offers no decision again.
func TestApprovalPages(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	judged := prepareApprovals(t, r.a, "")
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	sos, _, _ := approvalSession(t, r)
	record := onlyApprovalRecord(t, r.a)
	// A review that refused a playbook is shown with the request it moved to a person.
	if _, err := r.a.Store.db.Exec(`INSERT INTO playbook_run_reviews(run_id,request_id,coding_agent,credential_type,commit_id,outcome,reason,started_at,ended_at) SELECT run_id,request_id,'claude_code','api_key',commit_id,'refused','It reboots during business hours.',1,2 FROM triage_agent_playbook_runs_waiting_for_a_person WHERE id=?`, record.id); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	browser := approvalBrowsers(t, server)
	responder := browser(r.p.SignInEmailAddress)
	requestPage := "/approvals/" + record.id
	page := responder.get(requestPage, 200)
	text := approvalPageText(page)
	// The answer first: the question, then what an approval does, and where.
	if !phrasesInOrder(text, "Waiting for 1 approval", "Run “Reboot app002” on app002?", "The triage agent asked at 12:00, just now, for the SOS app002 stopped answering",
		"MMHotline runs the playbook as soon as one person approves.", "The playbook can change app002, in the inventory “production”.", "The result appears on the playbook run page and on the incident.",
		"Approve and run", "Refuse", "The triage agent gets your reason") {
		t.Errorf("the page's question and decision: %s", text)
	}
	for _, want := range []string{
		// Why the agent asks: its own summary, marked as its claims.
		"Why the triage agent asks", "df shows / at 97%; journalctl --disk-usage shows 9 GiB. Vacuuming the journal to 500 MiB frees the disk.", "MMHotline has not checked the triage agent's claims.",
		// Each step, with its kind, server, and module.
		"What the playbook changes, step by step", "Reboot a server app002 reboot_app002/task_1 · reboot, a module of kind core",
		// Why it needs a person, and the review that did not approve it.
		"Kinds of change Reboot a server Rule Get permission, then act (1 approval)", "The review agent did not approve the playbook.",
		"The review agent refused the playbook: It reboots during business hours.",
		"The playbook's text, and any files it copies, at commit " + judged[:7], "Commit MMHotline checked " + judged, "Times are in UTC.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if !strings.Contains(html.UnescapeString(page), rebootPlaybookText) {
		t.Error("the page lacks the playbook's text")
	}
	if !offers(page, requestPage+"/approve") || !offers(page, requestPage+"/refuse") || offers(page, requestPage+"/stop") {
		t.Error("a waiting request's page offers no Approve and Refuse, or offers Stop")
	}
	// The person who decides reads plain words, not the design's terms.
	for _, term := range []string{"judged", "node task"} {
		if strings.Contains(strings.ToLower(text), term) {
			t.Errorf("the page says %q", term)
		}
	}
	// The Content Security Policy would drop an inline style.
	if style := regexp.MustCompile(`<style|\sstyle\s*=`).FindString(page); style != "" {
		t.Errorf("the page has an inline style: %s", style)
	}
	if t.Failed() {
		t.Fatal(page)
	}
	// The incident page shows the request in short: what runs, where, what
	// it does, and why it waits for a person, with Approve and run, which
	// returns there, and Review first, which leads to the request's page.
	incident := html.UnescapeString(responder.get("/incidents/"+sos.IncidentID, 200))
	panel := liveSectionText(incident, "incident-approvals")
	for _, want := range []string{
		"Approval needed to run a playbook Needs 1 person", "Run “Reboot app002” on app002?", "The playbook can change app002, in the inventory “production”.", "Reboot a server",
		"The playbook run waits for a person because the rule is “Get permission, then act (1 approval)”.", "The review agent refused the playbook: It reboots during business hours.", "Approve and run Review first",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("the incident's approval lacks %q: %q", want, panel)
		}
	}
	for _, want := range []string{`id="approval-` + record.id + `"`, `action="/approvals/` + record.id + `/approve"`, `name="return_to_incident_id" value="` + sos.IncidentID + `"`, `href="` + requestPage + `">Review first</a>`} {
		if !strings.Contains(incident, want) {
			t.Errorf("the incident page lacks %q", want)
		}
	}
	for _, term := range []string{"judged", "node task"} {
		if strings.Contains(strings.ToLower(panel), term) {
			t.Errorf("the incident's approval says %q", term)
		}
	}
	if t.Failed() {
		t.Fatal(incident)
	}
	// The Approvals list leads to it; the menu counts it.
	if approvals := responder.get("/approvals", 200); !strings.Contains(liveSectionText(approvals, "open-approvals"), "Run “Reboot app002” on app002?") ||
		!strings.Contains(approvals, `href="`+requestPage+`"`) || menuLinkText(approvals, "/approvals") != "Approvals 1" {
		t.Fatalf("the Approvals page, whose menu counts the request that waits: %s", approvals)
	}
	// A person who is not a member, or not signed in, never sees it; an
	// unknown request is not found.
	browser("stranger@example.test").get(requestPage, 403)
	browser("").get(requestPage, 401)
	responder.get("/approvals/0123456789abcdef0123456789abcdef", 404)

	ben := addPerson(t, r.a, "ben@example.test", "Ben", RoleMember)
	other := browser(ben.SignInEmailAddress)
	csrf := workflowField(t, other.get(requestPage, 200), "csrf")
	// A form another site sends carries no matching CSRF token.
	other.post(requestPage+"/approve", url.Values{"csrf": {strings.Repeat("A", 43)}}, 403)
	if err := removePerson(t, r.a, ben.SignInEmailAddress); err != nil {
		t.Fatal(err)
	}
	if refused := other.post(requestPage+"/approve", url.Values{"csrf": {csrf}}, 403); !strings.Contains(refused, "Not a team member") {
		t.Fatalf("a removed member's approval: %s", refused)
	}
	// The operation checks membership in its own transaction too.
	if err := r.a.ApprovePlaybookRun(context.Background(), Principal{SignInEmailAddress: ben.SignInEmailAddress}, record.id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a removed member's approval through the operation: %v", err)
	}
	if decisions := countRows(t, r.a, `SELECT count(*) FROM playbook_run_approval_decisions`); decisions != 0 || onlyApprovalRecord(t, r.a).state != "open" {
		t.Fatalf("a refused form decided: %d decisions", decisions)
	}

	// A refusal without a reason shows the request's page again, with why.
	responderCSRF := workflowField(t, page, "csrf")
	if refused := responder.post(requestPage+"/refuse", url.Values{"csrf": {responderCSRF}, "reason": {""}}, 400); !strings.Contains(refused, "Give a short reason for refusing") || !offers(refused, requestPage+"/approve") {
		t.Fatalf("a refusal without a reason: %s", refused)
	}
	// After the approval, the person lands back on the request's page.
	if next := postForRedirect(t, responder, requestPage+"/approve", url.Values{"csrf": {responderCSRF}}); next != requestPage {
		t.Fatalf("the approval sent the browser to %q, not back to the request", next)
	}
	if state := onlyApprovalRecord(t, r.a).state; state != "approved" {
		t.Fatalf("state %s after the page's approval", state)
	}
	page = responder.get(requestPage, 200)
	if outcome := liveSectionText(page, "approval-decision"); !phrasesInOrder(outcome, "Approved; MMHotline starts the run next", "Approved by Responder at 12:00", "The result appears on the playbook run page when the run ends.") {
		t.Fatalf("the page after the approval: %q", outcome)
	}
	if offers(page, requestPage+"/approve") || offers(page, requestPage+"/refuse") {
		t.Fatal("the page offers a decision on an approved request")
	}
	// The incident's history says who approved it, and its page offers no
	// decision on it again.
	incident = responder.get("/incidents/"+sos.IncidentID, 200)
	if happened := liveSectionText(incident, "incident-audit"); !strings.Contains(happened, "You approved "+rebootPlaybook+" in the inventory “production” (1 of 1 approvals). MMHotline starts the run.") || offers(incident, requestPage+"/approve") {
		t.Fatalf("the incident page after the approval: %q", happened)
	}
	if link := menuLinkText(page, "/approvals"); link != "Approvals" {
		t.Fatalf("the menu still counts the approved request: %q", link)
	}
	// Once the run ends, the page shows its result, quietly, with no decision.
	runApprovedPlaybooks(t, r.a)
	page = responder.get(requestPage, 200)
	if outcome := liveSectionText(page, "approval-decision"); !strings.Contains(outcome, "Ran and succeeded") || !strings.Contains(liveSectionText(page, "approval-state"), "Ran and succeeded") {
		t.Fatalf("the page after the run: %q", outcome)
	}
}

// With 2 approvals needed, the page says how many more it needs, and never
// offers Approve again to the person who approved; another member still
// may, and anyone may still refuse.
func TestApprovalPageWithTwoApprovalsNeeded(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 2}); err != nil {
		t.Fatal(err)
	}
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	approvalSession(t, r)
	record := onlyApprovalRecord(t, r.a)
	addPerson(t, r.a, "ben@example.test", "Ben", RoleMember)
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	browser := approvalBrowsers(t, server)
	responder, ben := browser(r.p.SignInEmailAddress), browser("ben@example.test")
	requestPage := "/approvals/" + record.id
	page := responder.get(requestPage, 200)
	if text := approvalPageText(page); !phrasesInOrder(text, "Waiting for 2 approvals", "MMHotline runs the playbook once two different people approve.") {
		t.Fatalf("the page before any approval: %s", text)
	}
	responder.post(requestPage+"/approve", url.Values{"csrf": {workflowField(t, page, "csrf")}}, 303)
	page = responder.get(requestPage, 200)
	if text := approvalPageText(page); !phrasesInOrder(text, "Waiting for 1 more approval", "You approved the playbook run. MMHotline runs the playbook as soon as another person approves.") {
		t.Fatalf("the page of the person who approved: %s", text)
	}
	if offers(page, requestPage+"/approve") || !offers(page, requestPage+"/refuse") {
		t.Fatal("the person who approved is offered Approve again, or no Refuse")
	}
	page = ben.get(requestPage, 200)
	if text := approvalPageText(page); !phrasesInOrder(text, "Waiting for 1 more approval", "Responder approved the playbook run. MMHotline runs the playbook as soon as one more person approves.") || !offers(page, requestPage+"/approve") {
		t.Fatalf("another member's page: %s", text)
	}
	ben.post(requestPage+"/approve", url.Values{"csrf": {workflowField(t, page, "csrf")}}, 303)
	if state := onlyApprovalRecord(t, r.a).state; state != "approved" {
		t.Fatalf("state %s after two approvals", state)
	}
}

// With 2 approvals needed, an approval stops counting once its member is
// removed from the team: after Ben approves and is removed, Responder's
// approval leaves the request waiting, the page says Ben's approval no longer
// counts, and the run starts only when Cara, a current member, approves.
func TestRemovedMembersApprovalNoLongerCounts(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	fixture := Principal{UnixUser: "fixture"}
	if err := setAgentPlaybookRule(context.Background(), r.a, fixture, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 2}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	r.a.triageAgent.playbookRunner = runner
	sos, _, _ := approvalSession(t, r)
	record := onlyApprovalRecord(t, r.a)
	for _, person := range [][2]string{{"ben@example.test", "Ben"}, {"cara@example.test", "Cara"}} {
		addPerson(t, r.a, person[0], person[1], RoleMember)
	}
	approve := func(signInEmailAddress string) {
		t.Helper()
		if err := r.a.ApprovePlaybookRun(context.Background(), Principal{SignInEmailAddress: signInEmailAddress}, record.id); err != nil {
			t.Fatal(err)
		}
	}
	approve("ben@example.test")
	if err := removePerson(t, r.a, "ben@example.test"); err != nil {
		t.Fatal(err)
	}
	approve(r.p.SignInEmailAddress)
	runApprovedPlaybooks(t, r.a)
	noRunnerCall(t, runner)
	if state := onlyApprovalRecord(t, r.a).state; state != "open" {
		t.Fatalf("state %s after a removed member's and one current member's approvals", state)
	}
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	cara := approvalBrowsers(t, server)("cara@example.test")
	requestPage := "/approvals/" + record.id
	page := cara.get(requestPage, 200)
	if text := approvalPageText(page); !phrasesInOrder(text, "Waiting for 1 more approval", "Ben's approval no longer counts, because Ben was removed from the team. Responder approved the playbook run. MMHotline runs the playbook as soon as one more person approves.") || !offers(page, requestPage+"/approve") {
		t.Fatalf("the page after the removal: %s", text)
	}
	if happened := liveSectionText(cara.get("/incidents/"+sos.IncidentID, 200), "incident-audit"); !strings.Contains(happened, "Responder approved "+rebootPlaybook+" in the inventory “production” (1 of 2 approvals). Ben's approval no longer counts, because Ben was removed from the team.") {
		t.Fatalf("the incident's history: %q", happened)
	}
	approve("cara@example.test")
	if state := onlyApprovalRecord(t, r.a).state; state != "approved" {
		t.Fatalf("state %s after a second current member's approval", state)
	}
	runApprovedPlaybooks(t, r.a)
	select {
	case <-runner.calls:
	default:
		t.Fatal("the playbook did not run after two current members approved")
	}
}

// The Approvals page lists what waits for a person first, then the warned
// runs, the one that runs next first, then, quiet, what was decided or run
// lately; each leads to its own page.
func TestApprovalsListOrder(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	approvalSession(t, r)
	waiting := onlyApprovalRecord(t, r.a)
	// More playbook runs of the same request, in the other states the page
	// lists: two warned runs, the later asked running sooner, and a refused
	// request.
	for _, row := range []struct {
		id, playbook, setting, state    string
		waitMinutes, judgedSecondsLater int
	}{
		{"warned_late", "one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml", warnWaitThenAct, approvalOpen, 30, 0},
		{"warned_soon", "one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml", warnWaitThenAct, approvalOpen, 5, 60},
		{"refused_one", "playbooks/rotate_logs.yml", getPermissionThenAct, approvalRefused, 0, 0},
	} {
		if _, err := r.a.Store.db.Exec(`INSERT INTO triage_agent_playbook_runs_waiting_for_a_person(id,run_id,request_id,triage_session_id,playbook,inventory,commit_id,action_kinds,playbook_text,mmx_explain_output,rule,warning_wait_minutes,approvals_needed,judged_at,incident_id,state,state_changed_at)
			SELECT ?,run_id,request_id,triage_session_id,?,inventory,commit_id,action_kinds,playbook_text,mmx_explain_output,?,?,1,judged_at+?,incident_id,?,state_changed_at FROM triage_agent_playbook_runs_waiting_for_a_person WHERE id=?`,
			row.id, row.playbook, row.setting, row.waitMinutes, row.judgedSecondsLater, row.state, waiting.id); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	page := approvalBrowsers(t, server)(r.p.SignInEmailAddress).get("/approvals", 200)
	if text := approvalPageText(page); !phrasesInOrder(text,
		"Waiting for a person", "Run “Reboot app002” on app002?", "Review",
		"Runs unless someone presses Stop", "Run “Restart caddy on app002” on app002?", "Runs at 12:06, in 6 minutes", "Stop", "Run “Free disk space on app002” on app002?", "Runs at 12:30, in 30 minutes", "Stop",
		"Decided or run lately", "Run “Rotate logs” on app002?", "Refused") {
		t.Fatalf("the Approvals page's order: %s", text)
	}
	// No definitions at the top: the page opens with what waits.
	if text := approvalPageText(page); !strings.HasPrefix(text, "Approvals Waiting for a person") {
		t.Fatalf("the page does not open with what waits: %s", text)
	}
	for _, id := range []string{waiting.id, "warned_late", "warned_soon", "refused_one"} {
		if !strings.Contains(page, `href="/approvals/`+id+`"`) {
			t.Errorf("the list does not lead to %s", id)
		}
	}
	// The warned runs' Stop is one press; a waiting request's decision is on its page.
	if !offers(page, "/approvals/warned_soon/stop") || offers(page, "/approvals/"+waiting.id+"/approve") || offers(page, "/approvals/refused_one/refuse") {
		t.Fatal("the list's forms")
	}
}

// The titles of an approval request and a warned run name the servers the
// playbook changes, from the mmx explain output saved with it: an
// inventory's name does not tell a person which servers change. Each
// server is named once, in order; past three the title counts the rest. A
// task MMX knows it skips changes nothing.
func TestServersItChanges(t *testing.T) {
	t.Parallel()
	// explained is mmx explain's output with one node task per "node status" pair.
	explained := func(tasks ...string) string {
		var listed []string
		for index, task := range tasks {
			node, status, _ := strings.Cut(task, " ")
			listed = append(listed, `{"path":"fix/task_`+strconv.Itoa(index+1)+`","node":"`+node+`","action":"reboot","module_kind":"core","status":"`+status+`"}`)
		}
		return `{"schema_version":2,"fleet_stages":[{"node_tasks":[` + strings.Join(listed, ",") + `]}]}`
	}
	for _, test := range []struct {
		name  string
		tasks []string
		want  string
	}{
		{"one server with two tasks", []string{"app002 ready", "app002 conditional"}, ": it can change app002"},
		{"two servers", []string{"app002 ready", "app001 ready"}, ": it can change app001 and app002"},
		{"three servers and a skipped one", []string{"db001 ready", "app002 ready", "db002 skipped", "app001 conditional", "app002 ready"}, ": it can change app001, app002, and db001"},
		{"more than three servers", []string{"app007 ready", "app006 ready", "app005 ready", "app004 ready", "app003 ready", "app002 ready", "app001 ready"}, ": it can change app001, app002, app003, and 4 more"},
		{"every task skipped", []string{"app001 skipped"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := approvalRequest{MMXExplainOutput: explained(test.tasks...)}
			if got := r.serversItChanges(); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
