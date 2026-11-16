package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// watch reads one value from the alert source's row in check_in_watch.
func (r *checkInRig) watch(t *testing.T, column string) int64 {
	t.Helper()
	return scalarTeam(t, r.a, `SELECT `+column+` FROM check_in_watch WHERE alert_source_id=?`, r.source.Source.ID)
}

func TestCheckInsFromAHealthyAlertSourceAreNotWrittenToTheDatabase(t *testing.T) {
	r := newCheckInRig(t)
	r.checkIn(t, "")
	first := r.clock
	writeAheadLog := filepath.Join(r.a.Store.Dir, "mmhotline.db-wal")
	before, err := os.Stat(writeAheadLog)
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		r.at(t, 30*time.Second)
		r.checkIn(t, "")
	}
	after, err := os.Stat(writeAheadLog)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the team's database was written: write-ahead log %d bytes at %s, then %d bytes at %s: %v", before.Size(), before.ModTime(), after.Size(), after.ModTime(), err)
	}
	if at := r.watch(t, "last_check_in_at"); at != first.Unix() {
		t.Fatalf("the database has a check-in of %d, the first was at %d", at, first.Unix())
	}
	status, err := r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID)
	if err != nil || !status.LastCheckInAt.Equal(r.clock) || !status.LastHeardAt.Equal(r.clock) {
		t.Fatalf("the alert source page does not show the latest check-in: %+v %v", status, err)
	}
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("alerted about a healthy alert source: %v", titles)
	}
}

func TestAlertSourceMarkedAsStoppedKeepsItsTimesAcrossARestart(t *testing.T) {
	r := newCheckInRig(t)
	r.checkIn(t, "")
	lastCheckIn := r.clock
	r.at(t, 59*time.Second)
	if r.watch(t, "check_ins_stopped") != 0 {
		t.Fatal("marked as stopped before a minute had passed")
	}
	r.at(t, time.Second)
	if r.watch(t, "check_ins_stopped") != 1 || r.watch(t, "last_check_in_at") != lastCheckIn.Unix() {
		t.Fatal("not marked as stopped, with the time of the last check-in, after a minute")
	}
	r.clock = r.clock.Add(10 * time.Second)
	r.restart(t)
	status, err := r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID)
	if err != nil || !status.LastCheckInAt.Equal(lastCheckIn) {
		t.Fatalf("the alert source page after the restart: %+v %v", status, err)
	}
	r.at(t, 49*time.Second)
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("warned too early after the restart: %v", titles)
	}
	r.at(t, time.Second)
	if titles := r.titles(t); len(titles) != 1 || titles[0] != "warn: “Monitoring on util.example.com” hasn't checked in with MMHotline for 2 minutes: it may have stopped, or its server may be down" {
		t.Fatalf("2 minutes after the last check-in: %v", titles)
	}
	r.at(t, 3*time.Minute)
	if titles := r.titles(t); len(titles) != 2 || titles[1] != "sos: “Monitoring on util.example.com” hasn't checked in with MMHotline for 5 minutes: it may have stopped, or its server may be down" {
		t.Fatalf("5 minutes after the last check-in: %v", titles)
	}
}

// An alert source that was still checking in when MMHotline
// stopped has the time of its last one in no database. Its silence counts
// from when the new MMHotline read the team's database.
func TestRestartCountsSilenceFromTheRestartForAnAlertSourceHeardInTheLastMinute(t *testing.T) {
	r := newCheckInRig(t)
	r.checkIn(t, "")
	r.clock = r.clock.Add(30 * time.Second)
	r.restart(t)
	r.at(t, 0)
	r.at(t, 119*time.Second)
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("warned before 2 minutes had passed since the restart: %v", titles)
	}
	r.at(t, time.Second)
	if titles := r.titles(t); len(titles) != 1 || titles[0] != "warn: “Monitoring on util.example.com” hasn't checked in with MMHotline for 2 minutes: it may have stopped, or its server may be down" {
		t.Fatalf("2 minutes after the restart: %v", titles)
	}
	r.at(t, 3*time.Minute)
	if titles := r.titles(t); len(titles) != 2 || !strings.HasPrefix(titles[1], "sos: ") {
		t.Fatalf("a restart made the silent alert source look healthy: %v", titles)
	}
}

func TestCheckInsAfterARestartNeedNoDatabaseWrite(t *testing.T) {
	r := newCheckInRig(t)
	r.checkIn(t, "")
	first := r.clock
	r.clock = r.clock.Add(20 * time.Second)
	r.restart(t)
	// The check-in arrives before anything else read the team's database.
	r.clock = r.clock.Add(10 * time.Second)
	r.checkIn(t, "")
	r.at(t, 59*time.Second)
	if r.watch(t, "last_check_in_at") != first.Unix() || r.watch(t, "check_ins_stopped") != 0 {
		t.Fatal("a check-in from a healthy alert source was written after a restart")
	}
	status, err := r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID)
	if err != nil || !status.LastCheckInAt.Equal(r.clock.Add(-59*time.Second)) {
		t.Fatalf("the alert source page: %+v %v", status, err)
	}
}

func TestSigningSecretChangesReachCheckInTracking(t *testing.T) {
	r := newCheckInRig(t)
	ctx := context.Background()
	body := `{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"}`
	r.checkIn(t, "")
	first := r.clock
	r.clock = r.clock.Add(30 * time.Second)
	second, err := addSigningSecret(ctx, r.a, r.p, r.source.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := checkInHTTP(r.a, r.p, second, body, r.clock); w.Code != 200 {
		t.Fatalf("an added signing secret was refused: %d", w.Code)
	}
	if r.watch(t, "last_check_in_at") != first.Unix() {
		t.Fatal("an unchanged check-in signed with a second signing secret caused a database write")
	}
	if err = revokeSigningSecret(ctx, r.a, r.p, r.source.Source.ID, r.source.SigningSecretID); err != nil {
		t.Fatal(err)
	}
	if w := checkInHTTP(r.a, r.p, r.source, body, r.clock); w.Code != 401 {
		t.Fatalf("a revoked signing secret still signs check-ins: %d", w.Code)
	}
	if w := checkInHTTP(r.a, r.p, second, body, r.clock); w.Code != 200 {
		t.Fatalf("the remaining signing secret was refused: %d", w.Code)
	}
	if err = disableAlertSource(ctx, r.a, r.p, r.source.Source.ID); err != nil {
		t.Fatal(err)
	}
	if w := checkInHTTP(r.a, r.p, second, body, r.clock); w.Code != 401 {
		t.Fatalf("a disabled alert source still checks in: %d", w.Code)
	}
	r.at(t, 10*time.Minute)
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("alerted about a disabled alert source: %v", titles)
	}
}

func TestCheckInThatSaysSomethingNewIsWritten(t *testing.T) {
	r := newCheckInRig(t)
	ctx := context.Background()
	downtime := func() *PlannedDowntimeView {
		t.Helper()
		status, err := r.a.AlertSourceCheckInStatus(ctx, r.p, r.source.Source.ID)
		if err != nil {
			t.Fatal(err)
		}
		return status.PlannedDowntime
	}
	r.checkIn(t, "")
	r.clock = r.clock.Add(30 * time.Second)
	r.checkIn(t, `[{"node_name":"util","maintenance_until":"`+r.clock.Add(30*time.Minute).Format(time.RFC3339)+`"}]`)
	if downtime() == nil {
		t.Fatal("a maintenance window that started was not written")
	}
	r.clock = r.clock.Add(30 * time.Second)
	r.checkIn(t, "")
	if downtime() != nil {
		t.Fatal("a maintenance window that ended was not written")
	}
	r.clock = r.clock.Add(30 * time.Second)
	body := `{"sent_at":"` + r.clock.Format(time.RFC3339) + `","sent_by_node_name":"util-2"}`
	if w := checkInHTTP(r.a, r.p, r.source, body, r.clock); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if scalarTeam(t, r.a, `SELECT count(*) FROM check_in_watch WHERE sent_by_node_name='util-2'`) != 1 {
		t.Fatal("another sending node was not written")
	}
	if r.watch(t, "last_check_in_at") != r.clock.Unix() {
		t.Fatal("the write did not record when the check-in arrived")
	}
}

func TestMaintenanceWindowBeyondSevenDaysIsStoredAgainOnceADay(t *testing.T) {
	r := newCheckInRig(t)
	list := `[{"node_name":"util","maintenance_until":"` + r.clock.Add(100*24*time.Hour).Format(time.RFC3339) + `"}]`
	until := func() time.Time {
		t.Helper()
		status, err := r.a.AlertSourceCheckInStatus(context.Background(), r.p, r.source.Source.ID)
		if err != nil || status.PlannedDowntime == nil {
			t.Fatalf("status: %+v %v", status, err)
		}
		return status.PlannedDowntime.Until
	}
	first := r.clock
	r.checkIn(t, list)
	r.clock = r.clock.Add(23 * time.Hour)
	r.checkIn(t, list)
	if !until().Equal(first.Add(maxPlannedDowntimeAhead)) || r.watch(t, "last_check_in_at") != first.Unix() {
		t.Fatal("an unchanged maintenance window was written again before a day had passed")
	}
	r.clock = r.clock.Add(time.Hour)
	r.checkIn(t, list)
	if !until().Equal(r.clock.Add(maxPlannedDowntimeAhead)) {
		t.Fatal("after a day, the window does not count for 7 days from the latest check-in")
	}
}

func TestAnAlertCountsAsHearingFromTheAlertSourceInMemory(t *testing.T) {
	r := newCheckInRig(t)
	r.checkIn(t, "")
	r.clock = r.clock.Add(50 * time.Second)
	alertReceipt(t, intakeHTTP(r.a, r.p, r.source, `{"sender_alert_id":"disk","title":"Disk ok","message":""}`, r.clock))
	r.at(t, 59*time.Second)
	if r.watch(t, "check_ins_stopped") != 0 {
		t.Fatal("marked as stopped less than a minute after an alert")
	}
	r.at(t, time.Second)
	if r.watch(t, "check_ins_stopped") != 1 {
		t.Fatal("not marked as stopped a minute after the alert")
	}
	r.at(t, 59*time.Second)
	if titles := r.titles(t); len(titles) != 0 {
		t.Fatalf("warned before 2 minutes had passed since the alert: %v", titles)
	}
	r.at(t, time.Second)
	if titles := r.titles(t); len(titles) != 1 || !strings.HasPrefix(titles[0], "warn: ") {
		t.Fatalf("2 minutes after the alert: %v", titles)
	}
}

func TestCheckInsResumingEndTheWarningAtOnce(t *testing.T) {
	r := newCheckInRig(t)
	r.checkIn(t, "")
	r.at(t, 3*time.Minute)
	r.checkIn(t, "")
	titles := r.titles(t)
	if len(titles) != 2 || titles[1] != "resolved-warn: “Monitoring on util.example.com” checks in with MMHotline again" {
		t.Fatalf("titles: %v", titles)
	}
	if r.watch(t, "check_ins_stopped") != 0 || r.watch(t, "last_check_in_at") != r.clock.Unix() {
		t.Fatal("the resumed check-in did not remove the stopped mark")
	}
}

// A check-in arrives at the moment the watcher marks its alert source
// as stopped. Whichever comes first, memory and the team's database agree
// afterwards: the next check-in leaves the alert source not marked, and
// nobody is alerted.
func TestCheckInRacingTheStoppedMark(t *testing.T) {
	r := newCheckInRig(t)
	ctx := context.Background()
	r.checkIn(t, "")
	for round := range 20 {
		r.clock = r.clock.Add(61 * time.Second)
		body := `{"sent_at":"` + r.clock.Format(time.RFC3339) + `","sent_by_node_name":"util"}`
		var watchErr error
		var code int
		var both sync.WaitGroup
		both.Go(func() { watchErr = r.a.WatchAlertSourceCheckIns(ctx) })
		both.Go(func() { code = checkInHTTP(r.a, r.p, r.source, body, r.clock).Code })
		both.Wait()
		if watchErr != nil || code != 200 {
			t.Fatalf("round %d: watcher %v, check-in %d", round, watchErr, code)
		}
		r.checkIn(t, "")
		if r.watch(t, "check_ins_stopped") != 0 {
			t.Fatalf("round %d: still marked as stopped after a check-in", round)
		}
		if err := r.a.WatchAlertSourceCheckIns(ctx); err != nil {
			t.Fatal(err)
		}
		if titles := r.titles(t); len(titles) != 0 {
			t.Fatalf("round %d: alerted: %v", round, titles)
		}
	}
}
