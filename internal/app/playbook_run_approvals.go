package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

// Approvals for "Get permission, then act": MMHotline asks the team at sos,
// any team member approves or refuses on the page, and MMHotline runs an
// approved playbook from saved state and hands the result to the agent:
// docs/changing-mmhotline/design/triage-agent.md#approvals. "Warn, wait, then act" shares the
// record, the run, and the way back to the agent.
//
// triage_agent_playbook_runs_waiting_for_a_person holds one row each time
// MMHotline judged that a requested playbook waits for a person, with the
// playbook's text and mmx explain's output as judged, so people decide on
// what MMHotline judged. A row whose rule is get_permission_then_act
// is an approval request. Its state, and the code that changes it:
//
//	open → approved      ApprovePlaybookRun, once enough different members approved
//	open → refused       RefusePlaybookRun; one refusal ends it
//	approved → running   DriveApprovedPlaybookRuns, as the background loop starts the run
//	running → ran_and_succeeded, ran_and_failed, could_not_run, or stale
//	                     runApprovedPlaybook, from what MMX did; stale when the
//	                     copy moved past the judged commit and MMHotline judged again
//	running → ran_and_failed
//	                     failApprovedPlaybookRunsCutByRestart, at MMHotline's start:
//	                     a run that may have started is never repeated
//
// A row whose rule is warn_wait_then_act is a warned run
// (playbook_run_warnings.go). Its run time is judged_at plus
// warning_wait_minutes, both saved when MMHotline judged, so a restart keeps
// it. Nobody stopping it by then counts as the approval, so from then on it
// runs as an approved request does:
//
//	open → stopped       StopWarnedPlaybookRun, before the run time
//	open → running       DriveApprovedPlaybookRuns, at or after the run time
//	running → …          as for an approval request
//
// result_for_the_agent says where the result goes: none, waits_for_the_next_run
// of the request's triage session, sent in result_sent_in_run_id, or
// could_not_reach_the_agent when the session had ended.
//
// One approval can cover a known sequence of listed playbooks in one triage
// session (approvalThatCovers). its_approval_also_approves_in_the_same_session
// holds, on an approval request, the names its approval also approves once,
// saved when MMHotline judged, as the page showed them before the click. A
// covered run is a row of its own, created approved, whose
// covered_by_approval_request_id names that approval request. The covered
// rows are the record of which covers were used, so no other table is
// needed, and the unique index lets each named playbook use one approval's
// cover once.
const playbookRunApprovalSchema = `
CREATE TABLE triage_agent_playbook_runs_waiting_for_a_person (
 id TEXT PRIMARY KEY,
 run_id TEXT NOT NULL, request_id TEXT NOT NULL, triage_session_id TEXT NOT NULL,
 playbook TEXT NOT NULL, inventory TEXT NOT NULL, commit_id TEXT NOT NULL,
 action_kinds TEXT NOT NULL, playbook_text TEXT NOT NULL, mmx_explain_output TEXT NOT NULL,
 rule TEXT NOT NULL CHECK(rule IN ('get_permission_then_act','warn_wait_then_act')),
 warning_wait_minutes INTEGER NOT NULL, approvals_needed INTEGER NOT NULL,
 rule_from_listed_playbook INTEGER NOT NULL DEFAULT 0 CHECK(rule_from_listed_playbook IN (0,1)),
 its_approval_also_approves_in_the_same_session TEXT NOT NULL DEFAULT '',
 covered_by_approval_request_id TEXT NOT NULL DEFAULT '',
 judged_at INTEGER NOT NULL, incident_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('open','approved','refused','stopped','running','stale','ran_and_succeeded','ran_and_failed','could_not_run')),
 state_changed_at INTEGER NOT NULL, result_message TEXT NOT NULL DEFAULT '',
 stopped_by_member_id TEXT NOT NULL DEFAULT '', stop_reason TEXT NOT NULL DEFAULT '',
 result_for_the_agent TEXT NOT NULL DEFAULT 'none' CHECK(result_for_the_agent IN ('none','waits_for_the_next_run','sent','could_not_reach_the_agent')),
 result_sent_in_run_id TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(run_id, request_id) REFERENCES triage_agent_requests(run_id, request_id)
);
CREATE INDEX triage_agent_playbook_runs_waiting_for_a_person_state ON triage_agent_playbook_runs_waiting_for_a_person(state, state_changed_at);
CREATE INDEX triage_agent_playbook_runs_waiting_for_a_person_session ON triage_agent_playbook_runs_waiting_for_a_person(triage_session_id, result_for_the_agent);
CREATE UNIQUE INDEX triage_agent_playbook_runs_waiting_for_a_person_one_use_of_a_cover ON triage_agent_playbook_runs_waiting_for_a_person(covered_by_approval_request_id, playbook) WHERE covered_by_approval_request_id<>'';
CREATE TABLE playbook_run_approval_decisions (
 id INTEGER PRIMARY KEY,
 approval_request_id TEXT NOT NULL REFERENCES triage_agent_playbook_runs_waiting_for_a_person(id),
 member_id TEXT NOT NULL REFERENCES members(id),
 decision TEXT NOT NULL CHECK(decision IN ('approve','refuse')),
 reason TEXT NOT NULL, decided_at INTEGER NOT NULL,
 UNIQUE(approval_request_id, member_id, decision)
);
`

// The states of an approval request.
const (
	approvalOpen            = "open"
	approvalApproved        = "approved"
	approvalRefused         = "refused"
	approvalRunning         = "running"
	approvalStale           = "stale"
	approvalRanAndSucceeded = "ran_and_succeeded"
	approvalRanAndFailed    = "ran_and_failed"
	approvalCouldNotRun     = "could_not_run"
	// warnedRunStopped: a person pressed Stop before a warned run's time.
	warnedRunStopped = "stopped"
)

// Where an approval request's result goes.
const (
	resultForTheAgentNone                  = "none"
	resultForTheAgentWaits                 = "waits_for_the_next_run"
	resultForTheAgentSent                  = "sent"
	resultForTheAgentCouldNotReachTheAgent = "could_not_reach_the_agent"
)

// playbookRunApprovalsActor is the incident history's actor for MMHotline's
// asking and the approved runs, shown as MMHotline. It is not "system",
// because MMHotline's own SOS with actor system starts a triage session.
const playbookRunApprovalsActor = "playbook_run_approvals"

// maximumRefusalReasonCharacters bounds a refusal's reason, and a Stop's: a
// short reason for the agent and the history, not a report.
const maximumRefusalReasonCharacters = 500

// maximumApprovalPlaybookTextBytes bounds the playbook's text kept with a
// request, as a review prompt is bounded. A longer file is kept cut, with a
// line that says so; the commit holds it whole.
const maximumApprovalPlaybookTextBytes = 256 << 10

// waitingPlaybookRunColumns are the columns scanApprovalRequest scans.
const waitingPlaybookRunColumns = `id,run_id,request_id,triage_session_id,playbook,inventory,commit_id,action_kinds,playbook_text,mmx_explain_output,rule,warning_wait_minutes,approvals_needed,rule_from_listed_playbook,its_approval_also_approves_in_the_same_session,covered_by_approval_request_id,judged_at,incident_id,state,state_changed_at,result_message,result_for_the_agent,stopped_by_member_id,stop_reason`

// approvalRequest is one row of the waiting table: an approval request or a
// warned run.
type approvalRequest struct {
	ID, RunID, RequestID, TriageSessionID, Playbook, Inventory, CommitID string
	ActionKinds, PlaybookText, MMXExplainOutput, IncidentID              string
	PlaybookRule                                                         AgentPlaybookRule
	// RuleFromListedPlaybook says the rule is the one a person listed for
	// the playbook by name in rules_for_running_agent_playbooks.
	RuleFromListedPlaybook bool
	// ItsApprovalAlsoApprovesInTheSameSession names the listed playbooks an
	// approval of this request also approves once later in its triage
	// session; CoveredByApprovalRequestID names the approval request whose
	// approval covered this run, which then needed no approval of its own.
	ItsApprovalAlsoApprovesInTheSameSession []string
	CoveredByApprovalRequestID              string
	JudgedAt, StateChangedAt                time.Time
	State, ResultMessage, ResultForTheAgent string
	StoppedByMemberID, StopReason           string
}

func scanApprovalRequest(row scanner) (approvalRequest, error) {
	var r approvalRequest
	var judged, changed int64
	var alsoApproves string
	err := row.Scan(&r.ID, &r.RunID, &r.RequestID, &r.TriageSessionID, &r.Playbook, &r.Inventory, &r.CommitID, &r.ActionKinds, &r.PlaybookText, &r.MMXExplainOutput, &r.PlaybookRule.Rule, &r.PlaybookRule.WarningWaitMinutes, &r.PlaybookRule.ApprovalsNeeded, &r.RuleFromListedPlaybook, &alsoApproves, &r.CoveredByApprovalRequestID, &judged, &r.IncidentID, &r.State, &changed, &r.ResultMessage, &r.ResultForTheAgent, &r.StoppedByMemberID, &r.StopReason)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.JudgedAt, r.StateChangedAt = time.Unix(judged, 0).UTC(), time.Unix(changed, 0).UTC()
	r.ItsApprovalAlsoApprovesInTheSameSession = splitPlaybookNames(alsoApproves)
	return r, err
}

// warned says r is a warned run, which runs at its run time unless someone
// presses Stop. A run an earlier approval covered runs as approved instead,
// whatever its own rule.
func (r approvalRequest) warned() bool {
	return r.PlaybookRule.Rule == warnWaitThenAct && r.CoveredByApprovalRequestID == ""
}

// readWaitingPlaybookRun reads a row of the waiting table whose rule
// is setting. A row of the other setting is not found, so approving
// never starts a warned run before its time, and Stop never ends an
// approval request.
func readWaitingPlaybookRun(ctx context.Context, tx *sql.Tx, id, setting string) (approvalRequest, error) {
	return scanApprovalRequest(tx.QueryRowContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person WHERE id=? AND rule=?`, id, setting))
}

// insertPlaybookRunWaitingForAPerson records MMHotline's judgment that a
// playbook waits for a person, with what it judged: the playbook's text and
// mmx explain's output at commitID. A run an earlier approval covered is
// approved at once, with that approval's incident, so the background loop
// runs it next.
func insertPlaybookRunWaitingForAPerson(ctx context.Context, tx *sql.Tx, r approvalRequest) error {
	state := approvalOpen
	if r.CoveredByApprovalRequestID != "" {
		state = approvalApproved
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO triage_agent_playbook_runs_waiting_for_a_person(id,run_id,request_id,triage_session_id,playbook,inventory,commit_id,action_kinds,playbook_text,mmx_explain_output,rule,warning_wait_minutes,approvals_needed,rule_from_listed_playbook,its_approval_also_approves_in_the_same_session,covered_by_approval_request_id,incident_id,judged_at,state,state_changed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.RunID, r.RequestID, r.TriageSessionID, r.Playbook, r.Inventory, r.CommitID, r.ActionKinds, keptPlaybookText(r.PlaybookText), r.MMXExplainOutput, r.PlaybookRule.Rule, r.PlaybookRule.WarningWaitMinutes, r.PlaybookRule.ApprovalsNeeded, r.RuleFromListedPlaybook, joinedPlaybookNames(r.ItsApprovalAlsoApprovesInTheSameSession), r.CoveredByApprovalRequestID, r.IncidentID, r.JudgedAt.Unix(), state, r.JudgedAt.Unix())
	return err
}

// approvalThatCovers finds the approval in the request's triage session
// that also approves the requested playbook, on the same inventory, and that
// no run used yet: an approval request with
// its_approval_also_approves_in_the_same_session naming the playbook, which
// enough current members approved. This is the one place the cover's rule
// is decided; docs/changing-mmhotline/design/triage-agent.md#one-approval-for-a-known-sequence
// gives the reasons:
//   - a woken person approves a known sequence, such as replacing a failed
//     server, once instead of once for each step;
//   - only within the session, while it is open, because the person
//     approved one problem's fix, and a later session is another problem;
//     a software review has no session, so nothing covers its requests;
//   - only listed playbooks, playbooks/NAME.yml, which only a person writes
//     and names in the settings file; never a one-time playbook;
//   - each named playbook once: a second run of it is not the sequence the
//     person approved. Any covered run uses the cover, even one that failed
//     or went stale, because MMHotline cannot tell whether a failed run
//     changed the servers, so running it again needs a person;
//   - only an approval people gave: a refused or open request covers
//     nothing, a covered run's own row covers nothing further, and a member
//     removed since no longer counts toward the approval, as for an open
//     request.
func approvalThatCovers(ctx context.Context, tx *sql.Tx, request TriageAgentPlaybookRunRequest) (approvalRequest, bool, error) {
	name, ok := mmxPlaybookName(request.Playbook)
	if !ok || !strings.HasPrefix(request.Playbook, "playbooks/") {
		return approvalRequest{}, false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person w
WHERE triage_session_id<>'' AND triage_session_id=(SELECT triage_session_id FROM triage_agent_runs WHERE id=?)
 AND EXISTS(SELECT 1 FROM triage_sessions s WHERE s.id=w.triage_session_id AND s.ended_at=0)
 AND inventory=? AND rule=? AND covered_by_approval_request_id='' AND state NOT IN (?,?)
 AND ','||its_approval_also_approves_in_the_same_session||',' LIKE '%,'||?||',%'
 AND NOT EXISTS(SELECT 1 FROM triage_agent_playbook_runs_waiting_for_a_person c WHERE c.covered_by_approval_request_id=w.id AND c.playbook=?)
ORDER BY judged_at,id`, request.RunID, request.Inventory, getPermissionThenAct, approvalOpen, approvalRefused, name, request.Playbook)
	if err != nil {
		return approvalRequest{}, false, err
	}
	var candidates []approvalRequest
	for rows.Next() {
		r, err := scanApprovalRequest(rows)
		if err != nil {
			rows.Close()
			return approvalRequest{}, false, err
		}
		candidates = append(candidates, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return approvalRequest{}, false, err
	}
	for _, r := range candidates {
		counted, _, err := approvalRequestApprovers(ctx, tx, r.ID)
		if err != nil {
			return approvalRequest{}, false, err
		}
		if len(counted) >= r.PlaybookRule.ApprovalsNeeded {
			return r, true, nil
		}
	}
	return approvalRequest{}, false, nil
}

// alsoApprovesText says, before a person approves r, what else the
// approval lets the triage agent run: "Approving also lets the triage agent
// run every_server and destroy_replaced_servers once each later in this
// session, without asking again."; empty when it covers nothing.
func (r approvalRequest) alsoApprovesText() string {
	names := r.ItsApprovalAlsoApprovesInTheSameSession
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "Approving also lets the triage agent run " + names[0] + " once later in this session, without asking again."
	}
	return "Approving also lets the triage agent run " + namesInSentence(names) + " once each later in this session, without asking again."
}

// recordCoveredPlaybookRun records, in the history of the covering
// approval's incident, that MMHotline runs r without asking again and whose
// approval covers it. No alert goes out: nobody needs to decide.
func recordCoveredPlaybookRun(ctx context.Context, tx *sql.Tx, r approvalRequest, coveredBy string, now time.Time) error {
	if r.IncidentID == "" {
		return nil
	}
	incident, err := readIncident(ctx, tx, r.IncidentID)
	if err != nil {
		return err
	}
	return recordIncidentAction(ctx, tx, incident, playbookRunApprovalsActor, "playbook_run_covered_by_an_earlier_approval", "MMHotline runs "+playbookInInventoryText(r.Playbook, r.Inventory)+" at commit "+r.CommitID+" without asking again: "+coveredBy, "", "", now)
}

// coveredByText says which approval covered r and who approved it, such as
// "Barbara approved playbooks/replace_failed_servers.yml in the inventory
// “servers” (approval request 5f0c…), and that approval also approves
// playbooks/every_server.yml once in the same triage session."
func coveredByText(ctx context.Context, tx *sql.Tx, r approvalRequest) (string, error) {
	cover, err := scanApprovalRequest(tx.QueryRowContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person WHERE id=?`, r.CoveredByApprovalRequestID))
	if err != nil {
		return "", err
	}
	approvedBy, err := approvalRequestApproversText(ctx, tx, cover)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(approvedBy, ".") + " (approval request " + cover.ID + "), and that approval also approves " + r.Playbook + " once in the same triage session.", nil
}

// listedPlaybookSentence is " The playbook every_server is listed in
// rules_for_running_agent_playbooks for the inventory “servers” with Warn,
// wait 10 minutes, then act." when r's rule is the one a person listed for
// the playbook, and empty otherwise.
func (r approvalRequest) listedPlaybookSentence() string {
	name, _ := mmxPlaybookName(r.Playbook)
	if !r.RuleFromListedPlaybook {
		return ""
	}
	return " " + capitalize(listedPlaybookRuleText(name, r.Inventory, r.PlaybookRule)) + "."
}

// keptPlaybookText is the playbook's text as MMHotline keeps it with an
// approval request or a review: at most maximumApprovalPlaybookTextBytes,
// then cut with a line that says so.
func keptPlaybookText(text string) string {
	if len(text) <= maximumApprovalPlaybookTextBytes {
		return text
	}
	cut := maximumApprovalPlaybookTextBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[MMHotline keeps the first %d KiB of this playbook; the commit holds all of it.]\n", maximumApprovalPlaybookTextBytes>>10)
}

// askForPlaybookRunApproval asks the team at sos to approve r: an alert from
// MMHotline about an open incident of the run's work (triageRunIncidents), or
// one it opens, so the person on call is woken through the incident's
// escalation. The alert is MMHotline's own: it never waits for the triage
// agent and never starts a session. Its message is what a person needs to
// decide: the agent's reason, from its diagnosis, and why, which says why
// the run waits for a person (approvalReasonText), then a link to the
// request's own page, where a person decides; the incident page shows the
// request in short. The request's page and the answer to the agent keep
// each task.
func (a *App) askForPlaybookRunApproval(ctx context.Context, tx *sql.Tx, r approvalRequest, why string, now time.Time) error {
	reason, err := playbookRunReason(ctx, tx, r)
	if err != nil {
		return err
	}
	// A person woken by the alert may approve from the incident page, so
	// the alert says what else the approval approves.
	if also := r.alsoApprovesText(); also != "" {
		why = strings.TrimSpace(why + " " + also)
	}
	changes := r.serversItChanges()
	in := GenericAlert{
		Severity: "sos",
		Title:    truncateRunes("Approval needed to run "+playbookInInventoryText(r.Playbook, r.Inventory)+changes, 256),
		Message:  truncateRunes(reason+"\n\n"+why+"\n\nThe incident page shows a summary of the playbook run request. Approve or refuse the request on the approval page, which lists each step and the triage agent's full diagnosis: "+strings.TrimRight(a.WebURL, "/")+"/approvals/"+r.ID, maximumAlertMessageBytes/4),
	}
	alert, _, err := admitAlertAboutTriageRun(ctx, tx, r.TriageSessionID, r.RunID, playbookRunApprovalsActor, in, now)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET incident_id=? WHERE id=?`, alert.IncidentID, r.ID); err != nil {
		return err
	}
	incident, err := readIncident(ctx, tx, alert.IncidentID)
	if err != nil {
		return err
	}
	if err = recordIncidentAction(ctx, tx, incident, playbookRunApprovalsActor, "playbook_run_approval_asked", fmt.Sprintf("Approval needed to run %s, at commit %s%s. %s%s", playbookInInventoryText(r.Playbook, r.Inventory), r.CommitID, changes, approvalsNeededText(r.PlaybookRule.ApprovalsNeeded), " "+why), alert.ID, "", now); err != nil {
		return err
	}
	if r.TriageSessionID == "" {
		// A software review knows nothing of a session's trouble.
		return nil
	}
	return wakeIncidentsHeldForTriageAgent(ctx, tx, "MMHotline asked for approval of a playbook run; waking the person on call", now)
}

// maximumServersNamedInATitle keeps an alert title short enough to read in a
// notification; the card lists every task with its server.
const maximumServersNamedInATitle = 3

// serversItChanges says which servers r's playbook can change, from the node
// tasks of the mmx explain output saved with it, such as ": it can change
// app001, app002, and db001", or ": it can change app001, app002, app003, and
// 4 more". An inventory's name, such as production, does not tell a person
// which servers can change. A task MMX knows it skips changes nothing; when it
// skips every task, the text is empty.
func (r approvalRequest) serversItChanges() string {
	// MMHotline saves only output it could read, so an error leaves the
	// title without servers rather than failing the request.
	recognition, err := actionkinds.Recognize([]byte(r.MMXExplainOutput))
	if err != nil {
		return ""
	}
	return serversItChangesText(recognition.Tasks)
}

// unrecognizedTasksText says which node tasks of r's saved mmx explain output
// MMHotline did not recognize, and why, for the incident's history.
func (r approvalRequest) unrecognizedTasksText() string {
	recognition, err := actionkinds.Recognize([]byte(r.MMXExplainOutput))
	if err != nil {
		return ""
	}
	return unrecognizedTasksText(recognition.UnrecognizedTasks)
}

func serversItChangesText(tasks []actionkinds.Task) string {
	if servers := serverListText(tasks); servers != "" {
		return ": it can change " + servers
	}
	return ""
}

// serverListText names the servers the node tasks can change, as
// serversItChanges does, without its start: "app001, app002, and db001".
func serverListText(tasks []actionkinds.Task) string {
	var servers []string
	for _, task := range tasks {
		if task.Status != "skipped" && !slices.Contains(servers, task.Node) {
			servers = append(servers, task.Node)
		}
	}
	slices.Sort(servers)
	if more := len(servers) - maximumServersNamedInATitle; more > 0 {
		servers = append(servers[:maximumServersNamedInATitle], fmt.Sprintf("%d more", more)) //nolint:nilaway // Sliced only when servers holds more names than maximumServersNamedInATitle.
	}
	switch len(servers) {
	case 0:
		return ""
	case 1:
		return servers[0]
	case 2:
		return servers[0] + " and " + servers[1]
	}
	return strings.Join(servers[:len(servers)-1], ", ") + ", and " + servers[len(servers)-1]
}

func approvalsNeededText(needed int) string {
	if needed == 2 {
		return "The playbook run needs approval from 2 different people."
	}
	return "The playbook run needs approval from 1 person."
}

// approvalAskedText ends the answer to the agent, which ends its turn. A
// software review's run has no session to resume.
func approvalAskedText(inSession bool) string {
	if !inSession {
		return " MMHotline asked the team for approval. This run has no triage session to resume, so the result stays with the incident. End your turn now and report."
	}
	return " MMHotline asked the team for approval. When a person approves or refuses, MMHotline resumes your session with the result. Commit nothing more until MMHotline resumes you: a newer commit can make the approval stale, and a person must then approve again. End your turn now and report."
}

// ApprovePlaybookRun records a team member's approval of an approval
// request. Once as many different members as it needs have approved, it is
// approved, and the background loop runs it. A member who already approved
// it is refused, so one person never counts twice.
func (a *App) ApprovePlaybookRun(ctx context.Context, p Principal, approvalRequestID string) error {
	return a.decidePlaybookRunApproval(ctx, p, approvalRequestID, "approve", "")
}

// RefusePlaybookRun records a team member's refusal, with a short reason,
// which ends the approval request: nothing runs, and the reason reaches the
// agent.
func (a *App) RefusePlaybookRun(ctx context.Context, p Principal, approvalRequestID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > maximumRefusalReasonCharacters {
		return fmt.Errorf("%w: give a short reason for refusing, at most %d characters", ErrInvalid, maximumRefusalReasonCharacters)
	}
	return a.decidePlaybookRunApproval(ctx, p, approvalRequestID, "refuse", reason)
}

func (a *App) decidePlaybookRunApproval(ctx context.Context, p Principal, approvalRequestID, decision, reason string) error {
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		// Two approvals must come from two people, which the mmhotline command,
		// with no member ID, is not.
		if m.ID == "" {
			return fmt.Errorf("%w: only a signed-in team member approves or refuses a playbook run", ErrForbidden)
		}
		r, err := readWaitingPlaybookRun(ctx, tx, approvalRequestID, getPermissionThenAct)
		if err != nil {
			return err
		}
		if r.State != approvalOpen {
			return fmt.Errorf("%w: the playbook run is no longer waiting for approval: %s", ErrConflict, approvalStateLabel(r.State))
		}
		if decision == "approve" {
			var approved bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM playbook_run_approval_decisions WHERE approval_request_id=? AND member_id=? AND decision='approve')`, r.ID, m.ID).Scan(&approved); err != nil {
				return err
			}
			if approved {
				return fmt.Errorf("%w: you already approved the playbook run. The playbook run needs approval from another person", ErrConflict)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO playbook_run_approval_decisions(approval_request_id,member_id,decision,reason,decided_at) VALUES(?,?,?,?,?)`, r.ID, m.ID, decision, reason, now.Unix()); err != nil {
			return err
		}
		incident, err := readIncident(ctx, tx, r.IncidentID)
		if err != nil {
			return err
		}
		actor := "member:" + m.ID
		playbook := playbookInInventoryText(r.Playbook, r.Inventory)
		if decision == "refuse" {
			if err = recordIncidentAction(ctx, tx, incident, actor, "playbook_run_refused", "Refused "+playbook+": "+reason, "", "", now); err != nil {
				return err
			}
			return finishApprovalRequest(ctx, tx, r, approvalRefused, m.Name+" refused "+playbook+", so nothing ran. "+m.Name+"'s reason: "+reason, true, now)
		}
		approvers, removed, err := approvalRequestApprovers(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		message := fmt.Sprintf("Approved %s (%d of %d approvals).", playbook, len(approvers), r.PlaybookRule.ApprovalsNeeded)
		for _, name := range removed {
			message += " " + removedApproverText(name)
		}
		if len(approvers) >= r.PlaybookRule.ApprovalsNeeded {
			// The run's result says whether it ran, and at which commit.
			message += " MMHotline starts the run."
			if _, err = tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET state=?,state_changed_at=? WHERE id=?`, approvalApproved, now.Unix(), r.ID); err != nil {
				return err
			}
		}
		return recordIncidentAction(ctx, tx, incident, actor, "playbook_run_approved", message, "", "", now)
	})
}

// approvalRequestApprovers names the members who approved, first first:
// those whose approvals count, and those removed from the team since, whose
// approvals no longer count. A person the team removed may be one it no
// longer trusts, so two approvals always come from two current members.
func approvalRequestApprovers(ctx context.Context, tx *sql.Tx, id string) (counted, removed []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT d.member_id,m.removed_at IS NULL FROM playbook_run_approval_decisions d LEFT JOIN members m ON m.id=d.member_id WHERE d.approval_request_id=? AND d.decision='approve' ORDER BY d.decided_at,d.id`, id)
	if err != nil {
		return nil, nil, err
	}
	type approver struct {
		memberID string
		counts   bool
	}
	var approvers []approver
	for rows.Next() {
		var a approver
		var current sql.NullBool
		if err = rows.Scan(&a.memberID, &current); err != nil {
			rows.Close()
			return nil, nil, err
		}
		a.counts = current.Valid && current.Bool
		approvers = append(approvers, a)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	for _, a := range approvers {
		name, err := incidentMemberName(ctx, tx, a.memberID)
		if err != nil {
			return nil, nil, err
		}
		if a.counts {
			counted = append(counted, name)
		} else {
			removed = append(removed, name)
		}
	}
	return counted, removed, nil
}

// removedApproverText says that a removed member's approval no longer
// counts: "Ben's approval no longer counts, because Ben was removed from
// the team."
func removedApproverText(name string) string {
	return name + "'s approval no longer counts, because " + name + " was removed from the team."
}

// finishApprovalRequest moves a request to its last state with what
// happened, records that in its incident's history, and hands the result to
// the agent when forTheAgent is set: to the next run of the request's
// session, or, when the session has ended, nowhere, which the history says.
func finishApprovalRequest(ctx context.Context, tx *sql.Tx, r approvalRequest, state, result string, forTheAgent bool, now time.Time) error {
	// A refusal and a Stop record themselves. A warned run of a session with
	// no incident, such as one of warn alerts only, has no history.
	if state != approvalRefused && state != warnedRunStopped && r.IncidentID != "" {
		incident, err := readIncident(ctx, tx, r.IncidentID)
		if err != nil {
			return err
		}
		kind := "approved_playbook_run_result"
		if r.warned() {
			kind = "warned_playbook_run_result"
		}
		if err = recordIncidentAction(ctx, tx, incident, playbookRunApprovalsActor, kind, result, "", "", now); err != nil {
			return err
		}
	}
	if r.IncidentID != "" {
		incident, err := readIncident(ctx, tx, r.IncidentID)
		if err != nil {
			return err
		}
		var otherAlerts, otherRequests bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alerts WHERE incident_id=? AND actor<>?), EXISTS(SELECT 1 FROM triage_agent_playbook_runs_waiting_for_a_person WHERE incident_id=? AND id<>? AND state IN ('open','approved','running'))`, r.IncidentID, playbookRunApprovalsActor, r.IncidentID, r.ID).Scan(&otherAlerts, &otherRequests); err != nil {
			return err
		}
		if (incident.Status == "open" || incident.Status == "claimed") && !otherAlerts && !otherRequests {
			ended := approvalStateLabel(state)
			if state == approvalRefused {
				var name string
				if err := tx.QueryRowContext(ctx, `SELECT m.name FROM playbook_run_approval_decisions d JOIN members m ON m.id=d.member_id WHERE d.approval_request_id=? AND d.decision='refuse' ORDER BY d.id DESC LIMIT 1`, r.ID).Scan(&name); err != nil {
					return err
				}
				ended = "refused by " + name
			}
			why := ": this incident only asked for approval of a playbook run, and the request ended (" + ended + ")."
			if _, err := resolveIncidentTx(ctx, tx, incident, playbookRunApprovalsActor, "", why, "", now); err != nil {
				return err
			}
			result += " MMHotline resolved this incident: it only asked for approval of a playbook run, and the request ended (" + ended + ")."
		}
	}
	delivery := resultForTheAgentNone
	if forTheAgent {
		var open bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_sessions WHERE id=? AND ended_at=0)`, r.TriageSessionID).Scan(&open); err != nil {
			return err
		}
		delivery = resultForTheAgentWaits
		if !open {
			delivery = resultForTheAgentCouldNotReachTheAgent
			if err := recordResultCouldNotReachTheAgent(ctx, tx, r, now); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET state=?,state_changed_at=?,result_message=?,result_for_the_agent=? WHERE id=?`, state, now.Unix(), result, delivery, r.ID)
	return err
}

// recordResultCouldNotReachTheAgent says so in the history of the request's
// incident; a warned run without one keeps it on its card alone.
func recordResultCouldNotReachTheAgent(ctx context.Context, tx *sql.Tx, r approvalRequest, now time.Time) error {
	if r.IncidentID == "" {
		return nil
	}
	incident, err := readIncident(ctx, tx, r.IncidentID)
	if err != nil {
		return err
	}
	why := "the playbook request's triage session had ended"
	if r.TriageSessionID == "" {
		why = "the software review that asked has no triage session"
	}
	return recordIncidentAction(ctx, tx, incident, playbookRunApprovalsActor, "playbook_run_result_not_sent_to_triage_agent", "The result of "+playbookInInventoryText(r.Playbook, r.Inventory)+" could not reach the triage agent: "+why+". The result remains in the incident history.", "", "", now)
}

// approvalResultsCouldNotReachTheAgent records, as a session ends, that the
// results still waiting for its next run never reach the agent.
func approvalResultsCouldNotReachTheAgent(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person WHERE triage_session_id=? AND result_for_the_agent=?`, sessionID, resultForTheAgentWaits)
	if err != nil {
		return err
	}
	var waiting []approvalRequest
	for rows.Next() {
		r, err := scanApprovalRequest(rows)
		if err != nil {
			rows.Close()
			return err
		}
		waiting = append(waiting, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range waiting {
		if err = recordResultCouldNotReachTheAgent(ctx, tx, r, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET result_for_the_agent=? WHERE id=?`, resultForTheAgentCouldNotReachTheAgent, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// DriveApprovedPlaybookRuns is the background loop's turn for approved
// playbook runs and for warned runs whose run time came without a Stop: it
// starts each beside the loop, from saved state, so a restart after an
// approval or after a run time neither loses the run nor, once it started,
// repeats it. The run waits for MMHotline's one MMX run at a time.
func (a *App) DriveApprovedPlaybookRuns(ctx context.Context) error {
	var approved []approvalRequest
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person WHERE state=? OR (state=? AND rule=? AND `+warnedRunTimeColumn+`<=?) ORDER BY state_changed_at,id`, approvalApproved, approvalOpen, warnWaitThenAct, now.Unix())
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanApprovalRequest(rows)
			if err != nil {
				rows.Close()
				return err
			}
			approved = append(approved, r)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, r := range approved {
			if _, err = tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET state=?,state_changed_at=? WHERE id=?`, approvalRunning, now.Unix(), r.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range approved {
		a.triageAgent.approvedPlaybookRuns.Go(func() { a.runApprovedPlaybook(ctx, r) })
	}
	return nil
}

// runApprovedPlaybook runs an approved request, or a warned run whose time
// came, with MMX at the commit MMHotline judged, or at a newer commit
// that changes only records the playbook does not read
// (runAtJudgedOrNewerCommit), and records what happened. When the copy moved
// past that commit otherwise, nothing ran, and the approval or the warning
// is stale: MMHotline judges the playbook again at the new commit, which
// runs it when the rules for running the agent's playbooks let it run without a person, or asks for
// approval or warns again.
func (a *App) runApprovedPlaybook(ctx context.Context, r approvalRequest) {
	request := TriageAgentPlaybookRunRequest{RunID: r.RunID, RequestID: r.RequestID, Playbook: r.Playbook, Inventory: r.Inventory}
	ranAt, decision, notCovered := a.runAtJudgedOrNewerCommit(ctx, request, r)
	state, forTheAgent := approvalRanAndFailed, true
	result := r.ranAtText(ranAt) + " " + decision.Message
	switch {
	case decision.CopyMovedToCommitID != "":
		if ctx.Err() != nil {
			// MMHotline is stopping; at its start it records the run as cut short.
			return
		}
		again := a.triageAgent.playbookRuns.DecidePlaybookRun(ctx, request)
		state, decision = approvalStale, again
		result = "Other commits reached MMHotline's copy of the systems repository before the run, so nothing ran at commit " + r.CommitID + ". The " + r.whatLetItRun() + " does not cover the newer commits: " + notCovered + ". MMHotline checked the playbook again at the newer commit: " + again.Message
		// A new approval request or warned run carries the result to the
		// agent later.
		switch {
		case again.ApprovalRequestID != "":
			forTheAgent = false
			result += " MMHotline asked for approval again."
		case again.CoveredPlaybookRunID != "":
			forTheAgent = false
			result += " An earlier approval in the triage session covers the playbook, so MMHotline runs it next."
		case !again.RunsUnlessStoppedAt.IsZero():
			forTheAgent = false
			result += " MMHotline warned the team again: it runs the playbook at " + warnedRunTimeText(again.RunsUnlessStoppedAt, a.Now()) + " unless someone presses Stop."
		}
	case decision.Outcome == triageagent.RanAndSucceeded:
		state = approvalRanAndSucceeded
	case decision.Outcome == triageagent.Refused:
		// MMX could not start, so no commit ran.
		state, result = approvalCouldNotRun, decision.Message
	}
	if decision.PlaybookRunOutput != "" {
		result += "\n\nPlaybook run output:\n" + endOfPlaybookRunOutput(decision.PlaybookRunOutput)
	}
	// The result is recorded even while MMHotline stops: the run happened.
	ctx = context.WithoutCancel(ctx)
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		current, err := readWaitingPlaybookRun(ctx, tx, r.ID, r.PlaybookRule.Rule)
		if err != nil || current.State != approvalRunning {
			return err
		}
		approvedBy, err := approvalRequestApproversText(ctx, tx, r)
		if err != nil {
			return err
		}
		return finishApprovalRequest(ctx, tx, current, state, approvedBy+" "+result, forTheAgent, a.Now().UTC())
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mmhotline: could not record the approved playbook run: %s\n", err)
	}
}

// approvalRequestApproversText says who approved, such as "Barbara and Ben
// approved playbooks/x.yml in the inventory “production”.", or, for a warned
// run, that nobody stopped it before its run time.
func approvalRequestApproversText(ctx context.Context, tx *sql.Tx, r approvalRequest) (string, error) {
	if r.CoveredByApprovalRequestID != "" {
		return coveredByText(ctx, tx, r)
	}
	if r.PlaybookRule.Rule == warnWaitThenAct {
		return "Nobody stopped " + playbookInInventoryText(r.Playbook, r.Inventory) + " before " + warnedRunTimeText(r.runsAt(), r.JudgedAt) + ".", nil
	}
	names, _, err := approvalRequestApprovers(ctx, tx, r.ID)
	if err != nil {
		return "", err
	}
	who := strings.Join(names, " and ")
	if who == "" {
		who = "A person"
	}
	return who + " approved " + playbookInInventoryText(r.Playbook, r.Inventory) + ".", nil
}

// failApprovedPlaybookRunsCutByRestart records, at MMHotline's start, each
// approved run that was running when MMHotline's process ended. Systemd
// stopped MMX with the service, so the playbook may have stopped partway
// or not started; MMHotline never runs it again, because a fix that ran
// partway may not be safe to repeat.
func failApprovedPlaybookRunsCutByRestart(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person WHERE state=?`, approvalRunning)
	if err != nil {
		return err
	}
	var cut []approvalRequest
	for rows.Next() {
		r, err := scanApprovalRequest(rows)
		if err != nil {
			rows.Close()
			return err
		}
		cut = append(cut, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range cut {
		approvedBy, err := approvalRequestApproversText(ctx, tx, r)
		if err != nil {
			return err
		}
		result := approvedBy + " MMHotline restarted while the run was going, so the playbook may have stopped partway or not started. MMHotline does not run the playbook again. Check the servers before requesting another playbook run."
		if err = finishApprovalRequest(ctx, tx, r, approvalRanAndFailed, result, true, now); err != nil {
			return err
		}
	}
	return nil
}

// approvalResultsForSession lists the results that wait for the session's
// next run, oldest first.
func approvalResultsForSession(ctx context.Context, tx *sql.Tx, sessionID string) (ids, results []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,result_message,state_changed_at FROM triage_agent_playbook_runs_waiting_for_a_person WHERE triage_session_id=? AND result_for_the_agent=? ORDER BY state_changed_at,id`, sessionID, resultForTheAgentWaits)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, result string
		var recorded int64
		if err = rows.Scan(&id, &result, &recorded); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids, results = append(ids, id), append(results, "Recorded at "+time.Unix(recorded, 0).UTC().Format("2006-01-02 15:04:05 UTC")+".\n"+result)
	}
	return ids, results, errors.Join(rows.Err(), rows.Close())
}

// approvalStateLabel names a state for people.
func approvalStateLabel(state string) string {
	switch state {
	case approvalOpen:
		return "Waiting for approval"
	case approvalApproved:
		return "Approved; MMHotline starts the run next"
	case approvalRefused:
		return "Refused"
	case warnedRunStopped:
		return "Stopped"
	case approvalRunning:
		return "Running"
	case approvalStale:
		return "Stale: the systems repository changed before the run, so nothing ran"
	case approvalRanAndSucceeded:
		return "Ran and succeeded"
	case approvalRanAndFailed:
		return "Ran and failed"
	case approvalCouldNotRun:
		return "Could not run"
	}
	return state
}

// playbookRunReason reads the diagnosis saved with the agent's request.
func playbookRunReason(ctx context.Context, tx *sql.Tx, r approvalRequest) (string, error) {
	var diagnosis string
	if err := tx.QueryRowContext(ctx, `SELECT diagnosis FROM triage_agent_requests WHERE run_id=? AND request_id=?`, r.RunID, r.RequestID).Scan(&diagnosis); err != nil {
		return "", err
	}
	if summary := firstProseParagraph(diagnosis); summary != "" {
		return "The triage agent's reason: " + summary, nil
	}
	return "The triage agent gave its reason only in its diagnosis, on the page linked below.", nil
}
