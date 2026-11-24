package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A person takes a triage session over from the agent with "Stop after the
// current step" on the incident page:
// docs/changing-mmhotline/design/triage-agent.md#a-person-takes-over.
//
// TakeOverTriageSession records who and when on the session. From then on
// the background loop drives the session from that record:
//
//	stopping      the run from before the Stop is still going; the loop stops
//	              it after the agent's current step, the stop every run gets
//	handing_over  the loop resumes the agent once, with the hand-over prompt,
//	              and records that run in hand_over_run_id; MMHotline refuses
//	              every playbook run, and the agent commits its note and reports
//	ended         the hand-over run ended: the session ends, and the next
//	              alert starts a new one
//
// A hand-over run that MMHotline's own stop or restart cut short starts
// again, as every run so cut is resumed.

// The phases of a triage session, as the incident page names them.
const (
	triageSessionWorking     = "working"
	triageSessionWaiting     = "waiting"
	triageSessionStopping    = "stopping"
	triageSessionHandingOver = "handing_over"
	triageSessionEnded       = "ended"
)

// triageAgentTakeOverStopReason is the reason recorded with each warned run
// a take-over stops.
const triageAgentTakeOverStopReason = "Took over from the triage agent."

// triageSessionState is a session's row and its run that is running, which
// the background loop and the incident page read.
type triageSessionState struct {
	ID                                 string
	StartedAt, EndedAt                 time.Time
	EndedBecause                       string
	TakenOverByMemberID, HandOverRunID string
	TakenOverAt                        time.Time
	// RunningRunID is the session's run whose state is running, or "".
	RunningRunID string
}

func readTriageSessionState(ctx context.Context, tx *sql.Tx, sessionID string) (triageSessionState, error) {
	s := triageSessionState{ID: sessionID}
	var started, ended, takenOver int64
	err := tx.QueryRowContext(ctx, `SELECT started_at,ended_at,ended_because,taken_over_by_member_id,taken_over_at,hand_over_run_id,coalesce((SELECT id FROM triage_agent_runs WHERE triage_session_id=?1 AND state='running'),'') FROM triage_sessions WHERE id=?1`, sessionID).Scan(&started, &ended, &s.EndedBecause, &s.TakenOverByMemberID, &takenOver, &s.HandOverRunID, &s.RunningRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	s.StartedAt = time.Unix(started, 0).UTC()
	if ended != 0 {
		s.EndedAt = time.Unix(ended, 0).UTC()
	}
	if takenOver != 0 {
		s.TakenOverAt = time.Unix(takenOver, 0).UTC()
	}
	return s, err
}

func (s triageSessionState) phase() string {
	switch {
	case !s.EndedAt.IsZero():
		return triageSessionEnded
	case s.TakenOverByMemberID == "" && s.RunningRunID != "":
		return triageSessionWorking
	case s.TakenOverByMemberID == "":
		return triageSessionWaiting
	case s.RunningRunID != "" && s.RunningRunID != s.HandOverRunID:
		return triageSessionStopping
	}
	return triageSessionHandingOver
}

// What the background loop does next for a session a person took over.
const (
	handOverStopTheRun    = "stop_the_run"
	handOverWait          = "wait"
	handOverStart         = "start"
	handOverEndTheSession = "end_the_session"
)

// handOverNext says what the taken-over session s needs now.
func handOverNext(ctx context.Context, tx *sql.Tx, s triageSessionState) (string, error) {
	switch {
	case s.RunningRunID != "" && s.RunningRunID != s.HandOverRunID:
		return handOverStopTheRun, nil
	case s.RunningRunID != "":
		return handOverWait, nil
	case s.HandOverRunID == "":
		return handOverStart, nil
	}
	var cause runEndCause
	if err := tx.QueryRowContext(ctx, `SELECT end_cause FROM triage_agent_runs WHERE id=?`, s.HandOverRunID).Scan(&cause); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	// Not the agent's failure: the hand-over is resumed as any run so cut.
	if cause.cutShortByMMHotline() {
		return handOverStart, nil
	}
	return handOverEndTheSession, nil
}

// takenOverText says who took the session over and when, such as "Barbara
// pressed Stop at 2026-11-03 03:10:00 UTC".
func (s triageSessionState) takenOverText(ctx context.Context, tx *sql.Tx) (string, error) {
	name, err := incidentMemberName(ctx, tx, s.TakenOverByMemberID)
	return name + " pressed Stop at " + displayTime(s.TakenOverAt), err
}

// handOverEndedBecause is why a taken-over session ends: after its hand-over,
// or at once when the agent never began and so has nothing to hand over.
func (s triageSessionState) handOverEndedBecause(ctx context.Context, tx *sql.Tx, nothingToHandOver bool) (string, error) {
	who, err := s.takenOverText(ctx, tx)
	if nothingToHandOver {
		return "A person took over: " + who + ", before the triage agent began, so it had nothing to hand over.", err
	}
	return "A person took over: " + who + ".", err
}

// TakeOverTriageSession records that a team member took the triage session
// over from the agent: "Stop after the current step" on the incident page.
// Within a second the background loop stops the session's run after the
// agent's current step, with the stop every run gets, and then resumes the
// agent once for its hand-over. In the same transaction an SOS of the
// session that waits for the agent wakes the person on call, because the
// agent no longer works on it, and the session's warned runs stop before
// their run time, because each would run a fix for an agent a person
// replaced. Approval requests stay open: nothing runs without a person.
func (a *App) TakeOverTriageSession(ctx context.Context, p Principal, sessionID string) error {
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		// The history and the agent name the person, which the mmhotline
		// command, with no member ID, is not.
		if m.ID == "" {
			return fmt.Errorf("%w: only a signed-in team member stops the triage agent", ErrForbidden)
		}
		s, err := readTriageSessionState(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if !s.EndedAt.IsZero() {
			return fmt.Errorf("%w: this triage session already ended: %s", ErrConflict, s.EndedBecause)
		}
		if s.TakenOverByMemberID != "" {
			who, err := s.takenOverText(ctx, tx)
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: the triage agent is already stopping: %s", ErrConflict, who)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE triage_sessions SET taken_over_by_member_id=?,taken_over_at=? WHERE id=?`, m.ID, now.Unix(), sessionID); err != nil {
			return err
		}
		incidents, err := triageSessionIncidents(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		for _, i := range incidents {
			if err = recordIncidentAction(ctx, tx, i, "member:"+m.ID, "triage_agent_stopped", "The triage agent finishes its current step, writes what it found, did, and was about to do as a report and a note, and then its triage session ends. MMHotline refuses any new playbook run the triage agent asks for.", "", "", now); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person WHERE triage_session_id=? AND rule=? AND state=? AND `+warnedRunTimeColumn+`>? ORDER BY judged_at,id`, sessionID, warnWaitThenAct, approvalOpen, now.Unix())
		if err != nil {
			return err
		}
		var warned []approvalRequest
		for rows.Next() {
			r, err := scanApprovalRequest(rows)
			if err != nil {
				rows.Close()
				return err
			}
			warned = append(warned, r)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, r := range warned {
			if err = stopWarnedRun(ctx, tx, r, m, triageAgentTakeOverStopReason, now); err != nil {
				return err
			}
		}
		// The agent works on nothing more, so every held warning reaches
		// people now; they are only marked here, so a full delivery queue
		// cannot undo the Stop.
		if err = releaseEveryHeldWarn(ctx, tx, m.Name+" took the triage session over from the triage agent.", now); err != nil {
			return err
		}
		return wakeIncidentsHeldForTriageAgent(ctx, tx, m.Name+" stopped the triage agent; waking the person on call", now)
	})
}

// takenOverRefusal returns why MMHotline runs no playbook for the run runID:
// a person took its triage session over. It is "" when nobody did, or the
// run belongs to no session.
func takenOverRefusal(ctx context.Context, tx *sql.Tx, runID string) (string, error) {
	var memberID string
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT s.taken_over_by_member_id,s.taken_over_at FROM triage_agent_runs r JOIN triage_sessions s ON s.id=r.triage_session_id WHERE r.id=?`, runID).Scan(&memberID, &at)
	if errors.Is(err, sql.ErrNoRows) || err == nil && memberID == "" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	name, err := incidentMemberName(ctx, tx, memberID)
	return name + " took this triage session over from the triage agent at " + time.Unix(at, 0).UTC().Format("15:04 UTC") + ", so MMHotline runs no more playbooks in it.", err
}
