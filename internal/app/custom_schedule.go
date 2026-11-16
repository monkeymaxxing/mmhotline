package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Custom schedule code: the customer's Starlark on_call(at) is run ahead of
// time in a sandbox child process, never while an SOS waits. One run answers
// every quarter hour of the next 14 days; the answers are merged into shifts
// and stored. Live coverage reads the stored shifts
// (docs/changing-mmhotline/design/scheduling.md#custom-schedule-code).

const customScheduleKind = "custom_schedule_code"

const customScheduleSchema = `
CREATE TABLE custom_schedule_timeline (configuration_revision INTEGER NOT NULL, starts_at INTEGER NOT NULL, ends_at INTEGER NOT NULL, primary_member_id TEXT NOT NULL, backup_member_id TEXT NOT NULL, PRIMARY KEY(configuration_revision, starts_at));
CREATE TABLE custom_schedule_timeline_state (id INTEGER PRIMARY KEY CHECK(id=1), timeline_configuration_revision INTEGER NOT NULL, worked_out_at INTEGER NOT NULL, shifts_until INTEGER NOT NULL, next_run_at INTEGER NOT NULL, attempted_configuration_revision INTEGER NOT NULL, attempted_member_sign_in_email_addresses_sha256 TEXT NOT NULL, failing_since INTEGER NOT NULL, few_shifts_left_notice_sent INTEGER NOT NULL, last_failure_message TEXT NOT NULL);
INSERT INTO custom_schedule_timeline_state VALUES (1,0,0,0,0,0,'',0,0,'');
CREATE TABLE custom_schedule_code_versions (
 id TEXT PRIMARY KEY,
 created_at INTEGER NOT NULL,
 created_by_member_id TEXT NOT NULL,
 created_by_name TEXT NOT NULL,
 configuration TEXT NOT NULL
);
`

const (
	customScheduleVersionsKept         = 20
	customScheduleRunsPerHour          = 60
	customScheduleDaysWorkedOutAhead   = 14
	customScheduleAnswerInterval       = 15 * time.Minute
	customScheduleRepeatedTimeLookBack = 3 * time.Hour
	customScheduleRecomputeInterval    = time.Hour
	customScheduleFewShiftsLeft        = 24 * time.Hour
)

const customScheduleNoAnswerReason = "Your custom schedule code has no answer for this time, so the backup is contacted first, then everyone available."

// CustomScheduleTestCase is one row of "Test cases". When is a local time in
// the schedule timezone, "2026-10-05T09:30". An empty expected primary means
// "No one scheduled"; an empty expected backup is not checked.
type CustomScheduleTestCase struct {
	When                    string `json:"when"`
	ExpectedPrimaryMemberID string `json:"expected_primary_member_id"`
	ExpectedBackupMemberID  string `json:"expected_backup_member_id"`
}

// CustomScheduleShift is a stretch of time with one answer. Empty member IDs
// mean "No one scheduled" (primary) or the schedule's backup option (backup).
type CustomScheduleShift struct {
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	PrimaryMemberID string    `json:"primary_member_id"`
	BackupMemberID  string    `json:"backup_member_id"`
}

type CustomScheduleTestResult struct {
	TestCase                  CustomScheduleTestCase
	Passed                    bool
	ReturnedPrimaryMemberID   string
	EffectiveBackupMemberID   string
	PrimaryMatchedExpectation bool
}

// CustomSchedulePreview is the result of one 14-day run. Problem is set when
// the code, or a test case, failed; such a version cannot be saved.
type CustomSchedulePreview struct {
	Shifts      []CustomScheduleShift
	TestResults []CustomScheduleTestResult
	Problem     *CustomScheduleCodeProblem
}

// CustomScheduleCodeVersion is a saved configuration in the team's history.
type CustomScheduleCodeVersion struct {
	ID                string
	CreatedAt         time.Time
	CreatedByMemberID string
	CreatedByName     string
	Config            ScheduleConfig
}

// CustomScheduleTimelineState is what the schedule page shows about the
// stored shifts.
type CustomScheduleTimelineState struct {
	TimelineConfigurationRevision int64
	WorkedOutAt, ShiftsUntil      time.Time
	FailingSince                  time.Time
	LastFailureMessage            string
}

var errCustomScheduleTooManyRuns = fmt.Errorf("%w: You have run your code a lot in the last hour. Try again later.", ErrRateLimited)

// checkCustomScheduleConfig is the Custom schedule's own rules, which need
// no database.
func checkCustomScheduleConfig(c ScheduleConfig) error {
	if strings.TrimSpace(c.CustomScheduleCode) == "" {
		return fmt.Errorf("%w: write your code, with a function on_call(at)", ErrInvalid)
	}
	if len(c.CustomScheduleCode) > customScheduleCodeMaxBytes || !utf8.ValidString(c.CustomScheduleCode) {
		return fmt.Errorf("%w: code must be text of at most 64 KiB", ErrInvalid)
	}
	if line := customScheduleHiddenCharacterLine(c.CustomScheduleCode); line > 0 {
		return fmt.Errorf("%w: line %d has a control or text-direction character; remove it", ErrInvalid, line)
	}
	if len(c.CustomScheduleTestCases) > customScheduleMaxTestCases {
		return fmt.Errorf("%w: at most %d test cases", ErrInvalid, customScheduleMaxTestCases)
	}
	// Next person in the rotation is not offered for Custom schedule.
	if c.BackupKind != "everyone_available" && c.BackupKind != "fixed_person" {
		return fmt.Errorf("%w: the backup option must be everyone_available or fixed_person", ErrInvalid)
	}
	if err := checkScheduleBackupPerson(c); err != nil {
		return err
	}
	for _, test := range c.CustomScheduleTestCases {
		if _, err := time.Parse("2006-01-02T15:04", test.When); err != nil {
			return fmt.Errorf("%w: test case time must look like 2026-10-05T09:30", ErrInvalid)
		}
	}
	return nil
}

// customScheduleHiddenCharacterLine returns the first line with a character
// that could make the code look different from what runs, to someone
// reading it in a terminal: a control character other than tab
// and line feed, or a text-direction mark or override. It returns 0 when
// there is none.
func customScheduleHiddenCharacterLine(code string) int {
	line := 1
	for _, r := range code {
		switch {
		case r == '\n':
			line++
		case r == '\t':
		case r < 0x20, r >= 0x7f && r <= 0x9f,
			r == 0x061c, r == 0x200e, r == 0x200f,
			r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return line
		}
	}
	return 0
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// customScheduleMemberSignInEmailAddresses maps active member IDs to their sign-in email addresses.
func (a *App) customScheduleMemberSignInEmailAddresses(ctx context.Context) (map[string]string, error) {
	signInEmailAddresses := map[string]string{}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,sign_in_email_address FROM members WHERE removed_at IS NULL ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var memberID, signInEmailAddress string
			if err := rows.Scan(&memberID, &signInEmailAddress); err != nil {
				return err
			}
			signInEmailAddresses[memberID] = signInEmailAddress
		}
		return rows.Err()
	})
	return signInEmailAddresses, err
}

func customScheduleMemberSignInEmailAddressesSHA256(signInEmailAddresses map[string]string) string {
	var lines []string
	for memberID, signInEmailAddress := range signInEmailAddresses {
		lines = append(lines, memberID+" "+signInEmailAddress)
	}
	sort.Strings(lines)
	return sha256Hex(strings.Join(lines, "\n"))
}

// customScheduleTestCaseInstant reads a test case time in the schedule
// timezone, at the quarter hour that contains it, with the same
// daylight-saving rules as handoffs: a skipped time means the next valid
// minute, a repeated time its first occurrence.
func customScheduleTestCaseInstant(when string, loc *time.Location) (time.Time, error) {
	wall, err := time.Parse("2006-01-02T15:04", when)
	if err != nil {
		return time.Time{}, err
	}
	date := time.Date(wall.Year(), wall.Month(), wall.Day(), 12, 0, 0, 0, loc)
	return dayOfWeekBoundary(date, wall.Hour(), wall.Minute()-wall.Minute()%15, loc), nil
}

// workOutCustomSchedule asks the code, in one sandbox run, about every
// quarter hour of the next 14 days and about each test case. A repeated
// local time is asked once: through the second pass of a repeated hour, the
// person on call at the end of the first pass stays on call, so a handoff
// never switches back. A run that starts in the second pass also asks about
// the last quarter hour of the first pass. Skipped local times are never
// asked.
func (a *App) workOutCustomSchedule(ctx context.Context, c ScheduleConfig, signInEmailAddresses map[string]string, from time.Time) (CustomSchedulePreview, error) {
	var preview CustomSchedulePreview
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		return preview, fmt.Errorf("%w: the schedule's time zone is not a known time zone name", ErrInvalid)
	}
	start := from.UTC().Truncate(customScheduleAnswerInterval)
	count := customScheduleDaysWorkedOutAhead * 24 * int(time.Hour/customScheduleAnswerInterval)
	request := customScheduleSandboxRequest{Code: c.CustomScheduleCode, TimeZone: c.TimeZone, MaxStepsPerAnswer: customScheduleMaxStepsPerAnswer}
	quarter := func(i int) time.Time { return start.Add(time.Duration(i) * customScheduleAnswerInterval).In(loc) }
	wallTime := func(at time.Time) string { return at.Format("2006-01-02T15:04") }
	ask := func(at time.Time) int {
		request.AskedTimes = append(request.AskedTimes, at.Format(time.RFC3339))
		return len(request.AskedTimes) - 1
	}
	// Local times of the 3 hours before start. When start's local time is
	// among them, the run starts in the second pass of a repeated hour, and
	// the answer at the end of the first pass, just before the clock went
	// back, is carried into it.
	before := int(customScheduleRepeatedTimeLookBack / customScheduleAnswerInterval)
	seenBefore := map[string]bool{}
	for back := 1; back <= before; back++ {
		seenBefore[wallTime(quarter(-back))] = true
	}
	carried := -1
	if seenBefore[wallTime(quarter(0))] {
		for back := 1; back <= before; back++ {
			if wallTime(quarter(1-back)) < wallTime(quarter(-back)) {
				carried = ask(quarter(-back))
				break
			}
		}
	}
	askedIndex := make([]int, count)
	asked := map[string]bool{}
	for i := range count {
		at := quarter(i)
		wall := wallTime(at)
		if asked[wall] || seenBefore[wall] {
			if i > 0 {
				askedIndex[i] = askedIndex[i-1]
				continue
			}
			if carried >= 0 {
				askedIndex[i] = carried
				continue
			}
		}
		asked[wall] = true
		askedIndex[i] = ask(at)
	}
	for _, test := range c.CustomScheduleTestCases {
		at, err := customScheduleTestCaseInstant(test.When, loc)
		if err != nil {
			return preview, fmt.Errorf("%w: could not parse the custom schedule test case time", ErrInvalid)
		}
		request.TestCases = append(request.TestCases, customScheduleSandboxTestCase{AskedTime: at.In(loc).Format(time.RFC3339)})
	}
	memberBySignInEmailAddress := map[string]string{}
	for memberID, signInEmailAddress := range signInEmailAddresses {
		memberBySignInEmailAddress[signInEmailAddress] = memberID
		request.TeamMemberSignInEmailAddresses = append(request.TeamMemberSignInEmailAddresses, signInEmailAddress)
	}
	sort.Strings(request.TeamMemberSignInEmailAddresses)
	response, err := a.runCustomScheduleSandbox(ctx, request)
	var problem *CustomScheduleCodeProblem
	if errors.As(err, &problem) {
		preview.Problem = problem
		return preview, nil
	}
	if err != nil {
		return preview, err
	}
	member := func(signInEmailAddress *string) string {
		if signInEmailAddress == nil {
			return ""
		}
		return memberBySignInEmailAddress[*signInEmailAddress]
	}
	for i := range count {
		answer := response.Answers[askedIndex[i]]
		startsAt := start.Add(time.Duration(i) * customScheduleAnswerInterval)
		shift := CustomScheduleShift{StartsAt: startsAt, EndsAt: startsAt.Add(customScheduleAnswerInterval), PrimaryMemberID: member(answer.PrimarySignInEmailAddress), BackupMemberID: member(answer.BackupSignInEmailAddress)}
		if last := len(preview.Shifts) - 1; last >= 0 && preview.Shifts[last].PrimaryMemberID == shift.PrimaryMemberID && preview.Shifts[last].BackupMemberID == shift.BackupMemberID {
			preview.Shifts[last].EndsAt = shift.EndsAt
			continue
		}
		preview.Shifts = append(preview.Shifts, shift)
	}
	for i, test := range c.CustomScheduleTestCases {
		answer := response.TestCaseAnswers[i]
		result := CustomScheduleTestResult{TestCase: test, ReturnedPrimaryMemberID: member(answer.PrimarySignInEmailAddress), EffectiveBackupMemberID: member(answer.BackupSignInEmailAddress)}
		if result.EffectiveBackupMemberID == "" && c.BackupKind == "fixed_person" {
			result.EffectiveBackupMemberID = c.BackupMemberID
		}
		result.PrimaryMatchedExpectation = result.ReturnedPrimaryMemberID == test.ExpectedPrimaryMemberID
		result.Passed = result.PrimaryMatchedExpectation && (test.ExpectedBackupMemberID == "" || test.ExpectedBackupMemberID == result.EffectiveBackupMemberID)
		preview.TestResults = append(preview.TestResults, result)
		if !result.Passed && preview.Problem == nil {
			// The message names the case and what the code gave, so the
			// reader knows which of the two to change.
			person := func(memberID string) string {
				if address, ok := signInEmailAddresses[memberID]; ok && memberID != "" {
					return address
				}
				return "no one"
			}
			which, expected, got := "primary", test.ExpectedPrimaryMemberID, result.ReturnedPrimaryMemberID
			if result.PrimaryMatchedExpectation {
				which, expected, got = "backup", test.ExpectedBackupMemberID, result.EffectiveBackupMemberID
			}
			preview.Problem = &CustomScheduleCodeProblem{Kind: customScheduleErrorTestCaseFailed, Message: fmt.Sprintf("The test case for %s expects the %s %s, and the code gives %s. Change the code or the test case, then preview again.", test.When, which, person(expected), person(got))}
		}
	}
	return preview, nil
}

// customScheduleUsable turns a failed preview into the error a save or an
// activation returns.
func customScheduleUsable(preview CustomSchedulePreview) error {
	if preview.Problem == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalid, preview.Problem.Error())
}

// customScheduleRunLimiter counts an administrator's previews, saves, and
// activations for the installation in memory. The hourly runs are not counted.
type customScheduleRunLimiter struct {
	mu   sync.Mutex
	runs []time.Time
}

func (l *customScheduleRunLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	var recent []time.Time
	for _, at := range l.runs {
		if now.Sub(at) < time.Hour {
			recent = append(recent, at)
		}
	}
	if len(recent) >= customScheduleRunsPerHour {
		l.runs = recent
		return false
	}
	l.runs = append(recent, now)
	return true
}

// PreviewCustomScheduleCode runs the candidate for the next 14 days and its
// test cases without saving anything. Only administrators run code.
func (a *App) PreviewCustomScheduleCode(ctx context.Context, p Principal, c ScheduleConfig) (CustomSchedulePreview, error) {
	c.Kind = customScheduleKind
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		return validateSchedule(ctx, tx, c)
	})
	if err != nil {
		return CustomSchedulePreview{}, err
	}
	if !a.customScheduleRuns.allow(a.Now()) {
		return CustomSchedulePreview{}, errCustomScheduleTooManyRuns
	}
	signInEmailAddresses, err := a.customScheduleMemberSignInEmailAddresses(ctx)
	if err != nil {
		return CustomSchedulePreview{}, err
	}
	return a.workOutCustomSchedule(ctx, c, signInEmailAddresses, a.Now())
}

// CustomScheduleSaveResult identifies the saved and active version.
type CustomScheduleSaveResult struct {
	VersionID string
}

// SaveCustomScheduleCode checks the candidate with a fresh 14-day run and
// its test cases, then saves and activates it in one team transaction.
// No transaction is held during the sandbox run.
func (a *App) SaveCustomScheduleCode(ctx context.Context, p Principal, c ScheduleConfig, expectedRevision int64) (CustomScheduleSaveResult, error) {
	var result CustomScheduleSaveResult
	c.Kind = customScheduleKind
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM coverage_settings WHERE id=1`).Scan(&revision); err != nil {
			return err
		}
		if revision != expectedRevision {
			return ErrConflict
		}
		return validateSchedule(ctx, tx, c)
	})
	if err != nil {
		return result, err
	}
	if !a.customScheduleRuns.allow(a.Now()) {
		return result, errCustomScheduleTooManyRuns
	}
	signInEmailAddresses, err := a.customScheduleMemberSignInEmailAddresses(ctx)
	if err != nil {
		return result, err
	}
	now := a.Now().UTC()
	preview, err := a.workOutCustomSchedule(ctx, c, signInEmailAddresses, now)
	if err != nil {
		return result, err
	}
	if err = customScheduleUsable(preview); err != nil {
		return result, err
	}
	version := CustomScheduleCodeVersion{ID: NewID(), CreatedAt: now, Config: c}
	memberSignInEmailAddressesSHA256 := customScheduleMemberSignInEmailAddressesSHA256(signInEmailAddresses)
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		state, err := ReadSchedules(ctx, tx)
		if err != nil {
			return err
		}
		if state.Revision != expectedRevision {
			return ErrConflict
		}
		version.CreatedByMemberID = m.ID
		version.CreatedByName = m.Name
		version.Config.Revision = state.Revision + 1
		if err = insertCustomScheduleCodeVersion(ctx, tx, version); err != nil {
			return err
		}
		return activateCustomScheduleConfig(ctx, tx, state, c, preview.Shifts, memberSignInEmailAddressesSHA256, now, CoverageChange{Kind: "schedule_saved", ActorMemberID: m.ID, At: now, Title: "The on-call schedule changed", Message: m.Name + " saved the Custom schedule. The saved schedule is active."})
	})
	if err == nil {
		result.VersionID = version.ID
	}
	return result, err
}

// activateCustomScheduleConfig makes c the saved and active Custom schedule
// with its worked-out shifts, in the caller's team transaction.
func activateCustomScheduleConfig(ctx context.Context, tx *sql.Tx, state ScheduleState, c ScheduleConfig, shifts []CustomScheduleShift, memberSignInEmailAddressesSHA256 string, now time.Time, change CoverageChange) error {
	c.Kind = customScheduleKind
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
	if err = writeCustomScheduleTimeline(ctx, tx, c.Revision, shifts, memberSignInEmailAddressesSHA256, now); err != nil {
		return err
	}
	change.Revision = c.Revision
	return recordCoverageChange(ctx, tx, change)
}

// writeCustomScheduleTimeline replaces the stored shifts from a run that
// worked, which also ends any earlier failure. The next hourly run is due in
// an hour.
func writeCustomScheduleTimeline(ctx context.Context, tx *sql.Tx, configurationRevision int64, shifts []CustomScheduleShift, memberSignInEmailAddressesSHA256 string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM custom_schedule_timeline`); err != nil {
		return err
	}
	var until int64
	for _, shift := range shifts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO custom_schedule_timeline(configuration_revision,starts_at,ends_at,primary_member_id,backup_member_id) VALUES (?,?,?,?,?)`, configurationRevision, shift.StartsAt.Unix(), shift.EndsAt.Unix(), shift.PrimaryMemberID, shift.BackupMemberID); err != nil {
			return err
		}
		until = shift.EndsAt.Unix()
	}
	_, err := tx.ExecContext(ctx, `UPDATE custom_schedule_timeline_state SET timeline_configuration_revision=?,worked_out_at=?,shifts_until=?,next_run_at=?,attempted_configuration_revision=?,attempted_member_sign_in_email_addresses_sha256=?,failing_since=0,few_shifts_left_notice_sent=0,last_failure_message='' WHERE id=1`, configurationRevision, now.Unix(), until, now.Add(customScheduleRecomputeInterval).Unix(), configurationRevision, memberSignInEmailAddressesSHA256)
	return err
}

// customScheduleAssignment reads the stored shift for the moment at. When
// the stored shifts have run out, the time counts as "No one scheduled".
func customScheduleAssignment(ctx context.Context, tx *sql.Tx, c ScheduleConfig, at time.Time) (ScheduleAssignment, error) {
	assignment := ScheduleAssignment{At: at.UTC(), BackupKind: c.BackupKind, BackupID: c.BackupMemberID}
	var primary, backup string
	err := tx.QueryRowContext(ctx, `SELECT primary_member_id,backup_member_id FROM custom_schedule_timeline WHERE configuration_revision=? AND starts_at<=? AND ends_at>?`, c.Revision, at.Unix(), at.Unix()).Scan(&primary, &backup)
	if errors.Is(err, sql.ErrNoRows) {
		assignment.CustomScheduleHasNoAnswer = true
		return assignment, nil
	}
	if err != nil {
		return assignment, err
	}
	assignment.PrimaryID = primary
	if backup != "" {
		assignment.BackupKind = "fixed_person"
		assignment.BackupID = backup
	}
	return assignment, nil
}

// scheduledAssignment is the one place live coverage and handoff notices
// read the active schedule: built-in types are evaluated, Custom schedule
// reads its stored shifts.
func scheduledAssignment(ctx context.Context, tx *sql.Tx, c ScheduleConfig, at time.Time) (ScheduleAssignment, error) {
	if c.Kind == customScheduleKind {
		return customScheduleAssignment(ctx, tx, c, at)
	}
	return EvaluateSchedule(c, at)
}

// ReadCustomScheduleTimeline returns the stored shifts from now on and the
// state of the stored shifts.
func ReadCustomScheduleTimeline(ctx context.Context, tx *sql.Tx, now time.Time) ([]CustomScheduleShift, CustomScheduleTimelineState, error) {
	var state CustomScheduleTimelineState
	var workedOut, until, failing int64
	if err := tx.QueryRowContext(ctx, `SELECT timeline_configuration_revision,worked_out_at,shifts_until,failing_since,last_failure_message FROM custom_schedule_timeline_state WHERE id=1`).Scan(&state.TimelineConfigurationRevision, &workedOut, &until, &failing, &state.LastFailureMessage); err != nil {
		return nil, state, err
	}
	state.WorkedOutAt, state.ShiftsUntil, state.FailingSince = deadlineTime(workedOut), deadlineTime(until), deadlineTime(failing)
	rows, err := tx.QueryContext(ctx, `SELECT starts_at,ends_at,primary_member_id,backup_member_id FROM custom_schedule_timeline WHERE ends_at>? ORDER BY starts_at`, now.Unix())
	if err != nil {
		return nil, state, err
	}
	var shifts []CustomScheduleShift
	for rows.Next() {
		var shift CustomScheduleShift
		var starts, ends int64
		if err = rows.Scan(&starts, &ends, &shift.PrimaryMemberID, &shift.BackupMemberID); err != nil {
			rows.Close()
			return nil, state, err
		}
		shift.StartsAt, shift.EndsAt = time.Unix(starts, 0).UTC(), time.Unix(ends, 0).UTC()
		shifts = append(shifts, shift)
	}
	return shifts, state, errors.Join(rows.Err(), rows.Close())
}

const customScheduleVersionColumns = `id,created_at,created_by_member_id,created_by_name,configuration`

func scanCustomScheduleCodeVersion(row interface{ Scan(...any) error }) (CustomScheduleCodeVersion, error) {
	var v CustomScheduleCodeVersion
	var created int64
	var config string
	if err := row.Scan(&v.ID, &created, &v.CreatedByMemberID, &v.CreatedByName, &config); err != nil {
		return v, err
	}
	v.CreatedAt = deadlineTime(created)
	return v, json.Unmarshal([]byte(config), &v.Config)
}

func insertCustomScheduleCodeVersion(ctx context.Context, tx *sql.Tx, v CustomScheduleCodeVersion) error {
	config, err := json.Marshal(v.Config)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO custom_schedule_code_versions(`+customScheduleVersionColumns+`) VALUES (?,?,?,?,?)`, v.ID, v.CreatedAt.Unix(), v.CreatedByMemberID, v.CreatedByName, string(config))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM custom_schedule_code_versions WHERE id NOT IN (SELECT id FROM custom_schedule_code_versions ORDER BY created_at DESC, rowid DESC LIMIT ?)`, customScheduleVersionsKept)
	return err
}

// ReadCustomScheduleCodeVersions returns the kept versions, newest first,
// with their saved configurations.
func ReadCustomScheduleCodeVersions(ctx context.Context, tx *sql.Tx) ([]CustomScheduleCodeVersion, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+customScheduleVersionColumns+` FROM custom_schedule_code_versions ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	var versions []CustomScheduleCodeVersion
	for rows.Next() {
		v, err := scanCustomScheduleCodeVersion(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, errors.Join(rows.Err(), rows.Close())
}

// RunCustomScheduleBackground keeps stored shifts current. It runs apart
// from incident work, so a sandbox run never delays an SOS.
func (a *App) RunCustomScheduleBackground(ctx context.Context, report func(error)) {
	for ctx.Err() == nil {
		reportPanic(report, "the custom schedule round", func() {
			usesCode, err := a.usesCustomScheduleCode(ctx)
			if err != nil && ctx.Err() == nil {
				report(fmt.Errorf("read whether the team uses custom schedule code: %w", err))
			}
			if usesCode && ctx.Err() == nil {
				if err := a.CustomScheduleBackgroundRound(ctx); err != nil && ctx.Err() == nil {
					report(fmt.Errorf("custom schedule code: %w", err))
				}
			}
		})
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// CustomScheduleBackgroundRound does one team's due custom schedule work.
func (a *App) CustomScheduleBackgroundRound(ctx context.Context) error {
	now := a.Now().UTC()
	var active *ScheduleConfig
	var nextRun int64
	var attemptedRevision int64
	var attemptedMemberSignInEmailAddressesSHA256 string
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		state, err := ReadSchedules(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range state.Configs {
			if c.Kind == customScheduleKind && state.ActiveKind == customScheduleKind {
				config := c
				active = &config
			}
		}
		return tx.QueryRowContext(ctx, `SELECT next_run_at,attempted_configuration_revision,attempted_member_sign_in_email_addresses_sha256 FROM custom_schedule_timeline_state WHERE id=1`).Scan(&nextRun, &attemptedRevision, &attemptedMemberSignInEmailAddressesSHA256)
	})
	if err != nil {
		return err
	}
	if active == nil {
		return nil
	}
	signInEmailAddresses, err := a.customScheduleMemberSignInEmailAddresses(ctx)
	if err != nil {
		return err
	}
	memberSignInEmailAddressesSHA256 := customScheduleMemberSignInEmailAddressesSHA256(signInEmailAddresses)
	// Run every hour, and right away after a change of configuration or of
	// the team's members or their sign-in email addresses (such as a member being removed).
	if now.Unix() < nextRun && attemptedRevision == active.Revision && attemptedMemberSignInEmailAddressesSHA256 == memberSignInEmailAddressesSHA256 {
		return nil
	}
	preview, runErr := a.workOutCustomSchedule(ctx, *active, signInEmailAddresses, now)
	if runErr != nil && ctx.Err() != nil {
		return nil
	}
	return a.commitCustomScheduleRun(ctx, *active, memberSignInEmailAddressesSHA256, preview, runErr, a.Now().UTC())
}

// commitCustomScheduleRun stores an hourly run's shifts only if the
// configuration it started from is still the active one; otherwise the run
// is discarded. A failed run keeps the stored shifts and tells
// administrators once, again when fewer than 24 hours of shifts remain, and
// when a run works again.
func (a *App) commitCustomScheduleRun(ctx context.Context, started ScheduleConfig, memberSignInEmailAddressesSHA256 string, preview CustomSchedulePreview, runErr error, now time.Time) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		state, err := ReadSchedules(ctx, tx)
		if err != nil {
			return err
		}
		current := false
		for _, c := range state.Configs {
			if c.Kind == customScheduleKind && state.ActiveKind == customScheduleKind && c.Revision == started.Revision {
				current = true
			}
		}
		if !current {
			return nil
		}
		var failingSince, fewShiftsNoticeSent, shiftsUntil int64
		if err = tx.QueryRowContext(ctx, `SELECT failing_since,few_shifts_left_notice_sent,shifts_until FROM custom_schedule_timeline_state WHERE id=1`).Scan(&failingSince, &fewShiftsNoticeSent, &shiftsUntil); err != nil {
			return err
		}
		failure := ""
		if runErr != nil {
			failure = "MMHotline could not run the code this time. It tries again every hour."
		} else if preview.Problem != nil {
			failure = preview.Problem.Error()
			if preview.Problem.Kind == customScheduleErrorTestCaseFailed {
				failure = "A test case failed."
			}
		}
		if failure == "" {
			if err = writeCustomScheduleTimeline(ctx, tx, started.Revision, preview.Shifts, memberSignInEmailAddressesSHA256, now); err != nil {
				return err
			}
			if failingSince != 0 {
				bestEffort(ctx, tx, "custom_schedule_works_again", func() error {
					return RecordAdministratorNotice(ctx, tx, "warn", "The custom schedule code works again: MMHotline knows who is on call", "MMHotline worked out who is on call for the next 14 days again.", now)
				})
			}
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE custom_schedule_timeline_state SET next_run_at=?,attempted_configuration_revision=?,attempted_member_sign_in_email_addresses_sha256=?,last_failure_message=? WHERE id=1`, now.Add(customScheduleRecomputeInterval).Unix(), started.Revision, memberSignInEmailAddressesSHA256, failure); err != nil {
			return err
		}
		until := "no time"
		if shiftsUntil > now.Unix() {
			until = time.Unix(shiftsUntil, 0).UTC().Format("2006-01-02 15:04 UTC")
		}
		message := fmt.Sprintf("MMHotline could not work out who is on call from your custom schedule code: %s The stored shifts stay in force; MMHotline knows who is on call until %s. After that, SOS incidents contact the backup first, then everyone available.", failure, until)
		if failingSince == 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE custom_schedule_timeline_state SET failing_since=? WHERE id=1`, now.Unix()); err != nil {
				return err
			}
			bestEffort(ctx, tx, "custom_schedule_failed", func() error {
				return RecordAdministratorNotice(ctx, tx, "warn", "MMHotline cannot work out who is on call: the custom schedule code failed", truncateRunes(message, 4000), now)
			})
			return nil
		}
		if fewShiftsNoticeSent == 0 && time.Unix(shiftsUntil, 0).Sub(now) < customScheduleFewShiftsLeft {
			if _, err = tx.ExecContext(ctx, `UPDATE custom_schedule_timeline_state SET few_shifts_left_notice_sent=1 WHERE id=1`); err != nil {
				return err
			}
			bestEffort(ctx, tx, "custom_schedule_few_shifts_left", func() error {
				return RecordAdministratorNotice(ctx, tx, "warn", "MMHotline knows who is on call for less than 24 hours: the custom schedule code still fails", truncateRunes("Fewer than 24 hours of stored shifts remain. "+message, 4000), now)
			})
		}
		return nil
	})
}
