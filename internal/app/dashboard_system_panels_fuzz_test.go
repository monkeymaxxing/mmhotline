package app

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// FuzzFleetView reads fuzzed output of the fleet view command, which runs as
// root and prints the monitoring tool's fleet view. A refusal gives no view.
// From a view, the dashboard lists each server once: those whose exit code
// is not 0 as needing attention, the others as healthy, each list sorted by
// name, so no failing server is left out of the picture
// (docs/changing-mmhotline/design/interface.md#the-monitoring-tools-fleet-view).
func FuzzFleetView(f *testing.F) {
	for _, seed := range []string{
		fleetViewFixture,
		`{"nodes": [{"name": "app001", "state": "reporting", "exit_code": 0, "this_node_is_in_node_groups": ["app_servers"]}]}`,
		`{"nodes": []}`,
		`{"nodes": null}`,
		`{"nodes": [{"name": "db02", "state": "silent", "exit_code": 2, "last_report_received_at": "0001-01-01T00:00:00Z", "report": null}]}`,
		" \n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, output []byte) {
		view, err := decodeFleetView(output)
		if err != nil {
			if view.Nodes != nil {
				t.Fatalf("a refusal gave the view %+v", view)
			}
			return
		}
		needAttention, healthy := fleetServers(view)
		failing := 0
		for _, node := range view.Nodes {
			if node.ExitCode != 0 {
				failing++
			}
		}
		if len(needAttention) != failing || len(healthy) != len(view.Nodes)-failing {
			t.Fatalf("%d servers need attention and %d are healthy, of %d with %d failing", len(needAttention), len(healthy), len(view.Nodes), failing)
		}
		byName := func(a, b fleetServer) int { return strings.Compare(a.Name, b.Name) }
		if !slices.IsSortedFunc(needAttention, byName) || !slices.IsSortedFunc(healthy, byName) {
			t.Fatalf("servers out of order: %+v, %+v", needAttention, healthy)
		}
		for _, server := range needAttention {
			if server.ExitCode == 0 {
				t.Fatalf("the healthy server %q is listed as needing attention", server.Name)
			}
		}
		for _, server := range healthy {
			if server.ExitCode != 0 {
				t.Fatalf("the failing server %q is listed as healthy", server.Name)
			}
		}
	})
}

// checklistInterval is the documented form of verify_again_every.
var checklistInterval = regexp.MustCompile(`^[0-9]+ ?(h|d|w|hours?|days?|weeks?|months?|years?)$`)

// FuzzChecklistYAML reads a fuzzed monkeymaxxing_stack_checklist.yml. A file
// MMHotline cannot read gives no items. Each item is judged on its own. A
// typing mistake never hides an item: only an item whose state is exactly
// done is hidden, and one with verify_again_every only while its
// verified_at is an RFC 3339 time and its interval reads
// (docs/changing-mmhotline/design/interface.md#checklist). A shown item
// has a question that names it.
func FuzzChecklistYAML(f *testing.F) {
	f.Add([]byte(checklistFixture))
	f.Add([]byte("items:\n  - id: a\n    state: done\n    verified_at: 2026-10-19T04:12:00Z\n    verify_again_every: 7 m\n"))
	f.Add([]byte("apps: [shop]\n"))
	f.Add([]byte("- not a mapping\n"))
	f.Add([]byte("items: &a [*a]\n"))
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, text []byte) {
		items, err := checklistItems(text)
		if err != nil {
			if items != nil {
				t.Fatalf("a refusal gave the items %+v", items)
			}
			return
		}
		shown := 0
		for _, item := range items {
			views := openChecklistItems([]checklistItem{item}, now)
			switch {
			case len(views) > 1:
				t.Fatalf("one item gave %d views", len(views))
			case len(views) == 1:
				shown++
				if !strings.Contains(views[0].Question, "'"+views[0].Title+"'") || (item.Title != "" || item.ID != "") && views[0].Title == "" {
					t.Fatalf("the item %+v is shown as %+v", item, views[0])
				}
			case item.State != "done":
				t.Fatalf("the item %+v with the state %q is hidden", item, item.State)
			case item.VerifyAgainEvery != "":
				if _, err := time.Parse(time.RFC3339, item.VerifiedAt); err != nil || !checklistInterval.MatchString(strings.TrimSpace(item.VerifyAgainEvery)) {
					t.Fatalf("the done item %+v is hidden though its times do not read", item)
				}
			}
		}
		if all := openChecklistItems(items, now); len(all) != shown {
			t.Fatalf("%d items shown together, %d one by one", len(all), shown)
		}
	})
}
