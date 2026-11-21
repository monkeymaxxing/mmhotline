package app

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// hiddenFromAssistiveTechnology matches an element a screen reader skips,
// such as a person's initial in a round badge.
var hiddenFromAssistiveTechnology = regexp.MustCompile(`<span[^>]*aria-hidden="true"[^>]*>[^<]*</span>`)

// screenReaderText is what a screen reader says of the page's element with that
// id: its text, without tags and without what is hidden from assistive
// technology.
func screenReaderText(page, id string) string {
	return liveSectionText(hiddenFromAssistiveTechnology.ReplaceAllString(page, ""), id)
}

// pageText is the whole page's text, as screenReaderText gives a section's.
func pageText(page string) string {
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(hiddenFromAssistiveTechnology.ReplaceAllString(page, ""), " ")
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}

// onCallTeamRig is a team of three on a weekly rotation in New York that
// hands over on Mondays at 09:00, on Friday 30 October 2026 at 13:45
// there. Maya Okafor, the administrator, had the first week, from Monday
// 19 October; Daniel Reyes has this week; Priya Natarajan, the next person
// in the rotation and so the backup, has the next week.
type onCallTeamRig struct {
	h                         *workflowRig
	maya, daniel, priya       *workflowBrowser
	mayaID, danielID, priyaID string
}

func newOnCallTeamRig(t *testing.T) *onCallTeamRig {
	t.Helper()
	h := newWorkflowRig(t)
	h.clock.Store(time.Date(2026, 10, 30, 17, 45, 0, 0, time.UTC).Unix())
	h.app.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	r := &onCallTeamRig{h: h, maya: h.browser(), daniel: h.browser(), priya: h.browser()}
	r.mayaID = memberIDForUser(t, h.app, r.maya.addPerson("maya@example.test", "Maya Okafor"))
	r.danielID, _ = addTeamMember(t, h.app, "daniel@example.test", "Daniel Reyes", RoleMember)
	r.priyaID, _ = addTeamMember(t, h.app, "priya@example.test", "Priya Natarajan", RoleMember)
	r.daniel.identify("daniel@example.test")
	r.priya.identify("priya@example.test")
	rotation := ScheduleConfig{Kind: "weekly_rotation", TimeZone: "America/New_York", HandoffHour: 9, HandoffWeekday: 1, FirstStartDate: "2026-10-19", RotationMemberIDs: []string{r.mayaID, r.danielID, r.priyaID}, BackupKind: "next_in_rotation"}
	if _, err := h.app.SaveSchedule(context.Background(), h.app.settingsPrincipal(), rotation, 0); err != nil {
		t.Fatal(err)
	}
	return r
}

// post sends the page's form with the given fields, with the page's CSRF
// token.
func (b *workflowBrowser) postOnCall(fields url.Values, want int) {
	b.h.t.Helper()
	fields.Set("csrf", workflowField(b.h.t, b.get("/on-call", 200), "csrf"))
	b.post("/on-call", fields, want)
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("%s lacks %q: %s", what, want, text)
		}
	}
}

// The page says who is on call, until when, and who follows, and whom an
// SOS wakes, in order, as the escalation code wakes them: the person on
// call, after the time to claim the backup, then everyone else who is
// available. The person on call reads "You" and is not offered Take on
// call; another member takes it under today's rules, and from then on is
// the one on call.
func TestOnCallPageSaysWhoIsOnCallAndWhomAnSOSWakes(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	page := r.daniel.get("/on-call", 200)
	mustContain(t, "Daniel's on call now", screenReaderText(page, "on-call-now"),
		"On call now Daniel Reyes You Until 2 Nov 09:00, in 2 days. Then Priya Natarajan.",
		"Who an SOS wakes, in order You no claim in 5 minutes → Priya Natarajan no claim in 5 minutes → everyone else who is available: Maya Okafor MMHotline keeps waking everyone who is available until someone claims the incident.")
	mustContain(t, "Daniel's page", pageText(page), "Handover every Monday at 09:00 · 5 minutes to claim an SOS",
		"You are on call until 2 Nov 09:00. To hand over sooner, ask the next responder to sign in and choose Take on call on the On call page.")
	if strings.Contains(page, `value="claim_on_call"`) {
		t.Fatal("the person on call is offered Take on call")
	}
	page = r.priya.get("/on-call", 200)
	mustContain(t, "Priya's on call now", screenReaderText(page, "on-call-now"), "On call now Daniel Reyes Until 2 Nov 09:00, in 2 days. Then you.", "Daniel Reyes no claim in 5 minutes → You no claim in")
	mustContain(t, "Priya's page", pageText(page), "Taking on call makes MMHotline wake you first for a new SOS, until the time you choose.")

	r.priya.postOnCall(url.Values{"action": {"claim_on_call"}, "revision": {"0"}, "duration": {"2h"}, "take_from_member_id": {""}}, 303)
	page = r.daniel.get("/on-call", 200)
	mustContain(t, "on call now after Priya took it", screenReaderText(page, "on-call-now"), "On call now Priya Natarajan Until 15:45, in 2 hours. Then you. Priya Natarajan took on call. The schedule names you.",
		"Priya Natarajan no claim in 5 minutes → everyone else who is available: You Maya Okafor")
	if strings.Contains(r.priya.get("/on-call", 200), `value="claim_on_call"`) {
		t.Fatal("the person who took on call is offered Take on call")
	}
	// Daniel takes it back from Priya, naming her, as today's rules ask.
	mustContain(t, "Daniel's page", pageText(page), "Priya Natarajan took on call until 15:45.", "Take on call from Priya Natarajan")
	if got := workflowField(t, page, "take_from_member_id"); got != r.priyaID {
		t.Fatalf("take_from_member_id: %q, want Priya's ID", got)
	}
	r.daniel.postOnCall(url.Values{"action": {"claim_on_call"}, "revision": {"0"}, "duration": {"1h"}, "take_from_member_id": {r.priyaID}}, 409)
	r.daniel.postOnCall(url.Values{"action": {"claim_on_call"}, "revision": {"1"}, "duration": {"1h"}, "take_from_member_id": {r.priyaID}}, 303)
	// Daniel took on call while the schedule names him too, so its end
	// changes nothing: he stays on call until the handover.
	mustContain(t, "on call now after Daniel took it back", screenReaderText(r.daniel.get("/on-call", 200), "on-call-now"), "On call now Daniel Reyes You Until 2 Nov 09:00, in 2 days. Then Priya Natarajan. You took on call. The schedule names you.")
}

// The weeks list asks the schedule code who has each coming week, and
// moves on at the handover. With every week of the rotation filled, the
// page never says "No one scheduled", which the page before the redesign
// printed under every rotation.
func TestOnCallWeeksFollowTheRotationAcrossAHandover(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	page := r.maya.get("/on-call", 200)
	if strings.Contains(page, "No one scheduled") {
		t.Fatalf("a filled rotation shows No one scheduled: %s", pageText(page))
	}
	weeks := screenReaderText(page, "coming-on-call")
	if want := "Week by week This week · until 2 Nov Daniel Reyes 2 Nov – 9 Nov Priya Natarajan 9 Nov – 16 Nov You 16 Nov – 23 Nov Daniel Reyes"; !strings.HasPrefix(weeks, want) {
		t.Fatalf("weeks: %q, want %q", weeks, want)
	}
	// Monday 2 November, a minute after the 09:00 handover in New York.
	r.h.clock.Store(time.Date(2026, 11, 2, 14, 1, 0, 0, time.UTC).Unix())
	page = r.maya.get("/on-call", 200)
	if weeks, want := screenReaderText(page, "coming-on-call"), "Week by week This week · until 9 Nov Priya Natarajan 9 Nov – 16 Nov You 16 Nov – 23 Nov Daniel Reyes 23 Nov – 30 Nov Priya Natarajan"; !strings.HasPrefix(weeks, want) {
		t.Fatalf("weeks after the handover: %q, want %q", weeks, want)
	}
	mustContain(t, "on call now after the handover", screenReaderText(page, "on-call-now"), "On call now Priya Natarajan Until 9 Nov 09:00, in 6 days. Then you.")
}

// A day-of-week schedule lists its coming handovers, a run of days with the
// same person as one.
func TestOnCallHandoversJoinDaysWithTheSamePerson(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	weekdays := ScheduleConfig{Kind: "day_of_week", TimeZone: "America/New_York", HandoffHour: 9, Assignments: []string{r.priyaID, r.danielID, r.danielID, r.danielID, r.danielID, r.danielID, r.priyaID}, BackupKind: "everyone_available"}
	if _, err := r.h.app.SaveSchedule(context.Background(), r.h.app.settingsPrincipal(), weekdays, 1); err != nil {
		t.Fatal(err)
	}
	page := r.daniel.get("/on-call", 200)
	if got, want := screenReaderText(page, "coming-on-call"), "Coming handovers Now · until 31 Oct You 31 Oct – 2 Nov Priya Natarajan 2 Nov – 7 Nov You From 7 Nov Priya Natarajan"; !strings.HasPrefix(got, want) {
		t.Fatalf("handovers: %q, want %q", got, want)
	}
	mustContain(t, "the heading's line", pageText(page), "Handover every day at 09:00")
}

// "Your availability" says what MMHotline does while the person is
// unavailable: the escalation code skips them, and the person on call is
// followed by the backup first.
func TestOnCallPageShowsWhatHappensWhileYouAreUnavailable(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	mustContain(t, "Daniel's availability", screenReaderText(r.daniel.get("/on-call", 200), "your-availability"),
		"Your availability On call You are available. While you are unavailable, MMHotline skips you and wakes Priya Natarajan first.")
	mustContain(t, "Priya's availability", screenReaderText(r.priya.get("/on-call", 200), "your-availability"),
		"Your availability Backup You are available. While you are unavailable, MMHotline skips you when it wakes people for an SOS.")

	r.daniel.postOnCall(url.Values{"action": {"unavailable"}, "revision": {"0"}, "duration": {"2h"}}, 303)
	page := r.daniel.get("/on-call", 200)
	mustContain(t, "Daniel's availability", screenReaderText(page, "your-availability"), "Unavailable until 15:45 You are unavailable until 15:45. Until then, MMHotline skips you when it wakes people for an SOS.", "I'm available again")
	mustContain(t, "on call now", screenReaderText(page, "on-call-now"), "On call now Priya Natarajan Until 15:45, in 2 hours. Then you.", "You (skipped: unavailable until 15:45)")

	r.daniel.postOnCall(url.Values{"action": {"unavailable"}, "revision": {"1"}, "end": {"1"}}, 303)
	mustContain(t, "on call now", screenReaderText(r.daniel.get("/on-call", 200), "on-call-now"), "On call now Daniel Reyes You")
}

// When nobody is available, the escalation code wakes every member, and
// the page says so rather than skipping them.
func TestOnCallPageWhenNobodyIsAvailable(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	ctx := context.Background()
	until := r.h.app.Now().Add(time.Hour)
	for _, email := range []string{"maya@example.test", "daniel@example.test", "priya@example.test"} {
		if _, err := r.h.app.SetPersonalAvailability(ctx, Principal{SignInEmailAddress: email}, "unavailable", until, 0); err != nil {
			t.Fatal(err)
		}
	}
	page := r.daniel.get("/on-call", 200)
	mustContain(t, "on call now", screenReaderText(page, "on-call-now"), "On call now Everyone on the team", "Nobody is available, so everyone on the team: You", "Maya Okafor", "Priya Natarajan", "MMHotline keeps waking everyone on the team until someone claims the incident.")
	if strings.Contains(screenReaderText(page, "on-call-now"), "skipped") {
		t.Fatal("a person is shown as skipped although every member is woken")
	}
	mustContain(t, "Daniel's availability", screenReaderText(page, "your-availability"), "Nobody else is available either, so an SOS still wakes you")
}

// A member sees no control only an administrator may use: team mute. An
// administrator mutes the team, and everyone's page says that an SOS then
// wakes nobody. The forms keep their CSRF and revision checks, and the page
// saves no schedule: the settings file sets it.
func TestOnCallPageShowsMembersOnlyWhatTheyMayChange(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	page := r.daniel.get("/on-call", 200)
	if strings.Contains(page, `value="team_mute"`) {
		t.Fatal("a member is offered the team mute")
	}
	mustContain(t, "Daniel's page", pageText(page), "Mute the team Not muted", "Only an administrator can mute the team.")
	r.daniel.postOnCall(url.Values{"action": {"team_mute"}, "revision": {"0"}, "duration": {"2h"}}, 403)
	if !strings.Contains(r.maya.get("/on-call", 200), `value="team_mute"`) {
		t.Fatal("the administrator is not offered the team mute")
	}
	r.maya.postOnCall(url.Values{"action": {"team_mute"}, "revision": {"0"}, "duration": {"2h"}}, 303)
	page = r.daniel.get("/on-call", 200)
	mustContain(t, "on call now while muted", screenReaderText(page, "on-call-now"), "The team is muted until 15:45: MMHotline sends no Pushover notifications or automated voice calls, so an SOS wakes nobody until then.")
	mustContain(t, "Daniel's page", pageText(page), "Mute the team Muted until 15:45")
	r.maya.postOnCall(url.Values{"action": {"team_mute"}, "revision": {"0"}, "duration": {"1h"}}, 409)
	r.maya.post("/on-call", url.Values{"action": {"team_mute"}, "revision": {"1"}, "duration": {"1h"}}, 403)
	r.maya.postOnCall(url.Values{"action": {"save"}, "revision": {"1"}, "schedule_type": {"day_of_week"}, "time_zone": {"UTC"}}, 400)
	if state, err := r.h.app.Schedules(context.Background(), Principal{SignInEmailAddress: "maya@example.test"}); err != nil || state.ActiveKind != "weekly_rotation" {
		t.Fatalf("a schedule form changed the schedule: %+v %v", state, err)
	}
	// Where the schedule is set is folded, with the file and its key.
	mustContain(t, "Maya's page", pageText(r.maya.get("/on-call", 200)), "Where the schedule is set Set in /etc/mmhotline/mmhotline_settings.yml , in on_call_schedule .")
}

// Names are written as text, never as markup.
func TestOnCallPageEscapesNames(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	addTeamMember(t, r.h.app, "alex@example.test", "Alex <Admin>", RoleMember)
	page := r.daniel.get("/on-call", 200)
	if strings.Contains(page, "Alex <Admin>") || !strings.Contains(page, "Alex &lt;Admin&gt;") {
		t.Fatalf("name not safely written: %s", page)
	}
}

// The actions that end something show only while it lasts: I'm available
// again, Unmute the team, and Give on call back to the schedule.
func TestOnCallEndActionsShowOnlyWhileActive(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	ctx := context.Background()
	p := Principal{SignInEmailAddress: "maya@example.test"}
	check := func(active bool) {
		t.Helper()
		text := pageText(r.maya.get("/on-call", 200))
		for _, action := range []string{"I'm available again", "Unmute the team", "Give on call back to the schedule"} {
			if strings.Contains(text, action) != active {
				t.Fatalf("%q shown is %t, want %t: %s", action, !active, active, text)
			}
		}
	}
	check(false)
	until := r.h.app.Now().Add(time.Hour)
	if _, err := r.h.app.SetOnCallClaim(ctx, p, until, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.app.SetTeamMute(ctx, p, until, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.app.SetPersonalAvailability(ctx, p, "unavailable", until, 0); err != nil {
		t.Fatal(err)
	}
	check(true)
	mustContain(t, "on call now", screenReaderText(r.maya.get("/on-call", 200), "on-call-now"), "You took on call but are unavailable now, so the schedule is used.")
	r.h.clock.Store(until.Add(time.Minute).Unix())
	check(false)
}

// An action on a page that someone else changed meanwhile says so in plain
// words, in place of the bare "conflicting state".
func TestOnCallActionOnAChangedPageSaysWhatHappened(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	r.priya.postOnCall(url.Values{"action": {"claim_on_call"}, "revision": {"0"}, "duration": {"2h"}, "take_from_member_id": {""}}, 303)
	fields := url.Values{"action": {"end_on_call_claim"}, "revision": {"0"}}
	fields.Set("csrf", workflowField(t, r.priya.get("/on-call", 200), "csrf"))
	page := r.priya.post("/on-call", fields, 409)
	mustContain(t, "the page after a stale action", pageText(page), "The On call page changed while you had it open. Check the page, then choose again.")
}
