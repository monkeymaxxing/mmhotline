// Package cloudflareapi makes the few Cloudflare v4 API calls that
// the MMHotline-down alarm's part of the settings file and `mmhotline status-page set-up` need, in the
// team's own Cloudflare account, with the team's API token. Each method
// cites the Cloudflare documentation it follows. Tests point Addresses at
// internal/fakecloudflare and never reach Cloudflare.
package cloudflareapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// Addresses are where MMHotline reaches Cloudflare.
type Addresses struct {
	// APIBaseURL is the v4 API, https://api.cloudflare.com/client/v4.
	APIBaseURL string
	// R2BucketURL is a bucket's S3 API address.
	R2BucketURL func(accountID, bucketName string) string
	// WorkerURL is a Worker's workers.dev address.
	WorkerURL func(scriptName, workersDevSubdomain string) string
}

// CloudflareAddresses are Cloudflare's real addresses.
func CloudflareAddresses() Addresses {
	return Addresses{
		APIBaseURL: "https://api.cloudflare.com/client/v4",
		// https://developers.cloudflare.com/r2/api/s3/api/
		R2BucketURL: func(accountID, bucketName string) string {
			return "https://" + accountID + ".r2.cloudflarestorage.com/" + bucketName
		},
		// https://developers.cloudflare.com/workers/configuration/routing/workers-dev/
		WorkerURL: func(scriptName, subdomain string) string {
			return "https://" + scriptName + "." + subdomain + ".workers.dev"
		},
	}
}

// Client calls the v4 API with one API token.
type Client struct {
	APIBaseURL string
	APIToken   string
	HTTPClient *http.Client
}

// Error is Cloudflare's answer to a refused or failed call. Its text never
// contains the API token.
type Error struct {
	Action         string
	HTTPStatusCode int
	Code           int
	Message        string
}

func (e *Error) Error() string {
	text := fmt.Sprintf("Cloudflare refused to %s (HTTP %d", e.Action, e.HTTPStatusCode)
	if e.Code != 0 {
		text += fmt.Sprintf(", error %d", e.Code)
	}
	text += ")"
	if e.Message != "" {
		text += ": " + e.Message
	}
	if e.HTTPStatusCode == http.StatusForbidden || e.HTTPStatusCode == http.StatusUnauthorized {
		text += ". Check the API token's permissions"
	}
	return text
}

// NotFound reports whether Cloudflare answered 404.
func NotFound(err error) bool {
	var failure *Error
	return errors.As(err, &failure) && failure.HTTPStatusCode == http.StatusNotFound
}

func (c *Client) httpClient() *http.Client {
	client := http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	// A redirect must never move the API token elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (c *Client) redact(text string) string {
	if c.APIToken != "" {
		text = strings.ReplaceAll(text, c.APIToken, "[API token removed]")
	}
	return text
}

// call sends one request and decodes the v4 envelope's result into result.
// https://developers.cloudflare.com/fundamentals/api/how-to/make-api-calls/
func (c *Client) call(ctx context.Context, action, method, path string, contentType string, body []byte, result any) error {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.APIBaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("prepare the request to %s: %w", action, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.APIToken)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("cannot reach Cloudflare's API to %s", action)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("cannot read Cloudflare's answer to %s", action)
	}
	var envelope struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
	}
	decodeError := json.Unmarshal(data, &envelope)
	if response.StatusCode < 200 || response.StatusCode > 299 || decodeError != nil || !envelope.Success {
		failure := &Error{Action: action, HTTPStatusCode: response.StatusCode}
		if len(envelope.Errors) > 0 {
			failure.Code = envelope.Errors[0].Code
			failure.Message = c.redact(envelope.Errors[0].Message)
		}
		return failure
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("Cloudflare's answer to %s does not match the expected JSON format", action)
		}
	}
	return nil
}

func (c *Client) callJSON(ctx context.Context, action, method, path string, body, result any) error {
	var data []byte
	contentType := ""
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return err
		}
		contentType = "application/json"
	}
	return c.call(ctx, action, method, path, contentType, data, result)
}

func accountPath(accountID string, parts ...string) string {
	path := "/accounts/" + url.PathEscape(accountID)
	for _, part := range parts {
		path += "/" + url.PathEscape(part)
	}
	return path
}

// ZoneForHostName finds the zone in the account that holds hostName: the
// host name itself or its nearest parent, such as example.com for
// status.example.com.
// https://developers.cloudflare.com/api/resources/zones/methods/list/
func (c *Client) ZoneForHostName(ctx context.Context, accountID, hostName string) (zoneID, zoneName string, err error) {
	labels := strings.Split(hostName, ".")
	for index := 0; index < len(labels)-1; index++ {
		candidate := strings.Join(labels[index:], ".")
		var zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		query := url.Values{"name": {candidate}, "account.id": {accountID}}
		if err := c.callJSON(ctx, "list the account's zones", http.MethodGet, "/zones?"+query.Encode(), nil, &zones); err != nil {
			return "", "", err
		}
		for _, zone := range zones {
			if zone.Name == candidate {
				return zone.ID, zone.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("no zone in Cloudflare account %s holds %s; the host name must belong to a zone in the same account", accountID, hostName)
}

// BucketExists reports whether the account has an R2 bucket of this name.
// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/methods/get/
func (c *Client) BucketExists(ctx context.Context, accountID, bucketName string) (bool, error) {
	err := c.callJSON(ctx, "read R2 bucket "+bucketName, http.MethodGet, accountPath(accountID, "r2", "buckets", bucketName), nil, nil)
	if NotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// CreateBucket creates an R2 bucket.
// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/methods/create/
func (c *Client) CreateBucket(ctx context.Context, accountID, bucketName string) error {
	return c.callJSON(ctx, "create R2 bucket "+bucketName, http.MethodPost, accountPath(accountID, "r2", "buckets"), map[string]string{"name": bucketName}, nil)
}

// CustomDomain is one host name connected to an R2 bucket.
type CustomDomain struct {
	Domain  string `json:"domain"`
	Enabled bool   `json:"enabled"`
	Status  struct {
		Ownership string `json:"ownership"`
		SSL       string `json:"ssl"`
	} `json:"status"`
}

// BucketCustomDomains lists the host names connected to a bucket.
// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/domains/subresources/custom/methods/list/
func (c *Client) BucketCustomDomains(ctx context.Context, accountID, bucketName string) ([]CustomDomain, error) {
	var result struct {
		Domains []CustomDomain `json:"domains"`
	}
	err := c.callJSON(ctx, "list the host names of R2 bucket "+bucketName, http.MethodGet, accountPath(accountID, "r2", "buckets", bucketName, "domains", "custom"), nil, &result)
	return result.Domains, err
}

// ConnectCustomDomain makes the bucket public at a host name in one of the
// account's zones.
// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/domains/subresources/custom/methods/create/
func (c *Client) ConnectCustomDomain(ctx context.Context, accountID, bucketName, hostName, zoneID string) error {
	body := map[string]any{"domain": hostName, "zoneId": zoneID, "enabled": true, "minTLS": "1.2"}
	return c.callJSON(ctx, "connect "+hostName+" to R2 bucket "+bucketName, http.MethodPost, accountPath(accountID, "r2", "buckets", bucketName, "domains", "custom"), body, nil)
}

// Rule is one rule of a ruleset.
// https://developers.cloudflare.com/ruleset-engine/rulesets-api/add-rule/
type Rule struct {
	ID               string         `json:"id,omitempty"`
	Ref              string         `json:"ref"`
	Description      string         `json:"description"`
	Expression       string         `json:"expression"`
	Action           string         `json:"action"`
	ActionParameters map[string]any `json:"action_parameters"`
}

// Ruleset is a zone's entry point ruleset for one phase.
type Ruleset struct {
	ID    string `json:"id"`
	Rules []Rule `json:"rules"`
}

const transformPhase = "http_request_transform"

// TransformRuleset returns the zone's URL rewrite rules, the entry point
// ruleset of the http_request_transform phase, or found=false when the zone
// has none yet.
// https://developers.cloudflare.com/ruleset-engine/rulesets-api/view/
func (c *Client) TransformRuleset(ctx context.Context, zoneID string) (Ruleset, bool, error) {
	var ruleset Ruleset
	err := c.callJSON(ctx, "read the zone's URL rewrite rules", http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/rulesets/phases/"+transformPhase+"/entrypoint", nil, &ruleset)
	if NotFound(err) {
		return Ruleset{}, false, nil
	}
	return ruleset, err == nil, err
}

// CreateTransformRuleset creates the zone's entry point ruleset for URL
// rewrite rules with one rule.
// https://developers.cloudflare.com/rules/transform/url-rewrite/create-api/
func (c *Client) CreateTransformRuleset(ctx context.Context, zoneID string, rule Rule) error {
	body := map[string]any{"name": "default", "kind": "zone", "phase": transformPhase, "rules": []Rule{rule}}
	return c.callJSON(ctx, "create the zone's URL rewrite rules", http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/rulesets", body, nil)
}

// AddRule adds a rule at the end of a ruleset, keeping the others.
// https://developers.cloudflare.com/ruleset-engine/rulesets-api/add-rule/
func (c *Client) AddRule(ctx context.Context, zoneID, rulesetID string, rule Rule) error {
	return c.callJSON(ctx, "add a URL rewrite rule", http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/rulesets/"+url.PathEscape(rulesetID)+"/rules", rule, nil)
}

// UpdateRule replaces one rule of a ruleset.
// https://developers.cloudflare.com/ruleset-engine/rulesets-api/update-rule/
func (c *Client) UpdateRule(ctx context.Context, zoneID, rulesetID string, rule Rule) error {
	ruleID := rule.ID
	rule.ID = ""
	return c.callJSON(ctx, "update a URL rewrite rule", http.MethodPatch, "/zones/"+url.PathEscape(zoneID)+"/rulesets/"+url.PathEscape(rulesetID)+"/rules/"+url.PathEscape(ruleID), rule, nil)
}

// WorkersDevSubdomain returns the account's workers.dev subdomain, such as
// "example" in example.workers.dev.
// https://developers.cloudflare.com/api/resources/workers/subresources/subdomains/methods/get/
func (c *Client) WorkersDevSubdomain(ctx context.Context, accountID string) (string, error) {
	var result struct {
		Subdomain string `json:"subdomain"`
	}
	err := c.callJSON(ctx, "read the account's workers.dev subdomain", http.MethodGet, accountPath(accountID, "workers", "subdomain"), nil, &result)
	if err == nil && result.Subdomain == "" {
		err = errors.New("the Cloudflare account has no workers.dev subdomain yet")
	}
	return result.Subdomain, err
}

// WorkerMigrationTag reports whether the account has a Worker of this name
// and the Durable Object migration tag it was last deployed with.
// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/methods/list/
func (c *Client) WorkerMigrationTag(ctx context.Context, accountID, scriptName string) (exists bool, migrationTag string, err error) {
	var scripts []struct {
		ID           string `json:"id"`
		MigrationTag string `json:"migration_tag"`
	}
	if err := c.callJSON(ctx, "list the account's Workers", http.MethodGet, accountPath(accountID, "workers", "scripts"), nil, &scripts); err != nil {
		return false, "", err
	}
	for _, script := range scripts {
		if script.ID == scriptName {
			return true, script.MigrationTag, nil
		}
	}
	return false, "", nil
}

// WorkerUpload is a Worker made of one ES module.
type WorkerUpload struct {
	ModuleName, ModuleSource string
	// Metadata is the upload's metadata part: main_module, compatibility_date,
	// bindings, migrations, and keep_bindings.
	// https://developers.cloudflare.com/workers/configuration/multipart-upload-metadata/
	Metadata map[string]any
}

// UploadWorker deploys a Worker from one ES module.
// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/methods/update/
func (c *Client) UploadWorker(ctx context.Context, accountID, scriptName string, upload WorkerUpload) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadata, err := json.Marshal(upload.Metadata)
	if err != nil {
		return err
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="metadata"`}, "Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	_, _ = part.Write(metadata) // The part writes into a bytes.Buffer, which never fails.
	part, err = writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {fmt.Sprintf(`form-data; name=%q; filename=%q`, upload.ModuleName, upload.ModuleName)}, "Content-Type": {"application/javascript+module"}})
	if err != nil {
		return err
	}
	_, _ = part.Write([]byte(upload.ModuleSource)) // The part writes into a bytes.Buffer, which never fails.
	if err := writer.Close(); err != nil {
		return err
	}
	return c.call(ctx, "upload Worker "+scriptName, http.MethodPut, accountPath(accountID, "workers", "scripts", scriptName), writer.FormDataContentType(), body.Bytes(), nil)
}

// EnableWorkersDev makes the Worker answer at its workers.dev address, with
// its preview addresses turned off. A request without previews_enabled turns
// previews on, and Cloudflare then refuses a Worker name over 54 characters
// (error 100315), which a long host name gives. With previews off, the limit
// is the 63 characters that cloudflareName keeps to. MMHotline never uses a
// preview address.
// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/subdomain/methods/create/
func (c *Client) EnableWorkersDev(ctx context.Context, accountID, scriptName string) error {
	return c.callJSON(ctx, "turn on the workers.dev address of Worker "+scriptName, http.MethodPost, accountPath(accountID, "workers", "scripts", scriptName, "subdomain"), map[string]bool{"enabled": true, "previews_enabled": false}, nil)
}

// SetWorkerSecret sets an encrypted environment variable of the Worker.
// Cloudflare never returns its value.
// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/secrets/methods/update/
func (c *Client) SetWorkerSecret(ctx context.Context, accountID, scriptName, name, value string) error {
	body := map[string]string{"name": name, "text": value, "type": "secret_text"}
	return c.callJSON(ctx, "set secret "+name+" of Worker "+scriptName, http.MethodPut, accountPath(accountID, "workers", "scripts", scriptName, "secrets"), body, nil)
}
