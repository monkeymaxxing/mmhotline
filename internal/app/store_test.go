package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreCountsOnlyCommittedChanges(t *testing.T) {
	a := foundationApp(t)
	changes := 0
	a.Store.teamChanged = func() { changes++ }
	ctx := context.Background()
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var name string
		return tx.QueryRow(`SELECT name FROM team_info`).Scan(&name)
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("rollback")
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE team_info SET name='unsaved'`); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if changes != 0 {
		t.Fatalf("reads and rollback notified %d times", changes)
	}
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRow(`SELECT name FROM team_info`).Scan(&name); err != nil {
			return err
		}
		if name != "Fixture team" {
			t.Fatalf("rollback saved %q", name)
		}
		_, err := tx.Exec(`UPDATE team_info SET name='Saved team'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Fatalf("committed change notified %d times", changes)
	}
}

func TestOpenRequiresExistingRegularDatabase(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "symlink", "missing_schema", "invalid_database"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mmhotline.db")
			switch kind {
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "missing_schema", "invalid_database":
				var data []byte
				if kind == "invalid_database" {
					data = []byte("not a database")
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			store, err := Open(dir)
			if err == nil {
				store.Close()
				t.Fatal("opened invalid state")
			}
			if !strings.Contains(err.Error(), "run setup first") {
				t.Fatal(err)
			}
			if kind == "missing" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("open created missing database: %v", err)
				}
			}
		})
	}
}

func TestStoreDatabaseSettings(t *testing.T) {
	a := foundationApp(t)
	if a.Store.db.Stats().MaxOpenConnections != 1 {
		t.Fatal("database permits multiple connections")
	}
	if err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		for pragma, expected := range map[string]string{"journal_mode": "wal", "synchronous": "2", "busy_timeout": "5000", "foreign_keys": "1"} {
			var actual string
			if err := tx.QueryRow(`PRAGMA ` + pragma).Scan(&actual); err != nil {
				return err
			}
			if actual != expected {
				t.Errorf("%s = %q, want %q", pragma, actual, expected)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
