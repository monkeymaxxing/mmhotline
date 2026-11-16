package app

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/cloudflareaccess"
	"mmhotline/internal/statuspage"
	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// checkSettings checks each value of the settings file with the rule the
// operation that applies it uses, so a file that passes is refused later
// only by MMHotline's state or an outside test. It needs no network and no
// database.
func checkSettings(s Settings, problems *settingsProblems) {
	if _, err := checkTeamName(s.TeamName); err != nil {
		problems.addError("team_name", err)
	}
	if _, err := ParseWebURL(s.WebURL); err != nil {
		problems.addError("web_url", err)
	}
	if err := CheckListenAddress(s.WebListenAddress); err != nil {
		problems.addError("web_listen_address", err)
	}
	if err := CheckListenAddress(s.AlertIntakeListenAddress); err != nil {
		problems.addError("alert_intake_listen_address", err)
	}
	if access := s.CloudflareAccess; access != nil {
		if err := cloudflareaccess.CheckTeamDomain(access.TeamDomain); err != nil {
			problems.addError("cloudflare_access.team_domain", err)
		}
		if err := cloudflareaccess.CheckAudienceTag(access.AudienceTag); err != nil {
			problems.addError("cloudflare_access.audience_tag", err)
		}
	}
	if s.FleetViewCommand != nil {
		if index, err := CheckFleetViewCommand(s.FleetViewCommand); err != nil {
			path := "fleet_view_command"
			if index >= 0 {
				path = fmt.Sprintf("fleet_view_command[%d]", index)
			}
			problems.addError(path, err)
		}
	}
	if agent := s.TriageAgent; agent != nil {
		checkCodingAgentSettings("triage_agent", agent.SettingsCodingAgent, problems)
		triageAgentKey, err := systemsrepository.CommitSigningKeyFingerprint(agent.CommitSigningPublicKey)
		if err != nil {
			problems.addError("triage_agent.commit_signing_public_key", err)
		}
		for index, key := range agent.TrustedCommitSigningPublicKeys {
			path := fmt.Sprintf("triage_agent.trusted_commit_signing_public_keys[%d]", index)
			fingerprint, err := systemsrepository.CommitSigningKeyFingerprint(key)
			switch {
			case err != nil:
				problems.addError(path, err)
			case fingerprint == triageAgentKey:
				// A trusted key's commits pass whatever they change, so the
				// triage agent's commits would leave its two folders unchecked.
				problems.add(path, "is the triage agent's key, triage_agent.commit_signing_public_key; list only the keys of the team's people and machines, because the triage agent's commits may change only one_time_playbooks/ and triage_agent_notes/")
			}
		}
		checkCommitApprovalSettings(*agent, problems)
		if agent.InternetAccess != "listed_hosts_only" && agent.InternetAccess != "any_site" {
			problems.add("triage_agent.internet_access", "must be listed_hosts_only or any_site")
		}
		var listed []string
		for index, host := range agent.ListedHosts {
			path := fmt.Sprintf("triage_agent.listed_hosts[%d]", index)
			name, err := triageAgentHost(host.Host, host.Port)
			if err != nil {
				problems.addError(path, err)
				continue
			}
			address := net.JoinHostPort(name, strconv.Itoa(host.Port))
			if slices.Contains(listed, address) {
				problems.add(path, address+" is listed twice")
			}
			listed = append(listed, address)
		}
		if hours, err := parseSoftwareReviewIntervalHours(agent.SoftwareReviewIntervalHours); err != nil {
			problems.addError("triage_agent.software_review_interval_hours", err)
		} else {
			problems.note("triage_agent.software_review_interval_hours", softwareReviewIntervalNote(hours))
		}
		if minutes := agent.SOSWaitsForTheAgentMinutes; minutes < 0 || minutes > sosWaitsForTheAgentMaximumMinutes {
			problems.add("triage_agent.sos_waits_for_the_agent_minutes", fmt.Sprintf("must be a whole number of minutes from 0 to %d: 0 wakes the person on call at once, and a stuck or wrong agent must not hold an SOS longer than %d minutes", sosWaitsForTheAgentMaximumMinutes, sosWaitsForTheAgentMaximumMinutes))
		}
		if minutes := agent.WarnWaitsForTheAgentMinutes; minutes < 0 || minutes > warnWaitsForTheAgentMaximumMinutes {
			problems.add("triage_agent.warn_waits_for_the_agent_minutes", fmt.Sprintf("must be a whole number of minutes from 0 to %d: 0 holds no warning, and a stuck or wrong agent must not hold a warning longer than %d minutes", warnWaitsForTheAgentMaximumMinutes, warnWaitsForTheAgentMaximumMinutes))
		}
		if minutes := agent.SOSWaitsForAServerReplacementMinutes; minutes < 0 || minutes > sosWaitsForAServerReplacementMaximumMinutes {
			problems.add("triage_agent.sos_waits_for_a_server_replacement_minutes", fmt.Sprintf("must be a whole number of minutes from 0 to %d: 0 turns the longer wait off, and an MMX run longer than its one-hour limit cannot finish a replacement", sosWaitsForAServerReplacementMaximumMinutes))
		}
	}
	if agent := s.ReviewAgent; agent != nil {
		checkCodingAgentSettings("review_agent", *agent, problems)
		// A review runs while the triage run that asked for it is active, and
		// two copies of one sign-in end each other when either refreshes.
		if triage := s.TriageAgent; triage != nil && agent.CredentialType == "chatgpt_sign_in" && triage.CredentialType == "chatgpt_sign_in" && triageagent.SameChatGPTSignIn(agent.Credential, triage.Credential) {
			problems.add("review_agent.credential", "The review agent needs its own ChatGPT sign-in. Sign in again in a new empty folder and use that auth.json.")
		}
	}
	checkAgentPlaybookRulesSettings(s.AgentPlaybookRules, problems)
	var units []string
	for index, name := range s.ServicesWhoseChangesAPersonMustApprove {
		unit, err := servicesWhoseChangesAPersonMustApproveUnit(name, units)
		if err != nil {
			problems.addError(fmt.Sprintf("services_whose_changes_a_person_must_approve[%d]", index), err)
			continue
		}
		units = append(units, unit)
	}
	checkSendingServicesSettings(s.SendingServices, problems)
	if downAlarm := s.DownAlarm; downAlarm != nil {
		first, backup := downAlarm.WakeThePersonOnCallAfterMinutes, downAlarm.WakeTheBackupAfterMinutes
		if first < downAlarmWakeMinimumMinutes || first > downAlarmWakeMaximumMinutes {
			problems.add("mmhotline_down_alarm.wake_the_person_on_call_after_minutes_without_a_check_in", fmt.Sprintf("must be a whole number of minutes from %d to %d: MMHotline checks in every minute and at once when it starts, so a shorter wait would wake someone for a server reboot, and a longer one would leave a stopped MMHotline unnoticed for over an hour", downAlarmWakeMinimumMinutes, downAlarmWakeMaximumMinutes))
		} else if backup < first+downAlarmBackupWakeMinimumGapMinutes {
			problems.add("mmhotline_down_alarm.wake_the_backup_after_minutes_without_a_check_in", fmt.Sprintf("must be a whole number of minutes, at least %d more than wake_the_person_on_call_after_minutes_without_a_check_in (%d), so the person on call has time to answer before the backup is woken", downAlarmBackupWakeMinimumGapMinutes, first))
		}
		if _, err := checkCloudflareAccountID(downAlarm.CloudflareAccountID); err != nil {
			problems.addError("mmhotline_down_alarm.cloudflare_account_id", err)
		}
		if _, err := checkCloudflareAPIToken(downAlarm.CloudflareAPIToken); err != nil {
			problems.addError("mmhotline_down_alarm.cloudflare_api_token", err)
		}
	}
	if page := s.StatusPage; page != nil {
		if _, err := statuspage.CheckPageTitle(page.PageTitle); err != nil {
			problems.add("status_page.page_title", strings.TrimPrefix(err.Error(), statuspage.ErrRefused.Error()+": "))
		}
		if _, err := checkStatusPageHostName(page.HostName); err != nil {
			problems.addError("status_page.host_name", err)
		}
		if _, err := statuspage.CheckBucketURL(page.R2BucketURL); err != nil {
			problems.addError("status_page.r2_bucket_url", err)
		}
		if !r2AccessKeyIDPattern.MatchString(strings.TrimSpace(page.R2AccessKeyID)) {
			problems.add("status_page.r2_access_key_id", "must be letters and digits, as Cloudflare shows the R2 Access Key ID")
		}
		checkSecretSettings("status_page.r2_secret_access_key", page.R2SecretAccessKey, problems)
	}
	people := checkPeopleSettings(s.People, problems)
	checkOnCallScheduleSettings(s.OnCallSchedule, people, problems)
	checkAlertSourcesSettings(s.AlertSources, problems)
}

// ParseWebURL is the rule for web_url: an HTTPS origin, or
// http://localhost:PORT or http://127.0.0.1:PORT for a team that opens
// MMHotline only through an SSH forward. It returns the URL without a
// trailing slash.
func ParseWebURL(raw string) (string, error) {
	webURL := strings.TrimRight(raw, "/")
	u, err := url.Parse(webURL)
	validPort := func(port string) bool { n, err := strconv.Atoi(port); return err == nil && n > 0 && n <= 65535 }
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") && validPort(u.Port()))) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("must be an HTTPS origin, such as https://mmhotline.example.com, or http://localhost:PORT or http://127.0.0.1:PORT")
	}
	return webURL, nil
}

// CheckListenAddress is the rule for the listen addresses: a host and a
// port.
func CheckListenAddress(address string) error {
	_, port, err := net.SplitHostPort(address)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 0 || portNumber > 65535 {
		return errors.New("must be a host:port address, such as 127.0.0.1:8480")
	}
	return nil
}

// CheckFleetViewCommand is the rule for fleet_view_command: the program and
// its arguments, one word per item, run with no shell, so quotes,
// variables, and pipes mean nothing in them. The program must be an
// absolute path, so a search path never decides what runs as root. index
// is the item that is wrong, or -1 for the whole list.
func CheckFleetViewCommand(words []string) (int, error) {
	if len(words) == 0 {
		return -1, errors.New("must list the program and its arguments, such as [/usr/local/bin/PROGRAM, fleet, --json]")
	}
	for index, word := range words {
		if word == "" || strings.ContainsFunc(word, unicode.IsControl) {
			return index, errors.New("must be one word without control characters")
		}
	}
	if !filepath.IsAbs(words[0]) {
		return 0, errors.New("must be the program's absolute path, such as /usr/local/bin/PROGRAM")
	}
	return -1, nil
}

// checkCodingAgentSettings checks the triage agent's or the review agent's
// coding agent, credential, model, and reasoning effort.
func checkCodingAgentSettings(path string, agent SettingsCodingAgent, problems *settingsProblems) {
	var info *triageagent.CodingAgentInfo
	var names []string
	for _, candidate := range triageagent.CodingAgents() {
		names = append(names, candidate.Name)
		if candidate.Name == agent.CodingAgent {
			info = &candidate
		}
	}
	if info == nil {
		problems.add(path+".coding_agent", "must be "+strings.Join(names, " or "))
	} else {
		var credentialTypes []string
		for _, credentialType := range info.CredentialTypes {
			credentialTypes = append(credentialTypes, credentialType.Name)
		}
		switch {
		case !slices.Contains(credentialTypes, agent.CredentialType):
			problems.add(path+".credential_type", "must be "+strings.Join(credentialTypes, " or ")+" for "+agent.CodingAgent)
		default:
			if err := triageagent.CheckSetup(agent.CodingAgent, agent.CredentialType, agent.Credential); err != nil {
				problems.add(path+".credential", err.Error())
			}
		}
	}
	// MMHotline cannot read a Codex access token's end date from the token,
	// so the person copies it from ChatGPT's dialog; the reminders count to it.
	switch {
	case agent.CredentialType == "access_token" && agent.CredentialExpiresOn == "":
		problems.add(path+".credential_expires_on", "is required with credential_type access_token: the token's end date, such as 2027-02-06, as ChatGPT showed it when you made the token")
	case agent.CredentialType == "access_token":
		if _, err := time.Parse(credentialExpiresOnLayout, agent.CredentialExpiresOn); err != nil {
			problems.add(path+".credential_expires_on", "must be a date such as 2027-02-06")
		}
	case agent.CredentialExpiresOn != "":
		problems.add(path+".credential_expires_on", "is only for credential_type access_token; MMHotline knows when a subscription token ends, and an API key or a ChatGPT sign-in has no end date")
	}
	if err := triageagent.CheckModel(agent.Model); err != nil {
		problems.add(path+".model", err.Error())
	}
	if err := triageagent.CheckReasoningEffort(agent.ReasoningEffort); err != nil {
		problems.add(path+".reasoning_effort", err.Error())
	}
}

// checkAgentPlaybookRulesSettings checks each inventory's preset or its setting
// of every kind of action.
func checkAgentPlaybookRulesSettings(rules map[string]SettingsInventoryAgentPlaybookRules, problems *settingsProblems) {
	var kindNames []string
	for _, kind := range actionkinds.Kinds() {
		kindNames = append(kindNames, string(kind))
	}
	for _, inventory := range slices.Sorted(maps.Keys(rules)) {
		settings, path := rules[inventory], "rules_for_running_agent_playbooks."+inventory
		if err := checkAgentPlaybookRulesInventory(inventory); err != nil {
			problems.addError(path, err)
			continue
		}
		checkListedPlaybooksSettings(path, settings.Playbooks, problems)
		switch {
		case settings.Preset != "" && settings.ActionKinds != nil:
			problems.add(path, "has preset and action_kinds. Specify only preset or only action_kinds")
			continue
		case settings.Preset == "" && settings.ActionKinds == nil:
			problems.add(path, "needs preset or action_kinds")
			continue
		}
		if settings.Preset != "" {
			if _, found := findAgentPlaybookRulesPreset(settings.Preset); !found {
				problems.add(path+".preset", "must be permission_for_every_fix, routine_fixes_without_permission, or every_fix_without_permission")
			}
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(settings.ActionKinds)) {
			if !slices.Contains(kindNames, name) {
				problems.add(path+".action_kinds."+name, "unknown kind of action; the kinds are "+strings.Join(kindNames, ", "))
			}
		}
		for _, name := range kindNames {
			kindPath := path + ".action_kinds." + name
			setting, listed := settings.ActionKinds[name]
			if !listed {
				// A later MMHotline may add a kind of action, and an existing
				// settings file must not stop applying because of it. The kind
				// gets the safest setting, and the note shows it, so it is
				// never silent.
				problems.note(path, unsetActionKindNote(actionkinds.Kind(name)))
				continue
			}
			if err := checkAgentPlaybookRule(setting.agentPlaybookRule()); err != nil {
				problems.addError(kindPath, err)
			}
		}
	}
}

// checkListedPlaybooksSettings checks an inventory's playbooks: each key is
// NAME of an ordinary playbook, playbooks/NAME.yml, and each value a rule,
// with the playbooks its approval also approves in the same session.
func checkListedPlaybooksSettings(path string, playbooks map[string]SettingsListedPlaybookRule, problems *settingsProblems) {
	for _, name := range slices.Sorted(maps.Keys(playbooks)) {
		playbookPath := path + ".playbooks." + name
		if err := checkListedPlaybookName(name); err != nil {
			problems.addError(playbookPath, err)
		}
		setting := playbooks[name]
		if err := checkAgentPlaybookRule(setting.agentPlaybookRule()); err != nil {
			problems.addError(playbookPath, err)
		}
		alsoPath := playbookPath + ".its_approval_also_approves_in_the_same_session"
		if len(setting.ItsApprovalAlsoApprovesInTheSameSession) > 0 && setting.Rule != getPermissionThenAct {
			problems.add(alsoPath, "goes only with rule get_permission_then_act: only a person's approval can also approve later runs, and "+setting.Rule+" asks for none")
			continue
		}
		for index, covered := range setting.ItsApprovalAlsoApprovesInTheSameSession {
			coveredPath := fmt.Sprintf("%s[%d]", alsoPath, index)
			_, listed := playbooks[covered]
			switch {
			case covered == name:
				problems.add(coveredPath, "names "+name+" itself; an approval approves its own playbook already")
			case slices.Contains(setting.ItsApprovalAlsoApprovesInTheSameSession[:index], covered):
				problems.add(coveredPath, covered+" is named twice; each named playbook is approved once")
			case !listed:
				problems.add(coveredPath, covered+" is not listed under "+path+".playbooks; list it there with its own rule, so the approval covers only a playbook a person chose by name")
			}
		}
	}
}

// checkSecretSettings checks a secret: one line, of at most 4,096 bytes. A
// problem never shows the value.
func checkSecretSettings(path, value string, problems *settingsProblems) {
	switch {
	case strings.TrimSpace(value) == "":
		problems.add(path, "required")
	case len(value) > 4096:
		problems.add(path, "must be at most 4,096 bytes")
	case strings.ContainsFunc(value, unicode.IsControl):
		problems.add(path, "must be one line")
	}
}

// oneLineSecret is a secret that checkSecretSettings accepts, so a check
// of its form adds no second problem at the same key.
func oneLineSecret(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsFunc(value, unicode.IsControl)
}

// checkSendingServicesSettings checks each sending service's values; the
// sending service's own check, which calls the service, runs when the part
// applies.
func checkSendingServicesSettings(services SettingsSendingServices, problems *settingsProblems) {
	if pushover := services.Pushover; pushover != nil {
		if !pushoverKeyPattern.MatchString(pushover.ApplicationToken) {
			problems.add("sending_services.pushover.application_token", "must be one line of exactly 30 letters and digits, as Pushover shows the application's API token")
		}
	}
	if twilio := services.Twilio; twilio != nil {
		if !twilioAccountSIDPattern.MatchString(twilio.AccountSID) {
			problems.add("sending_services.twilio.account_sid", "must be AC followed by 32 hexadecimal digits")
		}
		if !twilioAPIKeySIDPattern.MatchString(twilio.APIKeySID) {
			problems.add("sending_services.twilio.api_key_sid", "must be SK followed by 32 hexadecimal digits")
		}
		checkSecretSettings("sending_services.twilio.api_key_secret", twilio.APIKeySecret, problems)
		if !callingNumberPattern.MatchString(twilio.PhoneNumberToCallFrom) {
			problems.add("sending_services.twilio.phone_number_to_call_from", "must be an E.164 phone number, such as +12025550102")
		}
		for index, number := range twilio.ExtraPhoneNumbersOnTheContactCard {
			if !callingNumberPattern.MatchString(number) {
				problems.add(fmt.Sprintf("sending_services.twilio.extra_phone_numbers_on_the_contact_card[%d]", index), "must be an E.164 phone number, such as +12025550102")
			}
		}
	}
	if telegram := services.Telegram; telegram != nil {
		checkSecretSettings("sending_services.telegram.bot_token", telegram.BotToken, problems)
		// fake-telegram-bot-token is development's documented stand-in
		// (design/operation.md); outside development the apply refuses it.
		if oneLineSecret(telegram.BotToken) && !telegramBotTokenPattern.MatchString(telegram.BotToken) && telegram.BotToken != "fake-telegram-bot-token" {
			problems.add("sending_services.telegram.bot_token", "must be the token from BotFather: digits, a colon, and letters, digits, _ and -")
		}
	}
	if slack := services.Slack; slack != nil {
		checkSecretSettings("sending_services.slack.bot_token", slack.BotToken, problems)
		checkSecretSettings("sending_services.slack.app_level_token", slack.AppLevelToken, problems)
		if oneLineSecret(slack.BotToken) && !strings.HasPrefix(slack.BotToken, "xoxb-") {
			problems.add("sending_services.slack.bot_token", "must be the Bot User OAuth Token, which starts with xoxb-")
		}
		if oneLineSecret(slack.AppLevelToken) && !strings.HasPrefix(slack.AppLevelToken, "xapp-") {
			problems.add("sending_services.slack.app_level_token", "must be the app-level token, which starts with xapp-")
		}
	}
	if mail := services.MailServer; mail != nil {
		server := EmailContactMethodType{SMTPServer: mail.SMTPServer, SMTPConnectionSecurity: mail.SMTPConnectionSecurity, SMTPUserName: mail.SMTPUserName, SMTPPassword: mail.SMTPPassword, EmailFromAddress: mail.EmailFromAddress}
		if err := server.validateSetup(); err != nil {
			problems.addError("sending_services.mail_server", err)
		}
	}
}

// checkPeopleSettings checks each person, and returns the sign-in email
// addresses of the people, as MMHotline saves them, for the schedule's
// check.
func checkPeopleSettings(people []SettingsPerson, problems *settingsProblems) []string {
	var addresses []string
	administrators := 0
	for index, person := range people {
		path := fmt.Sprintf("people[%d]", index)
		if _, err := validatePersonName(person.Name); err != nil {
			problems.addError(path+".name", err)
		}
		address, err := normalizeSignInEmailAddress(person.SignInEmailAddress)
		switch {
		case err != nil:
			problems.addError(path+".sign_in_email_address", err)
		case slices.Contains(addresses, address):
			problems.add(path+".sign_in_email_address", fmt.Sprintf("%s is also the sign-in email address of people[%d]; each person must have a unique sign-in email address", address, slices.Index(addresses, address)))
		}
		addresses = append(addresses, address)
		if err := validatePersonRole(person.Role); err != nil {
			problems.addError(path+".role", err)
		}
		if person.Role == RoleAdministrator {
			administrators++
		}
	}
	if administrators == 0 {
		problems.add("people", "needs at least one person with role administrator, who opens the administration pages")
	}
	return addresses
}

// onCallScheduleKeys are the keys each schedule type uses, besides
// schedule_type, escalate_if_unclaimed_after_minutes, time_zone, and backup_option.
var onCallScheduleKeys = map[string][]string{
	"day_of_week":      {"handoff_hour", "handoff_minute", "assignment_sign_in_email_addresses"},
	"hour_of_day":      {"assignment_sign_in_email_addresses"},
	"weekly_rotation":  {"handoff_hour", "handoff_minute", "handoff_weekday", "first_start_date", "rotation_sign_in_email_addresses"},
	customScheduleKind: {"custom_schedule_code", "custom_schedule_test_cases"},
}

// checkOnCallScheduleSettings checks the schedule with the schedules' own
// rules, after checking that it has the keys its type uses and no other,
// and that every person it names is one of people.
func checkOnCallScheduleSettings(schedule SettingsOnCallSchedule, people []string, problems *settingsProblems) {
	const path = "on_call_schedule"
	if schedule.EscalateIfUnclaimedAfterMinutes < 1 || schedule.EscalateIfUnclaimedAfterMinutes > 60 {
		problems.add(path+".escalate_if_unclaimed_after_minutes", "must be a whole number of minutes from 1 to 60")
	}
	used, known := onCallScheduleKeys[schedule.ScheduleType]
	if !known {
		problems.add(path+".schedule_type", "must be day_of_week, hour_of_day, weekly_rotation, or custom_schedule_code")
		return
	}
	present := map[string]bool{
		"handoff_hour": schedule.HandoffHour != nil, "handoff_minute": schedule.HandoffMinute != nil,
		"handoff_weekday": schedule.HandoffWeekday != nil, "first_start_date": schedule.FirstStartDate != nil,
		"rotation_sign_in_email_addresses":   schedule.RotationSignInEmailAddresses != nil,
		"assignment_sign_in_email_addresses": schedule.AssignmentSignInEmailAddresses != nil,
		"custom_schedule_code":               schedule.CustomScheduleCode != nil, "custom_schedule_test_cases": schedule.CustomScheduleTestCases != nil,
	}
	keysRight := true
	for _, key := range slices.Sorted(maps.Keys(present)) {
		switch {
		case slices.Contains(used, key) && !present[key]:
			problems.add(path+"."+key, "required for a "+schedule.ScheduleType+" schedule")
			keysRight = false
		case !slices.Contains(used, key) && present[key]:
			problems.add(path+"."+key, "a "+schedule.ScheduleType+" schedule does not use the setting. Remove the unused setting")
			keysRight = false
		}
	}
	switch fixed := schedule.BackupOption == "fixed_person"; {
	case fixed && schedule.BackupSignInEmailAddress == nil:
		problems.add(path+".backup_sign_in_email_address", "required with backup_option fixed_person")
		keysRight = false
	case !fixed && schedule.BackupSignInEmailAddress != nil:
		problems.add(path+".backup_sign_in_email_address", "used only with backup_option fixed_person. Remove the backup_sign_in_email_address setting")
		keysRight = false
	}
	// The schedule names people by sign-in email address; its own rules
	// then run with the addresses in place of member IDs.
	person := func(keyPath, address string, emptyAllowed bool) (string, error) {
		if address == "" && emptyAllowed {
			return "", nil
		}
		normalized, err := normalizeSignInEmailAddress(address)
		if err == nil && !slices.Contains(people, normalized) {
			err = fmt.Errorf("%s is not the sign-in email address of any person listed in people", normalized)
		}
		if err != nil {
			problems.addError(keyPath, err)
			keysRight = false
		}
		return normalized, nil
	}
	config, _ := schedule.scheduleConfig(person)
	if keysRight {
		if err := checkScheduleConfig(config); err != nil {
			problems.addError(path, err)
		}
	}
}

// alertSourceIDPattern is the rule for an alert source's ID, which the
// sender's URLs carry: the team's own ID, such as monitoring, or the 32
// hexadecimal characters of one MMHotline made.
var alertSourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkAlertSourcesSettings checks each alert source: its ID, name, signing
// secrets, and check-in times.
func checkAlertSourcesSettings(sources []SettingsAlertSource, problems *settingsProblems) {
	var ids []string
	for index, source := range sources {
		path := fmt.Sprintf("alert_sources[%d]", index)
		switch {
		case !alertSourceIDPattern.MatchString(source.AlertSourceID):
			problems.add(path+".alert_source_id", "must be 1 to 64 letters, digits, _ and -, such as monitoring")
		case source.AlertSourceID == systemAlertSourceID || source.AlertSourceID == manualAlertSourceID:
			problems.add(path+".alert_source_id", source.AlertSourceID+" names MMHotline's own alerts; choose another ID")
		case slices.Contains(ids, source.AlertSourceID):
			problems.add(path+".alert_source_id", fmt.Sprintf("%s is also the ID of alert_sources[%d]; each alert source must have a unique ID", source.AlertSourceID, slices.Index(ids, source.AlertSourceID)))
		}
		ids = append(ids, source.AlertSourceID)
		if _, err := checkAlertSourceName(source.Name); err != nil {
			problems.addError(path+".name", err)
		}
		if len(source.SigningSecrets) < 1 || len(source.SigningSecrets) > 2 {
			problems.add(path+".signing_secrets", "must list one signing secret, or two while the sender moves to a new one")
		}
		var secrets []string
		for secretIndex, secret := range source.SigningSecrets {
			secretPath := fmt.Sprintf("%s.signing_secrets[%d]", path, secretIndex)
			decoded, err := hex.DecodeString(secret)
			switch {
			case err != nil || len(decoded) != 32:
				problems.add(secretPath, "must be 64 hexadecimal characters, such as the output of openssl rand -hex 32")
			case slices.Contains(secrets, string(decoded)):
				problems.add(secretPath, fmt.Sprintf("is the same secret as signing_secrets[%d]", slices.Index(secrets, string(decoded))))
			}
			secrets = append(secrets, string(decoded))
		}
		warning, err := alertSourceWarningMinutes(source.WarnAfterNoCheckInForMinutes)
		if err != nil {
			problems.add(path+".warn_after_no_check_in_for_minutes", "must be a whole number of minutes from 1 to 60, or off")
			continue
		}
		if err := checkAlertSourceCheckInTimes(time.Duration(warning)*time.Minute, time.Duration(source.SOSAfterNoCheckInForMinutes)*time.Minute); err != nil {
			problems.addError(path+".sos_after_no_check_in_for_minutes", err)
		}
	}
	if len(sources) > maxAlertSources {
		problems.add("alert_sources", fmt.Sprintf("lists %d alert sources; an installation has at most %d", len(sources), maxAlertSources))
	}
}

// alertSourceWarningMinutes reads warn_after_no_check_in_for_minutes: off is
// 0.
func alertSourceWarningMinutes(value string) (int, error) {
	if value == "off" {
		return 0, nil
	}
	minutes, err := strconv.Atoi(value)
	if err != nil || minutes < 1 || minutes > 60 {
		return 0, errors.New("must be 1 to 60, or off")
	}
	return minutes, nil
}
