package app

import (
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

func claimCommand(t *testing.T, r *triageHoldRig, action, id, email, note string, want int) (CommandResult, string) {
	t.Helper()
	return commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: action, Incidents: IncidentsCommandRequest{IncidentID: id, SignInEmailAddress: email, Note: note}}, want)
}

// A claim by the command is the page's claim: the member owns the incident,
// escalation stops, and the history names the member and the Unix user.
// Fails without the claim action (the request is refused) or without
// claimIncidentTx (the backup would wake).
func TestIncidentsClaimCommandClaimsForAMember(t *testing.T) {
	r := newTriageHoldRig(t)
	real := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	id := real.IncidentID
	r.at(t, 6*time.Minute)
	result, raw := claimCommand(t, r, "claim", id, " "+strings.ToUpper(r.s.Principal.SignInEmailAddress)+" ", "looking now", 200)
	assertCommandKeys(t, raw, "incident", "message")
	if result.Incident.Status != "claimed" || result.Incident.ClaimedBy != "Responder" {
		t.Fatalf("incident: %+v", result.Incident)
	}
	want := "Responder now owns SOS incident " + id + ". MMHotline stopped contacting people about it. Next: when the problem is fixed, run: sudo mmhotline incidents resolve --incident-id " + id
	if result.Message != want {
		t.Fatalf("message %q, want %q", result.Message, want)
	}
	if !inOrder(r.history(t, id), "Claimed by Responder with the mmhotline command, run by alice") {
		t.Fatalf("history: %q", r.history(t, id))
	}
	shown, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "show", Incidents: IncidentsCommandRequest{IncidentID: id}}, 200)
	if h := shown.Incident.History; h[len(h)-1].Note != "looking now" {
		t.Fatalf("history: %+v", h)
	}
	before := r.emergencies(id, fakesendingservices.PushoverOtherUserKey)
	r.advance(30 * time.Minute)
	backgroundRounds(t, r.a)
	if r.emergencies(id, fakesendingservices.PushoverOtherUserKey) != before {
		t.Fatal("escalation went on after the claim")
	}
}

// Fails if resolve is not recorded as the Unix user, or the rules differ.
func TestIncidentsResolveCommandResolvesAsTheUnixUser(t *testing.T) {
	r := newTriageHoldRig(t)
	id := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk").IncidentID
	claimCommand(t, r, "claim", id, r.s.Principal.SignInEmailAddress, "", 200)
	result, _ := claimCommand(t, r, "resolve", id, "", "disk cleaned", 200)
	if result.Incident.Status != "resolved" || result.Message != "SOS incident "+id+" is resolved. MMHotline stopped contacting people about it. Nothing more to do." {
		t.Fatalf("result: %+v %q", result.Incident, result.Message)
	}
	if !inOrder(r.history(t, id), "Resolved by the mmhotline command, run by alice") {
		t.Fatalf("history: %q", r.history(t, id))
	}
	// An open incident resolves without a claim.
	open := r.sosArrives(t, "Another", "other").IncidentID
	claimCommand(t, r, "resolve", open, "", "", 200)
	r.status(t, open, "resolved")
}

// Fails if the member is not checked.
func TestIncidentsClaimCommandRefusesMissingOrUnknownMember(t *testing.T) {
	r := newTriageHoldRig(t)
	id := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk").IncidentID
	missing, _ := claimCommand(t, r, "claim", id, "", "", 400)
	if missing.Message != "--sign-in-email-address is required: name the member who claims the incident, because the Unix user alice is not linked to a member" {
		t.Fatalf("message %q", missing.Message)
	}
	unknown, _ := claimCommand(t, r, "claim", id, "nobody@example.test", "", 404)
	if unknown.Message != "no member has sign-in email address nobody@example.test" {
		t.Fatalf("message %q", unknown.Message)
	}
	claimCommand(t, r, "claim", id, "not an address", "", 400)
	r.status(t, id, "open")
}

// The page lets a claim take over; the command does too, with the page's
// "Taken over by" history line. Fails if the take-over is refused or recorded
// as a first claim.
func TestIncidentsClaimCommandTakesOverLikeThePage(t *testing.T) {
	r := newTriageHoldRig(t)
	id := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk").IncidentID
	claimCommand(t, r, "claim", id, r.s.Principal.SignInEmailAddress, "", 200)
	again, _ := claimCommand(t, r, "claim", id, r.s.Principal.SignInEmailAddress, "", 200)
	if !strings.HasPrefix(again.Message, "Responder already owns SOS incident "+id+". Nothing was changed.") {
		t.Fatalf("message %q", again.Message)
	}
	result, _ := claimCommand(t, r, "claim", id, "barbara@example.test", "", 200)
	if !strings.HasPrefix(result.Message, "Barbara took over SOS incident "+id+" from Responder.") || result.Incident.ClaimedBy != "Barbara" {
		t.Fatalf("message %q incident %+v", result.Message, result.Incident)
	}
	if !inOrder(r.history(t, id), "Taken over by Barbara from Responder with the mmhotline command, run by alice") {
		t.Fatalf("history: %q", r.history(t, id))
	}
}

// Fails if an unknown or resolved incident is not refused plainly.
func TestIncidentsClaimAndResolveCommandsRefuseUnknownAndResolvedIncidents(t *testing.T) {
	r := newTriageHoldRig(t)
	unknown, _ := claimCommand(t, r, "claim", "nosuchid", r.s.Principal.SignInEmailAddress, "", 404)
	if unknown.Message != "no SOS incident has ID nosuchid" {
		t.Fatalf("message %q", unknown.Message)
	}
	claimCommand(t, r, "resolve", "nosuchid", "", "", 404)
	id := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk").IncidentID
	claimCommand(t, r, "resolve", id, "", "", 200)
	for _, action := range []string{"claim", "resolve"} {
		result, _ := claimCommand(t, r, action, id, r.s.Principal.SignInEmailAddress, "", 409)
		if !strings.HasPrefix(result.Message, "SOS incident "+id+" is already resolved at ") || !strings.HasSuffix(result.Message, "Nothing was changed") {
			t.Fatalf("%s message %q", action, result.Message)
		}
	}
	long, _ := claimCommand(t, r, "claim", id, r.s.Principal.SignInEmailAddress, strings.Repeat("x", 1000), 400)
	if long.Message == "" {
		t.Fatal("a too long note was not refused")
	}
}

// A test SOS claimed by the command resolves itself, and the history is the
// page's; send-test-sos's wait reads the claim from the history. Fails if
// the claim bypasses claimIncidentTx.
func TestIncidentsClaimCommandResolvesATestSOSAndEndsTheWait(t *testing.T) {
	r := newTriageHoldRig(t)
	id := r.sendTestSOS(t)
	backgroundRounds(t, r.a)
	result, _ := claimCommand(t, r, "claim", id, r.s.Principal.SignInEmailAddress, "", 200)
	if result.Incident.Status != "resolved" || result.Message != "Responder claimed SOS incident "+id+". It was a test, so MMHotline resolved it after the claim. A claim from the terminal does not show that the phone woke: only Acknowledge on the phone does, so the contact method's emergency test is unchanged." {
		t.Fatalf("result: %+v %q", result.Incident, result.Message)
	}
	if !inOrder(r.history(t, id), "Claimed by Responder with the mmhotline command, run by alice", "Resolved by MMHotline after the claim, because this incident was a test") {
		t.Fatalf("history: %q", r.history(t, id))
	}
	shown, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "show", Incidents: IncidentsCommandRequest{IncidentID: id}}, 200)
	test := CommandTestSOS{IncidentID: id, IncidentPageURL: "page"}
	if !test.Update(*shown.Incident) || test.ClaimedBy != "Responder" || test.ResolvedAt.IsZero() {
		t.Fatalf("the wait did not see the claim: %+v", test)
	}
	if !strings.Contains(test.Text(), "Responder claimed the test with the mmhotline command, run by alice, at ") {
		t.Fatalf("text:\n%s", test.Text())
	}
}
