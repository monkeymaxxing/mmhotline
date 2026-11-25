package app

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
)

// stylesheet is MMHotline's one stylesheet, which every page links. The
// Content Security Policy allows no other style, so pages have no style
// attribute and no style element.
//
//go:embed mmhotline.css
var stylesheet string

// developmentModeNotice is set once at startup when chat and phone sending services use fakes.
var developmentModeNotice string

// ShowDevelopmentModeNotice shows which services development mode fakes.
func ShowDevelopmentModeNotice(fakeSendingServicesURL string) {
	developmentModeNotice = fakeSendingServicesURL
}

// navigationPageWriter carries the request to readFrameForPage, which reads the
// frame's data only when it renders a page, never for a redirect, an asset,
// or JSON. member says whether the request's identity is a team member: a
// person refused at sign-in sees no menu and no red bar.
type navigationPageWriter struct {
	http.ResponseWriter
	app       *App
	request   *http.Request
	principal Principal
	member    bool
}

// readFrame reads the frame's data for the page being rendered.
func (n navigationPageWriter) readFrame() (pageFrame, error) {
	switch {
	case n.app == nil:
		// A page rendered outside Handler, as some tests render one.
		return pageFrame{TimeZone: time.UTC, Now: time.Now().UTC()}, nil
	case !n.member:
		return pageFrame{TimeZone: time.UTC, Now: n.app.Now().UTC()}, nil
	}
	return n.app.readPageFrame(n.request.Context(), n.principal, n.request.URL.Path)
}

// readFrameForPage reads the frame's data for the page about to be rendered
// on w. When reading fails, it has already written the error page and
// returns false.
func readFrameForPage(w http.ResponseWriter) (pageFrame, bool) {
	var navigation navigationPageWriter
	if pageWriter, ok := w.(*navigationPageWriter); ok {
		navigation = *pageWriter
	}
	frame, err := navigation.readFrame()
	if err != nil {
		renderPageError(w, publicError(err), errorStatus(err))
		return frame, false
	}
	return frame, true
}

// renderPage writes a page written in templ, in the frame (layout.templ).
// heading is the page's own heading block, or nil for the frame's h1 with
// the title. Buffering prevents a render failure from leaving a
// half-successful page.
func renderPage(w http.ResponseWriter, status int, title string, frame pageFrame, heading, body templ.Component) {
	var b bytes.Buffer
	if err := pageLayout(title, developmentModeNotice, frame, heading, body).Render(context.Background(), &b); err != nil {
		renderPageError(w, "MMHotline could not display the requested page.", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// renderPageError can still show a browser a way back when reading the frame
// or executing a page fails. The fallback frame needs no database read.
func renderPageError(w http.ResponseWriter, reason string, status int) {
	if page, ok := w.(*navigationPageWriter); ok && browserPageRequest(page.request) {
		fallback := &navigationPageWriter{ResponseWriter: page.ResponseWriter, request: page.request}
		frame, _ := fallback.readFrame()
		renderPage(fallback, status, "Cannot display the page", frame, nil, pageErrorBody(reason))
		return
	}
	http.Error(w, reason, status)
}

// pageFrame is what the frame shows around a page. For a person who is not
// a team member it holds only the time zone and the moment, for the page's
// times; a member's frame also has the team's name, the menu, the red bar,
// and the footer's time zone line.
type pageFrame struct {
	Member                 bool
	TeamName, TimeZoneLine string
	Menu                   []menuGroup
	// WaitingSOS is the red bar, nil while no SOS waits for a claim.
	WaitingSOS *waitingSOSBar
	TimeZone   *time.Location
	Now        time.Time
}

// Clock writes t as pages show it, in the team's time zone (pageClock).
func (f pageFrame) Clock(t time.Time) string { return pageClock(t, f.Now, f.TimeZone) }

// RelativeTime says how long before or after the page's moment t is
// (relativeTime).
func (f pageFrame) RelativeTime(t time.Time) string { return relativeTime(t, f.Now) }

// readPageFrame reads, in one transaction, what the frame shows member p on
// the page at path.
func (a *App) readPageFrame(ctx context.Context, p Principal, path string) (pageFrame, error) {
	frame := pageFrame{Member: true, Now: a.Now().UTC()}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT name FROM team_info`).Scan(&frame.TeamName); err != nil {
			return err
		}
		if frame.TimeZone, err = teamTimeZone(ctx, tx); err != nil {
			return err
		}
		frame.TimeZoneLine = timeZoneLine(frame.TimeZone)
		// An approval request waits for a person; a warned run goes ahead
		// by itself unless someone stops it.
		var approvals int
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM triage_agent_playbook_runs_waiting_for_a_person WHERE state=? AND rule=?) + (SELECT count(*) FROM triage_agent_refused_commits WHERE reviewed_at IS NULL)`, approvalOpen, getPermissionThenAct).Scan(&approvals); err != nil {
			return err
		}
		frame.Menu = pageMenu(m, path, approvals)
		frame.WaitingSOS, err = readWaitingSOSBar(ctx, tx, m, path, frame.TimeZone, frame.Now)
		return err
	})
	return frame, err
}

// menuGroup is one zone of the menu. The first group holds day-to-day links
// with no heading; Collapsible groups fold under a <details> summary and open
// automatically when one of their links is current; Footer groups sit after a
// divider and anchor at the bottom of the desktop sidebar.
type menuGroup struct {
	Heading     string
	Links       []menuLink
	Collapsible bool
	Open        bool
	Footer      bool
}

// menuLink is one link of the menu. Current is aria-current's value: "page"
// on the link to the page shown, "true" on the link to the part of
// MMHotline the page belongs to, such as Alerts on one alert's page, and ""
// elsewhere. Count is a number beside the label, shown when above zero.
type menuLink struct {
	Href, Label, Current string
	Count                int
}

// pageMenu is the menu for member m on the page at path. Every member sees
// every page but Sending services and Triage agent, which are for
// administrators.
func pageMenu(m Member, path string, approvalsWaiting int) []menuGroup {
	link := func(href, label string) menuLink {
		l := menuLink{Href: href, Label: label}
		switch {
		case path == href:
			l.Current = "page"
		case strings.HasPrefix(path, href+"/"):
			l.Current = "true"
		}
		return l
	}
	approvals := link("/approvals", "Approvals")
	approvals.Count = approvalsWaiting
	settings := []menuLink{link("/alert-sources", "Alert sources"), link("/people", "People")}
	if m.Role == RoleAdministrator {
		settings = append(settings, link("/sending-services", "Sending services"), link("/triage-agent", "Triage agent"))
	}
	settings = append(settings, link("/agent-playbook-rules", "Playbook rules"))
	settingsOpen := false
	for _, l := range settings {
		if l.Current != "" {
			settingsOpen = true
			break
		}
	}
	return []menuGroup{
		{Links: []menuLink{link("/dashboard", "Dashboard"), link("/alerts", "Alerts"), approvals, link("/on-call", "On call"), link("/status-page", "Status page")}},
		{Heading: "Settings", Links: settings, Collapsible: true, Open: settingsOpen},
		// The On call link marks the on-call page as current.
		{Heading: "Signed in as " + m.Name, Links: []menuLink{link("/contact-methods", "Contact methods"), {Href: "/on-call#your-availability", Label: "Availability"}}, Footer: true},
	}
}

// waitingSOSBar is the red bar at the top of a page while an SOS incident
// waits for a claim: the oldest such incident, with its countdown, and how
// many more wait.
type waitingSOSBar struct {
	ID, Title string
	Countdown incidentCountdown
	More      int
}

// readWaitingSOSBar reads the red bar for member m on the page at path. The
// dashboard lists every SOS that waits, and an incident's own page shows its
// countdown, so neither repeats it in the bar.
func readWaitingSOSBar(ctx context.Context, tx *sql.Tx, m Member, path string, zone *time.Location, now time.Time) (*waitingSOSBar, error) {
	if path == "/dashboard" {
		return nil, nil
	}
	var pageIncidentID string
	if id, found := strings.CutPrefix(path, "/incidents/"); found {
		pageIncidentID = id
	}
	var waiting int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM incidents WHERE status='open' AND id!=?`, pageIncidentID).Scan(&waiting); err != nil || waiting == 0 {
		return nil, err
	}
	// Within one second, the opening alerts' order says which is older.
	i, err := scanIncident(tx.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.status='open' AND i.id!=? ORDER BY i.created_at,e.sequence LIMIT 1`, pageIncidentID))
	if err != nil {
		return nil, err
	}
	step, err := nextIncidentStep(ctx, tx, i)
	if err != nil {
		return nil, err
	}
	return &waitingSOSBar{ID: i.ID, Title: i.Title, Countdown: viewIncidentCountdown(step, m.ID, zone, now), More: waiting - 1}, nil
}

// teamTimeZone is the team's time zone, in which pages show times: the
// time zone of its schedule (on_call_schedule.time_zone in the settings
// file), or UTC while it has none. Notifications keep their own format
// (displayTime), and the public status page stays in UTC.
func teamTimeZone(ctx context.Context, tx *sql.Tx) (*time.Location, error) {
	state, err := ReadSchedules(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, c := range state.Configs {
		if c.Kind == state.ActiveKind {
			return time.LoadLocation(c.TimeZone)
		}
	}
	return time.UTC, nil
}

// timeZoneLine names the time zone of a page's times, for its footer:
// "Times are New York time (America/New_York), the team's time zone."
func timeZoneLine(zone *time.Location) string {
	name := zone.String()
	if name == "UTC" {
		return "Times are in UTC."
	}
	city := strings.ReplaceAll(name[strings.LastIndex(name, "/")+1:], "_", " ")
	if city == name {
		return "Times are in " + name + ", the team's time zone."
	}
	return "Times are " + city + " time (" + name + "), the team's time zone."
}

// partsAfterSeparators splits s after each slash and underscore:
// "app002/disk_usage" is "app002/", "disk_", and "usage". The last part may
// be "".
func partsAfterSeparators(s string) []string {
	var parts []string
	for {
		i := strings.IndexAny(s, "/_")
		if i < 0 {
			return append(parts, s)
		}
		parts, s = append(parts, s[:i+1]), s[i+1:]
	}
}

// pageClock writes a time as pages show it: to the minute, in the team's
// time zone, with the day only when it is not today there: "17:42",
// "28 Oct 17:42", "28 Oct 2025 17:42".
func pageClock(t, now time.Time, zone *time.Location) string {
	if t.IsZero() {
		return "—"
	}
	local, today := t.In(zone), now.In(zone)
	switch {
	case local.Year() != today.Year():
		return local.Format("2 Jan 2006 15:04")
	case local.YearDay() != today.YearDay():
		return local.Format("2 Jan 15:04")
	}
	return local.Format("15:04")
}

// relativeTime says how long before or after now t is, in whole minutes,
// hours, or days: "just now", "8 minutes ago", "in 2 minutes", "3 hours
// ago", "2 days ago".
func relativeTime(t, now time.Time) string {
	d, future := now.Sub(t), t.After(now)
	if future {
		d = -d
	}
	var amount string
	switch {
	case d < time.Minute && future:
		return "in less than a minute"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		amount = countText(int(d/time.Minute), "minute", "minutes")
	case d < 24*time.Hour:
		amount = countText(int(d/time.Hour), "hour", "hours")
	default:
		amount = countText(int(d/(24*time.Hour)), "day", "days")
	}
	if future {
		return "in " + amount
	}
	return amount + " ago"
}

// durationText says a duration in words: "30 seconds", "1 minute", "1 minute
// 30 seconds".
func durationText(d time.Duration) string {
	minutes, seconds := int(d/time.Minute), int(d%time.Minute/time.Second)
	var parts []string
	switch {
	case minutes == 1:
		parts = append(parts, "1 minute")
	case minutes > 1:
		parts = append(parts, fmt.Sprintf("%d minutes", minutes))
	}
	if seconds > 0 || minutes == 0 {
		parts = append(parts, fmt.Sprintf("%d seconds", seconds))
	}
	return strings.Join(parts, " ")
}

func severityLabel(value string) string {
	switch value {
	case "info":
		return "Info"
	case "warn":
		return "Warning"
	case "sos":
		return "SOS"
	case "resolved-info":
		return "Resolved info"
	case "resolved-warn":
		return "Resolved warning"
	case "resolved-sos":
		return "Resolved SOS"
	}
	return value
}

// severityMeaning says in plain words what a severity means, for a reader who
// meets it for the first time on a page.
func severityMeaning(value string) string {
	switch value {
	case "info":
		return "for information only"
	case "warn":
		return "needs attention, but nobody is woken"
	case "sos":
		return "an urgent problem: it wakes the person on call"
	case "resolved-info", "resolved-warn", "resolved-sos":
		return "the problem is over"
	}
	return ""
}
func scheduleTypeLabel(value string) string {
	switch value {
	case "day_of_week":
		return "By day of the week"
	case "hour_of_day":
		return "By hour of the day"
	case "weekly_rotation":
		return "Weekly rotation"
	case "custom_schedule_code":
		return "Custom schedule"
	}
	return value
}
func backupKindLabel(value string) string {
	switch value {
	case "fixed_person":
		return "A fixed person"
	case "next_in_rotation":
		return "Next person in the rotation"
	}
	return "Everyone available"
}
func incidentStatusLabel(value string) string {
	switch value {
	case "open":
		return "Not claimed yet"
	case "claimed":
		return "Claimed"
	case "resolved":
		return "Resolved"
	}
	return value
}
