package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"mmhotline/internal/triageagent"
)

func TestTriageAgentPageStatesAndForms(t *testing.T) {
	h := newWorkflowRig(t)
	h.app.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	settings := h.app.settingsPrincipal()
	admin := h.browser()
	admin.addPerson("admin@example.test", "Administrator")
	page := admin.get("/triage-agent", 200)
	if !strings.Contains(pageText(page), TriageAgentSandboxNotInstalled) || strings.Contains(page, "<form") {
		t.Fatal("uninstalled page", page)
	}
	if dashboard := admin.get("/dashboard", 200); !strings.Contains(dashboard, `href="/sending-services"`) || !strings.Contains(dashboard, `href="/triage-agent"`) {
		t.Fatal("administrator navigation missing")
	}
	member, _ := addMember(t, h.app, "Member", RoleMember)
	other := h.browser()
	other.identify(member.SignInEmailAddress)
	other.get("/triage-agent", 403)
	if strings.Contains(other.get("/dashboard", 200), `href="/triage-agent"`) {
		t.Fatal("member administrator link")
	}
	runs := triageRunFixture(t, h.app)
	page = admin.get("/triage-agent", 200)
	csrf := workflowField(t, admin.get("/contact-methods", 200), "csrf")
	for _, text := range []string{"Triage agent: not set up", `claude setup-token`, "Codex takes an API key", "Listed hosts only.", "OSV.dev's API", "Recent connections",
		"Where the triage agent is set up", "triage_agent", "review_agent", "sudo mmhotline settings apply"} {
		if !strings.Contains(pageText(page), text) {
			t.Fatalf("page missing %q: %s", text, page)
		}
	}
	for _, text := range []string{"<form", `type="password"`, "device-auth", "subscription_sign_in", "Subscription sign-in"} {
		if strings.Contains(page, text) {
			t.Fatalf("page has %q: %s", text, page)
		}
	}
	admin.post("/triage-agent", url.Values{"action": {"set-internet-access"}, "internet_access": {"any_site"}}, 403)
	other.post("/triage-agent", url.Values{"csrf": {csrf}, "action": {"set-internet-access"}, "internet_access": {"any_site"}}, 403)
	post := func(values url.Values, status int) string {
		values.Set("csrf", csrf)
		return admin.post("/triage-agent", values, status)
	}
	// The settings file sets the agents up; the page has no set-up.
	for _, action := range []string{"set-up", "remove-setup", "review-agent-set-up", "review-agent-remove-setup"} {
		if failed := post(url.Values{"action": {action}, "coding_agent": {"codex"}, "credential_type": {"api_key"}, "credential": {"never-show-this"}}, 400); strings.Contains(failed, "never-show-this") {
			t.Fatal("a credential was shown")
		}
	}
	// The settings file sets the internet access, the review interval, and
	// the listed hosts; the page only shows them.
	post(url.Values{"action": {"set-internet-access"}, "internet_access": {"any_site"}}, 400)
	ctx := context.Background()
	if err := h.app.SetTriageAgentInternetAccess(ctx, settings, "any_site"); err != nil {
		t.Fatal(err)
	}
	page = admin.get("/triage-agent", 200)
	if !strings.Contains(page, "Any site, with an instruction to use well-known sources.") || strings.Contains(page, `name="internet_access"`) {
		t.Fatal("internet access not shown read-only")
	}
	// Off until the settings file sets an interval.
	for _, text := range []string{"Scheduled software review: off. The triage agent wakes only for alerts.", "sudo mmhotline triage-agent run-software-review"} {
		if !strings.Contains(page, text) {
			t.Fatalf("page missing %q: %s", text, page)
		}
	}
	if strings.Contains(page, "Next scheduled software review") {
		t.Fatal("the page gives a next review while the scheduled review is off")
	}
	if strings.Contains(page, `name="software_review_interval_hours"`) {
		t.Fatal("the page still sets the review interval")
	}
	if err := h.app.SetSoftwareReviewInterval(ctx, settings, 6); err != nil {
		t.Fatal(err)
	}
	page = admin.get("/triage-agent", 200)
	for _, text := range []string{"Scheduled software review: every 6 hours.", "Set by the settings file at ", "Next scheduled software review due: at once."} {
		if !strings.Contains(page, text) {
			t.Fatalf("page missing %q after the change: %s", text, page)
		}
	}
	if err := h.app.SetTriageAgentListedHosts(ctx, settings, []SettingsListedHost{{Host: "GIT.Example", Port: 22}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(admin.get("/triage-agent", 200), "git.example") {
		t.Fatal("listed host absent")
	}
	if err := h.app.SetTriageAgentListedHosts(ctx, settings, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(admin.get("/triage-agent", 200), "git.example") {
		t.Fatal("listed host remains")
	}
	const credential = "private-triage-credential"
	if _, err := h.app.StartTriageAgentSetupTest(ctx, settings, "claude_code", "api_key", credential); err != nil {
		t.Fatal(err)
	}
	run := <-runs
	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "https://mmhotline.example/triage-agent", nil)
	addAccessToken(request, "admin@example.test")
	h.app.Handler().ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Refresh") != "5" || !strings.Contains(response.Body.String(), "Latest setup test: running") {
		t.Fatal("running page", response.Body.String())
	}
	sendTriageEvent(t, run, triageagent.Event{Event: "step", Step: &triageagent.Step{Kind: "message", Text: "<script>alert(1)</script> " + credential}})
	cost := 0.123
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: "MMHotline triage agent test passed. " + credential, CodingAgentSessionTotalUsage: &triageagent.Usage{CostInUSDollars: &cost}})
	view, _ := h.app.TriageAgentState(context.Background(), Principal{UnixUser: "fixture"})
	waitTriageRun(t, h.app, view.LatestSetupTest.ID)
	run.connection.Close()
	page = admin.get("/triage-agent", 200)
	for _, text := range []string{"Latest setup test: passed", "Triage agent: Claude Code with an API key, model test-model, reasoning effort high.", "Set up by the settings file at ", "Check the sign-in now", "Cost in US dollars: 0.123000", "&lt;script&gt;alert(1)&lt;/script&gt;"} {
		if !strings.Contains(page, text) {
			t.Fatalf("passed page missing %s: %s", text, page)
		}
	}
	if strings.Contains(page, credential) || strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("unsafe run output")
	}
	response = httptest.NewRecorder()
	h.app.Handler().ServeHTTP(response, request)
	if response.Header().Get("Refresh") != "" {
		t.Fatal("finished page refreshes")
	}
	if _, err := h.app.StartTriageAgentSetupTest(ctx, settings, "codex", "api_key", "other-credential"); err != nil {
		t.Fatal(err)
	}
	run = <-runs
	h.app.triageAgent.resolve = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("fixture DNS refusal") }
	// The connection table says which key of the settings file allows a
	// refused unlisted host.
	client, _, proxyResponse := proxyRequest(t, h.app, run.request.ProxyToken, "CONNECT needed.example:443 HTTP/1.1\r\n\r\n")
	io.ReadAll(proxyResponse.Body)
	client.Close()

	sendTriageEvent(t, run, triageagent.Event{Event: "failed", Reason: "Your account's credit is used up."})
	view, _ = h.app.TriageAgentState(context.Background(), Principal{UnixUser: "fixture"})
	waitTriageRun(t, h.app, view.LatestSetupTest.ID)
	run.connection.Close()
	page = admin.get("/triage-agent", 200)
	for _, text := range []string{"Latest setup test: failed", "Your account&#39;s credit is used up.", "Triage agent: Claude Code with an API key", "To allow the connection, add <code>{host: needed.example, port: 443}</code> to <code>triage_agent.listed_hosts</code> in the settings file.", "Refused:", "Bytes sent", "Bytes received"} {
		if !strings.Contains(page, text) {
			t.Fatalf("failed page missing %s: %s", text, page)
		}
	}
	if err := h.app.SetTriageAgentListedHosts(ctx, settings, []SettingsListedHost{{Host: "needed.example", Port: 443}}); err != nil {
		t.Fatal(err)
	}
	page = admin.get("/triage-agent", 200)
	if strings.Contains(page, "To allow the connection, add") {
		t.Fatal("a listed host is still offered")
	}
	if err := h.app.applyCodingAgentSettings(ctx, triageAgentRole, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(admin.get("/triage-agent", 200), "Triage agent: not set up") {
		t.Fatal("a settings file without triage_agent did not remove the setup")
	}
}
