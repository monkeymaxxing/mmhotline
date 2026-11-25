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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "Playbook rules", frame, nil, agentPlaybookRulesPageBody(frame, agentPlaybookRulesPageData{
		Inventories:                            inventories,
		ServicesWhoseChangesAPersonMustApprove: services,
		AcceptedTasksRules:                     actionkinds.AcceptedTasksRules,
		AcceptedTasks:                          actionkinds.AcceptedTasksOfEachKind(),
		settingsFileNoticeView:                 a.settingsFileNoticeView(),
	}))
}

// agentPlaybookRulesPageData is what the rules page shows (agent_playbook_rules.templ).
type agentPlaybookRulesPageData struct {
	Inventories                            []InventoryAgentPlaybookRules
	ServicesWhoseChangesAPersonMustApprove ServicesWhoseChangesAPersonMustApprove
	// What each kind accepts, as the triage agent reads it in its task.
	AcceptedTasksRules string
	AcceptedTasks      []actionkinds.AcceptedTasks
	settingsFileNoticeView
}
