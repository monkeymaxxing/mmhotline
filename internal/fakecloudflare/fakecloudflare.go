// Package fakecloudflare imitates the parts of Cloudflare that MMHotline
// uses, so the MMHotline-down alarm and status page set-ups and posts are tested without
// a Cloudflare account. Each handler implements only the calls MMHotline
// makes, following the Cloudflare documentation cited on it. A pass against
// this fake is never evidence that the real API behaves the same.
//
// The v4 API lives under {fake}/client/v4, a bucket's S3 API under
// {fake}/r2-s3/{account_id}/{bucket_name}, and a stand-in for an uploaded
// Worker's workers.dev address under {fake}/workers-dev/{script_name}.
package fakecloudflare

import (
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mmhotline/internal/cloudflareapi"
)

// Object is one object stored in a fake R2 bucket.
type Object struct {
	Content                   []byte
	ContentType, CacheControl string
	ETag                      string
}

// Zone is a zone of the fake account.
type Zone struct{ ID, Name string }

// Server is a fake Cloudflare with one account.
type Server struct {
	URL       string
	AccountID string
	APIToken  string
	// R2AccessKeyID is the one access key the fake S3 API accepts.
	R2AccessKeyID string
	// WorkersDevSubdomain is the account's workers.dev subdomain; empty
	// makes the account have none yet.
	WorkersDevSubdomain string
	// WorkersDevNotLiveForRequests makes that many requests to a workers.dev
	// address get Cloudflare's own 404, as before a new address is live.
	WorkersDevNotLiveForRequests int

	mu            sync.Mutex
	zones         []Zone
	buckets       map[string]bool
	customDomains map[string][]string
	rulesets      map[string]*cloudflareapi.Ruleset
	objects       map[string]map[string]Object
	workers       map[string]*Worker
	refuseNext    map[string]int
	calls         []string
	nextID        int
	server        *httptest.Server
	mux           *http.ServeMux
}

// New starts a fake Cloudflare for one test. Its account has one zone,
// example.com.
func New(t *testing.T) *Server {
	s := &Server{AccountID: "0123456789abcdef0123456789abcdef", APIToken: "fake-cloudflare-api-token", R2AccessKeyID: "f00dfeed0123456789abcdef01234567", WorkersDevSubdomain: "example-team", workers: map[string]*Worker{},
		zones: []Zone{{ID: "zone-example-com", Name: "example.com"}}, buckets: map[string]bool{}, customDomains: map[string][]string{},
		rulesets: map[string]*cloudflareapi.Ruleset{}, objects: map[string]map[string]Object{}, refuseNext: map[string]int{}, mux: http.NewServeMux()}
	api := "/client/v4"
	s.mux.HandleFunc("GET "+api+"/zones", s.api(s.listZones))
	s.mux.HandleFunc("GET "+api+"/accounts/{account}/r2/buckets/{bucket}", s.api(s.getBucket))
	s.mux.HandleFunc("POST "+api+"/accounts/{account}/r2/buckets", s.api(s.createBucket))
	s.mux.HandleFunc("GET "+api+"/accounts/{account}/r2/buckets/{bucket}/domains/custom", s.api(s.listCustomDomains))
	s.mux.HandleFunc("POST "+api+"/accounts/{account}/r2/buckets/{bucket}/domains/custom", s.api(s.connectCustomDomain))
	s.mux.HandleFunc("GET "+api+"/zones/{zone}/rulesets/phases/http_request_transform/entrypoint", s.api(s.getTransformRuleset))
	s.mux.HandleFunc("POST "+api+"/zones/{zone}/rulesets", s.api(s.createRuleset))
	s.mux.HandleFunc("POST "+api+"/zones/{zone}/rulesets/{ruleset}/rules", s.api(s.addRule))
	s.mux.HandleFunc("PATCH "+api+"/zones/{zone}/rulesets/{ruleset}/rules/{rule}", s.api(s.updateRule))
	s.mux.HandleFunc("GET /r2-s3/{account}/{bucket}/{key}", s.s3Object)
	s.mux.HandleFunc("PUT /r2-s3/{account}/{bucket}/{key}", s.s3Object)
	s.registerWorkerRoutes()
	s.server = httptest.NewServer(s.mux)
	s.URL = s.server.URL
	t.Cleanup(s.server.Close)
	return s
}

// Addresses points MMHotline at this fake.
func (s *Server) Addresses() cloudflareapi.Addresses {
	return cloudflareapi.Addresses{
		APIBaseURL:  s.URL + "/client/v4",
		R2BucketURL: func(accountID, bucketName string) string { return s.URL + "/r2-s3/" + accountID + "/" + bucketName },
		WorkerURL:   func(scriptName, _ string) string { return s.URL + "/workers-dev/" + scriptName },
	}
}

// RefuseNext makes the next count calls whose "METHOD path" starts with
// prefix fail with HTTP 403, as Cloudflare answers a token without the
// permission.
func (s *Server) RefuseNext(prefix string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseNext[prefix] = count
}

// Calls lists the v4 API calls made, as "METHOD path", oldest first.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// Object returns an object of a fake R2 bucket.
func (s *Server) Object(bucket, key string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[bucket][key]
	return object, ok
}

// AddBucket creates an empty R2 bucket, as the dashboard would.
func (s *Server) AddBucket(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[bucket] = true
}

// PutObject stores an object as another writer would, changing its ETag.
func (s *Server) PutObject(bucket, key string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeObject(bucket, key, Object{Content: content, ContentType: "application/json"})
}

// CustomDomains returns the host names connected to a bucket.
func (s *Server) CustomDomains(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.customDomains[bucket]...)
}

// TransformRules returns the zone's URL rewrite rules.
func (s *Server) TransformRules(zoneID string) []cloudflareapi.Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ruleset := s.rulesets[zoneID]; ruleset != nil {
		return append([]cloudflareapi.Rule(nil), ruleset.Rules...)
	}
	return nil
}

// apiHandler answers with a result, or an error status, code, and message.
type apiHandler func(r *http.Request) (result any, status, code int, message string)

// api wraps a handler in the v4 envelope and checks the bearer token.
// https://developers.cloudflare.com/fundamentals/api/how-to/make-api-calls/
func (s *Server) api(handle apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/client/v4")
		s.mu.Lock()
		s.calls = append(s.calls, call)
		refused := false
		for prefix, count := range s.refuseNext {
			if count > 0 && strings.HasPrefix(call, prefix) {
				s.refuseNext[prefix] = count - 1
				refused = true
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+s.APIToken {
			writeEnvelope(w, nil, http.StatusBadRequest, 6003, "Invalid request headers")
			return
		}
		if refused {
			writeEnvelope(w, nil, http.StatusForbidden, 10000, "Authentication error")
			return
		}
		if account := r.PathValue("account"); account != "" && account != s.AccountID {
			writeEnvelope(w, nil, http.StatusForbidden, 10000, "Authentication error")
			return
		}
		result, status, code, message := handle(r)
		writeEnvelope(w, result, status, code, message)
	}
}

func writeEnvelope(w http.ResponseWriter, result any, status, code int, message string) {
	if status == 0 {
		status = http.StatusOK
	}
	envelope := map[string]any{"success": status < 300, "errors": []any{}, "messages": []any{}, "result": result}
	if status >= 300 {
		envelope["errors"] = []any{map[string]any{"code": code, "message": message}}
		envelope["result"] = nil
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope)
}

func decodeBody(r *http.Request, into any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into) == nil
}

func (s *Server) id(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s-%d", prefix, s.nextID)
}

// https://developers.cloudflare.com/api/resources/zones/methods/list/
func (s *Server) listZones(r *http.Request) (any, int, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	zones := []map[string]any{}
	for _, zone := range s.zones {
		if zone.Name == r.URL.Query().Get("name") && r.URL.Query().Get("account.id") == s.AccountID {
			zones = append(zones, map[string]any{"id": zone.ID, "name": zone.Name, "status": "active"})
		}
	}
	return zones, 0, 0, ""
}

// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/methods/get/
func (s *Server) getBucket(r *http.Request) (any, int, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.buckets[r.PathValue("bucket")] {
		return nil, http.StatusNotFound, 10006, "The specified bucket does not exist."
	}
	return map[string]any{"name": r.PathValue("bucket"), "location": "WEUR"}, 0, 0, ""
}

// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/methods/create/
func (s *Server) createBucket(r *http.Request) (any, int, int, string) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(r, &body) || body.Name == "" {
		return nil, http.StatusBadRequest, 10040, "The JSON you provided was not well formed."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets[body.Name] {
		return nil, http.StatusConflict, 10004, "The bucket you tried to create already exists, and you own it."
	}
	s.buckets[body.Name] = true
	return map[string]any{"name": body.Name, "location": "WEUR"}, 0, 0, ""
}

// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/domains/subresources/custom/methods/list/
func (s *Server) listCustomDomains(r *http.Request) (any, int, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.buckets[r.PathValue("bucket")] {
		return nil, http.StatusNotFound, 10006, "The specified bucket does not exist."
	}
	domains := []map[string]any{}
	for _, domain := range s.customDomains[r.PathValue("bucket")] {
		domains = append(domains, map[string]any{"domain": domain, "enabled": true, "status": map[string]string{"ownership": "active", "ssl": "active"}})
	}
	return map[string]any{"domains": domains}, 0, 0, ""
}

// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/domains/subresources/custom/methods/create/
func (s *Server) connectCustomDomain(r *http.Request) (any, int, int, string) {
	var body struct {
		Domain  string `json:"domain"`
		ZoneID  string `json:"zoneId"`
		Enabled bool   `json:"enabled"`
		MinTLS  string `json:"minTLS"`
	}
	if !decodeBody(r, &body) || body.Domain == "" || body.ZoneID == "" {
		return nil, http.StatusBadRequest, 10040, "The JSON you provided was not well formed."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := r.PathValue("bucket")
	if !s.buckets[bucket] {
		return nil, http.StatusNotFound, 10006, "The specified bucket does not exist."
	}
	zoneFound := false
	for _, zone := range s.zones {
		zoneFound = zoneFound || (zone.ID == body.ZoneID && (body.Domain == zone.Name || strings.HasSuffix(body.Domain, "."+zone.Name)))
	}
	if !zoneFound {
		return nil, http.StatusBadRequest, 10057, "The domain is not in the zone."
	}
	s.customDomains[bucket] = append(s.customDomains[bucket], body.Domain)
	return map[string]any{"domain": body.Domain, "zoneId": body.ZoneID, "enabled": body.Enabled, "minTLS": body.MinTLS}, 0, 0, ""
}

// https://developers.cloudflare.com/ruleset-engine/rulesets-api/view/
func (s *Server) getTransformRuleset(r *http.Request) (any, int, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ruleset := s.rulesets[r.PathValue("zone")]
	if ruleset == nil {
		return nil, http.StatusNotFound, 10003, "Could not find entrypoint ruleset in the http_request_transform phase"
	}
	return ruleset, 0, 0, ""
}

// https://developers.cloudflare.com/rules/transform/url-rewrite/create-api/
func (s *Server) createRuleset(r *http.Request) (any, int, int, string) {
	var body struct {
		Name  string               `json:"name"`
		Kind  string               `json:"kind"`
		Phase string               `json:"phase"`
		Rules []cloudflareapi.Rule `json:"rules"`
	}
	if !decodeBody(r, &body) || body.Kind != "zone" || body.Phase != "http_request_transform" {
		return nil, http.StatusBadRequest, 20021, "invalid ruleset"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	zone := r.PathValue("zone")
	if s.rulesets[zone] != nil {
		return nil, http.StatusBadRequest, 20217, "A similar configuration with rules already exists and overwriting will have unintended consequences."
	}
	ruleset := &cloudflareapi.Ruleset{ID: s.id("ruleset")}
	for _, rule := range body.Rules {
		rule.ID = s.id("rule")
		ruleset.Rules = append(ruleset.Rules, rule)
	}
	s.rulesets[zone] = ruleset
	return ruleset, 0, 0, ""
}

// https://developers.cloudflare.com/ruleset-engine/rulesets-api/add-rule/
func (s *Server) addRule(r *http.Request) (any, int, int, string) {
	var rule cloudflareapi.Rule
	if !decodeBody(r, &rule) || rule.Expression == "" {
		return nil, http.StatusBadRequest, 20021, "invalid rule"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ruleset := s.rulesets[r.PathValue("zone")]
	if ruleset == nil || ruleset.ID != r.PathValue("ruleset") {
		return nil, http.StatusNotFound, 10003, "ruleset not found"
	}
	rule.ID = s.id("rule")
	ruleset.Rules = append(ruleset.Rules, rule)
	return ruleset, 0, 0, ""
}

// https://developers.cloudflare.com/ruleset-engine/rulesets-api/update-rule/
func (s *Server) updateRule(r *http.Request) (any, int, int, string) {
	var rule cloudflareapi.Rule
	if !decodeBody(r, &rule) || rule.Expression == "" {
		return nil, http.StatusBadRequest, 20021, "invalid rule"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ruleset := s.rulesets[r.PathValue("zone")]
	if ruleset == nil || ruleset.ID != r.PathValue("ruleset") {
		return nil, http.StatusNotFound, 10003, "ruleset not found"
	}
	for index := range ruleset.Rules {
		if ruleset.Rules[index].ID == r.PathValue("rule") {
			rule.ID = ruleset.Rules[index].ID
			ruleset.Rules[index] = rule
			return ruleset, 0, 0, ""
		}
	}
	return nil, http.StatusNotFound, 10003, "rule not found"
}

func (s *Server) storeObject(bucket, key string, object Object) Object {
	sum := md5.Sum(object.Content)
	s.nextID++
	// The ETag changes with every write, as a new version's does.
	object.ETag = fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(sum[:]), s.nextID)
	if s.objects[bucket] == nil {
		s.objects[bucket] = map[string]Object{}
	}
	s.objects[bucket][key] = object
	return object
}

// s3Object answers GetObject and PutObject, with PutObject's If-Match and
// If-None-Match conditions. It accepts any signature from the one access
// key; the signer itself is checked against AWS's published example.
// https://developers.cloudflare.com/r2/api/s3/api/
func (s *Server) s3Object(w http.ResponseWriter, r *http.Request) {
	bucket, key := r.PathValue("bucket"), r.PathValue("key")
	if r.PathValue("account") != s.AccountID || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential="+s.R2AccessKeyID+"/") || r.Header.Get("X-Amz-Date") == "" || r.Header.Get("X-Amz-Content-Sha256") == "" {
		s3Error(w, http.StatusForbidden, "AccessDenied", "Access Denied")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.buckets[bucket] {
		s3Error(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return
	}
	current, exists := s.objects[bucket][key]
	if r.Method == http.MethodGet {
		if !exists {
			s3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Header().Set("Content-Type", current.ContentType)
		// Real R2 answers a reader that accepts gzip with a gzipped body and
		// a weak ETag, which it then refuses in If-Match (seen 2026-10-27).
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("ETag", "W/"+current.ETag)
			w.Header().Set("Content-Encoding", "gzip")
			// A failed write means the client went away; nothing is left to do.
			compressed := gzip.NewWriter(w)
			_, _ = compressed.Write(current.Content)
			_ = compressed.Close()
			return
		}
		w.Header().Set("ETag", current.ETag)
		w.Write(current.Content)
		return
	}
	if match := r.Header.Get("If-Match"); match != "" && (!exists || match != current.ETag) {
		s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold.")
		return
	}
	if r.Header.Get("If-None-Match") == "*" && exists {
		s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold.")
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		s3Error(w, http.StatusBadRequest, "IncompleteBody", "The request body could not be read.")
		return
	}
	stored := s.storeObject(bucket, key, Object{Content: content, ContentType: r.Header.Get("Content-Type"), CacheControl: r.Header.Get("Cache-Control")})
	w.Header().Set("ETag", stored.ETag)
}

func s3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}
