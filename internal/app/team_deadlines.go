package app

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// Visit even an idle team this often to bound the effect of a wrong deadline.
const maximumTimeBetweenTeamVisits = 10 * time.Minute

// teamDeadline is the installation's one team's background state. The database
// is the durable record; a new App rebuilds these facts after a restart.
type teamDeadline struct {
	mu sync.Mutex
	// version counts committed transactions that changed the team's database.
	// Visits and fact reads record their starting version so concurrent changes
	// remain due for the next round.
	version, visitedVersion, factsVersion uint64
	facts                                 teamDeadlineFacts
	firstVisitDone                        bool
	visitFailed                           bool
	lastVisitedAt                         time.Time
	unfinishedDeliveriesTakenOver         bool
	// upToDateAt is when a background round last ended with no failed
	// visit: all due work was done. A healthy idle team is visited only
	// every 10 minutes, so the last visit's time cannot show that the loop
	// still runs; the end of a round, every second, can.
	upToDateAt time.Time
}

// teamDeadlineFacts is what the background loops need without opening the
// team's database.
type teamDeadlineFacts struct {
	// Zero means no work is due at a known time.
	nextDeadline           time.Time
	usesCustomScheduleCode bool
}

func (d *teamDeadline) hasTakenOverUnfinishedDeliveries() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.unfinishedDeliveriesTakenOver
}

// tookOverUnfinishedDeliveries records that the takeover is committed.
func (d *teamDeadline) tookOverUnfinishedDeliveries() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unfinishedDeliveriesTakenOver = true
}

// roundEnded records the end of a background round. A round whose visit
// failed leaves the team's work not up to date.
func (d *teamDeadline) roundEnded(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.visitFailed {
		d.upToDateAt = now
	}
}

// backgroundUpToDateAt returns when a background round last ended with all
// due work done. Before any round has ended, it counts from its first call,
// so a background loop whose first visit never succeeds becomes stale too.
func (d *teamDeadline) backgroundUpToDateAt(now time.Time) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.upToDateAt.IsZero() {
		d.upToDateAt = now
	}
	return d.upToDateAt
}

// changed is called after a committed transaction changed the team's database.
func (d *teamDeadline) changed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.version++
}

// earliest returns the earliest of the times that are set.
func earliest(times ...time.Time) time.Time {
	var first time.Time
	for _, t := range times {
		if !t.IsZero() && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first
}

// readTeamDeadlineFacts reads a team's facts from its database. Each part is
// owned by the code that does the work it describes.
func (a *App) readTeamDeadlineFacts(ctx context.Context, now time.Time) (teamDeadlineFacts, error) {
	var facts teamDeadlineFacts
	kinds := slices.Sorted(maps.Keys(a.configuredContactMethodTypes()))
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		schedules, err := ReadSchedules(ctx, tx)
		if err != nil {
			return err
		}
		facts.usesCustomScheduleCode = schedules.ActiveKind == customScheduleKind
		incident, err := nextIncidentDeadline(ctx, tx, now)
		if err != nil {
			return err
		}
		delivery, err := nextDeliveryDeadline(ctx, tx, kinds)
		if err != nil {
			return err
		}
		coverage, err := nextCoverageDeadline(ctx, tx, schedules, now)
		if err != nil {
			return err
		}
		receipt, err := nextPushoverReceiptDeadline(ctx, tx, now)
		if err != nil {
			return err
		}
		call, err := a.nextPhoneCallDeadline(ctx, tx, now)
		if err != nil {
			return err
		}
		triageSessionEnd, err := triageSessionNextEnd(ctx, tx)
		if err != nil {
			return err
		}
		heldWarn, err := nextHeldWarnDeadline(ctx, tx)
		if err != nil {
			return err
		}
		softwareReview, err := a.nextSoftwareReviewDeadline(ctx, tx)
		if err != nil {
			return err
		}
		warnedRun, err := nextWarnedRunTime(ctx, tx)
		if err != nil {
			return err
		}
		credentials, err := a.nextCodingAgentCredentialDeadline(ctx, tx)
		if err != nil {
			return err
		}
		runLimit, err := nextRunLimitDeadline(ctx, tx, now)
		if err != nil {
			return err
		}
		facts.nextDeadline = earliest(incident, delivery, coverage, receipt, call, triageSessionEnd, heldWarn, softwareReview, warnedRun, credentials, runLimit)
		return nil
	})
	return facts, err
}

// refreshTeamFacts reads the team's facts. Unless always is set, it does
// nothing when the facts are up to date with the team's database.
func (a *App) refreshTeamFacts(ctx context.Context, always bool) error {
	d := a.teamDeadline
	d.mu.Lock()
	if !always && d.factsVersion == d.version {
		d.mu.Unlock()
		return nil
	}
	version := d.version
	d.mu.Unlock()
	facts, err := a.readTeamDeadlineFacts(ctx, a.Now().UTC())
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// A concurrent commit leaves these facts marked as out of date.
	if version >= d.factsVersion {
		d.facts, d.factsVersion = facts, version
	}
	return nil
}

// usesCustomScheduleCode refreshes changed facts for the independent custom
// schedule loop. After a restart, the first visit loads unchanged facts.
func (a *App) usesCustomScheduleCode(ctx context.Context) (bool, error) {
	err := a.refreshTeamFacts(ctx, false)
	d := a.teamDeadline
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.facts.usesCustomScheduleCode, err
}

func (a *App) teamNeedsVisit(now time.Time) bool {
	checkInWork := a.alertSourceCheckInTracking.teamHasWork(now)
	d := a.teamDeadline
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.firstVisitDone || d.visitFailed || d.visitedVersion != d.version ||
		(!d.facts.nextDeadline.IsZero() && !d.facts.nextDeadline.After(now)) ||
		checkInWork || now.Sub(d.lastVisitedAt) >= maximumTimeBetweenTeamVisits
}

// visitTeam does the team's background work and then reads its facts again.
func (a *App) visitTeam(ctx context.Context, report func(error)) {
	d := a.teamDeadline
	d.mu.Lock()
	version := d.version
	d.mu.Unlock()

	failed := false
	panicked := reportPanic(report, "the background visit", func() {
		failed = a.teamBackgroundWork(ctx, report)
		// Always read again: an elapsed deadline must give way to the next one
		// even when the visit changed nothing.
		if err := a.refreshTeamFacts(ctx, true); err != nil {
			if ctx.Err() == nil {
				report(fmt.Errorf("read the next team deadline: %w", err))
			}
			failed = true
		}
	})
	failed = failed || panicked
	d.mu.Lock()
	defer d.mu.Unlock()
	d.visitFailed = failed
	if failed {
		// Retry next round, even if the refreshed deadline is in the future.
		return
	}
	d.firstVisitDone = true
	d.visitedVersion = version
	d.lastVisitedAt = a.Now()
}
