package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// databaseShape lists each table's columns, and each index's and trigger's
// SQL, one line each, sorted: what an upgraded database and a new one must
// share. A column added by ALTER TABLE has different CREATE TABLE text than
// the same column in a new table, so tables compare by their columns.
func databaseShape(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, coalesce(sql,'') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	type object struct{ kind, name, sql string }
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.kind, &o.name, &o.sql); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, o := range objects {
		if o.kind != "table" {
			shape = append(shape, o.kind+" "+o.name+": "+strings.Join(strings.Fields(o.sql), " "))
			continue
		}
		columns, err := db.Query(`SELECT name, type, "notnull", coalesce(dflt_value,''), pk FROM pragma_table_info(?)`, o.name)
		if err != nil {
			t.Fatal(err)
		}
		for columns.Next() {
			var name, kind, value string
			var notNull, pk int
			if err := columns.Scan(&name, &kind, &notNull, &value, &pk); err != nil {
				t.Fatal(err)
			}
			shape = append(shape, fmt.Sprintf("column %s.%s %s notnull=%d default=%s pk=%d", o.name, name, kind, notNull, value, pk))
		}
		if err := columns.Close(); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(shape)
	return shape
}

func databaseFormat(t *testing.T, db *sql.DB) int {
	t.Helper()
	var format int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&format); err != nil {
		t.Fatal(err)
	}
	return format
}

// writeDatabase makes dir/mmhotline.db from the statements, at the format
// number given, as an earlier release left it.
func writeDatabase(t *testing.T, dir string, format int, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "mmhotline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range append(statements, fmt.Sprintf(`PRAGMA user_version=%d`, format)) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
}

// An upgrade keeps the database and every row: a database the first
// release set up, with format 1's tables from testdata, opens at the newest
// format with the same tables, columns, indexes, and triggers as a new
// database, and keeps its rows.
func TestADatabaseOfEachFormatUpgradesToTheSetupSchema(t *testing.T) {
	t.Parallel()
	formatOne, err := os.ReadFile(filepath.Join("testdata", "database_format_1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	released := t.TempDir()
	writeDatabase(t, released, 1, string(formatOne),
		`INSERT INTO team_info(name) VALUES('Released team')`,
		`INSERT INTO members(id,sign_in_email_address,name,role) VALUES('m1','barbara@example.com','Barbara Example','administrator')`,
		`INSERT INTO triage_agent_internet_access(id,internet_access,changed_at,changed_by) VALUES(1,'listed_hosts_only',1,'installation setup')`)
	upgraded, err := Open(released)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()

	fresh := t.TempDir()
	if err := Setup(context.Background(), fresh, "Fixture team"); err != nil {
		t.Fatal(err)
	}
	current, err := Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()

	if got, want := databaseFormat(t, upgraded.db), currentDatabaseFormat(); got != want {
		t.Fatalf("the upgraded database has format %d, want %d", got, want)
	}
	if got := databaseFormat(t, current.db); got != currentDatabaseFormat() {
		t.Fatalf("setup made format %d, want %d", got, currentDatabaseFormat())
	}
	upgradedShape, currentShape := databaseShape(t, upgraded.db), databaseShape(t, current.db)
	for _, line := range currentShape {
		if !slices.Contains(upgradedShape, line) {
			t.Errorf("the upgraded database lacks %s: a schema change needs an upgrade step in databaseFormatUpgrades", line)
		}
	}
	for _, line := range upgradedShape {
		if !slices.Contains(currentShape, line) {
			t.Errorf("a new database lacks %s: nothing is renamed or removed after the release", line)
		}
	}
	var team, member string
	if err := upgraded.db.QueryRow(`SELECT name FROM team_info`).Scan(&team); err != nil || team != "Released team" {
		t.Fatalf("team %q, %v", team, err)
	}
	if err := upgraded.db.QueryRow(`SELECT name FROM members WHERE id='m1'`).Scan(&member); err != nil || member != "Barbara Example" {
		t.Fatalf("member %q, %v", member, err)
	}
}

// Open refuses a database it cannot upgrade, and changes nothing: one with
// no format number, from a build before the first release, and one of a
// newer format than the program knows.
func TestOpenRefusesADatabaseItCannotUpgrade(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		format int
		want   string
	}{
		{0, "mmhotline.db has no database format number"},
		{currentDatabaseFormat() + 1, fmt.Sprintf("mmhotline.db has database format %d, and this program knows formats up to %d", currentDatabaseFormat()+1, currentDatabaseFormat())},
	} {
		dir := t.TempDir()
		if err := Setup(context.Background(), dir, "Fixture team"); err != nil {
			t.Fatal(err)
		}
		writeDatabase(t, dir, c.format)
		store, err := Open(dir)
		if err == nil {
			store.Close()
			t.Fatalf("format %d opened", c.format)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("format %d: %v, want %q", c.format, err, c.want)
		}
	}
}

// Format 3 gives each run of an earlier release the end cause its recorded
// reason says. The reasons are the words every earlier release wrote. Where
// the words cannot tell MMHotline's stop from the run's own end, the cause
// is unknown rather than a guess, and only the listed causes are allowed.
func TestUpgradeGivesEachRunTheEndCauseItsReasonRecorded(t *testing.T) {
	t.Parallel()
	formatOne, err := os.ReadFile(filepath.Join("testdata", "database_format_1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	runs := []struct{ purpose, state, reason, want string }{
		{"triage_session", "running", "", ""},
		{"triage_session", "finished", "", "ended_on_its_own"},
		{"setup_test", "finished", "The coding agent answered, but not with the test sentence.", "ended_on_its_own"},
		{"sign_in_check", "failed", "401 Unauthorized: invalid bearer token", "ended_on_its_own"},
		{"setup_test", "failed", "Codex refused the API key: 401 Unauthorized", "ended_on_its_own"},
		{"triage_session", "failed", "The run ended without a result.", "ended_on_its_own"},
		{"triage_session", "failed", "the coding agent run was stopped before the coding agent reported a session ID", "ended_on_its_own"},
		{"triage_session", "stopped", "The triage agent run was stopped.", "ended_on_its_own"},
		{"triage_session", "failed", "MMHotline stopped during the run.", "mmhotline_stopped"},
		{"sign_in_check", "failed", "MMHotline stopped during the run.", "mmhotline_stopped"},
		{"software_review", "failed", "MMHotline restarted during the run.", "mmhotline_restarted"},
		{"setup_test", "failed", "MMHotline restarted during the run.", "mmhotline_restarted"},
		{"software_review", "stopped", "MMHotline stopped the software review because a triage session needed the agent; it starts again once the session's run ends.", "stopped_for_a_triage_session"},
		{"commit_review", "stopped", "MMHotline stopped the commit review because a triage session needed the agent; it starts again once the session's run ends.", "stopped_for_a_triage_session"},
		{"sign_in_check", "stopped", "MMHotline stopped the sign-in check because a triage session needed the agent. The sign-in check runs again once the triage session's run ends.", "stopped_for_a_triage_session"},
		{"review_agent_sign_in_check", "stopped", "MMHotline stopped the sign-in check because a triage session needed the agent. The sign-in check runs again once the triage session's run ends.", "stopped_for_a_triage_session"},
		// MMHotline's stop left these words on a setup test or a playbook
		// review, as a lost sandbox connection does.
		{"setup_test", "failed", "The run ended without a result.", "unknown_from_before_the_upgrade"},
		{"review_agent_setup_test", "failed", "The run's session could not be saved.", "unknown_from_before_the_upgrade"},
		{"playbook_review", "failed", "The run's step could not be saved.", "unknown_from_before_the_upgrade"},
		{"software_review", "stopped", "Words no release wrote.", "unknown_from_before_the_upgrade"},
	}
	statements := []string{string(formatOne),
		`INSERT INTO team_info(name) VALUES('Released team')`,
		`INSERT INTO triage_agent_internet_access(id,internet_access,changed_at,changed_by) VALUES(1,'listed_hosts_only',1,'installation setup')`}
	for i, run := range runs {
		statements = append(statements, fmt.Sprintf(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by,failure_reason) VALUES('run-%d','%s','codex','api_key','%s',1,'test','%s')`,
			i, run.purpose, run.state, strings.ReplaceAll(run.reason, "'", "''")))
	}
	dir := t.TempDir()
	writeDatabase(t, dir, 1, statements...)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, run := range runs {
		var cause, reason string
		if err := store.db.QueryRow(`SELECT end_cause,failure_reason FROM triage_agent_runs WHERE id=?`, fmt.Sprintf("run-%d", i)).Scan(&cause, &reason); err != nil {
			t.Fatal(err)
		}
		if cause != run.want || reason != run.reason {
			t.Errorf("%s %s %q: end cause %q, reason %q; want %q with the reason kept", run.purpose, run.state, run.reason, cause, reason, run.want)
		}
	}
	if _, err := store.db.Exec(`UPDATE triage_agent_runs SET end_cause='agent' WHERE id='run-1'`); err == nil {
		t.Fatal("the upgraded table took an end cause outside the list")
	}
}
