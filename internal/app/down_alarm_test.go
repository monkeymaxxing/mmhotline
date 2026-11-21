package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/downalarm"
	"mmhotline/internal/fakecloudflare"
	"mmhotline/internal/fakesendingservices"
)

const downAlarmWorker = "mmhotline-down-alarm-ops-example-com-7db8a22e"

type downAlarmRig struct {
	a                     *App
	fake                  *fakecloudflare.Server
	advance               func(time.Duration)
	barbara, bob          Principal
	barbaraID, bobID      string
	barbaraPushover       string
	barbaraCall, bobPhone string
	reports               []string
	// sendingServices and sendingServicesURL are the fake sending services
	// the rig set Pushover and Twilio up against.
	sendingServices    *fakesendingservices.Server
	sendingServicesURL string
}

// newDownAlarmRig sets up Barbara on call, with Pushover and an automated
// voice call, and Bob as her fixed backup with Pushover. Pushover and Twilio
// are checked against the fake sending services, then rebuilt with their
// real addresses, since only the MMHotline-down alarm's held requests use them here.
func newDownAlarmRig(t *testing.T) *downAlarmRig {
	t.Helper()
	a := foundationApp(t)
	clock := deliveryTestClock(a)
	r := &downAlarmRig{a: a, fake: fakecloudflare.New(t)}
	// The rig stands in for a healthy background loop, which ends a round
	// every second; a round whose visit failed records nothing.
	r.advance = func(d time.Duration) { clock(d); a.teamDeadline.roundEnded(a.Now()) }
	a.Cloudflare = r.fake.Addresses()
	a.UseSettingsFile(InstalledSettingsFilePath, nil)
	a.WebURL = "https://ops.example.com"
	ctx := context.Background()
	r.barbara = memberFixture(t, a, "barbara@example.com").Principal
	r.bob = memberFixture(t, a, "bob@example.com").Principal
	r.sendingServices = fakesendingservices.New()
	services := httptest.NewServer(r.sendingServices)
	t.Cleanup(services.Close)
	r.sendingServicesURL = services.URL
	if err := a.LoadSendingServiceSetups(ctx, nil, services.URL); err != nil {
		t.Fatal(err)
	}
	for name, fields := range map[string]map[string]string{"pushover": {"pushover_application_token": fakesendingservices.PushoverApplicationToken}, "twilio": fakePhoneSetupFields()} {
		if _, err := a.SetUpSendingService(ctx, r.barbara, name, fields); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.LoadSendingServiceSetups(ctx, nil, ""); err != nil {
		t.Fatal(err)
	}
	people, err := a.TeamMembers(ctx, r.barbara)
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range people {
		if person.SignInEmailAddress == "barbara@example.com" {
			r.barbaraID = person.ID
		} else {
			r.bobID = person.ID
		}
	}
	add := func(in ContactMethodsCommandRequest) string {
		severities := []string{"sos", "warn", "info"}
		if in.ContactMethodType == "automated_voice_call" {
			severities = []string{"sos"}
		}
		in.AlertSeverities = &severities
		result, _ := commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: in}, 200)
		return result.ContactMethods[0].ContactMethodID
	}
	r.barbaraPushover = add(ContactMethodsCommandRequest{SignInEmailAddress: "barbara@example.com", ContactMethodType: "pushover_notification", PushoverUserKey: fakesendingservices.PushoverUserKey})
	r.barbaraCall = add(ContactMethodsCommandRequest{SignInEmailAddress: "barbara@example.com", ContactMethodType: "automated_voice_call", PhoneNumberToCall: "+12025550123"})
	r.bobPhone = add(ContactMethodsCommandRequest{SignInEmailAddress: "bob@example.com", ContactMethodType: "pushover_notification", PushoverUserKey: fakesendingservices.PushoverOtherUserKey})
	if _, err := a.SaveSchedule(ctx, r.barbara, ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: []string{r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID}, BackupKind: "fixed_person", BackupMemberID: r.bobID}, 0); err != nil {
		t.Fatal(err)
	}
	return r
}

// setUp applies the settings file's mmhotline_down_alarm section with the
// fake account, as its part does at each apply that changed it, and returns
// down-alarm show. With failure, the part must fail with a reason that holds it.
func (r *downAlarmRig) setUp(t *testing.T, failure string) (CommandResult, string) {
	t.Helper()
	return r.applyDownAlarm(t, &SettingsDownAlarm{CloudflareAccountID: r.fake.AccountID, CloudflareAPIToken: r.fake.APIToken}, failure)
}

// removeSetup applies a settings file without the mmhotline_down_alarm section.
func (r *downAlarmRig) removeSetup(t *testing.T, failure string) {
	t.Helper()
	r.applyDownAlarm(t, nil, failure)
}

func (r *downAlarmRig) applyDownAlarm(t *testing.T, section *SettingsDownAlarm, failure string) (CommandResult, string) {
	t.Helper()
	reason := settingsPartFailureReason(settingsPartNamed(t, "mmhotline_down_alarm").apply(context.Background(), r.a, Settings{DownAlarm: section}), nil)
	if failure == "" && reason != "" || !strings.Contains(reason, failure) {
		t.Fatalf("the mmhotline_down_alarm part's failure %q, want %q", reason, failure)
	}
	return commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "show"}, 200)
}

func (r *downAlarmRig) round() {
	r.a.downAlarmRound(context.Background(), func(err error) { r.reports = append(r.reports, err.Error()) })
}

func (r *downAlarmRig) worker(t *testing.T) fakecloudflare.Worker {
	t.Helper()
	worker, ok := r.fake.Worker(downAlarmWorker)
	if !ok {
		t.Fatal("the Worker was not uploaded")
	}
	return worker
}

func (r *downAlarmRig) lastCheckIn(t *testing.T) map[string]downalarm.HeldMessage {
	t.Helper()
	checkIns := r.worker(t).CheckIns
	if len(checkIns) == 0 {
		t.Fatal("no check-in")
	}
	held := map[string]downalarm.HeldMessage{}
	for _, message := range checkIns[len(checkIns)-1] {
		held[message.HeldMessageID] = message
	}
	return held
}

func (r *downAlarmRig) alertsTitled(t *testing.T, title string) []Alert {
	t.Helper()
	alerts, err := r.a.ListAlerts(context.Background(), r.barbara, AlertFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var found []Alert
	for _, alert := range alerts {
		if alert.Title == title {
			found = append(found, alert)
		}
	}
	return found
}

func TestDownAlarmSetUpUploadsTheWorkerAndChecksInWithTheHeldMessages(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	result, raw := r.setUp(t, "")
	worker := r.worker(t)
	if worker.Module != downalarm.WorkerSource() || !worker.WorkersDev || len(worker.Secrets[downalarm.CheckInTokenSecretName]) != 64 {
		t.Fatalf("Worker %+v", worker.Metadata)
	}
	if migrations, _ := worker.Metadata["migrations"].(map[string]any); migrations["new_tag"] != "v1" {
		t.Fatalf("first upload migrations %v", worker.Metadata["migrations"])
	}
	held := r.lastCheckIn(t)
	if len(held) != 3 {
		t.Fatalf("held messages %v", held)
	}
	pushover := held["primary-contact-method-"+r.barbaraPushover]
	if pushover.DelaySeconds != 300 || pushover.Request.Method != "POST" || pushover.Request.URL != "https://api.pushover.net/1/messages.json" {
		t.Fatalf("Barbara's Pushover %+v", pushover)
	}
	fields, _ := url.ParseQuery(pushover.Request.Body)
	if fields.Get("token") != fakesendingservices.PushoverApplicationToken || fields.Get("user") != fakesendingservices.PushoverUserKey || fields.Get("priority") != "2" || fields.Get("retry") != "30" || fields.Get("expire") != "1500" || !strings.Contains(fields.Get("message"), "MMHotline for Fixture team stopped checking in with the MMHotline-down alarm") || strings.Contains(fields.Get("message"), "SOS") {
		t.Fatalf("Pushover fields %v", fields)
	}
	if testFields, _ := url.ParseQuery(pushover.TestRequest.Body); testFields.Get("title") != "Test of the MMHotline-down alarm" || testFields.Get("expire") != "600" {
		t.Fatalf("Pushover test %v", testFields)
	}
	call := held["primary-contact-method-"+r.barbaraCall]
	callFields, _ := url.ParseQuery(call.Request.Body)
	if call.DelaySeconds != 300 || call.Request.URL != "https://api.twilio.com/2010-04-01/Accounts/"+fakesendingservices.TwilioAccountSID+"/Calls.json" || !strings.HasPrefix(call.Request.Headers["Authorization"], "Basic ") || callFields.Get("To") != "+12025550123" || callFields.Get("From") != fakesendingservices.TwilioPhoneNumberToCallFrom || !strings.Contains(callFields.Get("Twiml"), "This is Hotline for ") || !strings.Contains(callFields.Get("Twiml"), "stopped checking in with the Hotline-down alarm") {
		t.Fatalf("Barbara's call %+v", call)
	}
	if bob := held["backup-contact-method-"+r.bobPhone]; bob.DelaySeconds != 600 {
		t.Fatalf("Bob's held message %+v", bob)
	}
	view := result.DownAlarm
	if view == nil || view.WorkerURL != r.fake.URL+"/workers-dev/"+downAlarmWorker || view.LastCheckInAcceptedAt == nil || len(view.HeldMessages) != 3 || view.SetUpBy != "the settings file "+InstalledSettingsFilePath {
		t.Fatalf("set-up result %s", raw)
	}
	assertCommandKeys(t, raw, "mmhotline_down_alarm", "worker_name", "worker_url", "last_check_in_accepted_at", "held_messages", "person_name", "delay_minutes")
	for _, secret := range []string{r.fake.APIToken, worker.Secrets[downalarm.CheckInTokenSecretName], fakesendingservices.PushoverApplicationToken} {
		if strings.Contains(raw, secret) || strings.Contains(result.Text(), secret) {
			t.Fatal("a secret appears in the result")
		}
	}
	if !strings.Contains(result.Text(), "If MMHotline stops checking in: Pushover notification to Responder") {
		t.Fatalf("text:\n%s", result.Text())
	}

	// Set-up again keeps the Worker, its check-in token, and its Durable Object.
	r.setUp(t, "")
	again := r.worker(t)
	if again.Uploads != 2 || again.Secrets[downalarm.CheckInTokenSecretName] != worker.Secrets[downalarm.CheckInTokenSecretName] {
		t.Fatalf("second set-up: %d uploads, token kept %t", again.Uploads, again.Secrets[downalarm.CheckInTokenSecretName] == worker.Secrets[downalarm.CheckInTokenSecretName])
	}
	if _, ok := again.Metadata["migrations"]; ok {
		t.Fatal("the second upload migrated the Durable Object class again")
	}
}

// A long host name gives a Worker name over 54 characters, which Cloudflare
// refuses while the Worker's preview addresses are on. The set-up turns
// previews off, so the whole name, up to 63 characters, works.
func TestDownAlarmSetUpWithALongHostName(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.a.WebURL = "https://oncall-production.example-company.com"
	name := downAlarmWorkerName(r.a.WebURL)
	if len(name) <= 54 {
		t.Fatalf("Worker name %q has only %d characters", name, len(name))
	}
	r.setUp(t, "")
	if worker, ok := r.fake.Worker(name); !ok || !worker.WorkersDev {
		t.Fatalf("Worker %s uploaded %t, workers.dev on %t", name, ok, worker.WorkersDev)
	}
}

// The settings file's wake times reach the MMHotline-down alarm at its next round: the
// person on call's held messages wait the first time, the backup's the
// second. Changing them uploads no Worker again.
func TestDownAlarmWakeTimesAreTheSettings(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	held := r.lastCheckIn(t)
	if held["primary-contact-method-"+r.barbaraPushover].DelaySeconds != 300 || held["backup-contact-method-"+r.bobPhone].DelaySeconds != 600 {
		t.Fatalf("before the setting: %+v", held)
	}
	wake := DownAlarmWakeTimes{WakeThePersonOnCallAfterMinutes: 15, WakeTheBackupAfterMinutes: 25}
	outcomes, err := r.a.applySettingsParts(context.Background(), Settings{DownAlarm: &SettingsDownAlarm{DownAlarmWakeTimes: wake}}, nil, []settingsFilePart{settingsDownAlarmWakeTimesPart})
	if err != nil || outcomes[0].State != "applied" {
		t.Fatalf("applying the wake times: %+v %v", outcomes, err)
	}
	r.round()
	held = r.lastCheckIn(t)
	if held["primary-contact-method-"+r.barbaraPushover].DelaySeconds != 900 || held["primary-contact-method-"+r.barbaraCall].DelaySeconds != 900 || held["backup-contact-method-"+r.bobPhone].DelaySeconds != 1500 {
		t.Fatalf("after the setting: %+v", held)
	}
	if uploads := r.worker(t).Uploads; uploads != 1 {
		t.Fatalf("changing the wake times uploaded the Worker %d times", uploads)
	}
}

func TestDownAlarmSetUpRefusals(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.fake.WorkersDevSubdomain = ""
	r.setUp(t, "Open Workers & Pages in the Cloudflare dashboard once")
	r.fake.WorkersDevSubdomain = "example-team"
	r.fake.RefuseNext("PUT /accounts/"+r.fake.AccountID+"/workers/scripts/", 1)
	r.setUp(t, "Cloudflare refused to upload Worker "+downAlarmWorker+" (HTTP 403")
	if result, _ := r.applyDownAlarm(t, &SettingsDownAlarm{CloudflareAccountID: "nope", CloudflareAPIToken: r.fake.APIToken}, "the Cloudflare account ID must be"); result.DownAlarm.IsSetUp {
		t.Fatal("a refused set-up was saved")
	}
	// Moving to another account would leave the first Worker holding messages.
	r.setUp(t, "")
	if result, _ := r.applyDownAlarm(t, &SettingsDownAlarm{CloudflareAccountID: "fedcba9876543210fedcba9876543210", CloudflareAPIToken: r.fake.APIToken}, "first apply the settings file without the mmhotline_down_alarm section"); result.DownAlarm.CloudflareAccountID != r.fake.AccountID {
		t.Fatalf("the earlier setup did not stay: %+v", result.DownAlarm)
	}
}

func TestDownAlarmChecksInEveryMinuteAndAtOnceWhenWhoIsOnCallChanges(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	r.advance(5 * time.Second)
	r.round()
	if n := len(r.worker(t).CheckIns); n != 1 {
		t.Fatalf("an unchanged round checked in: %d check-ins", n)
	}
	r.advance(55 * time.Second)
	r.round()
	if n := len(r.worker(t).CheckIns); n != 2 {
		t.Fatalf("no check-in after a minute: %d check-ins", n)
	}
	// Bob takes over: the next round checks in at once with Bob first.
	if _, err := r.a.SaveSchedule(context.Background(), r.barbara, ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: []string{r.bobID, r.bobID, r.bobID, r.bobID, r.bobID, r.bobID, r.bobID}, BackupKind: "fixed_person", BackupMemberID: r.barbaraID}, 1); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.round()
	held := r.lastCheckIn(t)
	if len(r.worker(t).CheckIns) != 3 || held["primary-contact-method-"+r.bobPhone].DelaySeconds != 300 || held["backup-contact-method-"+r.barbaraPushover].DelaySeconds != 600 {
		t.Fatalf("after the handoff: %v", held)
	}
	// During team mute the MMHotline-down alarm wakes nobody; its delays start when the mute ends.
	if _, err := r.a.SetTeamMute(context.Background(), r.barbara, r.a.Now().Add(2*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.round()
	delay := r.lastCheckIn(t)["primary-contact-method-"+r.bobPhone].DelaySeconds
	if delay < 300+2*3600-5 || delay > 300+2*3600+60 {
		t.Fatalf("delay during a two-hour team mute: %d seconds", delay)
	}
}

func TestDownAlarmAllClearReachesThePeopleTheDownAlarmWoke(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	stopped := r.a.Now().Add(-time.Hour).UTC()
	code := 200
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) {
		w.Sends = []downalarm.Send{
			{SendID: 2, HeldMessageID: "backup-contact-method-" + r.bobPhone, SilenceStartedAt: stopped, DueAt: stopped.Add(20 * time.Minute), SendState: "finished", SendOutcome: "delivered", HTTPStatusCode: &code},
			{SendID: 1, HeldMessageID: "primary-contact-method-" + r.barbaraPushover, SilenceStartedAt: stopped, DueAt: stopped.Add(10 * time.Minute), SendState: "finished", SendOutcome: "delivered", HTTPStatusCode: &code},
		}
	})
	r.advance(time.Minute)
	r.round()
	alerts := r.alertsTitled(t, "MMHotline runs again: it was down, and the MMHotline-down alarm woke you")
	if len(alerts) != 1 || alerts[0].Severity != "info" || !strings.Contains(alerts[0].Message, "Pushover notification to Responder, due "+displayTime(stopped.Add(10*time.Minute))+": accepted by the service (HTTP 200)") {
		t.Fatalf("all-clear %+v", alerts)
	}
	var recipients []string
	rows, err := r.a.Store.db.Query(`SELECT DISTINCT member_id FROM deliveries WHERE alert_id=? ORDER BY member_id`, alerts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		recipients = append(recipients, id)
	}
	rows.Close()
	if len(recipients) != 2 || !containsMember(recipients, r.barbaraID) || !containsMember(recipients, r.bobID) {
		t.Fatalf("all-clear recipients %v", recipients)
	}
	// The same sends in the next answer are not announced again.
	r.advance(time.Minute)
	r.round()
	if alerts := r.alertsTitled(t, "MMHotline runs again: it was down, and the MMHotline-down alarm woke you"); len(alerts) != 1 {
		t.Fatalf("%d all-clears", len(alerts))
	}
	result, _ := commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "show"}, 200)
	if len(result.DownAlarm.RecentSends) != 2 || !strings.Contains(result.Text(), "Sent: Pushover notification to Responder") {
		t.Fatalf("show:\n%s", result.Text())
	}
}

func TestDownAlarmSetUpAgainAnnouncesNoEarlierSends(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	stopped := r.a.Now().Add(-time.Hour).UTC()
	code := 200
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) {
		w.Sends = []downalarm.Send{{SendID: 1, HeldMessageID: "primary-contact-method-" + r.barbaraPushover, SilenceStartedAt: stopped, DueAt: stopped.Add(10 * time.Minute), SendState: "finished", SendOutcome: "delivered", HTTPStatusCode: &code}}
	})
	r.advance(time.Minute)
	r.round()
	const allClear = "MMHotline runs again: it was down, and the MMHotline-down alarm woke you"
	if alerts := r.alertsTitled(t, allClear); len(alerts) != 1 {
		t.Fatalf("%d all-clears for the first alarm", len(alerts))
	}
	// A new check-in token: remove the set-up, then set it up again. The
	// Worker keeps its sends for 7 days, and MMHotline already announced them.
	r.removeSetup(t, "")
	r.setUp(t, "")
	r.advance(time.Minute)
	r.round()
	if alerts := r.alertsTitled(t, allClear); len(alerts) != 1 {
		t.Fatalf("%d all-clears after setting the MMHotline-down alarm up again", len(alerts))
	}
	// An alarm after the new set-up is announced.
	restopped := r.a.Now().UTC()
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) {
		w.Sends = append(w.Sends, downalarm.Send{SendID: 2, HeldMessageID: "primary-contact-method-" + r.barbaraPushover, SilenceStartedAt: restopped, DueAt: restopped.Add(10 * time.Minute), SendState: "finished", SendOutcome: "delivered", HTTPStatusCode: &code})
	})
	r.advance(time.Minute)
	r.round()
	alerts := r.alertsTitled(t, allClear)
	if len(alerts) != 2 || !strings.Contains(alerts[0].Message+alerts[1].Message, "due "+displayTime(restopped.Add(10*time.Minute))) {
		t.Fatalf("all-clears after a new alarm: %+v", alerts)
	}
}

func TestDownAlarmFailingCheckInsRaiseOneWarnAlert(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) { w.Unreachable = true })
	for range 9 {
		r.advance(time.Minute)
		r.round()
	}
	if alerts := r.alertsTitled(t, "MMHotline cannot reach the MMHotline-down alarm"); len(alerts) != 0 {
		t.Fatal("an alert before 10 minutes of failed check-ins")
	}
	view, err := r.a.DownAlarm(context.Background(), r.barbara)
	if err != nil || view.CheckInsFailingSince == nil || !strings.Contains(view.LastCheckInError, "cannot reach the MMHotline-down alarm") {
		t.Fatalf("failing view %+v %v", view, err)
	}
	for range 3 {
		r.advance(time.Minute)
		r.round()
	}
	alerts := r.alertsTitled(t, "MMHotline cannot reach the MMHotline-down alarm")
	if len(alerts) != 1 || alerts[0].Severity != "warn" || !strings.Contains(alerts[0].Message, "sends its urgent alerts once, then nothing more") {
		t.Fatalf("failing alerts %+v", alerts)
	}
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) { w.Unreachable = false })
	r.advance(time.Minute)
	r.round()
	if alerts := r.alertsTitled(t, "MMHotline reaches the MMHotline-down alarm again: if MMHotline goes down, the person on call is woken"); len(alerts) != 1 {
		t.Fatalf("%d recovery alerts", len(alerts))
	}
	if len(r.reports) != 2 || !strings.Contains(r.reports[0], "the check-in failed") || !strings.Contains(r.reports[1], "work again") {
		t.Fatalf("log lines %q", r.reports)
	}
}

// While the background loop, which sends alerts, keeps failing, MMHotline
// stops checking in after 2 minutes, so the MMHotline-down alarm wakes the person on
// call. A successful visit brings the check-ins back.
func TestDownAlarmStopsCheckingInWhileTheBackgroundLoopFails(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	ctx := context.Background()
	// The background visit sends through fake sending services, never the
	// real addresses the rig's held messages use.
	services := httptest.NewServer(fakesendingservices.New())
	t.Cleanup(services.Close)
	if err := r.a.LoadSendingServiceSetups(ctx, nil, services.URL); err != nil {
		t.Fatal(err)
	}
	r.setUp(t, "")
	var reported []error
	report := func(err error) { reported = append(reported, err) }
	if !r.a.backgroundRound(ctx, report) || len(reported) != 0 {
		t.Fatalf("the first visit: %v", reported)
	}
	// A visit that cannot read its delivery work fails.
	if _, err := r.a.Store.db.Exec(`ALTER TABLE deliveries RENAME TO deliveries_unreadable`); err != nil {
		t.Fatal(err)
	}
	if !r.a.backgroundRound(ctx, report) || len(reported) == 0 {
		t.Fatal("the visit did not fail")
	}
	for range 2 {
		r.advance(time.Minute)
		r.a.backgroundRound(ctx, report)
		r.round()
	}
	checkIns := len(r.worker(t).CheckIns)
	for range 3 {
		r.advance(time.Minute)
		r.a.backgroundRound(ctx, report)
		r.round()
	}
	if got := len(r.worker(t).CheckIns); got != checkIns {
		t.Fatalf("%d check-ins while the background loop failed for over 2 minutes", got-checkIns)
	}
	view, err := r.a.DownAlarm(ctx, r.barbara)
	if err != nil || view.CheckInsFailingSince == nil || !strings.Contains(view.LastCheckInError, "stopped checking in on purpose") {
		t.Fatalf("view while the background loop fails %+v %v", view, err)
	}
	if len(r.reports) != 1 || !strings.Contains(r.reports[0], "its background work, which sends alerts, has not finished a round since") {
		t.Fatalf("log lines %q", r.reports)
	}
	if _, err := r.a.Store.db.Exec(`ALTER TABLE deliveries_unreadable RENAME TO deliveries`); err != nil {
		t.Fatal(err)
	}
	reported = nil
	if !r.a.backgroundRound(ctx, report) || len(reported) != 0 {
		t.Fatalf("the visit after the repair: %v", reported)
	}
	r.advance(time.Minute)
	r.round()
	if got := len(r.worker(t).CheckIns); got != checkIns+1 {
		t.Fatalf("%d check-ins after a successful visit, want 1", got-checkIns)
	}
}

// The administrators hear once whom the MMHotline-down alarm cannot wake, and again
// only when that changes.
func TestDownAlarmTellsTheAdministratorsOnceWhomItCannotWake(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	const title = "The MMHotline-down alarm cannot wake everyone on call"
	rounds := func() {
		for range 3 {
			r.advance(time.Minute)
			r.round()
		}
	}
	rounds()
	if alerts := r.alertsTitled(t, title); len(alerts) != 0 {
		t.Fatalf("a notice while everyone can be woken: %+v", alerts)
	}
	// Bob, the backup, loses his only waking contact method.
	if _, err := r.a.Store.db.Exec(`UPDATE contact_methods SET deleted=1 WHERE id=?`, r.bobPhone); err != nil {
		t.Fatal(err)
	}
	rounds()
	alerts := r.alertsTitled(t, title)
	if len(alerts) != 1 || alerts[0].Severity != "warn" || !strings.Contains(alerts[0].Message, "Responder, the backup: no Pushover notification or automated voice call subscribed to SOS") {
		t.Fatalf("notices after the backup lost his contact method %+v", alerts)
	}
	// Barbara, on call, keeps only a Pushover notification whose key is
	// incomplete, which the MMHotline-down alarm skips.
	if _, err := r.a.Store.db.Exec(`UPDATE contact_methods SET deleted=1 WHERE id=?`, r.barbaraCall); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Store.db.Exec(`UPDATE contact_methods SET settings_json=json_set(settings_json,'$.pushover_user_key','short') WHERE id=?`, r.barbaraPushover); err != nil {
		t.Fatal(err)
	}
	rounds()
	alerts = r.alertsTitled(t, title)
	if len(alerts) != 2 || !strings.Contains(alerts[0].Message+alerts[1].Message, "Responder, on call: Pushover notification to") || !strings.Contains(alerts[0].Message+alerts[1].Message, "the Pushover user key is incomplete") {
		t.Fatalf("notices after the person on call lost hers %+v", alerts)
	}
	if view, err := r.a.DownAlarm(context.Background(), r.barbara); err != nil || len(view.HeldMessages) != 0 {
		t.Fatalf("held messages %+v %v", view.HeldMessages, err)
	}
}

func TestDownAlarmUpgradeUploadsTheNewWorker(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	token := r.worker(t).Secrets[downalarm.CheckInTokenSecretName]
	if _, err := r.a.Store.db.Exec(`UPDATE mmhotline_down_alarm_setup SET uploaded_worker_sha256='the previous release'`); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	r.round()
	worker := r.worker(t)
	if worker.Uploads != 2 || worker.Secrets[downalarm.CheckInTokenSecretName] != token {
		t.Fatalf("after an upgrade: %d uploads, token kept %t", worker.Uploads, worker.Secrets[downalarm.CheckInTokenSecretName] == token)
	}
	var digest string
	r.a.Store.db.QueryRow(`SELECT uploaded_worker_sha256 FROM mmhotline_down_alarm_setup`).Scan(&digest)
	if digest != downalarm.WorkerDigest() {
		t.Fatalf("stored digest %q", digest)
	}
	r.advance(time.Minute)
	r.round()
	if r.worker(t).Uploads != 2 {
		t.Fatal("an up-to-date Worker was uploaded again")
	}
}

func TestDownAlarmUpgradeUploadFailingForADayTellsTheAdministrators(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	// The person who made the token left the account, so Cloudflare stopped
	// it, and then MMHotline was upgraded.
	if _, err := r.a.Store.db.Exec(`UPDATE mmhotline_down_alarm_setup SET uploaded_worker_sha256='the previous release', cloudflare_api_token='a stopped token'`); err != nil {
		t.Fatal(err)
	}
	const cannotUpdate = "MMHotline cannot update the MMHotline-down alarm, which still runs an older version"
	r.advance(time.Minute)
	r.round()
	for range 23 {
		r.advance(time.Hour)
		r.round()
	}
	if alerts := r.alertsTitled(t, cannotUpdate); len(alerts) != 0 {
		t.Fatal("told before the upload had failed for a day")
	}
	for range 3 {
		r.advance(time.Hour)
		r.round()
	}
	alerts := r.alertsTitled(t, cannotUpdate)
	if len(alerts) != 1 || alerts[0].Severity != "warn" || !strings.Contains(alerts[0].Message, "Account API Tokens") || !strings.Contains(alerts[0].Message, "put it in mmhotline_down_alarm.cloudflare_api_token in the settings file, and apply the file, with your deploy tool or with sudo mmhotline settings apply") {
		t.Fatalf("notices %+v", alerts)
	}
	if _, err := r.a.Store.db.Exec(`UPDATE mmhotline_down_alarm_setup SET cloudflare_api_token=?`, r.fake.APIToken); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Hour)
	r.round()
	if r.worker(t).Uploads != 2 {
		t.Fatalf("%d uploads", r.worker(t).Uploads)
	}
	if alerts := r.alertsTitled(t, "MMHotline updated the MMHotline-down alarm"); len(alerts) != 1 || alerts[0].Severity != "info" {
		t.Fatalf("updated notices %+v", alerts)
	}
}

func TestDownAlarmRemoveSetupHoldsNothingFirst(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) { w.Unreachable = true })
	r.removeSetup(t, "would let the MMHotline-down alarm wake the person on call")
	if result, _ := commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "show"}, 200); !result.DownAlarm.IsSetUp {
		t.Fatal("a refused removal removed the set-up")
	}
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) { w.Unreachable = false })
	r.removeSetup(t, "")
	if held := r.lastCheckIn(t); len(held) != 0 {
		t.Fatalf("the last check-in holds %d messages", len(held))
	}
	if result, _ := commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "show"}, 200); result.DownAlarm.IsSetUp {
		t.Fatal("the set-up stayed")
	}
	checkIns := len(r.worker(t).CheckIns)
	r.advance(time.Minute)
	r.round()
	if len(r.worker(t).CheckIns) != checkIns {
		t.Fatal("MMHotline checked in after the set-up was removed")
	}
	// A Worker already deleted in Cloudflare holds nothing.
	r.setUp(t, "")
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) { w.WorkersDev = false })
	r.removeSetup(t, "")
	// A file without an MMHotline-down alarm, applied again, removes nothing more.
	r.removeSetup(t, "")
}

func TestDownAlarmDashboardCardAndSendATest(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	server := httptest.NewServer(r.a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, "bob@example.com")
	if body := getPage(t, client, server.URL+"/dashboard"); strings.Contains(body, `id="mmhotline-down-alarm"`) {
		t.Fatal("the dashboard shows an MMHotline-down alarm that is not set up")
	}
	r.setUp(t, "")
	r.advance(20 * time.Second)
	body := getPage(t, client, server.URL+"/dashboard")
	panel := liveSectionText(body, "mmhotline-down-alarm")
	pageInOrder(t, panel, "MMHotline-down alarm Checking in", "MMHotline last checked in just now.", "If MMHotline goes quiet, the MMHotline-down alarm wakes:", "THE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO.", "Send a test now", "Where the MMHotline-down alarm is set up", "Set in", InstalledSettingsFilePath, "in mmhotline_down_alarm")
	for _, want := range []string{"Pushover notification to Responder after 5 minutes", "Automated voice call to Responder after 5 minutes", "Pushover notification to Responder after 10 minutes"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the MMHotline-down alarm panel lacks %q", want)
		}
	}
	// The settings file's path is folded away under "Where the MMHotline-down alarm is set up".
	if strings.Contains(foldedDetails.ReplaceAllString(body, ""), InstalledSettingsFilePath) {
		t.Fatalf("the dashboard names the settings file outside a fold:\n%s", body)
	}
	code := 200
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) {
		w.TestSends = []downalarm.TestSend{{HeldMessageID: "primary-contact-method-" + r.barbaraPushover, SendOutcome: "delivered", HTTPStatusCode: &code}}
	})
	token := pageToken(t, client, server.URL, "/dashboard")
	response := postForm(t, client, server.URL, "/mmhotline-down-alarm/send-test", url.Values{"csrf": {token}}, 200)
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(page), "Pushover notification to Responder: accepted by the service (HTTP 200)") {
		t.Fatalf("test page:\n%s", page)
	}
	if r.worker(t).TestsSent != 1 {
		t.Fatal("the test was not sent")
	}
	result, _ := commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "send-test"}, 200)
	if len(result.DownAlarm.TestSends) != 1 || !strings.Contains(result.Text(), "Test: Pushover notification to Responder: accepted by the service (HTTP 200).") {
		t.Fatalf("send-test:\n%s", result.Text())
	}
}

// A new workers.dev address answers with Cloudflare's own 404 for a few
// seconds; set-up waits for it instead of reporting a failure, and still
// reports one when the address never goes live.
func TestDownAlarmSetUpWaitsForANewWorkersDevAddress(t *testing.T) {
	pause := downAlarmFirstCheckInPause
	downAlarmFirstCheckInPause = time.Millisecond
	t.Cleanup(func() { downAlarmFirstCheckInPause = pause })

	r := newDownAlarmRig(t)
	r.fake.WorkersDevNotLiveForRequests = 2
	r.setUp(t, "")
	if len(r.worker(t).CheckIns) != 1 {
		t.Fatalf("check-ins stored: %d", len(r.worker(t).CheckIns))
	}
	// A test send has no send ID or due time, so its JSON shows neither.
	if _, raw := commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "send-test"}, 200); strings.Contains(raw, `"send_id"`) || strings.Contains(raw, "0001-01-01") {
		t.Fatalf("test send shows empty fields: %s", raw)
	}

	// A first check-in that never succeeds fails the part, and MMHotline
	// puts back the setup it had before: none, or the earlier one.
	never := newDownAlarmRig(t)
	never.fake.WorkersDevNotLiveForRequests = downAlarmFirstCheckInTries
	if result, _ := never.setUp(t, "MMHotline's first check-in with the MMHotline-down alarm failed"); result.DownAlarm.IsSetUp {
		t.Fatal("a set-up whose first check-in failed was kept")
	}
	never.setUp(t, "")
	before, _ := never.setUp(t, "")
	never.fake.WorkersDevNotLiveForRequests = downAlarmFirstCheckInTries
	if after, _ := never.setUp(t, "MMHotline's first check-in with the MMHotline-down alarm failed"); !after.DownAlarm.IsSetUp || after.DownAlarm.WorkerURL != before.DownAlarm.WorkerURL || !after.DownAlarm.SetUpAt.Equal(*before.DownAlarm.SetUpAt) {
		t.Fatalf("the earlier setup did not stay: %+v", after.DownAlarm)
	}
}

// When backup_option is everyone_available, the MMHotline-down
// alarm wakes everyone else available at the backup's delay, as an unclaimed
// SOS escalates. Members reached only as everyone available raise no notice
// when they cannot be woken, as with an SOS.
func TestDownAlarmWakesEveryoneElseAvailableWhenTheBackupIsEveryoneAvailable(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	if _, err := r.a.SaveSchedule(context.Background(), r.barbara, ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: []string{r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID, r.barbaraID}, BackupKind: "everyone_available"}, 1); err != nil {
		t.Fatal(err)
	}
	r.setUp(t, "")
	held := r.lastCheckIn(t)
	if barbara := held["primary-contact-method-"+r.barbaraPushover]; barbara.DelaySeconds != 300 {
		t.Fatalf("Barbara, on call, held %+v", barbara)
	}
	if bob := held["backup-contact-method-"+r.bobPhone]; bob.DelaySeconds != 600 {
		t.Fatalf("Bob, available, held %+v; want the backup's 600 seconds", bob)
	}
	if _, err := r.a.Store.db.Exec(`UPDATE contact_methods SET deleted=1 WHERE id=?`, r.bobPhone); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.advance(time.Minute)
		r.round()
	}
	if alerts := r.alertsTitled(t, "The MMHotline-down alarm cannot wake everyone on call"); len(alerts) != 0 {
		t.Fatalf("a notice for a member reached only as everyone available: %+v", alerts)
	}
}

// When the person on call is unavailable on the default backup option, the
// MMHotline-down alarm wakes everyone available. A person reached only that
// way raises no notice, unless no held message would wake anyone at all.
func TestDownAlarmNamesNobodyReachedOnlyAsEveryoneAvailableWhenThePersonOnCallIsUnavailable(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	ctx := context.Background()
	memberFixture(t, r.a, "carol@example.com") // no waking contact method
	if _, err := r.a.SaveSchedule(ctx, r.barbara, ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: []string{r.bobID, r.bobID, r.bobID, r.bobID, r.bobID, r.bobID, r.bobID}, BackupKind: "everyone_available"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.SetPersonalAvailability(ctx, r.bob, "unavailable", r.a.Now().Add(24*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	r.setUp(t, "")
	if barbara := r.lastCheckIn(t)["primary-contact-method-"+r.barbaraPushover]; barbara.DelaySeconds != 300 {
		t.Fatalf("Barbara, available, held %+v; want the person on call's 300 seconds", barbara)
	}
	for range 3 {
		r.advance(time.Minute)
		r.round()
	}
	if alerts := r.alertsTitled(t, "The MMHotline-down alarm cannot wake everyone on call"); len(alerts) != 0 {
		t.Fatalf("a notice for Carol, reached only as everyone available: %+v", alerts)
	}
	if _, err := r.a.Store.db.Exec(`UPDATE contact_methods SET deleted=1 WHERE id IN (?,?)`, r.barbaraPushover, r.barbaraCall); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.advance(time.Minute)
		r.round()
	}
	alerts := r.alertsTitled(t, "The MMHotline-down alarm cannot wake everyone on call")
	// Barbara and Carol; every fixture person is called Responder.
	if len(alerts) != 1 || strings.Count(alerts[0].Message, "- Responder, on call: ") != 2 {
		t.Fatalf("with no held message, the notice must name everyone the alarm should wake: %+v", alerts)
	}
}

// pushoverEmergencyAt sends Pushover an emergency notification, as the
// MMHotline-down alarm's test request does, and returns the receipt in its
// answer.
func pushoverEmergencyAt(t *testing.T, sendingServicesURL, userKey string) string {
	t.Helper()
	response, err := http.PostForm(sendingServicesURL+"/pushover/1/messages.json", url.Values{"token": {fakesendingservices.PushoverApplicationToken}, "user": {userKey}, "title": {"Test of the MMHotline-down alarm"}, "message": {"Nothing is wrong."}, "priority": {"2"}, "retry": {"30"}, "expire": {"600"}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var answer struct{ Receipt string }
	if err = json.NewDecoder(response.Body).Decode(&answer); err != nil || answer.Receipt == "" {
		t.Fatalf("Pushover's answer: %+v %v", answer, err)
	}
	return answer.Receipt
}

// The test of the MMHotline-down alarm is proved from Pushover's receipt:
// MMHotline reads the receipt, which shows that Pushover accepted the
// notification, and cancels the notification's repeats at once. A voice call
// keeps today's test. Each failure names its part.
func TestDownAlarmTestIsProvedFromPushoversReceiptAndItsRepeatsAreCancelled(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	// The held requests keep Pushover's real https address; the receipt is
	// read, and cancelled, at the fake Pushover.
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServicesURL); err != nil {
		t.Fatal(err)
	}
	code := 200
	pushoverID, callID := "primary-contact-method-"+r.barbaraPushover, "primary-contact-method-"+r.barbaraCall
	test := func(receipt string) (CommandResult, string) {
		r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) {
			w.TestSends = []downalarm.TestSend{{HeldMessageID: pushoverID, SendOutcome: "delivered", HTTPStatusCode: &code, PushoverReceipt: receipt}, {HeldMessageID: callID, SendOutcome: "delivered", HTTPStatusCode: &code}}
		})
		return commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "send-test"}, 200)
	}

	receipt := pushoverEmergencyAt(t, r.sendingServicesURL, fakesendingservices.PushoverUserKey)
	result, raw := test(receipt)
	push, call := result.DownAlarm.TestSends[0], result.DownAlarm.TestSends[1]
	if push.PushoverReceipt != receipt || push.PushoverReceiptOutcome != "accepted_and_repeats_cancelled" || push.PushoverReceiptProblem != "" || DownAlarmTestFailed(result.DownAlarm.TestSends) {
		t.Fatalf("Pushover test: %+v", push)
	}
	if call.PushoverReceipt != "" || call.PushoverReceiptOutcome != "" || strings.Count(raw, `"pushover_receipt_outcome"`) != 1 {
		t.Fatalf("the voice call's test changed: %+v\n%s", call, raw)
	}
	if receipts := r.sendingServices.PushoverReceipts(); len(receipts) != 1 || !receipts[0].Cancelled || len(receipts[0].Polls) != 1 {
		t.Fatalf("Pushover's receipt was not read once and then cancelled: %+v", receipts)
	}
	for _, want := range []string{"Test: Pushover notification to Responder: accepted by the service (HTTP 200). Pushover accepted the MMHotline-down alarm's notification (receipt " + receipt + "). MMHotline cancelled its repeats.", "Test: Automated voice call to Responder: accepted by the service (HTTP 200)."} {
		if !strings.Contains(result.Text(), want) {
			t.Errorf("the text lacks %q:\n%s", want, result.Text())
		}
	}
	if strings.Contains(result.Text(), "Test: Automated voice call to Responder: accepted by the service (HTTP 200). Pushover") {
		t.Fatalf("a voice call's test line mentions Pushover:\n%s", result.Text())
	}

	// No receipt in the answer: nothing proves that Pushover accepted it.
	result, _ = test("")
	if outcome := result.DownAlarm.TestSends[0].PushoverReceiptOutcome; outcome != "no_receipt" || !DownAlarmTestFailed(result.DownAlarm.TestSends) || !strings.Contains(result.Text(), "Pushover's answer had no receipt") {
		t.Fatalf("no receipt: %s\n%s", outcome, result.Text())
	}
	// A receipt Pushover does not know: Pushover refused it.
	result, _ = test("FakePushoverReceiptUnknown0000")
	if outcome := result.DownAlarm.TestSends[0].PushoverReceiptOutcome; outcome != "receipt_refused_by_pushover" || !DownAlarmTestFailed(result.DownAlarm.TestSends) || !strings.Contains(result.DownAlarm.TestSends[0].PushoverReceiptProblem, "Pushover answered HTTP 400") {
		t.Fatalf("an unknown receipt: %+v", result.DownAlarm.TestSends[0])
	}
	// A cancel Pushover refuses: the receipt proves the notification, but
	// the repeats go on.
	refusingCancel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/cancel.json") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		r.sendingServices.ServeHTTP(w, request)
	}))
	defer refusingCancel.Close()
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, refusingCancel.URL); err != nil {
		t.Fatal(err)
	}
	result, _ = test(pushoverEmergencyAt(t, r.sendingServicesURL, fakesendingservices.PushoverUserKey))
	if push = result.DownAlarm.TestSends[0]; push.PushoverReceiptOutcome != "cancel_failed" || !DownAlarmTestFailed(result.DownAlarm.TestSends) || !strings.Contains(result.Text(), "MMHotline could not cancel its repeats") {
		t.Fatalf("a cancel that failed: %+v\n%s", push, result.Text())
	}
}

// Each test of the MMHotline-down alarm calls and notifies everyone on its
// list at once, through the team's Twilio and Pushover accounts. So a member
// may start one at most once every 10 minutes, and 6 in 24 hours. Root's
// command is not limited: root already holds those accounts' credentials.
func TestDownAlarmTestFromThePageHasLimits(t *testing.T) {
	t.Parallel()
	r := newDownAlarmRig(t)
	r.setUp(t, "")
	code := 200
	r.fake.ChangeWorker(downAlarmWorker, func(w *fakecloudflare.Worker) {
		w.TestSends = []downalarm.TestSend{{HeldMessageID: "primary-contact-method-" + r.barbaraCall, SendOutcome: "delivered", HTTPStatusCode: &code}}
	})
	ctx := context.Background()
	if _, err := r.a.SendDownAlarmTest(ctx, r.bob); err != nil {
		t.Fatal(err)
	}
	r.advance(9*time.Minute + 30*time.Second)
	_, err := r.a.SendDownAlarmTest(ctx, r.barbara)
	if want := "Each test calls everyone on the MMHotline-down alarm's list, so a test can start once every 10 minutes. Try again in 1 minute."; !errors.Is(err, ErrRateLimited) || publicError(err) != want {
		t.Fatalf("a second test within 10 minutes: %v", err)
	}
	commandRequest(t, r.a, CommandRequest{Group: "down-alarm", Action: "send-test"}, 200)
	for range 5 {
		r.advance(10 * time.Minute)
		if _, err := r.a.SendDownAlarmTest(ctx, r.bob); err != nil {
			t.Fatal(err)
		}
	}
	r.advance(10 * time.Minute)
	_, err = r.a.SendDownAlarmTest(ctx, r.bob)
	if want := "Each test calls everyone on the MMHotline-down alarm's list, so at most 6 tests can start in 24 hours. Try again in 23 hours."; !errors.Is(err, ErrRateLimited) || publicError(err) != want {
		t.Fatalf("a seventh test in 24 hours: %v", err)
	}
	if sent := r.worker(t).TestsSent; sent != 7 {
		t.Fatalf("tests sent: %d", sent)
	}
	r.advance(24 * time.Hour)
	if _, err := r.a.SendDownAlarmTest(ctx, r.bob); err != nil {
		t.Fatalf("a test a day later: %v", err)
	}
}
