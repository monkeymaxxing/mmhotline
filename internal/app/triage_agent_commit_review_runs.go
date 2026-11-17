package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// The triage agent's review of the commits no trusted key signed, in a run
// of its own, the commit review:
// docs/changing-mmhotline/design/triage-agent.md#the-triage-agent-reviews-a-commit-no-trusted-key-signed.
// The agent approves an ordinary change and
// reports at sos a commit that changes who can get in or looks like nobody
// on the team made it; a short list of paths only a person approves keeps
// the agent from widening its own powers or adding a key MMHotline trusts.

// The values of systems_repository_commits.commits_without_a_trusted_signature_are_approved_by.
const (
	commitsApprovedByAPerson                 = "a_person"
	commitsApprovedByAPersonOrTheTriageAgent = "a_person_or_the_triage_agent"
)

// commitApprovalSettings are the two settings of the triage agent's
// approval, as the settings file applied them.
type commitApprovalSettings struct {
	ApprovedBy                 string   `json:"commits_without_a_trusted_signature_are_approved_by"`
	PathsOnlyAPersonMayApprove []string `json:"paths_only_a_person_may_approve"`
}

// settingsCommitApprovalPart keeps the two settings as applied values, which
// readCommitApprovalSettings reads: only applied values count. Applying
// writes the database, and the background loop's visit that follows sends
// MMHotline's warning for each commit the agent may no longer review.
var settingsCommitApprovalPart = settingsFilePart{
	name: "systems_repository_commits.commits_without_a_trusted_signature_are_approved_by, systems_repository_commits.paths_only_a_person_may_approve",
	value: fileValue(func(s Settings) any {
		commits := s.SystemsRepositoryCommits
		if commits == nil {
			return nil
		}
		return commitApprovalSettings{ApprovedBy: commits.CommitsWithoutATrustedSignatureAreApprovedBy, PathsOnlyAPersonMayApprove: commits.PathsOnlyAPersonMayApprove}
	}),
	apply:       func(context.Context, *App, Settings) error { return nil },
	keepsValues: true,
}

// readCommitApprovalSettings reads the applied settings. Before the
// settings file applies them, and without the systems_repository_commits
// section, only a person approves.
func readCommitApprovalSettings(ctx context.Context, tx *sql.Tx) (commitApprovalSettings, error) {
	settings := commitApprovalSettings{ApprovedBy: commitsApprovedByAPerson}
	_, err := appliedSettingsValue(ctx, tx, settingsCommitApprovalPart.name, &settings)
	return settings, err
}

// checkCommitApprovalSettings checks the two settings as the settings check
// does, naming each problem's key path, and notes what they mean. The
// triage agent approves only when the settings file sets it up.
func checkCommitApprovalSettings(commits SettingsSystemsRepositoryCommits, triageAgentSectionPresent bool, problems *settingsProblems) {
	const approvedBy = "systems_repository_commits.commits_without_a_trusted_signature_are_approved_by"
	switch commits.CommitsWithoutATrustedSignatureAreApprovedBy {
	case commitsApprovedByAPerson:
		problems.note(approvedBy, "A person reviews every commit no trusted key signed before MMX runs past it.")
	case commitsApprovedByAPersonOrTheTriageAgent:
		if !triageAgentSectionPresent {
			problems.add(approvedBy, "a_person_or_the_triage_agent needs the triage_agent section: without it, use a_person")
			break
		}
		note := "The triage agent reviews each commit no trusted key signed and approves an ordinary change; a person reviews the triage agent's own commits outside its folders, and every commit that changes a path of systems_repository_commits.paths_only_a_person_may_approve."
		if len(commits.PathsOnlyAPersonMayApprove) == 0 {
			note += " The list is empty, so the triage agent may approve a commit that changes its own rules or the trusted keys: list the files that set them."
		}
		problems.note(approvedBy, note)
	default:
		problems.add(approvedBy, "must be a_person or a_person_or_the_triage_agent")
	}
	for index, path := range commits.PathsOnlyAPersonMayApprove {
		key := fmt.Sprintf("systems_repository_commits.paths_only_a_person_may_approve[%d]", index)
		if err := checkPathOnlyAPersonMayApprove(path); err != nil {
			problems.addError(key, err)
		} else if slices.Contains(commits.PathsOnlyAPersonMayApprove[:index], path) {
			problems.add(key, path+" is listed twice")
		}
	}
}

// checkPathOnlyAPersonMayApprove allows a path relative to the systems
// repository, as Git names it, such as AGENTS.md or roles/every_server/files/;
// an entry ending in "/" is a directory.
func checkPathOnlyAPersonMayApprove(path string) error {
	if path == "" || path == "/" {
		return errors.New("must be a path in the systems repository, such as AGENTS.md, or a directory ending in /, such as roles/every_server/files/")
	}
	if strings.ContainsFunc(path, unicode.IsControl) {
		return errors.New("must be one line without control characters")
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("%q must be relative to the systems repository, as Git names the path, such as roles/every_server/files/sudoers_triage", path)
	}
	for _, part := range strings.Split(strings.TrimSuffix(path, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf(`%q must not have an empty, ".", or ".." part, because Git never names a path so`, path)
		}
	}
	return nil
}

// pathsOnlyAPersonMayApprove are the changed paths that the list keeps for a
// person: an entry names the path itself, or with "/" at its end every path
// in that directory.
func pathsOnlyAPersonMayApprove(list, changed []string) []string {
	var kept []string
	for _, path := range changed {
		for _, entry := range list {
			if path == entry || (strings.HasSuffix(entry, "/") && strings.HasPrefix(path, entry)) {
				kept = append(kept, path)
				break
			}
		}
	}
	return kept
}

// whyTheTriageAgentMayNotApprove is the one rule of which commits the
// triage agent may approve: "" when it may, otherwise the reason it may not,
// in words that follow "the triage agent may not approve the commit: ". The
// agent never approves a commit of its own key outside its two folders, so
// it can never widen what it changes without a person.
func whyTheTriageAgentMayNotApprove(settings commitApprovalSettings, review CommitReview) string {
	switch {
	case settings.ApprovedBy != commitsApprovedByAPersonOrTheTriageAgent:
		return "the settings let only a person approve a commit no trusted key signed (systems_repository_commits.commits_without_a_trusted_signature_are_approved_by is a_person)"
	case !review.NotSignedByATrustedKey():
		return "the triage agent's own key signed the commit, and it changes paths outside one_time_playbooks/ and triage_agent_notes/; only a person reviews the triage agent's own commits"
	}
	if kept := pathsOnlyAPersonMayApprove(settings.PathsOnlyAPersonMayApprove, review.ChangedPaths); len(kept) > 0 {
		return "the commit changes paths only a person may approve (systems_repository_commits.paths_only_a_person_may_approve): " + strings.Join(kept, ", ")
	}
	return ""
}

// whyTheTriageAgentCannotReview is "" when a commit review run can start
// now or later, otherwise the reason it cannot.
func (a *App) whyTheTriageAgentCannotReview(ctx context.Context, tx *sql.Tx) (string, error) {
	var setUp bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup)`).Scan(&setUp); err != nil {
		return "", err
	}
	a.triageAgent.mu.Lock()
	installed := a.triageAgent.runSocketPath != "" && a.triageAgent.proxySocketPath != ""
	a.triageAgent.mu.Unlock()
	switch {
	case !setUp:
		return "the triage agent is not set up", nil
	case !installed:
		return "the triage agent's sandbox is not installed", nil
	case !a.systemsRepositoryCopyExists():
		return "MMHotline's copy of the systems repository is missing", nil
	}
	return "", nil
}

// commitsForTheTriageAgent goes through every waiting commit nobody has been
// told of yet. It sends MMHotline's warning, once, for each one the triage
// agent may not or cannot review, and returns the others, oldest found first,
// which a commit review run reviews. So each commit gets one alert: the
// warning, or the agent's approval or report.
func (a *App) commitsForTheTriageAgent(ctx context.Context, tx *sql.Tx) ([]CommitReview, error) {
	// With every_commit_on_origin no commit is checked, so no commit review
	// run starts and no warning goes out.
	if every, err := trustsEveryCommitOnOrigin(ctx, tx); err != nil || every {
		return nil, err
	}
	waiting, err := readCommitReviews(ctx, tx, `c.reviewed_at IS NULL AND c.alert_sent='' ORDER BY c.found_at,c.commit_id`)
	if err != nil || len(waiting) == 0 {
		return nil, err
	}
	settings, err := readCommitApprovalSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	cannot, err := a.whyTheTriageAgentCannotReview(ctx, tx)
	if err != nil {
		return nil, err
	}
	var forTheAgent []CommitReview
	for _, review := range waiting {
		reason := whyTheTriageAgentMayNotApprove(settings, review)
		if reason == "" && cannot == "" {
			forTheAgent = append(forTheAgent, review)
			continue
		}
		// With the setting a_person, the warning says what it always said.
		if settings.ApprovedBy != commitsApprovedByAPersonOrTheTriageAgent {
			reason = ""
		} else if reason == "" {
			reason = cannot
		}
		if err := a.warnAboutCommitTx(ctx, tx, review, reason); err != nil {
			return nil, err
		}
	}
	return forTheAgent, nil
}

// warnAboutCommitTx sends MMHotline's warning about a waiting commit, unless
// people already got an alert about it. notReviewedBecause, when set, says
// why the triage agent did not review the commit.
func (a *App) warnAboutCommitTx(ctx context.Context, tx *sql.Tx, review CommitReview, notReviewedBecause string) error {
	result, err := tx.ExecContext(ctx, `UPDATE triage_agent_refused_commits SET alert_sent='mmhotline_warning' WHERE commit_id=? AND alert_sent=''`, review.ID)
	if err != nil {
		return err
	}
	if sent, err := result.RowsAffected(); err != nil || sent == 0 {
		return err
	}
	return RecordSystemAlert(ctx, tx, "system", "", "warn", review.alertTitle(), review.alertMessage(a.WebURL, notReviewedBecause), a.Now())
}

var errNoCommitForTheTriageAgent = errors.New("no commit waits for the triage agent's review")

// DriveCommitReview is the background loop's turn for the commit review. It
// sends MMHotline's warning for each commit the agent may not or cannot
// review, and starts a commit review run when a commit waits that the agent
// may review and the one triage run is free. No coding agent starts without
// such a commit. The commit's record brings the visit: the check that found
// it, the end of a run, and an applied setting each change the database.
func (a *App) DriveCommitReview(ctx context.Context) error {
	var due bool
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		commits, err := a.commitsForTheTriageAgent(ctx, tx)
		due = len(commits) > 0
		return err
	})
	if err != nil || !due || !a.oneTriageRunIsFree() {
		return err
	}
	a.startBesideTheLoop(func() {
		if _, err := a.startCommitReview(ctx); err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, errNoCommitForTheTriageAgent) && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "mmhotline: could not start the commit review: %s\n", err)
		}
	})
	return nil
}

// startCommitReview starts one commit review run for every commit that
// waits for the triage agent, given to the run in the transaction that
// records it, so a run that fails at any later step leaves each one with
// MMHotline's warning (warnAboutCommitsTheRunLeft).
func (a *App) startCommitReview(ctx context.Context) (string, error) {
	return a.startTriageAgentRunInSystemsRepository(ctx, "commit_review", "MMHotline", triageagent.Request{Action: "start"}, func(tx *sql.Tx, runID string) error {
		commits, err := a.commitsForTheTriageAgent(ctx, tx)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			return errNoCommitForTheTriageAgent
		}
		for _, commit := range commits {
			if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_refused_commits SET triage_agent_review_run_id=? WHERE commit_id=?`, runID, commit.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// commitReviewStoppedForTriageSession is why a commit review ended when
// MMHotline stopped it because a triage session needs the one run; its
// commits go to the next commit review run.
const commitReviewStoppedForTriageSession = "MMHotline stopped the commit review because a triage session needed the agent; it starts again once the session's run ends."

// warnAboutCommitsTheRunLeft sends MMHotline's warning, once, for each
// commit given to the run that ended that the agent gave no decision on, so
// no commit waits unannounced. A run that MMHotline cut short leaves its
// commits to the next run.
func (a *App) warnAboutCommitsTheRunLeft(ctx context.Context, tx *sql.Tx, runID, reason string) error {
	left, err := readCommitReviews(ctx, tx, `c.triage_agent_review_run_id=? AND c.reviewed_at IS NULL AND c.alert_sent='' ORDER BY c.found_at,c.commit_id`, runID)
	if err != nil {
		return err
	}
	for _, review := range left {
		if err := a.warnAboutCommitTx(ctx, tx, review, reason); err != nil {
			return err
		}
	}
	return nil
}

// commitReviewRefusal is MMHotline's refusal of a review-commit decision,
// in words for the agent.
type commitReviewRefusal string

func (refusal commitReviewRefusal) Error() string { return string(refusal) }

// decideCommitReview answers the agent's review-commit request. approve
// marks the commit reviewed by the triage agent and sends the one info
// alert; needs_a_person sends the agent's report at its alert severity, with
// the commit's review link, as the one alert about the commit.
func (a *App) decideCommitReview(run *triageAgentActiveRun, request triageagent.AgentRequest) triageagent.Answer {
	refused := func(message string) triageagent.Answer {
		return triageagent.Answer{Outcome: triageagent.Refused, Message: message}
	}
	if run.purpose != "commit_review" {
		return refused("mmhotline triage-agent review-commit works only in a commit review run, which MMHotline starts when a commit waits for the triage agent's review.")
	}
	short := request.CommitID[:7]
	approve := request.CommitReviewDecision == triageagent.ApproveCommit
	refusal := func(reason string) error {
		if !approve {
			return commitReviewRefusal("MMHotline did not record your decision on commit " + short + ": " + reason + ".")
		}
		return commitReviewRefusal("MMHotline did not mark commit " + short + " reviewed: " + reason + ". Decide needs_a_person and report instead: mmhotline triage-agent review-commit --commit-id " + request.CommitID + " --decision needs_a_person --alert-severity warn --report-file report.md, with --alert-severity sos when the commit changes who can get in or looks like nobody on the team made it.")
	}
	ctx := context.WithoutCancel(run.ctx)
	var review CommitReview
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		reviews, err := readCommitReviews(ctx, tx, `c.commit_id=?`, request.CommitID)
		if err != nil {
			return err
		}
		if len(reviews) == 0 || reviews[0].Reviewed() {
			return refusal("commit " + short + " does not wait for review")
		}
		review = reviews[0]
		if approve {
			settings, err := readCommitApprovalSettings(ctx, tx)
			if err != nil {
				return err
			}
			if reason := whyTheTriageAgentMayNotApprove(settings, review); reason != "" {
				return refusal(reason)
			}
		}
		if review.TriageAgentReviewRunID != run.id || review.AlertSent != "" {
			return refusal("commit " + short + " is not one of the commits this review run was started for")
		}
		link := strings.TrimRight(a.WebURL, "/") + review.URL()
		if !approve {
			if err := deliverTriageAgentReportTx(ctx, tx, run.id, request.AlertSeverity, request.ReportText+"\n\nReview commit "+short+" at "+link, a.Now().UTC()); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE triage_agent_refused_commits SET alert_sent='triage_agent_report' WHERE commit_id=?`, review.ID)
			return err
		}
		if err := a.markCommitReviewedTx(ctx, tx, commitReviewer{triageAgentRunID: run.id}, review); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_refused_commits SET alert_sent='triage_agent_approval' WHERE commit_id=?`, review.ID); err != nil {
			return err
		}
		title := "The triage agent marked commit " + short + " reviewed"
		if line := reportTitleLine(request.ReportText); line != "" {
			title += ": " + line
		}
		return RecordSystemAlert(ctx, tx, "triage_agent", "", "info", title, triageAgentReportMessage(request.ReportText+"\n\nThe commit: "+link), a.Now())
	})
	if err != nil {
		var refusedBecause commitReviewRefusal
		if errors.As(err, &refusedBecause) {
			return refused(string(refusedBecause))
		}
		return refused("MMHotline could not record your decision on commit " + short + ". Give your decision again.")
	}
	if !approve {
		return triageagent.Answer{Outcome: triageagent.ReportRecorded, Message: "MMHotline recorded your decision: commit " + short + " waits for a person's review, and your report went to the team at alert severity " + request.AlertSeverity + "."}
	}
	// The checkpoint and MMHotline's checkout move through the one function
	// that moves them; a failure leaves them for the next run or check.
	message := "MMHotline marked commit " + short + " reviewed by the triage agent and sent your report to the team at info. MMX runs may now pass the commit."
	if _, err := systemsrepository.UpdateFromOrigin(ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), a.trustedCommit(nil)); err != nil {
		message += " MMHotline moves its copy past the commit at its next check."
	}
	return triageagent.Answer{Outcome: triageagent.ReportRecorded, Message: message}
}
