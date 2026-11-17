package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Receipt state and the immutable counted incidents live in SQLite. Preparing
// the snapshot before sending protects against a concurrent claim removing a
// combined delivery before Pushover returns its receipt.
const pushoverSchema = `
CREATE TABLE pushover_receipts (
 delivery_id TEXT PRIMARY KEY REFERENCES deliveries(id),
 receipt TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('sending','open','cancelling','acknowledged','expired','cancelled','failed','uncertain')),
 next_poll_at INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX pushover_receipts_due ON pushover_receipts(state,next_poll_at);
CREATE TABLE pushover_receipt_incidents (
 delivery_id TEXT NOT NULL REFERENCES pushover_receipts(delivery_id),
 incident_id TEXT NOT NULL REFERENCES incidents(id),
 PRIMARY KEY(delivery_id,incident_id)
);
CREATE INDEX pushover_receipt_incidents_incident ON pushover_receipt_incidents(incident_id,delivery_id);
`

func (*PushoverNotificationContactMethodType) prepareDelivery(ctx context.Context, tx *sql.Tx, d Delivery, combined []CombinedAlert, now time.Time) error {
	if !pushoverEmergency(d.Purpose, d.Severity) {
		return nil
	}
	expire := pushoverEmergencyExpirySeconds
	if d.Purpose == "contact_method_test" {
		expire = pushoverTestExpirySeconds
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO pushover_receipts(delivery_id,state,expires_at) VALUES(?,'sending',?) ON CONFLICT DO NOTHING`, d.ID, now.Add(time.Duration(expire)*time.Second).Unix())
	if err != nil {
		return err
	}
	count, err := inserted.RowsAffected()
	if err != nil || count == 0 {
		return err
	}
	ids := []string{}
	if d.IncidentID != "" {
		ids = append(ids, d.IncidentID)
	}
	for _, alert := range combined {
		if alert.IncidentID != "" && !slices.Contains(ids, alert.IncidentID) {
			ids = append(ids, alert.IncidentID)
		}
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pushover_receipt_incidents(delivery_id,incident_id) VALUES(?,?)`, d.ID, id); err != nil {
			return err
		}
	}
	return nil
}
func (*PushoverNotificationContactMethodType) recordDeliveryOutcome(ctx context.Context, tx *sql.Tx, d Delivery, _ string, outcome deliveryOutcome, now time.Time) error {
	if !pushoverEmergency(d.Purpose, d.Severity) {
		return nil
	}
	state := outcome.AttemptState
	receipt := ""
	next := int64(0)
	if state == "accepted" {
		if err := tx.QueryRowContext(ctx, `SELECT id_from_sending_service FROM deliveries WHERE id=?`, d.ID).Scan(&receipt); err != nil {
			return err
		}
		state = "open"
		next = pushoverNextRequestAt(now)
	}
	expire := pushoverEmergencyExpirySeconds
	if d.Purpose == "contact_method_test" {
		expire = pushoverTestExpirySeconds
	}
	_, err := tx.ExecContext(ctx, `UPDATE pushover_receipts SET receipt=?,state=?,next_poll_at=?,expires_at=? WHERE delivery_id=?`, receipt, state, next, now.Add(time.Duration(expire)*time.Second).Unix(), d.ID)
	return err
}
func pushoverCountedIncidentIDs(ctx context.Context, tx *sql.Tx, deliveryID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT incident_id FROM pushover_receipt_incidents WHERE delivery_id=? ORDER BY incident_id`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// resendsAfterUnknownOutcome: an emergency may already ring, so only an
// ordinary message is sent again.
func (*PushoverNotificationContactMethodType) resendsAfterUnknownOutcome(d Delivery) bool {
	return !pushoverEmergency(d.Purpose, d.Severity)
}

// escalationRoundStillRinging reports whether an emergency about the
// incident still rings on the contact method: its receipt is open.
func (*PushoverNotificationContactMethodType) escalationRoundStillRinging(ctx context.Context, tx *sql.Tx, contactMethodID, incidentID string, at time.Time) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pushover_receipts r JOIN deliveries d ON d.id=r.delivery_id JOIN pushover_receipt_incidents i ON i.delivery_id=r.delivery_id WHERE r.state IN ('open','cancelling') AND r.expires_at>? AND d.contact_method_id=? AND i.incident_id=?`, at.Unix(), contactMethodID, incidentID).Scan(&count)
	return count > 0, err
}

type pushoverReceiptWork struct {
	State                 string
	Delivery              Delivery
	Receipt               string
	NextPollAt, ExpiresAt time.Time
	Cancel                bool
}

func openPushoverReceipts(ctx context.Context, tx *sql.Tx) ([]pushoverReceiptWork, error) {
	rows, err := tx.QueryContext(ctx, `SELECT delivery_id,receipt,next_poll_at,expires_at,state FROM pushover_receipts WHERE state IN ('open','cancelling') ORDER BY next_poll_at,delivery_id`)
	if err != nil {
		return nil, err
	}
	var work []pushoverReceiptWork
	for rows.Next() {
		var item pushoverReceiptWork
		var next, expires int64
		if err = rows.Scan(&item.Delivery.ID, &item.Receipt, &next, &expires, &item.State); err != nil {
			rows.Close()
			return nil, err
		}
		item.NextPollAt = deadlineTime(next)
		item.ExpiresAt = deadlineTime(expires)
		work = append(work, item)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for index := range work {
		work[index].Delivery, err = scanDelivery(tx.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE id=?`, work[index].Delivery.ID))
		if err != nil {
			return nil, err
		}
	}
	return work, nil
}
func pushoverReceiptNeedsCancellation(ctx context.Context, tx *sql.Tx, item pushoverReceiptWork, now time.Time) (bool, error) {
	d := item.Delivery
	wanted, _, _, err := contactMethodStillWants(ctx, tx, d, now)
	if err != nil || !wanted {
		return !wanted, err
	}
	if d.Purpose == "contact_method_test" {
		return false, nil
	}
	ids, err := pushoverCountedIncidentIDs(ctx, tx, d.ID)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		// A claim or resolution closes an incident's part of the emergency. An
		// escalation target change also stops waking the previous target.
		i, err := readIncident(ctx, tx, id)
		if err != nil {
			return false, err
		}
		if i.Status == "open" && containsMember(i.TargetMemberIDs, d.MemberID) {
			return false, nil
		}
		// An SOS that joined a claimed incident wakes the person who claimed
		// it until they acknowledge it, as any SOS does.
		if i.Status == "claimed" && i.OwnerMemberID == d.MemberID && strings.HasPrefix(d.NotificationKey, "related_alert_sent_to_claimer:") {
			return false, nil
		}
	}
	return true, nil
}
func nextPushoverReceiptDeadline(ctx context.Context, tx *sql.Tx, now time.Time) (time.Time, error) {
	work, err := openPushoverReceipts(ctx, tx)
	if err != nil {
		return time.Time{}, err
	}
	var deadline time.Time
	for _, item := range work {
		cancel, err := pushoverReceiptNeedsCancellation(ctx, tx, item, now)
		if err != nil {
			return time.Time{}, err
		}
		due := item.NextPollAt
		if cancel && item.State == "open" {
			due = now
		}
		deadline = earliest(deadline, due)
	}
	return deadline, nil
}

// ProcessPushoverReceipt reserves at most one due receipt action. Its persisted
// next_poll_at is moved before the request, so a concurrent visit or restart
// cannot ask for the same receipt faster than once every five seconds.
func (a *App) ProcessPushoverReceipt(ctx context.Context) (bool, error) {
	contactMethodType, ok := a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	if !ok {
		return false, nil
	}
	now := a.Now().UTC()
	var selected pushoverReceiptWork
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		work, err := openPushoverReceipts(ctx, tx)
		if err != nil {
			return err
		}
		for _, item := range work {
			cancel, err := pushoverReceiptNeedsCancellation(ctx, tx, item, now)
			if err != nil {
				return err
			}
			if item.NextPollAt.After(now) && (!cancel || item.State == "cancelling") {
				continue
			}
			// Poll a due receipt before cancelling, so an acknowledgement already
			// present can report that someone else claimed or resolved the incident.
			item.Cancel = item.State == "cancelling" || (cancel && item.NextPollAt.After(now))
			selected = item
			_, err = tx.ExecContext(ctx, `UPDATE pushover_receipts SET next_poll_at=?,state=CASE WHEN ? THEN 'cancelling' ELSE state END WHERE delivery_id=? AND state IN ('open','cancelling')`, pushoverNextRequestAt(now), item.Cancel, item.Delivery.ID)
			return err
		}
		return nil
	})
	if err != nil || selected.Delivery.ID == "" {
		return false, err
	}
	if selected.Cancel {
		return true, a.cancelPushoverReceipt(ctx, contactMethodType, selected)
	}
	answer, err := contactMethodType.call(ctx, http.MethodGet, "receipts/"+selected.Receipt, nil)
	if err != nil {
		return true, a.recordPushoverReceiptError(ctx, selected, err)
	}
	if answer.Acknowledged == 1 {
		return true, a.acknowledgePushoverReceipt(ctx, selected, answer)
	}
	if answer.Expired == 1 || !now.Before(selected.ExpiresAt) {
		return true, a.closePushoverReceipt(ctx, selected, "expired")
	}
	// Claims may have committed during the sending service request. Check again before
	// deciding whether the emergency should keep ringing.
	var cancel bool
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		cancel, err = pushoverReceiptNeedsCancellation(ctx, tx, selected, a.Now())
		return err
	})
	if err != nil {
		return true, err
	}
	if cancel {
		return true, a.cancelPushoverReceipt(ctx, contactMethodType, selected)
	}
	return true, nil
}
func (a *App) closePushoverReceipt(ctx context.Context, item pushoverReceiptWork, state string) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE pushover_receipts SET state=?,next_poll_at=0 WHERE delivery_id=? AND state IN ('open','cancelling')`, state, item.Delivery.ID)
		return err
	})
}
func (a *App) cancelPushoverReceipt(ctx context.Context, c *PushoverNotificationContactMethodType, item pushoverReceiptWork) error {
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE pushover_receipts SET state='cancelling',next_poll_at=? WHERE delivery_id=? AND state='open'`, pushoverNextRequestAt(a.Now()), item.Delivery.ID)
		return err
	}); err != nil {
		return err
	}
	if _, err := c.call(ctx, http.MethodPost, "receipts/"+item.Receipt+"/cancel", nil); err != nil {
		return a.recordPushoverReceiptError(ctx, item, err)
	}
	return a.closePushoverReceipt(ctx, item, "cancelled")
}
func (a *App) recordPushoverReceiptError(ctx context.Context, item pushoverReceiptWork, cause error) error {
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET reason=? WHERE id=?`, boundedDiagnostic("Pushover receipt: "+cause.Error()), item.Delivery.ID); err != nil {
			return err
		}
		// The sending service's configured expiry bounds ringing even if its receipt
		// API remains unavailable. Keep the diagnostic, then stop requesting.
		if !a.Now().Before(item.ExpiresAt) {
			_, err := tx.ExecContext(ctx, `UPDATE pushover_receipts SET state='expired',next_poll_at=0 WHERE delivery_id=? AND state IN ('open','cancelling')`, item.Delivery.ID)
			return err
		}
		return nil
	})
	return errors.Join(cause, err)
}
func (a *App) acknowledgePushoverReceipt(ctx context.Context, item pushoverReceiptWork, answer pushoverAnswer) error {
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM pushover_receipts WHERE delivery_id=?`, item.Delivery.ID).Scan(&state); err != nil {
			return err
		}
		if state != "open" {
			return nil
		}
		d := item.Delivery
		// A personal user key must acknowledge as itself. A mistakenly entered
		// group key can never let a different Pushover user act as the owner.
		if answer.AcknowledgedBy != d.SentTo {
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET reason=? WHERE id=?`, "Pushover acknowledgement came from another user; no incidents were claimed. Add a personal Pushover user key.", d.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE pushover_receipts SET state='acknowledged',next_poll_at=0 WHERE delivery_id=?`, d.ID)
			return err
		}
		var memberExists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM members WHERE id=? AND removed_at IS NULL`, d.MemberID).Scan(&memberExists); err != nil {
			return err
		}
		if memberExists > 0 && d.Purpose != "contact_method_test" {
			results, err := actOnNotificationIncidentsTx(ctx, tx, d.ID, d.MemberID, "pushover_acknowledgement", false, now)
			if err != nil {
				return err
			}

			// The receipt transition, incident claims, and one result message commit
			// together. A restart cannot claim twice or lose the result message.
			result := Delivery{ID: NewID(), IncidentID: d.IncidentID, AlertID: d.AlertID, NotificationKey: "pushover_acknowledgement/" + d.ID, ContactMethodID: d.ContactMethodID, ContactMethodRevision: d.ContactMethodRevision, MemberID: d.MemberID, ContactMethodType: d.ContactMethodType, SentTo: d.SentTo, Purpose: "system_notice", Severity: "info", Subject: "MMHotline claim result", Message: pushoverClaimResultText(results), State: "queued", CreatedAt: now, NextAttempt: now, IgnorePersonal: true}
			if err = insertDelivery(ctx, tx, result); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE pushover_receipts SET state='acknowledged',next_poll_at=0 WHERE delivery_id=?`, d.ID)
		return err
	})
}

// Keep complete result instructions; only incident titles may be shortened.
func pushoverClaimResultText(results []NotificationActionResult) string {
	var lines []string
	claimed := 0
	for _, result := range results {
		if result.Status == "claimed" && !strings.HasPrefix(result.Text, "Already claimed by ") {
			claimed++
		}
	}
	if len(results) > 1 {
		lines = append(lines, fmt.Sprintf("You claimed %d of %d incidents.", claimed, len(results)))
	}
	for _, result := range results {
		text := result.Text
		if result.Status == "claimed" && !strings.HasPrefix(text, "Already claimed by ") {
			text = "You claimed " + truncateRunes(result.Title, 100) + "."
		} else if len(results) > 1 {
			text = truncateRunes(result.Title, 100) + ": " + text
		}
		// Reserve room to explain omitted results, retaining the page link in
		// the supplementary URL rather than cutting an instruction in half.
		if len([]rune(strings.Join(lines, "\n")+"\n"+text)) > 930 {
			lines = append(lines, "Open in MMHotline to see all counted incidents.")
			break
		}
		lines = append(lines, text)
	}
	return strings.Join(lines, "\n")
}

// Deadlines are stored in whole seconds. Round up, never down, so a real
// clock with nanoseconds still leaves at least five seconds between requests.
func pushoverNextRequestAt(now time.Time) int64 {
	next := now.Add(pushoverReceiptInterval)
	if next.Nanosecond() != 0 {
		next = next.Truncate(time.Second).Add(time.Second)
	}
	return next.Unix()
}

// The outcomes of proving a Pushover test notification from its receipt.
const (
	// pushoverReceiptProven: Pushover knows the receipt, so it accepted the
	// notification, and MMHotline cancelled the notification's repeats.
	pushoverReceiptProven = "accepted_and_repeats_cancelled"
	// pushoverReceiptMissing: the answer to the notification had no usable
	// receipt, so nothing proves Pushover accepted it.
	pushoverReceiptMissing = "no_receipt"
	// pushoverReceiptRefused: Pushover did not confirm the receipt.
	pushoverReceiptRefused = "receipt_refused_by_pushover"
	// pushoverCancelFailed: Pushover knows the receipt, but MMHotline could
	// not cancel the notification's repeats.
	pushoverCancelFailed = "cancel_failed"
)

// proveEmergencyAndCancelIt reads the receipt of an emergency notification
// MMHotline did not send itself, as the MMHotline-down alarm's test does,
// to see that Pushover accepted the notification, and then cancels its
// repeats at once, so the phone does not go on sounding. It returns the
// outcome, and for a failure Pushover's or the network's reason.
func (c *PushoverNotificationContactMethodType) proveEmergencyAndCancelIt(ctx context.Context, receipt string) (outcome, problem string) {
	if !pushoverKeyPattern.MatchString(receipt) {
		return pushoverReceiptMissing, ""
	}
	if _, err := c.call(ctx, http.MethodGet, "receipts/"+receipt, nil); err != nil {
		return pushoverReceiptRefused, err.Error()
	}
	if _, err := c.call(ctx, http.MethodPost, "receipts/"+receipt+"/cancel", nil); err != nil {
		return pushoverCancelFailed, err.Error()
	}
	return pushoverReceiptProven, ""
}
