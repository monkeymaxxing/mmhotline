package app

import (
	"context"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// verifyReceipt checks an answer's receipt the way a sender does:
// against its own request's route, the status code, and its request HMAC.
func verifyReceipt(t *testing.T, secretHex, route, requestBody string, requestAt time.Time, w *httptest.ResponseRecorder) bool {
	t.Helper()
	header := w.Header().Get("Mmhotline-Receipt-Signature")
	stamp, _, ok := strings.Cut(strings.TrimPrefix(header, "timestamp_unix_seconds="), ",")
	if header == "" || !ok {
		return false
	}
	at, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	key, err := hex.DecodeString(secretHex)
	if err != nil {
		t.Fatal(err)
	}
	r := newReceipt(key, route, alertSignature(secretHex, requestAt, []byte(requestBody)))
	return header == receiptSignature(r, w.Code, time.Unix(at, 0), w.Body.Bytes())
}

func TestIntakeReceiptsAreSignedWithTheRequestsSecret(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	now := a.Now()
	body := `{"sender_alert_id":"one","title":"Backup","message":""}`
	w := intakeHTTP(a, p, s, body, now)
	if w.Code != 200 || !verifyReceipt(t, s.Secret, "alert-intake", body, now, w) || !strings.Contains(w.Body.String(), `"mmhotline_alert_id"`) {
		t.Fatalf("2xx receipt: %d %q %q", w.Code, w.Header().Get("Mmhotline-Receipt-Signature"), w.Body.String())
	}
	if verifyReceipt(t, s.Secret, "check-in", body, now, w) || verifyReceipt(t, s.Secret, "alert-intake", `{"other":1}`, now, w) {
		t.Fatal("a receipt verified for another route or another request")
	}
	invalid := `{"title":"","message":""}`
	if w = intakeHTTP(a, p, s, invalid, now); w.Code != 400 || !verifyReceipt(t, s.Secret, "alert-intake", invalid, now, w) {
		t.Fatalf("a signed request's 400 must be signed: %d", w.Code)
	}
	second, err := addSigningSecret(context.Background(), a, p, s.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated := `{"sender_alert_id":"two","title":"Rotated","message":""}`
	if w = intakeHTTP(a, p, second, rotated, now); w.Code != 200 || !verifyReceipt(t, second.Secret, "alert-intake", rotated, now, w) || verifyReceipt(t, s.Secret, "alert-intake", rotated, now, w) {
		t.Fatal("the receipt was not signed with the secret that signed the request")
	}
	wrong := s
	wrong.Secret = strings.Repeat("ab", 32)
	if w = intakeHTTP(a, p, wrong, `{"title":"x","message":""}`, now); w.Code != 401 || w.Header().Get("Mmhotline-Receipt-Signature") != "" {
		t.Fatal("an unverified request got a signed answer")
	}
	if w = intakeHTTP(a, p, s, `{"title":"x","message":""}`, now.Add(-10*time.Minute)); w.Code != 401 || w.Header().Get("Mmhotline-Receipt-Signature") != "" {
		t.Fatal("a 401 was signed")
	}
	for n := range SourceAlertsPerMinute {
		intakeHTTP(a, p, s, `{"sender_alert_id":"r`+strings.Repeat("x", n)+`","title":"x","message":""}`, now)
	}
	limited := `{"sender_alert_id":"limited","title":"x","message":""}`
	if w = intakeHTTP(a, p, s, limited, now); w.Code != 429 || !verifyReceipt(t, s.Secret, "alert-intake", limited, now, w) {
		t.Fatalf("a signed request's 429 must be signed: %d", w.Code)
	}
}

// A hijacked backend domain replays an alert's signed request to the
// check-in route; the signed 400 it gets back is no receipt for the
// alert, and relabelling the status code breaks the signature too.
func TestReceiptsCannotBeReusedAcrossRoutesOrStatuses(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	now := a.Now()
	alert := `{"sender_alert_id":"sos","severity":"sos","title":"Disk full","message":""}`
	w := checkInHTTP(a, p, s, alert, now)
	if w.Code != 400 || !verifyReceipt(t, s.Secret, "check-in", alert, now, w) {
		t.Fatalf("check-in route answer: %d", w.Code)
	}
	if verifyReceipt(t, s.Secret, "alert-intake", alert, now, w) {
		t.Fatal("an check-in rejection verified as an alert intake receipt")
	}
	w.Code = 200
	if verifyReceipt(t, s.Secret, "check-in", alert, now, w) {
		t.Fatal("a relabelled status code still verified")
	}
}

func TestHappenedAtIsShownWhenAnAlertArrivesLate(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	received := a.Now()
	if w := intakeHTTP(a, p, s, `{"title":"x","message":"","happened_at":"yesterday"}`, received); w.Code != 400 || !strings.Contains(w.Body.String(), "happened_at must be an RFC 3339 time") {
		t.Fatalf("bad happened_at: %d %s", w.Code, w.Body.String())
	}
	late := alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"late","title":"Disk full","message":"","happened_at":"`+received.Add(-29*time.Minute).Format(time.RFC3339)+`"}`, received))
	onTime := alertReceipt(t, intakeHTTP(a, p, s, `{"sender_alert_id":"on-time","title":"Disk full","message":"","happened_at":"`+received.Add(-90*time.Second).Format(time.RFC3339)+`"}`, received))
	e, err := a.GetAlert(context.Background(), p, late)
	if err != nil {
		t.Fatal(err)
	}
	if gap := happenedGap(e.HappenedAt, e.ReceivedAt); gap != "Happened at 11:31 UTC. Received at 12:00 UTC (29 minutes later)." {
		t.Fatalf("gap: %q", gap)
	}
	if e, err = a.GetAlert(context.Background(), p, onTime); err != nil || happenedGap(e.HappenedAt, e.ReceivedAt) != "" {
		t.Fatalf("a small gap was shown: %v", err)
	}
	if got := happenedGap(received.Add(-26*time.Hour), received); got != "Happened at 2026-09-28 10:00 UTC. Received at 12:00 UTC (26 hours later)." {
		t.Fatalf("day-old gap: %q", got)
	}
}
