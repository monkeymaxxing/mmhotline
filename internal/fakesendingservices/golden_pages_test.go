package fakesendingservices

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/htmlgolden"
)

// goldenPages lists every page the fake sending services render, with the
// states the golden test covers. A state the test cannot reach says why.
var goldenPages = []struct {
	render, page string
	states       []string
	notCovered   map[string]string
}{
	{render: "fakesendingservices.go sentPage (GET /)", page: "sent", states: []string{"empty", "with_messages"}},
	{render: "telegram.go telegramLinkStandIn (GET /telegram/t.me/{bot})", page: "telegram_link", states: []string{"start", "startgroup"}},
}

// Slack message timestamps and the sent time come from the real clock.
var (
	slackTimestamp = regexp.MustCompile(`\b\d{10}\.\d{6}\b`)
	sentAt         = regexp.MustCompile(`\b\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\b`)
)

// Each fake page renders as its golden file says, apart from whitespace
// (see package htmlgolden).
func TestGoldenPages(t *testing.T) {
	fake := New()
	call := func(method, path, contentType, body string, header http.Header) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		for key, values := range header {
			r.Header[key] = values
		}
		w := httptest.NewRecorder()
		fake.ServeHTTP(w, r)
		if w.Code >= 400 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
	}
	rendered := map[string]bool{}
	check := func(name, path string) {
		t.Helper()
		w := httptest.NewRecorder()
		fake.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		page := htmlgolden.Placeholders(w.Body.String(), slackTimestamp, "SLACK-TS")
		page = sentAt.ReplaceAllString(page, "SENT-AT")
		htmlgolden.Check(t, htmlgolden.Dir("fakesendingservices"), name, []byte(page))
		rendered[name] = true
	}
	check("sent__empty", "/")
	check("telegram_link__start", "/telegram/t.me/"+TelegramBotUsername+"?start=start-token")
	check("telegram_link__startgroup", "/telegram/t.me/"+TelegramBotUsername+"?startgroup=group-token")

	call("POST", "/fake-api/actions/telegram/start", "application/json", `{"telegram_user_id": 1001, "start_token": "group-token", "telegram_group_chat_id": -1001, "telegram_group_title": "Ops alerts"}`, nil)
	call("POST", "/telegram/botfake-token/sendMessage", "application/json", `{"chat_id":-1001,"text":"SOS: Database down\nSince 12:00","reply_markup":{"inline_keyboard":[[{"text":"Claim this incident","callback_data":"c:1"},{"text":"Open MMHotline","url":"https://mmhotline.example.com/incidents/one?from=telegram&x=1"}]]}}`, nil)
	messages := fake.TelegramMessages()
	query := fake.TelegramPressButton(TelegramUser{ID: 1001, FirstName: "Barbara"}, -1001, messages[len(messages)-1].MessageID, "c:1")
	call("POST", "/telegram/botfake-token/answerCallbackQuery", "application/json", fmt.Sprintf(`{"callback_query_id":%q,"text":"You claimed the incident."}`, query), nil)
	fake.SlackSendMessage(SlackUser{ID: "U0001"}, SlackConversation{ID: "D0001"}, "connection code", false)
	blocks, _ := json.Marshal(map[string]any{"channel": "D0001", "text": "SOS: Database down", "blocks": []SlackBlock{{Type: "actions", Elements: []SlackButton{
		{Type: "button", Text: SlackText{Type: "plain_text", Text: "Claim this incident"}, ActionID: "claim_incident", Value: "c.123"},
		{Type: "button", Text: SlackText{Type: "plain_text", Text: "Open MMHotline"}, ActionID: "open", URL: "https://mmhotline.example.com/incidents/one?from=slack&x=1"},
	}}}})
	call("POST", "/slack/api/chat.postMessage", "application/json", string(blocks), http.Header{"Authorization": {"Bearer " + SlackBotToken}})
	call("POST", "/slack/api/chat.postEphemeral", "application/json", `{"channel":"D0001","user":"U0001","text":"Only you can see this."}`, http.Header{"Authorization": {"Bearer " + SlackBotToken}})
	fake.SetPhoneClock(func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) })
	twilio := httptest.NewRequest("POST", "/twilio/2010-04-01/Accounts/"+TwilioAccountSID+"/Calls.json", strings.NewReader(url.Values{"To": {"+15555550123"}, "From": {TwilioPhoneNumberToCallFrom}, "Twiml": {`<Response><Say>An SOS incident needs someone now.</Say><Hangup/></Response>`}, "Timeout": {"30"}, "TimeLimit": {"120"}}.Encode()))
	twilio.SetBasicAuth(TwilioAPIKeySID, TwilioAPIKeySecret)
	twilio.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := httptest.NewRecorder(); func() bool { fake.ServeHTTP(w, twilio); return w.Code != 201 }() {
		t.Fatal("the fake did not accept the call")
	}
	call("POST", "/pushover/1/messages.json", "application/x-www-form-urlencoded", url.Values{"token": {PushoverApplicationToken}, "user": {PushoverUserKey}, "priority": {"2"}, "retry": {"60"}, "expire": {"3600"}, "title": {"SOS"}, "message": {"Database down"}}.Encode(), nil)
	check("sent__with_messages", "/")

	for _, page := range goldenPages {
		for _, state := range page.states {
			if !rendered[page.page+"__"+state] {
				t.Errorf("%s: the state %s is listed as covered but not rendered", page.render, state)
			}
		}
	}
}
