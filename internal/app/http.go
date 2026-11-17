package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"

	"mmhotline/internal/cloudflareaccess"
)

const csrfCookieName = "__Host-mmhotline-csrf"
const CSRFCookieLifetime = 30 * 24 * time.Hour

type browserIdentityContextKey struct{}
type browserIdentity struct {
	principal Principal
	member    Member
	err       error
}

func (a *App) resolveBrowserIdentity(r *http.Request) browserIdentity {
	var identity browserIdentity
	if tokens, present := r.Header[http.CanonicalHeaderKey("Cf-Access-Jwt-Assertion")]; present && a.AccessVerifier != nil {
		token := ""
		if len(tokens) == 1 {
			token = tokens[0]
		}
		email, err := a.AccessVerifier.Verify(r.Context(), token)
		if err != nil {
			log.Printf("Cloudflare Access sign-in failed: %v", err)
			identity.err = err
			if errors.Is(err, cloudflareaccess.ErrInvalidToken) {
				identity.err = ErrUnauthorized
			}
			return identity
		}
		identity.principal.SignInEmailAddress = strings.ToLower(email)
	} else {
		identity.principal, identity.err = a.cookiePrincipal(r)
		if identity.err != nil {
			return identity
		}
	}
	identity.member, identity.err = a.CurrentMember(r.Context(), identity.principal)
	return identity
}

func (a *App) BrowserPrincipal(r *http.Request) (Principal, error) {
	identity, ok := r.Context().Value(browserIdentityContextKey{}).(browserIdentity)
	if !ok {
		identity = a.resolveBrowserIdentity(r)
	}
	return identity.principal, identity.err
}

// browserCookie names a cookie for the address the browser used. Browsers keep
// a Secure cookie only over HTTPS, and WebKit does not count http://localhost
// as HTTPS. A browser on a loopback address came through an SSH forward over
// plain HTTP, so its cookies drop Secure and the __Host- prefix that requires
// it; through Cloudflare they keep both.
func browserCookie(r *http.Request, httpsName string) (name string, secure bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return strings.TrimPrefix(httpsName, "__Host-"), false
	}
	return httpsName, true
}

func (a *App) CSRFToken(w http.ResponseWriter, r *http.Request) string {
	name, secure := browserCookie(r, csrfCookieName)
	if c, e := r.Cookie(name); e == nil && len(c.Value) == 43 {
		return c.Value
	}
	token := randomToken()
	// Keep forms usable on long-open pages.
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", Secure: secure, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(CSRFCookieLifetime.Seconds())})
	return token
}
func (a *App) CheckBrowserMutation(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "POST" {
		return ErrInvalid
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		return ErrInvalid
	}
	name, _ := browserCookie(r, csrfCookieName)
	c, err := r.Cookie(name)
	if err != nil || len(c.Value) != 43 {
		return ErrForbidden
	}
	token := r.PostForm.Get("csrf")
	if len(token) != 43 || subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) != 1 {
		return ErrForbidden
	}
	return nil
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sign-in/{token}", a.signInLinkHTTP)
	a.RegisterSendingServiceSetupRoutes(mux)
	a.RegisterTriageAgentRoutes(mux)
	a.RegisterAgentPlaybookRulesRoutes(mux)
	a.RegisterApprovalRoutes(mux)
	a.RegisterTriageSessionRoutes(mux)
	a.RegisterPeopleRoutes(mux)
	a.RegisterCoverageRoutes(mux)
	a.RegisterSourceRoutes(mux)
	a.RegisterAlertSourcePageRoutes(mux)
	a.RegisterIncidentRoutes(mux)
	a.RegisterContactMethodRoutes(mux)
	a.RegisterSlackRoutes(mux)
	a.RegisterPhoneRoutes(mux)
	a.RegisterDashboardRoutes(mux)
	a.RegisterStatusPageRoutes(mux)
	a.RegisterDownAlarmRoutes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/dashboard", http.StatusSeeOther) })
	return SecurityHeaders(http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve once so the frame and the page use the same verified identity.
		identity := a.resolveBrowserIdentity(r)
		r = r.WithContext(context.WithValue(r.Context(), browserIdentityContextKey{}, identity))
		pageWriter := &navigationPageWriter{ResponseWriter: w, app: a, request: r, principal: identity.principal, member: identity.err == nil}
		_, pattern := mux.Handler(r)
		if pattern == "" && browserPageRequest(r) {
			// The mux also answers 405 here. Replace only its unknown-path 404.
			mux.ServeHTTP(&unknownPageWriter{ResponseWriter: pageWriter}, r)
			return
		}
		mux.ServeHTTP(pageWriter, r)
	})))
}
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func errorStatus(err error) int {
	switch {
	case errors.Is(err, cloudflareaccess.ErrKeysUnavailable):
		return 503
	case errors.Is(err, ErrInvalid):
		return 400
	case errors.Is(err, ErrUnauthorized):
		return 401
	case errors.Is(err, ErrForbidden):
		return 403
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrConflict):
		return 409
	case errors.Is(err, ErrRateLimited):
		return 429
	default:
		return 500
	}
}
func publicError(err error) string {
	if errors.Is(err, ErrUnauthorized) {
		return "Sign in through Cloudflare Access or a sign-in link."
	}
	if errorStatus(err) == 500 {
		return "The operation could not be saved. Please try again."
	}
	if errors.Is(err, ErrRateLimited) {
		return strings.TrimPrefix(err.Error(), ErrRateLimited.Error()+": ")
	}
	if errors.Is(err, errIncidentChanged) {
		return "The incident changed before your action was saved. Reload the page and try again."
	}
	// The kind of error sets the HTTP status; the reader needs only the reason.
	message := err.Error()
	for _, kind := range []error{ErrInvalid, ErrConflict, ErrNotFound, ErrForbidden} {
		message = strings.TrimPrefix(message, kind.Error()+": ")
	}
	return message
}

// browserPageRequest distinguishes a person's navigation from refresh.js and
// other fetches (whose mode is not navigate and whose Accept is normally */*).
func browserPageRequest(r *http.Request) bool {
	if r.URL.Path == "/mmhotline-contact-card.vcf" || strings.HasPrefix(r.URL.Path, "/assets/") {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// unknownPageWriter replaces only the mux's 404, leaving its 405 and Allow
// header intact. The mux's plain-text 404 body is discarded after rendering.
type unknownPageWriter struct {
	http.ResponseWriter
	notFound bool
}

func (w *unknownPageWriter) WriteHeader(status int) {
	w.notFound = status == http.StatusNotFound
	if w.notFound {
		HTTPError(w.ResponseWriter, ErrNotFound)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *unknownPageWriter) Write(body []byte) (int, error) {
	if w.notFound {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func HTTPError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrRateLimited) {
		w.Header().Set("Retry-After", "900")
	}
	if errors.Is(err, cloudflareaccess.ErrKeysUnavailable) {
		renderSignInUnavailable(w)
		return
	}
	if page, ok := w.(*navigationPageWriter); ok && browserPageRequest(page.request) {
		renderBrowserError(w, err, page.request)
		return
	}
	// Other requests, such as refresh.js's own fetches, keep a plain answer.
	http.Error(w, publicError(err), errorStatus(err))
}

func renderSignInUnavailable(w http.ResponseWriter) {
	renderSimplePage(w, 503, "Cannot check sign-ins right now", signInUnavailableBody())
}

func renderBrowserError(w http.ResponseWriter, err error, r *http.Request) {
	status := errorStatus(err)
	title, reason := "Cannot complete the request", publicError(err)
	switch status {
	case 400:
		title = "Check the request"
		if errors.Is(err, ErrInvalid) && err.Error() == ErrInvalid.Error() {
			reason = "The request is missing a value or contains a value MMHotline cannot use."
		}
	case 401:
		title = "Sign in to continue"
	case 403:
		title = "You cannot make this change"
		if err.Error() == ErrForbidden.Error() {
			reason = "Your sign-in does not allow this action, or the form has expired. Open the page again before trying the action."
		}
	case 404:
		title = "Page not found"
		if err.Error() == ErrNotFound.Error() {
			reason = "MMHotline could not find the page or record you asked for."
		}
	case 409:
		title = "The page changed"
		if err.Error() == ErrConflict.Error() {
			reason = "The information changed before your action was saved. Open the page again and try the action."
		}
	case 429:
		title = "Too many requests"
		if err.Error() == ErrRateLimited.Error() {
			reason = "MMHotline received too many requests."
		}
		reason += " Try again in 15 minutes."
	}
	target, label := "/dashboard", "Back to the dashboard"
	// A mutation's route identifies its source page without trusting Referer.
	if r.Method == http.MethodPost {
		if id := r.PathValue("id"); strings.HasPrefix(r.URL.Path, "/incidents/") && recordIDPattern.MatchString(id) {
			target, label = "/incidents/"+id, "Back to the incident"
		}
		if id := r.PathValue("triage_session_id"); recordIDPattern.MatchString(id) {
			target, label = "/triage-sessions/"+id, "Back to the triage session"
		}
	}
	renderSimplePage(w, status, title, browserErrorBody(reason, target, label))
}

// renderSimplePage writes a templ page that has the frame's own heading.
func renderSimplePage(w http.ResponseWriter, status int, title string, body templ.Component) {
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, status, title, frame, nil, body)
}

// signInPageData is what the sign-in page shows (http.templ).
type signInPageData struct {
	WebURL, SSHForwardCommand string
	AccessConfigured          bool
}

// notATeamMemberPageData is what the not-a-team-member page shows (http.templ).
type notATeamMemberPageData struct {
	SignInEmailAddress, TeamName string
	settingsFileNoticeView
}

// IntakeHandler exposes only signed sender requests.
func (a *App) IntakeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /alert-intake/{alert_source_id}", a.genericIntake)
	mux.HandleFunc("POST /check-in/{alert_source_id}", a.alertSourceCheckInHTTP)
	return SecurityHeaders(mux)
}
func (a *App) browserAccessError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnauthorized) {
		_, forward := a.sshForwardCommand()
		title := "Sign in with a sign-in link"
		if a.AccessVerifier != nil {
			title = "Sign in through Cloudflare Access"
		}
		renderSimplePage(w, 401, title, signInPageBody(signInPageData{a.WebURL, forward, a.AccessVerifier != nil}))
		return
	}
	if errors.Is(err, ErrForbidden) {
		var teamName string
		if e := a.Store.db.QueryRowContext(r.Context(), `SELECT name FROM team_info`).Scan(&teamName); e != nil {
			HTTPError(w, e)
			return
		}
		principal, _ := a.BrowserPrincipal(r)
		// Cloudflare Access let this person through, so the journal is where
		// an administrator finds who was refused and why.
		log.Printf("Sign-in refused: %s is not a member of the team", principal.SignInEmailAddress)
		notice := a.settingsFileNoticeView()
		notice.SettingsFile = a.SettingsFilePath()
		renderSimplePage(w, 403, "Not a team member", notATeamMemberBody(notATeamMemberPageData{principal.SignInEmailAddress, teamName, notice}))
		return
	}
	HTTPError(w, err)
}
