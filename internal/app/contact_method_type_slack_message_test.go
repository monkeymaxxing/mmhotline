package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"mmhotline/internal/fakesendingservices"
)

type slackRig struct {
	a       *App
	member  personFixture
	fake    *fakesendingservices.Server
	server  *httptest.Server
	advance func(time.Duration)
}

func newSlackRig(t *testing.T, setup bool) *slackRig {
	t.Helper()
	fake := fakesendingservices.New()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	application := foundationApp(t)
	advance := deliveryTestClock(application)
	application.WebURL = "https://mmhotline.example.com"
	member := memberFixture(t, application, "slack@example.test")
	if err := application.LoadSendingServiceSetups(context.Background(), nil, server.URL); err != nil {
		t.Fatal(err)
	}
	rig := &slackRig{application, member, fake, server, advance}
	if setup {
		rig.setUp(t)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); application.RunSlackSocketMode(ctx, func(err error) { t.Log(err) }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Slack connection did not stop")
		}
	})
	if setup {
		slackWait(t, "Socket Mode connection", func() bool { _, active := fake.SlackConnections(); return active == 1 })
	}
	return rig
}
func slackWait(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		case <-tick.C:
		}
	}
}
func (r *slackRig) setUp(t *testing.T) {
	t.Helper()
	_, err := r.a.SetUpSendingService(context.Background(), r.member.Principal, "slack", slackSetupFields())
	if err != nil {
		t.Fatal(err)
	}
}
func slackSetupFields() map[string]string {
	return map[string]string{"slack_bot_token": fakesendingservices.SlackBotToken, "slack_app_level_token": fakesendingservices.SlackAppLevelToken}
}
func (r *slackRig) code(t *testing.T, p Principal, kind string) SlackConnection {
	t.Helper()
	connection, err := r.a.StartSlackConnection(context.Background(), p, kind)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}
func (r *slackRig) sendCode(t *testing.T, code, user, conversation string, mention bool) {
	t.Helper()
	before := len(r.fake.SlackMessages())
	id := r.fake.SlackSendMessage(fakesendingservices.SlackUser{ID: user}, fakesendingservices.SlackConversation{ID: conversation}, code, mention)
	slackWait(t, "acknowledgement and connection answer", func() bool { return r.fake.SlackAcknowledged(id) && len(r.fake.SlackMessages()) > before })
}
func (r *slackRig) methods(t *testing.T) []ContactMethod {
	t.Helper()
	methods, err := r.a.ContactMethods(context.Background(), r.member.Principal)
	if err != nil {
		t.Fatal(err)
	}
	return methods
}
func (r *slackRig) connect(t *testing.T, kind, user, conversation string) ContactMethod {
	t.Helper()
	connection := r.code(t, r.member.Principal, kind)
	r.sendCode(t, connection.Code, user, conversation, kind == "shared_channel")
	methods := r.methods(t)
	if len(methods) == 0 {
		t.Fatal("no connected contact method")
	}
	for _, method := range methods {
		if strings.Contains(method.Settings, conversation) {
			return method
		}
	}
	t.Fatal("connected conversation not found")
	return ContactMethod{}
}
func (r *slackRig) dispatch(t *testing.T) {
	t.Helper()
	if _, err := r.a.DispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (r *slackRig) sos(t *testing.T) (Alert, fakesendingservices.SlackMessage) {
	t.Helper()
	alert, err := r.a.ManualSOS(context.Background(), r.member.Principal, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	messages := r.fake.SlackMessages()
	for index := len(messages) - 1; index >= 0; index-- {
		if len(messages[index].Buttons()) == 2 {
			return alert, messages[index]
		}
	}
	t.Fatal("SOS message has no Claim and Resolve buttons")
	return alert, fakesendingservices.SlackMessage{}
}

func TestSlackSetupCheckSecretsAndReplacement(t *testing.T) {
	r := newSlackRig(t, false)
	ctx := context.Background()
	for _, field := range []string{"slack_bot_token", "slack_app_level_token"} {
		fields := slackSetupFields()
		fields[field] += "-wrong"
		if _, err := r.a.SetUpSendingService(ctx, r.member.Principal, "slack", fields); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "invalid_auth") {
			t.Fatalf("%s wrong token: %v", field, err)
		}
	}
	r.setUp(t)
	setups, err := r.a.SendingServiceSetups(ctx, r.member.Principal)
	if err != nil {
		t.Fatal(err)
	}
	setup := savedSendingServiceSetup(t, setups, "slack")
	expected := map[string]any{"slack_workspace_id": fakesendingservices.SlackWorkspaceID, "slack_workspace_name": "Acme", "slack_bot_user_id": fakesendingservices.SlackBotUserID, "slack_app_id": fakesendingservices.SlackAppID}
	if !reflect.DeepEqual(setup.Settings, expected) {
		t.Fatalf("public identity: %#v", setup.Settings)
	}
	raw, _ := json.Marshal(setups)
	if strings.Contains(string(raw), "xoxb-") || strings.Contains(string(raw), "xapp-") {
		t.Fatal("public setup leaks tokens")
	}
	var secrets string
	if err = r.a.Store.db.QueryRow(`SELECT secret_settings_json FROM sending_service_setups WHERE sending_service='slack'`).Scan(&secrets); err != nil || !strings.Contains(secrets, fakesendingservices.SlackBotToken) {
		t.Fatalf("stored tokens: %v", err)
	}
	restarted := New(r.a.Store)
	if err = restarted.LoadSendingServiceSetups(ctx, nil, r.server.URL); err != nil {
		t.Fatal(err)
	}
	if restarted.configuredContactMethodTypes()["slack_message"].(*SlackMessageContactMethodType).BotToken != fakesendingservices.SlackBotToken {
		t.Fatal("restart lost setup")
	}
	slackWait(t, "set-up starts Socket Mode", func() bool { _, active := r.fake.SlackConnections(); return active == 1 })
	method := r.connect(t, "personal_direct_message", "U0001", "D0001")
	changed := slackSetupFields()
	changed["slack_bot_token"] = fakesendingservices.SlackOtherBotToken
	if _, err = r.a.SetUpSendingService(ctx, r.member.Principal, "slack", changed); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "1 Slack message contact method uses the sending service setup") {
		t.Fatalf("workspace replacement: %v", err)
	}
	if err = r.a.RemoveSendingServiceSetup(ctx, r.member.Principal, "slack"); !errors.Is(err, ErrConflict) {
		t.Fatalf("in-use removal: %v", err)
	}
	r.setUp(t)
	updated, err := r.a.SaveContactMethod(ctx, r.member.Principal, ContactMethodInput{ID: method.ID, ContactMethodType: "slack_message", ExpectedRevision: method.Revision, Subscriptions: []string{"sos"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.a.RemoveContactMethod(ctx, r.member.Principal, updated.ID, updated.Revision); err != nil {
		t.Fatal(err)
	}
	if err = r.a.RemoveSendingServiceSetup(ctx, r.member.Principal, "slack"); err != nil {
		t.Fatal(err)
	}
	slackWait(t, "removal stops Socket Mode", func() bool { _, active := r.fake.SlackConnections(); return active == 0 })
}

func TestSlackConnectsPersonalAndSharedConversations(t *testing.T) {
	r := newSlackRig(t, true)
	ctx := context.Background()
	connection := r.code(t, r.member.Principal, "personal_direct_message")
	parsed, _ := url.Parse(connection.AppURL)
	if parsed.Host != "slack.com" || parsed.Path != "/app_redirect" || parsed.Query().Get("app") != fakesendingservices.SlackAppID || parsed.Query().Get("team") != fakesendingservices.SlackWorkspaceID {
		t.Fatalf("app link: %s", connection.AppURL)
	}
	var hash string
	if err := r.a.Store.db.QueryRow(`SELECT code_hash FROM slack_connection_codes`).Scan(&hash); err != nil || hash != tokenHash(connection.Code) || hash == connection.Code {
		t.Fatalf("stored code hash: %v", err)
	}
	r.sendCode(t, connection.Code, "U0001", "D0001", false)
	personal := r.methods(t)[0]
	member, _ := r.a.CurrentMember(ctx, r.member.Principal)
	if personal.MemberID != member.ID || strings.Join(personal.Subscriptions, ",") != "info,sos,warn" || !personal.SendWhileUnavailableOrMuted {
		t.Fatalf("personal method: %+v", personal)
	}
	var secret, linked string
	if err := r.a.Store.db.QueryRow(`SELECT secret FROM contact_methods WHERE id=?`, personal.ID).Scan(&secret); err != nil || secret != "" {
		t.Fatalf("method has a token: %v %q", err, secret)
	}
	if err := r.a.Store.db.QueryRow(`SELECT member_id FROM slack_user_links WHERE slack_user_id='U0001'`).Scan(&linked); err != nil || linked != member.ID {
		t.Fatalf("linked member: %v", err)
	}
	deliveries := deliveriesOf(t, r.a, personal.ID)
	if len(deliveries) != 1 || deliveries[0].Purpose != "contact_method_test" {
		t.Fatal("connection did not queue a test")
	}
	shared := r.connect(t, "shared_channel", "U0001", "C0001")
	if shared.MemberID != "" || shared.Setting("slack_conversation_kind") != "channel" {
		t.Fatalf("shared method: %+v", shared)
	}
	// A fresh code for an existing conversation does not duplicate its contact method.
	r.connect(t, "shared_channel", "U0001", "C0001")
	if len(r.methods(t)) != 2 {
		t.Fatal("duplicate shared contact")
	}
	ordinary, _ := addMember(t, r.a, "Ordinary", RoleMember)
	if _, err := r.a.StartSlackConnection(ctx, ordinary, "shared_channel"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-administrator shared code: %v", err)
	}
}

func TestSlackConnectionCodeBoundaries(t *testing.T) {
	r := newSlackRig(t, true)
	personal := r.code(t, r.member.Principal, "personal_direct_message")
	r.sendCode(t, personal.Code, "U0001", "C0001", true)
	if len(r.methods(t)) != 0 {
		t.Fatal("personal code accepted in Slack channel")
	}
	r.sendCode(t, personal.Code, "U0001", "D0001", false)
	r.sendCode(t, personal.Code, "U0002", "D0001", false)
	if len(r.methods(t)) != 1 {
		t.Fatal("reused code created a contact")
	}
	var linked int
	if err := r.a.Store.db.QueryRow(`SELECT count(*) FROM slack_user_links WHERE slack_user_id='U0002'`).Scan(&linked); err != nil || linked != 0 {
		t.Fatal("second Slack user linked through a used code")
	}
	shared := r.code(t, r.member.Principal, "shared_channel")
	r.sendCode(t, shared.Code, "U0001", "D0001", false)
	if len(r.methods(t)) != 1 {
		t.Fatal("Slack channel code accepted in direct messages")
	}
	r.sendCode(t, shared.Code, "U0001", "C0001", true)
	r.sendCode(t, shared.Code, "U0001", "C0001", true)
	if len(r.methods(t)) != 2 {
		t.Fatal("shared code reuse created another contact")
	}
	expired := r.code(t, r.member.Principal, "personal_direct_message")
	r.advance(15 * time.Minute)
	r.sendCode(t, expired.Code, "U0003", "D0003", false)
	r.sendCode(t, strings.Repeat("A", 43), "U0004", "D0004", false)
	if len(r.methods(t)) != 2 {
		t.Fatal("expired or unknown code accepted")
	}
	for index := 0; index < 5; index++ {
		r.code(t, r.member.Principal, "personal_direct_message")
	}
	if _, err := r.a.StartSlackConnection(context.Background(), r.member.Principal, "personal_direct_message"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("unused code limit: %v", err)
	}
}

func TestSlackSOSButtonsAuthorizeAndMatchTheirMessage(t *testing.T) {
	r := newSlackRig(t, true)
	r.connect(t, "personal_direct_message", "U0001", "D0001")
	r.dispatch(t)
	alert, message := r.sos(t)
	if message.PostedWith != "chat.postMessage" || message.Buttons()[0].Text.Text != "Claim this incident (assign it to me)" || message.Buttons()[1].Text.Text != "Resolve" {
		t.Fatalf("SOS: %+v", message)
	}
	deliveries := deliveriesOf(t, r.a, r.methods(t)[0].ID)
	found := false
	for _, delivery := range deliveries {
		if delivery.IncidentID == alert.IncidentID {
			found = delivery.IDFromSendingService == slackIDFromSendingService(message.WorkspaceID, message.ChannelID, message.TS)
		}
	}
	if !found {
		t.Fatal("delivery did not record conversation and message timestamp")
	}
	repliesBefore := len(r.fake.SlackReplies())
	if err := r.fake.SlackPressButton(fakesendingservices.SlackUser{ID: "UUNKNOWN"}, message, slackClaimActionID); err != nil {
		t.Fatal(err)
	}
	slackWait(t, "unknown user reply", func() bool { return len(r.fake.SlackReplies()) > repliesBefore })
	reply := r.fake.SlackReplies()[repliesBefore]
	if reply.ResponseType != "ephemeral" || !strings.Contains(reply.Text, "/contact-methods") || !strings.Contains(reply.Text, "/incidents/"+alert.IncidentID) {
		t.Fatalf("unknown user reply: %+v", reply)
	}
	wrongMessage := message
	wrongMessage.TS = "0.000001"
	before := len(r.fake.SlackReplies())
	id := r.fake.SlackSendButtonPayload(fakesendingservices.SlackUser{ID: "U0001"}, wrongMessage, slackClaimActionID, message.Buttons()[0].Value)
	slackWait(t, "foreign message refusal", func() bool { return r.fake.SlackAcknowledged(id) && len(r.fake.SlackReplies()) > before })
	incident, err := r.a.GetIncident(context.Background(), r.member.Principal, alert.IncidentID)
	if err != nil || incident.Status != "open" {
		t.Fatalf("foreign message changed incident: %+v %v", incident, err)
	}
	if err = r.fake.SlackPressButton(fakesendingservices.SlackUser{ID: "U0001"}, message, slackClaimActionID); err != nil {
		t.Fatal(err)
	}
	postedMessage := func() fakesendingservices.SlackMessage {
		for _, posted := range r.fake.SlackMessages() {
			if posted.TS == message.TS {
				return posted
			}
		}
		t.Fatal("the pressed message is gone")
		return fakesendingservices.SlackMessage{}
	}
	slackWait(t, "claim updates message", func() bool {
		buttons := postedMessage().Buttons()
		return len(buttons) == 2 && strings.Contains(buttons[0].Text.Text, "Claimed by") && buttons[1].ActionID == slackResolveActionID
	})
	incident, err = r.a.GetIncident(context.Background(), r.member.Principal, alert.IncidentID)
	if err != nil || incident.Status != "claimed" {
		t.Fatalf("claim: %+v %v", incident, err)
	}
	// The Resolve button that stayed after the claim resolves the incident.
	if err = r.fake.SlackPressButton(fakesendingservices.SlackUser{ID: "U0001"}, postedMessage(), slackResolveActionID); err != nil {
		t.Fatal(err)
	}
	slackWait(t, "resolve updates incident and message", func() bool {
		incident, err := r.a.GetIncident(context.Background(), r.member.Principal, alert.IncidentID)
		buttons := postedMessage().Buttons()
		return err == nil && incident.Status == "resolved" && len(buttons) == 1 && strings.HasPrefix(buttons[0].Text.Text, "Resolved by ")
	})
}

func TestSlackSocketModeFollowsSetupAndReconnects(t *testing.T) {
	r := newSlackRig(t, false)
	if opened, active := r.fake.SlackConnections(); opened != 0 || active != 0 {
		t.Fatal("connected without setup")
	}
	if _, err := r.a.StartSlackConnection(context.Background(), r.member.Principal, "personal_direct_message"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("connection without setup: %v", err)
	}
	r.setUp(t)
	slackWait(t, "setup starts connection", func() bool { _, active := r.fake.SlackConnections(); return active == 1 })
	r.fake.SlackDisconnect()
	slackWait(t, "disconnect reconnect", func() bool { opened, active := r.fake.SlackConnections(); return opened >= 2 && active == 1 })
	r.fake.SlackCloseConnections()
	slackWait(t, "closed transport reconnect", func() bool { opened, active := r.fake.SlackConnections(); return opened >= 3 && active == 1 })
	if err := r.a.RemoveSendingServiceSetup(context.Background(), r.member.Principal, "slack"); err != nil {
		t.Fatal(err)
	}
	slackWait(t, "setup removal stops connection", func() bool { _, active := r.fake.SlackConnections(); return active == 0 })
	r.setUp(t)
	slackWait(t, "setup starts connection again", func() bool { opened, active := r.fake.SlackConnections(); return opened >= 4 && active == 1 })
}

type slackBlockingTransport struct {
	base             http.RoundTripper
	entered, release chan struct{}
	once             sync.Once
	failFirst        bool
}

func (transport *slackBlockingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/chat.postMessage") {
		first := false
		transport.once.Do(func() { first = true; close(transport.entered) })
		select {
		case <-transport.release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		if first && transport.failFirst {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"service_unavailable"}`)), Header: http.Header{}}, nil
		}
	}
	return transport.base.RoundTrip(request)
}

func TestSlackAcknowledgesWhileSendingServiceReplyWaits(t *testing.T) {
	r := newSlackRig(t, true)
	// Publish a replacement instance instead of mutating the running one.
	current := r.a.configuredContactMethodTypes()["slack_message"].(*SlackMessageContactMethodType)
	replacement := *current
	blocker := &slackBlockingTransport{base: http.DefaultTransport, entered: make(chan struct{}), release: make(chan struct{})}
	replacement.Client = &http.Client{Transport: blocker}
	contactMethodTypes := r.a.configuredContactMethodTypes()
	copyContactMethodTypes := map[string]ContactMethodType{}
	for kind, contactMethodType := range contactMethodTypes {
		copyContactMethodTypes[kind] = contactMethodType
	}
	copyContactMethodTypes["slack_message"] = &replacement
	r.a.publishContactMethodTypes(copyContactMethodTypes)
	defer close(blocker.release)
	code := r.code(t, r.member.Principal, "personal_direct_message")
	first := r.fake.SlackSendMessage(fakesendingservices.SlackUser{ID: "U0001"}, fakesendingservices.SlackConversation{ID: "D0001"}, code.Code, false)
	select {
	case <-blocker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sending service reply was not reached")
	}
	second := r.fake.SlackSendMessage(fakesendingservices.SlackUser{ID: "U0002"}, fakesendingservices.SlackConversation{ID: "D0002"}, "unrelated message", false)
	slackWait(t, "both acknowledgements before reply", func() bool { return r.fake.SlackAcknowledged(first) && r.fake.SlackAcknowledged(second) })
}

func TestSlackManifestAndCommandJSON(t *testing.T) {
	parsed, err := url.Parse(SlackManifestLink())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "api.slack.com" || parsed.Query().Get("new_app") != "1" {
		t.Fatalf("manifest link: %s", parsed)
	}
	var manifest struct {
		Display struct {
			Name string `json:"name"`
		} `json:"display_information"`
		Features struct {
			Bot struct {
				Name string `json:"display_name"`
			} `json:"bot_user"`
			Home struct {
				Messages bool `json:"messages_tab_enabled"`
				ReadOnly bool `json:"messages_tab_read_only_enabled"`
			} `json:"app_home"`
		} `json:"features"`
		Config struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Socket        bool `json:"socket_mode_enabled"`
			Rotation      bool `json:"token_rotation_enabled"`
			Interactivity struct {
				Enabled bool `json:"is_enabled"`
			} `json:"interactivity"`
			Events struct {
				Bot []string `json:"bot_events"`
			} `json:"event_subscriptions"`
		} `json:"settings"`
	}
	if err = json.Unmarshal([]byte(parsed.Query().Get("manifest_json")), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Display.Name != "MMHotline" || manifest.Features.Bot.Name != "MMHotline" || !manifest.Features.Home.Messages || manifest.Features.Home.ReadOnly || !manifest.Settings.Socket || manifest.Settings.Rotation || !manifest.Settings.Interactivity.Enabled || !reflect.DeepEqual(manifest.Settings.Events.Bot, []string{"message.im", "app_mention"}) {
		t.Fatalf("manifest: %+v", manifest)
	}
	if !reflect.DeepEqual(manifest.Config.Scopes.Bot, []string{"chat:write", "im:history", "app_mentions:read"}) {
		t.Fatalf("manifest scopes: %+v", manifest.Config.Scopes.Bot)
	}
	r := newSlackRig(t, false)
	if _, err := r.a.SetUpSendingService(context.Background(), Principal{UnixUser: "fixture"}, "slack", slackSetupFields()); err != nil {
		t.Fatal(err)
	}
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "sending-services", Action: "list"}, 200)
	assertCommandKeys(t, raw, "sending_service_setups", "slack_workspace_id", "slack_workspace_name", "slack_bot_user_id", "slack_app_id")
	if !strings.Contains(result.Text(), "Acme") || strings.Contains(raw, "xoxb-") || strings.Contains(raw, "xapp-") {
		t.Fatal(raw)
	}
}

func TestSlackConnectionPages(t *testing.T) {
	r := newSlackRig(t, true)
	server := httptest.NewTLSServer(r.a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, r.member.Principal.SignInEmailAddress)
	response, err := client.PostForm(server.URL+"/connect-slack", url.Values{"slack_connection": {"personal_direct_message"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("missing CSRF accepted: HTTP %d", response.StatusCode)
	}
	csrf := pageToken(t, client, server.URL, "/contact-methods")
	response, err = client.PostForm(server.URL+"/connect-slack", url.Values{"csrf": {csrf}, "slack_connection": {"personal_direct_message"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), "slack.com/app_redirect") || !strings.Contains(string(body), "15 minutes") {
		t.Fatalf("connection page: HTTP %d %s", response.StatusCode, body)
	}
	response, err = client.Get(server.URL + "/sending-services/slack")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), "manifest_json=") || !strings.Contains(settingsPageText(string(body)), "sending_services.slack.app_level_token") || strings.Contains(string(body), fakesendingservices.SlackBotToken) {
		t.Fatalf("setup page: HTTP %d %s", response.StatusCode, body)
	}
}

func TestSlackSendingKeepsTextEscapingAndResults(t *testing.T) {
	for _, test := range []struct {
		code          int
		answer, state string
		retry         bool
	}{
		{200, `{"ok":true,"ts":"123.456","channel":"D0001"}`, "accepted", false},
		{429, `{"ok":false,"error":"ratelimited"}`, "failed", true},
		{503, `{}`, "uncertain", true},
		{200, `{"ok":false,"error":"internal_error"}`, "uncertain", true},
		{200, `{"ok":false,"error":"channel_not_found"}`, "failed", false},
		{200, `not json`, "uncertain", true},
	} {
		t.Run(test.state+test.answer, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer xoxb-test" {
					t.Error("missing setup token")
				}
				var message slackMessage
				if json.NewDecoder(r.Body).Decode(&message) != nil {
					t.Error("invalid message")
				}
				if !strings.Contains(message.Text, "&lt;@channel&gt;") || !strings.HasSuffix(message.Text, "https://example.com/incident") || utf8.RuneCountInString(message.Blocks[0].Text.Text) > 3000 {
					t.Error("text escaping, truncation, or link preservation failed")
				}
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, test.answer)
			}))
			defer server.Close()
			slack := &SlackMessageContactMethodType{BotToken: "xoxb-test", WorkspaceID: "T0001", BaseURL: server.URL}
			alertText := "<@channel> " + strings.Repeat("界", 4000)
			result := slack.Send(context.Background(), ContactMethodSend{Settings: `{"slack_workspace_id":"T0001","slack_conversation_id":"D0001","slack_conversation_kind":"direct_message"}`, Text: "SOS: Database down\n\n" + alertText + "\n\nhttps://example.com/incident", AlertMessage: alertText})
			if result.State != test.state || result.Retryable != test.retry {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

func TestSlackFailedReplyKeepsAcknowledgedConnections(t *testing.T) {
	r := newSlackRig(t, true)
	current := r.a.configuredContactMethodTypes()["slack_message"].(*SlackMessageContactMethodType)
	replacement := *current
	blocker := &slackBlockingTransport{base: http.DefaultTransport, entered: make(chan struct{}), release: make(chan struct{}), failFirst: true}
	replacement.Client = &http.Client{Transport: blocker}
	r.a.publishContactMethodTypes(map[string]ContactMethodType{"slack_message": &replacement})
	personal := r.code(t, r.member.Principal, "personal_direct_message")
	shared := r.code(t, r.member.Principal, "shared_channel")
	first := r.fake.SlackSendMessage(fakesendingservices.SlackUser{ID: "U0001"}, fakesendingservices.SlackConversation{ID: "D0001"}, personal.Code, false)
	select {
	case <-blocker.entered:
	case <-time.After(5 * time.Second):
		close(blocker.release)
		t.Fatal("reply not reached")
	}
	second := r.fake.SlackSendMessage(fakesendingservices.SlackUser{ID: "U0001"}, fakesendingservices.SlackConversation{ID: "C0001"}, shared.Code, true)
	slackWait(t, "queued envelope acknowledgements", func() bool { return r.fake.SlackAcknowledged(first) && r.fake.SlackAcknowledged(second) })
	close(blocker.release)
	slackWait(t, "second code handled after failed reply", func() bool { return len(r.methods(t)) == 2 && len(r.fake.SlackMessages()) == 1 })
}

func TestSlackCombinedMessagesHaveNoButtons(t *testing.T) {
	r := newSlackRig(t, true)
	r.connect(t, "shared_channel", "U0001", "C0001")
	r.dispatch(t)
	for _, title := range []string{"first warning", "second warning", "third warning"} {
		queueDelivery(t, r.a, r.member.Principal, title, "warn")
	}
	for range 3 {
		r.dispatch(t)
	}
	messages := r.fake.SlackMessages()
	message := messages[len(messages)-1]
	if !strings.HasPrefix(message.Text, "MMHotline combined 3 alerts") || len(message.Buttons()) != 0 {
		t.Fatalf("combined message: %+v", message)
	}
}

func TestFitChatMessageShortensOnlyTheAlertMessage(t *testing.T) {
	fits := func(s string) bool { return utf8.RuneCountInString(s) <= 40 }
	// The message "down" also appears in the title; only the message after
	// the blank line is shortened.
	text := "SOS: DB down\n\ndown down down down down\n\nhttps://x/1"
	if got := fitChatMessage(text, "down down down down down", fits); got != "SOS: DB down\n\ndown down do…\n\nhttps://x/1" {
		t.Fatalf("%q", got)
	}
	if got := fitChatMessage("short\n\nmessage", "message", fits); got != "short\n\nmessage" {
		t.Fatalf("a text that fits changed: %q", got)
	}
}
