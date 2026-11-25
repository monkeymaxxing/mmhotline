package app

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
)

var (
	permissionFromOne = AgentPlaybookRule{Rule: "get_permission_then_act", ApprovalsNeeded: 1}
	actingAlone       = AgentPlaybookRule{Rule: "act_then_report"}
)

// The presets as the design lists them, kind by kind.
var presetSettings = map[string]map[actionkinds.Kind]AgentPlaybookRule{
	"permission_for_every_fix": {
		actionkinds.StartOrRestartAService: permissionFromOne, actionkinds.FreeDiskSpace: permissionFromOne,
		actionkinds.PullAContainerImageAgain: permissionFromOne, actionkinds.RebootAServer: permissionFromOne,
		actionkinds.BlockANetworkRange: permissionFromOne, actionkinds.ChangeDNSRecords: permissionFromOne,
		actionkinds.CheckOnly: actingAlone, actionkinds.UnrecognizedTask: permissionFromOne},
	"routine_fixes_without_permission": {
		actionkinds.StartOrRestartAService: actingAlone, actionkinds.FreeDiskSpace: actingAlone,
		actionkinds.PullAContainerImageAgain: actingAlone, actionkinds.RebootAServer: permissionFromOne,
		actionkinds.BlockANetworkRange: permissionFromOne, actionkinds.ChangeDNSRecords: permissionFromOne,
		actionkinds.CheckOnly: actingAlone, actionkinds.UnrecognizedTask: permissionFromOne},
	"every_fix_without_permission": {
		actionkinds.StartOrRestartAService: actingAlone, actionkinds.FreeDiskSpace: actingAlone,
		actionkinds.PullAContainerImageAgain: actingAlone, actionkinds.RebootAServer: actingAlone,
		actionkinds.BlockANetworkRange: actingAlone, actionkinds.ChangeDNSRecords: actingAlone,
		actionkinds.CheckOnly: actingAlone, actionkinds.UnrecognizedTask: actingAlone},
}

func agentPlaybookRulesCommand(t *testing.T, a *App, action string, in AgentPlaybookRulesCommandRequest, status int) (CommandResult, string) {
	t.Helper()
	return commandRequest(t, a, CommandRequest{Group: "agent-playbook-rules", Action: action, AgentPlaybookRules: in}, status)
}

// settingsByKind reads one inventory's settings from a command result.
func settingsByKind(t *testing.T, result CommandResult, inventory string) map[actionkinds.Kind]ActionKindAgentPlaybookRule {
	t.Helper()
	for _, settings := range result.AgentPlaybookRules {
		if settings.Inventory == inventory {
			byKind := map[actionkinds.Kind]ActionKindAgentPlaybookRule{}
			for _, row := range settings.ActionKinds {
				byKind[row.ActionKind] = row
			}
			if len(byKind) != len(actionkinds.Kinds()) {
				t.Fatalf("%s has %d kinds: %+v", inventory, len(byKind), settings)
			}
			return byKind
		}
	}
	t.Fatalf("no inventory %s in %+v", inventory, result.AgentPlaybookRules)
	return nil
}

// An inventory nobody has set waits for a person's approval for every kind
// of action, and each preset sets every kind as the design lists it,
// recording who applied it and when.
func TestAgentPlaybookRulesPresets(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	now := time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	result, _ := agentPlaybookRulesCommand(t, a, "show", AgentPlaybookRulesCommandRequest{Inventory: "staging"}, 200)
	for kind, row := range settingsByKind(t, result, "staging") {
		if row.AgentPlaybookRule != permissionFromOne || row.ChangedAt != nil || row.ChangedBy != "" {
			t.Errorf("staging without settings, %s: %+v", kind, row)
		}
	}
	settings := Principal{SettingsFile: "/etc/mmhotline/mmhotline_settings.yml"}
	for preset, want := range presetSettings {
		if err := applyAgentPlaybookRulesPreset(context.Background(), a, settings, "production", preset); err != nil {
			t.Fatal(err)
		}
		result, _ := agentPlaybookRulesCommand(t, a, "show", AgentPlaybookRulesCommandRequest{Inventory: "production"}, 200)
		for kind, row := range settingsByKind(t, result, "production") {
			if row.AgentPlaybookRule != want[kind] || row.ChangedAt == nil || !row.ChangedAt.Equal(now) || row.ChangedBy != "the settings file /etc/mmhotline/mmhotline_settings.yml" {
				t.Errorf("%s, %s: %+v", preset, kind, row)
			}
		}
	}
}

// agent-playbook-rules show prints each kind's setting; the rules of a setting
// refuse bad values, which change nothing.
func TestAgentPlaybookRulesShowAndSettingRules(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	settings := Principal{SettingsFile: "/etc/mmhotline/mmhotline_settings.yml"}
	set := func(kind, setting string, wait, approvals int) error {
		t.Helper()
		return setAgentPlaybookRule(context.Background(), a, settings, "production", kind, AgentPlaybookRule{Rule: setting, WarningWaitMinutes: wait, ApprovalsNeeded: approvals})
	}
	for _, change := range []struct {
		kind, setting   string
		wait, approvals int
	}{{"start_or_restart_a_service", "warn_wait_then_act", 10, 0}, {"free_disk_space", "get_permission_then_act", 0, 0}, {"reboot_a_server", "get_permission_then_act", 0, 2}, {"check_only", "act_then_report", 0, 0}} {
		if err := set(change.kind, change.setting, change.wait, change.approvals); err != nil {
			t.Fatal(err)
		}
	}
	result, raw := agentPlaybookRulesCommand(t, a, "show", AgentPlaybookRulesCommandRequest{Inventory: "production"}, 200)
	byKind := settingsByKind(t, result, "production")
	for kind, want := range map[actionkinds.Kind]struct {
		setting AgentPlaybookRule
		title   string
	}{
		actionkinds.StartOrRestartAService: {AgentPlaybookRule{Rule: "warn_wait_then_act", WarningWaitMinutes: 10}, "Warn, wait 10 minutes, then act"},
		// One approval unless two are asked for.
		actionkinds.FreeDiskSpace: {AgentPlaybookRule{Rule: "get_permission_then_act", ApprovalsNeeded: 1}, "Get permission, then act (1 approval)"},
		actionkinds.RebootAServer: {AgentPlaybookRule{Rule: "get_permission_then_act", ApprovalsNeeded: 2}, "Get permission, then act (2 approvals)"},
		actionkinds.CheckOnly:     {AgentPlaybookRule{Rule: "act_then_report"}, "Act, then report"},
	} {
		if row := byKind[kind]; row.AgentPlaybookRule != want.setting || row.RuleTitle != want.title || row.ChangedBy != "the settings file /etc/mmhotline/mmhotline_settings.yml" {
			t.Errorf("%s: %+v", kind, row)
		}
	}
	assertCommandKeys(t, raw, "agent_playbook_rules", "inventory", "inventory_file_exists", "action_kinds", "action_kind", "action_kind_title", "rule", "rule_title", "warning_wait_minutes", "approvals_needed", "changed_at", "changed_by")
	if text := result.Text(); !strings.Contains(text, "Reboot a server (reboot_a_server): Get permission, then act (2 approvals) (get_permission_then_act); changed by the settings file /etc/mmhotline/mmhotline_settings.yml at ") ||
		!strings.Contains(text, "Change DNS records (change_dns_records): Get permission, then act (1 approval) (get_permission_then_act); change_dns_records has no rule; it needs a person until you add one.") {
		t.Fatal(text)
	}
	for _, test := range []struct {
		kind, setting   string
		wait, approvals int
		want            string
	}{
		{"reboot_a_server", "ask_first", 0, 0, "rule must be"},
		{"reboot_a_server", "warn_wait_then_act", 0, 0, "warning_wait_minutes must be 1 to 1440"},
		{"reboot_a_server", "warn_wait_then_act", 1441, 0, "warning_wait_minutes must be 1 to 1440"},
		{"reboot_a_server", "act_then_report", 5, 0, "warning_wait_minutes goes only with warn_wait_then_act"},
		{"reboot_a_server", "get_permission_then_act", 0, 3, "approvals_needed must be 1 or 2"},
		{"reboot_a_server", "warn_wait_then_act", 10, 1, "approvals_needed goes only with get_permission_then_act"},
	} {
		if err := set(test.kind, test.setting, test.wait, test.approvals); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%+v: %v", test, err)
		}
	}
	if err := setAgentPlaybookRule(context.Background(), a, settings, "production;reboot", "reboot_a_server", AgentPlaybookRule{Rule: "act_then_report"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "may have only letters") {
		t.Errorf("inventory name: %v", err)
	}
	agentPlaybookRulesCommand(t, a, "remove", AgentPlaybookRulesCommandRequest{}, 404)
	// A refused change changes nothing.
	result, _ = agentPlaybookRulesCommand(t, a, "show", AgentPlaybookRulesCommandRequest{Inventory: "production"}, 200)
	if row := settingsByKind(t, result, "production")[actionkinds.RebootAServer]; row.AgentPlaybookRule != (AgentPlaybookRule{Rule: "get_permission_then_act", ApprovalsNeeded: 2}) {
		t.Fatalf("reboot_a_server changed: %+v", row)
	}
}

// Every member reads the rules for running the agent's playbooks on the page, which says where they
// are set; the page changes nothing.
func TestAgentPlaybookRulesPage(t *testing.T) {
	t.Parallel()
	h := newWorkflowRig(t)
	h.app.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	admin := h.browser()
	admin.addPerson("admin@example.test", "Administrator")
	if !strings.Contains(admin.get("/triage-agent", 200), `<a href="/agent-playbook-rules">Playbook rules</a>`) {
		t.Fatal("the triage agent page does not link Playbook rules")
	}
	inventories := filepath.Join(h.app.Store.Dir, "systems_repository", "inventories")
	if err := os.MkdirAll(inventories, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"production.yml", "staging.yml", "README.md"} {
		if err := os.WriteFile(filepath.Join(inventories, name), []byte("nodes: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An inventory with saved settings whose file is gone is still shown.
	if err := applyAgentPlaybookRulesPreset(context.Background(), h.app, Principal{UnixUser: "fixture"}, "retired", "every_fix_without_permission"); err != nil {
		t.Fatal(err)
	}
	if err := setAgentPlaybookRule(context.Background(), h.app, Principal{UnixUser: "fixture"}, "production", "reboot_a_server", AgentPlaybookRule{Rule: "get_permission_then_act", ApprovalsNeeded: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.Store.db.Exec(`INSERT INTO agent_playbook_rules_for_listed_playbooks(inventory,playbook_name,rule,warning_wait_minutes,approvals_needed,changed_at,changed_by) VALUES('production','every_server','act_then_report',0,0,1,'fixture')`); err != nil {
		t.Fatal(err)
	}
	_, member := addMember(t, h.app, "Member", RoleMember)
	other := h.browser()
	other.identify(member.SignInEmailAddress)
	page := other.get("/agent-playbook-rules", 200)
	// Every member reads what each kind accepts, as the triage agent reads it.
	for _, text := range []string{"Playbooks listed by name", "playbooks/every_server.yml Act, then report", "Each listed playbook takes its own rule instead of the rules of its kinds of action", "The inventory “production”", "The inventory “staging”", "The inventory “retired”", "no inventories/retired.yml", "Default (no rule saved)", "Get permission, then act (1 approval)", "Get permission, then act (2 approvals)", "by the mmhotline command, run by fixture", "Act, then report", "Each playbook uses the strictest rule", "Where the rules are set", "/etc/mmhotline/mmhotline_settings.yml", "What MMHotline counts as each kind of action", "the task's module is one of MMX's core modules listed below", "Start or restart a service", "systemd_service", "Arguments: name, state, restart_now, run_only_once_marker_file, in_systemd_of_user_account", "systemd_service.state must be running", "name: caddy", "show: Caddy answers again."} {
		if !strings.Contains(settingsPageText(page), text) {
			t.Fatalf("member's page lacks %q: %s", text, page)
		}
	}

	if strings.Contains(page, "<form") || strings.Contains(page, "Inventory README") || strings.Contains(page, "node task") {
		t.Fatalf("member's page: %s", page)
	}
	if page := admin.get("/agent-playbook-rules", 200); strings.Contains(page, "<form") {
		t.Fatalf("the administrator's page has a form: %s", page)
	}
	// The page has no form to post to.
	serverURL, _ := url.Parse(h.server.URL)
	var csrf string
	for _, cookie := range admin.client.Jar.Cookies(serverURL) {
		if strings.HasSuffix(cookie.Name, "mmhotline-csrf") {
			csrf = cookie.Value
		}
	}
	admin.post("/agent-playbook-rules", url.Values{"csrf": {csrf}, "action": {"apply-preset"}, "inventory": {"production"}, "preset": {"every_fix_without_permission"}}, 405)
}

// A playbook takes the strictest setting of its kinds, with the longest wait
// or the most approvals among the kinds of that setting.
func TestStrictestAgentPlaybookRule(t *testing.T) {
	t.Parallel()
	warn := func(minutes int) AgentPlaybookRule {
		return AgentPlaybookRule{Rule: "warn_wait_then_act", WarningWaitMinutes: minutes}
	}
	permission := func(approvals int) AgentPlaybookRule {
		return AgentPlaybookRule{Rule: "get_permission_then_act", ApprovalsNeeded: approvals}
	}
	for _, test := range []struct {
		settings []AgentPlaybookRule
		want     AgentPlaybookRule
	}{
		{[]AgentPlaybookRule{actingAlone, actingAlone}, actingAlone},
		{[]AgentPlaybookRule{actingAlone, warn(10), warn(30), warn(5)}, warn(30)},
		{[]AgentPlaybookRule{warn(30), permission(1), actingAlone}, permission(1)},
		{[]AgentPlaybookRule{permission(1), warn(60), permission(2)}, permission(2)},
	} {
		if got := strictestAgentPlaybookRule(test.settings); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%+v: %+v, want %+v", test.settings, got, test.want)
		}
	}
}
