package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The delivery throttle keeps a misconfigured sender from turning into a blast
// no person can read. Warning, Info, and resolved messages to one contact
// method go at most once a minute; anything more waits and goes out as one
// combined message. After 10 messages in an hour, one combined message every
// 10 minutes. SOS is never delayed or combined with Warning and Info.
// Webhooks, read by programs, get every alert, at most 60 a minute.
const (
	contactMethodMinimumMessageInterval       = time.Minute
	contactMethodSlowDownAfterMessagesPerHour = 10
	contactMethodSlowedMessageInterval        = 10 * time.Minute
	webhookMaximumAlertsPerMinute             = 60
	maxAlertsListedInCombinedMessage          = 20
	throttledDeliveryCondition                = `purpose IN ('subscribed_alert','system_notice') AND severity!='sos'`
	waitingToCombineReason                    = "Waiting until %s to send the notification together with other messages to the contact method"
	combinedIntoAnotherMessageReason          = "Combined with other alerts into one message"
	sentTogetherReason                        = "Sent together with %s"
	webhookTooManyAlertsReason                = "Not sent: this webhook got more than %d alerts in one minute"
	// SOS chat messages: the first 10 in 10 minutes per contact method go one
	// by one with their buttons.
	chatSOSMessagesSentOneByOne = 10
	chatSOSMessagesWindow       = 10 * time.Minute
	chatSOSMessageCondition     = `purpose IN ('subscribed_alert','system_notice') AND severity='sos'`
)

// throttled reports whether a delivery is a Warning, Info, or resolved
// message the throttle may delay and combine.
func throttled(d Delivery, facts ContactMethodTypeFacts) bool {
	return facts.CombinesMessages && (d.Purpose == "subscribed_alert" || d.Purpose == "system_notice") && d.Severity != "sos"
}

// applyDeliveryThrottle adjusts a delivery that is about to be queued. It may
// join a message already waiting for the same contact method (the delivery
// then finishes at once, so throttling never piles up pending deliveries), or
// wait until the contact method may get its next message.
func applyDeliveryThrottle(ctx context.Context, tx *sql.Tx, d *Delivery, facts ContactMethodTypeFacts) error {
	if d.State != "queued" {
		return nil
	}
	if !facts.CombinesMessages {
		if d.Severity == "sos" {
			return nil
		}
		minute := d.CreatedAt.Unix() / 60 * 60
		var count int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE contact_method_id=? AND purpose IN ('subscribed_alert','system_notice') AND severity!='sos' AND state!='suppressed' AND created_at>=? AND created_at<?`, d.ContactMethodID, minute, minute+60).Scan(&count)
		if err != nil {
			return err
		}
		if count >= webhookMaximumAlertsPerMinute {
			d.State, d.Reason, d.NextAttempt = "suppressed", fmt.Sprintf(webhookTooManyAlertsReason, webhookMaximumAlertsPerMinute), time.Time{}
		}
		return nil
	}
	if d.Severity == "sos" && facts.ClaimMethod == "button" && (d.Purpose == "subscribed_alert" || d.Purpose == "system_notice") {
		return applyChatSOSRule(ctx, tx, d)
	}
	if d.Purpose == "sos_escalation" && facts.CombinesSOSEscalation {
		return applySOSEscalationCombining(ctx, tx, d)
	}
	if !throttled(*d, facts) {
		return nil
	}
	// A server replacement's START warn goes at once: it tells the person
	// when they will be woken, and it is one message per replacement, so it
	// cannot be the blast the throttle guards against.
	if startWarn, err := startWarnOfAServerReplacement(ctx, tx, d.AlertID); err != nil || startWarn {
		return err
	}
	// So does the resolved message of an SOS that waited for a server
	// replacement: it is the end the START warn announced.
	if d.Severity == "resolved-sos" && d.IncidentID != "" {
		if ended, err := wasHeldForAServerReplacement(ctx, tx, d.IncidentID); err != nil || ended {
			return err
		}
	}
	var carrier string
	err := tx.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE contact_method_id=? AND state='queued' AND attempts=0 AND combined_into='' AND `+throttledDeliveryCondition+` ORDER BY created_at,id LIMIT 1`, d.ContactMethodID).Scan(&carrier)
	if err == nil {
		d.State, d.CombinedInto, d.Reason, d.NextAttempt = "combined", carrier, combinedIntoAnotherMessageReason, time.Time{}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var sent int
	var last int64
	err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(MAX(first_attempt),0) FROM deliveries WHERE contact_method_id=? AND combined_into='' AND first_attempt>? AND `+throttledDeliveryCondition, d.ContactMethodID, d.CreatedAt.Add(-time.Hour).Unix()).Scan(&sent, &last)
	if err != nil {
		return err
	}
	if last == 0 {
		return nil
	}
	interval := contactMethodMinimumMessageInterval
	if sent >= contactMethodSlowDownAfterMessagesPerHour {
		interval = contactMethodSlowedMessageInterval
	}
	if next := time.Unix(last, 0).Add(interval); next.After(d.NextAttempt) {
		d.NextAttempt = next
		d.Reason = fmt.Sprintf(waitingToCombineReason, displayTime(next))
	}
	return nil
}

// applyChatSOSRule keeps an SOS storm readable in a chat (a contact method type whose
// claim method is a button: Slack, Telegram). The first 10 SOS messages in 10
// minutes to one contact method go one by one with their buttons. After that
// the SOS messages join one "{N} more SOS incidents" message. The first such
// message goes at once, so an SOS is never held back; a later one waits at
// most contact_method_minimum_message_interval after the previous one, so a
// storm gives at most one of them a minute.
func applyChatSOSRule(ctx context.Context, tx *sql.Tx, d *Delivery) error {
	var oneByOne int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE contact_method_id=? AND `+chatSOSMessageCondition+` AND more_sos_incidents_message=0 AND combined_into='' AND state NOT IN ('suppressed','cancelled','combined') AND created_at>?`, d.ContactMethodID, d.CreatedAt.Add(-chatSOSMessagesWindow).Unix()).Scan(&oneByOne)
	if err != nil || oneByOne < chatSOSMessagesSentOneByOne {
		return err
	}
	var carrier string
	err = tx.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE contact_method_id=? AND more_sos_incidents_message=1 AND state='queued' AND attempts=0 ORDER BY created_at,id LIMIT 1`, d.ContactMethodID).Scan(&carrier)
	if err == nil {
		d.State, d.CombinedInto, d.Reason, d.NextAttempt = "combined", carrier, combinedIntoAnotherMessageReason, time.Time{}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	d.MoreSOSIncidentsMessage = true
	var last int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(first_attempt),0) FROM deliveries WHERE contact_method_id=? AND more_sos_incidents_message=1`, d.ContactMethodID).Scan(&last); err != nil {
		return err
	}
	if next := time.Unix(last, 0).Add(contactMethodMinimumMessageInterval); last != 0 && next.After(d.NextAttempt) {
		d.NextAttempt = next
		d.Reason = fmt.Sprintf(waitingToCombineReason, displayTime(next))
	}
	return nil
}

// applySOSEscalationCombining keeps SOS escalation messages to a contact method type
// that combines them (phone call) to one message at a time for each
// contact method. A new one joins a message still waiting for the same
// contact method, so one phone call is about every SOS incident
// waiting for that person; otherwise it waits until the contact method type allows the
// next message (the call to this number has ended, a minute since the last
// SOS calls).
func applySOSEscalationCombining(ctx context.Context, tx *sql.Tx, d *Delivery) error {
	var carrier string
	err := tx.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE contact_method_id=? AND purpose='sos_escalation' AND state='queued' AND attempts=0 AND combined_into='' ORDER BY created_at,id LIMIT 1`, d.ContactMethodID).Scan(&carrier)
	if err == nil {
		d.State, d.CombinedInto, d.Reason, d.NextAttempt = "combined", carrier, combinedIntoAnotherMessageReason, time.Time{}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	contactMethodType, _ := knownContactMethodType(d.ContactMethodType)
	pacer, ok := contactMethodType.(deliveryPacer)
	if !ok {
		return nil
	}
	wait, reason, _, err := pacer.PaceDelivery(ctx, tx, *d, d.CreatedAt, false)
	if err != nil {
		return err
	}
	if wait.After(d.NextAttempt) {
		d.NextAttempt, d.Reason = wait, reason
	}
	return nil
}

// moreSOSIncidentsText is the chat message that stands for the SOS messages
// beyond the first 10 in 10 minutes.
func moreSOSIncidentsText(n int, publicURL string) (string, string) {
	incidents := fmt.Sprintf("%d more SOS incidents", n)
	if n == 1 {
		incidents = "1 more SOS incident"
	}
	return "[MMHotline] " + incidents, incidents + ". Open MMHotline: " + publicURL + "/dashboard"
}

// claimButtons decides whether a message carries "Claim this incident" and
// "Resolve": only on a contact method type whose claim method is a button, only for an SOS
// message about one incident, and never on a combined message.
func claimButtons(d Delivery, facts ContactMethodTypeFacts, combined []CombinedAlert) bool {
	return facts.ClaimMethod == "button" && d.Severity == "sos" && d.IncidentID != "" && len(combined) == 0 && !d.MoreSOSIncidentsMessage &&
		(d.Purpose == "subscribed_alert" || d.Purpose == "system_notice" || d.Purpose == "sos_escalation")
}

// CombinedAlert is one alert inside a combined message.
type CombinedAlert struct {
	Severity, Title, AlertID, IncidentID string
	// PassedOverForTheRunLimit: the triage agent's run limit sent the alert
	// straight to people, so the message starts with runLimitPassedOverLine.
	PassedOverForTheRunLimit bool
}

// anyPassedOverForTheRunLimit says a combined message carries an alert the
// run limit passed over.
func anyPassedOverForTheRunLimit(alerts []CombinedAlert) bool {
	return slices.ContainsFunc(alerts, func(a CombinedAlert) bool { return a.PassedOverForTheRunLimit })
}

// combinedAlerts returns the alerts a waiting message carries, its own first,
// and on its first attempt marks the others as sent together with it.
func combinedAlerts(ctx context.Context, tx *sql.Tx, d Delivery) ([]CombinedAlert, error) {
	// A message that left (its incident was claimed while this one waited)
	// is not listed.
	const passedOver = `coalesce((SELECT e.passed_over_for_the_run_limit FROM alerts e WHERE e.id=d.alert_id AND d.severity IN ('warn','sos')),0)`
	rows, err := tx.QueryContext(ctx, `SELECT d.severity,d.title,d.alert_id,d.incident_id,`+passedOver+` FROM deliveries d WHERE d.combined_into=? AND d.state='combined' ORDER BY d.created_at,d.id`, d.ID)
	if err != nil {
		return nil, err
	}
	var joined []CombinedAlert
	for rows.Next() {
		var c CombinedAlert
		if err = rows.Scan(&c.Severity, &c.Title, &c.AlertID, &c.IncidentID, &c.PassedOverForTheRunLimit); err != nil {
			rows.Close()
			return nil, err
		}
		joined = append(joined, c)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(joined) == 0 {
		return nil, err
	}
	if d.Attempts == 1 {
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET reason=? WHERE combined_into=? AND state='combined'`, fmt.Sprintf(sentTogetherReason, countText(len(joined), "other alert", "other alerts")), d.ID); err != nil {
			return nil, err
		}
	}
	own := CombinedAlert{Severity: d.Severity, Title: d.Title, AlertID: d.AlertID, IncidentID: d.IncidentID}
	if d.Severity == "warn" || d.Severity == "sos" {
		if own.PassedOverForTheRunLimit, err = alertPassedOverForTheRunLimit(ctx, tx, d.AlertID); err != nil {
			return nil, err
		}
	}
	return append([]CombinedAlert{own}, joined...), nil
}

// combinedMessageText is the plain rendering of a combined message, used by
// email and the delivery page. The newest alert comes first.
func combinedMessageText(alerts []CombinedAlert, publicURL string) (string, string) {
	subject := fmt.Sprintf("[MMHotline] %d new alerts", len(alerts))
	var b strings.Builder
	if anyPassedOverForTheRunLimit(alerts) {
		b.WriteString(runLimitPassedOverLine + "\n\n")
	}
	fmt.Fprintf(&b, "MMHotline combined %d alerts for this contact method into one message.\n\n", len(alerts))
	for i := len(alerts) - 1; i >= 0 && len(alerts)-i <= maxAlertsListedInCombinedMessage; i-- {
		a := alerts[i]
		fmt.Fprintf(&b, "%s: %s\n", severityLabel(a.Severity), a.Title)
		switch {
		case a.IncidentID != "":
			fmt.Fprintf(&b, "%s/incidents/%s\n", publicURL, a.IncidentID)
		case a.AlertID != "":
			fmt.Fprintf(&b, "%s/alerts/%s\n", publicURL, a.AlertID)
		}
		b.WriteString("\n")
	}
	if len(alerts) > maxAlertsListedInCombinedMessage {
		fmt.Fprintf(&b, "And %d more.\n\n", len(alerts)-maxAlertsListedInCombinedMessage)
	}
	fmt.Fprintf(&b, "See all: %s/alerts\n", publicURL)
	return subject, b.String()
}
