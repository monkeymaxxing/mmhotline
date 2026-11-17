package app

import "net/http"

func (a *App) RegisterSlackRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /connect-slack", a.connectSlackHTTP)
}

func (a *App) connectSlackHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	connection, err := a.StartSlackConnection(r.Context(), principal, r.PostForm.Get("slack_connection"))
	if err != nil {
		a.renderContactMethods(w, r, principal, errorStatus(err), publicError(err), nil, nil)
		return
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "Connect Slack", frame, nil, connectSlackBody(connection))
}
