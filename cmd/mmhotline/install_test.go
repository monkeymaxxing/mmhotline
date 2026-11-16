package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"mmhotline/internal/app"
)

type fakeInstallCommands struct {
	missing         map[string]bool
	lookups, calls  []string
	mutations       []string
	enabled, active map[string]bool
	usersExist      map[string]bool
	failure         string
}

func (f *fakeInstallCommands) LookPath(name string) (string, error) {
	f.lookups = append(f.lookups, name)
	if f.missing[name] {
		return "", os.ErrNotExist
	}
	return "/usr/local/bin/" + name, nil
}
func (f *fakeInstallCommands) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if call == f.failure {
		return []byte("fixture failure"), errors.New("exit status 1")
	}
	switch {
	case strings.HasPrefix(call, "systemctl is-enabled "):
		if f.enabled[args[len(args)-1]] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), errors.New("disabled")
	case strings.HasPrefix(call, "systemctl is-active "):
		if f.active[args[len(args)-1]] {
			return nil, nil
		}
		return nil, errors.New("inactive")
	case strings.HasPrefix(call, "journalctl "):
		return []byte("service could not open settings\n"), nil
	}
	f.mutations = append(f.mutations, call)
	switch {
	case name == "useradd":
		f.usersExist[args[len(args)-1]] = true
	case name == "groupadd":
	case len(args) == 2 && args[0] == "enable":
		f.enabled[args[1]] = true
	case len(args) == 2 && args[0] == "restart":
		f.active[args[1]] = true
	default:
		if call != "systemctl daemon-reload" {
			return nil, fmt.Errorf("unexpected external command: %s", call)
		}
	}
	return nil, nil
}

func installFixture(t *testing.T) (*installer, *fakeInstallCommands, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	commands := &fakeInstallCommands{missing: map[string]bool{}, usersExist: map[string]bool{}, enabled: map[string]bool{}, active: map[string]bool{}}
	output := new(bytes.Buffer)
	i := &installer{root: root, operatingSystem: "linux", effectiveUID: 0, rootUID: os.Geteuid(), rootGID: os.Getegid(), executable: filepath.Join(root, "downloaded-mmhotline"), commands: commands, output: output, readyTimeout: time.Second}
	i.lookupUser = func(name string) (*user.User, error) {
		if !commands.usersExist[name] {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Uid: fmt.Sprint(os.Geteuid()), Gid: fmt.Sprint(os.Getegid())}, nil
	}
	i.people = func(_ context.Context, socket string) (app.CommandResult, error) {
		if socket != i.path(installCommandSocket) {
			t.Fatalf("readiness socket: %s", socket)
		}
		return app.CommandResult{PeopleCommandResult: app.PeopleCommandResult{WebURL: "https://mmhotline.example.com"}}, nil
	}
	// An empty bucket, as litestream ltx lists one.
	i.listBackup = func(context.Context, string, []string) (string, error) {
		return "level  min_txid  max_txid  size  created\n", nil
	}
	if err := os.MkdirAll(i.path("/run/systemd/system"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.executable, []byte("new program"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestSettingsFile(t, i.path(installSettingsPath), testSettingsYAML("https://mmhotline.example.com", "127.0.0.1:8480", "10.20.0.1:8484"))
	return i, commands, output
}
func writeInstallFixture(t *testing.T, i *installer, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(i.path(path)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.path(path), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

type installedFile struct {
	bytes    string
	mode     fs.FileMode
	modified time.Time
}

func installSnapshot(t *testing.T, root string) map[string]installedFile {
	t.Helper()
	files := map[string]installedFile{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var contents []byte
		if !entry.IsDir() {
			contents, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		files[strings.TrimPrefix(path, root)] = installedFile{string(contents), info.Mode(), info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
func TestInstallRerunPreservesFilesAndMakesNoChangeCalls(t *testing.T) {
	i, commands, output := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"Checks: Linux, root, and systemd confirmed",
		"Backup: not set up;",
		"MMHotline: https://mmhotline.example.com",
		"Database: created mmhotline.db for Operations",
		"Settings file: already has owner root, group mmhotline, and mode 0640: /etc/mmhotline/mmhotline_settings.yml",
		"Next: set up cloudflared and a Cloudflare Access application for https://mmhotline.example.com, add cloudflare_access to /etc/mmhotline/mmhotline_settings.yml, and apply the file, with your deploy tool or with sudo mmhotline settings apply (https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/put-mmhotline-behind-cloudflare-access.md)",
	} {
		if !strings.Contains(output.String(), line) {
			t.Fatalf("missing %q in:\n%s", line, output)
		}
	}
	wantCalls := []string{
		"groupadd --system --force mmhotline",
		"useradd --system --gid mmhotline --home-dir /var/lib/mmhotline --no-create-home --shell /usr/sbin/nologin mmhotline",
		"useradd --system --user-group --home-dir /var/lib/mmhotline-triage-agent --no-create-home --shell /usr/sbin/nologin mmhotline-triage-agent",
		"useradd --system --user-group --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin mmhotline-review-agent",
		"systemctl daemon-reload", "systemctl enable mmhotline-triage-agent-run.socket", "systemctl restart mmhotline-triage-agent-run.socket",
		"systemctl daemon-reload", "systemctl enable mmhotline-review-agent-run.socket", "systemctl restart mmhotline-review-agent-run.socket",
		"systemctl daemon-reload", "systemctl enable mmhotline-fleet-view.socket", "systemctl restart mmhotline-fleet-view.socket",
		"systemctl daemon-reload", "systemctl enable mmhotline", "systemctl restart mmhotline",
	}
	if !reflect.DeepEqual(commands.mutations, wantCalls) {
		t.Fatalf("change calls: %q", commands.mutations)
	}
	before := installSnapshot(t, i.root)
	commands.mutations = nil
	output.Reset()
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(commands.mutations) != 0 {
		t.Fatalf("unchanged rerun made change calls: %q", commands.mutations)
	}
	if after := installSnapshot(t, i.root); !reflect.DeepEqual(before, after) {
		t.Fatal("unchanged rerun changed file contents, modes, or modification times")
	}
	if !strings.Contains(output.String(), "Database: mmhotline.db already exists; kept") {
		t.Fatal(output.String())
	}
	store, err := app.Open(i.path(installStateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	members, err := app.New(store).TeamMembers(t.Context(), app.Principal{UnixUser: "root"})
	if err != nil || len(members) != 0 {
		t.Fatalf("setup must create no person: %v %v", members, err)
	}
}
func TestInstallPreservesTeamSettingsAndDatabase(t *testing.T) {
	i, commands, output := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The team changes its settings file; install reads it and never
	// rewrites it, and keeps the database it created.
	custom := strings.Replace(testSettingsYAML("https://custom.example.com", "127.0.0.1:8480", "10.20.0.1:8484"), "team_name: Operations", "team_name: Custom Operations\ncloudflare_access:\n  team_domain: example.cloudflareaccess.com\n  audience_tag: abc123", 1)
	writeTestSettingsFile(t, i.path(installSettingsPath), custom)
	i.people = func(context.Context, string) (app.CommandResult, error) {
		return app.CommandResult{PeopleCommandResult: app.PeopleCommandResult{WebURL: "https://custom.example.com", People: []app.Member{{Name: "Barbara"}}}}, nil
	}
	before := installSnapshot(t, i.root)
	output.Reset()
	commands.mutations = nil
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := installSnapshot(t, i.root); !reflect.DeepEqual(before, after) {
		t.Fatal("existing settings or database rewritten")
	}
	if !strings.Contains(output.String(), "MMHotline: https://custom.example.com") || strings.Contains(output.String(), "Next:") {
		t.Fatal(output.String())
	}
}
func TestInstallUpgradeFromInstalledBinary(t *testing.T) {
	i, commands, _ := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	settingsBefore, err := os.ReadFile(i.path(installSettingsPath))
	if err != nil {
		t.Fatal(err)
	}
	i.executable = i.path(installBinaryPath)
	if err := os.WriteFile(i.executable, []byte("upgraded program"), 0755); err != nil {
		t.Fatal(err)
	}
	commands.mutations = nil
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(commands.mutations, []string{"systemctl daemon-reload", "systemctl restart mmhotline"}) {
		t.Fatalf("upgrade: %q", commands.mutations)
	}
	unit, err := os.ReadFile(i.path("/etc/systemd/system/mmhotline.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), fmt.Sprintf("%x", sha256.Sum256([]byte("upgraded program")))) {
		t.Fatal("unit did not record upgraded binary")
	}
	settingsAfter, err := os.ReadFile(i.path(installSettingsPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(settingsBefore, settingsAfter) {
		t.Fatal("upgrade changed settings")
	}
	commands.mutations = nil
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(commands.mutations) != 0 {
		t.Fatalf("installed binary rerun: %q", commands.mutations)
	}
}
func TestInstallChecksExplainRemedy(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*installer, *fakeInstallCommands)
		want   string
	}{
		{"not_linux", func(i *installer, _ *fakeInstallCommands) { i.operatingSystem = "darwin" }, "team's Linux server"},
		{"not_root", func(i *installer, _ *fakeInstallCommands) { i.effectiveUID = 1000 }, "sudo mmhotline install"},
		{"no_systemd", func(i *installer, _ *fakeInstallCommands) { i.root = filepath.Join(i.root, "no-systemd") }, "boot the server with systemd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			i, f, _ := installFixture(t)
			test.change(i, f)
			err := i.run(t.Context())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
			if len(f.mutations) != 0 {
				t.Fatalf("failed checks changed server: %q", f.mutations)
			}
		})
	}
}

// Install needs the settings file first, with its owner and mode right and
// no problem, and changes nothing until it has one.
func TestInstallRefusesWithoutAGoodSettingsFile(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(t *testing.T, i *installer)
		want   []string
	}{
		{"missing", func(t *testing.T, i *installer) {
			if err := os.Remove(i.path(installSettingsPath)); err != nil {
				t.Fatal(err)
			}
		}, []string{"install requires the settings file /etc/mmhotline/mmhotline_settings.yml", "sudoedit /etc/mmhotline/mmhotline_settings.yml", "owner root and mode 0640", settingsFileReferenceURL}},
		{"a problem", func(t *testing.T, i *installer) {
			writeTestSettingsFile(t, i.path(installSettingsPath), strings.Replace(testSettingsYAML("https://mmhotline.example.com", "127.0.0.1:8480", "10.20.0.1:8484"), "    role: administrator", "    role: owner", 1))
		}, []string{"the settings file /etc/mmhotline/mmhotline_settings.yml has problems, so install changed nothing", "people[0].role: person role must be administrator or member", settingsFileReferenceURL}},
		{"readable by everyone", func(t *testing.T, i *installer) {
			if err := os.Chmod(i.path(installSettingsPath), 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"can be read by every user on the server (mode 0644)", "sudo chown root:mmhotline"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			i, f, _ := installFixture(t)
			test.change(t, i)
			before := installSnapshot(t, i.root)
			err := i.run(t.Context())
			for _, want := range test.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("want %q in: %v", want, err)
				}
			}
			if exitCode(err) != 4 {
				t.Fatalf("exit code %d", exitCode(err))
			}
			if len(f.calls) != 0 || !reflect.DeepEqual(before, installSnapshot(t, i.root)) {
				t.Fatalf("refused install changed the server: %q", f.calls)
			}
		})
	}
}

// The network path to MMHotline (cloudflared, the private network) is the
// team's to set up; install runs only what creates users and services.
func TestInstallRunsOnlyUseraddAndSystemctl(t *testing.T) {
	i, f, _ := installFixture(t)
	for range 2 {
		if err := i.run(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	programs := map[string]bool{}
	for _, call := range f.calls {
		programs[strings.Fields(call)[0]] = true
	}
	for _, name := range f.lookups {
		programs[name] = true
	}
	if !reflect.DeepEqual(programs, map[string]bool{"groupadd": true, "useradd": true, "systemctl": true}) {
		t.Fatalf("programs run or looked up: %v", programs)
	}
}
func TestInstallBackupNeedsSettingsAndLitestream(t *testing.T) {
	for _, settings := range []bool{false, true} {
		for _, program := range []bool{false, true} {
			t.Run(fmt.Sprintf("settings_%v_program_%v", settings, program), func(t *testing.T) {
				i, f, output := installFixture(t)
				f.missing["litestream"] = !program
				if settings {
					writeInstallFixture(t, i, installBackupSettingsPath, "MMHOTLINE_BACKUP_REPLICA_URL=s3://example/mmhotline\nLITESTREAM_SECRET_ACCESS_KEY=secret\n")
				}
				err := i.run(t.Context())
				if settings && !program {
					if err == nil || !strings.Contains(err.Error(), "install Litestream 0.5.18") {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				exists, err := installFileExists(i.path("/etc/systemd/system/mmhotline-backup.service"))
				if err != nil {
					t.Fatal(err)
				}
				if exists != (settings && program) {
					t.Fatalf("backup unit exists=%v", exists)
				}
				if settings && program {
					if !f.enabled["mmhotline-backup"] || !f.active["mmhotline-backup"] {
						t.Fatal("backup service not enabled and started")
					}
					// Litestream runs as its own user in group mmhotline.
					if !slices.Contains(f.mutations, "useradd --system --gid mmhotline --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin mmhotline-backup") {
						t.Fatalf("backup user not created: %q", f.mutations)
					}
					before := installSnapshot(t, i.root)
					f.mutations = nil
					if err := i.run(t.Context()); err != nil {
						t.Fatal(err)
					}
					if len(f.mutations) != 0 || !reflect.DeepEqual(before, installSnapshot(t, i.root)) {
						t.Fatalf("backup rerun changed installation: %q", f.mutations)
					}
				}
				if !settings && !strings.Contains(output.String(), "Backup: not set up") {
					t.Fatal(output.String())
				}
			})
		}
	}
}
func TestInstallReadinessFailureShowsJournal(t *testing.T) {
	i, _, _ := installFixture(t)
	i.readyTimeout = time.Millisecond
	i.people = func(ctx context.Context, _ string) (app.CommandResult, error) {
		<-ctx.Done()
		return app.CommandResult{}, ctx.Err()
	}
	err := i.run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "service could not open settings") {
		t.Fatal(err)
	}
}
func TestInstallFileModes(t *testing.T) {
	i, _, _ := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fs.FileMode{installBinaryPath: 0755, installStateDir: 0770, installStateDir + "/mmhotline.db": 0660, installSettingsPath: 0640, "/etc/systemd/system/mmhotline.service": 0644} {
		info, err := os.Stat(i.path(path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}
func TestInstallHelpAndArguments(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--unknown"}, {"unexpected"}} {
		err := runInstall(args)
		if args[0] == "--help" {
			if err != nil {
				t.Fatal(err)
			}
		} else if exitCode(err) != 4 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestInstallGeneratedUnits(t *testing.T) {
	service := `# Managed by mmhotline install. Binary SHA256: fixture-checksum
[Unit]
Description=MMHotline on-call service
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=notify
User=mmhotline
ExecStart=/usr/local/bin/mmhotline serve --state-dir /var/lib/mmhotline --settings-file /etc/mmhotline/mmhotline_settings.yml --command-socket /run/mmhotline/command.sock --triage-agent-run-socket /run/mmhotline-triage-agent/run.sock --review-agent-run-socket /run/mmhotline-review-agent/run.sock --triage-agent-proxy-socket /run/mmhotline/triage-agent-proxy.sock --fleet-view-socket /run/mmhotline-fleet-view.sock
RuntimeDirectory=mmhotline
RuntimeDirectoryMode=0700
UMask=0077
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/mmhotline
ProtectHome=yes
PrivateTmp=yes
CapabilityBoundingSet=
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM
SystemCallFilter=seccomp

[Install]
WantedBy=multi-user.target
`
	backup := `# Managed by mmhotline install.
[Unit]
Description=MMHotline continuous database backup with Litestream
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
User=mmhotline-backup
Group=mmhotline
EnvironmentFile=/etc/mmhotline/backup.env
StateDirectory=mmhotline-backup
StateDirectoryMode=0700
ExecStart="/usr/local/bin/litestream" replicate -config /etc/mmhotline/litestream.yml
UMask=0077
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/mmhotline
ProtectHome=yes
PrivateTmp=yes
CapabilityBoundingSet=
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
`
	if got := installServiceUnit("fixture-checksum"); got != service {
		t.Fatalf("service unit:\n%s", got)
	}
	if got := installBackupUnit("/usr/local/bin/litestream"); got != backup {
		t.Fatalf("backup unit:\n%s", got)
	}
	// A discovered path must remain one literal systemd executable, including
	// spaces and characters that systemd would otherwise expand.
	if got := installBackupUnit("/opt/backup tools/100%/$litestream"); !strings.Contains(got, `ExecStart="/opt/backup tools/100%%/$$litestream" replicate `) {
		t.Fatal(got)
	}
}

// Install takes the team's name from the settings file, and a team without
// Cloudflare, which opens MMHotline through an SSH forward, gets no step
// about cloudflared.
func TestInstallTakesTheTeamFromTheSettingsFile(t *testing.T) {
	i, _, output := installFixture(t)
	writeTestSettingsFile(t, i.path(installSettingsPath), strings.Replace(testSettingsYAML("http://127.0.0.1:8480", "127.0.0.1:8480", "[fd00::1]:8484"), "team_name: Operations", "team_name: Night Shift", 1))
	i.people = func(context.Context, string) (app.CommandResult, error) {
		return app.CommandResult{PeopleCommandResult: app.PeopleCommandResult{WebURL: "http://127.0.0.1:8480"}}, nil
	}
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Database: created mmhotline.db for Night Shift") || strings.Contains(output.String(), "cloudflared") {
		t.Fatalf("install output:\n%s", output)
	}
	store, err := app.Open(i.path(installStateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	members, err := app.New(store).TeamMembers(t.Context(), app.Principal{UnixUser: "root"})
	if err != nil || len(members) != 0 {
		t.Fatalf("install adds no person itself; the service applies people: %v %v", members, err)
	}
}
func TestInstallRepairsUnitAndRestartsStoppedService(t *testing.T) {
	i, f, _ := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, i, "/etc/systemd/system/mmhotline.service", "outdated unit\n")
	f.mutations = nil
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.mutations, []string{"systemctl daemon-reload", "systemctl restart mmhotline"}) {
		t.Fatal(f.mutations)
	}
	f.mutations = nil
	f.active["mmhotline"] = false
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.mutations, []string{"systemctl restart mmhotline"}) {
		t.Fatal(f.mutations)
	}
}

func TestInstallStartFailureShowsJournal(t *testing.T) {
	i, f, _ := installFixture(t)
	f.failure = "systemctl restart mmhotline"
	err := i.run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "service could not open settings") || !strings.Contains(err.Error(), "systemctl restart mmhotline") {
		t.Fatal(err)
	}
}

func TestInstallTriageAgentFiles(t *testing.T) {
	i, commands, output := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/etc/systemd/system/mmhotline-triage-agent-run.socket": `# Managed by mmhotline install.
[Unit]
Description=MMHotline triage agent runs: one sandboxed run per connection from MMHotline

[Socket]
ListenStream=/run/mmhotline-triage-agent/run.sock
SocketUser=mmhotline
SocketGroup=mmhotline
SocketMode=0600
Accept=yes
MaxConnections=4

[Install]
WantedBy=sockets.target
`,
		"/etc/systemd/system/mmhotline-triage-agent-run@.service": `# Managed by mmhotline install.
[Unit]
Description=One sandboxed MMHotline triage agent run
CollectMode=inactive-or-failed

[Service]
User=mmhotline-triage-agent
Group=mmhotline-triage-agent
ExecStart=/usr/local/bin/mmhotline triage-agent run-inside-sandbox --proxy-socket /run/mmhotline-triage-agent-proxy.sock
StandardInput=socket
StandardOutput=socket
StandardError=journal
Environment=HOME=/var/lib/mmhotline-triage-agent
WorkingDirectory=/var/lib/mmhotline-triage-agent
UMask=0077
RuntimeMaxSec=3600
PrivateNetwork=yes
BindPaths=/run/mmhotline/triage-agent-proxy.sock:/run/mmhotline-triage-agent-proxy.sock
InaccessiblePaths=-/var/lib/mmhotline -/etc/mmhotline -/run/mmhotline
ReadWritePaths=/var/lib/mmhotline-triage-agent
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
CapabilityBoundingSet=
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
`,
	} {
		contents, err := os.ReadFile(i.path(path))
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != want {
			t.Errorf("%s content:\n%s", path, contents)
		}
		info, err := os.Stat(i.path(path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0644 {
			t.Errorf("%s mode: %o", path, info.Mode().Perm())
		}
	}
	info, err := os.Stat(i.path("/var/lib/mmhotline-triage-agent"))
	if err != nil {
		t.Fatal(err)
	}
	owner := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || int(owner.Uid) != os.Geteuid() || int(owner.Gid) != os.Getegid() {
		t.Fatalf("triage home metadata: %v, %v", info, owner)
	}
	if !commands.usersExist["mmhotline-triage-agent"] || !commands.enabled["mmhotline-triage-agent-run.socket"] || !commands.active["mmhotline-triage-agent-run.socket"] {
		t.Fatal(commands.mutations)
	}
	if !strings.Contains(output.String(), "Unix user: created mmhotline-triage-agent with no login shell") || !strings.Contains(output.String(), "Triage agent home: installed or corrected") {
		t.Fatal(output.String())
	}
	output.Reset()
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Unix user: mmhotline-triage-agent already exists", "Triage agent home: already correct", "Service: mmhotline-triage-agent-run.socket unit already correct; already enabled; already running"} {
		if !strings.Contains(output.String(), want) {
			t.Fatal(output.String())
		}
	}
}

// A socket-activated unit starts one instance per connection; a run that
// fails or hits RuntimeMaxSec would stay as a failed unit, which the fleet's
// monitoring reports as a new alert. systemd reads CollectMode= only in
// [Unit], and ignores it in [Service].
func TestInstallCollectsFailedInstancesOfEverySocketActivatedUnit(t *testing.T) {
	i, _, _ := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	units, err := filepath.Glob(i.path("/etc/systemd/system/*@.service"))
	if err != nil || len(units) < 2 {
		t.Fatalf("instance units %v: %v", units, err)
	}
	for _, unit := range units {
		contents, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		section, collected := "", false
		for line := range strings.Lines(string(contents)) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				section = line
			}
			if line == "CollectMode=inactive-or-failed" {
				collected = section == "[Unit]"
			}
		}
		if !collected {
			t.Errorf("%s does not set CollectMode=inactive-or-failed in [Unit]:\n%s", filepath.Base(unit), contents)
		}
	}
}

func TestInstallRepairsSocketActivatedUnits(t *testing.T) {
	for unit, socket := range map[string]string{
		"mmhotline-triage-agent-run.socket":   "mmhotline-triage-agent-run.socket",
		"mmhotline-triage-agent-run@.service": "mmhotline-triage-agent-run.socket",
		"mmhotline-review-agent-run.socket":   "mmhotline-review-agent-run.socket",
		"mmhotline-review-agent-run@.service": "mmhotline-review-agent-run.socket",
		"mmhotline-fleet-view.socket":         "mmhotline-fleet-view.socket",
		"mmhotline-fleet-view@.service":       "mmhotline-fleet-view.socket",
	} {
		t.Run(unit, func(t *testing.T) {
			i, commands, _ := installFixture(t)
			if err := i.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			path := "/etc/systemd/system/" + unit
			before, err := os.ReadFile(i.path(path))
			if err != nil {
				t.Fatal(err)
			}
			writeInstallFixture(t, i, path, "outdated unit\n")
			commands.mutations = nil
			if err := i.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(i.path(path))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("unit not repaired: %s", after)
			}
			if !reflect.DeepEqual(commands.mutations, []string{"systemctl daemon-reload", "systemctl restart " + socket}) {
				t.Fatal(commands.mutations)
			}
		})
	}
}

func TestInstallRepairsTriageAgentHomeMode(t *testing.T) {
	i, _, output := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := i.path("/var/lib/mmhotline-triage-agent")
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 || !strings.Contains(output.String(), "Triage agent home: installed or corrected") {
		t.Fatal(output.String())
	}
}

// Install always writes the fleet view units, so that adding
// fleet_view_command later needs only settings apply; only the mmhotline
// user may connect, and the command runs as root under its own restrictions.
func TestInstallFleetViewFiles(t *testing.T) {
	i, commands, output := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/etc/systemd/system/mmhotline-fleet-view.socket": `# Managed by mmhotline install.
[Unit]
Description=MMHotline's fleet view: one run of the settings file's fleet_view_command per connection from MMHotline

[Socket]
ListenStream=/run/mmhotline-fleet-view.sock
SocketUser=mmhotline
SocketGroup=mmhotline
SocketMode=0600
Accept=yes
MaxConnections=2

[Install]
WantedBy=sockets.target
`,
		"/etc/systemd/system/mmhotline-fleet-view@.service": `# Managed by mmhotline install.
[Unit]
Description=One run of MMHotline's fleet view command, as root
CollectMode=inactive-or-failed

[Service]
ExecStart=/usr/local/bin/mmhotline run-fleet-view-command --settings-file /etc/mmhotline/mmhotline_settings.yml
StandardInput=socket
StandardOutput=socket
StandardError=journal
UMask=0077
RuntimeMaxSec=20
PrivateNetwork=yes
InaccessiblePaths=-/var/lib/mmhotline -/var/lib/mmhotline-backup -/var/lib/mmhotline-triage-agent -/etc/mmhotline/backup.env -/run/mmhotline
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
CapabilityBoundingSet=
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM
`,
	} {
		contents, err := os.ReadFile(i.path(path))
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != want {
			t.Errorf("%s content:\n%s", path, contents)
		}
		info, err := os.Stat(i.path(path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0644 {
			t.Errorf("%s mode: %o", path, info.Mode().Perm())
		}
	}
	if !commands.enabled["mmhotline-fleet-view.socket"] || !commands.active["mmhotline-fleet-view.socket"] {
		t.Fatal(commands.mutations)
	}
	output.Reset()
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Service: mmhotline-fleet-view.socket unit already correct; already enabled; already running") {
		t.Fatal(output.String())
	}
}

// unitValues returns the values of key's lines in a systemd unit.
func unitValues(unit, key string) []string {
	var values []string
	for line := range strings.Lines(unit) {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), key+"="); found {
			values = append(values, strings.Fields(value)...)
		}
	}
	return values
}

// The review agent judges the triage agent's playbooks, so a triage run must
// not reach a review run: each runs as its own user and group, so a triage
// run cannot read a review run's /proc entries, its credential among them,
// and the review run writes nothing the triage agent can write.
func TestInstallAgentRunUnitsShareNothingWritable(t *testing.T) {
	i, commands, output := installFixture(t)
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	read := func(path string) string {
		contents, err := os.ReadFile(i.path(path))
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}
	triage, review := read("/etc/systemd/system/mmhotline-triage-agent-run@.service"), read("/etc/systemd/system/mmhotline-review-agent-run@.service")
	for _, key := range []string{"User", "Group"} {
		if got := unitValues(review, key); !reflect.DeepEqual(got, []string{"mmhotline-review-agent"}) || reflect.DeepEqual(got, unitValues(triage, key)) {
			t.Errorf("review run %s=%q, triage run %s=%q", key, got, key, unitValues(triage, key))
		}
	}
	if got := unitValues(review, "ReadWritePaths"); len(got) != 0 {
		t.Errorf("the review run writes outside its private /tmp: %q", got)
	}
	for _, key := range []string{"Environment", "WorkingDirectory", "SupplementaryGroups", "RuntimeDirectory", "StateDirectory"} {
		if got := unitValues(review, key); len(got) != 0 {
			t.Errorf("review run %s=%q", key, got)
		}
	}
	if !slices.Contains(unitValues(review, "InaccessiblePaths"), "-/var/lib/mmhotline-triage-agent") {
		t.Errorf("the review run can see the triage agent's home:\n%s", review)
	}
	if !slices.Contains(unitValues(review, "PrivateTmp"), "yes") || !slices.Contains(unitValues(review, "ProtectSystem"), "strict") {
		t.Errorf("the review run's file system is not its own:\n%s", review)
	}
	if !slices.Contains(unitValues(read("/etc/systemd/system/mmhotline-review-agent-run.socket"), "ListenStream"), "/run/mmhotline-review-agent/run.sock") ||
		!strings.Contains(read("/etc/systemd/system/mmhotline.service"), " --review-agent-run-socket /run/mmhotline-review-agent/run.sock ") {
		t.Error("serve does not start review runs through the review agent's socket")
	}
	if !commands.usersExist["mmhotline-review-agent"] || !commands.enabled["mmhotline-review-agent-run.socket"] || !commands.active["mmhotline-review-agent-run.socket"] {
		t.Fatal(commands.mutations)
	}
	if !strings.Contains(output.String(), "Unix user: created mmhotline-review-agent with no login shell") {
		t.Fatal(output.String())
	}
	output.Reset()
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Unix user: mmhotline-review-agent already exists", "Service: mmhotline-review-agent-run.socket unit already correct; already enabled; already running"} {
		if !strings.Contains(output.String(), want) {
			t.Fatal(output.String())
		}
	}
}

// The bucket keys reach only Litestream's process, which runs as a user other
// than serve's, so serve cannot read them from /proc. Litestream writes the
// database through group mmhotline.
func TestInstallBackupRunsAsItsOwnUser(t *testing.T) {
	backup, service := installBackupUnit("/usr/local/bin/litestream"), installServiceUnit("fixture-checksum")
	if got := unitValues(backup, "User"); !reflect.DeepEqual(got, []string{"mmhotline-backup"}) || reflect.DeepEqual(got, unitValues(service, "User")) {
		t.Errorf("backup User=%q, serve User=%q", got, unitValues(service, "User"))
	}
	if got := unitValues(backup, "Group"); !reflect.DeepEqual(got, []string{"mmhotline"}) {
		t.Errorf("backup Group=%q", got)
	}
	if got := unitValues(service, "EnvironmentFile"); len(got) != 0 {
		t.Errorf("serve reads %q", got)
	}
}

// Litestream keeps its working files where only mmhotline-backup may write:
// in the directory systemd makes for the backup unit, outside the state
// directory that mmhotline writes. No step as root then gives the files to
// mmhotline-backup, after an upgrade or a restore, and the backup check reads
// the captured changes where Litestream writes them.
func TestInstallBackupKeepsLitestreamsWorkingFilesOutOfTheStateDirectory(t *testing.T) {
	i, _, _ := installFixture(t)
	writeInstallFixture(t, i, installBackupSettingsPath, "MMHOTLINE_BACKUP_REPLICA_URL=s3://example/mmhotline\n")
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(i.path("/etc/systemd/system/mmhotline-backup.service"))
	if err != nil {
		t.Fatal(err)
	}
	if got := unitValues(string(unit), "StateDirectory"); !reflect.DeepEqual(got, []string{"mmhotline-backup"}) {
		t.Fatalf("StateDirectory=%q", got)
	}
	if got := unitValues(string(unit), "StateDirectoryMode"); !reflect.DeepEqual(got, []string{"0700"}) {
		t.Fatalf("StateDirectoryMode=%q", got)
	}
	command := unitValues(string(unit), "ExecStart")
	if len(command) != 4 || !reflect.DeepEqual(command[1:3], []string{"replicate", "-config"}) {
		t.Fatalf("ExecStart=%q", command)
	}
	configPath := command[3]
	contents, err := os.ReadFile(i.path(configPath))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		DBs []struct {
			Path     string `yaml:"path"`
			MetaPath string `yaml:"meta-path"`
			Replica  struct {
				URL string `yaml:"url"`
			} `yaml:"replica"`
		} `yaml:"dbs"`
	}
	if err := yaml.Unmarshal(contents, &config); err != nil || len(config.DBs) != 1 {
		t.Fatalf("%s: %v\n%s", configPath, err, contents)
	}
	database := config.DBs[0]
	if database.Path != "/var/lib/mmhotline/mmhotline.db" || database.Replica.URL != "${MMHOTLINE_BACKUP_REPLICA_URL}" {
		t.Fatalf("%s:\n%s", configPath, contents)
	}
	if strings.HasPrefix(database.MetaPath, installStateDir+"/") || !strings.HasPrefix(database.MetaPath, "/var/lib/mmhotline-backup/") {
		t.Fatalf("Litestream's working files are at %q", database.MetaPath)
	}
	if litestreamCapturedChangesDirectory != database.MetaPath+"/ltx/0" {
		t.Fatalf("the check reads %q; Litestream writes %q", litestreamCapturedChangesDirectory, database.MetaPath+"/ltx/0")
	}
	// Litestream reads its configuration as mmhotline-backup; only root changes it.
	info, err := os.Stat(i.path(configPath))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("%s: %v %v", configPath, info, err)
	}
}

// Root changes the owner and mode of the file it checked, even when the
// mmhotline user puts a symbolic link at the path between the check and the
// change: the link's target keeps its owner and mode.
func TestInstallMetadataNeverFollowsALinkPutAfterTheCheck(t *testing.T) {
	directory := t.TempDir()
	path, target := filepath.Join(directory, "mmhotline.db-wal"), filepath.Join(directory, "root-owned-file")
	for _, file := range []string{path, target} {
		if err := os.WriteFile(file, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	installMetadataChecked = func(checked string) {
		if checked != path {
			return
		}
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { installMetadataChecked = func(string) {} })
	changed, err := ensureInstallMetadata(path, 0600, os.Geteuid(), os.Getegid())
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	for file, want := range map[string]fs.FileMode{target: 0644, path + ".moved": 0600} {
		info, err := os.Lstat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", filepath.Base(file), info.Mode().Perm(), want)
		}
	}
	// A link already in place is refused, and its target is left alone.
	installMetadataChecked = func(string) {}
	if _, err := ensureInstallMetadata(path, 0600, os.Geteuid(), os.Getegid()); err == nil || !strings.Contains(err.Error(), "must not be a symbolic link") {
		t.Fatal(err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0644 {
		t.Fatal(info.Mode(), err)
	}
}

// A new server's empty database must never be saved over a bucket that
// holds the team's backup: install then writes the backup unit but does not
// start it, says how to restore or how to start empty, and fails, so a
// deploy tool stops too. Once the database exists, such as after the
// restore, install starts the backup as usual.
func TestInstallDoesNotBackUpANewDatabaseOverAnExistingBackup(t *testing.T) {
	for _, bucket := range []struct {
		name, listing string
		listErr       error
		want          string
	}{
		{"holds_a_backup", "level  min_txid          max_txid          size    created\n0      0000000000000001  0000000000000006  239985  2026-11-15T07:29:12Z\n", nil, "already holds a backup, up to transaction 0000000000000006"},
		{"cannot_be_listed", "", errors.New("Error: NoSuchBucket"), "could not list the bucket"},
	} {
		t.Run(bucket.name, func(t *testing.T) {
			i, f, output := installFixture(t)
			writeInstallFixture(t, i, installBackupSettingsPath, "MMHOTLINE_BACKUP_REPLICA_URL=s3://example/mmhotline\nLITESTREAM_SECRET_ACCESS_KEY=secret\n")
			i.listBackup = func(_ context.Context, litestream string, settings []string) (string, error) {
				if litestream != "/usr/local/bin/litestream" || !slices.Contains(settings, "MMHOTLINE_BACKUP_REPLICA_URL=s3://example/mmhotline") {
					t.Fatalf("listing with %s %q", litestream, settings)
				}
				return bucket.listing, bucket.listErr
			}
			err := i.run(t.Context())
			if err == nil || !strings.Contains(err.Error(), bucket.want) || !strings.Contains(err.Error(), "back-up-and-restore-the-database.md#restore") || !strings.Contains(err.Error(), "a folder that holds no backup") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("install: %v", err)
			}
			if exists, _ := installFileExists(i.path("/etc/systemd/system/mmhotline-backup.service")); !exists {
				t.Fatal("backup unit not written, so the restore block cannot stop and start it")
			}
			if f.enabled["mmhotline-backup"] || f.active["mmhotline-backup"] {
				t.Fatal("backup started on a new database")
			}
			// Running install again, such as after fixing backup.env, must
			// not start the backup on the same empty database.
			if err := i.run(t.Context()); err == nil || !strings.Contains(err.Error(), bucket.want) {
				t.Fatalf("second install: %v", err)
			}
			if f.enabled["mmhotline-backup"] || f.active["mmhotline-backup"] {
				t.Fatal("backup started on the new database by a second install")
			}
			// The restore block replaces the state directory with the
			// restored database; install then starts the backup without
			// listing the bucket.
			if err := os.RemoveAll(i.path(installStateDir)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(i.path(installStateDir), 0770); err != nil {
				t.Fatal(err)
			}
			if err := app.Setup(t.Context(), i.path(installStateDir), "Restored team"); err != nil {
				t.Fatal(err)
			}
			i.listBackup = func(context.Context, string, []string) (string, error) {
				t.Fatal("bucket listed for a restored database")
				return "", nil
			}
			output.Reset()
			if err := i.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !f.enabled["mmhotline-backup"] || !f.active["mmhotline-backup"] {
				t.Fatalf("backup not started after the restore: %s", output)
			}
		})
	}
}

// A deploy tool that rebuilds a server, such as the Stack's rebuild of
// util01, restores mmhotline.db from the backup before install runs. Install
// did not create that database, so it starts the backup at once, even
// though the bucket holds a backup, and never lists the bucket.
func TestInstallBacksUpADatabaseRestoredBeforeInstall(t *testing.T) {
	i, f, output := installFixture(t)
	writeInstallFixture(t, i, installBackupSettingsPath, "MMHOTLINE_BACKUP_REPLICA_URL=s3://example/mmhotline\nLITESTREAM_SECRET_ACCESS_KEY=secret\n")
	if err := os.MkdirAll(i.path(installStateDir), 0770); err != nil {
		t.Fatal(err)
	}
	if err := app.Setup(t.Context(), i.path(installStateDir), "Restored team"); err != nil {
		t.Fatal(err)
	}
	i.listBackup = func(context.Context, string, []string) (string, error) {
		t.Fatal("bucket listed for a database restored before install")
		return "", nil
	}
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Database: mmhotline.db already exists; kept") || !f.enabled["mmhotline-backup"] || !f.active["mmhotline-backup"] {
		t.Fatalf("restored database not backed up: %s", output)
	}
}

// Starting empty: once backup.env names a folder that holds no backup, the
// new database's check passes, install starts the backup, and later
// installs no longer list the bucket.
func TestInstallStartsTheBackupOfANewDatabaseInAnEmptyFolder(t *testing.T) {
	i, f, _ := installFixture(t)
	writeInstallFixture(t, i, installBackupSettingsPath, "MMHOTLINE_BACKUP_REPLICA_URL=s3://example/mmhotline\nLITESTREAM_SECRET_ACCESS_KEY=secret\n")
	i.listBackup = func(context.Context, string, []string) (string, error) {
		return "level  min_txid          max_txid          size    created\n0      0000000000000001  0000000000000006  239985  2026-11-15T07:29:12Z\n", nil
	}
	if err := i.run(t.Context()); err == nil {
		t.Fatal("new database backed up over a backup")
	}
	writeInstallFixture(t, i, installBackupSettingsPath, "MMHOTLINE_BACKUP_REPLICA_URL=s3://example/mmhotline-new\nLITESTREAM_SECRET_ACCESS_KEY=secret\n")
	i.listBackup = func(context.Context, string, []string) (string, error) {
		return "level  min_txid  max_txid  size  created\n", nil
	}
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.enabled["mmhotline-backup"] || !f.active["mmhotline-backup"] {
		t.Fatal("backup not started in the empty folder")
	}
	i.listBackup = func(context.Context, string, []string) (string, error) {
		t.Fatal("bucket listed after the new database's check passed")
		return "", nil
	}
	if err := i.run(t.Context()); err != nil {
		t.Fatal(err)
	}
}
