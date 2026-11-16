package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const webhookTimeout = 10 * time.Second

// OutgoingWebhookContactMethodType posts each alert as signed JSON to a customer's URL.
// Requests go only to public internet addresses: the address is checked when
// the connection is made, so a second DNS answer cannot redirect it.
// AllowedPrivateURLPrefix, set only with the fake sending services, is the one
// private URL prefix allowed (the fake webhook receiver).
type OutgoingWebhookContactMethodType struct {
	AllowedPrivateURLPrefix string
	Now                     func() time.Time
}

type webhookSettings struct {
	OutgoingWebhookURL string `json:"outgoing_webhook_url"`
}

// webhookSecrets is stored in secret and never shown after creation.
type webhookSecrets struct {
	SigningSecrets []webhookSigningSecret `json:"webhook_signing_secrets"`
	BearerToken    string                 `json:"outgoing_webhook_bearer_token,omitempty"`
}
type webhookSigningSecret struct {
	ID        string `json:"id"`
	SecretHex string `json:"secret_hex"`
	CreatedAt int64  `json:"created_at"`
}

func (OutgoingWebhookContactMethodType) Name() string { return "outgoing_webhook" }

func (OutgoingWebhookContactMethodType) Facts() ContactMethodTypeFacts {
	return ContactMethodTypeFacts{
		CardTitle: "Outgoing webhook", ValueLabel: "Outgoing webhook URL", ValueField: "outgoing_webhook_url",
		ValueInputType: "url", SettingsFields: []string{"outgoing_webhook_url", "outgoing_webhook_bearer_token"},
		RetryUncertain: true, AllowedForTeamAccount: true,
		ForMachines: true, MaximumPerInstallation: 20, AcceptedLabel: "Accepted by the webhook",
	}
}

func (c OutgoingWebhookContactMethodType) CheckSettings(_ context.Context, input ContactMethodSettingsInput) (ContactMethodSettings, error) {
	raw := strings.TrimSpace(input.Fields["outgoing_webhook_url"])
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return ContactMethodSettings{}, fmt.Errorf("%w: enter the full webhook URL, such as https://example.com/alerts-from-mmhotline", ErrInvalid)
	}
	fake := c.AllowedPrivateURLPrefix != "" && strings.HasPrefix(raw, c.AllowedPrivateURLPrefix)
	if c.AllowedPrivateURLPrefix != "" && !fake {
		return ContactMethodSettings{}, fmt.Errorf("%w: in development mode a webhook can only point at the fake webhook receiver, %s<name>", ErrInvalid, c.AllowedPrivateURLPrefix)
	}
	if u.Scheme != "https" && !fake {
		return ContactMethodSettings{}, fmt.Errorf("%w: the webhook URL must start with https://", ErrInvalid)
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !fake && !publicInternetAddress(ip) {
		return ContactMethodSettings{}, fmt.Errorf("%w: the webhook URL must be a public internet address", ErrInvalid)
	}
	token := strings.TrimSpace(input.Fields["outgoing_webhook_bearer_token"])
	if len(token) > 1024 || strings.IndexFunc(token, func(r rune) bool { return r > unicode.MaxASCII || unicode.IsControl(r) || r == ' ' }) >= 0 {
		return ContactMethodSettings{}, fmt.Errorf("%w: the webhook bearer token must be at most 1024 printable characters without spaces", ErrInvalid)
	}
	settings, err := settingsJSON(webhookSettings{OutgoingWebhookURL: raw})
	if err != nil {
		return ContactMethodSettings{}, err
	}
	secret, err := json.Marshal(webhookSecrets{SigningSecrets: []webhookSigningSecret{newWebhookSigningSecret(c.now())}, BearerToken: token})
	if err != nil {
		return ContactMethodSettings{}, err
	}
	// Paths and queries can hold tokens (Slack- and Discord-style URLs), so the
	// destination other members see is only the scheme and host.
	shown := u.Scheme + "://" + u.Host
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		shown += "/…"
	}
	return ContactMethodSettings{JSON: settings, Secret: string(secret), SentTo: shown}, nil
}

func (c OutgoingWebhookContactMethodType) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func newWebhookSigningSecret(at time.Time) webhookSigningSecret {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return webhookSigningSecret{ID: NewID(), SecretHex: hex.EncodeToString(b), CreatedAt: at.Unix()}
}

// webhookBody builds the JSON body. It depends only on the delivery's copied
// facts, so every retry sends the same bytes.
func webhookBody(m ContactMethodSend) ([]byte, error) {
	title, message := m.Title, m.AlertMessage
	if title == "" {
		title, message = m.Subject, m.Text
	}
	body := struct {
		DeliveryID         string `json:"mmhotline_delivery_id"`
		Purpose            string `json:"delivery_purpose"`
		Severity           string `json:"severity,omitempty"`
		Title              string `json:"title,omitempty"`
		Message            string `json:"message,omitempty"`
		NotesFromMmhotline string `json:"notes_from_mmhotline,omitempty"`
		AlertID            string `json:"mmhotline_alert_id,omitempty"`
		IncidentID         string `json:"mmhotline_incident_id,omitempty"`
		IncidentStatus     string `json:"incident_status,omitempty"`
		IncidentURL        string `json:"incident_url,omitempty"`
		AlertURL           string `json:"alert_url,omitempty"`
		AlertReceivedAt    string `json:"alert_received_at,omitempty"`
	}{DeliveryID: m.DeliveryID, Purpose: m.Purpose, Severity: m.Severity, Title: title, Message: message, NotesFromMmhotline: m.NotesFromMmhotline, AlertID: m.AlertID, IncidentID: m.IncidentID, IncidentStatus: m.IncidentStatus}
	if m.IncidentID != "" && m.PublicURL != "" {
		body.IncidentURL = m.PublicURL + "/incidents/" + m.IncidentID
	}
	if m.AlertID != "" && m.PublicURL != "" {
		body.AlertURL = m.PublicURL + "/alerts/" + m.AlertID
	}
	if !m.AlertReceivedAt.IsZero() {
		body.AlertReceivedAt = m.AlertReceivedAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(body)
}

// webhookSignature is exactly the alert intake scheme: HMAC-SHA256 with the
// hex-decoded secret over the timestamp, a dot, and the raw body.
func webhookSignature(secretHex string, timestamp int64, body []byte) (string, error) {
	key, err := hex.DecodeString(secretHex)
	if err != nil || len(key) != 32 {
		return "", errors.New("invalid webhook signing secret")
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(body)
	return fmt.Sprintf("timestamp_unix_seconds=%d,hmac_sha256_hex=%s", timestamp, hex.EncodeToString(mac.Sum(nil))), nil
}

func (c OutgoingWebhookContactMethodType) Send(ctx context.Context, m ContactMethodSend) DeliveryResult {
	var settings webhookSettings
	var secrets webhookSecrets
	if json.Unmarshal([]byte(m.Settings), &settings) != nil || json.Unmarshal([]byte(m.Secret), &secrets) != nil || len(secrets.SigningSecrets) == 0 {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the webhook settings are incomplete"}
	}
	body, err := webhookBody(m)
	if err != nil {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: could not encode the webhook body"}
	}
	// The oldest signing secret signs, so a receiver can add the second secret
	// before the first is revoked.
	signature, err := webhookSignature(secrets.SigningSecrets[0].SecretHex, c.now().Unix(), body)
	if err != nil {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the webhook signing secret is invalid"}
	}
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.OutgoingWebhookURL, bytes.NewReader(body))
	if err != nil {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the webhook URL is invalid"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "MMHotline webhook")
	req.Header.Set("Mmhotline-Signature", signature)
	req.Header.Set("Mmhotline-Delivery-Id", m.DeliveryID)
	if secrets.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+secrets.BearerToken)
	}
	fake := c.AllowedPrivateURLPrefix != "" && strings.HasPrefix(settings.OutgoingWebhookURL, c.AllowedPrivateURLPrefix)
	if c.AllowedPrivateURLPrefix != "" && !fake {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: development mode sends webhooks only to the fake webhook receiver"}
	}
	resp, err := publicInternetClient(fake, webhookTimeout).Do(req)
	if errors.Is(err, errAddressNotPublic) {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the webhook address is not a public internet address"}
	}
	var opError *net.OpError
	if err != nil && errors.As(err, &opError) && opError.Op == "dial" {
		// Nothing reached the receiver.
		return DeliveryResult{State: "failed", Diagnostic: "Could not connect to the webhook", Retryable: true}
	}
	if err != nil {
		if ctx.Err() != nil {
			return DeliveryResult{State: "uncertain", Diagnostic: "No answer from the webhook within 10 seconds", Retryable: true}
		}
		return DeliveryResult{State: "uncertain", Diagnostic: "The webhook connection broke before an answer", Retryable: true}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return DeliveryResult{State: "accepted", Diagnostic: "HTTP " + strconv.Itoa(code)}
	case code >= 500 || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests:
		return DeliveryResult{State: "failed", Diagnostic: fmt.Sprintf("The webhook answered HTTP %d", code), Retryable: true}
	}
	return DeliveryResult{State: "failed", Diagnostic: fmt.Sprintf("Refused by the webhook (HTTP %d)", code)}
}

// webhookSecretsFor reads a webhook contact method's secrets for an
// administrator or its owner.
func webhookSecretsFor(ctx context.Context, tx *sql.Tx, id string) (webhookSecrets, error) {
	var raw string
	var secrets webhookSecrets
	if err := tx.QueryRowContext(ctx, `SELECT secret FROM contact_methods WHERE id=? AND contact_method_type='outgoing_webhook' AND deleted=0`, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return secrets, ErrNotFound
		}
		return secrets, err
	}
	return secrets, json.Unmarshal([]byte(raw), &secrets)
}

func saveWebhookSecrets(ctx context.Context, tx *sql.Tx, id string, secrets webhookSecrets) error {
	raw, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	// Secrets are read when each attempt is sent, so pending deliveries simply
	// use the new ones; the contact method's revision does not change.
	_, err = tx.ExecContext(ctx, `UPDATE contact_methods SET secret=? WHERE id=?`, string(raw), id)
	return err
}

// webhookForChange loads a webhook contact method the member may edit.
func webhookForChange(ctx context.Context, tx *sql.Tx, p Principal, id string) (ContactMethod, webhookSecrets, error) {
	m, err := RequireMember(ctx, tx, p, false)
	if err != nil {
		return ContactMethod{}, webhookSecrets{}, err
	}
	c, err := contactMethodByID(ctx, tx, id)
	if err != nil {
		return c, webhookSecrets{}, err
	}
	if c.ContactMethodType != "outgoing_webhook" {
		return c, webhookSecrets{}, ErrNotFound
	}
	if !canEditContactMethod(m, c) {
		return c, webhookSecrets{}, ErrForbidden
	}
	secrets, err := webhookSecretsFor(ctx, tx, id)
	return c, secrets, err
}

// WebhookSigningSecretShownOnce is a new signing secret, displayed once.
type WebhookSigningSecretShownOnce struct {
	ContactMethodID, SigningSecretID, SecretHex string
}

// WebhookSigningSecretIDs lists a webhook's signing secret IDs, oldest first;
// the oldest one signs.
func (a *App) WebhookSigningSecretIDs(ctx context.Context, p Principal, id string) ([]string, error) {
	var ids []string
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, secrets, err := webhookForChange(ctx, tx, p, id)
		for _, s := range secrets.SigningSecrets {
			ids = append(ids, s.ID)
		}
		return err
	})
	return ids, err
}

// AddWebhookSigningSecret adds a second signing secret for rotation. The
// first keeps signing until it is revoked.
func (a *App) AddWebhookSigningSecret(ctx context.Context, p Principal, id string) (WebhookSigningSecretShownOnce, error) {
	var shown WebhookSigningSecretShownOnce
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, secrets, err := webhookForChange(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if len(secrets.SigningSecrets) >= 2 {
			return fmt.Errorf("%w: the outgoing webhook already has two signing secrets; revoke a signing secret before adding another", ErrConflict)
		}
		s := newWebhookSigningSecret(a.Now())
		secrets.SigningSecrets = append(secrets.SigningSecrets, s)
		shown = WebhookSigningSecretShownOnce{ContactMethodID: id, SigningSecretID: s.ID, SecretHex: s.SecretHex}
		return saveWebhookSecrets(ctx, tx, id, secrets)
	})
	return shown, err
}

// RevokeWebhookSigningSecret removes one of two signing secrets. The last
// one cannot be revoked.
func (a *App) RevokeWebhookSigningSecret(ctx context.Context, p Principal, id, signingSecretID string) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, secrets, err := webhookForChange(ctx, tx, p, id)
		if err != nil {
			return err
		}
		kept := secrets.SigningSecrets[:0]
		for _, s := range secrets.SigningSecrets {
			if s.ID != signingSecretID {
				kept = append(kept, s)
			}
		}
		if len(kept) == len(secrets.SigningSecrets) {
			return ErrNotFound
		}
		if len(kept) == 0 {
			return fmt.Errorf("%w: an outgoing webhook needs at least one signing secret; add a second signing secret before revoking the remaining signing secret", ErrConflict)
		}
		secrets.SigningSecrets = kept
		return saveWebhookSecrets(ctx, tx, id, secrets)
	})
}

// SetWebhookBearerToken replaces or, when token is empty, removes the bearer
// token sent as Authorization: Bearer.
func (a *App) SetWebhookBearerToken(ctx context.Context, p Principal, id, token string) error {
	token = strings.TrimSpace(token)
	if _, err := (OutgoingWebhookContactMethodType{}).CheckSettings(ctx, ContactMethodSettingsInput{Fields: map[string]string{"outgoing_webhook_url": "https://example.com", "outgoing_webhook_bearer_token": token}}); err != nil {
		return err
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, secrets, err := webhookForChange(ctx, tx, p, id)
		if err != nil {
			return err
		}
		secrets.BearerToken = token
		return saveWebhookSecrets(ctx, tx, id, secrets)
	})
}

// webhookSecretShownOnce returns the signing secret of a webhook that was
// just created, for its one-time display.
func webhookSecretShownOnce(c ContactMethod) *WebhookSigningSecretShownOnce {
	if c.ContactMethodType != "outgoing_webhook" || c.NewSecret == "" {
		return nil
	}
	var secrets webhookSecrets
	if json.Unmarshal([]byte(c.NewSecret), &secrets) != nil || len(secrets.SigningSecrets) == 0 {
		return nil
	}
	return &WebhookSigningSecretShownOnce{ContactMethodID: c.ID, SigningSecretID: secrets.SigningSecrets[0].ID, SecretHex: secrets.SigningSecrets[0].SecretHex}
}
