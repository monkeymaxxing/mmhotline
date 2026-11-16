package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// receipt is what signs one answer: the signing secret that verified the
// request, the route the request came to ("alert-intake" or
// "check-in"), and the request's own hmac_sha256_hex. Binding all three,
// and the status code, means a signed answer can only ever confirm the very
// request it answers: a hijacked backend domain cannot replay a request to
// another route, or reuse an answer, to hand the sender a signed rejection.
type receipt struct {
	secret             []byte
	route, requestHMAC string
}

// newReceipt returns the receipt for a verified request, or nil.
func newReceipt(secret []byte, route, signatureHeader string) *receipt {
	if secret == nil {
		return nil
	}
	_, requestHMAC, _ := strings.Cut(signatureHeader, ",hmac_sha256_hex=")
	return &receipt{secret: secret, route: route, requestHMAC: strings.ToLower(requestHMAC)}
}

// receiptSignature is HMAC-SHA256 with the signing secret over
// "mmhotline-receipt." + route + "." + status + "." + request_hmac_hex + "." +
// timestamp + "." + the exact response body.
func receiptSignature(r *receipt, status int, at time.Time, body []byte) string {
	timestamp := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, r.secret)
	fmt.Fprintf(mac, "mmhotline-receipt.%s.%d.%s.%s.", r.route, status, r.requestHMAC, timestamp)
	mac.Write(body)
	return "timestamp_unix_seconds=" + timestamp + ",hmac_sha256_hex=" + hex.EncodeToString(mac.Sum(nil))
}

// writeSenderAnswer writes an answer to alert intake or a check-in. It
// carries Mmhotline-Receipt-Signature when the request's signature was verified
// (receiptKey is set) and the answer is 2xx, 400, or 429, so the sender
// can tell a receipt, a permanent rejection, or a rate
// limit from MMHotline apart from an answer by someone else. 401, 413, and 503 are
// never signed.
func writeSenderAnswer(w http.ResponseWriter, status int, contentType string, body []byte, r *receipt, at time.Time) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	signed := (status >= 200 && status < 300) || status == http.StatusBadRequest || status == http.StatusTooManyRequests
	if r != nil && signed {
		w.Header().Set("Mmhotline-Receipt-Signature", receiptSignature(r, status, at, body))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeSenderJSON(w http.ResponseWriter, status int, v any, r *receipt, at time.Time) {
	body, err := json.Marshal(v)
	if err != nil {
		writeSenderAnswer(w, http.StatusServiceUnavailable, "text/plain; charset=utf-8", []byte("answer could not be encoded; retry later\n"), nil, at)
		return
	}
	writeSenderAnswer(w, status, "application/json", append(body, '\n'), r, at)
}

func writeSenderText(w http.ResponseWriter, status int, text string, r *receipt, at time.Time) {
	writeSenderAnswer(w, status, "text/plain; charset=utf-8", []byte(text+"\n"), r, at)
}
