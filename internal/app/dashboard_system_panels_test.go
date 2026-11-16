package app

import (
	"fmt"
	"html"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFleetView stands in for the fleet view socket: each connection gets
// answer, as systemd's service instance would print it, and is closed.
type fakeFleetView struct {
	socket      string
	connections atomic.Int32
	answer      atomic.Value // []byte
}

func startFakeFleetView(t *testing.T, a *App, answer string) *fakeFleetView {
	t.Helper()
	// A short directory: a Unix socket path has a small length limit.
	directory, err := os.MkdirTemp("/tmp", "fleet-view-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	fake := &fakeFleetView{socket: filepath.Join(directory, "fleet-view.sock")}
	fake.answer.Store([]byte(answer))
	listener, err := net.Listen("unix", fake.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			fake.connections.Add(1)
			_, _ = connection.Write(fake.answer.Load().([]byte))
			connection.Close()
		}
	}()
	a.settingsFile.fleetViewCommand, a.FleetViewSocket = []string{"/usr/local/bin/monitoring-tool", "fleet", "--json"}, fake.socket
	return fake
}

// fleetViewFixture follows the fleet view's JSON: every server with its
// state, exit code, node groups, last report, and the checks that need
// attention, plus fields MMHotline does not read.
const fleetViewFixture = `{
  "exit_code": 1,
  "hub_address": "util01:8445",
  "hub_outage_alerter_nodes": ["app001", "db01"],
  "nodes": [
    {"name": "app001", "state": "reporting", "exit_code": 0, "this_node_is_in_node_groups": ["app_servers", "frankfurt"], "joined_at": "2026-10-01T00:00:00Z", "last_report_received_at": "2026-10-27T10:00:00Z", "report": {"summary": {"exit_code": 0, "attention": []}, "node_resources": {}}},
    {"name": "app002", "state": "reporting", "exit_code": 1, "this_node_is_in_node_groups": ["app_servers"], "joined_at": "2026-10-01T00:00:00Z", "last_report_received_at": "2026-10-27T10:00:00Z", "report": {"summary": {"exit_code": 1, "attention": [{"check": "disk/root_disk_usage", "state": "failing", "since": "2026-10-27T09:58:00Z", "message": "96% used, limit 90%"}, {"check": "systemd/every_systemd_unit", "finding": "caddy.service", "state": "failing", "since": "2026-10-27T09:59:00Z", "message": "failed"}]}}},
    {"name": "db01", "state": "silent", "exit_code": 1, "this_node_is_in_node_groups": ["database_servers"], "joined_at": "2026-10-01T00:00:00Z", "last_report_received_at": "2026-10-27T09:50:00Z", "report": {"summary": {"exit_code": 1, "attention": [{"check": "postgresql/replication_lag", "state": "failing", "since": "2026-10-27T09:45:00Z", "message": "lag 40s"}]}}},
    {"name": "util01", "state": "reporting", "exit_code": 0, "this_node_is_in_node_groups": ["util_server"], "has_fleet_hub_role": true, "joined_at": "2026-10-01T00:00:00Z", "last_report_received_at": "2026-10-27T10:00:00Z", "report": {"summary": {"exit_code": 0, "attention": []}}},
    {"name": "stray01", "state": "reporting", "exit_code": 3, "this_node_is_in_node_groups": [], "joined_at": "2026-10-01T00:00:00Z", "last_report_received_at": "2026-10-27T10:00:00Z", "report": {"summary": {"exit_code": 0, "attention": []}}}
  ],
  "refused_joins": {"count": 0},
  "storage_error": ""
}`

// systemsRepositoryCopy makes MMHotline's copy of a systems repository whose
// last commit holds files, and returns the copy's directory.
func systemsRepositoryCopy(t *testing.T, a *App, files map[string]string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	origin, seed := filepath.Join(root, "origin.git"), filepath.Join(root, "seed")
	testGit(t, root, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	testGit(t, root, "clone", "--quiet", origin, seed)
	for path, contents := range files {
		if err := os.WriteFile(filepath.Join(seed, path), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, seed, "add", "--all")
	testGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "Describe the servers")
	testGit(t, seed, "push", "--quiet", "origin", "main")
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	testGit(t, root, "clone", "--quiet", origin, directory)
	return directory
}

func dashboardPage(t *testing.T, a *App, signInEmailAddress string) string {
	t.Helper()
	request := httptest.NewRequest("GET", "https://mmhotline.example.com/dashboard", nil)
	addAccessToken(request, signInEmailAddress)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("dashboard: %d %s", response.Code, response.Body.String())
	}
	return html.UnescapeString(response.Body.String())
}

// pageInOrder fails unless page holds each text, in this order.
func pageInOrder(t *testing.T, page string, texts ...string) {
	t.Helper()
	rest := page
	for _, text := range texts {
		index := strings.Index(rest, text)
		if index < 0 {
			t.Fatalf("missing %q, in this order, in:\n%s", text, page)
		}
		rest = rest[index+len(text):]
	}
}

const checklistFixture = `# The checklist of this system.
stack_version: 2026-11
apps:
  shop:
    items:
      - id: shop_database_driver_finds_the_primary
        title: The app's Postgres driver finds the primary by itself
        state: deviated
        deviation: Node's pg driver can't, so the failover playbook rewrites the setting
      - id: shop_reads_settings_from_the_environment
        title: shop reads its settings from the environment
        state: done
        verified_at: 2026-10-01T00:00:00Z
items:
  - id: second_app_server
    title: A second app server, app002
    state: open
    protects_against: the site going down with app001
    waits_for: >-
      the first paying customers, or a restore that takes
      longer than the business can stay down
  - id: backups_restore_every_week
    title: Backups restore, every week
    state: done
    verified_at: 2026-10-19T04:12:00Z
    verify_again_every: 7d
  - id: dashboard_and_weekly_review
    title: The dashboard, and a weekly review
    state: done
    verified_at: "2026-10-22T09:00:00Z"
    verify_again_every: 7d
  - id: people_restore_drills
    title: "People drills: a bad change, total loss on staging"
    state: done
    verified_at: 2026-06-01T15:00:00Z
    verify_again_every: 6 months
  - id: break_glass_access
    title: Break-glass access, both ways in tested
    state: Done
  - id: secret_rotation
    title: Secret rotation
    state: done
    verified_at: 2026-10-20T00:00:00Z
    verify_again_every: 7 fortnights
what_this_system_does_not_survive: >-
  An outage of the whole location takes the site down.
`

// The dashboard shows, below what needs a person, each server of the fleet
// view once: those that need attention first, each with why, then the
// healthy ones by name, each with its groups in plain words. Beside it, the
// newest commits the copy has checked out, and the checklist: how much is
// done, the next item to do, and the other items that need a person, each
// with its question to copy.
func TestDashboardShowsServersChangesAndChecklist(t *testing.T) {
	a := foundationApp(t)
	person := memberFixture(t, a, "dashboard@example.test")
	now := time.Date(2026, 10, 27, 10, 0, 30, 0, time.UTC)
	a.Now = func() time.Time { return now }
	fleet := startFakeFleetView(t, a, fleetViewFixture)
	directory := systemsRepositoryCopy(t, a, map[string]string{checklistFileName: checklistFixture})
	for number := 1; number <= 10; number++ {
		testGit(t, directory, "commit", "--quiet", "--allow-empty", "-m", fmt.Sprintf("Change number %d", number))
	}
	// A commit the server's setup fetched but did not take, such as one it
	// refused, is on origin's branch only.
	other := filepath.Join(t.TempDir(), "other")
	testGit(t, a.Store.Dir, "clone", "--quiet", filepath.Join(directory, ".git"), other)
	testGit(t, other, "commit", "--quiet", "--allow-empty", "-m", "Refused change on origin only")
	testGit(t, directory, "fetch", "--quiet", other, "main:refs/remotes/origin/main")

	page := dashboardPage(t, a, person.Principal.SignInEmailAddress)
	pageInOrder(t, page, `id="incident-list"`, `id="system-servers"`, `id="system-changes"`, `id="system-checklist"`)
	// The team has no schedule, so times are in UTC, to the minute, with the
	// day when it is not today.
	pageInOrder(t, liveSectionText(page, "system-servers"), "Servers", "3 of 5 need attention",
		"app002", "App servers", "disk/root_disk_usage failing since 09:58: 96% used, limit 90%", "systemd/every_systemd_unit, caddy.service failing since 09:59: failed",
		"db01", "Database servers", "Silent: the server's last report arrived at 09:50", "Last known: postgresql/replication_lag failing since 09:45: lag 40s",
		"stray01", "Needs attention (exit code 3) without a failing check; the fleet view on this server shows why.",
		"app001 App servers, Frankfurt", "util01 Util server")
	changes := liveSectionText(page, "system-changes")
	pageInOrder(t, changes, "Recent changes to the systems repository", "Change number 10 · ", "Change number 2 · ", "Change number 1 · ")
	checklist := liveSectionText(page, "system-checklist")
	pageInOrder(t, checklist, "Checklist", "3 of 8 done", "4 open · 1 done another way",
		"Next:", "A second app server, app002", "Open", "Waits for: the first paying customers, or a restore that takes longer than the business can stay down",
		"Protects against: the site going down with app001", "Copy a question for your coding agent",
		"Show 4 more",
		"The app's Postgres driver finds the primary by itself", "Deviated", "Deviation: Node's pg driver can't, so the failover playbook rewrites the setting",
		"Backups restore, every week", "Open again", "Last verified 19 Oct 04:12; due again since 26 Oct 04:12 (every 7d).",
		"Break-glass access, both ways in tested", "Open", `The checklist item has state "Done". A checklist item must be open, done, or deviated.`,
		"Secret rotation", "Open again", `The checklist item has verify_again_every "7 fortnights", which is not an interval`)
	if !strings.Contains(page, `data-copy-question="Explain checklist item 'The app's Postgres driver finds the primary by itself' (shop_database_driver_finds_the_primary in monkeymaxxing_stack_checklist.yml) of my system and what it protects against."`) {
		t.Fatalf("the deviated item's question is missing:\n%s", page)
	}
	for _, absent := range []string{"node group", "app_servers", "Describe the servers", "Refused change on origin only", "shop reads its settings", "The dashboard, and a weekly review", "People drills", "An outage of the whole location"} {
		if strings.Contains(page, absent) {
			t.Fatalf("the dashboard shows %q:\n%s", absent, page)
		}
	}
	if strings.Count(page, "Copy a question for your coding agent") != 5 {
		t.Fatalf("one question for each of the 5 items:\n%s", page)
	}

	// Pages that refresh share one read for 10 seconds.
	dashboardPage(t, a, person.Principal.SignInEmailAddress)
	now = now.Add(9 * time.Second)
	dashboardPage(t, a, person.Principal.SignInEmailAddress)
	if fleet.connections.Load() != 1 {
		t.Fatalf("three pages within 10 seconds read the fleet view %d times", fleet.connections.Load())
	}
	now = now.Add(time.Second)
	fleet.answer.Store([]byte(`{"nodes": [{"name": "app001", "state": "reporting", "exit_code": 0, "this_node_is_in_node_groups": ["app_servers"]}]}`))
	page = dashboardPage(t, a, person.Principal.SignInEmailAddress)
	if servers := liveSectionText(page, "system-servers"); fleet.connections.Load() != 2 || servers != "Servers Healthy app001 App servers" {
		t.Fatalf("a page 10 seconds later reads the fleet view again (%d reads): %q", fleet.connections.Load(), servers)
	}
}

// The checklist's setup items count toward done and give the next step. The
// items for later, when it happens, fold under their own heading with their
// status, and never count as unfinished setup, even when open or due to be
// verified again. An item without part, and each app's item, is setup. A
// part MMHotline cannot read keeps the item among the setup items that need
// a person, with the reason, so a typing mistake never hides it.
func TestDashboardChecklistSeparatesSetupFromLater(t *testing.T) {
	a := foundationApp(t)
	person := memberFixture(t, a, "dashboard@example.test")
	a.Now = func() time.Time { return time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC) }
	systemsRepositoryCopy(t, a, map[string]string{checklistFileName: `items:
  - id: dns_and_tls
    title: DNS and TLS for the site
    part: setup
    state: open
  - id: backups
    title: Backups to a second provider
    part: setup
    state: done
    verified_at: 2026-10-01T00:00:00Z
  - id: monitoring
    title: Monitoring on every server
    state: done
  - id: staging_server
    title: A staging server
    part: later
    state: done
  - id: onboarding
    title: Onboard a new team member
    part: later_when_it_happens
    state: open
    waits_for: a new team member
  - id: restore_drill
    title: A restore drill
    part: later_when_it_happens
    state: done
    verified_at: 2026-10-01T00:00:00Z
  - id: key_rotation
    title: Rotate the keys
    part: later_when_it_happens
    state: done
    verified_at: 2026-10-01T00:00:00Z
    verify_again_every: 7d
apps:
  shop:
    items:
      - id: shop_settings
        title: shop reads its settings from the environment
        state: done
`})
	checklist := liveSectionText(dashboardPage(t, a, person.Principal.SignInEmailAddress), "system-checklist")
	pageInOrder(t, checklist, "Setup: 3 of 5 done", "2 open",
		"Next:", "DNS and TLS for the site", "Open",
		"Show 1 more", "A staging server", "Open", `The checklist item has part "later". A checklist item's part must be setup or later_when_it_happens.`,
		"Later, when it happens: 3 items · 1 due to be verified again", "None is unfinished setup.",
		"Onboard a new team member", "When it happens", "Waits for: a new team member",
		"A restore drill", "Done", "Verified 1 Oct 00:00.",
		"Rotate the keys", "Open again", "Last verified 1 Oct 00:00; due again since 8 Oct 00:00 (every 7d).")
	for _, absent := range []string{"Every setup item is done", "Backups to a second provider", "Monitoring on every server", "shop reads its settings"} {
		if strings.Contains(checklist, absent) {
			t.Fatalf("the checklist shows %q:\n%s", absent, checklist)
		}
	}
}

// foldedDetails matches each folded section of a page, which holds the
// technical detail a person opens only when they want it.
var foldedDetails = regexp.MustCompile(`(?s)<details.*?</details>`)

// A missing or unusable source makes its part of the dashboard say so in one
// line; the dashboard itself still opens. A path or the technical cause is
// folded away, never in the part's text.
func TestDashboardSaysWhatEachMissingSourceLacks(t *testing.T) {
	const noCopy, fold = "MMHotline has no copy of the systems repository", "Details"
	for _, test := range []struct {
		name string
		// prepare sets up the sources; nil fleet answer means no setting.
		prepare func(t *testing.T, a *App)
		want    []string
	}{
		{"nothing set up", func(t *testing.T, a *App) {}, []string{
			"The settings file has no fleet_view_command, so MMHotline shows no servers.",
			noCopy + ".", fold, "MMHotline looks for the systems repository copy at ", "systems_repository.", noCopy + ", so there is no checklist.", fold,
		}},
		{"no socket flag", func(t *testing.T, a *App) {
			a.settingsFile.fleetViewCommand = []string{"/usr/local/bin/monitoring-tool", "fleet", "--json"}
		}, []string{"MMHotline cannot show the servers: mmhotline serve was started without --fleet-view-socket."}},
		{"no units", func(t *testing.T, a *App) {
			a.settingsFile.fleetViewCommand, a.FleetViewSocket = []string{"/usr/local/bin/monitoring-tool", "fleet", "--json"}, filepath.Join(t.TempDir(), "missing.sock")
		}, []string{"MMHotline cannot show the servers.", fold, "MMHotline could not connect to the fleet view socket ", "which sudo mmhotline install sets up"}},
		{"a command that printed nothing", func(t *testing.T, a *App) { startFakeFleetView(t, a, "\n") }, []string{
			"MMHotline cannot show the servers.", fold, "the fleet view command printed nothing; check the command's errors with sudo journalctl -u 'mmhotline-fleet-view@*'.",
		}},
		{"a command that printed an error", func(t *testing.T, a *App) { startFakeFleetView(t, a, "fleet inspection runs on the hub\n") }, []string{
			"MMHotline cannot show the servers.", fold, "the fleet view command printed invalid JSON",
		}},
		{"JSON without nodes", func(t *testing.T, a *App) { startFakeFleetView(t, a, `{"exit_code": 0}`) }, []string{
			"MMHotline cannot show the servers.", fold, `the fleet view command printed JSON without a "nodes" list.`,
		}},
		{"a fleet view over the limit", func(t *testing.T, a *App) {
			startFakeFleetView(t, a, `{"nodes": [], "storage_error": "`+strings.Repeat("x", fleetViewSizeLimit)+`"}`)
		}, []string{"MMHotline cannot show the servers.", fold, "the fleet view is larger than MMHotline's 64 MiB limit for the fleet view."}},
		{"no servers yet", func(t *testing.T, a *App) { startFakeFleetView(t, a, `{"nodes": []}`) }, []string{"The fleet view lists no servers yet."}},
		{"no checklist file", func(t *testing.T, a *App) { systemsRepositoryCopy(t, a, map[string]string{"README.md": "# Servers\n"}) }, []string{
			"Describe the servers", "The systems repository has no monkeymaxxing_stack_checklist.yml at its root.",
		}},
		{"a checklist that is not YAML", func(t *testing.T, a *App) {
			systemsRepositoryCopy(t, a, map[string]string{checklistFileName: "items: [unclosed\n"})
		}, []string{"Describe the servers", "MMHotline cannot read monkeymaxxing_stack_checklist.yml.", fold, "yaml: "}},
		{"a checklist that is a symlink", func(t *testing.T, a *App) {
			directory := systemsRepositoryCopy(t, a, map[string]string{"elsewhere.yml": checklistFixture})
			if err := os.Symlink("elsewhere.yml", filepath.Join(directory, checklistFileName)); err != nil {
				t.Fatal(err)
			}
			testGit(t, directory, "add", "--all")
			testGit(t, directory, "commit", "--quiet", "-m", "Link the checklist")
		}, []string{"Link the checklist", "MMHotline cannot read monkeymaxxing_stack_checklist.yml.", fold, "monkeymaxxing_stack_checklist.yml at commit ", "is not a regular file."}},
		{"nothing open", func(t *testing.T, a *App) {
			systemsRepositoryCopy(t, a, map[string]string{checklistFileName: "items:\n  - id: one\n    title: One\n    state: done\n    verified_at: 2026-10-01T00:00:00Z\n"})
		}, []string{"1 of 1", "done", "0 open", "Every setup item is done, and none is due to be verified again."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := foundationApp(t)
			person := memberFixture(t, a, "dashboard@example.test")
			test.prepare(t, a)
			page := dashboardPage(t, a, person.Principal.SignInEmailAddress)
			parts := page[strings.Index(page, `id="system-servers"`):]
			pageInOrder(t, parts, test.want...)
			unfolded := foldedDetails.ReplaceAllString(parts, "")
			for _, path := range []string{filepath.Dir(t.TempDir()), "/tmp/fleet-view-"} {
				if strings.Contains(unfolded, path) {
					t.Fatalf("a part names the path %s outside its details:\n%s", path, parts)
				}
			}
		})
	}
}
