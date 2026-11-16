package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const MaxAvailabilityDuration = 30 * 24 * time.Hour

type PersonalAvailability struct {
	MemberID         string
	UnavailableUntil time.Time
	Revision         int64
}

type TeamMute struct {
	Until         time.Time
	ActorMemberID string
	Revision      int64
	Active        bool
	Expired       bool
}

type OnCallClaim struct {
	MemberID      string
	ActorMemberID string
	Until         time.Time
	Revision      int64
	Active        bool
	Expired       bool
}

type CoverageChange struct {
	ID            int64
	Kind          string
	ActorMemberID string
	MemberID      string
	Revision      int64
	At            time.Time
	Until         time.Time
	Severity      string
	Title         string
	Message       string
}

func ParseAvailabilityDuration(value string) (time.Duration, error) {
	var duration time.Duration
	if days, found := strings.CutSuffix(value, "d"); found {
		count, err := strconv.ParseInt(days, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: write the duration as a number and a unit, such as 30m, 2h, or 3d", ErrInvalid)
		}
		if count < 1 || count > 30 {
			return 0, fmt.Errorf("%w: duration must be between one minute and 30 days", ErrInvalid)
		}
		duration = time.Duration(count) * 24 * time.Hour
	} else {
		var err error
		if duration, err = time.ParseDuration(value); err != nil {
			return 0, fmt.Errorf("%w: write the duration as a number and a unit, such as 30m, 2h, or 3d", ErrInvalid)
		}
	}
	if duration < time.Minute || duration > MaxAvailabilityDuration {
		return 0, fmt.Errorf("%w: duration must be between one minute and 30 days", ErrInvalid)
	}
	return duration, nil
}

func validAvailabilityDeadline(now, until time.Time) bool {
	return until.IsZero() || until.After(now) && until.Sub(now) <= MaxAvailabilityDuration
}
func deadlineUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
func deadlineTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func ReadPersonalAvailability(ctx context.Context, tx *sql.Tx, memberID string) (PersonalAvailability, error) {
	state := PersonalAvailability{MemberID: memberID}
	var unavailable int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(a.unavailable_until,0),COALESCE(a.revision,0) FROM members m LEFT JOIN member_availability a ON a.member_id=m.id WHERE m.id=? AND m.removed_at IS NULL`, memberID).Scan(&unavailable, &state.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrNotFound
	}
	state.UnavailableUntil = deadlineTime(unavailable)
	return state, err
}

func MemberEligible(ctx context.Context, tx *sql.Tx, memberID string, at time.Time) (bool, string, error) {
	state, err := ReadPersonalAvailability(ctx, tx, memberID)
	if errors.Is(err, ErrNotFound) {
		return false, "member removed", nil
	}
	if err != nil {
		return false, "", err
	}
	if state.UnavailableUntil.After(at) {
		return false, "unavailable", nil
	}
	return true, "", nil
}

func ReadTeamMute(ctx context.Context, tx *sql.Tx, at time.Time) (TeamMute, error) {
	var state TeamMute
	var until int64
	err := tx.QueryRowContext(ctx, `SELECT until_at,actor_member_id,revision,expired FROM team_mute WHERE id=1`).Scan(&until, &state.ActorMemberID, &state.Revision, &state.Expired)
	state.Until = deadlineTime(until)
	state.Active = !state.Expired && state.Until.After(at)
	return state, err
}

func ReadOnCallClaim(ctx context.Context, tx *sql.Tx, at time.Time) (OnCallClaim, error) {
	var state OnCallClaim
	var until int64
	err := tx.QueryRowContext(ctx, `SELECT member_id,actor_member_id,until_at,revision,expired FROM on_call_claim WHERE id=1`).Scan(&state.MemberID, &state.ActorMemberID, &until, &state.Revision, &state.Expired)
	state.Until = deadlineTime(until)
	state.Active = !state.Expired && state.Until.After(at)
	return state, err
}

// DeliveryAllowed is separate from on-call eligibility. An empty member ID is
// the shared team account. The no-eligible-responder fallback may ignore
// personal controls; it never ignores the administrator's team-wide mute.
func DeliveryAllowed(ctx context.Context, tx *sql.Tx, memberID, contactMethodType string, sendWhileUnavailableOrMuted, ignorePersonal bool, at time.Time) (bool, string, error) {
	facts, ok := contactMethodTypeFacts(contactMethodType)
	if !ok {
		return false, "", ErrInvalid
	}
	if memberID != "" {
		eligible, reason, err := MemberEligible(ctx, tx, memberID, at)
		if err != nil {
			return false, "", err
		}
		if reason == "member removed" {
			return false, reason, nil
		}
		if !eligible && !sendWhileUnavailableOrMuted && !ignorePersonal {
			return false, reason, nil
		}
	}
	mute, err := ReadTeamMute(ctx, tx, at)
	if err != nil {
		return false, "", err
	}
	if mute.Active && (facts.WakesPeople || !sendWhileUnavailableOrMuted) {
		return false, "team mute", nil
	}
	return true, "", nil
}

func (a *App) SetPersonalAvailability(ctx context.Context, p Principal, kind string, until time.Time, expectedRevision int64) (PersonalAvailability, error) {
	var state PersonalAvailability
	now := a.Now().UTC()
	if kind != "unavailable" || !validAvailabilityDeadline(now, until) {
		return state, ErrInvalid
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		state, err = ReadPersonalAvailability(ctx, tx, m.ID)
		if err != nil {
			return err
		}
		if state.Revision != expectedRevision {
			return ErrConflict
		}
		state.UnavailableUntil = until.UTC()
		state.Revision++
		if _, err = tx.ExecContext(ctx, `INSERT INTO member_availability(member_id,unavailable_until,revision) VALUES (?,?,?) ON CONFLICT(member_id) DO UPDATE SET unavailable_until=excluded.unavailable_until,revision=excluded.revision`, m.ID, deadlineUnix(state.UnavailableUntil), state.Revision); err != nil {
			return err
		}
		title := m.Name + " is unavailable until " + displayTime(until)
		message := m.Name + " is unavailable until " + until.UTC().Format(time.RFC3339) + "."
		if until.IsZero() {
			title, message = m.Name+" is available again", m.Name+" is available again."
		}
		return recordCoverageChange(ctx, tx, CoverageChange{Kind: "personal_" + kind, ActorMemberID: m.ID, MemberID: m.ID, Revision: state.Revision, At: now, Until: until, Severity: "warn", Title: title, Message: message})
	})
	return state, err
}

func (a *App) SetTeamMute(ctx context.Context, p Principal, until time.Time, expectedRevision int64) (TeamMute, error) {
	var state TeamMute
	now := a.Now().UTC()
	if !validAvailabilityDeadline(now, until) {
		return state, ErrInvalid
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		state, err = ReadTeamMute(ctx, tx, now)
		if err != nil {
			return err
		}
		if state.Revision != expectedRevision {
			return ErrConflict
		}
		if until.IsZero() && state.Expired {
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE team_mute SET until_at=?,actor_member_id=?,revision=revision+1,expired=? WHERE id=1`, deadlineUnix(until), m.ID, until.IsZero()); err != nil {
			return err
		}
		kind, severity, title, message := "team_mute_started", "warn", m.Name+" muted the team until "+displayTime(until)+": nobody is woken", m.Name+" muted the team until "+until.UTC().Format(time.RFC3339)+". Pushover notifications and automated voice calls are not sent."
		if until.IsZero() {
			kind, severity, title, message = "team_mute_ended", "resolved-warn", m.Name+" unmuted the team: MMHotline wakes people again", m.Name+" unmuted the team. Personal unavailability still applies."
		}
		if err = recordCoverageChange(ctx, tx, CoverageChange{Kind: kind, ActorMemberID: m.ID, Revision: state.Revision + 1, At: now, Until: until, Severity: severity, Title: title, Message: message}); err != nil {
			return err
		}
		state, err = ReadTeamMute(ctx, tx, now)
		return err
	})
	return state, err
}

func (a *App) SetOnCallClaim(ctx context.Context, p Principal, until time.Time, expectedRevision int64, takeFromMemberID string) (OnCallClaim, error) {
	var state OnCallClaim
	now := a.Now().UTC()
	if until.IsZero() || !validAvailabilityDeadline(now, until) {
		return state, ErrInvalid
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		state, err = ReadOnCallClaim(ctx, tx, now)
		if err != nil {
			return err
		}
		if state.Revision != expectedRevision {
			return ErrConflict
		}
		if state.Active && state.MemberID != m.ID && takeFromMemberID != state.MemberID {
			return ErrConflict
		}
		if takeFromMemberID != "" && (!state.Active || takeFromMemberID != state.MemberID) {
			return ErrConflict
		}
		eligible, _, err := MemberEligible(ctx, tx, m.ID, now)
		if err != nil {
			return err
		}
		if !eligible {
			return fmt.Errorf("%w: make yourself available before taking on-call", ErrInvalid)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE on_call_claim SET member_id=?,actor_member_id=?,until_at=?,revision=revision+1,expired=0 WHERE id=1`, m.ID, m.ID, deadlineUnix(until)); err != nil {
			return err
		}
		if err = recordCoverageChange(ctx, tx, CoverageChange{Kind: "on_call_claim_started", ActorMemberID: m.ID, MemberID: m.ID, Revision: state.Revision + 1, At: now, Until: until, Severity: "warn", Title: m.Name + " takes on-call until " + displayTime(until), Message: m.Name + " has taken on-call until " + until.UTC().Format(time.RFC3339) + ". MMHotline contacts " + m.Name + " first. Incidents already assigned to a person keep the current owner."}); err != nil {
			return err
		}
		state, err = ReadOnCallClaim(ctx, tx, now)
		return err
	})
	return state, err
}

func (a *App) EndOnCallClaim(ctx context.Context, p Principal, expectedRevision int64) (OnCallClaim, error) {
	var state OnCallClaim
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		state, err = ReadOnCallClaim(ctx, tx, now)
		if err != nil {
			return err
		}
		if state.Revision != expectedRevision {
			return ErrConflict
		}
		if m.ID != state.MemberID && m.Role != RoleAdministrator {
			return ErrForbidden
		}
		if state.Expired {
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE on_call_claim SET expired=1,actor_member_id=?,revision=revision+1 WHERE id=1`, m.ID); err != nil {
			return err
		}
		if err = recordCoverageChange(ctx, tx, CoverageChange{Kind: "on_call_claim_ended", ActorMemberID: m.ID, MemberID: state.MemberID, Revision: state.Revision + 1, At: now, Severity: "resolved-warn", Title: "On-call is back with the schedule", Message: m.Name + " gave on-call back to the schedule. The current schedule applies."}); err != nil {
			return err
		}
		state, err = ReadOnCallClaim(ctx, tx, now)
		return err
	})
	return state, err
}

func recordCoverageChange(ctx context.Context, tx *sql.Tx, c CoverageChange) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO coverage_changes(kind,actor_member_id,member_id,revision,occurred_at,until_at,severity,title,message) VALUES (?,?,?,?,?,?,?,?,?)`, c.Kind, c.ActorMemberID, c.MemberID, c.Revision, c.At.Unix(), deadlineUnix(c.Until), c.Severity, c.Title, c.Message)
	if err == nil && c.Severity != "" {
		return RecordSystemAlert(ctx, tx, c.ActorMemberID, "", c.Severity, c.Title, c.Message, c.At)
	}
	return err
}

// ExpireCoverage controls logical expiry and audit exactly once in the existing
// team transaction. Eligibility already respects stored deadlines even before
// this runs. The scheduler uses returned changes to reconcile current work.
func ExpireCoverage(ctx context.Context, tx *sql.Tx, now time.Time) ([]CoverageChange, error) {
	var changes []CoverageChange
	rows, err := tx.QueryContext(ctx, `SELECT member_id,unavailable_until,revision FROM member_availability WHERE unavailable_until>0 AND unavailable_until<=? ORDER BY member_id`, now.Unix())
	if err != nil {
		return nil, err
	}
	var people []PersonalAvailability
	for rows.Next() {
		var p PersonalAvailability
		var unavailable int64
		if err = rows.Scan(&p.MemberID, &unavailable, &p.Revision); err != nil {
			rows.Close()
			return nil, err
		}
		p.UnavailableUntil = deadlineTime(unavailable)
		people = append(people, p)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		name, err := incidentMemberName(ctx, tx, p.MemberID)
		if err != nil {
			return nil, err
		}
		p.Revision++
		changes = append(changes, CoverageChange{Kind: "personal_unavailable_expired", MemberID: p.MemberID, Revision: p.Revision, At: now, Until: p.UnavailableUntil, Title: name + " is available again", Message: name + "'s time as unavailable ended."})
		if _, err = tx.ExecContext(ctx, `UPDATE member_availability SET unavailable_until=0,revision=? WHERE member_id=?`, p.Revision, p.MemberID); err != nil {
			return nil, err
		}
	}
	mute, err := ReadTeamMute(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	if !mute.Expired && !mute.Until.After(now) {
		if _, err = tx.ExecContext(ctx, `UPDATE team_mute SET expired=1,revision=revision+1 WHERE id=1`); err != nil {
			return nil, err
		}
		changes = append(changes, CoverageChange{Kind: "team_mute_expired", ActorMemberID: mute.ActorMemberID, Revision: mute.Revision + 1, At: now, Until: mute.Until, Severity: "resolved-warn", Title: "The team mute ended: MMHotline wakes people again", Message: "The team mute ended. Personal unavailability still applies."})
	}
	onCallClaim, err := ReadOnCallClaim(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	if !onCallClaim.Expired && !onCallClaim.Until.After(now) {
		if _, err = tx.ExecContext(ctx, `UPDATE on_call_claim SET expired=1,revision=revision+1 WHERE id=1`); err != nil {
			return nil, err
		}
		changes = append(changes, CoverageChange{Kind: "on_call_claim_expired", ActorMemberID: onCallClaim.ActorMemberID, MemberID: onCallClaim.MemberID, Revision: onCallClaim.Revision + 1, At: now, Until: onCallClaim.Until, Severity: "resolved-warn", Title: "On-call is back with the schedule", Message: "The temporary on-call duty ended. The current schedule applies."})
	}
	for i := range changes {
		if err = recordCoverageChange(ctx, tx, changes[i]); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// nextCoverageDeadline returns when ExpireCoverage or ObserveCoverageHandoff
// next has something to record: the end of a member's unavailability, of the
// team mute, or of the on-call claim, or the next handoff of the active
// schedule.
func nextCoverageDeadline(ctx context.Context, tx *sql.Tx, schedules ScheduleState, now time.Time) (time.Time, error) {
	var unavailable int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(unavailable_until),0) FROM member_availability WHERE unavailable_until>0`).Scan(&unavailable); err != nil {
		return time.Time{}, err
	}
	mute, err := ReadTeamMute(ctx, tx, now)
	if err != nil {
		return time.Time{}, err
	}
	claim, err := ReadOnCallClaim(ctx, tx, now)
	if err != nil {
		return time.Time{}, err
	}
	// ExpireCoverage records the end of one that is not marked as expired as
	// soon as its time is not ahead.
	ends := func(expired bool, until time.Time) time.Time {
		if expired {
			return time.Time{}
		}
		if !until.After(now) {
			return now
		}
		return until
	}
	handoff, err := nextScheduleHandoff(ctx, tx, schedules, now)
	if err != nil {
		return time.Time{}, err
	}
	return earliest(deadlineTime(unavailable), ends(mute.Expired, mute.Until), ends(claim.Expired, claim.Until), handoff), nil
}
