package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mmhotline/internal/triageagent"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mmhotline/internal/app"
)

// Serve takes the three triage agent sockets together or not at all.
func TestServeTriageAgentSocketFlagsTogether(t *testing.T) {
	for _, flags := range [][]string{
		{"--triage-agent-run-socket", "run.sock"},
		{"--review-agent-run-socket", "review.sock"},
		{"--triage-agent-proxy-socket", "proxy.sock"},
		{"--triage-agent-run-socket", "run.sock", "--triage-agent-proxy-socket", "proxy.sock"},
		{"--review-agent-run-socket", "review.sock", "--triage-agent-proxy-socket", "proxy.sock"},
	} {
		output, err := cliProcess(t, append([]string{"serve", "--state-dir", "unused", "--settings-file", "unused.yml", "--command-socket", "command.sock"}, flags...)...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "--triage-agent-run-socket, --review-agent-run-socket, and --triage-agent-proxy-socket must be supplied together") {
			t.Fatal(flags, string(output), err)
		}
	}
}
func TestTriageAgentSandboxCommandHelpAndFailureExit(t *testing.T) {
	output, err := cliProcess(t, "triage-agent", "run-inside-sandbox", "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Systemd starts run-inside-sandbox for each triage agent run. People do not run run-inside-sandbox directly.") {
		t.Fatal(string(output), err)
	}
	process := cliProcess(t, "triage-agent", "run-inside-sandbox", "--proxy-socket", "unused")
	process.Stdin = strings.NewReader("{\"action\":\"invalid\"}\n")
	output, err = process.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"event":"failed"`) {
		t.Fatal("failed protocol exited successfully", string(output), err)
	}
}

// The installed binary runs the agent's request commands with their own exit
// codes: 0 for help, 4 outside a triage agent run.
func TestTriageAgentRequestCommandsExitCodes(t *testing.T) {
	output, err := cliProcess(t, "triage-agent", "request-playbook-run", "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "3  the request waits for a person: end your turn and report") {
		t.Fatal(string(output), err)
	}
	command := cliProcess(t, "triage-agent", "report", "--alert-severity", "warn", "--report-file", "report.md")
	command.Env = append(command.Env, triageagent.RequestSocketEnvironmentVariable+"=")
	output, _ = command.CombinedOutput()
	if command.ProcessState.ExitCode() != 4 || !strings.Contains(string(output), "works only inside a triage agent run") {
		t.Fatalf("outside a run: exit %d, %s", command.ProcessState.ExitCode(), output)
	}
}

// SSH runs ssh-proxy as the agent's ProxyCommand; the installed binary gives
// it SSH's exit code 255 outside a run.
func TestTriageAgentSSHProxyCommandExitCodes(t *testing.T) {
	output, err := cliProcess(t, "triage-agent", "ssh-proxy", "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "usage: mmhotline triage-agent ssh-proxy HOST PORT") {
		t.Fatal(string(output), err)
	}
	command := cliProcess(t, "triage-agent", "ssh-proxy", "app001", "22")
	command.Env = append(command.Env, triageagent.RequestSocketEnvironmentVariable+"=")
	output, _ = command.CombinedOutput()
	if command.ProcessState.ExitCode() != 255 || !strings.Contains(string(output), "works only inside a triage agent run") {
		t.Fatalf("outside a run: exit %d, %s", command.ProcessState.ExitCode(), output)
	}
	// The agent's ssh runs mmhotline triage-agent ssh with ssh's arguments.
	command = cliProcess(t, "triage-agent", "ssh", "-F", "ssh/config", "app001")
	command.Env = append(command.Env, triageagent.RequestSocketEnvironmentVariable+"=")
	output, _ = command.CombinedOutput()
	if command.ProcessState.ExitCode() != 255 || !strings.Contains(string(output), "mmhotline triage-agent ssh works only inside a triage agent run") {
		t.Fatalf("ssh outside a run: exit %d, %s", command.ProcessState.ExitCode(), output)
	}
}

func TestServeTriageAgentSocketsAndSetupWorkflow(t *testing.T) {
	skipUnlessCommandSocketClientAllowed(t)
	directory := shortSocketDirectory(t)
	runSocket := filepath.Join(directory, "run.sock")
	proxySocket := filepath.Join(directory, "proxy.sock")
	webAddress := tcpListenAddress(t)
	commandSocket := filepath.Join(directory, "command.sock")
	runListener, err := net.Listen("unix", runSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer runListener.Close()
	reviewRunSocket := filepath.Join(directory, "review.sock")
	reviewRunListener, err := net.Listen("unix", reviewRunSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer reviewRunListener.Close()
	// The triage agent's setup test, then the review agent's. Both agents
	// use an API key, whose sign-in check asks OpenAI's models list, played
	// here by modelsList, and runs no coding agent.
	answers := []string{"MMHotline triage agent test passed.", "MMHotline review agent test passed."}
	var modelsListStatus atomic.Int32
	modelsListStatus.Store(http.StatusOK)
	modelsListKeys := make(chan string, 8)
	modelsList := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" {
			http.NotFound(w, r)
			return
		}
		modelsListKeys <- r.Header.Get("Authorization")
		w.WriteHeader(int(modelsListStatus.Load()))
	}))
	defer modelsList.Close()
	runRequests := make(chan triageagent.Request, len(answers))
	runErrors := make(chan error, len(answers))
	// Each agent's runs come in on that agent's own run socket, which
	// systemd runs as that agent's own Unix user.
	type connectionOnSocket struct {
		connection net.Conn
		agent      string
		err        error
	}
	connections := make(chan connectionOnSocket)
	for agent, listener := range map[string]net.Listener{triageagent.TriageAgentRole: runListener, triageagent.ReviewAgentRole: reviewRunListener} {
		go func() {
			for {
				connection, err := listener.Accept()
				connections <- connectionOnSocket{connection, agent, err}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		for _, answer := range answers {
			accepted := <-connections
			connection, err := accepted.connection, accepted.err
			if err != nil {
				runErrors <- err
				return
			}
			var request triageagent.Request
			if err = json.NewDecoder(connection).Decode(&request); err != nil {
				connection.Close()
				runErrors <- err
				return
			}
			if request.AgentRole != accepted.agent {
				connection.Close()
				runErrors <- fmt.Errorf("a %s run came in on the %s run socket", request.AgentRole, accepted.agent)
				return
			}
			runRequests <- request
			encoder := json.NewEncoder(connection)
			if err = encoder.Encode(triageagent.Event{Event: "session", SessionID: "actual-socket-session"}); err == nil {
				err = encoder.Encode(triageagent.Event{Event: "finished", Answer: answer, CodingAgentSessionTotalUsage: &triageagent.Usage{InputTokens: 11, OutputTokens: 7}})
			}
			connection.Close()
			runErrors <- err
		}
	}()
	stateDir := filepath.Join(t.TempDir(), "state")
	if err = app.Setup(context.Background(), stateDir, "Fixture team"); err != nil {
		t.Fatal(err)
	}
	// Serve applies both agents' setups at start, each through its setup
	// test, with its model and reasoning effort. Any coding agent may
	// review, the triage agent's own included.
	settings := strings.Replace(testSettingsYAML("https://mmhotline.example.com", webAddress, "127.0.0.1:0"), "rules_for_running_agent_playbooks: {}", `triage_agent:
  commit_signing_public_key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPY0ynRIHnG58nyyazSZJWIqQ0zGxWHo5AF4GMnzX3fM example-triage-key"
  trusted_commit_signing_public_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKkV3VQGyLmAYtl/HR7x93Qw22ugM6k9OhTepuWlHFPr util01"]
  commits_without_a_trusted_signature_are_approved_by: a_person_or_the_triage_agent
  paths_only_a_person_may_approve: [mmhotline_settings.yml, triage_agent_rules.md, AGENTS.md, roles/every_server/files/]
  coding_agent: codex
  credential_type: api_key
  credential: server-only-key
  model: triage-model-1
  reasoning_effort: high
  internet_access: listed_hosts_only
  listed_hosts: []
  software_review_interval_hours: 24
  sos_waits_for_the_agent_minutes: 5
  sos_waits_for_a_server_replacement_minutes: 10
review_agent:
  coding_agent: codex
  credential_type: api_key
  credential: server-only-key
  model: review-model-2
  reasoning_effort: medium
rules_for_running_agent_playbooks: {}`, 1)
	settingsPath := writeTestSettingsFile(t, "", settings)
	serve := cliProcess(t, "serve", "--state-dir", stateDir, "--settings-file", settingsPath, "--command-socket", commandSocket, "--triage-agent-run-socket", runSocket, "--review-agent-run-socket", reviewRunSocket, "--triage-agent-proxy-socket", proxySocket)
	// The development fake sending services' address also serves the
	// coding agents' models lists; the settings file sets no sending service.
	serve.Env = append(serve.Env, "MMHOTLINE_FAKE_SENDING_SERVICES_URL="+modelsList.URL)
	var output bytes.Buffer
	serve.Stdout = &output
	serve.Stderr = &output
	if err = serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if serve.ProcessState == nil {
			serve.Process.Signal(syscall.SIGTERM)
			serve.Wait()
		}
	})
	client := tcpHTTPClient(webAddress)
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		response, err := client.Get("http://mmhotline/people")
		if err == nil {
			response.Body.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("serve did not start")
	}
	info, err := os.Stat(proxySocket)
	if err != nil || info.Mode().Perm() != 0666 {
		t.Fatal("proxy socket mode", info, err)
	}
	proxy, err := net.Dial("unix", proxySocket)
	if err != nil {
		t.Fatal(err)
	}
	proxy.SetDeadline(time.Now().Add(3 * time.Second))
	// The first line is the run's proxy token; this connection names none.
	io.WriteString(proxy, "\nCONNECT example.com:443 HTTP/1.1\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(proxy), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	proxy.Close()
	if response.StatusCode != 403 || !strings.Contains(string(body), "The connection names no active triage agent or review agent run.") {
		t.Fatal("proxy not served", string(body))
	}
	request := <-runRequests
	if request.AgentRole != triageagent.TriageAgentRole || request.Credential != "server-only-key" || request.CodingAgent != "codex" || request.Model != "triage-model-1" || request.ReasoningEffort != "high" {
		t.Fatalf("the triage agent's setup test: %+v", request)
	}
	if err = <-runErrors; err != nil {
		t.Fatal(err)
	}
	if request = <-runRequests; request.AgentRole != triageagent.ReviewAgentRole || request.CodingAgent != "codex" || request.Credential != "server-only-key" || request.Model != "review-model-2" || request.ReasoningEffort != "medium" {
		t.Fatalf("the review agent's setup test: %+v", request)
	}
	if err = <-runErrors; err != nil {
		t.Fatal(err)
	}
	// An apply waits for the parts serve applies in the background at
	// start, and finds both agents unchanged.
	resultBytes, err := cliProcess(t, "settings", "apply", "--command-socket", commandSocket, "--json").CombinedOutput()
	var result app.CommandResult
	if err != nil || json.Unmarshal(resultBytes, &result) != nil || result.SettingsFile == nil || len(result.SettingsFile.FailedParts()) != 0 || strings.Contains(string(resultBytes), "server-only-key") {
		t.Fatalf("settings apply: %s %v", resultBytes, err)
	}
	shown, err := cliProcess(t, "triage-agent", "show", "--command-socket", commandSocket).CombinedOutput()
	if err != nil || !strings.Contains(string(shown), "Triage agent: Codex with an API key, model triage-model-1, reasoning effort high (coding_agent codex, credential_type api_key, model triage-model-1, reasoning_effort high), set up by the settings file "+settingsPath) || !strings.Contains(string(shown), "Coding agent session ID: actual-socket-session") {
		t.Fatalf("triage-agent show: %s %v", shown, err)
	}
	shown, err = cliProcess(t, "review-agent", "show", "--command-socket", commandSocket).CombinedOutput()
	if err != nil || !strings.Contains(string(shown), "Review agent: Codex with an API key, model review-model-2, reasoning effort medium (coding_agent codex, credential_type api_key, model review-model-2, reasoning_effort medium)") {
		t.Fatalf("review-agent show: %s %v", shown, err)
	}
	// An administrator checks each agent's sign-in at once; the command
	// waits for the check, which asks the models list with the agent's key.
	for _, group := range []string{"triage-agent", "review-agent"} {
		output, err := cliProcess(t, group, "check-sign-in", "--command-socket", commandSocket).CombinedOutput()
		if err != nil || !strings.Contains(string(output), "Sign-in to Codex: works; last checked at ") || !strings.Contains(string(output), ": passed; run state: finished; started by the mmhotline command, run by ") {
			t.Fatalf("%s check-sign-in: %s %v", group, output, err)
		}
		if key := <-modelsListKeys; key != "Bearer server-only-key" {
			t.Fatalf("%s check sent %q", group, key)
		}
	}
	modelsListStatus.Store(http.StatusUnauthorized)
	failedCheck, err := cliProcess(t, "triage-agent", "check-sign-in", "--command-socket", commandSocket).CombinedOutput()
	if err == nil || !strings.Contains(string(failedCheck), "Sign-in to Codex: fails; last checked at ") || !strings.Contains(string(failedCheck), "answered 401 Unauthorized: the API key is wrong, or was deleted or disabled.") || strings.Contains(string(failedCheck), "server-only-key") {
		t.Fatalf("a failing check-sign-in: %s %v", failedCheck, err)
	}
	<-modelsListKeys
	select {
	case request := <-runRequests:
		t.Fatalf("a sign-in check ran the coding agent: %+v", request)
	default:
	}
	if err = serve.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err = serve.Wait(); err != nil {
		t.Fatalf("serve shutdown: %s %v", output.String(), err)
	}
}

func TestTriageAgentOtherCommandFlags(t *testing.T) {
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
		json.NewEncoder(w).Encode(app.CommandResult{TriageAgent: &app.TriageAgentView{InternetAccess: "any_site"}})
	})}
	go server.Serve(listener)
	defer server.Close()
	for _, test := range []struct {
		args []string
	}{
		{[]string{"show"}},
		{[]string{"run-software-review"}},
	} {
		args := append([]string{"triage-agent"}, test.args...)
		args = append(args, "--command-socket", socket)
		output, err := cliProcess(t, args...).CombinedOutput()
		if err != nil {
			t.Fatal(string(output), err)
		}
		request := <-requests
		if request.Group != "triage-agent" || request.Action != test.args[0] {
			t.Fatal("wrong command flag values")
		}
	}
}
