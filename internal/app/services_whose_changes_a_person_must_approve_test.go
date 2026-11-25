package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/triageagent"
)

// The settings file replaces the whole list: a name without a unit type
// gets .service, as systemctl reads it, and show prints the list with who
// changed it and when.
func TestServicesWhoseChangesAPersonMustApproveList(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	a.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	now := time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	result, _ := agentPlaybookRulesCommand(t, a, "show", AgentPlaybookRulesCommandRequest{}, 200)
	if list := result.ServicesWhoseChangesAPersonMustApprove; list == nil || len(list.ServiceNames) != 0 || list.ChangedAt != nil ||
		!strings.Contains(result.Text(), "Services whose changes a person must approve, on every inventory: none; never set.") {
		t.Fatalf("before anyone set it: %+v\n%s", list, result.Text())
	}
	ctx := context.Background()
	if err := a.SetServicesWhoseChangesAPersonMustApprove(ctx, a.settingsPrincipal(), []string{"pgautofailover", "backup.timer", "postgresql@16-main.service"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"pgautofailover.service", "backup.timer", "postgresql@16-main.service"}
	result, raw := agentPlaybookRulesCommand(t, a, "show", AgentPlaybookRulesCommandRequest{}, 200)
	assertCommandKeys(t, raw, "services_whose_changes_a_person_must_approve", "service_names", "changed_at", "changed_by")
	if list := result.ServicesWhoseChangesAPersonMustApprove; list == nil || !slices.Equal(list.ServiceNames, want) || list.ChangedAt == nil || !list.ChangedAt.Equal(now) || list.ChangedBy != "the settings file /etc/mmhotline/mmhotline_settings.yml" {
		t.Fatalf("show: %s", raw)
	}
	if text := result.Text(); !strings.Contains(text, "Services whose changes a person must approve, on every inventory: pgautofailover.service, backup.timer, postgresql@16-main.service; changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-03T09:30:00Z.") {
		t.Fatal(text)
	}

	for _, test := range []struct {
		names []string
		want  string
	}{
		{[]string{"pg autofailover"}, `"pg autofailover" is not a systemd unit name`},
		{[]string{"-pgautofailover"}, "is not a systemd unit name"},
		{[]string{"caddy;reboot"}, "is not a systemd unit name"},
		{[]string{strings.Repeat("a", 248)}, "systemd allows at most 255 characters in a unit name, including the unit type; the unit name with its unit type has 256"},
		{[]string{"pgautofailover", "pgautofailover.service"}, "pgautofailover.service is listed twice; a name without a unit type means a .service, so pgautofailover is pgautofailover.service"},
	} {
		if err := a.SetServicesWhoseChangesAPersonMustApprove(ctx, a.settingsPrincipal(), test.names); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: %v", test.names, err)
		}
	}
	// Someone who is not an administrator is refused by the operation. The
	// settings file's people always include an administrator.
	addMember(t, a, "Administrator", RoleAdministrator)
	member, _ := addMember(t, a, "Member", RoleMember)
	if err := a.SetServicesWhoseChangesAPersonMustApprove(ctx, member, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member's change: %v", err)
	}
	// A refused change changes nothing.
	if list, err := a.ServicesWhoseChangesAPersonMustApprove(ctx, member); err != nil || !slices.Equal(list.ServiceNames, want) {
		t.Fatalf("after the refused changes: %+v %v", list, err)
	}

	now = now.Add(time.Hour)
	if err := a.SetServicesWhoseChangesAPersonMustApprove(ctx, a.settingsPrincipal(), nil); err != nil {
		t.Fatal(err)
	}
	result, _ = agentPlaybookRulesCommand(t, a, "show", AgentPlaybookRulesCommandRequest{}, 200)
	if list := result.ServicesWhoseChangesAPersonMustApprove; list == nil || len(list.ServiceNames) != 0 || !list.ChangedAt.Equal(now) ||
		!strings.Contains(result.Text(), "Services whose changes a person must approve, on every inventory: none; changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-03T10:30:00Z.") {
		t.Fatalf("emptied: %+v\n%s", list, result.Text())
	}
}

// Every member reads the list on the page of rules for running the agent's playbooks, with where it is
// set, so a person sees why a playbook waits.
func TestAgentPlaybookRulesPageShowsServicesWhoseChangesAPersonMustApprove(t *testing.T) {
	t.Parallel()
	h := newWorkflowRig(t)
	admin := h.browser()
	admin.addPerson("admin@example.test", "Administrator")
	if page := admin.get("/agent-playbook-rules", 200); !strings.Contains(page, "Services whose changes a person must approve") || !strings.Contains(page, "No services are listed. The rules below decide") {
		t.Fatalf("the page before anyone set the list: %s", page)
	}
	if err := h.app.SetServicesWhoseChangesAPersonMustApprove(context.Background(), Principal{UnixUser: "fixture"}, []string{"pgautofailover"}); err != nil {
		t.Fatal(err)
	}
	_, member := addMember(t, h.app, "Member", RoleMember)
	reader := h.browser()
	reader.identify(member.SignInEmailAddress)
	page := reader.get("/agent-playbook-rules", 200)
	for _, text := range []string{"pgautofailover.service", "by the mmhotline command, run by fixture", "Where the rules are set"} {
		if !strings.Contains(page, text) {
			t.Fatalf("the member's page lacks %q: %s", text, page)
		}
	}
}

// restartExplained is what mmx explain prints for a fleet stage that
// restarts one service on one server, in the form Start or restart a
// service accepts.
func restartExplained(stage, server, service string) string {
	return `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"` + stage + `","selection":"` + server + `","nodes":["` + server + `"],"node_tasks":[
{"path":"` + stage + `/task_1","node":"` + server + `","action":"systemd_service","module_kind":"core","status":"ready","arguments":[
 {"field":"systemd_service.name","value":"` + service + `","status":"literal"},
 {"field":"systemd_service.restart_now","value":true,"status":"literal"},
 {"field":"systemd_service.run_only_once_marker_file","value":"/var/lib/mmx/commands_run_once/2026-11-03_` + stage + `","status":"literal"},
 {"field":"systemd_service.state","value":"running","status":"literal"}]}]}]}`
}

// A playbook with a node task on a listed service waits for a person even
// when its kind's setting is Act, then report, and says why in the answer,
// the approval alert, the incident's history, the card, and
// approval-requests show. A playbook on another service still runs, and with
// the list empty both run.
func TestServicesWhoseChangesAPersonMustApproveMakeAPlaybookWait(t *testing.T) {
	parallelStartingProcesses(t)
	const (
		restartPostgres = "one_time_playbooks/2026-11-03_restart_postgres_on_db01.yml"
		restartCaddy    = "one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml"
		stopPostgres    = "one_time_playbooks/2026-11-03_stop_postgres_on_db01.yml"
		restartReason   = "restarts pgautofailover.service on db01, which is on the list of services whose changes a person must approve"
	)
	r := newTriageSessionRig(t, true)
	runner := &fakeRunner{calls: make(chan fakeRunnerCall, 8)}
	r.a.triageAgent.playbookRunner = runner
	installFakeMMX(t, r.a, map[string]string{
		// The playbook names the service without .service.
		"2026-11-03_restart_postgres_on_db01": restartExplained("restart_postgres", "db01", "pgautofailover"),
		"2026-11-03_restart_caddy_on_app002":  restartExplained("restart_caddy", "app002", "caddy"),
		// A stop is no kind's form, so it is an unrecognized task.
		"2026-11-03_stop_postgres_on_db01": `{"schema_version":2,"project":"systems","fleet_stages":[{"path":"stop_postgres","selection":"db01","nodes":["db01"],"node_tasks":[
{"path":"stop_postgres/task_1","node":"db01","action":"systemd_service","module_kind":"core","status":"ready","arguments":[
 {"field":"systemd_service.name","value":"pgautofailover.service","status":"literal"},
 {"field":"systemd_service.state","value":"stopped","status":"literal"}]}]}]}`,
	})
	fixture := Principal{UnixUser: "fixture"}
	// Start or restart a service: Act, then report.
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, fixture, "production", "routine_fixes_without_permission"); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SetServicesWhoseChangesAPersonMustApprove(context.Background(), fixture, []string{"pgautofailover"}); err != nil {
		t.Fatal(err)
	}
	commitToCopyAndOrigin(t, r.a, map[string]string{restartPostgres: "fleet_stages: []\n", restartCaddy: "fleet_stages: []\n", stopPostgres: "fleet_stages: []\n"})
	sos := r.alert(t, "sos", "The database on db01 stopped answering", "db01")
	r.drive(t)
	run := r.nextRun(t)
	controls := json.NewDecoder(run.connection)
	requestNumber := 0
	ask := func(playbook string) triageagent.Answer {
		t.Helper()
		requestNumber++
		requestID := strings.Repeat("0", 15) + string(rune('0'+requestNumber))
		sendTriageEvent(t, run, triageagent.Event{Event: "playbook_run_requested", RequestID: requestID, Playbook: playbook, Inventory: "production", Diagnosis: testDiagnosis})
		run.connection.SetReadDeadline(time.Now().Add(20 * time.Second))
		var control triageagent.Control
		if err := controls.Decode(&control); err != nil || control.Answer == nil || control.Answer.RequestID != requestID {
			t.Fatalf("answer: %+v %v", control, err)
		}
		return *control.Answer
	}
	ran := func(playbook string) {
		t.Helper()
		if answer := ask(playbook); answer.Outcome != triageagent.RanAndSucceeded {
			t.Fatalf("%s: %+v", playbook, answer)
		}
		if call := <-runner.calls; call.request.Playbook != playbook {
			t.Fatalf("the runner ran %+v", call)
		}
	}

	answer := ask(restartPostgres)
	noRunnerCall(t, runner)
	if want := restartPostgres + " in the inventory “production” does: Start or restart a service. Task restart_postgres/task_1 " + restartReason + ". " + changeServicesWhoseChangesAPersonMustApproveText + " Because of the list of services whose changes a person must approve, the run waits for a person: Get permission, then act (1 approval)." + approvalAskedText(true); answer.Outcome != triageagent.WaitsForAPerson || !strings.HasSuffix(answer.Message, want) {
		t.Fatalf("the answer to the agent:\n%q\nwant it to end with\n%q", answer.Message, want)
	}
	var message string
	if err := r.a.Store.db.QueryRow(`SELECT message FROM alerts WHERE severity='sos' AND title LIKE 'Approval needed%'`).Scan(&message); err != nil || !strings.Contains(message, "pgautofailover.service is on services_whose_changes_a_person_must_approve") || strings.Contains(message, "restart_postgres/task_1") {
		t.Errorf("the approval alert's message: %q %v", message, err)
	}
	if history := incidentHistory(t, r.a, r.p, sos.IncidentID); !slices.ContainsFunc(history, func(entry string) bool {
		return strings.HasPrefix(entry, "Approval needed to run "+restartPostgres) && strings.Contains(entry, "pgautofailover.service is on services_whose_changes_a_person_must_approve") && !strings.Contains(entry, "restart_postgres/task_1")
	}) {
		t.Errorf("history: %q", history)
	}
	server := httptest.NewTLSServer(r.a.Handler())
	t.Cleanup(server.Close)
	browser := (&workflowRig{t: t, server: server}).browser()
	browser.identify(r.p.SignInEmailAddress)
	// The request's page warns of the step, with why, and how the list changes.
	if text := approvalPageText(browser.get("/approvals/"+onlyApprovalRecord(t, r.a).id, 200)); !phrasesInOrder(text, "Steps on services whose changes a person must approve", "restart_postgres/task_1 (systemd_service) "+restartReason,
		"An administrator changes the list in services_whose_changes_a_person_must_approve", "A step acts on a service whose changes a person must approve.") {
		t.Errorf("the approval request's page: %s", text)
	}
	// The incident page says it in short.
	if panel := liveSectionText(browser.get("/incidents/"+sos.IncidentID, 200), "incident-approvals"); !strings.Contains(panel, "1 task changes a service whose changes a person must approve.") {
		t.Errorf("the incident's approval: %q", panel)
	}
	var result CommandResult
	if err := r.a.approvalRequestsCommand(context.Background(), r.p, "show", ApprovalRequestsCommandRequest{ApprovalRequestID: onlyApprovalRecord(t, r.a).id}, &result); err != nil || result.ApprovalRequest == nil {
		t.Fatalf("approval-requests show: %+v %v", result, err)
	}
	if shown := result.ApprovalRequest; !slices.Equal(shown.TasksOnServicesWhoseChangesAPersonMustApprove, []CommandTaskOnAServiceWhoseChangesAPersonMustApprove{{Path: "restart_postgres/task_1", Node: "db01", Module: "systemd_service", ServiceName: "pgautofailover.service", Reason: restartReason}}) ||
		!strings.Contains(shown.Text(), "Tasks on services whose changes a person must approve:\n  restart_postgres/task_1 (systemd_service) "+restartReason+"\n  "+changeServicesWhoseChangesAPersonMustApproveText+"\n") {
		t.Errorf("approval-requests show: %+v\n%s", shown.TasksOnServicesWhoseChangesAPersonMustApprove, shown.Text())
	}

	ran(restartCaddy)

	// A stop is an unrecognized task, which every_fix_without_permission
	// runs without a person; on a listed service it waits all the same.
	if err := applyAgentPlaybookRulesPreset(context.Background(), r.a, fixture, "production", "every_fix_without_permission"); err != nil {
		t.Fatal(err)
	}
	answer = ask(stopPostgres)
	noRunnerCall(t, runner)
	if answer.Outcome != triageagent.WaitsForAPerson || !strings.Contains(answer.Message, "does: Unrecognized task. The task MMHotline did not recognize, and why: stop_postgres/task_1 (systemd_service): systemd_service.state must be running. Task stop_postgres/task_1 stops pgautofailover.service on db01, which is on the list of services whose changes a person must approve.") {
		t.Fatalf("the stop: %+v", answer)
	}

	// With the list empty, the rules for running the agent's playbooks alone decide.
	if err := r.a.SetServicesWhoseChangesAPersonMustApprove(context.Background(), fixture, nil); err != nil {
		t.Fatal(err)
	}
	ran(restartPostgres)
	ran(restartCaddy)
	ran(stopPostgres)
}

// The rule reads the unit each systemd_service and systemd_unit task names
// as MMX does. While the list has any service, a task whose unit MMX names
// only during the run could act on one, so it needs a person too; with an
// empty list, no task does.
func TestTasksOnServicesWhoseChangesAPersonMustApprove(t *testing.T) {
	t.Parallel()
	recognition, err := actionkinds.Recognize([]byte(`{"schema_version":2,"fleet_stages":[{"path":"fix","node_tasks":[
{"path":"fix/task_1","node":"db01","action":"systemd_service","module_kind":"core","status":"ready","arguments":[{"field":"systemd_service.name","value":"pgautofailover.service","status":"resolved"},{"field":"systemd_service.state","value":"running","status":"literal"}]},
{"path":"fix/task_2","node":"db01","action":"systemd_unit","module_kind":"core","status":"ready","arguments":[{"field":"systemd_unit.name","value":"pgautofailover.service","status":"literal"},{"field":"systemd_unit.drop_in","value":"20-limits.conf","status":"literal"}]},
{"path":"fix/task_3","node":"db02","action":"systemd_service","module_kind":"core","status":"ready","arguments":[{"field":"systemd_service.name","status":"runtime"},{"field":"systemd_service.state","value":"running","status":"literal"}]},
{"path":"fix/task_4","node":"db01","action":"systemd_service","module_kind":"core","status":"skipped","arguments":[{"field":"systemd_service.name","value":"pgautofailover","status":"literal"}]},
{"path":"fix/task_5","node":"db01","action":"systemd_service","module_kind":"project_starlark","status":"ready","arguments":[{"field":"systemd_service.name","value":"pgautofailover","status":"literal"}]},
{"path":"fix/task_6","node":"app002","action":"systemd_service","module_kind":"core","status":"ready","arguments":[{"field":"systemd_service.name","value":"caddy","status":"literal"},{"field":"systemd_service.state","value":"running","status":"literal"}]},
{"path":"fix/task_7","node":"db01","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.command","value":"systemctl restart pgautofailover","status":"literal"}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if tasks := tasksOnServicesWhoseChangesAPersonMustApprove(recognition, nil); len(tasks) != 0 {
		t.Fatalf("with an empty list: %+v", tasks)
	}
	var got []string
	for _, task := range tasksOnServicesWhoseChangesAPersonMustApprove(recognition, []string{"pgautofailover.service"}) {
		got = append(got, task.Path+": "+task.ServiceName+": "+task.Reason)
	}
	want := []string{
		"fix/task_1: pgautofailover.service: starts pgautofailover.service on db01, which is on the list of services whose changes a person must approve",
		"fix/task_2: pgautofailover.service: changes the unit file of pgautofailover.service on db01, which is on the list of services whose changes a person must approve",
		"fix/task_3: : starts a unit on db02 whose name MMX knows only when the task runs, so the unit could be on the list of services whose changes a person must approve",
		// A module of the team's own with the same name: MMHotline cannot
		// tell what it does to the unit.
		"fix/task_5: pgautofailover.service: changes pgautofailover.service on db01, which is on the list of services whose changes a person must approve",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}
