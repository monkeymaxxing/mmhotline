package systemsrepository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// copyLockPath is the shared flock contract for MMHotline and a person's
// helper: <copy>.lock, such as /var/lib/mmhotline/systems_repository.lock.
// Fetches, merges, pushes and MMX runs all hold this lock.
func copyLockPath(directory string) string { return directory + ".lock" }

// CopyLockWaitingPath is the marker MMHotline keeps while it waits for the
// copy's lock: <copy>.lock.waiting, such as
// /var/lib/mmhotline/systems_repository.lock.waiting. Another holder that
// keeps the lock for long, such as a restore drill, reads it at its safe
// points and gives way, because MMHotline's wait counts toward an MMX run's
// time limit. Its one line names the waiter and since when it waits.
func CopyLockWaitingPath(directory string) string { return copyLockPath(directory) + ".waiting" }

// copyLockRetry is how often MMHotline tries the lock again while an MMX run
// holds it; such runs take minutes, so a second is plenty.
const copyLockRetry = time.Second

// CopyLockOptions say who holds the copy's lock and report a wait for it.
type CopyLockOptions struct {
	// Holder is the line written into the lock file once the lock is taken,
	// so a person's helper that waits can say who holds it. Every holder
	// empties the file first, so a line never outlives its holder's turn.
	Holder string
	// WaitingChanged, when set, hears true when another holder makes the
	// caller wait, and false when the wait ends.
	WaitingChanged func(waiting bool)
}

// withCopyLock runs work while holding the copy's lock, waiting for it until
// ctx ends. A waiting MMX run or deploy is normal, not a failure.
func withCopyLock(ctx context.Context, directory string, options CopyLockOptions, work func() error) error {
	file, err := os.OpenFile(copyLockPath(directory), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open the systems repository lock %s: %w", copyLockPath(directory), err)
	}
	defer file.Close()
	waiting := false
	var waitingSince time.Time
	defer func() {
		if waiting && options.WaitingChanged != nil {
			options.WaitingChanged(false)
		}
		if !waitingSince.IsZero() {
			_ = os.Remove(CopyLockWaitingPath(directory))
		}
	}()
	waiter := options.Holder
	if waiter == "" {
		waiter = "mmhotline: a short hold of the systems repository copy, such as a fetch at a triage agent run's start"
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK {
			return fmt.Errorf("take the systems repository lock %s: %w", copyLockPath(directory), err)
		}
		if !waiting {
			waiting = true
			waitingSince = time.Now().UTC()
			if options.WaitingChanged != nil {
				options.WaitingChanged(true)
			}
		}
		// Written again at each try: another of MMHotline's waiters that
		// got the lock first removes the marker, and this wait still
		// counts.
		writeCopyLockWaitingMarker(directory, waiter+"; waiting since "+waitingSince.Format(time.RFC3339)+"\n")
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the systems repository lock %s, which another process holds: %w", copyLockPath(directory), ctx.Err())
		case <-time.After(copyLockRetry):
		}
	}
	if waiting && options.WaitingChanged != nil {
		options.WaitingChanged(false)
	}
	waiting = false
	if !waitingSince.IsZero() {
		_ = os.Remove(CopyLockWaitingPath(directory))
		waitingSince = time.Time{}
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("write the holder of the systems repository lock %s: %w", copyLockPath(directory), err)
	}
	if options.Holder != "" {
		if _, err := fmt.Fprintln(file, options.Holder); err != nil {
			return fmt.Errorf("write the holder of the systems repository lock %s: %w", copyLockPath(directory), err)
		}
	}
	// Closing the file, deferred above, releases the lock.
	return work()
}

// writeCopyLockWaitingMarker replaces the waiting marker by renaming a new
// file over it, so a holder reading it at a safe point reads a whole line,
// never the empty file that rewriting it in place would show for a moment.
// A failed write leaves no marker; the wait goes on without it.
func writeCopyLockWaitingMarker(directory, line string) {
	path := CopyLockWaitingPath(directory)
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return
	}
	_, err = file.WriteString(line)
	// Mode 0644: the line holds no secret, and the lock's other holders run
	// as other users.
	if err = errors.Join(err, file.Chmod(0o644), file.Close()); err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		_ = os.Remove(file.Name())
	}
}
