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

// statusPagePageData is the Status page's data.
type statusPagePageData struct {
	View           StatusPageView
	CSRF, Error    string
	Ready          bool
	Open, Resolved []statusPageNoticeView
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
	a.renderStatusPagePage(w, r, 200, view, "")
}

func (a *App) renderStatusPagePage(w http.ResponseWriter, r *http.Request, status int, view StatusPageView, message string) {
	data := statusPagePageData{View: view, CSRF: a.CSRFToken(w, r), Error: message, Ready: view.PublishedStatusJSON != nil, settingsFileNoticeView: a.settingsFileNoticeView()}
	if view.PublishedStatusJSON != nil {
		data.Open = viewStatusPageNotices(view.PublishedStatusJSON.OpenNotices())
		data.Resolved = viewStatusPageNotices(view.PublishedStatusJSON.ResolvedNotices(5))
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, status, "Status page", frame, nil, statusPagePageBody(frame, data))
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
