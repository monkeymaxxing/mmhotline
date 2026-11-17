package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
)

// The settings file holds every team setting that is not a person's own:
// docs/changing-mmhotline/design/settings-file.md. A deploy tool renders it on the server with
// the secrets from a vault, or a team edits it with sudoedit; MMHotline reads
// it at start and on sudo mmhotline settings apply.

// InstalledSettingsFilePath is where the installed service reads its
// settings; install's units pass it to serve and to the fleet view.
const InstalledSettingsFilePath = "/etc/mmhotline/mmhotline_settings.yml"

// maximumSettingsFileSize bounds what MMHotline reads into memory. The
// largest value, custom schedule code, is at most 64 KiB, so 1 MiB leaves
// room for a large team and its comments.
const maximumSettingsFileSize = 1 << 20

// Settings is the whole settings file. Struct tags say each key's name and
// whether it may be left out (settings:"optional"); every other key of a
// section that is present is required. settings:"secret" marks a secret,
// whose value no problem, page, log, or output ever shows.
type Settings struct {
	TeamName                               string                                         `yaml:"team_name"`
	WebURL                                 string                                         `yaml:"web_url"`
	WebListenAddress                       string                                         `yaml:"web_listen_address"`
	AlertIntakeListenAddress               string                                         `yaml:"alert_intake_listen_address"`
	CloudflareAccess                       *SettingsCloudflareAccess                      `yaml:"cloudflare_access" settings:"optional"`
	FleetViewCommand                       []string                                       `yaml:"fleet_view_command" settings:"optional"`
	TriageAgent                            *SettingsTriageAgent                           `yaml:"triage_agent" settings:"optional"`
	ReviewAgent                            *SettingsCodingAgent                           `yaml:"review_agent" settings:"optional"`
	AgentPlaybookRules                     map[string]SettingsInventoryAgentPlaybookRules `yaml:"rules_for_running_agent_playbooks"`
	ServicesWhoseChangesAPersonMustApprove []string                                       `yaml:"services_whose_changes_a_person_must_approve"`
	SendingServices                        SettingsSendingServices                        `yaml:"sending_services"`
	DownAlarm                              *SettingsDownAlarm                             `yaml:"mmhotline_down_alarm" settings:"optional"`
	StatusPage                             *SettingsStatusPage                            `yaml:"status_page" settings:"optional"`
	People                                 []SettingsPerson                               `yaml:"people"`
	OnCallSchedule                         SettingsOnCallSchedule                         `yaml:"on_call_schedule"`
	AlertSources                           []SettingsAlertSource                          `yaml:"alert_sources"`
}

// SettingsCloudflareAccess is the Cloudflare Access application that signs
// people in.
type SettingsCloudflareAccess struct {
	TeamDomain  string `yaml:"team_domain" json:"team_domain"`
	AudienceTag string `yaml:"audience_tag" json:"audience_tag"`
}

// SettingsCodingAgent is the review agent's section, and the start of the
// triage agent's.
type SettingsCodingAgent struct {
	CodingAgent    string `yaml:"coding_agent"`
	CredentialType string `yaml:"credential_type"`
	Credential     string `yaml:"credential" settings:"secret"`
	// CredentialExpiresOn is a Codex access token's end date, required with
	// that credential type and refused with the others.
	CredentialExpiresOn string `yaml:"credential_expires_on" settings:"optional"`
	Model               string `yaml:"model"`
	ReasoningEffort     string `yaml:"reasoning_effort"`
}

// SettingsTriageAgent is the triage agent's section.
type SettingsTriageAgent struct {
	SettingsCodingAgent `yaml:",inline"`
	// TrustedCommits is signed_by_a_trusted_key_or_reviewed or
	// every_commit_on_origin; left out it is the first, so settings files
	// written before the key keep working. With every_commit_on_origin the
	// four keys that follow it are left out: no commit is checked.
	TrustedCommits         string `yaml:"trusted_commits" settings:"optional"`
	CommitSigningPublicKey string `yaml:"commit_signing_public_key" settings:"optional"`
	// TrustedCommitSigningPublicKeys are the keys of the team's people and
	// machines whose commits pass the check before MMX and triage agent runs.
	// A nil list is a key left out; [] is an empty list.
	TrustedCommitSigningPublicKeys []string `yaml:"trusted_commit_signing_public_keys" settings:"optional"`
	// CommitsWithoutATrustedSignatureAreApprovedBy is a_person, or
	// a_person_or_the_triage_agent: the triage agent then reviews each commit
	// no trusted key signed, except those that change a path of
	// PathsOnlyAPersonMayApprove, and approves an ordinary change.
	CommitsWithoutATrustedSignatureAreApprovedBy string `yaml:"commits_without_a_trusted_signature_are_approved_by" settings:"optional"`
	// PathsOnlyAPersonMayApprove are repository paths, an entry ending in "/"
	// a directory, such as the files that set the triage agent's own powers
	// and the trusted keys.
	PathsOnlyAPersonMayApprove []string             `yaml:"paths_only_a_person_may_approve" settings:"optional"`
	InternetAccess             string               `yaml:"internet_access"`
	ListedHosts                []SettingsListedHost `yaml:"listed_hosts"`
	// SoftwareReviewIntervalHours is off, or 1 to 8760 hours between
	// scheduled software reviews.
	SoftwareReviewIntervalHours string `yaml:"software_review_interval_hours"`
	SOSWaitsForTheAgentMinutes  int    `yaml:"sos_waits_for_the_agent_minutes"`
	// WarnWaitsForTheAgentMinutes is how long a warn alert that starts a
	// triage session is held for the agent, 0 to 15; it may be left out,
	// which is 0: nothing is held.
	WarnWaitsForTheAgentMinutes int `yaml:"warn_waits_for_the_agent_minutes" settings:"optional"`
	// SOSWaitsForAServerReplacementMinutes is how long an SOS waits after the
	// triage agent starts replacing a server, 0 to 60; 0 turns it off.
	SOSWaitsForAServerReplacementMinutes int `yaml:"sos_waits_for_a_server_replacement_minutes"`
}

// SettingsListedHost is one extra site the triage agent may reach.
type SettingsListedHost struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// SettingsInventoryAgentPlaybookRules are one inventory's rules for running the agent's playbooks: a
// preset, or the setting of every kind of action, and the rules of
// ordinary playbooks listed by name, NAME of playbooks/NAME.yml.
type SettingsInventoryAgentPlaybookRules struct {
	Preset      string                                `yaml:"preset" settings:"optional"`
	ActionKinds map[string]SettingsAgentPlaybookRule  `yaml:"action_kinds" settings:"optional"`
	Playbooks   map[string]SettingsListedPlaybookRule `yaml:"playbooks" settings:"optional"`
}

// SettingsListedPlaybookRule is one listed playbook's rule, and the other
// listed playbooks a person's approval of it also approves once in the same
// triage session: docs/changing-mmhotline/design/triage-agent.md#one-approval-for-a-known-sequence.
type SettingsListedPlaybookRule struct {
	SettingsAgentPlaybookRule               `yaml:",inline"`
	ItsApprovalAlsoApprovesInTheSameSession []string `yaml:"its_approval_also_approves_in_the_same_session" settings:"optional"`
	// StartsAServerReplacement marks the playbook that starts a server
	// replacement: docs/changing-mmhotline/design/triage-agent.md#an-sos-waits-for-a-server-replacement.
	StartsAServerReplacement bool `yaml:"starts_a_server_replacement" settings:"optional"`
}

// SettingsAgentPlaybookRule is one kind of action's or one listed
// playbook's rule.
type SettingsAgentPlaybookRule struct {
	Rule               string `yaml:"rule"`
	WarningWaitMinutes int    `yaml:"warning_wait_minutes" settings:"optional"`
	ApprovalsNeeded    int    `yaml:"approvals_needed" settings:"optional"`
}

func (setting SettingsAgentPlaybookRule) agentPlaybookRule() AgentPlaybookRule {
	return AgentPlaybookRule(setting)
}

// SettingsSendingServices are the sending services; one left out is not set
// up.
type SettingsSendingServices struct {
	Pushover   *SettingsPushover   `yaml:"pushover" settings:"optional"`
	Twilio     *SettingsTwilio     `yaml:"twilio" settings:"optional"`
	Telegram   *SettingsTelegram   `yaml:"telegram" settings:"optional"`
	Slack      *SettingsSlack      `yaml:"slack" settings:"optional"`
	MailServer *SettingsMailServer `yaml:"mail_server" settings:"optional"`
}

type SettingsPushover struct {
	ApplicationToken string `yaml:"application_token" settings:"secret"`
}

type SettingsTwilio struct {
	AccountSID                        string   `yaml:"account_sid"`
	APIKeySID                         string   `yaml:"api_key_sid"`
	APIKeySecret                      string   `yaml:"api_key_secret" settings:"secret"`
	PhoneNumberToCallFrom             string   `yaml:"phone_number_to_call_from"`
	ExtraPhoneNumbersOnTheContactCard []string `yaml:"extra_phone_numbers_on_the_contact_card"`
}

type SettingsTelegram struct {
	BotToken string `yaml:"bot_token" settings:"secret"`
}

type SettingsSlack struct {
	BotToken      string `yaml:"bot_token" settings:"secret"`
	AppLevelToken string `yaml:"app_level_token" settings:"secret"`
}

type SettingsMailServer struct {
	SMTPServer             string `yaml:"smtp_server"`
	SMTPConnectionSecurity string `yaml:"smtp_connection_security"`
	SMTPUserName           string `yaml:"smtp_user_name" settings:"optional"`
	SMTPPassword           string `yaml:"smtp_password" settings:"optional,secret"`
	EmailFromAddress       string `yaml:"email_from_address"`
}

type SettingsDownAlarm struct {
	CloudflareAccountID string `yaml:"cloudflare_account_id"`
	CloudflareAPIToken  string `yaml:"cloudflare_api_token" settings:"secret"`
	DownAlarmWakeTimes  `yaml:",inline"`
}

// DownAlarmWakeTimes are how long after MMHotline's last accepted check-in
// the MMHotline-down alarm wakes the person on call, and the backup.
type DownAlarmWakeTimes struct {
	WakeThePersonOnCallAfterMinutes int `yaml:"wake_the_person_on_call_after_minutes_without_a_check_in" json:"wake_the_person_on_call_after_minutes_without_a_check_in"`
	WakeTheBackupAfterMinutes       int `yaml:"wake_the_backup_after_minutes_without_a_check_in" json:"wake_the_backup_after_minutes_without_a_check_in"`
}

type SettingsStatusPage struct {
	PageTitle         string `yaml:"page_title"`
	HostName          string `yaml:"host_name"`
	R2BucketURL       string `yaml:"r2_bucket_url"`
	R2AccessKeyID     string `yaml:"r2_access_key_id" settings:"secret"`
	R2SecretAccessKey string `yaml:"r2_secret_access_key" settings:"secret"`
}

// SettingsPerson is one person on the team, identified by the sign-in email
// address.
type SettingsPerson struct {
	Name               string `yaml:"name"`
	SignInEmailAddress string `yaml:"sign_in_email_address"`
	Role               string `yaml:"role"`
}

// SettingsOnCallSchedule is the on-call schedule: the keys of the schedule
// file of the schedules commands, with only those its type uses, and the
// time to claim. The keys a type may not use are pointers, so the check
// tells a key left out from one set to zero.
type SettingsOnCallSchedule struct {
	EscalateIfUnclaimedAfterMinutes int                         `yaml:"escalate_if_unclaimed_after_minutes" json:"escalate_if_unclaimed_after_minutes"`
	ScheduleType                    string                      `yaml:"schedule_type" json:"schedule_type"`
	TimeZone                        string                      `yaml:"time_zone" json:"time_zone"`
	HandoffHour                     *int                        `yaml:"handoff_hour" settings:"optional" json:"handoff_hour,omitempty"`
	HandoffMinute                   *int                        `yaml:"handoff_minute" settings:"optional" json:"handoff_minute,omitempty"`
	HandoffWeekday                  *int                        `yaml:"handoff_weekday" settings:"optional" json:"handoff_weekday,omitempty"`
	FirstStartDate                  *string                     `yaml:"first_start_date" settings:"optional" json:"first_start_date,omitempty"`
	RotationSignInEmailAddresses    *[]string                   `yaml:"rotation_sign_in_email_addresses" settings:"optional" json:"rotation_sign_in_email_addresses,omitempty"`
	AssignmentSignInEmailAddresses  *[]string                   `yaml:"assignment_sign_in_email_addresses" settings:"optional" json:"assignment_sign_in_email_addresses,omitempty"`
	BackupOption                    string                      `yaml:"backup_option" json:"backup_option"`
	BackupSignInEmailAddress        *string                     `yaml:"backup_sign_in_email_address" settings:"optional" json:"backup_sign_in_email_address,omitempty"`
	CustomScheduleCode              *string                     `yaml:"custom_schedule_code" settings:"optional" json:"custom_schedule_code,omitempty"`
	CustomScheduleTestCases         *[]SettingsScheduleTestCase `yaml:"custom_schedule_test_cases" settings:"optional" json:"custom_schedule_test_cases,omitempty"`
}

// SettingsScheduleTestCase is one test case of custom schedule code; without
// an expected backup, the backup is not checked.
type SettingsScheduleTestCase struct {
	LocalTime                         string `yaml:"local_time" json:"local_time"`
	ExpectedPrimarySignInEmailAddress string `yaml:"expected_primary_sign_in_email_address" json:"expected_primary_sign_in_email_address"`
	ExpectedBackupSignInEmailAddress  string `yaml:"expected_backup_sign_in_email_address" settings:"optional" json:"expected_backup_sign_in_email_address"`
}

// SettingsAlertSource is one alert source, such as the monitoring tool.
type SettingsAlertSource struct {
	AlertSourceID string `yaml:"alert_source_id"`
	Name          string `yaml:"name"`
	// SigningSecrets are 64 hexadecimal characters each: one, or two while
	// the sender moves to a new one.
	SigningSecrets []string `yaml:"signing_secrets" settings:"secret"`
	// WarnAfterNoCheckInForMinutes is 1 to 60, or off.
	WarnAfterNoCheckInForMinutes string `yaml:"warn_after_no_check_in_for_minutes"`
	SOSAfterNoCheckInForMinutes  int    `yaml:"sos_after_no_check_in_for_minutes"`
}

// SettingsProblem is one problem in the settings file: the key path and what
// is wrong, such as "triage_agent.reasoning_effort: required". A problem
// never shows a secret's value.
type SettingsProblem struct {
	// KeyPath is empty for a problem with the whole file, such as its owner
	// and mode or YAML that does not parse.
	KeyPath string `json:"key_path"`
	Problem string `json:"problem"`
}

func (problem SettingsProblem) String() string {
	if problem.KeyPath == "" {
		return problem.Problem
	}
	return problem.KeyPath + ": " + problem.Problem
}

// SettingsNote is something the settings file should know that does not
// stop it from applying, such as "rules_for_running_agent_playbooks.production: check_only has
// no setting; it needs a person until you add one". Like a problem, a note
// names its key path and never shows a secret's value.
type SettingsNote struct {
	KeyPath string `json:"key_path"`
	Note    string `json:"note"`
}

func (note SettingsNote) String() string { return note.KeyPath + ": " + note.Note }

// SettingsFile is a settings file that MMHotline read.
type SettingsFile struct {
	Path string
	// FirstCommentLine is the first line without its "#", when the first
	// line is a comment, such as "Managed by MMX from mmhotline_settings.yml
	// in the systems repository"; the pages and every show command print it
	// next to the path.
	FirstCommentLine string
	Settings         Settings
	// Notes are what the check found that does not stop the file from
	// applying; settings check prints them, and apply attaches each to its
	// part.
	Notes []SettingsNote
}

// ReadSettingsFile reads and checks the settings file. With
// allowedOwnerUIDs, it first checks the file's owner and mode, as apply,
// start, and install do; without, it skips that check, as settings check
// does (CA2): a laptop that renders the file cannot give it root as owner.
// Problems mean the file must not be used.
func ReadSettingsFile(path string, allowedOwnerUIDs []uint32) (SettingsFile, []SettingsProblem) {
	file := SettingsFile{Path: path}
	if allowedOwnerUIDs != nil {
		if err := CheckSettingsFileOwnerAndMode(path, allowedOwnerUIDs); err != nil {
			return file, []SettingsProblem{{Problem: err.Error()}}
		}
	}
	opened, err := os.Open(path)
	if err != nil {
		return file, []SettingsProblem{{Problem: "MMHotline cannot read the settings file: " + err.Error()}}
	}
	defer opened.Close()
	raw, err := io.ReadAll(io.LimitReader(opened, maximumSettingsFileSize+1))
	if err != nil {
		return file, []SettingsProblem{{Problem: "MMHotline cannot read the settings file: " + err.Error()}}
	}
	if len(raw) > maximumSettingsFileSize {
		return file, []SettingsProblem{{Problem: "the settings file exceeds MMHotline's limit of 1 MiB"}}
	}
	file.FirstCommentLine = settingsFileFirstCommentLine(raw)
	var problems []SettingsProblem
	file.Settings, problems, file.Notes = parseSettings(raw)
	return file, problems
}

// settingsFileFirstCommentLine is the first line without its "#" and
// spaces, when it is a comment.
func settingsFileFirstCommentLine(raw []byte) string {
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	text, isComment := strings.CutPrefix(strings.TrimRight(string(line), "\r"), "#")
	if !isComment {
		return ""
	}
	return strings.TrimSpace(text)
}

// parseSettings reads the YAML and checks it: first its shape against the
// Go types, which names each unknown key, missing key, and value of the
// wrong kind by its key path, then each value.
func parseSettings(raw []byte) (Settings, []SettingsProblem, []SettingsNote) {
	var settings Settings
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		// yaml.v3's syntax errors name a line and never quote a value.
		return settings, []SettingsProblem{{Problem: "the settings file is not valid YAML: " + strings.TrimPrefix(err.Error(), "yaml: ")}}, nil
	}
	if len(document.Content) == 0 {
		return settings, []SettingsProblem{{Problem: "the settings file is empty"}}, nil
	}
	problems := &settingsProblems{}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return settings, []SettingsProblem{{Problem: "the settings file must be a section of keys, each on its own line as key: value, such as team_name: Example Operations"}}, nil
	}
	checkSettingsShape(root, reflect.TypeFor[Settings](), "", problems)
	// Decode what has the right shape, so the values are checked too; a
	// key with the wrong shape already has its problem.
	var typeError *yaml.TypeError
	if err := root.Decode(&settings); err != nil && !errors.As(err, &typeError) {
		problems.add("", "MMHotline cannot read the settings file: "+err.Error())
	}
	checkSettings(settings, problems)
	return settings, problems.list, problems.notes
}

// settingsProblems collects problems in the order the file has them. A key
// path gets one problem: a value check never repeats a key whose shape is
// already wrong. It collects the notes too.
type settingsProblems struct {
	list  []SettingsProblem
	notes []SettingsNote
}

func (problems *settingsProblems) note(keyPath, note string) {
	problems.notes = append(problems.notes, SettingsNote{KeyPath: keyPath, Note: note})
}

func (problems *settingsProblems) add(keyPath, problem string) {
	for _, existing := range problems.list {
		if existing.KeyPath == keyPath || (existing.KeyPath != "" && (strings.HasPrefix(keyPath, existing.KeyPath+".") || strings.HasPrefix(keyPath, existing.KeyPath+"["))) {
			return
		}
	}
	problems.list = append(problems.list, SettingsProblem{KeyPath: keyPath, Problem: problem})
}

// addError adds an operation's own refusal, without the kind of error it
// carries for HTTP.
func (problems *settingsProblems) addError(keyPath string, err error) {
	problems.add(keyPath, refusalText(err))
}

// refusalText is an operation's refusal without the kind of error it
// carries for HTTP, such as "invalid input: ".
func refusalText(err error) string {
	message := err.Error()
	for _, kind := range []error{ErrInvalid, ErrConflict, ErrNotFound, ErrForbidden, ErrRateLimited} {
		message = strings.TrimPrefix(message, kind.Error()+": ")
	}
	return message
}

// settingsField is one key of a section.
type settingsField struct {
	name     string
	typ      reflect.Type
	optional bool
}

// settingsFields lists a section's keys in their order, with the keys of an
// inline section in place.
func settingsFields(t reflect.Type) []settingsField {
	var fields []settingsField
	for index := range t.NumField() {
		field := t.Field(index)
		name, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if options == "inline" {
			fields = append(fields, settingsFields(field.Type)...)
			continue
		}
		fields = append(fields, settingsField{name: name, typ: field.Type, optional: slices.Contains(strings.Split(field.Tag.Get("settings"), ","), "optional")})
	}
	return fields
}

// renamedSettingsKeys names the new key of each renamed settings key, so a
// settings file written with the old name says what to write instead.
var renamedSettingsKeys = map[string]string{
	"trust_settings":                     "rules_for_running_agent_playbooks",
	"trust_setting":                      "rule",
	"response_window_minutes":            "escalate_if_unclaimed_after_minutes",
	"services_that_always_need_a_person": "services_whose_changes_a_person_must_approve",
	"lifeline":                           "mmhotline_down_alarm",
}

func joinKeyPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// checkSettingsShape checks one YAML node against its Go type: a section is
// a mapping with only its own keys and every required key, a list is a list,
// text is a value on its own, and a number is a whole number.
func checkSettingsShape(node *yaml.Node, t reflect.Type, path string, problems *settingsProblems) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			problems.add(path, "must be a section of keys, each on its own line as key: value")
			return
		}
		removeRepeatedKeys(node, path, problems)
		fields := settingsFields(t)
		seen := map[string]bool{}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index].Value, node.Content[index+1]
			keyPath := joinKeyPath(path, key)
			fieldIndex := slices.IndexFunc(fields, func(field settingsField) bool { return field.name == key })
			switch {
			case fieldIndex < 0 && renamedSettingsKeys[key] != "":
				problems.add(keyPath, "unknown key; it was renamed to "+renamedSettingsKeys[key])
			case fieldIndex < 0:
				problems.add(keyPath, "unknown key")
			case value.Tag == "!!null":
				problems.add(keyPath, "has no value")
			default:
				checkSettingsShape(value, fields[fieldIndex].typ, keyPath, problems) //nolint:nilaway // fieldIndex is 0 or more here, the case above handles a negative one.
			}
			seen[key] = true
		}
		for _, field := range fields {
			if !field.optional && !seen[field.name] {
				problems.add(joinKeyPath(path, field.name), "required")
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			problems.add(path, "must be a section of keys, each on its own line as key: value")
			return
		}
		removeRepeatedKeys(node, path, problems)
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index].Value, node.Content[index+1]
			keyPath := joinKeyPath(path, key)
			if value.Tag == "!!null" {
				problems.add(keyPath, "has no value")
				continue
			}
			checkSettingsShape(value, t.Elem(), keyPath, problems)
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			problems.add(path, "must be a list, such as [a, b], or [] for none")
			return
		}
		for index, item := range node.Content {
			itemPath := fmt.Sprintf("%s[%d]", path, index)
			if item.Tag == "!!null" {
				problems.add(itemPath, "has no value")
				continue
			}
			checkSettingsShape(item, t.Elem(), itemPath, problems)
		}
	case reflect.String:
		if node.Kind != yaml.ScalarNode {
			problems.add(path, "must be a single value, not a list or a section")
		}
	case reflect.Int:
		if _, err := strconv.Atoi(node.Value); node.Kind != yaml.ScalarNode || node.Tag != "!!int" || err != nil {
			problems.add(path, "must be a whole number")
		}
	case reflect.Bool:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
			problems.add(path, "must be true or false")
		}
	default:
		panic("settings type without a shape check: " + t.String())
	}
}

// removeRepeatedKeys reports each key that appears a second time in a
// mapping and removes it, so the rest of the file still decodes and its
// values are checked; YAML refuses to decode a mapping with a repeated key.
func removeRepeatedKeys(node *yaml.Node, path string, problems *settingsProblems) {
	seen := map[string]bool{}
	var kept []*yaml.Node
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index].Value
		if seen[key] {
			problems.add(joinKeyPath(path, key), "appears twice")
			continue
		}
		seen[key] = true
		kept = append(kept, node.Content[index], node.Content[index+1])
	}
	node.Content = kept
}

// CheckSettingsFileOwnerAndMode refuses a settings file that someone other
// than root could change, or that every user could read (CA1): the file
// decides what MMHotline does, including the fleet view command that runs
// as root, and holds the team's secrets. MMHotline's own user reads it
// through the file's group. allowedOwnerUIDs is root alone on a server; a
// development machine also allows its own user, as the command socket does.
func CheckSettingsFileOwnerAndMode(path string, allowedOwnerUIDs []uint32) error {
	fix := "Fix the settings file owner and mode with: sudo chown root:mmhotline " + path + "; sudo chmod 0640 " + path
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("MMHotline cannot read the settings file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a symbolic link or a directory, so MMHotline reads the file whose owner and mode MMHotline checked. Move the existing path aside and create a regular settings file at the original path", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("MMHotline cannot read the owner of %s", path)
	}
	switch mode := info.Mode().Perm(); {
	case !slices.Contains(allowedOwnerUIDs, stat.Uid):
		return fmt.Errorf("%s is owned by user ID %d, not root. Only root may change it, because it decides what MMHotline does, and the fleet view command in it runs as root. %s", path, stat.Uid, fix)
	case mode&0o022 != 0:
		return fmt.Errorf("%s can be changed by users other than root (mode %04o). Only root may change it, because it decides what MMHotline does, and the fleet view command in it runs as root. %s", path, mode, fix)
	case mode&0o004 != 0:
		return fmt.Errorf("%s can be read by every user on the server (mode %04o), and it holds the team's secrets. %s", path, mode, fix)
	}
	return nil
}
