package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// rigSoftwareReviewTask is the team's task in the rig's systems
// repository, scheduled_software_review_task.md.
const rigSoftwareReviewTask = "Check the servers' Debian packages with OSV.dev and report each known flaw.\n"

// settingsFilePrincipal sets what the settings file sets.
var settingsFilePrincipal = Principal{SettingsFile: "/etc/mmhotline/mmhotline_settings.yml"}

// newSoftwareReviewRig is the triage session rig with the scheduled
// software review every 24 hours and without the software review its
// fixture records, so a review is due at once.
func newSoftwareReviewRig(t *testing.T) *triageSessionRig {
	t.Helper()
	r := newTriageSessionRig(t, true)
	if _, err := r.a.Store.db.Exec(`DELETE FROM triage_agent_runs WHERE purpose='software_review'`); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SetSoftwareReviewInterval(context.Background(), settingsFilePrincipal, 24); err != nil {
		t.Fatal(err)
	}
	return r
}

// rounds lets the background loop do what is due now, and waits until a run
// it started has reached the sandbox or ended.
func (r *triageSessionRig) rounds(t *testing.T) {
	t.Helper()
	backgroundRounds(t, r.a)
	waitTriageIdleOrRunning(t, r.a)
}

const softwareReviewTask = "MMHotline started you for the scheduled software review; no alert started this run."

// nextReview returns the next run, which must be a software review in
// the systems repository.
func (r *triageSessionRig) nextReview(t *testing.T) fakeTriageRun {
	t.Helper()
	run := r.nextRun(t)
	if run.request.Action != "start" || run.request.SessionID != "" || run.request.SystemsRepositoryBaseCommitID == "" || !strings.Contains(run.request.Prompt, softwareReviewTask) {
		t.Fatalf("not a software review: action %q, session %q, base %q:\n%.400s", run.request.Action, run.request.SessionID, run.request.SystemsRepositoryBaseCommitID, run.request.Prompt)
	}
	return run
}

func lastFinishedReviewLine(at time.Time) string {
	return "The last software review that finished started at: " + at.UTC().Format("2006-01-02 15:04:05 UTC") + "."
}

// quotedTask is the team's task as the prompt quotes it.
func quotedTask(task string) string {
	return "----- start of scheduled_software_review_task.md -----\n" + task + "----- end of scheduled_software_review_task.md -----"
}

// The background loop starts a software review when none has run, and
// then one interval after the last one started, not a second sooner. Its
// prompt is MMHotline's frame around the team's task: who the agent is and
// what started it, the last review that finished, the rules file, the
// team's task quoted from scheduled_software_review_task.md, and the report
// command. MMHotline's own former task, OSV.dev's queries, is gone.
func TestSoftwareReviewStartsEachIntervalWithTheTeamsTask(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	firstStart := r.a.Now()
	r.rounds(t)
	first := r.nextReview(t)
	for _, want := range []string{
		"The last software review that finished started at: never.",
		"Your rules are in triage_agent_rules.md in this repository.",
		"Follow the team's instructions:\n\n" + quotedTask(rigSoftwareReviewTask),
		"mmhotline triage-agent report --alert-severity warn --report-file report.md",
		"Send the reports the team's task asks for.",
		"When you find nothing new, send no report",
	} {
		if !strings.Contains(first.request.Prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, first.request.Prompt)
		}
	}
	if strings.Contains(first.request.Prompt, "api.osv.dev") {
		t.Errorf("the prompt still holds MMHotline's own task:\n%s", first.request.Prompt)
	}
	if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='software_review' AND started_by='MMHotline' AND triage_session_id=''`); count != 1 {
		t.Fatalf("%d software review runs recorded", count)
	}
	r.end(t, first, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.rounds(t)
	r.noRunStarts(t)
	r.advance(24*time.Hour - time.Second)
	r.rounds(t)
	r.noRunStarts(t)
	r.advance(time.Second)
	r.rounds(t)
	second := r.nextReview(t)
	if want := lastFinishedReviewLine(firstStart); !strings.Contains(second.request.Prompt, want) {
		t.Fatalf("the second prompt lacks %q:\n%s", want, second.request.Prompt)
	}
	// A failed review counts, but the next one reads the advisories since
	// the last review that finished.
	r.end(t, second, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
	// No run has succeeded for 24 hours, so the sign-in check follows the
	// failed review; it does not count as a review. The rig's API key is
	// checked with the models list, which runs no coding agent.
	r.rounds(t)
	if state, _ := waitForSignInCheck(t, r.a, "sign_in_check"); state != "finished" {
		t.Fatalf("the sign-in check: %s", state)
	}
	r.rounds(t)
	r.noRunStarts(t)
	r.advance(24 * time.Hour)
	r.rounds(t)
	if third := r.nextReview(t); !strings.Contains(third.request.Prompt, lastFinishedReviewLine(firstStart)) {
		t.Fatalf("the third prompt does not start from the last review that finished:\n%s", third.request.Prompt)
	}
}

// The scheduled software review is off until the settings file sets an
// interval, and again once it sets off: the background loop starts no
// review, though none has ever run, however long time passes. show, its
// JSON, and the page say off. run-software-review still runs one review, so
// a team learns what a review costs before it turns the schedule on.
func TestScheduledSoftwareReviewOff(t *testing.T) {
	parallelStartingProcesses(t)
	for _, setOff := range []bool{false, true} {
		t.Run(map[bool]string{false: "never set", true: "set to off"}[setOff], func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			if _, err := r.a.Store.db.Exec(`DELETE FROM triage_agent_runs WHERE purpose='software_review'`); err != nil {
				t.Fatal(err)
			}
			if setOff {
				if err := r.a.SetSoftwareReviewInterval(context.Background(), settingsFilePrincipal, 24); err != nil {
					t.Fatal(err)
				}
				if err := r.a.SetSoftwareReviewInterval(context.Background(), settingsFilePrincipal, softwareReviewIntervalOff); err != nil {
					t.Fatal(err)
				}
			}
			r.rounds(t)
			r.noRunStarts(t)
			r.advance(30 * 24 * time.Hour)
			r.rounds(t)
			r.noRunStarts(t)
			if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='software_review'`); count != 0 {
				t.Fatalf("%d software reviews started while the scheduled review is off", count)
			}

			result, raw := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
			text := result.Text()
			for _, want := range []string{"Scheduled software review: off", "A software review runs only when an administrator runs sudo mmhotline triage-agent run-software-review."} {
				if !strings.Contains(text, want) {
					t.Errorf("show lacks %q:\n%s", want, text)
				}
			}
			if strings.Contains(text, "Next scheduled software review") {
				t.Errorf("show gives a next review while the scheduled review is off:\n%s", text)
			}
			var printed struct {
				TriageAgent map[string]any `json:"triage_agent"`
			}
			if err := json.Unmarshal([]byte(raw), &printed); err != nil {
				t.Fatal(err)
			}
			if got := printed.TriageAgent["software_review_interval_hours"]; got != "off" {
				t.Errorf("--json has software_review_interval_hours %v, want off", got)
			}
			if due, present := printed.TriageAgent["next_software_review_due_at"]; present {
				t.Errorf("--json has next_software_review_due_at %v while the scheduled review is off", due)
			}
			if page := triageAgentPageText(t, r); !strings.Contains(page, "Scheduled software review: off. The triage agent wakes only for alerts.") || strings.Contains(page, "Next scheduled software review") {
				t.Errorf("the page does not say the scheduled review is off:\n%s", page)
			}

			ran, _ := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "run-software-review"}, 200)
			review := r.nextReview(t)
			if ran.TriageAgent.LatestSoftwareReview == nil || ran.TriageAgent.LatestSoftwareReview.RunID != ran.TriageAgentRunID {
				t.Fatalf("run-software-review: %+v", ran)
			}
			// The cost recipe: once the run ends, show --json gives what the
			// one review cost.
			started := r.a.Now()
			r.advance(145 * time.Second)
			cost := 1.12
			r.end(t, review, triageagent.Event{Event: "finished", Answer: "Reported.", CodingAgentSessionTotalUsage: &triageagent.Usage{CostInUSDollars: &cost, InputTokens: 900000, CachedInputTokens: 800000, OutputTokens: 9000}})
			_, raw = commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
			var shown struct {
				TriageAgent struct {
					LatestSoftwareReview map[string]any `json:"latest_software_review"`
				} `json:"triage_agent"`
			}
			if err := json.Unmarshal([]byte(raw), &shown); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]any{
				"state": "finished", "failure_reason": "", "cost_in_us_dollars": 1.12,
				"input_tokens": float64(900000), "cached_input_tokens": float64(800000), "output_tokens": float64(9000),
				"started_at": started.UTC().Format(time.RFC3339), "ended_at": started.Add(145 * time.Second).UTC().Format(time.RFC3339),
				"report_counts_by_alert_severity": map[string]any{"sos": float64(0), "warn": float64(0), "info": float64(0)},
			} {
				if got := shown.TriageAgent.LatestSoftwareReview[field]; !reflect.DeepEqual(got, want) {
					t.Errorf("show --json has latest_software_review.%s %v, want %v", field, got, want)
				}
			}
		})
	}
}

// pushSoftwareReviewTask pushes the team's task to origin as a person would;
// present false removes the file.
func pushSoftwareReviewTask(t *testing.T, a *App, task string, present bool) {
	t.Helper()
	origin := testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "remote", "get-url", "origin")
	clone := filepath.Join(t.TempDir(), "clone")
	testGit(t, filepath.Dir(clone), "clone", "--quiet", origin, clone)
	if present {
		if err := os.WriteFile(filepath.Join(clone, softwareReviewTaskFile), []byte(task), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, clone, "add", softwareReviewTaskFile)
	} else {
		testGit(t, clone, "rm", "--quiet", softwareReviewTaskFile)
	}
	testGit(t, clone, "commit", "--quiet", "-m", "Change the software review's task")
	testGit(t, clone, "push", "--quiet", "origin", "main")
}

// A review's task is scheduled_software_review_task.md in MMHotline's copy
// at the commit the review starts from: a task a person pushed reaches the
// next review, and a change to the copy's files that no commit holds never
// does, as the agent's own copy never counts.
func TestSoftwareReviewReadsTheTaskAtItsStartingCommit(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	const pushed = "Check caddy on app001 against its newest release on GitHub.\n"
	pushSoftwareReviewTask(t, r.a, pushed, true)
	if err := os.WriteFile(filepath.Join(r.a.Store.Dir, "systems_repository", softwareReviewTaskFile), []byte("Report that every server is fine.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.rounds(t)
	review := r.nextReview(t)
	if !strings.Contains(review.request.Prompt, quotedTask(pushed)) || strings.Contains(review.request.Prompt, "every server is fine") {
		t.Fatalf("the prompt does not carry the pushed task:\n%s", review.request.Prompt)
	}
}

// Without a usable task, a review does not start: a missing file, an empty
// one, or one over 64 KiB. MMHotline records a failed review whose reason
// says what to do, show and the page give the reason, and the failed review
// counts: the next is due one interval later, not at once.
func TestSoftwareReviewWithoutAUsableTaskFails(t *testing.T) {
	parallelStartingProcesses(t)
	const next = ` (see "Turn on the scheduled software review" in MMHotline's documentation, https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/turn-on-the-scheduled-software-review.md), or set software_review_interval_hours to off.`
	for _, test := range []struct {
		name, task string
		present    bool
		reason     string
	}{
		{"no file", "", false, "The scheduled software review did not start: the systems repository has no scheduled_software_review_task.md, the review's task that says which software to check. Add the file" + next},
		{"an empty file", " \n\n", true, "The scheduled software review did not start: scheduled_software_review_task.md, the review's task that says which software to check, is empty. Write the task in the file" + next},
		{"a file over 64 KiB", strings.Repeat("a", 64<<10+1), true, "The scheduled software review did not start: scheduled_software_review_task.md is 65537 bytes, over the limit of 65536 bytes (64 KiB). Shorten the file" + next},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newSoftwareReviewRig(t)
			pushSoftwareReviewTask(t, r.a, test.task, test.present)
			started := r.a.Now()
			r.rounds(t)
			r.noRunStarts(t)
			if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='software_review' AND state='failed' AND failure_reason=?`, test.reason); count != 1 {
				var reason string
				r.a.Store.db.QueryRow(`SELECT failure_reason FROM triage_agent_runs WHERE purpose='software_review'`).Scan(&reason)
				t.Fatalf("no failed review with the reason; recorded %q", reason)
			}
			result, _ := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
			if !strings.Contains(result.Text(), "Software review failure reason: "+test.reason) {
				t.Errorf("show lacks the reason:\n%s", result.Text())
			}
			if due := result.TriageAgent.NextSoftwareReviewDueAt; due == nil || !due.Equal(started.Add(24*time.Hour)) {
				t.Errorf("next review due at %v, want %v", due, started.Add(24*time.Hour))
			}
			if page := triageAgentPageText(t, r); !strings.Contains(page, test.reason) {
				t.Errorf("the page lacks the reason")
			}
			r.advance(24*time.Hour - time.Second)
			r.rounds(t)
			if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='software_review'`); count != 1 {
				t.Fatalf("%d software reviews within the interval after the failed one", count)
			}
		})
	}
}

// Incidents come first while they need the agent: a software review that is
// due waits while a triage run is active, and starts while the open session
// waits for a person, here for an approval nobody decides, which keeps its
// SOS incident and so the session open. The decision's result resumes the
// session: MMHotline stops the review after the agent's current step, the
// session's run goes first, and the review starts again once that run ends,
// with the session still open.
func TestSoftwareReviewWaitsOnlyWhileATriageRunIsActive(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	prepareApprovals(t, r.a, "")
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	_, session, _ := approvalSession(t, r)
	r.rounds(t)
	r.noRunStarts(t)
	r.end(t, session, triageagent.Event{Event: "finished", Answer: "Asked for approval."})
	r.advance(time.Hour)
	r.rounds(t)
	review := r.nextReview(t)
	if open := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`); open != 1 {
		t.Fatal("the session ended, though its SOS incident is not resolved")
	}
	controls := json.NewDecoder(review.connection)
	if err := r.a.RefusePlaybookRun(context.Background(), r.p, onlyApprovalRecord(t, r.a).id, "Reboot after the nightly backup finishes."); err != nil {
		t.Fatal(err)
	}
	r.rounds(t)
	var control triageagent.Control
	review.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := controls.Decode(&control); err != nil || !control.Stop {
		t.Fatalf("the review was not stopped: %+v %v", control, err)
	}
	r.end(t, review, triageagent.Event{Event: "stopped", SessionID: "review-session"})
	if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='software_review' AND state='stopped' AND failure_reason=?`, softwareReviewStoppedForTriageSession); count != 1 {
		t.Fatal("the stopped review does not say why")
	}
	r.rounds(t)
	resume := r.nextRun(t)
	if resume.request.Action != "resume" || !strings.Contains(resume.request.Prompt, "Responder refused "+rebootPlaybook) {
		t.Fatalf("the session was not resumed with the refusal:\n%.400s", resume.request.Prompt)
	}
	r.end(t, resume, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.rounds(t)
	r.nextReview(t)
}

// A software review gives the one run to an alert's triage session:
// MMHotline stops the review after the agent's current step, the session's
// run starts, and the review starts again once that run ends, even when the
// stop came before the agent reported its session.
func TestSoftwareReviewStopsForATriageSession(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	r.rounds(t)
	review := r.nextReview(t)
	controls := json.NewDecoder(review.connection)
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.rounds(t)
	var control triageagent.Control
	review.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := controls.Decode(&control); err != nil || !control.Stop {
		t.Fatalf("the review was not stopped: %+v %v", control, err)
	}
	r.end(t, review, triageagent.Event{Event: "failed", Reason: "the coding agent run was stopped before the coding agent reported a session ID", StoppedBeforeSessionID: true})
	var state, reason string
	if err := r.a.Store.db.QueryRow(`SELECT state,failure_reason FROM triage_agent_runs WHERE purpose='software_review'`).Scan(&state, &reason); err != nil || state != "stopped" || reason != softwareReviewStoppedForTriageSession {
		t.Fatalf("the review ended %q: %q %v", state, reason, err)
	}
	r.rounds(t)
	session := r.nextRun(t)
	if session.request.Action != "start" || !strings.Contains(session.request.Prompt, "Disk of app002 is 97% full") {
		t.Fatalf("the session's run did not start:\n%.400s", session.request.Prompt)
	}
	r.end(t, session, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.rounds(t)
	r.nextReview(t)
}

// A review that fails on its own while MMHotline's stop for a session is on
// its way is a failed review, not a stopped one: it keeps its reason, and it
// counts, so the next review waits the interval.
func TestASoftwareReviewThatFailsBeforeTheStopForASessionFailed(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	r.rounds(t)
	review := r.nextReview(t)
	controls := json.NewDecoder(review.connection)
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.rounds(t)
	var control triageagent.Control
	review.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := controls.Decode(&control); err != nil || !control.Stop {
		t.Fatalf("the review was not stopped: %+v %v", control, err)
	}
	const reason = "The coding agent's service refused the request: 401 Unauthorized."
	r.end(t, review, triageagent.Event{Event: "failed", Reason: reason})
	var state, recorded string
	if err := r.a.Store.db.QueryRow(`SELECT state,failure_reason FROM triage_agent_runs WHERE purpose='software_review'`).Scan(&state, &recorded); err != nil || state != "failed" || recorded != reason {
		t.Fatalf("the review ended %q: %q %v", state, recorded, err)
	}
	r.rounds(t)
	r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: "Reported."})
	r.rounds(t)
	r.noRunStarts(t)
}

// The schedule lives in the database: after a restart, the next review is
// still due the team's interval after the last one started, and a review
// that a restart cut short starts again at once.
func TestSoftwareReviewScheduleSurvivesARestart(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("after a finished review", func(t *testing.T) {
		// The rig's fixture records a review that finished now.
		r := newTriageSessionRig(t, true)
		lastStart := r.a.Now()
		if err := r.a.SetSoftwareReviewInterval(context.Background(), r.p, 6); err != nil {
			t.Fatal(err)
		}
		r.advance(time.Hour)
		r.a = restartApp(t, r.a)
		r.runs = triageRunFixture(t, r.a)
		r.rounds(t)
		r.noRunStarts(t)
		r.advance(lastStart.Add(6*time.Hour - time.Second).Sub(r.a.Now()))
		r.rounds(t)
		r.noRunStarts(t)
		r.advance(time.Second)
		r.rounds(t)
		r.nextReview(t)
	})
	for _, restart := range restartsDuringARun {
		t.Run("a review cut short when "+restart.name, func(t *testing.T) {
			r := newSoftwareReviewRig(t)
			r.rounds(t)
			r.nextReview(t)
			r.a = restart.stop(t, r.a)
			r.runs = triageRunFixture(t, r.a)
			r.rounds(t)
			r.nextReview(t)
			if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='software_review' AND state='failed' AND failure_reason=?`, restart.reason); count != 1 {
				t.Fatalf("%d reviews recorded as cut short", count)
			}
		})
	}
}

// The team sets the interval, and the background loop takes a change at
// once: with the last review started 2 hours ago, a change from 24 hours to
// 1 starts a review at the next visit, which the change itself brings. The
// next is then due an hour after that one started, not a second sooner.
// show gives the interval, who set it, and when the next review is due. Only an administrator, or root through the
// command, changes it.
func TestAShorterSoftwareReviewIntervalStartsAReviewAtOnce(t *testing.T) {
	parallelStartingProcesses(t)
	// The rig's fixture records a review that finished now.
	r := newTriageSessionRig(t, true)
	lastStart := r.a.Now()
	if err := r.a.SetSoftwareReviewInterval(context.Background(), settingsFilePrincipal, 24); err != nil {
		t.Fatal(err)
	}
	before, _ := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
	if view := before.TriageAgent; view.SoftwareReviewIntervalHours != 24 || view.NextSoftwareReviewDueAt == nil || !view.NextSoftwareReviewDueAt.Equal(lastStart.Add(24*time.Hour)) {
		t.Fatalf("before a change: %d hours, next due %v", view.SoftwareReviewIntervalHours, view.NextSoftwareReviewDueAt)
	}
	if text := before.Text(); !strings.Contains(text, "Scheduled software review: every 24 hours; set by the settings file /etc/mmhotline/mmhotline_settings.yml at ") {
		t.Fatalf("show lacks the interval:\n%s", text)
	}
	r.advance(2 * time.Hour)
	r.rounds(t)
	r.noRunStarts(t)
	member, _ := addMember(t, r.a, "Member", RoleMember)
	if err := r.a.SetSoftwareReviewInterval(context.Background(), member, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member changed the interval: %v", err)
	}
	changedAt := r.a.Now()
	if err := r.a.SetSoftwareReviewInterval(context.Background(), settingsFilePrincipal, 1); err != nil {
		t.Fatal(err)
	}
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
	for _, want := range []string{
		"Scheduled software review: every hour; set by the settings file /etc/mmhotline/mmhotline_settings.yml at " + changedAt.UTC().Format(time.RFC3339) + ".",
		"Next scheduled software review due at: " + lastStart.Add(time.Hour).UTC().Format(time.RFC3339),
	} {
		if !strings.Contains(result.Text(), want) {
			t.Errorf("the command lacks %q:\n%s", want, result.Text())
		}
	}
	var printed struct {
		TriageAgent map[string]any `json:"triage_agent"`
	}
	if err := json.Unmarshal([]byte(raw), &printed); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"software_review_interval_hours":      float64(1),
		"software_review_interval_changed_at": changedAt.UTC().Format(time.RFC3339),
		"software_review_interval_changed_by": "the settings file /etc/mmhotline/mmhotline_settings.yml",
		"next_software_review_due_at":         lastStart.Add(time.Hour).UTC().Format(time.RFC3339),
	} {
		if got := printed.TriageAgent[field]; got != want {
			t.Errorf("--json has %s %v, want %v", field, got, want)
		}
	}
	r.rounds(t)
	review := r.nextReview(t)
	reviewStart := r.a.Now()
	r.end(t, review, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.advance(reviewStart.Add(time.Hour - time.Second).Sub(r.a.Now()))
	r.rounds(t)
	r.noRunStarts(t)
	r.advance(time.Second)
	r.rounds(t)
	r.nextReview(t)
}

// reportAlert reads the alert a report became, by its title.
func reportAlert(t *testing.T, a *App, title string) Alert {
	t.Helper()
	alert, err := scanAlert(a.Store.db.QueryRow(`SELECT `+alertColumns+` FROM alerts WHERE title=?`, title))
	if err != nil {
		t.Fatalf("no alert %q: %v", title, err)
	}
	return alert
}

// A software review has no triage session, so each of its reports is
// an alert of its own at its alert severity. At sos it opens an SOS incident
// that wakes the person on call at once, without waiting for the agent; no
// report starts a session.
func TestSoftwareReviewReportsBecomeAlerts(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	r.rounds(t)
	run := r.nextReview(t)
	answers := json.NewDecoder(run.connection)
	reports := []struct{ severity, text, title string }{
		{"sos", "# Caddy on app001 has an exploited flaw\n\nCVE-2026-0001 is in CISA's list. Installed 2.10.0, fixed in 2.10.1.\n", "The AI triage agent reports: Caddy on app001 has an exploited flaw"},
		{"warn", "# OpenSSL on db001 has a fixed flaw\n\nDSA-6000-1. Installed 3.5.1-1, fixed in 3.5.1-2.\n", "The AI triage agent reports: OpenSSL on db001 has a fixed flaw"},
		{"info", "# cloudflared 2026.10.1 is out\n\nNo security fix.\n", "The AI triage agent reports: cloudflared 2026.10.1 is out"},
	}
	for index, sent := range reports {
		if answer := report(t, run, answers, fmt.Sprintf("%016x", index+1), sent.severity, sent.text); answer.Outcome != triageagent.ReportRecorded {
			t.Fatalf("%s report: %+v", sent.severity, answer)
		}
	}
	for _, sent := range reports {
		alert := reportAlert(t, r.a, sent.title)
		if alert.Severity != sent.severity || alert.SourceID != "system" || (alert.IncidentID != "") != (sent.severity == "sos") {
			t.Fatalf("the %s report became %+v", sent.severity, alert)
		}
	}
	sos := reportAlert(t, r.a, reports[0].title)
	incident, err := r.a.GetIncident(context.Background(), r.p, sos.IncidentID)
	if err != nil || incident.Status != "open" || incident.Step == triageAgentStep {
		t.Fatalf("the SOS report's incident: %+v %v", incident, err)
	}
	if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND contact_method_type='test_waking' AND state='queued'`, sos.IncidentID); count == 0 {
		t.Fatal("the SOS report's incident does not wake the person on call at once")
	}
	if count := countRows(t, r.a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='triage_agent_report' AND message=?`, sos.IncidentID, reports[0].text); count != 1 {
		t.Fatal("the SOS report's incident does not hold the report")
	}
	warn := reportAlert(t, r.a, reports[1].title)
	if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE alert_id=? AND contact_method_type='email' AND severity='warn'`, warn.ID); count != 1 {
		t.Fatal("the warn report did not reach the contact method subscribed to warn")
	}
	if sessions, joined := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`), countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts`); sessions != 0 || joined != 0 {
		t.Fatalf("the reports started %d sessions and joined %d alerts", sessions, joined)
	}
}

// sudo mmhotline triage-agent run-software-review starts a review at
// once, though the last one started within the interval, and refuses while
// a run is active, with the reason; an open triage session whose run ended
// does not stop it. A review started by hand counts: the next is due one
// interval after it.
func TestRunSoftwareReviewCommand(t *testing.T) {
	parallelStartingProcesses(t)
	// The rig's fixture records a review that finished now.
	r := newTriageSessionRig(t, true)
	if err := r.a.SetSoftwareReviewInterval(context.Background(), settingsFilePrincipal, 24); err != nil {
		t.Fatal(err)
	}
	command := func(status int) (CommandResult, string) {
		t.Helper()
		return commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "run-software-review"}, status)
	}
	r.advance(2 * time.Hour)
	result, _ := command(200)
	review := r.nextReview(t)
	latest := result.TriageAgent.LatestSoftwareReview
	if result.TriageAgentRunID == "" || latest == nil || latest.RunID != result.TriageAgentRunID || latest.State != "running" || latest.StartedBy != "the mmhotline command, run by alice" {
		t.Fatalf("result: run %q, latest review %+v", result.TriageAgentRunID, latest)
	}
	if _, raw := command(409); !strings.Contains(raw, "A triage agent run is already active.") {
		t.Fatalf("while the review runs: %s", raw)
	}
	r.end(t, review, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.advance(22 * time.Hour)
	r.rounds(t)
	r.noRunStarts(t)
	r.advance(2 * time.Hour)
	r.rounds(t)
	r.end(t, r.nextReview(t), triageagent.Event{Event: "finished", Answer: "Reported."})
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.drive(t)
	session := r.nextRun(t)
	if _, raw := command(409); !strings.Contains(raw, "A triage agent run is already active.") {
		t.Fatalf("while the session's run is active: %s", raw)
	}
	r.end(t, session, triageagent.Event{Event: "finished", Answer: "Reported."})
	command(200)
	r.nextReview(t)
}

// The triage agent page shows the latest software review: when it
// ran, its outcome, its token use with the cached input, and how many reports
// it sent at each alert severity.
func TestTriageAgentPageShowsTheLatestSoftwareReview(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	page := func() string {
		t.Helper()
		request := httptest.NewRequest("GET", "https://mmhotline.example.com/triage-agent", nil)
		addAccessToken(request, r.p.SignInEmailAddress)
		response := httptest.NewRecorder()
		r.a.Handler().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("triage agent page: %d", response.Code)
		}
		return html.UnescapeString(response.Body.String())
	}
	if !strings.Contains(page(), "No software review has run yet.") {
		t.Fatal("the page does not say that no review has run")
	}
	started := r.a.Now()
	r.rounds(t)
	run := r.nextReview(t)
	answers := json.NewDecoder(run.connection)
	for index, severity := range []string{"sos", "warn", "warn", "info"} {
		report(t, run, answers, fmt.Sprintf("%016x", index+1), severity, fmt.Sprintf("# Finding %d\n", index+1))
	}
	// A refused report reached nobody, so it does not count.
	if answer := report(t, run, answers, "0000000000000009", "warn", " \n"); answer.Outcome != triageagent.Refused {
		t.Fatalf("answer: %+v", answer)
	}
	r.advance(10 * time.Minute)
	// The rig's coding agent is Codex, which reports tokens and no cost.
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported.", CodingAgentSessionTotalUsage: &triageagent.Usage{InputTokens: 26658, CachedInputTokens: 13207, OutputTokens: 145}})
	const usage = "Input tokens: 26658, of which 13207 cached; output tokens: 145."
	shown := page()
	for _, want := range []string{
		"Latest software review: Finished",
		"Started by MMHotline at " + pageClock(started, r.a.Now(), time.UTC) + ", ended at " + pageClock(started.Add(10*time.Minute), r.a.Now(), time.UTC),
		"Reports: 1 SOS, 2 Warning, 1 Info.",
		usage,
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
	for _, want := range []string{"Software review reports: 1 at sos, 2 at warn, 1 at info.", "Software review usage: " + usage} {
		if !strings.Contains(result.Text(), want) {
			t.Errorf("the command lacks %q:\n%s", want, result.Text())
		}
	}
	var printed struct {
		TriageAgent struct {
			LatestSoftwareReview map[string]any `json:"latest_software_review"`
		} `json:"triage_agent"`
	}
	if err := json.Unmarshal([]byte(raw), &printed); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]float64{"input_tokens": 26658, "cached_input_tokens": 13207, "output_tokens": 145} {
		if got := printed.TriageAgent.LatestSoftwareReview[field]; got != want {
			t.Errorf("show --json has latest_software_review.%s %v, want %v", field, got, want)
		}
	}
}
