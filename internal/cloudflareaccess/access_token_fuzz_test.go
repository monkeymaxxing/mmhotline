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
	"net/http"
	"strings"
	"testing"
	"time"
)

// failingTransport stands for Cloudflare's key endpoint in the fuzz target:
// the verifier already holds its key, so any fetch is a fault.
type failingTransport struct{ t *testing.T }

func (transport failingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Errorf("the verifier fetched %s while it held its keys", request.URL)
	return nil, errors.New("no key endpoint in the fuzz target")
}

// FuzzCloudflareAccessToken signs a fuzzed token header and claims with the
// key the verifier holds, as Cloudflare Access does, and verifies the token.
// The verifier never fetches keys it holds. A token either gives the
// signed-in email address, lowercased without surrounding spaces, or is
// refused with ErrInvalidToken and no address. An accepted token names the
// team's issuer and the application's audience tag and is not expired, with
// 60 seconds of clock allowance. The same token with any one byte changed is
// refused.
func FuzzCloudflareAccessToken(f *testing.F) {
	if sharedSigningKeyError != nil {
		f.Fatal(sharedSigningKeyError)
	}
	encode := func(value any) []byte {
		encoded, err := json.Marshal(value)
		if err != nil {
			f.Fatal(err)
		}
		return encoded
	}
	header := encode(map[string]any{"alg": "RS256", "kid": "current", "typ": "JWT"})
	claims := validClaims()
	f.Add(header, encode(claims), uint16(0), byte('A'))
	claims["aud"] = testAudience
	claims["nbf"] = testTime.Add(time.Minute).Unix()
	f.Add(header, encode(claims), uint16(40), byte('.'))
	claims["exp"] = testTime.Add(-2 * time.Minute).Unix()
	f.Add(encode(map[string]any{"alg": "none", "kid": "current"}), encode(claims), uint16(500), byte('_'))
	f.Add(encode(map[string]any{"alg": "RS256", "kid": "rotated"}), []byte(`{"iss":"https://example.cloudflareaccess.com","aud":[null],"exp":1.5e9,"email":" "}`), uint16(7), byte('='))
	f.Fuzz(func(t *testing.T, header, claims []byte, changeAt uint16, changeTo byte) {
		verifier, err := NewVerifier("example.cloudflareaccess.com", testAudience)
		if err != nil {
			t.Fatal(err)
		}
		verifier.Now = func() time.Time { return testTime }
		verifier.HTTPClient = &http.Client{Transport: failingTransport{t}}
		verifier.keys = map[string]*rsa.PublicKey{"current": &sharedSigningKey.PublicKey}
		verifier.fetchedAt, verifier.attemptedAt = testTime, testTime

		input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		digest := sha256.Sum256([]byte(input))
		signature, err := rsa.SignPKCS1v15(rand.Reader, sharedSigningKey, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		token := input + "." + base64.RawURLEncoding.EncodeToString(signature)
		email, err := verifier.Verify(context.Background(), token)
		switch {
		case err != nil && (!errors.Is(err, ErrInvalidToken) || email != ""):
			t.Fatalf("refused token: %q, %v", email, err)
		case err == nil && (email == "" || email != strings.ToLower(strings.TrimSpace(email))):
			t.Fatalf("accepted token gave the email address %q", email)
		case err == nil:
			checkAcceptedClaims(t, claims)
		}

		changed := []byte(token)
		index := int(changeAt) % len(changed)
		if changed[index] == changeTo {
			return
		}
		changed[index] = changeTo
		if email, err := verifier.Verify(context.Background(), string(changed)); !errors.Is(err, ErrInvalidToken) || email != "" {
			t.Fatalf("a token changed at byte %d after signing: %q, %v", index, email, err)
		}
	})
}

// checkAcceptedClaims reads the claims as encoding/json reads them.
func checkAcceptedClaims(t *testing.T, claims []byte) {
	t.Helper()
	var read struct {
		Issuer    string `json:"iss"`
		Audience  any    `json:"aud"`
		ExpiresAt int64  `json:"exp"`
	}
	if err := json.Unmarshal(claims, &read); err != nil {
		t.Fatalf("accepted claims that do not read: %v", err)
	}
	audiences, isList := read.Audience.([]any)
	if !isList {
		audiences = []any{read.Audience}
	}
	hasAudience := false
	for _, audience := range audiences {
		hasAudience = hasAudience || audience == testAudience
	}
	if read.Issuer != "https://example.cloudflareaccess.com" || !hasAudience || time.Unix(read.ExpiresAt, 0).Before(testTime.Add(-clockAllowance)) {
		t.Fatalf("accepted the claims %s", claims)
	}
}
