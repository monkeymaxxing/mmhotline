package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// A run's start waits for the systems repository copy's lock while another
// holder, such as an MMX run, has it. The wait is not a failure, a stop ends
// it at once, and MMHotline's stop or restart during it, or while a finished
// run's commits wait for the lock, loses neither the run's alerts nor the
// agent's commits: docs/changing-mmhotline/design/triage-agent.md#waiting-for-the-copys-lock.

// everyServerHolder is the line an MMX run of the Stack's every_server
// playbook writes into the copy's lock, as at monkeymaxxing.com at 18:08.
const everyServerHolder = "mmhotline: mmx run every_server --inventory production for the triage agent, started 2026-11-24T18:08:00Z, process 4242"

// holdCopyLock takes MMHotline's copy lock as another holder does, with its
// holder line, until release or the test's end.
func holdCopyLock(t *testing.T, a *App, holderLine string) (release func()) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(holderLine + "\n"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			file.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// waitUntil polls until happened, with a deadline far above the normal time.
func waitUntil(t *testing.T, what string, happened func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !happened(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what + " did not happen")
		}
	}
}

// mmhotlineWaitsForTheCopyLock says whether one of MMHotline's waiters waits
// for the lock: the marker beside the lock, which other holders read.
func mmhotlineWaitsForTheCopyLock(a *App) bool {
	_, err := os.Stat(systemsrepository.CopyLockWaitingPath(filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)))
	return err == nil
}

// latestRunOf reads the latest run of purpose as recorded.
func latestRunOf(t *testing.T, a *App, purpose string) (id, state, reason, cause string) {
	t.Helper()
	if err := a.Store.db.QueryRow(`SELECT id,state,failure_reason,end_cause FROM triage_agent_runs WHERE purpose=? ORDER BY started_at DESC,rowid DESC LIMIT 1`, purpose).Scan(&id, &state, &reason, &cause); err != nil {
		t.Fatal(err)
	}
	return id, state, reason, cause
}

// openSessionID is the open triage session's ID.
func openSessionID(t *testing.T, a *App) string {
	t.Helper()
	var id string
	if err := a.Store.db.QueryRow(`SELECT id FROM triage_sessions WHERE ended_at=0`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// restartHeldWarnRig starts MMHotline again on the same state directory
// after the old App stopped, with its sending services and fake sandbox.
func restartHeldWarnRig(t *testing.T, r *heldWarnRig) {
	t.Helper()
	r.a = restartApp(t, r.a)
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
		t.Fatal(err)
	}
	r.runs = triageRunFixture(t, r.a)
}

func (r *heldWarnRig) nextRun(t *testing.T) fakeTriageRun {
	t.Helper()
	select {
	case run := <-r.runs:
		r.started = append(r.started, run)
		return run
	case <-time.After(10 * time.Second):
		t.Fatal("no triage agent run started")
		return fakeTriageRun{}
	}
}

// A run's start that waits minutes for an MMX run's hold of the copy's lock
// is not a failure: the run waits, the session's page says what for, the
// held warn stays held, and the run starts once the lock is free.
func TestTriageRunStartWaitsForTheCopyLock(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 10)
	release := holdCopyLock(t, r.a, everyServerHolder)
	alert := r.warnArrives(t, loadWarning, "app002-load")
	waitUntil(t, "the run's start waiting for the copy's lock", func() bool { return mmhotlineWaitsForTheCopyLock(r.a) })
	page := "/triage-sessions/" + openSessionID(t, r.a)
	if body := r.get(t, page); !strings.Contains(body, "Waiting for the systems repository, which MMX is using: "+everyServerHolder) {
		t.Fatal("the session's page does not say that the run waits for the systems repository")
	}
	r.at(t, 8*time.Minute)
	if _, state, reason, _ := latestRunOf(t, r.a, "triage_session"); state != "running" {
		t.Fatalf("after 8 minutes of waiting the run is %s: %s", state, reason)
	}
	if got := r.pushovers(loadWarning); len(got) != 0 {
		t.Fatalf("the held warn was delivered while the run waited: %+v", got)
	}
	release()
	if run := r.nextRun(t); run.request.SystemsRepositoryBaseCommitID == "" {
		t.Fatal("the run started without the systems repository")
	}
	if _, _, ended := r.hold(t, alert.ID); ended {
		t.Fatal("the hold ended when the run started")
	}
	if body := r.get(t, page); strings.Contains(body, "Waiting for the systems repository") {
		t.Fatal("the session's page still says the run waits")
	}
}

// MMHotline's stop while a run's start waits ends the wait at once and cuts
// the run short, as any run MMHotline's stop cuts: after the restart the
// session's run starts again, and the held warn stays held. The same holds
// while the start waits in the inventory step, where mmx nodes hangs.
func TestStopWhileARunStartWaitsResumesAfterTheRestart(t *testing.T) {
	parallelStartingProcesses(t)
	for _, step := range []string{"the copy's lock", "the inventory step"} {
		t.Run(step, func(t *testing.T) {
			r := newHeldWarnRig(t, 10)
			release := func() {}
			if step == "the copy's lock" {
				release = holdCopyLock(t, r.a, everyServerHolder)
			} else {
				hangingMMXNodes(t, r.a)
			}
			alert := r.warnArrives(t, loadWarning, "app002-load")
			if step == "the copy's lock" {
				waitUntil(t, "the run's start waiting for the copy's lock", func() bool { return mmhotlineWaitsForTheCopyLock(r.a) })
			} else {
				waitUntil(t, "mmx nodes starting", func() bool { _, err := os.Stat(filepath.Join(r.a.Store.Dir, "mmx-nodes-started")); return err == nil })
			}
			r.advance(time.Minute)
			stopped := time.Now()
			r.a.CloseTriageAgent()
			if waited := time.Since(stopped); waited > 5*time.Second {
				t.Fatalf("MMHotline's stop waited %s for the run's start", waited)
			}
			if _, state, reason, cause := latestRunOf(t, r.a, "triage_session"); state != "failed" || reason != triageRunCutByStop || cause != string(runCutByMMHotlineStop) {
				t.Fatalf("the run cut short while it started: %s, %q, %s", state, reason, cause)
			}
			release()
			restartHeldWarnRig(t, r)
			r.a.triageAgent.mmxSearchPath = t.TempDir()
			backgroundRounds(t, r.a)
			r.nextRun(t)
			r.at(t, 9*time.Minute)
			if got := r.pushovers(loadWarning); len(got) != 0 {
				t.Fatalf("the held warn was delivered: %+v", got)
			}
			if _, _, ended := r.hold(t, alert.ID); ended {
				t.Fatal("the hold ended")
			}
		})
	}
}

// hangingMMXNodes gives the copy an inventory and puts a stand-in for mmx on
// the App's search path whose mmx nodes never ends.
func hangingMMXNodes(t *testing.T, a *App) {
	t.Helper()
	copyDirectory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	// Untracked, so the working tree's commit is still the checkout's.
	if err := os.MkdirAll(filepath.Join(copyDirectory, "inventories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDirectory, "inventories", "production.yml"), []byte(systemsRepositoryWithInventories["inventories/production.yml"]), 0o644); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	script := "#!/bin/sh\n: > '" + filepath.Join(a.Store.Dir, "mmx-nodes-started") + "'\nexec sleep 600\n"
	if err := os.WriteFile(filepath.Join(directory, "mmx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a.triageAgent.mmxSearchPath = directory
}

// An SOS that needs the one run stops a software review whose start waits
// for the copy's lock. The review ends stopped before it began, recorded as
// stopped for a triage session, so it neither counts toward the review
// schedule nor moves it; the session's run starts once the lock is free.
func TestSOSStopsASoftwareReviewWaitingForTheCopyLock(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageHoldRig(t)
	ctx := context.Background()
	if _, err := r.a.Store.db.Exec(`DELETE FROM triage_agent_runs WHERE purpose='software_review'`); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SetSoftwareReviewInterval(ctx, r.s.Principal, 24); err != nil {
		t.Fatal(err)
	}
	release := holdCopyLock(t, r.a, everyServerHolder)
	backgroundRounds(t, r.a)
	waitUntil(t, "the software review's start waiting for the copy's lock", func() bool { return mmhotlineWaitsForTheCopyLock(r.a) })
	reviewID, _, _, _ := latestRunOf(t, r.a, "software_review")
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	backgroundRounds(t, r.a)
	waitUntil(t, "the software review's end", func() bool {
		_, state, _, _ := latestRunOf(t, r.a, "software_review")
		return state != "running"
	})
	if id, state, reason, cause := latestRunOf(t, r.a, "software_review"); id != reviewID || state != "stopped" || reason != softwareReviewStoppedForTriageSession || cause != string(runStoppedForTriageSession) {
		t.Fatalf("the review stopped for the SOS: %s, %q, %s", state, reason, cause)
	}
	if err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		due, err := softwareReviewDueAt(ctx, tx, 24)
		if err == nil && !due.IsZero() {
			t.Errorf("the stopped review moved the next review to %s", due)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	backgroundRounds(t, r.a)
	waitUntil(t, "the session's run waiting for the copy's lock", func() bool {
		var running bool
		if err := r.a.Store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM triage_agent_runs WHERE purpose='triage_session' AND state='running')`).Scan(&running); err != nil {
			t.Fatal(err)
		}
		return running && mmhotlineWaitsForTheCopyLock(r.a)
	})
	release()
	if run := r.runStarted(t); !strings.Contains(run.request.Prompt, "Disk of db001 is 99% full") {
		t.Fatal("the run after the review is not the SOS's session's")
	}
	if r.woken(sos.IncidentID) {
		t.Fatal("the SOS woke the person on call before its deadline")
	}
}

// MMHotline's process ends abruptly while a run's start waits for the lock;
// the restart records the run as cut short, and an SOS whose deadline passed
// while MMHotline was down wakes the person on call at once.
func TestAbruptExitWhileARunStartWaits(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageHoldRig(t)
	release := holdCopyLock(t, r.a, everyServerHolder)
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	backgroundRounds(t, r.a)
	waitUntil(t, "the run's start waiting for the copy's lock", func() bool { return mmhotlineWaitsForTheCopyLock(r.a) })
	runID, _, _, _ := latestRunOf(t, r.a, "triage_session")
	// The store goes first, so nothing of the run's end is saved.
	if err := r.a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	r.a.CloseTriageAgent()
	release()
	r.advance(r.start.Add(6 * time.Minute).Sub(r.a.Now()))
	store, err := Open(r.a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(store)
	restarted.Now, restarted.WebURL, restarted.ContactMethodTypes = r.a.Now, r.a.WebURL, r.a.ContactMethodTypes
	t.Cleanup(func() { restarted.Store.Close() })
	r.a = restarted
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
		t.Fatal(err)
	}
	r.runs = triageRunFixture(t, r.a)
	if id, state, reason, cause := latestRunOf(t, r.a, "triage_session"); id != runID || state != "failed" || reason != triageRunCutByRestart || cause != string(runCutByMMHotlineRestart) {
		t.Fatalf("the run the exit cut short: %s, %q, %s", state, reason, cause)
	}
	backgroundRounds(t, r.a)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the SOS whose deadline passed while MMHotline was down did not wake the person on call at once")
	}
}

// agentCommitsForTest makes the agent's commits on the run's base, as the
// sandbox bundles them: one commit that adds a note.
func agentCommitsForTest(t *testing.T, request triageagent.Request, notePath string) []byte {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "agent_copy")
	if err := systemsrepository.PrepareAgentCopy(context.Background(), directory, request.SystemsRepositoryBundle, request.SystemsRepositoryBaseCommitID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, notePath), []byte("# What the agent found\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, directory, "add", notePath)
	testGit(t, directory, "commit", "--quiet", "-m", "Note what the agent found")
	bundle, err := systemsrepository.AgentCommitsBundle(context.Background(), directory, request.SystemsRepositoryBaseCommitID)
	if err != nil || len(bundle) == 0 {
		t.Fatalf("the agent's commits: %v", err)
	}
	return bundle
}

// MMHotline's stop while a finished run's commits wait for the copy's lock
// ends the wait at once and keeps the commits: they are saved in the state
// directory, and the next run after the restart pushes them first, so it
// starts from them.
func TestStopWhileAFinishedRunsCommitsWaitForTheLock(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 10)
	r.warnArrives(t, loadWarning, "app002-load")
	run := r.nextRun(t)
	const note = "triage_agent_notes/2026-11-24_load_on_app002.md"
	bundle := agentCommitsForTest(t, run.request, note)
	release := holdCopyLock(t, r.a, everyServerHolder)
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Done.", AgentCommitsBundle: bundle})
	waitUntil(t, "the finished run's commits waiting for the copy's lock", func() bool { return mmhotlineWaitsForTheCopyLock(r.a) })
	stopped := time.Now()
	r.a.CloseTriageAgent()
	if waited := time.Since(stopped); waited > 5*time.Second {
		t.Fatalf("MMHotline's stop waited %s for the commits", waited)
	}
	runID, state, _, _ := latestRunOf(t, r.a, "triage_session")
	if state != "finished" {
		t.Fatalf("the finished run is %s", state)
	}
	var recorded bool
	if err := r.a.Store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM triage_agent_commits WHERE run_id=?)`, runID).Scan(&recorded); err != nil || recorded {
		t.Fatalf("an outcome was recorded for commits MMHotline did not handle: %v", err)
	}
	saved, err := os.ReadDir(filepath.Join(r.a.Store.Dir, returnedAgentCommitsDirectoryName))
	if err != nil || len(saved) != 1 {
		t.Fatalf("the saved commits: %v %v", saved, err)
	}
	release()
	restartHeldWarnRig(t, r)
	// The next alert's run handles the saved commits before it starts.
	r.advance(time.Minute)
	admitSignedAlert(t, r.a, r.source, "warn", "Disk of app002 is 91% full", "app002-disk")
	backgroundRounds(t, r.a)
	next := r.nextRun(t)
	copyDirectory := filepath.Join(r.a.Store.Dir, systemsrepository.DirectoryName)
	testGit(t, copyDirectory, "cat-file", "-e", next.request.SystemsRepositoryBaseCommitID+":"+note)
	var outcome string
	if err := r.a.Store.db.QueryRow(`SELECT outcome FROM triage_agent_commits WHERE run_id=? AND request_id=''`, runID).Scan(&outcome); err != nil || outcome != "pushed" {
		t.Fatalf("the saved commits' outcome: %q %v", outcome, err)
	}
	if saved, err := os.ReadDir(filepath.Join(r.a.Store.Dir, returnedAgentCommitsDirectoryName)); err != nil || len(saved) != 0 {
		t.Fatalf("the saved commits stayed after they were pushed: %v %v", saved, err)
	}
}
