package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	_ "time/tzdata"
)

const coverageSchema = `
CREATE TABLE coverage_settings (id INTEGER PRIMARY KEY CHECK(id=1), active_kind TEXT NOT NULL, revision INTEGER NOT NULL);
INSERT INTO coverage_settings VALUES (1,'',0);
CREATE TABLE coverage_schedules (kind TEXT PRIMARY KEY, configuration TEXT NOT NULL);
CREATE TABLE coverage_handoff (id INTEGER PRIMARY KEY CHECK(id=1), schedule_revision INTEGER NOT NULL, primary_id TEXT NOT NULL);
INSERT INTO coverage_handoff VALUES (1,-1,'');
CREATE TABLE member_availability (member_id TEXT PRIMARY KEY REFERENCES members(id), unavailable_until INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0);
CREATE TABLE team_mute (id INTEGER PRIMARY KEY CHECK(id=1), until_at INTEGER NOT NULL, actor_member_id TEXT NOT NULL, revision INTEGER NOT NULL, expired INTEGER NOT NULL);
INSERT INTO team_mute VALUES (1,0,'',0,1);
CREATE TABLE on_call_claim (id INTEGER PRIMARY KEY CHECK(id=1), member_id TEXT NOT NULL, actor_member_id TEXT NOT NULL, until_at INTEGER NOT NULL, revision INTEGER NOT NULL, expired INTEGER NOT NULL);
INSERT INTO on_call_claim VALUES (1,'','',0,0,1);
CREATE TABLE coverage_changes (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, actor_member_id TEXT NOT NULL, member_id TEXT NOT NULL, revision INTEGER NOT NULL, occurred_at INTEGER NOT NULL, until_at INTEGER NOT NULL, severity TEXT NOT NULL, title TEXT NOT NULL, message TEXT NOT NULL);
`

// Assignments are indexed by Sunday=0 for day_of_week schedules and local hour for hour_of_day.
// Empty assignments use the backup, then everyone available, never the previous slot.
type ScheduleConfig struct {
	Kind              string   `json:"kind"`
	TimeZone          string   `json:"time_zone"`
	HandoffHour       int      `json:"handoff_hour"`
	HandoffMinute     int      `json:"handoff_minute"`
	Assignments       []string `json:"assignments"`
	RotationMemberIDs []string `json:"rotation_member_ids"`
	HandoffWeekday    int      `json:"handoff_weekday"`
	FirstStartDate    string   `json:"first_start_date"`
	BackupKind        string   `json:"backup_kind"`
	BackupMemberID    string   `json:"backup_member_id"`
	Revision          int64    `json:"revision"`
	// Custom schedule only: the Starlark code and its test cases.
	CustomScheduleCode      string                   `json:"custom_schedule_code,omitempty"`
	CustomScheduleTestCases []CustomScheduleTestCase `json:"custom_schedule_test_cases,omitempty"`
}

type ScheduleState struct {
	ActiveKind string
	Revision   int64
	Configs    []ScheduleConfig
}

type ScheduleAssignment struct {
	At         time.Time
	PrimaryID  string
	BackupKind string
	BackupID   string
	// The stored custom schedule shifts have run out for this moment.
	CustomScheduleHasNoAnswer bool
}

type Coverage struct {
	At                         time.Time
	ScheduleKind               string
	ScheduleRevision           int64
	AssignedPrimaryID          string
	AssignedBackupID           string
	PrimaryIDs                 []string
	BackupIDs                  []string
	PrimaryIsEveryoneAvailable bool
	BackupIsEveryoneAvailable  bool
	EligibleMemberIDs          []string
	AllMemberIDs               []string
	Reason                     string
	OnCallClaim                OnCallClaim
	TeamMute                   TeamMute
}

// checkScheduleConfig is the schedule's own rules, which need no database:
// the settings file's check runs them with sign-in email addresses in place
// of member IDs, and validateSchedule runs them before it looks the members
// up.
func checkScheduleConfig(c ScheduleConfig) error {
	if c.Kind != "day_of_week" && c.Kind != "hour_of_day" && c.Kind != "weekly_rotation" && c.Kind != customScheduleKind {
		return fmt.Errorf("%w: the schedule type must be day_of_week, hour_of_day, weekly_rotation, or custom_schedule_code", ErrInvalid)
	}
	if c.TimeZone == "" || c.TimeZone == "Local" {
		return fmt.Errorf("%w: the schedule needs a named time zone, such as America/New_York", ErrInvalid)
	}
	if _, err := time.LoadLocation(c.TimeZone); err != nil {
		return fmt.Errorf("%w: the time zone %s is unknown; use a name such as America/New_York", ErrInvalid, c.TimeZone)
	}
	if c.Kind == customScheduleKind {
		return checkCustomScheduleConfig(c)
	}
	switch {
	case c.Kind == "weekly_rotation":
		if _, err := weeklyRotationStart(c); err != nil {
			return err
		}
	case c.Kind == "day_of_week" && len(c.Assignments) != 7:
		return fmt.Errorf("%w: a day_of_week schedule has 7 assignment slots, one for each day of the week, Sunday first", ErrInvalid)
	case c.Kind == "hour_of_day" && len(c.Assignments) != 24:
		return fmt.Errorf("%w: an hour_of_day schedule has 24 assignment slots, one for each hour of the day, 0 first", ErrInvalid)
	}
	if c.HandoffHour < 0 || c.HandoffHour > 23 || c.HandoffMinute < 0 || c.HandoffMinute > 59 {
		return fmt.Errorf("%w: the handoff hour must be 0 to 23, and the handoff minute 0 to 59", ErrInvalid)
	}
	if c.BackupKind != "everyone_available" && c.BackupKind != "fixed_person" && !(c.BackupKind == "next_in_rotation" && c.Kind == "weekly_rotation") {
		if c.Kind == "weekly_rotation" {
			return fmt.Errorf("%w: the backup option must be everyone_available, fixed_person, or next_in_rotation", ErrInvalid)
		}
		return fmt.Errorf("%w: the backup option must be everyone_available or fixed_person", ErrInvalid)
	}
	return checkScheduleBackupPerson(c)
}

// checkScheduleBackupPerson: a fixed backup names its person, and only a
// fixed backup names one.
func checkScheduleBackupPerson(c ScheduleConfig) error {
	if c.BackupKind == "fixed_person" && c.BackupMemberID == "" {
		return fmt.Errorf("%w: the fixed_person backup option needs the backup person", ErrInvalid)
	}
	if c.BackupKind != "fixed_person" && c.BackupMemberID != "" {
		return fmt.Errorf("%w: only the fixed_person backup option names a backup person", ErrInvalid)
	}
	return nil
}

// validateSchedule checks the schedule's rules and that every person it
// names is a member.
func validateSchedule(ctx context.Context, tx *sql.Tx, c ScheduleConfig) error {
	if err := checkScheduleConfig(c); err != nil {
		return err
	}
	ids := append(append([]string{}, c.Assignments...), c.BackupMemberID)
	switch c.Kind {
	case "weekly_rotation":
		ids = append(append([]string{}, c.RotationMemberIDs...), c.BackupMemberID)
	case customScheduleKind:
		ids = []string{c.BackupMemberID}
		for _, test := range c.CustomScheduleTestCases {
			ids = append(ids, test.ExpectedPrimaryMemberID, test.ExpectedBackupMemberID)
		}
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM members WHERE id=? AND removed_at IS NULL`, id).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return fmt.Errorf("%w: the schedule names a person who is not a current team member", ErrInvalid)
		}
	}
	return nil
}

func weeklyRotationStart(c ScheduleConfig) (time.Time, error) {
	start, err := time.Parse("2006-01-02", c.FirstStartDate)
	if err != nil || c.HandoffWeekday < 0 || c.HandoffWeekday > 6 || int(start.Weekday()) != c.HandoffWeekday {
		return time.Time{}, fmt.Errorf("%w: the first start date must be a date such as 2026-11-02 on the handoff weekday (0 is Sunday)", ErrInvalid)
	}
	if len(c.RotationMemberIDs) == 0 {
		return time.Time{}, fmt.Errorf("%w: rotation needs at least one member", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, id := range c.RotationMemberIDs {
		if id == "" || seen[id] {
			return time.Time{}, fmt.Errorf("%w: select a person for each rotation slot and do not list a person more than once", ErrInvalid)
		}
		seen[id] = true
	}
	return start, nil
}

// EvaluateSchedule is shared by live coverage and preview. The caller validates
// configuration before persisting or evaluating it.
func EvaluateSchedule(c ScheduleConfig, at time.Time) (ScheduleAssignment, error) {
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		return ScheduleAssignment{}, fmt.Errorf("%w: the schedule's time zone is not a known time zone name", ErrInvalid)
	}
	local := at.In(loc)
	if c.Kind == "weekly_rotation" {
		start, err := weeklyRotationStart(c)
		if err != nil || c.HandoffHour < 0 || c.HandoffHour > 23 || c.HandoffMinute < 0 || c.HandoffMinute > 59 {
			return ScheduleAssignment{}, ErrInvalid
		}
		date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, loc)
		date = date.AddDate(0, 0, -(int(date.Weekday())-c.HandoffWeekday+7)%7)
		if at.Before(dayOfWeekBoundary(date, c.HandoffHour, c.HandoffMinute, loc)) {
			date = date.AddDate(0, 0, -7)
		}
		// Compare local calendar dates in UTC, so DST never changes the week index.
		calendarDate := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
		assignment := ScheduleAssignment{At: at.UTC(), BackupKind: c.BackupKind, BackupID: c.BackupMemberID}
		if !calendarDate.Before(start) {
			weeks := (calendarDate.Unix() - start.Unix()) / (7 * 24 * 60 * 60)
			index := int(weeks % int64(len(c.RotationMemberIDs)))
			assignment.PrimaryID = c.RotationMemberIDs[index]
			if c.BackupKind == "next_in_rotation" && len(c.RotationMemberIDs) > 1 {
				assignment.BackupID = c.RotationMemberIDs[(index+1)%len(c.RotationMemberIDs)]
			}
		}
		return assignment, nil
	}
	index := local.Hour()
	if c.Kind == "day_of_week" {
		if len(c.Assignments) != 7 || c.HandoffHour < 0 || c.HandoffHour > 23 || c.HandoffMinute < 0 || c.HandoffMinute > 59 {
			return ScheduleAssignment{}, ErrInvalid
		}
		date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, loc)
		boundary := dayOfWeekBoundary(date, c.HandoffHour, c.HandoffMinute, loc)
		if at.Before(boundary) {
			date = date.AddDate(0, 0, -1)
		}
		index = int(date.Weekday())
	} else if c.Kind != "hour_of_day" || len(c.Assignments) != 24 {
		return ScheduleAssignment{}, ErrInvalid
	}
	return ScheduleAssignment{At: at.UTC(), PrimaryID: c.Assignments[index], BackupKind: c.BackupKind, BackupID: c.BackupMemberID}, nil
}

// A missing wall-clock minute hands off at the next valid minute. A repeated
// minute hands off at its first occurrence. Scanning a bounded UTC interval
// avoids time.Date's intentionally unspecified choice at a timezone transition.
func dayOfWeekBoundary(date time.Time, hour, minute int, loc *time.Location) time.Time {
	y, m, d := date.Date()
	want := time.Date(y, m, d, hour, minute, 0, 0, time.UTC)
	start := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(-24 * time.Hour)
	for i := 0; i <= 72*60; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		local := at.In(loc)
		wall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, time.UTC)
		if !wall.Before(want) {
			return at
		}
	}
	panic("timezone boundary outside supported UTC offsets")
}

func ReadSchedules(ctx context.Context, tx *sql.Tx) (ScheduleState, error) {
	var state ScheduleState
	if err := tx.QueryRowContext(ctx, `SELECT active_kind,revision FROM coverage_settings WHERE id=1`).Scan(&state.ActiveKind, &state.Revision); err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT configuration FROM coverage_schedules ORDER BY kind`)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return state, err
		}
		var c ScheduleConfig
		if err = json.Unmarshal([]byte(raw), &c); err != nil {
			return state, err
		}
		state.Configs = append(state.Configs, c)
	}
	return state, rows.Err()
}

func (a *App) Schedules(ctx context.Context, p Principal) (ScheduleState, error) {
	var state ScheduleState
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		state, err = ReadSchedules(ctx, tx)
		return err
	})
	return state, err
}

// SaveSchedule validates, saves and activates one candidate atomically. The
// expected revision is the team's schedule revision, not a browser-local value.
func (a *App) SaveSchedule(ctx context.Context, p Principal, c ScheduleConfig, expectedRevision int64) (ScheduleState, error) {
	var state ScheduleState
	if c.Kind == customScheduleKind {
		// Custom schedule code is saved by SaveCustomScheduleCode, which
		// runs it and asks for review first.
		return state, fmt.Errorf("%w: custom schedule code is saved only after a preview of the code", ErrInvalid)
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		state, err = ReadSchedules(ctx, tx)
		if err != nil {
			return err
		}
		if state.Revision != expectedRevision {
			return ErrConflict
		}
		if err = validateSchedule(ctx, tx, c); err != nil {
			return err
		}
		c.Revision = state.Revision + 1
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO coverage_schedules(kind,configuration) VALUES (?,?) ON CONFLICT(kind) DO UPDATE SET configuration=excluded.configuration`, c.Kind, string(raw)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE coverage_settings SET active_kind=?,revision=? WHERE id=1`, c.Kind, c.Revision); err != nil {
			return err
		}
		if err = recordCoverageChange(ctx, tx, CoverageChange{Kind: "schedule_saved", ActorMemberID: m.ID, Revision: c.Revision, At: a.Now(), Title: "The on-call schedule changed", Message: m.Name + " saved the " + scheduleTypeLabel(c.Kind) + " schedule. The saved schedule is active."}); err != nil {
			return err
		}
		state, err = ReadSchedules(ctx, tx)
		return err
	})
	return state, err
}

func (a *App) PreviewSchedule(ctx context.Context, p Principal, c ScheduleConfig, instants []time.Time) ([]ScheduleAssignment, error) {
	if len(instants) == 0 || len(instants) > 128 {
		return nil, ErrInvalid
	}
	var preview []ScheduleAssignment
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		if err := validateSchedule(ctx, tx, c); err != nil {
			return err
		}
		for _, at := range instants {
			assignment, err := EvaluateSchedule(c, at)
			if err != nil {
				return err
			}
			preview = append(preview, assignment)
		}
		return nil
	})
	return preview, err
}

func CoverageAt(ctx context.Context, tx *sql.Tx, at time.Time) (Coverage, error) {
	return coverageAt(ctx, tx, at, "")
}

// coverageAt is CoverageAt, counting the member unavailableMemberID, when
// not empty, as unavailable: the on-call page asks it what changes while
// the person reading is unavailable.
func coverageAt(ctx context.Context, tx *sql.Tx, at time.Time, unavailableMemberID string) (Coverage, error) {
	c := Coverage{At: at.UTC()}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,COALESCE(a.unavailable_until,0) FROM members m LEFT JOIN member_availability a ON a.member_id=m.id WHERE m.removed_at IS NULL ORDER BY m.id`)
	if err != nil {
		return c, err
	}
	eligible := map[string]bool{}
	for rows.Next() {
		var id string
		var unavailable int64
		if err = rows.Scan(&id, &unavailable); err != nil {
			rows.Close()
			return c, err
		}
		c.AllMemberIDs = append(c.AllMemberIDs, id)
		if unavailable <= at.Unix() && id != unavailableMemberID {
			eligible[id] = true
			c.EligibleMemberIDs = append(c.EligibleMemberIDs, id)
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return c, err
	}
	state, err := ReadSchedules(ctx, tx)
	if err != nil {
		return c, err
	}
	c.ScheduleKind = state.ActiveKind
	c.ScheduleRevision = state.Revision
	backupKind := "everyone_available"
	noAnswer := false
	for _, config := range state.Configs {
		if config.Kind == state.ActiveKind {
			assignment, e := scheduledAssignment(ctx, tx, config, at)
			if e != nil {
				return c, e
			}
			c.AssignedPrimaryID = assignment.PrimaryID
			c.AssignedBackupID = assignment.BackupID
			backupKind = assignment.BackupKind
			noAnswer = assignment.CustomScheduleHasNoAnswer
		}
	}
	c.OnCallClaim, err = ReadOnCallClaim(ctx, tx, at)
	if err != nil {
		return c, err
	}
	c.TeamMute, err = ReadTeamMute(ctx, tx, at)
	if err != nil {
		return c, err
	}
	primary := c.AssignedPrimaryID
	if c.OnCallClaim.Active {
		if eligible[c.OnCallClaim.MemberID] {
			primary = c.OnCallClaim.MemberID
			c.Reason = "The person who took on-call is contacted first."
		} else {
			c.Reason = "The person who took on-call is unavailable or no longer on the team, so the schedule is used."
		}
	}
	if primary != "" && eligible[primary] {
		c.PrimaryIDs = []string{primary}
	} else if (backupKind == "fixed_person" || backupKind == "next_in_rotation") && eligible[c.AssignedBackupID] {
		c.PrimaryIDs = []string{c.AssignedBackupID}
		if backupKind == "next_in_rotation" {
			c.Reason = "No available person is scheduled right now, so the next person in the rotation is contacted first."
		} else if noAnswer {
			c.Reason = customScheduleNoAnswerReason
		} else {
			c.Reason = "No available person is scheduled right now, so the fixed backup is contacted first."
		}
	} else {
		c.PrimaryIsEveryoneAvailable = true
		c.PrimaryIDs = append([]string{}, c.EligibleMemberIDs...)
		if len(c.PrimaryIDs) == 0 {
			c.Reason = "No one is available. For an SOS, every team member is contacted."
		} else if noAnswer && c.Reason == "" {
			c.Reason = customScheduleNoAnswerReason
		} else if c.Reason == "" {
			c.Reason = "No available person is scheduled right now, so everyone available is contacted."
		}
	}
	if (backupKind == "fixed_person" || backupKind == "next_in_rotation") && eligible[c.AssignedBackupID] && !containsMember(c.PrimaryIDs, c.AssignedBackupID) {
		c.BackupIDs = []string{c.AssignedBackupID}
	} else {
		c.BackupIsEveryoneAvailable = true
		for _, id := range c.EligibleMemberIDs {
			if !containsMember(c.PrimaryIDs, id) {
				c.BackupIDs = append(c.BackupIDs, id)
			}
		}
	}
	return c, nil
}

func containsMember(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func (a *App) CurrentCoverage(ctx context.Context, p Principal) (Coverage, error) {
	var coverage Coverage
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		coverage, err = CoverageAt(ctx, tx, a.Now())
		return err
	})
	return coverage, err
}

// nextScheduleHandoff returns the next time after now at which the active
// schedule may hand off to another person, so that ObserveCoverageHandoff
// looks then. It may name a time at which nothing changes.
func nextScheduleHandoff(ctx context.Context, tx *sql.Tx, state ScheduleState, now time.Time) (time.Time, error) {
	for _, c := range state.Configs {
		if c.Kind != state.ActiveKind {
			continue
		}
		if c.Kind == customScheduleKind {
			var next int64
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(at),0) FROM (SELECT starts_at AS at FROM custom_schedule_timeline WHERE starts_at>? UNION ALL SELECT ends_at FROM custom_schedule_timeline WHERE ends_at>?)`, now.Unix(), now.Unix()).Scan(&next)
			return deadlineTime(next), err
		}
		loc, err := time.LoadLocation(c.TimeZone)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: the schedule's time zone is not a known time zone name", ErrInvalid)
		}
		local := now.In(loc)
		if c.Kind == "hour_of_day" {
			// The start of the next local hour.
			return now.Truncate(time.Second).Add(time.Hour - time.Duration(local.Minute())*time.Minute - time.Duration(local.Second())*time.Second), nil
		}
		// day_of_week and weekly_rotation hand off at the handoff time of a
		// day: today's if it is still ahead, or tomorrow's.
		date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, loc)
		boundary := dayOfWeekBoundary(date, c.HandoffHour, c.HandoffMinute, loc)
		if !boundary.After(now) {
			boundary = dayOfWeekBoundary(date.AddDate(0, 0, 1), c.HandoffHour, c.HandoffMinute, loc)
		}
		return boundary, nil
	}
	return time.Time{}, nil
}

// ObserveCoverageHandoff records only the currently observed change, never each
// missed timer tick. Configuration edits establish a new baseline; on-call claims and
// personal availability have their own notices and do not generate handoffs.
func ObserveCoverageHandoff(ctx context.Context, tx *sql.Tx, at time.Time) error {
	state, err := ReadSchedules(ctx, tx)
	if err != nil {
		return err
	}
	var previousRevision int64
	var previous, current, zone string
	if err = tx.QueryRowContext(ctx, `SELECT schedule_revision,primary_id FROM coverage_handoff WHERE id=1`).Scan(&previousRevision, &previous); err != nil {
		return err
	}
	for _, config := range state.Configs {
		if config.Kind == state.ActiveKind {
			assignment, e := scheduledAssignment(ctx, tx, config, at)
			if e != nil {
				return e
			}
			current = assignment.PrimaryID
			zone = config.TimeZone
		}
	}
	if previousRevision == state.Revision && previous == current {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE coverage_handoff SET schedule_revision=?,primary_id=? WHERE id=1`, state.Revision, current); err != nil {
		return err
	}
	if previousRevision != state.Revision {
		return nil
	}
	name := "No one scheduled"
	if current != "" {
		err = tx.QueryRowContext(ctx, `SELECT name FROM members WHERE id=? AND removed_at IS NULL`, current).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			name = "a person no longer on the team; on call uses the backup or everyone available"
		} else if err != nil {
			return err
		}
	}
	return recordCoverageChange(ctx, tx, CoverageChange{Kind: "schedule_handoff", MemberID: current, Revision: state.Revision, At: at, Severity: "info", Title: name + " is on call now, by the schedule", Message: "Scheduled primary is now " + name + " (" + zone + "). Anyone who took on-call, and personal availability, still apply."})
}

// nextOnCallChange is the first moment after now at which an SOS would
// first wake someone other than it does under current, the coverage at now,
// with the coverage from that moment; a zero time when nothing changes
// within the schedule's coming handovers. Both the dashboard and the on-call page say
// "until" and "then" from it. It asks CoverageAt, which owns who is on call,
// at each moment on call can change by itself: the active schedule's coming
// handovers or shifts (comingScheduleInstants), the end of an on-call duty
// someone took, and the ends of people's unavailability.
func nextOnCallChange(ctx context.Context, tx *sql.Tx, now time.Time, current Coverage) (time.Time, Coverage, error) {
	state, err := ReadSchedules(ctx, tx)
	if err != nil {
		return time.Time{}, Coverage{}, err
	}
	var candidates []time.Time
	for _, c := range state.Configs {
		if c.Kind == state.ActiveKind {
			if candidates, err = comingScheduleInstants(ctx, tx, c, now); err != nil {
				return time.Time{}, Coverage{}, err
			}
		}
	}
	if current.OnCallClaim.Active {
		candidates = append(candidates, current.OnCallClaim.Until)
	}
	rows, err := tx.QueryContext(ctx, `SELECT unavailable_until FROM member_availability WHERE unavailable_until>?`, now.Unix())
	if err != nil {
		return time.Time{}, Coverage{}, err
	}
	for rows.Next() {
		var until int64
		if err = rows.Scan(&until); err != nil {
			rows.Close()
			return time.Time{}, Coverage{}, err
		}
		candidates = append(candidates, deadlineTime(until))
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return time.Time{}, Coverage{}, err
	}
	slices.SortFunc(candidates, time.Time.Compare)
	for _, at := range slices.CompactFunc(candidates, time.Time.Equal) {
		if !at.After(now) {
			continue
		}
		c, err := CoverageAt(ctx, tx, at)
		if err != nil {
			return time.Time{}, Coverage{}, err
		}
		if onCallMemberID(c) != onCallMemberID(current) {
			return at, c, nil
		}
	}
	return time.Time{}, Coverage{}, nil
}

// onCallMemberID is the one person an SOS wakes first under coverage c, or
// "" when it wakes everyone who is available.
func onCallMemberID(c Coverage) string {
	if c.PrimaryIsEveryoneAvailable || len(c.PrimaryIDs) != 1 {
		return ""
	}
	return c.PrimaryIDs[0]
}

// comingScheduleInstants are now and each coming moment at which schedule c
// may name someone else: a built-in type's handovers within the preview
// horizon of schedulePreviewInstants, or the start of each stored shift of
// custom schedule code. The end of the stored shifts is no handover: the
// hourly run works out more shifts before then.
func comingScheduleInstants(ctx context.Context, tx *sql.Tx, c ScheduleConfig, now time.Time) ([]time.Time, error) {
	switch c.Kind {
	case "":
		return nil, nil
	case customScheduleKind:
		shifts, timeline, err := ReadCustomScheduleTimeline(ctx, tx, now)
		if err != nil || !timeline.ShiftsUntil.After(now) {
			return nil, err
		}
		instants := []time.Time{now}
		for _, shift := range shifts {
			if shift.StartsAt.After(now) && shift.StartsAt.Before(timeline.ShiftsUntil) {
				instants = append(instants, shift.StartsAt)
			}
		}
		return instants, nil
	}
	return schedulePreviewInstants(c, now)
}

// schedulePreviewInstants gives pages and commands the same preview horizon:
// 24 hours, eight daily handoffs, or eight weekly handoffs, including now.
func schedulePreviewInstants(c ScheduleConfig, now time.Time) ([]time.Time, error) {
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("%w: the schedule's time zone is not a known time zone name", ErrInvalid)
	}
	instants := []time.Time{now}
	switch c.Kind {
	case "hour_of_day":
		previous := now.In(loc).Hour()
		for at := now.Truncate(time.Minute).Add(time.Minute); !at.After(now.Add(24 * time.Hour)); at = at.Add(time.Minute) {
			local := at.In(loc)
			if local.Hour() != previous || local.Minute() == 0 {
				instants = append(instants, at)
			}
			previous = local.Hour()
		}
	case "day_of_week", "weekly_rotation":
		if c.HandoffHour < 0 || c.HandoffHour > 23 || c.HandoffMinute < 0 || c.HandoffMinute > 59 {
			return nil, fmt.Errorf("%w: the schedule handoff hour must be 0 to 23 and the handoff minute must be 0 to 59", ErrInvalid)
		}
		start := now.In(loc)
		date := time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, loc)
		step := 1
		if c.Kind == "weekly_rotation" {
			if _, err := weeklyRotationStart(c); err != nil {
				return nil, err
			}
			date = date.AddDate(0, 0, (c.HandoffWeekday-int(date.Weekday())+7)%7)
			step = 7
		}
		if !dayOfWeekBoundary(date, c.HandoffHour, c.HandoffMinute, loc).After(now) {
			date = date.AddDate(0, 0, step)
		}
		for i := 0; i < 8; i++ {
			instants = append(instants, dayOfWeekBoundary(date.AddDate(0, 0, i*step), c.HandoffHour, c.HandoffMinute, loc))
		}
	default:
		return nil, fmt.Errorf("%w: unknown schedule type", ErrInvalid)
	}
	return instants, nil
}
