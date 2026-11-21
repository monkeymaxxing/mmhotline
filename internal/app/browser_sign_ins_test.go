package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCloudflareAccessBrowserAuthentication(t *testing.T) {
	a := foundationApp(t)
	admin := memberFixture(t, a, "admin@example.test").Principal
	a.WebURL = "https://ops.example.com"
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, token string
		status      int
		text        string
	}{
		{"member", signedAccessToken(accessSigningKey(), admin.SignInEmailAddress), 200, "Dashboard"},
		{"unknown person", signedAccessToken(accessSigningKey(), "unknown@example.com"), 403, "unknown@example.com"},
		{"no identity", "", 401, "Sign in through Cloudflare Access"},
		{"different signing key", signedAccessToken(otherKey, admin.SignInEmailAddress), 401, "Sign in through Cloudflare Access"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			oldLog := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(oldLog) })
			request := httptest.NewRequest("GET", "/dashboard", nil)
			request.Header.Set("Accept", "text/html")
			if test.token != "" {
				request.Header.Set("Cf-Access-Jwt-Assertion", test.token)
			}
			response := httptest.NewRecorder()
			a.Handler().ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.text) {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
			}
			// Only a member sees the pages a member may open.
			if menu := strings.Contains(response.Body.String(), `href="/people"`); menu != (test.status == 200) {
				t.Fatalf("HTTP %d shows the menu: %v", response.Code, menu)
			}
			// A person refused at the door can be found in the journal.
			if test.status == 403 && !strings.Contains(logs.String(), "Sign-in refused: unknown@example.com is not a member of the team") {
				t.Fatalf("missing log line: %q", logs.String())
			}
			if test.status == 401 {
				for _, text := range []string{a.WebURL, "sudo mmhotline people create-sign-in-link --sign-in-email-address ADDRESS", "ssh -L 8480:127.0.0.1:8480 YOUR_SSH_HOST"} {
					if !strings.Contains(response.Body.String(), text) {
						t.Fatalf("missing recovery instruction %q", text)
					}
				}
			}
			if test.token != "" && strings.Contains(response.Body.String(), test.token) {
				t.Fatal("token exposed in page")
			}
		})
	}
}

func createBrowserSignInLink(t *testing.T, a *App, email string) CommandResult {
	t.Helper()
	result, raw := commandRequest(t, a, CommandRequest{Group: "people", Action: "create-sign-in-link", People: PeopleCommandRequest{SignInEmailAddress: email}}, 200)
	assertCommandKeys(t, raw, "sign_in_link_url", "sign_in_link_valid_until", "ssh_forward_command")
	return result
}

func useBrowserSignInLink(t *testing.T, a *App, link string, status int) *http.Cookie {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, httptest.NewRequest("GET", u.Path, nil))
	if response.Code != status {
		t.Fatalf("link: HTTP %d: %s", response.Code, response.Body.String())
	}
	if status != 303 {
		if !strings.Contains(response.Body.String(), "already used or has expired") || !strings.Contains(response.Body.String(), "create-sign-in-link") {
			t.Fatalf("missing recovery instructions: %s", response.Body.String())
		}
		if len(response.Result().Cookies()) != 0 {
			t.Fatal("invalid link set cookie")
		}
		return nil
	}
	if response.Header().Get("Location") != "/dashboard" {
		t.Fatal("wrong redirect")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %v", cookies)
	}
	c := cookies[0]
	if c.Name != "__Host-mmhotline-sign-in" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 43200 {
		t.Fatalf("cookie attributes: %+v", c)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(decoded) != 32 {
		t.Fatal("cookie is not a 32-byte token")
	}
	return c
}

func browserCookieRequest(t *testing.T, a *App, cookie *http.Cookie, status int) {
	t.Helper()
	request := httptest.NewRequest("GET", "/dashboard", nil)
	request.Header.Set("Accept", "text/html")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("cookie: HTTP %d: %s", response.Code, response.Body.String())
	}
}

func TestSignInLinkSingleUseAndCookieExpiry(t *testing.T) {
	a := foundationApp(t)
	a.AccessVerifier = nil
	memberFixture(t, a, "admin@example.test")
	a.AccessVerifier = nil
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldLog) })
	result := createBrowserSignInLink(t, a, "ADMIN@EXAMPLE.TEST")
	if result.SignInLinkValidUntil != "2026-10-27T10:10:00Z" || result.SSHForwardCommand != "ssh -L 8480:127.0.0.1:8480 YOUR_SSH_HOST" || !strings.HasPrefix(result.SignInLinkURL, "http://localhost:8480/sign-in/") {
		t.Fatalf("link result: %+v", result)
	}
	if !strings.Contains(result.Text(), "works once") || !strings.Contains(result.Text(), "UTC") {
		t.Fatal(result.Text())
	}
	token := strings.TrimPrefix(result.SignInLinkURL, "http://localhost:8480/sign-in/")
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatal("link is not a 32-byte token")
	}
	var stored string
	if err := a.Store.db.QueryRow(`SELECT token_sha256 FROM browser_sign_ins`).Scan(&stored); err != nil || stored != tokenHash(token) || stored == token {
		t.Fatalf("stored link: %q %v", stored, err)
	}
	if !strings.Contains(logs.String(), "admin@example.test") || strings.Contains(logs.String(), token) {
		t.Fatalf("unsafe or missing log: %s", logs.String())
	}
	cookie := useBrowserSignInLink(t, a, result.SignInLinkURL, 303)
	useBrowserSignInLink(t, a, result.SignInLinkURL, 401)
	if !strings.Contains(logs.String(), "Sign-in link refused: the link was already used or has expired") {
		t.Fatalf("missing log line: %q", logs.String())
	}
	browserCookieRequest(t, a, cookie, 200)
	if err := a.Store.db.QueryRow(`SELECT token_sha256 FROM browser_sign_ins`).Scan(&stored); err != nil || stored != tokenHash(cookie.Value) {
		t.Fatalf("stored cookie: %q %v", stored, err)
	}
	now = now.Add(12*time.Hour - time.Second)
	browserCookieRequest(t, a, cookie, 200)
	now = now.Add(time.Second)
	browserCookieRequest(t, a, cookie, 401)
}

func TestSignInLinkExpiryAndCleanup(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	memberFixture(t, a, "admin@example.test")
	now := a.Now()
	a.Now = func() time.Time { return now }
	old := createBrowserSignInLink(t, a, "admin@example.test")
	now = now.Add(10 * time.Minute)
	useBrowserSignInLink(t, a, old.SignInLinkURL, 401)
	createBrowserSignInLink(t, a, "admin@example.test")
	if n := scalarTeam(t, a, `SELECT count(*) FROM browser_sign_ins`); n != 1 {
		t.Fatalf("expired rows not removed: %d", n)
	}
}

func TestRemovingPersonRevokesBrowserSignIns(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	memberFixture(t, a, "admin@example.test")
	memberFixture(t, a, "member@example.test")
	link := createBrowserSignInLink(t, a, "member@example.test")
	cookie := useBrowserSignInLink(t, a, link.SignInLinkURL, 303)
	pending := createBrowserSignInLink(t, a, "member@example.test")
	browserCookieRequest(t, a, cookie, 200)
	if err := removePerson(t, a, "member@example.test"); err != nil {
		t.Fatal(err)
	}
	browserCookieRequest(t, a, cookie, 401)
	useBrowserSignInLink(t, a, pending.SignInLinkURL, 401)
	if n := scalarTeam(t, a, `SELECT count(*) FROM browser_sign_ins`); n != 0 {
		t.Fatalf("removal retained %d sign-ins", n)
	}
	for _, email := range []string{"member@example.test", "unknown@example.test"} {
		commandRequest(t, a, CommandRequest{Group: "people", Action: "create-sign-in-link", People: PeopleCommandRequest{SignInEmailAddress: email}}, 404)
	}
}

func TestAccessHeaderTakesPrecedenceOverCookie(t *testing.T) {
	for _, mode := range []string{"valid", "invalid", "empty", "keys unavailable", "no verifier"} {
		t.Run(mode, func(t *testing.T) {
			a := foundationApp(t)
			memberFixture(t, a, "admin@example.test")
			memberFixture(t, a, "member@example.test")
			link := createBrowserSignInLink(t, a, "member@example.test")
			cookie := useBrowserSignInLink(t, a, link.SignInLinkURL, 303)
			request := httptest.NewRequest("GET", "/dashboard", nil)
			request.Header.Set("Accept", "text/html")
			request.AddCookie(cookie)
			request.Header.Set("Cf-Access-Jwt-Assertion", "invalid")
			want := 401
			switch mode {
			case "valid":
				addAccessToken(request, "admin@example.test")
				want = 200
			case "empty":
				request.Header.Set("Cf-Access-Jwt-Assertion", "")
			case "keys unavailable":
				unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
				defer unavailable.Close()
				a.AccessVerifier.CertsURL = unavailable.URL
				addAccessToken(request, "admin@example.test")
				want = 503
			case "no verifier":
				a.AccessVerifier = nil
				want = 200
			}
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			response := httptest.NewRecorder()
			a.Handler().ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != want {
				t.Fatalf("HTTP %d: %s", response.Code, body)
			}
			if mode == "valid" && !strings.Contains(body, `href="/sending-services"`) {
				t.Fatal("navigation did not use token administrator")
			}
			if mode == "no verifier" && strings.Contains(body, `href="/sending-services"`) {
				t.Fatal("navigation did not use cookie member")
			}
			if mode == "keys unavailable" && (!strings.Contains(body, "cannot check") || !strings.Contains(body, "sign-in link") || !strings.Contains(logs.String(), "HTTP status 503")) {
				t.Fatalf("outage page/log missing: %s %s", body, logs.String())
			}
			if (mode == "invalid" || mode == "empty") && !strings.Contains(logs.String(), "expected three non-empty segments") {
				t.Fatal("failure reason not logged")
			}
			if mode == "keys unavailable" {
				browserCookieRequest(t, a, cookie, 200)
			}
			if token := request.Header.Get("Cf-Access-Jwt-Assertion"); len(token) > 50 && strings.Contains(logs.String(), token) {
				t.Fatal("logged token")
			}
		})
	}
}

func TestSignInExchangeRollsBackWhenCookieCannotBeSaved(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	memberFixture(t, a, "admin@example.test")
	link := createBrowserSignInLink(t, a, "admin@example.test")
	if _, err := a.Store.db.Exec(`CREATE TRIGGER refuse_cookie BEFORE INSERT ON browser_sign_ins WHEN NEW.kind='sign_in_cookie' BEGIN SELECT RAISE(ABORT,'cannot save cookie'); END`); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link.SignInLinkURL)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, httptest.NewRequest("GET", u.Path, nil))
	if response.Code != 500 || len(response.Result().Cookies()) != 0 {
		t.Fatalf("failed exchange: %d", response.Code)
	}
	if _, err := a.Store.db.Exec(`DROP TRIGGER refuse_cookie`); err != nil {
		t.Fatal(err)
	}
	useBrowserSignInLink(t, a, link.SignInLinkURL, 303)
}

func TestIntakeRoutesAreAbsentFromPages(t *testing.T) {
	t.Parallel()
	a, _, source := alertFixture(t)
	for _, route := range []string{"alert-intake", "check-in"} {
		request := httptest.NewRequest("POST", "/"+route+"/"+source.Source.ID, strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		if response.Code != 404 && response.Code != 405 {
			t.Fatalf("pages expose %s: %d", route, response.Code)
		}
	}
}

func TestAlertSourceAddressesUseIntakeListener(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	a.WebURL = "https://ops.example.com"
	a.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	principal := memberFixture(t, a, "admin@example.test").Principal
	source, err := createAlertSource(context.Background(), a, principal, "Monitoring")
	if err != nil {
		t.Fatal(err)
	}
	list, raw := commandRequest(t, a, CommandRequest{Group: "alert-sources", Action: "list"}, 200)
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"web_url", "private_network_url", "private_network_intake_url", "private_network_check_in_url"} {
		if _, found := fields[retired]; found {
			t.Fatalf("retired field %s", retired)
		}
	}
	intakeURL, checkInURL := "http://10.20.0.1:8484/alert-intake/"+source.Source.ID, "http://10.20.0.1:8484/check-in/"+source.Source.ID
	if list.AlertIntakeAddress != "http://10.20.0.1:8484" || len(list.AlertSources) != 1 || list.AlertSources[0].IntakeURL != intakeURL || list.AlertSources[0].CheckInURL != checkInURL {
		t.Fatalf("list addresses: %+v", list)
	}
	request := httptest.NewRequest("GET", "/alert-sources", nil)
	addAccessToken(request, principal.SignInEmailAddress)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	for _, text := range []string{"Alert intake address", "Intake URL", "Check-in URL", list.AlertIntakeAddress, intakeURL, checkInURL} {
		if !strings.Contains(response.Body.String(), text) || !strings.Contains(list.Text(), text) {
			t.Fatalf("missing page/text address %q", text)
		}
	}
	if !strings.Contains(response.Body.String(), "Set in <code>/etc/mmhotline/mmhotline_settings.yml</code>, in <code>alert_sources</code>.") {
		t.Fatalf("the page does not say where the alert sources are set:\n%s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), a.WebURL) {
		t.Fatal("web address shown as intake address")
	}
}

// The sign-in link is opened at http://localhost through an SSH forward.
// WebKit keeps no Secure cookie there, so both cookies go without Secure and
// the __Host- prefix, and the session still opens pages and saves forms.
func TestSignInLinkThroughSSHForwardUsesPlainHTTPCookies(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	memberFixture(t, a, "admin@example.test")
	a.WebURL = "https://ops.example.com"
	a.WebListenAddress = "127.0.0.1:8480"
	link := createBrowserSignInLink(t, a, "admin@example.test").SignInLinkURL
	if !strings.HasPrefix(link, "http://localhost:8480/sign-in/") {
		t.Fatalf("link: %s", link)
	}
	handler := a.Handler()
	opened := httptest.NewRecorder()
	handler.ServeHTTP(opened, httptest.NewRequest("GET", link, nil))
	cookies := opened.Result().Cookies()
	if opened.Code != 303 || len(cookies) != 1 || cookies[0].Name != "mmhotline-sign-in" || cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("HTTP %d, cookies %+v", opened.Code, cookies)
	}
	signIn := cookies[0]
	page := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "http://localhost:8480/on-call", nil)
	request.AddCookie(signIn)
	handler.ServeHTTP(page, request)
	var csrf *http.Cookie
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == "mmhotline-csrf" && !cookie.Secure {
			csrf = cookie
		}
	}
	if page.Code != 200 || csrf == nil {
		t.Fatalf("on-call page: HTTP %d, cookies %+v", page.Code, page.Result().Cookies())
	}
	availability := page.Body.String()[strings.Index(page.Body.String(), "<h2>Your availability</h2>"):]
	form := url.Values{"csrf": {csrf.Value}, "action": {"unavailable"}, "duration": {"2h"}, "revision": {workflowField(t, availability, "revision")}}
	post := httptest.NewRequest("POST", "http://localhost:8480/on-call", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("Origin", "http://localhost:8480")
	post.AddCookie(signIn)
	post.AddCookie(csrf)
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, post)
	if saved.Code != 303 {
		t.Fatalf("form: HTTP %d: %s", saved.Code, saved.Body.String())
	}
	after := httptest.NewRecorder()
	request = httptest.NewRequest("GET", "http://localhost:8480/on-call", nil)
	request.AddCookie(signIn)
	handler.ServeHTTP(after, request)
	if !strings.Contains(after.Body.String(), "You are unavailable until") {
		t.Fatal("the form through the SSH forward was not saved")
	}
}

func TestSignInPageWithoutCloudflareAccessShowsSignInLinkInstructions(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	a.AccessVerifier = nil
	a.WebURL = "https://ops.example.com"
	request := httptest.NewRequest("GET", "/dashboard", nil)
	request.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	page := response.Body.String()
	if response.Code != 401 {
		t.Fatalf("HTTP %d: %s", response.Code, page)
	}
	mustContain(t, "sign-in without Access", pageText(page), "Sign in with a sign-in link", "To get a sign-in link, ask someone with SSH access", "sudo mmhotline people create-sign-in-link --sign-in-email-address ADDRESS", "ssh -L 8480:127.0.0.1:8480 YOUR_SSH_HOST")
	if strings.Contains(page, "Cloudflare Access") || strings.Contains(page, a.WebURL) || strings.Contains(page, "<details") {
		t.Fatal("sign-in link instructions are hidden or the page offers unconfigured Access")
	}
}
