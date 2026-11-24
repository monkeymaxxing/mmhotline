package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The triage agent's run limit: at most triage_agent.most_runs_in_24_hours
// runs that MMHotline starts by itself in a rolling 24 hours:
// docs/changing-mmhotline/design/triage-agent.md#the-run-limit.
//
// The limit stops only agent runs, never alerts. Past it, MMHotline sends
// alerts as it would without a triage agent: no hold and no wait.
//
// One source of truth says whether the limit holds:
// triage_agent_setup.run_limit_notice, with run_limit_notice_changed_at.
// It is none, heads_up_sent (the 80% heads-up went out), or limit_holds.
// DriveTriageAgentRunLimit moves it at each background visit, and sends
// each message in the transaction that moves it, so each goes once; a full
// delivery queue does not refuse these messages (runLimitActor):
//
//	none or heads_up_sent  -> limit_holds    the count reached the limit
//	none                   -> heads_up_sent  the count reached 80% (limits of 5 and more)
//	limit_holds            -> none or heads_up_sent, with "can run again":
//	                          the count fell to 80%, the limit was raised,
//	                          or the setting is no_limit
//	heads_up_sent          -> none           the count is below 80% and the
//	                          heads-up is 24 hours old, so one heads-up goes
//	                          out at most once in 24 hours
//
// A new counted run is admitted only in the transaction that records it, so
// two starts can never both take the last place (admitRunUnderTheRunLimit).
// An alert that the limit passed over is marked on alerts itself
// (passed_over_for_the_run_limit): it is not held, an SOS does not wait,
// it never joins a session, and its notifications start with
// runLimitPassedOverLine. The stored alert is not changed, so a sender's
// retry still matches it.

// mostRunsIn24HoursDefault is the limit when the settings file leaves it
// out. monkeymaxxing.com's busiest day had 7 runs and the lab's worst
// recorded incident 13 runs in 3 hours, while a check that flaps every
// minute with a new key each time starts a run every 1-2 minutes, about
// 700-1,400 a day. Fifty stops that within hours and still covers a long
// real incident. A team that needs more raises it or sets no_limit.
const mostRunsIn24HoursDefault MostRunsIn24Hours = 50

// runLimitWindow is the rolling window the limit counts runs in.
const runLimitWindow = 24 * time.Hour

// runLimitHeadsUpSmallestLimit: with a limit below 5, 80% of it is too close
// to the limit to be worth a message of its own, so no heads-up goes out.
const runLimitHeadsUpSmallestLimit = 5

// runLimitNotice values: triage_agent_setup.run_limit_notice.
const (
	runLimitNoticeNone        = "none"
	runLimitNoticeHeadsUpSent = "heads_up_sent"
	runLimitNoticeLimitHolds  = "limit_holds"
)

// The run limit's columns, in their tables and in the upgrade step that
// adds them (addRunLimit).
const (
	runLimitNoticeColumn                        = `run_limit_notice TEXT NOT NULL DEFAULT 'none' CHECK(run_limit_notice IN ('none','heads_up_sent','limit_holds'))`
	runLimitNoticeChangedAtColumn               = `run_limit_notice_changed_at INTEGER NOT NULL DEFAULT 0`
	alertPassedOverForTheRunLimitColumn         = `passed_over_for_the_run_limit INTEGER NOT NULL DEFAULT 0`
	softwareReviewSkippedForTheRunLimitAtColumn = `skipped_for_the_run_limit_at INTEGER NOT NULL DEFAULT 0`
	triageAgentRunsStartedAtIndex               = `CREATE INDEX triage_agent_runs_started_at ON triage_agent_runs(started_at);`
)

// addRunLimit is database format 6.
func addRunLimit(tx *sql.Tx) error {
	for _, statement := range []string{
		`ALTER TABLE triage_agent_setup ADD COLUMN ` + runLimitNoticeColumn,
		`ALTER TABLE triage_agent_setup ADD COLUMN ` + runLimitNoticeChangedAtColumn,
		`ALTER TABLE alerts ADD COLUMN ` + alertPassedOverForTheRunLimitColumn,
		`ALTER TABLE software_review_interval ADD COLUMN ` + softwareReviewSkippedForTheRunLimitAtColumn,
		triageAgentRunsStartedAtIndex,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// runCountsTowardTheRunLimit picks, in triage_agent_runs r, the runs that
// MMHotline starts by itself: a triage session's runs except a person's
// hand-over, the scheduled software reviews, and the commit reviews. A
// setup test, a sign-in check, a person's software review, and the review
// agent's playbook reviews are not counted; each playbook review follows a
// counted run, and refusing one would only send the agent's fix to a
// person. Nor is a run that MMHotline's own stop or restart cut short, or a
// review stopped to make way for a triage session: those did not do the
// work they were started for, and a restart must not use up the limit.
const runCountsTowardTheRunLimit = `(r.purpose='commit_review' OR (r.purpose='software_review' AND r.started_by='MMHotline') OR (r.purpose='triage_session' AND NOT EXISTS(SELECT 1 FROM triage_sessions hs WHERE hs.hand_over_run_id=r.id))) AND r.end_cause NOT IN ('mmhotline_stopped','mmhotline_restarted','stopped_for_a_triage_session')`

// MostRunsIn24Hours is the settings file's
// triage_agent.most_runs_in_24_hours: a whole number of runs, 1 or more, or
// no_limit, which inside MMHotline is 0. There is no upper bound: no_limit
// already covers "as many as it takes".
type MostRunsIn24Hours int

// noRunLimit is no_limit: MMHotline counts runs for the numbers, and never
// stops one.
const noRunLimit MostRunsIn24Hours = 0

var wholeNumberPattern = regexp.MustCompile(`^[0-9]+$`)

// mostRunsIn24HoursRefusal is the settings check's words for any other
// value, 0 among them: a team that wants no agent leaves out triage_agent.
const mostRunsIn24HoursRefusal = "must be a whole number of runs, 1 or more, or no_limit"

// parseMostRunsIn24Hours reads the setting; left out, it is the default.
func parseMostRunsIn24Hours(value string) (MostRunsIn24Hours, error) {
	switch value {
	case "":
		return mostRunsIn24HoursDefault, nil
	case "no_limit":
		return noRunLimit, nil
	}
	runs, err := strconv.Atoi(value)
	if !wholeNumberPattern.MatchString(value) || err != nil || runs < 1 {
		return 0, errors.New(mostRunsIn24HoursRefusal)
	}
	return MostRunsIn24Hours(runs), nil
}

// Text names the limit for people: "50 runs in 24 hours" or "no limit".
func (limit MostRunsIn24Hours) Text() string {
	if limit == noRunLimit {
		return "no limit"
	}
	return countText(int(limit), "run", "runs") + " in 24 hours"
}

// MarshalJSON gives "no_limit" or the number, as the settings file does.
func (limit MostRunsIn24Hours) MarshalJSON() ([]byte, error) {
	if limit == noRunLimit {
		return []byte(`"no_limit"`), nil
	}
	return strconv.AppendInt(nil, int64(limit), 10), nil
}

// UnmarshalJSON reads what MarshalJSON gives.
func (limit *MostRunsIn24Hours) UnmarshalJSON(data []byte) error {
	parsed, err := parseMostRunsIn24Hours(strings.Trim(string(data), `"`))
	if err != nil {
		return fmt.Errorf("most_runs_in_24_hours %s", err)
	}
	*limit = parsed
	return nil
}

// liftPoint is the count at which a limit that holds ends, and the count of
// the 80% heads-up: 80% of the limit, rounded down, and at most one less
// than the limit, so a limit of 1 holds until no counted run is left.
// Without this margin the agent would switch on and off with every run that
// leaves the window, and the messages with it.
func (limit MostRunsIn24Hours) liftPoint() int {
	return min(int(limit)*4/5, int(limit)-1)
}

// mostRunsIn24HoursKey is the setting's key path.
const mostRunsIn24HoursKey = "triage_agent.most_runs_in_24_hours"

// settingsMostRunsIn24HoursPart is the run limit. The background loop and
// each run's start read the applied value; raising it ends a limit that
// holds at once.
var settingsMostRunsIn24HoursPart = settingsFilePart{
	name:        mostRunsIn24HoursKey,
	value:       triageAgentValue(func(agent SettingsTriageAgent) any { return agent.MostRunsIn24Hours }),
	apply:       applyMostRunsIn24Hours,
	keepsValues: true,
}

// readMostRunsIn24Hours is the applied limit, the default before any.
func readMostRunsIn24Hours(ctx context.Context, tx rowQuerier) (MostRunsIn24Hours, error) {
	var value string
	if _, err := appliedSettingsValue(ctx, tx, mostRunsIn24HoursKey, &value); err != nil {
		return 0, err
	}
	// The settings check refused anything else before it was applied.
	limit, err := parseMostRunsIn24Hours(value)
	if err != nil {
		return mostRunsIn24HoursDefault, nil
	}
	return limit, nil
}

// applyMostRunsIn24Hours ends a limit that holds when the new value raises
// it above the count, or is no_limit: a person who raises the limit wants
// the agent back now, not when the count falls to 80% of the new limit.
// The new value is recorded in the same transaction, which the settings
// apply then records again with the part: a background visit between the
// two would otherwise read the old limit and find it reached again.
func applyMostRunsIn24Hours(ctx context.Context, a *App, s Settings) error {
	if s.TriageAgent == nil {
		return nil
	}
	limit, err := parseMostRunsIn24Hours(s.TriageAgent.MostRunsIn24Hours)
	if err != nil {
		return fmt.Errorf("%w: most_runs_in_24_hours %s", ErrInvalid, err)
	}
	value, err := json.Marshal(s.TriageAgent.MostRunsIn24Hours)
	if err != nil {
		return err
	}
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		earlier, err := readMostRunsIn24Hours(ctx, tx)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings_file_parts(part,applied_values) VALUES(?,?) ON CONFLICT(part) DO UPDATE SET applied_values=excluded.applied_values`, mostRunsIn24HoursKey, string(value)); err != nil {
			return err
		}
		if limit != noRunLimit && (earlier == noRunLimit || limit <= earlier) {
			return nil
		}
		state, err := readRunLimitState(ctx, tx, now, "")
		if err != nil || state.notice != runLimitNoticeLimitHolds {
			return err
		}
		state.limit = limit
		if limit != noRunLimit && state.counted() >= int(limit) {
			return nil
		}
		return a.endRunLimit(ctx, tx, state, now)
	})
}

// runLimitState is the limit in force, its notice, and the counted runs.
type runLimitState struct {
	setUp           bool
	limit           MostRunsIn24Hours
	notice          string
	noticeChangedAt time.Time
	// starts are the start times of the counted runs in the window, oldest
	// first.
	starts []time.Time
}

func (state runLimitState) counted() int { return len(state.starts) }

// lifted says a limit that holds ends now.
func (state runLimitState) lifted() bool {
	return state.limit == noRunLimit || state.counted() <= state.limit.liftPoint()
}

// headsUpDue says the count reached 80% of a limit of 5 or more.
func (state runLimitState) headsUpDue() bool {
	return state.limit >= runLimitHeadsUpSmallestLimit && state.counted() >= state.limit.liftPoint()
}

// agentRunsAgainAt is when the count falls to 80% of the limit in force,
// from the counted runs' start times: when enough of the oldest have left
// the window. It is zero when the count is there already.
func (state runLimitState) agentRunsAgainAt() time.Time {
	leave := state.counted() - state.limit.liftPoint()
	if state.limit == noRunLimit || leave <= 0 {
		return time.Time{}
	}
	return state.starts[leave-1].Add(runLimitWindow)
}

// readRunLimitState reads the notice and the counted runs of the last 24
// hours, leaving out the run excluding, the one being admitted.
func readRunLimitState(ctx context.Context, tx *sql.Tx, now time.Time, excluding string) (runLimitState, error) {
	var state runLimitState
	var changedAt int64
	err := tx.QueryRowContext(ctx, `SELECT run_limit_notice,run_limit_notice_changed_at FROM triage_agent_setup WHERE id=1`).Scan(&state.notice, &changedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.setUp, state.noticeChangedAt = true, time.Unix(changedAt, 0).UTC()
	if state.limit, err = readMostRunsIn24Hours(ctx, tx); err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.started_at FROM triage_agent_runs r WHERE r.started_at>? AND r.id!=? AND `+runCountsTowardTheRunLimit+` ORDER BY r.started_at`, now.Add(-runLimitWindow).Unix(), excluding)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var started int64
		if err = rows.Scan(&started); err != nil {
			rows.Close()
			return state, err
		}
		state.starts = append(state.starts, time.Unix(started, 0).UTC())
	}
	return state, errors.Join(rows.Err(), rows.Close())
}

// errRunLimitReached refuses a counted run while the limit holds or the
// count is at the limit. The run is not recorded; the background loop's
// next visit acts on the limit.
var errRunLimitReached = fmt.Errorf("%w: the triage agent reached its run limit (triage_agent.most_runs_in_24_hours)", ErrConflict)

// admitRunUnderTheRunLimit runs in the transaction that records the run
// runID, after the caller's own checks, and refuses it when it counts and
// the limit holds or the count of the others is at the limit. The database
// takes one writer at a time, so two starts never both take the last place.
func admitRunUnderTheRunLimit(ctx context.Context, tx *sql.Tx, runID string, now time.Time) error {
	var counted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_runs r WHERE r.id=? AND `+runCountsTowardTheRunLimit+`)`, runID).Scan(&counted); err != nil || !counted {
		return err
	}
	state, err := readRunLimitState(ctx, tx, now, runID)
	if err != nil || !state.setUp || state.limit == noRunLimit {
		return err
	}
	if state.notice == runLimitNoticeLimitHolds || state.counted() >= int(state.limit) {
		return errRunLimitReached
	}
	return nil
}

// runLimitHolds says the limit holds: the one source of truth for
// whether new alerts go straight to people.
func runLimitHolds(ctx context.Context, tx *sql.Tx) (bool, error) {
	var holds bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup WHERE id=1 AND run_limit_notice=?)`, runLimitNoticeLimitHolds).Scan(&holds)
	return holds, err
}

// setRunLimitNotice records the notice and when it changed.
func setRunLimitNotice(ctx context.Context, tx *sql.Tx, notice string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE triage_agent_setup SET run_limit_notice=?,run_limit_notice_changed_at=? WHERE id=1`, notice, now.Unix())
	return err
}

// DriveTriageAgentRunLimit is the background loop's turn for the run limit,
// before the session's turn: it moves the notice, sends its messages, and,
// while the limit holds, sends straight to people what waits for the agent's
// next run (passOverWaitingWorkForTheRunLimit). That last step runs at every
// visit while the limit holds, so an alert the session's turn took in the
// moment before the limit was reached goes out at the next visit.
func (a *App) DriveTriageAgentRunLimit(ctx context.Context) error {
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		state, err := readRunLimitState(ctx, tx, now, "")
		if err != nil || !state.setUp {
			return err
		}
		switch {
		case state.notice == runLimitNoticeLimitHolds && state.lifted():
			return a.endRunLimit(ctx, tx, state, now)
		case state.notice != runLimitNoticeLimitHolds && state.limit != noRunLimit && state.counted() >= int(state.limit):
			if err = a.runLimitReached(ctx, tx, state, now); err != nil {
				return err
			}
			state.notice = runLimitNoticeLimitHolds
		case state.notice == runLimitNoticeNone && state.headsUpDue():
			return a.sendRunLimitHeadsUp(ctx, tx, state, now)
		case state.notice == runLimitNoticeHeadsUpSent && (state.limit == noRunLimit || state.counted() < state.limit.liftPoint() && now.Sub(state.noticeChangedAt) >= runLimitWindow):
			return setRunLimitNotice(ctx, tx, runLimitNoticeNone, now)
		}
		if state.notice != runLimitNoticeLimitHolds {
			return nil
		}
		return a.passOverWaitingWorkForTheRunLimit(ctx, tx, now)
	})
}

// nextRunLimitDeadline is when a limit that holds ends by the count, for
// the background loop's next visit.
func nextRunLimitDeadline(ctx context.Context, tx *sql.Tx, now time.Time) (time.Time, error) {
	state, err := readRunLimitState(ctx, tx, now, "")
	if err != nil || state.notice != runLimitNoticeLimitHolds {
		return time.Time{}, err
	}
	return state.agentRunsAgainAt(), nil
}

// The actors of MMHotline's own messages at the limit. The weekly numbers
// count the limit's warnings by runLimitActor, so a playbook run's result
// sent to a person has an actor of its own. Neither waits for room in the
// delivery queue (checkPendingDeliveryLimits): each goes in the transaction
// that moves the notice or marks the result, which also marks the alerts
// and wakes the SOS that the limit sends straight to people, and a full
// queue must not roll those back. They stay few: a warning, a heads-up, or
// "can run again" each needs counted runs to move the notice, and each
// result needs a playbook run that an agent's run asked for.
const (
	runLimitActor                  = "triage_agent_run_limit"
	runLimitPlaybookRunResultActor = "triage_agent_run_limit_playbook_run_result"
)

// The words of the messages about the limit: the promise comes first in
// every one.
const (
	runLimitPromise = "The limit stops only agent runs, never alerts."
	// runLimitPassedOverLine starts every notification of an alert the
	// limit passed over, before the alert's own message, so a phone shows
	// the title first and this line next.
	runLimitPassedOverLine = "The triage agent did not look at this alert and will not come back to it: its run limit was reached. Handle it yourself."
	// runLimitSOSHistoryLine goes in an SOS's history when the limit woke
	// the person on call without the agent's wait.
	runLimitSOSHistoryLine = "MMHotline woke the person on call without the agent's wait: the triage agent reached its run limit."
	// runLimitSOSWokeKind is that history entry's kind.
	runLimitSOSWokeKind = "woken_without_the_agents_wait_for_the_run_limit"
	// runLimitReleaseReason goes with the alert's page of a held warning
	// the limit released.
	runLimitReleaseReason = "The triage agent reached its run limit, so MMHotline sends the warning without waiting for the agent."
)

// sendRunLimitHeadsUp sends the 80% heads-up, once, to the people an
// ordinary info alert reaches.
func (a *App) sendRunLimitHeadsUp(ctx context.Context, tx *sql.Tx, state runLimitState, now time.Time) error {
	uses, err := runLimitUseLines(ctx, tx, now)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("Agent runs: %d of %d used in 24 hours. Alerts continue.", state.counted(), int(state.limit))
	message := fmt.Sprintf("%s At %d runs, MMHotline stops sending new alerts to the triage agent until the count falls back to %d.\nMost runs went to:\n%s\nNothing is needed from you now.",
		runLimitPromise, int(state.limit), state.limit.liftPoint(), strings.Join(uses, "\n"))
	if err = RecordSystemAlert(ctx, tx, runLimitActor, "", "info", title, message, now); err != nil {
		return err
	}
	return setRunLimitNotice(ctx, tx, runLimitNoticeHeadsUpSent, now)
}

// runLimitReached records that the limit holds and sends MMHotline's own
// warn, which is never held, to the people an ordinary warn reaches.
func (a *App) runLimitReached(ctx context.Context, tx *sql.Tx, state runLimitState, now time.Time) error {
	if err := setRunLimitNotice(ctx, tx, runLimitNoticeLimitHolds, now); err != nil {
		return err
	}
	uses, err := runLimitUseLines(ctx, tx, now)
	if err != nil {
		return err
	}
	tokens, err := runLimitTokenLines(ctx, tx, now)
	if err != nil {
		return err
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_runs WHERE state='running' AND purpose IN ('triage_session','software_review','commit_review'))`).Scan(&active); err != nil {
		return err
	}
	first := runLimitPromise + " New alerts now go straight to people: warnings are not held, and an SOS wakes the person on call without the agent's wait."
	if active {
		first += " The current run finishes its work."
	}
	first += " The agent can look at new alerts again at " + state.agentRunsAgainAt().Format("15:04") + " UTC."
	paragraphs := []string{first, "What used the runs in the last 24 hours:\n" + strings.Join(uses, "\n")}
	if len(tokens) > 0 {
		paragraphs = append(paragraphs, strings.Join(tokens, "\n"))
	}
	paragraphs = append(paragraphs, fmt.Sprintf("An administrator can change the limit: tell your coding agent \"Set triage_agent.most_runs_in_24_hours in MMHotline's settings file to %d, or to no_limit, and apply MMHotline's settings.\" The default is %d so a check that fails over and over cannot run the agent hundreds of times a day.\nDashboard: %s/",
		2*int(state.limit), int(mostRunsIn24HoursDefault), strings.TrimRight(a.WebURL, "/")))
	return RecordSystemAlert(ctx, tx, runLimitActor, "", "warn", "Agent run limit reached. Alerts continue.", strings.Join(paragraphs, "\n\n"), now)
}

// endRunLimit ends a limit that holds and says so, once, as the agent runs
// again. The heads-up is armed again; when the count is still at 80%, the
// "can run again" message has just said so, so the notice records the
// heads-up as sent.
func (a *App) endRunLimit(ctx context.Context, tx *sql.Tx, state runLimitState, now time.Time) error {
	notice := runLimitNoticeNone
	if state.headsUpDue() {
		notice = runLimitNoticeHeadsUpSent
	}
	if err := setRunLimitNotice(ctx, tx, notice, now); err != nil {
		return err
	}
	used := fmt.Sprintf("It has used %d of its %d runs in the last 24 hours.", state.counted(), int(state.limit))
	if state.limit == noRunLimit {
		used = fmt.Sprintf("It has used %s in the last 24 hours; triage_agent.most_runs_in_24_hours is now no_limit.", countText(state.counted(), "run", "runs"))
	}
	return RecordSystemAlert(ctx, tx, runLimitActor, "", "info", "The triage agent can run again. Alerts wait for it again.", used+" Alerts that arrived during the limit already reached people and are not sent to the agent.", now)
}

// passOverWaitingWorkForTheRunLimit sends straight to people what waits for
// the agent's next run, which the limit refuses. The active run finishes,
// and the alerts it carried keep their holds and deadlines. Everything else
// waiting goes out at once:
//
//   - each held warn no run carried is released, and its alert marked;
//   - each alert of the session that waits for its first run is marked, so
//     no later run carries it;
//   - each SOS that waits for the agent and is not carried by the active run
//     wakes the person on call, and its alert is marked when no run
//     carried it;
//   - each playbook run's result that waits for the agent goes to a person
//     once, with the alerts of the problem it was for.
func (a *App) passOverWaitingWorkForTheRunLimit(ctx context.Context, tx *sql.Tx, now time.Time) error {
	held, err := queryStrings(ctx, tx, `SELECT h.alert_id FROM held_warn_alerts h LEFT JOIN triage_session_alerts s ON s.alert_id=h.alert_id WHERE h.ended_at=0 AND h.release_requested_at=0 AND coalesce(s.sent_in_run_id,'')=''`)
	if err != nil {
		return err
	}
	for _, alertID := range held {
		if err = markAlertPassedOverForTheRunLimit(ctx, tx, alertID); err != nil {
			return err
		}
		if err = requestHeldWarnRelease(ctx, tx, runLimitReleaseReason, now, "alert_id=?", alertID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE alerts SET passed_over_for_the_run_limit=1 WHERE passed_over_for_the_run_limit=0 AND id IN (SELECT s.alert_id FROM triage_session_alerts s JOIN triage_sessions t ON t.id=s.triage_session_id WHERE s.sent_in_run_id='' AND t.ended_at=0)`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+incidentColumns+`,coalesce(s.sent_in_run_id,'') FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id LEFT JOIN triage_session_alerts s ON s.alert_id=e.id
		WHERE i.status='open' AND i.step=? AND NOT EXISTS(SELECT 1 FROM triage_agent_runs r WHERE r.id=s.sent_in_run_id AND r.state='running') ORDER BY i.created_at,i.id`, triageAgentStep)
	if err != nil {
		return err
	}
	type waiting struct {
		incident  Incident
		carriedBy string
	}
	var sos []waiting
	for rows.Next() {
		var w waiting
		if w.incident, err = scanIncident(rowAndMore{rows, []any{&w.carriedBy}}); err != nil {
			rows.Close()
			return err
		}
		sos = append(sos, w)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, w := range sos {
		// An SOS the agent looked at in an earlier run wakes without the
		// line: the line says the agent never looked.
		if w.carriedBy == "" {
			if err = markAlertPassedOverForTheRunLimit(ctx, tx, w.incident.OpeningAlertID); err != nil {
				return err
			}
		}
		if err = wakeHeldIncident(ctx, tx, w.incident, runLimitSOSWokeKind, runLimitSOSHistoryLine, now); err != nil {
			return err
		}
	}
	return a.sendPlaybookRunResultsToAPersonForTheRunLimit(ctx, tx, now)
}

// markAlertPassedOverForTheRunLimit marks an alert the limit sent straight
// to people. Only the mark changes: the stored alert stays as sent.
func markAlertPassedOverForTheRunLimit(ctx context.Context, tx *sql.Tx, alertID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE alerts SET passed_over_for_the_run_limit=1 WHERE id=?`, alertID)
	return err
}

// passOverAtIntakeForTheRunLimit marks a new alert that would go to the
// triage agent (triageSessionAlertCondition) while the limit holds, in
// the transaction that saves it, before anything decides its hold or its
// wait. It says whether it marked the alert.
func passOverAtIntakeForTheRunLimit(ctx context.Context, tx *sql.Tx, alertID string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE alerts SET passed_over_for_the_run_limit=1 WHERE id=? AND `+triageSessionAlertCondition+` AND EXISTS(SELECT 1 FROM triage_agent_setup WHERE id=1 AND run_limit_notice=?)`, alertID, runLimitNoticeLimitHolds)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

// alertPassedOverForTheRunLimit reads the mark.
func alertPassedOverForTheRunLimit(ctx context.Context, tx *sql.Tx, alertID string) (bool, error) {
	var passedOver bool
	err := tx.QueryRowContext(ctx, `SELECT passed_over_for_the_run_limit FROM alerts WHERE id=?`, alertID).Scan(&passedOver)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return passedOver, err
}

// sendPlaybookRunResultsToAPersonForTheRunLimit sends each playbook run's
// result that waits for the agent's next run to a person, once, with the
// alerts of the problem it was for, such as run 50's fix that failed after
// the run ended: the limit refuses the run that would take it to the agent.
// A result that the open session's hand-over carries never waits here: the
// hand-over is not counted and runs as usual.
func (a *App) sendPlaybookRunResultsToAPersonForTheRunLimit(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person w WHERE result_for_the_agent=? AND NOT EXISTS(SELECT 1 FROM triage_sessions s WHERE s.id=w.triage_session_id AND s.taken_over_by_member_id!='') ORDER BY state_changed_at,id`, resultForTheAgentWaits)
	if err != nil {
		return err
	}
	var waiting []approvalRequest
	for rows.Next() {
		r, err := scanApprovalRequest(rows)
		if err != nil {
			rows.Close()
			return err
		}
		waiting = append(waiting, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	webURL := strings.TrimRight(a.WebURL, "/")
	for _, r := range waiting {
		// The alerts the request named, or else those its run carried.
		problem, err := queryStrings(ctx, tx, `SELECT e.title||char(10)||'  '||?||'/alerts/'||e.id FROM triage_agent_covered_alerts c JOIN alerts e ON e.id=c.alert_id WHERE c.run_id=? AND c.item_kind=? AND c.item_id=? ORDER BY e.sequence`, webURL, r.RunID, coveredByPlaybookRequest, r.RequestID)
		if err == nil && len(problem) == 0 {
			problem, err = queryStrings(ctx, tx, `SELECT e.title||char(10)||'  '||?||'/alerts/'||e.id FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE s.sent_in_run_id=? AND e.actor NOT IN ('triage_agent',?) ORDER BY e.sequence`, webURL, r.RunID, playbookRunApprovalsActor)
		}
		if err != nil {
			return err
		}
		severity := "warn"
		if r.State == approvalRanAndSucceeded {
			severity = "info"
		}
		message := "The triage agent will not look at this result: its run limit was reached. Handle it yourself.\n\n" + r.ResultMessage
		if len(problem) > 0 {
			message += "\n\nThe problem the playbook run was for:\n- " + strings.Join(problem, "\n- ")
		}
		title := truncateRunes("Result of "+playbookInInventoryText(r.Playbook, r.Inventory)+": "+approvalStateLabel(r.State), 256)
		if err = RecordSystemAlert(ctx, tx, runLimitPlaybookRunResultActor, r.IncidentID, severity, title, truncateRunes(message, maximumAlertMessageBytes/4), now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET result_for_the_agent=? WHERE id=?`, resultForTheAgentCouldNotReachTheAgent, r.ID); err != nil {
			return err
		}
		if r.IncidentID != "" {
			incident, err := readIncident(ctx, tx, r.IncidentID)
			if err != nil {
				return err
			}
			if err = recordIncidentAction(ctx, tx, incident, playbookRunApprovalsActor, "playbook_run_result_not_sent_to_triage_agent", "The result of "+playbookInInventoryText(r.Playbook, r.Inventory)+" could not reach the triage agent: the triage agent reached its run limit, so MMHotline sent the result to people. The result remains in the incident history.", "", "", now); err != nil {
				return err
			}
		}
	}
	return nil
}

// runLimitUseLine is one line of "what used the runs".
type runLimitUseLine struct {
	label   string
	runs    int
	problem bool
}

// runLimitUseLines says what used the counted runs of the last 24 hours, as
// lines such as "- 22 runs: db01: ... failed": a triage session's run counts
// toward the problem (alert source and sender_incident_key) of the alerts
// it carried, named by that problem's newest alert's title; a run that
// carried several problems counts once as "several findings at once", since
// counting it under one would blame the wrong alert; a run that carried
// none, such as one that only took a playbook run's result back, counts
// under the problem of its session's first alert. Software reviews and
// commit reviews form one line each. The top three lines are shown, and
// one line for the rest.
func runLimitUseLines(ctx context.Context, tx *sql.Tx, now time.Time) ([]string, error) {
	// The alerts MMHotline adds about a run's own work, a report's or a
	// playbook run request's, join the session as sent by the run but were
	// never shown to it.
	const shown = `e.actor NOT IN ('triage_agent','` + playbookRunApprovalsActor + `')`
	const problemOf = `e.source_id||char(31)||CASE WHEN e.sender_incident_key='' THEN 'alert '||e.id ELSE e.sender_incident_key END`
	rows, err := tx.QueryContext(ctx, `SELECT r.purpose,
		(SELECT count(DISTINCT `+problemOf+`) FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE s.sent_in_run_id=r.id AND `+shown+`),
		coalesce((SELECT e.id FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE s.sent_in_run_id=r.id AND `+shown+` ORDER BY e.sequence LIMIT 1),
		 (SELECT e.id FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE s.triage_session_id=r.triage_session_id AND r.triage_session_id!='' AND `+shown+` ORDER BY e.sequence LIMIT 1), '')
		FROM triage_agent_runs r WHERE r.started_at>? AND `+runCountsTowardTheRunLimit, now.Add(-runLimitWindow).Unix())
	if err != nil {
		return nil, err
	}
	type run struct {
		purpose, alertID string
		problems         int
	}
	var runs []run
	for rows.Next() {
		var r run
		if err = rows.Scan(&r.purpose, &r.problems, &r.alertID); err != nil {
			rows.Close()
			return nil, err
		}
		runs = append(runs, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	counts := map[string]*runLimitUseLine{}
	count := func(key, label string, problem bool) {
		if counts[key] == nil {
			counts[key] = &runLimitUseLine{label: label, problem: problem}
		}
		counts[key].runs++
	}
	names := map[string]string{}
	for _, r := range runs {
		switch {
		case r.purpose == "software_review":
			count("software review", "the scheduled software review", false)
		case r.purpose == "commit_review":
			count("commit review", "the commit reviews", false)
		case r.problems > 1:
			count("several", "several findings at once", false)
		case r.alertID == "":
			count("no alert", "a triage session without alerts", false)
		default:
			var sourceID, key, title string
			if err = tx.QueryRowContext(ctx, `SELECT source_id,sender_incident_key,title FROM alerts WHERE id=?`, r.alertID).Scan(&sourceID, &key, &title); err != nil {
				return nil, err
			}
			problem := sourceID + "\x1f" + key
			if key == "" {
				problem = sourceID + "\x1falert " + r.alertID
			} else if names[problem] == "" {
				// The problem is named by its newest alert's title; a
				// resolved message names no finding.
				if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT title FROM alerts WHERE source_id=? AND sender_incident_key=? AND severity IN ('info','warn','sos') ORDER BY sequence DESC LIMIT 1),?)`, sourceID, key, title).Scan(&title); err != nil {
					return nil, err
				}
			}
			if names[problem] == "" {
				names[problem] = title
			}
			count(problem, names[problem], true)
		}
	}
	lines := make([]runLimitUseLine, 0, len(counts))
	for _, line := range counts {
		lines = append(lines, *line)
	}
	slices.SortFunc(lines, func(x, y runLimitUseLine) int {
		return cmp.Or(cmp.Compare(y.runs, x.runs), cmp.Compare(x.label, y.label))
	})
	if len(lines) > 4 {
		rest := runLimitUseLine{label: "other problems", problem: true}
		for _, line := range lines[3:] {
			rest.runs += line.runs
			if !line.problem {
				rest.label = "others"
			}
		}
		rest.label = fmt.Sprintf("%d %s", len(lines)-3, rest.label)
		lines = append(lines[:3], rest)
	}
	text := make([]string, 0, len(lines))
	for _, line := range lines {
		text = append(text, "- "+countText(line.runs, "run", "runs")+": "+line.label)
	}
	return text, nil
}

// runLimitTokenWindow is the window of the warning's token lines: a week
// smooths one long incident into a typical run.
const runLimitTokenWindow = 7 * 24 * time.Hour

// runLimitTokenLines are the warning's lines about what a finished run in
// the systems repository used in the last 7 days, one per coding agent
// used. A failed or stopped run reports no use of its own, so only finished
// runs count. Dollars are given where the coding agent reports them, as
// Claude Code does; Codex reports tokens only.
func runLimitTokenLines(ctx context.Context, tx *sql.Tx, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT coding_agent,avg(input_tokens),avg(cached_input_tokens),avg(output_tokens),avg(cost_in_us_dollars) FROM triage_agent_runs
		WHERE state='finished' AND started_at>? AND purpose IN ('triage_session','software_review','commit_review') GROUP BY coding_agent ORDER BY coding_agent`, now.Add(-runLimitTokenWindow).Unix())
	if err != nil {
		return nil, err
	}
	var lines []string
	for rows.Next() {
		var codingAgent string
		var input, cached, output float64
		var cost sql.NullFloat64
		if err = rows.Scan(&codingAgent, &input, &cached, &output, &cost); err != nil {
			rows.Close()
			return nil, err
		}
		title := codingAgentTitle(codingAgent)
		line := fmt.Sprintf("In the last 7 days, a finished %s run used on average %s input tokens (%s of them cached) and %s output tokens", title, wholeNumberText(int64(input+0.5)), wholeNumberText(int64(cached+0.5)), wholeNumberText(int64(output+0.5)))
		if cost.Valid {
			line += fmt.Sprintf(" and cost US$%.2f on average.", cost.Float64)
		} else {
			line += ". " + title + " reports tokens, not dollars."
		}
		lines = append(lines, line)
	}
	return lines, errors.Join(rows.Err(), rows.Close())
}

// wholeNumberText writes a count with a comma between each three digits,
// such as 410,000.
func wholeNumberText(n int64) string {
	digits := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + wholeNumberText(-n)
	}
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

// TriageAgentRunLimitView is the last 24 hours against the limit, as the
// triage agent page and show give it.
type TriageAgentRunLimitView struct {
	MostRunsIn24Hours MostRunsIn24Hours `json:"most_runs_in_24_hours"`
	// CountedRunsInTheLast24Hours are the runs that count toward the limit.
	CountedRunsInTheLast24Hours int    `json:"counted_runs_in_the_last_24_hours"`
	RunLimitNotice              string `json:"run_limit_notice"`
	// AgentLooksAtNewAlertsAgainAt is when a limit that holds ends by the
	// count, absent while it does not hold.
	AgentLooksAtNewAlertsAgainAt *time.Time `json:"agent_looks_at_new_alerts_again_at,omitempty"`
}

// LimitHolds says new alerts go straight to people.
func (view TriageAgentRunLimitView) LimitHolds() bool {
	return view.RunLimitNotice == runLimitNoticeLimitHolds
}

// Text is the view in words, as show prints it and the page shows it.
func (view TriageAgentRunLimitView) Text() string {
	text := fmt.Sprintf("Runs that count toward the run limit in the last 24 hours: %d of %d.", view.CountedRunsInTheLast24Hours, int(view.MostRunsIn24Hours))
	if view.MostRunsIn24Hours == noRunLimit {
		text = fmt.Sprintf("Runs that count toward the run limit in the last 24 hours: %d; the run limit is no_limit.", view.CountedRunsInTheLast24Hours)
	}
	if view.LimitHolds() {
		text += " The run limit was reached: new alerts go straight to people."
		if view.AgentLooksAtNewAlertsAgainAt != nil {
			text += " The agent can look at new alerts again at " + view.AgentLooksAtNewAlertsAgainAt.UTC().Format("15:04") + " UTC."
		}
	}
	return text
}

// readTriageAgentRunLimitView reads the view; nil without the setup.
func readTriageAgentRunLimitView(ctx context.Context, tx *sql.Tx, now time.Time) (*TriageAgentRunLimitView, error) {
	state, err := readRunLimitState(ctx, tx, now, "")
	if err != nil || !state.setUp {
		return nil, err
	}
	view := &TriageAgentRunLimitView{MostRunsIn24Hours: state.limit, CountedRunsInTheLast24Hours: state.counted(), RunLimitNotice: state.notice}
	if again := state.agentRunsAgainAt(); view.LimitHolds() && !again.IsZero() {
		view.AgentLooksAtNewAlertsAgainAt = &again
	}
	return view, nil
}
