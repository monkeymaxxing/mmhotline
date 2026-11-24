package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// lockOutFileForTest points the App at a lock-out file of the test's own,
// not there yet, and returns its path.
func lockOutFileForTest(t *testing.T, a *App) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "everyone-locked-out")
	a.lockOut.mu.Lock()
	a.lockOut.file = path
	a.lockOut.mu.Unlock()
	return path
}

func lockEveryoneOut(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The lock-out file appears while the triage agent works on a held SOS and
// a held warn, with no restart: the agent's run is cut at once, the SOS
// wakes the person on call and the warn reaches people, a new alert reaches
// people at once and starts no run, and once the file is gone the next
// alert starts a run again.
func TestALockOutStopsTheAgentAndTellsPeopleAtOnce(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 3)
	lockOut := lockOutFileForTest(t, r.a)
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	r.warnArrives(t, loadWarning, "app002-load")
	run := r.runStarted(t)
	if r.woken(sos.IncidentID) || len(r.pushovers(loadWarning)) != 0 {
		t.Fatal("the SOS or the warn reached people before the lock-out")
	}

	lockEveryoneOut(t, lockOut)
	backgroundRounds(t, r.a)
	// MMHotline closed the run's connection, which makes the sandbox kill
	// the coding agent at once; it did not wait for the agent's current step.
	run.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(run.connection); err != nil {
		t.Fatalf("the run's connection stayed open: %v", err)
	}
	waitNoTriageRun(t, r.a)
	var state, reason string
	if err := r.a.Store.db.QueryRow(`SELECT state,failure_reason FROM triage_agent_runs WHERE purpose='triage_session'`).Scan(&state, &reason); err != nil || state != "failed" || reason != everyoneLockedOutCannotRun {
		t.Fatalf("the cut run: %q %q %v", state, reason, err)
	}
	backgroundRounds(t, r.a)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the SOS still waits for a locked-out agent")
	}
	// The cut run's failure or the lock-out itself wakes the person on call,
	// whichever MMHotline reads first; the history names the lock-out.
	if history := r.history(t, sos.IncidentID); !inOrder(history, "Held for the triage agent", "Everyone is locked out: the file "+everyoneLockedOutFile+" exists", "waking the person on call", "Contacting Responder (on call)") {
		t.Fatalf("history: %q", history)
	}
	r.reachedSomeone(t, loadWarning)
	r.sentOnce(t, loadWarning, 0)

	// During the lock-out a new warn goes to people at once and starts no run.
	const memoryWarning = "Memory of app003 is low"
	r.warnArrives(t, memoryWarning, "app003-memory")
	r.sentOnce(t, memoryWarning, 0)
	select {
	case <-r.runs:
		t.Fatal("a triage agent run started during the lock-out")
	case <-time.After(200 * time.Millisecond):
	}

	if err := os.Remove(lockOut); err != nil {
		t.Fatal(err)
	}
	r.warnArrives(t, "CPU of app004 is busy", "app004-cpu")
	r.runStarted(t)
}

// During a lock-out MMHotline trusts no commit: commit-reviews check and
// trust-through are refused with the cause, MMX runs nothing, and no agent
// run starts. Removing the file brings the check back.
func TestALockOutRefusesTheCommitCheckMMXAndAgentRuns(t *testing.T) {
	parallelStartingProcesses(t)
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	lockOut := lockOutFileForTest(t, f.a)
	records := t.TempDir()
	commitRunMMXInCopy(t, f.a, fakeRunMMX(records, "echo 'PLAY RECAP app002 ok=1'\n"))
	head := testGit(t, filepath.Join(f.a.Store.Dir, "systems_repository"), "rev-parse", "HEAD")
	check := CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: head}}
	if result, raw := commandRequest(t, f.a, check, 200); result.CommitCheck == nil || result.CommitCheck.TrustedThroughCommitID != head {
		t.Fatalf("check before the lock-out: %s", raw)
	}

	lockEveryoneOut(t, lockOut)
	if _, raw := commandRequest(t, f.a, check, 409); !strings.Contains(raw, "everyone is locked out: the file "+everyoneLockedOutFile+" exists") {
		t.Fatalf("check during the lock-out: %s", raw)
	}
	if _, raw := commandRequest(t, f.a, CommandRequest{Group: "commit-reviews", Action: "trust-through", CommitReviews: CommitReviewsCommandRequest{CommitID: head}}, 409); !strings.Contains(raw, "everyone is locked out") {
		t.Fatalf("trust-through during the lock-out: %s", raw)
	}
	request := mmxRunRequest(t, f.a, "00000000000000a1")
	if decision := f.a.RunPlaybook(context.Background(), request, head); decision.Outcome != triageagent.Refused || decision.Message != "MMHotline did not run the playbook: "+everyoneLockedOutReason+"." {
		t.Fatalf("MMX during the lock-out: %+v", decision)
	}
	if _, err := os.Stat(filepath.Join(records, "arguments")); !os.IsNotExist(err) {
		t.Fatalf("MMX ran during the lock-out: %v", err)
	}
	if _, err := f.a.startSoftwareReview(context.Background(), "MMHotline", nil); !errors.Is(err, errEveryoneLockedOut) {
		t.Fatalf("a software review during the lock-out: %v", err)
	}
	if f.a.oneTriageRunIsFree() {
		t.Fatal("the one triage run is free for a review or a sign-in check during the lock-out")
	}

	if err := os.Remove(lockOut); err != nil {
		t.Fatal(err)
	}
	if result, raw := commandRequest(t, f.a, check, 200); result.CommitCheck == nil || result.CommitCheck.TrustedThroughCommitID != head {
		t.Fatalf("check after the lock-out: %s", raw)
	}
}

// trust-through looks for the lock-out file before it starts, but its wait
// for the copy's lock and its fetch take time: a lock-out file that appears
// during them still stops the trust. A look, such as the background loop's,
// ends the wait at once; without one, the transaction looks again before it
// records anything.
func TestALockOutStopsATrustThroughThatAlreadyBegan(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name             string
		aLookSeesTheFile bool
	}{
		{"a look sees the file during the wait for the copy's lock", true},
		{"no look sees the file before the transaction", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := foundationApp(t)
			commitRunMMXInCopy(t, a, "")
			lockOut := lockOutFileForTest(t, a)
			directory := filepath.Join(a.Store.Dir, "systems_repository")
			unsigned := commitInCopy(t, a, "unsigned", "roles/older.yml")
			checkpoint := func() string {
				var last string
				_ = a.Store.db.QueryRow(`SELECT last_checked_commit_id FROM triage_agent_commit_check_state`).Scan(&last)
				return last
			}
			before := checkpoint()
			release := holdCopyLock(t, a, everyServerHolder)
			results := make(chan error, 1)
			go func() {
				_, _, err := a.trustCommitsThrough(context.Background(), Principal{UnixUser: "alice"}, unsigned)
				results <- err
			}()
			// The wait's marker: trust-through passed its first look.
			waitForCondition(t, "trust-through to wait for the copy's lock", fileExists(systemsrepository.CopyLockWaitingPath(directory)))
			lockEveryoneOut(t, lockOut)
			if test.aLookSeesTheFile {
				a.everyoneLockedOut() // the background loop's look, every second
			} else {
				release()
			}
			select {
			case err := <-results:
				if !errors.Is(err, errEveryoneLockedOut) {
					t.Fatalf("trust-through during the lock-out: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("trust-through still runs during the lock-out")
			}
			var trustThroughs int
			if err := a.Store.db.QueryRow(`SELECT COUNT(*) FROM triage_agent_commit_trust_throughs`).Scan(&trustThroughs); err != nil || trustThroughs != 0 {
				t.Fatalf("trust-throughs recorded during the lock-out: %d %v", trustThroughs, err)
			}
			if after := checkpoint(); after != before {
				t.Fatalf("the checkpoint moved from %q to %q during the lock-out", before, after)
			}
			server, _ := os.Hostname()
			if countAlerts(t, a, "info", "alice on "+server+" marked every commit through "+unsigned[:7]+" trusted") != 0 {
				t.Fatal("an info alert announced a trust-through during the lock-out")
			}
			if !a.lockOut.lockedOutAtTheLastLook() {
				t.Fatal("the transaction's look was not recorded")
			}
		})
	}
}

// An MMX run for the triage agent outlasts a stopped triage run, but not a
// lock-out: one that waits for the copy's lock runs nothing, and one that
// runs gets SIGTERM.
func TestALockOutEndsMMXRuns(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("waiting for the copy's lock", func(t *testing.T) {
		a := foundationApp(t)
		lockOut := lockOutFileForTest(t, a)
		records := t.TempDir()
		commitRunMMXInCopy(t, a, fakeRunMMX(records, "echo 'PLAY RECAP app002 ok=1'\n"))
		directory := filepath.Join(a.Store.Dir, "systems_repository")
		lock, err := os.OpenFile(directory+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		request := mmxRunRequest(t, a, "00000000000000a1")
		decisions := make(chan TriageAgentPlaybookRunDecision, 1)
		go func() {
			decisions <- a.RunPlaybook(context.Background(), request, testGit(t, directory, "rev-parse", "HEAD"))
		}()
		waitForCondition(t, "the record of the wait", func() bool { return readMMXRunRecord(t, a, request.RunID, request.RequestID).waitingSince != 0 })
		lockEveryoneOut(t, lockOut)
		a.everyoneLockedOut() // the background loop's look, every second
		select {
		case decision := <-decisions:
			if decision.Outcome != triageagent.Refused || decision.Message != "MMHotline did not run the playbook: "+everyoneLockedOutReason+"." {
				t.Fatalf("%+v", decision)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the MMX run still waits for the copy's lock during the lock-out")
		}
		if _, err := os.Stat(filepath.Join(records, "arguments")); !os.IsNotExist(err) {
			t.Fatalf("MMX ran during the lock-out: %v", err)
		}
	})
	t.Run("running", func(t *testing.T) {
		a := foundationApp(t)
		lockOut := lockOutFileForTest(t, a)
		records := t.TempDir()
		commitRunMMXInCopy(t, a, fakeRunMMX(records, "echo 'TASK [free disk space]'\n"+waitForFileInShell(filepath.Join(records, "never"))))
		request := mmxRunRequest(t, a, "00000000000000a1")
		decisions := make(chan TriageAgentPlaybookRunDecision, 1)
		go func() {
			decisions <- a.RunPlaybook(context.Background(), request, testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", "HEAD"))
		}()
		waitForCondition(t, "MMX to start", fileExists(filepath.Join(records, "arguments")))
		lockEveryoneOut(t, lockOut)
		a.everyoneLockedOut()
		select {
		case decision := <-decisions:
			if decision.Outcome != triageagent.RanAndFailed || !strings.Contains(decision.Message, "MMHotline ended the MMX run because "+everyoneLockedOutReason) {
				t.Fatalf("%+v", decision)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the MMX run went on during the lock-out")
		}
	})
}

// A run that finished before the lock-out returned commits that wait for the
// copy's lock: none of them reaches origin during the lock-out. A look, such
// as the background loop's, ends the wait at once; without one, MMHotline
// looks again under the copy's lock just before the push. Either way the
// commits stay saved, as at MMHotline's stop, for the first run after the
// lock-out.
func TestALockOutKeepsAFinishedRunsCommitsFromOrigin(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name             string
		aLookSeesTheFile bool
	}{
		{"a look sees the file during the wait for the copy's lock", true},
		{"no look sees the file before the push", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newHeldWarnRig(t, 10)
			lockOut := lockOutFileForTest(t, r.a)
			r.warnArrives(t, loadWarning, "app002-load")
			run := r.nextRun(t)
			copyDirectory := filepath.Join(r.a.Store.Dir, systemsrepository.DirectoryName)
			originMain := func() string { return testGit(t, copyDirectory, "ls-remote", "origin", "refs/heads/main") }
			before := originMain()
			bundle := agentCommitsForTest(t, run.request, "triage_agent_notes/2026-11-24_load_on_app002.md")
			release := holdCopyLock(t, r.a, everyServerHolder)
			sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Done.", AgentCommitsBundle: bundle})
			waitUntil(t, "the finished run's commits waiting for the copy's lock", func() bool { return mmhotlineWaitsForTheCopyLock(r.a) })
			lockEveryoneOut(t, lockOut)
			if test.aLookSeesTheFile {
				r.a.everyoneLockedOut() // the background loop's look, every second
			} else {
				release()
			}
			waitUntil(t, "the finished run's record", func() bool {
				_, state, _, _ := latestRunOf(t, r.a, "triage_session")
				return state == "finished"
			})
			if after := originMain(); after != before {
				t.Fatalf("origin's main moved during the lock-out: %s, then %s", before, after)
			}
			runID, _, _, _ := latestRunOf(t, r.a, "triage_session")
			var recorded bool
			if err := r.a.Store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM triage_agent_commits WHERE run_id=?)`, runID).Scan(&recorded); err != nil || recorded {
				t.Fatalf("an outcome was recorded for commits MMHotline did not handle: %v", err)
			}
			if saved, err := os.ReadDir(filepath.Join(r.a.Store.Dir, returnedAgentCommitsDirectoryName)); err != nil || len(saved) != 1 {
				t.Fatalf("the saved commits: %v %v", saved, err)
			}
		})
	}
}
