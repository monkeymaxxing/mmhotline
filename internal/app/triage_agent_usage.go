package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The triage agent's weekly numbers: what its runs did and used, how often
// the run limit was reached, and what happened to the warnings held for
// it. `mmhotline triage-agent usage --since TIME` prints them for people
// and the desk, and the software review's prompt carries the last 7 days'
// (docs/changing-mmhotline/design/triage-agent.md#the-run-limits-numbers).
// They are read from the records themselves: the runs, MMHotline's own
// warnings that the limit was reached, the passed-over marks on alerts,
// and the holds.

// TriageAgentCommandRequest is the input of the triage-agent group's
// actions that take one.
type TriageAgentCommandRequest struct {
	// Since is usage's start: the numbers cover what started or arrived at
	// or after it.
	Since time.Time `json:"since,omitzero"`
}

// TriageAgentUsage is the numbers from Since to Until.
type TriageAgentUsage struct {
	Since             time.Time         `json:"since"`
	Until             time.Time         `json:"until"`
	MostRunsIn24Hours MostRunsIn24Hours `json:"most_runs_in_24_hours"`
	// Runs counts the runs that started, by purpose, by whether MMHotline
	// started them by itself, and by state.
	Runs []TriageAgentUsageRuns `json:"runs"`
	// CodingAgents is what each coding agent's runs used.
	CodingAgents []TriageAgentUsageOfACodingAgent `json:"coding_agents"`
	// TimesTheRunLimitWasReached counts MMHotline's own warnings that the
	// limit was reached.
	TimesTheRunLimitWasReached int `json:"times_the_run_limit_was_reached"`
	// AlertsSentStraightToPeopleForTheRunLimit counts the alerts the limit
	// passed over: not held, no SOS wait, never sent to the agent.
	AlertsSentStraightToPeopleForTheRunLimit int                          `json:"alerts_sent_straight_to_people_for_the_run_limit"`
	HeldWarnings                             TriageAgentUsageHeldWarnings `json:"held_warnings"`
}

// TriageAgentUsageRuns counts the runs of one purpose and state.
type TriageAgentUsageRuns struct {
	Purpose            string `json:"purpose"`
	StartedByMMHotline bool   `json:"started_by_mmhotline"`
	State              string `json:"state"`
	Runs               int    `json:"runs"`
}

// TriageAgentUsageOfACodingAgent is what one coding agent's runs used.
// A failed or stopped run reports no use of its own; the next finished run
// of its coding agent session counts it.
type TriageAgentUsageOfACodingAgent struct {
	CodingAgent       string `json:"coding_agent"`
	Runs              int    `json:"runs"`
	FinishedRuns      int    `json:"finished_runs"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	// CostInUSDollars is the sum of the runs' costs, absent when the coding
	// agent reports no dollars, as Codex does.
	CostInUSDollars *float64 `json:"cost_in_us_dollars,omitempty"`
}

// TriageAgentUsageHeldWarnings counts the warnings held for the agent, by
// how their hold ended.
type TriageAgentUsageHeldWarnings struct {
	TakenOver int `json:"taken_over"`
	Delivered int `json:"delivered"`
	Resolved  int `json:"resolved"`
	StillHeld int `json:"still_held"`
}

// TriageAgentUsage reads the numbers since a time, for an administrator.
func (a *App) TriageAgentUsage(ctx context.Context, p Principal, since time.Time) (TriageAgentUsage, error) {
	var usage TriageAgentUsage
	if since.IsZero() {
		return usage, fmt.Errorf("%w: --since is required: the numbers cover what started or arrived at or after that time", ErrInvalid)
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var err error
		usage, err = readTriageAgentUsage(ctx, tx, since, a.Now().UTC())
		return err
	})
	return usage, err
}

// readTriageAgentUsage reads the numbers from since to now.
func readTriageAgentUsage(ctx context.Context, tx *sql.Tx, since, now time.Time) (TriageAgentUsage, error) {
	usage := TriageAgentUsage{Since: since.UTC(), Until: now, Runs: []TriageAgentUsageRuns{}, CodingAgents: []TriageAgentUsageOfACodingAgent{}}
	var err error
	if usage.MostRunsIn24Hours, err = readMostRunsIn24Hours(ctx, tx); err != nil {
		return usage, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT purpose,started_by='MMHotline',state,count(*) FROM triage_agent_runs WHERE started_at>=? GROUP BY 1,2,3 ORDER BY 1,2 DESC,3`, since.Unix())
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var runs TriageAgentUsageRuns
		if err = rows.Scan(&runs.Purpose, &runs.StartedByMMHotline, &runs.State, &runs.Runs); err != nil {
			rows.Close()
			return usage, err
		}
		usage.Runs = append(usage.Runs, runs)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return usage, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT coding_agent,count(*),sum(state='finished'),sum(input_tokens),sum(cached_input_tokens),sum(output_tokens),sum(cost_in_us_dollars) FROM triage_agent_runs WHERE started_at>=? GROUP BY coding_agent ORDER BY coding_agent`, since.Unix())
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var used TriageAgentUsageOfACodingAgent
		var cost sql.NullFloat64
		if err = rows.Scan(&used.CodingAgent, &used.Runs, &used.FinishedRuns, &used.InputTokens, &used.CachedInputTokens, &used.OutputTokens, &cost); err != nil {
			rows.Close()
			return usage, err
		}
		if cost.Valid {
			used.CostInUSDollars = &cost.Float64
		}
		usage.CodingAgents = append(usage.CodingAgents, used)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return usage, err
	}
	held := &usage.HeldWarnings
	err = tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM alerts WHERE actor=? AND severity='warn' AND received_at>=?2),
		(SELECT count(*) FROM alerts WHERE passed_over_for_the_run_limit=1 AND received_at>=?2),
		coalesce(sum(h.outcome='taken_over'),0), coalesce(sum(h.outcome='delivered'),0), coalesce(sum(h.outcome='resolved'),0), coalesce(sum(h.ended_at=0),0)
		FROM held_warn_alerts h JOIN alerts e ON e.id=h.alert_id WHERE e.received_at>=?2`, runLimitActor, since.Unix()).
		Scan(&usage.TimesTheRunLimitWasReached, &usage.AlertsSentStraightToPeopleForTheRunLimit, &held.TakenOver, &held.Delivered, &held.Resolved, &held.StillHeld)
	return usage, err
}

// Text is the numbers in words, as the command prints them and the software
// review's prompt carries them.
func (usage TriageAgentUsage) Text() string {
	lines := []string{fmt.Sprintf("Triage agent usage from %s to %s.", displayTime(usage.Since), displayTime(usage.Until)),
		"Run limit (triage_agent.most_runs_in_24_hours): " + usage.MostRunsIn24Hours.Text() + "."}
	if len(usage.Runs) == 0 {
		lines = append(lines, "Runs: none.")
	} else {
		lines = append(lines, "Runs by purpose and state:")
	}
	for _, runs := range usage.Runs {
		purpose := runs.Purpose
		if runs.Purpose == "software_review" {
			purpose += map[bool]string{true: " (scheduled)", false: " (started by a person)"}[runs.StartedByMMHotline]
		}
		lines = append(lines, fmt.Sprintf("- %s, %s: %d", purpose, runs.State, runs.Runs))
	}
	if len(usage.CodingAgents) > 0 {
		lines = append(lines, "What the runs used, per coding agent:")
	}
	for _, used := range usage.CodingAgents {
		title := codingAgentTitle(used.CodingAgent)
		line := fmt.Sprintf("- %s: %s, %d finished; %s input tokens (%s of them cached), %s output tokens", title, countText(used.Runs, "run", "runs"), used.FinishedRuns,
			wholeNumberText(used.InputTokens), wholeNumberText(used.CachedInputTokens), wholeNumberText(used.OutputTokens))
		if used.CostInUSDollars != nil {
			line += fmt.Sprintf("; US$%.2f.", *used.CostInUSDollars)
		} else {
			line += "; " + title + " reports tokens, not dollars."
		}
		lines = append(lines, line)
	}
	held := usage.HeldWarnings
	lines = append(lines,
		fmt.Sprintf("The run limit was reached %s. Alerts sent straight to people because of the run limit: %d.", countText(usage.TimesTheRunLimitWasReached, "time", "times"), usage.AlertsSentStraightToPeopleForTheRunLimit),
		fmt.Sprintf("Warnings held for the triage agent: %d taken over by the agent's report or request, %d delivered, %d resolved while held, %d still held.", held.TakenOver, held.Delivered, held.Resolved, held.StillHeld))
	return strings.Join(lines, "\n")
}

// softwareReviewUsageWindow is the window of the numbers in the software
// review's prompt: a week, as the team reviews them weekly.
const softwareReviewUsageWindow = 7 * 24 * time.Hour
