package app

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"mmhotline/internal/cloudflareaccess"
)

var accessSigningKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

func configureAccessSignIn(t *testing.T, a *App) {
	t.Helper()
	key := accessSigningKey()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(server.Close)
	verifier, err := cloudflareaccess.NewVerifier("example.cloudflareaccess.com", "test123")
	if err != nil {
		t.Fatal(err)
	}
	verifier.CertsURL = server.URL
	a.AccessVerifier = verifier
}

func addAccessToken(request *http.Request, email string) {
	if email == "" {
		return
	}
	request.Header.Set("Cf-Access-Jwt-Assertion", signedAccessToken(accessSigningKey(), email))
}

func signedAccessToken(key *rsa.PrivateKey, email string) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
	claims, _ := json.Marshal(map[string]any{"iss": "https://example.cloudflareaccess.com", "aud": []string{"test123"}, "exp": time.Now().Add(time.Hour).Unix(), "email": email})
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(signature)
}
