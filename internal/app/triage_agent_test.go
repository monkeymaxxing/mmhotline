package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

type fakeTriageRun struct {
	connection net.Conn
	request    triageagent.Request
	// socket is the agent whose run socket the run came in on.
	socket string
}

// triageRunFixture listens on both agents' run sockets, as systemd does, and
// fails the test when a run comes in on the other agent's socket: each
// agent's runs run as that agent's own Unix user.
func triageRunFixture(t *testing.T, a *App) <-chan fakeTriageRun {
	t.Helper()
	// A short socket path also fits macOS's Unix socket path limit.
	directory, err := os.MkdirTemp("/tmp", "triage-")
	if err != nil {
		t.Fatal(err)
	}
	listeners := map[string]net.Listener{}
	for _, agent := range []string{triageagent.TriageAgentRole, triageagent.ReviewAgentRole} {
		if listeners[agent], err = net.Listen("unix", filepath.Join(directory, agent+".sock")); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.InitializeTriageAgent(context.Background(), filepath.Join(directory, triageagent.TriageAgentRole+".sock"), filepath.Join(directory, triageagent.ReviewAgentRole+".sock"), filepath.Join(directory, "proxy.sock")); err != nil {
		t.Fatal(err)
	}
	// An API key's sign-in check never reaches a vendor from a test.
	installFakeModelsLists(t, a)
	runs := make(chan fakeTriageRun, 8)
	// The fixture keeps each connection open until the test ends, as a
	// sandbox keeps its end until the run ends. A connection that no test
	// still uses would otherwise be closed by the garbage collector, ending
	// its run at a random moment.
	var connectionsMu sync.Mutex
	var connections []net.Conn
	for socket, listener := range listeners {
		go func() {
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				connectionsMu.Lock()
				connections = append(connections, connection)
				connectionsMu.Unlock()
				var request triageagent.Request
				if err = json.NewDecoder(connection).Decode(&request); err != nil {
					connection.Close()
					return
				}
				if request.AgentRole != socket {
					t.Errorf("a %s run came in on the %s run socket", request.AgentRole, socket)
				}
				runs <- fakeTriageRun{connection, request, socket}
			}
		}()
	}
	t.Cleanup(func() {
		a.CloseTriageAgent()
		for _, listener := range listeners {
			listener.Close()
		}
		os.RemoveAll(directory)
		connectionsMu.Lock()
		defer connectionsMu.Unlock()
		for _, connection := range connections {
			connection.Close()
		}
	})
	return runs
}
func waitTriageRun(t *testing.T, a *App, id string) TriageAgentView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := a.TriageAgentState(context.Background(), Principal{UnixUser: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if view.LatestSetupTest != nil && view.LatestSetupTest.ID == id && view.LatestSetupTest.State != "running" {
			a.triageAgent.mu.Lock()
			active := a.triageAgent.activeRun
			a.triageAgent.mu.Unlock()
			if active == nil {
				return view
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("run did not end")
	return TriageAgentView{}
}
func sendTriageEvent(t *testing.T, run fakeTriageRun, event triageagent.Event) {
	t.Helper()
	if err := json.NewEncoder(run.connection).Encode(event); err != nil {
		t.Fatal(err)
	}
}

// A setup test that never answers ends at its time limit, frees the one run
// for a retry, and closes the run socket so the sandbox stops the agent.
func TestTriageAgentSetupTestTimeLimit(t *testing.T) {
	previous := triageAgentSetupTestTimeLimit
	triageAgentSetupTestTimeLimit = 50 * time.Millisecond
	t.Cleanup(func() { triageAgentSetupTestTimeLimit = previous })
	a := foundationApp(t)
	admin := memberFixture(t, a, "admin@example.test").Principal
	runs := triageRunFixture(t, a)
	id, err := a.StartTriageAgentSetupTest(context.Background(), admin, "codex", "api_key", "key")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runs
	view := waitTriageRun(t, a, id)
	if test := view.LatestSetupTest; test.State != "failed" || test.FailureReason != "The setup test did not finish within 50ms." || view.Setup != nil {
		t.Fatalf("timed-out setup test: %+v", test)
	}
	if _, err := run.connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("the run socket stays open after the time limit")
	}
	if _, err := a.StartTriageAgentSetupTest(context.Background(), admin, "codex", "api_key", "key"); err != nil {
		t.Fatalf("retry after the time limit: %v", err)
	}
}

func TestTriageAgentSetupRunResults(t *testing.T) {
	for _, outcome := range []string{"passed", "failed", "wrong answer", "early close", "stopped"} {
		t.Run(outcome, func(t *testing.T) {
			a := foundationApp(t)
			admin := memberFixture(t, a, "admin@example.test").Principal
			runs := triageRunFixture(t, a)
			const credential = "private-triage-key"
			id, err := a.StartTriageAgentSetupTest(context.Background(), admin, "claude_code", "api_key", credential)
			if err != nil {
				t.Fatal(err)
			}
			run := <-runs
			if run.request.Credential != credential || run.request.Prompt != triageAgentSetupTestPrompt || run.request.Action != "start" {
				t.Fatal("incorrect run request")
			}
			if _, err = a.StartTriageAgentSetupTest(context.Background(), admin, "codex", "api_key", "other-key"); !errors.Is(err, ErrConflict) || publicError(err) != "A triage agent run is already active." {
				t.Fatalf("second start: %v", err)
			}
			var setupCount int
			a.Store.db.QueryRow(`SELECT count(*) FROM triage_agent_setup`).Scan(&setupCount)
			if setupCount != 0 {
				t.Fatal("saved before test passed")
			}
			sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "agent-session"})
			sendTriageEvent(t, run, triageagent.Event{Event: "step", Step: &triageagent.Step{Kind: "command", Text: "printf " + credential}})
			switch outcome {
			case "passed":
				cost := 0.012
				sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence + ". " + credential, CodingAgentSessionTotalUsage: &triageagent.Usage{CostInUSDollars: &cost, InputTokens: 42, CachedInputTokens: 30, OutputTokens: 9}})
			case "failed":
				sendTriageEvent(t, run, triageagent.Event{Event: "failed", Reason: "Agent refused " + credential})
			case "wrong answer":
				sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "another sentence"})
			case "early close":
				run.connection.Close()
			case "stopped":
				if err = a.StopTriageAgentRun(id); err != nil {
					t.Fatal(err)
				}
				var control map[string]bool
				if err = json.NewDecoder(run.connection).Decode(&control); err != nil || len(control) != 1 || !control["stop"] {
					t.Fatal("stop was not delivered", err)
				}
				sendTriageEvent(t, run, triageagent.Event{Event: "stopped", SessionID: "agent-session"})
			}
			view := waitTriageRun(t, a, id)
			run.connection.Close()
			latest := view.LatestSetupTest
			if latest.CodingAgentSessionID != "agent-session" || len(latest.Steps) != 1 || latest.Steps[0].Sequence != 1 || latest.EndedAt == nil {
				t.Fatalf("run details: %+v", latest)
			}
			if outcome == "passed" {
				if view.Setup == nil || view.Setup.SetUpBy != "Responder" || !latest.Passed() || latest.CostInUSDollars == nil || *latest.CostInUSDollars != 0.012 || latest.InputTokens != 42 || latest.CachedInputTokens != 30 || latest.OutputTokens != 9 {
					t.Fatal("passing setup not saved", view)
				}
				var saved string
				a.Store.db.QueryRow(`SELECT credential FROM triage_agent_setup`).Scan(&saved)
				if saved != credential {
					t.Fatal("credential not saved")
				}
			} else {
				if view.Setup != nil || latest.FailureReason == "" || latest.Passed() {
					t.Fatal("failed test saved setup", view)
				}
				if outcome == "early close" && latest.FailureReason != "The run ended without a result." {
					t.Fatal(latest.FailureReason)
				}
				if outcome == "wrong answer" && latest.FailureReason != "The coding agent answered, but not with the test sentence." {
					t.Fatal(latest.FailureReason)
				}
			}
			raw, _ := json.Marshal(view)
			if strings.Contains(string(raw), credential) {
				t.Fatal("credential exposed in state")
			}
		})
	}
}
func TestTriageAgentFailedReplacementKeepsSetup(t *testing.T) {
	a := foundationApp(t)
	runs := triageRunFixture(t, a)
	p := Principal{UnixUser: "administrator"}
	for index := range 2 {
		id, err := a.StartTriageAgentSetupTest(context.Background(), p, "codex", "api_key", []string{"first-secret", "new-secret"}[index])
		if err != nil {
			t.Fatal(err)
		}
		run := <-runs
		event := triageagent.Event{Event: "finished", Answer: triageAgentTestSentence}
		if index == 1 {
			event = triageagent.Event{Event: "failed", Reason: "No account credit."}
		}
		sendTriageEvent(t, run, event)
		waitTriageRun(t, a, id)
		run.connection.Close()
	}
	var credential string
	if err := a.Store.db.QueryRow(`SELECT credential FROM triage_agent_setup`).Scan(&credential); err != nil || credential != "first-secret" {
		t.Fatal("failed replacement changed setup", err)
	}
	// A settings file without the triage_agent section removes the setup.
	if err := a.applyCodingAgentSettings(context.Background(), triageAgentRole, nil); err != nil {
		t.Fatal(err)
	}
	view, _ := a.TriageAgentState(context.Background(), p)
	if view.Setup != nil {
		t.Fatal("setup not removed")
	}
}
func TestTriageAgentSetupValidationAndRecovery(t *testing.T) {
	a := foundationApp(t)
	admin := memberFixture(t, a, "admin@example.test").Principal
	member := memberFixture(t, a, "member@example.test").Principal
	ctx := context.Background()
	for _, input := range [][3]string{{"other", "api_key", "key"}, {"claude_code", "other", "key"}, {"claude_code", "api_key", ""}, {"codex", "subscription_sign_in", ""}} {
		if _, err := a.StartTriageAgentSetupTest(ctx, admin, input[0], input[1], input[2]); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted invalid setup", err)
		}
	}
	for _, operation := range []func() error{
		func() error { return a.SetTriageAgentInternetAccess(ctx, member, "any_site") }, func() error { return addTriageAgentListedHost(ctx, a, member, "host.example", 443) }, func() error { _, err := a.TriageAgentState(ctx, member); return err },
	} {
		if err := operation(); !errors.Is(err, ErrForbidden) {
			t.Fatal("member allowed", err)
		}
	}
	if _, err := a.StartTriageAgentSetupTest(ctx, admin, "codex", "api_key", "key"); !errors.Is(err, ErrInvalid) || publicError(err) != TriageAgentSandboxNotInstalled {
		t.Fatal("sandbox missing", err)
	}
	if err := a.InitializeTriageAgent(ctx, "run.sock", "", "proxy.sock"); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted unpaired socket")
	}
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by) VALUES('stale','setup_test','codex','api_key','running',1,'administrator')`); err != nil {
		t.Fatal(err)
	}
	if err := a.InitializeTriageAgent(ctx, "", "", ""); err != nil {
		t.Fatal(err)
	}
	view, _ := a.TriageAgentState(ctx, admin)
	if view.LatestSetupTest.State != "failed" || view.LatestSetupTest.FailureReason != "MMHotline restarted during the run." {
		t.Fatal("stale row not recovered")
	}
}
func TestTriageAgentInternetAccessAndListedHosts(t *testing.T) {
	a := foundationApp(t)
	p := Principal{UnixUser: "administrator"}
	ctx := context.Background()
	view, _ := a.TriageAgentState(ctx, p)
	if view.InternetAccess != "listed_hosts_only" {
		t.Fatal("wrong default")
	}
	for _, host := range []string{"localhost.invalid", "HOST.Example", "10.0.0.3", "2001:4860:4860::8888"} {
		if err := addTriageAgentListedHost(ctx, a, p, host, 443); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"", "-bad.test", "bad-.test", "bad..test", "a/b.test", "::", "127.0.0.1", "169.254.169.254", "224.0.0.1", "fe80::1", "::ffff:127.0.0.1", strings.Repeat("a", 254)} {
		if err := addTriageAgentListedHost(ctx, a, p, host, 443); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted host", host, err)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if err := addTriageAgentListedHost(ctx, a, p, "example.com", port); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted port", port)
		}
	}
	if err := a.SetTriageAgentInternetAccess(ctx, p, "unknown"); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted setting")
	}
	if err := a.SetTriageAgentInternetAccess(ctx, p, "any_site"); err != nil {
		t.Fatal(err)
	}
	view, _ = a.TriageAgentState(ctx, p)
	found := false
	for _, host := range view.ListedHosts {
		if host.Host == "host.example" {
			found = true
			if host.AddedBy == "" || !strings.Contains(host.ListedBecause, "administrator") {
				t.Fatal("missing reason")
			}
		}
	}
	if !found || view.InternetAccess != "any_site" {
		t.Fatal(view)
	}
	if err := a.SetTriageAgentListedHosts(ctx, p, []SettingsListedHost{{Host: "localhost.invalid", Port: 443}}); err != nil {
		t.Fatal(err)
	}
	view, _ = a.TriageAgentState(ctx, p)
	for _, host := range view.ListedHosts {
		if host.Host == "host.example" {
			t.Fatal("host not removed")
		}
	}
}

// proxyRequest sends request to the logging proxy after the first line a
// run's bridge sends, proxyToken.
func proxyRequest(t *testing.T, a *App, proxyToken, request string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	client, server := net.Pipe()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	go a.serveTriageAgentProxyConnection(server)
	writeDone := make(chan error, 1)
	go func() { _, err := io.WriteString(client, proxyToken+"\n"+request); writeDone <- err }()
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	return client, reader, response
}
func waitProxyEnded(t *testing.T, a *App, count int) TriageAgentView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := a.TriageAgentState(context.Background(), Principal{UnixUser: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if len(view.RecentConnections) == count {
			ended := true
			for _, connection := range view.RecentConnections {
				ended = ended && connection.EndedAt != nil
			}
			if ended {
				return view
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("proxy connection did not end")
	return TriageAgentView{}
}
func TestTriageAgentProxyPoliciesAndAccounting(t *testing.T) {
	for _, test := range []struct {
		name, host, address, access string
		listed                      bool
		status                      int
	}{
		{"listed public", "git.example", "8.8.8.8", "listed_hosts_only", true, 200},
		{"unlisted refused", "other.example", "8.8.8.8", "listed_hosts_only", false, 403},
		{"any public", "other.example", "8.8.8.8", "any_site", false, 200},
		{"any private refused", "other.example", "10.0.0.5", "any_site", false, 403},
		{"listed private", "git.example", "10.0.0.5", "any_site", true, 200},
		{"loopback refused", "git.example", "127.0.0.1", "any_site", true, 403},
		{"metadata refused", "git.example", "169.254.169.254", "listed_hosts_only", true, 403},
		{"service automatically listed", "api.anthropic.com", "8.8.8.8", "listed_hosts_only", false, 200},
		{"OSV.dev automatically listed", "api.osv.dev", "8.8.8.8", "listed_hosts_only", false, 200},
		{"CISA's list of known exploited vulnerabilities automatically listed", "raw.githubusercontent.com", "8.8.8.8", "listed_hosts_only", false, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := foundationApp(t)
			runs := triageRunFixture(t, a)
			ctx := context.Background()
			p := Principal{UnixUser: "administrator"}
			if test.listed {
				if err := addTriageAgentListedHost(ctx, a, p, test.host, 443); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.SetTriageAgentInternetAccess(ctx, p, test.access); err != nil {
				t.Fatal(err)
			}
			a.triageAgent.resolve = func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP(test.address)}}, nil
			}
			a.triageAgent.dial = func(_ context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" || address != net.JoinHostPort(netip.MustParseAddr(test.address).String(), "443") {
					t.Error("unchecked dial address", address)
				}
				client, server := net.Pipe()
				go func() {
					defer server.Close()
					data := make([]byte, 4)
					if _, err := io.ReadFull(server, data); err != nil {
						return
					}
					io.WriteString(server, "pong")
				}()
				return client, nil
			}
			id, err := a.StartTriageAgentSetupTest(ctx, p, "claude_code", "api_key", "secret-key")
			if err != nil {
				t.Fatal(err)
			}
			run := <-runs
			client, reader, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT "+net.JoinHostPort(test.host, "443")+" HTTP/1.1\r\nHost: "+test.host+"\r\n\r\n")
			if response.StatusCode != test.status {
				t.Fatal(response.StatusCode)
			}
			if test.status == 200 {
				if _, err = io.WriteString(client, "ping"); err != nil {
					t.Fatal(err)
				}
				reply := make([]byte, 4)
				if _, err = io.ReadFull(reader, reply); err != nil || string(reply) != "pong" {
					t.Fatal("tunnel did not carry reply", err)
				}
			} else {
				body, _ := io.ReadAll(response.Body)
				if !strings.HasPrefix(string(body), "MMHotline refused the connection: ") {
					t.Fatal(string(body))
				}
			}
			client.Close()
			view := waitProxyEnded(t, a, 1)
			connection := view.RecentConnections[0]
			if connection.Host != test.host || connection.RunID != id {
				t.Fatal(connection)
			}
			if test.status == 200 {
				if connection.Decision != "allowed" || connection.BytesSent != 4 || connection.BytesReceived != 4 || connection.Address != test.address {
					t.Fatal(connection)
				}
			} else if connection.Decision != "refused" || connection.RefusalReason == "" || connection.BytesSent != 0 || connection.BytesReceived != 0 {
				t.Fatal(connection)
			}
			sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
			waitTriageRun(t, a, id)
			run.connection.Close()
		})
	}
}
func TestTriageAgentProxyRefusesHTTPAndNoRun(t *testing.T) {
	a := foundationApp(t)
	for _, test := range []struct {
		request string
		status  int
		reason  string
	}{{"GET / HTTP/1.1\r\n\r\n", 405, "accepts only HTTPS through CONNECT"}, {"CONNECT example.com:443 HTTP/1.1\r\n\r\n", 403, "The connection names no active triage agent or review agent run."}} {
		client, _, response := proxyRequest(t, a, "", test.request)
		if response.StatusCode != test.status {
			t.Fatal(response.StatusCode)
		}
		body, _ := io.ReadAll(response.Body)
		client.Close()
		if !strings.Contains(string(body), test.reason) {
			t.Fatal(string(body))
		}
	}
	view := waitProxyEnded(t, a, 2)
	if view.RecentConnections[0].ID <= view.RecentConnections[1].ID {
		t.Fatal("connections not newest first")
	}
}
func TestTriageAgentRunEndClosesTunnels(t *testing.T) {
	a := foundationApp(t)
	runs := triageRunFixture(t, a)
	p := Principal{UnixUser: "administrator"}
	a.triageAgent.resolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	remoteClosed := make(chan struct{})
	a.triageAgent.dial = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { defer server.Close(); io.Copy(io.Discard, server); close(remoteClosed) }()
		return client, nil
	}
	id, err := a.StartTriageAgentSetupTest(context.Background(), p, "claude_code", "api_key", "secret")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runs
	client, reader, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT api.anthropic.com:443 HTTP/1.1\r\n\r\n")
	defer client.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	waitTriageRun(t, a, id)
	run.connection.Close()
	if _, err = reader.ReadByte(); err == nil {
		t.Fatal("tunnel survived run")
	}
	select {
	case <-remoteClosed:
	case <-time.After(time.Second):
		t.Fatal("upstream survived run")
	}
	waitProxyEnded(t, a, 1)
}

func TestTriageAgentProxyMixedAddressesAndServiceReplacement(t *testing.T) {
	a := foundationApp(t)
	runs := triageRunFixture(t, a)
	p := Principal{UnixUser: "administrator"}
	ctx := context.Background()
	// An existing Claude Code setup must not choose a new Codex test's service hosts.
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_setup(id,coding_agent,credential_type,credential,model,reasoning_effort,set_up_at,set_up_by) VALUES(1,'claude_code','api_key','existing-key','test-model','high',1,'administrator')`); err != nil {
		t.Fatal(err)
	}
	id, err := a.StartTriageAgentSetupTest(ctx, p, "codex", "api_key", "new-key")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runs
	a.triageAgent.resolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.1.2.3")}}, nil
	}
	a.triageAgent.dial = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { io.Copy(io.Discard, server); server.Close() }()
		return client, nil
	}
	for _, host := range []string{"api.openai.com"} {
		client, _, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT "+host+":443 HTTP/1.1\r\n\r\n")
		if response.StatusCode != 200 {
			t.Fatal("new coding agent's service refused", host, response.StatusCode)
		}
		client.Close()
	}
	client, _, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT api.anthropic.com:443 HTTP/1.1\r\n\r\n")
	io.ReadAll(response.Body)
	client.Close()
	if response.StatusCode != 403 {
		t.Fatal("old service still listed")
	}
	if err = a.SetTriageAgentInternetAccess(ctx, p, "any_site"); err != nil {
		t.Fatal(err)
	}
	client, _, response = proxyRequest(t, a, run.request.ProxyToken, "CONNECT mixed.example:443 HTTP/1.1\r\n\r\n")
	body, _ := io.ReadAll(response.Body)
	client.Close()
	if response.StatusCode != 403 || !strings.Contains(string(body), "only to public internet addresses") {
		t.Fatal("mixed DNS answer allowed", string(body))
	}
	sendTriageEvent(t, run, triageagent.Event{Event: "failed", Reason: "test not saved"})
	waitTriageRun(t, a, id)
	run.connection.Close()
	waitProxyEnded(t, a, 3)
}
func TestTriageAgentProxyTunnelLimitAndLatestFifty(t *testing.T) {
	a := foundationApp(t)
	runs := triageRunFixture(t, a)
	p := Principal{UnixUser: "administrator"}
	a.triageAgent.resolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	a.triageAgent.dial = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { defer server.Close(); io.Copy(io.Discard, server) }()
		return client, nil
	}
	id, err := a.StartTriageAgentSetupTest(context.Background(), p, "codex", "api_key", "credential")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runs
	clients := []net.Conn{}
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()
	for range 64 {
		client, _, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT api.openai.com:443 HTTP/1.1\r\n\r\n")
		clients = append(clients, client)
		if response.StatusCode != 200 {
			t.Fatal("tunnel refused before limit", response.StatusCode)
		}
	}
	client, _, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT api.openai.com:443 HTTP/1.1\r\n\r\n")
	body, _ := io.ReadAll(response.Body)
	client.Close()
	if response.StatusCode != 503 || !strings.Contains(string(body), "64 open tunnels") {
		t.Fatal("limit not enforced", string(body))
	}
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	view := waitTriageRun(t, a, id)
	run.connection.Close()
	var total, ended int
	if err = a.Store.db.QueryRow(`SELECT count(*),sum(ended_at>0) FROM triage_agent_proxy_connections`).Scan(&total, &ended); err != nil || total != 65 || ended != 65 {
		t.Fatalf("connection accounting: total=%d ended=%d err=%v", total, ended, err)
	}
	if len(view.RecentConnections) != 50 || view.RecentConnections[0].Decision != "refused" || view.RecentConnections[0].Host != "api.openai.com" || view.RecentConnections[0].Port != 443 {
		t.Fatal("latest 50 missing")
	}
	for index := 1; index < len(view.RecentConnections); index++ {
		if view.RecentConnections[index-1].ID <= view.RecentConnections[index].ID {
			t.Fatal("connections out of order")
		}
	}
}
func TestTriageAgentProxyRequestBound(t *testing.T) {
	a := foundationApp(t)
	client, server := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	go a.serveTriageAgentProxyConnection(server)
	go io.WriteString(client, "\nCONNECT example.com:443 HTTP/1.1\r\nHeader: "+strings.Repeat("a", 8192)+"\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 403 || !strings.Contains(string(body), "at most 8 KiB") {
		t.Fatal("oversized headers accepted", string(body))
	}
	client.Close()
	view := waitProxyEnded(t, a, 1)
	if view.RecentConnections[0].Decision != "refused" {
		t.Fatal("oversized attempt not recorded")
	}
}

func TestTriageAgentRunEndClosesPendingProxyRequests(t *testing.T) {
	a := foundationApp(t)
	runs := triageRunFixture(t, a)
	p := Principal{UnixUser: "administrator"}
	id, err := a.StartTriageAgentSetupTest(context.Background(), p, "claude_code", "api_key", "credential")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runs
	client, server := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	go a.serveTriageAgentProxyConnection(server)
	// Deliver only a partial header. Ending the run must close this socket too.
	if _, err = io.WriteString(client, run.request.ProxyToken+"\nCONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: "); err != nil {
		t.Fatal(err)
	}
	// The write returns once the proxy has read the token, which can be before
	// the run holds the connection. A connection whose run is already ending
	// is refused after its request instead, so wait for the run to hold it.
	waitForCondition(t, "the run to hold the proxy connection", func() bool {
		a.triageAgent.mu.Lock()
		defer a.triageAgent.mu.Unlock()
		active := a.triageAgent.activeRun
		return active != nil && len(active.connections) == 1
	})
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	waitTriageRun(t, a, id)
	run.connection.Close()
	if _, err = client.Write([]byte("api.anthropic.com\r\n\r\n")); err == nil {
		t.Fatal("pending request survived its run")
	}
	waitProxyEnded(t, a, 1)
}

func testGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	// An inherited GIT_DIR or GIT_INDEX_FILE must not reach another repository.
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			command.Env = append(command.Env, variable)
		}
	}
	// Barbara, whom the team trusts, makes and signs the fixtures' commits;
	// a repository's own settings, such as another signing key, win.
	command.Env = append(command.Env, "GIT_CONFIG_GLOBAL="+testSigning(t).trustedGitConfig)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

// A run in the systems repository starts the coding agent in its copy, which
// starts at MMHotline's main branch; the playbook and note the agent commits
// reach origin, and the run records what happened to them.
func TestTriageAgentRunPushesTheAgentsCommits(t *testing.T) {
	a := foundationApp(t)
	run := startSystemsRepositoryRun(t, a, `set -e
/bin/mkdir -p one_time_playbooks triage_agent_notes
printf -- '- hosts: app002\n' > one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml
printf '# Disk full on app002\n' > triage_agent_notes/2026-11-03_disk_full_on_app002.md
git add --all
git commit --quiet -m 'Free disk space on app002'
`+codexFinishes)
	run.wait(t)
	var state, outcome, pushed string
	if err := a.Store.db.QueryRow(`SELECT state FROM triage_agent_runs WHERE id=?`, run.id).Scan(&state); err != nil || state != "finished" {
		t.Fatalf("run state %q: %v", state, err)
	}
	if err := a.Store.db.QueryRow(`SELECT outcome,pushed_commit_id FROM triage_agent_commits WHERE run_id=? AND request_id=''`, run.id).Scan(&outcome, &pushed); err != nil || outcome != "pushed" {
		t.Fatalf("agent's commits %q: %v", outcome, err)
	}
	if main := testGit(t, run.origin, "rev-parse", "main"); main != pushed {
		t.Fatalf("origin's main %s, want %s", main, pushed)
	}
	for _, path := range []string{"one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml", "triage_agent_notes/2026-11-03_disk_full_on_app002.md"} {
		testGit(t, run.origin, "cat-file", "-e", "main:"+path)
	}
}
