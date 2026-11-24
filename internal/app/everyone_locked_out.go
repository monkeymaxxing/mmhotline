package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// While the lock-out file exists, MMHotline runs no agent and no MMX and
// trusts no commit, and people get every alert at once:
// docs/changing-mmhotline/design/operation.md#everyone-locked-out.

// everyoneLockedOutFile is the lock-out file a team's break-in procedure
// creates as root. The path is fixed, not a setting: the procedure that
// creates it and the systemd units that read it must agree on one path, and
// the settings file cannot hold it, because a lock-out must not depend on
// rewriting the settings file.
const everyoneLockedOutFile = "/etc/mmhotline/everyone-locked-out"

// everyoneLockedOutCannotRun is why the triage agent cannot run during a
// lock-out, in the words incident histories and the session's end show.
const everyoneLockedOutCannotRun = "Everyone is locked out: the file " + everyoneLockedOutFile + " exists."

// everyoneLockedOutReason is the refusal of an agent run, an MMX run, and
// the commit check during a lock-out.
const everyoneLockedOutReason = "everyone is locked out: the file " + everyoneLockedOutFile + " exists, so MMHotline runs no agent and no MMX and trusts no commit until a person removes the file"

// errEveryoneLockedOut is a conflict, so the command and pages show its words.
var errEveryoneLockedOut = fmt.Errorf("%w: %s", ErrConflict, everyoneLockedOutReason)

// lockOutWatch remembers what the last look at the lock-out file found.
type lockOutWatch struct {
	mu sync.Mutex
	// file is everyoneLockedOutFile; a test points it at a file of its own.
	file string
	// lockedOut is what the last look found.
	lockedOut bool
	// notLockedOut ends at the first look that finds the file. Work that
	// must stop at a lock-out, an MMX run, ends with it. A look that finds
	// the file gone after a lock-out makes a new one.
	notLockedOut context.Context
	end          context.CancelFunc
}

func newLockOutWatch() *lockOutWatch {
	w := &lockOutWatch{file: everyoneLockedOutFile}
	w.notLockedOut, w.end = context.WithCancel(context.Background())
	return w
}

// look reads whether the file exists now, and says whether that changed
// since the last look. A file MMHotline cannot check counts as there: a
// wrong refusal costs an agent run, and a wrong pass during a break-in
// costs much more.
func (w *lockOutWatch) look() (lockedOut, changed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := os.Stat(w.file)
	lockedOut = !errors.Is(err, fs.ErrNotExist)
	changed = lockedOut != w.lockedOut
	w.lockedOut = lockedOut
	switch {
	case lockedOut:
		w.end()
	case changed:
		w.notLockedOut, w.end = context.WithCancel(context.Background())
	}
	return lockedOut, changed
}

// lockedOutAtTheLastLook is what the last look found, without a new look.
func (w *lockOutWatch) lockedOutAtTheLastLook() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lockedOut
}

// endsAtALockOut ends when a look finds the lock-out file.
func (w *lockOutWatch) endsAtALockOut() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.notLockedOut
}

// untilALockOut is ctx, which also ends when a look finds the lock-out file,
// with errEveryoneLockedOut as its cause (lockedOutErr). Work that must not
// finish during a lock-out, such as trust-through and a finished run's push,
// runs under it. It ends at once when the last look found the file.
func (a *App) untilALockOut(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	stopWatching := context.AfterFunc(a.lockOut.endsAtALockOut(), func() { cancel(errEveryoneLockedOut) })
	return ctx, func() { stopWatching(); cancel(context.Canceled) }
}

// lockedOutErr is errEveryoneLockedOut when a lock-out ended ctx, which
// untilALockOut made, and err otherwise: the cause, not "context canceled".
func lockedOutErr(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), errEveryoneLockedOut) {
		return errEveryoneLockedOut
	}
	return err
}

// mayPushAgentCommits is systemsrepository.MayPush: during a lock-out no
// commit of the agent's reaches origin, even one a run returned before the
// lock-out began.
func (a *App) mayPushAgentCommits() error {
	if a.everyoneLockedOut() {
		return errEveryoneLockedOut
	}
	return nil
}

// everyoneLockedOut looks at the lock-out file and acts on what it finds.
// The lock-out watch (watchForALockOut) calls it every second, apart from
// the background visits, and every agent run, MMX run, and commit check
// calls it before it starts, so a lock-out takes effect
// within a second without a restart. When the file is there, it cuts every
// agent run at once and ends every MMX run, trust-through, and finished
// run's push (endsAtALockOut). When the file
// appeared or went, it brings a background visit, whose turn for triage
// sessions wakes the SOS alerts that wait for the agent and releases the
// held warns, or lets the agent run again. Callers hold no lock of the
// triage agent's runtime or the team's deadline.
func (a *App) everyoneLockedOut() bool {
	lockedOut, changed := a.lockOut.look()
	a.actOnALockOutLook(lockedOut, changed)
	return lockedOut
}

// actOnALockOutLook does what everyoneLockedOut does for what a look found.
func (a *App) actOnALockOutLook(lockedOut, changed bool) {
	if lockedOut {
		a.cutAgentRunsForALockOut()
	}
	if changed {
		a.teamDeadline.changed()
	}
}

// lockOutLookInATransaction is everyoneLockedOut for the last moment before
// a database transaction trusts a commit. The transaction holds the one
// database connection, so the look there takes only the watch's own lock;
// the caller passes the look to actOnALockOutLook after the transaction.
type lockOutLookInATransaction struct{ lockedOut, changed bool }

func (a *App) lookForALockOutInATransaction(look *lockOutLookInATransaction) bool {
	look.lockedOut, look.changed = a.lockOut.look()
	return look.lockedOut
}

// cutAgentRunsForALockOut ends the triage run and the review at once, as
// MMHotline's own stop does: closing a run's connection makes the sandbox
// kill the coding agent at once and end, so systemd stops the rest, and the
// run returns no commits. A stop after the agent's current step would let
// the agent act while everyone is locked out.
func (a *App) cutAgentRunsForALockOut() {
	a.triageAgent.mu.Lock()
	defer a.triageAgent.mu.Unlock()
	for _, run := range []*triageAgentActiveRun{a.triageAgent.activeRun, a.triageAgent.activeReview} {
		if run == nil || run.ending {
			continue
		}
		run.cutForALockOut = true
		run.cancel()
		if run.connection != nil {
			run.connection.Close()
		}
	}
}
