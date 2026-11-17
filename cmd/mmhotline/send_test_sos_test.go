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
