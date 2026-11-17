package app

import (
	"context"
	"html"
	"io"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// liveSectionText is the text of the page's live section with that id,
// without its tags, or "" when the page has none.
func liveSectionText(page, id string) string {
	at := strings.Index(page, ` id="`+id+`"`)
	if at < 0 {
		return ""
	}
	section := page[strings.LastIndex(page[:at], "<"):]
	tag := strings.FieldsFunc(section[1:], func(r rune) bool { return r == ' ' || r == '>' })[0]
	depth, end := 0, 0
	for {
		opens, closes := strings.Index(section[end:], "<"+tag), strings.Index(section[end:], "</"+tag+">")
		if closes < 0 {
			break
		}
		if opens >= 0 && opens < closes {
			depth, end = depth+1, end+opens+1
			continue
		}
		depth, end = depth-1, end+closes+len("</"+tag+">")
		if depth == 0 {
			break
		}
	}
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(section[:end], " ")
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}

// menuLinkText is the text of the page's first link to href, without tags,
// such as "Approvals 1" when the menu counts one approval request.
func menuLinkText(page, href string) string {
	match := regexp.MustCompile(`<a href="` + regexp.QuoteMeta(href) + `"[^>]*>(.*?)</a>`).FindStringSubmatch(page)
	if match == nil {
		return ""
	}
	return strings.Join(strings.Fields(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(match[1], " ")), " ")
}

// While an SOS waits for a claim, every page shows the red bar with the
// incident, that nobody claimed it, and whom MMHotline wakes next, at a time
// in the team's time zone; the dashboard and the incident's own page show
// it themselves. Once the SOS is claimed or resolved, the bar is gone.
func TestTheRedBarShowsWhileAnSOSWaitsForAClaim(t *testing.T) {
	h := newWorkflowRig(t)
	ctx := context.Background()
	admin := h.browser()
	p := admin.addPerson("admin@example.test", "Daniel Reyes")
	_, backup := addMember(t, h.app, "Priya Natarajan", RoleMember)
	giveWakingContactMethod(t, h.app, "admin@example.test")
	giveWakingContactMethod(t, h.app, backup.SignInEmailAddress)
	daniel, err := h.app.CurrentMember(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	week := []string{daniel.ID, daniel.ID, daniel.ID, daniel.ID, daniel.ID, daniel.ID, daniel.ID}
	if _, err = h.app.SaveSchedule(ctx, p, ScheduleConfig{Kind: "day_of_week", TimeZone: "America/New_York", Assignments: week, BackupKind: "fixed_person", BackupMemberID: backup.ID}, 0); err != nil {
		t.Fatal(err)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if got := liveSectionText(admin.get("/alerts", 200), "sos-waiting"); got != "" {
		t.Fatalf("a red bar with no SOS: %q", got)
	}

	first, err := h.app.ManualSOS(ctx, p, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	deadline := h.app.Now().Add(5 * time.Minute).In(newYork).Format("15:04")
	want := "SOS Database down — not claimed. Claim the incident by " + deadline + ", or MMHotline also wakes Priya Natarajan. Open"
	for _, path := range []string{"/alerts", "/on-call", "/people", "/contact-methods"} {
		page := admin.get(path, 200)
		if got := liveSectionText(page, "sos-waiting"); got != want {
			t.Fatalf("red bar on %s: %q, want %q", path, got, want)
		}
		if !strings.Contains(page, `href="/incidents/`+first.IncidentID+`"`) {
			t.Fatalf("the red bar on %s does not open the incident", path)
		}
	}
	for _, path := range []string{"/dashboard", "/incidents/" + first.IncidentID} {
		if got := liveSectionText(admin.get(path, 200), "sos-waiting"); got != "" {
			t.Fatalf("red bar on %s: %q", path, got)
		}
	}
	if page := h.browser().get("/alerts", 401); strings.Contains(page, "Database down") {
		t.Fatal("the red bar shows to a person who is not signed in")
	}

	// With two waiting, the bar shows the older and how many more wait; on
	// the older one's page, the other.
	second, err := h.app.ManualSOS(ctx, p, "Disk full", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := liveSectionText(admin.get("/alerts", 200), "sos-waiting"); !strings.HasPrefix(got, "SOS Database down and 1 more — not claimed.") {
		t.Fatalf("red bar with two SOS: %q", got)
	}
	if got := liveSectionText(admin.get("/incidents/"+first.IncidentID, 200), "sos-waiting"); !strings.HasPrefix(got, "SOS Disk full — not claimed.") {
		t.Fatalf("red bar on the older SOS's page: %q", got)
	}

	for _, i := range []Alert{first, second} {
		incident, err := h.app.GetIncident(ctx, p, i.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		if i == first {
			_, err = h.app.ClaimIncident(ctx, p, i.IncidentID, incident.Revision, "")
		} else {
			_, err = h.app.ResolveIncident(ctx, p, i.IncidentID, incident.Revision, "")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := liveSectionText(admin.get("/alerts", 200), "sos-waiting"); got != "" {
		t.Fatalf("a red bar after the claim and the resolve: %q", got)
	}
}

// The menu shows every member the pages for members and the person's own
// name, and shows Sending services and Triage agent only to administrators.
func TestTheMenuFollowsTheRole(t *testing.T) {
	h := newWorkflowRig(t)
	admin := h.browser()
	admin.addPerson("admin@example.test", "Daniel Reyes")
	member, _ := addMember(t, h.app, "Priya Natarajan", RoleMember)
	other := h.browser()
	other.identify(member.SignInEmailAddress)
	everyone := []string{`href="/dashboard"`, `href="/alerts"`, `href="/approvals"`, `href="/on-call"`, `href="/status-page"`, `href="/alert-sources"`, `href="/people"`, `href="/agent-playbook-rules"`, `href="/contact-methods"`, `href="/on-call#your-availability"`}
	administrators := []string{`href="/sending-services"`, `href="/triage-agent"`}
	for _, test := range []struct {
		browser *workflowBrowser
		name    string
		admin   bool
	}{{admin, "Daniel Reyes", true}, {other, "Priya Natarajan", false}} {
		page := test.browser.get("/people", 200)
		for _, link := range everyone {
			if !strings.Contains(page, link) {
				t.Errorf("%s's menu lacks %s", test.name, link)
			}
		}
		for _, link := range administrators {
			if strings.Contains(page, link) != test.admin {
				t.Errorf("%s's menu has %s: %v", test.name, link, !test.admin)
			}
		}
		if !strings.Contains(page, "Signed in as "+test.name) {
			t.Errorf("the menu does not say %s is signed in", test.name)
		}
		if !strings.Contains(page, `href="/people" aria-current="page"`) {
			t.Errorf("the menu does not mark the page shown")
		}
	}
}

// Pages load the one stylesheet, and the Content Security Policy allows no
// other style.
func TestPagesLoadTheStylesheetUnderAStrictStylePolicy(t *testing.T) {
	h := newWorkflowRig(t)
	admin := h.browser()
	admin.addPerson("admin@example.test", "Daniel Reyes")
	response, err := admin.client.Get(h.server.URL + "/assets/mmhotline.css")
	if err != nil {
		t.Fatal(err)
	}
	css, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/css; charset=utf-8" || !strings.Contains(string(css), ":root") {
		t.Fatalf("stylesheet: %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	// A rule block left open, as a merge can leave one, makes every later
	// rule apply only inside it, and nothing else fails.
	if open, closed := strings.Count(string(css), "{"), strings.Count(string(css), "}"); open != closed {
		t.Fatalf("the stylesheet has %d { and %d }", open, closed)
	}
	response, err = admin.client.Get(h.server.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if policy := response.Header.Get("Content-Security-Policy"); !strings.Contains(policy, "style-src 'self';") || strings.Contains(policy, "unsafe-inline") {
		t.Fatalf("policy: %s", policy)
	}
	if !strings.Contains(string(page), `href="/assets/mmhotline.css"`) {
		t.Fatal("the dashboard does not load the stylesheet")
	}
}

// The Content Security Policy allows only the stylesheet, so a browser
// silently drops a style attribute or a style element. No page has one: the
// pages of a team set up with the example settings file, an SOS incident
// and its alert, a warning, and the pages a person who is not signed in, or
// not a member, sees.
func TestNoPageHasAnInlineStyle(t *testing.T) {
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	configureAccessSignIn(t, f.a)
	ana := Principal{SignInEmailAddress: "ana@example.com"}
	sos, err := f.a.ManualSOS(ctx, ana, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	warning := admitSignedAlert(t, f.a, CreatedSource{Source: Source{ID: "monitoring"}, Secret: exampleMonitoringSecret}, "warn", "Disk of app002 is 97% full", "")
	pages := []string{"/dashboard", "/alerts", "/alerts/" + sos.ID, "/alerts/" + warning.ID, "/incidents/" + sos.IncidentID, "/approvals", "/on-call", "/status-page",
		"/alert-sources", "/alert-sources/monitoring", "/people", "/contact-methods", "/agent-playbook-rules", "/triage-agent",
		"/sending-services", "/sending-services/pushover", "/sending-services/twilio", "/sending-services/telegram", "/sending-services/slack", "/sending-services/mail_server"}
	server := httptest.NewTLSServer(f.a.Handler())
	defer server.Close()
	inline := regexp.MustCompile(`<style|\sstyle\s*=`)
	for _, visitor := range []struct{ signInEmailAddress, pages string }{{"ana@example.com", "every page"}, {"stranger@example.com", "the page for a person who is not a member"}, {"", "the page for a person who is not signed in"}} {
		clientValue := *server.Client()
		client := &clientValue
		client.Jar, _ = cookiejar.New(nil)
		if visitor.signInEmailAddress != "" {
			useSignInEmailAddress(client, visitor.signInEmailAddress)
		}
		for _, path := range pages {
			response, err := client.Get(server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			page, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if want := map[string]int{"ana@example.com": 200, "stranger@example.com": 403, "": 401}[visitor.signInEmailAddress]; response.StatusCode != want {
				t.Fatalf("%s for %q: %d, want %d", path, visitor.signInEmailAddress, response.StatusCode, want)
			}
			if inline.Match(page) {
				t.Errorf("%s has an inline style (%s): %s", path, visitor.pages, inline.Find(page))
			}
			if response.Header.Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("%s is %s", path, response.Header.Get("Content-Type"))
			}
			if visitor.signInEmailAddress != "ana@example.com" {
				break
			}
		}
	}
}

// A page may print its own heading in place of the frame's h1; every other
// page keeps the frame's h1 with its title.
func TestAPageMayPrintItsOwnHeading(t *testing.T) {
	for _, test := range []struct {
		heading      templ.Component
		want, absent string
	}{
		{nil, "<h1>Plain page</h1>", ""},
		{templ.Raw(`<div class="head"><h1>Own heading of db01</h1></div>`), "<h1>Own heading of db01</h1>", "<h1>Plain page</h1>"},
	} {
		w := httptest.NewRecorder()
		renderPage(w, 200, "Plain page", pageFrame{TimeZone: time.UTC}, test.heading, templ.Raw(`<p>Body of db01</p>`))
		page := w.Body.String()
		if w.Code != 200 || !strings.Contains(page, test.want) || strings.Count(page, "<h1") != 1 || test.absent != "" && strings.Contains(page, test.absent) || !strings.Contains(page, "<p>Body") {
			t.Errorf("heading %q: %d %s", test.want, w.Code, page)
		}
	}
}
