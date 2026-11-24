package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"mmhotline/internal/actionkinds"
)

// settingsFilePart is one part of the settings file: keys that are checked,
// tested, and written together, independently of the other parts, so one
// part that fails keeps its earlier setting and the others still apply.
//
// A part plugs in as one entry of settingsFileParts. value is the part's
// value, which change detection fingerprints: a part whose value is the one
// in effect is not tested or written again. apply checks the value against
// MMHotline's state, runs the part's outside test when it has one, and
// writes it through the operations the pages and commands use; its error is
// the reason the part failed, in words for the person, never with a
// secret's value.
type settingsFilePart struct {
	// name is the part's key paths, as the output names the part.
	name  string
	value func(ctx context.Context, a *App, s Settings) (any, error)
	apply func(ctx context.Context, a *App, s Settings) error
	// hasOutsideTest marks a part whose apply calls an outside service; at
	// start it applies in the background, so serve answers sooner.
	hasOutsideTest bool
	// keepsValues keeps the applied value in settings_file_parts, for a
	// start with a broken file (CA7). Only a part without secrets keeps it.
	keepsValues bool
	// failedButApplied says what applied when the part fails, for a part
	// that writes the rest of its value whatever fails, in place of "the
	// earlier setting stays".
	failedButApplied string
	// nextStep is shown with the part when it applies for the first time,
	// or again after its section was left out, such as connecting a
	// contact method to a sending service newly set up.
	nextStep string
	// shownNotes are lines settings show prints under the part from
	// MMHotline's own state, such as the hosts it lists by itself. They are
	// never part of the value, so they never change its fingerprint.
	shownNotes func(ctx context.Context, tx *sql.Tx) ([]string, error)
}

// fileValue is a part whose value is its keys in the file alone.
func fileValue(value func(Settings) any) func(context.Context, *App, Settings) (any, error) {
	return func(_ context.Context, _ *App, s Settings) (any, error) { return value(s), nil }
}

// settingsFileParts are the parts in the order they apply: people before
// the schedule that names them, and the sending services before the
// MMHotline-down alarm (settingsPartsWithAnOutsideTest).
var settingsFileParts = append([]settingsFilePart{
	settingsServerPart,
	{name: "team_name", value: fileValue(func(s Settings) any { return s.TeamName }), apply: applyTeamNameSettings},
	settingsFleetViewCommandPart,
	{name: "people", value: fileValue(func(s Settings) any { return s.People }), apply: applyPeopleSettings},
	{name: "on_call_schedule", value: onCallScheduleSettingsValue, apply: applyOnCallScheduleSettings},
	{name: "rules_for_running_agent_playbooks", value: agentPlaybookRulesSettingsValue, apply: applyAgentPlaybookRulesSettings, failedButApplied: "The rules for running the agent's playbooks applied for the inventories listed in the settings file. Any inventory without rules requires a person to approve every fix until you add its rules."},
	{name: "services_whose_changes_a_person_must_approve", value: fileValue(func(s Settings) any { return s.ServicesWhoseChangesAPersonMustApprove }), apply: func(ctx context.Context, a *App, s Settings) error {
		return a.SetServicesWhoseChangesAPersonMustApprove(ctx, a.settingsPrincipal(), s.ServicesWhoseChangesAPersonMustApprove)
	}},
	{name: "alert_sources", value: fileValue(func(s Settings) any { return s.AlertSources }), apply: func(ctx context.Context, a *App, s Settings) error {
		return a.applyAlertSourceSettings(ctx, s.AlertSources)
	}},
	settingsTrustedCommitsPart,
	settingsCommitSigningPublicKeysPart,
	settingsCommitApprovalPart,
	settingsTriageAgentSetupPart,
	// The triage agent's own settings apply when its section is present;
	// without it there is no triage agent, and they have no effect.
	{name: "triage_agent.internet_access", value: triageAgentValue(func(agent SettingsTriageAgent) any { return agent.InternetAccess }), apply: func(ctx context.Context, a *App, s Settings) error {
		if s.TriageAgent == nil {
			return nil
		}
		return a.SetTriageAgentInternetAccess(ctx, a.settingsPrincipal(), s.TriageAgent.InternetAccess)
	}},
	{name: "triage_agent.listed_hosts", value: triageAgentValue(func(agent SettingsTriageAgent) any { return agent.ListedHosts }), apply: func(ctx context.Context, a *App, s Settings) error {
		if s.TriageAgent == nil {
			return nil
		}
		return a.SetTriageAgentListedHosts(ctx, a.settingsPrincipal(), s.TriageAgent.ListedHosts)
	}, shownNotes: vendorPageHostsSettingsNotes},
	settingsSOSWaitsForTheAgentPart,
	settingsWarnWaitsForTheAgentPart,
	settingsSendAMessageWhenTheAgentStartsLookingPart,
	settingsMostRunsIn24HoursPart,
	settingsSOSWaitsForAServerReplacementPart,
	{name: "triage_agent.software_review_interval_hours", value: triageAgentValue(func(agent SettingsTriageAgent) any { return agent.SoftwareReviewIntervalHours }), apply: func(ctx context.Context, a *App, s Settings) error {
		if s.TriageAgent == nil {
			return nil
		}
		hours, err := parseSoftwareReviewIntervalHours(s.TriageAgent.SoftwareReviewIntervalHours)
		if err != nil {
			return fmt.Errorf("%w: software_review_interval_hours %s", ErrInvalid, err)
		}
		return a.SetSoftwareReviewInterval(ctx, a.settingsPrincipal(), hours)
	}},
	settingsDownAlarmWakeTimesPart,
}, settingsPartsWithAnOutsideTest...)

// settingsServerPart is the keys serve listens and signs in with. They
// apply when serve starts with them: settings apply restarts MMHotline
// first when they changed, so applying writes nothing but the record, unless
// serve could not listen with them and runs with the earlier ones.
var settingsServerPart = settingsFilePart{
	name:  "web_url, web_listen_address, alert_intake_listen_address, cloudflare_access",
	value: fileValue(func(s Settings) any { return s.server() }),
	apply: func(_ context.Context, a *App, s Settings) error {
		a.settingsFile.mu.RLock()
		running, failure := a.settingsFile.server, a.settingsFile.serverFailure
		a.settingsFile.mu.RUnlock()
		if keys := restartKeysThatDiffer(running, s.server()); len(keys) > 0 {
			if failure == "" {
				failure = "MMHotline runs with the earlier " + strings.Join(keys, ", ") + "; they change only when it restarts"
			}
			return fmt.Errorf("%w: %s", errSettingsPartRefused, failure)
		}
		return nil
	},
	keepsValues: true,
}

// settingsFleetViewCommandPart is the fleet view command. The root fleet
// view unit reads it from the file each time the dashboard asks; MMHotline
// keeps it to know whether there is one.
var settingsFleetViewCommandPart = settingsFilePart{
	name:  "fleet_view_command",
	value: fileValue(func(s Settings) any { return s.FleetViewCommand }),
	apply: func(_ context.Context, a *App, s Settings) error {
		a.settingsFile.mu.Lock()
		a.settingsFile.fleetViewCommand = s.FleetViewCommand
		a.settingsFile.mu.Unlock()
		return nil
	},
	keepsValues: true,
}

// settingsDownAlarmWakeTimesPart is the MMHotline-down alarm's wake times. The MMHotline-down alarm
// reads the applied value at each round, which tells the Worker the new
// times within seconds; it applies before the MMHotline-down alarm's set-up, which
// checks in at once.
var settingsDownAlarmWakeTimesPart = settingsFilePart{
	name: "mmhotline_down_alarm.wake_the_person_on_call_after_minutes_without_a_check_in, mmhotline_down_alarm.wake_the_backup_after_minutes_without_a_check_in",
	value: fileValue(func(s Settings) any {
		if s.DownAlarm == nil {
			return nil
		}
		return s.DownAlarm.DownAlarmWakeTimes
	}),
	apply:       func(context.Context, *App, Settings) error { return nil },
	keepsValues: true,
}

// settingsSOSWaitsForTheAgentPart is how long an SOS waits for the triage
// agent. An SOS reads the applied value as it opens its incident.
var settingsSOSWaitsForTheAgentPart = settingsFilePart{
	name:        "triage_agent.sos_waits_for_the_agent_minutes",
	value:       triageAgentValue(func(agent SettingsTriageAgent) any { return agent.SOSWaitsForTheAgentMinutes }),
	apply:       func(context.Context, *App, Settings) error { return nil },
	keepsValues: true,
}

// settingsWarnWaitsForTheAgentPart is how long a warn alert from an alert
// source is held for the triage agent. A warn reads the applied value as it
// arrives; a value left out is kept as null, which is the default.
var settingsWarnWaitsForTheAgentPart = settingsFilePart{
	name:        "triage_agent.warn_waits_for_the_agent_minutes",
	value:       triageAgentValue(func(agent SettingsTriageAgent) any { return agent.WarnWaitsForTheAgentMinutes }),
	apply:       func(context.Context, *App, Settings) error { return nil },
	keepsValues: true,
}

// settingsSendAMessageWhenTheAgentStartsLookingPart says whether a new
// triage session sends the person on call the quiet start message. A
// session reads the applied value as it starts.
var settingsSendAMessageWhenTheAgentStartsLookingPart = settingsFilePart{
	name:        "triage_agent.send_a_message_when_the_agent_starts_looking",
	value:       triageAgentValue(func(agent SettingsTriageAgent) any { return agent.SendAMessageWhenTheAgentStartsLooking }),
	apply:       func(context.Context, *App, Settings) error { return nil },
	keepsValues: true,
}

// settingsSOSWaitsForAServerReplacementPart is how long an SOS waits after
// the triage agent starts replacing a server. A replacement reads the
// applied value as it starts.
var settingsSOSWaitsForAServerReplacementPart = settingsFilePart{
	name:        "triage_agent.sos_waits_for_a_server_replacement_minutes",
	value:       triageAgentValue(func(agent SettingsTriageAgent) any { return agent.SOSWaitsForAServerReplacementMinutes }),
	apply:       func(context.Context, *App, Settings) error { return nil },
	keepsValues: true,
}

// rowQuerier is a transaction or the database: where one row is read from.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// appliedSettingsValue reads the value a part that keeps its values last
// applied into value, and says whether there was one. A part applied
// without its section kept null, which leaves value as it was.
func appliedSettingsValue(ctx context.Context, tx rowQuerier, part string, value any) (bool, error) {
	var values string
	err := tx.QueryRowContext(ctx, `SELECT applied_values FROM settings_file_parts WHERE part=?`, part).Scan(&values)
	if errors.Is(err, sql.ErrNoRows) || values == "" || values == "null" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(values), value)
}

func triageAgentValue(value func(SettingsTriageAgent) any) func(context.Context, *App, Settings) (any, error) {
	return fileValue(func(s Settings) any {
		if s.TriageAgent == nil {
			return nil
		}
		return value(*s.TriageAgent)
	})
}

func applyTeamNameSettings(ctx context.Context, a *App, s Settings) error {
	name, err := checkTeamName(s.TeamName)
	if err != nil {
		return err
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, a.settingsPrincipal(), true); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE team_info SET name=?`, name)
		return err
	})
}

// applyPeopleSettings makes the team the people of the file, matched by
// sign-in email address, in one transaction: it adds each new person,
// changes names and roles, and deactivates each person left out, whose
// history stays. A refused deactivation, such as of a person who owns a
// claimed incident, fails the part, and the earlier people stay.
//
// A deactivation is not refused because a saved schedule names the person:
// the file's schedule names only people of the file, and the schedule part,
// which applies next, makes it the only saved schedule. Refusing would make
// a change of people and schedule together, such as one person taking over
// another's place in the rotation, impossible in one edit.
func applyPeopleSettings(ctx context.Context, a *App, s Settings) error {
	now := a.Now()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, a.settingsPrincipal(), true); err != nil {
			return err
		}
		listed := map[string]bool{}
		for _, person := range s.People {
			address, err := normalizeSignInEmailAddress(person.SignInEmailAddress)
			if err != nil {
				return err
			}
			name, err := validatePersonName(person.Name)
			if err != nil {
				return err
			}
			listed[address] = true
			existing, err := personBySignInEmailAddress(ctx, tx, address)
			switch {
			case errors.Is(err, ErrNotFound):
				_, err = insertPersonTx(ctx, tx, address, name, person.Role)
			case err == nil && (existing.Name != name || existing.Role != person.Role):
				_, err = tx.ExecContext(ctx, `UPDATE members SET name=?,role=? WHERE id=?`, name, person.Role, existing.ID)
			}
			if err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,sign_in_email_address,name,role FROM members WHERE removed_at IS NULL ORDER BY name,id`)
		if err != nil {
			return err
		}
		var leftOut []Member
		for rows.Next() {
			var m Member
			if err = rows.Scan(&m.ID, &m.SignInEmailAddress, &m.Name, &m.Role); err != nil {
				rows.Close()
				return err
			}
			if !listed[m.SignInEmailAddress] {
				leftOut = append(leftOut, m)
			}
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, m := range leftOut {
			if err := removePersonTx(ctx, tx, m, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// scheduleConfig is the one conversion of the file's schedule to a
// ScheduleConfig, for the check, the apply, and the schedules preview. The
// schedule names people by sign-in email address; person turns each address
// into what the caller needs in its place (the check keeps the normalized
// address, apply and preview use the member ID), and reports a problem with
// the address at keyPath. An empty address is allowed where emptyAllowed
// says so, and then stands for no person.
func (schedule SettingsOnCallSchedule) scheduleConfig(person func(keyPath, address string, emptyAllowed bool) (string, error)) (ScheduleConfig, error) {
	const path = "on_call_schedule"
	config := ScheduleConfig{Kind: schedule.ScheduleType, TimeZone: schedule.TimeZone, BackupKind: schedule.BackupOption}
	if schedule.HandoffHour != nil {
		config.HandoffHour = *schedule.HandoffHour
	}
	if schedule.HandoffMinute != nil {
		config.HandoffMinute = *schedule.HandoffMinute
	}
	if schedule.HandoffWeekday != nil {
		config.HandoffWeekday = *schedule.HandoffWeekday
	}
	if schedule.FirstStartDate != nil {
		config.FirstStartDate = *schedule.FirstStartDate
	}
	if schedule.CustomScheduleCode != nil {
		config.CustomScheduleCode = *schedule.CustomScheduleCode
	}
	var err error
	if schedule.BackupSignInEmailAddress != nil {
		if config.BackupMemberID, err = person(path+".backup_sign_in_email_address", *schedule.BackupSignInEmailAddress, false); err != nil {
			return config, err
		}
	}
	if schedule.AssignmentSignInEmailAddresses != nil {
		for index, address := range *schedule.AssignmentSignInEmailAddresses {
			id, err := person(fmt.Sprintf("%s.assignment_sign_in_email_addresses[%d]", path, index), address, true)
			if err != nil {
				return config, err
			}
			config.Assignments = append(config.Assignments, id)
		}
	}
	if schedule.RotationSignInEmailAddresses != nil {
		for index, address := range *schedule.RotationSignInEmailAddresses {
			id, err := person(fmt.Sprintf("%s.rotation_sign_in_email_addresses[%d]", path, index), address, false)
			if err != nil {
				return config, err
			}
			config.RotationMemberIDs = append(config.RotationMemberIDs, id)
		}
	}
	if schedule.CustomScheduleTestCases != nil {
		for index, test := range *schedule.CustomScheduleTestCases {
			testPath := fmt.Sprintf("%s.custom_schedule_test_cases[%d]", path, index)
			primary, err := person(testPath+".expected_primary_sign_in_email_address", test.ExpectedPrimarySignInEmailAddress, true)
			if err != nil {
				return config, err
			}
			backup, err := person(testPath+".expected_backup_sign_in_email_address", test.ExpectedBackupSignInEmailAddress, true)
			if err != nil {
				return config, err
			}
			config.CustomScheduleTestCases = append(config.CustomScheduleTestCases, CustomScheduleTestCase{When: test.LocalTime, ExpectedPrimaryMemberID: primary, ExpectedBackupMemberID: backup})
		}
	}
	return config, nil
}

// scheduleConfigForMembers is scheduleConfig with the member IDs of members
// in place of the addresses: apply and the schedules preview use it.
func (schedule SettingsOnCallSchedule) scheduleConfigForMembers(members []Member) (ScheduleConfig, error) {
	memberIDs := map[string]string{}
	for _, m := range members {
		memberIDs[m.SignInEmailAddress] = m.ID
	}
	return schedule.scheduleConfig(func(_, address string, emptyAllowed bool) (string, error) {
		if address == "" && emptyAllowed {
			return "", nil
		}
		normalized, err := normalizeSignInEmailAddress(address)
		if err != nil {
			return "", err
		}
		id, ok := memberIDs[normalized]
		if !ok {
			return "", fmt.Errorf("%w: unknown sign-in email address %s", ErrInvalid, address)
		}
		return id, nil
	})
}

// onCallScheduleSettingsValue is the schedule with the member IDs of the
// team's people: the saved schedule names people by member ID, so a person
// deactivated and added again changes it.
func onCallScheduleSettingsValue(ctx context.Context, a *App, s Settings) (any, error) {
	members, err := a.TeamMembers(ctx, a.settingsPrincipal())
	if err != nil {
		return nil, err
	}
	memberIDs := map[string]string{}
	for _, member := range members {
		memberIDs[member.SignInEmailAddress] = member.ID
	}
	return struct {
		Schedule  SettingsOnCallSchedule
		MemberIDs map[string]string
	}{s.OnCallSchedule, memberIDs}, nil
}

// applyOnCallScheduleSettings makes the file's schedule the saved and
// active schedule of its type, with the schedules' own validation, and for
// custom schedule code its run and test cases, then sets the time
// to claim. It removes the saved schedules of the other types: only the file
// can make a schedule active, so they would only name people who may have
// left.
func applyOnCallScheduleSettings(ctx context.Context, a *App, s Settings) error {
	p := a.settingsPrincipal()
	members, err := a.TeamMembers(ctx, p)
	if err != nil {
		return err
	}
	config, err := s.OnCallSchedule.scheduleConfigForMembers(members)
	if err != nil {
		return err
	}
	state, err := a.Schedules(ctx, p)
	if err != nil {
		return err
	}
	if config.Kind == customScheduleKind {
		_, err = a.SaveCustomScheduleCode(ctx, p, config, state.Revision)
	} else {
		_, err = a.SaveSchedule(ctx, p, config, state.Revision)
	}
	if err != nil {
		return err
	}
	if err = a.SetEscalateIfUnclaimedAfter(ctx, p, s.OnCallSchedule.EscalateIfUnclaimedAfterMinutes); err != nil {
		return err
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM coverage_schedules WHERE kind<>?`, config.Kind)
		return err
	})
}

// agentPlaybookRulesSettingsValue is the rules for running the agent's playbooks with the inventories of
// MMHotline's copy of the systems repository: an inventory added there
// changes the part, which then fails until the file lists it.
func agentPlaybookRulesSettingsValue(_ context.Context, a *App, s Settings) (any, error) {
	return struct {
		AgentPlaybookRules   map[string]SettingsInventoryAgentPlaybookRules
		InventoriesInTheCopy []string
	}{s.AgentPlaybookRules, a.inventoryNames()}, nil
}

// applyAgentPlaybookRulesSettings saves each listed inventory's preset or
// settings (confirmed change 1). Each inventory of MMHotline's copy of the
// systems repository that the file leaves out gets the safest setting for
// every kind, every fix waiting for a person's approval, and fails the part,
// which names it: no inventory goes without rules for running the agent's playbooks, and none gets a
// silent default. The saved settings of an inventory in neither are removed.
// The saved listed playbooks become the file's, each with its rule.
// Without a copy yet, only the listed inventories apply.
func applyAgentPlaybookRulesSettings(ctx context.Context, a *App, s Settings) error {
	inventories := a.inventoryNames()
	var missing []string
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		person, err := RequireMember(ctx, tx, a.settingsPrincipal(), true)
		if err != nil {
			return err
		}
		now := a.Now()
		for _, inventory := range slices.Sorted(maps.Keys(s.AgentPlaybookRules)) {
			entry := s.AgentPlaybookRules[inventory]
			preset, _ := findAgentPlaybookRulesPreset(entry.Preset)
			for _, kind := range actionkinds.Kinds() {
				setting := preset.settings[kind]
				if entry.Preset == "" {
					listed, found := entry.ActionKinds[string(kind)]
					if !found {
						// A kind the file leaves out has no saved row, which
						// is the safest setting; the file's note names it.
						if _, err := tx.ExecContext(ctx, `DELETE FROM agent_playbook_rules WHERE inventory=? AND action_kind=?`, inventory, kind); err != nil {
							return err
						}
						continue
					}
					setting = listed.agentPlaybookRule()
				}
				if err := checkAgentPlaybookRule(setting); err != nil {
					return err
				}
				if err := saveAgentPlaybookRule(ctx, tx, inventory, kind, setting, now, person.Name); err != nil {
					return err
				}
			}
		}
		for _, inventory := range inventories {
			if _, listed := s.AgentPlaybookRules[inventory]; listed {
				continue
			}
			missing = append(missing, inventory)
			for _, kind := range actionkinds.Kinds() {
				if err := saveAgentPlaybookRule(ctx, tx, inventory, kind, safestAgentPlaybookRule, now, person.Name); err != nil {
					return err
				}
			}
		}
		// The listed playbooks equal the file's: a playbook the file no
		// longer lists, or of an inventory the file leaves out, goes back to
		// the rules of its kinds of action.
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_playbook_rules_for_listed_playbooks`); err != nil {
			return err
		}
		for _, inventory := range slices.Sorted(maps.Keys(s.AgentPlaybookRules)) {
			playbooks := s.AgentPlaybookRules[inventory].Playbooks
			for _, name := range slices.Sorted(maps.Keys(playbooks)) {
				setting := playbooks[name].agentPlaybookRule()
				if err := errors.Join(checkListedPlaybookName(name), checkAgentPlaybookRule(setting)); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO agent_playbook_rules_for_listed_playbooks(inventory,playbook_name,rule,warning_wait_minutes,approvals_needed,its_approval_also_approves_in_the_same_session,starts_a_server_replacement,changed_at,changed_by) VALUES(?,?,?,?,?,?,?,?,?)`,
					inventory, name, setting.Rule, setting.WarningWaitMinutes, setting.ApprovalsNeeded, joinedPlaybookNames(playbooks[name].ItsApprovalAlsoApprovesInTheSameSession), playbooks[name].StartsAServerReplacement, now.Unix(), person.Name); err != nil {
					return err
				}
			}
		}
		saved, err := savedAgentPlaybookRules(ctx, tx, "")
		if err != nil {
			return err
		}
		for inventory := range saved {
			if _, listed := s.AgentPlaybookRules[inventory]; !listed && !slices.Contains(inventories, inventory) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM agent_playbook_rules WHERE inventory=?`, inventory); err != nil {
					return err
				}
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return err
	case len(missing) == 1:
		return fmt.Errorf("%w: inventory %s has no rules for running the agent's playbooks. Every fix in the inventory needs a person until you add its rules", errSettingsPartRefused, missing[0])
	case len(missing) > 1:
		return fmt.Errorf("%w: inventories %s have no rules for running the agent's playbooks. Every fix in the listed inventories needs a person until you add their rules", errSettingsPartRefused, strings.Join(missing, ", "))
	}
	return nil
}
