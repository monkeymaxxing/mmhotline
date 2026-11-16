package fakecloudflare

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"time"

	"mmhotline/internal/downalarm"
)

// Worker is what the fake knows about one uploaded Worker, and what its
// stand-in at {fake}/workers-dev/{script} received and answers.
type Worker struct {
	Module       string
	Metadata     map[string]any
	MigrationTag string
	Uploads      int
	WorkersDev   bool
	Secrets      map[string]string
	// CheckIns holds each check-in's held messages, oldest first.
	CheckIns [][]downalarm.HeldMessage
	// Sends and TestSends are what the stand-in answers with.
	Sends     []downalarm.Send
	TestSends []downalarm.TestSend
	// Unreachable makes the stand-in drop every connection, as when
	// Cloudflare cannot be reached.
	Unreachable bool
	TestsSent   int
}

func (s *Server) registerWorkerRoutes() {
	api := "/client/v4"
	s.mux.HandleFunc("GET "+api+"/accounts/{account}/workers/subdomain", s.api(s.workersSubdomain))
	s.mux.HandleFunc("GET "+api+"/accounts/{account}/workers/scripts", s.api(s.listWorkers))
	s.mux.HandleFunc("PUT "+api+"/accounts/{account}/workers/scripts/{script}", s.api(s.uploadWorker))
	s.mux.HandleFunc("POST "+api+"/accounts/{account}/workers/scripts/{script}/subdomain", s.api(s.enableWorkersDev))
	s.mux.HandleFunc("PUT "+api+"/accounts/{account}/workers/scripts/{script}/secrets", s.api(s.setWorkerSecret))
	s.mux.HandleFunc("POST /workers-dev/{script}/{action}", s.workerStandIn)
}

// Worker returns a copy of what the fake knows about a Worker.
func (s *Server) Worker(script string) (Worker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	worker, ok := s.workers[script]
	if !ok {
		return Worker{}, false
	}
	copied := *worker
	copied.CheckIns = slices.Clone(worker.CheckIns) //nolint:nilaway // uploadWorker, the only writer of s.workers, stores no nil Worker.
	return copied, true
}

// ChangeWorker changes what a Worker's stand-in answers.
func (s *Server) ChangeWorker(script string, change func(*Worker)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if worker, ok := s.workers[script]; ok {
		change(worker)
	}
}

// https://developers.cloudflare.com/api/resources/workers/subresources/subdomains/methods/get/
func (s *Server) workersSubdomain(*http.Request) (any, int, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.WorkersDevSubdomain == "" {
		return nil, http.StatusNotFound, 10007, "workers.dev subdomain not found"
	}
	return map[string]string{"subdomain": s.WorkersDevSubdomain}, 0, 0, ""
}

// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/methods/list/
func (s *Server) listWorkers(*http.Request) (any, int, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scripts := []map[string]any{}
	for name, worker := range s.workers {
		scripts = append(scripts, map[string]any{"id": name, "migration_tag": worker.MigrationTag})
	}
	return scripts, 0, 0, ""
}

// uploadWorker takes a multipart upload of one ES module. A new Durable
// Object class needs its migration once; a migration for a class that
// already exists is refused. Secrets stay only with keep_bindings.
// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/methods/update/
//
//nolint:nilaway // A Worker missing from s.workers is replaced by a new one before use; the fake stores no nil Worker.
func (s *Server) uploadWorker(r *http.Request) (any, int, int, string) {
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return nil, http.StatusBadRequest, 10021, "the upload must be multipart/form-data"
	}
	reader := multipart.NewReader(r.Body, parameters["boundary"])
	parts := map[string]string{}
	types := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, http.StatusBadRequest, 10021, "unreadable multipart body"
		}
		data, _ := io.ReadAll(io.LimitReader(part, 10<<20))
		parts[part.FormName()] = string(data)
		types[part.FormName()] = part.Header.Get("Content-Type")
	}
	var metadata struct {
		MainModule        string           `json:"main_module"`
		CompatibilityDate string           `json:"compatibility_date"`
		Bindings          []map[string]any `json:"bindings"`
		KeepBindings      []string         `json:"keep_bindings"`
		Migrations        *struct {
			NewTag           string   `json:"new_tag"`
			NewSQLiteClasses []string `json:"new_sqlite_classes"`
		} `json:"migrations"`
	}
	if err := json.Unmarshal([]byte(parts["metadata"]), &metadata); err != nil || metadata.MainModule == "" || metadata.CompatibilityDate == "" {
		return nil, http.StatusBadRequest, 10021, "metadata needs main_module and compatibility_date"
	}
	module, ok := parts[metadata.MainModule]
	if !ok || types[metadata.MainModule] != "application/javascript+module" {
		return nil, http.StatusBadRequest, 10021, "no ES module part named by main_module"
	}
	var classes []string
	for _, binding := range metadata.Bindings {
		if binding["type"] == "durable_object_namespace" {
			class, _ := binding["class_name"].(string)
			if !strings.Contains(module, "export class "+class+" ") {
				return nil, http.StatusBadRequest, 10061, "the script does not export Durable Object class " + class
			}
			classes = append(classes, class)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	script := r.PathValue("script")
	worker := s.workers[script]
	isNew := worker == nil
	if isNew {
		worker = &Worker{Secrets: map[string]string{}}
	}
	switch {
	case metadata.Migrations != nil && !isNew && worker.MigrationTag == metadata.Migrations.NewTag:
		return nil, http.StatusBadRequest, 10074, "migration tag " + metadata.Migrations.NewTag + " was already applied"
	case metadata.Migrations == nil && isNew && len(classes) > 0:
		return nil, http.StatusBadRequest, 10074, "a new Durable Object class needs a migration"
	}
	if metadata.Migrations != nil {
		worker.MigrationTag = metadata.Migrations.NewTag
	}
	if !slices.Contains(metadata.KeepBindings, "secret_text") {
		worker.Secrets = map[string]string{}
	}
	worker.Module, worker.Uploads, worker.Metadata = module, worker.Uploads+1, nil
	_ = json.Unmarshal([]byte(parts["metadata"]), &worker.Metadata)
	s.workers[script] = worker
	return map[string]any{"id": script, "etag": s.id("etag"), "modified_on": time.Now().UTC().Format(time.RFC3339)}, 0, 0, ""
}

// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/subdomain/methods/create/
func (s *Server) enableWorkersDev(r *http.Request) (any, int, int, string) {
	var body struct {
		Enabled         bool  `json:"enabled"`
		PreviewsEnabled *bool `json:"previews_enabled"`
	}
	if !decodeBody(r, &body) {
		return nil, http.StatusBadRequest, 10021, "invalid body"
	}
	// Like Cloudflare, a request without previews_enabled turns previews on,
	// and previews need a Worker name of at most 54 characters.
	previews := body.PreviewsEnabled == nil || *body.PreviewsEnabled
	script := r.PathValue("script")
	if body.Enabled && previews && len(script) > 54 {
		return nil, http.StatusBadRequest, 100315, "Preview URLs need a Worker name of at most 54 characters."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	worker := s.workers[script]
	if worker == nil {
		return nil, http.StatusNotFound, 10007, "This Worker does not exist on your account."
	}
	worker.WorkersDev = body.Enabled
	return map[string]bool{"enabled": body.Enabled, "previews_enabled": previews}, 0, 0, ""
}

// https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/secrets/methods/update/
func (s *Server) setWorkerSecret(r *http.Request) (any, int, int, string) {
	var body struct {
		Name string `json:"name"`
		Text string `json:"text"`
		Type string `json:"type"`
	}
	if !decodeBody(r, &body) || body.Name == "" || body.Type != "secret_text" {
		return nil, http.StatusBadRequest, 10021, "invalid secret"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	worker := s.workers[r.PathValue("script")]
	if worker == nil {
		return nil, http.StatusNotFound, 10007, "This Worker does not exist on your account."
	}
	worker.Secrets[body.Name] = body.Text
	return map[string]string{"name": body.Name, "type": body.Type}, 0, 0, ""
}

// workerStandIn answers check-ins and test requests in the Worker's place,
// with the Worker's token check and strict field names. The Worker's own
// logic is tested by mmhotline_down_alarm_worker_test.mjs.
func (s *Server) workerStandIn(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	worker := s.workers[r.PathValue("script")]
	if worker != nil && worker.Unreachable {
		s.mu.Unlock()
		if hijacker, ok := w.(http.Hijacker); ok {
			if connection, _, err := hijacker.Hijack(); err == nil {
				connection.Close()
				return
			}
		}
		http.Error(w, "unreachable", http.StatusBadGateway)
		return
	}
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	answer := func(status int, body any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	if s.WorkersDevNotLiveForRequests > 0 {
		s.WorkersDevNotLiveForRequests--
		http.Error(w, "There is nothing here yet", http.StatusNotFound)
		return
	}
	switch {
	case worker == nil || !worker.WorkersDev:
		answer(http.StatusNotFound, map[string]string{"error_code": "not_found"})
	case worker.Secrets[downalarm.CheckInTokenSecretName] == "":
		answer(http.StatusServiceUnavailable, map[string]string{"error_code": "check_in_token_not_set", "error_message": "The Worker's secret is not set."})
	case r.Header.Get("Authorization") != "Bearer "+worker.Secrets[downalarm.CheckInTokenSecretName]:
		answer(http.StatusUnauthorized, map[string]string{"error_code": "check_in_token_wrong", "error_message": "The check-in token is wrong."})
	case r.PathValue("action") == "send-test":
		worker.TestsSent++
		answer(http.StatusOK, map[string]any{"test_sends": worker.TestSends})
	case r.PathValue("action") == "check-in":
		var body struct {
			HeldMessages []downalarm.HeldMessage `json:"held_messages"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.HeldMessages == nil {
			answer(http.StatusBadRequest, map[string]string{"error_code": "check_in_refused", "error_message": "The check-in must be JSON with held_messages."})
			return
		}
		worker.CheckIns = append(worker.CheckIns, body.HeldMessages)
		sends := worker.Sends
		if sends == nil {
			sends = []downalarm.Send{}
		}
		answer(http.StatusOK, map[string]any{"check_in_accepted_at": time.Now().UTC().Format(time.RFC3339), "held_messages_stored": len(body.HeldMessages), "sends": sends})
	default:
		answer(http.StatusNotFound, map[string]string{"error_code": "not_found"})
	}
}
