package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// fakeModelsLists stands in for Anthropic's and OpenAI's models lists. It
// answers each request with status and keeps the requests' paths and
// headers.
type fakeModelsLists struct {
	mu       sync.Mutex
	status   int
	requests []*http.Request
}

func (f *fakeModelsLists) answer(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeModelsLists) asked() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request{}, f.requests...)
}

// installFakeModelsLists points the API-key sign-in check at the fake
// models lists, which answer 200 until told otherwise: Anthropic's at
// /anthropic and OpenAI's at /openai.
func installFakeModelsLists(t *testing.T, a *App) (*fakeModelsLists, string) {
	t.Helper()
	fake := &fakeModelsLists{status: http.StatusOK}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.requests = append(fake.requests, r)
		status := fake.status
		fake.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-ab***yz"}}`))
	}))
	t.Cleanup(server.Close)
	a.triageAgent.codingAgentAPIBaseURLs = map[string]string{"claude_code": server.URL + "/anthropic", "codex": server.URL + "/openai"}
	return fake, server.URL
}

// waitForSignInCheck waits until the latest sign-in check of purpose ends
// and returns its state and failure reason.
func waitForSignInCheck(t *testing.T, a *App, purpose string) (state, reason string) {
	t.Helper()
	waitForCondition(t, "the sign-in check's end", func() bool {
		return countRows(t, a, `SELECT count(*) FROM triage_agent_runs WHERE purpose=? AND state='running'`, purpose) == 0
	})
	if err := a.Store.db.QueryRow(`SELECT state,failure_reason FROM triage_agent_runs WHERE purpose=? ORDER BY started_at DESC,rowid DESC LIMIT 1`, purpose).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	return state, reason
}

// An API key's daily and by-hand sign-in check asks the vendor's models
// list instead of running the coding agent: Claude Code's key goes to
// Anthropic in x-api-key with anthropic-version, Codex's to OpenAI as a
// bearer token. 200 passes; another answer or no answer fails with a plain
// reason that never shows the key or the vendor's words, and is told as a
// failed prompt check is: one warn alert, then one info alert when the key
// works again, and show gives the reason.
func TestAnAPIKeySignInCheckAsksTheModelsList(t *testing.T) {
	for _, role := range agentRoles {
		t.Run(role.name, func(t *testing.T) {
			r := newTriageSessionRig(t, false)
			fake, serverURL := installFakeModelsLists(t, r.a)
			const credential = "sk-secret-api-key-0123456789"
			vendor, codingAgent, path := "OpenAI", "Codex", "/openai/v1/models"
			if role.name == reviewAgentRole.name {
				vendor, codingAgent, path = "Anthropic", "Claude Code", "/anthropic/v1/models"
				if _, err := r.a.StartReviewAgentSetupTest(context.Background(), r.p, "claude_code", "api_key", credential); err != nil {
					t.Fatal(err)
				}
				// The setup test still runs the prompt in the sandbox.
				r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: reviewAgentTestSentence})
			} else if _, err := r.a.Store.db.Exec(`UPDATE triage_agent_setup SET credential=?`, credential); err != nil {
				t.Fatal(err)
			}
			address := serverURL + path
			failing := "The " + role.name + " cannot sign in to " + codingAgent
			again := "The " + role.name + " can sign in to " + codingAgent + " again"

			check := func(status int) (string, string) {
				t.Helper()
				fake.answer(status)
				before := len(fake.asked())
				if _, raw := commandRequest(t, r.a, CommandRequest{Group: role.commandGroup, Action: "check-sign-in"}, 200); strings.Contains(raw, credential) {
					t.Fatalf("check-sign-in shows the key: %s", raw)
				}
				state, reason := waitForSignInCheck(t, r.a, role.signInCheckPurpose)
				asked := fake.asked()
				if len(asked) != before+1 {
					t.Fatalf("the models list was asked %d times", len(asked)-before)
				}
				request := asked[len(asked)-1]
				wantHeaders := map[string]string{"Authorization": "Bearer " + credential, "X-Api-Key": "", "Anthropic-Version": ""}
				if role.name == reviewAgentRole.name {
					wantHeaders = map[string]string{"Authorization": "", "X-Api-Key": credential, "Anthropic-Version": "2023-06-01"}
				}
				for header, want := range wantHeaders {
					if got := request.Header.Get(header); got != want {
						t.Errorf("%s: %q, want %q", header, got, want)
					}
				}
				if request.Method != http.MethodGet || request.URL.Path != path || request.URL.RawQuery != "" {
					t.Errorf("asked %s %s", request.Method, request.URL)
				}
				return state, reason
			}

			// No coding agent ran: the check asked only the models list.
			if state, reason := check(http.StatusOK); state != "finished" || reason != "" {
				t.Fatalf("200: %s %q", state, reason)
			}
			r.noRunStarts(t)

			state, reason := check(http.StatusUnauthorized)
			if want := vendor + "'s models list at " + address + " answered 401 Unauthorized: the API key is wrong, or was deleted or disabled."; state != "failed" || reason != want {
				t.Fatalf("401: %s %q\nwant %q", state, reason, want)
			}
			if got := notices(t, r.a, "warn", failing); got != 1 {
				t.Fatalf("%d warn alerts after the failed check", got)
			}
			message := noticeMessage(t, r.a, failing)
			if !strings.Contains(message, "failed at") || !strings.Contains(message, reason) || strings.Contains(message, credential) || strings.Contains(message, "sk-ab") {
				t.Errorf("the alert:\n%s", message)
			}
			result, _ := commandRequest(t, r.a, CommandRequest{Group: role.commandGroup, Action: "show"}, 200)
			var setup *TriageAgentSetup
			if role.name == reviewAgentRole.name {
				setup = result.ReviewAgent.Setup
			} else {
				setup = result.TriageAgent.Setup
			}
			if setup.SignInWorks || setup.SignInFailureReason != reason {
				t.Errorf("show: works %t, reason %q", setup.SignInWorks, setup.SignInFailureReason)
			}

			// An answer the check does not explain names only its status.
			if _, reason = check(http.StatusServiceUnavailable); reason != vendor+"'s models list at "+address+" answered 503 Service Unavailable." {
				t.Fatalf("503: %q", reason)
			}
			if got := notices(t, r.a, "warn", failing); got != 1 {
				t.Fatalf("%d warn alerts while the check keeps failing", got)
			}
			if state, _ = check(http.StatusOK); state != "finished" || notices(t, r.a, "info", again) != 1 {
				t.Fatalf("the check passed again: %s, %d info alerts", state, notices(t, r.a, "info", again))
			}

			// No answer at all fails the check too.
			r.a.triageAgent.codingAgentAPIBaseURLs = map[string]string{"claude_code": "http://127.0.0.1:1", "codex": "http://127.0.0.1:1"}
			if _, err := r.a.CheckSignInNow(context.Background(), r.p, role); err != nil {
				t.Fatal(err)
			}
			if state, reason = waitForSignInCheck(t, r.a, role.signInCheckPurpose); state != "failed" || !strings.HasPrefix(reason, "MMHotline could not reach "+vendor+"'s models list at http://127.0.0.1:1/v1/models: ") {
				t.Fatalf("no answer: %s %q", state, reason)
			}
		})
	}
}

// The daily check of an API key asks the models list when it is due, as the
// daily prompt check does.
func TestTheDailyAPIKeySignInCheckAsksTheModelsList(t *testing.T) {
	r := newTriageSessionRig(t, true)
	fake, _ := installFakeModelsLists(t, r.a)
	if err := r.a.SetSoftwareReviewInterval(context.Background(), r.p, 8760); err != nil {
		t.Fatal(err)
	}
	r.advance(24*time.Hour - time.Second)
	r.rounds(t)
	if asked := len(fake.asked()); asked != 0 {
		t.Fatalf("asked %d times before the check was due", asked)
	}
	r.advance(time.Second)
	r.rounds(t)
	if state, _ := waitForSignInCheck(t, r.a, "sign_in_check"); state != "finished" || len(fake.asked()) != 1 {
		t.Fatalf("the daily check: %s, asked %d times", state, len(fake.asked()))
	}
	r.noRunStarts(t)
}

// TestAnAPIKeySignInCheckFollowsNoRedirect: a models list that answers with a
// redirect fails the check, and the key never reaches the redirect's address.
func TestAnAPIKeySignInCheckFollowsNoRedirect(t *testing.T) {
	var elsewhere sync.Mutex
	reached := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Lock()
		reached = true
		elsewhere.Unlock()
	}))
	t.Cleanup(other.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/models", http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	r := newTriageSessionRig(t, false)
	r.a.triageAgent.codingAgentAPIBaseURLs = map[string]string{"claude_code": redirecting.URL, "codex": redirecting.URL}
	event := r.a.checkAPIKeyWithModelsList(context.Background(), triageagent.Request{CodingAgent: "codex", Credential: "sk-secret-api-key-0123456789"})
	if event.Event != "failed" || !strings.Contains(event.Reason, "302") {
		t.Fatalf("a redirect gave %s %q", event.Event, event.Reason)
	}
	elsewhere.Lock()
	defer elsewhere.Unlock()
	if reached {
		t.Fatal("the check followed the redirect and sent the key elsewhere")
	}
}
