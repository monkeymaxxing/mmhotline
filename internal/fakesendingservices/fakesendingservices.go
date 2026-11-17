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

func (s *Server) sentPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	slackMessages, slackReplies := s.SlackMessages(), s.SlackReplies()
	_ = sentPage(sentPageData{
		Sent: s.Sent(), TelegramMessages: s.TelegramMessages(), TelegramCallbackAnswers: s.TelegramCallbackAnswers(),
		SlackMessages: slackMessages, SlackReplies: slackReplies, PhoneCalls: s.PhoneCalls(), PushoverReceipts: s.PushoverReceipts(),
		CallOutcomes:          map[string]string{CallAnswered: "Answered", CallNoAnswer: "No answer", CallBusy: "Busy", CallFailed: "Call failed"},
		SendingServiceActions: map[string]string{RefuseNextRequest: "Refuse the next request", LoseNextResponse: "Lose the next response"},
		SendingServices:       []string{sendingServiceTwilio},
	}).Render(r.Context(), w)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
