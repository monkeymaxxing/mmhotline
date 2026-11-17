package app

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Every unresolved SOS the team admitted stays on the dashboard, however
// many arrive in one outage, and each one nobody claimed offers Claim.
func TestDashboardRetainsAllAdmittedUnresolvedIncidents(t *testing.T) {
	h := newWorkflowRig(t)
	browser := h.browser()
	p := browser.addPerson("burst@example.test", "Barbara")
	for i := 0; i < 201; i++ {
		h.clock.Add(1)
		if _, err := h.app.ManualSOS(context.Background(), p, fmt.Sprintf("Accepted incident %03d", i), "One outage can produce a burst of alerts"); err != nil {
			t.Fatal(err)
		}
	}
	incidents, err := h.app.ListIncidents(context.Background(), p)
	if err != nil || len(incidents) != 201 {
		t.Fatalf("%d incidents, %v", len(incidents), err)
	}
	page := browser.get("/dashboard", 200)
	for _, incident := range incidents {
		if !strings.Contains(page, `action="/incidents/`+incident.ID+`/claim"`) {
			t.Fatalf("%s is not offered for a claim", incident.Title)
		}
	}
	if needs := liveSectionText(page, "needs-a-person"); !strings.HasPrefix(needs, "Needs a person 201 waiting SOS") {
		t.Fatalf("Needs a person: %.200s", needs)
	}
}

// dashboardTeam is a team of two in New York for dashboard tests: Daniel
// Reyes, an administrator, is on call every day, with Priya Natarajan as the
// backup, unless week says otherwise. It returns Daniel's browser, Daniel,
// and Priya's principal and member.
func dashboardTeam(t *testing.T, h *workflowRig, week func(daniel, priya string) []string) (*workflowBrowser, Member, Principal, Member) {
	t.Helper()
	ctx := context.Background()
	browser := h.browser()
	p := browser.addPerson("daniel@example.test", "Daniel Reyes")
	priyaPrincipal, priya := addMember(t, h.app, "Priya Natarajan", RoleMember)
	giveWakingContactMethod(t, h.app, "daniel@example.test")
	giveWakingContactMethod(t, h.app, priya.SignInEmailAddress)
	daniel, err := h.app.CurrentMember(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	assignments := []string{daniel.ID, daniel.ID, daniel.ID, daniel.ID, daniel.ID, daniel.ID, daniel.ID}
	if week != nil {
		assignments = week(daniel.ID, priya.ID)
	}
	if _, err = h.app.SaveSchedule(ctx, p, ScheduleConfig{Kind: "day_of_week", TimeZone: "America/New_York", Assignments: assignments, BackupKind: "fixed_person", BackupMemberID: priya.ID}, 0); err != nil {
		t.Fatal(err)
	}
	return browser, daniel, priyaPrincipal, priya
}

// What needs a person comes first: each SOS nobody claimed, oldest first,
// with when it opened, whom MMHotline wakes now and, by when, whom next, and
// Claim, which works from the dashboard. A claimed SOS moves to Being
// handled, with who claimed it and when, and offers no Claim. With nothing
// left, the page says All clear and names the last SOS and who resolved it.
func TestDashboardSaysWhatNeedsAPerson(t *testing.T) {
	h := newWorkflowRig(t)
	ctx := context.Background()
	daniel, _, priyaPrincipal, _ := dashboardTeam(t, h, nil)
	p := Principal{SignInEmailAddress: "daniel@example.test"}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	clock := func(at time.Time) string { return at.In(newYork).Format("15:04") }
	page := daniel.get("/dashboard", 200)
	if got := liveSectionText(page, "all-clear"); got != "All clear Nothing needs a person." {
		t.Fatalf("a new team's dashboard: %q", got)
	}

	first, err := h.app.ManualSOS(ctx, p, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	opened := h.app.Now()
	h.clock.Add(60)
	if _, err = h.app.ManualSOS(ctx, p, "Disk full", ""); err != nil {
		t.Fatal(err)
	}
	page = daniel.get("/dashboard", 200)
	pageInOrder(t, liveSectionText(page, "needs-a-person"), "Needs a person", "2 waiting",
		"SOS", "Opened "+clock(opened)+", 1 minute ago. MMHotline is waking you.", "Database down",
		"Claim the incident by "+clock(opened.Add(5*time.Minute))+", or MMHotline also wakes Priya Natarajan.", "in 4 minutes", "Claim the incident", "Open",
		"SOS", "Opened "+clock(h.app.Now())+", just now. MMHotline is waking you.", "Disk full", "Claim the incident",
		"Claiming an incident tells the team you are handling the incident and stops MMHotline waking anyone else.")
	if strings.Contains(page, `id="all-clear"`) || strings.Contains(page, `id="being-handled"`) {
		t.Fatalf("All clear or Being handled while two SOS wait:\n%s", page)
	}

	// The oldest SOS comes first, so the page's first claim form is its.
	daniel.post("/incidents/"+first.IncidentID+"/claim", url.Values{"csrf": {workflowField(t, page, "csrf")}, "revision": {workflowField(t, page, "revision")}}, 303)
	claimedAt := h.app.Now()
	page = daniel.get("/dashboard", 200)
	if needs := liveSectionText(page, "needs-a-person"); strings.Contains(needs, "Database down") || !strings.Contains(needs, "1 waiting") || !strings.Contains(needs, "Disk full") {
		t.Fatalf("Needs a person after the claim: %q", needs)
	}
	if got, want := liveSectionText(page, "being-handled"), "Being handled 1 claimed Claimed You claimed the incident at "+clock(claimedAt)+". Opened "+clock(opened)+". Database down"; got != want {
		t.Fatalf("Being handled: %q, want %q", got, want)
	}
	if strings.Contains(page, `action="/incidents/`+first.IncidentID+`/claim"`) {
		t.Fatal("a claimed SOS offers Claim")
	}
	priya := h.browser()
	priya.identify(priyaPrincipal.SignInEmailAddress)
	if handled := liveSectionText(priya.get("/dashboard", 200), "being-handled"); !strings.Contains(handled, "Daniel Reyes claimed the incident at") {
		t.Fatalf("Priya's Being handled: %q", handled)
	}

	incidents, err := h.app.ListIncidents(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: Disk full is resolved last.
	for index := len(incidents) - 1; index >= 0; index-- {
		h.clock.Add(60)
		if _, err = h.app.ResolveIncident(ctx, p, incidents[index].ID, incidents[index].Revision, ""); err != nil {
			t.Fatal(err)
		}
	}
	page = daniel.get("/dashboard", 200)
	if got, want := liveSectionText(page, "all-clear"), "All clear Nothing needs a person. The last SOS was resolved at "+clock(h.app.Now())+" by you: Disk full"; got != want {
		t.Fatalf("All clear: %q, want %q", got, want)
	}
	if strings.Contains(page, `id="needs-a-person"`) || strings.Contains(page, `id="being-handled"`) {
		t.Fatalf("Needs a person or Being handled after both were resolved:\n%s", page)
	}
	if got := liveSectionText(priya.get("/dashboard", 200), "all-clear"); !strings.HasSuffix(got, "by Daniel Reyes: Disk full") {
		t.Fatalf("Priya's All clear: %q", got)
	}
}

// The On call panel names who an SOS wakes first, "You" for the viewer,
// until when and who then, whom MMHotline also wakes when nobody claims in
// the time to claim, and the team mute while it lasts.
func TestDashboardOnCallPanel(t *testing.T) {
	h := newWorkflowRig(t)
	ctx := context.Background()
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	today := h.app.Now().In(newYork)
	// Daniel today, Priya every other day: the turn ends at the next
	// midnight in New York, the schedule's handoff.
	daniel, _, priyaPrincipal, _ := dashboardTeam(t, h, func(daniel, priya string) []string {
		week := []string{priya, priya, priya, priya, priya, priya, priya}
		week[today.Weekday()] = daniel
		return week
	})
	handoff := time.Date(today.Year(), today.Month(), today.Day()+1, 0, 0, 0, 0, newYork).Format("2 Jan 15:04")
	if got, want := liveSectionText(daniel.get("/dashboard", 200), "coverage-summary"), "On call Schedule D You Until "+handoff+", then Priya Natarajan If nobody claims an SOS within 5 minutes, MMHotline also wakes Priya Natarajan."; got != want {
		t.Fatalf("Daniel's On call: %q, want %q", got, want)
	}
	priya := h.browser()
	priya.identify(priyaPrincipal.SignInEmailAddress)
	if got, want := liveSectionText(priya.get("/dashboard", 200), "coverage-summary"), "On call Schedule D Daniel Reyes Until "+handoff+", then you If nobody claims an SOS within 5 minutes, MMHotline also wakes you."; got != want {
		t.Fatalf("Priya's On call: %q, want %q", got, want)
	}

	until := h.app.Now().Add(2 * time.Hour)
	if _, err = h.app.SetTeamMute(ctx, Principal{SignInEmailAddress: "daniel@example.test"}, until, 0); err != nil {
		t.Fatal(err)
	}
	if got := liveSectionText(daniel.get("/dashboard", 200), "coverage-summary"); !strings.HasSuffix(got, "The whole team is muted until "+until.In(newYork).Format("15:04")+": no Pushover notification or automated voice call goes out, even for an SOS.") {
		t.Fatalf("On call while muted: %q", got)
	}
}

// The dashboard's On call panel and the on-call page name the same "until"
// and "then" for the same team: here a schedule by hour that keeps Daniel on
// call for the next 17 hours, then Priya.
func TestDashboardAndOnCallPageAgreeOnUntilAndThen(t *testing.T) {
	h := newWorkflowRig(t)
	daniel, danielMember, _, priya := dashboardTeam(t, h, nil)
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := h.app.Now()
	hour := now.In(newYork).Hour()
	assignments := make([]string, 24)
	for i := range assignments {
		assignments[i] = priya.ID
		if (i-hour+24)%24 < 17 {
			assignments[i] = danielMember.ID
		}
	}
	if _, err = h.app.SaveSchedule(context.Background(), Principal{SignInEmailAddress: "daniel@example.test"}, ScheduleConfig{Kind: "hour_of_day", TimeZone: "America/New_York", Assignments: assignments, BackupKind: "fixed_person", BackupMemberID: priya.ID}, 1); err != nil {
		t.Fatal(err)
	}
	// Daniel's turn ends when the local hour 17 hours on from this one starts.
	end := now.Truncate(time.Hour).Add(time.Hour)
	for end.In(newYork).Hour() != (hour+17)%24 {
		end = end.Add(time.Hour)
	}
	until := "Until " + end.In(newYork).Format("2 Jan 15:04")
	if got := liveSectionText(daniel.get("/dashboard", 200), "coverage-summary"); !strings.Contains(got, "You "+until+", then Priya Natarajan") {
		t.Errorf("the dashboard's On call: %q, want %q and Priya Natarajan", got, until)
	}
	mustContain(t, "the on-call page's on call now", screenReaderText(daniel.get("/on-call", 200), "on-call-now"), "You "+until+", ", ". Then Priya Natarajan.")
}

// An approval request and a warned run wait for a person too. The dashboard
// lists an approval request with who asked, what it needs, and Review, which
// opens its page; a warned run with when it runs by itself, and Stop, which
// works from the dashboard. The session's SOS, held for the triage agent,
// says that nobody is woken yet.
func TestDashboardListsApprovalsAndWarnedRunsThatWait(t *testing.T) {
	t.Run("an approval request", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareApprovals(t, r.a, "")
		r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		approvalSession(t, r)
		record := onlyApprovalRecord(t, r.a)
		page := dashboardPage(t, r.a, r.p.SignInEmailAddress)
		pageInOrder(t, liveSectionText(page, "needs-a-person"), "Needs a person", "2 waiting", "SOS", "app002 stopped answering", "Claim the incident",
			"Approval", "The triage agent asked at 12:00. The playbook run needs approval from 1 person.", "Run “Reboot app002” on app002?", "The playbook can change app002, in the inventory “production”.", "Review")
		if !strings.Contains(page, `href="/approvals/`+record.id+`"`) {
			t.Fatalf("no link to the approval request's page:\n%s", page)
		}
	})
	t.Run("a warned run", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		prepareWarnedRun(t, r.a, "", 10)
		r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		approvalSession(t, r)
		record := onlyWarnedRunRecord(t, r.a)
		r.advance(3 * time.Minute)
		server := httptest.NewTLSServer(r.a.Handler())
		t.Cleanup(server.Close)
		responder := (&workflowRig{t: t, server: server}).browser()
		responder.identify(r.p.SignInEmailAddress)
		page := responder.get("/dashboard", 200)
		pageInOrder(t, liveSectionText(page, "needs-a-person"), "Needs a person", "2 waiting",
			"SOS", "Opened 12:00, 3 minutes ago. The triage agent looks into the incident first, so nobody is woken yet.", "app002 stopped answering", "Claim the incident by 12:03, or MMHotline wakes you.",
			"Runs by itself", "Runs at 12:10, in 7 minutes, unless someone presses Stop.", "Run “Reboot app002” on app002?", "The playbook can change app002, in the inventory “production”.", "Stop", "Open")
		responder.post("/approvals/"+record.id+"/stop", url.Values{"csrf": {workflowField(t, page, "csrf")}}, 303)
		if state := onlyWarnedRunRecord(t, r.a).state; state != "stopped" {
			t.Fatalf("the warned run is %s after Stop on the dashboard", state)
		}
		if needs := liveSectionText(responder.get("/dashboard", 200), "needs-a-person"); strings.Contains(needs, "Runs by itself") {
			t.Fatalf("a stopped run still waits: %q", needs)
		}
	})
}

// A part of the settings file that did not apply shows at the top of an
// administrator's dashboard, with the reason; a member, who cannot change
// the file, does not see it.
func TestDashboardShowsSettingsFailuresToAdministrators(t *testing.T) {
	ctx := context.Background()
	withBruno := replaced(t, exampleSettingsYAML, "    role: administrator\n", "    role: administrator\n  - name: Bruno Costa\n    sign_in_email_address: bruno@example.com\n    role: member\n")
	f := newSettingsFileFixture(t, withBruno)
	f.start()
	configureAccessSignIn(t, f.a)
	bruno := Principal{SignInEmailAddress: "bruno@example.com"}
	alert, err := f.a.ManualSOS(ctx, bruno, "Disk full on db01", "")
	if err != nil {
		t.Fatal(err)
	}
	incident, err := f.a.GetIncident(ctx, bruno, alert.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.ClaimIncident(ctx, bruno, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	// Removing Bruno fails while he owns a claimed incident.
	f.write(exampleSettingsYAML)
	f.apply()
	page := dashboardPage(t, f.a, "ana@example.com")
	pageInOrder(t, page, `id="settings-failures"`, "Parts of the settings file did not apply", "people", "Bruno Costa owns 1 claimed incident", "Where to change the settings", f.path, `id="incident-list"`)
	if page := dashboardPage(t, f.a, bruno.SignInEmailAddress); strings.Contains(page, "settings-failures") || strings.Contains(page, "Bruno Costa owns") {
		t.Fatalf("a member sees the settings file's failures:\n%s", page)
	}
}

func TestIncidentPageListsOnlyItsDeliveriesAndClearPages(t *testing.T) {
	a := foundationApp(t)
	person := memberFixture(t, a, "page@example.test")
	p := person.Principal
	savedEmail(t, a, p, true)
	first, err := a.ManualSOS(context.Background(), p, "First incident", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.ManualSOS(context.Background(), p, "Other incident", "")
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := a.IncidentDeliveries(context.Background(), p, first.IncidentID)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("incident deliveries: %+v %v", deliveries, err)
	}
	others, err := a.IncidentDeliveries(context.Background(), p, second.IncidentID)
	if err != nil || len(others) != 1 {
		t.Fatalf("other deliveries: %+v %v", others, err)
	}
	for _, path := range []string{"/incidents/" + first.IncidentID, "/people", "/contact-methods", "/on-call", "/alert-sources", "/alerts", "/alerts/" + first.ID, "/deliveries/" + deliveries[0].ID, "/dashboard"} {
		request := httptest.NewRequest("GET", "https://mmhotline.example.com"+path, nil)
		addAccessToken(request, person.Principal.SignInEmailAddress)
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("private pages must not be cached")
		}
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		body := response.Body.String()
		switch path {
		case "/incidents/" + first.IncidentID:
			// The page lists its own notifications with their outcomes,
			// says who raised it and whom MMHotline wakes, and offers
			// Claim and a person to escalate to.
			if notifications := liveSectionText(body, "incident-deliveries"); !strings.Contains(notifications, "Email to you, the alert “First incident”") || !strings.Contains(notifications, "Waiting to send") || !strings.Contains(body, `href="/deliveries/`+deliveries[0].ID+`"`) {
				t.Fatalf("the incident's notifications: %q", notifications)
			}
			// The person has only email, which wakes nobody.
			if happened := liveSectionText(body, "incident-audit"); !strings.Contains(happened, "You raised this SOS by hand.") || !strings.Contains(happened, "MMHotline could not wake you: you have no contact method that wakes people for SOS alerts.") || strings.Contains(happened, "MMHotline woke you") {
				t.Fatalf("What happened: %q", happened)
			}
			if !strings.Contains(body, "Claim: I'm handling the incident") || !strings.Contains(body, "Choose a person") {
				t.Fatal("an SOS that waits offers no claim or escalation")
			}
			if strings.Contains(body, "/deliveries/"+others[0].ID) {
				t.Fatal("the incident page lists another incident's notification")
			}
			checkNoInternalTexts(t, body)
		case "/people":
			if !strings.Contains(body, "page@example.test") || !strings.Contains(body, "(you)") {
				t.Fatal("People must show the sign-in email address and identify the viewer")
			}
		case "/contact-methods":
			for _, text := range []string{"For a shared contact method, only team mute affects delivery.", "Contact methods", "Email", "Yours", "Alerts to send through the contact method", "Warning", "Send even when I'm unavailable or the team is muted", "Email to you", "Waiting to send", "Recent notifications", "Which alerts should I choose?"} {
				if !strings.Contains(pageText(body), text) {
					t.Fatalf("missing %s in %s", text, body)
				}
			}
		case "/on-call":
			if !strings.Contains(body, "You are available.") || !strings.Contains(body, "No schedule is set yet, so an SOS wakes everyone who is available.") {
				t.Fatal("on-call labels missing")
			}
		case "/alert-sources":
			// No source yet; exercise a configured origin below.
		}
	}
	// A claimed incident wakes nobody, so its page has no countdown.
	claimed, err := a.GetIncident(context.Background(), p, second.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimIncident(context.Background(), p, claimed.ID, claimed.Revision, ""); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "https://mmhotline.example.com/incidents/"+claimed.ID, nil)
	addAccessToken(request, person.Principal.SignInEmailAddress)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if got := liveSectionText(response.Body.String(), "incident-state"); !strings.Contains(got, "MMHotline wakes nobody else about the incident.") || strings.Contains(got, "Claim the incident by") {
		t.Fatalf("claimed incident page: %q", got)
	}
	source, err := createAlertSource(context.Background(), a, p, "CI")
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"", "https://public.mmhotline.example.com"} {
		a.WebURL = origin
		request := httptest.NewRequest("GET", "https://request.mmhotline.example.com/alert-sources", nil)
		addAccessToken(request, person.Principal.SignInEmailAddress)
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		expectedOrigin := "http://" + a.AlertIntakeListenAddress
		if !strings.Contains(pageText(response.Body.String()), "Intake URL POST ") || !strings.Contains(response.Body.String(), "Generic signed JSON") || !strings.Contains(response.Body.String(), expectedOrigin+"/alert-intake/"+source.Source.ID) {
			t.Fatal("full intake URL missing")
		}
		for _, text := range []string{"Alert intake address " + expectedOrigin, "Alert source ID " + source.Source.ID, "POST " + expectedOrigin + "/check-in/" + source.Source.ID} {
			if !strings.Contains(pageText(response.Body.String()), text) {
				t.Fatalf("source connection value missing: %s", text)
			}
		}
		// An administrator sees each signing secret's ID, never the secret,
		// and no form: the settings file sets the secrets.
		if body := response.Body.String(); !strings.Contains(pageText(body), "Signing secret ID: "+source.SigningSecretID) || strings.Contains(body, source.Secret) || strings.Contains(body, "/revoke") {
			t.Fatal("signing secret line missing, or a secret or revoke form shown")
		}
	}
}

func TestIncidentPageListsEscalationNotices(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%t", automatic), func(t *testing.T) {
			a := foundationApp(t)
			person := memberFixture(t, a, "escalation@example.test")
			p := person.Principal
			savedEmail(t, a, p, true)
			ctx := context.Background()
			member, err := a.CurrentMember(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			backup := Member{ID: NewID(), SignInEmailAddress: NewID(), Name: "Backup", Role: RoleMember}
			if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(`INSERT INTO members(id,sign_in_email_address,name,role) VALUES(?,?,?,?)`, backup.ID, backup.SignInEmailAddress, backup.Name, backup.Role)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			giveWakingContactMethod(t, a, "escalation@example.test")
			giveWakingContactMethod(t, a, backup.SignInEmailAddress)
			schedule := dayOfWeekFor(member.ID)
			schedule.BackupKind, schedule.BackupMemberID = "fixed_person", backup.ID
			if _, err := a.SaveSchedule(ctx, p, schedule, 0); err != nil {
				t.Fatal(err)
			}
			first, err := a.ManualSOS(ctx, p, "First incident", "")
			if err != nil {
				t.Fatal(err)
			}
			incident, err := a.GetIncident(ctx, p, first.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			if automatic {
				now := incident.Deadline.Add(time.Second)
				a.Now = func() time.Time { return now }
				if err := a.ReconcileIncidents(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := a.EscalateIncident(ctx, p, incident.ID, backup.ID, incident.Revision); err != nil {
					t.Fatal(err)
				}
			}
			second, err := a.ManualSOS(ctx, p, "Other incident", "")
			if err != nil {
				t.Fatal(err)
			}
			alerts, err := a.ListAlerts(ctx, p, AlertFilter{IncidentID: first.IncidentID})
			if err != nil || len(alerts) != 2 {
				t.Fatalf("related alerts: %+v %v", alerts, err)
			}
			var notice Alert
			for _, alert := range alerts {
				if alert.SourceID == "system" {
					notice = alert
				}
			}
			if notice.ID == "" || notice.Severity != "warn" || notice.IncidentID != incident.ID {
				t.Fatalf("escalation notice: %+v", notice)
			}
			// The waking contact method's deliveries aside, the incident has
			// the alert's email and the notice's email.
			notWaking := func(deliveries []Delivery) (n int) {
				for _, delivery := range deliveries {
					if delivery.ContactMethodType == "email" {
						n++
					}
				}
				return n
			}
			deliveries, err := a.IncidentDeliveries(ctx, p, incident.ID)
			if err != nil || notWaking(deliveries) != 2 {
				t.Fatalf("incident deliveries: %+v %v", deliveries, err)
			}
			found := false
			for _, delivery := range deliveries {
				if delivery.AlertID == notice.ID && delivery.Purpose == "system_notice" && delivery.IncidentID == incident.ID {
					found = true
				}
			}
			if !found {
				t.Fatal("escalation notice delivery missing")
			}
			others, err := a.IncidentDeliveries(ctx, p, second.IncidentID)
			if err != nil || notWaking(others) != 1 {
				t.Fatalf("other deliveries: %+v %v", others, err)
			}
			request := httptest.NewRequest("GET", "https://mmhotline.example.com/incidents/"+incident.ID, nil)
			addAccessToken(request, person.Principal.SignInEmailAddress)
			response := httptest.NewRecorder()
			a.Handler().ServeHTTP(response, request)
			body := response.Body.String()
			// MMHotline's alert to the team about the escalation is a link
			// on the escalation's line, not a second line.
			if response.Code != 200 || !strings.Contains(html.UnescapeString(body), `href="/alerts/`+notice.ID+`">MMHotline's alert to the team</a>`) || strings.Contains(body, "/deliveries/"+others[0].ID) {
				t.Fatalf("incident page: %d %s", response.Code, body)
			}
			for _, delivery := range deliveries {
				if !strings.Contains(body, "/deliveries/"+delivery.ID) {
					t.Fatalf("missing delivery %s", delivery.ID)
				}
			}
		})
	}
}

func TestIncidentActionLabelsAndSeverityBadges(t *testing.T) {
	for kind, label := range map[string]string{"opened": "Opened", "sos_escalation_due": "Contacting", "claimed": "Claimed", "reclaimed": "Taken over", "escalated": "Escalated", "resolved": "Resolved", "related_alert": "Related alert received"} {
		if got := incidentActionLabel(kind); got != label {
			t.Fatalf("%s: %s", kind, got)
		}
	}
	for severity, label := range map[string]string{"info": "Info", "warn": "Warning", "sos": "SOS", "resolved-info": "Resolved info", "resolved-warn": "Resolved warning", "resolved-sos": "Resolved SOS"} {
		if got := severityLabel(severity); got != label {
			t.Fatalf("severity %s: %s", severity, got)
		}
	}
}
