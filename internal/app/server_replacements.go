package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"mmhotline/internal/triageagent"
)

// An SOS keeps waiting while the triage agent replaces a failed server:
// docs/changing-mmhotline/design/triage-agent.md#an-sos-waits-for-a-server-replacement.
//
// server_replacements holds the replacements of triage sessions: a session
// may have several over time, at most one in progress at a time, because
// each replacement the agent starts is a fresh outage it is fixing. One
// starts when MMHotline begins a run of a listed playbook marked
// starts_a_server_replacement in a session while an SOS waits for the agent
// and none of the session's replacements is in progress
// (startServerReplacement); that run sends the one warn alert, in the same
// transaction as the row, so a restart never sends a second. incident_id is
// the SOS the warn alert is about, the newest that waits. sos_waits_until is started_at plus the
// settings file's triage_agent.sos_waits_for_a_server_replacement_minutes,
// read once at the start; each waiting SOS's own deadline holds it too, so
// a restart keeps it. ended_at is when an early end woke the person on call
// (wakeIncidentHeldForTriageAgent); 0 while the replacement may go on. A
// replacement is in progress while ended_at is 0, sos_waits_until is still
// ahead, and its session is open.
const serverReplacementSchema = `
CREATE TABLE server_replacements (
 id INTEGER PRIMARY KEY,
 triage_session_id TEXT NOT NULL REFERENCES triage_sessions(id),
 incident_id TEXT NOT NULL, playbook TEXT NOT NULL, inventory TEXT NOT NULL,
 started_at INTEGER NOT NULL, sos_waits_until INTEGER NOT NULL,
 ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX server_replacements_by_session ON server_replacements(triage_session_id);
`

// heldForAServerReplacement is the incident history's kind for an SOS whose
// wait a server replacement extended. The incident's step stays
// triageAgentStep; this action says why its deadline is later.
const heldForAServerReplacement = "held_for_a_server_replacement"

// sosWaitsForAServerReplacementMinutesBeforeTheSetting is the extension
// before the settings file has applied one, the recommended start: a
// measured replacement by the agent alone, from creating the new server to
// removing the failed one, took about 5 to 6 minutes, and 10 adds a margin.
// A later measurement may change it.
const sosWaitsForAServerReplacementMinutesBeforeTheSetting = 10

// sosWaitsForAServerReplacementMaximumMinutes: one MMX run may take up to
// an hour (mmxRunTimeLimit), so a longer wait would hold an SOS for a
// replacement that MMHotline itself would already have ended.
const sosWaitsForAServerReplacementMaximumMinutes = 60

// sosWaitsForAServerReplacement is the applied extension; 0 turns it off.
func sosWaitsForAServerReplacement(ctx context.Context, tx *sql.Tx) (time.Duration, error) {
	minutes := sosWaitsForAServerReplacementMinutesBeforeTheSetting
	_, err := appliedSettingsValue(ctx, tx, settingsSOSWaitsForAServerReplacementPart.name, &minutes)
	return time.Duration(minutes) * time.Minute, err
}

// runPlaybookForTheAgent runs a playbook of a triage agent's request with
// the runner, for a request that runs without a person's approval step now
// and for one people approved, or that a warning or an earlier approval let
// run. A run of a marked playbook may start a server replacement first; a
// run that fails or could not run during a replacement ends it and wakes
// the person on call.
func (a *App) runPlaybookForTheAgent(ctx context.Context, request TriageAgentPlaybookRunRequest, commitID string) TriageAgentPlaybookRunDecision {
	ctx = context.WithoutCancel(ctx)
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return startServerReplacement(ctx, tx, request, a.Now().UTC())
	}); err != nil {
		// The run goes on: an SOS that keeps its ordinary wait wakes the
		// person on call sooner, never later.
		fmt.Fprintf(os.Stderr, "mmhotline: could not start the server replacement: %s\n", err)
	}
	decision := a.triageAgent.playbookRunner.RunPlaybook(ctx, request, commitID)
	// The copy moved on: nothing ran, and MMHotline judges the playbook again.
	if decision.CopyMovedToCommitID != "" || decision.Outcome == triageagent.RanAndSucceeded {
		return decision
	}
	cause := playbookInInventoryText(request.Playbook, request.Inventory) + " ran and failed"
	if decision.Outcome != triageagent.RanAndFailed {
		cause = "MMHotline could not run " + playbookInInventoryText(request.Playbook, request.Inventory)
	}
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return serverReplacementFailed(ctx, tx, request.RunID, cause, a.Now().UTC())
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mmhotline: could not end the server replacement: %s\n", err)
	}
	return decision
}

// startServerReplacement starts the request's session's server replacement
// when the playbook is listed with starts_a_server_replacement, an SOS
// waits for the agent, the extension is not 0, and none of the session's
// replacements is in progress. Each waiting SOS then waits until the start
// plus the extension, when that is later than its own deadline, and
// MMHotline sends the one warn alert about the newest of them.
func startServerReplacement(ctx context.Context, tx *sql.Tx, request TriageAgentPlaybookRunRequest, now time.Time) error {
	name, ok := mmxPlaybookName(request.Playbook)
	if !ok || !strings.HasPrefix(request.Playbook, "playbooks/") {
		return nil
	}
	var sessionID string
	var marked bool
	err := tx.QueryRowContext(ctx, `SELECT s.id, EXISTS(SELECT 1 FROM agent_playbook_rules_for_listed_playbooks WHERE inventory=?2 AND playbook_name=?3 AND starts_a_server_replacement=1)
		FROM triage_agent_runs r JOIN triage_sessions s ON s.id=r.triage_session_id
		WHERE r.id=?1 AND s.ended_at=0 AND s.taken_over_by_member_id='' AND NOT EXISTS(SELECT 1 FROM server_replacements WHERE triage_session_id=s.id AND ended_at=0 AND sos_waits_until>?4)`, request.RunID, request.Inventory, name, now.Unix()).Scan(&sessionID, &marked)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !marked {
		return nil
	}
	if err != nil {
		return err
	}
	extension, err := sosWaitsForAServerReplacement(ctx, tx)
	if err != nil || extension == 0 {
		return err
	}
	held, err := incidentsWaitingForTriageAgent(ctx, tx)
	if err != nil || len(held) == 0 {
		return err
	}
	until := now.Add(extension)
	newest := held[len(held)-1]
	if _, err = tx.ExecContext(ctx, `INSERT INTO server_replacements(triage_session_id,incident_id,playbook,inventory,started_at,sos_waits_until) VALUES(?,?,?,?,?,?)`, sessionID, newest.ID, request.Playbook, request.Inventory, now.Unix(), until.Unix()); err != nil {
		return err
	}
	for _, i := range held {
		if err = extendWaitForServerReplacement(ctx, tx, i, request.Playbook, request.Inventory, until, now); err != nil {
			return err
		}
	}
	// A warn held with a waiting SOS waits as long as the SOS does.
	if _, err = tx.ExecContext(ctx, `UPDATE held_warn_alerts SET held_until=max(held_until,?) WHERE ended_at=0 AND held_with_sos_incident_id!='' AND triage_session_id=?`, until.Unix(), sessionID); err != nil {
		return err
	}
	// The person is woken at the later of the two waits: an SOS whose own
	// wait for the agent ends after the replacement's limit keeps it.
	wake := until
	if newest.Deadline.After(wake) {
		wake = newest.Deadline
	}
	title, message := serverReplacementStartWarning(newest.Title, wake)
	_, err = admitAlert(ctx, tx, "system", "system", serverReplacementStartWarnPrefix+NewID(), newest.ID, GenericAlert{Severity: "warn", Title: title, Message: message}, nil, now)
	return err
}

// serverReplacementStartWarnPrefix starts the replay key of a
// replacement's START warn, so the delivery throttle can send it at once
// (startWarnOfAServerReplacement).
const serverReplacementStartWarnPrefix = "server_replacement_start:"

// startWarnOfAServerReplacement reports whether the alert is a replacement's
// START warn.
func startWarnOfAServerReplacement(ctx context.Context, tx *sql.Tx, alertID string) (bool, error) {
	var replayKey string
	err := tx.QueryRowContext(ctx, `SELECT replay_key FROM alerts WHERE id=?`, alertID).Scan(&replayKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return strings.HasPrefix(replayKey, serverReplacementStartWarnPrefix), err
}

// serverReplacementStartWarning is the one warn alert of a replacement:
// what failed, that the agent replaces the server, and when MMHotline wakes
// the person at the latest. It names that time, not the setting, because the
// SOS's own wait for the agent can end later than the replacement's limit.
func serverReplacementStartWarning(sosTitle string, wake time.Time) (title, message string) {
	return truncateRunes("Your site is down: "+sosTitle, 256),
		fmt.Sprintf("The triage agent is replacing the server. You will be woken only if the replacement fails or is not done by %s UTC. Follow the steps on the incident's page.", wake.UTC().Format("15:04"))
}

// resolvedSOSText is the title and message of a resolved SOS's notification.
// An SOS that waited for a server replacement, and woke nobody, resolves with
// the partner of the replacement's start warn: the person heard "Your site is
// down: TITLE", so they hear "Your site works again: TITLE". Once MMHotline
// tried to wake someone, the SOS ended an emergency like any other, so the
// ordinary text applies.
func resolvedSOSText(ctx context.Context, tx *sql.Tx, i Incident) (title, message string, err error) {
	held, err := wasHeldForAServerReplacement(ctx, tx, i.ID)
	if err != nil || !held {
		return i.Title, i.Message, err
	}
	var woken bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND `+attemptedDelivery+`)`, i.ID).Scan(&woken); err != nil || woken {
		return i.Title, i.Message, err
	}
	return truncateRunes("Your site works again: "+i.Title, 256), "The triage agent's server replacement fixed it; nobody needed to be woken.", nil
}

// extendWaitForServerReplacement moves a waiting SOS's deadline to until,
// when that is later, and records why. The step and revision stay: the SOS
// still waits for the agent, and a person's claim prepared before is still
// about the same step.
func extendWaitForServerReplacement(ctx context.Context, tx *sql.Tx, i Incident, playbook, inventory string, until, now time.Time) error {
	if until.After(i.Deadline) {
		i.Deadline, i.RepeatAt = until, until
		if err := saveIncident(ctx, tx, i); err != nil {
			return err
		}
	}
	return recordIncidentAction(ctx, tx, i, "system", heldForAServerReplacement, "The triage agent is replacing a server ("+playbookInInventoryText(playbook, inventory)+"), so MMHotline waits until "+i.Deadline.UTC().Format("15:04")+" UTC before waking the person on call, unless the replacement fails first.", "", "", now)
}

// incidentsWaitingForTriageAgent lists the open incidents whose first
// waking step waits for the agent, oldest first.
func incidentsWaitingForTriageAgent(ctx context.Context, tx *sql.Tx) ([]Incident, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+incidentColumns+` FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.status='open' AND i.step=? ORDER BY i.created_at,i.id`, triageAgentStep)
	if err != nil {
		return nil, err
	}
	var held []Incident
	for rows.Next() {
		i, err := scanIncident(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		held = append(held, i)
	}
	return held, errors.Join(rows.Err(), rows.Close())
}

// serverReplacementInProgress returns the open session's replacement that
// may still go on at now: its playbook, inventory, and when the SOS waits
// until; ok is false when there is none.
func serverReplacementInProgress(ctx context.Context, tx *sql.Tx, now time.Time) (playbook, inventory string, until time.Time, ok bool, err error) {
	var untilUnix int64
	err = tx.QueryRowContext(ctx, `SELECT r.playbook,r.inventory,r.sos_waits_until FROM server_replacements r JOIN triage_sessions s ON s.id=r.triage_session_id WHERE s.ended_at=0 AND r.ended_at=0 AND r.sos_waits_until>?`, now.Unix()).Scan(&playbook, &inventory, &untilUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", time.Time{}, false, nil
	}
	return playbook, inventory, time.Unix(untilUnix, 0).UTC(), err == nil, err
}

// serverReplacementHoldsANewSOS extends the wait of a new SOS that joins the
// open session while its replacement goes on to the replacement's limit,
// with no second warn alert. An SOS about a new problem, which starts a new
// session, is not extended.
func serverReplacementHoldsANewSOS(ctx context.Context, tx *sql.Tx, i Incident, now time.Time) error {
	playbook, inventory, until, ok, err := serverReplacementInProgress(ctx, tx, now)
	if err != nil || !ok {
		return err
	}
	var sessionID string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM triage_sessions WHERE ended_at=0`).Scan(&sessionID); err != nil {
		return err
	}
	newProblem, err := alertStartsANewTriageSession(ctx, tx, sessionID, i.OpeningAlertID)
	if err != nil || newProblem {
		return err
	}
	return extendWaitForServerReplacement(ctx, tx, i, playbook, inventory, until, now)
}

// serverReplacementFailed wakes the person on call at once for every SOS
// that waits, when a playbook run of the run's session failed or could not
// run while the session's replacement goes on. Outside a replacement a
// failed run wakes nobody by itself: the agent reads the result and decides.
func serverReplacementFailed(ctx context.Context, tx *sql.Tx, runID, cause string, now time.Time) error {
	var going bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_runs t JOIN server_replacements r ON r.triage_session_id=t.triage_session_id JOIN triage_sessions s ON s.id=r.triage_session_id WHERE t.id=? AND s.ended_at=0 AND r.ended_at=0 AND r.sos_waits_until>?)`, runID, now.Unix()).Scan(&going)
	if err != nil || !going {
		return err
	}
	return wakeIncidentsHeldForTriageAgent(ctx, tx, "The server replacement failed: "+cause+"; waking the person on call", now)
}

// wasHeldForAServerReplacement says whether a server replacement extended
// the incident's wait.
func wasHeldForAServerReplacement(ctx context.Context, tx *sql.Tx, incidentID string) (bool, error) {
	var held bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM incident_actions WHERE incident_id=? AND kind=?)`, incidentID, heldForAServerReplacement).Scan(&held)
	return held, err
}

// serverReplacementLimitText is why MMHotline wakes the person on call at an
// extended deadline: "The server replacement took longer than 10 minutes".
func serverReplacementLimitText(ctx context.Context, tx *sql.Tx) (string, error) {
	var started, until int64
	err := tx.QueryRowContext(ctx, `SELECT started_at,sos_waits_until FROM server_replacements ORDER BY started_at DESC,id DESC LIMIT 1`).Scan(&started, &until)
	if err != nil {
		return "", err
	}
	return "The server replacement took longer than " + minutesText(time.Duration(until-started)*time.Second), nil
}

// serverReplacementWakeMessage is the waking message of an SOS a server
// replacement held: why MMHotline wakes the person now, the title of the
// agent's last report, if any, then the SOS's own message. It also ends the
// replacement, so a later SOS waits only its ordinary time.
func serverReplacementWakeMessage(ctx context.Context, tx *sql.Tx, i Incident, why string, now time.Time) (string, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE server_replacements SET ended_at=? WHERE ended_at=0`, now.Unix()); err != nil {
		return "", err
	}
	cause := strings.TrimSuffix(strings.TrimSuffix(why, "; waking the person on call"), ". Waking the person on call")
	message := "MMHotline is waking you now. " + strings.TrimSuffix(cause, ".") + "."
	// The agent's report at sos, which wakes the person, is the last one
	// even before its answer is recorded.
	var report string
	err := tx.QueryRowContext(ctx, `SELECT q.report_text FROM triage_agent_requests q JOIN triage_agent_runs r ON r.id=q.run_id WHERE r.triage_session_id=(SELECT triage_session_id FROM server_replacements ORDER BY started_at DESC,id DESC LIMIT 1) AND q.kind='report' AND q.outcome IN ('','recorded') ORDER BY q.requested_at DESC,q.rowid DESC LIMIT 1`).Scan(&report)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if line := reportTitleLine(report); line != "" {
		message += " The triage agent's last report: “" + line + "”."
	}
	return message + "\n\n" + i.Message, nil
}

// endServerReplacementOfResolvedIncident ends the replacement whose START
// warn was about the incident that resolved: the outage it fixed is over.
// Otherwise the replacement would count as in progress until its limit, and
// another server that fails soon after, in the same session, would get no
// wait of its own and wake the person at the ordinary wait.
func endServerReplacementOfResolvedIncident(ctx context.Context, tx *sql.Tx, incidentID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE server_replacements SET ended_at=? WHERE incident_id=? AND ended_at=0`, at.Unix(), incidentID)
	return err
}
