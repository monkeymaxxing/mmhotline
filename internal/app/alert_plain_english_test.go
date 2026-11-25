package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
	"mmhotline/internal/triageagent"
)

func TestHumanizeAlertTitle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "systemd failed unit",
			in:   "util01: systemd_units/no_systemd_unit_has_failed: release_the_newest_website_build.service failed",
			want: "util01: No systemd unit has failed: release_the_newest_website_build.service failed",
		},
		{
			name: "known exploited flaws",
			in:   "desk01: installed_packages/known_exploited_flaws: CVE-2026-46300 linux failed",
			want: "desk01: Known exploited flaws: CVE-2026-46300 linux failed",
		},
		{
			name: "filesystem usage check",
			in:   "app001: disks/every_filesystem_is_below_95_percent_full failed",
			want: "app001: Every filesystem is below 95 percent full failed",
		},
		{
			name: "cannot check trailing slug",
			in:   "MMWatchdog cannot check util01: disks/root_disk_usage",
			want: "MMWatchdog cannot check util01: Root disk usage",
		},
		{
			name: "parenthesized slug with trailing period",
			in:   "Check failed (disks/root_disk_usage).",
			want: "Check failed (Root disk usage).",
		},
		{
			name: "ordinary human title unchanged",
			in:   "Database down",
			want: "Database down",
		},
		{
			name: "ordinary backup title unchanged",
			in:   "Backup 10 finished",
			want: "Backup 10 finished",
		},
		{
			name: "fleet hub silence title unchanged",
			in:   "app001 stopped reporting to the MMWatchdog fleet hub",
			want: "app001 stopped reporting to the MMWatchdog fleet hub",
		},
		{
			name: "https url unchanged",
			in:   "Health check failed for https://example.com/health",
			want: "Health check failed for https://example.com/health",
		},
		{
			name: "https url with underscores unchanged",
			in:   "Health check failed for https://my_app.example.com/health_check",
			want: "Health check failed for https://my_app.example.com/health_check",
		},
		{
			name: "http url with underscore path unchanged",
			in:   "http://localhost/status_check returned 503",
			want: "http://localhost/status_check returned 503",
		},
		{
			name: "absolute file path with underscores unchanged",
			in:   "Cannot read /var/lib/mmhotline_state/config_file",
			want: "Cannot read /var/lib/mmhotline_state/config_file",
		},
		{
			name: "playbook file path with extension unchanged",
			in:   "Playbook one_time_playbooks/free_disk_space.yml failed",
			want: "Playbook one_time_playbooks/free_disk_space.yml failed",
		},
		{
			name: "ordinary English slash pair unchanged",
			in:   "Cannot read/write on app001 (yes/no and/or 10/20)",
			want: "Cannot read/write on app001 (yes/no and/or 10/20)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanizeAlertTitle(tc.in); got != tc.want {
				t.Fatalf("humanizeAlertTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func renderedText(htmlDoc string) string {
	return pageText(strings.ReplaceAll(htmlDoc, "<wbr>", ""))
}

func TestAlertPagesLeadWithTriageSummaryAndHumanizedTitles(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)

	const (
		rawWarnTitle     = "util01: systemd_units/no_systemd_unit_has_failed: release_the_newest_website_build.service failed"
		rawResolvedTitle = "util01: systemd_units/no_systemd_unit_has_failed: release_the_newest_website_build.service ok"
		triageHeadline   = "The website release job failed on util01"
		triageReportBody = "# " + triageHeadline + "\n\nThe release timer exited with status 1 because the deploy lock was busy. No user-facing outage occurred."
	)

	warn := r.warnArrives(t, rawWarnTitle, "util01-release-unit")
	run := r.runStarted(t)
	r.at(t, time.Minute)
	answer := report(t, run, json.NewDecoder(run.connection), "0000000000000001", "warn", triageReportBody, warn.ID)
	if answer.Outcome != triageagent.ReportRecorded {
		t.Fatalf("report answer: %+v", answer)
	}
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	waitNoTriageRun(t, r.a)

	var reportAlertID, sessionID string
	if err := r.a.Store.db.QueryRow(`SELECT q.report_alert_id, r.triage_session_id FROM triage_agent_requests q JOIN triage_agent_runs r ON r.id=q.run_id WHERE q.request_id='0000000000000001'`).Scan(&reportAlertID, &sessionID); err != nil || reportAlertID == "" || sessionID == "" {
		t.Fatalf("lookup report/session: %q %q %v", reportAlertID, sessionID, err)
	}

	// 1. Covered warn on /alerts and /alerts/<id>.
	historyPage := r.get(t, "/alerts")
	for _, want := range []string{triageHeadline, "Explained by triage agent", "Original alert:"} {
		if !strings.Contains(historyPage, want) {
			t.Fatalf("/alerts missing %q:\n%s", want, historyPage)
		}
	}
	if !strings.Contains(renderedText(historyPage), "Original alert: "+rawWarnTitle) {
		t.Fatalf("/alerts missing original raw title:\n%s", renderedText(historyPage))
	}

	warnPage := r.get(t, "/alerts/"+warn.ID)
	for _, want := range []string{
		"<h1>" + triageHeadline + "</h1>",
		`id="triage-agent-summary"`,
		"Triage agent summary",
		`<p class="finding">` + triageHeadline + `</p>`,
		"The release timer exited with status 1",
		`href="/alerts/` + reportAlertID + `">View triage report</a>`,
		`href="/triage-sessions/` + sessionID + `">Triage session</a>`,
		`<details class="more"><summary>Full alert message</summary>`,
	} {
		if !strings.Contains(warnPage, want) {
			t.Fatalf("covered warn page missing %q:\n%s", want, warnPage)
		}
	}
	if strings.Contains(warnPage, `<details class="more" open><summary>Full alert message</summary>`) {
		t.Fatalf("covered warn page left Full alert message open by default:\n%s", warnPage)
	}
	if !strings.Contains(renderedText(warnPage), "Original alert: "+rawWarnTitle) {
		t.Fatalf("covered warn page missing original raw title:\n%s", renderedText(warnPage))
	}

	// Triage agent's own system report alert is not redundantly wrapped.
	reportPage := r.get(t, "/alerts/"+reportAlertID)
	if strings.Contains(reportPage, `id="triage-agent-summary"`) {
		t.Fatalf("triage agent's own system alert should not show a nested triage summary:\n%s", reportPage)
	}

	// 2. Resolved warning in the same incident cycle leads with "Resolved: <triage title>".
	r.at(t, 2*time.Minute)
	resolvedWarn := admitSignedAlert(t, r.a, r.source, "resolved-warn", rawResolvedTitle, "util01-release-unit")
	backgroundRounds(t, r.a)

	resolvedHeadline := "Resolved: " + triageHeadline
	resolvedHistory := r.get(t, "/alerts")
	if !strings.Contains(resolvedHistory, resolvedHeadline) {
		t.Fatalf("/alerts missing %q for resolved-warn:\n%s", resolvedHeadline, resolvedHistory)
	}
	resolvedPage := r.get(t, "/alerts/"+resolvedWarn.ID)
	for _, want := range []string{
		"<h1>" + resolvedHeadline + "</h1>",
		`id="triage-agent-summary"`,
		`<details class="more"><summary>Full alert message</summary>`,
		`href="/alerts/` + reportAlertID + `">View triage report</a>`,
	} {
		if !strings.Contains(resolvedPage, want) {
			t.Fatalf("resolved-warn page missing %q:\n%s", want, resolvedPage)
		}
	}
	if !strings.Contains(renderedText(resolvedPage), "Original alert: "+rawResolvedTitle) {
		t.Fatalf("resolved-warn page missing original raw title:\n%s", renderedText(resolvedPage))
	}

	// 3. A later cycle of the same sender_incident_key without a new report does
	// NOT inherit the old triage report; it falls back to humanizing the slug.
	r.at(t, 3*time.Minute)
	nextRaw := "util01: systemd_units/no_systemd_unit_has_failed: backup.service failed"
	nextWarn := admitSignedAlert(t, r.a, r.source, "warn", nextRaw, "util01-release-unit")
	nextResolved := admitSignedAlert(t, r.a, r.source, "resolved-warn", "util01: systemd_units/no_systemd_unit_has_failed: backup.service ok", "util01-release-unit")
	_ = nextWarn

	nextResolvedPage := r.get(t, "/alerts/"+nextResolved.ID)
	wantNextHumanized := "util01: No systemd unit has failed: backup.service ok"
	if !strings.Contains(nextResolvedPage, "<h1>"+wantNextHumanized+"</h1>") {
		t.Fatalf("later cycle resolved-warn should not inherit old triage report; got:\n%s", nextResolvedPage)
	}
	if strings.Contains(nextResolvedPage, `id="triage-agent-summary"`) {
		t.Fatalf("later cycle resolved-warn unexpectedly showed old triage summary:\n%s", nextResolvedPage)
	}
	if !strings.Contains(nextResolvedPage, `<details class="more" open><summary>Full alert message</summary>`) {
		t.Fatalf("uncovered alert should keep Full alert message open:\n%s", nextResolvedPage)
	}

	// 4. Uncovered raw check slug alert vs ordinary human title.
	uncoveredSlugRaw := "desk01: installed_packages/known_exploited_flaws: CVE-2026-46300 linux failed"
	uncoveredSlug := admitSignedAlert(t, r.a, r.source, "info", uncoveredSlugRaw, "")
	ordinary := admitSignedAlert(t, r.a, r.source, "info", "Backup 10 finished", "")

	// Database keeps raw title verbatim.
	stored, err := r.a.GetAlert(context.Background(), r.s.Principal, uncoveredSlug.ID)
	if err != nil || stored.Title != uncoveredSlugRaw {
		t.Fatalf("stored alert title = %q (err %v), want raw %q", stored.Title, err, uncoveredSlugRaw)
	}

	slugPage := r.get(t, "/alerts/"+uncoveredSlug.ID)
	wantSlugHuman := "desk01: Known exploited flaws: CVE-2026-46300 linux failed"
	if !strings.Contains(slugPage, "<h1>"+wantSlugHuman+"</h1>") || !strings.Contains(slugPage, `<details class="more" open><summary>Full alert message</summary>`) {
		t.Fatalf("uncovered slug page:\n%s", slugPage)
	}
	if !strings.Contains(renderedText(slugPage), "Original alert: "+uncoveredSlugRaw) {
		t.Fatalf("uncovered slug page missing Original alert line:\n%s", renderedText(slugPage))
	}

	ordinaryPage := r.get(t, "/alerts/"+ordinary.ID)
	if !strings.Contains(ordinaryPage, "<h1>Backup 10 finished</h1>") || strings.Contains(ordinaryPage, "Original alert:") || !strings.Contains(ordinaryPage, `<details class="more" open><summary>Full alert message</summary>`) {
		t.Fatalf("ordinary alert page should remain unchanged:\n%s", ordinaryPage)
	}
}

func TestResolvedWarnNotificationBehaviorForWarnVsInfoTakeover(t *testing.T) {
	parallelStartingProcesses(t)

	t.Run("taken over at warn sends resolved-warn with triage plain-English title", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		const (
			rawWarn     = "util01: systemd_units/no_systemd_unit_has_failed: release.service failed"
			rawResolved = "util01: systemd_units/no_systemd_unit_has_failed: release.service ok"
			headline    = "Website release service failed on util01"
		)
		warn := r.warnArrives(t, rawWarn, "util01-release")
		run := r.runStarted(t)
		r.at(t, time.Minute)
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "warn", "# "+headline+"\n\nNeeds a quick look.", warn.ID)
		r.sentOnce(t, headline, 0)

		r.at(t, 4*time.Minute)
		admitSignedAlert(t, r.a, r.source, "resolved-warn", rawResolved, "util01-release")
		r.advance(2 * time.Minute)
		backgroundRounds(t, r.a)

		wantTitle := "[Resolved warning] Resolved: " + headline
		var matched []fakesendingservices.PushoverMessage
		for _, m := range r.fake.PushoverMessages() {
			if m.Title == wantTitle {
				matched = append(matched, m)
			}
		}
		if len(matched) != 2 {
			t.Fatalf("got %d resolved-warn Pushover messages titled %q (want 2); all messages: %+v", len(matched), wantTitle, r.fake.PushoverMessages())
		}
		for _, m := range matched {
			if !strings.Contains(m.Message, "Original alert: "+rawResolved) {
				t.Fatalf("resolved-warn notification body missing original raw alert title: %q", m.Message)
			}
		}
	})

	t.Run("taken over at info suppresses resolved-warn for warn-only contact methods", func(t *testing.T) {
		r := newHeldWarnRig(t, 15)
		// Subscribe both Pushover contact methods to warn + sos only (no info).
		if _, err := r.a.Store.db.Exec(`DELETE FROM contact_method_subscriptions WHERE severity='info'`); err != nil {
			t.Fatal(err)
		}
		before := len(r.fake.PushoverMessages())
		const (
			rawWarn     = "desk01: installed_packages/known_exploited_flaws: CVE-2026-46300 linux failed"
			rawResolved = "desk01: installed_packages/known_exploited_flaws: CVE-2026-46300 linux ok"
		)
		warn := r.warnArrives(t, rawWarn, "desk01-cve")
		run := r.runStarted(t)
		r.at(t, time.Minute)
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "info", "# False positive CVE on desk01\n\nDebian backported the fix already.", warn.ID)
		sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		waitNoTriageRun(t, r.a)

		r.at(t, 4*time.Minute)
		resolved := admitSignedAlert(t, r.a, r.source, "resolved-warn", rawResolved, "desk01-cve")
		r.advance(2 * time.Minute)
		backgroundRounds(t, r.a)

		if got := r.fake.PushoverMessages()[before:]; len(got) != 0 {
			t.Fatalf("warn-only contact methods received unexpected Pushover messages after info takeover: %+v", got)
		}
		storedResolved, err := r.a.GetAlert(context.Background(), r.s.Principal, resolved.ID)
		if err != nil {
			t.Fatal(err)
		}
		if storedResolved.Notes != resolvedWarnOfUnsentWarnsNote {
			t.Fatalf("resolved-warn notes = %q, want %q", storedResolved.Notes, resolvedWarnOfUnsentWarnsNote)
		}
	})

	t.Run("uncovered raw slug alert humanizes outgoing notification title and keeps raw in body", func(t *testing.T) {
		r := newHeldWarnRig(t, 0)
		const rawWarn = "app001: disks/every_filesystem_is_below_95_percent_full failed"
		warn := r.warnArrives(t, rawWarn, "app001-disk")
		r.advance(2 * time.Minute)
		backgroundRounds(t, r.a)

		wantTitle := "[Warning] app001: Every filesystem is below 95 percent full failed"
		var matched []fakesendingservices.PushoverMessage
		for _, m := range r.fake.PushoverMessages() {
			if m.Title == wantTitle {
				matched = append(matched, m)
			}
		}
		if len(matched) != 2 {
			t.Fatalf("got %d Pushover messages titled %q (want 2); all: %+v", len(matched), wantTitle, r.fake.PushoverMessages())
		}
		for _, m := range matched {
			if !strings.Contains(m.Message, "Original alert: "+rawWarn) {
				t.Fatalf("notification body missing original raw title: %q", m.Message)
			}
		}
		stored, err := r.a.GetAlert(context.Background(), r.s.Principal, warn.ID)
		if err != nil || stored.Title != rawWarn {
			t.Fatalf("stored alert title = %q (err %v), want raw %q", stored.Title, err, rawWarn)
		}
	})
}

func TestResolvedSOSNotificationUsesIncidentTriageReportTitle(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)
	const (
		rawSOS     = "util01: disks/root_disk_usage failed"
		rawBack    = "util01: disks/root_disk_usage ok"
		reportHead = "Root disk on util01 filled up with old build logs"
	)
	sos := r.sosArrives(t, rawSOS, "util01-disk")
	// Add a second alert in the same run so a report without --about-alert-id
	// records no direct triage_agent_covered_alerts row and exercises the
	// incident_actions fallback cleanly.
	admitSignedAlert(t, r.a, r.source, "warn", "util01: memory/available_memory failed", "util01-mem")
	run := r.runStarted(t)
	r.at(t, time.Minute)
	report(t, run, json.NewDecoder(run.connection), "0000000000000001", "sos", "# "+reportHead+"\n\nFreed space or needs cleanup.")
	r.at(t, 2*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("SOS did not wake the person on call")
	}
	if covered := countRows(t, r.a, `SELECT count(*) FROM triage_agent_covered_alerts WHERE alert_id=?`, sos.ID); covered != 0 {
		t.Fatalf("expected 0 direct covered rows to test incident_actions fallback, got %d", covered)
	}

	// Opening SOS alert page shows the triage summary.
	sosPage := r.get(t, "/alerts/"+sos.ID)
	if !strings.Contains(sosPage, "<h1>"+reportHead+"</h1>") || !strings.Contains(sosPage, `id="triage-agent-summary"`) {
		t.Fatalf("SOS alert page missing triage summary:\n%s", sosPage)
	}

	resolved := admitSignedAlert(t, r.a, r.source, "resolved-sos", rawBack, "util01-disk")
	r.advance(2 * time.Minute)
	backgroundRounds(t, r.a)

	// Resolved SOS page leads with "Resolved: <reportHead>".
	resolvedPage := r.get(t, "/alerts/"+resolved.ID)
	if !strings.Contains(resolvedPage, "<h1>Resolved: "+reportHead+"</h1>") {
		t.Fatalf("resolved-sos page missing plain-English heading:\n%s", resolvedPage)
	}

	wantTitle := "[Resolved SOS] Resolved: " + reportHead
	found := false
	for _, m := range r.fake.PushoverMessages() {
		if m.Title == wantTitle {
			found = true
			if !strings.Contains(m.Message, "Original alert: "+rawSOS) {
				t.Fatalf("resolved-sos notification missing original alert title: %q", m.Message)
			}
		}
	}
	if !found {
		t.Fatalf("no resolved-sos Pushover message titled %q; all messages: %+v", wantTitle, r.fake.PushoverMessages())
	}
}
