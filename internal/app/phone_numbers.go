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

// phoneTeamSchema records contact card downloads and announcements.
const phoneTeamSchema = `
CREATE TABLE contact_card_downloads (
 member_id TEXT PRIMARY KEY,
 contact_card_fingerprint TEXT NOT NULL,
 downloaded_at INTEGER NOT NULL
);
CREATE TABLE contact_card_announcements (
 id INTEGER PRIMARY KEY CHECK(id=1),
 contact_card_fingerprint TEXT NOT NULL
);
`

// Each member may keep three phone numbers to bound accidental call fan-out.
const phoneNumbersPerMember = 3

const phoneNumberConsentText = "By adding this number you agree to get alert calls from MMHotline."

type phoneNumberSettings struct {
	PhoneNumberToCall string `json:"phone_number_to_call"`
}

// checkPhoneNumberSettings validates a phone number for phone calls. The destination shown is the number in E.164 form.
func checkPhoneNumberSettings(input ContactMethodSettingsInput) (ContactMethodSettings, error) {
	number, _, err := parsePhoneNumber(input.Fields["phone_number_to_call"])
	if err != nil {
		return ContactMethodSettings{}, err
	}
	settings, err := settingsJSON(phoneNumberSettings{PhoneNumberToCall: number})
	return ContactMethodSettings{JSON: settings, SentTo: number}, err
}

// contactMethodTypesWith lists the contact method type kinds whose facts match.
func contactMethodTypesWith(match func(ContactMethodTypeFacts) bool) []any {
	var kinds []any
	for _, c := range contactMethodTypes {
		if match(c.Facts()) {
			kinds = append(kinds, c.Name())
		}
	}
	return kinds
}

func sqlPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// PhoneNumberAdded is the result of adding a phone number and its call contact method.
type PhoneNumberAdded struct {
	PhoneNumberToCall string
	ContactMethods    []ContactMethod
}

// AddPhoneNumber adds a member's own phone number: an "automated voice call" contact
// method (SOS only), ready for alerts immediately. Only the owner adds a
// personal number; the team account never has one.
func (a *App) AddPhoneNumber(ctx context.Context, p Principal, typed string) (PhoneNumberAdded, error) {
	var added PhoneNumberAdded
	number, _, err := parsePhoneNumber(typed)
	if err != nil {
		return added, err
	}
	added.PhoneNumberToCall = number
	settings, err := checkPhoneNumberSettings(ContactMethodSettingsInput{Fields: map[string]string{"phone_number_to_call": number}})
	if err != nil {
		return added, err
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		if err := a.requireSendingServiceSetupTx(ctx, tx, &AutomatedVoiceCallContactMethodType{}); err != nil {
			return err
		}
		if err := checkNewPhoneNumber(ctx, tx, m.ID, number); err != nil {
			return err
		}

		contactMethodType, _ := a.contactMethodTypeFor("automated_voice_call")
		// Calls follow availability and team mute by default.
		c, err := createContactMethodTx(ctx, tx, contactMethodType, m.ID, settings, false, []string{"sos"}, m.Name, a.Now())
		if err != nil {
			return err
		}
		added.ContactMethods = []ContactMethod{c}

		return nil
	})
	return added, err
}

// checkNewPhoneNumber shares duplicate and per-person limits with command saves.
func checkNewPhoneNumber(ctx context.Context, tx *sql.Tx, memberID, number string) error {
	phoneKinds := contactMethodTypesWith(func(f ContactMethodTypeFacts) bool { return f.ValueFixed })
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contact_methods WHERE deleted=0 AND member_id=? AND sent_to=? AND contact_method_type IN (`+sqlPlaceholders(len(phoneKinds))+`)`, append([]any{memberID, number}, phoneKinds...)...).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return fmt.Errorf("%w: you already added %s", ErrConflict, number)
	}
	var numbers int
	if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT sent_to) FROM contact_methods WHERE deleted=0 AND member_id=? AND contact_method_type IN (`+sqlPlaceholders(len(phoneKinds))+`)`, append([]any{memberID}, phoneKinds...)...).Scan(&numbers); err != nil {
		return err
	}
	if numbers >= phoneNumbersPerMember {
		return fmt.Errorf("%w: you can add at most %d phone numbers. Remove an existing phone number first", ErrRateLimited, phoneNumbersPerMember)
	}
	return nil
}

// sosFallbackNote is added to the SOS that goes to a person's other contact
// methods when their phone call failed.
const sosFallbackNote = "MMHotline could not reach you with an automated voice call about this SOS."

// afterWakingDeliveryFailed: when every automated voice call contact method of
// a person that takes SOS failed definitively during an escalation step,
// their other contact methods (Slack, Telegram, email) get the SOS once for
// that step, with a claim button where the contact method type has one. d is the
// message that just failed for good; the incidents that joined it (a call or
// call about several) failed with it, and each of them is checked.
func afterWakingDeliveryFailed(ctx context.Context, tx *sql.Tx, d Delivery, now time.Time) error {
	if d.Purpose != "sos_escalation" || d.IncidentID == "" || d.MemberID == "" {
		return nil
	}
	var reason string
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT reason,next_attempt FROM deliveries WHERE id=?`, d.ID).Scan(&reason, &next); err != nil {
		return err
	}
	// A message that will be retried has not failed for good yet.
	if next != 0 {
		return nil
	}
	// Nothing reached the phone, so the joined messages are not "sent
	// together" but failed with it.
	if err := carrierEnded(ctx, tx, d.ID, "failed", reason); err != nil {
		return err
	}
	incidents := []string{d.IncidentID}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT incident_id FROM deliveries WHERE combined_into=? AND incident_id!=''`, d.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !slices.Contains(incidents, id) {
			incidents = append(incidents, id)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, incidentID := range incidents {
		if err = fallBackIfUnreachable(ctx, tx, incidentID, d.MemberID, now); err != nil {
			return err
		}
	}
	return nil
}

// fallBackIfUnreachable sends one incident's SOS to a member's other
// contact methods when each of their automated voice call contact methods
// that takes SOS was tried in the current step and failed for good, and
// none reached the phone or may still.
func fallBackIfUnreachable(ctx context.Context, tx *sql.Tx, incidentID, memberID string, now time.Time) error {
	i, err := readIncident(ctx, tx, incidentID)
	if err != nil || i.Status != "open" {
		return err
	}
	stepStart := i.stepStartedAt()
	waking := contactMethodTypesWith(func(f ContactMethodTypeFacts) bool { return f.WakesPeople })
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM contact_methods c JOIN contact_method_subscriptions s ON s.contact_method_id=c.id AND s.severity='sos' WHERE c.deleted=0 AND c.member_id=? AND c.contact_method_type IN (`+sqlPlaceholders(len(waking))+`)`, append([]any{memberID}, waking...)...)
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
	anyFailed := false
	for _, id := range ids {
		var failed, stillPossible int
		err = tx.QueryRowContext(ctx, `SELECT count(CASE WHEN state IN ('failed','suppressed') AND next_attempt=0 THEN 1 END),count(CASE WHEN NOT (state IN ('failed','suppressed','cancelled') AND next_attempt=0) THEN 1 END) FROM deliveries WHERE incident_id=? AND contact_method_id=? AND purpose='sos_escalation' AND created_at>=?`, incidentID, id, stepStart.Unix()).Scan(&failed, &stillPossible)
		if err != nil {
			return err
		}
		if stillPossible > 0 {
			return nil
		}
		// A contact method not tried in this step says nothing either way.
		anyFailed = anyFailed || failed > 0
	}
	if !anyFailed {
		return nil
	}
	return QueueNotification(ctx, tx, Notification{Key: fmt.Sprintf("sos_fallback:%s:%d:%s", i.ID, stepStart.Unix(), memberID), AlertID: i.OpeningAlertID, IncidentID: i.ID, Severity: "sos", Title: i.Title, Message: i.Message, Notes: sosFallbackNote, Purpose: "subscribed_alert", Recipients: []string{memberID}, PeopleOnly: true, At: now})
}

// contactMethodTypesThatWakeEachMember returns, for each member, the types
// of their working contact methods that reliably wake a sleeping person
// (Pushover or a phone call), such as "pushover_notification". It says
// nothing about whether the member is available.
func contactMethodTypesThatWakeEachMember(ctx context.Context, tx *sql.Tx) (map[string][]string, error) {
	kinds := contactMethodTypesWith(func(f ContactMethodTypeFacts) bool { return f.WakesSleepingPeople })
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT member_id,contact_method_type FROM contact_methods WHERE deleted=0 AND failing_since=0 AND member_id!='' AND contact_method_type IN (`+sqlPlaceholders(len(kinds))+`) ORDER BY member_id,contact_method_type DESC`, kinds...)
	if err != nil {
		return nil, err
	}
	wakes := map[string][]string{}
	for rows.Next() {
		var id, kind string
		if err = rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		wakes[id] = append(wakes[id], kind)
	}
	return wakes, errors.Join(rows.Err(), rows.Close())
}

// Send Test permits 5 calls per contact method and 20 per installation in 24 hours.
const (
	testsPerAutomatedVoiceCallContactMethodPerDay = 5
	phoneCallTestsPerInstallationPerDay           = 20
)

// phoneCallTestLimits refuses a Send Test on a automated voice call contact
// method beyond the daily limits.
func phoneCallTestLimits(ctx context.Context, tx *sql.Tx, c ContactMethod, now time.Time) error {
	if facts, _ := contactMethodTypeFacts(c.ContactMethodType); !facts.ValueFixed {
		return nil
	}
	since := now.Add(-24 * time.Hour).Unix()
	var tests int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE contact_method_id=? AND purpose='contact_method_test' AND created_at>?`, c.ID, since).Scan(&tests); err != nil {
		return err
	}
	if tests >= testsPerAutomatedVoiceCallContactMethodPerDay {
		return fmt.Errorf("%w: the automated voice call contact method was tested %d times today. Try again tomorrow", ErrRateLimited, tests)
	}
	kinds := contactMethodTypesWith(func(f ContactMethodTypeFacts) bool { return f.ValueFixed })
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE purpose='contact_method_test' AND created_at>? AND contact_method_type IN (`+sqlPlaceholders(len(kinds))+`)`, append([]any{since}, kinds...)...).Scan(&tests); err != nil {
		return err
	}
	if tests >= phoneCallTestsPerInstallationPerDay {
		return fmt.Errorf("%w: this installation's automated voice call contact methods were tested %d times today. Try again tomorrow", ErrRateLimited, tests)
	}
	return nil
}
