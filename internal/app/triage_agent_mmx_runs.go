package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// MMHotline runs a playbook that the rules for running the agent's playbooks or people let it run
// with mmx, in MMHotline's copy, as the mmhotline user:
// docs/changing-mmhotline/design/triage-agent.md#running-mmx-for-the-agent.

// mmxVaultPassphraseFileName is the file in MMHotline's state directory that
// holds the team's vault passphrase for MMX, such as
// /var/lib/mmhotline/mmx_vault_passphrase. MMX reads the file only when a
// task needs the vault.
const mmxVaultPassphraseFileName = "mmx_vault_passphrase"

// mmxRunTimeLimit ends an MMX run, its wait for another run included, so a
// hung run cannot hold MMHotline's MMX runs forever; a fix for an incident
// takes minutes.
const mmxRunTimeLimit = time.Hour

// mmxRunSearchPath is MMX's whole PATH: mmx in /usr/local/bin and
// system programs from the distribution.
const mmxRunSearchPath = "/usr/local/bin:/usr/bin:/bin"

// mmxRunTerminationGracePeriod is how long MMX's process group has after
// SIGTERM at the time limit before it gets SIGKILL.
const mmxRunTerminationGracePeriod = 10 * time.Second

// RunPlaybook runs mmx run NAME --inventory INVENTORY in MMHotline's copy, at
// commitID, and records the run with the request. It runs one MMX run at a
// time; the copy's lock orders it with a person's helper that runs MMX there.
func (a *App) RunPlaybook(ctx context.Context, request TriageAgentPlaybookRunRequest, commitID string) TriageAgentPlaybookRunDecision {
	name, ok := mmxPlaybookName(request.Playbook)
	if !ok {
		return TriageAgentPlaybookRunDecision{Outcome: triageagent.Refused, Message: "MMX cannot name " + request.Playbook + ", so MMHotline did not run it."}
	}
	// The MMX run is the triage run's current step: a triage run that ends or
	// is stopped must not end it halfway.
	ctx = context.WithoutCancel(ctx)
	// The copy's lock, taken inside the time limit, makes MMX runs wait one
	// for another, also within this process, and records the wait.
	limit := a.triageAgent.mmxRunTimeLimit
	runContext, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	decision := TriageAgentPlaybookRunDecision{Outcome: triageagent.Refused}
	options := systemsrepository.CopyLockOptions{
		Holder: fmt.Sprintf("mmhotline: mmx run %s --inventory %s for the triage agent, started %s, process %d", name, request.Inventory, a.Now().UTC().Format(time.RFC3339), os.Getpid()),
		WaitingChanged: func(waiting bool) {
			since := int64(0)
			if waiting {
				since = a.Now().Unix()
			}
			a.recordMMXRun(ctx, request, `playbook_run_waiting_for_another_run_since=?`, since)
		},
	}
	// The check goes through origin's branch: a commit that waits there stops
	// MMX even at a judged commit before it.
	var check CommitCheck
	newer, err := systemsrepository.WithCopyForMMX(runContext, directory, commitID, options, a.trustedCommit(&check), func() error {
		if refusal := a.mmxRefusal(check, nil); refusal != "" {
			decision.Message = refusal
			return nil
		}
		program, err := FindMMXProgram(a.triageAgent.mmxSearchPath)
		if err != nil {
			decision.Message = "MMHotline did not run the playbook: " + err.Error()
			return nil
		}
		command := exec.CommandContext(runContext, program, "run", "--non-interactive", "--vault-password-file", filepath.Join(a.Store.Dir, mmxVaultPassphraseFileName), name, "--inventory", request.Inventory)
		command.Dir = directory
		command.Env = mmxEnvironment(a.Store.Dir)
		// Standard input stays nil, the null device: nobody answers a prompt.
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// MMX starts a clean stop on SIGTERM.
		command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGTERM) }
		command.WaitDelay = mmxRunTerminationGracePeriod
		output := &bytes.Buffer{}
		// One writer for both, so the output keeps their order.
		command.Stdout, command.Stderr = output, output
		startedAt := a.Now().Unix()
		if err := command.Start(); err != nil {
			var pathError *fs.PathError
			if errors.As(err, &pathError) {
				err = pathError.Err
			}
			decision.Message = fmt.Sprintf("MMHotline could not start %s, so nothing ran: %s.", command.Path, err)
			return nil
		}
		a.recordMMXRun(ctx, request, `playbook_run_started_at=?`, startedAt)
		err = command.Wait()
		if runContext.Err() != nil {
			// What the process group left after the grace period. The copy's
			// lock is still held, so none of it overlaps the next holder.
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}

		decision = TriageAgentPlaybookRunDecision{Outcome: triageagent.RanAndFailed, PlaybookRunOutput: output.String()}
		var exitCode sql.NullInt64 // NULL when MMX did not exit by itself
		state := command.ProcessState
		if state != nil && state.Exited() {
			exitCode = sql.NullInt64{Int64: int64(state.ExitCode()), Valid: true}
		}
		switch {
		case runContext.Err() != nil:
			decision.Message = fmt.Sprintf("MMHotline ran the playbook with MMX, and the run failed: MMX did not finish within %s, MMHotline's time limit for an MMX run, so MMHotline ended the MMX run. The playbook may have stopped partway.", limit)
		case !exitCode.Valid:
			decision.Message = fmt.Sprintf("MMHotline ran the playbook with MMX, and the run failed: MMX ended without an exit code (%s).", err)
		case exitCode.Int64 == 0:
			decision.Outcome, decision.Message = triageagent.RanAndSucceeded, "MMHotline ran the playbook with MMX, and the run succeeded."
		default:
			decision.Message = fmt.Sprintf("MMHotline ran the playbook with MMX, and the run failed: MMX exited with %d. The end of the output says why.", exitCode.Int64)
		}
		a.recordMMXRun(ctx, request, `playbook_run_ended_at=?,playbook_run_exit_code=?,playbook_run_output=?,playbook_run_waiting_for_another_run_since=0`, a.Now().Unix(), exitCode, decision.PlaybookRunOutput)
		return nil
	})
	if err != nil {
		decision.Message = "MMHotline did not run the playbook: " + err.Error()
		// Missing keys, or commits that wait with nothing trusted before them,
		// say why in the check's own words.
		switch {
		case errors.Is(err, errCommitSigningKeysNotSet):
			decision.Message = a.mmxRefusal(check, errCommitSigningKeysNotSet)
		case len(check.Waiting) > 0:
			decision.Message = a.mmxRefusal(check, nil)
		}
	}
	if newer != "" && len(check.Waiting) > 0 {
		// A commit that waits on origin's branch stops MMX without judging
		// the request again at the newer commit.
		decision.Message = a.mmxRefusal(check, nil)
	} else if newer != "" {
		decision.CopyMovedToCommitID = newer
		decision.Message = fmt.Sprintf("MMHotline did not run the playbook: the copy moved past the judged commit %s to %s.", commitID, newer)
	}
	return decision
}

// recordMMXRun sets columns of the request's MMX run. A failed write leaves
// the run going: the record is for people, and MMX is changing servers.
func (a *App) recordMMXRun(ctx context.Context, request TriageAgentPlaybookRunRequest, columns string, values ...any) {
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE triage_agent_requests SET `+columns+` WHERE run_id=? AND request_id=?`, append(values, request.RunID, request.RequestID)...)
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmhotline: could not save the MMX run's record")
	}
}

// mmxEnvironment is the whole environment of mmx explain and MMX: nothing
// of MMHotline's own, which also holds whatever the team put in its settings
// file, since a GIT_ or MMX_ variable would change what Git or MMX does.
// install makes the state directory the mmhotline user's home.
func mmxEnvironment(home string) []string {
	return []string{"HOME=" + home, "PATH=" + mmxRunSearchPath}
}
