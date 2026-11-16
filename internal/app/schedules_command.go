package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ScheduleFile is the public editable form. Database IDs and revisions never
// belong in this file: operators identify people by their sign-in email addresses.
type ScheduleFile struct {
	ScheduleType                   string                 `json:"schedule_type"`
	TimeZone                       string                 `json:"time_zone"`
	HandoffHour                    int                    `json:"handoff_hour"`
	HandoffMinute                  int                    `json:"handoff_minute"`
	AssignmentSignInEmailAddresses []string               `json:"assignment_sign_in_email_addresses"`
	RotationSignInEmailAddresses   []string               `json:"rotation_sign_in_email_addresses"`
	HandoffWeekday                 int                    `json:"handoff_weekday"`
	FirstStartDate                 string                 `json:"first_start_date"`
	BackupOption                   string                 `json:"backup_option"`
	BackupSignInEmailAddress       string                 `json:"backup_sign_in_email_address"`
	CustomScheduleCode             string                 `json:"custom_schedule_code,omitempty"`
	CustomScheduleTestCases        []ScheduleFileTestCase `json:"custom_schedule_test_cases,omitempty"`
}
type ScheduleFileTestCase struct {
	LocalTime                         string `json:"local_time"`
	ExpectedPrimarySignInEmailAddress string `json:"expected_primary_sign_in_email_address"`
	ExpectedBackupSignInEmailAddress  string `json:"expected_backup_sign_in_email_address"`
}

// SchedulesCommandRequest carries the schedule to preview: the
// on_call_schedule of a settings file.
type SchedulesCommandRequest struct {
	Schedule *SettingsOnCallSchedule `json:"schedule,omitempty"`
}
type CommandScheduleState struct {
	ActiveScheduleType     string         `json:"active_schedule_type"`
	Revision               int64          `json:"revision"`
	ScheduleConfigurations []ScheduleFile `json:"schedule_configurations"`
	// EscalateIfUnclaimedAfterMinutes is how long each escalation step waits for a
	// claim before the next step starts.
	EscalateIfUnclaimedAfterMinutes int `json:"escalate_if_unclaimed_after_minutes"`
	// OnCallNow is whom an SOS wakes first now and whom next, from the
	// coverage the dashboard shows: the schedule with unavailability and
	// taking on-call counted.
	OnCallNow *CommandOnCallNow `json:"on_call_now,omitempty"`
}

type CommandOnCallPerson struct {
	Name               string `json:"name"`
	SignInEmailAddress string `json:"sign_in_email_address"`
}

type CommandOnCallNow struct {
	At                        time.Time             `json:"at"`
	OnCall                    []CommandOnCallPerson `json:"on_call"`
	OnCallIsEveryoneAvailable bool                  `json:"on_call_is_everyone_available"`
	Backup                    []CommandOnCallPerson `json:"backup"`
	BackupIsEveryoneAvailable bool                  `json:"backup_is_everyone_available"`
	// Reason says why coverage differs from the plain schedule, such as
	// nobody being scheduled; empty otherwise.
	Reason string `json:"reason,omitempty"`
	// TeamMutedUntil is set while the team is muted: no Pushover
	// notification or automated voice call goes out until then, so an SOS
	// wakes nobody.
	TeamMutedUntil *time.Time `json:"team_muted_until,omitempty"`
}

// Text is the on-call lines of schedules show.
func (n CommandOnCallNow) Text() string {
	people := func(list []CommandOnCallPerson) string {
		var names []string
		for _, person := range list {
			names = append(names, person.Name+" ("+person.SignInEmailAddress+")")
		}
		return strings.Join(names, ", ")
	}
	var text string
	switch {
	case len(n.OnCall) == 0:
		text = "On call now: nobody."
	case n.OnCallIsEveryoneAvailable:
		text = "On call now: everyone available: " + people(n.OnCall) + "."
	default:
		text = "On call now: " + people(n.OnCall) + "."
	}
	if !n.OnCallIsEveryoneAvailable {
		switch {
		case len(n.Backup) == 0:
			text += " Backup now: nobody else is available."
		case n.BackupIsEveryoneAvailable:
			text += " Backup now: everyone else available: " + people(n.Backup) + "."
		default:
			text += " Backup now: " + people(n.Backup) + "."
		}
	}
	if n.Reason != "" {
		text += " " + n.Reason
	}
	if n.TeamMutedUntil != nil {
		text += " The team is muted until " + commandTime(*n.TeamMutedUntil) + ": MMHotline sends no Pushover notifications or automated voice calls, so an SOS wakes nobody until then."
	}
	return text
}

// onCallNow reads the coverage at this moment, as the dashboard's On call
// panel does.
func (a *App) onCallNow(ctx context.Context) (*CommandOnCallNow, error) {
	now := &CommandOnCallNow{At: a.Now(), OnCall: []CommandOnCallPerson{}, Backup: []CommandOnCallPerson{}}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		coverage, err := CoverageAt(ctx, tx, now.At)
		if err != nil {
			return err
		}
		person := func(id string) (CommandOnCallPerson, error) {
			var p CommandOnCallPerson
			return p, tx.QueryRowContext(ctx, `SELECT name,sign_in_email_address FROM members WHERE id=?`, id).Scan(&p.Name, &p.SignInEmailAddress)
		}
		for _, list := range []struct {
			ids []string
			to  *[]CommandOnCallPerson
		}{{coverage.PrimaryIDs, &now.OnCall}, {coverage.BackupIDs, &now.Backup}} {
			for _, id := range list.ids {
				p, err := person(id)
				if err != nil {
					return err
				}
				*list.to = append(*list.to, p)
			}
		}
		now.OnCallIsEveryoneAvailable, now.BackupIsEveryoneAvailable, now.Reason = coverage.PrimaryIsEveryoneAvailable, coverage.BackupIsEveryoneAvailable, coverage.Reason
		mute, err := ReadTeamMute(ctx, tx, now.At)
		if err != nil {
			return err
		}
		if mute.Active {
			now.TeamMutedUntil = &mute.Until
		}
		return nil
	})
	return now, err
}

type CommandScheduleChange struct {
	At                        time.Time `json:"at"`
	PrimarySignInEmailAddress string    `json:"primary_sign_in_email_address"`
	BackupOption              string    `json:"backup_option"`
	BackupSignInEmailAddress  string    `json:"backup_sign_in_email_address"`
}

func scheduleFile(c ScheduleConfig, members []Member) (ScheduleFile, error) {
	byID := map[string]string{}
	for _, m := range members {
		byID[m.ID] = m.SignInEmailAddress
	}
	lookup := func(id string) (string, error) {
		if id == "" {
			return "", nil
		}
		signInEmailAddress, ok := byID[id]
		if !ok {
			return "", fmt.Errorf("%w: saved schedule member %s has no sign-in email address", ErrNotFound, id)
		}
		return signInEmailAddress, nil
	}
	f := ScheduleFile{ScheduleType: c.Kind, TimeZone: c.TimeZone, HandoffHour: c.HandoffHour, HandoffMinute: c.HandoffMinute, HandoffWeekday: c.HandoffWeekday, FirstStartDate: c.FirstStartDate, BackupOption: c.BackupKind, CustomScheduleCode: c.CustomScheduleCode, AssignmentSignInEmailAddresses: []string{}, RotationSignInEmailAddresses: []string{}}
	var err error
	f.BackupSignInEmailAddress, err = lookup(c.BackupMemberID)
	if err != nil {
		return f, err
	}
	for _, id := range c.Assignments {
		signInEmailAddress, err := lookup(id)
		if err != nil {
			return f, err
		}
		f.AssignmentSignInEmailAddresses = append(f.AssignmentSignInEmailAddresses, signInEmailAddress)
	}
	for _, id := range c.RotationMemberIDs {
		signInEmailAddress, err := lookup(id)
		if err != nil {
			return f, err
		}
		f.RotationSignInEmailAddresses = append(f.RotationSignInEmailAddresses, signInEmailAddress)
	}
	for _, test := range c.CustomScheduleTestCases {
		primary, err := lookup(test.ExpectedPrimaryMemberID)
		if err != nil {
			return f, err
		}
		backup, err := lookup(test.ExpectedBackupMemberID)
		if err != nil {
			return f, err
		}
		f.CustomScheduleTestCases = append(f.CustomScheduleTestCases, ScheduleFileTestCase{LocalTime: test.When, ExpectedPrimarySignInEmailAddress: primary, ExpectedBackupSignInEmailAddress: backup})
	}
	return f, nil
}
func (f ScheduleFile) Text() string {
	text := fmt.Sprintf("Saved %s schedule in %s. Handoff: %02d:%02d; weekday: %d (Sunday is 0); first start date: %s. Backup option: %s; fixed backup: %s.", f.ScheduleType, f.TimeZone, f.HandoffHour, f.HandoffMinute, f.HandoffWeekday, f.FirstStartDate, f.BackupOption, displaySignInEmailAddress(f.BackupSignInEmailAddress))
	if f.ScheduleType == "custom_schedule_code" {
		// The code decides the handoffs and the backup.
		text = fmt.Sprintf("Saved %s schedule in %s.", f.ScheduleType, f.TimeZone)
	}
	for i, signInEmailAddress := range f.AssignmentSignInEmailAddresses {
		text += fmt.Sprintf("\nAssignment slot %d: %s.", i, displaySignInEmailAddress(signInEmailAddress))
	}
	for i, signInEmailAddress := range f.RotationSignInEmailAddresses {
		text += fmt.Sprintf("\nRotation position %d: %s.", i+1, signInEmailAddress)
	}
	if f.CustomScheduleCode != "" {
		text += "\nCustom schedule code:\n" + f.CustomScheduleCode
	}
	for _, test := range f.CustomScheduleTestCases {
		backup := "backup not checked"
		if test.ExpectedBackupSignInEmailAddress != "" {
			backup = "expected backup " + test.ExpectedBackupSignInEmailAddress
		}
		text += fmt.Sprintf("\nTest at local time %s: expected primary %s; %s.", test.LocalTime, displaySignInEmailAddress(test.ExpectedPrimarySignInEmailAddress), backup)
	}
	return text
}

// schedulesCommand shows the saved schedule, and previews the schedule of a
// settings file before it applies: the settings file's on_call_schedule part
// saves and activates schedules.
func (a *App) schedulesCommand(ctx context.Context, p Principal, action string, request SchedulesCommandRequest, result *CommandResult) error {
	switch action {
	case "show":
	case "preview":
		if request.Schedule == nil {
			return fmt.Errorf("%w: --settings-file is required", ErrInvalid)
		}
		members, err := a.TeamMembers(ctx, p)
		if err != nil {
			return err
		}
		config, err := request.Schedule.scheduleConfigForMembers(members)
		if err != nil {
			return fmt.Errorf("%w; the schedule names only people already on the team, so apply the settings file with those people first", err)
		}
		var assignments []ScheduleAssignment
		if config.Kind == customScheduleKind {
			preview, err := a.PreviewCustomScheduleCode(ctx, p, config)
			if err != nil {
				return err
			}
			if err = customScheduleUsable(preview); err != nil {
				return err
			}
			for _, shift := range preview.Shifts {
				backup := shift.BackupMemberID
				backupOption := config.BackupKind
				if backup != "" {
					backupOption = "fixed_person"
				}
				if backup == "" && config.BackupKind == "fixed_person" {
					backup = config.BackupMemberID
				}
				assignments = append(assignments, ScheduleAssignment{At: shift.StartsAt, PrimaryID: shift.PrimaryMemberID, BackupKind: backupOption, BackupID: backup})
			}
		} else {
			instants, err := schedulePreviewInstants(config, a.Now())
			if err != nil {
				return err
			}
			assignments, err = a.PreviewSchedule(ctx, p, config, instants)
			if err != nil {
				return err
			}
		}
		byID := map[string]string{}
		for _, m := range members {
			byID[m.ID] = m.SignInEmailAddress
		}
		for i, assignment := range assignments {
			if i > 0 && assignment.PrimaryID == assignments[i-1].PrimaryID && assignment.BackupID == assignments[i-1].BackupID {
				continue
			}
			result.Preview = append(result.Preview, CommandScheduleChange{At: assignment.At, PrimarySignInEmailAddress: byID[assignment.PrimaryID], BackupOption: assignment.BackupKind, BackupSignInEmailAddress: byID[assignment.BackupID]})
		}
		result.Message = "Schedule preview complete. Nothing was saved."
		return nil
	default:
		return ErrNotFound
	}
	state, err := a.Schedules(ctx, p)
	if err != nil {
		return err
	}
	window, err := a.EscalateIfUnclaimedAfter(ctx, p)
	if err != nil {
		return err
	}
	// Custom test expectations can retain removed people in saved history.
	history, err := a.membersForHistory(ctx, p)
	if err != nil {
		return err
	}
	result.ScheduleState = &CommandScheduleState{ActiveScheduleType: state.ActiveKind, Revision: state.Revision, ScheduleConfigurations: []ScheduleFile{}, EscalateIfUnclaimedAfterMinutes: int(window / time.Minute)}
	for _, config := range state.Configs {
		file, err := scheduleFile(config, history)
		if err != nil {
			return err
		}
		result.ScheduleState.ScheduleConfigurations = append(result.ScheduleState.ScheduleConfigurations, file)
	}
	if result.ScheduleState.OnCallNow, err = a.onCallNow(ctx); err != nil {
		return err
	}
	result.Message = "Listed the saved schedule, set in " + a.SettingsFilePath() + ", in on_call_schedule."
	return nil
}
