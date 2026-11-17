package triageagent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// The agent reads the team's servers over SSH. SSH has no proxy environment
// variable, so the agent's ~/.ssh/config, which each run writes, names
// mmhotline triage-agent ssh-proxy as its ProxyCommand; that command carries
// SSH through the run's bridge to MMHotline's logging proxy:
// docs/changing-mmhotline/design/triage-agent.md#reading-the-servers-over-ssh. An -F names
// other settings in place of that file, so each triage run also puts its own
// ssh first on the agent's search path, which drops -F:
// docs/changing-mmhotline/design/triage-agent.md#the-agents-ssh.

// sshSettingsHeader starts the agent's ~/.ssh/config.
const sshSettingsHeader = `# Written by MMHotline at the start of every triage agent run. Each run
# replaces this whole file, so a run cannot leave changed SSH settings for the
# next run. Every connection goes through MMHotline's logging proxy, which
# lists each server of the systems repository's inventories at port 22.
`

// sshSettingsWord is a server name or host key alias that ssh_config reads as
// one plain word, so no space, pattern character, or quote changes the file.
var sshSettingsWord = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// writeSSHSettings replaces the agent's ~/.ssh/config at the start of a run:
// every host through the logging proxy, as the triage user, with the triage
// key the team's server setup made, and never a prompt. In the systems
// repository, ssh checks host keys against the copy's ssh/known_hosts, which
// holds each server's key under the inventory's address. The setup test has
// no copy and so no known hosts of its own.
func writeSSHSettings(home, mmhotlineProgram, agentCopy string, servers []InventoryServer) error {
	var settings strings.Builder
	settings.WriteString(sshSettingsHeader)
	for _, server := range servers {
		if sshSettingsWord.MatchString(server.ServerName) && sshSettingsWord.MatchString(server.HostKeyAlias) {
			fmt.Fprintf(&settings, "Host %s\n  HostKeyAlias %s\n", server.ServerName, server.HostKeyAlias)
		}
	}
	fmt.Fprintf(&settings, "Host *\n  ProxyCommand %s triage-agent %s %%h %%p\n  User triage\n  IdentityFile %s\n", mmhotlineProgram, SSHProxyAction, filepath.Join(home, ".ssh", "id_ed25519"))
	if agentCopy != "" {
		fmt.Fprintf(&settings, "  UserKnownHostsFile %s\n", filepath.Join(agentCopy, "ssh", "known_hosts"))
	}
	settings.WriteString("  StrictHostKeyChecking yes\n  BatchMode yes\n")
	return replaceFile(filepath.Join(home, ".ssh"), "config", settings.String(), 0o600)
}

// replaceFile writes content as directory/name with mode. A new file renamed
// over the old one also replaces a symlink a run left there.
func replaceFile(directory, name, content string, mode os.FileMode) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(directory, name))
}

// agentSSHHeader starts the agent's ssh.
const agentSSHHeader = `#!/bin/sh
# Written by MMHotline at the start of every triage agent run, first on the
# agent's search path. A copied command such as ssh -F ssh/config app001
# would replace the agent's ~/.ssh/config, whose ProxyCommand is the only way
# to the servers; mmhotline triage-agent ssh drops -F and runs the real ssh,
# which it finds on the run's own search path.
`

// bashEnvironmentHeader starts the file in BASH_ENV.
const bashEnvironmentHeader = `# Written by MMHotline at the start of every triage agent run. Bash reads this
# file, which BASH_ENV names, after its login files, which can set PATH anew:
# Codex runs each command in a login shell. It puts the agent's ssh first.
`

// writeAgentSSH replaces, at the start of a triage run, the agent's ssh, which
// runs mmhotline triage-agent ssh with PATH set back to searchPath, and the
// file for BASH_ENV, which puts the ssh first again in a bash login shell.
// It returns the ssh's directory, which goes first on the agent's search
// path, and that file. It empties the directory first: any other program an
// earlier run left there would run in place of the command of its name.
func writeAgentSSH(home, mmhotlineProgram, searchPath string) (directory, bashEnvironmentFile string, err error) {
	mmhotlineDirectory := filepath.Join(home, ".mmhotline")
	directory = filepath.Join(mmhotlineDirectory, "bin")
	if err = os.RemoveAll(directory); err != nil {
		return "", "", err
	}
	ssh := agentSSHHeader + "PATH=" + shellQuoted(searchPath) + "\nexport PATH\nexec " + shellQuoted(mmhotlineProgram) + " triage-agent " + SSHAction + " \"$@\"\n"
	if err = replaceFile(directory, "ssh", ssh, 0o700); err != nil {
		return "", "", err
	}
	bashEnvironment := bashEnvironmentHeader + "case \"$PATH\" in\n" + shellQuoted(directory+string(os.PathListSeparator)) + "*) ;;\n*) PATH=" + shellQuoted(directory) + "\":$PATH\" ;;\nesac\n"
	if err = replaceFile(mmhotlineDirectory, "bash_env", bashEnvironment, 0o600); err != nil {
		return "", "", err
	}
	return directory, filepath.Join(mmhotlineDirectory, "bash_env"), nil
}

// shellQuoted quotes text as one word for sh.
func shellQuoted(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// SSHAction is mmhotline triage-agent ssh, which the agent's ssh runs.
const SSHAction = "ssh"

// sshOptionsWithValue are the options of OpenSSH's ssh that take a value,
// from its getopt string
// "1246ab:c:e:fgi:kl:m:no:p:qstvxAB:CD:E:F:GI:J:KL:MNO:P:Q:R:S:TVw:W:XYy".
const sshOptionsWithValue = "BbcDEeFIiJLlmOoPpQRSWw"

// withoutSSHConfigFiles returns ssh's arguments without each -F and its file,
// and those files. It reads the options as OpenSSH's ssh does: before the
// destination, and once more right after it up to the first word that is not
// an option, where the remote command starts; "--" ends them. So the -F of a
// remote command, such as ssh db07 grep -F x /var/log/syslog, stays.
func withoutSSHConfigFiles(args []string) (kept, configFiles []string) {
	destinationSeen := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" || len(arg) < 2 || arg[0] != '-' {
			if arg == "--" || destinationSeen {
				return append(kept, args[index:]...), configFiles
			}
			destinationSeen = true
			kept = append(kept, arg)
			continue
		}
		// One word holds options without a value, such as -vq, and may end
		// with one that takes a value: the rest of the word, or else the
		// next word.
		position := strings.IndexAny(arg[1:], sshOptionsWithValue) + 1
		if position == 0 {
			kept = append(kept, arg)
			continue
		}
		value, words := arg[position+1:], 1
		if value == "" {
			if index+1 == len(args) {
				// The value is missing, which ssh refuses itself.
				kept = append(kept, arg)
				continue
			}
			value, words = args[index+1], 2
		}
		if arg[position] == 'F' {
			configFiles = append(configFiles, value)
			if position > 1 {
				kept = append(kept, arg[:position])
			}
		} else {
			kept = append(kept, args[index:index+words]...)
		}
		index += words - 1
	}
	return kept, configFiles
}

// RunSSHCommand runs mmhotline triage-agent ssh, which the agent's ssh runs
// in a triage run: it runs the ssh on PATH, which the agent's ssh set back to
// the run's own search path, with args less each -F and its file, so the
// agent's ~/.ssh/config applies. It returns only when it fails, with 255.
func RunSSHCommand(args []string, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "mmhotline: %s\n", err)
		return sshFailed
	}
	// Only a run's own ssh drops -F; outside a run, -F is a person's.
	if os.Getenv(RequestSocketEnvironmentVariable) == "" {
		return fail(fmt.Errorf("mmhotline triage-agent ssh works only inside a triage agent run; %s is not set", RequestSocketEnvironmentVariable))
	}
	program, err := findProgram("ssh", os.Getenv("PATH"), "OpenSSH's ssh")
	if err != nil {
		return fail(err)
	}
	kept, configFiles := withoutSSHConfigFiles(args)
	for _, file := range configFiles {
		fmt.Fprintf(stderr, "mmhotline: ignored -F %s: in a triage agent run, ssh uses MMHotline's SSH settings to reach the inventory servers through MMHotline's logging proxy\n", file)
	}
	err = syscall.Exec(program, append([]string{"ssh"}, kept...), os.Environ())
	return fail(fmt.Errorf("run %s: %w", program, err))
}

// SSHProxyAction is mmhotline triage-agent ssh-proxy, which SSH runs inside a
// triage agent run.
const SSHProxyAction = "ssh-proxy"

// sshFailed is the exit code of every failure of the agent's ssh and of
// ssh-proxy: ssh exits 255 on its own errors, and reports any failed
// ProxyCommand as a connection that closed.
const sshFailed = 255

const sshProxyHelp = `usage: mmhotline triage-agent ssh-proxy HOST PORT

SSH runs ssh-proxy as the ProxyCommand in the triage agent's ~/.ssh/config,
which MMHotline writes at the start of each run. ssh-proxy asks MMHotline's
logging proxy for a connection to HOST at PORT and carries SSH's data over
that connection. ssh-proxy works only inside a triage agent run. When the
logging proxy refuses, the reason goes to standard error and the exit code
is 255.
`

// RunSSHProxyCommand runs mmhotline triage-agent ssh-proxy with args and
// returns its exit code: 0 once the connection ended, 255 on any failure.
func RunSSHProxyCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "mmhotline: %s\n", err)
		return sshFailed
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-help" || args[0] == "-h") {
		fmt.Fprint(stdout, sshProxyHelp)
		return 0
	}
	if len(args) != 2 {
		return fail(errors.New("usage: mmhotline triage-agent ssh-proxy HOST PORT"))
	}
	// The request socket marks a run, as for the request commands; a person's
	// own HTTPS_PROXY outside a run must not carry SSH somewhere else.
	if os.Getenv(RequestSocketEnvironmentVariable) == "" {
		return fail(fmt.Errorf("mmhotline triage-agent ssh-proxy works only inside a triage agent run; %s is not set", RequestSocketEnvironmentVariable))
	}
	// The proxy lists each server by its name, never by its address, so the
	// proxy's own refusal, which says an administrator can list a host, would
	// point the agent the wrong way. ssh passes an IPv6 address without its
	// brackets, but a person may write them.
	if _, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(args[0], "["), "]")); err == nil {
		return fail(errors.New("connect to a server by the server's inventory name, such as ssh db07; MMHotline lists each inventory server by name, not by IP address"))
	}
	proxy, err := url.Parse(os.Getenv("HTTPS_PROXY"))
	if err != nil || proxy.Scheme != "http" || proxy.Host == "" {
		return fail(errors.New("HTTPS_PROXY must be an HTTP URL with a host for the triage agent run's connection to MMHotline's logging proxy"))
	}
	connection, err := net.Dial("tcp", proxy.Host)
	if err != nil {
		return fail(fmt.Errorf("cannot reach MMHotline's logging proxy: %w", err))
	}
	defer connection.Close()
	target := net.JoinHostPort(args[0], args[1])
	if _, err := fmt.Fprintf(connection, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return fail(fmt.Errorf("cannot ask MMHotline's logging proxy for %s: %w", target, err))
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return fail(fmt.Errorf("MMHotline's logging proxy did not answer: %w", err))
	}
	if response.StatusCode != http.StatusOK {
		// The proxy's body is one line: "MMHotline refused the connection: ...".
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		reason := strings.TrimSpace(string(body))
		if reason == "" {
			reason = "MMHotline's logging proxy refused the connection to " + target + ": " + response.Status
		}
		fmt.Fprintln(stderr, reason)
		return sshFailed
	}
	// SSH closes our standard input when it is done sending; the server may
	// still answer, so only the write half closes.
	go func() {
		_, _ = io.Copy(connection, stdin)
		_ = connection.(*net.TCPConn).CloseWrite()
	}()
	// The reader holds whatever arrived with the proxy's answer.
	_, _ = io.Copy(stdout, reader)
	return 0
}
