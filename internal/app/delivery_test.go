package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dispatchWith sends through one test contact method type, or through none when contact method type
// is nil, so tests control exactly what can send.
func dispatchWith(a *App, publicURL string, contactMethodType ContactMethodType) (bool, error) {
	a.WebURL = publicURL
	a.ContactMethodTypes = nil
	if contactMethodType != nil {
		a.ContactMethodTypes = map[string]ContactMethodType{contactMethodType.Name(): contactMethodType}
	}
	return a.DispatchOne(context.Background())
}

// testWakingContactMethodType stands in for a contact method type that wakes people (like phone
// calls) until a real one exists. It never sends.
type testWakingContactMethodType struct{}

func (testWakingContactMethodType) Name() string { return "test_waking" }
func (testWakingContactMethodType) Facts() ContactMethodTypeFacts {
	return ContactMethodTypeFacts{CardTitle: "Test waking contact method type", WakesPeople: true, CombinesMessages: true}
}
func (testWakingContactMethodType) CheckSettings(context.Context, ContactMethodSettingsInput) (ContactMethodSettings, error) {
	return ContactMethodSettings{JSON: "{}", SentTo: "test waking contact method type"}, nil
}
func (testWakingContactMethodType) Send(context.Context, ContactMethodSend) DeliveryResult {
	return DeliveryResult{State: "failed", Diagnostic: "test contact method type never sends"}
}

func init() { contactMethodTypes = append(contactMethodTypes, testWakingContactMethodType{}) }

type recordingEmailSender struct {
	EmailContactMethodType
	mu       sync.Mutex
	Messages []ContactMethodSend
	Results  []DeliveryResult
}

func (s *recordingEmailSender) Send(_ context.Context, m ContactMethodSend) DeliveryResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, m)
	n := len(s.Messages) - 1
	if n < len(s.Results) {
		return s.Results[n]
	}
	return DeliveryResult{State: "accepted", IDFromSendingService: "mail-id"}
}
func (s *recordingEmailSender) messages() []ContactMethodSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ContactMethodSend(nil), s.Messages...)
}
func deliveryTestClock(a *App) func(time.Duration) {
	var timestamp atomic.Int64
	timestamp.Store(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC).Unix())
	a.Now = func() time.Time { return time.Unix(timestamp.Load(), 0) }
	return func(d time.Duration) { timestamp.Add(int64(d.Seconds())) }
}
func savedEmail(t *testing.T, a *App, p Principal, sendWhileUnavailableOrMuted bool) ContactMethod {
	t.Helper()
	c, err := a.SaveContactMethod(context.Background(), p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "sent_to@example.test"}, SendWhileUnavailableOrMuted: sendWhileUnavailableOrMuted, Subscriptions: []string{"info", "warn", "sos"}})
	if err != nil {
		t.Fatal(err)
	}
	sender := &recordingEmailSender{}
	if work, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil || !work {
		t.Fatalf("save test: %v %v", work, err)
	}
	return c
}

func queueDelivery(t *testing.T, a *App, p Principal, key, severity string) {
	t.Helper()
	err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		return QueueNotification(context.Background(), tx, Notification{Key: key, Severity: severity, Title: "Disk pressure", Message: "Investigate the worker", Purpose: "subscribed_alert", At: a.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
}
func findDelivery(t *testing.T, a *App, p Principal, key string) Delivery {
	t.Helper()
	var d Delivery
	err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		d, err = scanDelivery(tx.QueryRowContext(context.Background(), `SELECT `+deliveryColumns+` FROM deliveries WHERE notification_key=?`, key))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeliveryRetryPreservesIdentityAndPayloadAcrossRestart(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "delivery@example.test")
	savedEmail(t, a, s.Principal, true)
	queueDelivery(t, a, s.Principal, "ordinary", "warn")
	sender := &recordingEmailSender{Results: []DeliveryResult{{State: "uncertain", Diagnostic: "response unavailable", Retryable: true, SentFrom: "MMHotline <alerts@example.test>"}, {State: "accepted", IDFromSendingService: "accepted-id"}}}
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	d := findDelivery(t, a, s.Principal, "ordinary")
	if d.State != "uncertain" || d.NextAttempt.IsZero() {
		t.Fatalf("unknown outcome lost: %+v", d)
	}
	if err := a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	a.Store = reopened
	if work, err := dispatchWith(a, "https://new.test", nil); err != nil || work {
		t.Fatalf("disabled sender changed pending work: %v %v", work, err)
	}
	advance(5 * time.Second)
	if _, err = dispatchWith(a, "https://new.test", sender); err != nil {
		t.Fatal(err)
	}
	messages := sender.messages()
	if len(messages) != 2 || messages[0].SentFrom != "" || messages[1].SentFrom != "MMHotline <alerts@example.test>" {
		t.Fatalf("retry did not keep the first sender: %+v", messages)
	}
	messages[1].SentFrom = ""
	// Each attempt has its own delivery attempt ID; everything else stays.
	if messages[0].DeliveryAttemptID == messages[1].DeliveryAttemptID {
		t.Fatal("two attempts share a delivery attempt ID")
	}
	messages[0].DeliveryAttemptID, messages[1].DeliveryAttemptID = "", ""
	if !reflect.DeepEqual(messages[0], messages[1]) {
		t.Fatalf("retry changed identity/payload: %+v", messages)
	}
	d = findDelivery(t, a, s.Principal, "ordinary")
	if d.State != "accepted" || d.Attempts != 2 || !d.NextAttempt.IsZero() {
		t.Fatalf("result: %+v", d)
	}
	attempts, err := a.DeliveryAttempts(context.Background(), s.Principal, d.ID)
	if err != nil || len(attempts) != 2 || attempts[0].State != "uncertain" || attempts[1].State != "accepted" {
		t.Fatalf("attempt evidence: %+v %v", attempts, err)
	}
}

func TestDeliveryRechecksMuteAndContactMethodAfterReservation(t *testing.T) {
	for _, change := range []string{"team mute", "personal unavailability", "contact method changed", "contact method removed"} {
		t.Run(change, func(t *testing.T) {
			a := foundationApp(t)
			deliveryTestClock(a)
			s := memberFixture(t, a, "change@example.test")
			c := savedEmail(t, a, s.Principal, false)
			queueDelivery(t, a, s.Principal, "pending", "info")
			d, work, err := a.reserveDelivery(context.Background(), "https://mmhotline.example.com", []string{"email"})
			if err != nil || !work || d.ID == "" {
				t.Fatalf("reserve %v %v", work, err)
			}
			switch change {
			case "team mute":
				_, err = a.SetTeamMute(context.Background(), s.Principal, a.Now().Add(time.Hour), 0)
			case "personal unavailability":
				_, err = a.SetPersonalAvailability(context.Background(), s.Principal, "unavailable", a.Now().Add(time.Hour), 0)
			case "contact method changed":
				_, err = a.SaveContactMethod(context.Background(), s.Principal, ContactMethodInput{ID: c.ID, ContactMethodType: "email", Fields: map[string]string{"email_address": "changed@example.test"}, ExpectedRevision: c.Revision, Subscriptions: c.Subscriptions})
			case "contact method removed":
				err = a.RemoveContactMethod(context.Background(), s.Principal, c.ID, c.Revision)
			}
			if err != nil {
				t.Fatal(err)
			}
			sender := &recordingEmailSender{}
			if err = a.dispatchReserved(context.Background(), d, sender); err != nil {
				t.Fatal(err)
			}
			if len(sender.messages()) != 0 {
				t.Fatal("stale delivery sent")
			}
			d = findDelivery(t, a, s.Principal, "pending")
			if d.State != "cancelled" && d.State != "suppressed" {
				t.Fatalf("stale work retained: %+v", d)
			}
		})
	}
}

type barrierEmailSender struct {
	EmailContactMethodType
	Started chan ContactMethodSend
	Release chan struct{}
}

func (s *barrierEmailSender) Send(ctx context.Context, m ContactMethodSend) DeliveryResult {
	s.Started <- m
	select {
	case <-s.Release:
		return DeliveryResult{State: "accepted", IDFromSendingService: "actual-old-email_address-send"}
	case <-ctx.Done():
		return DeliveryResult{State: "uncertain", Retryable: true}
	}
}
func TestDeliveryLateCompletionRecordsOriginalAddress(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "inflight@example.test")
	c := savedEmail(t, a, s.Principal, true)
	queueDelivery(t, a, s.Principal, "in-flight", "warn")
	sender := &barrierEmailSender{Started: make(chan ContactMethodSend, 1), Release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := dispatchWith(a, "https://mmhotline.example.com", sender)
		done <- err
	}()
	original := <-sender.Started
	updated, err := a.SaveContactMethod(context.Background(), s.Principal, ContactMethodInput{ID: c.ID, ContactMethodType: "email", Fields: map[string]string{"email_address": "different@example.test"}, ExpectedRevision: c.Revision, SendWhileUnavailableOrMuted: true, Subscriptions: c.Subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	close(sender.Release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	d := findDelivery(t, a, s.Principal, "in-flight")
	if d.State != "accepted" || d.SentTo != original.SentTo || d.SentTo == updated.SentTo {
		t.Fatalf("late result retargeted: %+v", d)
	}
	contactMethods, err := a.ContactMethods(context.Background(), s.Principal)
	if err != nil || len(contactMethods) != 1 || contactMethods[0].SentTo != updated.SentTo || contactMethods[0].LastTest == nil || contactMethods[0].LastTest.State != "queued" {
		t.Fatalf("old result changed new contact method: %+v %v", contactMethods, err)
	}
}

func TestDeliveryExpiredReservationAndRetryHorizon(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "recovery@example.test")
	savedEmail(t, a, s.Principal, true)
	queueDelivery(t, a, s.Principal, "crashed", "info")
	old, _, err := a.reserveDelivery(context.Background(), "https://mmhotline.example.com", []string{"email"})
	if err != nil {
		t.Fatal(err)
	}
	advance(old.ReservedUntil.Sub(a.Now()))
	if work, err := dispatchWith(a, "", nil); err != nil || !work {
		t.Fatalf("recovery without sending service: %v %v", work, err)
	}
	d := findDelivery(t, a, s.Principal, "crashed")
	if d.State != "uncertain" || d.NextAttempt.IsZero() {
		t.Fatalf("lost uncertain work: %+v", d)
	}
	sender := &recordingEmailSender{}
	if _, err = dispatchWith(a, "https://new.test", sender); err != nil {
		t.Fatal(err)
	}
	if err = a.finishDelivery(context.Background(), old, DeliveryResult{State: "failed", Diagnostic: "late old failure"}); err != nil {
		t.Fatal(err)
	}
	d = findDelivery(t, a, s.Principal, "crashed")
	if d.State != "accepted" || d.IDFromSendingService != "mail-id" {
		t.Fatalf("stale completion overwrote retry: %+v", d)
	}
	queueDelivery(t, a, s.Principal, "aged", "warn")
	advance(contactMethodMinimumMessageInterval)
	uncertain := &recordingEmailSender{Results: []DeliveryResult{{State: "uncertain", Retryable: true}}}
	if _, err = dispatchWith(a, "https://mmhotline.example.com", uncertain); err != nil {
		t.Fatal(err)
	}
	advance(retryHorizon)
	if _, err = dispatchWith(a, "https://mmhotline.example.com", uncertain); err != nil {
		t.Fatal(err)
	}
	if len(uncertain.messages()) != 1 {
		t.Fatal("retried beyond sending service idempotency safety horizon")
	}
	d = findDelivery(t, a, s.Principal, "aged")
	if d.State != "uncertain" || !d.NextAttempt.IsZero() {
		t.Fatalf("uncertainty not retained: %+v", d)
	}
}

func TestDeliveryAttemptLimitAndQueueAtomicity(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "limits@example.test")
	savedEmail(t, a, s.Principal, true)
	queueDelivery(t, a, s.Principal, "always-fails", "warn")
	// Every attempt fails, including the notice to administrators that the
	// email contact method is failing, which goes to the same address.
	sender := &recordingEmailSender{}
	for range 4 * maxDeliveryAttempts {
		sender.Results = append(sender.Results, DeliveryResult{State: "failed", Diagnostic: "sending service rejected request", Retryable: true})
	}
	for range 3 * maxDeliveryAttempts {
		if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
			t.Fatal(err)
		}
		advance(5 * time.Minute)
	}
	d := findDelivery(t, a, s.Principal, "always-fails")
	attempts := 0
	for _, m := range sender.messages() {
		if m.DeliveryID == d.ID {
			attempts++
		}
	}
	if d.Attempts != maxDeliveryAttempts || attempts != maxDeliveryAttempts || !d.NextAttempt.IsZero() {
		t.Fatalf("unbounded retries: %d sent, %+v", attempts, d)
	}
	ctx := context.Background()
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE atomic_probe(id TEXT); CREATE TRIGGER fail_delivery BEFORE INSERT ON deliveries BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END;`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO atomic_probe VALUES('accepted')`); err != nil {
			return err
		}
		return QueueNotification(ctx, tx, Notification{Key: "failed-admission", Severity: "warn", Purpose: "subscribed_alert", Title: "Cannot commit", At: a.Now()})
	})
	if err == nil {
		t.Fatal("queue failure ignored")
	}
	if err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM atomic_probe`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("domain state committed without notification intent")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContactMethodPermissionsAndPrivateSentTos(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	owner := memberFixture(t, a, "owner@example.test")
	other := memberFixture(t, a, "other@example.test")
	c := savedEmail(t, a, owner.Principal, true)
	ctx := context.Background()
	p := other.Principal
	contactMethods, err := a.ContactMethods(ctx, p)
	if err != nil || len(contactMethods) != 0 {
		t.Fatalf("private sent_to visible: %+v %v", contactMethods, err)
	}
	if _, err = a.SaveContactMethod(ctx, p, ContactMethodInput{ID: c.ID, ContactMethodType: "email", Fields: map[string]string{"email_address": "stolen@example.test"}, ExpectedRevision: c.Revision}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("edited someone else's contact method: %v", err)
	}
	if _, err = a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "shared@example.test"}, Shared: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member created shared contact method: %v", err)
	}
	queueDelivery(t, a, owner.Principal, "private", "info")
	deliveries, err := a.Deliveries(ctx, p, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deliveries {
		if d.SentTo == c.SentTo {
			t.Fatal("private sent_to leaked through delivery history")
		}
	}
	// Administrator can inspect, but still cannot edit another member's personal contact method.
	personal, err := a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "other-private@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RemoveContactMethod(ctx, owner.Principal, personal.ID, personal.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin edited someone else's contact method: %v", err)
	}
}

func TestRoutingSeverityMuteAndSOSEscalationCoalescing(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "routing@example.test")
	c := savedEmail(t, a, s.Principal, true)
	ctx := context.Background()
	// The first ordinary message is queued and the next ones join it as one
	// combined message; SOS is never combined.
	for _, severity := range []string{"info", "resolved-info", "warn", "resolved-warn", "sos", "resolved-sos"} {
		queueDelivery(t, a, s.Principal, severity, severity)
		want := "combined"
		if severity == "info" || severity == "sos" {
			want = "queued"
		}
		if d := findDelivery(t, a, s.Principal, severity); d.State != want {
			t.Fatalf("base/resolved routing: %+v", d)
		}
	}
	if _, err := a.SetTeamMute(ctx, s.Principal, a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	queueDelivery(t, a, s.Principal, "send-while-muted", "info")
	if d := findDelivery(t, a, s.Principal, "send-while-muted"); d.State != "queued" && d.State != "combined" {
		t.Fatalf("email configured to send while muted suppressed: %+v", d)
	}
	c, err := a.SaveContactMethod(ctx, s.Principal, ContactMethodInput{ID: c.ID, ContactMethodType: "email", Fields: map[string]string{"email_address": c.SentTo}, ExpectedRevision: c.Revision, Subscriptions: c.Subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	queueDelivery(t, a, s.Principal, "team-muted-suppressed", "info")
	if d := findDelivery(t, a, s.Principal, "team-muted-suppressed"); d.State != "suppressed" || d.Reason != "team mute" {
		t.Fatalf("team mute bypassed: %+v", d)
	}
	advance(time.Hour + time.Second)
	m, err := a.CurrentMember(ctx, s.Principal)
	if err != nil {
		t.Fatal(err)
	}
	phoneID := NewID()
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted) VALUES(?,?,'test_waking','{}','synthetic phone',1,0); INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,'sos')`, phoneID, m.ID, phoneID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := a.ManualSOS(ctx, s.Principal, "Synthetic sos_escalation", "No phone sending service is enabled")
	if err != nil {
		t.Fatal(err)
	}
	i, err := a.GetIncident(ctx, s.Principal, e.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for _, key := range []string{"repeat-one", "repeat-two", "repeat-three"} {
			if err := QueueNotification(ctx, tx, Notification{Key: key, IncidentID: i.ID, IncidentRevision: i.Revision, Severity: "sos", Purpose: "sos_escalation", Recipients: []string{m.ID}, Title: i.Title, At: a.Now()}); err != nil {
				return err
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND state='queued'`, i.ID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("catch-up burst queued %d sos_escalation intents", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := a.ClaimIncident(ctx, s.Principal, i.ID, i.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != "claimed" {
		t.Fatal(claimed.Status)
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var states []string
		rows, err := tx.QueryContext(ctx, `SELECT state FROM deliveries WHERE incident_id=? AND purpose='sos_escalation'`, i.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			if err = rows.Scan(&state); err != nil {
				return err
			}
			states = append(states, state)
		}
		if !reflect.DeepEqual(states, []string{"cancelled"}) {
			t.Fatalf("claim left sos_escalation work: %v", states)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQueueBoundCountsPendingNotSuppressedOrAccepted(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "queue-limit@example.test")
	c := savedEmail(t, a, s.Principal, true)
	ctx := context.Background()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO deliveries(id,notification_key,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,attempts,created_at,next_attempt) SELECT 'capacity-'||x,'capacity-'||x,?,1,?,'email',?,'subscribed_alert','warn','Synthetic capacity test','synthetic','uncertain',1,?,? FROM n`, maxPendingDeliveries, c.ID, c.MemberID, c.SentTo, a.Now().Unix(), a.Now().Unix())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return QueueNotification(ctx, tx, Notification{Key: "over-capacity", Severity: "warn", Purpose: "subscribed_alert", Title: "Reject atomically", At: a.Now()})
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("queue cap bypassed: %v", err)
	}
	// A suppressed explanation is not pending sending service work and remains visible.
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE contact_methods SET send_while_unavailable_or_muted=0 WHERE id=?`, c.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE team_mute SET until_at=?,expired=0 WHERE id=1`, a.Now().Add(time.Hour).Unix()); err != nil {
			return err
		}
		return QueueNotification(ctx, tx, Notification{Key: "suppressed-at-capacity", Severity: "warn", Purpose: "subscribed_alert", Title: "Muted", At: a.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := findDelivery(t, a, s.Principal, "suppressed-at-capacity"); d.State != "suppressed" {
		t.Fatalf("suppression lost: %+v", d)
	}
}

func TestPausedReservedWorkerDoesNotSendAfterExpiry(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "paused@example.test")
	savedEmail(t, a, s.Principal, true)
	queueDelivery(t, a, s.Principal, "paused", "warn")
	d, _, err := a.reserveDelivery(context.Background(), "https://mmhotline.example.com", []string{"email"})
	if err != nil {
		t.Fatal(err)
	}
	advance(retryHorizon)
	sender := &recordingEmailSender{}
	if err = a.dispatchReserved(context.Background(), d, sender); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages()) != 0 {
		t.Fatal("expired reserved worker sent a stale request")
	}
	saved := findDelivery(t, a, s.Principal, "paused")
	if saved.State != "uncertain" || !saved.NextAttempt.IsZero() {
		t.Fatalf("expired work was not reconciled: %+v", saved)
	}
}

func TestUnchangedContactMethodSavePreservesPendingDelivery(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	p := memberFixture(t, a, "unchanged@example.test").Principal
	c := savedEmail(t, a, p, true)
	queueDelivery(t, a, p, "waiting-for-sending service", "warn")
	saved, err := a.SaveContactMethod(context.Background(), p, ContactMethodInput{ID: c.ID, ContactMethodType: c.ContactMethodType, Fields: map[string]string{"email_address": c.SentTo}, ExpectedRevision: c.Revision, SendWhileUnavailableOrMuted: true, Subscriptions: []string{"warn", "sos", "info"}})
	if err != nil || saved.Revision != c.Revision {
		t.Fatalf("unchanged save: %+v %v", saved, err)
	}
	if d := findDelivery(t, a, p, "waiting-for-sending service"); d.State != "queued" {
		t.Fatalf("pending delivery: %+v", d)
	}
	if ds := deliveriesOf(t, a, c.ID); len(ds) != 2 {
		t.Fatalf("unchanged save queued extra work: %+v", ds)
	}
}

func TestSOSDeliveryGoesBeforeOlderBacklog(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "backlog@example.test")
	savedEmail(t, a, s.Principal, true)
	queueDelivery(t, a, s.Principal, "older-warn-1", "warn")
	queueDelivery(t, a, s.Principal, "older-warn-2", "warn")
	advance(time.Minute)
	queueDelivery(t, a, s.Principal, "newer-sos", "sos")
	sender := &recordingEmailSender{}
	if _, err := dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	messages := sender.messages()
	if len(messages) != 1 || !strings.HasPrefix(messages[0].Subject, "[SOS] ") {
		t.Fatalf("SOS waited behind older notices: %+v", messages)
	}
	if d := findDelivery(t, a, s.Principal, "older-warn-1"); d.State != "queued" {
		t.Fatalf("older notice should still be queued: %+v", d)
	}
}

func TestDeliveryOutcomeLabelsIncludeRetryState(t *testing.T) {
	next := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		state string
		next  time.Time
		want  string
	}{
		{"queued", time.Time{}, "Waiting to send"}, {"sending", next, "Sending"}, {"accepted", time.Time{}, "Accepted by the mail server"},
		{"failed", next, "Failed, will retry at 2026-09-30 12:00:00 UTC"}, {"failed", time.Time{}, "Failed, no more retries"},
		{"uncertain", next, "Unknown (no answer from the sending service), will retry at 2026-09-30 12:00:00 UTC"}, {"uncertain", time.Time{}, "Unknown (no answer from the sending service), no more retries"}, {"cancelled", time.Time{}, "Cancelled"}, {"suppressed", time.Time{}, "Suppressed"},
	} {
		delivery := Delivery{ContactMethodType: "email", State: test.state, NextAttempt: test.next}
		if label := delivery.OutcomeLabel(); label != test.want {
			t.Fatalf("%s next=%s: %q", test.state, test.next, label)
		}
	}
}

func TestContactMethodEmailSubjectsAndPurposes(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	person := memberFixture(t, a, "mail@example.test")
	p := person.Principal
	contactMethod := savedEmail(t, a, p, true)
	advance(time.Minute)
	if err := a.SendContactMethodTest(context.Background(), p, contactMethod.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		return RecordSystemAlert(context.Background(), tx, "system", "", "warn", "Team notice", "Team muted", a.Now())
	}); err != nil {
		t.Fatal(err)
	}
	deliveries, err := a.Deliveries(context.Background(), p, 50)
	if err != nil {
		t.Fatal(err)
	}
	purposes := map[string]bool{}
	for _, delivery := range deliveries {
		purposes[delivery.Purpose] = true
		switch delivery.Purpose {
		case "contact_method_test":
			if delivery.Subject != "MMHotline test message" || !strings.Contains(delivery.Message, "This is a test of this contact method, not an incident.") || delivery.PurposeLabel() != "Test" {
				t.Fatal(delivery)
			}
		case "system_notice":
			if delivery.PurposeLabel() != "Notice from MMHotline" {
				t.Fatal(delivery)
			}
		default:
			t.Fatalf("unexpected purpose: %s", delivery.Purpose)
		}
	}
	for _, purpose := range []string{"contact_method_test", "system_notice"} {
		if !purposes[purpose] {
			t.Fatalf("missing %s", purpose)
		}
	}
}

func TestContactMethodKindRulesComeFromContactMethodTypeFacts(t *testing.T) {
	a := foundationApp(t)
	s := memberFixture(t, a, "kinds@example.test")
	ctx := context.Background()
	if _, err := a.SaveContactMethod(ctx, s.Principal, ContactMethodInput{ContactMethodType: "carrier_pigeon", Subscriptions: []string{"sos"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind accepted: %v", err)
	}
	if _, err := a.SaveContactMethod(ctx, s.Principal, ContactMethodInput{ContactMethodType: "test_waking", Shared: true, Subscriptions: []string{"sos"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a kind not allowed for the team account was shared: %v", err)
	}
	c, err := a.SaveContactMethod(ctx, s.Principal, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": " Kinds@Example.test "}, Subscriptions: []string{"sos"}})
	if err != nil || c.SentTo != "kinds@example.test" || c.Setting("email_address") != "kinds@example.test" {
		t.Fatalf("email settings: %+v %v", c, err)
	}
	if _, err = a.SaveContactMethod(ctx, s.Principal, ContactMethodInput{ID: c.ID, ContactMethodType: "test_waking", ExpectedRevision: c.Revision, Subscriptions: []string{"sos"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("contact method changed its kind: %v", err)
	}
}

// acceptedSaveTestContactMethodType consumes fixture setup tests without adding a fake
// sending service request to the notification workflow being checked.
type acceptedSaveTestContactMethodType struct{ ContactMethodType }

func (c acceptedSaveTestContactMethodType) Send(_ context.Context, m ContactMethodSend) DeliveryResult {
	if m.Purpose != "contact_method_test" {
		return DeliveryResult{State: "failed", Diagnostic: "expected fixture setup test"}
	}
	return DeliveryResult{State: "accepted", IDFromSendingService: "fixture-test"}
}
func finishSaveTest(t *testing.T, a *App, p Principal, contactMethodType ContactMethodType) {
	t.Helper()
	contactMethodTypes := a.ContactMethodTypes
	defer func() { a.ContactMethodTypes = contactMethodTypes }()
	if _, err := dispatchWith(a, a.WebURL, acceptedSaveTestContactMethodType{contactMethodType}); err != nil {
		t.Fatal(err)
	}
}

func TestContactMethodSaveQueuesTestsOnlyForNewDestinations(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	p := memberFixture(t, a, "save-test@example.test").Principal
	ctx := context.Background()
	m, _ := a.CurrentMember(ctx, p)
	c, err := a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "first@example.test"}, Subscriptions: []string{"sos"}, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}
	ds := deliveriesOf(t, a, c.ID)
	if len(ds) != 1 || ds[0].Purpose != "contact_method_test" || ds[0].SentTo != "first@example.test" || !strings.Contains(ds[0].Message, m.Name+" added this contact method at "+a.Now().UTC().Format(time.RFC3339)) || !strings.Contains(ds[0].Message, "not an incident") {
		t.Fatalf("new destination: %+v", ds)
	}
	if err = a.SendContactMethodTest(ctx, p, c.ID); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("button after save: %v", err)
	}
	c, err = a.SaveContactMethod(ctx, p, ContactMethodInput{ID: c.ID, ContactMethodType: c.ContactMethodType, ExpectedRevision: c.Revision, Fields: map[string]string{"email_address": "second@example.test"}, Subscriptions: c.Subscriptions, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}
	ds = deliveriesOf(t, a, c.ID)
	if len(ds) != 2 {
		t.Fatalf("changed destination queued %d tests", len(ds))
	}
	for _, d := range ds {
		if d.SentTo == "first@example.test" && d.State == "cancelled" {
			continue
		}
		if d.SentTo == "second@example.test" && d.State == "queued" && strings.Contains(d.Message, m.Name+" changed this contact method at "+a.Now().UTC().Format(time.RFC3339)) {
			continue
		}
		t.Fatalf("changed destination: %+v", d)
	}
	sender := &recordingEmailSender{}
	if _, err = dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	if messages := sender.messages(); len(messages) != 1 || messages[0].SentTo != "second@example.test" || messages[0].Purpose != "contact_method_test" {
		t.Fatalf("save test inside button limit: %+v", messages)
	}
	c, err = a.SaveContactMethod(ctx, p, ContactMethodInput{ID: c.ID, ContactMethodType: c.ContactMethodType, ExpectedRevision: c.Revision, Fields: map[string]string{"email_address": c.SentTo}, Subscriptions: []string{"info", "sos"}, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}
	if ds = deliveriesOf(t, a, c.ID); len(ds) != 2 {
		t.Fatalf("subscription-only save queued a test: %+v", ds)
	}
	_, err = a.SaveContactMethod(ctx, p, ContactMethodInput{ID: c.ID, ContactMethodType: c.ContactMethodType, ExpectedRevision: c.Revision, Fields: map[string]string{"email_address": c.SentTo}, Subscriptions: c.Subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	if ds = deliveriesOf(t, a, c.ID); len(ds) != 2 {
		t.Fatalf("preference-only save queued a test: %+v", ds)
	}
}

func TestNewContactMethodReceivesSOSBeforeItsTest(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	p := memberFixture(t, a, "immediate@example.test").Principal
	c, err := a.SaveContactMethod(context.Background(), p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "new@example.test"}, Subscriptions: []string{"sos"}, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}
	event, err := a.ManualSOS(context.Background(), p, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	sender := &recordingEmailSender{}
	if _, err = dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	messages := sender.messages()
	if len(messages) != 1 || messages[0].SentTo != c.SentTo || messages[0].IncidentID != event.IncidentID || messages[0].Purpose != "subscribed_alert" {
		t.Fatalf("new contact did not receive SOS immediately: %+v", messages)
	}
	ds := deliveriesOf(t, a, c.ID)
	for _, d := range ds {
		if d.Purpose == "contact_method_test" && d.State != "queued" {
			t.Fatalf("SOS needed an accepted test: %+v", d)
		}
	}
}

func TestSendContactMethodTestOncePerMinute(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	p := memberFixture(t, a, "minute@example.test").Principal
	c := savedEmail(t, a, p, true)
	ctx := context.Background()
	advance(time.Minute)
	if err := a.SendContactMethodTest(ctx, p, c.ID); err != nil {
		t.Fatal(err)
	}
	for _, delay := range []time.Duration{0, 59 * time.Second} {
		advance(delay)
		if err := a.SendContactMethodTest(ctx, p, c.ID); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("button before a minute: %v", err)
		}
	}
	advance(time.Second)
	if err := a.SendContactMethodTest(ctx, p, c.ID); err != nil {
		t.Fatalf("button at one minute: %v", err)
	}
	if ds := deliveriesOf(t, a, c.ID); len(ds) != 3 {
		t.Fatalf("refused presses queued work: %+v", ds)
	}
}

func TestSaveTestDoesNotFailAtQueueCapacity(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	p := memberFixture(t, a, "full-save@example.test").Principal
	c := savedEmail(t, a, p, true)
	ctx := context.Background()
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO deliveries(id,notification_key,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,created_at,next_attempt) SELECT 'full-'||x,'full-'||x,?,1,?,'email',?,'subscribed_alert','warn','','','queued',?,? FROM n`, maxPendingDeliveries, c.ID, c.MemberID, c.SentTo, a.Now().Unix(), a.Now().Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	newContact, err := a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "another@example.test"}})
	if err != nil {
		t.Fatalf("backlog refused save: %v", err)
	}
	if ds := deliveriesOf(t, a, newContact.ID); len(ds) != 1 || ds[0].State != "queued" {
		t.Fatalf("save test at capacity: %+v", ds)
	}
}

func TestContactMethodsPageShowsLatestTestResult(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	p := memberFixture(t, a, "test-result@example.test").Principal
	ctx := context.Background()
	c, err := a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "last-test@example.test"}, Subscriptions: []string{"warn"}, SendWhileUnavailableOrMuted: true})
	if err != nil {
		t.Fatal(err)
	}
	page := func(wants ...string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://mmhotline.example.com/contact-methods", nil)
		addAccessToken(request, p.SignInEmailAddress)
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("contact methods: HTTP %d", response.Code)
		}
		for _, want := range wants {
			if !strings.Contains(pageText(response.Body.String()), want) {
				t.Fatalf("missing %q: %s", want, pageText(response.Body.String()))
			}
		}
	}
	page("Last test: Waiting to send")
	sender := &recordingEmailSender{Results: []DeliveryResult{{State: "failed", Diagnostic: "Recipient address refused"}}}
	if _, err = dispatchWith(a, "https://mmhotline.example.com", sender); err != nil {
		t.Fatal(err)
	}
	page("Last test: Failed, no more retries", "Recipient address refused")
	advance(time.Minute)
	if err = a.SendContactMethodTest(ctx, p, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = dispatchWith(a, "https://mmhotline.example.com", &recordingEmailSender{}); err != nil {
		t.Fatal(err)
	}
	// More than 50 newer records must not hide the last test on the card.
	advance(time.Second)
	for n := range 51 {
		queueDelivery(t, a, p, fmt.Sprintf("after-test-%d", n), "warn")
	}
	page("Last test: Accepted by the mail server")
	// A retained contact with no retained test has an explicit empty state.
	if err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM delivery_attempts WHERE delivery_id IN (SELECT id FROM deliveries WHERE contact_method_id=? AND purpose='contact_method_test')`, c.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM deliveries WHERE contact_method_id=? AND purpose='contact_method_test'`, c.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page("Last test: Not tested yet")
}

// A due delivery that cannot be checked before sending fails with the
// reason, and the error is reported, so the next delivery is still sent.
// Before, reservation always picked the same delivery first, so its error
// stopped every later delivery.
func TestADeliveryThatCannotBeCheckedFailsAndTheNextOneIsSent(t *testing.T) {
	a, p, _ := incidentTeam(t)
	ctx := context.Background()
	now := a.Now()
	a.Now = func() time.Time { return now }
	broken := newIncidentFixture(t, a, p)
	now = now.Add(time.Second)
	healthy := newIncidentFixture(t, a, p)
	// Checking an SOS escalation reads its incident, which now cannot be read.
	if _, err := a.Store.db.Exec(`UPDATE incidents SET targets='not a list' WHERE id=?`, broken.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.reserveDelivery(ctx, "https://mmhotline.example.com", []string{"test_waking"}); err == nil {
		t.Fatal("the delivery that cannot be checked was not reported")
	}
	d, _, err := a.reserveDelivery(ctx, "https://mmhotline.example.com", []string{"test_waking"})
	if err != nil || d.IncidentID != healthy.ID {
		t.Fatalf("the next delivery was not reserved: incident %s, error %v", d.IncidentID, err)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND state='failed' AND reason LIKE 'MMHotline could not check this notification before sending it: %'`, broken.ID); n != 1 {
		t.Fatalf("the delivery that cannot be checked did not fail with the reason: %d", n)
	}
}
