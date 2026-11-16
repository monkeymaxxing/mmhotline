package app

import "net/http"

func (a *App) renderCommitReviewPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	review, err := a.CommitReview(r.Context(), p, r.PathValue("commit_id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	RenderPage(w, status, review.Question(), commitReviewPageBody, struct {
		Review      CommitReview
		CSRF, Error string
	}{review, a.CSRFToken(w, r), message})
}

func (a *App) reviewCommitHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	if err := a.ReviewCommit(r.Context(), p, r.PathValue("commit_id")); err != nil {
		a.renderCommitReviewPage(w, r, errorStatus(err), publicError(err))
		return
	}
	http.Redirect(w, r, "/approvals/commits/"+r.PathValue("commit_id"), http.StatusSeeOther)
}

const commitReviewsListBody = `<section id="commit-reviews" data-live {{if .Commits}}class="panel warn"{{else}}hidden{{end}}>{{with .Commits}}<div class="panel-head"><h2>Commits waiting for review</h2><span class="tag warn">{{len .}} waiting</span></div>` +
	`<ul class="items">{{range .}}<li><div class="what"><a class="title" href="{{.URL}}">{{.Question}}</a><span class="faint small">Found at {{clock .FoundAt}}, {{relativeTime .FoundAt}}</span></div><div class="do"><a class="btn primary" href="{{.URL}}">Review</a></div></li>{{end}}</ul><p class="panel-foot">Read each commit's changes before allowing MMX runs past the commit. Any team member may review a commit.</p>{{end}}</section>`

// commitsTheTriageAgentApprovedBody lists the commits the triage agent marked
// reviewed, newest first, each with its report's title, so a person can
// read what the agent let through.
const commitsTheTriageAgentApprovedBody = `<section id="commits-the-triage-agent-approved" data-live class="panel quiet"><div class="panel-head"><h2>Commits the triage agent approved</h2></div>` +
	`{{with .ApprovedByTheTriageAgent}}<ul class="items">{{range .}}<li><div class="what"><a href="{{.URL}}">Commit {{.ShortID}}{{with .TriageAgentReview}}{{with .ReportTitle}}: {{.}}{{end}}{{end}}</a><span class="faint small">Marked reviewed by the triage agent at {{clock .ReviewedAt}}</span></div></li>{{end}}</ul>` +
	`{{else}}<div class="panel-body"><p class="faint">The triage agent has approved no commit.</p></div>{{end}}</section>`

// A commit no trusted key signed has only its diff to review, so the diff
// starts open; the triage agent's commit lists its paths that need review.
const commitReviewPageBody = `{{define "heading"}}{{with .Data.Review}}<div class="approval-head"><p class="crumbs"><a href="/dashboard">Dashboard</a> / <a href="/approvals">Approvals</a> / Commit {{.ShortID}}</p><h1>{{.Question}}</h1></div>{{end}}{{end}}` +
	`{{with .Error}}<p class="form-error" role="alert">{{.}}</p>{{end}}{{with .Review}}` +
	`<section id="commit-review-decision" data-live class="{{if .Reviewed}}panel quiet{{else}}decide warn approval{{end}}">{{if .Reviewed}}<div class="panel-body"><p>{{if eq .ReviewerKind "triage_agent"}}Marked reviewed by the triage agent at {{clock .ReviewedAt}}.{{else}}Commit {{.ShortID}} was reviewed by {{.ReviewedBy}} at {{clock .ReviewedAt}}.{{end}}</p><p class="muted">MMHotline can run MMX past the reviewed commit on the next playbook run request, and the triage agent's next run starts past it.</p></div>{{else}}<div class="say"><h2>MMX runs wait for a person's review</h2>` +
	`{{if .NotSignedByATrustedKey}}<p>No key the team trusts signed commit {{.ShortID}}: the commit is unsigned, or a key the settings file does not list signed it. Until a person reviews the commit, the triage agent works from the commit before it.</p><p>If you or a teammate made the commit, for example with the merge button of your Git host's web page, read its changes in the diff below, then mark it reviewed.</p><p><strong>A commit nobody on the team made is a possible break-in.</strong> Then do not mark it reviewed: follow your team's break-in procedure.</p>` +
	`{{else}}<p>Commit {{.ShortID}}, signed by the triage agent's key, changes paths the triage agent may not change without a person's review.</p><p>MMHotline then allows MMX runs past commit {{.ShortID}}. If a commit change is wrong, revert the wrong change with your own commit first.</p>{{end}}` +
	`<p class="small muted">Review starts no playbook run. The triage agent must request the playbook run again.</p></div><form class="do" method="post" action="{{.URL}}/review"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="btn primary big block">Mark commit reviewed</button></form>{{end}}</section>` +
	`{{with .TriageAgentReview}}<section id="triage-agent-commit-review" class="panel"><div class="panel-head"><h2>The triage agent's review</h2></div><div class="panel-body stack"><p>{{if .NeedsAPerson}}The triage agent decided that a person must review the commit, and sent its report at alert severity {{.AlertSeverity}}{{else}}The triage agent approved the commit{{end}} at {{clock .DecidedAt}}, in the commit review run <code>{{.RunID}}</code> (<a href="/triage-agent#latest-commit-review">latest commit review</a>).</p><pre>{{.Report}}</pre></div></section>{{end}}` +
	`<section class="panel"><div class="panel-head"><h2>Commit facts</h2></div><div class="panel-body stack"><dl class="kv"><dt>Full commit ID</dt><dd><code>{{.ID}}</code></dd><dt>Author</dt><dd>{{.Author}}</dd><dt>Commit date</dt><dd>{{clock .CommittedAt}}</dd><dt>Found by MMHotline</dt><dd>{{clock .FoundAt}}</dd></dl><div><h3>Commit message</h3><p class="result">{{.Message}}</p></div></div></section>` +
	`{{if .Paths}}<section class="panel{{if .Reviewed}} quiet{{else}} warn{{end}}"><div class="panel-head"><h2>Paths that need review</h2></div><div class="panel-body"><ul>{{range .Paths}}<li><code>{{.Path}}</code> {{.Reason}}.</li>{{end}}</ul></div></section>{{end}}` +
	`<details class="more"{{if .NotSignedByATrustedKey}} open{{end}}><summary>Commit diff</summary><div class="body">{{if .DiffTruncated}}<p class="small muted">MMHotline shows only the first 128 KiB of the commit diff to keep the review page readable. Read the full diff in your systems repository before reviewing the commit.</p>{{end}}<pre>{{.Diff}}</pre></div></details>{{end}}`
