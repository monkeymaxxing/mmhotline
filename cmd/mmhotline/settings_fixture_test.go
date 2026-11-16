package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// processExitCode is the exit code of a finished command: 0 when it
// succeeded, -1 when it did not run.
func processExitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// testSettingsYAML is a small settings file that passes the check: one
// administrator, a weekly rotation, and no optional section.
func testSettingsYAML(webURL, webListenAddress, alertIntakeListenAddress string) string {
	return `# Test settings for the mmhotline command's tests.
team_name: Operations
web_url: ` + webURL + `
web_listen_address: "` + webListenAddress + `"
alert_intake_listen_address: "` + alertIntakeListenAddress + `"
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
  time_zone: UTC
  handoff_hour: 9
  handoff_minute: 0
  handoff_weekday: 1
  first_start_date: "2026-11-02"
  rotation_sign_in_email_addresses: [ana@example.com]
  backup_option: everyone_available
alert_sources: []
`
}

// writeTestSettingsFile writes a settings file as an administrator leaves
// it, readable by its owner and group only, and returns its path.
func writeTestSettingsFile(t *testing.T, path, text string) string {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "mmhotline_settings.yml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}
