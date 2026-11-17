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
	"mmhotline/internal/fakesendingservices"
)

func TestPushoverCommandFlagsAndJSONFieldNames(t *testing.T) {
	directory := shortSocketDirectory(t)
	socket := filepath.Join(directory, "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan map[string]any, 3)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		json.NewEncoder(w).Encode(app.CommandResult{PeopleCommandResult: app.PeopleCommandResult{Message: "Saved.", WebURL: "https://mmhotline.example.com"}})
	})}
	go server.Serve(listener)
	defer server.Close()
	keyFile := filepath.Join(t.TempDir(), "pushover-user-key")
	if err := os.WriteFile(keyFile, []byte(fakesendingservices.PushoverUserKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args               []string
		group, field, want string
	}{
		{[]string{"contact-methods", "add", "--sign-in-email-address", "barbara@example.test", "--contact-method-type", "pushover_notification", "--pushover-user-key-file", keyFile}, "contact_methods", "pushover_user_key", fakesendingservices.PushoverUserKey},
		{[]string{"contact-methods", "add", "--sign-in-email-address", "barbara@example.test", "--contact-method-type", "pushover_notification", "--pushover-user-key-file", keyFile, "--pushover-device-name", "tablet"}, "contact_methods", "pushover_device_name", "tablet"},
	}
	for _, check := range cases {
		args := append(check.args, "--command-socket", socket, "--json")
		output, err := cliProcess(t, args...).CombinedOutput()
		if err != nil {
			t.Fatal(string(output), err)
		}
		var response map[string]any
		if err = json.Unmarshal(output, &response); err != nil {
			t.Fatal(err)
		}
		if response["message"] != "Saved." || response["web_url"] != "https://mmhotline.example.com" {
			t.Fatal(response)
		}
		request := <-requests
		values := request[check.group].(map[string]any)
		if values[check.field] != check.want {
			t.Fatalf("missing explicit %s: %+v", check.field, request)
		}
	}
	// - reads the key from standard input, so the key never appears in the
	// command line.
	fromStandardInput := cliProcess(t, "contact-methods", "add", "--sign-in-email-address", "barbara@example.test", "--contact-method-type", "pushover_notification", "--pushover-user-key-file", "-", "--command-socket", socket, "--json")
	fromStandardInput.Stdin = strings.NewReader(fakesendingservices.PushoverUserKey + "\n")
	if output, err := fromStandardInput.CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	if request := <-requests; request["contact_methods"].(map[string]any)["pushover_user_key"] != fakesendingservices.PushoverUserKey {
		t.Fatalf("key from standard input: %+v", request)
	}
	deviceOutput, deviceErr := cliProcess(t, "contact-methods", "add", "--contact-method-type", "email", "--pushover-device-name", "tablet").CombinedOutput()
	if deviceErr == nil || !strings.Contains(string(deviceOutput), "--pushover-device-name requires --contact-method-type pushover_notification") {
		t.Fatalf("device name on another kind: %s %v", deviceOutput, deviceErr)
	}
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	for wantError, keyFlags := range map[string][]string{
		"chmod 600 " + keyFile:                 {"--pushover-user-key-file", keyFile},
		"--pushover-user-key-file is required": {},
	} {
		output, err := cliProcess(t, append([]string{"contact-methods", "add", "--sign-in-email-address", "barbara@example.test", "--contact-method-type", "pushover_notification", "--command-socket", socket}, keyFlags...)...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), wantError) {
			t.Fatalf("want %q: %s %v", wantError, output, err)
		}
	}
	output, err := cliProcess(t, "contact-methods", "add", "--contact-method-type", "email", "--pushover-user-key-file", keyFile).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--pushover-user-key-file requires --contact-method-type pushover_notification") {
		t.Fatalf("wrong contact method type: %s %v", output, err)
	}
}
