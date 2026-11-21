package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakecloudflare"
	"mmhotline/internal/statuspage"
)

func pointerTo[T any](value T) *T { return &value }

func statusPageApp(t *testing.T) (*App, *fakecloudflare.Server) {
	t.Helper()
	a := foundationApp(t)
	fake := fakecloudflare.New(t)
	a.Cloudflare = fake.Addresses()
	a.UseSettingsFile(InstalledSettingsFilePath, nil)
	return a, fake
}

// setUpStatusPageFromALaptop runs status-page set-up as the laptop does,
// for the host name, against the fake Cloudflare API.
func setUpStatusPageFromALaptop(fake *fakecloudflare.Server, hostName string) (StatusPageCloudflareSetUp, error) {
	return SetUpStatusPageInCloudflare(context.Background(), fake.Addresses(), fake.AccountID, hostName, fake.APIToken)
}

const statusPageBucket = "mmhotline-status-status-example-com-18a8ea11"

func TestStatusPageSetUpInCloudflareIsRepeatableAndKeepsNoToken(t *testing.T) {
	_, fake := statusPageApp(t)
	result, err := setUpStatusPageFromALaptop(fake, "Status.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	raw := string(encoded)
	if result.PageURL != "https://status.example.com/" || result.BucketName != statusPageBucket || result.R2BucketURL != fake.Addresses().R2BucketURL(fake.AccountID, statusPageBucket) || !strings.Contains(result.NextStep, "Object Read & Write for the bucket "+statusPageBucket+" only") {
		t.Fatalf("set-up result %s", raw)
	}
	assertCommandKeys(t, raw, "page_url", "bucket_name", "r2_bucket_url", "next_step")
	if domains := fake.CustomDomains(statusPageBucket); len(domains) != 1 || domains[0] != "status.example.com" {
		t.Fatalf("custom domains %v", domains)
	}
	rules := fake.TransformRules("zone-example-com")
	if len(rules) != 1 || rules[0].Expression != `(http.host eq "status.example.com" and http.request.uri.path eq "/")` || rules[0].Action != "rewrite" {
		t.Fatalf("rewrite rules %+v", rules)
	}
	path, _ := rules[0].ActionParameters["uri"].(map[string]any)["path"].(map[string]any)
	if path["value"] != "/index.html" {
		t.Fatalf("rewrite to %v", path)
	}
	callsAfterFirst := len(fake.Calls())
	if _, err := setUpStatusPageFromALaptop(fake, "status.example.com"); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.Calls()[callsAfterFirst:] {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("a repeated set-up changed something: %s", call)
		}
	}
	if len(fake.TransformRules("zone-example-com")) != 1 || len(fake.CustomDomains(statusPageBucket)) != 1 {
		t.Fatal("a repeated set-up added a second rule or host name")
	}
	if strings.Contains(raw, fake.APIToken) || strings.Contains(result.Text(), fake.APIToken) {
		t.Fatal("the API token appears in the result")
	}
	// Set-up publishes no page: writing to the bucket needs the R2 key the
	// person creates next.
	if _, published := fake.Object(statusPageBucket, "status.json"); published {
		t.Fatal("set-up published a page")
	}
}

// TestCloudflareNamesDifferForEachHostName checks the names of the
// MMHotline-down alarm's Worker and the status page's bucket. The same host name gives
// the same name, so a set-up after a lost database finds its own Worker and
// bucket; host names that differ only in punctuation get different names, so
// a second installation in the account never takes over the first one's.
// Each name keeps the rules of a workers.dev Worker name and an R2 bucket
// name: at most 63 lowercase letters, digits, and hyphens, starting and
// ending with a letter or digit.
func TestCloudflareNamesDifferForEachHostName(t *testing.T) {
	digest := func(hostName string) string {
		sum := sha256.Sum256([]byte(hostName))
		return hex.EncodeToString(sum[:])[:8]
	}
	if got, want := downAlarmWorkerName("https://Ops.Example.com"), "mmhotline-down-alarm-ops-example-com-"+digest("ops.example.com"); got != want {
		t.Errorf("Worker name %q, want %q", got, want)
	}
	if got, want := statusPageBucketName("status.example.com"), "mmhotline-status-status-example-com-"+digest("status.example.com"); got != want {
		t.Errorf("bucket name %q, want %q", got, want)
	}
	if downAlarmWorkerName("https://ops.eu.example.com") == downAlarmWorkerName("https://ops-eu.example.com") {
		t.Error("ops.eu.example.com and ops-eu.example.com get the same Worker")
	}
	if statusPageBucketName("status.eu.example.com") == statusPageBucketName("status-eu.example.com") {
		t.Error("status.eu.example.com and status-eu.example.com get the same bucket")
	}
	long := strings.Repeat("a", 60) + ".example.com"
	otherLong := strings.Repeat("a", 60) + ".example.org"
	// The readable part of this one is cut just after a hyphen.
	cutAtHyphen := "abcdefghij.abcdefghij.abcdefghij.a.example.com"
	names := []string{downAlarmWorkerName("https://" + long), downAlarmWorkerName("https://" + otherLong), statusPageBucketName(long), statusPageBucketName(otherLong),
		downAlarmWorkerName("https://" + cutAtHyphen), statusPageBucketName(cutAtHyphen), downAlarmWorkerName("http://[::1]:8080")}
	if names[0] == names[1] || names[2] == names[3] {
		t.Errorf("long host names share a name: %q", names)
	}
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	for _, name := range names {
		if !valid.MatchString(name) || strings.Contains(name, "--") {
			t.Errorf("name %q breaks Cloudflare's naming rules", name)
		}
	}
}

func TestStatusPageSetUpKeepsTheZonesOtherRewriteRules(t *testing.T) {
	_, fake := statusPageApp(t)
	for _, hostName := range []string{"other.example.com", "status.example.com"} {
		if _, err := setUpStatusPageFromALaptop(fake, hostName); err != nil {
			t.Fatal(err)
		}
	}
	rules := fake.TransformRules("zone-example-com")
	if len(rules) != 2 || !strings.Contains(rules[0].Expression, "other.example.com") || !strings.Contains(rules[1].Expression, "status.example.com") {
		t.Fatalf("rules %+v", rules)
	}
	if domains := fake.CustomDomains(statusPageBucket); len(domains) != 1 {
		t.Fatalf("host names of the page's bucket: %v", domains)
	}
}

func TestStatusPageSetUpRefusals(t *testing.T) {
	a, fake := statusPageApp(t)
	fake.RefuseNext("POST /accounts/"+fake.AccountID+"/r2/buckets", 1)
	if _, err := setUpStatusPageFromALaptop(fake, "status.example.com"); err == nil || !strings.Contains(err.Error(), "Cloudflare refused to create R2 bucket "+statusPageBucket+" (HTTP 403, error 10000): Authentication error. Check the API token's permissions") {
		t.Fatalf("refused bucket creation: %v", err)
	}
	if _, err := setUpStatusPageFromALaptop(fake, "status.elsewhere.org"); err == nil || !strings.Contains(err.Error(), "no zone in Cloudflare account") {
		t.Fatalf("host name outside the account: %v", err)
	}
	for _, input := range [][3]string{{"not-an-id", "status.example.com", fake.APIToken}, {fake.AccountID, "localhost", fake.APIToken}, {fake.AccountID, "status.example.com", ""}} {
		if _, err := SetUpStatusPageInCloudflare(context.Background(), fake.Addresses(), input[0], input[1], input[2]); err == nil || strings.HasPrefix(err.Error(), "invalid") {
			t.Fatalf("%q: %v", input, err)
		}
	}
	if _, raw := commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: StatusPageCommandRequest{Title: pointerTo("Hello"), Message: pointerTo("Hello")}}, 409); !strings.Contains(raw, "the status page is not set up; an administrator adds the status_page section to the settings file") {
		t.Fatalf("post before set-up: %s", raw)
	}
}

// applyStatusPage applies the settings file's status_page section, as its
// part does at each apply that changed it, and returns the part's failure
// reason, empty when it applied.
func applyStatusPage(t *testing.T, a *App, page *SettingsStatusPage) string {
	t.Helper()
	return settingsPartFailureReason(settingsPartNamed(t, "status_page").apply(context.Background(), a, Settings{StatusPage: page}), nil)
}

// setUpStatusPageWithKey sets the status page up in Cloudflare, as the
// laptop does, then applies the settings file's status_page section: a
// wrong R2 key fails the part and is not saved, and the right one posts
// the current page and saves it.
func setUpStatusPageWithKey(t *testing.T, a *App, fake *fakecloudflare.Server) {
	t.Helper()
	cloudflare, err := setUpStatusPageFromALaptop(fake, "status.example.com")
	if err != nil {
		t.Fatal(err)
	}
	page := &SettingsStatusPage{PageTitle: "Example Inc.", HostName: "status.example.com", R2BucketURL: cloudflare.R2BucketURL, R2AccessKeyID: "anotherkey", R2SecretAccessKey: "secret"}
	if reason := applyStatusPage(t, a, page); !strings.Contains(reason, "posting the current status page with the settings file's R2 key failed") {
		t.Fatalf("wrong key: %s", reason)
	}
	if result, _ := commandRequest(t, a, CommandRequest{Group: "status-page", Action: "show"}, 200); result.StatusPage.IsSetUp {
		t.Fatal("a wrong key was saved")
	}
	page.R2AccessKeyID, page.R2SecretAccessKey = fake.R2AccessKeyID, "fake-r2-secret-access-key"
	if reason := applyStatusPage(t, a, page); reason != "" {
		t.Fatal(reason)
	}
	result, raw := commandRequest(t, a, CommandRequest{Group: "status-page", Action: "show"}, 200)
	if result.StatusPage.PublishedStatusJSON == nil || result.StatusPage.PublishedStatusJSON.PageTitle != "Example Inc." || result.StatusPage.PublishedStatusJSON.HostName != "status.example.com" || result.StatusPage.SetUpBy != "the settings file "+InstalledSettingsFilePath || result.StatusPage.BucketName != statusPageBucket || strings.Contains(raw, "fake-r2-secret-access-key") {
		t.Fatalf("status-page show %s", raw)
	}
	index, ok := fake.Object(statusPageBucket, "index.html")
	if !ok || !strings.Contains(string(index.Content), "Everything is working.") || !strings.Contains(string(index.Content), "Example Inc.") {
		t.Fatalf("first page: %q", index.Content)
	}
	// The apply uploads the Atom feed with the page, with links from host_name.
	feed, ok := fake.Object(statusPageBucket, "feed.xml")
	if !ok || feed.ContentType != "application/atom+xml; charset=utf-8" || !strings.Contains(string(feed.Content), `href="https://status.example.com/feed.xml"`) || !strings.Contains(string(index.Content), `href="feed.xml"`) {
		t.Fatalf("first feed: %q", feed.Content)
	}
}

func TestStatusPagePostsFromTheIncidentPageAndTheStatusPagePage(t *testing.T) {
	a, fake := statusPageApp(t)
	memberFixture(t, a, "admin@example.com")
	member := memberFixture(t, a, "responder@example.com")
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)
	incident, err := a.ManualSOS(context.Background(), member.Principal, "Sign-in fails", "")
	if err != nil {
		t.Fatal(err)
	}
	incidentPath := "/incidents/" + incident.IncidentID
	if body := getPage(t, client, server.URL+incidentPath); strings.Contains(body, "Post to the status page") {
		t.Fatal("the incident page offers the status page before it is set up")
	}
	setUpStatusPageWithKey(t, a, fake)
	body := getPage(t, client, server.URL+incidentPath)
	if !strings.Contains(body, "Post to the status page") {
		t.Fatalf("incident page lacks the button:\n%s", body)
	}
	token := pageToken(t, client, server.URL, incidentPath)
	response := postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "title": {"Sign-in fails"}, "impact": {"outage"}, "message": {StatusPageGenericMessage}}, 303)
	if response.Header.Get("Location") != "/status-page" {
		t.Fatalf("redirect to %s", response.Header.Get("Location"))
	}
	body = getPage(t, client, server.URL+"/status-page")
	for _, want := range []string{"https://status.example.com/", "Sign-in fails", "Notice 1, opened", StatusPageGenericMessage, "their own time zone", "Add an update", "Resolve this notice", `<option value="investigating">Investigating</option>`, `value="outage" selected`} {
		if !strings.Contains(body, want) {
			t.Errorf("status page page lacks %q", want)
		}
	}
	token = pageToken(t, client, server.URL, "/status-page")
	postForm(t, client, server.URL, "/status-page/notices/1/updates", url.Values{"csrf": {token}, "stage": {"identified"}, "message": {"Sign-in fails for some customers."}}, 303)
	postForm(t, client, server.URL, "/status-page/notices/1/resolve", url.Values{"csrf": {token}, "closing_message": {"Sign-in works again."}}, 303)
	file := publishedStatusFile(t, fake)
	notice := file.Notices[0]
	if notice.ResolvedAt == nil || len(notice.Updates) != 3 || notice.Updates[1].Message != "Sign-in fails for some customers." || notice.Updates[2].Message != "Sign-in works again." {
		t.Fatalf("published notice %+v", notice)
	}
	// A new notice starts at the default stage, an update at its chosen stage,
	// and a closing message at resolved.
	if notice.Title != "Sign-in fails" || notice.Impact != statuspage.ImpactOutage || notice.Updates[0].Stage != statuspage.StageInvestigating || notice.Updates[1].Stage != statuspage.StageIdentified || notice.Updates[2].Stage != statuspage.StageResolved {
		t.Fatalf("published notice %+v", notice)
	}
	response = postForm(t, client, server.URL, "/status-page/notices/1/updates", url.Values{"csrf": {token}, "message": {"Late."}}, 400)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(data), "notice 1 is resolved; post a new notice instead") {
		t.Fatalf("update to a resolved notice: %s", data)
	}
	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {"wrong"}, "title": {"x"}, "message": {"x"}}, 403)
}

func TestStatusPageCommandPostAndResolve(t *testing.T) {
	a, fake := statusPageApp(t)
	setUpStatusPageWithKey(t, a, fake)
	post := func(request StatusPageCommandRequest, want int) (CommandResult, string) {
		t.Helper()
		return commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: request}, want)
	}
	// Without --impact, a new notice is an outage.
	result, raw := post(StatusPageCommandRequest{Title: pointerTo("Database move"), Message: pointerTo("We are moving the database.")}, 200)
	if result.StatusPageNoticeID != 1 || !strings.Contains(result.Text(), "Open notice 1, outage: Database move") || !strings.Contains(result.Text(), "Update 1, at") {
		t.Fatalf("post: %s\n%s", raw, result.Text())
	}
	assertCommandKeys(t, raw, "notice_id", "status_page", "notices", "page_title", "host_name", "published_at", "next_notice_id", "title", "impact", "update_id")
	result, _ = post(StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), Stage: pointerTo("monitoring"), Message: pointerTo("The replica is up.")}, 200)
	if !strings.Contains(result.Text(), "Update 2, monitoring, at") || !strings.Contains(result.Text(), "The replica is up.") {
		t.Fatalf("update:\n%s", result.Text())
	}
	// A maintenance notice with its planned window.
	result, _ = post(StatusPageCommandRequest{Title: pointerTo("Network work"), Impact: pointerTo("maintenance"), Stage: pointerTo("scheduled"), Message: pointerTo("A switch is replaced."),
		ScheduledStartAt: pointerTo("2026-11-22T02:00:00+01:00"), ScheduledEndAt: pointerTo("2026-11-22T03:00:00Z")}, 200)
	if !strings.Contains(result.Text(), "Open notice 2, maintenance: Network work") || !strings.Contains(result.Text(), "Planned start 2026-11-22T01:00:00Z. Planned end 2026-11-22T03:00:00Z.") {
		t.Fatalf("maintenance notice:\n%s", result.Text())
	}
	// Refusals change nothing. A flag that was given is never taken for one
	// that was left out: notice 0 is refused, not treated as a new notice.
	for name, test := range map[string]struct {
		request StatusPageCommandRequest
		want    string
	}{
		"unknown notice":            {StatusPageCommandRequest{NoticeID: pointerTo(int64(7)), Message: pointerTo("Hello")}, "no notice 7"},
		"notice ID 0":               {StatusPageCommandRequest{NoticeID: pointerTo(int64(0)), Message: pointerTo("Hello")}, "--notice-id must be 1 or more"},
		"negative notice ID":        {StatusPageCommandRequest{NoticeID: pointerTo(int64(-1)), Message: pointerTo("Hello")}, "--notice-id must be 1 or more"},
		"no title":                  {StatusPageCommandRequest{Message: pointerTo("Hello")}, "--title is required to post a new notice"},
		"an empty title":            {StatusPageCommandRequest{Title: pointerTo(""), Message: pointerTo("Hello")}, "--title is required to post a new notice"},
		"no message":                {StatusPageCommandRequest{Title: pointerTo("Hello")}, "--message is required"},
		"a title on an update":      {StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), Title: pointerTo("New"), Message: pointerTo("Hello")}, "describe a new notice"},
		"an impact on an update":    {StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), Impact: pointerTo("outage"), Message: pointerTo("Hello")}, "describe a new notice"},
		"a window on an update":     {StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), ScheduledEndAt: pointerTo("2026-11-22T03:00:00Z"), Message: pointerTo("Hello")}, "describe a new notice"},
		"an unknown impact":         {StatusPageCommandRequest{Title: pointerTo("T"), Impact: pointerTo("down"), Message: pointerTo("Hello")}, "the impact must be outage, degraded, or maintenance"},
		"a stage of another kind":   {StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), Stage: pointerTo("scheduled"), Message: pointerTo("Hello")}, `is not a stage of outage notices`},
		"a window on an incident":   {StatusPageCommandRequest{Title: pointerTo("T"), ScheduledStartAt: pointerTo("2026-11-22T03:00:00Z"), Message: pointerTo("Hello")}, "only for a maintenance notice"},
		"a time that is not a time": {StatusPageCommandRequest{Title: pointerTo("T"), Impact: pointerTo("maintenance"), ScheduledStartAt: pointerTo("tomorrow"), Message: pointerTo("Hello")}, `--scheduled-start must be a time such as 2026-11-21T02:00:00Z, not`},
		"an end before the start":   {StatusPageCommandRequest{Title: pointerTo("T"), Impact: pointerTo("maintenance"), ScheduledStartAt: pointerTo("2026-11-22T03:00:00Z"), ScheduledEndAt: pointerTo("2026-11-22T02:00:00Z"), Message: pointerTo("Hello")}, "the planned end must be after the planned start"},
	} {
		if _, raw := post(test.request, 400); !strings.Contains(raw, test.want) {
			t.Errorf("%s: %s", name, raw)
		}
	}
	if file := publishedStatusFile(t, fake); len(file.Notices) != 2 || len(file.Notices[1].Updates) != 2 {
		t.Fatalf("a refused post changed the page: %+v", file.Notices)
	}
	resolve := func(request StatusPageCommandRequest, want int) (CommandResult, string) {
		t.Helper()
		return commandRequest(t, a, CommandRequest{Group: "status-page", Action: "resolve", StatusPage: request}, want)
	}
	if _, raw := resolve(StatusPageCommandRequest{}, 400); !strings.Contains(raw, "--notice-id is required") {
		t.Errorf("resolve without a notice ID: %s", raw)
	}
	if _, raw := resolve(StatusPageCommandRequest{NoticeID: pointerTo(int64(0))}, 400); !strings.Contains(raw, "--notice-id must be 1 or more") {
		t.Errorf("resolve notice 0: %s", raw)
	}
	if _, raw := resolve(StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), Stage: pointerTo("resolved")}, 400); !strings.Contains(raw, "a stage needs a closing message") {
		t.Errorf("a stage without a closing message: %s", raw)
	}
	result, _ = resolve(StatusPageCommandRequest{NoticeID: pointerTo(int64(1))}, 200)
	if !strings.Contains(result.Text(), "Open notice 2") || !strings.Contains(result.Text(), "Resolved notice 1, outage") {
		t.Fatalf("resolve:\n%s", result.Text())
	}
	resolve(StatusPageCommandRequest{NoticeID: pointerTo(int64(2)), ClosingMessage: pointerTo("The switch is replaced.")}, 200)
	file := publishedStatusFile(t, fake)
	if last := file.Notices[0].Updates[1]; file.Notices[0].ResolvedAt == nil || last.Stage != statuspage.StageCompleted || last.UpdateID != 2 {
		t.Fatalf("a maintenance notice closed with %+v", file.Notices[0])
	}
}

// The command shows a notice without updates, which only a file edited by
// hand holds, instead of failing on it.
func TestStatusPageTextShowsNoticesWithoutUpdates(t *testing.T) {
	at := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	resolvedAt := at.Add(time.Hour)
	view := StatusPageView{IsSetUp: true, PublishedStatusJSON: &statuspage.File{PageTitle: "Example Inc.", PublishedAt: at, Notices: []statuspage.Notice{
		{NoticeID: 3, Title: "Empty open", Impact: statuspage.ImpactMaintenance, ScheduledStartAt: &at, OpenedAt: at},
		{NoticeID: 2, Title: "Empty resolved", Impact: statuspage.ImpactOutage, OpenedAt: at, ResolvedAt: &resolvedAt},
		{NoticeID: 1, Title: "Full", Impact: statuspage.ImpactDegraded, OpenedAt: at, Updates: []statuspage.Update{{UpdateID: 1, Stage: statuspage.StageInvestigating, Message: "Slow.", PostedAt: at}}},
	}}}
	text := view.Text()
	for _, want := range []string{"Open notice 1, degraded: Full", "Update 1, investigating, at 2026-11-21T08:00:00Z: Slow.", "Open notice 3, maintenance: Empty open", "Planned start 2026-11-21T08:00:00Z.", "Resolved notice 2, outage, resolved at 2026-11-21T09:00:00Z: Empty resolved"} {
		if !strings.Contains(text, want) {
			t.Errorf("status-page show lacks %q:\n%s", want, text)
		}
	}
}

func publishedStatusFile(t *testing.T, fake *fakecloudflare.Server) statuspage.File {
	t.Helper()
	object, ok := fake.Object(statusPageBucket, "status.json")
	if !ok {
		t.Fatal("no status.json")
	}
	var file statuspage.File
	if err := json.Unmarshal(object.Content, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// A person under stress presses "Post to the status page" twice; customers
// must still read one notice. While the notice an incident posted is open,
// the incident page shows it instead of the button, and a second post from
// that incident is refused.
func TestTheIncidentPagePostsOneNoticeWhileItIsOpen(t *testing.T) {
	a, fake := statusPageApp(t)
	member := memberFixture(t, a, "responder@example.com")
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)
	incident, err := a.ManualSOS(context.Background(), member.Principal, "Sign-in fails", "")
	if err != nil {
		t.Fatal(err)
	}
	setUpStatusPageWithKey(t, a, fake)
	incidentPath := "/incidents/" + incident.IncidentID
	token := pageToken(t, client, server.URL, incidentPath)
	press := url.Values{"csrf": {token}, "incident_id": {incident.IncidentID}}
	postForm(t, client, server.URL, "/status-page/notices", press, 303)
	response := postForm(t, client, server.URL, "/status-page/notices", press, 409)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(data), "notice 1, posted from this incident, is still open") {
		t.Fatalf("second press: %s", data)
	}
	file := publishedStatusFile(t, fake)
	if len(file.Notices) != 1 {
		t.Fatalf("two presses published %d notices", len(file.Notices))
	}
	// One click opens an outage with the generic title and first message.
	if notice := file.Notices[0]; notice.Title != StatusPageGenericTitle || notice.Impact != statuspage.ImpactOutage || notice.Updates[0].Stage != statuspage.StageInvestigating || notice.Updates[0].Message != StatusPageGenericMessage {
		t.Fatalf("the incident's notice %+v", notice)
	}
	body := getPage(t, client, server.URL+incidentPath)
	if strings.Contains(body, "Post to the status page") || !strings.Contains(body, "Notice 1, posted from this incident, is open on the status page.") || !strings.Contains(body, `href="/status-page"`) {
		t.Fatalf("incident page while its notice is open:\n%s", body)
	}
	token = pageToken(t, client, server.URL, "/status-page")
	postForm(t, client, server.URL, "/status-page/notices/1/resolve", url.Values{"csrf": {token}, "closing_message": {"Sign-in works again."}}, 303)
	if body := getPage(t, client, server.URL+incidentPath); !strings.Contains(body, "Post to the status page") {
		t.Fatalf("incident page after its notice was resolved:\n%s", body)
	}

	// A laptop resolves notice 2 straight in the bucket while MMHotline is
	// down; opening the Status page page brings the incident page up to date.
	token = pageToken(t, client, server.URL, incidentPath)
	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "incident_id": {incident.IncidentID}}, 303)
	var setup statusPageSetup
	if err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		setup, _, err = readStatusPageSetup(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := statuspage.Publish(context.Background(), a.statusPageBucket(setup), statuspage.Resolve(statuspage.ResolveInput{NoticeID: 2}), setup.pageTitle, a.Now()); err != nil {
		t.Fatal(err)
	}
	getPage(t, client, server.URL+"/status-page")
	if body := getPage(t, client, server.URL+incidentPath); !strings.Contains(body, "Post to the status page") {
		t.Fatalf("incident page after a laptop resolved its notice:\n%s", body)
	}
}

// The incident page's button says what one click publishes: an outage notice
// with the generic title and message. It sends only the incident.
func TestTheIncidentPageSaysWhatItsButtonPublishes(t *testing.T) {
	a, fake := statusPageApp(t)
	member := memberFixture(t, a, "responder@example.com")
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)
	incident, err := a.ManualSOS(context.Background(), member.Principal, "Sign-in fails", "")
	if err != nil {
		t.Fatal(err)
	}
	setUpStatusPageWithKey(t, a, fake)
	body := getPage(t, client, server.URL+"/incidents/"+incident.IncidentID)
	if !strings.Contains(body, "an outage notice titled “"+StatusPageGenericTitle+"” that says “"+StatusPageGenericMessage+"”") || strings.Contains(body, `name="message"`) {
		t.Fatalf("incident page:\n%s", body)
	}
}

// A refused form shows what the person typed, with its fold open, so a long
// message typed in a hurry is never lost.
func TestStatusPageFormsKeepWhatWasTypedAfterARefusal(t *testing.T) {
	a, fake := statusPageApp(t)
	member := memberFixture(t, a, "responder@example.com")
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)
	setUpStatusPageWithKey(t, a, fake)
	token := pageToken(t, client, server.URL, "/status-page")
	refused := func(path string, form url.Values, status int, want string) string {
		t.Helper()
		form.Set("csrf", token)
		response := postForm(t, client, server.URL, path, form, status)
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(data), want) {
			t.Fatalf("POST %s did not say %q:\n%s", path, want, data)
		}
		return string(data)
	}
	for _, want := range []string{`value="Slow sign-in"`, `<option value="degraded" selected>`, `value="2026-11-22T21:00"`, `value="2026-11-22T20:00"`, `<option value="in_progress" selected>In progress</option>`, "Sign-in is slow.\nWe are looking."} {
		refused("/status-page/notices", url.Values{"title": {"Slow sign-in"}, "impact": {"degraded"}, "stage": {"in_progress"}, "scheduled_start_at": {"2026-11-22T21:00"}, "scheduled_end_at": {"2026-11-22T20:00"}, "message": {"Sign-in is slow.\nWe are looking."}}, 400, want)
	}
	// An end before the start is refused by the rule for a maintenance notice.
	refused("/status-page/notices", url.Values{"title": {"Upgrade"}, "impact": {"maintenance"}, "scheduled_start_at": {"2026-11-22T21:00"}, "scheduled_end_at": {"2026-11-22T20:00"}, "message": {"Planned."}}, 400, "the planned end must be after the planned start")
	refused("/status-page/notices", url.Values{"title": {"Upgrade"}, "impact": {"maintenance"}, "scheduled_start_at": {"soon"}, "message": {"Planned."}}, 400, "the planned start must be a date and a time")
	refused("/status-page/notices", url.Values{"title": {""}, "impact": {"outage"}, "message": {"Kept message."}}, 400, "Kept message.")
	if file := publishedStatusFile(t, fake); len(file.Notices) != 0 {
		t.Fatalf("a refused form published %+v", file.Notices)
	}

	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "title": {"Sign-in fails"}, "impact": {"outage"}, "message": {"Sign-in fails."}}, 303)
	// An update at a stage of another kind of notice keeps its text, and the
	// stage select offers the stages of the notice's impact.
	body := refused("/status-page/notices/1/updates", url.Values{"stage": {"scheduled"}, "message": {"Drafted update."}}, 400, "is not a stage of outage notices")
	stageSelect := body[strings.Index(body, "Stage (optional)"):strings.Index(body, "Add an update")]
	if !strings.Contains(body, "Drafted update.</textarea>") || !strings.Contains(stageSelect, `<option value="monitoring">Monitoring</option>`) || strings.Contains(stageSelect, "in_progress") {
		t.Fatalf("the update form after a refusal:\n%s", body)
	}
	// A refused closing message keeps its fold open and its text.
	long := strings.Repeat("x", 5001)
	body = refused("/status-page/notices/1/resolve", url.Values{"closing_message": {long}}, 400, "the closing message must have 1 to 5000 characters")
	if !strings.Contains(body, `<details class="fold" open>`) || !strings.Contains(body, long+"</textarea>") {
		t.Fatalf("the resolve form after a refusal lacks its draft or is closed")
	}
	if file := publishedStatusFile(t, fake); file.Notices[0].ResolvedAt != nil || len(file.Notices[0].Updates) != 1 {
		t.Fatalf("a refused form changed the notice: %+v", file.Notices[0])
	}
	// Another notice's forms show no draft.
	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "title": {"Second"}, "impact": {"degraded"}, "message": {"Second."}}, 303)
	body = refused("/status-page/notices/1/updates", url.Values{"stage": {"scheduled"}, "message": {"Only for notice 1."}}, 400, "Only for notice 1.</textarea>")
	if strings.Count(body, "Only for notice 1.") != 1 {
		t.Fatalf("a draft shows in two forms")
	}
}

// A planned time typed on the page is in the team's time zone; the notice is
// stored in UTC, and the page shows it in the team's time zone again.
func TestStatusPagePlannedTimesAreInTheTeamsTimeZone(t *testing.T) {
	a, fake := statusPageApp(t)
	ctx := context.Background()
	member := memberFixture(t, a, "responder@example.com")
	person, err := a.CurrentMember(ctx, member.Principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SaveSchedule(ctx, member.Principal, ScheduleConfig{Kind: "day_of_week", TimeZone: "America/New_York", Assignments: []string{person.ID, person.ID, person.ID, person.ID, person.ID, person.ID, person.ID}, BackupKind: "everyone_available"}, 0); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)
	setUpStatusPageWithKey(t, a, fake)
	token := pageToken(t, client, server.URL, "/status-page")
	// 21:00 in New York on 22 November is 02:00 UTC on the 23rd, standard time.
	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "title": {"Database upgrade"}, "impact": {"maintenance"}, "scheduled_start_at": {"2026-11-22T21:00"}, "scheduled_end_at": {"2026-11-22T23:30"}, "message": {"Planned."}}, 303)
	notice := publishedStatusFile(t, fake).Notices[0]
	if notice.ScheduledStartAt == nil || !notice.ScheduledStartAt.Equal(time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)) || notice.ScheduledEndAt == nil || !notice.ScheduledEndAt.Equal(time.Date(2026, 11, 23, 4, 30, 0, 0, time.UTC)) || notice.Updates[0].Stage != statuspage.StageScheduled {
		t.Fatalf("notice %+v", notice)
	}
	body := getPage(t, client, server.URL+"/status-page")
	for _, want := range []string{"America/New_York", "Complete this notice", "Planned"} {
		if !strings.Contains(body, want) {
			t.Errorf("status page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Resolve this notice") {
		t.Errorf("a maintenance notice is resolved, not completed")
	}
	// The public page keeps UTC.
	if index, _ := fake.Object(statusPageBucket, "index.html"); !strings.Contains(string(index.Content), "Scheduled maintenance") || !strings.Contains(string(index.Content), "23 November 2026, 02:00") {
		t.Fatalf("index.html: %s", index.Content)
	}
}

// setUpStatusPageWithComponents sets the status page up in Cloudflare, as the
// laptop does, and applies a status_page section with a website link, a
// support link, and two components, one without a description.
func setUpStatusPageWithComponents(t *testing.T, a *App, fake *fakecloudflare.Server) *SettingsStatusPage {
	t.Helper()
	cloudflare, err := setUpStatusPageFromALaptop(fake, "status.example.com")
	if err != nil {
		t.Fatal(err)
	}
	page := &SettingsStatusPage{PageTitle: "Example Inc.", HostName: "status.example.com", R2BucketURL: cloudflare.R2BucketURL, R2AccessKeyID: fake.R2AccessKeyID, R2SecretAccessKey: "fake-r2-secret-access-key",
		WebsiteURL: "https://example.com", SupportURL: "mailto:support@example.com",
		Components: []SettingsStatusPageComponent{{ComponentID: "website", Name: "Website", Description: "Main site and customer dashboard"}, {ComponentID: "api", Name: "API"}}}
	if reason := applyStatusPage(t, a, page); reason != "" {
		t.Fatal(reason)
	}
	return page
}

// Applying the settings file publishes its links and components in
// status.json and index.html, and a later apply that changes them keeps the
// notices. status-page show prints them with the status the open notices give.
func TestStatusPageApplyingTheSettingsFilePublishesTheLinksAndComponents(t *testing.T) {
	a, fake := statusPageApp(t)
	page := setUpStatusPageWithComponents(t, a, fake)
	file := publishedStatusFile(t, fake)
	if file.WebsiteURL != "https://example.com" || file.SupportURL != "mailto:support@example.com" || len(file.Components) != 2 || file.Components[0] != (statuspage.Component{ComponentID: "website", Name: "Website", Description: "Main site and customer dashboard"}) || file.Components[1] != (statuspage.Component{ComponentID: "api", Name: "API"}) {
		t.Fatalf("status.json %+v", file)
	}
	index, _ := fake.Object(statusPageBucket, "index.html")
	for _, want := range []string{`<a href="https://example.com">Website</a>`, `<a href="mailto:support@example.com">Support</a>`, `<section class="panel" aria-label="Components">`, "Main site and customer dashboard"} {
		if !strings.Contains(string(index.Content), want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	result, _ := commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: StatusPageCommandRequest{Title: pointerTo("Slow API"), Impact: pointerTo("degraded"), AffectedComponentIDs: &[]string{"api"}, Message: pointerTo("The API is slow.")}}, 200)
	for _, want := range []string{"Website link: https://example.com", "Support link: mailto:support@example.com", "Component Website (website): operational", "Component API (api): degraded", "Open notice 1, degraded: Slow API", "Affects API."} {
		if !strings.Contains(result.Text(), want) {
			t.Errorf("status-page show lacks %q:\n%s", want, result.Text())
		}
	}
	// A later apply that drops the links and one component keeps the notice,
	// which still names its component.
	page.WebsiteURL, page.SupportURL, page.Components = "", "", page.Components[:1]
	if reason := applyStatusPage(t, a, page); reason != "" {
		t.Fatal(reason)
	}
	file = publishedStatusFile(t, fake)
	if file.WebsiteURL != "" || file.SupportURL != "" || len(file.Components) != 1 || len(file.Notices) != 1 || len(file.Notices[0].AffectedComponentIDs) != 1 || file.NextNoticeID != 2 {
		t.Fatalf("status.json after the second apply: %+v", file)
	}
	if text := (StatusPageView{PublishedStatusJSON: &file}).Text(); !strings.Contains(text, "Affects api.") || !strings.Contains(text, "Component Website (website): operational") {
		t.Errorf("a notice of a removed component:\n%s", text)
	}
	// A page that fails the check is refused and changes nothing.
	page.WebsiteURL = "http://example.com"
	if reason := applyStatusPage(t, a, page); !strings.Contains(reason, "the website URL must be an https:// address") {
		t.Fatalf("an http:// link: %q", reason)
	}
	if again := publishedStatusFile(t, fake); len(again.Components) != 1 || again.WebsiteURL != "" {
		t.Fatalf("a refused apply changed status.json: %+v", again)
	}
}

// A notice's ID is only meaningful in its own bucket. When the settings file
// names another bucket, the links from incidents to notices are removed, so a
// notice 1 in the new bucket is never taken for the old one.
func TestStatusPageApplyingAnotherBucketRemovesTheIncidentNoticeLinks(t *testing.T) {
	a, fake := statusPageApp(t)
	page := setUpStatusPageWithComponents(t, a, fake)
	linked := func() int {
		var count int
		if err := a.Store.db.QueryRow(`SELECT COUNT(*) FROM status_page_incident_notices`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if _, err := a.Store.db.Exec(`INSERT INTO status_page_incident_notices(incident_id,notice_id,resolved) VALUES('incident-1',1,0),('incident-2',2,1)`); err != nil {
		t.Fatal(err)
	}
	// Applying the same bucket again keeps them.
	page.PageTitle = "Example Inc. status"
	if reason := applyStatusPage(t, a, page); reason != "" {
		t.Fatal(reason)
	}
	if got := linked(); got != 2 {
		t.Fatalf("applying the same bucket left %d links, want 2", got)
	}
	other, err := setUpStatusPageFromALaptop(fake, "other.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// A refused apply to the other bucket, with a wrong key, keeps them too.
	page.R2BucketURL, page.R2AccessKeyID = other.R2BucketURL, "anotherkey"
	if reason := applyStatusPage(t, a, page); reason == "" {
		t.Fatal("a wrong key was accepted")
	}
	if got := linked(); got != 2 {
		t.Fatalf("a refused apply left %d links, want 2", got)
	}
	page.R2AccessKeyID = fake.R2AccessKeyID
	if reason := applyStatusPage(t, a, page); reason != "" {
		t.Fatal(reason)
	}
	if got := linked(); got != 0 {
		t.Fatalf("applying another bucket left %d links, want 0", got)
	}
	if file, ok := fake.Object(statusPageBucketOf(other.R2BucketURL), "status.json"); !ok || len(file.Content) == 0 {
		t.Fatal("the new bucket got no status.json")
	}
}

// Removing the status_page section removes the incident links with the setup,
// so enabling the page again on another bucket never inherits them.
func TestStatusPageRemovingTheSectionAndEnablingAnotherBucketKeepsNoIncidentNoticeLinks(t *testing.T) {
	a, fake := statusPageApp(t)
	page := setUpStatusPageWithComponents(t, a, fake)
	linked := func() int {
		var count int
		if err := a.Store.db.QueryRow(`SELECT COUNT(*) FROM status_page_incident_notices`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if _, err := a.Store.db.Exec(`INSERT INTO status_page_incident_notices(incident_id,notice_id,resolved) VALUES('incident-1',1,0),('incident-2',2,1)`); err != nil {
		t.Fatal(err)
	}
	if reason := applyStatusPage(t, a, nil); reason != "" {
		t.Fatal(reason)
	}
	if got := linked(); got != 0 {
		t.Fatalf("removing the section left %d links, want 0", got)
	}
	other, err := setUpStatusPageFromALaptop(fake, "other.example.com")
	if err != nil {
		t.Fatal(err)
	}
	page.R2BucketURL = other.R2BucketURL
	if reason := applyStatusPage(t, a, page); reason != "" {
		t.Fatal(reason)
	}
	if got := linked(); got != 0 {
		t.Fatalf("enabling another bucket found %d links, want 0", got)
	}
}

// statusPageBucketOf is the bucket's name in an R2 bucket URL.
func statusPageBucketOf(bucketURL string) string {
	_, name := r2BucketURLParts(bucketURL)
	return name
}

// A post from MMHotline never starts a status.json: it would lack the host
// name, links, and components of the settings file. Only applying the file
// does.
func TestStatusPageAnOrdinaryPostNeverCreatesAMissingStatusJSON(t *testing.T) {
	a, fake := statusPageApp(t)
	setUpStatusPageWithComponents(t, a, fake)
	other, err := setUpStatusPageFromALaptop(fake, "other.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// The saved setup now points at a bucket that holds no status.json, as if
	// the file had been removed from R2.
	if _, err := a.Store.db.Exec(`UPDATE status_page_setup SET r2_bucket_url=?`, other.R2BucketURL); err != nil {
		t.Fatal(err)
	}
	_, raw := commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: StatusPageCommandRequest{Title: pointerTo("Hello"), Message: pointerTo("Hello")}}, 409)
	if !strings.Contains(raw, "the status page has no status.json yet; apply the settings file") {
		t.Fatalf("post without status.json: %s", raw)
	}
	if _, ok := fake.Object(statusPageBucketOf(other.R2BucketURL), "status.json"); ok {
		t.Fatal("an ordinary post created status.json")
	}
}

func TestStatusPageCommandPostWithComponents(t *testing.T) {
	a, fake := statusPageApp(t)
	setUpStatusPageWithComponents(t, a, fake)
	post := func(request StatusPageCommandRequest, want int) (CommandResult, string) {
		t.Helper()
		return commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: request}, want)
	}
	// Each component once, in the page's order, whatever the order given.
	post(StatusPageCommandRequest{Title: pointerTo("Slow"), Impact: pointerTo("degraded"), AffectedComponentIDs: &[]string{"api", "website", "api"}, Message: pointerTo("Slow.")}, 200)
	if got := publishedStatusFile(t, fake).Notices[0].AffectedComponentIDs; len(got) != 2 || got[0] != "website" || got[1] != "api" {
		t.Fatalf("affected components %v", got)
	}
	post(StatusPageCommandRequest{Title: pointerTo("Plain"), Message: pointerTo("No components.")}, 200)
	if got := publishedStatusFile(t, fake).Notices[0]; got.AffectedComponentIDs != nil {
		t.Fatalf("a notice without --component has %v", got.AffectedComponentIDs)
	}
	for name, test := range map[string]struct {
		request StatusPageCommandRequest
		want    string
	}{
		"an unknown component":                     {StatusPageCommandRequest{Title: pointerTo("T"), AffectedComponentIDs: &[]string{"cdn"}, Message: pointerTo("m")}, `the status page has no component \"cdn\"; its components are website, api`},
		"a component on an update":                 {StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), AffectedComponentIDs: &[]string{"api"}, Message: pointerTo("m")}, "describe a new notice"},
		"an empty list of components on an update": {StatusPageCommandRequest{NoticeID: pointerTo(int64(1)), AffectedComponentIDs: &[]string{}, Message: pointerTo("m")}, "describe a new notice"},
	} {
		if _, raw := post(test.request, 400); !strings.Contains(raw, test.want) {
			t.Errorf("%s: %s", name, raw)
		}
	}
	if file := publishedStatusFile(t, fake); len(file.Notices) != 2 {
		t.Fatalf("a refused post changed the page: %+v", file.Notices)
	}
}

// The Post a new notice form offers a checkbox for each component of the
// page, and a refused form keeps the boxes that were checked.
func TestStatusPageFormTagsComponentsAndKeepsTheBoxesAfterARefusal(t *testing.T) {
	a, fake := statusPageApp(t)
	member := memberFixture(t, a, "responder@example.com")
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)
	setUpStatusPageWithKey(t, a, fake)
	// Without components, the form has no checkboxes.
	if body := getPage(t, client, server.URL+"/status-page"); strings.Contains(body, "affected_component_ids") || strings.Contains(body, "Affected components") {
		t.Fatalf("the form offers components the page does not have:\n%s", body)
	}
	setUpStatusPageWithComponents(t, a, fake)
	token := pageToken(t, client, server.URL, "/status-page")
	boxes := regexp.MustCompile(`<input type="checkbox" name="affected_component_ids" value="([a-z]+)"( checked)?> ([A-Za-z]+)`)
	checked := func(body string) map[string]bool {
		t.Helper()
		result := map[string]bool{}
		for _, box := range boxes.FindAllStringSubmatch(body, -1) {
			result[box[1]+":"+box[3]] = box[2] != ""
		}
		return result
	}
	body := getPage(t, client, server.URL+"/status-page")
	if got := checked(body); len(got) != 2 || got["website:Website"] || got["api:API"] || !strings.Contains(body, "Affected components (optional)") {
		t.Fatalf("the form's checkboxes %v:\n%s", got, body)
	}
	refuse := func(form url.Values, want string) string {
		t.Helper()
		form.Set("csrf", token)
		response := postForm(t, client, server.URL, "/status-page/notices", form, 400)
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(data), want) {
			t.Fatalf("the refusal lacks %q:\n%s", want, data)
		}
		return string(data)
	}
	// A refusal keeps the checked boxes, and only those.
	body = refuse(url.Values{"title": {""}, "impact": {"degraded"}, "message": {"Kept message."}, "affected_component_ids": {"api"}}, "Kept message.")
	if got := checked(body); !got["api:API"] || got["website:Website"] {
		t.Fatalf("after a refusal the boxes are %v", got)
	}
	// A component the page does not have is refused, and the other boxes stay.
	body = refuse(url.Values{"title": {"Slow"}, "impact": {"degraded"}, "message": {"Slow."}, "affected_component_ids": {"website", "cdn"}}, `the status page has no component &#34;cdn&#34;`)
	if got := checked(body); !got["website:Website"] || got["api:API"] {
		t.Fatalf("after an unknown component the boxes are %v", got)
	}
	if file := publishedStatusFile(t, fake); len(file.Notices) != 0 {
		t.Fatalf("a refused form published %+v", file.Notices)
	}
	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "title": {"Slow"}, "impact": {"degraded"}, "message": {"Slow."}, "affected_component_ids": {"api", "website"}}, 303)
	if got := publishedStatusFile(t, fake).Notices[0].AffectedComponentIDs; len(got) != 2 || got[0] != "website" || got[1] != "api" {
		t.Fatalf("affected components %v", got)
	}
	// The page shows them on the open notice, and a notice of a removed
	// component by its ID; a new form starts with no box checked.
	body = getPage(t, client, server.URL+"/status-page")
	if !regexp.MustCompile(`Affects\s*<span class="tag nodot">Website</span>\s*<span class="tag nodot">API</span>`).MatchString(body) {
		t.Fatalf("the open notice does not name its components:\n%s", body)
	}
	if got := checked(body); got["website:Website"] || got["api:API"] {
		t.Fatalf("a new form has boxes checked: %v", got)
	}
}
