package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mmhotline/internal/systemsrepository"
)

// The triage agent at work, as people see it:
// docs/changing-mmhotline/design/incidents.md#the-triage-agent-on-the-incident-page and
// docs/changing-mmhotline/design/interface.md#dashboard.
//
//   - The live view: a session's phase, what it looks into and since when,
//     the steps of its run as they happen, and "Stop after the current step",
//     which takes the session over from the agent
//     (triage_session_take_over.go). One section shows it on each incident
//     page of the session and on the session's own page.
//   - The session's own page, /triage-sessions/{triage_session_id}, which
//     also lists its alerts with their SOS incidents, its runs, and its
//     reports. A session of warn alerts only has no incident, so this page is
//     the only one where people see and stop it.
//   - The dashboard's part: the open session, what the agent works on, since
//     when, and its latest step, with a link to the session's page.

// maximumLiveViewSteps bounds the steps the live view shows, the latest
// ones. The page refreshes every 10 seconds, and 50 steps of at most 2,000
// characters keep each refresh near 100 KB; the coding agent's session
// files keep the whole transcript.
const maximumLiveViewSteps = 50

// maximumDashboardStepCharacters cuts the latest step on the dashboard,
// which shows it at a glance; the session's page shows it whole.
const maximumDashboardStepCharacters = 300

// TriageSessionLiveView is a triage session as its live view shows it: the
// phase, what it looks into and since when, who took it over, and its run
// that is running, or else its latest run.
type TriageSessionLiveView struct {
	SessionID, Phase, PhaseText string
	// About names the session's first alert and how many more joined it.
	About string
	// Started and Ended are the session's times, which the incident page
	// shows in the team's time zone; Ended is zero while it is open.
	// EndedBecause says why it ended.
	Started, Ended time.Time
	EndedBecause   string
	// TakenOverName and TakenOverAt say who pressed Stop and when, if anyone.
	TakenOverName string
	TakenOverAt   time.Time
	Run           *TriageSessionLiveRun
}

// TriageSessionLiveRun is one run of a session with its latest steps.
type TriageSessionLiveRun struct {
	Title, StateLabel, FailureReason string
	Started, Ended                   time.Time
	Steps                            []TriageAgentRunStep
	// EarlierSteps counts the steps before those shown.
	EarlierSteps int
	// WaitingForSystemsRepository says, while the run's start waits for the
	// systems repository copy's lock, who holds the lock; "" otherwise.
	WaitingForSystemsRepository string
	id                          string
}

// Stoppable says whether a member may still press Stop: the session is
// open and nobody took it over.
func (v TriageSessionLiveView) Stoppable() bool {
	return v.Phase == triageSessionWorking || v.Phase == triageSessionWaiting
}

// StopButton names the Stop for the phase: with no run going there is no
// current step to finish.
func (v TriageSessionLiveView) StopButton() string {
	if v.Phase == triageSessionWaiting {
		return "Stop the triage agent"
	}
	return "Stop after the current step"
}

// IncidentTriageSession returns the incident's triage session for its page:
// the session its newest alert joined, or nil when none of its alerts did.
func (a *App) IncidentTriageSession(ctx context.Context, p Principal, incidentID string) (*TriageSessionLiveView, error) {
	var view *TriageSessionLiveView
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var sessionID string
		err := tx.QueryRowContext(ctx, `SELECT s.triage_session_id FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE e.incident_id=? ORDER BY e.sequence DESC LIMIT 1`, incidentID).Scan(&sessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		view, err = a.readTriageSessionLiveView(ctx, tx, sessionID, maximumLiveViewSteps)
		return err
	})
	return view, err
}

// phaseText says the session's phase for people, as the incident page and
// the mmhotline command show it.
func (s triageSessionState) phaseText() string {
	switch s.phase() {
	case triageSessionWorking:
		return "Working: the triage agent is running. The run steps appear below as the steps happen."
	case triageSessionWaiting:
		return "Waiting: the triage agent is not running. MMHotline resumes the triage agent when another alert or the result of a playbook run arrives."
	case triageSessionStopping:
		return "Stopping: the triage agent is finishing the current step."
	case triageSessionHandingOver:
		return "Handing over: the triage agent writes a report and a note about what the triage agent found, did, and was about to do. MMHotline refuses the triage agent's new playbook-run requests during the hand-over."
	}
	return "The triage session ended at " + displayTime(s.EndedAt) + ": " + s.EndedBecause
}

// readTriageSessionLiveView reads the session's live view with the latest
// steps of its run, at most steps of them; ErrNotFound when there is no such
// session.
func (a *App) readTriageSessionLiveView(ctx context.Context, tx *sql.Tx, sessionID string, steps int) (*TriageSessionLiveView, error) {
	s, err := readTriageSessionState(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	view := &TriageSessionLiveView{SessionID: s.ID, Phase: s.phase(), PhaseText: s.phaseText(), Started: s.StartedAt, Ended: s.EndedAt, EndedBecause: s.EndedBecause}
	var firstTitle string
	var alerts int
	if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT e.title FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE s.triage_session_id=?1 ORDER BY e.sequence LIMIT 1),''),(SELECT count(*) FROM triage_session_alerts WHERE triage_session_id=?1)`, s.ID).Scan(&firstTitle, &alerts); err != nil {
		return nil, err
	}
	view.About = humanizeAlertTitle(firstTitle)
	switch {
	case alerts == 2:
		view.About += ", and 1 more alert"
	case alerts > 2:
		view.About += ", and " + strconv.Itoa(alerts-1) + " more alerts"
	}
	if s.TakenOverByMemberID != "" {
		if view.TakenOverName, err = incidentMemberName(ctx, tx, s.TakenOverByMemberID); err != nil {
			return nil, err
		}
		view.TakenOverAt = s.TakenOverAt
	}
	if view.Run, err = triageSessionLiveRun(ctx, tx, s, steps); view.Run != nil {
		view.Run.WaitingForSystemsRepository = a.copyLockWaitText(view.Run.id)
	}
	return view, err
}

// copyLockWaitText is the line the session's page shows while the start of
// run runID waits for the systems repository copy's lock, with the line the
// lock's holder wrote, such as an MMX run's; "" when it does not wait.
func (a *App) copyLockWaitText(runID string) string {
	a.triageAgent.mu.Lock()
	run := a.triageAgent.activeRun
	waiting := run != nil && run.id == runID && run.waitingForCopyLock
	a.triageAgent.mu.Unlock()
	if !waiting {
		return ""
	}
	holder := systemsrepository.CopyLockHolder(filepath.Join(a.Store.Dir, systemsrepository.DirectoryName))
	if holder == "" {
		return "Waiting for the systems repository, which another process is using."
	}
	return "Waiting for the systems repository, which MMX is using: " + holder
}

// triageSessionLiveRun reads the session's run that is running, or else its
// latest run, with at most steps of its latest steps; nil when it has none
// yet.
func triageSessionLiveRun(ctx context.Context, tx *sql.Tx, s triageSessionState, steps int) (*TriageSessionLiveRun, error) {
	var id, state, reason string
	var started, ended int64
	// The reason shows for a failed run; a stopped run's only repeats its state.
	err := tx.QueryRowContext(ctx, `SELECT id,state,CASE WHEN state='failed' THEN failure_reason ELSE '' END,started_at,ended_at FROM triage_agent_runs WHERE triage_session_id=? ORDER BY state='running' DESC,started_at DESC,rowid DESC LIMIT 1`, s.ID).Scan(&id, &state, &reason, &started, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run := &TriageSessionLiveRun{id: id, Started: time.Unix(started, 0), Title: "Latest run", StateLabel: triageSessionRunStateLabel(state), FailureReason: reason}
	if id == s.HandOverRunID {
		run.Title = "Hand-over run"
	}
	if ended != 0 {
		run.Ended = time.Unix(ended, 0)
	}
	run.Steps, run.EarlierSteps, err = triageAgentRunSteps(ctx, tx, id, steps)
	return run, err
}

func triageSessionRunStateLabel(state string) string {
	switch state {
	case "running":
		return "Running"
	case "finished":
		return "Finished"
	case "stopped":
		return "Stopped after the current step"
	}
	return "Failed"
}

// TriageSessionPage is a triage session's own page: its live view, its
// alerts with their SOS incidents, its runs, and its reports.
type TriageSessionPage struct {
	TriageSession *TriageSessionLiveView
	Alerts        []TriageSessionPageAlert
	Runs          []TriageSessionPageRun
	Reports       []TriageSessionReport
}

// TriageSessionPageAlert is a row of the page's alerts: an alert that joined
// the session, with its SOS incident; a warning has none.
type TriageSessionPageAlert struct {
	ID, Title, Severity                       string
	ReceivedAt                                time.Time
	IncidentID, IncidentTitle, IncidentStatus string
}

// TriageSessionPageRun is a row of the page's runs, oldest first.
type TriageSessionPageRun struct {
	Title, StateLabel, FailureReason string
	StartedAt, EndedAt               time.Time
	Steps                            int
}

// TriageSessionReport is a report the agent sent to people during a run of
// the session.
type TriageSessionReport struct {
	RunTitle, Severity, Text string
	SentAt                   time.Time
}

// TriageSessionPageFor reads a triage session's own page for a team member,
// from the same record that `mmhotline triage-sessions show` prints.
func (a *App) TriageSessionPageFor(ctx context.Context, p Principal, sessionID string) (TriageSessionPage, error) {
	var page TriageSessionPage
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		if page.TriageSession, err = a.readTriageSessionLiveView(ctx, tx, sessionID, maximumLiveViewSteps); err != nil {
			return err
		}
		session, err := readTriageSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		incidents := map[string]Incident{}
		for _, incident := range session.Incidents {
			incidents[incident.ID] = incident
		}
		for _, alert := range session.Alerts {
			row := TriageSessionPageAlert{ID: alert.ID, Title: humanizeAlertTitle(alert.Title), Severity: alert.Severity, ReceivedAt: alert.ReceivedAt, IncidentID: alert.IncidentID}
			if incident, ok := incidents[alert.IncidentID]; ok {
				row.IncidentTitle, row.IncidentStatus = humanizeAlertTitle(incident.Title), incident.Status
			}
			page.Alerts = append(page.Alerts, row)
		}
		runTitles := map[string]string{}
		for index, run := range session.Runs {
			row := TriageSessionPageRun{Title: "Run " + strconv.Itoa(index+1), StateLabel: triageSessionRunStateLabel(run.State), StartedAt: run.StartedAt, Steps: len(run.Steps) + run.EarlierStepsNotShown}
			if run.ID == session.HandOverRunID {
				row.Title = "Hand-over run"
			}
			// The reason shows for a failed run; a stopped run's only repeats its state.
			if run.State == "failed" {
				row.FailureReason = run.FailureReason
			}
			if run.EndedAt != nil {
				row.EndedAt = *run.EndedAt
			}
			runTitles[run.ID] = row.Title
			page.Runs = append(page.Runs, row)
		}
		page.Reports, err = triageSessionReports(ctx, tx, sessionID, runTitles)
		return err
	})
	return page, err
}

// triageSessionReports lists the reports the session's runs sent to people,
// oldest first; a refused report reached nobody.
func triageSessionReports(ctx context.Context, tx *sql.Tx, sessionID string, runTitles map[string]string) ([]TriageSessionReport, error) {
	rows, err := tx.QueryContext(ctx, `SELECT q.run_id,q.requested_at,q.alert_severity,q.report_text FROM triage_agent_requests q JOIN triage_agent_runs r ON r.id=q.run_id WHERE r.triage_session_id=? AND q.kind='report' AND q.outcome='recorded' ORDER BY q.requested_at,q.rowid`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []TriageSessionReport
	for rows.Next() {
		var report TriageSessionReport
		var runID string
		var at int64
		if err = rows.Scan(&runID, &at, &report.Severity, &report.Text); err != nil {
			return nil, err
		}
		report.RunTitle, report.SentAt = runTitles[runID], time.Unix(at, 0)
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

// TriageAgentAtWork is the dashboard's part for the triage agent: the open
// session with its latest step, or else the latest session that ended.
type TriageAgentAtWork struct {
	Session *TriageSessionLiveView
	// For the open session: when it started, its phase in a word or two,
	// such as "Working", and the steps of its running or latest run, with
	// the latest step cut to maximumDashboardStepCharacters; LatestStep is
	// nil before the first step.
	SessionStartedAt time.Time
	Phase            string
	Steps            int
	LatestStep       *TriageAgentRunStep
	// With no session open, the session that ended last; "" for none.
	LastSessionID      string
	LastSessionEndedAt time.Time
}

// TriageAgentAtWorkNow reads the dashboard's part for the triage agent; nil
// when the triage agent is not set up and no session is open, as for a team
// without it.
func (a *App) TriageAgentAtWorkNow(ctx context.Context, p Principal) (*TriageAgentAtWork, error) {
	var part *TriageAgentAtWork
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var setUp bool
		var openID, lastID string
		var openStarted, lastEnded int64
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup),coalesce((SELECT id FROM triage_sessions WHERE ended_at=0),''),coalesce((SELECT started_at FROM triage_sessions WHERE ended_at=0),0),coalesce((SELECT id FROM triage_sessions WHERE ended_at>0 ORDER BY ended_at DESC,rowid DESC LIMIT 1),''),coalesce((SELECT max(ended_at) FROM triage_sessions),0)`).Scan(&setUp, &openID, &openStarted, &lastID, &lastEnded); err != nil {
			return err
		}
		if !setUp && openID == "" {
			return nil
		}
		part = &TriageAgentAtWork{}
		if openID == "" {
			if lastID != "" {
				part.LastSessionID, part.LastSessionEndedAt = lastID, time.Unix(lastEnded, 0).UTC()
			}
			return nil
		}
		var err error
		if part.Session, err = a.readTriageSessionLiveView(ctx, tx, openID, 1); err != nil {
			return err
		}
		part.SessionStartedAt = time.Unix(openStarted, 0).UTC()
		part.Phase, _, _ = strings.Cut(part.Session.PhaseText, ":")
		if run := part.Session.Run; run != nil && len(run.Steps) > 0 {
			latest := run.Steps[len(run.Steps)-1]
			latest.Text = truncateRunes(latest.Text, maximumDashboardStepCharacters)
			part.Steps, part.LatestStep = run.EarlierSteps+len(run.Steps), &latest
		}
		return nil
	})
	return part, err
}

func (a *App) RegisterTriageSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /triage-sessions/{triage_session_id}", a.triageSessionPage)
	mux.HandleFunc("POST /triage-sessions/{triage_session_id}/stop", a.takeOverTriageSessionHTTP)
}

func (a *App) triageSessionPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	page, err := a.TriageSessionPageFor(r.Context(), p, r.PathValue("triage_session_id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	title := "Triage session"
	if page.TriageSession.About != "" {
		title += ": " + page.TriageSession.About
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, title, frame, triageSessionHeading(title), triageSessionBody(frame, page, a.CSRFToken(w, r)))
}

// takeOverTriageSessionHTTP is the live view's Stop: it takes the session
// over through the one operation, then returns to the incident the form was
// on, or to the session's own page.
func (a *App) takeOverTriageSessionHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	sessionID := r.PathValue("triage_session_id")
	if err := a.TakeOverTriageSession(r.Context(), p, sessionID); err != nil {
		HTTPError(w, err)
		return
	}
	target := "/triage-sessions/" + sessionID + "#triage-agent"
	if incidentID := r.PostForm.Get("return_to_incident_id"); recordIDPattern.MatchString(incidentID) {
		target = "/incidents/" + incidentID + "#triage-agent"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// PageKind names a recorded step without exposing the coding agent's value.
func (s TriageAgentRunStep) PageKind() string {
	switch s.Kind {
	case "command":
		return "Command"
	case "message":
		return "Agent message"
	case "tool":
		return "Tool use"
	}
	return "Agent step"
}

// PageEndReason avoids repeating the take-over's UTC timestamp in page text.
func (v TriageSessionLiveView) PageEndReason() string {
	if v.TakenOverName != "" {
		if strings.Contains(v.EndedBecause, "before the triage agent began") {
			return "The triage agent never began, so there was nothing to hand over."
		}
		return "A person took over: " + v.TakenOverName + " stopped the triage agent."
	}
	return v.EndedBecause
}
