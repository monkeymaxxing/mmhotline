package fakesendingservices

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type telegramReply struct {
	OK          bool            `json:"ok"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func telegramCall(t *testing.T, server *httptest.Server, method, body string) telegramReply {
	t.Helper()
	response, err := http.Post(server.URL+"/telegram/botfake-token/"+method, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var reply telegramReply
	if err = json.NewDecoder(response.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.OK != (response.StatusCode == 200) || (!reply.OK && reply.ErrorCode != response.StatusCode) {
		t.Fatalf("%s: HTTP %d with %+v", method, response.StatusCode, reply)
	}
	return reply
}

func TestTelegramBotAPI(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()
	var me TelegramUser
	if reply := telegramCall(t, server, "getMe", `{}`); json.Unmarshal(reply.Result, &me) != nil || me.Username != TelegramBotUsername || !me.IsBot {
		t.Fatalf("getMe: %+v", reply)
	}
	if reply := telegramCall(t, server, "sendMessage", `{"chat_id":1001,"text":"hello"}`); reply.Description != "Bad Request: chat not found" {
		t.Fatalf("unknown chat: %+v", reply)
	}
	fake.TelegramStart(TelegramUser{ID: 1001, FirstName: "Barbara"}, TelegramChat{ID: 1001, Type: "private"}, "token-1")
	fake.TelegramStart(TelegramUser{ID: 1001, FirstName: "Barbara"}, TelegramChat{ID: -5, Type: "group", Title: "Ops"}, "token-2")
	var updates []TelegramUpdate
	if json.Unmarshal(telegramCall(t, server, "getUpdates", `{"offset":0,"timeout":0}`).Result, &updates) != nil || len(updates) != 2 || updates[0].Message.Text != "/start token-1" || updates[1].Message.Text != "/start@"+TelegramBotUsername+" token-2" {
		t.Fatalf("updates: %+v", updates)
	}
	// An offset confirms the updates below it; the rest come again.
	if json.Unmarshal(telegramCall(t, server, "getUpdates", `{"offset":`+jsonNumber(updates[1].UpdateID)+`}`).Result, &updates) != nil || len(updates) != 1 {
		t.Fatalf("after the offset: %+v", updates)
	}
	if reply := telegramCall(t, server, "sendMessage", `{"chat_id":1001,"text":"hello","reply_markup":{"inline_keyboard":[[{"text":"Claim","callback_data":"`+strings.Repeat("x", 65)+`"}]]}}`); reply.Description != "Bad Request: BUTTON_DATA_INVALID" {
		t.Fatalf("long callback_data: %+v", reply)
	}
	var sent TelegramMessage
	if json.Unmarshal(telegramCall(t, server, "sendMessage", `{"chat_id":1001,"text":"SOS","reply_markup":{"inline_keyboard":[[{"text":"Claim","callback_data":"c:1"}]]}}`).Result, &sent) != nil || sent.MessageID == 0 || sent.Chat.ID != 1001 {
		t.Fatalf("sent: %+v", sent)
	}
	edit := `{"chat_id":1001,"message_id":` + jsonNumber(sent.MessageID) + `,"reply_markup":{"inline_keyboard":[[{"text":"Claimed by Barbara","url":"https://mmhotline.example.com/incidents/1"}]]}}`
	if reply := telegramCall(t, server, "editMessageReplyMarkup", edit); !reply.OK {
		t.Fatalf("edit: %+v", reply)
	}
	if reply := telegramCall(t, server, "editMessageReplyMarkup", edit); !strings.HasPrefix(reply.Description, "Bad Request: message is not modified") {
		t.Fatalf("repeated edit: %+v", reply)
	}
	if messages := fake.TelegramMessages(); len(messages) != 1 || messages[0].ReplyMarkup.InlineKeyboard[0][0].Text != "Claimed by Barbara" {
		t.Fatalf("messages: %+v", messages)
	}
	query := fake.TelegramPressButton(TelegramUser{ID: 1001}, 1001, sent.MessageID, "c:1")
	answer := `{"callback_query_id":"` + query + `","text":"Claimed by Barbara"}`
	if reply := telegramCall(t, server, "answerCallbackQuery", answer); !reply.OK {
		t.Fatalf("answer: %+v", reply)
	}
	if reply := telegramCall(t, server, "answerCallbackQuery", answer); reply.OK {
		t.Fatal("a query was answered twice")
	}
	if answers := fake.TelegramCallbackAnswers(); len(answers) != 1 || answers[0].Text != "Claimed by Barbara" {
		t.Fatalf("answers: %+v", answers)
	}
	if reply := telegramCall(t, server, "deleteWebhook", `{}`); reply.ErrorCode != 404 {
		t.Fatalf("unknown method: %+v", reply)
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestTelegramLongPollWakesWhenAnUpdateArrives(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		fake.TelegramStart(TelegramUser{ID: 7}, TelegramChat{ID: 7, Type: "private"}, "late")
	}()
	started := time.Now()
	var updates []TelegramUpdate
	if json.Unmarshal(telegramCall(t, server, "getUpdates", `{"timeout":20}`).Result, &updates) != nil || len(updates) != 1 || time.Since(started) > 5*time.Second {
		t.Fatalf("long poll: %+v after %s", updates, time.Since(started))
	}
}

func TestTelegramActionsAndPage(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.PostForm(server.URL+"/fake-api/actions/telegram/start", url.Values{"telegram_user_id": {"1001"}, "start_token": {"abc"}, "telegram_group_chat_id": {"-1001"}, "telegram_group_title": {"Ops alerts"}})
	if err != nil || response.StatusCode != 303 {
		t.Fatalf("start from the page: %v %v", response, err)
	}
	response, err = http.Post(server.URL+"/fake-api/actions/telegram/start", "application/json", strings.NewReader(`{"telegram_user_id": 1001, "start_token": "def"}`))
	if err != nil || response.StatusCode != 204 {
		t.Fatalf("start from a suite: %v %v", response, err)
	}
	telegramCall(t, server, "sendMessage", `{"chat_id":-1001,"text":"SOS: Database down","reply_markup":{"inline_keyboard":[[{"text":"Claim this incident","callback_data":"c:1"}]]}}`)
	response, err = http.Post(server.URL+"/fake-api/actions/telegram/press", "application/json", strings.NewReader(`{"telegram_user_id": 1001, "chat_id": -1001, "message_id": 3, "callback_data": "c:1"}`))
	if err != nil || response.StatusCode != 204 {
		t.Fatalf("press: %v %v", response, err)
	}
	var updates []TelegramUpdate
	json.Unmarshal(telegramCall(t, server, "getUpdates", `{}`).Result, &updates)
	if len(updates) != 3 || updates[0].Message.Chat.Title != "Ops alerts" || updates[1].Message.Chat.ID != 1001 || updates[2].CallbackQuery.Message.Text != "SOS: Database down" {
		t.Fatalf("updates: %+v", updates)
	}
	for path, want := range map[string]string{"/": "Claim this incident", "/telegram/t.me/" + TelegramBotUsername + "?startgroup=abc": "Add the bot to this group"} {
		response, err = http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s lacks %q: %s", path, want, body)
		}
	}
}
