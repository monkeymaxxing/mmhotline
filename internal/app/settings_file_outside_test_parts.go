package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"mmhotline/internal/statuspage"
)

// The parts of the settings file with an outside test: the agents, each
// sending service, the MMHotline-down alarm, and the status page. Each applies through
// the outside test the feature always had, and a changed part is saved only
// when that test passes: docs/changing-mmhotline/design/settings-file.md#parts.

// settingsTriageAgentSetupPart is the triage agent's coding agent,
// credential, model, and reasoning effort; its other keys are parts of
// their own, which apply without an outside test.
var settingsTriageAgentSetupPart = settingsFilePart{
	name:           "triage_agent.coding_agent, triage_agent.credential_type, triage_agent.credential, triage_agent.credential_expires_on, triage_agent.model, triage_agent.reasoning_effort",
	value:          triageAgentValue(func(agent SettingsTriageAgent) any { return agent.SettingsCodingAgent }),
	hasOutsideTest: true,
	apply: func(ctx context.Context, a *App, s Settings) error {
		var setup *SettingsCodingAgent
		if s.TriageAgent != nil {
			setup = &s.TriageAgent.SettingsCodingAgent
		}
		return a.applyCodingAgentSettings(ctx, triageAgentRole, setup)
	},
}

// settingsPartsWithAnOutsideTest are the other parts, in the order they
// apply: the sending services before the MMHotline-down alarm, whose held messages use
// them.
var settingsPartsWithAnOutsideTest = []settingsFilePart{
	{
		name:           "review_agent",
		value:          fileValue(func(s Settings) any { return s.ReviewAgent }),
		hasOutsideTest: true,
		apply: func(ctx context.Context, a *App, s Settings) error {
			return a.applyCodingAgentSettings(ctx, reviewAgentRole, s.ReviewAgent)
		},
	},
	sendingServicePart("pushover", func(services SettingsSendingServices) any { return services.Pushover }),
	sendingServicePart("twilio", func(services SettingsSendingServices) any { return services.Twilio }),
	sendingServicePart("telegram", func(services SettingsSendingServices) any { return services.Telegram }),
	sendingServicePart("slack", func(services SettingsSendingServices) any { return services.Slack }),
	sendingServicePart("mail_server", func(services SettingsSendingServices) any { return services.MailServer }),
	{
		name: "mmhotline_down_alarm",
		// The wake times are their own part, so changing them never sets up
		// the MMHotline-down alarm again.
		value: fileValue(func(s Settings) any {
			if s.DownAlarm == nil {
				return nil
			}
			return struct{ CloudflareAccountID, CloudflareAPIToken string }{s.DownAlarm.CloudflareAccountID, s.DownAlarm.CloudflareAPIToken}
		}),
		hasOutsideTest: true,
		apply: func(ctx context.Context, a *App, s Settings) error {
			if s.DownAlarm == nil {
				err := a.RemoveDownAlarmSetup(ctx, a.settingsPrincipal())
				if errors.Is(err, ErrNotFound) {
					return nil
				}
				return err
			}
			_, err := a.SetUpDownAlarm(ctx, a.settingsPrincipal(), DownAlarmSetUpInput{CloudflareAccountID: s.DownAlarm.CloudflareAccountID, CloudflareAPIToken: s.DownAlarm.CloudflareAPIToken})
			return err
		},
	},
	{
		name:           "status_page",
		value:          fileValue(func(s Settings) any { return s.StatusPage }),
		hasOutsideTest: true,
		apply: func(ctx context.Context, a *App, s Settings) error {
			return a.applyStatusPageSettings(ctx, s.StatusPage)
		},
	},
}

// errTriageRunActive is why an agent's part did not apply while another run
// held the one triage run, which its setup test needs. The run is the
// reason, and trying again costs nothing.
var errTriageRunActive = fmt.Errorf("%w: a triage agent run was active, so the new agent setup was not tested. Apply the settings file again after the triage agent run ends", errSettingsPartRefused)

// applyCodingAgentSettings applies role's setup from the settings file.
// Without its section, the setup is removed. With it, the setup test runs
// with the file's coding agent, credential, model, and reasoning effort,
// and the setup is saved only when the test passes, so a model the service
// refuses keeps the earlier setup (CA6).
func (a *App) applyCodingAgentSettings(ctx context.Context, role agentRole, setup *SettingsCodingAgent) error {
	if setup == nil {
		return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM `+role.setupTable)
			return err
		})
	}
	run, err := a.startCodingAgentSetupTest(ctx, role, *setup, a.settingsPrincipalName())
	if errors.Is(err, ErrConflict) {
		return errTriageRunActive
	}
	if err != nil {
		return err
	}
	select {
	case <-run.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	// The run's reader has set the result, with the credential removed,
	// before it closed done.
	if result := run.result; result.Event != "finished" || result.Reason != "" {
		reason := strings.TrimSpace(result.Reason)
		if reason == "" {
			reason = "the agent setup test did not finish"
		}
		return fmt.Errorf("%w: the %s's setup test, with the file's settings, failed: %s", errSettingsPartRefused, role.name, strings.TrimSuffix(reason, "."))
	}
	return nil
}

// settingsPrincipalName is how what the settings file changes names who
// changed it, such as "the settings file /etc/mmhotline/mmhotline_settings.yml".
func (a *App) settingsPrincipalName() string {
	return "the settings file " + a.settingsFile.path
}

// sendingServicePart is one service of sending_services. Applying it runs
// the service's own check, such as Pushover's limits endpoint, and saves it
// as the sending service setup; without its section, the setup is removed.
// The setup's refusals, such as removing a service that contact methods
// use, fail the part with the earlier setup kept.
func sendingServicePart(service string, section func(SettingsSendingServices) any) settingsFilePart {
	_, definition, err := sendingServiceSetup(service)
	if err != nil {
		panic(err)
	}
	return settingsFilePart{
		name:           "sending_services." + service,
		value:          fileValue(func(s Settings) any { return section(s.SendingServices) }),
		hasOutsideTest: true,
		nextStep:       definition.NextStep,
		apply: func(ctx context.Context, a *App, s Settings) error {
			value := reflect.ValueOf(section(s.SendingServices))
			if value.IsNil() {
				return a.RemoveSendingServiceSetup(ctx, a.settingsPrincipal(), service)
			}
			_, err := a.SetUpSendingService(ctx, a.settingsPrincipal(), service, sendingServiceFields(definition, value.Elem()))
			return err
		},
	}
}

// sendingServiceFields are a sending service's setup fields from its
// section of the settings file: each field's value is the key its
// SettingsKey names, and a list is its items joined by commas.
func sendingServiceFields(definition SendingServiceSetupDefinition, section reflect.Value) map[string]string {
	fields := map[string]string{}
	for _, field := range definition.Fields {
		for index := range section.NumField() {
			name, _, _ := strings.Cut(section.Type().Field(index).Tag.Get("yaml"), ",")
			if name != field.SettingsKey {
				continue
			}
			switch value := section.Field(index).Interface().(type) {
			case string:
				fields[field.Name] = value
			case []string:
				fields[field.Name] = strings.Join(value, ",")
			}
		}
	}
	return fields
}

// pageConfig is the public page's configuration, as status.json holds it.
func (page SettingsStatusPage) pageConfig() statuspage.PageConfig {
	config := statuspage.PageConfig{PageTitle: page.PageTitle, HostName: page.HostName, WebsiteURL: page.WebsiteURL, SupportURL: page.SupportURL}
	for _, c := range page.Components {
		config.Components = append(config.Components, statuspage.Component{ComponentID: c.ComponentID, Name: c.Name, Description: c.Description})
	}
	return config
}

// applyStatusPageSettings posts the current page with the file's R2 key,
// then saves the setup. The post writes status.json and index.html with the
// file's page_title, host_name, website_url, support_url, and components,
// creating status.json when the bucket has none (only this apply does; an
// ordinary post to a bucket without one is refused), so a key that cannot
// write the bucket is refused and never saved. When the file names another
// bucket than the saved setup, the links from incidents to notices are
// removed with the setup: notice IDs start again at 1 in a new bucket, and
// an old link would point at another notice. Without the section, the setup
// and the links are removed; the bucket and its page stay in Cloudflare, where a laptop with
// the key can still post.
func (a *App) applyStatusPageSettings(ctx context.Context, page *SettingsStatusPage) error {
	if page == nil {
		return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			// The links go with the setup: enabling the page again, perhaps on
			// another bucket where notice IDs start at 1, must not find them.
			if _, err := tx.ExecContext(ctx, `DELETE FROM status_page_setup`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM status_page_incident_notices`)
			return err
		})
	}
	hostName, err := checkStatusPageHostName(page.HostName)
	if err != nil {
		return err
	}
	pageTitle, err := statuspage.CheckPageTitle(page.PageTitle)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(err.Error(), statuspage.ErrRefused.Error()+": "))
	}
	bucketURL, err := statuspage.CheckBucketURL(page.R2BucketURL)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	accountID, bucketName := r2BucketURLParts(bucketURL)
	setup := statusPageSetup{accountID: accountID, hostName: hostName, bucketName: bucketName, bucketURL: bucketURL, pageTitle: pageTitle, accessKeyID: strings.TrimSpace(page.R2AccessKeyID), secretAccessKey: strings.TrimSpace(page.R2SecretAccessKey)}
	config := page.pageConfig()
	config.PageTitle, config.HostName = pageTitle, hostName
	if _, _, err := statuspage.Publish(ctx, a.statusPageBucket(setup), statuspage.ApplyPageConfig(config), pageTitle, a.Now()); err != nil {
		return fmt.Errorf("%w: posting the current status page with the settings file's R2 key failed: %s", errSettingsPartRefused, strings.TrimSuffix(err.Error(), "."))
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var previousBucketURL string
		switch err := tx.QueryRowContext(ctx, `SELECT r2_bucket_url FROM status_page_setup WHERE id=1`).Scan(&previousBucketURL); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case previousBucketURL != setup.bucketURL:
			if _, err := tx.ExecContext(ctx, `DELETE FROM status_page_incident_notices`); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO status_page_setup(id,cloudflare_account_id,host_name,bucket_name,r2_bucket_url,page_title,r2_access_key_id,r2_secret_access_key,set_up_at,set_up_by) VALUES(1,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET cloudflare_account_id=excluded.cloudflare_account_id,host_name=excluded.host_name,bucket_name=excluded.bucket_name,r2_bucket_url=excluded.r2_bucket_url,page_title=excluded.page_title,r2_access_key_id=excluded.r2_access_key_id,r2_secret_access_key=excluded.r2_secret_access_key,set_up_at=excluded.set_up_at,set_up_by=excluded.set_up_by`,
			setup.accountID, setup.hostName, setup.bucketName, setup.bucketURL, setup.pageTitle, setup.accessKeyID, setup.secretAccessKey, a.Now().Unix(), a.settingsPrincipalName())
		return err
	})
}

// settingsSecretValues are the values of every secret key of the settings,
// the keys marked settings:"secret", for settingsPartFailureReason to
// remove from a reason.
func settingsSecretValues(value reflect.Value, secret bool) []string {
	var values []string
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			values = settingsSecretValues(value.Elem(), secret)
		}
	case reflect.Struct:
		for index := range value.NumField() {
			marked := strings.Contains(","+value.Type().Field(index).Tag.Get("settings")+",", ",secret,")
			values = append(values, settingsSecretValues(value.Field(index), marked)...)
		}
	case reflect.Slice:
		for index := range value.Len() {
			values = append(values, settingsSecretValues(value.Index(index), secret)...)
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			values = append(values, settingsSecretValues(value.MapIndex(key), secret)...)
		}
	case reflect.String:
		if secret && strings.TrimSpace(value.String()) != "" {
			values = append(values, value.String(), strings.TrimSpace(value.String()))
		}
	default:
		// Numbers, booleans and the other kinds hold no secret text.
	}
	return values
}
