package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

// What mmx explain prints for an ordinary playbook every server runs: a
// shell task and a systemd_service task MMHotline does not recognize. The
// one-time playbook below prints the same.
const everyServerExplained = `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"every_server","selection":"db01","nodes":["db01"],"node_tasks":[
{"path":"every_server/task_1","node":"db01","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.script","value":"./provider/create_server","status":"literal"}]},
{"path":"every_server/task_2","node":"db01","action":"systemd_service","module_kind":"core","status":"ready","arguments":[{"field":"systemd_service.name","value":"pgautofailover","status":"literal"},{"field":"systemd_service.state","value":"restarted","status":"literal"}]}]}]}`

const (
	listedEveryServer  = "playbooks/every_server.yml"
	listedReboot       = "playbooks/reboot_servers_after_updates.yml"
	oneTimeEveryServer = "one_time_playbooks/2026-11-03_every_server_again.yml"
)

// listedPlaybooksRig is a triage rig whose production inventory takes
// Routine fixes without permission and lists every_server with
// Act, then report and reboot_servers_after_updates with Warn, wait 10
// minutes, then act, as the settings file's playbooks under production
// would.
func listedPlaybooksRig(t *testing.T) (*triageSessionRig, *fakeRunner) {
	t.Helper()
	r := newTriageSessionRig(t, true)
	ctx := context.Background()
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 8)}
	r.a.triageAgent.playbookRunner = runner
	installFakeMMX(t, r.a, map[string]string{
		"every_server":                  everyServerExplained,
		"reboot_servers_after_updates":  rebootExplained,
		"2026-11-03_every_server_again": everyServerExplained,
	})
	fixture := Principal{UnixUser: "fixture"}
	if err := applyAgentPlaybookRulesPreset(ctx, r.a, fixture, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Store.db.Exec(`INSERT INTO agent_playbook_rules_for_listed_playbooks(inventory,playbook_name,rule,warning_wait_minutes,approvals_needed,changed_at,changed_by) VALUES
('production','every_server','act_then_report',0,0,1,'the settings file'),('production','reboot_servers_after_updates','warn_wait_then_act',10,0,1,'the settings file')`); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{listedEveryServer: "fleet_stages: []\n", listedReboot: "fleet_stages: []\n", oneTimeEveryServer: "fleet_stages: []\n"})
	return r, runner
}

// A playbook a person listed by name takes the listed rule instead of the
// strictest rule of its kinds: every_server, with tasks MMHotline does not
// recognize, runs without a person, and
// says why, in the check command and in the answer to the agent, while its
// tasks are still named. The same tasks in a one-time playbook, which can
// never be listed, and every_server on an inventory that does not list it,
// still need a person. A listed Warn, wait, then act playbook is a warned
// run whose warning and page say the rule came from the listed playbook.
func TestAListedPlaybookTakesItsListedRule(t *testing.T) {
	parallelStartingProcesses(t)
	r, runner := listedPlaybooksRig(t)
	const everyServerListed = "The playbook every_server is listed in rules_for_running_agent_playbooks for the inventory “production” with Act, then report"

	for _, test := range []struct {
		playbook, inventory, firstLine, why string
	}{
		{listedEveryServer, "production", "Runs without a person: Act, then report.", everyServerListed + ", so the playbook takes the listed rule instead of the rules of its kinds of action.\n"},
		{listedReboot, "production", "Runs without a person unless someone presses Stop: Warn, wait 10 minutes, then act.", "The playbook reboot_servers_after_updates is listed in rules_for_running_agent_playbooks for the inventory “production” with Warn, wait 10 minutes, then act, so the playbook takes the listed rule instead of the rules of its kinds of action.\n"},
		{oneTimeEveryServer, "production", "Needs a person: Get permission, then act (1 approval).", "On the inventory “production”, the strictest rule of the playbook's kinds of action is Get permission, then act (1 approval)"},
		{listedEveryServer, "staging", "Needs a person: Get permission, then act (1 approval).", "On the inventory “staging”, the strictest rule of the playbook's kinds of action is Get permission, then act (1 approval)"},
	} {
		result, raw := checkWhetherAPlaybookNeedsAPersonCommand(t, r.a, test.playbook, test.inventory, 200)
		text := result.Text()
		if !strings.HasPrefix(text, test.firstLine+"\nWhy: "+test.why) {
			t.Errorf("%s on %s:\n%s", test.playbook, test.inventory, text)
		}
		listed := test.inventory == "production" && strings.HasPrefix(test.playbook, "playbooks/")
		if strings.Contains(raw, `"listed_playbook":`) != listed {
			t.Errorf("%s on %s: listed_playbook in %s", test.playbook, test.inventory, raw)
		}
		// The kinds and recognition are shown as for any playbook.
		if test.playbook != listedReboot && !strings.Contains(text, "Tasks MMHotline did not recognize, and why:\n  every_server/task_1 (shell)") {
			t.Errorf("%s on %s: the tasks are not named:\n%s", test.playbook, test.inventory, text)
		}
	}

	r.alert(t, "sos", "The database on db01 stopped answering", "db01")
	r.drive(t)
	run := r.nextRun(t)
	controls := json.NewDecoder(run.connection)
	ask := func(requestID, playbook string) triageagent.Answer {
		t.Helper()
		sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: requestID, Playbook: playbook, Inventory: "production", Diagnosis: testDiagnosis})
		run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
		var control triageagent.Control
		if err := controls.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != requestID {
			t.Fatalf("answer: %+v %v", control, err)
		}
		return *control.Answer
	}

	answer := ask(fmt.Sprintf("%016x", 1), listedEveryServer)
	if answer.Outcome != triageagent.RanAndSucceeded || !strings.Contains(answer.Message, "every_server/task_1 (shell)") || !strings.Contains(answer.Message, everyServerListed+", so MMHotline runs the playbook without a person. MMHotline ran the playbook, and it succeeded.") {
		t.Errorf("every_server: %+v", answer)
	}
	if call := <-runner.calls; call.request.Playbook != listedEveryServer {
		t.Fatalf("the runner ran %+v", call)
	}

	answer = ask(fmt.Sprintf("%016x", 2), listedReboot)
	const rebootListed = "The playbook reboot_servers_after_updates is listed in rules_for_running_agent_playbooks for the inventory “production” with Warn, wait 10 minutes, then act"
	if answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "does: Reboot a server. "+rebootListed+", so the run waits for a person. MMHotline warned the team") {
		t.Errorf("reboot_servers_after_updates: %+v", answer)
	}
	var id string
	var fromListed bool
	if err := r.a.Store.db.QueryRow(`SELECT id,rule_from_listed_playbook FROM triage_agent_playbook_runs_waiting_for_a_person WHERE playbook=? AND rule='warn_wait_then_act'`, listedReboot).Scan(&id, &fromListed); err != nil || !fromListed {
		t.Fatalf("the warned run: %v, rule from the listed playbook %v", err, fromListed)
	}
	view, err := r.a.ApprovalRequest(context.Background(), Principal{UnixUser: "root"}, id)
	if err != nil || view.ListedPlaybookRule != rebootListed+"." || !strings.Contains(commandApprovalRequest(view).Text(), "Rule: Warn, wait 10 minutes, then act. "+rebootListed+". Commit: ") {
		t.Errorf("the warned run's page: %v %q\n%s", err, view.ListedPlaybookRule, commandApprovalRequest(view).Text())
	}
	var warning string
	if err := r.a.Store.db.QueryRow(`SELECT message FROM alerts WHERE message LIKE '%reboot_servers_after_updates is listed%'`).Scan(&warning); err != nil || !strings.Contains(warning, "The rule is Warn, wait 10 minutes, then act. "+rebootListed+".") {
		t.Errorf("the warning: %v %q", err, warning)
	}
	noRunnerCall(t, runner)
}

// The owner's choice, made only in servicesListDecides and asserted only
// here: a listed playbook's rule overrides the list of services whose
// changes a person must approve, and says so; the list decides for a
// playbook nobody listed.
func TestAListedRuleOverridesTheServicesList(t *testing.T) {
	t.Parallel()
	kinds := []ActionKindAgentPlaybookRule{{ActionKind: actionkinds.StartOrRestartAService, AgentPlaybookRule: AgentPlaybookRule{Rule: actThenReport}}}
	onTheList := []taskOnAServiceWhoseChangesAPersonMustApprove{{ServiceName: "pgautofailover.service"}}
	listed := AgentPlaybookRule{Rule: actThenReport}
	judgment := playbookJudgment{kindSettings: kinds, onServicesWhoseChangesAPersonMustApprove: onTheList, listed: &ListedPlaybookAgentPlaybookRule{PlaybookName: "every_server", AgentPlaybookRule: listed}}
	if judgment.setting = playbookRule(kinds, onTheList, &listed); judgment.setting != listed {
		t.Errorf("a listed playbook restarting a listed service takes %+v; want its listed rule", judgment.setting)
	}
	if want := "The playbook every_server is listed in rules_for_running_agent_playbooks for the inventory “production” with Act, then report, so the playbook takes the listed rule instead of the rules of its kinds of action. A listed playbook keeps its listed rule even when a task acts on a service whose changes a person must approve."; judgment.reason("production") != want {
		t.Errorf("why:\n%s\nwant\n%s", judgment.reason("production"), want)
	}
	if got := playbookRule(kinds, onTheList, nil); got != (AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 1}) {
		t.Errorf("a playbook nobody listed restarting a listed service takes %+v", got)
	}
}
