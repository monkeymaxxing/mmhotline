package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mmhotline/internal/actionkinds"
)

// ApprovalRequestsCommandRequest is the input of `mmhotline
// approval-requests list` and `show`.
type ApprovalRequestsCommandRequest struct {
	ApprovalRequestID string `json:"approval_request_id,omitempty"`
}

// CommandApprovalRequest is a playbook run that waits or waited for a
// person, as its card shows it: an approval request, or a warned run.
type CommandApprovalRequest struct {
	ApprovalRequestID string `json:"approval_request_id"`
	Playbook          string `json:"playbook"`
	Inventory         string `json:"inventory"`
	CommitID          string `json:"commit_id"`
	// AgentPlaybookRule is get_permission_then_act with approvals_needed, or
	// warn_wait_then_act with warning_wait_minutes.
	AgentPlaybookRule
	// ListedPlaybookRule says the rule is the one a person listed for the
	// playbook by name in rules_for_running_agent_playbooks; absent when the
	// rule came from the playbook's kinds of action.
	ListedPlaybookRule string `json:"listed_playbook_rule,omitempty"`
	// ItsApprovalAlsoApprovesInTheSameSession names the listed playbooks
	// this request's approval also approves once later in its triage
	// session; CoveredByApprovalRequestID names the earlier approval that
	// covered this run, and CoveredBy says who approved it.
	ItsApprovalAlsoApprovesInTheSameSession []string                  `json:"its_approval_also_approves_in_the_same_session,omitempty"`
	CoveredByApprovalRequestID              string                    `json:"covered_by_approval_request_id,omitempty"`
	CoveredBy                               string                    `json:"covered_by,omitempty"`
	State                                   string                    `json:"state"`
	StateLabel                              string                    `json:"state_label"`
	AskedAt                                 time.Time                 `json:"asked_at"`
	StateChangedAt                          time.Time                 `json:"state_changed_at"`
	RunsAt                                  time.Time                 `json:"runs_at,omitzero"`
	IncidentID                              string                    `json:"incident_id,omitempty"`
	TriageSessionID                         string                    `json:"triage_session_id,omitempty"`
	KindsOfAction                           []string                  `json:"kinds_of_action"`
	Tasks                                   []CommandApprovalTask     `json:"tasks"`
	UnrecognizedTasks                       []CommandUnrecognizedTask `json:"unrecognized_tasks"`
	// TasksOnServicesWhoseChangesAPersonMustApprove are the node tasks on the list of
	// services whose changes a person must approve, as the list is now.
	TasksOnServicesWhoseChangesAPersonMustApprove []CommandTaskOnAServiceWhoseChangesAPersonMustApprove `json:"tasks_on_services_whose_changes_a_person_must_approve"`
	PlaybookText                                  string                                                `json:"playbook_text"`
	ReviewAgentAnswer                             string                                                `json:"review_agent_answer,omitempty"`
	// Diagnosis is why the triage agent asks, as the card's "Why the triage
	// agent asks" shows it.
	Diagnosis string `json:"diagnosis"`
	// EarlierVersionsRefusedByTheReviewAgent are the versions the review
	// agent refused in a row before this request waited, oldest first.
	EarlierVersionsRefusedByTheReviewAgent []CommandRefusedVersion   `json:"earlier_versions_refused_by_the_review_agent"`
	Decisions                              []CommandApprovalDecision `json:"decisions"`
	StoppedBy                              string                    `json:"stopped_by,omitempty"`
	StopReason                             string                    `json:"stop_reason,omitempty"`
	ResultMessage                          string                    `json:"result_message,omitempty"`
	ResultForTheAgent                      string                    `json:"result_for_the_agent"`
}

// CommandRefusedVersion is one version of the fix that the review agent
// refused, as the card's "Earlier versions the review agent refused" shows
// it.
type CommandRefusedVersion struct {
	Playbook          string `json:"playbook"`
	Inventory         string `json:"inventory"`
	CommitID          string `json:"commit_id"`
	Diagnosis         string `json:"diagnosis"`
	ReviewAgentReason string `json:"review_agent_reason"`
	PlaybookText      string `json:"playbook_text"`
}

// CommandApprovalTask is one node task the playbook runs, as mmx explain
// listed it when MMHotline judged the playbook.
type CommandApprovalTask struct {
	Path       string                 `json:"path"`
	Node       string                 `json:"node"`
	Module     string                 `json:"module"`
	ModuleKind string                 `json:"module_kind,omitempty"`
	Status     string                 `json:"status"`
	Arguments  []actionkinds.Argument `json:"arguments"`
}

// CommandUnrecognizedTask is a node task MMHotline did not recognize, or a
// file a fleet stage saves on MMHotline's server, with why, as the card
// shows it.
type CommandUnrecognizedTask struct {
	Path string `json:"path"`
	// Module is the node task's module, or save_on_controller_machine.
	Module string `json:"module"`
	Reason string `json:"reason"`
}

// CommandTaskOnAServiceWhoseChangesAPersonMustApprove is a node task that acts on a
// service on the list of services whose changes a person must approve, or on a unit
// whose name MMX knows only during the run, with why, as the card shows it.
type CommandTaskOnAServiceWhoseChangesAPersonMustApprove struct {
	Path   string `json:"path"`
	Node   string `json:"node"`
	Module string `json:"module"`
	// ServiceName is the listed unit, such as pgautofailover.service; absent
	// when MMX knows the unit's name only during the run.
	ServiceName string `json:"service_name,omitempty"`
	Reason      string `json:"reason"`
}

// CommandApprovalDecision is one person's approval or refusal.
type CommandApprovalDecision struct {
	MemberName string    `json:"member_name"`
	Decision   string    `json:"decision"`
	Reason     string    `json:"reason,omitempty"`
	DecidedAt  time.Time `json:"decided_at"`
}

func (a *App) approvalRequestsCommand(ctx context.Context, p Principal, action string, request ApprovalRequestsCommandRequest, result *CommandResult) error {
	switch action {
	case "list":
		open, _, err := a.ApprovalRequests(ctx, p)
		if err != nil {
			return err
		}
		result.ApprovalRequests = []CommandApprovalRequest{}
		for _, view := range open {
			result.ApprovalRequests = append(result.ApprovalRequests, commandApprovalRequest(view))
		}
		result.Message = fmt.Sprintf("Listed %s waiting for a person, newest first. Team members approve, refuse, or stop playbook runs at %s/approvals.", countText(len(open), "playbook run", "playbook runs"), a.WebURL)
		return nil
	case "show":
		if request.ApprovalRequestID == "" {
			return fmt.Errorf("%w: --approval-request-id is required", ErrInvalid)
		}
		view, err := a.ApprovalRequest(ctx, p, request.ApprovalRequestID)
		if err != nil {
			return err
		}
		out := commandApprovalRequest(view)
		result.ApprovalRequest = &out
		if view.Open() {
			result.Message = fmt.Sprintf("Team members approve, refuse, or stop the playbook run at %s/approvals.", a.WebURL)
		}
		return nil
	}
	return ErrNotFound
}

func commandApprovalRequest(view ApprovalRequestView) CommandApprovalRequest {
	r := view.request
	out := CommandApprovalRequest{ApprovalRequestID: r.ID, Playbook: r.Playbook, Inventory: r.Inventory, CommitID: r.CommitID, AgentPlaybookRule: r.PlaybookRule, ListedPlaybookRule: view.ListedPlaybookRule, State: r.State, StateLabel: view.StateLabel, AskedAt: r.JudgedAt, StateChangedAt: r.StateChangedAt, IncidentID: r.IncidentID, TriageSessionID: r.TriageSessionID,
		KindsOfAction: []string{}, Tasks: []CommandApprovalTask{}, PlaybookText: r.PlaybookText, ReviewAgentAnswer: view.Review, Diagnosis: view.Diagnosis, EarlierVersionsRefusedByTheReviewAgent: []CommandRefusedVersion{}, Decisions: []CommandApprovalDecision{},
		StoppedBy: view.StoppedBy, StopReason: view.StopReason, ResultMessage: r.ResultMessage, ResultForTheAgent: r.ResultForTheAgent,
		ItsApprovalAlsoApprovesInTheSameSession: r.ItsApprovalAlsoApprovesInTheSameSession, CoveredByApprovalRequestID: r.CoveredByApprovalRequestID, CoveredBy: view.CoveredBy}
	if view.Warned {
		out.RunsAt = r.runsAt()
	}
	for _, kind := range strings.Split(r.ActionKinds, ",") {
		if kind != "" {
			out.KindsOfAction = append(out.KindsOfAction, kind)
		}
	}
	// The card reads the same saved output; MMHotline saves only output it
	// could read.
	if recognition, err := actionkinds.Recognize([]byte(r.MMXExplainOutput)); err == nil {
		for _, task := range recognition.Tasks {
			arguments := append([]actionkinds.Argument{}, task.Arguments...)
			out.Tasks = append(out.Tasks, CommandApprovalTask{Path: task.Path, Node: task.Node, Module: task.Action, ModuleKind: task.ModuleKind, Status: task.Status, Arguments: arguments})
		}
	}
	out.UnrecognizedTasks = commandUnrecognizedTasks(view.UnrecognizedTasks)
	out.TasksOnServicesWhoseChangesAPersonMustApprove = commandTasksOnServicesWhoseChangesAPersonMustApprove(view.TasksOnServicesWhoseChangesAPersonMustApprove)
	for _, version := range view.EarlierVersionsRefused {
		out.EarlierVersionsRefusedByTheReviewAgent = append(out.EarlierVersionsRefusedByTheReviewAgent, CommandRefusedVersion{Playbook: version.Playbook, Inventory: version.Inventory, CommitID: version.CommitID, Diagnosis: version.Diagnosis, ReviewAgentReason: version.Reason, PlaybookText: version.PlaybookText})
	}
	for _, decision := range view.Decisions {
		out.Decisions = append(out.Decisions, CommandApprovalDecision{MemberName: decision.MemberName, Decision: decision.decision, Reason: decision.Reason, DecidedAt: decision.decidedAt})
	}
	return out
}

// commandUnrecognizedTasks lists the tasks as approval-requests show and
// agent-playbook-rules check-whether-a-playbook-needs-a-person print them; an empty
// list when there are none.
func commandUnrecognizedTasks(tasks []actionkinds.Unrecognized) []CommandUnrecognizedTask {
	out := []CommandUnrecognizedTask{}
	for _, task := range tasks {
		out = append(out, CommandUnrecognizedTask{Path: task.Path, Module: task.Action, Reason: task.Reason})
	}
	return out
}

// commandTasksOnServicesWhoseChangesAPersonMustApprove lists the tasks as approval-requests
// show and agent-playbook-rules check-whether-a-playbook-needs-a-person print them;
// an empty list when there are none.
func commandTasksOnServicesWhoseChangesAPersonMustApprove(tasks []taskOnAServiceWhoseChangesAPersonMustApprove) []CommandTaskOnAServiceWhoseChangesAPersonMustApprove {
	out := []CommandTaskOnAServiceWhoseChangesAPersonMustApprove{}
	for _, task := range tasks {
		out = append(out, CommandTaskOnAServiceWhoseChangesAPersonMustApprove{Path: task.Path, Node: task.Node, Module: task.Action, ServiceName: task.ServiceName, Reason: task.Reason})
	}
	return out
}

// Line is the request in one line, as the list prints it.
func (r CommandApprovalRequest) Line() string {
	return fmt.Sprintf("Playbook run %s: %s. %s. Asked at %s.", r.ApprovalRequestID, playbookInInventoryText(r.Playbook, r.Inventory), r.StateLabel, commandTime(r.AskedAt))
}

// Text is the request with what it would run, as its card shows it.
func (r CommandApprovalRequest) Text() string {
	rule := "Rule: " + r.AgentPlaybookRule.Title() + "."
	if r.ListedPlaybookRule != "" {
		rule += " " + r.ListedPlaybookRule
	}
	lines := []string{r.Line(), rule + " Commit: " + r.CommitID + "."}
	if len(r.ItsApprovalAlsoApprovesInTheSameSession) > 0 {
		lines = append(lines, "Its approval also approves, once each later in the same triage session: "+strings.Join(r.ItsApprovalAlsoApprovesInTheSameSession, ", ")+".")
	}
	if r.CoveredBy != "" {
		lines = append(lines, "Approved without a new request: "+r.CoveredBy)
	}
	if !r.RunsAt.IsZero() {
		lines = append(lines, "Runs at: "+commandTime(r.RunsAt)+".")
	}
	if r.IncidentID != "" {
		lines = append(lines, "SOS incident: "+r.IncidentID)
	}
	if r.TriageSessionID != "" {
		lines = append(lines, "Triage session: "+r.TriageSessionID)
	}
	var kinds []string
	for _, kind := range r.KindsOfAction {
		kinds = append(kinds, actionkinds.Kind(kind).Title())
	}
	lines = append(lines, "Kinds of action in the playbook: "+listOrNone(kinds)+".", "What the playbook can change on each server, as listed by mmx explain:")
	for _, task := range r.Tasks {
		module := task.Module
		if task.ModuleKind != "" {
			module += " (" + task.ModuleKind + ")"
		}
		lines = append(lines, fmt.Sprintf("  %s on %s: %s, %s", task.Path, task.Node, module, task.Status))
		for _, argument := range task.Arguments {
			lines = append(lines, fmt.Sprintf("    %s = %v (%s)", argument.Field, argument.Value, argument.Status))
		}
	}
	if len(r.UnrecognizedTasks) > 0 {
		lines = append(lines, "Tasks MMHotline did not recognize, and why:")
		for _, task := range r.UnrecognizedTasks {
			lines = append(lines, "  "+task.Path+" ("+task.Module+"): "+task.Reason)
		}
	}
	if len(r.TasksOnServicesWhoseChangesAPersonMustApprove) > 0 {
		lines = append(lines, "Tasks on services whose changes a person must approve:")
		for _, task := range r.TasksOnServicesWhoseChangesAPersonMustApprove {
			lines = append(lines, "  "+task.Path+" ("+task.Module+") "+task.Reason)
		}
		lines = append(lines, "  "+changeServicesWhoseChangesAPersonMustApproveText)
	}
	if r.ReviewAgentAnswer != "" {
		lines = append(lines, "Review agent: "+r.ReviewAgentAnswer)
	}
	if r.Diagnosis != "" {
		lines = append(lines, "Why the triage agent asks:", "  "+indentedText(r.Diagnosis, "  "))
	}
	if len(r.EarlierVersionsRefusedByTheReviewAgent) > 0 {
		lines = append(lines, "Earlier versions the review agent refused, oldest first:")
		for index, version := range r.EarlierVersionsRefusedByTheReviewAgent {
			lines = append(lines, fmt.Sprintf("  %d. %s, at commit %s. Review agent's reason: %s", index+1, playbookInInventoryText(version.Playbook, version.Inventory), version.CommitID, indentedText(version.ReviewAgentReason, "     ")),
				"     Why the triage agent asked:", "       "+indentedText(version.Diagnosis, "       "),
				"     The playbook's text the review agent saw:", "       "+indentedText(version.PlaybookText, "       "))
		}
	}
	for _, decision := range r.Decisions {
		line := fmt.Sprintf("%s %sd the playbook run at %s.", decision.MemberName, decision.Decision, commandTime(decision.DecidedAt))
		if decision.Reason != "" {
			line += " Reason: " + decision.Reason
		}
		lines = append(lines, line)
	}
	if r.StoppedBy != "" {
		lines = append(lines, "Stopped by "+r.StoppedBy+". Reason: "+r.StopReason)
	}
	if r.ResultMessage != "" {
		lines = append(lines, "Result: "+indentedText(r.ResultMessage, "  "))
	}
	lines = append(lines, "The playbook's text, and any files it copies, at the checked commit:", "  "+indentedText(r.PlaybookText, "  "))
	return strings.Join(lines, "\n")
}
