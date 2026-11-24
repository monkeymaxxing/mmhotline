package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/fakesendingservices"
	"mmhotline/internal/triageagent"
)

// triageSessionRig is a team of one with an alert source, an email contact
// method for warn and sos and a waking one for sos, the triage agent set up,
// MMHotline's copy of a systems repository whose origin holds earlier notes,
// a fake sandbox that hands each run to the test, and a clock the test moves.
type triageSessionRig struct {
	a       *App
	p       Principal
	source  CreatedSource
	runs    <-chan fakeTriageRun
	advance func(time.Duration)
}

const newestNote = "triage_agent_notes/2026-09-28_disk_full_on_app002.md"

func newTriageSessionRig(t *testing.T, withCopy bool) *triageSessionRig {
	t.Helper()
	a, p, source := alertFixture(t)
	r := &triageSessionRig{a: a, p: p, source: source, advance: deliveryTestClock(a)}
	member, err := a.CurrentMember(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for contactMethodType, severities := range map[string][]string{"email": {"warn", "sos"}, "test_waking": {"sos"}} {
		id := NewID()
		if _, err := a.Store.db.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) VALUES(?,?,?,'{}',?,1,1,0)`, id, member.ID, contactMethodType, contactMethodType+"@example.test"); err != nil {
			t.Fatal(err)
		}
		for _, severity := range severities {
			if _, err := a.Store.db.Exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,?)`, id, severity); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.runs = setUpTriageAgentForTest(t, a, withCopy)
	return r
}

// setUpTriageAgentForTest saves the triage agent's setup, with MMHotline's
// copy of a systems repository whose origin holds earlier notes when withCopy
// is set, and installs a fake sandbox that hands each run to the test.
func setUpTriageAgentForTest(t *testing.T, a *App, withCopy bool) <-chan fakeTriageRun {
	t.Helper()
	if withCopy {
		root := t.TempDir()
		origin, seed := filepath.Join(root, "origin.git"), filepath.Join(root, "seed")
		testGit(t, root, "init", "--quiet", "--bare", "--initial-branch=main", origin)
		testGit(t, root, "clone", "--quiet", origin, seed)
		if err := os.MkdirAll(filepath.Join(seed, "triage_agent_notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The newest note by its date, not the last name: README.md sorts after it.
		for _, path := range []string{"AGENTS.md", "triage_agent_notes/2026-09-27_backup_failed_on_db001.md", newestNote, "triage_agent_notes/README.md"} {
			if err := os.WriteFile(filepath.Join(seed, path), []byte("# "+path+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(seed, softwareReviewTaskFile), []byte(rigSoftwareReviewTask), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, seed, "add", "--all")
		testGit(t, seed, "commit", "--quiet", "-m", "Describe the servers")
		testGit(t, seed, "push", "--quiet", "origin", "main")
		testGit(t, root, "clone", "--quiet", origin, filepath.Join(a.Store.Dir, "systems_repository"))
	}
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_setup(id,coding_agent,credential_type,credential,model,reasoning_effort,set_up_at,set_up_by) VALUES(1,'codex','api_key','sk-test-credential','test-model','high',1,'administrator')`); err != nil {
		t.Fatal(err)
	}
	// A software review finished just now, so none is due while a test
	// of sessions runs; the review's own tests remove it.
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by,ended_at) VALUES(?,'software_review','codex','api_key','finished',?,'MMHotline',?)`, NewID(), a.Now().Unix(), a.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return triageRunFixture(t, a)
}

// alert admits a signed alert from the rig's alert source; key, when set, is
// its sender incident key.
func (r *triageSessionRig) alert(t *testing.T, severity, title, key string) Alert {
	t.Helper()
	return admitSignedAlert(t, r.a, r.source, severity, title, key)
}

// admitSignedAlert admits a signed alert from an alert source; key, when
// set, is its sender incident key.
func admitSignedAlert(t *testing.T, a *App, source CreatedSource, severity, title, key string) Alert {
	t.Helper()
	fields := map[string]string{"severity": severity, "title": title, "message": "Details of " + title}
	if key != "" {
		fields["sender_incident_key"] = key
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	alert, _, _, err := a.AdmitGeneric(context.Background(), source.Source.ID, alertSignature(source.Secret, a.Now(), body), body)
	if err != nil {
		t.Fatal(err)
	}
	return alert
}

// drive is the background loop's turn for triage sessions.
func (r *triageSessionRig) drive(t *testing.T) {
	t.Helper()
	if err := r.a.DriveTriageSession(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *triageSessionRig) nextRun(t *testing.T) fakeTriageRun {
	t.Helper()
	select {
	case run := <-r.runs:
		return run
	case <-time.After(10 * time.Second):
		t.Fatal("no triage agent run started")
		return fakeTriageRun{}
	}
}

// noRunStarts checks that nothing is starting and no run reached the sandbox.
func (r *triageSessionRig) noRunStarts(t *testing.T) {
	t.Helper()
	waitTriageIdleOrRunning(t, r.a)
	select {
	case run := <-r.runs:
		t.Fatalf("a run started: %+v", run.request.Action)
	default:
	}
}

// waitTriageIdleOrRunning returns once no session run is starting.
func waitTriageIdleOrRunning(t *testing.T, a *App) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		a.triageAgent.mu.Lock()
		starting := a.triageAgent.backgroundRunStarting
		a.triageAgent.mu.Unlock()
		if !starting {
			return
		}
	}
	t.Fatal("a run is still starting")
}

// end sends the run's final event and waits until MMHotline handled it.
func (r *triageSessionRig) end(t *testing.T, run fakeTriageRun, event triageagent.Event) {
	t.Helper()
	sendTriageEvent(t, run, event)
	waitNoTriageRun(t, r.a)
}

func waitNoTriageRun(t *testing.T, a *App) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		a.triageAgent.mu.Lock()
		active := a.triageAgent.activeRun
		a.triageAgent.mu.Unlock()
		if active == nil {
			return
		}
	}
	t.Fatal("the run did not end")
}

// waitSessionIDSaved waits until MMHotline saved the coding agent's session
// ID the run reported, as the run's reader does in order.
func waitSessionIDSaved(t *testing.T, a *App, sessionID string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		var saved bool
		if err := a.Store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM triage_agent_runs WHERE coding_agent_session_id=?)`, sessionID).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		if saved {
			return
		}
	}
	t.Fatal("the session ID was not saved")
}

func countRows(t *testing.T, a *App, query string, args ...any) int {
	t.Helper()
	var count int
	if err := a.Store.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// promptText is the prompt of triageSessionStartPrompt or
// triageSessionResumePrompt, without the alerts the run carries.
func promptText(prompt string, _ []string) string { return prompt }

// report sends the agent's report and returns MMHotline's answer.
func report(t *testing.T, run fakeTriageRun, answers *json.Decoder, requestID, severity, text string) triageagent.Answer {
	t.Helper()
	sendTriageEvent(t, run, triageagent.Event{Event: "report", RequestID: requestID, AlertSeverity: severity, ReportText: text})
	run.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	var control triageagent.Control
	if err := answers.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != requestID {
		t.Fatalf("answer: %+v %v", control, err)
	}
	return *control.Answer
}

// A warn or sos alert starts a session and its run, whose task names the
// alert, the last session's note, and the two commands. Nothing else starts
// one.
func TestTriageSessionStartsWithAWarnOrSOSAlert(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("a warn alert starts it, an info alert does not", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		r.alert(t, "info", "Nightly backup finished", "")
		r.drive(t)
		r.noRunStarts(t)
		if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 0 {
			t.Fatalf("an info alert started %d sessions", sessions)
		}
		warn := r.alert(t, "warn", "Disk of app002 is 97% full", "")
		r.drive(t)
		run := r.nextRun(t)
		if run.request.Action != "start" || run.request.SessionID != "" || run.request.SystemsRepositoryBaseCommitID == "" {
			t.Fatalf("request: action %q, session %q, base %q", run.request.Action, run.request.SessionID, run.request.SystemsRepositoryBaseCommitID)
		}
		for _, want := range []string{"Disk of app002 is 97% full", "The last triage session left its note in " + newestNote + ": read the note for what happened before.", "mmhotline triage-agent request-playbook-run", "mmhotline triage-agent report", "Your rules are in triage_agent_rules.md in this repository. Read that whole file first", "https://mmhotline.example.com/alerts/" + warn.ID} {
			if !strings.Contains(run.request.Prompt, want) {
				t.Errorf("the prompt lacks %q:\n%s", want, run.request.Prompt)
			}
		}
		if strings.Contains(run.request.Prompt, "Nightly backup finished") {
			t.Errorf("the prompt holds the info alert:\n%s", run.request.Prompt)
		}
		if count := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts s JOIN triage_agent_runs r ON r.id=s.sent_in_run_id AND r.triage_session_id=s.triage_session_id WHERE s.alert_id=?`, warn.ID); count != 1 {
			t.Fatal("the run did not carry the alert in its session")
		}
	})
	t.Run("an SOS alert links its incident", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		sos := r.alert(t, "sos", "Database on db001 is down", "db001")
		r.drive(t)
		if run := r.nextRun(t); !strings.Contains(run.request.Prompt, "Incident: https://mmhotline.example.com/incidents/"+sos.IncidentID) {
			t.Fatalf("prompt:\n%s", run.request.Prompt)
		}
	})
	t.Run("nothing starts when the agent is not set up", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		if _, err := r.a.Store.db.Exec(`DELETE FROM triage_agent_setup`); err != nil {
			t.Fatal(err)
		}
		r.alert(t, "sos", "Database on db001 is down", "db001")
		r.drive(t)
		r.noRunStarts(t)
		if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 0 {
			t.Fatalf("%d sessions", sessions)
		}
	})
	t.Run("nothing starts without MMHotline's copy", func(t *testing.T) {
		r := newTriageSessionRig(t, false)
		r.alert(t, "warn", "Disk of app002 is 97% full", "")
		r.drive(t)
		r.noRunStarts(t)
		if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 0 {
			t.Fatalf("%d sessions", sessions)
		}
	})
}

// An alert that arrives during a run waits and goes with a resume of the
// same agent session when the run ends; one that arrives while no run is
// active resumes the session at once.
func TestTriageSessionResumesWithAlertsThatArrived(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.drive(t)
	first := r.nextRun(t)
	sendTriageEvent(t, first, triageagent.Event{Event: "session", SessionID: "session-one"})
	r.alert(t, "sos", "app002 stopped answering", "app002")
	r.drive(t)
	r.noRunStarts(t)
	r.end(t, first, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.drive(t)
	second := r.nextRun(t)
	if second.request.Action != "resume" || second.request.SessionID != "session-one" {
		t.Fatalf("second run: action %q, session %q", second.request.Action, second.request.SessionID)
	}
	if !strings.Contains(second.request.Prompt, "app002 stopped answering") || strings.Contains(second.request.Prompt, "Disk of app002") {
		t.Fatalf("the resume must carry only the new alert:\n%s", second.request.Prompt)
	}
	sendTriageEvent(t, second, triageagent.Event{Event: "session", SessionID: "session-one"})
	r.end(t, second, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.drive(t)
	r.noRunStarts(t)
	r.alert(t, "warn", "Disk of app003 is 95% full", "")
	r.drive(t)
	third := r.nextRun(t)
	if third.request.Action != "resume" || third.request.SessionID != "session-one" || !strings.Contains(third.request.Prompt, "Disk of app003 is 95% full") {
		t.Fatalf("third run: action %q, session %q:\n%s", third.request.Action, third.request.SessionID, third.request.Prompt)
	}
	if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 1 {
		t.Fatalf("%d sessions; every alert joins the one session", sessions)
	}
}

type reportDelivery struct{ contactMethodType, severity, state, reason string }

// reportOutcome reads the alert a report became and its deliveries.
func reportOutcome(t *testing.T, a *App, reportText string) (Alert, []reportDelivery) {
	t.Helper()
	var alertID string
	if err := a.Store.db.QueryRow(`SELECT related_alert_id FROM incident_actions WHERE kind='triage_agent_report' AND message=? LIMIT 1`, reportText).Scan(&alertID); err != nil {
		t.Fatalf("no incident holds the report: %v", err)
	}
	alert, err := scanAlert(a.Store.db.QueryRow(`SELECT `+alertColumns+` FROM alerts WHERE id=?`, alertID))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.Store.db.Query(`SELECT contact_method_type,severity,state,reason FROM deliveries WHERE alert_id=? AND purpose!='sos_escalation' ORDER BY contact_method_type`, alertID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var deliveries []reportDelivery
	for rows.Next() {
		var d reportDelivery
		if err := rows.Scan(&d.contactMethodType, &d.severity, &d.state, &d.reason); err != nil {
			t.Fatal(err)
		}
		deliveries = append(deliveries, d)
	}
	return alert, deliveries
}

// The agent's report is recorded with every incident of the session and
// reaches people at its alert severity; at sos, the person on call is woken
// by an incident's SOS escalation.
func TestTriageAgentReportReachesPeople(t *testing.T) {
	parallelStartingProcesses(t)
	const reportText = "# Freed disk space on db001\n\nDeleted old logs; 40% free now.\n"
	t.Run("warn", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		first := r.alert(t, "sos", "Database on db001 is down", "db001")
		second := r.alert(t, "sos", "Backups on db001 fail", "db001-backup")
		r.drive(t)
		run := r.nextRun(t)
		if answer := report(t, run, json.NewDecoder(run.connection), "0000000000000001", "warn", reportText); answer.Outcome != triageagent.ReportRecorded {
			t.Fatalf("answer: %+v", answer)
		}
		for _, incidentID := range []string{first.IncidentID, second.IncidentID} {
			if count := countRows(t, r.a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='triage_agent_report' AND message=?`, incidentID, reportText); count != 1 {
				t.Fatalf("incident %s has %d records of the report", incidentID, count)
			}
		}
		alert, deliveries := reportOutcome(t, r.a, reportText)
		if alert.Severity != "warn" || alert.Title != "The AI triage agent reports: Freed disk space on db001" || alert.IncidentID == "" {
			t.Fatalf("report alert: %+v", alert)
		}
		if want := []reportDelivery{{"email", "warn", "queued", ""}}; !reflect.DeepEqual(deliveries, want) {
			t.Fatalf("deliveries %+v, want %+v", deliveries, want)
		}
	})
	t.Run("sos with an open incident", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		sos := r.alert(t, "sos", "Database on db001 is down", "db001")
		r.drive(t)
		run := r.nextRun(t)
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "sos", reportText)
		alert, deliveries := reportOutcome(t, r.a, reportText)
		if alert.Severity != "sos" || alert.IncidentID != sos.IncidentID {
			t.Fatalf("the report must be about the open incident: %+v", alert)
		}
		// The incident's escalation wakes the person on call; the report itself wakes nobody twice.
		if want := []reportDelivery{{"email", "sos", "queued", ""}, {"test_waking", "sos", "suppressed", "SOS contact method type follows the active target"}}; !reflect.DeepEqual(deliveries, want) {
			t.Fatalf("deliveries %+v, want %+v", deliveries, want)
		}
		if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND contact_method_type='test_waking' AND state='queued'`, sos.IncidentID); count == 0 {
			t.Fatal("the incident's escalation does not wake the person on call")
		}
		if incidents := countRows(t, r.a, `SELECT count(*) FROM incidents`); incidents != 1 {
			t.Fatalf("%d incidents; the report must go through the open one", incidents)
		}
	})
	t.Run("sos with no unresolved incident", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		r.alert(t, "warn", "Disk of db001 is 97% full", "")
		resolved := r.alert(t, "sos", "Database on db001 is down", "db001")
		r.drive(t)
		run := r.nextRun(t)
		incident, err := r.a.GetIncident(context.Background(), r.p, resolved.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.a.ResolveIncident(context.Background(), r.p, incident.ID, incident.Revision, ""); err != nil {
			t.Fatal(err)
		}
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "sos", reportText)
		alert, _ := reportOutcome(t, r.a, reportText)
		if alert.Severity != "sos" || alert.IncidentID == "" || alert.IncidentID == resolved.IncidentID {
			t.Fatalf("the report must open an SOS incident of its own: %+v", alert)
		}
		if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND contact_method_type='test_waking' AND state='queued'`, alert.IncidentID); count == 0 {
			t.Fatal("the report's incident does not wake the person on call")
		}
		if count := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=? AND sent_in_run_id=?`, alert.ID, lastRunID(t, r.a)); count != 1 {
			t.Fatal("the report's incident did not join the session as already sent")
		}
		// Its incident keeps the session open after the quiet time.
		r.advance(time.Hour)
		r.drive(t)
		if open := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`); open != 1 {
			t.Fatal("the session ended while the report's incident is open")
		}
	})
}

func lastRunID(t *testing.T, a *App) string {
	t.Helper()
	var id string
	if err := a.Store.db.QueryRow(`SELECT id FROM triage_agent_runs ORDER BY started_at DESC,rowid DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A session ends once every incident of it is resolved and no alert joined
// it for 30 minutes; its run still going is stopped, and the next alert
// starts a new session with a new agent session.
func TestTriageSessionEndsWhenResolvedAndQuiet(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	sos := r.alert(t, "sos", "Database on db001 is down", "db001")
	r.drive(t)
	run := r.nextRun(t)
	controls := json.NewDecoder(run.connection)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	open := func() int { return countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`) }
	r.advance(40 * time.Minute)
	r.drive(t)
	if open() != 1 {
		t.Fatal("the session ended while its incident is unresolved")
	}
	// A warn alert joins: the 30 quiet minutes count from here.
	lastJoined := r.a.Now()
	r.alert(t, "warn", "Disk of db001 is 97% full", "")
	r.drive(t)
	r.advance(5 * time.Minute)
	incident, err := r.a.GetIncident(context.Background(), r.p, sos.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ResolveIncident(context.Background(), r.p, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.drive(t)
	r.advance(lastJoined.Add(30*time.Minute - time.Second).Sub(r.a.Now()))
	r.drive(t)
	if open() != 1 {
		t.Fatal("the session ended before 30 quiet minutes")
	}
	r.advance(time.Second)
	r.drive(t)
	var endedBecause string
	if err := r.a.Store.db.QueryRow(`SELECT ended_because FROM triage_sessions WHERE ended_at!=0`).Scan(&endedBecause); err != nil || !strings.Contains(endedBecause, "30 minutes") {
		t.Fatalf("ended because %q: %v", endedBecause, err)
	}
	var control triageagent.Control
	run.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := controls.Decode(&control); err != nil || !control.Stop {
		t.Fatalf("the run was not stopped: %+v %v", control, err)
	}
	r.end(t, run, triageagent.Event{Event: "stopped", SessionID: "session-one"})
	r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.drive(t)
	next := r.nextRun(t)
	if next.request.Action != "start" || next.request.SessionID != "" || strings.Contains(next.request.Prompt, "db001") {
		t.Fatalf("the next alert must start a new session: action %q, session %q:\n%s", next.request.Action, next.request.SessionID, next.request.Prompt)
	}
	if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 2 || open() != 1 {
		t.Fatalf("%d sessions, %d open", sessions, open())
	}
}

// resolve and claim act on an incident as the rig's member.
func (r *triageSessionRig) resolve(t *testing.T, incidentID string) {
	t.Helper()
	incident, err := r.a.GetIncident(context.Background(), r.p, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ResolveIncident(context.Background(), r.p, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
}

func (r *triageSessionRig) claim(t *testing.T, incidentID string) {
	t.Helper()
	incident, err := r.a.GetIncident(context.Background(), r.p, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ClaimIncident(context.Background(), r.p, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
}

// reportsInHistory counts the triage agent's reports in an incident's history.
func reportsInHistory(t *testing.T, a *App, incidentID string) int {
	t.Helper()
	return countRows(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='triage_agent_report'`, incidentID)
}

// startSessionWithAnOutage starts a session with an SOS whose run reports the
// coding agent session "session-one" and finishes; the SOS's incident stays
// open.
func startSessionWithAnOutage(t *testing.T, r *triageSessionRig) Alert {
	t.Helper()
	outage := r.alert(t, "sos", "Writes to db001 fail", "db001-writes")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	return outage
}

// The lab's case: the session's SOS was resolved long ago and nothing of it
// waits, and a warn about another problem arrives less than 30 minutes after
// the last alert. It starts a new session, the old one ends with the reason,
// and the agent's report about the warn cites no incident and adds nothing
// to the resolved incident's history.
func TestTriageSessionNewProblemAfterItsWorkIsDone(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	outage := startSessionWithAnOutage(t, r)
	r.resolve(t, outage.IncidentID)
	old := openTriageSessionID(t, r.a)
	r.advance(10 * time.Minute)
	r.alert(t, "warn", "chrony on app003 stopped", "app003-chrony")
	r.drive(t)
	if because := triageSessionEndedBecause(t, r.a, old); because != "An alert about a new problem arrived after the session's work was done, and started a new session." {
		t.Fatalf("the old session ended because %q", because)
	}
	if openTriageSessionID(t, r.a) == old {
		t.Fatal("the warn joined the old session")
	}
	run := r.nextRun(t)
	if run.request.Action != "start" || run.request.SessionID != "" || strings.Contains(run.request.Prompt, "Writes to db001 fail") || !strings.Contains(run.request.Prompt, "chrony on app003 stopped") {
		t.Fatalf("the new session's run: action %q, session %q:\n%s", run.request.Action, run.request.SessionID, run.request.Prompt)
	}
	report(t, run, json.NewDecoder(run.connection), "0000000000000001", "info", "# Started chrony on app003\n")
	if alert := reportAlert(t, r.a, "The AI triage agent reports: Started chrony on app003"); alert.IncidentID != "" {
		t.Fatalf("the report cites incident %s", alert.IncidentID)
	}
	if reports := reportsInHistory(t, r.a, outage.IncidentID); reports != 0 {
		t.Fatalf("the resolved outage's history holds %d reports", reports)
	}
}

// After a session's work is done, an alert about a problem the session has,
// from the same alert source with the same sender incident key, joins it, so
// the agent keeps its context. The same key from another alert source, and
// an alert without a key, are new problems and start a new session.
func TestTriageSessionKeepsAProblemThatComesBack(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name       string
		comesBack  func(t *testing.T, r *triageSessionRig, other CreatedSource)
		newSession bool
	}{
		{"the same key", func(t *testing.T, r *triageSessionRig, _ CreatedSource) {
			r.alert(t, "sos", "Writes to db001 fail again", "db001-writes")
		}, false},
		{"the same key from another alert source", func(t *testing.T, r *triageSessionRig, other CreatedSource) {
			admitSignedAlert(t, r.a, other, "sos", "Writes to db001 fail", "db001-writes")
		}, true},
		{"no key", func(t *testing.T, r *triageSessionRig, _ CreatedSource) {
			r.alert(t, "warn", "Writes to db001 are slow", "")
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			other, err := createAlertSource(context.Background(), r.a, r.p, "Other monitoring")
			if err != nil {
				t.Fatal(err)
			}
			outage := startSessionWithAnOutage(t, r)
			r.resolve(t, outage.IncidentID)
			r.advance(10 * time.Minute)
			test.comesBack(t, r, other)
			r.drive(t)
			run := r.nextRun(t)
			sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`)
			if test.newSession && (sessions != 2 || run.request.Action != "start") {
				t.Fatalf("%d sessions, run action %q; want a new session", sessions, run.request.Action)
			}
			if !test.newSession && (sessions != 1 || run.request.Action != "resume" || run.request.SessionID != "session-one") {
				t.Fatalf("%d sessions, run action %q of %q; want a resume of the same session", sessions, run.request.Action, run.request.SessionID)
			}
		})
	}
}

// While the agent's work goes on, an alert about a new problem joins the
// session, as every alert did: while a run is going, and while an alert
// waits for its first run. The run that carries it reports about it alone.
func TestTriageSessionNewProblemJoinsWhileWorkGoesOn(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("while a run is going", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		outage := r.alert(t, "sos", "Writes to db001 fail", "db001-writes")
		r.drive(t)
		first := r.nextRun(t)
		sendTriageEvent(t, first, triageagent.Event{Event: "session", SessionID: "session-one"})
		waitSessionIDSaved(t, r.a, "session-one")
		r.resolve(t, outage.IncidentID)
		r.alert(t, "warn", "chrony on app003 stopped", "app003-chrony")
		r.drive(t)
		r.end(t, first, triageagent.Event{Event: "finished", Answer: "Reported."})
		r.drive(t)
		second := r.nextRun(t)
		if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 1 || second.request.Action != "resume" || !strings.Contains(second.request.Prompt, "chrony on app003 stopped") {
			t.Fatalf("%d sessions, second run action %q:\n%s", sessions, second.request.Action, second.request.Prompt)
		}
		report(t, second, json.NewDecoder(second.connection), "0000000000000001", "info", "# Started chrony on app003\n")
		if alert := reportAlert(t, r.a, "The AI triage agent reports: Started chrony on app003"); alert.IncidentID != "" {
			t.Fatalf("the report about chrony cites incident %s", alert.IncidentID)
		}
		if reports := reportsInHistory(t, r.a, outage.IncidentID); reports != 0 {
			t.Fatalf("the outage's history holds %d reports about chrony", reports)
		}
	})
	t.Run("while an alert waits for its first run", func(t *testing.T) {
		r := newTriageSessionRig(t, true)
		outage := startSessionWithAnOutage(t, r)
		r.resolve(t, outage.IncidentID)
		r.alert(t, "warn", "chrony on app003 stopped", "app003-chrony")
		r.alert(t, "warn", "Disk of app003 is 97% full", "app003-disk")
		r.drive(t)
		// The first warn starts a new session; the second joins it, as the
		// first waits for its run.
		run := r.nextRun(t)
		if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 2 || run.request.Action != "start" || !strings.Contains(run.request.Prompt, "chrony on app003 stopped") || !strings.Contains(run.request.Prompt, "Disk of app003 is 97% full") {
			t.Fatalf("%d sessions, run action %q:\n%s", sessions, run.request.Action, run.request.Prompt)
		}
	})
}

// A run started by a playbook run's result carries no alert: its report goes
// with an incident of the session's latest run that carried alerts, and the
// approval request that run made went with that incident too, not with the
// session's other problem.
func TestTriageAgentReportAfterAnApprovalResult(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	// db001's SOS stays open while app002's joins, which a person claims.
	outage := startSessionWithAnOutage(t, r)
	app002 := r.alert(t, "sos", "app002 stopped answering", "app002")
	r.claim(t, app002.IncidentID)
	r.drive(t)
	second := r.nextRun(t)
	requestPlaybookRun(t, second, json.NewDecoder(second.connection), "0000000000000001")
	r.end(t, second, triageagent.Event{Event: "finished", Answer: "Reported."})
	record := onlyApprovalRecord(t, r.a)
	if record.incidentID != app002.IncidentID {
		t.Fatalf("the approval request went with incident %s, not app002's %s", record.incidentID, app002.IncidentID)
	}
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	r.drive(t)
	third := r.nextRun(t)
	if third.request.Action != "resume" || strings.Contains(third.request.Prompt, "Alert 1 of") || countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE sent_in_run_id=?`, lastRunID(t, r.a)) != 0 {
		t.Fatalf("the third run must carry the result alone: action %q:\n%s", third.request.Action, third.request.Prompt)
	}
	report(t, third, json.NewDecoder(third.connection), "0000000000000001", "info", "# Rebooted app002\n")
	if alert := reportAlert(t, r.a, "The AI triage agent reports: Rebooted app002"); alert.IncidentID != app002.IncidentID {
		t.Fatalf("the report went with incident %q, not app002's %s", alert.IncidentID, app002.IncidentID)
	}
	if reports, outageReports := reportsInHistory(t, r.a, app002.IncidentID), reportsInHistory(t, r.a, outage.IncidentID); reports != 1 || outageReports != 0 {
		t.Fatalf("app002's history holds %d reports and db001's %d; want 1 and 0", reports, outageReports)
	}
}

// An sos report about a problem whose alert has no incident, a warn, opens an
// SOS incident of its own that wakes the person on call, though another
// problem's incident of the session is claimed: its owner has only that one.
func TestTriageAgentSOSReportAboutAnotherProblemWakes(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	outage := startSessionWithAnOutage(t, r)
	r.claim(t, outage.IncidentID)
	r.alert(t, "warn", "chrony on app003 stopped", "app003-chrony")
	r.drive(t)
	run := r.nextRun(t)
	report(t, run, json.NewDecoder(run.connection), "0000000000000001", "sos", "# app003 needs a person\n")
	alert := reportAlert(t, r.a, "The AI triage agent reports: app003 needs a person")
	if alert.IncidentID == "" || alert.IncidentID == outage.IncidentID {
		t.Fatalf("the report must open an SOS incident of its own: %+v", alert)
	}
	if count := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND contact_method_type='test_waking' AND state='queued'`, alert.IncidentID); count == 0 {
		t.Fatal("the report's incident does not wake the person on call")
	}
	if reports := reportsInHistory(t, r.a, outage.IncidentID); reports != 0 {
		t.Fatalf("the claimed outage's history holds %d reports about app003", reports)
	}
}

// A hand-over is about the whole session, so its report is recorded in the
// history of every incident of the session, though the latest run that
// carried alerts carried only one of them.
func TestHandOverReportReachesEveryIncidentOfTheSession(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	outage := startSessionWithAnOutage(t, r)
	app002 := r.alert(t, "sos", "app002 stopped answering", "app002")
	r.drive(t)
	r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: "Reported."})
	if err := r.a.TakeOverTriageSession(context.Background(), r.p, openTriageSessionID(t, r.a)); err != nil {
		t.Fatal(err)
	}
	r.drive(t)
	handOver := r.nextRun(t)
	report(t, handOver, json.NewDecoder(handOver.connection), "0000000000000001", "info", "# Hand-over: db001 and app002\n")
	for _, incidentID := range []string{outage.IncidentID, app002.IncidentID} {
		if reports := reportsInHistory(t, r.a, incidentID); reports != 1 {
			t.Errorf("incident %s holds %d reports; the hand-over's must reach each", incidentID, reports)
		}
	}
}

// triageHoldRig is a team of two through the fake sending services:
// Responder, on call all week, with Pushover for every severity and an
// automated voice call, and Barbara, the backup, with Pushover for every
// severity. It has an alert source, and the triage agent is set up with its
// fake sandbox and MMHotline's copy.
type triageHoldRig struct {
	*phoneRig
	source  CreatedSource
	runs    <-chan fakeTriageRun
	barbara Member
	// start is when the SOS arrived; each test counts from it.
	start time.Time
	// started keeps each run's connection reachable: the garbage collector
	// closes an unreachable one, which ends the run.
	started []fakeTriageRun
}

const onCallNumber = "+15555550142"

func newTriageHoldRig(t *testing.T) *triageHoldRig {
	t.Helper()
	r := &triageHoldRig{phoneRig: newPhoneRig(t)}
	ctx := context.Background()
	r.fake.SetPushoverClock(r.a.Now)
	// The hold tests use a 5-minute wait, set as a team sets it in the
	// settings file, so they do not depend on the wait before any setting.
	if outcomes, err := r.a.applySettingsParts(ctx, Settings{TriageAgent: &SettingsTriageAgent{SOSWaitsForTheAgentMinutes: 5}}, nil, []settingsFilePart{settingsSOSWaitsForTheAgentPart}); err != nil || outcomes[0].State != "applied" {
		t.Fatalf("applying the wait: %+v %v", outcomes, err)
	}
	if _, err := r.a.SetUpSendingService(ctx, r.s.Principal, "pushover", map[string]string{"pushover_application_token": fakesendingservices.PushoverApplicationToken}); err != nil {
		t.Fatal(err)
	}
	responder, err := r.a.CurrentMember(ctx, r.s.Principal)
	if err != nil {
		t.Fatal(err)
	}
	r.barbara = addPerson(t, r.a, "barbara@example.test", "Barbara", RoleMember)
	for _, person := range []struct {
		principal Principal
		key       string
	}{{r.s.Principal, fakesendingservices.PushoverUserKey}, {Principal{SignInEmailAddress: r.barbara.SignInEmailAddress}, fakesendingservices.PushoverOtherUserKey}} {
		if _, err = r.a.SaveContactMethod(ctx, person.principal, ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": person.key}, Subscriptions: []string{"sos", "warn", "info"}}); err != nil {
			t.Fatal(err)
		}
		r.dispatch(t)
	}
	r.addPhone(t, r.s.Principal, onCallNumber)
	week := []string{responder.ID, responder.ID, responder.ID, responder.ID, responder.ID, responder.ID, responder.ID}
	if _, err = r.a.SaveSchedule(ctx, r.s.Principal, ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: week, BackupKind: "fixed_person", BackupMemberID: r.barbara.ID}, 0); err != nil {
		t.Fatal(err)
	}
	if r.source, err = createAlertSource(ctx, r.a, r.s.Principal, "Monitoring"); err != nil {
		t.Fatal(err)
	}
	r.runs = setUpTriageAgentForTest(t, r.a, true)
	backgroundRounds(t, r.a)
	return r
}

// sosArrives admits an SOS from the alert source; the test's clock starts.
func (r *triageHoldRig) sosArrives(t *testing.T, title, key string) Alert {
	t.Helper()
	r.start = r.a.Now()
	return admitSignedAlert(t, r.a, r.source, "sos", title, key)
}

// at moves the clock to d after the SOS arrived and lets MMHotline's
// background loop do what is due then.
func (r *triageHoldRig) at(t *testing.T, d time.Duration) {
	t.Helper()
	r.advance(r.start.Add(d).Sub(r.a.Now()))
	backgroundRounds(t, r.a)
}

// runStarted lets the background loop start the session's run and returns it.
func (r *triageHoldRig) runStarted(t *testing.T) fakeTriageRun {
	t.Helper()
	backgroundRounds(t, r.a)
	select {
	case run := <-r.runs:
		r.started = append(r.started, run)
		return run
	case <-time.After(10 * time.Second):
		t.Fatal("no triage agent run started")
		return fakeTriageRun{}
	}
}

// emergencies counts the Pushover emergency messages about an incident to
// one Pushover user key.
func (r *triageHoldRig) emergencies(incidentID, userKey string) int {
	count := 0
	for _, m := range r.fake.PushoverMessages() {
		if m.Priority == 2 && m.UserKey == userKey && strings.HasSuffix(m.URL, "/incidents/"+incidentID) {
			count++
		}
	}
	return count
}

// woken says whether Responder, the person on call, got a Pushover emergency
// or an SOS call about the incident.
func (r *triageHoldRig) woken(incidentID string) bool {
	return r.emergencies(incidentID, fakesendingservices.PushoverUserKey) > 0 || len(r.callsTo(onCallNumber)) > 0
}

// history lists the messages of an incident's history, oldest first.
func (r *triageHoldRig) history(t *testing.T, incidentID string) []string {
	t.Helper()
	actions, err := r.a.IncidentAudit(context.Background(), r.s.Principal, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, action := range actions {
		messages = append(messages, action.Message)
	}
	return messages
}

// inOrder says whether every wanted text is in a message, in this order.
func inOrder(messages []string, want ...string) bool {
	for _, message := range messages {
		if len(want) > 0 && strings.Contains(message, want[0]) {
			want = want[1:]
		}
	}
	return len(want) == 0
}

// An SOS that joins a triage session whose run started wakes nobody while
// the triage agent works, whatever the agent reports, and wakes the person
// on call 5 minutes after it arrived. Its repeats and the backup count from
// then.
func TestSOSWaitsFiveMinutesForTheTriageAgent(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageHoldRig(t)
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	run := r.runStarted(t)
	r.at(t, time.Minute)
	report(t, run, json.NewDecoder(run.connection), "0000000000000001", "info", "# Freed 2 GB on db001\n")
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	waitNoTriageRun(t, r.a)
	r.at(t, 5*time.Minute-time.Second)
	if r.woken(sos.IncidentID) {
		t.Fatal("the person on call was woken before 5 minutes")
	}
	r.at(t, 5*time.Minute)
	calls := r.callsTo(onCallNumber)
	if r.emergencies(sos.IncidentID, fakesendingservices.PushoverUserKey) != 1 || len(calls) != 1 {
		t.Fatalf("at 5 minutes: %d Pushover emergencies, %d calls; want one of each", r.emergencies(sos.IncidentID, fakesendingservices.PushoverUserKey), len(calls))
	}
	held := "Held for the triage agent until " + r.start.Add(5*time.Minute).UTC().Format("15:04") + " UTC"
	if history := r.history(t, sos.IncidentID); !inOrder(history, held, "Freed 2 GB on db001", "The triage agent's time ran out; waking the person on call", "Contacting Responder (on call)") {
		t.Fatalf("history: %q", history)
	}
	// The call ends unanswered; the next one goes a minute after the first.
	if err := r.fake.ReportCallStatus(calls[0].IDFromSendingService, "no-answer"); err != nil {
		t.Fatal(err)
	}
	r.at(t, 6*time.Minute-time.Second)
	if calls := r.callsTo(onCallNumber); len(calls) != 1 {
		t.Fatalf("%d calls before 6 minutes", len(calls))
	}
	r.at(t, 6*time.Minute)
	if calls := r.callsTo(onCallNumber); len(calls) != 2 {
		t.Fatalf("%d calls at 6 minutes; the repeat must follow the first call by a minute", len(calls))
	}
	// The time to claim of the person on call starts at 5 minutes, so the backup's starts at 10.
	r.at(t, 10*time.Minute-time.Second)
	if n := r.emergencies(sos.IncidentID, fakesendingservices.PushoverOtherUserKey); n != 0 {
		t.Fatalf("the backup got %d emergencies before 10 minutes", n)
	}
	r.at(t, 10*time.Minute)
	if n := r.emergencies(sos.IncidentID, fakesendingservices.PushoverOtherUserKey); n != 1 {
		t.Fatalf("the backup got %d emergencies at 10 minutes", n)
	}
}

// The settings file's triage_agent.sos_waits_for_the_agent_minutes sets the
// wait: an SOS waits that long for the triage agent. With 0, the person on
// call is woken at once, and the triage agent still looks into the SOS.
func TestSOSWaitForTheTriageAgentIsTheSetting(t *testing.T) {
	parallelStartingProcesses(t)
	setWait := func(t *testing.T, r *triageHoldRig, minutes int) {
		t.Helper()
		outcomes, err := r.a.applySettingsParts(context.Background(), Settings{TriageAgent: &SettingsTriageAgent{SOSWaitsForTheAgentMinutes: minutes}}, nil, []settingsFilePart{settingsSOSWaitsForTheAgentPart})
		if err != nil || outcomes[0].State != "applied" {
			t.Fatalf("applying the wait: %+v %v", outcomes, err)
		}
	}
	t.Run("2 minutes", func(t *testing.T) {
		r := newTriageHoldRig(t)
		setWait(t, r, 2)
		sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		r.at(t, 2*time.Minute-time.Second)
		if r.woken(sos.IncidentID) {
			t.Fatal("the person on call was woken before 2 minutes")
		}
		r.at(t, 2*time.Minute)
		if !r.woken(sos.IncidentID) {
			t.Fatal("the person on call was not woken at 2 minutes")
		}
		if history := r.history(t, sos.IncidentID); !inOrder(history, "Held for the triage agent until "+r.start.Add(2*time.Minute).UTC().Format("15:04")+" UTC") {
			t.Fatalf("history: %q", history)
		}
	})
	t.Run("0 wakes at once", func(t *testing.T) {
		r := newTriageHoldRig(t)
		setWait(t, r, 0)
		sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		r.at(t, 0)
		if !r.woken(sos.IncidentID) {
			t.Fatal("with a wait of 0, the person on call was not woken at once")
		}
		if history := r.history(t, sos.IncidentID); inOrder(history, "Held for the triage agent") {
			t.Fatalf("an SOS with a wait of 0 was held: %q", history)
		}
	})
}

// An SOS resolved or claimed while it waits for the triage agent wakes nobody.
func TestSOSResolvedOrClaimedWhileItWaitsWakesNobody(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name   string
		at     time.Duration
		action func(t *testing.T, r *triageHoldRig, incident Incident)
	}{
		{"resolved by its alert source", 3 * time.Minute, func(t *testing.T, r *triageHoldRig, _ Incident) {
			admitSignedAlert(t, r.a, r.source, "resolved-sos", "Disk of db001 is 60% full", "db001-disk")
		}},
		{"resolved by a person", 3 * time.Minute, func(t *testing.T, r *triageHoldRig, incident Incident) {
			if _, err := r.a.ResolveIncident(context.Background(), r.s.Principal, incident.ID, incident.Revision, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"claimed by a person", 2 * time.Minute, func(t *testing.T, r *triageHoldRig, incident Incident) {
			if _, err := r.a.ClaimIncident(context.Background(), r.s.Principal, incident.ID, incident.Revision, ""); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageHoldRig(t)
			sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
			r.runStarted(t)
			r.at(t, test.at)
			incident, err := r.a.GetIncident(context.Background(), r.s.Principal, sos.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			test.action(t, r, incident)
			for _, later := range []time.Duration{test.at, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute} {
				r.at(t, later)
			}
			if r.woken(sos.IncidentID) || r.emergencies(sos.IncidentID, fakesendingservices.PushoverOtherUserKey) > 0 {
				t.Fatal("someone was woken about the incident")
			}
		})
	}
}

// A person never waits for the triage agent: an SOS a person creates by hand
// wakes the person on call at once, though it joins the session, and
// escalating a waiting SOS to another person contacts them at once.
func TestAPersonDoesNotWaitForTheTriageAgent(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("an SOS created by hand", func(t *testing.T) {
		r := newTriageHoldRig(t)
		waiting := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		manual, err := r.a.ManualSOS(context.Background(), r.s.Principal, "Customers cannot sign in", "The sign-in page times out.")
		if err != nil {
			t.Fatal(err)
		}
		r.at(t, 0)
		if r.emergencies(manual.IncidentID, fakesendingservices.PushoverUserKey) != 1 || r.emergencies(waiting.IncidentID, fakesendingservices.PushoverUserKey) != 0 {
			t.Fatal("the SOS created by hand must wake the person on call at once, and only it")
		}
		if joined := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=?`, manual.ID); joined != 1 {
			t.Fatal("the SOS created by hand did not join the session")
		}
	})
	t.Run("escalating a waiting SOS", func(t *testing.T) {
		r := newTriageHoldRig(t)
		sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		r.at(t, time.Minute)
		incident, err := r.a.GetIncident(context.Background(), r.s.Principal, sos.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.a.EscalateIncident(context.Background(), r.s.Principal, incident.ID, r.barbara.ID, incident.Revision); err != nil {
			t.Fatal(err)
		}
		r.at(t, time.Minute)
		if r.emergencies(sos.IncidentID, fakesendingservices.PushoverOtherUserKey) != 1 {
			t.Fatal("Barbara, chosen by hand, was not contacted at once")
		}
		r.at(t, 5*time.Minute)
		if r.woken(sos.IncidentID) {
			t.Fatal("the end of the wait woke the person on call after a person escalated the incident")
		}
	})
}

// An SOS that joins a triage session which already woke the person on call
// neither waits nor wakes them again: its incident says it is part of the
// session they were woken for. If nobody claims it in the time to claim, its
// escalation goes on to the backup, so it is never left unseen. While the
// session has woken nobody yet, or its wake failed to send, a joining SOS
// waits and wakes as any other.
func TestSOSJoiningASessionThatAlreadyWokeSomeone(t *testing.T) {
	parallelStartingProcesses(t)
	ctx := context.Background()
	held := func(t *testing.T, r *triageHoldRig, incidentID string) bool {
		t.Helper()
		return inOrder(r.history(t, incidentID), "Held for the triage agent")
	}
	t.Run("no second wake and no wait", func(t *testing.T) {
		r := newTriageHoldRig(t)
		first := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		r.at(t, 5*time.Minute)
		if !r.woken(first.IncidentID) {
			t.Fatal("the first SOS did not wake the person on call at the end of its wait")
		}
		incident, err := r.a.GetIncident(ctx, r.s.Principal, first.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.a.ClaimIncident(ctx, r.s.Principal, incident.ID, incident.Revision, ""); err != nil {
			t.Fatal(err)
		}
		r.at(t, 5*time.Minute+30*time.Second)
		calls := len(r.callsTo(onCallNumber))
		second := admitSignedAlert(t, r.a, r.source, "sos", "Web service on app001 stopped", "app001-web")
		r.at(t, 5*time.Minute+30*time.Second)
		if joined := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=?`, second.ID); joined != 1 {
			t.Fatal("the second SOS did not join the session")
		}
		if held(t, r, second.IncidentID) {
			t.Fatalf("the second SOS waited for the triage agent: %q", r.history(t, second.IncidentID))
		}
		if history := r.history(t, second.IncidentID); !inOrder(history, "MMHotline already woke Responder for this triage session") {
			t.Fatalf("the second SOS's history does not say the session already woke the person on call: %q", history)
		}
		request := httptest.NewRequest("GET", "https://mmhotline.example.com/incidents/"+second.IncidentID, nil)
		addAccessToken(request, r.s.Principal.SignInEmailAddress)
		response := httptest.NewRecorder()
		r.a.Handler().ServeHTTP(response, request)
		if got := liveSectionText(html.UnescapeString(response.Body.String()), "incident-state"); !strings.HasPrefix(got, "MMHotline already woke you for this incident's triage session, so this incident wakes nobody now.") {
			t.Errorf("the second SOS's decide box: %q", got)
		}
		r.at(t, 10*time.Minute+29*time.Second)
		if n := r.emergencies(second.IncidentID, fakesendingservices.PushoverUserKey); n != 0 || len(r.callsTo(onCallNumber)) != calls {
			t.Fatalf("the person on call was woken again: %d Pushover emergencies about the second SOS, %d calls after it arrived", n, len(r.callsTo(onCallNumber))-calls)
		}
		// Unclaimed after the time to claim, the second SOS goes to the backup.
		r.at(t, 10*time.Minute+30*time.Second)
		if n := r.emergencies(second.IncidentID, fakesendingservices.PushoverOtherUserKey); n != 1 {
			t.Fatalf("the backup got %d emergencies about the unclaimed second SOS; want 1", n)
		}
		if n := r.emergencies(second.IncidentID, fakesendingservices.PushoverUserKey); n != 0 {
			t.Fatalf("the person on call got %d emergencies about the second SOS", n)
		}
	})
	t.Run("the agent's report at sos still wakes at once", func(t *testing.T) {
		r := newTriageHoldRig(t)
		first := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		run := r.runStarted(t)
		r.at(t, 5*time.Minute)
		incident, err := r.a.GetIncident(ctx, r.s.Principal, first.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.a.ClaimIncident(ctx, r.s.Principal, incident.ID, incident.Revision, ""); err != nil {
			t.Fatal(err)
		}
		second := admitSignedAlert(t, r.a, r.source, "sos", "Web service on app001 stopped", "app001-web")
		r.at(t, 5*time.Minute+30*time.Second)
		if n := r.emergencies(second.IncidentID, fakesendingservices.PushoverUserKey); n != 0 {
			t.Fatalf("the second SOS woke the person on call %d times on arrival", n)
		}
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "sos", "# db001 needs a person\n\nThe disk is failing.\n")
		r.at(t, 5*time.Minute+30*time.Second)
		if n := r.emergencies(second.IncidentID, fakesendingservices.PushoverUserKey); n != 1 {
			t.Fatalf("the report at sos woke the person on call %d times about the second SOS; want once", n)
		}
	})
	t.Run("the session has woken nobody yet", func(t *testing.T) {
		r := newTriageHoldRig(t)
		first := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		r.at(t, time.Minute)
		second := admitSignedAlert(t, r.a, r.source, "sos", "Web service on app001 stopped", "app001-web")
		r.at(t, 6*time.Minute-time.Second)
		if !held(t, r, second.IncidentID) || r.emergencies(second.IncidentID, fakesendingservices.PushoverUserKey) != 0 {
			t.Fatalf("the second SOS did not wait for the triage agent: %q", r.history(t, second.IncidentID))
		}
		if !r.woken(first.IncidentID) {
			t.Fatal("the first SOS did not wake the person on call at the end of its wait")
		}
		r.at(t, 6*time.Minute)
		if n := r.emergencies(second.IncidentID, fakesendingservices.PushoverUserKey); n != 1 {
			t.Fatalf("the second SOS woke the person on call %d times at the end of its wait; want once", n)
		}
	})
	t.Run("the earlier wake failed to send", func(t *testing.T) {
		r := newTriageHoldRig(t)
		first := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		r.at(t, 5*time.Minute)
		if !r.woken(first.IncidentID) {
			t.Fatal("the first SOS did not wake the person on call at the end of its wait")
		}
		// Every message of the wake failed: it may have reached nobody.
		if _, err := r.a.Store.db.Exec(`UPDATE deliveries SET state='failed' WHERE incident_id=? AND purpose='sos_escalation'`, first.IncidentID); err != nil {
			t.Fatal(err)
		}
		r.at(t, 5*time.Minute+30*time.Second)
		second := admitSignedAlert(t, r.a, r.source, "sos", "Web service on app001 stopped", "app001-web")
		r.at(t, 5*time.Minute+30*time.Second)
		if !held(t, r, second.IncidentID) {
			t.Fatalf("the second SOS did not wait for the triage agent: %q", r.history(t, second.IncidentID))
		}
		r.at(t, 10*time.Minute+30*time.Second)
		if n := r.emergencies(second.IncidentID, fakesendingservices.PushoverUserKey); n != 1 {
			t.Fatalf("the second SOS woke the person on call %d times at the end of its wait; want once", n)
		}
	})
}

// The triage agent's report at sos wakes the person on call at once.
func TestTriageAgentSOSReportWakesAtOnce(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageHoldRig(t)
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	run := r.runStarted(t)
	r.at(t, time.Minute)
	report(t, run, json.NewDecoder(run.connection), "0000000000000001", "sos", "# db001 needs a person\n\nThe disk is failing.\n")
	r.at(t, time.Minute)
	if r.emergencies(sos.IncidentID, fakesendingservices.PushoverUserKey) != 1 || len(r.callsTo(onCallNumber)) != 1 {
		t.Fatal("the report at sos did not wake the person on call at once")
	}
	if history := r.history(t, sos.IncidentID); !inOrder(history, "db001 needs a person", "The triage agent sent an urgent report (sos). Waking the person on call", "Contacting Responder (on call)") {
		t.Fatalf("history: %q", history)
	}
}

// When the triage agent cannot run, the SOS wakes the person on call at
// once, and its history says why.
func TestSOSWakesAtOnceWhenTheTriageAgentCannotRun(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("the run fails", func(t *testing.T) {
		r := newTriageHoldRig(t)
		sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		run := r.runStarted(t)
		r.at(t, 30*time.Second)
		sendTriageEvent(t, run, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
		waitNoTriageRun(t, r.a)
		r.at(t, 30*time.Second)
		if !r.woken(sos.IncidentID) {
			t.Fatal("the failed run did not wake the person on call")
		}
		if history := r.history(t, sos.IncidentID); !inOrder(history, "Held for the triage agent", "The triage agent's run failed: The coding agent's service refused the request: 529 Overloaded.", "The triage agent could not run; waking the person on call", "Contacting Responder (on call)") {
			t.Fatalf("history: %q", history)
		}
	})
	t.Run("the run cannot start", func(t *testing.T) {
		r := newTriageHoldRig(t)
		// A commit in MMHotline's copy that origin lacks stops every run before it starts.
		testGit(t, filepath.Join(r.a.Store.Dir, "systems_repository"), "commit", "--quiet", "--allow-empty", "-m", "Not pushed")
		sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		backgroundRounds(t, r.a)
		waitTriageIdleOrRunning(t, r.a)
		waitNoTriageRun(t, r.a)
		r.at(t, 0)
		if !r.woken(sos.IncidentID) {
			t.Fatal("the run that could not start did not wake the person on call")
		}
		if history := r.history(t, sos.IncidentID); !inOrder(history, "Could not prepare the systems repository", "The triage agent could not run; waking the person on call") {
			t.Fatalf("history: %q", history)
		}
	})
	t.Run("MMHotline's copy is missing", func(t *testing.T) {
		r := newTriageHoldRig(t)
		copy := filepath.Join(r.a.Store.Dir, "systems_repository")
		if err := os.Rename(copy, copy+".elsewhere"); err != nil {
			t.Fatal(err)
		}
		sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.at(t, 0)
		if !r.woken(sos.IncidentID) {
			t.Fatal("the SOS waited for an agent that cannot run")
		}
		if history := r.history(t, sos.IncidentID); !inOrder(history, "MMHotline's copy of the systems repository is missing; waking the person on call") {
			t.Fatalf("history: %q", history)
		}
	})
	t.Run("the triage agent is not set up", func(t *testing.T) {
		r := newTriageHoldRig(t)
		if _, err := r.a.Store.db.Exec(`DELETE FROM triage_agent_setup`); err != nil {
			t.Fatal(err)
		}
		sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.at(t, 0)
		if !r.woken(sos.IncidentID) {
			t.Fatal("the SOS did not wake the person on call at once")
		}
		if history := r.history(t, sos.IncidentID); inOrder(history, "triage agent") {
			t.Fatalf("history mentions the triage agent: %q", history)
		}
	})
}

// The deadline lives in the database: after MMHotline restarts, a waiting
// SOS wakes the person on call at its deadline, or at once when it passed.
func TestSOSWaitForTheTriageAgentSurvivesARestart(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name              string
		restartAt, wakeAt time.Duration
	}{
		{"before the deadline", 2 * time.Minute, 5 * time.Minute},
		{"after the deadline", 6 * time.Minute, 6 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageHoldRig(t)
			sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
			run := r.runStarted(t)
			sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
			waitSessionIDSaved(t, r.a, "session-one")
			r.advance(r.start.Add(test.restartAt).Sub(r.a.Now()))
			r.a.CloseTriageAgent()
			r.a = restartApp(t, r.a)
			// Startup builds the sending services' adapters again.
			if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
				t.Fatal(err)
			}
			r.runs = triageRunFixture(t, r.a)
			if resumed := r.runStarted(t); resumed.request.Action != "resume" {
				t.Fatalf("after the restart the session's run is a %q", resumed.request.Action)
			}
			if test.wakeAt > test.restartAt {
				r.at(t, test.wakeAt-time.Second)
				if r.woken(sos.IncidentID) {
					t.Fatal("the person on call was woken before the deadline")
				}
			}
			r.at(t, test.wakeAt)
			if !r.woken(sos.IncidentID) {
				t.Fatal("the person on call was not woken at the deadline")
			}
			incident, err := r.a.GetIncident(context.Background(), r.s.Principal, sos.IncidentID)
			if err != nil || !incident.PrimaryAt.Equal(r.start.Add(test.wakeAt)) {
				t.Fatalf("the first waking step started at %s, want %s: %v", incident.PrimaryAt, r.start.Add(test.wakeAt), err)
			}
		})
	}
}

// With triage_agent.send_a_message_when_the_agent_starts_looking on, when
// the triage agent starts on a session, the person on call, and only they,
// get one quiet message about it; a resume sends none, and the message
// starts no session and opens no incident. By default nobody gets it.
func TestTriageSessionStartMessageIsQuietAndGoesToThePersonOnCall(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("off by default", func(t *testing.T) {
		r := newTriageHoldRig(t)
		r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
		r.runStarted(t)
		r.at(t, time.Minute)
		if n := countRows(t, r.a, `SELECT count(*) FROM alerts WHERE title LIKE 'The AI triage agent is looking into: %'`); n != 0 {
			t.Fatalf("%d start messages without the setting", n)
		}
		for _, m := range r.fake.PushoverMessages() {
			if strings.Contains(m.Title, "The AI triage agent is looking into") {
				t.Fatalf("a start message reached a phone: %q", m.Title)
			}
		}
	})
	r := newTriageHoldRig(t)
	outcomes, err := r.a.applySettingsParts(context.Background(), Settings{TriageAgent: &SettingsTriageAgent{SendAMessageWhenTheAgentStartsLooking: true}}, nil, []settingsFilePart{settingsSendAMessageWhenTheAgentStartsLookingPart})
	if err != nil || outcomes[0].State != "applied" {
		t.Fatalf("applying the setting: %+v %v", outcomes, err)
	}
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	run := r.runStarted(t)
	title := "The AI triage agent is looking into: Disk of db001 is 99% full (started " + r.start.UTC().Format("15:04") + " UTC)"
	quiet := func(userKey string) []fakesendingservices.PushoverMessage {
		var found []fakesendingservices.PushoverMessage
		for _, m := range r.fake.PushoverMessages() {
			if m.UserKey == userKey && m.Title == "[Info] "+title {
				found = append(found, m)
			}
		}
		return found
	}
	if sent := quiet(fakesendingservices.PushoverUserKey); len(sent) != 1 || sent[0].Priority != -1 || sent[0].Sound != "" || !strings.HasSuffix(sent[0].URL, "/incidents/"+sos.IncidentID) {
		t.Fatalf("the person on call got %+v; want one low-priority message without sound that links the incident", sent)
	}
	if sent := quiet(fakesendingservices.PushoverOtherUserKey); len(sent) != 0 {
		t.Fatalf("the backup, who is not on call, got %+v", sent)
	}
	// A warn alert during the run goes with a resume of the same session.
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	admitSignedAlert(t, r.a, r.source, "warn", "Disk of db002 is 91% full", "")
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	waitNoTriageRun(t, r.a)
	if resumed := r.runStarted(t); resumed.request.Action != "resume" {
		t.Fatalf("the second run is a %q", resumed.request.Action)
	}
	if sent := quiet(fakesendingservices.PushoverUserKey); len(sent) != 1 {
		t.Fatalf("%d start messages; a resume sends none", len(sent))
	}
	var severity, sourceID, alertID string
	if err := r.a.Store.db.QueryRow(`SELECT severity,source_id,id FROM alerts WHERE title=?`, title).Scan(&severity, &sourceID, &alertID); err != nil || severity != "info" || sourceID != "system" {
		t.Fatalf("the start message is %s from %s: %v", severity, sourceID, err)
	}
	if joined := countRows(t, r.a, `SELECT count(*) FROM triage_session_alerts WHERE alert_id=?`, alertID); joined != 0 {
		t.Fatal("the start message joined the session")
	}
	if sessions, incidents := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`), countRows(t, r.a, `SELECT count(*) FROM incidents`); sessions != 1 || incidents != 1 {
		t.Fatalf("%d sessions and %d incidents; the start message must start or open none", sessions, incidents)
	}
}

// The incident page says the triage agent works on the incident and when
// MMHotline wakes the person on call, and its history shows the wait.
func TestIncidentPageShowsTheWaitForTheTriageAgent(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageHoldRig(t)
	sos := r.sosArrives(t, "Disk of db001 is 99% full", "db001-disk")
	r.runStarted(t)
	page := func() string {
		request := httptest.NewRequest("GET", "https://mmhotline.example.com/incidents/"+sos.IncidentID, nil)
		addAccessToken(request, r.s.Principal.SignInEmailAddress)
		response := httptest.NewRecorder()
		r.a.Handler().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("incident page: %d", response.Code)
		}
		return html.UnescapeString(response.Body.String())
	}
	deadline := r.start.Add(5 * time.Minute).UTC().Format("15:04")
	waiting := page()
	if got := liveSectionText(waiting, "incident-state"); !strings.HasPrefix(got, "The triage agent looks first, so MMHotline has woken nobody yet. Claim the incident by "+deadline+", or MMHotline wakes you, the person on call.") {
		t.Errorf("the decide box while the triage agent looks: %q", got)
	}
	if got := liveSectionText(waiting, "incident-steps"); !strings.Contains(got, "The triage agent looks first until "+deadline) {
		t.Errorf("where it is: %q", got)
	}
	if got := liveSectionText(waiting, "incident-audit"); !strings.Contains(got, "MMHotline held the incident for the triage agent to investigate first. MMHotline wakes nobody until 12:06 UTC.") {
		t.Errorf("What happened while the triage agent looks: %q", got)
	}
	r.at(t, 5*time.Minute)
	woken := page()
	if got := liveSectionText(woken, "incident-audit"); !strings.Contains(got, deadline+" The hold for the triage agent ended. The triage agent's time ran out.") || !strings.Contains(got, deadline+" MMHotline woke you (on call).") {
		t.Errorf("What happened after the deadline: %q", got)
	}
	if got := liveSectionText(woken, "incident-state"); !strings.HasPrefix(got, "You are on call. MMHotline woke you at "+deadline+". Claim the incident by ") || !strings.Contains(got, ", or MMHotline also wakes Barbara.") {
		t.Errorf("the decide box after the deadline: %q", got)
	}
	checkNoInternalTexts(t, woken)
}

// restartsDuringARun are the two ways a restart of MMHotline cuts a run
// short, each with the reason the run records.
var restartsDuringARun = []struct {
	name, reason string
	stop         func(t *testing.T, a *App) *App
}{
	{"MMHotline stops", triageRunCutByStop, func(t *testing.T, a *App) *App {
		a.CloseTriageAgent()
		return restartApp(t, a)
	}},
	{"MMHotline's process ends", triageRunCutByRestart, func(t *testing.T, a *App) *App {
		// The store goes first, so nothing of the run's end is saved.
		if err := a.Store.Close(); err != nil {
			t.Fatal(err)
		}
		a.CloseTriageAgent()
		store, err := Open(a.Store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		restarted := New(store)
		restarted.Now, restarted.WebURL = a.Now, a.WebURL
		t.Cleanup(func() { restarted.Store.Close() })
		return restarted
	}},
}

// A restart during a run resumes the session with the alerts and the notes
// from the team that run carried, whether MMHotline stopped cleanly or its
// process ended at once.
func TestTriageSessionResumesAfterARestart(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range restartsDuringARun {
		t.Run(test.name, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			sos := r.alert(t, "sos", "Database on db001 is down", "db001")
			const note = "“I restarted PostgreSQL on db001.”"
			r.claimWithNote(t, sos.IncidentID, strings.Trim(note, "“”"))
			r.drive(t)
			run := r.nextRun(t)
			if !strings.Contains(run.request.Prompt, note) {
				t.Fatalf("the first run lacks the note:\n%s", run.request.Prompt)
			}
			sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
			waitSessionIDSaved(t, r.a, "session-one")
			r.a = test.stop(t, r.a)
			r.runs = triageRunFixture(t, r.a)
			r.drive(t)
			resumed := r.nextRun(t)
			if resumed.request.Action != "resume" || resumed.request.SessionID != "session-one" {
				t.Fatalf("after the restart: action %q, session %q", resumed.request.Action, resumed.request.SessionID)
			}
			for _, want := range []string{"Database on db001 is down", note, "Your last run ended before the work finished: " + test.reason} {
				if !strings.Contains(resumed.request.Prompt, want) {
					t.Fatalf("the prompt lacks %q:\n%s", want, resumed.request.Prompt)
				}
			}
			if count := countRows(t, r.a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='triage_agent_could_not_run' AND message LIKE ?`, sos.IncidentID, "%"+test.reason); count != 1 {
				t.Fatalf("%d records of the cut run", count)
			}
			if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`); sessions != 1 {
				t.Fatalf("%d open sessions", sessions)
			}
		})
	}
}

// MMHotline's process ends while a playbook the agent asked for runs. The
// resumed run hears that the playbook run started and that MMHotline
// recorded no outcome, so the agent checks the servers instead of guessing.
func TestResumeAfterARestartTellsOfAPlaybookRunWithNoOutcome(t *testing.T) {
	parallelStartingProcesses(t)
	test := restartsDuringARun[1]
	r := newTriageSessionRig(t, true)
	r.alert(t, "sos", "Database on db001 is down", "db001")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
	waitSessionIDSaved(t, r.a, "session-one")
	// As MMHotline records a request whose MMX run has started.
	if _, err := r.a.Store.db.Exec(`INSERT INTO triage_agent_requests(run_id,request_id,kind,playbook,inventory,requested_at,playbook_run_started_at) VALUES(?,'0000000000000001','playbook_run',?,'production',?,?)`, lastRunID(t, r.a), rebootPlaybook, r.a.Now().Unix(), r.a.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	r.a = test.stop(t, r.a)
	r.runs = triageRunFixture(t, r.a)
	r.drive(t)
	resumed := r.nextRun(t)
	for _, want := range []string{"Your last run ended before the work finished: " + test.reason, "MMHotline's record of that request, to run " + playbookInInventoryText(rebootPlaybook, "production"), "was still running when your run stopped. MMHotline recorded no outcome"} {
		if !strings.Contains(resumed.request.Prompt, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, resumed.request.Prompt)
		}
	}
}

// Codex and Claude Code report what their session has used so far, so a
// resumed session's total includes its earlier runs. Each run records its own
// use, its total less the total of the session's latest finished run, and
// keeps its total for the next run. A failed run reports no total, so the
// next run's own use includes it.
func TestTriageSessionRunsRecordTheirOwnUse(t *testing.T) {
	parallelStartingProcesses(t)
	type usage = triageagent.Usage
	cost := func(value float64) *float64 { return &value }
	for _, test := range []struct {
		codingAgent string
		// totals has each run's reported total in turn; nil is a failed run.
		totals []*usage
		own    []usage
	}{
		// Codex 0.164.0 for two runs of one thread on 2026-10-27, then a failed
		// run and a fourth run.
		{"codex", []*usage{
			{InputTokens: 24814, CachedInputTokens: 7168, OutputTokens: 5},
			{InputTokens: 49646, CachedInputTokens: 31872, OutputTokens: 10},
			nil,
			{InputTokens: 100000, CachedInputTokens: 80000, OutputTokens: 40},
		}, []usage{
			{InputTokens: 24814, CachedInputTokens: 7168, OutputTokens: 5},
			{InputTokens: 24832, CachedInputTokens: 24704, OutputTokens: 5},
			{},
			{InputTokens: 50354, CachedInputTokens: 48128, OutputTokens: 30},
		}},
		// Claude Code 2.1.311 for two runs of one session on 2026-10-27: its
		// modelUsage and total_cost_usd.
		{"claude_code", []*usage{
			{CostInUSDollars: cost(0.00335973), InputTokens: 27606, CachedInputTokens: 11383, OutputTokens: 3},
			{CostInUSDollars: cost(0.00525969), InputTokens: 55250, CachedInputTokens: 30489, OutputTokens: 6},
		}, []usage{
			{CostInUSDollars: cost(0.00335973), InputTokens: 27606, CachedInputTokens: 11383, OutputTokens: 3},
			{CostInUSDollars: cost(0.00189996), InputTokens: 27644, CachedInputTokens: 19106, OutputTokens: 3},
		}},
	} {
		t.Run(test.codingAgent, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			if _, err := r.a.Store.db.Exec(`UPDATE triage_agent_setup SET coding_agent=?,credential_type='api_key'`, test.codingAgent); err != nil {
				t.Fatal(err)
			}
			for index, total := range test.totals {
				// One problem, so each alert joins the one session.
				r.alert(t, "warn", fmt.Sprintf("Disk of app002 is %d%% full", 90+index), "app002-disk")
				r.drive(t)
				run := r.nextRun(t)
				if index > 0 && (run.request.Action != "resume" || run.request.SessionID != "session-one") {
					t.Fatalf("run %d: action %q, session %q", index+1, run.request.Action, run.request.SessionID)
				}
				// Both coding agents report the session at the start of a resume, too.
				sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
				if total == nil {
					r.end(t, run, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
					continue
				}
				r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported.", CodingAgentSessionTotalUsage: total})
			}
			rows, err := r.a.Store.db.Query(`SELECT cost_in_us_dollars,input_tokens,cached_input_tokens,output_tokens,coding_agent_session_total_cost_in_us_dollars,coding_agent_session_total_input_tokens,coding_agent_session_total_cached_input_tokens,coding_agent_session_total_output_tokens FROM triage_agent_runs WHERE purpose='triage_session' ORDER BY started_at,rowid`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			// Costs compare to the dollar's millionth part, as pages show them.
			text := func(u usage) string {
				shown := "no cost"
				if u.CostInUSDollars != nil {
					shown = fmt.Sprintf("%.8f", *u.CostInUSDollars)
				}
				return fmt.Sprintf("%s, %d input, %d cached, %d output", shown, u.InputTokens, u.CachedInputTokens, u.OutputTokens)
			}
			for index := 0; rows.Next(); index++ {
				var own, total usage
				var ownCost, totalCost sql.NullFloat64
				if err := rows.Scan(&ownCost, &own.InputTokens, &own.CachedInputTokens, &own.OutputTokens, &totalCost, &total.InputTokens, &total.CachedInputTokens, &total.OutputTokens); err != nil {
					t.Fatal(err)
				}
				if ownCost.Valid {
					own.CostInUSDollars = &ownCost.Float64
				}
				if totalCost.Valid {
					total.CostInUSDollars = &totalCost.Float64
				}
				wantTotal := usage{}
				if test.totals[index] != nil {
					wantTotal = *test.totals[index]
				}
				if text(own) != text(test.own[index]) || text(total) != text(wantTotal) {
					t.Errorf("run %d recorded its own use %s and the session's total %s; want %s and %s", index+1, text(own), text(total), text(test.own[index]), text(wantTotal))
				}
			}
		})
	}
}

// commitInventories adds inventories/NAME.yml for each name to MMHotline's
// copy and pushes it to origin, as a person's commit.
func commitInventories(t *testing.T, a *App, names ...string) {
	t.Helper()
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	if err := os.MkdirAll(filepath.Join(directory, "inventories"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(directory, "inventories", name+".yml"), []byte("nodes: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, directory, "add", "--all")
	testGit(t, directory, "commit", "--quiet", "-m", "Add the inventories")
	testGit(t, directory, "push", "--quiet", "origin", "main")
}

// The tasks of a software review and of a triage session name the
// inventories of MMHotline's copy, because the agent takes an example
// inventory for an instruction: the one inventory goes into the command,
// and several are listed for the agent to choose from.
func TestTriageAgentTasksNameTheInventories(t *testing.T) {
	parallelStartingProcesses(t)
	for _, test := range []struct {
		name        string
		inventories []string
		want        []string
	}{
		{"one inventory", []string{"servers"}, []string{
			"ask MMHotline to run the playbook in the inventory “servers”, the one inventory of the systems repository:",
			"--inventory servers --diagnosis-file diagnosis.md\n",
			"diagnosis.md holds your diagnosis in Markdown. Start with one to three plain sentences on what the run will change, on which servers, and why it is needed now: a person who must approve the run reads those first. Then explain what you found, the cause, the evidence, and why this playbook fixes it.",
			"- 4: MMHotline refused the request, or the run is ending. The answer says why. When the review agent refused your playbook, revise it to answer the reason, commit the new version, and ask again.",
		}},
		{"two inventories", []string{"staging", "production"}, []string{
			"Use an inventory from the systems repository: production, staging.",
			"--inventory INVENTORY --diagnosis-file diagnosis.md\n",
		}},
		{"no inventory", nil, []string{
			"The systems repository has no inventory yet, no file inventories/NAME.yml. No playbook can run until a person adds an inventory",
			"--inventory INVENTORY --diagnosis-file diagnosis.md\n",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newSoftwareReviewRig(t)
			if test.inventories != nil {
				commitInventories(t, r.a, test.inventories...)
			}
			r.rounds(t)
			review := r.nextReview(t)
			r.end(t, review, triageagent.Event{Event: "finished", Answer: "Reported."})
			r.alert(t, "warn", "Disk of app002 is 97% full", "")
			r.drive(t)
			session := r.nextRun(t)
			for task, prompt := range map[string]string{"software review": review.request.Prompt, "triage session": session.request.Prompt} {
				for _, want := range test.want {
					if !strings.Contains(prompt, want) {
						t.Errorf("the %s's task lacks %q:\n%s", task, want, prompt)
					}
				}
				if strings.Contains(prompt, "production") != slices.Contains(test.inventories, "production") {
					t.Errorf("the %s's task names production for the inventories %v:\n%s", task, test.inventories, prompt)
				}
				// The agent learns the task forms MMHotline recognizes before it writes a playbook.
				if !strings.Contains(prompt, "so that MMHotline recognizes its kind.\n\n"+actionkinds.AcceptedTasksText()) {
					t.Errorf("the %s's task lacks the task forms of each kind of action:\n%s", task, prompt)
				}
			}
		})
	}
}

// A new session reads the last session's note and leaves it alone: it
// writes its own note in a new file. A resumed run of the same session
// updates that note, and asks for no new one. The first run says how to reach
// a server over SSH.
func TestTriageSessionPromptNotesAndSSH(t *testing.T) {
	t.Parallel()
	contains := func(name, prompt string, wanted, unwanted []string) {
		t.Helper()
		for _, want := range wanted {
			if !strings.Contains(prompt, want) {
				t.Errorf("the %s lacks %q:\n%s", name, want, prompt)
			}
		}
		for _, text := range unwanted {
			if strings.Contains(prompt, text) {
				t.Errorf("the %s holds %q:\n%s", name, text, prompt)
			}
		}
	}
	newNote := []string{
		"Write this triage session's note in a new file, triage_agent_notes/YYYY-MM-DD_what_happened.md, with today's date and a few words for the session's first cause,",
		"Keep that one note for this session: before each run ends, write what happened so far in it and commit it.",
		"Reach a server with ssh and its name in the inventory only, such as ssh db07. MMHotline's SSH settings carry every connection through its proxy, so an address is refused and -F is ignored.",
	}
	contains("start of a session after another", promptText(triageSessionStartPrompt("util01", newestNote, []string{"servers"}, nil, nil, nil)),
		append([]string{"The last triage session left its note in " + newestNote + ": read the note for what happened before. Do not change the note from the last triage session."}, newNote...),
		[]string{"Start from the note", "Keep one note for the session"})
	contains("start of the first session", promptText(triageSessionStartPrompt("util01", "", []string{"servers"}, nil, nil, nil)),
		append([]string{"No earlier triage session left a note in triage_agent_notes/."}, newNote...),
		[]string{"Do not change the note from the last triage session."})
	contains("resume", promptText(triageSessionResumePrompt("", "", nil, nil, nil)),
		[]string{"before the run ends, update your note in triage_agent_notes/ and commit it."},
		[]string{"new file", "last triage session"})
}

// A prompt stays within 64 KiB as JSON on the run socket, however many and
// however long the alerts are, and says how many alerts it left out.
func TestTriageSessionPromptStaysWithinItsLimit(t *testing.T) {
	t.Parallel()
	var alerts []triageSessionPromptAlert
	for n := range 200 {
		// '<' takes six bytes in JSON, so only the encoded size keeps the limit.
		alerts = append(alerts, triageSessionPromptAlert{SourceName: "Monitoring", Severity: "warn", Title: fmt.Sprintf("Alert number %d", n+1), Message: strings.Repeat("<", 16<<10) + "END", AlertURL: "https://mmhotline.example.com/alerts/x", ReceivedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)})
	}
	for name, prompt := range map[string]string{
		"start":  promptText(triageSessionStartPrompt("util01", newestNote, []string{"production", "staging"}, nil, nil, alerts)),
		"resume": promptText(triageSessionResumePrompt("", "", nil, nil, alerts)),
	} {
		encoded, err := json.Marshal(prompt)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded)-2 > 64<<10 {
			t.Fatalf("%s prompt is %d bytes in JSON", name, len(encoded)-2)
		}
		shown := len(regexp.MustCompile(`(?m)^Alert [0-9]+ of 200: `).FindAllString(prompt, -1))
		if shown == 0 || !strings.Contains(prompt, fmt.Sprintf("Alert %d of 200: ", shown)) || strings.Contains(prompt, "END") {
			t.Fatalf("%s prompt shows %d alerts, in order and cut:\n%.500s", name, shown, prompt)
		}
		if want := fmt.Sprintf("\n\nAnd %d more alerts, which MMHotline left out to keep this prompt short.", 200-shown); !strings.HasSuffix(prompt, want) {
			t.Fatalf("%s prompt ends %q, want %q", name, prompt[len(prompt)-100:], want)
		}
	}
	if prompt := promptText(triageSessionResumePrompt("", "", nil, nil, alerts[:3])); !strings.Contains(prompt, "Alert 3 of 3: ") || strings.Contains(prompt, "left out") {
		t.Fatalf("three alerts fit:\n%.300s", prompt)
	}
}

// A playbook run's result reports on the problem whose run asked for it, even
// when a run about another problem came between the request and the result.
func TestTriageAgentReportAfterAnotherProblemsRun(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
	outage := startSessionWithAnOutage(t, r)
	app002 := r.alert(t, "sos", "app002 stopped answering", "app002")
	r.claim(t, app002.IncidentID)
	r.drive(t)
	second := r.nextRun(t)
	requestPlaybookRun(t, second, json.NewDecoder(second.connection), "0000000000000001")
	r.end(t, second, triageagent.Event{Event: "finished", Answer: "Asked to reboot app002."})
	record := onlyApprovalRecord(t, r.a)
	// While the approval waits, another problem joins and gets its own run.
	db002 := r.alert(t, "sos", "db002 disk full", "db002")
	r.drive(t)
	between := r.nextRun(t)
	r.end(t, between, triageagent.Event{Event: "finished", Answer: "Looked at db002."})
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	r.drive(t)
	result := r.nextRun(t)
	report(t, result, json.NewDecoder(result.connection), "0000000000000001", "info", "# Rebooted app002\n")
	if alert := reportAlert(t, r.a, "The AI triage agent reports: Rebooted app002"); alert.IncidentID != app002.IncidentID {
		t.Fatalf("the result's report went with incident %q, not app002's %s (db002's is %s)", alert.IncidentID, app002.IncidentID, db002.IncidentID)
	}
	if reports := reportsInHistory(t, r.a, db002.IncidentID) + reportsInHistory(t, r.a, outage.IncidentID); reports != 0 {
		t.Fatalf("%d reports reached db002's or db001's history", reports)
	}
}
