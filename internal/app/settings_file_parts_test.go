package app

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/fakesendingservices"
	"mmhotline/internal/statuspage"
	"mmhotline/internal/triageagent"
)

// settingsPartNamed is the settings file's part of that name.
func settingsPartNamed(t *testing.T, name string) settingsFilePart {
	t.Helper()
	for _, part := range settingsFileParts {
		if part.name == name {
			return part
		}
	}
	t.Fatalf("no settings file part %s", name)
	return settingsFilePart{}
}

// withoutSection is the settings with the top-level section key left out.
func withoutSection(t *testing.T, text, key string) string {
	t.Helper()
	start := strings.Index(text, "\n"+key+":")
	if start < 0 {
		t.Fatalf("the settings have no section %s", key)
	}
	end := start + 1
	for {
		next := strings.Index(text[end:], "\n")
		if next < 0 {
			return text[:start+1]
		}
		end += next + 1
		if end < len(text) && text[end] != ' ' && text[end] != '\n' {
			return text[:start+1] + text[end:]
		}
	}
}

const triageAgentSetupPartName = "triage_agent.coding_agent, triage_agent.credential_type, triage_agent.credential, triage_agent.credential_expires_on, triage_agent.model, triage_agent.reasoning_effort"

// TestSettingsFileAppliesTheAgentsThroughTheirSetupTests: each agent's setup
// applies only through its setup test, run with the file's coding agent,
// credential, model, and reasoning effort; a failed test keeps the earlier
// setup, an unchanged part runs no test, another active triage run fails
// the part with its reason, and a section left out removes the setup. A
// subscription token's year counts from when the file's credential applied.
func TestSettingsFileAppliesTheAgentsThroughTheirSetupTests(t *testing.T) {
	ctx := context.Background()
	root := Principal{UnixUser: "root"}
	clock := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.a.Now = func() time.Time { return clock }
	f.start()
	requests := f.requests()
	if len(requests) != 2 {
		t.Fatalf("setup tests at start: %+v", requests)
	}
	if triage := requests[0]; triage.AgentRole != triageagent.TriageAgentRole || triage.Prompt != triageAgentSetupTestPrompt || triage.CodingAgent != "claude_code" || triage.CredentialType != "subscription_token" || triage.Credential != "sk-ant-oat01-example-triage-agent-credential" || triage.Model != "opus" || triage.ReasoningEffort != "high" {
		t.Fatalf("the triage agent's setup test: %+v", triage)
	}
	if review := requests[1]; review.AgentRole != triageagent.ReviewAgentRole || review.Prompt != reviewAgentSetupTestPrompt || review.CodingAgent != "codex" || review.CredentialType != "api_key" || review.Credential != "sk-example-review-agent-credential" || review.Model != "gpt-5-codex" || review.ReasoningEffort != "high" {
		t.Fatalf("the review agent's setup test: %+v", review)
	}
	triageSetup := func() *TriageAgentSetup {
		t.Helper()
		view, err := f.a.TriageAgentState(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		return view.Setup
	}
	setup := triageSetup()
	if setup == nil || setup.Model != "opus" || setup.ReasoningEffort != "high" || setup.SetUpBy != "the settings file "+f.path || !setup.SetUpAt.Equal(clock) || setup.CredentialExpiresAt == nil || !setup.CredentialExpiresAt.Equal(clock.Add(365*24*time.Hour)) {
		t.Fatalf("the triage agent's setup: %+v", setup)
	}
	firstAppliedAt := clock

	// An apply with nothing changed runs no setup test.
	if view := f.apply(); states(view)[triageAgentSetupPartName] != "unchanged" || states(view)["review_agent"] != "unchanged" || len(f.requests()) != 2 {
		t.Fatalf("an unchanged apply: %v, %d runs", states(view), len(f.requests()))
	}

	// A change of the model alone is tested with the new model and saved;
	// the token's year still counts from when the credential applied.
	clock = clock.Add(24 * time.Hour)
	f.write(replaced(t, exampleSettingsYAML, "  model: opus\n", "  model: opus-next\n"))
	if view := f.apply(); states(view)[triageAgentSetupPartName] != "applied" || states(view)["review_agent"] != "unchanged" {
		t.Fatalf("a new model: %v", states(view))
	}
	if last := f.requests()[len(f.requests())-1]; last.Model != "opus-next" || last.ReasoningEffort != "high" {
		t.Fatalf("the new model's setup test: %+v", last)
	}
	if setup = triageSetup(); setup.Model != "opus-next" || !setup.SetUpAt.Equal(firstAppliedAt) || !setup.CredentialExpiresAt.Equal(firstAppliedAt.Add(365*24*time.Hour)) {
		t.Fatalf("after a new model: %+v", setup)
	}

	// A model the service refuses fails the part, and the earlier setup stays.
	f.answerRuns(func(triageagent.Request) triageagent.Event {
		return triageagent.Event{Event: "failed", Reason: "There is an issue with the selected model (opus-typo). It may not exist."}
	})
	f.write(replaced(t, exampleSettingsYAML, "  model: opus\n", "  model: opus-typo\n"))
	view := f.apply()
	if got, want := states(view)[triageAgentSetupPartName], "failed: the triage agent's setup test, with the file's settings, failed: There is an issue with the selected model (opus-typo). It may not exist"; got != want {
		t.Fatalf("a refused model: %q, want %q", got, want)
	}
	if !strings.Contains(view.Text(), triageAgentSetupPartName+": did not apply: the triage agent's setup test, with the file's settings, failed: There is an issue with the selected model (opus-typo). It may not exist. The earlier setting, applied at ") {
		t.Fatalf("apply's text:\n%s", view.Text())
	}
	if setup = triageSetup(); setup.Model != "opus-next" {
		t.Fatalf("a refused model replaced the setup: %+v", setup)
	}

	// A new credential starts a new year.
	f.answerRuns(nil)
	clock = clock.Add(24 * time.Hour)
	f.write(replaced(t, exampleSettingsYAML, "sk-ant-oat01-example-triage-agent-credential", "sk-ant-oat01-example-new-token"))
	if view := f.apply(); states(view)[triageAgentSetupPartName] != "applied" {
		t.Fatalf("a new credential: %v", states(view))
	}
	if setup = triageSetup(); setup.Model != "opus" || !setup.SetUpAt.Equal(clock) || !setup.CredentialExpiresAt.Equal(clock.Add(365*24*time.Hour)) {
		t.Fatalf("after a new credential: %+v", setup)
	}

	// While another triage agent run holds the one run, the setup test
	// cannot start: the part fails with that reason, and a later apply
	// tests it.
	hold := make(chan struct{})
	f.answerRuns(func(request triageagent.Request) triageagent.Event {
		<-hold
		return setupTestPassed(request)
	})
	if _, err := f.a.CheckSignInNow(ctx, root, triageAgentRole); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(f.requests()) != 6; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the sign-in check did not reach the sandbox")
		}
	}
	f.write(replaced(t, exampleSettingsYAML, "sk-ant-oat01-example-triage-agent-credential", "sk-ant-oat01-example-new-token", "  model: opus\n", "  model: opus-later\n"))
	if got, want := states(f.apply())[triageAgentSetupPartName], "failed: a triage agent run was active, so the new agent setup was not tested. Apply the settings file again after the triage agent run ends"; got != want {
		t.Fatalf("during another run: %q, want %q", got, want)
	}
	close(hold)
	waitNoTriageRun(t, f.a)
	f.answerRuns(nil)
	if view := f.apply(); states(view)[triageAgentSetupPartName] != "applied" || triageSetup().Model != "opus-later" {
		t.Fatalf("after the run ended: %v", states(view))
	}

	// A file without the agents' sections removes their setups.
	f.write(withoutSection(t, withoutSection(t, exampleSettingsYAML, "triage_agent"), "review_agent"))
	view = f.apply()
	if states(view)[triageAgentSetupPartName] != "applied" || states(view)["review_agent"] != "applied" || triageSetup() != nil {
		t.Fatalf("without the agents: %v", states(view))
	}
	if review, err := f.a.ReviewAgentState(ctx, root); err != nil || review.Setup != nil {
		t.Fatalf("the review agent stayed: %+v %v", review.Setup, err)
	}
}

// TestEveryAgentRunCarriesTheModelAndReasoningEffort: every run of an agent
// takes its setup's model and reasoning effort, which the sandbox passes to
// the coding agent (TestSandboxPassesModelAndReasoningEffort): the triage
// agent's software reviews, triage sessions, and sign-in checks, and the
// review agent's reviews and sign-in checks. The setup tests take the
// file's (TestSettingsFileAppliesTheAgentsThroughTheirSetupTests).
func TestEveryAgentRunCarriesTheModelAndReasoningEffort(t *testing.T) {
	ctx := context.Background()
	r := newSoftwareReviewRig(t)
	// Credentials whose sign-in check runs the coding agent: an API key's
	// asks the models list and runs no model.
	useCodexAccessToken(t, r.a)
	if _, err := r.a.Store.db.Exec(`INSERT INTO review_agent_setup(id,coding_agent,credential_type,credential,model,reasoning_effort,set_up_at,set_up_by) VALUES(1,'claude_code','subscription_token','review-key','review-model','medium',1,'administrator')`); err != nil {
		t.Fatal(err)
	}
	check := func(kind string, request triageagent.Request, role, model, effort string) {
		t.Helper()
		if request.AgentRole != role || request.Model != model || request.ReasoningEffort != effort {
			t.Errorf("%s: role %q, model %q, reasoning effort %q", kind, request.AgentRole, request.Model, request.ReasoningEffort)
		}
	}
	r.rounds(t)
	review := r.nextReview(t)
	check("a software review", review.request, triageagent.TriageAgentRole, testAgentModel, testAgentReasoningEffort)
	r.end(t, review, triageagent.Event{Event: "finished", Answer: "Nothing to report."})

	r.alert(t, "sos", "Database on db001 is down", "db001")
	r.drive(t)
	session := r.nextRun(t)
	check("a triage session", session.request, triageagent.TriageAgentRole, testAgentModel, testAgentReasoningEffort)
	r.end(t, session, triageagent.Event{Event: "finished", Answer: "Reported."})

	for _, role := range agentRoles {
		if _, err := r.a.CheckSignInNow(ctx, r.p, role); err != nil {
			t.Fatal(err)
		}
		signInCheck := r.nextRun(t)
		if role.name == triageAgentRole.name {
			check("the triage agent's sign-in check", signInCheck.request, triageagent.TriageAgentRole, testAgentModel, testAgentReasoningEffort)
		} else {
			check("the review agent's sign-in check", signInCheck.request, triageagent.ReviewAgentRole, "review-model", "medium")
		}
		r.end(t, signInCheck, setupTestPassed(signInCheck.request))
	}

	agent, set, err := r.a.savedReviewAgent(ctx)
	if err != nil || !set {
		t.Fatal(set, err)
	}
	reviewed := make(chan playbookReview, 1)
	go func() { reviewed <- r.a.reviewPlaybook(ctx, agent, "Review this playbook.") }()
	playbookReview := r.nextRun(t)
	check("a review of a playbook", playbookReview.request, triageagent.ReviewAgentRole, "review-model", "medium")
	sendTriageEvent(t, playbookReview, triageagent.Event{Event: "finished", Answer: "APPROVE\nIt only frees disk space."})
	if result := <-reviewed; result.outcome != reviewApproved {
		t.Fatalf("review %+v", result)
	}
}

// fiveSendingServices is the example with each sending service, as the
// fake sending services and the SMTP fixture at smtpServer accept them.
func fiveSendingServices(t *testing.T, smtpServer string) string {
	return replaced(t, exampleSettingsYAML, `    application_token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
`, `    application_token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
  twilio:
    account_sid: `+fakesendingservices.TwilioAccountSID+`
    api_key_sid: `+fakesendingservices.TwilioAPIKeySID+`
    api_key_secret: `+fakesendingservices.TwilioAPIKeySecret+`
    phone_number_to_call_from: "`+fakesendingservices.TwilioPhoneNumberToCallFrom+`"
    extra_phone_numbers_on_the_contact_card: ["+12025550199"]
  telegram:
    bot_token: "`+fakeNumberedTelegramToken+`"
  slack:
    bot_token: `+fakesendingservices.SlackBotToken+`
    app_level_token: `+fakesendingservices.SlackAppLevelToken+`
  mail_server:
    smtp_server: `+smtpServer+`
    smtp_connection_security: unencrypted
    email_from_address: alerts@example.com
`)
}

// TestSettingsFileAppliesEachSendingService: each sending service applies
// through its own check against the fake sending services and shows its
// next step when newly set up; a failing check keeps the earlier setup; an
// unchanged service is not checked again; a service left out is removed,
// and refused while contact methods use it.
func TestSettingsFileAppliesEachSendingService(t *testing.T) {
	ctx := context.Background()
	root := Principal{UnixUser: "root"}
	smtp := newSMTPFixture(t, "250 accepted", "", false, nil)
	f := newSettingsFileFixture(t, fiveSendingServices(t, smtp.server.Addr().String()))
	f.start()
	setUp := func() map[string]SendingServiceSetup {
		t.Helper()
		setups, err := f.a.SendingServiceSetups(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]SendingServiceSetup{}
		for _, setup := range setups {
			byName[setup.SendingService] = setup
		}
		return byName
	}
	shown, err := f.a.SettingsFileState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"pushover", "twilio", "telegram", "slack", "mail_server"} {
		part := "sending_services." + service
		if states(shown)[part] != "applied" || !setUp()[service].IsSetUp || setUp()[service].SetUpBy != "the settings file "+f.path {
			t.Fatalf("%s: %v %+v", part, states(shown), setUp()[service])
		}
		_, definition, _ := sendingServiceSetup(service)
		if !strings.Contains(shown.Text(), part+": applied at ") || !strings.Contains(shown.Text(), "\n  "+part+": Next step: "+definition.NextStep) {
			t.Fatalf("settings show lacks %s's next step:\n%s", part, shown.Text())
		}
	}
	if twilio := setUp()["twilio"]; twilio.Settings["extra_phone_numbers_on_the_contact_card"] != "+12025550199" || twilio.Settings["twilio_phone_number_to_call_from"] != fakesendingservices.TwilioPhoneNumberToCallFrom {
		t.Fatalf("Twilio %+v", twilio.Settings)
	}
	commands, _ := smtp.snapshot()
	smtpChecks := strings.Count(strings.Join(commands, "\n"), "QUIT")

	// Nothing changed: nothing is checked again, and the next steps are gone.
	view := f.apply()
	for _, service := range []string{"pushover", "twilio", "telegram", "slack", "mail_server"} {
		if states(view)["sending_services."+service] != "unchanged" {
			t.Fatalf("%s: %v", service, states(view))
		}
	}
	if commands, _ = smtp.snapshot(); strings.Count(strings.Join(commands, "\n"), "QUIT") != smtpChecks || strings.Contains(view.Text(), "Next step") {
		t.Fatalf("an unchanged apply checked the mail server again or showed a next step:\n%s", view.Text())
	}

	// A mail server that refuses the check fails the part, and the earlier
	// setup stays.
	f.write(replaced(t, fiveSendingServices(t, smtp.server.Addr().String()), "smtp_server: "+smtp.server.Addr().String(), "smtp_server: 127.0.0.1:1"))
	view = f.apply()
	if !strings.HasPrefix(states(view)["sending_services.mail_server"], "failed: ") || setUp()["mail_server"].Settings["smtp_server"] != smtp.server.Addr().String() {
		t.Fatalf("a refused mail server: %v %+v", states(view), setUp()["mail_server"].Settings)
	}

	// A service that a contact method uses cannot be left out.
	if _, err := f.a.SaveContactMethod(ctx, Principal{SignInEmailAddress: "ana@example.com"}, ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}, Subscriptions: []string{"sos"}}); err != nil {
		t.Fatal(err)
	}
	withoutPushover := replaced(t, fiveSendingServices(t, smtp.server.Addr().String()), "  pushover:\n    application_token: \"azGDORePK8gMaC0QOYAMyEEuzJnyUi\"\n", "")
	f.write(withoutPushover)
	view = f.apply()
	if got, want := states(view)["sending_services.pushover"], "failed: cannot remove the Pushover setup: 1 Pushover notification contact method uses the sending service setup. Remove those contact methods on the Contact methods page, or with mmhotline contact-methods remove --contact-method-id ID, first"; got != want || !setUp()["pushover"].IsSetUp {
		t.Fatalf("Pushover left out while a contact method uses it: %q, want %q", got, want)
	}
	if !strings.Contains(view.Text(), "The earlier setting, applied at ") {
		t.Fatalf("apply's text:\n%s", view.Text())
	}

	// Services no contact method uses are removed when left out: the
	// example has Pushover alone.
	f.write(exampleSettingsYAML)
	view = f.apply()
	for _, service := range []string{"twilio", "telegram", "slack", "mail_server"} {
		if states(view)["sending_services."+service] != "applied" || setUp()[service].IsSetUp {
			t.Fatalf("%s left out: %v %+v", service, states(view), setUp()[service])
		}
	}
}

// TestSettingsFileAppliesTheDownAlarmAndTheStatusPage: the MMHotline-down alarm applies
// through its set-up against the fake Cloudflare API and the status page
// through a post of the current page with the file's R2 key; an unchanged
// apply calls Cloudflare and R2 no more; a section left out removes each.
func TestSettingsFileAppliesTheDownAlarmAndTheStatusPage(t *testing.T) {
	ctx := context.Background()
	root := Principal{UnixUser: "root"}
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	downAlarmView, err := f.a.DownAlarm(ctx, root)
	if err != nil || !downAlarmView.IsSetUp || downAlarmView.CloudflareAccountID != f.fakes.cloudflare.AccountID || downAlarmView.SetUpBy != "the settings file "+f.path {
		t.Fatalf("MMHotline-down alarm %+v %v", downAlarmView, err)
	}
	if worker, ok := f.fakes.cloudflare.Worker(downAlarmView.WorkerName); !ok || worker.Uploads != 1 || len(worker.CheckIns) == 0 {
		t.Fatalf("Worker %+v", worker)
	}
	page, err := f.a.StatusPage(ctx, root)
	if err != nil || !page.IsSetUp || page.PublishedStatusJSON == nil || page.PublishedStatusJSON.PageTitle != "Example Inc." || page.BucketName != settingsFileFakeBucket || page.SetUpBy != "the settings file "+f.path {
		t.Fatalf("status page %+v %v", page, err)
	}
	if index, ok := f.fakes.cloudflare.Object(settingsFileFakeBucket, "index.html"); !ok || !strings.Contains(string(index.Content), "Example Inc.") {
		t.Fatal("no index.html with the file's page title")
	}

	calls := len(f.fakes.cloudflare.Calls())
	if view := f.apply(); states(view)["mmhotline_down_alarm"] != "unchanged" || states(view)["status_page"] != "unchanged" || len(f.fakes.cloudflare.Calls()) != calls {
		t.Fatalf("an unchanged apply: %v, %d new calls", states(view), len(f.fakes.cloudflare.Calls())-calls)
	}

	// A new page title posts the page again; a key R2 refuses fails the
	// part, and the earlier key stays.
	f.write(replaced(t, exampleSettingsYAML, "page_title: Example Inc.", "page_title: Example Incorporated"))
	if view := f.apply(); states(view)["status_page"] != "applied" {
		t.Fatalf("a new page title: %v", states(view))
	}
	object, _ := f.fakes.cloudflare.Object(settingsFileFakeBucket, "status.json")
	var published statuspage.File
	if err := json.Unmarshal(object.Content, &published); err != nil || published.PageTitle != "Example Incorporated" {
		t.Fatalf("page title %q %v", published.PageTitle, err)
	}
	f.write(replaced(t, exampleSettingsYAML, `r2_secret_access_key: "example-r2-secret-access-key"`, `r2_secret_access_key: "another-secret"`, `r2_access_key_id: "0123456789abcdef0123456789abcdef"`, `r2_access_key_id: "anotherkey"`))
	view := f.apply()
	if got := states(view)["status_page"]; !strings.HasPrefix(got, "failed: posting the current status page with the settings file's R2 key failed: R2 refused to ") {
		t.Fatalf("a refused key: %q", got)
	}
	var keyID string
	if err := f.a.Store.db.QueryRow(`SELECT r2_access_key_id FROM status_page_setup`).Scan(&keyID); err != nil || keyID != f.fakes.cloudflare.R2AccessKeyID {
		t.Fatalf("the earlier key did not stay: %q %v", keyID, err)
	}

	// Without the sections, the MMHotline-down alarm holds nothing and its setup is
	// removed, and the status page's setup is removed.
	f.write(withoutSection(t, withoutSection(t, exampleSettingsYAML, "mmhotline_down_alarm"), "status_page"))
	view = f.apply()
	if states(view)["mmhotline_down_alarm"] != "applied" || states(view)["status_page"] != "applied" {
		t.Fatalf("without the sections: %v", states(view))
	}
	if worker, _ := f.fakes.cloudflare.Worker(downAlarmView.WorkerName); len(worker.CheckIns[len(worker.CheckIns)-1]) != 0 {
		t.Fatal("the MMHotline-down alarm still holds messages")
	}
	if downAlarmView, err := f.a.DownAlarm(ctx, root); err != nil || downAlarmView.IsSetUp {
		t.Fatalf("the MMHotline-down alarm stayed: %v", err)
	}
	if page, err := f.a.StatusPage(ctx, root); err != nil || page.IsSetUp {
		t.Fatalf("the status page stayed: %v", err)
	}
}

// The saved listed playbooks equal the file's after each apply: those the
// file lists are saved with their rules, recorded as set by the settings
// file, and shown; a playbook the file stops listing, or of an inventory
// the file stops listing, goes.
func TestListedPlaybooksApplyAsTheFileListsThem(t *testing.T) {
	ctx := context.Background()
	const preset = "    preset: routine_fixes_without_permission\n"
	const staging = "  staging:\n    preset: permission_for_every_fix\n    playbooks:\n      every_server: {rule: get_permission_then_act, approvals_needed: 2}\n"
	both := replaced(t, exampleSettingsYAML, preset, preset+"    playbooks:\n      every_server: {rule: act_then_report}\n      reboot_servers_after_updates: {rule: warn_wait_then_act, warning_wait_minutes: 10}\n      replace_failed_servers:\n        rule: get_permission_then_act\n        approvals_needed: 1\n        its_approval_also_approves_in_the_same_session: [every_server, reboot_servers_after_updates]\n        starts_a_server_replacement: true\n"+staging)
	f := newSettingsFileFixture(t, both)
	f.start()
	saved := func() (map[string]string, string) {
		t.Helper()
		rules, err := f.a.AgentPlaybookRules(ctx, Principal{UnixUser: "root"}, "")
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]string{}
		for _, inventory := range rules {
			for _, row := range inventory.ListedPlaybooks {
				listed[inventory.Inventory+" "+row.PlaybookName] = row.RuleTitle
				if !strings.HasPrefix(row.ChangedBy, "the settings file") {
					t.Errorf("%s %s was changed by %q", inventory.Inventory, row.PlaybookName, row.ChangedBy)
				}
			}
		}
		return listed, strings.Join(agentPlaybookRulesText(rules), "\n")
	}
	listed, text := saved()
	want := map[string]string{"production every_server": "Act, then report", "production reboot_servers_after_updates": "Warn, wait 10 minutes, then act", "production replace_failed_servers": "Get permission, then act (1 approval)", "staging every_server": "Get permission, then act (2 approvals)"}
	if !reflect.DeepEqual(listed, want) || !strings.Contains(text, "\nplaybooks/reboot_servers_after_updates.yml, listed by name: Warn, wait 10 minutes, then act (warn_wait_then_act); changed by the settings file") ||
		!strings.Contains(text, "\nplaybooks/replace_failed_servers.yml, listed by name: Get permission, then act (1 approval) (get_permission_then_act); its approval also approves in the same session: every_server, reboot_servers_after_updates; starts a server replacement; changed by the settings file") {
		t.Fatalf("listed after the first apply: %v\n%s", listed, text)
	}

	// The file stops listing every_server on production and the staging
	// inventory.
	f.write(replaced(t, exampleSettingsYAML, preset, preset+"    playbooks:\n      reboot_servers_after_updates: {rule: warn_wait_then_act, warning_wait_minutes: 10}\n"))
	if view := f.apply(); states(view)["rules_for_running_agent_playbooks"] != "applied" {
		t.Fatalf("apply: %v\n%s", states(view), view.Text())
	}
	if listed, text = saved(); !reflect.DeepEqual(listed, map[string]string{"production reboot_servers_after_updates": "Warn, wait 10 minutes, then act"}) {
		t.Fatalf("listed after the file stopped listing: %v\n%s", listed, text)
	}
}

// TestAnActionKindTheFileLeavesOutIsNamed: a kind of action the file's
// action_kinds leaves out is a note, not a problem, so a later MMHotline's
// new kind never stops an existing file from applying. The kind has no
// saved setting, the safest: it waits for a person. Check, apply, show, the
// page of rules for running the agent's playbooks, and agent-playbook-rules show each name it; listing it
// removes the note.
func TestAnActionKindTheFileLeavesOutIsNamed(t *testing.T) {
	ctx := context.Background()
	const note = "rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one"
	kinds := `    action_kinds:
      start_or_restart_a_service: {rule: act_then_report}
      free_disk_space: {rule: act_then_report}
      pull_a_container_image_again: {rule: act_then_report}
      reboot_a_server: {rule: get_permission_then_act, approvals_needed: 2}
      block_a_network_range: {rule: get_permission_then_act, approvals_needed: 1}
      change_dns_records: {rule: get_permission_then_act, approvals_needed: 1}
      unrecognized_task: {rule: get_permission_then_act, approvals_needed: 1}
`
	withoutCheckOnly := replaced(t, exampleSettingsYAML, "    preset: routine_fixes_without_permission\n", kinds)
	file, problems := ReadSettingsFile(writeSettingsFile(t, withoutCheckOnly), nil)
	var rulesNotes []string
	for _, fileNote := range file.Notes {
		if strings.HasPrefix(fileNote.KeyPath, "rules_for_running_agent_playbooks") {
			rulesNotes = append(rulesNotes, fileNote.String())
		}
	}
	if len(problems) != 0 || len(rulesNotes) != 1 || rulesNotes[0] != note {
		t.Fatalf("check: problems %q, notes %+v", problemTexts(problems), file.Notes)
	}

	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	// A kind that had a saved setting loses it when the file leaves it out.
	f.write(withoutCheckOnly)
	view := f.apply()
	if states(view)["rules_for_running_agent_playbooks"] != "applied" || len(view.FailedParts()) != 0 || !strings.Contains(view.Text(), "rules_for_running_agent_playbooks: applied.\n  "+note+"\n") {
		t.Fatalf("apply: %v\n%s", states(view), view.Text())
	}
	rules, err := f.a.AgentPlaybookRules(ctx, Principal{UnixUser: "root"}, "production")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range rules[0].ActionKinds {
		if kind.ActionKind == actionkinds.CheckOnly && (kind.ChangedAt != nil || kind.AgentPlaybookRule != safestAgentPlaybookRule || !strings.HasSuffix(kind.Text(), "; check_only has no rule; it needs a person until you add one.")) {
			t.Fatalf("check_only %+v %q", kind, kind.Text())
		}
	}
	// settings show keeps the note, also when the next apply finds the part
	// unchanged.
	if view = f.apply(); states(view)["rules_for_running_agent_playbooks"] != "unchanged" || !strings.Contains(view.Text(), "rules_for_running_agent_playbooks: unchanged.\n  "+note+"\n") {
		t.Fatalf("unchanged apply:\n%s", view.Text())
	}
	shown, err := f.a.SettingsFileState(ctx)
	if err != nil || !strings.Contains(shown.Text(), "\n  "+note) {
		t.Fatalf("show: %v\n%s", err, shown.Text())
	}
	// Listing the kind removes the note.
	f.write(replaced(t, withoutCheckOnly, "      unrecognized_task:", "      check_only: {rule: act_then_report}\n      unrecognized_task:"))
	if view = f.apply(); states(view)["rules_for_running_agent_playbooks"] != "applied" || strings.Contains(view.Text(), "has no setting") {
		t.Fatalf("listed: %s", view.Text())
	}
	if shown, err = f.a.SettingsFileState(ctx); err != nil || strings.Contains(shown.Text(), "has no setting") {
		t.Fatalf("show after listing: %v\n%s", err, shown.Text())
	}
}
