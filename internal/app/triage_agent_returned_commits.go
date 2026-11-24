package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"mmhotline/internal/systemsrepository"
)

// The commits a finished run returned are saved in MMHotline's state
// directory before MMHotline waits for the systems repository copy's lock to
// push them, so MMHotline's stop during that wait never loses them:
// docs/changing-mmhotline/design/triage-agent.md#a-finished-runs-commits-and-mmhotlines-stop.
// The next run in the systems repository handles them first, after a
// restart, so it starts from them.

// returnedAgentCommitsDirectoryName is the folder in MMHotline's state
// directory, such as /var/lib/mmhotline/triage_agent_returned_commits, with
// one file for each finished run whose commits MMHotline has not handled
// yet: <run ID>_<base commit ID>.bundle, the Git bundle the run's final
// event carried. The base is the commit the run started from, which the
// commits must descend from. A file goes once the outcome is recorded with
// the run in triage_agent_commits.
const returnedAgentCommitsDirectoryName = "triage_agent_returned_commits"

// returnedAgentCommitsFileName matches a saved bundle's name: a run ID and a
// SHA-1 or SHA-256 commit ID.
var returnedAgentCommitsFileName = regexp.MustCompile(`^([a-f0-9]{32})_([a-f0-9]{40}|[a-f0-9]{64})\.bundle$`)

// partialReturnedAgentCommitsPrefix starts the name of a bundle being
// written; it is renamed into place once whole, so a saved name always holds
// a whole bundle.
const partialReturnedAgentCommitsPrefix = ".partial-"

func (a *App) returnedAgentCommitsDirectory() string {
	return filepath.Join(a.Store.Dir, returnedAgentCommitsDirectoryName)
}

// saveReturnedAgentCommits writes a finished run's bundle to the disk, and
// returns its path once the bundle is there to stay.
func (a *App) saveReturnedAgentCommits(runID, base string, bundle []byte) (path string, err error) {
	directory := a.returnedAgentCommitsDirectory()
	if err = os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(directory, partialReturnedAgentCommitsPrefix+"*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(file.Name())
		}
	}()
	_, err = file.Write(bundle)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return "", err
	}
	path = filepath.Join(directory, runID+"_"+base+".bundle")
	if err = os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	// The rename is on the disk too, so an abrupt exit keeps the file.
	parent, err := os.Open(directory)
	if err != nil {
		return path, nil
	}
	_ = parent.Sync()
	_ = parent.Close()
	return path, nil
}

// receiveReturnedAgentCommits saves a finished run's commits, then handles
// them in MMHotline's copy until MMHotline stops or everyone is locked out.
// waits says MMHotline stopped or the lock-out began first: the commits stay
// saved for the next run in the systems repository, after the restart or the
// lock-out, and nothing is recorded yet. None of them reaches origin during
// a lock-out. saved is the saved file, "" when it could not be written; the
// commits are then handled all the same, and a stop or a lock-out during the
// wait records the failure.
func (a *App) receiveReturnedAgentCommits(runID, base string, bundle []byte) (saved string, commits systemsrepository.ReceivedAgentCommits, waits bool, err error) {
	ctx, stop := a.untilALockOut(a.mmhotlineStopping())
	defer stop()
	saved, saveErr := a.saveReturnedAgentCommits(runID, base, bundle)
	if saveErr != nil {
		fmt.Fprintf(os.Stderr, "mmhotline: could not save the triage agent's commits of run %s before handling them: %s\n", runID, saveErr)
	}
	if saved != "" && (ctx.Err() != nil || a.everyoneLockedOut()) {
		return saved, commits, true, nil
	}
	commits, err = systemsrepository.ReceiveAgentCommits(ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), bundle, base, a.trustedCommit(nil), a.triageAgentsOwnCommits(ctx), a.mayPushAgentCommits)
	if err != nil && saved != "" && (ctx.Err() != nil || errors.Is(err, errEveryoneLockedOut)) {
		return saved, systemsrepository.ReceivedAgentCommits{}, true, nil
	}
	return saved, commits, false, lockedOutErr(ctx, err)
}

// applySavedAgentCommits handles, oldest first, the commits that finished
// runs saved before MMHotline stopped, and records each outcome with its
// run, as at the run's end. It runs at the start of every run in the systems
// repository, while that run holds the one run, and returns when ctx ends;
// the commits left then wait for the next run.
func (a *App) applySavedAgentCommits(ctx context.Context) {
	directory := a.returnedAgentCommitsDirectory()
	entries, err := os.ReadDir(directory)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "mmhotline: could not read the triage agent's saved commits: %s\n", err)
		}
		return
	}
	type savedBundle struct {
		path, runID, base string
		savedAt           time.Time
	}
	var saved []savedBundle
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		// A write that an abrupt exit cut short; its run was cut short too.
		if strings.HasPrefix(entry.Name(), partialReturnedAgentCommitsPrefix) {
			_ = os.Remove(path)
			continue
		}
		match := returnedAgentCommitsFileName.FindStringSubmatch(entry.Name())
		info, err := entry.Info()
		if match == nil || err != nil {
			continue
		}
		saved = append(saved, savedBundle{path, match[1], match[2], info.ModTime()})
	}
	// Runs are one at a time, so each saved its commits after the last.
	slices.SortFunc(saved, func(x, y savedBundle) int {
		return cmp.Or(x.savedAt.Compare(y.savedAt), cmp.Compare(x.path, y.path))
	})
	for _, bundle := range saved {
		if ctx.Err() != nil {
			return
		}
		var runKnown, recorded bool
		if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_runs WHERE id=?1),EXISTS(SELECT 1 FROM triage_agent_commits WHERE run_id=?1 AND request_id='')`, bundle.runID).Scan(&runKnown, &recorded)
		}); err != nil {
			return
		}
		// Recorded already when an exit came between the record and the
		// file's removal; a run the database lacks has nowhere to record.
		if recorded || !runKnown {
			if !runKnown {
				fmt.Fprintf(os.Stderr, "mmhotline: removed the saved commits of run %s, which the database does not have\n", bundle.runID)
			}
			_ = os.Remove(bundle.path)
			continue
		}
		content, err := os.ReadFile(bundle.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mmhotline: could not read the saved commits of run %s: %s\n", bundle.runID, err)
			continue
		}
		commits, err := systemsrepository.ReceiveAgentCommits(ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), content, bundle.base, a.trustedCommit(nil), a.triageAgentsOwnCommits(ctx), a.mayPushAgentCommits)
		// Left saved for the next run: MMHotline stops, the run ends, or
		// everyone is locked out.
		if err != nil && (ctx.Err() != nil || errors.Is(err, errEveryoneLockedOut)) {
			return
		}
		outcome, reason := receivedAgentCommitsOutcome(commits, err)
		if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			return insertTriageAgentCommits(tx, bundle.runID, "", bundle.base, commits, outcome, reason)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "mmhotline: could not record the saved commits of run %s: %s\n", bundle.runID, err)
			continue
		}
		_ = os.Remove(bundle.path)
	}
}
