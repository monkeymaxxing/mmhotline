package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// An alert source's repeated_warnings setting says what intake does with a
// warn that repeats an earlier one
// (docs/changing-mmhotline/design/ingestion.md, "Repeated warnings").
//
// keep_each is the default: for most senders a changed message under one
// key ("retry underway", then "restore required") is new news, and
// MMWatchdog's fleet hub must keep every warning, because its warning that
// a new server has a finding has the same key and first line as the first.
//
// fold_by_sender_incident_key_and_first_message_line is for a source such
// as MMWatchdog's nodes sending directly while the hub is down: thousands of
// servers send one finding, and each repeat would otherwise use up the
// intake limits and refuse unrelated warnings.
const (
	repeatedWarningsKeepEach = "keep_each"
	repeatedWarningsFold     = "fold_by_sender_incident_key_and_first_message_line"
)

// repeatedWarningsColumn is sources.repeated_warnings, in the table and in
// its upgrade step.
const repeatedWarningsColumn = `repeated_warnings TEXT NOT NULL DEFAULT 'keep_each' CHECK(repeated_warnings IN ('keep_each','fold_by_sender_incident_key_and_first_message_line'))`

// The alerts table's record of the repeats folded into a warn.
const (
	alertRepeatCountColumn     = `repeat_count INTEGER NOT NULL DEFAULT 0`
	alertLastRepeatTitleColumn = `last_repeat_title TEXT NOT NULL DEFAULT ''`
)

// foldedWarningRepeatsSchema keeps each folded repeat's retry identity: its
// replay key (the sender_alert_id, or else the signature) and a SHA-256 of
// what it says, with the warn it was folded into. A sender's retry after a
// lost receipt is then answered as a duplicate, and a different alert that
// reuses the sender_alert_id is refused, as for an alert with its own row.
const foldedWarningRepeatsSchema = `
CREATE TABLE folded_warning_repeats(source_id TEXT NOT NULL,replay_key TEXT NOT NULL,alert_id TEXT NOT NULL,content_sha256 BLOB NOT NULL,received_at INTEGER NOT NULL,PRIMARY KEY(source_id,replay_key));
CREATE INDEX folded_warning_repeats_received ON folded_warning_repeats(source_id,received_at);
`

const (
	// repeatedWarningFoldingWindow: a warn folds into an earlier one
	// received within the last hour. A finding that lasts longer is
	// delivered again each hour, so people still hear it goes on, and the
	// hourly installation limit sees at most one alert an hour for it.
	repeatedWarningFoldingWindow = time.Hour
	// foldedRepeatRetryIdentityKeptFor: a sender retries until its retry
	// deadline, minutes or hours after the first attempt, so a day covers
	// every retry after a lost receipt with room to spare.
	foldedRepeatRetryIdentityKeptFor = 24 * time.Hour
	// maxFoldedRepeatRetryIdentitiesPerAlertSource bounds the table: 5,000
	// servers each sending 4 findings in a day, at about 500 bytes a row,
	// about 10 MB per folding source. Past it a repeat is still folded and
	// counted, without its retry identity: a retry of it is then folded
	// again and counted twice, which changes only the repeat count.
	maxFoldedRepeatRetryIdentitiesPerAlertSource = 20000
)

// addRepeatedWarnings is the upgrade step that adds the repeated_warnings
// setting, the alerts' repeat record, and the folded repeats' retry
// identities.
func addRepeatedWarnings(tx *sql.Tx) error {
	for _, statement := range []string{
		`ALTER TABLE sources ADD COLUMN ` + repeatedWarningsColumn,
		`ALTER TABLE alerts ADD COLUMN ` + alertRepeatCountColumn,
		`ALTER TABLE alerts ADD COLUMN ` + alertLastRepeatTitleColumn,
		foldedWarningRepeatsSchema,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// checkRepeatedWarnings reads an alert source's repeated_warnings setting;
// left out is keep_each.
func checkRepeatedWarnings(value string) (string, error) {
	switch value {
	case "":
		return repeatedWarningsKeepEach, nil
	case repeatedWarningsKeepEach, repeatedWarningsFold:
		return value, nil
	}
	return "", errors.New("must be keep_each or fold_by_sender_incident_key_and_first_message_line")
}

// firstMessageLine is the message up to its first line break.
func firstMessageLine(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	return strings.TrimSuffix(line, "\r")
}

// alertContentSHA256 covers what sameAlertContent compares.
func alertContentSHA256(in GenericAlert) []byte {
	severity := in.Severity
	if severity == "" {
		severity = "warn"
	}
	sum := sha256.New()
	for _, field := range []string{severity, in.Title, in.Message, in.SenderIncidentKey} {
		sum.Write([]byte(field))
		sum.Write([]byte{0})
	}
	return sum.Sum(nil)
}

// retryOfFoldedRepeat finds the request with replayKey among the alert
// source's folded repeats of the last 24 hours, and returns the warn it was
// folded into. found is false when it is not a folded repeat's retry. A
// different alert with the same replay key is refused.
func retryOfFoldedRepeat(ctx context.Context, tx *sql.Tx, sourceID, replayKey string, in GenericAlert, now time.Time) (earlier Alert, found bool, err error) {
	var alertID string
	var content []byte
	err = tx.QueryRowContext(ctx, `SELECT alert_id,content_sha256 FROM folded_warning_repeats WHERE source_id=? AND replay_key=? AND received_at>?`, sourceID, replayKey, now.Add(-foldedRepeatRetryIdentityKeptFor).Unix()).Scan(&alertID, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, false, nil
	}
	if err != nil {
		return Alert{}, false, err
	}
	if !bytes.Equal(content, alertContentSHA256(in)) {
		return Alert{}, false, errSenderAlertIDReused
	}
	earlier, err = readAlertBy(ctx, tx, "id=?", alertID)
	return earlier, err == nil, err
}

// foldRepeatedWarning records the alert as a repeat of an earlier warn when
// its alert source folds repeated warnings: a warn with a sender incident
// key, whose alert source, key, and first message line match an unresolved
// warn received within the folding window. "Unresolved": no resolved-warn
// of that source and key arrived after it, as resolvesOnlyWarnsNeverSent
// matches. The earlier warn's repeat count goes up and keeps the repeat's
// title; the repeat adds no alert row and is delivered to nobody. It
// returns the earlier warn, or found false when the alert is not a repeat.
func foldRepeatedWarning(ctx context.Context, tx *sql.Tx, sourceID, replayKey string, in GenericAlert, now time.Time) (earlier Alert, found bool, err error) {
	if in.SenderIncidentKey == "" || in.Severity != "" && in.Severity != "warn" {
		return Alert{}, false, nil
	}
	var setting string
	if err = tx.QueryRowContext(ctx, `SELECT repeated_warnings FROM sources WHERE id=?`, sourceID).Scan(&setting); err != nil || setting != repeatedWarningsFold {
		return Alert{}, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+alertColumns+` FROM alerts e WHERE source_id=? AND sender_incident_key=? AND severity='warn' AND received_at>?
 AND NOT EXISTS(SELECT 1 FROM alerts r WHERE r.source_id=e.source_id AND r.sender_incident_key=e.sender_incident_key AND r.severity='resolved-warn' AND r.sequence>e.sequence)
 ORDER BY sequence DESC`, sourceID, in.SenderIncidentKey, now.Add(-repeatedWarningFoldingWindow).Unix())
	if err != nil {
		return Alert{}, false, err
	}
	line := firstMessageLine(in.Message)
	for rows.Next() {
		candidate, scanErr := scanAlert(rows)
		if scanErr != nil {
			rows.Close()
			return Alert{}, false, scanErr
		}
		if firstMessageLine(candidate.Message) == line {
			earlier, found = candidate, true
			break
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || !found {
		return Alert{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE alerts SET repeat_count=repeat_count+1,last_repeat_title=? WHERE id=?`, in.Title, earlier.ID); err != nil {
		return Alert{}, false, err
	}
	earlier.RepeatCount++
	earlier.LastRepeatTitle = in.Title
	return earlier, true, recordFoldedRepeatRetryIdentity(ctx, tx, sourceID, replayKey, earlier.ID, in, now)
}

// recordFoldedRepeatRetryIdentity keeps the folded repeat's replay key and
// content for 24 hours, up to the per-source bound, and drops the source's
// expired ones.
func recordFoldedRepeatRetryIdentity(ctx context.Context, tx *sql.Tx, sourceID, replayKey, alertID string, in GenericAlert, now time.Time) error {
	expired := now.Add(-foldedRepeatRetryIdentityKeptFor).Unix()
	if _, err := tx.ExecContext(ctx, `DELETE FROM folded_warning_repeats WHERE source_id=? AND received_at<=?`, sourceID, expired); err != nil {
		return err
	}
	var kept int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM folded_warning_repeats WHERE source_id=?`, sourceID).Scan(&kept); err != nil || kept >= maxFoldedRepeatRetryIdentitiesPerAlertSource {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO folded_warning_repeats(source_id,replay_key,alert_id,content_sha256,received_at) VALUES(?,?,?,?,?)`, sourceID, replayKey, alertID, alertContentSHA256(in), now.Unix())
	return err
}

// repeatsSentence tells a person about the repeats folded into a warn, for
// its page and the alerts command; it is empty when there were none.
func repeatsSentence(count int64, lastTitle string) string {
	if count == 0 {
		return ""
	}
	return "Repeated " + countText(int(count), "more time", "more times") + " within the hour, and not delivered again. The latest repeat's title: " + lastTitle
}
