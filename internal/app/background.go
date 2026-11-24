package app

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// RunBackground is the background scheduler: it sends notifications, moves
// incidents to their next step, and alerts about missing check-ins.
// Every second it visits the installation's team if background work is due
// (team_deadlines.go), with one delivery attempt and at most one due receipt action per visit.
// Shutdown drains active work before the caller closes the stores;
// interrupted sends recover as uncertain. Custom schedule code has its own
// loop, so its sandbox runs never delay incident work.
func (a *App) RunBackground(ctx context.Context, report func(error)) {
	var customSchedules sync.WaitGroup
	customSchedules.Go(func() { a.RunCustomScheduleBackground(ctx, report) })
	defer customSchedules.Wait()
	// The lock-out file has its own watch, so a slow visit, such as one
	// that waits 30 seconds for a mail server, never delays a lock-out.
	var lockOutWatch sync.WaitGroup
	lockOutWatch.Go(func() { a.watchForALockOut(ctx, report) })
	defer lockOutWatch.Wait()
	// A triage session's run, a software review, or a sign-in check that is
	// starting stops with ctx; it must not outlive the stores.
	defer a.triageAgent.backgroundRunStarts.Wait()
	// An approved playbook run records its result before the stores close.
	defer a.triageAgent.approvedPlaybookRuns.Wait()
	for ctx.Err() == nil {
		a.backgroundRound(ctx, report)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// backgroundRound visits the installation's team when work is due.
// It returns whether the team was visited.
func (a *App) backgroundRound(ctx context.Context, report func(error)) bool {
	if ctx.Err() != nil {
		return false
	}
	// The lock-out watch looks every second as well; this look makes the
	// visit act on the file as it is now.
	a.everyoneLockedOut()
	visited := a.teamNeedsVisit(a.Now())
	if visited {
		a.visitTeam(ctx, report)
	}
	a.teamDeadline.roundEnded(a.Now())
	return visited
}

// watchForALockOut looks at the lock-out file every second until ctx ends,
// on its own, apart from the visits: a lock-out cuts the agent's runs, ends
// MMX runs, and brings the visit that tells people within a second, however
// long the current visit takes.
func (a *App) watchForALockOut(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		reportPanic(report, "the look at the lock-out file", func() { a.everyoneLockedOut() })
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reportPanic runs one round of a background loop and turns a panic in it
// into a reported error with the stack, as a returned error is reported.
// Without it, one panic stops all of mmhotline serve, intake included, and a
// panic that repeats at every start, such as one caused by a stored row,
// keeps it down. It returns whether work panicked.
func reportPanic(report func(error), what string, work func()) (panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			report(fmt.Errorf("%s panicked: %v\n%s", what, recovered, debug.Stack()))
			panicked = true
		}
	}()
	work()
	return false
}

// teamBackgroundWork does one team's incident, check-in, and delivery
// work, and reports whether any of it failed.
func (a *App) teamBackgroundWork(ctx context.Context, report func(error)) bool {
	failed := false
	fail := func(err error) {
		if ctx.Err() == nil {
			report(err)
		}
		failed = true
	}
	if _, err := a.ProcessPhoneCall(ctx); err != nil {
		fail(fmt.Errorf("check phone call: %w", err))
	}
	if _, err := a.ProcessPushoverReceipt(ctx); err != nil {
		fail(fmt.Errorf("check Pushover receipt: %w", err))
	}
	if err := a.ReconcileIncidents(ctx); err != nil {
		fail(fmt.Errorf("reconcile incidents: %w", err))
	}
	// Before the session's turn: while the run limit holds, the session
	// starts no run, and what waits for one goes straight to people.
	if err := a.DriveTriageAgentRunLimit(ctx); err != nil {
		fail(fmt.Errorf("drive the triage agent's run limit: %w", err))
	}
	if err := a.DriveTriageSession(ctx); err != nil {
		fail(fmt.Errorf("drive the triage session: %w", err))
	}
	// After the incidents' and the session's turns, which decide what the
	// triage agent's holds release, and outside their transactions.
	if err := a.DeliverReleasedHeldAlerts(ctx); err != nil {
		fail(fmt.Errorf("deliver what the triage agent's holds released: %w", err))
	}
	// After the session's turn, so an alert's session takes the one run
	// first; before the software review, as a waiting commit holds every
	// MMX run.
	if err := a.DriveCommitReview(ctx); err != nil {
		fail(fmt.Errorf("start the commit review: %w", err))
	}
	if err := a.DriveSoftwareReview(ctx); err != nil {
		fail(fmt.Errorf("start the software review: %w", err))
	}
	// After the review's turn: a review that succeeds is a sign-in check too.
	if err := a.DriveCodingAgentCredentials(ctx); err != nil {
		fail(fmt.Errorf("check the coding agents' credentials: %w", err))
	}
	if err := a.DriveApprovedPlaybookRuns(ctx); err != nil {
		fail(fmt.Errorf("run the approved playbooks: %w", err))
	}
	if err := a.WatchAlertSourceCheckIns(ctx); err != nil {
		fail(fmt.Errorf("watch check-ins: %w", err))
	}
	if err := a.AnnounceContactCardChange(ctx); err != nil {
		fail(fmt.Errorf("announce the contact card: %w", err))
	}
	// A full queue can prevent a new reconciliation notice. Existing
	// independently revalidated work must still drain.
	if _, err := a.DispatchOne(ctx); err != nil {
		fail(fmt.Errorf("dispatch notifications: %w", err))
	}
	return failed
}
