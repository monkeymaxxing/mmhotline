package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mmhotline/internal/cloudflareaccess"
	"mmhotline/internal/cloudflareapi"
	_ "modernc.org/sqlite"
)

var (
	ErrInvalid      = errors.New("invalid input")
	ErrUnauthorized = errors.New("authentication required")
	ErrForbidden    = errors.New("access denied")
	ErrConflict     = errors.New("conflicting state")
	ErrNotFound     = errors.New("not found")
	ErrRateLimited  = errors.New("too many attempts; try again later")
)

const RoleAdministrator = "administrator"
const RoleMember = "member"

// Principal identifies a signed-in request, a trusted local command, or the
// settings file as it applies. Membership is rechecked in each team
// operation.
type Principal struct {
	SignInEmailAddress string
	UnixUser           string
	// SettingsFile is the settings file's path while it applies; what it
	// changes says "changed by the settings file PATH".
	SettingsFile string
}
type Member struct {
	ID                 string `json:"id"`
	SignInEmailAddress string `json:"sign_in_email_address"`
	Name               string `json:"name"`
	Role               string `json:"role"`
}
type App struct {
	Store *Store
	Now   func() time.Time
	// ContactMethodTypes holds the configured contact method types that can send, by kind.
	// Production readers use configuredContactMethodTypes; maps are replaced, never mutated.
	ContactMethodTypes         map[string]ContactMethodType
	contactMethodTypesMu       sync.RWMutex
	sendingServiceSetupMu      sync.Mutex
	contactMethodTypesChanged  chan struct{}
	fakeSendingServicesURL     string
	AccessVerifier             *cloudflareaccess.Verifier
	WebListenAddress           string
	WebURL                     string
	AlertIntakeListenAddress   string
	rejected                   *rejectedRequests
	alertSourceCheckInTracking *alertSourceCheckInTracking
	teamDeadline               *teamDeadline
	customScheduleSandboxSlots *customScheduleSandboxSlots
	customScheduleRuns         *customScheduleRunLimiter
	triageAgent                triageAgentRuntime

	// FleetViewSocket is where systemd runs the settings file's
	// fleet_view_command as root for each connection: the dashboard's
	// servers come from it.
	FleetViewSocket   string
	systemPanelsCache systemPanelsCache
	// settingsFile is the settings file and what applied from it.
	settingsFile settingsFileRuntime
	// Cloudflare is where the MMHotline-down alarm and status page set-ups and posts
	// reach Cloudflare; tests point it at internal/fakecloudflare.
	Cloudflare cloudflareapi.Addresses
	downAlarm  *downAlarmRuntime
}

func New(store *Store) *App {
	deadline := &teamDeadline{}
	store.teamChanged = deadline.changed
	a := &App{Store: store, Now: time.Now, contactMethodTypesChanged: make(chan struct{}), rejected: newRejectedRequests(), alertSourceCheckInTracking: newAlertSourceCheckInTracking(), teamDeadline: deadline, customScheduleSandboxSlots: newCustomScheduleSandboxSlots(), customScheduleRuns: &customScheduleRunLimiter{}, triageAgent: triageAgentRuntime{mmxSearchPath: MMXSearchPath, mmxRunTimeLimit: mmxRunTimeLimit, codingAgentAPIBaseURLs: codingAgentAPIBaseURLs("")}, Cloudflare: cloudflareapi.CloudflareAddresses(), downAlarm: newDownAlarmRuntime()}
	a.triageAgent.playbookRuns, a.triageAgent.playbookRunner = a, a
	return a
}

// recordIDPattern validates the IDs MMHotline makes, such as delivery and
// attempt IDs, at request boundaries; alert source IDs, which a team may
// choose, follow alertSourceIDPattern.
var recordIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Store owns the installation's database and reports committed changes to the scheduler.
type Store struct {
	Dir         string
	db          *sql.DB
	teamChanged func()
}

func openDB(path string) (*sql.DB, error) {
	// A relative path would be read as the file URI's host name.
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// checkTeamName returns the team name without surrounding spaces, or why it
// is refused.
func checkTeamName(teamName string) (string, error) {
	teamName = strings.TrimSpace(teamName)
	if teamName == "" || !utf8.ValidString(teamName) || utf8.RuneCountInString(teamName) > 100 {
		return "", fmt.Errorf("%w: team name must contain 1–100 characters", ErrInvalid)
	}
	return teamName, nil
}

// Setup creates the installation and its team name, never replacing existing state.
func Setup(ctx context.Context, dir, teamName string) (err error) {
	if teamName, err = checkTeamName(teamName); err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "mmhotline.db")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return fmt.Errorf("setup cannot replace the existing mmhotline.db database")
	}
	if err != nil {
		return err
	}
	file.Close()
	defer func() {
		if err != nil {
			os.Remove(path)
			os.Remove(path + "-wal")
			os.Remove(path + "-shm")
		}
	}()
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, currentDatabaseFormat())); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO team_info(name) VALUES(?)`, teamName); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO triage_agent_internet_access(id,internet_access,changed_at,changed_by) VALUES(1,'listed_hosts_only',?,'installation setup')`, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "mmhotline.db")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("run setup first: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("run setup first: mmhotline.db must be a regular file")
	}
	db, err := openDB(path)
	if err != nil {
		return nil, fmt.Errorf("run setup first: %w", err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM team_info`).Scan(&count); err != nil {
		db.Close()
		return nil, fmt.Errorf("run setup first: read team name: %w", err)
	}
	if count != 1 {
		db.Close()
		return nil, fmt.Errorf("run setup first: database must contain exactly one team_info row (found %d)", count)
	}
	if err = upgradeDatabaseFormat(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{Dir: dir, db: db}, nil
}

// The database format is the shape of mmhotline.db's tables, numbered in
// SQLite's user_version. Format 1 is the first release's: Setup's schema
// before any upgrade step, kept in testdata/database_format_1.sql, which
// never changes. An upgrade keeps the database and every row:
//
//   - a new format adds tables and columns;
//   - to allow a new value in a CHECK, a step may rebuild that one table,
//     copying every row;
//   - nothing is renamed or removed, so a table or column keeps its name
//     even when the product's word for it changes.
//
// The schema (schema.go) is always the newest format, which Setup creates
// whole. databaseFormatUpgrades[i] takes format i+1 to format i+2 inside
// the transaction that records the new number, so each step runs once, in
// order. TestADatabaseOfEachFormatUpgradesToTheSetupSchema checks that the
// steps lead from format 1 to the schema.
var databaseFormatUpgrades = []func(*sql.Tx) error{
	// Format 2: the incident page's link to the status page notice it posted.
	func(tx *sql.Tx) error { _, err := tx.Exec(statusPageIncidentNoticesSchema); return err },
	// Format 3: what ended each triage agent run, as a value decisions read.
	addRunEndCause,
	// Format 4: the hold rules: a held warn's release, the alerts each of
	// the triage agent's reports and requests covers, the alert each
	// report sent, and an SOS's
	// subscribed notification that waits with its first waking step.
	addHoldRules,
	// Format 5: an alert source's repeated_warnings setting, the repeats
	// folded into a warn, and the folded repeats' retry identities.
	addRepeatedWarnings,
	// Format 6: the triage agent's run limit: its notice on the setup row,
	// the alerts it passed over, the scheduled software review it skipped,
	// and an index on the runs' start times, which the count reads.
	addRunLimit,
}

// currentDatabaseFormat is the format Setup creates and Open upgrades to.
func currentDatabaseFormat() int { return 1 + len(databaseFormatUpgrades) }

// upgradeDatabaseFormat runs, in order, each upgrade step the database has
// not had. It refuses a database without a format number, which a build
// from before the first release set up, and a database of a newer format,
// which an older program would misread.
func upgradeDatabaseFormat(db *sql.DB) error {
	var format int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&format); err != nil {
		return fmt.Errorf("read the database format: %w", err)
	}
	switch {
	case format == 0:
		return errors.New("mmhotline.db has no database format number: a build from before the first release set it up, and MMHotline cannot upgrade it; move the state directory away and run setup to make a new database")
	case format > currentDatabaseFormat():
		return fmt.Errorf("mmhotline.db has database format %d, and this program knows formats up to %d: a newer release wrote it, so install that release or a newer one", format, currentDatabaseFormat())
	}
	for ; format < currentDatabaseFormat(); format++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err = databaseFormatUpgrades[format-1](tx); err == nil {
			_, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, format+1))
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("upgrade the database from format %d to %d: %w", format, format+1, err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// WithTransaction runs an operation and counts committed database changes.
func (s *Store) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The database has one connection, so its change count isolates this transaction.
	var before, after int64
	if s.teamChanged != nil {
		if err = tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
			return err
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	if s.teamChanged != nil {
		if err = tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if after != before {
		s.teamChanged()
	}
	return nil
}

// RequireMember must run in the same transaction as the action it authorizes.
func RequireMember(ctx context.Context, tx *sql.Tx, p Principal, admin bool) (Member, error) {
	var m Member
	if p.SettingsFile != "" {
		return Member{Name: "the settings file " + p.SettingsFile, Role: RoleAdministrator}, nil
	}
	if p.UnixUser != "" {
		return Member{Name: "the mmhotline command, run by " + p.UnixUser, Role: RoleAdministrator}, nil
	}
	if p.SignInEmailAddress == "" {
		return m, ErrUnauthorized
	}
	err := tx.QueryRowContext(ctx, `SELECT id,sign_in_email_address,name,role FROM members WHERE sign_in_email_address=? AND removed_at IS NULL`, strings.ToLower(p.SignInEmailAddress)).Scan(&m.ID, &m.SignInEmailAddress, &m.Name, &m.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrForbidden
	}
	if err != nil {
		return m, err
	}
	if admin && m.Role != RoleAdministrator {
		return m, ErrForbidden
	}
	return m, nil
}

func (a *App) CurrentMember(ctx context.Context, p Principal) (Member, error) {
	var m Member
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { var err error; m, err = RequireMember(ctx, tx, p, false); return err })
	return m, err
}
