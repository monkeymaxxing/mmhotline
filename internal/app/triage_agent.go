package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mmhotline/internal/triageagent"
)

const triageAgentSchema = `
CREATE TABLE triage_agent_setup (
 id INTEGER PRIMARY KEY CHECK(id=1), coding_agent TEXT NOT NULL,
 credential_type TEXT NOT NULL, credential TEXT NOT NULL,
 model TEXT NOT NULL, reasoning_effort TEXT NOT NULL,
 set_up_at INTEGER NOT NULL, set_up_by TEXT NOT NULL,
 ` + credentialColumns + `,
 ` + credentialCheckColumns + `,
 ` + runLimitNoticeColumn + `,
 ` + runLimitNoticeChangedAtColumn + `
);
CREATE TABLE triage_agent_internet_access (
 id INTEGER PRIMARY KEY CHECK(id=1), internet_access TEXT NOT NULL CHECK(internet_access IN ('listed_hosts_only','any_site')),
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL
);
CREATE TABLE triage_agent_listed_hosts (
 host TEXT NOT NULL, port INTEGER NOT NULL, added_at INTEGER NOT NULL, added_by TEXT NOT NULL,
 PRIMARY KEY(host, port)
);
CREATE TABLE triage_agent_inventory_servers (
 server_name TEXT NOT NULL, inventory TEXT NOT NULL, PRIMARY KEY(server_name, inventory)
);
CREATE TABLE triage_agent_runs (
 id TEXT PRIMARY KEY, purpose TEXT NOT NULL CHECK(purpose IN ('setup_test','sign_in_check','triage_session','software_review','commit_review','review_agent_setup_test','review_agent_sign_in_check','playbook_review')),
 coding_agent TEXT NOT NULL, credential_type TEXT NOT NULL, coding_agent_session_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('running','finished','stopped','failed')),
 started_at INTEGER NOT NULL, started_by TEXT NOT NULL, ended_at INTEGER NOT NULL DEFAULT 0,
 answer TEXT NOT NULL DEFAULT '', failure_reason TEXT NOT NULL DEFAULT '', cost_in_us_dollars REAL,
 input_tokens INTEGER NOT NULL DEFAULT 0, cached_input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 coding_agent_session_total_cost_in_us_dollars REAL,
 coding_agent_session_total_input_tokens INTEGER NOT NULL DEFAULT 0,
 coding_agent_session_total_cached_input_tokens INTEGER NOT NULL DEFAULT 0,
 coding_agent_session_total_output_tokens INTEGER NOT NULL DEFAULT 0,
 triage_session_id TEXT NOT NULL DEFAULT '',
 inventory_servers_not_listed_because TEXT NOT NULL DEFAULT '',
 vendor_page_hosts_not_listed_because TEXT NOT NULL DEFAULT '',
 ` + runEndCauseColumn + `
);
` + triageAgentRunsStartedAtIndex + `
CREATE TABLE triage_agent_commits (
 run_id TEXT NOT NULL REFERENCES triage_agent_runs(id), request_id TEXT NOT NULL,
 base_commit_id TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('pushed','waits_for_a_person','refused','failed')),
 reason TEXT NOT NULL DEFAULT '', agent_commit_id TEXT NOT NULL DEFAULT '',
 pushed_commit_id TEXT NOT NULL DEFAULT '', private_ref TEXT NOT NULL DEFAULT '',
 working_tree_not_updated_because TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(run_id, request_id)
);
CREATE TABLE triage_agent_requests (
 run_id TEXT NOT NULL REFERENCES triage_agent_runs(id), request_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('playbook_run','report','commit_review')),
 playbook TEXT NOT NULL DEFAULT '', inventory TEXT NOT NULL DEFAULT '',
 alert_severity TEXT NOT NULL DEFAULT '', report_text TEXT NOT NULL DEFAULT '',
 requested_at INTEGER NOT NULL,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','ran_and_succeeded','ran_and_failed','waits_for_a_person','refused','recorded')),
 message TEXT NOT NULL DEFAULT '', answered_at INTEGER NOT NULL DEFAULT 0,
 playbook_read_at_commit_id TEXT NOT NULL DEFAULT '',
 playbook_run_started_at INTEGER NOT NULL DEFAULT 0,
 playbook_run_waiting_for_another_run_since INTEGER NOT NULL DEFAULT 0,
 playbook_run_ended_at INTEGER NOT NULL DEFAULT 0, playbook_run_exit_code INTEGER,
 playbook_run_output TEXT NOT NULL DEFAULT '',
 diagnosis TEXT NOT NULL DEFAULT '',
 commit_id TEXT NOT NULL DEFAULT '',
 commit_review_decision TEXT NOT NULL DEFAULT '' CHECK(commit_review_decision IN ('','approve','needs_a_person')),
 PRIMARY KEY(run_id, request_id)
);
CREATE TABLE triage_agent_run_steps (
 run_id TEXT NOT NULL REFERENCES triage_agent_runs(id), sequence INTEGER NOT NULL,
 occurred_at INTEGER NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('message','command','tool')),
 text TEXT NOT NULL, PRIMARY KEY(run_id, sequence)
);
CREATE TABLE triage_agent_proxy_connections (
 id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, started_at INTEGER NOT NULL,
 host TEXT NOT NULL, port INTEGER NOT NULL, address TEXT NOT NULL,
 decision TEXT NOT NULL CHECK(decision IN ('allowed','refused')), refusal_reason TEXT NOT NULL DEFAULT '',
 bytes_sent INTEGER NOT NULL DEFAULT 0, bytes_received INTEGER NOT NULL DEFAULT 0, ended_at INTEGER NOT NULL DEFAULT 0
);
`

const TriageAgentSandboxNotInstalled = "The triage agent sandbox is not installed on the MMHotline server. Run sudo mmhotline install on the team's Linux server."
const triageAgentTestSentence = "MMHotline triage agent test passed"
const triageAgentSetupTestPrompt = "This is MMHotline's setup test for its triage agent. Reply with exactly this sentence and nothing else: MMHotline triage agent test passed."

// setupTestSentence is the sentence a setup test of this purpose asks for.
func setupTestSentence(purpose string) string {
	if reviewAgentPurpose(purpose) {
		return reviewAgentTestSentence
	}
	return triageAgentTestSentence
}

// TriageAgentSetup deliberately has no credential field. The review agent's
// setup has the same fields.
type TriageAgentSetup struct {
	CodingAgent     string `json:"coding_agent"`
	CredentialType  string `json:"credential_type"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	// SetUpAt is when the setup's credential applied; a change of only the
	// model or the reasoning effort keeps it.
	SetUpAt time.Time `json:"set_up_at"`
	SetUpBy string    `json:"set_up_by"`
	// CredentialExpiresAt is a subscription token's end, a year after it
	// applied, or the start of a Codex access token's credential_expires_on;
	// absent for a credential whose end MMHotline does not know.
	CredentialExpiresAt *time.Time `json:"credential_expires_at,omitempty"`
	// The sign-in, from the agent's latest run that succeeded or its latest
	// sign-in check that failed, whichever ended later; the set-up counts.
	SignInLastCheckedAt time.Time `json:"sign_in_last_checked_at"`
	SignInWorks         bool      `json:"sign_in_works"`
	SignInFailureReason string    `json:"sign_in_failure_reason"`
}

// CodingAgentTitle names the setup's coding agent for people, such as
// "Claude Code".
func (setup TriageAgentSetup) CodingAgentTitle() string {
	return codingAgentTitle(setup.CodingAgent)
}

// credentialLines are the setup's sign-in and its credential's expiry, as
// the show commands print them.
func (setup TriageAgentSetup) credentialLines() []string {
	signIn := fmt.Sprintf("Sign-in to %s: works; last checked at %s.", setup.CodingAgentTitle(), commandTime(setup.SignInLastCheckedAt))
	if !setup.SignInWorks {
		signIn = fmt.Sprintf("Sign-in to %s: fails; last checked at %s: %s", setup.CodingAgentTitle(), commandTime(setup.SignInLastCheckedAt), setup.SignInFailureReason)
	}
	lines := []string{signIn}
	if setup.CredentialExpiresAt != nil && setup.CredentialType == "access_token" {
		lines = append(lines, "Codex access token expires on: "+setup.CredentialExpiresAt.Format(credentialExpiresOnLayout))
	} else if setup.CredentialExpiresAt != nil {
		lines = append(lines, "Subscription token expires at: "+commandTime(*setup.CredentialExpiresAt))
	}
	return lines
}

// triageAgentCredentialPhrases name each credential type inside a sentence.
var triageAgentCredentialPhrases = map[string]string{"api_key": "an API key", "subscription_token": "a subscription token", "access_token": "a Codex access token", "chatgpt_sign_in": "a ChatGPT sign-in"}

// Title names the setup for people, such as "Claude Code with an API key,
// model opus, reasoning effort high".
func (setup TriageAgentSetup) Title() string {
	return setup.CodingAgentTitle() + " with " + triageAgentCredentialPhrases[setup.CredentialType] + ", model " + setup.Model + ", reasoning effort " + setup.ReasoningEffort
}

type TriageAgentRunStep struct {
	Sequence   int       `json:"sequence"`
	OccurredAt time.Time `json:"occurred_at"`
	Kind       string    `json:"kind"`
	Text       string    `json:"text"`
}
type TriageAgentRun struct {
	ID                   string               `json:"id"`
	Purpose              string               `json:"purpose"`
	CodingAgent          string               `json:"coding_agent"`
	CredentialType       string               `json:"credential_type"`
	CodingAgentSessionID string               `json:"coding_agent_session_id"`
	State                string               `json:"state"`
	StartedAt            time.Time            `json:"started_at"`
	StartedBy            string               `json:"started_by"`
	EndedAt              *time.Time           `json:"ended_at,omitempty"`
	Answer               string               `json:"answer"`
	FailureReason        string               `json:"failure_reason"`
	CostInUSDollars      *float64             `json:"cost_in_us_dollars,omitempty"`
	InputTokens          int64                `json:"input_tokens"`
	CachedInputTokens    int64                `json:"cached_input_tokens"`
	OutputTokens         int64                `json:"output_tokens"`
	Steps                []TriageAgentRunStep `json:"steps"`
}

func (run TriageAgentRun) UsageText() string {
	return usageText(run.CostInUSDollars, run.InputTokens, run.CachedInputTokens, run.OutputTokens)
}

// usageText is a run's cost, which Claude Code reports, or else its tokens,
// which Codex reports.
func usageText(costInUSDollars *float64, inputTokens, cachedInputTokens, outputTokens int64) string {
	if costInUSDollars != nil {
		return fmt.Sprintf("Cost in US dollars: %.6f", *costInUSDollars)
	}
	return tokensText(inputTokens, cachedInputTokens, outputTokens)
}

// tokensText is a run's token use. Cached input tokens are part of the input
// and cost less, so they are named beside it.
func tokensText(inputTokens, cachedInputTokens, outputTokens int64) string {
	return fmt.Sprintf("Input tokens: %d, of which %d cached; output tokens: %d.", inputTokens, cachedInputTokens, outputTokens)
}

// Passed says a setup test or sign-in check passed. finishTriageAgentRun
// decides it once and records a failure reason for a run that did not, such
// as an answer without the test sentence.
func (run TriageAgentRun) Passed() bool {
	return run.State == "finished" && run.FailureReason == ""
}
func (run TriageAgentRun) Status() string {
	if run.State == "running" {
		return "running"
	}
	if run.Passed() {
		return "passed"
	}
	return "failed"
}

type TriageAgentListedHost struct {
	triageagent.ListedHost
	AddedBy string `json:"added_by,omitempty"`
}
type TriageAgentProxyConnection struct {
	ID               int64      `json:"id"`
	RunID            string     `json:"run_id"`
	StartedAt        time.Time  `json:"started_at"`
	Host             string     `json:"host"`
	Port             int        `json:"port"`
	Address          string     `json:"address"`
	Decision         string     `json:"decision"`
	RefusalReason    string     `json:"refusal_reason"`
	BytesSent        int64      `json:"bytes_sent"`
	BytesReceived    int64      `json:"bytes_received"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	CanAddListedHost bool       `json:"can_add_listed_host"`
}
type TriageAgentView struct {
	SandboxInstalled bool                    `json:"sandbox_installed"`
	Setup            *TriageAgentSetup       `json:"setup,omitempty"`
	InternetAccess   string                  `json:"internet_access"`
	ListedHosts      []TriageAgentListedHost `json:"listed_hosts"`
	// CommitSigningPublicKey is the applied triage_agent.commit_signing_public_key,
	// a public key; absent while none is applied, such as without the
	// systems_repository_commits section. sudo mmhotline triage-agent
	// check-preparation compares it with the agent's own git settings.
	CommitSigningPublicKey string `json:"commit_signing_public_key,omitempty"`
	// TrustsEveryCommitOnOrigin is true when the applied
	// systems_repository_commits.trusted_commits is every_commit_on_origin:
	// no commit signing key is applied then, and none is needed.
	TrustsEveryCommitOnOrigin bool `json:"trusts_every_commit_on_origin,omitempty"`
	// InventoryServersNotListedBecause says why the latest run in the systems
	// repository could not list every inventory server, if it could not.
	InventoryServersNotListedBecause string `json:"inventory_servers_not_listed_because,omitempty"`
	// VendorPageHostsNotListedBecause says why the latest run in the systems
	// repository left out a vendor page's host, if it did.
	VendorPageHostsNotListedBecause string `json:"vendor_page_hosts_not_listed_because,omitempty"`
	// HostsFromTheSystemsRepositoryListedAt is when the latest run in the
	// systems repository started and listed the inventory servers and the
	// vendor pages' hosts; a server or a page removed since stays listed
	// until the next run.
	HostsFromTheSystemsRepositoryListedAt *time.Time      `json:"hosts_from_the_systems_repository_listed_at,omitempty"`
	LatestSetupTest                       *TriageAgentRun `json:"latest_setup_test,omitempty"`
	LatestSignInCheck                     *TriageAgentRun `json:"latest_sign_in_check,omitempty"`
	LatestSoftwareReview                  *ReviewRun      `json:"latest_software_review,omitempty"`
	// LatestCommitReview is the latest run that reviewed commits no trusted
	// key signed.
	LatestCommitReview *ReviewRun `json:"latest_commit_review,omitempty"`
	// The scheduled software review: off, or its interval. The interval's
	// change is absent while the settings file has not set it; the next due
	// time is absent while the review is off, and while no review that
	// counts has started, as the next is then due at once.
	SoftwareReviewIntervalHours     SoftwareReviewIntervalHours  `json:"software_review_interval_hours"`
	SoftwareReviewIntervalChangedAt *time.Time                   `json:"software_review_interval_changed_at,omitempty"`
	SoftwareReviewIntervalChangedBy string                       `json:"software_review_interval_changed_by,omitempty"`
	NextSoftwareReviewDueAt         *time.Time                   `json:"next_software_review_due_at,omitempty"`
	// SoftwareReviewSkippedForTheRunLimitAt is when a scheduled software
	// review was last skipped because the run limit held, absent when none
	// was skipped since the latest review started.
	SoftwareReviewSkippedForTheRunLimitAt *time.Time `json:"software_review_skipped_for_the_run_limit_at,omitempty"`
	// RunLimit is the last 24 hours against the run limit, absent without
	// the setup.
	RunLimit *TriageAgentRunLimitView `json:"run_limit,omitempty"`
	RecentConnections               []TriageAgentProxyConnection `json:"recent_connections"`
}

// ScheduledSoftwareReviewIsOff says that only a person starts a software
// review.
func (view TriageAgentView) ScheduledSoftwareReviewIsOff() bool {
	return view.SoftwareReviewIntervalHours == softwareReviewIntervalOff
}

func (a *App) SetTriageAgentInternetAccess(ctx context.Context, p Principal, value string) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		person, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		if value != "listed_hosts_only" && value != "any_site" {
			return fmt.Errorf("%w: internet_access must be listed_hosts_only or any_site", ErrInvalid)
		}
		_, err = tx.ExecContext(ctx, `UPDATE triage_agent_internet_access SET internet_access=?,changed_at=?,changed_by=? WHERE id=1`, value, a.Now().Unix(), person.Name)
		return err
	})
}

var triageAgentDNSName = regexp.MustCompile(`^[a-z0-9.-]+$`)

func triageAgentHost(host string, port int) (string, error) {
	host = strings.ToLower(host)
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("%w: port must be 1 to 65535", ErrInvalid)
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if reason := triageAgentForbiddenAddress(address); reason != "" {
			return "", fmt.Errorf("%w: %s", ErrInvalid, reason)
		}
		return address.Unmap().String(), nil
	}
	if len(host) == 0 || len(host) > 253 || !triageAgentDNSName.MatchString(host) {
		return "", fmt.Errorf("%w: host must be a DNS name or an IP address", ErrInvalid)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("%w: host must be a DNS name or an IP address", ErrInvalid)
		}
	}
	return host, nil
}

// SetTriageAgentListedHosts makes the listed hosts exactly hosts; a host
// listed before keeps when and by whom it was added.
func (a *App) SetTriageAgentListedHosts(ctx context.Context, p Principal, hosts []SettingsListedHost) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		person, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, listed := range hosts {
			host, err := triageAgentHost(listed.Host, listed.Port)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO triage_agent_listed_hosts(host,port,added_at,added_by) VALUES(?,?,?,?) ON CONFLICT(host,port) DO NOTHING`, host, listed.Port, a.Now().Unix(), person.Name); err != nil {
				return err
			}
			keep[net.JoinHostPort(host, strconv.Itoa(listed.Port))] = true
		}
		rows, err := tx.QueryContext(ctx, `SELECT host,port FROM triage_agent_listed_hosts`)
		if err != nil {
			return err
		}
		var removed []TriageAgentListedHost
		for rows.Next() {
			var host TriageAgentListedHost
			if err = rows.Scan(&host.Host, &host.Port); err != nil {
				rows.Close()
				return err
			}
			if !keep[net.JoinHostPort(host.Host, strconv.Itoa(host.Port))] {
				removed = append(removed, host)
			}
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, host := range removed {
			if _, err = tx.ExecContext(ctx, `DELETE FROM triage_agent_listed_hosts WHERE host=? AND port=?`, host.Host, host.Port); err != nil {
				return err
			}
		}
		return nil
	})
}

// inventoryServerHost is an inventory server as a listed host, with the
// inventories it is in, such as "the inventory production".
type inventoryServerHost struct {
	TriageAgentListedHost
	inventories string
}

// listedInventoryServers lists each server that the latest run in the
// systems repository found in the inventories, at port 22, by name.
func listedInventoryServers(ctx context.Context, tx *sql.Tx) ([]inventoryServerHost, error) {
	rows, err := tx.QueryContext(ctx, `SELECT server_name,inventory FROM triage_agent_inventory_servers ORDER BY server_name,inventory`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	inventories := map[string][]string{}
	for rows.Next() {
		var name, inventory string
		if err := rows.Scan(&name, &inventory); err != nil {
			return nil, err
		}
		if inventories[name] == nil {
			names = append(names, name)
		}
		inventories[name] = append(inventories[name], inventory)
	}
	var servers []inventoryServerHost
	for _, name := range names {
		in := inventoryText(inventories[name][0]) //nolint:nilaway // Each name in names has at least one inventory.
		if count := len(inventories[name]); count > 1 {
			in = "the inventories “" + strings.Join(inventories[name][:count-1], "”, “") + "” and “" + inventories[name][count-1] + "”"
		}
		servers = append(servers, inventoryServerHost{TriageAgentListedHost{ListedHost: triageagent.ListedHost{Host: name, Port: inventoryServerSSHPort, ListedBecause: "A server in " + in}}, in})
	}
	return servers, rows.Err()
}

// triageAgentHosts are the listed hosts of a triage agent run of
// codingAgent: its service, OSV.dev's API, the host of CISA's list of known
// exploited vulnerabilities, the settings file's hosts, and,
// withSystemsRepositoryHosts, the inventory servers and the vendor pages'
// hosts that the latest run in the systems repository listed. Only a run in
// the systems repository reaches those, because only it has the repository;
// the page shows them all.
func triageAgentHosts(ctx context.Context, tx *sql.Tx, codingAgent, credentialType string, withSystemsRepositoryHosts bool) ([]TriageAgentListedHost, error) {
	hosts := []TriageAgentListedHost{
		{ListedHost: triageagent.ListedHost{Host: "api.osv.dev", Port: 443, ListedBecause: "OSV.dev's API"}},
		// www.cisa.gov answers HTTP 403 to some data center addresses, and
		// CISA's repository cisagov/kev-data on GitHub holds the same file,
		// which the example software review task in docs/how-to/turn-on-the-scheduled-software-review.md reads.
		// The proxy sees only the host, so this lists every public file on
		// raw.githubusercontent.com.
		{ListedHost: triageagent.ListedHost{Host: "raw.githubusercontent.com", Port: 443, ListedBecause: "CISA's list of known exploited vulnerabilities, from CISA's copy on GitHub"}},
	}
	for _, host := range triageagent.ServiceHosts(codingAgent, credentialType) {
		hosts = append(hosts, TriageAgentListedHost{ListedHost: host})
	}
	if withSystemsRepositoryHosts {
		servers, err := listedInventoryServers(ctx, tx)
		if err != nil {
			return nil, err
		}
		for _, server := range servers {
			hosts = append(hosts, server.TriageAgentListedHost)
		}
		vendorPageHosts, err := listedVendorPageHosts(ctx, tx)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, vendorPageHosts...)
	}
	rows, err := tx.QueryContext(ctx, `SELECT host,port,added_by FROM triage_agent_listed_hosts ORDER BY host,port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var host TriageAgentListedHost
		if err = rows.Scan(&host.Host, &host.Port, &host.AddedBy); err != nil {
			return nil, err
		}
		host.ListedBecause = "Added by " + host.AddedBy
		found := false
		for index, existing := range hosts {
			if existing.Host == host.Host && existing.Port == host.Port {
				hosts[index].AddedBy = host.AddedBy
				found = true
				break
			}
		}
		if !found {
			hosts = append(hosts, host)
		}
	}
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].Host == hosts[j].Host {
			return hosts[i].Port < hosts[j].Port
		}
		return hosts[i].Host < hosts[j].Host
	})
	return hosts, rows.Err()
}

// codingAgentSetup reads role's setup without its credential, with its
// credential's expiry and its sign-in; nil when there is none.
func codingAgentSetup(ctx context.Context, tx *sql.Tx, role agentRole) (*TriageAgentSetup, error) {
	setup := TriageAgentSetup{}
	var at int64
	var expiresOn string
	err := tx.QueryRowContext(ctx, `SELECT coding_agent,credential_type,credential_expires_on,model,reasoning_effort,set_up_at,set_up_by FROM `+role.setupTable+` WHERE id=1`).Scan(&setup.CodingAgent, &setup.CredentialType, &expiresOn, &setup.Model, &setup.ReasoningEffort, &at, &setup.SetUpBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	setup.SetUpAt = time.Unix(at, 0).UTC()
	if expiresAt := credentialExpiresAt(setup.CredentialType, setup.SetUpAt, expiresOn); !expiresAt.IsZero() {
		setup.CredentialExpiresAt = &expiresAt
	}
	status, err := readSignInStatus(ctx, tx, role, setup.SetUpAt)
	setup.SignInLastCheckedAt, setup.SignInWorks, setup.SignInFailureReason = status.lastCheckedAt, status.works, status.failureReason
	return &setup, err
}

// latestTestRun reads the latest run of purpose, a setup test or a sign-in
// check of either agent, with its steps; nil when there is none.
func latestTestRun(ctx context.Context, tx *sql.Tx, purpose string) (*TriageAgentRun, error) {
	run, err := scanTriageAgentRun(tx.QueryRowContext(ctx, `SELECT `+triageAgentRunColumns+` FROM triage_agent_runs WHERE purpose=? ORDER BY started_at DESC,rowid DESC LIMIT 1`, purpose))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run.Steps, _, err = triageAgentRunSteps(ctx, tx, run.ID, -1)
	return &run, err
}

// triageAgentRunColumns are the columns of triage_agent_runs that
// scanTriageAgentRun reads.
const triageAgentRunColumns = `id,purpose,coding_agent,credential_type,coding_agent_session_id,state,started_at,started_by,ended_at,answer,failure_reason,cost_in_us_dollars,input_tokens,cached_input_tokens,output_tokens`

// scanTriageAgentRun reads a run without its steps.
func scanTriageAgentRun(row scanner) (TriageAgentRun, error) {
	run := TriageAgentRun{Steps: []TriageAgentRunStep{}}
	var started, ended int64
	var cost sql.NullFloat64
	if err := row.Scan(&run.ID, &run.Purpose, &run.CodingAgent, &run.CredentialType, &run.CodingAgentSessionID, &run.State, &started, &run.StartedBy, &ended, &run.Answer, &run.FailureReason, &cost, &run.InputTokens, &run.CachedInputTokens, &run.OutputTokens); err != nil {
		return run, err
	}
	run.StartedAt = time.Unix(started, 0).UTC()
	if ended != 0 {
		at := time.Unix(ended, 0).UTC()
		run.EndedAt = &at
	}
	if cost.Valid {
		run.CostInUSDollars = &cost.Float64
	}
	return run, nil
}

// triageAgentRunSteps reads a run's latest steps, oldest first, all of them
// when latest is -1, and counts the earlier steps it left out.
func triageAgentRunSteps(ctx context.Context, tx *sql.Tx, runID string, latest int) ([]TriageAgentRunStep, int, error) {
	steps := []TriageAgentRunStep{}
	earlier := 0
	if latest >= 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM triage_agent_run_steps WHERE run_id=?`, runID).Scan(&count); err != nil {
			return nil, 0, err
		}
		earlier = max(0, count-latest)
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,occurred_at,kind,text FROM triage_agent_run_steps WHERE run_id=? ORDER BY sequence LIMIT -1 OFFSET ?`, runID, earlier)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var step TriageAgentRunStep
		var timestamp int64
		if err = rows.Scan(&step.Sequence, &timestamp, &step.Kind, &step.Text); err != nil {
			return nil, 0, err
		}
		step.OccurredAt = time.Unix(timestamp, 0).UTC()
		steps = append(steps, step)
	}
	return steps, earlier, rows.Err()
}

func (a *App) TriageAgentState(ctx context.Context, p Principal) (TriageAgentView, error) {
	a.triageAgent.mu.Lock()
	installed := a.triageAgent.runSocketPath != "" && a.triageAgent.proxySocketPath != ""
	active := a.triageAgent.activeRun
	codingAgent, credentialType := "", ""
	// The review agent's setup test reaches its own coding agent's service.
	if active != nil && !reviewAgentPurpose(active.purpose) {
		codingAgent, credentialType = active.request.CodingAgent, active.request.CredentialType
	}
	a.triageAgent.mu.Unlock()
	view := TriageAgentView{SandboxInstalled: installed, RecentConnections: []TriageAgentProxyConnection{}}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var err error
		if view.Setup, err = codingAgentSetup(ctx, tx, triageAgentRole); err != nil {
			return err
		}
		if view.Setup != nil && codingAgent == "" {
			codingAgent, credentialType = view.Setup.CodingAgent, view.Setup.CredentialType
		}
		if err = tx.QueryRowContext(ctx, `SELECT internet_access FROM triage_agent_internet_access WHERE id=1`).Scan(&view.InternetAccess); err != nil {
			return err
		}
		view.ListedHosts, err = triageAgentHosts(ctx, tx, codingAgent, credentialType, true)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT commit_signing_public_key FROM triage_agent_commit_check_state WHERE id=1`).Scan(&view.CommitSigningPublicKey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if view.TrustsEveryCommitOnOrigin, err = trustsEveryCommitOnOrigin(ctx, tx); err != nil {
			return err
		}
		var listedAt int64
		err = tx.QueryRowContext(ctx, `SELECT inventory_servers_not_listed_because,vendor_page_hosts_not_listed_because,started_at FROM triage_agent_runs WHERE purpose IN ('triage_session','software_review','commit_review') ORDER BY started_at DESC,rowid DESC LIMIT 1`).Scan(&view.InventoryServersNotListedBecause, &view.VendorPageHostsNotListedBecause, &listedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			at := time.Unix(listedAt, 0).UTC()
			view.HostsFromTheSystemsRepositoryListedAt = &at
		}
		if view.LatestSetupTest, err = latestTestRun(ctx, tx, "setup_test"); err != nil {
			return err
		}
		if view.LatestSignInCheck, err = latestTestRun(ctx, tx, "sign_in_check"); err != nil {
			return err
		}
		if view.LatestSoftwareReview, err = latestReviewRun(ctx, tx, "software_review"); err != nil {
			return err
		}
		if view.LatestCommitReview, err = latestReviewRun(ctx, tx, "commit_review"); err != nil {
			return err
		}
		interval, err := readSoftwareReviewInterval(ctx, tx)
		if err != nil {
			return err
		}
		view.SoftwareReviewIntervalHours = interval.hours
		view.SoftwareReviewIntervalChangedAt, view.SoftwareReviewIntervalChangedBy = interval.changedAt, interval.changedBy
		if interval.hours != softwareReviewIntervalOff {
			due, err := softwareReviewDueAt(ctx, tx, interval.hours)
			if err != nil {
				return err
			}
			if !due.IsZero() {
				view.NextSoftwareReviewDueAt = &due
			}
		}
		if skipped := interval.skippedForTheRunLimitAt; skipped != nil && (view.LatestSoftwareReview == nil || skipped.After(view.LatestSoftwareReview.StartedAt)) {
			view.SoftwareReviewSkippedForTheRunLimitAt = skipped
		}
		if view.RunLimit, err = readTriageAgentRunLimitView(ctx, tx, a.Now().UTC()); err != nil {
			return err
		}
		// A listed host lets the triage agent through, never a review.
		rows, err := tx.QueryContext(ctx, `SELECT c.id,c.run_id,c.started_at,c.host,c.port,c.address,c.decision,c.refusal_reason,c.bytes_sent,c.bytes_received,c.ended_at,coalesce(r.purpose,'') FROM triage_agent_proxy_connections c LEFT JOIN triage_agent_runs r ON r.id=c.run_id ORDER BY c.id DESC LIMIT 50`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var connection TriageAgentProxyConnection
			var started, ended int64
			var purpose string
			if err = rows.Scan(&connection.ID, &connection.RunID, &started, &connection.Host, &connection.Port, &connection.Address, &connection.Decision, &connection.RefusalReason, &connection.BytesSent, &connection.BytesReceived, &ended, &purpose); err != nil {
				return err
			}
			connection.StartedAt = time.Unix(started, 0).UTC()
			if ended != 0 {
				at := time.Unix(ended, 0).UTC()
				connection.EndedAt = &at
			}
			listed := false
			for _, host := range view.ListedHosts {
				listed = listed || host.Host == connection.Host && host.Port == connection.Port
			}
			_, hostErr := triageAgentHost(connection.Host, connection.Port)
			connection.CanAddListedHost = connection.Decision == "refused" && !listed && hostErr == nil && !reviewAgentPurpose(purpose)
			view.RecentConnections = append(view.RecentConnections, connection)
		}
		return rows.Err()
	})
	return view, err
}
