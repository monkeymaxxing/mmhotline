package triageagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signInFields are the parts of a test auth.json that the checks read.
type signInFields struct {
	account, user, loginOwner, refreshToken, apiKey string
	lastRefresh                                     time.Time
	// edit changes the decoded file before it is written.
	edit func(map[string]any)
}

// testSignIn is an auth.json as codex login writes it, with made-up tokens.
func testSignIn(t *testing.T, fields signInFields) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"email": "ops@example.com", "https://api.openai.com/auth": map[string]any{"chatgpt_user_id": fields.user, "chatgpt_account_id": fields.account, "chatgpt_plan_type": "plus"}})
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]any{
		"id_token":      "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".signature-" + fields.refreshToken,
		"access_token":  "access-for-" + fields.refreshToken,
		"refresh_token": fields.refreshToken,
		"account_id":    fields.account,
	}
	if fields.loginOwner != "" {
		tokens["login_owner_id"] = fields.loginOwner
	}
	file := map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": tokens, "last_refresh": fields.lastRefresh.UTC().Format(time.RFC3339Nano)}
	if fields.apiKey != "" {
		file["OPENAI_API_KEY"] = fields.apiKey
	}
	if fields.edit != nil {
		fields.edit(file)
	}
	text, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

var signInTime = time.Date(2026, 11, 1, 9, 30, 0, 123456000, time.UTC)

func firstSignIn(t *testing.T) string {
	t.Helper()
	return testSignIn(t, signInFields{account: "account-1", user: "user-1", loginOwner: "login-1", refreshToken: "refresh-1", lastRefresh: signInTime})
}

// indentedSignIn is a sign-in on several lines, as Codex writes auth.json.
func indentedSignIn(t *testing.T) string {
	t.Helper()
	signIn := firstSignIn(t)
	if !strings.Contains(signIn, "\n") {
		t.Fatal("the sign-in is one line")
	}
	return signIn
}

// The settings file's check refuses a credential that Codex would not use
// as a ChatGPT sign-in, and never quotes it.
func TestChatGPTSignInShape(t *testing.T) {
	without := func(path ...string) signInFields {
		fields := signInFields{account: "account-1", user: "user-1", refreshToken: "refresh-1", lastRefresh: signInTime}
		fields.edit = func(file map[string]any) {
			if len(path) == 1 {
				delete(file, path[0])
			} else {
				delete(file[path[0]].(map[string]any), path[1])
			}
		}
		return fields
	}
	for _, test := range []struct {
		name, text, want string
	}{
		{"pretty-printed, as Codex writes it", firstSignIn(t), ""},
		{"no login_owner_id, from an older Codex", testSignIn(t, signInFields{account: "account-1", user: "user-1", refreshToken: "refresh-1", lastRefresh: signInTime}), ""},
		{"no auth_mode", testSignIn(t, without("auth_mode")), ""},
		{"an API key from the login's token exchange", testSignIn(t, signInFields{account: "a", user: "u", refreshToken: "r", apiKey: "sk-exchanged", lastRefresh: signInTime}), ""},
		{"not JSON", "codex login", "not one JSON object"},
		{"two objects", firstSignIn(t) + "{}", "not one JSON object"},
		{"no tokens", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`, "has no tokens"},
		{"no refresh token", testSignIn(t, without("tokens", "refresh_token")), "lacks tokens.refresh_token"},
		{"no access token", testSignIn(t, without("tokens", "access_token")), "lacks tokens.access_token"},
		{"no ID token", testSignIn(t, without("tokens", "id_token")), "lacks tokens.id_token"},
		{"no account", testSignIn(t, without("tokens", "account_id")), "lacks tokens.account_id"},
		{"no last_refresh", testSignIn(t, without("last_refresh")), "lacks last_refresh"},
		{"an API key sign-in", testSignIn(t, signInFields{account: "a", user: "u", refreshToken: "r", lastRefresh: signInTime, edit: func(file map[string]any) { file["auth_mode"] = "apikey" }}), "auth_mode other than chatgpt"},
		{"an API key without auth_mode", testSignIn(t, signInFields{account: "a", user: "u", refreshToken: "r", apiKey: "sk-x", lastRefresh: signInTime, edit: func(file map[string]any) { delete(file, "auth_mode") }}), "holds another kind of credential"},
		{"last_refresh not a time", testSignIn(t, signInFields{account: "a", user: "u", refreshToken: "r", edit: func(file map[string]any) { file["last_refresh"] = "yesterday" }}), "last_refresh that is not a time"},
		{"ID token not a JSON web token", testSignIn(t, signInFields{account: "a", user: "u", refreshToken: "r", lastRefresh: signInTime, edit: func(file map[string]any) { file["tokens"].(map[string]any)["id_token"] = "opaque" }}), "not a JSON web token"},
		{"65 KiB", testSignIn(t, signInFields{account: "a", user: "u", refreshToken: "r", lastRefresh: signInTime, edit: func(file map[string]any) { file["padding"] = strings.Repeat("x", 65<<10) }}), "larger than 64 KiB"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckSetup("codex", "chatgpt_sign_in", test.text)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "refresh-1") || strings.Contains(err.Error(), "sk-") {
				t.Fatalf("the reason quotes the credential: %v", err)
			}
		})
	}
}

// A copy a run sends back is kept only when it is the same login, no older,
// and adds no API key: the agent's home is not trusted.
func TestCheckChatGPTSignInChange(t *testing.T) {
	stored := firstSignIn(t)
	refreshed := signInFields{account: "account-1", user: "user-1", loginOwner: "login-1", refreshToken: "refresh-2", lastRefresh: signInTime.Add(9 * 24 * time.Hour)}
	with := func(change func(*signInFields)) string {
		fields := refreshed
		change(&fields)
		return testSignIn(t, fields)
	}
	for _, test := range []struct {
		name, returned, want string
	}{
		{"refreshed", testSignIn(t, refreshed), ""},
		{"refreshed by a Codex without login_owner_id", with(func(f *signInFields) { f.loginOwner = "" }), ""},
		{"same last_refresh", with(func(f *signInFields) { f.lastRefresh = signInTime }), ""},
		{"another account", with(func(f *signInFields) { f.account = "account-2" }), "another ChatGPT account"},
		{"another user", with(func(f *signInFields) { f.user = "user-2" }), "another ChatGPT user"},
		{"another login", with(func(f *signInFields) { f.loginOwner = "login-2" }), "another login"},
		{"older last_refresh", with(func(f *signInFields) { f.lastRefresh = signInTime.Add(-time.Second) }), "older last_refresh"},
		{"half written", testSignIn(t, refreshed)[:200], "not one JSON object"},
		{"empty, just truncated", "", "not one JSON object"},
		{"65 KiB", with(func(f *signInFields) {
			f.edit = func(file map[string]any) { file["padding"] = strings.Repeat("x", 65<<10) }
		}), "larger than 64 KiB"},
		{"an added API key", with(func(f *signInFields) { f.apiKey = "sk-planted" }), "adds or changes an OPENAI_API_KEY"},
		{"switched to API key mode", with(func(f *signInFields) { f.edit = func(file map[string]any) { file["auth_mode"] = "apikey" } }), "auth_mode other than chatgpt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckChatGPTSignInChange(stored, test.returned)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
	// A login that stored an API key keeps it through each refresh.
	withKey := testSignIn(t, signInFields{account: "account-1", user: "user-1", refreshToken: "refresh-1", apiKey: "sk-exchanged", lastRefresh: signInTime})
	if err := CheckChatGPTSignInChange(withKey, testSignIn(t, signInFields{account: "account-1", user: "user-1", refreshToken: "refresh-2", apiKey: "sk-exchanged", lastRefresh: signInTime.Add(time.Hour)})); err != nil {
		t.Fatal(err)
	}
}

// Two copies of one login end each other when either refreshes; two logins
// of one account are separate.
func TestSameChatGPTSignIn(t *testing.T) {
	login := firstSignIn(t)
	refreshed := testSignIn(t, signInFields{account: "account-1", user: "user-1", loginOwner: "login-1", refreshToken: "refresh-2", lastRefresh: signInTime.Add(time.Hour)})
	secondLogin := testSignIn(t, signInFields{account: "account-1", user: "user-1", loginOwner: "login-2", refreshToken: "refresh-9", lastRefresh: signInTime})
	olderCodexCopy := testSignIn(t, signInFields{account: "account-1", user: "user-1", refreshToken: "refresh-1", lastRefresh: signInTime})
	if !SameChatGPTSignIn(login, login) || !SameChatGPTSignIn(login, refreshed) || !SameChatGPTSignIn(login, olderCodexCopy) {
		t.Fatal("copies of one login are not the same sign-in")
	}
	if SameChatGPTSignIn(login, secondLogin) {
		t.Fatal("two logins of one account are the same sign-in")
	}
}

// writeSignInScript is a fake codex that records what it was lent, the
// file's mode, and its arguments, rewrites auth.json with refreshed after a
// second when given, and then runs rest.
func fakeCodexWithSignIn(results, refreshed, rest string) string {
	script := `/bin/cp "$CODEX_HOME/auth.json" '` + results + `/lent.json' || exit 7
/bin/ls -ln "$CODEX_HOME/auth.json" | /usr/bin/cut -c1-10 > '` + results + `/mode'
printf '%s\n' "$@" > '` + results + `/arguments'
` + codexSession + "\n"
	if refreshed != "" {
		// Codex truncates auth.json and writes it again.
		script += "/bin/sleep 1.2\nprintf '%s' '" + refreshed + "' > \"$CODEX_HOME/auth.json\"\n"
	}
	return script + rest
}

func readResult(t *testing.T, results, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(results, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// The run writes the lent sign-in to $CODEX_HOME/auth.json, only the run's
// user may read it, Codex keeps its tokens in that file, and the refreshed
// copy goes back to MMHotline before the run's result. Nothing is left
// in the home.
func TestSandboxLendsAndReadsBackTheChatGPTSignIn(t *testing.T) {
	for _, role := range []string{TriageAgentRole, ReviewAgentRole} {
		t.Run(role, func(t *testing.T) {
			results := t.TempDir()
			refreshed := testSignIn(t, signInFields{account: "account-1", user: "user-1", loginOwner: "login-1", refreshToken: "refresh-2", lastRefresh: signInTime.Add(time.Hour)})
			request := testRequest("codex")
			request.AgentRole, request.CredentialType, request.Credential = role, "chatgpt_sign_in", firstSignIn(t)
			run := startFakeRun(t, "codex", fakeCodexWithSignIn(results, refreshed, `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}' '{"type":"turn.completed","usage":{}}'`), request)
			if event := run.next(t); event.Event != "session" {
				t.Fatalf("session: %+v", event)
			}
			var changes []string
			for event := run.next(t); event.Event != "finished"; event = run.next(t) {
				if event.Event == ChatGPTSignInChangedEvent {
					changes = append(changes, event.ChatGPTSignIn)
				} else if event.Event != "step" {
					t.Fatalf("event: %+v", event)
				}
			}
			if len(changes) != 1 || changes[0] != refreshed {
				t.Fatalf("changed sign-ins before the result: %q", changes)
			}
			run.finish(t)
			if lent := readResult(t, results, "lent.json"); lent != request.Credential {
				t.Fatalf("lent %q", lent)
			}
			if mode := readResult(t, results, "mode"); mode != "-rw-------\n" {
				t.Fatalf("auth.json mode %q", mode)
			}
			if arguments := readResult(t, results, "arguments"); !strings.Contains(arguments, "-c\ncli_auth_credentials_store=\"file\"\n") {
				t.Fatalf("arguments: %q", arguments)
			}
			if _, err := os.Stat(filepath.Join(run.home, ".codex", "auth.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("auth.json left in the agent's home: %v", err)
			}
		})
	}
}

// While Codex runs, the run sends a refreshed sign-in within about a second,
// so a run killed later, or during a refresh, loses at most that second.
// A copy Codex was halfway through writing is sent as it is; MMHotline
// refuses its shape.
func TestSandboxSendsARefreshWhileCodexStillRuns(t *testing.T) {
	results := t.TempDir()
	refreshed := testSignIn(t, signInFields{account: "account-1", user: "user-1", loginOwner: "login-1", refreshToken: "refresh-2", lastRefresh: signInTime.Add(time.Hour)})
	half := refreshed[:len(refreshed)/2]
	request := testRequest("codex")
	request.CredentialType, request.Credential = "chatgpt_sign_in", firstSignIn(t)
	home, searchPath := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(searchPath, "codex"), []byte("#!/bin/sh\n"+fakeCodexWithSignIn(results, refreshed, "/bin/sleep 1.5\nprintf '%s' '"+half+"' > \"$CODEX_HOME/auth.json\"\n"+waitForTermination)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	input, inputWriter := io.Pipe()
	output, outputWriter := io.Pipe()
	ctx, kill := context.WithCancel(t.Context())
	t.Cleanup(func() { kill(); _ = inputWriter.Close(); _ = output.Close() })
	done := make(chan error, 1)
	go func() {
		done <- RunInsideSandbox(ctx, input, outputWriter, SandboxSettings{Home: home, SearchPath: searchPath, MMHotlineProgram: testMMHotlineProgram(t)})
		_ = outputWriter.Close()
	}()
	if err := json.NewEncoder(inputWriter).Encode(request); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(output)
	next := func() Event {
		t.Helper()
		var event Event
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	if event := next(); event.Event != "session" {
		t.Fatalf("session: %+v", event)
	}
	if event := next(); event.Event != ChatGPTSignInChangedEvent || event.ChatGPTSignIn != refreshed {
		t.Fatalf("the refresh while Codex runs: %+v", event)
	}
	if event := next(); event.Event != ChatGPTSignInChangedEvent || event.ChatGPTSignIn != half {
		t.Fatalf("the half-written file: %+v", event)
	}
	// MMHotline stops: the run is killed while Codex still runs.
	kill()
	if event := next(); event.Event != "failed" {
		t.Fatalf("after the kill: %+v", event)
	}
	if err := <-done; !errors.Is(err, ErrRunFailed) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auth.json left in the agent's home: %v", err)
	}
}

// A run ends its auth.json once. Runs share the agent's home, so a second
// end after the run's final event, from the run's deferred cleanup, must
// not delete the auth.json that the next run has written meanwhile.
func TestEndingTheSignInFileTwiceKeepsTheNextRunsFile(t *testing.T) {
	home := t.TempDir()
	writer := &eventWriter{encoder: json.NewEncoder(io.Discard)}
	first, err := writeChatGPTSignIn(home, firstSignIn(t), writer)
	if err != nil {
		t.Fatal(err)
	}
	first.end()
	if _, err := writeChatGPTSignIn(home, firstSignIn(t), writer); err != nil {
		t.Fatal(err)
	}
	first.end()
	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); err != nil {
		t.Fatalf("the next run's auth.json after the earlier run ended again: %v", err)
	}
}
