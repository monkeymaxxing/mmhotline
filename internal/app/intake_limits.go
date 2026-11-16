package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Alert intake limits. MMHotline is an alerting system read by people, not a
// logging system: these stop a misconfigured sender from flooding a team, and
// never stop an SOS.
const (
	installationMaximumNewWarningAndInfoAlertsPerHour = 1000
	maxAlertSources                                   = 100
	// sosReservedPendingDeliveries is the part of the pending-delivery limit
	// only SOS deliveries may use, so ordinary deliveries never block an SOS.
	sosReservedPendingDeliveries = 1000
	// maxPendingSOSDeliveriesPerAlertSource is one alert source's share of
	// the pending deliveries for SOS. At a quarter of the SOS reserve, it
	// takes four alert sources flooding at once to use up the reserve. It
	// still holds 25 incidents' messages to 10 contact methods subscribed to
	// SOS, and at about one delivery a second it drains in about 4 minutes.
	maxPendingSOSDeliveriesPerAlertSource = 250
)

var (
	errWrongSignature      = fmt.Errorf("%w: wrong signature", ErrUnauthorized)
	errSenderClockOff      = fmt.Errorf("%w: the sender's clock is more than 5 minutes off", ErrUnauthorized)
	errTooManyAlerts       = fmt.Errorf("%w: too many alerts", ErrRateLimited)
	errTooManyIncidents    = fmt.Errorf("%w: this installation has %d unresolved SOS incidents", ErrRateLimited, maxUnresolvedIncidents)
	errPendingDeliveryFull = fmt.Errorf("%w: the limit of pending notification deliveries has been reached", ErrRateLimited)
	// errAlertSourcePendingSOSFull refuses an SOS from an alert source that
	// already has its share of SOS notifications waiting to be sent.
	errAlertSourcePendingSOSFull = fmt.Errorf("%w: this alert source already has %d SOS notifications waiting to be sent", ErrRateLimited, maxPendingSOSDeliveriesPerAlertSource)
)

// Reasons shown in "Rejected requests in the last 24 hours".
const (
	rejectedTooManyAlerts  = "Too many alerts"
	rejectedWrongSignature = "Wrong signature"
	rejectedClockOff       = "The sender's clock is more than 5 minutes off"
	rejectedInvalidAlert   = "The alert is not valid"
)

func rejectionReason(err error) string {
	switch {
	case errors.Is(err, errTooManyAlerts):
		return rejectedTooManyAlerts
	case errors.Is(err, errWrongSignature):
		return rejectedWrongSignature
	case errors.Is(err, errSenderClockOff):
		return rejectedClockOff
	case errors.Is(err, ErrInvalid):
		return rejectedInvalidAlert
	}
	return ""
}

// rejectedRequests counts refused intake requests per alert source in memory,
// by hour, for the last 24 hours. Keeping them in memory means garbage
// requests can never force database writes. Only existing alert sources are
// counted, so memory stays bounded. Counts start again after a restart.
type rejectedRequests struct {
	mu     sync.Mutex
	counts map[string]map[string][]hourCount // source -> reason -> hours
	// told remembers when administrators were last told that an alert source
	// sends too many alerts, so they are told at most once an hour.
	told map[string]time.Time
}

type hourCount struct{ hour, count int64 }

func newRejectedRequests() *rejectedRequests {
	return &rejectedRequests{counts: map[string]map[string][]hourCount{}, told: map[string]time.Time{}}
}

func (r *rejectedRequests) add(sourceID, reason string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := sourceID
	if r.counts[key] == nil {
		r.counts[key] = map[string][]hourCount{}
	}
	hour := now.Unix() / 3600
	hours := r.counts[key][reason]
	kept := hours[:0]
	for _, h := range hours {
		if h.hour > hour-24 {
			kept = append(kept, h)
		}
	}
	//nolint:nilaway // Indexed only when n > 0.
	if n := len(kept); n > 0 && kept[n-1].hour == hour {
		kept[n-1].count++
	} else {
		kept = append(kept, hourCount{hour: hour, count: 1})
	}
	r.counts[key][reason] = kept
}

// RejectedRequest is one reason and how many requests it refused.
type RejectedRequest struct {
	Reason string
	Count  int64
}

func (r *rejectedRequests) last24Hours(sourceID string, now time.Time) (int64, []RejectedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hour := now.Unix() / 3600
	var total int64
	var out []RejectedRequest
	for reason, hours := range r.counts[sourceID] {
		var n int64
		for _, h := range hours {
			if h.hour > hour-24 {
				n += h.count
			}
		}
		if n > 0 {
			out = append(out, RejectedRequest{Reason: reason, Count: n})
			total += n
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reason < out[j].Reason })
	return total, out
}

// shouldTellAboutTooManyAlerts reports whether administrators should now be
// told about this alert source, at most once an hour.
func (r *rejectedRequests) shouldTellAboutTooManyAlerts(sourceID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := sourceID
	if last, ok := r.told[key]; ok && now.Sub(last) < time.Hour {
		return false
	}
	r.told[key] = now
	return true
}

// RejectedRequests returns an alert source's refused requests of the last 24
// hours, for administrators.
func (a *App) RejectedRequests(ctx context.Context, p Principal, sourceID string) (int64, []RejectedRequest, error) {
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := RequireMember(ctx, tx, p, true)
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	total, reasons := a.rejected.last24Hours(sourceID, a.Now())
	return total, reasons, nil
}

// countTeamWarningAndInfoAlert enforces the installation's hourly limit on new
// Warning and Info alerts (resolved ones included). SOS and resolved SOS
// alerts are never counted.
func countTeamWarningAndInfoAlert(ctx context.Context, tx *sql.Tx, severity string, now time.Time) error {
	if baseNotificationSeverity(severity) == "sos" {
		return nil
	}
	hour := now.Unix() / 3600
	var bucket, count int64
	err := tx.QueryRowContext(ctx, `SELECT hour_bucket,warning_and_info_alerts FROM team_alert_rate WHERE id=1`).Scan(&bucket, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if bucket != hour {
		count = 0
	}
	if count >= installationMaximumNewWarningAndInfoAlertsPerHour {
		return errTooManyAlerts
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO team_alert_rate(id,hour_bucket,warning_and_info_alerts) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET hour_bucket=excluded.hour_bucket,warning_and_info_alerts=excluded.warning_and_info_alerts`, hour, count+1)
	return err
}

// refusedSOSReason is the plain reason shown in the MMHotline SOS that opens
// when a correctly signed SOS is refused.
func refusedSOSReason(err error) string {
	switch {
	case errors.Is(err, errSenderClockOff):
		return "the sender's clock is more than 5 minutes off"
	case errors.Is(err, errTooManyIncidents):
		return fmt.Sprintf("this installation already has %d unresolved SOS incidents", maxUnresolvedIncidents)
	case errors.Is(err, errPendingDeliveryFull):
		return "too many notifications are waiting to be sent for this installation"
	case errors.Is(err, errAlertSourcePendingSOSFull):
		return fmt.Sprintf("this alert source already has %d SOS notifications waiting to be sent", maxPendingSOSDeliveriesPerAlertSource)
	case errors.Is(err, ErrInvalid):
		return "the alert is not valid (" + publicError(err) + ")"
	}
	return "MMHotline could not save the alert"
}

// recordRefusedSOS opens, or joins, one MMHotline SOS per alert source saying
// that MMHotline is refusing that source's SOS alerts. It runs after the
// refused request's own transaction has rolled back. It is the one SOS
// MMHotline raises itself, and it is exempt from the unresolved-incident limit
// (at most one per alert source is open).
func (a *App) recordRefusedSOS(ctx context.Context, sourceID string, refusal error) error {
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM sources WHERE id=?`, sourceID).Scan(&name); err != nil {
			return err
		}
		reason := refusedSOSReason(refusal)
		in := GenericAlert{Severity: "sos", SenderIncidentKey: "refusing-sos:" + sourceID, Title: truncateRunes(fmt.Sprintf("MMHotline is refusing SOS alerts from “%s”: %s", name, reason), 256), Message: fmt.Sprintf("Alert source “%s” sent an SOS alert, and MMHotline refused it because %s. Fix the sender, then resolve the incident about refused SOS alerts.", name, reason)}
		_, err := admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", in, nil, now)
		return err
	})
}

// tellAboutTooManyAlerts tells administrators, at most once an hour per alert
// source, that a source is being refused for sending too many alerts.
func (a *App) tellAboutTooManyAlerts(ctx context.Context, sourceID string) error {
	now := a.Now().UTC()
	if !a.rejected.shouldTellAboutTooManyAlerts(sourceID, now) {
		return nil
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM sources WHERE id=?`, sourceID).Scan(&name); err != nil {
			return err
		}
		return RecordAdministratorNotice(ctx, tx, "warn", truncateRunes(fmt.Sprintf("Alert source “%s” is sending too many alerts: MMHotline refuses some of them", name), 256), fmt.Sprintf("MMHotline is refusing some Warning and Info alerts from “%s”: an alert source may send %d alerts a minute, and an installation %d new Warning and Info alerts an hour. SOS alerts are exempt from the rate limits. See the alert source's page for the rejected requests.", name, SourceAlertsPerMinute, installationMaximumNewWarningAndInfoAlertsPerHour), now)
	})
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
