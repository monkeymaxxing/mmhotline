package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mmhotline/internal/app"
	"mmhotline/internal/systemsrepository"
)

// preparationGuideURL is the how-to page section that says how a team does,
// by hand, what triage-agent check-preparation checks; each missing check
// names its step there.
const preparationGuideURL = "https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository"

// triageAgentHome is the triage agent's home folder (mode 0700, owned by
// the triage agent's Unix user). The serve process runs as mmhotline in a
// sandbox and cannot read it, which is why the check runs as root in the
// command line program, like backup check-uploads.
const triageAgentHome = "/var/lib/mmhotline-triage-agent"

const triageAgentCheckPreparationUsage = `usage: sudo mmhotline triage-agent check-preparation [--json]
Checks the eight things a team prepares by hand before the triage agent
works, and prints one line for each: Present or Missing. A missing line names
the step of the how-to page that fixes it.
Run check-preparation as root on the MMHotline server. It reads the triage
agent's home folder and uses the triage agent's SSH key and MMHotline's SSH
key to log in to each server. It changes nothing and never prints a private
key, a token, or a credential from the settings.
       --json  Print {"checks":[{"number","name","present","detail","fix"}]}
       --help  Show help
Exit codes: 0 all eight are present; 1 at least one is missing;
3 cannot run, such as without root; 4 bad arguments.`

func preparationFix(step int) string {
	return preparationGuideURL + " step " + strconv.Itoa(step)
}

// preparationCheck is one line of the report.
type preparationCheck struct {
	Number  int    `json:"number"`
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix"`
}

func writePreparationReport(stdout io.Writer, asJSON bool, checks []preparationCheck) error {
	missing := 0
	for _, check := range checks {
		if !check.Present {
			missing++
		}
	}
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(struct {
			Checks []preparationCheck `json:"checks"`
		}{checks}); err != nil {
			return err
		}
	} else {
		for _, check := range checks {
			if check.Present {
				fmt.Fprintf(stdout, "Present: %d. %s: %s.\n", check.Number, check.Name, check.Detail)
			} else {
				fmt.Fprintf(stdout, "Missing: %d. %s: %s. Fix: %s.\n", check.Number, check.Name, check.Detail, check.Fix)
			}
		}
		if missing == 0 {
			fmt.Fprintf(stdout, "All %d checks are present.\n", len(checks))
		} else {
			verb := "are"
			if missing == 1 {
				verb = "is"
			}
			fmt.Fprintf(stdout, "%d of %d checks are present; %d %s missing.\n", len(checks)-missing, len(checks), missing, verb)
		}
	}
	if missing > 0 {
		return checkAnswer{1}
	}
	return nil
}

func requireRootForPreparation(effectiveUserID int) error {
	if effectiveUserID != 0 {
		return errors.New("triage-agent check-preparation reads the triage agent's home folder and logs in with two private keys, so it must run as root: sudo mmhotline triage-agent check-preparation")
	}
	return nil
}

func runTriageAgentCheckPreparation(args []string) error {
	flags := flag.NewFlagSet("triage-agent check-preparation", flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	flags.Usage = func() { fmt.Fprintln(flags.Output(), triageAgentCheckPreparationUsage) }
	asJSON := flags.Bool("json", false, "print one JSON object")
	if err := flags.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, flagOutput.String())
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return commandError{reportedFlagError{err}}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("triage-agent check-preparation accepts no positional arguments; see mmhotline triage-agent check-preparation --help")}
	}
	if err := requireRootForPreparation(os.Geteuid()); err != nil {
		fmt.Fprintf(os.Stderr, "mmhotline: %s\n", err)
		return checkAnswer{3}
	}
	p := preparation{
		copyDirectory: filepath.Join(installStateDir, systemsrepository.DirectoryName),
		agentHome:     triageAgentHome, mmhotlineHome: installStateDir, mmxSearchPath: app.MMXSearchPath,
		run: runPreparationCommand, lookupAddresses: net.LookupIP,
	}
	p.appliedSigningKey, p.agentSetUp, p.setupProblem = appliedTriageAgentSigningKey("/run/mmhotline/command.sock")
	return writePreparationReport(os.Stdout, *asJSON, p.checks())
}

// appliedTriageAgentSigningKey asks the running MMHotline, through the
// command socket as the other commands do, whether the triage agent is set
// up and which commit signing public key is applied. The key is public.
func appliedTriageAgentSigningKey(socket string) (key string, setUp bool, problem string) {
	unixUser := os.Getenv("SUDO_USER")
	if unixUser == "" {
		unixUser = "root"
	}
	result, err := sendCommand(socket, app.CommandRequest{Group: "triage-agent", Action: "show", UnixUser: unixUser})
	if err != nil {
		return "", false, "MMHotline did not answer triage-agent show: " + err.Error()
	}
	if result.TriageAgent == nil || result.TriageAgent.Setup == nil {
		return "", false, ""
	}
	return result.TriageAgent.CommitSigningPublicKey, true, ""
}

// The two Unix users the checks run programs as.
const (
	runAsRoot      = "root"
	runAsMMHotline = "mmhotline"
)

// preparationRunner runs a program as runAs in directory (the current one
// when empty) and returns what it printed. It is the one place that starts
// programs, so tests stand in their own.
type preparationRunner func(runAs, directory, program string, args ...string) (stdout, stderr string, err error)

// preparation holds the checks' inputs; tests use temporary folders.
type preparation struct {
	copyDirectory, agentHome, mmhotlineHome, mmxSearchPath string
	appliedSigningKey                                      string
	agentSetUp                                             bool
	setupProblem                                           string // why the applied setup could not be read
	run                                                    preparationRunner
	lookupAddresses                                        func(name string) ([]net.IP, error)
}

type preparationServer struct{ name, address, sshUser string }

const nodeRunnerAmd64, nodeRunnerArm64 = "mmx-runner-linux-amd64", "mmx-runner-linux-arm64"

// nodeRunnerProgramName names the MMX node runner for the processor that
// uname -m reports, or "" for a processor MMX has no node runner for.
func nodeRunnerProgramName(unameMachine string) string {
	switch strings.TrimSpace(unameMachine) {
	case "x86_64":
		return nodeRunnerAmd64
	case "aarch64":
		return nodeRunnerArm64
	}
	return ""
}

// isSSHRemoteURL says whether a git remote reaches its server over SSH: an
// ssh:// URL, or the scp-like form [user@]host:path. A folder path, file://,
// and https:// are not.
func isSSHRemoteURL(remote string) bool {
	if strings.HasPrefix(remote, "ssh://") {
		return true
	}
	return !strings.Contains(remote, "://") && scpLikeRemote.MatchString(remote)
}

var scpLikeRemote = regexp.MustCompile(`^([^@/:\s]+@)?[^@/:\s]+:[^\s/][^\s]*$|^([^@/:\s]+@)?[^@/:\s]+:/[^\s/][^\s]*$`)

// describeOrigin shows a remote for the report without any user name or
// password in a URL, as a token may be the user name of an https remote.
func describeOrigin(remote string) string {
	if parsed, err := url.Parse(remote); err == nil && strings.Contains(remote, "://") && parsed.User != nil {
		parsed.User = nil
		return parsed.String()
	}
	return remote
}

// sameSigningKey compares two public key lines by key type and key, the
// first two fields; the comment does not matter.
func sameSigningKey(a, b string) bool {
	first, second := strings.Fields(a), strings.Fields(b)
	return len(first) >= 2 && len(second) >= 2 && first[0] == second[0] && first[1] == second[1]
}

func lastLine(text string) string {
	last := ""
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); line != "" {
			last = line
		}
	}
	return last
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() //nolint:nilaway // info is read only when err is nil.
}

// checks runs the eight checks in order.
func (p preparation) checks() []preparationCheck {
	servers, serversProblem := p.listServers()
	serverChecks := func(check func(preparationServer) string) (problems []string) {
		for _, server := range servers {
			if problem := check(server); problem != "" {
				problems = append(problems, problem)
			}
		}
		return problems
	}
	noServers := func() string {
		if len(servers) > 0 {
			return ""
		}
		return "no server to check: " + serversProblem
	}
	sshOptions := func(server string, extra ...string) []string {
		return append([]string{"-F", "/dev/null", "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + p.knownHosts(), "-o", "ConnectTimeout=10"}, extra...)
	}
	var checks []preparationCheck
	add := func(number, step int, name, presentDetail string, problems ...string) {
		problems = slices.DeleteFunc(problems, func(problem string) bool { return problem == "" })
		check := preparationCheck{Number: number, Name: name, Present: len(problems) == 0, Detail: presentDetail}
		if !check.Present {
			check.Detail, check.Fix = strings.Join(problems, "; "), preparationFix(step)
		}
		checks = append(checks, check)
	}

	// 1. Server names resolve.
	var resolved []string
	problems := serverChecks(func(server preparationServer) string {
		addresses, err := p.lookupAddresses(server.name)
		if err != nil {
			return server.name + " does not resolve: " + err.Error()
		}
		for _, address := range addresses {
			if !address.IsLoopback() {
				resolved = append(resolved, server.name+" -> "+address.String())
				return ""
			}
		}
		return server.name + " resolves only to a loopback address"
	})
	if len(servers) > 0 {
		problems = append(problems, serversProblem) // some inventory could not be listed
	}
	add(1, 1, "Server names resolve", strings.Join(resolved, ", "), append(problems, noServers())...)

	// 2. The triage user accepts the agent's key. The processors feed check 8.
	agentKey := filepath.Join(p.agentHome, ".ssh", "id_ed25519")
	machines := map[string]string{}
	var accepted []string
	check2 := []string{noServers()}
	if len(servers) > 0 && !fileExists(agentKey) {
		check2 = append(check2, agentKey+" does not exist")
	} else if len(servers) > 0 {
		check2 = append(check2, serverChecks(func(server preparationServer) string {
			arguments := append(sshOptions(server.name, "-i", agentKey, "-o", "HostKeyAlias="+server.address), "triage@"+server.name, "uname", "-m")
			stdout, stderr, err := p.run(runAsRoot, "", "ssh", arguments...)
			if err != nil {
				return "could not log in to triage@" + server.name + " with the agent's key: " + lastLine(stderr)
			}
			machines[server.name] = strings.TrimSpace(stdout)
			accepted = append(accepted, server.name)
			return ""
		})...)
	}
	add(2, 2, "The triage user accepts the agent's key", "triage@ on "+strings.Join(accepted, ", ")+" accepts "+agentKey, check2...)

	// 3. The agent signs its commits.
	add(3, 3, "The agent signs its commits", "git signs with the applied commit signing key", p.signingProblem())

	// 4. The rules file.
	rules := filepath.Join(p.copyDirectory, "triage_agent_rules.md")
	rulesProblem := ""
	if !fileExists(rules) {
		rulesProblem = rules + " does not exist"
	}
	add(4, 4, "The rules file", rules+" exists", rulesProblem)

	// 5. Host keys.
	check5 := []string{noServers()}
	check5 = append(check5, serverChecks(func(server preparationServer) string {
		if _, _, err := p.run(runAsRoot, "", "ssh-keygen", "-F", server.address, "-f", p.knownHosts()); err != nil {
			return "no host key for " + server.name + " (" + server.address + ") in " + p.knownHosts()
		}
		return ""
	})...)
	add(5, 4, "Host keys of the servers are known", "every server has a host key in "+p.knownHosts(), check5...)

	// 6. MMHotline's own key reaches the servers.
	ownKey := filepath.Join(p.mmhotlineHome, ".ssh", "id_ed25519")
	check6 := []string{noServers()}
	if len(servers) > 0 && !fileExists(ownKey) {
		check6 = append(check6, ownKey+" does not exist")
	} else if len(servers) > 0 {
		check6 = append(check6, serverChecks(func(server preparationServer) string {
			login := cmp.Or(server.sshUser, "root") + "@" + server.address
			if _, stderr, err := p.run(runAsRoot, "", "ssh", append(sshOptions(server.name, "-i", ownKey), login, "true")...); err != nil {
				return "could not log in to " + login + " with MMHotline's key: " + lastLine(stderr)
			}
			return ""
		})...)
	}
	add(6, 5, "MMHotline's own key reaches the servers", ownKey+" logs in to every server", check6...)

	// 7. MMHotline's copy reaches its origin over SSH.
	add(7, 6, "MMHotline's copy reaches its origin over SSH", "origin is reachable with git ls-remote", p.originProblem())

	// 8. mmx and its node runners.
	var check8 []string
	var runners []string
	program, err := app.FindMMXProgram(p.mmxSearchPath)
	if err != nil {
		check8 = append(check8, err.Error())
	} else {
		runners = append(runners, program)
		check8 = append(check8, noServers())
		for _, server := range servers {
			machine, known := machines[server.name]
			if !known {
				check8 = append(check8, "the processor of "+server.name+" is unknown, because check 2 failed there")
				continue
			}
			runner := nodeRunnerProgramName(machine)
			if runner == "" {
				check8 = append(check8, server.name+" has the processor "+machine+", which MMX has no node runner for")
			} else if path := filepath.Join(filepath.Dir(program), runner); !fileExists(path) {
				check8 = append(check8, path+" does not exist (needed for "+server.name+", "+machine+")")
			} else if !slices.Contains(runners, path) {
				runners = append(runners, path)
			}
		}
	}
	add(8, 7, "mmx and its node runners are installed", strings.Join(runners, ", "), check8...)
	return checks
}

func (p preparation) knownHosts() string { return filepath.Join(p.copyDirectory, "ssh", "known_hosts") }

// listServers lists the servers of every inventory in MMHotline's copy, as
// MMHotline does at the start of a run: mmx nodes, as the mmhotline user.
func (p preparation) listServers() (servers []preparationServer, problem string) {
	inventories, _ := filepath.Glob(filepath.Join(p.copyDirectory, "inventories", "*.yml"))
	if _, err := os.Stat(p.copyDirectory); err != nil {
		return nil, "MMHotline's copy " + p.copyDirectory + " does not exist"
	}
	if len(inventories) == 0 {
		return nil, p.copyDirectory + "/inventories has no .yml file"
	}
	program, err := app.FindMMXProgram(p.mmxSearchPath)
	if err != nil {
		return nil, err.Error()
	}
	var problems []string
	for _, file := range inventories {
		name := strings.TrimSuffix(filepath.Base(file), ".yml")
		stdout, stderr, err := p.run(runAsMMHotline, p.copyDirectory, program, "nodes", "--inventory", name, "--json")
		if err != nil {
			problems = append(problems, "mmx nodes --inventory "+name+" failed: "+lastLine(stderr))
			continue
		}
		var listing struct {
			Nodes []struct {
				Name    string `json:"name"`
				Address string `json:"address"`
				SSHUser string `json:"ssh_user"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
			problems = append(problems, "mmx nodes --inventory "+name+" printed output that is not JSON")
			continue
		}
		for _, node := range listing.Nodes {
			if !slices.ContainsFunc(servers, func(listed preparationServer) bool { return listed.name == node.Name }) {
				servers = append(servers, preparationServer{node.Name, node.Address, node.SSHUser})
			}
		}
	}
	if len(servers) == 0 && len(problems) == 0 {
		problems = append(problems, "no inventory lists a server")
	}
	return servers, strings.Join(problems, "; ")
}

var signingSettingNames = regexp.MustCompile(`^(gpg\.format|commit\.gpgsign|user\.signingkey) `)

// signingProblem says what is wrong with the agent's git signing settings,
// or "" when git signs with the applied key.
func (p preparation) signingProblem() string {
	if !p.agentSetUp {
		return cmp.Or(p.setupProblem, "the triage agent is not set up, so no commit signing key is applied (see triage-agent show)")
	}
	if p.appliedSigningKey == "" {
		return "no triage_agent.commit_signing_public_key is applied"
	}
	gitConfig := filepath.Join(p.agentHome, ".gitconfig")
	if !fileExists(gitConfig) {
		return gitConfig + " does not exist"
	}
	stdout, stderr, err := p.run(runAsRoot, "", "git", "config", "--file", gitConfig, "--get-regexp", `^(gpg\.format|commit\.gpgsign|user\.signingkey)$`)
	if err != nil && stdout == "" {
		return "git cannot read " + gitConfig + ": " + lastLine(stderr)
	}
	settings := map[string]string{}
	for line := range strings.Lines(stdout) {
		if signingSettingNames.MatchString(line) {
			name, value, _ := strings.Cut(strings.TrimSpace(line), " ")
			settings[name] = value
		}
	}
	if settings["gpg.format"] != "ssh" {
		return gitConfig + " does not set gpg.format to ssh"
	}
	if !slices.Contains([]string{"true", "yes", "on", "1"}, strings.ToLower(settings["commit.gpgsign"])) {
		return gitConfig + " does not set commit.gpgsign to true"
	}
	keyFile := settings["user.signingkey"]
	if keyFile == "" || !strings.HasSuffix(keyFile, ".pub") {
		return gitConfig + " does not set user.signingkey to a .pub file"
	}
	if rest, found := strings.CutPrefix(keyFile, "~/"); found {
		keyFile = filepath.Join(p.agentHome, rest)
	}
	contents, err := os.ReadFile(keyFile)
	if err != nil {
		return "user.signingkey names " + keyFile + ", which cannot be read"
	}
	if !sameSigningKey(string(contents), p.appliedSigningKey) {
		return keyFile + " holds a different key than triage_agent.commit_signing_public_key"
	}
	return ""
}

// originProblem says what is wrong with MMHotline's copy reaching its
// origin over SSH, or "" when it does.
func (p preparation) originProblem() string {
	if _, err := os.Stat(p.copyDirectory); err != nil {
		return "MMHotline's copy " + p.copyDirectory + " does not exist"
	}
	stdout, stderr, err := p.run(runAsRoot, "", "git", "-c", "safe.directory="+p.copyDirectory, "-C", p.copyDirectory, "remote", "get-url", "origin")
	if err != nil {
		return "the copy has no origin: " + lastLine(stderr)
	}
	origin := strings.TrimSpace(stdout)
	if !isSSHRemoteURL(origin) {
		if strings.Contains(origin, "://") {
			return "origin is " + describeOrigin(origin) + ", not an SSH address"
		}
		return "origin is " + origin + ", a folder on this server"
	}
	if _, stderr, err := p.run(runAsMMHotline, p.copyDirectory, "git", "ls-remote", "--heads", "origin"); err != nil {
		return "git ls-remote " + describeOrigin(origin) + " as mmhotline failed: " + lastLine(stderr)
	}
	return ""
}

// runPreparationCommand starts a program for a check. Programs get no
// terminal, so ssh cannot wait for a password, and a short environment.
func runPreparationCommand(runAs, directory, program string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = directory
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	home := "/root"
	if runAs == runAsMMHotline {
		home = installStateDir
		account, err := user.Lookup(runAsMMHotline)
		if err != nil {
			return "", "", err
		}
		uid, _ := strconv.ParseUint(account.Uid, 10, 32)
		gid, _ := strconv.ParseUint(account.Gid, 10, 32)
		command.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	command.Env = []string{"HOME=" + home, "PATH=" + app.MMXSearchPath + ":/bin", "GIT_TERMINAL_PROMPT=0"}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}
