package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What mmx nodes prints for the two inventories, as MMX's cmd/mmx/nodes.go
// writes it; util01 serves both.
const (
	productionNodes        = `{"schema_version":1,"inventory":"production","inventory_file":"inventories/production.yml","nodes":[{"name":"app001","address":"203.0.113.11","ssh_user":"root","groups":["app_servers"]},{"name":"util01","address":"203.0.113.1","ssh_user":"root","groups":["util_server"]}],"groups":{"app_servers":["app001"],"util_server":["util01"]}}`
	stagingNodes           = `{"schema_version":1,"inventory":"staging","inventory_file":"inventories/staging.yml","nodes":[{"name":"staging01","address":"203.0.113.2","ssh_user":"root","groups":["staging_server"]},{"name":"util01","address":"203.0.113.1","ssh_user":"root","groups":["util_server"]}],"groups":{"staging_server":["staging01"],"util_server":["util01"]}}`
	brokenInventoryMessage = "mmx: inventories/staging.yml:4:5: unknown field adress"
)

// systemsRepositoryWithInventories is a systems repository with the
// inventories production and staging.
var systemsRepositoryWithInventories = map[string]string{
	"inventories/production.yml": "nodes:\n  app001:\n    address: 203.0.113.11\n  util01:\n    address: 203.0.113.1\n",
	"inventories/staging.yml":    "nodes:\n  staging01:\n    address: 203.0.113.2\n  util01:\n    address: 203.0.113.1\n",
}

// installFakeMMXNodes puts a stand-in for mmx on the App's search path. It
// prints the recorded mmx nodes output of each inventory in listed and fails
// as MMX fails for any other.
func installFakeMMXNodes(t *testing.T, a *App, listed map[string]string) {
	t.Helper()
	directory := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n"
	for inventory, output := range listed {
		file := filepath.Join(directory, inventory+".json")
		if err := os.WriteFile(file, []byte(output), 0o644); err != nil {
			t.Fatal(err)
		}
		script += "'nodes --inventory " + inventory + " --json') cat '" + file + "' ;;\n"
	}
	script += "*) echo '" + brokenInventoryMessage + "' >&2; exit 4 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(directory, "mmx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a.triageAgent.mmxSearchPath = directory
}

// listedServers is each listed host at port 22 with why it is listed.
func listedServers(t *testing.T, a *App) (map[string]string, TriageAgentView) {
	t.Helper()
	view, err := a.TriageAgentState(context.Background(), Principal{UnixUser: "administrator"})
	if err != nil {
		t.Fatal(err)
	}
	servers := map[string]string{}
	for _, host := range view.ListedHosts {
		if host.Port == 22 {
			servers[net.JoinHostPort(host.Host, strconv.Itoa(host.Port))] = host.ListedBecause
		}
	}
	return servers, view
}

// A commit review run lists the servers and vendor pages' hosts too, so the
// page and show describe its listing when it is the latest.
func TestTheLatestCommitReviewRunsListingIsShown(t *testing.T) {
	a := foundationApp(t)
	for _, run := range []struct {
		id, purpose, notListedBecause string
		startedAt                     int64
	}{
		{"00000000000000000000000000000001", "triage_session", "", 100},
		{"00000000000000000000000000000002", "commit_review", "the file is not valid YAML", 200},
		{"00000000000000000000000000000003", "setup_test", "", 300},
	} {
		if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by,vendor_page_hosts_not_listed_because) VALUES(?,?,'codex','api_key','finished',?,'MMHotline',?)`, run.id, run.purpose, run.startedAt, run.notListedBecause); err != nil {
			t.Fatal(err)
		}
	}
	_, view := listedServers(t, a)
	if view.HostsFromTheSystemsRepositoryListedAt == nil || view.HostsFromTheSystemsRepositoryListedAt.Unix() != 200 || view.VendorPageHostsNotListedBecause != "the file is not valid YAML" {
		t.Fatalf("listed at %v, not listed because %q; want the commit review run's", view.HostsFromTheSystemsRepositoryListedAt, view.VendorPageHostsNotListedBecause)
	}
}

func runRecord(t *testing.T, a *App, runID string) (state, notListedBecause string) {
	t.Helper()
	if err := a.Store.db.QueryRow(`SELECT state,inventory_servers_not_listed_because FROM triage_agent_runs WHERE id=?`, runID).Scan(&state, &notListedBecause); err != nil {
		t.Fatal(err)
	}
	return state, notListedBecause
}

// At a run's start MMHotline lists each server of each inventory at port 22,
// with the inventories it is in, in place of the servers an earlier run
// listed. The agent's SSH settings get each server's address as its host key
// alias. A person cannot remove an inventory server.
func TestTriageAgentRunListsTheInventoryServers(t *testing.T) {
	a := foundationApp(t)
	installFakeMMXNodes(t, a, map[string]string{"production": productionNodes, "staging": stagingNodes})
	// A server an earlier run listed, which the inventories no longer have.
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_inventory_servers(server_name,inventory) VALUES('app009','production')`); err != nil {
		t.Fatal(err)
	}
	run := prepareSystemsRepositoryRunWithFiles(t, a, systemsRepositoryWithInventories, `/bin/cp "$HOME/.ssh/config" "$HOME/ssh-settings-at-start"
`+codexFinishes)
	run.start(t)
	run.wait(t)
	servers, view := listedServers(t, a)
	want := map[string]string{
		"app001:22":    "A server in the inventory “production”",
		"staging01:22": "A server in the inventory “staging”",
		"util01:22":    "A server in the inventories “production” and “staging”",
	}
	if !reflect.DeepEqual(servers, want) || view.InventoryServersNotListedBecause != "" {
		t.Fatalf("listed servers %v, not listed because %q", servers, view.InventoryServersNotListedBecause)
	}
	// The page and show say when the servers were listed: at the run's start.
	var started int64
	if err := a.Store.db.QueryRow(`SELECT started_at FROM triage_agent_runs WHERE id=?`, run.id).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if view.HostsFromTheSystemsRepositoryListedAt == nil || view.HostsFromTheSystemsRepositoryListedAt.Unix() != started {
		t.Fatalf("listed at %v, want the run's start %d", view.HostsFromTheSystemsRepositoryListedAt, started)
	}
	if text := (CommandResult{TriageAgent: &view}).Text(); !strings.Contains(text, "Inventory servers and vendor pages' hosts listed at the start of the latest run: "+displayTime(time.Unix(started, 0))) {
		t.Fatalf("show:\n%s", text)
	}
	if state, notListedBecause := runRecord(t, a, run.id); state != "finished" || notListedBecause != "" {
		t.Fatalf("run %s, not listed because %q", state, notListedBecause)
	}
	settings := readTestFile(t, filepath.Join(run.agentHome, "ssh-settings-at-start"))
	for _, block := range []string{"Host app001\n  HostKeyAlias 203.0.113.11\n", "Host staging01\n  HostKeyAlias 203.0.113.2\n", "Host util01\n  HostKeyAlias 203.0.113.1\n"} {
		if strings.Count(settings, block) != 1 {
			t.Errorf("the agent's SSH settings have %q %d times:\n%s", block, strings.Count(settings, block), settings)
		}
	}
	// An inventory server follows the inventory, not the settings file's
	// listed hosts.
	if err := a.SetTriageAgentListedHosts(context.Background(), Principal{UnixUser: "administrator"}, nil); err != nil {
		t.Fatal(err)
	}
	if servers, _ := listedServers(t, a); servers["util01:22"] == "" {
		t.Fatal("util01:22 is no longer listed")
	}
}

// When mmx nodes fails for an inventory, or MMHotline finds no mmx, the run
// goes on without those servers, and the run's record and the page say why.
func TestTriageAgentRunGoesOnWithoutServersMMXCannotList(t *testing.T) {
	for _, test := range []struct {
		name             string
		mmx              map[string]string // nil: no mmx program
		want             map[string]string
		notListedBecause string
	}{
		{"one inventory's mmx nodes fails", map[string]string{"production": productionNodes},
			map[string]string{"app001:22": "A server in the inventory “production”", "util01:22": "A server in the inventory “production”"},
			"inventory staging: mmx nodes failed: " + brokenInventoryMessage},
		{"no mmx program", nil, map[string]string{}, "MMHotline finds no mmx program in "},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := foundationApp(t)
			if test.mmx != nil {
				installFakeMMXNodes(t, a, test.mmx)
			} else {
				a.triageAgent.mmxSearchPath = t.TempDir()
				test.notListedBecause += a.triageAgent.mmxSearchPath
			}
			run := prepareSystemsRepositoryRunWithFiles(t, a, systemsRepositoryWithInventories, codexFinishes)
			run.start(t)
			run.wait(t)
			if state, notListedBecause := runRecord(t, a, run.id); state != "finished" || notListedBecause != test.notListedBecause {
				t.Fatalf("run %s, not listed because %q", state, notListedBecause)
			}
			servers, view := listedServers(t, a)
			if !reflect.DeepEqual(servers, test.want) || view.InventoryServersNotListedBecause != test.notListedBecause {
				t.Fatalf("listed servers %v, the page says %q", servers, view.InventoryServersNotListedBecause)
			}
			if !strings.Contains(view.Text(), "At the start of the latest run, MMHotline could not list every inventory server: "+test.notListedBecause) {
				t.Fatalf("the command's text leaves out why:\n%s", view.Text())
			}
		})
	}
}

// The agent's ssh reaches an inventory server by its name: its settings run
// mmhotline triage-agent ssh-proxy, which asks MMHotline's logging proxy
// through the run's bridge; the proxy resolves the name to the server's
// private address and lets the connection through, both ways. A name the
// inventories do not have is refused, and ssh shows the proxy's reason.
func TestTriageAgentReadsAServerOverSSHThroughTheLoggingProxy(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("this test connects with OpenSSH's ssh, which is not installed")
	}
	a := foundationApp(t)
	installFakeMMXNodes(t, a, map[string]string{"production": productionNodes, "staging": stagingNodes})
	// The server: it sends an SSH banner, reads the client's, and closes.
	const serverBanner = "SSH-2.0-MMHotlineTestServer\r\n"
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	clientBanners := make(chan string, 4)
	go func() {
		for {
			connection, err := server.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				io.WriteString(connection, serverBanner)
				banner, _ := bufio.NewReader(connection).ReadString('\n')
				clientBanners <- banner
			}()
		}
	}()
	// util01's /etc/hosts gives app001's private address; the test server
	// stands in for it.
	a.triageAgent.resolve = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "app001" {
			return []net.IPAddr{{IP: net.ParseIP("10.20.1.1")}}, nil
		}
		return nil, errors.New("no such host")
	}
	a.triageAgent.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "10.20.1.1:22" {
			return nil, fmt.Errorf("dialed %s %s, not the address the proxy checked", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Addr().String())
	}
	run := prepareSystemsRepositoryRunWithFiles(t, a, systemsRepositoryWithInventories, `
'`+ssh+`' -F "$HOME/.ssh/config" -v app001 true </dev/null 2> "$HOME/app001-errors"; echo $? > "$HOME/app001-exit"
'`+ssh+`' -F "$HOME/.ssh/config" app009 true </dev/null 2> "$HOME/app009-errors"; echo $? > "$HOME/app009-exit"
`+codexFinishes)
	run.start(t)
	run.wait(t)

	select {
	case banner := <-clientBanners:
		if !strings.HasPrefix(banner, "SSH-2.0-") {
			t.Fatalf("the server got %q from ssh", banner)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ssh never reached the server")
	}
	app001Errors := readTestFile(t, filepath.Join(run.agentHome, "app001-errors"))
	if !strings.Contains(app001Errors, "remote software version MMHotlineTestServer") {
		t.Fatalf("ssh did not get the server's banner:\n%s", app001Errors)
	}
	// The test server is no SSH server, so ssh fails after the banners.
	if exit := strings.TrimSpace(readTestFile(t, filepath.Join(run.agentHome, "app001-exit"))); exit != "255" {
		t.Fatalf("ssh app001 exited %s", exit)
	}
	app009Errors := readTestFile(t, filepath.Join(run.agentHome, "app009-errors"))
	if !strings.Contains(app009Errors, "MMHotline refused the connection: app009:22 is not a listed host; an administrator can add it to triage_agent.listed_hosts in MMHotline's settings file.") {
		t.Fatalf("ssh app009 did not show the proxy's reason:\n%s", app009Errors)
	}
	if exit := strings.TrimSpace(readTestFile(t, filepath.Join(run.agentHome, "app009-exit"))); exit != "255" {
		t.Fatalf("ssh app009 exited %s", exit)
	}
	_, view := listedServers(t, a)
	connections := map[string]TriageAgentProxyConnection{}
	for _, connection := range view.RecentConnections {
		if connection.RunID == run.id {
			connections[net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port))] = connection
		}
	}
	if app001 := connections["app001:22"]; app001.Decision != "allowed" || app001.Address != "10.20.1.1" || app001.BytesSent == 0 || app001.BytesReceived != int64(len(serverBanner)) || app001.EndedAt == nil {
		t.Fatalf("app001:22 through the proxy: %+v", app001)
	}
	if app009 := connections["app009:22"]; app009.Decision != "refused" || len(connections) != 2 {
		t.Fatalf("connections of the run: %+v", connections)
	}
}
