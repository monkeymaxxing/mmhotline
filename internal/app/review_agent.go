package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"mmhotline/internal/triageagent"
)

// The review agent is a second coding agent run, in a fresh home that sees
// only the playbook and the diagnosis, never the logs, that must approve each
// playbook the triage agent asks to run before the rules for running the agent's playbooks apply. Any
// coding agent may review, the triage agent's own too; another vendor's is
// recommended: docs/changing-mmhotline/design/triage-agent.md#the-review-agent.

// reviewAgentSetupTable is the review agent's setup, with the same columns
// as triage_agent_setup.
const reviewAgentSetupTable = `
CREATE TABLE review_agent_setup (
 id INTEGER PRIMARY KEY CHECK(id=1), coding_agent TEXT NOT NULL,
 credential_type TEXT NOT NULL, credential TEXT NOT NULL,
 model TEXT NOT NULL, reasoning_effort TEXT NOT NULL,
 set_up_at INTEGER NOT NULL, set_up_by TEXT NOT NULL,
 ` + credentialColumns + `,
 ` + credentialCheckColumns + `
)`

// playbook_run_reviews holds one row for each review of a playbook run
// request: the triage run's request, the review agent that reviewed it and
// its run, what it answered, and the playbook's text it saw, kept as an
// approval request keeps it. A request MMHotline judged again at a newer
// commit has one review for each commit. The review agent's refusals in a
// row on one problem are read from these rows (reviewAgentRefusalsInARow).
const reviewAgentSchema = reviewAgentSetupTable + `;
CREATE TABLE playbook_run_reviews (
 id INTEGER PRIMARY KEY,
 run_id TEXT NOT NULL, request_id TEXT NOT NULL,
 review_run_id TEXT NOT NULL DEFAULT '',
 coding_agent TEXT NOT NULL, credential_type TEXT NOT NULL, commit_id TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('approved','refused','could_not_review')),
 reason TEXT NOT NULL, started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL,
 playbook_text TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(run_id, request_id) REFERENCES triage_agent_requests(run_id, request_id)
);
`

const reviewAgentTestSentence = "MMHotline review agent test passed"
const reviewAgentSetupTestPrompt = "This is MMHotline's setup test for its review agent. Reply with exactly this sentence and nothing else: MMHotline review agent test passed."

// The outcomes of a review.
const (
	reviewApproved       = "approved"
	reviewRefused        = "refused"
	reviewCouldNotReview = "could_not_review"
)

// maximumReviewReason bounds the review agent's reason in the answer to the
// triage agent, as a step's text is bounded; the review run keeps the whole
// answer.
const maximumReviewReason = 2000

// ReviewAgentView is the review agent's setup, its latest setup test, and
// its latest sign-in check.
type ReviewAgentView struct {
	SandboxInstalled  bool              `json:"sandbox_installed"`
	Setup             *TriageAgentSetup `json:"setup,omitempty"`
	LatestSetupTest   *TriageAgentRun   `json:"latest_setup_test,omitempty"`
	LatestSignInCheck *TriageAgentRun   `json:"latest_sign_in_check,omitempty"`
}

func (a *App) ReviewAgentState(ctx context.Context, p Principal) (ReviewAgentView, error) {
	a.triageAgent.mu.Lock()
	view := ReviewAgentView{SandboxInstalled: a.triageAgent.runSocketPath != "" && a.triageAgent.proxySocketPath != ""}
	a.triageAgent.mu.Unlock()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var err error
		if view.Setup, err = codingAgentSetup(ctx, tx, reviewAgentRole); err != nil {
			return err
		}
		if view.LatestSetupTest, err = latestTestRun(ctx, tx, "review_agent_setup_test"); err != nil {
			return err
		}
		view.LatestSignInCheck, err = latestTestRun(ctx, tx, "review_agent_sign_in_check")
		return err
	})
	return view, err
}

// savedReviewAgent is the review agent's setup, with its credential, model,
// and reasoning effort, as a run request; set is false when no review agent
// is set up.
func (a *App) savedReviewAgent(ctx context.Context) (request triageagent.Request, set bool, err error) {
	request, err = a.savedAgentRequest(ctx, reviewAgentRole, "", "")
	if errors.Is(err, ErrInvalid) {
		return request, false, nil
	}
	return request, err == nil, err
}

// playbookReview is the review agent's review of one playbook run request.
type playbookReview struct {
	outcome     string // approved, refused, or could_not_review
	reason      string
	reviewRunID string // empty when the review run could not start
	agent       triageagent.Request
	startedAt   time.Time
	endedAt     time.Time
}

// reviewPlaybook runs the review agent on prompt, while the triage run that
// asked waits, and returns its review. A review run that cannot start,
// fails, or times out could not review; an answer whose first line is
// neither APPROVE nor REFUSE counts as a refusal. Either way a person decides.
func (a *App) reviewPlaybook(ctx context.Context, agent triageagent.Request, prompt string) playbookReview {
	review := playbookReview{agent: agent, startedAt: a.Now().UTC()}
	couldNotReview := func(reason string) playbookReview {
		review.outcome, review.reason, review.endedAt = reviewCouldNotReview, reason, a.Now().UTC()
		return review
	}
	if ctx.Err() != nil {
		return couldNotReview("The triage agent's run ended before the review started.")
	}
	request := agent
	request.Action, request.Prompt = "start", prompt
	run, err := a.startTriageAgentRun(ctx, "playbook_review", "MMHotline", request, nil, func(*sql.Tx, string) error { return nil })
	if err != nil {
		return couldNotReview("The review agent's run could not start: " + publicError(err))
	}
	review.reviewRunID = run.id
	// The review ends with the triage run that waits for it, such as when
	// MMHotline stops.
	stop := context.AfterFunc(ctx, func() {
		a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The triage agent's run ended during the review."})
	})
	<-run.done
	stop()
	if run.result.Event != "finished" {
		return couldNotReview(run.result.Reason)
	}
	review.outcome, review.reason = readReviewAnswer(run.result.Answer)
	review.endedAt = a.Now().UTC()
	return review
}

// readReviewAnswer reads the review agent's answer: a first line of exactly
// APPROVE or REFUSE, then the reason. Anything else is a refusal, so a broken
// answer never lets a playbook run.
func readReviewAnswer(answer string) (outcome, reason string) {
	first, rest, _ := strings.Cut(strings.TrimSpace(answer), "\n")
	reason = strings.TrimSpace(rest)
	if reason == "" {
		reason = "The review agent gave no reason."
	}
	switch strings.TrimSpace(first) {
	case "APPROVE":
		return reviewApproved, cutReviewReason(reason)
	case "REFUSE":
		return reviewRefused, cutReviewReason(reason)
	}
	return reviewRefused, cutReviewReason("The review agent's answer did not start with a line APPROVE or REFUSE, so MMHotline treats the answer as a refusal. The review agent answered: " + strings.TrimSpace(answer))
}

func cutReviewReason(reason string) string {
	runes := []rune(reason)
	if len(runes) > maximumReviewReason {
		return string(runes[:maximumReviewReason-1]) + "…"
	}
	return reason
}

// sentenceReason is the reason ending as a sentence ends, so a text can go on
// after it.
func (review playbookReview) sentenceReason() string {
	reason := review.reason
	if !strings.HasSuffix(reason, ".") && !strings.HasSuffix(reason, "!") && !strings.HasSuffix(reason, "?") && !strings.HasSuffix(reason, "…") {
		reason += "."
	}
	return reason
}

// text says what the review answered, for the triage agent's answer.
func (review playbookReview) text() string {
	reason := review.sentenceReason()
	switch review.outcome {
	case reviewApproved:
		return "The review agent approved the playbook: " + reason
	case reviewRefused:
		return "The review agent refused the playbook: " + reason
	}
	return "The review agent could not review the playbook: " + reason
}

// insertPlaybookRunReview records a review with the playbook's text it saw.
func insertPlaybookRunReview(ctx context.Context, tx *sql.Tx, request TriageAgentPlaybookRunRequest, commitID, playbookText string, review playbookReview) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO playbook_run_reviews(run_id,request_id,review_run_id,coding_agent,credential_type,commit_id,outcome,reason,started_at,ended_at,playbook_text) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		request.RunID, request.RequestID, review.reviewRunID, review.agent.CodingAgent, review.agent.CredentialType, commitID, review.outcome, review.reason, review.startedAt.Unix(), review.endedAt.Unix(), keptPlaybookText(playbookText))
	return err
}

// maximumReviewAgentRefusalsInARow is how many refusals in a row the review
// agent gives on one problem before a person decides; until then each
// refusal goes back to the triage agent, which may revise the playbook and
// ask again. It is fixed, not a setting: each round is a paid run of both
// agents while the outage goes on, and more than three refusals usually
// means the two agents see the problem differently, which is a person's call.
const maximumReviewAgentRefusalsInARow = 3

// reviewProblem is the problem a playbook run request is about, over which
// the review agent's refusals in a row count: the request's triage session,
// or for a software review, whose run has no session, that run.
type reviewProblem struct{ triageSessionID, runID string }

// readReviewProblem reads the problem of the run runID.
func readReviewProblem(ctx context.Context, tx *sql.Tx, runID string) (reviewProblem, error) {
	problem := reviewProblem{runID: runID}
	err := tx.QueryRowContext(ctx, `SELECT triage_session_id FROM triage_agent_runs WHERE id=?`, runID).Scan(&problem.triageSessionID)
	return problem, err
}

// in names the problem in an answer or the incident's history.
func (problem reviewProblem) in() string {
	if problem.triageSessionID == "" {
		return "during the software review"
	}
	return "during the triage session"
}

// refusedVersion is one version of a fix that the review agent refused: the
// request's playbook, inventory, and diagnosis, and the review's commit,
// reason, and the playbook's text it saw.
type refusedVersion struct {
	reviewID                                                       int64
	Playbook, Inventory, CommitID, Diagnosis, Reason, PlaybookText string
}

// reviewAgentRefusalsInARow lists, oldest first, the review agent's refusals
// on problem since its latest approval there, among the reviews recorded
// before the review beforeReviewID. A review that could not finish neither
// counts nor ends the row: it is no judgment of the fix.
func reviewAgentRefusalsInARow(ctx context.Context, tx *sql.Tx, problem reviewProblem, beforeReviewID int64) ([]refusedVersion, error) {
	rows, err := tx.QueryContext(ctx, `WITH reviews AS (
 SELECT v.id,v.run_id,v.request_id,v.commit_id,v.outcome,v.reason,v.playbook_text FROM playbook_run_reviews v JOIN triage_agent_runs r ON r.id=v.run_id
 WHERE v.id<?3 AND CASE WHEN ?1!='' THEN r.triage_session_id=?1 ELSE v.run_id=?2 END)
SELECT v.id,q.playbook,q.inventory,v.commit_id,q.diagnosis,v.reason,v.playbook_text FROM reviews v JOIN triage_agent_requests q ON q.run_id=v.run_id AND q.request_id=v.request_id
WHERE v.outcome=?4 AND v.id>coalesce((SELECT max(id) FROM reviews WHERE outcome=?5),0) ORDER BY v.id`, problem.triageSessionID, problem.runID, beforeReviewID, reviewRefused, reviewApproved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versions []refusedVersion
	for rows.Next() {
		var version refusedVersion
		if err = rows.Scan(&version.reviewID, &version.Playbook, &version.Inventory, &version.CommitID, &version.Diagnosis, &version.Reason, &version.PlaybookText); err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

// latestReviewID stands for no bound in reviewAgentRefusalsInARow: every
// review recorded so far.
const latestReviewID = math.MaxInt64

// sentBackToTheTriageAgentText ends the answer to a refusal that goes back
// to the triage agent, the refusalsInARow-th in a row on problem.
func sentBackToTheTriageAgentText(problem reviewProblem, refusalsInARow int) string {
	return fmt.Sprintf(" This is the review agent's refusal %d of %d in a row %s, so nothing ran and no person was asked. Revise the playbook to answer the reason, commit the new version, and ask again with a new mmhotline triage-agent request-playbook-run. The review agent reviews each new version. After a third refusal in a row, the request waits for a person, who sees every version and every reason.",
		refusalsInARow, maximumReviewAgentRefusalsInARow, problem.in())
}

// aPersonDecidesAfterRefusalsText says why a refused playbook waits for a
// person: the review agent refused it refusalsInARow times in a row.
func aPersonDecidesAfterRefusalsText(problem reviewProblem, refusalsInARow int) string {
	return fmt.Sprintf("The review agent has refused %d times in a row %s, so a person decides.", refusalsInARow, problem.in())
}

// recordReviewAgentRefusal records a refusal that goes back to the triage
// agent in the history of each unresolved incident of the run's work, so the
// people on the incident see each version and reason as it comes. A
// software review's run has no incident, so nothing is recorded.
func recordReviewAgentRefusal(ctx context.Context, tx *sql.Tx, request TriageAgentPlaybookRunRequest, problem reviewProblem, commitID string, refusalsInARow int, review playbookReview, now time.Time) error {
	incidents, err := triageRunIncidents(ctx, tx, problem.triageSessionID, request.RunID)
	if err != nil {
		return err
	}
	message := fmt.Sprintf("The review agent refused %s, at commit %s, so nothing ran: refusal %d of %d in a row %s. The review agent's reason: %s The triage agent may revise the playbook and ask again. After a third refusal in a row, a person decides.",
		playbookInInventoryText(request.Playbook, request.Inventory), commitID, refusalsInARow, maximumReviewAgentRefusalsInARow, problem.in(), review.sentenceReason())
	for _, incident := range incidents {
		if incident.Status == "resolved" {
			continue
		}
		if err = recordIncidentAction(ctx, tx, incident, playbookRunApprovalsActor, "playbook_run_refused_by_the_review_agent", message, "", "", now); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) reviewAgentCommand(ctx context.Context, p Principal, action string, result *CommandResult) error {
	var err error
	switch action {
	case "show":
	case "check-sign-in":
		result.ReviewAgentRunID, err = a.CheckSignInNow(ctx, p, reviewAgentRole)
	default:
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	view, err := a.ReviewAgentState(ctx, p)
	if err != nil {
		return err
	}
	result.ReviewAgent = &view
	return nil
}

func (view ReviewAgentView) Text() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Triage agent sandbox installed: %t.", view.SandboxInstalled))
	if !view.SandboxInstalled {
		lines = append(lines, TriageAgentSandboxNotInstalled)
	}
	if view.Setup == nil {
		lines = append(lines, "Review agent: not set up")
	} else {
		lines = append(lines, fmt.Sprintf("Review agent: %s (coding_agent %s, credential_type %s, model %s, reasoning_effort %s), set up by %s at %s.", view.Setup.Title(), view.Setup.CodingAgent, view.Setup.CredentialType, view.Setup.Model, view.Setup.ReasoningEffort, view.Setup.SetUpBy, commandTime(view.Setup.SetUpAt)))
		lines = append(lines, view.Setup.credentialLines()...)
	}
	if run := view.LatestSetupTest; run != nil {
		lines = append(lines, setupTestText(*run)...)
	}
	if run := view.LatestSignInCheck; run != nil {
		lines = append(lines, signInCheckText(*run)...)
	}
	return strings.Join(lines, "\n")
}
