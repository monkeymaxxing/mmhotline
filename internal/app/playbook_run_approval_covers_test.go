package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

const (
	replaceFailedServers   = "playbooks/replace_failed_servers.yml"
	destroyReplacedServers = "playbooks/destroy_replaced_servers.yml"
	// alsoApprovesSentence is what the approval of replace_failed_servers
	// says before the click.
	alsoApprovesSentence = "Approving also lets the triage agent run every_server and destroy_replaced_servers once each later in this session, without asking again."
)

// approvalCoversRig is a triage rig whose production inventory lists
// replace_failed_servers, every_server, and destroy_replaced_servers, each
// with Get permission, then act; an approval of replace_failed_servers also
// approves every_server and destroy_replaced_servers once in the same
// session, as the settings file's
// its_approval_also_approves_in_the_same_session would save it. With
// runMMX, the real runner runs a stand-in mmx; without, a fake runner
// stands in for MMX.
func approvalCoversRig(t *testing.T, runMMX string) (*triageSessionRig, *fakeRunner) {
	t.Helper()
	r := newTriageSessionRig(t, true)
	var runner *fakeRunner
	if runMMX == "" {
		runner = &fakeRunner{calls: make(chan fakeRunnerCall, 8)}
		r.a.triageAgent.playbookRunner = runner
	} else {
		installFakeMMXRun(t, r.a, runMMX)
	}
	installFakeMMX(t, r.a, map[string]string{"replace_failed_servers": everyServerExplained, "every_server": everyServerExplained, "destroy_replaced_servers": everyServerExplained})
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, Principal{UnixUser: "fixture"}, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Store.db.Exec(`INSERT INTO agent_playbook_rules_for_listed_playbooks(inventory,playbook_name,rule,warning_wait_minutes,approvals_needed,its_approval_also_approves_in_the_same_session,changed_at,changed_by) VALUES
('production','replace_failed_servers','get_permission_then_act',0,1,'every_server,destroy_replaced_servers',1,'the settings file'),
('production','every_server','get_permission_then_act',0,1,'',1,'the settings file'),
('production','destroy_replaced_servers','get_permission_then_act',0,1,'',1,'the settings file')`); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{replaceFailedServers: "fleet_stages: []\n", listedEveryServer: "fleet_stages: []\n", destroyReplacedServers: "fleet_stages: []\n"})
	return r, runner
}

// coversSession starts a session with an SOS about db01 and returns its
// run and a function that asks for a playbook on an inventory in that run.
func coversSession(t *testing.T, r *triageSessionRig, key string) (Alert, fakeTriageRun, func(playbook, inventory string) triageagent.Answer) {
	t.Helper()
	sos := r.alert(t, "sos", "The server "+key+" stopped answering", key)
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-" + key})
	answers := json.NewDecoder(run.connection)
	next := 0
	return sos, run, func(playbook, inventory string) triageagent.Answer {
		t.Helper()
		next++
		requestID := fmt.Sprintf("%016x", next)
		sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: requestID, Playbook: playbook, Inventory: inventory, Diagnosis: testDiagnosis})
		run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
		var control triageagent.Control
		if err := answers.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != requestID {
			t.Fatalf("answer: %+v %v", control, err)
		}
		return *control.Answer
	}
}

// waitingRow is one row of the waiting table as these tests read it.
type waitingRow struct {
	id, playbook, inventory, state, coveredBy, result string
}

func waitingRows(t *testing.T, a *App) []waitingRow {
	t.Helper()
	rows, err := a.Store.db.Query(`SELECT id,playbook,inventory,state,covered_by_approval_request_id,result_message FROM triage_agent_playbook_runs_waiting_for_a_person ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []waitingRow
	for rows.Next() {
		var w waitingRow
		if err := rows.Scan(&w.id, &w.playbook, &w.inventory, &w.state, &w.coveredBy, &w.result); err != nil {
			t.Fatal(err)
		}
		all = append(all, w)
	}
	return all
}

func approvalAlerts(t *testing.T, a *App, playbook string) int {
	t.Helper()
	return countRows(t, a, `SELECT count(*) FROM alerts WHERE severity='sos' AND title LIKE ?`, "Approval needed to run "+playbook+"%")
}

// A person approves replace_failed_servers once, and the approval says
// first that it also approves every_server and destroy_replaced_servers.
// Later in the same session each of the two runs once without an approval
// request: no alert, a covered run whose record, history, result, and page
// name the approval and who gave it. A second request for a covered
// playbook asks a person again, and a request on another inventory is not
// covered.
func TestOneApprovalCoversTheNamedPlaybooksOnceInTheSession(t *testing.T) {
	r, runner := approvalCoversRig(t, "")
	sos, _, ask := coversSession(t, r, "db01")

	if answer := ask(replaceFailedServers, "production"); answer.Outcome != triageagent.WaitsForAPerson {
		t.Fatalf("replace_failed_servers: %+v", answer)
	}
	approval := waitingRows(t, r.a)[0]
	var message string
	if err := r.a.Store.db.QueryRow(`SELECT message FROM alerts WHERE severity='sos' AND title LIKE 'Approval needed to run playbooks/replace_failed_servers.yml%'`).Scan(&message); err != nil || !strings.Contains(message, alsoApprovesSentence) {
		t.Fatalf("the approval alert does not say what else it approves: %v %q", err, message)
	}
	// The page says it before the click, beside Approve and run.
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	browser := approvalBrowsers(t, server)(r.p.SignInEmailAddress)
	if text := approvalPageText(browser.get("/approvals/"+approval.id, 200)); !phrasesInOrder(text, "MMHotline runs the playbook as soon as one person approves.", alsoApprovesSentence, "Approve and run") {
		t.Fatalf("the approval page: %s", text)
	}
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, approval.id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	if call := <-runner.calls; call.request.Playbook != replaceFailedServers {
		t.Fatalf("ran %+v", call)
	}

	coveredBy := "Responder approved " + replaceFailedServers + " in the inventory “production” (approval request " + approval.id + "), and that approval also approves "
	for _, playbook := range []string{listedEveryServer, destroyReplacedServers} {
		answer := ask(playbook, "production")
		if answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, coveredBy+playbook+" once in the same triage session. So MMHotline runs the playbook next without asking a person again.") {
			t.Fatalf("%s: %+v", playbook, answer)
		}
		if asked := approvalAlerts(t, r.a, playbook); asked != 0 {
			t.Fatalf("%s: %d approval alerts", playbook, asked)
		}
		rows := waitingRows(t, r.a)
		covered := rows[len(rows)-1]
		if covered.playbook != playbook || covered.state != "approved" || covered.coveredBy != approval.id {
			t.Fatalf("%s: the covered run %+v", playbook, covered)
		}
		runApprovedPlaybooks(t, r.a)
		if call := <-runner.calls; call.request.Playbook != playbook {
			t.Fatalf("ran %+v", call)
		}
		covered = waitingRows(t, r.a)[len(rows)-1]
		if covered.state != "ran_and_succeeded" || !strings.HasPrefix(covered.result, coveredBy+playbook+" once in the same triage session. The run used commit ") {
			t.Fatalf("%s: the covered run's result %+v", playbook, covered)
		}
		if history := incidentHistory(t, r.a, r.p, sos.IncidentID); !inOrder(history, "MMHotline runs "+playbook+" in the inventory “production” at commit ", covered.result) {
			t.Fatalf("%s: history %q", playbook, history)
		}
		view, err := r.a.ApprovalRequest(context.Background(), r.p, covered.id)
		if err != nil || view.CoveredBy != coveredBy+playbook+" once in the same triage session." || !strings.Contains(commandApprovalRequest(view).Text(), "Approved without a new request: "+view.CoveredBy) {
			t.Fatalf("%s: the covered run's page %v %+v", playbook, err, view)
		}
		if text := approvalPageText(browser.get("/approvals/"+covered.id, 200)); !strings.Contains(text, "Approved without a new request: "+view.CoveredBy) {
			t.Fatalf("%s: the covered run's page: %s", playbook, text)
		}
	}

	// Each named playbook is covered once; another inventory is not covered.
	for _, again := range []struct{ playbook, inventory string }{{listedEveryServer, "production"}, {destroyReplacedServers, "staging"}} {
		before := len(waitingRows(t, r.a))
		if answer := ask(again.playbook, again.inventory); answer.Outcome != triageagent.WaitsForAPerson || !strings.HasSuffix(answer.Message, approvalAskedText(true)) {
			t.Fatalf("%s in %s: %+v", again.playbook, again.inventory, answer)
		}
		rows := waitingRows(t, r.a)
		if len(rows) != before+1 || rows[before].state != "open" || rows[before].coveredBy != "" || rows[before].inventory != again.inventory {
			t.Fatalf("%s in %s: %+v", again.playbook, again.inventory, rows)
		}
	}
	if asked := approvalAlerts(t, r.a, listedEveryServer) + approvalAlerts(t, r.a, destroyReplacedServers); asked != 2 {
		t.Fatalf("%d approval alerts after the covers were used", asked)
	}
	noRunnerCall(t, runner)
}

// A refused approval covers nothing: every_server, which the refused
// approval would have covered, asks a person.
func TestARefusedApprovalCoversNothing(t *testing.T) {
	r, runner := approvalCoversRig(t, "")
	_, _, ask := coversSession(t, r, "db01")
	ask(replaceFailedServers, "production")
	if err := r.a.RefusePlaybookRun(context.Background(), r.p, waitingRows(t, r.a)[0].id, "Wait for the provider's status page."); err != nil {
		t.Fatal(err)
	}
	if answer := ask(listedEveryServer, "production"); !strings.HasSuffix(answer.Message, approvalAskedText(true)) {
		t.Fatalf("every_server after a refusal: %+v", answer)
	}
	if rows := waitingRows(t, r.a); len(rows) != 2 || rows[1].state != "open" || rows[1].coveredBy != "" {
		t.Fatalf("rows: %+v", rows)
	}
	runApprovedPlaybooks(t, r.a)
	noRunnerCall(t, runner)
}

// The cover ends with its session: a later session's request for
// every_server asks a person, though the earlier approval's cover was
// never used.
func TestACoverEndsWithItsSession(t *testing.T) {
	r, runner := approvalCoversRig(t, "")
	sos, run, ask := coversSession(t, r, "db01")
	ask(replaceFailedServers, "production")
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, waitingRows(t, r.a)[0].id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	<-runner.calls
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	r.drive(t)
	r.end(t, r.nextRun(t), triageagent.Event{Event: "finished", Answer: "The server was replaced."})
	r.resolve(t, sos.IncidentID)

	_, _, askLater := coversSession(t, r, "app003")
	if sessions := countRows(t, r.a, `SELECT count(*) FROM triage_sessions`); sessions != 2 {
		t.Fatalf("%d sessions", sessions)
	}
	if answer := askLater(listedEveryServer, "production"); !strings.HasSuffix(answer.Message, approvalAskedText(true)) {
		t.Fatalf("every_server in a later session: %+v", answer)
	}
	if rows := waitingRows(t, r.a); len(rows) != 2 || rows[1].state != "open" || rows[1].coveredBy != "" {
		t.Fatalf("rows: %+v", rows)
	}
}

// A covered run still passes the commit check before MMX: while a commit
// no trusted key signed waits for review, the covered run does not run, its
// record says why, and the cover is used.
func TestACoveredRunStillWaitsForTheCommitCheck(t *testing.T) {
	records := t.TempDir()
	r, _ := approvalCoversRig(t, miniRunMMX(records))
	_, _, ask := coversSession(t, r, "db01")
	ask(replaceFailedServers, "production")
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, waitingRows(t, r.a)[0].id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	if answer := ask(listedEveryServer, "production"); answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "So MMHotline runs the playbook next without asking a person again.") {
		t.Fatalf("every_server: %+v", answer)
	}
	waiting := pushUnsignedToOrigin(t, r.a, "roles/x.yml", "# a change nobody signed\n")
	if _, err := r.a.checkCommitsForCommand(t.Context(), waiting); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	covered := waitingRows(t, r.a)[1]
	if covered.state != "could_not_run" || !strings.Contains(covered.result, "commit "+waiting[:7]+", which no key the team trusts signed, waits for a person's review") {
		t.Fatalf("the covered run: %+v", covered)
	}
	if calls := readMMXCalls(t, records); len(calls) != 1 || !strings.Contains(calls[0], "replace_failed_servers") {
		t.Fatalf("MMX calls: %q", calls)
	}
}

// The real order of a replacement: a person approves replace_failed_servers;
// its run commits an inventory change from its own clone, signed by a key
// the team trusts, as util01 does; every_server and destroy_replaced_servers
// then run at that newer commit, with a different inventory list. The cover
// matches the inventory's name, not its servers or the commit, so both still
// run without asking a person, each at the newest commit.
func TestACoverHoldsAcrossTheReplacementsInventoryCommit(t *testing.T) {
	records := t.TempDir()
	r, _ := approvalCoversRig(t, miniRunMMX(records))
	_, _, ask := coversSession(t, r, "app001")
	ask(replaceFailedServers, "production")
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, waitingRows(t, r.a)[0].id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)

	// What replace_failed_servers commits: app001 out, app003 in, pushed from
	// its own clone and signed by a trusted key.
	origin := testGit(t, filepath.Join(r.a.Store.Dir, "systems_repository"), "remote", "get-url", "origin")
	clone := filepath.Join(t.TempDir(), "clone")
	testGit(t, filepath.Dir(clone), "clone", "--quiet", origin, clone)
	if err := os.MkdirAll(filepath.Join(clone, "inventories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "inventories", "production.yml"), []byte("app003:\n  address: 10.20.0.13\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, clone, "add", "inventories/production.yml")
	testGit(t, clone, "commit", "--quiet", "-m", "Replace app001 with app003")
	testGit(t, clone, "push", "--quiet", "origin", "main")
	replaced := testGit(t, clone, "rev-parse", "HEAD")

	for i, playbook := range []string{listedEveryServer, destroyReplacedServers} {
		if answer := ask(playbook, "production"); answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "So MMHotline runs the playbook next without asking a person again.") {
			t.Fatalf("%s after the inventory commit: %+v", playbook, answer)
		}
		runApprovedPlaybooks(t, r.a)
		covered := waitingRows(t, r.a)[i+1]
		if covered.playbook != playbook || covered.state != "ran_and_succeeded" || !strings.Contains(covered.result, "The run used commit "+replaced) {
			t.Fatalf("%s: %+v", playbook, covered)
		}
	}
	if asked := approvalAlerts(t, r.a, listedEveryServer) + approvalAlerts(t, r.a, destroyReplacedServers); asked != 0 {
		t.Fatalf("%d approval alerts for covered runs", asked)
	}
	if calls := readMMXCalls(t, records); len(calls) != 3 {
		t.Fatalf("MMX calls: %q", calls)
	}
}

// The cover names playbooks exactly: an approval that also approves
// every-server covers neither every_server, which a LIKE pattern's "_"
// would match, nor Every-Server, which a LIKE would match in any case.
func TestACoverMatchesPlaybookNamesExactly(t *testing.T) {
	r, runner := approvalCoversRig(t, "")
	if _, err := r.a.Store.db.Exec(`UPDATE agent_playbook_rules_for_listed_playbooks SET its_approval_also_approves_in_the_same_session='every-server,Destroy_Replaced_Servers' WHERE playbook_name='replace_failed_servers'`); err != nil {
		t.Fatal(err)
	}
	_, _, ask := coversSession(t, r, "db01")
	ask(replaceFailedServers, "production")
	if err := r.a.ApprovePlaybookRun(context.Background(), r.p, waitingRows(t, r.a)[0].id); err != nil {
		t.Fatal(err)
	}
	runApprovedPlaybooks(t, r.a)
	<-runner.calls
	for _, playbook := range []string{listedEveryServer, destroyReplacedServers} {
		before := len(waitingRows(t, r.a))
		if answer := ask(playbook, "production"); answer.Outcome != triageagent.WaitsForAPerson || !strings.HasSuffix(answer.Message, approvalAskedText(true)) {
			t.Fatalf("%s: %+v", playbook, answer)
		}
		if rows := waitingRows(t, r.a); len(rows) != before+1 || rows[before].state != "open" || rows[before].coveredBy != "" {
			t.Fatalf("%s was covered: %+v", playbook, rows)
		}
	}
	runApprovedPlaybooks(t, r.a)
	noRunnerCall(t, runner)
}
