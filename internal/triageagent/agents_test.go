package triageagent

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAgentFixtures(t *testing.T) {
	for _, test := range []struct {
		name, session, answer string
		steps                 []Step
		usage                 Usage
	}{
		{"claude-code-stream-clean.jsonl", "0b5f7f0e-4c2a-4d7e-9a51-2f3c8e1d6a90", "done", []Step{{"command", "echo hello-from-tool"}, {"message", "done"}}, Usage{InputTokens: 73822, CachedInputTokens: 47178, OutputTokens: 87}},
		{"codex-stream.jsonl", "01a175c4-35db-7912-8114-8824dc5cbc5e", "200 ; curl: (56) CONNECT tunnel failed, response 403 — 000", []Step{
			{"message", "I’ll run both commands and return their outputs on one line.\n"},
			{"command", `/usr/bin/bash -lc "curl -sS -m 10 -o /dev/null -w '%{http_code}' https://example.com"`},
			{"command", `/usr/bin/bash -lc "curl -sS -m 10 -o /dev/null -w '%{http_code}' https://github.com"`},
			{"message", "200 ; curl: (56) CONNECT tunnel failed, response 403 — 000"},
		}, Usage{InputTokens: 26658, CachedInputTokens: 13207, OutputTokens: 145}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.Open("testdata/" + test.name)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var agent codingAgent = codex{}
			if strings.HasPrefix(test.name, "claude") {
				agent = claudeCode{}
			}
			stream := &agentStream{}
			var steps []Step
			var sessions []string
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				events, err := agent.readLine(scanner.Bytes(), stream)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Event == "session" {
						sessions = append(sessions, event.SessionID)
					}
					if event.Step != nil {
						steps = append(steps, *event.Step)
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sessions, []string{test.session}) {
				t.Fatalf("sessions: %v", sessions)
			}
			if !reflect.DeepEqual(steps, test.steps) {
				t.Fatalf("steps: %#v", steps)
			}
			if len(stream.activeSteps) != 0 {
				t.Fatalf("steps still active: %v", stream.activeSteps)
			}
			if stream.final == nil || stream.final.Event != "finished" || stream.final.Answer != test.answer {
				t.Fatalf("final: %+v", stream.final)
			}
			usage := *stream.final.CodingAgentSessionTotalUsage
			if strings.HasPrefix(test.name, "claude") {
				if usage.CostInUSDollars == nil || *usage.CostInUSDollars != 0.00584368 {
					t.Fatalf("cost: %v", usage.CostInUSDollars)
				}
				usage.CostInUSDollars = nil
			}
			if !reflect.DeepEqual(usage, test.usage) {
				t.Fatalf("usage: %+v", usage)
			}
		})
	}
}

func TestAgentFailures(t *testing.T) {
	for _, test := range []struct {
		agent codingAgent
		line  string
	}{
		{claudeCode{}, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"permission refused"}`},
		{claudeCode{}, `{"type":"result","subtype":"success","is_error":true,"result":"permission refused"}`},
		{codex{}, `{"type":"turn.failed","error":{"message":"permission refused"}}`},
	} {
		stream := &agentStream{}
		_, err := test.agent.readLine([]byte(test.line), stream)
		if err != nil {
			t.Fatal(err)
		}
		if stream.final == nil || stream.final.Event != "failed" || stream.final.Reason != "permission refused" {
			t.Fatalf("final: %+v", stream.final)
		}
	}
}

// A resumed Claude Code session's result reports the run's own usage in
// usage, but its cost and modelUsage are the session's totals, earlier runs
// included, as Claude Code 2.1.311 reported them on 2026-10-27 for a third run
// of one session. Codex's turn.completed is the thread's total, too. Each
// coding agent's connection reports its session's totals, so MMHotline works
// out every run's own use the same way.
func TestClaudeCodeReportsItsSessionsTotals(t *testing.T) {
	stream := &agentStream{}
	line := `{"type":"result","subtype":"success","is_error":false,"result":"four","session_id":"c23dc8a8-0863-4f19-91ca-73a6a428e553","total_cost_usd":0.00555016,` +
		`"usage":{"input_tokens":2,"cache_creation_input_tokens":63,"cache_read_input_tokens":27617,"output_tokens":3},` +
		`"modelUsage":{"claude-haiku-5-5":{"inputTokens":6,"outputTokens":9,"cacheReadInputTokens":58106,"cacheCreationInputTokens":24820,"costUSD":0.00555016}},` +
		`"model_usage_since_last_result":{"claude-haiku-5-5":{"inputTokens":2,"outputTokens":3,"cacheReadInputTokens":27617,"cacheCreationInputTokens":63,"costUSD":0.00029047}}}`
	if _, err := (claudeCode{}).readLine([]byte(line), stream); err != nil {
		t.Fatal(err)
	}
	cost := 0.00555016
	want := Usage{CostInUSDollars: &cost, InputTokens: 6 + 58106 + 24820, CachedInputTokens: 58106, OutputTokens: 9}
	if stream.final == nil || stream.final.CodingAgentSessionTotalUsage == nil || !reflect.DeepEqual(*stream.final.CodingAgentSessionTotalUsage, want) {
		t.Fatalf("final: %+v, want the session's totals %+v", stream.final, want)
	}
}

// Codex sends error events for retries it recovers from, so only a turn that
// never completes fails with the error.
func TestCodexErrorEventFailsOnlyWithoutCompletedTurn(t *testing.T) {
	recovered := &agentStream{}
	for _, line := range []string{`{"type":"error","message":"Reconnecting... 1/5"}`, `{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":1}}`} {
		if _, err := (codex{}).readLine([]byte(line), recovered); err != nil {
			t.Fatal(err)
		}
	}
	if recovered.final == nil || recovered.final.Event != "finished" {
		t.Fatalf("recovered turn: %+v", recovered.final)
	}
	run := startFakeRun(t, "codex", `/bin/cat >/dev/null
`+codexSession+`
printf '%s\n' '{"type":"error","message":"stream disconnected before completion"}'`, testRequest("codex"))
	if event := run.next(t); event.Event != "session" {
		t.Fatalf("session: %+v", event)
	}
	if event := run.next(t); event.Event != "failed" || event.Reason != "stream disconnected before completion" {
		t.Fatalf("unfinished turn: %+v", event)
	}
	run.finish(t)
}

func TestCheckSetup(t *testing.T) {
	for _, test := range []struct{ agent, kind, credential, want string }{
		{"unknown", "api_key", "key", "claude_code, codex"},
		{"claude_code", "unknown", "key", "api_key, subscription_token"},
		{"codex", "unknown", "key", "valid values: api_key, access_token, chatgpt_sign_in"},
		{"codex", "subscription_sign_in", "", "valid values: api_key, access_token, chatgpt_sign_in"},
		{"claude_code", "api_key", "", "requires a credential"},
		{"claude_code", "subscription_token", "", "requires a credential"},
		{"codex", "api_key", "", "requires a credential"},
		{"codex", "api_key", "key\nvalue", "control characters"},
		{"codex", "api_key", "key\x00value", "control characters"},
		{"codex", "api_key", "key\u0085value", "control characters"},
		{"codex", "api_key", strings.Repeat("é", 2049), "4,096 bytes"},
		{"codex", "api_key", strings.Repeat("é", 2048), ""},
		{"claude_code", "subscription_token", "token", ""},
		{"codex", "access_token", "at-token", ""},
		{"codex", "access_token", "at-token\nsecond line", "control characters"},
		// A ChatGPT sign-in is the content of auth.json: several lines are fine.
		{"codex", "chatgpt_sign_in", indentedSignIn(t), ""},
		{"codex", "chatgpt_sign_in", "not json", "the whole content of Codex's auth.json"},
		{"codex", "chatgpt_sign_in", " ", "requires a credential"},
	} {
		err := CheckSetup(test.agent, test.kind, test.credential)
		if test.want == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s/%s: %v", test.agent, test.kind, err)
		}
	}
}

func TestCodingAgentsAndServiceHosts(t *testing.T) {
	want := []CodingAgentInfo{
		{"claude_code", "Claude Code", []CredentialTypeInfo{{"api_key", "API key"}, {"subscription_token", "Subscription token"}}},
		{"codex", "Codex", []CredentialTypeInfo{{"api_key", "API key"}, {"access_token", "Codex access token"}, {"chatgpt_sign_in", "ChatGPT sign-in"}}},
	}
	if !reflect.DeepEqual(CodingAgents(), want) {
		t.Fatalf("agents: %+v", CodingAgents())
	}
	for _, test := range []struct {
		agent, kind string
		hosts       []string
	}{
		{"claude_code", "api_key", []string{"api.anthropic.com"}},
		{"claude_code", "subscription_token", []string{"api.anthropic.com"}},
		{"codex", "api_key", []string{"api.openai.com"}},
		// A ChatGPT account reaches Codex through ChatGPT, and refreshes or
		// checks its token at OpenAI's sign-in service, never at OpenAI's API.
		{"codex", "access_token", []string{"auth.openai.com", "chatgpt.com"}},
		{"codex", "chatgpt_sign_in", []string{"auth.openai.com", "chatgpt.com"}},
		{"unknown", "api_key", nil}, {"codex", "unknown", nil},
	} {
		var hosts []string
		for _, host := range ServiceHosts(test.agent, test.kind) {
			if host.Port != 443 || host.ListedBecause == "" {
				t.Fatalf("host: %+v", host)
			}
			hosts = append(hosts, host.Host)
		}
		if !reflect.DeepEqual(hosts, test.hosts) {
			t.Fatalf("hosts: %v", hosts)
		}
	}
}

func TestStreamStepsAndUnknownEvents(t *testing.T) {
	for _, agent := range codingAgents() {
		stream := &agentStream{}
		if _, err := agent.readLine([]byte(`{"type":"future.event","message":"ignore me"}`), stream); err != nil {
			t.Fatal(err)
		}
	}
	stream := &agentStream{}
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"one","name":"Read","input":{"file_path":"/home/team/README.md"}},{"type":"tool_use","id":"two","name":"Grep","input":{"pattern":"failure"}}]}}`
	events, err := (claudeCode{}).readLine([]byte(line), stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Step.Text != "Read: /home/team/README.md" || events[1].Step.Text != "Grep: failure" {
		t.Fatalf("events: %+v", events)
	}
	_, err = (claudeCode{}).readLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"one"}]}}`), stream)
	if err != nil || len(stream.activeSteps) != 1 || !stream.activeSteps["two"] {
		t.Fatalf("active steps: %v, %v", stream.activeSteps, err)
	}
	content, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": strings.Repeat("界", 2001)}})
	events, err = (codex{}).readLine(content, &agentStream{})
	if err != nil {
		t.Fatal(err)
	}
	runes := []rune(events[0].Step.Text)
	if len(runes) != 2000 || runes[1999] != '…' {
		t.Fatalf("truncated text: %d runes", len(runes))
	}
	stream = &agentStream{}
	events, err = (codex{}).readLine([]byte(`{"type":"item.started","item":{"id":"think","type":"reasoning","text":"private"}}`), stream)
	if err != nil || len(events) != 0 || len(stream.activeSteps) != 0 {
		t.Fatalf("reasoning: %v %v", events, err)
	}
}
