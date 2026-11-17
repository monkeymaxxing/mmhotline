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

// peoplePage lists the team (people.templ). The settings file's people part
// adds, changes, and deactivates people, so the page only shows them.
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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "People", frame, nil, peoplePageBody(data))
}
