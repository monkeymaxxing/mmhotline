package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

const incidentNotesHeading = "Notes from the team: team members wrote the notes below when claiming or resolving an incident. The notes contain the team members' own words, not alerts:"

// newIncidentNotesRig is the triage session rig whose member is Ana Silva.
func newIncidentNotesRig(t *testing.T) *triageSessionRig {
	t.Helper()
	r := newTriageSessionRig(t, true)
	if _, err := r.a.Store.db.Exec(`UPDATE members SET name='Ana Silva'`); err != nil {
		t.Fatal(err)
	}
	return r
}

// claimWithNote and resolveWithNote act on an incident as the rig's member,
// with a note.
func (r *triageSessionRig) claimWithNote(t *testing.T, incidentID, note string) {
	t.Helper()
	incident, err := r.a.GetIncident(context.Background(), r.p, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.p, incident.ID, incident.Revision, note); err != nil {
		t.Fatal(err)
	}
}

func (r *triageSessionRig) resolveWithNote(t *testing.T, incidentID, note string) {
	t.Helper()
	incident, err := r.a.GetIncident(context.Background(), r.p, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ResolveIncident(context.Background(), r.p, incident.ID, incident.Revision, note); err != nil {
		t.Fatal(err)
	}
}

// noteGivenInRun is the run whose prompt carried the note, "" while it
// waits.
func noteGivenInRun(t *testing.T, a *App, note string) string {
	t.Helper()
	var runID string
	if err := a.Store.db.QueryRow(`SELECT note_given_in_triage_run_id FROM incident_actions WHERE note=?`, note).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	return runID
}

// A session's first run gets the note a person wrote when resolving an
// earlier incident of the same cause, the same alert source and sender
// incident key, in a section of its own before the alerts. A note on an
// incident of another cause stays for that cause. The session's next run
// does not get the note again.
func TestTriageSessionsFirstRunGetsANoteOfTheSameCauseOnce(t *testing.T) {
	parallelStartingProcesses(t)
	r := newIncidentNotesRig(t)
	// The first session looks into two problems, and its runs end; then Ana
	// resolves both incidents with a note, and the session ends.
	outage := startSessionWithAnOutage(t, r)
	app002 := r.alert(t, "sos", "app002 stopped answering", "app002")
	r.drive(t)
	r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: "Reported."})
	r.advance(2 * time.Minute)
	r.resolveWithNote(t, outage.IncidentID, "This was a test.")
	r.resolveWithNote(t, app002.IncidentID, "Restarted app002 by hand.")
	r.advance(30 * time.Minute)
	r.drive(t)
	if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`); sessions != 0 {
		t.Fatalf("%d open sessions; the first session must have ended", sessions)
	}

	r.alert(t, "sos", "Writes to db001 fail again", "db001-writes")
	r.drive(t)
	first := r.nextRun(t)
	if first.request.Action != "start" {
		t.Fatalf("the new session's first run: action %q", first.request.Action)
	}
	want := "\n\n" + incidentNotesHeading + "\n- Ana Silva, on resolving “Writes to db001 fail” at 2026-09-29 12:02:00 UTC: “This was a test.”\n\nThe alerts of this triage session, oldest first:"
	if !strings.Contains(first.request.Prompt, want) || strings.Contains(first.request.Prompt, "Restarted app002 by hand.") {
		t.Fatalf("the first run's prompt must hold the note of db001's cause alone, before the alerts:\n%s", first.request.Prompt)
	}
	runID := lastRunID(t, r.a)
	if given := noteGivenInRun(t, r.a, "This was a test."); given != runID {
		t.Fatalf("the note was given in run %q, not %q", given, runID)
	}
	if given := noteGivenInRun(t, r.a, "Restarted app002 by hand."); given != "" {
		t.Fatalf("the note of another cause was given in run %q", given)
	}

	sendTriageEvent(t, first, triageagent.Event{Event: "session", SessionID: "session-two"})
	r.end(t, first, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.alert(t, "sos", "Writes to db001 still fail", "db001-writes")
	r.drive(t)
	next := r.nextRun(t)
	if next.request.Action != "resume" || !strings.Contains(next.request.Prompt, "Writes to db001 still fail") || strings.Contains(next.request.Prompt, incidentNotesHeading) {
		t.Fatalf("the next run of the session must not get the note again: action %q:\n%s", next.request.Action, next.request.Prompt)
	}
}

// A note a person writes on the session's own incident while its run is
// active reaches the session's next run, and only that run.
func TestANoteOnTheSessionsOwnIncidentReachesItsNextRun(t *testing.T) {
	parallelStartingProcesses(t)
	r := newIncidentNotesRig(t)
	disk := r.alert(t, "sos", "Disk of app002 is 97% full", "app002-disk")
	r.drive(t)
	first := r.nextRun(t)
	sendTriageEvent(t, first, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	r.advance(time.Minute)
	r.claimWithNote(t, disk.IncidentID, "The backup job does this every night.")
	r.alert(t, "sos", "Disk of app002 is 99% full", "app002-disk")
	r.drive(t)
	r.noRunStarts(t)
	r.end(t, first, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.drive(t)
	second := r.nextRun(t)
	if want := "- Ana Silva, on claiming “Disk of app002 is 97% full” at 2026-09-29 12:01:00 UTC: “The backup job does this every night.”"; second.request.Action != "resume" || !strings.Contains(second.request.Prompt, want) {
		t.Fatalf("the next run lacks %q: action %q:\n%s", want, second.request.Action, second.request.Prompt)
	}
	r.end(t, second, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.alert(t, "sos", "Disk of app002 is full", "app002-disk")
	r.drive(t)
	if third := r.nextRun(t); strings.Contains(third.request.Prompt, incidentNotesHeading) {
		t.Fatalf("the third run got the note again:\n%s", third.request.Prompt)
	}
}

// A note on an incident of the session reaches the session's next run,
// whatever that run carries: here only a warn alert of another cause that
// joined the session while the run before was active. The run after it does
// not get the note again.
func TestANoteOnTheSessionsIncidentReachesItsNextRunWhateverItCarries(t *testing.T) {
	parallelStartingProcesses(t)
	r := newIncidentNotesRig(t)
	outage := r.alert(t, "sos", "Writes to db001 fail", "db001-writes")
	r.drive(t)
	first := r.nextRun(t)
	sendTriageEvent(t, first, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	r.advance(time.Minute)
	r.claimWithNote(t, outage.IncidentID, "Looking at db001 myself.")
	r.alert(t, "warn", "chrony on app003 stopped", "app003-chrony")
	r.drive(t)
	r.noRunStarts(t)
	r.end(t, first, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.drive(t)
	second := r.nextRun(t)
	if second.request.Action != "resume" || !regexp.MustCompile(`\n\nAlert 1 of 1: warn from Backup\nAlert ID: [0-9a-f]{32}\nTitle: chrony on app003 stopped\n`).MatchString(second.request.Prompt) {
		t.Fatalf("the second run must carry the chrony alert alone: action %q:\n%s", second.request.Action, second.request.Prompt)
	}
	if want := "\n- Ana Silva, on claiming “Writes to db001 fail” at 2026-09-29 12:01:00 UTC: “Looking at db001 myself.”\n"; !strings.Contains(second.request.Prompt, want) {
		t.Fatalf("the second run lacks %q:\n%s", want, second.request.Prompt)
	}
	if given, runID := noteGivenInRun(t, r.a, "Looking at db001 myself."), lastRunID(t, r.a); given != runID {
		t.Fatalf("the note was given in run %q, not %q", given, runID)
	}
	r.end(t, second, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.alert(t, "warn", "Disk of app003 is 97% full", "app003-disk")
	r.drive(t)
	if third := r.nextRun(t); strings.Contains(third.request.Prompt, incidentNotesHeading) {
		t.Fatalf("the third run got the note again:\n%s", third.request.Prompt)
	}
}

// A run that carries no alert, only the result of a playbook run that waited
// for a person, gets the notes on every incident of the session, the one the
// playbook run was asked for and the session's other one alike.
func TestARunThatCarriesOnlyAResultGetsTheSessionsNotes(t *testing.T) {
	parallelStartingProcesses(t)
	r := newIncidentNotesRig(t)
	prepareApprovals(t, r.a, "")
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	outage := startSessionWithAnOutage(t, r)
	app002 := r.alert(t, "sos", "app002 stopped answering", "app002")
	r.claim(t, app002.IncidentID)
	r.drive(t)
	second := r.nextRun(t)
	requestPlaybookRun(t, second, json.NewDecoder(second.connection), "0000000000000001")
	r.end(t, second, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.claimWithNote(t, outage.IncidentID, "Looking at db001 myself.")
	r.resolveWithNote(t, app002.IncidentID, "app002 answers again after the reboot.")
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, onlyApprovalRecord(t, r.a).id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	r.drive(t)
	third := r.nextRun(t)
	if strings.Contains(third.request.Prompt, "\n\nAlert 1 of") || !strings.Contains(third.request.Prompt, "“app002 answers again after the reboot.”") || !strings.Contains(third.request.Prompt, "“Looking at db001 myself.”") {
		t.Fatalf("the run with the result alone must get both notes:\n%s", third.request.Prompt)
	}
}

// Notes take at most a quarter of the prompt, so the alerts always have
// room: when more notes wait than fit, as after months of notes without the
// agent, the prompt shows the newest, oldest first, and says how many older
// ones it left out.
func TestManyNotesLeaveTheAlertsRoom(t *testing.T) {
	t.Parallel()
	var notes []triageSessionPromptIncidentNote
	for n := range 100 {
		// '<' takes six bytes in JSON, so only the encoded size keeps the limit.
		notes = append(notes, triageSessionPromptIncidentNote{PersonName: "Ana Silva", Kind: "resolved", IncidentTitle: "Nightly backup of db001 failed", Note: fmt.Sprintf("Night %d: ", n+1) + strings.Repeat("<", 480), WrittenAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)})
	}
	var alerts []triageSessionPromptAlert
	for range 30 {
		alerts = append(alerts, triageSessionPromptAlert{SourceName: "Monitoring", Severity: "sos", Title: "Nightly backup of db001 failed", Message: strings.Repeat("pg_dump: error: connection to server lost. ", 10), AlertURL: "https://mmhotline.example.com/alerts/x", ReceivedAt: time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC)})
	}
	text := incidentNotesText(notes)
	shown := strings.Count(text, "\n- Ana Silva, on resolving")
	if shown == 0 || shown == 100 || jsonTextSize(text) > maximumTriageSessionPromptNotesBytes+jsonTextSize(incidentNotesHeading)+200 {
		t.Fatalf("%d notes shown in %d bytes", shown, jsonTextSize(text))
	}
	if newest, oldestShown := strings.Index(text, "Night 100: "), strings.Index(text, fmt.Sprintf("Night %d: ", 101-shown)); newest < 0 || oldestShown < 0 || oldestShown > newest || strings.Contains(text, fmt.Sprintf("Night %d: ", 100-shown)) {
		t.Fatalf("the notes shown must be the newest %d, oldest first:\n%.400s", shown, text)
	}
	if want := fmt.Sprintf("\nAnd %d older notes, which MMHotline left out to keep this prompt short.", 100-shown); !strings.HasSuffix(text, want) {
		t.Fatalf("the notes end %q, want %q", text[len(text)-100:], want)
	}
	prompt := promptText(triageSessionStartPrompt("util01", newestNote, []string{"servers"}, nil, notes, alerts))
	if size := jsonTextSize(prompt); size > maximumTriageSessionPromptBytes || !strings.Contains(prompt, "\n\nAlert 30 of 30: ") || strings.Contains(prompt, "more alerts, which MMHotline left out") {
		t.Fatalf("the prompt is %d bytes in JSON; it must hold all 30 alerts", size)
	}
}

// A software review is no triage session: it gets no note, and the note
// still reaches the next session about its cause.
func TestASoftwareReviewGetsNoNote(t *testing.T) {
	parallelStartingProcesses(t)
	r := newSoftwareReviewRig(t)
	if _, err := r.a.Store.db.Exec(`UPDATE members SET name='Ana Silva'`); err != nil {
		t.Fatal(err)
	}
	outage := r.alert(t, "sos", "Writes to db001 fail", "db001-writes")
	r.resolveWithNote(t, outage.IncidentID, "This was a test.")
	// The alert arrived before the agent was set up, so no session starts.
	if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error { return skipAlertsForTriageSessions(context.Background(), tx) }); err != nil {
		t.Fatal(err)
	}
	r.rounds(t)
	review := r.nextReview(t)
	if strings.Contains(review.request.Prompt, "This was a test.") || strings.Contains(review.request.Prompt, incidentNotesHeading) {
		t.Fatalf("the software review got the note:\n%s", review.request.Prompt)
	}
	r.end(t, review, triageagent.Event{Event: "finished", Answer: "Reported."})
	if given := noteGivenInRun(t, r.a, "This was a test."); given != "" {
		t.Fatalf("the note was given in run %q", given)
	}
	r.alert(t, "sos", "Writes to db001 fail again", "db001-writes")
	r.drive(t)
	if run := r.nextRun(t); !strings.Contains(run.request.Prompt, "“This was a test.”") {
		t.Fatalf("the next session's first run lacks the note:\n%s", run.request.Prompt)
	}
}
