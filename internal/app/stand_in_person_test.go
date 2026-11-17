package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// The testing guide's "A stand-in person" reads a page's hidden form fields
// with `grep -o PATTERN | head -n 1 | cut -d'"' -f4`: the first match on the
// page, and its quoted value. These are its patterns, character for
// character, so a form the guide's commands could no longer read fails here.
var (
	standInCSRFField     = regexp.MustCompile(`name="csrf" value="([^"]*)"`)
	standInRevisionField = regexp.MustCompile(`name="revision" value="([0-9]*)"`)
	// The guide reads the availability form's revision from the rest of the
	// line after this heading, as `grep -o '<h2>Your availability</h2>.*'`.
	standInAvailabilitySection = regexp.MustCompile(`<h2>Your availability</h2>.*`)
)

// standInScheduleFile is the guide's schedule file: the stand-in is the
// primary every day, and everyone available is the backup.
const standInScheduleFile = `{
  "schedule_type": "day_of_week",
  "time_zone": "UTC",
  "handoff_hour": 0,
  "handoff_minute": 0,
  "assignment_sign_in_email_addresses": [
    "stand-in-1@example.invalid", "stand-in-1@example.invalid", "stand-in-1@example.invalid",
    "stand-in-1@example.invalid", "stand-in-1@example.invalid", "stand-in-1@example.invalid",
    "stand-in-1@example.invalid"
  ],
  "rotation_sign_in_email_addresses": [],
  "backup_option": "everyone_available",
  "backup_sign_in_email_address": ""
}`

// standInBrowser is curl with a cookie jar, as the guide runs it: it keeps
// the cookies a response sets and does not follow redirects.
type standInBrowser struct {
	t      *testing.T
	web    string
	client *http.Client
}

// signInStandIn opens a sign-in link from `people create-sign-in-link` for
// the address at the test server, as the guide's first curl does.
func signInStandIn(t *testing.T, a *App, web, signInEmailAddress string) *standInBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &standInBrowser{t: t, web: web, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	result, _ := commandRequest(t, a, CommandRequest{Group: "people", Action: "create-sign-in-link", People: PeopleCommandRequest{SignInEmailAddress: signInEmailAddress}}, 200)
	link, err := url.Parse(result.SignInLinkURL)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := b.request("GET", link.Path, nil); status != 303 {
		t.Fatalf("the sign-in link for %s: HTTP %d", signInEmailAddress, status)
	}
	return b
}

func (b *standInBrowser) request(method, path string, form url.Values) (int, string) {
	b.t.Helper()
	var response *http.Response
	var err error
	if method == "POST" {
		response, err = b.client.PostForm(b.web+path, form)
	} else {
		response, err = b.client.Get(b.web + path)
	}
	if err != nil {
		b.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

// page reads a page, as `curl -b "$jar" -c "$jar" -o page.html`.
func (b *standInBrowser) page(path string) string {
	b.t.Helper()
	status, body := b.request("GET", path, nil)
	if status != 200 {
		b.t.Fatalf("GET %s: HTTP %d: %s", path, status, body)
	}
	return body
}

// post sends a form and checks the guide's answer, 303 for a saved action.
func (b *standInBrowser) post(path string, form url.Values) {
	b.t.Helper()
	if status, body := b.request("POST", path, form); status != 303 {
		b.t.Fatalf("POST %s: HTTP %d: %s", path, status, body)
	}
}

// standInField is the value the guide's grep, head, and cut read.
func standInField(t *testing.T, field *regexp.Regexp, text string) string {
	t.Helper()
	match := field.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("the guide's pattern %s finds nothing in:\n%s", field, text)
	}
	return match[1]
}

// A stand-in person lets an agent do a person's part in a proof on a real
// installation, as the testing guide's "A stand-in person" says: a member
// with no contact method is the only person on call, the real person is
// unavailable, the read commands say who is contacted, and the agent signs in
// with a sign-in link and posts the pages' own forms. This test follows the
// guide's steps on the real handler, through a server with a cookie jar, so a
// change to a form field or a route the guide uses fails here.
func TestStandInPersonActsThroughThePages(t *testing.T) {
	ctx := context.Background()
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	server := httptest.NewServer(r.a.Handler())
	t.Cleanup(server.Close)
	command := func(request CommandRequest) CommandResult {
		t.Helper()
		result, _ := commandRequest(t, r.a, request, 200)
		return result
	}
	showIncident := func(id string) CommandIncident {
		t.Helper()
		return *command(CommandRequest{Group: "incidents", Action: "show", Incidents: IncidentsCommandRequest{IncidentID: id}}).Incident
	}
	// The real person is the rig's administrator, whose email and waking
	// contact methods take SOS even while unavailable.
	realPerson, err := r.a.CurrentMember(ctx, r.p)
	if err != nil {
		t.Fatal(err)
	}
	realContactMethods := countRows(t, r.a, `SELECT count(*) FROM contact_methods WHERE member_id=? AND send_while_unavailable_or_muted=1`, realPerson.ID)
	if realContactMethods != 2 {
		t.Fatalf("the real person has %d contact methods that send while unavailable", realContactMethods)
	}
	reachedRealPerson := func(incidentID string) int {
		t.Helper()
		return countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND member_id=? AND purpose='sos_escalation'`, incidentID, realPerson.ID)
	}

	// The guide adds the stand-in to the settings file's people, with no
	// contact method, and makes it the schedule's primary every day; the
	// people and on_call_schedule parts apply them with these operations.
	settings := Principal{SettingsFile: "/etc/mmhotline/mmhotline_settings.yml"}
	name := "Stand-in for Responder"
	addPerson(t, r.a, "stand-in-1@example.invalid", name, RoleMember)
	decoder := json.NewDecoder(strings.NewReader(standInScheduleFile))
	decoder.DisallowUnknownFields()
	var schedule ScheduleFile
	if err := decoder.Decode(&schedule); err != nil {
		t.Fatal(err)
	}
	members, err := r.a.TeamMembers(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	config, err := settingsSchedule(schedule).scheduleConfigForMembers(members)
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.a.Schedules(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.SaveSchedule(ctx, settings, config, state.Revision); err != nil {
		t.Fatal(err)
	}

	// The real person becomes unavailable for 30 days through the "Set
	// yourself unavailable" form, which the guide posts with a sign-in link
	// for them. A team mute form comes first on the page, with its own
	// revision; the guide reads the one after the availability heading.
	realBrowser := signInStandIn(t, r.a, server.URL, realPerson.SignInEmailAddress)
	onCall := realBrowser.page("/on-call")
	realBrowser.post("/on-call", url.Values{
		"csrf":     {standInField(t, standInCSRFField, onCall)},
		"revision": {standInField(t, standInRevisionField, standInAvailabilitySection.FindString(onCall))},
		"action":   {"unavailable"},
		"duration": {"30d"},
	})
	zone, err := time.LoadLocation(config.TimeZone)
	if err != nil {
		t.Fatal(err)
	}
	if until := pageClock(r.a.Now().Add(30*24*time.Hour), r.a.Now(), zone); !strings.Contains(realBrowser.page("/on-call"), "You are unavailable until "+until) {
		t.Fatal("the real person is not unavailable for 30 days")
	}

	// An SOS waits for the triage agent, which asks for two playbook runs
	// that need a person's approval. Asking wakes the person on call: the
	// stand-in, whom the guide reads in incidents show.
	sos := r.alert(t, "sos", "app002 stopped answering", "app002")
	if held := showIncident(sos.IncidentID); held.EscalationStep != "triage_agent" || !held.HeldForTriageAgent || held.BeingContactedNow != "Nobody yet: the triage agent is looking into the incident" {
		t.Fatalf("while the triage agent works: %+v", held)
	}
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	answers := json.NewDecoder(run.connection)
	for _, requestID := range []string{"0000000000000001", "0000000000000002"} {
		if answer := requestPlaybookRun(t, run, answers, requestID); answer.Outcome != triageagent.WaitsForAPerson {
			t.Fatalf("request %s: %+v", requestID, answer)
		}
	}
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	// The stand-in has no contact method that wakes people, so MMHotline
	// records that it could not wake the stand-in and moves at once to the
	// everyone-available step, which is the stand-in alone: the unavailable
	// real person is not contacted at either step.
	everyone := showIncident(sos.IncidentID)
	if everyone.EscalationStep != "everyone_available" || everyone.BeingContactedNow != name {
		t.Fatalf("the everyone-available step: %+v", everyone)
	}
	if !inOrder(incidentHistory(t, r.a, r.p, sos.IncidentID), "Could not wake "+name+": "+name+" has no contact method that wakes people for SOS alerts") {
		t.Fatalf("history: %q", incidentHistory(t, r.a, r.p, sos.IncidentID))
	}
	for _, entry := range everyone.History {
		if strings.Contains(entry.Message, "Nobody is available") {
			t.Fatalf("MMHotline fell back to every member: %+v", entry)
		}
	}
	if reached := reachedRealPerson(sos.IncidentID); reached != 0 {
		t.Fatalf("the SOS's escalation made %d deliveries to the unavailable real person", reached)
	}

	// The stand-in signs in with a sign-in link and claims the incident with
	// the csrf and revision the guide reads from its page, and the guide's
	// note.
	standIn := signInStandIn(t, r.a, server.URL, "stand-in-1@example.invalid")
	incidentPage := standIn.page("/incidents/" + sos.IncidentID)
	const claimNote, resolveNote = "A stand-in person claimed this SOS for a test.", "This was a test with a stand-in person."
	standIn.post("/incidents/"+sos.IncidentID+"/claim", url.Values{
		"csrf":     {standInField(t, standInCSRFField, incidentPage)},
		"revision": {standInField(t, standInRevisionField, incidentPage)},
		"note":     {claimNote},
	})
	if claimed := showIncident(sos.IncidentID); claimed.Status != "claimed" || claimed.ClaimedBy != name {
		t.Fatalf("after the claim: %+v", claimed)
	}

	// The two requests wait, as approval-requests list says; the stand-in
	// approves one and refuses the other from each request's page.
	waiting := command(CommandRequest{Group: "approval-requests", Action: "list"}).ApprovalRequests
	if len(waiting) != 2 || waiting[0].State != "open" || waiting[1].State != "open" || waiting[0].IncidentID != sos.IncidentID {
		t.Fatalf("waiting: %+v", waiting)
	}
	approved, refused := waiting[0].ApprovalRequestID, waiting[1].ApprovalRequestID
	csrf := standInField(t, standInCSRFField, standIn.page("/approvals/"+approved))
	standIn.post("/approvals/"+approved+"/approve", url.Values{"csrf": {csrf}})
	const reason = "Refused by the stand-in to test a refusal."
	standIn.post("/approvals/"+refused+"/refuse", url.Values{"csrf": {csrf}, "reason": {reason}})
	for id, want := range map[string]CommandApprovalDecision{approved: {MemberName: name, Decision: "approve"}, refused: {MemberName: name, Decision: "refuse", Reason: reason}} {
		shown := command(CommandRequest{Group: "approval-requests", Action: "show", ApprovalRequests: ApprovalRequestsCommandRequest{ApprovalRequestID: id}}).ApprovalRequest
		if wantState := map[string]string{"approve": "approved", "refuse": "refused"}[want.Decision]; shown.State != wantState || len(shown.Decisions) != 1 || shown.Decisions[0].MemberName != want.MemberName || shown.Decisions[0].Decision != want.Decision || shown.Decisions[0].Reason != want.Reason {
			t.Errorf("approval request %s: state %s, decisions %+v", id, shown.State, shown.Decisions)
		}
	}

	// The claim changed the incident's revision; the guide reads the page
	// again and resolves it.
	incidentPage = standIn.page("/incidents/" + sos.IncidentID)
	standIn.post("/incidents/"+sos.IncidentID+"/resolve", url.Values{
		"csrf":     {standInField(t, standInCSRFField, incidentPage)},
		"revision": {standInField(t, standInRevisionField, incidentPage)},
		"note":     {resolveNote},
	})
	resolved := showIncident(sos.IncidentID)
	if resolved.Status != "resolved" || resolved.ClaimedBy != name {
		t.Fatalf("after the resolve: %+v", resolved)
	}
	notes := map[string]string{}
	for _, entry := range resolved.History {
		if entry.Note != "" {
			notes[entry.Kind] = entry.Note
		}
	}
	if len(notes) != 2 || notes["claimed"] != claimNote || notes["resolved"] != resolveNote {
		t.Fatalf("the history's notes: %q", notes)
	}
	if reached := reachedRealPerson(sos.IncidentID); reached != 0 {
		t.Fatalf("%d deliveries reached the unavailable real person", reached)
	}
}
