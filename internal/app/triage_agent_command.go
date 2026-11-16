package app

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
)

func (a *App) triageAgentCommand(ctx context.Context, p Principal, action string, result *CommandResult) error {
	var err error
	switch action {
	case "show":
	case "run-software-review":
		result.TriageAgentRunID, err = a.RunSoftwareReviewNow(ctx, p)
	case "check-sign-in":
		result.TriageAgentRunID, err = a.CheckSignInNow(ctx, p, triageAgentRole)
	default:
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	view, err := a.TriageAgentState(ctx, p)
	if err != nil {
		return err
	}
	result.TriageAgent = &view
	return nil
}

// setupTestText is a setup test of the triage agent or the review agent, as
// the commands print it.
func setupTestText(run TriageAgentRun) []string {
	lines := []string{fmt.Sprintf("Setup test %s: %s; run state: %s; purpose: %s; coding agent: %s; credential type: %s.", run.ID, run.Status(), run.State, run.Purpose, run.CodingAgent, run.CredentialType), "Coding agent session ID: " + run.CodingAgentSessionID, fmt.Sprintf("Setup test started by %s at %s.", run.StartedBy, commandTime(run.StartedAt))}
	if run.EndedAt != nil {
		lines = append(lines, "Setup test ended at: "+commandTime(*run.EndedAt))
	}
	if run.FailureReason != "" {
		lines = append(lines, "Setup test failure reason: "+run.FailureReason)
	}
	for _, step := range run.Steps {
		lines = append(lines, fmt.Sprintf("Step %d at %s (%s): %s", step.Sequence, commandTime(step.OccurredAt), step.Kind, step.Text))
	}
	lines = append(lines, "Answer: "+run.Answer)
	if run.CostInUSDollars != nil {
		lines = append(lines, fmt.Sprintf("Cost in US dollars: %.6f", *run.CostInUSDollars))
	}
	return append(lines, tokensText(run.InputTokens, run.CachedInputTokens, run.OutputTokens))
}

// signInCheckText is the latest sign-in check of the triage agent or the
// review agent, as the commands print it. Its steps and answer stay with
// the run: the check asks for one sentence.
func signInCheckText(run TriageAgentRun) []string {
	lines := []string{fmt.Sprintf("Latest sign-in check %s: %s; run state: %s; started by %s at %s.", run.ID, run.Status(), run.State, run.StartedBy, commandTime(run.StartedAt))}
	if run.EndedAt != nil {
		lines = append(lines, "Sign-in check ended at: "+commandTime(*run.EndedAt))
	}
	if run.FailureReason != "" {
		lines = append(lines, "Sign-in check failure reason: "+run.FailureReason)
	}
	return lines
}

func (view TriageAgentView) Text() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Triage agent sandbox installed: %t.", view.SandboxInstalled))
	if !view.SandboxInstalled {
		lines = append(lines, TriageAgentSandboxNotInstalled)
	}
	if view.Setup == nil {
		lines = append(lines, "Triage agent: not set up")
	} else {
		lines = append(lines, fmt.Sprintf("Triage agent: %s (coding_agent %s, credential_type %s, model %s, reasoning_effort %s), set up by %s at %s.", view.Setup.Title(), view.Setup.CodingAgent, view.Setup.CredentialType, view.Setup.Model, view.Setup.ReasoningEffort, view.Setup.SetUpBy, commandTime(view.Setup.SetUpAt)))
		lines = append(lines, view.Setup.credentialLines()...)
	}
	lines = append(lines, "Internet access: "+view.InternetAccess)
	for _, host := range view.ListedHosts {
		lines = append(lines, fmt.Sprintf("Listed host: %s; listed because: %s.", net.JoinHostPort(host.Host, strconv.Itoa(host.Port)), host.ListedBecause))
		if host.AddedBy != "" {
			lines = append(lines, "Listed host added by: "+host.AddedBy)
		}
	}
	if view.HostsFromTheSystemsRepositoryListedAt != nil {
		lines = append(lines, "Inventory servers and vendor pages' hosts listed at the start of the latest run: "+displayTime(*view.HostsFromTheSystemsRepositoryListedAt)+". A server or a page removed since stays listed until the next run.")
	}
	if view.InventoryServersNotListedBecause != "" {
		lines = append(lines, "At the start of the latest run, MMHotline could not list every inventory server: "+view.InventoryServersNotListedBecause)
	}
	if view.VendorPageHostsNotListedBecause != "" {
		lines = append(lines, "At the start of the latest run, MMHotline could not list the host of every vendor page in "+vendorPagesFile+": "+view.VendorPageHostsNotListedBecause)
	}
	if run := view.LatestSetupTest; run != nil {
		lines = append(lines, setupTestText(*run)...)
	}
	if run := view.LatestSignInCheck; run != nil {
		lines = append(lines, signInCheckText(*run)...)
	}
	scheduled := "Scheduled software review: " + view.SoftwareReviewIntervalHours.Text()
	if view.SoftwareReviewIntervalChangedAt != nil {
		scheduled += fmt.Sprintf("; set by %s at %s", view.SoftwareReviewIntervalChangedBy, commandTime(*view.SoftwareReviewIntervalChangedAt))
	}
	lines = append(lines, scheduled+".")
	switch {
	case view.ScheduledSoftwareReviewIsOff():
		lines = append(lines, "A software review runs only when an administrator runs sudo mmhotline triage-agent run-software-review.")
	case view.NextSoftwareReviewDueAt == nil:
		lines = append(lines, "Next scheduled software review due: at once.")
	default:
		lines = append(lines, "Next scheduled software review due at: "+commandTime(*view.NextSoftwareReviewDueAt))
	}
	if review := view.LatestSoftwareReview; review != nil {
		lines = append(lines, fmt.Sprintf("Latest software review %s: %s; started by %s at %s.", review.RunID, review.State, review.StartedBy, commandTime(review.StartedAt)))
		if review.EndedAt != nil {
			lines = append(lines, "Software review ended at: "+commandTime(*review.EndedAt))
		}
		if review.FailureReason != "" {
			lines = append(lines, "Software review failure reason: "+review.FailureReason)
		}
		counts := review.ReportCountsByAlertSeverity
		lines = append(lines, fmt.Sprintf("Software review reports: %d at sos, %d at warn, %d at info.", counts.SOS, counts.Warn, counts.Info), "Software review usage: "+review.UsageText())
	}
	if review := view.LatestCommitReview; review != nil {
		lines = append(lines, fmt.Sprintf("Latest commit review %s: %s; started by %s at %s.", review.RunID, review.State, review.StartedBy, commandTime(review.StartedAt)))
		if review.FailureReason != "" {
			lines = append(lines, "Commit review failure reason: "+review.FailureReason)
		}
		lines = append(lines, "Commit review usage: "+review.UsageText())
	}
	for _, connection := range view.RecentConnections {
		target := net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port))
		if connection.Address != "" {
			target += " at address " + connection.Address
		}
		line := fmt.Sprintf("Connection %d for run %s at %s to %s: ", connection.ID, connection.RunID, commandTime(connection.StartedAt), target)
		if connection.Decision == "allowed" {
			line += fmt.Sprintf("allowed; %d bytes sent, %d bytes received", connection.BytesSent, connection.BytesReceived)
			if connection.EndedAt != nil {
				line += "; ended at " + commandTime(*connection.EndedAt)
			}
			line += "."
		} else {
			line += "refused: " + connection.RefusalReason
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
