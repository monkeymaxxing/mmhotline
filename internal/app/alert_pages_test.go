package app

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestAlertPagesShowWhatEachAlertDid(t *testing.T) {
	r := newOnCallTeamRig(t)
	h, browser := r.h, r.maya
	p := Principal{SignInEmailAddress: "maya@example.test"}
	source, err := createAlertSource(context.Background(), h.app, p, "Monitoring")
	if err != nil {
		t.Fatal(err)
	}
	var incidentID string
	for n, tc := range []struct{ severity, title, outcome string }{
		{"sos", "Database down", "Opened SOS incident: Database down"},
		{"sos", "Database still down", "Joined SOS incident: Database down"},
		{"warn", "Certificate expires soon", "No SOS incident"},
		{"info", "Deployment finished", "No SOS incident"},
		{"resolved-sos", "Database back", "Resolved SOS incident: Database down"},
		{"resolved-sos", "Unmatched resolution", "No unresolved SOS incident matched the sender incident key. Nobody was notified."},
	} {
		body := fmt.Sprintf(`{"sender_alert_id":"alert-%d","sender_incident_key":"database","severity":%q,"title":%q,"message":"Full sender message","happened_at":"2026-10-30T17:40:00Z"}`, n, tc.severity, tc.title)
		id := alertReceipt(t, intakeHTTP(h.app, p, source, body, h.app.Now()))
		page := browser.get("/alerts/"+id, 200)
		assertBreadcrumbBeforeTitle(t, page, "/alerts", "Alert history")
		text := pageText(page)
		for _, want := range []string{tc.outcome, "Received 13:45", "happened at 13:40", "(5 minutes later)", "Full alert message", "Full sender message", "Alert IDs and sender details", source.Source.ID} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing %q: %s", tc.title, want, text)
			}
		}
		if strings.Contains(page, "/claim") || strings.Contains(page, "/resolve") {
			t.Fatal("alert pages must link to incidents rather than offer incident actions")
		}
		if n == 0 {
			e, err := h.app.GetAlert(context.Background(), p, id)
			if err != nil {
				t.Fatal(err)
			}
			incidentID = e.IncidentID
		}
		if n < 2 || n == 4 {
			main := regexp.MustCompile(`(?s)<main\b[^>]*>(.*?)</main>`).FindStringSubmatch(page)
			if main == nil || strings.Count(main[1], `href="/incidents/`+incidentID+`"`) != 1 {
				t.Fatal("the alert does not link to the incident it affected")
			}
		}
		if history := pageText(browser.get("/alerts?q="+url.QueryEscape(tc.title), 200)); !strings.Contains(history, tc.outcome) || !strings.Contains(history, tc.title) {
			t.Fatalf("history loses the alert title or outcome: %s", history)
		}
	}
}

func TestAlertHistoryFiltersUseLocalCalendarDays(t *testing.T) {
	r := newOnCallTeamRig(t)
	h := r.h
	p := Principal{SignInEmailAddress: "maya@example.test"}
	// New York's local day lasts 25 hours when the clocks change.
	for _, tc := range []struct{ at, title string }{
		{"2026-11-01T03:59:00Z", "Before local day"},
		{"2026-11-01T04:00:00Z", "Start of local day"},
		{"2026-11-02T04:59:00Z", "End of local day"},
		{"2026-11-02T05:00:00Z", "After local day"},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		h.clock.Store(at.Unix())
		if _, err := h.app.ManualSOS(context.Background(), p, tc.title, ""); err != nil {
			t.Fatal(err)
		}
	}
	text := pageText(r.maya.get("/alerts?from=2026-11-01&until=2026-11-01", 200))
	for _, title := range []string{"Start of local day", "End of local day"} {
		if !strings.Contains(text, title) {
			t.Fatalf("local date excludes %s", title)
		}
	}
	// The red bar may name a waiting incident outside the search results.
	for _, title := range []string{"Before local day", "After local day"} {
		if !strings.Contains(r.maya.get("/alerts?q="+url.QueryEscape(title)+"&from=2026-11-01&until=2026-11-01", 200), "No alerts match these filters.") {
			t.Fatalf("local date includes %s", title)
		}
	}
}

func TestAlertSourcePageStatesAndLocalDowntime(t *testing.T) {
	r := newOnCallTeamRig(t)
	h, browser := r.h, r.daniel // Ordinary members can start and end downtime.
	p := Principal{SignInEmailAddress: "maya@example.test"}
	source, err := createAlertSource(context.Background(), h.app, p, "Monitoring")
	if err != nil {
		t.Fatal(err)
	}
	path := "/alert-sources/" + source.Source.ID
	page := browser.get(path, 200)
	assertBreadcrumbBeforeTitle(t, page, "/alert-sources", "Alert sources")
	if !strings.Contains(pageText(page), "Waiting for the first check-in") || !strings.Contains(pageText(page), "No check-ins are expected until the first check-in arrives.") {
		t.Fatal("an unused source is presented as late")
	}
	checkListStatus := func(want string) {
		t.Helper()
		if text := pageText(browser.get("/alert-sources", 200)); !strings.Contains(text, want) {
			t.Fatalf("alert sources do not show %q: %s", want, text)
		}
	}
	checkListStatus("Waiting for the first check-in")
	body := `{"sent_at":"2026-10-30T17:45:00Z","sent_by_node_name":"util"}`
	if w := checkInHTTP(h.app, p, source, body, h.app.Now()); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if text := pageText(browser.get(path, 200)); !strings.Contains(text, "Check-ins arriving") || !strings.Contains(text, "13:45") {
		t.Fatalf("check-in status or local receipt time missing: %s", text)
	}
	checkListStatus("Check-ins arriving")
	h.clock.Add(90)
	if text := pageText(browser.get(path, 200)); !strings.Contains(text, "No recent check-in") || !strings.Contains(text, "Check the sending program") {
		t.Fatal("a silent source does not explain what to check")
	}
	checkListStatus("No recent check-in")
	alertReceipt(t, intakeHTTP(h.app, p, source, `{"severity":"info","title":"Sender is running","message":""}`, h.app.Now()))
	if text := pageText(browser.get(path, 200)); !strings.Contains(text, "Receiving alerts, but no recent check-in") {
		t.Fatal("a recent alert is confused with a check-in")
	}
	checkListStatus("Receiving alerts, but no recent check-in")
	page = browser.get(path, 200)
	page = browser.post(path+"/planned-downtime", url.Values{"csrf": {workflowField(t, page, "csrf")}, "until": {"2026-10-29T15:45"}, "reason": {"Keep my reason"}}, 400)
	if workflowField(t, page, "until") != "2026-10-29T15:45" || workflowField(t, page, "reason") != "Keep my reason" {
		t.Fatal("a validation error lost the entered time or reason")
	}
	browser.post(path+"/planned-downtime", url.Values{"csrf": {workflowField(t, page, "csrf")}, "until": {"2026-10-30T15:45"}, "reason": {"Move monitoring"}}, 303)
	status, err := h.app.AlertSourceCheckInStatus(context.Background(), p, source.Source.ID)
	if err != nil || status.PlannedDowntime == nil || !status.PlannedDowntime.Until.Equal(time.Date(2026, 10, 30, 19, 45, 0, 0, time.UTC)) {
		t.Fatalf("downtime did not interpret New York time: %+v %v", status.PlannedDowntime, err)
	}
	page = browser.get(path, 200)
	if text := pageText(page); !strings.Contains(text, "In planned downtime") || !strings.Contains(text, "Until 15:45") || !strings.Contains(text, "started by Daniel Reyes: Move monitoring") {
		t.Fatalf("planned downtime lacks its local end or actor: %s", text)
	}
	checkListStatus("In planned downtime")
	browser.post(path+"/planned-downtime", url.Values{"csrf": {workflowField(t, page, "csrf")}, "end": {"1"}}, 303)
	if text := pageText(browser.get(path, 200)); !strings.Contains(text, "Start planned downtime") || strings.Contains(text, "End planned downtime now") {
		t.Fatal("downtime did not end")
	}
	if text := pageText(browser.get("/alert-sources", 200)); strings.Contains(text, "Signing secret ID:") {
		t.Fatal("ordinary members see administrator-only signing secret metadata")
	}
	if err := disableAlertSource(context.Background(), h.app, p, source.Source.ID); err != nil {
		t.Fatal(err)
	}
	checkListStatus("Disabled")
}

func TestEmptyAlertsAndSourcesSayWhatToDo(t *testing.T) {
	h := newWorkflowRig(t)
	browser := h.browser()
	browser.addPerson("admin@example.test", "Barbara")
	if text := pageText(browser.get("/alerts", 200)); !strings.Contains(text, "No alerts match these filters.") {
		t.Fatal("empty history has no explanation")
	}
	text := pageText(browser.get("/alert-sources", 200))
	for _, want := range []string{"No alert sources are set up.", "An administrator adds the sending tool or script", "alert_sources", "Where alert sources are set"} {
		if !strings.Contains(text, want) {
			t.Fatalf("empty sources missing %q: %s", want, text)
		}
	}
}

// Breadcrumb links must precede the page's main heading, so the route back
// is visible before the person starts reading the page.
func assertBreadcrumbBeforeTitle(t *testing.T, page, target, label string) {
	t.Helper()
	main := regexp.MustCompile(`(?s)<main\b[^>]*>(.*?)<h1\b`).FindStringSubmatch(page)
	if main == nil || !strings.Contains(main[1], `href="`+target+`"`) || !strings.Contains(pageText(main[1]), label) {
		t.Fatalf("breadcrumb %q does not precede the page title", label)
	}
}
