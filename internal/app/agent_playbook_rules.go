package app

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// Rules for running the agent's playbooks say, for each inventory and kind of action, how much
// MMHotline may do without a person when the triage agent asks it to run a
// playbook: docs/changing-mmhotline/design/triage-agent.md#rules-for-running-the-agents-playbooks-and-kinds-of-action.
// Only a signed-in administrator or root through the command changes them;
// the agent cannot reach either.

// agentPlaybookRuleColumns are the columns and checks of a rule, the same
// for a kind of action and for a listed playbook.
const agentPlaybookRuleColumns = `
 rule TEXT NOT NULL CHECK(rule IN ('get_permission_then_act','warn_wait_then_act','act_then_report')),
 warning_wait_minutes INTEGER NOT NULL DEFAULT 0, approvals_needed INTEGER NOT NULL DEFAULT 0,
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL,
 CHECK((rule='warn_wait_then_act' AND warning_wait_minutes BETWEEN 1 AND 1440) OR (rule<>'warn_wait_then_act' AND warning_wait_minutes=0)),
 CHECK((rule='get_permission_then_act' AND approvals_needed IN (1,2)) OR (rule<>'get_permission_then_act' AND approvals_needed=0)),`

// agent_playbook_rules holds the rule of each kind of action on each
// inventory. agent_playbook_rules_for_listed_playbooks holds the rule a
// person listed for an ordinary playbook, playbooks/NAME.yml, on an
// inventory, by NAME; the playbook takes that rule instead of the rules of
// its kinds of action. its_approval_also_approves_in_the_same_session holds
// the names, comma-separated in the settings file's order, of the listed
// playbooks a person's approval of this playbook also approves once in the
// same triage session (approvalThatCovers); empty for most playbooks.
// starts_a_server_replacement marks the playbook whose run starts a server
// replacement, during which an SOS waits longer (server_replacements.go).
const agentPlaybookRulesSchema = `
CREATE TABLE agent_playbook_rules (
 inventory TEXT NOT NULL, action_kind TEXT NOT NULL,` + agentPlaybookRuleColumns + `
 PRIMARY KEY(inventory, action_kind)
);
CREATE TABLE agent_playbook_rules_for_listed_playbooks (
 inventory TEXT NOT NULL, playbook_name TEXT NOT NULL,
 its_approval_also_approves_in_the_same_session TEXT NOT NULL DEFAULT '',
 starts_a_server_replacement INTEGER NOT NULL DEFAULT 0 CHECK(starts_a_server_replacement IN (0,1)),` + agentPlaybookRuleColumns + `
 PRIMARY KEY(inventory, playbook_name)
);
`

const (
	getPermissionThenAct = "get_permission_then_act"
	warnWaitThenAct      = "warn_wait_then_act"
	actThenReport        = "act_then_report"
)

// maximumWarningWaitMinutes is one day: a fix written for an incident is
// stale by then, so a longer wait is a forgotten action, not a warning.
const maximumWarningWaitMinutes = 1440

// AgentPlaybookRule is how much MMHotline may do without a person for a kind of
// action: get_permission_then_act with the approvals it needs,
// warn_wait_then_act with the minutes it waits after the warning, or
// act_then_report.
type AgentPlaybookRule struct {
	Rule               string `json:"rule"`
	WarningWaitMinutes int    `json:"warning_wait_minutes,omitempty"`
	ApprovalsNeeded    int    `json:"approvals_needed,omitempty"`
}

// Title names the setting for people, such as "Warn, wait 10 minutes, then act".
func (setting AgentPlaybookRule) Title() string {
	switch setting.Rule {
	case getPermissionThenAct:
		if setting.ApprovalsNeeded == 2 {
			return "Get permission, then act (2 approvals)"
		}
		return "Get permission, then act (1 approval)"
	case warnWaitThenAct:
		return "Warn, wait " + minutesText(time.Duration(setting.WarningWaitMinutes)*time.Minute) + ", then act"
	case actThenReport:
		return "Act, then report"
	}
	return setting.Rule
}

func checkAgentPlaybookRule(setting AgentPlaybookRule) error {
	switch setting.Rule {
	case getPermissionThenAct, warnWaitThenAct, actThenReport:
	default:
		return fmt.Errorf("%w: rule must be get_permission_then_act, warn_wait_then_act, or act_then_report", ErrInvalid)
	}
	if setting.Rule == warnWaitThenAct && (setting.WarningWaitMinutes < 1 || setting.WarningWaitMinutes > maximumWarningWaitMinutes) {
		return fmt.Errorf("%w: warning_wait_minutes must be 1 to 1440 with warn_wait_then_act", ErrInvalid)
	}
	if setting.Rule != warnWaitThenAct && setting.WarningWaitMinutes != 0 {
		return fmt.Errorf("%w: warning_wait_minutes goes only with warn_wait_then_act", ErrInvalid)
	}
	if setting.Rule == getPermissionThenAct && setting.ApprovalsNeeded != 1 && setting.ApprovalsNeeded != 2 {
		return fmt.Errorf("%w: approvals_needed must be 1 or 2", ErrInvalid)
	}
	if setting.Rule != getPermissionThenAct && setting.ApprovalsNeeded != 0 {
		return fmt.Errorf("%w: approvals_needed goes only with get_permission_then_act", ErrInvalid)
	}
	return nil
}

var agentPlaybookRuleStrictness = map[string]int{actThenReport: 0, warnWaitThenAct: 1, getPermissionThenAct: 2}

// strictestAgentPlaybookRule is the setting for a playbook whose node tasks are of
// several kinds: the strictest setting, with the longest wait or the most
// approvals among the kinds of that setting, so no kind gets less care than
// its own setting asks.
func strictestAgentPlaybookRule(settings []AgentPlaybookRule) AgentPlaybookRule {
	strictest := AgentPlaybookRule{Rule: actThenReport}
	for _, setting := range settings {
		switch {
		case agentPlaybookRuleStrictness[setting.Rule] > agentPlaybookRuleStrictness[strictest.Rule]:
			strictest = setting
		case setting.Rule == strictest.Rule:
			strictest.WarningWaitMinutes = max(strictest.WarningWaitMinutes, setting.WarningWaitMinutes)
			strictest.ApprovalsNeeded = max(strictest.ApprovalsNeeded, setting.ApprovalsNeeded)
		}
	}
	return strictest
}

// agentPlaybookRulesPreset sets every kind of action of one inventory at once.
type agentPlaybookRulesPreset struct {
	ID, Title string
	settings  map[actionkinds.Kind]AgentPlaybookRule
}

var (
	permissionWithOneApproval = AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 1}
	withoutPermission         = AgentPlaybookRule{Rule: actThenReport}
)

var agentPlaybookRulesPresets = []agentPlaybookRulesPreset{
	{"permission_for_every_fix", "Permission for every fix", map[actionkinds.Kind]AgentPlaybookRule{
		actionkinds.StartOrRestartAService: permissionWithOneApproval, actionkinds.FreeDiskSpace: permissionWithOneApproval,
		actionkinds.PullAContainerImageAgain: permissionWithOneApproval, actionkinds.RebootAServer: permissionWithOneApproval,
		actionkinds.BlockANetworkRange: permissionWithOneApproval, actionkinds.ChangeDNSRecords: permissionWithOneApproval,
		actionkinds.CheckOnly: withoutPermission, actionkinds.UnrecognizedTask: permissionWithOneApproval}},
	{"routine_fixes_without_permission", "Routine fixes without permission", map[actionkinds.Kind]AgentPlaybookRule{
		actionkinds.StartOrRestartAService: withoutPermission, actionkinds.FreeDiskSpace: withoutPermission,
		actionkinds.PullAContainerImageAgain: withoutPermission, actionkinds.RebootAServer: permissionWithOneApproval,
		actionkinds.BlockANetworkRange: permissionWithOneApproval, actionkinds.ChangeDNSRecords: permissionWithOneApproval,
		actionkinds.CheckOnly: withoutPermission, actionkinds.UnrecognizedTask: permissionWithOneApproval}},
	{"every_fix_without_permission", "Every fix without permission", map[actionkinds.Kind]AgentPlaybookRule{
		actionkinds.StartOrRestartAService: withoutPermission, actionkinds.FreeDiskSpace: withoutPermission,
		actionkinds.PullAContainerImageAgain: withoutPermission, actionkinds.RebootAServer: withoutPermission,
		actionkinds.BlockANetworkRange: withoutPermission, actionkinds.ChangeDNSRecords: withoutPermission,
		actionkinds.CheckOnly: withoutPermission, actionkinds.UnrecognizedTask: withoutPermission}},
}

// safestAgentPlaybookRule is the setting of a kind of action with no saved
// setting, and of every kind of an inventory the settings file leaves out:
// every fix waits for a person's approval, because a missing setting must
// never let a fix run without a person.
var safestAgentPlaybookRule = AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 1}

func findAgentPlaybookRulesPreset(id string) (agentPlaybookRulesPreset, bool) {
	for _, preset := range agentPlaybookRulesPresets {
		if preset.ID == id {
			return preset, true
		}
	}
	return agentPlaybookRulesPreset{}, false
}

// ActionKindAgentPlaybookRule is the rule of one kind of action on one
// inventory.
type ActionKindAgentPlaybookRule struct {
	ActionKind      actionkinds.Kind `json:"action_kind"`
	ActionKindTitle string           `json:"action_kind_title"`
	AgentPlaybookRule
	RuleTitle string `json:"rule_title"`
	// ChangedAt and ChangedBy are absent while the kind has no saved setting
	// and every fix waits for a person's approval.
	ChangedAt *time.Time `json:"changed_at,omitempty"`
	ChangedBy string     `json:"changed_by,omitempty"`
}

// InventoryAgentPlaybookRules are the rules for running the agent's playbooks of one inventory, one for
// each kind of action.
type InventoryAgentPlaybookRules struct {
	Inventory string `json:"inventory"`
	// InventoryFileExists says whether MMHotline's copy of the systems
	// repository has inventories/<inventory>.yml.
	InventoryFileExists bool                          `json:"inventory_file_exists"`
	ActionKinds         []ActionKindAgentPlaybookRule `json:"action_kinds"`
	// ListedPlaybooks are the ordinary playbooks listed by name under the
	// inventory's playbooks in the settings file, in name order.
	ListedPlaybooks []ListedPlaybookAgentPlaybookRule `json:"listed_playbooks"`
}

// ListedPlaybookAgentPlaybookRule is the rule a person listed for one
// ordinary playbook, playbooks/NAME.yml, on one inventory. The playbook
// takes the listed rule instead of the strictest rule of its kinds of
// action, because a playbooks/ file is a person's text: a triage-signed
// commit touching playbooks/ stops every MMX run until a person reviews it.
type ListedPlaybookAgentPlaybookRule struct {
	// PlaybookName is NAME of playbooks/NAME.yml, the key under playbooks.
	PlaybookName string `json:"playbook_name"`
	AgentPlaybookRule
	RuleTitle string `json:"rule_title"`
	// ItsApprovalAlsoApprovesInTheSameSession names other playbooks listed
	// on the inventory, each NAME of playbooks/NAME.yml, that a person's
	// approval of this playbook also approves once later in the same triage
	// session; only with get_permission_then_act.
	ItsApprovalAlsoApprovesInTheSameSession []string `json:"its_approval_also_approves_in_the_same_session,omitempty"`
	// StartsAServerReplacement says a run of the playbook starts a server
	// replacement, during which an SOS of the session waits longer.
	StartsAServerReplacement bool      `json:"starts_a_server_replacement,omitempty"`
	ChangedAt                time.Time `json:"changed_at"`
	ChangedBy                string    `json:"changed_by"`
}

// listedPlaybookRuleText says that a playbook takes the rule a person listed
// for it, such as "the playbook every_server is listed in
// rules_for_running_agent_playbooks for the inventory “servers” with Act,
// then report".
func listedPlaybookRuleText(playbookName, inventory string, rule AgentPlaybookRule) string {
	return "the playbook " + playbookName + " is listed in rules_for_running_agent_playbooks for " + inventoryText(inventory) + " with " + rule.Title()
}

// Text is the listed playbook's rule in one line, as show prints it.
func (row ListedPlaybookAgentPlaybookRule) Text() string {
	var also string
	if len(row.ItsApprovalAlsoApprovesInTheSameSession) > 0 {
		also = "; its approval also approves in the same session: " + strings.Join(row.ItsApprovalAlsoApprovesInTheSameSession, ", ")
	}
	if row.StartsAServerReplacement {
		also += "; starts a server replacement"
	}
	return fmt.Sprintf("playbooks/%s.yml, listed by name: %s (%s)%s; changed by %s at %s.", row.PlaybookName, row.RuleTitle, row.Rule, also, row.ChangedBy, commandTime(row.ChangedAt))
}

// joinedPlaybookNames and splitPlaybookNames store a list of playbook
// names in one column, comma-separated: a playbook name has no comma.
func joinedPlaybookNames(names []string) string { return strings.Join(names, ",") }

func splitPlaybookNames(column string) []string {
	if column == "" {
		return nil
	}
	return strings.Split(column, ",")
}

// checkListedPlaybookName checks a key under an inventory's playbooks: NAME
// of an ordinary playbook, playbooks/NAME.yml, as MMX names it. A one-time
// playbook, whose name starts with its date, can never be listed: the triage
// agent writes those.
func checkListedPlaybookName(name string) error {
	if !ordinaryPlaybookName.MatchString(name) {
		return fmt.Errorf(`%w: must be NAME of a playbook file playbooks/NAME.yml, starting with a letter or "_", then letters, digits, "_", or "-"; a one-time playbook in one_time_playbooks/ cannot be listed`, ErrInvalid)
	}
	return nil
}

// savedListedPlaybookRules reads the listed playbooks' rules by inventory,
// in name order, of every inventory or of one, and of every playbook or of
// the one named.
func savedListedPlaybookRules(ctx context.Context, tx *sql.Tx, inventory, playbookName string) (map[string][]ListedPlaybookAgentPlaybookRule, error) {
	rows, err := tx.QueryContext(ctx, `SELECT inventory,playbook_name,rule,warning_wait_minutes,approvals_needed,its_approval_also_approves_in_the_same_session,starts_a_server_replacement,changed_at,changed_by FROM agent_playbook_rules_for_listed_playbooks WHERE (?='' OR inventory=?) AND (?='' OR playbook_name=?) ORDER BY playbook_name`, inventory, inventory, playbookName, playbookName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	saved := map[string][]ListedPlaybookAgentPlaybookRule{}
	for rows.Next() {
		var name, alsoApproves string
		var row ListedPlaybookAgentPlaybookRule
		var changedAt int64
		if err := rows.Scan(&name, &row.PlaybookName, &row.Rule, &row.WarningWaitMinutes, &row.ApprovalsNeeded, &alsoApproves, &row.StartsAServerReplacement, &changedAt, &row.ChangedBy); err != nil {
			return nil, err
		}
		row.ChangedAt, row.RuleTitle = time.Unix(changedAt, 0).UTC(), row.AgentPlaybookRule.Title()
		row.ItsApprovalAlsoApprovesInTheSameSession = splitPlaybookNames(alsoApproves)
		saved[name] = append(saved[name], row)
	}
	return saved, rows.Err()
}

func checkAgentPlaybookRulesInventory(inventory string) error {
	if err := triageagent.CheckInventoryName(inventory); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	return nil
}

// AgentPlaybookRules lists the rules for running the agent's playbooks of the inventories in MMHotline's
// copy of the systems repository and of those with saved settings, or of
// one inventory. Every member may read them.
func (a *App) AgentPlaybookRules(ctx context.Context, p Principal, inventory string) ([]InventoryAgentPlaybookRules, error) {
	if inventory != "" {
		if err := checkAgentPlaybookRulesInventory(inventory); err != nil {
			return nil, err
		}
	}
	files := a.inventoryFiles()
	var result []InventoryAgentPlaybookRules
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		saved, err := savedAgentPlaybookRules(ctx, tx, inventory)
		if err != nil {
			return err
		}
		listed, err := savedListedPlaybookRules(ctx, tx, inventory, "")
		if err != nil {
			return err
		}
		names := []string{inventory}
		if inventory == "" {
			names = nil
			for name := range files {
				names = append(names, name)
			}
			for name := range saved {
				if !files[name] {
					names = append(names, name)
				}
			}
			sort.Strings(names)
		}
		for _, name := range names {
			settings := InventoryAgentPlaybookRules{Inventory: name, InventoryFileExists: files[name], ListedPlaybooks: listed[name]}
			if settings.ListedPlaybooks == nil {
				settings.ListedPlaybooks = []ListedPlaybookAgentPlaybookRule{}
			}
			for _, kind := range actionkinds.Kinds() {
				settings.ActionKinds = append(settings.ActionKinds, kindAgentPlaybookRule(saved[name], kind))
			}
			result = append(result, settings)
		}
		return nil
	})
	return result, err
}

// inventoryFiles names the inventories in MMHotline's copy of the systems
// repository, from the file names inventories/*.yml.
func (a *App) inventoryFiles() map[string]bool {
	names := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(a.Store.Dir, systemsrepository.DirectoryName, "inventories"))
	for _, entry := range entries {
		name, isYAML := strings.CutSuffix(entry.Name(), ".yml")
		if isYAML && entry.Type().IsRegular() && triageagent.CheckInventoryName(name) == nil {
			names[name] = true
		}
	}
	return names
}

// inventoryNames is inventoryFiles in order, for the triage agent's tasks
// and its listing of the inventory servers.
func (a *App) inventoryNames() []string {
	return slices.Sorted(maps.Keys(a.inventoryFiles()))
}

// unsetActionKindNote says that kind has no saved setting, so it takes the
// safest: the settings file's note, the page of rules for running the agent's playbooks, and
// agent-playbook-rules show all say it with these words.
func unsetActionKindNote(kind actionkinds.Kind) string {
	return string(kind) + " has no rule; it needs a person until you add one"
}

// kindAgentPlaybookRule is the rule of kind from one inventory's saved
// rows, with its titles; a kind without a saved row takes the safest
// setting.
func kindAgentPlaybookRule(saved map[actionkinds.Kind]ActionKindAgentPlaybookRule, kind actionkinds.Kind) ActionKindAgentPlaybookRule {
	row, found := saved[kind]
	if !found {
		row = ActionKindAgentPlaybookRule{AgentPlaybookRule: safestAgentPlaybookRule}
	}
	row.ActionKind, row.ActionKindTitle, row.RuleTitle = kind, kind.Title(), row.AgentPlaybookRule.Title()
	return row
}

// savedAgentPlaybookRules reads the saved rows by inventory and kind, of every
// inventory or of one.
func savedAgentPlaybookRules(ctx context.Context, tx *sql.Tx, inventory string) (map[string]map[actionkinds.Kind]ActionKindAgentPlaybookRule, error) {
	rows, err := tx.QueryContext(ctx, `SELECT inventory,action_kind,rule,warning_wait_minutes,approvals_needed,changed_at,changed_by FROM agent_playbook_rules WHERE ?='' OR inventory=?`, inventory, inventory)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	saved := map[string]map[actionkinds.Kind]ActionKindAgentPlaybookRule{}
	for rows.Next() {
		var name, kind string
		var row ActionKindAgentPlaybookRule
		var changedAt int64
		if err := rows.Scan(&name, &kind, &row.Rule, &row.WarningWaitMinutes, &row.ApprovalsNeeded, &changedAt, &row.ChangedBy); err != nil {
			return nil, err
		}
		at := time.Unix(changedAt, 0).UTC()
		row.ChangedAt = &at
		if saved[name] == nil {
			saved[name] = map[actionkinds.Kind]ActionKindAgentPlaybookRule{}
		}
		saved[name][actionkinds.Kind(kind)] = row
	}
	return saved, rows.Err()
}

func saveAgentPlaybookRule(ctx context.Context, tx *sql.Tx, inventory string, kind actionkinds.Kind, setting AgentPlaybookRule, at time.Time, by string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_playbook_rules(inventory,action_kind,rule,warning_wait_minutes,approvals_needed,changed_at,changed_by) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(inventory,action_kind) DO UPDATE SET rule=excluded.rule,warning_wait_minutes=excluded.warning_wait_minutes,approvals_needed=excluded.approvals_needed,changed_at=excluded.changed_at,changed_by=excluded.changed_by`,
		inventory, kind, setting.Rule, setting.WarningWaitMinutes, setting.ApprovalsNeeded, at.Unix(), by)
	return err
}

// playbookRule is the setting a playbook takes: the rule a person listed
// for the playbook, or else the strictest rule of its kinds; and Get
// permission, then act with at least 1 approval when the list of services
// whose changes a person must approve decides (servicesListDecides).
func playbookRule(kinds []ActionKindAgentPlaybookRule, onServicesWhoseChangesAPersonMustApprove []taskOnAServiceWhoseChangesAPersonMustApprove, listed *AgentPlaybookRule) AgentPlaybookRule {
	var settings []AgentPlaybookRule
	for _, kind := range kinds {
		settings = append(settings, kind.AgentPlaybookRule)
	}
	setting := strictestAgentPlaybookRule(settings)
	if listed != nil {
		setting = *listed
	}
	if servicesListDecides(listed != nil, onServicesWhoseChangesAPersonMustApprove) {
		setting = AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: max(1, setting.ApprovalsNeeded)}
	}
	return setting
}

// servicesListDecides says whether the list of services whose changes a
// person must approve makes a playbook wait for a person: when a node task
// acts on a listed service, unless a person listed the playbook by name in
// rules_for_running_agent_playbooks. The listed rule overrides the list: the
// person listed the playbook knowingly, a playbook for every server (such
// as every_server) restarts listed services as a matter of course, and the
// list keeps guarding the triage agent's one-time playbooks, which can
// never be listed. This is the one place the choice is made: removing
// "&& !playbookListed" makes the list win over a listed rule.
func servicesListDecides(playbookListed bool, onServicesWhoseChangesAPersonMustApprove []taskOnAServiceWhoseChangesAPersonMustApprove) bool {
	return len(onServicesWhoseChangesAPersonMustApprove) > 0 && !playbookListed
}

// AgentPlaybookRulesCommandRequest is what agent-playbook-rules show and
// check-whether-a-playbook-needs-a-person take; the settings file sets the
// rules for running the agent's playbooks.
type AgentPlaybookRulesCommandRequest struct {
	Inventory string `json:"inventory,omitempty"`
	// Playbook is the playbook check-whether-a-playbook-needs-a-person
	// checks, such as
	// one_time_playbooks/2026-11-03_restart_postgres_on_db01.yml.
	Playbook string `json:"playbook,omitempty"`
}

func (a *App) agentPlaybookRulesCommand(ctx context.Context, p Principal, action string, in AgentPlaybookRulesCommandRequest, result *CommandResult) error {
	switch action {
	case "check-whether-a-playbook-needs-a-person":
		answer, err := a.CheckWhetherAPlaybookNeedsAPerson(ctx, p, in.Playbook, in.Inventory)
		if err != nil {
			return err
		}
		result.WhetherAPlaybookNeedsAPerson = &answer
		result.Message = playbookCheckChangedNothingText
		return nil
	case "show":
	default:
		return ErrNotFound
	}
	var err error
	if result.AgentPlaybookRules, err = a.AgentPlaybookRules(ctx, p, in.Inventory); err != nil {
		return err
	}
	// The list applies to every inventory, so show prints it with any.
	services, err := a.ServicesWhoseChangesAPersonMustApprove(ctx, p)
	if err != nil {
		return err
	}
	result.ServicesWhoseChangesAPersonMustApprove = &services
	result.Message = "The rules for running the agent's playbooks and the services whose changes a person must approve are set in " + a.SettingsFilePath() + ", in rules_for_running_agent_playbooks and services_whose_changes_a_person_must_approve."
	if len(result.AgentPlaybookRules) == 0 {
		result.Message = "MMHotline's copy of the systems repository has no inventories, and no rules for running the agent's playbooks are saved. " + result.Message
	}
	return nil
}

func agentPlaybookRulesText(inventories []InventoryAgentPlaybookRules) []string {
	var lines []string
	for _, inventory := range inventories {
		header := "Rules for running the agent's playbooks on inventory " + inventory.Inventory + ":"
		if !inventory.InventoryFileExists {
			header = "Rules for running the agent's playbooks on inventory " + inventory.Inventory + ", which has no inventories/" + inventory.Inventory + ".yml in MMHotline's copy of the systems repository:"
		}
		lines = append(lines, header)
		for _, row := range inventory.ActionKinds {
			lines = append(lines, row.Text())
		}
		for _, row := range inventory.ListedPlaybooks {
			lines = append(lines, row.Text())
		}
	}
	return lines
}

// Text is the kind's setting in one line, as show and
// check-whether-a-playbook-needs-a-person print it.
func (row ActionKindAgentPlaybookRule) Text() string {
	line := fmt.Sprintf("%s (%s): %s (%s)", row.ActionKindTitle, row.ActionKind, row.RuleTitle, row.Rule)
	if row.ChangedAt == nil {
		return line + "; " + unsetActionKindNote(row.ActionKind) + "."
	}
	return line + fmt.Sprintf("; changed by %s at %s.", row.ChangedBy, commandTime(*row.ChangedAt))
}
