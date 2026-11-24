package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A warn alert from an alert source is held for the triage agent:
// docs/changing-mmhotline/design/triage-agent.md#a-warn-waits-for-the-agent.
//
// The hold keeps the warn's notification, not its deliveries: intake saves
// the alert and a row of held_warn_alerts and queues nothing. Every held warn
// follows one set of rules, whether it started its session or joined one,
// and ends exactly once, with its outcome:
//
//   - taken_over, nothing sent: a report of the triage agent, or a playbook
//     run request for which MMHotline tells a person now (a new approval
//     request or a new warned run), named it (triage_agent_covered_alerts)
//     from a run that carried it;
//   - resolved, nothing sent: its own alert resolved while it was held;
//   - delivered: its notification was queued.
//
// Delivery takes two steps. A transaction that decides that a warn must
// reach people (an SOS of the session wakes the person on call, a person
// takes the session over, the agent cannot run, the run that carried the
// warn ended without naming it, its session ended) only marks it:
// release_requested_at and release_reason. DeliverReleasedHeldAlerts, a
// background step of its own, queues the marked warns and those whose wait
// ended, each in its own transaction. So a full delivery queue never rolls
// back an SOS's wake, the session's work, or a person's Stop: a warn that
// meets a full queue stays marked and goes at a later visit, also after a
// restart. A marked warn can no longer be taken over or resolved.
//
// held_until is when the wait ends: the alert's arrival plus
// triage_agent.warn_waits_for_the_agent_minutes, or, when the warn joins a
// session in which an SOS waits for the agent, that SOS's deadline if it is
// later (held_with_sos_incident_id names the SOS, the one that waits
// longest). A server replacement moves the deadline of every held warn of
// its session as it moves the SOS's (startServerReplacement).
// triage_session_id is the session the warn joined, "" until the background
// loop reads the alert.
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
 taken_over_link TEXT NOT NULL DEFAULT '',
 ` + heldWarnReleaseRequestedAtColumn + `,
 ` + heldWarnReleaseReasonColumn + `
);
`

// The columns of a held warn's release, in the table and in the upgrade step
// that adds them (addHoldRules).
const (
	heldWarnReleaseRequestedAtColumn = `release_requested_at INTEGER NOT NULL DEFAULT 0`
	heldWarnReleaseReasonColumn      = `release_reason TEXT NOT NULL DEFAULT ''`
)

// addHoldRules is database format 4: the release of a held warn, recorded
// apart from its delivery; the alerts each report and playbook run request
// of the triage agent covers, and the alert each report sent; and an SOS's
// subscribed notification that waits with its first waking step.
func addHoldRules(tx *sql.Tx) error {
	for _, statement := range []string{
		`ALTER TABLE held_warn_alerts ADD COLUMN ` + heldWarnReleaseRequestedAtColumn,
		`ALTER TABLE held_warn_alerts ADD COLUMN ` + heldWarnReleaseReasonColumn,
		`ALTER TABLE incidents ADD COLUMN ` + incidentSubscribedAlertNotificationWaitsColumn,
		`ALTER TABLE triage_agent_requests ADD COLUMN ` + triageAgentRequestReportAlertIDColumn,
		triageAgentCoveredAlertsSchema,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// The settings file's triage_agent.warn_waits_for_the_agent_minutes: how
// long a warn from an alert source is held for the triage agent after it
// arrived, so a change the agent handles itself reaches people as one
// message, the agent's own, and not as a warning followed by a report. 0
// holds nothing.
//
// The default is 15 minutes: a run takes 30 to 70 seconds, plus a queue
// behind another run, a software review being stopped, or an MMX run that
// holds the systems repository, and a warn is not urgent. The maximum is 30
// minutes: a warning held longer than half an hour no longer gives a person
// the news in time to plan the day's work. A team that wants quiet beyond
// that routes the check to info.
const (
	warnWaitsForTheAgentMaximumMinutes = 30
	warnWaitsForTheAgentDefaultMinutes = 15
)

// warnWaitsForTheAgent is the applied wait of a warn for the triage agent.
func warnWaitsForTheAgent(ctx context.Context, tx *sql.Tx) (time.Duration, error) {
	minutes := warnWaitsForTheAgentDefaultMinutes
	_, err := appliedSettingsValue(ctx, tx, settingsWarnWaitsForTheAgentPart.name, &minutes)
	return time.Duration(minutes) * time.Minute, err
}

// holdWarnForTheAgent saves the hold for a warn that has just been admitted
// and says whether the warn is held, in which case the caller queues nothing.
// A warn is held when the wait is above 0, the triage agent is set up, no
// person has taken the open session over (the agent works on nothing more
// in it), and the alert is one that starts or joins a session
// (triageSessionAlertCondition) from an alert source: not a notice from
// MMHotline, not one made by hand (a person who raises an alert wants people
// now), not one meant for chosen people only, and not one the triage
// agent's run limit passed over.
func holdWarnForTheAgent(ctx context.Context, tx *sql.Tx, e Alert, onlyForChosenPeople bool, at time.Time) (bool, error) {
	if e.Severity != "warn" || e.SourceID == "system" || e.SourceID == "manual" || onlyForChosenPeople || e.PassedOverForTheRunLimit {
		return false, nil
	}
	wait, err := warnWaitsForTheAgent(ctx, tx)
	if err != nil || wait <= 0 {
		return false, err
	}
	var holds bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup) AND NOT EXISTS(SELECT 1 FROM triage_sessions WHERE ended_at=0 AND taken_over_by_member_id!='')`).Scan(&holds); err != nil || !holds {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO held_warn_alerts(alert_id,held_until) VALUES(?,?)`, e.ID, at.Add(wait).Unix())
	return err == nil, err
}

// heldWarnTakenOverByAPersonReason is why a held warn is released when a
// person took its session over before the background loop read it.
const heldWarnTakenOverByAPersonReason = "A person took the triage session over from the triage agent."

// heldWarnJoinsSession records the session a held warn joined, as the
// background loop reads the alert into it. While an SOS of the session waits
// for the agent, the warn waits at least as long as that SOS. A warn whose
// session a person has taken over is released at once, and one whose own
// alert already resolved ends. A warn that was not held has no row, and
// nothing happens.
func heldWarnJoinsSession(ctx context.Context, tx *sql.Tx, alertID, sessionID string, at time.Time) error {
	var heldUntil int64
	err := tx.QueryRowContext(ctx, `SELECT held_until FROM held_warn_alerts WHERE alert_id=? AND ended_at=0 AND release_requested_at=0`, alertID).Scan(&heldUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var sosIncidentID string
	var sosDeadline int64
	err = tx.QueryRowContext(ctx, `SELECT i.id,i.deadline FROM incidents i WHERE i.status='open' AND i.step=? AND EXISTS(SELECT 1 FROM alerts e JOIN triage_session_alerts s ON s.alert_id=e.id WHERE e.incident_id=i.id AND s.triage_session_id=?) ORDER BY i.deadline DESC,i.id LIMIT 1`, triageAgentStep, sessionID).Scan(&sosIncidentID, &sosDeadline)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// A warn past its deadline is due, and waits no longer (heldWarnStillWaits).
	if heldUntil > at.Unix() {
		heldUntil = max(heldUntil, sosDeadline)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE held_warn_alerts SET triage_session_id=?,held_with_sos_incident_id=?,held_until=? WHERE alert_id=?`, sessionID, sosIncidentID, heldUntil, alertID); err != nil {
		return err
	}
	var takenOver bool
	if err = tx.QueryRowContext(ctx, `SELECT taken_over_by_member_id!='' FROM triage_sessions WHERE id=?`, sessionID).Scan(&takenOver); err != nil {
		return err
	}
	if takenOver {
		return requestHeldWarnRelease(ctx, tx, heldWarnTakenOverByAPersonReason, at, "alert_id=?", alertID)
	}
	// Its alert may have resolved before the loop read it.
	var resolved bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alerts e JOIN alerts r ON r.source_id=e.source_id AND r.sender_incident_key=e.sender_incident_key WHERE e.id=? AND e.sender_incident_key!='' AND r.severity='resolved-warn' AND r.sequence>e.sequence)`, alertID).Scan(&resolved); err != nil || !resolved {
		return err
	}
	return endHeldWarnsBecauseResolved(ctx, tx, at, "alert_id=?", alertID)
}

// warnResolvedWhileHeldReason goes with the alert's page.
const warnResolvedWhileHeldReason = "The warning's alert resolved while MMHotline held it for the triage agent."

// heldWarnAlertResolved ends the holds of the warns that the resolved-warn
// alert e resolves: those of its alert source and sender_incident_key.
// Nobody needed to hear them, as with a held SOS that resolves. A warn the
// background loop has not read yet ends here too; one already released for
// delivery is delivered.
func heldWarnAlertResolved(ctx context.Context, tx *sql.Tx, e Alert, at time.Time) error {
	if e.Severity != "resolved-warn" || e.SenderIncidentKey == "" {
		return nil
	}
	return endHeldWarnsBecauseResolved(ctx, tx, at, "alert_id IN (SELECT id FROM alerts WHERE source_id=? AND sender_incident_key=? AND severity='warn')", e.SourceID, e.SenderIncidentKey)
}

// endHeldWarnsBecauseResolved ends, sending nothing, the holds that where
// picks and that still wait (heldWarnStillWaits); one released for
// delivery, or past its deadline, is delivered.
func endHeldWarnsBecauseResolved(ctx context.Context, tx *sql.Tx, at time.Time, where string, args ...any) error {
	_, err := tx.ExecContext(ctx, `UPDATE held_warn_alerts SET ended_at=?,outcome='resolved',outcome_reason=? WHERE `+heldWarnStillWaits+` AND `+where, append([]any{at.Unix(), warnResolvedWhileHeldReason, at.Unix()}, args...)...)
	return err
}

// heldWarnStillWaits picks the holds that still wait for the triage agent at
// a time, the query's argument: not ended, not marked for delivery, and not
// past their deadline. A hold past its deadline is due though the
// background step has not marked it yet, so a resolve, a take-over, or a
// longer wait must leave it to that step, which delivers it: when MMHotline
// cannot tell, it delivers.
const heldWarnStillWaits = `ended_at=0 AND release_requested_at=0 AND held_until>?`

// requestHeldWarnRelease marks the open holds that where picks for delivery,
// with the reason the alert's page shows; DeliverReleasedHeldAlerts queues
// them. A hold already marked keeps its first reason.
func requestHeldWarnRelease(ctx context.Context, tx *sql.Tx, because string, at time.Time, where string, args ...any) error {
	_, err := tx.ExecContext(ctx, `UPDATE held_warn_alerts SET release_requested_at=?,release_reason=? WHERE ended_at=0 AND release_requested_at=0 AND `+where, append([]any{at.Unix(), because}, args...)...)
	return err
}

// releaseEveryHeldWarn marks every open hold for delivery: the person on
// call is woken, a person took the session over, or the agent cannot run,
// so people are told everything now. One session is open at a time; a hold
// of an ended session or one the loop has not read yet is released too.
func releaseEveryHeldWarn(ctx context.Context, tx *sql.Tx, because string, at time.Time) error {
	return requestHeldWarnRelease(ctx, tx, because, at, "1")
}

// heldWarnDeadlineReason goes with the alert's page of a warn delivered at
// the end of its wait.
const heldWarnDeadlineReason = "The wait ended before a report or playbook run request of the triage agent named the warning."

// releasedAlertsPerVisit bounds how many released alerts one background
// visit queues, each in its own transaction, so a take-over of a session
// with thousands of held warns does not hold up the visit's other work. A
// visit that queued some changes the database, which brings the next visit
// at once.
const releasedAlertsPerVisit = 100

// DeliverReleasedHeldAlerts is the background step that queues what the
// triage agent's holds released: each held warn marked for delivery or whose
// wait ended, and the subscribed notification of each SOS whose wait for the
// agent is over (queueSOSNotificationAfterItsWait). It runs outside the
// transactions that decide a release, and queues each alert in its own
// transaction. When the delivery queue is full, the alert stays marked and
// the step tries again at a later visit, which the deliveries that free room
// bring; nothing else rolls back.
func (a *App) DeliverReleasedHeldAlerts(ctx context.Context) error {
	now := a.Now().UTC()
	var warns, incidents []string
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := requestHeldWarnRelease(ctx, tx, heldWarnDeadlineReason, now, "held_until<=?", now.Unix()); err != nil {
			return err
		}
		var err error
		if warns, err = queryStrings(ctx, tx, `SELECT alert_id FROM held_warn_alerts WHERE ended_at=0 AND release_requested_at>0 ORDER BY release_requested_at,alert_id LIMIT ?`, releasedAlertsPerVisit); err != nil {
			return err
		}
		incidents, err = queryStrings(ctx, tx, `SELECT id FROM incidents WHERE `+sosNotificationWaitIsOver+` ORDER BY created_at,id LIMIT ?`, releasedAlertsPerVisit)
		return err
	}); err != nil {
		return err
	}
	for _, alertID := range warns {
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { return deliverHeldWarn(ctx, tx, alertID, now) })
		if errors.Is(err, errPendingDeliveryFull) {
			break
		}
		if err != nil {
			return err
		}
	}
	for _, incidentID := range incidents {
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { return queueSOSNotificationAfterItsWait(ctx, tx, incidentID, now) })
		if errors.Is(err, errPendingDeliveryFull) {
			break
		}
		// The alert source's share of SOS deliveries is full: another
		// source's SOS may still fit.
		if err != nil && !errors.Is(err, errAlertSourcePendingSOSFull) {
			return err
		}
	}
	return nil
}

// queryStrings reads the first column of each row.
func queryStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var values []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			rows.Close()
			return nil, err
		}
		values = append(values, value)
	}
	return values, errors.Join(rows.Err(), rows.Close())
}

// deliverHeldWarn ends the hold of a warn marked for delivery by queueing
// its notification, once: it does nothing when the hold has ended or is not
// marked. The reason of the mark goes with the alert's page.
func deliverHeldWarn(ctx context.Context, tx *sql.Tx, alertID string, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE held_warn_alerts SET ended_at=?,outcome='delivered',outcome_reason=release_reason WHERE alert_id=? AND ended_at=0 AND release_requested_at>0`, at.Unix(), alertID)
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

// heldWarnTakenOver ends, sending nothing, the holds of the warns that the
// triage agent's report or playbook run request named: the item of itemKind
// and itemID of run runID (triage_agent_covered_alerts), when that run
// carried the warn. A warn already released for delivery, or past its
// deadline, stays to be delivered (heldWarnStillWaits). by
// is "report" or "playbook_run_request"; link goes to the report or to the
// session whose agent asked for the playbook run. A playbook run request
// takes warns over only in the transaction that makes a new approval
// request or a new warned run, because only then does MMHotline tell a
// person now.
func heldWarnTakenOver(ctx context.Context, tx *sql.Tx, runID, itemKind, itemID, by, link string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE held_warn_alerts SET ended_at=?,outcome='taken_over',taken_over_by=?,taken_over_link=? WHERE `+heldWarnStillWaits+` AND alert_id IN (SELECT c.alert_id FROM triage_agent_covered_alerts c JOIN triage_session_alerts s ON s.alert_id=c.alert_id AND s.sent_in_run_id=c.run_id WHERE c.run_id=? AND c.item_kind=? AND c.item_id=?)`, at.Unix(), by, link, at.Unix(), runID, itemKind, itemID)
	return err
}

// settleHeldWarns marks for delivery each held warn the triage agent can no
// longer take over: every one when the agent cannot run (cannotRun says
// why, "" when it can); and otherwise one no session took, one whose session
// ended or a person took over, and one whose carrying run failed or ended
// without naming it. A warn no run has carried yet waits for its run or its
// deadline, which DeliverReleasedHeldAlerts keeps. It runs in the background
// loop's turn for triage sessions, after the alerts that arrived since the
// last visit were read, and only marks: the delivery is a step of its own.
func settleHeldWarns(ctx context.Context, tx *sql.Tx, cannotRun string, now time.Time) error {
	if cannotRun != "" {
		return releaseEveryHeldWarn(ctx, tx, cannotRun, now)
	}
	rows, err := tx.QueryContext(ctx, `SELECT h.alert_id,h.triage_session_id,coalesce(s.ended_at,0),coalesce(s.ended_because,''),coalesce(s.taken_over_by_member_id,'')!='',coalesce(sa.sent_in_run_id,'') FROM held_warn_alerts h LEFT JOIN triage_sessions s ON s.id=h.triage_session_id LEFT JOIN triage_session_alerts sa ON sa.alert_id=h.alert_id WHERE h.ended_at=0 AND h.release_requested_at=0 ORDER BY h.held_until,h.alert_id`)
	if err != nil {
		return err
	}
	type held struct {
		alertID, sessionID, sessionEndedBecause, carriedByRunID string
		sessionEndedAt                                          int64
		takenOver                                               bool
	}
	var open []held
	for rows.Next() {
		var h held
		if err = rows.Scan(&h.alertID, &h.sessionID, &h.sessionEndedAt, &h.sessionEndedBecause, &h.takenOver, &h.carriedByRunID); err != nil {
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
		case h.sessionID == "":
			because = "No triage session took the warning."
		case h.sessionEndedAt != 0:
			because = "The triage session ended: " + h.sessionEndedBecause
		case h.takenOver:
			because = heldWarnTakenOverByAPersonReason
		case h.carriedByRunID != "":
			if because, err = heldWarnRunEnded(ctx, tx, h.carriedByRunID, h.alertID); err != nil {
				return err
			}
		}
		if because != "" {
			if err = requestHeldWarnRelease(ctx, tx, because, now, "alert_id=?", h.alertID); err != nil {
				return err
			}
		}
	}
	return nil
}

// heldWarnRunEnded says why the run runID, which carried the held warn
// alertID, can no longer take the warn over: it failed, or it ended without
// a report or request that took the warn over, since one that did ended the
// hold already. It returns "" while the run goes on, while a run that
// MMHotline's own stop or restart cut short waits to be resumed (its alerts
// wait again), and while a playbook run request of the run that names the
// warn waits for MMHotline's answer, because its MMX run is still going: the
// answer may yet tell a person.
func heldWarnRunEnded(ctx context.Context, tx *sql.Tx, runID, alertID string) (string, error) {
	var state, reason string
	var cause runEndCause
	err := tx.QueryRowContext(ctx, `SELECT state,failure_reason,end_cause FROM triage_agent_runs WHERE id=?`, runID).Scan(&state, &reason, &cause)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || state == "running" || cause.cutShortByMMHotline() {
		return "", err
	}
	var answering bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_requests q JOIN triage_agent_covered_alerts c ON c.run_id=q.run_id AND c.item_kind=? AND c.item_id=q.request_id WHERE q.run_id=? AND q.kind='playbook_run' AND q.outcome='' AND c.alert_id=?)`, coveredByPlaybookRequest, runID, alertID).Scan(&answering); err != nil || answering {
		return "", err
	}
	if state == "failed" {
		return "The triage agent's run failed: " + reason, nil
	}
	return "The triage agent's run ended, and no report or playbook run request of it named the warning.", nil
}

// nextHeldWarnDeadline is when the earliest held warn's wait ends, for the
// background loop's next visit. A warn already marked for delivery is queued
// at the next visit, which any change to the database brings.
func nextHeldWarnDeadline(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var next sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT min(held_until) FROM held_warn_alerts WHERE ended_at=0 AND release_requested_at=0`).Scan(&next); err != nil || !next.Valid {
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
	// Held is true until the hold ends. Released is true once MMHotline
	// decided to deliver it, with ReleaseReason, while its notification
	// waits to be queued. Otherwise Ended is when it ended, and TakenOver
	// says whether the agent's report or request took the warn over
	// (TakenOverBy, TakenOverLink), or the warn was delivered.
	Held          bool
	Released      bool
	ReleaseReason string
	Ended         time.Time
	TakenOver     bool
	TakenOverBy   string
	TakenOverLink string
	// OutcomeReason says why the warn was delivered, or why it ended
	// without being sent because its alert resolved.
	OutcomeReason string
	// SOSTitle is the title of the SOS the warn waits with, "" when no SOS
	// of its session waited for the agent as the warn joined. Resolved is
	// true when the warn ended because its own alert resolved.
	SOSTitle string
	Resolved bool
}

// JoinedSentence is the plain statement of the hold of a warn that waits
// with an SOS, with the time its wait ends, already written for the page.
func (h HeldWarn) JoinedSentence(until string) string {
	return "MMHotline holds this warning with the SOS the triage agent is handling (" + h.SOSTitle + "); it reaches people if the person on call is woken, or at " + until + ", unless the agent's report names it or it resolves first."
}

// Sentence is the plain statement of the hold, without its times.
func (h HeldWarn) Sentence() string {
	return fmt.Sprintf("MMHotline holds this warning for up to %d %s while the triage agent looks into it; it reaches people then, unless the agent's report or playbook run request names it.", h.Minutes, map[bool]string{true: "minute", false: "minutes"}[h.Minutes == 1])
}

// ReleasedSentence is the plain statement of a warn MMHotline decided to
// deliver, whose notification waits to be queued.
func (h HeldWarn) ReleasedSentence() string {
	return "MMHotline is sending this warning now. " + h.ReleaseReason
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
	rows, err := tx.QueryContext(ctx, `SELECT h.alert_id,e.title,e.received_at,h.held_until,h.ended_at,h.outcome,h.taken_over_by,h.taken_over_link,h.outcome_reason,h.release_requested_at,h.release_reason,coalesce((SELECT o.title FROM incidents i JOIN alerts o ON o.id=i.opening_alert_id WHERE i.id=h.held_with_sos_incident_id),'') FROM held_warn_alerts h JOIN alerts e ON e.id=h.alert_id WHERE `+where+` ORDER BY e.sequence`, args...)
	if err != nil {
		return nil, err
	}
	var warns []HeldWarn
	for rows.Next() {
		var h HeldWarn
		var received, until, ended, released int64
		var outcome string
		if err = rows.Scan(&h.AlertID, &h.Title, &received, &until, &ended, &outcome, &h.TakenOverBy, &h.TakenOverLink, &h.OutcomeReason, &released, &h.ReleaseReason, &h.SOSTitle); err != nil {
			rows.Close()
			return nil, err
		}
		h.Received, h.HeldUntil, h.Ended = time.Unix(received, 0).UTC(), time.Unix(until, 0).UTC(), deadlineTime(ended)
		h.Minutes = int(h.HeldUntil.Sub(h.Received) / time.Minute)
		h.Held, h.Released, h.TakenOver, h.Resolved = ended == 0, ended == 0 && released > 0, outcome == "taken_over", outcome == "resolved"
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
