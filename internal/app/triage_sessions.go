package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// Triage sessions hand alerts to the triage agent, one session at a time:
// docs/changing-mmhotline/design/triage-agent.md#triage-sessions.
//
// A session holds alerts, not incidents, because a warn alert opens no
// incident. Its incidents are those of its alerts. The alerts a run carried
// name it in sent_in_run_id; ” means the alert waits for the next run. A
// run belongs to the session named in triage_agent_runs.triage_session_id,
// and the session's active run is its run whose state is running. The
// agent's session ID, for a resume, is the latest one its runs reported.
// A member who pressed Stop took the session over (taken_over_by_member_id,
// taken_over_at); hand_over_run_id is the run that resumed the agent for
// its hand-over (triage_session_take_over.go).
const triageSessionSchema = `
CREATE TABLE triage_sessions (
 id TEXT PRIMARY KEY, started_at INTEGER NOT NULL,
 ended_at INTEGER NOT NULL DEFAULT 0, ended_because TEXT NOT NULL DEFAULT '',
 taken_over_by_member_id TEXT NOT NULL DEFAULT '', taken_over_at INTEGER NOT NULL DEFAULT 0,
 hand_over_run_id TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX triage_sessions_one_open ON triage_sessions(ended_at) WHERE ended_at=0;
CREATE TABLE triage_session_alerts (
 triage_session_id TEXT NOT NULL REFERENCES triage_sessions(id),
 alert_id TEXT NOT NULL UNIQUE REFERENCES alerts(id),
 joined_at INTEGER NOT NULL, sent_in_run_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX triage_session_alerts_session ON triage_session_alerts(triage_session_id, sent_in_run_id);
CREATE TABLE triage_session_alert_cursor (id INTEGER PRIMARY KEY CHECK(id=1), last_considered_alert_sequence INTEGER NOT NULL);
INSERT INTO triage_session_alert_cursor VALUES(1,0);
`

// triageSessionQuietTime: a session ends once every incident of it is
// resolved and no alert has joined it for this long. A problem that comes
// back soon after its incident is resolved joins the same session, where
// the agent still has its context.
const triageSessionQuietTime = 30 * time.Minute

// triageSessionAlertCondition picks the alerts that start or join a
// session: warn and sos from the team's alert sources and people, and
// MMHotline's own SOS about an alert source it stopped hearing from or
// refuses. MMHotline's notices about its own work, such as an escalation,
// the triage agent's own reports, and the test SOS of
// `mmhotline incidents send-test-sos` never do: nothing is wrong, so the
// agent has nothing to look into, and the test must wake the person at once.
const triageSessionAlertCondition = `severity IN ('warn','sos') AND (source_id!='system' OR (severity='sos' AND actor='system')) AND source_id!='` + testAlertSourceID + `'`

// triageAgentStep is the escalation step of an SOS incident whose first
// waking step waits for the triage agent: nobody is contacted yet, and the
// incident's deadline is when MMHotline wakes the person on call. Claiming,
// resolving, and escalating by hand end it as they end any step.
const triageAgentStep = "triage_agent"

// The settings file's triage_agent.sos_waits_for_the_agent_minutes: an SOS
// that joins a triage session waits this long after it arrived before
// MMHotline wakes the person on call, to give the agent a chance to fix the
// problem first. A team chooses by its risk appetite: a team with customers
// on a service-level agreement wants 0, which wakes the person at once; a
// team of one may let the agent try first. A person can always claim or
// escalate during the wait. A stuck or wrong agent must not hold an SOS
// longer than the maximum.
const sosWaitsForTheAgentMaximumMinutes = 15

// sosWaitsForTheAgentMinutesBeforeTheSetting is the wait before the settings
// file has applied one, the recommended start: in measured runs
// (2026-11-11) the agent's one fix was done 55 seconds after the session
// started and the alert resolved about 2 minutes later, and a hand-over
// came 1:00 to 1:50 after the start and ends the wait by itself. So 3
// minutes covers a fix and its confirmation, and a stuck agent delays the
// phone by at most 3 minutes.
const sosWaitsForTheAgentMinutesBeforeTheSetting = 3

// sosWaitsForTheAgent is the applied wait of an SOS for the triage agent.
func sosWaitsForTheAgent(ctx context.Context, tx *sql.Tx) (time.Duration, error) {
	minutes := sosWaitsForTheAgentMinutesBeforeTheSetting
	_, err := appliedSettingsValue(ctx, tx, settingsSOSWaitsForTheAgentPart.name, &minutes)
	return time.Duration(minutes) * time.Minute, err
}

// A run that MMHotline's own stop or restart cut short is resumed with the
// alerts it carried.
const (
	triageRunCutByStop    = "MMHotline stopped during the run."
	triageRunCutByRestart = "MMHotline restarted during the run."
)

var errTriageSessionEnded = errors.New("the triage session ended before its run started")

// triageSessionRunPlan is the run a session needs next: a start with all of
// its alerts, or a resume of the agent's session with the alerts that wait.
// The run carries the alerts up to throughAlertSequence, and the results of
// approved or refused playbook runs that wait, by their approval request IDs.
type triageSessionRunPlan struct {
	sessionID, action, codingAgentSessionID, lastRunEndedBecause string
	alerts                                                       []triageSessionPromptAlert
	throughAlertSequence                                         int64
	approvalRequestIDs, approvalResults                          []string
	// takenOver, for the hand-over run of a session a person took over,
	// says who took it over and when; it is "" for every other run.
	takenOver string
	// earlierRuns are the session's runs so far, oldest first, each with
	// its start and end, so the agent computes times instead of guessing.
	earlierRuns []string
	// incidentNotes are the notes from the team the run gets, oldest first,
	// and incidentNoteIDs their history entries' IDs, which the run records
	// as given (incident_notes_for_the_triage_agent.go).
	incidentNoteIDs []int64
	incidentNotes   []triageSessionPromptIncidentNote
}

// DriveTriageSession is the background loop's turn for triage sessions. The
// loop drives them, rather than the intake path, because every alert is
// already committed when the loop sees it, and the loop reads the same
// durable state after a restart. Starting a run fetches from the team's Git
// host, so it goes on in its own goroutine and never delays a delivery.
func (a *App) DriveTriageSession(ctx context.Context) error {
	runtime := &a.triageAgent
	runtime.mu.Lock()
	installed := runtime.runSocketPath != "" && runtime.proxySocketPath != ""
	busy := runtime.activeRun != nil || runtime.backgroundRunStarting
	runtime.mu.Unlock()
	copyExists := a.systemsRepositoryCopyExists()
	now := a.Now().UTC()
	var runToStop string
	var stopRunForSession bool
	var plan *triageSessionRunPlan
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var codingAgent, sessionID string
		err := tx.QueryRowContext(ctx, `SELECT coding_agent FROM triage_agent_setup WHERE id=1`).Scan(&codingAgent)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		setUp := err == nil
		if err = tx.QueryRowContext(ctx, `SELECT id FROM triage_sessions WHERE ended_at=0`).Scan(&sessionID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !setUp && sessionID == "" {
			// A warn held while the agent was set up cannot wait for it now.
			return settleHeldWarns(ctx, tx, "The triage agent is no longer set up.", now)
		}
		cannotRun := ""
		switch {
		case !setUp:
			cannotRun = "The triage agent is no longer set up."
		case !installed:
			cannotRun = "The triage agent sandbox is not installed."
		case !copyExists:
			cannotRun = "MMHotline's copy of the systems repository is missing."
		}
		if cannotRun != "" {
			// An SOS waits for the agent only while the agent can run.
			if err = wakeIncidentsHeldForTriageAgent(ctx, tx, strings.TrimSuffix(cannotRun, ".")+"; waking the person on call", now); err != nil {
				return err
			}
		}
		if setUp {
			if sessionID, err = joinNewAlertsToTriageSession(ctx, tx, sessionID, cannotRun == "", now); err != nil {
				return err
			}
		}
		// Held warns are settled once the alerts are read: a warn that
		// started the session waits for the agent, any other is delivered.
		if err = settleHeldWarns(ctx, tx, cannotRun, now); err != nil {
			return err
		}
		if sessionID == "" {
			return nil
		}
		endBecause, next := cannotRun, ""
		var session triageSessionState
		if endBecause == "" {
			if session, err = readTriageSessionState(ctx, tx, sessionID); err != nil {
				return err
			}
			// A person took the session over: its hand-over, not the quiet
			// time, decides its end.
			if session.TakenOverByMemberID != "" {
				if next, err = handOverNext(ctx, tx, session); err != nil {
					return err
				}
			} else if endBecause, err = triageSessionQuietEnd(ctx, tx, sessionID, now); err != nil {
				return err
			}
		}
		switch next {
		case handOverStopTheRun:
			// The stop every run gets, after the agent's current step. It is
			// sent again at each visit, as a run still starting has no
			// connection yet; a second stop changes nothing.
			runToStop = session.RunningRunID
			return nil
		case handOverWait:
			return nil
		case handOverEndTheSession:
			if endBecause, err = session.handOverEndedBecause(ctx, tx, false); err != nil {
				return err
			}
		case handOverStart:
			// While the one run is taken, the hand-over waits for it, and a
			// software review or a sign-in check gives it way.
			if busy {
				stopRunForSession = true
				return nil
			}
			if plan, err = a.planTriageSessionHandOver(ctx, tx, session, codingAgent); err != nil || plan != nil {
				return err
			}
			if endBecause, err = session.handOverEndedBecause(ctx, tx, true); err != nil {
				return err
			}
		}
		if endBecause != "" {
			if err = endTriageSession(ctx, tx, sessionID, endBecause, now); err != nil {
				return err
			}
			// The session's active run is its run that is still running.
			err = tx.QueryRowContext(ctx, `SELECT id FROM triage_agent_runs WHERE triage_session_id=? AND state='running'`, sessionID).Scan(&runToStop)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if plan, err = a.planTriageSessionRun(ctx, tx, sessionID, codingAgent); err != nil || plan == nil || !busy {
			return err
		}
		// Incidents come first: the session needs a run, for an alert or the
		// result of a playbook run a person decided, so a software review or
		// a sign-in check gives it the one run. The run starts at the visit
		// after the one run is free. A session that waits for a person, such
		// as for an approval, needs no run and holds nothing up.
		stopRunForSession, plan = true, nil
		return nil
	})
	if err != nil {
		return err
	}
	if runToStop != "" {
		// The existing stop: after the agent's current step. A run already
		// ending needs none.
		_ = a.StopTriageAgentRun(runToStop)
	}
	if stopRunForSession {
		a.stopRunForTriageSession()
	}
	if plan != nil {
		a.startBesideTheLoop(func() { a.startTriageSessionRun(ctx, *plan) })
	}
	return nil
}

// stopRunForTriageSession stops a software review or a sign-in check that
// holds the one run while a triage session needs it, after the agent's
// current step. The run records why, so it starts again once the one run is
// free.
func (a *App) stopRunForTriageSession() {
	a.triageAgent.mu.Lock()
	run := a.triageAgent.activeRun
	givesWay := run != nil && givesWayToTriageSessions(run.purpose)
	if givesWay {
		run.stoppedForTriageSession = true
	}
	a.triageAgent.mu.Unlock()
	if givesWay {
		// A run still starting has no connection yet; its first event
		// brings another visit, which sends the stop.
		_ = a.writeTriageAgentControl(run, triageagent.Control{Stop: true})
	}
}

// systemsRepositoryCopyExists says whether MMHotline's copy of the systems
// repository is there, which every run in the systems repository needs.
func (a *App) systemsRepositoryCopyExists() bool {
	_, err := os.Stat(filepath.Join(a.Store.Dir, systemsrepository.DirectoryName, ".git"))
	return err == nil
}

// endTriageSession ends the open session and records why. A result of a
// playbook run that still waits for the agent can no longer reach it.
func endTriageSession(ctx context.Context, tx *sql.Tx, sessionID, because string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE triage_sessions SET ended_at=?,ended_because=? WHERE id=?`, now.Unix(), because, sessionID); err != nil {
		return err
	}
	return approvalResultsCouldNotReachTheAgent(ctx, tx, sessionID, now)
}

// triageSessionNewProblemEnd is why a session ends when an alert about a new
// problem arrives after the session's work is done.
const triageSessionNewProblemEnd = "An alert about a new problem arrived after the session's work was done, and started a new session."

// joinNewAlertsToTriageSession considers the alerts admitted since the last
// visit, once each. When the agent can run, those that start or join a
// session join the open session, or start one; an alert about a new problem
// that arrives after the open session's work is done ends that session and
// starts a new one. An alert that arrived while the agent could not run never
// starts a session later.
func joinNewAlertsToTriageSession(ctx context.Context, tx *sql.Tx, sessionID string, agentCanRun bool, now time.Time) (string, error) {
	var considered, newest int64
	if err := tx.QueryRowContext(ctx, `SELECT last_considered_alert_sequence FROM triage_session_alert_cursor WHERE id=1`).Scan(&considered); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM alerts`).Scan(&newest); err != nil {
		return "", err
	}
	if newest <= considered {
		return sessionID, nil
	}
	if agentCanRun {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM alerts WHERE sequence>? AND sequence<=? AND `+triageSessionAlertCondition+` ORDER BY sequence`, considered, newest)
		if err != nil {
			return "", err
		}
		var alertIDs []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return "", err
			}
			alertIDs = append(alertIDs, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return "", err
		}
		for _, id := range alertIDs {
			if sessionID != "" {
				newProblem, err := alertStartsANewTriageSession(ctx, tx, sessionID, id)
				if err != nil {
					return "", err
				}
				if newProblem {
					if err = endTriageSession(ctx, tx, sessionID, triageSessionNewProblemEnd, now); err != nil {
						return "", err
					}
					sessionID = ""
				}
			}
			startedTheSession := sessionID == ""
			if startedTheSession {
				sessionID = NewID()
				if _, err = tx.ExecContext(ctx, `INSERT INTO triage_sessions(id,started_at) VALUES(?,?)`, sessionID, now.Unix()); err != nil {
					return "", err
				}
				if err = sendTriageSessionStartMessage(ctx, tx, id, now); err != nil {
					return "", err
				}
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO triage_session_alerts(triage_session_id,alert_id,joined_at) VALUES(?,?,?)`, sessionID, id, now.Unix()); err != nil {
				return "", err
			}
			if err = sessionHoldsItsWarn(ctx, tx, id, sessionID, startedTheSession, now); err != nil {
				return "", err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE triage_session_alert_cursor SET last_considered_alert_sequence=? WHERE id=1`, newest)
	return sessionID, err
}

// alertStartsANewTriageSession says whether an alert ends the open session
// and starts a new one: the session's work is done, and the alert is about a
// problem the session does not have. A problem is an alert source and a
// sender incident key; an alert without a key is a problem of its own. The
// work is done when every incident of the session is resolved, no approval
// request or warned run of it waits or runs, no result of one waits for the
// agent, no run of it is going, and no alert of it waits for its first run.
// A session a person took over ends after its hand-over instead.
func alertStartsANewTriageSession(ctx context.Context, tx *sql.Tx, sessionID, alertID string) (bool, error) {
	var starts bool
	err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM triage_sessions WHERE id=?1 AND taken_over_by_member_id!='')
		AND NOT EXISTS(SELECT 1 FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id JOIN incidents i ON i.id=e.incident_id WHERE s.triage_session_id=?1 AND i.status!='resolved')
		AND NOT EXISTS(SELECT 1 FROM triage_agent_playbook_runs_waiting_for_a_person WHERE triage_session_id=?1 AND (state IN (?3,?4,?5) OR result_for_the_agent=?6))
		AND NOT EXISTS(SELECT 1 FROM triage_agent_runs WHERE triage_session_id=?1 AND state='running')
		AND NOT EXISTS(SELECT 1 FROM triage_session_alerts WHERE triage_session_id=?1 AND sent_in_run_id='')
		AND NOT EXISTS(SELECT 1 FROM alerts n JOIN alerts e ON e.source_id=n.source_id AND e.sender_incident_key=n.sender_incident_key JOIN triage_session_alerts s ON s.alert_id=e.id WHERE n.id=?2 AND n.sender_incident_key!='' AND s.triage_session_id=?1)`,
		sessionID, alertID, approvalOpen, approvalApproved, approvalRunning, resultForTheAgentWaits).Scan(&starts)
	return starts, err
}

// sendTriageSessionStartMessage tells the person on call quietly, once per
// session, that the triage agent started on it: an info alert from MMHotline
// to them alone, so Pushover sends it at low priority without sound, and
// other contact methods send it as any info alert. Info from MMHotline never
// starts a session or opens an incident. The person on call is whom an SOS
// would contact first now: the scheduled person, or with nobody scheduled,
// everyone available. When nobody is available, nobody gets it: a quiet
// message is no reason to reach an unavailable person.
func sendTriageSessionStartMessage(ctx context.Context, tx *sql.Tx, firstAlertID string, startedAt time.Time) error {
	c, err := CoverageAt(ctx, tx, startedAt)
	if err != nil || len(c.PrimaryIDs) == 0 {
		return err
	}
	var title, incidentID string
	if err = tx.QueryRowContext(ctx, `SELECT title,incident_id FROM alerts WHERE id=?`, firstAlertID).Scan(&title, &incidentID); err != nil {
		return err
	}
	started := " (started " + startedAt.UTC().Format("15:04") + " UTC)"
	in := GenericAlert{Severity: "info", Title: truncateRunes("The AI triage agent is looking into: "+title, 256-utf8.RuneCountInString(started)) + started, Message: "Nothing is needed from you now. MMHotline wakes the person on call when a person is needed."}
	// The SOS's incident page shows when MMHotline wakes the person on call.
	_, err = admitAlert(ctx, tx, "system", "system", "local:"+NewID(), incidentID, in, c.PrimaryIDs, startedAt)
	return err
}

// skipAlertsForTriageSessions marks every alert so far as considered, when
// the triage agent is first set up: alerts from before the setup never start
// a session.
func skipAlertsForTriageSessions(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE triage_session_alert_cursor SET last_considered_alert_sequence=(SELECT coalesce(max(sequence),0) FROM alerts) WHERE id=1`)
	return err
}

// sosWaitsForTriageAgent says whether a new SOS incident's first waking step
// waits for the triage agent: its alert joins a triage session, and no person
// created it by hand, because a person who creates an SOS wants someone now.
// It never waits while a person has taken the open session over: the agent
// works on nothing more in it, and the next session starts only after it.
// Whether the agent's sandbox and MMHotline's copy are there is known only to
// the background loop, whose next visit wakes the incident when they are not.
func sosWaitsForTriageAgent(ctx context.Context, tx *sql.Tx, alertID string) (bool, error) {
	var waits bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup) AND EXISTS(SELECT 1 FROM alerts WHERE id=? AND source_id!='manual' AND `+triageSessionAlertCondition+`) AND NOT EXISTS(SELECT 1 FROM triage_sessions WHERE ended_at=0 AND taken_over_by_member_id!='')`, alertID).Scan(&waits)
	return waits, err
}

// triageSessionAlreadyWoke returns an unresolved incident of the open triage
// session whose SOS escalation already woke each of targets, the people a
// new SOS of the session would wake first, or "" when it did not. Such an
// SOS neither waits nor wakes them again: they are already at work on the
// session, so a second wake only interrupts them. An unresolved incident
// keeps the session's work going, so the new SOS joins this session.
//
// A wake counts only when a contact method that wakes people had it
// accepted by its sending service. When the wake failed to send, its
// outcome is unknown, or its incident was resolved, it may have reached
// nobody, so the new SOS waits and wakes as any other: a rare second wake
// is better than a dropped SOS.
func triageSessionAlreadyWoke(ctx context.Context, tx *sql.Tx, targets []string) (incidentTitle string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT d.member_id,d.contact_method_type,o.title FROM triage_sessions s
		JOIN triage_session_alerts sa ON sa.triage_session_id=s.id JOIN alerts e ON e.id=sa.alert_id
		JOIN incidents i ON i.id=e.incident_id JOIN alerts o ON o.id=i.opening_alert_id JOIN deliveries d ON d.incident_id=i.id
		WHERE s.ended_at=0 AND i.status!='resolved' AND d.purpose='sos_escalation' AND d.state IN ('accepted','delivered')
		ORDER BY i.created_at,i.id`)
	if err != nil {
		return "", err
	}
	woken := map[string]string{}
	for rows.Next() {
		var memberID, contactMethodType, title string
		if err = rows.Scan(&memberID, &contactMethodType, &title); err != nil {
			rows.Close()
			return "", err
		}
		if facts, known := contactMethodTypeFacts(contactMethodType); known && facts.WakesPeople && woken[memberID] == "" {
			woken[memberID] = title
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(targets) == 0 {
		return "", err
	}
	for _, id := range targets {
		if woken[id] == "" {
			return "", nil
		}
	}
	return woken[targets[0]], nil
}

// wakeIncidentHeldForTriageAgent ends an incident's wait for the triage agent
// and starts its first waking step now, as createIncident starts it for an
// SOS that does not wait, so the repeats and later steps count from now. why
// goes in the incident's history.
func wakeIncidentHeldForTriageAgent(ctx context.Context, tx *sql.Tx, i Incident, why string, at time.Time) error {
	// The person is told everything now, the warnings held with the SOS too.
	if err := deliverJoinedHeldWarns(ctx, tx, why, at); err != nil {
		return err
	}
	c, err := CoverageAt(ctx, tx, at)
	if err != nil {
		return err
	}
	window, err := escalateIfUnclaimedAfter(ctx, tx)
	if err != nil {
		return err
	}
	// A person's action prepared during the wait must not act on the step
	// that replaced it unseen.
	i.Revision++
	i.TeamMuteRevision = c.TeamMute.Revision
	chooseInitialTarget(&i, c, window, at)
	if err = recordIncidentAction(ctx, tx, i, "system", "triage_agent_hold_ended", why, "", "", at); err != nil {
		return err
	}
	// An SOS a server replacement held says, in its first waking message,
	// why MMHotline wakes the person now: the person heard the replacement
	// start and expects to sleep through it.
	replacement, err := wasHeldForAServerReplacement(ctx, tx, i.ID)
	if err != nil {
		return err
	}
	if replacement {
		if i.Message, err = serverReplacementWakeMessage(ctx, tx, i, why, at); err != nil {
			return err
		}
	}
	return queueSOSEscalation(ctx, tx, &i, at)
}

// wakeIncidentsHeldForTriageAgent wakes every incident that waits for the
// triage agent, when the agent cannot help with them now: it cannot run, its
// run failed, or it reported at sos. One session is open at a time, so these
// are the session's incidents and those about to join it. An SOS that woke
// nobody because the session already woke its people
// (triageSessionAlreadyWoke) wakes them too, before its time to claim ends:
// the agent cannot help or asks for a person, and a second wake is better
// than a late one.
func wakeIncidentsHeldForTriageAgent(ctx context.Context, tx *sql.Tx, why string, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+incidentColumns+` FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.status='open' AND (i.step=? OR EXISTS(SELECT 1 FROM incident_actions a WHERE a.incident_id=i.id AND a.revision=i.revision AND a.kind='already_woken_for_triage_session')) ORDER BY i.created_at,i.id`, triageAgentStep)
	if err != nil {
		return err
	}
	var held []Incident
	for rows.Next() {
		i, err := scanIncident(rows)
		if err != nil {
			rows.Close()
			return err
		}
		held = append(held, i)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, i := range held {
		if err = wakeIncidentHeldForTriageAgent(ctx, tx, i, why, at); err != nil {
			return err
		}
	}
	return nil
}

// triageSessionQuietEnd returns why the session ends now, or "" while one of
// its incidents is unresolved or an alert joined it less than
// triageSessionQuietTime ago.
func triageSessionQuietEnd(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time) (string, error) {
	quietSince, err := triageSessionQuietSince(ctx, tx, sessionID)
	if err != nil || quietSince.IsZero() || now.Sub(quietSince) < triageSessionQuietTime {
		return "", err
	}
	return fmt.Sprintf("Every incident of the session was resolved, and no alert joined it for %d minutes.", int(triageSessionQuietTime.Minutes())), nil
}

// triageSessionNextEnd returns when the open session's quiet time runs out,
// for the background loop's next visit. Any other change to the session is a
// change to the database, which brings a visit anyway.
func triageSessionNextEnd(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var sessionID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM triage_sessions WHERE ended_at=0`).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	quietSince, err := triageSessionQuietSince(ctx, tx, sessionID)
	if err != nil || quietSince.IsZero() {
		return time.Time{}, err
	}
	return quietSince.Add(triageSessionQuietTime), nil
}

// triageSessionQuietSince returns when the session's last alert joined it or
// a playbook run of it last changed, such as by an approval, or zero while one
// of its incidents is unresolved or an approved or warned run of it has not
// ended, so the result reaches the agent in a session that is still open.
func triageSessionQuietSince(ctx context.Context, tx *sql.Tx, sessionID string) (time.Time, error) {
	var unresolved int
	var lastJoined, lastChanged int64
	err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id JOIN incidents i ON i.id=e.incident_id WHERE s.triage_session_id=?1 AND i.status!='resolved')
		+ (SELECT count(*) FROM triage_agent_playbook_runs_waiting_for_a_person WHERE triage_session_id=?1 AND (state IN (?2,?3) OR (state=?4 AND rule=?5))),
		(SELECT coalesce(max(joined_at),0) FROM triage_session_alerts WHERE triage_session_id=?1),
		(SELECT coalesce(max(state_changed_at),0) FROM triage_agent_playbook_runs_waiting_for_a_person WHERE triage_session_id=?1)`, sessionID, approvalApproved, approvalRunning, approvalOpen, warnWaitThenAct).Scan(&unresolved, &lastJoined, &lastChanged)
	if err != nil || unresolved > 0 {
		return time.Time{}, err
	}
	return time.Unix(max(lastJoined, lastChanged), 0).UTC(), nil
}

// planTriageSessionRun returns the session's next run, or nil when no alert
// and no result of a playbook run a person decided waits.
func (a *App) planTriageSessionRun(ctx context.Context, tx *sql.Tx, sessionID, codingAgent string) (*triageSessionRunPlan, error) {
	plan := &triageSessionRunPlan{sessionID: sessionID, action: "start"}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(e.sequence),0) FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE s.triage_session_id=? AND s.sent_in_run_id=''`, sessionID).Scan(&plan.throughAlertSequence); err != nil {
		return nil, err
	}
	var err error
	if plan.approvalRequestIDs, plan.approvalResults, err = approvalResultsForSession(ctx, tx, sessionID); err != nil || plan.throughAlertSequence == 0 && len(plan.approvalResults) == 0 {
		return nil, err
	}
	if plan.codingAgentSessionID, plan.lastRunEndedBecause, err = resumableCodingAgentSession(ctx, tx, sessionID, codingAgent); err != nil {
		return nil, err
	}
	waitingOnly := ""
	if plan.codingAgentSessionID != "" {
		plan.action = "resume"
		waitingOnly = ` AND s.sent_in_run_id=''`
	}
	// A start shows every alert of the session: an earlier run that failed
	// before the agent began may have carried some.
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.source_id,e.actor,e.incident_id,e.severity,e.title,e.message,e.received_at,coalesce(src.name,'') FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id LEFT JOIN sources src ON src.id=e.source_id WHERE s.triage_session_id=? AND e.sequence<=?`+waitingOnly+` ORDER BY e.sequence`, sessionID, plan.throughAlertSequence)
	if err != nil {
		return nil, err
	}
	type row struct {
		alert                           triageSessionPromptAlert
		id, sourceID, actor, incidentID string
	}
	var found []row
	for rows.Next() {
		var r row
		var received int64
		if err = rows.Scan(&r.id, &r.sourceID, &r.actor, &r.incidentID, &r.alert.Severity, &r.alert.Title, &r.alert.Message, &received, &r.alert.SourceName); err != nil {
			rows.Close()
			return nil, err
		}
		r.alert.ReceivedAt = time.Unix(received, 0).UTC()
		found = append(found, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	webURL := strings.TrimRight(a.WebURL, "/")
	var alertIDs []string
	for _, r := range found {
		alertIDs = append(alertIDs, r.id)
		switch r.sourceID {
		case "system":
			r.alert.SourceName = "MMHotline"
		case "manual":
			name, err := incidentActorName(ctx, tx, r.actor)
			if err != nil {
				return nil, err
			}
			r.alert.SourceName = name + ", who created it by hand"
		}
		r.alert.AlertURL = webURL + "/alerts/" + r.id
		if r.incidentID != "" {
			r.alert.IncidentURL = webURL + "/incidents/" + r.incidentID
		}
		plan.alerts = append(plan.alerts, r.alert)
	}
	if err = plan.gatherIncidentNotes(ctx, tx, alertIDs); err != nil {
		return nil, err
	}
	if plan.earlierRuns, err = triageSessionRunTimes(ctx, tx, plan.sessionID); err != nil {
		return nil, err
	}
	return plan, nil
}

// resumableCodingAgentSession returns the coding agent session the session's
// runs last reported for codingAgent, "" when none did, and why the session's
// last run failed, if it did. A session ID of another coding agent cannot be
// resumed; the setup may have changed since.
func resumableCodingAgentSession(ctx context.Context, tx *sql.Tx, sessionID, codingAgent string) (codingAgentSessionID, lastRunEndedBecause string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT coding_agent_session_id FROM triage_agent_runs WHERE triage_session_id=? AND coding_agent=? AND coding_agent_session_id!='' ORDER BY started_at DESC,rowid DESC LIMIT 1`, sessionID, codingAgent).Scan(&codingAgentSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var state, reason string
	if err = tx.QueryRowContext(ctx, `SELECT state,failure_reason FROM triage_agent_runs WHERE triage_session_id=? ORDER BY started_at DESC,rowid DESC LIMIT 1`, sessionID).Scan(&state, &reason); err != nil {
		return "", "", err
	}
	if state == "failed" {
		lastRunEndedBecause = reason
	}
	return codingAgentSessionID, lastRunEndedBecause, nil
}

// planTriageSessionHandOver returns the hand-over run of a session a person
// took over: a resume of the agent's session with the results of playbook
// runs that wait for it, so its note says how they went, and no alert,
// because a person handles what arrived since. It is nil when the agent
// never reported a session, which leaves nothing to hand over.
func (a *App) planTriageSessionHandOver(ctx context.Context, tx *sql.Tx, session triageSessionState, codingAgent string) (*triageSessionRunPlan, error) {
	plan := &triageSessionRunPlan{sessionID: session.ID, action: "resume"}
	var err error
	if plan.codingAgentSessionID, plan.lastRunEndedBecause, err = resumableCodingAgentSession(ctx, tx, session.ID, codingAgent); err != nil || plan.codingAgentSessionID == "" {
		return nil, err
	}
	if plan.approvalRequestIDs, plan.approvalResults, err = approvalResultsForSession(ctx, tx, session.ID); err != nil {
		return nil, err
	}
	if plan.takenOver, err = session.takenOverText(ctx, tx); err != nil {
		return nil, err
	}
	if err = plan.gatherIncidentNotes(ctx, tx, nil); err != nil {
		return nil, err
	}
	if plan.earlierRuns, err = triageSessionRunTimes(ctx, tx, plan.sessionID); err != nil {
		return nil, err
	}
	return plan, nil
}

// startTriageSessionRun writes the plan's prompt and starts its run. The run
// takes the alerts it carries in the transaction that records it, and only
// while the session is still open.
func (a *App) startTriageSessionRun(ctx context.Context, plan triageSessionRunPlan) {
	prompt := triageSessionResumePrompt(plan.lastRunEndedBecause, plan.approvalResults, plan.incidentNotes, plan.alerts)
	switch {
	case plan.takenOver != "":
		prompt = triageSessionHandOverPrompt(plan.takenOver, plan.lastRunEndedBecause, plan.approvalResults, plan.incidentNotes)
	case plan.action == "start":
		serverName, _ := os.Hostname()
		// A copy Git cannot read fails the run's own fetch too, with the reason.
		lastNote, _ := systemsrepository.NewestTriageAgentNote(ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName))
		prompt = triageSessionStartPrompt(serverName, lastNote, a.inventoryNames(), plan.approvalResults, plan.incidentNotes, plan.alerts)
	}
	prompt += triageSessionTimesText(a.Now(), plan.earlierRuns)
	request := triageagent.Request{Action: plan.action, SessionID: plan.codingAgentSessionID, Prompt: prompt}
	_, err := a.startTriageAgentRunInSystemsRepository(ctx, "triage_session", "MMHotline", request, func(tx *sql.Tx, runID string) error {
		var open, takenOver bool
		if err := tx.QueryRowContext(ctx, `SELECT ended_at=0,taken_over_by_member_id!='' FROM triage_sessions WHERE id=?`, plan.sessionID).Scan(&open, &takenOver); err != nil {
			return err
		}
		// A person who took the session over while this run was being
		// prepared gets the hand-over at the next visit instead.
		if !open || takenOver && plan.takenOver == "" {
			return errTriageSessionEnded
		}
		if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_runs SET triage_session_id=? WHERE id=?`, plan.sessionID, runID); err != nil {
			return err
		}
		if plan.takenOver != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE triage_sessions SET hand_over_run_id=? WHERE id=?`, runID, plan.sessionID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE triage_session_alerts SET sent_in_run_id=? WHERE triage_session_id=? AND sent_in_run_id='' AND alert_id IN (SELECT id FROM alerts WHERE sequence<=?)`, runID, plan.sessionID, plan.throughAlertSequence); err != nil {
			return err
		}
		for _, id := range plan.approvalRequestIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET result_for_the_agent=?,result_sent_in_run_id=? WHERE id=? AND result_for_the_agent=?`, resultForTheAgentSent, runID, id, resultForTheAgentWaits); err != nil {
				return err
			}
		}
		return incidentNotesGiven(ctx, tx, runID, plan.incidentNoteIDs)
	})
	// The alerts still wait; the next visit tries again.
	if err != nil && !errors.Is(err, errTriageSessionEnded) && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "mmhotline: could not start the triage session's run: %s\n", err)
	}
}

// triageSessionIncidents lists the incidents of a session, the one whose
// alert joined last first.
func triageSessionIncidents(ctx context.Context, tx *sql.Tx, sessionID string) ([]Incident, error) {
	return incidentsOfTriageSessionAlerts(ctx, tx, `s.triage_session_id=?`, sessionID)
}

// triageRunIncidents lists the incidents a triage run's work is about, the
// one whose alert joined last first: those of the alerts the run carried to
// the agent, and, for each playbook run's result it carried, those of the
// run that asked for that playbook run. A run that carried neither, such as
// one after a restart, goes on with the work of the session's latest run
// that carried alerts. A hand-over is about the whole session, so its run's
// incidents are all the session's. A software review's run has no session
// and no incident.
func triageRunIncidents(ctx context.Context, tx *sql.Tx, sessionID, runID string) ([]Incident, error) {
	if sessionID == "" {
		return nil, nil
	}
	var handOver bool
	if err := tx.QueryRowContext(ctx, `SELECT hand_over_run_id=? FROM triage_sessions WHERE id=?`, runID, sessionID).Scan(&handOver); err != nil {
		return nil, err
	}
	if handOver {
		return triageSessionIncidents(ctx, tx, sessionID)
	}
	// The run's work is the alerts it carried and, for each playbook run's
	// result it carried, the work of the run that asked for that playbook
	// run, so a result's report stays with the problem it was asked for
	// even when another problem's run came between.
	const runsOfTheWork = `WITH RECURSIVE worked(run_id) AS (SELECT ? UNION SELECT w.run_id FROM triage_agent_playbook_runs_waiting_for_a_person w JOIN worked ON w.result_sent_in_run_id=worked.run_id) SELECT run_id FROM worked`
	var carried bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_session_alerts WHERE sent_in_run_id IN (`+runsOfTheWork+`))`, runID).Scan(&carried); err != nil {
		return nil, err
	}
	if carried {
		return incidentsOfTriageSessionAlerts(ctx, tx, `s.sent_in_run_id IN (`+runsOfTheWork+`)`, runID)
	}
	return incidentsOfTriageSessionAlerts(ctx, tx, `s.sent_in_run_id=(SELECT r.id FROM triage_agent_runs r WHERE r.triage_session_id=? AND EXISTS(SELECT 1 FROM triage_session_alerts carried WHERE carried.sent_in_run_id=r.id) ORDER BY r.started_at DESC,r.rowid DESC LIMIT 1)`, sessionID)
}

// incidentsOfTriageSessionAlerts lists the incidents of the rows of
// triage_session_alerts, named s, that where picks, the one whose alert
// joined last first.
func incidentsOfTriageSessionAlerts(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]Incident, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.incident_id FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE `+where+` AND e.incident_id!='' GROUP BY e.incident_id ORDER BY max(e.sequence) DESC`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	incidents := make([]Incident, 0, len(ids))
	for _, id := range ids {
		i, err := readIncident(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		incidents = append(incidents, i)
	}
	return incidents, nil
}

// triageSessionRunFailed records why a session's run failed with each
// incident of the session, and wakes the person on call for the incidents
// that wait for the agent. A run that MMHotline's own stop or restart cut
// short is resumed: the alerts it carried wait again, and so does an SOS,
// until its deadline.
func triageSessionRunFailed(ctx context.Context, tx *sql.Tx, runID, reason string, cutShort bool, at time.Time) error {
	var sessionID string
	if err := tx.QueryRowContext(ctx, `SELECT triage_session_id FROM triage_agent_runs WHERE id=?`, runID).Scan(&sessionID); err != nil || sessionID == "" {
		return err
	}
	if cutShort {
		if _, err := tx.ExecContext(ctx, `UPDATE triage_session_alerts SET sent_in_run_id='' WHERE sent_in_run_id=?`, runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET result_for_the_agent=?,result_sent_in_run_id='' WHERE result_sent_in_run_id=?`, resultForTheAgentWaits, runID); err != nil {
			return err
		}
		if err := incidentNotesWaitAgain(ctx, tx, runID); err != nil {
			return err
		}
	}
	incidents, err := triageSessionIncidents(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	for _, i := range incidents {
		if err = recordIncidentAction(ctx, tx, i, "system", "triage_agent_could_not_run", "The triage agent's run failed: "+reason, "", "", at); err != nil {
			return err
		}
	}
	if cutShort {
		return nil
	}
	return wakeIncidentsHeldForTriageAgent(ctx, tx, "The triage agent could not run; waking the person on call", at)
}

// deliverTriageAgentReport records a report of a session's run with each
// incident the run's work is about (triageRunIncidents) and sends it to
// people at its alert severity, as an alert from MMHotline about one of
// them. At sos, an open one wakes the person on call through its own SOS
// escalation, and a claimed one's owner has it; when every one is resolved,
// or the run's alerts have none, as for a warn alert, the report opens an SOS
// incident, whose escalation wakes them, and that incident joins the
// session. Every incident of the session that waits for the agent stops
// waiting at an sos report.
//
// A software review's run has no session, so each of its reports is an
// alert of its own: at sos it opens an SOS incident, which records the
// report, and whose escalation wakes the person on call at once. The agent's
// own alert never waits for the agent, and it wakes no incident of a session
// the review knows nothing about.
func (a *App) deliverTriageAgentReport(ctx context.Context, runID, severity, report string) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return deliverTriageAgentReportTx(ctx, tx, runID, severity, report, a.Now().UTC())
	})
}

// deliverTriageAgentReportTx is deliverTriageAgentReport in the caller's
// transaction, so a commit review's report and the commit's record of it
// commit together.
func deliverTriageAgentReportTx(ctx context.Context, tx *sql.Tx, runID, severity, report string, now time.Time) error {
	var sessionID string
	if err := tx.QueryRowContext(ctx, `SELECT triage_session_id FROM triage_agent_runs WHERE id=?`, runID).Scan(&sessionID); err != nil {
		return err
	}
	in := GenericAlert{Severity: severity, Title: triageAgentReportTitle(report), Message: triageAgentReportMessage(report)}
	alert, incidents, err := admitAlertAboutTriageRun(ctx, tx, sessionID, runID, "triage_agent", in, now)
	if err != nil {
		return err
	}
	// The agent's report takes over the warn that started the session.
	if err = heldWarnTakenOver(ctx, tx, sessionID, runID, "report", "/alerts/"+alert.ID, now); err != nil {
		return err
	}
	for _, i := range incidents {
		if err = recordIncidentAction(ctx, tx, i, "triage_agent", "triage_agent_report", report, alert.ID, "", now); err != nil {
			return err
		}
	}
	if severity != "sos" || sessionID == "" {
		return nil
	}
	return wakeIncidentsHeldForTriageAgent(ctx, tx, "The triage agent sent an urgent report (sos). Waking the person on call", now)
}

// admitAlertAboutTriageRun admits an alert from MMHotline, by actor, about
// the work of a triage run in sessionID, "" for a software review. It goes
// with an open incident the run's work is about (triageRunIncidents), then a
// claimed one, then below sos a resolved one, the last to join first, and
// never with an incident of the session's other problems. Otherwise an sos
// opens an incident of its own, which joins the session as already sent by
// the run, because the run's own alert carries nothing new to the agent, and
// which keeps the session open until a person resolves it. It returns the
// alert and the run's incidents, the opened one included.
func admitAlertAboutTriageRun(ctx context.Context, tx *sql.Tx, sessionID, runID, actor string, in GenericAlert, now time.Time) (Alert, []Incident, error) {
	incidents, err := triageRunIncidents(ctx, tx, sessionID, runID)
	if err != nil {
		return Alert{}, nil, err
	}
	pick := func(statuses ...string) string {
		for _, status := range statuses {
			for _, i := range incidents {
				if i.Status == status {
					return i.ID
				}
			}
		}
		return ""
	}
	about := pick("open", "claimed", "resolved")
	if in.Severity == "sos" {
		about = pick("open", "claimed")
	}
	alert, err := admitAlert(ctx, tx, "system", actor, "local:"+NewID(), about, in, nil, now)
	if err != nil {
		return Alert{}, nil, err
	}
	if about == "" && alert.IncidentID != "" {
		if sessionID != "" {
			if _, err = tx.ExecContext(ctx, `INSERT INTO triage_session_alerts(triage_session_id,alert_id,joined_at,sent_in_run_id) VALUES(?,?,?,?)`, sessionID, alert.ID, now.Unix(), runID); err != nil {
				return Alert{}, nil, err
			}
		}
		opened, err := readIncident(ctx, tx, alert.IncidentID)
		if err != nil {
			return Alert{}, nil, err
		}
		incidents = append(incidents, opened)
	}
	return alert, incidents, nil
}

// triageAgentReportTitle is the report's first line, usually its Markdown
// heading, as plain text cut at a word: a title is read in a notification,
// where Markdown shows as its marks and only a short line fits.
func triageAgentReportTitle(report string) string {
	if line := reportTitleLine(report); line != "" {
		return "The AI triage agent reports: " + line
	}
	return "The AI triage agent sent a report"
}

// reportTitleLine is the report's first line as plain text, cut for a
// title, or "" when the report has no text.
func reportTitleLine(report string) string {
	for _, line := range strings.Split(report, "\n") {
		if line = plainTitleText(line); line != "" {
			return cutAtWord(line, maximumReportTitleRunes)
		}
	}
	return ""
}

// maximumReportTitleRunes is how much of the report's first line its title
// keeps: about what a phone shows of a notification's title.
const maximumReportTitleRunes = 100

var (
	markdownLink       = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	markdownLineMarker = regexp.MustCompile(`^(#+|>|[-*+]|[0-9]+[.)])\s+`)
)

// plainTitleText is a Markdown line as plain text: without a heading, quote,
// or list mark, with a link's text in place of the link, and without the
// marks of bold, italics, code, and strike-through. Underscores stay, since
// names such as mmx_run_output_page hold them.
func plainTitleText(line string) string {
	line = strings.TrimSpace(line)
	for {
		trimmed := markdownLineMarker.ReplaceAllString(line, "")
		if trimmed == line {
			break
		}
		line = strings.TrimSpace(trimmed)
	}
	line = strings.TrimLeft(line, "#")
	line = markdownLink.ReplaceAllString(line, "$1")
	line = strings.NewReplacer("**", "", "*", "", "`", "", "~~", "").Replace(line)
	return strings.Join(strings.Fields(line), " ")
}

// cutAtWord cuts text to at most n runes, at the last space before the cut
// when there is one, and marks the cut with "…".
func cutAtWord(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	cut := string(runes[:n-1])
	if space := strings.LastIndex(cut, " "); space > len(cut)/2 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}

// maximumAlertMessageBytes is the limit validateAlert applies to a message.
const maximumAlertMessageBytes = 16 << 10

// triageAgentReportMessage fits a report of up to 64 KiB into an alert's
// message; the incidents' history and the run's requests keep the whole
// report.
func triageAgentReportMessage(report string) string {
	if len(report) <= maximumAlertMessageBytes {
		return report
	}
	const cut = "\n\n[The report goes on. MMHotline keeps the whole report.]"
	end := maximumAlertMessageBytes - len(cut)
	for end > 0 && !utf8.RuneStart(report[end]) {
		end--
	}
	return report[:end] + cut
}

// maximumReportSummaryRunes is about what a phone's lock screen shows of a
// notification.
const maximumReportSummaryRunes = 300

// reportParagraphBoundary matches a Markdown line that ends a paragraph: a
// heading, a list item, a code fence, a quote, or a table row.
var reportParagraphBoundary = regexp.MustCompile(`^(#{1,6}(\s|$)|[-*+](\s|$)|[0-9]+[.)]\s|` + "```" + `|~~~|>|.*\|)`)

// triageAgentReportNotificationMessage is what every notification of a
// report carries in place of the report (QueueNotification): the report's
// first paragraph after its first line, as plain text cut at a word, then
// where to read the rest. Evidence, such as journal lines with addresses and
// key fingerprints, stays on the pages, off lock screens and shared chats.
func triageAgentReportNotificationMessage(report string) string {
	const instruction = "Open MMHotline for the whole report."
	_, body, _ := strings.Cut(strings.TrimSpace(report), "\n")
	summary := firstProseParagraph(body)
	if summary == "" {
		return instruction
	}
	return summary + "\n\n" + instruction
}

// firstProseParagraph is a Markdown text's first paragraph, after any blank
// lines and headings, as plain text cut at a word to
// maximumReportSummaryRunes: the summary people read of a report or a
// diagnosis.
func firstProseParagraph(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var paragraph []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(paragraph) == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if line == "" || reportParagraphBoundary.MatchString(line) {
			break
		}
		paragraph = append(paragraph, line)
	}
	return cutAtWord(plainTitleText(strings.Join(paragraph, " ")), maximumReportSummaryRunes)
}
