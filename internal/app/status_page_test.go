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

	"mmhotline/internal/fakecloudflare"
	"mmhotline/internal/statuspage"
)

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
	if _, raw := commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: StatusPageCommandRequest{Message: "Hello"}}, 409); !strings.Contains(raw, "the status page is not set up; an administrator adds the status_page section to the settings file") {
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
	if result.StatusPage.PublishedStatusJSON == nil || result.StatusPage.PublishedStatusJSON.PageTitle != "Example Inc." || result.StatusPage.SetUpBy != "the settings file "+InstalledSettingsFilePath || result.StatusPage.BucketName != statusPageBucket || strings.Contains(raw, "fake-r2-secret-access-key") {
		t.Fatalf("status-page show %s", raw)
	}
	index, ok := fake.Object(statusPageBucket, "index.html")
	if !ok || !strings.Contains(string(index.Content), "Everything is working.") || !strings.Contains(string(index.Content), "Example Inc.") {
		t.Fatalf("first page: %q", index.Content)
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
	response := postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "message": {StatusPageGenericMessage}}, 303)
	if response.Header.Get("Location") != "/status-page" {
		t.Fatalf("redirect to %s", response.Header.Get("Location"))
	}
	body = getPage(t, client, server.URL+"/status-page")
	for _, want := range []string{"https://status.example.com/", "Open notice 1", StatusPageGenericMessage, "Add an update", "Resolve this notice"} {
		if !strings.Contains(body, want) {
			t.Errorf("status page page lacks %q", want)
		}
	}
	token = pageToken(t, client, server.URL, "/status-page")
	postForm(t, client, server.URL, "/status-page/notices/1/updates", url.Values{"csrf": {token}, "message": {"Sign-in fails for some customers."}}, 303)
	postForm(t, client, server.URL, "/status-page/notices/1/resolve", url.Values{"csrf": {token}, "closing_message": {"Sign-in works again."}}, 303)
	file := publishedStatusFile(t, fake)
	notice := file.Notices[0]
	if notice.ResolvedAt == nil || len(notice.Updates) != 3 || notice.Updates[1].Message != "Sign-in fails for some customers." || notice.Updates[2].Message != "Sign-in works again." {
		t.Fatalf("published notice %+v", notice)
	}
	response = postForm(t, client, server.URL, "/status-page/notices/1/updates", url.Values{"csrf": {token}, "message": {"Late."}}, 400)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(data), "notice 1 is resolved; post a new notice instead") {
		t.Fatalf("update to a resolved notice: %s", data)
	}
	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {"wrong"}, "message": {"x"}}, 403)
}

func TestStatusPageCommandPostAndResolve(t *testing.T) {
	a, fake := statusPageApp(t)
	setUpStatusPageWithKey(t, a, fake)
	result, raw := commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: StatusPageCommandRequest{Message: "We are moving the database."}}, 200)
	if result.StatusPageNoticeID != 1 || !strings.Contains(result.Text(), "Open notice 1") {
		t.Fatalf("post: %s\n%s", raw, result.Text())
	}
	assertCommandKeys(t, raw, "notice_id", "status_page", "notices", "page_title", "published_at")
	if _, raw = commandRequest(t, a, CommandRequest{Group: "status-page", Action: "post", StatusPage: StatusPageCommandRequest{NoticeID: 7, Message: "Hello"}}, 400); !strings.Contains(raw, "no notice 7") {
		t.Fatalf("unknown notice: %s", raw)
	}
	result, _ = commandRequest(t, a, CommandRequest{Group: "status-page", Action: "resolve", StatusPage: StatusPageCommandRequest{NoticeID: 1}}, 200)
	if !strings.Contains(result.Text(), "No open notices") || !strings.Contains(result.Text(), "Resolved notice 1") {
		t.Fatalf("resolve:\n%s", result.Text())
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
	press := url.Values{"csrf": {token}, "message": {StatusPageGenericMessage}, "incident_id": {incident.IncidentID}}
	postForm(t, client, server.URL, "/status-page/notices", press, 303)
	response := postForm(t, client, server.URL, "/status-page/notices", press, 409)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(data), "notice 1, posted from this incident, is still open") {
		t.Fatalf("second press: %s", data)
	}
	if file := publishedStatusFile(t, fake); len(file.Notices) != 1 {
		t.Fatalf("two presses published %d notices", len(file.Notices))
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
	postForm(t, client, server.URL, "/status-page/notices", url.Values{"csrf": {token}, "message": {StatusPageGenericMessage}, "incident_id": {incident.IncidentID}}, 303)
	var setup statusPageSetup
	if err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		setup, _, err = readStatusPageSetup(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := statuspage.Publish(context.Background(), a.statusPageBucket(setup), statuspage.Resolve(2, ""), setup.pageTitle, a.Now()); err != nil {
		t.Fatal(err)
	}
	getPage(t, client, server.URL+"/status-page")
	if body := getPage(t, client, server.URL+incidentPath); !strings.Contains(body, "Post to the status page") {
		t.Fatalf("incident page after a laptop resolved its notice:\n%s", body)
	}
}
