package app

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"mmhotline/internal/triageagent"
)

// FuzzTriageAgentRequest checks a request the triage agent sends, field by
// field, as MMHotline checks every request it reads from the run socket: it
// never trusts the run. A refusal's message stays short, so a refused value
// of up to 1 MiB cannot make the answer exceed a run socket line. An
// accepted playbook path stays inside the systems repository, and an
// accepted inventory name is one argument of letters, digits, "_", and "-".
// An accepted playbook request has a diagnosis, and an accepted report a
// valid alert severity, each of 1 byte to 64 KiB of Markdown text; the
// fuzz's markdown argument is both. A request ID is 16 lowercase
// hexadecimal characters. When MMHotline
// gives MMX a playbook name for an accepted path, MMX reads exactly that
// file for the name: a dated name from one_time_playbooks/, any other from
// playbooks/ (docs/changing-mmhotline/design/triage-agent.md#requests-from-the-agent).
func FuzzTriageAgentRequest(f *testing.F) {
	for _, seed := range []triageagent.AgentRequest{
		{Kind: triageagent.PlaybookRunRequest, Playbook: "one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml", Inventory: "production", ReportText: "# The journal fills app002's disk"},
		{Kind: triageagent.PlaybookRunRequest, Playbook: "playbooks/_shared-setup.yml", Inventory: "staging-2"},
		{Kind: triageagent.PlaybookRunRequest, Playbook: "one_time_playbooks/../run_mmx.yml", Inventory: "production"},
		{Kind: triageagent.PlaybookRunRequest, Playbook: "/etc/mmx/playbook.yml", Inventory: "production"},
		{Kind: triageagent.PlaybookRunRequest, Playbook: "playbooks/2026-11-03_reboot_app002.yml", Inventory: "production;reboot"},
		{Kind: triageagent.ReportRequest, AlertSeverity: "warn", ReportText: "# Disk full on app002\n\nI freed 4 GiB."},
		{Kind: triageagent.ReportRequest, AlertSeverity: "critical", ReportText: " \n"},
		{Kind: "reboot", Playbook: "playbooks/deploy.yml"},
	} {
		f.Add(seed.Kind, seed.Playbook, seed.Inventory, seed.AlertSeverity, seed.ReportText, "0123456789abcdef")
	}
	f.Add(triageagent.ReportRequest, "", "", "info", strings.Repeat("a", triageagent.MaximumMarkdownFileSize+1), "../../x")
	inventoryName := regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
	requestID := regexp.MustCompile(`^[0-9a-f]{16}$`)
	repository := filepath.Join(string(filepath.Separator), "var", "lib", "mmhotline", "systems_repository")
	f.Fuzz(func(t *testing.T, kind, playbook, inventory, severity, markdown, id string) {
		if err := triageagent.CheckRequestID(id); (err == nil) != requestID.MatchString(id) {
			t.Fatalf("request ID %q: %v", id, err)
		}
		request := triageagent.AgentRequest{Kind: kind, Playbook: playbook, Inventory: inventory, AlertSeverity: severity, ReportText: markdown, Diagnosis: markdown}
		err := triageagent.CheckAgentRequest(request)
		if err != nil {
			if len(err.Error()) > 8<<10 {
				t.Fatalf("a refusal of %d bytes", len(err.Error()))
			}
			return
		}
		switch kind {
		case triageagent.PlaybookRunRequest:
			inside, joinErr := filepath.Rel(repository, filepath.Join(repository, playbook))
			parts := strings.Split(playbook, "/")
			if joinErr != nil || inside != filepath.Clean(playbook) || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(playbook) ||
				!strings.HasSuffix(playbook, ".yml") || utf8.RuneCountInString(playbook) > 300 || strings.ContainsFunc(playbook, unicode.IsControl) ||
				slices.Contains(parts, "") || slices.Contains(parts, ".") || slices.Contains(parts, "..") {
				t.Fatalf("accepted the playbook path %q", playbook)
			}
			if !inventoryName.MatchString(inventory) {
				t.Fatalf("accepted the inventory name %q", inventory)
			}
			if len(markdown) > triageagent.MaximumMarkdownFileSize || strings.TrimSpace(markdown) == "" {
				t.Fatalf("accepted a diagnosis of %d bytes", len(markdown))
			}
			if name, ok := mmxPlaybookName(playbook); ok {
				file := "playbooks/" + name + ".yml"
				if name != "" && name[0] >= '0' && name[0] <= '9' {
					file = "one_time_playbooks/" + name + ".yml"
				}
				if file != playbook || strings.HasPrefix(name, "-") {
					t.Fatalf("the playbook %q became the name %q, which MMX reads from %q", playbook, name, file)
				}
			}
		case triageagent.ReportRequest:
			if (severity != "info" && severity != "warn" && severity != "sos") || len(markdown) > triageagent.MaximumMarkdownFileSize || strings.TrimSpace(markdown) == "" {
				t.Fatalf("accepted the report at %q of %d bytes", severity, len(markdown))
			}
		default:
			t.Fatalf("accepted the request kind %q", kind)
		}
	})
}
