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

// alertSourceStatusPanelClass marks the status panel of a source with no recent check-in.
func alertSourceStatusPanelClass(tone string) string {
	if tone == "warn" {
		return "warn"
	}
	return ""
}

// nodeMaintenanceText says how long a server listed in a check-in stays in maintenance.
func nodeMaintenanceText(frame pageFrame, node NodeInMaintenance) string {
	text := node.NodeName + " until " + frame.Clock(node.MaintenanceUntil)
	if node.StartedBy != "" {
		text += ", started by " + node.StartedBy
	}
	if node.Reason != "" {
		text += ": " + node.Reason
	}
	return text
}

// plannedDowntimeText says how long planned downtime lasts and who started it.
func plannedDowntimeText(frame pageFrame, downtime PlannedDowntimeView) string {
	text := "Until " + frame.Clock(downtime.Until) + ", " + frame.RelativeTime(downtime.Until)
	if downtime.StartedByName != "" {
		text += ", started by " + downtime.StartedByName
	}
	if downtime.Reason != "" {
		text += ": " + downtime.Reason
	}
	return text + "."
}

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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, status, source.Name, frame, alertSourceHeading(source.Name), alertSourcePageBody(frame, data))
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
