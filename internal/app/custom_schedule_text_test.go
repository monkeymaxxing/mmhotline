package app

import (
	"strings"
	"testing"
	"time"
)

// The custom schedule's messages read as sentences: an unknown address ends
// with a period before "This happened for", and schedules show says a test
// case's backup is not checked, in place of an empty value.
func TestCustomScheduleMessagesReadAsSentences(t *testing.T) {
	t.Parallel()
	problem := CustomScheduleCodeProblem{Kind: customScheduleErrorUnknownSignInEmailAddress, Message: "dana@example.com", AskedAt: time.Date(2026, 11, 15, 6, 15, 0, 0, time.UTC)}
	if text := problem.Error(); !strings.Contains(text, "dana@example.com. This happened for") {
		t.Fatalf("unknown address: %q", text)
	}
	file := ScheduleFile{ScheduleType: "custom_schedule_code", TimeZone: "America/New_York", CustomScheduleCode: "def on_call(at):\n    return {}\n", CustomScheduleTestCases: []ScheduleFileTestCase{{LocalTime: "2026-11-21T10:00", ExpectedPrimarySignInEmailAddress: "carlos@example.com"}}}
	if text := file.Text(); !strings.Contains(text, "Test at local time 2026-11-21T10:00: expected primary carlos@example.com; backup not checked.") {
		t.Fatalf("schedules show test case: %s", text)
	}
}
