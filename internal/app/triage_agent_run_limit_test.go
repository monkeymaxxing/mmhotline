package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// The triage agent's run limit, walked through MMHotline's own paths: the
// alert intake, the background loop, a fake sandbox, a stand-in MMX, and a
// fake Pushover. Earlier runs of the day are recorded as the runs table
// keeps them, so a test starts at 49 or 50 without running 49 agents.

// setRunLimit applies triage_agent.most_runs_in_24_hours as the settings
// file applies it.
func setRunLimit(t *testing.T, a *App, value string) {
	t.Helper()
	outcomes, err := a.applySettingsParts(context.Background(), Settings{TriageAgent: &SettingsTriageAgent{MostRunsIn24Hours: value}}, nil, []settingsFilePart{settingsMostRunsIn24HoursPart})
	if err != nil || outcomes[0].State != "applied" {
		t.Fatalf("applying the run limit %q: %+v %v", value, outcomes, err)
	}
}

// forgetEarlierRuns removes the software review the rigs record as just
// finished, so a test counts only its own runs. No review interval is set,
// so no review becomes due.
func forgetEarlierRuns(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.Store.db.Exec(`DELETE FROM triage_agent_runs`); err != nil {
		t.Fatal(err)
	}
}

// recordRun records one finished run that started at started.
func recordRun(t *testing.T, a *App, purpose, startedBy, sessionID string, started time.Time, endCause runEndCause) string {
	t.Helper()
	id := NewID()
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by,ended_at,triage_session_id,end_cause,input_tokens,cached_input_tokens,output_tokens) VALUES(?,?,'codex','api_key','finished',?,?,?,?,?,410000,380000,2100)`,
		id, purpose, started.Unix(), startedBy, started.Add(time.Minute).Unix(), sessionID, endCause); err != nil {
		t.Fatal(err)
	}
	return id
}

// recordSessionRuns records n finished runs of a past triage session about
// alert, the first carrying it, one minute apart from first, as one long
// incident's runs are kept.
func recordSessionRuns(t *testing.T, a *App, alert Alert, n int, first time.Time) {
	t.Helper()
	sessionID := NewID()
	if _, err := a.Store.db.Exec(`INSERT INTO triage_sessions(id,started_at,ended_at,ended_because) VALUES(?,?,?,'test')`, sessionID, first.Unix(), first.Add(time.Duration(n)*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		runID := recordRun(t, a, "triage_session", "MMHotline", sessionID, first.Add(time.Duration(i)*time.Minute), runEndedOnItsOwn)
		if i == 0 {
			if _, err := a.Store.db.Exec(`INSERT INTO triage_session_alerts(triage_session_id,alert_id,joined_at,sent_in_run_id) VALUES(?,?,?,?)`, sessionID, alert.ID, first.Unix(), runID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// fillCountedRuns records counted runs of an earlier triage session until n
// count, one minute apart from first.
func fillCountedRuns(t *testing.T, a *App, source CreatedSource, n int, first time.Time) {
	t.Helper()
	if missing := n - runLimitStateOf(t, a).counted(); missing > 0 {
		// Info, so the alert itself starts no session now.
		earlier := admitSignedAlert(t, a, source, "info", fmt.Sprintf("Earlier finding %s", NewID()[:6]), "")
		recordSessionRuns(t, a, earlier, missing, first)
	}
}

func runLimitStateOf(t *testing.T, a *App) runLimitState {
	t.Helper()
	var state runLimitState
	if err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		state, err = readRunLimitState(context.Background(), tx, a.Now().UTC(), "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

// driveRunLimit is the background loop's turn for the run limit.
func driveRunLimit(t *testing.T, a *App) {
	t.Helper()
	if err := a.DriveTriageAgentRunLimit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// runLimitMessages lists MMHotline's own messages about the run limit,
// oldest first, as "severity | title | message".
func runLimitMessages(t *testing.T, a *App) []string {
	t.Helper()
	rows, err := a.Store.db.Query(`SELECT severity,title,message FROM alerts WHERE actor=? ORDER BY sequence`, runLimitActor)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var messages []string
	for rows.Next() {
		var severity, title, message string
		if err := rows.Scan(&severity, &title, &message); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, severity+" | "+title+" | "+message)
	}
	return messages
}

func passedOver(t *testing.T, a *App, alertID string) bool {
	t.Helper()
	return countRows(t, a, `SELECT count(*) FROM alerts WHERE id=? AND passed_over_for_the_run_limit=1`, alertID) == 1
}

// Two starts racing for the last place: each records its run and asks the
// limit in its own transaction, and only one is admitted.
func TestRunLimitAdmitsOneOfTwoStartsRacingForTheLastPlace(t *testing.T) {
	t.Parallel()
	r := newTriageSessionRig(t, false)
	forgetEarlierRuns(t, r.a)
	setRunLimit(t, r.a, "50")
	fillCountedRuns(t, r.a, r.source, 49, r.a.Now().Add(-time.Hour))
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			errs[i] = admitOneRun(r.a)
		})
	}
	wg.Wait()
	admitted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case !errors.Is(err, errRunLimitReached):
			t.Fatal(err)
		}
	}
	if admitted != 1 || runLimitStateOf(t, r.a).counted() != 50 {
		t.Fatalf("%d starts admitted, %d runs counted; want 1 and 50", admitted, runLimitStateOf(t, r.a).counted())
	}
}

// Run 50 finishes and keeps the holds of the alerts it carried. What waited
// for the next run goes out at once: a held warn no run carried, and an SOS
// that waits and is not carried by the active run, which wakes the person
// on call. Alerts that arrive while the limit holds are marked at intake,
// sent at once with the limit's line, and never join a session, also after
// the count falls and the agent runs again.
func TestRunLimitRun50KeepsItsHoldsAndTheRestGoesStraightToPeople(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)
	forgetEarlierRuns(t, r.a)
	setRunLimit(t, r.a, "50")
	r.start = r.a.Now()
	fillCountedRuns(t, r.a, r.source, 49, r.a.Now().Add(-23*time.Hour))
	app001 := admitSignedAlert(t, r.a, r.source, "warn", app001Warning, "app001-cve")
	// The session's turn alone starts run 50, so the limit's turn has not
	// seen it yet when the next alerts arrive.
	if err := r.a.DriveTriageSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	run50 := <-r.runs
	r.started = append(r.started, run50)
	sendTriageEvent(t, run50, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	sos := admitSignedAlert(t, r.a, r.source, "sos", "The server db02 stopped answering", "db02")
	disk := admitSignedAlert(t, r.a, r.source, "warn", diskWarning, "db001-disk")
	r.at(t, 10*time.Second)

	if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeLimitHolds || state.counted() != 50 {
		t.Fatalf("notice %q with %d runs; want the limit to hold at 50", state.notice, state.counted())
	}
	messages := runLimitMessages(t, r.a)
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "warn | Agent run limit reached. Alerts continue. | The limit stops only agent runs, never alerts. New alerts now go straight to people: warnings are not held, and an SOS wakes the person on call without the agent's wait. The current run finishes its work. The agent can look at new alerts again at "+r.start.Add(time.Hour+9*time.Minute).UTC().Format("15:04")+" UTC.\n\nWhat used the runs in the last 24 hours:\n- 49 runs: Earlier finding ") {
		t.Fatalf("messages about the limit: %q", messages)
	}
	// The waiting SOS wakes the person on call at once, with the line.
	if !r.woken(sos.IncidentID) || !strings.HasPrefix(r.emergencyMessage(t, sos.IncidentID), runLimitPassedOverLine+"\n\n") {
		t.Fatalf("the waiting SOS: woken %v", r.woken(sos.IncidentID))
	}
	if history := r.history(t, sos.IncidentID); !inOrder(history, "Held for the triage agent until", runLimitSOSHistoryLine) {
		t.Fatalf("history: %q", history)
	}
	// The warn that waited for the next run goes out at once, marked; run
	// 50's own warn stays held.
	if outcome, reason, _, _ := r.holdOf(t, disk.ID); outcome != "delivered" || reason != runLimitReleaseReason || !passedOver(t, r.a, disk.ID) {
		t.Fatalf("the waiting warn: %q %q", outcome, reason)
	}
	if _, _, ended, released := r.holdOf(t, app001.ID); ended || released || passedOver(t, r.a, app001.ID) {
		t.Fatal("run 50's warn was released")
	}
	// A warn that arrives while the limit holds is marked and goes at once.
	later := admitSignedAlert(t, r.a, r.source, "warn", loadWarning, "app002-load")
	r.at(t, 20*time.Second)
	if !passedOver(t, r.a, later.ID) || countRows(t, r.a, `SELECT count(*) FROM held_warn_alerts WHERE alert_id=?`, later.ID) != 0 {
		t.Fatal("a warn during the limit was held")
	}
	// Run 50 reports on its own warn, which nobody hears about.
	agentRequest(t, run50, json.NewDecoder(run50.connection), reportEvent("0000000000000001", "info", "# app001 runs a fixed openssh\n"))
	sendTriageEvent(t, run50, triageagent.Event{Event: "finished", Answer: "Reported."})
	waitNoTriageRun(t, r.a)
	r.at(t, 3*time.Minute)
	if outcome, _, _, _ := r.holdOf(t, app001.ID); outcome != "taken_over" || r.warnPushovers(app001Warning) != 0 {
		t.Fatalf("run 50's warn: %q, %d messages", outcome, r.warnPushovers(app001Warning))
	}
	for _, title := range []string{diskWarning, loadWarning} {
		found := false
		for _, m := range r.pushovers(title) {
			found = found || strings.HasPrefix(m.Message, runLimitPassedOverLine)
		}
		if !found {
			t.Errorf("no Pushover message about %q starts with the limit's line", title)
		}
	}
	for _, alert := range []Alert{sos, disk, later} {
		if joined := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=? AND sent_in_run_id!=''`, alert.ID); joined != 0 {
			t.Errorf("%q was carried to the agent", alert.Title)
		}
	}
	waitTriageIdleOrRunning(t, r.a)
	select {
	case run := <-r.runs:
		t.Fatalf("a run started while the limit holds: %s", run.request.Prompt)
	default:
	}

	// An hour and 9 minutes on, the 10th of the earlier runs leaves the 24 hours and the count
	// falls to 40: the agent runs again, and a new warn is held and goes to
	// the agent; nothing from the limit does.
	r.at(t, time.Hour+9*time.Minute)
	if messages = runLimitMessages(t, r.a); len(messages) != 2 || messages[1] != "info | The triage agent can run again. Alerts wait for it again. | It has used 40 of its 50 runs in the last 24 hours. Alerts that arrived during the limit already reached people and are not sent to the agent." {
		t.Fatalf("messages about the limit: %q", messages)
	}
	fresh := admitSignedAlert(t, r.a, r.source, "warn", db01Warning, "db01-cve")
	run := r.runStarted(t)
	if passedOver(t, r.a, fresh.ID) || !strings.Contains(run.request.Prompt, "Alert ID: "+fresh.ID) {
		t.Fatalf("the new warn did not reach the agent:\n%s", run.request.Prompt)
	}
	for _, alert := range []Alert{sos, disk, later} {
		if strings.Contains(run.request.Prompt, alert.ID) {
			t.Errorf("%q, which went out during the limit, reached the agent", alert.Title)
		}
	}
}

// The limit holds from 50 until the count falls to 40, and the agent stays
// off at 49. The heads-up at 40 and the limit's warning each go once, and
// "can run again" goes at 40. A limit of 1 holds until no counted run is
// left; with limits of 1 and 4, no heads-up goes out.
func TestRunLimitHoldsUntil80PercentAndSaysSoOnce(t *testing.T) {
	t.Parallel()
	t.Run("50", func(t *testing.T) {
		r := newTriageSessionRig(t, false)
		forgetEarlierRuns(t, r.a)
		setRunLimit(t, r.a, "50")
		start := r.a.Now()
		db01 := admitSignedAlert(t, r.a, r.source, "warn", "db01: installed_packages/known_exploited_flaws: CVE-2026-123158 openssh failed", "db01-cve")
		recordSessionRuns(t, r.a, db01, 39, start.Add(-23*time.Hour))
		driveRunLimit(t, r.a)
		if messages := runLimitMessages(t, r.a); len(messages) != 0 {
			t.Fatalf("messages at 39 runs: %q", messages)
		}
		recordRun(t, r.a, "software_review", "MMHotline", "", start.Add(-22*time.Hour), runEndedOnItsOwn)
		driveRunLimit(t, r.a)
		driveRunLimit(t, r.a)
		headsUp := "info | Agent runs: 40 of 50 used in 24 hours. Alerts continue. | The limit stops only agent runs, never alerts. At 50 runs, MMHotline stops sending new alerts to the triage agent until the count falls back to 40.\nMost runs went to:\n- 39 runs: db01: installed_packages/known_exploited_flaws: CVE-2026-123158 openssh failed\n- 1 run: the scheduled software review\nNothing is needed from you now."
		if messages := runLimitMessages(t, r.a); !slices.Equal(messages, []string{headsUp}) {
			t.Fatalf("messages at 40 runs: %q", messages)
		}
		fillCountedRuns(t, r.a, r.source, 50, start.Add(-21*time.Hour))
		driveRunLimit(t, r.a)
		driveRunLimit(t, r.a)
		messages := runLimitMessages(t, r.a)
		if len(messages) != 2 || !strings.HasPrefix(messages[1], "warn | Agent run limit reached. Alerts continue. | The limit stops only agent runs, never alerts. New alerts now go straight to people: warnings are not held, and an SOS wakes the person on call without the agent's wait. The agent can look at new alerts again at 13:09 UTC.") {
			t.Fatalf("messages at 50 runs: %q", messages)
		}
		// The earliest runs started at 13:00 yesterday, one a minute: 10 must
		// leave the 24 hours for the count to fall to 40, the 10th at 13:09.
		// At 13:00 the count is 49, and the agent stays off: a counted run
		// is refused. At 13:08 it is 41.
		for _, at := range []time.Duration{time.Hour, time.Hour + 8*time.Minute} {
			r.advance(start.Add(at).Sub(r.a.Now()))
			driveRunLimit(t, r.a)
			if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeLimitHolds {
				t.Fatalf("at %s with %d runs the limit ended", r.a.Now().UTC().Format("15:04"), state.counted())
			}
			if err := admitOneRun(r.a); !errors.Is(err, errRunLimitReached) {
				t.Fatalf("a run at %d counted runs: %v", runLimitStateOf(t, r.a).counted(), err)
			}
		}
		r.advance(start.Add(time.Hour + 9*time.Minute).Sub(r.a.Now()))
		driveRunLimit(t, r.a)
		driveRunLimit(t, r.a)
		messages = runLimitMessages(t, r.a)
		if state := runLimitStateOf(t, r.a); len(messages) != 3 || state.counted() != 40 || messages[2] != "info | The triage agent can run again. Alerts wait for it again. | It has used 40 of its 50 runs in the last 24 hours. Alerts that arrived during the limit already reached people and are not sent to the agent." {
			t.Fatalf("at 40 runs (%d counted): %q", state.counted(), messages)
		}
	})
	for _, c := range []struct {
		limit, holdsUntil int
	}{{1, 0}, {4, 3}} {
		t.Run(fmt.Sprint(c.limit), func(t *testing.T) {
			r := newTriageSessionRig(t, false)
			forgetEarlierRuns(t, r.a)
			setRunLimit(t, r.a, fmt.Sprint(c.limit))
			start := r.a.Now()
			for i := range c.limit {
				recordRun(t, r.a, "commit_review", "MMHotline", "", start.Add(-23*time.Hour+time.Duration(i)*time.Minute), runEndedOnItsOwn)
				driveRunLimit(t, r.a)
			}
			messages := runLimitMessages(t, r.a)
			if len(messages) != 1 || !strings.HasPrefix(messages[0], "warn | Agent run limit reached. Alerts continue.") || !strings.Contains(messages[0], fmt.Sprintf("- %s: the commit reviews", countText(c.limit, "run", "runs"))) {
				t.Fatalf("messages: %q", messages)
			}
			// The first run leaves the 24 hours at 13:00.
			r.advance(start.Add(time.Hour - time.Minute).Sub(r.a.Now()))
			driveRunLimit(t, r.a)
			if runLimitStateOf(t, r.a).notice != runLimitNoticeLimitHolds {
				t.Fatalf("the limit of %d ended before a run left", c.limit)
			}
			r.advance(time.Minute)
			driveRunLimit(t, r.a)
			if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeNone || state.counted() != c.holdsUntil || len(runLimitMessages(t, r.a)) != 2 {
				t.Fatalf("at %d runs: notice %q, messages %q", state.counted(), state.notice, runLimitMessages(t, r.a))
			}
		})
	}
}

// admitOneRun records a commit review and asks the run limit, as a run's
// start does; a refusal records nothing.
func admitOneRun(a *App) error {
	return a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		id := NewID()
		if _, err := tx.Exec(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by) VALUES(?,'commit_review','codex','api_key','running',?,'MMHotline')`, id, a.Now().Unix()); err != nil {
			return err
		}
		return admitRunUnderTheRunLimit(context.Background(), tx, id, a.Now())
	})
}

// The count holds only the runs MMHotline starts by itself in the last 24
// hours: not a setup test, a sign-in check, a person's software review, a
// playbook review, a person's hand-over, a run that MMHotline's stop or
// restart cut short, or a review stopped for a triage session.
func TestRunLimitCountsOnlyTheRunsMMHotlineStartsByItself(t *testing.T) {
	t.Parallel()
	r := newTriageSessionRig(t, false)
	forgetEarlierRuns(t, r.a)
	setRunLimit(t, r.a, "50")
	now := r.a.Now()
	recordRun(t, r.a, "triage_session", "MMHotline", "", now.Add(-time.Hour), runEndedOnItsOwn)
	recordRun(t, r.a, "software_review", "MMHotline", "", now.Add(-time.Hour), runEndedOnItsOwn)
	recordRun(t, r.a, "commit_review", "MMHotline", "", now.Add(-time.Hour), runEndedOnItsOwn)
	for _, run := range []struct {
		purpose, startedBy string
		cause              runEndCause
	}{
		{"setup_test", "Barbara Example", runEndedOnItsOwn},
		{"sign_in_check", "MMHotline", runEndedOnItsOwn},
		{"software_review", "Barbara Example", runEndedOnItsOwn},
		{"playbook_review", "MMHotline", runEndedOnItsOwn},
		{"triage_session", "MMHotline", runCutByMMHotlineRestart},
		{"triage_session", "MMHotline", runCutByMMHotlineStop},
		{"software_review", "MMHotline", runStoppedForTriageSession},
		{"commit_review", "MMHotline", runStoppedForTriageSession},
	} {
		recordRun(t, r.a, run.purpose, run.startedBy, "", now.Add(-time.Hour), run.cause)
	}
	handOver := recordRun(t, r.a, "triage_session", "MMHotline", "", now.Add(-time.Hour), runEndedOnItsOwn)
	if _, err := r.a.Store.db.Exec(`INSERT INTO triage_sessions(id,started_at,taken_over_by_member_id,taken_over_at,hand_over_run_id) VALUES(?,?,'m',?,?)`, NewID(), now.Add(-2*time.Hour).Unix(), now.Add(-time.Hour).Unix(), handOver); err != nil {
		t.Fatal(err)
	}
	recordRun(t, r.a, "triage_session", "MMHotline", "", now.Add(-24*time.Hour), runEndedOnItsOwn)
	view, err := r.a.TriageAgentState(context.Background(), r.p)
	if err != nil {
		t.Fatal(err)
	}
	if view.RunLimit == nil || view.RunLimit.CountedRunsInTheLast24Hours != 3 || view.RunLimit.Text() != "Runs that count toward the run limit in the last 24 hours: 3 of 50." {
		t.Fatalf("the run limit's view: %+v", view.RunLimit)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"run_limit":{"most_runs_in_24_hours":50,"counted_runs_in_the_last_24_hours":3,"run_limit_notice":"none"}`) {
		t.Fatalf("show --json: %s", encoded)
	}
}

// With no_limit, MMHotline counts runs and never stops one.
func TestNoRunLimitNeverStopsARun(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	setRunLimit(t, r.a, "no_limit")
	fillCountedRuns(t, r.a, r.source, 200, r.a.Now().Add(-23*time.Hour))
	driveRunLimit(t, r.a)
	warn := r.alert(t, "warn", "Disk of db001 is filling", "db001-disk")
	r.drive(t)
	run := r.nextRun(t)
	if !strings.Contains(run.request.Prompt, "Alert ID: "+warn.ID) || passedOver(t, r.a, warn.ID) {
		t.Fatalf("the run does not carry the warn:\n%s", run.request.Prompt)
	}
	driveRunLimit(t, r.a)
	if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeNone || state.counted() != 201 || len(runLimitMessages(t, r.a)) != 0 {
		t.Fatalf("notice %q, %d runs, messages %q", state.notice, state.counted(), runLimitMessages(t, r.a))
	}
}

// Raising the limit above the count, or setting no_limit, ends a limit that
// holds at once, with "can run again"; lowering it does not.
func TestRaisingTheRunLimitEndsItAtOnce(t *testing.T) {
	t.Parallel()
	r := newTriageSessionRig(t, false)
	forgetEarlierRuns(t, r.a)
	setRunLimit(t, r.a, "50")
	fillCountedRuns(t, r.a, r.source, 50, r.a.Now().Add(-23*time.Hour))
	driveRunLimit(t, r.a)
	setRunLimit(t, r.a, "45")
	if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeLimitHolds {
		t.Fatalf("lowering the limit ended it: %q", state.notice)
	}
	setRunLimit(t, r.a, "55")
	messages := runLimitMessages(t, r.a)
	if state := runLimitStateOf(t, r.a); state.notice == runLimitNoticeLimitHolds || len(messages) != 2 || messages[1] != "info | The triage agent can run again. Alerts wait for it again. | It has used 50 of its 55 runs in the last 24 hours. Alerts that arrived during the limit already reached people and are not sent to the agent." {
		t.Fatalf("after raising the limit to 55: %q, %q", state.notice, messages)
	}
	fillCountedRuns(t, r.a, r.source, 55, r.a.Now().Add(-22*time.Hour))
	driveRunLimit(t, r.a)
	setRunLimit(t, r.a, "no_limit")
	messages = runLimitMessages(t, r.a)
	if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeNone || len(messages) != 4 || messages[3] != "info | The triage agent can run again. Alerts wait for it again. | It has used 55 runs in the last 24 hours; triage_agent.most_runs_in_24_hours is now no_limit. Alerts that arrived during the limit already reached people and are not sent to the agent." {
		t.Fatalf("after no_limit: %q, %q", state.notice, messages)
	}
}

// Run 50's fix fails after the run ended: the limit refuses the run that
// would take the result to the agent, so the result goes to a person once,
// with the alerts of the problem it was for.
func TestRunLimitSendsAFailedFixAfterRun50ToAPersonOnce(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareWarnedRun(t, r.a, fakeRunMMX(t.TempDir(), "echo 'fatal: app002 is unreachable'\nexit 2\n"), 10)
	setRunLimit(t, r.a, "50")
	sos, run, _ := approvalSession(t, r)
	fillCountedRuns(t, r.a, r.source, 50, r.a.Now().Add(-23*time.Hour))
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Asked for a reboot."})
	driveRunLimit(t, r.a)
	r.advance(10 * time.Minute)
	runApprovedPlaybooks(t, r.a)
	if record := onlyWarnedRunRecord(t, r.a); record.state != approvalRanAndFailed || record.resultForTheAgent != resultForTheAgentWaits {
		t.Fatalf("the warned run: %+v", record)
	}
	driveRunLimit(t, r.a)
	driveRunLimit(t, r.a)
	r.drive(t)
	r.noRunStarts(t)
	rows, err := r.a.Store.db.Query(`SELECT severity,title,message,incident_id FROM alerts WHERE actor=?`, runLimitPlaybookRunResultActor)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for rows.Next() {
		var severity, title, message, incidentID string
		if err := rows.Scan(&severity, &title, &message, &incidentID); err != nil {
			t.Fatal(err)
		}
		results = append(results, severity+" | "+title+" | "+incidentID+" | "+message)
	}
	rows.Close()
	record := onlyWarnedRunRecord(t, r.a)
	want := "warn | Result of " + rebootPlaybook + " in the inventory “production”: Ran and failed | " + sos.IncidentID + " | The triage agent will not look at this result: its run limit was reached. Handle it yourself.\n\n" + record.result + "\n\nThe problem the playbook run was for:\n- app002 stopped answering\n  " + r.a.WebURL + "/alerts/" + sos.ID
	if len(results) != 1 || results[0] != want || record.resultForTheAgent != resultForTheAgentCouldNotReachTheAgent {
		t.Fatalf("results sent to a person:\n%q\nwant:\n%q\nrecord %+v", results, want, record)
	}
	// The result, a warn, is not the limit being reached a second time.
	usage, err := r.a.TriageAgentUsage(context.Background(), r.p, r.a.Now().Add(-7*24*time.Hour))
	if err != nil || usage.TimesTheRunLimitWasReached != 1 {
		t.Fatalf("the run limit was reached %d times; want 1 (%v)", usage.TimesTheRunLimitWasReached, err)
	}
}

// A person's take-over at the limit still gets its hand-over run, which is
// not counted.
func TestRunLimitStillRunsAHandOver(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	setRunLimit(t, r.a, "50")
	r.alert(t, "warn", "Disk of db001 is filling", "db001-disk")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Looked."})
	fillCountedRuns(t, r.a, r.source, 50, r.a.Now().Add(-23*time.Hour))
	driveRunLimit(t, r.a)
	if err := r.a.TakeOverTriageSession(context.Background(), r.p, openTriageSessionID(t, r.a)); err != nil {
		t.Fatal(err)
	}
	r.drive(t)
	if handOver := r.nextRun(t); !strings.Contains(handOver.request.Prompt, "for a hand-over") {
		t.Fatalf("not a hand-over:\n%s", handOver.request.Prompt)
	}
	if state := runLimitStateOf(t, r.a); state.counted() != 50 || state.notice != runLimitNoticeLimitHolds {
		t.Fatalf("after the hand-over started: %d runs, notice %q", state.counted(), state.notice)
	}
}

// admitAlertWithSenderAlertID admits a signed warn with its sender_alert_id
// and says whether intake answered it as already received.
func admitAlertWithSenderAlertID(t *testing.T, a *App, source CreatedSource, severity, title, senderAlertID string) (Alert, bool) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"severity": severity, "title": title, "message": "Details of " + title, "sender_alert_id": senderAlertID})
	if err != nil {
		t.Fatal(err)
	}
	alert, outcome, _, err := a.AdmitGeneric(context.Background(), source.Source.ID, alertSignature(source.Secret, a.Now(), body), body)
	if err != nil {
		t.Fatal(err)
	}
	return alert, outcome.AlreadyReceived
}

// While the limit holds, each alert's notification starts with the limit's
// line, in a message about one alert, in Pushover's combined form, and in an
// emergency message. The stored alert is unchanged, so a sender's retry is
// answered as the duplicate it is.
func TestRunLimitLineInEachNotification(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)
	forgetEarlierRuns(t, r.a)
	setRunLimit(t, r.a, "1")
	recordRun(t, r.a, "commit_review", "MMHotline", "", r.a.Now().Add(-time.Hour), runEndedOnItsOwn)
	backgroundRounds(t, r.a)
	r.advance(2 * time.Minute)
	backgroundRounds(t, r.a)

	single, _ := admitAlertWithSenderAlertID(t, r.a, r.source, "warn", loadWarning, "load-1")
	backgroundRounds(t, r.a)
	var found bool
	for _, m := range r.fake.PushoverMessages() {
		found = found || m.Title == "[Warning] "+loadWarning && strings.HasPrefix(m.Message, runLimitPassedOverLine+"\n\nDetails of "+loadWarning)
	}
	if !found {
		t.Fatalf("no single message starts with the line: %+v", r.fake.PushoverMessages())
	}
	retry, duplicate := admitAlertWithSenderAlertID(t, r.a, r.source, "warn", loadWarning, "load-1")
	if !duplicate || retry.ID != single.ID || retry.Message != "Details of "+loadWarning {
		t.Fatalf("the sender's retry: duplicate %v, %+v", duplicate, retry)
	}

	// The next two wait for Pushover's minute and go out together.
	r.advance(10 * time.Second)
	admitSignedAlert(t, r.a, r.source, "warn", diskWarning, "db001-disk")
	admitSignedAlert(t, r.a, r.source, "warn", db01Warning, "db01-cve")
	backgroundRounds(t, r.a)
	r.advance(2 * time.Minute)
	backgroundRounds(t, r.a)
	found = false
	for _, m := range r.fake.PushoverMessages() {
		found = found || strings.HasPrefix(m.Message, runLimitPassedOverLine+"\n\n") && strings.Contains(m.Message, diskWarning) && strings.Contains(m.Message, db01Warning)
	}
	if !found {
		t.Fatalf("no combined message starts with the line: %+v", r.fake.PushoverMessages())
	}

	sos := admitSignedAlert(t, r.a, r.source, "sos", "The server db02 stopped answering", "db02")
	backgroundRounds(t, r.a)
	if message := r.emergencyMessage(t, sos.IncidentID); !strings.HasPrefix(message, runLimitPassedOverLine+"\n\nDetails of The server db02 stopped answering") {
		t.Fatalf("the emergency message: %q", message)
	}
	if history := r.history(t, sos.IncidentID); !inOrder(history, "SOS incident opened", runLimitSOSHistoryLine) {
		t.Fatalf("history: %q", history)
	}
	page := r.get(t, "/alerts/"+single.ID)
	if !strings.Contains(page, runLimitPassedOverLine) {
		t.Fatal("the alert's page lacks the line")
	}
}

// What used the runs: a run counts toward the problem it carried, named by
// that problem's newest alert; a run that carried none toward its session's
// first alert's problem; a run that carried two problems once, on its own
// line; the top three lines, then one for the rest.
func TestRunLimitSaysWhatUsedTheRuns(t *testing.T) {
	t.Parallel()
	r := newTriageSessionRig(t, false)
	forgetEarlierRuns(t, r.a)
	start := r.a.Now().Add(-23 * time.Hour)
	db01 := admitSignedAlert(t, r.a, r.source, "info", "db01: CVE-2026-123158 openssh", "db01-cve")
	recordSessionRuns(t, r.a, db01, 22, start)
	admitSignedAlert(t, r.a, r.source, "info", "db01: installed_packages/known_exploited_flaws: CVE-2026-123158 openssh failed", "db01-cve")
	for i := range 9 {
		recordRun(t, r.a, "software_review", "MMHotline", "", start.Add(time.Duration(30+i)*time.Minute), runEndedOnItsOwn)
	}
	for i, runs := range []int{3, 2, 2} {
		recordSessionRuns(t, r.a, admitSignedAlert(t, r.a, r.source, "info", fmt.Sprintf("Problem %d", i+2), ""), runs, start.Add(time.Hour))
	}
	sessionID := NewID()
	if _, err := r.a.Store.db.Exec(`INSERT INTO triage_sessions(id,started_at) VALUES(?,?)`, sessionID, start.Unix()); err != nil {
		t.Fatal(err)
	}
	shared := recordRun(t, r.a, "triage_session", "MMHotline", sessionID, start.Add(2*time.Hour), runEndedOnItsOwn)
	for _, key := range []string{"app021", "app022"} {
		alert := admitSignedAlert(t, r.a, r.source, "info", key+" stopped reporting", key)
		if _, err := r.a.Store.db.Exec(`INSERT INTO triage_session_alerts(triage_session_id,alert_id,joined_at,sent_in_run_id) VALUES(?,?,?,?)`, sessionID, alert.ID, start.Unix(), shared); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		lines, err = runLimitUseLines(context.Background(), tx, r.a.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"- 22 runs: db01: installed_packages/known_exploited_flaws: CVE-2026-123158 openssh failed", "- 9 runs: the scheduled software review", "- 3 runs: Problem 2", "- 5 runs: 3 others"}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// The token lines: one per coding agent used in the last 7 days, from its
// finished runs only, with dollars where the coding agent reports them.
func TestRunLimitTokenLines(t *testing.T) {
	t.Parallel()
	r := newTriageSessionRig(t, false)
	forgetEarlierRuns(t, r.a)
	now := r.a.Now()
	for _, run := range []struct {
		codingAgent, state, purpose string
		ago                         time.Duration
		input, cached, output       int64
		cost                        any
	}{
		{"codex", "finished", "triage_session", time.Hour, 400000, 370000, 2000, nil},
		{"codex", "finished", "software_review", 2 * time.Hour, 420000, 390000, 2200, nil},
		{"codex", "failed", "triage_session", time.Hour, 9000000, 0, 90000, nil},
		{"codex", "finished", "triage_session", 8 * 24 * time.Hour, 9000000, 0, 90000, nil},
		{"codex", "finished", "setup_test", time.Hour, 9000000, 0, 90000, nil},
		{"claude_code", "finished", "triage_session", time.Hour, 100000, 90000, 1000, 0.40},
		{"claude_code", "finished", "commit_review", time.Hour, 100000, 90000, 1000, 0.44},
	} {
		if _, err := r.a.Store.db.Exec(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by,input_tokens,cached_input_tokens,output_tokens,cost_in_us_dollars) VALUES(?,?,?,'api_key',?,?,'MMHotline',?,?,?,?)`,
			NewID(), run.purpose, run.codingAgent, run.state, now.Add(-run.ago).Unix(), run.input, run.cached, run.output, run.cost); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		lines, err = runLimitTokenLines(context.Background(), tx, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"In the last 7 days, a finished Claude Code run used on average 100,000 input tokens (90,000 of them cached) and 1,000 output tokens and cost US$0.42 on average.",
		"In the last 7 days, a finished Codex run used on average 410,000 input tokens (380,000 of them cached) and 2,100 output tokens. Codex reports tokens, not dollars.",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// triage-agent usage gives the numbers since a time, in words and as JSON,
// and the software review's prompt carries the last 7 days' numbers.
func TestTriageAgentUsage(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	now := r.a.Now()
	recordRun(t, r.a, "triage_session", "MMHotline", "", now.Add(-2*time.Hour), runEndedOnItsOwn)
	recordRun(t, r.a, "software_review", "Barbara Example", "", now.Add(-3*time.Hour), runEndedOnItsOwn)
	setRunLimit(t, r.a, "1")
	held := r.alert(t, "warn", "Disk of db001 is filling", "db001-disk")
	backgroundRounds(t, r.a)
	if !passedOver(t, r.a, held.ID) {
		t.Fatal("the held warn was not sent straight to people at the limit")
	}
	r.alert(t, "warn", "app002 answers slowly", "")
	backgroundRounds(t, r.a)
	usage, err := r.a.TriageAgentUsage(context.Background(), r.p, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := "Triage agent usage from 2026-09-22 12:00:00 UTC to 2026-09-29 12:00:00 UTC.\n" +
		"Run limit (triage_agent.most_runs_in_24_hours): 1 run in 24 hours.\n" +
		"Runs by purpose and state:\n" +
		"- software_review (started by a person), finished: 1\n" +
		"- triage_session, finished: 1\n" +
		"What the runs used, per coding agent:\n" +
		"- Codex: 2 runs, 2 finished; 820,000 input tokens (760,000 of them cached), 4,200 output tokens; Codex reports tokens, not dollars.\n" +
		"The run limit was reached 1 time. Alerts sent straight to people because of the run limit: 2.\n" +
		"Warnings held for the triage agent: 0 taken over by the agent's report or request, 1 delivered, 0 resolved while held, 0 still held."
	if got := usage.Text(); got != want {
		t.Fatalf("usage:\n%s\nwant:\n%s", got, want)
	}
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"most_runs_in_24_hours":1,"runs":[{"purpose":"software_review","started_by_mmhotline":false,"state":"finished","runs":1},{"purpose":"triage_session","started_by_mmhotline":true,"state":"finished","runs":1}],"coding_agents":[{"coding_agent":"codex","runs":2,"finished_runs":2,"input_tokens":820000,"cached_input_tokens":760000,"output_tokens":4200}],"times_the_run_limit_was_reached":1,"alerts_sent_straight_to_people_for_the_run_limit":2,"held_warnings":{"taken_over":0,"delivered":1,"resolved":0,"still_held":0}}`) {
		t.Fatalf("usage as JSON: %s", encoded)
	}

	// The software review's prompt carries the same numbers for 7 days.
	// The limit skipped the review due now, so the next is due in 24 hours.
	setRunLimit(t, r.a, "no_limit")
	r.advance(24 * time.Hour)
	r.rounds(t)
	review := r.nextReview(t)
	if usage, err = r.a.TriageAgentUsage(context.Background(), r.p, r.a.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(review.request.Prompt, "MMHotline's record of the triage agent's last 7 days, for a task that reviews what the agent costs and how its holds went:\n\n"+usage.Text()+"\n\n") {
		t.Fatalf("the review's prompt lacks the numbers:\n%s\nwant:\n%s", review.request.Prompt, usage.Text())
	}
}

// While the limit holds, a scheduled software review is skipped and the
// skip recorded, and a commit no trusted key signed waits for a person with
// MMHotline's warning.
func TestRunLimitSkipsTheScheduledReview(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	setRunLimit(t, r.a, "1")
	recordRun(t, r.a, "commit_review", "MMHotline", "", r.a.Now().Add(-time.Hour), runEndedOnItsOwn)
	r.rounds(t)
	r.noRunStarts(t)
	view, err := r.a.TriageAgentState(context.Background(), r.p)
	if err != nil {
		t.Fatal(err)
	}
	if view.SoftwareReviewSkippedForTheRunLimitAt == nil || !view.SoftwareReviewSkippedForTheRunLimitAt.Equal(r.a.Now()) || view.NextSoftwareReviewDueAt == nil || !view.NextSoftwareReviewDueAt.Equal(r.a.Now().Add(24*time.Hour)) {
		t.Fatalf("skipped at %v, next due at %v", view.SoftwareReviewSkippedForTheRunLimitAt, view.NextSoftwareReviewDueAt)
	}
	if text := view.Text(); !strings.Contains(text, "Scheduled software review skipped at 2026-09-29T12:00:00Z: the triage agent's run limit had been reached.") {
		t.Fatalf("show: %s", text)
	}
}

// The limit is reached while the ordinary delivery queue is full: the limit
// still holds, the SOS that waits for the agent still wakes the person on
// call, and the limit's warning goes out once, as MMHotline's own SOS does
// with a full queue (checkPendingDeliveryLimits).
func TestRunLimitHoldsAndWakesTheSOSWithAFullDeliveryQueue(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 15)
	r.addEmail(t)
	forgetEarlierRuns(t, r.a)
	// The earlier runs' messages go out first, so the limit's messages
	// cannot join one still waiting.
	setRunLimit(t, r.a, "no_limit")
	fillCountedRuns(t, r.a, r.source, 50, r.a.Now().Add(-23*time.Hour))
	r.advance(2 * time.Minute)
	backgroundRounds(t, r.a)
	setRunLimit(t, r.a, "50")
	sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
	if step := countRows(t, r.a, `SELECT count(*) FROM incidents WHERE id=? AND step=?`, sos.IncidentID, triageAgentStep); step != 1 {
		t.Fatal("the SOS does not wait for the agent")
	}
	// Room for no ordinary delivery, with deliveries due long after the
	// test.
	if _, err := r.a.Store.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO deliveries(id,notification_key,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,state,created_at,next_attempt)
SELECT 'filler'||i,'filler:'||i,id,1,member_id,'email','x','subscribed_alert','info','s','m','queued',?,? FROM n, (SELECT id,member_id FROM contact_methods WHERE contact_method_type='email')`,
		maxPendingDeliveries-sosReservedPendingDeliveries-countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE state='sending' OR `+pendingDelivery), r.a.Now().Unix(), r.a.Now().Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	driveRunLimit(t, r.a)
	if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeLimitHolds {
		t.Fatalf("notice %q; want the limit to hold", state.notice)
	}
	if countRows(t, r.a, `SELECT count(*) FROM incidents WHERE id=? AND step=?`, sos.IncidentID, triageAgentStep) != 0 || countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation'`, sos.IncidentID) == 0 {
		t.Fatal("the full queue held up the SOS's wake")
	}
	driveRunLimit(t, r.a)
	if messages := runLimitMessages(t, r.a); len(messages) != 1 || !strings.HasPrefix(messages[0], "warn | Agent run limit reached.") || countRows(t, r.a, `SELECT count(*) FROM deliveries d JOIN alerts e ON e.id=d.alert_id WHERE e.actor=?`, runLimitActor) == 0 {
		t.Fatalf("messages about the limit: %q", messages)
	}
}

// A commit review takes the last run. The next visit finds the limit
// reached, and the commit the running review was given waits for its
// decision: no warning goes out about it, and the agent's approval is
// recorded as the one alert about the commit.
func TestRunLimitLeavesTheRunningCommitReviewItsCommits(t *testing.T) {
	parallelStartingProcesses(t)
	r := newCommitReviewRig(t)
	forgetEarlierRuns(t, r.a)
	setRunLimit(t, r.a, "1")
	commit := r.pushAndCheck(t, "playbooks/deploy.yml", "- hosts: app_servers\n  roles: [app]\n")
	r.rounds(t)
	run := r.nextCommitReview(t)
	r.rounds(t)
	if state := runLimitStateOf(t, r.a); state.notice != runLimitNoticeLimitHolds {
		t.Fatalf("notice %q; want the limit to hold", state.notice)
	}
	if count := alertsAbout(t, r.a, commit); count != 0 {
		t.Fatalf("%d alerts about the commit while the agent reviews it", count)
	}
	answer := reviewCommit(t, run, json.NewDecoder(run.connection), "00000000000000a1", commit, "approve", "", "Ordinary deploy change\n")
	if answer.Outcome != triageagent.ReportRecorded || alertsAbout(t, r.a, commit) != 1 {
		t.Fatalf("approve: %+v, %d alerts about the commit", answer, alertsAbout(t, r.a, commit))
	}
}
