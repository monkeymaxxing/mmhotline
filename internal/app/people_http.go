package app

import (
	"net/http"
)

func (a *App) RegisterPeopleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /people", a.peoplePage)
}

type peoplePageData struct {
	Viewer Member
	People []Member
	settingsFileNoticeView
}

// peoplePage lists the team. The settings file's people part adds, changes,
// and deactivates people, so the page only shows them.
func (a *App) peoplePage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	data := peoplePageData{settingsFileNoticeView: a.settingsFileNoticeView()}
	var err error
	data.Viewer, err = a.CurrentMember(r.Context(), p)
	if err != nil {
		a.browserAccessError(w, r, err)
		return
	}
	data.People, err = a.TeamMembers(r.Context(), p)
	if err != nil {
		a.browserAccessError(w, r, err)
		return
	}
	RenderPage(w, 200, "People", peopleBody, data)
}

var peopleBody = `{{$people := .}}<div class="stack people-page"><section class="panel"><div class="panel-head"><h2>The team</h2><span class="faint small">{{len .People}} {{if eq (len .People) 1}}person{{else}}people{{end}}</span></div><ul class="people-list">{{range .People}}<li><div><strong>{{.Name}}</strong>{{if eq .ID $people.Viewer.ID}} <span class="faint small">(you)</span>{{end}}<p class="small muted">{{.SignInEmailAddress}}</p></div><span class="tag nodot">{{capitalize .Role}}</span></li>{{else}}<li>No active people.</li>{{end}}</ul><p class="panel-foot">Administrators can open administration pages and see other people's contact methods.</p></section><details class="more"><summary>Where people are set</summary><div class="body">` + settingsFileNotice("<code>people</code>") + `<p class="small muted">The settings file adds, changes, and removes people. Removing a person stops sign-in and keeps the person's history.</p></div></details></div>`
