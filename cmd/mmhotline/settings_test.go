package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmhotline/internal/app"
)

// minimalSettingsForCommandTests is a settings file with only the required
// keys.
const minimalSettingsForCommandTests = `team_name: Example Operations
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
alert_sources: []
`

// runSettingsCapturingOutput runs a settings command in this process and
// returns what it printed. It replaces the standard output, so it must not
// run in parallel with other tests.
func runSettingsCapturingOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	output := make(chan string)
	go func() {
		raw, _ := io.ReadAll(reader)
		output <- string(raw)
	}()
	runErr := runSettings(args)
	os.Stdout = stdout
	writer.Close()
	return <-output, runErr
}

// fakeSettingsSocket answers the settings commands on a Unix socket with
// the next answer of apply, and records the requests.
type fakeSettingsSocket struct {
	mu       sync.Mutex
	requests []string
	applies  []app.SettingsFileView
}

func (fake *fakeSettingsSocket) serve(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "mmhotline-settings-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	socket := filepath.Join(directory, "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request app.CommandRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.requests = append(fake.requests, request.Group+" "+request.Action)
		view := app.SettingsFileView{SettingsFile: app.InstalledSettingsFilePath, Parts: []app.SettingsFilePartOutcome{}}
		if request.Action == "apply" {
			view, fake.applies = fake.applies[0], fake.applies[1:]
		}
		_ = json.NewEncoder(w).Encode(app.CommandResult{SettingsFile: &view})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}

// TestSettingsApplyRestartsMMHotlineForARestartKey: when the service
// answers that a restart is needed, the command restarts it through its
// restart step, waits for the command socket, and asks again.
func TestSettingsApplyRestartsMMHotlineForARestartKey(t *testing.T) {
	applied := app.SettingsFileView{SettingsFile: app.InstalledSettingsFilePath, Parts: []app.SettingsFilePartOutcome{{Part: "team_name", State: "applied"}, {Part: "people", State: "unchanged"}}}
	fake := &fakeSettingsSocket{applies: []app.SettingsFileView{{SettingsFile: app.InstalledSettingsFilePath, RestartNeededFor: []string{"web_url"}, Parts: []app.SettingsFilePartOutcome{}}, applied}}
	socket := fake.serve(t)
	restarts := 0
	previous := restartMMHotline
	restartMMHotline = func(context.Context) error { restarts++; return nil }
	t.Cleanup(func() { restartMMHotline = previous })
	output, err := runSettingsCapturingOutput(t, "apply", "--command-socket", socket)
	if err != nil {
		t.Fatal(err)
	}
	if restarts != 1 || strings.Join(fake.requests, ", ") != "settings apply, settings show, settings apply" {
		t.Fatalf("restarts %d, requests %q", restarts, fake.requests)
	}
	for _, want := range []string{
		"Restarting MMHotline to apply the settings that require a restart: web_url.\n",
		"Restarted MMHotline first, because these keys change only when it restarts: web_url.\n",
		"team_name: applied.\npeople: unchanged.\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}

	// A restart that still leaves the earlier values is a failure.
	fake.applies = []app.SettingsFileView{{RestartNeededFor: []string{"web_url"}}, {RestartNeededFor: []string{"web_url"}}}
	if _, err = runSettingsCapturingOutput(t, "apply", "--command-socket", socket); err == nil || !strings.Contains(err.Error(), "after restarting, MMHotline still uses the previous values for settings web_url") {
		t.Fatalf("a restart that changed nothing: %v", err)
	}
	// A failed restart step stops the command with its reason.
	restartMMHotline = func(context.Context) error { return errors.New("systemctl restart mmhotline.service failed") }
	fake.applies = []app.SettingsFileView{{RestartNeededFor: []string{"web_url"}}}
	if _, err = runSettingsCapturingOutput(t, "apply", "--command-socket", socket); err == nil || !strings.Contains(err.Error(), "systemctl restart mmhotline.service failed") {
		t.Fatalf("a failed restart: %v", err)
	}
}

// TestSettingsApplyWaitsBoundedForTheRestartedService: a service that does
// not answer after its restart ends the wait with a clear failure.
func TestSettingsApplyWaitsBoundedForTheRestartedService(t *testing.T) {
	fake := &fakeSettingsSocket{applies: []app.SettingsFileView{{RestartNeededFor: []string{"web_listen_address"}}}}
	socket := fake.serve(t)
	previous, previousWait := restartMMHotline, settingsRestartWait
	restartMMHotline = func(context.Context) error { return os.Remove(socket) }
	settingsRestartWait = time.Second
	t.Cleanup(func() { restartMMHotline, settingsRestartWait = previous, previousWait })
	_, err := runSettingsCapturingOutput(t, "apply", "--command-socket", socket)
	if err == nil || !strings.Contains(err.Error(), "MMHotline gave no successful answer on its command socket "+socket+" within 1s of the restart") {
		t.Fatalf("wait: %v", err)
	}
}

// TestSettingsApplyExitsOneWhenAPartFailed: the exit code says whether
// every part applied.
func TestSettingsApplyExitsOneWhenAPartFailed(t *testing.T) {
	note := app.SettingsNote{KeyPath: "rules_for_running_agent_playbooks.production", Note: "check_only has no rule; it needs a person until you add one"}
	fake := &fakeSettingsSocket{applies: []app.SettingsFileView{
		{Parts: []app.SettingsFilePartOutcome{{Part: "people", State: "failed", FailureReason: "Bruno Costa owns 1 claimed incident(s)"}}},
		{Problems: []app.SettingsProblem{{KeyPath: "team_name", Problem: "required"}}, Parts: []app.SettingsFilePartOutcome{}},
		{Parts: []app.SettingsFilePartOutcome{{Part: "rules_for_running_agent_playbooks", State: "applied", Notes: []app.SettingsNote{note}}}},
	}}
	socket := fake.serve(t)
	for range 2 {
		_, err := runSettingsCapturingOutput(t, "apply", "--command-socket", socket, "--json")
		var answer checkAnswer
		if !errors.As(err, &answer) || answer.exitCode != 1 {
			t.Fatalf("apply: %v", err)
		}
	}
	// A note, such as a kind of action without a setting, is no failure.
	if output, err := runSettingsCapturingOutput(t, "apply", "--command-socket", socket); err != nil || !strings.Contains(output, "rules_for_running_agent_playbooks: applied.\n  "+note.String()+"\n") {
		t.Fatalf("apply with a note: %q, %v", output, err)
	}
}

func TestSettingsCheckCommand(t *testing.T) {
	directory := t.TempDir()
	good := filepath.Join(directory, "good.yml")
	if err := os.WriteFile(good, []byte(minimalSettingsForCommandTests), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runSettingsCapturingOutput(t, "check", "--settings-file", good)
	if err != nil || output != "The settings file "+good+" has no problems.\n" {
		t.Fatalf("a good file: %q, %v", output, err)
	}
	broken := filepath.Join(directory, "broken.yml")
	text := strings.Replace(minimalSettingsForCommandTests, "team_name: Example Operations\n", "team_nmae: Example Operations\n", 1)
	text = strings.Replace(text, "    role: administrator\n", "    role: owner\n", 1)
	// Readable by every user: settings check skips the owner and mode.
	if err := os.WriteFile(broken, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err = runSettingsCapturingOutput(t, "check", "--settings-file", broken)
	var answer checkAnswer
	if !errors.As(err, &answer) || answer.exitCode != 1 {
		t.Fatalf("a broken file: %v", err)
	}
	want := "The settings file " + broken + " has 4 problems:\nteam_nmae: unknown key\nteam_name: required\npeople[0].role: person role must be administrator or member\npeople: needs at least one person with role administrator, who opens the administration pages\n"
	if output != want {
		t.Fatalf("output:\n%s\nwant:\n%s", output, want)
	}
	output, _ = runSettingsCapturingOutput(t, "check", "--settings-file", broken, "--json")
	var result struct {
		SettingsFile string                `json:"settings_file"`
		Problems     []app.SettingsProblem `json:"problems"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil || result.SettingsFile != broken || len(result.Problems) != 4 || result.Problems[0] != (app.SettingsProblem{KeyPath: "team_nmae", Problem: "unknown key"}) {
		t.Fatalf("JSON %q, %v", output, err)
	}
	for _, args := range [][]string{{"check"}, {"verify"}, {}, {"apply", "extra"}} {
		var usage commandError
		if _, err := runSettingsCapturingOutput(t, args...); !errors.As(err, &usage) {
			t.Errorf("%q: %v", args, err)
		}
	}

	// A kind of action the file leaves out is a note: it prints, and the
	// check passes, so a deploy tool's dry run shows it.
	withNote := filepath.Join(directory, "with-note.yml")
	text = strings.Replace(minimalSettingsForCommandTests, "rules_for_running_agent_playbooks: {}", `rules_for_running_agent_playbooks:
  production:
    action_kinds:
      start_or_restart_a_service: {rule: act_then_report}
      free_disk_space: {rule: act_then_report}
      pull_a_container_image_again: {rule: act_then_report}
      reboot_a_server: {rule: get_permission_then_act, approvals_needed: 1}
      block_a_network_range: {rule: get_permission_then_act, approvals_needed: 1}
      change_dns_records: {rule: get_permission_then_act, approvals_needed: 1}
      unrecognized_task: {rule: get_permission_then_act, approvals_needed: 1}`, 1)
	if err := os.WriteFile(withNote, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	const note = "rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one"
	if output, err = runSettingsCapturingOutput(t, "check", "--settings-file", withNote); err != nil || output != "The settings file "+withNote+" has no problems.\nNotes, which do not stop MMHotline from applying the settings file:\n"+note+"\n" {
		t.Fatalf("a file with a note: %q, %v", output, err)
	}
	output, err = runSettingsCapturingOutput(t, "check", "--settings-file", withNote, "--json")
	var noted struct {
		Problems []app.SettingsProblem `json:"problems"`
		Notes    []app.SettingsNote    `json:"notes"`
	}
	if err != nil || json.Unmarshal([]byte(output), &noted) != nil || len(noted.Problems) != 0 || len(noted.Notes) != 1 || noted.Notes[0] != (app.SettingsNote{KeyPath: "rules_for_running_agent_playbooks.production", Note: "check_only has no rule; it needs a person until you add one"}) {
		t.Fatalf("JSON with a note %q, %v", output, err)
	}
}
