package app

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// The on-call page lists the coming shifts that the saved Custom schedule
// code worked out, to every member, and keeps the code, its test cases,
// and its saved versions in a fold. The settings file saves the code.
func TestPublicWorkflowCustomScheduleCode(t *testing.T) {
	h := newWorkflowRig(t)
	h.app.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	admin := h.browser()
	admin.addPerson("barbara@example.com", "Barbara")
	carlosID, _ := addTeamMember(t, h.app, "carlos@example.com", "Carlos", RoleMember)
	carlos := h.browser()
	carlos.identify("carlos@example.com")

	code := "def on_call(at):\n    if at.weekday in [\"saturday\", \"sunday\"]:\n        return {\"primary_sign_in_email_address\": \"carlos@example.com\"}\n    return {\"primary_sign_in_email_address\": \"barbara@example.com\"}\n"
	config := ScheduleConfig{Kind: customScheduleKind, TimeZone: "America/New_York", BackupKind: "everyone_available", CustomScheduleCode: code, CustomScheduleTestCases: []CustomScheduleTestCase{{When: "2026-10-10T12:00", ExpectedPrimaryMemberID: carlosID}}}
	if _, err := h.app.SaveCustomScheduleCode(context.Background(), h.app.settingsPrincipal(), config, 0); err != nil {
		t.Fatal(err)
	}
	for browser, other := range map[*workflowBrowser]string{admin: "Carlos", carlos: "Barbara"} {
		page := browser.get("/on-call", 200)
		// Fourteen days ahead hold weekdays and a weekend: Barbara's and
		// Carlos's shifts, the reader's own as "You".
		mustContain(t, "the coming shifts", screenReaderText(page, "coming-on-call"), "Coming shifts Now", "You", other, "MMHotline works out who is on call from your schedule code every hour, 14 days ahead.")
		mustContain(t, "the page", pageText(page), "Your schedule code says who is on call",
			`The schedule code def on_call(at):`, `return {"primary_sign_in_email_address": "barbara@example.com"}`,
			"2026-10-10T12:00 Carlos Not checked", "Saved versions of the code",
			"Where the schedule is set Set in /etc/mmhotline/mmhotline_settings.yml , in on_call_schedule .")
		if strings.Contains(page, "<textarea") || strings.Contains(page, `name="custom_schedule_code"`) {
			t.Fatalf("the page still edits the code: %s", page)
		}
	}
	// The page no longer previews or saves code.
	page := admin.get("/on-call", 200)
	admin.post("/on-call", url.Values{"csrf": {workflowField(t, page, "csrf")}, "revision": {"1"}, "schedule_type": {"custom_schedule_code"}, "time_zone": {"UTC"}, "backup_kind": {"everyone_available"}, "custom_schedule_code": {code}, "test_case_rows": {"0"}, "action": {"preview"}}, 400)
}
