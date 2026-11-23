package triageagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// CredentialTypeInfo names one kind of credential a coding agent accepts.
type CredentialTypeInfo struct {
	Name  string
	Title string
}

// CodingAgentInfo names a coding agent and the credential types it accepts.
type CodingAgentInfo struct {
	Name            string
	Title           string
	CredentialTypes []CredentialTypeInfo
}

// codingAgent is the connection to one coding agent: the one file that
// support for another coding agent adds.
type codingAgent interface {
	info() CodingAgentInfo
	command(Request) []string
	// env is the coding agent's own variables; home is the run's home.
	env(request Request, home string) []string
	serviceHosts(string) []ListedHost
	readLine([]byte, *agentStream) ([]Event, error)
	// sessionDirectory is where the coding agent keeps its session files,
	// relative to the agent's home. It is the only part of the coding
	// agent's own files that a run in the agent's home keeps, because the
	// next run of a triage session resumes its session from it.
	sessionDirectory() string
}

func codingAgents() []codingAgent { return []codingAgent{claudeCode{}, codex{}} }

// CodingAgents lists the supported coding agents for pages and commands.
func CodingAgents() []CodingAgentInfo {
	var result []CodingAgentInfo
	for _, agent := range codingAgents() {
		result = append(result, agent.info())
	}
	return result
}

func findCodingAgent(name string) codingAgent {
	for _, agent := range codingAgents() {
		if agent.info().Name == name {
			return agent
		}
	}
	return nil
}

// CheckSetup refuses a coding agent, credential type, or credential that no
// run could use. A credential that goes into an environment variable is one
// line; a ChatGPT sign-in is the content of Codex's auth.json.
func CheckSetup(codingAgent, credentialType, credential string) error {
	agent := findCodingAgent(codingAgent)
	if agent == nil {
		return fmt.Errorf("unknown coding agent %q; valid values: claude_code, codex", codingAgent)
	}
	var validTypes []string
	valid := false
	for _, info := range agent.info().CredentialTypes {
		validTypes = append(validTypes, info.Name)
		valid = valid || info.Name == credentialType
	}
	if !valid {
		return fmt.Errorf("unknown credential type %q for %s; valid values: %s", credentialType, agent.info().Title, strings.Join(validTypes, ", "))
	}
	if strings.TrimSpace(credential) == "" {
		return fmt.Errorf("%s requires a credential", credentialType)
	}
	if credentialType == "chatgpt_sign_in" {
		if _, err := parseChatGPTSignIn(credential); err != nil {
			return fmt.Errorf("credential must be the whole content of Codex's auth.json after codex login, but it %w", err)
		}
		return nil
	}
	if len(credential) > 4096 {
		return fmt.Errorf("credential must be at most 4,096 bytes")
	}
	if strings.ContainsFunc(credential, unicode.IsControl) {
		return fmt.Errorf("credential must be one line without control characters")
	}
	return nil
}

// modelPattern is the shape of a model name. The vendors name models with
// letters, digits, and punctuation, and change the names every few weeks,
// so MMHotline checks only the shape, and the agent's setup test proves the
// name (CA6). The first character is a letter or digit, so the name, its
// own argument after --model or -m, is never read as another flag.
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,199}$`)

// reasoningEffortPattern is the shape of a reasoning effort, one lowercase
// word such as high; each coding agent and model has its own list (CA6).
// Letters only, so Codex's TOML value model_reasoning_effort="LEVEL" never
// holds a quote.
var reasoningEffortPattern = regexp.MustCompile(`^[a-z]{1,32}$`)

// CheckModel refuses a model name without the shape of one.
func CheckModel(model string) error {
	if !modelPattern.MatchString(model) {
		return errors.New("must be a model name of at most 200 characters without spaces, such as opus; the coding agent's setup test checks whether the service recognizes the model name")
	}
	return nil
}

// CheckReasoningEffort refuses a reasoning effort that is not one lowercase
// word.
func CheckReasoningEffort(effort string) error {
	if !reasoningEffortPattern.MatchString(effort) {
		return errors.New("must be a lowercase word, such as high; the coding agent's setup test checks whether the model supports the reasoning effort")
	}
	return nil
}

// ServiceHosts lists the hosts a coding agent needs for its own service; the
// logging proxy lists them for runs of that coding agent.
func ServiceHosts(codingAgent, credentialType string) []ListedHost {
	agent := findCodingAgent(codingAgent)
	if agent == nil {
		return nil
	}
	for _, info := range agent.info().CredentialTypes {
		if info.Name == credentialType {
			return agent.serviceHosts(credentialType)
		}
	}
	return nil
}

// agentStream is what one run has read from its coding agent so far.
type agentStream struct {
	sessionID   string
	activeSteps map[string]bool // steps started and not yet ended, by the agent's ID
	answer      string
	final       *Event
	lastError   string
}

func (stream *agentStream) startStep(id, kind, text string) Event {
	if stream.activeSteps == nil {
		stream.activeSteps = make(map[string]bool)
	}
	stream.activeSteps[id] = true
	return stepEvent(kind, text)
}

func stepEvent(kind, text string) Event {
	runes := []rune(text)
	if len(runes) > 2000 {
		text = string(runes[:1999]) + "…"
	}
	return Event{Event: "step", Step: &Step{Kind: kind, Text: text}}
}

func tellingField(fields map[string]json.RawMessage) string {
	for _, name := range []string{"file_path", "path", "pattern", "url", "query", "command", "name", "prompt"} {
		var value string
		if json.Unmarshal(fields[name], &value) == nil && value != "" {
			return value
		}
	}
	return ""
}

func toolText(name string, fields map[string]json.RawMessage) string {
	if value := tellingField(fields); value != "" {
		return name + ": " + value
	}
	return name
}
