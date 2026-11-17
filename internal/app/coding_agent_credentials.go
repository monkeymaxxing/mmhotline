package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"mmhotline/internal/triageagent"
)

// The triage agent and the review agent each sign in to their coding
// agent's service with the credential of their setup. MMHotline tells the
// team's administrators before a credential stops working, and when one has:
// docs/changing-mmhotline/design/triage-agent.md#keeping-the-sign-in-working.
//
//   - A Claude Code subscription token lasts one year. MMHotline counts that
//     year from when the settings file's credential applied, and reminds
//     administrators 30 and 7 days before it ends. A Codex access token ends
//     on the settings file's credential_expires_on, and gets the same
//     reminders. The setup row records each reminder sent, so a restart
//     neither repeats nor skips one, and a new credential starts both
//     reminders again (saveCodingAgentSetup).
//   - A Codex ChatGPT sign-in has no end date. Codex refreshes it, and
//     MMHotline keeps the newest copy (saveReturnedChatGPTSignIn).
//   - Once a day, MMHotline runs the setup's own test prompt in the sandbox,
//     the sign-in check, unless a run of that agent succeeded in the last
//     24 hours. The runs in triage_agent_runs are the schedule and the
//     record, as for the software review. The setup row records only
//     whether administrators were told that the sign-in fails, so each
//     change is told once.

// credentialColumns are the setup's columns that some credential types add,
// in triage_agent_setup and review_agent_setup: a Codex access token's end
// date, YYYY-MM-DD, and a ChatGPT sign-in's newest copy, which each run is
// lent and which the copies it sends back replace. credential stays the
// settings file's value, which tells whether the file's credential changed.
const credentialColumns = `credential_expires_on TEXT NOT NULL DEFAULT '',
 current_chatgpt_sign_in TEXT NOT NULL DEFAULT ''`

// credentialCheckColumns are the setup's columns for its credential's
// reminders and sign-in check, in triage_agent_setup and review_agent_setup.
// A reminder's column holds when it was sent, 0 until then.
const credentialCheckColumns = `expiry_reminder_30_days_before_sent_at INTEGER NOT NULL DEFAULT 0,
 expiry_reminder_7_days_before_sent_at INTEGER NOT NULL DEFAULT 0,
 sign_in_failing_alert_sent INTEGER NOT NULL DEFAULT 0`

const (
	// subscriptionTokenLifetime: claude setup-token prints a token "valid
	// for 1 year". The keep-the-agents-signed-in guide says to make the token right before it
	// goes in the settings file, so the year counts from when it applied.
	subscriptionTokenLifetime = 365 * 24 * time.Hour
	// signInCheckInterval: a credential that stopped working is found
	// within a day, and a check costs one short run of the coding agent a
	// day at most.
	signInCheckInterval = 24 * time.Hour
)

// credentialExpiryReminders are the reminders before a subscription token
// expires, earliest first, each with the setup column that records it. 30
// days leaves time to reach the person who holds the team's Claude account;
// 7 days is the last call.
var credentialExpiryReminders = []struct {
	before       time.Duration
	sentAtColumn string
}{
	{30 * 24 * time.Hour, "expiry_reminder_30_days_before_sent_at"},
	{7 * 24 * time.Hour, "expiry_reminder_7_days_before_sent_at"},
}

// signInCheckStoppedForTriageSession is why a sign-in check ended when
// MMHotline stopped it because a triage session needs the one run.
const signInCheckStoppedForTriageSession = "MMHotline stopped the sign-in check because a triage session needed the agent. The sign-in check runs again once the triage session's run ends."

// agentRole is a job a coding agent does in MMHotline: the triage agent's
// or the review agent's. Each has its own setup, setup test, and sign-in
// check.
type agentRole struct {
	// name is the agent for people, such as "triage agent".
	name string
	// commandGroup is the agent's group of mmhotline commands.
	commandGroup string
	// settingsKey is the agent's section of the settings file.
	settingsKey string
	setupTable  string
	// runPurposes are the purposes of the agent's runs in triage_agent_runs.
	runPurposes        []string
	setupTestPurpose   string
	signInCheckPurpose string
	setupTestPrompt    string
	// whileSignInFails says what stops working while the sign-in fails.
	whileSignInFails string
}

var triageAgentRole = agentRole{
	name: "triage agent", commandGroup: "triage-agent", settingsKey: "triage_agent", setupTable: "triage_agent_setup",
	runPurposes:      []string{"setup_test", "sign_in_check", "triage_session", "software_review"},
	setupTestPurpose: "setup_test", signInCheckPurpose: "sign_in_check", setupTestPrompt: triageAgentSetupTestPrompt,
	whileSignInFails: "While the sign-in check fails, the triage agent cannot look into alerts or review the servers' software. An SOS wakes the person on call as soon as the triage agent's run fails.",
}

var reviewAgentRole = agentRole{
	name: "review agent", commandGroup: "review-agent", settingsKey: "review_agent", setupTable: "review_agent_setup",
	runPurposes:      []string{"review_agent_setup_test", "review_agent_sign_in_check", "playbook_review"},
	setupTestPurpose: "review_agent_setup_test", signInCheckPurpose: "review_agent_sign_in_check", setupTestPrompt: reviewAgentSetupTestPrompt,
	whileSignInFails: "While the sign-in check fails, the review agent cannot review playbooks, so each playbook the triage agent asks to run waits for a person.",
}

// agentRoles are both jobs, the triage agent's first: when both sign-in
// checks are due, the triage agent's runs first.
var agentRoles = []agentRole{triageAgentRole, reviewAgentRole}

// agentRoleOf is the job a run of purpose is for.
func agentRoleOf(purpose string) agentRole {
	if reviewAgentPurpose(purpose) {
		return reviewAgentRole
	}
	return triageAgentRole
}

// codingAgentTitle names a coding agent for people, such as "Claude Code".
func codingAgentTitle(name string) string {
	for _, agent := range triageagent.CodingAgents() {
		if agent.Name == name {
			return agent.Title
		}
	}
	return name
}

// credentialExpiresOnLayout is credential_expires_on's form, such as
// 2027-02-06, as ChatGPT's dialog shows a Codex access token's end date.
const credentialExpiresOnLayout = "2006-01-02"

// credentialExpiresAt is when a credential of credentialType that applied at
// setUpAt stops working: a year later for a subscription token; the start of
// the day expiresOn, in UTC, for a Codex access token, since the token may
// end at any time that day; and zero for an API key or a ChatGPT sign-in,
// whose end MMHotline does not know.
func credentialExpiresAt(credentialType string, setUpAt time.Time, expiresOn string) time.Time {
	switch credentialType {
	case "subscription_token":
		return setUpAt.Add(subscriptionTokenLifetime).UTC()
	case "access_token":
		// The settings file's check refuses any other form.
		day, _ := time.Parse(credentialExpiresOnLayout, expiresOn)
		return day
	}
	return time.Time{}
}

// credentialState is what the background loop reads of an agent's setup.
type credentialState struct {
	codingAgent, credentialType, expiresOn string
	setUpAt                                time.Time
	// reminderSent tells, for each of credentialExpiryReminders, whether it
	// was sent.
	reminderSent []bool
}

func (state credentialState) expiresAt() time.Time {
	return credentialExpiresAt(state.credentialType, state.setUpAt, state.expiresOn)
}

// readCredentialState reads role's setup; nil when the agent is not set up.
func readCredentialState(ctx context.Context, tx *sql.Tx, role agentRole) (*credentialState, error) {
	state := credentialState{reminderSent: make([]bool, len(credentialExpiryReminders))}
	var setUpAt int64
	columns, destinations := "coding_agent,credential_type,credential_expires_on,set_up_at", []any{&state.codingAgent, &state.credentialType, &state.expiresOn, &setUpAt}
	for index, reminder := range credentialExpiryReminders {
		columns += "," + reminder.sentAtColumn + "!=0"
		destinations = append(destinations, &state.reminderSent[index])
	}
	err := tx.QueryRowContext(ctx, `SELECT `+columns+` FROM `+role.setupTable+` WHERE id=1`).Scan(destinations...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	state.setUpAt = time.Unix(setUpAt, 0).UTC()
	return &state, err
}

// nextExpiryReminderAt is when the earliest reminder not yet sent is due,
// which may have passed; zero when the credential does not expire or every
// reminder was sent.
func (state credentialState) nextExpiryReminderAt() time.Time {
	expiresAt := state.expiresAt()
	if expiresAt.IsZero() {
		return time.Time{}
	}
	for index, reminder := range credentialExpiryReminders {
		if !state.reminderSent[index] {
			return expiresAt.Add(-reminder.before)
		}
	}
	return time.Time{}
}

// sendExpiryReminder tells administrators that role's subscription token or
// access token expires soon, and records as sent every reminder whose time
// has come. When MMHotline was stopped past both reminder times, one
// reminder goes out, as both would say the same.
func (a *App) sendExpiryReminder(ctx context.Context, tx *sql.Tx, role agentRole, state credentialState, now time.Time) error {
	expiresAt := state.expiresAt()
	for index, reminder := range credentialExpiryReminders {
		if state.reminderSent[index] || expiresAt.Add(-reminder.before).After(now) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+role.setupTable+` SET `+reminder.sentAtColumn+`=? WHERE id=1`, now.Unix()); err != nil {
			return err
		}
	}
	title := fmt.Sprintf("The %s's %s token expires on %s", role.name, codingAgentTitle(state.codingAgent), expiresAt.Format(credentialExpiresOnLayout))
	message := fmt.Sprintf("The %s signs in to %s with a subscription token from claude setup-token. A subscription token lasts one year. MMHotline counts the year from when the settings file's token applied, at %s, so the token stops working at %s. Renew the token before the expiry time:\n\n%s",
		role.name, codingAgentTitle(state.codingAgent), displayTime(state.setUpAt), displayTime(expiresAt), credentialRenewalSteps(role, state.codingAgent, state.credentialType))
	if state.credentialType == "access_token" {
		message = fmt.Sprintf("The %s signs in to %s with a Codex access token, which ends on %s, the date in the settings file's %s.credential_expires_on. Renew the token before that day:\n\n%s",
			role.name, codingAgentTitle(state.codingAgent), state.expiresOn, role.settingsKey, credentialRenewalSteps(role, state.codingAgent, state.credentialType))
	}
	return RecordAdministratorNotice(ctx, tx, "warn", title, message, now)
}

// credentialRenewalSteps says how an administrator gives role a working
// credential again: through the settings file, as docs/how-to/keep-the-agents-signed-in.md says.
func credentialRenewalSteps(role agentRole, codingAgent, credentialType string) string {
	vendor := map[string]string{"claude_code": "Anthropic", "codex": "OpenAI"}[codingAgent]
	first := "1. Make a new " + vendor + " API key, unless the old API key still works and only the API account needs credit."
	second := "2. Put the new credential in " + role.settingsKey + ".credential in the settings file, " + InstalledSettingsFilePath + ": use your deploy tool's vault, or use sudoedit on the MMHotline server."
	last := ""
	switch credentialType {
	case "subscription_token":
		first = "1. On a computer with Claude Code, signed in to the team's Claude account, make a new token:\n   claude setup-token"
		last = "\n\nA new token starts a new year."
	case "access_token":
		first = "1. In ChatGPT, make a new Codex access token on the Access tokens page. Choose the longest end date your workspace allows."
		second = "2. Put the new token in " + role.settingsKey + ".credential, and its end date in " + role.settingsKey + ".credential_expires_on, in the settings file, " + InstalledSettingsFilePath + ": use your deploy tool's vault, or use sudoedit on the MMHotline server."
	case "chatgpt_sign_in":
		first = "1. On your own computer, sign in to Codex again, just for MMHotline, in a new empty folder:\n   export CODEX_HOME=\"$(mktemp -d)\"\n   codex login\n   cat \"$CODEX_HOME/auth.json\"\n   rm -rf \"$CODEX_HOME\" && unset CODEX_HOME\n   Do not run codex logout: it would end the new sign-in too."
		second = "2. Put the printed auth.json text in " + role.settingsKey + ".credential in the settings file, " + InstalledSettingsFilePath + ": use your deploy tool's vault, or use sudoedit on the MMHotline server."
		last = "\n\nUse this sign-in only for MMHotline's " + role.name + ". Any other use of it ends it."
	}
	return first + "\n" + second +
		"\n3. Apply the settings file. MMHotline tests the new credential before saving the credential. Use your deploy tool, or run the command on the MMHotline server:\n   sudo mmhotline settings apply" + last
}

// chatGPTSignInEndedCause names, in plain words, why OpenAI ended a ChatGPT
// sign-in, from the refresh failure Codex reports: Codex prints a sentence
// for each of OpenAI's codes (openai/codex rust-v0.166.0,
// login/src/auth/manager.rs, classify_refresh_token_failure). It is empty
// for any other failure.
func chatGPTSignInEndedCause(reason string) string {
	for _, cause := range []struct{ code, sentence, plain string }{
		{"refresh_token_reused", "refresh token was already used", "Another copy of this ChatGPT sign-in was used, so OpenAI ended this one. Sign in again just for MMHotline, and use that sign-in nowhere else."},
		{"refresh_token_expired", "refresh token has expired", "OpenAI ended this ChatGPT sign-in because its refresh token expired. Sign in again just for MMHotline."},
		{"refresh_token_invalidated", "refresh token was revoked", "OpenAI revoked this ChatGPT sign-in, for example after codex logout on a computer that had a copy of it. Sign in again just for MMHotline."},
	} {
		if strings.Contains(reason, cause.code) || strings.Contains(reason, cause.sentence) {
			return cause.plain
		}
	}
	return ""
}

// signInStatus is what an agent's latest evidence says about its sign-in.
type signInStatus struct {
	lastCheckedAt time.Time
	works         bool
	failureReason string
}

// readSignInStatus reads role's latest evidence about its sign-in: its
// latest run that succeeded, or its latest sign-in check that failed,
// whichever ended later. The set-up counts as a check at setUpAt, as its
// setup test passed then. A real run that failed proves nothing, since it
// can fail for other reasons, and neither does a check that MMHotline's own
// stop, its restart, or a triage session cut short.
func readSignInStatus(ctx context.Context, tx *sql.Tx, role agentRole, setUpAt time.Time) (signInStatus, error) {
	status := signInStatus{lastCheckedAt: setUpAt, works: true}
	arguments := []any{}
	for _, purpose := range role.runPurposes {
		arguments = append(arguments, purpose)
	}
	arguments = append(arguments, role.signInCheckPurpose, runCutByMMHotlineStop, runCutByMMHotlineRestart)
	var endedAt int64
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT ended_at,failure_reason FROM triage_agent_runs WHERE purpose IN (?`+strings.Repeat(",?", len(role.runPurposes)-1)+`) AND ended_at>0
 AND ((state='finished' AND failure_reason='') OR (purpose=? AND state IN ('finished','failed') AND end_cause NOT IN (?,?)))
 ORDER BY ended_at DESC,rowid DESC LIMIT 1`, arguments...).Scan(&endedAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if at := time.Unix(endedAt, 0).UTC(); err == nil && !at.Before(setUpAt) {
		status = signInStatus{lastCheckedAt: at, works: reason == "", failureReason: reason}
	}
	return status, err
}

// nextCodingAgentCredentialDeadline returns when the background loop next
// has credential work: the next expiry reminder of either agent, and, while
// the one triage run is free, the next sign-in check.
func (a *App) nextCodingAgentCredentialDeadline(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	canCheck := a.oneTriageRunIsFree()
	var deadlines []time.Time
	for _, role := range agentRoles {
		state, err := readCredentialState(ctx, tx, role)
		if err != nil || state == nil {
			if err != nil {
				return time.Time{}, err
			}
			continue
		}
		deadlines = append(deadlines, state.nextExpiryReminderAt())
		if canCheck {
			status, err := readSignInStatus(ctx, tx, role, state.setUpAt)
			if err != nil {
				return time.Time{}, err
			}
			deadlines = append(deadlines, status.lastCheckedAt.Add(signInCheckInterval))
		}
	}
	return earliest(deadlines...), nil
}

// DriveCodingAgentCredentials is the background loop's turn for the agents'
// credentials: it sends the expiry reminders that are due, and starts one
// sign-in check that is due beside the loop.
func (a *App) DriveCodingAgentCredentials(ctx context.Context) error {
	now := a.Now().UTC()
	var check *agentRole
	canCheck := a.oneTriageRunIsFree()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for _, role := range agentRoles {
			state, err := readCredentialState(ctx, tx, role)
			if err != nil || state == nil {
				if err != nil {
					return err
				}
				continue
			}
			if at := state.nextExpiryReminderAt(); !at.IsZero() && !at.After(now) {
				if err = a.sendExpiryReminder(ctx, tx, role, *state, now); err != nil {
					return err
				}
			}
			if canCheck && check == nil {
				status, err := readSignInStatus(ctx, tx, role, state.setUpAt)
				if err != nil {
					return err
				}
				if !status.lastCheckedAt.Add(signInCheckInterval).After(now) {
					check = &role
				}
			}
		}
		return nil
	})
	if err != nil || check == nil {
		return err
	}
	role := *check
	a.startBesideTheLoop(func() {
		// A run that started meanwhile has the one run; the check stays due
		// for a later visit.
		if _, err := a.startSignInCheck(ctx, role, "MMHotline", nil); err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "mmhotline: could not start the %s's sign-in check: %s\n", role.name, err)
		}
	})
	return nil
}

// CheckSignInNow starts role's sign-in check at once for an administrator,
// from the triage agent page or the command. It is refused while a run is
// active, and a triage session that needs the run stops it, as it stops any
// check. It counts as any check: the next is due a day after it.
func (a *App) CheckSignInNow(ctx context.Context, p Principal, role agentRole) (string, error) {
	var person Member
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { var err error; person, err = RequireMember(ctx, tx, p, true); return err }); err != nil {
		return "", err
	}
	return a.startSignInCheck(ctx, role, person.Name, func(tx *sql.Tx) error {
		_, err := RequireMember(ctx, tx, p, true)
		return err
	})
}

// savedAgentRequest is role's saved setup as a run request: its coding
// agent, credential, model, and reasoning effort, which every run of the
// agent takes. For a ChatGPT sign-in, startTriageAgentRun puts in the newest
// copy once the run holds its place. It is ErrInvalid when the agent is not
// set up.
func (a *App) savedAgentRequest(ctx context.Context, role agentRole, action, prompt string) (triageagent.Request, error) {
	request := triageagent.Request{Action: action, Prompt: prompt}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT coding_agent,credential_type,credential,model,reasoning_effort FROM `+role.setupTable+` WHERE id=1`).Scan(&request.CodingAgent, &request.CredentialType, &request.Credential, &request.Model, &request.ReasoningEffort)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return request, fmt.Errorf("%w: the %s is not set up", ErrInvalid, role.name)
	}
	return request, err
}

// saveCodingAgentSetup saves role's setup from the settings file, after its
// setup test passed. A subscription token lasts a year from when its
// credential applied, so a change of only the model or the reasoning effort
// keeps that time and the reminders already sent; a new credential, or a
// new end date, starts the year, the reminders, and the sign-in alert
// again. A ChatGPT sign-in's current copy is the one the test gave back,
// never the file's, since the test may have refreshed it.
func saveCodingAgentSetup(tx *sql.Tx, role agentRole, setup SettingsCodingAgent, chatGPTSignIn string, now int64, by string) error {
	const same = `(credential=excluded.credential AND credential_type=excluded.credential_type AND coding_agent=excluded.coding_agent AND credential_expires_on=excluded.credential_expires_on)`
	keep := func(column string) string {
		return column + `=CASE WHEN ` + same + ` THEN ` + column + ` ELSE excluded.` + column + ` END`
	}
	if setup.CredentialType != "chatgpt_sign_in" {
		chatGPTSignIn = ""
	}
	_, err := tx.Exec(`INSERT INTO `+role.setupTable+`(id,coding_agent,credential_type,credential,credential_expires_on,current_chatgpt_sign_in,model,reasoning_effort,set_up_at,set_up_by) VALUES(1,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET `+keep("set_up_at")+`,`+keep("expiry_reminder_30_days_before_sent_at")+`,`+keep("expiry_reminder_7_days_before_sent_at")+`,`+keep("sign_in_failing_alert_sent")+`,
 coding_agent=excluded.coding_agent,credential_type=excluded.credential_type,credential=excluded.credential,credential_expires_on=excluded.credential_expires_on,current_chatgpt_sign_in=excluded.current_chatgpt_sign_in,model=excluded.model,reasoning_effort=excluded.reasoning_effort,set_up_by=excluded.set_up_by`,
		setup.CodingAgent, setup.CredentialType, setup.Credential, setup.CredentialExpiresOn, chatGPTSignIn, setup.Model, setup.ReasoningEffort, now, by)
	if err != nil {
		return err
	}
	// A credential that applies with less time left than a reminder gives
	// counts that reminder as sent, so only the last one goes out: a 20-day
	// access token gets the 7-day reminder, not a 30-day one at once. Only
	// for a new credential, whose set_up_at is now.
	expiresAt := credentialExpiresAt(setup.CredentialType, time.Unix(now, 0), setup.CredentialExpiresOn)
	for _, reminder := range credentialExpiryReminders[:len(credentialExpiryReminders)-1] {
		if !expiresAt.IsZero() && !expiresAt.Add(-reminder.before).After(time.Unix(now, 0)) {
			if _, err = tx.Exec(`UPDATE `+role.setupTable+` SET `+reminder.sentAtColumn+`=? WHERE id=1 AND set_up_at=? AND `+reminder.sentAtColumn+`=0`, now, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// startSignInCheck runs role's setup test prompt with the saved setup,
// started by startedBy, when the one triage run is free. authorize, when
// set, runs in the transaction that records the run.
func (a *App) startSignInCheck(ctx context.Context, role agentRole, startedBy string, authorize func(*sql.Tx) error) (string, error) {
	request, err := a.savedAgentRequest(ctx, role, "start", role.setupTestPrompt)
	if err != nil {
		return "", err
	}
	run, err := a.startTriageAgentRun(ctx, role.signInCheckPurpose, startedBy, request, nil, func(tx *sql.Tx, _ string) error {
		if authorize == nil {
			return nil
		}
		return authorize(tx)
	})
	if err != nil {
		return "", err
	}
	return run.id, nil
}

// recordSignInOutcome tells administrators once when the sign-in of the
// run's agent stops working, and once when it works again. Any run of the
// agent that succeeded shows that it works; only a sign-in check shows that
// it does not. It runs in the transaction that records the run's end, before
// a passing setup test replaces the setup, so a new set-up that fixes a
// failing sign-in says so.
func (a *App) recordSignInOutcome(ctx context.Context, tx *sql.Tx, run *triageAgentActiveRun, event triageagent.Event, cause runEndCause, now time.Time) error {
	role := agentRoleOf(run.purpose)
	succeeded := event.Event == "finished" && event.Reason == ""
	failed := run.purpose == role.signInCheckPurpose && !succeeded && event.Event != "stopped" && cause == runEndedOnItsOwn
	if !succeeded && !failed {
		return nil
	}
	var codingAgent string
	var failingAlertSent bool
	err := tx.QueryRowContext(ctx, `SELECT coding_agent,sign_in_failing_alert_sent FROM `+role.setupTable+` WHERE id=1`).Scan(&codingAgent, &failingAlertSent)
	if errors.Is(err, sql.ErrNoRows) || err == nil && failingAlertSent == failed {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE `+role.setupTable+` SET sign_in_failing_alert_sent=? WHERE id=1`, failed); err != nil {
		return err
	}
	if failed {
		title := fmt.Sprintf("The %s cannot sign in to %s", role.name, codingAgentTitle(codingAgent))
		cause := ""
		if run.request.CredentialType == "chatgpt_sign_in" {
			if cause = chatGPTSignInEndedCause(event.Reason); cause != "" {
				cause = "\n\n" + cause
			}
		}
		message := fmt.Sprintf("MMHotline's sign-in check of the %s failed at %s: %s%s\n\n%s\n\nIf the agent's credential no longer works, replace the credential:\n%s\n\nAfter another fix, check again on the Triage agent page, or on the MMHotline server:\n   sudo mmhotline %s check-sign-in",
			role.name, displayTime(now), truncateRunes(event.Reason, 2000), cause, role.whileSignInFails, credentialRenewalSteps(role, codingAgent, run.request.CredentialType), role.commandGroup)
		return RecordAdministratorNotice(ctx, tx, "warn", title, message, now)
	}
	proof := "a run of the agent finished"
	switch {
	case signInCheckPurpose(run.purpose):
		proof = "the agent's sign-in check passed"
	case testRunPurpose(run.purpose):
		proof = "the agent's setup from the settings file passed the setup test"
	}
	codingAgent = codingAgentTitle(run.request.CodingAgent)
	title := fmt.Sprintf("The %s can sign in to %s again", role.name, codingAgent)
	message := fmt.Sprintf("The %s's sign-in to %s works again: %s at %s.", role.name, codingAgent, proof, displayTime(now))
	return RecordAdministratorNotice(ctx, tx, "info", title, message, now)
}
