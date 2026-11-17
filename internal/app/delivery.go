package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const maxPendingDeliveries = 10000
const maxDeliveryAttempts = 8
const retryHorizon = 23 * time.Hour

const reservationDuration = 30 * time.Second

// Delivery is one notification to one contact method. Subject and Message are
// the plain rendering a person reads (email and the delivery page); Title,
// AlertMessage, NotesFromMmhotline, IncidentStatus, and AlertReceivedAt are the
// raw facts, copied when the delivery is queued, for contact method types that format
// their own messages.
type Delivery struct {
	ID, NotificationKey, AlertID, IncidentID, ContactMethodID, MemberID, ContactMethodType, SentTo, Purpose, Severity, Subject, Message, State, Reason, IDFromSendingService, SentFrom, PublicURL, Reservation string
	Title, AlertMessage, NotesFromMmhotline, IncidentStatus, CombinedInto                                                                                                                                      string
	AlertReceivedAt                                                                                                                                                                                            time.Time
	ContactMethodRevision, IncidentRevision                                                                                                                                                                    int64
	IgnorePersonal                                                                                                                                                                                             bool
	// MoreSOSIncidentsMessage marks the chat message "{N} more SOS incidents"
	// that carries the SOS messages beyond the first 10 in 10 minutes.
	MoreSOSIncidentsMessage bool
	// CallResult is how a phone call went, such as "answered_no_key"; empty
	// for other contact method types.
	CallResult                                          string
	Attempts                                            int
	CreatedAt, NextAttempt, FirstAttempt, ReservedUntil time.Time
}
type DeliveryAttempt struct {
	ID, DeliveryID, State, IDFromSendingService, Diagnostic string
	Number                                                  int
	StartedAt, CompletedAt                                  time.Time
}

const deliveryColumns = `id,notification_key,alert_id,incident_id,incident_revision,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,ignore_personal,state,reason,created_at,next_attempt,attempts,first_attempt,reservation,reserved_until,id_from_sending_service,sent_from,public_url,title,alert_message,notes_from_mmhotline,incident_status,alert_received_at,combined_into,more_sos_incidents_message,call_result`

func scanDelivery(row interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	var created, next, first, until, received int64
	err := row.Scan(&d.ID, &d.NotificationKey, &d.AlertID, &d.IncidentID, &d.IncidentRevision, &d.ContactMethodID, &d.ContactMethodRevision, &d.MemberID, &d.ContactMethodType, &d.SentTo, &d.Purpose, &d.Severity, &d.Subject, &d.Message, &d.IgnorePersonal, &d.State, &d.Reason, &created, &next, &d.Attempts, &first, &d.Reservation, &until, &d.IDFromSendingService, &d.SentFrom, &d.PublicURL, &d.Title, &d.AlertMessage, &d.NotesFromMmhotline, &d.IncidentStatus, &received, &d.CombinedInto, &d.MoreSOSIncidentsMessage, &d.CallResult)
	d.AlertReceivedAt = deadlineTime(received)
	d.CreatedAt = deadlineTime(created)
	d.NextAttempt = deadlineTime(next)
	d.FirstAttempt = deadlineTime(first)
	d.ReservedUntil = deadlineTime(until)
	return d, err
}
func insertDelivery(ctx context.Context, tx *sql.Tx, d Delivery) error {
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE notification_key=? AND contact_method_id=?`, d.NotificationKey, d.ContactMethodID).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		return nil
	}
	if d.State == "queued" && !strings.HasPrefix(d.NotificationKey, "contact_method_saved/") {
		if err = checkPendingDeliveryLimits(ctx, tx, d); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deliveries(id,notification_key,alert_id,incident_id,incident_revision,contact_method_id,contact_method_revision,member_id,contact_method_type,sent_to,purpose,severity,subject,message,title,alert_message,notes_from_mmhotline,incident_status,alert_received_at,ignore_personal,state,reason,created_at,next_attempt,combined_into,more_sos_incidents_message) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(notification_key,contact_method_id) DO NOTHING`, d.ID, d.NotificationKey, d.AlertID, d.IncidentID, d.IncidentRevision, d.ContactMethodID, d.ContactMethodRevision, d.MemberID, d.ContactMethodType, d.SentTo, d.Purpose, d.Severity, d.Subject, d.Message, d.Title, d.AlertMessage, d.NotesFromMmhotline, d.IncidentStatus, deadlineUnix(d.AlertReceivedAt), d.IgnorePersonal, d.State, d.Reason, deadlineUnix(d.CreatedAt), deadlineUnix(d.NextAttempt), d.CombinedInto, d.MoreSOSIncidentsMessage)
	return err
}

// checkPendingDeliveryLimits refuses a new queued delivery when too many
// deliveries wait to be sent. Ordinary deliveries stop short of the part
// reserved for SOS. An alert source's own SOS alerts stop at its share, so
// one alert source cannot take the room other alert sources' SOS need.
func checkPendingDeliveryLimits(ctx context.Context, tx *sql.Tx, d Delivery) error {
	sos := d.Severity == "sos" || d.Purpose == "sos_escalation"
	var sourceID, actor string
	if sos && d.AlertID != "" {
		err := tx.QueryRowContext(ctx, `SELECT source_id,actor FROM alerts WHERE id=?`, d.AlertID).Scan(&sourceID, &actor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	// MMHotline's own SOS says that an alert source stopped checking in, or
	// that MMHotline refuses an alert source's SOS alerts. A full queue is
	// when that matters most, so no limit refuses it. It stays bounded: at
	// most one is unresolved per alert source, and while a message to the
	// person who claimed it waits, a new one adds no other (admitAlert).
	if sourceID == "system" && actor == "system" {
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE state='sending' OR `+pendingDelivery).Scan(&count); err != nil {
		return err
	}
	limit := maxPendingDeliveries - sosReservedPendingDeliveries
	if sos {
		limit = maxPendingDeliveries
	}
	if count >= limit {
		return errPendingDeliveryFull
	}
	// The share applies to the SOS alerts a sender sends. An SOS escalation
	// is for an incident already accepted, so it counts but is never
	// refused. MMHotline's other SOS alerts, such as the triage agent's,
	// are not from an alert source and keep only the installation limit.
	if !sos || d.Purpose != "subscribed_alert" || sourceID == "" || sourceID == "system" {
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries d JOIN alerts a ON a.id=d.alert_id WHERE a.source_id=? AND (d.severity='sos' OR d.purpose='sos_escalation') AND (d.state='sending' OR `+pendingDelivery+`)`, sourceID).Scan(&count); err != nil {
		return err
	}
	if count >= maxPendingSOSDeliveriesPerAlertSource {
		return errAlertSourcePendingSOSFull
	}
	return nil
}

// QueueNotification appends intent to the caller's transaction. It never commits
// or performs external work, so admission cannot outlive a failed queue write.
func QueueNotification(ctx context.Context, tx *sql.Tx, n Notification) error {
	base := baseNotificationSeverity(n.Severity)
	if n.Key == "" || n.At.IsZero() || (base != "info" && base != "warn" && base != "sos") {
		return ErrInvalid
	}
	if n.Purpose == "" {
		n.Purpose = "subscribed_alert"
	}
	if n.Purpose != "subscribed_alert" && n.Purpose != "sos_escalation" && n.Purpose != "system_notice" {
		return ErrInvalid
	}
	if n.Purpose == "sos_escalation" && (len(n.Recipients) == 0 || n.IncidentID == "" || n.IncidentRevision < 1) {
		return ErrInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.`+strings.ReplaceAll(contactMethodColumns, ",", ",c.")+` FROM contact_methods c JOIN contact_method_subscriptions s ON s.contact_method_id=c.id WHERE c.deleted=0 AND s.severity=? ORDER BY c.id`, base)
	if err != nil {
		return err
	}
	var contactMethods []ContactMethod
	for rows.Next() {
		c, e := scanContactMethod(rows)
		if e != nil {
			rows.Close()
			return e
		}
		contactMethods = append(contactMethods, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var incidentStatus string
	var alertReceivedAt time.Time
	if n.IncidentID != "" {
		err = tx.QueryRowContext(ctx, `SELECT status FROM incidents WHERE id=?`, n.IncidentID).Scan(&incidentStatus)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if n.AlertID != "" {
		var received int64
		err = tx.QueryRowContext(ctx, `SELECT received_at FROM alerts WHERE id=?`, n.AlertID).Scan(&received)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		alertReceivedAt = deadlineTime(received)
	}
	// A triage agent report's notifications carry its summary, not the report
	// (triageAgentReportNotificationMessage). An incident's notifications carry
	// the message of the alert that opened it, which for a resolved incident
	// is not n.AlertID, the alert that resolved it.
	messageAlertID := n.AlertID
	if n.Severity == "resolved-sos" && n.IncidentID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT opening_alert_id FROM incidents WHERE id=?`, n.IncidentID).Scan(&messageAlertID); err != nil {
			return err
		}
	}
	if messageAlertID != "" {
		var actor, message string
		err := tx.QueryRowContext(ctx, `SELECT actor,message FROM alerts WHERE id=?`, messageAlertID).Scan(&actor, &message)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if actor == "triage_agent" {
			n.Message = triageAgentReportNotificationMessage(message)
		}
	}
	for _, c := range contactMethods {
		if len(n.Recipients) > 0 && !containsMember(n.Recipients, c.MemberID) {
			continue
		}
		// An unknown contact method type is recorded as not sent; it never blocks admission.
		facts, known := contactMethodTypeFacts(c.ContactMethodType)
		if n.Purpose == "sos_escalation" && !facts.WakesPeople {
			continue
		}
		if n.PeopleOnly && facts.ForMachines {
			continue
		}
		// A retry is still the same round. Do not accumulate repeated SOS escalation
		// rounds behind pending or in-flight work for the same current target.
		if n.Purpose == "sos_escalation" {
			var existing int
			// A message that joined another one still waiting counts too.
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE incident_id=? AND incident_revision=? AND contact_method_id=? AND purpose='sos_escalation' AND (state='sending' OR `+pendingDelivery+` OR `+waitingJoiner+`)`, n.IncidentID, n.IncidentRevision, c.ID).Scan(&existing)
			if err != nil {
				return err
			}
			if existing > 0 {
				continue
			}
			contactMethodType, _ := knownContactMethodType(c.ContactMethodType)
			if sender, ok := contactMethodType.(ringingEmergencySender); ok {
				ringing, err := sender.escalationRoundStillRinging(ctx, tx, c.ID, n.IncidentID, n.At)
				if err != nil {
					return err
				}
				if ringing {
					continue
				}
			}
		}
		reason := ""
		if !known {
			reason = "this kind of contact method is not available"
		}
		if n.Purpose != "sos_escalation" && n.Severity == "sos" && facts.WakesPeople && !n.WakesRecipients {
			reason = "SOS contact method type follows the active target"
		}
		if facts.SOSOnly && n.Severity != "sos" {
			reason = "this contact method is only for SOS"
		}
		if reason == "" && n.Severity == "resolved-sos" && n.IncidentID != "" {
			told, e := contactMethodWasToldAboutIncident(ctx, tx, n.IncidentID, c.ID)
			if e != nil {
				return e
			}
			if !told {
				reason = nothingAboutTheIncidentWasSentReason
			}
		}
		if reason == "" {
			allowed, why, e := DeliveryAllowed(ctx, tx, c.MemberID, c.ContactMethodType, c.SendWhileUnavailableOrMuted, n.IgnorePersonal, n.At)
			if e != nil {
				return e
			}
			if !allowed {
				reason = why
			}
		}
		body := fmt.Sprintf("%s: %s\n\n%s\n\nAt: %s", severityLabel(n.Severity), n.Title, n.Message, n.At.UTC().Format(time.RFC3339))
		if gap := happenedGap(n.HappenedAt, n.At); gap != "" {
			body += "\n" + gap
		}
		if n.Notes != "" {
			body += "\nNotes: " + n.Notes
		}
		switch {
		case n.Severity == "sos" && incidentStatus == "claimed":
			// Such as a new SOS that joined a claimed incident: nobody needs to claim it.
			body += "\nSomeone has claimed this SOS incident. Open the incident to see the new alert, or to resolve the incident."
		case n.Severity == "sos":
			body += "\nAn SOS incident needs a person now. Open the incident to claim it, which assigns it to you and stops MMHotline contacting others, or to resolve it."
		}
		d := Delivery{ID: NewID(), NotificationKey: n.Key, AlertID: n.AlertID, IncidentID: n.IncidentID, IncidentRevision: n.IncidentRevision, ContactMethodID: c.ID, ContactMethodRevision: c.Revision, MemberID: c.MemberID, ContactMethodType: c.ContactMethodType, SentTo: c.SentTo, Purpose: n.Purpose, Severity: n.Severity, Subject: "[" + severityLabel(n.Severity) + "] " + n.Title, Message: body, Title: n.Title, AlertMessage: n.Message, NotesFromMmhotline: n.Notes, IncidentStatus: incidentStatus, AlertReceivedAt: alertReceivedAt, IgnorePersonal: n.IgnorePersonal, State: "queued", CreatedAt: n.At, NextAttempt: n.At}
		if reason != "" {
			d.State = "suppressed"
			d.Reason = reason
			d.NextAttempt = time.Time{}
		}
		if err = applyDeliveryThrottle(ctx, tx, &d, facts); err != nil {
			return err
		}
		if err = insertDelivery(ctx, tx, d); err != nil {
			return err
		}
	}
	return nil
}
func CancelIncidentSOSEscalation(ctx context.Context, tx *sql.Tx, id, reason string) error {
	// A message that joined another one still waiting leaves it.
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='cancelled',reason=?,next_attempt=0 WHERE incident_id=? AND purpose='sos_escalation' AND `+waitingJoiner, reason, id); err != nil {
		return err
	}
	// The incident's own messages that others joined (a phone call about
	// several incidents, waiting or waiting for a retry) give the others a
	// message again.
	return cancelDeliveries(ctx, tx, `incident_id=? AND purpose='sos_escalation' AND `+pendingDelivery, reason, id)
}

// nothingAboutTheIncidentWasSentReason is why a resolved message to a contact
// method is not sent: it ends something the person was told, and MMHotline
// sent this contact method nothing about the incident.
const nothingAboutTheIncidentWasSentReason = "nothing about the incident was sent to this contact method"

// attemptedDelivery is the condition for a delivery MMHotline tried to send
// or still means to send: it was not cancelled or suppressed before sending.
// A delivery that was attempted and then cancelled, such as an SOS message
// waiting for a retry when the incident resolved, still counts, because the
// person may have been reached.
const attemptedDelivery = `(attempts>0 OR state NOT IN ('suppressed','cancelled'))`

// contactMethodWasToldAboutIncident says whether MMHotline sent the contact
// method an SOS or a warning about the incident: the SOS itself, an
// escalation, or a warning on the incident such as a server replacement's
// start. Info is not counted: the quiet note that the triage agent is looking
// into an SOS says nothing is needed, so its end needs no message.
func contactMethodWasToldAboutIncident(ctx context.Context, tx *sql.Tx, incidentID, contactMethodID string) (bool, error) {
	var told bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deliveries WHERE incident_id=? AND contact_method_id=? AND severity IN ('sos','warn') AND `+attemptedDelivery+`)`, incidentID, contactMethodID).Scan(&told)
	return told, err
}

// pendingDelivery is the condition for a delivery that will still be tried:
// waiting for its first attempt, or for a retry.
const pendingDelivery = `(state='queued' OR (state IN ('failed','uncertain') AND next_attempt>0))`

// waitingJoiner is the condition for a delivery combined into a message
// that has not been attempted yet. Once the message is attempted, the
// delivery is sent together with it, or ends with it (carrierEnded).
const waitingJoiner = `(state='combined' AND combined_into IN (SELECT id FROM deliveries WHERE state='queued' AND attempts=0))`

// cancelDeliveries cancels the pending deliveries that match where, and
// settles the messages combined into each of them.
func cancelDeliveries(ctx context.Context, tx *sql.Tx, where, reason string, args ...any) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM deliveries WHERE `+where, args...)
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
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='cancelled',reason=?,next_attempt=0 WHERE `+where, append([]any{reason}, args...)...); err != nil {
		return err
	}
	for _, id := range ids {
		if err = carrierEnded(ctx, tx, id, "cancelled", reason); err != nil {
			return err
		}
	}
	return nil
}

// joinerEndedReasons say why a message combined into another one was not
// sent, by the final state of the message it joined.
var joinerEndedReasons = map[string]string{
	"failed":     "Not sent: the message it was combined into failed",
	"uncertain":  "The message it was combined into may not have arrived",
	"cancelled":  "Not sent: the message it was combined into was cancelled",
	"suppressed": "Not sent: the message it was combined into was suppressed",
}

// carrierEnded settles the messages combined into a message that ended
// without being sent: finalState is failed, uncertain, cancelled, or
// suppressed, with the message's reason. A cancelled or suppressed message
// may have been about another incident than the SOS escalation messages that
// joined it, so they get a message of their own again. The other joined
// messages end with the same state. They keep combined_into, which links
// them to the message they joined.
func carrierEnded(ctx context.Context, tx *sql.Tx, carrierID, finalState, reason string) error {
	if finalState == "cancelled" || finalState == "suppressed" {
		if err := promoteOrphanedSOSJoiners(ctx, tx, carrierID); err != nil {
			return err
		}
	}
	text := joinerEndedReasons[finalState]
	if reason = (Delivery{Reason: reason}).ReasonText(); reason != "" {
		text += ": " + reason
	}
	// A joined message an unknown outcome left uncertain follows the
	// message's later, definite outcome.
	_, err := tx.ExecContext(ctx, `UPDATE deliveries SET state=?,reason=?,next_attempt=0 WHERE combined_into=? AND state IN ('combined','uncertain')`, finalState, truncateRunes(text, 512), carrierID)
	return err
}

// promoteOrphanedSOSJoiners gives the SOS escalation messages that joined
// messages which will now never be sent, or never sent again after a failed
// attempt (cancelled or suppressed), a message of their own: for each, the
// oldest joiner becomes the new message, due at once (reservation checks the
// contact method type's pacing again), and the others join it. A joiner of a message
// that was attempted may be sent twice; a duplicate call is preferable to dropping an alert.
func promoteOrphanedSOSJoiners(ctx context.Context, tx *sql.Tx, carriers ...string) error {
	for _, carrier := range carriers {
		rows, err := tx.QueryContext(ctx, `SELECT id,created_at FROM deliveries WHERE combined_into=? AND state='combined' AND purpose='sos_escalation' ORDER BY created_at,id`, carrier)
		if err != nil {
			return err
		}
		var ids []string
		var due int64
		for rows.Next() {
			var id string
			var created int64
			if err = rows.Scan(&id, &created); err != nil {
				rows.Close()
				return err
			}
			if len(ids) == 0 {
				due = created
			}
			ids = append(ids, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(ids) == 0 {
			if err != nil {
				return err
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='queued',combined_into='',reason='',next_attempt=? WHERE id=?`, due, ids[0]); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET combined_into=?,reason=? WHERE combined_into=? AND state='combined'`, ids[0], combinedIntoAnotherMessageReason, carrier); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) Deliveries(ctx context.Context, p Principal, limit int) ([]Delivery, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	var out []Delivery
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries ORDER BY created_at DESC,id DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, e := scanDelivery(rows)
			if e != nil {
				return e
			}
			if d.MemberID != "" && d.MemberID != m.ID && m.Role != RoleAdministrator {
				d.SentTo = "private contact method"
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}
func (a *App) DeliveryByID(ctx context.Context, p Principal, id string) (Delivery, error) {
	var d Delivery
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		d, err = scanDelivery(tx.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if d.MemberID != "" && d.MemberID != m.ID && m.Role != RoleAdministrator {
			d.SentTo = "private contact method"
		}
		return nil
	})
	return d, err
}

func (a *App) DeliveryAttempts(ctx context.Context, p Principal, id string) ([]DeliveryAttempt, error) {
	var out []DeliveryAttempt
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,delivery_id,number,started_at,completed_at,state,id_from_sending_service,diagnostic FROM delivery_attempts WHERE delivery_id=? ORDER BY number`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DeliveryAttempt
			var start, done int64
			if err = rows.Scan(&d.ID, &d.DeliveryID, &d.Number, &start, &done, &d.State, &d.IDFromSendingService, &d.Diagnostic); err != nil {
				return err
			}
			d.StartedAt = deadlineTime(start)
			d.CompletedAt = deadlineTime(done)
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

const (
	reservationExpiredReason = "worker reservation expired; outcome unknown"
	processRestartedReason   = "MMHotline restarted during this attempt; outcome unknown"
)

// recoverDeliveries records as uncertain the attempts whose worker will not
// save a result, and says when each delivery is tried again.
//
// On a visit like any other, those are the reservations that ran out. Only
// contact method types that may resend after an unknown outcome retry on their next reservation. A phone call does
// not: the next repeat is the retry.
//
// processRestarted is set on the first reservation for a team after
// mmhotline serve starts. Previous workers are gone, so reservations do not
// wait for their reservation to expire. A phone call is made again at once: a duplicate
// is better than a dropped alert.
func recoverDeliveries(ctx context.Context, tx *sql.Tx, now time.Time, processRestarted bool) (bool, error) {
	reserved, reason := fmt.Sprintf("state='sending' AND reserved_until<=%d", now.Unix()), reservationExpiredReason
	// When a contact method type that does not resend after an unknown outcome is tried
	// again: 0 is never.
	withoutResend := int64(0)
	if processRestarted {
		reserved, reason, withoutResend = "state='sending'", processRestartedReason, now.Unix()
	}
	_, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET state='uncertain',completed_at=?,diagnostic=? WHERE state='sending' AND id IN (SELECT reservation FROM deliveries WHERE `+reserved+`)`, now.Unix(), reason)
	if err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE `+reserved)
	if err != nil {
		return false, err
	}
	var expired []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			rows.Close()
			return false, err
		}
		expired = append(expired, d)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(expired) == 0 {
		return false, err
	}
	for _, d := range expired {
		next := int64(0)
		if d.Attempts < maxDeliveryAttempts && now.Before(d.FirstAttempt.Add(retryHorizon)) {
			next = withoutResend
			if c, known := knownContactMethodType(d.ContactMethodType); known && c.Facts().RetryUncertain {
				next = now.Unix()
				if sender, ok := c.(ringingEmergencySender); ok && !sender.resendsAfterUnknownOutcome(d) {
					next = 0
				}
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='uncertain',reason=?,reservation='',reserved_until=0,next_attempt=? WHERE id=?`, reason, next, d.ID); err != nil {
			return false, err
		}
	}
	// A combined message that will not be tried again ended with an unknown
	// outcome, and the messages that joined it with it.
	ended, err := tx.QueryContext(ctx, `SELECT id,reason FROM deliveries WHERE state='uncertain' AND next_attempt=0 AND id IN (SELECT combined_into FROM deliveries WHERE state='combined')`)
	if err != nil {
		return false, err
	}
	var carriers [][2]string
	for ended.Next() {
		var carrier [2]string
		if err = ended.Scan(&carrier[0], &carrier[1]); err != nil {
			ended.Close()
			return false, err
		}
		carriers = append(carriers, carrier)
	}
	if err = errors.Join(ended.Err(), ended.Close()); err != nil {
		return false, err
	}
	for _, carrier := range carriers {
		if err = carrierEnded(ctx, tx, carrier[0], "uncertain", carrier[1]); err != nil {
			return false, err
		}
	}
	return true, nil
}

// nextDeliveryDeadline returns when the team's next delivery work is due: the
// next attempt of a delivery that reserveDelivery would pick for one of the
// contact method types that can send, or the end of a reservation that recoverDeliveries
// would then record as uncertain.
func nextDeliveryDeadline(ctx context.Context, tx *sql.Tx, kinds []string) (time.Time, error) {
	var reserved, attempt int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(reserved_until),0) FROM deliveries WHERE state='sending'`).Scan(&reserved); err != nil {
		return time.Time{}, err
	}
	if len(kinds) > 0 {
		args := []any{}
		for _, kind := range kinds {
			args = append(args, kind)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(next_attempt),0) FROM deliveries WHERE contact_method_type IN (?`+strings.Repeat(",?", len(kinds)-1)+`) AND `+pendingDelivery+` AND next_attempt>0`, args...).Scan(&attempt); err != nil {
			return time.Time{}, err
		}
	}
	return earliest(deadlineTime(reserved), deadlineTime(attempt)), nil
}

// deliveryEligible reports whether a delivery may still be sent: its contact
// method still wants it, and an SOS escalation is still valid. A delivery
// that may not be sent is cancelled or suppressed, with the reason.
func deliveryEligible(ctx context.Context, tx *sql.Tx, d Delivery, at time.Time) (bool, string, string, error) {
	ok, state, reason, err := contactMethodStillWants(ctx, tx, d, at)
	if err != nil || state == "cancelled" {
		return ok, state, reason, err
	}
	// An SOS escalation that is no longer valid is cancelled, even when the
	// person's or the team's controls would also suppress it.
	if d.Purpose == "sos_escalation" {
		valid, why, err := ValidateSOSEscalation(ctx, tx, d.IncidentID, d.IncidentRevision, d.MemberID, at)
		if err != nil {
			return false, "", "", err
		}
		if !valid {
			return false, "cancelled", why, nil
		}
	}
	return ok, state, reason, nil
}

// contactMethodStillWants reports whether the delivery's contact method still
// wants it. The delivery is cancelled when the contact method was removed or
// its destination changed (SaveContactMethod keeps a delivery on the current
// revision for any other change), or its member was removed. It is suppressed when the person's or
// the team's controls stop it (DeliveryAllowed). Pushover also stops an
// emergency that already rings for these reasons.
func contactMethodStillWants(ctx context.Context, tx *sql.Tx, d Delivery, at time.Time) (bool, string, string, error) {
	c, err := contactMethodByID(ctx, tx, d.ContactMethodID)
	if errors.Is(err, ErrNotFound) {
		return false, "cancelled", "contact method removed", nil
	}
	if err != nil {
		return false, "", "", err
	}
	if c.Revision != d.ContactMethodRevision || c.SentTo != d.SentTo {
		return false, "cancelled", "contact method settings changed", nil
	}
	if c.MemberID != "" {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM members WHERE id=? AND removed_at IS NULL`, c.MemberID).Scan(&exists); err != nil {
			return false, "", "", err
		}
		if exists != 1 {
			return false, "cancelled", "member removed", nil
		}
	}
	allowed, reason, err := DeliveryAllowed(ctx, tx, c.MemberID, c.ContactMethodType, c.SendWhileUnavailableOrMuted, d.IgnorePersonal, at)
	return allowed, "suppressed", reason, err
}

// reserveDelivery reserves the next due delivery for one of the contact method types that
// can send now. With no contact method type it only recovers expired reservations.
//
// On the first reservation for a team after mmhotline serve starts, recover
// the previous process's unfinished work in the same transaction, before
// sending anything. Retry reserved deliveries (recoverDeliveries), and resume polling calls with known sending service IDs.
func (a *App) reserveDelivery(ctx context.Context, publicURL string, kinds []string) (Delivery, bool, error) {
	var d Delivery
	var work bool
	var reserveFailure error
	now := a.Now()
	takeOver := !a.teamDeadline.hasTakenOverUnfinishedDeliveries()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		reserveFailure = nil
		work, err = recoverDeliveries(ctx, tx, now, takeOver)
		if err != nil || len(kinds) == 0 {
			return err
		}
		args := []any{}
		for _, kind := range kinds {
			args = append(args, kind)
		}
		args = append(args, now.Unix())
		// SOS incidents go first so an SOS never waits behind a backlog of
		// ordinary notices from the same team. Among SOS deliveries, the
		// alert source with the fewest due goes first, so one alert source's
		// flood never pushes another alert source's SOS behind its backlog.
		d, err = scanDelivery(tx.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM (SELECT d.*,(d.severity='sos' OR d.purpose='sos_escalation') AS sos,count(*) OVER (PARTITION BY (d.severity='sos' OR d.purpose='sos_escalation'),(SELECT a.source_id FROM alerts a WHERE a.id=d.alert_id)) AS due_from_source FROM deliveries d WHERE d.contact_method_type IN (?`+strings.Repeat(",?", len(kinds)-1)+`) AND `+pendingDelivery+` AND d.next_attempt>0 AND d.next_attempt<=?) ORDER BY sos DESC,CASE WHEN sos THEN due_from_source ELSE 0 END,next_attempt,created_at,id LIMIT 1`, args...))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		work = true
		// A delivery that cannot be checked fails with the reason, and the
		// error is reported, so it never holds up the deliveries behind it.
		if _, err = tx.ExecContext(ctx, `SAVEPOINT reserve_delivery`); err != nil {
			return err
		}
		id := d.ID
		if failure := a.reserveDueDelivery(ctx, tx, &d, publicURL, now); failure != nil {
			if ctx.Err() != nil {
				return failure
			}
			if _, err = tx.ExecContext(ctx, `ROLLBACK TO reserve_delivery`); err != nil {
				return errors.Join(failure, err)
			}
			reason := truncateRunes("MMHotline could not check this notification before sending it: "+failure.Error(), 512)
			if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='failed',reason=?,next_attempt=0 WHERE id=?`, reason, id); err != nil {
				return err
			}
			// The SOS messages combined into it get a message of their own.
			if err = promoteOrphanedSOSJoiners(ctx, tx, id); err != nil {
				return err
			}
			if err = carrierEnded(ctx, tx, id, "failed", reason); err != nil {
				return err
			}
			d.ID = ""
			reserveFailure = fmt.Errorf("delivery %s: %w", id, failure)
		}
		_, err = tx.ExecContext(ctx, `RELEASE reserve_delivery`)
		return err
	})
	if err == nil && takeOver {
		a.teamDeadline.tookOverUnfinishedDeliveries()
	}
	return d, work, errors.Join(err, reserveFailure)
}

// reserveDueDelivery reserves d, the next due delivery, for an attempt now,
// or settles it without one: it ends at the retry limit, is cancelled or
// suppressed when it may no longer be sent, or waits for its contact method
// type's pacing. A delivery that is not reserved leaves d.ID empty.
func (a *App) reserveDueDelivery(ctx context.Context, tx *sql.Tx, d *Delivery, publicURL string, now time.Time) error {
	var err error
	if d.Attempts >= maxDeliveryAttempts || (!d.FirstAttempt.IsZero() && !now.Before(d.FirstAttempt.Add(retryHorizon))) {
		reason := d.Reason + "; retry limit reached"
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET next_attempt=0,reason=? WHERE id=?`, reason, d.ID); err != nil {
			return err
		}
		carrier := d.ID
		d.ID = ""
		return carrierEnded(ctx, tx, carrier, d.State, reason)
	}
	allowed, state, reason, err := deliveryEligible(ctx, tx, *d, now)
	if err != nil {
		return err
	}
	if !allowed {
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state=?,reason=?,next_attempt=0 WHERE id=?`, state, reason, d.ID)
		carrier := d.ID
		d.ID = ""
		if err != nil {
			return err
		}
		return carrierEnded(ctx, tx, carrier, state, reason)
	}
	// A contact method type that paces its deliveries (one call to a number at a
	// time, each phone sending service's rate) may make this one wait. Waiting
	// here, before the attempt, lets a claim still cancel it.
	sendingService := ""
	if pacer, ok := a.configuredContactMethodTypes()[d.ContactMethodType].(deliveryPacer); ok {
		var wait time.Time
		wait, reason, sendingService, err = pacer.PaceDelivery(ctx, tx, *d, now, true)
		if err != nil {
			return err
		}
		if wait.After(now) {
			_, err = tx.ExecContext(ctx, `UPDATE deliveries SET next_attempt=?,reason=? WHERE id=?`, wait.Unix(), reason, d.ID)
			d.ID = ""
			return err
		}
	}
	d.Attempts++
	d.Reservation = NewID()
	reservationTimeLimit := reservationDuration
	// A longer contact method type conversation still needs time to record its result.
	if contactMethodType := a.configuredContactMethodTypes()[d.ContactMethodType]; contactMethodType != nil {
		reservationTimeLimit = max(reservationTimeLimit, contactMethodType.Facts().AttemptTimeLimit+15*time.Second)
	}
	d.ReservedUntil = now.Add(reservationTimeLimit)
	d.State = "sending"
	if d.FirstAttempt.IsZero() {
		d.FirstAttempt = now
		d.PublicURL = publicURL
	}
	_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='sending',attempts=?,first_attempt=?,reservation=?,reserved_until=?,public_url=? WHERE id=?`, d.Attempts, d.FirstAttempt.Unix(), d.Reservation, d.ReservedUntil.Unix(), d.PublicURL, d.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,number,started_at,state,contact_method_type,sending_service) VALUES(?,?,?,?,'sending',?,?)`, d.Reservation, d.ID, d.Attempts, now.Unix(), d.ContactMethodType, sendingService)
	return err
}

// DispatchOne reserves and sends at most one delivery through the configured
// contact method types (App.ContactMethodTypes). Deliveries for a contact method type that is not configured stay
// queued, while reservations left by an interrupted worker are still recorded
// as uncertain.
func (a *App) DispatchOne(ctx context.Context) (bool, error) {
	contactMethodTypes := a.configuredContactMethodTypes()
	kinds := slices.Sorted(maps.Keys(contactMethodTypes))
	// serve checked web_url when it started; links are built from it.
	d, work, err := a.reserveDelivery(ctx, strings.TrimRight(a.WebURL, "/"), kinds)
	if err != nil || d.ID == "" {
		return work, err
	}
	return true, a.dispatchReserved(ctx, d, contactMethodTypes[d.ContactMethodType])
}
func (a *App) dispatchReserved(ctx context.Context, d Delivery, contactMethodType ContactMethodType) error {
	// Recheck after reservation. A concurrent mute, claim, resolution, or contact method
	// change may invalidate the copied request before the external attempt begins.
	send := false
	var settings, secret, memberName string
	var resolvesAReplacementSOS bool
	var combined []CombinedAlert
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := recoverDeliveries(ctx, tx, a.Now(), false); err != nil {
			return err
		}
		var reservation, state string
		if err := tx.QueryRowContext(ctx, `SELECT reservation,state FROM deliveries WHERE id=?`, d.ID).Scan(&reservation, &state); err != nil {
			return err
		}
		if reservation != d.Reservation || state != "sending" {
			return nil
		}
		allowed, state, reason, err := deliveryEligible(ctx, tx, d, a.Now())
		if err != nil {
			return err
		}
		if !allowed {
			if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state=?,reason=?,next_attempt=0,reservation='',reserved_until=0 WHERE id=? AND reservation=?`, state, reason, d.ID, d.Reservation); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET state=?,completed_at=?,diagnostic=? WHERE id=?`, state, a.Now().Unix(), reason, d.Reservation); err != nil {
				return err
			}
			return carrierEnded(ctx, tx, d.ID, state, reason)
		}
		if err = tx.QueryRowContext(ctx, `SELECT settings_json,secret FROM contact_methods WHERE id=?`, d.ContactMethodID).Scan(&settings, &secret); err != nil {
			return err
		}
		if d.MemberID != "" {
			if memberName, err = incidentMemberName(ctx, tx, d.MemberID); err != nil {
				return err
			}
		}
		if combined, err = combinedAlerts(ctx, tx, d); err != nil {
			return err
		}
		if d.Severity == "resolved-sos" && d.IncidentID != "" {
			if resolvesAReplacementSOS, err = wasHeldForAServerReplacement(ctx, tx, d.IncidentID); err != nil {
				return err
			}
		}
		if recorder, ok := contactMethodType.(deliveryPreparationRecorder); ok {
			if err = recorder.prepareDelivery(ctx, tx, d, combined, a.Now()); err != nil {
				return err
			}
		}
		send = true
		return nil
	})
	if err != nil || !send {
		return err
	}
	facts := contactMethodType.Facts() //nolint:nilaway // DispatchOne reserves only a delivery whose contact method type is in its map, and every value there is built by a sending service setup.
	body := d.Message
	if d.IncidentID != "" {
		body += "\n\n" + d.PublicURL + "/incidents/" + d.IncidentID
	} else if d.AlertID != "" {
		body += "\n\n" + d.PublicURL + "/alerts/" + d.AlertID
	}
	subject := d.Subject
	if d.MoreSOSIncidentsMessage {
		subject, body = moreSOSIncidentsText(max(1, len(combined)), d.PublicURL)
	} else if len(combined) > 0 {
		subject, body = combinedMessageText(combined, d.PublicURL)
	}
	var teamName string
	if facts.SaysTeamName {
		if err = a.Store.db.QueryRowContext(ctx, `SELECT name FROM team_info`).Scan(&teamName); err != nil {
			// Nothing was sent: a definite, retryable failure.
			return a.finishDelivery(ctx, d, DeliveryResult{State: "failed", Diagnostic: "Not sent: the team's name could not be read", Retryable: true})
		}
	}
	acknowledgementClaimsSetUp := slices.ContainsFunc(slices.Collect(maps.Values(a.configuredContactMethodTypes())), func(c ContactMethodType) bool { return c.Facts().ClaimMethod == "acknowledgement" })
	request := ContactMethodSend{AcknowledgementClaimsSetUp: acknowledgementClaimsSetUp, ClaimButtons: claimButtons(d, facts, combined), DeliveryID: d.ID, Purpose: d.Purpose, Severity: d.Severity, SentTo: d.SentTo, SentFrom: d.SentFrom, Subject: subject, Text: body, Settings: settings, Secret: secret,
		Title: d.Title, AlertMessage: d.AlertMessage, NotesFromMmhotline: d.NotesFromMmhotline, AlertID: d.AlertID, IncidentID: d.IncidentID, IncidentStatus: d.IncidentStatus, AlertReceivedAt: d.AlertReceivedAt, PublicURL: d.PublicURL, CombinedAlerts: combined,
		DeliveryAttemptID: d.Reservation, MemberName: memberName, TeamName: teamName, ResolvesAnSOSThatWaitedForAServerReplacement: resolvesAReplacementSOS}
	attemptTimeLimit := facts.AttemptTimeLimit
	if attemptTimeLimit == 0 {
		attemptTimeLimit = 15 * time.Second
	}
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeLimit)
	result := contactMethodType.Send(attemptCtx, request)
	cancel()
	// The contact method type's Send already says whether an unknown outcome may be resent.
	if result.State == "uncertain" && !facts.RetryUncertain {
		result.Retryable = false
	}
	// Shutdown must still record a known result. This bounded, independent
	// context only persists the result; it cannot start another sending service request.
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer saveCancel()
	return a.finishDelivery(saveCtx, d, result)
}
func boundedDiagnostic(s string) string {
	if !utf8.ValidString(s) {
		return "invalid sending service diagnostic"
	}
	r := []rune(s)
	if len(r) > 512 {
		return string(r[:512])
	}
	return s
}
func (a *App) finishDelivery(ctx context.Context, d Delivery, result DeliveryResult) error {
	if result.State != "accepted" && result.State != "failed" && result.State != "uncertain" {
		result = DeliveryResult{State: "uncertain", Diagnostic: "sending service returned an invalid outcome", Retryable: true}
	}
	result.Diagnostic = boundedDiagnostic(result.Diagnostic)
	if len(result.IDFromSendingService) > 256 {
		result.IDFromSendingService = ""
	}
	if len(result.SentFrom) > 512 {
		result.SentFrom = ""
	}
	now := a.Now()
	next := int64(0)
	if result.Retryable && result.State != "accepted" && d.Attempts < maxDeliveryAttempts {
		backoff := 5 * time.Second * time.Duration(1<<min(d.Attempts-1, 6))
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		if result.RetryAfter > 0 {
			backoff = result.RetryAfter
		}
		when := now.Add(backoff)
		if when.Before(d.FirstAttempt.Add(retryHorizon)) {
			next = when.Unix()
		}
	}
	// sent_at marks an attempt that may have reached the sending service.
	// Active-call recovery reads this timestamp.
	sentAt := int64(0)
	if result.State == "accepted" || result.State == "uncertain" {
		sentAt = now.Unix()
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET state=?,completed_at=?,id_from_sending_service=?,diagnostic=?,sent_at=?,sending_service=CASE WHEN ?!='' THEN ? ELSE sending_service END WHERE id=?`, result.State, now.Unix(), result.IDFromSendingService, result.Diagnostic, sentAt, result.SendingService, result.SendingService, d.Reservation); err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE deliveries SET state=?,reason=?,id_from_sending_service=?,next_attempt=?,reservation='',reserved_until=0,sent_from=CASE WHEN sent_from='' THEN ? ELSE sent_from END WHERE id=? AND reservation=? AND state='sending'`, result.State, result.Diagnostic, result.IDFromSendingService, next, result.SentFrom, d.ID, d.Reservation)
		if err != nil {
			return err
		}
		// A stale completion does not count toward the contact method's health.
		if n, err := updated.RowsAffected(); err != nil || n == 0 {
			return err
		}
		if next == 0 && result.State != "accepted" {
			if err = carrierEnded(ctx, tx, d.ID, result.State, result.Diagnostic); err != nil {
				return err
			}
		}
		if recorder, ok := a.outcomeRecorder(d.ContactMethodType); ok {
			if err = recorder.recordDeliveryOutcome(ctx, tx, d, d.Reservation, deliveryOutcome{AttemptState: result.State}, now); err != nil {
				return err
			}
		}
		return recordContactMethodOutcome(ctx, tx, d, result.State == "accepted", now)
	})
}

// outcomeRecorder returns the contact method type that records what results and reports
// mean for its contact methods: the configured one when it can send here.
func (a *App) outcomeRecorder(kind string) (deliveryOutcomeRecorder, bool) {
	c, ok := a.contactMethodTypeFor(kind)
	if !ok {
		return nil, false
	}
	recorder, ok := c.(deliveryOutcomeRecorder)
	return recorder, ok
}

var errReportBeforeResult = fmt.Errorf("%w: the attempt's own result is not saved yet; send the report again", ErrConflict)

// attemptStateRank orders attempt states so a report only moves an attempt
// forward: sending, then uncertain, then accepted, then delivered or failed.
func attemptStateRank(state string) int {
	switch state {
	case "uncertain":
		return 1
	case "accepted":
		return 2
	case "delivered", "failed":
		return 3
	}
	return 0
}

// firstIDFromSendingServiceFits checks a sending service ID a report gives an
// attempt that has none yet: the report names this attempt's recipient, and
// no other attempt has the ID.
func firstIDFromSendingServiceFits(ctx context.Context, tx *sql.Tx, d Delivery, sendingService, idFromSendingService, recipientNumber string) error {
	if idFromSendingService == "" || len(idFromSendingService) > 256 || recipientNumber != d.SentTo {
		return ErrForbidden
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM delivery_attempts WHERE sending_service=? AND id_from_sending_service=?`, sendingService, idFromSendingService).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return ErrForbidden
	}
	return nil
}

// RecordDeliveryReport records what a sending service reported later about one
// attempt, found by its delivery attempt ID. Reports
// only move an attempt and its delivery forward
// (accepted, then delivered or failed), so a repeated or late report changes
// nothing. The contact method type records what the report means for its contact
// method, such as a call ending or a number that keeps failing.
func (a *App) RecordDeliveryReport(ctx context.Context, deliveryAttemptID string, report DeliveryReport) error {
	if !recordIDPattern.MatchString(deliveryAttemptID) {
		return ErrNotFound
	}
	now := a.Now()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var deliveryID, attemptState, attemptSendingService, attemptIDFromSendingService string
		err := tx.QueryRowContext(ctx, `SELECT delivery_id,state,sending_service,id_from_sending_service FROM delivery_attempts WHERE id=?`, deliveryAttemptID).Scan(&deliveryID, &attemptState, &attemptSendingService, &attemptIDFromSendingService)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// A report comes only from the sending service the request went to.
		if attemptSendingService == "" || report.SendingService != attemptSendingService {
			return ErrForbidden
		}
		if attemptState == "sending" {
			return errReportBeforeResult
		}
		// The report must be about this attempt's own message or call: a
		// signed report copied onto another attempt's URL changes nothing.
		if attemptIDFromSendingService != "" && report.IDFromSendingService != attemptIDFromSendingService {
			return ErrForbidden
		}
		d, err := scanDelivery(tx.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE id=?`, deliveryID))
		if err != nil {
			return err
		}
		if attemptIDFromSendingService == "" {
			// The sending service's answer gave no ID (an unreadable call answer, or
			// an answer that never arrived): the first report names it. It
			// counts only when it is about this attempt's recipient and its
			// ID belongs to no other attempt, so a signed report about another
			// call cannot become this attempt's.
			if err = firstIDFromSendingServiceFits(ctx, tx, d, attemptSendingService, report.IDFromSendingService, report.RecipientNumber); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET id_from_sending_service=? WHERE id=?`, report.IDFromSendingService, deliveryAttemptID); err != nil {
				return err
			}
			attemptIDFromSendingService = report.IDFromSendingService
		}
		moved := ""
		if report.State != "" && attemptStateRank(report.State) > attemptStateRank(attemptState) {
			moved = report.State
			diagnostic := boundedDiagnostic(report.Diagnostic)
			if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET state=?,diagnostic=CASE WHEN ?!='' THEN ? ELSE diagnostic END,sent_at=CASE WHEN sent_at=0 AND ?!='failed' THEN ? ELSE sent_at END WHERE id=?`, moved, diagnostic, diagnostic, moved, now.Unix(), deliveryAttemptID); err != nil {
				return err
			}
			switch {
			case moved != "failed" && (d.State == "uncertain" || d.State == "failed" || (d.State == "accepted" && moved == "delivered")):
				// Receipt of any attempt settles the delivery and ends its
				// retries.
				_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state=?,next_attempt=0,id_from_sending_service=CASE WHEN id_from_sending_service='' THEN ? ELSE id_from_sending_service END WHERE id=?`, moved, attemptIDFromSendingService, d.ID)
			case moved == "failed" && (d.State == "accepted" || d.State == "uncertain") && d.Reservation == "" && latestAttempt(ctx, tx, d.ID) == deliveryAttemptID:
				_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='failed',reason=?,next_attempt=0 WHERE id=?`, diagnostic, d.ID)
				if err == nil {
					err = carrierEnded(ctx, tx, d.ID, "failed", diagnostic)
				}
			default:
				moved = ""
			}
			if err != nil {
				return err
			}
		}
		if moved == "" && report.CallStatus == "" {
			return nil
		}
		if recorder, ok := a.outcomeRecorder(d.ContactMethodType); ok {
			return recorder.recordDeliveryOutcome(ctx, tx, d, deliveryAttemptID, deliveryOutcome{AttemptState: moved, CallStatus: report.CallStatus, FromReport: true}, now)
		}
		return nil
	})
}

// latestAttempt returns the ID of a delivery's newest attempt.
func latestAttempt(ctx context.Context, tx *sql.Tx, deliveryID string) string {
	var id string
	_ = tx.QueryRowContext(ctx, `SELECT id FROM delivery_attempts WHERE delivery_id=? ORDER BY number DESC LIMIT 1`, deliveryID).Scan(&id)
	return id
}

const failuresBeforeKeepingOnlyNewest = 3

// recordContactMethodOutcome keeps count of failures in a row. A contact method type that
// sends every alert separately (webhook) would pile up retries while its
// receiver is down, so after 3 failures in a row it keeps only its newest
// pending delivery, and administrators are told once.
func recordContactMethodOutcome(ctx context.Context, tx *sql.Tx, d Delivery, accepted bool, now time.Time) error {
	// A result for older settings (such as a webhook's previous URL) says
	// nothing about the current destination.
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM contact_methods WHERE id=?`, d.ContactMethodID).Scan(&revision); err != nil || revision != d.ContactMethodRevision {
		return err
	}
	if accepted {
		_, err := tx.ExecContext(ctx, `UPDATE contact_methods SET failures_in_a_row=0,failing_since=0 WHERE id=?`, d.ContactMethodID)
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_methods SET failures_in_a_row=failures_in_a_row+1,failing_since=CASE WHEN failing_since=0 THEN ? ELSE failing_since END WHERE id=?`, now.Unix(), d.ContactMethodID); err != nil {
		return err
	}
	var failures, since int64
	var contactMethodType, sentTo, ownerID string
	err := tx.QueryRowContext(ctx, `SELECT failures_in_a_row,failing_since,contact_method_type,sent_to,member_id FROM contact_methods WHERE id=?`, d.ContactMethodID).Scan(&failures, &since, &contactMethodType, &sentTo, &ownerID)
	if err != nil {
		return err
	}
	facts, _ := contactMethodTypeFacts(contactMethodType)
	if failures < failuresBeforeKeepingOnlyNewest {
		return nil
	}
	failingSince := displayTime(deadlineTime(since))
	keptNote := ""
	if !facts.CombinesMessages {
		// A contact method type that sends every alert separately (webhook) would pile
		// up retries while it is down. SOS alerts and tests are never
		// dropped, and never count as the newest.
		const droppable = `contact_method_id=? AND severity!='sos' AND purpose!='contact_method_test' AND ` + pendingDelivery
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='cancelled',reason=?,next_attempt=0 WHERE `+droppable+` AND id!=(SELECT id FROM deliveries WHERE `+droppable+` ORDER BY created_at DESC,id DESC LIMIT 1)`, fmt.Sprintf("Not sent: the contact method has been failing since %s", failingSince), d.ContactMethodID, d.ContactMethodID); err != nil {
			return err
		}
		keptNote = " Until the contact method works again, MMHotline keeps only the newest alert for the contact method."
	}
	if failures != failuresBeforeKeepingOnlyNewest {
		return nil
	}
	// Administrators are told once per failing streak, for every contact method type: a
	// Telegram group the bot was removed from fails as quietly as a webhook.
	// The notice is optional: if it cannot be recorded (a full queue), the
	// delivery's own result is still saved.
	where := sentTo
	if u, err := url.Parse(sentTo); err == nil && u.Host != "" {
		where = u.Host
	}
	// Every member reads the alert history, so a member's phone number is
	// named by its owner, never shown.
	if ownerID != "" && facts.ValueFixed {
		owner, err := incidentMemberName(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		where, sentTo = owner+"'s "+facts.CardTitle, owner+"'s "+facts.CardTitle
	}
	bestEffort(ctx, tx, "administrator_notice", func() error {
		return RecordAdministratorNotice(ctx, tx, "warn", truncateRunes("MMHotline cannot deliver alerts to "+where, 256), fmt.Sprintf("MMHotline could not deliver to “%s” %d times in a row, since %s.%s", sentTo, failures, failingSince, keptNote), now)
	})
	return nil
}

// fitChatMessage shortens only the alert's own message inside a chat
// message's text, so the time, notes, SOS instruction, and link that follow
// it survive the chat's length limit. fits says whether a text is short
// enough. The message follows the title line and a blank line; if it is not
// found, the text is returned unchanged and the contact method type cuts its end.
func fitChatMessage(text, alertMessage string, fits func(string) bool) string {
	start := strings.Index(text, "\n\n"+alertMessage)
	if fits(text) || alertMessage == "" || start < 0 {
		return text
	}
	before, after := text[:start+2], text[start+2+len(alertMessage):]
	runes := []rune(alertMessage)
	shortened := func(n int) string { return before + string(runes[:n]) + "…" + after }
	// The longest start of the message that fits.
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if fits(shortened(middle)) {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return shortened(low)
}

// bestEffort runs an optional side effect inside a savepoint. If it fails,
// only its own writes are undone and the caller's transaction goes on.
func bestEffort(ctx context.Context, tx *sql.Tx, savepoint string, effect func() error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return
	}
	if effect() != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO "+savepoint)
	}
	_, _ = tx.ExecContext(ctx, "RELEASE "+savepoint)
}

func (d Delivery) PurposeLabel() string {
	switch d.Purpose {
	case "subscribed_alert":
		return "Alert"
	case "sos_escalation":
		return "SOS: contacting the on-call person"
	case "system_notice":
		return "Notice from MMHotline"
	case "contact_method_test":
		return "Test"
	}
	return d.Purpose
}
func (d Delivery) OutcomeLabel() string {
	// A phone call says how it went: "Answered (by a person or voicemail)", "No answer".
	if label := callResultLabel(d.CallResult); label != "" && d.State != "cancelled" && d.State != "suppressed" {
		return label
	}
	switch d.State {
	case "queued":
		return "Waiting to send"
	case "sending":
		return "Sending"
	case "accepted":
		if facts, ok := contactMethodTypeFacts(d.ContactMethodType); ok {
			return facts.AcceptedLabel
		}
		return "Accepted"
	case "failed", "uncertain":
		label := "Failed"
		if d.State == "uncertain" {
			label = "Unknown (no answer from the sending service)"
		}
		if !d.NextAttempt.IsZero() {
			return label + ", will retry at " + displayTime(d.NextAttempt)
		}
		return label + ", no more retries"
	case "cancelled":
		return "Cancelled"
	case "suppressed":
		return "Suppressed"
	case "combined":
		return "Sent together with other alerts"
	case "delivered":
		return "Delivered"
	}
	return d.State
}

// deliveryReasonTexts says in plain words the reasons MMHotline records as
// short internal values when it does not send a notification. Pages translate
// the reason without changing the stored value. Every other reason, such
// as a sending service's answer, is shown as it is.
var deliveryReasonTexts = map[string]string{
	"SOS contact method type follows the active target": "for an SOS, this contact method only wakes the people MMHotline is waking",
	"sos_escalation step changed":                       "MMHotline moved on to the next person",
	"team eligibility changed":                          "who is available changed, so MMHotline wakes the people available now",
	"incident claimed":                                  "someone claimed the incident",
	"incident resolved":                                 "the incident was resolved",
	nothingAboutTheIncidentWasSentReason:                "MMHotline sent this contact method nothing about the incident, so there is no end to tell",
	"incident escalated":                                "someone escalated the incident to another person",
	"incident or target changed":                        "the incident changed before the notification was sent",
	"time to claim ran out":                             "the time to claim ran out",
	"incident missing":                                  "the incident no longer exists",
	"unavailable":                                       "the person is unavailable",
	"member removed":                                    "the person is no longer on the team",
	"team mute":                                         "the team is muted",
	reservationExpiredReason:                            "MMHotline does not know whether the notification arrived: the sending service did not answer in time",
}

// ReasonText is the delivery's reason in plain words for pages.
func (d Delivery) ReasonText() string {
	if text, ok := deliveryReasonTexts[d.Reason]; ok {
		return text
	}
	return d.Reason
}

func (a *App) IncidentDeliveries(ctx context.Context, p Principal, incidentID string) ([]Delivery, error) {
	var out []Delivery
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		if _, err = readIncident(ctx, tx, incidentID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE incident_id=? ORDER BY created_at DESC,id DESC LIMIT 200`, incidentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelivery(rows)
			if err != nil {
				return err
			}
			if d.MemberID != "" && d.MemberID != m.ID && m.Role != RoleAdministrator {
				d.SentTo = "private contact method"
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

func (d DeliveryAttempt) OutcomeLabel() string {
	switch d.State {
	case "failed":
		return "Failed"
	case "uncertain":
		return "Unknown (no answer from the sending service)"
	}
	return (Delivery{State: d.State}).OutcomeLabel()
}
