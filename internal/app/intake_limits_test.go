package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSOSIsNeverLimitedByRate(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	now := a.Now()
	for n := range SourceAlertsPerMinute {
		alertReceipt(t, intakeHTTP(a, p, s, fmt.Sprintf(`{"sender_alert_id":"w%d","severity":"warn","title":"Busy","message":""}`, n), now))
	}
	if w := intakeHTTP(a, p, s, `{"sender_alert_id":"one-too-many","severity":"warn","title":"Busy","message":""}`, now); w.Code != 429 {
		t.Fatalf("121st Warning in a minute: %d", w.Code)
	}
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"sos","sender_incident_key":"disk","severity":"sos","title":"Disk full","message":""}`, now))
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"sos-ok","sender_incident_key":"disk","severity":"resolved-sos","title":"Disk ok","message":""}`, now))
	total, reasons, err := a.RejectedRequests(context.Background(), p, s.Source.ID)
	if err != nil || total != 1 || len(reasons) != 1 || reasons[0].Reason != rejectedTooManyAlerts {
		t.Fatalf("rejected requests: %d %+v %v", total, reasons, err)
	}
}

func TestTeamHourlyWarningAndInfoLimit(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	ctx := context.Background()
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO team_alert_rate(id,hour_bucket,warning_and_info_alerts) VALUES(1,?,?)`, a.Now().Unix()/3600, installationMaximumNewWarningAndInfoAlertsPerHour-1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"last","severity":"info","title":"Last one","message":""}`, a.Now()))
	for _, severity := range []string{"info", "warn", "resolved-warn"} {
		if w := intakeHTTP(a, p, s, `{"sender_alert_id":"over-`+severity+`","severity":"`+severity+`","title":"Over","message":""}`, a.Now()); w.Code != 429 || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s over the team limit: %d", severity, w.Code)
		}
	}
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"sos","severity":"sos","title":"Still wakes people","message":""}`, a.Now()))
	notices := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE source_id='system' AND title=?`, "Alert source “Backup” is sending too many alerts: MMHotline refuses some of them")
	if notices != 1 {
		t.Fatalf("administrators told %d times within an hour", notices)
	}
	later := a.Now().Add(time.Hour)
	a.Now = func() time.Time { return later }
	alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"next-hour","severity":"warn","title":"New hour","message":""}`, later))
}

func TestRejectedRequestReasonsAreCountedInMemory(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	body := `{"sender_alert_id":"x","severity":"warn","title":"Hello","message":""}`
	wrong := s
	wrong.Secret = strings.Repeat("ab", 32)
	if w := intakeHTTP(a, p, wrong, body, a.Now()); w.Code != 401 {
		t.Fatalf("wrong signature: %d", w.Code)
	}
	if w := intakeHTTP(a, p, s, body, a.Now().Add(-6*time.Minute)); w.Code != 401 {
		t.Fatalf("old timestamp: %d", w.Code)
	}
	if w := intakeHTTP(a, p, s, `{"severity":"warn","title":"","message":""}`, a.Now()); w.Code != 400 {
		t.Fatalf("invalid alert: %d", w.Code)
	}
	unknown := s
	unknown.Source.ID = NewID()
	if w := intakeHTTP(a, p, unknown, body, a.Now()); w.Code != 401 {
		t.Fatalf("unknown source: %d", w.Code)
	}
	total, reasons, err := a.RejectedRequests(context.Background(), p, s.Source.ID)
	if err != nil || total != 3 || len(reasons) != 3 {
		t.Fatalf("reasons: %d %+v %v", total, reasons, err)
	}
	for _, want := range []string{rejectedWrongSignature, rejectedClockOff, rejectedInvalidAlert} {
		found := false
		for _, r := range reasons {
			found = found || (r.Reason == want && r.Count == 1)
		}
		if !found {
			t.Fatalf("missing %q in %+v", want, reasons)
		}
	}
	if total, _, _ = a.RejectedRequests(context.Background(), p, unknown.Source.ID); total != 0 {
		t.Fatal("an unknown alert source was counted")
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts`); n != 0 {
		t.Fatalf("rejected requests wrote %d alerts", n)
	}
	a.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 30, 0, 0, time.UTC) }
	if total, _, _ = a.RejectedRequests(context.Background(), p, s.Source.ID); total != 0 {
		t.Fatal("counts older than 24 hours were kept")
	}
}

func TestRefusedSignedSOSOpensOneMmhotlineSOS(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	sos := `{"sender_alert_id":"late","sender_incident_key":"disk","severity":"sos","title":"Disk full","message":""}`
	for range 2 {
		if w := intakeHTTP(a, p, s, sos, a.Now().Add(-10*time.Minute)); w.Code != 401 {
			t.Fatalf("clock off: %d", w.Code)
		}
	}
	title := "MMHotline is refusing SOS alerts from “Backup”: the sender's clock is more than 5 minutes off"
	if n := scalarTeam(t, a, `SELECT count(*) FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE e.title=? AND i.status='open'`, title); n != 1 {
		t.Fatalf("MMHotline SOS incidents: %d", n)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE title=?`, title); n != 2 {
		t.Fatalf("the second refusal did not join: %d alerts", n)
	}
	invalid := `{"sender_alert_id":"bad","severity":"sos","title":"` + strings.Repeat("x", 300) + `","message":""}`
	if w := intakeHTTP(a, p, s, invalid, a.Now()); w.Code != 400 {
		t.Fatalf("invalid SOS: %d", w.Code)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE title LIKE 'MMHotline is refusing SOS alerts from “Backup”: the alert is not valid%'`); n != 1 {
		t.Fatalf("invalid SOS did not open an MMHotline SOS: %d", n)
	}
	wrong := s
	wrong.Secret = strings.Repeat("cd", 32)
	before := scalarTeam(t, a, `SELECT count(*) FROM alerts`)
	if w := intakeHTTP(a, p, wrong, sos, a.Now()); w.Code != 401 {
		t.Fatalf("wrong signature: %d", w.Code)
	}
	if after := scalarTeam(t, a, `SELECT count(*) FROM alerts`); after != before {
		t.Fatal("a request with a wrong signature opened something")
	}
}

// A request captured on the private network and sent again after the
// 5-minute signature window is refused like any old signature. It repeats an
// SOS MMHotline already received, so it opens no SOS of MMHotline's own:
// otherwise each resend after the incident is resolved would wake the person
// on call again.
func TestAResentSOSThatWasReceivedOpensNoMmhotlineSOS(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	for _, sos := range []string{
		`{"sender_alert_id":"captured","sender_incident_key":"disk","severity":"sos","title":"Disk full","message":""}`,
		// Without a sender_alert_id, the signature is the replay key.
		`{"sender_incident_key":"cpu","severity":"sos","title":"CPU","message":""}`,
	} {
		signedAt := a.Now()
		alertReceipt(t, intakeHTTP(a, p, s, sos, signedAt))
		start := a.Now()
		for _, later := range []time.Duration{10 * time.Minute, 48 * time.Hour} {
			a.Now = func() time.Time { return start.Add(later) }
			if w := intakeHTTP(a, p, s, sos, signedAt); w.Code != 401 {
				t.Fatalf("a resent SOS %s later: %d %s", later, w.Code, w.Body.String())
			}
		}
		a.Now = func() time.Time { return start }
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE title LIKE 'MMHotline is refusing SOS alerts%'`); n != 0 {
		t.Fatalf("a resent SOS opened %d SOS alerts of MMHotline's own", n)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE source_id=?`, s.Source.ID); n != 2 {
		t.Fatalf("alerts from the alert source: %d", n)
	}
}

func TestMmhotlineSOSOpensAtTheUnresolvedIncidentLimit(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	ctx := context.Background()
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for n := range maxUnresolvedIncidents {
			if _, err := admitAlert(ctx, tx, s.Source.ID, "test", fmt.Sprintf("fill-%d", n), "", GenericAlert{Severity: "sos", SenderIncidentKey: fmt.Sprintf("k%d", n), Title: "Filler", Message: ""}, nil, a.Now()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := intakeHTTP(a, p, s, `{"sender_alert_id":"one-more","sender_incident_key":"new","severity":"sos","title":"One more","message":""}`, a.Now()); w.Code != 429 {
		t.Fatalf("SOS over the incident limit: %d", w.Code)
	}
	title := fmt.Sprintf("MMHotline is refusing SOS alerts from “Backup”: this installation already has %d unresolved SOS incidents", maxUnresolvedIncidents)
	if n := scalarTeam(t, a, `SELECT count(*) FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE e.title=?`, title); n != 1 {
		t.Fatalf("MMHotline SOS did not open at the limit: %d", n)
	}
}

func TestSOSReserveInThePendingDeliveryLimit(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	deliveryTestClock(a)
	s := memberFixture(t, a, "reserve@example.test")
	c := savedEmail(t, a, s.Principal, true)
	ctx := context.Background()
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var pending int
		if err := tx.QueryRow(`SELECT count(*) FROM deliveries WHERE state IN ('queued','sending') OR (state IN ('failed','uncertain') AND next_attempt>0)`).Scan(&pending); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO deliveries(id,notification_key,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,attempts,created_at,next_attempt) SELECT 'fill-'||x,'fill-'||x,?,?,?,'email',?,'subscribed_alert','warn','Backlog','Backlog','uncertain',1,?,? FROM n`, maxPendingDeliveries-sosReservedPendingDeliveries-pending, c.ID, c.Revision, c.MemberID, c.SentTo, a.Now().Unix(), a.Now().Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return QueueNotification(ctx, tx, Notification{Key: "ordinary", Severity: "warn", Title: "Ordinary", Purpose: "subscribed_alert", At: a.Now()})
	})
	if !errors.Is(err, errPendingDeliveryFull) {
		t.Fatalf("ordinary delivery past its part of the limit: %v", err)
	}
	if _, err = a.ManualSOS(ctx, s.Principal, "Database down", ""); err != nil {
		t.Fatalf("SOS blocked by ordinary deliveries: %v", err)
	}
}

// fillPendingDeliveries adds n pending deliveries to c directly. A non-empty
// sourceID makes them SOS deliveries about one alert from that alert source.
func fillPendingDeliveries(t *testing.T, a *App, c ContactMethod, n int64, sourceID string) {
	t.Helper()
	ctx := context.Background()
	severity, alertID := "warn", ""
	if sourceID != "" {
		severity, alertID = "sos", "fill-alert-"+sourceID
	}
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if sourceID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO alerts(id,source_id,sender_alert_id,actor,replay_key,incident_id,sender_incident_key,severity,title,message,notes,received_at) VALUES(?,?,'','test',?,'','','sos','Backlog','','',?)`, alertID, sourceID, alertID, a.Now().Unix()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO deliveries(id,notification_key,alert_id,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,created_at,next_attempt) SELECT 'fill-'||?||x,'fill-'||?||x,?,?,?,?,'email',?,'subscribed_alert',?,'Backlog','Backlog','queued',?,? FROM n`, n, sourceID, sourceID, alertID, c.ID, c.Revision, c.MemberID, c.SentTo, severity, a.Now().Unix(), a.Now().Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func pendingDeliveryCount(t *testing.T, a *App) int64 {
	t.Helper()
	return scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE state='sending' OR `+pendingDelivery)
}

func TestOneAlertSourceCannotUseTheWholeSOSReserve(t *testing.T) {
	t.Parallel()
	a, p, flood := alertFixture(t)
	deliveryTestClock(a)
	c := savedEmail(t, a, p, true)
	other, err := createAlertSource(context.Background(), a, p, "Database")
	if err != nil {
		t.Fatal(err)
	}
	// Room for 999 more deliveries: less than the SOS reserve, and less than
	// the unresolved-incident limit, so only a share per alert source can
	// keep room for the other alert source.
	fillPendingDeliveries(t, a, c, maxPendingDeliveries-999-pendingDeliveryCount(t, a), "")
	refused := false
	for n := range 999 {
		w := intakeHTTP(a, p, flood, fmt.Sprintf(`{"sender_alert_id":"f%d","sender_incident_key":"f%d","severity":"sos","title":"Flood","message":""}`, n, n), a.Now())
		if w.Code == 429 {
			refused = true
			break
		}
		if w.Code != 200 {
			t.Fatalf("flood SOS %d: %d %s", n, w.Code, w.Body.String())
		}
	}
	if !refused {
		t.Error("one alert source's SOS alerts used the whole SOS reserve")
	}
	if w := intakeHTTP(a, p, other, `{"sender_alert_id":"b","sender_incident_key":"b","severity":"sos","title":"Database down","message":""}`, a.Now()); w.Code != 200 {
		t.Fatalf("another alert source's SOS after the flood: %d %s", w.Code, w.Body.String())
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries d JOIN alerts e ON e.id=d.alert_id WHERE e.title='Database down' AND d.state='queued'`); n != 1 {
		t.Fatalf("the other alert source's SOS deliveries: %d", n)
	}
}

func TestAlertSourceOverItsSOSShareIsRefusedVisibly(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	deliveryTestClock(a)
	c := savedEmail(t, a, p, true)
	fillPendingDeliveries(t, a, c, maxPendingSOSDeliveriesPerAlertSource, s.Source.ID)
	if w := intakeHTTP(a, p, s, `{"sender_alert_id":"one-more","sender_incident_key":"new","severity":"sos","title":"One more","message":""}`, a.Now()); w.Code != 429 {
		t.Fatalf("SOS over the alert source's share: %d %s", w.Code, w.Body.String())
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE title='One more'`); n != 0 {
		t.Fatalf("the refused SOS was saved: %d", n)
	}
	title := fmt.Sprintf("MMHotline is refusing SOS alerts from “Backup”: this alert source already has %d SOS notifications waiting to be sent", maxPendingSOSDeliveriesPerAlertSource)
	if n := scalarTeam(t, a, `SELECT count(*) FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE e.title=? AND i.status='open'`, title); n != 1 {
		t.Fatalf("MMHotline SOS about the refusal: %d", n)
	}
}

func TestMmhotlineRefusingSOSPersistsWhenTheQueueIsFull(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	deliveryTestClock(a)
	c := savedEmail(t, a, p, true)
	fillPendingDeliveries(t, a, c, maxPendingDeliveries-pendingDeliveryCount(t, a), "")
	if w := intakeHTTP(a, p, s, `{"sender_alert_id":"full","sender_incident_key":"full","severity":"sos","title":"Queue full","message":""}`, a.Now()); w.Code != 429 {
		t.Fatalf("SOS with the queue full: %d", w.Code)
	}
	title := "MMHotline is refusing SOS alerts from “Backup”: too many notifications are waiting to be sent for this installation"
	if n := scalarTeam(t, a, `SELECT count(*) FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE e.title=? AND i.status='open'`, title); n != 1 {
		t.Fatalf("MMHotline SOS about the refusal with the queue full: %d", n)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries d JOIN alerts e ON e.id=d.alert_id WHERE e.title=? AND d.state='queued'`, title); n != 1 {
		t.Fatalf("deliveries of MMHotline's SOS with the queue full: %d", n)
	}
}

func TestMmhotlineRefusingSOSAddsNoMessageWhileOneWaits(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	deliveryTestClock(a)
	savedEmail(t, a, p, true)
	ctx := context.Background()
	invalid := func(n int) {
		t.Helper()
		body := fmt.Sprintf(`{"sender_alert_id":"bad%d","severity":"sos","title":"%s","message":""}`, n, strings.Repeat("x", 300))
		if w := intakeHTTP(a, p, s, body, a.Now()); w.Code != 400 {
			t.Fatalf("invalid SOS: %d", w.Code)
		}
	}
	invalid(0)
	var id string
	var revision int64
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT id,revision FROM incidents WHERE source_id='system'`).Scan(&id, &revision)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimIncident(ctx, p, id, revision, ""); err != nil {
		t.Fatal(err)
	}
	// MMHotline's own SOS is exempt from the pending-delivery limits, so
	// a sender refused again and again must not queue a message each time.
	for n := 1; n <= 3; n++ {
		invalid(n)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE incident_id=?`, id); n != 4 {
		t.Fatalf("refusals recorded in the incident: %d", n)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND notification_key LIKE 'related_alert_sent_to_claimer:%' AND `+pendingDelivery, id); n != 1 {
		t.Fatalf("waiting messages to the person who claimed the refusal incident: %d", n)
	}
}

func TestAnotherAlertSourcesSOSGoesBeforeAFloodsBacklog(t *testing.T) {
	t.Parallel()
	a, p, flood := alertFixture(t)
	advance := deliveryTestClock(a)
	savedEmail(t, a, p, true)
	ctx := context.Background()
	other, err := createAlertSource(ctx, a, p, "Database")
	if err != nil {
		t.Fatal(err)
	}
	admit := func(sourceID, key, title string) {
		t.Helper()
		advance(time.Second)
		if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			_, err := admitAlert(ctx, tx, sourceID, "source:"+sourceID, "sender:"+key, "", GenericAlert{Severity: "sos", SenderIncidentKey: key, Title: title}, nil, a.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for n := range 50 {
		admit(flood.Source.ID, fmt.Sprintf("a%d", n), "Flood")
	}
	admit(other.Source.ID, "b", "Database down")
	sender := &recordingEmailSender{}
	for {
		work, err := dispatchWith(a, "https://mmhotline.example.com", sender)
		if err != nil {
			t.Fatal(err)
		}
		if !work {
			break
		}
	}
	messages := sender.messages()
	position := -1
	for n, m := range messages {
		if m.Title == "Database down" {
			position = n
		}
	}
	// Before most of the flood's backlog: the flood must not push the
	// other alert source's SOS behind its own.
	if position < 0 || position >= 25 {
		t.Fatalf("the other alert source's SOS was message %d of %d", position+1, len(messages))
	}
}

func TestAlertSourceLimitPerTeam(t *testing.T) {
	t.Parallel()
	a, p, _ := alertFixture(t)
	for n := 1; n < maxAlertSources; n++ {
		if _, err := createAlertSource(context.Background(), a, p, fmt.Sprintf("Source %d", n)); err != nil {
			t.Fatal(n, err)
		}
	}
	if _, err := createAlertSource(context.Background(), a, p, "One too many"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("101st alert source: %v", err)
	}
}
