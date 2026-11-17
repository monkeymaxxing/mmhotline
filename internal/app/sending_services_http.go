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

// sendingServicesPageData is the Sending services page's data.
type sendingServicesPageData struct {
	Setups []sendingServicePageView
	settingsFileNoticeView
}

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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, status, "Sending services", frame, nil, sendingServicesPageBody(frame, sendingServicesPageData{setups, a.settingsFileNoticeView()}))
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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	title := "Set up " + definition.Title
	renderPage(w, 200, title, frame, sendingServiceSetupHeading(title), sendingServiceSetupPageBody(frame, view))
}
