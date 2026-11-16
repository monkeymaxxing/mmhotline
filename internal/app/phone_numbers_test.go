package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// phoneRig runs operations against fake Twilio. Its administrator is Responder.
type phoneRig struct {
	a               *App
	s               personFixture
	fake            *fakesendingservices.Server
	server          *httptest.Server
	sendingServices *TwilioSendingService
	advance         func(time.Duration)
}

const barbaraNumber = "+15555550123"

func newPhoneRig(t *testing.T) *phoneRig {
	t.Helper()
	fake := fakesendingservices.New()
	fakeServer := httptest.NewServer(fake)
	t.Cleanup(fakeServer.Close)
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	server := httptest.NewTLSServer(a.Handler())
	t.Cleanup(server.Close)
	fake.SetPhoneClock(a.Now)
	a.WebURL = "https://mmhotline.example.com"
	fixture := memberFixture(t, a, "phone@example.test")
	if err := a.LoadSendingServiceSetups(context.Background(), nil, fakeServer.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetUpSendingService(context.Background(), fixture.Principal, "twilio", fakePhoneSetupFields()); err != nil {
		t.Fatal(err)
	}
	sendingServices := a.twilioSendingService()
	a.WebURL = "https://mmhotline.example.com"
	return &phoneRig{a: a, s: fixture, fake: fake, server: server, sendingServices: sendingServices, advance: advance}
}

// dispatch runs the delivery worker until nothing more is due now.
func (r *phoneRig) dispatch(t *testing.T) {
	t.Helper()
	r.dispatchTeam(t)
}

func (r *phoneRig) dispatchTeam(t *testing.T) {
	t.Helper()
	for range 50 {
		if _, err := r.a.ProcessPhoneCall(context.Background()); err != nil {
			t.Fatal(err)
		}
		work, err := r.a.DispatchOne(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !work {
			return
		}
	}
	t.Fatal("the worker never ran out of work")
}

// phoneMethods returns a member's automated voice call contact method for a number.
func (r *phoneRig) phoneMethods(t *testing.T, p Principal, number string) ContactMethod {
	t.Helper()
	methods, err := r.a.ContactMethods(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.a.CurrentMember(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range methods {
		if c.SentTo != number || c.MemberID != m.ID {
			continue
		}
		if c.ContactMethodType == "automated_voice_call" {
			return c
		}
	}
	return ContactMethod{}
}

func (r *phoneRig) callsTo(number string) []fakesendingservices.PhoneCall {
	var found []fakesendingservices.PhoneCall
	for _, c := range r.fake.PhoneCalls() {
		if c.To == number && !strings.Contains(c.Markup, " added this contact method at ") {
			found = append(found, c)
		}
	}
	return found
}

// addPhone saves a number and answers its automatic test call.
func (r *phoneRig) addPhone(t *testing.T, p Principal, number string) ContactMethod {
	t.Helper()
	r.advance(time.Second)
	if _, err := r.a.AddPhoneNumber(context.Background(), p, number); err != nil {
		t.Fatal(err)
	}
	r.dispatchTeam(t)
	calls := r.fake.PhoneCalls()
	if _, err := r.fake.AnswerCall(calls[len(calls)-1].IDFromSendingService, fakesendingservices.CallAnswered); err != nil {
		t.Fatal(err)
	}
	if err := r.fake.ReportCallStatus(calls[len(calls)-1].IDFromSendingService, "completed"); err != nil {
		t.Fatal(err)
	}
	r.poll(t, 5*time.Second)
	r.advance(time.Minute)
	return r.phoneMethods(t, p, number)
}

// lastEscalation returns the newest SOS escalation delivery of a list.
func lastEscalation(t *testing.T, ds []Delivery) Delivery {
	t.Helper()
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Purpose == "sos_escalation" {
			return ds[i]
		}
	}
	t.Fatal("no SOS escalation delivery")
	return Delivery{}
}

func deliveriesOf(t *testing.T, a *App, contactMethodID string) []Delivery {
	t.Helper()
	var out []Delivery
	err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT `+deliveryColumns+` FROM deliveries WHERE contact_method_id=? ORDER BY created_at,id`, contactMethodID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelivery(rows)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParsePhoneNumberCountries(t *testing.T) {
	cases := []struct{ typed, number, country, refusal string }{
		{"+1 (555) 555-0123", "+15555550123", usOrCanada, ""},
		{"0044 7700 900123", "+447700900123", "the UK", ""},
		{"+49 30 1234567", "+49301234567", "Germany", ""},
		{"+34 612 345 678", "+34612345678", "Spain", ""},
		{"+354 611 2345", "+3546112345", "Iceland", ""},
		{"+1 876 555 0123", "", "", "phone numbers in Jamaica yet"},
		{"+1 787 555 0123", "", "", "phone numbers in Puerto Rico yet"},
		{"+1 800 555 0123", "", "", "not a toll-free or premium number"},
		{"+91 98765 43210", "", "", "phone numbers in India yet. Use a Pushover notification instead."},
		{"+7 912 345 6789", "", "", "phone numbers in Russia or Kazakhstan yet"},
		{"+224 612 34 56 78", "", "", "phone numbers in the country with calling code +224 yet"},
		{"555 0123", "", "", "with its country code"},
		{"+1 555 0123", "", "", "10 digits after +1"},
	}
	for _, c := range cases {
		number, country, err := parsePhoneNumber(c.typed)
		if c.refusal != "" {
			if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.refusal) {
				t.Errorf("%q: %v, want %q", c.typed, err, c.refusal)
			}
			continue
		}
		if err != nil || number != c.number || country.Name != c.country {
			t.Errorf("%q: %q %q %v", c.typed, number, country.Name, err)
		}
	}
	if len(acceptedPhoneCountries) != 32 {
		t.Fatalf("accepted countries: %d, want the US or Canada, 27 EU countries, the UK, Norway, Switzerland, Iceland", len(acceptedPhoneCountries))
	}
}

func TestAddingAPhoneNumberCreatesAAutomatedVoiceCallContactMethod(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	added, err := r.a.AddPhoneNumber(ctx, p, "+1 555 555 0123")
	if err != nil || added.PhoneNumberToCall != barbaraNumber || len(added.ContactMethods) != 1 {
		t.Fatalf("added: %+v %v", added, err)
	}
	call := r.phoneMethods(t, p, barbaraNumber)
	if call.ID == "" || call.SendWhileUnavailableOrMuted || strings.Join(call.Subscriptions, ",") != "sos" {
		t.Fatalf("call: %+v", call)
	}
	if _, err = r.a.AddPhoneNumber(ctx, p, barbaraNumber); !errors.Is(err, ErrConflict) {
		t.Fatalf("same number twice: %v", err)
	}
	if _, err = r.a.SaveContactMethod(ctx, p, ContactMethodInput{ID: call.ID, ContactMethodType: "automated_voice_call", ExpectedRevision: call.Revision, Subscriptions: []string{"sos", "warn"}}); err == nil || !strings.Contains(err.Error(), "only for SOS") {
		t.Fatalf("Warning on a phone call: %v", err)
	}
	saved, err := r.a.SaveContactMethod(ctx, p, ContactMethodInput{ID: call.ID, ContactMethodType: "automated_voice_call", ExpectedRevision: call.Revision, Fields: map[string]string{"phone_number_to_call": "+15555550999"}, Subscriptions: []string{"sos"}})
	if err != nil || saved.SentTo != barbaraNumber {
		t.Fatalf("number changed: %+v %v", saved, err)
	}
	if _, err = r.a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "automated_voice_call", Shared: true, Fields: map[string]string{"phone_number_to_call": barbaraNumber}, Subscriptions: []string{"sos"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shared phone: %v", err)
	}
	if _, err = r.a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "automated_voice_call", Fields: map[string]string{"phone_number_to_call": barbaraNumber}, Subscriptions: []string{"sos"}}); err == nil || !strings.Contains(err.Error(), "Add a phone number") {
		t.Fatalf("generic form: %v", err)
	}
	added, err = r.a.AddPhoneNumber(ctx, p, "+34 612 345 678")
	if err != nil || len(added.ContactMethods) != 1 || added.ContactMethods[0].ContactMethodType != "automated_voice_call" {
		t.Fatalf("Spain: %+v %v", added, err)
	}
	if _, err = r.a.AddPhoneNumber(ctx, p, "+91 98765 43210"); err == nil || !strings.Contains(err.Error(), "MMHotline can't call phone numbers in India yet") {
		t.Fatalf("India: %v", err)
	}
}

func (r *phoneRig) browser(t *testing.T) *http.Client {
	t.Helper()
	c := browserClient(r.server)
	useSignInEmailAddress(c, r.s.Principal.SignInEmailAddress)
	return c
}

func getPage(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, res.StatusCode, body)
	}
	return string(body)
}

func TestAddAPhoneNumberThroughTheContactMethodsPage(t *testing.T) {
	r := newPhoneRig(t)
	c := r.browser(t)
	page := getPage(t, c, r.server.URL+"/contact-methods")
	if !strings.Contains(page, "Add a phone number") || !strings.Contains(page, phoneNumberConsentText) {
		t.Fatalf("no phone number form: %s", page)
	}
	token := csrfPattern.FindStringSubmatch(page)[1]
	res := postForm(t, c, r.server.URL, "/contact-methods/phone-number", url.Values{"csrf": {token}, "phone_number_to_call": {"+91 98765 43210"}}, 400)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(pageText(string(body)), "MMHotline can't call phone numbers in India yet.") || workflowField(t, string(body), "phone_number_to_call") != "+91 98765 43210" {
		t.Fatalf("refusal page: %s", body)
	}
	res = postForm(t, c, r.server.URL, "/contact-methods/phone-number", url.Values{"csrf": {token}, "phone_number_to_call": {barbaraNumber}}, 200)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{"Phone number +15555550123 added", "Automated voice call", "A test call is queued", "Send test", "Last test: Waiting to send"} {
		if !strings.Contains(pageText(string(body)), want) {
			t.Fatalf("missing %q: %s", want, body)
		}
	}
	r.dispatch(t)
	page = getPage(t, c, r.server.URL+"/contact-methods")
	for _, want := range []string{"Download the MMHotline contact card", "Allow the MMHotline contact through Do Not Disturb", "Turn on Do Not Disturb and press “Send test”"} {
		if !strings.Contains(page, want) {
			t.Fatalf("missing %q: %s", want, page)
		}
	}
	// A automated voice call card offers SOS only.
	callCard := contactMethodForm(t, page, "automated_voice_call")
	if strings.Contains(callCard, `value="info"`) || strings.Contains(callCard, `value="warn"`) || !strings.Contains(callCard, `value="sos"`) || strings.Contains(callCard, `name="phone_number_to_call"`) {
		t.Fatalf("automated voice call card: %s", callCard)
	}
}

// sos opens an SOS incident by hand and returns it.
func (r *phoneRig) sos(t *testing.T, title string) Alert {
	t.Helper()
	e, err := r.a.ManualSOS(context.Background(), r.s.Principal, title, "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// minuteLater moves the clock on, lets the incident owner repeat and
// escalate, and dispatches.
func (r *phoneRig) minuteLater(t *testing.T, d time.Duration) {
	t.Helper()
	r.advance(d)
	if err := r.a.ReconcileIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
}

func memberID(t *testing.T, a *App, p Principal) string {
	t.Helper()
	m, err := a.CurrentMember(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func alertTitles(t *testing.T, a *App, p Principal) string {
	t.Helper()
	alerts, err := a.ListAlerts(context.Background(), p, AlertFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range alerts {
		titles = append(titles, e.Title)
	}
	return strings.Join(titles, "\n")
}

func TestNewPhoneReceivesSOSBeforeItsTest(t *testing.T) {
	r := newPhoneRig(t)
	added, err := r.a.AddPhoneNumber(context.Background(), r.s.Principal, barbaraNumber)
	if err != nil {
		t.Fatal(err)
	}
	r.sos(t, "Database down")
	r.dispatch(t)
	calls := r.fake.PhoneCalls()
	if len(calls) != 1 || calls[0].Said[0] != oneIncidentScript {
		t.Fatalf("new phone needed another step before SOS: %+v", calls)
	}
	ds := deliveriesOf(t, r.a, added.ContactMethods[0].ID)
	var testCount int
	for _, d := range ds {
		if d.Purpose == "contact_method_test" {
			testCount++
			if d.State != "queued" {
				t.Fatalf("SOS required an accepted test: %+v", d)
			}
		}
	}
	if testCount != 1 {
		t.Fatalf("save queued %d tests", testCount)
	}
}

func TestThreePhoneNumbersPerMember(t *testing.T) {
	r := newPhoneRig(t)
	for _, number := range []string{"+15555550101", "+15555550102", "+15555550103"} {
		if _, err := r.a.AddPhoneNumber(context.Background(), r.s.Principal, number); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.a.AddPhoneNumber(context.Background(), r.s.Principal, "+15555550104"); !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "at most 3 phone numbers") {
		t.Fatalf("fourth number: %v", err)
	}
}

func fakePhoneSetupFields() map[string]string {
	return map[string]string{"twilio_account_sid": fakesendingservices.TwilioAccountSID, "twilio_api_key_sid": fakesendingservices.TwilioAPIKeySID, "twilio_api_key_secret": fakesendingservices.TwilioAPIKeySecret, "twilio_phone_number_to_call_from": fakesendingservices.TwilioPhoneNumberToCallFrom}
}

func (r *phoneRig) poll(t *testing.T, after time.Duration) {
	t.Helper()
	r.advance(after)
	if _, err := r.a.ProcessPhoneCall(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// recordTwilioSetUpForTest supports command tests that do not send calls.
func recordTwilioSetUpForTest(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO sending_service_setups(sending_service,settings_json,secret_settings_json,set_up_at,set_up_by) VALUES('twilio','{}','{}',0,'a test fixture')`); err != nil {
		t.Fatal(err)
	}
}
