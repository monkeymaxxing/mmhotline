package triageagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"mmhotline/internal/systemsrepository"
)

// processTerminationGracePeriod is how long a stopped coding agent may take to
// end after SIGTERM before SIGKILL.
var processTerminationGracePeriod = 10 * time.Second

// ErrRunFailed is returned after the run sent its failed event, so the
// sandboxed process exits non-zero and systemd records the failure.
var ErrRunFailed = errors.New("the triage agent run failed")

// SandboxSettings are the paths a run uses inside its sandbox.
type SandboxSettings struct {
	ProxySocketPath string
	Home            string
	SearchPath      string
	// MMHotlineProgram is this mmhotline program's own path, which the
	// agent's SSH settings run as their ProxyCommand.
	MMHotlineProgram string
}

type eventWriter struct {
	mutex   sync.Mutex
	encoder *json.Encoder
}

func (writer *eventWriter) write(event Event) error {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.encoder.Encode(event)
}

// RunInsideSandbox serves one run on systemd's connected standard input and
// output. The caller supplies the agent's writable home and executable path.
func RunInsideSandbox(ctx context.Context, in io.Reader, out io.Writer, settings SandboxSettings) error {
	writer := &eventWriter{encoder: json.NewEncoder(out)}
	// Set once the agent's copy of the systems repository is ready; from then
	// on the final event carries the agent's commits, however the run ends.
	var agentCopy, baseCommitID string
	var relay *requestRelay
	var signIn *chatGPTSignInFile
	final := func(event Event) error {
		if relay != nil {
			relay.end(runEndedMessage)
		}
		if signIn != nil {
			signIn.end()
		}
		if agentCopy != "" {
			event = withAgentCommits(context.WithoutCancel(ctx), event, agentCopy, baseCommitID)
		}
		if err := writer.write(event); err != nil {
			return err
		}
		if event.Event == "failed" {
			return ErrRunFailed
		}
		return nil
	}
	fail := func(err error) error { return final(Event{Event: "failed", Reason: err.Error()}) }
	reader := bufio.NewReader(in)
	requestLines := make(chan lineReadResult, 1)
	go func() {
		line, err := readRunLine(reader, MaximumLineSize)
		requestLines <- lineReadResult{line: line, err: err}
	}()
	var line []byte
	select {
	case <-ctx.Done():
		return fail(ctx.Err())
	case result := <-requestLines:
		if result.err != nil {
			return fail(fmt.Errorf("read triage agent request: %w", result.err))
		}
		line = result.line
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fail(fmt.Errorf("invalid triage agent request: %w", err))
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fail(errors.New("triage agent request must be one JSON object"))
	}
	if len(line)-base64.StdEncoding.EncodedLen(len(request.SystemsRepositoryBundle)) > maximumLineSizeWithoutBundle {
		return fail(errors.New("triage agent request exceeds 1 MiB excluding the systems repository bundle"))
	}
	if err := checkRequest(request); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	agent := findCodingAgent(request.CodingAgent)
	args := agent.command(request) //nolint:nilaway // checkRequest, above, refuses an unknown coding agent, so findCodingAgent finds one.
	program, err := findProgram(args[0], settings.SearchPath, agent.info().Title)
	if err != nil {
		return fail(err)
	}
	home, workingDirectory := settings.Home, settings.Home
	reviewAgent := request.AgentRole == ReviewAgentRole
	if reviewAgent || request.NewEmptyHome {
		// The run's private /tmp: only this run sees it, and systemd discards
		// it when the run ends. The agent's home, which every triage run can
		// write, stays out of the way of a review agent and a commit review.
		if home, err = os.MkdirTemp("/tmp", "mmhotline-"+strings.ReplaceAll(request.AgentRole, "_", "-")+"-home-"); err != nil {
			return fail(fmt.Errorf("make the run's new empty home: %w", err))
		}
		defer os.RemoveAll(home)
		workingDirectory = home
	} else if err := clearAgentHome(home); err != nil {
		return fail(err)
	}
	if request.SystemsRepositoryBaseCommitID != "" {
		// In the run's home: Claude Code reads the CLAUDE.md files of the
		// working directory's parents.
		directory := filepath.Join(home, systemsrepository.DirectoryName)
		if err := systemsrepository.PrepareAgentCopy(ctx, directory, request.SystemsRepositoryBundle, request.SystemsRepositoryBaseCommitID); err != nil {
			return fail(fmt.Errorf("prepare the agent's copy of the systems repository: %w", err))
		}
		agentCopy, baseCommitID, workingDirectory = directory, request.SystemsRepositoryBaseCommitID, directory
		request.SystemsRepositoryBundle = nil
	}
	// A review run leaves the agent's SSH settings and its ssh alone: the
	// triage run that waits for it uses them. Its search path holds nothing
	// the triage agent can change. The SSH settings stay in the agent's home,
	// where ssh looks; the agent's ssh goes in the run's home, so a commit
	// review's search path is new too.
	environment := []string{"PATH=" + settings.SearchPath}
	if !reviewAgent {
		if err := writeSSHSettings(settings.Home, settings.MMHotlineProgram, agentCopy, request.InventoryServers); err != nil {
			return fail(fmt.Errorf("write the agent's SSH settings: %w", err))
		}
		sshDirectory, bashEnvironmentFile, err := writeAgentSSH(home, settings.MMHotlineProgram, settings.SearchPath)
		if err != nil {
			return fail(fmt.Errorf("write the agent's ssh: %w", err))
		}
		environment = []string{"PATH=" + sshDirectory + string(os.PathListSeparator) + settings.SearchPath, "BASH_ENV=" + bashEnvironmentFile}
	}
	proxyContext, cancelProxy := context.WithCancel(ctx)
	defer cancelProxy()
	listener, err := listenForProxy(proxyContext, settings.ProxySocketPath, request.ProxyToken)
	if err != nil {
		return fail(fmt.Errorf("listen for connections to the coding agent's proxy: %w", err))
	}
	defer listener.Close()
	proxyURL := "http://" + listener.Addr().String()
	command := exec.Command(program, args[1:]...)
	command.Dir = workingDirectory
	command.Env = append(environment, "HOME="+home, "LANG=C.UTF-8",
		"HTTPS_PROXY="+proxyURL, "HTTP_PROXY="+proxyURL, "https_proxy="+proxyURL, "http_proxy="+proxyURL)
	// A review agent only answers its prompt, so it gets no relay: it cannot
	// ask for a playbook run or send a report to people.
	if !reviewAgent {
		if relay, err = startRequestRelay(ctx, writer, agentCopy, baseCommitID); err != nil {
			return fail(fmt.Errorf("listen for the agent's requests: %w", err))
		}
		defer relay.close()
		command.Env = append(command.Env, RequestSocketEnvironmentVariable+"="+relay.socketPath)
	}
	command.Env = append(command.Env, agent.env(request, home)...)
	if request.CredentialType == "chatgpt_sign_in" {
		if signIn, err = writeChatGPTSignIn(home, request.Credential, writer); err != nil {
			return fail(fmt.Errorf("write Codex's auth.json: %w", err))
		}
		// final ends it; this covers a run socket that broke first.
		defer signIn.end()
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdin = strings.NewReader(request.Prompt)
	standardOutput, outputPipe, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	defer standardOutput.Close()
	defer outputPipe.Close()
	command.Stdout = outputPipe
	standardError := &stderrTail{}
	command.Stderr = standardError
	if err := command.Start(); err != nil {
		return fail(fmt.Errorf("start %s: %w", agent.info().Title, err))
	}
	_ = outputPipe.Close()
	if signIn != nil {
		signIn.watch()
	}
	runDone := make(chan struct{})
	defer close(runDone)
	// Also clean up descendants if writing to the run socket fails.
	// The group is usually gone by then, so the kill's error says nothing.
	defer func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }()
	processExit := make(chan error, 1)
	go func() { processExit <- command.Wait() }()
	lines := make(chan lineReadResult)
	go readAgentOutput(standardOutput, lines, runDone)
	stopRequests := make(chan struct{}, 1)
	go readControls(reader, stopRequests, relay, runDone)
	stream := &agentStream{}
	stopRequested, terminating := false, false
	var failure error
	var exitError error
	outputEnded, processEnded := false, false
	contextDone := ctx.Done()
	for !outputEnded || !processEnded {
		if !terminating && (failure != nil || stopRequested && len(stream.activeSteps) == 0) {
			terminating = true
			terminateProcessGroup(command.Process.Pid, runDone)
		}
		select {
		case <-contextDone:
			failure = fmt.Errorf("the coding agent run was ended before the coding agent finished: %w", ctx.Err())
			contextDone = nil
		case <-stopRequests:
			stopRequested = true
			// The command waiting for an answer is the agent's current step,
			// which the stop waits for.
			relay.end(runStoppingMessage)
		case result, ok := <-lines:
			if !ok {
				outputEnded = true
				lines = nil
				continue
			}
			if result.err != nil {
				failure = fmt.Errorf("read %s output: %w", agent.info().Title, result.err)
				continue
			}
			if terminating || stream.final != nil {
				continue
			}
			events, err := agent.readLine(result.line, stream)
			if err != nil {
				failure = fmt.Errorf("read %s output: %w", agent.info().Title, err)
				continue
			}
			for _, event := range events {
				if err := writer.write(event); err != nil {
					return err
				}
			}
		case exitError = <-processExit:
			processEnded = true
			processExit = nil
			// A command left waiting could hold the agent's output open.
			relay.end(runEndedMessage)
		}
	}
	if failure != nil {
		return fail(failure)
	}
	if stopRequested {
		if stream.sessionID == "" {
			return final(Event{Event: "failed", Reason: "the coding agent run was stopped before the coding agent reported a session ID", StoppedBeforeSessionID: true})
		}
		return final(Event{Event: "stopped", SessionID: stream.sessionID})
	}
	if stream.final != nil && stream.final.Event == "failed" {
		return final(*stream.final)
	}
	if exitError != nil {
		reason := fmt.Sprintf("%s exited unexpectedly: %v", agent.info().Title, exitError)
		if tail := standardError.text(); tail != "" {
			reason += "; standard error: " + tail
		}
		return fail(errors.New(reason))
	}
	if stream.final == nil && stream.lastError != "" {
		return fail(errors.New(stream.lastError))
	}
	if stream.final == nil {
		return fail(fmt.Errorf("%s exited without reporting a result", agent.info().Title))
	}
	return final(*stream.final)
}

// withAgentCommits adds the agent's commits to a run's final event. Commits
// that cannot be bundled fail the run with the reason, so a person learns of
// them before the next run resets the agent's copy.
func withAgentCommits(ctx context.Context, event Event, agentCopy, baseCommitID string) Event {
	bundle, err := systemsrepository.AgentCommitsBundle(ctx, agentCopy, baseCommitID)
	if err == nil {
		event.AgentCommitsBundle = bundle
		return event
	}
	reason := "the agent's commits could not be sent to MMHotline: " + err.Error()
	if event.Event == "failed" {
		reason = event.Reason + "; " + reason
	}
	return Event{Event: "failed", SessionID: event.SessionID, Reason: reason}
}

func checkRequest(request Request) error {
	if request.Action != "start" && request.Action != "resume" {
		return errors.New("triage agent request action must be start or resume")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return errors.New("a coding agent prompt is required")
	}
	if request.Action == "resume" && strings.TrimSpace(request.SessionID) == "" {
		return errors.New("resume requires the coding agent's session ID")
	}
	if (request.SystemsRepositoryBaseCommitID == "") != (len(request.SystemsRepositoryBundle) == 0) {
		return errors.New("systems_repository_base_commit_id and systems_repository_bundle must be supplied together")
	}
	if err := CheckSetup(request.CodingAgent, request.CredentialType, request.Credential); err != nil {
		return err
	}
	if err := CheckModel(request.Model); err != nil {
		return fmt.Errorf("model %w", err)
	}
	if err := CheckReasoningEffort(request.ReasoningEffort); err != nil {
		return fmt.Errorf("reasoning_effort %w", err)
	}
	if !proxyTokenPattern.MatchString(request.ProxyToken) {
		return errors.New("proxy_token must be 32 lowercase hexadecimal characters")
	}
	switch request.AgentRole {
	case TriageAgentRole:
		if request.NewEmptyHome && request.Action != "start" {
			return errors.New("a triage agent run in a new empty home only starts; its home holds no session to resume")
		}
	case ReviewAgentRole:
		// Its home is new and empty, so it has no session to resume.
		if request.Action != "start" {
			return errors.New("a review agent run only starts; its new empty home holds no session to resume")
		}
		if request.SystemsRepositoryBaseCommitID != "" || len(request.InventoryServers) > 0 {
			return errors.New("a review agent run has no systems repository and no inventory servers")
		}
	default:
		return errors.New("agent_role must be triage_agent or review_agent")
	}
	return nil
}

// clearAgentHome removes, at the start of a run in the agent's home,
// everything in that home except the team's SSH key and Git settings and
// each supported coding agent's session files. A run that a log misled can
// write any file in its home, and a coding agent reads some of them at every
// start: instructions, settings with hooks, skills, memories, and MCP
// servers. Clearing the whole home, not a list of known files, means a
// settings file that a newer coding agent version reads also cannot carry
// over to the next run
// (docs/changing-mmhotline/design/triage-agent.md#what-a-run-leaves-for-the-next).
// Every supported agent's session files stay, since the team may switch
// agents. MMHotline makes the systems repository copy, ~/.ssh/config, and
// ~/.mmhotline new for each run.
func clearAgentHome(home string) error {
	kept := []string{".ssh", ".gitconfig"}
	for _, agent := range codingAgents() {
		kept = append(kept, agent.sessionDirectory())
	}
	return clearDirectoryExcept(home, "", kept)
}

// clearDirectoryExcept removes each entry of root/directory whose path,
// relative to root, is not kept and leads to nothing kept. A directory on
// the way to a kept path is cleared the same way; anything else on the way,
// such as a symbolic link, is removed.
func clearDirectoryExcept(root, directory string, kept []string) error {
	entries, err := os.ReadDir(filepath.Join(root, directory))
	if err != nil {
		return fmt.Errorf("clear the agent's home: %w", err)
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if slices.Contains(kept, path) {
			continue
		}
		if entry.IsDir() && slices.ContainsFunc(kept, func(keptPath string) bool { return strings.HasPrefix(keptPath, path+"/") }) {
			if err := clearDirectoryExcept(root, path, kept); err != nil {
				return err
			}
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, path)); err != nil {
			return fmt.Errorf("remove %s from the agent's home: %w", path, err)
		}
	}
	return nil
}

// proxyTokenPattern is MMHotline's run ID format, which proxy tokens share.
var proxyTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// readRunLine reads one line of at most limit bytes, including its newline.
func readRunLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, fmt.Errorf("triage agent request or control exceeds %d bytes", limit)
		}
		line = append(line, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		return line, nil
	}
}

// readControls reads MMHotline's control lines until the connection closes,
// which counts as a stop: nobody receives the run's results any more.
func readControls(reader *bufio.Reader, stops chan<- struct{}, relay *requestRelay, done <-chan struct{}) {
	for {
		line, err := readRunLine(reader, maximumLineSizeWithoutBundle)
		var control Control
		if err == nil && json.Unmarshal(line, &control) == nil && control.Answer != nil {
			relay.deliver(*control.Answer)
		}
		if err != nil || control.Stop {
			// One stop waiting in the channel is enough.
			select {
			case stops <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

type lineReadResult struct {
	line []byte
	err  error
}

func readAgentOutput(reader io.Reader, lines chan<- lineReadResult, done <-chan struct{}) {
	defer close(lines)
	buffered := bufio.NewReader(reader)
	for {
		line, err := buffered.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) != 0 {
			select {
			case lines <- lineReadResult{line: line}:
			case <-done:
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				select {
				case lines <- lineReadResult{err: err}:
				case <-done:
				}
			}
			return
		}
	}
}

func terminateProcessGroup(pid int, done <-chan struct{}) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	grace := processTerminationGracePeriod
	go func() {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		case <-done:
		}
	}()
}

func findProgram(name, searchPath, title string) (string, error) {
	var directories []string
	for _, directory := range filepath.SplitList(searchPath) {
		if directory == "" {
			continue
		}
		directories = append(directories, directory)
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return filepath.Abs(path)
		}
	}
	places := strings.Join(directories, ", ")
	if len(directories) == 2 {
		places = directories[0] + " or " + directories[1]
	}
	if len(directories) > 2 {
		places = strings.Join(directories[:len(directories)-1], ", ") + ", or " + directories[len(directories)-1]
	}
	if places == "" {
		places = "the empty search path"
	}
	return "", fmt.Errorf("%s was not found in %s; install %s in a directory on the run's search path", name, places, title)
}

type stderrTail struct {
	mutex sync.Mutex
	data  []byte
}

func (tail *stderrTail) Write(data []byte) (int, error) {
	tail.mutex.Lock()
	defer tail.mutex.Unlock()
	length := len(data)
	if len(data) >= 4096 {
		tail.data = append(tail.data[:0], data[len(data)-4096:]...)
	} else {
		tail.data = append(tail.data, data...)
		if len(tail.data) > 4096 {
			tail.data = tail.data[len(tail.data)-4096:]
		}
	}
	return length, nil
}

func (tail *stderrTail) text() string {
	tail.mutex.Lock()
	defer tail.mutex.Unlock()
	return string(tail.data)
}
