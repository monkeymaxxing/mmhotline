package app

import (
	"strings"
	"testing"
	"time"
)

func TestTriageAgentReportTitleIsPlainAndShort(t *testing.T) {
	t.Parallel()
	for _, item := range []struct{ report, want string }{
		{"## **What happened:** `mmx_run_output_page.service` (the read-only page) stopped\nmore", "The AI triage agent reports: What happened: mmx_run_output_page.service (the read-only page) stopped"},
		{"- See [the run](https://example.com/run/1) for *details*", "The AI triage agent reports: See the run for details"},
		{"\n\n> ~~old~~ new", "The AI triage agent reports: old new"},
		{"   \n", "The AI triage agent sent a report"},
	} {
		if got := triageAgentReportTitle(item.report); got != item.want {
			t.Errorf("title of %q:\n got %q\nwant %q", item.report, got, item.want)
		}
	}
	long := triageAgentReportTitle(strings.Repeat("disk full on app002 ", 20))
	line := strings.TrimPrefix(long, "The AI triage agent reports: ")
	if !strings.HasSuffix(line, "…") || len([]rune(line)) > maximumReportTitleRunes || strings.HasSuffix(strings.TrimSuffix(line, "…"), " ") || !strings.HasPrefix(strings.Repeat("disk full on app002 ", 20), strings.TrimSuffix(line, "…")) {
		t.Fatalf("long title %q is not cut at a word within %d characters", line, maximumReportTitleRunes)
	}
}

func TestTriageSessionTimesTextGivesNowAndEarlierRuns(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 1, 34, 59, 0, time.FixedZone("EET", 2*3600))
	text := triageSessionTimesText(now, []string{"run 1: started 2026-10-28 01:04:06 UTC, ended 2026-10-28 01:09:40 UTC, finished"})
	for _, want := range []string{"it is now 2026-10-27 23:34:59 UTC", "- run 1: started 2026-10-28 01:04:06 UTC, ended 2026-10-28 01:09:40 UTC, finished"} {
		if !strings.Contains(text, want) {
			t.Fatalf("times text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(triageSessionTimesText(now, nil), "earlier runs") {
		t.Fatal("a first run lists earlier runs")
	}
}
