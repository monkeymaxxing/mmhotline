package triageagent

import "encoding/json"

type claudeCode struct{}

func (claudeCode) info() CodingAgentInfo {
	return CodingAgentInfo{Name: "claude_code", Title: "Claude Code", CredentialTypes: []CredentialTypeInfo{
		{Name: "api_key", Title: "API key"}, {Name: "subscription_token", Title: "Subscription token"},
	}}
}

func (claudeCode) command(request Request) []string {
	args := []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--model", request.Model, "--effort", request.ReasoningEffort}
	if request.Action == "resume" {
		args = append(args, "--resume", request.SessionID)
	}
	return args
}

func (claudeCode) env(request Request, _ string) []string {
	values := []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	if request.CredentialType == "api_key" {
		return append(values, "ANTHROPIC_API_KEY="+request.Credential)
	}
	return append(values, "CLAUDE_CODE_OAUTH_TOKEN="+request.Credential)
}

func (claudeCode) serviceHosts(string) []ListedHost {
	return []ListedHost{{Host: "api.anthropic.com", Port: 443, ListedBecause: "Claude Code's model API"}}
}

func (claudeCode) readLine(line []byte, stream *agentStream) ([]Event, error) {
	var record struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		// After claude --resume, total_cost_usd and modelUsage are the
		// session's totals so far, earlier runs included, while usage is the
		// run's own; the totals are used, as Codex reports only totals. A
		// model's input is inputTokens, cache reads, and cache writes.
		TotalCost  *float64 `json:"total_cost_usd"`
		ModelUsage map[string]struct {
			Input         int64 `json:"inputTokens"`
			CacheCreation int64 `json:"cacheCreationInputTokens"`
			CacheRead     int64 `json:"cacheReadInputTokens"`
			Output        int64 `json:"outputTokens"`
		} `json:"modelUsage"`
		Message struct {
			Content []struct {
				Type      string                     `json:"type"`
				ID        string                     `json:"id"`
				ToolUseID string                     `json:"tool_use_id"`
				Name      string                     `json:"name"`
				Text      string                     `json:"text"`
				Input     map[string]json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	// Only decode message content for the event types that own it. Other events
	// may use a different message shape, including future agent output.
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		return nil, err
	}
	if header.Type != "system" && header.Type != "assistant" && header.Type != "user" && header.Type != "result" {
		return nil, nil
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, err
	}
	var events []Event
	switch record.Type {
	case "system":
		if record.Subtype == "init" && stream.sessionID == "" && record.SessionID != "" {
			stream.sessionID = record.SessionID
			events = append(events, Event{Event: "session", SessionID: record.SessionID})
		}
	case "assistant":
		for _, content := range record.Message.Content {
			switch content.Type {
			case "text":
				if content.Text == "" {
					continue
				}
				stream.answer = content.Text
				events = append(events, stepEvent("message", content.Text))
			case "tool_use":
				kind, text := "tool", toolText(content.Name, content.Input)
				if content.Name == "Bash" {
					kind = "command"
					_ = json.Unmarshal(content.Input["command"], &text)
				}
				events = append(events, stream.startStep(content.ID, kind, text))
			}
		}
	case "user":
		for _, content := range record.Message.Content {
			if content.Type == "tool_result" {
				delete(stream.activeSteps, content.ToolUseID)
			}
		}
	case "result":
		reason := record.Result
		if reason == "" {
			reason = "Claude Code ended with " + record.Subtype
		}
		final := Event{Event: "failed", Reason: reason}
		if record.Subtype == "success" && !record.IsError {
			total := Usage{CostInUSDollars: record.TotalCost}
			for _, model := range record.ModelUsage {
				total.InputTokens += model.Input + model.CacheCreation + model.CacheRead
				total.CachedInputTokens += model.CacheRead
				total.OutputTokens += model.Output
			}
			final = Event{Event: "finished", Answer: record.Result, CodingAgentSessionTotalUsage: &total}
		}
		stream.final = &final
	}
	return events, nil
}
