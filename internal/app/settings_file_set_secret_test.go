package app

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// Every key that the settings reference marks secret is a settings:"secret"
// key of the Go types, and the other way round. The two lists must not
// drift, because set-secret accepts exactly the types' list.
func TestSettingsSecretKeyPathsAreTheReferencesSecretRows(t *testing.T) {
	raw, err := os.ReadFile("../../docs/reference/settings-file.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `([^`]+)` \\| [^|]*secret[^|]*\\|")
	var fromReference []string
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if heading, found := strings.CutPrefix(line, "### "); found {
			section = heading
		}
		if match := row.FindStringSubmatch(line); match != nil {
			key := strings.ReplaceAll(match[1], "[]", "[ID]")
			if !strings.ContainsAny(key, ".[") {
				key = section + "." + key
			}
			fromReference = append(fromReference, key)
		}
	}
	fromTypes := SettingsSecretKeyPaths()
	slices.Sort(fromReference)
	slices.Sort(fromTypes)
	if !slices.Equal(fromReference, fromTypes) {
		t.Fatalf("secret keys differ.\nreference rows: %v\nGo types:       %v", fromReference, fromTypes)
	}
}

const setSecretFile = `# Written by hand. Comments stay.
team_name: Example Operations
web_url: http://localhost:8480
web_listen_address: 127.0.0.1:8480
alert_intake_listen_address: 127.0.0.1:8484
rules_for_running_agent_playbooks: {}
services_whose_changes_a_person_must_approve: []
sending_services: {}  # none yet
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

const (
	firstHexSecret  = "5f0c2d6e8a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
	secondHexSecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	thirdHexSecret  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestEditSettingsSecretKeepsCommentsAndOrderAndCreatesTheParentSections(t *testing.T) {
	const token = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	edit, problems, err := EditSettingsSecret([]byte(setSecretFile), "sending_services.pushover.application_token", token)
	if err != nil {
		t.Fatal(err)
	}
	// user_key is required too, but only the secret's own key is reported.
	if len(problems) != 0 {
		t.Fatalf("problems at the secret: %v", problems)
	}
	text := string(edit.Text)
	for _, kept := range []string{"# Written by hand. Comments stay.", "# none yet", "# the monitoring tool", "alert_source_id: monitoring"} {
		if !strings.Contains(text, kept) {
			t.Errorf("lost %q:\n%s", kept, text)
		}
	}
	if !strings.Contains(text, "  pushover:\n    application_token: \""+token+"\"") {
		t.Errorf("the secret is not in a new pushover section:\n%s", text)
	}
	if strings.Index(text, "team_name") > strings.Index(text, "web_url") || strings.Index(text, "people:") > strings.Index(text, "alert_sources:") {
		t.Errorf("key order changed:\n%s", text)
	}
}

func TestEditSettingsSecretRefusesWhatIsNotASecretKeyAndNamesTheSecretKeys(t *testing.T) {
	for _, keyPath := range []string{"team_name", "sending_services.pushover.user_key", "alert_sources[].signing_secrets", "triage_agent[x].credential", ""} {
		_, _, err := EditSettingsSecret([]byte(setSecretFile), keyPath, "value-0123")
		if err == nil || !strings.Contains(err.Error(), "is not a secret key") || !strings.Contains(err.Error(), "triage_agent.credential") || !strings.Contains(err.Error(), "alert_sources[ID].signing_secrets") {
			t.Errorf("%q: err=%v", keyPath, err)
		}
	}
}

func TestEditSettingsSecretRefusesAnUnknownAlertSourceAndNamesTheKnownOnes(t *testing.T) {
	_, _, err := EditSettingsSecret([]byte(setSecretFile), "alert_sources[nope].signing_secrets", firstHexSecret)
	if err == nil || !strings.Contains(err.Error(), "no alert source with the ID nope") || !strings.Contains(err.Error(), "monitoring") {
		t.Fatalf("err=%v", err)
	}
}

func TestEditSettingsSecretReportsTheKeysOwnProblemWithoutTheValue(t *testing.T) {
	const bad = "Zq7-not-a-pushover-token"
	_, problems, err := EditSettingsSecret([]byte(setSecretFile), "sending_services.pushover.application_token", bad)
	if err != nil || len(problems) != 1 || problems[0].KeyPath != "sending_services.pushover.application_token" {
		t.Fatalf("problems=%v err=%v", problems, err)
	}
	_, problems, _ = EditSettingsSecret([]byte(setSecretFile), "alert_sources[monitoring].signing_secrets", "Zq7-short")
	if len(problems) != 1 || problems[0].KeyPath != "alert_sources[monitoring].signing_secrets[0]" || strings.Contains(problems[0].String(), "Zq7") {
		t.Fatalf("problems=%v", problems)
	}
}

func TestEditSettingsSecretReplacesThePlaceholderThenKeepsTheEarlierSigningSecret(t *testing.T) {
	first, problems, err := EditSettingsSecret([]byte(setSecretFile), "alert_sources[monitoring].signing_secrets", firstHexSecret)
	if err != nil || len(problems) != 0 || first.KeptEarlierSigningSecret {
		t.Fatalf("edit=%+v problems=%v err=%v", first, problems, err)
	}
	if strings.Contains(string(first.Text), "YOUR_64") || !strings.Contains(string(first.Text), `signing_secrets: ["`+firstHexSecret+`"]`) {
		t.Fatalf("placeholder not replaced:\n%s", first.Text)
	}
	second, problems, err := EditSettingsSecret(first.Text, "alert_sources[monitoring].signing_secrets", secondHexSecret)
	if err != nil || len(problems) != 0 || !second.KeptEarlierSigningSecret || !strings.Contains(string(second.Text), `["`+firstHexSecret+`", "`+secondHexSecret+`"]`) {
		t.Fatalf("edit=%s problems=%v err=%v", second.Text, problems, err)
	}
	third, problems, err := EditSettingsSecret(second.Text, "alert_sources[monitoring].signing_secrets", thirdHexSecret)
	if err != nil || len(problems) != 0 || !strings.Contains(string(third.Text), `["`+secondHexSecret+`", "`+thirdHexSecret+`"]`) || strings.Contains(string(third.Text), firstHexSecret) {
		t.Fatalf("edit=%s problems=%v err=%v", third.Text, problems, err)
	}
}

func TestWriteSettingsFileTextKeepsTheModeAndLeavesNoTemporaryFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "mmhotline_settings.yml")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := WriteSettingsFileText(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	entries, _ := os.ReadDir(directory)
	content, _ := os.ReadFile(path)
	if string(content) != "new" || after.Mode().Perm() != 0o640 || after.Sys().(*syscall.Stat_t).Uid != before.Sys().(*syscall.Stat_t).Uid || len(entries) != 1 {
		t.Fatalf("content=%q mode=%v entries=%d", content, after.Mode(), len(entries))
	}
}
