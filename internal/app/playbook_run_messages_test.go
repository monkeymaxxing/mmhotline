package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

const rebootUpdatesPlaybook = "playbooks/reboot_servers_after_updates.yml"
const rebootUpdatesReason = "app002 runs its image's kernel, which has a flaw attackers exploit from inside a container. Only app002 waits for a reboot; this run reboots servers whose updates wait for a reboot, one at a time."

// The explain output includes ready shell tasks and conditional reboot and
// wait_for tasks. Every server is a possible target, not a promised change.
func rebootUpdatesExplanation(t *testing.T) string {
	t.Helper()
	var tasks []map[string]any
	for index, server := range []string{"app001", "app002", "staging01"} {
		modules := []string{"shell", "reboot", "reboot"}
		modules = append(modules, strings.Fields(strings.Repeat("wait_for ", 6-index/2))...)
		for _, module := range modules {
			status := "conditional"
			if module == "shell" {
				status = "ready"
			}
			tasks = append(tasks, map[string]any{"path": "find_and_reboot/task_" + module, "node": server, "action": module, "module_kind": "core", "status": status})
		}
	}
	output, err := json.Marshal(map[string]any{"schema_version": 2, "fleet_stages": []any{map[string]any{"path": "find_and_reboot", "node_tasks": tasks}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestPlaybookRunNotificationMessages(t *testing.T) {
	parallelStartingProcesses(t)
	for _, warning := range []bool{false, true} {
		t.Run(fmt.Sprintf("warning=%v", warning), func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			installFakeMMX(t, r.a, map[string]string{"reboot_servers_after_updates": rebootUpdatesExplanation(t)})
			commit := commitToCopyAndOrigin(t, r.a, map[string]string{rebootUpdatesPlaybook: "fleet_stages: []\n"})
			if warning {
				if err := setAgentPlaybookRule(context.Background(), r.a, Principal{UnixUser: "fixture"}, "servers", string(actionkinds.UnrecognizedTask), AgentPlaybookRule{Rule: warnWaitThenAct, WarningWaitMinutes: 10}); err != nil {
					t.Fatal(err)
				}
			}
			r.alert(t, "warn", "app002 needs a reboot", "")
			r.drive(t)
			run := r.nextRun(t)
			sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
			sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: rebootUpdatesPlaybook, Inventory: "servers", Diagnosis: "\n# Diagnosis\n\n" + rebootUpdatesReason + "\n\n## Evidence\nDetails stay on the card."})
			run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
			var control triageagent.Control
			if err := json.NewDecoder(run.connection).Decode(&control); err != nil || control.Answer == nil {
				t.Fatalf("answer: %+v %v", control, err)
			}
			if !strings.Contains(control.Answer.Message, "find_and_reboot/task_shell") || !strings.Contains(control.Answer.Message, "did not recognize") {
				t.Fatalf("agent lost details: %+v", control.Answer)
			}
			var title, message string
			if err := r.a.Store.db.QueryRow(`SELECT title,message FROM alerts WHERE actor=?`, playbookRunApprovalsActor).Scan(&title, &message); err != nil {
				t.Fatal(err)
			}
			why := "The playbook run waits for your approval because MMHotline cannot check what 26 of the playbook's tasks do (the tasks use shell, reboot, and wait_for), and such tasks need Get permission, then act (1 approval)."
			wantTitle := "Approval needed to run " + rebootUpdatesPlaybook + " in the inventory “servers”: it can change app001, app002, and staging01"
			// Each message links the request's own page; the approval's
			// incident page shows it in short. The warning came from a warn
			// alert, so it has no incident.
			var wantMessage string
			if warning {
				wantTitle = "MMHotline will run " + rebootUpdatesPlaybook + " in the inventory “servers” for the triage agent at 12:10 UTC unless someone presses Stop: it can change app001, app002, and staging01"
				wantMessage = "The triage agent's reason: " + rebootUpdatesReason + "\n\nMMHotline runs the playbook at 12:10 UTC unless someone presses Stop before then. The rule is Warn, wait 10 minutes, then act.\n\nPress Stop on the playbook run page: https://mmhotline.example.com/approvals/" + onlyWarnedRunRecord(t, r.a).id
			} else {
				wantMessage = "The triage agent's reason: " + rebootUpdatesReason + "\n\n" + why + "\n\nThe incident page shows a summary of the playbook run request. Approve or refuse the request on the approval page, which lists each step and the triage agent's full diagnosis: https://mmhotline.example.com/approvals/" + onlyApprovalRecord(t, r.a).id
			}
			if title != wantTitle || message != wantMessage {
				t.Fatalf("title: %s\nmessage: %s", title, message)
			}
			if strings.Contains(message, "find_and_reboot/") || strings.Contains(message, "did not recognize") {
				t.Fatal(message)
			}
			t.Logf("TITLE:\n%s\nMESSAGE:\n%s", title, message)
			if !warning {
				record := onlyApprovalRecord(t, r.a)
				wantHistory := "Approval needed to run " + rebootUpdatesPlaybook + " in the inventory “servers”, at commit " + commit + ": it can change app001, app002, and staging01. The playbook run needs approval from 1 person. " + why
				history := incidentHistory(t, r.a, r.p, record.incidentID)
				found := false
				for _, entry := range history {
					if entry == wantHistory {
						found = true
						t.Logf("HISTORY:\n%s", entry)
					}
				}
				if !found {
					t.Fatalf("history: %q", history)
				}
			}
			r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
		})
	}
}

func TestDiagnosisNotificationParagraph(t *testing.T) {
	parallelStartingProcesses(t)
	r := newTriageSessionRig(t, true)
	prepareApprovals(t, r.a, "")
	r.alert(t, "warn", "app002 answers slowly", "")
	r.drive(t)
	run := r.nextRun(t)
	sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: "0000000000000001", Playbook: rebootPlaybook, Inventory: "production", Diagnosis: "# Diagnosis\n\n- Only a list\n- No prose"})
	var control triageagent.Control
	if err := json.NewDecoder(run.connection).Decode(&control); err != nil {
		t.Fatal(err)
	}
	var message string
	if err := r.a.Store.db.QueryRow(`SELECT message FROM alerts WHERE actor=?`, playbookRunApprovalsActor).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(message, "The triage agent gave its reason only in its diagnosis, on the page linked below.\n\n") {
		t.Fatal(message)
	}
	r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
	for _, boundary := range []string{"", "# Evidence", "- item", "1. item", "```", "> quote", "a | b"} {
		input := "\n# Summary\n\n**First** line\nsecond line\n" + boundary + "\nnot summary"
		if got := firstProseParagraph(input); got != "First line second line" {
			t.Errorf("boundary %q: %q", boundary, got)
		}
		if got := triageAgentReportNotificationMessage("Title\n" + input); got != "First line second line\n\nOpen MMHotline for the whole report." {
			t.Error(got)
		}
	}
	if got := firstProseParagraph(strings.Repeat("word ", 80)); len([]rune(got)) > 300 || strings.HasSuffix(got, "wor") {
		t.Fatal(got)
	}
}

func TestApprovalOnlyIncidentEndsWithRequest(t *testing.T) {
	parallelStartingProcesses(t)
	for _, scenario := range []string{"refused", "claimed", "succeeded", "failed", "restart", "sender alert", "agent report", "another open", "another approved", "another running"} {
		t.Run(scenario, func(t *testing.T) {
			r := newTriageSessionRig(t, true)
			script := "echo done\n"
			if scenario == "failed" {
				script = "exit 1\n"
			}
			prepareApprovals(t, r.a, fakeRunMMX(t.TempDir(), script))
			if _, err := r.a.Store.db.Exec(`UPDATE members SET name='Ana Silva' WHERE sign_in_email_address=?`, r.p.SignInEmailAddress); err != nil {
				t.Fatal(err)
			}
			r.alert(t, "warn", "app002 answers slowly", "")
			r.drive(t)
			run := r.nextRun(t)
			sendTriageEvent(t, run, triageagent.Event{Event: "session", SessionID: "session-one"})
			answers := json.NewDecoder(run.connection)
			requestPlaybookRun(t, run, answers, "0000000000000001")
			record := onlyApprovalRecord(t, r.a)
			if strings.HasPrefix(scenario, "another ") {
				requestPlaybookRun(t, run, answers, "0000000000000002")
				records := approvalRecords(t, r.a)
				state := strings.TrimPrefix(scenario, "another ")
				if _, err := r.a.Store.db.Exec(`UPDATE triage_agent_playbook_runs_waiting_for_a_person SET incident_id=?,state=? WHERE id=?`, record.incidentID, state, records[1].id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "sender alert" || scenario == "agent report" {
				actor := "source:" + r.source.Source.ID
				if scenario == "agent report" {
					actor = "triage_agent"
				}
				if _, err := r.a.Store.db.Exec(`INSERT INTO alerts(id,source_id,sender_alert_id,actor,replay_key,incident_id,sender_incident_key,severity,title,message,notes,received_at) VALUES(?,'','',?,?,?,'','warn','Another problem','Evidence','',?)`, NewID(), actor, NewID(), record.incidentID, r.a.Now().Unix()); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "claimed" {
				incident, err := r.a.GetIncident(context.Background(), r.p, record.incidentID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.a.ClaimIncident(context.Background(), r.p, incident.ID, incident.Revision, ""); err != nil {
					t.Fatal(err)
				}
			}
			r.end(t, run, triageagent.Event{Event: "finished", Answer: "Reported."})
			switch scenario {
			case "succeeded", "failed":
				if err := r.a.ApprovePlaybookRun(context.Background(), r.p, record.id); err != nil {
					t.Fatal(err)
				}
				runApprovedPlaybooks(t, r.a)
			case "restart":
				if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(context.Background(), `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET state='running' WHERE id=?`, record.id); err != nil {
						return err
					}
					return failApprovedPlaybookRunsCutByRestart(context.Background(), tx, r.a.Now())
				}); err != nil {
					t.Fatal(err)
				}
			default:
				if err := r.a.RefusePlaybookRun(context.Background(), r.p, record.id, "Reboot after the backup finishes."); err != nil {
					t.Fatal(err)
				}
			}
			incident, err := r.a.GetIncident(context.Background(), r.p, record.incidentID)
			if err != nil {
				t.Fatal(err)
			}
			resolves := !strings.HasPrefix(scenario, "another ") && scenario != "sender alert" && scenario != "agent report"
			if (incident.Status == "resolved") != resolves {
				t.Fatalf("status %s", incident.Status)
			}
			record = approvalRecords(t, r.a)[0]
			if strings.Contains(record.result, "MMHotline resolved this incident") != resolves {
				t.Fatalf("result: %s", record.result)
			}
			if scenario == "refused" {
				want := "Resolved by MMHotline: this incident only asked for approval of a playbook run, and the request ended (refused by Ana Silva)."
				history := incidentHistory(t, r.a, r.p, record.incidentID)
				found := false
				for _, entry := range history {
					if entry == want {
						found = true
						t.Logf("RESOLUTION HISTORY:\n%s", entry)
					}
				}
				if !found {
					t.Fatalf("history: %q", history)
				}
				if n := countRows(t, r.a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND severity='resolved-sos'`, record.incidentID); n == 0 {
					t.Fatal("no resolved notification")
				}
				t.Logf("RESULT FOR THE AGENT:\n%s", record.result)
				r.drive(t)
				resumed := r.nextRun(t)
				if !strings.Contains(resumed.request.Prompt, record.result) {
					t.Fatal(resumed.request.Prompt)
				}
				r.end(t, resumed, triageagent.Event{Event: "finished", Answer: "Reported."})
				r.advance(triageSessionQuietTime + time.Second)
				r.drive(t)
				if countRows(t, r.a, `SELECT count(*) FROM triage_sessions WHERE ended_at=0`) != 0 {
					t.Fatal("session did not end")
				}
			}
		})
	}
}

func TestTriagePromptsExplainNotificationTextAndResolution(t *testing.T) {
	t.Parallel()
	session := promptText(triageSessionStartPrompt("app001", "", []string{"servers"}, nil, nil, nil))
	review := softwareReviewPrompt("app001", time.Time{}, []string{"servers"}, rigSoftwareReviewTask)
	for name, prompt := range map[string]string{"session": session, "software review": review} {
		for _, required := range []string{"one-line title", "one to three plain sentences about what happened and what the person should do", "Notifications show only the title and opening sentences", "Give the evidence after the opening sentences"} {
			if !strings.Contains(prompt, required) {
				t.Errorf("%s missing %q", name, required)
			}
		}
	}
	for _, required := range []string{"one to three plain sentences on what the run will change, on which servers, and why it is needed now", "a person who must approve the run reads those first", "You cannot resolve incidents.", "People resolve incidents, and MMHotline resolves an approval request's incident when the request ends.", "Never say an incident is resolved unless MMHotline's messages say so."} {
		if !strings.Contains(session, required) {
			t.Errorf("session missing %q", required)
		}
	}
}
