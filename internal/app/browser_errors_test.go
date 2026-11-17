package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/cloudflareaccess"

	"github.com/a-h/templ"
)

// pageLinkText checks a link's destination and readable label without coupling
// the assertion to a class or to the order of the link's attributes.
func pageLinkText(page, target string) string {
	match := regexp.MustCompile(`<a\b[^>]*href="` + regexp.QuoteMeta(target) + `"[^>]*>(.*?)</a>`).FindStringSubmatch(page)
	if len(match) < 2 {
		return ""
	}
	return pageText(match[1])
}

func TestBrowserErrorsKeepStatusAndOfferAWayBack(t *testing.T) {
	for _, test := range []struct {
		err             error
		heading, reason string
	}{
		{ErrInvalid, "Check the request", "missing a value"},
		{fmt.Errorf("%w: invalid date", ErrInvalid), "Check the request", "invalid date"},
		{ErrForbidden, "You cannot make this change", "form has expired"},
		{ErrNotFound, "Page not found", "could not find"},
		{ErrConflict, "The page changed", "information changed"},
		{errIncidentChanged, "The page changed", "incident changed"},
		{ErrRateLimited, "Too many requests", "Try again in 15 minutes"},
		{errors.New("private database failure"), "Cannot complete the request", "operation could not be saved"},
		{cloudflareaccess.ErrKeysUnavailable, "Cannot check sign-ins right now", "create-sign-in-link"},
	} {
		t.Run(test.heading+test.err.Error(), func(t *testing.T) {
			request := httptest.NewRequest("GET", "/alerts/missing", nil)
			request.Header.Set("Accept", "text/html")
			response := httptest.NewRecorder()
			HTTPError(&navigationPageWriter{ResponseWriter: response, request: request}, test.err)
			body := response.Body.String()
			if response.Code != errorStatus(test.err) || response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("browser error: HTTP %d, %s", response.Code, body)
			}
			if !strings.Contains(body, `name="viewport"`) || !strings.Contains(pageText(body), test.heading) || !strings.Contains(pageText(body), test.reason) || pageLinkText(body, "/dashboard") == "" {
				t.Fatalf("missing readable reason or way back: %s", body)
			}
			if strings.Contains(body, "private database failure") {
				t.Fatal("private failure exposed")
			}
			if test.err == ErrRateLimited && response.Header().Get("Retry-After") != "900" {
				t.Fatal("lost rate-limit retry time")
			}
		})
	}
	for _, path := range []string{"/incidents/0123456789abcdef0123456789abcdef/claim", "/triage-sessions/0123456789abcdef0123456789abcdef/stop"} {
		request := httptest.NewRequest("POST", path, nil)
		request.Header.Set("Sec-Fetch-Mode", "navigate")
		key := "id"
		if strings.HasPrefix(path, "/triage-sessions/") {
			key = "triage_session_id"
		}
		request.SetPathValue(key, "0123456789abcdef0123456789abcdef")
		response := httptest.NewRecorder()
		HTTPError(&navigationPageWriter{ResponseWriter: response, request: request}, ErrConflict)
		if pageLinkText(response.Body.String(), path[:strings.LastIndex(path, "/")]) == "" {
			t.Fatal("mutation error does not return to its page")
		}
	}
}

func TestFetchAndNonPageErrorsKeepTheirResponses(t *testing.T) {
	for _, test := range []struct{ path, accept, mode string }{
		{"/alerts/missing", "*/*", "same-origin"}, // refresh.js
		{"/alerts/missing", "text/html", "cors"},  // a fetch may ask for HTML
		{"/alerts/missing", "application/json", ""},
		{"/alerts/missing", "", ""},
		{"/mmhotline-contact-card.vcf", "text/html", "navigate"},
		{"/assets/missing", "text/html", "navigate"},
	} {
		for _, err := range []error{ErrInvalid, ErrNotFound, ErrConflict, ErrRateLimited, errors.New("private failure")} {
			request := httptest.NewRequest("GET", test.path, nil)
			request.Header.Set("Accept", test.accept)
			request.Header.Set("Sec-Fetch-Mode", test.mode)
			response := httptest.NewRecorder()
			HTTPError(&navigationPageWriter{ResponseWriter: response, request: request}, err)
			if response.Code != errorStatus(err) || response.Body.String() != publicError(err)+"\n" || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("changed non-page response: %d %s", response.Code, response.Body.String())
			}
			if err == ErrRateLimited && response.Header().Get("Retry-After") != "900" {
				t.Fatal("lost non-page retry time")
			}
		}
	}
	// Signed senders use a separate listener even when they send browser headers.
	a := foundationApp(t)
	request := httptest.NewRequest("POST", "/alert-intake/missing", strings.NewReader("bad body"))
	request.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	a.IntakeHandler().ServeHTTP(response, request)
	if response.Code != 400 || response.Body.String() != "body: Content-Type must be application/json\n" {
		t.Fatalf("changed intake response: %d %s", response.Code, response.Body.String())
	}
}

func TestUnknownBrowserPathHasAFramed404AndKeepsMethodErrors(t *testing.T) {
	a := foundationApp(t)
	for _, test := range []struct {
		method, path, accept string
		status               int
		framed               bool
	}{
		{"GET", "/unknown", "text/html", 404, true},
		{"GET", "/unknown", "*/*", 404, false},
		{"GET", "/assets/missing", "text/html", 404, false},
		{"POST", "/unknown", "text/html", 404, true},
		{"DELETE", "/dashboard", "text/html", 405, false},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		request.Header.Set("Accept", test.accept)
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		body := response.Body.String()
		if response.Code != test.status {
			t.Fatalf("%s %s: %d %s", test.method, test.path, response.Code, body)
		}
		if test.framed {
			if !strings.Contains(pageText(body), "Page not found") || pageLinkText(body, "/dashboard") == "" || !strings.Contains(body, `name="viewport"`) {
				t.Fatalf("missing browser recovery: %s", body)
			}
		} else if test.status == 404 && body != "404 page not found\n" {
			t.Fatalf("changed non-page not found: %q", body)
		}
		if test.status == 405 && response.Header().Get("Allow") == "" {
			t.Fatal("lost allowed methods")
		}
	}
}

func TestBrowserRenderingFailureStillHasAWayBack(t *testing.T) {
	request := httptest.NewRequest("GET", "/dashboard", nil)
	request.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	broken := templ.ComponentFunc(func(context.Context, io.Writer) error { return errors.New("broken page") })
	renderPage(&navigationPageWriter{ResponseWriter: response, request: request}, 200, "Broken page", pageFrame{TimeZone: time.UTC}, nil, broken)
	if response.Code != http.StatusInternalServerError || pageLinkText(response.Body.String(), "/dashboard") == "" {
		t.Fatalf("rendering failure stranded the browser: %d %s", response.Code, response.Body.String())
	}
}

func TestNonMemberSettingsPathIsFolded(t *testing.T) {
	a := foundationApp(t)
	a.UseSettingsFile("/etc/team/mmhotline_settings.yml", nil)
	request := httptest.NewRequest("GET", "/dashboard", nil)
	request.Header.Set("Accept", "text/html")
	addAccessToken(request, "stranger@example.test")
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	page := response.Body.String()
	folds := regexp.MustCompile(`(?s)<details\b[^>]*>.*?</details>`)
	if response.Code != 403 || !strings.Contains(pageText(page), "Ask an administrator to add your sign-in email address") || !strings.Contains(page, "/etc/team/mmhotline_settings.yml") || strings.Contains(folds.ReplaceAllString(page, ""), "/etc/team/mmhotline_settings.yml") || pageLinkText(page, "/people") != "" {
		t.Fatalf("refused person cannot find the right recovery step: %s", page)
	}
}

func TestManualSOSErrorKeepsInputAndOffersRetry(t *testing.T) {
	h := newWorkflowRig(t)
	person := h.browser()
	person.addPerson("admin@example.test", "Responder")
	csrf := workflowField(t, person.get("/dashboard", 200), "csrf")
	page := person.post("/incidents/new", url.Values{"csrf": {csrf}, "title": {""}, "message": {"The database log says <error>."}}, 400)
	if !strings.Contains(pageText(page), "Raise an SOS by hand") || !strings.Contains(page, "The database log says &lt;error&gt;.") || pageLinkText(page, "/dashboard") == "" || workflowField(t, page, "csrf") == "" {
		t.Fatalf("failed SOS form lost input or recovery: %s", page)
	}
}
