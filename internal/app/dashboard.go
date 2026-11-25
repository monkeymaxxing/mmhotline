package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (a *App) pagePrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		a.browserAccessError(w, r, err)
		return p, false
	}
	return p, true
}

func displayTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
func memberNames(members []Member) map[string]string {
	names := map[string]string{"": "Not claimed yet"}
	for _, m := range members {
		names[m.ID] = m.Name
	}
	return names
}
func targetNames(ids []string, names map[string]string) string {
	var result []string
	for _, id := range ids {
		n := names[id]
		if n == "" {
			n = "Former member"
		}
		result = append(result, n)
	}
	if len(result) == 0 {
		return "No one available"
	}
	return strings.Join(result, ", ")
}

type incidentView struct {
	Incident
	Owner, Targets, Created, Due string
	// HeldForTriageAgent: nobody is contacted yet, and Due is when MMHotline
	// wakes the person on call.
	HeldForTriageAgent bool
}

func viewIncident(i Incident, names map[string]string) incidentView {
	v := incidentView{Incident: i, Owner: names[i.OwnerMemberID], Targets: targetNames(i.TargetMemberIDs, names), Created: displayTime(i.CreatedAt), Due: displayTime(i.Deadline)}
	if i.Step == triageAgentStep {
		// Claimed or resolved while it waited, it never contacted anyone.
		v.Targets = "No one"
		if i.Status == "open" {
			v.HeldForTriageAgent = true
			v.Targets = "Nobody yet: the triage agent is looking into the incident"
		}
	}
	return v
}

func (a *App) RegisterDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /dashboard", a.dashboard)
	mux.HandleFunc("GET /alerts", a.alertsPage)
	mux.HandleFunc("GET /alerts/{alert_id}", a.alertPage)
	mux.HandleFunc("GET /incidents/{id}", a.incidentPage)
	mux.HandleFunc("GET /assets/mmhotline.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		fmt.Fprint(w, stylesheet)
	})
	mux.HandleFunc("GET /assets/refresh.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		fmt.Fprint(w, refreshScript)
	})
	mux.HandleFunc("GET /assets/copy-question.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		fmt.Fprint(w, copyQuestionScript)
	})
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	view, err := a.readDashboard(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	downAlarm, err := a.DownAlarm(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	if downAlarm.IsSetUp {
		view.DownAlarm = &downAlarm
	}
	if view.TriageAgent, err = a.TriageAgentAtWorkNow(r.Context(), p); err != nil {
		HTTPError(w, err)
		return
	}
	// Administrators, who change the settings file, see each part that did
	// not apply.
	if view.administrator {
		state, err := a.SettingsFileState(r.Context())
		if err != nil {
			HTTPError(w, err)
			return
		}
		for _, part := range state.Parts {
			if part.State == "failed" {
				view.SettingsFailures = append(view.SettingsFailures, part)
			}
		}
	}
	view.CSRF, view.System, view.settingsFileNoticeView = a.CSRFToken(w, r), a.readSystemPanels(r.Context()), a.settingsFileNoticeView()
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "Dashboard", frame, dashboardHeading(frame, view), dashboardBody(frame, view))
}

// dashboardView is the dashboard as one member sees it
// (docs/changing-mmhotline/design/interface.md#dashboard).
type dashboardView struct {
	CSRF string
	// What needs a person: the SOS incidents nobody claimed, and the
	// approval requests and warned runs that wait; each oldest first.
	Waiting   []waitingIncidentView
	Approvals []waitingApprovalView
	// Handled are the claimed SOS incidents, oldest first.
	Handled []handledIncidentView
	// HeldWarns are the warnings MMHotline holds for the triage agent now.
	HeldWarns []HeldWarn
	// CommitsTheTriageAgentReviews are the waiting commits no alert has
	// gone out for yet: the triage agent reviews them, so they do not need
	// a person until the agent decides or its review cannot happen.
	CommitsTheTriageAgentReviews []CommitReview
	// LastSOS is the SOS resolved last, read only while nothing needs a
	// person and nothing is handled; nil for a team without one.
	LastSOS          *lastSOSView
	OnCall           onCallView
	TriageAgent      *TriageAgentAtWork
	DownAlarm        *DownAlarmView
	System           systemPanels
	SettingsFailures []SettingsFilePartOutcome
	settingsFileNoticeView
	administrator bool
}

// NeedsAPerson counts what waits for a person.
func (v dashboardView) NeedsAPerson() int { return len(v.Waiting) + len(v.Approvals) }

// waitingIncidentView is an SOS incident nobody claimed, with what MMHotline
// does next by itself.
type waitingIncidentView struct {
	ID, Title string
	Revision  int64
	CreatedAt time.Time
	// HeldForTriageAgent: nobody is woken yet. Waking names whom MMHotline
	// wakes now, the viewer as "you", while its step has a deadline; on the
	// last step the countdown says whom it keeps waking.
	HeldForTriageAgent bool
	Waking             string
	Countdown          incidentCountdown
}

// handledIncidentView is a claimed SOS incident.
type handledIncidentView struct {
	ID, Title            string
	CreatedAt, ClaimedAt time.Time
	// ClaimedBy is "You" or the person's name.
	ClaimedBy string
}

// waitingApprovalView is an approval request or a warned run that waits.
type waitingApprovalView struct {
	// Question and Where name it as every page does
	// (ApprovalRequestView.Question and Where).
	ID, Question, Where       string
	ReviewURL, Label, AskedBy string
	AskedAt                   time.Time
	// Warned: a warned run, which runs by itself at RunsAt unless someone
	// presses Stop while Stoppable, before then.
	Warned, Stoppable bool
	RunsAt            time.Time
	// Needs says whose approval an approval request still needs.
	Needs string
}

// lastSOSView is the SOS incident resolved last.
type lastSOSView struct {
	ID, Title  string
	ResolvedAt time.Time
	// ResolvedBy is "you", a person's or an alert source's name, the triage
	// agent, or MMHotline.
	ResolvedBy string
}

// onCallView is the On call panel for the member viewing it.
type onCallView struct {
	// Who is whom an SOS wakes first now: "You", a person's name, or, while
	// nobody is scheduled, "Everyone who is available", named in People.
	// Initial is the one person's first letter, for the round badge.
	Who, Initial, People string
	// Until is when someone else is on call, and Then who
	// (nextOnCallChange); zero when nothing changes within the schedule's
	// coming handovers.
	Until time.Time
	Then  string
	// Backup is whom MMHotline also wakes when nobody claims an SOS within
	// EscalateIfUnclaimedAfter; "" when there is nobody else.
	Backup, EscalateIfUnclaimedAfter string
	// Reason explains an exception, such as a person who took on-call.
	Reason string
	// MutedUntil is the end of the team mute; zero while the team is not
	// muted.
	MutedUntil time.Time
}

// readDashboard reads, in one transaction, what needs a person, what is
// being handled, and who is on call, as member p sees them.
func (a *App) readDashboard(ctx context.Context, p Principal) (dashboardView, error) {
	var view dashboardView
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		view.administrator = m.Role == RoleAdministrator
		zone, err := teamTimeZone(ctx, tx)
		if err != nil {
			return err
		}
		// Within one second, the opening alerts' order says which is older.
		rows, err := tx.QueryContext(ctx, `SELECT `+incidentColumns+` FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.status!='resolved' ORDER BY i.created_at,e.sequence`)
		if err != nil {
			return err
		}
		var incidents []Incident
		for rows.Next() {
			i, err := scanIncident(rows)
			if err != nil {
				rows.Close()
				return err
			}
			incidents = append(incidents, i)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, i := range incidents {
			if i.Status == "claimed" {
				claimedBy, err := youOrName(ctx, tx, i.OwnerMemberID, m.ID)
				if err != nil {
					return err
				}
				view.Handled = append(view.Handled, handledIncidentView{ID: i.ID, Title: i.Title, CreatedAt: i.CreatedAt, ClaimedAt: i.ClaimedAt, ClaimedBy: capitalize(claimedBy)})
				continue
			}
			step, err := nextIncidentStep(ctx, tx, i)
			if err != nil {
				return err
			}
			waiting := waitingIncidentView{ID: i.ID, Title: i.Title, Revision: i.Revision, CreatedAt: i.CreatedAt, HeldForTriageAgent: step.HeldForTriageAgent, Countdown: viewIncidentCountdown(step, m.ID, zone, now)}
			if !step.HeldForTriageAgent && !step.At.IsZero() {
				wakeable, _, err := wakeableMembers(ctx, tx, i.TargetMemberIDs)
				if err != nil {
					return err
				}
				people, err := stepPeople(ctx, tx, wakeable)
				if err != nil {
					return err
				}
				waiting.Waking = peopleText(people, m.ID)
			}
			view.Waiting = append(view.Waiting, waiting)
		}
		requests, err := approvalRequestViews(ctx, tx, m, now, `state=? ORDER BY judged_at,id`, approvalOpen)
		if err != nil {
			return err
		}
		for _, r := range requests {
			view.Approvals = append(view.Approvals, viewWaitingApproval(r))
		}
		commits, err := readCommitReviews(ctx, tx, `c.reviewed_at IS NULL ORDER BY c.found_at,c.commit_id`)
		if err != nil {
			return err
		}
		for _, commit := range commits {
			if commit.AlertSent == "" {
				view.CommitsTheTriageAgentReviews = append(view.CommitsTheTriageAgentReviews, commit)
				continue
			}
			view.Approvals = append(view.Approvals, waitingApprovalView{Question: commit.Question(), Where: "MMX runs wait for a person's review of commit " + commit.ShortID() + ", " + commit.Reason() + ".", ReviewURL: commit.URL(), Label: "Commit review", AskedBy: "MMHotline found the commit", AskedAt: commit.FoundAt, Needs: "Any team member may review the commit."})
		}
		slices.SortStableFunc(view.Approvals, func(x, y waitingApprovalView) int { return x.AskedAt.Compare(y.AskedAt) })
		if view.NeedsAPerson() == 0 && len(view.Handled) == 0 {
			if view.LastSOS, err = readLastSOS(ctx, tx, m.ID); err != nil {
				return err
			}
		}
		if view.HeldWarns, err = readHeldWarns(ctx, tx, `h.ended_at=0`); err != nil {
			return err
		}
		view.OnCall, err = readOnCall(ctx, tx, m.ID, now)
		return err
	})
	return view, err
}

// stepPeople names the members whose IDs are ids, for a sentence.
func stepPeople(ctx context.Context, tx *sql.Tx, ids []string) ([]stepPerson, error) {
	people := make([]stepPerson, 0, len(ids))
	for _, id := range ids {
		name, err := incidentMemberName(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		people = append(people, stepPerson{id, name})
	}
	return people, nil
}

// youOrName names member id to the viewer: "you" for the viewer.
func youOrName(ctx context.Context, tx *sql.Tx, id, viewerID string) (string, error) {
	if id == viewerID {
		return "you", nil
	}
	return incidentMemberName(ctx, tx, id)
}

// viewWaitingApproval is the dashboard's line for an approval request or a
// warned run that waits.
func viewWaitingApproval(r ApprovalRequestView) waitingApprovalView {
	v := waitingApprovalView{ID: r.ID, ReviewURL: "/approvals/" + r.ID, Label: "Approval", AskedBy: "The triage agent asked", Question: r.Question(), Where: r.Where(), AskedAt: r.request.JudgedAt, Warned: r.Warned, Stoppable: r.Stoppable, RunsAt: r.request.runsAt()}
	switch {
	case r.ApprovedByYou:
		v.Needs = "You approved the playbook run. The playbook run needs approval from another person."
	case len(r.countedApprovers()) > 0:
		v.Needs = "The playbook run needs approval from 1 more person."
	default:
		v.Needs = approvalsNeededText(r.ApprovalsNeeded)
	}
	return v
}

// readLastSOS reads the SOS incident resolved last, and who resolved it.
func readLastSOS(ctx context.Context, tx *sql.Tx, viewerID string) (*lastSOSView, error) {
	var last lastSOSView
	var resolvedAt int64
	var actor string
	err := tx.QueryRowContext(ctx, `SELECT i.id,e.title,i.resolved_at,coalesce((SELECT actor FROM incident_actions WHERE incident_id=i.id AND kind='resolved' ORDER BY id DESC LIMIT 1),'') FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.status='resolved' ORDER BY i.resolved_at DESC,e.sequence DESC LIMIT 1`).Scan(&last.ID, &last.Title, &resolvedAt, &actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	last.ResolvedAt = time.Unix(resolvedAt, 0).UTC()
	if actor == "member:"+viewerID {
		last.ResolvedBy = "you"
	} else if last.ResolvedBy, err = incidentActorName(ctx, tx, actor); err != nil {
		return nil, err
	}
	return &last, nil
}

// readOnCall reads the On call panel for the viewer at now, from the
// coverage, as the escalation uses it: the first people an SOS wakes, and
// the backup whom it also wakes after the time to claim.
func readOnCall(ctx context.Context, tx *sql.Tx, viewerID string, now time.Time) (onCallView, error) {
	coverage, err := CoverageAt(ctx, tx, now)
	if err != nil {
		return onCallView{}, err
	}
	window, err := escalateIfUnclaimedAfter(ctx, tx)
	if err != nil {
		return onCallView{}, err
	}
	view := onCallView{EscalateIfUnclaimedAfter: durationText(window), Reason: coverage.Reason}
	if coverage.TeamMute.Active {
		view.MutedUntil = coverage.TeamMute.Until
	}
	first, err := stepPeople(ctx, tx, coverage.PrimaryIDs)
	if err != nil {
		return view, err
	}
	switch {
	case coverage.PrimaryIsEveryoneAvailable && len(first) == 0:
		view.Who = "Nobody is available"
	case coverage.PrimaryIsEveryoneAvailable:
		view.Who, view.People = "Everyone who is available", capitalize(peopleText(first, viewerID))
	default:
		view.Who = capitalize(peopleText(first, viewerID))
		if name := []rune(first[0].Name); len(first) == 1 && len(name) > 0 {
			view.Initial = strings.ToUpper(string(name[:1]))
		}
	}
	if !coverage.PrimaryIsEveryoneAvailable && len(coverage.BackupIDs) > 0 {
		view.Backup = "everyone else who is available"
		if !coverage.BackupIsEveryoneAvailable {
			backup, err := stepPeople(ctx, tx, coverage.BackupIDs)
			if err != nil {
				return view, err
			}
			view.Backup = peopleText(backup, viewerID)
		}
	}
	until, then, err := nextOnCallChange(ctx, tx, now, coverage)
	if err != nil || until.IsZero() {
		return view, err
	}
	view.Until, view.Then = until, "everyone who is available"
	if !then.PrimaryIsEveryoneAvailable {
		next, err := stepPeople(ctx, tx, then.PrimaryIDs)
		if err != nil {
			return view, err
		}
		view.Then = peopleText(next, viewerID)
	}
	return view, nil
}

// alertView adds the plain-English display title, triage agent summary,
// incident name, and alert effect for the history and detail pages. Stored
// alerts and the incident remain the source of truth.
type alertView struct {
	Alert
	DisplayTitle           string
	IncidentTitle, Outcome string
	TriageSummary          *triageSummary
	// HeldWarn is what the triage agent's wait did to a warn, nil when the
	// warn was not held.
	HeldWarn *HeldWarn
}

// ReceivedLater says how late the alert arrived, when that matters.
func (v alertView) ReceivedLater() string { return receivedLater(v.HappenedAt, v.ReceivedAt) }

func (a *App) viewAlert(ctx context.Context, p Principal, e Alert, summary *triageSummary) (alertView, error) {
	v := alertView{
		Alert:         e,
		DisplayTitle:  displayAlertTitle(e, summary),
		Outcome:       "No SOS incident",
		TriageSummary: summary,
	}
	if e.IncidentID == "" {
		if e.Severity == "resolved-sos" {
			v.Outcome = "No unresolved SOS incident matched the sender incident key. Nobody was notified."
		}
		return v, nil
	}
	i, err := a.GetIncident(ctx, p, e.IncidentID)
	if err != nil {
		return v, err
	}
	v.IncidentTitle, v.Outcome = i.Title, "Related SOS incident"
	switch e.Severity {
	case "sos":
		v.Outcome = "Joined SOS incident"
		if e.ID == i.OpeningAlertID {
			v.Outcome = "Opened SOS incident"
		}
	case "resolved-sos":
		v.Outcome = "Resolved SOS incident"
	}
	return v, nil
}

// alertsPageData is the alert history: the filter as typed, the alerts, and
// the link to older alerts ("" on the last page).
type alertsPageData struct {
	Query, Severity, From, Until, Next string
	Alerts                             []alertView
}

func (a *App) alertsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	zone, err := a.alertPageTimeZone(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	q := r.URL.Query()
	filter := AlertFilter{Query: q.Get("q"), Severity: q.Get("severity"), Limit: 30}
	if q.Get("before") != "" {
		v, e := strconv.ParseInt(q.Get("before"), 10, 64)
		if e != nil || v < 1 {
			HTTPError(w, ErrInvalid)
			return
		}
		filter.Before = v
	}
	for key, dst := range map[string]*time.Time{"from": &filter.From, "until": &filter.Until} {
		if q.Get(key) != "" {
			v, e := time.ParseInLocation("2006-01-02", q.Get(key), zone)
			if e != nil {
				HTTPError(w, fmt.Errorf("%w: invalid date", ErrInvalid))
				return
			}
			if key == "until" {
				v = v.AddDate(0, 0, 1)
			}
			*dst = v
		}
	}
	alerts, err := a.ListAlerts(r.Context(), p, filter)
	if err != nil {
		HTTPError(w, err)
		return
	}
	summaries, err := a.triageSummariesForAlerts(r.Context(), p, alerts)
	if err != nil {
		HTTPError(w, err)
		return
	}
	data := alertsPageData{Query: filter.Query, Severity: filter.Severity, From: q.Get("from"), Until: q.Get("until")}
	for _, e := range alerts {
		v, err := a.viewAlert(r.Context(), p, e, summaries[e.ID])
		if err != nil {
			HTTPError(w, err)
			return
		}
		data.Alerts = append(data.Alerts, v)
	}
	if len(alerts) == 30 {
		q.Set("before", strconv.FormatInt(alerts[len(alerts)-1].Sequence, 10))
		data.Next = "/alerts?" + q.Encode()
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "Alert history", frame, nil, alertsBody(frame, data))
}

func (a *App) alertPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	e, err := a.GetAlert(r.Context(), p, r.PathValue("alert_id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	summary, err := a.triageSummaryForAlert(r.Context(), p, e)
	if err != nil {
		HTTPError(w, err)
		return
	}
	view, err := a.viewAlert(r.Context(), p, e, summary)
	if err != nil {
		HTTPError(w, err)
		return
	}
	if view.HeldWarn, err = a.heldWarnOfAlert(r.Context(), p, e.ID); err != nil {
		HTTPError(w, err)
		return
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, view.DisplayTitle, frame, alertHeading(view.DisplayTitle, view.Title), alertBody(frame, view))
}

// Refresh only live status sections, [data-live][id], retaining active forms
// and their inputs: a section with focus, with text a person typed, such as
// a note, or with a folded part a person opened, such as the incident page's
// More, stays. Only a section whose HTML from the server changed is
// replaced, so an unchanged one keeps its open details, and an alert in it,
// such as the red bar, is not announced again. Revision-checked mutations
// remain authoritative if a user submits stale state.
const refreshScript = `"use strict";const served=new Map();document.querySelectorAll("[data-live][id]").forEach(section=>served.set(section.id,section.outerHTML));setInterval(async()=>{if(document.hidden)return;try{const response=await fetch(location.href,{cache:"no-store"});if(!response.ok||response.redirected)return;const next=new DOMParser().parseFromString(await response.text(),"text/html");document.querySelectorAll("[data-live][id]").forEach(current=>{if(current.contains(document.activeElement)||current.querySelector("details[open]")||[...current.querySelectorAll("input,textarea")].some(field=>field.value!==field.defaultValue))return;const replacement=next.getElementById(current.id);if(!replacement||replacement.outerHTML===served.get(current.id))return;served.set(current.id,replacement.outerHTML);current.replaceWith(replacement);});}catch(_){}},10000);`

func incidentActionLabel(kind string) string {
	switch kind {
	case "opened":
		return "Opened"
	case "sos_escalation_due":
		return "Contacting"
	case "claimed":
		return "Claimed"
	case "reclaimed":
		return "Taken over"
	case "escalated":
		return "Escalated"
	case "could_not_wake":
		return "Could not wake"
	case "resolved":
		return "Resolved"
	case "related_alert":
		return "Related alert received"
	case "related_alert_sent_to_claimer":
		return "Related alert sent to the person who claimed the incident"
	case "triage_agent_report":
		return "Triage agent report"
	case "triage_agent_could_not_run":
		return "Triage agent could not run"
	case "triage_agent_stopped":
		return "Triage agent stopped"
	case "held_for_triage_agent":
		return "Held for the triage agent"
	case heldForAServerReplacement:
		return "Held while the triage agent replaces a server"
	case "triage_agent_hold_ended":
		return "Waking the person on call"
	case runLimitSOSWokeKind:
		return "Woken without the agent's wait"
	case "already_woken_for_triage_session":
		return "Already woken for the triage session"
	case "playbook_run_refused_by_the_review_agent":
		return "Review agent refused a playbook run"
	case "playbook_run_approval_asked":
		return "Approval needed"
	case "playbook_run_approved":
		return "Playbook run approved"
	case "playbook_run_refused":
		return "Playbook run refused"
	case "playbook_run_covered_by_an_earlier_approval":
		return "Playbook run covered by an earlier approval"
	case "approved_playbook_run_result":
		return "Approved playbook run"
	case "playbook_run_warning_sent":
		return "Playbook run warning"
	case "playbook_run_stopped":
		return "Playbook run stopped"
	case "warned_playbook_run_result":
		return "Warned playbook run"
	case "playbook_run_result_not_sent_to_triage_agent":
		return "Result not sent to the triage agent"
	}
	return "Incident updated"
}
