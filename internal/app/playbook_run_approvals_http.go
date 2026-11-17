package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"mmhotline/internal/actionkinds"
)

// The Approvals page, each approval request's own page, and the approval
// card on the incident page: docs/changing-mmhotline/design/triage-agent.md#approvals. A warned
// run has a page and a card too, with its run time and Stop.

// maximumDecidedApprovalRequestsShown bounds the decided requests the
// Approvals page lists below the open ones; the incidents keep the rest.
const maximumDecidedApprovalRequestsShown = 20

// ApprovalRequestView is one approval request or warned run as its page and
// the card show it: what MMHotline judged, who decided or stopped it, and
// what happened.
type ApprovalRequestView struct {
	ID, Playbook, Inventory, CommitID, PlaybookText, IncidentID string
	// Title is the playbook in plain words, from its file name: “Reboot
	// app002”. Servers names the servers it can change, as the alert's
	// title does: "app001 and app002"; empty when MMX skips every task.
	Title, Servers string
	// IncidentTitle is the title of the incident its alert went with.
	IncidentTitle     string
	Kinds             []string
	UnrecognizedTasks []actionkinds.Unrecognized
	// TasksOnServicesWhoseChangesAPersonMustApprove are the node tasks on the list of
	// services whose changes a person must approve as the list is now.
	TasksOnServicesWhoseChangesAPersonMustApprove []taskOnAServiceWhoseChangesAPersonMustApprove
	Tasks                                         []approvalTaskView
	RuleTitle, Review                             string
	// ListedPlaybookRule says the rule is the one a person listed for the
	// playbook by name; empty when the rule came from the playbook's kinds.
	ListedPlaybookRule string
	// AlsoApproves says what else approving lets the triage agent run in
	// the session (alsoApprovesText); CoveredBy says which earlier approval
	// covered this run, and who approved it (coveredByText). Each is empty
	// otherwise.
	AlsoApproves, CoveredBy string
	// ReviewOutcome is the review agent's: approved, refused, or
	// could_not_review; empty when no review ran.
	ReviewOutcome string
	// Diagnosis is why the triage agent asks: what it says it found, the
	// cause, the evidence, and why the playbook fixes it.
	Diagnosis string
	// EarlierVersionsRefused are the versions of the fix the review agent
	// refused in a row before the review that made this request wait,
	// oldest first; empty unless a review made it wait.
	EarlierVersionsRefused           []refusedVersion
	ApprovalsNeeded                  int
	Decisions                        []approvalDecisionView
	ApprovedByYou                    bool
	State, StateLabel                string
	ResultMessage, ResultForTheAgent string
	// Warned: a warned run, which runs at RunsAt unless someone presses Stop
	// while Stoppable, before then; RunsIn counts down to it: "Runs in 7
	// minutes".
	Warned, Stoppable     bool
	RunsAt, RunsIn        string
	StoppedBy, StopReason string
	// request is the row the page shows, which the mmhotline command prints
	// with its times and values as stored.
	request approvalRequest
}

// approvalTaskView is one node task; Kind is its kind of action's title, such
// as "Reboot a server".
type approvalTaskView struct {
	Path, Node, Module, Status, Kind string
	Arguments                        []string
}

type approvalDecisionView struct {
	MemberName, Decision, Reason string
	// decision, approve or refuse, and decidedAt are as stored.
	decision  string
	decidedAt time.Time
	// memberRemoved: the member was removed from the team since, so an
	// approval no longer counts toward an open request.
	memberRemoved bool
}

// countedApprovers names the members whose approvals count, first first.
func (v ApprovalRequestView) countedApprovers() []string {
	var names []string
	for _, d := range v.Decisions {
		if d.decision == "approve" && !d.memberRemoved {
			names = append(names, d.MemberName)
		}
	}
	return names
}

// removedApproversText says, for each approval of a member removed since,
// that it no longer counts, each sentence followed by a space.
func (v ApprovalRequestView) removedApproversText() string {
	var text string
	for _, d := range v.Decisions {
		if d.decision == "approve" && d.memberRemoved {
			text += removedApproverText(d.MemberName) + " "
		}
	}
	return text
}

// Open says whether people can still approve or refuse it, or a warned run
// still waits for its run time.
func (v ApprovalRequestView) Open() bool { return v.State == approvalOpen }

// Question is what the person decides, as the page's heading asks it: Run
// “Reboot app002” on app002? Without a server it names the inventory.
func (v ApprovalRequestView) Question() string {
	where := v.Servers
	if where == "" {
		where = inventoryText(v.Inventory)
	}
	return "Run “" + v.Title + "” on " + where + "?"
}

// Where says what it can change, and in which inventory, as every page says
// it under the Question: "It can change app002, in the inventory
// “production”."
func (v ApprovalRequestView) Where() string {
	changes := "MMX lists no server the playbook changes"
	if v.Servers != "" {
		changes = "The playbook can change " + v.Servers
	}
	return changes + ", in " + inventoryText(v.Inventory) + "."
}

// AskedAt is when MMHotline judged the playbook, asked, or warned.
func (v ApprovalRequestView) AskedAt() time.Time { return v.request.JudgedAt }

// ChangedAt is when its state last changed.
func (v ApprovalRequestView) ChangedAt() time.Time { return v.request.StateChangedAt }

// RunTime is a warned run's run time.
func (v ApprovalRequestView) RunTime() time.Time { return v.request.runsAt() }

// FromSoftwareReview says that a software review asked, which has no
// triage session to hand the result to.
func (v ApprovalRequestView) FromSoftwareReview() bool { return v.request.TriageSessionID == "" }

// CoveredByApprovalRequestID is the earlier approval request whose
// approval covered this run; empty for every other request.
func (v ApprovalRequestView) CoveredByApprovalRequestID() string {
	return v.request.CoveredByApprovalRequestID
}

// TriageSessionID is the session whose agent asked; empty for a software review.
func (v ApprovalRequestView) TriageSessionID() string { return v.request.TriageSessionID }

// ShortCommitID is the commit as people read it in a sentence, such as 3beb741.
func (v ApprovalRequestView) ShortCommitID() string { return v.CommitID[:min(len(v.CommitID), 7)] }

// DiagnosisSummary is the diagnosis's first prose paragraph, which the agent
// writes for the person who decides, as the SOS message gives it.
func (v ApprovalRequestView) DiagnosisSummary() string { return firstProseParagraph(v.Diagnosis) }

// ApprovalsStillNeeded counts the approvals an open approval request still needs.
func (v ApprovalRequestView) ApprovalsStillNeeded() int {
	return max(v.ApprovalsNeeded-len(v.countedApprovers()), 0)
}

// WhatAnApprovalDoes tells the viewer of an open approval request what
// their approval does now, after saying which approvals of removed members
// no longer count.
func (v ApprovalRequestView) WhatAnApprovalDoes() string {
	still := v.ApprovalsStillNeeded()
	removed := v.removedApproversText()
	switch {
	case v.ApprovedByYou:
		return removed + "You approved the playbook run. MMHotline runs the playbook as soon as another person approves."
	case still == 2:
		return removed + "MMHotline runs the playbook once two different people approve."
	case still < v.ApprovalsNeeded:
		return removed + strings.Join(v.countedApprovers(), " and ") + " approved the playbook run. MMHotline runs the playbook as soon as one more person approves."
	}
	return removed + "MMHotline runs the playbook as soon as one person approves."
}

// StateTag names its state in a few words, for a tag: "Waiting for 1
// approval", "Runs in 7 minutes", "Ran and succeeded".
func (v ApprovalRequestView) StateTag() string {
	switch {
	case v.State == approvalOpen && v.Stoppable:
		return v.RunsIn
	case v.State == approvalOpen && v.Warned:
		return "Runs now"
	case v.State == approvalOpen && v.ApprovalsStillNeeded() < v.ApprovalsNeeded:
		return "Waiting for 1 more approval"
	case v.State == approvalOpen:
		return "Waiting for " + countText(v.ApprovalsNeeded, "approval", "approvals")
	case v.State == approvalApproved:
		return "Approved"
	case v.State == approvalStale:
		return "Did not run"
	}
	return approvalStateLabel(v.State)
}

// Tone is its state's color, by the colors' one meaning: warn while it waits
// for a person or its run time, and when a run failed or could not start;
// work while it runs; ok when it ran; empty, gray, when it ended without
// running.
func (v ApprovalRequestView) Tone() string {
	switch v.State {
	case approvalOpen:
		if v.Warned && !v.Stoppable {
			return "work"
		}
		return "warn"
	case approvalApproved, approvalRunning:
		return "work"
	case approvalRanAndSucceeded:
		return "ok"
	case approvalRanAndFailed, approvalCouldNotRun:
		return "warn"
	}
	return ""
}

// At is when the decision was made.
func (d approvalDecisionView) At() time.Time { return d.decidedAt }

// approvalRequestViews reads the approval requests and warned runs the
// condition picks, for the viewer m at now.
func approvalRequestViews(ctx context.Context, tx *sql.Tx, m Member, now time.Time, condition string, args ...any) ([]ApprovalRequestView, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+waitingPlaybookRunColumns+` FROM triage_agent_playbook_runs_waiting_for_a_person WHERE `+condition, args...)
	if err != nil {
		return nil, err
	}
	var requests []approvalRequest
	for rows.Next() {
		r, err := scanApprovalRequest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		requests = append(requests, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	services, err := readServicesWhoseChangesAPersonMustApprove(ctx, tx)
	if err != nil {
		return nil, err
	}
	views := make([]ApprovalRequestView, 0, len(requests))
	for _, r := range requests {
		v := ApprovalRequestView{request: r, ID: r.ID, Playbook: r.Playbook, Inventory: r.Inventory, CommitID: r.CommitID, PlaybookText: r.PlaybookText, IncidentID: r.IncidentID,
			Title: playbookTitle(r.Playbook), RuleTitle: r.PlaybookRule.Title(), ListedPlaybookRule: strings.TrimSpace(r.listedPlaybookSentence()), ApprovalsNeeded: r.PlaybookRule.ApprovalsNeeded, State: r.State, StateLabel: approvalStateLabel(r.State),
			ResultMessage: r.ResultMessage}
		if r.IncidentID != "" {
			if err := tx.QueryRowContext(ctx, `SELECT e.title FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.id=?`, r.IncidentID).Scan(&v.IncidentTitle); err != nil {
				return nil, err
			}
		}
		for _, kind := range strings.Split(r.ActionKinds, ",") {
			if kind != "" {
				v.Kinds = append(v.Kinds, actionkinds.Kind(kind).Title())
			}
		}
		// The node tasks as mmx explain listed them when MMHotline judged;
		// the card calls them what it can change on each server.
		if recognition, err := actionkinds.Recognize([]byte(r.MMXExplainOutput)); err == nil {
			v.Servers = serverListText(recognition.Tasks)
			for _, task := range recognition.Tasks {
				view := approvalTaskView{Path: task.Path, Node: task.Node, Module: task.Action, Status: task.Status, Kind: task.Kind().Title()}
				if task.ModuleKind != "" {
					view.Module += ", a module of kind " + task.ModuleKind
				}
				for _, argument := range task.Arguments {
					if argument.Value == nil {
						view.Arguments = append(view.Arguments, argument.Field+": value not known before the run ("+argument.Status+")")
						continue
					}
					value, err := json.Marshal(argument.Value)
					if err != nil {
						value = []byte(fmt.Sprint(argument.Value))
					}
					view.Arguments = append(view.Arguments, argument.Field+" = "+string(value)+" ("+argument.Status+")")
				}
				v.Tasks = append(v.Tasks, view)
			}
			v.UnrecognizedTasks = recognition.UnrecognizedTasks
			v.TasksOnServicesWhoseChangesAPersonMustApprove = tasksOnServicesWhoseChangesAPersonMustApprove(recognition, services.ServiceNames)
		}
		if err := tx.QueryRowContext(ctx, `SELECT diagnosis FROM triage_agent_requests WHERE run_id=? AND request_id=?`, r.RunID, r.RequestID).Scan(&v.Diagnosis); err != nil {
			return nil, err
		}
		var reviewID int64
		var outcome, reason string
		err := tx.QueryRowContext(ctx, `SELECT id,outcome,reason FROM playbook_run_reviews WHERE run_id=? AND request_id=? AND commit_id=? ORDER BY id DESC LIMIT 1`, r.RunID, r.RequestID, r.CommitID).Scan(&reviewID, &outcome, &reason)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			v.Review, v.ReviewOutcome = playbookReview{outcome: outcome, reason: reason}.text(), outcome
			// The person who decides sees each version the review agent
			// refused in the row that led here, and why.
			if outcome != reviewApproved {
				if v.EarlierVersionsRefused, err = reviewAgentRefusalsInARow(ctx, tx, reviewProblem{triageSessionID: r.TriageSessionID, runID: r.RunID}, reviewID); err != nil {
					return nil, err
				}
			}
		}
		if v.Decisions, v.ApprovedByYou, err = approvalDecisionViews(ctx, tx, r.ID, m.ID); err != nil {
			return nil, err
		}
		v.AlsoApproves = r.alsoApprovesText()
		if r.CoveredByApprovalRequestID != "" {
			if v.CoveredBy, err = coveredByText(ctx, tx, r); err != nil {
				return nil, err
			}
		}
		if r.warned() {
			v.Warned, v.RunsAt = true, displayTime(r.runsAt())
			if r.State == approvalOpen {
				// The background loop starts it within a second of its run time.
				v.StateLabel = "Runs now"
				if now.Before(r.runsAt()) {
					v.Stoppable, v.RunsIn = true, runsInText(r.runsAt().Sub(now))
					v.StateLabel = v.RunsIn + " unless someone presses Stop"
				}
			}
			if r.StoppedByMemberID != "" {
				if v.StoppedBy, err = incidentMemberName(ctx, tx, r.StoppedByMemberID); err != nil {
					return nil, err
				}
				v.StopReason = r.StopReason
			}
		}
		switch r.ResultForTheAgent {
		case resultForTheAgentWaits:
			v.ResultForTheAgent = "The result goes to the triage agent in its session's next run."
		case resultForTheAgentSent:
			v.ResultForTheAgent = "The result reached the triage agent in its session."
		case resultForTheAgentCouldNotReachTheAgent:
			v.ResultForTheAgent = "The result could not reach the triage agent: its session had ended, or the software review that asked has none."
		}
		views = append(views, v)
	}
	return views, nil
}

func approvalDecisionViews(ctx context.Context, tx *sql.Tx, id, viewerID string) ([]approvalDecisionView, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT d.member_id,d.decision,d.reason,d.decided_at,m.removed_at IS NULL FROM playbook_run_approval_decisions d LEFT JOIN members m ON m.id=d.member_id WHERE d.approval_request_id=? ORDER BY d.decided_at,d.id`, id)
	if err != nil {
		return nil, false, err
	}
	type decision struct {
		memberID, decision, reason string
		at                         int64
		memberRemoved              bool
	}
	var decisions []decision
	for rows.Next() {
		var d decision
		var current sql.NullBool
		if err = rows.Scan(&d.memberID, &d.decision, &d.reason, &d.at, &current); err != nil {
			rows.Close()
			return nil, false, err
		}
		d.memberRemoved = !current.Valid || !current.Bool
		decisions = append(decisions, d)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	var views []approvalDecisionView
	approvedByViewer := false
	for _, d := range decisions {
		name, err := incidentMemberName(ctx, tx, d.memberID)
		if err != nil {
			return nil, false, err
		}
		label := "Approved"
		if d.decision == "refuse" {
			label = "Refused"
		}
		approvedByViewer = approvedByViewer || d.decision == "approve" && d.memberID == viewerID
		views = append(views, approvalDecisionView{MemberName: name, Decision: label, Reason: d.reason, decision: d.decision, decidedAt: deadlineTime(d.at), memberRemoved: d.memberRemoved})
	}
	return views, approvedByViewer, nil
}

// ApprovalRequests lists the open approval requests, newest first, and the
// latest decided ones.
func (a *App) ApprovalRequests(ctx context.Context, p Principal) (open, decided []ApprovalRequestView, err error) {
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		now := a.Now().UTC()
		if open, err = approvalRequestViews(ctx, tx, m, now, `state=? ORDER BY judged_at DESC,id`, approvalOpen); err != nil {
			return err
		}
		decided, err = approvalRequestViews(ctx, tx, m, now, `state!=? ORDER BY state_changed_at DESC,id LIMIT ?`, approvalOpen, maximumDecidedApprovalRequestsShown)
		return err
	})
	return open, decided, err
}

// ApprovalRequest reads one approval request or warned run as its card
// shows it.
func (a *App) ApprovalRequest(ctx context.Context, p Principal, approvalRequestID string) (ApprovalRequestView, error) {
	var views []ApprovalRequestView
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		views, err = approvalRequestViews(ctx, tx, m, a.Now().UTC(), `id=?`, approvalRequestID)
		return err
	})
	if err == nil && len(views) == 0 {
		err = fmt.Errorf("%w: no approval request has ID %s", ErrNotFound, approvalRequestID)
	}
	if err != nil {
		return ApprovalRequestView{}, err
	}
	return views[0], nil //nolint:nilaway // views holds at least one view here, because the len(views) == 0 case set err above.
}

// IncidentApprovalRequests lists the approval requests of the incident's
// triage sessions, and of the incident itself, which a software
// review's request opens; newest first.
func (a *App) IncidentApprovalRequests(ctx context.Context, p Principal, incidentID string) ([]ApprovalRequestView, error) {
	var views []ApprovalRequestView
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		views, err = approvalRequestViews(ctx, tx, m, a.Now().UTC(), `(incident_id=?1 OR triage_session_id IN (SELECT s.triage_session_id FROM triage_session_alerts s JOIN alerts e ON e.id=s.alert_id WHERE e.incident_id=?1)) ORDER BY judged_at DESC,id`, incidentID)
		return err
	})
	return views, err
}

// playbookTitle is a playbook's file name in plain words, as a page's
// heading names it: one_time_playbooks/2026-11-03_reboot_app002.yml is
// “Reboot app002”, without a one-time playbook's date.
func playbookTitle(playbook string) string {
	name := strings.TrimSuffix(path.Base(playbook), ".yml")
	if _, what, found := strings.Cut(name, "_"); found && strings.HasPrefix(playbook, "one_time_playbooks/") && oneTimePlaybookName.MatchString(name) {
		name = what
	}
	return capitalize(strings.ReplaceAll(name, "_", " "))
}

func (a *App) RegisterApprovalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /approvals", a.approvalsPageHTTP)
	mux.HandleFunc("GET /approvals/commits/{commit_id}", func(w http.ResponseWriter, r *http.Request) { a.renderCommitReviewPage(w, r, 200, "") })
	mux.HandleFunc("POST /approvals/commits/{commit_id}/review", a.reviewCommitHTTP)
	mux.HandleFunc("GET /approvals/{approval_request_id}", func(w http.ResponseWriter, r *http.Request) { a.renderApprovalRequestPage(w, r, 200, "") })
	mux.HandleFunc("POST /approvals/{approval_request_id}/approve", a.decideApprovalHTTP)
	mux.HandleFunc("POST /approvals/{approval_request_id}/refuse", a.decideApprovalHTTP)
	mux.HandleFunc("POST /approvals/{approval_request_id}/stop", a.decideApprovalHTTP)
}

// approvalsPageHTTP lists what waits for a person first, then the warned
// runs, the one that runs next first, then what was decided or run lately.
func (a *App) approvalsPageHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	open, decided, err := a.ApprovalRequests(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	commits, err := a.CommitReviews(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	approvedByTheTriageAgent, err := a.CommitsTheTriageAgentApproved(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	var waiting, warned []ApprovalRequestView
	for _, v := range open {
		if v.Warned {
			warned = append(warned, v)
		} else {
			waiting = append(waiting, v)
		}
	}
	slices.SortStableFunc(warned, func(x, y ApprovalRequestView) int { return x.RunTime().Compare(y.RunTime()) })
	RenderPage(w, 200, "Approvals", approvalsPageBody, struct {
		Waiting, Warned, Decided          []ApprovalRequestView
		Commits, ApprovedByTheTriageAgent []CommitReview
		CSRF                              string
	}{waiting, warned, decided, commits, approvedByTheTriageAgent, a.CSRFToken(w, r)})
}

// renderApprovalRequestPage shows one approval request or warned run, with
// message, the error of a decision that failed, when set.
func (a *App) renderApprovalRequestPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	v, err := a.ApprovalRequest(r.Context(), p, r.PathValue("approval_request_id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	RenderPage(w, status, v.Question(), approvalRequestPageBody, struct {
		Request                 ApprovalRequestView
		CSRF, Error             string
		MaximumReasonCharacters int
	}{v, a.CSRFToken(w, r), message, maximumRefusalReasonCharacters})
}

// decideApprovalHTTP approves, refuses, or stops through the one operation
// for each, then returns to the incident the form was on, or to the
// request's own page, which shows what the decision did. A decision that
// fails shows the request's page with the reason.
func (a *App) decideApprovalHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	id := r.PathValue("approval_request_id")
	var err error
	switch {
	case strings.HasSuffix(r.URL.Path, "/refuse"):
		err = a.RefusePlaybookRun(r.Context(), p, id, r.PostForm.Get("reason"))
	case strings.HasSuffix(r.URL.Path, "/stop"):
		err = a.StopWarnedPlaybookRun(r.Context(), p, id, r.PostForm.Get("reason"))
	default:
		err = a.ApprovePlaybookRun(r.Context(), p, id)
	}
	if err != nil {
		a.renderApprovalRequestPage(w, r, errorStatus(err), publicError(err))
		return
	}
	target := "/approvals/" + id
	if incidentID := r.PostForm.Get("return_to_incident_id"); recordIDPattern.MatchString(incidentID) {
		target = "/incidents/" + incidentID + "#approval-" + id
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// approvalsPageBody lists, first and amber, the approval requests that wait
// for a person, each with Review; then the warned runs with their run time
// and Stop, one press, because the run goes ahead by itself; then, quiet,
// what was decided or run lately. Each section is live, so it stays current.
const approvalsPageBody = commitReviewsListBody + `<section id="open-approvals" data-live class="panel{{if .Waiting}} warn{{end}}"><div class="panel-head"><h2>Waiting for a person</h2>{{with .Waiting}}<span class="tag warn">{{len .}} waiting</span>{{end}}</div>` +
	`{{with .Waiting}}<ul class="items">{{range .}}<li><div class="what"><a class="title" href="/approvals/{{.ID}}">{{.Question}}</a><span class="faint small">{{.StateTag}} · asked at {{clock .AskedAt}}, {{relativeTime .AskedAt}}{{with .IncidentTitle}} · for the SOS {{.}}{{end}}</span></div><div class="do"><a class="btn primary" href="/approvals/{{.ID}}">Review</a></div></li>{{end}}</ul>` +
	`<p class="panel-foot">Open a playbook run request to read the proposed changes, then approve or refuse the request. Any team member may decide.</p>{{else}}<div class="panel-body"><p class="muted">No playbook run waits for a person's approval.</p></div>{{end}}</section>` +
	`<section id="warned-playbook-runs" data-live class="panel"><div class="panel-head"><h2>Runs unless someone presses Stop</h2></div>` +
	`{{with .Warned}}<ul class="items">{{range .}}<li><div class="what"><a class="title" href="/approvals/{{.ID}}">{{.Question}}</a><span class="faint small">{{if .Stoppable}}Runs at {{clock .RunTime}}, {{relativeTime .RunTime}}{{else}}{{.StateTag}}{{end}}{{with .IncidentTitle}} · for the SOS {{.}}{{end}}</span></div>{{if .Stoppable}}<form class="do" method="post" action="/approvals/{{.ID}}/stop"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="btn">Stop</button></form>{{end}}</li>{{end}}</ul>` +
	`<p class="panel-foot">MMHotline starts each playbook run at the scheduled time. Press Stop before the scheduled time to cancel the playbook run.</p>{{else}}<div class="panel-body"><p class="muted">No playbook run waits for its run time.</p></div>{{end}}</section>` +
	`<section id="decided-approvals" data-live class="panel quiet"><div class="panel-head"><h2>Decided or run lately</h2></div>` +
	`{{with .Decided}}<ul class="items">{{range .}}<li><div class="what"><a href="/approvals/{{.ID}}">{{.Question}}</a><span class="faint small">{{if .Warned}}Warned{{else}}Asked{{end}} at {{clock .AskedAt}} · last change at {{clock .ChangedAt}}</span></div><span class="tag {{.Tone}}">{{.StateTag}}</span></li>{{end}}</ul>` +
	`{{else}}<div class="panel-body"><p class="faint">No playbook run was decided, stopped, or run yet.</p></div>{{end}}</section>` +
	commitsTheTriageAgentApprovedBody

// approvalRequestPageBody is one approval request or warned run. The heading
// asks the question. The box below it says what MMHotline does next and
// holds the decision, or, once decided, says what happened. The facts, the
// warnings that make a person decide, why the triage agent asks, and each
// step follow; the playbook's text and the identifiers are folded.
const approvalRequestPageBody = `{{define "heading"}}{{with .Data.Request}}<div class="approval-head"><p class="crumbs"><a href="/dashboard">Dashboard</a> / <a href="/approvals">Approvals</a> / {{.Title}}</p>` +
	`<div id="approval-state" data-live class="tags"><span class="tag {{.Tone}}">{{.StateTag}}</span></div><h1>{{.Question}}</h1>` +
	`<p class="meta">The triage agent asked{{if .FromSoftwareReview}} during a software review{{end}} at {{clock .AskedAt}}, {{relativeTime .AskedAt}}{{if .IncidentID}}, for the SOS <a href="/incidents/{{.IncidentID}}">{{.IncidentTitle}}</a>{{end}}.</p></div>{{end}}{{end}}` +
	`{{define "reviewTag"}}{{if eq . "approved"}}<span class="tag ok">Approved</span>{{else if eq . "refused"}}<span class="tag warn">Refused</span>{{else}}<span class="tag warn">Could not review</span>{{end}}{{end}}` +
	`{{with .Error}}<p class="form-error" role="alert">{{capitalize .}}</p>{{end}}{{with .Request}}` +
	`{{if and .Open (not .Warned)}}<section id="approval-decision" data-live class="decide warn approval" aria-label="Your decision"><div class="say"><strong>{{.WhatAnApprovalDoes}}</strong>{{with .AlsoApproves}}<p><strong>{{.}}</strong></p>{{end}}<p class="muted">{{.Where}} The result appears on the playbook run page{{if .IncidentID}} and on the incident{{end}}.</p></div>` +
	`<div class="do">{{if not .ApprovedByYou}}<form method="post" action="/approvals/{{.ID}}/approve"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="btn primary big block">Approve and run</button></form>{{end}}` +
	`<form method="post" action="/approvals/{{.ID}}/refuse" class="refuse"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input name="reason" required maxlength="{{$.MaximumReasonCharacters}}" aria-label="Reason for refusing" placeholder="Reason for refusing"><button class="btn">Refuse</button></form>` +
	`<p class="faint small">{{if .FromSoftwareReview}}One refusal ends the request. Your refusal reason stays on the playbook run page and in the incident history.{{else}}One refusal ends the request. The triage agent gets your reason, and may ask again with another fix.{{end}}</p></div></section>` +
	`{{else if .Stoppable}}<section id="approval-decision" data-live class="decide warn approval" aria-label="Your decision"><div class="say"><strong>MMHotline runs the playbook at {{clock .RunTime}}, {{relativeTime .RunTime}}, unless someone presses Stop.</strong><p class="muted">{{.Where}} The playbook run needs no approval because the rule is {{.RuleTitle}}.</p></div>` +
	`<form class="do" method="post" action="/approvals/{{.ID}}/stop"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input name="reason" maxlength="{{$.MaximumReasonCharacters}}" aria-label="Reason for stopping, optional" placeholder="Reason for stopping (optional)"><button class="btn primary big block">Stop</button><p class="faint small">Then nothing runs{{if not .FromSoftwareReview}}, and MMHotline tells the triage agent you stopped the playbook run{{end}}.</p></form></section>` +
	`{{else}}<section id="approval-decision" data-live class="panel" aria-label="What happened"><div class="panel-head"><h2>{{.StateLabel}}</h2><span class="faint small">{{clock .ChangedAt}}</span></div><div class="panel-body">` +
	`{{with .CoveredBy}}<p>Approved without a new request: {{.}} <a href="/approvals/{{$.Request.CoveredByApprovalRequestID}}">Open the earlier approval</a></p>{{end}}` +
	`{{range .Decisions}}<p>{{.Decision}} by {{.MemberName}} at {{clock .At}}{{with .Reason}}: {{.}}{{end}}</p>{{end}}{{with .StoppedBy}}<p>Stopped by {{.}} at {{clock $.Request.ChangedAt}}{{with $.Request.StopReason}}: {{.}}{{end}}</p>{{end}}` +
	`{{with .ResultMessage}}<div class="result">{{.}}</div>{{else}}{{if or (eq .State "approved") (eq .State "running") .Open}}<p class="muted">The result appears on the playbook run page when the run ends.</p>{{end}}{{end}}{{with .ResultForTheAgent}}<p class="faint small">{{.}}</p>{{end}}</div></section>{{end}}` +
	`<div class="facts"><div class="fact"><div class="k">Can change</div><div class="v">{{with .Servers}}{{.}}{{else}}No server{{end}}</div></div><div class="fact"><div class="k">Inventory</div><div class="v">{{.Inventory}}</div></div><div class="fact"><div class="k">Steps</div><div class="v">{{len .Tasks}}</div></div>{{with .ReviewOutcome}}<div class="fact"><div class="k">Review agent</div><div class="v">{{template "reviewTag" .}}</div></div>{{end}}</div>` +
	`<div class="cols"><div class="col-main">` +
	`{{with .UnrecognizedTasks}}<section class="panel warn"><div class="panel-head"><h2>Steps MMHotline did not recognize</h2></div><div class="panel-body"><p>MMHotline cannot check what the unrecognized steps do, so a person decides.</p><ul>{{range .}}<li><code>{{.Path}}</code> ({{.Action}}): {{.Reason}}</li>{{end}}</ul></div></section>{{end}}` +
	`{{with .TasksOnServicesWhoseChangesAPersonMustApprove}}<section class="panel warn"><div class="panel-head"><h2>Steps on services whose changes a person must approve</h2></div><div class="panel-body"><p>A person decides every change to a service on the list below.</p><ul>{{range .}}<li><code>{{.Path}}</code> ({{.Action}}) {{.Reason}}</li>{{end}}</ul><p class="faint small">An administrator changes the list in <code>services_whose_changes_a_person_must_approve</code> in MMHotline's settings file, and the <a href="/agent-playbook-rules">Rules for running the agent's playbooks</a> page shows the list.</p></div></section>{{end}}` +
	`{{if .Diagnosis}}<section class="panel"><div class="panel-head"><h2>Why the triage agent asks</h2></div><div class="panel-body">{{with .DiagnosisSummary}}<p class="agent-summary">{{.}}</p><details class="more"><summary>The agent's whole diagnosis</summary><div class="body"><pre>{{$.Request.Diagnosis}}</pre></div></details>{{else}}<pre>{{$.Request.Diagnosis}}</pre>{{end}}<p class="faint small">MMHotline has not checked the triage agent's claims.</p></div></section>{{end}}` +
	`<section class="panel"><div class="panel-head"><h2>What the playbook changes, step by step</h2><span class="faint small">as <code>mmx explain</code> lists the steps</span></div>{{with .Tasks}}<ol class="changes">{{range .}}<li><span class="t">{{.Kind}}</span><span class="tag nodot">{{.Node}}</span><span class="h"><code>{{.Path}}</code> · {{.Module}}{{if eq .Status "conditional"}} · only if the step's condition holds at run time{{else if eq .Status "skipped"}} · MMX skips the step{{end}}{{range .Arguments}}<br><code>{{.}}</code>{{end}}</span></li>{{end}}</ol>{{else}}<div class="panel-body"><p class="muted">MMX lists no step.</p></div>{{end}}</section>` +
	`<div class="folds">{{with .EarlierVersionsRefused}}<details class="more"><summary>Earlier versions the review agent refused ({{len .}})</summary><div class="body"><p class="faint small">The triage agent revised its fix after each refusal and asked again. After the review agent's third refusal in a row, a person decides. Oldest first:</p><ol class="earlier">{{range .}}<li><p><code>{{.Playbook}}</code> in the inventory “{{.Inventory}}”, at commit <code>{{.CommitID}}</code></p><p>Review agent's reason: {{.Reason}}</p>` +
	`<details class="more"><summary>Why the triage agent asked</summary><div class="body"><pre>{{.Diagnosis}}</pre></div></details><details class="more"><summary>The playbook's text the review agent saw</summary><div class="body"><pre>{{.PlaybookText}}</pre></div></details></li>{{end}}</ol></div></details>{{end}}` +
	`<details class="more"><summary>The playbook's text, and any files it copies, at commit {{.ShortCommitID}}</summary><div class="body"><pre>{{.PlaybookText}}</pre></div></details>` +
	`<details class="more"><summary>Identifiers</summary><div class="body"><dl class="kv"><dt>Playbook</dt><dd><code>{{.Playbook}}</code></dd><dt>Inventory</dt><dd><code>{{.Inventory}}</code></dd><dt>Commit MMHotline checked</dt><dd><code>{{.CommitID}}</code></dd><dt>Request</dt><dd><code>{{.ID}}</code></dd>{{with .TriageSessionID}}<dt>Triage session</dt><dd><a href="/triage-sessions/{{.}}">{{.}}</a></dd>{{end}}</dl></div></details></div></div>` +
	`<div class="col-side"><section class="panel"><div class="panel-head"><h2>{{if .Warned}}Why MMHotline warns first{{else}}Why the playbook needs a person{{end}}</h2></div><div class="panel-body"><dl class="kv"><dt>Kinds of change</dt><dd>{{range $index, $kind := .Kinds}}{{if $index}}, {{end}}{{$kind}}{{else}}None{{end}}</dd><dt>Rule</dt><dd><strong>{{.RuleTitle}}</strong></dd></dl>` +
	`<p class="small muted">{{with .ListedPlaybookRule}}{{.}}{{else}}The strictest rule of the playbook's kinds of change in the inventory “{{.Inventory}}” applies.{{end}}</p>{{if .TasksOnServicesWhoseChangesAPersonMustApprove}}<p class="small">A step acts on a service whose changes a person must approve.</p>{{end}}{{if and .ReviewOutcome (ne .ReviewOutcome "approved")}}<p class="small">The review agent did not approve the playbook.</p>{{end}}</div><div class="panel-foot"><a href="/agent-playbook-rules">Rules for running the agent's playbooks</a></div></section>` +
	`{{with .Review}}<section class="panel"><div class="panel-head"><h2>Review agent</h2>{{template "reviewTag" $.Request.ReviewOutcome}}</div><div class="panel-body"><p class="small muted">{{.}}</p><p class="faint small">A second AI agent that checks each playbook before it runs.</p></div></section>{{end}}</div></div>{{end}}`
