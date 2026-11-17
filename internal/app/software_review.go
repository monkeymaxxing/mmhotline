package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// The triage agent's scheduled software review: once each interval the team
// sets, MMHotline starts the triage agent with the team's task from
// scheduled_software_review_task.md in the systems repository, outside any
// triage session. It is off unless the settings file sets an interval, so
// no team pays for reviews it did not ask for.
// docs/changing-mmhotline/design/triage-agent.md#the-software-review.
//
// The review runs in triage_agent_runs are the schedule, so a restart
// neither skips a review nor runs one twice: the next review is due one
// interval after the latest one started, except a review MMHotline itself
// cut short, which starts again.

// software_review_interval holds the settings file's value: interval_hours
// is NULL for off. Without the row, the scheduled review is off too.
const softwareReviewSchema = `
CREATE TABLE software_review_interval (
 id INTEGER PRIMARY KEY CHECK(id=1),
 interval_hours INTEGER CHECK(interval_hours BETWEEN 1 AND 8760),
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL
);
`

// softwareReviewIntervalMaximumHours is 365 days: the deadline arithmetic
// needs a bound, and a longer interval is far beyond any use. The interval
// is at least 1 hour: a review can run for up to an hour, the sandbox's
// ceiling, so a shorter interval would only run reviews back to back.
const softwareReviewIntervalMaximumHours = 8760

// SoftwareReviewIntervalHours is the scheduled software review's interval:
// off, or 1 to 8760 whole hours. Inside MMHotline 0 stands for off; the
// settings file, JSON, and text say off.
type SoftwareReviewIntervalHours int

// softwareReviewIntervalOff is the scheduled software review turned off.
const softwareReviewIntervalOff SoftwareReviewIntervalHours = 0

// parseSoftwareReviewIntervalHours reads the settings file's
// software_review_interval_hours: off, or a whole number of hours.
func parseSoftwareReviewIntervalHours(value string) (SoftwareReviewIntervalHours, error) {
	if value == "off" {
		return softwareReviewIntervalOff, nil
	}
	hours, err := strconv.Atoi(value)
	if err != nil || hours < 1 || hours > softwareReviewIntervalMaximumHours {
		return 0, fmt.Errorf("must be off, or a whole number of hours from 1 to %d", softwareReviewIntervalMaximumHours)
	}
	return SoftwareReviewIntervalHours(hours), nil
}

// Text names the interval for people: "off", "every hour", or "every 24
// hours".
func (hours SoftwareReviewIntervalHours) Text() string {
	switch hours {
	case softwareReviewIntervalOff:
		return "off"
	case 1:
		return "every hour"
	}
	return fmt.Sprintf("every %d hours", int(hours))
}

// MarshalJSON gives "off" or the number, as the settings file does.
func (hours SoftwareReviewIntervalHours) MarshalJSON() ([]byte, error) {
	if hours == softwareReviewIntervalOff {
		return []byte(`"off"`), nil
	}
	return strconv.AppendInt(nil, int64(hours), 10), nil
}

// UnmarshalJSON reads what MarshalJSON gives, as the mmhotline command
// reads serve's answer.
func (hours *SoftwareReviewIntervalHours) UnmarshalJSON(data []byte) error {
	parsed, err := parseSoftwareReviewIntervalHours(strings.Trim(string(data), `"`))
	if err != nil {
		return fmt.Errorf("software_review_interval_hours %s", err)
	}
	*hours = parsed
	return nil
}

// softwareReviewIntervalNote is the settings file's note under
// triage_agent.software_review_interval_hours, so settings check, apply,
// and show say plainly whether the scheduled review runs.
func softwareReviewIntervalNote(hours SoftwareReviewIntervalHours) string {
	if hours == softwareReviewIntervalOff {
		return "Scheduled software review: off. The triage agent wakes only for alerts; sudo mmhotline triage-agent run-software-review still runs one review, such as to learn what one costs."
	}
	return "Scheduled software review: " + hours.Text() + ", with the team's task in " + softwareReviewTaskFile + " in the systems repository."
}

// softwareReviewInterval is the team's interval between scheduled software
// reviews. changedAt is nil while the settings file has not set it.
type softwareReviewInterval struct {
	hours     SoftwareReviewIntervalHours
	changedAt *time.Time
	changedBy string
}

func readSoftwareReviewInterval(ctx context.Context, tx *sql.Tx) (softwareReviewInterval, error) {
	var interval softwareReviewInterval
	var hours sql.NullInt64
	var changedAt int64
	err := tx.QueryRowContext(ctx, `SELECT interval_hours,changed_at,changed_by FROM software_review_interval WHERE id=1`).Scan(&hours, &changedAt, &interval.changedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return interval, nil
	}
	interval.hours = SoftwareReviewIntervalHours(hours.Int64)
	at := time.Unix(changedAt, 0).UTC()
	interval.changedAt = &at
	return interval, err
}

// SetSoftwareReviewInterval turns the scheduled software review off, or
// sets its whole hours, for an administrator, or root through the command.
// The change commits to the team's database, so the background loop visits
// and takes the new deadline: a shorter interval can make a review due at
// once.
func (a *App) SetSoftwareReviewInterval(ctx context.Context, p Principal, hours SoftwareReviewIntervalHours) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		person, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		if hours < 0 || hours > softwareReviewIntervalMaximumHours {
			return fmt.Errorf("%w: software_review_interval_hours must be off, or a whole number of hours from 1 to %d", ErrInvalid, softwareReviewIntervalMaximumHours)
		}
		stored := sql.NullInt64{Int64: int64(hours), Valid: hours != softwareReviewIntervalOff}
		_, err = tx.ExecContext(ctx, `INSERT INTO software_review_interval(id,interval_hours,changed_at,changed_by) VALUES(1,?,?,?)
ON CONFLICT(id) DO UPDATE SET interval_hours=excluded.interval_hours,changed_at=excluded.changed_at,changed_by=excluded.changed_by`, stored, a.Now().Unix(), person.Name)
		return err
	})
}

// softwareReviewDueAt returns when the next software review is due by its
// schedule: intervalHours after the latest review that counts started, or
// zero when none has, as the next is then due at once. A review that failed
// counts, and so does one started by hand; one that MMHotline's stop, its
// restart, or a triage session cut short does not, so it starts again.
func softwareReviewDueAt(ctx context.Context, tx *sql.Tx, intervalHours SoftwareReviewIntervalHours) (time.Time, error) {
	var lastStarted int64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(max(started_at),0) FROM triage_agent_runs WHERE purpose='software_review' AND end_cause NOT IN (?,?,?)`,
		runCutByMMHotlineStop, runCutByMMHotlineRestart, runStoppedForTriageSession).Scan(&lastStarted)
	if err != nil || lastStarted == 0 {
		return time.Time{}, err
	}
	return time.Unix(lastStarted, 0).Add(time.Duration(intervalHours) * time.Hour).UTC(), nil
}

// softwareReviewStoppedForTriageSession is why a review ended when
// MMHotline stopped it because a triage session needs the one run.
const softwareReviewStoppedForTriageSession = "MMHotline stopped the software review because a triage session needed the agent; it starts again once the session's run ends."

// nextSoftwareReviewDeadline returns when the next scheduled software review
// is due, for the background loop's next visit: one interval after the
// latest review that counts started, or a time long past when none did. It
// is zero while the scheduled review is off, and while no review can start:
// without the setup, the sandbox, or MMHotline's copy, and while the one run
// is taken, whose end changes the database and so brings a visit.
func (a *App) nextSoftwareReviewDeadline(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	if !a.oneTriageRunIsFree() || !a.systemsRepositoryCopyExists() {
		return time.Time{}, nil
	}
	var setUp bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup)`).Scan(&setUp)
	if err != nil || !setUp {
		return time.Time{}, err
	}
	interval, err := readSoftwareReviewInterval(ctx, tx)
	if err != nil || interval.hours == softwareReviewIntervalOff {
		return time.Time{}, err
	}
	due, err := softwareReviewDueAt(ctx, tx, interval.hours)
	if err == nil && due.IsZero() {
		due = time.Unix(0, 0).UTC()
	}
	return due, err
}

// DriveSoftwareReview is the background loop's turn for the
// software review. When one is due and the one triage run is free, it starts
// the review in its own goroutine, because the start fetches from the team's
// Git host and a notification must never wait for that.
func (a *App) DriveSoftwareReview(ctx context.Context) error {
	var due bool
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		deadline, err := a.nextSoftwareReviewDeadline(ctx, tx)
		due = !deadline.IsZero() && !deadline.After(a.Now())
		return err
	})
	if err != nil || !due {
		return err
	}
	a.startBesideTheLoop(func() {
		// A run that started meanwhile has the one run; the review stays
		// due for a later visit.
		if _, err := a.startSoftwareReview(ctx, "MMHotline", nil); err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "mmhotline: could not start the software review: %s\n", err)
		}
	})
	return nil
}

// RunSoftwareReviewNow starts a software review at once for an
// administrator, such as after listing a host the last review could not
// reach, or to learn what one review costs before turning the scheduled
// review on: it runs while the scheduled review is off too. It is refused
// while a run is active, and a triage session that
// needs the run stops it, as it stops any review. It counts as any review:
// the next is due one interval after it.
func (a *App) RunSoftwareReviewNow(ctx context.Context, p Principal) (string, error) {
	// The start fetches from the team's Git host; the command may stop
	// waiting meanwhile, and the review must not stop with it.
	ctx = context.WithoutCancel(ctx)
	var person Member
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { var err error; person, err = RequireMember(ctx, tx, p, true); return err }); err != nil {
		return "", err
	}
	return a.startSoftwareReview(ctx, person.Name, func(tx *sql.Tx) error {
		_, err := RequireMember(ctx, tx, p, true)
		return err
	})
}

// startSoftwareReview starts a review, started by startedBy, when the one
// triage run is free. Its prompt, with the team's task, is written once the
// run's starting commit is known (softwareReviewPromptAt). authorize, when
// set, runs in the transaction that records the run.
func (a *App) startSoftwareReview(ctx context.Context, startedBy string, authorize func(*sql.Tx) error) (string, error) {
	if !a.systemsRepositoryCopyExists() {
		return "", fmt.Errorf("%w: the systems repository copy used by MMHotline is missing", ErrInvalid)
	}
	request := triageagent.Request{Action: "start"}
	return a.startTriageAgentRunInSystemsRepository(ctx, "software_review", startedBy, request, func(tx *sql.Tx, _ string) error {
		if authorize == nil {
			return nil
		}
		return authorize(tx)
	})
}

// softwareReviewPromptAt writes a review's prompt with the team's task from
// MMHotline's copy at commitID, the commit the review starts from. An error
// says why the review did not start, for the failed review's record.
func (a *App) softwareReviewPromptAt(ctx context.Context, commitID string) (string, error) {
	task, err := readSoftwareReviewTask(ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), commitID)
	if err != nil {
		return "", err
	}
	// The advisories since the last review that finished: a review that
	// failed or was stopped may not have read them all.
	var lastReview time.Time
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var started int64
		err := tx.QueryRowContext(ctx, `SELECT coalesce(max(started_at),0) FROM triage_agent_runs WHERE purpose='software_review' AND state='finished'`).Scan(&started)
		if started != 0 {
			lastReview = time.Unix(started, 0).UTC()
		}
		return err
	}); err != nil {
		return "", err
	}
	serverName, _ := os.Hostname()
	return softwareReviewPrompt(serverName, lastReview, a.inventoryNames(), task), nil
}

// ReportCountsByAlertSeverity counts the reports a run sent to people at
// each alert severity.
type ReportCountsByAlertSeverity struct {
	SOS  int `json:"sos"`
	Warn int `json:"warn"`
	Info int `json:"info"`
}

// ReviewRun is a software review or commit review run as the triage agent page
// shows it. Its steps stay with the run: a review takes many.
type ReviewRun struct {
	RunID                       string                      `json:"run_id"`
	State                       string                      `json:"state"`
	StartedAt                   time.Time                   `json:"started_at"`
	StartedBy                   string                      `json:"started_by"`
	EndedAt                     *time.Time                  `json:"ended_at,omitempty"`
	FailureReason               string                      `json:"failure_reason"`
	CostInUSDollars             *float64                    `json:"cost_in_us_dollars,omitempty"`
	InputTokens                 int64                       `json:"input_tokens"`
	CachedInputTokens           int64                       `json:"cached_input_tokens"`
	OutputTokens                int64                       `json:"output_tokens"`
	ReportCountsByAlertSeverity ReportCountsByAlertSeverity `json:"report_counts_by_alert_severity"`
}

func (review ReviewRun) UsageText() string {
	return usageText(review.CostInUSDollars, review.InputTokens, review.CachedInputTokens, review.OutputTokens)
}

// latestReviewRun reads the latest run of purpose, software_review or commit_review, with its
// report counts; nil when there is none.
func latestReviewRun(ctx context.Context, tx *sql.Tx, purpose string) (*ReviewRun, error) {
	var review ReviewRun
	var started, ended int64
	var cost sql.NullFloat64
	err := tx.QueryRowContext(ctx, `SELECT id,state,started_at,started_by,ended_at,failure_reason,cost_in_us_dollars,input_tokens,cached_input_tokens,output_tokens FROM triage_agent_runs WHERE purpose=? ORDER BY started_at DESC,rowid DESC LIMIT 1`, purpose).Scan(&review.RunID, &review.State, &started, &review.StartedBy, &ended, &review.FailureReason, &cost, &review.InputTokens, &review.CachedInputTokens, &review.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	review.StartedAt = time.Unix(started, 0).UTC()
	if ended != 0 {
		at := time.Unix(ended, 0).UTC()
		review.EndedAt = &at
	}
	if cost.Valid {
		review.CostInUSDollars = &cost.Float64
	}
	// A refused report reached nobody.
	rows, err := tx.QueryContext(ctx, `SELECT alert_severity,count(*) FROM triage_agent_requests WHERE run_id=? AND kind='report' AND outcome='recorded' GROUP BY alert_severity`, review.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var severity string
		var count int
		if err = rows.Scan(&severity, &count); err != nil {
			return nil, err
		}
		switch severity {
		case "sos":
			review.ReportCountsByAlertSeverity.SOS = count
		case "warn":
			review.ReportCountsByAlertSeverity.Warn = count
		case "info":
			review.ReportCountsByAlertSeverity.Info = count
		}
	}
	return &review, rows.Err()
}
