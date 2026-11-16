package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

const sendingServiceSetupSchema = `
CREATE TABLE sending_service_setups (
 sending_service TEXT PRIMARY KEY,
 settings_json TEXT NOT NULL,
 secret_settings_json TEXT NOT NULL,
 set_up_at INTEGER NOT NULL,
 set_up_by TEXT NOT NULL
);
`

// SendingServiceSetupDefinition declares installation setup beside a contact method type's facts.
// Only contact method types that need database-backed setup implement needsSendingService.
type SendingServiceSetupDefinition struct {
	Name, Title string
	// TitleInSentence names a service whose Title is not a product name
	// inside a sentence, such as "the mail server"; empty means Title.
	TitleInSentence  string
	Fields           []SendingServiceSetupField
	Guide            []SendingServiceSetupStep
	NextStep         string
	IdentitySecret   bool // compare the declared identity in secret settings
	IdentitySetting  string
	IdentityLabel    string
	DescribeCheck    func(map[string]any) string
	Check            func(context.Context, map[string]string, string) (sendingServiceSetupCheck, error)
	Build            func(map[string]any, map[string]string, string) ContactMethodType
	ClearConnections func(context.Context, *sql.Tx) error
	// ReplacingKeepsContactMethods accepts another identity while contact
	// methods use the sending service, because they work with any identity:
	// a Pushover user key works with any Pushover application.
	ReplacingKeepsContactMethods bool
}
type SendingServiceSetupField struct {
	Name, Label, Help string
	// SettingsKey is the field's key in the service's section of the
	// settings file, such as application_token in sending_services.pushover.
	SettingsKey        string
	Secret             bool
	Optional           bool
	PreserveWhitespace bool
}
type SendingServiceSetupStep struct{ Text, URL, LinkLabel string }
type needsSendingService interface {
	Setup() SendingServiceSetupDefinition
}
type sendingServiceSetupCheck struct {
	Settings       map[string]any
	SecretSettings map[string]string
}

// SendingServiceSetup contains only public settings. Secrets are never part of a view.
type SendingServiceSetup struct {
	SendingService      string         `json:"sending_service"`
	SendingServiceTitle string         `json:"sending_service_title"`
	IsSetUp             bool           `json:"is_set_up"`
	Settings            map[string]any `json:"settings"`
	SetUpAt             *time.Time     `json:"set_up_at,omitempty"`
	SetUpBy             string         `json:"set_up_by,omitempty"`
}

// TitleInSentence is the service's name inside a sentence: a product name
// keeps its capital, and "Mail server" becomes "the mail server".
func (s SendingServiceSetup) TitleInSentence() string {
	if _, definition, err := sendingServiceSetup(s.SendingService); err == nil && definition.TitleInSentence != "" {
		return definition.TitleInSentence
	}
	return s.SendingServiceTitle
}

func (s SendingServiceSetup) Description() string {
	if !s.IsSetUp {
		return ""
	}
	_, definition, err := sendingServiceSetup(s.SendingService)
	if err != nil {
		return ""
	}
	return definition.DescribeCheck(s.Settings)
}

type SendingServiceSetupResult struct {
	Setup             SendingServiceSetup
	Checked           string
	NextStep          string
	ContactMethodsURL string
}

func sendingServiceSetup(name string) (ContactMethodType, SendingServiceSetupDefinition, error) {
	var available []string
	for _, contactMethodType := range contactMethodTypes {
		owner, ok := contactMethodType.(needsSendingService)
		if !ok {
			continue
		}
		definition := owner.Setup()
		available = append(available, definition.Name)
		if definition.Name == name {
			return contactMethodType, definition, nil
		}
	}
	return nil, SendingServiceSetupDefinition{}, fmt.Errorf("%w: sending service must be one of: %s", ErrInvalid, strings.Join(available, ", "))
}

// configuredContactMethodTypes returns an immutable map. Copying happens only on setup.
func (a *App) configuredContactMethodTypes() map[string]ContactMethodType {
	a.contactMethodTypesMu.RLock()
	defer a.contactMethodTypesMu.RUnlock()
	return a.ContactMethodTypes
}
func (a *App) publishContactMethodTypes(contactMethodTypes map[string]ContactMethodType) {
	a.contactMethodTypesMu.Lock()
	a.ContactMethodTypes = contactMethodTypes
	close(a.contactMethodTypesChanged)
	a.contactMethodTypesChanged = make(chan struct{})
	a.contactMethodTypesMu.Unlock()
	// The transaction notification can precede publication. Wake the scheduler
	// again so its deadline facts include the newly available contact method type.
	a.teamDeadline.changed()
}

// LoadSendingServiceSetups builds configured contact method types before serving requests. Contact method types
// still configured by the environment are supplied separately during this build.
func (a *App) LoadSendingServiceSetups(ctx context.Context, startupContactMethodTypes map[string]ContactMethodType, fakeSendingServicesURL string) error {
	a.sendingServiceSetupMu.Lock()
	defer a.sendingServiceSetupMu.Unlock()
	a.fakeSendingServicesURL = fakeSendingServicesURL
	if fakeSendingServicesURL != "" {
		a.triageAgent.codingAgentAPIBaseURLs = codingAgentAPIBaseURLs(fakeSendingServicesURL)
	}
	configured := maps.Clone(startupContactMethodTypes)
	if configured == nil {
		configured = map[string]ContactMethodType{}
	}
	// Database-backed contact method types can never come from the startup map.
	for _, contactMethodType := range contactMethodTypes {
		if _, ok := contactMethodType.(needsSendingService); ok {
			delete(configured, contactMethodType.Name())
		}
	}
	rows, err := a.Store.db.QueryContext(ctx, `SELECT sending_service,settings_json,secret_settings_json FROM sending_service_setups`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sendingServiceName, settingsJSON, secretJSON string
		if err = rows.Scan(&sendingServiceName, &settingsJSON, &secretJSON); err != nil {
			return err
		}
		_, definition, err := sendingServiceSetup(sendingServiceName)
		if err != nil {
			return err
		}
		var settings map[string]any
		var secrets map[string]string
		if json.Unmarshal([]byte(settingsJSON), &settings) != nil || json.Unmarshal([]byte(secretJSON), &secrets) != nil {
			return errors.New("invalid stored sending service setup")
		}
		configuredType := definition.Build(settings, secrets, fakeSendingServicesURL)
		configured[configuredType.Name()] = configuredType
	}
	if err = rows.Err(); err != nil {
		return err
	}
	a.publishContactMethodTypes(configured)
	return nil
}

func (a *App) SendingServiceSetups(ctx context.Context, p Principal) ([]SendingServiceSetup, error) {
	setups := []SendingServiceSetup{}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		for _, contactMethodType := range contactMethodTypes {
			owner, ok := contactMethodType.(needsSendingService)
			if !ok {
				continue
			}
			definition := owner.Setup()
			setup := SendingServiceSetup{SendingService: definition.Name, SendingServiceTitle: definition.Title, Settings: map[string]any{}}
			var settings string
			var at int64
			err := tx.QueryRowContext(ctx, `SELECT settings_json,set_up_at,set_up_by FROM sending_service_setups WHERE sending_service=?`, setup.SendingService).Scan(&settings, &at, &setup.SetUpBy)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if err = json.Unmarshal([]byte(settings), &setup.Settings); err != nil {
					return err
				}
				setup.IsSetUp = true
				timestamp := time.Unix(at, 0).UTC()
				setup.SetUpAt = &timestamp
			}
			setups = append(setups, setup)
		}
		return nil
	})
	return setups, err
}

func (a *App) SetUpSendingService(ctx context.Context, p Principal, sendingServiceName string, fields map[string]string) (SendingServiceSetupResult, error) {
	var result SendingServiceSetupResult
	contactMethodType, definition, err := sendingServiceSetup(sendingServiceName)
	if err != nil {
		return result, err
	}
	if err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { _, err := RequireMember(ctx, tx, p, true); return err }); err != nil {
		return result, err
	}
	input := map[string]string{}
	for _, field := range definition.Fields {
		input[field.Name] = fields[field.Name]
		if !field.PreserveWhitespace {
			input[field.Name] = strings.TrimSpace(input[field.Name])
		}
		if input[field.Name] == "" && !field.Optional {
			return result, fmt.Errorf("%w: %s is required", ErrInvalid, field.Label)
		}
	}
	// The sending service call runs outside both the setup lock and the transaction.
	checked, err := definition.Check(ctx, input, a.fakeSendingServicesURL)
	if err != nil {
		return result, err
	}
	settings, err := json.Marshal(checked.Settings)
	if err != nil {
		return result, err
	}
	secrets, err := json.Marshal(checked.SecretSettings)
	if err != nil {
		return result, err
	}
	configured := definition.Build(checked.Settings, checked.SecretSettings, a.fakeSendingServicesURL)
	a.sendingServiceSetupMu.Lock()
	defer a.sendingServiceSetupMu.Unlock()
	now := a.Now().UTC().Truncate(time.Second)
	var actor Member
	var replacedForContactMethods bool
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		actor, err = RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		var previous, previousSecrets string
		err = tx.QueryRowContext(ctx, `SELECT settings_json,secret_settings_json FROM sending_service_setups WHERE sending_service=?`, sendingServiceName).Scan(&previous, &previousSecrets)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		previousIdentity, nextIdentity := previous, string(settings)
		if definition.IdentitySecret {
			previousIdentity, nextIdentity = previousSecrets, string(secrets)
		}
		identityChanged := previous != "" && settingIdentity(previousIdentity, definition.IdentitySetting) != settingIdentity(nextIdentity, definition.IdentitySetting)
		if identityChanged {
			if definition.ReplacingKeepsContactMethods {
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_methods WHERE contact_method_type=? AND deleted=0)`, contactMethodType.Name()).Scan(&replacedForContactMethods); err != nil {
					return err
				}
			} else if err = refuseContactMethodTypeInUse(ctx, tx, contactMethodType, "change to another "+definition.IdentityLabel); err != nil {
				return err
			}
			if definition.ClearConnections != nil {
				if err = definition.ClearConnections(ctx, tx); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sending_service_setups(sending_service,settings_json,secret_settings_json,set_up_at,set_up_by) VALUES(?,?,?,?,?) ON CONFLICT(sending_service) DO UPDATE SET settings_json=excluded.settings_json,secret_settings_json=excluded.secret_settings_json,set_up_at=excluded.set_up_at,set_up_by=excluded.set_up_by`, sendingServiceName, string(settings), string(secrets), now.Unix(), actor.Name)
		return err
	})
	if err != nil {
		return result, err
	}
	contactMethodTypes := maps.Clone(a.configuredContactMethodTypes())
	if contactMethodTypes == nil {
		contactMethodTypes = map[string]ContactMethodType{}
	}
	contactMethodTypes[contactMethodType.Name()] = configured
	a.publishContactMethodTypes(contactMethodTypes)
	if contactMethodType.Name() == "automated_voice_call" {
		if err := a.AnnounceContactCardChange(ctx); err != nil {
			return result, err
		}
	}
	result = SendingServiceSetupResult{Setup: SendingServiceSetup{SendingService: definition.Name, SendingServiceTitle: definition.Title, IsSetUp: true, Settings: checked.Settings, SetUpAt: &now, SetUpBy: actor.Name}, Checked: definition.DescribeCheck(checked.Settings), NextStep: definition.NextStep, ContactMethodsURL: a.WebURL + "/contact-methods"}
	if replacedForContactMethods {
		result.NextStep = fmt.Sprintf("Every %s contact method now sends through the new %s. Send a test notification to a contact method with Send test on the Contact methods page or mmhotline contact-methods send-test --contact-method-id ID:", cardTitleInSentence(contactMethodType.Facts()), definition.IdentityLabel)
	}
	return result, nil
}
func settingIdentity(settings, field string) string {
	var values map[string]json.RawMessage
	_ = json.Unmarshal([]byte(settings), &values)
	return string(values[field])
}
func refuseContactMethodTypeInUse(ctx context.Context, tx *sql.Tx, contactMethodType ContactMethodType, action string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contact_methods WHERE contact_method_type=? AND deleted=0`, contactMethodType.Name()).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		kind := cardTitleInSentence(contactMethodType.Facts())
		usage := fmt.Sprintf("%d %s contact methods use the sending service setup", count, kind)
		if count == 1 {
			usage = fmt.Sprintf("1 %s contact method uses the sending service setup", kind)
		}
		return fmt.Errorf("%w: cannot %s: %s. Remove those contact methods on the Contact methods page, or with mmhotline contact-methods remove --contact-method-id ID, first", ErrConflict, action, usage)
	}
	return nil
}
func (a *App) RemoveSendingServiceSetup(ctx context.Context, p Principal, sendingServiceName string) error {
	contactMethodType, definition, err := sendingServiceSetup(sendingServiceName)
	if err != nil {
		return err
	}
	a.sendingServiceSetupMu.Lock()
	defer a.sendingServiceSetupMu.Unlock()
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		if err := refuseContactMethodTypeInUse(ctx, tx, contactMethodType, "remove the "+definition.Title+" setup"); err != nil {
			return err
		}
		if definition.ClearConnections != nil {
			if err := definition.ClearConnections(ctx, tx); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sending_service_setups WHERE sending_service=?`, sendingServiceName)
		return err
	})
	if err != nil {
		return err
	}
	contactMethodTypes := maps.Clone(a.configuredContactMethodTypes())
	delete(contactMethodTypes, contactMethodType.Name())
	a.publishContactMethodTypes(contactMethodTypes)
	return nil
}
