// Package triageagent runs one triage agent run inside its sandbox. MMHotline
// and the run exchange JSON lines over the run socket:
// docs/changing-mmhotline/design/triage-agent.md#the-run-socket.
package triageagent

import "mmhotline/internal/systemsrepository"

// maximumLineSizeWithoutBundle bounds everything on one run socket line but a
// systems repository bundle and the run request's server list: the prompt,
// the credential, and the steps.
const maximumLineSizeWithoutBundle = 1 << 20

// MaximumInventoryServersSize bounds the run request's inventory_servers, as
// JSON, apart from the 1 MiB: at about 100 bytes a server, 1 MiB would stop
// runs near 8,000 servers, while 16 MiB lists about 160,000. That is far
// beyond the fleets MMHotline serves, and keeps the request line, which the
// sandbox reads whole into memory, bounded.
const MaximumInventoryServersSize = 16 << 20

// MaximumLineSize bounds one JSON line on the run socket: 1 MiB for the rest
// of the line, plus one systems repository bundle of at most 64 MiB in base64,
// plus the server list of a run request.
const MaximumLineSize = maximumLineSizeWithoutBundle + (systemsrepository.MaximumBundleSize+2)/3*4 + MaximumInventoryServersSize

// The agent a run is for: docs/changing-mmhotline/design/triage-agent.md#the-review-agent.
const (
	TriageAgentRole = "triage_agent"
	// ReviewAgentRole: the run starts the coding agent in a new empty home in
	// the run's private /tmp, because the triage agent can write the agent's
	// home and could plant instructions for the review agent there. It has no
	// systems repository, no SSH settings, and no requests.
	ReviewAgentRole = "review_agent"
)

// Request is the first line MMHotline sends.
type Request struct {
	Action    string `json:"action"`
	AgentRole string `json:"agent_role"`
	// NewEmptyHome, on a triage agent run, starts the coding agent in a new
	// empty home in the run's private /tmp, as every review agent run starts:
	// a commit review decides which commits MMX may run, so nothing a misled
	// earlier run planted in the agent's home may reach it.
	NewEmptyHome   bool   `json:"new_empty_home,omitempty"`
	CodingAgent    string `json:"coding_agent"`
	CredentialType string `json:"credential_type"`
	Credential     string `json:"credential,omitempty"`
	// Model and ReasoningEffort are the setup's; the sandbox passes them to
	// the coding agent on every run of that agent.
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	Prompt          string `json:"prompt"`
	SessionID       string `json:"session_id,omitempty"`
	// ProxyToken names the run on each connection to MMHotline's logging
	// proxy, as the connection's first line: a review run and the triage run
	// that waits for it share the proxy socket, and each has its own hosts.
	ProxyToken string `json:"proxy_token"`
	// A run in the agent's copy of the systems repository gets both: the
	// commit the copy starts at and a Git bundle of MMHotline's main branch,
	// base64 in JSON. The setup test has no repository and sends neither.
	SystemsRepositoryBaseCommitID string `json:"systems_repository_base_commit_id,omitempty"`
	SystemsRepositoryBundle       []byte `json:"systems_repository_bundle,omitempty"`
	// InventoryServers are the servers MMHotline listed from the inventories
	// of its copy at the run's start, which the agent reads over SSH.
	InventoryServers []InventoryServer `json:"inventory_servers,omitempty"`
}

// InventoryServer is one server of an inventory, which the agent reaches by
// its name through the logging proxy.
type InventoryServer struct {
	// ServerName is the inventory's name for the server, such as app001; the
	// team's server resolves it to the server's private address.
	ServerName string `json:"server_name"`
	// HostKeyAlias is the name the systems repository's ssh/known_hosts holds
	// the server's host key under: the inventory's address, where MMX
	// connects, such as 203.0.113.11.
	HostKeyAlias string `json:"host_key_alias"`
}

// Control is any later line MMHotline sends: {"stop":true} stops the run after
// its current step, and {"answer":{...}} answers a request the agent made.
type Control struct {
	Stop   bool    `json:"stop,omitempty"`
	Answer *Answer `json:"answer,omitempty"`
}

// Event is one line the run sends: session, step, the agent's requests,
// chatgpt_sign_in_changed, and one final finished, stopped, or failed. A request is playbook_run_requested,
// report, or commit_review, which MMHotline answers on a control line.
type Event struct {
	Event     string `json:"event"`
	SessionID string `json:"session_id,omitempty"`
	Step      *Step  `json:"step,omitempty"`
	Answer    string `json:"answer,omitempty"`
	// CodingAgentSessionTotalUsage, on finished, is what the coding agent
	// session has used so far, as the coding agent reports it: after a
	// resume, its earlier runs included. MMHotline works out the run's own.
	CodingAgentSessionTotalUsage *Usage `json:"coding_agent_session_total_usage,omitempty"`
	Reason                       string `json:"reason,omitempty"`
	// StoppedBeforeSessionID, on failed, says MMHotline's stop ended the run
	// before the coding agent reported its session, so nothing can resume
	// it. Every other stop ends with stopped.
	StoppedBeforeSessionID bool `json:"stopped_before_session_id,omitempty"`
	// AgentCommitsBundle is a Git bundle of the agent's commits after the
	// base, base64 in JSON, when the agent committed: on a playbook request
	// and on the final event of a run in the systems repository.
	AgentCommitsBundle []byte `json:"agent_commits_bundle,omitempty"`
	// RequestID, assigned by the run, names a request in MMHotline's answer.
	RequestID string `json:"request_id,omitempty"`
	// Playbook, Inventory, and the agent's Diagnosis are on
	// playbook_run_requested.
	Playbook  string `json:"playbook,omitempty"`
	Inventory string `json:"inventory,omitempty"`
	Diagnosis string `json:"diagnosis,omitempty"`
	// AlertSeverity and ReportText are on report and commit_review.
	AlertSeverity string `json:"alert_severity,omitempty"`
	ReportText    string `json:"report_text,omitempty"`
	// CommitID and CommitReviewDecision are on commit_review: the agent's
	// decision on a commit it reviewed, approve or needs_a_person.
	CommitID             string `json:"commit_id,omitempty"`
	CommitReviewDecision string `json:"commit_review_decision,omitempty"`
	// AboutAlertIDs are on report and playbook_run_requested: the alerts
	// the agent named with --about-alert-id.
	AboutAlertIDs []string `json:"about_alert_ids,omitempty"`
	// ChatGPTSignIn is on chatgpt_sign_in_changed: the content of Codex's
	// auth.json after Codex changed it, for MMHotline to check and keep.
	ChatGPTSignIn string `json:"chatgpt_sign_in,omitempty"`
}

// RequestOutcome is how MMHotline answered a request of the agent.
type RequestOutcome string

const (
	RanAndSucceeded RequestOutcome = "ran_and_succeeded"
	RanAndFailed    RequestOutcome = "ran_and_failed"
	// WaitsForAPerson: the playbook did not run; a person must approve the
	// agent's commits or the playbook run first.
	WaitsForAPerson RequestOutcome = "waits_for_a_person"
	// Refused: the request is invalid, MMHotline will not run the playbook,
	// or the run ended before MMHotline answered.
	Refused        RequestOutcome = "refused"
	ReportRecorded RequestOutcome = "recorded"
)

// Answer is MMHotline's answer to one request, which the run hands to the
// waiting command.
type Answer struct {
	RequestID string         `json:"request_id"`
	Outcome   RequestOutcome `json:"outcome"`
	// Message says what happened in plain words, for the agent to read.
	Message string `json:"message"`
	// CommitsOutcome is what MMHotline did with the agent's commits a playbook
	// request carried: pushed, waits_for_a_person, refused, or failed. It is
	// empty when the request carried none.
	CommitsOutcome string `json:"commits_outcome,omitempty"`
	// PlaybookRunOutput is the end of the playbook run's output, at most
	// MaximumPlaybookRunOutputForAgent bytes; MMHotline keeps the whole output.
	PlaybookRunOutput string `json:"playbook_run_output,omitempty"`
}

// ExitCode is the exit code of the command that made the request, as the
// commands' --help states.
func (answer Answer) ExitCode() int {
	switch answer.Outcome {
	case RanAndSucceeded, ReportRecorded:
		return 0
	case RanAndFailed:
		return 1
	case WaitsForAPerson:
		return 3
	case Refused:
		return 4
	}
	// An outcome this command does not know is not a success.
	return 4
}

// Text is the answer in plain words, as the command prints it.
func (answer Answer) Text() string {
	if answer.PlaybookRunOutput == "" {
		return answer.Message
	}
	return answer.Message + "\n\nPlaybook run output:\n" + answer.PlaybookRunOutput
}

// Step is one message, shell command, or other tool call of the coding agent.
type Step struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Usage is what a coding agent used: its cost in US dollars, which only
// Claude Code reports, and its tokens. InputTokens include CachedInputTokens,
// the input the model's service read from its cache, which costs less.
type Usage struct {
	CostInUSDollars   *float64 `json:"cost_in_us_dollars,omitempty"`
	InputTokens       int64    `json:"input_tokens,omitempty"`
	CachedInputTokens int64    `json:"cached_input_tokens,omitempty"`
	OutputTokens      int64    `json:"output_tokens,omitempty"`
}

// ListedHost is a host and port the logging proxy allows under "Listed hosts
// only", with the reason it is listed.
type ListedHost struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	ListedBecause string `json:"listed_because"`
}
