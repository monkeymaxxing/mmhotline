package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// The triage agent's requests during a run, a playbook run or its report,
// which the run's relay sends up the run socket:
// docs/changing-mmhotline/design/triage-agent.md#requests-from-the-agent.

// TriageAgentPlaybookRunRequest is a playbook run the triage agent asked for.
// The commits it sent with the request, if any, are on origin.
type TriageAgentPlaybookRunRequest struct {
	RunID     string
	RequestID string
	// Playbook is relative to the systems repository, such as
	// one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml.
	Playbook  string
	Inventory string
}

// TriageAgentPlaybookRunDecision is what MMHotline did with a playbook run
// request. Its message and output reach the agent, so they hold nothing the
// agent may not read, such as a vault password.
type TriageAgentPlaybookRunDecision struct {
	// Outcome is ran_and_succeeded, ran_and_failed, waits_for_a_person, or
	// refused, also when MMHotline could not run the playbook.
	Outcome triageagent.RequestOutcome
	Message string
	// CopyMovedToCommitID is nonempty when the copy advanced past the judged
	// commit before MMX started. Nothing ran; the caller must check again.
	CopyMovedToCommitID string
	// PlaybookRunOutput is the playbook run's whole output when it ran. The
	// runner records it with the request; the agent gets its end.
	PlaybookRunOutput string
	// ApprovalRequestID names the approval request MMHotline opened when the
	// run waits with Get permission, then act.
	ApprovalRequestID string
	// RunsUnlessStoppedAt is when MMHotline runs a playbook that waits with
	// Warn, wait, then act, unless someone presses Stop first.
	RunsUnlessStoppedAt time.Time
	// CoveredPlaybookRunID names the approved run MMHotline recorded when an
	// earlier approval in the session covers the request (approvalThatCovers);
	// the background loop runs it, and its result resumes the session.
	CoveredPlaybookRunID string
}

// TriageAgentPlaybookRunDecider decides from the rules for running the agent's playbooks whether
// MMHotline runs a playbook the agent asked for, and runs it with MMX. The
// App is the decider; tests stand in their own.
type TriageAgentPlaybookRunDecider interface {
	DecidePlaybookRun(ctx context.Context, request TriageAgentPlaybookRunRequest) TriageAgentPlaybookRunDecision
}

// maximumPathsThatWaitInAnswer bounds the paths an answer lists, so an agent
// that committed a formatted repository still gets an answer that fits one
// run socket line.
const maximumPathsThatWaitInAnswer = 50

var errTriageAgentRequestIDUsed = errors.New("request ID used before")

// answerTriageAgentRequest records one request of the run, decides it, and
// sends the answer to the run. It returns an error only when the request
// cannot be answered at all, which fails the run.
func (a *App) answerTriageAgentRequest(run *triageAgentActiveRun, event triageagent.Event) error {
	if err := triageagent.CheckRequestID(event.RequestID); err != nil {
		return fmt.Errorf("The sandbox sent a request MMHotline cannot answer: %s.", err)
	}
	// The agent holds the credential, so nothing it sends is kept with it.
	request := triageagent.AgentRequest{Kind: triageagent.ReportRequest, AlertSeverity: run.redact(event.AlertSeverity), ReportText: run.redact(event.ReportText), AboutAlertIDs: event.AboutAlertIDs}
	switch event.Event {
	case "playbook_run_requested":
		request = triageagent.AgentRequest{Kind: triageagent.PlaybookRunRequest, Playbook: run.redact(event.Playbook), Inventory: run.redact(event.Inventory), Diagnosis: run.redact(event.Diagnosis), AboutAlertIDs: event.AboutAlertIDs}
	case "commit_review":
		request.Kind, request.CommitID, request.CommitReviewDecision = triageagent.CommitReviewRequest, run.redact(event.CommitID), run.redact(event.CommitReviewDecision)
	}
	// What the report or request covers, in a triage session's run: the
	// alerts it named, checked here, at MMHotline's trust boundary.
	var coverage alertCoverage
	err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
		var used bool
		if err := tx.QueryRowContext(run.ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_requests WHERE run_id=? AND request_id=?)`, run.id, event.RequestID).Scan(&used); err != nil {
			return err
		}
		if used {
			return errTriageAgentRequestIDUsed
		}
		// A commit review request that does not pass its checks keeps no
		// decision, as the column allows only the two.
		decision := ""
		if triageagent.CheckAgentRequest(request) == nil {
			decision = request.CommitReviewDecision
		}
		if _, err := tx.ExecContext(run.ctx, `INSERT INTO triage_agent_requests(run_id,request_id,kind,playbook,inventory,alert_severity,report_text,diagnosis,commit_id,commit_review_decision,requested_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, run.id, event.RequestID, request.Kind, request.Playbook, request.Inventory, request.AlertSeverity, request.ReportText, request.Diagnosis, request.CommitID, decision, a.Now().Unix()); err != nil {
			return err
		}
		itemKind := map[string]string{triageagent.ReportRequest: coveredByReport, triageagent.PlaybookRunRequest: coveredByPlaybookRequest}[request.Kind]
		if run.purpose != "triage_session" || itemKind == "" || triageagent.CheckAgentRequest(request) != nil {
			return nil
		}
		var err error
		coverage, err = recordCoveredAlerts(run.ctx, tx, run.id, itemKind, event.RequestID, request.AlertSeverity, request.AboutAlertIDs)
		return err
	})
	if errors.Is(err, errTriageAgentRequestIDUsed) {
		return fmt.Errorf("The sandbox sent the request ID %s a second time.", event.RequestID)
	}
	if err != nil {
		return errors.New("The run's request could not be saved.")
	}
	answer := a.decideTriageAgentRequest(run, event.RequestID, request, event.AgentCommitsBundle)
	answer.RequestID = event.RequestID
	if text := coverage.answerText(map[bool]string{true: "report", false: "playbook run request"}[request.Kind == triageagent.ReportRequest]); text != "" && answer.Outcome != triageagent.Refused {
		answer.Message += " " + text
	}
	// An MMX run outlasts a triage run that ended meanwhile, and so does the
	// record of what MMHotline answered.
	ctx := context.WithoutCancel(run.ctx)
	// A playbook run request takes held warns over only where MMHotline
	// asks a person, in the decision's own transaction
	// (judgeAndRunPlaybook); a report in the transaction that sends it.
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE triage_agent_requests SET outcome=?,message=?,answered_at=? WHERE run_id=? AND request_id=?`, answer.Outcome, answer.Message, a.Now().Unix(), run.id, event.RequestID)
		return err
	}); err != nil {
		return errors.New("The answer to the run's request could not be saved.")
	}
	// A run that is ending reads no answers; the relay has told the command.
	_ = a.writeTriageAgentControl(run, triageagent.Control{Answer: &answer})
	return nil
}

// decideTriageAgentRequest returns the answer for the agent.
func (a *App) decideTriageAgentRequest(run *triageAgentActiveRun, requestID string, request triageagent.AgentRequest, bundle []byte) triageagent.Answer {
	refused := func(message string) triageagent.Answer {
		return triageagent.Answer{Outcome: triageagent.Refused, Message: message}
	}
	if err := triageagent.CheckAgentRequest(request); err != nil {
		return refused(requestRefusedText(err))
	}
	if request.Kind == triageagent.CommitReviewRequest {
		return a.decideCommitReview(run, request)
	}
	if request.Kind == triageagent.ReportRequest {
		// A report reaches people even when the run ends meanwhile.
		if inSystemsRepository(run.purpose) {
			if err := a.deliverTriageAgentReport(context.WithoutCancel(run.ctx), run.id, requestID, request.AlertSeverity, request.ReportText); err != nil {
				return refused("MMHotline could not send your report to the team. Send the report again.")
			}
		}
		return triageagent.Answer{Outcome: triageagent.ReportRecorded, Message: "MMHotline recorded your report at alert severity " + request.AlertSeverity + "."}
	}
	base := run.request.SystemsRepositoryBaseCommitID
	if base == "" {
		return refused("This run has no systems repository, so MMHotline runs no playbooks for it.")
	}
	// A commit review judges the commits of its prompt, which the commits'
	// own text could try to turn into a fix.
	if run.purpose == "commit_review" {
		return refused("A commit review run asks for no playbook runs. Give your decision on each commit with mmhotline triage-agent review-commit.")
	}
	playbook := playbookInInventoryText(request.Playbook, request.Inventory)
	// Refused at once after a person took the session over; the decision
	// checks again, as a Stop can come while MMHotline reads the playbook.
	var takenOver string
	if err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
		var err error
		takenOver, err = takenOverRefusal(run.ctx, tx, run.id)
		return err
	}); err != nil {
		return refused("MMHotline could not read this run's triage session, so it did not run " + playbook + ".")
	}
	if takenOver != "" {
		return refused("MMHotline refused the request: " + takenOver)
	}
	var answer triageagent.Answer
	if len(bundle) > 0 {
		commits, err := systemsrepository.ReceiveAgentCommits(run.ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), bundle, base, a.trustedCommit(nil), a.triageAgentsOwnCommits(run.ctx), a.mayPushAgentCommits)
		outcome, reason := receivedAgentCommitsOutcome(commits, err)
		if err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
			return insertTriageAgentCommits(tx, run.id, requestID, base, commits, outcome, reason)
		}); err != nil {
			return refused("MMHotline could not record your commits, so it did not run " + playbook + ".")
		}
		answer.CommitsOutcome = outcome
		switch {
		case err != nil:
			// Git's message about origin may name the push credential, which
			// the agent must not read; the run's record keeps it.
			answer.Outcome, answer.Message = triageagent.Refused, "MMHotline could not push your commits, so it did not run "+playbook+". The reason is recorded with the run."
			return answer
		case commits.Outcome == systemsrepository.WaitsForAPerson:
			answer.Outcome, answer.Message = triageagent.WaitsForAPerson, "A person must approve your commits before MMHotline runs "+playbook+": "+pathsThatWaitText(commits)
			return answer
		case commits.Outcome == systemsrepository.Refused:
			answer.Outcome, answer.Message = triageagent.Refused, "MMHotline refused your commits, so it did not run "+playbook+": "+commits.Reason
			return answer
		}
	}
	if len(bundle) == 0 {
		// Without the agent's commits nothing above fetched, so MMHotline
		// would judge at its checkout from before, such as before a commit
		// that the previous playbook run pushed from its own clone, and the
		// run would then find newer commits and ask a person again. Bringing
		// the checkout forward to the trusted commit first judges the request
		// where it runs. A fetch that fails leaves the checkout as it was;
		// the run fetches again and reports the failure then.
		_, _ = systemsrepository.UpdateFromOrigin(run.ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), a.trustedCommit(nil))
	}
	decision := a.triageAgent.playbookRuns.DecidePlaybookRun(run.ctx, TriageAgentPlaybookRunRequest{RunID: run.id, RequestID: requestID, Playbook: request.Playbook, Inventory: request.Inventory})
	answer.Outcome, answer.Message = decision.Outcome, decision.Message
	switch {
	case decision.ApprovalRequestID != "":
		answer.Message += approvalAskedText(run.purpose == "triage_session")
	case !decision.RunsUnlessStoppedAt.IsZero():
		answer.Message += warningSentText(decision.RunsUnlessStoppedAt, a.Now(), run.purpose == "triage_session")
	}
	if answer.CommitsOutcome == string(systemsrepository.Pushed) {
		answer.Message = "MMHotline pushed your commits to origin. " + answer.Message
	}
	answer.PlaybookRunOutput = endOfPlaybookRunOutput(decision.PlaybookRunOutput)
	return answer
}

func pathsThatWaitText(commits systemsrepository.ReceivedAgentCommits) string {
	if len(commits.PathsThatWait) == 0 {
		return commits.Reason // such as a conflict with newer commits on origin
	}
	var lines []string
	for index, path := range commits.PathsThatWait {
		if index == maximumPathsThatWaitInAnswer {
			lines = append(lines, fmt.Sprintf("and %d more paths", len(commits.PathsThatWait)-index))
			break
		}
		lines = append(lines, path.Path+" "+path.Reason)
	}
	return strings.Join(lines, "; ") + "."
}

// endOfPlaybookRunOutput keeps the end of the output, where MMX prints its
// summary and the error that stopped the run.
func endOfPlaybookRunOutput(output string) string {
	if len(output) <= triageagent.MaximumPlaybookRunOutputForAgent {
		return output
	}
	cut := len(output) - triageagent.MaximumPlaybookRunOutputForAgent
	for cut < len(output) && !utf8.RuneStart(output[cut]) {
		cut++
	}
	return fmt.Sprintf("[MMHotline left out the first %d bytes of the output and keeps the whole output with the run.]\n", cut) + output[cut:]
}

// receivedAgentCommitsOutcome is the outcome recorded for one bundle of the
// agent's commits: failed when MMHotline could not finish.
func receivedAgentCommitsOutcome(commits systemsrepository.ReceivedAgentCommits, err error) (outcome, reason string) {
	if err != nil {
		return "failed", err.Error()
	}
	return string(commits.Outcome), commits.Reason
}

// insertTriageAgentCommits records one bundle of the agent's commits: the one
// a playbook request carried, or with requestID "" the one the run's final
// event carried.
func insertTriageAgentCommits(tx *sql.Tx, runID, requestID, base string, commits systemsrepository.ReceivedAgentCommits, outcome, reason string) error {
	_, err := tx.Exec(`INSERT INTO triage_agent_commits(run_id,request_id,base_commit_id,outcome,reason,agent_commit_id,pushed_commit_id,private_ref,working_tree_not_updated_because) VALUES(?,?,?,?,?,?,?,?,?)`, runID, requestID, base, outcome, reason, commits.AgentCommitID, commits.PushedCommitID, commits.PrivateRef, commits.WorkingTreeNotUpdatedBecause)
	return err
}
