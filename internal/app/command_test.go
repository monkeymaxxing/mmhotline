package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func commandRequest(t *testing.T, a *App, request CommandRequest, want int) (CommandResult, string) {
	t.Helper()
	request.UnixUser = "alice"
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.CommandHandler().ServeHTTP(response, httptest.NewRequest("POST", "http://mmhotline/command", bytes.NewReader(raw)))
	if response.Code != want {
		t.Fatalf("%s %s: HTTP %d: %s", request.Group, request.Action, response.Code, response.Body.String())
	}
	var result CommandResult
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result, response.Body.String()
}
func assertCommandKeys(t *testing.T, raw string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if !strings.Contains(raw, `"`+key+`":`) {
			t.Errorf("missing JSON field %s: %s", key, raw)
		}
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	var check func(any)
	check = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if key != strings.ToLower(key) {
					t.Errorf("non-snake-case field %s", key)
				}
				check(child)
			}
		case []any:
			for _, child := range v {
				check(child)
			}
		}
	}
	check(value)
}
func TestCommandPeopleHandler(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	addPerson(t, a, "Barbara@Example.com", "Barbara", RoleAdministrator)
	result, raw := commandRequest(t, a, CommandRequest{Group: "people", Action: "list"}, 200)
	assertCommandKeys(t, raw, "people", "sign_in_email_address", "name", "role", "message", "web_url")
	if len(result.People) != 1 || result.People[0].SignInEmailAddress != "barbara@example.com" {
		t.Fatal(raw)
	}
}

// TestRemovedCommandsAreRefusedAsUnknown: the settings file sets what these
// commands changed, so the command socket answers each as unknown.
func TestRemovedCommandsAreRefusedAsUnknown(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	for group, actions := range map[string][]string{
		"people":               {"add", "change", "remove"},
		"schedules":            {"save", "activate", "set-response-window"},
		"agent-playbook-rules": {"apply-preset", "set", "set-services-whose-changes-a-person-must-approve"},
		"alert-sources":        {"add", "add-signing-secret", "revoke-signing-secret", "disable", "set-check-in-times"},
		"triage-agent":         {"set-internet-access", "add-listed-host", "remove-listed-host", "set-software-review-interval"},
	} {
		for _, action := range actions {
			commandRequest(t, a, CommandRequest{Group: group, Action: action}, 404)
		}
	}
}
func TestCommandContactMethodsHandlerAndPageOwnership(t *testing.T) {
	t.Parallel()
	a, admin, members := coverageFixture(t)
	a.WebURL = "https://mmhotline.example.com"
	severities := []string{"sos", "warn", "info"}
	send := true
	in := ContactMethodsCommandRequest{SignInEmailAddress: members[1].SignInEmailAddress, ContactMethodType: "email", EmailAddress: "backup@example.com", AlertSeverities: &severities, SendWhileUnavailableOrMuted: &send}
	result, raw := commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: in}, 200)
	assertCommandKeys(t, raw, "contact_methods", "contact_method_id", "sign_in_email_address", "shared_with_team", "contact_method_type", "sends_to", "alert_severities", "send_while_unavailable_or_muted", "last_test_message", "delivery_state", "delivery_state_explanation", "queued_at")
	if len(result.ContactMethods) != 1 || result.ContactMethods[0].LastTestMessage.DeliveryState != "queued" || !strings.Contains(result.Message, "test message for the contact method is queued") {
		t.Fatal(raw)
	}
	var fields struct {
		ContactMethods []map[string]json.RawMessage `json:"contact_methods"`
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || len(fields.ContactMethods[0]) != 8 {
		t.Fatalf("wrong contact method key set: %s %v", raw, err)
	}
	var testFields map[string]json.RawMessage
	if err := json.Unmarshal(fields.ContactMethods[0]["last_test_message"], &testFields); err != nil || len(testFields) != 3 {
		t.Fatalf("wrong last test message key set: %s %v", raw, err)
	}
	id := result.ContactMethods[0].ContactMethodID
	methods, err := a.ContactMethods(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(methods[0].LastTest.Message, "the mmhotline command, run by alice added") {
		t.Fatal(methods[0].LastTest.Message)
	}
	lastTest := result.ContactMethods[0].LastTestMessage
	if result.ContactMethods[0].SendsTo != in.EmailAddress || lastTest.DeliveryStateExplanation != methods[0].LastTest.Reason || !lastTest.QueuedAt.Equal(methods[0].LastTest.CreatedAt) {
		t.Fatal("command changed destination or last test message values")
	}
	// The browser administrator still cannot edit another person's contact method.
	if err = a.RemoveContactMethod(context.Background(), admin, id, methods[0].Revision); err != ErrForbidden {
		t.Fatalf("page ownership changed: %v", err)
	}
	send = false
	severities = []string{"sos"}
	result, _ = commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "change", ContactMethods: ContactMethodsCommandRequest{ContactMethodID: id, AlertSeverities: &severities, SendWhileUnavailableOrMuted: &send}}, 200)
	if result.ContactMethods[0].SendWhileUnavailableOrMuted || !reflect.DeepEqual(result.ContactMethods[0].AlertSeverities, severities) {
		t.Fatal(result)
	}
	commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "list", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: members[1].SignInEmailAddress}}, 200)
	// The save test shares the explicit Send test rate limit.
	commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "send-test", ContactMethods: ContactMethodsCommandRequest{ContactMethodID: id}}, 429)
	now := a.Now()
	a.Now = func() time.Time { return now.Add(2 * time.Minute) }
	commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "send-test", ContactMethods: ContactMethodsCommandRequest{ContactMethodID: id}}, 200)
	for _, contactMethodType := range []string{"telegram_message", "slack_message"} {
		in.ContactMethodType = contactMethodType
		result, _ = commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: in}, 400)
		if !strings.Contains(result.Message, "the person connects "+contactMethodType) || !strings.Contains(result.Message, a.WebURL+"/contact-methods") {
			t.Fatal(result.Message)
		}
	}
	commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "remove", ContactMethods: ContactMethodsCommandRequest{ContactMethodID: id}}, 200)
	result, _ = commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "remove", ContactMethods: ContactMethodsCommandRequest{ContactMethodID: id}}, 404)
	if !strings.Contains(result.Message, id) {
		t.Fatal(result.Message)
	}
}
func TestCommandWebhookSecretAndPhoneSave(t *testing.T) {
	t.Parallel()
	a, _, members := coverageFixture(t)
	severities := []string{"sos"}
	result, raw := commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: ContactMethodsCommandRequest{SharedWithTeam: true, ContactMethodType: "outgoing_webhook", OutgoingWebhookURL: "https://example.com/private-path?token=hidden", OutgoingWebhookBearerToken: "private-bearer", AlertSeverities: &severities}}, 200)
	if len(result.OutgoingWebhookSigningSecretShownOnce) != 64 || result.OutgoingWebhookSigningSecretID == "" || strings.Contains(raw, "private-bearer") {
		t.Fatal("webhook secret is missing or leaks stored contact method type secrets")
	}
	assertCommandKeys(t, raw, "outgoing_webhook_signing_secret_id", "outgoing_webhook_signing_secret_shown_once")
	if result.ContactMethods[0].SendsTo != "https://example.com/…" || strings.Contains(raw, "private-path") || strings.Contains(raw, "token=hidden") {
		t.Fatal(raw)
	}
	listed, _ := commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "list", ContactMethods: ContactMethodsCommandRequest{SharedWithTeam: true}}, 200)
	if len(listed.ContactMethods) != 1 || !listed.ContactMethods[0].SharedWithTeam {
		t.Fatal(listed)
	}
	id := result.ContactMethods[0].ContactMethodID
	result, raw = commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "change", ContactMethods: ContactMethodsCommandRequest{ContactMethodID: id, AlertSeverities: &severities}}, 200)
	if result.OutgoingWebhookSigningSecretShownOnce != "" || strings.Contains(raw, "private-bearer") {
		t.Fatal("secret returned after creation")
	}
	recordTwilioSetUpForTest(t, a.Store)
	in := ContactMethodsCommandRequest{SignInEmailAddress: members[1].SignInEmailAddress, ContactMethodType: "automated_voice_call", PhoneNumberToCall: "+12025550123", AlertSeverities: &severities}
	result, _ = commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: in}, 200)
	if result.ContactMethods[0].SendsTo != in.PhoneNumberToCall {
		t.Fatal(result)
	}
	result, _ = commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: in}, 409)
	if !strings.Contains(result.Message, "already added") {
		t.Fatal(result)
	}
	in.SharedWithTeam = true
	in.SignInEmailAddress = ""
	commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: in}, 400)
}
func TestCommandAlertSourcesHandler(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	a.WebURL = "https://mmhotline.example.com"
	a.AlertIntakeListenAddress = "10.0.0.5:8080"
	source, err := createAlertSource(context.Background(), a, Principal{UnixUser: "fixture"}, "Monitoring")
	if err != nil {
		t.Fatal(err)
	}
	result, raw := commandRequest(t, a, CommandRequest{Group: "alert-sources", Action: "list"}, 200)
	assertCommandKeys(t, raw, "alert_sources", "alert_source_id", "intake_url", "check_in_url", "alert_intake_address", "active", "signing_secrets", "signing_secret_id", "created_at", "check_in_state", "expected", "last_check_in_at", "last_heard_at", "sent_by_node_name", "warn_after_no_check_in_for_minutes", "sos_after_no_check_in_for_minutes", "nodes_in_whole_node_maintenance", "planned_downtime")
	if strings.Contains(raw, source.Secret) || len(result.AlertSources) != 1 {
		t.Fatal("list leaked secret or lost source")
	}
	listed := result.AlertSources[0]
	if listed.IntakeURL != "http://10.0.0.5:8080/alert-intake/"+source.Source.ID || listed.CheckInURL != "http://10.0.0.5:8080/check-in/"+source.Source.ID || len(listed.SigningSecrets) != 1 || listed.SigningSecrets[0].SigningSecretID != source.SigningSecretID {
		t.Fatal(raw)
	}
	if err := disableAlertSource(context.Background(), a, Principal{UnixUser: "fixture"}, source.Source.ID); err != nil {
		t.Fatal(err)
	}
	result, _ = commandRequest(t, a, CommandRequest{Group: "alert-sources", Action: "list"}, 200)
	if result.AlertSources[0].Active || len(result.AlertSources[0].SigningSecrets) != 0 {
		t.Fatal(result)
	}
}
func TestScheduleFileSignInEmailAddressTranslation(t *testing.T) {
	t.Parallel()
	members := []Member{{ID: "person-1", SignInEmailAddress: "barbara@example.com"}, {ID: "person-2", SignInEmailAddress: "carlos@example.com"}}
	original := ScheduleConfig{Kind: "custom_schedule_code", TimeZone: "UTC", Assignments: []string{"person-1", ""}, RotationMemberIDs: []string{"person-2", "person-1"}, BackupKind: "fixed_person", BackupMemberID: "person-2", CustomScheduleCode: "unchanged source", CustomScheduleTestCases: []CustomScheduleTestCase{{When: "2026-10-05T09:00", ExpectedPrimaryMemberID: "person-1", ExpectedBackupMemberID: "person-2"}}}
	file, err := scheduleFile(original, members)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(file)
	assertCommandKeys(t, string(raw), "schedule_type", "assignment_sign_in_email_addresses", "rotation_sign_in_email_addresses", "backup_sign_in_email_address", "local_time", "expected_primary_sign_in_email_address", "expected_backup_sign_in_email_address")
	if strings.Contains(string(raw), "person-1") || strings.Contains(string(raw), `"revision"`) || strings.Contains(string(raw), `"kind"`) {
		t.Fatal(string(raw))
	}
	file.AssignmentSignInEmailAddresses[0] = "BARBARA@EXAMPLE.COM"
	roundTrip, err := settingsSchedule(file).scheduleConfigForMembers(members)
	if err != nil || !reflect.DeepEqual(roundTrip, original) {
		t.Fatalf("round trip: %+v %v", roundTrip, err)
	}
	file.BackupSignInEmailAddress = "unknown@example.com"
	if _, err = settingsSchedule(file).scheduleConfigForMembers(members); err == nil || !strings.Contains(err.Error(), "unknown@example.com") {
		t.Fatal(err)
	}
}
func TestCommandSchedulesHandlerAndAudit(t *testing.T) {
	t.Parallel()
	a, _, members := coverageFixture(t)
	a.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	file := ScheduleFile{ScheduleType: "weekly_rotation", TimeZone: "UTC", HandoffHour: 9, HandoffWeekday: 1, FirstStartDate: "2026-09-28", RotationSignInEmailAddresses: []string{members[0].SignInEmailAddress, members[1].SignInEmailAddress}, BackupOption: "next_in_rotation"}
	result, raw := commandRequest(t, a, CommandRequest{Group: "schedules", Action: "preview", Schedules: SchedulesCommandRequest{Schedule: settingsSchedule(file)}}, 200)
	assertCommandKeys(t, raw, "preview", "at", "primary_sign_in_email_address", "backup_option", "backup_sign_in_email_address")
	if len(result.Preview) < 2 {
		t.Fatal(raw)
	}
	state, err := a.Schedules(context.Background(), Principal{UnixUser: "alice"})
	if err != nil || state.Revision != 0 {
		t.Fatalf("preview saved: %+v %v", state, err)
	}
	config, err := settingsSchedule(file).scheduleConfigForMembers(members)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SaveSchedule(context.Background(), a.settingsPrincipal(), config, 0); err != nil {
		t.Fatal(err)
	}
	result, raw = commandRequest(t, a, CommandRequest{Group: "schedules", Action: "show"}, 200)
	assertCommandKeys(t, raw, "schedule_state", "active_schedule_type", "revision", "schedule_configurations", "schedule_type", "rotation_sign_in_email_addresses")
	if result.ScheduleState.ActiveScheduleType != "weekly_rotation" || result.ScheduleState.Revision != 1 || result.ScheduleState.ScheduleConfigurations[0].RotationSignInEmailAddresses[1] != members[1].SignInEmailAddress {
		t.Fatal(raw)
	}
	if !strings.Contains(result.Message, "/etc/mmhotline/mmhotline_settings.yml, in on_call_schedule") {
		t.Fatal(result.Message)
	}
	var actor, message string
	if err = a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT actor_member_id,message FROM coverage_changes ORDER BY id DESC LIMIT 1`).Scan(&actor, &message)
	}); err != nil {
		t.Fatal(err)
	}
	if actor != "" || !strings.Contains(message, "the settings file /etc/mmhotline/mmhotline_settings.yml") {
		t.Fatalf("settings file audit: %q %q", actor, message)
	}
	file.RotationSignInEmailAddresses = []string{"missing@example.com"}
	result, _ = commandRequest(t, a, CommandRequest{Group: "schedules", Action: "preview", Schedules: SchedulesCommandRequest{Schedule: settingsSchedule(file)}}, 400)
	if !strings.Contains(result.Message, "missing@example.com") || !strings.Contains(result.Message, "apply the settings file with those people first") {
		t.Fatal(result.Message)
	}
}

// schedules show reports the time to claim; the settings file sets it in
// whole minutes from 1 to 60.
func TestCommandEscalateIfUnclaimedAfter(t *testing.T) {
	t.Parallel()
	a, _, members := coverageFixture(t)
	a.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	ctx := context.Background()
	shown := func() int {
		t.Helper()
		result, raw := commandRequest(t, a, CommandRequest{Group: "schedules", Action: "show"}, 200)
		assertCommandKeys(t, raw, "escalate_if_unclaimed_after_minutes")
		return result.ScheduleState.EscalateIfUnclaimedAfterMinutes
	}
	if minutes := shown(); minutes != 5 {
		t.Fatalf("schedules show reports %d minutes before any change; the default is 5", minutes)
	}
	for _, refused := range []int{0, 61} {
		if err := a.SetEscalateIfUnclaimedAfter(ctx, a.settingsPrincipal(), refused); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "from 1 to 60") {
			t.Fatalf("%d minutes: %v", refused, err)
		}
	}
	if err := a.SetEscalateIfUnclaimedAfter(ctx, Principal{SignInEmailAddress: members[1].SignInEmailAddress}, 2); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member who is not an administrator set the time to claim: %v", err)
	}
	if minutes := shown(); minutes != 5 {
		t.Fatalf("a refused change left %d minutes", minutes)
	}
	if err := a.SetEscalateIfUnclaimedAfter(ctx, a.settingsPrincipal(), 60); err != nil {
		t.Fatal(err)
	}
	result, _ := commandRequest(t, a, CommandRequest{Group: "schedules", Action: "show"}, 200)
	if !strings.Contains(result.Text(), "Time to claim before MMHotline escalates: 60 minutes.") || result.ScheduleState.EscalateIfUnclaimedAfterMinutes != 60 {
		t.Fatalf("schedules show after setting 60: %s", result.Text())
	}
}
func TestCommandCustomScheduleValidationAndAuthor(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	h.app.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	file := ScheduleFile{ScheduleType: customScheduleKind, TimeZone: "UTC", BackupOption: "fixed_person", BackupSignInEmailAddress: h.barbaraSignInEmailAddress, CustomScheduleCode: "def on_call(at):\n    return {\"primary_sign_in_email_address\": \"carlos@example.com\"}\n", CustomScheduleTestCases: []ScheduleFileTestCase{{LocalTime: "2026-10-05T09:00", ExpectedPrimarySignInEmailAddress: h.carlosInputSignInEmailAddress, ExpectedBackupSignInEmailAddress: h.barbaraSignInEmailAddress}}}
	commandRequest(t, h.app, CommandRequest{Group: "schedules", Action: "preview", Schedules: SchedulesCommandRequest{Schedule: settingsSchedule(file)}}, 200)
	members, err := h.app.TeamMembers(context.Background(), h.admin)
	if err != nil {
		t.Fatal(err)
	}
	save := func(file ScheduleFile) error {
		config, err := settingsSchedule(file).scheduleConfigForMembers(members)
		if err != nil {
			return err
		}
		_, err = h.app.SaveCustomScheduleCode(context.Background(), h.app.settingsPrincipal(), config, h.revision(t))
		return err
	}
	if err := save(file); err != nil {
		t.Fatal(err)
	}
	versions := h.versions(t)
	if len(versions) != 1 || versions[0].CreatedByMemberID != "" || versions[0].CreatedByName != "the settings file /etc/mmhotline/mmhotline_settings.yml" {
		t.Fatalf("custom author: %+v", versions)
	}
	explicitBackup := file
	explicitBackup.BackupOption = "everyone_available"
	explicitBackup.BackupSignInEmailAddress = ""
	explicitBackup.CustomScheduleCode = "def on_call(at):\n    return {\"primary_sign_in_email_address\": \"carlos@example.com\", \"backup_sign_in_email_address\": \"barbara@example.com\"}\n"
	preview, _ := commandRequest(t, h.app, CommandRequest{Group: "schedules", Action: "preview", Schedules: SchedulesCommandRequest{Schedule: settingsSchedule(explicitBackup)}}, 200)
	if len(preview.Preview) == 0 || preview.Preview[0].BackupOption != "fixed_person" || preview.Preview[0].BackupSignInEmailAddress != h.barbaraSignInEmailAddress {
		t.Fatal("custom backup disagrees with live assignment", preview.Preview)
	}
	file.CustomScheduleTestCases[0].ExpectedPrimarySignInEmailAddress = h.barbaraSignInEmailAddress
	before := h.revision(t)
	if err := save(file); err == nil || h.revision(t) != before {
		t.Fatal("failed custom test changed saved schedule")
	}
}
func TestSchedulePreviewExactChangeTimes(t *testing.T) {
	t.Parallel()
	a, _, members := coverageFixture(t)
	now := time.Date(2026, 10, 5, 8, 37, 12, 0, time.UTC)
	a.Now = func() time.Time { return now }
	file := ScheduleFile{ScheduleType: "hour_of_day", TimeZone: "UTC", BackupOption: "everyone_available", AssignmentSignInEmailAddresses: make([]string, 24)}
	file.AssignmentSignInEmailAddresses[9] = members[1].SignInEmailAddress
	result, _ := commandRequest(t, a, CommandRequest{Group: "schedules", Action: "preview", Schedules: SchedulesCommandRequest{Schedule: settingsSchedule(file)}}, 200)
	if len(result.Preview) != 3 || !result.Preview[1].At.Equal(time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)) || result.Preview[1].PrimarySignInEmailAddress != members[1].SignInEmailAddress || !result.Preview[2].At.Equal(time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)) {
		t.Fatal(result.Preview)
	}
}
func TestCommandRejectsMalformedEnvelope(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	for _, body := range []string{`{"group":"people","action":"list","typo":true}`, `{"group":"people","action":"list"} {}`} {
		response := httptest.NewRecorder()
		a.CommandHandler().ServeHTTP(response, httptest.NewRequest("POST", "http://mmhotline/command", strings.NewReader(body)))
		if response.Code != 400 || !strings.Contains(response.Body.String(), `"message"`) {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func TestCommandContactMethodTextNames(t *testing.T) {
	t.Parallel()
	result := CommandResult{ContactMethods: []CommandContactMethod{{ContactMethodID: "method", SharedWithTeam: true, ContactMethodType: "email", SendsTo: "team@example.com"}}}
	if text := result.Text(); !strings.Contains(text, "belongs to the team") || !strings.Contains(text, "Sends to: team@example.com") || !strings.Contains(text, "Last test message: no test message yet") {
		t.Fatal(text)
	}
	result.ContactMethods[0].LastTestMessage = &CommandLastTestMessage{DeliveryState: "failed", DeliveryStateExplanation: "Delivery refused", QueuedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	result.OutgoingWebhookSigningSecretID = "secret-id"
	result.OutgoingWebhookSigningSecretShownOnce = "secret-value"
	result.OutgoingWebhookSigningSecretFile = "/secret-file"
	for _, expected := range []string{
		"Last test message: failed (queued at 2026-10-24T12:00:00Z): Delivery refused",
		"Outgoing webhook signing secret ID: secret-id",
		"Outgoing webhook signing secret (shown only once): secret-value",
		"Outgoing webhook signing secret file: /secret-file",
	} {
		if text := result.Text(); !strings.Contains(text, expected) {
			t.Fatalf("missing %q: %s", expected, text)
		}
	}
	// A test message with no explanation yet, such as one just queued or
	// accepted, ends with its time.
	result.ContactMethods[0].LastTestMessage = &CommandLastTestMessage{DeliveryState: "accepted", QueuedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	if text := result.Text(); !strings.Contains(text, "Last test message: accepted (queued at 2026-10-24T12:00:00Z).\n") {
		t.Fatalf("accepted test message: %s", text)
	}
}

// The alert source's lines call the machine that sends the check-ins, and
// one in maintenance, a server, as the pages do.
func TestCommandAlertSourceTextNamesServers(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	result := CommandResult{AlertSources: []CommandAlertSource{{Name: "Monitoring on util.example.com", AlertSourceID: "source", Active: true, CheckInState: CommandAlertSourceCheckInState{
		Expected: true, LastCheckInAt: until.Add(-time.Hour), SentByNodeName: "util",
		NodesInWholeNodeMaintenance: []NodeInMaintenance{{NodeName: "db01", MaintenanceUntil: until, StartedBy: "matt", Reason: "upgrading"}},
	}}}}
	text := result.Text()
	for _, expected := range []string{"Last check-in received: 2026-10-27T13:00:00Z. Sent from server: util.", "Server db01 is in maintenance until 2026-10-27T14:00:00Z, started by matt: upgrading."} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q", expected)
		}
	}
	if strings.Contains(strings.ToLower(text), "node") {
		t.Errorf("the lines say node for a server")
	}
	if t.Failed() {
		t.Fatal(text)
	}
}

// settingsSchedule is a ScheduleFile as the settings file's on_call_schedule,
// which these tests use as the shorter way to write a schedule.
func settingsSchedule(f ScheduleFile) *SettingsOnCallSchedule {
	s := &SettingsOnCallSchedule{ScheduleType: f.ScheduleType, TimeZone: f.TimeZone, BackupOption: f.BackupOption,
		HandoffHour: &f.HandoffHour, HandoffMinute: &f.HandoffMinute, HandoffWeekday: &f.HandoffWeekday, FirstStartDate: &f.FirstStartDate,
		CustomScheduleCode:             &f.CustomScheduleCode,
		AssignmentSignInEmailAddresses: &f.AssignmentSignInEmailAddresses, RotationSignInEmailAddresses: &f.RotationSignInEmailAddresses}
	if f.BackupSignInEmailAddress != "" {
		s.BackupSignInEmailAddress = &f.BackupSignInEmailAddress
	}
	if f.CustomScheduleTestCases != nil {
		cases := []SettingsScheduleTestCase{}
		for _, test := range f.CustomScheduleTestCases {
			cases = append(cases, SettingsScheduleTestCase(test))
		}
		s.CustomScheduleTestCases = &cases
	}
	return s
}

// An address with spaces around it and capitals is the same person for the
// settings check, the apply, and the schedules preview.
func TestScheduleAddressWithSpacesAndCapitalsIsOnePersonEverywhere(t *testing.T) {
	t.Parallel()
	a, _, members := coverageFixture(t)
	a.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	admin := members[0]
	rotation := []string{" Admin@Example.com "}
	handoffHour, handoffMinute, handoffWeekday, firstStartDate := 9, 0, 1, "2026-09-28"
	schedule := SettingsOnCallSchedule{EscalateIfUnclaimedAfterMinutes: 5, ScheduleType: "weekly_rotation", TimeZone: "UTC", HandoffHour: &handoffHour, HandoffMinute: &handoffMinute, HandoffWeekday: &handoffWeekday, FirstStartDate: &firstStartDate, RotationSignInEmailAddresses: &rotation, BackupOption: "everyone_available"}

	problems := &settingsProblems{}
	checkOnCallScheduleSettings(schedule, []string{admin.SignInEmailAddress}, problems)
	if len(problems.list) != 0 {
		t.Fatalf("check: %v", problems.list)
	}
	result, raw := commandRequest(t, a, CommandRequest{Group: "schedules", Action: "preview", Schedules: SchedulesCommandRequest{Schedule: &schedule}}, 200)
	if len(result.Preview) == 0 || result.Preview[0].PrimarySignInEmailAddress != admin.SignInEmailAddress {
		t.Fatalf("preview: %s", raw)
	}
	if err := applyOnCallScheduleSettings(context.Background(), a, Settings{OnCallSchedule: schedule}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	state, err := a.Schedules(context.Background(), a.settingsPrincipal())
	if err != nil || len(state.Configs) != 1 || !reflect.DeepEqual(state.Configs[0].RotationMemberIDs, []string{admin.ID}) {
		t.Fatalf("saved: %+v %v", state, err)
	}
}
