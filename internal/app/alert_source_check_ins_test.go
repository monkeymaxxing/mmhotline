package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func checkInHTTP(a *App, p Principal, source CreatedSource, body string, at time.Time) *httptest.ResponseRecorder {
	mux := a.IntakeHandler()
	r := httptest.NewRequest("POST", "/check-in/"+source.Source.ID, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Mmhotline-Signature", alertSignature(source.Secret, at, []byte(body)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// checkInRig is a team with an alert source named like one the monitoring
// tool sets up, and a movable clock.
type checkInRig struct {
	a      *App
	p      Principal
	source CreatedSource
	clock  time.Time
}

func newCheckInRig(t *testing.T) *checkInRig {
	t.Helper()
	a := foundationApp(t)
	r := &checkInRig{a: a, clock: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	a.Now = func() time.Time { return r.clock }
	s := memberFixture(t, a, "checkin@example.test")
	giveWakingContactMethod(t, a, "checkin@example.test")
	r.p = s.Principal
	var err error
	if r.source, err = createAlertSource(context.Background(), a, r.p, "Monitoring on util.example.com"); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *checkInRig) checkIn(t *testing.T, maintenance string) {
	t.Helper()
	body := `{"sent_at":"` + r.clock.Format(time.RFC3339) + `","sent_by_node_name":"util"`
	if maintenance != "" {
		body += `,"nodes_in_whole_node_maintenance":` + maintenance
	}
	body += `}`
	if w := checkInHTTP(r.a, r.p, r.source, body, r.clock); w.Code != 200 {
		t.Fatalf("check-in: %d %s", w.Code, w.Body.String())
	}
}

// at moves the clock and runs the watcher once.
func (r *checkInRig) at(t *testing.T, d time.Duration) {
	t.Helper()
	r.clock = r.clock.Add(d)
	if err := r.a.WatchAlertSourceCheckIns(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// restart stands in for MMHotline stopping and starting again: a new App on
// the same state directory, with nothing in memory.
func (r *checkInRig) restart(t *testing.T) {
	t.Helper()
	if err := r.a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(r.a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	r.a = New(reopened)
	r.a.Now = func() time.Time { return r.clock }
}

func (r *checkInRig) titles(t *testing.T) []string {
	t.Helper()
	var titles []string
	err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT severity||': '||title FROM alerts WHERE source_id='system' AND title NOT LIKE '% planned downtime for %' ORDER BY sequence`)
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

func TestCheckInRouteAndReceipt(t *testing.T) {
	r := newCheckInRig(t)
	body := `{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util","nodes_in_whole_node_maintenance":[{"node_name":"web-1","maintenance_until":"2026-09-29T12:30:00Z","reason":"moving the database","started_by":"matt"}]}`
	w := checkInHTTP(r.a, r.p, r.source, body, r.clock)
	if w.Code != 200 || !verifyReceipt(t, r.source.Secret, "check-in", body, r.clock, w) || !strings.Contains(w.Body.String(), `"check_in_received_at":"2026-09-29T12:00:00Z"`) {
		t.Fatalf("check-in answer: %d %s", w.Code, w.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || len(answer) != 1 || answer["check_in_received_at"] != "2026-09-29T12:00:00Z" {
		t.Fatalf("check-in answer fields: %s (%v)", w.Body.String(), err)
	}
	status, err := r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID)
	if err != nil || !status.Expected || !status.LastCheckInAt.Equal(r.clock) || len(status.NodesInWholeNodeMaintenance) != 1 || status.SenderDescription != "alert source “Monitoring on util.example.com”" {
		t.Fatalf("status: %+v %v", status, err)
	}
	alertBody := `{"title":"x","message":""}`
	if w = checkInHTTP(r.a, r.p, r.source, alertBody, r.clock); w.Code != 400 {
		t.Fatalf("an alert body at the check-in route: %d", w.Code)
	}
	if w = intakeHTTP(r.a, r.p, r.source, body, r.clock); w.Code != 400 {
		t.Fatalf("a check-in at alert intake: %d", w.Code)
	}
	wrong := r.source
	wrong.Secret = strings.Repeat("ab", 32)
	if w = checkInHTTP(r.a, r.p, wrong, body, r.clock); w.Code != 401 {
		t.Fatalf("wrong signature: %d", w.Code)
	}
	for n := 0; n < maxAlertSourceCheckInsPerMinute; n++ {
		checkInHTTP(r.a, r.p, r.source, body, r.clock)
	}
	if w = checkInHTTP(r.a, r.p, r.source, body, r.clock); w.Code != 429 {
		t.Fatalf("check-ins beyond the minute's limit: %d", w.Code)
	}
}

func TestMissingCheckInsWarnThenSOSThenResolve(t *testing.T) {
	r := newCheckInRig(t)
	r.at(t, 10*time.Minute)
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("an alert source that never sent a check-in is not expected to: %v", titles)
	}
	r.checkIn(t, "")
	r.at(t, 119*time.Second)
	if len(r.titles(t)) != 0 {
		t.Fatal("warned too early")
	}
	r.at(t, time.Second)
	r.at(t, 2*time.Minute)
	if titles := r.titles(t); len(titles) != 1 || titles[0] != "warn: “Monitoring on util.example.com” hasn't checked in with MMHotline for 2 minutes: it may have stopped, or its server may be down" {
		t.Fatalf("after 2 minutes: %v", titles)
	}
	r.at(t, time.Minute)
	titles := r.titles(t)
	if len(titles) != 2 || titles[1] != "sos: “Monitoring on util.example.com” hasn't checked in with MMHotline for 5 minutes: it may have stopped, or its server may be down" {
		t.Fatalf("after 5 minutes: %v", titles)
	}
	incidents, err := r.a.ListIncidents(context.Background(), r.p)
	if err != nil || len(incidents) != 1 || incidents[0].Status != "open" || incidents[0].SenderIncidentKey != "check-ins:"+r.source.Source.ID {
		t.Fatalf("incident: %+v %v", incidents, err)
	}
	r.at(t, 10*time.Minute)
	if len(r.titles(t)) != 2 {
		t.Fatal("repeated alerts while still silent")
	}
	// An alert counts as hearing from the alert source, so nothing new is
	// sent, but only a check-in clears the SOS.
	alertReceipt(t, intakeHTTP(r.a, r.p, r.source, `{"sender_alert_id":"back","title":"Disk ok","message":""}`, r.clock))
	r.at(t, 10*time.Minute)
	if titles = r.titles(t); len(titles) != 2 {
		t.Fatalf("an alert cleared or repeated the SOS: %v", titles)
	}
	r.checkIn(t, "")
	r.at(t, time.Second)
	titles = r.titles(t)
	if titles[len(titles)-1] != "resolved-sos: “Monitoring on util.example.com” checks in with MMHotline again" {
		t.Fatalf("resolution: %v", titles)
	}
	if i, err := r.a.GetIncident(context.Background(), r.p, incidents[0].ID); err != nil || i.Status != "resolved" {
		t.Fatalf("unclaimed incident not resolved: %+v %v", i, err)
	}
}

func TestClaimedCheckInIncidentStaysOpen(t *testing.T) {
	r := newCheckInRig(t)
	email := savedEmail(t, r.a, r.p, true)
	r.checkIn(t, "")
	r.at(t, 5*time.Minute)
	incidents, err := r.a.ListIncidents(context.Background(), r.p)
	if err != nil || len(incidents) != 1 {
		t.Fatal(incidents, err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.p, incidents[0].ID, incidents[0].Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.at(t, time.Minute)
	r.checkIn(t, "")
	r.at(t, time.Second)
	i, err := r.a.GetIncident(context.Background(), r.p, incidents[0].ID)
	if err != nil || i.Status != "claimed" {
		t.Fatalf("a claimed incident was resolved for its owner: %+v %v", i, err)
	}
	audit, err := r.a.IncidentAudit(context.Background(), r.p, i.ID)
	if err != nil || !strings.HasPrefix(audit[len(audit)-1].Message, "Check-ins from “Monitoring on util.example.com” resumed at ") {
		t.Fatalf("history: %+v %v", audit, err)
	}
	// The owner gets one message that does not wake them.
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM deliveries WHERE purpose='system_notice' AND severity='info' AND incident_id=? AND contact_method_id=?`, i.ID, email.ID); n != 1 {
		t.Fatalf("messages to the owner: %d", n)
	}
}

func TestWarningOnlyEpisodeResolvesWithResolvedWarn(t *testing.T) {
	r := newCheckInRig(t)
	r.checkIn(t, "")
	r.at(t, 3*time.Minute)
	r.checkIn(t, "")
	r.at(t, time.Second)
	titles := r.titles(t)
	if len(titles) != 2 || titles[1] != "resolved-warn: “Monitoring on util.example.com” checks in with MMHotline again" {
		t.Fatalf("titles: %v", titles)
	}
}

func TestCheckInTimesAndOff(t *testing.T) {
	r := newCheckInRig(t)
	ctx := context.Background()
	for _, bad := range [][2]time.Duration{{5 * time.Minute, 5 * time.Minute}, {30 * time.Second, 5 * time.Minute}, {0, 61 * time.Minute}, {time.Minute, 90 * time.Second}} {
		if err := setAlertSourceCheckInTimes(ctx, r.a, r.p, r.source.Source.ID, bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
	if err := setAlertSourceCheckInTimes(ctx, r.a, r.p, r.source.Source.ID, 0, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	r.checkIn(t, "")
	r.at(t, 2*time.Minute)
	r.at(t, time.Minute)
	if titles := r.titles(t); len(titles) != 1 || !strings.HasPrefix(titles[0], "sos: ") {
		t.Fatalf("Warning off, SOS at 3 minutes: %v", titles)
	}
}

func TestPlannedDowntimeByHandForCheckIns(t *testing.T) {
	r := newCheckInRig(t)
	ctx := context.Background()
	r.checkIn(t, "")
	target := plannedDowntimeTarget{AlertSourceID: r.source.Source.ID}
	if err := r.a.StartPlannedDowntime(ctx, r.p, target, r.clock.Add(8*24*time.Hour), ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("planned downtime more than 7 days ahead")
	}
	if err := r.a.StartPlannedDowntime(ctx, r.p, target, r.clock.Add(time.Hour), "moving the database"); err != nil {
		t.Fatal(err)
	}
	status, err := r.a.AlertSourceCheckInStatus(ctx, r.p, r.source.Source.ID)
	if err != nil || status.PlannedDowntime == nil || status.PlannedDowntime.Text() != "Planned downtime until 2026-09-29T13:00:00Z, started by Responder: moving the database." {
		t.Fatalf("status: %+v %v", status.PlannedDowntime, err)
	}
	r.at(t, 30*time.Minute)
	r.at(t, 30*time.Minute)
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("alerted during planned downtime: %v", titles)
	}
	r.at(t, 59*time.Second)
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("alerted before one more minute: %v", titles)
	}
	r.at(t, time.Second)
	if titles := r.titles(t); len(titles) != 1 || !strings.HasPrefix(titles[0], "sos: “Monitoring on util.example.com” hasn't checked in with MMHotline for 61 minutes: it may have stopped, or its server may be down") {
		t.Fatalf("after planned downtime: %v", titles)
	}
	if err = r.a.EndPlannedDowntime(ctx, r.p, target); !errors.Is(err, ErrConflict) {
		t.Fatalf("ended planned downtime that already ended: %v", err)
	}
}

func TestPlannedDowntimeEndedEarlyByHand(t *testing.T) {
	r := newCheckInRig(t)
	ctx := context.Background()
	r.checkIn(t, "")
	target := plannedDowntimeTarget{AlertSourceID: r.source.Source.ID}
	if err := r.a.StartPlannedDowntime(ctx, r.p, target, r.clock.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	r.at(t, 10*time.Minute)
	if err := r.a.EndPlannedDowntime(ctx, r.p, target); err != nil {
		t.Fatal(err)
	}
	r.at(t, 59*time.Second)
	if len(r.titles(t)) != 0 {
		t.Fatal("alerted before one more minute")
	}
	r.at(t, time.Second)
	if titles := r.titles(t); len(titles) != 1 || !strings.HasPrefix(titles[0], "sos: ") {
		t.Fatalf("after ending early: %v", titles)
	}
}

func TestSenderMaintenanceWindowOnTheSendingNodeIsPlannedDowntime(t *testing.T) {
	r := newCheckInRig(t)
	until := r.clock.Add(30 * time.Minute).Format(time.RFC3339)
	// A window for another node, or only some check groups (not listed), does
	// not count for the sending node's check-ins.
	r.checkIn(t, fmt.Sprintf(`[{"node_name":"web-1","maintenance_until":%q},{"node_name":"util","maintenance_until":%q,"reason":"upgrading","started_by":"matt"}]`, until, until))
	status, err := r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID)
	if err != nil || status.PlannedDowntime == nil || status.PlannedDowntime.Text() != "Planned downtime until 2026-09-29T12:30:00Z, because the check-ins list server util as in maintenance, started by matt: upgrading." {
		t.Fatalf("status: %+v %v", status.PlannedDowntime, err)
	}
	// The sending node goes silent during its window; MMHotline keeps honoring the last
	// window it received until its end time.
	r.at(t, 29*time.Minute)
	if len(r.titles(t)) != 0 {
		t.Fatal("alerted during the maintenance window listed in the check-ins")
	}
	r.at(t, 90*time.Second)
	r.at(t, 30*time.Second)
	if titles := r.titles(t); len(titles) != 1 || !strings.HasPrefix(titles[0], "sos: ") {
		t.Fatalf("one minute after the window ended: %v", titles)
	}
}

func TestCheckInDecodingIsStrict(t *testing.T) {
	for _, bad := range []string{
		`{}`,
		`{"sent_at":"2026-09-29T12:00:00Z"}`,
		`{"sent_at":"yesterday","sent_by_node_name":"util"}`,
		`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util","extra":1}`,
		`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util","nodes_in_whole_node_maintenance":[{"node_name":"web-1"}]}`,
		`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"} {}`,
	} {
		if _, err := decodeAlertSourceCheckIn([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted", bad)
		}
	}
	m, err := decodeAlertSourceCheckIn([]byte(`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"}`))
	if raw, _ := json.Marshal(m.NodesInWholeNodeMaintenance); err != nil || string(raw) != "null" {
		t.Fatalf("empty list: %s %v", raw, err)
	}
}

func TestAlertSourcePageCheckInsAndPlannedDowntime(t *testing.T) {
	h := newWorkflowRig(t)
	owner := h.browser()
	p := owner.addPerson("owner@example.com", "Matt")
	source, err := createAlertSource(context.Background(), h.app, p, "Monitoring on util.example.com")
	if err != nil {
		t.Fatal(err)
	}
	page := owner.get("/alert-sources/"+source.Source.ID, 200)
	if !strings.Contains(page, "Waiting for the first check-in") {
		t.Fatal("waiting text missing")
	}
	body := []byte(`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"}`)
	if _, _, err = h.app.ReceiveAlertSourceCheckIn(context.Background(), source.Source.ID, alertSignature(source.Secret, h.app.Now(), body), body); err != nil {
		t.Fatal(err)
	}
	page = owner.get("/alert-sources/"+source.Source.ID, 200)
	if !strings.Contains(page, "Last check-in from alert source “Monitoring on util.example.com”: ") || !strings.Contains(page, "The sending program checks in every 30 seconds.") {
		t.Fatal("last check-in line missing")
	}
	// The settings file sets the check-in times; the page shows them, and
	// has no form for them.
	if err := setAlertSourceCheckInTimes(context.Background(), h.app, p, source.Source.ID, 0, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	owner.post("/alert-sources/"+source.Source.ID+"/check-in-times", url.Values{"csrf": {workflowField(t, page, "csrf")}, "warn_after_no_check_in_for": {"0"}, "sos_after_no_check_in_for": {"5"}}, 404)
	page = owner.get("/alert-sources/"+source.Source.ID, 200)
	if !strings.Contains(pageText(page), "Warning Off Wake the person on call After 10 minutes without an alert or check-in.") || !strings.Contains(pageText(page), "in alert_sources") || strings.Contains(page, "Save check-in times") {
		t.Fatal("the page does not show the times read-only")
	}
	until := h.app.Now().UTC().Add(2 * time.Hour).Format("2006-01-02T15:04")
	owner.post("/alert-sources/"+source.Source.ID+"/planned-downtime", url.Values{"csrf": {workflowField(t, page, "csrf")}, "until": {until}, "reason": {"moving the database"}}, 303)
	page = owner.get("/alert-sources/"+source.Source.ID, 200)
	if !strings.Contains(pageText(page), "started by Matt: moving the database.") || !strings.Contains(pageText(page), "MMHotline records check-ins but makes no alerts about missing check-ins during planned downtime.") || !strings.Contains(page, "End planned downtime now") {
		t.Fatal("planned downtime not shown")
	}
	owner.post("/alert-sources/"+source.Source.ID+"/planned-downtime", url.Values{"csrf": {workflowField(t, page, "csrf")}, "end": {"1"}}, 303)
	if page = owner.get("/alert-sources/"+source.Source.ID, 200); !strings.Contains(page, "Start planned downtime") {
		t.Fatal("planned downtime not ended")
	}
}

func TestCheckInWindowsAreCappedAndOrdered(t *testing.T) {
	r := newCheckInRig(t)
	far := r.clock.Add(100 * 24 * time.Hour).Format(time.RFC3339)
	r.checkIn(t, `[{"node_name":"util","maintenance_until":"`+far+`"}]`)
	status, err := r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID)
	if err != nil || status.PlannedDowntime == nil || !status.PlannedDowntime.Until.Equal(r.clock.Add(maxPlannedDowntimeAhead)) {
		t.Fatalf("a window listed in the check-ins was not capped at 7 days: %+v %v", status.PlannedDowntime, err)
	}
	// An older check-in, arriving late or replayed, does not replace the
	// newer list.
	old := `{"sent_at":"` + r.clock.Add(-time.Hour).Format(time.RFC3339) + `","sent_by_node_name":"util"}`
	if w := checkInHTTP(r.a, r.p, r.source, old, r.clock); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if status, _ = r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID); len(status.NodesInWholeNodeMaintenance) != 1 {
		t.Fatal("an older check-in replaced the maintenance list")
	}
	if _, err = decodeAlertSourceCheckIn([]byte(`{"Sent_At":"2026-09-29T12:00:00Z","sent_by_node_name":"util"}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("field names are matched without regard to case")
	}
}
