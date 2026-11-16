// Package fakesendingservices imitates the outside sending services and outgoing
// webhook receivers MMHotline contacts, so contact method types can be developed
// and tested without real accounts. Each handler implements only the calls
// MMHotline makes, following the public API documentation cited on the handler.
// Fakes are never evidence that real delivery works.
//
// Go tests use the Server in-process (httptest); local development and the
// Linux test suites run it as cmd/mmhotline-fake-sending-services. Each sending
// service lives under its own path prefix: {fake}/telegram, {fake}/slack,
// {fake}/twilio, and {fake}/pushover. Outgoing webhook receivers live under
// {fake}/webhook-receiver/{name}.
package fakesendingservices

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sync"
	"time"
)

// Sent is one message a sending service accepted.
type Sent struct {
	SendingService       string    `json:"sending_service"`
	IDFromSendingService string    `json:"id_from_sending_service"`
	From                 string    `json:"from"`
	To                   string    `json:"to"`
	Subject              string    `json:"subject,omitempty"`
	Text                 string    `json:"text"`
	SentAt               time.Time `json:"sent_at"`
}

type Server struct {
	mu              sync.Mutex
	sent            []Sent
	webhookAnswers  map[string]webhookAnswer
	webhookRequests []WebhookRequest
	telegram        telegramState
	slack           slackState
	phone           phoneState
	pushover        pushoverState
	nextID          int
	mux             *http.ServeMux
}

func New() *Server {
	s := &Server{webhookAnswers: map[string]webhookAnswer{}, telegram: newTelegramState(), slack: newSlackState(), phone: newPhoneState(), mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /telegram/{bot}/{method}", s.telegramBotAPI)
	s.mux.HandleFunc("GET /telegram/t.me/{bot}", s.telegramLinkStandIn)
	s.mux.HandleFunc("POST /fake-api/actions/telegram/start", s.telegramStartAction)
	s.mux.HandleFunc("POST /fake-api/actions/telegram/press", s.telegramPressAction)
	s.registerSlackRoutes()
	s.mux.HandleFunc("POST /webhook-receiver/{name}", s.webhookReceive)
	s.mux.HandleFunc("POST /fake-api/actions/webhook-receiver/{name}/answer", s.webhookAnswerAction)
	s.registerTwilioRoutes()
	s.registerPushoverRoutes()
	s.registerModelsListRoutes()
	s.mux.HandleFunc("POST /fake-api/actions/phone", s.phoneAction)
	s.mux.HandleFunc("GET /fake-api/sent", s.sentJSON)
	s.mux.HandleFunc("GET /{$}", s.sentPage)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Sent returns everything the fake sending services accepted, oldest first.
func (s *Server) Sent() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sent(nil), s.sent...)
}

func (s *Server) record(m Sent) {
	m.SentAt = time.Now().UTC()
	s.sent = append(s.sent, m)
}

func (s *Server) sentJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Sent []Sent `json:"sent"`
	}{s.Sent()})
}

var sentPageTemplate = template.Must(template.New("sent").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>What MMHotline would have sent</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:2rem}table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:.3rem .6rem;text-align:left;vertical-align:top}pre{margin:0;white-space:pre-wrap}form{display:inline-block;margin:.2rem}</style></head>
<body><h1>What MMHotline would have sent</h1><p>Nothing here reached a real person. These are the fake sending services used for development and tests.</p>
<p>Email is not faked here. Set up the mail server against Mailpit to inspect development email.</p>
<table><thead><tr><th>Sent at (UTC)</th><th>Sending service</th><th>From</th><th>To</th><th>Subject</th><th>Text</th></tr></thead><tbody>
{{range .Sent}}<tr><td>{{.SentAt.Format "2006-01-02 15:04:05"}}</td><td>{{.SendingService}}</td><td>{{.From}}</td><td>{{.To}}</td><td>{{.Subject}}</td><td><pre>{{.Text}}</pre></td></tr>{{else}}<tr><td colspan="6">Nothing sent yet.</td></tr>{{end}}
</tbody></table>
<h2>Telegram messages and their buttons</h2><p>Press a button as the Telegram user with this ID. A user MMHotline does not know gets its "does not know your Telegram account" answer.</p>
<table><thead><tr><th>Chat</th><th>Message</th><th>Text</th><th>Buttons</th></tr></thead><tbody>
{{range .TelegramMessages}}{{$message := .}}<tr><td>{{.Chat.ID}} {{.Chat.Title}}</td><td>{{.MessageID}}</td><td><pre>{{.Text}}</pre></td><td>{{with .ReplyMarkup}}{{range .InlineKeyboard}}{{range .}}{{if .CallbackData}}<form method="post" action="/fake-api/actions/telegram/press"><input type="hidden" name="chat_id" value="{{$message.Chat.ID}}"><input type="hidden" name="message_id" value="{{$message.MessageID}}"><input type="hidden" name="callback_data" value="{{.CallbackData}}"><input name="telegram_user_id" value="1001" size="6" aria-label="Telegram user ID"> <button>{{.Text}}</button></form>{{else}}<a href="{{.URL}}">{{.Text}}</a>{{end}}{{end}}{{end}}{{end}}</td></tr>{{else}}<tr><td colspan="4">No Telegram messages yet.</td></tr>{{end}}
</tbody></table>
<h2>Telegram button answers</h2><ul>{{range .TelegramCallbackAnswers}}<li>{{.Text}}</li>{{else}}<li>None yet.</li>{{end}}</ul>
<h2>Slack connections</h2><p>Set up Slack with bot token <code>xoxb-fake-slack-bot-token</code> and app-level token <code>xapp-fake-slack-app-level-token</code>. Send a connection code as a chosen user.</p>
<form method="post" action="/fake-api/actions/slack/message"><label>Slack user ID <input name="slack_user_id" value="U0001"></label><label>Conversation ID <input name="slack_conversation_id" value="D0001"></label><label>Message kind <select name="slack_message_kind"><option value="direct_message">Direct message</option><option value="mention">Channel mention (use a C conversation ID)</option></select></label><label>Connection code <input name="slack_message"></label><button>Send to MMHotline</button></form>
<h2>Slack messages and their buttons</h2><p>Press as the chosen Slack user. Socket Mode delivers the press over MMHotline's outbound connection.</p>
<table><thead><tr><th>Workspace</th><th>Channel</th><th>Message</th><th>Text</th><th>Buttons</th></tr></thead><tbody>
{{range .SlackMessages}}{{$message := .}}<tr><td>{{.WorkspaceID}}</td><td>{{.ChannelID}}</td><td>{{.TS}} ({{.PostedWith}})</td><td><pre>{{.Text}}</pre></td><td>{{range .Buttons}}{{if .Value}}<form method="post" action="/fake-api/actions/slack/press"><input type="hidden" name="slack_channel_id" value="{{$message.ChannelID}}"><input type="hidden" name="message_ts" value="{{$message.TS}}"><input type="hidden" name="action_id" value="{{.ActionID}}"><input name="slack_user_id" value="U0001" size="8" aria-label="Slack user ID"> <input name="slack_workspace_id" value="{{$message.WorkspaceID}}" size="8" aria-label="Slack workspace ID of the user"> <button>{{.Text.Text}}</button></form>{{else}}<a href="{{.URL}}">{{.Text.Text}}</a>{{end}}{{end}}</td></tr>{{else}}<tr><td colspan="5">No Slack messages yet.</td></tr>{{end}}
</tbody></table>
<h2>Slack replies only the presser saw</h2><ul>{{range .SlackReplies}}<li>{{.SlackUserID}}: {{.Text}}</li>{{else}}<li>None yet.</li>{{end}}</ul>
<h2>Pushover emergency receipts</h2><table><thead><tr><th>User key</th><th>Receipt</th><th>Status</th><th>Action</th></tr></thead><tbody>{{range .PushoverReceipts}}<tr><td>{{.UserKey}}</td><td>{{.Receipt}}</td><td>{{if .Acknowledged}}Acknowledged{{else if .Cancelled}}Cancelled{{else if .Expired}}Expired{{else}}Open{{end}}</td><td>{{if not (or .Acknowledged .Cancelled .Expired)}}<form method="post" action="/fake-api/actions/pushover/acknowledge"><input type="hidden" name="receipt" value="{{.Receipt}}"><button>Acknowledge</button></form>{{end}}</td></tr>{{else}}<tr><td colspan="4">No Pushover emergency messages yet.</td></tr>{{end}}</tbody></table>
<h2>Automated voice calls (Twilio)</h2><table><thead><tr><th>Sending service</th><th>To</th><th>What the call says</th><th>Status</th><th>Play the phone</th></tr></thead><tbody>
{{range .PhoneCalls}}<tr><td>{{.SendingService}}</td><td>{{.To}}</td><td>{{range .Said}}<p>{{.}}</p>{{end}}</td><td>{{.Status}}</td><td>{{$id := .IDFromSendingService}}{{range $outcome, $label := $.CallOutcomes}}<form method="post" action="/fake-api/actions/phone"><input type="hidden" name="action" value="call_answer"><input type="hidden" name="id_from_sending_service" value="{{$id}}"><input type="hidden" name="outcome" value="{{$outcome}}"><button>{{$label}}</button></form>{{end}}</td></tr>{{else}}<tr><td colspan="5">No calls yet.</td></tr>{{end}}
</tbody></table>
<h2>Phone sending services</h2>{{range $sendingService := .SendingServices}}<p><strong>{{$sendingService}}</strong> {{range $action, $label := $.SendingServiceActions}}<form method="post" action="/fake-api/actions/phone"><input type="hidden" name="action" value="next_request"><input type="hidden" name="sending_service" value="{{$sendingService}}"><input type="hidden" name="behavior" value="{{$action}}"><button>{{$label}}</button></form>{{end}}</p>{{end}}
<form method="post" action="/fake-api/actions/phone"><input type="hidden" name="action" value="number_behavior"><label>Phone number <input name="phone_number" placeholder="+15555550123"></label> <select name="behavior"><option value="">Normal phone</option><option value="invalid_number">Invalid number (permanent failure)</option><option value="geographic">Outside the geographic permissions</option></select> <button>Set</button></form>
</body></html>`))

func (s *Server) sentPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	slackMessages, slackReplies := s.SlackMessages(), s.SlackReplies()
	_ = sentPageTemplate.Execute(w, struct {
		Sent                    []Sent
		TelegramMessages        []TelegramMessage
		TelegramCallbackAnswers []TelegramCallbackAnswer
		SlackMessages           []SlackMessage
		SlackReplies            []SlackReply
		PhoneCalls              []PhoneCall
		PushoverReceipts        []PushoverReceipt
		CallOutcomes            map[string]string
		SendingServiceActions   map[string]string
		SendingServices         []string
	}{s.Sent(), s.TelegramMessages(), s.TelegramCallbackAnswers(), slackMessages, slackReplies, s.PhoneCalls(), s.PushoverReceipts(),
		map[string]string{CallAnswered: "Answered", CallNoAnswer: "No answer", CallBusy: "Busy", CallFailed: "Call failed"},
		map[string]string{RefuseNextRequest: "Refuse the next request", LoseNextResponse: "Lose the next response"},
		[]string{sendingServiceTwilio}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
