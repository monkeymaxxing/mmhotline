package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// The read-only commands tell a person or a coding agent on the server what
// happened, with the facts of the pages: a warn alert with no incident, a
// triage session with two runs and their use, the incident's history with
// the agent's report, and an approval request with what it would run. They
// change nothing in the database.
func TestReadOnlyCommandsShowWhatHappened(t *testing.T) {
	r := newTriageSessionRig(t, true)
	r.a.WebURL = "https://ops.example.com"
	ctx := context.Background()
	prepareApprovals(t, r.a, "")
	start := r.a.Now().UTC()

	// A warn alert starts the session; its run takes a step, an SOS joins
	// the session meanwhile, and the run finishes.
	warn := r.alert(t, "warn", "Disk of app002 is 97% full", "")
	r.drive(t)
	first := r.nextRun(t)
	sendTriageEvent(t, first, triageagent.Event{Event: "session", SessionID: "session-one"})
	sendTriageEvent(t, first, triageagent.Event{Event: "step", Step: &triageagent.Step{Kind: "command", Text: "ssh app002 df -h /"}})
	r.advance(time.Minute)
	sos := r.alert(t, "sos", "app002 stopped answering", "app002")
	r.drive(t)
	r.end(t, first, triageagent.Event{Event: "finished", Answer: "Reported.", CodingAgentSessionTotalUsage: &triageagent.Usage{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 20}})

	// The second run carries the SOS, reports, and asks to reboot app002,
	// which waits for a person's approval.
	r.drive(t)
	second := r.nextRun(t)
	sendTriageEvent(t, second, triageagent.Event{Event: "session", SessionID: "session-one"})
	answers := json.NewDecoder(second.connection)
	report(t, second, answers, "0000000000000001", "warn", "The disk of app002 is full of old logs.")
	requestPlaybookRun(t, second, answers, "0000000000000002")
	r.end(t, second, triageagent.Event{Event: "finished", Answer: "Reported.", CodingAgentSessionTotalUsage: &triageagent.Usage{InputTokens: 2500, CachedInputTokens: 1400, OutputTokens: 50}})

	// Another SOS, resolved by a person, which the session never saw.
	r.advance(time.Minute)
	backup := r.alert(t, "sos", "Backup failed on db001", "db001")
	resolved, err := r.a.GetIncident(ctx, r.p, backup.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.a.ResolveIncident(ctx, r.p, resolved.ID, resolved.Revision, ""); err != nil {
		t.Fatal(err)
	}

	// The database has one connection, so its change count covers every
	// write the commands could make.
	changes := func(t *testing.T) int64 {
		t.Helper()
		var count int64
		if err := r.a.Store.db.QueryRow(`SELECT total_changes()`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	changesBefore := changes(t)
	unchanged := func(t *testing.T) {
		t.Helper()
		if after := changes(t); after != changesBefore {
			t.Fatalf("the read-only commands changed %d rows", after-changesBefore)
		}
	}
	run := func(group, action string, request CommandRequest) (CommandResult, string) {
		t.Helper()
		request.Group, request.Action = group, action
		return commandRequest(t, r.a, request, 200)
	}

	t.Run("a warn alert with no incident", func(t *testing.T) {
		result, raw := run("alerts", "list", CommandRequest{Alerts: AlertsCommandRequest{Severity: "warn", AlertSourceID: r.source.Source.ID}})
		assertCommandKeys(t, raw, "alerts", "alert_id", "severity", "title", "message", "received_at", "alert_source_id", "page_url", "message", "web_url")
		if len(result.Alerts) != 1 || result.Alerts[0].AlertID != warn.ID || result.Alerts[0].IncidentID != "" || strings.Contains(raw, `"incident_id"`) || result.Alerts[0].PageURL != "https://ops.example.com/alerts/"+warn.ID {
			t.Fatalf("warn alerts of the source: %s", raw)
		}
		result, _ = run("alerts", "list", CommandRequest{Alerts: AlertsCommandRequest{AlertSourceID: r.source.Source.ID}})
		if len(result.Alerts) != 3 || result.Alerts[0].AlertID != backup.ID || result.Alerts[2].AlertID != warn.ID || result.Alerts[1].IncidentID != sos.IncidentID {
			t.Fatalf("the source's alerts, newest first: %+v", result.Alerts)
		}
		result, _ = run("alerts", "list", CommandRequest{Alerts: AlertsCommandRequest{AlertSourceID: r.source.Source.ID, OlderThanAlertID: sos.ID}})
		if len(result.Alerts) != 1 || result.Alerts[0].AlertID != warn.ID {
			t.Fatalf("alerts older than the SOS: %+v", result.Alerts)
		}
		result, _ = run("alerts", "list", CommandRequest{Alerts: AlertsCommandRequest{AlertSourceID: r.source.Source.ID, ReceivedSince: start.Add(time.Minute), ReceivedBefore: start.Add(2 * time.Minute)}})
		if len(result.Alerts) != 1 || result.Alerts[0].AlertID != sos.ID {
			t.Fatalf("alerts received in the second minute: %+v", result.Alerts)
		}
		if text := result.Text(); !strings.Contains(text, "SOS "+sos.ID+": app002 stopped answering (SOS incident "+sos.IncidentID+")") || !strings.Contains(text, "Details of app002 stopped answering") {
			t.Fatalf("text:\n%s", text)
		}
		for _, refusal := range []struct {
			request AlertsCommandRequest
			status  int
			message string
		}{
			{AlertsCommandRequest{AlertSourceID: "0123456789abcdef0123456789abcdef"}, 404, "no alert source has ID 0123456789abcdef0123456789abcdef"},
			{AlertsCommandRequest{OlderThanAlertID: "missing"}, 404, "no alert has ID missing"},
			{AlertsCommandRequest{IncidentID: "0123456789abcdef0123456789abcdef"}, 404, "no SOS incident has ID 0123456789abcdef0123456789abcdef"},
			{AlertsCommandRequest{Severity: "urgent"}, 400, "severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos"},
		} {
			refused, _ := commandRequest(t, r.a, CommandRequest{Group: "alerts", Action: "list", Alerts: refusal.request}, refusal.status)
			if refused.Message != refusal.message {
				t.Errorf("%+v: %q", refusal.request, refused.Message)
			}
		}
	})

	t.Run("incidents not resolved, and resolved ones over a span", func(t *testing.T) {
		result, raw := run("incidents", "list", CommandRequest{})
		assertCommandKeys(t, raw, "incidents", "incident_id", "title", "status", "opened_at", "escalation_step", "held_for_triage_agent", "being_contacted_now", "opening_alert_id", "alert_source_id", "page_url")
		if len(result.Incidents) != 1 || result.Incidents[0].IncidentID != sos.IncidentID || result.Incidents[0].Status != "open" || result.Incidents[0].BeingContactedNow == "" {
			t.Fatalf("incidents not resolved: %s", raw)
		}
		result, _ = run("incidents", "list", CommandRequest{Incidents: IncidentsCommandRequest{OpenedSince: start.Add(2 * time.Minute)}})
		if len(result.Incidents) != 1 || result.Incidents[0].IncidentID != resolved.ID || result.Incidents[0].Status != "resolved" || result.Incidents[0].ResolvedAt.IsZero() || result.Incidents[0].BeingContactedNow != "" {
			t.Fatalf("incidents opened since the third minute: %+v", result.Incidents)
		}
		if !strings.Contains(result.Message, "resolved or not") {
			t.Fatal(result.Message)
		}
		result, _ = run("incidents", "list", CommandRequest{Incidents: IncidentsCommandRequest{OpenedSince: start, OpenedBefore: start.Add(3 * time.Minute)}})
		if len(result.Incidents) != 2 || result.Incidents[0].IncidentID != resolved.ID || result.Incidents[1].IncidentID != sos.IncidentID {
			t.Fatalf("incidents opened in the span, newest first: %+v", result.Incidents)
		}
	})

	var sessionID string
	t.Run("one incident with its alerts and history", func(t *testing.T) {
		result, raw := run("incidents", "show", CommandRequest{Incidents: IncidentsCommandRequest{IncidentID: sos.IncidentID}})
		assertCommandKeys(t, raw, "incident", "alerts", "history", "kind", "label", "by", "occurred_at", "triage_session_id")
		// Its alerts, newest first: the approval request, the agent's
		// report, and the SOS that opened it.
		incident := result.Incident
		if incident == nil || incident.IncidentID != sos.IncidentID || incident.TriageSessionID == "" || len(incident.Alerts) != 3 || !strings.HasPrefix(incident.Alerts[0].Title, "Approval needed to run "+rebootPlaybook) || incident.Alerts[1].Message != "The disk of app002 is full of old logs." || incident.Alerts[2].AlertID != sos.ID {
			t.Fatalf("incident: %s", raw)
		}
		sessionID = incident.TriageSessionID
		var reported, asked bool
		for _, entry := range incident.History {
			reported = reported || entry.Kind == "triage_agent_report" && entry.Label == "Triage agent report" && strings.Contains(entry.Message, "The disk of app002 is full of old logs.")
			asked = asked || entry.Label == "Approval needed"
		}
		if !reported || !asked || incident.History[0].Kind != "opened" {
			t.Fatalf("history: %+v", incident.History)
		}
		if text := result.Text(); !strings.Contains(text, "Triage agent report: ") || !strings.Contains(text, "Triage session: "+sessionID) {
			t.Fatalf("text:\n%s", text)
		}
		refused, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "show", Incidents: IncidentsCommandRequest{IncidentID: "missing"}}, 404)
		if refused.Message != "no SOS incident has ID missing" {
			t.Fatal(refused.Message)
		}
	})

	t.Run("a triage session with two runs", func(t *testing.T) {
		result, raw := run("triage-sessions", "list", CommandRequest{})
		assertCommandKeys(t, raw, "triage_sessions", "triage_session_id", "started_at", "phase", "phase_text", "alert_count", "run_count", "incident_ids")
		if len(result.TriageSessions) != 1 || result.TriageSessions[0].TriageSessionID != sessionID || result.TriageSessions[0].Phase != "waiting" || result.TriageSessions[0].AlertCount != 2 || result.TriageSessions[0].RunCount != 2 || strings.Join(result.TriageSessions[0].IncidentIDs, ",") != sos.IncidentID || result.TriageSessions[0].Runs != nil {
			t.Fatalf("sessions: %s", raw)
		}
		result, raw = run("triage-sessions", "show", CommandRequest{TriageSessions: TriageSessionsCommandRequest{TriageSessionID: sessionID}})
		assertCommandKeys(t, raw, "triage_session", "runs", "purpose", "state", "ended_at", "input_tokens", "cached_input_tokens", "output_tokens", "steps", "earlier_steps_not_shown", "joined_at", "sent_in_run_id", "incidents")
		session := result.TriageSession
		if session == nil || len(session.Runs) != 2 || len(session.Alerts) != 2 || len(session.Incidents) != 1 || session.Incidents[0].IncidentID != sos.IncidentID {
			t.Fatalf("session: %s", raw)
		}
		// Each run's own use: the second run's is its total less the first's.
		for index, want := range [][3]int64{{1000, 400, 20}, {1500, 1000, 30}} {
			run := session.Runs[index]
			if run.Purpose != "triage_session" || run.State != "finished" || run.EndedAt == nil || [3]int64{run.InputTokens, run.CachedInputTokens, run.OutputTokens} != want {
				t.Errorf("run %d: %+v", index+1, run.TriageAgentRun)
			}
		}
		if len(session.Runs[0].Steps) != 1 || session.Runs[0].Steps[0].Text != "ssh app002 df -h /" || session.Runs[0].EarlierStepsNotShown != 0 {
			t.Fatalf("first run's steps: %+v", session.Runs[0].Steps)
		}
		if session.Alerts[0].AlertID != warn.ID || session.Alerts[0].SentInRunID != session.Runs[0].ID || session.Alerts[1].AlertID != sos.ID || session.Alerts[1].SentInRunID != session.Runs[1].ID {
			t.Fatalf("alerts and the runs that carried them: %+v", session.Alerts)
		}
		if text := result.Text(); !strings.Contains(text, "command: ssh app002 df -h /") || !strings.Contains(text, "Input tokens: 1500, of which 1000 cached; output tokens: 30.") {
			t.Fatalf("text:\n%s", text)
		}
		refused, _ := commandRequest(t, r.a, CommandRequest{Group: "triage-sessions", Action: "show", TriageSessions: TriageSessionsCommandRequest{TriageSessionID: "missing"}}, 404)
		if refused.Message != "no triage session has ID missing" {
			t.Fatal(refused.Message)
		}
	})

	var approvalRequestID string
	t.Run("an approval request that waits, with what it would run", func(t *testing.T) {
		result, raw := run("approval-requests", "list", CommandRequest{})
		assertCommandKeys(t, raw, "approval_requests", "approval_request_id", "playbook", "inventory", "commit_id", "rule", "approvals_needed", "state", "state_label", "asked_at", "kinds_of_action", "tasks", "node", "module", "arguments", "playbook_text", "decisions", "result_for_the_agent")
		if len(result.ApprovalRequests) != 1 {
			t.Fatalf("approval requests: %s", raw)
		}
		request := result.ApprovalRequests[0]
		approvalRequestID = request.ApprovalRequestID
		if request.Playbook != rebootPlaybook || request.Inventory != "production" || request.Rule != getPermissionThenAct || request.ApprovalsNeeded != 1 || request.State != "open" || request.IncidentID != sos.IncidentID || request.TriageSessionID != sessionID || request.PlaybookText != rebootPlaybookText {
			t.Fatalf("approval request: %+v", request)
		}
		if strings.Join(request.KindsOfAction, ",") != "reboot_a_server" || len(request.Tasks) != 1 || request.Tasks[0].Node != "app002" || request.Tasks[0].Module != "reboot" {
			t.Fatalf("what it would run: %+v %+v", request.KindsOfAction, request.Tasks)
		}
		unchanged(t)
		if err := r.a.ApprovePlaybookRun(ctx, r.p, approvalRequestID); err != nil {
			t.Fatal(err)
		}
		if changes(t) == changesBefore {
			t.Fatal("the change count does not see an operation's writes")
		}
		changesBefore = changes(t)
		result, raw = run("approval-requests", "show", CommandRequest{ApprovalRequests: ApprovalRequestsCommandRequest{ApprovalRequestID: approvalRequestID}})
		assertCommandKeys(t, raw, "approval_request", "member_name", "decision", "decided_at")
		shown := result.ApprovalRequest
		if shown == nil || shown.State != "approved" || len(shown.Decisions) != 1 || shown.Decisions[0].Decision != "approve" || shown.Decisions[0].MemberName == "" || shown.Decisions[0].DecidedAt.IsZero() {
			t.Fatalf("approved request: %s", raw)
		}
		if text := result.Text(); !strings.Contains(text, "Kinds of action in the playbook: Reboot a server.") || !strings.Contains(text, "on app002: reboot") || !strings.Contains(text, "\n  # Reboot app002, whose kernel hangs.\n") || !strings.Contains(text, " approved the playbook run at ") {
			t.Fatalf("text:\n%s", text)
		}
		if result, _ = run("approval-requests", "list", CommandRequest{}); len(result.ApprovalRequests) != 0 || !strings.Contains(result.Message, "Listed 0 ") {
			t.Fatalf("an approved request no longer waits: %+v", result)
		}
		refused, _ := commandRequest(t, r.a, CommandRequest{Group: "approval-requests", Action: "show", ApprovalRequests: ApprovalRequestsCommandRequest{ApprovalRequestID: "missing"}}, 404)
		if refused.Message != "no approval request has ID missing" {
			t.Fatal(refused.Message)
		}
	})

	unchanged(t)
}
