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

func postTwilio(t *testing.T, base, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/twilio/2010-04-01/Accounts/"+TwilioAccountSID+path, strings.NewReader(form.Encode()))
	req.SetBasicAuth(TwilioAPIKeySID, TwilioAPIKeySecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func TestFakePhonePageHasTheButtons(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()
	res, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{"Refuse the next request", "Lose the next response", "Outside the geographic permissions"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("page lacks %q", want)
		}
	}
}

func TestFakeTwilioCallsRateLimitsAndFetchedStatus(t *testing.T) {
	fake := New()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	fake.SetPhoneClock(func() time.Time { return now })
	server := httptest.NewServer(fake)
	defer server.Close()
	markup := `<Response><Say>An SOS incident needs someone now.</Say><Say>An SOS incident needs someone now.</Say><Hangup/></Response>`
	form := url.Values{"To": {"+15555550123"}, "From": {TwilioPhoneNumberToCallFrom}, "Twiml": {markup}, "Timeout": {"30"}, "TimeLimit": {"120"}}
	status, body := postTwilio(t, server.URL, "/Calls.json", form)
	if status != 201 {
		t.Fatalf("call: %d %v", status, body)
	}
	callID := body["sid"].(string)
	if status, body := postTwilio(t, server.URL, "/Calls.json", form); status != 429 || body["code"] != float64(20429) {
		t.Fatalf("rate limit: %d %v", status, body)
	}
	calls := fake.PhoneCalls()
	if len(calls) != 1 || len(calls[0].Said) != 2 || calls[0].RingSeconds != 30 || calls[0].TimeLimitSeconds != 120 {
		t.Fatalf("calls: %+v", calls)
	}
	if _, err := fake.AnswerCall(callID, CallAnswered); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/twilio/2010-04-01/Accounts/"+TwilioAccountSID+"/Calls/"+callID+".json", nil)
	req.SetBasicAuth(TwilioAPIKeySID, TwilioAPIKeySecret)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var fetched map[string]any
	json.NewDecoder(response.Body).Decode(&fetched)
	response.Body.Close()
	if response.StatusCode != 200 || fetched["status"] != "completed" || fetched["sid"] != callID {
		t.Fatal(fetched)
	}
	now = now.Add(time.Second)
	fake.SetNumberBehavior("+15555550123", NumberInvalid)
	if status, body := postTwilio(t, server.URL, "/Calls.json", form); status != 400 || body["code"] != float64(21211) {
		t.Fatalf("invalid number: %d %v", status, body)
	}
	now = now.Add(time.Second)
	fake.SetNumberBehavior("+15555550123", NumberOutsideGeographicPermissions)
	if status, body := postTwilio(t, server.URL, "/Calls.json", form); status != 400 || body["code"] != float64(21215) {
		t.Fatalf("geographic refusal: %d %v", status, body)
	}
}

// The page's "Refuse the next request" button posts this form.
func TestFakePhonePageRefusesTheNextRequest(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()
	res, err := http.PostForm(server.URL+"/fake-api/actions/phone", url.Values{"action": {"next_request"}, "sending_service": {"twilio"}, "behavior": {RefuseNextRequest}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	form := url.Values{"To": {"+15555550123"}, "From": {TwilioPhoneNumberToCallFrom}, "Twiml": {`<Response><Say>Test.</Say></Response>`}, "Timeout": {"30"}, "TimeLimit": {"120"}}
	if status, body := postTwilio(t, server.URL, "/Calls.json", form); status < 400 {
		t.Fatalf("the armed refusal let the call through: %d %v", status, body)
	}
	if status, body := postTwilio(t, server.URL, "/Calls.json", form); status != 201 {
		t.Fatalf("the refusal was not used up: %d %v", status, body)
	}
}
