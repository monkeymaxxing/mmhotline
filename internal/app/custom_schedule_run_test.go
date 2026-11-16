package app

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests start the sandbox child: the test binary run as
// `custom-schedule-sandbox` (see TestMain).

func runProblem(t *testing.T, h *customScheduleTeam, code string) *CustomScheduleCodeProblem {
	t.Helper()
	preview, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, customConfig(code))
	if err != nil {
		t.Fatal(err)
	}
	return preview.Problem
}

func TestCustomScheduleSandboxAnswersAndLimits(t *testing.T) {
	h := newCustomScheduleTeam(t)
	preview, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, customConfig(weekdayWeekendCode))
	if err != nil || preview.Problem != nil {
		t.Fatal(err, preview.Problem)
	}
	// 2026-10-05 07:00 UTC is Monday 03:00 in New York. Weekdays until
	// Saturday 00:00 local (Saturday 04:00 UTC), then the weekend.
	first := preview.Shifts[0]
	if first.PrimaryMemberID != h.carlos || first.BackupMemberID != h.barbara || !first.StartsAt.Equal(h.clock.UTC()) || !first.EndsAt.Equal(time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("first shift: %+v", first)
	}
	if preview.Shifts[1].PrimaryMemberID != h.dana {
		t.Fatalf("weekend: %+v", preview.Shifts[1])
	}
	last := preview.Shifts[len(preview.Shifts)-1]
	if !last.EndsAt.Equal(h.clock.Add(14 * 24 * time.Hour)) {
		t.Fatalf("14 days: %v", last.EndsAt)
	}
	for _, tc := range []struct {
		name, code, want string
	}{
		{"syntax", "def on_call(at):\n    return {\"primary_sign_in_email_address\": }\n", "Your code has a mistake on line 2: got '}', want primary expression"},
		{"runtime", "def on_call(at):\n    return {\"primary_sign_in_email_address\": [][1]}\n", "Your code has a mistake on line 2: index 1 out of range: empty list. This happened for Mon 2026-10-05 03:00 EDT."},
		{"unknown sign-in email address", "def on_call(at):\n    return {\"primary_sign_in_email_address\": \"mallory@example.com\"}\n", "on_call returned a sign-in email address that is not a team member's sign-in email address: mallory@example.com. This happened for Mon 2026-10-05 03:00 EDT."},
		{"too much work", "def on_call(at):\n    x = 0\n    for i in range(200000):\n        x += i\n    return {\"primary_sign_in_email_address\": None}\n", "Your code did too much work for Mon 2026-10-05 03:00 EDT. Make it simpler."},
		{"too long", "def on_call(at):\n    x = sorted(list(range(200000)), reverse=True)\n    return {\"primary_sign_in_email_address\": None}\n", "Your code took too long to run."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problem := runProblem(t, h, tc.code)
			if problem == nil || problem.Error() != tc.want {
				t.Fatalf("got %v", problem)
			}
		})
	}
	if runtime.GOOS == "linux" && !raceDetectorEnabled {
		// RLIMIT_DATA is enforced only on Linux, and race builds raise it.
		problem := runProblem(t, h, "def on_call(at):\n    x = \"x\" * 400000000\n    return {\"primary_sign_in_email_address\": None}\n")
		if problem == nil || problem.Error() != "Your code used too much memory." {
			t.Fatalf("memory: %v", problem)
		}
	}
	member := h.member
	if _, err := h.app.PreviewCustomScheduleCode(context.Background(), member, customConfig(weekdayWeekendCode)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member ran code: %v", err)
	}
	big := customConfig(strings.Repeat("#", customScheduleCodeMaxBytes+1))
	if _, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, big); !errors.Is(err, ErrInvalid) {
		t.Fatalf("code over 64 KiB: %v", err)
	}
	rotationBackup := customConfig(weekdayWeekendCode)
	rotationBackup.BackupKind = "next_in_rotation"
	if _, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, rotationBackup); !errors.Is(err, ErrInvalid) {
		t.Fatalf("next person in the rotation offered for custom: %v", err)
	}
	tooManyTests := customConfig(weekdayWeekendCode)
	tooManyTests.CustomScheduleTestCases = make([]CustomScheduleTestCase, customScheduleMaxTestCases+1)
	for i := range tooManyTests.CustomScheduleTestCases {
		tooManyTests.CustomScheduleTestCases[i].When = "2026-10-10T12:00"
	}
	if _, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, tooManyTests); !errors.Is(err, ErrInvalid) {
		t.Fatalf("51 test cases: %v", err)
	}
}

// Previews, saves, and activations share one limit, so saving code that
// fails its test cases cannot run the sandbox without end.
func TestCustomScheduleRunLimitPerTeam(t *testing.T) {
	h := newCustomScheduleTeam(t)
	h.save(t, customConfig(weekdayWeekendCode))
	for range customScheduleRunsPerHour - 1 {
		if !h.app.customScheduleRuns.allow(h.app.Now()) {
			t.Fatal("refused too early")
		}
	}
	const refused = "You have run your code a lot in the last hour. Try again later."
	_, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, customConfig(weekdayWeekendCode))
	if !errors.Is(err, ErrRateLimited) || publicError(err) != refused {
		t.Fatalf("61st preview: %v", err)
	}
	if _, err = h.app.SaveCustomScheduleCode(context.Background(), h.admin, customConfig(weekdayWeekendCode), h.revision(t)); !errors.Is(err, ErrRateLimited) || publicError(err) != refused {
		t.Fatalf("save after 60 runs: %v", err)
	}
	h.advance(time.Hour)
	if _, err = h.app.PreviewCustomScheduleCode(context.Background(), h.admin, customConfig(weekdayWeekendCode)); err != nil {
		t.Fatal(err)
	}
}

// The code records every asked moment by failing on the ones that must not
// be asked. Around both daylight saving changes in New York, a repeated local
// time is asked once and never switches back; a skipped one is never asked.
func TestCustomScheduleWorkedOutAheadAcrossDaylightSaving(t *testing.T) {
	h := newCustomScheduleTeam(t)
	ctx := context.Background()
	signInEmailAddresses, err := h.app.customScheduleMemberSignInEmailAddresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config := customConfig(`
def on_call(at):
    if at.date == "2027-03-14" and at.hour == 2:
        fail("a skipped local time was asked")
    if at.date == "2026-11-01" and at.time_of_day >= "01:30" and at.time_of_day < "02:00":
        return {"primary_sign_in_email_address": "dana@example.com"}
    if at.minute == 15:
        return {"primary_sign_in_email_address": "barbara@example.com"}
    return {"primary_sign_in_email_address": "carlos@example.com"}
`)
	config.TimeZone = "America/New_York"
	config.CustomScheduleTestCases = []CustomScheduleTestCase{
		{When: "2026-11-01T01:40", ExpectedPrimaryMemberID: h.dana},
		{When: "2026-11-01T09:15", ExpectedPrimaryMemberID: h.barbara},
		{When: "2026-11-01T09:20", ExpectedPrimaryMemberID: h.barbara},
		{When: "2026-11-01T09:30", ExpectedPrimaryMemberID: h.barbara},
	}
	fall := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	preview, err := h.app.workOutCustomSchedule(ctx, config, signInEmailAddresses, fall)
	if err != nil || preview.Problem == nil || preview.Problem.Kind != customScheduleErrorTestCaseFailed {
		t.Fatalf("%v %+v", err, preview.Problem)
	}
	passed := []bool{true, true, true, false}
	for i, result := range preview.TestResults {
		if result.Passed != passed[i] {
			t.Fatalf("test case %d: %+v", i, result)
		}
	}
	if preview.TestResults[3].ReturnedPrimaryMemberID != h.carlos {
		t.Fatalf("failed test case answer: %+v", preview.TestResults[3])
	}
	at := func(utc string) string {
		moment, _ := time.Parse(time.RFC3339, utc)
		for _, shift := range preview.Shifts {
			if !moment.Before(shift.StartsAt) && moment.Before(shift.EndsAt) {
				return shift.PrimaryMemberID
			}
		}
		return "no shift"
	}
	// 01:30 EDT is 05:30 UTC: Dana. The clock repeats 01:00-01:59 as EST
	// (06:00-06:59 UTC): Dana, on call at the end of the first pass, stays on
	// call; nobody before her is on call again. 02:00 EST (07:00 UTC) is asked.
	for utc, want := range map[string]string{"2026-11-01T05:00:00Z": h.carlos, "2026-11-01T05:15:00Z": h.barbara, "2026-11-01T05:30:00Z": h.dana, "2026-11-01T05:45:00Z": h.dana, "2026-11-01T06:00:00Z": h.dana, "2026-11-01T06:15:00Z": h.dana, "2026-11-01T06:45:00Z": h.dana, "2026-11-01T07:00:00Z": h.carlos} {
		if got := at(utc); got != want {
			t.Errorf("%s: got %s want %s", utc, got, want)
		}
	}
	// Adjacent equal answers merge: every hour has a Barbara quarter, so a
	// normal hour is three shifts (Carlos, Barbara, Carlos).
	if total := preview.Shifts[len(preview.Shifts)-1].EndsAt.Sub(preview.Shifts[0].StartsAt); total != 14*24*time.Hour {
		t.Fatalf("worked out %v", total)
	}
	// An hourly run that starts in the second pass (01:10 EST, 06:10 UTC)
	// carries Dana from the end of the first pass; nobody is on call again.
	secondPass := time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC)
	config.CustomScheduleTestCases = nil
	preview, err = h.app.workOutCustomSchedule(ctx, config, signInEmailAddresses, secondPass)
	if err != nil || preview.Problem != nil {
		t.Fatalf("second pass: %v %v", err, preview.Problem)
	}
	if preview.Shifts[0].StartsAt != secondPass.Truncate(15*time.Minute) {
		t.Fatalf("shifts start %v", preview.Shifts[0].StartsAt)
	}
	for utc, want := range map[string]string{"2026-11-01T06:00:00Z": h.dana, "2026-11-01T06:15:00Z": h.dana, "2026-11-01T06:45:00Z": h.dana, "2026-11-01T07:00:00Z": h.carlos, "2026-11-01T07:15:00Z": h.barbara} {
		if got := at(utc); got != want {
			t.Errorf("run in the second pass, %s: got %s want %s", utc, got, want)
		}
	}
	spring := time.Date(2027, 3, 13, 12, 0, 0, 0, time.UTC)
	preview, err = h.app.workOutCustomSchedule(ctx, config, signInEmailAddresses, spring)
	if err != nil || preview.Problem != nil {
		t.Fatalf("spring: %v %v", err, preview.Problem)
	}
	// 01:45 EST (06:45 UTC) is followed directly by 03:00 EDT (07:00 UTC).
	if at("2027-03-14T06:45:00Z") != h.carlos || at("2027-03-14T07:00:00Z") != h.carlos || at("2027-03-14T07:15:00Z") != h.barbara {
		t.Fatal("spring forward shifts")
	}
}

func TestCustomScheduleRunLimiterCountsInstallationRunsAndExpiresThem(t *testing.T) {
	limiter := &customScheduleRunLimiter{}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for range 60 {
		if !limiter.allow(now) {
			t.Fatal("refused before 60 runs")
		}
	}
	if limiter.allow(now.Add(time.Hour - time.Nanosecond)) {
		t.Fatal("allowed more than 60 runs in an hour")
	}
	if !limiter.allow(now.Add(time.Hour)) {
		t.Fatal("retained expired runs")
	}
}
