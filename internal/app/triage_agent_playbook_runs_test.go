package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

// What mmx explain prints for the sample one-offs, as MMX's
// internal/config/inspect.go writes it.
const (
	freeDiskSpaceExplained = `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"free_disk_space","selection":"app002","nodes":["app002"],"node_tasks":[
{"path":"free_disk_space/task_1","node":"app002","action":"systemd_journal_vacuum","module_kind":"core","status":"ready","arguments":[{"field":"systemd_journal_vacuum.keep_at_most_size","value":"500MiB","status":"literal"}]}]}]}`
	rebootExplained = `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"reboot_app002","selection":"app002","nodes":["app002"],"node_tasks":[
{"path":"reboot_app002/task_1","node":"app002","action":"reboot","module_kind":"core","status":"ready"}]}]}`
	freeDiskSpaceAndRebootExplained = `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"free_disk_space_and_reboot","selection":"app002","nodes":["app002"],"node_tasks":[
{"path":"free_disk_space_and_reboot/task_1","node":"app002","action":"systemd_journal_vacuum","module_kind":"core","status":"ready","arguments":[{"field":"systemd_journal_vacuum.keep_at_most_size","value":"500MiB","status":"literal"}]},
{"path":"free_disk_space_and_reboot/task_2","node":"app002","action":"reboot","module_kind":"core","status":"ready"}]}]}`
	brokenPlaybookMessage = "mmx: fleet stage fix: one_time_playbooks/2026-11-03_broken_playbook.yml:5:9 (fleet_stages[0].node_tasks[0]): unknown module systemd_servise"
)

// installFakeMMX puts a stand-in for mmx on the App's search path, where
// /usr/local/bin would be. It prints the recorded explain output for each
// playbook name, fails as MMX fails for any other, and logs the directory it
// ran in and its arguments.
func installFakeMMX(t *testing.T, a *App, explained map[string]string) string {
	t.Helper()
	directory := t.TempDir()
	log := filepath.Join(directory, "calls")
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then exec '" + filepath.Join(a.Store.Dir, "fake_mmx_run") + "' \"$@\"; fi\nprintf '%s\\n' \"$(pwd -P)\" \"$*\" >> '" + log + "'\ncase \"$2\" in\n"
	for name, output := range explained {
		file := filepath.Join(directory, name+".json")
		if err := os.WriteFile(file, []byte(output), 0o644); err != nil {
			t.Fatal(err)
		}
		script += name + ") cat '" + file + "' ;;\n"
	}
	script += "*) echo '" + brokenPlaybookMessage + "' >&2; exit 4 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(directory, "mmx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a.triageAgent.mmxSearchPath = filepath.Join(directory, "not_here") + ":" + directory
	return log
}

type fakeRunnerCall struct {
	request  TriageAgentPlaybookRunRequest
	commitID string
}

// fakeRunner stands in for MMHotline's MMX runs.
type fakeRunner struct{ calls chan fakeRunnerCall }

func (runner *fakeRunner) RunPlaybook(_ context.Context, request TriageAgentPlaybookRunRequest, commitID string) TriageAgentPlaybookRunDecision {
	runner.calls <- fakeRunnerCall{request, commitID}
	return TriageAgentPlaybookRunDecision{Outcome: triageagent.RanAndSucceeded, Message: "MMHotline ran the playbook, and it succeeded.", PlaybookRunOutput: "PLAY RECAP app002 ok=1\n"}
}

type waitingPlaybookRun struct {
	requestID, playbook, inventory, commitID, actionKinds, agentPlaybookRule string
	warningWaitMinutes, approvalsNeeded                                      int
}

// The agent asks for several playbooks in one run. MMHotline reads each
// one's kinds with mmx explain in its copy, at the commit it pushed, and
// applies the inventory's rules for running the agent's playbooks: Act, then report reaches the
// runner; a stricter setting waits for a person and is recorded; a mixed
// playbook takes the stricter setting; an inventory with no settings needs
// permission; a playbook MMX cannot read is refused with MMX's message.
func TestTriageAgentPlaybookRunDecisions(t *testing.T) {
	a := foundationApp(t)
	// Each approval request wakes the person on call.
	memberFixture(t, a, "responder@example.test")
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 8)}
	a.triageAgent.playbookRunner = runner
	mmxCalls := installFakeMMX(t, a, map[string]string{
		"2026-11-03_free_disk_space_on_app002":         freeDiskSpaceExplained,
		"2026-11-03_reboot_app002":                     rebootExplained,
		"2026-11-03_free_disk_space_and_reboot_app002": freeDiskSpaceAndRebootExplained,
	})
	if err := applyAgentPlaybookRulesPreset(context.Background(), a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	run := prepareSystemsRepositoryRun(t, a, `/bin/mkdir -p one_time_playbooks
for name in 2026-11-03_free_disk_space_on_app002 2026-11-03_reboot_app002 2026-11-03_free_disk_space_and_reboot_app002 2026-11-03_broken_playbook; do
	printf 'fleet_stages: []\n' > one_time_playbooks/$name.yml
done
git add --all
git commit --quiet -m 'Fix app002'
ask() {
	mmhotline triage-agent request-playbook-run --playbook "$2" --inventory "$3" --diagnosis-file "$HOME/diagnosis.md" > "$HOME/$1-answer" 2>&1; echo $? > "$HOME/$1-exit"
}
ask free one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml production
ask reboot one_time_playbooks/2026-11-03_reboot_app002.yml production
ask mixed one_time_playbooks/2026-11-03_free_disk_space_and_reboot_app002.yml production
ask staging one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml staging
ask broken one_time_playbooks/2026-11-03_broken_playbook.yml production
ask role roles/web/main.yml production
`+codexFinishes)
	run.start(t)
	run.wait(t)
	pushed := testGit(t, run.origin, "rev-parse", "main")
	const pushedPrefix = "MMHotline pushed your commits to origin. "

	answer, exit := run.commandResult(t, "free")
	want := pushedPrefix + "one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml in the inventory “production” does: Free disk space. For the inventory “production”, the rules for running the agent's playbooks let MMHotline run the playbook without a person. MMHotline ran the playbook, and it succeeded.\n\nPlaybook run output:\nPLAY RECAP app002 ok=1\n"
	if answer != want || exit != "0" {
		t.Errorf("free disk space: exit %s\n%q\nwant\n%q", exit, answer, want)
	}
	select {
	case call := <-runner.calls:
		if call.request.RunID != run.id || call.request.Playbook != "one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml" || call.request.Inventory != "production" || call.commitID != pushed {
			t.Errorf("runner's call: %+v; want the pushed commit %s", call, pushed)
		}
	default:
		t.Error("the runner was not asked to run the free disk space playbook")
	}
	select {
	case call := <-runner.calls:
		t.Errorf("the runner was asked a second time: %+v", call)
	default:
	}

	for _, test := range []struct{ name, exit, says string }{
		{"reboot", "3", "one_time_playbooks/2026-11-03_reboot_app002.yml in the inventory “production” does: Reboot a server. For the inventory “production”, the strictest rule of the playbook's kinds of action is Get permission, then act (1 approval), so the run waits for a person." + approvalAskedText(true)},
		{"mixed", "3", "one_time_playbooks/2026-11-03_free_disk_space_and_reboot_app002.yml in the inventory “production” does: Free disk space, Reboot a server. For the inventory “production”, the strictest rule of the playbook's kinds of action is Get permission, then act (1 approval), so the run waits for a person." + approvalAskedText(true)},
		{"staging", "3", "one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml in the inventory “staging” does: Free disk space. For the inventory “staging”, the strictest rule of the playbook's kinds of action is Get permission, then act (1 approval), so the run waits for a person." + approvalAskedText(true)},
		{"broken", "4", "MMHotline cannot tell what one_time_playbooks/2026-11-03_broken_playbook.yml in the inventory “production” does: mmx explain failed: " + brokenPlaybookMessage + "."},
		{"role", "4", "MMX runs a playbook only from playbooks/NAME.yml or one_time_playbooks/YYYY-MM-DD_what_it_does.yml, so MMHotline cannot run roles/web/main.yml."},
	} {
		if answer, exit := run.commandResult(t, test.name); answer != pushedPrefix+test.says || exit != test.exit {
			t.Errorf("%s: exit %s\n%q\nwant exit %s\n%q", test.name, exit, answer, test.exit, pushedPrefix+test.says)
		}
	}

	waiting := map[string]waitingPlaybookRun{}
	rows, err := a.Store.db.Query(`SELECT w.request_id,w.playbook,w.inventory,w.commit_id,w.action_kinds,w.rule,w.warning_wait_minutes,w.approvals_needed,r.outcome,r.playbook_read_at_commit_id FROM triage_agent_playbook_runs_waiting_for_a_person w JOIN triage_agent_requests r USING(run_id,request_id) WHERE w.run_id=?`, run.id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var row waitingPlaybookRun
		var outcome, requestReadAt string
		if err := rows.Scan(&row.requestID, &row.playbook, &row.inventory, &row.commitID, &row.actionKinds, &row.agentPlaybookRule, &row.warningWaitMinutes, &row.approvalsNeeded, &outcome, &requestReadAt); err != nil {
			t.Fatal(err)
		}
		if outcome != "waits_for_a_person" {
			t.Errorf("request %s waits, but its answer was %s", row.requestID, outcome)
		}
		// Every request shows the commit it was judged at, whether it ran or waits.
		if requestReadAt != row.commitID {
			t.Errorf("request %s was judged at %s, but its record says %q", row.requestID, row.commitID, requestReadAt)
		}
		row.requestID = ""
		waiting[row.playbook+" on "+row.inventory] = row
	}
	permission := func(playbook, inventory, kinds string) waitingPlaybookRun {
		return waitingPlaybookRun{playbook: playbook, inventory: inventory, commitID: pushed, actionKinds: kinds, agentPlaybookRule: "get_permission_then_act", approvalsNeeded: 1}
	}
	wantWaiting := map[string]waitingPlaybookRun{
		"one_time_playbooks/2026-11-03_reboot_app002.yml on production":                     permission("one_time_playbooks/2026-11-03_reboot_app002.yml", "production", "reboot_a_server"),
		"one_time_playbooks/2026-11-03_free_disk_space_and_reboot_app002.yml on production": permission("one_time_playbooks/2026-11-03_free_disk_space_and_reboot_app002.yml", "production", "free_disk_space,reboot_a_server"),
		"one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml on staging":            permission("one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml", "staging", "free_disk_space"),
	}
	if !reflect.DeepEqual(waiting, wantWaiting) {
		t.Errorf("recorded runs waiting for a person:\n%+v\nwant\n%+v", waiting, wantWaiting)
	}

	// mmx ran in MMHotline's copy, once for each playbook it could name.
	calls, err := os.ReadFile(mmxCalls)
	if err != nil {
		t.Fatal(err)
	}
	copyDirectory, err := filepath.EvalSymlinks(filepath.Join(a.Store.Dir, "systems_repository"))
	if err != nil {
		t.Fatal(err)
	}
	var wantCalls []string
	for _, call := range []string{"2026-11-03_free_disk_space_on_app002 --inventory production", "2026-11-03_reboot_app002 --inventory production", "2026-11-03_free_disk_space_and_reboot_app002 --inventory production", "2026-11-03_free_disk_space_on_app002 --inventory staging", "2026-11-03_broken_playbook --inventory production"} {
		wantCalls = append(wantCalls, copyDirectory, "explain "+call+" --json")
	}
	if got := strings.Split(strings.TrimSuffix(string(calls), "\n"), "\n"); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("mmx calls:\n%q\nwant\n%q", got, wantCalls)
	}
}

// A fleet stage's the fleet stage writes a file on MMHotline's
// server, the controller machine, such as a Git hook in MMHotline's copy that
// runs at its next commit. So a playbook whose only node task is show, which
// is Check only, waits for a person when it saves a file there, and the
// answer names the file.
func TestPlaybookThatSavesOnTheControllerMachineWaitsForAPerson(t *testing.T) {
	const playbook = "one_time_playbooks/2026-11-03_check_caddy_on_app002.yml"
	r := newTriageSessionRig(t, true)
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 1)}
	r.a.triageAgent.playbookRunner = runner
	// What MMX's mmx explain printed for this playbook, cut to the fields
	// MMHotline reads.
	installFakeMMX(t, r.a, map[string]string{"2026-11-03_check_caddy_on_app002": `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"check_caddy","selection":"app002","nodes":["app002"],"node_tasks":[
{"path":"check_caddy/task_1","node":"app002","action":"show","status":"ready","arguments":[{"field":"show","value":"Caddy answers again.","status":"resolved"}]}],
"save_on_controller_machine":[{"template":"one_time_playbooks/hook.tmpl","template_file":"/var/lib/mmhotline/systems_repository/one_time_playbooks/hook.tmpl","path":".git/hooks/pre-commit","files":[{"file":"/var/lib/mmhotline/systems_repository/.git/hooks/pre-commit"}],"permissions_mode":"0755",
 "source":{"file":"one_time_playbooks/2026-11-03_check_caddy_on_app002.yml","line":7,"column":9,"field":"fleet_stages[0].save_on_controller_machine[0]"}}]}]}`})
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{playbook: "fleet_stages: []\n"})
	r.alert(t, "sos", "Caddy on app002 stopped answering", "app002")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: playbook, Inventory: "production", Diagnosis: testDiagnosis})
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	var control triageagent.Control
	if err := json.NewDecoder(run.connection).Decode(&control); err != nil || control.Answer == nil {
		t.Fatalf("answer: %+v %v", control, err)
	}
	select {
	case call := <-runner.calls:
		t.Fatalf("the playbook ran without a person: %+v", call)
	default:
	}
	if answer := control.Answer; answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, playbook+" in the inventory “production” does: Check only, Unrecognized task. The task MMHotline did not recognize, and why: fleet stage check_caddy (save_on_controller_machine): the fleet stage writes /var/lib/mmhotline/systems_repository/.git/hooks/pre-commit on MMHotline's server, with permissions_mode 0755.") {
		t.Fatalf("answer: %+v", answer)
	}
}

// The agent controls a playbook's size and, through the playbook, how much
// mmx explain prints. MMHotline reads at most 1 MiB of a playbook and 64 MiB
// of mmx's output, and refuses a playbook past either limit, with the limit,
// rather than run it or keep it. Both playbooks here would otherwise run
// without a person: each reads as the free disk space playbook.
func TestOversizedPlaybookOrExplainOutputIsRefused(t *testing.T) {
	const (
		longOutput    = "one_time_playbooks/2026-11-03_long_explain_output.yml"
		largePlaybook = "one_time_playbooks/2026-11-03_large_playbook.yml"
	)
	r := newTriageSessionRig(t, true)
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 2)}
	r.a.triageAgent.playbookRunner = runner
	mmxCalls := installFakeMMX(t, r.a, map[string]string{
		// Valid JSON after 64 MiB of spaces.
		"2026-11-03_long_explain_output": strings.Repeat(" ", 64<<20) + freeDiskSpaceExplained,
		"2026-11-03_large_playbook":      freeDiskSpaceExplained,
	})
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{
		longOutput:    "fleet_stages: []\n",
		largePlaybook: "fleet_stages: []\n" + strings.Repeat("#", 1<<20) + "\n",
	})
	for _, test := range []struct{ playbook, says string }{
		{longOutput, "MMHotline cannot tell what " + longOutput + " in the inventory “production” does: mmx explain printed more than 64 MiB, the most MMHotline reads; a smaller playbook, or one for fewer servers, prints less."},
		{largePlaybook, "MMHotline cannot tell what " + largePlaybook + " in the inventory “production” does: the playbook is 1025 KiB, more than the 1024 KiB MMHotline reads of a playbook; split the playbook into smaller playbooks."},
	} {
		decision := r.a.DecidePlaybookRun(context.Background(), TriageAgentPlaybookRunRequest{RunID: "run", RequestID: "0123456789abcdef", Playbook: test.playbook, Inventory: "production"})
		if decision.Outcome != triageagent.Refused || decision.Message != test.says {
			t.Errorf("%s: %+v\nwant the refusal %q", test.playbook, decision, test.says)
		}
	}
	noRunnerCall(t, runner)
	if waiting := countRows(t, r.a, `SELECT count(*) FROM triage_agent_playbook_runs_waiting_for_a_person`); waiting != 0 {
		t.Errorf("%d playbook runs kept", waiting)
	}
	// MMHotline refuses the large playbook before mmx reads it.
	if calls, err := os.ReadFile(mmxCalls); err != nil || strings.Contains(string(calls), "large_playbook") {
		t.Errorf("mmx calls: %q %v", calls, err)
	}
}

// Without an mmx program, MMHotline cannot tell what a playbook does, so it
// runs nothing.
func TestTriageAgentPlaybookRunWithoutMMX(t *testing.T) {
	a := foundationApp(t)
	a.triageAgent.mmxSearchPath = t.TempDir()
	decision := a.DecidePlaybookRun(context.Background(), TriageAgentPlaybookRunRequest{RunID: "run", RequestID: "0123456789abcdef", Playbook: "playbooks/deploy.yml", Inventory: "production"})
	if decision.Outcome != triageagent.Refused || decision.Message != "MMHotline cannot tell what playbooks/deploy.yml in the inventory “production” does: MMHotline finds no mmx program in "+a.triageAgent.mmxSearchPath+"." {
		t.Fatalf("%+v", decision)
	}
	var waiting int
	if err := a.Store.db.QueryRow(`SELECT count(*) FROM triage_agent_playbook_runs_waiting_for_a_person`).Scan(&waiting); err != nil || waiting != 0 {
		t.Fatalf("%d recorded: %v", waiting, err)
	}
}

// MMX names a playbook by its file name, so MMHotline gives mmx the name
// only when it stands for exactly the file the agent asked for.
func TestMMXPlaybookName(t *testing.T) {
	for _, test := range []struct {
		path, name string
		ok         bool
	}{
		{"playbooks/deploy.yml", "deploy", true},
		{"playbooks/_shared-setup.yml", "_shared-setup", true},
		{"one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml", "2026-11-03_free_disk_space_on_app002", true},
		// MMX would read one_time_playbooks/2026-11-03_reboot_app002.yml for this name.
		{"playbooks/2026-11-03_reboot_app002.yml", "", false},
		{"one_time_playbooks/reboot_app002.yml", "", false},
		{"one_time_playbooks/completed/2026-11-03_reboot_app002.yml", "", false},
		{"playbooks/web/deploy.yml", "", false},
		{"roles/web/main.yml", "", false},
		{"playbooks/deploy.yaml", "", false},
	} {
		if name, ok := mmxPlaybookName(test.path); ok != test.ok || ok && name != test.name {
			t.Errorf("%s: %q %t", test.path, name, ok)
		}
	}
}

// With MMHOTLINE_TEST_MMX_PROGRAM naming an mmx program, MMHotline reads the
// real mmx explain and mmx nodes output of a small project, which proves the
// JSON field names it reads.
func TestRealMMXOutputIsRead(t *testing.T) {
	program := os.Getenv("MMHOTLINE_TEST_MMX_PROGRAM")
	if program == "" {
		t.Skip("set MMHOTLINE_TEST_MMX_PROGRAM to an mmx program, such as one built with go build -o $TMPDIR/mmx ./cmd/mmx in MMX's repository")
	}
	project := t.TempDir()
	for path, content := range map[string]string{
		"mmx_project_settings.yml":   "project_name: systems\ndefault_inventory: production\n",
		"inventories/production.yml": "nodes:\n  app002:\n    address: 192.0.2.10\n    ssh_user: root\n",
		"one_time_playbooks/2026-11-03_restart_caddy_and_reboot_app002.yml": `fleet_stages:
  - id: restart_caddy_and_reboot
    nodes: app002
    node_tasks:
      - systemd_service:
          name: caddy
          state: running
      - reboot: {}
`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(project, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output, err := readWithMMX(context.Background(), program, project, mmxEnvironment(project), "explain", "2026-11-03_restart_caddy_and_reboot_app002", "--inventory", "production", "--json")
	if err != nil {
		t.Fatal(err)
	}
	recognition, err := actionkinds.Recognize(output)
	if err != nil || !reflect.DeepEqual(recognition.Kinds, []actionkinds.Kind{actionkinds.StartOrRestartAService, actionkinds.RebootAServer}) {
		t.Fatalf("%+v %v\n%s", recognition, err, output)
	}
	if _, err := readWithMMX(context.Background(), program, project, mmxEnvironment(project), "explain", "2026-11-03_free_disk_space_on_app002", "--inventory", "production", "--json"); err == nil || !strings.Contains(err.Error(), "mmx explain failed: mmx: there is no one-time playbook 2026-11-03_free_disk_space_on_app002") {
		t.Fatalf("missing playbook: %v", err)
	}
	servers, err := inventoryServers(context.Background(), program, project, mmxEnvironment(project), "production")
	if want := []triageagent.InventoryServer{{ServerName: "app002", HostKeyAlias: "192.0.2.10"}}; err != nil || !reflect.DeepEqual(servers, want) {
		t.Fatalf("mmx nodes: %+v %v", servers, err)
	}
}

// What a playbook does, as the approval and warning alerts and the agent
// read it, names each task MMHotline did not recognize with why, in plain
// words, and counts those past the first ten.
func TestPlaybookKindsTextNamesUnrecognizedTasks(t *testing.T) {
	recognize := func(count int) actionkinds.Recognition {
		var tasks []string
		for number := 1; number <= count; number++ {
			tasks = append(tasks, fmt.Sprintf(`{"path":"fix/task_%d","node":"app002","action":"shell","module_kind":"core","status":"ready"}`, number))
		}
		recognition, err := actionkinds.Recognize([]byte(`{"schema_version":2,"fleet_stages":[{"node_tasks":[` + strings.Join(tasks, ",") + `]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return recognition
	}
	const does = "one_time_playbooks/fix.yml in the inventory “production” does: Unrecognized task."
	if text := playbookKindsText("one_time_playbooks/fix.yml in the inventory “production”", recognize(1)); text != does+" The task MMHotline did not recognize, and why: fix/task_1 (shell): no kind covers the module shell." {
		t.Error(text)
	}
	want := does + " The tasks MMHotline did not recognize, and why:"
	for number := 1; number <= 10; number++ {
		want += fmt.Sprintf(" fix/task_%d (shell): no kind covers the module shell.", number)
	}
	if text := playbookKindsText("one_time_playbooks/fix.yml in the inventory “production”", recognize(12)); text != want+" And 2 more." {
		t.Error(text)
	}
}
