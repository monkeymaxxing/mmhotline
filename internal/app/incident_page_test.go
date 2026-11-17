package app

import (
	"context"
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// incidentPageAs is the incident page as member p sees it.
func incidentPageAs(t *testing.T, a *App, p Principal, incidentID string) string {
	t.Helper()
	request := httptest.NewRequest("GET", "https://mmhotline.example.com/incidents/"+incidentID, nil)
	addAccessToken(request, p.SignInEmailAddress)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("incident page: HTTP %d: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

// renameMember gives member p a name.
func renameMember(t *testing.T, a *App, p Principal, name string) {
	t.Helper()
	if err := changePerson(t, a, p.SignInEmailAddress, name, ""); err != nil {
		t.Fatal(err)
	}
}

// buttonsOf lists the labels of the page's buttons, without tags.
func buttonsOf(page string) []string {
	var labels []string
	for _, match := range regexp.MustCompile(`(?s)<button[^>]*>(.*?)</button>`).FindAllStringSubmatch(page, -1) {
		labels = append(labels, strings.Join(strings.Fields(html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(match[1], " "))), " "))
	}
	return labels
}

// internalIncidentTexts are values MMHotline stores for itself, which a page
// must say in plain words instead.
var internalIncidentTexts = []string{"sos_escalation", "follows the active target", "step changed", "eligibility changed", "Contacting ", "only team member", "reported at sos", "Suppressed"}

func checkNoInternalTexts(t *testing.T, page string) {
	t.Helper()
	for _, text := range internalIncidentTexts {
		if strings.Contains(page, text) {
			t.Errorf("the incident page shows the internal text %q", text)
		}
	}
}

// An SOS that waits for a claim says, to each reader, whom MMHotline wakes
// and whom it wakes next, and offers Claim with one note field, which Claim
// and Resolve both send. Rounds that wake the same person again show as one
// line with a count, the next step shows at the end of "What happened", and
// every reason is in plain words.
func TestAnSOSThatWaitsForAClaimSaysWhomItWakesAndOffersClaim(t *testing.T) {
	r := newPushoverRig(t)
	ctx := context.Background()
	renameMember(t, r.a, r.principal, "Daniel Reyes")
	priya, _ := addMember(t, r.a, "Priya Natarajan", RoleMember)
	giveWakingContactMethod(t, r.a, priya.SignInEmailAddress)
	maya, _ := addMember(t, r.a, "Maya Okafor", RoleMember)
	mayaBack := r.a.Now().Add(3 * time.Hour)
	if _, err := r.a.SetPersonalAvailability(ctx, maya, "unavailable", mayaBack, 0); err != nil {
		t.Fatal(err)
	}
	r.add(t, fakesendingservices.PushoverUserKey)
	incidentID := r.sos(t, "Database down")
	r.dispatch(t)
	woke, deadline := r.a.Now().UTC().Format("15:04"), r.a.Now().Add(5*time.Minute).UTC().Format("15:04")

	daniel := incidentPageAs(t, r.a, r.principal, incidentID)
	if got := liveSectionText(daniel, "incident-state"); !strings.Contains(got, "You are on call. MMHotline woke you at "+woke+".") || !strings.Contains(got, "Claim the incident by "+deadline+", or MMHotline also wakes everyone else who is available: Priya Natarajan.") {
		t.Fatalf("the decide box for the person on call: %q", got)
	}
	if got := liveSectionText(incidentPageAs(t, r.a, priya, incidentID), "incident-state"); !strings.HasPrefix(got, "MMHotline is waking Daniel Reyes.") || !strings.Contains(got, "or MMHotline also wakes everyone else who is available: you.") {
		t.Fatalf("the decide box for the next person: %q", got)
	}
	buttons := strings.Join(buttonsOf(daniel), ",")
	if !strings.Contains(buttons, "Claim: I'm handling the incident,Resolve") || strings.Contains(buttons, "Take over") {
		t.Fatalf("an SOS that waits for a claim offers %s", buttons)
	}
	// One note field; the Claim button sends its form, whose action
	// claims, and the Resolve button sends the same form to resolve.
	if count := strings.Count(daniel, `name="note"`); count != 1 {
		t.Fatalf("%d note fields; want 1", count)
	}
	form := regexp.MustCompile(`name="note" form="([^"]+)"`).FindStringSubmatch(daniel)
	if form == nil || !strings.Contains(daniel, `<form id="`+form[1]+`" method="post" action="/incidents/`+incidentID+`/claim">`) || strings.Count(daniel, `<button form="`+form[1]+`"`) != 2 || !strings.Contains(daniel, `formaction="/incidents/`+incidentID+`/resolve"`) {
		t.Fatal("Claim and Resolve do not both send the note's form")
	}
	if got := liveSectionText(daniel, "incident-people"); !strings.Contains(got, "Can't be woken Maya Okafor, unavailable until "+mayaBack.UTC().Format("15:04")) {
		t.Fatalf("the People panel does not say who can't be woken and why: %q", got)
	}
	if !regexp.MustCompile(`<option value="[^"]+" disabled>Maya Okafor, unavailable until `).MatchString(daniel) {
		t.Fatal("an unavailable person can be chosen to escalate to")
	}

	// Three more rounds wake Daniel again, a minute apart.
	for round := range 3 {
		r.advance(time.Minute)
		if err := r.a.ReconcileIncidents(ctx); err != nil {
			t.Fatal(err)
		}
		r.dispatch(t)
		if round == 0 {
			text := liveSectionText(incidentPageAs(t, r.a, r.principal, incidentID), "incident-audit")
			if !strings.Contains(text, "Once more, at "+r.a.Now().UTC().Format("15:04")+".") || strings.Contains(text, "Again 1 time") {
				t.Fatalf("one repeat is not natural English: %s", text)
			}
		}
	}
	happened := liveSectionText(incidentPageAs(t, r.a, r.principal, incidentID), "incident-audit")
	if count := strings.Count(happened, "MMHotline woke you"); count != 1 || !strings.Contains(happened, "Again 3 times") || !strings.Contains(happened, "Pushover notification: ") {
		t.Fatalf("the rounds of waking in What happened: %q", happened)
	}
	if !strings.HasPrefix(happened, "What happened Oldest first "+woke+" You raised this SOS by hand.") || !strings.HasSuffix(happened, deadline+" Claim the incident by "+deadline+", or MMHotline also wakes everyone else who is available: Priya Natarajan.") {
		t.Fatalf("What happened does not start with the SOS and end with the next step: %q", happened)
	}

	// At the deadline MMHotline wakes everyone available.
	r.advance(2 * time.Minute)
	if err := r.a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	page := incidentPageAs(t, r.a, r.principal, incidentID)
	if happened := liveSectionText(page, "incident-audit"); !strings.Contains(happened, "Nobody had claimed the incident, so MMHotline woke everyone who is available: you and Priya Natarajan.") {
		t.Fatalf("the escalation in What happened: %q", happened)
	}
	if notifications := liveSectionText(page, "incident-deliveries"); !strings.Contains(notifications, "Pushover notification to you, the alert “Database down” · Not sent") || !strings.Contains(notifications, ": for an SOS, this contact method only wakes the people MMHotline is waking") {
		t.Fatalf("the notifications do not say in plain words why one was not sent: %q", notifications)
	}
	checkNoInternalTexts(t, page)
}

// Once claimed, the incident wakes nobody: its owner is offered Resolve and
// no claim, anyone else Take over; the note shows with its writer's name.
// Once resolved, nobody is offered Claim, Take over, or Resolve, and the
// page says who resolved it.
func TestAClaimedIncidentOffersTakeOverToOthersAndAResolvedOneNothing(t *testing.T) {
	r := newPushoverRig(t)
	ctx := context.Background()
	renameMember(t, r.a, r.principal, "Daniel Reyes")
	priya, _ := addMember(t, r.a, "Priya Natarajan", RoleMember)
	incidentID := r.sos(t, "Disk full")
	incident, err := r.a.GetIncident(ctx, r.principal, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	r.advance(2 * time.Minute)
	claimedAt := r.a.Now().UTC().Format("15:04")
	if incident, err = r.a.ClaimIncident(ctx, r.principal, incidentID, incident.Revision, "Removing old releases."); err != nil {
		t.Fatal(err)
	}

	daniel, other := incidentPageAs(t, r.a, r.principal, incidentID), incidentPageAs(t, r.a, priya, incidentID)
	if got := liveSectionText(daniel, "incident-state"); !strings.HasPrefix(got, "You claimed the incident at "+claimedAt+". MMHotline wakes nobody else about the incident.") || strings.Contains(got, "Claim the incident by") {
		t.Fatalf("the decide box for the owner: %q", got)
	}
	if buttons := strings.Join(buttonsOf(daniel), ","); strings.Contains(buttons, "Claim the incident") || strings.Contains(buttons, "Take over") || !strings.Contains(buttons, "Resolve") {
		t.Fatalf("the owner is offered %s", buttons)
	}
	if got := liveSectionText(other, "incident-state"); !strings.HasPrefix(got, "Daniel Reyes claimed the incident at "+claimedAt+".") {
		t.Fatalf("the decide box for another member: %q", got)
	}
	if buttons := strings.Join(buttonsOf(other), ","); !strings.Contains(buttons, "Take over: I'm handling the incident,Resolve") || strings.Contains(buttons, "Claim the incident") {
		t.Fatalf("another member is offered %s", buttons)
	}
	if happened := liveSectionText(other, "incident-audit"); !strings.Contains(happened, "Daniel Reyes claimed the incident. Note from Daniel Reyes: “Removing old releases.”") {
		t.Fatalf("the claim in What happened: %q", happened)
	}

	r.advance(time.Minute)
	resolvedAt := r.a.Now().UTC().Format("15:04")
	if _, err = r.a.ResolveIncident(ctx, r.principal, incidentID, incident.Revision, "Kept the last 5 releases."); err != nil {
		t.Fatal(err)
	}
	for reader, want := range map[Principal]string{r.principal: "You resolved the incident at " + resolvedAt + ". Your note: “Kept the last 5 releases.”", priya: "Daniel Reyes resolved the incident at " + resolvedAt + ". Note from Daniel Reyes: “Kept the last 5 releases.”"} {
		page := incidentPageAs(t, r.a, reader, incidentID)
		if got := liveSectionText(page, "incident-state"); got != want {
			t.Fatalf("the resolved incident's box: %q, want %q", got, want)
		}
		if buttons := strings.Join(buttonsOf(page), ","); strings.Contains(buttons, "Claim") || strings.Contains(buttons, "Take over") || strings.Contains(buttons, "Resolve") || strings.Contains(page, `name="note"`) {
			t.Fatalf("a resolved incident offers %s", buttons)
		}
		if got := liveSectionText(page, "incident-tags"); got != "SOS Resolved" {
			t.Fatalf("the resolved incident's tags: %q", got)
		}
		checkNoInternalTexts(t, page)
	}
}

// A round of waking whose notifications all went unsent, as when someone
// claimed the incident first, woke nobody, so the page says MMHotline was
// about to wake the person; once one notification goes out, it woke them.
func TestARoundThatSentNothingSaysMMHotlineWasAboutToWake(t *testing.T) {
	r := incidentReader{id: "reader", names: map[string]string{"daniel": "Daniel Reyes"}, now: time.Date(2026, 11, 3, 3, 20, 0, 0, time.UTC), zone: time.UTC}
	at := r.now.Add(-5 * time.Minute)
	round := IncidentAction{Kind: "sos_escalation_due", Revision: 2, At: at, TargetMemberIDs: []string{"daniel"}, Message: "Contacting Daniel Reyes (on call)"}
	call := Delivery{Purpose: "sos_escalation", IncidentRevision: 2, CreatedAt: at, MemberID: "daniel", ContactMethodType: "automated_voice_call", State: "cancelled", Reason: "incident claimed"}
	push := Delivery{Purpose: "sos_escalation", IncidentRevision: 2, CreatedAt: at, MemberID: "daniel", ContactMethodType: "pushover_notification", State: "suppressed", Reason: "incident claimed"}
	for _, test := range []struct {
		pushState, want string
	}{
		{"suppressed", "MMHotline was about to wake Daniel Reyes (on call)."},
		{"accepted", "MMHotline woke Daniel Reyes (on call)."},
	} {
		push.State = test.pushState
		_, sentNothing := r.reached(round, []Delivery{push, call})
		if got := r.wokeText(round, sentNothing); got != test.want {
			t.Errorf("Pushover %s: %q, want %q", test.pushState, got, test.want)
		}
	}
	// A round with no notifications at all keeps saying whom it woke.
	if _, sentNothing := r.reached(round, nil); r.wokeText(round, sentNothing) != "MMHotline woke Daniel Reyes (on call)." {
		t.Error("a round with no notifications")
	}
}

// A notice about no alert, such as the claim result a Pushover
// acknowledgement sends, has a subject and no title; the incident's history
// names it by its subject instead of printing an empty name.
func TestTheHistoryNamesANoticeWithoutATitleBySubject(t *testing.T) {
	r := incidentReader{id: "reader", names: map[string]string{"daniel": "Daniel Reyes"}, now: time.Date(2026, 11, 3, 3, 20, 0, 0, time.UTC), zone: time.UTC}
	notice := Delivery{Purpose: "system_notice", Subject: "MMHotline claim result", Message: "You claimed Disk full.", CreatedAt: r.now, MemberID: "daniel", ContactMethodType: "pushover_notification", State: "accepted"}
	rows := r.deliveries([]Delivery{notice})
	if len(rows) != 1 || !strings.Contains(rows[0].What, "MMHotline's notice “MMHotline claim result”") {
		t.Fatalf("history line: %+v", rows)
	}
}
