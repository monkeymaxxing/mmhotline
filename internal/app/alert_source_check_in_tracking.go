package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Check-in tracking in memory: MMHotline keeps each alert source's
// latest check-in in memory and writes the team's database only when
// something about the alert source changes. An alert source that keeps
// checking in causes no database write.
//
// The team's database is written for the alert source's first check-in,
// a check-in that says something new, check-ins stopping, and check-ins
// resuming.
// The times of the latest check-ins of an alert source that is still
// sending are in memory only, and are lost when MMHotline stops.
const (
	// alertSourceCheckInsCountAsStoppedAfter: when MMHotline has heard nothing from
	// an alert source for this long, it writes the time it last heard from the
	// alert source to the team's database and marks the alert source as
	// stopped. The Warning and SOS rules then run on the database. It is the
	// shortest Warning time a team can set, so no Warning waits for the mark.
	alertSourceCheckInsCountAsStoppedAfter = time.Minute
	// cappedMaintenanceWindowsRewrittenAfter: a maintenance window counts for
	// at most 7 days from the check-in that listed it. A window that ends
	// later is stored with that end, and stored again this long afterwards.
	cappedMaintenanceWindowsRewrittenAfter = 24 * time.Hour
)

func newAlertSourceCheckInTracking() *alertSourceCheckInTracking {
	return &alertSourceCheckInTracking{alertSources: map[string]*trackedAlertSource{}}
}

// alertSourceCheckInTracking is one team's alert sources in memory. Its lock is held while
// the team's check-in state changes in memory and in the team's
// database, so the two cannot disagree. Take the lock before a transaction on
// the team's database, never inside one.
type alertSourceCheckInTracking struct {
	mu sync.Mutex
	// current is false until the alert sources are read from the team's
	// database, and again after the database changed them.
	current      bool
	alertSources map[string]*trackedAlertSource
}

// storedAlertSourceCheckInState is what a team's database holds about one active
// alert source: who may sign for it and what its check-ins said.
type storedAlertSourceCheckInState struct {
	signingSecrets [][]byte
	// expectsCheckIns: a check-in has arrived at least once.
	expectsCheckIns bool
	// checkInsStopped: MMHotline heard nothing from the alert source for
	// alertSourceCheckInsCountAsStoppedAfter. Until check-ins resume, the
	// database holds the exact lastHeardAt and lastCheckInAt.
	checkInsStopped            bool
	lastHeardAt, lastCheckInAt time.Time
	// The newest check-in by its sent_at: when it was sent, which node
	// sent it, and its nodes in whole-node maintenance as stored JSON.
	newestSentAt                int64
	sentByNodeName              string
	nodesInWholeNodeMaintenance string
}

// trackedAlertSource is an alert source in memory. While its check-ins
// have not stopped, lastHeardAt and lastCheckInAt are newer here than in
// the team's database.
type trackedAlertSource struct {
	storedAlertSourceCheckInState
	// maintenanceWindowsCappedFrom is the time from which the stored
	// maintenance windows count for at most 7 days.
	maintenanceWindowsCappedFrom     time.Time
	minuteBucket, checkInsThisMinute int64
	// watchAgainAt: while the alert source is marked as stopped, when the
	// passing of time next changes something for it (its Warning, its SOS,
	// the end of planned downtime). Zero when only a check-in or a
	// change to the team's database changes anything.
	watchAgainAt time.Time
}

func readStoredAlertSourceCheckInState(ctx context.Context, tx *sql.Tx) (map[string]storedAlertSourceCheckInState, error) {
	stored := map[string]storedAlertSourceCheckInState{}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.last_heard_at,w.alert_source_id IS NOT NULL,COALESCE(w.check_ins_stopped,0),COALESCE(w.last_check_in_at,0),COALESCE(w.last_sent_at,0),COALESCE(w.sent_by_node_name,''),COALESCE(w.nodes_in_whole_node_maintenance,'') FROM sources s LEFT JOIN check_in_watch w ON w.alert_source_id=s.id WHERE s.active=1 AND s.kind='generic'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var s storedAlertSourceCheckInState
		var heard, lastCheckIn int64
		if err = rows.Scan(&id, &heard, &s.expectsCheckIns, &s.checkInsStopped, &lastCheckIn, &s.newestSentAt, &s.sentByNodeName, &s.nodesInWholeNodeMaintenance); err != nil {
			rows.Close()
			return nil, err
		}
		s.lastHeardAt, s.lastCheckInAt = deadlineTime(heard), deadlineTime(lastCheckIn)
		stored[id] = s
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT k.source_id,k.secret FROM signing_secrets k JOIN sources s ON s.id=k.source_id WHERE s.active=1 AND s.kind='generic'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var secret []byte
		if err = rows.Scan(&id, &secret); err != nil {
			rows.Close()
			return nil, err
		}
		s := stored[id]
		s.signingSecrets = append(s.signingSecrets, secret)
		stored[id] = s
	}
	return stored, errors.Join(rows.Err(), rows.Close())
}

// apply brings memory up to date with what was read from the team's database
// at readAt.
func (team *alertSourceCheckInTracking) apply(stored map[string]storedAlertSourceCheckInState, readAt time.Time) {
	for id := range team.alertSources {
		if _, ok := stored[id]; !ok {
			delete(team.alertSources, id)
		}
	}
	for id, state := range stored {
		source := team.alertSources[id]
		if source == nil {
			// Memory did not hold this alert source. If its check-ins have
			// not stopped, the times of the latest ones were in the memory of a
			// MMHotline that no longer runs, so its silence counts from now.
			source = &trackedAlertSource{}
			source.lastHeardAt, source.lastCheckInAt = readAt, readAt
			// If it is marked as stopped, a visit works out when to look next.
			source.watchAgainAt = readAt
			team.alertSources[id] = source
		}
		heard, lastCheckIn, sent := source.lastHeardAt, source.lastCheckInAt, source.newestSentAt
		source.storedAlertSourceCheckInState = state
		if !state.checkInsStopped {
			if heard.After(source.lastHeardAt) {
				source.lastHeardAt = heard
			}
			if lastCheckIn.After(source.lastCheckInAt) {
				source.lastCheckInAt = lastCheckIn
			}
		}
		if sent > source.newestSentAt {
			source.newestSentAt = sent
		}
	}
}

// lockTrackedTeam returns the team's alert sources with the team's lock held;
// the caller unlocks. It reads them from the team's database first when
// memory does not hold them, or when changed says that the database changed
// them.
func (a *App) lockTrackedTeam(ctx context.Context, changed bool) (*alertSourceCheckInTracking, error) {
	team := a.alertSourceCheckInTracking
	team.mu.Lock()
	if changed {
		team.current = false
	}
	if team.current {
		return team, nil
	}
	var stored map[string]storedAlertSourceCheckInState
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		stored, err = readStoredAlertSourceCheckInState(ctx, tx)
		return err
	})
	if err != nil {
		team.mu.Unlock()
		return nil, err
	}
	team.apply(stored, a.Now().UTC())
	team.current = true
	return team, nil
}

// alertSourcesChanged brings memory up to date after a committed change to
// the team's alert sources or signing secrets. If the team's database cannot
// be read now, the next use of the team reads it.
func (a *App) alertSourcesChanged(ctx context.Context) {
	if team, err := a.lockTrackedTeam(context.WithoutCancel(ctx), true); err == nil {
		team.mu.Unlock()
	}
}

// heardFromAlertSource records in memory that a signed alert from the alert
// source was accepted; the alert's transaction wrote the same to the team's
// database.
func (a *App) heardFromAlertSource(sourceID string, at time.Time) {
	team := a.alertSourceCheckInTracking
	team.mu.Lock()
	defer team.mu.Unlock()
	if source := team.alertSources[sourceID]; source != nil && at.After(source.lastHeardAt) {
		source.lastHeardAt = at
	}
}

// teamHasWork reports whether the team's alert sources need a visit: silence
// reached the stopped mark, Warning or SOS time, or end of planned downtime.
func (team *alertSourceCheckInTracking) teamHasWork(now time.Time) bool {
	// Look next round if a check-in or a visit holds the lock.
	if !team.mu.TryLock() {
		return false
	}
	defer team.mu.Unlock()
	return team.hasWork(now)
}

func (team *alertSourceCheckInTracking) hasWork(now time.Time) bool {
	if !team.current {
		// The team's database changed its alert sources and could not be
		// read. A visit reads it.
		return true
	}
	for _, source := range team.alertSources {
		if source.checkInsStopped {
			if !source.watchAgainAt.IsZero() && !now.Before(source.watchAgainAt) {
				return true
			}
		} else if source.expectsCheckIns && now.Sub(source.lastHeardAt) >= alertSourceCheckInsCountAsStoppedAfter {
			return true
		}
	}
	return false
}

// maintenanceWindowsJSON encodes a check-in's nodes in whole-node
// maintenance for the team's database. A maintenance window counts as planned
// downtime for at most 7 days from countedFrom, like planned downtime set by
// hand.
func maintenanceWindowsJSON(nodes []NodeInMaintenance, countedFrom time.Time) (string, error) {
	capped := append([]NodeInMaintenance(nil), nodes...)
	limit := countedFrom.Add(maxPlannedDowntimeAhead)
	for i := range capped {
		if capped[i].MaintenanceUntil.After(limit) {
			capped[i].MaintenanceUntil = limit
		}
	}
	raw, err := json.Marshal(capped)
	return string(raw), err
}
