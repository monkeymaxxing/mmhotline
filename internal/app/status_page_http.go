package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmhotline/internal/statuspage"
)

func (a *App) RegisterStatusPageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /status-page", a.statusPagePage)
	mux.HandleFunc("POST /status-page/notices", a.postStatusPageNoticeHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/updates", a.postStatusPageUpdateHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/resolve", a.resolveStatusPageNoticeHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/edit", a.editStatusPageNoticeHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/delete", a.deleteStatusPageNoticeHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/updates/{update_id}/edit", a.editStatusPageUpdateHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/updates/{update_id}/delete", a.deleteStatusPageUpdateHTTP)
}

type statusPageUpdateView struct {
	UpdateID   int64
	StageLabel string // empty when the update has no stage
	Posted     time.Time
	EditedAt   *time.Time
	Message    string
	// StageOptions and EditMessage fill the update's edit form: the stages of
	// the notice's impact with the update's own selected, and its message, or
	// what the person typed before the form was refused.
	StageOptions []statusPageOption
	EditMessage  string
}

// statusPageOption is one choice of a select.
type statusPageOption struct {
	Value, Label string
	Selected     bool
}

type statusPageNoticeView struct {
	NoticeID                 int64
	Title                    string
	ImpactClass, ImpactLabel string // the tag's color class and text
	// AffectedComponents are the names of the components the notice affects.
	AffectedComponents             []string
	ScheduledStartAt, ScheduledEnd *time.Time
	Opened                         time.Time
	Resolved                       *time.Time
	Updates                        []statusPageUpdateView
	// StageOptions are the stages of the notice's impact, for an update.
	StageOptions []statusPageOption
	// AddUpdateMessage fills the "Add an update" box: what the person typed
	// there before it was refused, empty otherwise.
	AddUpdateMessage string
	// ResolveLabel names the notice's closing action: complete a maintenance
	// notice, resolve an incident.
	ResolveLabel string
	// Draft is what a person typed into this notice's forms before one was
	// refused, empty otherwise.
	Draft statusPageDraft
	// UpdatedAt is the notice's updated_at, carried by its edit and delete
	// forms: a form from a page that is out of date is refused.
	UpdatedAt string
	// The edit form of the notice's details: its title, impact choices, planned
	// window in the team's time zone, and component checkboxes, with the
	// retired components the notice names checked and marked removed.
	EditTitle                              string
	EditImpactOptions                      []statusPageOption
	EditScheduledStartAt, EditScheduledEnd string
	EditComponents                         []statusPageComponentOption
	// CanDeleteUpdates is true when the notice has more than one update: a
	// notice keeps at least one.
	CanDeleteUpdates bool
	// UpdatesFoldOpen is true after a refused edit or delete of an update.
	UpdatesFoldOpen bool
}

// The forms of the Status page whose input a refusal keeps.
const (
	statusPageNewNoticeForm    = "new_notice"
	statusPageUpdateForm       = "update"
	statusPageResolveForm      = "resolve"
	statusPageEditNoticeForm   = "edit_notice"
	statusPageEditUpdateForm   = "edit_update"
	statusPageDeleteUpdateForm = "delete_update"
	statusPageDeleteNoticeForm = "delete_notice"
)

// statusPageDraft is what a person typed into a form of the Status page that
// was refused, so the page shows it again and nothing typed is lost. Form says
// which form it was, NoticeID which notice the form belongs to, and UpdateID
// which update when it edits or deletes one.
type statusPageDraft struct {
	Form                                                            string
	NoticeID, UpdateID                                              int64
	Title, Impact, Stage, Message, ScheduledStartAt, ScheduledEndAt string
	ClosingMessage                                                  string
	// AffectedComponentIDs are the boxes that were checked in the new notice
	// form or the form that edits a notice.
	AffectedComponentIDs []string
}

func statusPageImpactTag(impact statuspage.Impact) (class, label string) {
	switch impact {
	case statuspage.ImpactDegraded:
		return "warn", "Degraded"
	case statuspage.ImpactMaintenance:
		return "work", "Maintenance"
	case statuspage.ImpactOutage:
		return "sos", "Outage"
	}
	// An impact only a file edited by hand can hold shows as an outage, as the
	// public page does.
	return "sos", "Outage"
}

// statusPageDatetimeLocal writes t as the value of a datetime-local input in
// the team's time zone, empty when t is nil.
func statusPageDatetimeLocal(t *time.Time, zone *time.Location) string {
	if t == nil {
		return ""
	}
	return t.In(zone).Format("2006-01-02T15:04")
}

// statusPageStageOptions are the stage choices of a notice with the impact:
// no stage, then each stage of the impact, with selected chosen.
func statusPageStageOptions(impact statuspage.Impact, selected statuspage.Stage) []statusPageOption {
	options := []statusPageOption{{Value: "", Label: "No stage", Selected: selected == ""}}
	for _, stage := range statuspage.Stages(impact) {
		options = append(options, statusPageOption{Value: string(stage), Label: statuspage.StageLabel(stage), Selected: selected == stage})
	}
	return options
}

func viewStatusPageNotices(file statuspage.File, notices []statuspage.Notice, draft statusPageDraft, zone *time.Location) []statusPageNoticeView {
	var views []statusPageNoticeView
	for _, n := range notices {
		v := statusPageNoticeView{NoticeID: n.NoticeID, Title: n.Title, ScheduledStartAt: n.ScheduledStartAt, ScheduledEnd: n.ScheduledEndAt, Opened: n.OpenedAt, Resolved: n.ResolvedAt, ResolveLabel: "Resolve this notice"}
		for _, id := range n.AffectedComponentIDs {
			v.AffectedComponents = append(v.AffectedComponents, file.ComponentName(id))
		}
		v.ImpactClass, v.ImpactLabel = statusPageImpactTag(n.Impact)
		if n.Impact == statuspage.ImpactMaintenance {
			v.ResolveLabel = "Complete this notice"
		}
		if draft.NoticeID == n.NoticeID {
			v.Draft = draft
		}
		if v.Draft.Form == statusPageUpdateForm {
			v.AddUpdateMessage = v.Draft.Message
		}
		v.UpdatedAt = n.UpdatedAt.UTC().Format(time.RFC3339)
		v.CanDeleteUpdates = len(n.Updates) > 1
		v.UpdatesFoldOpen = v.Draft.Form == statusPageEditUpdateForm || v.Draft.Form == statusPageDeleteUpdateForm
		for _, u := range n.Updates {
			update := statusPageUpdateView{UpdateID: u.UpdateID, StageLabel: stageLabelOrEmpty(u.Stage), Posted: u.PostedAt, EditedAt: u.EditedAt, Message: u.Message, EditMessage: u.Message}
			selected := u.Stage
			if v.Draft.Form == statusPageEditUpdateForm && v.Draft.UpdateID == u.UpdateID {
				update.EditMessage, selected = v.Draft.Message, statuspage.Stage(v.Draft.Stage)
			}
			update.StageOptions = statusPageStageOptions(n.Impact, selected)
			// A stage the notice's impact no longer has, after the impact changed,
			// stays selected, so fixing the message does not clear the stage.
			if u.Stage != "" && !slices.Contains(statuspage.Stages(n.Impact), u.Stage) {
				update.StageOptions = append(update.StageOptions, statusPageOption{Value: string(u.Stage), Label: statuspage.StageLabel(u.Stage), Selected: selected == u.Stage})
			}
			v.Updates = append(v.Updates, update)
		}
		// An update starts at the stage of the notice's latest update, which
		// the next message most often shares; a refused update keeps its own.
		selected := statuspage.Stage(draft.Stage)
		if draft.Form != statusPageUpdateForm || draft.NoticeID != n.NoticeID {
			selected = ""
			if len(n.Updates) > 0 {
				selected = n.Updates[len(n.Updates)-1].Stage
			}
		}
		v.StageOptions = statusPageStageOptions(n.Impact, selected)

		// The form that edits the notice's details starts with its own values,
		// or what the person typed before the form was refused.
		editing := v.Draft.Form == statusPageEditNoticeForm
		v.EditTitle, v.EditScheduledStartAt, v.EditScheduledEnd = n.Title, statusPageDatetimeLocal(n.ScheduledStartAt, zone), statusPageDatetimeLocal(n.ScheduledEndAt, zone)
		impact := n.Impact
		if editing {
			v.EditTitle, v.EditScheduledStartAt, v.EditScheduledEnd, impact = v.Draft.Title, v.Draft.ScheduledStartAt, v.Draft.ScheduledEndAt, statuspage.Impact(v.Draft.Impact)
		}
		for _, option := range []struct{ value, label string }{{"outage", "Outage"}, {"degraded", "Degraded performance"}, {"maintenance", "Scheduled maintenance"}} {
			v.EditImpactOptions = append(v.EditImpactOptions, statusPageOption{Value: option.value, Label: option.label, Selected: string(impact) == option.value})
		}
		checked := func(id string) bool {
			if editing {
				return slices.Contains(v.Draft.AffectedComponentIDs, id)
			}
			return slices.Contains(n.AffectedComponentIDs, id)
		}
		for _, c := range file.Components {
			v.EditComponents = append(v.EditComponents, statusPageComponentOption{ComponentID: c.ComponentID, Name: c.Name, Checked: checked(c.ComponentID)})
		}
		// A component the settings file no longer lists stays checked and
		// marked removed, so editing the title does not drop it.
		for _, id := range n.AffectedComponentIDs {
			if !slices.ContainsFunc(file.Components, func(c statuspage.Component) bool { return c.ComponentID == id }) {
				v.EditComponents = append(v.EditComponents, statusPageComponentOption{ComponentID: id, Name: id + " (removed)", Checked: checked(id)})
			}
		}
		views = append(views, v)
	}
	return views
}

func stageLabelOrEmpty(stage statuspage.Stage) string {
	if stage == "" {
		return ""
	}
	return statuspage.StageLabel(stage)
}

// statusPageNewNotice holds the values of the form that posts a new notice.
type statusPageNewNotice struct {
	Title, Impact, Stage, Message, ScheduledStartAt, ScheduledEndAt string
}

// statusPageComponentOption is one checkbox of the new notice form: a
// component the notice may affect.
type statusPageComponentOption struct {
	ComponentID, Name string
	Checked           bool
}

// statusPagePageData is the Status page's data.
type statusPagePageData struct {
	View           StatusPageView
	CSRF, Error    string
	Ready          bool
	Open, Resolved []statusPageNoticeView
	// ResolvedOpen is true after a refused edit or delete of a resolved
	// notice, so its fold shows the form again.
	ResolvedOpen bool
	NewNotice    statusPageNewNotice
	// Components are the checkboxes of the new notice form, empty when the
	// settings file lists no components.
	Components []statusPageComponentOption
	// TimeZone is the team's time zone, which the form's planned times use.
	TimeZone string
	// LostDraftText is what the person typed into a form that was refused and
	// that the page no longer has, such as an update to a notice another
	// responder resolved meanwhile. The page shows it beside the error, so
	// nothing typed is lost.
	LostDraftText string
	settingsFileNoticeView
}

func (a *App) statusPagePage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	view, err := a.StatusPage(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	a.renderStatusPagePage(w, r, 200, view, "", statusPageDraft{})
}

// renderStatusPagePage writes the Status page. After a refused form, draft is
// what the person typed, which the form shows again.
func (a *App) renderStatusPagePage(w http.ResponseWriter, r *http.Request, status int, view StatusPageView, message string, draft statusPageDraft) {
	data := statusPagePageData{View: view, CSRF: a.CSRFToken(w, r), Error: message, Ready: view.PublishedStatusJSON != nil, settingsFileNoticeView: a.settingsFileNoticeView(), NewNotice: statusPageNewNotice{Impact: string(statuspage.ImpactOutage)}}
	if draft.Form == statusPageNewNoticeForm {
		data.NewNotice = statusPageNewNotice{Title: draft.Title, Impact: draft.Impact, Stage: draft.Stage, Message: draft.Message, ScheduledStartAt: draft.ScheduledStartAt, ScheduledEndAt: draft.ScheduledEndAt}
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	data.TimeZone = frame.TimeZone.String()
	if view.PublishedStatusJSON != nil {
		file := *view.PublishedStatusJSON
		data.Open = viewStatusPageNotices(file, file.OpenNotices(), draft, frame.TimeZone)
		data.Resolved = viewStatusPageNotices(file, file.ResolvedNotices(5), draft, frame.TimeZone)
		for _, notice := range data.Resolved {
			switch notice.Draft.Form {
			case statusPageEditNoticeForm, statusPageEditUpdateForm, statusPageDeleteUpdateForm, statusPageDeleteNoticeForm:
				data.ResolvedOpen = true
			}
		}
		for _, c := range file.Components {
			data.Components = append(data.Components, statusPageComponentOption{ComponentID: c.ComponentID, Name: c.Name, Checked: draft.Form == statusPageNewNoticeForm && slices.Contains(draft.AffectedComponentIDs, c.ComponentID)})
		}
	}
	data.LostDraftText = statusPageLostDraftText(draft, data.Open, data.Resolved)
	renderPage(w, status, "Status page", frame, nil, statusPagePageBody(frame, data))
}

// statusPageLostDraftText is the text of a refused form that the reloaded page
// has no form for: an update or a closing message needs an open notice, and an
// edit of an update needs that update. It is empty when the page has the form,
// which shows the text itself, and for a form without free text.
func statusPageLostDraftText(draft statusPageDraft, open, resolved []statusPageNoticeView) string {
	var text string
	switch draft.Form {
	case statusPageUpdateForm, statusPageEditUpdateForm:
		text = draft.Message
	case statusPageResolveForm:
		text = draft.ClosingMessage
	}
	if strings.TrimSpace(text) == "" {
		return ""
	}
	hasUpdate := func(n statusPageNoticeView) bool {
		return slices.ContainsFunc(n.Updates, func(u statusPageUpdateView) bool { return u.UpdateID == draft.UpdateID })
	}
	for _, n := range open {
		if n.NoticeID == draft.NoticeID && (draft.Form != statusPageEditUpdateForm || hasUpdate(n)) {
			return ""
		}
	}
	for _, n := range resolved {
		if n.NoticeID == draft.NoticeID && draft.Form == statusPageEditUpdateForm && hasUpdate(n) {
			return ""
		}
	}
	return text
}

func (a *App) statusPageMutation(w http.ResponseWriter, r *http.Request) (Principal, int64, bool) {
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return Principal{}, 0, false
	}
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		HTTPError(w, err)
		return Principal{}, 0, false
	}
	var noticeID int64
	if raw := r.PathValue("notice_id"); raw != "" {
		noticeID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || noticeID < 1 {
			HTTPError(w, ErrNotFound)
			return Principal{}, 0, false
		}
	}
	return p, noticeID, true
}

// finishStatusPageMutation redirects after a post. After a refusal it shows
// the page again with the reason and the person's draft.
func (a *App) finishStatusPageMutation(w http.ResponseWriter, r *http.Request, p Principal, view StatusPageView, err error, draft statusPageDraft) {
	if err != nil {
		current, readErr := a.StatusPage(r.Context(), p)
		if readErr != nil {
			HTTPError(w, err)
			return
		}
		a.renderStatusPagePage(w, r, errorStatus(err), current, publicError(err), draft)
		return
	}
	if view.ReadError != "" {
		// The page already shows view.ReadError beside the page's address.
		a.renderStatusPagePage(w, r, 200, view, "", statusPageDraft{})
		return
	}
	http.Redirect(w, r, "/status-page", http.StatusSeeOther)
}

// teamLocalTime reads the value of a datetime-local input, such as
// 2026-11-21T02:00, in the team's time zone; nil when the input is empty.
func teamLocalTime(value string, zone *time.Location, label string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02T15:04", value, zone)
	if err != nil {
		return nil, fmt.Errorf("%w: the %s must be a date and a time", ErrInvalid, label)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

// defaultStage is the stage a new notice starts at when the person leaves the
// stage to the default: investigating, or scheduled for maintenance.
func defaultStage(impact statuspage.Impact) statuspage.Stage {
	if impact == statuspage.ImpactMaintenance {
		return statuspage.StageScheduled
	}
	return statuspage.StageInvestigating
}

func (a *App) postStatusPageNoticeHTTP(w http.ResponseWriter, r *http.Request) {
	p, _, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	if incidentID := r.PostForm.Get("incident_id"); incidentID != "" {
		view, _, err := a.PostIncidentToStatusPage(r.Context(), p, incidentID)
		a.finishStatusPageMutation(w, r, p, view, err, statusPageDraft{})
		return
	}
	form := r.PostForm
	draft := statusPageDraft{Form: statusPageNewNoticeForm, Title: form.Get("title"), Impact: form.Get("impact"), Stage: form.Get("stage"), Message: form.Get("message"), ScheduledStartAt: form.Get("scheduled_start_at"), ScheduledEndAt: form.Get("scheduled_end_at"), AffectedComponentIDs: form["affected_component_ids"]}
	zone, err := a.alertPageTimeZone(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	in := statuspage.PostNoticeInput{Title: draft.Title, Impact: statuspage.Impact(draft.Impact), AffectedComponentIDs: draft.AffectedComponentIDs, Stage: statuspage.Stage(draft.Stage), Message: draft.Message}
	if in.Stage == "" {
		in.Stage = defaultStage(in.Impact)
	}
	var view StatusPageView
	if in.ScheduledStartAt, err = teamLocalTime(draft.ScheduledStartAt, zone, "planned start"); err == nil {
		if in.ScheduledEndAt, err = teamLocalTime(draft.ScheduledEndAt, zone, "planned end"); err == nil {
			view, _, err = a.PostToStatusPage(r.Context(), p, in)
		}
	}
	a.finishStatusPageMutation(w, r, p, view, err, draft)
}

func (a *App) postStatusPageUpdateHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	draft := statusPageDraft{Form: statusPageUpdateForm, NoticeID: noticeID, Stage: r.PostForm.Get("stage"), Message: r.PostForm.Get("message")}
	view, _, err := a.PostStatusPageUpdate(r.Context(), p, statuspage.PostUpdateInput{NoticeID: noticeID, Stage: statuspage.Stage(draft.Stage), Message: draft.Message})
	a.finishStatusPageMutation(w, r, p, view, err, draft)
}

func (a *App) resolveStatusPageNoticeHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	draft := statusPageDraft{Form: statusPageResolveForm, NoticeID: noticeID, ClosingMessage: r.PostForm.Get("closing_message")}
	view, _, err := a.ResolveStatusPageNotice(r.Context(), p, statuspage.ResolveInput{NoticeID: noticeID, ClosingMessage: draft.ClosingMessage})
	a.finishStatusPageMutation(w, r, p, view, err, draft)
}

// statusPageExpectedUpdatedAt reads the version of the notice a form was built
// from, its updated_at. A form without it is refused, so no edit or delete
// goes through without the check that the page was up to date.
func statusPageExpectedUpdatedAt(form url.Values) (*time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, form.Get("expected_updated_at"))
	if err != nil {
		return nil, fmt.Errorf("%w: the form does not say which version of the notice it was made from; reload the page and try again", ErrInvalid)
	}
	return &parsed, nil
}

// statusPageUpdateID reads the update ID of an edit or delete route.
func statusPageUpdateID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	updateID, err := strconv.ParseInt(r.PathValue("update_id"), 10, 64)
	if err != nil || updateID < 1 {
		HTTPError(w, ErrNotFound)
		return 0, false
	}
	return updateID, true
}

func (a *App) editStatusPageNoticeHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	form := r.PostForm
	draft := statusPageDraft{Form: statusPageEditNoticeForm, NoticeID: noticeID, Title: form.Get("title"), Impact: form.Get("impact"), ScheduledStartAt: form.Get("scheduled_start_at"), ScheduledEndAt: form.Get("scheduled_end_at"), AffectedComponentIDs: form["affected_component_ids"]}
	var view StatusPageView
	expected, err := statusPageExpectedUpdatedAt(form)
	if err == nil {
		zone, zoneErr := a.alertPageTimeZone(r.Context(), p)
		if zoneErr != nil {
			HTTPError(w, zoneErr)
			return
		}
		impact, affected := statuspage.Impact(draft.Impact), draft.AffectedComponentIDs
		if affected == nil {
			affected = []string{}
		}
		in := statuspage.EditNoticeInput{NoticeID: noticeID, ExpectedUpdatedAt: expected, Title: &draft.Title, Impact: &impact, AffectedComponentIDs: &affected}
		// The planned times belong to a maintenance notice. For another impact
		// they are left out, so changing a maintenance notice to an outage
		// removes its window instead of being refused.
		if impact == statuspage.ImpactMaintenance {
			in.SetScheduledStartAt, in.SetScheduledEndAt = true, true
			if in.ScheduledStartAt, err = teamLocalTime(draft.ScheduledStartAt, zone, "planned start"); err == nil {
				in.ScheduledEndAt, err = teamLocalTime(draft.ScheduledEndAt, zone, "planned end")
			}
		}
		if err == nil {
			view, _, err = a.EditStatusPageNotice(r.Context(), p, in)
		}
	}
	a.finishStatusPageMutation(w, r, p, view, err, draft)
}

func (a *App) deleteStatusPageNoticeHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	draft := statusPageDraft{Form: statusPageDeleteNoticeForm, NoticeID: noticeID}
	var view StatusPageView
	var err error
	if r.PostForm.Get("confirm_delete") != "1" {
		err = fmt.Errorf("%w: tick the box to confirm that notice %d is deleted from the status page", ErrInvalid, noticeID)
	}
	var expected *time.Time
	if err == nil {
		expected, err = statusPageExpectedUpdatedAt(r.PostForm)
	}
	if err == nil {
		view, _, err = a.DeleteStatusPageNotice(r.Context(), p, statuspage.DeleteNoticeInput{NoticeID: noticeID, ExpectedUpdatedAt: expected})
	}
	a.finishStatusPageMutation(w, r, p, view, err, draft)
}

func (a *App) editStatusPageUpdateHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	updateID, ok := statusPageUpdateID(w, r)
	if !ok {
		return
	}
	draft := statusPageDraft{Form: statusPageEditUpdateForm, NoticeID: noticeID, UpdateID: updateID, Stage: r.PostForm.Get("stage"), Message: r.PostForm.Get("message")}
	var view StatusPageView
	expected, err := statusPageExpectedUpdatedAt(r.PostForm)
	if err == nil {
		view, _, err = a.EditStatusPageUpdate(r.Context(), p, statuspage.EditUpdateInput{NoticeID: noticeID, UpdateID: updateID, ExpectedUpdatedAt: expected, Message: &draft.Message, SetStage: true, Stage: statuspage.Stage(draft.Stage)})
	}
	a.finishStatusPageMutation(w, r, p, view, err, draft)
}

func (a *App) deleteStatusPageUpdateHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	updateID, ok := statusPageUpdateID(w, r)
	if !ok {
		return
	}
	draft := statusPageDraft{Form: statusPageDeleteUpdateForm, NoticeID: noticeID, UpdateID: updateID}
	var view StatusPageView
	expected, err := statusPageExpectedUpdatedAt(r.PostForm)
	if err == nil {
		view, _, err = a.DeleteStatusPageUpdate(r.Context(), p, statuspage.DeleteUpdateInput{NoticeID: noticeID, UpdateID: updateID, ExpectedUpdatedAt: expected})
	}
	a.finishStatusPageMutation(w, r, p, view, err, draft)
}

// statusPageCanPost reports whether members can post, for the incident
// page's "Post to the status page" button.
func (a *App) statusPageCanPost(ctx context.Context) (bool, error) {
	var ready bool
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM status_page_setup WHERE r2_access_key_id!='')`).Scan(&ready)
	})
	return ready, err
}
