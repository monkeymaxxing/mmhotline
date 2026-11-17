package triageagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The agent asks MMHotline for a playbook run, sends its report, or gives
// its decision on a commit it reviews, with a command it runs inside the run;
// the command talks to the run's relay:
// docs/changing-mmhotline/design/triage-agent.md#requests-from-the-agent.

// RequestSocketEnvironmentVariable names the relay's socket in the coding
// agent's environment and every program it starts.
const RequestSocketEnvironmentVariable = "MMHOTLINE_TRIAGE_AGENT_REQUEST_SOCKET"

// MaximumMarkdownFileSize bounds a report and a playbook request's
// diagnosis, each a Markdown file the agent writes: a person reads it, and
// one run socket line carries it.
const MaximumMarkdownFileSize = 64 << 10

// MaximumPlaybookRunOutputForAgent bounds the playbook run output in an
// answer. The agent reads the answer into its context, where a long output
// crowds out the rest; the end holds MMX's summary and the error that stopped
// the run. MMHotline keeps the whole output.
const MaximumPlaybookRunOutputForAgent = 16 << 10

// The kinds of AgentRequest.
const (
	PlaybookRunRequest = "playbook_run"
	ReportRequest      = "report"
	// CommitReviewRequest is the agent's decision, with its report, on a
	// commit it reviews in a commit review run.
	CommitReviewRequest = "commit_review"
)

// The decisions of a CommitReviewRequest.
const (
	ApproveCommit      = "approve"
	CommitNeedsAPerson = "needs_a_person"
)

// AgentRequest is one line a request command sends to the run's relay.
type AgentRequest struct {
	Kind          string `json:"kind"`
	Playbook      string `json:"playbook,omitempty"`
	Inventory     string `json:"inventory,omitempty"`
	AlertSeverity string `json:"alert_severity,omitempty"`
	ReportText    string `json:"report_text,omitempty"`
	// Diagnosis is on a playbook run request: what the agent found, which is
	// the cause, the evidence, and why the playbook fixes it, in Markdown.
	Diagnosis string `json:"diagnosis,omitempty"`
	// CommitID and CommitReviewDecision are on a commit review request.
	// approve carries no alert severity, as MMHotline sends one info alert;
	// needs_a_person sends the report at its alert severity.
	CommitID             string `json:"commit_id,omitempty"`
	CommitReviewDecision string `json:"commit_review_decision,omitempty"`
}

var (
	inventoryNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	requestIDPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	// A full Git commit ID: SHA-1, or SHA-256 in a repository that uses it.
	commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// CheckAgentRequest refuses a request MMHotline cannot act on. The command,
// the run's relay, and MMHotline each check it: MMHotline never trusts the run.
func CheckAgentRequest(request AgentRequest) error {
	switch request.Kind {
	case PlaybookRunRequest:
		if err := CheckPlaybookPath(request.Playbook); err != nil {
			return err
		}
		if err := CheckInventoryName(request.Inventory); err != nil {
			return err
		}
		return checkMarkdownText("diagnosis", request.Diagnosis)
	case ReportRequest:
		if err := checkAlertSeverity(request.AlertSeverity); err != nil {
			return err
		}
		return checkMarkdownText("report", request.ReportText)
	case CommitReviewRequest:
		if !commitIDPattern.MatchString(request.CommitID) {
			return fmt.Errorf("the commit ID %s must be a full Git commit ID, 40 lowercase hexadecimal characters, as the prompt gives it", shortQuote(request.CommitID))
		}
		switch request.CommitReviewDecision {
		case ApproveCommit:
			if request.AlertSeverity != "" {
				return errors.New("approve takes no alert severity: MMHotline sends one info alert with your report")
			}
		case CommitNeedsAPerson:
			if err := checkAlertSeverity(request.AlertSeverity); err != nil {
				return fmt.Errorf("needs_a_person sends your report at its alert severity: %w", err)
			}
		default:
			return fmt.Errorf("the decision %s must be approve or needs_a_person", shortQuote(request.CommitReviewDecision))
		}
		return checkMarkdownText("report", request.ReportText)
	}
	return fmt.Errorf("unknown request kind %s; valid kinds: playbook_run, report, commit_review", shortQuote(request.Kind))
}

func checkAlertSeverity(severity string) error {
	if severity != "info" && severity != "warn" && severity != "sos" {
		return fmt.Errorf("the alert severity %s must be info, warn, or sos", shortQuote(severity))
	}
	return nil
}

// checkMarkdownText refuses a report or a diagnosis, named by what, that is
// empty or over MaximumMarkdownFileSize.
func checkMarkdownText(what, text string) error {
	if len(text) > MaximumMarkdownFileSize {
		return markdownFileTooLarge(what, int64(len(text)))
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("the " + what + " is empty")
	}
	return nil
}

func markdownFileTooLarge(what string, size int64) error {
	return fmt.Errorf("the %s is %d bytes, over the limit of %d bytes (64 KiB)", what, size, MaximumMarkdownFileSize)
}

// CheckPlaybookPath allows a path inside the systems repository only: MMX
// reads it in MMHotline's copy.
func CheckPlaybookPath(path string) error {
	if path == "" {
		return errors.New("a playbook path is required, such as one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml")
	}
	if length := utf8.RuneCountInString(path); length > 300 {
		return fmt.Errorf("the playbook path has %d characters; at most 300 are allowed", length)
	}
	if strings.ContainsFunc(path, unicode.IsControl) {
		return errors.New("the playbook path must be one line without control characters")
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("the playbook path %q must be relative to the systems repository, such as playbooks/deploy.yml", path)
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf(`the playbook path %q must not have an empty, ".", or ".." part`, path)
		}
	}
	if !strings.HasSuffix(path, ".yml") {
		return fmt.Errorf("the playbook path %q must end in .yml", path)
	}
	return nil
}

// CheckInventoryName allows an MMX inventory name, such as production.
func CheckInventoryName(name string) error {
	if name == "" {
		return errors.New("an inventory name is required")
	}
	if len(name) > 100 || !inventoryNamePattern.MatchString(name) {
		return fmt.Errorf(`the inventory name %s may have only letters, digits, "_", and "-", and at most 100 characters`, shortQuote(name))
	}
	return nil
}

// CheckRequestID refuses a request ID the run's relay would not assign.
func CheckRequestID(id string) error {
	if !requestIDPattern.MatchString(id) {
		return fmt.Errorf("the request ID %s must be 16 lowercase hexadecimal characters", shortQuote(id))
	}
	return nil
}

// shortQuote quotes the start of a value for a message, so a refused value
// of up to 1 MiB cannot make the answer exceed a run socket line.
func shortQuote(value string) string {
	if runes := []rune(value); len(runes) > 100 {
		return fmt.Sprintf("%q…", string(runes[:100]))
	}
	return fmt.Sprintf("%q", value)
}

var agentRequestCommandHelp = map[string]string{
	"request-playbook-run": `usage: mmhotline triage-agent request-playbook-run --playbook PATH --inventory NAME --diagnosis-file FILE [--json]

Asks MMHotline to run an MMX playbook from the systems repository on one
inventory, and waits for MMHotline's answer. The request-playbook-run command
works only inside a triage agent run.
Commit the playbook first. The request includes your commits, which MMHotline
pushes when the commits change nothing outside one_time_playbooks/ and
triage_agent_notes/. A change to a one-time playbook that someone else
changed last, including deleting or renaming it, waits for a person: write a
new playbook instead. MMHotline then checks whether the playbook may run.
When allowed, MMHotline runs the playbook and returns the end of the run's output.

FILE holds your diagnosis in Markdown, at most 64 KiB: what you found, the
cause, the evidence, and why the playbook fixes the problem. If the team has
a review agent, the review agent reads the diagnosis. People who approve
the playbook run also read the diagnosis.

Exit codes:
  0  the playbook ran and succeeded
  1  the playbook ran and failed; read the playbook run output
  3  the request waits for a person: end your turn and report
  4  the request was refused or is invalid, or the run is ending. When the
     review agent refused the playbook, read the refusal reason in the answer,
     revise the playbook, commit the new version, and ask again
`,
	"report": `usage: mmhotline triage-agent report --alert-severity info|warn|sos --report-file FILE [--json]

Sends your report to MMHotline, which records the report with the run. The
report command works only inside a triage agent run. FILE holds the report
in Markdown, at most 64 KiB.

Exit codes:
  0  MMHotline recorded the report
  4  the report was refused or is invalid, or the run is ending
`,
	"review-commit": `usage: mmhotline triage-agent review-commit --commit-id COMMIT_ID --decision approve|needs_a_person --report-file FILE [--alert-severity info|warn|sos] [--json]

Gives MMHotline your decision on a commit you reviewed, with your report. The
review-commit command works only in a commit review run. The prompt holds
the commits as data; COMMIT_ID is a full commit ID from the prompt.

approve marks the commit reviewed: MMX runs may then pass the commit, and
MMHotline sends one info alert with your report. Approve only an ordinary
change.

needs_a_person leaves the commit waiting for a person's review and sends your
report at --alert-severity, which needs_a_person requires: sos when the
commit changes who can get in or looks like nobody on the team made it.

FILE holds the report in Markdown, at most 64 KiB.

Exit codes:
  0  MMHotline recorded your decision
  4  the decision was refused or is invalid, or the run is ending. When
     MMHotline refused approve, decide needs_a_person and report
`,
}

// IsAgentRequestCommand says whether mmhotline triage-agent ACTION is one of
// the commands the agent runs inside a run.
func IsAgentRequestCommand(action string) bool {
	_, found := agentRequestCommandHelp[action]
	return found
}

// RunAgentRequestCommand runs mmhotline triage-agent request-playbook-run,
// report, or review-commit with args and returns its exit code.
func RunAgentRequestCommand(action string, args []string, stdout, stderr io.Writer) int {
	refuse := func(err error) int {
		fmt.Fprintf(stderr, "mmhotline: %s\n", err)
		return 4
	}
	flags := flag.NewFlagSet("triage-agent "+action, flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	flags.Usage = func() {
		fmt.Fprint(&flagOutput, agentRequestCommandHelp[action]+"\n")
		flags.PrintDefaults()
	}
	request := AgentRequest{Kind: PlaybookRunRequest}
	var reportFile, diagnosisFile string
	switch action {
	case "report":
		request.Kind = ReportRequest
		flags.StringVar(&request.AlertSeverity, "alert-severity", "", "the report's alert severity: info, warn, or sos")
		flags.StringVar(&reportFile, "report-file", "", "file that holds the report in Markdown, at most 64 KiB")
	case "review-commit":
		request.Kind = CommitReviewRequest
		flags.StringVar(&request.CommitID, "commit-id", "", "full ID of the commit you reviewed, as the prompt gives it")
		flags.StringVar(&request.CommitReviewDecision, "decision", "", "approve or needs_a_person")
		flags.StringVar(&request.AlertSeverity, "alert-severity", "", "with needs_a_person only, the report's alert severity: info, warn, or sos")
		flags.StringVar(&reportFile, "report-file", "", "file that holds your report on the commit in Markdown, at most 64 KiB")
	default:
		flags.StringVar(&request.Playbook, "playbook", "", "playbook path relative to the systems repository, such as one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml or playbooks/deploy.yml")
		flags.StringVar(&request.Inventory, "inventory", "", "inventory name: NAME of an inventory file inventories/NAME.yml in the systems repository")
		flags.StringVar(&diagnosisFile, "diagnosis-file", "", "file that holds your diagnosis in Markdown, at most 64 KiB: what you found, the cause, the evidence, and why the playbook fixes the problem")
	}
	asJSON := flags.Bool("json", false, "print the answer as one JSON object")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, flagOutput.String())
			return 0
		}
		fmt.Fprint(stderr, "mmhotline: "+flagOutput.String())
		return 4
	}
	if flags.NArg() != 0 {
		return refuse(errors.New("unexpected positional arguments; use named flags for the triage agent request"))
	}
	socketPath := os.Getenv(RequestSocketEnvironmentVariable)
	if socketPath == "" {
		return refuse(fmt.Errorf("mmhotline triage-agent %s works only inside a triage agent run; %s is not set", action, RequestSocketEnvironmentVariable))
	}
	switch {
	case action == "review-commit" && request.CommitID == "":
		return refuse(errors.New("--commit-id is required"))
	case action == "review-commit" && request.CommitReviewDecision == "":
		return refuse(errors.New("--decision is required: approve or needs_a_person"))
	case action == "review-commit" && request.CommitReviewDecision == CommitNeedsAPerson && request.AlertSeverity == "":
		return refuse(errors.New("--alert-severity is required with needs_a_person"))
	case action == "report" && request.AlertSeverity == "":
		return refuse(errors.New("--alert-severity is required"))
	}
	if action != "request-playbook-run" {
		if reportFile == "" {
			return refuse(errors.New("--report-file is required"))
		}
		text, err := readMarkdownFile("report", reportFile)
		if err != nil {
			return refuse(err)
		}
		request.ReportText = text
	} else {
		if request.Playbook == "" {
			return refuse(errors.New("--playbook is required"))
		}
		if request.Inventory == "" {
			return refuse(errors.New("--inventory is required"))
		}
		if diagnosisFile == "" {
			return refuse(errors.New("--diagnosis-file is required: a Markdown file with what you found, the cause, the evidence, and why the playbook fixes the problem"))
		}
		text, err := readMarkdownFile("diagnosis", diagnosisFile)
		if err != nil {
			return refuse(err)
		}
		request.Diagnosis = text
	}
	if err := CheckAgentRequest(request); err != nil {
		return refuse(err)
	}
	answer, err := sendAgentRequest(socketPath, request)
	if err != nil {
		return refuse(err)
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(answer)
	} else {
		fmt.Fprintln(stdout, answer.Text())
	}
	return answer.ExitCode()
}

// readMarkdownFile reads the report or the diagnosis, named by what, from
// path. It reads at most one byte over the limit, so a large file named by
// mistake is refused with its size without being read.
func readMarkdownFile(what, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read the %s file: %w", what, err)
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > MaximumMarkdownFileSize {
		return "", markdownFileTooLarge(what, info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(file, MaximumMarkdownFileSize+1))
	if err != nil {
		return "", fmt.Errorf("read the %s file: %w", what, err)
	}
	return string(data), nil
}

// sendAgentRequest sends request to the run's relay and waits, without a time
// limit, for MMHotline's answer: a playbook run may wait for another MMX run.
// The run's one-hour limit bounds the wait.
func sendAgentRequest(socketPath string, request AgentRequest) (Answer, error) {
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return Answer{}, fmt.Errorf("cannot reach the triage agent run's request socket: %w", err)
	}
	defer connection.Close()
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return Answer{}, fmt.Errorf("send the request to the triage agent run: %w", err)
	}
	var answer Answer
	if err := json.NewDecoder(connection).Decode(&answer); err != nil {
		return Answer{}, errors.New("the triage agent run ended before MMHotline answered")
	}
	return answer, nil
}
