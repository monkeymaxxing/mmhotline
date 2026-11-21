package app

import (
	"cmp"
	"context"
	"mmhotline/internal/triageagent"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
)

// The records that reach the systems repository between judging and running:
// the agent's note at the end of each run.
const (
	earlierAgentNote = "triage_agent_notes/2026-11-02_disk_full_on_app002.md"
	agentNote        = "triage_agent_notes/2026-11-03_app002_stopped_answering.md"
	runSummary       = "mmx_run_summaries/run.txt"
)

// What mmx explain prints for a playbook with one task MMHotline does not
// recognize, with the values it shows or hides.
func unrecognizedTaskExplained(task string) string {
	return `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"fix_app002","selection":"app002","nodes":["app002"],"node_tasks":[` + task + `]}]}`
}

var (
	shellExplained       = unrecognizedTaskExplained(`{"path":"fix_app002/task_1","node":"app002","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.script","value":"systemctl restart app","status":"literal"}]}`)
	hiddenShellExplained = unrecognizedTaskExplained(`{"path":"fix_app002/task_1","node":"app002","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.script","status":"runtime"}]}`)
)

// removeFromCopyAndOrigin deletes path in a commit to MMHotline's copy and
// origin and returns the commit.
func removeFromCopyAndOrigin(t *testing.T, a *App, path string) string {
	t.Helper()
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	testGit(t, directory, "rm", "--quiet", path)
	testGit(t, directory, "commit", "--quiet", "-m", "Remove "+path)
	testGit(t, directory, "push", "--quiet", "origin", "main")
	return testGit(t, directory, "rev-parse", "HEAD")
}

// advancingCopyRunner pushes before each execution attempt, as a person or
// the triage agent can do while a request waits. MMHotline still fetches,
// checks and runs MMX itself.
type advancingCopyRunner struct {
	a       *App
	t       *testing.T
	pushes  []string
	commits []string
}

func (r *advancingCopyRunner) RunPlaybook(ctx context.Context, request TriageAgentPlaybookRunRequest, commit string) TriageAgentPlaybookRunDecision {
	index := len(r.commits)
	r.commits = append(r.commits, commit)
	if index < len(r.pushes) {
		origin := testGit(r.t, filepath.Join(r.a.Store.Dir, "systems_repository"), "remote", "get-url", "origin")
		clone := filepath.Join(r.t.TempDir(), "clone")
		testGit(r.t, filepath.Dir(clone), "clone", "--quiet", origin, clone)
		path := filepath.Join(clone, r.pushes[index])
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("Another record\n"), 0o644); err != nil {
			r.t.Fatal(err)
		}
		testGit(r.t, clone, "add", "--all")
		testGit(r.t, clone, "commit", "--quiet", "-m", "Another record")
		testGit(r.t, clone, "push", "--quiet", "origin", "main")
	}
	return r.a.RunPlaybook(ctx, request, commit)
}

// newerCommitRig drives one approval request or warned run of the reboot
// playbook through its decision and its run, with newer commits in between.
type newerCommitRig struct {
	*triageSessionRig
	warned  bool
	records string
}

// prepare makes the reboot playbook wait for one approval, or with a
// 10-minute warning, with runMMX as MMX. explained, when set, is what mmx
// explain prints for it instead of a reboot. The judged commit also holds
// an earlier note of the agent, and then each of files.
func (r newerCommitRig) prepare(t *testing.T, runMMX, explained string, files map[string]string) string {
	t.Helper()
	if r.warned {
		prepareWarnedRun(t, r.a, runMMX, 10)
	} else {
		prepareApprovals(t, r.a, runMMX)
	}
	if explained != "" {
		installFakeMMX(t, r.a, map[string]string{"2026-11-03_reboot_app002": explained})
		if r.warned {
			if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.UnrecognizedTask), AgentPlaybookRule{Rule: warnWaitThenAct, WarningWaitMinutes: 10}); err != nil {
				t.Fatal(err)
			}
		}
	}
	judged := commitToCopyAndOrigin(t, r.a, map[string]string{earlierAgentNote: "# Disk full on app002\n"})
	if len(files) > 0 {
		judged = commitToCopyAndOrigin(t, r.a, files)
	}
	approvalSession(t, r.triageSessionRig)
	return judged
}

// decide approves the request, or lets the warning's 10 minutes pass, and
// runs what is due.
func (r newerCommitRig) decide(t *testing.T) {
	t.Helper()
	if r.warned {
		r.advance(10 * time.Minute)
	} else {
		if err := r.a.ApprovePlaybookRun(context.Background(), r.p, onlyApprovalRecord(t, r.a).id); err != nil {
			t.Fatal(err)
		}
	}
	runApprovedPlaybooks(t, r.a)
}

// results lists each record's state and result, oldest first, and the
// incident of the first.
func (r newerCommitRig) results(t *testing.T) (states, results []string, incidentID string) {
	t.Helper()
	if r.warned {
		for _, record := range warnedRunRecords(t, r.a) {
			states, results = append(states, record.state), append(results, record.result)
			incidentID = cmp.Or(incidentID, record.incidentID)
		}
		return states, results, incidentID
	}
	for _, record := range approvalRecords(t, r.a) {
		states, results = append(states, record.state), append(results, record.result)
		incidentID = cmp.Or(incidentID, record.incidentID)
	}
	return states, results, incidentID
}

// readMMXCalls lists actual MMX starts, excluding refused attempts.
func readMMXCalls(t *testing.T, records string) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(records, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}
func (r newerCommitRig) calls(t *testing.T) []string { return readMMXCalls(t, r.records) }

// decidedBy starts a result: who approved, or that nobody stopped it.
func (r newerCommitRig) decidedBy() string {
	if r.warned {
		return "Nobody stopped " + rebootPlaybook + " in the inventory “production” before 12:10 UTC."
	}
	return "Responder approved " + rebootPlaybook + " in the inventory “production”."
}

func (r newerCommitRig) what() string {
	if r.warned {
		return "warning"
	}
	return "approval"
}

// The commits after the judged one, as the end of the agent's run brings its
// note, hold the approval or the warning when they change only the agent's
// notes or the records of MMX runs and the playbook cannot read them: the
// playbook runs once, at the newer commit, and the history says both
// commits. Any other change, a playbook that could read a record, or a task
// list that changed makes the request stale, as before: nothing runs, and
// MMHotline judges the playbook again and asks or warns again.
func TestRunAtANewerCommitOfRecords(t *testing.T) {
	parallelStartingProcesses(t)
	type newer func(t *testing.T, a *App) string
	commit := func(files map[string]string) newer {
		return func(t *testing.T, a *App) string { return commitToCopyAndOrigin(t, a, files) }
	}
	for _, test := range []struct {
		name string
		// warnedToo also runs the test for a warned run.
		warnedToo      bool
		files          map[string]string
		explained      string
		newer          newer
		explainedAtRun string
		// why is the reason a stale record gives; empty when the run holds.
		why string
	}{
		{name: "a note of the agent, as at the end of the agent's run", warnedToo: true, newer: commit(map[string]string{agentNote: "# app002 stopped answering\n"})},
		{name: "a former run summary folder is an ordinary change", warnedToo: true, newer: func(t *testing.T, a *App) string {
			commitToCopyAndOrigin(t, a, map[string]string{runSummary: "Ran on: util01\n"})
			return removeFromCopyAndOrigin(t, a, earlierAgentNote)
		}, why: "the newer commits change mmx_run_summaries/run.txt, outside the triage agent's notes"},
		{name: "a change outside the records", warnedToo: true, newer: commit(map[string]string{agentNote: "# app002\n", "roles/web/main.yml": "node_tasks: []\n"}),
			why: "the newer commits change roles/web/main.yml, outside the triage agent's notes"},
		{name: "a playbook that names the notes", warnedToo: true, files: map[string]string{rebootPlaybook: rebootPlaybookText + "# The cause: triage_agent_notes/2026-11-02_disk_full_on_app002.md\n"}, newer: commit(map[string]string{agentNote: "# app002\n"}),
			why: "the playbook names the folder triage_agent_notes, so it could read a file that changed"},
		{name: "an unrecognized task with a value mmx explain does not show", warnedToo: true, explained: hiddenShellExplained, newer: commit(map[string]string{agentNote: "# app002\n"}),
			why: "fix_app002/task_1 (shell), a task MMHotline does not recognize, has a value mmx explain does not show, so it could read a file that changed"},
		{name: "an unrecognized task whose values mmx explain shows", explained: shellExplained, newer: commit(map[string]string{agentNote: "# app002\n"})},
		{name: "other tasks at the newer commit", newer: commit(map[string]string{agentNote: "# app002\n"}), explainedAtRun: strings.ReplaceAll(rebootExplained, "app002", "app003"),
			why: "mmx explain lists other tasks or values for the playbook at the newer commit"},
		{name: "a task that copies a directory of the systems repository", newer: commit(map[string]string{agentNote: "# app002\n"}),
			explained: unrecognizedTaskExplained(`{"path":"fix_app002/task_1","node":"app002","action":"directory","module_kind":"core","status":"ready","arguments":[{"field":"directory.path","value":"/srv/copy","status":"literal"},{"field":"directory.source","value":".","status":"literal"}]}`),
			why:       "fix_app002/task_1 (directory) copies a directory of the systems repository, which can hold the triage agent's notes"},
	} {
		for _, warned := range []bool{false, true} {
			if warned && !test.warnedToo {
				continue
			}
			name := test.name
			if warned {
				name += ", warned run"
			}
			t.Run(name, func(t *testing.T) {
				r := newerCommitRig{triageSessionRig: newTriageSessionRig(t, true), warned: warned, records: t.TempDir()}
				judged := r.prepare(t, miniRunMMX(r.records), test.explained, test.files)
				newer := test.newer(t, r.a)
				if test.explainedAtRun != "" {
					installFakeMMX(t, r.a, map[string]string{"2026-11-03_reboot_app002": test.explainedAtRun})
				}
				r.decide(t)
				states, results, incidentID := r.results(t)
				calls := r.calls(t)
				if test.why == "" {
					want := r.decidedBy() + " The run used commit " + newer + ", which is newer than commit " + judged + ", the one MMHotline checked. The commits in between change only the triage agent's notes, which the playbook does not read, so the " + r.what() + " still applies. MMHotline ran the playbook with MMX, and the run succeeded.\n\nPlaybook run output:\nPLAY RECAP app002 ok=1 changed=1\n"
					if len(states) != 1 || states[0] != "ran_and_succeeded" || results[0] != want {
						t.Fatalf("records: %q\n%q", states, results)
					}
					if len(calls) != 1 || calls[0] != mmxRunArguments(r.a, "2026-11-03_reboot_app002") {
						t.Fatalf("MMX calls: %q", calls)
					}
					if history := incidentHistory(t, r.a, r.p, incidentID); !inOrder(history, want) {
						t.Fatalf("history: %q", history)
					}
					return
				}
				stale := "Other commits reached MMHotline's copy of the systems repository before the run, so nothing ran at commit " + judged + ". The " + r.what() + " does not cover the newer commits: " + test.why + ". MMHotline checked the playbook again at the newer commit: "
				if len(states) != 2 || states[0] != "stale" || !strings.HasPrefix(results[0], r.decidedBy()+" "+stale) || states[1] != "open" {
					t.Fatalf("records: %q\n%q", states, results)
				}
				if len(calls) != 0 {
					t.Fatalf("MMX calls: %q", calls)
				}
			})
		}
	}
}

// A symlink in one_time_playbooks/, which a person may commit, can point at
// a note without naming its folder, so it ends the carry-over.
func TestRunAtANewerCommitWithASymlink(t *testing.T) {
	parallelStartingProcesses(t)
	r := newerCommitRig{triageSessionRig: newTriageSessionRig(t, true), records: t.TempDir()}
	prepareApprovals(t, r.a, miniRunMMX(r.records))
	directory := filepath.Join(r.a.Store.Dir, "systems_repository")
	if err := os.Symlink("../triage_agent_notes/2026-11-02_disk_full_on_app002.md", filepath.Join(directory, "one_time_playbooks", "cause.md")); err != nil {
		t.Fatal(err)
	}
	judged := commitToCopyAndOrigin(t, r.a, map[string]string{earlierAgentNote: "# Disk full on app002\n"})
	approvalSession(t, r.triageSessionRig)
	commitToCopyAndOrigin(t, r.a, map[string]string{earlierAgentNote: "# Changed\n"})
	r.decide(t)
	states, results, _ := r.results(t)
	if len(states) != 2 || states[0] != "stale" || !strings.Contains(results[0], "nothing ran at commit "+judged+". The approval does not cover the newer commits: one_time_playbooks/cause.md is a symlink, which can point at a file that changed.") {
		t.Fatalf("records: %q\n%q", states, results)
	}
}

// When the copy moves again between MMHotline's check and MMHotline's fetch,
// MMHotline checks the newer commit again and tries again, three execution attempts at most for one approval.
func TestRunAtANewerCommitWhileTheCopyMoves(t *testing.T) {
	parallelStartingProcesses(t)
	const otherNote = "triage_agent_notes/2026-11-03_app003_stopped_answering.md"
	for _, test := range []struct {
		name   string
		pushes []string
		calls  int
		why    string
	}{
		{name: "notes twice", pushes: []string{agentNote, otherNote}, calls: 3},
		{name: "then a change outside the records", pushes: []string{agentNote, "README.md"}, calls: 2,
			why: "the newer commits change README.md, outside the triage agent's notes"},
		{name: "notes at every try", pushes: []string{agentNote, otherNote, "triage_agent_notes/2026-11-03_another_note.md"}, calls: 3,
			why: "new commits kept arriving during 3 attempts to run the playbook"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newerCommitRig{triageSessionRig: newTriageSessionRig(t, true), records: t.TempDir()}
			judged := r.prepare(t, miniRunMMX(r.records), "", nil)
			runner := &advancingCopyRunner{a: r.a, t: t, pushes: test.pushes}
			r.a.triageAgent.playbookRunner = runner
			r.decide(t)
			states, results, _ := r.results(t)
			calls := r.calls(t)
			if len(runner.commits) != test.calls || runner.commits[0] != judged {
				t.Fatalf("MMX calls: %q", calls)
			}
			if test.why == "" {
				ranAt := runner.commits[len(runner.commits)-1]
				if len(states) != 1 || states[0] != "ran_and_succeeded" || !strings.Contains(results[0], "The run used commit "+ranAt+", which is newer than commit "+judged+", the one MMHotline checked.") || ranAt == runner.commits[1] || len(calls) != 1 {
					t.Fatalf("records: %q\n%q\ncalls: %q", states, results, calls)
				}
				return
			}
			if len(states) != 2 || states[0] != "stale" || !strings.Contains(results[0], "nothing ran at commit "+judged+". The approval does not cover the newer commits: "+test.why+". MMHotline checked the playbook again") {
				t.Fatalf("records: %q\n%q", states, results)
			}
		})
	}
}

// A playbook reads a record only where it names a record folder, so its
// text and every YAML key and value as MMX decodes it must not name the notes folder;
// an escape hides a name from the text, and a !!binary value hides its
// text.
func TestRecordFolderInPlaybook(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ text, want string }{
		{rebootPlaybookText, ""},
		{"fleet_stages:\n  - id: copy\n    nodes: app002\n    node_tasks:\n      - file: {path: /etc/motd, source: triage_agent_notes/x.md}\n", "the playbook names the folder triage_agent_notes, so it could read a file that changed"},
		{"fleet_stages:\n  - id: copy\n    nodes: app002\n    node_tasks:\n      - file: {path: /etc/motd, source: \"triage_agent_\\x6eotes/x.md\"}\n", "the playbook names the folder triage_agent_notes, so it could read a file that changed"},
		{"fleet_stages:\n  - id: copy\n    \"save_on_controller_\\x6dachine\": [{template: \"triage_agent_\\u006eotes/x\"}]\n", "the playbook names the folder triage_agent_notes, so it could read a file that changed"},
		{"fleet_stages:\n  - id: copy\n    nodes: app002\n    node_tasks:\n      - file: {path: /etc/motd, source: !!binary dHJpYWdlX2FnZW50X25vdGVzL3gubWQ=}\n", "the playbook holds a !!binary value, whose text MMHotline does not check"},
		{"fleet_stages: [\n", "MMHotline cannot read the playbook as YAML: "},
	} {
		if got := recordFolderInPlaybook(test.text); !strings.HasPrefix(got, test.want) || (test.want == "") != (got == "") {
			t.Errorf("%q: %q, want %q", test.text, got, test.want)
		}
	}
}

// A node task reads a record when mmx explain's output names a record
// folder, when it copies a directory of the project, or when it is a task
// MMHotline does not recognize whose values mmx explain does not all show.
// The recognized kinds read no project file, so their hidden values, such
// as the Cloudflare token, do not count. A registry password from the vault
// makes podman_image unrecognized, so it counts.
func TestTasksCouldReadARecord(t *testing.T) {
	t.Parallel()
	task := func(action, status, arguments string) string {
		return unrecognizedTaskExplained(`{"path":"fix_app002/task_1","node":"app002","action":"` + action + `","module_kind":"core","status":"` + status + `","arguments":[` + arguments + `]}`)
	}
	unrecognized := "fix_app002/task_1 (file), a task MMHotline does not recognize,"
	for _, test := range []struct{ explained, want string }{
		{rebootExplained, ""},
		{shellExplained, ""},
		{task("http", "ready", `{"field":"http.headers.Authorization","status":"secret"},{"field":"http.method","value":"PATCH","status":"literal"},{"field":"http.url","value":"https://api.cloudflare.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records/372e67954025e0ba6aaa6d586b9e0b59","status":"literal"}`), ""},
		{task("podman_image", "ready", `{"field":"podman_image.image","value":"ghcr.io/example/shop:3f2a1c9","status":"literal"},{"field":"podman_image.pull_image","value":"always","status":"literal"},{"field":"podman_image.registry_password","status":"secret"}`), "fix_app002/task_1 (podman_image), a task MMHotline does not recognize, has a value mmx explain does not show, so it could read a file that changed"},
		{task("file", "ready", `{"field":"file.path","value":"/etc/motd","status":"literal"},{"field":"file.source","value":"triage_agent_notes/x.md","status":"literal"}`), "the playbook's tasks listed by mmx explain name the folder triage_agent_notes, so a task could read a file that changed"},
		{task("role", "ready", `{"field":"inputs.notes","value":"/var/lib/mmhotline/systems_repository/triage_agent_notes","status":"resolved"}`), "the playbook's tasks listed by mmx explain name the folder triage_agent_notes, so a task could read a file that changed"},
		{task("directory", "ready", `{"field":"directory.path","value":"/srv/copy","status":"literal"},{"field":"directory.source","value":"/proc/self/cwd","status":"literal"}`), "fix_app002/task_1 (directory) copies a directory of the systems repository, which can hold the triage agent's notes"},
		{task("file", "ready", `{"field":"file.path","value":"/etc/motd","status":"literal"},{"field":"file.content","status":"runtime"}`), unrecognized + " has a value mmx explain does not show, so it could read a file that changed"},
		{task("file", "ready", `{"field":"file.path","value":"/etc/motd","status":"literal"},{"field":"file.content","status":"secret"}`), unrecognized + " has a value mmx explain does not show, so it could read a file that changed"},
		{task("file", "conditional", ``), unrecognized + " runs only if a condition holds on the server, so mmx explain does not show all its values"},
		// A file saved on MMHotline's server reads no record, but MMHotline
		// runs such a playbook only at the commit people saw.
		{`{"schema_version":2,"fleet_stages":[{"path":"check_caddy","node_tasks":[{"path":"check_caddy/task_1","node":"app002","action":"show","status":"ready"}],"save_on_controller_machine":[{"template":"one_time_playbooks/hook.tmpl","path":".git/hooks/pre-commit","files":[{"file":"/var/lib/mmhotline/systems_repository/.git/hooks/pre-commit"}],"permissions_mode":"0755"}]}]}`,
			"fleet stage check_caddy (save_on_controller_machine) writes a file on MMHotline's server, so MMHotline runs the playbook only at the commit MMHotline checked"},
		{"{", "MMHotline cannot read mmx explain's output: "},
	} {
		if got := tasksCouldReadARecord([]byte(test.explained), true); !strings.HasPrefix(got, test.want) || (test.want == "") != (got == "") {
			t.Errorf("%s:\n%q, want %q", test.explained, got, test.want)
		}
	}
	// A person's playbook in playbooks/: hidden values and server-side
	// conditions do not count, since the triage agent cannot have written it
	// to read a note; every other guard still does.
	for _, test := range []struct{ explained, want string }{
		{hiddenShellExplained, ""},
		{task("file", "conditional", ``), ""},
		{task("file", "ready", `{"field":"file.path","value":"/etc/motd","status":"literal"},{"field":"file.source","value":"triage_agent_notes/x.md","status":"literal"}`), "the playbook's tasks listed by mmx explain name the folder triage_agent_notes"},
		{task("directory", "ready", `{"field":"directory.path","value":"/srv/copy","status":"literal"},{"field":"directory.source","value":".","status":"literal"}`), "fix_app002/task_1 (directory) copies a directory of the systems repository"},
		{`{"schema_version":2,"fleet_stages":[{"path":"check_caddy","node_tasks":[],"save_on_controller_machine":[{"template":"playbooks/hook.tmpl","path":"x","files":[{"file":"/var/lib/mmhotline/x"}],"permissions_mode":"0644"}]}]}`,
			"fleet stage check_caddy (save_on_controller_machine) writes a file on MMHotline's server"},
	} {
		if got := tasksCouldReadARecord([]byte(test.explained), false); !strings.HasPrefix(got, test.want) || (test.want == "") != (got == "") {
			t.Errorf("a person's playbook, %s:\n%q, want %q", test.explained, got, test.want)
		}
	}
}

// The case found on real servers: the triage agent asks for the team's
// listed reboot_servers_after_updates, warned for 10 minutes, whose shell
// tasks run only if a condition holds on the server, and then commits a
// note. At the warned time the playbook runs once, at the newer commit,
// with one warning: a person's playbook cannot have been written to read
// the note.
func TestAWarnedListedPlaybookRunsOnceAfterANoteCommit(t *testing.T) {
	parallelStartingProcesses(t)
	const reboot = "playbooks/reboot_servers_after_updates.yml"
	records := t.TempDir()
	r := newTriageSessionRig(t, true)
	installFakeMMXRun(t, r.a, miniRunMMX(records))
	installFakeMMX(t, r.a, map[string]string{"reboot_servers_after_updates": unrecognizedTaskExplained(`{"path":"fix_app002/task_1","node":"app002","action":"shell","module_kind":"core","status":"conditional","arguments":[{"field":"shell.script","status":"runtime"}]}`)})
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Store.db.Exec(`INSERT INTO agent_playbook_rules_for_listed_playbooks(inventory,playbook_name,rule,warning_wait_minutes,approvals_needed,its_approval_also_approves_in_the_same_session,changed_at,changed_by) VALUES('production','reboot_servers_after_updates','warn_wait_then_act',10,0,'',1,'the settings file')`); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{reboot: "fleet_stages: []\n"})
	_, _, ask := coversSession(t, r, "app002")
	if answer := ask(reboot, "production"); answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "unless someone presses Stop") {
		t.Fatalf("the request: %+v", answer)
	}
	note := commitToCopyAndOrigin(t, r.a, map[string]string{agentNote: "# app002 needs a reboot after updates\n"})
	r.advance(10 * time.Minute)
	runApprovedPlaybooks(t, r.a)

	if calls := readMMXCalls(t, records); len(calls) != 1 || !strings.Contains(calls[0], "reboot_servers_after_updates") {
		t.Fatalf("MMX calls: %q", calls)
	}
	var state, result string
	if err := r.a.Store.db.QueryRow(`SELECT state,result_message FROM triage_agent_playbook_runs_waiting_for_a_person WHERE playbook=?`, reboot).Scan(&state, &result); err != nil || state != "ran_and_succeeded" || !strings.Contains(result, "The run used commit "+note) {
		t.Fatalf("the warned run: %q %q %v", state, result, err)
	}
	if warnings := countRows(t, r.a, `SELECT count(*) FROM alerts WHERE severity='warn' AND title LIKE ?`, "MMHotline will run "+reboot+"%"); warnings != 1 {
		t.Fatalf("%d warnings, want 1", warnings)
	}
}
