package fakesendingservices

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Fake-only credentials, with the real sending service's 30-character alphabet.
const PushoverApplicationToken = "FakePushoverApplicationToken01"

// PushoverReplacementApplicationToken is a second application's token, for
// replacing the application.
const PushoverReplacementApplicationToken = "FakePushoverApplicationToken02"
const PushoverUserKey = "FakePushoverPersonalUserKey001"
const PushoverOtherUserKey = "FakePushoverPersonalUserKey002"
const PushoverThirdUserKey = "FakePushoverPersonalUserKey003"

// PushoverOneDeviceUserKey is a user whose account has one device, phone;
// the other user keys have two, phone and tablet.
const PushoverOneDeviceUserKey = "FakePushoverOneDeviceUserKey01"

// PushoverGroupKey is a delivery-group key: messages to it reach every user
// in the group, and the validate call answers "group": 1 for it.
const PushoverGroupKey = "FakePushoverGroupUserKey000001"
const PushoverMonthlyMessageLimit = 10000

type PushoverMessage struct {
	Sound   string `json:"sound,omitempty"`
	Receipt string `json:"receipt,omitempty"`
	UserKey string `json:"pushover_user_key"`
	// Device is the `device` form field: the one device named, empty when none.
	Device   string `json:"device,omitempty"`
	Title    string `json:"title"`
	Message  string `json:"message"`
	URL      string `json:"url"`
	URLTitle string `json:"url_title"`
	Priority int    `json:"priority"`
	Retry    int    `json:"retry"`
	Expire   int    `json:"expire"`
}
type PushoverReceipt struct {
	Receipt      string `json:"receipt"`
	UserKey      string `json:"pushover_user_key"`
	Acknowledged bool   `json:"acknowledged"`
	// AcknowledgedBy is the user key that acknowledged, when it is not UserKey,
	// as when one user of a group acknowledges for the group.
	AcknowledgedBy string      `json:"acknowledged_by,omitempty"`
	Expired        bool        `json:"expired"`
	Cancelled      bool        `json:"cancelled"`
	ExpiresAt      time.Time   `json:"expires_at"`
	Polls          []time.Time `json:"polls"`
}
type pushoverState struct {
	messages            []PushoverMessage
	receipts            map[string]*PushoverReceipt
	deletedApplications map[string]bool
	now                 func() time.Time
}

func (s *Server) registerPushoverRoutes() {
	s.pushover = pushoverState{receipts: map[string]*PushoverReceipt{}, deletedApplications: map[string]bool{}, now: time.Now}
	s.mux.HandleFunc("GET /pushover/1/apps/limits.json", s.pushoverLimits)
	s.mux.HandleFunc("POST /pushover/1/messages.json", s.pushoverSend)
	s.mux.HandleFunc("POST /pushover/1/users/validate.json", s.pushoverValidateUser)
	s.mux.HandleFunc("GET /pushover/1/receipts/{receipt}", s.pushoverReceipt)
	s.mux.HandleFunc("POST /pushover/1/receipts/{receipt}/cancel.json", s.pushoverCancel)
	s.mux.HandleFunc("POST /fake-api/actions/pushover/acknowledge", s.pushoverAcknowledgeAction)
}
func (s *Server) SetPushoverClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushover.now = now
}
func (s *Server) PushoverMessages() []PushoverMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PushoverMessage(nil), s.pushover.messages...)
}
func (s *Server) PushoverReceipts() []PushoverReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	var receipts []PushoverReceipt
	for _, m := range s.pushover.messages {
		if receipt := s.pushover.receipts[m.Receipt]; receipt != nil {
			copy := *receipt
			copy.Polls = append([]time.Time(nil), receipt.Polls...)
			copy.Expired = copy.Expired || !s.pushover.now().Before(copy.ExpiresAt)
			receipts = append(receipts, copy)
		}
	}
	return receipts
}
func (s *Server) PushoverAcknowledge(receipt string) error {
	return s.PushoverAcknowledgeAs(receipt, "")
}

// PushoverAcknowledgeAs acknowledges as the user with key by, who may differ
// from the key the message went to. An empty by is the key it went to.
func (s *Server) PushoverAcknowledgeAs(receipt, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.pushover.receipts[receipt]
	if r == nil {
		return fmt.Errorf("unknown Pushover receipt")
	}
	if r.Cancelled || r.Expired || !s.pushover.now().Before(r.ExpiresAt) {
		return fmt.Errorf("Pushover receipt is closed")
	}
	r.Acknowledged = true
	r.AcknowledgedBy = by
	return nil
}

// DeletePushoverApplication makes the application's token invalid, as
// deleting the application at Pushover does.
func (s *Server) DeletePushoverApplication(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushover.deletedApplications[token] = true
}
func (s *Server) PushoverExpire(receipt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.pushover.receipts[receipt]
	if r == nil {
		return fmt.Errorf("unknown Pushover receipt")
	}
	r.Expired = true
	return nil
}
func pushoverRefuse(w http.ResponseWriter, code int, text string) {
	writeJSON(w, code, map[string]any{"status": 0, "errors": []string{text}})
}
func (s *Server) pushoverToken(w http.ResponseWriter, r *http.Request) bool {
	token := r.FormValue("token")
	s.mu.Lock()
	deleted := s.pushover.deletedApplications[token]
	s.mu.Unlock()
	if (token != PushoverApplicationToken && token != PushoverReplacementApplicationToken) || deleted {
		pushoverRefuse(w, 400, "application token is invalid")
		return false
	}
	return true
}

// https://pushover.net/api#limits: GET apps/limits.json validates the token.
func (s *Server) pushoverLimits(w http.ResponseWriter, r *http.Request) {
	if !s.pushoverToken(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"status": 1, "limit": PushoverMonthlyMessageLimit, "remaining": PushoverMonthlyMessageLimit, "reset": time.Now().Add(30 * 24 * time.Hour).Unix()})
}

// https://pushover.net/api/validate: POST users/validate.json answers
// "group": 1 for a delivery-group key and "group": 0 for a user key.
func (s *Server) pushoverValidateUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		pushoverRefuse(w, 400, "invalid form")
		return
	}
	if !s.pushoverToken(w, r) {
		return
	}
	switch r.Form.Get("user") {
	case PushoverUserKey, PushoverOtherUserKey, PushoverThirdUserKey:
		writeJSON(w, 200, map[string]any{"status": 1, "group": 0, "devices": []string{"phone", "tablet"}, "request": "fake-pushover-validate"})
	case PushoverOneDeviceUserKey:
		writeJSON(w, 200, map[string]any{"status": 1, "group": 0, "devices": []string{"phone"}, "request": "fake-pushover-validate"})
	case PushoverGroupKey:
		writeJSON(w, 200, map[string]any{"status": 1, "group": 1, "devices": []string{}, "request": "fake-pushover-validate"})
	default:
		pushoverRefuse(w, 400, "user identifier is invalid")
	}
}

// https://pushover.net/api: form fields, priorities, lengths, retry and expire.
func (s *Server) pushoverSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := r.ParseForm(); err != nil {
		pushoverRefuse(w, 400, "invalid form")
		return
	}
	if !s.pushoverToken(w, r) {
		return
	}
	key := r.Form.Get("user")
	if key != PushoverUserKey && key != PushoverOtherUserKey && key != PushoverThirdUserKey && key != PushoverGroupKey && key != PushoverOneDeviceUserKey {
		pushoverRefuse(w, 400, "user identifier is invalid")
		return
	}
	priority, err := strconv.Atoi(r.Form.Get("priority"))
	if err != nil || priority < -2 || priority > 2 {
		pushoverRefuse(w, 400, "priority is invalid")
		return
	}
	m := PushoverMessage{Sound: r.Form.Get("sound"), UserKey: key, Device: r.Form.Get("device"), Title: r.Form.Get("title"), Message: r.Form.Get("message"), URL: r.Form.Get("url"), URLTitle: r.Form.Get("url_title"), Priority: priority}
	if m.Message == "" || !utf8.ValidString(m.Message) || utf8.RuneCountInString(m.Message) > 1024 || utf8.RuneCountInString(m.Title) > 250 || utf8.RuneCountInString(m.URL) > 512 || utf8.RuneCountInString(m.URLTitle) > 100 {
		pushoverRefuse(w, 400, "message, title, or URL exceeds its limit")
		return
	}
	if priority == 2 {
		m.Retry, _ = strconv.Atoi(r.Form.Get("retry"))
		m.Expire, _ = strconv.Atoi(r.Form.Get("expire"))
		if m.Retry < 30 || m.Expire <= 0 || m.Expire > 10800 {
			pushoverRefuse(w, 400, "retry or expire is invalid")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	request := fmt.Sprintf("fake-pushover-request-%d", s.nextID)
	answer := map[string]any{"status": 1, "request": request}
	if priority == 2 {
		m.Receipt = fmt.Sprintf("FakePushoverReceipt%011d", s.nextID)
		s.pushover.receipts[m.Receipt] = &PushoverReceipt{Receipt: m.Receipt, UserKey: key, ExpiresAt: s.pushover.now().Add(time.Duration(m.Expire) * time.Second)}
		answer["receipt"] = m.Receipt
	}
	s.pushover.messages = append(s.pushover.messages, m)
	idFromSendingService := m.Receipt
	if idFromSendingService == "" {
		idFromSendingService = request
	}
	s.record(Sent{SendingService: "pushover", IDFromSendingService: idFromSendingService, From: "MMHotline", To: key, Subject: m.Title, Text: m.Message})
	writeJSON(w, 200, answer)
}

// https://pushover.net/api/receipts: never ask faster than every five seconds.
func (s *Server) pushoverReceipt(w http.ResponseWriter, r *http.Request) {
	if !s.pushoverToken(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt := s.pushover.receipts[strings.TrimSuffix(r.PathValue("receipt"), ".json")]
	if receipt == nil {
		pushoverRefuse(w, 400, "receipt is invalid")
		return
	}
	now := s.pushover.now()
	if len(receipt.Polls) > 0 && now.Sub(receipt.Polls[len(receipt.Polls)-1]) < 5*time.Second {
		pushoverRefuse(w, 429, "receipt requested faster than every 5 seconds")
		return
	}
	receipt.Polls = append(receipt.Polls, now)
	acknowledged, expired := 0, 0
	if receipt.Acknowledged {
		acknowledged = 1
	}
	if receipt.Expired || receipt.Cancelled || !now.Before(receipt.ExpiresAt) {
		expired = 1
	}
	by := ""
	if receipt.Acknowledged {
		by = receipt.UserKey
		if receipt.AcknowledgedBy != "" {
			by = receipt.AcknowledgedBy
		}
	}
	writeJSON(w, 200, map[string]any{"status": 1, "acknowledged": acknowledged, "acknowledged_by": by, "expired": expired, "expires_at": receipt.ExpiresAt.Unix()})
}

// https://pushover.net/api/receipts: cancel is a POST with the token.
func (s *Server) pushoverCancel(w http.ResponseWriter, r *http.Request) {
	if !s.pushoverToken(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt := s.pushover.receipts[r.PathValue("receipt")]
	if receipt == nil {
		pushoverRefuse(w, 400, "receipt is invalid")
		return
	}
	receipt.Cancelled = true
	writeJSON(w, 200, map[string]any{"status": 1})
}
func (s *Server) pushoverAcknowledgeAction(w http.ResponseWriter, r *http.Request) {
	if err := s.PushoverAcknowledge(r.FormValue("receipt")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
