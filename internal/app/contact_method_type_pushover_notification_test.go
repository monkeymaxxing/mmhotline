package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mmhotline/internal/fakesendingservices"
)

type pushoverRig struct {
	a         *App
	principal Principal
	fake      *fakesendingservices.Server
	advance   func(time.Duration)
}

func newPushoverRig(t *testing.T) *pushoverRig {
	t.Helper()
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	a.WebURL = "https://mmhotline.example.com"
	p := memberFixture(t, a, "pushover@example.test").Principal
	fake := fakesendingservices.New()
	fake.SetPushoverClock(a.Now)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	if err := a.LoadSendingServiceSetups(context.Background(), nil, server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetUpSendingService(context.Background(), p, "pushover", map[string]string{"pushover_application_token": fakesendingservices.PushoverApplicationToken}); err != nil {
		t.Fatal(err)
	}
	owner, err := a.CurrentMember(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SaveSchedule(context.Background(), p, ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: []string{owner.ID, owner.ID, owner.ID, owner.ID, owner.ID, owner.ID, owner.ID}, BackupKind: "everyone_available"}, 0); err != nil {
		t.Fatal(err)
	}
	return &pushoverRig{a, p, fake, advance}
}
func (r *pushoverRig) dispatch(t *testing.T) {
	t.Helper()
	for range 50 {
		work, err := r.a.DispatchOne(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !work {
			return
		}
	}
	t.Fatal("outbox did not drain")
}
func (r *pushoverRig) poll(t *testing.T) {
	t.Helper()
	if _, err := r.a.ProcessPushoverReceipt(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (r *pushoverRig) add(t *testing.T, key string) ContactMethod {
	t.Helper()
	c, err := r.a.SaveContactMethod(context.Background(), r.principal, ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": key}, Subscriptions: []string{"sos", "warn", "info"}})
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	messages := r.fake.PushoverMessages()
	test := messages[len(messages)-1]
	if test.Priority != 0 || test.Title != "MMHotline can reach this phone" || test.Receipt != "" {
		t.Fatalf("test message: %+v", test)
	}
	return c
}
func (r *pushoverRig) sos(t *testing.T, title string) string {
	t.Helper()
	event, err := r.a.ManualSOS(context.Background(), r.principal, title, "Alert body")
	if err != nil {
		t.Fatal(err)
	}
	return event.IncidentID
}
func (r *pushoverRig) status(t *testing.T, id, want string) {
	t.Helper()
	i, err := r.a.GetIncident(context.Background(), r.principal, id)
	if err != nil || i.Status != want {
		t.Fatalf("incident %s: %+v, %v; want %s", id, i, err, want)
	}
}
func TestPushoverSetupChecksTokenAndRestoresWithoutSecrets(t *testing.T) {
	r := newPushoverRig(t)
	ctx := context.Background()
	_, err := r.a.SetUpSendingService(ctx, r.principal, "pushover", map[string]string{"pushover_application_token": strings.Repeat("x", 30)})
	if err == nil || !strings.Contains(err.Error(), "application token is invalid") {
		t.Fatalf("wrong token: %v", err)
	}
	setups, err := r.a.SendingServiceSetups(ctx, r.principal)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(setups)
	if strings.Contains(string(raw), fakesendingservices.PushoverApplicationToken) {
		t.Fatal("setup exposed token")
	}
	var found bool
	for _, setup := range setups {
		if setup.SendingService == "pushover" {
			found = true
			if !setup.IsSetUp || setup.Settings["pushover_monthly_message_limit"] != float64(10000) {
				t.Fatalf("setup: %+v", setup)
			}
		}
	}
	if !found {
		t.Fatal("missing Pushover setup")
	}
	restarted := New(r.a.Store)
	if err = restarted.LoadSendingServiceSetups(ctx, nil, r.a.fakeSendingServicesURL); err != nil {
		t.Fatal(err)
	}
	if restarted.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType).ApplicationToken != fakesendingservices.PushoverApplicationToken {
		t.Fatal("setup was not restored")
	}
}

// TestPushoverContactCommandsUseExplicitJSON: a Pushover contact method
// needs the Pushover sending service set up, and defaults to every severity.
func TestPushoverContactCommandsUseExplicitJSON(t *testing.T) {
	a := foundationApp(t)
	a.WebURL = "https://mmhotline.example.com"
	addPerson(t, a, "barbara@example.test", "Barbara", RoleAdministrator)
	request := CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: "barbara@example.test", ContactMethodType: "pushover_notification", PushoverUserKey: fakesendingservices.PushoverUserKey}}
	result, _ := commandRequest(t, a, request, 400)
	if !strings.Contains(result.Message, "adds sending_services.pushover to the settings file") || !strings.Contains(result.Message, a.WebURL+"/sending-services/pushover") {
		t.Fatal(result.Message)
	}
	fake := fakesendingservices.New()
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := a.LoadSendingServiceSetups(context.Background(), nil, server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetUpSendingService(context.Background(), Principal{UnixUser: "fixture"}, "pushover", map[string]string{"pushover_application_token": fakesendingservices.PushoverApplicationToken}); err != nil {
		t.Fatal(err)
	}
	setup, raw := commandRequest(t, a, CommandRequest{Group: "sending-services", Action: "list"}, 200)
	assertCommandKeys(t, raw, "sending_service_setups", "pushover_monthly_message_limit")
	if strings.Contains(raw, fakesendingservices.PushoverApplicationToken) || !savedSendingServiceSetup(t, setup.SendingServiceSetups, "pushover").IsSetUp {
		t.Fatal(raw)
	}
	result, raw = commandRequest(t, a, request, 200)
	assertCommandKeys(t, raw, "contact_method_id", "sign_in_email_address", "contact_method_type", "sends_to", "alert_severities", "last_test_message")
	// Pushover is often a person's only contact method, so it defaults to every
	// severity: with SOS alone, Warning and Info alerts would reach nobody.
	if len(result.ContactMethods) != 1 || strings.Join(result.ContactMethods[0].AlertSeverities, ",") != "info,sos,warn" {
		t.Fatal(raw)
	}
	if _, err := a.DispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := fake.PushoverMessages()
	if len(sent) != 1 || sent[0].Priority != 0 || sent[0].Receipt != "" || sent[0].Title != "MMHotline can reach this phone" {
		t.Fatalf("test: %+v", sent)
	}
	commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: ContactMethodsCommandRequest{SharedWithTeam: true, ContactMethodType: "pushover_notification", PushoverUserKey: fakesendingservices.PushoverThirdUserKey}}, 400)
}
func TestPushoverContactRequiresSetupAndBoundsPersonalKeys(t *testing.T) {
	a := foundationApp(t)
	p := memberFixture(t, a, "barbara@example.test").Principal
	a.WebURL = "https://mmhotline.example.com"
	in := ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}, Subscriptions: []string{"sos"}}
	if _, err := a.SaveContactMethod(context.Background(), p, in); err == nil || !strings.Contains(err.Error(), "/sending-services/pushover") {
		t.Fatalf("without setup: %v", err)
	}
	refused, _ := commandRequest(t, a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: p.SignInEmailAddress, ContactMethodType: "pushover_notification", PushoverUserKey: fakesendingservices.PushoverUserKey}}, 400)
	if !strings.Contains(refused.Message, "adds sending_services.pushover to the settings file") {
		t.Fatal(refused.Message)
	}

	r := newPushoverRig(t)
	for _, key := range []string{fakesendingservices.PushoverUserKey, fakesendingservices.PushoverOtherUserKey, fakesendingservices.PushoverThirdUserKey} {
		r.add(t, key)
	}
	in.Fields["pushover_user_key"] = strings.Repeat("a", 30)
	if _, err := r.a.SaveContactMethod(context.Background(), r.principal, in); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("fourth key: %v", err)
	}
	in.Fields["pushover_user_key"] = fakesendingservices.PushoverUserKey
	if _, err := r.a.SaveContactMethod(context.Background(), r.principal, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate key: %v", err)
	}
	var wakes map[string][]string
	err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		wakes, err = contactMethodTypesThatWakeEachMember(context.Background(), tx)
		return err
	})
	if err != nil || !slices.Equal(wakes[memberIDForUser(t, r.a, r.principal)], []string{"pushover_notification"}) {
		t.Fatalf("Pushover not counted for waking: %v %v", wakes, err)
	}
}
func TestPushoverSOSCombinesAndAcknowledgementClaimsExactlyCountedIncidents(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	first := r.sos(t, "Database down")
	second := r.sos(t, "Disk full")
	r.dispatch(t)
	sent := r.fake.PushoverMessages()
	if len(sent) != 2 {
		t.Fatalf("SOS was not combined: %+v", sent)
	}
	emergency := sent[1]
	if emergency.Priority != 2 || emergency.Retry != 30 || emergency.Expire != 1500 || !strings.HasSuffix(emergency.Message, "Acknowledging the alert also claims all 2 incidents for you: you become responsible for the incidents, and MMHotline stops contacting additional people.") || emergency.URLTitle != "Open in MMHotline" || !strings.Contains(emergency.URL, "/incidents/") {
		t.Fatalf("SOS: %+v", emergency)
	}
	r.advance(time.Minute)
	if err := r.a.ReconcileIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.poll(t)
	r.dispatch(t)
	if len(r.fake.PushoverMessages()) != 2 {
		t.Fatal("open emergency accumulated a minute repeat")
	}
	third := r.sos(t, "Queue stuck") // not counted in the already sent message
	if err := r.fake.PushoverAcknowledge(emergency.Receipt); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.poll(t)
	r.dispatch(t)
	r.status(t, first, "claimed")
	r.status(t, second, "claimed")
	r.status(t, third, "open")
	sent = r.fake.PushoverMessages()
	var result int
	for _, m := range sent {
		if m.Priority == -1 && strings.Contains(m.Message, "You claimed Database down.") && strings.Contains(m.Message, "You claimed Disk full.") {
			result++
		}
	}
	if result != 1 {
		t.Fatalf("claim result: %+v", sent)
	}
	r.advance(5 * time.Second)
	r.poll(t)
	r.dispatch(t)
	if len(r.fake.PushoverMessages()) != len(sent) {
		t.Fatal("closed receipt sent another result")
	}
}
func TestPushoverPageClaimAndResolutionCancelEveryFinishedEmergency(t *testing.T) {
	for _, action := range []string{"claim", "resolve"} {
		t.Run(action, func(t *testing.T) {
			r := newPushoverRig(t)
			r.add(t, fakesendingservices.PushoverUserKey)
			r.add(t, fakesendingservices.PushoverOtherUserKey)
			id := r.sos(t, "Database down")
			r.dispatch(t)
			i, err := r.a.GetIncident(context.Background(), r.principal, id)
			if err != nil {
				t.Fatal(err)
			}
			if action == "claim" {
				_, err = r.a.ClaimIncident(context.Background(), r.principal, id, i.Revision, "")
			} else {
				_, err = r.a.ResolveIncident(context.Background(), r.principal, id, i.Revision, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				r.poll(t)
			}
			receipts := r.fake.PushoverReceipts()
			for _, receipt := range receipts {
				if !receipt.Acknowledged && !receipt.Cancelled {
					t.Fatalf("open after %s: %+v", action, receipt)
				}
			}
		})
	}
}
func TestPushoverAcknowledgementDoesNotTakeOverAnotherClaim(t *testing.T) {
	for _, outcome := range []string{"claimed", "resolved"} {
		t.Run(outcome, func(t *testing.T) {
			r := newPushoverRig(t)
			r.add(t, fakesendingservices.PushoverUserKey)
			id := r.sos(t, "Database down")
			r.dispatch(t)
			sent := r.fake.PushoverMessages()
			emergency := sent[len(sent)-1]
			bob := addPerson(t, r.a, "bob@example.test", "Bob", RoleMember)
			i, err := r.a.GetIncident(context.Background(), r.principal, id)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "claimed" {
				_, err = r.a.ClaimIncident(context.Background(), Principal{SignInEmailAddress: bob.SignInEmailAddress}, id, i.Revision, "")
			} else {
				_, err = r.a.ResolveIncident(context.Background(), r.principal, id, i.Revision, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = r.fake.PushoverAcknowledge(emergency.Receipt); err != nil {
				t.Fatal(err)
			}
			r.advance(5 * time.Second)
			r.poll(t)
			r.dispatch(t)
			sent = r.fake.PushoverMessages()
			result := sent[len(sent)-1]
			want := "Already claimed by Bob"
			if outcome == "resolved" {
				want = "This incident is already resolved."
			}
			if result.Priority != -1 || !strings.Contains(result.Message, want) {
				t.Fatalf("answer: %+v", result)
			}
			r.status(t, id, outcome)
			if outcome == "claimed" {
				i, _ = r.a.GetIncident(context.Background(), r.principal, id)
				if i.OwnerMemberID != bob.ID {
					t.Fatal("acknowledgement took over Bob's claim")
				}
			}
		})
	}
}
func TestPushoverExpiredEmergencyAllowsNextMinuteRepeat(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	r.sos(t, "Database down")
	r.dispatch(t)
	emergency := r.fake.PushoverMessages()[1]
	if err := r.fake.PushoverExpire(emergency.Receipt); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.poll(t)
	r.advance(55 * time.Second)
	if err := r.a.ReconcileIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	messages := r.fake.PushoverMessages()
	if len(messages) != 3 || messages[2].Priority != 2 || messages[2].Receipt == emergency.Receipt {
		t.Fatalf("no fresh emergency: %+v", messages)
	}
}
func TestPushoverReceiptDeadlinesSurviveRestartAndIdleDoesNoWork(t *testing.T) {
	r := newPushoverRig(t)
	ctx := context.Background()
	r.a.backgroundRound(ctx, func(err error) { t.Error(err) })
	var before, after int
	if err := r.a.Store.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 600 {
		r.advance(time.Second)
		r.a.backgroundRound(ctx, func(err error) { t.Error(err) })
	}
	if err := r.a.Store.db.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after || len(r.fake.PushoverReceipts()) != 0 {
		t.Fatalf("idle wrote or polled: %d -> %d", before, after)
	}
	r.add(t, fakesendingservices.PushoverUserKey)
	r.sos(t, "Database down")
	r.dispatch(t)
	receipts := r.fake.PushoverReceipts()
	id := receipts[len(receipts)-1].Receipt
	for range 4 {
		r.advance(time.Second)
		r.poll(t)
	}
	receipts = r.fake.PushoverReceipts()
	if len(receipts[len(receipts)-1].Polls) != 0 {
		t.Fatal("polled early")
	}
	r.advance(time.Second)
	r.poll(t)
	restarted := New(r.a.Store)
	restarted.Now = r.a.Now
	restarted.WebURL = r.a.WebURL
	if err := restarted.LoadSendingServiceSetups(ctx, nil, r.a.fakeSendingServicesURL); err != nil {
		t.Fatal(err)
	}
	r.a = restarted
	for range 9 {
		r.advance(time.Second)
		r.poll(t)
	}
	for _, receipt := range r.fake.PushoverReceipts() {
		if receipt.Receipt != id {
			continue
		}
		if len(receipt.Polls) != 2 || receipt.Polls[1].Sub(receipt.Polls[0]) < 5*time.Second {
			t.Fatalf("poll times: %+v", receipt.Polls)
		}
	}
}
func TestPushoverWarningInfoResolvedPrioritiesAndMessageLimits(t *testing.T) {
	r := newPushoverRig(t)
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	settings, _ := c.CheckSettings(context.Background(), ContactMethodSettingsInput{Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}})
	for severity, priority := range map[string]int{"warn": 0, "info": -1, "resolved-sos": -1, "sos": 2} {
		m := ContactMethodSend{Purpose: "subscribed_alert", Severity: severity, Subject: strings.Repeat("界", 300), Title: "Database down", AlertMessage: strings.Repeat("界", 2000), IncidentID: NewID(), PublicURL: r.a.WebURL, Settings: settings.JSON}
		result := c.Send(context.Background(), m)
		if result.State != "accepted" {
			t.Fatalf("%s: %+v", severity, result)
		}
		sent := r.fake.PushoverMessages()
		last := sent[len(sent)-1]
		if last.Sound != "" || last.Priority != priority || utf8.RuneCountInString(last.Title) > 250 || utf8.RuneCountInString(last.Message) > 1024 || last.URL != r.a.WebURL+"/incidents/"+m.IncidentID {
			t.Fatalf("%s: %+v", severity, last)
		}
		if severity == "sos" && !strings.HasSuffix(last.Message, "Acknowledging the alert also claims the incident for you: you become responsible for the incident, and MMHotline stops contacting additional people.") {
			t.Fatal("shortening lost the claim instruction")
		}
	}
}

type pushoverLostResponse struct{ next http.RoundTripper }

func (c pushoverLostResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := c.next.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages.json") {
		response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, err
}
func TestPushoverUnknownSOSIsNotResentButNextRepeatCanWake(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	c.Client = &http.Client{Transport: pushoverLostResponse{http.DefaultTransport}}
	id := r.sos(t, "Database down")
	r.dispatch(t)
	deliveries, err := r.a.IncidentDeliveries(context.Background(), r.principal, id)
	if err != nil {
		t.Fatal(err)
	}
	var uncertain bool
	for _, d := range deliveries {
		if d.Purpose == "sos_escalation" && d.State == "uncertain" && d.NextAttempt.IsZero() {
			uncertain = true
		}
	}
	if !uncertain {
		t.Fatalf("SOS unknown was retried: %+v", deliveries)
	}
	r.advance(5 * time.Second)
	r.dispatch(t)
	if len(r.fake.PushoverMessages()) != 2 {
		t.Fatal("resent an unknown SOS")
	}
	c.Client = nil
	r.advance(55 * time.Second)
	if err = r.a.ReconcileIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	if len(r.fake.PushoverMessages()) != 3 {
		t.Fatal("next minute repeat did not send")
	}
}

func TestPushoverSendingServiceFailuresAndUnknownOrdinaryMessages(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"status": 0, "errors": []string{"sending service says no: " + fakesendingservices.PushoverApplicationToken}})
			}))
			defer server.Close()
			c := &PushoverNotificationContactMethodType{ApplicationToken: fakesendingservices.PushoverApplicationToken, APIBaseURL: server.URL}
			settings, _ := c.CheckSettings(context.Background(), ContactMethodSettingsInput{Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}})
			result := c.Send(context.Background(), ContactMethodSend{Purpose: "subscribed_alert", Severity: "sos", Title: "Down", Subject: "SOS", PublicURL: "https://mmhotline.example.com", Settings: settings.JSON})
			if result.State != "failed" || result.Retryable != (status == 429 || status >= 500) || !strings.Contains(result.Diagnostic, "sending service says no") || strings.Contains(result.Diagnostic, fakesendingservices.PushoverApplicationToken) {
				t.Fatalf("refusal: %+v", result)
			}
		})
	}
	r := newPushoverRig(t)
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	settings, _ := c.CheckSettings(context.Background(), ContactMethodSettingsInput{Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}})
	c.Client = &http.Client{Transport: pushoverLostResponse{http.DefaultTransport}}
	result := c.Send(context.Background(), ContactMethodSend{Purpose: "subscribed_alert", Severity: "info", Text: "Information", Subject: "Info", PublicURL: r.a.WebURL, Settings: settings.JSON})
	if result.State != "uncertain" || !result.Retryable {
		t.Fatalf("unknown Info: %+v", result)
	}
}

func TestPushoverMuteCancelsEmergencyAndDoesNotSendNewSOS(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	r.sos(t, "Database down")
	r.dispatch(t)
	if _, err := r.a.SetTeamMute(context.Background(), r.principal, r.a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	r.poll(t)
	if !r.fake.PushoverReceipts()[0].Cancelled {
		t.Fatal("team mute left an emergency ringing")
	}
	before := len(r.fake.PushoverMessages())
	r.sos(t, "Disk full")
	r.dispatch(t)
	if len(r.fake.PushoverMessages()) != before {
		t.Fatal("team mute did not stop Pushover")
	}
}

type pushoverSendBarrier struct {
	next          http.RoundTripper
	sent, release chan struct{}
}

func (c pushoverSendBarrier) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := c.next.RoundTrip(r)
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages.json") {
		close(c.sent)
		<-c.release
	}
	return response, err
}
func TestPushoverInFlightCombinedMessageKeepsItsCountedIncidents(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	first := r.sos(t, "Database down")
	second := r.sos(t, "Disk full")
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	sent, release := make(chan struct{}), make(chan struct{})
	c.Client = &http.Client{Transport: pushoverSendBarrier{http.DefaultTransport, sent, release}}
	done := make(chan error, 1)
	go func() { _, err := r.a.DispatchOne(context.Background()); done <- err }()
	<-sent
	i, err := r.a.GetIncident(context.Background(), r.principal, first)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	_, err = r.a.ResolveIncident(context.Background(), r.principal, first, i.Revision, "")
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	c.Client = nil
	messages := r.fake.PushoverMessages()
	emergency := messages[len(messages)-1]
	if err = r.fake.PushoverAcknowledge(emergency.Receipt); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.poll(t)
	r.dispatch(t)
	r.status(t, first, "resolved")
	r.status(t, second, "claimed")
	var found bool
	for _, message := range r.fake.PushoverMessages() {
		if message.Priority == -1 && strings.Contains(message.Message, "This incident is already resolved.") && strings.Contains(message.Message, "You claimed Disk full.") {
			found = true
		}
	}
	if !found {
		t.Fatal("in-flight acknowledgement lost or changed the counted incidents")
	}
}

func TestPushoverFractionalClockNeverPollsEarly(t *testing.T) {
	r := newPushoverRig(t)
	clock := r.a.Now
	r.a.Now = func() time.Time { return clock().Add(900 * time.Millisecond) }
	r.fake.SetPushoverClock(r.a.Now)
	if _, err := r.a.SaveContactMethod(context.Background(), r.principal, ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}, Subscriptions: []string{"sos"}}); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	r.sos(t, "Database down")
	r.dispatch(t)
	r.advance(4 * time.Second)
	r.poll(t)
	if len(r.fake.PushoverReceipts()[0].Polls) != 0 {
		t.Fatal("rounded the deadline down below five seconds")
	}
	r.advance(time.Second)
	r.poll(t)
	if len(r.fake.PushoverReceipts()[0].Polls) != 0 {
		t.Fatal("polled before the rounded-up persisted deadline")
	}
	r.advance(time.Second)
	r.poll(t)
	if len(r.fake.PushoverReceipts()[0].Polls) != 1 {
		t.Fatal("did not poll after the rounded-up deadline")
	}
}

func TestPushoverClosedReceiptsMakeNoIdleRequestsOrWrites(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	ctx := context.Background()
	r.sos(t, "Database down")
	r.dispatch(t)
	if err := r.fake.PushoverAcknowledge(r.fake.PushoverReceipts()[0].Receipt); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.poll(t)
	r.a.backgroundRound(ctx, func(err error) { t.Error(err) })
	var before, after int
	if err := r.a.Store.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	polls, messages := len(r.fake.PushoverReceipts()[0].Polls), len(r.fake.PushoverMessages())
	for range 600 {
		r.advance(time.Second)
		r.a.backgroundRound(ctx, func(err error) { t.Error(err) })
	}
	if err := r.a.Store.db.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after || len(r.fake.PushoverReceipts()[0].Polls) != polls || len(r.fake.PushoverMessages()) != messages {
		t.Fatalf("closed receipt kept working: writes %d -> %d, receipts %+v", before, after, r.fake.PushoverReceipts())
	}
}

type pushoverCancelFailure struct {
	next  http.RoundTripper
	calls *int
	fail  *bool
}

func (c pushoverCancelFailure) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/cancel.json") {
		*c.calls = *c.calls + 1
		if *c.fail {
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":0,"errors":["temporarily unavailable"]}`)), Request: r}, nil
		}
	}
	return c.next.RoundTrip(r)
}
func TestPushoverFailedCancellationWaitsForPersistedRetryDeadline(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	id := r.sos(t, "Database down")
	r.dispatch(t)
	i, err := r.a.GetIncident(context.Background(), r.principal, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.principal, id, i.Revision, ""); err != nil {
		t.Fatal(err)
	}
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	calls, fail := 0, true
	c.Client = &http.Client{Transport: pushoverCancelFailure{http.DefaultTransport, &calls, &fail}}
	if _, err = r.a.ProcessPushoverReceipt(context.Background()); err == nil {
		t.Fatal("cancellation failure was hidden")
	}
	for range 4 {
		r.advance(time.Second)
		r.poll(t)
	}
	if calls != 1 {
		t.Fatalf("cancel retried too early: %d requests", calls)
	}
	fail = false
	r.advance(time.Second)
	r.poll(t)
	if calls != 2 || !r.fake.PushoverReceipts()[0].Cancelled {
		t.Fatal("cancel was not retried at five seconds")
	}
}

func TestPushoverSubscribedWarningInfoAndResolutionUseOrdinaryPriorities(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	ctx := context.Background()
	for severity, priority := range map[string]int{"warn": 0, "info": -1} {
		before := len(r.fake.PushoverMessages())
		err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			return RecordSystemAlert(ctx, tx, "mmhotline", "", severity, "Notification priority", "An ordinary notification", r.a.Now())
		})
		if err != nil {
			t.Fatal(err)
		}
		r.advance(time.Minute)
		r.dispatch(t)
		messages := r.fake.PushoverMessages()
		if len(messages) != before+1 || messages[len(messages)-1].Priority != priority {
			t.Fatalf("%s priority: %+v", severity, messages)
		}
	}
	id := r.sos(t, "Database down")
	r.dispatch(t)
	i, err := r.a.GetIncident(ctx, r.principal, id)
	if err != nil {
		t.Fatal(err)
	}
	before := len(r.fake.PushoverMessages())
	if _, err = r.a.ResolveIncident(ctx, r.principal, id, i.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	r.dispatch(t)
	messages := r.fake.PushoverMessages()
	if len(messages) != before+1 || messages[len(messages)-1].Priority != -1 {
		t.Fatalf("resolved SOS priority: %+v", messages)
	}
}

func TestPushoverRestartNeverResendsAnUnfinishedEmergency(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	ctx := context.Background()
	id := r.sos(t, "Database down")
	d, work, err := r.a.reserveDelivery(ctx, r.a.WebURL, []string{"pushover_notification"})
	if err != nil || !work || d.ID == "" {
		t.Fatalf("reservation: %+v %v", d, err)
	}
	restarted := New(r.a.Store)
	restarted.Now = r.a.Now
	restarted.WebURL = r.a.WebURL
	if err = restarted.LoadSendingServiceSetups(ctx, nil, r.a.fakeSendingServicesURL); err != nil {
		t.Fatal(err)
	}
	r.a = restarted
	r.dispatch(t)
	recorded, err := r.a.DeliveryByID(ctx, r.principal, d.ID)
	if err != nil || recorded.State != "uncertain" || !recorded.NextAttempt.IsZero() {
		t.Fatalf("restart resent unfinished SOS: %+v %v", recorded, err)
	}
	if len(r.fake.PushoverMessages()) != 1 {
		t.Fatal("restart sent the old emergency")
	}
	r.advance(time.Minute)
	if err = r.a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	r.status(t, id, "open")
	if len(r.fake.PushoverMessages()) != 2 {
		t.Fatal("restart prevented the next repeat")
	}
}

// A Pushover send whose worker is gone, after a restart or when its
// reservation runs out, has an unknown outcome. An SOS emergency may already
// be ringing, so it is never sent again. Any other Pushover message, a
// contact method test among them, is sent again.
func TestPushoverUnfinishedSendIsResentOnlyWhenItIsNoEmergency(t *testing.T) {
	for _, restart := range []bool{true, false} {
		for _, message := range []string{"sos", "contact_method_test", "warn", "info"} {
			name := message + " after its reservation runs out"
			if restart {
				name = message + " after a restart"
			}
			t.Run(name, func(t *testing.T) {
				r := newPushoverRig(t)
				ctx := context.Background()
				if message == "contact_method_test" {
					if _, err := r.a.SaveContactMethod(ctx, r.principal, ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}, Subscriptions: []string{"sos", "warn", "info"}}); err != nil {
						t.Fatal(err)
					}
				} else {
					r.add(t, fakesendingservices.PushoverUserKey)
				}
				switch message {
				case "sos":
					r.sos(t, "Database down")
				case "warn", "info":
					queueDelivery(t, r.a, r.principal, "unfinished", message)
				}
				d, work, err := r.a.reserveDelivery(ctx, r.a.WebURL, []string{"pushover_notification"})
				if err != nil || !work || d.ID == "" {
					t.Fatalf("reservation: %+v %v", d, err)
				}
				before := len(r.fake.PushoverMessages())
				if restart {
					r.advance(10 * time.Second)
					restarted := New(r.a.Store)
					restarted.Now = r.a.Now
					restarted.WebURL = r.a.WebURL
					if err = restarted.LoadSendingServiceSetups(ctx, nil, r.a.fakeSendingServicesURL); err != nil {
						t.Fatal(err)
					}
					r.a = restarted
				} else {
					r.advance(d.ReservedUntil.Sub(r.a.Now()) + time.Second)
				}
				r.dispatch(t)
				recorded, err := r.a.DeliveryByID(ctx, r.principal, d.ID)
				if err != nil {
					t.Fatal(err)
				}
				sent := len(r.fake.PushoverMessages()) - before
				if message == "sos" {
					if sent != 0 || recorded.State != "uncertain" || !recorded.NextAttempt.IsZero() {
						t.Fatalf("emergency sent again %d times: %+v", sent, recorded)
					}
					return
				}
				if sent != 1 || recorded.State != "accepted" || recorded.Attempts != 2 {
					t.Fatalf("ordinary message sent again %d times: %+v", sent, recorded)
				}
			})
		}
	}
}

// An SOS escalation that stopped being valid after its reservation is
// cancelled, not suppressed, even when the person's or the team's controls
// would also suppress it: the escalation round is over.
func TestReservedSOSEscalationThatStoppedBeingValidIsCancelled(t *testing.T) {
	for _, change := range []string{"team mute", "personal unavailability"} {
		t.Run(change, func(t *testing.T) {
			r := newPushoverRig(t)
			ctx := context.Background()
			r.add(t, fakesendingservices.PushoverUserKey)
			r.sos(t, "Database down")
			d, work, err := r.a.reserveDelivery(ctx, r.a.WebURL, []string{"pushover_notification"})
			if err != nil || !work || d.Purpose != "sos_escalation" {
				t.Fatalf("reservation: %+v %v", d, err)
			}
			want := "team mute"
			if change == "team mute" {
				_, err = r.a.SetTeamMute(ctx, r.principal, r.a.Now().Add(time.Hour), 0)
			} else {
				want = "unavailable"
				_, err = r.a.SetPersonalAvailability(ctx, r.principal, "unavailable", r.a.Now().Add(time.Hour), 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := len(r.fake.PushoverMessages())
			if err = r.a.dispatchReserved(ctx, d, r.a.configuredContactMethodTypes()["pushover_notification"]); err != nil {
				t.Fatal(err)
			}
			recorded, err := r.a.DeliveryByID(ctx, r.principal, d.ID)
			if err != nil || recorded.State != "cancelled" || recorded.Reason != want || len(r.fake.PushoverMessages()) != before {
				t.Fatalf("escalation after %s: %+v %v", change, recorded, err)
			}
		})
	}
}

// Each reason a delivery is no longer wanted also stops an emergency that
// already rings.
func TestPushoverEmergencyStopsWhenTheContactMethodNoLongerWantsIt(t *testing.T) {
	for _, change := range []string{"contact method removed", "contact method changed", "personal unavailability"} {
		t.Run(change, func(t *testing.T) {
			r := newPushoverRig(t)
			ctx := context.Background()
			c := r.add(t, fakesendingservices.PushoverUserKey)
			r.sos(t, "Database down")
			r.dispatch(t)
			var err error
			switch change {
			case "contact method removed":
				err = r.a.RemoveContactMethod(ctx, r.principal, c.ID, c.Revision)
			case "contact method changed":
				_, err = r.a.SaveContactMethod(ctx, r.principal, ContactMethodInput{ID: c.ID, ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverOtherUserKey}, ExpectedRevision: c.Revision, Subscriptions: c.Subscriptions})
			case "personal unavailability":
				_, err = r.a.SetPersonalAvailability(ctx, r.principal, "unavailable", r.a.Now().Add(time.Hour), 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			r.poll(t)
			if receipts := r.fake.PushoverReceipts(); len(receipts) < 1 || !receipts[0].Cancelled {
				t.Fatalf("emergency still rings: %+v", receipts)
			}
		})
	}
}

func TestPushoverSetupAndContactThroughTheBrowserOperations(t *testing.T) {
	h := newWorkflowRig(t)
	h.app.WebURL = "https://mmhotline.example.com"
	browser := h.browser()
	browser.addPerson("barbara@example.test", "Barbara")
	fake := fakesendingservices.New()
	fake.SetPushoverClock(h.app.Now)
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := h.app.LoadSendingServiceSetups(context.Background(), h.app.configuredContactMethodTypes(), server.URL); err != nil {
		t.Fatal(err)
	}
	page := browser.get("/contact-methods", 200)
	if !strings.Contains(page, `href="/sending-services/pushover"`) {
		t.Fatal("missing Pushover setup link")
	}
	setup := browser.get("/sending-services/pushover", 200)
	if !strings.Contains(setup, "https://pushover.net/apps/build") || !strings.Contains(settingsPageText(setup), "sending_services.pushover.application_token") {
		t.Fatal("missing setup guide or the file key")
	}
	h.app.UseSettingsFile(InstalledSettingsFilePath, nil)
	if _, err := h.app.SetUpSendingService(context.Background(), h.app.settingsPrincipal(), "pushover", map[string]string{"pushover_application_token": fakesendingservices.PushoverApplicationToken}); err != nil {
		t.Fatal(err)
	}
	page = browser.get("/contact-methods", 200)
	if !strings.Contains(page, "Add Pushover") {
		t.Fatal("configured Pushover form missing")
	}
	browser.post("/contact-methods/save", url.Values{"csrf": {workflowField(t, page, "csrf")}, "contact_method_type": {"pushover_notification"}, "pushover_user_key": {fakesendingservices.PushoverUserKey}, "severity": {"sos"}}, 303)
	if _, err := h.app.DispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages := fake.PushoverMessages()
	if len(messages) != 1 || messages[0].Priority != 0 || messages[0].Receipt != "" {
		t.Fatalf("browser save test: %+v", messages)
	}
	page = browser.get("/contact-methods", 200)
	if !strings.Contains(page, "Accepted by Pushover") || !strings.Contains(page, "Team mute always stops this contact method.") || strings.Contains(page, "Download the MMHotline contact card") {
		t.Fatal("Pushover card confused waking behavior with phone setup")
	}
	if strings.Contains(browser.get("/sending-services/pushover", 200), fakesendingservices.PushoverApplicationToken) {
		t.Fatal("setup page exposed saved token")
	}
}

type pushoverReceiptFailure struct {
	next     http.RoundTripper
	requests *int
}

func (c pushoverReceiptFailure) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/receipts/") {
		*c.requests = *c.requests + 1
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":0,"errors":["temporarily unavailable"]}`)), Request: r}, nil
	}
	return c.next.RoundTrip(r)
}
func TestPushoverExpiredReceiptStopsPollingDuringSendingServiceOutage(t *testing.T) {
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	r.sos(t, "Database down")
	r.dispatch(t)
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	requests := 0
	c.Client = &http.Client{Transport: pushoverReceiptFailure{http.DefaultTransport, &requests}}
	r.advance(3 * time.Hour)
	if _, err := r.a.ProcessPushoverReceipt(context.Background()); err == nil {
		t.Fatal("sending service outage was hidden")
	}
	r.advance(5 * time.Second)
	r.poll(t)
	if requests != 1 {
		t.Fatal("kept requesting an expired receipt")
	}
}

// A new application token, such as after a person who knew the old one
// leaves, keeps every Pushover contact method; the MMHotline-down alarm's held messages
// and the next test send through it. It waits while an emergency is open,
// which only the old token can read and cancel.
func TestPushoverApplicationTokenReplacementKeepsContactMethods(t *testing.T) {
	r := newPushoverRig(t)
	ctx := context.Background()
	method := r.add(t, fakesendingservices.PushoverUserKey)
	incident := r.sos(t, "Database down")
	r.dispatch(t)
	sent := r.fake.PushoverMessages()
	emergency := sent[len(sent)-1]
	replacement := map[string]string{"pushover_application_token": fakesendingservices.PushoverReplacementApplicationToken}
	if _, err := r.a.SetUpSendingService(ctx, r.principal, "pushover", replacement); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "close the open Pushover emergency messages") {
		t.Fatalf("replacement with an open emergency: %v", err)
	}
	if err := r.fake.PushoverAcknowledge(emergency.Receipt); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	r.poll(t)
	r.dispatch(t)
	r.status(t, incident, "claimed")

	result, err := r.a.SetUpSendingService(ctx, r.principal, "pushover", replacement)
	if err != nil || !strings.Contains(result.NextStep, "contact-methods send-test") {
		t.Fatalf("replacement: %+v, %v", result, err)
	}
	methods, err := r.a.ContactMethods(ctx, r.principal)
	if err != nil || len(methods) != 1 || methods[0].ID != method.ID {
		t.Fatalf("contact methods after the replacement: %+v, %v", methods, err)
	}
	var held []downAlarmHeld
	if err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		held, _, err = r.a.downAlarmHeldMessages(ctx, tx, r.a.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(heldMessagesOf(held))
	if len(held) != 1 || !strings.Contains(string(encoded), fakesendingservices.PushoverReplacementApplicationToken) || strings.Contains(string(encoded), fakesendingservices.PushoverApplicationToken) {
		t.Fatalf("MMHotline-down alarm held messages after the replacement: %s", encoded)
	}

	r.fake.DeletePushoverApplication(fakesendingservices.PushoverApplicationToken)
	before := len(r.fake.PushoverMessages())
	r.advance(time.Minute)
	if err = r.a.SendContactMethodTest(ctx, r.principal, method.ID); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	if sent = r.fake.PushoverMessages(); len(sent) != before+1 || sent[before].Priority != 0 {
		t.Fatalf("test after deleting the old application: %+v", sent[before:])
	}
}

// A combined message lists whole alert titles while they fit in Pushover's
// 1,024 characters, then counts the rest, as a combined email does.
func TestPushoverCombinedMessageListsWholeTitlesAndCountsTheRest(t *testing.T) {
	r := newPushoverRig(t)
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	settings, _ := c.CheckSettings(context.Background(), ContactMethodSettingsInput{Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}})
	for _, severity := range []string{"warn", "sos"} {
		var alerts []CombinedAlert
		for i := range 60 {
			alerts = append(alerts, CombinedAlert{Severity: severity, Title: fmt.Sprintf("Disk 91%% full on app%03d, the logs volume", i), AlertID: NewID(), IncidentID: NewID()})
		}
		m := ContactMethodSend{Purpose: "subscribed_alert", Severity: severity, Subject: "[MMHotline] 60 new alerts", Title: alerts[0].Title, CombinedAlerts: alerts, IncidentID: alerts[0].IncidentID, PublicURL: r.a.WebURL, Settings: settings.JSON}
		if result := c.Send(context.Background(), m); result.State != "accepted" {
			t.Fatalf("%s: %+v", severity, result)
		}
		sent := r.fake.PushoverMessages()
		message := sent[len(sent)-1].Message
		if utf8.RuneCountInString(message) > 1024 {
			t.Fatalf("%s: %d characters", severity, utf8.RuneCountInString(message))
		}
		list, _, _ := strings.Cut(message, "\n\n")
		lines := strings.Split(list, "\n")
		var listed, more int
		if _, err := fmt.Sscanf(lines[len(lines)-1], "And %d more.", &more); err != nil {
			t.Fatalf("%s: the list does not end with the count of the rest: %q", severity, list)
		}
		for _, line := range lines[:len(lines)-1] {
			if !strings.HasSuffix(line, "the logs volume") {
				t.Fatalf("%s: a title was cut: %q", severity, line)
			}
			listed++
		}
		if listed+more != 60 || listed == 0 {
			t.Fatalf("%s: %d listed and %d more, for 60 alerts", severity, listed, more)
		}
	}
}

func TestPushoverSingleAlertDoesNotRepeatTitle(t *testing.T) {
	r := newPushoverRig(t)
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	settings, _ := c.CheckSettings(context.Background(), ContactMethodSettingsInput{Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}})
	for _, severity := range []string{"sos", "warn", "info"} {
		for _, body := range []string{"The service needs attention.", ""} {
			t.Run(severity+"/"+body, func(t *testing.T) {
				title := "Service stopped"
				m := ContactMethodSend{Purpose: "subscribed_alert", Severity: severity, Subject: "[" + severityLabel(severity) + "] " + title, Title: title, AlertMessage: body, Text: title, Settings: settings.JSON, PublicURL: r.a.WebURL}
				if result := c.Send(context.Background(), m); result.State != "accepted" {
					t.Fatal(result)
				}
				messages := r.fake.PushoverMessages()
				sent := messages[len(messages)-1]
				if sent.Title != m.Subject || strings.Contains(sent.Message, title) || strings.TrimSpace(sent.Message) == "" || (body != "" && !strings.HasPrefix(sent.Message, body)) {
					t.Fatalf("sent: %+v", sent)
				}
			})
		}
	}
}

// Every member reads the alert history, so the notice to administrators about
// a member's failing contact method names it by its owner. A Pushover user key
// would let anyone with a Pushover application send that member notifications.
func TestFailingPersonalContactMethodNoticeNamesTheOwnerNotTheDestination(t *testing.T) {
	r := newPushoverRig(t)
	c := r.add(t, fakesendingservices.PushoverUserKey)
	ctx := context.Background()
	var owner string
	if err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for range failuresBeforeKeepingOnlyNewest {
			if err := recordContactMethodOutcome(ctx, tx, Delivery{ContactMethodID: c.ID, ContactMethodRevision: c.Revision}, false, r.a.Now()); err != nil {
				return err
			}
		}
		var err error
		owner, err = incidentMemberName(ctx, tx, c.MemberID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var title, message string
	if err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT title,message FROM alerts WHERE source_id='system' AND title LIKE 'MMHotline cannot deliver alerts to %'`).Scan(&title, &message)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(title+message, fakesendingservices.PushoverUserKey) {
		t.Fatalf("the notice shows the member's Pushover user key: %q %q", title, message)
	}
	if want := "MMHotline cannot deliver alerts to " + owner + "'s Pushover notification"; title != want {
		t.Fatalf("title %q, want %q", title, want)
	}
}
