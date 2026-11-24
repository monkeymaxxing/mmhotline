package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Source struct {
	ID, Name, Kind string
	Active         bool
	CreatedAt      time.Time
	Secrets        []SigningSecret
}
type SigningSecret struct {
	ID        string
	CreatedAt time.Time
}

// checkAlertSourceName returns the name without surrounding spaces, or why
// it is refused.
func checkAlertSourceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 {
		return "", fmt.Errorf("%w: alert source name must contain 1–100 characters", ErrInvalid)
	}
	return name, nil
}

// applyAlertSourceSettings makes the alert sources those of the settings
// file (CA3): each listed source is active, with its name, signing secrets,
// check-in times, and repeated_warnings; each other source is disabled, so its secrets stop
// working, and its alerts and check-ins stay. A signing secret listed before
// keeps its ID and creation time.
func (a *App) applyAlertSourceSettings(ctx context.Context, sources []SettingsAlertSource) error {
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, a.settingsPrincipal(), true); err != nil {
			return err
		}
		listed := map[string]bool{}
		for _, source := range sources {
			listed[source.AlertSourceID] = true
			name, err := checkAlertSourceName(source.Name)
			if err != nil {
				return err
			}
			warning, err := alertSourceWarningMinutes(source.WarnAfterNoCheckInForMinutes)
			if err != nil {
				return fmt.Errorf("%w: warn_after_no_check_in_for_minutes %s", ErrInvalid, err)
			}
			warnAfter, sosAfter := time.Duration(warning)*time.Minute, time.Duration(source.SOSAfterNoCheckInForMinutes)*time.Minute
			if err = checkAlertSourceCheckInTimes(warnAfter, sosAfter); err != nil {
				return err
			}
			repeatedWarnings, err := checkRepeatedWarnings(source.RepeatedWarnings)
			if err != nil {
				return fmt.Errorf("%w: repeated_warnings %s", ErrInvalid, err)
			}
			_, err = readSource(ctx, tx, source.AlertSourceID)
			switch {
			case errors.Is(err, ErrNotFound):
				var count int
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sources`).Scan(&count); err != nil {
					return err
				}
				if count >= maxAlertSources {
					return fmt.Errorf("%w: an installation can have at most %d alert sources, counting disabled ones", ErrRateLimited, maxAlertSources)
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO sources(id,name,kind,active,created_at,warn_after_no_check_in_for,sos_after_no_check_in_for,repeated_warnings) VALUES(?,?,'generic',1,?,?,?,?)`, source.AlertSourceID, name, now.Unix(), int64(warnAfter/time.Second), int64(sosAfter/time.Second), repeatedWarnings)
			case err == nil:
				_, err = tx.ExecContext(ctx, `UPDATE sources SET name=?,active=1,warn_after_no_check_in_for=?,sos_after_no_check_in_for=?,repeated_warnings=? WHERE id=?`, name, int64(warnAfter/time.Second), int64(sosAfter/time.Second), repeatedWarnings, source.AlertSourceID)
			}
			if err != nil {
				return err
			}
			if err = replaceSigningSecrets(ctx, tx, source.AlertSourceID, source.SigningSecrets, now); err != nil {
				return err
			}
		}
		active, err := tx.QueryContext(ctx, `SELECT id FROM sources WHERE active=1`)
		if err != nil {
			return err
		}
		var leftOut []string
		for active.Next() {
			var id string
			if err = active.Scan(&id); err != nil {
				active.Close()
				return err
			}
			if !listed[id] {
				leftOut = append(leftOut, id)
			}
		}
		if err = errors.Join(active.Err(), active.Close()); err != nil {
			return err
		}
		for _, id := range leftOut {
			if _, err = tx.ExecContext(ctx, `UPDATE sources SET active=0 WHERE id=?`, id); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM signing_secrets WHERE source_id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		a.alertSourcesChanged(ctx)
	}
	return err
}

// replaceSigningSecrets makes the alert source's signing secrets exactly
// the listed ones, each 64 hexadecimal characters.
func replaceSigningSecrets(ctx context.Context, tx *sql.Tx, sourceID string, listed []string, now time.Time) error {
	wanted := map[string]bool{}
	for _, secret := range listed {
		raw, err := hex.DecodeString(secret)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("%w: a signing secret must be 64 hexadecimal characters", ErrInvalid)
		}
		wanted[string(raw)] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,secret FROM signing_secrets WHERE source_id=?`, sourceID)
	if err != nil {
		return err
	}
	var removed []string
	for rows.Next() {
		var id string
		var secret []byte
		if err = rows.Scan(&id, &secret); err != nil {
			rows.Close()
			return err
		}
		if wanted[string(secret)] {
			delete(wanted, string(secret))
		} else {
			removed = append(removed, id)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range removed {
		if _, err = tx.ExecContext(ctx, `DELETE FROM signing_secrets WHERE id=?`, id); err != nil {
			return err
		}
	}
	// Save each listed secret that is not saved yet.
	for _, secret := range listed {
		raw, _ := hex.DecodeString(secret)
		if !wanted[string(raw)] {
			continue
		}
		delete(wanted, string(raw))
		if _, err = tx.ExecContext(ctx, `INSERT INTO signing_secrets(id,source_id,secret,created_at) VALUES(?,?,?,?)`, NewID(), sourceID, raw, now.Unix()); err != nil {
			return err
		}
	}
	return nil
}

func readSource(ctx context.Context, tx *sql.Tx, id string) (Source, error) {
	var s Source
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT id,name,kind,active,created_at FROM sources WHERE id=?`, id).Scan(&s.ID, &s.Name, &s.Kind, &s.Active, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	s.CreatedAt = time.Unix(at, 0).UTC()
	rows, err := tx.QueryContext(ctx, `SELECT id,created_at FROM signing_secrets WHERE source_id=? ORDER BY created_at,id`, id)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var k SigningSecret
		if err = rows.Scan(&k.ID, &at); err != nil {
			return s, err
		}
		k.CreatedAt = time.Unix(at, 0).UTC()
		s.Secrets = append(s.Secrets, k)
	}
	return s, rows.Err()
}

// SourceByID returns one alert source to any team member; signing secrets are
// never included.
func (a *App) SourceByID(ctx context.Context, p Principal, id string) (Source, error) {
	var s Source
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		s, err = readSource(ctx, tx, id)
		return err
	})
	return s, err
}

// Sources lists the team's alert sources. Every member may see them, so they
// can find check-ins and planned downtime; only administrators see the
// signing secret IDs and manage them.
func (a *App) Sources(ctx context.Context, p Principal) ([]Source, error) {
	var result []Source
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		admin := m.Role == RoleAdministrator
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sources ORDER BY created_at DESC,id`)
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
		for _, id := range ids {
			s, err := readSource(ctx, tx, id)
			if err != nil {
				return err
			}
			if !admin {
				s.Secrets = nil
			}
			result = append(result, s)
		}
		return nil
	})
	return result, err
}

// authenticateSource returns a canonical signature identity for id-less replay
// detection. Secrets are never returned by the listing or HTTP receipt paths.
//
// It also returns the signing secret that verified the request, which signs
// the receipt; it is set only when the signature is valid, even if the
// request is then refused.
func authenticateSource(ctx context.Context, tx *sql.Tx, sourceID, header string, body []byte, now time.Time) (string, []byte, error) {
	signature, ok := parseSenderSignature(header)
	if !ok {
		return "", nil, ErrUnauthorized
	}
	rows, err := tx.QueryContext(ctx, `SELECT k.secret FROM signing_secrets k JOIN sources s ON s.id=k.source_id WHERE s.id=? AND s.active=1 AND s.kind='generic'`, sourceID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var secrets [][]byte
	for rows.Next() {
		var secret []byte
		if err = rows.Scan(&secret); err != nil {
			return "", nil, err
		}
		secrets = append(secrets, secret)
	}
	if err = rows.Err(); err != nil {
		return "", nil, err
	}
	return signature.verify(body, secrets, now)
}

// senderSignature is a parsed Mmhotline-Signature header.
type senderSignature struct {
	timestamp string
	seconds   int64
	digest    []byte
}

func parseSenderSignature(header string) (senderSignature, bool) {
	parts := strings.Split(header, ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "timestamp_unix_seconds=") || !strings.HasPrefix(parts[1], "hmac_sha256_hex=") {
		return senderSignature{}, false
	}
	timestamp := strings.TrimPrefix(parts[0], "timestamp_unix_seconds=")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != timestamp {
		return senderSignature{}, false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(parts[1], "hmac_sha256_hex="))
	if err != nil || len(digest) != sha256.Size {
		return senderSignature{}, false
	}
	return senderSignature{timestamp: timestamp, seconds: seconds, digest: digest}, true
}

// verify checks the signature against an active alert source's signing
// secrets. It returns what authenticateSource returns. Without any signing
// secret there is no such alert source.
func (s senderSignature) verify(body []byte, secrets [][]byte, now time.Time) (string, []byte, error) {
	var matched []byte
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(s.timestamp + "."))
		mac.Write(body)
		if hmac.Equal(mac.Sum(nil), s.digest) {
			matched = secret
		}
	}
	if len(secrets) == 0 {
		return "", nil, ErrUnauthorized
	}
	if matched == nil {
		return "", nil, errWrongSignature
	}
	replayKey := "signature:" + s.timestamp + ":" + hex.EncodeToString(s.digest)
	// The clock is checked after the signature, so a correctly signed request
	// from a sender with a wrong clock can be told apart and reported. The
	// replay key comes with it, so a resent request that MMHotline already
	// received can be told apart too.
	if delta := now.Unix() - s.seconds; delta < -300 || delta > 300 {
		return replayKey, matched, errSenderClockOff
	}
	return replayKey, matched, nil
}
