package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBurstBecomesOneCombinedMessage(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "burst@example.test")
	savedEmail(t, a, s.Principal, true)
	for n := range 5 {
		queueDelivery(t, a, s.Principal, fmt.Sprintf("burst-%d", n), "warn")
	}
	sender := &recordingEmailSender{}
	for range 5 {
		if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
			t.Fatal(err)
		}
	}
	messages := sender.messages()
	if len(messages) != 1 || messages[0].Subject != "[MMHotline] 5 new alerts" || len(messages[0].CombinedAlerts) != 5 || !strings.Contains(messages[0].Text, "See all: https://mmhotline.example.com/alerts") {
		t.Fatalf("combined message: %+v", messages)
	}
	if d := findDelivery(t, a, s.Principal, "burst-0"); d.State != "accepted" || d.CombinedInto != "" {
		t.Fatalf("carrier: %+v", d)
	}
	for n := 1; n < 5; n++ {
		d := findDelivery(t, a, s.Principal, fmt.Sprintf("burst-%d", n))
		if d.State != "combined" || d.Reason != "Sent together with 4 other alerts" || d.OutcomeLabel() != "Sent together with other alerts" {
			t.Fatalf("joined %d: %+v", n, d)
		}
	}
}

func TestThrottleWaitsOneMinuteThenTenAfterTenMessages(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "slow@example.test")
	savedEmail(t, a, s.Principal, true)
	sender := &recordingEmailSender{}
	send := func() {
		t.Helper()
		if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
			t.Fatal(err)
		}
	}
	queueDelivery(t, a, s.Principal, "first", "info")
	send()
	advance(10 * time.Second)
	queueDelivery(t, a, s.Principal, "second", "info")
	d := findDelivery(t, a, s.Principal, "second")
	sentAt := findDelivery(t, a, s.Principal, "first").FirstAttempt
	if d.State != "queued" || !d.NextAttempt.Equal(sentAt.Add(time.Minute)) || !strings.HasPrefix(d.Reason, "Waiting until ") {
		t.Fatalf("second message did not wait: %+v", d)
	}
	queueDelivery(t, a, s.Principal, "third", "resolved-info")
	if d = findDelivery(t, a, s.Principal, "third"); d.State != "combined" {
		t.Fatalf("third did not join the waiting message: %+v", d)
	}
	send()
	if len(sender.messages()) != 1 {
		t.Fatal("the waiting message went early")
	}
	advance(50 * time.Second)
	send()
	if messages := sender.messages(); len(messages) != 2 || messages[1].Subject != "[MMHotline] 2 new alerts" {
		t.Fatalf("after one minute: %+v", messages)
	}
	// Eight more single messages, one a minute, make 10 in the hour.
	for n := range 8 {
		advance(time.Minute)
		queueDelivery(t, a, s.Principal, fmt.Sprintf("minute-%d", n), "info")
		send()
	}
	if len(sender.messages()) != 10 {
		t.Fatalf("messages: %d", len(sender.messages()))
	}
	advance(time.Minute)
	queueDelivery(t, a, s.Principal, "slowed", "info")
	last := findDelivery(t, a, s.Principal, "minute-7").FirstAttempt
	if d = findDelivery(t, a, s.Principal, "slowed"); !d.NextAttempt.Equal(last.Add(10 * time.Minute)) {
		t.Fatalf("not slowed to one every 10 minutes: %+v (last sent %s)", d, last)
	}
}

func TestSOSIsNeverDelayedOrCombined(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "sos-fast@example.test")
	savedEmail(t, a, s.Principal, true)
	sender := &recordingEmailSender{}
	queueDelivery(t, a, s.Principal, "warning", "warn")
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	queueDelivery(t, a, s.Principal, "waiting-warning", "warn")
	for _, key := range []string{"sos-1", "sos-2"} {
		queueDelivery(t, a, s.Principal, key, "sos")
		if d := findDelivery(t, a, s.Principal, key); d.State != "queued" || d.CombinedInto != "" || !d.NextAttempt.Equal(a.Now()) {
			t.Fatalf("%s was delayed or combined: %+v", key, d)
		}
	}
	for range 3 {
		if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
			t.Fatal(err)
		}
	}
	if messages := sender.messages(); len(messages) != 3 || !strings.HasPrefix(messages[1].Subject, "[SOS]") || !strings.HasPrefix(messages[2].Subject, "[SOS]") {
		t.Fatalf("SOS messages: %+v", messages)
	}
}

func TestCombinedMessageIsFrozenAfterItsFirstAttempt(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "frozen@example.test")
	savedEmail(t, a, s.Principal, true)
	queueDelivery(t, a, s.Principal, "one", "warn")
	queueDelivery(t, a, s.Principal, "two", "warn")
	sender := &recordingEmailSender{Results: []DeliveryResult{{State: "uncertain", Retryable: true}}}
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	queueDelivery(t, a, s.Principal, "three", "warn")
	if d := findDelivery(t, a, s.Principal, "three"); d.State != "queued" || d.CombinedInto != "" {
		t.Fatalf("joined a message that was already attempted: %+v", d)
	}
	advance(5 * time.Second)
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	messages := sender.messages()
	if len(messages) != 2 || messages[0].Text != messages[1].Text || messages[0].Subject != "[MMHotline] 2 new alerts" {
		t.Fatalf("retry changed the combined message: %+v", messages)
	}
}

func TestWebhookGetsEveryAlertUpToSixtyAMinute(t *testing.T) {
	t.Parallel()
	r := newWebhookRig(t, "warn", "sos")
	for n := range webhookMaximumAlertsPerMinute {
		queueDelivery(t, r.a, r.s.Principal, fmt.Sprintf("w%d", n), "warn")
	}
	if d := findDelivery(t, r.a, r.s.Principal, "w59"); d.State != "queued" || d.CombinedInto != "" {
		t.Fatalf("webhooks are not combined: %+v", d)
	}
	queueDelivery(t, r.a, r.s.Principal, "one-too-many", "warn")
	if d := findDelivery(t, r.a, r.s.Principal, "one-too-many"); d.State != "suppressed" || d.Reason != "Not sent: this webhook got more than 60 alerts in one minute" {
		t.Fatalf("61st alert in a minute: %+v", d)
	}
	queueDelivery(t, r.a, r.s.Principal, "sos", "sos")
	if d := findDelivery(t, r.a, r.s.Principal, "sos"); d.State != "queued" {
		t.Fatalf("SOS skipped: %+v", d)
	}
	r.advance(time.Minute)
	queueDelivery(t, r.a, r.s.Principal, "next-minute", "warn")
	if d := findDelivery(t, r.a, r.s.Principal, "next-minute"); d.State != "queued" {
		t.Fatalf("next minute: %+v", d)
	}
}

func TestContactMethodTestsAreNeverThrottled(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "tests@example.test")
	c := savedEmail(t, a, s.Principal, true)
	advance(time.Minute)
	queueDelivery(t, a, s.Principal, "ordinary", "info")
	if _, err := dispatchWith(a, "https://mmhotline.example.com", &recordingEmailSender{}); err != nil {
		t.Fatal(err)
	}
	advance(time.Second)
	if err := a.SendContactMethodTest(context.Background(), s.Principal, c.ID); err != nil {
		t.Fatal(err)
	}
	deliveries, err := a.Deliveries(context.Background(), s.Principal, 10)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries[0].Purpose != "contact_method_test" || deliveries[0].State != "queued" || !deliveries[0].NextAttempt.Equal(a.Now()) {
		t.Fatalf("test delivery throttled: %+v", deliveries[0])
	}
}

// joinersOf checks that the messages combined into an unsent message no longer
// read as sent together, and say what happened to them instead.
func joinersOf(t *testing.T, a *App, p Principal, keys []string, state, reason, label string) {
	t.Helper()
	for _, key := range keys {
		d := findDelivery(t, a, p, key)
		if d.State != state || d.Reason != reason || d.OutcomeLabel() != label || d.CombinedInto == "" || !d.NextAttempt.IsZero() {
			t.Errorf("%s: %+v (outcome %q)", key, d, d.OutcomeLabel())
		}
	}
}

// When a combined email fails for good, the messages combined into it failed
// with it: nothing was sent together.
func TestCombinedMessagesFailWithTheEmailThatCarriedThem(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "combined-failed@example.test")
	savedEmail(t, a, s.Principal, true)
	for _, key := range []string{"carrier", "joined-1", "joined-2"} {
		queueDelivery(t, a, s.Principal, key, "warn")
	}
	sender := &recordingEmailSender{Results: []DeliveryResult{{State: "failed", Diagnostic: "550 mailbox unavailable"}}}
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	if d := findDelivery(t, a, s.Principal, "carrier"); d.State != "failed" || !d.NextAttempt.IsZero() {
		t.Fatalf("carrier: %+v", d)
	}
	joinersOf(t, a, s.Principal, []string{"joined-1", "joined-2"}, "failed", "Not sent: the message it was combined into failed: 550 mailbox unavailable", "Failed, no more retries")
}

// A combined email that is suppressed before it goes (the team is muted)
// suppresses the messages combined into it.
func TestCombinedMessagesAreSuppressedWithTheirMessage(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "combined-suppressed@example.test")
	savedEmail(t, a, s.Principal, false)
	for _, key := range []string{"carrier", "joined"} {
		queueDelivery(t, a, s.Principal, key, "warn")
	}
	if _, err := a.SetTeamMute(context.Background(), s.Principal, a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	sender := &recordingEmailSender{}
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	if d := findDelivery(t, a, s.Principal, "carrier"); d.State != "suppressed" || len(sender.messages()) != 0 {
		t.Fatalf("carrier: %+v", d)
	}
	joinersOf(t, a, s.Principal, []string{"joined"}, "suppressed", "Not sent: the message it was combined into was suppressed: the team is muted", "Suppressed")
}

// Removing a contact method cancels its waiting combined message and the
// messages combined into it.
func TestRemovingAContactMethodCancelsItsCombinedMessages(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "combined-removed@example.test")
	c := savedEmail(t, a, s.Principal, true)
	for _, key := range []string{"carrier", "joined"} {
		queueDelivery(t, a, s.Principal, key, "warn")
	}
	if err := a.RemoveContactMethod(context.Background(), s.Principal, c.ID, c.Revision); err != nil {
		t.Fatal(err)
	}
	if d := findDelivery(t, a, s.Principal, "carrier"); d.State != "cancelled" {
		t.Fatalf("carrier: %+v", d)
	}
	joinersOf(t, a, s.Principal, []string{"joined"}, "cancelled", "Not sent: the message it was combined into was cancelled: contact method removed", "Cancelled")
}

// Changing a contact method's subscriptions or availability preference keeps
// the messages already queued for it, a combined message and the messages
// combined into it included: only a new destination cancels them. A severity
// the person unticks keeps the messages already queued for it, so nothing
// the person was about to get is lost.
func TestChangingSubscriptionsKeepsQueuedMessages(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "combined-subscriptions@example.test")
	c := savedEmail(t, a, s.Principal, true)
	for _, key := range []string{"carrier", "joined"} {
		queueDelivery(t, a, s.Principal, key, "warn")
	}
	saved, err := a.SaveContactMethod(context.Background(), s.Principal, ContactMethodInput{ID: c.ID, ContactMethodType: c.ContactMethodType, Fields: map[string]string{"email_address": c.SentTo}, ExpectedRevision: c.Revision, SendWhileUnavailableOrMuted: false, Subscriptions: []string{"sos"}})
	if err != nil || saved.Revision == c.Revision {
		t.Fatalf("subscription change: %+v %v", saved, err)
	}
	if d := findDelivery(t, a, s.Principal, "carrier"); d.State != "queued" {
		t.Fatalf("carrier: %+v", d)
	}
	sender := &recordingEmailSender{}
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	// One message carries both warnings: the joined one was not dropped.
	if messages := sender.messages(); len(messages) != 1 || messages[0].Subject != "[MMHotline] 2 new alerts" {
		t.Fatalf("combined message after the change: %+v", messages)
	}
}
