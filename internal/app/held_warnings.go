package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A warn alert that starts a triage session is held for the triage agent:
// docs/changing-mmhotline/design/triage-agent.md#a-warn-waits-for-the-agent.
//
// The hold keeps the warn's notification, not its deliveries: intake saves
// the alert and a row of held_warn_alerts and queues nothing. The row ends
// exactly once, in the transaction that either queues the warn's notification
// (outcome delivered) or records that the agent's own request or report took
// the warn over (outcome taken_over, with a link to it). So a restart neither
// drops nor repeats a held warn, and a warn never ends both cancelled and
// unsent: only the agent's report or request cancels it.
//
// held_until is when the wait ends, the alert's arrival plus
// triage_agent.warn_waits_for_the_agent_minutes. triage_session_id is the
// session the warn started or joined, "" until the background loop reads the
// alert.
//
// A warn that starts a session is taken over by any report or accepted
// playbook request of the session. A warn that joins a session is held only
// while one of the session's SOS incidents waits for the agent
// (held_with_sos_incident_id says which, the one that waits longest, "" for a
// warn that started its session), and is taken over only by a report or
// request of a run that carried it, because in a busy session an unrelated
// report must not swallow it. It also ends, sending nothing, when its own
// alert resolves (outcome resolved), and it is delivered at once when an SOS
// of the session wakes the person on call. Its held_until is the later of
// its own wait and that SOS's deadline, and a server replacement that moves
// the SOS's deadline moves it too (startServerReplacement).
const heldWarnSchema = `
CREATE TABLE held_warn_alerts (
 alert_id TEXT PRIMARY KEY REFERENCES alerts(id),
 held_until INTEGER NOT NULL,
 triage_session_id TEXT NOT NULL DEFAULT '',
 held_with_sos_incident_id TEXT NOT NULL DEFAULT '',
 ended_at INTEGER NOT NULL DEFAULT 0,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','delivered','taken_over','resolved')),
 outcome_reason TEXT NOT NULL DEFAULT '',
 taken_over_by TEXT NOT NULL DEFAULT '' CHECK(taken_over_by IN ('','report','playbook_run_request')),
 taken_over_link TEXT NOT NULL DEFAULT ''
);
`

// The settings file's triage_agent.warn_waits_for_the_agent_minutes: a warn
// that starts a triage session, or joins one while an SOS waits for the
// agent, is held this long after it arrived, so a
// change the agent handles itself reaches people as one message, the agent's
// own, and not as a warning followed by a report. The default is 0, which
// holds nothing, as MMHotline behaved before the setting: a team chooses to
// hold warnings. The maximum is 15 minutes, as for an SOS, because a stuck or
// wrong agent must not hold a warning longer.
const (
	warnWaitsForTheAgentMaximumMinutes          = 15
	warnWaitsForTheAgentMinutesBeforeTheSetting = 0
)

// warnWaitsForTheAgent is the applied wait of a warn for the triage agent.
func warnWaitsForTheAgent(ctx context.Context, tx *sql.Tx) (time.Duration, error) {
	minutes := warnWaitsForTheAgentMinutesBeforeTheSetting
	_, err := appliedSettingsValue(ctx, tx, settingsWarnWaitsForTheAgentPart.name, &minutes)
	return time.Duration(minutes) * time.Minute, err
}

// holdWarnForTheAgent saves the hold for a warn that has just been admitted
// and says whether the warn is held, in which case the caller queues nothing.
// A warn is held when the wait is above 0, the triage agent is set up, and
// the alert is one that starts or joins a session (triageSessionAlertCondition)
// from an alert source: not a notice from MMHotline, not one made by hand
// (a person who raises an alert wants people now), and not one meant for
// chosen people only. Whether it starts a session, or joins one already
// running, is known when the background loop reads it.
func holdWarnForTheAgent(ctx context.Context, tx *sql.Tx, e Alert, onlyForChosenPeople bool, at time.Time) (bool, error) {
	if e.Severity != "warn" || e.SourceID == "system" || e.SourceID == "manual" || onlyForChosenPeople {
		return false, nil
	}
	wait, err := warnWaitsForTheAgent(ctx, tx)
	if err != nil || wait <= 0 {
		return false, err
	}
	var setUp bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup)`).Scan(&setUp); err != nil || !setUp {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO held_warn_alerts(alert_id,held_until) VALUES(?,?)`, e.ID, at.Add(wait).Unix())
	return err == nil, err
}

// sessionHoldsItsWarn settles the hold of a held warn the background loop has
// just read into the session. A warn that started the session stays held. One
// that joined stays held while an SOS of the session waits for the agent, with
// the later of its own wait and that SOS's deadline, and ends at once when its
// own alert already resolved; otherwise it is delivered at once. A warn that
// was not held has no row and nothing happens.
func sessionHoldsItsWarn(ctx context.Context, tx *sql.Tx, alertID, sessionID string, startedTheSession bool, at time.Time) error {
	var heldUntil int64
	err := tx.QueryRowContext(ctx, `SELECT held_until FROM held_warn_alerts WHERE alert_id=? AND ended_at=0`, alertID).Scan(&heldUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if startedTheSession {
		_, err = tx.ExecContext(ctx, `UPDATE held_warn_alerts SET triage_session_id=? WHERE alert_id=?`, sessionID, alertID)
		return err
	}
	var sosIncidentID string
	var sosDeadline int64
	err = tx.QueryRowContext(ctx, `SELECT i.id,i.deadline FROM incidents i WHERE i.status='open' AND i.step=? AND EXISTS(SELECT 1 FROM alerts e JOIN triage_session_alerts s ON s.alert_id=e.id WHERE e.incident_id=i.id AND s.triage_session_id=?) ORDER BY i.deadline DESC,i.id LIMIT 1`, triageAgentStep, sessionID).Scan(&sosIncidentID, &sosDeadline)
	if errors.Is(err, sql.ErrNoRows) {
		return deliverHeldWarn(ctx, tx, alertID, "The warning joined a triage session in which no SOS waited for the triage agent, and MMHotline holds a joining warning only with such an SOS.", at)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE held_warn_alerts SET triage_session_id=?,held_with_sos_incident_id=?,held_until=? WHERE alert_id=?`, sessionID, sosIncidentID, max(heldUntil, sosDeadline), alertID); err != nil {
		return err
	}
	// Its alert may have resolved before the loop read it.
	var resolved bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alerts e JOIN alerts r ON r.source_id=e.source_id AND r.sender_incident_key=e.sender_incident_key WHERE e.id=? AND e.sender_incident_key!='' AND r.severity='resolved-warn' AND r.sequence>e.sequence)`, alertID).Scan(&resolved); err != nil || !resolved {
		return err
	}
	return endJoinedHeldWarnsBecauseResolved(ctx, tx, at, "alert_id=?", alertID)
}

// warnResolvedWhileHeldReason goes with the alert's page.
const warnResolvedWhileHeldReason = "The warning's alert resolved while the triage agent handled the outage."

// heldWarnAlertResolved ends the holds of the joined warns that the
// resolved-warn alert e resolves: those of its alert source and
// sender_incident_key. Nobody needed to hear them, as with a held SOS that
// resolves. A warn that started its session keeps its rules, and one the loop
// has not read yet is settled when it is read (sessionHoldsItsWarn).
func heldWarnAlertResolved(ctx context.Context, tx *sql.Tx, e Alert, at time.Time) error {
	if e.Severity != "resolved-warn" || e.SenderIncidentKey == "" {
		return nil
	}
	return endJoinedHeldWarnsBecauseResolved(ctx, tx, at, "alert_id IN (SELECT id FROM alerts WHERE source_id=? AND sender_incident_key=? AND severity='warn')", e.SourceID, e.SenderIncidentKey)
}

// endJoinedHeldWarnsBecauseResolved ends, sending nothing, the open holds of
// joined warns that where picks.
func endJoinedHeldWarnsBecauseResolved(ctx context.Context, tx *sql.Tx, at time.Time, where string, args ...any) error {
	_, err := tx.ExecContext(ctx, `UPDATE held_warn_alerts SET ended_at=?,outcome='resolved',outcome_reason=? WHERE ended_at=0 AND held_with_sos_incident_id!='' AND `+where, append([]any{at.Unix(), warnResolvedWhileHeldReason}, args...)...)
	return err
}

// deliverHeldWarn ends the hold of a warn by queueing its notification, once:
// it does nothing when the hold has ended, or when the alert was not held.
// because goes with the alert's page.
func deliverHeldWarn(ctx context.Context, tx *sql.Tx, alertID, because string, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE held_warn_alerts SET ended_at=?,outcome='delivered',outcome_reason=? WHERE alert_id=? AND ended_at=0`, at.Unix(), because, alertID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return err
	}
	var e Alert
	var received int64
	var happened int64
	if err = tx.QueryRowContext(ctx, `SELECT source_id,title,message,notes,received_at,happened_at FROM alerts WHERE id=?`, alertID).Scan(&e.SourceID, &e.Title, &e.Message, &e.Notes, &received, &happened); err != nil {
		return err
	}
	e.ReceivedAt, e.HappenedAt = time.Unix(received, 0).UTC(), deadlineTime(happened)
	notes := strings.TrimSpace(e.Notes + " MMHotline held this warning for the triage agent from " + e.ReceivedAt.Format("15:04") + " UTC to " + at.UTC().Format("15:04") + " UTC.")
	return QueueNotification(ctx, tx, Notification{Key: alertID, AlertID: alertID, Severity: "warn", Title: e.Title, Message: e.Message, Notes: notes, Purpose: notificationPurpose(e.SourceID), HappenedAt: e.HappenedAt, At: at})
}

// heldWarnTakenOver ends the holds of warns that the triage agent's own
// request or report, from run runID of session sessionID, took over: nothing
// is sent for them. The warn that started the session is taken over by any
// request or report of the session. A warn that joined is taken over only by
// a run that carried it, so an unrelated report in a busy session does not
// swallow it. by is "report" or "playbook_run_request"; link goes to the
// report or to the session whose agent asked for the playbook run.
func heldWarnTakenOver(ctx context.Context, tx *sql.Tx, sessionID, runID, by, link string, at time.Time) error {
	if sessionID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE held_warn_alerts SET ended_at=?,outcome='taken_over',taken_over_by=?,taken_over_link=? WHERE ended_at=0 AND ((held_with_sos_incident_id='' AND triage_session_id=?) OR alert_id IN (SELECT alert_id FROM triage_session_alerts WHERE sent_in_run_id=?))`, at.Unix(), by, link, sessionID, runID)
	return err
}

// deliverJoinedHeldWarns delivers, at once, the joined warns held with an SOS,
// because an SOS of the session wakes the person on call: the person is told
// everything then. why is the wake's reason.
func deliverJoinedHeldWarns(ctx context.Context, tx *sql.Tx, why string, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT alert_id FROM held_warn_alerts WHERE ended_at=0 AND held_with_sos_incident_id!='' ORDER BY held_until,alert_id`)
	if err != nil {
		return err
	}
	var alertIDs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		alertIDs = append(alertIDs, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	because := "An SOS woke the person on call: " + strings.TrimSuffix(strings.TrimSuffix(why, "; waking the person on call"), ". Waking the person on call") + "."
	for _, id := range alertIDs {
		if err = deliverHeldWarn(ctx, tx, id, because, at); err != nil {
			return err
		}
	}
	return nil
}

// settleHeldWarns delivers each held warn the triage agent cannot take over:
// at once when the agent cannot run (cannotRun says why, "" when it can),
// no session took the warn, its session ended, or its run failed or ended
// without a request or report; and otherwise at the end of the wait. A warn
// the agent took over is already ended (heldWarnTakenOver). It runs in the
// background loop's turn for triage sessions, after the alerts that arrived
// since the last visit were read.
func settleHeldWarns(ctx context.Context, tx *sql.Tx, cannotRun string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT h.alert_id,h.held_until,h.triage_session_id,coalesce(s.ended_at,0),coalesce(s.ended_because,''),h.held_with_sos_incident_id!='',coalesce((SELECT sent_in_run_id FROM triage_session_alerts WHERE alert_id=h.alert_id),'') FROM held_warn_alerts h LEFT JOIN triage_sessions s ON s.id=h.triage_session_id WHERE h.ended_at=0 ORDER BY h.held_until,h.alert_id`)
	if err != nil {
		return err
	}
	type held struct {
		alertID, sessionID, sessionEndedBecause, carriedByRunID string
		heldUntil, sessionEndedAt                               int64
		joined                                                  bool
	}
	var open []held
	for rows.Next() {
		var h held
		if err = rows.Scan(&h.alertID, &h.heldUntil, &h.sessionID, &h.sessionEndedAt, &h.sessionEndedBecause, &h.joined, &h.carriedByRunID); err != nil {
			rows.Close()
			return err
		}
		open = append(open, h)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, h := range open {
		because := ""
		switch {
		case cannotRun != "":
			because = cannotRun
		case h.sessionID == "":
			because = "No triage session took the warning."
		case h.sessionEndedAt != 0:
			because = "The triage session ended: " + h.sessionEndedBecause
		default:
			// A warn that started the session answers to the session's
			// latest run. A joined one answers to the run that carried it,
			// and until a run carries it, to the deadline alone.
			if !h.joined || h.carriedByRunID != "" {
				if because, err = heldWarnRunEnded(ctx, tx, h.sessionID, h.carriedByRunID); err != nil {
					return err
				}
			}
			if because == "" && now.Unix() >= h.heldUntil {
				because = "The wait ended, and the triage agent had sent no report and asked for no playbook run."
			}
		}
		if because != "" {
			if err = deliverHeldWarn(ctx, tx, h.alertID, because, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// heldWarnRunEnded says why a run cannot take over a warn any more: it
// failed, or ended with no request or report, because a taken over warn ended
// already. The run is the one runID names, a joined warn's carrying run, or
// the session's latest run when runID is "". It returns "" while the run goes
// on, before the first run starts, and while a run that MMHotline's own stop
// or restart cut short waits to be resumed. A playbook request that waits for
// MMHotline's answer is a request the agent made, so it keeps the warn held
// until the end of the wait.
func heldWarnRunEnded(ctx context.Context, tx *sql.Tx, sessionID, runID string) (string, error) {
	runWhere, requestWhere, arg := `triage_session_id=? AND purpose='triage_session'`, `r.triage_session_id=?`, sessionID
	if runID != "" {
		runWhere, requestWhere, arg = `id=?`, `r.id=?`, runID
	}
	var state, reason string
	err := tx.QueryRowContext(ctx, `SELECT state,failure_reason FROM triage_agent_runs WHERE `+runWhere+` ORDER BY started_at DESC,rowid DESC LIMIT 1`, arg).Scan(&state, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || state == "running" {
		return "", err
	}
	if state == "failed" && (reason == triageRunCutByStop || reason == triageRunCutByRestart) {
		return "", nil
	}
	var answering bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_requests q JOIN triage_agent_runs r ON r.id=q.run_id WHERE `+requestWhere+` AND q.kind='playbook_run' AND q.outcome='')`, arg).Scan(&answering); err != nil || answering {
		return "", err
	}
	if state == "failed" {
		return "The triage agent's run failed: " + reason, nil
	}
	return "The triage agent's run ended without a report or a playbook run request.", nil
}

// nextHeldWarnDeadline is when the earliest held warn's wait ends, for the
// background loop's next visit.
func nextHeldWarnDeadline(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var next sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT min(held_until) FROM held_warn_alerts WHERE ended_at=0`).Scan(&next); err != nil || !next.Valid {
		return time.Time{}, err
	}
	return time.Unix(next.Int64, 0).UTC(), nil
}

// HeldWarn is what the alert's page and the dashboard say about a warn that
// was held for the triage agent.
type HeldWarn struct {
	AlertID, Title string
	Received       time.Time
	HeldUntil      time.Time
	// Minutes is the wait the warn got, from its arrival.
	Minutes int
	// Held is true while the wait goes on. Otherwise Ended is when it ended,
	// and TakenOver says whether the agent's report or request took the warn
	// over (TakenOverBy, TakenOverLink), or the warn was delivered.
	Held          bool
	Ended         time.Time
	TakenOver     bool
	TakenOverBy   string
	TakenOverLink string
	// OutcomeReason says why the warn was delivered, or why it ended
	// without being sent because its alert resolved.
	OutcomeReason string
	// SOSTitle is the title of the SOS a joined warn is held with, "" for
	// a warn that started its session. Resolved is true when the warn ended
	// because its own alert resolved.
	SOSTitle string
	Resolved bool
}

// JoinedSentence is the plain statement of the hold of a warn that joined a
// session, with the time its wait ends, already written for the page.
func (h HeldWarn) JoinedSentence(until string) string {
	return "MMHotline holds this warning with the SOS the triage agent is handling (" + h.SOSTitle + "); it reaches people if the person on call is woken, or at " + until + ", unless the agent's report covers it or it resolves first."
}

// Sentence is the plain statement of the hold, without its times.
func (h HeldWarn) Sentence() string {
	return fmt.Sprintf("MMHotline holds this warning for up to %d %s while the triage agent looks into it; it reaches people then, unless the agent takes it over.", h.Minutes, map[bool]string{true: "minute", false: "minutes"}[h.Minutes == 1])
}

// TakenOverByText names what took the warn over.
func (h HeldWarn) TakenOverByText() string {
	if h.TakenOverBy == "report" {
		return "the triage agent's report"
	}
	return "the triage agent's playbook run request"
}

// readHeldWarns reads the hold of each warn alert of where, or all the holds
// that are still open when where is "".
func readHeldWarns(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]HeldWarn, error) {
	rows, err := tx.QueryContext(ctx, `SELECT h.alert_id,e.title,e.received_at,h.held_until,h.ended_at,h.outcome,h.taken_over_by,h.taken_over_link,h.outcome_reason,coalesce((SELECT o.title FROM incidents i JOIN alerts o ON o.id=i.opening_alert_id WHERE i.id=h.held_with_sos_incident_id),'') FROM held_warn_alerts h JOIN alerts e ON e.id=h.alert_id WHERE `+where+` ORDER BY e.sequence`, args...)
	if err != nil {
		return nil, err
	}
	var warns []HeldWarn
	for rows.Next() {
		var h HeldWarn
		var received, until, ended int64
		var outcome string
		if err = rows.Scan(&h.AlertID, &h.Title, &received, &until, &ended, &outcome, &h.TakenOverBy, &h.TakenOverLink, &h.OutcomeReason, &h.SOSTitle); err != nil {
			rows.Close()
			return nil, err
		}
		h.Received, h.HeldUntil, h.Ended = time.Unix(received, 0).UTC(), time.Unix(until, 0).UTC(), deadlineTime(ended)
		h.Minutes = int(h.HeldUntil.Sub(h.Received) / time.Minute)
		h.Held, h.TakenOver, h.Resolved = ended == 0, outcome == "taken_over", outcome == "resolved"
		warns = append(warns, h)
	}
	return warns, errors.Join(rows.Err(), rows.Close())
}

// heldWarnOfAlert reads the hold of an alert for its page, nil when the alert
// was not held.
func (a *App) heldWarnOfAlert(ctx context.Context, p Principal, alertID string) (*HeldWarn, error) {
	var held *HeldWarn
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		warns, err := readHeldWarns(ctx, tx, `h.alert_id=?`, alertID)
		if len(warns) == 1 {
			held = &warns[0]
		}
		return err
	})
	return held, err
}

// resolvedWarnOfUnsentWarnsNote is the alert's note when MMHotline sends a
// resolved-warn to nobody.
const resolvedWarnOfUnsentWarnsNote = "MMHotline sent nothing: the warnings this message resolves were held for the triage agent and never sent, so nobody needs to hear that they ended."

// resolvesOnlyWarnsNeverSent reports whether the resolved-warn alert e ends
// only warns MMHotline held and never sent (their alert resolved while held,
// or the triage agent took them over): the warns of its alert source and
// sender_incident_key since the previous resolved-warn of that key. It then
// notes why on the alert, which notifies nobody: a resolved message tells the
// end of something the person was told about. A resolved-warn with no such
// warn, or with any warn MMHotline sent or still holds, is sent as usual.
func resolvesOnlyWarnsNeverSent(ctx context.Context, tx *sql.Tx, e Alert) (bool, error) {
	if e.Severity != "resolved-warn" || e.SenderIncidentKey == "" {
		return false, nil
	}
	var unsent, other int
	err := tx.QueryRowContext(ctx, `SELECT
 COALESCE(SUM(h.outcome IN ('resolved','taken_over') AND h.ended_at>0),0),
 COALESCE(SUM(h.alert_id IS NULL OR h.outcome NOT IN ('resolved','taken_over') OR h.ended_at=0),0)
 FROM alerts a LEFT JOIN held_warn_alerts h ON h.alert_id=a.id
 WHERE a.source_id=?1 AND a.sender_incident_key=?2 AND a.severity='warn' AND a.sequence<?3
 AND a.sequence>COALESCE((SELECT MAX(sequence) FROM alerts WHERE source_id=?1 AND sender_incident_key=?2 AND severity='resolved-warn' AND sequence<?3),0)`,
		e.SourceID, e.SenderIncidentKey, e.Sequence).Scan(&unsent, &other)
	if err != nil || unsent == 0 || other > 0 {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE alerts SET notes=? WHERE id=?`, resolvedWarnOfUnsentWarnsNote, e.ID)
	return true, err
}
