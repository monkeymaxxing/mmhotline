package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

func TestTwilioSetupChecksAndProtectsContacts(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	fields := fakePhoneSetupFields()
	fields["twilio_api_key_secret"] = "wrong-key"
	if _, err := r.a.SetUpSendingService(ctx, r.s.Principal, "twilio", fields); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong key: %v", err)
	}
	fields = fakePhoneSetupFields()
	fields["twilio_phone_number_to_call_from"] = "+12025550199"
	if _, err := r.a.SetUpSendingService(ctx, r.s.Principal, "twilio", fields); err == nil || !strings.Contains(err.Error(), "owned by the configured Twilio account") {
		t.Fatalf("unowned number: %v", err)
	}
	fields = fakePhoneSetupFields()
	fields["extra_phone_numbers_on_the_contact_card"] = "+12025550198, +12025550199"
	result, err := r.a.SetUpSendingService(ctx, r.s.Principal, "twilio", fields)
	if err != nil || result.Setup.Settings["twilio_account_sid"] != fields["twilio_account_sid"] {
		t.Fatalf("setup: %+v %v", result, err)
	}
	if got := strings.Join(r.a.twilioSendingService().ContactCardNumbers(), ","); got != "+12025550102,+12025550198,+12025550199" {
		t.Fatal(got)
	}
	r.sendingServices = r.a.twilioSendingService()
	r.addPhone(t, r.s.Principal, barbaraNumber)
	if err := r.a.RemoveSendingServiceSetup(ctx, r.s.Principal, "twilio"); !errors.Is(err, ErrConflict) {
		t.Fatalf("remove with contact: %v", err)
	}
	public, secret := "", ""
	if err := r.a.Store.db.QueryRow(`SELECT settings_json,secret_settings_json FROM sending_service_setups WHERE sending_service='twilio'`).Scan(&public, &secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(public, fakesendingservices.TwilioAPIKeySecret) || !strings.Contains(secret, fakesendingservices.TwilioAPIKeySecret) {
		t.Fatal("secret storage")
	}
	restarted := New(r.a.Store)
	if err := restarted.LoadSendingServiceSetups(ctx, nil, r.a.fakeSendingServicesURL); err != nil {
		t.Fatal(err)
	}
	if numbers := restarted.twilioSendingService().ContactCardNumbers(); len(numbers) != 3 {
		t.Fatal(numbers)
	}
}

func TestPhoneNumberRequiresSendingServiceSetup(t *testing.T) {
	a := foundationApp(t)
	s := memberFixture(t, a, "phone-setup@example.test")
	if _, err := a.AddPhoneNumber(context.Background(), s.Principal, barbaraNumber); !errors.Is(err, ErrInvalid) {
		t.Fatalf("number without setup: %v", err)
	}
}

func TestPhoneCallPollingDeadlinesAndNoChangeWrites(t *testing.T) {
	r := newPhoneRig(t)
	r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	call := r.lastCallTo(t, barbaraNumber)
	changes := scalarTeam(t, r.a, `SELECT total_changes()`)
	for range 14 {
		r.poll(t, time.Second)
	}
	calls := r.fake.PhoneCalls()
	var polled []time.Time
	for _, c := range calls {
		if c.IDFromSendingService == call.IDFromSendingService {
			polled = c.PolledAt
		}
	}
	if len(polled) != 2 {
		t.Fatalf("polls: %v", polled)
	}
	if polled[1].Sub(polled[0]) < 5*time.Second {
		t.Fatal(polled)
	}
	if after := scalarTeam(t, r.a, `SELECT total_changes()`); after != changes {
		t.Fatalf("unchanged polls wrote: %d -> %d", changes, after)
	}
	if _, err := r.fake.AnswerCall(call.IDFromSendingService, fakesendingservices.CallAnswered); err != nil {
		t.Fatal(err)
	}
	r.poll(t, time.Second)
	for range 10 {
		r.poll(t, time.Second)
	}
	for _, c := range r.fake.PhoneCalls() {
		if c.IDFromSendingService == call.IDFromSendingService && len(c.PolledAt) != 3 {
			t.Fatal(c.PolledAt)
		}
	}
}

func TestPhoneCallPollingStopsAtThreeMinutes(t *testing.T) {
	r := newPhoneRig(t)
	r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	call := r.lastCallTo(t, barbaraNumber)
	for range 200 {
		r.poll(t, time.Second)
	}
	for _, c := range r.fake.PhoneCalls() {
		if c.IDFromSendingService != call.IDFromSendingService {
			continue
		}
		if len(c.PolledAt) != 35 {
			t.Fatalf("polls: %d", len(c.PolledAt))
		}
		for _, at := range c.PolledAt {
			if !at.Before(call.CreatedAt.Add(3 * time.Minute)) {
				t.Fatal("poll after bound", at)
			}
		}
	}
}

func TestIdleAutomatedVoiceCallContactMethodTypeMakesNoRequestsOrWrites(t *testing.T) {
	r := newPhoneRig(t)
	// The fake records call fetches on each call. Count all HTTP requests too,
	// so an accidental account or empty-call lookup cannot pass this check.
	count := 0
	r.sendingServices.Client = countingHTTPClient(r.sendingServices.client(), &count)
	backgroundRounds(t, r.a)
	changes := scalarTeam(t, r.a, `SELECT total_changes()`)
	for range 20 {
		r.advance(time.Second)
		backgroundRounds(t, r.a)
	}
	if count != 0 || scalarTeam(t, r.a, `SELECT total_changes()`) != changes {
		t.Fatalf("idle requests=%d", count)
	}
}

type phoneCountingTransport struct {
	next  http.RoundTripper
	count *int
}

func (c phoneCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*c.count++
	return c.next.RoundTrip(r)
}
func countingHTTPClient(client *http.Client, count *int) *http.Client {
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	copied := *client
	copied.Transport = phoneCountingTransport{next: next, count: count}
	return &copied
}

// Capture creation parameters on the same HTTP connection to fake Twilio.
// This checks the actual request rather than reconstructing the adapter form.
type phoneCreationTransport struct {
	next  http.RoundTripper
	forms []url.Values
}

func (c *phoneCreationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Calls.json") {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		c.forms = append(c.forms, form)
	}
	return c.next.RoundTrip(r)
}
func TestPhoneCallCreationHasNoStatusCallbackParameters(t *testing.T) {
	r := newPhoneRig(t)
	capture := &phoneCreationTransport{next: http.DefaultTransport}
	r.sendingServices.Client = &http.Client{Transport: capture}
	r.addPhone(t, r.s.Principal, barbaraNumber)
	if len(capture.forms) != 1 {
		t.Fatal(capture.forms)
	}
	for name := range capture.forms[0] {
		if strings.HasPrefix(name, "StatusCallback") {
			t.Fatalf("outbound callback parameter %s", name)
		}
	}
}

func TestPhoneCallCanceledResultEndsPolling(t *testing.T) {
	r := newPhoneRig(t)
	contact := r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	call := r.lastCallTo(t, barbaraNumber)
	if err := r.fake.ReportCallStatus(call.IDFromSendingService, "canceled"); err != nil {
		t.Fatal(err)
	}
	r.poll(t, 5*time.Second)
	if delivery := lastEscalation(t, deliveriesOf(t, r.a, contact.ID)); delivery.OutcomeLabel() != "No answer" {
		t.Fatal(delivery)
	}
	for range 10 {
		r.poll(t, time.Second)
	}
	for _, fetched := range r.fake.PhoneCalls() {
		if fetched.IDFromSendingService == call.IDFromSendingService && len(fetched.PolledAt) != 1 {
			t.Fatal(fetched.PolledAt)
		}
	}
}
