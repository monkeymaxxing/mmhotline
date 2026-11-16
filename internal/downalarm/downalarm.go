// Package downalarm carries the MMHotline-down alarm's Worker, one plain
// JavaScript file embedded in the mmhotline program, and the check-in
// protocol MMHotline speaks with it (docs/changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md).
package downalarm

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed mmhotline_down_alarm_worker.js
var workerSource string

const (
	// WorkerModuleName is the module's name in the upload.
	WorkerModuleName = "mmhotline_down_alarm_worker.js"
	// CheckInTokenSecretName is the Worker secret that holds the check-in
	// token. It keeps the feature's earlier name, the Lifeline: an upgrade
	// uploads the new Worker but keeps the secrets already set, so a new
	// secret name would leave an installed Worker without its token, refusing
	// every check-in and waking the person on call.
	CheckInTokenSecretName = "MMHOTLINE_LIFELINE_CHECK_IN_TOKEN"
	// DurableObjectClassName and DurableObjectBindingName name the one
	// SQLite-backed Durable Object class and the binding the Worker reaches
	// it through. The class keeps the feature's earlier name, the Lifeline:
	// Cloudflare renames a Durable Object class only through a migration
	// that installed Workers would need, and the class name is never shown.
	DurableObjectClassName   = "Lifeline"
	DurableObjectBindingName = "MMHOTLINE_DOWN_ALARM"
	// DurableObjectMigrationTag is the tag of the migration that creates the
	// class. A Worker deployed with it gets no migration again.
	DurableObjectMigrationTag = "v1"
	// compatibilityDate fixes the Workers runtime behavior the Worker is
	// written for. https://developers.cloudflare.com/workers/configuration/compatibility-dates/
	compatibilityDate = "2026-10-01"
)

// WorkerSource returns the Worker's JavaScript.
func WorkerSource() string { return workerSource }

// WorkerDigest identifies this build's Worker and its settings, so an
// upgraded MMHotline sees that the uploaded Worker is older and uploads it.
func WorkerDigest() string {
	sum := sha256.Sum256([]byte(compatibilityDate + "\n" + workerSource))
	return hex.EncodeToString(sum[:])
}

// WorkerMetadata is the upload's metadata part. A Worker that already runs
// with the migration tag gets no migrations, as Cloudflare's wrangler tool
// does; secrets are kept across uploads.
// https://developers.cloudflare.com/workers/configuration/multipart-upload-metadata/
func WorkerMetadata(migrationTag string) map[string]any {
	metadata := map[string]any{
		"main_module":        WorkerModuleName,
		"compatibility_date": compatibilityDate,
		"bindings":           []map[string]string{{"type": "durable_object_namespace", "name": DurableObjectBindingName, "class_name": DurableObjectClassName}},
		"keep_bindings":      []string{"secret_text"},
	}
	if migrationTag != DurableObjectMigrationTag {
		metadata["migrations"] = map[string]any{"new_tag": DurableObjectMigrationTag, "new_sqlite_classes": []string{DurableObjectClassName}}
	}
	return metadata
}

// HeldRequest is one ready-made HTTPS request.
type HeldRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// HeldMessage is what the MMHotline-down alarm sends when check-ins have stopped for
// DelaySeconds, and its test variant for "Send a test now".
type HeldMessage struct {
	HeldMessageID string      `json:"held_message_id"`
	DelaySeconds  int64       `json:"delay_seconds"`
	Request       HeldRequest `json:"request"`
	TestRequest   HeldRequest `json:"test_request"`
}

// Send is one held message the MMHotline-down alarm sent, or is sending.
type Send struct {
	SendID           int64      `json:"send_id"`
	HeldMessageID    string     `json:"held_message_id"`
	SilenceStartedAt time.Time  `json:"silence_started_at"`
	DueAt            time.Time  `json:"due_at"`
	Attempts         int        `json:"attempts"`
	SendState        string     `json:"send_state"`
	SendOutcome      string     `json:"send_outcome"`
	HTTPStatusCode   *int       `json:"http_status_code"`
	FinishedAt       *time.Time `json:"finished_at"`
}

// CheckInAnswer is the MMHotline-down alarm's answer to an accepted check-in.
type CheckInAnswer struct {
	CheckInAcceptedAt  time.Time `json:"check_in_accepted_at"`
	HeldMessagesStored int       `json:"held_messages_stored"`
	// Sends lists the sends of the last 7 days, newest first.
	Sends []Send `json:"sends"`
}

// TestSend is the result of one test request.
type TestSend struct {
	HeldMessageID  string `json:"held_message_id"`
	SendOutcome    string `json:"send_outcome"`
	HTTPStatusCode *int   `json:"http_status_code"`
}

// Refusal is the Worker's answer to a request it refused.
type Refusal struct {
	HTTPStatusCode int
	ErrorCode      string `json:"error_code"`
	Field          string `json:"field"`
	ErrorMessage   string `json:"error_message"`
}

func (r *Refusal) Error() string {
	if r.ErrorMessage == "" {
		return fmt.Sprintf("the MMHotline-down alarm answered HTTP %d", r.HTTPStatusCode)
	}
	return fmt.Sprintf("the MMHotline-down alarm answered HTTP %d: %s", r.HTTPStatusCode, r.ErrorMessage)
}

// Client talks to one MMHotline-down alarm Worker.
type Client struct {
	WorkerURL, CheckInToken string
	HTTPClient              *http.Client
}

// CheckIn hands over the held messages and returns the MMHotline-down alarm's answer.
func (c *Client) CheckIn(ctx context.Context, held []HeldMessage) (CheckInAnswer, error) {
	if held == nil {
		held = []HeldMessage{}
	}
	var answer CheckInAnswer
	err := c.post(ctx, "/check-in", map[string]any{"held_messages": held}, &answer)
	return answer, err
}

// SendTest asks the MMHotline-down alarm to send each held message's test request now.
func (c *Client) SendTest(ctx context.Context) ([]TestSend, error) {
	var answer struct {
		TestSends []TestSend `json:"test_sends"`
	}
	err := c.post(ctx, "/send-test", nil, &answer)
	return answer.TestSends, err
}

func (c *Client) post(ctx context.Context, path string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.WorkerURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.CheckInToken)
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 60 * time.Second}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	// A redirect must never carry the check-in token elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return errors.New("cannot reach the MMHotline-down alarm at " + c.WorkerURL)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return errors.New("cannot read the MMHotline-down alarm's answer")
	}
	if response.StatusCode != http.StatusOK {
		refusal := &Refusal{HTTPStatusCode: response.StatusCode}
		_ = json.Unmarshal(answer, refusal)
		return refusal
	}
	if err := json.Unmarshal(answer, result); err != nil {
		return errors.New("the MMHotline-down alarm's answer does not match the expected JSON format")
	}
	return nil
}
