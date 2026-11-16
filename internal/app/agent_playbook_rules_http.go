package app

import (
	"net/http"

	"mmhotline/internal/actionkinds"
)

func (a *App) RegisterAgentPlaybookRulesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /agent-playbook-rules", a.renderAgentPlaybookRulesPage)
}

// renderAgentPlaybookRulesPage shows every member the rules for running the agent's playbooks, which the
// settings file sets.
func (a *App) renderAgentPlaybookRulesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	inventories, err := a.AgentPlaybookRules(r.Context(), p, "")
	if err != nil {
		HTTPError(w, err)
		return
	}
	services, err := a.ServicesWhoseChangesAPersonMustApprove(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	for i := range inventories {
		for j := range inventories[i].ActionKinds {
			inventories[i].ActionKinds[j].ChangedBy = settingsPageActor(inventories[i].ActionKinds[j].ChangedBy)
		}
		for j := range inventories[i].ListedPlaybooks {
			inventories[i].ListedPlaybooks[j].ChangedBy = settingsPageActor(inventories[i].ListedPlaybooks[j].ChangedBy)
		}
	}
	services.ChangedBy = settingsPageActor(services.ChangedBy)
	RenderPage(w, 200, "Rules for running the agent's playbooks", agentPlaybookRulesPageBody, struct {
		Inventories                            []InventoryAgentPlaybookRules
		ServicesWhoseChangesAPersonMustApprove ServicesWhoseChangesAPersonMustApprove
		// What each kind accepts, as the triage agent reads it in its task.
		AcceptedTasksRules string
		AcceptedTasks      []actionkinds.AcceptedTasks
		settingsFileNoticeView
	}{inventories, services, actionkinds.AcceptedTasksRules, actionkinds.AcceptedTasksOfEachKind(), a.settingsFileNoticeView()})
}

var agentPlaybookRulesPageBody = `<div class="settings-page stack"><p class="muted">The rules for running the agent's playbooks decide which fixes the triage agent may run without a person's approval.</p>
<section class="panel" id="services-whose-changes-a-person-must-approve"><div class="panel-head"><h2>Services whose changes a person must approve</h2></div><div class="panel-body">
{{with .ServicesWhoseChangesAPersonMustApprove}}{{if .ServiceNames}}<p>On every inventory, a playbook with a task that starts, stops, restarts, or changes a listed systemd service waits for a person, whatever the rules of its kinds of action below, unless the playbook is listed by name below:</p><ul class="bullets">{{range .ServiceNames}}<li><code>{{.}}</code></li>{{end}}</ul><p class="small muted">MMHotline cannot check a service against the list when the service name is known only when the playbook runs. A person must approve the playbook.</p>{{else}}<p>No services are listed. The rules below decide whether a playbook waits for a person.</p>{{end}}
<p class="faint small">{{if .ChangedAt}}Changed {{clock .ChangedAt}} by {{.ChangedBy}}.{{else}}Never set.{{end}}</p>{{end}}</div></section>
{{range .Inventories}}<section class="panel"><div class="panel-head"><h2>The inventory “{{.Inventory}}”</h2></div>
{{if not .InventoryFileExists}}<div class="panel-body"><p class="line-warn small">MMHotline's copy of the systems repository has no inventories/{{.Inventory}}.yml.</p></div>{{end}}
<ul class="settings-rules">{{range .ActionKinds}}<li><strong>{{.ActionKindTitle}}</strong><span>{{.RuleTitle}}</span><span class="faint small">{{if .ChangedAt}}Changed {{clock .ChangedAt}} by {{.ChangedBy}}{{else}}No rule saved. A person must approve {{lower .ActionKindTitle}} tasks until the settings file lists a rule.{{end}}</span></li>{{end}}</ul>
{{with .ListedPlaybooks}}<div class="panel-body"><h3>Playbooks listed by name</h3><p class="small muted">Each listed playbook takes its own rule instead of the rules of its kinds of action, even when a step acts on a service whose changes a person must approve.</p></div><ul class="settings-rules">{{range .}}<li><strong><code>playbooks/{{.PlaybookName}}.yml</code></strong><span>{{.RuleTitle}}</span>{{with .ItsApprovalAlsoApprovesInTheSameSession}}<span class="small">Its approval also approves, once each later in the same triage session: {{range $index, $name := .}}{{if $index}}, {{end}}<code>{{$name}}</code>{{end}}</span>{{end}}<span class="faint small">Changed {{clock .ChangedAt}} by {{.ChangedBy}}</span></li>{{end}}</ul>{{end}}</section>{{else}}<section class="panel"><div class="panel-body"><p>MMHotline's copy of the systems repository has no inventories yet, and no rules for running the agent's playbooks are saved.</p></div></section>{{end}}
<details class="more" id="kinds-of-action"><summary>What MMHotline counts as each kind of action</summary><div class="body stack"><p class="small muted">Each playbook uses the strictest rule among the playbook's kinds of action.</p><p class="result small">{{.AcceptedTasksRules}}</p>{{range .AcceptedTasks}}<div class="stack"><h3>{{.Kind.Title}}</h3><ul class="bullets">{{range .Modules}}<li><code>{{.Module}}</code>. Arguments: {{.ArgumentsText}}.{{with .Conditions}} Conditions: {{range $index, $condition := .}}{{if $index}}; {{end}}{{$condition}}{{end}}.{{end}}{{range .Examples}}<pre class="settings-code" tabindex="0">{{.}}</pre>{{end}}</li>{{end}}</ul></div>{{end}}</div></details>
<details class="more"><summary>Where the rules are set</summary><div class="body">` + settingsFileNotice("<code>rules_for_running_agent_playbooks</code> and <code>services_whose_changes_a_person_must_approve</code>") + `</div></details></div>`
