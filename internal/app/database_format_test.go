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
