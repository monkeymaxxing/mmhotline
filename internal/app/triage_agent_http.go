package app

import (
	"html/template"
	"net/http"
	"strings"
)

func (a *App) RegisterTriageAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /triage-agent", func(w http.ResponseWriter, r *http.Request) { a.renderTriageAgentPage(w, r, 200, "") })
	mux.HandleFunc("POST /triage-agent", a.changeTriageAgentHTTP)
}
func (a *App) renderTriageAgentPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	p, ok := a.sendingServiceSetupAccess(w, r)
	if !ok {
		return
	}
	state, err := a.TriageAgentState(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	reviewAgent, err := a.ReviewAgentState(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	for _, test := range []*TriageAgentRun{state.LatestSetupTest, reviewAgent.LatestSetupTest, state.LatestSignInCheck, reviewAgent.LatestSignInCheck} {
		if test != nil && test.State == "running" {
			w.Header().Set("Refresh", "5")
		}
	}
	// Presentation keeps the settings path in the page's single folded notice.
	for _, setup := range []*TriageAgentSetup{state.Setup, reviewAgent.Setup} {
		if setup != nil {
			setup.SetUpBy = settingsPageActor(setup.SetUpBy)
			setup.SignInFailureReason = strings.TrimSuffix(strings.TrimSpace(setup.SignInFailureReason), ".")
		}
	}
	for _, run := range []*TriageAgentRun{state.LatestSetupTest, reviewAgent.LatestSetupTest, state.LatestSignInCheck, reviewAgent.LatestSignInCheck} {
		if run != nil {
			run.StartedBy = settingsPageActor(run.StartedBy)
		}
	}
	state.SoftwareReviewIntervalChangedBy = settingsPageActor(state.SoftwareReviewIntervalChangedBy)
	if state.LatestSoftwareReview != nil {
		state.LatestSoftwareReview.StartedBy = settingsPageActor(state.LatestSoftwareReview.StartedBy)
	}
	for i := range state.ListedHosts {
		state.ListedHosts[i].ListedBecause = strings.ReplaceAll(state.ListedHosts[i].ListedBecause, "the settings file "+a.settingsFile.path, "the settings file")
	}
	RenderPage(w, status, "Triage agent", triageAgentPageBody, struct {
		TriageAgentView
		ReviewAgent   ReviewAgentView
		CSRF, Error   string
		SandboxNotice template.HTML
		settingsFileNoticeView
	}{state, reviewAgent, a.CSRFToken(w, r), message, template.HTML(strings.Replace(template.HTMLEscapeString(TriageAgentSandboxNotInstalled), "sudo mmhotline install", "<code>sudo mmhotline install</code>", 1)), a.settingsFileNoticeView()})
}
func (a *App) changeTriageAgentHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.sendingServiceSetupAccess(w, r)
	if !ok {
		return
	}
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	var err error
	switch r.PostForm.Get("action") {
	case "check-sign-in":
		_, err = a.CheckSignInNow(r.Context(), p, triageAgentRole)
	case "review-agent-check-sign-in":
		_, err = a.CheckSignInNow(r.Context(), p, reviewAgentRole)
	default:
		err = ErrInvalid
	}
	if err != nil {
		a.renderTriageAgentPage(w, r, errorStatus(err), publicError(err))
		return
	}
	http.Redirect(w, r, "/triage-agent", http.StatusSeeOther)
}

var triageAgentPageBody = `<div class="settings-page stack">{{if .Error}}<p class="form-error" role="alert">{{.Error}}</p>{{end}}

{{if not .SandboxInstalled}}<section class="panel"><div class="panel-head"><h2>Triage agent not available</h2></div><div class="panel-body"><p class="line-warn">{{.SandboxNotice}}</p><p class="small muted">The <a href="/agent-playbook-rules">rules for running the agent's playbooks</a> decide which fixes need a person.</p></div></section>{{else}}
{{$page := .}}{{with .LatestSetupTest}}{{if eq .Status "failed"}}<p class="form-error" role="alert">The latest triage agent setup test failed. {{if $page.Setup}}The earlier setup remains in use.{{end}} Fix <code>triage_agent</code> in the settings file and apply the settings again. The setup test details below give the reason.</p>{{end}}{{end}}
<section class="panel"><div class="panel-head"><h2>Triage agent setup</h2>{{with .Setup}}{{if .SignInWorks}}<span class="tag ok">Sign-in works</span>{{else}}<span class="tag sos">Sign-in failed</span>{{end}}{{else}}<span class="tag">Not set up</span>{{end}}</div><div class="panel-body">
{{with .Setup}}<p>Triage agent: {{.Title}}.</p><p>Set up by {{.SetUpBy}} at {{clock .SetUpAt}}.</p>
<p{{if not .SignInWorks}} class="form-error" role="alert"{{end}}>Sign-in to {{.CodingAgentTitle}} {{if .SignInWorks}}works.{{else}}fails: {{.SignInFailureReason}}.{{end}} Last checked at {{clock .SignInLastCheckedAt}}.</p>
{{if and .CredentialExpiresAt (eq .CredentialType "access_token")}}<p>The Codex access token expires on {{.CredentialExpiresAt.Format "2006-01-02"}}, the date in <code>triage_agent.credential_expires_on</code>. MMHotline reminds administrators 30 days and 7 days before the token expires. To renew the token, make a new Codex access token on ChatGPT's Access tokens page, put it in <code>triage_agent.credential</code> and its end date in <code>triage_agent.credential_expires_on</code>, and apply the settings file.</p>{{else}}{{with .CredentialExpiresAt}}<p>The subscription token expires at {{clock .}}, a year after the token applied. MMHotline reminds administrators 30 days and 7 days before the token expires. To renew the token, run <code>claude setup-token</code>, put the new token in <code>triage_agent.credential</code>, and apply the settings file.</p>{{end}}{{end}}
<p>MMHotline checks the sign-in once a day with the setup test's prompt, unless a run of the triage agent succeeded in the last 24 hours, and tells administrators when the sign-in fails and when the sign-in works again.</p>
{{with $page.LatestSignInCheck}}{{if eq .State "running"}}<p role="status">Checking the sign-in now: started by {{.StartedBy}} at {{clock .StartedAt}}.</p>{{end}}{{end}}
<form method="post" action="/triage-agent"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="action" value="check-sign-in"><button class="btn">Check the sign-in now</button></form>
{{else}}<p>Triage agent: not set up</p><p>Add <code>triage_agent</code> to the settings file, then run <code>sudo mmhotline settings apply</code>.</p>{{end}}<p class="small muted">The triage agent investigates SOS incidents before MMHotline wakes a person. The <a href="/agent-playbook-rules">rules for running the agent's playbooks</a> decide which fixes need approval.</p>
<details class="fold"><summary>How setup is checked</summary><p>The coding agent, its credential, its model, and its reasoning effort are tested before they apply: MMHotline runs one test prompt with them, and keeps the earlier setup when the test fails. Codex takes an API key, a Codex access token, or a ChatGPT sign-in made just for MMHotline; Claude Code an API key or its subscription token from <code>claude setup-token</code>.</p></details>
</div></section>
{{with .LatestSetupTest}}<details class="more"><summary>Latest setup test: {{.Status}}</summary><div class="body stack"><p>Run {{.ID}}, started by {{.StartedBy}} at {{clock .StartedAt}}.</p>{{if .FailureReason}}<p class="form-error">{{.FailureReason}}</p>{{end}}
<h3>Steps</h3><ol>{{range .Steps}}<li><p>{{clock .OccurredAt}} · {{if eq .Kind "command"}}Command{{else if eq .Kind "message"}}Message{{else if eq .Kind "tool"}}Tool use{{else}}Agent step{{end}}</p><pre>{{.Text}}</pre></li>{{else}}<li>No steps yet.</li>{{end}}</ol>
<h3>Answer</h3><pre>{{.Answer}}</pre><p>{{.UsageText}}</p></div></details>{{end}}
<section class="panel"><div class="panel-head"><h2>Software review</h2></div><div class="panel-body">
<p>Scheduled software review: {{.SoftwareReviewIntervalHours.Text}}.{{if .ScheduledSoftwareReviewIsOff}} The triage agent wakes only for alerts.{{end}}</p><details class="fold"><summary>What the software review does</summary><p>The software review starts the triage agent with the team's task from <code>scheduled_software_review_task.md</code> in the systems repository, such as checking the servers' packages and programs for known flaws. Each finding reaches people as a report at its alert severity. Each review costs one run of the coding agent. To turn the scheduled review on, add the task file and set <code>triage_agent.software_review_interval_hours</code> in the settings file. To start one review now, such as to learn what a review costs, run <code>sudo mmhotline triage-agent run-software-review</code> on the server.</p></details>
{{with .SoftwareReviewIntervalChangedAt}}<p>Set by {{$page.SoftwareReviewIntervalChangedBy}} at {{clock .}}.</p>{{end}}
{{if not .ScheduledSoftwareReviewIsOff}}<p>{{with .NextSoftwareReviewDueAt}}Next scheduled software review due at {{clock .}}.{{else}}Next scheduled software review due: at once.{{end}}</p>{{end}}
{{with .LatestSoftwareReview}}<h3>Latest software review: {{if eq .State "finished"}}Finished{{else if eq .State "running"}}Running{{else if eq .State "failed"}}Failed{{else}}Stopped{{end}}</h3><p>Started by {{.StartedBy}} at {{clock .StartedAt}}{{with .EndedAt}}, ended at {{clock .}}{{end}}.</p><details class="fold"><summary>Software review run identifier</summary><p><code>{{.RunID}}</code></p></details>{{if .FailureReason}}<p{{if eq .State "failed"}} class="form-error"{{end}}>{{.FailureReason}}</p>{{end}}
<p>Reports: {{.ReportCountsByAlertSeverity.SOS}} SOS, {{.ReportCountsByAlertSeverity.Warn}} Warning, {{.ReportCountsByAlertSeverity.Info}} Info.</p><p>{{.UsageText}}</p>{{else}}<p>No software review has run yet.</p>{{end}}</div></section>
<section id="latest-commit-review" class="panel"><div class="panel-head"><h2>Commit review</h2></div><div class="panel-body">
<p>When the settings let the triage agent approve, MMHotline starts a commit review run for each commit no trusted key signed that the triage agent may review. The agent approves an ordinary change, or decides that a person must review the commit; the decisions are on the <a href="/approvals">Approvals</a> page.</p>
{{with .LatestCommitReview}}<h3>Latest commit review: {{if eq .State "finished"}}Finished{{else if eq .State "running"}}Running{{else if eq .State "failed"}}Failed{{else}}Stopped{{end}}</h3><p>Run <code>{{.RunID}}</code>, started by {{.StartedBy}} at {{clock .StartedAt}}{{with .EndedAt}}, ended at {{clock .}}{{end}}.</p>{{if .FailureReason}}<p{{if eq .State "failed"}} class="form-error"{{end}}>{{.FailureReason}}</p>{{end}}<p>{{.UsageText}}</p>{{else}}<p>No commit review has run yet.</p>{{end}}</div></section>
<section class="panel"><div class="panel-head"><h2>Review agent</h2>{{with .ReviewAgent.Setup}}{{if .SignInWorks}}<span class="tag ok">Sign-in works</span>{{else}}<span class="tag sos">Sign-in failed</span>{{end}}{{else}}<span class="tag">Not set up</span>{{end}}</div><div class="panel-body">
{{with .ReviewAgent.LatestSetupTest}}{{if eq .Status "failed"}}<p class="form-error" role="alert">The latest review agent setup test failed. {{if $page.ReviewAgent.Setup}}The earlier setup remains in use.{{end}} Fix <code>review_agent</code> in the settings file and apply the settings again. The setup test details below give the reason.</p>{{end}}{{end}}{{with .ReviewAgent.Setup}}<p>Review agent: {{.Title}}.</p><p>Set up by {{.SetUpBy}} at {{clock .SetUpAt}}.</p>
<p{{if not .SignInWorks}} class="form-error" role="alert"{{end}}>Sign-in to {{.CodingAgentTitle}} {{if .SignInWorks}}works.{{else}}fails: {{.SignInFailureReason}}.{{end}} Last checked at {{clock .SignInLastCheckedAt}}.</p>
{{if and .CredentialExpiresAt (eq .CredentialType "access_token")}}<p>The Codex access token expires on {{.CredentialExpiresAt.Format "2006-01-02"}}, the date in <code>review_agent.credential_expires_on</code>. MMHotline reminds administrators 30 days and 7 days before the token expires. To renew the token, make a new Codex access token on ChatGPT's Access tokens page, put it in <code>review_agent.credential</code> and its end date in <code>review_agent.credential_expires_on</code>, and apply the settings file.</p>{{else}}{{with .CredentialExpiresAt}}<p>The subscription token expires at {{clock .}}, a year after the token applied. MMHotline reminds administrators 30 days and 7 days before the token expires. To renew the token, run <code>claude setup-token</code>, put the new token in <code>review_agent.credential</code>, and apply the settings file.</p>{{end}}{{end}}
<p>MMHotline checks the sign-in once a day with the setup test's prompt, unless a run of the review agent succeeded in the last 24 hours, and tells administrators when the sign-in fails and when the sign-in works again.</p>
{{with $page.ReviewAgent.LatestSignInCheck}}{{if eq .State "running"}}<p role="status">Checking the sign-in now: started by {{.StartedBy}} at {{clock .StartedAt}}.</p>{{end}}{{end}}
<form method="post" action="/triage-agent"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="action" value="review-agent-check-sign-in"><button class="btn">Check the sign-in now</button></form>
{{else}}<p>Review agent: not set up</p><p class="small muted">To add a second coding agent that reviews proposed playbooks, set <code>review_agent</code> in the settings file.</p>{{end}}

<details class="fold"><summary>How playbook review works</summary><p>A review agent is a second coding agent. When a review agent is set up, the review agent must approve each playbook the triage agent asks to run before the rules for running the agent's playbooks apply. When the review agent refuses a playbook, the refusal reason goes back to the triage agent, which may revise the playbook and ask again. After the review agent's third refusal in a row on a problem, a person decides. Each review is a fresh run in the triage agent's sandbox, in a new empty home directory: the review agent sees the playbook and the triage agent's diagnosis, never the logs, and reaches only the review agent's coding agent service.</p>
<p>Any coding agent can review, the triage agent's own included. A coding agent of another vendor than the triage agent's is recommended: its model is less likely to share the triage agent's mistakes, and like every review agent it never sees the logs that could mislead the triage agent.</p>
</details>{{with .ReviewAgent.LatestSetupTest}}<details class="fold"><summary>Latest review agent setup test: {{.Status}}</summary><p>Run {{.ID}}, started by {{.StartedBy}} at {{clock .StartedAt}}.</p>{{if .FailureReason}}<p class="form-error">{{.FailureReason}}</p>{{end}}
<ol>{{range .Steps}}<li><p>{{clock .OccurredAt}} · {{if eq .Kind "command"}}Command{{else if eq .Kind "message"}}Message{{else if eq .Kind "tool"}}Tool use{{else}}Agent step{{end}}</p><pre>{{.Text}}</pre></li>{{else}}<li>No steps yet.</li>{{end}}</ol>
<pre>{{.Answer}}</pre><p>{{.UsageText}}</p></details>{{end}}</div></section>
<details class="more"><summary>Internet access and connections</summary><div class="body stack"><section class="panel"><div class="panel-head"><h2>Internet access</h2></div><div class="panel-body"><p>{{if eq .InternetAccess "any_site"}}Any site, with an instruction to use well-known sources.{{else}}Listed hosts only.{{end}} The triage agent reaches the internet only through MMHotline's logging proxy.</p></div></section>
<section class="panel"><div class="panel-head"><h2>Listed hosts</h2></div><div class="panel-body"><p>At the start of each run in the systems repository, MMHotline lists each server of its inventories at port 22, so the triage agent can read the servers over SSH. It also lists the host of each vendor page in <code>software_outside_the_distribution.yml</code>, each program's <code>releases_page</code> and <code>security_advisories_page</code>, so the software review can read them.{{with .HostsFromTheSystemsRepositoryListedAt}} The servers and the vendor pages' hosts below were listed at the start of the latest run, {{clock .}}; a server or a page removed since stays listed until the next run.{{end}}</p>{{if .InventoryServersNotListedBecause}}<p class="form-error">At the start of the latest run, MMHotline could not list every inventory server: {{.InventoryServersNotListedBecause}}</p>{{end}}{{if .VendorPageHostsNotListedBecause}}<p class="form-error">At the start of the latest run, MMHotline could not list the host of every vendor page in <code>software_outside_the_distribution.yml</code>: {{.VendorPageHostsNotListedBecause}}</p>{{end}}<div class="scroll"><table class="table"><thead><tr><th>Host</th><th>Port</th><th>Listed because</th></tr></thead><tbody>{{range .ListedHosts}}<tr><td>{{.Host}}</td><td>{{.Port}}</td><td>{{.ListedBecause}}</td></tr>{{end}}</tbody></table></div>
</div></section>
<section class="panel"><div class="panel-head"><h2>Recent connections</h2></div><div class="panel-body"><div class="scroll"><table class="table"><thead><tr><th>Time</th><th>Host and port</th><th>Decision</th><th>Bytes sent</th><th>Bytes received</th><th>Action</th></tr></thead><tbody>{{range .RecentConnections}}<tr><td>{{clock .StartedAt}}</td><td>{{.Host}}:{{.Port}}</td><td>{{if eq .Decision "allowed"}}Allowed{{else}}Refused: {{.RefusalReason}}{{end}}</td><td>{{.BytesSent}}</td><td>{{.BytesReceived}}</td><td>{{if .CanAddListedHost}}To allow the connection, add <code>{host: {{.Host}}, port: {{.Port}}}</code> to <code>triage_agent.listed_hosts</code> in the settings file.{{end}}</td></tr>{{else}}<tr><td colspan="6">No connections yet.</td></tr>{{end}}</tbody></table></div></div></section>
</div></details>{{end}}<details class="more"><summary>Where the triage agent is set up</summary><div class="body">` + settingsFileNotice("<code>triage_agent</code> and <code>review_agent</code>") + `</div></details></div>`
