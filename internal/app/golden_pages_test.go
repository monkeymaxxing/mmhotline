package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/cryptotest"
	"time"

	"mmhotline/internal/cloudflareaccess"
	"mmhotline/internal/fakesendingservices"
	"mmhotline/internal/htmlgolden"
)

// goldenPages lists every HTML render in this package: each renderPage call
// and renderPageError, with the states the golden test covers and the states
// it does not, with the reason. TestGoldenPages renders each covered state
// through the page's real handler and compares it with its golden file under
// testdata/pages/app, apart from whitespace (see package htmlgolden).
var goldenPages = []goldenPage{
	{render: "agent_playbook_rules_http.go renderAgentPlaybookRulesPage (GET /agent-playbook-rules)", page: "agent_playbook_rules",
		states: []string{"default", "preset_and_services_to_approve", "routine_fixes_without_permission"}},
	{render: "alert_source_page_http.go alertSourcePage (GET /alert-sources/{id}, and a planned downtime form error)", page: "alert_source",
		states:     []string{"waiting_for_the_first_check_in", "new_waiting_for_the_first_check_in", "check_ins_arriving", "no_recent_check_in", "receiving_alerts_without_check_ins", "downtime_form_error", "planned_downtime"},
		notCovered: map[string]string{"disabled": "a disabled source leaves the page's list; left out to keep the alerts world short"}},
	{render: "browser_sign_ins.go signInLinkHTTP (GET /sign-in/{token}, used or expired)", page: "sign_in_link_unavailable",
		states: []string{"default"}},
	{render: "commit_reviews_http.go renderCommitReviewPage (GET /approvals/commits/{id})", page: "commit_review",
		states:     []string{"waiting", "reviewed_by_a_person"},
		notCovered: map[string]string{"triage_agent_change_needs_review": "needs the triage agent's signed commit with paths only a person may approve", "reviewed_by_the_triage_agent": "needs a commit review run and its report", "diff_truncated": "needs a diff over 128 KiB", "review_error": "needs ReviewCommit to fail"}},
	{render: "contact_method_type_slack_message_http.go connectSlackHTTP (POST /connect-slack)", page: "connect_slack",
		states: []string{"personal_direct_message", "shared_channel"}},
	{render: "contacts_http.go renderContactMethods (GET /contact-methods and each contact method form)", page: "contact_methods",
		states:     []string{"none", "all_types_offered", "each_type_tests_queued", "each_type_tests_sent_and_one_failing", "seen_by_the_administrator", "saved_notice", "form_error_keeps_the_typed_value"},
		notCovered: map[string]string{"webhook_signing_secret_shown_once": "shows a new random signing secret once", "shared_contact_methods": "only personal ones are made; the administrator's view lists other people's"}},
	{render: "contacts_http.go deliveryPage (GET /deliveries/{id})", page: "delivery",
		states:     []string{"test_call_failed", "test_message_posted", "sos_notice_not_sent"},
		notCovered: map[string]string{"combined_into_another_delivery": "needs several SOS messages combined for one contact method", "retries_with_attempts": "needs a sending service that fails and is retried"}},
	{render: "contacts_http.go connectTelegramHTTP (POST /connect-telegram)", page: "connect_telegram",
		states: []string{"personal_chat", "team_group"}},
	{render: "coverage_http.go coveragePage (GET /on-call)", page: "on_call",
		states:     []string{"rotation", "override", "form_error", "override_and_unavailable_person", "team_mute", "one_person_by_day_of_week"},
		notCovered: map[string]string{"no_schedule": "every rig the test reuses sets up a schedule", "custom_schedule_code": "needs the custom schedule sandbox"}},
	{render: "dashboard.go dashboard (GET /dashboard)", page: "dashboard",
		states:     []string{"quiet", "down_alarm_set_up", "down_alarm_not_checking_in", "settings_part_failed", "open_sos", "claimed_sos", "sos_and_approval_waiting"},
		notCovered: map[string]string{"warned_run_waits": "covered on the Approvals page, not the dashboard", "fleet_view_servers": "needs the fleet view command's socket"}},
	{render: "dashboard.go alertsPage (GET /alerts)", page: "alerts",
		states: []string{"empty", "with_items", "filtered", "more_than_a_page"}},
	{render: "dashboard.go alertPage (GET /alerts/{id})", page: "alert",
		states:     []string{"manual_sos", "warn", "info", "received_later_than_it_happened", "resolved_sos"},
		notCovered: map[string]string{"held_warning": "needs a warning hold (TestHeldWarn* rigs)"}},
	{render: "down_alarm_http.go sendDownAlarmTestHTTP, its error (POST /mmhotline-down-alarm/send-test)", page: "down_alarm_test",
		states: []string{"error_too_soon"}},
	{render: "down_alarm_http.go sendDownAlarmTestHTTP, its results (POST /mmhotline-down-alarm/send-test)", page: "down_alarm_test",
		states:     []string{"nobody_to_reach"},
		notCovered: map[string]string{"results_per_person": "the alarm's list of people syncs to the fake Cloudflare worker only in background work"}},
	{render: "http.go renderSignInUnavailable (through HTTPError with cloudflareaccess.ErrKeysUnavailable)", page: "error",
		states: []string{"sign_ins_cannot_be_checked"}},
	{render: "http.go renderBrowserError (through HTTPError)", page: "error",
		states: []string{"page_not_found", "forbidden_for_a_member", "incident_changed", "invalid", "invalid_with_a_reason", "unauthorized", "forbidden", "not_found", "conflict", "rate_limited", "internal", "conflict_back_to_the_incident", "conflict_back_to_the_triage_session"}},
	{render: "http.go browserAccessError, signed out (any page, 401)", page: "sign_in",
		states: []string{"access_configured", "sign_in_link_only"}},
	{render: "http.go browserAccessError, not a team member (any page, 403)", page: "not_a_team_member",
		states: []string{"default"}},
	{render: "incident_page.go incidentPage (GET /incidents/{id})", page: "incident",
		states:     []string{"unclaimed", "claimed", "resolved", "unclaimed_with_triage_session_and_approval"},
		notCovered: map[string]string{"escalated_to_someone_else": "needs Escalate; left out", "warned_run_waits": "needs the warned-run flow (prepareWarnedRun), a second triage session world"}},
	{render: "incidents_http.go manualSOSHTTP, its error (POST /incidents/new)", page: "manual_sos",
		states: []string{"missing_title"}},
	{render: "people_http.go peoplePage (GET /people)", page: "people",
		states: []string{"three_people", "development_mode_notice", "one_administrator", "red_bar_for_a_waiting_sos"}},
	{render: "playbook_run_approvals_http.go approvalsPageHTTP (GET /approvals)", page: "approvals",
		states: []string{"none", "one_waiting", "waiting_warned_and_decided", "with_a_commit_waiting"}},
	{render: "playbook_run_approvals_http.go renderApprovalRequestPage (GET /approvals/{id}, and a decision's error)", page: "approval_request",
		states:     []string{"waiting", "warned_run", "refused", "ran_and_succeeded", "ran_and_failed", "running", "stale", "error_already_decided"},
		notCovered: map[string]string{"approved_by_the_viewer_waiting_for_a_second": "needs a two-person approval rule", "with_decisions_and_results": "the extra rows copy the waiting request without decisions or result messages"}},
	{render: "sending_services_http.go renderSendingServicesPage (GET /sending-services)", page: "sending_services",
		states: []string{"email_only", "all_set_up", "one_check_failed"}},
	{render: "sending_services_http.go sendingServiceSetupPage (GET /sending-services/{service})", page: "sending_service_setup",
		states:     []string{"mail_server_set_up_without_a_check", "pushover_not_set_up", "pushover_set_up", "twilio_set_up", "telegram_set_up", "slack_set_up", "mail_server_set_up", "pushover_later_check_failed"},
		notCovered: map[string]string{"first_check_failed": "not rendered; TestSendingServiceGuidesExplainMissingSetupAndFailedFirstCheck checks its text"}},
	{render: "sources_http.go sourcesPage (GET /alert-sources)", page: "alert_sources",
		states: []string{"none", "one_from_the_settings_file", "one_with_check_ins", "one_in_planned_downtime"}},
	{render: "status_page_http.go renderStatusPagePage (GET /status-page)", page: "status_page_settings",
		states:     []string{"not_set_up", "set_up", "open_notice", "resolved_notice"},
		notCovered: map[string]string{"post_error": "needs the fake Cloudflare bucket to refuse a post"}},
	{render: "triage_agent_http.go renderTriageAgentPage (GET /triage-agent)", page: "triage_agent",
		states:     []string{"off", "on_with_setup_test_runs", "on_after_a_software_review", "on_with_a_session_run"},
		notCovered: map[string]string{"sign_in_check_failed": "needs a failing setup test run", "action_error": "needs a failing action on the page"}},
	{render: "triage_session_live_view.go triageSessionPage (GET /triage-sessions/{id})", page: "triage_session",
		states:     []string{"running_with_approval_waiting"},
		notCovered: map[string]string{"finished": "the run's end arrives on the sandbox connection and is handled in the background", "stopped_or_taken_over": "same as finished"}},
	{render: "ui.go renderPageError (when reading the frame or running a page's template fails)", page: "error",
		states: []string{"cannot_display_the_page"}},
}

type goldenPage struct {
	// render names the renderPage call: its file, function, and route.
	render, page string
	states       []string
	// notCovered gives, for each state the test does not render, the reason.
	notCovered map[string]string
}

// goldenCSRF is the CSRF cookie every golden request carries, so forms show
// it instead of a random token.
const goldenCSRF = "goldenCSRFgoldenCSRFgoldenCSRFgoldenCSRFgol"

// Pages show random record IDs (NewID) and commit IDs, whose commit dates
// come from the real clock; the golden files hold placeholders, numbered in
// order of first appearance on each page.
var (
	goldenRecordID = regexp.MustCompile(`\b[0-9a-f]{32}\b`)
	goldenCommitID = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	// goldenToken matches a 43-character token (randomToken), such as a
	// Slack connection code or a Telegram start token, with the characters
	// around it: a word of lowercase letters and hyphens is not a token.
	goldenToken = regexp.MustCompile(`(^|[^A-Za-z0-9_-])([A-Za-z0-9_-]{43})($|[^A-Za-z0-9_-])`)
)

// placeholdersForTokens replaces each distinct random token with TOKEN-1,
// TOKEN-2, and so on, in order of first appearance. The golden CSRF token
// stays as it is.
func placeholdersForTokens(page string) string {
	seen := map[string]string{goldenCSRF: goldenCSRF}
	return goldenToken.ReplaceAllStringFunc(page, func(match string) string {
		parts := goldenToken.FindStringSubmatch(match)
		token := parts[2]
		if strings.ToLower(token) == token && !strings.ContainsAny(token, "0123456789") {
			return match
		}
		if _, ok := seen[token]; !ok {
			seen[token] = fmt.Sprintf("TOKEN-%d", len(seen))
		}
		return parts[1] + seen[token] + parts[3]
	})
}

// goldenRendered holds the pages TestGoldenPages rendered, page__state.
var goldenRendered = map[string]bool{}

// goldenRig renders pages through App.Handler, as a browser at
// https://mmhotline.example.com requests them, on a clock the test moves.
type goldenRig struct {
	t        *testing.T
	a        *App
	clock    *atomic.Int64
	rendered map[string]bool
	// replace holds values that change from run to run, such as a fake
	// server's address, and the placeholders the golden files show instead.
	replace []string
}

// goldenTemporaryDirectory matches a test's temporary directory, as the
// settings file's path and the state directory show it.
var goldenTemporaryDirectory = regexp.MustCompile(`/[^\s"<>]*/TestGoldenPages[^\s"<>/]*/\d+`)

// seedGoldenRandom makes crypto/rand repeat the same stream from here on,
// so records listed in the order of their random IDs (NewID), such as
// contact methods, list the same way in each run. The golden files still
// show placeholders, not the IDs. The Cloudflare Access test key is made
// first, outside the stream, as earlier tests in the process may have made
// it already.
func seedGoldenRandom(t *testing.T) {
	accessSigningKey()
	cryptotest.SetGlobalRandom(t, 1)
}

// newGoldenRig renders a's pages with a's clock at the seconds clock holds.
func newGoldenRig(t *testing.T, a *App, clock *atomic.Int64) *goldenRig {
	t.Helper()
	g := &goldenRig{t: t, a: a, clock: clock, rendered: goldenRendered}
	a.Now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	a.WebURL = "https://mmhotline.example.com"
	a.WebListenAddress = "127.0.0.1:8480"
	configureAccessSignIn(t, a)
	// Setup records the moment of installation from the real clock.
	g.exec(`UPDATE triage_agent_internet_access SET changed_at=? WHERE changed_by='installation setup'`, clock.Load())
	return g
}

// newGoldenTeam is the on-call team rig's team: Maya Okafor, the
// administrator, Daniel Reyes, and Priya Natarajan on a weekly rotation in
// New York, on Friday 30 October 2026 at 13:45 there.
func newGoldenTeam(t *testing.T) *goldenRig {
	t.Helper()
	seedGoldenRandom(t)
	r := newOnCallTeamRig(t)
	return newGoldenRig(t, r.h.app, &r.h.clock)
}

func (g *goldenRig) exec(query string, args ...any) {
	g.t.Helper()
	if _, err := g.a.Store.db.Exec(query, args...); err != nil {
		g.t.Fatal(err)
	}
}

// advance moves the clock, so records made one after another have
// different times and a stable order.
func (g *goldenRig) advance(d time.Duration) { g.clock.Add(int64(d / time.Second)) }

// request sends a browser's request as signInEmailAddress ("" for nobody)
// and returns the response.
func (g *goldenRig) request(method, path, signInEmailAddress string, form url.Values) *httptest.ResponseRecorder {
	g.t.Helper()
	var r *http.Request
	if form != nil {
		form.Set("csrf", goldenCSRF)
		r = httptest.NewRequest(method, "https://mmhotline.example.com"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://mmhotline.example.com")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	} else {
		r = httptest.NewRequest(method, "https://mmhotline.example.com"+path, nil)
	}
	r.Header.Set("Accept", "text/html")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: goldenCSRF})
	addAccessToken(r, signInEmailAddress)
	w := httptest.NewRecorder()
	g.a.Handler().ServeHTTP(w, r)
	return w
}

// check renders the page at path as signInEmailAddress and compares it
// with the golden file page__state.html.
func (g *goldenRig) check(page, state, method, path, signInEmailAddress string, form url.Values, wantStatus int) string {
	g.t.Helper()
	w := g.request(method, path, signInEmailAddress, form)
	if w.Code != wantStatus {
		g.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, w.Code, wantStatus, w.Body)
	}
	g.compare(page, state, w.Body.String())
	return w.Body.String()
}

// compare compares a rendered page with the golden file page__state.html.
func (g *goldenRig) compare(page, state, body string) {
	g.t.Helper()
	stable := strings.NewReplacer(g.replace...).Replace(body)
	stable = goldenTemporaryDirectory.ReplaceAllString(stable, "TEMPORARY-DIRECTORY")
	stable = placeholdersForTokens(stable)
	stable = htmlgolden.Placeholders(stable, goldenCommitID, "COMMIT")
	stable = htmlgolden.Placeholders(stable, goldenRecordID, "ID")
	name := page + "__" + state
	if g.rendered[name] {
		g.t.Fatalf("%s rendered twice", name)
	}
	g.rendered[name] = true
	htmlgolden.Check(g.t, htmlgolden.Dir("app"), name, []byte(stable))
}

func (g *goldenRig) get(page, state, path, signInEmailAddress string) string {
	g.t.Helper()
	return g.check(page, state, "GET", path, signInEmailAddress, nil, 200)
}

const (
	maya   = "maya@example.test" // the administrator
	daniel = "daniel@example.test"
)

// Every page renders as its golden file says, apart from whitespace.
func TestGoldenPages(t *testing.T) {
	clear(goldenRendered)
	t.Run("settings pages of a new team", func(t *testing.T) {
		g := newGoldenTeam(t)
		g.get("dashboard", "quiet", "/dashboard", maya)
		g.get("alerts", "empty", "/alerts", maya)
		g.get("people", "three_people", "/people", maya)
		ShowDevelopmentModeNotice("http://127.0.0.1:8490")
		g.get("people", "development_mode_notice", "/people", maya)
		ShowDevelopmentModeNotice("")
		g.get("alert_sources", "none", "/alert-sources", maya)
		g.get("on_call", "rotation", "/on-call", daniel)
		g.get("contact_methods", "none", "/contact-methods", daniel)
		g.get("sending_services", "email_only", "/sending-services", maya)
		g.get("sending_service_setup", "mail_server_set_up_without_a_check", "/sending-services/mail_server", maya)
		g.get("sending_service_setup", "pushover_not_set_up", "/sending-services/pushover", maya)
		g.get("triage_agent", "off", "/triage-agent", maya)
		g.get("agent_playbook_rules", "default", "/agent-playbook-rules", maya)
		g.get("status_page_settings", "not_set_up", "/status-page", maya)
		g.get("approvals", "none", "/approvals", maya)
		g.check("error", "page_not_found", "GET", "/no-such-page", maya, nil, 404)
		g.check("error", "forbidden_for_a_member", "GET", "/triage-agent", daniel, nil, 403)
		g.check("sign_in", "access_configured", "GET", "/dashboard", "", nil, 401)
		verifier := g.a.AccessVerifier
		g.a.AccessVerifier = nil
		g.check("sign_in", "sign_in_link_only", "GET", "/dashboard", "", nil, 401)
		g.a.AccessVerifier = verifier
		g.check("not_a_team_member", "default", "GET", "/dashboard", "stranger@example.test", nil, 403)
		g.check("sign_in_link_unavailable", "default", "GET", "/sign-in/no-such-token", "", nil, 401)
		g.check("manual_sos", "missing_title", "POST", "/incidents/new", maya, url.Values{"title": {""}, "message": {"Line one\n  line two"}}, 400)
	})
	t.Run("a team set up by the settings file", func(t *testing.T) {
		g := newGoldenSettingsFileTeam(t)
		g.get("dashboard", "down_alarm_set_up", "/dashboard", ana)
		g.get("sending_services", "all_set_up", "/sending-services", ana)
		for _, service := range []string{"pushover", "twilio", "telegram", "slack", "mail_server"} {
			g.get("sending_service_setup", service+"_set_up", "/sending-services/"+service, ana)
		}
		g.get("triage_agent", "on_with_setup_test_runs", "/triage-agent", ana)
		g.get("agent_playbook_rules", "preset_and_services_to_approve", "/agent-playbook-rules", ana)
		g.get("status_page_settings", "set_up", "/status-page", ana)
		g.get("alert_sources", "one_from_the_settings_file", "/alert-sources", ana)
		g.get("alert_source", "waiting_for_the_first_check_in", "/alert-sources/monitoring", ana)
		g.get("people", "one_administrator", "/people", ana)
		g.get("contact_methods", "all_types_offered", "/contact-methods", ana)
		g.check("down_alarm_test", "nobody_to_reach", "POST", "/mmhotline-down-alarm/send-test", ana, url.Values{}, 200)

		ctx := context.Background()
		p := Principal{SignInEmailAddress: ana}
		g.advance(time.Minute)
		if _, err := g.a.SaveContactMethod(ctx, p, ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey}, Subscriptions: []string{"sos"}}); err != nil {
			t.Fatal(err)
		}
		g.check("down_alarm_test", "error_too_soon", "POST", "/mmhotline-down-alarm/send-test", ana, url.Values{}, 429)
		g.advance(10 * time.Minute)
		g.get("dashboard", "down_alarm_not_checking_in", "/dashboard", ana)
		g.advance(time.Minute)
		if _, _, err := g.a.PostToStatusPage(ctx, p, 0, "Orders are delayed.\nWe are looking into it."); err != nil {
			t.Fatal(err)
		}
		g.advance(time.Minute)
		if _, _, err := g.a.PostToStatusPage(ctx, p, 1, "We found the cause."); err != nil {
			t.Fatal(err)
		}
		g.get("status_page_settings", "open_notice", "/status-page", ana)
		g.advance(time.Minute)
		if _, _, err := g.a.ResolveStatusPageNotice(ctx, p, 1, "Orders work again."); err != nil {
			t.Fatal(err)
		}
		g.get("status_page_settings", "resolved_notice", "/status-page", ana)

		// A later settings check failed: the earlier setup remains in use.
		g.exec(`UPDATE settings_file_parts SET failure_reason='The sending service refused the credential.',failed_at=? WHERE part='sending_services.pushover'`, g.a.Now().Unix())
		g.get("sending_service_setup", "pushover_later_check_failed", "/sending-services/pushover", ana)
		g.get("sending_services", "one_check_failed", "/sending-services", ana)
		g.get("dashboard", "settings_part_failed", "/dashboard", ana)
	})
	t.Run("contact methods of each type", func(t *testing.T) {
		g, fake := newGoldenTeamWithSendingServices(t)
		ctx := context.Background()
		p := Principal{SignInEmailAddress: daniel}
		save := func(in ContactMethodInput) ContactMethod {
			t.Helper()
			g.advance(time.Second)
			c, err := g.a.SaveContactMethod(ctx, p, in)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		g.check("connect_telegram", "personal_chat", "POST", "/connect-telegram", daniel, url.Values{"telegram_connection": {"personal_chat"}}, 200)
		g.check("connect_telegram", "team_group", "POST", "/connect-telegram", maya, url.Values{"telegram_connection": {"team_group"}}, 200)
		g.check("connect_slack", "personal_direct_message", "POST", "/connect-slack", daniel, url.Values{"slack_connection": {"personal_direct_message"}}, 200)
		g.check("connect_slack", "shared_channel", "POST", "/connect-slack", maya, url.Values{"slack_connection": {"shared_channel"}}, 200)

		save(ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "daniel@example.test"}, Subscriptions: []string{"info", "warn", "sos"}})
		save(ContactMethodInput{ContactMethodType: "pushover_notification", Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey, "pushover_device_name": "phone"}, Subscriptions: []string{"sos"}})
		save(ContactMethodInput{ContactMethodType: "outgoing_webhook", Fields: map[string]string{"outgoing_webhook_url": fake.URL + "/webhook-receiver/ops"}, Subscriptions: []string{"warn", "sos"}})
		fake.SetNumberBehavior("+15555550123", fakesendingservices.NumberInvalid)
		g.advance(time.Second)
		if _, err := g.a.AddPhoneNumber(ctx, p, "+1 555 555 0123"); err != nil {
			t.Fatal(err)
		}
		// Telegram: Daniel presses Start on the link MMHotline made.
		g.advance(time.Second)
		link, err := g.a.StartTelegramConnection(ctx, p, "personal_chat")
		if err != nil {
			t.Fatal(err)
		}
		start, _ := url.Parse(link)
		fake.Fake.TelegramStart(fakesendingservices.TelegramUser{ID: 1001, FirstName: "Daniel"}, fakesendingservices.TelegramChat{ID: 1001, Type: "private", FirstName: "Daniel"}, start.Query().Get("start"))
		telegram := g.a.configuredContactMethodTypes()["telegram_message"].(*TelegramMessageContactMethodType)
		if _, err = g.a.pollTelegramOnce(ctx, telegram, 0, 0, func(err error) { t.Error(err) }); err != nil {
			t.Fatal(err)
		}
		// Slack: Daniel sends the connection code in a direct message.
		g.advance(time.Second)
		slack := g.a.configuredContactMethodTypes()["slack_message"].(*SlackMessageContactMethodType)
		code, err := g.a.StartSlackConnection(ctx, p, "personal_direct_message")
		if err != nil {
			t.Fatal(err)
		}
		// The fake learns the conversation from the message; MMHotline
		// handles the message here, as Socket Mode would hand it over.
		fake.Fake.SlackSendMessage(fakesendingservices.SlackUser{ID: "U0001"}, fakesendingservices.SlackConversation{ID: "D0001"}, code.Code, false)
		if err = g.a.handleSlackEvent(ctx, slack, slackEvent{Type: "message", User: "U0001", Channel: "D0001", ChannelType: "im", Text: code.Code}); err != nil {
			t.Fatal(err)
		}
		g.get("contact_methods", "each_type_tests_queued", "/contact-methods", daniel)
		g.dispatch()
		page := g.get("contact_methods", "each_type_tests_sent_and_one_failing", "/contact-methods", daniel)
		deliveries := regexp.MustCompile(`href="/deliveries/([0-9a-f]{32})"`).FindAllStringSubmatch(page, -1)
		if len(deliveries) < 2 {
			t.Fatalf("%d delivery links", len(deliveries))
		}
		// Newest first: the failed automated voice call, then the Slack message.
		g.get("delivery", "test_call_failed", "/deliveries/"+deliveries[0][1], daniel)
		g.get("delivery", "test_message_posted", "/deliveries/"+deliveries[1][1], daniel)
		g.get("contact_methods", "seen_by_the_administrator", "/contact-methods", maya)
		g.get("contact_methods", "saved_notice", "/contact-methods?done=saved", daniel)
		g.check("contact_methods", "form_error_keeps_the_typed_value", "POST", "/contact-methods/save", daniel, url.Values{"contact_method_type": {"email"}, "email_address": {"not an address"}, "subscriptions": {"sos"}}, 400)
	})
	t.Run("alerts and incidents", func(t *testing.T) {
		g := newGoldenTeam(t)
		ctx := context.Background()
		admin, member := Principal{SignInEmailAddress: maya}, Principal{SignInEmailAddress: daniel}
		source, err := createAlertSource(ctx, g.a, admin, "Monitoring <main>")
		if err != nil {
			t.Fatal(err)
		}
		sourcePath := "/alert-sources/" + source.Source.ID
		g.get("alert_source", "new_waiting_for_the_first_check_in", sourcePath, daniel)
		g.advance(time.Minute)
		if w := checkInHTTP(g.a, admin, source, `{"sent_at":"2026-10-30T17:46:00Z","sent_by_node_name":"util"}`, g.a.Now()); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		g.get("alert_source", "check_ins_arriving", sourcePath, daniel)
		g.get("alert_sources", "one_with_check_ins", "/alert-sources", maya)
		g.advance(3 * time.Minute)
		g.get("alert_source", "no_recent_check_in", sourcePath, daniel)
		admitSignedAlert(t, g.a, source, "info", "Sender is running", "")
		g.get("alert_source", "receiving_alerts_without_check_ins", sourcePath, daniel)
		g.check("alert_source", "downtime_form_error", "POST", sourcePath+"/planned-downtime", daniel, url.Values{"until": {"2026-10-29T15:45"}, "reason": {"Keep my reason"}}, 400)
		if w := g.request("POST", sourcePath+"/planned-downtime", daniel, url.Values{"until": {"2026-10-30T15:45"}, "reason": {"Move monitoring"}}); w.Code != 303 {
			t.Fatal(w.Code, w.Body.String())
		}
		g.get("alert_source", "planned_downtime", sourcePath, daniel)
		g.get("alert_sources", "one_in_planned_downtime", "/alert-sources", daniel)

		g.advance(time.Minute)
		sos, err := g.a.ManualSOS(ctx, admin, "Database down", "Connection refused\n  on db001 & db002 <primary>")
		if err != nil {
			t.Fatal(err)
		}
		incidentPath := "/incidents/" + sos.IncidentID
		g.advance(time.Minute)
		g.get("dashboard", "open_sos", "/dashboard", daniel)
		g.get("incident", "unclaimed", incidentPath, daniel)
		g.get("people", "red_bar_for_a_waiting_sos", "/people", daniel)
		g.get("alert", "manual_sos", "/alerts/"+sos.ID, daniel)
		for _, alert := range []struct{ severity, title, key string }{
			{"warn", "Disk 85% full on app002", "app002/disk/root_disk_usage"},
			{"info", "Deployment finished", ""},
			{"sos", "Database still down", ""},
		} {
			g.advance(time.Minute)
			admitted := admitSignedAlert(t, g.a, source, alert.severity, alert.title, alert.key)
			if alert.severity == "warn" {
				g.get("alert", "warn", "/alerts/"+admitted.ID, daniel)
			}
			if alert.severity == "info" {
				g.get("alert", "info", "/alerts/"+admitted.ID, daniel)
			}
		}
		g.get("alerts", "with_items", "/alerts", daniel)
		g.get("alerts", "filtered", "/alerts?q=down&severity=sos&from=2026-10-30&until=2026-10-30", daniel)

		i, err := g.a.GetIncident(ctx, member, sos.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		g.advance(time.Minute)
		if i, err = g.a.ClaimIncident(ctx, member, i.ID, i.Revision, "Looking at db001."); err != nil {
			t.Fatal(err)
		}
		g.advance(time.Minute)
		g.get("incident", "claimed", incidentPath, daniel)
		g.get("dashboard", "claimed_sos", "/dashboard", maya)
		g.advance(time.Minute)
		if _, err = g.a.ResolveIncident(ctx, member, i.ID, i.Revision, "Restarted the primary."); err != nil {
			t.Fatal(err)
		}
		g.advance(time.Minute)
		g.get("incident", "resolved", incidentPath, daniel)
		g.check("error", "incident_changed", "POST", incidentPath+"/claim", daniel, url.Values{"revision": {"1"}}, 409)

		// More than a page of alerts: the history links to older alerts.
		for n := range 30 {
			g.advance(time.Second)
			admitSignedAlert(t, g.a, source, "info", fmt.Sprintf("Backup %d finished", n+1), "")
		}
		g.get("alerts", "more_than_a_page", "/alerts?q=Backup", daniel)

		// Priya takes on call from the schedule for two hours.
		if w := g.request("POST", "/on-call", "priya@example.test", url.Values{"action": {"claim_on_call"}, "revision": {"0"}, "duration": {"2h"}, "take_from_member_id": {""}}); w.Code != 303 {
			t.Fatal(w.Code, w.Body.String())
		}
		g.get("on_call", "override", "/on-call", daniel)
		g.check("on_call", "form_error", "POST", "/on-call", daniel, url.Values{"action": {"unavailable"}, "revision": {"0"}, "duration": {"forever"}}, 400)
		if w := g.request("POST", "/on-call", daniel, url.Values{"action": {"unavailable"}, "revision": {"0"}, "duration": {"3h"}}); w.Code != 303 {
			t.Fatal(w.Code, w.Body.String())
		}
		g.get("on_call", "override_and_unavailable_person", "/on-call", daniel)

		// An alert that happened well before it arrived, then an SOS with a
		// sender incident key and its resolution.
		admit := func(fields string) Alert {
			t.Helper()
			g.advance(time.Minute)
			alert, _, _, err := g.a.AdmitGeneric(ctx, source.Source.ID, alertSignature(source.Secret, g.a.Now(), []byte(fields)), []byte(fields))
			if err != nil {
				t.Fatal(err)
			}
			return alert
		}
		late := admit(`{"sender_alert_id":"queue-17","severity":"warn","title":"Queue backed up","message":"Queue depth 9000.","happened_at":"` + g.a.Now().Add(-40*time.Minute).Format(time.RFC3339) + `"}`)
		g.get("alert", "received_later_than_it_happened", "/alerts/"+late.ID, daniel)
		admit(`{"sender_incident_key":"db/primary","severity":"sos","title":"Primary database down","message":"No connections."}`)
		resolved := admit(`{"sender_incident_key":"db/primary","severity":"resolved-sos","title":"Primary database up","message":"Connections work again."}`)
		g.get("alert", "resolved_sos", "/alerts/"+resolved.ID, daniel)

		// Maya mutes the team for an hour.
		if w := g.request("POST", "/on-call", maya, url.Values{"action": {"team_mute"}, "revision": {"0"}, "duration": {"1h"}}); w.Code != 303 {
			t.Fatal(w.Code, w.Body.String())
		}
		g.get("on_call", "team_mute", "/on-call", maya)
	})
	t.Run("the triage agent, approvals, and commit reviews", func(t *testing.T) {
		seedGoldenRandom(t)
		r := newTriageSessionRig(t, true)
		clock := &atomic.Int64{}
		clock.Store(r.a.Now().Unix())
		g := newGoldenRig(t, r.a, clock)
		responder := r.p.SignInEmailAddress
		// The rig adds its two contact methods in map order, so their
		// random IDs, which order deliveries made at the same second, come
		// in either order. Make them again, in one order.
		member, err := r.a.CurrentMember(context.Background(), r.p)
		if err != nil {
			t.Fatal(err)
		}
		g.exec(`DELETE FROM contact_method_subscriptions`)
		g.exec(`DELETE FROM contact_methods`)
		for _, method := range []struct {
			contactMethodType string
			severities        []string
		}{{"email", []string{"warn", "sos"}}, {"test_waking", []string{"sos"}}} {
			id := NewID()
			g.exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) VALUES(?,?,?,'{}',?,1,1,0)`, id, member.ID, method.contactMethodType, method.contactMethodType+"@example.test")
			for _, severity := range method.severities {
				g.exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,?)`, id, severity)
			}
		}
		g.get("triage_agent", "on_after_a_software_review", "/triage-agent", responder)
		prepareApprovals(t, r.a, "")
		r.a.triageAgent.playbookRunner = &fakeRunner{calls: make(chan fakeRunnerCall, 4)}
		sos, _, _ := approvalSession(t, r)
		g.replaceRepositoryCommits(filepath.Join(r.a.Store.Dir, "systems_repository"))
		g.advance(time.Minute)
		waiting := onlyApprovalRecord(t, r.a)
		var sessionID string
		if err := r.a.Store.db.QueryRow(`SELECT id FROM triage_sessions`).Scan(&sessionID); err != nil {
			t.Fatal(err)
		}
		g.get("dashboard", "sos_and_approval_waiting", "/dashboard", responder)
		g.get("incident", "unclaimed_with_triage_session_and_approval", "/incidents/"+sos.IncidentID, responder)
		g.get("triage_session", "running_with_approval_waiting", "/triage-sessions/"+sessionID, responder)
		g.get("approvals", "one_waiting", "/approvals", responder)
		g.get("approval_request", "waiting", "/approvals/"+waiting.id, responder)
		g.get("triage_agent", "on_with_a_session_run", "/triage-agent", responder)
		g.get("on_call", "one_person_by_day_of_week", "/on-call", responder)
		incidentPage := g.request("GET", "/incidents/"+sos.IncidentID, responder, nil).Body.String()
		delivery := regexp.MustCompile(`href="/deliveries/([0-9a-f]{32})"`).FindStringSubmatch(incidentPage)
		if delivery == nil {
			t.Fatal("the incident page links to no delivery")
		}
		g.get("delivery", "sos_notice_not_sent", "/deliveries/"+delivery[1], responder)
		g.get("agent_playbook_rules", "routine_fixes_without_permission", "/agent-playbook-rules", responder)

		// More playbook runs of the same request, as TestApprovalsListOrder
		// adds them: two warned runs and a refused request.
		for _, row := range []struct {
			id, playbook, setting, state    string
			waitMinutes, judgedSecondsLater int
		}{
			{"warned_late", "one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml", warnWaitThenAct, approvalOpen, 30, 0},
			{"warned_soon", "one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml", warnWaitThenAct, approvalOpen, 5, 60},
			{"refused_one", "playbooks/rotate_logs.yml", getPermissionThenAct, approvalRefused, 0, 0},
			{"ran_one", "playbooks/restart_nginx.yml", getPermissionThenAct, approvalRanAndSucceeded, 0, 0},
			{"failed_one", "playbooks/renew_certificates.yml", getPermissionThenAct, approvalRanAndFailed, 0, 0},
			{"running_one", "playbooks/restart_postgres.yml", getPermissionThenAct, approvalRunning, 0, 0},
			{"stale_one", "playbooks/clean_tmp.yml", getPermissionThenAct, approvalStale, 0, 0},
		} {
			g.exec(`INSERT INTO triage_agent_playbook_runs_waiting_for_a_person(id,run_id,request_id,triage_session_id,playbook,inventory,commit_id,action_kinds,playbook_text,mmx_explain_output,rule,warning_wait_minutes,approvals_needed,judged_at,incident_id,state,state_changed_at)
			SELECT ?,run_id,request_id,triage_session_id,?,inventory,commit_id,action_kinds,playbook_text,mmx_explain_output,?,?,1,judged_at+?,incident_id,?,state_changed_at FROM triage_agent_playbook_runs_waiting_for_a_person WHERE id=?`,
				row.id, row.playbook, row.setting, row.waitMinutes, row.judgedSecondsLater, row.state, waiting.id)
		}
		g.get("approvals", "waiting_warned_and_decided", "/approvals", responder)
		g.get("approval_request", "warned_run", "/approvals/warned_soon", responder)
		g.get("approval_request", "refused", "/approvals/refused_one", responder)
		g.get("approval_request", "ran_and_succeeded", "/approvals/ran_one", responder)
		g.get("approval_request", "ran_and_failed", "/approvals/failed_one", responder)
		g.get("approval_request", "running", "/approvals/running_one", responder)
		g.get("approval_request", "stale", "/approvals/stale_one", responder)
		g.check("approval_request", "error_already_decided", "POST", "/approvals/refused_one/approve", responder, url.Values{}, 409)

		// A commit no trusted key signed waits for a person's review; an
		// earlier one a person reviewed.
		diff := "diff --git a/inventory.yml b/inventory.yml\n--- a/inventory.yml\n+++ b/inventory.yml\n@@ -1,2 +1,3 @@\n app002:\n+  ansible_host: 10.0.0.12\n"
		g.exec(`INSERT INTO triage_agent_refused_commits(commit_id,reason_kind,paths_json,changed_paths_json,author,committed_at,message,diff,diff_truncated,found_at) VALUES(?,'not_signed_by_a_trusted_key','[]','["inventory.yml"]','Someone <someone@example.test>',?,'Add app002 <host>',?,0,?)`, strings.Repeat("a", 40), g.a.Now().Add(-time.Hour).Unix(), diff, g.a.Now().Unix())
		g.exec(`INSERT INTO triage_agent_refused_commits(commit_id,reason_kind,paths_json,changed_paths_json,author,committed_at,message,diff,diff_truncated,found_at,reviewed_at,reviewed_by_member_id,reviewer_kind) VALUES(?,'not_signed_by_a_trusted_key','[]','["README.md"]','Someone <someone@example.test>',?,'Describe the servers','',0,?,?,?,'member')`, strings.Repeat("b", 40), g.a.Now().Add(-2*time.Hour).Unix(), g.a.Now().Add(-time.Hour).Unix(), g.a.Now().Unix(), member.ID)
		g.get("commit_review", "waiting", "/approvals/commits/"+strings.Repeat("a", 40), responder)
		g.get("commit_review", "reviewed_by_a_person", "/approvals/commits/"+strings.Repeat("b", 40), responder)
		g.get("approvals", "with_a_commit_waiting", "/approvals", responder)
	})
	t.Run("error pages outside a signed-in page", func(t *testing.T) {
		// HTTPError and renderPageError render these for a browser's page
		// request before or without a member's frame, as when reading the
		// frame fails.
		g := &goldenRig{t: t, rendered: goldenRendered}
		render := func(state, method, path string, do func(w http.ResponseWriter)) {
			t.Helper()
			r := httptest.NewRequest(method, "https://mmhotline.example.com"+path, nil)
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			if id := strings.TrimPrefix(path, "/incidents/"); id != path {
				r.SetPathValue("id", strings.TrimSuffix(id, "/claim"))
			}
			if id := strings.TrimPrefix(path, "/triage-sessions/"); id != path {
				r.SetPathValue("triage_session_id", strings.TrimSuffix(id, "/stop"))
			}
			w := httptest.NewRecorder()
			do(&navigationPageWriter{ResponseWriter: w, request: r})
			g.compare("error", state, w.Body.String())
		}
		for state, err := range map[string]error{
			"invalid":                    ErrInvalid,
			"invalid_with_a_reason":      fmt.Errorf("%w: invalid date", ErrInvalid),
			"unauthorized":               ErrUnauthorized,
			"forbidden":                  ErrForbidden,
			"not_found":                  ErrNotFound,
			"conflict":                   ErrConflict,
			"rate_limited":               ErrRateLimited,
			"internal":                   errors.New("private database failure"),
			"sign_ins_cannot_be_checked": cloudflareaccess.ErrKeysUnavailable,
		} {
			render(state, "GET", "/alerts/missing", func(w http.ResponseWriter) { HTTPError(w, err) })
		}
		render("conflict_back_to_the_incident", "POST", "/incidents/0123456789abcdef0123456789abcdef/claim", func(w http.ResponseWriter) { HTTPError(w, ErrConflict) })
		render("conflict_back_to_the_triage_session", "POST", "/triage-sessions/0123456789abcdef0123456789abcdef/stop", func(w http.ResponseWriter) { HTTPError(w, ErrConflict) })
		render("cannot_display_the_page", "GET", "/dashboard", func(w http.ResponseWriter) {
			renderPageError(w, "MMHotline could not display the requested page.", 500)
		})
	})
	checkGoldenCoverage(t, goldenRendered)
}

// goldenFake is the fake sending services and its address.
type goldenFake struct {
	Fake *fakesendingservices.Server
	URL  string
}

func (f goldenFake) SetNumberBehavior(number, behavior string) {
	f.Fake.SetNumberBehavior(number, behavior)
}

// newGoldenTeamWithSendingServices is the on-call team with Pushover,
// Twilio, Telegram, and Slack set up against the fake sending services, and
// every contact method type offered.
func newGoldenTeamWithSendingServices(t *testing.T) (*goldenRig, goldenFake) {
	t.Helper()
	g := newGoldenTeam(t)
	fake := goldenFake{Fake: fakesendingservices.New()}
	server := httptest.NewServer(fake.Fake)
	t.Cleanup(server.Close)
	fake.URL = server.URL
	fake.Fake.SetPhoneClock(g.a.Now)
	fake.Fake.SetPushoverClock(g.a.Now)
	g.replace = append(g.replace, server.URL, "FAKE-SENDING-SERVICES")
	ctx := context.Background()
	if err := g.a.LoadSendingServiceSetups(ctx, nil, server.URL); err != nil {
		t.Fatal(err)
	}
	admin := Principal{SignInEmailAddress: maya}
	for service, fields := range map[string]map[string]string{
		"pushover": {"pushover_application_token": fakesendingservices.PushoverApplicationToken},
		"twilio":   fakePhoneSetupFields(),
		"telegram": {"telegram_bot_token": "fake-telegram-bot-token"},
		"slack":    slackSetupFields(),
	} {
		if _, err := g.a.SetUpSendingService(ctx, admin, service, fields); err != nil {
			t.Fatal(service, err)
		}
	}
	types := maps.Clone(g.a.configuredContactMethodTypes())
	types["outgoing_webhook"] = OutgoingWebhookContactMethodType{AllowedPrivateURLPrefix: server.URL + "/webhook-receiver/"}
	// Email goes to a stand-in that accepts each message, not a mail server.
	types["email"] = &workflowEmailSink{}
	g.a.ContactMethodTypes = types
	return g, fake
}

// replaceRepositoryCommits has the golden files show placeholders for the
// commits of the git repository in directory and their times: git dates
// each commit by the real clock, so commit IDs, full and short (12 and 7
// characters), and commit times change from run to run.
func (g *goldenRig) replaceRepositoryCommits(directory string) {
	g.t.Helper()
	log := testGit(g.t, directory, "log", "--all", "--format=%H %ct %at")
	var full, short, times []string
	for n, line := range strings.Split(strings.TrimSpace(log), "\n") {
		fields := strings.Fields(line)
		name := fmt.Sprintf("REPOSITORY-COMMIT-%d", n+1)
		full = append(full, fields[0], name)
		short = append(short, fields[0][:12], name+"-SHORT-12", fields[0][:7], name+"-SHORT-7")
		for _, seconds := range fields[1:] {
			at, err := strconv.ParseInt(seconds, 10, 64)
			if err != nil {
				g.t.Fatal(err)
			}
			times = append(times, pageClock(time.Unix(at, 0), g.a.Now(), time.UTC), "REPOSITORY-COMMIT-TIME")
		}
	}
	// A Replacer tries its strings in argument order: full IDs first.
	g.replace = append(append(append(g.replace, full...), short...), times...)
}

// dispatch runs the delivery worker until nothing more is due now.
func (g *goldenRig) dispatch() {
	g.t.Helper()
	for range 50 {
		// Each send gets its own second, so records sort the same way each run.
		g.advance(time.Second)
		if _, err := g.a.ProcessPhoneCall(context.Background()); err != nil {
			g.t.Fatal(err)
		}
		work, err := g.a.DispatchOne(context.Background())
		if err != nil {
			g.t.Fatal(err)
		}
		if !work {
			return
		}
	}
	g.t.Fatal("the delivery worker never ran out of work")
}

const ana = "ana@example.com" // the settings file team's administrator

// newGoldenSettingsFileTeam is the example settings file's team, with all
// five sending services, applied as serve applies it at start, against the
// fake sending services, a fake Cloudflare account, and a fake sandbox, on
// Friday 30 October 2026 at 13:45 in New York.
func newGoldenSettingsFileTeam(t *testing.T) *goldenRig {
	t.Helper()
	seedGoldenRandom(t)
	smtp := newSMTPFixture(t, "250 accepted", "", false, nil)
	f := newSettingsFileFixture(t, fiveSendingServices(t, smtp.server.Addr().String()))
	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, 10, 30, 17, 45, 0, 0, time.UTC).Unix())
	g := newGoldenRig(t, f.a, clock)
	f.start()
	g.replace = []string{smtp.server.Addr().String(), "SMTP-SERVER", f.fakes.sendingServicesURL, "FAKE-SENDING-SERVICES", f.fakes.cloudflare.URL, "FAKE-CLOUDFLARE"}
	return g
}

// checkGoldenCoverage checks that each state goldenPages lists as covered
// was rendered, and that each rendered page is listed.
func checkGoldenCoverage(t *testing.T, rendered map[string]bool) {
	t.Helper()
	listed := map[string]bool{}
	for _, page := range goldenPages {
		for _, state := range page.states {
			listed[page.page+"__"+state] = true
			if !rendered[page.page+"__"+state] {
				t.Errorf("%s: the state %s is listed as covered but not rendered", page.render, state)
			}
		}
	}
	for name := range rendered {
		if !listed[name] {
			t.Errorf("%s is rendered but not listed in goldenPages", name)
		}
	}
}
