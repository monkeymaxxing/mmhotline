package triageagent

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeConnectProxy stands in for the run's bridge to MMHotline's logging
// proxy on a loopback port. It answers each CONNECT with answer, and after a
// 200 echoes what it receives until the client stops sending. It reports each
// request line.
func fakeConnectProxy(t *testing.T, answer string) (address string, requestLines <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	lines := make(chan string, 4)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				reader := bufio.NewReader(connection)
				line, _ := reader.ReadString('\n')
				lines <- strings.TrimSpace(line)
				for {
					header, err := reader.ReadString('\n')
					if err != nil || header == "\r\n" {
						break
					}
				}
				io.WriteString(connection, answer)
				if strings.HasPrefix(answer, "HTTP/1.1 200") {
					io.Copy(connection, reader)
				}
			}()
		}
	}()
	return listener.Addr().String(), lines
}

func runSSHProxy(args ...string) (exitCode int, stdout, stderr string) {
	var output, errorOutput bytes.Buffer
	exitCode = RunSSHProxyCommand(args, strings.NewReader("SSH-2.0-OpenSSH_10.0\r\n"), &output, &errorOutput)
	return exitCode, output.String(), errorOutput.String()
}

// After the proxy's 200, SSH's bytes reach the server and the server's come
// back, through the address in the run's HTTPS_PROXY.
func TestSSHProxyCarriesBothWays(t *testing.T) {
	address, requestLines := fakeConnectProxy(t, "HTTP/1.1 200 Connection established\r\n\r\n")
	t.Setenv(RequestSocketEnvironmentVariable, "/tmp/run/request.sock")
	t.Setenv("HTTPS_PROXY", "http://"+address)
	exitCode, stdout, stderr := runSSHProxy("app001", "22")
	if exitCode != 0 || stdout != "SSH-2.0-OpenSSH_10.0\r\n" || stderr != "" {
		t.Fatalf("exit %d, standard output %q, standard error %q", exitCode, stdout, stderr)
	}
	if line := <-requestLines; line != "CONNECT app001:22 HTTP/1.1" {
		t.Fatalf("request line %q", line)
	}
}

// A refusal gives SSH the proxy's reason on standard error and exit code 255.
func TestSSHProxyRefusal(t *testing.T) {
	body := "MMHotline refused the connection: app009:22 is not a listed host; an administrator can add it on MMHotline's triage agent page.\n"
	address, _ := fakeConnectProxy(t, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
	t.Setenv(RequestSocketEnvironmentVariable, "/tmp/run/request.sock")
	t.Setenv("HTTPS_PROXY", "http://"+address)
	exitCode, stdout, stderr := runSSHProxy("app009", "22")
	if exitCode != 255 || stdout != "" || stderr != body {
		t.Fatalf("exit %d, standard output %q, standard error %q", exitCode, stdout, stderr)
	}
}

// Outside a run the command refuses before connecting anywhere, even when a
// person's HTTPS_PROXY names a proxy.
func TestSSHProxyOutsideARun(t *testing.T) {
	address, requestLines := fakeConnectProxy(t, "HTTP/1.1 200 Connection established\r\n\r\n")
	t.Setenv(RequestSocketEnvironmentVariable, "")
	t.Setenv("HTTPS_PROXY", "http://"+address)
	exitCode, stdout, stderr := runSSHProxy("app001", "22")
	if exitCode != 255 || stdout != "" || !strings.Contains(stderr, "works only inside a triage agent run") {
		t.Fatalf("exit %d, standard output %q, standard error %q", exitCode, stdout, stderr)
	}
	select {
	case line := <-requestLines:
		t.Fatalf("connected outside a run: %q", line)
	default:
	}
}

// An address is refused before any connection, with what to do instead: the
// proxy lists each server by name, never by its address. A name still goes
// to the proxy.
func TestSSHProxyRefusesAnAddress(t *testing.T) {
	address, requestLines := fakeConnectProxy(t, "HTTP/1.1 200 Connection established\r\n\r\n")
	t.Setenv(RequestSocketEnvironmentVariable, "/tmp/run/request.sock")
	t.Setenv("HTTPS_PROXY", "http://"+address)
	for _, host := range []string{"10.20.2.7", "[fd00::7]", "2001:db8::7"} {
		exitCode, stdout, stderr := runSSHProxy(host, "22")
		if want := "mmhotline: connect to a server by the server's inventory name, such as ssh db07; MMHotline lists each inventory server by name, not by IP address\n"; exitCode != 255 || stdout != "" || stderr != want {
			t.Errorf("%s: exit %d, standard output %q, standard error %q", host, exitCode, stdout, stderr)
		}
	}
	select {
	case line := <-requestLines:
		t.Fatalf("an address reached the proxy: %q", line)
	default:
	}
	if exitCode, _, stderr := runSSHProxy("db07", "22"); exitCode != 0 || stderr != "" {
		t.Fatalf("db07: exit %d, standard error %q", exitCode, stderr)
	}
	if line := <-requestLines; line != "CONNECT db07:22 HTTP/1.1" {
		t.Fatalf("request line %q", line)
	}
}

// fakeSSH writes an ssh into a new directory that records its arguments, one
// per line, in the file arguments beside it, and exits 7. It returns the
// directory.
func fakeSSH(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	script := "#!/bin/sh\nfor argument in \"$@\"; do printf '%s\\n' \"$argument\"; done > '" + filepath.Join(directory, "arguments") + "'\nexit 7\n"
	if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

// runSSHCommand runs the test binary as mmhotline triage-agent ssh with args,
// with PATH at the directory of a fake ssh, as the agent's ssh sets it, and
// the request socket set when insideARun. It returns the exit code, the
// arguments the fake ssh got, or nil when it did not run, and the output.
func runSSHCommand(t *testing.T, insideARun bool, args ...string) (exitCode int, sshArguments []string, stdout, stderr string) {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := fakeSSH(t)
	command := exec.Command(testBinary, append([]string{"triage-agent", SSHAction}, args...)...)
	requestSocket := ""
	if insideARun {
		requestSocket = "/tmp/run/request.sock"
	}
	command.Env = []string{"PATH=" + directory, RequestSocketEnvironmentVariable + "=" + requestSocket}
	var output, errorOutput bytes.Buffer
	command.Stdout, command.Stderr = &output, &errorOutput
	_ = command.Run()
	if recorded, err := os.ReadFile(filepath.Join(directory, "arguments")); err == nil {
		sshArguments = strings.Split(string(recorded), "\n")
		sshArguments = sshArguments[:len(sshArguments)-1]
	}
	return command.ProcessState.ExitCode(), sshArguments, output.String(), errorOutput.String()
}

// The agent's ssh drops each -F and its file, which would replace the
// agent's ~/.ssh/config and its ProxyCommand, says so on standard error, and
// runs the real ssh with everything else. It reads ssh's options as OpenSSH
// does: before the destination and right after it, never in the remote
// command.
func TestSSHCommandIgnoresAConfigFile(t *testing.T) {
	notice := "mmhotline: ignored -F ssh/config: in a triage agent run, ssh uses MMHotline's SSH settings to reach the inventory servers through MMHotline's logging proxy\n"
	for _, test := range []struct {
		args, want []string
		notice     string
	}{
		{[]string{"-F", "ssh/config", "triage@db07"}, []string{"triage@db07"}, notice},
		{[]string{"-Fssh/config", "triage@db07"}, []string{"triage@db07"}, notice},
		{[]string{"-vF", "ssh/config", "db07"}, []string{"-v", "db07"}, notice},
		{[]string{"-o", "BatchMode=yes", "-F", "ssh/config", "db07", "uptime"}, []string{"-o", "BatchMode=yes", "db07", "uptime"}, notice},
		// ssh reads options right after the destination too.
		{[]string{"db07", "-F", "ssh/config", "uptime"}, []string{"db07", "uptime"}, notice},
		// The remote command's -F is grep's.
		{[]string{"db07", "grep", "-F", "x", "/var/log/syslog"}, []string{"db07", "grep", "-F", "x", "/var/log/syslog"}, ""},
		{[]string{"-v", "--", "db07", "grep", "-F", "x"}, []string{"-v", "--", "db07", "grep", "-F", "x"}, ""},
		// Options that take a value keep it, even one that looks like -F.
		{[]string{"-l", "triage", "-p", "22", "-i", "key", "-E", "-F", "db07", "uptime"}, []string{"-l", "triage", "-p", "22", "-i", "key", "-E", "-F", "db07", "uptime"}, ""},
		{[]string{"-oUser=triage", "-p22", "db07"}, []string{"-oUser=triage", "-p22", "db07"}, ""},
	} {
		exitCode, sshArguments, stdout, stderr := runSSHCommand(t, true, test.args...)
		// The exit code is the fake ssh's own: the action became ssh.
		if exitCode != 7 || !slices.Equal(sshArguments, test.want) || stdout != "" || stderr != test.notice {
			t.Errorf("ssh %q: exit %d, ssh got %q, standard output %q, standard error %q; want ssh to get %q", test.args, exitCode, sshArguments, stdout, stderr, test.want)
		}
	}
}

// Outside a run the agent's ssh refuses and runs no ssh.
func TestSSHCommandOutsideARun(t *testing.T) {
	exitCode, sshArguments, _, stderr := runSSHCommand(t, false, "-F", "ssh/config", "db07")
	if exitCode != 255 || sshArguments != nil || !strings.Contains(stderr, "mmhotline triage-agent ssh works only inside a triage agent run") {
		t.Fatalf("exit %d, ssh got %q, standard error %q", exitCode, sshArguments, stderr)
	}
}

// codexFinishes ends a fake Codex run with a session and an answer.
const codexFinishes = codexSession + `
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"Done."}}' '{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3}}'`

// systemsRepositoryBundle is a bundle of one commit of a systems repository
// with ssh/known_hosts, as MMHotline sends a run, and that commit's ID.
func systemsRepositoryBundle(t *testing.T) (bundle []byte, baseCommitID string) {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command("git", append([]string{"-c", "user.name=Barbara", "-c", "user.email=barbara@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		command.Dir = directory
		output, err := command.Output()
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet", "--initial-branch=main")
	if err := os.MkdirAll(filepath.Join(directory, "ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ssh", "known_hosts"), []byte("203.0.113.11 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPsX9pp+bDzWfdnB2oQs5Hfkdg86mfoXg/fTLpSaAVrE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "--all")
	git("commit", "--quiet", "-m", "Describe the servers")
	path := filepath.Join(t.TempDir(), "main.bundle")
	git("bundle", "create", "--quiet", path, "main")
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bundle, git("rev-parse", "HEAD")
}

// effectiveSSHSettings is what OpenSSH's ssh makes of the settings file for
// one host: each setting's values, by ssh -G, which connects nowhere.
func effectiveSSHSettings(t *testing.T, ssh, settingsFile, host string) map[string][]string {
	t.Helper()
	output, err := exec.Command(ssh, "-F", settingsFile, "-G", host).Output()
	if err != nil {
		t.Fatalf("ssh -G %s: %v", host, err)
	}
	settings := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		key, value, _ := strings.Cut(line, " ")
		settings[key] = append(settings[key], value)
	}
	return settings
}

// Every run writes the agent's ~/.ssh/config before the coding agent starts,
// replacing what an earlier run left: every host goes through the logging
// proxy as the triage user with the triage key, with no prompt and no unknown
// host key. In the systems repository the host keys come from the copy's
// ssh/known_hosts, where each inventory server's key is under its address.
func TestSandboxWritesTheAgentsSSHSettings(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("this test reads the settings with OpenSSH's ssh, which is not installed")
	}
	// results is outside the agent's home, which each run clears, as the
	// run's private /tmp is.
	home, results := t.TempDir(), t.TempDir()
	settingsFile := filepath.Join(home, ".ssh", "config")
	// The first run, a setup test, keeps what it was given and then changes
	// it: a symlink to settings that trust any host key.
	first := startFakeRunInHome(t, home, "codex", `/bin/cp "$HOME/.ssh/config" '`+results+`/first_run_ssh_config'
printf 'Host *\n  StrictHostKeyChecking no\n' > '`+results+`/changed_ssh_config'
/bin/ln -sf '`+results+`/changed_ssh_config' "$HOME/.ssh/config"
`+codexFinishes, testRequest("codex"))
	for event := first.next(t); event.Event == "session" || event.Event == "step"; event = first.next(t) {
	}
	first.finish(t)

	bundle, base := systemsRepositoryBundle(t)
	request := testRequest("codex")
	request.SystemsRepositoryBaseCommitID, request.SystemsRepositoryBundle = base, bundle
	request.InventoryServers = []InventoryServer{{ServerName: "app001", HostKeyAlias: "203.0.113.11"}, {ServerName: "db01", HostKeyAlias: "203.0.113.21"}}
	second := startFakeRunInHome(t, home, "codex", codexFinishes, request)
	for event := second.next(t); event.Event == "session" || event.Event == "step"; event = second.next(t) {
	}
	second.finish(t)

	if info, err := os.Lstat(settingsFile); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("the second run's ~/.ssh/config: %v %v", info, err)
	}
	if changed := readTestFile(t, filepath.Join(results, "changed_ssh_config")); changed != "Host *\n  StrictHostKeyChecking no\n" {
		t.Fatalf("the file the first run's symlink named was changed: %q", changed)
	}
	wantEverywhere := map[string]string{
		"proxycommand":          testMMHotlineProgram(t) + " triage-agent ssh-proxy %h %p",
		"user":                  "triage",
		"identityfile":          filepath.Join(home, ".ssh", "id_ed25519"),
		"stricthostkeychecking": "true",
		"batchmode":             "yes",
	}
	check := func(run, settingsFile, host, hostKeyAlias, knownHosts string) {
		t.Helper()
		actual := effectiveSSHSettings(t, ssh, settingsFile, host)
		for key, value := range wantEverywhere {
			if got := actual[key]; len(got) != 1 || got[0] != value {
				t.Errorf("%s, %s: %s is %q, want %q", run, host, key, got, value)
			}
		}
		if got := actual["hostkeyalias"]; hostKeyAlias == "" && len(got) != 0 || hostKeyAlias != "" && (len(got) != 1 || got[0] != hostKeyAlias) {
			t.Errorf("%s, %s: hostkeyalias is %q, want %q", run, host, got, hostKeyAlias)
		}
		if knownHosts != "" {
			if got := actual["userknownhostsfile"]; len(got) != 1 || got[0] != knownHosts {
				t.Errorf("%s, %s: userknownhostsfile is %q, want %q", run, host, got, knownHosts)
			}
		}
	}
	check("the setup test", filepath.Join(results, "first_run_ssh_config"), "app001", "", "")
	if strings.Contains(readTestFile(t, filepath.Join(results, "first_run_ssh_config")), "UserKnownHostsFile") {
		t.Error("the setup test, which has no copy of the systems repository, names a known hosts file")
	}
	knownHosts := filepath.Join(home, "systems_repository", "ssh", "known_hosts")
	check("the run in the systems repository", settingsFile, "app001", "203.0.113.11", knownHosts)
	check("the run in the systems repository", settingsFile, "db01", "203.0.113.21", knownHosts)
	check("the run in the systems repository", settingsFile, "jobs001", "", knownHosts)
}

// Every triage run puts its own ssh first on the agent's search path,
// replacing what an earlier run left, also in bash's login shells, in which
// Codex runs each command and whose login files set PATH anew. That ssh runs
// mmhotline triage-agent ssh, which runs the ssh on the run's own search
// path without -F.
func TestSandboxPutsTheAgentsSSHFirst(t *testing.T) {
	home := t.TempDir()
	// The real ssh, on the run's search path, records its arguments.
	if err := os.WriteFile(filepath.Join(fakeSearchPath(t, home), "ssh"), []byte("#!/bin/sh\nfor argument in \"$@\"; do printf '%s\\n' \"$argument\"; done > \"$HOME/ssh-arguments\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The first run, a setup test, changes its ssh and the file BASH_ENV names.
	first := startFakeRunInHome(t, home, "codex", `printf '#!/bin/sh\necho changed\n' > "$HOME/.mmhotline/bin/ssh"
printf 'PATH=/usr/bin:/bin\n' > "$BASH_ENV"
`+codexFinishes, testRequest("codex"))
	for event := first.next(t); event.Event == "session" || event.Event == "step"; event = first.next(t) {
	}
	first.finish(t)

	second := startFakeRunInHome(t, home, "codex", `command -v ssh > "$HOME/ssh-found"
/bin/bash -lc 'command -v ssh' > "$HOME/ssh-found-by-a-login-shell"
ssh -F ssh/config triage@db01 uptime 2> "$HOME/ssh-errors"
`+codexFinishes, testRequest("codex"))
	for event := second.next(t); event.Event == "session" || event.Event == "step"; event = second.next(t) {
	}
	second.finish(t)

	agentSSH := filepath.Join(home, ".mmhotline", "bin", "ssh")
	if found := readTestFile(t, filepath.Join(home, "ssh-found")); found != agentSSH+"\n" {
		t.Errorf("the agent's first ssh is %q, want %s", found, agentSSH)
	}
	if found := readTestFile(t, filepath.Join(home, "ssh-found-by-a-login-shell")); found != agentSSH+"\n" {
		t.Errorf("a login shell's first ssh is %q, want %s", found, agentSSH)
	}
	if arguments := readTestFile(t, filepath.Join(home, "ssh-arguments")); arguments != "triage@db01\nuptime\n" {
		t.Errorf("the real ssh got %q", arguments)
	}
	if standardError := readTestFile(t, filepath.Join(home, "ssh-errors")); standardError != "mmhotline: ignored -F ssh/config: in a triage agent run, ssh uses MMHotline's SSH settings to reach the inventory servers through MMHotline's logging proxy\n" {
		t.Errorf("standard error %q", standardError)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
