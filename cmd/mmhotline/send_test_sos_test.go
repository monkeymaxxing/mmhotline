package main

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/app"
)

// testSOSServer stands in for mmhotline serve: send-test-sos opens an
// incident, and show answers with the incident as claimed says.
func testSOSServer(t *testing.T, claimed bool) string {
	t.Helper()
	return testSOSServerWithHistory(t, claimed, nil)
}

// testSOSServerWithHistory is testSOSServer whose open incident also has the
// extra history entries.
func testSOSServerWithHistory(t *testing.T, claimed bool, extra []app.CommandIncidentHistoryEntry) string {
	t.Helper()
	socket := filepath.Join(shortSocketDirectory(t), "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request app.CommandRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		result := app.CommandResult{}
		switch request.Action {
		case "send-test-sos":
			result.TestSOS = &app.CommandTestSOS{IncidentID: "i1", IncidentPageURL: "https://hotline.example.com/incidents/i1"}
			// Echo the person the command sent, so a test can see the flag arrive.
			if request.Incidents.SignInEmailAddress != "" {
				result.TestSOS.SentOnlyToName, result.TestSOS.SentOnlyToSignInEmailAddress = "Robin Example", request.Incidents.SignInEmailAddress
			}
		case "show":
			if request.Incidents.IncidentID != "i1" {
				t.Errorf("show asked for incident %q", request.Incidents.IncidentID)
			}
			incident := app.CommandIncident{IncidentID: "i1", Status: "open", OpenedAt: opened}
			if claimed {
				incident.Status, incident.ClaimedBy, incident.ClaimedAt, incident.ResolvedAt = "resolved", "Ana Silva", opened.Add(40*time.Second), opened.Add(40*time.Second)
				incident.History = []app.CommandIncidentHistoryEntry{
					{Kind: "opened", Message: "SOS incident opened", By: "MMHotline", OccurredAt: opened},
					{Kind: "claimed", Message: "Claimed by Ana Silva, who acknowledged the alert in Pushover", By: "Ana Silva", OccurredAt: opened.Add(40 * time.Second)},
					{Kind: "resolved", Message: "Resolved by MMHotline after the claim, because this incident was a test", By: "MMHotline", OccurredAt: opened.Add(40 * time.Second)},
				}
			}
			if !claimed {
				incident.History = extra
			}
			result.Incident = &incident
		}
		json.NewEncoder(w).Encode(result)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}

// A claimed test prints who claimed it, how, and when, and exits 0.
func TestSendTestSOSPrintsTheClaimAndExitsZero(t *testing.T) {
	socket := testSOSServer(t, true)
	output, err := cliProcess(t, "incidents", "send-test-sos", "--command-socket", socket).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}
	for _, want := range []string{
		"MMHotline sent a test SOS to the person on call. Waiting up to 10 minutes for someone to acknowledge it on the phone.",
		"Incident: https://hotline.example.com/incidents/i1",
		"Ana Silva claimed the test by acknowledging the alert in Pushover, at 2026-11-17T10:00:40Z.",
		"MMHotline resolved the test after the claim, at 2026-11-17T10:00:40Z.",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("the output lacks %q:\n%s", want, output)
		}
	}
	jsonOutput, err := cliProcess(t, "incidents", "send-test-sos", "--command-socket", socket, "--json", "--wait-for-claim-minutes", "3").Output()
	var result app.CommandResult
	if err != nil || json.Unmarshal(jsonOutput, &result) != nil || result.TestSOS == nil || !result.TestSOS.Claimed || result.TestSOS.ClaimedBy != "Ana Silva" || result.TestSOS.WaitedForClaimMinutes != 3 {
		t.Fatalf("--json: %s %v", jsonOutput, err)
	}
}

// Nobody claims within the time limit: exit code 1, the incident stays open,
// and the message says what to check first.
func TestSendTestSOSExitsOneWhenNobodyClaims(t *testing.T) {
	socket := testSOSServer(t, false)
	pause := testSOSPollInterval
	testSOSPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { testSOSPollInterval = pause })
	result, err := waitForTestSOSClaim(socket, "root", app.CommandResult{TestSOS: &app.CommandTestSOS{IncidentID: "i1"}}, 200*time.Millisecond, true)
	if answer, ok := err.(checkAnswer); !ok || answer.exitCode != 1 {
		t.Fatalf("exit: %v", err)
	}
	for _, want := range []string{"Nobody acknowledged the test SOS", "The incident is still open", "Pushover app is installed and signed in on the phone", "critical alerts are allowed for Pushover in the phone's settings"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("the message lacks %q: %s", want, result.Message)
		}
	}
	if result.TestSOS.Claimed {
		t.Fatal("the test was reported as claimed")
	}
}

func TestSendTestSOSTimeLimitFlag(t *testing.T) {
	socket := testSOSServer(t, true)
	output, err := cliProcess(t, "incidents", "send-test-sos", "--command-socket", socket, "--wait-for-claim-minutes", "0").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--wait-for-claim-minutes must be at least 1") {
		t.Fatalf("a time limit of 0: %s %v", output, err)
	}
}

// down-alarm send-test says, before it sends, that the phone may sound once
// more, in capitals with a blank line around it. With --json the line goes
// to standard error, so standard output stays one JSON object.
func TestDownAlarmSendTestWarnsBeforeItSends(t *testing.T) {
	socket := testSOSServer(t, false)
	output, err := cliProcess(t, "down-alarm", "send-test", "--command-socket", socket).CombinedOutput()
	if err != nil && !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("%s %v", output, err)
	}
	if !strings.HasPrefix(string(output), "\nTHE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO.\n\n") {
		t.Fatalf("the warning is not first, in capitals, with a blank line around it:\n%q", output)
	}
	command := cliProcess(t, "down-alarm", "send-test", "--command-socket", socket, "--json")
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	_ = command.Run()
	var result map[string]any
	if json.Unmarshal([]byte(stdout.String()), &result) != nil || !strings.Contains(stderr.String(), "THE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO.") {
		t.Fatalf("--json: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestSendTestSOSSignInEmailAddressFlagReachesTheServer(t *testing.T) {
	socket := testSOSServer(t, true)
	output, err := cliProcess(t, "incidents", "send-test-sos", "--command-socket", socket, "--json", "--sign-in-email-address", "robin@example.com").Output()
	var result app.CommandResult
	if err != nil || json.Unmarshal(output, &result) != nil || result.TestSOS == nil || result.TestSOS.SentOnlyToSignInEmailAddress != "robin@example.com" {
		t.Fatalf("the flag did not reach the server: %s, %v", output, err)
	}
	// The text output says the test went to that one person.
	text, err := cliProcess(t, "incidents", "send-test-sos", "--command-socket", socket, "--sign-in-email-address", "robin@example.com").CombinedOutput()
	if want := "MMHotline sent a test SOS only to Robin Example (robin@example.com), whether or not they are on call. Waiting up to 10 minutes"; err != nil || !strings.Contains(string(text), want) {
		t.Fatalf("the text output lacks the person (%q): %s, %v", want, text, err)
	}
}

// A claim names the member, because a Unix user is not linked to a member;
// the refusal comes before any socket is used. Fails without the flag check.
func TestIncidentsClaimNeedsASignInEmailAddress(t *testing.T) {
	output, err := cliProcess(t, "incidents", "claim", "--command-socket", "/nonexistent.sock", "--incident-id", "abc").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--sign-in-email-address is required: it names the member who claims the incident") {
		t.Fatalf("a claim without a member: %s %v", output, err)
	}
	output, err = cliProcess(t, "incidents", "resolve", "--command-socket", "/nonexistent.sock").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--incident-id is required") {
		t.Fatalf("a resolve without an incident: %s %v", output, err)
	}
}

// An acknowledgement from another Pushover user (a group key) never claims.
// The command stops at once, says why and what to do, and exits 1, instead of
// waiting out the time limit. The limit is an hour, so a command that waits
// fails this test by timing out.
func TestSendTestSOSStopsWhenAnotherPushoverUserAcknowledged(t *testing.T) {
	socket := testSOSServerWithHistory(t, false, []app.CommandIncidentHistoryEntry{
		{Kind: "pushover_acknowledgement_did_not_claim", Message: "Robin Example's phone acknowledged, but ...", By: "Robin Example"},
	})
	pause := testSOSPollInterval
	testSOSPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { testSOSPollInterval = pause })
	done := make(chan struct{})
	var result app.CommandResult
	var err error
	go func() {
		defer close(done)
		result, err = waitForTestSOSClaim(socket, "root", app.CommandResult{TestSOS: &app.CommandTestSOS{IncidentID: "i1"}}, time.Hour, true)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command kept waiting after another Pushover user acknowledged")
	}
	if answer, ok := err.(checkAnswer); !ok || answer.exitCode != 1 {
		t.Fatalf("exit: %v", err)
	}
	for _, want := range []string{"the acknowledgement came from another Pushover user", "Pushover group key", "Remove Robin Example's contact method and add it again with Robin Example's own Pushover user key", "keeps paging Robin Example", "Running this test again closes it"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("the message lacks %q: %s", want, result.Message)
		}
	}
	if result.TestSOS.Claimed {
		t.Fatal("the test was reported as claimed")
	}
}
