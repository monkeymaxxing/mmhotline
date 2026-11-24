package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"html"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/fakesendingservices"
	"mmhotline/internal/triageagent"
)

// A warn from an alert source is held for the triage agent for the settings
// file's triage_agent.warn_waits_for_the_agent_minutes, here 3. These tests
// walk it through MMHotline's own paths: the alert intake, the background
// loop, a fake sandbox, a stand-in MMX, and a fake Pushover.

type heldWarnRig struct{ *triageHoldRig }

func newHeldWarnRig(t *testing.T, minutes int) *heldWarnRig {
	t.Helper()
	r := &heldWarnRig{newTriageHoldRig(t)}
	r.setWait(t, minutes)
	return r
}

func (r *heldWarnRig) setWait(t *testing.T, minutes int) {
	t.Helper()
	outcomes, err := r.a.applySettingsParts(context.Background(), Settings{TriageAgent: &SettingsTriageAgent{WarnWaitsForTheAgentMinutes: &minutes}}, nil, []settingsFilePart{settingsWarnWaitsForTheAgentPart})
	if err != nil || outcomes[0].State != "applied" {
		t.Fatalf("applying the wait: %+v %v", outcomes, err)
	}
}

// warnArrives admits a warn from the alert source and lets the background
// loop read it; the test's clock starts.
func (r *heldWarnRig) warnArrives(t *testing.T, title, key string) Alert {
	t.Helper()
	r.start = r.a.Now()
	alert := admitSignedAlert(t, r.a, r.source, "warn", title, key)
	backgroundRounds(t, r.a)
	return alert
}

// pushovers lists the Pushover messages whose title or text names the alert,
// to both people.
func (r *heldWarnRig) pushovers(titleText string) []fakesendingservices.PushoverMessage {
	var found []fakesendingservices.PushoverMessage
	for _, m := range r.fake.PushoverMessages() {
		// The quiet start message names the alert too; it is not the warn.
		if strings.Contains(m.Title, "The AI triage agent is looking into") {
			continue
		}
		if strings.Contains(m.Title, titleText) || strings.Contains(m.Message, titleText) {
			found = append(found, m)
		}
	}
	return found
}

// reachedSomeone checks that a Pushover message about the text was already
// sent: the person who got the quiet start message a moment ago waits for
// the minimum interval between Pushover messages, so the other person's
// message is the proof that nothing held the warn.
func (r *heldWarnRig) reachedSomeone(t *testing.T, text string) {
	t.Helper()
	if len(r.pushovers(text)) == 0 {
		t.Fatalf("no Pushover message about %q yet", text)
	}
}

// sentOnce lets Pushover's minimum interval between messages pass, then
// checks that each of the two people got exactly one Pushover message about
// the text, at the priority.
func (r *heldWarnRig) sentOnce(t *testing.T, text string, priority int) {
	t.Helper()
	r.advance(2 * time.Minute)
	backgroundRounds(t, r.a)
	found := r.pushovers(text)
	if len(found) != 2 {
		t.Fatalf("%d Pushover messages about %q; want one to each of 2 people: %+v", len(found), text, found)
	}
	for _, m := range found {
		if m.Priority != priority {
			t.Fatalf("the message about %q has priority %d, want %d", text, m.Priority, priority)
		}
	}
}

func (r *heldWarnRig) hold(t *testing.T, alertID string) (outcome, link string, ended bool) {
	t.Helper()
	var endedAt int64
	if err := r.a.Store.db.QueryRow(`SELECT ended_at,outcome,taken_over_link FROM held_warn_alerts WHERE alert_id=?`, alertID).Scan(&endedAt, &outcome, &link); err != nil {
		t.Fatalf("hold of %s: %v", alertID, err)
	}
	return outcome, link, endedAt != 0
}

func (r *heldWarnRig) get(t *testing.T, path string) string {
	t.Helper()
	request := httptest.NewRequest("GET", "https://mmhotline.example.com"+path, nil)
	addAccessToken(request, r.s.Principal.SignInEmailAddress)
	response := httptest.NewRecorder()
	r.a.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("%s: %d", path, response.Code)
	}
	return html.UnescapeString(response.Body.String())
}

const loadWarning = "Load on app002 is high"

// The load-change walk: the agent asks for a playbook run under Warn, wait,
// then act within the wait. MMHotline's own warning about the run is the one
// warning that reaches people; the original warn is never delivered.
func TestHeldWarnTakenOverByAPlaybookRunRequest(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 3)
	prepareWarnedRun(t, r.a, "", 10)
	alert := r.warnArrives(t, loadWarning, "app002-load")
	run := r.runStarted(t)
	if got := r.pushovers(loadWarning); len(got) != 0 {
		t.Fatalf("the held warn reached people: %+v", got)
	}
	heldText := "MMHotline holds this warning for up to 3 minutes while the triage agent looks into it; it reaches people then, unless the agent's report or playbook run request names it."
	if page := r.get(t, "/alerts/"+alert.ID); !strings.Contains(page, heldText) {
		t.Errorf("the alert page while held:\n%s", page)
	}
	if dashboard := r.get(t, "/dashboard"); !strings.Contains(liveSectionText(dashboard, "held-warnings"), loadWarning) || !strings.Contains(liveSectionText(dashboard, "held-warnings"), heldText) {
		t.Errorf("the dashboard while held: %q", liveSectionText(dashboard, "held-warnings"))
	}
	r.at(t, time.Minute)
	if answer := requestPlaybookRun(t, run, json.NewDecoder(run.connection), "0000000000000001"); answer.Outcome != triageagent.WaitsForAPerson {
		t.Fatalf("answer: %+v", answer)
	}
	r.at(t, time.Minute)
	if len(r.pushovers("MMHotline will run")) != 2 {
		t.Fatalf("MMHotline's own warning reached %d messages, want one to each of 2 people", len(r.pushovers("MMHotline will run")))
	}
	r.at(t, 10*time.Minute)
	if got := r.pushovers(loadWarning); len(got) != 0 {
		t.Fatalf("the original warn was delivered after the agent took it over: %+v", got)
	}
	if outcome, link, ended := r.hold(t, alert.ID); outcome != "taken_over" || !ended || !strings.HasPrefix(link, "/triage-sessions/") {
		t.Fatalf("hold: %q %q %v", outcome, link, ended)
	}
	page := r.get(t, "/alerts/"+alert.ID)
	if !strings.Contains(page, "Taken over by <a href=\"/triage-sessions/") || !strings.Contains(page, "the triage agent's playbook run request</a> at ") {
		t.Errorf("the alert page after the take-over:\n%s", page)
	}
	if strings.Contains(r.get(t, "/dashboard"), `id="held-warnings"`) {
		t.Error("the dashboard still lists a warn that was taken over")
	}
}

// A warn the agent fixes: it runs an act_then_report playbook and sends an
// info report. No warn notification is ever delivered, and the info report
// goes at Pushover's low priority.
func TestHeldWarnTheAgentFixesSendsOnlyTheInfoReport(t *testing.T) {
	parallelStartingProcesses(t)
	r := newHeldWarnRig(t, 3)
	records := t.TempDir()
	prepareApprovals(t, r.a, fakeRunMMX(records, "echo 'PLAY RECAP app002 ok=1 changed=1'\n"))
	if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", string(actionkinds.RebootAServer), AgentPlaybookRule{Rule: actThenReport}); err != nil {
		t.Fatal(err)
	}
	alert := r.warnArrives(t, loadWarning, "app002-load")
	run := r.runStarted(t)
	r.at(t, 30*time.Second)
	answers := json.NewDecoder(run.connection)
	if answer := requestPlaybookRun(t, run, answers, "0000000000000001"); answer.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("answer: %+v", answer)
	}
	report(t, run, answers, "0000000000000002", "info", "# Rebooted app002; the load is normal\n")
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	waitNoTriageRun(t, r.a)
	r.at(t, 4*time.Minute)
	if got := r.pushovers(loadWarning); len(got) != 0 {
		t.Fatalf("the warn was delivered although the agent fixed it: %+v", got)
	}
	r.sentOnce(t, "Rebooted app002; the load is normal", -1)
	if outcome, _, ended := r.hold(t, alert.ID); outcome != "taken_over" || !ended {
		t.Fatalf("hold: %q %v", outcome, ended)
	}
}

// A warn the agent cannot handle itself.
func TestHeldWarnTheAgentCannotHandle(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("the agent reports at warn: only the report sounds", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		alert := r.warnArrives(t, loadWarning, "app002-load")
		run := r.runStarted(t)
		r.at(t, time.Minute)
		report(t, run, json.NewDecoder(run.connection), "0000000000000001", "warn", "# app002 needs a person to look at its disks\n")
		r.at(t, 5*time.Minute)
		if got := r.pushovers(loadWarning); len(got) != 0 {
			t.Fatalf("the original warn was delivered: %+v", got)
		}
		r.sentOnce(t, "app002 needs a person to look at its disks", 0)
		if outcome, link, _ := r.hold(t, alert.ID); outcome != "taken_over" || !strings.HasPrefix(link, "/alerts/") {
			t.Fatalf("hold: %q %q", outcome, link)
		}
		if page := r.get(t, "/alerts/"+alert.ID); !strings.Contains(page, "the triage agent's report</a> at ") {
			t.Errorf("the alert page:\n%s", page)
		}
	})
	t.Run("the agent stays silent: delivered once, at the end of the wait", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		alert := r.warnArrives(t, loadWarning, "app002-load")
		r.runStarted(t)
		r.at(t, 3*time.Minute-time.Second)
		if got := r.pushovers(loadWarning); len(got) != 0 {
			t.Fatalf("delivered before the end of the wait: %+v", got)
		}
		r.at(t, 3*time.Minute)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
		r.at(t, 10*time.Minute)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
		if page := r.get(t, "/alerts/"+alert.ID); !strings.Contains(page, "MMHotline delivered this warning at "+r.start.Add(3*time.Minute).UTC().Format("15:04")) {
			t.Errorf("the alert page after delivery:\n%s", page)
		}
	})
	t.Run("the triage agent is not set up: delivered at once", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		if _, err := r.a.Store.db.Exec(`DELETE FROM triage_agent_setup`); err != nil {
			t.Fatal(err)
		}
		r.warnArrives(t, loadWarning, "app002-load")
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("the agent is removed while the warn is held: delivered at once", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		r.warnArrives(t, loadWarning, "app002-load")
		r.runStarted(t)
		// Through a transaction, as every change MMHotline makes: only a
		// committed transaction tells the background loop that the team's
		// database changed and its next visit is due.
		if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM triage_agent_setup`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r.at(t, 30*time.Second)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("MMHotline's copy of the systems repository is missing: delivered at once", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		copy := filepath.Join(r.a.Store.Dir, "systems_repository")
		if err := os.Rename(copy, copy+".elsewhere"); err != nil {
			t.Fatal(err)
		}
		r.warnArrives(t, loadWarning, "app002-load")
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("the session's run fails: delivered at once", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		r.warnArrives(t, loadWarning, "app002-load")
		run := r.runStarted(t)
		r.at(t, 30*time.Second)
		if got := r.pushovers(loadWarning); len(got) != 0 {
			t.Fatalf("delivered while the run goes on: %+v", got)
		}
		sendTriageEvent(t, run, triageagent.Event{Event: "failed", Reason: "The coding agent's service refused the request: 529 Overloaded."})
		waitNoTriageRun(t, r.a)
		r.at(t, 30*time.Second)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("the run ends having done nothing: delivered at once", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		r.warnArrives(t, loadWarning, "app002-load")
		run := r.runStarted(t)
		sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "Nothing to do."})
		waitNoTriageRun(t, r.a)
		r.at(t, 30*time.Second)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
}

// Only a warn is held: an info alert goes at once, and with the setting at 0
// a warn goes at once as before. Without the setting, a warn waits the
// default 15 minutes.
func TestHeldWarnOnlyAWarnWithTheSettingAbove0IsHeld(t *testing.T) {
	parallelStartingProcesses(t)
	t.Run("the setting is 0", func(t *testing.T) {
		r := newHeldWarnRig(t, 0)
		alert := r.warnArrives(t, loadWarning, "app002-load")
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
		if n := countRows(t, r.a, `SELECT count(*) FROM held_warn_alerts WHERE alert_id=?`, alert.ID); n != 0 {
			t.Fatal("a warn was recorded as held with the setting at 0")
		}
	})
	t.Run("the setting was never applied", func(t *testing.T) {
		r := &heldWarnRig{newTriageHoldRig(t)}
		r.warnArrives(t, loadWarning, "app002-load")
		r.runStarted(t)
		r.at(t, 15*time.Minute-time.Second)
		if n := len(r.pushovers(loadWarning)); n != 0 {
			t.Fatalf("%d messages before the default wait ended", n)
		}
		r.at(t, 15*time.Minute)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("the settings file leaves the setting out", func(t *testing.T) {
		r := &heldWarnRig{newTriageHoldRig(t)}
		outcomes, err := r.a.applySettingsParts(context.Background(), Settings{TriageAgent: &SettingsTriageAgent{}}, nil, []settingsFilePart{settingsWarnWaitsForTheAgentPart})
		if err != nil || outcomes[0].State != "applied" {
			t.Fatalf("applying: %+v %v", outcomes, err)
		}
		alert := r.warnArrives(t, loadWarning, "app002-load")
		var heldUntil int64
		if err := r.a.Store.db.QueryRow(`SELECT held_until FROM held_warn_alerts WHERE alert_id=?`, alert.ID).Scan(&heldUntil); err != nil || heldUntil != r.start.Add(15*time.Minute).Unix() {
			t.Fatalf("held until %d, want 15 minutes after arrival: %v", heldUntil, err)
		}
	})
	t.Run("an info alert", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		r.start = r.a.Now()
		admitSignedAlert(t, r.a, r.source, "info", "Backup finished", "")
		backgroundRounds(t, r.a)
		r.sentOnce(t, "Backup finished", -1)
	})
}

// A hold keeps its deadline across a restart, and ends exactly once.
func TestHeldWarnSurvivesARestart(t *testing.T) {
	parallelStartingProcesses(t)
	restart := func(t *testing.T, r *heldWarnRig, at time.Duration) {
		t.Helper()
		run := r.runStarted(t)
		sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
		waitSessionIDSaved(t, r.a, "session-one")
		r.advance(r.start.Add(at).Sub(r.a.Now()))
		r.a.CloseTriageAgent()
		r.a = restartApp(t, r.a)
		if err := r.a.LoadSendingServiceSetups(context.Background(), nil, r.sendingServices.Twilio.APIBaseURL[:len(r.sendingServices.Twilio.APIBaseURL)-len("/twilio")]); err != nil {
			t.Fatal(err)
		}
		r.runs = triageRunFixture(t, r.a)
	}
	t.Run("delivered at the deadline", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		r.warnArrives(t, loadWarning, "app002-load")
		restart(t, r, time.Minute)
		r.runStarted(t)
		r.at(t, 3*time.Minute-time.Second)
		if got := r.pushovers(loadWarning); len(got) != 0 {
			t.Fatalf("delivered before the deadline: %+v", got)
		}
		r.at(t, 3*time.Minute)
		r.at(t, 8*time.Minute)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("delivered at once when the deadline passed during the restart", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		r.warnArrives(t, loadWarning, "app002-load")
		restart(t, r, 4*time.Minute)
		backgroundRounds(t, r.a)
		r.reachedSomeone(t, loadWarning)
		r.sentOnce(t, loadWarning, 0)
	})
	t.Run("taken over by the report of the resumed run", func(t *testing.T) {
		r := newHeldWarnRig(t, 3)
		alert := r.warnArrives(t, loadWarning, "app002-load")
		restart(t, r, time.Minute)
		resumed := r.runStarted(t)
		report(t, resumed, json.NewDecoder(resumed.connection), "0000000000000001", "info", "# Freed space on app002\n")
		r.at(t, 6*time.Minute)
		if got := r.pushovers(loadWarning); len(got) != 0 {
			t.Fatalf("the original warn was delivered: %+v", got)
		}
		if outcome, _, _ := r.hold(t, alert.ID); outcome != "taken_over" {
			t.Fatalf("hold: %q", outcome)
		}
	})
}
