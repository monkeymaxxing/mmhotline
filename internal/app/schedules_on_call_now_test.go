package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// schedules show says who MMHotline wakes first for an SOS now, and who
// next, from the coverage the dashboard uses, so a person or a coding agent
// need not work out the rotation; schedules preview prints its times as
// the other commands do.
func TestSchedulesShowSaysWhoIsOnCallNow(t *testing.T) {
	r := newDownAlarmRig(t)
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "schedules", Action: "show"}, 200)
	text := result.Text()
	if !strings.Contains(text, "On call now: Responder (barbara@example.com). Backup now: Responder (bob@example.com).") {
		t.Fatalf("schedules show:\n%s", text)
	}
	assertCommandKeys(t, raw, "on_call_now", "on_call", "backup", "name", "sign_in_email_address", "on_call_is_everyone_available", "backup_is_everyone_available")
	if now := result.ScheduleState.OnCallNow; now == nil || len(now.OnCall) != 1 || now.OnCall[0].SignInEmailAddress != "barbara@example.com" || now.BackupIsEveryoneAvailable {
		t.Fatalf("on_call_now %+v", now)
	}
}

func TestSchedulesPreviewPrintsRFC3339Times(t *testing.T) {
	a, _, members := coverageFixture(t)
	file := ScheduleFile{ScheduleType: "weekly_rotation", TimeZone: "UTC", HandoffHour: 9, HandoffWeekday: 1, FirstStartDate: "2026-09-28", RotationSignInEmailAddresses: []string{members[0].SignInEmailAddress, members[1].SignInEmailAddress}, BackupOption: "next_in_rotation"}
	result, _ := commandRequest(t, a, CommandRequest{Group: "schedules", Action: "preview", Schedules: SchedulesCommandRequest{Schedule: settingsSchedule(file)}}, 200)
	if text := result.Text(); strings.Contains(text, "+0000 UTC") || !strings.Contains(text, "T09:00:00Z, primary:") {
		t.Fatalf("schedules preview:\n%s", text)
	}
}

// While the team is muted, schedules show says that an SOS wakes nobody
// until the mute ends, as the On call page does, so a person or a coding
// agent reading the command does not expect a phone to ring.
func TestSchedulesShowSaysAMutedTeamWakesNobody(t *testing.T) {
	r := newDownAlarmRig(t)
	until := r.a.Now().Add(15 * time.Minute)
	if _, err := r.a.SetTeamMute(context.Background(), r.a.settingsPrincipal(), until, 0); err != nil {
		t.Fatal(err)
	}
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "schedules", Action: "show"}, 200)
	want := "The team is muted until " + commandTime(until) + ": MMHotline sends no Pushover notifications or automated voice calls, so an SOS wakes nobody until then."
	if text := result.Text(); !strings.Contains(text, want) {
		t.Fatalf("schedules show:\n%s", text)
	}
	assertCommandKeys(t, raw, "team_muted_until")
}
