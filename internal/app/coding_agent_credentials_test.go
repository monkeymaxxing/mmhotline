package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// nextSignInCheck returns the next run, which must be a sign-in check: the
// setup test's prompt, outside the systems repository, recorded as a check.
func (r *triageSessionRig) nextSignInCheck(t *testing.T, prompt string) fakeTriageRun {
	t.Helper()
	run := r.nextRun(t)
	if run.request.Action != "start" || run.request.Prompt != prompt || run.request.SystemsRepositoryBaseCommitID != "" {
		t.Fatalf("not a sign-in check: action %q, base %q:\n%.300s", run.request.Action, run.request.SystemsRepositoryBaseCommitID, run.request.Prompt)
	}
	if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose IN ('sign_in_check','review_agent_sign_in_check') AND state='running'`); count != 1 {
		t.Fatalf("%d sign-in checks running", count)
	}
	return run
}

// notices counts the alerts from MMHotline at severity titled title.
func notices(t *testing.T, a *App, severity, title string) int {
	t.Helper()
	return countRows(t, a, `SELECT count(*) FROM alerts WHERE source_id='system' AND severity=? AND title=?`, severity, title)
}

// noticeMessage is the message of the latest alert from MMHotline titled title.
func noticeMessage(t *testing.T, a *App, title string) string {
	t.Helper()
	var message string
	if err := a.Store.db.QueryRow(`SELECT message FROM alerts WHERE source_id='system' AND title=? ORDER BY sequence DESC LIMIT 1`, title).Scan(&message); err != nil {
		t.Fatalf("no alert %q: %v", title, err)
	}
	return message
}

// useCodexAccessToken gives the rig's triage agent a Codex access token
// far from its end. Its sign-in check runs the setup test's prompt in the
// sandbox, as the tests of the prompt check need; an API key's check asks
// the models list instead (coding_agent_models_list_test.go).
func useCodexAccessToken(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.Store.db.Exec(`UPDATE triage_agent_setup SET credential_type='access_token',credential_expires_on='2099-12-31'`); err != nil {
		t.Fatal(err)
	}
}

// setUpWithSubscriptionToken sets up role with Claude Code and a
// subscription token through its setup test, which the fake sandbox passes,
// and returns when the setup was saved.
func (r *triageSessionRig) setUpWithSubscriptionToken(t *testing.T, role agentRole) time.Time {
	t.Helper()
	start, sentence := r.a.StartTriageAgentSetupTest, triageAgentTestSentence
	if role.name == reviewAgentRole.name {
		start, sentence = r.a.StartReviewAgentSetupTest, reviewAgentTestSentence
	}
	// Each set-up is a new token: a token's year counts from when it applied.
	if _, err := start(context.Background(), r.p, "claude_code", "subscription_token", "sk-ant-oat01-fixture-"+NewID()); err != nil {
		t.Fatal(err)
	}
	setUpAt := r.a.Now().UTC()
	r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: sentence})
	return setUpAt
}

// A Claude Code subscription token lasts a year from the set-up. MMHotline
// warns the administrators 30 days and 7 days before, each once, at the
// background loop's deadline: not a second early, never again after a
// restart, and at the first visit after a reminder time MMHotline was
// stopped through. Other members hear nothing.
func TestSubscriptionTokenExpiryRemindersAt30And7Days(t *testing.T) {
	parallelStartingProcesses(t)
	for _, role := range agentRoles {
		t.Run(role.name, func(t *testing.T) {
			r := newTriageSessionRig(t, false)
			_, member := addMember(t, r.a, "Member", RoleMember)
			memberEmail := addEmailContactMethod(t, r.a, member, "warn")
			setUpAt := r.setUpWithSubscriptionToken(t, role)
			expiresAt := setUpAt.Add(365 * 24 * time.Hour)
			// Without the sandbox no sign-in check runs; the reminders need none.
			r.a = restartApp(t, r.a)
			title := "The " + role.name + "'s Claude Code token expires on " + expiresAt.Format("2006-01-02")
			reminders := func() int { t.Helper(); r.rounds(t); return notices(t, r.a, "warn", title) }
			if got := reminders(); got != 0 {
				t.Fatalf("%d reminders right after the set-up", got)
			}
			r.advance(expiresAt.Add(-30*24*time.Hour - time.Second).Sub(r.a.Now()))
			if got := reminders(); got != 0 {
				t.Fatalf("%d reminders a second before 30 days remain", got)
			}
			r.advance(time.Second)
			if got := reminders(); got != 1 {
				t.Fatalf("%d reminders when 30 days remain", got)
			}
			message := noticeMessage(t, r.a, title)
			for _, want := range []string{
				"MMHotline counts the year from when the settings file's token applied, at " + displayTime(setUpAt) + ", so the token stops working at " + displayTime(expiresAt),
				"   claude setup-token",
				"2. Put the new credential in " + role.settingsKey + ".credential in the settings file, /etc/mmhotline/mmhotline_settings.yml",
				"   sudo mmhotline settings apply",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("the reminder lacks %q:\n%s", want, message)
				}
			}
			alert := reportAlert(t, r.a, title)
			if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE alert_id=? AND contact_method_id=?`, alert.ID, memberEmail); count != 0 {
				t.Fatal("a member who is not an administrator got the reminder")
			}
			if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE alert_id=? AND contact_method_type='email' AND contact_method_id!=?`, alert.ID, memberEmail); count != 1 {
				t.Fatal("the administrator did not get the reminder")
			}
			r.a = restartApp(t, r.a)
			if got := reminders(); got != 1 {
				t.Fatalf("%d reminders after a restart", got)
			}
			// MMHotline is stopped from an hour before the 7-day reminder
			// to an hour after it.
			r.advance(expiresAt.Add(-7*24*time.Hour - time.Hour).Sub(r.a.Now()))
			if got := reminders(); got != 1 {
				t.Fatalf("%d reminders before 7 days remain", got)
			}
			r.a = restartApp(t, r.a)
			r.advance(2 * time.Hour)
			if got := reminders(); got != 2 {
				t.Fatalf("%d reminders after a restart past 7 days before", got)
			}
			r.a = restartApp(t, r.a)
			r.advance(60 * 24 * time.Hour)
			if got := reminders(); got != 2 {
				t.Fatalf("%d reminders after the token expired", got)
			}
		})
	}
}

// addEmailContactMethod gives member an email contact method subscribed to
// severity and returns its ID.
func addEmailContactMethod(t *testing.T, a *App, member Member, severity string) string {
	t.Helper()
	id := NewID()
	if _, err := a.Store.db.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) VALUES(?,?,'email','{}',?,1,1,0)`, id, member.ID, member.SignInEmailAddress); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.db.Exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,?)`, id, severity); err != nil {
		t.Fatal(err)
	}
	return id
}

// A new set-up starts a new year: the reminders count from it, and the old
// token's reminders that were not sent never come.
func TestANewSetUpRestartsTheExpiryReminders(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, false)
	firstSetUpAt := r.setUpWithSubscriptionToken(t, triageAgentRole)
	firstTitle := "The triage agent's Claude Code token expires on " + firstSetUpAt.Add(365*24*time.Hour).Format("2006-01-02")
	r.advance(335 * 24 * time.Hour)
	r.rounds(t)
	// No run succeeded for a day, so the sign-in check runs too.
	r.end(t, r.nextSignInCheck(t, triageAgentSetupTestPrompt), triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	if got := notices(t, r.a, "warn", firstTitle); got != 1 {
		t.Fatalf("%d reminders of the first token", got)
	}
	r.advance(time.Hour)
	secondSetUpAt := r.setUpWithSubscriptionToken(t, triageAgentRole)
	secondExpiresAt := secondSetUpAt.Add(365 * 24 * time.Hour)
	result, _ := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
	if setup := result.TriageAgent.Setup; setup.CredentialExpiresAt == nil || !setup.CredentialExpiresAt.Equal(secondExpiresAt) {
		t.Fatalf("the new token expires at %v", setup.CredentialExpiresAt)
	}
	secondTitle := "The triage agent's Claude Code token expires on " + secondExpiresAt.Format("2006-01-02")
	// Without the sandbox no sign-in check runs. Past the first token's
	// 7-day reminder, up to a second before the new token's 30-day reminder.
	r.a = restartApp(t, r.a)
	r.advance(secondExpiresAt.Add(-30*24*time.Hour - time.Second).Sub(r.a.Now()))
	r.rounds(t)
	if first, second := notices(t, r.a, "warn", firstTitle), notices(t, r.a, "warn", secondTitle); first != 1 || second != 0 {
		t.Fatalf("before the new token's reminder: %d of the first token's, %d of the new one's", first, second)
	}
	r.advance(time.Second)
	r.rounds(t)
	if got := notices(t, r.a, "warn", secondTitle); got != 1 {
		t.Fatalf("%d reminders of the new token when 30 days remain", got)
	}
}

// Once a day MMHotline checks the sign-in, unless a run of the agent
// succeeded in the last 24 hours: the next check is due a day after the
// latest run that succeeded. A run that failed does not count.
func TestSignInCheckSkippedAfterARunSucceededWithin24Hours(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	useCodexAccessToken(t, r.a)
	// The rig's fixture records a software review that finished now; the
	// next would be due in a year.
	if err := r.a.SetSoftwareReviewInterval(context.Background(), r.p, 8760); err != nil {
		t.Fatal(err)
	}
	start := r.a.Now()
	r.advance(20 * time.Hour)
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.rounds(t)
	r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: "Reported."})
	sessionRunEnded := r.a.Now()
	r.advance(start.Add(24 * time.Hour).Sub(r.a.Now()))
	r.rounds(t)
	r.noRunStarts(t)
	// A session's run that fails proves nothing about the sign-in.
	r.advance(6 * time.Hour)
	r.alert(t, "warn", "Disk of app002 is 98% full", "")
	r.rounds(t)
	r.end(t, r.nextRun(t), triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
	r.advance(sessionRunEnded.Add(24*time.Hour - time.Second).Sub(r.a.Now()))
	r.rounds(t)
	r.noRunStarts(t)
	if open := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`); open != 0 {
		t.Fatal("the session did not end")
	}
	r.advance(time.Second)
	r.rounds(t)
	check := r.nextSignInCheck(t, triageAgentSetupTestPrompt)
	if check.request.AgentRole != triageagent.TriageAgentRole || check.request.CodingAgent != "codex" || check.request.CredentialType != "access_token" || check.request.Credential != "sk-test-credential" {
		t.Fatalf("the check's request: %+v", check.request)
	}
}

// When a sign-in check fails, the administrators get one warn alert that
// names the agent and the reason, and the page and show say so; while it
// keeps failing, nothing more. When the sign-in works again, one info alert.
// A failure after that is told again. The review agent's check runs as a
// review runs, after the triage agent's.
func TestSignInCheckFailsThenWorksAgainWithOneAlertEach(t *testing.T) {
	parallelStartingProcesses(t)
	for _, role := range agentRoles {
		t.Run(role.name, func(t *testing.T) {
			r := newTriageSessionRig(t, false)
			useCodexAccessToken(t, r.a)
			prompt, sentence, agentRole, codingAgent := triageAgentSetupTestPrompt, triageAgentTestSentence, triageagent.TriageAgentRole, "Codex"
			if role.name == reviewAgentRole.name {
				prompt, sentence, agentRole, codingAgent = reviewAgentSetupTestPrompt, reviewAgentTestSentence, triageagent.ReviewAgentRole, "Claude Code"
				if _, err := r.a.StartReviewAgentSetupTest(context.Background(), r.p, "claude_code", "subscription_token", "sk-ant-oat01-review"); err != nil {
					t.Fatal(err)
				}
				r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: sentence})
			}
			// daily moves a day on and answers the day's checks: the triage
			// agent's comes first, and passes while the review agent's is tested.
			daily := func(event triageagent.Event) {
				t.Helper()
				r.advance(24 * time.Hour)
				r.rounds(t)
				if role.name == reviewAgentRole.name {
					r.end(t, r.nextSignInCheck(t, triageAgentSetupTestPrompt), triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
					r.rounds(t)
				}
				check := r.nextSignInCheck(t, prompt)
				if check.request.AgentRole != agentRole {
					t.Fatalf("the check runs as %q", check.request.AgentRole)
				}
				r.end(t, check, event)
				r.rounds(t)
				r.noRunStarts(t)
			}
			failing := "The " + role.name + " cannot sign in to " + codingAgent
			again := "The " + role.name + " can sign in to " + codingAgent + " again"
			// Without the word key: the run removes the rig's credential, key.
			const reason = "401 Unauthorized: invalid bearer token"
			daily(triageagent.Event{Event: "failed", Reason: reason})
			failedAt := r.a.Now().UTC()
			if got := notices(t, r.a, "warn", failing); got != 1 {
				t.Fatalf("%d warn alerts after a failed check", got)
			}
			message := noticeMessage(t, r.a, failing)
			for _, want := range []string{"MMHotline's sign-in check of the " + role.name + " failed at " + displayTime(failedAt) + ": " + reason, role.whileSignInFails, "   sudo mmhotline " + role.commandGroup + " check-sign-in"} {
				if !strings.Contains(message, want) {
					t.Errorf("the alert lacks %q:\n%s", want, message)
				}
			}
			show := func() *TriageAgentSetup {
				t.Helper()
				result, raw := commandRequest(t, r.a, CommandRequest{Group: role.commandGroup, Action: "show"}, 200)
				var printed map[string]any
				if err := json.Unmarshal([]byte(raw), &printed); err != nil {
					t.Fatal(err)
				}
				group := strings.ReplaceAll(role.commandGroup, "-", "_")
				view, _ := printed[group].(map[string]any)
				setup, _ := view["setup"].(map[string]any)
				for _, field := range []string{"sign_in_last_checked_at", "sign_in_works", "sign_in_failure_reason"} {
					if _, ok := setup[field]; !ok {
						t.Errorf("show --json lacks %s.setup.%s", group, field)
					}
				}
				if role.name == reviewAgentRole.name {
					return result.ReviewAgent.Setup
				}
				return result.TriageAgent.Setup
			}
			if setup := show(); setup.SignInWorks || setup.SignInFailureReason != reason || !setup.SignInLastCheckedAt.Equal(failedAt) {
				t.Fatalf("show after the failed check: works %t, reason %q, checked at %v", setup.SignInWorks, setup.SignInFailureReason, setup.SignInLastCheckedAt)
			}
			daily(triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
			if got := notices(t, r.a, "warn", failing); got != 1 {
				t.Fatalf("%d warn alerts while the check keeps failing", got)
			}
			// An administrator checks at once after a fix.
			result, _ := commandRequest(t, r.a, CommandRequest{Group: role.commandGroup, Action: "check-sign-in"}, 200)
			check := r.nextSignInCheck(t, prompt)
			if runID := result.TriageAgentRunID + result.ReviewAgentRunID; runID == "" {
				t.Fatal("check-sign-in returned no run ID")
			}
			r.end(t, check, triageagent.Event{Event: "finished", Answer: sentence + "."})
			if got := notices(t, r.a, "info", again); got != 1 {
				t.Fatalf("%d info alerts after the check passed", got)
			}
			if setup := show(); !setup.SignInWorks || setup.SignInFailureReason != "" {
				t.Fatalf("show after the passing check: works %t, reason %q", setup.SignInWorks, setup.SignInFailureReason)
			}
			daily(triageagent.Event{Event: "finished", Answer: sentence})
			if failingCount, againCount := notices(t, r.a, "warn", failing), notices(t, r.a, "info", again); failingCount != 1 || againCount != 1 {
				t.Fatalf("after another passing check: %d warn, %d info", failingCount, againCount)
			}
			// An answer without the test sentence fails the check too.
			daily(triageagent.Event{Event: "finished", Answer: "Please run /login."})
			if got := notices(t, r.a, "warn", failing); got != 2 {
				t.Fatalf("%d warn alerts after a failure that follows a recovery", got)
			}
		})
	}
}

// A sign-in check gives way to a triage session's run: a due check waits
// while a run is active, and runs while the open session waits for nothing,
// before its 30 quiet minutes have passed. An alert that joins the session
// during a check stops it after the agent's current step, and the check
// runs again once the session's run ends. A stopped check is no failure. An
// administrator's check is refused while a run is active.
func TestSignInCheckGivesWayToATriageSession(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	useCodexAccessToken(t, r.a)
	if err := r.a.SetSoftwareReviewInterval(context.Background(), r.p, 8760); err != nil {
		t.Fatal(err)
	}
	r.advance(24 * time.Hour)
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.rounds(t)
	if session := r.nextRun(t); !strings.Contains(session.request.Prompt, "Disk of app002 is 97% full") {
		t.Fatalf("the session's run must go first:\n%.300s", session.request.Prompt)
	} else {
		r.end(t, session, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
	}
	r.rounds(t)
	check := r.nextSignInCheck(t, triageAgentSetupTestPrompt)
	if open := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`); open != 1 {
		t.Fatal("the session ended before its 30 quiet minutes")
	}
	if _, raw := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "check-sign-in"}, 409); !strings.Contains(raw, "A triage agent run is already active.") {
		t.Fatalf("check-sign-in while a check runs: %s", raw)
	}
	controls := json.NewDecoder(check.connection)
	r.alert(t, "warn", "Disk of app002 is 98% full", "")
	r.rounds(t)
	var control triageagent.Control
	check.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := controls.Decode(&control); err != nil || !control.Stop {
		t.Fatalf("the check was not stopped: %+v %v", control, err)
	}
	r.end(t, check, triageagent.Event{Event: "stopped", SessionID: "check-session"})
	if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='sign_in_check' AND state='stopped' AND failure_reason=?`, signInCheckStoppedForTriageSession); count != 1 {
		t.Fatal("the stopped check does not say why")
	}
	r.rounds(t)
	session := r.nextRun(t)
	if !strings.Contains(session.request.Prompt, "Disk of app002 is 98% full") {
		t.Fatalf("the session's run did not start:\n%.300s", session.request.Prompt)
	}
	r.end(t, session, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
	r.rounds(t)
	r.end(t, r.nextSignInCheck(t, triageAgentSetupTestPrompt), triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	if count := countRows(t, r.a, `SELECT count(*) FROM alerts WHERE source_id='system' AND title LIKE '%sign in%'`); count != 0 {
		t.Fatalf("%d sign-in alerts, though no check failed", count)
	}
}

// A sign-in check that fails on its own while MMHotline's stop for a session
// is on its way is a sign-in failure: administrators hear it at once, not
// after the next check.
func TestASignInCheckThatFailsBeforeTheStopForASessionIsASignInFailure(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	useCodexAccessToken(t, r.a)
	if err := r.a.SetSoftwareReviewInterval(context.Background(), r.p, 8760); err != nil {
		t.Fatal(err)
	}
	r.advance(24 * time.Hour)
	r.rounds(t)
	check := r.nextSignInCheck(t, triageAgentSetupTestPrompt)
	controls := json.NewDecoder(check.connection)
	r.alert(t, "warn", "Disk of app002 is 98% full", "")
	r.rounds(t)
	var control triageagent.Control
	check.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := controls.Decode(&control); err != nil || !control.Stop {
		t.Fatalf("the check was not stopped: %+v %v", control, err)
	}
	const reason = "401 Unauthorized: invalid bearer token"
	r.end(t, check, triageagent.Event{Event: "failed", Reason: reason})
	if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='sign_in_check' AND state='failed' AND failure_reason=?`, reason); count != 1 {
		t.Fatal("the failed check is not recorded as failed with its reason")
	}
	if got := notices(t, r.a, "warn", "The triage agent cannot sign in to Codex"); got != 1 {
		t.Fatalf("%d warn alerts after the check failed", got)
	}
}

// A lock-out that cuts a sign-in check short shows nothing about the
// sign-in: the check ends as stopped with the lock-out's reason, no warning
// goes to administrators, the sign-in still counts as working, and the check
// runs again once the lock-out file is gone.
func TestASignInCheckThatALockOutCutsIsNoSignInFailure(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	useCodexAccessToken(t, r.a)
	if err := r.a.SetSoftwareReviewInterval(context.Background(), r.p, 8760); err != nil {
		t.Fatal(err)
	}
	lockOut := lockOutFileForTest(t, r.a)
	r.advance(24 * time.Hour)
	r.rounds(t)
	r.nextSignInCheck(t, triageAgentSetupTestPrompt)
	lockEveryoneOut(t, lockOut)
	r.a.everyoneLockedOut() // the lock-out watch's look, every second
	waitNoTriageRun(t, r.a)
	if count := countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE purpose='sign_in_check' AND state='stopped' AND failure_reason=?`, everyoneLockedOutCannotRun); count != 1 {
		t.Fatal("the cut check is not recorded as stopped by the lock-out")
	}
	if got := notices(t, r.a, "warn", "The triage agent cannot sign in to Codex"); got != 0 {
		t.Fatalf("%d warn alerts about the sign-in after a lock-out cut the check", got)
	}
	result, _ := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
	if setup := result.TriageAgent.Setup; setup == nil || !setup.SignInWorks || setup.SignInFailureReason != "" {
		t.Fatalf("the sign-in after the cut check: %+v", setup)
	}
	if err := os.Remove(lockOut); err != nil {
		t.Fatal(err)
	}
	r.rounds(t)
	r.end(t, r.nextSignInCheck(t, triageAgentSetupTestPrompt), triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	if count := countRows(t, r.a, `SELECT count(*) FROM alerts WHERE source_id='system' AND title LIKE '%sign in%'`); count != 0 {
		t.Fatalf("%d sign-in alerts, though no check failed", count)
	}
}

// The triage agent page shows each agent's sign-in, a subscription token's
// expiry, and "Check the sign-in now", which only an administrator presses;
// the page refreshes while a check runs.
func TestTriageAgentPageShowsTheSignIn(t *testing.T) {
	t.Parallel()
	h := newWorkflowRig(t)
	admin := h.browser()
	admin.addPerson("admin@example.test", "Administrator")
	runs := triageRunFixture(t, h.app)
	finish := func(run fakeTriageRun, event triageagent.Event) {
		t.Helper()
		sendTriageEvent(t, run, event)
		waitNoTriageRun(t, h.app)
	}
	administrator := Principal{SignInEmailAddress: "admin@example.test"}
	if _, err := h.app.StartTriageAgentSetupTest(context.Background(), administrator, "claude_code", "subscription_token", "sk-ant-oat01-fixture"); err != nil {
		t.Fatal(err)
	}
	finish(<-runs, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	setUpAt := h.app.Now().UTC()
	if _, err := h.app.startCodingAgentSetupTest(context.Background(), reviewAgentRole, SettingsCodingAgent{CodingAgent: "codex", CredentialType: "access_token", Credential: "at-review", CredentialExpiresOn: "2027-02-06", Model: testAgentModel, ReasoningEffort: testAgentReasoningEffort}, "Administrator"); err != nil {
		t.Fatal(err)
	}
	finish(<-runs, triageagent.Event{Event: "finished", Answer: reviewAgentTestSentence})
	csrf := workflowField(t, admin.get("/triage-agent", 200), "csrf")
	post := func(values url.Values, status int) string {
		t.Helper()
		values.Set("csrf", csrf)
		return admin.post("/triage-agent", values, status)
	}
	page := admin.get("/triage-agent", 200)
	for _, want := range []string{
		"Sign-in to Claude Code works. Last checked at " + pageClock(setUpAt, h.app.Now(), time.UTC) + ".",
		"The subscription token expires at " + pageClock(setUpAt.Add(365*24*time.Hour), h.app.Now(), time.UTC) + ", a year after the token applied.",
		"Sign-in to Codex works. Last checked at ",
		"Review agent: Codex with a Codex access token, model ",
		`value="check-sign-in"`,
		`value="review-agent-check-sign-in"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if want := "The Codex access token expires on 2027-02-06, the date in review_agent.credential_expires_on"; !strings.Contains(pageText(page), want) {
		t.Errorf("the page lacks %q", want)
	}
	if strings.Count(page, "The subscription token expires at") != 1 {
		t.Error("the page gives a subscription token's expiry for the review agent's access token")
	}
	member, _ := addMember(t, h.app, "Member", RoleMember)
	other := h.browser()
	other.identify(member.SignInEmailAddress)
	other.post("/triage-agent", url.Values{"csrf": {csrf}, "action": {"check-sign-in"}}, 403)
	post(url.Values{"action": {"review-agent-check-sign-in"}}, 303)
	check := <-runs
	if check.request.AgentRole != triageagent.ReviewAgentRole || check.request.Prompt != reviewAgentSetupTestPrompt {
		t.Fatalf("the page's check: %+v", check.request)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "https://mmhotline.example/triage-agent", nil)
	addAccessToken(request, "admin@example.test")
	h.app.Handler().ServeHTTP(response, request)
	if response.Header().Get("Refresh") != "5" || !strings.Contains(response.Body.String(), "Checking the sign-in now: started by Administrator at ") {
		t.Fatalf("the page while the check runs: refresh %q", response.Header().Get("Refresh"))
	}
	finish(check, triageagent.Event{Event: "failed", Reason: "Not logged in · Please run /login"})
	if page = admin.get("/triage-agent", 200); !strings.Contains(page, `Sign-in to Codex fails: Not logged in · Please run /login. Last checked at `) {
		t.Fatal("the page does not show the failing sign-in")
	}
	if _, err := h.app.CheckSignInNow(context.Background(), Principal{SignInEmailAddress: member.SignInEmailAddress}, triageAgentRole); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member checked the sign-in: %v", err)
	}
}
