package app

import (
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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, status, "Triage agent", frame, nil, triageAgentPageBody(frame, triageAgentPageData{
		TriageAgentView:        state,
		ReviewAgent:            reviewAgent,
		CSRF:                   a.CSRFToken(w, r),
		Error:                  message,
		settingsFileNoticeView: a.settingsFileNoticeView(),
	}))
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

// triageAgentPageData is what the Triage agent page shows (triage_agent.templ).
type triageAgentPageData struct {
	TriageAgentView
	ReviewAgent ReviewAgentView
	CSRF, Error string
	settingsFileNoticeView
}
