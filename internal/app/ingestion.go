package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxAlertBody = 64 * 1024
const SourceAlertsPerMinute = 120
const missingSeverityNote = "Received without severity, defaulting to 'warn'"

// GenericAlert is the signed JSON a sender posts. HappenedAt is optional: an
// RFC 3339 time on the sender's clock, for display only.
type GenericAlert struct{ SenderAlertID, SenderIncidentKey, Severity, Title, Message, HappenedAt string }
type Alert struct {
	Sequence                                                                                           int64
	ID, SourceID, SenderAlertID, IncidentID, SenderIncidentKey, Severity, Title, Message, Notes, Actor string
	ReceivedAt, HappenedAt                                                                             time.Time
	// RepeatCount counts the later warns folded into this one, and
	// LastRepeatTitle is the latest one's title (repeated_warnings.go).
	RepeatCount     int64
	LastRepeatTitle string
	// PassedOverForTheRunLimit: the triage agent's run limit held, so the
	// alert went straight to people and never to the agent
	// (triage_agent_run_limit.go).
	PassedOverForTheRunLimit bool
}

// happenedGap describes a late arrival: "Happened at 14:02 UTC. Received at
// 14:31 UTC (29 minutes later)." It is empty unless the alert arrived more
// than two minutes after it happened on the sender's clock.
func happenedGap(happened, received time.Time) string {
	if receivedLater(happened, received) == "" {
		return ""
	}
	format := func(t time.Time) string {
		if t.UTC().Format("2006-01-02") == received.UTC().Format("2006-01-02") {
			return t.UTC().Format("15:04 UTC")
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return fmt.Sprintf("Happened at %s. Received at %s (%s).", format(happened), format(received), receivedLater(happened, received))
}

// receivedLater says how much later than it happened an alert arrived, such
// as "29 minutes later", or "" when happenedGap finds no late arrival.
func receivedLater(happened, received time.Time) string {
	if happened.IsZero() || received.Sub(happened) <= 2*time.Minute {
		return ""
	}
	gap := received.Sub(happened)
	switch {
	case gap >= 48*time.Hour:
		return fmt.Sprintf("%d days later", int(gap.Hours()/24))
	case gap >= 2*time.Hour:
		return fmt.Sprintf("%d hours later", int(gap.Hours()))
	}
	return fmt.Sprintf("%d minutes later", int(gap.Minutes()))
}

// AlertFilter picks alerts for ListAlerts. SourceID is an alert source's ID,
// or one of the three IDs of alerts no alert source sent: systemAlertSourceID,
// manualAlertSourceID, and testAlertSourceID.
type AlertFilter struct {
	Query, Severity, IncidentID, SourceID string
	Before                                int64
	Limit                                 int
	From, Until                           time.Time
}

// The alert source IDs of the alerts no alert source sent: MMHotline's own,
// the SOS incidents a person creates by hand, and the test SOS that
// `mmhotline incidents send-test-sos` opens. Only a test SOS has the third
// ID, so it is the one place that says "this incident is a test", and the
// only incident that resolves itself when someone claims it.
const (
	systemAlertSourceID = "system"
	manualAlertSourceID = "manual"
	testAlertSourceID   = "mmhotline_test_sos"
)

// Decode each named value exactly once. encoding/json's ordinary struct decode
// otherwise accepts duplicate names and case-insensitive aliases.
func decodeGeneric(body []byte) (GenericAlert, error) {
	var in GenericAlert
	if len(body) > MaxAlertBody || !utf8.Valid(body) {
		return GenericAlert{}, fmt.Errorf("%w: alert request body must be UTF-8 JSON within the request size limit", ErrInvalid)
	}
	d := json.NewDecoder(bytes.NewReader(body))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return GenericAlert{}, fmt.Errorf("%w: body must be an object", ErrInvalid)
	}
	fields := map[string]*string{"sender_alert_id": &in.SenderAlertID, "sender_incident_key": &in.SenderIncidentKey, "severity": &in.Severity, "title": &in.Title, "message": &in.Message, "happened_at": &in.HappenedAt}
	seen := map[string]bool{}
	for d.More() {
		keyToken, err := d.Token()
		if err != nil {
			return GenericAlert{}, fmt.Errorf("%w: alert request body is not valid JSON", ErrInvalid)
		}
		key, ok := keyToken.(string)
		target, known := fields[key]
		if !ok || !known || seen[key] {
			return GenericAlert{}, fmt.Errorf("%w: unknown or duplicate field %q", ErrInvalid, key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil || bytes.Equal(raw, []byte("null")) {
			if key == "severity" {
				return GenericAlert{}, fmt.Errorf("%w: severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos", ErrInvalid)
			}
			return GenericAlert{}, fmt.Errorf("%w: %s must be a string", ErrInvalid, key)
		}
		if err = json.Unmarshal(raw, target); err != nil {
			if key == "severity" {
				return GenericAlert{}, fmt.Errorf("%w: severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos", ErrInvalid)
			}
			return GenericAlert{}, fmt.Errorf("%w: %s must be a string", ErrInvalid, key)
		}
	}
	if _, err = d.Token(); err != nil {
		return GenericAlert{}, fmt.Errorf("%w: alert request body is not valid JSON", ErrInvalid)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return GenericAlert{}, fmt.Errorf("%w: one JSON object required", ErrInvalid)
	}
	if !seen["title"] || !seen["message"] {
		return GenericAlert{}, fmt.Errorf("%w: title and message required", ErrInvalid)
	}
	if seen["severity"] && !validSeverity(in.Severity) {
		return GenericAlert{}, fmt.Errorf("%w: severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos", ErrInvalid)
	}
	if seen["sender_alert_id"] && in.SenderAlertID == "" || seen["sender_incident_key"] && in.SenderIncidentKey == "" {
		return GenericAlert{}, fmt.Errorf("%w: sender_alert_id and sender_incident_key must be nonempty when present", ErrInvalid)
	}
	if seen["happened_at"] {
		if _, err := time.Parse(time.RFC3339, in.HappenedAt); err != nil || len(in.HappenedAt) > 64 {
			return GenericAlert{}, fmt.Errorf("%w: happened_at must be an RFC 3339 time, such as 2026-09-29T03:00:12Z", ErrInvalid)
		}
	}
	if err = validateAlert(in); err != nil {
		return GenericAlert{}, err
	}
	return in, nil
}
func validSeverity(s string) bool {
	switch s {
	case "info", "warn", "sos", "resolved-info", "resolved-warn", "resolved-sos":
		return true
	}
	return false
}
func validateAlert(in GenericAlert) error {
	if in.Severity != "" && !validSeverity(in.Severity) {
		return fmt.Errorf("%w: severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos", ErrInvalid)
	}
	if strings.TrimSpace(in.Title) == "" || !utf8.ValidString(in.Title) || utf8.RuneCountInString(in.Title) > 256 {
		return fmt.Errorf("%w: title must contain 1–256 characters", ErrInvalid)
	}
	if len(in.Message) > 16*1024 || !utf8.ValidString(in.Message) {
		return fmt.Errorf("%w: message exceeds 16 KiB or is not UTF-8", ErrInvalid)
	}
	if len(in.SenderAlertID) > 256 || len(in.SenderIncidentKey) > 256 || !utf8.ValidString(in.SenderAlertID) || !utf8.ValidString(in.SenderIncidentKey) {
		return fmt.Errorf("%w: sender_alert_id or sender_incident_key exceeds 256 bytes or is not UTF-8", ErrInvalid)
	}
	return nil
}

// refusedAlertFields reads the sender_alert_id and severity of a correctly
// signed alert that MMHotline refuses, as invalid or for the sender's clock:
// they still say whether to open MMHotline's own SOS about it, even when the
// field that broke a rule came before them. Each is read on its own, so a
// field of the wrong type hides neither.
func refusedAlertFields(body []byte) (senderAlertID, severity string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return "", ""
	}
	_ = json.Unmarshal(fields["sender_alert_id"], &senderAlertID)
	_ = json.Unmarshal(fields["severity"], &severity)
	return senderAlertID, severity
}

// IntakeOutcome says what intake did with an accepted alert, for the answer
// to the sender.
type IntakeOutcome struct {
	// AlreadyReceived: the request repeats one MMHotline already accepted.
	AlreadyReceived bool
	// RecordedAsRepeatOfEarlierAlert: the alert was folded into an earlier
	// warn, which the answer's alert ID names (repeated_warnings.go).
	RecordedAsRepeatOfEarlierAlert bool
}

// errSenderAlertIDReused refuses a different alert that reuses a
// sender_alert_id, so it is never answered as received and then dropped.
var errSenderAlertIDReused = fmt.Errorf("%w: sender_alert_id already used for a different alert; give each new alert its own sender_alert_id", ErrInvalid)

// AdmitGeneric admits one signed generic alert. It also returns the signing
// secret that verified the request, to sign the receipt, or nil when the
// signature was not verified.
func (a *App) AdmitGeneric(ctx context.Context, sourceID, signature string, body []byte) (Alert, IntakeOutcome, []byte, error) {
	var alert Alert
	var outcome IntakeOutcome
	now := a.Now().UTC()
	if !alertSourceIDPattern.MatchString(sourceID) {
		return alert, outcome, nil, ErrUnauthorized
	}
	var in GenericAlert
	var receiptKey []byte
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		replayKey, secret, err := authenticateSource(ctx, tx, sourceID, signature, body, now)
		receiptKey = secret
		if err == nil {
			// Any signed request, an alert or a check-in, counts as
			// hearing from the alert source.
			if err = heardFromSource(ctx, tx, sourceID, now); err != nil {
				return err
			}
		}
		if errors.Is(err, errSenderClockOff) {
			// Correctly signed: read the severity so a refused SOS is reported.
			in.SenderAlertID, in.Severity = refusedAlertFields(body)
			if in.SenderAlertID != "" {
				replayKey = "sender:" + in.SenderAlertID
			}
			// A copy of an alert MMHotline already received, such as a request
			// captured on the private network and sent again later, opens no
			// SOS of MMHotline's own, so it cannot wake people again and again.
			if _, findErr := readAlertBy(ctx, tx, "source_id=? AND replay_key=?", sourceID, replayKey); findErr == nil {
				return errResentAfterTheSignatureWindow
			}
		}
		if err != nil {
			return err
		}
		in, err = decodeGeneric(body)
		if err != nil {
			in.SenderAlertID, in.Severity = refusedAlertFields(body)
			return err
		}
		if in.SenderAlertID != "" {
			replayKey = "sender:" + in.SenderAlertID
		}
		alert, err = readAlertBy(ctx, tx, "source_id=? AND replay_key=?", sourceID, replayKey)
		if err == nil {
			// Only the sender's retry of the same alert is a duplicate. A
			// different alert that reuses the sender_alert_id is refused, so
			// it is never answered as received and then dropped.
			if !sameAlertContent(alert, in) {
				return errSenderAlertIDReused
			}
			outcome.AlreadyReceived = true
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		// A repeat folded into an earlier warn, and a retry of one, are
		// recognized before both intake limits, so a burst of repeats
		// leaves room for unrelated alerts.
		var repeat bool
		if alert, repeat, err = retryOfFoldedRepeat(ctx, tx, sourceID, replayKey, in, now); err != nil || repeat {
			outcome = IntakeOutcome{AlreadyReceived: repeat, RecordedAsRepeatOfEarlierAlert: repeat}
			return err
		}
		if alert, repeat, err = foldRepeatedWarning(ctx, tx, sourceID, replayKey, in, now); err != nil || repeat {
			outcome.RecordedAsRepeatOfEarlierAlert = repeat
			return err
		}
		// SOS and resolved SOS alerts are never limited by rate; the team's
		// unresolved-incident limit bounds them.
		if baseNotificationSeverity(in.Severity) != "sos" {
			var bucket, count int64
			err = tx.QueryRowContext(ctx, `SELECT bucket,alert_count FROM source_rates WHERE source_id=?`, sourceID).Scan(&bucket, &count)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if bucket != now.Unix()/60 {
				count = 0
			}
			if count >= SourceAlertsPerMinute {
				return errTooManyAlerts
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO source_rates(source_id,bucket,alert_count) VALUES(?,?,?) ON CONFLICT(source_id) DO UPDATE SET bucket=excluded.bucket,alert_count=excluded.alert_count`, sourceID, now.Unix()/60, count+1); err != nil {
				return err
			}
			if err = countTeamWarningAndInfoAlert(ctx, tx, in.Severity, now); err != nil {
				return err
			}
		}
		alert, err = admitAlert(ctx, tx, sourceID, "source:"+sourceID, replayKey, "", in, nil, now)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		err = ErrUnauthorized
	}
	if err != nil {
		// One journal line for each refused alert, so a refusal can be
		// traced on the server; the sender's fields are only shown when the
		// request was signed.
		log.Printf("Alert refused from alert source %s: sender_alert_id %q, severity %q: %v", sourceID, in.SenderAlertID, in.Severity, err)
		a.afterRefusedRequest(ctx, sourceID, in.Severity, err)
	} else {
		a.heardFromAlertSource(sourceID, now)
	}
	return alert, outcome, receiptKey, err
}

// sameAlertContent says whether a stored alert says what the new alert says:
// its severity, title, message, and sender_incident_key, which decide what
// MMHotline does with it and what people read. happened_at is left out,
// because a sender may set it again on a retry. The alerts table keeps these
// fields as sent, except a missing severity, which it keeps as warn, so every
// stored alert can be compared and no stored hash is needed.
func sameAlertContent(stored Alert, in GenericAlert) bool {
	severity := in.Severity
	if severity == "" {
		severity = "warn"
	}
	return stored.Severity == severity && stored.Title == in.Title && stored.Message == in.Message && stored.SenderIncidentKey == in.SenderIncidentKey
}

// afterRefusedRequest counts the refusal for the alert source page, tells
// administrators about a source sending too many alerts, and opens a
// MMHotline SOS when a correctly signed SOS was refused. These are best
// effort: the sender already gets the refusal and retries.
func (a *App) afterRefusedRequest(ctx context.Context, sourceID, severity string, refusal error) {
	if reason := rejectionReason(refusal); reason != "" {
		a.rejected.add(sourceID, reason, a.Now())
	}
	if errors.Is(refusal, errTooManyAlerts) {
		_ = a.tellAboutTooManyAlerts(ctx, sourceID)
	}
	signed := errors.Is(refusal, errSenderClockOff) || errors.Is(refusal, ErrInvalid) || errors.Is(refusal, errTooManyIncidents) || errors.Is(refusal, errPendingDeliveryFull) || errors.Is(refusal, errAlertSourcePendingSOSFull)
	if severity == "sos" && signed {
		if err := a.recordRefusedSOS(ctx, sourceID, refusal); err != nil {
			log.Printf("MMHotline could not open its own SOS about the refused SOS from alert source %s: %v", sourceID, err)
		}
	}
}

// admitAlert records one alert and its effects. A non-empty recipients list
// limits its notification to those members.
func admitAlert(ctx context.Context, tx *sql.Tx, sourceID, actor, replayKey, incidentID string, in GenericAlert, recipients []string, at time.Time) (Alert, error) {
	// Notices to chosen members are about MMHotline itself, so they are for people only.
	peopleOnly := len(recipients) > 0
	e := Alert{ID: NewID(), IncidentID: incidentID, SourceID: sourceID, SenderAlertID: in.SenderAlertID, Actor: actor, SenderIncidentKey: in.SenderIncidentKey, Severity: in.Severity, Title: in.Title, Message: in.Message, ReceivedAt: at}
	if in.HappenedAt != "" {
		e.HappenedAt, _ = time.Parse(time.RFC3339, in.HappenedAt)
	}
	if e.Severity == "" {
		e.Severity = "warn"
		e.Notes = missingSeverityNote
	}
	var existing Incident
	var err error
	if (e.Severity == "sos" || e.Severity == "resolved-sos") && e.SenderIncidentKey != "" {
		existing, err = readIncidentBy(ctx, tx, "i.source_id=? AND i.sender_incident_key=? AND i.status!='resolved'", sourceID, e.SenderIncidentKey)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Alert{}, err
		}
		if err == nil {
			e.IncidentID = existing.ID
		}
	}
	newIncident := e.Severity == "sos" && e.IncidentID == ""
	// An SOS that names recipients opens an incident for only them.
	var sosRecipients []string
	if newIncident {
		sosRecipients = recipients
	}
	if newIncident {
		e.IncidentID = NewID()
	}
	if e.Severity == "resolved-sos" && e.IncidentID == "" {
		e.Notes = fmt.Sprintf("No active incident matched resolution key %q.", e.SenderIncidentKey)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO alerts(id,source_id,sender_alert_id,actor,replay_key,incident_id,sender_incident_key,severity,title,message,notes,received_at,happened_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, e.SourceID, e.SenderAlertID, e.Actor, replayKey, e.IncidentID, e.SenderIncidentKey, e.Severity, e.Title, e.Message, e.Notes, at.Unix(), deadlineUnix(e.HappenedAt))
	if err != nil {
		return Alert{}, err
	}
	e.Sequence, err = result.LastInsertId()
	if err != nil {
		return Alert{}, err
	}
	// While the triage agent's run limit holds, an alert that would go to
	// the agent goes straight to people: marked before its hold or its wait
	// is decided.
	if e.PassedOverForTheRunLimit, err = passOverAtIntakeForTheRunLimit(ctx, tx, e.ID); err != nil {
		return Alert{}, err
	}
	switch {
	case newIncident:
		i, err := createIncident(ctx, tx, e, actor, at, sosRecipients)
		if err != nil {
			return Alert{}, err
		}
		// An SOS whose first waking step waits for the triage agent sends
		// its email and chat when the wait is over
		// (queueSOSNotificationAfterItsWait), not before the agent looked.
		if i.Step == triageAgentStep {
			return e, nil
		}
	// An SOS that joined an open incident by its key contacts nobody again:
	// the incident's escalation is already waking people. One that joined a
	// claimed incident goes to the person who claimed it, through their
	// contact methods subscribed to SOS, waking ones included: the problem
	// came back, and nobody else is being woken for it. An SOS from MMHotline
	// about a known incident, the triage agent's report, goes on to the SOS
	// subscriptions below.
	case e.Severity == "sos" && existing.ID != "":
		waiting := 0
		// MMHotline's own SOS is exempt from the pending-delivery limits
		// (checkPendingDeliveryLimits), so while a message about the
		// incident to the person who claimed it still waits, a new one adds
		// no other: a sender refused again and again cannot fill the queue.
		if existing.Status == "claimed" && sourceID == "system" && actor == "system" {
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE incident_id=? AND notification_key LIKE 'related_alert_sent_to_claimer:%' AND `+pendingDelivery, existing.ID).Scan(&waiting); err != nil {
				return Alert{}, err
			}
		}
		if existing.Status != "claimed" || waiting > 0 {
			if err = recordIncidentAction(ctx, tx, existing, actor, "related_alert", "Related alert: "+e.Title, e.ID, "", at); err != nil {
				return Alert{}, err
			}
			return e, nil
		}
		if err = recordIncidentAction(ctx, tx, existing, actor, "related_alert_sent_to_claimer", "Related alert: "+e.Title, e.ID, "", at); err != nil {
			return Alert{}, err
		}
		return e, QueueNotification(ctx, tx, Notification{Key: "related_alert_sent_to_claimer:" + e.ID, AlertID: e.ID, IncidentID: existing.ID, Severity: "sos", Title: e.Title, Message: e.Message, Notes: "You claimed this incident, so this new SOS about it goes to you.", Purpose: "subscribed_alert", Recipients: []string{existing.OwnerMemberID}, PeopleOnly: true, WakesRecipients: true, HappenedAt: e.HappenedAt, At: at})
	case e.Severity == "resolved-sos":
		if e.IncidentID == "" {
			return e, nil
		}
		if _, err = resolveIncidentTx(ctx, tx, existing, actor, e.ID, "", "", at); err != nil {
			return Alert{}, err
		}
		return e, nil
	}
	// A warn that may start a triage session waits for the triage agent;
	// the background loop delivers it or the agent takes it over.
	if err = heldWarnAlertResolved(ctx, tx, e, at); err != nil {
		return Alert{}, err
	}
	if waits, err := resolutionWaitsForItsWarns(ctx, tx, e); err != nil || waits {
		return e, err
	}
	if quiet, err := resolvesOnlyWarnsNeverSent(ctx, tx, e); err != nil || quiet {
		return e, err
	}
	held, err := holdWarnForTheAgent(ctx, tx, e, peopleOnly, at)
	if err != nil || held {
		return e, err
	}
	err = QueueNotification(ctx, tx, Notification{Key: e.ID, AlertID: e.ID, IncidentID: e.IncidentID, Severity: e.Severity, Title: e.Title, Message: e.Message, Notes: e.Notes, Purpose: notificationPurpose(e.SourceID), Recipients: recipients, PeopleOnly: peopleOnly, HappenedAt: e.HappenedAt, At: at})
	return e, err
}
func RecordSystemAlert(ctx context.Context, tx *sql.Tx, actor, incidentID, severity, title, message string, at time.Time) error {
	if severity == "" {
		severity = "info"
	}
	if severity == "sos" || severity == "resolved-sos" {
		return ErrInvalid
	}
	in := GenericAlert{Severity: severity, Title: title, Message: message}
	if err := validateAlert(in); err != nil {
		return err
	}
	_, err := admitAlert(ctx, tx, "system", actor, "local:"+NewID(), incidentID, in, nil, at)
	return err
}

// RecordAdministratorNotice records a notice from MMHotline at severity,
// warn or info, in the alert history and notifies only the team's
// administrators.
func RecordAdministratorNotice(ctx context.Context, tx *sql.Tx, severity, title, message string, at time.Time) error {
	in := GenericAlert{Severity: severity, Title: title, Message: message}
	if err := validateAlert(in); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM members WHERE role=? AND removed_at IS NULL ORDER BY id`, RoleAdministrator)
	if err != nil {
		return err
	}
	var administrators []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		administrators = append(administrators, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(administrators) == 0 {
		return err
	}
	_, err = admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", in, administrators, at)
	return err
}
func (a *App) ManualSOS(ctx context.Context, p Principal, title, message string) (Alert, error) {
	var e Alert
	in := GenericAlert{Severity: "sos", Title: title, Message: message}
	if err := validateAlert(in); err != nil {
		return e, err
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		e, err = admitAlert(ctx, tx, "manual", "member:"+m.ID, "local:"+NewID(), "", in, nil, a.Now().UTC())
		return err
	})
	return e, err
}

const alertColumns = `sequence,id,source_id,sender_alert_id,actor,incident_id,sender_incident_key,severity,title,message,notes,received_at,happened_at,repeat_count,last_repeat_title,passed_over_for_the_run_limit`

type scanner interface{ Scan(...any) error }

func scanAlert(row scanner) (Alert, error) {
	var e Alert
	var at, happened int64
	err := row.Scan(&e.Sequence, &e.ID, &e.SourceID, &e.SenderAlertID, &e.Actor, &e.IncidentID, &e.SenderIncidentKey, &e.Severity, &e.Title, &e.Message, &e.Notes, &at, &happened, &e.RepeatCount, &e.LastRepeatTitle, &e.PassedOverForTheRunLimit)
	e.HappenedAt = deadlineTime(happened)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	e.ReceivedAt = time.Unix(at, 0).UTC()
	return e, err
}
func readAlertBy(ctx context.Context, tx *sql.Tx, where string, args ...any) (Alert, error) {
	return scanAlert(tx.QueryRowContext(ctx, `SELECT `+alertColumns+` FROM alerts WHERE `+where, args...))
}
func (a *App) GetAlert(ctx context.Context, p Principal, id string) (Alert, error) {
	var e Alert
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		e, err = readAlertBy(ctx, tx, "id=?", id)
		return err
	})
	return e, err
}
func (a *App) ListAlerts(ctx context.Context, p Principal, f AlertFilter) ([]Alert, error) {
	var result []Alert
	if f.Limit == 0 {
		f.Limit = 30
	}
	if f.Severity != "" && !validSeverity(f.Severity) {
		return nil, fmt.Errorf("%w: severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos", ErrInvalid)
	}
	if f.Limit < 1 || f.Limit > 100 || len(f.Query) > 256 || f.Before < 0 {
		return nil, ErrInvalid
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		where := []string{"1=1"}
		var args []any
		if f.Before > 0 {
			where = append(where, "sequence<?")
			args = append(args, f.Before)
		}
		if f.Severity != "" {
			where = append(where, "severity=?")
			args = append(args, f.Severity)
		}
		if f.IncidentID != "" {
			// A mistyped ID would list no alerts, which reads as a quiet incident.
			var known bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM incidents WHERE id=?)`, f.IncidentID).Scan(&known); err != nil {
				return err
			}
			if !known {
				return fmt.Errorf("%w: no SOS incident has ID %s", ErrNotFound, f.IncidentID)
			}
			where = append(where, "incident_id=?")
			args = append(args, f.IncidentID)
		}
		if f.SourceID != "" {
			// A mistyped ID would list no alerts, which reads as a quiet
			// alert source.
			var known bool
			if err := tx.QueryRowContext(ctx, `SELECT ? IN (?,?,?) OR EXISTS(SELECT 1 FROM sources WHERE id=?)`, f.SourceID, systemAlertSourceID, manualAlertSourceID, testAlertSourceID, f.SourceID).Scan(&known); err != nil {
				return err
			}
			if !known {
				return fmt.Errorf("%w: no alert source has ID %s", ErrNotFound, f.SourceID)
			}
			where = append(where, "source_id=?")
			args = append(args, f.SourceID)
		}
		if !f.From.IsZero() {
			where = append(where, "received_at>=?")
			args = append(args, f.From.Unix())
		}
		if !f.Until.IsZero() {
			where = append(where, "received_at<?")
			args = append(args, f.Until.Unix())
		}
		if f.Query != "" {
			where = append(where, "(instr(lower(title),lower(?))>0 OR instr(lower(message),lower(?))>0 OR id=?)")
			args = append(args, f.Query, f.Query, f.Query)
		}
		args = append(args, f.Limit)
		rows, err := tx.QueryContext(ctx, `SELECT `+alertColumns+` FROM alerts WHERE `+strings.Join(where, " AND ")+` ORDER BY sequence DESC LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanAlert(rows)
			if err != nil {
				return err
			}
			result = append(result, e)
		}
		return rows.Err()
	})
	return result, err
}

func notificationPurpose(sourceID string) string {
	if sourceID == "system" {
		return "system_notice"
	}
	return "subscribed_alert"
}
