package app

import (
	"context"
	"net/http"
	"strings"
)

func (a *App) RegisterSendingServiceSetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sending-services", a.sendingServicesPage)
	mux.HandleFunc("GET /sending-services/{sending_service}", a.sendingServiceSetupPage)
}

// sendingServiceSetupView is a sending service's guided page: what to
// create at the service, and the settings file key each value goes in.
type sendingServiceSetupView struct {
	sendingServicePageView
	Fields   []sendingServiceFieldView
	Guide    []SendingServiceSetupStep
	NextStep string
	settingsFileNoticeView
}

type sendingServiceFieldView struct {
	SendingServiceSetupField
	KeyParts []string
}

// SectionExample is the service's section of the settings file, with a
// placeholder for each value, as the guided page shows it.
func (view sendingServiceSetupView) SectionExample() string {
	lines := []string{"sending_services:", "  " + view.SendingService + ":"}
	for _, field := range view.Fields {
		value := "<" + field.Label + ">"
		if field.Name == "extra_phone_numbers_on_the_contact_card" {
			value = "[]"
		}
		lines = append(lines, "    "+field.SettingsKey+": "+value)
	}
	return strings.Join(lines, "\n")
}

// Page views keep settings paths out of ordinary text and show a failed
// replacement separately from the saved setup that still serves notifications.
type sendingServicePageView struct {
	SendingServiceSetup
	Failure SettingsFilePartOutcome
}

func settingsPageActor(actor string) string {
	if strings.HasPrefix(actor, "the settings file ") {
		return "the settings file"
	}
	return actor
}

func (a *App) sendingServicePageViews(ctx context.Context, p Principal) ([]sendingServicePageView, error) {
	setups, err := a.SendingServiceSetups(ctx, p)
	if err != nil {
		return nil, err
	}
	settings, err := a.SettingsFileState(ctx)
	if err != nil {
		return nil, err
	}
	var views []sendingServicePageView
	for _, setup := range setups {
		setup.SetUpBy = settingsPageActor(setup.SetUpBy)
		view := sendingServicePageView{SendingServiceSetup: setup}
		for _, part := range settings.Parts {
			if part.Part == "sending_services."+setup.SendingService && part.FailureReason != "" {
				view.Failure = part
			}
		}
		views = append(views, view)
	}
	return views, nil
}

const sendingServiceState = `{{if .Failure.FailureReason}}<p class="form-error" role="alert">The latest settings check failed{{with .Failure.FailedAt}} at {{clock .}}{{end}}. {{if .IsSetUp}}The earlier setup remains in use.{{else}}The service is not set up.{{end}} Fix the settings file and apply the settings again.</p><details class="fold"><summary>Check failure details</summary><p class="small result">{{.Failure.FailureReason}}</p></details>{{end}}`

var sendingServicesPageBody = `<div class="settings-page stack"><p class="muted">Set up the services that deliver your team's notifications, then add your own <a href="/contact-methods">contact methods</a>.</p><div class="settings-grid">{{range .Setups}}<section class="panel"><div class="panel-head"><h2>{{.SendingServiceTitle}}</h2>{{if .IsSetUp}}{{if .Failure.FailureReason}}<span class="tag warn">Check failed</span>{{else}}<span class="tag ok">Set up</span>{{end}}{{else}}<span class="tag">Not set up</span>{{end}}</div><div class="panel-body">` + sendingServiceState + `{{if .IsSetUp}}<details class="fold"><summary>Last successful check</summary><p class="small result">{{.Description}}</p></details><p class="faint small">Set up by {{.SetUpBy}} at {{clock .SetUpAt}}.</p>{{else}}<p>Not set up. Follow the setup guide to add {{.TitleInSentence}} to the settings file.</p>{{end}}</div><div class="panel-foot"><a href="/sending-services/{{.SendingService}}">{{if .IsSetUp}}Review{{else}}How to set up{{end}} {{.TitleInSentence}}</a></div></section>{{end}}</div><details class="more"><summary>Where sending services are set up</summary><div class="body">` + settingsFileNotice("<code>sending_services</code>") + `</div></details></div>`
var sendingServiceSetupPageBody = `{{define "heading"}}<div><p class="crumbs"><a href="/sending-services">All sending services</a></p><h1>{{.Title}}</h1></div>{{end}}<div class="settings-page stack"><section class="panel"><div class="panel-head"><h2>{{.SendingServiceTitle}}</h2>{{if .IsSetUp}}{{if .Failure.FailureReason}}<span class="tag warn">Earlier setup in use</span>{{else}}<span class="tag ok">Set up · last check passed</span>{{end}}{{else}}<span class="tag">Not set up</span>{{end}}</div><div class="panel-body">` + sendingServiceState + `{{if .IsSetUp}}<details class="fold"><summary>Last successful check</summary><p class="small result">{{.Description}}</p></details><p class="faint small">Set up by {{.SetUpBy}} at {{clock .SetUpAt}}.</p><p>{{.NextStep}} <a href="/contact-methods">Contact methods</a></p>{{else}}<p>Follow the guide below, add the values to the settings file, then run <code>sudo mmhotline settings apply</code>. MMHotline checks the setup before saving.</p>{{end}}</div></section><section class="panel"><div class="panel-head"><h2>Set up {{.TitleInSentence}}</h2></div><div class="panel-body"><ol class="settings-guide">{{range .Guide}}<li><p>{{.Text}}{{if .URL}} <a href="{{.URL}}">{{.LinkLabel}}</a>{{end}}</p></li>{{end}}</ol><h3>Settings file keys</h3>{{$service := .SendingService}}<ul class="settings-fields">{{range .Fields}}<li><strong>{{.Label}}{{if .Optional}} (optional){{end}}</strong><p><code>sending_services.{{$service}}.{{range $index, $part := .KeyParts}}{{if $index}}_<wbr>{{end}}{{$part}}{{end}}</code></p><p class="small muted">{{.Help}}</p></li>{{end}}</ul><h3>Example settings</h3><pre class="settings-code" tabindex="0" aria-label="Example settings, scroll horizontally for long lines">{{.SectionExample}}</pre></div></section><details class="more"><summary>Where {{.TitleInSentence}} is set up</summary><div class="body">` + settingsFileNotice("<code>sending_services.{{.SendingService}}</code>") + `</div></details></div>`

func (a *App) sendingServiceSetupAccess(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		a.browserAccessError(w, r, err)
		return p, false
	}
	member, err := a.CurrentMember(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return p, false
	}
	if member.Role != RoleAdministrator {
		HTTPError(w, ErrForbidden)
		return p, false
	}
	return p, true
}
func (a *App) sendingServicesPage(w http.ResponseWriter, r *http.Request) {
	a.renderSendingServicesPage(w, r, 200, "")
}
func (a *App) renderSendingServicesPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	p, ok := a.sendingServiceSetupAccess(w, r)
	if !ok {
		return
	}
	setups, err := a.sendingServicePageViews(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	RenderPage(w, status, "Sending services", sendingServicesPageBody, struct {
		Setups []sendingServicePageView
		settingsFileNoticeView
	}{setups, a.settingsFileNoticeView()})
}
func (a *App) sendingServiceSetupPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.sendingServiceSetupAccess(w, r)
	if !ok {
		return
	}
	_, definition, err := sendingServiceSetup(r.PathValue("sending_service"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	setups, err := a.sendingServicePageViews(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	view := sendingServiceSetupView{Guide: definition.Guide, NextStep: definition.NextStep, settingsFileNoticeView: a.settingsFileNoticeView()}
	for _, field := range definition.Fields {
		view.Fields = append(view.Fields, sendingServiceFieldView{field, strings.Split(field.SettingsKey, "_")})
	}
	for _, setup := range setups {
		if setup.SendingService == definition.Name {
			view.sendingServicePageView = setup
		}
	}
	RenderPage(w, 200, "Set up "+definition.Title, sendingServiceSetupPageBody, view)
}
