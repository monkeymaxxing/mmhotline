package fakesendingservices

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// The fake Slack speaks the same Web API and outbound Socket Mode protocol.
// https://docs.slack.dev/apis/events-api/using-socket-mode/
const (
	SlackBotToken      = "xoxb-fake-slack-bot-token"
	SlackAppLevelToken = "xapp-fake-slack-app-level-token"
	SlackOtherBotToken = "xoxb-fake-other-workspace-token"
	SlackWorkspaceID   = "T0001"
	SlackAppID         = "AFAKEMMHOTLINE"
	SlackBotUserID     = "UFAKEBOT"
)

// SlackWorkspace is a Slack workspace (a Slack "team").
type SlackWorkspace struct {
	ID, Name string
}

// SlackUser is a person in a Slack workspace.
type SlackUser struct {
	ID, Name  string
	Workspace SlackWorkspace
}

// SlackConversation is a Slack conversation.
type SlackConversation struct {
	ID, Name string
}

// Block Kit objects, with only the fields MMHotline uses.
// Docs: https://api.slack.com/reference/block-kit/blocks
type SlackText struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji"`
}
type SlackButton struct {
	Type     string    `json:"type"`
	Text     SlackText `json:"text"`
	ActionID string    `json:"action_id"`
	Value    string    `json:"value,omitempty"`
	URL      string    `json:"url,omitempty"`
	Style    string    `json:"style,omitempty"`
}
type SlackBlock struct {
	Type     string        `json:"type"`
	BlockID  string        `json:"block_id,omitempty"`
	Text     *SlackText    `json:"text,omitempty"`
	Elements []SlackButton `json:"elements,omitempty"`
}

// SlackMessage is a message the MMHotline app posted, with its current text
// and blocks (chat.update can replace them).
type SlackMessage struct {
	WorkspaceID string       `json:"workspace_id"`
	ChannelID   string       `json:"channel_id"`
	TS          string       `json:"ts"`
	PostedWith  string       `json:"posted_with"` // "chat.postMessage"
	Text        string       `json:"text"`
	Blocks      []SlackBlock `json:"blocks"`
}

// Buttons lists the message's buttons.
func (m SlackMessage) Buttons() []SlackButton {
	var buttons []SlackButton
	for _, b := range m.Blocks {
		if b.Type == "actions" {
			buttons = append(buttons, b.Elements...)
		}
	}
	return buttons
}

// SlackReply is a response_url post that did not replace the message: a reply
// only the person who pressed the button sees.
type SlackReply struct {
	SlackUserID  string `json:"slack_user_id"`
	ResponseType string `json:"response_type"`
	Text         string `json:"text"`
}

// slackMessageProblem checks a message the way Slack does for the fields
// MMHotline uses: section text at most 3,000 characters; a button's text at
// most 75, its value at most 2,000, and exactly one of value or URL.
// Docs: https://api.slack.com/reference/block-kit/block-elements#button
func slackMessageProblem(text string, blocks []SlackBlock) string {
	if strings.TrimSpace(text) == "" && len(blocks) == 0 {
		return "no_text"
	}
	for _, b := range blocks {
		if b.Text != nil && utf8.RuneCountInString(b.Text.Text) > 3000 {
			return "invalid_blocks"
		}
		for _, e := range b.Elements {
			if e.Type != "button" || e.Text.Text == "" || utf8.RuneCountInString(e.Text.Text) > 75 || len(e.ActionID) > 255 || len(e.Value) > 2000 || (e.Value == "") == (e.URL == "") {
				return "invalid_blocks"
			}
		}
	}
	return ""
}

// postSlackMessage must be called with s.mu held.
func (s *Server) postSlackMessage(workspaceID, channelID, postedWith, text string, blocks []SlackBlock) SlackMessage {
	s.slack.nextID++
	message := SlackMessage{WorkspaceID: workspaceID, ChannelID: channelID, TS: fmt.Sprintf("%d.%06d", time.Now().Unix(), s.slack.nextID), PostedWith: postedWith, Text: text, Blocks: blocks}
	s.slack.messages = append(s.slack.messages, message)
	s.record(Sent{SendingService: "slack", IDFromSendingService: workspaceID + "/" + channelID + "/" + message.TS, From: "MMHotline Slack app", To: channelID, Text: text})
	return message
}

type slackEnvelope struct {
	EnvelopeID string `json:"envelope_id,omitempty"`
	Type       string `json:"type"`
	Payload    any    `json:"payload,omitempty"`
}
type slackState struct {
	nextID         int
	baseURL        string
	messages       []SlackMessage
	replies        []SlackReply
	channels       map[string]bool
	envelopes      []slackEnvelope
	acknowledged   map[string]bool
	changed        chan struct{}
	socketURLs     map[string]bool
	opened, active int
	connections    map[*websocket.Conn]bool
}

func newSlackState() slackState {
	return slackState{channels: map[string]bool{}, acknowledged: map[string]bool{}, changed: make(chan struct{}), socketURLs: map[string]bool{}, connections: map[*websocket.Conn]bool{}}
}
func slackRandom() string { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func (s *Server) noteBaseURL(r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	s.slack.baseURL = scheme + "://" + r.Host
}
func (s *Server) registerSlackRoutes() {
	s.mux.HandleFunc("POST /slack/api/{method}", s.slackWebAPI)
	s.mux.HandleFunc("GET /slack/socket/{token}", s.slackSocket)
	s.mux.HandleFunc("POST /fake-api/actions/slack/message", s.slackMessageAction)
	s.mux.HandleFunc("POST /fake-api/actions/slack/press", s.slackPressAction)
}
func slackWebAPIAnswer(w http.ResponseWriter, fields map[string]any) {
	fields["ok"] = true
	writeJSON(w, 200, fields)
}
func slackWebAPIRefuse(w http.ResponseWriter, code string) {
	writeJSON(w, 200, map[string]any{"ok": false, "error": code})
}
func (s *Server) slackWebAPI(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var args map[string]json.RawMessage
	if err != nil || json.Unmarshal(body, &args) != nil {
		slackWebAPIRefuse(w, "invalid_arguments")
		return
	}
	text := func(key string) string { var value string; _ = json.Unmarshal(args[key], &value); return value }
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteBaseURL(r)
	method := r.PathValue("method")
	if method == "apps.connections.open" {
		if token != SlackAppLevelToken {
			slackWebAPIRefuse(w, "invalid_auth")
			return
		}
		key := slackRandom()
		s.slack.socketURLs[key] = true
		address := strings.Replace(s.slack.baseURL, "http", "ws", 1) + "/slack/socket/" + key
		slackWebAPIAnswer(w, map[string]any{"url": address})
		return
	}
	workspace := ""
	switch token {
	case SlackBotToken:
		workspace = SlackWorkspaceID
	case SlackOtherBotToken:
		workspace = "T0002"
	}
	if workspace == "" {
		slackWebAPIRefuse(w, "invalid_auth")
		return
	}
	switch method {
	case "auth.test":
		name := "Acme"
		if workspace != "T0001" {
			name = "Other workspace"
		}
		slackWebAPIAnswer(w, map[string]any{"team_id": workspace, "team": name, "user_id": SlackBotUserID, "bot_id": "BFAKEBOT"})
	case "chat.postMessage", "chat.update", "chat.postEphemeral":
		channel := text("channel")
		if !s.slack.channels[workspace+"/"+channel] {
			slackWebAPIRefuse(w, "channel_not_found")
			return
		}
		var blocks []SlackBlock
		_ = json.Unmarshal(args["blocks"], &blocks)
		if problem := slackMessageProblem(text("text"), blocks); problem != "" {
			slackWebAPIRefuse(w, problem)
			return
		}
		if method == "chat.postEphemeral" {
			if text("user") == "" {
				slackWebAPIRefuse(w, "user_not_found")
				return
			}
			s.slack.replies = append(s.slack.replies, SlackReply{SlackUserID: text("user"), ResponseType: "ephemeral", Text: text("text")})
			slackWebAPIAnswer(w, map[string]any{"message_ts": "ephemeral"})
			return
		}
		if method == "chat.update" {
			for index, message := range s.slack.messages {
				if message.WorkspaceID == workspace && message.ChannelID == channel && message.TS == text("ts") {
					s.slack.messages[index].Text = text("text")
					s.slack.messages[index].Blocks = blocks
					slackWebAPIAnswer(w, map[string]any{"channel": channel, "ts": message.TS})
					return
				}
			}
			slackWebAPIRefuse(w, "message_not_found")
			return
		}
		message := s.postSlackMessage(workspace, channel, method, text("text"), blocks)
		slackWebAPIAnswer(w, map[string]any{"channel": channel, "ts": message.TS})
	default:
		slackWebAPIRefuse(w, "unknown_method")
	}
}

func (s *Server) enqueueSlack(kind string, payload any) string {
	id := slackRandom()
	s.slack.envelopes = append(s.slack.envelopes, slackEnvelope{EnvelopeID: id, Type: kind, Payload: payload})
	close(s.slack.changed)
	s.slack.changed = make(chan struct{})
	return id
}

// SlackSendMessage plays a person's direct message or mention, including the
// normal Slack mention prefix. The app has already been invited to the channel.
func (s *Server) SlackSendMessage(user SlackUser, conversation SlackConversation, text string, mention bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if user.Workspace.ID == "" {
		user.Workspace.ID = SlackWorkspaceID
	}
	kind, channelType := "message", "im"
	if mention {
		kind = "app_mention"
		channelType = "channel"
		text = "<@" + SlackBotUserID + "> " + text
	}
	s.slack.channels[user.Workspace.ID+"/"+conversation.ID] = true
	event := map[string]string{"type": kind, "channel_type": channelType, "user": user.ID, "channel": conversation.ID, "text": text}
	return s.enqueueSlack("events_api", map[string]any{"team_id": user.Workspace.ID, "api_app_id": SlackAppID, "event": event})
}
func (s *Server) SlackPressButton(user SlackUser, message SlackMessage, actionID string) error {
	for _, button := range message.Buttons() {
		if button.ActionID == actionID {
			s.SlackSendButtonPayload(user, message, actionID, button.Value)
			return nil
		}
	}
	return errors.New("the message has no such button")
}

// SlackSendButtonPayload allows a test to put a real button value on another
// message; MMHotline must refuse it, even though Slack delivered the envelope.
func (s *Server) SlackSendButtonPayload(user SlackUser, message SlackMessage, actionID, value string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if user.Workspace.ID == "" {
		user.Workspace.ID = SlackWorkspaceID
	}
	payload := map[string]any{
		"type": "block_actions", "api_app_id": SlackAppID,
		"team":      map[string]string{"id": message.WorkspaceID},
		"user":      map[string]string{"id": user.ID, "team_id": user.Workspace.ID},
		"channel":   map[string]string{"id": message.ChannelID},
		"container": map[string]string{"type": "message", "channel_id": message.ChannelID, "message_ts": message.TS},
		"message":   map[string]any{"text": message.Text, "blocks": message.Blocks, "ts": message.TS},
		"actions":   []map[string]string{{"action_id": actionID, "value": value}},
	}
	return s.enqueueSlack("interactive", payload)
}

// SlackCloseConnections plays a transport closure without a disconnect envelope.
func (s *Server) SlackCloseConnections() {
	s.mu.Lock()
	var connections []*websocket.Conn
	for connection := range s.slack.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		connection.CloseNow()
	}
}

func (s *Server) SlackDisconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueueSlack("disconnect", nil)
}
func (s *Server) SlackAcknowledged(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slack.acknowledged[id]
}
func (s *Server) SlackConnections() (opened, active int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slack.opened, s.slack.active
}
func (s *Server) SlackMessages() []SlackMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]SlackMessage, len(s.slack.messages))
	for index, message := range s.slack.messages {
		data, _ := json.Marshal(message)
		_ = json.Unmarshal(data, &result[index])
	}
	return result
}
func (s *Server) SlackReplies() []SlackReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SlackReply(nil), s.slack.replies...)
}

func (s *Server) slackSocket(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	known := s.slack.socketURLs[r.PathValue("token")]
	delete(s.slack.socketURLs, r.PathValue("token"))
	s.mu.Unlock()
	if !known {
		http.Error(w, "invalid connection address", http.StatusUnauthorized)
		return
	}
	connection, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer connection.CloseNow()
	s.mu.Lock()
	s.slack.opened++
	s.slack.active++
	s.slack.connections[connection] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.slack.active--; delete(s.slack.connections, connection); s.mu.Unlock() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for {
			_, data, err := connection.Read(ctx)
			if err != nil {
				return
			}
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if json.Unmarshal(data, &ack) == nil && ack.EnvelopeID != "" {
				s.mu.Lock()
				s.slack.acknowledged[ack.EnvelopeID] = true
				s.mu.Unlock()
			}
		}
	}()
	defer func() { cancel(); connection.CloseNow(); <-done }()
	hello, _ := json.Marshal(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": SlackAppID}})
	if connection.Write(ctx, websocket.MessageText, hello) != nil {
		return
	}
	cursor := 0
	for {
		s.mu.Lock()
		changed := s.slack.changed
		pending := append([]slackEnvelope(nil), s.slack.envelopes[cursor:]...)
		cursor = len(s.slack.envelopes)
		var outgoing []slackEnvelope
		for _, envelope := range pending {
			if !s.slack.acknowledged[envelope.EnvelopeID] {
				outgoing = append(outgoing, envelope)
				if envelope.Type == "disconnect" {
					s.slack.acknowledged[envelope.EnvelopeID] = true
				}
			}
		}
		s.mu.Unlock()
		for _, envelope := range outgoing {
			data, _ := json.Marshal(envelope)
			if connection.Write(ctx, websocket.MessageText, data) != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}
func (s *Server) slackMessageAction(w http.ResponseWriter, r *http.Request) {
	fields, ok := actionFields(r)
	if !ok || fields["slack_user_id"] == "" || fields["slack_conversation_id"] == "" || fields["slack_message"] == "" {
		http.Error(w, "send slack_user_id, slack_conversation_id, and slack_message", 400)
		return
	}
	user := SlackUser{ID: fields["slack_user_id"], Workspace: SlackWorkspace{ID: SlackWorkspaceID}}
	s.SlackSendMessage(user, SlackConversation{ID: fields["slack_conversation_id"]}, fields["slack_message"], fields["slack_message_kind"] == "mention")
	actionDone(w, r)
}
func (s *Server) slackPressAction(w http.ResponseWriter, r *http.Request) {
	fields, ok := actionFields(r)
	if !ok {
		http.Error(w, "invalid fields", 400)
		return
	}
	for _, message := range s.SlackMessages() {
		if message.ChannelID == fields["slack_channel_id"] && message.TS == fields["message_ts"] {
			err := s.SlackPressButton(SlackUser{ID: fields["slack_user_id"], Workspace: SlackWorkspace{ID: fields["slack_workspace_id"]}}, message, fields["action_id"])
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			actionDone(w, r)
			return
		}
	}
	http.Error(w, "message not found", 404)
}
