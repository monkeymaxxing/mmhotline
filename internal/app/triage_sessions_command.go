package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TriageSessionsCommandRequest is the input of `mmhotline triage-sessions
// list` and `show`.
type TriageSessionsCommandRequest struct {
	TriageSessionID string `json:"triage_session_id,omitempty"`
}

// CommandTriageSession is a triage session as the command prints it.
// Alerts, Incidents, and Runs are filled by show only.
type CommandTriageSession struct {
	TriageSessionID string    `json:"triage_session_id"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at,omitzero"`
	EndedBecause    string    `json:"ended_because,omitempty"`
	// Phase is working, waiting, stopping, handing_over, or ended.
	Phase         string                      `json:"phase"`
	PhaseText     string                      `json:"phase_text"`
	TakenOver     string                      `json:"taken_over,omitempty"`
	HandOverRunID string                      `json:"hand_over_run_id,omitempty"`
	AlertCount    int                         `json:"alert_count"`
	RunCount      int                         `json:"run_count"`
	IncidentIDs   []string                    `json:"incident_ids"`
	Alerts        []CommandTriageSessionAlert `json:"alerts,omitzero"`
	Incidents     []CommandIncident           `json:"incidents,omitzero"`
	Runs          []TriageSessionRun          `json:"runs,omitzero"`
}

// CommandTriageSessionAlert is an alert of a session: when it joined, and
// the run that carried it to the agent.
type CommandTriageSessionAlert struct {
	CommandAlert
	JoinedAt    time.Time `json:"joined_at"`
	SentInRunID string    `json:"sent_in_run_id,omitempty"`
}

func (a *App) triageSessionsCommand(ctx context.Context, p Principal, action string, request TriageSessionsCommandRequest, result *CommandResult) error {
	switch action {
	case "list":
		sessions, err := a.TriageSessions(ctx, p)
		if err != nil {
			return err
		}
		result.TriageSessions = []CommandTriageSession{}
		for _, session := range sessions {
			result.TriageSessions = append(result.TriageSessions, commandTriageSession(session))
		}
		result.Message = "Listed " + countText(len(sessions), "triage session", "triage sessions") + ", newest first."
		if len(sessions) == maximumTriageSessionsListed {
			result.Message += fmt.Sprintf(" These are the newest %d; mmhotline incidents show gives an older incident's session.", maximumTriageSessionsListed)
		}
		return nil
	case "show":
		if request.TriageSessionID == "" {
			return fmt.Errorf("%w: --triage-session-id is required", ErrInvalid)
		}
		members, err := a.membersForHistory(ctx, p)
		if err != nil {
			return err
		}
		session, err := a.TriageSession(ctx, p, request.TriageSessionID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: no triage session has ID %s", ErrNotFound, request.TriageSessionID)
		}
		if err != nil {
			return err
		}
		out := commandTriageSession(session)
		names := memberNames(members)
		out.Incidents = []CommandIncident{}
		for _, i := range session.Incidents {
			out.Incidents = append(out.Incidents, a.commandIncident(i, names))
		}
		out.Alerts = []CommandTriageSessionAlert{}
		for _, alert := range session.Alerts {
			out.Alerts = append(out.Alerts, CommandTriageSessionAlert{CommandAlert: a.commandAlert(alert.Alert), JoinedAt: alert.JoinedAt, SentInRunID: alert.SentInRunID})
		}
		out.Runs = session.Runs
		result.TriageSession = &out
		return nil
	}
	return ErrNotFound
}

func commandTriageSession(session TriageSessionRecord) CommandTriageSession {
	out := CommandTriageSession{TriageSessionID: session.ID, StartedAt: session.StartedAt, EndedAt: session.EndedAt, EndedBecause: session.EndedBecause, Phase: session.Phase, PhaseText: session.PhaseText, TakenOver: session.TakenOver, HandOverRunID: session.HandOverRunID, AlertCount: session.AlertCount, RunCount: session.RunCount, IncidentIDs: []string{}}
	for _, i := range session.Incidents {
		out.IncidentIDs = append(out.IncidentIDs, i.ID)
	}
	return out
}

// Text is the session's lines; its alerts, incidents, and runs follow for
// show.
func (s CommandTriageSession) Text() string {
	lines := []string{"Triage session " + s.TriageSessionID + ": " + s.PhaseText}
	if s.TakenOver != "" {
		lines = append(lines, s.TakenOver+".")
	}
	lines = append(lines, fmt.Sprintf("Started at %s. Alerts: %d. Runs: %d. SOS incidents: %s.", commandTime(s.StartedAt), s.AlertCount, s.RunCount, listOrNone(s.IncidentIDs)))
	if s.Runs == nil {
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "Alerts, oldest first:")
	for _, alert := range s.Alerts {
		sent := "waits for the next run"
		if alert.SentInRunID != "" {
			sent = "sent in run " + alert.SentInRunID
		}
		lines = append(lines, "  "+alert.Line()+"; "+sent)
	}
	lines = append(lines, "SOS incidents:")
	for _, incident := range s.Incidents {
		lines = append(lines, fmt.Sprintf("  %s %s: %s", incident.IncidentID, incidentStatusLabel(incident.Status), incident.Title))
	}
	lines = append(lines, "Runs, oldest first:")
	for _, run := range s.Runs {
		ended := "not yet"
		if run.EndedAt != nil {
			ended = commandTime(*run.EndedAt)
		}
		title := "Run"
		if run.ID == s.HandOverRunID {
			title = "Hand-over run"
		}
		lines = append(lines, fmt.Sprintf("  %s %s: %s. Started at %s, ended %s. %s", title, run.ID, run.State, commandTime(run.StartedAt), ended, run.UsageText()))
		if run.FailureReason != "" {
			lines = append(lines, "    Failure reason: "+run.FailureReason)
		}
		if run.EarlierStepsNotShown > 0 {
			lines = append(lines, fmt.Sprintf("    %d earlier steps are not shown.", run.EarlierStepsNotShown))
		}
		for _, step := range run.Steps {
			lines = append(lines, fmt.Sprintf("    %s %s: %s", commandTime(step.OccurredAt), step.Kind, indentedText(step.Text, "      ")))
		}
	}
	return strings.Join(lines, "\n")
}

func listOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
