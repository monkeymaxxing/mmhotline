package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"mmhotline/internal/actionkinds"
)

// agent-playbook-rules check-whether-a-playbook-needs-a-person shows what
// MMHotline does when the triage agent asks to run a playbook, decided as the
// agent's request is decided (judgePlaybook), and changes nothing:
// docs/changing-mmhotline/design/triage-agent.md#checking-whether-a-playbook-needs-a-person.

// playbookCheckChangedNothingText ends the command's answer.
const playbookCheckChangedNothingText = "MMHotline checked the playbook as it checks a triage agent's request to run it, and changed nothing: it ran only mmx explain, and recorded no request, asked for no approval, sent no alert, and ran no playbook."

// reviewAgentWouldReviewText is the line the answer adds when a review agent
// would also review the playbook. The command does not ask it, because a
// review is a run of a coding agent, which costs money and takes minutes,
// and its answer could differ next time.
const reviewAgentWouldReviewText = "A review agent would also review the playbook before the playbook runs. The check command does not ask the review agent."

// CommandWhetherAPlaybookNeedsAPerson is what MMHotline does when the triage
// agent asks to run a playbook on an inventory, as agent-playbook-rules
// check-whether-a-playbook-needs-a-person prints it.
type CommandWhetherAPlaybookNeedsAPerson struct {
	Playbook  string `json:"playbook"`
	Inventory string `json:"inventory"`
	// CommitID is the commit of MMHotline's copy of the systems repository
	// that mmx explain read.
	CommitID string                       `json:"commit_id"`
	Tasks    []CommandCheckedPlaybookTask `json:"tasks"`
	// ActionKinds are the playbook's kinds of action, each with its rule
	// on the inventory.
	ActionKinds []ActionKindAgentPlaybookRule `json:"action_kinds"`
	// NeedsAPerson is whether the playbook waits for a person's approval,
	// which only Get permission, then act does. Warn, wait, then act runs
	// without one unless someone presses Stop.
	NeedsAPerson bool `json:"needs_a_person"`
	// ListedPlaybook is the rule a person listed for the playbook by name,
	// which the playbook takes instead of its kinds' rules; absent when the
	// playbook is not listed on the inventory.
	ListedPlaybook *ListedPlaybookAgentPlaybookRule `json:"listed_playbook,omitempty"`
	// AgentPlaybookRule is the setting the playbook takes: what MMHotline does.
	AgentPlaybookRule
	RuleTitle string `json:"rule_title"`
	// Reason says why in plain words: the kinds whose setting is the
	// strictest, or the list of services whose changes a person must approve.
	Reason string `json:"reason"`
	// ReviewAgentWouldReviewThePlaybook is whether a review agent is set up
	// and would review the playbook before it runs, which it does for a
	// playbook that runs without a person. The command does not ask it.
	ReviewAgentWouldReviewThePlaybook             bool                                                  `json:"review_agent_would_review_the_playbook"`
	TasksOnServicesWhoseChangesAPersonMustApprove []CommandTaskOnAServiceWhoseChangesAPersonMustApprove `json:"tasks_on_services_whose_changes_a_person_must_approve"`
	UnrecognizedTasks                             []CommandUnrecognizedTask                             `json:"unrecognized_tasks"`
}

// CommandCheckedPlaybookTask is one node task of the checked playbook, as
// mmx explain listed it, with its kind of action.
type CommandCheckedPlaybookTask struct {
	Path string `json:"path"`
	// Node is the server the task runs on.
	Node            string           `json:"node"`
	Module          string           `json:"module"`
	ActionKind      actionkinds.Kind `json:"action_kind"`
	ActionKindTitle string           `json:"action_kind_title"`
}

// CheckWhetherAPlaybookNeedsAPerson decides a playbook on an inventory
// exactly as the triage agent's request to run it is decided, and changes
// nothing. Only administrators check, as only they change the rules for running
// the agent's playbooks. A playbook MMHotline cannot decide is refused with the message
// the agent's request gets.
func (a *App) CheckWhetherAPlaybookNeedsAPerson(ctx context.Context, p Principal, playbookPath, inventory string) (CommandWhetherAPlaybookNeedsAPerson, error) {
	var reviewAgentSetUp bool
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		setup, err := codingAgentSetup(ctx, tx, reviewAgentRole)
		reviewAgentSetUp = setup != nil
		return err
	}); err != nil {
		return CommandWhetherAPlaybookNeedsAPerson{}, err
	}
	judgment, err := a.judgePlaybook(ctx, playbookPath, inventory)
	if err != nil {
		return CommandWhetherAPlaybookNeedsAPerson{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	needsAPerson := judgment.setting.Rule == getPermissionThenAct
	out := CommandWhetherAPlaybookNeedsAPerson{Playbook: playbookPath, Inventory: inventory, CommitID: judgment.commitID, Tasks: []CommandCheckedPlaybookTask{}, ActionKinds: judgment.kindSettings, ListedPlaybook: judgment.listed,
		NeedsAPerson: needsAPerson, AgentPlaybookRule: judgment.setting, RuleTitle: judgment.setting.Title(), Reason: judgment.reason(inventory),
		// As judgeAndRunPlaybook decides: the review agent reviews only a
		// playbook that would run without a person.
		ReviewAgentWouldReviewThePlaybook:             reviewAgentSetUp && !needsAPerson,
		TasksOnServicesWhoseChangesAPersonMustApprove: commandTasksOnServicesWhoseChangesAPersonMustApprove(judgment.onServicesWhoseChangesAPersonMustApprove), UnrecognizedTasks: commandUnrecognizedTasks(judgment.recognition.UnrecognizedTasks)}
	for _, task := range judgment.recognition.Tasks {
		kind := task.Kind()
		out.Tasks = append(out.Tasks, CommandCheckedPlaybookTask{Path: task.Path, Node: task.Node, Module: task.Action, ActionKind: kind, ActionKindTitle: kind.Title()})
	}
	return out, nil
}

// reason says why the playbook takes its rule on the inventory.
func (judgment playbookJudgment) reason(inventory string) string {
	if listed := judgment.listedRuleText(inventory, "the playbook takes the listed rule instead of the rules of its kinds of action"); listed != "" {
		return listed
	}
	if judgment.servicesListDecides() {
		return "A node task acts on a service on the list of services whose changes a person must approve, so the playbook waits for a person whatever the rules for its kinds of action, with the number of approvals required by the strictest rule, and at least 1 approval: " + judgment.setting.Title() + "."
	}
	if judgment.setting.Rule == actThenReport {
		return "Every kind of action of the playbook is set to Act, then report on " + inventoryText(inventory) + ", so MMHotline runs it without a person."
	}
	var strictest []string
	for _, kind := range judgment.kindSettings {
		if kind.AgentPlaybookRule == judgment.setting {
			strictest = append(strictest, kind.ActionKindTitle)
		}
	}
	return "On " + inventoryText(inventory) + ", the strictest rule of the playbook's kinds of action is " + judgment.setting.Title() + ", the rule of " + strings.Join(strictest, ", ") + "."
}

// Text is the answer as check-whether-a-playbook-needs-a-person prints it.
// Its first line answers the command's question.
func (c CommandWhetherAPlaybookNeedsAPerson) Text() string {
	answer := "Runs without a person: " + c.RuleTitle + "."
	switch {
	case c.NeedsAPerson:
		answer = "Needs a person: " + c.RuleTitle + "."
	case c.Rule == warnWaitThenAct:
		answer = "Runs without a person unless someone presses Stop: " + c.RuleTitle + "."
	}
	lines := []string{answer, "Why: " + c.Reason}
	if c.ReviewAgentWouldReviewThePlaybook {
		lines = append(lines, reviewAgentWouldReviewText)
	}
	lines = append(lines,
		"Checked "+playbookInInventoryText(c.Playbook, c.Inventory)+" at commit "+c.CommitID+" of MMHotline's copy of the systems repository.",
		"Node tasks, as mmx explain listed them, with their kinds of action:",
	)
	for _, task := range c.Tasks {
		lines = append(lines, "  "+task.Path+" on "+task.Node+": "+task.Module+"; "+task.ActionKindTitle)
	}
	lines = append(lines, "Rules for the playbook's kinds of action on "+inventoryText(c.Inventory)+":")
	for _, kind := range c.ActionKinds {
		lines = append(lines, "  "+kind.Text())
	}
	if len(c.TasksOnServicesWhoseChangesAPersonMustApprove) > 0 {
		lines = append(lines, "Tasks on services whose changes a person must approve:")
		for _, task := range c.TasksOnServicesWhoseChangesAPersonMustApprove {
			lines = append(lines, "  "+task.Path+" ("+task.Module+") "+task.Reason)
		}
		lines = append(lines, "  "+changeServicesWhoseChangesAPersonMustApproveText)
	}
	if len(c.UnrecognizedTasks) > 0 {
		lines = append(lines, "Tasks MMHotline did not recognize, and why:")
		for _, task := range c.UnrecognizedTasks {
			lines = append(lines, "  "+task.Path+" ("+task.Module+"): "+task.Reason)
		}
	}
	return strings.Join(lines, "\n")
}
