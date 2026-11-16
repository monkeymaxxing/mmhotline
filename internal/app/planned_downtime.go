package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Planned downtime is a time, always with an end, during which MMHotline
// makes no alerts about missing check-ins from one alert source.
// It is set by hand, or comes from a whole-node maintenance window listed in
// a check-in.
const maxPlannedDowntimeAhead = 7 * 24 * time.Hour

const plannedDowntimeSchema = `
CREATE TABLE planned_downtimes(
 id TEXT PRIMARY KEY,
 alert_source_id TEXT NOT NULL,
 until INTEGER NOT NULL,
 reason TEXT NOT NULL,
 started_by_member_id TEXT NOT NULL,
 started_at INTEGER NOT NULL,
 ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX planned_downtimes_target ON planned_downtimes(alert_source_id,until);
`

// plannedDowntimeTarget names what the downtime is for: the check-ins of
// one alert source.
type plannedDowntimeTarget struct{ AlertSourceID string }

// PlannedDowntimeView is an active planned downtime as a page shows it.
type PlannedDowntimeView struct {
	ID            string `json:"planned_downtime_id"`
	Reason        string `json:"reason"`
	StartedByName string `json:"started_by_name"`
	// SenderMaintenanceNodeName names the node whose maintenance window, listed
	// in the alert source's check-ins, supplies the downtime; the window's
	// starter is in StartedByName.
	SenderMaintenanceNodeName string    `json:"sender_maintenance_node_name"`
	Until                     time.Time `json:"until"`
}

// Text is the line alert-sources list prints, for example "Planned
// downtime until 2026-11-15T07:45:00Z, started by Matt: moving the
// database.", with the command's RFC 3339 times.
func (v PlannedDowntimeView) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Planned downtime until %s", commandTime(v.Until))
	if v.SenderMaintenanceNodeName != "" {
		fmt.Fprintf(&b, ", because the check-ins list server %s as in maintenance", v.SenderMaintenanceNodeName)
		if v.StartedByName != "" {
			fmt.Fprintf(&b, ", started by %s", v.StartedByName)
		}
	} else if v.StartedByName != "" {
		fmt.Fprintf(&b, ", started by %s", v.StartedByName)
	}
	if v.Reason != "" {
		fmt.Fprintf(&b, ": %s", v.Reason)
	}
	return strings.TrimSuffix(b.String(), ".") + "."
}

// currentPlannedDowntime returns the active planned downtime for a target, or
// nil. By-hand downtime comes first; otherwise a whole-node maintenance window
// on followedNode, from the latest check-in, counts.
func currentPlannedDowntime(ctx context.Context, tx *sql.Tx, target plannedDowntimeTarget, followedNode string, maintenance []NodeInMaintenance, now time.Time) (*PlannedDowntimeView, error) {
	var v PlannedDowntimeView
	var until int64
	var member string
	err := tx.QueryRowContext(ctx, `SELECT id,reason,started_by_member_id,until FROM planned_downtimes WHERE alert_source_id=? AND started_at<=? AND until>? AND ended_at=0 ORDER BY until DESC LIMIT 1`, target.AlertSourceID, now.Unix(), now.Unix()).Scan(&v.ID, &v.Reason, &member, &until)
	if err == nil {
		v.Until = deadlineTime(until)
		v.StartedByName, err = incidentMemberName(ctx, tx, member)
		return &v, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if followedNode == "" {
		return nil, nil
	}
	var best *PlannedDowntimeView
	for _, n := range maintenance {
		if n.NodeName == followedNode && n.MaintenanceUntil.After(now) && (best == nil || n.MaintenanceUntil.After(best.Until)) {
			best = &PlannedDowntimeView{SenderMaintenanceNodeName: n.NodeName, StartedByName: n.StartedBy, Reason: n.Reason, Until: n.MaintenanceUntil}
		}
	}
	return best, nil
}

// lastPlannedDowntimeEnd returns when the most recent planned downtime for a
// target ended, or zero.
func lastPlannedDowntimeEnd(ctx context.Context, tx *sql.Tx, target plannedDowntimeTarget, followedNode string, maintenance []NodeInMaintenance, now time.Time) (time.Time, error) {
	var ended int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CASE WHEN ended_at>0 AND ended_at<until THEN ended_at ELSE until END),0) FROM planned_downtimes WHERE alert_source_id=? AND (until<=? OR (ended_at>0 AND ended_at<=?))`, target.AlertSourceID, now.Unix(), now.Unix()).Scan(&ended)
	if err != nil {
		return time.Time{}, err
	}
	last := deadlineTime(ended)
	for _, n := range maintenance {
		if followedNode != "" && n.NodeName == followedNode && !n.MaintenanceUntil.After(now) && n.MaintenanceUntil.After(last) {
			last = n.MaintenanceUntil
		}
	}
	return last, nil
}

// StartPlannedDowntime starts planned downtime by hand. Any team member may;
// the alert history records who did.
func (a *App) StartPlannedDowntime(ctx context.Context, p Principal, target plannedDowntimeTarget, until time.Time, reason string) error {
	now := a.Now().UTC()
	reason = strings.TrimSpace(reason)
	if !until.After(now) || until.After(now.Add(maxPlannedDowntimeAhead)) {
		return fmt.Errorf("%w: planned downtime must end in the future, at most 7 days from now", ErrInvalid)
	}
	if !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > 200 {
		return fmt.Errorf("%w: the planned downtime reason must be at most 200 characters", ErrInvalid)
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		what, err := plannedDowntimeSubject(ctx, tx, target)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE planned_downtimes SET ended_at=? WHERE alert_source_id=? AND ended_at=0 AND until>?`, now.Unix(), target.AlertSourceID, now.Unix()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO planned_downtimes(id,alert_source_id,until,reason,started_by_member_id,started_at) VALUES(?,?,?,?,?,?)`, NewID(), target.AlertSourceID, until.Unix(), reason, m.ID, now.Unix()); err != nil {
			return err
		}
		message := fmt.Sprintf("%s started planned downtime until %s. MMHotline will not alert if %s until then.", m.Name, displayTime(until), what.stopsCheckingIn)
		if reason != "" {
			message += " Reason: " + reason
		}
		return RecordSystemAlert(ctx, tx, "member:"+m.ID, "", "info", truncateRunes(m.Name+" started planned downtime for "+what.source+" until "+displayTime(until), 256), message, now)
	})
}

// EndPlannedDowntime ends the active by-hand planned downtime now.
func (a *App) EndPlannedDowntime(ctx context.Context, p Principal, target plannedDowntimeTarget) error {
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		what, err := plannedDowntimeSubject(ctx, tx, target)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE planned_downtimes SET ended_at=? WHERE alert_source_id=? AND ended_at=0 AND until>? AND started_at<=?`, now.Unix(), target.AlertSourceID, now.Unix(), now.Unix())
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			return errors.Join(err, fmt.Errorf("%w: there is no planned downtime to end", ErrConflict))
		}
		return RecordSystemAlert(ctx, tx, "member:"+m.ID, "", "info", truncateRunes(m.Name+" ended planned downtime for "+what.source, 256), fmt.Sprintf("%s ended planned downtime. MMHotline alerts again if %s.", m.Name, what.stopsCheckingIn), now)
	})
}

// plannedDowntimeSubject describes the alert source whose missing check-ins
// are silenced by the planned downtime: "alert source “Monitoring”", and
// "alert source “Monitoring” stops checking in".
type plannedDowntimeSubjectText struct{ source, stopsCheckingIn string }

func plannedDowntimeSubject(ctx context.Context, tx *sql.Tx, target plannedDowntimeTarget) (plannedDowntimeSubjectText, error) {
	source, err := readSource(ctx, tx, target.AlertSourceID)
	if err != nil {
		return plannedDowntimeSubjectText{}, err
	}
	who := senderDescription(source.Name)
	return plannedDowntimeSubjectText{source: who, stopsCheckingIn: who + " stops checking in"}, nil
}
