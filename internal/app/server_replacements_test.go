package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
	"mmhotline/internal/triageagent"
)

// replacementRunMMX stands in for MMX: every run succeeds, except a run of
// every_server when everyServerFails is set.
func replacementRunMMX(everyServerFails bool) string {
	script := "#!/bin/sh\n"
	if everyServerFails {
		script += "if [ \"$5\" = every_server ]; then echo 'every_server: db02 did not answer'; exit 2; fi\n"
	}
	return script + "echo 'PLAY RECAP db02 ok=1 changed=1'\n"
}

// replacementRig is the triage hold rig, whose SOS waits 5 minutes for the
// agent, with the production inventory listing replace_failed_servers,
// marked starts_a_server_replacement, every_server, and
// destroy_replaced_servers, each with Act, then report, as the settings
// file would save them. The extension is the 10 minutes before any setting.
func replacementRig(t *testing.T, everyServerFails bool) *triageHoldRig {
	t.Helper()
	r := newTriageHoldRig(t)
	installFakeMMX(t, r.a, map[string]string{"replace_failed_servers": everyServerExplained, "every_server": everyServerExplained, "destroy_replaced_servers": everyServerExplained})
	installFakeMMXRun(t, r.a, replacementRunMMX(everyServerFails))
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Store.db.Exec(`INSERT INTO agent_playbook_rules_for_listed_playbooks(inventory,playbook_name,rule,warning_wait_minutes,approvals_needed,starts_a_server_replacement,changed_at,changed_by) VALUES
('production','replace_failed_servers','act_then_report',0,0,1,1,'the settings file'),
('production','every_server','act_then_report',0,0,0,1,'the settings file'),
('production','destroy_replaced_servers','act_then_report',0,0,0,1,'the settings file')`); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{replaceFailedServers: "fleet_stages: []\n", listedEveryServer: "fleet_stages: []\n", destroyReplacedServers: "fleet_stages: []\n"})
	return r
}

// replacementSession lets an SOS about db02 arrive and its session's run
// start, and returns the SOS, the run, its answers, and a function that asks
// for a playbook on production in that run.
func replacementSession(t *testing.T, r *triageHoldRig) (Alert, fakeTriageRun, *json.Decoder, func(playbook string) triageagent.Answer) {
	t.Helper()
	sos := r.sosArrives(t, "The server db02 stopped answering", "db02")
	run := r.runStarted(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-db02"})
	answers := json.NewDecoder(run.connection)
	next := 100
	return sos, run, answers, func(playbook string) triageagent.Answer {
		t.Helper()
		next++
		requestID := fmt.Sprintf("%016x", next)
		sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: requestID, Playbook: playbook, Inventory: "production", Diagnosis: testDiagnosis})
		run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
		var control triageagent.Control
		if err := answers.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != requestID {
			t.Fatalf("answer: %+v %v", control, err)
		}
		return *control.Answer
	}
}

// startWarnings lists the warn alerts that announce a server replacement.
func startWarnings(t *testing.T, a *App) []Alert {
	t.Helper()
	rows, err := a.Store.db.Query(`SELECT id,title,message,incident_id FROM alerts WHERE severity='warn' AND title LIKE 'Your site is down: %' ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []Alert
	for rows.Next() {
		var alert Alert
		if err := rows.Scan(&alert.ID, &alert.Title, &alert.Message, &alert.IncidentID); err != nil {
			t.Fatal(err)
		}
		found = append(found, alert)
	}
	return found
}

// oneStartWarning checks that exactly one start warning went out, about the
// SOS, with the limit at startedAt plus minutes.
func (r *triageHoldRig) oneStartWarning(t *testing.T, sos Alert, startedAt time.Time, minutes int) {
	t.Helper()
	warnings := startWarnings(t, r.a)
	want := fmt.Sprintf("The triage agent is replacing the server. You will be woken only if the replacement fails or is not done by %s UTC. Follow the steps on the incident's page.", startedAt.Add(time.Duration(minutes)*time.Minute).UTC().Format("15:04"))
	if len(warnings) != 1 || warnings[0].Title != "Your site is down: The server db02 stopped answering" || warnings[0].Message != want || warnings[0].IncidentID != sos.IncidentID {
		t.Fatalf("start warnings %+v; want one about the SOS: %q", warnings, want)
	}
}

// emergencyMessage is the first Pushover emergency to the person on call
// about the incident.
func (r *triageHoldRig) emergencyMessage(t *testing.T, incidentID string) string {
	t.Helper()
	for _, m := range r.fake.PushoverMessages() {
		if m.Priority == 2 && m.UserKey == fakesendingservices.PushoverUserKey && strings.HasSuffix(m.URL, "/incidents/"+incidentID) {
			return m.Message
		}
	}
	t.Fatal("no Pushover emergency to the person on call")
	return ""
}

// pushoverAbout lists what Pushover got for one Pushover user key about the
// incident, oldest first, as "priority title | message".
func (r *triageHoldRig) pushoverAbout(incidentID, userKey string) []string {
	var found []string
	for _, m := range r.fake.PushoverMessages() {
		// The quiet note that the triage agent started looking into the SOS is not what these tests judge.
		if m.UserKey == userKey && strings.HasSuffix(m.URL, "/incidents/"+incidentID) && !strings.HasPrefix(m.Title, "[Info]") {
			found = append(found, fmt.Sprintf("%d %s | %s", m.Priority, m.Title, strings.SplitN(m.Message, "\n", 2)[0]))
		}
	}
	return found
}

// addEmail gives Responder an email contact method that gets SOS, as an
// email does at once.
func (r *triageHoldRig) addEmail(t *testing.T) {
	t.Helper()
	member, err := r.a.CurrentMember(context.Background(), r.s.Principal)
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	if _, err := r.a.Store.db.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) VALUES(?,?,'email','{}','responder@example.test',1,1,0)`, id, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Store.db.Exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,'sos')`, id); err != nil {
		t.Fatal(err)
	}
}

// emailStates lists the severity and whether it was suppressed of the email
// deliveries about the incident, oldest first.
func (r *triageHoldRig) emailStates(t *testing.T, incidentID string) []string {
	t.Helper()
	rows, err := r.a.Store.db.Query(`SELECT severity||' '||CASE WHEN state='suppressed' THEN 'suppressed' ELSE 'not suppressed' END FROM deliveries WHERE incident_id=? AND contact_method_type='email' ORDER BY created_at,rowid`, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var states []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	return states
}

// A replacement whose alert resolves while it runs: the SOS keeps waiting
// past its 5 minutes, MMHotline sends one warn alert at the start, wakes
// nobody, and the incident closes. The resolved message reaches the person
// on call at Pushover's normal priority, as the start did.
func TestAServerReplacementKeepsTheSOSWaitingUntilItsAlertResolves(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	if answer := ask(replaceFailedServers); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("replace_failed_servers: %+v", answer)
	}
	r.at(t, time.Minute)
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 10)
	// The warn alert reaches the person at normal priority, with the link.
	var heard bool
	for _, m := range r.fake.PushoverMessages() {
		heard = heard || m.UserKey == fakesendingservices.PushoverUserKey && m.Title == "[Warning] Your site is down: The server db02 stopped answering" && m.Priority == 0 && strings.HasSuffix(m.URL, "/incidents/"+sos.IncidentID)
	}
	if !heard {
		t.Fatalf("the person on call did not get the start warning: %+v", r.fake.PushoverMessages())
	}
	// The page says when MMHotline wakes the person, and why it waits.
	var sentence string
	if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		i, err := readIncident(context.Background(), tx, sos.IncidentID)
		if err != nil {
			return err
		}
		step, err := nextIncidentStep(context.Background(), tx, i)
		sentence = countdownSentence(step, "", time.UTC, r.a.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := "MMHotline wakes Responder at " + r.start.Add(11*time.Minute).UTC().Format("15:04") + " unless the replacement ends first."; sentence != want {
		t.Fatalf("countdown %q, want %q", sentence, want)
	}
	if answer := ask(listedEveryServer); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("every_server: %+v", answer)
	}
	r.at(t, 6*time.Minute)
	if r.woken(sos.IncidentID) {
		t.Fatal("the person on call was woken during the replacement")
	}
	if answer := ask(destroyReplacedServers); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("destroy_replaced_servers: %+v", answer)
	}
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 12*time.Minute)
	incident, err := r.a.GetIncident(context.Background(), r.s.Principal, sos.IncidentID)
	if err != nil || incident.Status != "resolved" {
		t.Fatalf("the incident is %q: %v", incident.Status, err)
	}
	if r.woken(sos.IncidentID) {
		t.Fatal("the person on call was woken")
	}
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 10)
	// Each person who heard the start warn gets its partner, at the same
	// priority, and never the ordinary "is ok again" text under the SOS's
	// title, and no emergency.
	for _, key := range []string{fakesendingservices.PushoverUserKey, fakesendingservices.PushoverOtherUserKey} {
		want := []string{
			"0 [Warning] Your site is down: The server db02 stopped answering | The triage agent is replacing the server. You will be woken only if the replacement fails or is not done by " + r.start.Add(11*time.Minute).UTC().Format("15:04") + " UTC. Follow the steps on the incident's page.",
			"0 [Resolved SOS] Your site works again: The server db02 stopped answering | The triage agent's server replacement fixed it; nobody needed to be woken.",
		}
		if got := r.pushoverAbout(sos.IncidentID, key); !reflect.DeepEqual(got, want) {
			t.Fatalf("Pushover for %s:\n got %q\nwant %q", key, got, want)
		}
	}
	if history := r.history(t, sos.IncidentID); !inOrder(history, "Held for the triage agent until "+r.start.Add(5*time.Minute).UTC().Format("15:04")+" UTC", "The triage agent is replacing a server ("+replaceFailedServers+" in the inventory “production”), so MMHotline waits until "+r.start.Add(11*time.Minute).UTC().Format("15:04")+" UTC") {
		t.Fatalf("history: %q", history)
	}
}

// A held SOS the agent fixed without a replacement: nothing reached
// Pushover, so Pushover gets no resolved message either. Email, which got
// the SOS at once, gets its resolved message.
func TestAHeldSOSTheAgentFixedSendsNoResolvedMessageToPushover(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	r.addEmail(t)
	sos, _, _, _ := replacementSession(t, r)
	r.at(t, time.Minute)
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 2*time.Minute)
	for _, key := range []string{fakesendingservices.PushoverUserKey, fakesendingservices.PushoverOtherUserKey} {
		if got := r.pushoverAbout(sos.IncidentID, key); len(got) != 0 {
			t.Fatalf("Pushover for %s got %q about a held SOS that nobody woke anyone for", key, got)
		}
	}
	if got, want := r.emailStates(t, sos.IncidentID), []string{"sos not suppressed", "resolved-sos not suppressed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("email deliveries %q, want %q", got, want)
	}
	if warnings := startWarnings(t, r.a); len(warnings) != 0 {
		t.Fatalf("start warnings without a replacement: %+v", warnings)
	}
}

// An SOS that woke the person on call resolves as it always did: the
// ordinary resolved message, at low priority.
func TestAnSOSThatWokeThePersonResolvesWithTheOrdinaryMessage(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, _, _, _ := replacementSession(t, r)
	r.at(t, 6*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the person on call was not woken after the wait")
	}
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 7*time.Minute)
	got := r.pushoverAbout(sos.IncidentID, fakesendingservices.PushoverUserKey)
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "-1 [Resolved SOS] The server db02 stopped answering | ") {
		t.Fatalf("Pushover: %q", got)
	}
}

// A replacement whose wait ran out woke the person, so the end is the
// ordinary resolved message under the SOS's own title, not the partner of
// a warn that said nobody would be woken.
func TestAnSOSWokenAfterAReplacementResolvesWithTheOrdinaryMessage(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.at(t, 12*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("not woken at the replacement's limit")
	}
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 13*time.Minute)
	got := r.pushoverAbout(sos.IncidentID, fakesendingservices.PushoverUserKey)
	for _, line := range got {
		if strings.Contains(line, "works again") || strings.Contains(line, "nobody needed to be woken") {
			t.Fatalf("a woken person got the partner message: %q", got)
		}
	}
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "0 [Resolved SOS] The server db02 stopped answering | ") {
		t.Fatalf("Pushover: %q", got)
	}
}

// A run that fails during the replacement wakes the person on call at
// once, saying which run failed, and sends no further warn alert.
func TestAFailedRunDuringAServerReplacementWakesThePersonAtOnce(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, true)
	sos, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.at(t, 6*time.Minute)
	if r.woken(sos.IncidentID) {
		t.Fatal("woken before every_server failed")
	}
	if answer := ask(listedEveryServer); answer.Outcome != triageagent.RanAndFailed {
		t.Fatalf("every_server: %+v", answer)
	}
	r.at(t, 6*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the person on call was not woken when every_server failed")
	}
	if message := r.emergencyMessage(t, sos.IncidentID); !strings.HasPrefix(message, "MMHotline is waking you now. The server replacement failed: "+listedEveryServer+" in the inventory “production” ran and failed.") {
		t.Fatalf("the waking message: %q", message)
	}
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 10)
}

// A replacement that passes its limit wakes the person on call at the
// limit, measured from the replacement's start; before the replacement
// starts, the SOS waits as any other.
func TestAServerReplacementPastItsLimitWakesThePerson(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, _, _, ask := replacementSession(t, r)
	if history := r.history(t, sos.IncidentID); !inOrder(history, "Held for the triage agent until "+r.start.Add(5*time.Minute).UTC().Format("15:04")+" UTC") {
		t.Fatalf("history before the replacement: %q", history)
	}
	r.at(t, 2*time.Minute)
	ask(replaceFailedServers)
	// An SOS that joins during the replacement waits for the same limit,
	// with no second warn alert.
	r.at(t, 4*time.Minute)
	joined := admitSignedAlert(t, r.a, r.source, "sos", "The shop behind db02 is down", "shop")
	r.at(t, 12*time.Minute-time.Second)
	if r.woken(sos.IncidentID) || r.woken(joined.IncidentID) {
		t.Fatal("woken before the replacement's limit")
	}
	r.at(t, 12*time.Minute)
	if !r.woken(sos.IncidentID) || !r.woken(joined.IncidentID) {
		t.Fatal("not woken at the replacement's limit")
	}
	// The two SOS wake together, so Pushover combines them into one message
	// of titles; each SOS's own waking message names the cause.
	for _, incidentID := range []string{sos.IncidentID, joined.IncidentID} {
		var message string
		if err := r.a.Store.db.QueryRow(`SELECT alert_message FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' ORDER BY created_at,rowid LIMIT 1`, incidentID).Scan(&message); err != nil || !strings.HasPrefix(message, "MMHotline is waking you now. The server replacement took longer than 10 minutes.") {
			t.Fatalf("the waking message: %q %v", message, err)
		}
	}
	if history := r.history(t, sos.IncidentID); !inOrder(history, "The server replacement took longer than 10 minutes; waking the person on call") {
		t.Fatalf("history: %q", history)
	}
	r.oneStartWarning(t, sos, r.start.Add(2*time.Minute), 10)
}

// With the setting at 0, a replacement extends nothing and sends no warn
// alert: the SOS wakes the person at its ordinary wait.
func TestAServerReplacementWithTheSettingAtZeroChangesNothing(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	if outcomes, err := r.a.applySettingsParts(context.Background(), Settings{TriageAgent: &SettingsTriageAgent{SOSWaitsForAServerReplacementMinutes: 0}}, nil, []settingsFilePart{settingsSOSWaitsForAServerReplacementPart}); err != nil || outcomes[0].State != "applied" {
		t.Fatalf("applying 0: %+v %v", outcomes, err)
	}
	sos, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.at(t, 5*time.Minute-time.Second)
	if r.woken(sos.IncidentID) {
		t.Fatal("woken before the ordinary wait")
	}
	r.at(t, 5*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("not woken at the ordinary wait")
	}
	if warnings := startWarnings(t, r.a); len(warnings) != 0 {
		t.Fatalf("start warnings with the setting at 0: %+v", warnings)
	}
}

// The agent's report at sos during the replacement wakes the person at
// once, and the waking message names the report.
func TestAnSOSReportDuringAServerReplacementWakesThePersonAtOnce(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, run, answers, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.at(t, 7*time.Minute)
	report(t, run, answers, "0000000000000001", "sos", "# The new server db03 does not boot\n\nThe provider shows a kernel panic.\n")
	r.at(t, 7*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the agent's sos report did not wake the person on call")
	}
	if message := r.emergencyMessage(t, sos.IncidentID); !strings.Contains(message, "The triage agent's last report: “The new server db03 does not boot”.") {
		t.Fatalf("the waking message: %q", message)
	}
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 10)
}

// A session whose run fails after the start warning wakes the person on
// call at once, naming the cause.
func TestASessionThatFailsDuringAServerReplacementWakesThePerson(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, run, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	sendTriageEvent(t, run, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
	waitNoTriageRun(t, r.a)
	r.at(t, 2*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the person on call was not woken when the agent could not run")
	}
	if message := r.emergencyMessage(t, sos.IncidentID); !strings.HasPrefix(message, "MMHotline is waking you now. The triage agent could not run.") {
		t.Fatalf("the waking message: %q", message)
	}
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 10)
}

// A restart during the replacement sends no second warn alert and keeps
// the replacement's limit.
func TestARestartDuringAServerReplacementKeepsItsLimit(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.advance(r.start.Add(3 * time.Minute).Sub(r.a.Now()))
	r.a.CloseTriageAgent()
	r.a = restartApp(t, r.a)
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
		t.Fatal(err)
	}
	r.runs = triageRunFixture(t, r.a)
	r.runStarted(t)
	r.at(t, 11*time.Minute-time.Second)
	if r.woken(sos.IncidentID) {
		t.Fatal("woken before the replacement's limit after the restart")
	}
	r.at(t, 11*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("not woken at the replacement's limit after the restart")
	}
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 10)
}

// An SOS that joins the still-open session after a first replacement ended
// well gets the wait of the replacement the agent starts for it: its own
// warn alert, and no waking at the ordinary wait. Each replacement is a
// fresh outage the agent is fixing.
func TestASecondServerReplacementInTheSameSessionGetsItsOwnWait(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos1, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	if answer := ask(replaceFailedServers); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("first replace_failed_servers: %+v", answer)
	}
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 3*time.Minute)
	if r.woken(sos1.IncidentID) {
		t.Fatal("the first SOS was woken")
	}
	r.oneStartWarning(t, sos1, r.start.Add(time.Minute), 10)
	// Another server fails and its SOS joins the same session.
	r.at(t, 14*time.Minute)
	sos2 := admitSignedAlert(t, r.a, r.source, "sos", "The server db02 stopped answering", "db02-again")
	r.at(t, 15*time.Minute)
	if answer := ask(replaceFailedServers); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("second replace_failed_servers: %+v", answer)
	}
	warnings := startWarnings(t, r.a)
	if len(warnings) != 2 || warnings[1].IncidentID != sos2.IncidentID {
		t.Fatalf("start warnings %+v; want a second one about the second SOS", warnings)
	}
	if want := "not done by " + r.start.Add(25*time.Minute).UTC().Format("15:04") + " UTC."; !strings.Contains(warnings[1].Message, want) {
		t.Fatalf("second start warning %q; want the limit %s", warnings[1].Message, want)
	}
	// The second SOS's ordinary wait ended at 19 minutes, and the first replacement's limit at 11; it keeps waiting.
	r.at(t, 24*time.Minute)
	if r.woken(sos2.IncidentID) {
		t.Fatal("the second SOS was woken before the second replacement's limit")
	}
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02-again")
	r.at(t, 26*time.Minute)
	if r.woken(sos2.IncidentID) {
		t.Fatal("the second SOS was woken although its alert resolved in time")
	}
	if len(startWarnings(t, r.a)) != 2 {
		t.Fatalf("start warnings %+v; want two", startWarnings(t, r.a))
	}
}

// A marked run while a replacement is in progress changes nothing: no
// second warn alert and no new limit.
func TestAMarkedRunDuringAServerReplacementStartsNoSecondOne(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.at(t, 4*time.Minute)
	ask(replaceFailedServers)
	r.oneStartWarning(t, sos, r.start.Add(time.Minute), 10)
	r.at(t, 11*time.Minute-time.Second)
	if r.woken(sos.IncidentID) {
		t.Fatal("woken before the first replacement's limit")
	}
	r.at(t, 11*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("the second marked run moved the limit")
	}
}

// A restart after the second replacement started sends no further warn
// alert and keeps the second limit.
func TestARestartAfterASecondServerReplacementKeepsItsLimit(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	sos1, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 14*time.Minute)
	sos2 := admitSignedAlert(t, r.a, r.source, "sos", "The server db02 stopped answering", "db02-again")
	r.at(t, 15*time.Minute)
	ask(replaceFailedServers)
	r.at(t, 16*time.Minute)
	r.a.CloseTriageAgent()
	r.a = restartApp(t, r.a)
	if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
		t.Fatal(err)
	}
	r.runs = triageRunFixture(t, r.a)
	r.runStarted(t)
	r.at(t, 25*time.Minute-time.Second)
	if r.woken(sos2.IncidentID) {
		t.Fatal("woken before the second replacement's limit after the restart")
	}
	r.at(t, 25*time.Minute)
	if !r.woken(sos2.IncidentID) {
		t.Fatal("not woken at the second replacement's limit after the restart")
	}
	if warnings := startWarnings(t, r.a); len(warnings) != 2 || warnings[0].IncidentID != sos1.IncidentID {
		t.Fatalf("start warnings %+v; want two", warnings)
	}
}

// A second outage soon after the first replacement's site served, inside
// the first replacement's limit: the first SOS resolving ended that
// replacement, so the second replace run starts a new one with its own
// START warn and limit.
func TestASecondOutageSoonAfterAReplacementGetsItsOwnWait(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	_, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.at(t, 5*time.Minute)
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 9*time.Minute)
	sos2 := admitSignedAlert(t, r.a, r.source, "sos", "The server db02 stopped answering", "db02-again")
	r.at(t, 10*time.Minute)
	ask(replaceFailedServers)
	warnings := startWarnings(t, r.a)
	if len(warnings) != 2 || warnings[1].IncidentID != sos2.IncidentID {
		t.Fatalf("start warnings %+v; want a second one about the second SOS", warnings)
	}
	// The first replacement's limit (11 minutes) and the second SOS's
	// ordinary wait (14 minutes) pass; the second limit is 20 minutes.
	r.at(t, 20*time.Minute-time.Second)
	if r.woken(sos2.IncidentID) {
		t.Fatal("the second SOS was woken before its own replacement's limit")
	}
	r.at(t, 20*time.Minute)
	if !r.woken(sos2.IncidentID) {
		t.Fatal("the second SOS was not woken at its own replacement's limit")
	}
}

// When the SOS's own wait for the agent ends after the replacement's limit,
// the person is woken at the SOS's wait, so the START warn names that time.
func TestTheStartWarnNamesTheLaterOfTheTwoWaits(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	ctx := context.Background()
	if outcomes, err := r.a.applySettingsParts(ctx, Settings{TriageAgent: &SettingsTriageAgent{SOSWaitsForTheAgentMinutes: 6, SOSWaitsForAServerReplacementMinutes: 1}}, nil, []settingsFilePart{settingsSOSWaitsForTheAgentPart, settingsSOSWaitsForAServerReplacementPart}); err != nil || outcomes[0].State != "applied" || outcomes[1].State != "applied" {
		t.Fatalf("apply the waits: %+v %v", outcomes, err)
	}
	sos, _, _, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.oneStartWarning(t, sos, r.start, 6)
	r.at(t, 6*time.Minute-time.Second)
	if r.woken(sos.IncidentID) {
		t.Fatal("woken before the time the start warn named")
	}
	r.at(t, 6*time.Minute)
	if !r.woken(sos.IncidentID) {
		t.Fatal("not woken at the time the start warn named")
	}
}

// The START warn goes at once, even right after another message to the same
// contact method: it tells the person when they will be woken.
func TestTheStartWarnSkipsTheOneAMinuteGrouping(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	_, run, answers, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	sendTriageEvent(t, run, triageagent.Event{Event: "report", RequestID: "00000000000000aa", AlertSeverity: "info", ReportText: "# Replacing db02\n\nThe triage agent replaces db02."})
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	var control triageagent.Control
	if err := answers.Decode(&control); err != nil || control.Answer == nil {
		t.Fatalf("report answer: %+v %v", control, err)
	}
	r.at(t, time.Minute+time.Second)
	reported := false
	for _, m := range r.fake.PushoverMessages() {
		reported = reported || strings.Contains(m.Title, "Replacing db02")
	}
	if !reported {
		t.Fatal("the info report did not reach Pushover first")
	}
	ask(replaceFailedServers)
	r.at(t, time.Minute+2*time.Second)
	for _, m := range r.fake.PushoverMessages() {
		if strings.HasPrefix(m.Title, "[Warning] Your site is down") {
			return
		}
	}
	t.Fatal("the START warn waited for the one-a-minute grouping")
}

// The end message of a replacement goes at once, even right after the
// agent's info report to the same contact method: it is the end the START
// warn announced.
func TestTheEndMessageSkipsTheOneAMinuteGrouping(t *testing.T) {
	parallelStartingProcesses(t)
	r := replacementRig(t, false)
	_, run, answers, ask := replacementSession(t, r)
	r.at(t, time.Minute)
	ask(replaceFailedServers)
	r.at(t, 3*time.Minute)
	sendTriageEvent(t, run, triageagent.Event{Event: "report", RequestID: "00000000000000ab", AlertSeverity: "info", ReportText: "# Installing the new servers\n\nThe triage agent installs them."})
	run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
	var control triageagent.Control
	if err := answers.Decode(&control); err != nil || control.Answer == nil {
		t.Fatalf("report answer: %+v %v", control, err)
	}
	r.at(t, 3*time.Minute+time.Second)
	admitSignedAlert(t, r.a, r.source, "resolved-sos", "The server db02 answers again", "db02")
	r.at(t, 3*time.Minute+2*time.Second)
	for _, m := range r.fake.PushoverMessages() {
		if strings.HasPrefix(m.Title, "[Resolved SOS] Your site works again") {
			return
		}
	}
	t.Fatal("the end message waited for the one-a-minute grouping")
}
