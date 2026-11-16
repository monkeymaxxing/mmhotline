package cloudflareaccess

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidToken    = errors.New("invalid Cloudflare Access token")
	ErrKeysUnavailable = errors.New("Cloudflare Access signing keys unavailable")
)

const (
	maxTokenBytes = 16 * 1024
	maxCertsBytes = 1024 * 1024
	keyLifetime   = time.Hour
	// At most one fetch a minute: invented key IDs must not turn a token flood
	// into requests to Cloudflare, and pages must not each wait on a key
	// endpoint that is down.
	fetchInterval  = time.Minute
	clockAllowance = 60 * time.Second
)

type Verifier struct {
	CertsURL   string
	HTTPClient *http.Client
	Now        func() time.Time

	issuer      string
	audienceTag string
	mutex       sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	attemptedAt time.Time
}

func NewVerifier(teamDomain, audienceTag string) (*Verifier, error) {
	if err := CheckTeamDomain(teamDomain); err != nil {
		return nil, err
	}
	if err := CheckAudienceTag(audienceTag); err != nil {
		return nil, err
	}
	issuer := "https://" + teamDomain
	return &Verifier{
		CertsURL:    issuer + "/cdn-cgi/access/certs",
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
		Now:         time.Now,
		issuer:      issuer,
		audienceTag: audienceTag,
	}, nil
}

// CheckTeamDomain refuses a team domain that is not a bare host name, such
// as example.cloudflareaccess.com.
func CheckTeamDomain(domain string) error {
	if !validTeamDomain(domain) {
		return errors.New("the Cloudflare Access team domain must be a host name without a URL scheme, path, port, or spaces")
	}
	return nil
}

// CheckAudienceTag refuses an audience tag that is empty or has a character
// other than an ASCII letter or digit.
func CheckAudienceTag(audienceTag string) error {
	if audienceTag == "" {
		return errors.New("the Cloudflare Access audience tag must not be empty and must contain only ASCII letters and digits")
	}
	for _, character := range []byte(audienceTag) {
		if !asciiLetterOrDigit(character) {
			return errors.New("the Cloudflare Access audience tag must contain only ASCII letters and digits")
		}
	}
	return nil
}

func validTeamDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || !asciiLetterOrDigit(label[0]) || !asciiLetterOrDigit(label[len(label)-1]) {
			return false
		}
		for _, character := range []byte(label) {
			if !asciiLetterOrDigit(character) && character != '-' {
				return false
			}
		}
	}
	return true
}

func asciiLetterOrDigit(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func decodeBase64URL(encoded string) ([]byte, error) {
	for _, character := range []byte(encoded) {
		if !asciiLetterOrDigit(character) && character != '-' && character != '_' {
			return nil, errors.New("expected unpadded base64url")
		}
	}
	return base64.RawURLEncoding.Strict().DecodeString(encoded)
}

func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("%w: token exceeds 16 KiB", ErrInvalidToken)
	}
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return "", fmt.Errorf("%w: expected three non-empty segments", ErrInvalidToken)
	}
	var decoded [3][]byte
	for index, segment := range segments {
		if segment == "" {
			return "", fmt.Errorf("%w: expected three non-empty segments", ErrInvalidToken)
		}
		var err error
		decoded[index], err = decodeBase64URL(segment)
		if err != nil {
			return "", fmt.Errorf("%w: invalid base64url segment: %v", ErrInvalidToken, err)
		}
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(decoded[0], &header); err != nil {
		return "", fmt.Errorf("%w: invalid token header: %v", ErrInvalidToken, err)
	}
	if header.Algorithm != "RS256" {
		return "", fmt.Errorf("%w: signing algorithm must be RS256", ErrInvalidToken)
	}
	if header.KeyID == "" {
		return "", fmt.Errorf("%w: missing signing key ID", ErrInvalidToken)
	}
	key, err := v.signingKey(ctx, header.KeyID)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], decoded[2]); err != nil {
		return "", fmt.Errorf("%w: signature verification failed", ErrInvalidToken)
	}
	var claims struct {
		Issuer    string          `json:"iss"`
		Audience  json.RawMessage `json:"aud"`
		ExpiresAt *int64          `json:"exp"`
		NotBefore json.RawMessage `json:"nbf"`
		Email     string          `json:"email"`
	}
	if err := json.Unmarshal(decoded[1], &claims); err != nil {
		return "", fmt.Errorf("%w: invalid token claims: %v", ErrInvalidToken, err)
	}
	if claims.Issuer != v.issuer {
		return "", fmt.Errorf("%w: token issuer does not match the URL for the configured Cloudflare Access team domain", ErrInvalidToken)
	}
	var audiences []*string
	if err := json.Unmarshal(claims.Audience, &audiences); err != nil {
		var audience string
		if err := json.Unmarshal(claims.Audience, &audience); err != nil {
			return "", fmt.Errorf("%w: invalid audience claim", ErrInvalidToken)
		}
		audiences = []*string{&audience}
	}
	matchedAudience := false
	for _, audience := range audiences {
		if audience == nil {
			return "", fmt.Errorf("%w: invalid audience claim", ErrInvalidToken)
		}
		if *audience == v.audienceTag {
			matchedAudience = true
		}
	}
	if !matchedAudience {
		return "", fmt.Errorf("%w: token audience does not include the configured Cloudflare Access audience tag", ErrInvalidToken)
	}
	if claims.ExpiresAt == nil {
		return "", fmt.Errorf("%w: missing expiration time", ErrInvalidToken)
	}
	now := v.Now()
	if now.Add(-clockAllowance).After(time.Unix(*claims.ExpiresAt, 0)) {
		return "", fmt.Errorf("%w: token expired", ErrInvalidToken)
	}
	if len(claims.NotBefore) != 0 {
		var notBefore *int64
		if err := json.Unmarshal(claims.NotBefore, &notBefore); err != nil || notBefore == nil {
			return "", fmt.Errorf("%w: invalid token claims: nbf (not-before time) must be an integer number of Unix seconds", ErrInvalidToken)
		}
		if now.Add(clockAllowance).Before(time.Unix(*notBefore, 0)) {
			return "", fmt.Errorf("%w: token is not valid yet", ErrInvalidToken)
		}
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return "", fmt.Errorf("%w: missing or empty email claim, which holds the person's sign-in email address", ErrInvalidToken)
	}
	return email, nil
}

func (v *Verifier) signingKey(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	now := v.Now()
	_, known := v.keys[keyID]
	needed := len(v.keys) == 0 || now.Sub(v.fetchedAt) > keyLifetime || !known
	if needed && now.Sub(v.attemptedAt) > fetchInterval {
		v.attemptedAt = now
		keys, err := v.fetchKeys(ctx)
		if err != nil {
			if key := v.keys[keyID]; key != nil {
				return key, nil
			}
			return nil, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
		}
		v.keys = keys
		v.fetchedAt = v.Now()
	}
	if key := v.keys[keyID]; key != nil {
		return key, nil
	}
	if len(v.keys) == 0 {
		return nil, fmt.Errorf("%w: the last attempt to fetch Cloudflare Access signing keys failed less than a minute ago", ErrKeysUnavailable)
	}
	return nil, fmt.Errorf("%w: unknown signing key", ErrInvalidToken)
}

func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.CertsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create signing keys request: %v", err)
	}
	response, err := v.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch signing keys: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signing keys request returned HTTP status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCertsBytes))
	if err != nil {
		return nil, fmt.Errorf("read signing keys: %v", err)
	}
	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("decode signing keys JSON: %v", err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, encodedEntry := range document.Keys {
		var entry struct {
			KeyID    string `json:"kid"`
			KeyType  string `json:"kty"`
			Exponent string `json:"e"`
			Modulus  string `json:"n"`
		}
		if err := json.Unmarshal(encodedEntry, &entry); err != nil || entry.KeyType != "RSA" || entry.KeyID == "" {
			continue
		}
		exponentBytes, err := decodeBase64URL(entry.Exponent)
		if err != nil {
			continue
		}
		exponent := new(big.Int).SetBytes(exponentBytes)
		if !exponent.IsInt64() || exponent.Cmp(big.NewInt(1)) <= 0 || uint64(exponent.Int64()) > uint64(^uint(0)>>1) {
			continue
		}
		modulusBytes, err := decodeBase64URL(entry.Modulus)
		if err != nil {
			continue
		}
		modulus := new(big.Int).SetBytes(modulusBytes)
		if modulus.BitLen() < 2048 {
			continue
		}
		keys[entry.KeyID] = &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("signing keys JSON contains no usable RSA key")
	}
	return keys, nil
}
