package main

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
	"net/url"
	"sync"
	"testing"
	"time"
)

var cliAccessKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

func cliAccessSignIn(t *testing.T) (string, func(*http.Request, string)) {
	t.Helper()
	key := cliAccessKey()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(server.Close)
	return server.URL, func(request *http.Request, email string) {
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
		claims, _ := json.Marshal(map[string]any{"iss": "https://example.cloudflareaccess.com", "aud": []string{"test123"}, "email": email, "exp": time.Now().Add(time.Hour).Unix()})
		message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		digest := sha256.Sum256([]byte(message))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Cf-Access-Jwt-Assertion", message+"."+base64.RawURLEncoding.EncodeToString(signature))
	}
}

// The subprocess uses real verification with a local keys endpoint, without a production setting for it.
type cliAccessCertsTransport struct {
	next     http.RoundTripper
	certsURL *url.URL
}

func (transport cliAccessCertsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host == "example.cloudflareaccess.com" && request.URL.Path == "/cdn-cgi/access/certs" {
		request = request.Clone(request.Context())
		copied := *transport.certsURL
		request.URL = &copied
		request.Host = copied.Host
	}
	return transport.next.RoundTrip(request)
}
