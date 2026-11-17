package app

import (
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

func TestAddingAPushoverGroupKeyWarnsInCapitalsAndStillSaves(t *testing.T) {
	r := newPushoverRig(t)
	add := func(key string) (CommandResult, string) {
		severities := []string{"sos", "warn", "info"}
		return commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: "pushover@example.test", ContactMethodType: "pushover_notification", PushoverUserKey: key, AlertSeverities: &severities}}, 200)
	}
	result, raw := add(fakesendingservices.PushoverGroupKey)
	name := strings.ToUpper(memberNameFor(t, r, "pushover@example.test"))
	want := "THIS IS A PUSHOVER GROUP KEY. ACKNOWLEDGING ON A PHONE WILL NOT CLAIM THE INCIDENT FOR " + name + ". USE " + name + "'S OWN PUSHOVER USER KEY. CLAIMING ON THE INCIDENT'S PAGE STILL WORKS."
	if result.PushoverGroupKeyWarning != want || len(result.ContactMethods) != 1 {
		t.Fatalf("warning %q, want %q; result %+v", result.PushoverGroupKeyWarning, want, result)
	}
	if !strings.Contains(result.Text(), "\n\n"+want+"\n") {
		t.Fatalf("the warning is not on its own line with a blank line before and after it: %q", result.Text())
	}
	assertCommandKeys(t, raw, "pushover_group_key_warning")

	// A personal user key gets no warning.
	result, raw = add(fakesendingservices.PushoverUserKey)
	if result.PushoverGroupKeyWarning != "" || strings.Contains(raw, "pushover_group_key_warning") || strings.Contains(result.Text(), "GROUP KEY") {
		t.Fatalf("a user key got a warning: %s", raw)
	}
}

func memberNameFor(t *testing.T, r *pushoverRig, signInEmailAddress string) string {
	t.Helper()
	members, err := r.a.TeamMembers(t.Context(), r.principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.SignInEmailAddress == signInEmailAddress {
			return m.Name
		}
	}
	t.Fatalf("no member %s", signInEmailAddress)
	return ""
}

func TestAnAcknowledgementFromAnotherPushoverUserShowsWhyItDidNotClaim(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	id := r.sos(t, "Database down")
	r.dispatch(t)
	messages := r.fake.PushoverMessages()
	if err := r.fake.PushoverAcknowledgeAs(messages[len(messages)-1].Receipt, fakesendingservices.PushoverGroupKey); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.poll(t)
	r.status(t, id, "open")
	name := memberNameFor(t, r, "pushover@example.test")

	shown, raw := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "show", Incidents: IncidentsCommandRequest{IncidentID: id}}, 200)
	wantHistory := name + "'s phone acknowledged, but the acknowledgement came from another Pushover user, so it did not claim the incident. The contact method uses a key that isn't " + name + "'s own Pushover user key, such as a group key."
	found := false
	for _, entry := range shown.Incident.History {
		found = found || (entry.Kind == "pushover_acknowledgement_did_not_claim" && entry.Message == wantHistory)
	}
	if !found || !strings.Contains(shown.Text(), wantHistory) {
		t.Fatalf("incident history lacks the reason:\n%s", shown.Text())
	}
	assertCommandKeys(t, raw, "history")

	listed, raw := commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "list"}, 200)
	wantProblem := "An acknowledgement in Pushover came from another Pushover user, so it did not claim the incident. This contact method uses a key that isn't " + name + "'s own Pushover user key, such as a group key. To fix it, remove this contact method and add it again with " + name + "'s own Pushover user key."
	if len(listed.ContactMethods) != 1 || listed.ContactMethods[0].PushoverAcknowledgementProblem != wantProblem || !strings.Contains(listed.Text(), "Problem: "+wantProblem) {
		t.Fatalf("contact method lacks the problem:\n%s", listed.Text())
	}
	assertCommandKeys(t, raw, "pushover_acknowledgement_problem")
}
