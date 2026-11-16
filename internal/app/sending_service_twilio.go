package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TwilioSendingService owns Twilio call credentials and request pacing.
type TwilioSendingService struct {
	Twilio                            *TwilioAccount
	ExtraPhoneNumbersOnTheContactCard []string
	Client                            *http.Client
	mu                                sync.Mutex
	nextFreeSlot                      time.Time
	waiting                           map[int]time.Time
	nextPoll                          map[string]time.Time
}

// TwilioAccount holds the team's Twilio call credentials. APIBaseURL defaults
// to https://api.twilio.com; tests use the fake sending service's /twilio.
type TwilioAccount struct {
	AccountSID, APIKeySID, APIKeySecret, PhoneNumberToCallFrom string
	APIBaseURL                                                 string
}

const (
	sendingServiceTwilio = "twilio"
	twilioWaitRetry      = time.Second
	twilioResponseLimit  = 64 * 1024
	priorityFirstContact = 0
	priorityUrgent       = 1
)

// takeSlot reserves Twilio's next call slot. First SOS contacts go before repeats.
func (p *TwilioSendingService) takeSlot(priority int, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting == nil {
		p.waiting = map[int]time.Time{}
	}
	blocked := p.nextFreeSlot.After(now)
	for waitingPriority, until := range p.waiting {
		if waitingPriority < priority && until.After(now) {
			blocked = true
		}
	}
	if blocked {
		p.waiting[priority] = now.Add(2 * twilioWaitRetry)
		return false
	}
	p.nextFreeSlot = now.Add(time.Second)
	return true
}

// twilioRequestResult is what one Twilio request did.
type twilioRequestResult struct {
	State, IDFromSendingService, Diagnostic string
	// TooManyRequests: the sending service answered HTTP 429. It did nothing with
	// the request and takes requests again a moment later.
	TooManyRequests bool
}

func (p *TwilioSendingService) client() *http.Client {
	client := http.Client{Timeout: 15 * time.Second}
	if p.Client != nil {
		client = *p.Client
	}
	// A redirect must never move credentials or a POST elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// do sends one sending service request and reads a bounded answer. A request that
// may have reached the sending service without a readable answer is an unknown
// outcome; the error never includes the URL or credentials.
func (p *TwilioSendingService) do(req *http.Request) (int, []byte, error) {
	resp, err := p.client().Do(req)
	if err != nil {
		return 0, nil, errors.New("no answer from Twilio")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, twilioResponseLimit+1))
	if err != nil || len(data) > twilioResponseLimit {
		return resp.StatusCode, nil, errors.New("cannot read the answer from Twilio")
	}
	return resp.StatusCode, data, nil
}

// twilioPost posts a form to Twilio's REST API under the account.
// https://www.twilio.com/docs/usage/requests-to-twilio
func (p *TwilioSendingService) twilioPost(ctx context.Context, path string, form url.Values) (int, []byte, error) {
	req, err := p.twilioPostRequest(ctx, path, form)
	if err != nil {
		return 0, nil, err
	}
	return p.do(req)
}

// twilioPostRequest builds a form POST under the account, with its
// credentials. Both calls and the MMHotline-down alarm's held calls use it.
func (p *TwilioSendingService) twilioPostRequest(ctx context.Context, path string, form url.Values) (*http.Request, error) {
	t := p.Twilio
	base := t.APIBaseURL
	if base == "" {
		base = "https://api.twilio.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/2010-04-01/Accounts/"+url.PathEscape(t.AccountSID)+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(t.APIKeySID, t.APIKeySecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// twilioAnswer turns a Twilio answer into a twilioRequestResult.
func twilioAnswer(status int, data []byte, err error) twilioRequestResult {
	if err != nil {
		return twilioRequestResult{State: "uncertain", Diagnostic: "No answer from Twilio"}
	}
	var answer struct {
		SID     string `json:"sid"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &answer)
	switch {
	case status >= 200 && status < 300 && answer.SID != "" && len(answer.SID) <= 64:
		return twilioRequestResult{State: "accepted", IDFromSendingService: answer.SID}
	case status >= 200 && status < 300:
		return twilioRequestResult{State: "uncertain", Diagnostic: "Twilio accepted the request without an ID"}
	case status == http.StatusTooManyRequests || status >= 500:
		return twilioRequestResult{State: "failed", Diagnostic: fmt.Sprintf("Twilio answered HTTP %d", status), TooManyRequests: status == http.StatusTooManyRequests}
	case answer.Code == 21211:
		return twilioRequestResult{State: "failed", Diagnostic: fmt.Sprintf("Refused by Twilio: the number cannot be reached (error %d)", answer.Code)}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return twilioRequestResult{State: "failed", Diagnostic: fmt.Sprintf("Twilio refused MMHotline's credentials (HTTP %d)", status)}
	}
	return twilioRequestResult{State: "failed", Diagnostic: fmt.Sprintf("Refused by Twilio (HTTP %d, error %d)", status, answer.Code)}
}

// Automated voice call settings: ring for 30 seconds, end after at most 2 minutes, and
// never use answering-machine detection.
const (
	callRingSeconds      = 30
	callTimeLimitSeconds = 120
)

// createCall starts a Twilio call with inline TwiML.
// https://www.twilio.com/docs/voice/api/call-resource#create-a-call-resource
func (p *TwilioSendingService) createCall(ctx context.Context, to, markup string) twilioRequestResult {
	if p.Twilio == nil {
		return twilioRequestResult{State: "failed", Diagnostic: "Not called: Twilio is not set up"}
	}
	return twilioAnswer(p.twilioPost(ctx, "/Calls.json", p.callForm(to, markup)))
}

// callForm is the form that creates one call; the MMHotline-down alarm's held calls use it too.
func (p *TwilioSendingService) callForm(to, markup string) url.Values {
	return url.Values{"To": {to}, "From": {p.Twilio.PhoneNumberToCallFrom}, "Twiml": {markup}, "Timeout": {strconv.Itoa(callRingSeconds)}, "TimeLimit": {strconv.Itoa(callTimeLimitSeconds)}}
}

// twilioGet fetches an account resource or one of its subresources.
func (p *TwilioSendingService) twilioGet(ctx context.Context, path string) (int, []byte, error) {
	t := p.Twilio
	base := t.APIBaseURL
	if base == "" {
		base = "https://api.twilio.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/2010-04-01/Accounts/"+url.PathEscape(t.AccountSID)+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(t.APIKeySID, t.APIKeySecret)
	return p.do(req)
}

func (a *App) twilioSendingService() *TwilioSendingService {
	if c, ok := a.configuredContactMethodTypes()["automated_voice_call"].(*AutomatedVoiceCallContactMethodType); ok {
		return c.SendingService
	}
	return nil
}
