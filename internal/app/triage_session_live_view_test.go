package app

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"mmhotline/internal/triageagent"
)

// A session of warn alerts only has no incident, so its own page and the
// dashboard are where people see the agent at work and stop it. The
// dashboard says what the agent works on, since when, and its latest step,
// and links to the session's page. That page shows the live view with Stop,
// the session's alerts, its runs, and its reports; its Stop takes the
// session over as on an incident page and returns to it, and the hand-over
// shows there. With no session open, the dashboard says so in one line and
// links to the last session.
func TestASessionOfWarnAlertsOnlyIsSeenAndStoppedOnItsOwnPage(t *testing.T) {
	r := newTriageSessionRig(t, true)
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	responder := (&workflowRig{t: t, server: server}).browser()
	responder.identify(r.p.SignInEmailAddress)
	dashboard := func() string { return html.UnescapeString(responder.get("/dashboard", 200)) }
	page := dashboard()
	pageInOrder(t, page, `id="incident-list"`, `id="triage-agent-at-work"`, `id="system-servers"`)
	if got := liveSectionText(page, "triage-agent-at-work"); got != "Triage agent Idle Not working on any alert now." {
		t.Fatalf("the triage agent's panel before any session: %q", got)
	}

	// The team has no schedule, so the dashboard's times are in UTC.
	startedClock := r.a.Now().UTC().Format("15:04")
	first := r.alert(t, "warn", "Disk of app002 is 97% full", "")
	second := r.alert(t, "warn", "Disk of app003 is 96% full", "")
	r.drive(t)
	run := r.nextRun(t)
	controls := json.NewDecoder(run.connection)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	sendTriageEvent(t, run, triageagent.Event{Event: "step", Step: &triageagent.Step{Kind: "command", Text: "ssh app002 df -h /var\n" + strings.Repeat("x", 400)}})
	waitForCondition(t, "the step", func() bool {
		return countRows(t, r.a, `SELECT count(*) FROM triage_agent_run_steps WHERE text LIKE 'ssh app002 df -h /var%'`) == 1
	})
	sessionID := openTriageSessionID(t, r.a)
	page = dashboard()
	pageInOrder(t, liveSectionText(page, "triage-agent-at-work"), "Triage agent Working", "Working on Disk of app002 is 97% full, and 1 more alert", "Since "+startedClock+".",
		"1 step so far. The latest, at "+r.a.Now().UTC().Format("15:04")+", a command:", "ssh app002 df -h /var", "…")
	if !strings.Contains(page, `<a href="/triage-sessions/`+sessionID+`">Disk of app002 is 97% full, and 1 more alert</a>`) {
		t.Fatalf("the dashboard does not open the triage session:\n%s", page)
	}
	if strings.Contains(page, "/stop") || strings.Contains(page, "Working: ") || strings.Contains(page, strings.Repeat("x", 300)) {
		t.Fatalf("the dashboard shows the latest step only, cut, and no Stop:\n%s", page)
	}

	sessionPage := func() string { return html.UnescapeString(responder.get("/triage-sessions/"+sessionID, 200)) }
	page = sessionPage()
	pageInOrder(t, pageText(page), "Triage session: Disk of app002 is 97% full, and 1 more alert",
		"Working: the triage agent is running", "Started "+startedClock, "Stop after the current step", "Latest run: Running", "Run steps", "ssh app002 df -h /var",
		"Reports from the triage agent", "No reports yet.", "Alerts in this session", "Disk of app002 is 97% full", "No SOS incident: a warning wakes nobody.", "Disk of app003 is 96% full",
		"Runs", "Run 1: Running", "1 step")
	for _, id := range []string{"triage-agent", "triage-session-alerts", "triage-session-runs", "triage-session-reports"} {
		if liveSectionText(page, id) == "" {
			t.Fatalf("the session page lost live section %s", id)
		}
	}
	for id, title := range map[string]string{first.ID: first.Title, second.ID: second.Title} {
		if pageLinkText(page, "/alerts/"+id) != title {
			t.Fatalf("the session does not link to alert %s", title)
		}
	}
	if strings.Contains(page, "return_to_incident_id") || strings.Contains(page, "Open the triage session") {
		t.Fatalf("the session's own page links back to itself:\n%s", page)
	}
	csrf := workflowField(t, page, "csrf")
	response, err := responder.client.PostForm(server.URL+"/triage-sessions/"+sessionID+"/stop", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if location := response.Header.Get("Location"); response.StatusCode != 303 || location != "/triage-sessions/"+sessionID+"#triage-agent" {
		t.Fatalf("Stop on the session's page: %d, to %q", response.StatusCode, location)
	}
	r.drive(t)
	readStop(t, run, controls)
	pageInOrder(t, pageText(sessionPage()), "Stopping: the triage agent is finishing the current step.", "Responder pressed Stop at "+startedClock)
	r.end(t, run, triageagent.Event{Event: "stopped", SessionID: "session-one"})
	r.drive(t)
	handOver := r.nextRun(t)
	if handOver.request.Action != "resume" || !strings.Contains(handOver.request.Prompt, "for a hand-over") {
		t.Fatalf("the hand-over: action %q:\n%s", handOver.request.Action, handOver.request.Prompt)
	}
	if answer := report(t, handOver, json.NewDecoder(handOver.connection), "0000000000000001", "info", "# Hand-over: the disks of app002 and app003 are full\nI found old logs in /var/log/app and was about to remove them.\n"); answer.Outcome != triageagent.ReportRecorded {
		t.Fatalf("the hand-over's report: %+v", answer)
	}
	pageInOrder(t, liveSectionText(dashboard(), "triage-agent-at-work"), "Triage agent Handing over", "Working on Disk of app002 is 97% full, and 1 more alert", "Handing over: the triage agent writes a report and a note about what the triage agent found")
	pageInOrder(t, pageText(sessionPage()), "Handing over: the triage agent writes a report and a note about what the triage agent found", "Hand-over run: Running",
		"Reports from the triage agent", "Hand-over run", "Info", "# Hand-over: the disks of app002 and app003 are full",
		"Runs", "Run 1: Stopped after the current step", "Hand-over run: Running")
	r.end(t, handOver, triageagent.Event{Event: "finished", Answer: "Handed over."})
	r.drive(t)
	page = sessionPage()
	pageInOrder(t, pageText(page), "Ended", "Responder pressed Stop at "+startedClock, "A person took over: Responder stopped the triage agent.", "Hand-over run: Finished")
	if strings.Contains(page, `/stop"`) {
		t.Fatalf("an ended session offers Stop:\n%s", page)
	}
	page = dashboard()
	if got := liveSectionText(page, "triage-agent-at-work"); got != "Triage agent Idle Not working on any alert now. The last triage session ended at "+r.a.Now().UTC().Format("15:04")+"." || !strings.Contains(page, `<a href="/triage-sessions/`+sessionID+`">The last triage session</a>`) {
		t.Fatalf("the triage agent's panel after the session ended: %q", got)
	}

	responder.get("/triage-sessions/0123456789abcdef0123456789abcdef", 404)
}

// The incident page of a session's SOS shows what the agent does, with Take
// over, which returns to the incident, and links to the session's own page; a team without the triage agent sees no
// triage agent part on the dashboard.
func TestIncidentPageLinksToItsTriageSession(t *testing.T) {
	r := newTriageSessionRig(t, true)
	sos := r.alert(t, "sos", "Database on db001 is down", "db001")
	r.drive(t)
	r.nextRun(t)
	sessionID := openTriageSessionID(t, r.a)
	get := func(path string) string {
		t.Helper()
		request := httptest.NewRequest("GET", "https://mmhotline.example.com"+path, nil)
		addAccessToken(request, r.p.SignInEmailAddress)
		response := httptest.NewRecorder()
		r.a.Handler().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
		return html.UnescapeString(response.Body.String())
	}
	page := get("/incidents/" + sos.IncidentID)
	if panel := liveSectionText(page, "triage-agent"); !strings.HasPrefix(panel, "The triage agent Working since ") || !strings.Contains(panel, "The triage agent is looking into the incident now.") || !strings.Contains(panel, "MMHotline then wakes the person on call at once. Watch the session Take over from the agent") {
		t.Fatalf("the incident's triage agent panel: %q", panel)
	}
	pageInOrder(t, page, `<a href="/triage-sessions/`+sessionID+`">Watch the session</a>`, `<form method="post" action="/triage-sessions/`+sessionID+`/stop">`,
		`name="return_to_incident_id" value="`+sos.IncidentID+`"`, "Take over from the agent")
	sessionPage := get("/triage-sessions/" + sessionID)
	sessionAlerts := sessionPage[strings.Index(sessionPage, `id="triage-session-alerts"`):]
	if pageLinkText(sessionAlerts, "/alerts/"+sos.ID) != sos.Title || pageLinkText(sessionAlerts, "/incidents/"+sos.IncidentID) != sos.Title {
		t.Fatal("the session must link to its alert and SOS incident")
	}
	if !strings.Contains(liveSectionText(sessionPage, "triage-session-alerts"), "Not claimed yet") {
		t.Fatal("the session must show the SOS incident's state")
	}

	a, p, _ := alertFixture(t)
	if page := dashboardPage(t, a, p.SignInEmailAddress); strings.Contains(page, "triage-agent-at-work") {
		t.Fatalf("a team without the triage agent sees its part:\n%s", page)
	}
}

// The session page uses the team's time zone in every section and gives each
// phase an honest action, including no run yet and a failed latest run.
func TestTriageSessionPageStatesAndTeamTimes(t *testing.T) {
	r := newOnCallTeamRig(t)
	now := r.h.app.Now()
	for _, test := range []struct {
		phase, text string
		stop, ended bool
	}{
		{triageSessionWorking, "Working: the triage agent is running.", true, false},
		{triageSessionWaiting, "Waiting: the triage agent is not running.", true, false},
		{triageSessionStopping, "Stopping: the triage agent is finishing the current step.", false, false},
		{triageSessionHandingOver, "Handing over: the triage agent writes a report and a note", false, false},
		{"ended", "Ended 13:45.", false, true},
	} {
		t.Run(test.phase, func(t *testing.T) {
			session := &TriageSessionLiveView{SessionID: "0123456789abcdef0123456789abcdef", Phase: test.phase, PhaseText: test.text, Started: now}
			if test.ended {
				session.Ended = now
				session.EndedBecause = "All SOS incidents are resolved and no alert arrived for 30 minutes."
			}
			data := struct {
				TriageSessionPage
				CSRF string
			}{TriageSessionPage: TriageSessionPage{TriageSession: session}, CSRF: "test-csrf"}
			request := httptest.NewRequest("GET", "/triage-sessions/"+session.SessionID, nil)
			response := httptest.NewRecorder()
			writer := &navigationPageWriter{ResponseWriter: response, app: r.h.app, request: request, principal: Principal{SignInEmailAddress: "maya@example.test"}, member: true}
			render := func() string {
				response = httptest.NewRecorder()
				writer.ResponseWriter = response
				RenderPage(writer, 200, "Triage session", triageSessionBody, data)
				if response.Code != 200 {
					t.Fatal(response.Body.String())
				}
				return response.Body.String()
			}
			page := render()
			mustContain(t, "session phase", pageText(page), test.text, "Started 13:45", "Times are New York time", "No run yet.", "No reports yet.", "No alerts in this session.")
			if strings.Contains(page, `/stop"`) != test.stop {
				t.Fatal("Stop availability disagrees with session phase")
			}
			if test.phase == triageSessionWaiting && !strings.Contains(pageText(page), "Stop the triage agent") {
				t.Fatal("waiting session's Stop promises a current step")
			}
			session.Run = &TriageSessionLiveRun{Title: "Latest run", StateLabel: "Failed", Started: now, Ended: now, FailureReason: "The coding agent could not connect.", EarlierSteps: 2, Steps: []TriageAgentRunStep{{Kind: "message", OccurredAt: now, Text: "<script>read the logs</script>"}, {Kind: "tool", OccurredAt: now, Text: "Read a file"}}}
			data.Alerts = []TriageSessionPageAlert{{ID: "alert", Title: "Disk full", Severity: "warn", ReceivedAt: now}}
			data.Runs = []TriageSessionPageRun{{Title: "Run 1", StateLabel: "Failed", StartedAt: now, EndedAt: now, Steps: 4, FailureReason: session.Run.FailureReason}}
			data.Reports = []TriageSessionReport{{RunTitle: "Run 1", Severity: "info", SentAt: now, Text: "The logs filled the disk."}}
			page = render()
			mustContain(t, "session details", pageText(page), "Latest run: Failed", "Started 13:45 · Ended 13:45", "The coding agent could not connect.", "2 earlier steps are not shown.", "13:45 · Agent message", "13:45 · Tool use", "The logs filled the disk.", "Received 13:45", "4 steps")
			if strings.Contains(page, "<script>read the logs</script>") || strings.Contains(page, "17:45") {
				t.Fatal("session leaked unescaped step text or UTC into the page")
			}
			if !regexp.MustCompile(`<details[^>]*><summary>Run steps</summary>`).MatchString(page) {
				t.Fatal("run steps must be folded")
			}
		})
	}
}
