package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"mmhotline/internal/systemsrepository"
)

// CommitReviewsCommandRequest is the input of `mmhotline commit-reviews`.
type CommitReviewsCommandRequest struct {
	CommitID string `json:"commit_id,omitempty"`
}

// CommandCommitReview is a commit that waits or waited for a person's
// review, as its page shows it.
type CommandCommitReview struct {
	CommitID string `json:"commit_id"`
	// ReasonKind is not_signed_by_a_trusted_key or
	// triage_agent_change_needs_review; Reason says it in words.
	ReasonKind          string                            `json:"reason_kind"`
	Reason              string                            `json:"reason"`
	Author              string                            `json:"author"`
	CommittedAt         time.Time                         `json:"committed_at"`
	Message             string                            `json:"message"`
	PathsThatNeedReview []systemsrepository.PathThatWaits `json:"paths_that_need_review"`
	FoundAt             time.Time                         `json:"found_at"`
	// ChangedPaths are every path the commit changes, against each parent of
	// a merge.
	ChangedPaths []string  `json:"changed_paths"`
	ReviewedBy   string    `json:"reviewed_by,omitempty"`
	ReviewedAt   time.Time `json:"reviewed_at,omitzero"`
	// ReviewerKind is member, unix_user, trust_through, or triage_agent once
	// reviewed.
	ReviewerKind string `json:"reviewer_kind,omitempty"`
	// TriageAgentReview is the triage agent's latest decision on the commit,
	// with its report.
	TriageAgentReview *TriageAgentCommitReview `json:"triage_agent_review,omitempty"`
	ReviewURL         string                   `json:"review_url"`
	// Diff is in show only; a list of several diffs is too long to read.
	Diff          string `json:"diff,omitempty"`
	DiffTruncated bool   `json:"diff_truncated,omitempty"`
}

// CommandCommitCheck is what commit-reviews check answers.
type CommandCommitCheck struct {
	CheckedCommitID         string                `json:"checked_commit_id"`
	TrustedThroughCommitID  string                `json:"trusted_through_commit_id"`
	CommitsWaitingForReview []CommandCommitReview `json:"commits_waiting_for_review"`
}

func (a *App) commitReviewsCommand(ctx context.Context, p Principal, action string, request CommitReviewsCommandRequest, result *CommandResult) error {
	if action != "list" && request.CommitID == "" {
		return fmt.Errorf("%w: --commit-id is required", ErrInvalid)
	}
	approvals := strings.TrimRight(a.WebURL, "/") + "/approvals"
	switch action {
	case "check", "trust-through":
		var check CommitCheck
		var err error
		if action == "check" {
			check, err = a.checkCommitsForCommand(ctx, request.CommitID)
		} else {
			check, result.Message, err = a.trustCommitsThrough(ctx, p, request.CommitID)
		}
		if err != nil {
			return err
		}
		out := CommandCommitCheck{CheckedCommitID: check.CheckedCommitID, TrustedThroughCommitID: check.TrustedThroughCommitID, CommitsWaitingForReview: []CommandCommitReview{}}
		for _, review := range check.Waiting {
			out.CommitsWaitingForReview = append(out.CommitsWaitingForReview, a.commandCommitReview(review, false))
		}
		result.CommitCheck = &out
		if len(check.Waiting) > 0 {
			result.Message = strings.TrimSpace(result.Message + " " + fmt.Sprintf("%s for a person's review at %s.", countText(len(check.Waiting), "commit waits", "commits wait"), approvals))
		}
		return nil
	case "list":
		reviews, err := a.CommitReviews(ctx, p)
		if err != nil {
			return err
		}
		result.CommitReviews = []CommandCommitReview{}
		for _, review := range reviews {
			result.CommitReviews = append(result.CommitReviews, a.commandCommitReview(review, false))
		}
		approved, err := a.CommitsTheTriageAgentApproved(ctx, p)
		if err != nil {
			return err
		}
		result.CommitsTheTriageAgentApproved = []CommandCommitReview{}
		for _, review := range approved {
			result.CommitsTheTriageAgentApproved = append(result.CommitsTheTriageAgentApproved, a.commandCommitReview(review, false))
		}
		result.Message = fmt.Sprintf("Listed %s waiting for review, oldest found first, then %s the triage agent approved, newest first. Team members review commits at %s.", countText(len(reviews), "commit", "commits"), countText(len(approved), "commit", "commits"), approvals)
		return nil
	case "show", "mark-reviewed":
		if action == "mark-reviewed" {
			if err := a.ReviewCommit(ctx, p, request.CommitID); err != nil {
				return err
			}
		}
		review, err := a.CommitReview(ctx, p, request.CommitID)
		if err != nil {
			return err
		}
		out := a.commandCommitReview(review, true)
		result.CommitReview = &out
		return nil
	}
	return ErrNotFound
}

func (a *App) commandCommitReview(review CommitReview, withDiff bool) CommandCommitReview {
	out := CommandCommitReview{CommitID: review.ID, ReasonKind: review.ReasonKind, Reason: review.Reason(), Author: review.Author, CommittedAt: review.CommittedAt, Message: review.Message,
		PathsThatNeedReview: append([]systemsrepository.PathThatWaits{}, review.Paths...), ChangedPaths: append([]string{}, review.ChangedPaths...), FoundAt: review.FoundAt, ReviewedBy: review.ReviewedBy, ReviewedAt: review.ReviewedAt, ReviewerKind: review.ReviewerKind,
		TriageAgentReview: review.TriageAgentReview, ReviewURL: strings.TrimRight(a.WebURL, "/") + review.URL()}
	if withDiff {
		out.Diff, out.DiffTruncated = review.Diff, review.DiffTruncated
	}
	return out
}

// checkCommitsForCommand fetches origin into MMHotline's copy, under the
// copy's lock as runs do, and checks every commit through commitID, which
// the copy must then hold. New commits that wait are recorded and alerted
// as before an MMX run.
func (a *App) checkCommitsForCommand(ctx context.Context, commitID string) (CommitCheck, error) {
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	if _, err := systemsrepository.UpdateFromOrigin(ctx, directory, a.trustedCommit(nil)); err != nil {
		return CommitCheck{}, fmt.Errorf("MMHotline could not fetch origin into its copy of the systems repository: %w", err)
	}
	found, err := systemsrepository.HasCommit(ctx, directory, commitID)
	if err != nil {
		return CommitCheck{}, fmt.Errorf("%w: --commit-id %s", ErrInvalid, err)
	}
	if !found {
		return CommitCheck{}, fmt.Errorf("%w: MMHotline's copy of the systems repository has no commit %s, even after fetching origin", ErrNotFound, commitID)
	}
	check, err := a.checkCommitsThrough(ctx, commitID)
	if err != nil {
		return check, fmt.Errorf("MMHotline could not check the systems repository commits: %w", err)
	}
	return check, nil
}

// commitReviewText is one commit review's lines in the text output.
func commitReviewText(review CommandCommitReview) []string {
	lines := []string{fmt.Sprintf("Commit %s, %s.", review.CommitID, review.Reason),
		fmt.Sprintf("Author: %s. Commit date: %s. Found by MMHotline: %s.", review.Author, commandTime(review.CommittedAt), commandTime(review.FoundAt)),
		"Message: " + review.Message}
	for _, path := range review.PathsThatNeedReview {
		lines = append(lines, "Needs review: "+path.Path+" "+path.Reason+".")
	}
	if review.ReviewedBy != "" {
		lines = append(lines, fmt.Sprintf("Reviewed by %s at %s.", review.ReviewedBy, commandTime(review.ReviewedAt)))
	} else {
		lines = append(lines, "Waits for review at "+review.ReviewURL+".")
	}
	if agent := review.TriageAgentReview; agent != nil {
		decision := "approved the commit"
		if agent.NeedsAPerson() {
			decision = "decided that a person must review the commit, with its report at alert severity " + agent.AlertSeverity
		}
		lines = append(lines, fmt.Sprintf("The triage agent %s at %s, in commit review run %s. Its report:", decision, commandTime(agent.DecidedAt), agent.RunID), strings.TrimSuffix(agent.Report, "\n"))
	}
	if review.Diff != "" {
		lines = append(lines, "Diff:", strings.TrimSuffix(review.Diff, "\n"))
		if review.DiffTruncated {
			lines = append(lines, "[MMHotline keeps only the first 128 KiB of the diff. Read the full diff in your systems repository.]")
		}
	}
	return lines
}

func (c CommandCommitCheck) text() []string {
	trusted := "No commit is trusted yet."
	if c.TrustedThroughCommitID != "" {
		trusted = "Trusted through commit " + c.TrustedThroughCommitID + ": every commit through it passed the check or a person reviewed it."
	}
	lines := []string{"Checked commit " + c.CheckedCommitID + ".", trusted}
	for _, review := range c.CommitsWaitingForReview {
		lines = append(lines, commitReviewText(review)...)
	}
	return lines
}
