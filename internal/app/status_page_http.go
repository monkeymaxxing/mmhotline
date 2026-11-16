package app

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"mmhotline/internal/statuspage"
)

func (a *App) RegisterStatusPageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /status-page", a.statusPagePage)
	mux.HandleFunc("POST /status-page/notices", a.postStatusPageNoticeHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/updates", a.postStatusPageNoticeHTTP)
	mux.HandleFunc("POST /status-page/notices/{notice_id}/resolve", a.resolveStatusPageNoticeHTTP)
}

type statusPageUpdateView struct {
	Posted  time.Time
	Message string
}
type statusPageNoticeView struct {
	NoticeID int64
	Opened   time.Time
	Resolved *time.Time
	Updates  []statusPageUpdateView
}

func viewStatusPageNotices(notices []statuspage.Notice) []statusPageNoticeView {
	var views []statusPageNoticeView
	for _, n := range notices {
		v := statusPageNoticeView{NoticeID: n.NoticeID, Opened: n.OpenedAt}
		if n.ResolvedAt != nil {
			v.Resolved = n.ResolvedAt
		}
		for _, u := range n.Updates {
			v.Updates = append(v.Updates, statusPageUpdateView{u.PostedAt, u.Message})
		}
		views = append(views, v)
	}
	return views
}

const statusPageNoticeUpdates = `<ol class="settings-updates">{{range .Updates}}<li><p class="faint small">{{clock .Posted}}</p><p class="result">{{.Message}}</p></li>{{end}}</ol>`

var statusPagePageBody = `<div class="settings-page stack">{{if .Error}}<p class="form-error" role="alert">{{.Error}}</p>{{end}}{{if not .View.IsSetUp}}<section class="panel"><div class="panel-head"><h2>Status page not set up</h2></div><div class="panel-body"><p>An administrator creates the status page in the team's Cloudflare account from a laptop, with <code>mmhotline status-page set-up</code>, then adds the <code>status_page</code> section to the settings file and applies the settings.</p></div></section>{{else}}<section class="panel"><div class="panel-head"><h2>Public status page</h2><span class="tag ok">Set up</span></div><div class="panel-body"><p><a href="{{.View.PageURL}}">{{.View.PageURL}}</a></p><p class="small muted">Customers see only the notices your team posts. The public page shows times in UTC and changes only when someone posts.</p>{{if .View.ReadError}}<p class="form-error" role="alert">{{.View.ReadError}}</p>{{end}}</div></section>{{if .Ready}}{{$csrf := .CSRF}}{{range .Open}}<section class="panel"><div class="panel-head"><h2>Open notice {{.NoticeID}}</h2><span class="tag warn">Open</span></div><div class="panel-body"><p class="faint small">Opened {{clock .Opened}}</p>` + statusPageNoticeUpdates + `<form class="stack" method="post" action="/status-page/notices/{{.NoticeID}}/updates"><input type="hidden" name="csrf" value="{{$csrf}}"><label class="field">Add an update<textarea name="message" required maxlength="5000"></textarea></label><div><button class="btn primary">Post the update</button></div></form><details class="fold"><summary>Resolve this notice</summary><form class="stack" method="post" action="/status-page/notices/{{.NoticeID}}/resolve"><input type="hidden" name="csrf" value="{{$csrf}}"><label class="field">Closing message (optional)<span class="faint small">The closing message is added as the last update.</span><textarea name="closing_message" maxlength="5000"></textarea></label><div><button class="btn">Resolve this notice</button></div></form></details></div></section>{{else}}<section class="panel quiet"><div class="panel-body"><p>No open notices: the public page says that everything is working.</p></div></section>{{end}}<section class="panel"><div class="panel-head"><h2>Post a new notice</h2></div><div class="panel-body"><form class="stack" method="post" action="/status-page/notices"><input type="hidden" name="csrf" value="{{$csrf}}"><label class="field">Message<textarea name="message" required maxlength="5000">` + StatusPageGenericMessage + `</textarea></label><div><button class="btn primary">Post to the status page</button></div></form></div></section>{{with .Resolved}}<details class="more"><summary>Recently resolved ({{len .}})</summary><div class="body stack">{{range .}}<section class="stack"><h3>Notice {{.NoticeID}}, resolved {{clock .Resolved}}</h3>` + statusPageNoticeUpdates + `</section>{{end}}</div></details>{{end}}{{end}}{{end}}<details class="more"><summary>Where status page settings are set</summary><div class="body">` + settingsFileNotice("<code>status_page</code>") + `</div></details></div>`

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
	a.renderStatusPagePage(w, r, 200, view, "")
}

func (a *App) renderStatusPagePage(w http.ResponseWriter, r *http.Request, status int, view StatusPageView, message string) {
	data := struct {
		View           StatusPageView
		CSRF, Error    string
		Ready          bool
		Open, Resolved []statusPageNoticeView
		settingsFileNoticeView
	}{View: view, CSRF: a.CSRFToken(w, r), Error: message, Ready: view.PublishedStatusJSON != nil, settingsFileNoticeView: a.settingsFileNoticeView()}
	if view.PublishedStatusJSON != nil {
		data.Open = viewStatusPageNotices(view.PublishedStatusJSON.OpenNotices())
		data.Resolved = viewStatusPageNotices(view.PublishedStatusJSON.ResolvedNotices(5))
	}
	RenderPage(w, status, "Status page", statusPagePageBody, data)
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

func (a *App) finishStatusPageMutation(w http.ResponseWriter, r *http.Request, p Principal, view StatusPageView, err error) {
	if err != nil {
		current, readErr := a.StatusPage(r.Context(), p)
		if readErr != nil {
			HTTPError(w, err)
			return
		}
		a.renderStatusPagePage(w, r, errorStatus(err), current, publicError(err))
		return
	}
	if view.ReadError != "" {
		a.renderStatusPagePage(w, r, 200, view, view.ReadError)
		return
	}
	http.Redirect(w, r, "/status-page", http.StatusSeeOther)
}

func (a *App) postStatusPageNoticeHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	var view StatusPageView
	var err error
	if incidentID := r.PostForm.Get("incident_id"); incidentID != "" && noticeID == 0 {
		view, _, err = a.PostIncidentToStatusPage(r.Context(), p, incidentID)
	} else {
		view, _, err = a.PostToStatusPage(r.Context(), p, noticeID, r.PostForm.Get("message"))
	}
	a.finishStatusPageMutation(w, r, p, view, err)
}

func (a *App) resolveStatusPageNoticeHTTP(w http.ResponseWriter, r *http.Request) {
	p, noticeID, ok := a.statusPageMutation(w, r)
	if !ok {
		return
	}
	view, _, err := a.ResolveStatusPageNotice(r.Context(), p, noticeID, r.PostForm.Get("closing_message"))
	a.finishStatusPageMutation(w, r, p, view, err)
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
