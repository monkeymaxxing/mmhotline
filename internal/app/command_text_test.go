package app

import (
	"strings"
	"testing"
	"time"
)

// sending-services list ends each service's line with one period, also
// when the service's check description already ends with one.
func TestSendingServicesListLineEndsWithOnePeriod(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 15, 5, 41, 53, 0, time.UTC)
	result := CommandResult{SendingServiceSetups: []SendingServiceSetup{{SendingService: "mail_server", SendingServiceTitle: "Mail server", IsSetUp: true, SetUpAt: &at, SetUpBy: "the settings file /etc/mmhotline/mmhotline_settings.yml", Settings: map[string]any{"smtp_server": "127.0.0.1:2525", "smtp_connection_security": "unencrypted", "email_from_address": "alerts@example.com"}}}}
	text := result.Text()
	if strings.Contains(text, "..") || !strings.Contains(text, "The password and alerts cross the network unencrypted.") {
		t.Fatalf("mail server line: %s", text)
	}
}

// alert-sources list counts its minutes in words: "1 minute", never
// "1 minutes".
func TestAlertSourcesListCountsMinutes(t *testing.T) {
	t.Parallel()
	text := CommandResult{AlertSources: []CommandAlertSource{{Name: "Monitoring", AlertSourceID: "monitoring", Active: true, CheckInState: CommandAlertSourceCheckInState{Expected: true, WarnAfterNoCheckInForMinutes: 1, SOSAfterNoCheckInForMinutes: 1}}}}.Text()
	if !strings.Contains(text, "Warn after 1 minute without a check-in. Wake the person on call (SOS) after 1 minute without a check-in.") {
		t.Fatalf("alert-sources list: %s", text)
	}
}

// down-alarm send-test says whether the services accepted each test, so a
// refused one, such as a wrong Pushover key, does not read as a success.
func TestDownAlarmSendTestSaysWhenAServiceRefused(t *testing.T) {
	t.Parallel()
	status := 400
	sends := []DownAlarmSendView{{PersonName: "Carlos Example", ContactMethodType: "Pushover notification", SendOutcome: "refused_by_service", HTTPStatusCode: &status}, {PersonName: "Barbara Example", ContactMethodType: "Pushover notification", SendOutcome: "delivered"}}
	if got := DownAlarmTestSummary(sends); got != "The MMHotline-down alarm sent a test alert for each held message; 1 of 2 was not accepted, so that person would not be woken. Check the contact method with sudo mmhotline contact-methods send-test." {
		t.Fatalf("summary: %q", got)
	}
	if !DownAlarmTestFailed(sends) {
		t.Fatal("a refused test is not a failure")
	}
	sends[0].SendOutcome = "delivered"
	if got := DownAlarmTestSummary(sends); got != "The MMHotline-down alarm sent a test alert for each held message, and each service accepted it." || DownAlarmTestFailed(sends) {
		t.Fatalf("summary: %q", got)
	}
	if got := DownAlarmTestSummary(nil); got != "The MMHotline-down alarm holds no message, so it would wake nobody." || !DownAlarmTestFailed(nil) {
		t.Fatalf("summary: %q", got)
	}
}
