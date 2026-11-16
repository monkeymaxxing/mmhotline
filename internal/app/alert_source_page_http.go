package app

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

func (a *App) RegisterAlertSourcePageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /alert-sources/{alert_source_id}", a.alertSourcePage)
	mux.HandleFunc("POST /alert-sources/{alert_source_id}/planned-downtime", a.alertSourcePlannedDowntimeHTTP)
}

type alertSourcePageData struct {
	CSRF, Error, Notice            string
	Source                         Source
	CheckIns                       AlertSourceCheckInStatus
	UntilDefault, Reason, TimeZone string
	Status, Tone, Summary          string
	settingsFileNoticeView
}

// checkInSummary describes what MMHotline has heard, never whether the
// services monitored by the sender are healthy. Recent signed alerts also
// count as hearing from a source, even while check-ins have stopped.
func (d *alertSourcePageData) checkInSummary(now time.Time) {
	switch {
	case !d.Source.Active:
		d.Status, d.Summary = "Disabled", "MMHotline refuses this source's signed alerts and check-ins. Earlier alerts and check-ins stay."
	case d.CheckIns.PlannedDowntime != nil:
		d.Status, d.Tone, d.Summary = "In planned downtime", "work", "MMHotline records check-ins but makes no alerts about missing check-ins during planned downtime. Ordinary signed alerts still arrive."
	case !d.CheckIns.Expected:
		d.Status, d.Summary = "Waiting for the first check-in", "No check-ins are expected until the first check-in arrives. A source that never checks in is not watched for silence."
	case now.Sub(d.CheckIns.LastCheckInAt) < alertSourceCheckInsCountAsStoppedAfter:
		d.Status, d.Tone, d.Summary = "Check-ins arriving", "ok", "The sending program checks in every 30 seconds. MMHotline watches for missing check-ins."
	case now.Sub(d.CheckIns.LastHeardAt) < alertSourceCheckInsCountAsStoppedAfter:
		d.Status, d.Summary = "Receiving alerts, but no recent check-in", "Recent signed alerts count as hearing from the source and delay alerts about missing check-ins. Only a check-in clears an alert about missing check-ins that already went out."
	default:
		d.Status, d.Tone, d.Summary = "No recent check-in", "warn", "Check the sending program, the sending server, and the network connection to MMHotline. The missing-check-in rules below set when MMHotline warns the team and wakes the person on call."
	}
}

var alertSourceBody = `{{define "heading"}}<div><p class="crumbs"><a href="/alert-sources">Alert sources</a></p><h1>{{.Title}}</h1></div>{{end}}{{$page := .}}<div class="stack alert-pages">{{if .Error}}<p class="form-error" role="alert">{{.Error}}</p>{{end}}{{if .Notice}}<p class="small" role="status">{{.Notice}}</p>{{end}}
<section class="panel {{if eq .Tone "warn"}}warn{{end}}"><div class="panel-head"><h2>{{.Status}}</h2>{{if eq .Tone "ok"}}<span class="tag ok">Checking in</span>{{else if eq .Tone "work"}}<span class="tag work">Downtime until {{clock .CheckIns.PlannedDowntime.Until}}</span>{{end}}</div><div class="panel-body"><p>{{.Summary}}</p>{{with .CheckIns}}{{if .Expected}}<p class="small muted">Last check-in from {{.SenderDescription}}: {{clock .LastCheckInAt}}, {{relativeTime .LastCheckInAt}}.</p>{{if .LastHeardAt.After .LastCheckInAt}}<p class="small muted">Last heard from the source at {{clock .LastHeardAt}}, {{relativeTime .LastHeardAt}}.</p>{{end}}{{end}}{{end}}{{if eq .Tone "warn"}}<p><a href="/alerts">Look for alerts in the alert history</a></p>{{end}}</div></section>
<div class="cols"><div class="col-main"><section class="panel"><div class="panel-head"><h2>Missing-check-in rules</h2></div><div class="panel-body">{{with .CheckIns}}<p class="small">After MMHotline last hears from the source:</p><dl class="kv"><dt>Warning</dt><dd>{{if .WarnAfterNoCheckInFor}}After {{minutes .WarnAfterNoCheckInFor}}. Nobody is woken.{{else}}Off{{end}}</dd><dt>Wake the person on call</dt><dd>After {{minutes .SOSAfterNoCheckInFor}} without an alert or check-in.</dd></dl>{{end}}<details class="fold"><summary>Where the alert source is set</summary>` + settingsFileNotice("<code>alert_sources</code>") + `</details></div></section>
{{with .CheckIns.NodesInWholeNodeMaintenance}}<details class="more"><summary>Servers listed in maintenance</summary><div class="body"><p class="small muted">From the source's latest check-in. Maintenance on the server sending check-ins counts as planned downtime.</p><ul class="bullets small">{{range .}}<li>{{.NodeName}} until {{clock .MaintenanceUntil}}{{if .StartedBy}}, started by {{.StartedBy}}{{end}}{{if .Reason}}: {{.Reason}}{{end}}</li>{{end}}</ul></div></details>{{end}}</div>
<div class="col-side"><section class="panel"><div class="panel-head"><h2>Planned downtime</h2></div><div class="panel-body">{{with .CheckIns.PlannedDowntime}}<p>Until {{clock .Until}}, {{relativeTime .Until}}{{if .StartedByName}}, started by {{.StartedByName}}{{end}}{{if .Reason}}: {{.Reason}}{{end}}.</p>{{if .SenderMaintenanceNodeName}}<p class="small muted">Check-ins list server {{.SenderMaintenanceNodeName}} in maintenance. End the maintenance window in the sending tool.</p>{{else}}<form method="post" action="/alert-sources/{{$page.Source.ID}}/planned-downtime"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="end" value="1"><button class="btn">End planned downtime now</button></form>{{end}}{{else}}{{if .Source.Active}}<form method="post" action="/alert-sources/{{.Source.ID}}/planned-downtime" class="source-downtime"><input type="hidden" name="csrf" value="{{.CSRF}}"><label class="field">Until ({{.TimeZone}})<input type="datetime-local" name="until" value="{{.UntilDefault}}" required></label><label class="field">Reason (optional)<input name="reason" value="{{.Reason}}" maxlength="200"></label><p class="small muted">Stop alerts about missing check-ins until the chosen time, at most 7 days ahead. Ordinary signed alerts still arrive.</p><p><button class="btn primary">Start planned downtime</button></p></form>{{else}}<p class="small muted">Enable the source in the settings file before starting planned downtime.</p>{{end}}{{end}}</div></section></div></div></div>`

// alertPageTimeZone uses the frame's single source for the team's time zone
// when interpreting date filters and datetime-local form values.
func (a *App) alertPageTimeZone(ctx context.Context, p Principal) (*time.Location, error) {
	var zone *time.Location
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		zone, err = teamTimeZone(ctx, tx)
		return err
	})
	return zone, err
}

func (a *App) alertSourcePage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodPrincipal(w, r)
	if !ok {
		return
	}
	notice := ""
	switch r.URL.Query().Get("done") {
	case "downtime_started":
		notice = "Planned downtime started."
	case "downtime_ended":
		notice = "Planned downtime ended."
	}
	a.renderAlertSource(w, r, p, 200, "", notice)
}

func (a *App) renderAlertSource(w http.ResponseWriter, r *http.Request, p Principal, status int, message, notice string) {
	source, err := a.SourceByID(r.Context(), p, r.PathValue("alert_source_id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	checkIns, err := a.AlertSourceCheckInStatus(r.Context(), p, source.ID)
	if err != nil {
		HTTPError(w, err)
		return
	}
	zone, err := a.alertPageTimeZone(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	data := alertSourcePageData{CSRF: a.CSRFToken(w, r), Error: message, Notice: notice, Source: source, CheckIns: checkIns, UntilDefault: a.Now().In(zone).Add(time.Hour).Format("2006-01-02T15:04"), TimeZone: zone.String(), settingsFileNoticeView: a.settingsFileNoticeView()}
	if message != "" {
		data.UntilDefault, data.Reason = r.PostForm.Get("until"), r.PostForm.Get("reason")
	}
	data.checkInSummary(a.Now())
	RenderPage(w, status, source.Name, alertSourceBody, data)
}

func (a *App) alertSourcePlannedDowntimeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	target := plannedDowntimeTarget{AlertSourceID: r.PathValue("alert_source_id")}
	done, err := a.plannedDowntimeForm(r, p, target)
	if err != nil {
		a.renderAlertSource(w, r, p, errorStatus(err), publicError(err), "")
		return
	}
	http.Redirect(w, r, "/alert-sources/"+target.AlertSourceID+"?done="+done, http.StatusSeeOther)
}

// plannedDowntimeForm starts or ends planned downtime from a posted form.
func (a *App) plannedDowntimeForm(r *http.Request, p Principal, target plannedDowntimeTarget) (string, error) {
	if r.PostForm.Get("end") == "1" {
		return "downtime_ended", a.EndPlannedDowntime(r.Context(), p, target)
	}
	zone, err := a.alertPageTimeZone(r.Context(), p)
	if err != nil {
		return "", err
	}
	until, err := time.ParseInLocation("2006-01-02T15:04", r.PostForm.Get("until"), zone)
	if err != nil {
		return "", ErrInvalid
	}
	return "downtime_started", a.StartPlannedDowntime(r.Context(), p, target, until, r.PostForm.Get("reason"))
}
