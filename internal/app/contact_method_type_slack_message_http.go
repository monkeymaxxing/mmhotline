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
	RenderPage(w, 200, "Connect Slack", `<div class="stack"><section class="panel"><div class="panel-head"><h2>{{if .SharedChannel}}Connect a shared Slack channel{{else}}Connect your Slack direct messages{{end}}</h2></div><div class="panel-body">{{if .SharedChannel}}<p>Invite the MMHotline app to your Slack channel, then mention the MMHotline app with the connection code:</p><pre><code>@MMHotline {{.Code}}</code></pre><p class="small muted">Alerts will be shared with the team in the connected Slack channel.</p>{{else}}<p>Open direct messages with the MMHotline app and send the connection code:</p><pre><code>{{.Code}}</code></pre>{{end}}<p><a class="btn primary" href="{{.AppURL}}">Open MMHotline in Slack</a></p><p class="faint small">The connection code works once, for 15 minutes. Share the connection code only in the Slack conversation you want to connect.</p></div></section><p><a href="/contact-methods">Back to contact methods</a></p></div>`, connection)
}
