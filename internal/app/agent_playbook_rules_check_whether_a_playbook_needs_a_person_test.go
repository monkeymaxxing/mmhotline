package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

const (
	checkedRestartPostgres = "one_time_playbooks/2026-11-03_restart_postgres_on_db01.yml"
	checkedRestartCaddy    = "one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml"
	checkedShell           = "one_time_playbooks/2026-11-03_clean_up_with_a_shell_on_app002.yml"
	checkedFreeDiskSpace   = "one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml"
	checkedReboot          = "one_time_playbooks/2026-11-03_reboot_app002.yml"
)

// checkedPlaybooksExplained is what mmx explain prints for the playbooks the
// check-whether-a-playbook-needs-a-person tests use, by MMX's name for each.
func checkedPlaybooksExplained() map[string]string {
	return map[string]string{
		"2026-11-03_restart_postgres_on_db01":  restartExplained("restart_postgres", "db01", "pgautofailover"),
		"2026-11-03_restart_caddy_on_app002":   restartExplained("restart_caddy", "app002", "caddy"),
		"2026-11-03_free_disk_space_on_app002": freeDiskSpaceExplained,
		"2026-11-03_reboot_app002":             rebootExplained,
		"2026-11-03_clean_up_with_a_shell_on_app002": `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"clean_up","selection":"app002","nodes":["app002"],"node_tasks":[
{"path":"clean_up/task_1","node":"app002","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.script","value":"rm -rf /tmp/old","status":"literal"}]}]}]}`,
	}
}

// commitCheckedPlaybooks commits the tests' playbooks to MMHotline's copy and
// origin and returns the commit.
func commitCheckedPlaybooks(t *testing.T, a *App) string {
	t.Helper()
	files := map[string]string{}
	for _, path := range []string{checkedRestartPostgres, checkedRestartCaddy, checkedShell, checkedFreeDiskSpace, checkedReboot} {
		files[path] = "fleet_stages: []\n"
	}
	return commitToCopyAndOrigin(t, a, files)
}

func checkWhetherAPlaybookNeedsAPersonCommand(t *testing.T, a *App, playbook, inventory string, status int) (CommandResult, string) {
	t.Helper()
	return agentPlaybookRulesCommand(t, a, "check-whether-a-playbook-needs-a-person", AgentPlaybookRulesCommandRequest{Playbook: playbook, Inventory: inventory}, status)
}

// checkedPlaybooksRig is a triage rig whose copy holds the checked playbooks,
// with Start or restart a service and Free disk space set to Act, then report
// on production, Unrecognized task set to Warn, wait 15 minutes, then act,
// and pgautofailover on the list of services whose changes a person must approve. So a
// restart of pgautofailover needs a person, a restart of caddy runs without
// one, and a shell task runs unless someone presses Stop.
func checkedPlaybooksRig(t *testing.T) (*triageSessionRig, *fakeRunner, string, string) {
	t.Helper()
	r := newTriageSessionRig(t, true)
	ctx := context.Background()
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 8)}
	r.a.triageAgent.playbookRunner = runner
	mmxCalls := installFakeMMX(t, r.a, checkedPlaybooksExplained())
	fixture := Principal{UnixUser: "fixture"}
	if err := applyAgentPlaybookRulesPreset(ctx, r.a, fixture, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	if err := setAgentPlaybookRule(ctx, r.a, fixture, "production", "unrecognized_task", AgentPlaybookRule{Rule: warnWaitThenAct, WarningWaitMinutes: 15}); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SetServicesWhoseChangesAPersonMustApprove(ctx, fixture, []string{"pgautofailover"}); err != nil {
		t.Fatal(err)
	}
	return r, runner, mmxCalls, commitCheckedPlaybooks(t, r.a)
}

// An administrator sees what the rules for running the agent's playbooks and the list of services
// whose changes a person must approve do to a playbook before any triage agent asks
// for it: with pgautofailover on the list and Start or restart a service set
// to Act, then report, a restart of pgautofailover needs a person and says
// why, and a restart of caddy runs without one; with the list empty both
// run without one. A task MMHotline does not recognize takes the setting of
// Unrecognized task, with its reason. The first line answers the question
// for each of the three settings, and needs_a_person says it in the JSON.
// Checking changes nothing in the database and runs nothing.
func TestCheckWhetherAPlaybookNeedsAPersonShowsWhatTheAgentPlaybookRulesDecide(t *testing.T) {
	r, runner, mmxCalls, commit := checkedPlaybooksRig(t)
	ctx := context.Background()
	fixture := Principal{UnixUser: "fixture"}
	member, _ := addMember(t, r.a, "Member", RoleMember)

	// The database has one connection, so its change count covers every
	// write checking could make.
	changes := func() int64 {
		t.Helper()
		var count int64
		if err := r.a.Store.db.QueryRow(`SELECT total_changes()`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	unchanged := func(before int64) {
		t.Helper()
		if after := changes(); after != before {
			t.Fatalf("checking changed %d rows", after-before)
		}
	}
	changesBefore := changes()
	// The text says nothing of a review agent while none is set up, and
	// never says "judge".
	checkText := func(text string) {
		t.Helper()
		for _, absent := range []string{"review agent", "judg", "Judg"} {
			if strings.Contains(text, absent) {
				t.Errorf("the text has %q:\n%s", absent, text)
			}
		}
	}

	result, raw := checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, checkedRestartPostgres, "production", 200)
	assertCommandKeys(t, raw, "whether_a_playbook_needs_a_person", "playbook", "inventory", "commit_id", "tasks", "node", "module", "action_kind", "action_kind_title", "action_kinds", "needs_a_person", "rule", "rule_title", "approvals_needed", "reason", "review_agent_would_review_the_playbook", "tasks_on_services_whose_changes_a_person_must_approve", "service_name", "unrecognized_tasks", "message")
	if !strings.Contains(raw, `"needs_a_person":true`) {
		t.Fatalf("the restart of pgautofailover: %s", raw)
	}
	answer := result.WhetherAPlaybookNeedsAPerson
	restartPostgres := CommandCheckedPlaybookTask{Path: "restart_postgres/task_1", Node: "db01", Module: "systemd_service", ActionKind: actionkinds.StartOrRestartAService, ActionKindTitle: "Start or restart a service"}
	if answer == nil || answer.Playbook != checkedRestartPostgres || answer.Inventory != "production" || answer.CommitID != commit ||
		!reflect.DeepEqual(answer.Tasks, []CommandCheckedPlaybookTask{restartPostgres}) ||
		len(answer.ActionKinds) != 1 || answer.ActionKinds[0].ActionKind != actionkinds.StartOrRestartAService || answer.ActionKinds[0].AgentPlaybookRule != actingAlone {
		t.Fatalf("the restart of pgautofailover: %s", raw)
	}
	if !answer.NeedsAPerson || answer.ReviewAgentWouldReviewThePlaybook || answer.AgentPlaybookRule != permissionFromOne || answer.RuleTitle != "Get permission, then act (1 approval)" ||
		!reflect.DeepEqual(answer.TasksOnServicesWhoseChangesAPersonMustApprove, []CommandTaskOnAServiceWhoseChangesAPersonMustApprove{{Path: "restart_postgres/task_1", Node: "db01", Module: "systemd_service", ServiceName: "pgautofailover.service", Reason: "restarts pgautofailover.service on db01, which is on the list of services whose changes a person must approve"}}) ||
		!strings.Contains(answer.Reason, "list of services whose changes a person must approve") || len(answer.UnrecognizedTasks) != 0 {
		t.Fatalf("the restart of pgautofailover needs a person because of the list: %s", raw)
	}
	text := result.Text()
	if !strings.HasPrefix(text, "Needs a person: Get permission, then act (1 approval).\nWhy: A node task acts on a service on the list of services whose changes a person must approve") {
		t.Errorf("the first lines:\n%s", text)
	}
	for _, line := range []string{
		"\nChecked " + checkedRestartPostgres + " in the inventory “production” at commit " + commit + " of MMHotline's copy of the systems repository.\n",
		"\n  restart_postgres/task_1 on db01: systemd_service; Start or restart a service\n",
		"\n  Start or restart a service (start_or_restart_a_service): Act, then report (act_then_report); changed by ",
		"\nTasks on services whose changes a person must approve:\n  restart_postgres/task_1 (systemd_service) restarts pgautofailover.service on db01, which is on the list of services whose changes a person must approve\n",
		// What did not happen.
		"\nMMHotline checked the playbook as it checks a triage agent's request to run it, and changed nothing: it ran only mmx explain, and recorded no request, asked for no approval, sent no alert, and ran no playbook.",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("the text lacks %q:\n%s", line, text)
		}
	}
	checkText(text)

	result, raw = checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, checkedRestartCaddy, "production", 200)
	if answer := result.WhetherAPlaybookNeedsAPerson; answer == nil || answer.NeedsAPerson || !strings.Contains(raw, `"needs_a_person":false`) ||
		answer.AgentPlaybookRule != actingAlone || answer.RuleTitle != "Act, then report" || len(answer.TasksOnServicesWhoseChangesAPersonMustApprove) != 0 ||
		answer.Reason != "Every kind of action of the playbook is set to Act, then report on the inventory “production”, so MMHotline runs it without a person." ||
		!strings.HasPrefix(result.Text(), "Runs without a person: Act, then report.\nWhy: Every kind of action") {
		t.Fatalf("the restart of caddy runs without a person: %s\n%s", raw, result.Text())
	}
	checkText(result.Text())

	result, raw = checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, checkedShell, "production", 200)
	if answer := result.WhetherAPlaybookNeedsAPerson; answer == nil || answer.NeedsAPerson || !strings.Contains(raw, `"needs_a_person":false`) ||
		answer.AgentPlaybookRule != (AgentPlaybookRule{Rule: warnWaitThenAct, WarningWaitMinutes: 15}) ||
		!reflect.DeepEqual(answer.UnrecognizedTasks, []CommandUnrecognizedTask{{Path: "clean_up/task_1", Module: "shell", Reason: "no kind covers the module shell"}}) ||
		answer.Reason != "On the inventory “production”, the strictest rule of the playbook's kinds of action is Warn, wait 15 minutes, then act, the rule of Unrecognized task." ||
		!strings.HasPrefix(result.Text(), "Runs without a person unless someone presses Stop: Warn, wait 15 minutes, then act.\nWhy: On the inventory") ||
		!strings.Contains(result.Text(), "\nTasks MMHotline did not recognize, and why:\n  clean_up/task_1 (shell): no kind covers the module shell") {
		t.Fatalf("the unrecognized task: %s\n%s", raw, result.Text())
	}
	checkText(result.Text())

	// Only an administrator checks, as only one changes the rules for running the agent's playbooks.
	if _, err := r.a.CheckWhetherAPlaybookNeedsAPerson(ctx, member, checkedRestartCaddy, "production"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member checked: %v", err)
	}
	// The command's old name is an unknown action.
	oldName, raw := agentPlaybookRulesCommand(t, r.a, "judge-playbook", AgentPlaybookRulesCommandRequest{Playbook: checkedRestartCaddy, Inventory: "production"}, 404)
	if oldName.WhetherAPlaybookNeedsAPerson != nil {
		t.Fatalf("judge-playbook answered: %s", raw)
	}
	unchanged(changesBefore)

	// With the list empty, the rules for running the agent's playbooks alone decide.
	if err := r.a.SetServicesWhoseChangesAPersonMustApprove(ctx, fixture, nil); err != nil {
		t.Fatal(err)
	}
	changesBefore = changes()
	for _, playbook := range []string{checkedRestartPostgres, checkedRestartCaddy} {
		if result, raw := checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, playbook, "production", 200); result.WhetherAPlaybookNeedsAPerson == nil || result.WhetherAPlaybookNeedsAPerson.NeedsAPerson || result.WhetherAPlaybookNeedsAPerson.AgentPlaybookRule != actingAlone || len(result.WhetherAPlaybookNeedsAPerson.TasksOnServicesWhoseChangesAPersonMustApprove) != 0 {
			t.Fatalf("%s with the list empty: %s", playbook, raw)
		}
	}
	unchanged(changesBefore)

	// Nothing was asked, sent, recorded, or run, and mmx ran only to explain.
	for _, table := range []string{"triage_agent_requests", "triage_agent_playbook_runs_waiting_for_a_person", "playbook_run_reviews", "alerts", "incidents"} {
		var count int
		if err := r.a.Store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s has %d rows: %v", table, count, err)
		}
	}
	noRunnerCall(t, runner)
	calls, err := os.ReadFile(mmxCalls)
	if err != nil {
		t.Fatal(err)
	}
	for index, line := range strings.Split(strings.TrimSuffix(string(calls), "\n"), "\n") {
		if index%2 == 1 && !strings.HasPrefix(line, "explain ") {
			t.Errorf("mmx ran %q", line)
		}
	}
}

// With a review agent set up, the answer for a playbook that would run
// without a person, by Act, then report or by Warn, wait, then act, says
// that the review agent would also review it, right after why, and that the
// command does not ask it; the JSON says so in
// review_agent_would_review_the_playbook. A playbook that needs a person is
// never reviewed, so its answer leaves the line out. The review agent is not
// asked.
func TestCheckWhetherAPlaybookNeedsAPersonSaysWhenTheReviewAgentWouldReviewIt(t *testing.T) {
	r, _, _, commit := checkedPlaybooksRig(t)
	const reviewLine = "\nA review agent would also review the playbook before the playbook runs. The check command does not ask the review agent.\n"

	// Without a review agent, no playbook gets the line.
	for _, playbook := range []string{checkedRestartPostgres, checkedRestartCaddy, checkedShell} {
		result, raw := checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, playbook, "production", 200)
		if !strings.Contains(raw, `"review_agent_would_review_the_playbook":false`) || strings.Contains(result.Text(), "review agent") {
			t.Fatalf("%s without a review agent: %s\n%s", playbook, raw, result.Text())
		}
	}

	if _, err := r.a.Store.db.Exec(`INSERT INTO review_agent_setup(id,coding_agent,credential_type,credential,model,reasoning_effort,set_up_at,set_up_by) VALUES(1,'claude_code','api_key','review-key','test-model','high',1,'administrator')`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		playbook, firstLine string
		reviewed            bool
	}{
		{checkedRestartPostgres, "Needs a person: Get permission, then act (1 approval).", false},
		{checkedRestartCaddy, "Runs without a person: Act, then report.", true},
		{checkedShell, "Runs without a person unless someone presses Stop: Warn, wait 15 minutes, then act.", true},
	} {
		result, raw := checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, test.playbook, "production", 200)
		answer, text := result.WhetherAPlaybookNeedsAPerson, result.Text()
		if answer == nil || answer.ReviewAgentWouldReviewThePlaybook != test.reviewed || !strings.Contains(raw, fmt.Sprintf(`"review_agent_would_review_the_playbook":%v`, test.reviewed)) {
			t.Fatalf("%s: %s", test.playbook, raw)
		}
		why := test.firstLine + "\nWhy: " + answer.Reason + "\n"
		if !strings.HasPrefix(text, why) {
			t.Errorf("%s: the first lines:\n%s", test.playbook, text)
		}
		if test.reviewed && !strings.HasPrefix(text, strings.TrimSuffix(why, "\n")+reviewLine+"Checked "+test.playbook+" in the inventory “production” at commit "+commit+" ") ||
			!test.reviewed && strings.Contains(text, "review agent") {
			t.Errorf("%s: the review agent's line:\n%s", test.playbook, text)
		}
	}
	// Checking asked the review agent nothing.
	var runs int
	if err := r.a.Store.db.QueryRow(`SELECT count(*) FROM triage_agent_runs WHERE purpose='playbook_review'`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("%d review runs: %v", runs, err)
	}
}

// One function decides for the command and for the triage agent's request,
// so the command shows exactly what the agent's request then does: the same
// rule, which runs, warns, or waits for a person's approval, and
// the same message for a request MMHotline refuses. needs_a_person is true
// exactly when the agent's request waits for a person's approval.
func TestCheckWhetherAPlaybookNeedsAPersonAgreesWithTheTriageAgentsRequest(t *testing.T) {
	r := newTriageSessionRig(t, true)
	ctx := context.Background()
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 8)}
	r.a.triageAgent.playbookRunner = runner
	installFakeMMX(t, r.a, checkedPlaybooksExplained())
	fixture := Principal{UnixUser: "fixture"}
	if err := applyAgentPlaybookRulesPreset(ctx, r.a, fixture, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	if err := setAgentPlaybookRule(ctx, r.a, fixture, "production", "free_disk_space", AgentPlaybookRule{Rule: warnWaitThenAct, WarningWaitMinutes: 10}); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SetServicesWhoseChangesAPersonMustApprove(ctx, fixture, []string{"pgautofailover"}); err != nil {
		t.Fatal(err)
	}
	commitCheckedPlaybooks(t, r.a)
	r.alert(t, "sos", "The database on db01 stopped answering", "db01")
	r.drive(t)
	run := r.nextRun(t)
	controls := json.NewDecoder(run.connection)
	requestNumber := 0
	ask := func(playbook, inventory string) (string, triageagent.Answer) {
		t.Helper()
		requestNumber++
		requestID := fmt.Sprintf("%016x", requestNumber)
		sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: requestID, Playbook: playbook, Inventory: inventory, Diagnosis: testDiagnosis})
		run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
		var control triageagent.Control
		if err := controls.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != requestID {
			t.Fatalf("answer: %+v %v", control, err)
		}
		return requestID, *control.Answer
	}
	for _, test := range []struct {
		playbook, inventory string
		refused             bool
	}{
		{checkedRestartPostgres, "production", false}, // the list: Get permission, then act
		{checkedRestartCaddy, "production", false},    // Act, then report
		{checkedFreeDiskSpace, "production", false},   // Warn, wait 10 minutes, then act
		{checkedReboot, "production", false},          // Reboot a server: Get permission, then act
		{checkedShell, "production", false},           // Unrecognized task: Get permission, then act
		{checkedReboot, "staging", false},             // no settings: Permission for every fix
		// Refused before MMX reads anything, and by MMX.
		{"roles/web/main.yml", "production", true},
		{"one_time_playbooks/../run_mmx.yml", "production", true},
		{checkedReboot, "production;reboot", true},
		{"one_time_playbooks/2026-11-03_broken_playbook.yml", "production", true},
	} {
		name := test.playbook + " on " + test.inventory
		status := 200
		if test.refused {
			status = 400
		}
		checkedResult, raw := checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, test.playbook, test.inventory, status)
		requestID, answer := ask(test.playbook, test.inventory)
		checked := checkedResult.WhetherAPlaybookNeedsAPerson
		if checked == nil {
			if answer.Outcome != triageagent.Refused || checkedResult.Message != answer.Message {
				t.Errorf("%s: the command refused with %q; the agent's request answered %s: %q", name, checkedResult.Message, answer.Outcome, answer.Message)
			}
			continue
		}
		if checked.Rule == actThenReport {
			if answer.Outcome != triageagent.RanAndSucceeded || checked.NeedsAPerson {
				t.Errorf("%s: checked %s, needs a person %v; the agent's request answered %+v", name, checked.RuleTitle, checked.NeedsAPerson, answer)
			}
			if call := <-runner.calls; call.request.Playbook != test.playbook {
				t.Fatalf("the runner ran %+v", call)
			}
			continue
		}
		var waited AgentPlaybookRule
		if err := r.a.Store.db.QueryRow(`SELECT rule,warning_wait_minutes,approvals_needed FROM triage_agent_playbook_runs_waiting_for_a_person WHERE request_id=?`, requestID).Scan(&waited.Rule, &waited.WarningWaitMinutes, &waited.ApprovalsNeeded); err != nil {
			t.Fatalf("%s: checked %s (%s); the agent's request waits for no person: %+v %v", name, checked.RuleTitle, raw, answer, err)
		}
		if answer.Outcome != triageagent.WaitsForAPerson || waited != checked.AgentPlaybookRule {
			t.Errorf("%s: checked %+v; the agent's request %s with %+v", name, checked.AgentPlaybookRule, answer.Outcome, waited)
		}
		// A warned run waits for nobody's approval; it runs at its run time.
		if needsApproval := waited.Rule == getPermissionThenAct; checked.NeedsAPerson != needsApproval {
			t.Errorf("%s: needs a person %v; the agent's request waits for approval %v: %+v", name, checked.NeedsAPerson, needsApproval, answer)
		}
	}
	noRunnerCall(t, runner)
}
