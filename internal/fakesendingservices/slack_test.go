package fakesendingservices

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func slackCall(t *testing.T, server *httptest.Server, token, method string, body any) map[string]json.RawMessage {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request, _ := http.NewRequest("POST", server.URL+"/slack/api/"+method, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var answer map[string]json.RawMessage
	if json.NewDecoder(response.Body).Decode(&answer) != nil {
		t.Fatal("invalid API response")
	}
	return answer
}
func TestSlackSocketModeProtocol(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, method := range []string{"auth.test", "apps.connections.open"} {
		if answer := slackCall(t, server, "wrong-token", method, map[string]string{}); string(answer["error"]) != `"invalid_auth"` {
			t.Fatalf("wrong token: %s", answer)
		}
	}
	identity := slackCall(t, server, SlackBotToken, "auth.test", map[string]string{})
	if string(identity["team_id"]) != `"T0001"` || string(identity["bot_id"]) != `"BFAKEBOT"` {
		t.Fatalf("identity: %s", identity)
	}
	open := slackCall(t, server, SlackAppLevelToken, "apps.connections.open", map[string]string{})
	var address string
	_ = json.Unmarshal(open["url"], &address)
	connection, _, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	_, hello, err := connection.Read(ctx)
	if err != nil || !strings.Contains(string(hello), SlackAppID) {
		t.Fatalf("hello: %s %v", hello, err)
	}
	code := "test-code"
	id := fake.SlackSendMessage(SlackUser{ID: "U0001"}, SlackConversation{ID: "D0001"}, code, false)
	_, data, err := connection.Read(ctx)
	if err != nil || !strings.Contains(string(data), code) || !strings.Contains(string(data), `"type":"events_api"`) {
		t.Fatalf("message: %s %v", data, err)
	}
	ack, _ := json.Marshal(map[string]string{"envelope_id": id})
	if err = connection.Write(ctx, websocket.MessageText, ack); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !fake.SlackAcknowledged(id) {
		select {
		case <-deadline.C:
			t.Fatal("acknowledgement not recorded")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	sent := slackCall(t, server, SlackBotToken, "chat.postMessage", map[string]any{"channel": "D0001", "text": "SOS", "blocks": []SlackBlock{{Type: "actions", Elements: []SlackButton{{Type: "button", Text: SlackText{Type: "plain_text", Text: "Claim this incident"}, ActionID: "claim_incident", Value: "c.123"}}}}})
	if string(sent["ok"]) != "true" || len(fake.SlackMessages()) != 1 {
		t.Fatalf("post: %s", sent)
	}
	if err = fake.SlackPressButton(SlackUser{ID: "U0001"}, fake.SlackMessages()[0], "claim_incident"); err != nil {
		t.Fatal(err)
	}
	_, data, err = connection.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"type":"interactive"`) || !strings.Contains(string(data), `"value":"c.123"`) {
		t.Fatalf("press: %s %v", data, err)
	}
	message := fake.SlackMessages()[0]
	if answer := slackCall(t, server, SlackBotToken, "chat.update", map[string]any{"channel": message.ChannelID, "ts": message.TS, "text": "Claimed", "blocks": []SlackBlock{}}); string(answer["ok"]) != "true" || fake.SlackMessages()[0].Text != "Claimed" {
		t.Fatal("update failed")
	}
	slackCall(t, server, SlackBotToken, "chat.postEphemeral", map[string]string{"channel": "D0001", "user": "U0001", "text": "You claimed it"})
	if replies := fake.SlackReplies(); len(replies) != 1 || replies[0].SlackUserID != "U0001" {
		t.Fatalf("ephemeral reply: %+v", replies)
	}
}

func TestSlackFakePageAndMessageAction(t *testing.T) {
	server := httptest.NewServer(New())
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "/fake-api/actions/slack/message") || !strings.Contains(string(body), SlackAppLevelToken) {
		t.Fatal("missing connection form or setup instructions")
	}
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err = client.PostForm(server.URL+"/fake-api/actions/slack/message", url.Values{"slack_user_id": {"U0001"}, "slack_conversation_id": {"C0001"}, "slack_message": {"code"}, "slack_message_kind": {"mention"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatalf("action: HTTP %d", response.StatusCode)
	}
}
