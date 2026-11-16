package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

// Limits for custom schedule code. They are measured on the smallest
// production machine before launch.
const (
	customScheduleCodeMaxBytes           = 64 * 1024
	customScheduleMaxTestCases           = 50
	customScheduleMaxStepsPerAnswer      = 100_000
	customScheduleRunWallClockLimit      = 5 * time.Second
	customScheduleRunCPUSecondsLimit     = 6
	customScheduleGoMemoryLimitBytes     = 192 << 20
	customScheduleOpenFilesLimit         = 8
	customScheduleStdoutLimitBytes       = 1 << 20
	customScheduleStderrLimitBytes       = 16 << 10
	customScheduleSandboxRunsAtOnce      = 2
	customScheduleSandboxRequestMaxBytes = 4 << 20
	// The child exits with this code when it cannot lock itself down; the
	// run fails closed.
	customScheduleSandboxExitLockdownFailed = 3
	customScheduleSandboxExitBadRequest     = 4
)

// CustomScheduleSandboxCommand is the hidden subcommand the mmhotline binary
// runs itself as.
const CustomScheduleSandboxCommand = "custom-schedule-sandbox"

// Customer-visible kinds of failure that the parent detects itself.
const (
	customScheduleErrorTookTooLong    = "took_too_long"
	customScheduleErrorTooMuchMemory  = "too_much_memory"
	customScheduleErrorTestCaseFailed = "test_case_failed"
)

// CustomScheduleCodeProblem is a mistake in customer code (or its test
// cases). Its Error text is what the administrator reads.
type CustomScheduleCodeProblem struct {
	Kind    string
	Message string
	Line    int
	// AskedAt is the moment the code was asked about, in the schedule
	// timezone; zero when the problem is not about one moment.
	AskedAt time.Time
}

func (p *CustomScheduleCodeProblem) Error() string {
	when := ""
	if !p.AskedAt.IsZero() {
		when = p.AskedAt.Format("Mon 2006-01-02 15:04 MST")
	}
	var text string
	switch p.Kind {
	case customScheduleErrorSyntax, customScheduleErrorRuntime:
		if p.Line > 0 {
			text = fmt.Sprintf("Your code has a mistake on line %d: %s", p.Line, p.Message)
		} else {
			text = "Your code has a mistake: " + p.Message
		}
	case customScheduleErrorTooMuchWork:
		if when == "" {
			return "Your code did too much work before on_call was asked anything. Make it simpler."
		}
		return fmt.Sprintf("Your code did too much work for %s. Make it simpler.", when)
	case customScheduleErrorTookTooLong:
		return "Your code took too long to run."
	case customScheduleErrorTooMuchMemory:
		return "Your code used too much memory."
	case customScheduleErrorUnknownSignInEmailAddress:
		text = "on_call returned a sign-in email address that is not a team member's sign-in email address: " + p.Message
	default:
		text = p.Message
	}
	if when != "" {
		if !strings.HasSuffix(text, ".") {
			text += "."
		}
		text += " This happened for " + when + "."
	}
	return text
}

// errCustomScheduleSandboxFailed means MMHotline, not the customer code,
// failed: for example seccomp could not be installed. The details go to the
// server administrator's log, never to the customer.
var errCustomScheduleSandboxFailed = errors.New("custom schedule code could not be run")

// RunCustomScheduleSandbox is `mmhotline custom-schedule-sandbox`: the child
// process that runs customer code. It reads one request, locks itself down,
// answers, and exits.
func RunCustomScheduleSandbox(stdin io.Reader, stdout, stderr io.Writer) int {
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(customScheduleGoMemoryLimitBytes)
	// The local timezone would be read from /etc/localtime on first use,
	// which the lockdown forbids. The child never needs it.
	time.Local = time.UTC
	if err := limitCustomScheduleSandboxResources(); err != nil {
		fmt.Fprintf(stderr, "custom schedule sandbox: resource limits: %v\n", err)
		return customScheduleSandboxExitLockdownFailed
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, customScheduleSandboxRequestMaxBytes+1))
	if err != nil || len(raw) > customScheduleSandboxRequestMaxBytes {
		fmt.Fprintln(stderr, "custom schedule sandbox: unreadable request")
		return customScheduleSandboxExitBadRequest
	}
	var request customScheduleSandboxRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil || request.MaxStepsPerAnswer == 0 {
		fmt.Fprintln(stderr, "custom schedule sandbox: invalid request")
		return customScheduleSandboxExitBadRequest
	}
	// Timezone files can only be read before the lockdown.
	zones := customScheduleTimeZones(request.TimeZone, request.Code)
	if err = lockDownCustomScheduleSandbox(); err != nil {
		fmt.Fprintf(stderr, "custom schedule sandbox: lockdown: %v\n", err)
		return customScheduleSandboxExitLockdownFailed
	}
	response := evaluateCustomScheduleCode(request, zones)
	if err = json.NewEncoder(stdout).Encode(response); err != nil {
		return 1
	}
	return 0
}

var customScheduleTimeZoneName = regexp.MustCompile(`^(UTC|[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){1,2})$`)
var customScheduleQuotedText = regexp.MustCompile(`"([^"\\\n]{1,64})"|'([^'\\\n]{1,64})'`)

// customScheduleTimeZones loads the schedule timezone and every timezone
// name written in the code as quoted text, so at.in_time_zone works after
// the lockdown. At most 50 names are tried.
func customScheduleTimeZones(scheduleTimeZone, code string) map[string]*time.Location {
	zones := map[string]*time.Location{}
	if loc, err := time.LoadLocation(scheduleTimeZone); err == nil {
		zones[scheduleTimeZone] = loc
	}
	var names []string
	for _, match := range customScheduleQuotedText.FindAllStringSubmatch(code, -1) {
		names = append(names, match[1]+match[2])
	}
	tried := 0
	for _, name := range names {
		if zones[name] != nil || !customScheduleTimeZoneName.MatchString(name) || tried >= 50 {
			continue
		}
		tried++
		if loc, err := time.LoadLocation(name); err == nil {
			zones[name] = loc
		}
	}
	return zones
}

// customScheduleSandboxSlots bounds CPU and memory use to two concurrent runs.
type customScheduleSandboxSlots struct{ slots chan struct{} }

func newCustomScheduleSandboxSlots() *customScheduleSandboxSlots {
	return &customScheduleSandboxSlots{slots: make(chan struct{}, customScheduleSandboxRunsAtOnce)}
}

func (s *customScheduleSandboxSlots) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// limitedBuffer keeps at most max bytes and remembers that more arrived.
type limitedBuffer struct {
	max        int
	buffer     bytes.Buffer
	overflowed bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.max - b.buffer.Len()
	if len(p) > room {
		b.overflowed = true
		if room > 0 {
			b.buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buffer.Write(p)
}

// runCustomScheduleSandbox runs one request in a fresh child process of the
// mmhotline binary. A mistake in the customer code comes back as a
// *CustomScheduleCodeProblem; any other error means the run itself failed.
func (a *App) runCustomScheduleSandbox(ctx context.Context, request customScheduleSandboxRequest) (customScheduleSandboxResponse, error) {
	var response customScheduleSandboxResponse
	body, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	release, err := a.customScheduleSandboxSlots.acquire(ctx)
	if err != nil {
		return response, err
	}
	defer release()
	executable, err := os.Executable()
	if err != nil {
		return response, fmt.Errorf("%w: %v", errCustomScheduleSandboxFailed, err)
	}
	runContext, cancel := context.WithTimeout(ctx, customScheduleRunWallClockLimit)
	defer cancel()
	stdout := &limitedBuffer{max: customScheduleStdoutLimitBytes}
	stderr := &limitedBuffer{max: customScheduleStderrLimitBytes}
	command := exec.CommandContext(runContext, executable, CustomScheduleSandboxCommand)
	command.Env = []string{}
	command.Dir = "/"
	command.Stdin = bytes.NewReader(body)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = customScheduleSandboxProcessAttributes()
	// The child runs in its own process group; the whole group is killed.
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	runError := command.Run()
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return response, ctx.Err()
	}
	if errors.Is(runContext.Err(), context.DeadlineExceeded) {
		return response, &CustomScheduleCodeProblem{Kind: customScheduleErrorTookTooLong}
	}
	if runError != nil {
		var exit *exec.ExitError
		if errors.As(runError, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() && (status.Signal() == syscall.SIGXCPU || status.Signal() == syscall.SIGKILL) {
				return response, &CustomScheduleCodeProblem{Kind: customScheduleErrorTookTooLong}
			}
			if exit.ExitCode() == 2 && (strings.Contains(stderr.buffer.String(), "out of memory") || strings.Contains(stderr.buffer.String(), "cannot allocate memory")) {
				return response, &CustomScheduleCodeProblem{Kind: customScheduleErrorTooMuchMemory}
			}
			// With Go 1.26's Green Tea garbage collector, RLIMIT_DATA refusing
			// the runtime's memory sometimes ends the child with a segmentation
			// fault inside the runtime instead of "out of memory". Nothing else
			// in the child can cause one: Starlark and the child are plain Go,
			// without cgo or unsafe code. The server administrator still sees the crash.
			if exit.ExitCode() == 2 && strings.Contains(stderr.buffer.String(), "SIGSEGV: segmentation violation") {
				_ = a.customScheduleSandboxFailure("segmentation fault, counted as using too much memory", stderr)
				return response, &CustomScheduleCodeProblem{Kind: customScheduleErrorTooMuchMemory}
			}
		}
		return response, a.customScheduleSandboxFailure(fmt.Sprintf("child failed: %v", runError), stderr)
	}
	if stdout.overflowed {
		return response, a.customScheduleSandboxFailure("standard output over the limit", stderr)
	}
	response, problem, err := readCustomScheduleSandboxResponse(request, stdout.buffer.Bytes())
	if err != nil {
		return response, a.customScheduleSandboxFailure(err.Error(), stderr)
	}
	if problem != nil {
		return response, problem
	}
	return response, nil
}

// readCustomScheduleSandboxResponse reads the child's standard output: one
// JSON response and nothing after it. A mistake in the customer code comes
// back as a problem; an error means the child broke the protocol. Either
// way the response is empty.
func readCustomScheduleSandboxResponse(request customScheduleSandboxRequest, output []byte) (customScheduleSandboxResponse, *CustomScheduleCodeProblem, error) {
	var response customScheduleSandboxResponse
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return customScheduleSandboxResponse{}, nil, errors.New("undecodable response")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return customScheduleSandboxResponse{}, nil, errors.New("data after the response")
	}
	problem, err := validateCustomScheduleSandboxResponse(request, response)
	if err != nil || problem != nil {
		return customScheduleSandboxResponse{}, problem, err
	}
	return response, nil, nil
}

// customScheduleSandboxFailure writes the details, including the child's
// standard error, to the server administrator's log (the service's standard error).
func (a *App) customScheduleSandboxFailure(what string, stderr *limitedBuffer) error {
	fmt.Fprintf(os.Stderr, "mmhotline: custom schedule sandbox: %s; child standard error: %q\n", what, stderr.buffer.String())
	return fmt.Errorf("%w: %s", errCustomScheduleSandboxFailed, what)
}

// validateCustomScheduleSandboxResponse never trusts the child: every answer
// must name a team member's sign-in email address or None, and every error must be well formed.
func validateCustomScheduleSandboxResponse(request customScheduleSandboxRequest, response customScheduleSandboxResponse) (*CustomScheduleCodeProblem, error) {
	loc, err := time.LoadLocation(request.TimeZone)
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		e := response.Error
		if len(response.Answers) != 0 || len(response.TestCaseAnswers) != 0 {
			return nil, errors.New("both answers and an error")
		}
		switch e.Kind {
		case customScheduleErrorSyntax, customScheduleErrorRuntime, customScheduleErrorTooMuchWork, customScheduleErrorBadResult, customScheduleErrorUnknownSignInEmailAddress:
		default:
			return nil, errors.New("unknown error kind")
		}
		if e.Line < 0 || e.Line > 1_000_000 {
			return nil, errors.New("invalid error line")
		}
		problem := &CustomScheduleCodeProblem{Kind: e.Kind, Message: truncateRunes(e.Message, 500), Line: e.Line}
		if e.AskedTime != "" {
			at, err := time.Parse(time.RFC3339, e.AskedTime)
			if err != nil {
				return nil, errors.New("invalid error time")
			}
			problem.AskedAt = at.In(loc)
		}
		return problem, nil
	}
	if len(response.Answers) != len(request.AskedTimes) || len(response.TestCaseAnswers) != len(request.TestCases) {
		return nil, errors.New("wrong number of answers")
	}
	known := map[string]bool{}
	for _, signInEmailAddress := range request.TeamMemberSignInEmailAddresses {
		known[signInEmailAddress] = true
	}
	for _, answers := range [][]customScheduleAnswer{response.Answers, response.TestCaseAnswers} {
		for _, answer := range answers {
			for _, signInEmailAddress := range []*string{answer.PrimarySignInEmailAddress, answer.BackupSignInEmailAddress} {
				if signInEmailAddress != nil && !known[*signInEmailAddress] {
					return nil, errors.New("answer names a sign-in email address that is not a team member's sign-in email address")
				}
			}
		}
	}
	return nil, nil
}
