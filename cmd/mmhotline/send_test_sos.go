package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"mmhotline/internal/app"
)

// defaultWaitForClaimMinutes is how long `incidents send-test-sos` waits for
// a claim. Ten minutes covers the time to claim of a team that keeps the
// default (5 minutes, so the person on call has a whole step to answer)
// plus the start of the backup's step. A phone that has not sounded by then
// has a setup problem, which waiting longer does not fix.
const defaultWaitForClaimMinutes = 10

// testSOSPollInterval is how often the command asks whether the test was
// claimed.
var testSOSPollInterval = 2 * time.Second

// testSOSNotClaimedAdvice is what to check first when nobody claims the test.
const testSOSNotClaimedAdvice = "Check first that the Pushover app is installed and signed in on the phone, and that critical alerts are allowed for Pushover in the phone's settings. Then run the command again: a new test closes this one."

// waitForTestSOSClaim waits for a person to claim the test SOS that
// send-test-sos opened, for at most wait, and fills the result with who
// claimed it, how, and when. It returns checkAnswer{exitCode: 1} when
// nobody claimed the test in time or someone resolved it without a claim.
func waitForTestSOSClaim(socket, unixUser string, result app.CommandResult, wait time.Duration, asJSON bool) (app.CommandResult, error) {
	test := result.TestSOS
	if test == nil || test.IncidentID == "" {
		return result, commandError{fmt.Errorf("mmhotline serve did not return the test SOS's incident ID")}
	}
	test.WaitedForClaimMinutes = int(wait / time.Minute)
	say(asJSON, fmt.Sprintf("MMHotline sent a test SOS to the person on call. Waiting up to %s for someone to acknowledge it on the phone.", minutesText(test.WaitedForClaimMinutes)))
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		shown, err := sendCommandWithTimeout(ctx, socket, app.CommandRequest{Group: "incidents", Action: "show", Incidents: app.IncidentsCommandRequest{IncidentID: test.IncidentID}, UnixUser: unixUser}, 30*time.Second)
		if err == nil && shown.Incident != nil && shown.Incident.Status != "open" {
			if test.Update(*shown.Incident) {
				result.Message = "The test SOS worked: a person acknowledged it on the phone."
				return result, nil
			}
			result.Message = "Someone resolved the test SOS without claiming it, so it proved nothing about the phone. Run the command again."
			return result, checkAnswer{exitCode: 1}
		}
		if err != nil && ctx.Err() == nil {
			return result, err
		}
		timer := time.NewTimer(testSOSPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			result.Message = fmt.Sprintf("Nobody acknowledged the test SOS within %s. The incident is still open, so MMHotline keeps trying to reach the person on call. %s", minutesText(test.WaitedForClaimMinutes), testSOSNotClaimedAdvice)
			return result, checkAnswer{exitCode: 1}
		case <-timer.C:
		}
	}
}

func minutesText(minutes int) string {
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// say prints a line before a command's answer, in the text output only: the
// --json output stays one JSON object.
func say(asJSON bool, line string) {
	if asJSON {
		fmt.Fprintln(os.Stderr, line)
		return
	}
	printText(line)
}

// downAlarmTestWarning is said before `down-alarm send-test` sends, in
// capitals, because the test sends the MMHotline-down alarm's emergency
// notification, which sounds on the phone until MMHotline cancels it.
const downAlarmTestWarning = "THE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO."
