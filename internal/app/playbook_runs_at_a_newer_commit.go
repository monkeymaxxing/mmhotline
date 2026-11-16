package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/systemsrepository"
)

// An approval, or a warned run whose time came, still applies at a newer
// commit of MMHotline's copy when that commit cannot change what MMHotline
// judged: docs/changing-mmhotline/design/triage-agent.md#newer-commits-of-records-only.

// recordFolders hold records that people and the triage agent read and that
// no MMX run needs: the agent's notes, one per triage session, which every
// session ends with.
var recordFolders = []string{"triage_agent_notes"}

// runAtJudgedOrNewerCommit runs r's playbook with MMX at the commit
// MMHotline judged. When the copy has moved past it, and the
// commits since change nothing MMHotline judged, it runs the playbook at the
// newer commit, checking again each time the copy moves, at most
// playbookJudgingAttempts runs in all. It returns the commit of the last run
// and its decision, and, when the copy moved past that commit, why the
// approval or the warning does not cover the newer commits.
func (a *App) runAtJudgedOrNewerCommit(ctx context.Context, request TriageAgentPlaybookRunRequest, r approvalRequest) (commit string, decision TriageAgentPlaybookRunDecision, notCovered string) {
	commit = r.CommitID
	for attempt := 1; ; attempt++ {
		decision = a.runPlaybookForTheAgent(ctx, request, commit)
		if decision.CopyMovedToCommitID == "" || ctx.Err() != nil {
			return commit, decision, ""
		}
		if attempt == playbookJudgingAttempts {
			return commit, decision, fmt.Sprintf("new commits kept arriving during %d attempts to run the playbook", attempt)
		}
		var newer string
		if newer, notCovered = a.judgmentAtCurrentCommit(ctx, r); notCovered != "" {
			return commit, decision, notCovered
		}
		commit = newer
	}
}

// judgmentAtCurrentCommit checks, under the copy's lock, whether r's
// judgment holds at the commit MMHotline's copy is at now. It returns that
// commit, or why it does not hold, such as "they change README.md, outside
// the triage agent's notes".
func (a *App) judgmentAtCurrentCommit(ctx context.Context, r approvalRequest) (commit, notCovered string) {
	name, _ := mmxPlaybookName(r.Playbook)
	program, err := FindMMXProgram(a.triageAgent.mmxSearchPath)
	if err != nil {
		return "", "MMHotline could not check the newer commits: " + err.Error()
	}
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	err = systemsrepository.ReadWorkingTree(ctx, directory, func(current string) error {
		commit = current
		// Only the record folders changed.
		changed, err := systemsrepository.ChangedPaths(ctx, directory, r.CommitID, current)
		if err != nil {
			return err
		}
		if notCovered = changedOutsideRecords(changed); notCovered != "" {
			return nil
		}
		// Nothing the playbook runs can read a record. A symlink in the
		// agent's folder can point at a record without naming its folder;
		// MMHotline never pushes one of the agent's, but a person may commit
		// one.
		symlinks, err := systemsrepository.SymlinksUnder(ctx, directory, r.CommitID, "one_time_playbooks")
		if err != nil {
			return err
		}
		if len(symlinks) > 0 {
			notCovered = symlinks[0] + " is a symlink, which can point at a file that changed"
			return nil
		}
		text, err := systemsrepository.FileAtCommit(ctx, directory, r.CommitID, r.Playbook)
		if err != nil {
			return err
		}
		if notCovered = recordFolderInPlaybook(text); notCovered != "" {
			return nil
		}
		agentWrote := strings.HasPrefix(r.Playbook, "one_time_playbooks/")
		if notCovered = tasksCouldReadARecord([]byte(r.MMXExplainOutput), agentWrote); notCovered != "" {
			return nil
		}
		// The same node tasks run, on the same servers, with the same
		// arguments. mmx explain reads only the project's files and the
		// inventory, so its output at an unchanged playbook is the same, byte
		// for byte; this also catches a role or a variable that reads a
		// record.
		explained, err := readWithMMX(ctx, program, directory, mmxEnvironment(a.Store.Dir), "explain", name, "--inventory", r.Inventory, "--json")
		if err != nil {
			return err
		}
		if !bytes.Equal(explained, []byte(r.MMXExplainOutput)) {
			notCovered = "mmx explain lists other tasks or values for the playbook at the newer commit"
		}
		return nil
	})
	if err != nil {
		return "", "MMHotline could not check the newer commits: " + strings.TrimSuffix(err.Error(), ".")
	}
	return commit, notCovered
}

// changedOutsideRecords says which of the changed paths lie outside the
// record folders, or returns "" when none does.
func changedOutsideRecords(paths []string) string {
	var outside []string
	for _, path := range paths {
		if folder, _, inFolder := strings.Cut(path, "/"); !inFolder || !slices.Contains(recordFolders, folder) {
			outside = append(outside, path)
		}
	}
	switch len(outside) {
	case 0:
		return ""
	case 1:
		return "the newer commits change " + outside[0] + ", outside the triage agent's notes"
	}
	return fmt.Sprintf("the newer commits change %s and %d more files outside the triage agent's notes", outside[0], len(outside)-1)
}

// recordFolderInPlaybook says why the playbook's text could make MMX read a
// record, or returns "". MMX reads a project file only where a playbook or a
// role names it, so a playbook that does not name the notes folder reads no record:
// not in its text, and not in a YAML key or value as MMX decodes it, where an
// escape such as "\x6e" hides a name from the text. A !!binary value hides
// its decoded text, so MMHotline does not check it and refuses it.
func recordFolderInPlaybook(text string) string {
	if folder := recordFolderIn(text); folder != "" {
		return playbookNamesFolder(folder)
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return "MMHotline cannot read the playbook as YAML: " + err.Error()
		}
		if why := recordFolderInYAML(&document); why != "" {
			return why
		}
	}
}

func recordFolderInYAML(node *yaml.Node) string {
	if node.Kind == yaml.ScalarNode {
		if node.ShortTag() == "!!binary" {
			return "the playbook holds a !!binary value, whose text MMHotline does not check"
		}
		if folder := recordFolderIn(node.Value); folder != "" {
			return playbookNamesFolder(folder)
		}
	}
	// An alias's node is checked where its anchor stands.
	for _, child := range node.Content {
		if why := recordFolderInYAML(child); why != "" {
			return why
		}
	}
	return ""
}

func playbookNamesFolder(folder string) string {
	return "the playbook names the folder " + folder + ", so it could read a file that changed"
}

// recordFolderIn returns the record folder that text names, or "".
func recordFolderIn(text string) string {
	for _, folder := range recordFolders {
		if strings.Contains(text, folder) {
			return folder
		}
	}
	return ""
}

// tasksCouldReadARecord says why a node task that mmx explain listed could
// read a record, or returns "":
//   - mmx explain's output names a record folder, such as a file task's
//     source, a role's input, or a condition;
//   - a directory task copies a directory of the project, which can be the
//     project itself, such as "." or "/proc/self/cwd", without naming a
//     folder;
//   - in a playbook the triage agent wrote (agentWrote, one_time_playbooks/),
//     a task MMHotline does not recognize has a value that mmx explain does
//     not show, or runs only if a condition holds on the server, for which
//     mmx explain lists only some of its arguments. The tasks of the
//     recognized kinds read no project file. A playbook in playbooks/ is a
//     person's text, which the triage agent's key can never change, so a
//     misled agent cannot have written it to read a note it changes after
//     the approval or warning; its hidden values do not count, or every
//     note the agent commits while a person's playbook waits would make the
//     approval or warning stale and the team warned twice;
//   - a fleet stage saves a file on MMHotline's server. It reads no record,
//     but MMHotline treats it with the most care.
func tasksCouldReadARecord(explained []byte, agentWrote bool) string {
	if folder := recordFolderIn(string(explained)); folder != "" {
		return "the playbook's tasks listed by mmx explain name the folder " + folder + ", so a task could read a file that changed"
	}
	recognition, err := actionkinds.Recognize(explained)
	if err != nil {
		return "MMHotline cannot read mmx explain's output: " + err.Error()
	}
	for _, task := range recognition.Tasks {
		for _, argument := range task.Arguments {
			if task.Action == "directory" && argument.Field == "directory.source" {
				return task.Path + " (directory) copies a directory of the systems repository, which can hold the triage agent's notes"
			}
		}
	}
	for _, task := range recognition.UnrecognizedTasks {
		// A file a fleet stage saves on MMHotline's server reads no record,
		// but it can change MMHotline itself, so such a playbook runs only at
		// the commit people saw.
		if task.Action == "save_on_controller_machine" {
			return task.Path + " (save_on_controller_machine) writes a file on MMHotline's server, so MMHotline runs the playbook only at the commit MMHotline checked"
		}
		if !agentWrote {
			continue
		}
		about := task.Path + " (" + task.Action + "), a task MMHotline does not recognize,"
		if task.Status == "conditional" {
			return about + " runs only if a condition holds on the server, so mmx explain does not show all its values"
		}
		for _, argument := range task.Arguments {
			if argument.Status != "literal" && argument.Status != "resolved" {
				return about + " has a value mmx explain does not show, so it could read a file that changed"
			}
		}
	}
	return ""
}

// ranAtText says which commit a run used, for its result: the one MMHotline
// checked, or a newer one that the approval or the warning still covers.
func (r approvalRequest) ranAtText(commit string) string {
	if commit == r.CommitID {
		return "The run used commit " + commit + ", the one MMHotline checked."
	}
	return "The run used commit " + commit + ", which is newer than commit " + r.CommitID + ", the one MMHotline checked. The commits in between change only the triage agent's notes, which the playbook does not read, so the " + r.whatLetItRun() + " still applies."
}

// whatLetItRun names what let r run: people's approval, or the warning
// nobody stopped.
func (r approvalRequest) whatLetItRun() string {
	if r.warned() {
		return "warning"
	}
	return "approval"
}
