package app

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func coverageFixture(t *testing.T) (*App, Principal, []Member) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := Setup(ctx, dir, "Fixture team"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a := New(store)
	configureAccessSignIn(t, a)
	recordEmailSetUpForTest(t, store)
	var people []Member
	for _, input := range []struct{ signInEmailAddress, name, role string }{{"admin@example.com", "Admin", RoleAdministrator}, {"backup@example.com", "Backup", RoleMember}, {"cover@example.com", "Cover", RoleMember}} {
		people = append(people, addPerson(t, a, input.signInEmailAddress, input.name, input.role))
	}
	a.Now = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	return a, Principal{SignInEmailAddress: people[0].SignInEmailAddress}, people
}

func dayOfWeekFor(memberID string) ScheduleConfig {
	return ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", HandoffHour: 9, Assignments: []string{memberID, memberID, memberID, memberID, memberID, memberID, memberID}, BackupKind: "everyone_available"}
}

func TestScheduleDayOfWeekDSTAndHourOfDayRepeatedHours(t *testing.T) {
	c := ScheduleConfig{Kind: "day_of_week", TimeZone: "America/New_York", HandoffHour: 2, HandoffMinute: 30, Assignments: []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}, BackupKind: "everyone_available"}
	cases := []struct{ at, want string }{{"2026-03-08T06:59:59Z", "Saturday"}, {"2026-03-08T07:00:00Z", "Sunday"}, {"2026-03-09T06:29:59Z", "Sunday"}, {"2026-03-09T06:30:00Z", "Monday"}}
	for _, tc := range cases {
		at, _ := time.Parse(time.RFC3339, tc.at)
		got, err := EvaluateSchedule(c, at)
		if err != nil || got.PrimaryID != tc.want {
			t.Errorf("spring %s: %+v %v", tc.at, got, err)
		}
	}
	c.HandoffHour = 1
	for _, tc := range []struct{ at, want string }{{"2026-11-01T05:29:59Z", "Saturday"}, {"2026-11-01T05:30:00Z", "Sunday"}, {"2026-11-01T06:15:00Z", "Sunday"}, {"2026-11-01T06:30:00Z", "Sunday"}} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		got, err := EvaluateSchedule(c, at)
		if err != nil || got.PrimaryID != tc.want {
			t.Errorf("fall %s: %+v %v", tc.at, got, err)
		}
	}
	c.Kind = "hour_of_day"
	c.Assignments = make([]string, 24)
	c.Assignments[1] = "both occurrences"
	c.Assignments[2] = "next hour"
	for _, ts := range []string{"2026-11-01T05:05:00Z", "2026-11-01T06:05:00Z"} {
		at, _ := time.Parse(time.RFC3339, ts)
		got, err := EvaluateSchedule(c, at)
		if err != nil || got.PrimaryID != "both occurrences" {
			t.Fatalf("hour_of_day repeated slot: %+v %v", got, err)
		}
	}
	at, _ := time.Parse(time.RFC3339, "2026-11-01T07:00:00Z")
	got, err := EvaluateSchedule(c, at)
	if err != nil || got.PrimaryID != "next hour" {
		t.Fatalf("hour boundary: %+v %v", got, err)
	}
}

func TestSchedulePreviewActivationPreservationAndAuthorization(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	c := dayOfWeekFor(people[0].ID)
	c.BackupKind = "fixed_person"
	c.BackupMemberID = people[1].ID
	preview, err := a.PreviewSchedule(ctx, p, c, []time.Time{a.Now()})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := a.SaveSchedule(ctx, p, c, 0)
	if err != nil {
		t.Fatal(err)
	}
	live, err := a.CurrentCoverage(ctx, p)
	if err != nil || live.AssignedPrimaryID != preview[0].PrimaryID || !reflect.DeepEqual(live.PrimaryIDs, []string{people[0].ID}) || !reflect.DeepEqual(live.BackupIDs, []string{people[1].ID}) {
		t.Fatalf("preview/current disagree: %+v %+v %v", preview, live, err)
	}
	invalid := c
	invalid.Assignments = []string{"bad"}
	if _, err = a.SaveSchedule(ctx, p, invalid, saved.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid candidate: %v", err)
	}
	state, _ := a.Schedules(ctx, p)
	if state.Revision != saved.Revision || state.ActiveKind != "day_of_week" {
		t.Fatalf("invalid save changed active: %+v", state)
	}
	hour_of_day := ScheduleConfig{Kind: "hour_of_day", TimeZone: "UTC", Assignments: make([]string, 24), BackupKind: "everyone_available"}
	hour_of_day.Assignments[12] = people[2].ID
	state, err = a.SaveSchedule(ctx, p, hour_of_day, state.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Configs) != 2 {
		t.Fatalf("lost config: %+v", state)
	}
	live, _ = a.CurrentCoverage(ctx, p)
	if live.AssignedPrimaryID == people[0].ID || live.ScheduleKind != "hour_of_day" {
		t.Fatalf("the saved hour_of_day schedule is not the active one: %+v", live)
	}
	member := Principal{SignInEmailAddress: people[1].SignInEmailAddress}
	if _, err = a.SaveSchedule(ctx, member, c, 3); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member altered schedule: %v", err)
	}
	if _, err = a.CurrentCoverage(ctx, Principal{SignInEmailAddress: NewID()}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonmember read: %v", err)
	}
}

func TestOnCallClaimExplicitReplacementConcurrentConflictAndBypass(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	now := a.Now()
	if _, err := a.SaveSchedule(ctx, p, dayOfWeekFor(people[0].ID), 0); err != nil {
		t.Fatal(err)
	}
	first, err := a.SetOnCallClaim(ctx, p, now.Add(4*time.Hour), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	other := Principal{SignInEmailAddress: people[1].SignInEmailAddress}
	if _, err = a.SetOnCallClaim(ctx, other, now.Add(time.Hour), first.Revision, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("implicit replace: %v", err)
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for _, m := range people[1:] {
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			_, e := a.SetOnCallClaim(ctx, Principal{SignInEmailAddress: m.SignInEmailAddress}, now.Add(2*time.Hour), first.Revision, people[0].ID)
			out <- e
		}(m)
	}
	wg.Wait()
	close(out)
	success, conflict := 0, 0
	for e := range out {
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent outcomes: %d success %d conflicts", success, conflict)
	}
	c, err := a.CurrentCoverage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	holder := c.OnCallClaim.MemberID
	var hp Principal
	var nonholder Principal
	for _, m := range people[1:] {
		if m.ID == holder {
			hp = Principal{SignInEmailAddress: m.SignInEmailAddress}
		} else {
			nonholder = Principal{SignInEmailAddress: m.SignInEmailAddress}
		}
	}
	if _, err = a.EndOnCallClaim(ctx, nonholder, c.OnCallClaim.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonholder cancelled: %v", err)
	}
	if _, err = a.SetPersonalAvailability(ctx, hp, "unavailable", now.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	c, _ = a.CurrentCoverage(ctx, p)
	if !c.OnCallClaim.Active || len(c.PrimaryIDs) != 1 || c.PrimaryIDs[0] != people[0].ID {
		t.Fatalf("unavailable on-call claim did not bypass: %+v", c)
	}
	a.Now = func() time.Time { return now.Add(61 * time.Minute) }
	c, _ = a.CurrentCoverage(ctx, p)
	if len(c.PrimaryIDs) != 1 || c.PrimaryIDs[0] != holder {
		t.Fatalf("on-call claim did not return: %+v", c)
	}
	if _, err = a.EndOnCallClaim(ctx, p, c.OnCallClaim.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestTeamMuteIsSeparateFromEligibilityAndCannotBeBypassed(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	now := a.Now()
	if _, err := a.SetPersonalAvailability(ctx, p, "unavailable", now.Add(3*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	member := Principal{SignInEmailAddress: people[1].SignInEmailAddress}
	if _, err := a.SetTeamMute(ctx, member, now.Add(time.Hour), 0); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	mute, err := a.SetTeamMute(ctx, p, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		ok, _, e := MemberEligible(ctx, tx, people[1].ID, now)
		if e != nil || !ok {
			t.Fatalf("team mute changed eligibility: %v %v", ok, e)
		}
		for _, tc := range []struct {
			member, contactMethodType                 string
			sendWhileUnavailableOrMuted, ignore, want bool
		}{{people[0].ID, "test_waking", true, true, false}, {people[0].ID, "email", true, false, true}, {people[0].ID, "email", false, true, false}, {"", "email", false, false, false}, {"", "email", true, false, true}} {
			ok, _, e = DeliveryAllowed(ctx, tx, tc.member, tc.contactMethodType, tc.sendWhileUnavailableOrMuted, tc.ignore, now)
			if e != nil || ok != tc.want {
				t.Errorf("delivery %+v: %v %v", tc, ok, e)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetTeamMute(ctx, p, time.Time{}, mute.Revision); err != nil {
		t.Fatal(err)
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		state, e := ReadPersonalAvailability(ctx, tx, people[0].ID)
		if e != nil {
			return e
		}
		if !state.UnavailableUntil.Equal(now.Add(3 * time.Hour)) {
			t.Fatalf("team unmute destroyed personal settings: %+v", state)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCoverageExpiryPersistsAndAuditsOnce(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	now := a.Now()
	if _, err := a.SetOnCallClaim(ctx, p, now.Add(time.Hour), 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPersonalAvailability(ctx, p, "unavailable", now.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetTeamMute(ctx, p, now.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	dir := a.Store.Dir
	if err := a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a = New(store)
	a.Now = func() time.Time { return now.Add(2 * time.Hour) }
	c, err := a.CurrentCoverage(ctx, p)
	if err != nil || c.TeamMute.Active || c.OnCallClaim.Active || len(c.EligibleMemberIDs) != len(people) {
		t.Fatalf("restart deadline state: %+v %v", c, err)
	}
	err = store.WithTransaction(ctx, func(tx *sql.Tx) error {
		changes, e := ExpireCoverage(ctx, tx, a.Now())
		if e != nil {
			return e
		}
		if len(changes) != 3 {
			t.Fatalf("first expiry changes: %+v", changes)
		}
		changes, e = ExpireCoverage(ctx, tx, a.Now())
		if e != nil {
			return e
		}
		if len(changes) != 0 {
			t.Fatalf("duplicate expiry: %+v", changes)
		}
		mute, e := ReadTeamMute(ctx, tx, a.Now())
		if e != nil {
			return e
		}
		if !mute.Expired || mute.Revision != 2 || !mute.Until.Equal(now.Add(time.Hour)) {
			t.Fatalf("lost mute deadline or revision: %+v", mute)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAvailabilityDurationAndRejectedState(t *testing.T) {
	for _, s := range []string{"0m", "-1h", "31d", "9223372036854775807d", "1.5d", "soon", "30s"} {
		if _, err := ParseAvailabilityDuration(s); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
	// The message says which rule the value breaks: its length, or how to
	// write it.
	for value, want := range map[string]string{
		"45d":     "duration must be between one minute and 30 days",
		"31d":     "duration must be between one minute and 30 days",
		"30s":     "duration must be between one minute and 30 days",
		"2 hours": "write the duration as a number and a unit, such as 30m, 2h, or 3d",
		"1.5d":    "write the duration as a number and a unit, such as 30m, 2h, or 3d",
	} {
		if _, err := ParseAvailabilityDuration(value); err == nil || publicError(err) != want {
			t.Errorf("%q: got %v, want %q", value, err, want)
		}
	}
	for _, s := range []string{"15m", "2h", "3d", "1h30m"} {
		if _, err := ParseAvailabilityDuration(s); err != nil {
			t.Errorf("rejected %s: %v", s, err)
		}
	}
	a, p, _ := coverageFixture(t)
	if _, err := a.SetPersonalAvailability(context.Background(), p, "unavailable", a.Now().Add(31*24*time.Hour), 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestScheduleHandoffCurrentOnlyAndNoOnCallClaimDuplicate(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	now := a.Now()
	c := ScheduleConfig{Kind: "hour_of_day", TimeZone: "UTC", Assignments: make([]string, 24), BackupKind: "everyone_available"}
	c.Assignments[12] = people[0].ID
	c.Assignments[13] = people[1].ID
	c.Assignments[14] = people[2].ID
	c.Assignments[15] = people[1].ID
	if _, err := a.SaveSchedule(ctx, p, c, 0); err != nil {
		t.Fatal(err)
	}
	observe := func(at time.Time) {
		t.Helper()
		if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { return ObserveCoverageHandoff(ctx, tx, at) }); err != nil {
			t.Fatal(err)
		}
	}
	observe(now)
	if _, err := a.SetOnCallClaim(ctx, p, now.Add(4*time.Hour), 0, ""); err != nil {
		t.Fatal(err)
	}
	observe(now)
	// Three elapsed hours yield one current handoff, not all missed slots.
	observe(now.Add(3 * time.Hour))
	observe(now.Add(3 * time.Hour))
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM coverage_changes WHERE kind='schedule_handoff'`).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("handoff notices: %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnavailabilityIsTheOnlyPersonalControl(t *testing.T) {
	a, p, members := coverageFixture(t)
	ctx := context.Background()
	now := a.Now()
	state, err := a.SetPersonalAvailability(ctx, p, "unavailable", now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !state.UnavailableUntil.Equal(now.Add(time.Hour)) {
		t.Fatal(state)
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var legacyColumns int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('member_availability') WHERE name='mute_until'`).Scan(&legacyColumns); err != nil {
			return err
		}
		if legacyColumns != 0 {
			t.Fatal("retired personal control still stored")
		}
		eligible, reason, err := MemberEligible(ctx, tx, members[0].ID, now)
		if err != nil || eligible || reason != "unavailable" {
			t.Fatalf("eligibility: %v %s %v", eligible, reason, err)
		}
		for _, sendWhileUnavailable := range []bool{false, true} {
			allowed, reason, err := DeliveryAllowed(ctx, tx, members[0].ID, "email", sendWhileUnavailable, false, now)
			if err != nil || allowed != sendWhileUnavailable {
				t.Fatalf("routing: %v %s %v", allowed, reason, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPersonalAvailability(ctx, p, "mute", now.Add(time.Hour), state.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retired control accepted: %v", err)
	}
	state, err = a.SetPersonalAvailability(ctx, p, "unavailable", time.Time{}, state.Revision)
	if err != nil || !state.UnavailableUntil.IsZero() {
		t.Fatalf("available again: %+v %v", state, err)
	}
}

func weeklyRotationFor(memberIDs ...string) ScheduleConfig {
	return ScheduleConfig{Kind: "weekly_rotation", TimeZone: "UTC", HandoffWeekday: int(time.Monday), HandoffHour: 9, FirstStartDate: "2026-09-28", RotationMemberIDs: memberIDs, BackupKind: "next_in_rotation"}
}

func TestWeeklyRotationCalendarOrderAndBoundaries(t *testing.T) {
	c := weeklyRotationFor("First", "Second", "Third")
	for _, tc := range []struct{ at, primary, backup string }{
		{"2026-09-21T09:00:00Z", "", ""},
		{"2026-09-28T08:59:59Z", "", ""},
		{"2026-09-28T09:00:00Z", "First", "Second"},
		{"2026-10-05T08:59:59Z", "First", "Second"},
		{"2026-10-05T09:00:00Z", "Second", "Third"},
		{"2026-10-12T09:00:00Z", "Third", "First"},
		{"2026-10-19T09:00:00Z", "First", "Second"},
		{"2026-10-26T09:00:00Z", "Second", "Third"},
	} {
		t.Run(tc.at, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			got, err := EvaluateSchedule(c, at)
			if err != nil || got.PrimaryID != tc.primary || got.BackupID != tc.backup || !got.At.Equal(at) {
				t.Fatalf("assignment: %+v, %v; want %s/%s", got, err, tc.primary, tc.backup)
			}
		})
	}
}

func TestWeeklyRotationDST(t *testing.T) {
	for _, transition := range []struct {
		name, start string
		hour        int
		cases       []struct{ at, primary string }
	}{
		{"spring", "2026-03-01", 2, []struct{ at, primary string }{
			{"2026-03-08T06:59:59Z", "First"}, {"2026-03-08T07:00:00Z", "Second"},
			{"2026-03-15T06:29:59Z", "Second"}, {"2026-03-15T06:30:00Z", "First"},
		}},
		{"fall", "2026-10-25", 1, []struct{ at, primary string }{
			{"2026-11-01T05:29:59Z", "First"}, {"2026-11-01T05:30:00Z", "Second"},
			{"2026-11-01T06:15:00Z", "Second"}, {"2026-11-01T06:30:00Z", "Second"},
			{"2026-11-08T06:29:59Z", "Second"}, {"2026-11-08T06:30:00Z", "First"},
		}},
	} {
		t.Run(transition.name, func(t *testing.T) {
			c := weeklyRotationFor("First", "Second")
			c.TimeZone, c.FirstStartDate = "America/New_York", transition.start
			c.HandoffWeekday, c.HandoffHour, c.HandoffMinute = int(time.Sunday), transition.hour, 30
			for _, tc := range transition.cases {
				at, _ := time.Parse(time.RFC3339, tc.at)
				got, err := EvaluateSchedule(c, at)
				if err != nil || got.PrimaryID != tc.primary {
					t.Errorf("%s: %+v %v", tc.at, got, err)
				}
			}
		})
	}
}

func TestWeeklyRotationValidationAndPreservation(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	c := weeklyRotationFor(people[2].ID, people[0].ID, people[1].ID)
	preview, err := a.PreviewSchedule(ctx, p, c, []time.Time{a.Now()})
	if err != nil {
		t.Fatal(err)
	}
	state, err := a.SaveSchedule(ctx, p, c, 0)
	if err != nil {
		t.Fatal(err)
	}
	live, err := a.CurrentCoverage(ctx, p)
	if err != nil || live.AssignedPrimaryID != preview[0].PrimaryID || live.AssignedBackupID != preview[0].BackupID {
		t.Fatalf("preview/live: %+v %+v %v", preview, live, err)
	}
	for _, tc := range []struct {
		name   string
		change func(*ScheduleConfig)
	}{
		{"empty list", func(c *ScheduleConfig) { c.RotationMemberIDs = nil }},
		{"duplicate", func(c *ScheduleConfig) { c.RotationMemberIDs = []string{people[0].ID, people[0].ID} }},
		{"empty member", func(c *ScheduleConfig) { c.RotationMemberIDs = []string{""} }},
		{"wrong weekday", func(c *ScheduleConfig) { c.FirstStartDate = "2026-09-29" }},
		{"invalid date", func(c *ScheduleConfig) { c.FirstStartDate = "2026-02-30" }},
		{"invalid weekday", func(c *ScheduleConfig) { c.HandoffWeekday = 7 }},
		{"invalid time", func(c *ScheduleConfig) { c.HandoffHour = 24 }},
		{"unknown member", func(c *ScheduleConfig) { c.RotationMemberIDs = []string{NewID()} }},
		{"unknown timezone", func(c *ScheduleConfig) { c.TimeZone = "Unknown/Mars" }},
		{"next backup on day of week", func(c *ScheduleConfig) { *c = dayOfWeekFor(people[0].ID); c.BackupKind = "next_in_rotation" }},
		{"next backup on hour of day", func(c *ScheduleConfig) { c.Kind = "hour_of_day"; c.Assignments = make([]string, 24) }},
		{"next backup with fixed member", func(c *ScheduleConfig) { c.BackupMemberID = people[0].ID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := c
			tc.change(&invalid)
			if _, err := a.PreviewSchedule(ctx, p, invalid, []time.Time{a.Now()}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("preview: %v", err)
			}
			if _, err := a.SaveSchedule(ctx, p, invalid, state.Revision); !errors.Is(err, ErrInvalid) {
				t.Fatalf("save: %v", err)
			}
		})
	}
	if _, err = a.SaveSchedule(ctx, Principal{SignInEmailAddress: people[1].SignInEmailAddress}, c, state.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member save: %v", err)
	}
}

func TestWeeklyRotationBackupFallbackAndHandoffs(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	c := weeklyRotationFor(people[0].ID, people[1].ID, people[2].ID)
	if _, err := a.SaveSchedule(ctx, p, c, 0); err != nil {
		t.Fatal(err)
	}
	live, err := a.CurrentCoverage(ctx, p)
	if err != nil || !reflect.DeepEqual(live.BackupIDs, []string{people[1].ID}) {
		t.Fatalf("next backup: %+v %v", live, err)
	}
	if _, err := a.SetPersonalAvailability(ctx, p, "unavailable", a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	live, err = a.CurrentCoverage(ctx, p)
	if err != nil || !reflect.DeepEqual(live.PrimaryIDs, []string{people[1].ID}) || !live.BackupIsEveryoneAvailable || !reflect.DeepEqual(live.BackupIDs, []string{people[2].ID}) {
		t.Fatalf("promoted next backup: %+v %v", live, err)
	}
	backup := Principal{SignInEmailAddress: people[1].SignInEmailAddress}
	if _, err := a.SetPersonalAvailability(ctx, backup, "unavailable", a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	live, err = a.CurrentCoverage(ctx, p)
	if err != nil || !live.PrimaryIsEveryoneAvailable || !reflect.DeepEqual(live.PrimaryIDs, []string{people[2].ID}) {
		t.Fatalf("ineligible next backup: %+v %v", live, err)
	}
	a.Now = func() time.Time { return time.Date(2026, 9, 28, 8, 59, 59, 0, time.UTC) }
	live, err = a.CurrentCoverage(ctx, p)
	if err != nil || live.AssignedPrimaryID != "" || live.AssignedBackupID != "" || !live.PrimaryIsEveryoneAvailable {
		t.Fatalf("before start: %+v %v", live, err)
	}
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for _, at := range []time.Time{a.Now(), a.Now().Add(time.Second), a.Now().Add(time.Second), a.Now().AddDate(0, 0, 15)} {
			if err := ObserveCoverageHandoff(ctx, tx, at); err != nil {
				return err
			}
		}
		var notices int
		if err := tx.QueryRow(`SELECT count(*) FROM coverage_changes WHERE kind='schedule_handoff'`).Scan(&notices); err != nil {
			return err
		}
		if notices != 2 {
			t.Fatalf("handoff notices: %d", notices)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c = weeklyRotationFor(people[2].ID)
	if _, err := a.SaveSchedule(ctx, p, c, 1); err != nil {
		t.Fatal(err)
	}
	a.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	live, err = a.CurrentCoverage(ctx, p)
	if err != nil || live.AssignedBackupID != "" || !live.BackupIsEveryoneAvailable || len(live.BackupIDs) != 2 {
		t.Fatalf("one-person rotation: %+v %v", live, err)
	}
	c.BackupKind, c.BackupMemberID = "fixed_person", people[2].ID
	c.FirstStartDate = "2026-10-05"
	if _, err := a.SaveSchedule(ctx, p, c, 2); err != nil {
		t.Fatal(err)
	}
	live, err = a.CurrentCoverage(ctx, p)
	if err != nil || live.AssignedPrimaryID != "" || !reflect.DeepEqual(live.PrimaryIDs, []string{people[2].ID}) {
		t.Fatalf("fixed backup before start: %+v %v", live, err)
	}
}

func TestOnCallReasonsUsePlainSentences(t *testing.T) {
	a, p, people := coverageFixture(t)
	ctx := context.Background()
	check := func(want string) {
		t.Helper()
		coverage, err := a.CurrentCoverage(ctx, p)
		if err != nil || coverage.Reason != want {
			t.Fatalf("reason: %q, want %q: %v", coverage.Reason, want, err)
		}
	}
	check("No available person is scheduled right now, so everyone available is contacted.")
	config := dayOfWeekFor("")
	config.BackupKind, config.BackupMemberID = "fixed_person", people[1].ID
	if _, err := a.SaveSchedule(ctx, p, config, 0); err != nil {
		t.Fatal(err)
	}
	check("No available person is scheduled right now, so the fixed backup is contacted first.")
	until := a.Now().Add(time.Hour)
	if _, err := a.SetOnCallClaim(ctx, p, until, 0, ""); err != nil {
		t.Fatal(err)
	}
	check("The person who took on-call is contacted first.")
	if _, err := a.SaveSchedule(ctx, p, dayOfWeekFor(people[1].ID), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPersonalAvailability(ctx, p, "unavailable", until, 0); err != nil {
		t.Fatal(err)
	}
	check("The person who took on-call is unavailable or no longer on the team, so the schedule is used.")
	for _, person := range people[1:] {
		if _, err := a.SetPersonalAvailability(ctx, Principal{SignInEmailAddress: person.SignInEmailAddress}, "unavailable", until, 0); err != nil {
			t.Fatal(err)
		}
	}
	check("No one is available. For an SOS, every team member is contacted.")
}
