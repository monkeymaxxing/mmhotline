package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzAlertSourceRequestBody feeds one request body to both decoders of the
// alert intake listener: the alert intake's and the check-in's. The two
// formats share no field, so no body is both. A refusal is ErrInvalid with
// an empty result. An accepted alert keeps the documented bounds
// (docs/changing-mmhotline/design/ingestion.md#alert-intake) and reads back the same after
// encoding. An accepted check-in keeps its bounds, and its maintenance
// windows store and read back with each one ending at most 7 days after the
// check-in arrived. A correctly signed body that is refused but is a JSON
// object whose severity is "sos" still reads as an SOS, so MMHotline opens
// its own SOS about the refusal instead of dropping it.
func FuzzAlertSourceRequestBody(f *testing.F) {
	for _, seed := range []string{
		// The request format on the alert sources page and in the ingestion design.
		`{"sender_alert_id":"alert-1","sender_incident_key":"backup","severity":"sos","title":"Backup failed","message":"Disk full"}`,
		`{"sender_alert_id":"backup-2026-09-29","sender_incident_key":"nightly-backup","severity":"sos","title":"Nightly backup failed","message":"pg_dump exited 1: disk full on /backups.","happened_at":"2026-09-29T03:00:12Z"}`,
		`{"sender_alert_id":"monitoring-connection-test","severity":"info","title":"Monitoring connected","message":"The monitoring sender can authenticate to MMHotline."}`,
		`{"title":"x","message":"","happened_at":"yesterday"}`,
		`{"sender_alert_id":"refused-1","unknown_field":"x","severity":"sos","title":"t","message":"m"}`,
		`{"severity":"warn","title":"","message":""}`,
		// Check-ins from the check-in tests and the command guide.
		`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"}`,
		`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util","nodes_in_whole_node_maintenance":[{"node_name":"web-1","maintenance_until":"2026-09-29T12:30:00Z","reason":"moving the database","started_by":"matt"}]}`,
		`{"sent_at":"2026-09-29T12:00:00.123456+00:00","sent_by_node_name":"monitor01","nodes_in_whole_node_maintenance":[]}`,
		`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"} {}`,
	} {
		f.Add([]byte(seed))
	}
	received := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, body []byte) {
		alert, alertErr := decodeGeneric(body)
		checkIn, checkInErr := decodeAlertSourceCheckIn(body)
		if alertErr == nil && checkInErr == nil {
			t.Fatalf("one body decoded as an alert %+v and as a check-in %+v", alert, checkIn)
		}
		if alertErr != nil && (!errors.Is(alertErr, ErrInvalid) || alert != (GenericAlert{})) {
			t.Fatalf("refused alert: %+v, %v", alert, alertErr)
		}
		if checkInErr != nil && (!errors.Is(checkInErr, ErrInvalid) || checkIn.SentByNodeName != "" || !checkIn.SentAt.IsZero() || checkIn.NodesInWholeNodeMaintenance != nil) {
			t.Fatalf("refused check-in: %+v, %v", checkIn, checkInErr)
		}
		if alertErr == nil {
			checkAcceptedAlert(t, body, alert)
		} else {
			checkRefusedSOSStillReadsAsSOS(t, body)
		}
		if checkInErr == nil {
			checkAcceptedCheckIn(t, body, checkIn, received)
		}
	})
}

func checkAcceptedAlert(t *testing.T, body []byte, alert GenericAlert) {
	t.Helper()
	if len(body) > 64*1024 || !utf8.Valid(body) {
		t.Fatalf("accepted a body of %d bytes or not UTF-8", len(body))
	}
	title := utf8.RuneCountInString(alert.Title)
	if strings.TrimSpace(alert.Title) == "" || title > 256 || !utf8.ValidString(alert.Title) {
		t.Fatalf("accepted the title %q", alert.Title)
	}
	if len(alert.Message) > 16*1024 || !utf8.ValidString(alert.Message) {
		t.Fatalf("accepted a message of %d bytes", len(alert.Message))
	}
	for _, id := range []string{alert.SenderAlertID, alert.SenderIncidentKey} {
		if len(id) > 256 || !utf8.ValidString(id) {
			t.Fatalf("accepted the sender field %q", id)
		}
	}
	switch alert.Severity {
	case "", "info", "warn", "sos", "resolved-info", "resolved-warn", "resolved-sos":
	default:
		t.Fatalf("accepted the severity %q", alert.Severity)
	}
	if alert.HappenedAt != "" {
		if _, err := time.Parse(time.RFC3339, alert.HappenedAt); err != nil {
			t.Fatalf("accepted happened_at %q", alert.HappenedAt)
		}
	}
	// Present fields are nonempty, so encoding only those reads back the same.
	fields := map[string]string{"title": alert.Title, "message": alert.Message}
	for name, value := range map[string]string{"sender_alert_id": alert.SenderAlertID, "sender_incident_key": alert.SenderIncidentKey, "severity": alert.Severity, "happened_at": alert.HappenedAt} {
		if value != "" {
			fields[name] = value
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	again, err := decodeGeneric(encoded)
	if err != nil || again != alert {
		t.Fatalf("the alert %+v encoded as %s reads back as %+v, %v", alert, encoded, again, err)
	}
}

// checkRefusedSOSStillReadsAsSOS: a sender gives up on a signed 400, so a
// correctly signed SOS that MMHotline refuses must still open MMHotline's
// own SOS (docs/changing-mmhotline/design/ingestion.md#intake-limits-and-rejected-requests).
func checkRefusedSOSStillReadsAsSOS(t *testing.T, body []byte) {
	t.Helper()
	var object map[string]any
	if json.Unmarshal(body, &object) != nil || object["severity"] != "sos" {
		return
	}
	if _, severity := refusedAlertFields(body); severity != "sos" {
		t.Fatalf("a refused body whose severity is sos reads as severity %q: %s", severity, body)
	}
}

func checkAcceptedCheckIn(t *testing.T, body []byte, checkIn alertSourceCheckIn, received time.Time) {
	t.Helper()
	if len(body) > 64*1024 || !utf8.Valid(body) {
		t.Fatalf("accepted a check-in of %d bytes or not UTF-8", len(body))
	}
	if checkIn.SentAt.IsZero() || checkIn.SentByNodeName == "" || len(checkIn.SentByNodeName) > 256 || len(checkIn.NodesInWholeNodeMaintenance) > 1000 {
		t.Fatalf("accepted the check-in %+v", checkIn)
	}
	for _, node := range checkIn.NodesInWholeNodeMaintenance {
		if node.NodeName == "" || len(node.NodeName) > 256 || node.MaintenanceUntil.IsZero() || len(node.Reason) > 256 || len(node.StartedBy) > 256 {
			t.Fatalf("accepted the node in maintenance %+v", node)
		}
	}
	stored, err := maintenanceWindowsJSON(checkIn.NodesInWholeNodeMaintenance, received)
	if err != nil {
		t.Fatalf("an accepted check-in's maintenance windows cannot be stored: %v", err)
	}
	var windows []NodeInMaintenance
	if err := json.Unmarshal([]byte(stored), &windows); err != nil || len(windows) != len(checkIn.NodesInWholeNodeMaintenance) {
		t.Fatalf("stored maintenance windows %s read back as %+v, %v", stored, windows, err)
	}
	for index, window := range windows {
		if window.NodeName != checkIn.NodesInWholeNodeMaintenance[index].NodeName || window.MaintenanceUntil.After(received.Add(7*24*time.Hour)) {
			t.Fatalf("stored maintenance window %+v for %+v", window, checkIn.NodesInWholeNodeMaintenance[index])
		}
	}
}

// FuzzAlertSourceSignature checks the Mmhotline-Signature header that both
// sender routes verify. It signs a fuzzed body at a fuzzed time with a known
// signing secret, beside a second secret as during rotation. The signature
// verifies and names the secret that signed it; a sender clock more than
// 5 minutes off is told apart from a wrong signature; a body changed after
// signing is refused as a wrong signature; no secret means no such alert
// source. Any fuzzed header that parses is the one canonical form, and one
// that verifies is a correct HMAC-SHA256 of its timestamp and the body.
func FuzzAlertSourceSignature(f *testing.F) {
	secret := bytes.Repeat([]byte{0x5a}, 32)
	otherSecret := bytes.Repeat([]byte{0xa5}, 32)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	sign := func(seconds int64, body []byte) string {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(strconv.FormatInt(seconds, 10) + "."))
		mac.Write(body)
		return "timestamp_unix_seconds=" + strconv.FormatInt(seconds, 10) + ",hmac_sha256_hex=" + hex.EncodeToString(mac.Sum(nil))
	}
	alert := []byte(`{"sender_alert_id":"alert-1","sender_incident_key":"backup","severity":"sos","title":"Backup failed","message":"Disk full"}`)
	checkIn := []byte(`{"sent_at":"2026-09-29T12:00:00Z","sent_by_node_name":"util"}`)
	f.Add(alert, append(bytes.Clone(alert), ' '), int64(0), sign(now.Unix(), alert))
	f.Add(checkIn, alert, int64(-301), sign(now.Unix()-301, checkIn))
	f.Add(checkIn, checkIn, int64(300), strings.ToUpper(sign(now.Unix(), checkIn)))
	f.Add([]byte{}, []byte("x"), int64(-1), "timestamp_unix_seconds=+1759147200,hmac_sha256_hex=00")
	f.Fuzz(func(t *testing.T, body, otherBody []byte, clockOffset int64, header string) {
		// Wrapping addition, as a sender's timestamp of any int64 value arrives.
		seconds := now.Unix() + clockOffset
		signed := sign(seconds, body)
		parsed, ok := parseSenderSignature(signed)
		if !ok {
			t.Fatalf("a correctly formed header does not parse: %s", signed)
		}
		replayKey, matched, err := parsed.verify(body, [][]byte{otherSecret, secret}, now)
		withinFiveMinutes := clockOffset >= -300 && clockOffset <= 300
		switch {
		case !bytes.Equal(matched, secret):
			t.Fatalf("the signing secret was not named: %x, %v", matched, err)
		case withinFiveMinutes && (err != nil || replayKey == ""):
			t.Fatalf("a correct signature %d seconds off was refused: %v", clockOffset, err)
		case !withinFiveMinutes && !errors.Is(err, errSenderClockOff):
			t.Fatalf("a correct signature %d seconds off: %v", clockOffset, err)
		}
		if _, _, err := parsed.verify(body, nil, now); !errors.Is(err, ErrUnauthorized) || errors.Is(err, errWrongSignature) {
			t.Fatalf("no signing secret: %v", err)
		}
		if !bytes.Equal(otherBody, body) {
			if _, matched, err := parsed.verify(otherBody, [][]byte{otherSecret, secret}, now); !errors.Is(err, errWrongSignature) || matched != nil {
				t.Fatalf("a body changed after signing: %x, %v", matched, err)
			}
		}

		fuzzed, ok := parseSenderSignature(header)
		if !ok {
			return
		}
		// Only the hexadecimal digits may be uppercase.
		stamp, digest, _ := strings.Cut(header, ",")
		if stamp != "timestamp_unix_seconds="+strconv.FormatInt(fuzzed.seconds, 10) || strings.ToLower(digest) != "hmac_sha256_hex="+hex.EncodeToString(fuzzed.digest) {
			t.Fatalf("the header %q parses as %d and %x", header, fuzzed.seconds, fuzzed.digest)
		}
		if _, _, err := fuzzed.verify(body, [][]byte{otherSecret, secret}, now); err == nil || errors.Is(err, errSenderClockOff) {
			if expected := sign(fuzzed.seconds, body); strings.ToLower(header) != expected {
				t.Fatalf("the header %q verified for a body it does not sign", header)
			}
		}
	})
}
