package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"mmhotline/internal/triageagent"
)

// fakeRunMMX records the arguments, working directory and environment of MMX.
func fakeRunMMX(records, body string) string {
	return "#!/bin/sh\nprintf '%s\\n' \"$*\" > '" + records + "/arguments'\npwd -P > '" + records + "/directory'\nenv > '" + records + "/environment'\n" + body
}

// mmxRunArguments is MMX's command line for a run of playbook in the
// inventory production: no prompts, and the team's vault passphrase from
// the file in MMHotline's state directory.
func mmxRunArguments(a *App, playbook string) string {
	return "run --non-interactive --vault-password-file " + filepath.Join(a.Store.Dir, "mmx_vault_passphrase") + " " + playbook + " --inventory production"
}

// installFakeMMXRun puts a fake MMX run on the same search path as explain.
func installFakeMMXRun(t *testing.T, a *App, body string) {
	t.Helper()
	program, _ := FindMMXProgram(a.triageAgent.mmxSearchPath)
	content, _ := os.ReadFile(program)
	if !strings.Contains(string(content), filepath.Join(a.Store.Dir, "fake_mmx_run")) {
		installFakeMMX(t, a, nil)
	}
	if err := os.WriteFile(filepath.Join(a.Store.Dir, "fake_mmx_run"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// commitRunMMXInCopy creates a copy with origin, and installs MMX outside it.
func commitRunMMXInCopy(t *testing.T, a *App, runMMX string) {
	t.Helper()
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	origin := filepath.Join(t.TempDir(), "origin.git")
	testGit(t, a.Store.Dir, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	testGit(t, a.Store.Dir, "clone", "--quiet", origin, directory)
	signMergesInCopyForTest(t, directory)
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("The servers.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, directory, "add", "README.md")
	testGit(t, directory, "commit", "--quiet", "-m", "Describe the servers")
	testGit(t, directory, "push", "--quiet", "origin", "main")
	if runMMX != "" {
		installFakeMMXRun(t, a, runMMX)
	} else {
		a.triageAgent.mmxSearchPath = t.TempDir()
	}
}

// mmxRunRequest records a playbook request of one triage run, as the run's
// reader does before it asks the decider.
func mmxRunRequest(t *testing.T, a *App, requestID string) TriageAgentPlaybookRunRequest {
	t.Helper()
	request := TriageAgentPlaybookRunRequest{RunID: "0123456789abcdef0123456789abcdef", RequestID: requestID, Playbook: requestedPlaybook, Inventory: "production"}
	if _, err := a.Store.db.Exec(`INSERT OR IGNORE INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by) VALUES(?,'triage_session','codex','api_key','running',1,'MMHotline')`, request.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_requests(run_id,request_id,kind,playbook,inventory,requested_at) VALUES(?,?,'playbook_run',?,?,1)`, request.RunID, request.RequestID, request.Playbook, request.Inventory); err != nil {
		t.Fatal(err)
	}
	return request
}

type mmxRunRecord struct {
	outcome, readAtCommitID, output  string
	startedAt, waitingSince, endedAt int64
	exitCode                         sql.NullInt64
}

func readMMXRunRecord(t *testing.T, a *App, runID, requestID string) mmxRunRecord {
	t.Helper()
	var record mmxRunRecord
	if err := a.Store.db.QueryRow(`SELECT outcome,playbook_read_at_commit_id,playbook_run_output,playbook_run_started_at,playbook_run_waiting_for_another_run_since,playbook_run_ended_at,playbook_run_exit_code FROM triage_agent_requests WHERE run_id=? AND request_id=?`, runID, requestID).Scan(&record.outcome, &record.readAtCommitID, &record.output, &record.startedAt, &record.waitingSince, &record.endedAt, &record.exitCode); err != nil {
		t.Fatal(err)
	}
	return record
}

// onlyPlaybookRequestID is the ID of the one playbook request of a run.
func onlyPlaybookRequestID(t *testing.T, a *App, runID string) string {
	t.Helper()
	var requestID string
	if err := a.Store.db.QueryRow(`SELECT request_id FROM triage_agent_requests WHERE run_id=? AND kind='playbook_run'`, runID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	return requestID
}

func waitForCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 seconds for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForFileInShell is a fake's shell line that waits until path exists,
// for at most 20 seconds, so a failed test leaves no process behind.
func waitForFileInShell(path string) string {
	return "i=0; while [ ! -e '" + path + "' ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done\n"
}

func fileExists(path string) func() bool {
	return func() bool { _, err := os.Stat(path); return err == nil }
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// requestFreeDiskSpace is the fake agent's work: it commits a one-time
// playbook and asks MMHotline to run it on production.
const requestFreeDiskSpace = `/bin/mkdir -p one_time_playbooks
printf 'fleet_stages: []\n' > ` + requestedPlaybook + `
git add --all
git commit --quiet -m 'Free disk space on app002'
mmhotline triage-agent request-playbook-run --playbook ` + requestedPlaybook + ` --inventory production --diagnosis-file "$HOME/diagnosis.md" > "$HOME/playbook-answer" 2>&1; echo $? > "$HOME/playbook-exit"
`

// actThenReportApp is an App whose rules for running the agent's playbooks let it free disk space on
// production without a person, with a stand-in for mmx explain.
func actThenReportApp(t *testing.T) *App {
	t.Helper()
	a := foundationApp(t)
	installFakeMMX(t, a, map[string]string{"2026-11-03_free_disk_space_on_app002": freeDiskSpaceExplained})
	if err := applyAgentPlaybookRulesPreset(context.Background(), a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	return a
}

// A push before execution moves the copy forward without starting MMX.
func TestMMXRunCopyMovedPastJudgedCommit(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "exit 0\n"))
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	judged := testGit(t, directory, "rev-parse", "HEAD")
	newer := pushToOriginOnly(t, a)
	request := mmxRunRequest(t, a, "00000000000000a1")
	decision := a.RunPlaybook(t.Context(), request, judged)
	if decision.Outcome != triageagent.Refused || decision.CopyMovedToCommitID != newer {
		t.Fatalf("%+v", decision)
	}
	if _, err := os.Stat(filepath.Join(records, "arguments")); !os.IsNotExist(err) {
		t.Fatalf("MMX started: %v", err)
	}
	if head := testGit(t, directory, "rev-parse", "HEAD"); head != newer {
		t.Fatalf("HEAD %s", head)
	}
}

// A commit that waits on origin's branch refuses the run with the review
// message even when the copy moved to a newer trusted commit, so nobody is
// asked to approve the playbook again at that commit.
func TestMMXRunRefusedWhenTheCopyMovedAndACommitWaits(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "exit 0\n"))
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	judged := testGit(t, directory, "rev-parse", "HEAD")
	trusted := commitInCopy(t, a, "trusted", "roles/a.yml")
	waiting := commitInCopy(t, a, "unsigned", "roles/b.yml")
	testGit(t, directory, "reset", "--quiet", "--hard", judged)
	decision := a.RunPlaybook(t.Context(), mmxRunRequest(t, a, "00000000000000a1"), judged)
	want := "MMHotline did not run the playbook: commit " + waiting[:7] + ", which no key the team trusts signed, waits for a person's review"
	if decision.Outcome != triageagent.Refused || decision.CopyMovedToCommitID != "" || !strings.HasPrefix(decision.Message, want) {
		t.Fatalf("%+v", decision)
	}
	if head := testGit(t, directory, "rev-parse", "HEAD"); head != trusted {
		t.Fatalf("HEAD %s, want the trusted commit %s", head, trusted)
	}
	if _, err := os.Stat(filepath.Join(records, "arguments")); !os.IsNotExist(err) {
		t.Fatalf("MMX started: %v", err)
	}
}

// The agent asks for a playbook whose kinds are set to Act, then report.
// MMHotline runs mmx run NAME --inventory production in its copy, keeps
// the whole output with the request, and answers with its end.
func TestTriageAgentActThenReportRunsMMX(t *testing.T) {
	a := actThenReportApp(t)
	records := t.TempDir()
	longOutput := "TASK [free disk space]\n" + strings.Repeat("ok: [app002] systemd_journal_vacuum\n", 1000) + "PLAY RECAP app002 ok=1 changed=1 failed=0\n"
	if err := os.WriteFile(filepath.Join(records, "output"), []byte(longOutput), 0o644); err != nil {
		t.Fatal(err)
	}
	run := prepareSystemsRepositoryRunWithRunMMX(t, a, fakeRunMMX(records, "cat '"+records+"/output'\nexit 0\n"), requestFreeDiskSpace+codexFinishes)
	run.start(t)
	run.wait(t)

	leftOut := len(longOutput) - 16<<10
	want := "MMHotline pushed your commits to origin. " + requestedPlaybook + " in the inventory “production” does: Free disk space. For the inventory “production”, the rules for running the agent's playbooks let MMHotline run the playbook without a person. MMHotline ran the playbook with MMX, and the run succeeded." +
		"\n\nPlaybook run output:\n[MMHotline left out the first " + strconv.Itoa(leftOut) + " bytes of the output and keeps the whole output with the run.]\n" + longOutput[leftOut:]
	if answer, exit := run.commandResult(t, "playbook"); answer != want || exit != "0" {
		t.Fatalf("playbook command: exit %s\n%.400q\nwant exit 0\n%.400q", exit, answer, want)
	}
	if arguments := readTestFile(t, filepath.Join(records, "arguments")); arguments != mmxRunArguments(a, "2026-11-03_free_disk_space_on_app002")+"\n" {
		t.Fatalf("MMX's arguments: %q", arguments)
	}
	copyDirectory, err := filepath.EvalSymlinks(filepath.Join(a.Store.Dir, "systems_repository"))
	if err != nil {
		t.Fatal(err)
	}
	if directory := readTestFile(t, filepath.Join(records, "directory")); directory != copyDirectory+"\n" {
		t.Fatalf("MMX ran in %q, not in MMHotline's copy %q", directory, copyDirectory)
	}
	record := readMMXRunRecord(t, a, run.id, onlyPlaybookRequestID(t, a, run.id))
	pushed := testGit(t, run.origin, "rev-parse", "main")
	if record.outcome != "ran_and_succeeded" || record.output != longOutput || record.readAtCommitID != pushed || record.startedAt == 0 || record.endedAt < record.startedAt || record.exitCode != (sql.NullInt64{Int64: 0, Valid: true}) {
		t.Fatalf("record: %+v", record)
	}
}

// Every nonzero exit is ran_and_failed and names MMX's exit code.
// An MMX program that cannot start runs nothing and is refused.
func TestMMXRunOutcomes(t *testing.T) {
	for _, test := range []struct {
		name, body, output string
		outcome            triageagent.RequestOutcome
		says               string
		exitCode           sql.NullInt64
	}{
		{"MMX failed", "echo 'PLAY RECAP app002 failed=1'\nexit 2\n", "PLAY RECAP app002 failed=1\n", triageagent.RanAndFailed, "MMX exited with 2.", sql.NullInt64{Int64: 2, Valid: true}},
		{"MMX exited 1", "echo failed >&2\nexit 1\n", "failed\n", triageagent.RanAndFailed, "MMX exited with 1.", sql.NullInt64{Int64: 1, Valid: true}},
		{"no MMX", "", "", triageagent.Refused, "MMHotline finds no mmx program", sql.NullInt64{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := foundationApp(t)
			runMMX := ""
			if test.body != "" {
				runMMX = fakeRunMMX(t.TempDir(), test.body)
			}
			commitRunMMXInCopy(t, a, runMMX)
			request := mmxRunRequest(t, a, "00000000000000a1")
			decision := a.RunPlaybook(context.Background(), request, testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD"))
			if decision.Outcome != test.outcome || !strings.Contains(decision.Message, test.says) || decision.PlaybookRunOutput != test.output {
				t.Fatalf("%+v", decision)
			}
			record := readMMXRunRecord(t, a, request.RunID, request.RequestID)
			if record.exitCode != test.exitCode || record.output != test.output || (record.endedAt != 0) != (test.body != "") {
				t.Fatalf("record: %+v", record)
			}
		})
	}
}

// A held flock records waiting, blocks MMX, and releases the queued run.
func TestMMXRunWaitingForAnotherRun(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "echo 'PLAY RECAP app002 ok=1'\n"))
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	lock, err := os.OpenFile(directory+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	request := mmxRunRequest(t, a, "00000000000000a1")
	commit := testGit(t, directory, "rev-parse", "HEAD")
	before := time.Now().Unix()
	decisions := make(chan TriageAgentPlaybookRunDecision, 1)
	go func() { decisions <- a.RunPlaybook(context.Background(), request, commit) }()
	waitForCondition(t, "the record of the wait", func() bool { return readMMXRunRecord(t, a, request.RunID, request.RequestID).waitingSince != 0 })
	if record := readMMXRunRecord(t, a, request.RunID, request.RequestID); record.waitingSince < before || record.startedAt != 0 {
		t.Fatalf("%+v", record)
	}
	if _, err := os.Stat(filepath.Join(records, "arguments")); !os.IsNotExist(err) {
		t.Fatalf("MMX started under another holder: %v", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if decision := <-decisions; decision.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("%+v", decision)
	}
	if record := readMMXRunRecord(t, a, request.RunID, request.RequestID); record.waitingSince != 0 || record.output != "PLAY RECAP app002 ok=1\n" || !record.exitCode.Valid || record.exitCode.Int64 != 0 {
		t.Fatalf("record: %+v", record)
	}
	holder := readTestFile(t, directory+".lock")
	if !strings.HasPrefix(holder, "mmhotline: mmx run 2026-11-03_free_disk_space_on_app002 --inventory production for the triage agent, started ") || !strings.HasSuffix(holder, ", process "+strconv.Itoa(os.Getpid())+"\n") {
		t.Fatalf("holder: %q", holder)
	}
}

func TestMMXRunRefusesTrackedChanges(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "exit 0\n"))
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	commit := testGit(t, directory, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("Uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decision := a.RunPlaybook(t.Context(), mmxRunRequest(t, a, "00000000000000a1"), commit)
	if decision.Outcome != triageagent.Refused || !strings.Contains(decision.Message, "uncommitted changes to tracked files") {
		t.Fatalf("%+v", decision)
	}
	if _, err := os.Stat(filepath.Join(records, "arguments")); !os.IsNotExist(err) {
		t.Fatalf("MMX started: %v", err)
	}
}

// MMX gets HOME and PATH and nothing of MMHotline's environment, so no
// GIT_ or MMX_ variable changes what Git or MMX does, and no setting of
// MMHotline's own reaches it.
func TestMMXRunEnvironment(t *testing.T) {
	inherited := []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_GLOBAL", "GIT_SSH_COMMAND", "MMX_STATE_DIR", "MMX_RUNNER_DIR", "MMX_VAULT_PASSWORD_FILE", "MMHOTLINE_WEB_URL"}
	for _, variable := range inherited {
		t.Setenv(variable, "/elsewhere")
	}
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "exit 0\n"))
	request := mmxRunRequest(t, a, "00000000000000a1")
	if decision := a.RunPlaybook(context.Background(), request, testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD")); decision.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("%+v", decision)
	}
	environment := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(readTestFile(t, filepath.Join(records, "environment")), "\n"), "\n") {
		name, value, _ := strings.Cut(line, "=")
		environment[name] = value
	}
	if environment["HOME"] != a.Store.Dir || environment["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("HOME %q, PATH %q", environment["HOME"], environment["PATH"])
	}
	// The shell adds PWD, SHLVL, _, and OLDPWD itself.
	for name := range environment {
		switch name {
		case "HOME", "PATH", "PWD", "SHLVL", "_", "OLDPWD":
		default:
			t.Errorf("MMX got %s=%s", name, environment[name])
		}
	}
}

// A run that does not finish within the time limit is ended and recorded as
// failed, with the limit in the message.
func TestMMXRunTimeLimit(t *testing.T) {
	a := foundationApp(t)
	// Long enough for the fetch before MMX starts, and for macOS to check the
	// new script before its first line, even under the whole suite's load.
	a.triageAgent.mmxRunTimeLimit = 4 * time.Second
	commitRunMMXInCopy(t, a, fakeRunMMX(t.TempDir(), "echo 'TASK [free disk space]'\nsleep 20\necho 'PLAY RECAP app002 ok=1'\n"))
	request := mmxRunRequest(t, a, "00000000000000a1")
	started := time.Now()
	decision := a.RunPlaybook(context.Background(), request, testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD"))
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the run ended after %s", elapsed)
	}
	if decision.Outcome != triageagent.RanAndFailed || !strings.Contains(decision.Message, "MMX did not finish within 4s, MMHotline's time limit for an MMX run") {
		t.Fatalf("%+v", decision)
	}
	if record := readMMXRunRecord(t, a, request.RunID, request.RequestID); record.exitCode.Valid || record.endedAt == 0 || record.output != "TASK [free disk space]\n" {
		t.Fatalf("record: %+v", record)
	}
}

// A stop reaches the agent at once, but the MMX run is the current step: it
// goes on to its end and is recorded, even after the triage run's context
// ends.
func TestMMXRunOutlastsAStoppedTriageRun(t *testing.T) {
	a := actThenReportApp(t)
	records := t.TempDir()
	// The agent reports its session first, so the stopped run can be resumed.
	reportsSession := `printf '%s\n' '{"type":"thread.started","thread_id":"session-one"}'` + "\n"
	run := prepareSystemsRepositoryRunWithRunMMX(t, a, fakeRunMMX(records, "echo 'TASK [free disk space]'\n: > '"+records+"/started'\n"+waitForFileInShell(filepath.Join(records, "go_on"))+"echo 'PLAY RECAP app002 ok=1'\n"), reportsSession+requestFreeDiskSpace+codexFinishes)
	run.start(t)
	waitForCondition(t, "MMX to start", fileExists(filepath.Join(records, "started")))
	if err := a.StopTriageAgentRun(run.id); err != nil {
		t.Fatal(err)
	}
	if err := <-run.sandbox; err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	// MMHotline ends its side of the run too, as when it stops, which
	// cancels the run's context.
	a.triageAgent.mu.Lock()
	active := a.triageAgent.activeRun
	a.triageAgent.mu.Unlock()
	closed := make(chan struct{})
	go func() { a.CloseTriageAgent(); close(closed) }()
	waitForCondition(t, "the triage run's context to end", func() bool { return active.ctx.Err() != nil })
	// Time for a run that went with the context to be ended.
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(records, "go_on"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end after the MMX run")
	}
	record := readMMXRunRecord(t, a, run.id, onlyPlaybookRequestID(t, a, run.id))
	if record.outcome != "ran_and_succeeded" || record.output != "TASK [free disk space]\nPLAY RECAP app002 ok=1\n" || record.exitCode != (sql.NullInt64{Int64: 0, Valid: true}) || record.endedAt == 0 {
		t.Fatalf("record: %+v", record)
	}
}

// MMHotline runs one MMX run at a time: a second call waits for the first,
// and its record shows the wait while the first runs.
func TestMMXRunsOneAtATime(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	order, finish := filepath.Join(records, "order"), filepath.Join(records, "finish")
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "echo \"started $$\" >> '"+order+"'\n"+waitForFileInShell(finish)+"echo \"ended $$\" >> '"+order+"'\n"))
	first, second := mmxRunRequest(t, a, "00000000000000a1"), mmxRunRequest(t, a, "00000000000000a2")
	decisions := make(chan TriageAgentPlaybookRunDecision, 2)
	go func() {
		decisions <- a.RunPlaybook(context.Background(), first, testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD"))
	}()
	waitForCondition(t, "the first run to start", fileExists(order))
	go func() {
		decisions <- a.RunPlaybook(context.Background(), second, testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD"))
	}()
	waitForCondition(t, "the record of the second run's wait", func() bool {
		return readMMXRunRecord(t, a, second.RunID, second.RequestID).waitingSince != 0
	})
	if err := os.WriteFile(finish, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if decision := <-decisions; decision.Outcome != triageagent.RanAndSucceeded {
			t.Fatalf("%+v", decision)
		}
	}
	lines := strings.Fields(readTestFile(t, order))
	if len(lines) != 8 || lines[0] != "started" || lines[2] != "ended" || lines[4] != "started" || lines[6] != "ended" || lines[1] != lines[3] || lines[5] != lines[7] || lines[1] == lines[5] {
		t.Fatalf("runs overlapped: %q", lines)
	}
}

// The shared lock remains held while MMX changes servers and is released at
// the end, so a person's helper cannot change the working tree midway.
func TestMMXRunHoldsCopyLockThroughExecution(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, ": > '"+records+"/started'\n"+waitForFileInShell(filepath.Join(records, "finish"))))
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	commit := testGit(t, directory, "rev-parse", "HEAD")
	request := mmxRunRequest(t, a, "00000000000000a1")
	decisions := make(chan TriageAgentPlaybookRunDecision, 1)
	go func() { decisions <- a.RunPlaybook(context.Background(), request, commit) }()
	waitForCondition(t, "MMX to start", fileExists(filepath.Join(records, "started")))
	lock, err := os.OpenFile(directory+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
		t.Fatalf("copy lock during MMX: %v", err)
	}
	if holder := readTestFile(t, directory+".lock"); !strings.Contains(holder, "for the triage agent, started ") {
		t.Fatalf("holder during MMX: %q", holder)
	}
	if err := os.WriteFile(filepath.Join(records, "finish"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if decision := <-decisions; decision.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("%+v", decision)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("copy lock after MMX: %v", err)
	}
	if head := testGit(t, directory, "rev-parse", "HEAD"); head != commit {
		t.Fatalf("MMX committed a summary: %s", head)
	}
}

func TestMMXRunTimeLimitKillsAnUnresponsiveProcessAfterGrace(t *testing.T) {
	a := foundationApp(t)
	// The limit also covers the fetch before MMX starts, so it leaves a busy
	// machine time to start the fake MMX and set its trap before SIGTERM.
	a.triageAgent.mmxRunTimeLimit = 5 * time.Second
	commitRunMMXInCopy(t, a, fakeRunMMX(t.TempDir(), "trap '' TERM\necho 'MMX ignores SIGTERM'\nexec sleep 60\n"))
	commit := testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD")
	request := mmxRunRequest(t, a, "00000000000000a1")
	started := time.Now()
	decision := a.RunPlaybook(t.Context(), request, commit)
	elapsed := time.Since(started)
	if elapsed < 15*time.Second || elapsed > 20*time.Second {
		t.Fatalf("SIGKILL after %s; want the 5s limit and 10s grace", elapsed)
	}
	if decision.Outcome != triageagent.RanAndFailed || !strings.Contains(decision.Message, "MMX did not finish within 5s") {
		t.Fatalf("%+v", decision)
	}
	if record := readMMXRunRecord(t, a, request.RunID, request.RequestID); record.exitCode.Valid || record.endedAt == 0 || record.output != "MMX ignores SIGTERM\n" {
		t.Fatalf("%+v", record)
	}
}

// A copy behind the judged commit or on a different history is never
// treated as a newer commit that may keep an approval.
func TestMMXRunRefusesAHeadOutsideTheJudgedHistory(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "exit 0\n"))
	judged := pushToOriginOnly(t, a)
	testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "fetch", "--quiet", "origin")
	// A later origin rewind leaves this disposable copy behind the judgment.
	origin := testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "remote", "get-url", "origin")
	older := testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD")
	testGit(t, origin, "update-ref", "refs/heads/main", older)
	decision := a.RunPlaybook(t.Context(), mmxRunRequest(t, a, "00000000000000a1"), judged)
	if decision.Outcome != triageagent.Refused || decision.CopyMovedToCommitID != "" || !strings.Contains(decision.Message, "not the judged commit") {
		t.Fatalf("%+v", decision)
	}
	if _, err := os.Stat(filepath.Join(records, "arguments")); !os.IsNotExist(err) {
		t.Fatalf("MMX started: %v", err)
	}
}

// An unattended run judges again when origin advances before execution.
func TestTriageAgentPlaybookJudgedAgainWhenTheCopyMoved(t *testing.T) {
	a := actThenReportApp(t)
	records := t.TempDir()
	run := prepareSystemsRepositoryRunWithRunMMX(t, a, miniRunMMX(records), requestFreeDiskSpace+codexFinishes)
	runner := &advancingCopyRunner{a: a, t: t, pushes: []string{"README.md"}}
	a.triageAgent.playbookRunner = runner
	run.start(t)
	run.wait(t)
	answer, exit := run.commandResult(t, "playbook")
	if exit != "0" || !strings.Contains(answer, "MMHotline ran the playbook with MMX, and the run succeeded.") {
		t.Fatalf("exit %s: %s", exit, answer)
	}
	if len(runner.commits) != 2 || runner.commits[0] == runner.commits[1] || len(readMMXCalls(t, records)) != 1 {
		t.Fatalf("execution attempts: %v", runner.commits)
	}
}

// A triage session asks for two playbooks a person committed, one after the
// other, and between them someone else pushes to origin, as a playbook that
// creates a server commits and pushes the inventory from a separate clone.
// The agent commits nothing, so MMHotline fetches on its own before judging:
// the second request is judged and runs at the newer commit, with no second
// judging.
func TestASecondRequestRunsAtTheCommitSomeoneElsePushed(t *testing.T) {
	a := actThenReportApp(t)
	records := t.TempDir()
	explainCalls := installFakeMMX(t, a, map[string]string{"2026-11-03_free_disk_space_on_app002": freeDiskSpaceExplained})
	installFakeMMXRun(t, a, miniRunMMX(records))
	ask := func(name string) string {
		return `mmhotline triage-agent request-playbook-run --playbook ` + requestedPlaybook + ` --inventory production --diagnosis-file "$HOME/diagnosis.md" > "$HOME/` + name + `-answer" 2>&1; echo $? > "$HOME/` + name + `-exit"
`
	}
	// As waitForFileInShell, with $HOME expanded and /bin/sleep, since the
	// agent's search path holds only git, mmhotline, and codex.
	waitForThePush := `i=0; while [ ! -e "$HOME/someone-else-pushed" ] && [ $i -lt 400 ]; do /bin/sleep 0.05; i=$((i+1)); done
`
	run := prepareSystemsRepositoryRunWithFiles(t, a, map[string]string{"README.md": "The servers.\n", requestedPlaybook: "fleet_stages: []\n"}, ask("first")+waitForThePush+ask("second")+codexFinishes)
	copyDirectory := filepath.Join(a.Store.Dir, "systems_repository")
	first := testGit(t, copyDirectory, "rev-parse", "HEAD")
	run.start(t)
	waitForCondition(t, "the first request's answer", fileExists(filepath.Join(run.agentHome, "first-exit")))
	newer := pushToOriginOnly(t, a)
	if err := os.WriteFile(filepath.Join(run.agentHome, "someone-else-pushed"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run.wait(t)

	for _, name := range []string{"first", "second"} {
		if answer, exit := run.commandResult(t, name); exit != "0" || !strings.HasPrefix(answer, requestedPlaybook+" in the inventory “production” does: Free disk space.") || !strings.Contains(answer, "MMHotline ran the playbook with MMX, and the run succeeded.") {
			t.Errorf("%s request: exit %s: %s", name, exit, answer)
		}
	}
	rows, err := a.Store.db.Query(`SELECT playbook_read_at_commit_id,outcome FROM triage_agent_requests WHERE run_id=? AND kind='playbook_run' ORDER BY requested_at,rowid`, run.id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var judgedAt []string
	for rows.Next() {
		var commit, outcome string
		if err := rows.Scan(&commit, &outcome); err != nil {
			t.Fatal(err)
		}
		judgedAt = append(judgedAt, commit+" "+outcome)
	}
	if want := []string{first + " ran_and_succeeded", newer + " ran_and_succeeded"}; strings.Join(judgedAt, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests judged and run at:\n%s\nwant\n%s", strings.Join(judgedAt, "\n"), strings.Join(want, "\n"))
	}
	// mmx explain read the playbook once for each request, the second time
	// at the newer commit. MMX ran once for each request.
	if explained := strings.Count(readTestFile(t, explainCalls), "explain "); explained != 2 {
		t.Errorf("mmx explain ran %d times", explained)
	}
	if calls := readMMXCalls(t, records); len(calls) != 2 {
		t.Fatalf("MMX runs: %q", calls)
	}
	if head := testGit(t, copyDirectory, "rev-parse", "HEAD"); head != newer {
		t.Fatalf("MMHotline's copy is at %s, not at the newer commit %s", head, newer)
	}
}
