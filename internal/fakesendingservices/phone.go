package fakesendingservices

import (
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// The fake Twilio accepts calls and exposes their status for polling.
const TwilioPhoneNumberToCallFrom = "+12025550102"

// PhoneCall is one call a fake phone sending service made.
type PhoneCall struct {
	PolledAt             []time.Time `json:"polled_at,omitempty"`
	SendingService       string      `json:"sending_service"`
	IDFromSendingService string      `json:"id_from_sending_service"`
	From                 string      `json:"from"`
	To                   string      `json:"to"`
	Markup               string      `json:"markup"`
	Said                 []string    `json:"said"`
	RingSeconds          int         `json:"ring_seconds"`
	TimeLimitSeconds     int         `json:"time_limit_seconds"`
	MachineDetection     string      `json:"machine_detection,omitempty"`
	Status               string      `json:"status"`
	CreatedAt            time.Time   `json:"created_at"`
}

// Number behaviors a test sets for a recipient number.
const (
	// NumberInvalid refuses calls to an invalid number (Twilio 21211).
	NumberInvalid = "invalid_number"
	// NumberOutsideGeographicPermissions refuses a number in a country the
	// account may not reach (Twilio 21215).
	NumberOutsideGeographicPermissions = "geographic"
)

// Actions on a sending service's next request.
const (
	RefuseNextRequest = "refuse_next_request"
	LoseNextResponse  = "lose_next_response"
)

type phoneState struct {
	calls           []PhoneCall
	numberBehaviors map[string]string
	// nextActions holds actions for the next request.
	nextActions map[string]map[string]bool
	// requestsPerSecond is each sending service's enforced rate limit
	// ("twilio/call"); nextFree is when the next request is allowed.
	requestsPerSecond map[string]float64
	nextFree          map[string]time.Time
	// connection overrides fragment, the other form Twilio might use.
	now    func() time.Time
	nextID int
}

func newPhoneState() phoneState {
	return phoneState{
		numberBehaviors:   map[string]string{},
		nextActions:       map[string]map[string]bool{sendingServiceTwilio: {}},
		requestsPerSecond: map[string]float64{"twilio/call": 1},
		nextFree:          map[string]time.Time{},
		now:               time.Now,
	}
}

const (
	sendingServiceTwilio = "twilio"
)

// SetPhoneClock makes the fake phone sending services use a test's clock for rate
// limits and call timestamps.
func (s *Server) SetPhoneClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phone.now = now
}

// SetNumberBehavior makes a recipient number behave as NumberInvalid, or NumberOutsideGeographicPermissions; "" is a normal
// phone.
func (s *Server) SetNumberBehavior(number, behavior string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if behavior == "" {
		delete(s.phone.numberBehaviors, number)
		return
	}
	s.phone.numberBehaviors[number] = behavior
}

// SetNextSendingServiceAction arms a request refusal or lost response.
func (s *Server) SetNextSendingServiceAction(sendingService, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phone.nextActions[sendingService] != nil {
		s.phone.nextActions[sendingService][action] = true
	}
}

// SetSendingServiceRateLimit changes Twilio's enforced call requests per second.
func (s *Server) SetSendingServiceRateLimit(sendingService, kind string, perSecond float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phone.requestsPerSecond[sendingService+"/"+kind] = perSecond
}

// PhoneCalls returns every call the fake sending services made, oldest first.
func (s *Server) PhoneCalls() []PhoneCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PhoneCall(nil), s.phone.calls...)
}

// takeAction consumes one armed action. Must be called with s.mu held.
func (s *Server) takeAction(sendingService, action string) bool {
	if s.phone.nextActions[sendingService][action] {
		delete(s.phone.nextActions[sendingService], action)
		return true
	}
	return false
}

// allowRequest enforces the sending service's rate limit. Must be called with s.mu
// held.
func (s *Server) allowRequest(sendingService, kind string) bool {
	key := sendingService + "/" + kind
	now := s.phone.now()
	if s.phone.nextFree[key].After(now) {
		return false
	}
	s.phone.nextFree[key] = now.Add(time.Duration(float64(time.Second) / s.phone.requestsPerSecond[key]))
	return true
}

// newPhoneID makes a sending service-style ID. Must be called with s.mu held.
func (s *Server) newPhoneID(prefix string) string {
	s.phone.nextID++
	return fmt.Sprintf("%s%032x", prefix, s.phone.nextID)
}

// loseResponse closes the connection without an answer, as a request whose
// response was lost: the sending service may have acted on it.
func loseResponse(w http.ResponseWriter) {
	if hijacker, ok := w.(http.Hijacker); ok {
		if conn, _, err := hijacker.Hijack(); err == nil {
			conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

var sayPattern = regexp.MustCompile(`<Say>(.*?)</Say>`)

func parseMarkup(markup string) (said []string) {
	for _, m := range sayPattern.FindAllStringSubmatch(markup, -1) {
		said = append(said, html.UnescapeString(m[1]))
	}
	return said
}

const (
	CallAnswered  = "answered"
	CallVoicemail = "voicemail"
	CallNoAnswer  = "no_answer"
	CallBusy      = "busy"
	CallFailed    = "call_failed"
)

// AnswerCall changes the resource fetched by MMHotline; it sends no request.
func (s *Server) AnswerCall(idFromSendingService, outcome string) (string, error) {
	status := ""
	switch outcome {
	case CallAnswered, CallVoicemail:
		status = "completed"
	case CallNoAnswer:
		status = "no-answer"
	case CallBusy:
		status = "busy"
	case CallFailed:
		status = "failed"
	default:
		return "", fmt.Errorf("unknown call outcome %q", outcome)
	}
	return "", s.ReportCallStatus(idFromSendingService, status)
}

func (s *Server) ReportCallStatus(idFromSendingService, status string) error {
	if _, err := s.findCall(idFromSendingService); err != nil {
		return err
	}
	s.setCallStatus(idFromSendingService, status)
	return nil
}

func (s *Server) findCall(idFromSendingService string) (PhoneCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.phone.calls {
		if c.IDFromSendingService == idFromSendingService {
			return c, nil
		}
	}
	return PhoneCall{}, fmt.Errorf("no call %s", idFromSendingService)
}

func (s *Server) setCallStatus(idFromSendingService, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.phone.calls {
		if s.phone.calls[i].IDFromSendingService == idFromSendingService {
			s.phone.calls[i].Status = status
		}
	}
}

// phoneAction changes fake call outcomes or request behavior.
func (s *Server) phoneAction(w http.ResponseWriter, r *http.Request) {
	fields, ok := actionFields(r)
	if !ok {
		http.Error(w, "send JSON or a form", 400)
		return
	}
	var err error
	switch fields["action"] {
	case "call_answer":
		_, err = s.AnswerCall(fields["id_from_sending_service"], fields["outcome"])
	case "number_behavior":
		s.SetNumberBehavior(fields["phone_number"], fields["behavior"])
	case "next_request":
		s.SetNextSendingServiceAction(fields["sending_service"], fields["behavior"])
	case "rate_limit":
		perSecond, e := strconv.ParseFloat(fields["requests_per_second"], 64)
		if e != nil || perSecond <= 0 {
			err = fmt.Errorf("requests_per_second must be a positive number")
		} else {
			s.SetSendingServiceRateLimit(fields["sending_service"], fields["kind"], perSecond)
		}
	default:
		err = fmt.Errorf("unknown action %q", fields["action"])
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	actionDone(w, r)
}
