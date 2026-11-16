package main

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmhotline/internal/app"
)

func TestAgentPlaybookRulesCommandRequiredFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"show", "--preset", "routine_fixes_without_permission"}, "flag provided but not defined"},
		{[]string{"check-whether-a-playbook-needs-a-person", "--inventory", "production"}, "--playbook is required"},
		{[]string{"check-whether-a-playbook-needs-a-person", "--playbook", "playbooks/deploy.yml"}, "--inventory is required"},
	} {
		output, err := cliProcess(t, append([]string{"agent-playbook-rules"}, test.args...)...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), test.want) {
			t.Fatalf("%v: %s %v", test.args, output, err)
		}
	}
}

func TestAgentPlaybookRulesCommandFlags(t *testing.T) {
	directory := shortSocketDirectory(t)
	socket := filepath.Join(directory, "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan app.CommandRequest, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request app.CommandRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		json.NewEncoder(w).Encode(app.CommandResult{})
	})}
	go server.Serve(listener)
	defer server.Close()
	for _, test := range []struct {
		args []string
		want app.AgentPlaybookRulesCommandRequest
	}{
		{[]string{"show"}, app.AgentPlaybookRulesCommandRequest{}},
		{[]string{"show", "--inventory", "staging"}, app.AgentPlaybookRulesCommandRequest{Inventory: "staging"}},
		{[]string{"check-whether-a-playbook-needs-a-person", "--playbook", "one_time_playbooks/2026-11-03_restart_postgres_on_db01.yml", "--inventory", "production"}, app.AgentPlaybookRulesCommandRequest{Playbook: "one_time_playbooks/2026-11-03_restart_postgres_on_db01.yml", Inventory: "production"}},
	} {
		args := append([]string{"agent-playbook-rules"}, test.args...)
		args = append(args, "--command-socket", socket)
		output, err := cliProcess(t, args...).CombinedOutput()
		if err != nil {
			t.Fatal(string(output), err)
		}
		request := <-requests
		if request.Group != "agent-playbook-rules" || request.Action != test.args[0] || !reflect.DeepEqual(request.AgentPlaybookRules, test.want) {
			t.Fatalf("%v: %+v", test.args, request)
		}
	}
}

// check-whether-a-playbook-needs-a-person's help says what it shows, that it
// changes nothing, and that it does not ask the review agent, without
// connecting to the service.
func TestCheckWhetherAPlaybookNeedsAPersonHelp(t *testing.T) {
	output, err := cliProcess(t, "agent-playbook-rules", "check-whether-a-playbook-needs-a-person", "--help", "--command-socket", filepath.Join(t.TempDir(), "missing.sock")).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}
	help := strings.ReplaceAll(string(output), "\n", " ")
	for _, text := range []string{"usage: mmhotline agent-playbook-rules check-whether-a-playbook-needs-a-person --playbook PATH --inventory NAME", "-playbook", "-inventory", "-json", "Get permission, then act requires approval", "changes nothing", "The check-whether-a-playbook-needs-a-person command does not ask the review agent."} {
		if !strings.Contains(help, text) {
			t.Errorf("the help lacks %q:\n%s", text, output)
		}
	}
	if strings.Contains(strings.ToLower(help), "judge") {
		t.Errorf("the help says judge:\n%s", output)
	}
}

// The command's old name, judge-playbook, is an unknown action: the CLI
// refuses it with the usage that names the new one, and sends nothing.
func TestAgentPlaybookRulesRefusesTheOldJudgePlaybookName(t *testing.T) {
	command := cliProcess(t, "agent-playbook-rules", "judge-playbook", "--playbook", "playbooks/deploy.yml", "--inventory", "production", "--command-socket", filepath.Join(t.TempDir(), "missing.sock"))
	output, err := command.CombinedOutput()
	if err == nil || command.ProcessState.ExitCode() != 4 || !strings.Contains(string(output), "usage: mmhotline agent-playbook-rules show|check-whether-a-playbook-needs-a-person ") {
		t.Fatalf("exit %d: %s", command.ProcessState.ExitCode(), output)
	}
}
