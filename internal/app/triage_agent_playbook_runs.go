package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// MMHotline decides whether it runs a playbook the agent asked for from what
// the playbook's node tasks do and the inventory's rules for running the agent's playbooks:
// docs/changing-mmhotline/design/triage-agent.md#rules-for-running-the-agents-playbooks-and-kinds-of-action.

// TriageAgentPlaybookRunner runs a playbook that the rules for running the agent's playbooks let
// MMHotline run without a person; commitID is the commit MMHotline read its
// node tasks at. The App is the runner (triage_agent_mmx_runs.go); tests
// stand in their own.
type TriageAgentPlaybookRunner interface {
	RunPlaybook(ctx context.Context, request TriageAgentPlaybookRunRequest, commitID string) TriageAgentPlaybookRunDecision
}

// MMXSearchPath is where MMHotline looks for the mmx program, as runs look
// for the coding agent's programs: the team installs it in /usr/local/bin.
const MMXSearchPath = "/usr/local/bin:/usr/bin"

// mmxReadingTimeLimit ends an mmx explain or mmx nodes that hangs, so it
// cannot hold the one triage run; both read files and contact no server.
const mmxReadingTimeLimit = 2 * time.Minute

// maximumMMXMessage bounds MMX's error message in an answer, as a step's
// text is bounded; MMX names the file, line, and field in its first lines.
const maximumMMXMessage = 2000

// maximumMMXOutputBytes bounds what MMHotline reads of mmx explain's or mmx
// nodes' output, whose size the agent controls through the playbook it
// commits. mmx explain of the Monkeymaxxing Stack's largest playbook,
// every_server.yml, prints 3.1 MiB for 4 servers, about 800 KiB a server,
// so 64 MiB fits about 80 servers. It is also the fleet view's read limit
// and the systems repository bundle's limit. Output past the limit is
// refused, never cut: MMHotline judges the playbook, and keeps the output
// for the people who approve, only as a whole.
const maximumMMXOutputBytes = 64 << 20

// maximumMMXErrorOutputBytes bounds what MMHotline reads of mmx's error
// output: more than the maximumMMXMessage characters an answer shows.
const maximumMMXErrorOutputBytes = 64 << 10

// maximumPlaybookFileBytes bounds the playbook file MMHotline reads into
// memory, before mmx reads it. The Monkeymaxxing Stack's largest playbook,
// every_server.yml, is 68 KiB, so 1 MiB is about 15 times that. A larger
// playbook is refused with the limit; the agent can split it.
const maximumPlaybookFileBytes = 1 << 20

// maximumTasksNamedInAnswer bounds the node tasks an answer names in each list.
const maximumTasksNamedInAnswer = 10

// MMX names a playbook by its file name: playbooks/NAME.yml, whose name
// starts with a letter or "_", or one_time_playbooks/DATE_WHAT.yml, whose name
// starts with its date. The two never overlap, so a name stands for exactly
// the file the agent asked for.
var (
	ordinaryPlaybookName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	oneTimePlaybookName  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_[a-z0-9]+(_[a-z0-9]+)*$`)
)

// inventoryText names an inventory as one, such as the inventory “servers”:
// a person who reads "on servers" or "on production" takes the name for
// ordinary words. The quotes are those MMHotline puts around other names.
func inventoryText(inventory string) string {
	return "the inventory “" + inventory + "”"
}

// playbookInInventoryText names a playbook with the inventory it runs in,
// such as one_time_playbooks/x.yml in the inventory “servers”.
func playbookInInventoryText(playbook, inventory string) string {
	return playbook + " in " + inventoryText(inventory)
}

func mmxPlaybookName(playbook string) (string, bool) {
	directory, file, _ := strings.Cut(playbook, "/")
	name, isYAML := strings.CutSuffix(file, ".yml")
	switch {
	case !isYAML:
		return "", false
	case directory == "playbooks":
		return name, ordinaryPlaybookName.MatchString(name)
	case directory == "one_time_playbooks":
		return name, oneTimePlaybookName.MatchString(name)
	}
	return "", false
}

// DecidePlaybookRun judges the playbook (judgePlaybook), asks the review
// agent when one is set up, and runs the playbook only when the review
// agent, if any, approved it, every kind is set to Act, then report, and no
// node task acts on a service whose changes a person must approve; otherwise the run
// waits for a person. The answer names the kinds, those tasks, and the
// review, so the agent learns why.
func (a *App) DecidePlaybookRun(ctx context.Context, request TriageAgentPlaybookRunRequest) TriageAgentPlaybookRunDecision {
	for attempt := 1; ; attempt++ {
		decision, judgedAnotherCommit := a.judgeAndRunPlaybook(ctx, request)
		// MMHotline refused because the copy moved past the commit MMHotline
		// judged: what runs must be what was judged, so judge again there.
		if !judgedAnotherCommit || attempt == playbookJudgingAttempts {
			return decision
		}
	}
}

// playbookJudgingAttempts bounds judging again when other commits keep
// arriving between judging and running; a copy that busy needs a person.
const playbookJudgingAttempts = 3

// playbookJudgment is what decides a playbook run on one inventory: the
// playbook's node tasks as mmx explain lists them at one commit of
// MMHotline's copy, their kinds of action, the rule of each kind,
// and the node tasks on services whose changes a person must approve.
type playbookJudgment struct {
	// commitID is the commit of MMHotline's copy that mmx explain read.
	commitID     string
	playbookText string
	// projectFilesText shows the files the node tasks copy or render from
	// the systems repository at commitID (projectFilesText).
	projectFilesText string
	mmxExplainOutput []byte
	recognition      actionkinds.Recognition
	// kindSettings are the rules for running the agent's playbooks of recognition.Kinds on the
	// inventory, in that order.
	kindSettings                             []ActionKindAgentPlaybookRule
	onServicesWhoseChangesAPersonMustApprove []taskOnAServiceWhoseChangesAPersonMustApprove
	// listed is the rule a person listed for the playbook, playbooks/NAME.yml,
	// on the inventory, or nil.
	listed *ListedPlaybookAgentPlaybookRule
	// setting is the rule the playbook takes (playbookRule).
	setting AgentPlaybookRule
}

// servicesListDecides says whether the list of services whose changes a
// person must approve made the playbook wait (servicesListDecides).
func (j playbookJudgment) servicesListDecides() bool {
	return servicesListDecides(j.listed != nil, j.onServicesWhoseChangesAPersonMustApprove)
}

// ruleFromListedPlaybook says the playbook takes the rule a person listed
// for it: it is listed, and the list of services whose changes a person
// must approve does not decide instead.
func (j playbookJudgment) ruleFromListedPlaybook() bool {
	return j.listed != nil && !j.servicesListDecides()
}

// listedRuleText says the playbook takes the rule listed for it and what
// follows, such as "The playbook every_server is listed in
// rules_for_running_agent_playbooks for the inventory “servers” with Act,
// then report, so MMHotline runs the playbook without a person.", and that
// the listed rule holds although a task acts on a service whose changes a
// person must approve; empty when the playbook is not listed.
func (j playbookJudgment) listedRuleText(inventory, so string) string {
	if !j.ruleFromListedPlaybook() {
		return ""
	}
	text := capitalize(listedPlaybookRuleText(j.listed.PlaybookName, inventory, j.listed.AgentPlaybookRule)) + ", so " + so + "."
	if len(j.onServicesWhoseChangesAPersonMustApprove) > 0 {
		text += " A listed playbook keeps its listed rule even when a task acts on a service whose changes a person must approve."
	}
	return text
}

// judgePlaybook judges a playbook on an inventory, for the triage agent's
// requests and for agent-playbook-rules check-whether-a-playbook-needs-a-person
// alike, so the two never disagree. It checks the path and the inventory name as the agent's request
// is checked, runs mmx explain in MMHotline's copy at the commit the copy is
// at, recognizes the kinds of action, and reads the rules for running the agent's playbooks and the
// list of services whose changes a person must approve, once. It changes nothing. When
// MMHotline cannot judge the playbook, the error's text is the refusal the
// agent reads.
func (a *App) judgePlaybook(ctx context.Context, playbookPath, inventory string) (playbookJudgment, error) {
	var judgment playbookJudgment
	err := triageagent.CheckPlaybookPath(playbookPath)
	if err == nil {
		err = triageagent.CheckInventoryName(inventory)
	}
	if err != nil {
		return judgment, errors.New(requestRefusedText(err))
	}
	name, ok := mmxPlaybookName(playbookPath)
	if !ok {
		return judgment, errors.New("MMX runs a playbook only from playbooks/NAME.yml or one_time_playbooks/YYYY-MM-DD_what_it_does.yml, so MMHotline cannot run " + playbookPath + ".")
	}
	playbook := playbookInInventoryText(playbookPath, inventory)
	program, err := FindMMXProgram(a.triageAgent.mmxSearchPath)
	if err != nil {
		return judgment, errors.New("MMHotline cannot tell what " + playbook + " does: " + err.Error() + ".")
	}
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	err = systemsrepository.ReadWorkingTree(ctx, directory, func(commit string) error {
		// The working tree is at commit, with no uncommitted changes, so the
		// file's size is the size of the text read below.
		if info, err := os.Lstat(filepath.Join(directory, playbookPath)); err == nil && info.Size() > maximumPlaybookFileBytes {
			return fmt.Errorf("the playbook is %d KiB, more than the %d KiB MMHotline reads of a playbook; split the playbook into smaller playbooks", (info.Size()+1023)>>10, maximumPlaybookFileBytes>>10)
		}
		output, err := readWithMMX(ctx, program, directory, mmxEnvironment(a.Store.Dir), "explain", name, "--inventory", inventory, "--json")
		if err != nil {
			return err
		}
		judgment.commitID, judgment.mmxExplainOutput = commit, output
		if judgment.recognition, err = actionkinds.Recognize(output); err != nil {
			return err
		}
		// The review agent and the people who approve read the playbook as
		// MMHotline judged it.
		judgment.playbookText, err = systemsrepository.FileAtCommit(ctx, directory, commit, playbookPath)
		if err != nil {
			return err
		}
		judgment.projectFilesText, err = projectFilesText(ctx, directory, commit, output)
		return err
	})
	if err != nil {
		return judgment, errors.New("MMHotline cannot tell what " + playbook + " does: " + strings.TrimSuffix(err.Error(), ".") + ".")
	}
	// The rules for running the agent's playbooks and the services whose changes a person must approve are read
	// once, before any review, and decide a request even when a person
	// changes them during the review.
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		saved, err := savedAgentPlaybookRules(ctx, tx, inventory)
		if err != nil {
			return err
		}
		for _, kind := range judgment.recognition.Kinds {
			judgment.kindSettings = append(judgment.kindSettings, kindAgentPlaybookRule(saved[inventory], kind))
		}
		services, err := readServicesWhoseChangesAPersonMustApprove(ctx, tx)
		if err != nil {
			return err
		}
		judgment.onServicesWhoseChangesAPersonMustApprove = tasksOnServicesWhoseChangesAPersonMustApprove(judgment.recognition, services.ServiceNames)
		// Only an ordinary playbook can be listed; the triage agent writes
		// the one-time playbooks.
		if !strings.HasPrefix(playbookPath, "playbooks/") {
			return nil
		}
		listed, err := savedListedPlaybookRules(ctx, tx, inventory, name)
		if rows := listed[inventory]; err == nil && len(rows) == 1 {
			judgment.listed = &rows[0]
		}
		return err
	})
	if err != nil {
		return judgment, errors.New("MMHotline could not read the rules for running the agent's playbooks, so it did not run " + playbook + ".")
	}
	var listed *AgentPlaybookRule
	if judgment.listed != nil {
		listed = &judgment.listed.AgentPlaybookRule
	}
	judgment.setting = playbookRule(judgment.kindSettings, judgment.onServicesWhoseChangesAPersonMustApprove, listed)
	return judgment, nil
}

// requestRefusedText is the answer to a request MMHotline refuses before it
// reads anything.
func requestRefusedText(err error) string {
	return "MMHotline refused the request: " + err.Error() + "."
}

func (a *App) judgeAndRunPlaybook(ctx context.Context, request TriageAgentPlaybookRunRequest) (decision TriageAgentPlaybookRunDecision, judgeAgain bool) {
	refused := func(message string) (TriageAgentPlaybookRunDecision, bool) {
		return TriageAgentPlaybookRunDecision{Outcome: triageagent.Refused, Message: message}, false
	}
	playbook := playbookInInventoryText(request.Playbook, request.Inventory)
	judgment, err := a.judgePlaybook(ctx, request.Playbook, request.Inventory)
	if err != nil {
		// The checkout stays at the trusted commit while a commit waits, so
		// the agent's newer playbook may be missing there; say why.
		return refused(strings.TrimSpace(err.Error() + "\n\n" + a.waitingCommitsRefusal(ctx)))
	}
	reviewAgent, reviewed, err := a.savedReviewAgent(ctx)
	if err != nil {
		return refused("MMHotline could not read the review agent's setup, so it did not run " + playbook + ".")
	}
	commitID, recognition, setting, onServicesWhoseChangesAPersonMustApprove := judgment.commitID, judgment.recognition, judgment.setting, judgment.onServicesWhoseChangesAPersonMustApprove
	// An earlier approval in the session may cover a playbook that would
	// wait for a person (approvalThatCovers). Act, then report runs anyway,
	// so it never uses a cover.
	var covered bool
	if setting.Rule != actThenReport {
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			var err error
			_, covered, err = approvalThatCovers(ctx, tx, request)
			return err
		})
		if err != nil {
			return refused("MMHotline could not read the approvals of your triage session, so it did not run " + playbook + ".")
		}
	}
	// The review agent gates only a playbook that would run without a
	// person: one that waits for a person anyway gains nothing from it, and
	// a review would delay the person's request by up to its time limit. A
	// covered playbook runs without a person's approval step, so it is
	// reviewed.
	reviewed = reviewed && (setting.Rule != getPermissionThenAct || covered)
	var review playbookReview
	var problem reviewProblem
	if reviewed {
		// The review agent reads the agent's diagnosis and its own earlier
		// refusals in this problem's row, which show what it asked to change.
		var diagnosis string
		var earlierRefusals []refusedVersion
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, `SELECT diagnosis FROM triage_agent_requests WHERE run_id=? AND request_id=?`, request.RunID, request.RequestID).Scan(&diagnosis); err != nil {
				return err
			}
			var err error
			if problem, err = readReviewProblem(ctx, tx, request.RunID); err != nil {
				return err
			}
			earlierRefusals, err = reviewAgentRefusalsInARow(ctx, tx, problem, latestReviewID)
			return err
		})
		if err != nil {
			return refused("MMHotline could not read your diagnosis for the review agent, so it did not run " + playbook + ".")
		}
		if prompt, fits := reviewAgentPrompt(request, commitID, judgment.playbookText, judgment.projectFilesText, diagnosis, earlierRefusals, recognition); fits {
			review = a.reviewPlaybook(ctx, reviewAgent, prompt)
		} else {
			now := a.Now().UTC()
			review = playbookReview{outcome: reviewCouldNotReview, reason: fmt.Sprintf("The playbook, its list of tasks, the diagnosis, and the earlier reasons are longer than the %d KiB a review prompt holds.", maximumReviewPromptBytes>>10), agent: reviewAgent, startedAt: now, endedAt: now}
		}
	}
	does := playbookKindsText(playbook, recognition) + servicesWhoseChangesAPersonMustApproveText(onServicesWhoseChangesAPersonMustApprove)
	if reviewed {
		does += " " + review.text()
	}
	var approvalRequestID, coveredPlaybookRunID, coveredBy, takenOver string
	var runsUnlessStoppedAt time.Time
	// refusalsInARow counts the review agent's refusals in this problem's
	// row, this one included, when it refused.
	var refusalsInARow int
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		// Every judged request shows the commit it was judged at, whether
		// it runs or waits for a person.
		if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_requests SET playbook_read_at_commit_id=? WHERE run_id=? AND request_id=?`, commitID, request.RunID, request.RequestID); err != nil {
			return err
		}
		if reviewed {
			if err := insertPlaybookRunReview(ctx, tx, request, commitID, judgment.keptText(), review); err != nil {
				return err
			}
		}
		// A person may have taken the session over while MMHotline or the
		// review agent read the playbook: then nothing runs or waits.
		var err error
		if takenOver, err = takenOverRefusal(ctx, tx, request.RunID); err != nil || takenOver != "" {
			return err
		}
		if reviewed && review.outcome == reviewRefused {
			refusals, err := reviewAgentRefusalsInARow(ctx, tx, problem, latestReviewID)
			if err != nil {
				return err
			}
			// Before the row is long enough, the refusal goes back to the
			// triage agent, which may revise the playbook and ask again.
			if refusalsInARow = len(refusals); refusalsInARow < maximumReviewAgentRefusalsInARow {
				return recordReviewAgentRefusal(ctx, tx, request, problem, commitID, refusalsInARow, review, a.Now().UTC())
			}
		}
		// A playbook the review agent did not approve waits for a person,
		// with at least the approvals its kinds ask, even when an earlier
		// approval covers it.
		if reviewed && review.outcome != reviewApproved {
			setting = AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: max(1, setting.ApprovalsNeeded)}
			covered = false
		}
		if setting.Rule == actThenReport {
			return nil
		}
		// Read again in this transaction, so one cover is used once even
		// when MMHotline judges two requests at once.
		var cover approvalRequest
		if covered {
			if cover, covered, err = approvalThatCovers(ctx, tx, request); err != nil {
				return err
			}
		}
		// Approvals and warned runs read this record; action_kinds holds the
		// kinds' IDs, comma-separated, in the order of Kinds().
		var kinds []string
		for _, kind := range recognition.Kinds {
			kinds = append(kinds, string(kind))
		}
		waiting := approvalRequest{ID: NewID(), RunID: request.RunID, RequestID: request.RequestID, Playbook: request.Playbook, Inventory: request.Inventory, CommitID: commitID, ActionKinds: strings.Join(kinds, ","), PlaybookText: judgment.keptText(), MMXExplainOutput: string(judgment.mmxExplainOutput), PlaybookRule: setting, JudgedAt: a.Now().UTC(),
			// A review that did not approve sets the rule instead.
			RuleFromListedPlaybook: judgment.ruleFromListedPlaybook() && setting == judgment.setting}
		if err := tx.QueryRowContext(ctx, `SELECT triage_session_id FROM triage_agent_runs WHERE id=?`, request.RunID).Scan(&waiting.TriageSessionID); err != nil {
			return err
		}
		switch {
		case covered:
			waiting.CoveredByApprovalRequestID, waiting.IncidentID = cover.ID, cover.IncidentID
		case waiting.RuleFromListedPlaybook && setting.Rule == getPermissionThenAct && waiting.TriageSessionID != "":
			// The approval's cover is saved now, as the approval page shows
			// it before the click; a software review has no session to
			// cover.
			waiting.ItsApprovalAlsoApprovesInTheSameSession = judgment.listed.ItsApprovalAlsoApprovesInTheSameSession
		}
		if err := insertPlaybookRunWaitingForAPerson(ctx, tx, waiting); err != nil {
			return err
		}
		if covered {
			coveredPlaybookRunID = waiting.ID
			if coveredBy, err = coveredByText(ctx, tx, waiting); err != nil {
				return err
			}
			return recordCoveredPlaybookRun(ctx, tx, waiting, coveredBy, waiting.JudgedAt)
		}
		if setting.Rule == warnWaitThenAct {
			runsUnlessStoppedAt = waiting.runsAt()
			return a.warnOfPlaybookRun(ctx, tx, waiting)
		}
		approvalRequestID = waiting.ID
		why := judgment.approvalReasonText(request.Inventory)
		if reviewed && review.outcome != reviewApproved {
			why += " The playbook run waits for your approval because the review agent did not approve the playbook: " + cutAtWord(plainTitleText(review.text()), maximumReportSummaryRunes)
		}
		if refusalsInARow > 0 {
			why += " " + cutAtWord(plainTitleText(aPersonDecidesAfterRefusalsText(problem, refusalsInARow)), maximumReportSummaryRunes)
		}
		return a.askForPlaybookRunApproval(ctx, tx, waiting, strings.TrimSpace(why), waiting.JudgedAt)
	})
	if err != nil {
		return refused("MMHotline could not read the rules for running the agent's playbooks or record its decision, so it did not run " + playbook + ".")
	}
	if takenOver != "" {
		return refused(takenOver)
	}
	if coveredPlaybookRunID != "" {
		return TriageAgentPlaybookRunDecision{Outcome: triageagent.WaitsForAPerson, Message: does + " " + coveredBy + " So MMHotline runs the playbook next without asking a person again. When the run ends, MMHotline resumes your session with the result. Commit nothing more until MMHotline resumes you: a newer commit can make the approval stale, and a person must then approve again. End your turn now and report.", CoveredPlaybookRunID: coveredPlaybookRunID}, false
	}
	switch {
	case refusalsInARow > 0 && refusalsInARow < maximumReviewAgentRefusalsInARow:
		return refused(does + sentBackToTheTriageAgentText(problem, refusalsInARow))
	case refusalsInARow > 0:
		return TriageAgentPlaybookRunDecision{Outcome: triageagent.WaitsForAPerson, Message: does + " " + aPersonDecidesAfterRefusalsText(problem, refusalsInARow) + " The request waits for a person: " + setting.Title() + ". The approval request shows the person every version you asked for and every reason the review agent gave.", ApprovalRequestID: approvalRequestID}, false
	case reviewed && review.outcome != reviewApproved:
		return TriageAgentPlaybookRunDecision{Outcome: triageagent.WaitsForAPerson, Message: does + " A playbook the review agent did not approve waits for a person: " + setting.Title() + ".", ApprovalRequestID: approvalRequestID}, false
	}
	if setting.Rule == actThenReport {
		decision := a.runPlaybookForTheAgent(ctx, request, commitID)
		if decision.CopyMovedToCommitID != "" {
			decision.Message = "Other commits arrived before the run, so MMHotline judged " + playbook + " again and nothing ran. " + decision.Message
			return decision, true
		}
		if listed := judgment.listedRuleText(request.Inventory, "MMHotline runs the playbook without a person"); listed != "" {
			decision.Message = does + " " + listed + " " + decision.Message
			return decision, false
		}
		decision.Message = does + " For " + inventoryText(request.Inventory) + ", the rules for running the agent's playbooks let MMHotline run the playbook without a person. " + decision.Message
		return decision, false
	}
	if judgment.servicesListDecides() {
		return TriageAgentPlaybookRunDecision{Outcome: triageagent.WaitsForAPerson, Message: does + " Because of the list of services whose changes a person must approve, the run waits for a person: " + setting.Title() + ".", ApprovalRequestID: approvalRequestID}, false
	}
	if listed := judgment.listedRuleText(request.Inventory, "the run waits for a person"); listed != "" {
		return TriageAgentPlaybookRunDecision{Outcome: triageagent.WaitsForAPerson, Message: does + " " + listed, ApprovalRequestID: approvalRequestID, RunsUnlessStoppedAt: runsUnlessStoppedAt}, false
	}
	return TriageAgentPlaybookRunDecision{Outcome: triageagent.WaitsForAPerson, Message: does + " For " + inventoryText(request.Inventory) + ", the strictest rule of the playbook's kinds of action is " + setting.Title() + ", so the run waits for a person.", ApprovalRequestID: approvalRequestID, RunsUnlessStoppedAt: runsUnlessStoppedAt}, false
}

// FindMMXProgram finds mmx as the triage agent's programs are found: in the
// directories of the search path, in order.
func FindMMXProgram(searchPath string) (string, error) {
	for _, directory := range filepath.SplitList(searchPath) {
		program := filepath.Join(directory, "mmx")
		if info, err := os.Stat(program); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return program, nil
		}
	}
	return "", fmt.Errorf("MMHotline finds no mmx program in %s", strings.Join(filepath.SplitList(searchPath), " or "))
}

// readWithMMX runs mmx explain or mmx nodes, with args, as the mmhotline user
// in its copy of the systems repository and returns what it printed. Both
// read the project's files, explain also the playbook and its roles; they
// unlock no vault and contact no server. An error carries MMX's own message,
// which names the file and the field.
func readWithMMX(ctx context.Context, program, directory string, environment []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, mmxReadingTimeLimit)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = directory
	command.Env = environment
	output, errorOutput := &limitedBuffer{max: maximumMMXOutputBytes}, &limitedBuffer{max: maximumMMXErrorOutputBytes}
	command.Stdout, command.Stderr = output, errorOutput
	err := command.Run()
	if err == nil && output.overflowed {
		return nil, fmt.Errorf("mmx %s printed more than %d MiB, the most MMHotline reads; a smaller playbook, or one for fewer servers, prints less", args[0], maximumMMXOutputBytes>>20) //nolint:nilaway // Every caller names an mmx command.
	}
	if err == nil {
		return output.buffer.Bytes(), nil
	}
	message := strings.TrimSpace(errorOutput.buffer.String())
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		message = fmt.Sprintf("did not finish within %s", mmxReadingTimeLimit)
	case message == "":
		message = err.Error()
	}
	if len(message) > maximumMMXMessage {
		cut := maximumMMXMessage
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "…"
	}
	return nil, errors.New("mmx " + args[0] + " failed: " + message) //nolint:nilaway // Every caller names an mmx command.
}

// playbookKindsText says what the playbook does, such as "playbooks/x.yml in
// the inventory “production” does: Free disk space, Reboot a server.", and
// which node tasks MMHotline did not recognize and why, so the agent can
// write them differently and a person who approves sees why the run waits.
func playbookKindsText(playbook string, recognition actionkinds.Recognition) string {
	var titles []string
	for _, kind := range recognition.Kinds {
		titles = append(titles, kind.Title())
	}
	return playbook + " does: " + strings.Join(titles, ", ") + "." + unrecognizedTasksText(recognition.UnrecognizedTasks)
}

// unrecognizedTasksText names the node tasks MMHotline did not recognize,
// each with why, such as " The task MMHotline did not recognize, and why:
// fix/task_1 (shell): no kind covers the module shell."; past
// maximumTasksNamedInAnswer it counts the rest. With none, it is empty.
func unrecognizedTasksText(tasks []actionkinds.Unrecognized) string {
	if len(tasks) == 0 {
		return ""
	}
	heading := " The tasks MMHotline did not recognize, and why:"
	if len(tasks) == 1 {
		heading = " The task MMHotline did not recognize, and why:"
	}
	entries := []string{heading}
	for index, task := range tasks {
		if index == maximumTasksNamedInAnswer {
			entries = append(entries, fmt.Sprintf("And %d more.", len(tasks)-index))
			break
		}
		entries = append(entries, task.Path+" ("+task.Action+"): "+task.Reason+".")
	}
	return strings.Join(entries, " ")
}

// approvalReasonText says, for the approval request's SOS and history, why
// the playbook waits for a person: each kind whose rule asks for
// permission, and each service whose changes a person must approve. It counts the
// tasks MMHotline can't check and names their modules, never their paths or
// recognition reasons, which the request's card and the answer to the agent
// hold.
func (j playbookJudgment) approvalReasonText(inventory string) string {
	var causes []string
	// A listed playbook's rule replaces the rules of its kinds.
	if j.ruleFromListedPlaybook() && j.listed.Rule == getPermissionThenAct {
		causes = append(causes, listedPlaybookRuleText(j.listed.PlaybookName, inventory, j.listed.AgentPlaybookRule))
	}
	for _, kind := range j.kindSettings {
		if j.ruleFromListedPlaybook() || kind.Rule != getPermissionThenAct {
			continue
		}
		if kind.ActionKind == actionkinds.UnrecognizedTask {
			var modules []string
			for _, task := range j.recognition.UnrecognizedTasks {
				if !slices.Contains(modules, task.Action) {
					modules = append(modules, task.Action)
				}
			}
			count := len(j.recognition.UnrecognizedTasks)
			what := fmt.Sprintf("what %d of the playbook's tasks do (the tasks use %s), and such tasks need ", count, namesInSentence(modules))
			if count == 1 {
				what = "what 1 of the playbook's tasks does (the task uses " + namesInSentence(modules) + "), and such a task needs "
			}
			causes = append(causes, "MMHotline cannot check "+what+kind.AgentPlaybookRule.Title())
		} else {
			causes = append(causes, "the playbook does “"+kind.ActionKind.Title()+"”, whose rule is "+kind.AgentPlaybookRule.Title())
		}
	}
	var services []string
	for _, task := range j.onServicesWhoseChangesAPersonMustApprove {
		if !j.servicesListDecides() {
			break
		}
		service := task.ServiceName
		if service == "" {
			service = "a service whose name MMX knows only when it runs, which could be on services_whose_changes_a_person_must_approve"
		} else {
			service += " is on services_whose_changes_a_person_must_approve"
		}
		if !slices.Contains(services, service) {
			services = append(services, service)
		}
	}
	causes = append(causes, services...)
	if len(causes) == 0 {
		return ""
	}
	return "The playbook run waits for your approval because " + strings.Join(causes, "; ") + "."
}

func namesInSentence(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	if len(names) == 2 {
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}
