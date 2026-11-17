package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// exampleSettingsYAML is the stack's initial setup, as the settings file schema
// shows it, with made-up values in place of the vault's.
const exampleSettingsYAML = `# Managed by MMX from mmhotline_settings.yml in the systems repository. Change it there and push; util01 applies it.
team_name: Example Operations
web_url: https://hotline.example.com
web_listen_address: 127.0.0.1:8480
alert_intake_listen_address: 10.20.0.1:8484
cloudflare_access:
  team_domain: example.cloudflareaccess.com
  audience_tag: 0123456789abcdef
fleet_view_command: [/usr/local/bin/MONITORING_TOOL, fleet, --json]

systems_repository_commits:
  trusted_commits: signed_by_a_trusted_key_or_reviewed
  trusted_commit_signing_public_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKkV3VQGyLmAYtl/HR7x93Qw22ugM6k9OhTepuWlHFPr Barbara, a person the team trusts"]
  commits_without_a_trusted_signature_are_approved_by: a_person_or_the_triage_agent
  paths_only_a_person_may_approve: [mmhotline_settings.yml, triage_agent_rules.md, AGENTS.md, roles/every_server/files/]

triage_agent:
  commit_signing_public_key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGUdoc+44+a4QWDPzegMsM38OwWeeUot9ceymJ5D1H0i the triage agent"
  coding_agent: claude_code
  credential_type: subscription_token
  credential: "sk-ant-oat01-example-triage-agent-credential"
  model: opus
  reasoning_effort: high
  internet_access: listed_hosts_only
  listed_hosts: []
  software_review_interval_hours: 24
  sos_waits_for_the_agent_minutes: 5
  warn_waits_for_the_agent_minutes: 3
  sos_waits_for_a_server_replacement_minutes: 10

review_agent:
  coding_agent: codex
  credential_type: api_key
  credential: "sk-example-review-agent-credential"
  model: gpt-5-codex
  reasoning_effort: high

rules_for_running_agent_playbooks:
  production:
    preset: routine_fixes_without_permission
services_whose_changes_a_person_must_approve: [pgautofailover.service]

sending_services:
  pushover:
    application_token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi"

mmhotline_down_alarm:
  cloudflare_account_id: 0123456789abcdef0123456789abcdef
  cloudflare_api_token: "example-cloudflare-workers-token"
  wake_the_person_on_call_after_minutes_without_a_check_in: 10
  wake_the_backup_after_minutes_without_a_check_in: 20

status_page:
  page_title: Example Inc.
  host_name: status.example.com
  r2_bucket_url: https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/status
  r2_access_key_id: "0123456789abcdef0123456789abcdef"
  r2_secret_access_key: "example-r2-secret-access-key"

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
  - alert_source_id: monitoring
    name: Monitoring
    signing_secrets: ["5f0c2d6e8a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"]
    warn_after_no_check_in_for_minutes: 2
    sos_after_no_check_in_for_minutes: 5
`

// writeSettingsFile writes a settings file as an administrator would leave
// it: readable by its owner and group only.
func writeSettingsFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mmhotline_settings.yml")
	if err := os.WriteFile(path, []byte(text), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func problemTexts(problems []SettingsProblem) []string {
	var texts []string
	for _, problem := range problems {
		texts = append(texts, problem.String())
	}
	return texts
}

func TestTheExampleSettingsFileHasNoProblems(t *testing.T) {
	file, problems := ReadSettingsFile(writeSettingsFile(t, exampleSettingsYAML), nil)
	if len(problems) != 0 {
		t.Fatalf("problems in the example: %q", problemTexts(problems))
	}
	if file.FirstCommentLine != "Managed by MMX from mmhotline_settings.yml in the systems repository. Change it there and push; util01 applies it." {
		t.Fatalf("first comment line %q", file.FirstCommentLine)
	}
	s := file.Settings
	if s.TeamName != "Example Operations" || s.TriageAgent.Model != "opus" || *s.OnCallSchedule.FirstStartDate != "2026-11-02" || s.AlertSources[0].WarnAfterNoCheckInForMinutes != "2" || !slices.Equal(s.FleetViewCommand, []string{"/usr/local/bin/MONITORING_TOOL", "fleet", "--json"}) {
		t.Fatalf("read %+v", s)
	}
}

// TestEveryCommitOnOriginNeedsNoSigningKeys checks the good forms of
// systems_repository_commits: every_commit_on_origin with its three keys and
// the triage agent's key left out, and empty lists with
// signed_by_a_trusted_key_or_reviewed, which still count as present.
func TestEveryCommitOnOriginNeedsNoSigningKeys(t *testing.T) {
	for name, yaml := range map[string]string{
		"every commit on origin": everyCommitOnOriginSettingsYAML(),
		"empty lists with signatures": strings.NewReplacer(
			`trusted_commit_signing_public_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKkV3VQGyLmAYtl/HR7x93Qw22ugM6k9OhTepuWlHFPr Barbara, a person the team trusts"]`, "trusted_commit_signing_public_keys: []",
			"paths_only_a_person_may_approve: [mmhotline_settings.yml, triage_agent_rules.md, AGENTS.md, roles/every_server/files/]", "paths_only_a_person_may_approve: []").Replace(exampleSettingsYAML),
	} {
		if _, problems := ReadSettingsFile(writeSettingsFile(t, yaml), nil); len(problems) != 0 {
			t.Errorf("%s: %q", name, problemTexts(problems))
		}
	}
}

// The systems_repository_commits section stands on its own: without the
// triage_agent section, signatures need no triage agent key, and only a
// person may approve a commit no trusted key signed. Without the section,
// the triage agent's own key may stay, so a lock-out removes one section.
func TestSystemsRepositoryCommitsWithoutTheTriageAgent(t *testing.T) {
	withoutTriageAgent := withoutSection(t, withoutSection(t, exampleSettingsYAML, "triage_agent"), "review_agent")
	for name, yaml := range map[string]string{
		"signatures, approved by a person": strings.Replace(withoutTriageAgent, "approved_by: a_person_or_the_triage_agent", "approved_by: a_person", 1),
		"every commit on origin":           withoutSection(t, withoutSection(t, everyCommitOnOriginSettingsYAML(), "triage_agent"), "review_agent"),
		"no section, the agent's key kept": withoutSection(t, exampleSettingsYAML, "systems_repository_commits"),
	} {
		if _, problems := ReadSettingsFile(writeSettingsFile(t, yaml), nil); len(problems) != 0 {
			t.Errorf("%s: %q", name, problemTexts(problems))
		}
	}
	_, problems := ReadSettingsFile(writeSettingsFile(t, withoutTriageAgent), nil)
	if got, want := problemTexts(problems), []string{"systems_repository_commits.commits_without_a_trusted_signature_are_approved_by: a_person_or_the_triage_agent needs the triage_agent section: without it, use a_person"}; !slices.Equal(got, want) {
		t.Errorf("the triage agent approves without its section: %q, want %q", got, want)
	}
}

// TestEachSettingsProblemNamesItsKeyPath changes the example in one place
// and checks the problems the check reports, each with its key path.
func TestEachSettingsProblemNamesItsKeyPath(t *testing.T) {
	for _, test := range []struct {
		name, from, to string
		want           []string
	}{
		{"unknown key", "  reasoning_effort: high\n  internet_access", "  reasoning_effort: high\n  reasoning_efort: high\n  internet_access",
			[]string{"triage_agent.reasoning_efort: unknown key"}},
		{"unknown top-level key", "team_name: Example Operations\n", "team_name: Example Operations\nteam_nmae: Example\n",
			[]string{"team_nmae: unknown key"}},
		{"renamed keys", "rules_for_running_agent_playbooks:\n  production:\n    preset: routine_fixes_without_permission\nservices_whose_changes_a_person_must_approve:",
			"trust_settings:\n  production:\n    action_kinds:\n      check_only: {trust_setting: act_then_report}\nservices_that_always_need_a_person:",
			[]string{"trust_settings: unknown key; it was renamed to rules_for_running_agent_playbooks", "services_that_always_need_a_person: unknown key; it was renamed to services_whose_changes_a_person_must_approve", "rules_for_running_agent_playbooks: required", "services_whose_changes_a_person_must_approve: required"}},
		{"renamed key of a kind of action", "    preset: routine_fixes_without_permission", "    action_kinds:\n      check_only: {trust_setting: act_then_report}",
			[]string{"rules_for_running_agent_playbooks.production.action_kinds.check_only.trust_setting: unknown key; it was renamed to rule", "rules_for_running_agent_playbooks.production.action_kinds.check_only.rule: required", "rules_for_running_agent_playbooks.production.action_kinds.check_only: rule must be get_permission_then_act, warn_wait_then_act, or act_then_report"}},
		{"renamed alarm section", "\nmmhotline_down_alarm:\n", "\nlifeline:\n",
			[]string{"lifeline: unknown key; it was renamed to mmhotline_down_alarm"}},
		{"renamed schedule key", "escalate_if_unclaimed_after_minutes: 5", "response_window_minutes: 5",
			[]string{"on_call_schedule.response_window_minutes: unknown key; it was renamed to escalate_if_unclaimed_after_minutes", "on_call_schedule.escalate_if_unclaimed_after_minutes: required"}},
		{"missing key", "  reasoning_effort: high\n  internet_access", "  internet_access",
			[]string{"triage_agent.reasoning_effort: required"}},
		{"a key twice", "team_name: Example Operations\n", "team_name: Example Operations\nteam_name: Other\n",
			[]string{"team_name: appears twice"}},
		{"no value", "team_name: Example Operations", "team_name:",
			[]string{"team_name: has no value"}},
		{"not a whole number", "  handoff_hour: 9", "  handoff_hour: nine",
			[]string{"on_call_schedule.handoff_hour: must be a whole number"}},
		{"not a list", "  listed_hosts: []", "  listed_hosts: github.com",
			[]string{"triage_agent.listed_hosts: must be a list, such as [a, b], or [] for none"}},
		{"not a section", "rules_for_running_agent_playbooks:\n  production:\n    preset: routine_fixes_without_permission", "rules_for_running_agent_playbooks: production",
			[]string{"rules_for_running_agent_playbooks: must be a section of keys, each on its own line as key: value"}},
		{"secret on two lines", `credential: "sk-ant-oat01-example-triage-agent-credential"`, `credential: "first line\nsecond line"`,
			[]string{"triage_agent.credential: credential must be one line without control characters"}},
		{"Pushover token", `application_token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi"`, `application_token: "short"`,
			[]string{"sending_services.pushover.application_token: must be one line of exactly 30 letters and digits, as Pushover shows the application's API token"}},
		{"unknown Codex credential type", "  credential_type: api_key\n  credential: \"sk-example-review-agent-credential\"", "  credential_type: subscription_sign_in\n  credential: \"\"",
			[]string{"review_agent.credential_type: must be api_key or access_token or chatgpt_sign_in for codex"}},
		{"credential type of another coding agent", "  credential_type: api_key\n  credential: \"sk-example-review", "  credential_type: subscription_token\n  credential: \"sk-example-review",
			[]string{"review_agent.credential_type: must be api_key or access_token or chatgpt_sign_in for codex"}},
		{"access token without its end date", "  credential_type: api_key\n  credential: \"sk-example-review", "  credential_type: access_token\n  credential: \"at-example-review",
			[]string{"review_agent.credential_expires_on: is required with credential_type access_token: the token's end date, such as 2027-02-06, as ChatGPT showed it when you made the token"}},
		{"access token end date not a date", "  credential_type: api_key\n  credential: \"sk-example-review", "  credential_type: access_token\n  credential_expires_on: 6 February 2027\n  credential: \"at-example-review",
			[]string{"review_agent.credential_expires_on: must be a date such as 2027-02-06"}},
		{"end date with an API key", "  credential_type: api_key\n", "  credential_type: api_key\n  credential_expires_on: 2027-02-06\n",
			[]string{"review_agent.credential_expires_on: is only for credential_type access_token; MMHotline knows when a subscription token ends, and an API key or a ChatGPT sign-in has no end date"}},
		{"ChatGPT sign-in that is not auth.json", "  credential_type: api_key\n  credential: \"sk-example-review-agent-credential\"", "  credential_type: chatgpt_sign_in\n  credential: \"sk-example-review-agent-credential\"",
			[]string{"review_agent.credential: credential must be the whole content of Codex's auth.json after codex login, but it is not one JSON object with the shape of Codex's auth.json"}},
		{"missing commit signing public key", "  commit_signing_public_key: \"" + testTriageAgentCommitSigningPublicKey + "\"\n", "",
			[]string{"triage_agent.commit_signing_public_key: required with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed, so MMHotline knows the triage agent's commits"}},
		{"bad commit signing public key", "  commit_signing_public_key: ", "  commit_signing_public_key: invalid # ",
			[]string{"triage_agent.commit_signing_public_key: must be one OpenSSH public key line: the key type, such as ssh-ed25519, then the key, then an optional comment"}},
		{"missing trusted commit signing public keys", "  trusted_commit_signing_public_keys: [\"" + testTrustedCommitSigningPublicKey + "\"]\n", "",
			[]string{"systems_repository_commits.trusted_commit_signing_public_keys: required"}},
		{"trusted keys with a key cut short and the triage agent's key", "  trusted_commit_signing_public_keys: [", "  trusted_commit_signing_public_keys: [\"ssh-ed25519 AAAA\", \"" + testTriageAgentCommitSigningPublicKey + "\", ",
			[]string{"systems_repository_commits.trusted_commit_signing_public_keys[0]: has a key that is cut short: copy the whole line of the .pub file",
				"systems_repository_commits.trusted_commit_signing_public_keys[1]: is the triage agent's key, triage_agent.commit_signing_public_key; list only the keys of the team's people and machines, because the triage agent's commits may change only one_time_playbooks/ and triage_agent_notes/"}},
		{"trusted commits with another value", "  trusted_commits: signed_by_a_trusted_key_or_reviewed\n", "  trusted_commits: every_commit\n",
			[]string{"systems_repository_commits.trusted_commits: must be signed_by_a_trusted_key_or_reviewed or every_commit_on_origin"}},
		{"trusted commits left out", "  trusted_commits: signed_by_a_trusted_key_or_reviewed\n", "",
			[]string{"systems_repository_commits.trusted_commits: required"}},
		{"missing approval and paths with signatures", "  commits_without_a_trusted_signature_are_approved_by: a_person_or_the_triage_agent\n  paths_only_a_person_may_approve: [mmhotline_settings.yml, triage_agent_rules.md, AGENTS.md, roles/every_server/files/]\n", "",
			[]string{"systems_repository_commits.commits_without_a_trusted_signature_are_approved_by: required", "systems_repository_commits.paths_only_a_person_may_approve: required"}},
		{"every commit on origin with the four keys", "  trusted_commits: signed_by_a_trusted_key_or_reviewed\n", "  trusted_commits: every_commit_on_origin\n",
			[]string{"systems_repository_commits.trusted_commit_signing_public_keys: used only with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed; remove it",
				"systems_repository_commits.commits_without_a_trusted_signature_are_approved_by: used only with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed; remove it",
				"systems_repository_commits.paths_only_a_person_may_approve: used only with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed; remove it",
				"triage_agent.commit_signing_public_key: used only with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed; remove it"}},
		{"keys moved out of triage_agent", "  coding_agent: claude_code\n", "  trusted_commits: every_commit_on_origin\n  trusted_commit_signing_public_keys: []\n  commits_without_a_trusted_signature_are_approved_by: a_person\n  paths_only_a_person_may_approve: []\n  coding_agent: claude_code\n",
			[]string{"triage_agent.trusted_commits: moved to systems_repository_commits.trusted_commits",
				"triage_agent.trusted_commit_signing_public_keys: moved to systems_repository_commits.trusted_commit_signing_public_keys",
				"triage_agent.commits_without_a_trusted_signature_are_approved_by: moved to systems_repository_commits.commits_without_a_trusted_signature_are_approved_by",
				"triage_agent.paths_only_a_person_may_approve: moved to systems_repository_commits.paths_only_a_person_may_approve"}},
		{"approval by another", "approved_by: a_person_or_the_triage_agent", "approved_by: the_review_agent",
			[]string{"systems_repository_commits.commits_without_a_trusted_signature_are_approved_by: must be a_person or a_person_or_the_triage_agent"}},
		{"missing paths only a person may approve", "  paths_only_a_person_may_approve: [mmhotline_settings.yml, triage_agent_rules.md, AGENTS.md, roles/every_server/files/]\n", "",
			[]string{"systems_repository_commits.paths_only_a_person_may_approve: required"}},
		{"paths only a person may approve", "may_approve: [mmhotline_settings.yml,", "may_approve: [/etc/sudoers, roles/../AGENTS.md, AGENTS.md, mmhotline_settings.yml,",
			[]string{"systems_repository_commits.paths_only_a_person_may_approve[0]: \"/etc/sudoers\" must be relative to the systems repository, as Git names the path, such as roles/every_server/files/sudoers_triage",
				"systems_repository_commits.paths_only_a_person_may_approve[1]: \"roles/../AGENTS.md\" must not have an empty, \".\", or \"..\" part, because Git never names a path so",
				"systems_repository_commits.paths_only_a_person_may_approve[5]: AGENTS.md is listed twice"}},
		{"unknown coding agent", "  coding_agent: codex", "  coding_agent: gemini",
			[]string{"review_agent.coding_agent: must be claude_code or codex"}},
		{"model with a space (CA6)", "  model: opus", "  model: claude opus",
			[]string{"triage_agent.model: must be a model name of at most 200 characters without spaces, such as opus; the coding agent's setup test checks whether the service recognizes the model name"}},
		{"reasoning effort not one word (CA6)", "  reasoning_effort: high\n  internet_access", "  reasoning_effort: Very high\n  internet_access",
			[]string{"triage_agent.reasoning_effort: must be a lowercase word, such as high; the coding agent's setup test checks whether the model supports the reasoning effort"}},
		{"internet access", "internet_access: listed_hosts_only", "internet_access: everywhere",
			[]string{"triage_agent.internet_access: must be listed_hosts_only or any_site"}},
		{"listed host", "  listed_hosts: []", "  listed_hosts: [{host: github.com, port: 443}, {host: GitHub.com, port: 443}, {host: 127.0.0.1, port: 22}, {host: 10.0.0.1, port: 22}]",
			[]string{"triage_agent.listed_hosts[1]: github.com:443 is listed twice", "triage_agent.listed_hosts[2]: Loopback addresses are never allowed."}},
		{"review interval", "software_review_interval_hours: 24", "software_review_interval_hours: 0",
			[]string{"triage_agent.software_review_interval_hours: must be off, or a whole number of hours from 1 to 8760"}},
		{"review interval not off or hours", "software_review_interval_hours: 24", "software_review_interval_hours: daily",
			[]string{"triage_agent.software_review_interval_hours: must be off, or a whole number of hours from 1 to 8760"}},
		{"SOS wait for the agent", "sos_waits_for_the_agent_minutes: 5", "sos_waits_for_the_agent_minutes: 16",
			[]string{"triage_agent.sos_waits_for_the_agent_minutes: must be a whole number of minutes from 0 to 15: 0 wakes the person on call at once, and a stuck or wrong agent must not hold an SOS longer than 15 minutes"}},
		{"warn wait for the agent", "warn_waits_for_the_agent_minutes: 3", "warn_waits_for_the_agent_minutes: 16",
			[]string{"triage_agent.warn_waits_for_the_agent_minutes: must be a whole number of minutes from 0 to 15: 0 holds no warning, and a stuck or wrong agent must not hold a warning longer than 15 minutes"}},
		{"SOS wait for a server replacement", "sos_waits_for_a_server_replacement_minutes: 10", "sos_waits_for_a_server_replacement_minutes: 61",
			[]string{"triage_agent.sos_waits_for_a_server_replacement_minutes: must be a whole number of minutes from 0 to 60: 0 turns the longer wait off, and an MMX run longer than its one-hour limit cannot finish a replacement"}},
		{"MMHotline-down alarm wake too soon", "wake_the_person_on_call_after_minutes_without_a_check_in: 10", "wake_the_person_on_call_after_minutes_without_a_check_in: 2",
			[]string{"mmhotline_down_alarm.wake_the_person_on_call_after_minutes_without_a_check_in: must be a whole number of minutes from 3 to 60: MMHotline checks in every minute and at once when it starts, so a shorter wait would wake someone for a server reboot, and a longer one would leave a stopped MMHotline unnoticed for over an hour"}},
		{"MMHotline-down alarm wake too late", "wake_the_person_on_call_after_minutes_without_a_check_in: 10", "wake_the_person_on_call_after_minutes_without_a_check_in: 61",
			[]string{"mmhotline_down_alarm.wake_the_person_on_call_after_minutes_without_a_check_in: must be a whole number of minutes from 3 to 60: MMHotline checks in every minute and at once when it starts, so a shorter wait would wake someone for a server reboot, and a longer one would leave a stopped MMHotline unnoticed for over an hour"}},
		{"MMHotline-down alarm backup too close", "wake_the_backup_after_minutes_without_a_check_in: 20", "wake_the_backup_after_minutes_without_a_check_in: 14",
			[]string{"mmhotline_down_alarm.wake_the_backup_after_minutes_without_a_check_in: must be a whole number of minutes, at least 5 more than wake_the_person_on_call_after_minutes_without_a_check_in (10), so the person on call has time to answer before the backup is woken"}},
		{"MMHotline-down alarm wake left out", "  wake_the_backup_after_minutes_without_a_check_in: 20\n", "",
			[]string{"mmhotline_down_alarm.wake_the_backup_after_minutes_without_a_check_in: required"}},
		{"web URL", "web_url: https://hotline.example.com", "web_url: http://hotline.example.com",
			[]string{"web_url: must be an HTTPS origin, such as https://mmhotline.example.com, or http://localhost:PORT or http://127.0.0.1:PORT"}},
		{"listen address", "web_listen_address: 127.0.0.1:8480", "web_listen_address: 127.0.0.1",
			[]string{"web_listen_address: must be a host:port address, such as 127.0.0.1:8480"}},
		{"Cloudflare Access", "audience_tag: 0123456789abcdef", "audience_tag: abc-123",
			[]string{"cloudflare_access.audience_tag: the Cloudflare Access audience tag must contain only ASCII letters and digits"}},
		{"fleet view command", "fleet_view_command: [/usr/local/bin/MONITORING_TOOL", "fleet_view_command: [MONITORING_TOOL",
			[]string{"fleet_view_command[0]: must be the program's absolute path, such as /usr/local/bin/PROGRAM"}},
		{"rules for running the agent's playbooks with both", "    preset: routine_fixes_without_permission", "    preset: routine_fixes_without_permission\n    action_kinds: {}",
			[]string{"rules_for_running_agent_playbooks.production: has preset and action_kinds. Specify only preset or only action_kinds"}},
		{"rules for running the agent's playbooks preset", "preset: routine_fixes_without_permission", "preset: routine",
			[]string{"rules_for_running_agent_playbooks.production.preset: must be permission_for_every_fix, routine_fixes_without_permission, or every_fix_without_permission"}},
		{"rules for running the agent's playbooks kinds", "    preset: routine_fixes_without_permission", `    action_kinds:
      start_or_restart_a_service: {rule: warn_wait_then_act}
      free_disk_space: {rule: act_then_report}
      pull_a_container_image_again: {rule: act_then_report, approvals_needed: 1}
      reboot_a_server: {rule: get_permission_then_act, approvals_needed: 2}
      block_a_network_range: {rule: get_permission_then_act, approvals_needed: 1}
      change_dns_records: {rule: get_permission_then_act, approvals_needed: 1}
      check_only: {rule: act_then_report}
      restart_everything: {rule: act_then_report}`,
			[]string{
				"rules_for_running_agent_playbooks.production.action_kinds.restart_everything: unknown kind of action; the kinds are start_or_restart_a_service, free_disk_space, pull_a_container_image_again, reboot_a_server, block_a_network_range, change_dns_records, check_only, unrecognized_task",
				"rules_for_running_agent_playbooks.production.action_kinds.start_or_restart_a_service: warning_wait_minutes must be 1 to 1440 with warn_wait_then_act",
				"rules_for_running_agent_playbooks.production.action_kinds.pull_a_container_image_again: approvals_needed goes only with get_permission_then_act",
			}},
		{"listed playbooks", "    preset: routine_fixes_without_permission", `    preset: routine_fixes_without_permission
    playbooks:
      every_server: {rule: act_then_report}
      2026-11-03_free_disk_space: {rule: act_then_report}
      roles/web: {rule: act_then_report}
      reboot_servers_after_updates: {rule: warn_wait_then_act}`,
			[]string{
				`rules_for_running_agent_playbooks.production.playbooks.2026-11-03_free_disk_space: must be NAME of a playbook file playbooks/NAME.yml, starting with a letter or "_", then letters, digits, "_", or "-"; a one-time playbook in one_time_playbooks/ cannot be listed`,
				"rules_for_running_agent_playbooks.production.playbooks.reboot_servers_after_updates: warning_wait_minutes must be 1 to 1440 with warn_wait_then_act",
				`rules_for_running_agent_playbooks.production.playbooks.roles/web: must be NAME of a playbook file playbooks/NAME.yml, starting with a letter or "_", then letters, digits, "_", or "-"; a one-time playbook in one_time_playbooks/ cannot be listed`,
			}},
		{"listed playbook's approval also approves", "    preset: routine_fixes_without_permission", `    preset: routine_fixes_without_permission
    playbooks:
      replace_failed_servers: {rule: get_permission_then_act, approvals_needed: 1, its_approval_also_approves_in_the_same_session: [every_server, replace_failed_servers, every_server, destroy_replaced_servers]}
      every_server: {rule: act_then_report, its_approval_also_approves_in_the_same_session: [replace_failed_servers]}`,
			[]string{
				"rules_for_running_agent_playbooks.production.playbooks.every_server.its_approval_also_approves_in_the_same_session: goes only with rule get_permission_then_act: only a person's approval can also approve later runs, and act_then_report asks for none",
				"rules_for_running_agent_playbooks.production.playbooks.replace_failed_servers.its_approval_also_approves_in_the_same_session[1]: names replace_failed_servers itself; an approval approves its own playbook already",
				"rules_for_running_agent_playbooks.production.playbooks.replace_failed_servers.its_approval_also_approves_in_the_same_session[2]: every_server is named twice; each named playbook is approved once",
				"rules_for_running_agent_playbooks.production.playbooks.replace_failed_servers.its_approval_also_approves_in_the_same_session[3]: destroy_replaced_servers is not listed under rules_for_running_agent_playbooks.production.playbooks; list it there with its own rule, so the approval covers only a playbook a person chose by name",
			}},
		{"inventory name", "  production:\n", "  prod/eu:\n",
			[]string{`rules_for_running_agent_playbooks.prod/eu: the inventory name "prod/eu" may have only letters, digits, "_", and "-", and at most 100 characters`}},
		{"service name", "services_whose_changes_a_person_must_approve: [pgautofailover.service]", "services_whose_changes_a_person_must_approve: [pgautofailover, pgautofailover.service]",
			[]string{"services_whose_changes_a_person_must_approve[1]: pgautofailover.service is listed twice; a name without a unit type means a .service, so pgautofailover is pgautofailover.service"}},
		{"no administrator", "    role: administrator", "    role: member",
			[]string{"people: needs at least one person with role administrator, who opens the administration pages"}},
		{"role", "    role: administrator", "    role: owner",
			[]string{"people[0].role: person role must be administrator or member", "people: needs at least one person with role administrator, who opens the administration pages"}},
		{"two people with one address", "    role: administrator\n", "    role: administrator\n  - name: Ana Silva Again\n    sign_in_email_address: ANA@example.com\n    role: member\n",
			[]string{"people[1].sign_in_email_address: ana@example.com is also the sign-in email address of people[0]; each person must have a unique sign-in email address"}},
		{"schedule names someone not in people", "rotation_sign_in_email_addresses: [ana@example.com]", "rotation_sign_in_email_addresses: [ana@example.com, bruno@example.com]",
			[]string{"on_call_schedule.rotation_sign_in_email_addresses[1]: bruno@example.com is not the sign-in email address of any person listed in people"}},
		{"schedule key its type does not use", "  backup_option: everyone_available\n", "  backup_option: everyone_available\n  assignment_sign_in_email_addresses: [ana@example.com]\n",
			[]string{"on_call_schedule.assignment_sign_in_email_addresses: a weekly_rotation schedule does not use the setting. Remove the unused setting"}},
		{"schedule key its type needs", "  first_start_date: \"2026-11-02\"\n", "",
			[]string{"on_call_schedule.first_start_date: required for a weekly_rotation schedule"}},
		{"fixed backup without a person", "backup_option: everyone_available", "backup_option: fixed_person",
			[]string{"on_call_schedule.backup_sign_in_email_address: required with backup_option fixed_person"}},
		{"schedule rule", "  first_start_date: \"2026-11-02\"", "  first_start_date: \"2026-11-03\"",
			[]string{"on_call_schedule: the first start date must be a date such as 2026-11-02 on the handoff weekday (0 is Sunday)"}},
		{"time to claim", "escalate_if_unclaimed_after_minutes: 5", "escalate_if_unclaimed_after_minutes: 61",
			[]string{"on_call_schedule.escalate_if_unclaimed_after_minutes: must be a whole number of minutes from 1 to 60"}},
		{"alert source ID", "alert_source_id: monitoring", "alert_source_id: monitoring tool",
			[]string{"alert_sources[0].alert_source_id: must be 1 to 64 letters, digits, _ and -, such as monitoring"}},
		{"alert source ID of MMHotline's own alerts", "alert_source_id: monitoring", "alert_source_id: system",
			[]string{"alert_sources[0].alert_source_id: system names MMHotline's own alerts; choose another ID"}},
		{"signing secret", `signing_secrets: ["5f0c2d6e8a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"]`, `signing_secrets: ["5f0c2d6e"]`,
			[]string{"alert_sources[0].signing_secrets[0]: must be 64 hexadecimal characters, such as the output of openssl rand -hex 32"}},
		{"check-in times", "sos_after_no_check_in_for_minutes: 5", "sos_after_no_check_in_for_minutes: 2",
			[]string{"alert_sources[0].sos_after_no_check_in_for_minutes: the time before waking the person on call must be longer than the time before a warning"}},
		{"warning off", "warn_after_no_check_in_for_minutes: 2", "warn_after_no_check_in_for_minutes: off", nil},
		{"warning time", "warn_after_no_check_in_for_minutes: 2", "warn_after_no_check_in_for_minutes: never",
			[]string{"alert_sources[0].warn_after_no_check_in_for_minutes: must be a whole number of minutes from 1 to 60, or off"}},
		{"MMHotline-down alarm account", "cloudflare_account_id: 0123456789abcdef0123456789abcdef", "cloudflare_account_id: example",
			[]string{"mmhotline_down_alarm.cloudflare_account_id: the Cloudflare account ID must be 32 lowercase hexadecimal characters, as the dashboard shows it"}},
		{"mail server", "sending_services:\n", "sending_services:\n  mail_server:\n    smtp_server: smtp.example.com\n    smtp_connection_security: starttls\n    email_from_address: alerts@example.com\n",
			[]string{"sending_services.mail_server: smtp_server must be a host name and port, such as smtp.example.com:587"}},
		{"Telegram bot username in place of the token", "sending_services:\n", "sending_services:\n  telegram:\n    bot_token: mmhotline_bot\n",
			[]string{"sending_services.telegram.bot_token: must be the token from BotFather: digits, a colon, and letters, digits, _ and -"}},
		{"Slack tokens of the wrong kind", "sending_services:\n", "sending_services:\n  slack:\n    bot_token: xapp-wrong-kind\n    app_level_token: xoxb-wrong-kind\n",
			[]string{"sending_services.slack.bot_token: must be the Bot User OAuth Token, which starts with xoxb-", "sending_services.slack.app_level_token: must be the app-level token, which starts with xapp-"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(exampleSettingsYAML, test.from) {
				t.Fatalf("the example has no %q", test.from)
			}
			_, problems := ReadSettingsFile(writeSettingsFile(t, strings.Replace(exampleSettingsYAML, test.from, test.to, 1)), nil)
			if got := problemTexts(problems); !slices.Equal(got, test.want) {
				t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(test.want, "\n"))
			}
		})
	}
}

func TestSettingsFileThatIsNotSettings(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{"", "the settings file is empty"},
		{"team_name: [", "the settings file is not valid YAML: line 1: did not find expected node content"},
		{"- a list", "the settings file must be a section of keys, each on its own line as key: value, such as team_name: Example Operations"},
	} {
		_, problems := ReadSettingsFile(writeSettingsFile(t, test.text), nil)
		if got := problemTexts(problems); !slices.Equal(got, []string{test.want}) {
			t.Errorf("%q: problems %q", test.text, got)
		}
	}
	_, problems := ReadSettingsFile(filepath.Join(t.TempDir(), "missing.yml"), nil)
	if len(problems) != 1 || !strings.HasPrefix(problems[0].Problem, "MMHotline cannot read the settings file: ") {
		t.Fatalf("a missing file: %q", problemTexts(problems))
	}
}

// TestSettingsFileOwnerAndMode checks CA1: apply, start, and install refuse
// a file someone other than root could change or everyone could read, and
// say how to fix it; settings check skips the check (CA2).
func TestSettingsFileOwnerAndMode(t *testing.T) {
	self := []uint32{uint32(os.Getuid())}
	fix := "Fix the settings file owner and mode with: sudo chown root:mmhotline "
	path := writeSettingsFile(t, exampleSettingsYAML)
	if _, problems := ReadSettingsFile(path, self); len(problems) != 0 {
		t.Fatalf("a 0640 file of an allowed owner: %q", problemTexts(problems))
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSettingsFileOwnerAndMode(path, self); err != nil {
		t.Fatalf("a 0600 file: %s", err)
	}
	for _, test := range []struct {
		mode os.FileMode
		want string
	}{
		{0o644, "can be read by every user on the server (mode 0644), and it holds the team's secrets. " + fix},
		{0o660, "can be changed by users other than root (mode 0660). Only root may change it, because it decides what MMHotline does, and the fleet view command in it runs as root. " + fix},
		{0o602, "can be changed by users other than root (mode 0602)"},
	} {
		if err := os.Chmod(path, test.mode); err != nil {
			t.Fatal(err)
		}
		_, problems := ReadSettingsFile(path, self)
		if len(problems) != 1 || problems[0].KeyPath != "" || !strings.Contains(problems[0].Problem, path+" "+test.want) {
			t.Errorf("mode %04o: %q", test.mode, problemTexts(problems))
		}
		if _, problems := ReadSettingsFile(path, nil); len(problems) != 0 {
			t.Errorf("settings check of mode %04o: %q", test.mode, problemTexts(problems))
		}
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CheckSettingsFileOwnerAndMode(path, []uint32{0}); err == nil || !strings.Contains(err.Error(), "not root. Only root may change it") || !strings.Contains(err.Error(), fix+path+"; sudo chmod 0640 "+path) {
		t.Errorf("another owner: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link.yml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckSettingsFileOwnerAndMode(link, self); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("a symbolic link: %v", err)
	}
	if err := CheckSettingsFileOwnerAndMode(t.TempDir(), self); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("a directory: %v", err)
	}
}
