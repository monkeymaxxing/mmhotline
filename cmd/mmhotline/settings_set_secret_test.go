package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const setSecretSourceFile = `# Written by hand. Comments stay.
team_name: Example Operations
web_url: http://localhost:8480
web_listen_address: 127.0.0.1:8480
alert_intake_listen_address: 127.0.0.1:8484
rules_for_running_agent_playbooks: {}
services_whose_changes_a_person_must_approve: []
sending_services: {}
people:
  - name: Ana Silva
    sign_in_email_address: ana@example.com
    role: administrator
on_call_schedule:
  escalate_if_unclaimed_after_minutes: 5
  schedule_type: weekly_rotation
  time_zone: America/New_York
  handoff_hour: 9
  handoff_minute: 0
  handoff_weekday: 1
  first_start_date: "2026-11-02"
  rotation_sign_in_email_addresses: [ana@example.com]
  backup_option: everyone_available
alert_sources:
  # the monitoring tool
  - alert_source_id: monitoring
    name: Monitoring
    signing_secrets: ["YOUR_64_HEXADECIMAL_CHARACTER_SIGNING_SECRET"]
    warn_after_no_check_in_for_minutes: 2
    sos_after_no_check_in_for_minutes: 5
`

type setSecretRun struct {
	stdout, prompt bytes.Buffer
	applied        bool
	err            error
}

// runSetSecret runs set-secret on a new settings file with mode 0640 and
// standard input holding input. The settings file's path is the first
// returned value.
func runSetSecret(t *testing.T, stdoutIsTerminal bool, input string, args ...string) (string, *setSecretRun) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "mmhotline_settings.yml")
	if err := os.WriteFile(path, []byte(setSecretSourceFile), 0o640); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o640)
	run := &setSecretRun{}
	run.err = settingsSetSecret(append(args, "--settings-file", path), setSecretTerminalsFor(t, run, stdoutIsTerminal, input))
	return path, run
}

func setSecretTerminalsFor(t *testing.T, run *setSecretRun, stdoutIsTerminal bool, input string) setSecretTerminals {
	return setSecretTerminals{Stdin: closedPipeWith(t, input), Stdout: &run.stdout, Prompt: &run.prompt, StdoutIsTerminal: stdoutIsTerminal,
		TurnOffEcho: func(*os.File) (func(), bool) { return nil, false },
		Apply:       func() error { run.applied = true; return nil }}
}

func (run *setSecretRun) everything() string {
	text := run.stdout.String() + run.prompt.String()
	if run.err != nil {
		text += run.err.Error()
	}
	return text
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSetSecretWritesTheValueKeepsCommentsAndModeAndNeverShowsTheValue(t *testing.T) {
	const token = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	path, run := runSetSecret(t, false, token+"\n", "sending_services.pushover.application_token")
	if run.err != nil {
		t.Fatal(run.err)
	}
	text := readFile(t, path)
	info, _ := os.Stat(path)
	if !strings.Contains(text, `application_token: "`+token+`"`) || !strings.Contains(text, "# Written by hand. Comments stay.") || !strings.Contains(text, "# the monitoring tool") {
		t.Fatalf("settings file:\n%s", text)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", info.Mode())
	}
	if strings.Contains(run.everything(), token) || strings.Contains(run.everything(), token[:12]) {
		t.Fatalf("the value is in the output: %s", run.everything())
	}
	// user_key is still missing, so the file cannot be applied yet.
	if !strings.Contains(run.stdout.String(), "Saved sending_services.pushover.application_token in "+path+".") || !strings.Contains(run.stdout.String(), "still has problems") || run.applied {
		t.Fatalf("output: %s", run.stdout.String())
	}
}

func TestSetSecretOnAFileWithNoOtherProblemsSaysToApplyOrAppliesWithApply(t *testing.T) {
	_, run := runSetSecret(t, false, exampleSecret+"\n", "alert_sources[monitoring].signing_secrets")
	if run.err != nil || !strings.Contains(run.stdout.String(), " has no problems.\nRun: sudo mmhotline settings apply\n") || run.applied {
		t.Fatalf("err=%v output=%s", run.err, run.stdout.String())
	}
	_, run = runSetSecret(t, false, exampleSecret+"\n", "alert_sources[monitoring].signing_secrets", "--apply")
	if run.err != nil || !run.applied || strings.Contains(run.stdout.String(), "Run: sudo") {
		t.Fatalf("err=%v applied=%t output=%s", run.err, run.applied, run.stdout.String())
	}
}

const exampleSecret = "5f0c2d6e8a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"

func TestSetSecretRefusesAndChangesNothing(t *testing.T) {
	for _, test := range []struct {
		name, input    string
		stdoutTerminal bool
		args           []string
		message        string
	}{
		{"not a secret key", "x\n", false, []string{"team_name"}, "is not a secret key"},
		{"unknown alert source", exampleSecret + "\n", false, []string{"alert_sources[nope].signing_secrets"}, "no alert source with the ID nope"},
		{"invalid value", "not-hex-Zq7\n", false, []string{"alert_sources[monitoring].signing_secrets"}, "must be 64 hexadecimal characters"},
		{"empty value", "\n", false, []string{"alert_sources[monitoring].signing_secrets"}, "the value is empty"},
		{"make-new on another key", "", true, []string{"sending_services.pushover.application_token", "--make-new"}, "--make-new works only for a signing secret"},
		{"make-new without a terminal", "", false, []string{"alert_sources[monitoring].signing_secrets", "--make-new"}, "only on a person's terminal"},
		{"write-to-file without make-new", exampleSecret + "\n", false, []string{"alert_sources[monitoring].signing_secrets", "--write-to-file", "x"}, "works only with --make-new"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "mmhotline_settings.yml")
			os.WriteFile(path, []byte(setSecretSourceFile), 0o640)
			run := &setSecretRun{}
			run.err = settingsSetSecret(append(test.args, "--settings-file", path), setSecretTerminalsFor(t, run, test.stdoutTerminal, test.input))
			if run.err == nil || !strings.Contains(run.err.Error(), test.message) {
				t.Fatalf("err=%v", run.err)
			}
			if readFile(t, path) != setSecretSourceFile {
				t.Fatal("the settings file changed")
			}
			entries, _ := os.ReadDir(directory)
			if len(entries) != 1 {
				t.Fatalf("files left: %v", entries)
			}
			for _, secret := range []string{"not-hex-Zq7", exampleSecret} {
				if strings.Contains(run.everything(), secret) {
					t.Fatalf("the value is in the output: %s", run.everything())
				}
			}
		})
	}
}

var signingSecretLine = regexp.MustCompile(`(?m)^[0-9a-f]{64}$`)

func TestSetSecretMakeNewOnATerminalShowsTheValueOnceOnItsOwnLine(t *testing.T) {
	path, run := runSetSecret(t, true, "", "alert_sources[monitoring].signing_secrets", "--make-new")
	if run.err != nil {
		t.Fatal(run.err)
	}
	lines := signingSecretLine.FindAllString(run.stdout.String(), -1)
	if len(lines) != 1 {
		t.Fatalf("output: %s", run.stdout.String())
	}
	if !strings.Contains(run.stdout.String(), "Copy this new signing secret into the monitoring tool's setting now. MMHotline will not show it again:\n"+lines[0]+"\n") {
		t.Fatalf("output: %s", run.stdout.String())
	}
	if !strings.Contains(readFile(t, path), `signing_secrets: ["`+lines[0]+`"]`) {
		t.Fatal("the new secret is not in the settings file")
	}
}

func TestSetSecretMakeNewWriteToFileWritesRootOnlyFileRefusesAnExistingOneAndNeverShowsTheValue(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mmhotline_signing_secret")
	path, run := runSetSecret(t, false, "", "alert_sources[monitoring].signing_secrets", "--make-new", "--write-to-file", target)
	if run.err != nil {
		t.Fatal(run.err)
	}
	written := readFile(t, target)
	info, _ := os.Stat(target)
	if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).MatchString(written) || info.Mode().Perm() != 0o600 {
		t.Fatalf("file %q mode %v", written, info.Mode())
	}
	secret := strings.TrimSpace(written)
	if !strings.Contains(readFile(t, path), secret) {
		t.Fatal("the settings file does not hold the same secret")
	}
	if strings.Contains(run.everything(), secret) || !strings.Contains(run.stdout.String(), target) || !strings.Contains(run.stdout.String(), "mmhotline_signing_secret_file") {
		t.Fatalf("output: %s", run.stdout.String())
	}
	// A second run finds the file and refuses before it changes anything.
	before := readFile(t, path)
	again := &setSecretRun{}
	again.err = settingsSetSecret([]string{"alert_sources[monitoring].signing_secrets", "--make-new", "--write-to-file", target, "--settings-file", path}, setSecretTerminalsFor(t, again, false, ""))
	if again.err == nil || !strings.Contains(again.err.Error(), "already exists") || readFile(t, path) != before || readFile(t, target) != written {
		t.Fatalf("err=%v", again.err)
	}
}

// closedPipeWith is standard input that holds text and then ends, as a
// pipe does when the writer is done.
func closedPipeWith(t *testing.T, text string) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	writer.WriteString(text)
	writer.Close()
	t.Cleanup(func() { reader.Close() })
	return reader
}
