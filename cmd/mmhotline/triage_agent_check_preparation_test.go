package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSSHRemoteURL(t *testing.T) {
	for url, want := range map[string]bool{
		"ssh://git@git.example.com/systems.git": true,
		"git@git.example.com:team/systems.git":  true,
		"git.example.com:team/systems.git":      true,
		"/srv/git/systems.git":                  false,
		"../systems.git":                        false,
		"file:///srv/git/systems.git":           false,
		"https://git.example.com/systems.git":   false,
		"":                                      false,
	} {
		if got := isSSHRemoteURL(url); got != want {
			t.Errorf("isSSHRemoteURL(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestNodeRunnerProgramName(t *testing.T) {
	for machine, want := range map[string]string{
		"x86_64":  "mmx-runner-linux-amd64",
		"aarch64": "mmx-runner-linux-arm64",
		"riscv64": "",
		"":        "",
	} {
		if got := nodeRunnerProgramName(machine); got != want {
			t.Errorf("nodeRunnerProgramName(%q) = %q, want %q", machine, got, want)
		}
	}
}

func TestSameSigningKeyComparesKeyTypeAndKeyOnly(t *testing.T) {
	applied := "ssh-ed25519 AAAAC3Nza triage agent"
	if !sameSigningKey("ssh-ed25519 AAAAC3Nza other-comment\n", applied) {
		t.Error("a different comment must not matter")
	}
	if sameSigningKey("ssh-ed25519 AAAAOTHER triage agent", applied) || sameSigningKey("", applied) || sameSigningKey("ssh-ed25519", applied) {
		t.Error("a different or missing key must not match")
	}
}

func TestOriginWithoutCredentials(t *testing.T) {
	got := describeOrigin("https://user:s3cretT0ken@git.example.com/systems.git")
	if strings.Contains(got, "s3cretT0ken") || strings.Contains(got, "user") {
		t.Errorf("origin shows a credential: %q", got)
	}
}

func TestPreparationReportLinesAndSummary(t *testing.T) {
	checks := []preparationCheck{
		{Number: 1, Name: "Server names resolve", Present: true, Detail: "web1 -> 10.77.253.93"},
		{Number: 7, Name: "MMHotline's copy reaches its origin over SSH", Detail: "origin is /srv/git/systems.git, a folder on this server", Fix: preparationFix(6)},
	}
	var text bytes.Buffer
	err := writePreparationReport(&text, false, checks)
	want := "Present: 1. Server names resolve: web1 -> 10.77.253.93.\n" +
		"Missing: 7. MMHotline's copy reaches its origin over SSH: origin is /srv/git/systems.git, a folder on this server. Fix: " + preparationGuideURL + " step 6.\n" +
		"1 of 2 checks are present; 1 is missing.\n"
	if text.String() != want {
		t.Errorf("report:\n%s\nwant:\n%s", text.String(), want)
	}
	var answer checkAnswer
	if !errors.As(err, &answer) || answer.exitCode != 1 {
		t.Errorf("error with a missing check: %v, want exit code 1", err)
	}

	var allPresent bytes.Buffer
	if err := writePreparationReport(&allPresent, false, checks[:1]); err != nil {
		t.Errorf("all present: %v", err)
	}
	if !strings.HasSuffix(allPresent.String(), "All 1 checks are present.\n") {
		t.Errorf("summary: %q", allPresent.String())
	}
}

func TestPreparationReportJSON(t *testing.T) {
	var out bytes.Buffer
	_ = writePreparationReport(&out, true, []preparationCheck{{Number: 4, Name: "The rules file", Detail: "missing", Fix: preparationFix(4)}})
	var parsed struct {
		Checks []struct {
			Number  int    `json:"number"`
			Name    string `json:"name"`
			Present bool   `json:"present"`
			Detail  string `json:"detail"`
			Fix     string `json:"fix"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil || len(parsed.Checks) != 1 {
		t.Fatalf("%v: %s", err, out.String())
	}
	if c := parsed.Checks[0]; c.Number != 4 || c.Present || c.Fix != preparationGuideURL+" step 4" {
		t.Errorf("check: %+v", c)
	}
}

func TestPreparationRefusesWithoutRoot(t *testing.T) {
	if err := requireRootForPreparation(1000); err == nil || !strings.Contains(err.Error(), "sudo mmhotline triage-agent check-preparation") {
		t.Errorf("non-root: %v", err)
	}
	if err := requireRootForPreparation(0); err != nil {
		t.Errorf("root: %v", err)
	}
}

// preparationFixture is a fully prepared server: the fake runner answers as
// the real programs do, and the files are in temporary folders.
type preparationFixture struct {
	p           preparation
	failSSHFor  string // a server name whose triage login fails
	origin      string
	ranCommands []string
}

const fixtureSigningKey = "ssh-ed25519 AAAAC3NzaFIXTURE triage agent"

func newPreparationFixture(t *testing.T) *preparationFixture {
	t.Helper()
	root := t.TempDir()
	f := &preparationFixture{origin: "git@git.example.com:team/systems.git"}
	agentHome, copy, mmhotlineHome, bin := filepath.Join(root, "agent"), filepath.Join(root, "copy"), filepath.Join(root, "mmhotline"), filepath.Join(root, "bin")
	for path, content := range map[string]string{
		filepath.Join(agentHome, ".ssh", "id_ed25519"):       "PRIVATE",
		filepath.Join(agentHome, ".ssh", "id_ed25519.pub"):   fixtureSigningKey + "\n",
		filepath.Join(mmhotlineHome, ".ssh", "id_ed25519"):   "PRIVATE",
		filepath.Join(agentHome, ".gitconfig"):               "",
		filepath.Join(copy, "triage_agent_rules.md"):         "rules",
		filepath.Join(copy, "inventories", "production.yml"): "",
		filepath.Join(bin, "mmx"):                            "#!/bin/sh\n",
		filepath.Join(bin, "mmx-runner-linux-amd64"):         "",
		filepath.Join(bin, "mmx-runner-linux-arm64"):         "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.p = preparation{
		copyDirectory: copy, agentHome: agentHome, mmhotlineHome: mmhotlineHome, mmxSearchPath: bin,
		appliedSigningKey: fixtureSigningKey, agentSetUp: true,
		lookupAddresses: func(name string) ([]net.IP, error) {
			if name == "gone" {
				return nil, errors.New("no such host")
			}
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.77.253.93")}, nil
		},
		run: func(runAs, directory, program string, args ...string) (string, string, error) {
			f.ranCommands = append(f.ranCommands, runAs+": "+program+" "+strings.Join(args, " "))
			joined := strings.Join(args, " ")
			program = filepath.Base(program)
			switch {
			case program == "mmx":
				return `{"schema_version":1,"nodes":[{"name":"web1","address":"10.77.253.93"},{"name":"db1","address":"10.77.253.94","ssh_user":"admin"}]}`, "", nil
			case program == "git" && strings.Contains(joined, "--get-regexp"):
				return "gpg.format ssh\ncommit.gpgsign true\nuser.signingkey " + filepath.Join(agentHome, ".ssh", "id_ed25519.pub") + "\n", "", nil
			case program == "git" && strings.Contains(joined, "remote get-url"):
				return f.origin + "\n", "", nil
			case program == "git" && strings.Contains(joined, "ls-remote"):
				return "abc\trefs/heads/main\n", "", nil
			case program == "ssh-keygen":
				return "|1|hash ssh-ed25519 AAAA\n", "", nil
			case program == "ssh" && strings.Contains(joined, "triage@"):
				if f.failSSHFor != "" && strings.Contains(joined, "triage@"+f.failSSHFor) {
					return "", "warning\ntriage@" + f.failSSHFor + ": Permission denied (publickey).\n", errors.New("exit status 255")
				}
				if strings.Contains(joined, "triage@db1") {
					return "aarch64\n", "", nil
				}
				return "x86_64\n", "", nil
			case program == "ssh":
				return "", "", nil
			}
			t.Errorf("unexpected command %s %s", program, joined)
			return "", "", errors.New("unexpected")
		},
	}
	return f
}

func presentNumbers(checks []preparationCheck) (present, missing []int) {
	for _, c := range checks {
		if c.Present {
			present = append(present, c.Number)
		} else {
			missing = append(missing, c.Number)
		}
	}
	return
}

func TestPreparedServerHasAllEightChecksPresent(t *testing.T) {
	f := newPreparationFixture(t)
	checks := f.p.checks()
	if len(checks) != 8 {
		t.Fatalf("%d checks", len(checks))
	}
	for i, c := range checks {
		if c.Number != i+1 || !c.Present || c.Fix != "" {
			t.Errorf("check %d: %+v", i+1, c)
		}
	}
	// Check 6 logs in as the inventory's ssh_user, or root without one.
	joined := strings.Join(f.ranCommands, "\n")
	if !strings.Contains(joined, "admin@10.77.253.94 true") || !strings.Contains(joined, "root@10.77.253.93 true") {
		t.Errorf("check 6 logins missing:\n%s", joined)
	}
}

func TestMissingChecksNameTheirStepAndLeavePrivateKeysOut(t *testing.T) {
	f := newPreparationFixture(t)
	f.failSSHFor = "db1"
	f.origin = "/srv/git/systems.git"
	if err := os.Remove(filepath.Join(f.p.copyDirectory, "triage_agent_rules.md")); err != nil {
		t.Fatal(err)
	}
	f.p.appliedSigningKey = "ssh-ed25519 AAAADIFFERENT other"
	checks := f.p.checks()
	_, missing := presentNumbers(checks)
	// 2: db1 refuses; 3: other signing key; 4: no rules; 7: local origin;
	// 8: db1's processor is unknown, so its runner cannot be named.
	if want := []int{2, 3, 4, 7, 8}; !equalInts(missing, want) {
		t.Fatalf("missing %v, want %v: %+v", missing, want, checks)
	}
	steps := map[int]int{2: 2, 3: 3, 4: 4, 7: 6, 8: 7}
	for _, c := range checks {
		if step, ok := steps[c.Number]; ok && c.Fix != preparationFix(step) {
			t.Errorf("check %d fix %q", c.Number, c.Fix)
		}
		if strings.Contains(c.Detail, "PRIVATE") {
			t.Errorf("check %d shows a private key: %q", c.Number, c.Detail)
		}
	}
	if !strings.Contains(checks[1].Detail, "Permission denied") || strings.Contains(checks[1].Detail, "warning") {
		t.Errorf("check 2 shows ssh's last line only: %q", checks[1].Detail)
	}
	if !strings.Contains(checks[7].Detail, "db1") || !strings.Contains(checks[7].Detail, "unknown") {
		t.Errorf("check 8: %q", checks[7].Detail)
	}
}

func TestChecksWithoutTheirInputsAreMissingWithTheReason(t *testing.T) {
	f := newPreparationFixture(t)
	f.p.copyDirectory = filepath.Join(t.TempDir(), "no-copy")
	f.p.agentSetUp = false
	checks := f.p.checks()
	for _, number := range []int{1, 3, 4, 7} {
		if checks[number-1].Present {
			t.Errorf("check %d is present without its input", number)
		}
	}
	if !strings.Contains(checks[2].Detail, "not set up") || !strings.Contains(checks[0].Detail, f.p.copyDirectory) {
		t.Errorf("reasons: %q / %q", checks[2].Detail, checks[0].Detail)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
