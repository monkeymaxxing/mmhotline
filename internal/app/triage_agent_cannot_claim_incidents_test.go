package app

import (
	"slices"
	"strings"
	"testing"

	"mmhotline/internal/actionkinds"
)

// A claim or a resolve stops MMHotline contacting people about an incident.
// MMX runs the agent's playbooks with root on the servers, and MMHotline's
// own server may be in the inventory, so a task that runs
// `mmhotline incidents claim` or `resolve` there must never run without a
// person, except under the opt-in every_fix_without_permission, which gives
// the agent root on every server. Each playbook runs the command through a
// different module or argument that runs a program. Fails if a kind of
// action recognizes any of them, or if a preset other than
// every_fix_without_permission lets an unrecognized task act.
func TestPlaybooksThatClaimOrResolveIncidentsWaitForAPerson(t *testing.T) {
	const (
		claim   = "mmhotline incidents claim --incident-id 4f2a --sign-in-email-address alex@example.com"
		resolve = "mmhotline incidents resolve --incident-id 4f2a"
	)
	task := func(action, moduleKind string, arguments ...string) string {
		kind := ""
		if moduleKind != "" {
			kind = `"module_kind":"` + moduleKind + `",`
		}
		return `{"path":"silence/task_1","node":"util01","action":"` + action + `",` + kind + `"status":"ready","arguments":[` + strings.Join(arguments, ",") + `]}`
	}
	argument := func(field, value string) string {
		return `{"field":"` + field + `","value":"` + value + `","status":"literal"}`
	}
	playbooks := []struct {
		name  string
		tasks []string
	}{
		{"shell", []string{task("shell", "core", argument("shell.script", claim))}},
		{"shell as root", []string{task("shell", "core", argument("shell.script", resolve), argument("shell.run_as_user_account", "root"))}},
		{"shell inside a role", []string{
			`{"path":"silence/silence","node":"util01","action":"role","status":"ready"}`,
			task("shell", "core", argument("shell.script", claim)),
		}},
		{"shell after a check only task", []string{
			task("show", "", argument("show", "Freeing disk space")),
			task("shell", "core", argument("shell.script", resolve)),
		}},
		{"wait_for's command_succeeds", []string{task("wait_for", "core", argument("wait_for.command_succeeds", claim))}},
		{"reboot's run_only_if_this_command_succeeds", []string{task("reboot", "core", argument("reboot.run_only_if_this_command_succeeds", claim))}},
		{"reboot's skip_if_this_command_succeeds", []string{task("reboot", "core", argument("reboot.skip_if_this_command_succeeds", resolve))}},
		{"cron_job", []string{task("cron_job", "core", argument("cron_job.name", "silence"), argument("cron_job.cron_schedule", "* * * * *"), argument("cron_job.command", claim))}},
		{"systemd_scheduled_job", []string{task("systemd_scheduled_job", "core", argument("systemd_scheduled_job.name", "silence"), argument("systemd_scheduled_job.script", resolve))}},
		{"systemd_unit", []string{task("systemd_unit", "core", argument("systemd_unit.name", "silence.service"), argument("systemd_unit.content", `[Service]\nExecStart=/usr/local/bin/`+claim))}},
		{"a module of the team's own", []string{task("claim_incident", "project_executable", argument("claim_incident.incident_id", "4f2a"))}},
		{"a module of the team's own named systemd_service", []string{task("systemd_service", "project_starlark", argument("systemd_service.name", "caddy"), argument("systemd_service.state", "running"))}},
		{"an official extra module", []string{task("pg_auto_failover_node", "official_extra", argument("pg_auto_failover_node.name", "db01"))}},
	}
	for _, playbook := range playbooks {
		recognition, err := actionkinds.Recognize([]byte(`{"schema_version":2,"fleet_stages":[{"path":"silence","node_tasks":[` + strings.Join(playbook.tasks, ",") + `]}]}`))
		if err != nil || !slices.Contains(recognition.Kinds, actionkinds.UnrecognizedTask) {
			t.Errorf("%s: kinds %v, %v; want an unrecognized task", playbook.name, recognition.Kinds, err)
			continue
		}
		for _, preset := range agentPlaybookRulesPresets {
			var kinds []ActionKindAgentPlaybookRule
			for _, kind := range recognition.Kinds {
				kinds = append(kinds, ActionKindAgentPlaybookRule{ActionKind: kind, AgentPlaybookRule: preset.settings[kind]})
			}
			want := AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 1}
			if preset.ID == "every_fix_without_permission" {
				want = AgentPlaybookRule{Rule: actThenReport}
			}
			if got := playbookRule(kinds, nil, nil); got != want {
				t.Errorf("%s under %s: %+v; want %+v", playbook.name, preset.ID, got, want)
			}
		}
	}
}
