package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func normalizeSignInEmailAddress(signInEmailAddress string) (string, error) {
	validUTF8 := utf8.ValidString(signInEmailAddress)
	signInEmailAddress = strings.ToLower(strings.TrimSpace(signInEmailAddress))
	length := utf8.RuneCountInString(signInEmailAddress)
	if !validUTF8 || length < 3 || length > 254 || strings.Count(signInEmailAddress, "@") != 1 || strings.HasPrefix(signInEmailAddress, "@") || strings.HasSuffix(signInEmailAddress, "@") || strings.ContainsFunc(signInEmailAddress, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", fmt.Errorf("%w: sign-in email address must have 3–254 characters, exactly one @ with text on both sides, and no spaces or control characters", ErrInvalid)
	}
	return signInEmailAddress, nil
}
func validatePersonName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || name == "" || utf8.RuneCountInString(name) > 100 {
		return "", fmt.Errorf("%w: person name must contain 1–100 characters", ErrInvalid)
	}
	return name, nil
}
func validatePersonRole(role string) error {
	if role != RoleAdministrator && role != RoleMember {
		return fmt.Errorf("%w: person role must be administrator or member", ErrInvalid)
	}
	return nil
}

func (a *App) TeamMembers(ctx context.Context, p Principal) ([]Member, error) {
	return a.readMembers(ctx, p, false)
}
func (a *App) membersForHistory(ctx context.Context, p Principal) ([]Member, error) {
	return a.readMembers(ctx, p, true)
}
func (a *App) readMembers(ctx context.Context, p Principal, includeRemoved bool) ([]Member, error) {
	members := []Member{}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		query := `SELECT id,sign_in_email_address,name,role FROM members`
		if !includeRemoved {
			query += ` WHERE removed_at IS NULL`
		}
		rows, err := tx.QueryContext(ctx, query+` ORDER BY name,id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Member
			if err := rows.Scan(&m.ID, &m.SignInEmailAddress, &m.Name, &m.Role); err != nil {
				return err
			}
			members = append(members, m)
		}
		return rows.Err()
	})
	return members, err
}
func personBySignInEmailAddress(ctx context.Context, tx *sql.Tx, signInEmailAddress string) (Member, error) {
	var m Member
	err := tx.QueryRowContext(ctx, `SELECT id,sign_in_email_address,name,role FROM members WHERE sign_in_email_address=? AND removed_at IS NULL`, signInEmailAddress).Scan(&m.ID, &m.SignInEmailAddress, &m.Name, &m.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return m, fmt.Errorf("%w: no member has sign-in email address %s", ErrNotFound, signInEmailAddress)
	}
	return m, err
}

// AddPerson adds one person; the settings file's people part adds each new
// person with insertPersonTx, in the transaction of the whole list.
func (a *App) AddPerson(ctx context.Context, p Principal, signInEmailAddress, name, role string) (Member, error) {
	var m Member
	signInEmailAddress, err := normalizeSignInEmailAddress(signInEmailAddress)
	if err != nil {
		return m, err
	}
	name, err = validatePersonName(name)
	if err != nil {
		return m, err
	}
	if err = validatePersonRole(role); err != nil {
		return m, err
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		m, err = insertPersonTx(ctx, tx, signInEmailAddress, name, role)
		return err
	})
	return m, err
}

// insertPersonTx adds a person whose checked sign-in email address no
// member has.
func insertPersonTx(ctx context.Context, tx *sql.Tx, signInEmailAddress, name, role string) (Member, error) {
	existing, err := personBySignInEmailAddress(ctx, tx, signInEmailAddress)
	if err == nil {
		return Member{}, fmt.Errorf("%w: %s (%s) is already a member", ErrConflict, existing.Name, existing.SignInEmailAddress)
	}
	if !errors.Is(err, ErrNotFound) {
		return Member{}, err
	}
	m := Member{ID: NewID(), SignInEmailAddress: signInEmailAddress, Name: name, Role: role}
	_, err = tx.ExecContext(ctx, `INSERT INTO members(id,sign_in_email_address,name,role) VALUES(?,?,?,?)`, m.ID, m.SignInEmailAddress, m.Name, m.Role)
	return m, err
}
func protectLastAdministrator(ctx context.Context, tx *sql.Tx, m Member) error {
	if m.Role != RoleAdministrator {
		return nil
	}
	var administrators int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM members WHERE role='administrator' AND removed_at IS NULL`).Scan(&administrators); err != nil {
		return err
	}
	if administrators == 1 {
		return fmt.Errorf("%w: add another administrator before removing the last administrator or changing the last administrator to a member", ErrConflict)
	}
	return nil
}
func (a *App) ChangePerson(ctx context.Context, p Principal, signInEmailAddress string, name, role *string) (Member, error) {
	var m Member
	signInEmailAddress, err := normalizeSignInEmailAddress(signInEmailAddress)
	if err != nil {
		return m, err
	}
	if name == nil && role == nil {
		return m, fmt.Errorf("%w: provide a name or role to change", ErrInvalid)
	}
	var normalizedName string
	if name != nil {
		normalizedName, err = validatePersonName(*name)
		if err != nil {
			return m, err
		}
	}
	if role != nil {
		if err = validatePersonRole(*role); err != nil {
			return m, err
		}
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var err error
		m, err = personBySignInEmailAddress(ctx, tx, signInEmailAddress)
		if err != nil {
			return err
		}
		if role != nil && *role != RoleAdministrator {
			if err = protectLastAdministrator(ctx, tx, m); err != nil {
				return err
			}
		}
		if name != nil {
			m.Name = normalizedName
		}
		if role != nil {
			m.Role = *role
		}
		_, err = tx.ExecContext(ctx, `UPDATE members SET name=?,role=? WHERE id=?`, m.Name, m.Role, m.ID)
		return err
	})
	return m, err
}
func (a *App) RemovePerson(ctx context.Context, p Principal, signInEmailAddress string) (Member, error) {
	var m Member
	signInEmailAddress, err := normalizeSignInEmailAddress(signInEmailAddress)
	if err != nil {
		return m, err
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var err error
		m, err = personBySignInEmailAddress(ctx, tx, signInEmailAddress)
		if err != nil {
			return err
		}
		if err = protectLastAdministrator(ctx, tx, m); err != nil {
			return err
		}
		schedules, err := ReadSchedules(ctx, tx)
		if err != nil {
			return err
		}
		return removePersonTx(ctx, tx, m, schedules.Configs, a.Now())
	})
	return m, err
}

// removePersonTx deactivates a person: it ends their contact methods, the
// work waiting for them, and their sign-ins, and keeps their history. It
// refuses while a schedule of schedules names them, while they own a
// claimed incident, or while they have taken on-call.
func removePersonTx(ctx context.Context, tx *sql.Tx, m Member, schedules []ScheduleConfig, now time.Time) error {
	for _, schedule := range schedules {
		if containsMember(schedule.Assignments, m.ID) || containsMember(schedule.RotationMemberIDs, m.ID) || schedule.BackupMemberID == m.ID {
			return fmt.Errorf("%w: %s is named by the saved %s schedule; change the schedule before removing the person", ErrConflict, m.Name, scheduleTypeLabel(schedule.Kind))
		}
	}
	// A claimed incident reminds only its owner, so a removed owner would
	// leave it waiting with nobody told.
	var claimedIncidents, holdsOnCallClaim int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM incidents WHERE status='claimed' AND owner_member_id=?`, m.ID).Scan(&claimedIncidents); err != nil {
		return err
	}
	if claimedIncidents > 0 {
		return fmt.Errorf("%w: %s owns %s. Resolve the claimed incidents or have someone take over the claimed incidents before removing the person", ErrConflict, m.Name, plural(claimedIncidents, "claimed incident"))
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM on_call_claim WHERE id=1 AND member_id=? AND expired=0 AND until_at>?`, m.ID, now.Unix()).Scan(&holdsOnCallClaim); err != nil {
		return err
	}
	if holdsOnCallClaim > 0 {
		return fmt.Errorf("%w: %s has taken on-call; give on-call back to the schedule before removing this person", ErrConflict, m.Name)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM contact_methods WHERE member_id=? AND deleted=0`, m.ID)
	if err != nil {
		return err
	}
	var contactMethodIDs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		contactMethodIDs = append(contactMethodIDs, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range contactMethodIDs {
		if err = cancelContactMethodWork(ctx, tx, id, "person removed"); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE contact_methods SET deleted=1,revision=revision+1 WHERE member_id=? AND deleted=0`, m.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM browser_sign_ins WHERE member_id=?`, m.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE members SET removed_at=? WHERE id=?`, now.Unix(), m.ID)
	return err
}
