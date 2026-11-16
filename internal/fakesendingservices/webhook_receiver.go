package fakesendingservices

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// WebhookRequest is one request a fake webhook receiver got.
type WebhookRequest struct {
	ReceiverName   string    `json:"receiver_name"`
	DeliveryID     string    `json:"mmhotline_delivery_id"`
	Signature      string    `json:"mmhotline_signature"`
	Authorization  string    `json:"authorization,omitempty"`
	Body           string    `json:"body"`
	HTTPStatusCode int       `json:"http_status_code"`
	ReceivedAt     time.Time `json:"received_at"`
}

// webhookAnswer is how a named receiver answers its next requests.
type webhookAnswer struct {
	HTTPStatusCode int  `json:"http_status_code"`
	TimeOut        bool `json:"time_out"`
}

var receiverName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// webhookReceive stands in for a customer's webhook receiver at
// {fake}/webhook-receiver/{name}. It records every request and answers 200
// unless told otherwise.
func (s *Server) webhookReceive(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !receiverName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	answer, ok := s.webhookAnswers[name]
	if !ok {
		answer = webhookAnswer{HTTPStatusCode: 200}
	}
	s.webhookRequests = append(s.webhookRequests, WebhookRequest{ReceiverName: name, DeliveryID: r.Header.Get("Mmhotline-Delivery-Id"), Signature: r.Header.Get("Mmhotline-Signature"), Authorization: r.Header.Get("Authorization"), Body: string(body), HTTPStatusCode: answer.HTTPStatusCode, ReceivedAt: time.Now().UTC()})
	if answer.HTTPStatusCode >= 200 && answer.HTTPStatusCode < 300 && !answer.TimeOut {
		s.record(Sent{SendingService: "webhook", IDFromSendingService: r.Header.Get("Mmhotline-Delivery-Id"), To: "webhook-receiver/" + name, Text: string(body)})
	}
	s.mu.Unlock()
	if answer.TimeOut {
		select {
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
		}
		return
	}
	w.WriteHeader(answer.HTTPStatusCode)
}

// SetWebhookAnswer makes a named receiver answer with this HTTP status code,
// or never answer when timeOut is true.
func (s *Server) SetWebhookAnswer(name string, httpStatusCode int, timeOut bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhookAnswers[name] = webhookAnswer{HTTPStatusCode: httpStatusCode, TimeOut: timeOut}
}

// WebhookRequests returns every request the fake receivers got, oldest first.
func (s *Server) WebhookRequests() []WebhookRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WebhookRequest(nil), s.webhookRequests...)
}

// webhookAnswerAction is POST /fake-api/actions/webhook-receiver/{name}/answer
// with {"http_status_code": 500} or {"time_out": true}.
func (s *Server) webhookAnswerAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var answer webhookAnswer
	if !receiverName.MatchString(name) || json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&answer) != nil || (!answer.TimeOut && (answer.HTTPStatusCode < 100 || answer.HTTPStatusCode > 599)) {
		http.Error(w, "send {\"http_status_code\": 200..599} or {\"time_out\": true}", 400)
		return
	}
	s.SetWebhookAnswer(name, answer.HTTPStatusCode, answer.TimeOut)
	w.WriteHeader(204)
}

// VerifyWebhookSignature checks a Mmhotline-Signature header the way a customer's
// receiver should: HMAC-SHA256 with the hex-decoded secret over the
// timestamp, a dot, and the raw body.
func VerifyWebhookSignature(secretHex, header, body string) bool {
	var timestamp int64
	var signature string
	if n, err := fmt.Sscanf(header, "timestamp_unix_seconds=%d,hmac_sha256_hex=%s", &timestamp, &signature); n != 2 || err != nil {
		return false
	}
	key, err := hex.DecodeString(secretHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%d.%s", timestamp, body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}
