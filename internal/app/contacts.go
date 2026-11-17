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

const maxContactMethods = 1000

type ContactMethod struct {
	ID, MemberID, ContactMethodType, Settings, SentTo string
	Revision                                          int64
	LastTest                                          *Delivery
	SendWhileUnavailableOrMuted                       bool
	Subscriptions                                     []string
	// NewSecret is set only in the result of creating a contact method whose
	// contact method type made a secret, so it can be shown once.
	NewSecret string
	// TestNotSentReason is set only in the result of adding or changing a
	// contact method whose test was skipped because the daily Send Test limit
	// was reached. It is the sentence to show the person.
	TestNotSentReason string
	// PushoverGroupKey is set only in the result of adding a Pushover contact
	// method whose key Pushover says is a delivery-group key.
	PushoverGroupKey bool
	// OwnerSignInEmailAddress is the owner's, empty for a shared contact method.
	// EmergencyTestAcknowledgedAt is set on a Pushover contact method that has
	// proved it wakes its owner (pushoverEmergencyTestAcknowledgedAt).
	OwnerSignInEmailAddress     string
	EmergencyTestAcknowledgedAt time.Time
}

// ContactMethodInput is what a person saves. Fields holds the contact method type's own
// form fields, such as email_address.
type ContactMethodInput struct {
	ID, ContactMethodType               string
	Fields                              map[string]string
	Shared, SendWhileUnavailableOrMuted bool
	OwnerSignInEmailAddress             string // trusted command only; browser operations cannot select another owner
	ExpectedRevision                    int64
	Subscriptions                       []string
}

func scanContactMethod(row interface{ Scan(...any) error }) (ContactMethod, error) {
	var c ContactMethod
	err := row.Scan(&c.ID, &c.MemberID, &c.ContactMethodType, &c.Settings, &c.SentTo, &c.Revision, &c.SendWhileUnavailableOrMuted)
	return c, err
}

const contactMethodColumns = `id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted`

// Setting returns one stored setting, such as "email_address", for display.
func (c ContactMethod) Setting(field string) string { return settingValue(c.Settings, field) }

// SentToText is SentTo for people to read: a Pushover contact method that
// names one device adds "on device NAME" after the user key.
// SentToText is the destination as people and coding agents read it. A
// Pushover user key shows only its last four characters: output read in a
// terminal often lands in a coding agent's conversation.
func (c ContactMethod) SentToText() string {
	if c.ContactMethodType != "pushover_notification" {
		return c.SentTo
	}
	text := "the Pushover user key ending in " + c.SentTo[max(0, len(c.SentTo)-4):]
	if device := c.Setting("pushover_device_name"); device != "" {
		text += " on device " + device
	}
	return text
}

func contactMethodByID(ctx context.Context, tx *sql.Tx, id string) (ContactMethod, error) {
	c, err := scanContactMethod(tx.QueryRowContext(ctx, `SELECT `+contactMethodColumns+` FROM contact_methods WHERE id=? AND deleted=0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}
func canEditContactMethod(actor Member, c ContactMethod) bool {
	return c.MemberID == actor.ID || (c.MemberID == "" && actor.Role == RoleAdministrator)
}
func subscriptions(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT severity FROM contact_method_subscriptions WHERE contact_method_id=? ORDER BY severity`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}
func (a *App) ContactMethods(ctx context.Context, p Principal) ([]ContactMethod, error) {
	var out []ContactMethod
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+contactMethodColumns+` FROM contact_methods WHERE deleted=0 AND (member_id=? OR member_id='' OR ?=1) ORDER BY id`, m.ID, m.Role == RoleAdministrator)
		if err != nil {
			return err
		}
		for rows.Next() {
			c, e := scanContactMethod(rows)
			if e != nil {
				rows.Close()
				return e
			}
			out = append(out, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for i := range out {
			last, testErr := scanDelivery(tx.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE contact_method_id=? AND purpose='contact_method_test' ORDER BY created_at DESC,rowid DESC LIMIT 1`, out[i].ID))
			if testErr == nil {
				out[i].LastTest = &last
			} else if !errors.Is(testErr, sql.ErrNoRows) {
				return testErr
			}
			out[i].Subscriptions, err = subscriptions(ctx, tx, out[i].ID)
			if err != nil {
				return err
			}
			if out[i].MemberID != "" {
				if err = tx.QueryRowContext(ctx, `SELECT sign_in_email_address FROM members WHERE id=?`, out[i].MemberID).Scan(&out[i].OwnerSignInEmailAddress); err != nil {
					return err
				}
			}
			if out[i].ContactMethodType == "pushover_notification" {
				if out[i].EmergencyTestAcknowledgedAt, err = pushoverEmergencyTestAcknowledgedAt(ctx, tx, out[i].ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}
func (a *App) SaveContactMethod(ctx context.Context, p Principal, in ContactMethodInput) (ContactMethod, error) {
	var c ContactMethod
	contactMethodType, ok := a.contactMethodTypeFor(in.ContactMethodType)
	if !ok {
		return c, fmt.Errorf("%w: unknown kind of contact method", ErrInvalid)
	}
	facts := contactMethodType.Facts()
	if in.Shared && !facts.AllowedForTeamAccount {
		return c, fmt.Errorf("%w: %s cannot be a shared team contact method", ErrInvalid, facts.CardTitle)
	}
	// A contact method type connected through its sending service (Telegram) is added with its
	// connect button. Its settings never come from a form, so nobody can point
	// it at another chat.
	// A phone number (ValueFixed) is added with its own form, which creates
	// its automated voice call contact method; to change the number,
	// a person removes it and adds the new one.
	connected := facts.ConnectsThroughSendingService || facts.ValueFixed
	if facts.ValueFixed && in.ID == "" && p.UnixUser == "" {
		return c, fmt.Errorf("%w: add a phone number with the “Add a phone number” form on the Contact methods page", ErrInvalid)
	}
	if facts.ConnectsThroughSendingService && in.ID == "" {
		return c, fmt.Errorf("%w: add %s with its connect button on the Contact methods page", ErrInvalid, facts.CardTitle)
	}
	var settings ContactMethodSettings
	var err error
	if !connected || (facts.ValueFixed && in.ID == "") {
		if settings, err = contactMethodType.CheckSettings(ctx, ContactMethodSettingsInput{Fields: in.Fields}); err != nil {
			return c, err
		}
	}
	// Pushover tells a group key from a user key. A group key still works for
	// waking phones, so adding it is allowed; the person is warned. When
	// Pushover does not answer, no warning is made: the test message then shows
	// any problem with the key.
	pushoverGroupKey := false
	if pushover, isPushover := contactMethodType.(*PushoverNotificationContactMethodType); isPushover && pushover.ApplicationToken != "" {
		// A new contact method is always checked. A saved one is checked
		// again only when it names a device, since the person may have
		// changed the device name.
		device := settingValue(settings.JSON, "pushover_device_name")
		if in.ID == "" || device != "" {
			group, err := pushover.checkUserKeyAndDevice(ctx, settings.SentTo, device)
			if err != nil {
				return c, err
			}
			pushoverGroupKey = group && in.ID == ""
		}
	}
	values := append([]string(nil), in.Subscriptions...)
	slices.Sort(values)
	for i, v := range values {
		if (v != "info" && v != "warn" && v != "sos") || (i > 0 && v == values[i-1]) {
			return c, ErrInvalid
		}
		if facts.SOSOnly && v != "sos" {
			return c, fmt.Errorf("%w: %s contact method is only for SOS", ErrInvalid, cardTitleWithArticle(facts))
		}
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		if _, needsSetup := contactMethodType.(needsSendingService); needsSetup {
			if err = a.requireSendingServiceSetupTx(ctx, tx, contactMethodType); err != nil {
				return err
			}
		}
		if in.ID == "" {
			if in.ExpectedRevision != 0 {
				return ErrConflict
			}
			memberID := m.ID
			if in.OwnerSignInEmailAddress != "" {
				if p.UnixUser == "" {
					return ErrForbidden
				}
				owner, err := personBySignInEmailAddress(ctx, tx, strings.ToLower(in.OwnerSignInEmailAddress))
				if err != nil {
					return err
				}
				memberID = owner.ID
			}
			if !in.Shared && memberID == "" {
				return fmt.Errorf("%w: a personal contact method needs a sign-in email address", ErrInvalid)
			}
			if facts.ValueFixed {
				if err := checkNewPhoneNumber(ctx, tx, memberID, settings.SentTo); err != nil {
					return err
				}
			}
			if in.Shared {
				if m.Role != RoleAdministrator {
					return ErrForbidden
				}
				memberID = ""
			}
			c, err = createContactMethodTx(ctx, tx, contactMethodType, memberID, settings, in.SendWhileUnavailableOrMuted, values, m.Name, a.Now())
			return err
		} else {
			c, err = contactMethodByID(ctx, tx, in.ID)
			if err != nil {
				return err
			}
			if p.UnixUser == "" && !canEditContactMethod(m, c) {
				return ErrForbidden
			}
			if c.Revision != in.ExpectedRevision {
				return ErrConflict
			}
			if c.ContactMethodType != contactMethodType.Name() {
				return fmt.Errorf("%w: a contact method cannot change its kind", ErrInvalid)
			}
			if connected {
				settings = ContactMethodSettings{JSON: c.Settings, SentTo: c.SentTo}
			}
			if in.Shared != (c.MemberID == "") {
				return fmt.Errorf("%w: a contact method cannot change owner", ErrInvalid)
			}
			c.Subscriptions, err = subscriptions(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			if c.Settings == settings.JSON && c.SendWhileUnavailableOrMuted == in.SendWhileUnavailableOrMuted && slices.Equal(c.Subscriptions, values) {
				return nil
			}
			if facts.MaximumPerPerson > 0 {
				if err = checkPersonalContactMethodLimit(ctx, tx, contactMethodType, c.MemberID, c.ID, settings.SentTo); err != nil {
					return err
				}
			}
			addressChanged := c.Settings != settings.JSON
			c.Settings = settings.JSON
			c.SentTo = settings.SentTo
			c.SendWhileUnavailableOrMuted = in.SendWhileUnavailableOrMuted
			c.Revision++
			// New settings start a new count of failures in a row.
			_, err = tx.ExecContext(ctx, `UPDATE contact_methods SET settings_json=?,sent_to=?,send_while_unavailable_or_muted=?,revision=?,failures_in_a_row=CASE WHEN settings_json=? THEN failures_in_a_row ELSE 0 END,failing_since=CASE WHEN settings_json=? THEN failing_since ELSE 0 END WHERE id=?`, c.Settings, c.SentTo, c.SendWhileUnavailableOrMuted, c.Revision, c.Settings, c.Settings, c.ID)
			if err != nil {
				return err
			}
			// Only a new destination cancels the work queued for the old one.
			// Subscriptions and availability are checked when a message goes
			// (DeliveryAllowed), and a message already queued for a severity
			// the person unticked still goes: it was wanted when it was queued.
			// The new revision still catches two people saving at once.
			if addressChanged {
				err = cancelContactMethodWork(ctx, tx, c.ID, "contact method settings changed")
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE deliveries SET contact_method_revision=? WHERE contact_method_id=? AND contact_method_revision=?`, c.Revision, c.ID, c.Revision-1)
			}
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM contact_method_subscriptions WHERE contact_method_id=?`, c.ID); err != nil {
				return err
			}
			if addressChanged {
				if err = queueSaveTest(ctx, tx, &c, m.Name, "changed", a.Now()); err != nil {
					return err
				}
			}
		}
		for _, v := range values {
			if _, err = tx.ExecContext(ctx, `INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,?)`, c.ID, v); err != nil {
				return err
			}
		}
		c.Subscriptions = values
		return nil
	})
	c.PushoverGroupKey = err == nil && pushoverGroupKey
	return c, err
}

// createContactMethodTx adds a contact method in the caller's team
// transaction, within the installation's limits. memberID is empty for a shared
// contact method. Every new contact method can send from the start and queues a test.
func createContactMethodTx(ctx context.Context, tx *sql.Tx, contactMethodType ContactMethodType, memberID string, settings ContactMethodSettings, sendWhileUnavailableOrMuted bool, subscriptions []string, actorName string, now time.Time) (ContactMethod, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contact_methods WHERE deleted=0`).Scan(&count); err != nil {
		return ContactMethod{}, err
	}
	if count >= maxContactMethods {
		return ContactMethod{}, fmt.Errorf("%w: contact method limit reached", ErrRateLimited)
	}
	facts := contactMethodType.Facts()
	if facts.MaximumPerPerson > 0 {
		if err := checkPersonalContactMethodLimit(ctx, tx, contactMethodType, memberID, "", settings.SentTo); err != nil {
			return ContactMethod{}, err
		}
	}
	if facts.MaximumPerInstallation > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contact_methods WHERE deleted=0 AND contact_method_type=?`, contactMethodType.Name()).Scan(&count); err != nil {
			return ContactMethod{}, err
		}
		if count >= facts.MaximumPerInstallation {
			return ContactMethod{}, fmt.Errorf("%w: an installation can have at most %d %s contact methods", ErrRateLimited, facts.MaximumPerInstallation, cardTitleInSentence(facts))
		}
	}
	c := ContactMethod{ID: NewID(), MemberID: memberID, ContactMethodType: contactMethodType.Name(), Settings: settings.JSON, SentTo: settings.SentTo, Revision: 1, SendWhileUnavailableOrMuted: sendWhileUnavailableOrMuted, NewSecret: settings.Secret, Subscriptions: subscriptions}
	_, err := tx.ExecContext(ctx, `INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,secret,sent_to,revision,send_while_unavailable_or_muted) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.MemberID, c.ContactMethodType, c.Settings, settings.Secret, c.SentTo, c.Revision, c.SendWhileUnavailableOrMuted)
	if err != nil {
		return c, err
	}
	for _, v := range subscriptions {
		if _, err = tx.ExecContext(ctx, `INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,?)`, c.ID, v); err != nil {
			return c, err
		}
	}
	return c, queueSaveTest(ctx, tx, &c, actorName, "added", now)
}
func cancelContactMethodWork(ctx context.Context, tx *sql.Tx, id, reason string) error {
	return cancelDeliveries(ctx, tx, `contact_method_id=? AND `+pendingDelivery, reason, id)
}
func (a *App) RemoveContactMethod(ctx context.Context, p Principal, id string, revision int64) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		c, err := contactMethodByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.UnixUser == "" && !canEditContactMethod(m, c) {
			return ErrForbidden
		}
		if c.Revision != revision {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE contact_methods SET deleted=1,revision=revision+1 WHERE id=?`, id); err != nil {
			return err
		}
		return cancelContactMethodWork(ctx, tx, id, "contact method removed")
	})
}

func (a *App) SendContactMethodTest(ctx context.Context, p Principal, id string) error {
	now := a.Now()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		c, err := contactMethodByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.UnixUser == "" && !canEditContactMethod(m, c) {
			return ErrForbidden
		}
		var last int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at),0) FROM deliveries WHERE contact_method_id=? AND purpose='contact_method_test'`, id).Scan(&last); err != nil {
			return err
		}
		if last > now.Add(-time.Minute).Unix() {
			return ErrRateLimited
		}
		if err = phoneCallTestLimits(ctx, tx, c, now); err != nil {
			return err
		}
		return queueContactMethodTest(ctx, tx, c, m.Name, "requested a test of", now)
	})
}

// queueSaveTest queues the test made when a contact method is added or its
// destination changes. An automated voice call test counts against the same
// daily limits as Send test, so adding and removing a number again and again
// cannot make the team's Twilio account call it again and again. At the limit
// the save still succeeds and the test is skipped; c.TestNotSentReason says why.
func queueSaveTest(ctx context.Context, tx *sql.Tx, c *ContactMethod, actorName, action string, now time.Time) error {
	if err := phoneCallTestLimits(ctx, tx, *c, now); err != nil {
		if !errors.Is(err, ErrRateLimited) {
			return err
		}
		c.TestNotSentReason = "Test not sent: " + publicError(err)
		return nil
	}
	return queueContactMethodTest(ctx, tx, *c, actorName, action, now)
}

// queueContactMethodTest shares the durable outbox for saves and Send test.
// Save tests bypass the queue capacity so setup cannot be refused
// because notifications are backed up. Sending service failures happen after the save.
func queueContactMethodTest(ctx context.Context, tx *sql.Tx, c ContactMethod, actorName, action string, now time.Time) error {
	key := NewID()
	notificationKey := "test/" + key
	if action != "requested a test of" {
		notificationKey = "contact_method_saved/" + key
	}
	return insertDelivery(ctx, tx, Delivery{ID: key, NotificationKey: notificationKey, ContactMethodID: c.ID, ContactMethodRevision: c.Revision, MemberID: c.MemberID, ContactMethodType: c.ContactMethodType, SentTo: c.SentTo, Purpose: "contact_method_test", Subject: "MMHotline test message", Message: fmt.Sprintf("%s %s this contact method at %s.\n\nThis is a test of this contact method, not an incident.", actorName, action, now.UTC().Format(time.RFC3339)), State: "queued", CreatedAt: now, NextAttempt: now})
}

func baseNotificationSeverity(severity string) string {
	return strings.TrimPrefix(severity, "resolved-")
}

func (a *App) requireSendingServiceSetupTx(ctx context.Context, tx *sql.Tx, contactMethodType ContactMethodType) error {
	definition := contactMethodType.(needsSendingService).Setup()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sending_service_setups WHERE sending_service=?`, definition.Name).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: %s is not set up yet: an administrator adds sending_services.%s to the settings file %s and applies the settings file; %s/sending-services/%s says how", ErrInvalid, definition.Title, definition.Name, a.settingsFile.path, a.WebURL, definition.Name)
	}
	return nil
}

// ContactMethodAlreadyAdded starts the refusal of a contact method the person
// already has; connect-pushover tells a re-run by it.
const ContactMethodAlreadyAdded = "you already added this"

func checkPersonalContactMethodLimit(ctx context.Context, tx *sql.Tx, contactMethodType ContactMethodType, memberID, exceptID, destination string) error {
	facts := contactMethodType.Facts()
	var count, duplicate int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(SUM(sent_to=?),0) FROM contact_methods WHERE deleted=0 AND contact_method_type=? AND member_id=? AND id!=?`, destination, contactMethodType.Name(), memberID, exceptID).Scan(&count, &duplicate); err != nil {
		return err
	}
	if duplicate > 0 {
		return fmt.Errorf("%w: %s %s contact method", ErrConflict, ContactMethodAlreadyAdded, facts.CardTitle)
	}
	if exceptID == "" && count >= facts.MaximumPerPerson {
		return fmt.Errorf("%w: each person can add at most %d %s contact methods to limit the number of accidental duplicate notifications; remove a contact method first", ErrRateLimited, facts.MaximumPerPerson, facts.CardTitle)
	}
	return nil
}
