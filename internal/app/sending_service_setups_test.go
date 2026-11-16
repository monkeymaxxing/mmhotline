package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

const fakeNumberedTelegramToken = "7000000001:fake_bot_token_abcdefghij"
const fakeRotatedTelegramToken = "7000000001:rotated_bot_token_abcdefg"
const fakeOtherTelegramToken = "7000000002:other_bot_token_abcdefgh"

func TestSendingServiceSetupsCheckStoreAndRestart(t *testing.T) {
	a := foundationApp(t)
	s := memberFixture(t, a, "setup@example.test")
	fake := httptest.NewServer(fakesendingservices.New())
	defer fake.Close()
	ctx := context.Background()
	if err := a.LoadSendingServiceSetups(ctx, nil, fake.URL); err != nil {
		t.Fatal(err)
	}
	if a.configuredContactMethodTypes()["telegram_message"] != nil {
		t.Fatal("fake mode automatically set up Telegram")
	}
	fields := map[string]string{"telegram_bot_token": "7000000001:refused_token_abcdefghij"}
	if _, err := a.SetUpSendingService(ctx, s.Principal, "telegram", fields); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("sending service refusal: %v", err)
	}
	setups, err := a.SendingServiceSetups(ctx, s.Principal)
	if err != nil || savedSendingServiceSetup(t, setups, "telegram").IsSetUp {
		t.Fatalf("refused setup saved: %+v %v", setups, err)
	}
	fields["telegram_bot_token"] = fakeNumberedTelegramToken
	result, err := a.SetUpSendingService(ctx, s.Principal, "telegram", fields)
	if err != nil {
		t.Fatal(err)
	}
	if result.Setup.SetUpBy != "Responder" || result.Setup.Settings["telegram_bot_username"] != fakesendingservices.TelegramBotUsername || !strings.Contains(result.Checked, "@"+fakesendingservices.TelegramBotUsername) {
		t.Fatalf("setup result: %+v", result)
	}
	var publicSettings, secretSettings string
	if err = a.Store.db.QueryRow(`SELECT settings_json,secret_settings_json FROM sending_service_setups WHERE sending_service='telegram'`).Scan(&publicSettings, &secretSettings); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(publicSettings, fakeNumberedTelegramToken) || !strings.Contains(secretSettings, fakeNumberedTelegramToken) {
		t.Fatal("settings not stored separately")
	}
	setups, err = a.SendingServiceSetups(ctx, s.Principal)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(setups)
	if strings.Contains(string(raw), fakeNumberedTelegramToken) || strings.Contains(string(raw), "secret_settings") {
		t.Fatal("list exposed secrets")
	}
	// A failed replacement keeps both the durable and running setup.
	fields["telegram_bot_token"] = "7000000001:refused_token_abcdefghij"
	if _, err = a.SetUpSendingService(ctx, s.Principal, "telegram", fields); err == nil {
		t.Fatal("accepted refused replacement")
	}
	previous := a.configuredContactMethodTypes()["telegram_message"]
	restarted := New(a.Store)
	if err = restarted.LoadSendingServiceSetups(ctx, nil, fake.URL); err != nil {
		t.Fatal(err)
	}
	restored, ok := restarted.configuredContactMethodTypes()["telegram_message"].(*TelegramMessageContactMethodType)
	if !ok || restored.BotToken != fakeNumberedTelegramToken {
		t.Fatal("setup was not restored")
	}
	if a.configuredContactMethodTypes()["telegram_message"] != previous {
		t.Fatal("failed replacement changed running contact method type")
	}
}

func TestSendingServiceSetupContactMethodProtection(t *testing.T) {
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	for _, action := range []string{"remove", "different_bot"} {
		var err error
		if action == "remove" {
			err = r.a.RemoveSendingServiceSetup(ctx, r.s.Principal, "telegram")
		} else {
			_, err = r.a.SetUpSendingService(ctx, r.s.Principal, "telegram", map[string]string{"telegram_bot_token": fakeOtherTelegramToken})
		}
		if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "1 Telegram message contact method uses the sending service setup") || !strings.Contains(err.Error(), "Contact methods") {
			t.Fatalf("%s: %v", action, err)
		}
	}
	result, err := r.a.SetUpSendingService(ctx, r.s.Principal, "telegram", map[string]string{"telegram_bot_token": fakeRotatedTelegramToken})
	if err != nil || !result.Setup.IsSetUp {
		t.Fatalf("same bot token rotation: %v", err)
	}
	// The replacement contact method type actually sends through the newly saved token.
	methods, _ := r.a.ContactMethods(ctx, r.s.Principal)
	r.advance(time.Minute)
	if err = r.a.SendContactMethodTest(ctx, r.s.Principal, methods[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.DispatchOne(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.fake.TelegramMessages()) < 2 {
		t.Fatal("replacement token did not send contact test")
	}
	if err = r.a.RemoveContactMethod(ctx, r.s.Principal, methods[0].ID, methods[0].Revision); err != nil {
		t.Fatal(err)
	}
	if err = r.a.RemoveSendingServiceSetup(ctx, r.s.Principal, "telegram"); err != nil {
		t.Fatal(err)
	}
	if r.a.configuredContactMethodTypes()["telegram_message"] != nil {
		t.Fatal("removed contact method type remains available")
	}
}

func TestSendingServiceSetupAuthorization(t *testing.T) {
	a, admin, members := coverageFixture(t)
	member := Principal{SignInEmailAddress: members[1].SignInEmailAddress}
	for _, p := range []Principal{{}, member} {
		if _, err := a.SendingServiceSetups(context.Background(), p); err == nil {
			t.Fatal("unauthorized list")
		}
		if _, err := a.SetUpSendingService(context.Background(), p, "telegram", map[string]string{"telegram_bot_token": fakeNumberedTelegramToken}); err == nil {
			t.Fatal("unauthorized setup")
		}
		if err := a.RemoveSendingServiceSetup(context.Background(), p, "telegram"); err == nil {
			t.Fatal("unauthorized removal")
		}
	}
	if _, err := a.SendingServiceSetups(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
}

func TestSendingServicesCommandHandler(t *testing.T) {
	a := foundationApp(t)
	fake := httptest.NewServer(fakesendingservices.New())
	defer fake.Close()
	if err := a.LoadSendingServiceSetups(context.Background(), nil, fake.URL); err != nil {
		t.Fatal(err)
	}
	a.WebURL = "https://mmhotline.example.com"
	a.UseSettingsFile(InstalledSettingsFilePath, nil)
	if _, err := a.SetUpSendingService(context.Background(), a.settingsPrincipal(), "telegram", map[string]string{"telegram_bot_token": fakeNumberedTelegramToken}); err != nil {
		t.Fatal(err)
	}
	result, raw := commandRequest(t, a, CommandRequest{Group: "sending-services", Action: "list"}, http.StatusOK)
	expectedTitles := map[string]string{"mail_server": "Mail server", "twilio": "Twilio", "pushover": "Pushover", "slack": "Slack", "telegram": "Telegram"}
	if len(result.SendingServiceSetups) != len(expectedTitles) {
		t.Fatalf("sending services: %+v", result.SendingServiceSetups)
	}
	for _, setup := range result.SendingServiceSetups {
		if title, ok := expectedTitles[setup.SendingService]; !ok || setup.SendingServiceTitle != title {
			t.Fatalf("sending service name or title: %+v", setup)
		}
		delete(expectedTitles, setup.SendingService)
	}
	if strings.Count(result.Text(), "Checked with Telegram") != 1 || !strings.Contains(result.Text(), "set in "+InstalledSettingsFilePath+", in sending_services.") {
		t.Fatalf("list text:\n%s", result.Text())
	}
	assertCommandKeys(t, raw, "sending_service_setups", "sending_service", "sending_service_title", "is_set_up", "settings", "telegram_bot_username", "telegram_bot_id", "set_up_at", "set_up_by", "web_url", "message")
	if strings.Contains(raw, fakeNumberedTelegramToken) || savedSendingServiceSetup(t, result.SendingServiceSetups, "telegram").SetUpBy != "the settings file "+InstalledSettingsFilePath {
		t.Fatal(raw)
	}
	// The settings file sets the sending services up; these commands are gone.
	for _, action := range []string{"set-up", "remove-setup"} {
		commandRequest(t, a, CommandRequest{Group: "sending-services", Action: action}, http.StatusNotFound)
	}
}

func settingsPageText(page string) string { return pageText(strings.ReplaceAll(page, "<wbr>", "")) }

func TestSendingServiceSetupPages(t *testing.T) {
	h := newWorkflowRig(t)
	admin := h.browser()
	admin.addPerson("admin@example.test", "Administrator")
	fake := httptest.NewServer(fakesendingservices.New())
	defer fake.Close()
	if err := h.app.LoadSendingServiceSetups(context.Background(), h.app.configuredContactMethodTypes(), fake.URL); err != nil {
		t.Fatal(err)
	}
	contact := admin.get("/contact-methods", 200)
	if !strings.Contains(contact, `href="/sending-services/telegram"`) || !strings.Contains(contact, `href="/sending-services">Sending services`) {
		t.Fatal("administrator setup links missing")
	}
	h.app.UseSettingsFile(InstalledSettingsFilePath, nil)
	list := admin.get("/sending-services", 200)
	if !strings.Contains(list, "Not set up.") || !strings.Contains(settingsPageText(list), "Set in "+InstalledSettingsFilePath) || strings.Contains(list, "<form") {
		t.Fatal(list)
	}
	// The guided page keeps the how-to at the service and names the file
	// key each value goes in; it has no form.
	guide := admin.get("/sending-services/telegram", 200)
	assertBreadcrumbBeforeTitle(t, guide, "/sending-services", "All sending services")
	if !strings.Contains(settingsPageText(guide), "Not set up") || !strings.Contains(guide, "https://t.me/BotFather") || !strings.Contains(settingsPageText(guide), "sending_services.telegram.bot_token") || !strings.Contains(settingsPageText(guide), "sending_services: telegram: bot_token: <Telegram bot token>") || strings.Contains(guide, "<input") {
		t.Fatal(guide)
	}
	slack := admin.get("/sending-services/slack", 200)
	if !strings.Contains(slack, "Create the Slack app") || !strings.Contains(settingsPageText(slack), "sending_services.slack.bot_token") || !strings.Contains(settingsPageText(slack), "sending_services.slack.app_level_token") {
		t.Fatal(slack)
	}
	if !strings.Contains(settingsPageText(slack), "The link includes the app manifest.") {
		t.Fatal("Slack guide does not explain the manifest link")
	}
	for _, service := range []string{"mail_server", "twilio", "pushover", "slack", "telegram"} {
		page := admin.get("/sending-services/"+service, 200)
		assertBreadcrumbBeforeTitle(t, page, "/sending-services", "All sending services")
	}
	csrf := workflowField(t, admin.get("/contact-methods", 200), "csrf")
	admin.post("/sending-services/telegram", url.Values{"csrf": {csrf}, "telegram_bot_token": {fakeNumberedTelegramToken}}, 405)
	if _, err := h.app.SetUpSendingService(context.Background(), h.app.settingsPrincipal(), "telegram", map[string]string{"telegram_bot_token": fakeNumberedTelegramToken}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/sending-services", "/sending-services/telegram", "/contact-methods"} {
		page := admin.get(path, 200)
		if strings.Contains(page, fakeNumberedTelegramToken) {
			t.Fatal("token in page " + path)
		}
		if strings.HasPrefix(path, "/sending-services") && !strings.Contains(page, "@"+fakesendingservices.TelegramBotUsername) {
			t.Fatal("bot username missing: " + page)
		}
	}
	member, _ := addMember(t, h.app, "Member", RoleMember)
	other := h.browser()
	other.identify(member.SignInEmailAddress)
	page := other.get("/contact-methods", 200)
	if strings.Contains(page, `href="/sending-services">Sending services`) {
		t.Fatal("member administrator navigation")
	}
	other.get("/sending-services", 403)
	other.get("/sending-services/telegram", 403)
	if err := h.app.RemoveSendingServiceSetup(context.Background(), h.app.settingsPrincipal(), "telegram"); err != nil {
		t.Fatal(err)
	}
	page = other.get("/contact-methods", 200)
	if !strings.Contains(page, "Ask an administrator to set up Telegram") {
		t.Fatal(page)
	}
}

func TestSendingServiceSetupStartsAndStopsTelegramPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := fakesendingservices.New()
	polling := make(chan struct{}, 16)
	stopped := make(chan struct{}, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			polling <- struct{}{}
			defer func() {
				if r.Context().Err() != nil {
					stopped <- struct{}{}
				}
			}()
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	a := foundationApp(t)
	s := memberFixture(t, a, "polling@example.test")
	a.WebURL = "https://mmhotline.example.com"
	if err := a.LoadSendingServiceSetups(ctx, nil, server.URL); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); a.RunTelegramPolling(ctx, func(err error) { t.Errorf("poller: %v", err) }) }()
	defer func() { cancel(); <-done }()
	if _, err := a.SetUpSendingService(ctx, s.Principal, "telegram", map[string]string{"telegram_bot_token": fakeNumberedTelegramToken}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polling:
	case <-time.After(3 * time.Second):
		t.Fatal("setup did not start polling")
	}
	link, err := a.StartTelegramConnection(ctx, s.Principal, "personal_chat")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link)
	fake.TelegramStart(barbara, barbaraChat(), parsed.Query().Get("start"))
	var methods []ContactMethod
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		methods, err = a.ContactMethods(ctx, s.Principal)
		if err != nil {
			t.Fatal(err)
		}
		if len(methods) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(methods) != 1 {
		t.Fatal("new setup did not connect the chat without restart")
	}
	if err = a.RemoveContactMethod(ctx, s.Principal, methods[0].ID, methods[0].Revision); err != nil {
		t.Fatal(err)
	}
	// The next long poll is waiting when removal cancels the bot.
	select {
	case <-polling:
	case <-time.After(3 * time.Second):
		t.Fatal("no next long poll")
	}
	if err = a.RemoveSendingServiceSetup(ctx, s.Principal, "telegram"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("removal did not cancel long polling")
	}
	if _, err = a.StartTelegramConnection(ctx, s.Principal, "personal_chat"); err == nil {
		t.Fatal("removed setup still connects")
	}
	if _, err = a.SetUpSendingService(ctx, s.Principal, "telegram", map[string]string{"telegram_bot_token": fakeOtherTelegramToken}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polling:
	case <-time.After(3 * time.Second):
		t.Fatal("second setup did not resume polling")
	}
}

func TestSendingServiceSetupConcurrentReaders(t *testing.T) {
	a := foundationApp(t)
	s := memberFixture(t, a, "concurrent@example.test")
	fake := httptest.NewServer(fakesendingservices.New())
	defer fake.Close()
	ctx := context.Background()
	a.WebURL = "https://mmhotline.example.com"
	if err := a.LoadSendingServiceSetups(ctx, map[string]ContactMethodType{"outgoing_webhook": OutgoingWebhookContactMethodType{}}, fake.URL); err != nil {
		t.Fatal(err)
	}
	handler := a.Handler()
	var readers sync.WaitGroup
	for range 2 {
		readers.Go(func() {
			for range 20 {
				response := httptest.NewRecorder()
				request := httptest.NewRequest("GET", "https://mmhotline.example.com/contact-methods", nil)
				addAccessToken(request, s.Principal.SignInEmailAddress)
				handler.ServeHTTP(response, request)
				if response.Code != 200 {
					t.Errorf("concurrent contact page: %d", response.Code)
					return
				}
				if _, err := a.DispatchOne(ctx); err != nil {
					t.Error(err)
					return
				}
				if _, err := a.readTeamDeadlineFacts(ctx, a.Now()); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 10 {
		if _, err := a.SetUpSendingService(ctx, s.Principal, "telegram", map[string]string{"telegram_bot_token": fakeNumberedTelegramToken}); err != nil {
			t.Error(err)
			break
		}
		if err := a.RemoveSendingServiceSetup(ctx, s.Principal, "telegram"); err != nil {
			t.Error(err)
			break
		}
	}
	readers.Wait()
	if a.configuredContactMethodTypes()["outgoing_webhook"] == nil {
		t.Fatal("setup changes discarded startup-configured webhook")
	}
}

func savedSendingServiceSetup(t *testing.T, setups []SendingServiceSetup, kind string) SendingServiceSetup {
	t.Helper()
	for _, setup := range setups {
		if setup.SendingService == kind {
			return setup
		}
	}
	t.Fatalf("sending service setup missing: %s", kind)
	return SendingServiceSetup{}
}
