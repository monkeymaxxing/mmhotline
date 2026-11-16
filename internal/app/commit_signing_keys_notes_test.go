package app

import (
	"context"
	"database/sql"
	"testing"
)

// A team without the triage agent has no MMX runs or commit checks, so the
// commit signing keys part shows no note about them.
func TestCommitSigningKeysPartHasNoNoteWithoutTheTriageAgent(t *testing.T) {
	dir := t.TempDir()
	if err := Setup(context.Background(), dir, "Fixture team"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		notes, err := commitSigningKeysSettingsNotes(context.Background(), tx)
		if len(notes) != 0 {
			t.Fatalf("notes without the triage agent: %q", notes)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
