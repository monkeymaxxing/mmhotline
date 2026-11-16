package triageagent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for mmhotline when a fake coding agent runs
// mmhotline triage-agent request-playbook-run or report, or its ssh runs
// mmhotline triage-agent ssh.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "triage-agent" && IsAgentRequestCommand(os.Args[2]) {
		os.Exit(RunAgentRequestCommand(os.Args[2], os.Args[3:], os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 3 && os.Args[1] == "triage-agent" && os.Args[2] == SSHAction {
		os.Exit(RunSSHCommand(os.Args[3:], os.Stderr))
	}
	os.Exit(m.Run())
}

func runRequestCommand(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := RunAgentRequestCommand(args[0], args[1:], &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAgentRequestCommandsWorkOnlyInsideARun(t *testing.T) {
	t.Setenv(RequestSocketEnvironmentVariable, "")
	report := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(report, []byte("# Disk full on app002\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"request-playbook-run", "--playbook", "playbooks/deploy.yml", "--inventory", "production", "--diagnosis-file", report},
		{"report", "--alert-severity", "warn", "--report-file", report},
	} {
		code, _, stderr := runRequestCommand(args...)
		if code != 4 || !strings.Contains(stderr, "works only inside a triage agent run") {
			t.Fatalf("%v: exit %d, %q", args, code, stderr)
		}
	}
}

func TestAgentRequestCommandArgumentChecks(t *testing.T) {
	// A socket nobody listens on: a request that passes the checks fails to
	// connect instead.
	t.Setenv(RequestSocketEnvironmentVariable, filepath.Join(t.TempDir(), "request.sock"))
	directory := t.TempDir()
	reportFile := func(name string, size int) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, bytes.Repeat([]byte("a"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	report, empty, oversized := reportFile("report.md", 100), reportFile("empty.md", 0), reportFile("oversized.md", MaximumMarkdownFileSize+1)
	playbook := func(path, inventory string) []string {
		return []string{"request-playbook-run", "--playbook", path, "--inventory", inventory, "--diagnosis-file", report}
	}
	commit := strings.Repeat("ab", 20)
	review := func(commitID, decision, severity string) []string {
		args := []string{"review-commit", "--commit-id", commitID, "--decision", decision, "--report-file", report}
		if severity != "" {
			args = append(args, "--alert-severity", severity)
		}
		return args
	}
	withDiagnosis := func(diagnosisFile string) []string {
		return []string{"request-playbook-run", "--playbook", "playbooks/deploy.yml", "--inventory", "production", "--diagnosis-file", diagnosisFile}
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{playbook("one_time_playbooks/../run_mmx.yml", "production"), `".." part`},
		{playbook("one_time_playbooks//x.yml", "production"), `empty, ".", or ".." part`},
		{playbook("/etc/mmx/playbook.yml", "production"), "must be relative to the systems repository"},
		{playbook("one_time_playbooks/x.yaml", "production"), "must end in .yml"},
		{playbook(strings.Repeat("a", 297)+".yml", "production"), "has 301 characters; at most 300"},
		{playbook("playbooks/deploy.yml", "production;reboot"), "inventory name"},
		{playbook("playbooks/deploy.yml", strings.Repeat("a", 101)), "at most 100 characters"},
		{[]string{"request-playbook-run", "--inventory", "production", "--diagnosis-file", report}, "--playbook is required"},
		{[]string{"request-playbook-run", "--playbook", "playbooks/deploy.yml", "--diagnosis-file", report}, "--inventory is required"},
		{[]string{"request-playbook-run", "--playbook", "playbooks/deploy.yml", "--inventory", "production"}, "--diagnosis-file is required: a Markdown file with what you found, the cause, the evidence, and why the playbook fixes the problem"},
		{withDiagnosis(oversized), "the diagnosis is 65537 bytes, over the limit of 65536 bytes (64 KiB)"},
		{withDiagnosis(empty), "the diagnosis is empty"},
		{withDiagnosis(filepath.Join(directory, "missing.md")), "read the diagnosis file: "},
		{[]string{"report", "--alert-severity", "critical", "--report-file", report}, `alert severity "critical" must be info, warn, or sos`},
		{[]string{"report", "--alert-severity", "warn", "--report-file", oversized}, "the report is 65537 bytes, over the limit of 65536 bytes (64 KiB)"},
		{[]string{"report", "--alert-severity", "warn", "--report-file", empty}, "the report is empty"},
		{[]string{"report", "--report-file", report}, "--alert-severity is required"},
		{[]string{"report", "--alert-severity", "warn"}, "--report-file is required"},
		{review("abc1234", "approve", ""), `commit ID "abc1234" must be a full Git commit ID`},
		{review(commit, "approve", "info"), "approve takes no alert severity"},
		{review(commit, "needs_a_person", ""), "--alert-severity is required with needs_a_person"},
		{review(commit, "needs_a_person", "critical"), `alert severity "critical" must be info, warn, or sos`},
		{review(commit, "allow", ""), `the decision "allow" must be approve or needs_a_person`},
		{[]string{"review-commit", "--decision", "approve", "--report-file", report}, "--commit-id is required"},
		{[]string{"review-commit", "--commit-id", commit, "--report-file", report}, "--decision is required"},
		{[]string{"review-commit", "--commit-id", commit, "--decision", "approve"}, "--report-file is required"},
		// Valid requests pass every check and reach for the socket.
		{playbook(strings.Repeat("a", 296)+".yml", strings.Repeat("a", 100)), "cannot reach the triage agent run's request socket"},
		{playbook("one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml", "production_eu-2"), "cannot reach"},
		{[]string{"report", "--alert-severity", "sos", "--report-file", reportFile("largest.md", MaximumMarkdownFileSize)}, "cannot reach"},
		{withDiagnosis(reportFile("largest.md", MaximumMarkdownFileSize)), "cannot reach"},
		{review(commit, "approve", ""), "cannot reach"},
		{review(commit, "needs_a_person", "sos"), "cannot reach"},
	} {
		code, _, stderr := runRequestCommand(test.args...)
		if code != 4 || !strings.Contains(stderr, test.want) {
			t.Errorf("%.80v: exit %d, %q; want %q", test.args, code, stderr, test.want)
		}
	}
}

// The agent reads --help to learn what each exit code asks of it.
func TestAgentRequestCommandHelpStatesExitCodes(t *testing.T) {
	code, stdout, _ := runRequestCommand("request-playbook-run", "--help")
	for _, want := range []string{"0  the playbook ran and succeeded", "1  the playbook ran and failed", "3  the request waits for a person: end your turn and report", "4  the request was refused or is invalid", "review agent refused the playbook, read the refusal reason in the answer", "--diagnosis-file", "why the playbook fixes the problem"} {
		if code != 0 || !strings.Contains(stdout, want) {
			t.Fatalf("exit %d, help lacks %q:\n%s", code, want, stdout)
		}
	}
	// The agent takes an example inventory for an instruction, and the task
	// names the real ones.
	if !strings.Contains(stdout, "inventories/NAME.yml") || strings.Contains(stdout, "production") {
		t.Fatalf("help names an example inventory:\n%s", stdout)
	}
	code, stdout, _ = runRequestCommand("report", "--help")
	if code != 0 || !strings.Contains(stdout, "0  MMHotline recorded the report") || !strings.Contains(stdout, "4  the report was refused") {
		t.Fatalf("exit %d, report help:\n%s", code, stdout)
	}
	code, stdout, _ = runRequestCommand("review-commit", "--help")
	for _, want := range []string{"0  MMHotline recorded your decision", "When\n     MMHotline refused approve, decide needs_a_person and report", "--alert-severity", "works only in a commit review run"} {
		if code != 0 || !strings.Contains(stdout, want) {
			t.Fatalf("exit %d, review-commit help lacks %q:\n%s", code, want, stdout)
		}
	}
}

// requestScript runs the test binary as mmhotline triage-agent ACTION and
// keeps its output in $HOME/NAME-answer and its exit code in $HOME/NAME-exit.
func requestScript(t *testing.T, name string, args string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return `'` + executable + `' triage-agent ` + args + ` > "$HOME/` + name + `-answer" 2>&1; echo $? > "$HOME/` + name + `-exit"`
}

func readCommandResult(t *testing.T, home, name string) (string, string) {
	t.Helper()
	answer, err := os.ReadFile(filepath.Join(home, name+"-answer"))
	if err != nil {
		t.Fatal(err)
	}
	exit, err := os.ReadFile(filepath.Join(home, name+"-exit"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(answer)), strings.TrimSpace(string(exit))
}

func answerRequest(t *testing.T, run *fakeRun, answer Answer) {
	t.Helper()
	if err := json.NewEncoder(run.input).Encode(Control{Answer: &answer}); err != nil {
		t.Fatal(err)
	}
}

// nextEvents reads events until it has one of each named event and returns
// them by name; other events, such as steps, may come in between.
func (run *fakeRun) nextEvents(t *testing.T, names ...string) map[string]Event {
	t.Helper()
	found := map[string]Event{}
	for len(found) < len(names) {
		event := run.next(t)
		for _, name := range names {
			if event.Event == name {
				found[name] = event
			}
		}
	}
	return found
}

// A report goes up the run socket and MMHotline's answer comes back to the
// command. A run without a systems repository refuses a playbook request
// itself, and nothing goes up.
func TestSandboxRelaysAReportAndRefusesPlaybooksWithoutARepository(t *testing.T) {
	script := codexSession + `
printf '# Disk full on app002\n\nFreed 4 GB.\n' > "$HOME/report.md"
` + requestScript(t, "report", `report --alert-severity warn --report-file "$HOME/report.md"`) + `
` + requestScript(t, "playbook", `request-playbook-run --playbook playbooks/deploy.yml --inventory production --diagnosis-file "$HOME/report.md"`) + `
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`
	run := startFakeRun(t, "codex", script, testRequest("codex"))
	_ = run.next(t)
	report := run.next(t)
	if report.Event != "report" || report.AlertSeverity != "warn" || report.ReportText != "# Disk full on app002\n\nFreed 4 GB.\n" || CheckRequestID(report.RequestID) != nil {
		t.Fatalf("report: %+v", report)
	}
	answerRequest(t, run, Answer{RequestID: report.RequestID, Outcome: ReportRecorded, Message: "MMHotline recorded your report at alert severity warn."})
	if event := run.next(t); event.Event != "step" || event.Step.Text != "done" {
		t.Fatalf("a playbook request went up: %+v", event)
	}
	if event := run.next(t); event.Event != "finished" {
		t.Fatalf("final: %+v", event)
	}
	run.finish(t)
	if answer, exit := readCommandResult(t, run.home, "report"); answer != "MMHotline recorded your report at alert severity warn." || exit != "0" {
		t.Fatalf("report command: exit %s, %q", exit, answer)
	}
	if answer, exit := readCommandResult(t, run.home, "playbook"); answer != noSystemsRepositoryMessage || exit != "4" {
		t.Fatalf("playbook command: exit %s, %q", exit, answer)
	}
}

// The agent's decision on a commit goes up as commit_review with the commit,
// the decision, the alert severity, and the report, and the refusal comes
// back to the command with exit code 4.
func TestSandboxRelaysACommitReview(t *testing.T) {
	commit := strings.Repeat("c", 40)
	script := codexSession + `
printf 'Ordinary change\n' > "$HOME/review.md"
` + requestScript(t, "review", `review-commit --commit-id `+commit+` --decision needs_a_person --alert-severity sos --report-file "$HOME/review.md"`) + `
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`
	run := startFakeRun(t, "codex", script, testRequest("codex"))
	_ = run.next(t)
	review := run.next(t)
	if review.Event != "commit_review" || review.CommitID != commit || review.CommitReviewDecision != CommitNeedsAPerson || review.AlertSeverity != "sos" || review.ReportText != "Ordinary change\n" {
		t.Fatalf("review: %+v", review)
	}
	answerRequest(t, run, Answer{RequestID: review.RequestID, Outcome: Refused, Message: "refused"})
	run.nextEvents(t, "finished")
	run.finish(t)
	if answer, exit := readCommandResult(t, run.home, "review"); answer != "refused" || exit != "4" {
		t.Fatalf("review command: exit %s, %q", exit, answer)
	}
}

// The relay sends one request at a time; the next waits until MMHotline
// answered the first.
func TestSandboxRelaysOneRequestAtATime(t *testing.T) {
	script := codexSession + `
printf 'first\n' > "$HOME/first.md"
printf 'second\n' > "$HOME/second.md"
{ ` + requestScript(t, "first", `report --alert-severity info --report-file "$HOME/first.md"`) + `; } &
{ ` + requestScript(t, "second", `report --alert-severity info --report-file "$HOME/second.md"`) + `; } &
wait
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`
	run := startFakeRun(t, "codex", script, testRequest("codex"))
	_ = run.next(t)
	first := run.next(t)
	select {
	case event := <-run.events:
		t.Fatalf("a second request went up before the first was answered: %+v", event)
	case <-time.After(200 * time.Millisecond):
	}
	answerRequest(t, run, Answer{RequestID: first.RequestID, Outcome: ReportRecorded, Message: "recorded " + strings.TrimSpace(first.ReportText)})
	second := run.next(t)
	if second.Event != "report" || second.RequestID == first.RequestID || second.ReportText == first.ReportText {
		t.Fatalf("second: %+v", second)
	}
	answerRequest(t, run, Answer{RequestID: second.RequestID, Outcome: ReportRecorded, Message: "recorded " + strings.TrimSpace(second.ReportText)})
	run.nextEvents(t, "finished")
	run.finish(t)
	for _, name := range []string{"first", "second"} {
		if answer, exit := readCommandResult(t, run.home, name); answer != "recorded "+name || exit != "0" {
			t.Fatalf("%s: exit %s, %q", name, exit, answer)
		}
	}
}

// A stop waits for the agent's current step, and the command waiting for
// MMHotline's answer is that step: the relay ends it with exit 4.
func TestSandboxStopWhileARequestWaits(t *testing.T) {
	script := codexSession + `
printf '# Disk full\n' > "$HOME/report.md"
printf '%s\n' '{"type":"item.started","item":{"id":"task","type":"command_execution","command":"mmhotline triage-agent report"}}'
` + requestScript(t, "report", `report --alert-severity sos --report-file "$HOME/report.md"`) + `
printf '%s\n' '{"type":"item.completed","item":{"id":"task","type":"command_execution"}}'
` + waitForTermination
	run := startFakeRun(t, "codex", script, testRequest("codex"))
	_ = run.next(t)
	run.nextEvents(t, "step", "report")
	if err := json.NewEncoder(run.input).Encode(Control{Stop: true}); err != nil {
		t.Fatal(err)
	}
	if event := run.next(t); event.Event != "stopped" || event.SessionID != "session-one" {
		t.Fatalf("stop: %+v", event)
	}
	run.finish(t)
	if answer, exit := readCommandResult(t, run.home, "report"); answer != runStoppingMessage || exit != "4" {
		t.Fatalf("report command: exit %s, %q", exit, answer)
	}
}
