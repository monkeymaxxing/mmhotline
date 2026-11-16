package fakesendingservices

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The fake Twilio lives at {fake}/twilio/2010-04-01/Accounts/{AccountSid}/.
// It implements calls with inline TwiML and Twilio's call refusals.
// Docs: https://www.twilio.com/docs/usage/api

func (s *Server) registerTwilioRoutes() {
	s.mux.HandleFunc("POST /twilio/2010-04-01/Accounts/{sid}/Calls.json", s.twilioCreateCall)
	s.mux.HandleFunc("GET /twilio/2010-04-01/Accounts/{sid}/Calls/{call}", s.twilioFetchCall)
	s.mux.HandleFunc("GET /twilio/2010-04-01/Accounts/{sid}/IncomingPhoneNumbers.json", s.twilioNumbers)
}

// twilioRefuse writes Twilio's error body.
// Docs: https://www.twilio.com/docs/usage/twilios-response#response-formats-exceptions
func twilioRefuse(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message, "more_info": fmt.Sprintf("https://www.twilio.com/docs/errors/%d", code), "status": status})
}

const TwilioAccountSID = "AC00000000000000000000000000000001"
const TwilioAPIKeySID = "SK00000000000000000000000000000001"
const TwilioAPIKeySecret = "fake-twilio-api-key-secret"

func twilioRequest(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	key, secret, ok := r.BasicAuth()
	if !ok || key != TwilioAPIKeySID || secret != TwilioAPIKeySecret || r.PathValue("sid") != TwilioAccountSID {
		twilioRefuse(w, 401, 20003, "Authenticate")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	form, parseErr := url.ParseQuery(string(body))
	if err != nil || parseErr != nil {
		twilioRefuse(w, 400, 21601, "Could not read the request")
		return nil, false
	}
	return form, true
}

// twilioCreateCall imitates creating a Call resource with inline TwiML.
// Docs: https://www.twilio.com/docs/voice/api/call-resource#create-a-call-resource
func (s *Server) twilioCreateCall(w http.ResponseWriter, r *http.Request) {
	form, ok := twilioRequest(w, r)
	if !ok {
		return
	}
	to, twiml := form.Get("To"), form.Get("Twiml")
	if to == "" || form.Get("From") == "" || twiml == "" {
		twilioRefuse(w, 400, 21205, "'To', 'From', and 'Twiml' are required")
		return
	}
	ring, _ := strconv.Atoi(form.Get("Timeout"))
	limit, _ := strconv.Atoi(form.Get("TimeLimit"))
	s.mu.Lock()
	switch {
	case s.takeAction(sendingServiceTwilio, RefuseNextRequest):
		s.mu.Unlock()
		twilioRefuse(w, 500, 20500, "Internal Server Error")
		return
	case !s.allowRequest(sendingServiceTwilio, "call"):
		s.mu.Unlock()
		twilioRefuse(w, 429, 20429, "Too Many Requests")
		return
	}
	switch s.phone.numberBehaviors[to] {
	case NumberInvalid:
		s.mu.Unlock()
		twilioRefuse(w, 400, 21211, "Invalid 'To' Phone Number")
		return
	case NumberOutsideGeographicPermissions:
		s.mu.Unlock()
		twilioRefuse(w, 400, 21215, "Account not authorized to call this number: geographic permissions")
		return
	}
	id := s.newPhoneID("CA")
	said := parseMarkup(twiml)
	call := PhoneCall{SendingService: sendingServiceTwilio, IDFromSendingService: id, From: form.Get("From"), To: to, Markup: twiml, Said: said, RingSeconds: ring, TimeLimitSeconds: limit, MachineDetection: form.Get("MachineDetection"), Status: "queued", CreatedAt: s.phone.now().UTC()}
	s.phone.calls = append(s.phone.calls, call)
	s.record(Sent{SendingService: sendingServiceTwilio, IDFromSendingService: id, From: call.From, To: to, Subject: "Automated voice call", Text: strings.Join(said, "\n")})
	lose := s.takeAction(sendingServiceTwilio, LoseNextResponse)
	s.mu.Unlock()
	if lose {
		loseResponse(w)
		return
	}
	writeJSON(w, 201, map[string]any{"sid": id, "status": "queued", "to": to, "from": call.From})
}

// twilioFetchCall implements the documented Call resource GET.
func (s *Server) twilioFetchCall(w http.ResponseWriter, r *http.Request) {
	if _, ok := twilioRequest(w, r); !ok {
		return
	}
	call, err := s.findCall(strings.TrimSuffix(r.PathValue("call"), ".json"))
	if err != nil {
		twilioRefuse(w, 404, 20404, "Call not found")
		return
	}
	s.mu.Lock()
	for i := range s.phone.calls {
		if s.phone.calls[i].IDFromSendingService == call.IDFromSendingService {
			s.phone.calls[i].PolledAt = append(s.phone.calls[i].PolledAt, s.phone.now())
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"sid": call.IDFromSendingService, "status": call.Status, "to": call.To})
}

// twilioNumbers filters IncomingPhoneNumbers by PhoneNumber and exposes voice capability.
func (s *Server) twilioNumbers(w http.ResponseWriter, r *http.Request) {
	if _, ok := twilioRequest(w, r); !ok {
		return
	}
	numbers := []any{}
	number := r.URL.Query().Get("PhoneNumber")
	if number == TwilioPhoneNumberToCallFrom || number == "+12025550103" {
		numbers = append(numbers, map[string]any{"account_sid": TwilioAccountSID, "phone_number": number, "capabilities": map[string]bool{"voice": true}})
	}
	writeJSON(w, 200, map[string]any{"incoming_phone_numbers": numbers})
}
