package app

import (
	"net/http"
	"strings"
)

func (a *App) RegisterDownAlarmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /mmhotline-down-alarm/send-test", a.sendDownAlarmTestHTTP)
}

func (a *App) sendDownAlarmTestHTTP(w http.ResponseWriter, r *http.Request) {
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		HTTPError(w, err)
		return
	}
	view, err := a.SendDownAlarmTest(r.Context(), p)
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	if err != nil {
		renderPage(w, errorStatus(err), "MMHotline-down alarm test", frame, nil, downAlarmTestErrorBody(publicError(err)))
		return
	}
	renderPage(w, 200, "MMHotline-down alarm test", frame, nil, downAlarmTestResultBody(testSendLines(view)))
}

func testSendLines(view DownAlarmView) []downAlarmTestLine {
	var lines []downAlarmTestLine
	for _, s := range view.TestSends {
		lines = append(lines, downAlarmTestLine{s.ContactMethodType, s.PersonName, strings.TrimPrefix(s.testText(), "Test: "+s.ContactMethodType+" to "+s.PersonName+": ")})
	}
	return lines
}
