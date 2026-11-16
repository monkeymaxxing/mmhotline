package app

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The triage sessions as people read them back after the fact, on the
// server with the mmhotline command: the newest sessions, and one session
// with its alerts, incidents, and runs. The phase, its text, and who took
// the session over come from the same state and functions as the incident
// page's live view (triage_session_live_view.go).

// maximumTriageSessionsListed bounds TriageSessions to the newest sessions;
// an older one is read by its ID, which `incidents show` gives for each of
// its incidents.
const maximumTriageSessionsListed = 50

// TriageSessionRecord is one triage session. Alerts and Runs are read by
// TriageSession only; the list counts them.
type TriageSessionRecord struct {
	ID                                                       string
	StartedAt, EndedAt                                       time.Time
	EndedBecause, Phase, PhaseText, TakenOver, HandOverRunID string
	AlertCount, RunCount                                     int
	// Incidents are the incidents of the session's alerts, the one whose
	// alert joined last first.
	Incidents []Incident
	Alerts    []TriageSessionAlert
	Runs      []TriageSessionRun
}

// TriageSessionAlert is an alert that joined a session.
type TriageSessionAlert struct {
	Alert
	JoinedAt time.Time
	// SentInRunID is the run that carried the alert to the agent, or ""
	// while it waits for the next run.
	SentInRunID string
}

// TriageSessionRun is a run of a session with its latest steps, as many as
// the incident page's live view shows.
type TriageSessionRun struct {
	TriageAgentRun
	EarlierStepsNotShown int `json:"earlier_steps_not_shown"`
}

// TriageSessions lists the newest triage sessions, newest first, without
// their alerts and runs.
func (a *App) TriageSessions(ctx context.Context, p Principal) ([]TriageSessionRecord, error) {
	var sessions []TriageSessionRecord
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM triage_sessions ORDER BY started_at DESC,rowid DESC LIMIT ?`, maximumTriageSessionsListed)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			session, err := readTriageSessionRecord(ctx, tx, id)
			if err != nil {
				return err
			}
			sessions = append(sessions, session)
		}
		return nil
	})
	return sessions, err
}

// TriageSession reads one triage session with its alerts, oldest first, and
// its runs, oldest first, each with its latest steps.
func (a *App) TriageSession(ctx context.Context, p Principal, sessionID string) (TriageSessionRecord, error) {
	var session TriageSessionRecord
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		session, err = readTriageSession(ctx, tx, sessionID)
		return err
	})
	return session, err
}

// readTriageSession reads one session with its alerts and runs, for the
// command and the session's page.
func readTriageSession(ctx context.Context, tx *sql.Tx, sessionID string) (TriageSessionRecord, error) {
	session, err := readTriageSessionRecord(ctx, tx, sessionID)
	if err != nil {
		return session, err
	}
	if session.Alerts, err = triageSessionAlerts(ctx, tx, sessionID); err != nil {
		return session, err
	}
	session.Runs, err = triageSessionRuns(ctx, tx, sessionID)
	return session, err
}

// readTriageSessionRecord reads a session's row, its phase, its incidents,
// and how many alerts and runs it has.
func readTriageSessionRecord(ctx context.Context, tx *sql.Tx, sessionID string) (TriageSessionRecord, error) {
	s, err := readTriageSessionState(ctx, tx, sessionID)
	if err != nil {
		return TriageSessionRecord{}, err
	}
	session := TriageSessionRecord{ID: s.ID, StartedAt: s.StartedAt, EndedAt: s.EndedAt, EndedBecause: s.EndedBecause, Phase: s.phase(), PhaseText: s.phaseText(), HandOverRunID: s.HandOverRunID}
	if s.TakenOverByMemberID != "" {
		if session.TakenOver, err = s.takenOverText(ctx, tx); err != nil {
			return session, err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM triage_session_alerts WHERE triage_session_id=?1),(SELECT count(*) FROM triage_agent_runs WHERE triage_session_id=?1)`, sessionID).Scan(&session.AlertCount, &session.RunCount); err != nil {
		return session, err
	}
	session.Incidents, err = triageSessionIncidents(ctx, tx, sessionID)
	return session, err
}

// alertAndMore scans an alert's columns, then more columns after them.
type alertAndMore struct {
	row  scanner
	more []any
}

func (s alertAndMore) Scan(dest ...any) error { return s.row.Scan(append(dest, s.more...)...) }

func triageSessionAlerts(ctx context.Context, tx *sql.Tx, sessionID string) ([]TriageSessionAlert, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+alertColumns+`,s.joined_at,s.sent_in_run_id FROM triage_session_alerts s JOIN alerts ON alerts.id=s.alert_id WHERE s.triage_session_id=? ORDER BY alerts.sequence`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	alerts := []TriageSessionAlert{}
	for rows.Next() {
		var alert TriageSessionAlert
		var joined int64
		if alert.Alert, err = scanAlert(alertAndMore{rows, []any{&joined, &alert.SentInRunID}}); err != nil {
			return nil, err
		}
		alert.JoinedAt = time.Unix(joined, 0).UTC()
		alerts = append(alerts, alert)
	}
	return alerts, rows.Err()
}

func triageSessionRuns(ctx context.Context, tx *sql.Tx, sessionID string) ([]TriageSessionRun, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+triageAgentRunColumns+` FROM triage_agent_runs WHERE triage_session_id=? ORDER BY started_at,rowid`, sessionID)
	if err != nil {
		return nil, err
	}
	runs := []TriageSessionRun{}
	for rows.Next() {
		run, err := scanTriageAgentRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		runs = append(runs, TriageSessionRun{TriageAgentRun: run})
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for index := range runs {
		if runs[index].Steps, runs[index].EarlierStepsNotShown, err = triageAgentRunSteps(ctx, tx, runs[index].ID, maximumLiveViewSteps); err != nil {
			return nil, err
		}
	}
	return runs, nil
}
