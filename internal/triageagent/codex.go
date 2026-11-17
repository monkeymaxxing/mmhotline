package triageagent

import (
	"encoding/json"
	"path/filepath"
)

type codex struct{}

// Codex takes an API key, a Codex access token (ChatGPT Business and
// Enterprise), or a ChatGPT sign-in: the content of auth.json after codex
// login, which the person makes on their own computer, so nobody signs in
// by hand on the server (docs/changing-mmhotline/design/triage-agent.md#triage-agent-setup).
func (codex) info() CodingAgentInfo {
	return CodingAgentInfo{Name: "codex", Title: "Codex", CredentialTypes: []CredentialTypeInfo{
		{Name: "api_key", Title: "API key"}, {Name: "access_token", Title: "Codex access token"}, {Name: "chatgpt_sign_in", Title: "ChatGPT sign-in"},
	}}
}

// command passes the model with -m, and the reasoning effort as Codex's
// configuration value model_reasoning_effort, a TOML string. With a ChatGPT
// sign-in, cli_auth_credentials_store="file" keeps the tokens in the
// auth.json the run reads back; the flag wins over a config.toml the agent
// may have written in its home.
func (codex) command(request Request) []string {
	args := []string{"codex", "exec"}
	if request.Action == "resume" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-m", request.Model, "-c", `model_reasoning_effort="`+request.ReasoningEffort+`"`)
	if request.CredentialType == "chatgpt_sign_in" {
		args = append(args, "-c", `cli_auth_credentials_store="file"`)
	}
	if request.Action == "resume" {
		args = append(args, request.SessionID)
	}
	return append(args, "-")
}

// env gives Codex its credential: a ChatGPT sign-in is the file
// $CODEX_HOME/auth.json, which the run writes; the others are a variable.
func (codex) env(request Request, home string) []string {
	switch request.CredentialType {
	case "access_token":
		return []string{"CODEX_ACCESS_TOKEN=" + request.Credential}
	case "chatgpt_sign_in":
		return []string{"CODEX_HOME=" + filepath.Join(home, ".codex")}
	}
	return []string{"CODEX_API_KEY=" + request.Credential}
}

// plantablePaths: config.toml can add MCP servers, a notify command, and
// instructions; MMHotline passes every setting Codex needs with -c. Session
// files in ~/.codex/sessions/ stay for resume.
func (codex) plantablePaths() []string {
	return []string{".codex/AGENTS.md", ".codex/AGENTS.override.md", ".codex/config.toml", ".codex/hooks.json", ".codex/prompts", ".codex/skills", ".codex/rules"}
}

// serviceHosts: with an API key Codex calls OpenAI's API. With a ChatGPT
// sign-in or a Codex access token it calls ChatGPT's Codex service, and
// OpenAI's sign-in service, where it refreshes a sign-in and checks an
// access token (openai/codex rust-v0.166.0: model-provider-info/src/lib.rs,
// login/src/auth/manager.rs REFRESH_TOKEN_URL, personal_access_token.rs).
func (codex) serviceHosts(credentialType string) []ListedHost {
	if credentialType == "api_key" {
		return []ListedHost{{Host: "api.openai.com", Port: 443, ListedBecause: "Codex's model API"}}
	}
	return []ListedHost{
		{Host: "auth.openai.com", Port: 443, ListedBecause: "OpenAI's sign-in service, where Codex refreshes its ChatGPT sign-in or checks its access token"},
		{Host: "chatgpt.com", Port: 443, ListedBecause: "Codex's model service for a ChatGPT account"},
	}
}

func (codex) readLine(line []byte, stream *agentStream) ([]Event, error) {
	var record struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Error    struct {
			Message string `json:"message"`
		} `json:"error"`
		Item map[string]json.RawMessage `json:"item"`
		// turn.completed's usage is the thread's total so far: after codex
		// exec resume, the earlier runs' tokens are included. input_tokens
		// includes cached_input_tokens.
		Usage struct {
			Input       int64 `json:"input_tokens"`
			CachedInput int64 `json:"cached_input_tokens"`
			Output      int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, err
	}
	switch record.Type {
	case "thread.started":
		if stream.sessionID == "" && record.ThreadID != "" {
			stream.sessionID = record.ThreadID
			return []Event{{Event: "session", SessionID: record.ThreadID}}, nil
		}
	case "item.started", "item.completed":
		var id, itemType, text string
		_ = json.Unmarshal(record.Item["id"], &id)
		_ = json.Unmarshal(record.Item["type"], &itemType)
		if itemType == "reasoning" {
			return nil, nil
		}
		if record.Type == "item.completed" {
			delete(stream.activeSteps, id)
			if itemType == "agent_message" {
				_ = json.Unmarshal(record.Item["text"], &text)
				stream.answer = text
				return []Event{stepEvent("message", text)}, nil
			}
			// Codex reports problems it works around, such as a missing helper
			// program, as error items; show them, since the run goes on.
			if itemType == "error" {
				_ = json.Unmarshal(record.Item["message"], &text)
				return []Event{stepEvent("message", "Codex reported: "+text)}, nil
			}
		} else {
			kind, text := "tool", toolText(itemType, record.Item)
			if itemType == "command_execution" {
				kind = "command"
				_ = json.Unmarshal(record.Item["command"], &text)
			}
			return []Event{stream.startStep(id, kind, text)}, nil
		}
	case "turn.completed":
		stream.final = &Event{Event: "finished", Answer: stream.answer, CodingAgentSessionTotalUsage: &Usage{InputTokens: record.Usage.Input, CachedInputTokens: record.Usage.CachedInput, OutputTokens: record.Usage.Output}}
	case "turn.failed":
		stream.final = &Event{Event: "failed", Reason: codexErrorText(record.Error.Message, record.Message)}
	case "error":
		// An error event can be a retry that the turn recovers from, so only
		// a turn that never completes fails with it.
		stream.lastError = codexErrorText(record.Error.Message, record.Message)
	}
	return nil, nil
}

func codexErrorText(errorMessage, message string) string {
	if errorMessage != "" {
		return errorMessage
	}
	return message
}
