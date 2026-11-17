package app

import (
	"net/http"
	"strings"
)

func (a *App) RegisterDownAlarmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /mmhotline-down-alarm/send-test", a.sendDownAlarmTestHTTP)
}

// downAlarmCard is the dashboard's MMHotline-down alarm panel: whether MMHotline checks
// in, whom the MMHotline-down alarm wakes if MMHotline goes quiet, what it sent, and a
// test; the dashboard's data has DownAlarm, CSRF, and the settings file's
// notice.
var downAlarmCard = `{{with .DownAlarm}}<section id="mmhotline-down-alarm" data-live class="panel" aria-labelledby="mmhotline-down-alarm-heading"><div class="panel-head"><h2 id="mmhotline-down-alarm-heading">MMHotline-down alarm</h2>{{if .CheckInsFailingSince}}<span class="tag warn">Not checking in</span>{{else if .LastCheckInAcceptedAt}}<span class="tag ok">Checking in</span>{{else}}<span class="tag">Starting</span>{{end}}</div><div class="panel-body">` +
	`{{with .CheckInsFailingSince}}<p class="line-warn small">MMHotline has failed to check in with the MMHotline-down alarm since {{clock .}}. Without check-ins, the MMHotline-down alarm acts as if MMHotline were down: it sends its urgent alerts once, then nothing more, even if MMHotline really goes down later.</p><details class="fold"><summary>Why</summary><p class="small">{{$.DownAlarm.LastCheckInError}}</p></details>{{else}}{{with .LastCheckInAcceptedAt}}<p>MMHotline last checked in {{relativeTime .}}.</p>{{else}}<p>MMHotline has not checked in with the MMHotline-down alarm since MMHotline started.</p>{{end}}{{end}}` +
	`{{with .HeldMessages}}<p class="faint small">If MMHotline goes quiet, the MMHotline-down alarm wakes:</p><ul class="bullets faint small">{{range .}}<li>{{.ContactMethodType}} to {{.PersonName}} after {{.DelayMinutes}} minutes</li>{{end}}</ul>{{else}}<p class="line-warn small">If MMHotline goes quiet, the MMHotline-down alarm wakes nobody: nobody on call gets SOS alerts by Pushover or automated voice call.</p>{{end}}` +
	`{{with .RecentSends}}<p class="faint small">The MMHotline-down alarm sent:</p><ul class="bullets faint small">{{range .}}<li>{{.ContactMethodType}} to {{.PersonName}}, at {{clock .DueAt}}</li>{{end}}</ul>{{end}}` +
	`</div><div class="panel-foot"><p class="line-warn"><strong>THE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO.</strong></p><form method="post" action="/mmhotline-down-alarm/send-test" class="row"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="btn small">Send a test now</button><span>Each person above gets a test alert now. For a Pushover notification, MMHotline checks Pushover's receipt and cancels the repeats.</span></form>{{with $}}<details class="fold"><summary>Where the MMHotline-down alarm is set up</summary>` + settingsFileNotice("<code>mmhotline_down_alarm</code>") + `</details>{{end}}</div></section>{{end}}`

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
