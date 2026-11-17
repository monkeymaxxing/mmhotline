package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Alert source check-ins: a monitoring tool checks in with MMHotline every 30
// seconds, a signed message that says it still runs. When its check-ins stop,
// MMHotline alerts, because a stopped monitoring tool cannot report its own
// failure. MMHotline tracks them in memory (alert_source_check_in_tracking.go).
// They are not the MMHotline-down alarm's check-ins, which MMHotline itself sends
// (down_alarm.go).
const (
	defaultWarnAfterNoCheckInFor    = 2 * time.Minute
	defaultSOSAfterNoCheckInFor     = 5 * time.Minute
	maxAlertSourceCheckInsPerMinute = 12
	maxNodesInWholeNodeMaintenance  = 1000
	// sosAfterPlannedDowntimeEndsWithoutCheckIn: when planned downtime
	// ends and check-ins have not resumed, the SOS comes this much later,
	// with no Warning first.
	sosAfterPlannedDowntimeEndsWithoutCheckIn = time.Minute
)

const alertSourceCheckInSchema = `
CREATE TABLE check_in_watch(
 alert_source_id TEXT PRIMARY KEY REFERENCES sources(id),
 last_check_in_at INTEGER NOT NULL,
 last_sent_at INTEGER NOT NULL DEFAULT 0,
 sent_by_node_name TEXT NOT NULL,
 nodes_in_whole_node_maintenance TEXT NOT NULL,
 check_ins_stopped INTEGER NOT NULL DEFAULT 0 CHECK(check_ins_stopped IN (0,1)),
 alerted TEXT NOT NULL DEFAULT '' CHECK(alerted IN ('','warning','sos')),
 alerted_at INTEGER NOT NULL DEFAULT 0
);
`

// NodeInMaintenance is one monitored node in a whole-node maintenance window, as
// listed in a check-in.
type NodeInMaintenance struct {
	NodeName         string    `json:"node_name"`
	MaintenanceUntil time.Time `json:"maintenance_until"`
	Reason           string    `json:"reason,omitempty"`
	StartedBy        string    `json:"started_by,omitempty"`
}

type alertSourceCheckIn struct {
	SentAt                      time.Time           `json:"sent_at"`
	SentByNodeName              string              `json:"sent_by_node_name"`
	NodesInWholeNodeMaintenance []NodeInMaintenance `json:"nodes_in_whole_node_maintenance"`
}

// decodeAlertSourceCheckIn reads a check-in strictly: unknown fields are
// refused, so a body signed for alert intake is rejected here and the other
// way round.
func decodeAlertSourceCheckIn(body []byte) (alertSourceCheckIn, error) {
	var m alertSourceCheckIn
	if len(body) > MaxAlertBody || !utf8.Valid(body) {
		return alertSourceCheckIn{}, fmt.Errorf("%w: check-in request body must be UTF-8 JSON within the request size limit", ErrInvalid)
	}
	// encoding/json matches field names without regard to case; a
	// check-in must use the exact names.
	var names map[string]json.RawMessage
	if json.Unmarshal(body, &names) != nil {
		return alertSourceCheckIn{}, fmt.Errorf("%w: a check-in is a JSON object", ErrInvalid)
	}
	for name := range names {
		if name != "sent_at" && name != "sent_by_node_name" && name != "nodes_in_whole_node_maintenance" {
			return alertSourceCheckIn{}, fmt.Errorf("%w: unknown field %q", ErrInvalid, name)
		}
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return alertSourceCheckIn{}, fmt.Errorf("%w: a check-in is {\"sent_at\": ..., \"sent_by_node_name\": ..., \"nodes_in_whole_node_maintenance\": [...]}", ErrInvalid)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return alertSourceCheckIn{}, fmt.Errorf("%w: one JSON object required", ErrInvalid)
	}
	if m.SentAt.IsZero() || m.SentByNodeName == "" || len(m.SentByNodeName) > 256 {
		return alertSourceCheckIn{}, fmt.Errorf("%w: sent_at and sent_by_node_name are required", ErrInvalid)
	}
	if len(m.NodesInWholeNodeMaintenance) > maxNodesInWholeNodeMaintenance {
		return alertSourceCheckIn{}, fmt.Errorf("%w: at most %d nodes in nodes_in_whole_node_maintenance", ErrInvalid, maxNodesInWholeNodeMaintenance)
	}
	for _, n := range m.NodesInWholeNodeMaintenance {
		if n.NodeName == "" || len(n.NodeName) > 256 || n.MaintenanceUntil.IsZero() || len(n.Reason) > 256 || len(n.StartedBy) > 256 {
			return alertSourceCheckIn{}, fmt.Errorf("%w: each node in maintenance needs node_name and maintenance_until", ErrInvalid)
		}
	}
	return m, nil
}

// ReceiveAlertSourceCheckIn records one signed check-in. It returns the time
// MMHotline received it and the signing secret that verified the request, to
// sign the receipt.
func (a *App) ReceiveAlertSourceCheckIn(ctx context.Context, sourceID, signature string, body []byte) (time.Time, []byte, error) {
	now := a.Now().UTC()
	if !alertSourceIDPattern.MatchString(sourceID) {
		return now, nil, ErrUnauthorized
	}
	receiptKey, err := a.trackAlertSourceCheckIn(ctx, sourceID, signature, body, now)
	if errors.Is(err, ErrNotFound) {
		err = ErrUnauthorized
	}
	if err != nil {
		if reason := rejectionReason(err); reason != "" {
			a.rejected.add(sourceID, reason, now)
		}
	}
	return now, receiptKey, err
}

// trackAlertSourceCheckIn checks one check-in against the alert source in
// memory and records it there. It writes the team's database only when the
// check-in changes something about the alert source.
func (a *App) trackAlertSourceCheckIn(ctx context.Context, sourceID, signature string, body []byte, now time.Time) ([]byte, error) {
	team, err := a.lockTrackedTeam(ctx, false)
	if err != nil {
		return nil, err
	}
	defer team.mu.Unlock()
	source := team.alertSources[sourceID]
	parsed, ok := parseSenderSignature(signature)
	if source == nil || !ok {
		return nil, ErrUnauthorized
	}
	_, receiptKey, err := parsed.verify(body, source.signingSecrets, now)
	if err != nil {
		return receiptKey, err
	}
	m, err := decodeAlertSourceCheckIn(body)
	if err != nil {
		return receiptKey, err
	}
	if minute := now.Unix() / 60; source.minuteBucket != minute {
		source.minuteBucket, source.checkInsThisMinute = minute, 0
	}
	if source.checkInsThisMinute >= maxAlertSourceCheckInsPerMinute {
		return receiptKey, errTooManyAlerts
	}
	// Only a check-in newer than the newest one received, by its sent_at,
	// counts. An older or equal one, arriving late or resent as it was, which
	// its signature allows for 5 minutes, never replaces what the newest one
	// said, and never delays or ends an alarm about silence.
	if source.expectsCheckIns && m.SentAt.Unix() <= source.newestSentAt {
		source.checkInsThisMinute++
		return receiptKey, nil
	}
	cappedFrom := now
	if now.Sub(source.maintenanceWindowsCappedFrom) < cappedMaintenanceWindowsRewrittenAfter {
		cappedFrom = source.maintenanceWindowsCappedFrom
	}
	maintenance, err := maintenanceWindowsJSON(m.NodesInWholeNodeMaintenance, cappedFrom)
	if err != nil {
		return receiptKey, err
	}
	saysSomethingNew := m.SentByNodeName != source.sentByNodeName || maintenance != source.nodesInWholeNodeMaintenance
	if !source.expectsCheckIns || source.checkInsStopped || saysSomethingNew {
		sentAt, sentBy := m.SentAt.Unix(), m.SentByNodeName
		if maintenance, err = maintenanceWindowsJSON(m.NodesInWholeNodeMaintenance, now); err != nil {
			return receiptKey, err
		}
		resumed := source.checkInsStopped
		var stored map[string]storedAlertSourceCheckInState
		err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			// Memory verified the signature. The database decides, because the
			// alert source may have been disabled, or the signing secret
			// revoked, a moment ago.
			if _, _, err := authenticateSource(ctx, tx, sourceID, signature, body, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO check_in_watch(alert_source_id,last_check_in_at,last_sent_at,sent_by_node_name,nodes_in_whole_node_maintenance) VALUES(?,?,?,?,?) ON CONFLICT(alert_source_id) DO UPDATE SET last_check_in_at=excluded.last_check_in_at,last_sent_at=excluded.last_sent_at,sent_by_node_name=excluded.sent_by_node_name,nodes_in_whole_node_maintenance=excluded.nodes_in_whole_node_maintenance,check_ins_stopped=0`, sourceID, now.Unix(), sentAt, sentBy, maintenance); err != nil {
				return err
			}
			if err := heardFromSource(ctx, tx, sourceID, now); err != nil {
				return err
			}
			if resumed {
				// Ends a Warning or SOS that went out while check-ins had
				// stopped.
				if _, err := watchOneAlertSourceCheckIns(ctx, tx, sourceID, now); err != nil {
					return err
				}
			}
			var err error
			stored, err = readStoredAlertSourceCheckInState(ctx, tx)
			return err
		})
		if err != nil {
			return receiptKey, err
		}
		team.apply(stored, now)
		source.maintenanceWindowsCappedFrom = now
	}
	source.lastHeardAt, source.lastCheckInAt = now, now
	source.newestSentAt = m.SentAt.Unix()
	source.checkInsThisMinute++
	return receiptKey, nil
}

// heardFromSource records in the team's database that a signed request
// arrived from the alert source: a check-in or an alert both count. It
// never moves the time back.
func heardFromSource(ctx context.Context, tx *sql.Tx, sourceID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE sources SET last_heard_at=? WHERE id=? AND last_heard_at<?`, at.Unix(), sourceID, at.Unix())
	return err
}

// AlertSourceCheckInStatus is what the alert source page shows.
type AlertSourceCheckInStatus struct {
	Expected                          bool // a check-in has arrived at least once
	LastCheckInAt, LastHeardAt        time.Time
	SentByNodeName, SenderDescription string
	WarnAfterNoCheckInFor             time.Duration // 0 means off
	SOSAfterNoCheckInFor              time.Duration
	NodesInWholeNodeMaintenance       []NodeInMaintenance
	PlannedDowntime                   *PlannedDowntimeView
}

// senderDescription names the sender in titles and pages by its alert
// source's name, quoted, because a team names its alert sources freely.
func senderDescription(sourceName string) string {
	return "alert source “" + sourceName + "”"
}

func readAlertSourceCheckInStatus(ctx context.Context, tx *sql.Tx, source Source, now time.Time) (AlertSourceCheckInStatus, error) {
	var s AlertSourceCheckInStatus
	var warning, sos, heard int64
	if err := tx.QueryRowContext(ctx, `SELECT warn_after_no_check_in_for,sos_after_no_check_in_for,last_heard_at FROM sources WHERE id=?`, source.ID).Scan(&warning, &sos, &heard); err != nil {
		return s, err
	}
	s.WarnAfterNoCheckInFor = time.Duration(warning) * time.Second
	s.SOSAfterNoCheckInFor = time.Duration(sos) * time.Second
	s.LastHeardAt = deadlineTime(heard)
	s.SenderDescription = senderDescription(source.Name)
	var last int64
	var maintenance string
	err := tx.QueryRowContext(ctx, `SELECT last_check_in_at,sent_by_node_name,nodes_in_whole_node_maintenance FROM check_in_watch WHERE alert_source_id=?`, source.ID).Scan(&last, &s.SentByNodeName, &maintenance)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Expected = true
	s.LastCheckInAt = deadlineTime(last)
	if err = json.Unmarshal([]byte(maintenance), &s.NodesInWholeNodeMaintenance); err != nil {
		return s, err
	}
	s.PlannedDowntime, err = currentPlannedDowntime(ctx, tx, plannedDowntimeTarget{AlertSourceID: source.ID}, s.SentByNodeName, s.NodesInWholeNodeMaintenance, now)
	return s, err
}

// checkAlertSourceCheckInTimes is the rule for when missing check-ins send
// a Warning (0 means off) and an SOS.
func checkAlertSourceCheckInTimes(warning, sos time.Duration) error {
	if warning != 0 && (warning < time.Minute || warning > time.Hour || warning%time.Minute != 0) {
		return fmt.Errorf("%w: the time before a warning must be 1 to 60 minutes, or off", ErrInvalid)
	}
	if sos < time.Minute || sos > time.Hour || sos%time.Minute != 0 {
		return fmt.Errorf("%w: the time before waking the person on call must be 1 to 60 minutes", ErrInvalid)
	}
	if warning != 0 && sos <= warning {
		return fmt.Errorf("%w: the time before waking the person on call must be longer than the time before a warning", ErrInvalid)
	}
	return nil
}

// AlertSourceCheckInStatus returns an alert source's check-in state.
func (a *App) AlertSourceCheckInStatus(ctx context.Context, p Principal, sourceID string) (AlertSourceCheckInStatus, error) {
	var s AlertSourceCheckInStatus
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		source, err := readSource(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		s, err = readAlertSourceCheckInStatus(ctx, tx, source, a.Now())
		return err
	})
	if err != nil {
		return s, err
	}
	// While an alert source keeps checking in, the times of its
	// latest ones are in memory only.
	team, err := a.lockTrackedTeam(ctx, false)
	if err != nil {
		return s, err
	}
	defer team.mu.Unlock()
	if tracked := team.alertSources[sourceID]; tracked != nil && tracked.expectsCheckIns && !tracked.checkInsStopped {
		s.LastHeardAt, s.LastCheckInAt = tracked.lastHeardAt, tracked.lastCheckInAt
	}
	return s, nil
}

// WatchAlertSourceCheckIns alerts about alert sources whose check-ins stopped,
// and clears those alerts when they resume. The background runner calls it
// for a team it visits. It goes through the team's alert sources in memory,
// and reads and writes the team's database only when an alert source has
// just gone silent or is marked as stopped.
// For each alert source marked as stopped it remembers when the
// passing of time next changes something, so that the team is visited then.
func (a *App) WatchAlertSourceCheckIns(ctx context.Context) error {
	now := a.Now().UTC()
	team, err := a.lockTrackedTeam(ctx, false)
	if err != nil {
		return err
	}
	defer team.mu.Unlock()
	var justStopped []string
	databaseWork := false
	for id, source := range team.alertSources {
		if source.checkInsStopped {
			databaseWork = true
		} else if source.expectsCheckIns && now.Sub(source.lastHeardAt) >= alertSourceCheckInsCountAsStoppedAfter {
			justStopped = append(justStopped, id)
		}
	}
	if !databaseWork && len(justStopped) == 0 {
		return nil
	}
	sort.Strings(justStopped)
	var stored map[string]storedAlertSourceCheckInState
	watchAgainAt := map[string]time.Time{}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		clear(watchAgainAt)
		// The exact times were in memory only. From here the Warning and SOS
		// rules need them in the database.
		for _, id := range justStopped {
			source := team.alertSources[id]
			if _, err := tx.ExecContext(ctx, `UPDATE check_in_watch SET check_ins_stopped=1,last_check_in_at=? WHERE alert_source_id=?`, source.lastCheckInAt.Unix(), id); err != nil {
				return err
			}
			if err := heardFromSource(ctx, tx, id, source.lastHeardAt); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT w.alert_source_id FROM check_in_watch w JOIN sources s ON s.id=w.alert_source_id WHERE s.active=1 AND w.check_ins_stopped=1 ORDER BY w.alert_source_id`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			if watchAgainAt[id], err = watchOneAlertSourceCheckIns(ctx, tx, id, now); err != nil {
				return err
			}
		}
		stored, err = readStoredAlertSourceCheckInState(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	team.apply(stored, now)
	for id, source := range team.alertSources {
		if source.checkInsStopped {
			source.watchAgainAt = watchAgainAt[id]
		}
	}
	return nil
}

// watchOneAlertSourceCheckIns applies the Warning, SOS, planned downtime, and
// resumption rules to one alert source from the team's database. The times
// there are exact while the alert source is marked as stopped, and in the
// transaction that records a check-in.
//
// It returns when the passing of time next changes something for the alert
// source: the Warning time, the SOS time, the end of planned downtime, or the
// SOS time after a person resolved the SOS incident. It returns zero when
// only a check-in or a change to the team's database changes anything.
func watchOneAlertSourceCheckIns(ctx context.Context, tx *sql.Tx, sourceID string, now time.Time) (time.Time, error) {
	source, err := readSource(ctx, tx, sourceID)
	if err != nil {
		return time.Time{}, err
	}
	status, err := readAlertSourceCheckInStatus(ctx, tx, source, now)
	if err != nil {
		return time.Time{}, err
	}
	var alerted string
	var alertedAt int64
	if err = tx.QueryRowContext(ctx, `SELECT alerted,alerted_at FROM check_in_watch WHERE alert_source_id=?`, sourceID).Scan(&alerted, &alertedAt); err != nil {
		return time.Time{}, err
	}
	key := "check-ins:" + sourceID
	// The titles stand alone: they name the alert source by the name the team
	// gave it and say what its silence may mean.
	quotedName := "“" + source.Name + "”"
	silenceExplained := "Alert source " + quotedName + " normally checks in with MMHotline every 30 seconds to show the sending program is running. Check the sending program, the server running the program, and the network connection from the server to MMHotline."
	// Only a check-in clears the alert: a signed alert counts as hearing
	// from the alert source, but it does not mean check-ins resumed.
	resumed := alerted != "" && status.LastCheckInAt.Unix() >= alertedAt
	if resumed {
		if _, err = tx.ExecContext(ctx, `UPDATE check_in_watch SET alerted='',alerted_at=0 WHERE alert_source_id=?`, sourceID); err != nil {
			return time.Time{}, err
		}
		title := quotedName + " checks in with MMHotline again"
		if alerted == "warning" {
			return time.Time{}, RecordSystemAlert(ctx, tx, "system", "", "resolved-warn", title, "", now)
		}
		return time.Time{}, problemCleared(ctx, tx, key, title, "Check-ins from "+quotedName+" resumed at "+displayTime(status.LastHeardAt), now)
	}
	if status.PlannedDowntime != nil {
		return status.PlannedDowntime.Until, nil
	}
	silentSince := status.LastHeardAt
	sosAt := silentSince.Add(status.SOSAfterNoCheckInFor)
	warningAt := time.Time{}
	if status.WarnAfterNoCheckInFor > 0 {
		warningAt = silentSince.Add(status.WarnAfterNoCheckInFor)
	}
	// Silent when planned downtime ended: the SOS follows one more minute
	// later, with no Warning first.
	if ended, err := lastPlannedDowntimeEnd(ctx, tx, plannedDowntimeTarget{AlertSourceID: sourceID}, status.SentByNodeName, status.NodesInWholeNodeMaintenance, now); err != nil {
		return time.Time{}, err
	} else if !ended.IsZero() && silentSince.Before(ended) {
		warningAt = time.Time{}
		if afterDowntime := ended.Add(sosAfterPlannedDowntimeEndsWithoutCheckIn); afterDowntime.After(sosAt) {
			sosAt = afterDowntime
		}
	}
	silenceTitle := func() string {
		return truncateRunes(quotedName+" hasn't checked in with MMHotline for "+minutesText(now.Sub(silentSince))+": it may have stopped, or its server may be down", 256)
	}
	sosDue, err := sosDueAgain(ctx, tx, key, alerted, !now.Before(sosAt), status.SOSAfterNoCheckInFor, now)
	if err != nil {
		return time.Time{}, err
	}
	switch {
	case sosDue:
		if _, err = tx.ExecContext(ctx, `UPDATE check_in_watch SET alerted='sos',alerted_at=? WHERE alert_source_id=?`, now.Unix(), sourceID); err != nil {
			return time.Time{}, err
		}
		in := GenericAlert{Severity: "sos", SenderIncidentKey: key, Title: silenceTitle(), Message: silenceExplained}
		_, err = admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", in, nil, now)
		// The SOS incident is unresolved now: nothing more happens by the
		// passing of time.
		return time.Time{}, err
	case alerted == "" && !warningAt.IsZero() && !now.Before(warningAt):
		if _, err = tx.ExecContext(ctx, `UPDATE check_in_watch SET alerted='warning',alerted_at=? WHERE alert_source_id=?`, now.Unix(), sourceID); err != nil {
			return time.Time{}, err
		}
		// A problem that returns while its incident is still open joins it
		// and contacts nobody again.
		if open, err := openSystemIncident(ctx, tx, key); err != nil || open {
			return sosAt, err
		}
		return sosAt, RecordSystemAlert(ctx, tx, "system", "", "warn", silenceTitle(), silenceExplained, now)
	}
	// Nothing was due. Say when something will be.
	switch alerted {
	case "":
		return earliest(warningAt, sosAt), nil
	case "warning":
		return sosAt, nil
	}
	return sosAgainAt(ctx, tx, key, status.SOSAfterNoCheckInFor)
}

// problemCleared handles an SOS incident whose problem MMHotline saw clear
// (check-ins resumed). An unclaimed
// incident is resolved with one resolution message. A claimed one stays
// open, because its owner may be in the middle of a fix: its history gets
// the note, and the owner gets one message that does not wake them.
func problemCleared(ctx context.Context, tx *sql.Tx, key, title, note string, at time.Time) error {
	i, err := readIncidentBy(ctx, tx, "i.source_id='system' AND i.sender_incident_key=? AND i.status!='resolved'", key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if i.Status == "open" {
		_, err = admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", GenericAlert{Severity: "resolved-sos", SenderIncidentKey: key, Title: title, Message: ""}, nil, at)
		return err
	}
	if err = recordIncidentAction(ctx, tx, i, "system", "problem_cleared", note, "", "", at); err != nil {
		return err
	}
	return QueueNotification(ctx, tx, Notification{Key: "cleared:" + NewID(), IncidentID: i.ID, Severity: "info", Title: title, Message: note + ". You claimed this incident, so it stays open until you resolve it.", Purpose: "system_notice", Recipients: []string{i.OwnerMemberID}, PeopleOnly: true, At: at})
}

func minutesText(d time.Duration) string {
	minutes := int(d / time.Minute)
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// openSystemIncident reports whether an unresolved incident MMHotline raised
// with this key is open.
func openSystemIncident(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM incidents WHERE source_id='system' AND sender_incident_key=? AND status!='resolved'`, key).Scan(&n)
	return n > 0, err
}

// sosDueAgain decides whether an SOS may go out for a problem that is still
// there. Normally one SOS per episode is enough. But when a person resolved
// that SOS incident while the problem went on, MMHotline alerts again once
// the SOS time has passed since the resolution, so a problem nobody is
// watching any more is never silent for good.
func sosDueAgain(ctx context.Context, tx *sql.Tx, key, alerted string, reachedSOSTime bool, sosAfter time.Duration, at time.Time) (bool, error) {
	if alerted != "sos" {
		return reachedSOSTime, nil
	}
	again, err := sosAgainAt(ctx, tx, key, sosAfter)
	return !again.IsZero() && !at.Before(again), err
}

// sosAgainAt returns from when an SOS may go out again for a problem whose
// SOS went out before: the SOS time after a person resolved that SOS
// incident. It returns zero while the incident is unresolved, because then
// nothing more happens by the passing of time.
func sosAgainAt(ctx context.Context, tx *sql.Tx, key string, sosAfter time.Duration) (time.Time, error) {
	if open, err := openSystemIncident(ctx, tx, key); err != nil || open {
		return time.Time{}, err
	}
	var resolved int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(resolved_at),0) FROM incidents WHERE source_id='system' AND sender_incident_key=?`, key).Scan(&resolved); err != nil || resolved == 0 {
		return time.Time{}, err
	}
	return time.Unix(resolved, 0).Add(sosAfter), nil
}
