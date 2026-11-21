package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// Codex with a ChatGPT sign-in or a Codex access token:
// docs/changing-mmhotline/design/triage-agent.md#codex-with-a-chatgpt-sign-in.

// chatGPTSignIn is an auth.json as codex login writes it, with made-up
// tokens: login is the login_owner_id, refresh the refresh token.
func chatGPTSignIn(t *testing.T, account, user, login, refresh string, lastRefresh time.Time) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": user, "chatgpt_account_id": account}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.MarshalIndent(map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "last_refresh": lastRefresh.UTC().Format(time.RFC3339Nano),
		"tokens": map[string]any{"id_token": "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".id-signature-" + refresh, "access_token": "access-" + refresh, "refresh_token": refresh, "account_id": account, "login_owner_id": login},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

var firstSignInAt = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// signInCopy is the nth copy of one login, each refreshed an hour after the
// one before, as Codex rotates the refresh token.
func signInCopy(t *testing.T, n int) string {
	t.Helper()
	return chatGPTSignIn(t, "account-1", "user-1", "login-1", "refresh-"+string(rune('0'+n)), firstSignInAt.Add(time.Duration(n)*time.Hour))
}

// storedSignIn is role's setup's settings file credential and current copy.
func storedSignIn(t *testing.T, a *App, role agentRole) (string, string) {
	t.Helper()
	var credential, current string
	if err := a.Store.db.QueryRow(`SELECT credential,current_chatgpt_sign_in FROM `+role.setupTable+` WHERE id=1`).Scan(&credential, &current); err != nil {
		t.Fatal(err)
	}
	return credential, current
}

// realSandboxFixture serves both run sockets with the real run inside the
// sandbox, as systemd starts it, around a fake codex in programs.
func realSandboxFixture(t *testing.T, a *App, programs string) {
	t.Helper()
	// A short socket path also fits macOS's Unix socket path limit.
	directory, err := os.MkdirTemp("/tmp", "sandbox-")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(directory, "home")
	if err = os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, role := range []string{triageagent.TriageAgentRole, triageagent.ReviewAgentRole, "proxy"} {
		paths[role] = filepath.Join(directory, role+".sock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var listeners []net.Listener
	for _, role := range []string{triageagent.TriageAgentRole, triageagent.ReviewAgentRole} {
		listener, err := net.Listen("unix", paths[role])
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		go func() {
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer connection.Close()
					_ = triageagent.RunInsideSandbox(ctx, connection, connection, triageagent.SandboxSettings{ProxySocketPath: paths["proxy"], Home: home, SearchPath: programs, MMHotlineProgram: "/usr/bin/false"})
				}()
			}
		}()
	}
	if err = a.InitializeTriageAgent(ctx, paths[triageagent.TriageAgentRole], paths[triageagent.ReviewAgentRole], paths["proxy"]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.CloseTriageAgent()
		cancel()
		for _, listener := range listeners {
			listener.Close()
		}
		os.RemoveAll(directory)
	})
}

// fakeRefreshingCodex is a codex that plays Codex's ChatGPT sign-in. It
// fails without $CODEX_HOME/auth.json, as Codex does, keeps the copy it was
// lent in lent.json, and reads its plan from files in results: with
// next.json it refreshes after a second, writing next.json to auth.json as
// Codex does, by truncating it and writing again; with half.json it then
// writes half.json, as a Codex killed halfway through a write leaves it,
// and hangs; with fail it then fails, as for a model the service refuses;
// otherwise it answers.
func fakeRefreshingCodex(t *testing.T, results string) string {
	t.Helper()
	answer, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": triageAgentTestSentence}})
	if err != nil {
		t.Fatal(err)
	}
	programs := t.TempDir()
	script := `#!/bin/sh
results='` + results + `'
[ -f "$CODEX_HOME/auth.json" ] || { echo 'Not logged in' >&2; exit 1; }
/bin/cp "$CODEX_HOME/auth.json" "$results/lent.json"
printf '%s\n' '{"type":"thread.started","thread_id":"session-one"}'
if [ -f "$results/next.json" ]; then
  /bin/sleep 1.2
  /bin/cat "$results/next.json" > "$CODEX_HOME/auth.json"
  /bin/mv "$results/next.json" "$results/used.json"
fi
if [ -f "$results/half.json" ]; then
  /bin/sleep 1.5
  /bin/cat "$results/half.json" > "$CODEX_HOME/auth.json"
  /bin/mv "$results/half.json" "$results/used-half.json"
  count=0; while [ "$count" -lt 6000 ]; do /bin/sleep 0.01; count=$((count+1)); done
fi
if [ -f "$results/fail" ]; then
  /bin/rm "$results/fail"
  echo 'The model gpt-typo does not exist.' >&2
  exit 1
fi
printf '%s\n' '` + string(answer) + `' '{"type":"turn.completed","usage":{}}'
`
	if err = os.WriteFile(filepath.Join(programs, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return programs
}

func writeResult(t *testing.T, results, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(results, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// MMHotline lends each run the newest copy of a ChatGPT sign-in and keeps
// the copy Codex refreshed during the run, through the real run inside the
// sandbox and a fake codex that refreshes as Codex does. The settings file's
// sign-in, which the runs used up, never replaces the newest copy, and a run
// killed halfway through a refresh keeps the last whole copy.
func TestChatGPTSignInKeepsTheNewestCopyAcrossRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := foundationApp(t)
	results := t.TempDir()
	realSandboxFixture(t, a, fakeRefreshingCodex(t, results))
	lent := func() string { t.Helper(); return readFile(t, filepath.Join(results, "lent.json")) }
	setUp := func(model string) triageagent.Event {
		t.Helper()
		run, err := a.startCodingAgentSetupTest(ctx, triageAgentRole, SettingsCodingAgent{CodingAgent: "codex", CredentialType: "chatgpt_sign_in", Credential: signInCopy(t, 0), Model: model, ReasoningEffort: "high"}, "the settings file")
		if err != nil {
			t.Fatal(err)
		}
		<-run.done
		return run.result
	}
	checkSignIn := func() triageagent.Event {
		t.Helper()
		id, err := a.startSignInCheck(ctx, triageAgentRole, "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitNoTriageRun(t, a)
		return latestRun(t, a, id)
	}

	// The setup test refreshes the file's sign-in: the setup keeps the
	// file's credential, to tell a new one, and the refreshed copy as the
	// one to lend.
	writeResult(t, results, "next.json", signInCopy(t, 1))
	if result := setUp("gpt-test"); result.Event != "finished" || result.Reason != "" {
		t.Fatalf("setup test: %+v", result)
	}
	if lent() != signInCopy(t, 0) {
		t.Fatal("the setup test did not get the file's sign-in")
	}
	if credential, current := storedSignIn(t, a, triageAgentRole); credential != signInCopy(t, 0) || current != signInCopy(t, 1) {
		t.Fatalf("after the setup test: credential %q, current %q", credential, current)
	}

	// The next run gets the refreshed copy, and its own refresh is kept.
	writeResult(t, results, "next.json", signInCopy(t, 2))
	if result := checkSignIn(); result.Event != "finished" {
		t.Fatalf("sign-in check: %+v", result)
	}
	if lent() != signInCopy(t, 1) {
		t.Fatal("the sign-in check did not get the newest copy")
	}
	if _, current := storedSignIn(t, a, triageAgentRole); current != signInCopy(t, 2) {
		t.Fatal("the sign-in check's refresh was not kept")
	}

	// A change of only the model tests with the newest copy, not the file's
	// used-up one, and keeps the newest copy.
	if result := setUp("gpt-test-next"); result.Event != "finished" || result.Reason != "" {
		t.Fatalf("model change: %+v", result)
	}
	if lent() != signInCopy(t, 2) {
		t.Fatal("a model change lent the settings file's used-up sign-in")
	}
	if credential, current := storedSignIn(t, a, triageAgentRole); credential != signInCopy(t, 0) || current != signInCopy(t, 2) {
		t.Fatal("a model change replaced the newest copy")
	}

	// Codex refreshes, then is killed halfway through its next write. The
	// once-a-second check sent the whole copy while Codex still ran; the
	// half-written file is refused, and the whole copy stays.
	writeResult(t, results, "next.json", signInCopy(t, 3))
	whole := signInCopy(t, 4)
	writeResult(t, results, "half.json", whole[:len(whole)/2])
	id, err := a.startSignInCheck(ctx, triageAgentRole, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the refresh to be saved while Codex runs", func() bool {
		_, current := storedSignIn(t, a, triageAgentRole)
		return current == signInCopy(t, 3)
	})
	waitFor(t, "the half-written copy to be refused", func() bool {
		return countRows(t, a, `SELECT count(*) FROM triage_agent_run_steps WHERE run_id=? AND text LIKE 'MMHotline did not save the changed ChatGPT sign-in, so it keeps the earlier copy: the changed auth.json is not one JSON object%'`, id) == 1
	})
	if err = a.StopTriageAgentRun(id); err != nil {
		t.Fatal(err)
	}
	waitNoTriageRun(t, a)
	if _, current := storedSignIn(t, a, triageAgentRole); current != signInCopy(t, 3) {
		t.Fatal("the run killed during a write did not keep the last whole copy")
	}
	if countRows(t, a, `SELECT count(*) FROM triage_agent_run_steps WHERE text LIKE '%refresh-%'`) != 0 {
		t.Fatal("a step quotes a token")
	}
	if result := checkSignIn(); result.Event != "finished" || lent() != signInCopy(t, 3) {
		t.Fatalf("the run after the kill: %+v", result)
	}

	// A model typo fails the setup test after Codex refreshed the newest
	// copy it was lent. The refresh used up the lent copy, so the refreshed
	// copy is kept although the setup test failed.
	writeResult(t, results, "next.json", signInCopy(t, 5))
	writeResult(t, results, "fail", "")
	if result := setUp("gpt-typo"); result.Event == "finished" {
		t.Fatalf("the setup test with a model typo passed: %+v", result)
	}
	if lent() != signInCopy(t, 3) {
		t.Fatal("the model typo's setup test did not get the newest copy")
	}
	if _, current := storedSignIn(t, a, triageAgentRole); current != signInCopy(t, 5) {
		t.Fatal("the failed setup test lost the copy Codex refreshed")
	}
}

// latestRun is run id's recorded result.
func latestRun(t *testing.T, a *App, id string) triageagent.Event {
	t.Helper()
	var event triageagent.Event
	if err := a.Store.db.QueryRow(`SELECT state,failure_reason FROM triage_agent_runs WHERE id=?`, id).Scan(&event.Event, &event.Reason); err != nil {
		t.Fatal(err)
	}
	return event
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatal("timed out waiting for " + what)
}

// setUpChatGPTSignIn saves role's setup as Codex with a ChatGPT sign-in
// whose newest copy is current, as a passed setup test leaves it.
func setUpChatGPTSignIn(t *testing.T, a *App, role agentRole, current string) {
	t.Helper()
	if _, err := a.Store.db.Exec(`INSERT INTO `+role.setupTable+`(id,coding_agent,credential_type,credential,current_chatgpt_sign_in,model,reasoning_effort,set_up_at,set_up_by) VALUES(1,'codex','chatgpt_sign_in',?,?,'gpt-test','high',?,'the settings file')
ON CONFLICT(id) DO UPDATE SET coding_agent='codex',credential_type='chatgpt_sign_in',credential=excluded.credential,current_chatgpt_sign_in=excluded.current_chatgpt_sign_in`, signInCopy(t, 0), current, a.Now().Unix()); err != nil {
		t.Fatal(err)
	}
}

// A copy a run sends back that is not the same login, is older, is broken
// or too large, or adds an API key is refused: the record says why without
// the content, the stored copy stays, and no output shows a token, of the
// lent copy or of one the run sent back.
func TestReturnedChatGPTSignInsThatFailTheChecksAreRefused(t *testing.T) {
	parallelStartingProcesses(t)
	for _, role := range agentRoles {
		t.Run(role.name, func(t *testing.T) {
			r := newTriageSessionRig(t, false)
			setUpChatGPTSignIn(t, r.a, role, signInCopy(t, 1))
			id, err := r.a.startSignInCheck(context.Background(), role, "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			run := r.nextRun(t)
			if run.request.Credential != signInCopy(t, 1) {
				t.Fatal("the run was not lent the current copy")
			}
			withAPIKey := strings.Replace(signInCopy(t, 2), `"OPENAI_API_KEY": null`, `"OPENAI_API_KEY": "sk-planted"`, 1)
			refused := map[string]string{
				chatGPTSignIn(t, "account-2", "user-1", "login-1", "refresh-x", firstSignInAt.Add(5*time.Hour)): "the changed auth.json is for another ChatGPT account",
				chatGPTSignIn(t, "account-1", "user-2", "login-1", "refresh-x", firstSignInAt.Add(5*time.Hour)): "the changed auth.json is for another ChatGPT user",
				chatGPTSignIn(t, "account-1", "user-1", "login-2", "refresh-x", firstSignInAt.Add(5*time.Hour)): "the changed auth.json is another login",
				signInCopy(t, 0):                               "the changed auth.json has an older last_refresh than the stored copy",
				signInCopy(t, 2)[:120]:                         "the changed auth.json is not one JSON object with the shape of Codex's auth.json",
				signInCopy(t, 2) + strings.Repeat(" ", 65<<10): "the changed auth.json is larger than 64 KiB",
				withAPIKey: "the changed auth.json adds or changes an OPENAI_API_KEY",
				strings.Replace(signInCopy(t, 2), "chatgpt", "x", 1): "the changed auth.json has an auth_mode other than chatgpt",
			}
			for copy := range refused {
				sendTriageEvent(t, run, triageagent.Event{Event: triageagent.ChatGPTSignInChangedEvent, ChatGPTSignIn: copy})
			}
			// Tokens of the lent copy, and of a copy the run sent back, in
			// the agent's output.
			sendTriageEvent(t, run, triageagent.Event{Event: triageagent.ChatGPTSignInChangedEvent, ChatGPTSignIn: signInCopy(t, 2)})
			sendTriageEvent(t, run, triageagent.Event{Event: "step", Step: &triageagent.Step{Kind: "command", Text: "cat ~/.codex/auth.json # access-refresh-1 refresh-1 refresh-2"}})
			r.end(t, run, triageagent.Event{Event: "finished", Answer: setupTestSentence(role.signInCheckPurpose) + " access-refresh-2"})
			for _, reason := range refused {
				if countRows(t, r.a, `SELECT count(*) FROM triage_agent_run_steps WHERE run_id=? AND text=?`, id, "MMHotline did not save the changed ChatGPT sign-in, so it keeps the earlier copy: "+reason+".") != 1 {
					t.Errorf("no record of the refusal %q", reason)
				}
			}
			if _, current := storedSignIn(t, r.a, role); current != signInCopy(t, 2) {
				t.Fatal("the stored copy is not the one valid copy the run sent back")
			}
			for _, token := range []string{"refresh-1", "refresh-2", "sk-planted", "account-2"} {
				if countRows(t, r.a, `SELECT count(*) FROM triage_agent_run_steps WHERE text LIKE ?`, "%"+token+"%")+countRows(t, r.a, `SELECT count(*) FROM triage_agent_runs WHERE answer LIKE ?`, "%"+token+"%") != 0 {
					t.Errorf("output shows %q", token)
				}
			}
			if countRows(t, r.a, `SELECT count(*) FROM triage_agent_run_steps WHERE text LIKE '%[credential removed] [credential removed] [credential removed]%'`) != 1 {
				t.Fatal("the tokens were not removed from the step")
			}
		})
	}
}

// The review agent needs its own ChatGPT sign-in: a review runs while the
// triage run that asked for it is active, and two copies of one sign-in end
// each other. Two sign-ins of one account are fine.
func TestReviewAgentChatGPTSignInMustNotBeTheTriageAgents(t *testing.T) {
	t.Parallel()
	settings := func(triage, review string) string {
		text := replaced(t, exampleSettingsYAML, "  coding_agent: claude_code\n  credential_type: subscription_token\n  credential: \"sk-ant-oat01-example-triage-agent-credential\"\n",
			"  coding_agent: codex\n  credential_type: chatgpt_sign_in\n  credential: '"+strings.ReplaceAll(triage, "\n", "")+"'\n")
		return replaced(t, text, "  credential_type: api_key\n  credential: \"sk-example-review-agent-credential\"\n",
			"  credential_type: chatgpt_sign_in\n  credential: '"+strings.ReplaceAll(review, "\n", "")+"'\n")
	}
	const refusal = "review_agent.credential: The review agent needs its own ChatGPT sign-in. Sign in again in a new empty folder and use that auth.json."
	for _, test := range []struct {
		name, review string
		refused      bool
	}{
		{"the same sign-in", signInCopy(t, 0), true},
		{"a copy of the same login with another refresh token", signInCopy(t, 3), true},
		{"the same refresh token from an older Codex", chatGPTSignIn(t, "account-1", "user-1", "", "refresh-0", firstSignInAt), true},
		{"another sign-in of the same account", chatGPTSignIn(t, "account-1", "user-1", "login-2", "refresh-9", firstSignInAt), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, problems := ReadSettingsFile(writeSettingsFile(t, settings(signInCopy(t, 0), test.review)), nil)
			texts := problemTexts(problems)
			if refused := len(texts) == 1 && texts[0] == refusal; refused != test.refused || !test.refused && len(texts) != 0 {
				t.Fatalf("problems: %q", texts)
			}
		})
	}
	// settings apply refuses the file with the same problem, before any
	// setup test.
	f := newSettingsFileFixture(t, settings(signInCopy(t, 0), signInCopy(t, 0)))
	view, err := f.a.ApplySettingsFile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if texts := problemTexts(view.Problems); len(texts) != 1 || texts[0] != refusal || len(view.Parts) != 0 {
		t.Fatalf("apply: %q, %d parts", texts, len(view.Parts))
	}
}

// A Codex access token gets the expiry reminders 30 and 7 days before the
// date in credential_expires_on. A token that applies with 30 days or less
// left gets only the 7-day reminder.
func TestAccessTokenExpiryReminders(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name          string
		daysLeft      int
		wantReminders []int
	}{
		{"90-day token", 90, []int{30, 7}},
		{"20-day token", 20, []int{7}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, false)
			expiresOn := r.a.Now().UTC().AddDate(0, 0, test.daysLeft+1).Format("2006-01-02")
			expiresAt, _ := time.Parse("2006-01-02", expiresOn)
			if _, err := r.a.startCodingAgentSetupTest(context.Background(), triageAgentRole, SettingsCodingAgent{CodingAgent: "codex", CredentialType: "access_token", Credential: "at-example-token", CredentialExpiresOn: expiresOn, Model: "gpt-test", ReasoningEffort: "high"}, "the settings file"); err != nil {
				t.Fatal(err)
			}
			run := r.nextRun(t)
			if run.request.Credential != "at-example-token" || run.request.CredentialType != "access_token" {
				t.Fatalf("setup test request: %+v", run.request)
			}
			r.end(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
			r.a = restartApp(t, r.a)
			title := "The triage agent's Codex token expires on " + expiresOn
			reminders := func() int { t.Helper(); r.rounds(t); return notices(t, r.a, "warn", title) }
			if got := reminders(); got != 0 {
				t.Fatalf("%d reminders right after the set-up", got)
			}
			for index, days := range test.wantReminders {
				r.advance(expiresAt.Add(-time.Duration(days)*24*time.Hour - time.Second).Sub(r.a.Now()))
				if got := reminders(); got != index {
					t.Fatalf("%d reminders a second before %d days remain", got, days)
				}
				r.advance(time.Second)
				if got := reminders(); got != index+1 {
					t.Fatalf("%d reminders when %d days remain", got, days)
				}
			}
			message := noticeMessage(t, r.a, title)
			for _, want := range []string{
				"with a Codex access token, which ends on " + expiresOn + ", the date in the settings file's triage_agent.credential_expires_on",
				"make a new Codex access token on the Access tokens page",
				"its end date in triage_agent.credential_expires_on",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("the reminder lacks %q:\n%s", want, message)
				}
			}
			view, err := r.a.TriageAgentState(context.Background(), r.p)
			if err != nil {
				t.Fatal(err)
			}
			if lines := strings.Join(view.Setup.credentialLines(), "\n"); !strings.Contains(lines, "Codex access token expires on: "+expiresOn) {
				t.Fatalf("show: %s", lines)
			}
		})
	}
}

// When a ChatGPT sign-in stops working, the sign-in check's alert names the
// cause in plain words and says how to sign in again; an access token's
// alert says how to make a new token.
func TestCodexSignInCheckAlertsSayHowToSignInAgain(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		credentialType, reason string
		want                   []string
	}{
		{"chatgpt_sign_in", "Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.", []string{
			"Another copy of this ChatGPT sign-in was used, so OpenAI ended this one.",
			"   export CODEX_HOME=\"$(mktemp -d)\"\n   codex login\n   cat \"$CODEX_HOME/auth.json\"",
			"Do not run codex logout",
			"2. Put the printed auth.json text in triage_agent.credential",
		}},
		{"chatgpt_sign_in", "unexpected status 401: refresh_token_invalidated", []string{"OpenAI revoked this ChatGPT sign-in"}},
		{"access_token", "unexpected status 401 Unauthorized", []string{
			"make a new Codex access token on the Access tokens page",
			"and its end date in triage_agent.credential_expires_on",
		}},
	} {
		t.Run(test.credentialType+"/"+test.reason, func(t *testing.T) {
			r := newTriageSessionRig(t, false)
			if test.credentialType == "chatgpt_sign_in" {
				setUpChatGPTSignIn(t, r.a, triageAgentRole, signInCopy(t, 1))
			} else if _, err := r.a.Store.db.Exec(`UPDATE triage_agent_setup SET credential_type='access_token',credential='at-token',credential_expires_on='2027-02-06'`); err != nil {
				t.Fatal(err)
			}
			if _, err := r.a.startSignInCheck(context.Background(), triageAgentRole, "MMHotline", nil); err != nil {
				t.Fatal(err)
			}
			r.end(t, r.nextRun(t), triageagent.Event{Event: "failed", Reason: test.reason})
			message := noticeMessage(t, r.a, "The triage agent cannot sign in to Codex")
			for _, want := range test.want {
				if !strings.Contains(message, want) {
					t.Errorf("the alert lacks %q:\n%s", want, message)
				}
			}
		})
	}
}

// The logging proxy lists the hosts that the run's credential type needs:
// OpenAI's API for an API key; ChatGPT and OpenAI's sign-in service for a
// ChatGPT sign-in or a Codex access token, which never reach the API.
func TestCodexListedHostsFollowTheCredentialType(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		credentialType, credential string
		allowed, refused           []string
	}{
		{"api_key", "sk-key", []string{"api.openai.com"}, []string{"chatgpt.com", "auth.openai.com"}},
		{"access_token", "at-token", []string{"chatgpt.com", "auth.openai.com"}, []string{"api.openai.com"}},
		{"chatgpt_sign_in", "", []string{"chatgpt.com", "auth.openai.com"}, []string{"api.openai.com"}},
	} {
		t.Run(test.credentialType, func(t *testing.T) {
			a := foundationApp(t)
			runs := triageRunFixture(t, a)
			a.triageAgent.resolve = func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
			}
			a.triageAgent.dial = func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() { _, _ = io.Copy(io.Discard, server) }()
				return client, nil
			}
			credential := test.credential
			if credential == "" {
				credential = signInCopy(t, 0)
			}
			setup := SettingsCodingAgent{CodingAgent: "codex", CredentialType: test.credentialType, Credential: credential, Model: "gpt-test", ReasoningEffort: "high"}
			if test.credentialType == "access_token" {
				setup.CredentialExpiresOn = "2027-02-06"
			}
			if _, err := a.startCodingAgentSetupTest(context.Background(), triageAgentRole, setup, "the settings file"); err != nil {
				t.Fatal(err)
			}
			run := <-runs
			for _, host := range append(append([]string{}, test.allowed...), test.refused...) {
				client, _, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
				client.Close()
				if allowed := response.StatusCode == 200; allowed != strings.Contains(strings.Join(test.allowed, " "), host) {
					t.Errorf("%s: %d", host, response.StatusCode)
				}
			}
			sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
			waitNoTriageRun(t, a)
		})
	}
}
