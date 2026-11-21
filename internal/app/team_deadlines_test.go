package app

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// deadlineRig is a team with a saved email contact method, an email
// contact method type that records what it sends, and a clock that tests move.
type deadlineRig struct {
	a       *App
	p       Principal
	advance func(time.Duration)
	sender  *recordingEmailSender
}

func newDeadlineRig(t *testing.T) *deadlineRig {
	t.Helper()
	a := foundationApp(t)
	r := &deadlineRig{a: a, advance: deliveryTestClock(a), sender: &recordingEmailSender{}}
	r.p = memberFixture(t, a, "deadlines@example.test").Principal
	savedEmail(t, a, r.p, true)
	a.WebURL = "https://mmhotline.example.com"
	a.ContactMethodTypes = map[string]ContactMethodType{"email": r.sender}
	return r
}

// round runs the production background round and reports whether it visited.
func (r *deadlineRig) round(t *testing.T) bool {
	t.Helper()
	return r.a.backgroundRound(context.Background(), func(err error) { t.Errorf("background work: %v", err) })
}

// settle runs rounds until one visits no team.
func (r *deadlineRig) settle(t *testing.T) {
	t.Helper()
	for n := 0; r.round(t); n++ {
		if n > 10 {
			t.Fatal("a team with nothing due is visited every round")
		}
	}
}

// quietFor moves the clock a second at a time and fails if a round visits a
// team.
func (r *deadlineRig) quietFor(t *testing.T, d time.Duration) {
	t.Helper()
	for range int(d / time.Second) {
		r.advance(time.Second)
		if r.round(t) {
			t.Fatalf("visited at %s with nothing due", r.a.Now().UTC().Format(time.TimeOnly))
		}
	}
}

func (r *deadlineRig) nextDeadline(t *testing.T) time.Time {
	t.Helper()
	d := r.a.teamDeadline
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.facts.nextDeadline
}

func (r *deadlineRig) systemTitles(t *testing.T) []string {
	t.Helper()
	var titles []string
	err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT title FROM alerts WHERE source_id='system' ORDER BY sequence`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err = rows.Scan(&s); err != nil {
				return err
			}
			titles = append(titles, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return titles
}

func TestATeamWithNothingDueIsLeftAlone(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	if !r.round(t) {
		t.Fatal("a team whose database changed was not visited")
	}
	r.settle(t)
	if !r.nextDeadline(t).IsZero() {
		t.Fatalf("a team with nothing due has a deadline: %s", r.nextDeadline(t))
	}
	r.quietFor(t, 5*time.Minute)
}

func TestIdleBackgroundRoundsDoNotWrite(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	if !r.round(t) {
		t.Fatal("the first round did not visit the team")
	}
	before := scalarTeam(t, r.a, `SELECT total_changes()`)
	for range 3 {
		r.advance(time.Second)
		r.round(t)
		r.advance(10 * time.Minute)
		if !r.round(t) {
			t.Fatal("the periodic idle visit did not run")
		}
	}
	if after := scalarTeam(t, r.a, `SELECT total_changes()`); after != before {
		t.Fatalf("idle rounds changed %d rows", after-before)
	}
}

func TestIdleTeamIsVisitedAfterTenMinutes(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	r.settle(t)
	r.quietFor(t, 10*time.Minute-time.Second)
	r.advance(time.Second)
	if !r.round(t) {
		t.Fatal("the idle team was not visited after ten minutes")
	}
	r.quietFor(t, time.Minute)
}

func TestFailedBackgroundVisitIsRetriedNextRound(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	r.settle(t)
	// A visit that cannot read its delivery work fails.
	if _, err := r.a.Store.db.Exec(`ALTER TABLE deliveries RENAME TO deliveries_unreadable`); err != nil {
		t.Fatal(err)
	}
	r.advance(10 * time.Minute)
	var reported []error
	if !r.a.backgroundRound(context.Background(), func(err error) { reported = append(reported, err) }) || len(reported) == 0 {
		t.Fatal("the visit did not report the failure")
	}
	if _, err := r.a.Store.db.Exec(`ALTER TABLE deliveries_unreadable RENAME TO deliveries`); err != nil {
		t.Fatal(err)
	}
	if !r.round(t) {
		t.Fatal("the failed visit was not retried next round")
	}
	r.quietFor(t, time.Minute)
}

// panickingEmailSender panics as a send could on a stored row MMHotline
// cannot handle.
type panickingEmailSender struct{ recordingEmailSender }

func (*panickingEmailSender) Send(context.Context, ContactMethodSend) DeliveryResult {
	panic("a stored row MMHotline cannot handle")
}

// A panic in a visit is reported with its stack, like a returned error, and
// the loop goes on: the next round visits again, and the delivery the panic
// interrupted is still sent.
func TestPanickingBackgroundVisitIsReportedAndTheLoopGoesOn(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	r.settle(t)
	r.a.ContactMethodTypes = map[string]ContactMethodType{"email": &panickingEmailSender{}}
	queueDelivery(t, r.a, r.p, "panic", "warn")
	var reported []string
	if !r.a.backgroundRound(context.Background(), func(err error) { reported = append(reported, err.Error()) }) {
		t.Fatal("the delivery did not make the team due")
	}
	if len(reported) != 1 || !strings.Contains(reported[0], "the background visit panicked: a stored row MMHotline cannot handle") || !strings.Contains(reported[0], "panickingEmailSender") {
		t.Fatalf("reported %q", reported)
	}
	r.a.ContactMethodTypes = map[string]ContactMethodType{"email": r.sender}
	before := len(r.sender.messages())
	for range 60 {
		r.advance(time.Second)
		r.round(t)
	}
	if messages := r.sender.messages(); len(messages) != before+1 || !strings.Contains(messages[before].Subject, "Disk pressure") {
		t.Fatalf("after the panic, the loop sent %d messages", len(messages)-before)
	}
	r.settle(t)
}

func TestAnAlertWakesItsTeam(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	r.settle(t)
	r.quietFor(t, 10*time.Second)
	source, err := createAlertSource(context.Background(), r.a, r.p, "Backup")
	if err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	before := len(r.sender.messages())
	alertReceipt(t, intakeHTTP(r.a, r.p, source, `{"sender_alert_id":"disk","severity":"warn","title":"Disk pressure","message":""}`, r.a.Now()))
	if !r.round(t) {
		t.Fatal("an accepted alert did not make its team due")
	}
	if messages := r.sender.messages(); len(messages) != before+1 || !strings.Contains(messages[before].Subject, "Disk pressure") {
		t.Fatalf("the round after the alert sent %d messages", len(messages)-before)
	}
	r.settle(t)
	r.quietFor(t, 10*time.Second)
}

func TestARetryIsDueAtItsTime(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	r.settle(t)
	r.sender.Results = append(make([]DeliveryResult, len(r.sender.messages())), DeliveryResult{State: "failed", Diagnostic: "sending service is down", Retryable: true})
	queueDelivery(t, r.a, r.p, "retry", "warn")
	r.round(t)
	retry := findDelivery(t, r.a, r.p, "retry")
	if retry.State != "failed" || !retry.NextAttempt.After(r.a.Now()) {
		t.Fatalf("delivery after a failed attempt: %+v", retry)
	}
	r.settle(t)
	if !r.nextDeadline(t).Equal(retry.NextAttempt) {
		t.Fatalf("next deadline %s, the retry is at %s", r.nextDeadline(t), retry.NextAttempt)
	}
	wait := retry.NextAttempt.Sub(r.a.Now())
	r.quietFor(t, wait-time.Second)
	r.advance(time.Second)
	if !r.round(t) {
		t.Fatal("the team was not visited when the retry was due")
	}
	if d := findDelivery(t, r.a, r.p, "retry"); d.State != "accepted" {
		t.Fatalf("the retry was not sent: %+v", d)
	}
}

func TestAnUnclaimedIncidentIsVisitedAtItsRepeatAndItsResponseDeadline(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	ctx := context.Background()
	r.settle(t)
	if _, err := r.a.ManualSOS(ctx, r.p, "Database down", ""); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	unclaimed := func() Incident {
		t.Helper()
		incidents, err := r.a.ListIncidents(ctx, r.p)
		if err != nil || len(incidents) != 1 {
			t.Fatal(incidents, err)
		}
		return incidents[0]
	}
	if len(r.sender.messages()) == 0 {
		t.Fatal("the SOS was not sent")
	}
	// Nothing happens for the incident until its next repeat or its response
	// deadline, whichever comes first.
	for range 3 {
		i := unclaimed()
		next := earliest(i.RepeatAt, i.Deadline)
		if !r.nextDeadline(t).Equal(next) {
			t.Fatalf("next deadline %s, the incident's repeat is at %s and its response deadline at %s", r.nextDeadline(t), i.RepeatAt, i.Deadline)
		}
		r.quietFor(t, next.Sub(r.a.Now())-time.Second)
		r.advance(time.Second)
		if !r.round(t) {
			t.Fatal("the team was not visited when its incident had something due")
		}
		r.settle(t)
		// The visit did the incident's work: the next repeat is scheduled, or
		// the incident moved to its next step.
		if after := unclaimed(); !after.RepeatAt.After(i.RepeatAt) && after.Revision == i.Revision {
			t.Fatalf("the visit at %s left the incident as it was: %+v", next, after)
		}
	}
	i := unclaimed()
	if _, err := r.a.ClaimIncident(ctx, r.p, i.ID, i.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	if !r.nextDeadline(t).IsZero() {
		t.Fatalf("a claimed incident leaves a deadline: %s", r.nextDeadline(t))
	}
	r.quietFor(t, 3*time.Minute)
}

func TestCoverageDeadlines(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	ctx := context.Background()
	r.settle(t)
	// Both waits end before the team's periodic visit.
	until := r.a.Now().Add(4 * time.Minute)
	if _, err := r.a.SetPersonalAvailability(ctx, r.p, "unavailable", until, 0); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	if !r.nextDeadline(t).Equal(until) {
		t.Fatalf("next deadline %s, the unavailability ends at %s", r.nextDeadline(t), until)
	}
	r.quietFor(t, 4*time.Minute-time.Second)
	r.advance(time.Second)
	r.settle(t)
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM coverage_changes WHERE kind='personal_unavailable_expired' AND occurred_at=?`, until.Unix()); n != 1 {
		t.Fatalf("the end of the unavailability was recorded %d times at the moment it came", n)
	}
	if !r.nextDeadline(t).IsZero() {
		t.Fatalf("a deadline is left: %s", r.nextDeadline(t))
	}

	mute := r.a.Now().Add(4 * time.Minute)
	if _, err := r.a.SetTeamMute(ctx, r.p, mute, 0); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	if !r.nextDeadline(t).Equal(mute) {
		t.Fatalf("next deadline %s, the team mute ends at %s", r.nextDeadline(t), mute)
	}
	r.quietFor(t, 4*time.Minute-time.Second)
	r.advance(time.Second)
	r.settle(t)
	if titles := r.systemTitles(t); !slices.Contains(titles, "The team mute ended: MMHotline wakes people again") {
		t.Fatalf("the end of the team mute was not recorded when it came: %v", titles)
	}
}

func TestScheduleHandoffIsObservedAtItsTime(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	ctx := context.Background()
	_, barbara := addMember(t, r.a, "Barbara", RoleMember)
	second := barbara.ID
	first, err := r.a.CurrentMember(ctx, r.p)
	if err != nil {
		t.Fatal(err)
	}
	// The clock is at 12:00 UTC on Tuesday 2026-09-29. Handoff is at 09:30.
	assignments := []string{first.ID, first.ID, first.ID, second, first.ID, first.ID, first.ID}
	if _, err = r.a.SaveSchedule(ctx, r.p, ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", HandoffHour: 9, HandoffMinute: 30, Assignments: assignments, BackupKind: "everyone_available"}, 0); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	handoff := time.Date(2026, 9, 30, 9, 30, 0, 0, time.UTC)
	if !r.nextDeadline(t).Equal(handoff) {
		t.Fatalf("next deadline %s, the next handoff is at %s", r.nextDeadline(t), handoff)
	}
	before := len(r.systemTitles(t))
	r.advance(handoff.Sub(r.a.Now()) - time.Second)
	r.round(t) // A periodic visit can occur before the handoff.
	r.settle(t)
	r.advance(time.Second)
	if !r.round(t) {
		t.Fatal("the team was not visited at the handoff")
	}
	r.settle(t)
	if titles := r.systemTitles(t); len(titles) != before+1 || !strings.HasSuffix(titles[before], " is on call now, by the schedule") {
		t.Fatalf("handoff notice: %v", titles[before:])
	}
	if next := handoff.Add(24 * time.Hour); !r.nextDeadline(t).Equal(next) {
		t.Fatalf("next deadline %s, the handoff after is at %s", r.nextDeadline(t), next)
	}
}

func TestNextScheduleHandoffForEachKindOfSchedule(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 12, 10, 20, 0, time.UTC)
	for _, c := range []struct {
		config ScheduleConfig
		want   time.Time
	}{
		{ScheduleConfig{Kind: "hour_of_day", TimeZone: "UTC"}, time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC)},
		// Kolkata is 5 hours 30 minutes ahead: its next hour starts at :30 UTC.
		{ScheduleConfig{Kind: "hour_of_day", TimeZone: "Asia/Kolkata"}, time.Date(2026, 9, 29, 12, 30, 0, 0, time.UTC)},
		{ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", HandoffHour: 18}, time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)},
		{ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", HandoffHour: 9}, time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)},
		// New York is 4 hours behind in September: 09:00 there is 13:00 UTC.
		{ScheduleConfig{Kind: "weekly_rotation", TimeZone: "America/New_York", HandoffHour: 9}, time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC)},
	} {
		got, err := nextScheduleHandoff(context.Background(), nil, ScheduleState{ActiveKind: c.config.Kind, Configs: []ScheduleConfig{c.config}}, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s in %s: next handoff %s, want %s (%v)", c.config.Kind, c.config.TimeZone, got, c.want, err)
		}
	}
	if got, err := nextScheduleHandoff(context.Background(), nil, ScheduleState{}, now); err != nil || !got.IsZero() {
		t.Errorf("no active schedule: %s %v", got, err)
	}
}

func TestASilentAlertSourceIsVisitedAtItsStoppedMarkItsWarningAndItsSOS(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	ctx := context.Background()
	source, err := createAlertSource(ctx, r.a, r.p, "Monitoring on util.example.com")
	if err != nil {
		t.Fatal(err)
	}
	checkIn := func() {
		t.Helper()
		body := `{"sent_at":"` + r.a.Now().UTC().Format(time.RFC3339) + `","sent_by_node_name":"util"}`
		if w := checkInHTTP(r.a, r.p, source, body, r.a.Now()); w.Code != 200 {
			t.Fatalf("check-in: %d %s", w.Code, w.Body.String())
		}
	}
	checkIn()
	r.settle(t)
	// Check-ins every 30 seconds: no visit, although nothing tells the
	// scheduler when the next one comes.
	for range 4 {
		r.quietFor(t, 30*time.Second)
		checkIn()
		r.settle(t)
	}
	r.quietFor(t, 59*time.Second)
	r.advance(time.Second)
	if !r.round(t) {
		t.Fatal("an alert source silent for a minute did not make its team due")
	}
	r.settle(t)
	// Marked as stopped. The team is left alone until the Warning time, 2
	// minutes after the last check-in, and then until the SOS time.
	r.quietFor(t, 59*time.Second)
	r.advance(time.Second)
	if !r.round(t) {
		t.Fatal("the team was not visited at the Warning time")
	}
	r.settle(t)
	if titles := r.systemTitles(t); !slices.ContainsFunc(titles, func(title string) bool { return strings.Contains(title, "hasn't checked in with MMHotline") }) {
		t.Fatalf("no Warning 2 minutes after the last check-in: %v", titles)
	}
	r.quietFor(t, 3*time.Minute-time.Second)
	r.advance(time.Second)
	if !r.round(t) {
		t.Fatal("the team was not visited at the SOS time")
	}
	r.settle(t)
	incidents, err := r.a.ListIncidents(ctx, r.p)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("no SOS 5 minutes after the last check-in: %+v %v", incidents, err)
	}
	if _, err = r.a.ClaimIncident(ctx, r.p, incidents[0].ID, incidents[0].Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	r.quietFor(t, 2*time.Minute)
	// A person resolves the incident while the alert source is still silent:
	// the SOS comes again once the SOS time has passed.
	claimed, err := r.a.GetIncident(ctx, r.p, incidents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ResolveIncident(ctx, r.p, claimed.ID, claimed.Revision, ""); err != nil {
		t.Fatal(err)
	}
	for range 5 * 60 {
		r.advance(time.Second)
		r.round(t)
	}
	if incidents, err = r.a.ListIncidents(ctx, r.p); err != nil || len(incidents) != 1 || incidents[0].ID == claimed.ID {
		t.Fatalf("no new SOS after a person resolved the first one: %+v %v", incidents, err)
	}
}

func TestASilentAlertSourceInPlannedDowntimeIsVisitedWhenTheDowntimeEnds(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	ctx := context.Background()
	source, err := createAlertSource(ctx, r.a, r.p, "Monitoring on util.example.com")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"sent_at":"` + r.a.Now().UTC().Format(time.RFC3339) + `","sent_by_node_name":"util"}`
	if w := checkInHTTP(r.a, r.p, source, body, r.a.Now()); w.Code != 200 {
		t.Fatalf("check-in: %d %s", w.Code, w.Body.String())
	}
	if err = r.a.StartPlannedDowntime(ctx, r.p, plannedDowntimeTarget{AlertSourceID: source.Source.ID}, r.a.Now().Add(6*time.Minute), "moving the database"); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	visitedAfter := func(wait time.Duration, what string) {
		t.Helper()
		r.quietFor(t, wait-time.Second)
		r.advance(time.Second)
		if !r.round(t) {
			t.Fatalf("the team was not visited %s", what)
		}
		r.settle(t)
	}
	visitedAfter(time.Minute, "when its alert source had been silent for a minute")
	visitedAfter(5*time.Minute, "when the planned downtime ended")
	if incidents, err := r.a.ListIncidents(ctx, r.p); err != nil || len(incidents) != 0 {
		t.Fatalf("an SOS at the end of the planned downtime, a minute early: %+v %v", incidents, err)
	}
	visitedAfter(time.Minute, "a minute after the planned downtime ended")
	if incidents, err := r.a.ListIncidents(ctx, r.p); err != nil || len(incidents) != 1 {
		t.Fatalf("no SOS a minute after the planned downtime ended: %+v %v", incidents, err)
	}
}

func TestCustomScheduleCodeIsKnownAfterRestart(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	ctx := context.Background()
	if usesCode, err := h.app.usesCustomScheduleCode(ctx); err != nil || usesCode {
		t.Fatalf("without custom schedule code: %v %v", usesCode, err)
	}
	h.save(t, customConfig(weekdayWeekendCode))
	if usesCode, err := h.app.usesCustomScheduleCode(ctx); err != nil || !usesCode {
		t.Fatalf("with custom schedule code: %v %v", usesCode, err)
	}
	h.app = restartApp(t, h.app)
	if usesCode, err := h.app.usesCustomScheduleCode(ctx); err != nil || usesCode {
		t.Fatalf("custom schedule before the first visit: %v %v", usesCode, err)
	}
	if !h.app.backgroundRound(ctx, func(err error) { t.Errorf("background work: %v", err) }) {
		t.Fatal("the team was not visited after restart")
	}
	if usesCode, err := h.app.usesCustomScheduleCode(ctx); err != nil || !usesCode {
		t.Fatalf("custom schedule after the first visit: %v %v", usesCode, err)
	}
}

// restartApp reopens the same state directory with empty process memory.
func restartApp(t *testing.T, previous *App) *App {
	t.Helper()
	if err := previous.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(previous.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(store)
	restarted.Now = previous.Now
	restarted.WebURL = previous.WebURL
	restarted.ContactMethodTypes = previous.ContactMethodTypes
	t.Cleanup(func() { restarted.Store.Close() })
	return restarted
}

// backgroundRounds runs production rounds until the team has no work due.
func backgroundRounds(t *testing.T, a *App) {
	t.Helper()
	for range 50 {
		if !a.backgroundRound(context.Background(), func(err error) { t.Errorf("background work: %v", err) }) {
			return
		}
	}
	t.Fatal("the background scheduler never ran out of work")
}

func TestAfterARestartKnownCallsArePolledBeforeAnotherCall(t *testing.T) {
	t.Parallel()
	r := newPhoneRig(t)
	r.addPhone(t, r.s.Principal, barbaraNumber)
	r.sos(t, "Database down")
	r.dispatch(t)
	first := r.lastCallTo(t, barbaraNumber)
	r.minuteLater(t, time.Minute)
	r.a = restartApp(t, r.a)
	// Startup rebuilds adapters, including their empty polling memory.
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
		t.Fatal(err)
	}
	r.sendingServices = r.a.twilioSendingService()
	backgroundRounds(t, r.a)
	if n := len(r.callsTo(barbaraNumber)); n != 1 {
		t.Fatalf("overlapped after restart: %d", n)
	}
	if _, err := r.fake.AnswerCall(first.IDFromSendingService, fakesendingservices.CallNoAnswer); err != nil {
		t.Fatal(err)
	}
	r.advance(4 * time.Second)
	backgroundRounds(t, r.a)
	if n := len(r.callsTo(barbaraNumber)); n != 1 {
		t.Fatal("restart polled before five seconds")
	}
	r.advance(time.Second)
	backgroundRounds(t, r.a)
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM delivery_attempts WHERE id_from_sending_service=? AND call_result=?`, first.IDFromSendingService, callNoAnswer); n != 1 {
		t.Fatal("restart lost call result")
	}
	if n := len(r.callsTo(barbaraNumber)); n != 2 {
		t.Fatalf("waiting call did not go: %d", n)
	}
}

func TestAfterARestartTheCallThatWasReservedIsMade(t *testing.T) {
	t.Parallel()
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	r.sos(t, "Database down")
	// The process reserves the call and stops before it saves a result.
	if d, _, err := r.a.reserveDelivery(ctx, "https://mmhotline.example.com", []string{"automated_voice_call"}); err != nil || d.ID == "" {
		t.Fatal(err)
	}
	// mmhotline serve starts again before the reservation's 30 seconds end.
	r.advance(10 * time.Second)
	r.a = restartApp(t, r.a)
	r.server.Config.Handler = r.a.Handler()
	backgroundRounds(t, r.a)
	if n := len(r.callsTo(barbaraNumber)); n != 1 {
		t.Fatalf("calls after mmhotline serve starts: %d", n)
	}
	if d := lastEscalation(t, deliveriesOf(t, r.a, call.ID)); d.State != "accepted" || d.Attempts != 2 {
		t.Fatalf("the reserved call: %+v", d)
	}
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM delivery_attempts WHERE contact_method_type='automated_voice_call' AND number=1 AND state='uncertain' AND diagnostic=?`, processRestartedReason); n != 1 {
		t.Fatal("the attempt from before the restart is not recorded as uncertain")
	}
}

// A recovered call refused by Twilio fails and reaches the other contact methods.
func TestAfterARestartAReservedCallRefusedByTwilioFallsBack(t *testing.T) {
	t.Parallel()
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	email := r.savedEmail(t, p, "sos")
	e := r.sos(t, "Database down")
	if d, _, err := r.a.reserveDelivery(ctx, "https://mmhotline.example.com", []string{"automated_voice_call"}); err != nil || d.ID == "" {
		t.Fatal(err)
	}
	r.fake.SetNextSendingServiceAction("twilio", fakesendingservices.RefuseNextRequest)
	r.advance(10 * time.Second)
	r.a = restartApp(t, r.a)
	r.server.Config.Handler = r.a.Handler()
	backgroundRounds(t, r.a)
	calls := r.callsTo(barbaraNumber)
	if len(calls) != 0 {
		t.Fatalf("calls after Twilio refused the call recovered after the restart: %+v", calls)
	}
	if d := lastEscalation(t, deliveriesOf(t, r.a, call.ID)); d.State != "failed" || d.Attempts != 2 {
		t.Fatalf("the call: %+v", d)
	}
	if fallbacks(t, r, email.ID)[e.IncidentID] != 1 {
		t.Fatal("refused recovered call did not fall back")
	}
}

func TestAfterARestartTheEmailThatWasReservedIsSentAtOnce(t *testing.T) {
	t.Parallel()
	r := newDeadlineRig(t)
	queueDelivery(t, r.a, r.p, "reserved", "warn")
	if d, _, err := r.a.reserveDelivery(context.Background(), "https://mmhotline.example.com", []string{"email"}); err != nil || d.ID == "" {
		t.Fatal(err)
	}
	before := len(r.sender.messages())
	r.advance(10 * time.Second)
	r.a = restartApp(t, r.a)
	backgroundRounds(t, r.a)
	if d := findDelivery(t, r.a, r.p, "reserved"); len(r.sender.messages()) != before+1 || d.State != "accepted" || d.Attempts != 2 {
		t.Fatalf("%d emails after mmhotline serve starts; delivery %+v", len(r.sender.messages())-before, d)
	}
}
