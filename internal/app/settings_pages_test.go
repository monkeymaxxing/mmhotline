package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/statuspage"
)

// Settings pages distinguish the saved setup from a later failed check.
// Times match the team's footer, and settings paths appear only in a fold.
func TestSettingsPagesShowSavedSetupsAndFailedChecks(t *testing.T) {
	t.Parallel()
	smtp := newSMTPFixture(t, "250 accepted", "", false, nil)
	f := newSettingsFileFixture(t, fiveSendingServices(t, smtp.server.Addr().String()))
	f.a.Now = func() time.Time { return time.Date(2026, 10, 30, 17, 45, 0, 0, time.UTC) }
	f.start()
	configureAccessSignIn(t, f.a)
	server := httptest.NewServer(f.a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, "ana@example.com")
	check := func(path string, wants ...string) string {
		t.Helper()
		page := getPage(t, client, server.URL+path)
		for _, want := range wants {
			if !strings.Contains(settingsPageText(page), want) {
				t.Fatalf("%s lacks %q: %s", path, want, page)
			}
		}
		if strings.Contains(foldedDetails.ReplaceAllString(page, ""), f.path) {
			t.Fatalf("%s shows the settings path outside a fold", path)
		}
		if strings.Count(page, f.path) != 1 {
			t.Fatalf("%s repeats the settings path", path)
		}
		return page
	}
	check("/sending-services", "13:45", "Set up by the settings file")
	check("/triage-agent", "Sign-in to Claude Code works", "Last checked at 13:45", "Check the sign-in now")
	check("/agent-playbook-rules", "Changed 13:45 by the settings file")
	for _, service := range []string{"pushover", "twilio", "telegram", "slack", "mail_server"} {
		path := "/sending-services/" + service
		check(path, "Set up · last check passed", "13:45", "Contact methods")
		// Apply records failures here. A failed replacement keeps the saved setup.
		if _, err := f.a.Store.db.Exec(`UPDATE settings_file_parts SET failure_reason='The sending service refused the credential.',failed_at=? WHERE part=?`, f.a.Now().Unix(), "sending_services."+service); err != nil {
			t.Fatal(err)
		}
		check(path, "The latest settings check failed at 13:45", "The earlier setup remains in use", "Fix the settings file", "The sending service refused the credential")
		check("/sending-services", "The latest settings check failed at 13:45")
	}
	// Notice history also uses the team's time zone; the public page keeps UTC.
	p := Principal{SignInEmailAddress: "ana@example.com"}
	if _, _, err := f.a.PostToStatusPage(context.Background(), p, statuspage.PostNoticeInput{Title: "Delayed orders", Impact: statuspage.ImpactOutage, Message: "Investigating delayed orders."}); err != nil {
		t.Fatal(err)
	}
	check("/status-page", "Delayed orders", "Notice 1, opened 13:45", "Investigating delayed orders.", "Add an update", "Resolve this notice")
	if _, _, err := f.a.ResolveStatusPageNotice(context.Background(), p, statuspage.ResolveInput{NoticeID: 1, ClosingMessage: "Orders work again."}); err != nil {
		t.Fatal(err)
	}
	check("/status-page", "No open notices", "Recently resolved", "resolved 13:45", "Orders work again.")
}

func TestSendingServiceGuidesExplainMissingSetupAndFailedFirstCheck(t *testing.T) {
	t.Parallel()
	h := newWorkflowRig(t)
	admin := h.browser()
	admin.addPerson("admin@example.test", "Administrator")
	if err := h.app.RemoveSendingServiceSetup(context.Background(), Principal{UnixUser: "fixture"}, "mail_server"); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"pushover", "twilio", "telegram", "slack", "mail_server"} {
		page := admin.get("/sending-services/"+service, 200)
		for _, want := range []string{"Not set up", "Follow the guide below", "sudo mmhotline settings apply", "Example settings"} {
			if !strings.Contains(settingsPageText(page), want) {
				t.Fatalf("%s lacks %q", service, want)
			}
		}
		if _, err := h.app.Store.db.Exec(`INSERT INTO settings_file_parts(part,failure_reason,failed_at) VALUES(?,?,?)`, "sending_services."+service, "The service refused the credential.", h.app.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		page = admin.get("/sending-services/"+service, 200)
		if !strings.Contains(page, "The service is not set up.") || strings.Contains(page, "The earlier setup remains in use.") {
			t.Fatalf("%s claims an earlier setup exists: %s", service, page)
		}
	}
}
