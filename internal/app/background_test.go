package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

type progressEmailSender struct {
	EmailContactMethodType
	sent chan ContactMethodSend
}

func (s *progressEmailSender) Send(_ context.Context, m ContactMethodSend) DeliveryResult {
	select {
	case s.sent <- m:
	default:
	}
	return DeliveryResult{State: "accepted", IDFromSendingService: "fixture-acceptance"}
}

func TestFullQueueDrainsWhenMuteExpiryCannotEnqueue(t *testing.T) {
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	s := memberFixture(t, a, "queue-drain@example.test")
	c := savedEmail(t, a, s.Principal, true)
	ctx := context.Background()
	if _, err := a.SetTeamMute(ctx, s.Principal, a.Now().Add(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	// The mute notice occupies one slot. Fill the rest of the ordinary part of
	// the pending limit (the remainder is reserved for SOS) with the durable
	// backlog a sending service outage can leave; suppressed/finished work is excluded.
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		// The outage left the mute notice retrying too, so the expiry notice
		// cannot join it as a combined message.
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='uncertain',attempts=1 WHERE state='queued'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO deliveries(id,notification_key,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,attempts,created_at,next_attempt) SELECT 'drain-'||x,'drain-'||x,?,?,?,'email',?,'subscribed_alert','warn','Outage backlog','Work remains actionable','uncertain',1,?,? FROM n`, maxPendingDeliveries-sosReservedPendingDeliveries-1, c.ID, c.Revision, c.MemberID, c.SentTo, a.Now().Unix(), a.Now().Unix())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Minute)
	sender := &progressEmailSender{sent: make(chan ContactMethodSend, 1)}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	reported := make(chan error, 2)
	a.WebURL = "https://mmhotline.example.com"
	a.ContactMethodTypes = map[string]ContactMethodType{"email": sender}
	go func() {
		defer close(done)
		a.RunBackground(workerCtx, func(err error) {
			select {
			case reported <- err:
			default:
			}
		})
	}()
	defer func() { cancel(); <-done }()
	// The wait is far above the 0.1 seconds of an ordinary run: with the race
	// detector, SQLite over the 9,000 queued deliveries takes seconds for one
	// round, and the first round is the one that sends.
	select {
	case err := <-reported:
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("unexpected reconciliation error: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("no reconciliation progress")
	}
	select {
	case <-sender.sent:
	case <-time.After(time.Minute):
		t.Fatal("full pending queue did not drain after reconciliation could not enqueue its expiry notice")
	}
	cancel()
	<-done
	// Once dispatch frees a slot the original reconciliation can commit too.
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatalf("reconciliation did not recover: %v", err)
	}
}

// The real loop: a team with nothing due costs nothing, and an alert that
// arrives is sent in the next round.
func TestBackgroundSchedulerSendsAnAlertThatArrivesWhileItsTeamIsIdle(t *testing.T) {
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "idle@example.test")
	savedEmail(t, a, s.Principal, true)
	source, err := createAlertSource(context.Background(), a, s.Principal, "Backup")
	if err != nil {
		t.Fatal(err)
	}
	sender := &progressEmailSender{sent: make(chan ContactMethodSend, 1)}
	a.WebURL = "https://mmhotline.example.com"
	a.ContactMethodTypes = map[string]ContactMethodType{"email": sender}
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.RunBackground(workerCtx, func(err error) { t.Errorf("background work: %v", err) })
	}()
	defer func() { cancel(); <-done }()
	// Long enough for the first visits and for the team to become idle.
	time.Sleep(2500 * time.Millisecond)
	select {
	case m := <-sender.sent:
		t.Fatalf("sent %q with no alert", m.Subject)
	default:
	}
	alertReceipt(t, intakeHTTP(a, s.Principal, source, `{"sender_alert_id":"disk","severity":"warn","title":"Disk pressure","message":""}`, a.Now()))
	select {
	case <-sender.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the alert was not sent")
	}
}
