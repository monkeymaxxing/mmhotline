package cloudflareaccess

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var sharedSigningKey, sharedSigningKeyError = rsa.GenerateKey(rand.Reader, 2048)

const testAudience = "Application123"

var testTime = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type certsServer struct {
	mutex    sync.Mutex
	requests int
	status   int
	body     string
}

func (server *certsServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.requests++
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writer.WriteHeader(server.status)
	io.WriteString(writer, server.body)
}

func (server *certsServer) setResponse(status int, body string) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.status = status
	server.body = body
}

func (server *certsServer) requestCount() int {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.requests
}

func newTestVerifier(t *testing.T) (*Verifier, *certsServer, *time.Time) {
	t.Helper()
	if sharedSigningKeyError != nil {
		t.Fatal(sharedSigningKeyError)
	}
	certs := &certsServer{status: http.StatusOK, body: jwks(t, publicKeyEntry("current", &sharedSigningKey.PublicKey))}
	server := httptest.NewServer(certs)
	t.Cleanup(server.Close)
	verifier, err := NewVerifier("example.cloudflareaccess.com", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	now := testTime
	verifier.CertsURL = server.URL
	verifier.HTTPClient = server.Client()
	verifier.Now = func() time.Time { return now }
	return verifier, certs, &now
}

func publicKeyEntry(keyID string, key *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kid": keyID, "kty": "RSA", "alg": "RS256", "use": "sig",
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
	}
}

func jwks(t *testing.T, keys ...map[string]any) string {
	t.Helper()
	return string(jsonBytes(t, map[string]any{"keys": keys, "public_cert": "ignored", "public_certs": "ignored"}))
}

func jsonBytes(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func validClaims() map[string]any {
	return map[string]any{
		"iss":           "https://example.cloudflareaccess.com",
		"aud":           []string{"anotherApplication", testAudience},
		"exp":           testTime.Add(4 * time.Hour).Unix(),
		"iat":           testTime.Unix(),
		"email":         "  Alice@Example.COM \t",
		"ignored_claim": map[string]any{"anything": true},
	}
}

func signedToken(t *testing.T, key *rsa.PrivateKey, header, claims any) string {
	t.Helper()
	headerSegment := base64.RawURLEncoding.EncodeToString(jsonBytes(t, header))
	payloadSegment := base64.RawURLEncoding.EncodeToString(jsonBytes(t, claims))
	return signSegments(t, key, headerSegment, payloadSegment)
}

func signSegments(t *testing.T, key *rsa.PrivateKey, header, payload string) string {
	t.Helper()
	input := header + "." + payload
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func tokenFor(t *testing.T, keyID string, claims any) string {
	t.Helper()
	return signedToken(t, sharedSigningKey, map[string]any{"alg": "RS256", "kid": keyID, "typ": "JWT"}, claims)
}

func requireError(t *testing.T, verifier *Verifier, token string, kind error, reason string) {
	t.Helper()
	email, err := verifier.Verify(context.Background(), token)
	if !errors.Is(err, kind) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Verify() = %q, %v; want %v containing %q", email, err, kind, reason)
	}
	if email != "" {
		t.Fatalf("failed verification returned email %q", email)
	}
}

func requireValid(t *testing.T, verifier *Verifier, token string) {
	t.Helper()
	email, err := verifier.Verify(context.Background(), token)
	if err != nil || email != "alice@example.com" {
		t.Fatalf("Verify() = %q, %v; want alice@example.com", email, err)
	}
}

func TestVerifyValidAudience(t *testing.T) {
	for _, audience := range []any{[]string{"anotherApplication", testAudience}, testAudience} {
		t.Run(string(jsonBytes(t, audience)), func(t *testing.T) {
			verifier, certs, _ := newTestVerifier(t)
			claims := validClaims()
			claims["aud"] = audience
			token := tokenFor(t, "current", claims)
			requireValid(t, verifier, token)
			requireValid(t, verifier, token)
			if count := certs.requestCount(); count != 1 {
				t.Fatalf("cached verification fetched keys %d times; want 1", count)
			}
		})
	}
}

func TestVerifyInvalidClaims(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]any)
		reason string
	}{
		{"wrong audience", func(c map[string]any) { c["aud"] = []string{"application123"} }, "token audience does not include the configured Cloudflare Access audience tag"},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://other.cloudflareaccess.com" }, "issuer does not match"},
		{"issuer trailing slash", func(c map[string]any) { c["iss"] = "https://example.cloudflareaccess.com/" }, "issuer does not match"},
		{"expired", func(c map[string]any) { c["exp"] = testTime.Add(-61 * time.Second).Unix() }, "token expired"},
		{"not yet valid", func(c map[string]any) { c["nbf"] = testTime.Add(61 * time.Second).Unix() }, "not valid yet"},
		{"missing email", func(c map[string]any) { delete(c, "email") }, "missing or empty email claim"},
		{"blank email", func(c map[string]any) { c["email"] = " \t" }, "missing or empty email claim"},
		{"non-string email", func(c map[string]any) { c["email"] = 1 }, "invalid token claims"},
		{"missing expiration", func(c map[string]any) { delete(c, "exp") }, "missing expiration"},
		{"null expiration", func(c map[string]any) { c["exp"] = nil }, "missing expiration"},
		{"string expiration", func(c map[string]any) { c["exp"] = "tomorrow" }, "invalid token claims"},
		{"non-numeric not before", func(c map[string]any) { c["nbf"] = false }, "invalid token claims"},
		{"null not before", func(c map[string]any) { c["nbf"] = nil }, "invalid token claims"},
		{"missing audience", func(c map[string]any) { delete(c, "aud") }, "invalid audience claim"},
		{"non-string audience", func(c map[string]any) { c["aud"] = 42 }, "invalid audience claim"},
		{"mixed audience array", func(c map[string]any) { c["aud"] = []any{testAudience, 42} }, "invalid audience claim"},
		{"null in audience array", func(c map[string]any) { c["aud"] = []any{testAudience, nil} }, "invalid audience claim"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verifier, _, _ := newTestVerifier(t)
			claims := validClaims()
			test.change(claims)
			requireError(t, verifier, tokenFor(t, "current", claims), ErrInvalidToken, test.reason)
		})
	}
}

func TestVerifyClockAllowance(t *testing.T) {
	for _, offset := range []time.Duration{-60 * time.Second, -59 * time.Second, 0} {
		t.Run("expiration "+offset.String(), func(t *testing.T) {
			verifier, _, _ := newTestVerifier(t)
			claims := validClaims()
			claims["exp"] = testTime.Add(offset).Unix()
			requireValid(t, verifier, tokenFor(t, "current", claims))
		})
	}
	for _, offset := range []time.Duration{0, 59 * time.Second, 60 * time.Second} {
		t.Run("not before "+offset.String(), func(t *testing.T) {
			verifier, _, _ := newTestVerifier(t)
			claims := validClaims()
			claims["nbf"] = testTime.Add(offset).Unix()
			requireValid(t, verifier, tokenFor(t, "current", claims))
		})
	}
}

func TestVerifyRejectsInvalidHeaderBeforeFetchingKeys(t *testing.T) {
	for name, header := range map[string]any{
		"none":              map[string]any{"alg": "none", "kid": "current"},
		"HS256":             map[string]any{"alg": "HS256", "kid": "current"},
		"other algorithm":   map[string]any{"alg": "RS512", "kid": "current"},
		"missing key ID":    map[string]any{"alg": "RS256"},
		"empty key ID":      map[string]any{"alg": "RS256", "kid": ""},
		"numeric key ID":    map[string]any{"alg": "RS256", "kid": 1},
		"non-object header": "header",
	} {
		t.Run(name, func(t *testing.T) {
			verifier, certs, _ := newTestVerifier(t)
			requireError(t, verifier, signedToken(t, sharedSigningKey, header, validClaims()), ErrInvalidToken, "")
			if count := certs.requestCount(); count != 0 {
				t.Fatalf("invalid header fetched keys %d times", count)
			}
		})
	}
}

func TestVerifySignatureBeforeClaims(t *testing.T) {
	verifier, _, _ := newTestVerifier(t)
	token := tokenFor(t, "current", validClaims())
	segments := strings.Split(token, ".")
	claims := validClaims()
	claims["email"] = "attacker@example.com"
	segments[1] = base64.RawURLEncoding.EncodeToString(jsonBytes(t, claims))
	requireError(t, verifier, strings.Join(segments, "."), ErrInvalidToken, "signature verification failed")
	segments[1] = base64.RawURLEncoding.EncodeToString([]byte("not JSON"))
	requireError(t, verifier, strings.Join(segments, "."), ErrInvalidToken, "signature verification failed")
	requireError(t, verifier, signSegments(t, sharedSigningKey, segments[0], segments[1]), ErrInvalidToken, "invalid token claims")
}

func TestVerifyMalformedToken(t *testing.T) {
	valid := tokenFor(t, "current", validClaims())
	segments := strings.Split(valid, ".")
	tests := map[string]string{
		"empty": "", "one segment": "a", "two segments": "a.b", "four segments": "a.b.c.d",
		"empty header":    "." + segments[1] + "." + segments[2],
		"empty payload":   segments[0] + ".." + segments[2],
		"empty signature": segments[0] + "." + segments[1] + ".",
		"oversize":        strings.Repeat("a", 16*1024+1),
		"bad header JSON": "e30Kew." + segments[1] + "." + segments[2],
	}
	for index, name := range []string{"header", "payload", "signature"} {
		for _, suffix := range []string{"=", "+", "/", "\n", "\r", " "} {
			changed := append([]string(nil), segments...)
			changed[index] += suffix
			tests[name+" suffix "+suffix] = strings.Join(changed, ".")
		}
		changed := append([]string(nil), segments...)
		changed[index] = "a"
		tests[name+" invalid length"] = strings.Join(changed, ".")
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			verifier, certs, _ := newTestVerifier(t)
			requireError(t, verifier, token, ErrInvalidToken, "")
			if count := certs.requestCount(); count != 0 {
				t.Fatalf("malformed token fetched keys %d times", count)
			}
		})
	}
}

func TestVerifyTokenSizeLimit(t *testing.T) {
	verifier, _, _ := newTestVerifier(t)
	claims := validClaims()
	claims["padding"] = ""
	header := base64.RawURLEncoding.EncodeToString(jsonBytes(t, map[string]any{"alg": "RS256", "kid": "current"}))
	payload := jsonBytes(t, claims)
	for size := len(payload); size < 16*1024; size++ {
		if len(header)+1+base64.RawURLEncoding.EncodedLen(size)+1+base64.RawURLEncoding.EncodedLen(sharedSigningKey.Size()) != 16*1024 {
			continue
		}
		claims["padding"] = strings.Repeat("a", size-len(payload))
		token := signSegments(t, sharedSigningKey, header, base64.RawURLEncoding.EncodeToString(jsonBytes(t, claims)))
		if len(token) != 16*1024 {
			t.Fatalf("token length = %d; want 16384", len(token))
		}
		requireValid(t, verifier, token)
		requireError(t, verifier, token+"a", ErrInvalidToken, "token exceeds 16 KiB")
		return
	}
	t.Fatal("could not construct a 16 KiB token")
}

func TestKeyRotation(t *testing.T) {
	verifier, certs, now := newTestVerifier(t)
	requireValid(t, verifier, tokenFor(t, "current", validClaims()))
	rotatedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	certs.setResponse(http.StatusOK, jwks(t, publicKeyEntry("rotated", &rotatedKey.PublicKey)))
	*now = now.Add(time.Minute + time.Second)
	token := signedToken(t, rotatedKey, map[string]any{"alg": "RS256", "kid": "rotated"}, validClaims())
	requireValid(t, verifier, token)
	requireValid(t, verifier, token)
	if count := certs.requestCount(); count != 2 {
		t.Fatalf("rotation made %d total requests; want 2", count)
	}
	requireError(t, verifier, tokenFor(t, "current", validClaims()), ErrInvalidToken, "unknown signing key")
}

func TestUnknownKeyFetchInterval(t *testing.T) {
	verifier, certs, now := newTestVerifier(t)
	requireValid(t, verifier, tokenFor(t, "current", validClaims()))
	for _, keyID := range []string{"invented1", "invented2", "invented3"} {
		requireError(t, verifier, tokenFor(t, keyID, validClaims()), ErrInvalidToken, "unknown signing key")
	}
	*now = now.Add(time.Minute)
	requireError(t, verifier, tokenFor(t, "atBoundary", validClaims()), ErrInvalidToken, "unknown signing key")
	if count := certs.requestCount(); count != 1 {
		t.Fatalf("unknown keys within one minute made %d requests; want 1", count)
	}
	*now = now.Add(time.Second)
	requireError(t, verifier, tokenFor(t, "afterMinute", validClaims()), ErrInvalidToken, "unknown signing key")
	for _, keyID := range []string{"invented4", "invented5"} {
		requireError(t, verifier, tokenFor(t, keyID, validClaims()), ErrInvalidToken, "unknown signing key")
	}
	if count := certs.requestCount(); count != 2 {
		t.Fatalf("unknown keys after one minute made %d requests; want 2", count)
	}
	*now = now.Add(time.Minute + time.Second)
	requireError(t, verifier, tokenFor(t, "afterNextMinute", validClaims()), ErrInvalidToken, "unknown signing key")
	if count := certs.requestCount(); count != 3 {
		t.Fatalf("unknown key after next minute made %d requests; want 3", count)
	}
}

func TestKeysUnavailable(t *testing.T) {
	t.Run("no cached key", func(t *testing.T) {
		verifier, certs, _ := newTestVerifier(t)
		certs.setResponse(http.StatusInternalServerError, "unavailable")
		requireError(t, verifier, tokenFor(t, "current", validClaims()), ErrKeysUnavailable, "500")
	})
	t.Run("cached key survives failed refresh", func(t *testing.T) {
		verifier, certs, now := newTestVerifier(t)
		token := tokenFor(t, "current", validClaims())
		requireValid(t, verifier, token)
		certs.setResponse(http.StatusInternalServerError, "unavailable")
		*now = now.Add(time.Hour + time.Second)
		requireValid(t, verifier, token)
		if count := certs.requestCount(); count != 2 {
			t.Fatalf("failed refresh made %d requests; want 2", count)
		}
		requireValid(t, verifier, token)
		if count := certs.requestCount(); count != 2 {
			t.Fatalf("stale keys within a minute of a failed refresh made %d requests; want 2", count)
		}
		certs.setResponse(http.StatusOK, jwks(t, publicKeyEntry("replacement", &sharedSigningKey.PublicKey)))
		*now = now.Add(time.Minute + time.Second)
		requireError(t, verifier, token, ErrInvalidToken, "unknown signing key")
		if count := certs.requestCount(); count != 3 {
			t.Fatalf("failed refresh changed successful fetch time: got %d requests; want 3", count)
		}
	})
	t.Run("no keys and a failed fetch within a minute makes no request", func(t *testing.T) {
		verifier, certs, now := newTestVerifier(t)
		certs.setResponse(http.StatusInternalServerError, "unavailable")
		token := tokenFor(t, "current", validClaims())
		requireError(t, verifier, token, ErrKeysUnavailable, "500")
		requireError(t, verifier, token, ErrKeysUnavailable, "less than a minute ago")
		if count := certs.requestCount(); count != 1 {
			t.Fatalf("repeated failures within a minute made %d requests; want 1", count)
		}
		certs.setResponse(http.StatusOK, jwks(t, publicKeyEntry("current", &sharedSigningKey.PublicKey)))
		*now = now.Add(time.Minute + time.Second)
		requireValid(t, verifier, token)
	})
	t.Run("failed unknown key refresh records attempt", func(t *testing.T) {
		verifier, certs, now := newTestVerifier(t)
		token := tokenFor(t, "current", validClaims())
		requireValid(t, verifier, token)
		certs.setResponse(http.StatusInternalServerError, "unavailable")
		*now = now.Add(time.Minute + time.Second)
		requireError(t, verifier, tokenFor(t, "unknown1", validClaims()), ErrKeysUnavailable, "500")
		requireValid(t, verifier, token)
		requireError(t, verifier, tokenFor(t, "unknown2", validClaims()), ErrInvalidToken, "unknown signing key")
		if count := certs.requestCount(); count != 2 {
			t.Fatalf("failed unknown key refresh made %d requests; want 2", count)
		}
		*now = now.Add(time.Minute + time.Second)
		requireError(t, verifier, tokenFor(t, "unknown3", validClaims()), ErrKeysUnavailable, "500")
		if count := certs.requestCount(); count != 3 {
			t.Fatalf("retry after failed attempt made %d requests; want 3", count)
		}
	})
}

func TestKeyLifetime(t *testing.T) {
	verifier, certs, now := newTestVerifier(t)
	token := tokenFor(t, "current", validClaims())
	requireValid(t, verifier, token)
	*now = now.Add(time.Hour)
	requireValid(t, verifier, token)
	if count := certs.requestCount(); count != 1 {
		t.Fatalf("keys no older than an hour fetched %d times; want 1", count)
	}
	*now = now.Add(time.Second)
	requireValid(t, verifier, token)
	if count := certs.requestCount(); count != 2 {
		t.Fatalf("keys older than an hour fetched %d times; want 2", count)
	}
	certs.setResponse(http.StatusOK, jwks(t, publicKeyEntry("replacement", &sharedSigningKey.PublicKey)))
	*now = now.Add(time.Hour + time.Second)
	requireError(t, verifier, token, ErrInvalidToken, "unknown signing key")
}

func TestFetchSkipsUnusableKeys(t *testing.T) {
	badKeys := []map[string]any{}
	for _, change := range []func(map[string]any){
		func(k map[string]any) { k["kty"] = "EC" },
		func(k map[string]any) { k["kid"] = "" },
		func(k map[string]any) { k["kid"] = 12 },
		func(k map[string]any) { k["kty"] = false },
		func(k map[string]any) { k["e"] = 65537 },
		func(k map[string]any) { k["n"] = []string{"invalid"} },
		func(k map[string]any) { k["e"] = "!" },
		func(k map[string]any) { k["e"] = "AA" },
		func(k map[string]any) { k["e"] = "AQ" },
		func(k map[string]any) {
			k["e"] = base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), 64).Bytes())
		},
		func(k map[string]any) {
			k["e"] = base64.RawURLEncoding.EncodeToString(new(big.Int).SetUint64(uint64(^uint(0)>>1) + 1).Bytes())
		},
		func(k map[string]any) { k["n"] = "!" },
		func(k map[string]any) { k["n"] = "" },
		func(k map[string]any) {
			k["n"] = base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), 2046).Bytes())
		},
	} {
		entry := publicKeyEntry("bad", &sharedSigningKey.PublicKey)
		change(entry)
		badKeys = append(badKeys, entry)
	}
	verifier, certs, _ := newTestVerifier(t)
	certs.setResponse(http.StatusOK, jwks(t, append(badKeys, publicKeyEntry("current", &sharedSigningKey.PublicKey))...))
	requireValid(t, verifier, tokenFor(t, "current", validClaims()))
	requireError(t, verifier, tokenFor(t, "bad", validClaims()), ErrInvalidToken, "unknown signing key")
	verifier, certs, _ = newTestVerifier(t)
	certs.setResponse(http.StatusOK, jwks(t, badKeys...))
	requireError(t, verifier, tokenFor(t, "current", validClaims()), ErrKeysUnavailable, "no usable RSA key")
	verifier, certs, _ = newTestVerifier(t)
	certs.setResponse(http.StatusOK, string(jsonBytes(t, map[string]any{
		"keys": []any{nil, 42, []any{}, publicKeyEntry("current", &sharedSigningKey.PublicKey)},
	})))
	requireValid(t, verifier, tokenFor(t, "current", validClaims()))
}

func TestFetchInvalidResponse(t *testing.T) {
	for name, body := range map[string]string{
		"bad JSON": "{", "missing keys": `{}`, "empty keys": `{"keys":[]}`,
		"wrong keys type": `{"keys":"not an array"}`, "trailing JSON": `{"keys":[]} {}`,
		"too large": strings.Repeat(" ", 1024*1024) + jwks(t, publicKeyEntry("current", &sharedSigningKey.PublicKey)),
	} {
		t.Run(name, func(t *testing.T) {
			verifier, certs, _ := newTestVerifier(t)
			certs.setResponse(http.StatusOK, body)
			requireError(t, verifier, tokenFor(t, "current", validClaims()), ErrKeysUnavailable, "")
		})
	}
	for _, status := range []int{http.StatusCreated, http.StatusNoContent, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			verifier, certs, _ := newTestVerifier(t)
			certs.setResponse(status, jwks(t, publicKeyEntry("current", &sharedSigningKey.PublicKey)))
			requireError(t, verifier, tokenFor(t, "current", validClaims()), ErrKeysUnavailable, "HTTP status")
		})
	}
}

func TestFetchResponseAtSizeLimit(t *testing.T) {
	verifier, certs, _ := newTestVerifier(t)
	body := jwks(t, publicKeyEntry("current", &sharedSigningKey.PublicKey))
	certs.setResponse(http.StatusOK, strings.Repeat(" ", 1024*1024-len(body))+body)
	requireValid(t, verifier, tokenFor(t, "current", validClaims()))
}

func TestFetchUsesRequestContext(t *testing.T) {
	verifier, certs, _ := newTestVerifier(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := verifier.Verify(ctx, tokenFor(t, "current", validClaims()))
	if !errors.Is(err, ErrKeysUnavailable) || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("canceled request = %v; want unavailable keys with context canceled", err)
	}
	if count := certs.requestCount(); count != 0 {
		t.Fatalf("canceled request reached server %d times", count)
	}
}

func TestConcurrentVerifySharesFetch(t *testing.T) {
	verifier, certs, _ := newTestVerifier(t)
	token := tokenFor(t, "current", validClaims())
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() { requireValid(t, verifier, token) })
	}
	workers.Wait()
	if count := certs.requestCount(); count != 1 {
		t.Fatalf("concurrent verifications made %d requests; want 1", count)
	}
}

func TestNewVerifier(t *testing.T) {
	verifier, err := NewVerifier("example.cloudflareaccess.com", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if verifier.CertsURL != "https://example.cloudflareaccess.com/cdn-cgi/access/certs" {
		t.Fatalf("CertsURL = %q", verifier.CertsURL)
	}
	if verifier.HTTPClient == nil || verifier.HTTPClient.Timeout != 10*time.Second {
		t.Fatalf("HTTPClient = %#v; want 10-second timeout", verifier.HTTPClient)
	}
	before := time.Now()
	now := verifier.Now()
	if now.Before(before) || now.After(time.Now()) {
		t.Fatalf("Now() = %v; want current time", now)
	}
	for _, domain := range []string{
		"", "https://example.cloudflareaccess.com", "example.cloudflareaccess.com/path",
		"example.cloudflareaccess.com:443", " example.cloudflareaccess.com", "example .cloudflareaccess.com",
		"example.cloudflareaccess.com?query", "example.cloudflareaccess.com#fragment", "user@example.cloudflareaccess.com",
		"example..com", "-example.com", "example-.com", "example.com.", "example_com", "éxample.com",
	} {
		t.Run("team domain "+domain, func(t *testing.T) {
			if _, err := NewVerifier(domain, testAudience); err == nil || !strings.Contains(err.Error(), "team domain") {
				t.Fatalf("NewVerifier(%q) = %v; want team domain error", domain, err)
			}
		})
	}
	for _, audience := range []string{"", "tag-dash", "tag_underscore", "space tag", "abc!", "é", "abc\n"} {
		t.Run("audience tag "+audience, func(t *testing.T) {
			if _, err := NewVerifier("example.cloudflareaccess.com", audience); err == nil || !strings.Contains(err.Error(), "audience tag") {
				t.Fatalf("NewVerifier audience %q = %v; want audience tag error", audience, err)
			}
		})
	}
}
