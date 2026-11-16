package app

import (
	"net/http"
	"strconv"
)

func (a *App) RegisterIncidentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /incidents/new", a.manualSOSHTTP)
	mux.HandleFunc("POST /incidents/{id}/claim", a.claimIncidentHTTP)
	mux.HandleFunc("POST /incidents/{id}/resolve", a.resolveIncidentHTTP)
	mux.HandleFunc("POST /incidents/{id}/escalate", a.escalateIncidentHTTP)
}
func (a *App) incidentMutation(w http.ResponseWriter, r *http.Request) (Principal, int64, bool) {
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return Principal{}, 0, false
	}
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		HTTPError(w, err)
		return Principal{}, 0, false
	}
	revision, err := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
	if err != nil || revision < 1 {
		HTTPError(w, ErrInvalid)
		return Principal{}, 0, false
	}
	return p, revision, true
}
func (a *App) claimIncidentHTTP(w http.ResponseWriter, r *http.Request) {
	p, revision, ok := a.incidentMutation(w, r)
	if !ok {
		return
	}
	if _, err := a.ClaimIncident(r.Context(), p, r.PathValue("id"), revision, r.PostForm.Get("note")); err != nil {
		HTTPError(w, err)
		return
	}
	http.Redirect(w, r, "/incidents/"+r.PathValue("id"), http.StatusSeeOther)
}
func (a *App) resolveIncidentHTTP(w http.ResponseWriter, r *http.Request) {
	p, revision, ok := a.incidentMutation(w, r)
	if !ok {
		return
	}
	if _, err := a.ResolveIncident(r.Context(), p, r.PathValue("id"), revision, r.PostForm.Get("note")); err != nil {
		HTTPError(w, err)
		return
	}
	http.Redirect(w, r, "/incidents/"+r.PathValue("id"), http.StatusSeeOther)
}
func (a *App) escalateIncidentHTTP(w http.ResponseWriter, r *http.Request) {
	p, revision, ok := a.incidentMutation(w, r)
	if !ok {
		return
	}
	if _, err := a.EscalateIncident(r.Context(), p, r.PathValue("id"), r.PostForm.Get("target"), revision); err != nil {
		HTTPError(w, err)
		return
	}
	http.Redirect(w, r, "/incidents/"+r.PathValue("id"), http.StatusSeeOther)
}
func (a *App) manualSOSHTTP(w http.ResponseWriter, r *http.Request) {
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		HTTPError(w, err)
		return
	}
	title, message := r.PostForm.Get("title"), r.PostForm.Get("message")
	e, err := a.ManualSOS(r.Context(), p, title, message)
	if err != nil {
		RenderPage(w, errorStatus(err), "Raise an SOS by hand", `<section class="panel"><div class="panel-body"><p class="form-error" role="alert">{{.Error}}</p><form class="stack manual-sos-form" method="post" action="/incidents/new"><input type="hidden" name="csrf" value="{{.CSRF}}"><label class="field">Title<input name="title" value="{{.Title}}" maxlength="256" required></label><label class="field">Message<textarea name="message">{{.Message}}</textarea></label><div class="row"><button class="btn primary">Raise the SOS</button><a href="/dashboard">Back to the dashboard</a></div></form></div></section>`, struct{ Error, CSRF, Title, Message string }{publicError(err), a.CSRFToken(w, r), title, message})
		return
	}
	http.Redirect(w, r, "/incidents/"+e.IncidentID, http.StatusSeeOther)
}
