package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"mmhotline/internal/triageagent"
)

// The systems repository's list of programs from outside the distribution:
// caddy's two pages and cloudflared's releases page on github.com, and
// cloudflared's advisories on a host of their own at port 8443. MMHotline
// reads only the two page keys.
const softwareOutsideTheDistribution = `# Every program on the servers that doesn't come from the distribution.
software_outside_the_distribution:
  caddy:
    installed_on_server_groups: [app_servers]
    installed_from: vendor_apt_repository
    installed_version_command: caddy version
    releases_page: https://github.com/caddyserver/caddy/releases
    security_advisories_page: https://github.com/caddyserver/caddy/security/advisories
  cloudflared:
    installed_on_server_groups: [app_servers, util_server]
    installed_from: vendor_apt_repository
    installed_version_command: cloudflared --version
    releases_page: https://github.com/cloudflare/cloudflared/releases
    security_advisories_page: https://advisories.example.com:8443/cloudflared
`

// The reasons of the example's two hosts.
const (
	githubPagesReason     = "The releases page of caddy and of cloudflared, and the security advisories page of caddy, from software_outside_the_distribution.yml"
	advisoriesPagesReason = "The security advisories page of cloudflared, from software_outside_the_distribution.yml"
)

// pushToOrigin commits files to origin from a clone of its own, as a person
// pushes; MMHotline's copy fetches the commit at the next run's start.
func pushToOrigin(t *testing.T, a *App, files map[string]string) {
	t.Helper()
	origin := testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "remote", "get-url", "origin")
	clone := filepath.Join(t.TempDir(), "clone")
	testGit(t, filepath.Dir(clone), "clone", "--quiet", origin, clone)
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(clone, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(clone, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, clone, "add", "--all")
	testGit(t, clone, "commit", "--quiet", "-m", "Change the programs")
	testGit(t, clone, "push", "--quiet", "origin", "main")
}

// startTriageSessionRun starts a triage session's run in the systems
// repository and returns it once it reached the sandbox.
func (r *triageSessionRig) startTriageSessionRun(t *testing.T) (string, fakeTriageRun) {
	t.Helper()
	id, err := r.a.startTriageAgentRunInSystemsRepository(context.Background(), "triage_session", "MMHotline", triageagent.Request{Action: "start", Prompt: "The disk of app002 is full."}, func(*sql.Tx, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return id, r.nextRun(t)
}

// reachEveryHostThroughTheProxy makes the proxy resolve every host to one
// address and connect to a stand-in that reads and closes.
func reachEveryHostThroughTheProxy(a *App) {
	a.triageAgent.resolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.80")}}, nil
	}
	a.triageAgent.dial = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { defer server.Close(); io.Copy(io.Discard, server) }()
		return client, nil
	}
}

// proxyAnswer is the proxy's status and body for a CONNECT to target from
// run.
func proxyAnswer(t *testing.T, a *App, run fakeTriageRun, target string) (int, string) {
	t.Helper()
	client, _, response := proxyRequest(t, a, run.request.ProxyToken, "CONNECT "+target+" HTTP/1.1\r\n\r\n")
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	client.Close()
	return response.StatusCode, string(body)
}

// vendorPageHostsListed is each listed host from
// software_outside_the_distribution.yml, by host and port, with why it is
// listed; it fails when any host and port is listed twice.
func vendorPageHostsListed(t *testing.T, a *App) (map[string]string, TriageAgentView) {
	t.Helper()
	view, err := a.TriageAgentState(context.Background(), Principal{UnixUser: "administrator"})
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]string{}
	seen := map[string]bool{}
	for _, host := range view.ListedHosts {
		target := net.JoinHostPort(host.Host, strconv.Itoa(host.Port))
		if seen[target] {
			t.Fatalf("%s is listed twice: %+v", target, view.ListedHosts)
		}
		seen[target] = true
		if strings.HasSuffix(host.ListedBecause, "from software_outside_the_distribution.yml") {
			hosts[target] = host.ListedBecause
		}
	}
	return hosts, view
}

// triageAgentPageText is the Triage agent page as the rig's administrator
// sees it.
func triageAgentPageText(t *testing.T, r *triageSessionRig) string {
	t.Helper()
	request := httptest.NewRequest("GET", "https://mmhotline.example.com/triage-agent", nil)
	addAccessToken(request, r.p.SignInEmailAddress)
	response := httptest.NewRecorder()
	r.a.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("triage agent page: %d", response.Code)
	}
	return html.UnescapeString(response.Body.String())
}

// At a run's start in the systems repository, MMHotline lists the host of
// each vendor page in software_outside_the_distribution.yml once, at port
// 443 or the port its address names, with the pages it holds; the run
// reaches them through the logging proxy with only listed hosts allowed.
// triage-agent show and the page list them, and the settings file's listed
// hosts neither add nor remove them.
func TestTriageAgentRunListsTheVendorPagesHosts(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	ctx := context.Background()
	administrator := Principal{UnixUser: "administrator"}
	if err := r.a.SetTriageAgentInternetAccess(ctx, administrator, "listed_hosts_only"); err != nil {
		t.Fatal(err)
	}
	pushToOrigin(t, r.a, map[string]string{"software_outside_the_distribution.yml": softwareOutsideTheDistribution})
	reachEveryHostThroughTheProxy(r.a)
	runID, run := r.startTriageSessionRun(t)

	want := map[string]string{"github.com:443": githubPagesReason, "advisories.example.com:8443": advisoriesPagesReason}
	hosts, view := vendorPageHostsListed(t, r.a)
	if !reflect.DeepEqual(hosts, want) || view.VendorPageHostsNotListedBecause != "" {
		t.Fatalf("vendor page hosts %v, not listed because %q", hosts, view.VendorPageHostsNotListedBecause)
	}
	for _, target := range []string{"github.com:443", "advisories.example.com:8443"} {
		if status, body := proxyAnswer(t, r.a, run, target); status != 200 {
			t.Fatalf("%s: %d %s", target, status, body)
		}
	}
	// Only the pages' hosts and ports: another host or port stays refused.
	for _, target := range []string{"api.github.com:443", "github.com:22", "advisories.example.com:443"} {
		if status, body := proxyAnswer(t, r.a, run, target); status != 403 || !strings.Contains(body, target+" is not a listed host") {
			t.Fatalf("%s: %d %s", target, status, body)
		}
	}

	result, raw := commandRequest(t, r.a, CommandRequest{Group: "triage-agent", Action: "show"}, 200)
	for target, reason := range want {
		if line := "Listed host: " + target + "; listed because: " + reason + "."; !strings.Contains(result.Text(), line) {
			t.Errorf("show lacks %q:\n%s", line, result.Text())
		}
	}
	var printed struct {
		TriageAgent struct {
			ListedHosts []struct {
				Host          string `json:"host"`
				Port          int    `json:"port"`
				ListedBecause string `json:"listed_because"`
				AddedBy       string `json:"added_by"`
			} `json:"listed_hosts"`
			ListedAt string `json:"hosts_from_the_systems_repository_listed_at"`
		} `json:"triage_agent"`
	}
	if err := json.Unmarshal([]byte(raw), &printed); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, host := range printed.TriageAgent.ListedHosts {
		if reason, listed := want[net.JoinHostPort(host.Host, strconv.Itoa(host.Port))]; listed && host.ListedBecause == reason && host.AddedBy == "" {
			found++
		}
	}
	if found != 2 || printed.TriageAgent.ListedAt == "" {
		t.Fatalf("show --json lists %d of the 2 hosts, listed at %q: %s", found, printed.TriageAgent.ListedAt, raw)
	}

	page := triageAgentPageText(t, r)
	for _, row := range []string{"<td>github.com</td><td>443</td><td>" + githubPagesReason + "</td>", "<td>advisories.example.com</td><td>8443</td><td>" + advisoriesPagesReason + "</td>"} {
		if !strings.Contains(page, row) {
			t.Errorf("the page lacks %q", row)
		}
	}

	// The settings file's listed hosts neither add nor remove them.
	if err := r.a.SetTriageAgentListedHosts(ctx, administrator, nil); err != nil {
		t.Fatal(err)
	}
	if hosts, _ := vendorPageHostsListed(t, r.a); !reflect.DeepEqual(hosts, want) {
		t.Fatalf("after the listed hosts were emptied: %v", hosts)
	}
	if err := r.a.SetTriageAgentListedHosts(ctx, administrator, []SettingsListedHost{{Host: "github.com", Port: 443}}); err != nil {
		t.Fatal(err)
	}
	if hosts, _ := vendorPageHostsListed(t, r.a); !reflect.DeepEqual(hosts, want) {
		t.Fatalf("with github.com in the listed hosts too: %v", hosts)
	}
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Done."})
	var notListedBecause string
	if err := r.a.Store.db.QueryRow(`SELECT vendor_page_hosts_not_listed_because FROM triage_agent_runs WHERE id=?`, runID).Scan(&notListedBecause); err != nil || notListedBecause != "" {
		t.Fatalf("the run's record: %q, %v", notListedBecause, err)
	}
}

// The listing follows the file at the commit each run starts from: a
// program a person pushed is listed at the next run, and a page moved to
// another host takes its old host off the list.
func TestTheNextRunListsTheVendorPagesOfANewCommit(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	pushToOrigin(t, r.a, map[string]string{"software_outside_the_distribution.yml": softwareOutsideTheDistribution})
	_, run := r.startTriageSessionRun(t)
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Done."})

	pushToOrigin(t, r.a, map[string]string{"software_outside_the_distribution.yml": strings.Replace(softwareOutsideTheDistribution,
		"https://advisories.example.com:8443/cloudflared", "https://github.com/cloudflare/cloudflared/security/advisories", 1) + `  rclone:
    installed_on_server_groups: [util_server]
    installed_from: downloaded_binary
    installed_version_command: rclone version
    releases_page: https://github.com/rclone/rclone/releases
    security_advisories_page: https://github.com/rclone/rclone/security/advisories
`})
	// Until a run starts, the earlier listing stays.
	if hosts, _ := vendorPageHostsListed(t, r.a); hosts["advisories.example.com:8443"] == "" {
		t.Fatalf("the listing changed before a run: %v", hosts)
	}
	_, run = r.startTriageSessionRun(t)
	hosts, _ := vendorPageHostsListed(t, r.a)
	want := map[string]string{"github.com:443": "The releases page of caddy, of cloudflared, and of rclone, and the security advisories page of caddy, of cloudflared, and of rclone, from software_outside_the_distribution.yml"}
	if !reflect.DeepEqual(hosts, want) {
		t.Fatalf("after the new commit: %v", hosts)
	}
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Done."})
}

// A page whose address is not https:// with a host, and a file MMHotline
// cannot read, are left out: the run starts all the same, and its record,
// triage-agent show, and the page say why. A missing file lists nothing and
// is no problem.
func TestVendorPagesMMHotlineCannotListAreLeftOut(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name             string
		file             string // "": no file
		want             map[string]string
		notListedBecause string
	}{
		{"an http address", strings.Replace(softwareOutsideTheDistribution, "releases_page: https://github.com/caddyserver", "releases_page: http://github.com/caddyserver", 1),
			map[string]string{"github.com:443": "The releases page of cloudflared, and the security advisories page of caddy, from software_outside_the_distribution.yml", "advisories.example.com:8443": advisoriesPagesReason},
			"the releases_page of caddy, http://github.com/caddyserver/caddy/releases, is not an https:// address with a host"},
		{"an address without a host", strings.Replace(softwareOutsideTheDistribution, "https://advisories.example.com:8443/cloudflared", "advisories.example.com/cloudflared", 1),
			map[string]string{"github.com:443": githubPagesReason},
			"the security_advisories_page of cloudflared, advisories.example.com/cloudflared, is not an https:// address with a host"},
		{"a file that is not YAML", "software_outside_the_distribution:\n  caddy: [releases_page\n", map[string]string{},
			"MMHotline cannot read software_outside_the_distribution.yml: yaml: "},
		{"no file", "", map[string]string{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			if test.file != "" {
				pushToOrigin(t, r.a, map[string]string{"software_outside_the_distribution.yml": test.file})
			}
			// The run reaches the sandbox.
			runID, run := r.startTriageSessionRun(t)
			hosts, view := vendorPageHostsListed(t, r.a)
			if !reflect.DeepEqual(hosts, test.want) || !strings.HasPrefix(view.VendorPageHostsNotListedBecause, test.notListedBecause) || (test.notListedBecause == "") != (view.VendorPageHostsNotListedBecause == "") {
				t.Fatalf("vendor page hosts %v, not listed because %q", hosts, view.VendorPageHostsNotListedBecause)
			}
			var recorded string
			if err := r.a.Store.db.QueryRow(`SELECT vendor_page_hosts_not_listed_because FROM triage_agent_runs WHERE id=?`, runID).Scan(&recorded); err != nil || recorded != view.VendorPageHostsNotListedBecause {
				t.Fatalf("the run's record says %q, %v", recorded, err)
			}
			line := "At the start of the latest run, MMHotline could not list the host of every vendor page in software_outside_the_distribution.yml: " + view.VendorPageHostsNotListedBecause
			text, page := view.Text(), triageAgentPageText(t, r)
			if test.notListedBecause == "" {
				if strings.Contains(text, "could not list the host") || strings.Contains(page, "could not list the host") {
					t.Fatalf("a reason is shown for no problem:\n%s", text)
				}
			} else if !strings.Contains(text, line) || !strings.Contains(page, strings.Replace(line, "software_outside_the_distribution.yml", "<code>software_outside_the_distribution.yml</code>", 1)) {
				t.Fatalf("show or the page lacks %q:\n%s", line, text)
			}
			r.end(t, run, triageagent.Event{Event: "finished", Answer: "Done."})
		})
	}
}

// Only a run in the systems repository reaches the hosts listed from it: a
// review run reaches only its own coding agent's service, and a setup test
// has no systems repository, so it reaches neither the vendor pages' hosts
// nor the inventory servers. Both keep the latest listing.
func TestOnlyRunsInTheSystemsRepositoryReachItsHosts(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	ctx := context.Background()
	administrator := Principal{UnixUser: "administrator"}
	if err := r.a.SetTriageAgentInternetAccess(ctx, administrator, "listed_hosts_only"); err != nil {
		t.Fatal(err)
	}
	installFakeMMXNodes(t, r.a, map[string]string{"production": productionNodes})
	pushToOrigin(t, r.a, map[string]string{"software_outside_the_distribution.yml": softwareOutsideTheDistribution, "inventories/production.yml": systemsRepositoryWithInventories["inventories/production.yml"]})
	reachEveryHostThroughTheProxy(r.a)
	_, triage := r.startTriageSessionRun(t)
	review, err := r.a.startTriageAgentRun(ctx, "playbook_review", "MMHotline", triageagent.Request{Action: "start", CodingAgent: "codex", CredentialType: "api_key", Credential: "review-key", Prompt: "Review this."}, nil, func(*sql.Tx, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	reviewRun := r.nextRun(t)
	for _, test := range []struct {
		run            fakeTriageRun
		target         string
		status         int
		refusedBecause string
	}{
		{triage, "github.com:443", 200, ""},
		{triage, "app001:22", 200, ""},
		{reviewRun, "github.com:443", 403, "is not the review agent's coding agent service"},
		{reviewRun, "app001:22", 403, "is not the review agent's coding agent service"},
	} {
		if status, body := proxyAnswer(t, r.a, test.run, test.target); status != test.status || !strings.Contains(body, test.refusedBecause) {
			t.Fatalf("%s from the %s run: %d %s", test.target, test.run.request.AgentRole, status, body)
		}
	}
	sendTriageEvent(t, reviewRun, triageagent.Event{Event: "finished", Answer: "APPROVE\nFine."})
	<-review.done
	r.end(t, triage, triageagent.Event{Event: "finished", Answer: "Done."})

	setupTestID, err := r.a.StartTriageAgentSetupTest(ctx, administrator, "codex", "api_key", "key")
	if err != nil {
		t.Fatal(err)
	}
	setupTest := r.nextRun(t)
	for _, target := range []string{"github.com:443", "app001:22"} {
		if status, body := proxyAnswer(t, r.a, setupTest, target); status != 403 || !strings.Contains(body, target+" is not a listed host") {
			t.Fatalf("%s from the setup test: %d %s", target, status, body)
		}
	}
	sendTriageEvent(t, setupTest, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence})
	waitTriageRun(t, r.a, setupTestID)
	hosts, view := vendorPageHostsListed(t, r.a)
	if !reflect.DeepEqual(hosts, map[string]string{"github.com:443": githubPagesReason, "advisories.example.com:8443": advisoriesPagesReason}) {
		t.Fatalf("after the setup test: %v", hosts)
	}
	servers, _ := listedServers(t, r.a)
	if servers["app001:22"] == "" || view.HostsFromTheSystemsRepositoryListedAt == nil {
		t.Fatalf("after the setup test: servers %v", servers)
	}
}

// settings show prints the hosts listed from the vendor pages under
// triage_agent.listed_hosts, as of the latest run. They are not part of the
// file's value: its fingerprint stays, and the part stays unchanged.
func TestSettingsShowNamesTheVendorPagesHosts(t *testing.T) {
	parallelStartingProcesses(t)
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	fingerprint := func() string {
		t.Helper()
		var value string
		if err := f.a.Store.db.QueryRow(`SELECT applied_fingerprint FROM settings_file_parts WHERE part='triage_agent.listed_hosts'`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := fingerprint()
	const line = "  triage_agent.listed_hosts: Also listed automatically from software_outside_the_distribution.yml, as of the latest run: advisories.example.com port 8443, github.com port 443"
	view, err := f.a.SettingsFileState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(view.Text(), "Also listed automatically") {
		t.Fatalf("settings show names hosts before any run:\n%s", view.Text())
	}
	systemsRepositoryCopy(t, f.a, map[string]string{"software_outside_the_distribution.yml": softwareOutsideTheDistribution})
	// The fixture's sandbox ends the run at once.
	if _, err := f.a.startTriageAgentRunInSystemsRepository(ctx, "triage_session", "MMHotline", triageagent.Request{Action: "start", Prompt: "The disk of app002 is full."}, func(*sql.Tx, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	waitNoTriageRun(t, f.a)
	if view, err = f.a.SettingsFileState(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Text(), "\ntriage_agent.listed_hosts: applied at ") || !strings.Contains(view.Text(), "\n"+line+"\n") {
		t.Fatalf("settings show lacks %q:\n%s", line, view.Text())
	}
	if got := states(f.apply())["triage_agent.listed_hosts"]; got != "unchanged" || fingerprint() != before {
		t.Fatalf("after the run, the part is %s and its fingerprint changed: %t", got, fingerprint() != before)
	}
	if view, err = f.a.SettingsFileState(ctx); err != nil || !strings.Contains(view.Text(), "\n"+line+"\n") {
		t.Fatalf("settings show after the apply lacks the line (%v):\n%s", err, view.Text())
	}
}
