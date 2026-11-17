package app

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

type triageAgentRuntime struct {
	mu sync.Mutex
	// runSocketPath starts a triage agent run as the triage agent's Unix
	// user, and reviewRunSocketPath a review agent run as the review
	// agent's own Unix user, so neither run can read the other's process.
	runSocketPath, reviewRunSocketPath, proxySocketPath string
	// activeRun is the one triage run: a triage session's run or a setup test.
	activeRun *triageAgentActiveRun
	// activeReview is the one review of a playbook, which runs while the
	// triage run that asked for the playbook waits for its answer.
	activeReview *triageAgentActiveRun
	// proxyConnectionsWithoutARun counts the logging proxy's connections that
	// have not yet named a run, or named none they may use.
	proxyConnectionsWithoutARun int
	resolve                     func(context.Context, string) ([]net.IPAddr, error)
	dial                        func(context.Context, string, string) (net.Conn, error)
	playbookRuns                TriageAgentPlaybookRunDecider
	playbookRunner              TriageAgentPlaybookRunner
	mmxSearchPath               string
	mmxRunTimeLimit             time.Duration
	// codingAgentAPIBaseURLs are where an API key's sign-in check asks for
	// the models list (codingAgentAPIBaseURLs).
	codingAgentAPIBaseURLs map[string]string
	// backgroundRunStarting is set while the background loop starts a
	// triage session's run, a software review, or a sign-in check in its
	// own goroutine, before it holds the one run (startBesideTheLoop).
	backgroundRunStarting bool
	backgroundRunStarts   sync.WaitGroup
	// approvedPlaybookRuns are the approved playbook runs going beside the
	// background loop, which waits for them before the stores close.
	approvedPlaybookRuns sync.WaitGroup
}
type triageAgentActiveRun struct {
	id string
	// purpose is setup_test, triage_session, software_review, or
	// commit_review for the triage agent, and review_agent_setup_test or playbook_review for the
	// review agent.
	purpose     string
	request     triageagent.Request // without the systems repository bundle, which is sent and dropped
	startedBy   string
	connection  net.Conn
	writeMu     sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	connections map[net.Conn]bool
	tunnels     sync.WaitGroup
	ending      bool
	done        chan struct{}
	timeLimit   *time.Timer
	// closedForShutdown: CloseTriageAgent ended the run, as MMHotline stops.
	closedForShutdown bool
	// stoppedForTriageSession: a software review, a commit review, or a
	// sign-in check that MMHotline stopped because a triage session needs the one run.
	stoppedForTriageSession bool
	// result is the run's final event as recorded, set before done closes.
	result triageagent.Event
	// setup is a setup test's setup from the settings file, which the test
	// saves when it passes; nil for every other run.
	setup *SettingsCodingAgent
	// signInMu guards the run's ChatGPT sign-in and its secrets, which change
	// when the run sends a refreshed copy (saveReturnedChatGPTSignIn).
	signInMu sync.Mutex
	// chatGPTSignIn is the newest checked copy of a ChatGPT sign-in: the
	// copy lent to the run, then each copy it sent back that passed.
	chatGPTSignIn string
	// secrets are the credential's values that output and reasons never
	// show: the credential, or each token of every ChatGPT sign-in copy.
	secrets []string
}

// reviewAgentPurpose tells the review agent's runs from the triage agent's.
func reviewAgentPurpose(purpose string) bool {
	return purpose == "review_agent_setup_test" || purpose == "review_agent_sign_in_check" || purpose == "playbook_review"
}

// signInCheckPurpose tells the daily sign-in checks of either agent.
func signInCheckPurpose(purpose string) bool {
	return purpose == "sign_in_check" || purpose == "review_agent_sign_in_check"
}

// testRunPurpose tells the runs that ask for the test sentence: the setup
// tests, and the sign-in checks, which run the setup's own test prompt.
func testRunPurpose(purpose string) bool {
	return purpose == "setup_test" || purpose == "review_agent_setup_test" || signInCheckPurpose(purpose)
}

// givesWayToTriageSessions tells the runs that MMHotline starts by itself
// and stops for a triage session: incidents come first.
func givesWayToTriageSessions(purpose string) bool {
	return purpose == "software_review" || purpose == "commit_review" || signInCheckPurpose(purpose)
}

// inSystemsRepository tells the triage agent's runs that work on the team's
// servers, in its copy of the systems repository, from the setup tests. Their
// reports reach people; a setup test's report is only recorded.
func inSystemsRepository(purpose string) bool {
	return purpose == "triage_session" || purpose == "software_review" || purpose == "commit_review"
}

// triageAgentSetupTestTimeLimit ends a setup test that has not answered. The
// test asks for one sentence, which takes seconds; without this limit a hung
// test would hold the one run until the sandbox's one-hour ceiling and refuse
// every retry. It applies to the review agent's setup test too.
var triageAgentSetupTestTimeLimit = 5 * time.Minute

// reviewAgentRunTimeLimit ends a review that has not answered. A review reads
// one playbook and answers in a minute or two; the triage run waits for it,
// and a stuck review agent must not hold that run, so after the limit a
// person decides.
var reviewAgentRunTimeLimit = 10 * time.Minute

func (run *triageAgentActiveRun) redact(text string) string {
	run.signInMu.Lock()
	defer run.signInMu.Unlock()
	for _, secret := range run.secrets {
		text = strings.ReplaceAll(text, secret, "[credential removed]")
	}
	return text
}

// credentialSecrets are the values of a credential that output never shows.
func credentialSecrets(credentialType, credential string) []string {
	if credentialType == "chatgpt_sign_in" {
		return triageagent.ChatGPTSignInSecrets(credential)
	}
	if credential == "" {
		return nil
	}
	return []string{credential}
}

// saveReturnedChatGPTSignIn checks a copy of the run's ChatGPT sign-in that
// the run sent back after Codex changed it, and keeps it when it passes:
// docs/changing-mmhotline/design/triage-agent.md#codex-with-a-chatgpt-sign-in. A run lent the
// setup's newest copy saves it in the setup at once, so a run cut short
// loses at most the last second; this includes a setup test after only the
// model or reasoning effort changed, since Codex used up the copy it lent
// even when the test then fails. A setup test of a new sign-in keeps it on
// the run, for the setup it saves. The reason a copy was refused never
// quotes it.
func (a *App) saveReturnedChatGPTSignIn(run *triageAgentActiveRun, returned string) error {
	if run.request.CredentialType != "chatgpt_sign_in" {
		return errors.New("the run's credential is not a ChatGPT sign-in")
	}
	run.signInMu.Lock()
	stored := run.chatGPTSignIn
	run.signInMu.Unlock()
	if err := triageagent.CheckChatGPTSignInChange(stored, returned); err != nil {
		return err
	}
	role := agentRoleOf(run.purpose)
	var saved int64
	err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
		// Only over the copy the run was lent or saved last: a setup that
		// changed meanwhile, or a setup test's new sign-in, is another sign-in.
		result, err := tx.ExecContext(run.ctx, `UPDATE `+role.setupTable+` SET current_chatgpt_sign_in=? WHERE id=1 AND credential_type='chatgpt_sign_in' AND current_chatgpt_sign_in=?`, returned, stored)
		if err == nil {
			saved, err = result.RowsAffected()
		}
		return err
	})
	if err != nil {
		return errors.New("MMHotline could not save it")
	}
	if saved == 0 && run.setup == nil {
		return fmt.Errorf("the %s's setup changed during the run", role.name)
	}
	run.signInMu.Lock()
	run.chatGPTSignIn = returned
	run.secrets = append(run.secrets, triageagent.ChatGPTSignInSecrets(returned)...)
	run.signInMu.Unlock()
	return nil
}

// TriageAgentSocketFlagsTogether is serve's rule for its three triage agent
// sockets: all three, or none in local development without the sandbox.
const TriageAgentSocketFlagsTogether = "--triage-agent-run-socket, --review-agent-run-socket, and --triage-agent-proxy-socket must be supplied together"

// InitializeTriageAgent runs before listeners start, including in local development.
func (a *App) InitializeTriageAgent(ctx context.Context, runSocketPath, reviewRunSocketPath, proxySocketPath string) error {
	if (runSocketPath == "") != (proxySocketPath == "") || (runSocketPath == "") != (reviewRunSocketPath == "") {
		return fmt.Errorf("%w: %s", ErrInvalid, TriageAgentSocketFlagsTogether)
	}
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM triage_agent_runs WHERE state='running' AND purpose='triage_session'`)
		if err != nil {
			return err
		}
		var cut []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			cut = append(cut, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range cut {
			if err = triageSessionRunFailed(ctx, tx, id, triageRunCutByRestart, true, a.Now().UTC()); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE triage_agent_runs SET state='failed',ended_at=?,failure_reason=? WHERE state='running'`, a.Now().Unix(), triageRunCutByRestart); err != nil {
			return err
		}
		return failApprovedPlaybookRunsCutByRestart(ctx, tx, a.Now().UTC())
	}); err != nil {
		return err
	}
	a.triageAgent.mu.Lock()
	a.triageAgent.runSocketPath, a.triageAgent.reviewRunSocketPath, a.triageAgent.proxySocketPath = runSocketPath, reviewRunSocketPath, proxySocketPath
	a.triageAgent.mu.Unlock()
	return nil
}

// startCodingAgentSetupTest runs role's setup test with setup: one test
// prompt, with its coding agent, credential, model, and reasoning effort,
// which takes the one triage run. The run saves the setup when it passes
// (finishTriageAgentRun). Each review runs in a new empty home, and so does
// the review agent's setup test.
func (a *App) startCodingAgentSetupTest(ctx context.Context, role agentRole, setup SettingsCodingAgent, startedBy string) (*triageAgentActiveRun, error) {
	if err := triageagent.CheckSetup(setup.CodingAgent, setup.CredentialType, setup.Credential); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	request := triageagent.Request{Action: "start", CodingAgent: setup.CodingAgent, CredentialType: setup.CredentialType, Credential: setup.Credential, Model: setup.Model, ReasoningEffort: setup.ReasoningEffort, Prompt: role.setupTestPrompt}
	return a.startTriageAgentRun(ctx, role.setupTestPurpose, startedBy, request, &setup, func(*sql.Tx, string) error { return nil })
}

// lentChatGPTSignIn is the copy of role's ChatGPT sign-in a run gets: the
// setup's newest copy. A setup test gets it too when the settings file's
// sign-in is the one that applied before, which the runs since used up, as
// when only the model or the reasoning effort changed; a new sign-in from
// the file is lent as it is. It is read after the run's place is reserved,
// so no other run can save a newer copy meanwhile.
func lentChatGPTSignIn(ctx context.Context, tx *sql.Tx, role agentRole, request triageagent.Request, setup *SettingsCodingAgent) (string, error) {
	query, arguments := `SELECT current_chatgpt_sign_in FROM `+role.setupTable+` WHERE id=1 AND credential_type='chatgpt_sign_in' AND current_chatgpt_sign_in!=''`, []any{}
	if setup != nil {
		query += ` AND coding_agent=? AND credential=?`
		arguments = append(arguments, setup.CodingAgent, setup.Credential)
	}
	var current string
	err := tx.QueryRowContext(ctx, query, arguments...).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return request.Credential, nil
	}
	return current, err
}

// oneTriageRunIsFree says whether a software review or a sign-in check may
// take the one triage run now: the sandbox is installed, and no run is
// active or starting. An open triage session does not matter, because one
// that needs the run stops them. The end of a run changes the database,
// which brings the background loop's next visit.
func (a *App) oneTriageRunIsFree() bool {
	runtime := &a.triageAgent
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.runSocketPath != "" && runtime.proxySocketPath != "" && runtime.activeRun == nil && !runtime.backgroundRunStarting
}

// startBesideTheLoop runs start, which starts a run of the background loop,
// in its own goroutine, unless the one triage run is taken or another start
// is under way. A start may fetch from the team's Git host, which can take
// minutes, and a notification must never wait for that.
func (a *App) startBesideTheLoop(start func()) {
	runtime := &a.triageAgent
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.activeRun != nil || runtime.backgroundRunStarting {
		return
	}
	runtime.backgroundRunStarting = true
	runtime.backgroundRunStarts.Go(func() {
		defer func() {
			runtime.mu.Lock()
			runtime.backgroundRunStarting = false
			runtime.mu.Unlock()
		}()
		start()
	})
}

// startTriageAgentRunInSystemsRepository starts a run of purpose,
// triage_session, software_review, or commit_review, with the saved setup in the
// agent's copy of the systems repository, which starts at MMHotline's main
// branch. request holds the action, the prompt, and for a resume the coding
// agent's session ID; recordRun runs in the transaction that records the run,
// and may refuse it.
func (a *App) startTriageAgentRunInSystemsRepository(ctx context.Context, purpose, startedBy string, planned triageagent.Request, recordRun func(tx *sql.Tx, runID string) error) (string, error) {
	request, err := a.savedAgentRequest(ctx, triageAgentRole, planned.Action, planned.Prompt)
	if err != nil {
		return "", err
	}
	request.SessionID = planned.SessionID
	run, err := a.startTriageAgentRun(ctx, purpose, startedBy, request, nil, recordRun)
	if err != nil {
		return "", err
	}
	return run.id, nil
}

// startTriageAgentRun reserves the run's place, records it, and sends request
// to the sandbox: a playbook review takes the one review run, every other
// run the one triage run. authorize runs in the transaction that records the
// run, which it may refuse. setup is a setup test's setup from the settings
// file, nil for any other run. A run that fails after it was recorded is
// returned ended.
func (a *App) startTriageAgentRun(ctx context.Context, purpose, startedBy string, request triageagent.Request, setup *SettingsCodingAgent, authorize func(tx *sql.Tx, runID string) error) (*triageAgentActiveRun, error) {
	runtime := &a.triageAgent
	runtime.mu.Lock()
	if runtime.runSocketPath == "" || runtime.proxySocketPath == "" {
		runtime.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrInvalid, TriageAgentSandboxNotInstalled)
	}
	place, busy := &runtime.activeRun, "A triage agent run is already active."
	if purpose == "playbook_review" {
		place, busy = &runtime.activeReview, "A review agent run is already active."
	}
	if *place != nil {
		runtime.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrConflict, busy)
	}
	request.AgentRole = triageagent.TriageAgentRole
	socket := runtime.runSocketPath
	if reviewAgentPurpose(purpose) {
		request.AgentRole = triageagent.ReviewAgentRole
		socket = runtime.reviewRunSocketPath
	}
	// The proxy token is a secret apart from the run's ID, which pages show.
	request.ProxyToken = NewID()
	runContext, cancel := context.WithCancel(context.Background())
	run := &triageAgentActiveRun{id: NewID(), purpose: purpose, request: request, startedBy: startedBy, ctx: runContext, cancel: cancel, connections: map[net.Conn]bool{}, done: make(chan struct{}), setup: setup}
	*place = run
	runtime.mu.Unlock()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by) VALUES(?,?,?,?,'running',?,?)`, run.id, purpose, request.CodingAgent, request.CredentialType, a.Now().Unix(), startedBy); err != nil {
			return err
		}
		if request.CredentialType == "chatgpt_sign_in" {
			var err error
			if request.Credential, err = lentChatGPTSignIn(ctx, tx, agentRoleOf(purpose), request, setup); err != nil {
				return err
			}
		}
		return authorize(tx, run.id)
	})
	// Set before the request goes out, so the run's first event finds them.
	run.request.Credential, run.chatGPTSignIn, run.secrets = request.Credential, request.Credential, credentialSecrets(request.CredentialType, request.Credential)
	if err != nil {
		cancel()
		runtime.mu.Lock()
		*place = nil
		close(run.done)
		runtime.mu.Unlock()
		return nil, err
	}
	if checksSignInWithModelsList(purpose, request.CredentialType) {
		go func() { a.finishTriageAgentRun(run, a.checkAPIKeyWithModelsList(run.ctx, run.request)) }()
		return run, nil
	}
	sent := request
	if inSystemsRepository(purpose) {
		// The one run is reserved first, so MMHotline's copy is never handled for two runs at once.
		bundle, base, err := a.trustedRunBase(ctx)
		if err != nil {
			a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "Could not prepare the systems repository: " + err.Error()})
			return run, nil
		}
		runtime.mu.Lock()
		run.request.SystemsRepositoryBaseCommitID = base
		runtime.mu.Unlock()
		sent.SystemsRepositoryBaseCommitID, sent.SystemsRepositoryBundle = base, bundle
		// Before the run starts, so the proxy lets the agent read the servers
		// and the vendor pages from its first step, and its SSH settings know
		// the servers' host keys.
		sent.InventoryServers = a.listInventoryServers(ctx, run.id)
		a.listVendorPageHosts(ctx, run.id, base)
		// A software review's task is the team's file at the commit the run
		// starts from; without a usable file, the review fails and counts.
		if purpose == "software_review" {
			if sent.Prompt, err = a.softwareReviewPromptAt(ctx, base); err != nil {
				a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: err.Error()})
				return run, nil
			}
		}
		// The commits reach the agent only in its prompt, as data; its copy
		// and its rules are at the trusted commit.
		if purpose == "commit_review" {
			if sent.Prompt, err = a.commitReviewPrompt(ctx, run.id); err != nil {
				a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: err.Error()})
				return run, nil
			}
		}
	}
	connection, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "Could not connect to the triage agent sandbox."})
		return run, nil
	}
	run.writeMu.Lock()
	runtime.mu.Lock()
	run.connection = connection
	if run.ctx.Err() != nil {
		connection.Close()
	}
	runtime.mu.Unlock()
	connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = json.NewEncoder(connection).Encode(sent)
	connection.SetWriteDeadline(time.Time{})
	run.writeMu.Unlock()
	if err != nil {
		a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "Could not send the request to the triage agent sandbox."})
		return run, nil
	}
	// The timer is set under the lock that finishTriageAgentRun reads it
	// under, so a timer that fires at once waits for the field to be set.
	a.triageAgent.mu.Lock()
	switch {
	case testRunPurpose(purpose):
		test := "setup test"
		if signInCheckPurpose(purpose) {
			test = "sign-in check"
		}
		run.timeLimit = time.AfterFunc(triageAgentSetupTestTimeLimit, func() {
			a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: fmt.Sprintf("The %s did not finish within %s.", test, triageAgentSetupTestTimeLimit)})
		})
	case purpose == "playbook_review":
		run.timeLimit = time.AfterFunc(reviewAgentRunTimeLimit, func() {
			a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: fmt.Sprintf("The review did not finish within %s.", reviewAgentRunTimeLimit)})
		})
	}
	a.triageAgent.mu.Unlock()
	go a.readTriageAgentRun(run)
	return run, nil
}
func (a *App) StopTriageAgentRun(runID string) error {
	a.triageAgent.mu.Lock()
	run := a.triageAgent.activeRun
	a.triageAgent.mu.Unlock()
	if run == nil || run.id != runID {
		return ErrNotFound
	}
	return a.writeTriageAgentControl(run, triageagent.Control{Stop: true})
}

// writeTriageAgentControl sends one control line to a run that is not ending.
func (a *App) writeTriageAgentControl(run *triageAgentActiveRun, control triageagent.Control) error {
	a.triageAgent.mu.Lock()
	connection, ending := run.connection, run.ending
	a.triageAgent.mu.Unlock()
	if ending || connection == nil {
		return ErrNotFound
	}
	run.writeMu.Lock()
	defer run.writeMu.Unlock()
	connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer connection.SetWriteDeadline(time.Time{})
	return json.NewEncoder(connection).Encode(control)
}
func (a *App) readTriageAgentRun(run *triageAgentActiveRun) {
	scanner := bufio.NewScanner(run.connection)
	scanner.Buffer(make([]byte, 4096), triageagent.MaximumLineSize)
	sequence := 0
	for scanner.Scan() {
		var event triageagent.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			break
		}
		event.SessionID = run.redact(event.SessionID)
		event.Answer = run.redact(event.Answer)
		event.Reason = run.redact(event.Reason)
		switch event.Event {
		case "session":
			if err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(run.ctx, `UPDATE triage_agent_runs SET coding_agent_session_id=? WHERE id=?`, event.SessionID, run.id)
				return err
			}); err != nil {
				a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The run's session could not be saved."})
				return
			}
		case "step":
			if event.Step == nil || (event.Step.Kind != "message" && event.Step.Kind != "command" && event.Step.Kind != "tool") {
				a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The sandbox sent an invalid run step."})
				return
			}
			sequence++
			text := []rune(run.redact(event.Step.Text))
			if len(text) > 2000 {
				text = append(text[:1999], '…')
			}
			if err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(run.ctx, `INSERT INTO triage_agent_run_steps(run_id,sequence,occurred_at,kind,text) VALUES(?,?,?,?,?)`, run.id, sequence, a.Now().Unix(), event.Step.Kind, string(text))
				return err
			}); err != nil {
				a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The run's step could not be saved."})
				return
			}
		// Answered here, in the run's reader, so MMHotline's copy handles one
		// bundle of the agent's commits at a time, the final event's included.
		case "playbook_run_requested", "report", "commit_review":
			// The review agent's run has no relay; a request from it went
			// around the relay, and a review must never reach people.
			if reviewAgentPurpose(run.purpose) {
				a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The review agent's run sent a request; review agents make none."})
				return
			}
			if err := a.answerTriageAgentRequest(run, event); err != nil {
				a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: err.Error()})
				return
			}
		case triageagent.ChatGPTSignInChangedEvent:
			if err := a.saveReturnedChatGPTSignIn(run, event.ChatGPTSignIn); err != nil {
				// A refused copy is not saved; the record says why, and the
				// run goes on with its sign-in as Codex left it.
				sequence++
				if err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(run.ctx, `INSERT INTO triage_agent_run_steps(run_id,sequence,occurred_at,kind,text) VALUES(?,?,?,'message',?)`, run.id, sequence, a.Now().Unix(), run.redact("MMHotline did not save the changed ChatGPT sign-in, so it keeps the earlier copy: "+err.Error()+"."))
					return err
				}); err != nil {
					a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The run's step could not be saved."})
					return
				}
			}
		case "finished", "stopped", "failed":
			a.finishTriageAgentRun(run, event)
			return
		default:
			a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The sandbox sent an invalid run event."})
			return
		}
	}
	a.finishTriageAgentRun(run, triageagent.Event{Event: "failed", Reason: "The run ended without a result."})
}

// finishTriageAgentRun ends a run once; the time limit and the run's reader
// may both try.
func (a *App) finishTriageAgentRun(run *triageAgentActiveRun, event triageagent.Event) {
	a.triageAgent.mu.Lock()
	if run.ending {
		a.triageAgent.mu.Unlock()
		return
	}
	run.ending = true
	closedForShutdown, stoppedForTriageSession := run.closedForShutdown, run.stoppedForTriageSession
	if run.timeLimit != nil {
		run.timeLimit.Stop()
	}
	run.cancel()
	if run.connection != nil {
		run.connection.Close()
	}
	for connection := range run.connections {
		connection.Close()
	}
	a.triageAgent.mu.Unlock()
	run.tunnels.Wait()
	// The agent's commits reach the Git host while the one run stays reserved,
	// so the next run starts from them. A run without a base sent none.
	base := run.request.SystemsRepositoryBaseCommitID
	received := base != "" && len(event.AgentCommitsBundle) > 0
	var commits systemsrepository.ReceivedAgentCommits
	var commitsErr error
	if received {
		commits, commitsErr = systemsrepository.ReceiveAgentCommits(context.Background(), filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), event.AgentCommitsBundle, base, a.trustedCommit(nil))
	}
	event.Answer, event.Reason, event.SessionID = run.redact(event.Answer), run.redact(event.Reason), run.redact(event.SessionID)
	testRun := testRunPurpose(run.purpose)
	passed := testRun && event.Event == "finished" && (checksSignInWithModelsList(run.purpose, run.request.CredentialType) || strings.Contains(event.Answer, setupTestSentence(run.purpose)))
	if testRun && event.Event == "finished" && !passed {
		event.Reason = "The coding agent answered, but not with the test sentence."
	}
	if event.Event == "failed" && event.Reason == "" {
		event.Reason = "The triage agent run failed."
	}
	if event.Event == "stopped" && event.Reason == "" {
		event.Reason = "The triage agent run was stopped."
	}
	// Closing the connection fails the run; a result that came first stands.
	cutShort := closedForShutdown && (inSystemsRepository(run.purpose) || signInCheckPurpose(run.purpose)) && event.Event == "failed"
	if cutShort {
		event.Reason = triageRunCutByStop
	} else if stoppedForTriageSession && event.Event != "finished" {
		// A stop before the agent reported its session ends as failed; either
		// way the review or the check was cut short and starts again after
		// the session.
		event.Event, event.Reason = "stopped", softwareReviewStoppedForTriageSession
		if signInCheckPurpose(run.purpose) {
			event.Reason = signInCheckStoppedForTriageSession
		}
		if run.purpose == "commit_review" {
			event.Reason = commitReviewStoppedForTriageSession
		}
	}
	total := triageagent.Usage{}
	if event.CodingAgentSessionTotalUsage != nil {
		total = *event.CodingAgentSessionTotalUsage
	}
	now := a.Now().Unix()
	err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		// A resumed coding agent session reports its total so far: the run's
		// own use is what the session used after its latest finished run.
		// A run that reported no total, such as a failed one, records none.
		own := total
		if run.request.Action == "resume" && event.CodingAgentSessionTotalUsage != nil {
			earlier, err := codingAgentSessionTotalUsage(tx, run.request.SessionID, run.id)
			if err != nil {
				return err
			}
			own = usageAfter(total, earlier)
		}
		_, err := tx.Exec(`UPDATE triage_agent_runs SET state=?,ended_at=?,answer=?,failure_reason=?,cost_in_us_dollars=?,input_tokens=?,cached_input_tokens=?,output_tokens=?,coding_agent_session_total_cost_in_us_dollars=?,coding_agent_session_total_input_tokens=?,coding_agent_session_total_cached_input_tokens=?,coding_agent_session_total_output_tokens=?,coding_agent_session_id=CASE WHEN ?='' THEN coding_agent_session_id ELSE ? END WHERE id=?`,
			event.Event, now, event.Answer, event.Reason, own.CostInUSDollars, own.InputTokens, own.CachedInputTokens, own.OutputTokens, total.CostInUSDollars, total.InputTokens, total.CachedInputTokens, total.OutputTokens, event.SessionID, event.SessionID, run.id)
		if err != nil {
			return err
		}
		if received {
			outcome, reason := receivedAgentCommitsOutcome(commits, commitsErr)
			if err = insertTriageAgentCommits(tx, run.id, "", base, commits, outcome, reason); err != nil {
				return err
			}
		}
		// A commit review that MMHotline cut short leaves its commits to
		// the next one; any other end leaves MMHotline's warning for each
		// commit the agent gave no decision on.
		if run.purpose == "commit_review" && !cutShort && !stoppedForTriageSession {
			reason := "the commit review run ended without a decision on the commit"
			if event.Event == "failed" {
				reason = "the commit review run failed: " + strings.TrimSuffix(event.Reason, ".")
			}
			if err = a.warnAboutCommitsTheRunLeft(context.Background(), tx, run.id, reason); err != nil {
				return err
			}
		}
		if run.purpose == "triage_session" && event.Event == "failed" {
			if err = triageSessionRunFailed(context.Background(), tx, run.id, event.Reason, cutShort, time.Unix(now, 0).UTC()); err != nil {
				return err
			}
		}
		// Before a passing setup test replaces the setup, whose row records
		// whether administrators heard that the sign-in fails.
		if err = a.recordSignInOutcome(context.Background(), tx, run, event, time.Unix(now, 0).UTC()); err != nil {
			return err
		}
		run.signInMu.Lock()
		chatGPTSignIn := run.chatGPTSignIn
		run.signInMu.Unlock()
		if passed && run.purpose == "review_agent_setup_test" {
			err = saveCodingAgentSetup(tx, reviewAgentRole, *run.setup, chatGPTSignIn, now, run.startedBy)
		} else if passed && run.purpose == "setup_test" {
			var replaced bool
			if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM triage_agent_setup)`).Scan(&replaced); err != nil {
				return err
			}
			if err = saveCodingAgentSetup(tx, triageAgentRole, *run.setup, chatGPTSignIn, now, run.startedBy); err != nil {
				return err
			}
			if !replaced {
				err = skipAlertsForTriageSessions(context.Background(), tx)
			}
		}
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmhotline: could not save the triage agent run result")
	}
	a.triageAgent.mu.Lock()
	if a.triageAgent.activeReview == run {
		a.triageAgent.activeReview = nil
	} else {
		a.triageAgent.activeRun = nil
	}
	run.result = event
	close(run.done)
	a.triageAgent.mu.Unlock()
}

// codingAgentSessionTotalUsage is the total the coding agent session
// sessionID had used when its latest finished run other than runID ended,
// or nothing when it has none. Only a finished run reports a total; a failed
// or stopped run's use is in the next total.
func codingAgentSessionTotalUsage(tx *sql.Tx, sessionID, runID string) (triageagent.Usage, error) {
	var total triageagent.Usage
	var cost sql.NullFloat64
	err := tx.QueryRow(`SELECT coding_agent_session_total_cost_in_us_dollars,coding_agent_session_total_input_tokens,coding_agent_session_total_cached_input_tokens,coding_agent_session_total_output_tokens FROM triage_agent_runs WHERE coding_agent_session_id=? AND id!=? AND state='finished' ORDER BY started_at DESC,rowid DESC LIMIT 1`, sessionID, runID).Scan(&cost, &total.InputTokens, &total.CachedInputTokens, &total.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return triageagent.Usage{}, nil
	}
	if cost.Valid {
		total.CostInUSDollars = &cost.Float64
	}
	return total, err
}

// usageAfter is what a coding agent session used after it had used earlier,
// when it has used total.
func usageAfter(total, earlier triageagent.Usage) triageagent.Usage {
	after := triageagent.Usage{
		InputTokens:       total.InputTokens - earlier.InputTokens,
		CachedInputTokens: total.CachedInputTokens - earlier.CachedInputTokens,
		OutputTokens:      total.OutputTokens - earlier.OutputTokens,
	}
	if total.CostInUSDollars != nil {
		cost := *total.CostInUSDollars
		if earlier.CostInUSDollars != nil {
			cost -= *earlier.CostInUSDollars
		}
		after.CostInUSDollars = &cost
	}
	return after
}

// CloseTriageAgent closes the run connections so systemd also stops the
// sandboxes: the triage run's and a review's.
func (a *App) CloseTriageAgent() {
	a.triageAgent.mu.Lock()
	var runs []*triageAgentActiveRun
	for _, run := range []*triageAgentActiveRun{a.triageAgent.activeRun, a.triageAgent.activeReview} {
		if run == nil {
			continue
		}
		run.closedForShutdown = true
		run.cancel()
		if run.connection != nil {
			run.connection.Close()
		}
		runs = append(runs, run)
	}
	a.triageAgent.mu.Unlock()
	for _, run := range runs {
		<-run.done
	}
}
