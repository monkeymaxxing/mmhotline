package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmhotline/internal/app"
)

func TestCommandGroupAndActionHelp(t *testing.T) {
	for group, actions := range commandActions {
		output, err := cliProcess(t, group, "--help").CombinedOutput()
		if err != nil {
			t.Fatal(err, string(output))
		}
		for _, action := range actions {
			if !strings.Contains(string(output), action) {
				t.Fatalf("%s help omits %s", group, action)
			}
			actionOutput, err := cliProcess(t, group, action, "--help").CombinedOutput()
			// status-page set-up runs on a laptop and reaches no MMHotline.
			onALaptop := group == "status-page" && action == "set-up"
			if err != nil || !strings.Contains(string(actionOutput), "-json") || strings.Contains(string(actionOutput), "-command-socket") == onALaptop {
				t.Fatalf("%s %s help: %s %v", group, action, actionOutput, err)
			}
		}
	}
	output, err := cliProcess(t, "--help").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	for group, actions := range commandActions {
		for _, action := range actions {
			if !strings.Contains(string(output), group+" ") || !strings.Contains(string(output), action) {
				t.Fatalf("top-level help omits %s %s", group, action)
			}
		}
	}
}
func TestCommandSecretFilesAndBearerInput(t *testing.T) {
	directory := shortSocketDirectory(t)
	socket := filepath.Join(directory, "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	requests := make(chan app.CommandRequest, 8)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/command" {
			t.Errorf("wrong request path: %s", r.URL.Path)
		}
		var request app.CommandRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		result := app.CommandResult{}
		result.Message = "Contact method saved. A test message for the contact method is queued. Inspect the last test result to check delivery."
		result.OutgoingWebhookSigningSecretShownOnce = secret
		json.NewEncoder(w).Encode(result)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	for _, group := range []string{"contact-methods"} {
		for _, jsonOutput := range []bool{false, true} {
			target := filepath.Join(directory, group+"-secret")
			if err = os.WriteFile(target, []byte("old secret"), 0644); err != nil {
				t.Fatal(err)
			}
			bearer := filepath.Join(directory, "bearer-token")
			if err = os.WriteFile(bearer, []byte("private-bearer\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{group, "add", "--shared-with-team", "--contact-method-type", "outgoing_webhook", "--outgoing-webhook-url", "https://example.com/receive", "--outgoing-webhook-bearer-token-file", bearer, "--alert-severities", "sos", "--outgoing-webhook-signing-secret-file", target}
			args = append(args, "--command-socket", socket)
			if jsonOutput {
				args = append(args, "--json")
			}
			output, err := cliProcess(t, args...).CombinedOutput()
			if err != nil {
				t.Fatal(string(output), err)
			}
			if strings.Contains(string(output), secret) || strings.Contains(string(output), "private-bearer") || strings.Contains(string(output), "signing_secret_shown_once") || !strings.Contains(string(output), target) {
				t.Fatalf("secret leaked or file path missing: %s", output)
			}
			if strings.Contains(string(output), "shown only once") {
				t.Fatalf("stale secret warning: %s", output)
			}
			if jsonOutput {
				fileKey := "outgoing_webhook_signing_secret_file"
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(output, &fields); err != nil || string(fields[fileKey]) != `"`+target+`"` || len(fields) != 2 {
					t.Fatalf("wrong secret file fields: %s %v", output, err)
				}
			}
			raw, err := os.ReadFile(target)
			if err != nil || string(raw) != secret {
				t.Fatalf("secret file has wrong bytes: %v", err)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("secret file permissions: %v %v", info, err)
			}
			request := <-requests
			if group == "contact-methods" && (request.ContactMethods.OutgoingWebhookBearerToken != "private-bearer" || !request.ContactMethods.SharedWithTeam) {
				t.Fatal("bearer token was not read from its file")
			}
		}
	}
	output, err := cliProcess(t, "contact-methods", "add", "--shared-with-team", "--contact-method-type", "outgoing_webhook", "--outgoing-webhook-url", "https://example.com/receive", "--alert-severities", "sos", "--command-socket", socket, "--json").CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"outgoing_webhook_signing_secret_shown_once":"`+secret+`"`) {
		t.Fatalf("one-time webhook stdout secret missing: %s %v", output, err)
	}
	<-requests
	// Preparing an unwritable path fails before the request mutates the server.
	output, err = cliProcess(t, "contact-methods", "add", "--shared-with-team", "--contact-method-type", "outgoing_webhook", "--outgoing-webhook-url", "https://example.com/receive", "--alert-severities", "sos", "--command-socket", socket, "--outgoing-webhook-signing-secret-file", filepath.Join(directory, "missing", "secret")).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "prepare temporary outgoing webhook signing secret file") {
		t.Fatalf("bad secret path: %s %v", output, err)
	}
	select {
	case <-requests:
		t.Fatal("created a secret despite unusable output path")
	default:
	}
	files, err := filepath.Glob(filepath.Join(directory, ".mmhotline-signing-secret-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary secret files remain: %v %v", files, err)
	}
}

// schedules preview reads the on_call_schedule of a settings file, and
// refuses a file with problems before it asks MMHotline anything.
func TestSchedulesPreviewReadsASettingsFile(t *testing.T) {
	output, err := cliProcess(t, "schedules", "preview").CombinedOutput()
	if processExitCode(err) != 4 || !strings.Contains(string(output), "--settings-file is required") {
		t.Fatalf("no settings file: %s %v", output, err)
	}
	path := writeTestSettingsFile(t, "", strings.Replace(testSettingsYAML("https://mmhotline.example.com", "127.0.0.1:8480", "10.20.0.1:8484"), "  handoff_weekday: 1", "  handoff_weekday: 1\n  revision: 1", 1))
	output, err = cliProcess(t, "schedules", "preview", "--settings-file", path, "--command-socket", filepath.Join(t.TempDir(), "missing.sock")).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "on_call_schedule.revision: unknown key") || strings.Contains(string(output), "not running") {
		t.Fatalf("a settings file with a problem: %s %v", output, err)
	}
}

// The settings file sets what these commands changed, so the command line
// refuses them as unknown, with the group's usage.
func TestRemovedCommandsAreUnknown(t *testing.T) {
	for group, actions := range map[string][]string{
		"people":               {"add", "change", "remove"},
		"schedules":            {"save", "activate", "set-response-window"},
		"agent-playbook-rules": {"apply-preset", "set", "set-services-whose-changes-a-person-must-approve"},
		"alert-sources":        {"add", "add-signing-secret", "revoke-signing-secret", "disable", "set-check-in-times"},
		"triage-agent":         {"set-internet-access", "add-listed-host", "remove-listed-host", "set-software-review-interval", "set-up", "remove-setup"},
		"review-agent":         {"set-up", "remove-setup"},
		"sending-services":     {"set-up", "remove-setup"},
		"down-alarm":           {"set-up", "remove-setup"},
		"status-page":          {"set-r2-access-key"},
	} {
		for _, action := range actions {
			output, err := cliProcess(t, group, action).CombinedOutput()
			if processExitCode(err) != 4 || !strings.Contains(string(output), "usage: mmhotline "+group+" ") {
				t.Errorf("%s %s: %s %v", group, action, output, err)
			}
		}
	}
}
