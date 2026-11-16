package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"mmhotline/internal/actionkinds"
)

// The settings file sets alert sources, rules for running the agent's playbooks, and listed hosts as
// a whole. These fixtures set one of them at a time, as the tests of other
// behavior need; each keeps the rule of the part that sets it.

// CreatedSource is an alert source a fixture created, with its signing
// secret in hexadecimal.
type CreatedSource struct {
	Source                  Source
	SigningSecretID, Secret string
}

func newSigningSecret() (string, []byte) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw), raw
}

// createAlertSource adds an active alert source with one signing secret and
// the default check-in times.
func createAlertSource(ctx context.Context, a *App, p Principal, name string) (CreatedSource, error) {
	var result CreatedSource
	name, err := checkAlertSourceName(name)
	if err != nil {
		return result, err
	}
	secret, raw := newSigningSecret()
	now := a.Now().UTC()
	result = CreatedSource{Source: Source{ID: NewID(), Name: name, Kind: "generic", Active: true, CreatedAt: now}, SigningSecretID: NewID(), Secret: secret}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sources`).Scan(&count); err != nil {
			return err
		}
		if count >= maxAlertSources {
			return fmt.Errorf("%w: an installation can have at most %d alert sources", ErrRateLimited, maxAlertSources)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sources(id,name,kind,active,created_at) VALUES(?,?,'generic',1,?)`, result.Source.ID, name, now.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO signing_secrets(id,source_id,secret,created_at) VALUES(?,?,?,?)`, result.SigningSecretID, result.Source.ID, raw, now.Unix())
		return err
	})
	if err != nil {
		return CreatedSource{}, err
	}
	a.alertSourcesChanged(ctx)
	return result, nil
}

// addSigningSecret adds a second signing secret to an active alert source.
func addSigningSecret(ctx context.Context, a *App, p Principal, sourceID string) (CreatedSource, error) {
	var result CreatedSource
	secret, raw := newSigningSecret()
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		source, err := readSource(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if !source.Active {
			return ErrConflict
		}
		if len(source.Secrets) >= 2 {
			return fmt.Errorf("%w: revoke an old signing secret before adding another", ErrConflict)
		}
		result = CreatedSource{Source: source, SigningSecretID: NewID(), Secret: secret}
		_, err = tx.ExecContext(ctx, `INSERT INTO signing_secrets(id,source_id,secret,created_at) VALUES(?,?,?,?)`, result.SigningSecretID, sourceID, raw, now.Unix())
		return err
	})
	if err != nil {
		return CreatedSource{}, err
	}
	a.alertSourcesChanged(ctx)
	return result, nil
}

// revokeSigningSecret removes one of an alert source's two signing secrets.
func revokeSigningSecret(ctx context.Context, a *App, p Principal, sourceID, signingSecretID string) error {
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		source, err := readSource(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if len(source.Secrets) < 2 {
			return fmt.Errorf("%w: keep one signing secret or disable the alert source", ErrConflict)
		}
		r, err := tx.ExecContext(ctx, `DELETE FROM signing_secrets WHERE source_id=? AND id=?`, sourceID, signingSecretID)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err == nil && n == 0 {
			return ErrNotFound
		}
		return err
	})
	if err == nil {
		a.alertSourcesChanged(ctx)
	}
	return err
}

// disableAlertSource disables an alert source and removes its signing
// secrets, as the settings file does with one it leaves out.
func disableAlertSource(ctx context.Context, a *App, p Principal, sourceID string) error {
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		if _, err := readSource(ctx, tx, sourceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sources SET active=0 WHERE id=?`, sourceID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM signing_secrets WHERE source_id=?`, sourceID)
		return err
	})
	if err == nil {
		a.alertSourcesChanged(ctx)
	}
	return err
}

// setAlertSourceCheckInTimes sets when missing check-ins send a Warning (0
// means off) and an SOS.
func setAlertSourceCheckInTimes(ctx context.Context, a *App, p Principal, sourceID string, warning, sos time.Duration) error {
	if err := checkAlertSourceCheckInTimes(warning, sos); err != nil {
		return err
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE sources SET warn_after_no_check_in_for=?,sos_after_no_check_in_for=? WHERE id=?`, int64(warning/time.Second), int64(sos/time.Second), sourceID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err == nil {
				err = ErrNotFound
			}
			return err
		}
		return nil
	})
}

// applyAgentPlaybookRulesPreset sets every kind of action of the inventory as
// the preset lists it.
func applyAgentPlaybookRulesPreset(ctx context.Context, a *App, p Principal, inventory, presetID string) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		person, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		if err := checkAgentPlaybookRulesInventory(inventory); err != nil {
			return err
		}
		preset, found := findAgentPlaybookRulesPreset(presetID)
		if !found {
			return fmt.Errorf("%w: unknown preset %s", ErrInvalid, presetID)
		}
		for _, kind := range actionkinds.Kinds() {
			if err := saveAgentPlaybookRule(ctx, tx, inventory, kind, preset.settings[kind], a.Now(), person.Name); err != nil {
				return err
			}
		}
		return nil
	})
}

// setAgentPlaybookRule sets one kind of action of the inventory. Get permission,
// then act needs one approval unless two are asked for.
func setAgentPlaybookRule(ctx context.Context, a *App, p Principal, inventory, actionKind string, setting AgentPlaybookRule) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		person, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		if err := checkAgentPlaybookRulesInventory(inventory); err != nil {
			return err
		}
		kind := actionkinds.Kind(actionKind)
		if !slices.Contains(actionkinds.Kinds(), kind) {
			return fmt.Errorf("%w: unknown kind of action %s", ErrInvalid, actionKind)
		}
		if setting.Rule == getPermissionThenAct && setting.ApprovalsNeeded == 0 {
			setting.ApprovalsNeeded = 1
		}
		if err := checkAgentPlaybookRule(setting); err != nil {
			return err
		}
		return saveAgentPlaybookRule(ctx, tx, inventory, kind, setting, a.Now(), person.Name)
	})
}

// addTriageAgentListedHost adds one listed host to those already listed.
func addTriageAgentListedHost(ctx context.Context, a *App, p Principal, host string, port int) error {
	var hosts []SettingsListedHost
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT host,port FROM triage_agent_listed_hosts ORDER BY host,port`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var listed SettingsListedHost
			if err := rows.Scan(&listed.Host, &listed.Port); err != nil {
				return err
			}
			hosts = append(hosts, listed)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	return a.SetTriageAgentListedHosts(ctx, p, append(hosts, SettingsListedHost{Host: host, Port: port}))
}

// Model and reasoning effort of the agents' setups that the fixtures below
// start: the settings file names them, and the sandbox passes them on.
const (
	testAgentModel           = "test-model"
	testAgentReasoningEffort = "high"
)

// StartTriageAgentSetupTest starts the triage agent's setup test with a
// coding agent and credential, as the settings file's triage agent part
// does, with the test model and reasoning effort, started by p. The test
// drives the run, which saves the setup when it passes.
func (a *App) StartTriageAgentSetupTest(ctx context.Context, p Principal, codingAgent, credentialType, credential string) (string, error) {
	return a.startSetupTestFixture(ctx, triageAgentRole, p, codingAgent, credentialType, credential)
}

// StartReviewAgentSetupTest is StartTriageAgentSetupTest for the review
// agent.
func (a *App) StartReviewAgentSetupTest(ctx context.Context, p Principal, codingAgent, credentialType, credential string) (string, error) {
	return a.startSetupTestFixture(ctx, reviewAgentRole, p, codingAgent, credentialType, credential)
}

func (a *App) startSetupTestFixture(ctx context.Context, role agentRole, p Principal, codingAgent, credentialType, credential string) (string, error) {
	var person Member
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error { var err error; person, err = RequireMember(ctx, tx, p, true); return err }); err != nil {
		return "", err
	}
	run, err := a.startCodingAgentSetupTest(ctx, role, SettingsCodingAgent{CodingAgent: codingAgent, CredentialType: credentialType, Credential: credential, Model: testAgentModel, ReasoningEffort: testAgentReasoningEffort}, person.Name)
	if err != nil {
		return "", err
	}
	return run.id, nil
}
