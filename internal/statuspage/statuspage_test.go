package statuspage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakecloudflare"
)

func testBucket(t *testing.T) (*R2Bucket, *fakecloudflare.Server) {
	t.Helper()
	fake := fakecloudflare.New(t)
	fake.AddBucket("mmhotline-status-example-com")
	return &R2Bucket{BucketURL: fake.Addresses().R2BucketURL(fake.AccountID, "mmhotline-status-example-com"), AccessKeyID: fake.R2AccessKeyID, SecretAccessKey: "fake-secret"}, fake
}

func publishedFile(t *testing.T, fake *fakecloudflare.Server) (File, string) {
	t.Helper()
	statusJSON, ok := fake.Object("mmhotline-status-example-com", "status.json")
	if !ok {
		t.Fatal("status.json was not uploaded")
	}
	page, ok := fake.Object("mmhotline-status-example-com", "index.html")
	if !ok {
		t.Fatal("index.html was not uploaded")
	}
	if statusJSON.ContentType != "application/json" || page.ContentType != "text/html; charset=utf-8" {
		t.Fatalf("content types %q and %q", statusJSON.ContentType, page.ContentType)
	}
	var file File
	if err := json.Unmarshal(statusJSON.Content, &file); err != nil {
		t.Fatal(err)
	}
	return file, string(page.Content)
}

var opened = time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)

// incident is an outage notice with the stage investigating.
func incident(message string) PostNoticeInput {
	return PostNoticeInput{Title: "Service disruption", Impact: ImpactOutage, Stage: StageInvestigating, Message: message}
}

func pointer(t time.Time) *time.Time { return &t }

// pageText is what a reader of index.html sees, without its head, script, and
// tags, in one line.
func pageText(page string) string {
	body := page[strings.Index(page, "<body>"):]
	body = regexp.MustCompile(`(?s)<script>.*?</script>`).ReplaceAllString(body, " ")
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(body, " ")
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}

// inOrder says whether text holds each phrase, one after another.
func inOrder(text string, phrases ...string) bool {
	for _, phrase := range phrases {
		index := strings.Index(text, phrase)
		if index < 0 {
			return false
		}
		text = text[index+len(phrase):]
	}
	return true
}

// titleAndHeading are the page's title, which a browser's tab shows, and
// its heading.
func titleAndHeading(page string) (string, string) {
	title := regexp.MustCompile(`<title>(.*?)</title>`).FindStringSubmatch(page)
	heading := regexp.MustCompile(`<h1>(.*?)</h1>`).FindStringSubmatch(page)
	if title == nil || heading == nil {
		return "", ""
	}
	return html.UnescapeString(title[1]), html.UnescapeString(heading[1])
}

func TestPostUpdateAndResolveKeepTheWholeHistory(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	_, id, err := Publish(ctx, bucket, PostNotice(incident("We are looking into a problem.")), "Example Inc.", opened)
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("first notice ID %d", id)
	}
	file, page := publishedFile(t, fake)
	if file.PageTitle != "Example Inc." || len(file.OpenNotices()) != 1 {
		t.Fatalf("published %+v", file)
	}
	// The page title is page_title exactly as written: nothing is added.
	if title, heading := titleAndHeading(page); title != "Example Inc." || heading != "Example Inc." {
		t.Errorf("title %q and heading %q, want the page title as written", title, heading)
	}
	text := pageText(page)
	if !inOrder(text, "Outage", "We are experiencing a service outage.", "Last updated 26 October 2026, 08:00 UTC", "Outage", "Service disruption", "Started 26 October 2026, 08:00 UTC", "Investigating", "We are looking into a problem.") {
		t.Errorf("index.html with an open notice: %s", text)
	}
	if strings.Contains(text, "Everything is working.") || strings.Contains(text, " ago") || !strings.Contains(page, "fill='%23c4231c'") {
		t.Errorf("index.html with an open notice: %s", text)
	}

	if _, _, err = Publish(ctx, bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Stage: StageIdentified, Message: "Sign-in fails for some customers.\nWe are moving the database."}), "", opened.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Publish(ctx, bucket, Resolve(ResolveInput{NoticeID: 1, ClosingMessage: "Sign-in works again."}), "", opened.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	file, page = publishedFile(t, fake)
	notice := file.Notices[0]
	if notice.ResolvedAt == nil || !notice.ResolvedAt.Equal(opened.Add(30*time.Minute)) || !notice.UpdatedAt.Equal(opened.Add(30*time.Minute)) || len(notice.Updates) != 3 || notice.Updates[2].Stage != StageResolved || notice.Updates[2].UpdateID != 3 || notice.NextUpdateID != 4 {
		t.Fatalf("resolved notice %+v", notice)
	}
	if notice.Updates[1].Message != "Sign-in fails for some customers.\nWe are moving the database." || !notice.Updates[1].PostedAt.Equal(opened.Add(9*time.Minute)) || notice.Updates[1].UpdateID != 2 || notice.Updates[1].Stage != StageIdentified {
		t.Fatalf("update kept as %+v", notice.Updates[1])
	}
	// Resolved, the page says everything works, and lists the notice under
	// Recently resolved with when it was open and its updates, newest first.
	text = pageText(page)
	if !inOrder(text, "Everything is working.", "Last updated 26 October 2026, 08:30 UTC", "Recently resolved", "Outage", "Resolved", "Service disruption", "26 October 2026, 08:00 to 08:30 UTC",
		"08:30", "Resolved", "Sign-in works again.", "08:09", "Identified", "Sign-in fails for some customers.", "08:00", "Investigating", "We are looking into a problem.") || strings.Contains(text, "We are experiencing") || !strings.Contains(page, "fill='%231f9d5c'") {
		t.Errorf("index.html after the resolve: %s", text)
	}
	if !file.PublishedAt.Equal(opened.Add(30 * time.Minute)) {
		t.Fatalf("published_at %s", file.PublishedAt)
	}

	_, _, err = Publish(ctx, bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Too late."}), "", opened.Add(time.Hour))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "post a new notice instead") {
		t.Fatalf("update to a resolved notice: %v", err)
	}
	_, id, err = Publish(ctx, bucket, PostNotice(incident("A new problem.")), "", opened.Add(time.Hour))
	if err != nil || id != 2 {
		t.Fatalf("second notice %d: %v", id, err)
	}
	file, _ = publishedFile(t, fake)
	if len(file.Notices) != 2 || file.Notices[0].NoticeID != 2 || file.Notices[1].NoticeID != 1 || file.NextNoticeID != 3 {
		t.Fatalf("history newest first: %+v", file.Notices)
	}
}

// A problem's first line gives when it started and its latest update, its
// updates follow newest first, and every time is in UTC, whatever the
// poster's time zone; an update on a later day than the start names its day.
func TestAProblemShowsItsUpdatesInUTC(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	late := time.Date(2026, 10, 26, 23, 50, 0, 0, time.UTC)
	if _, _, err := Publish(ctx, bucket, PostNotice(incident("We are looking into a problem.")), "Example Inc.", late.In(newYork)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Publish(ctx, bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Sign-in fails.\nWe are moving the database."}), "", late.Add(25*time.Minute).In(newYork)); err != nil {
		t.Fatal(err)
	}
	_, page := publishedFile(t, fake)
	if text := pageText(page); !inOrder(text, "Outage", "Service disruption", "Started 26 October 2026, 23:50 UTC · last update 27 Oct 00:15 UTC",
		"27 Oct 00:15", "Sign-in fails. We are moving the database.", "23:50", "We are looking into a problem.", "Times are in UTC.") {
		t.Fatalf("index.html: %s", text)
	}
	// The message keeps its line break.
	if !strings.Contains(page, "Sign-in fails.\nWe are moving the database.") {
		t.Fatalf("index.html changed the message's line breaks:\n%s", page)
	}
}

// index.html carries its own stylesheet and script, and loads nothing: it
// must look right and work while MMHotline is down, and a static file in a
// bucket has no other files to load.
func TestPageEmbedsItsOwnStylesheetAndScript(t *testing.T) {
	page, err := RenderPage(File{PageTitle: "Example Inc."})
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if strings.Count(html, "<style>") != 1 || !strings.Contains(html, strings.TrimSpace(pageStylesheet)) {
		t.Errorf("index.html does not embed status_page.css once")
	}
	if strings.Count(html, "<script") != 1 || !strings.Contains(html, strings.TrimSpace(localTimeScript)) {
		t.Errorf("index.html does not embed local_time.js once")
	}
	if strings.Contains(localTimeScript, "</script") {
		t.Errorf("local_time.js would end its script element early")
	}
	for _, loaded := range []string{`rel="stylesheet"`, "@import", "<script src", "url(", "<img", "<iframe"} {
		if strings.Contains(html, loaded) {
			t.Errorf("index.html loads something from outside (%s)", loaded)
		}
	}
	// The stylesheet has its own rules for what the page uses, not
	// MMHotline's admin stylesheet's.
	for _, rule := range []string{".public {", ".public-header {", ".state.ok {", ".state.sos {", ".state.warn {", ".state.work {", ".tag.warn {", ".tag.work {", ".components {", ".chips {", ".chip {", ".stage {", ".timeline li {"} {
		if !strings.Contains(pageStylesheet, rule) {
			t.Errorf("status_page.css has no %s rule", rule)
		}
	}
}

// Every time is its own <time> element holding only the time, the zone is a
// sibling .tz element, and each notice card has an anchor ID. local_time.js
// depends on this markup.
func TestTimesAreTimeElementsAndCardsHaveAnchors(t *testing.T) {
	resolvedAt := opened.Add(40 * time.Minute)
	file := File{PageTitle: "Example Inc.", PublishedAt: opened.Add(time.Hour), Notices: []Notice{
		{NoticeID: 2, Title: "Two", Impact: ImpactMaintenance, ScheduledStartAt: pointer(opened.Add(2 * time.Hour)), ScheduledEndAt: pointer(opened.Add(3 * time.Hour)), OpenedAt: opened.Add(50 * time.Minute), Updates: []Update{{UpdateID: 1, Message: "B.", PostedAt: opened.Add(50 * time.Minute)}}},
		{NoticeID: 1, Title: "One", Impact: ImpactOutage, OpenedAt: opened, ResolvedAt: &resolvedAt, Updates: []Update{{UpdateID: 1, Message: "A.", PostedAt: opened}, {UpdateID: 2, Message: "Fixed.", PostedAt: resolvedAt}}},
	}}
	page, err := RenderPage(file)
	if err != nil {
		t.Fatal(err)
	}
	html := regexp.MustCompile(`(?s)<script>.*?</script>`).ReplaceAllString(string(page), "")
	times := regexp.MustCompile(`<time datetime="([^"]*)" data-format="(full|update)"[^>]*>([^<]*)</time>`).FindAllStringSubmatch(html, -1)
	if len(times) != strings.Count(html, "<time ") || len(times) == 0 {
		t.Fatalf("%d well-formed <time> elements of %d", len(times), strings.Count(html, "<time "))
	}
	for _, found := range times {
		if _, err := time.Parse(time.RFC3339, found[1]); err != nil || strings.Contains(found[3], "UTC") {
			t.Errorf("<time> with datetime %q and text %q", found[1], found[3])
		}
	}
	for _, want := range []string{`id="notice-1"`, `id="notice-2"`, `<span class="tz">UTC</span>`, `<span id="time-zone-note">Times are in UTC.</span>`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	// The planned window is two <time> elements and one UTC marker.
	window := regexp.MustCompile(`Planned: <time datetime="2026-10-26T10:00:00Z" data-format="full">[^<]*</time> to <time datetime="2026-10-26T11:00:00Z" data-format="update" data-day-ref="2026-10-26T10:00:00Z">11:00</time> <span class="tz">UTC</span>`)
	if !window.MatchString(html) {
		t.Errorf("the planned window is not two time elements:\n%s", html)
	}
	// The resolved notice's interval is two <time> elements and one UTC marker.
	interval := regexp.MustCompile(`<time datetime="2026-10-26T08:00:00Z" data-format="full">[^<]*</time> to <time datetime="2026-10-26T08:40:00Z" data-format="update" data-day-ref="2026-10-26T08:00:00Z">08:40</time> <span class="tz">UTC</span>`)
	if !interval.MatchString(html) {
		t.Errorf("the resolved interval is not two time elements:\n%s", html)
	}
}

func TestStatusJSONFieldNames(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, ApplyPageConfig(PageConfig{PageTitle: "Example Inc.", HostName: "status.example.com"}), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Publish(ctx, bucket, PostNotice(incident("We are looking into a problem.")), "", opened); err != nil {
		t.Fatal(err)
	}
	object, _ := fake.Object("mmhotline-status-example-com", "status.json")
	for _, want := range []string{`"page_title": "Example Inc."`, `"host_name": "status.example.com"`, `"published_at": "2026-10-26T08:00:00Z"`, `"next_notice_id": 2`, `"notice_id": 1`, `"title": "Service disruption"`, `"impact": "outage"`,
		`"opened_at": "2026-10-26T08:00:00Z"`, `"updated_at": "2026-10-26T08:00:00Z"`, `"resolved_at": null`, `"next_update_id": 2`, `"update_id": 1`, `"stage": "investigating"`, `"message": "We are looking into a problem."`, `"posted_at": "2026-10-26T08:00:00Z"`} {
		if !strings.Contains(string(object.Content), want) {
			t.Errorf("status.json lacks %s:\n%s", want, object.Content)
		}
	}
	// A maintenance notice carries its window; an update without a stage and
	// an unedited update leave their keys out.
	if _, _, err := Publish(ctx, bucket, PostNotice(PostNoticeInput{Title: "Database upgrade", Impact: ImpactMaintenance, Message: "Planned.", ScheduledStartAt: pointer(opened.Add(time.Hour + 500*time.Millisecond)), ScheduledEndAt: pointer(opened.Add(2 * time.Hour))}), "", opened); err != nil {
		t.Fatal(err)
	}
	object, _ = fake.Object("mmhotline-status-example-com", "status.json")
	for _, want := range []string{`"scheduled_start_at": "2026-10-26T09:00:00Z"`, `"scheduled_end_at": "2026-10-26T10:00:00Z"`} {
		if !strings.Contains(string(object.Content), want) {
			t.Errorf("status.json lacks %s:\n%s", want, object.Content)
		}
	}
	if strings.Count(string(object.Content), `"stage"`) != 1 || strings.Contains(string(object.Content), "edited_at") {
		t.Errorf("status.json writes an empty stage or an edited_at:\n%s", object.Content)
	}
}

func TestPostRefusesBadInputAndAMissingPage(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, PostNotice(incident("Hello")), "", opened); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("post before set-up: %v", err)
	}
	if _, ok := fake.Object("mmhotline-status-example-com", "status.json"); ok {
		t.Fatal("a refused post uploaded status.json")
	}
	for _, message := range []string{"", "   \n", strings.Repeat("x", 5001)} {
		if _, _, err := Publish(ctx, bucket, PostNotice(incident(message)), "Example Inc.", opened); !errors.Is(err, ErrRefused) {
			t.Errorf("message of %d characters: %v", len(message), err)
		}
	}
	if _, _, err := Publish(ctx, bucket, PostNotice(incident(strings.Repeat("é", 5000))), "Example Inc.", opened); err != nil {
		t.Fatalf("5,000 characters: %v", err)
	}
	if _, _, err := Publish(ctx, bucket, PostUpdate(PostUpdateInput{NoticeID: 9, Message: "Hello"}), "", opened); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no notice 9") {
		t.Fatalf("unknown notice: %v", err)
	}
	if _, err := CheckPageTitle("Two\nlines"); !errors.Is(err, ErrRefused) {
		t.Fatalf("two-line title: %v", err)
	}
}

func TestPageShowsMessagesAsText(t *testing.T) {
	bucket, fake := testBucket(t)
	if _, _, err := Publish(context.Background(), bucket, PostNotice(PostNoticeInput{Title: "<i>Down</i>", Impact: ImpactOutage, Message: `<script>alert("x")</script>`}), "Example <b>Inc.</b>", opened); err != nil {
		t.Fatal(err)
	}
	_, page := publishedFile(t, fake)
	if strings.Contains(page, "<script>alert") || strings.Contains(page, "<b>Inc.</b>") || strings.Contains(page, "<i>Down</i>") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("index.html does not escape text:\n%s", page)
	}
}

// changeBeforeFirstWrite is another writer that posts between this post's
// read and its first write of status.json, as a second responder would.
type changeBeforeFirstWrite struct {
	fake *fakecloudflare.Server
	done bool
}

func (c *changeBeforeFirstWrite) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/status.json") && !c.done {
		c.done = true
		current, _ := c.fake.Object("mmhotline-status-example-com", "status.json")
		var file File
		json.Unmarshal(current.Content, &file)
		PostNotice(incident("Posted from a laptop at the same time."))(&file, opened.Add(time.Minute))
		content, _ := json.Marshal(file)
		c.fake.PutObject("mmhotline-status-example-com", "status.json", content)
	}
	return http.DefaultTransport.RoundTrip(request)
}

func TestPostsAtTheSameTimeAreBothKept(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, PostNotice(incident("First.")), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	bucket.Client = &http.Client{Transport: &changeBeforeFirstWrite{fake: fake}}
	if _, _, err := Publish(ctx, bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Second."}), "", opened.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	file, page := publishedFile(t, fake)
	if len(file.Notices) != 2 || file.Notices[0].Updates[0].Message != "Posted from a laptop at the same time." || len(file.Notices[1].Updates) != 2 || file.Notices[1].Updates[1].Message != "Second." {
		t.Fatalf("a post was lost: %+v", file.Notices)
	}
	if !strings.Contains(page, "Posted from a laptop at the same time.") {
		t.Fatalf("index.html misses the other post:\n%s", page)
	}
}

// otherPostBeforeIndexHTML is a second responder whose whole post runs after
// this post saved status.json and before this post's index.html arrives, so
// this post's older index.html is uploaded last.
type otherPostBeforeIndexHTML struct {
	other *R2Bucket
	done  bool
}

func (o *otherPostBeforeIndexHTML) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/index.html") && !o.done {
		o.done = true
		if _, _, err := Publish(request.Context(), o.other, PostNotice(incident("A second problem.")), "", opened.Add(31*time.Minute)); err != nil {
			return nil, err
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}

func TestPageShowsTheLatestStatusJSONAfterPostsAtTheSameMoment(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, PostNotice(incident("First.")), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	other := *bucket
	bucket.Client = &http.Client{Transport: &otherPostBeforeIndexHTML{other: &other}}
	if _, _, err := Publish(ctx, bucket, Resolve(ResolveInput{NoticeID: 1, ClosingMessage: "Fixed."}), "", opened.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	file, page := publishedFile(t, fake)
	if open := file.OpenNotices(); len(open) != 1 || open[0].Updates[0].Message != "A second problem." || file.Notices[1].ResolvedAt == nil {
		t.Fatalf("status.json lost a post: %+v", file.Notices)
	}
	want, err := RenderPage(file)
	if err != nil {
		t.Fatal(err)
	}
	if page != string(want) {
		t.Fatalf("index.html does not show the latest status.json:\n%s", page)
	}
}

func TestWrongAccessKeyIsExplained(t *testing.T) {
	bucket, _ := testBucket(t)
	bucket.AccessKeyID = "another-key"
	_, _, err := Publish(context.Background(), bucket, PostNotice(incident("Hello")), "Example Inc.", opened)
	if err == nil || !strings.Contains(err.Error(), "check whether the R2 access key has read and write permissions for the status page's bucket") || strings.Contains(err.Error(), "fake-secret") {
		t.Fatalf("wrong key: %v", err)
	}
}

func change(t *testing.T, file *File, c Change, at time.Time) int64 {
	t.Helper()
	id, err := c(file, at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func refusal(t *testing.T, file *File, c Change, want string) {
	t.Helper()
	before, _ := json.Marshal(file)
	_, err := c(file, opened)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want a refusal with %q", err, want)
	}
	if after, _ := json.Marshal(file); string(before) != string(after) {
		t.Fatalf("a refused change changed the file:\n%s\n%s", before, after)
	}
}

func TestANoticeNeedsATitleAnImpactAndAFittingStage(t *testing.T) {
	file := &File{}
	good := PostNoticeInput{Title: "Sign-in delays", Impact: ImpactDegraded, Message: "Slow."}
	for name, test := range map[string]struct {
		edit func(*PostNoticeInput)
		want string
	}{
		"an empty title":          {func(in *PostNoticeInput) { in.Title = "  " }, "the title must have 1 to 140 characters on one line"},
		"a two-line title":        {func(in *PostNoticeInput) { in.Title = "Two\nlines" }, "on one line"},
		"a title of 141 letters":  {func(in *PostNoticeInput) { in.Title = strings.Repeat("é", 141) }, "1 to 140"},
		"no impact":               {func(in *PostNoticeInput) { in.Impact = "" }, "the impact must be outage, degraded, or maintenance"},
		"an unknown impact":       {func(in *PostNoticeInput) { in.Impact = "down" }, "the impact must be"},
		"a maintenance stage":     {func(in *PostNoticeInput) { in.Stage = StageScheduled }, `the stage "scheduled" is not a stage of degraded notices`},
		"an unknown stage":        {func(in *PostNoticeInput) { in.Stage = "fixed" }, `the stage "fixed" is not a stage of degraded notices`},
		"an incident stage":       {func(in *PostNoticeInput) { in.Impact, in.Stage = ImpactMaintenance, StageMonitoring }, `the stage "monitoring" is not a stage of maintenance notices`},
		"a window on an incident": {func(in *PostNoticeInput) { in.ScheduledStartAt = pointer(opened) }, "only for a maintenance notice"},
		"an end equal to a start": {func(in *PostNoticeInput) {
			in.Impact, in.ScheduledStartAt, in.ScheduledEndAt = ImpactMaintenance, pointer(opened), pointer(opened)
		}, "the planned end must be after the planned start"},
		"an end before the start": {func(in *PostNoticeInput) {
			in.Impact, in.ScheduledStartAt, in.ScheduledEndAt = ImpactMaintenance, pointer(opened), pointer(opened.Add(-time.Hour))
		}, "the planned end must be after the planned start"},
		"an end in the same second": {func(in *PostNoticeInput) {
			in.Impact, in.ScheduledStartAt, in.ScheduledEndAt = ImpactMaintenance, pointer(opened), pointer(opened.Add(900*time.Millisecond))
		}, "the planned end must be after the planned start"},
	} {
		in := good
		test.edit(&in)
		t.Run(name, func(t *testing.T) { refusal(t, file, PostNotice(in), test.want) })
	}
	for _, in := range []PostNoticeInput{
		{Title: strings.Repeat("x", 140), Impact: ImpactOutage, Message: "m"},
		{Title: "t", Impact: ImpactMaintenance, Stage: StageInProgress, Message: "m", ScheduledStartAt: pointer(opened), ScheduledEndAt: pointer(opened.Add(time.Minute))},
		{Title: "t", Impact: ImpactMaintenance, Message: "m", ScheduledEndAt: pointer(opened)},
	} {
		if _, err := PostNotice(in)(&File{}, opened); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

func TestIDsGoUpAndAreNeverUsedAgain(t *testing.T) {
	// The next ID is above every notice's ID and above next_notice_id, which
	// remembers a notice that was removed from status.json.
	file := &File{NextNoticeID: 10, Notices: []Notice{{NoticeID: 3}}}
	if id := change(t, file, PostNotice(incident("a")), opened); id != 10 || file.NextNoticeID != 11 {
		t.Fatalf("notice %d, next %d", id, file.NextNoticeID)
	}
	file = &File{NextNoticeID: 2, Notices: []Notice{{NoticeID: 7}, {NoticeID: 5}}}
	if id := change(t, file, PostNotice(incident("a")), opened); id != 8 || file.NextNoticeID != 9 {
		t.Fatalf("notice %d, next %d", id, file.NextNoticeID)
	}
	if id := change(t, &File{}, PostNotice(incident("a")), opened); id != 1 {
		t.Fatalf("first notice %d", id)
	}
	notice := &file.Notices[0]
	if notice.NoticeID != 8 || notice.NextUpdateID != 2 || notice.Updates[0].UpdateID != 1 || !notice.OpenedAt.Equal(opened) || !notice.UpdatedAt.Equal(opened) {
		t.Fatalf("new notice %+v", notice)
	}
	// An update gets an ID above the notice's updates and its next_update_id.
	notice.NextUpdateID = 20
	later := opened.Add(time.Hour)
	change(t, file, PostUpdate(PostUpdateInput{NoticeID: 8, Message: "b"}), later)
	notice = &file.Notices[0]
	if notice.Updates[1].UpdateID != 20 || notice.NextUpdateID != 21 || !notice.UpdatedAt.Equal(later) {
		t.Fatalf("update %+v", notice)
	}
	notice.NextUpdateID = 0
	notice.Updates[0].UpdateID = 30
	change(t, file, PostUpdate(PostUpdateInput{NoticeID: 8, Message: "c"}), later)
	if notice = &file.Notices[0]; notice.Updates[2].UpdateID != 31 || notice.NextUpdateID != 32 {
		t.Fatalf("update %+v", notice)
	}
}

func TestResolveRules(t *testing.T) {
	file := &File{}
	change(t, file, PostNotice(incident("Down.")), opened)
	change(t, file, PostNotice(PostNoticeInput{Title: "Upgrade", Impact: ImpactMaintenance, Stage: StageScheduled, Message: "Planned."}), opened)
	// A stage needs a closing message, and must fit the notice.
	refusal(t, file, Resolve(ResolveInput{NoticeID: 1, Stage: StageResolved}), "a stage needs a closing message")
	refusal(t, file, Resolve(ResolveInput{NoticeID: 1, Stage: StageCompleted, ClosingMessage: "Done."}), `the stage "completed" is not a stage of outage notices`)
	refusal(t, file, Resolve(ResolveInput{NoticeID: 9}), "no notice 9")
	// With a closing message, the stage defaults to resolved, or completed.
	later := opened.Add(time.Hour)
	change(t, file, Resolve(ResolveInput{NoticeID: 1, ClosingMessage: "Fixed."}), later)
	change(t, file, Resolve(ResolveInput{NoticeID: 2, ClosingMessage: "Done."}), later)
	if got := file.Notices[1].Updates[1].Stage; got != StageResolved {
		t.Errorf("an outage closes with the stage %q", got)
	}
	if got := file.Notices[0].Updates[1].Stage; got != StageCompleted {
		t.Errorf("maintenance closes with the stage %q", got)
	}
	// Resolving again changes nothing, and a closing message that would be
	// dropped is refused instead.
	before, _ := json.Marshal(file)
	change(t, file, Resolve(ResolveInput{NoticeID: 1}), later.Add(time.Hour))
	refusal(t, file, Resolve(ResolveInput{NoticeID: 1, ClosingMessage: "Again."}), "notice 1 is already resolved; use edit to change it")
	refusal(t, file, Resolve(ResolveInput{NoticeID: 1, Stage: StageResolved, ClosingMessage: "Again."}), "notice 1 is already resolved; use edit to change it")
	if after, _ := json.Marshal(file); string(before) != string(after) {
		t.Errorf("resolving a resolved notice changed it:\n%s\n%s", before, after)
	}
	// Without a closing message the notice closes with no new update.
	change(t, file, PostNotice(incident("Down again.")), later)
	change(t, file, Resolve(ResolveInput{NoticeID: 3}), later.Add(time.Minute))
	if notice := file.Notices[0]; notice.ResolvedAt == nil || len(notice.Updates) != 1 || !notice.UpdatedAt.Equal(later.Add(time.Minute)) {
		t.Errorf("resolved without a message: %+v", notice)
	}
}

func TestAChangeRefusesAnUnclearNoticeID(t *testing.T) {
	file := &File{Notices: []Notice{{NoticeID: 1, Impact: ImpactOutage, Updates: []Update{{UpdateID: 1, Message: "a"}}}, {NoticeID: 1, Impact: ImpactOutage}}}
	refusal(t, file, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "b"}), "more than one notice 1")
	refusal(t, file, Resolve(ResolveInput{NoticeID: 1}), "more than one notice 1")
	file = &File{Notices: []Notice{{NoticeID: 1, Impact: ImpactDegraded, Updates: []Update{{UpdateID: 1, Message: "a"}}}}}
	refusal(t, file, PostUpdate(PostUpdateInput{NoticeID: 1, Stage: StageScheduled, Message: "b"}), `the stage "scheduled" is not a stage of degraded notices`)
}

func TestTheOverallStatusAndTheOrderOfOpenNotices(t *testing.T) {
	resolvedAt := opened
	notice := func(id int64, impact Impact, minutes int) Notice {
		return Notice{NoticeID: id, Impact: impact, OpenedAt: opened.Add(time.Duration(minutes) * time.Minute)}
	}
	for _, test := range []struct {
		name    string
		notices []Notice
		status  OverallStatus
		order   []int64
	}{
		{"none", nil, StatusOperational, nil},
		{"only resolved", []Notice{{NoticeID: 1, Impact: ImpactOutage, ResolvedAt: &resolvedAt}}, StatusOperational, nil},
		{"maintenance", []Notice{notice(1, ImpactMaintenance, 0)}, StatusMaintenance, []int64{1}},
		{"degraded over maintenance", []Notice{notice(2, ImpactMaintenance, 5), notice(1, ImpactDegraded, 0)}, StatusDegraded, []int64{1, 2}},
		{"outage over all", []Notice{notice(4, ImpactMaintenance, 30), notice(3, ImpactDegraded, 20), notice(2, ImpactOutage, 10), notice(1, ImpactMaintenance, 0)}, StatusOutage, []int64{3, 2, 4, 1}},
		{"newest first, then the higher ID", []Notice{notice(1, ImpactOutage, 0), notice(2, ImpactOutage, 0), notice(3, ImpactDegraded, 7)}, StatusOutage, []int64{3, 2, 1}},
		{"an impact written by hand is an outage", []Notice{notice(1, "", 0)}, StatusOutage, []int64{1}},
	} {
		file := File{Notices: test.notices}
		var order []int64
		for _, n := range file.OpenNotices() {
			order = append(order, n.NoticeID)
		}
		if file.OverallStatus() != test.status || !slices.Equal(order, test.order) {
			t.Errorf("%s: status %s and order %v, want %s and %v", test.name, file.OverallStatus(), order, test.status, test.order)
		}
	}
}

// The banner and the icon follow the overall status; the notices' cards
// follow their own impact. The maintenance banner says nothing about time.
func TestThePageShowsEachStatusAndImpact(t *testing.T) {
	file := File{PageTitle: "Example Inc.", PublishedAt: opened}
	for _, test := range []struct {
		notices                  []Notice
		color, tag, sentence     string
		cardClass, cardTag, text string
	}{
		{nil, "1f9d5c", "Working", "Everything is working.", "", "", ""},
		{[]Notice{{NoticeID: 1, Title: "Database upgrade", Impact: ImpactMaintenance, ScheduledStartAt: pointer(opened.Add(24 * time.Hour)), OpenedAt: opened, Updates: []Update{{UpdateID: 1, Stage: StageScheduled, Message: "Planned.", PostedAt: opened}}}},
			"2348b3", "Maintenance", "Scheduled maintenance", "tag work", "Maintenance", "Database upgrade Planned start: 27 October 2026, 08:00 UTC Started 26 October 2026, 08:00 UTC 08:00 Scheduled Planned."},
		{[]Notice{{NoticeID: 1, Title: "Slow sign-in", Impact: ImpactDegraded, OpenedAt: opened, Updates: []Update{{UpdateID: 1, Message: "Slow.", PostedAt: opened}}}},
			"b86e00", "Degraded", "Some systems are experiencing issues.", "tag warn", "Degraded", "Slow sign-in Started 26 October 2026, 08:00 UTC 08:00 Slow."},
		{[]Notice{{NoticeID: 1, Title: "Down", Impact: ImpactOutage, OpenedAt: opened, Updates: []Update{{UpdateID: 1, Message: "Down.", PostedAt: opened}}}},
			"c4231c", "Outage", "We are experiencing a service outage.", "tag sos", "Outage", "Down Started 26 October 2026, 08:00 UTC 08:00 Down."},
	} {
		file.Notices = test.notices
		page, err := RenderPage(file)
		if err != nil {
			t.Fatal(err)
		}
		text := pageText(string(page))
		if !strings.Contains(string(page), "fill='%23"+test.color+"'") || !inOrder(text, test.tag, test.sentence, "Last updated 26 October 2026, 08:00 UTC") {
			t.Errorf("%s: icon or banner:\n%s", test.tag, text)
		}
		if test.cardTag != "" && (!strings.Contains(string(page), `<span class="`+test.cardClass+`">`+test.cardTag+`</span>`) || !strings.Contains(text, test.text)) {
			t.Errorf("%s: card:\n%s", test.tag, text)
		}
	}
}

// A notice that was posted without a title or updates, which only a file
// edited by hand holds, still renders.
func TestThePageRendersANoticeWithoutUpdates(t *testing.T) {
	resolvedAt := opened.Add(time.Hour)
	for _, file := range []File{
		{PageTitle: "Example Inc.", Notices: []Notice{{NoticeID: 1, OpenedAt: opened}}},
		{PageTitle: "Example Inc.", Notices: []Notice{{NoticeID: 1, OpenedAt: opened, ResolvedAt: &resolvedAt, Impact: ImpactMaintenance}}},
	} {
		if _, err := RenderPage(file); err != nil {
			t.Errorf("%+v: %v", file, err)
		}
	}
}

// The page is refused before the upload when status.json would be larger
// than the limit MMHotline reads back, so the notices stay readable.
func TestAPostThatMakesStatusJSONTooLargeIsRefusedBeforeTheUpload(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, PostNotice(incident("First.")), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	// Grow status.json to just under the limit with a long first message,
	// as a file edited by hand could.
	file, _ := publishedFile(t, fake)
	update := &file.Notices[0].Updates[0]
	update.Message = ""
	empty, _ := json.MarshalIndent(file, "", "  ")
	update.Message = strings.Repeat("x", maximumStatusJSONBytes-len(empty)-200)
	large, _ := json.MarshalIndent(file, "", "  ")
	fake.PutObject("mmhotline-status-example-com", "status.json", append(large, '\n'))
	before, _ := fake.Object("mmhotline-status-example-com", "status.json")
	if len(before.Content) >= maximumStatusJSONBytes || len(before.Content) < maximumStatusJSONBytes-1000 {
		t.Fatalf("test file of %d bytes", len(before.Content))
	}
	calls := len(fake.Calls())
	_, _, err := Publish(ctx, bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: strings.Repeat("y", 1000)}), "", opened.Add(time.Minute))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "more than the 16777216 bytes MMHotline reads back") {
		t.Fatalf("an oversized post: %v", err)
	}
	for _, call := range fake.Calls()[calls:] {
		if strings.HasPrefix(call, "PUT ") {
			t.Errorf("an oversized post uploaded: %s", call)
		}
	}
	if after, _ := fake.Object("mmhotline-status-example-com", "status.json"); !bytes.Equal(after.Content, before.Content) {
		t.Fatal("an oversized post changed status.json")
	}
	// A small post still fits, and what it writes reads back.
	if _, _, err := Publish(ctx, bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Short."}), "", opened.Add(time.Minute)); err != nil {
		t.Fatalf("a small post: %v", err)
	}
	if _, err := Read(ctx, bucket); err != nil {
		t.Fatalf("the file written under the limit does not read back: %v", err)
	}
}

func TestCheckWebsiteURLAndCheckSupportURL(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", 481)
	for _, test := range []struct {
		raw, website, support string // the result of each check; "" with wantErr
		websiteRefused        bool
		supportRefused        bool
	}{
		{raw: "", website: "", support: ""},
		{raw: "   ", website: "", support: ""},
		{raw: " https://example.com/about ", website: "https://example.com/about", support: "https://example.com/about"},
		{raw: "https://example.com:8443/help?topic=a#top", website: "https://example.com:8443/help?topic=a#top", support: "https://example.com:8443/help?topic=a#top"},
		{raw: "mailto:support@example.com", websiteRefused: true, support: "mailto:support@example.com"},
		{raw: "http://example.com", websiteRefused: true, supportRefused: true},
		{raw: "javascript:alert(1)", websiteRefused: true, supportRefused: true},
		{raw: "data:text/html,hi", websiteRefused: true, supportRefused: true},
		{raw: "example.com", websiteRefused: true, supportRefused: true},
		{raw: "https://", websiteRefused: true, supportRefused: true},
		{raw: "https:///path", websiteRefused: true, supportRefused: true},
		{raw: "https://user:password@example.com", websiteRefused: true, supportRefused: true},
		{raw: "https://user@example.com", websiteRefused: true, supportRefused: true},
		{raw: "https://exa mple.com", websiteRefused: true, supportRefused: true},
		{raw: "https://example.com/\nnext", websiteRefused: true, supportRefused: true},
		{raw: "https://example.com/\x00", websiteRefused: true, supportRefused: true},
		{raw: "https://example.com/\xff", websiteRefused: true, supportRefused: true},
		{raw: long, websiteRefused: true, supportRefused: true},
		{raw: "mailto:", websiteRefused: true, supportRefused: true},
		{raw: "mailto:Support <support@example.com>", websiteRefused: true, supportRefused: true},
		{raw: "mailto:support@example.com?subject=Hi", websiteRefused: true, supportRefused: true},
		{raw: "mailto:support@example.com#top", websiteRefused: true, supportRefused: true},
		{raw: "mailto:a@example.com,b@example.com", websiteRefused: true, supportRefused: true},
		{raw: "mailto:not-an-address", websiteRefused: true, supportRefused: true},
		{raw: "mailto://support@example.com", websiteRefused: true, supportRefused: true},
		{raw: "mailto:a%0Ab@example.com", websiteRefused: true, supportRefused: true},
		{raw: "mailto:a%20b@example.com", websiteRefused: true, supportRefused: true},
		{raw: "mailto:bad%ZZ@example.com", websiteRefused: true, supportRefused: true},
		{raw: "mailto:support%40example.com", websiteRefused: true, support: "mailto:support%40example.com"},
	} {
		website, websiteErr := CheckWebsiteURL(test.raw)
		support, supportErr := CheckSupportURL(test.raw)
		if website != test.website || (websiteErr != nil) != test.websiteRefused || (websiteErr != nil && !errors.Is(websiteErr, ErrRefused)) {
			t.Errorf("CheckWebsiteURL(%q) = %q, %v", test.raw, website, websiteErr)
		}
		if support != test.support || (supportErr != nil) != test.supportRefused || (supportErr != nil && !errors.Is(supportErr, ErrRefused)) {
			t.Errorf("CheckSupportURL(%q) = %q, %v", test.raw, support, supportErr)
		}
	}
	// The exact limit of 500 characters fits.
	if _, err := CheckWebsiteURL("https://example.com/" + strings.Repeat("a", 480)); err != nil {
		t.Errorf("a link of 500 characters: %v", err)
	}
}

func TestReadRefusesAnEmptyBucketWithErrNotPublished(t *testing.T) {
	bucket, _ := testBucket(t)
	if _, err := Read(context.Background(), bucket); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("Read of an empty bucket: %v", err)
	}
}

func TestCheckComponents(t *testing.T) {
	good := Component{ComponentID: "api", Name: "API"}
	checked, err := CheckComponents([]Component{{ComponentID: " website ", Name: " Website ", Description: "  Main site  "}, good})
	if err != nil || len(checked) != 2 || checked[0] != (Component{ComponentID: "website", Name: "Website", Description: "Main site"}) || checked[1] != good {
		t.Fatalf("good components: %+v, %v", checked, err)
	}
	if checked, err := CheckComponents(nil); err != nil || len(checked) != 0 {
		t.Fatalf("no components: %+v, %v", checked, err)
	}
	many := make([]Component, 51)
	for index := range many {
		many[index] = Component{ComponentID: "c" + strings.Repeat("x", index), Name: "Name"}
	}
	if _, err := CheckComponents(many[:50]); err != nil {
		t.Errorf("50 components: %v", err)
	}
	for name, test := range map[string]struct {
		components []Component
		want       string
	}{
		"51 components":          {many, "at most 50 components"},
		"an empty ID":            {[]Component{{Name: "API"}}, "components[0].component_id must have 1 to 64"},
		"an uppercase ID":        {[]Component{good, {ComponentID: "Api", Name: "API"}}, "components[1].component_id"},
		"an ID with a space":     {[]Component{{ComponentID: "my api", Name: "API"}}, "components[0].component_id"},
		"an ID starting with -":  {[]Component{{ComponentID: "-api", Name: "API"}}, "components[0].component_id"},
		"an ID of 65 letters":    {[]Component{{ComponentID: strings.Repeat("a", 65), Name: "API"}}, "components[0].component_id"},
		"a repeated ID":          {[]Component{good, {ComponentID: " api", Name: "Another"}}, `components[1].component_id "api" is used twice`},
		"no name":                {[]Component{{ComponentID: "api", Name: "  "}}, "components[0].name must have 1 to 100"},
		"a two-line name":        {[]Component{{ComponentID: "api", Name: "A\nB"}}, "components[0].name"},
		"a name of 101 letters":  {[]Component{{ComponentID: "api", Name: strings.Repeat("é", 101)}}, "components[0].name"},
		"a two-line description": {[]Component{{ComponentID: "api", Name: "API", Description: "A\r\nB"}}, "components[0].description must have at most 200"},
		"a long description":     {[]Component{{ComponentID: "api", Name: "API", Description: strings.Repeat("é", 201)}}, "components[0].description"},
	} {
		if _, err := CheckComponents(test.components); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v, want a refusal with %q", name, err, test.want)
		}
	}
	for _, edge := range []Component{{ComponentID: strings.Repeat("a", 64), Name: strings.Repeat("é", 100), Description: strings.Repeat("é", 200)}, {ComponentID: "0_-", Name: "N"}} {
		if _, err := CheckComponents([]Component{edge}); err != nil {
			t.Errorf("%+v: %v", edge, err)
		}
	}
}

func TestApplyPageConfigSetsTheTitleLinksAndComponentsAndKeepsTheNotices(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	config := PageConfig{PageTitle: " Example Inc. status ", HostName: "status.example.com", WebsiteURL: "https://example.com", SupportURL: "mailto:support@example.com",
		Components: []Component{{ComponentID: "website", Name: "Website", Description: "Main site and customer dashboard"}, {ComponentID: "api", Name: "API"}}}
	// The first apply starts a missing status.json.
	if _, _, err := Publish(ctx, bucket, ApplyPageConfig(config), config.PageTitle, opened); err != nil {
		t.Fatal(err)
	}
	object, _ := fake.Object("mmhotline-status-example-com", "status.json")
	for _, want := range []string{`"page_title": "Example Inc. status"`, `"host_name": "status.example.com"`, `"website_url": "https://example.com"`, `"support_url": "mailto:support@example.com"`,
		`"components": [`, `"component_id": "website"`, `"name": "Website"`, `"description": "Main site and customer dashboard"`, `"component_id": "api"`} {
		if !strings.Contains(string(object.Content), want) {
			t.Errorf("status.json lacks %s:\n%s", want, object.Content)
		}
	}
	// A component without a description leaves its key out.
	if strings.Count(string(object.Content), `"description"`) != 1 {
		t.Errorf("status.json writes an empty description:\n%s", object.Content)
	}
	if _, _, err := Publish(ctx, bucket, PostNotice(PostNoticeInput{Title: "Slow API", Impact: ImpactDegraded, AffectedComponentIDs: []string{"api"}, Message: "Slow."}), "", opened); err != nil {
		t.Fatal(err)
	}
	// A later apply without links or components removes them; the notice stays,
	// and its component ID reads as the ID.
	if _, _, err := Publish(ctx, bucket, ApplyPageConfig(PageConfig{PageTitle: "Example Inc. status", HostName: "status.example.com"}), "", opened.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	file, page := publishedFile(t, fake)
	if file.WebsiteURL != "" || file.SupportURL != "" || file.Components != nil || len(file.Notices) != 1 || !slices.Equal(file.Notices[0].AffectedComponentIDs, []string{"api"}) || file.NextNoticeID != 2 {
		t.Fatalf("after removing the links and components: %+v", file)
	}
	object, _ = fake.Object("mmhotline-status-example-com", "status.json")
	for _, absent := range []string{"website_url", "support_url", `"components"`} {
		if strings.Contains(string(object.Content), absent) {
			t.Errorf("status.json keeps %s:\n%s", absent, object.Content)
		}
	}
	if text := pageText(page); strings.Contains(text, "Website") || !inOrder(text, "Slow API", "api", "Started") {
		t.Errorf("index.html after removing the components: %s", text)
	}
	// Refused configurations change nothing and upload nothing.
	before, _ := fake.Object("mmhotline-status-example-com", "status.json")
	calls := len(fake.Calls())
	for name, bad := range map[string]PageConfig{
		"a title":     {PageTitle: "Two\nlines"},
		"a website":   {PageTitle: "T", WebsiteURL: "http://example.com"},
		"a support":   {PageTitle: "T", SupportURL: "mailto:Name <a@example.com>"},
		"a component": {PageTitle: "T", Components: []Component{{ComponentID: "Bad", Name: "Bad"}}},
		"a repeat":    {PageTitle: "T", Components: []Component{{ComponentID: "a", Name: "A"}, {ComponentID: "a", Name: "B"}}},
	} {
		if _, _, err := Publish(ctx, bucket, ApplyPageConfig(bad), "", opened); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	after, _ := fake.Object("mmhotline-status-example-com", "status.json")
	if !bytes.Equal(before.Content, after.Content) {
		t.Error("a refused configuration changed status.json")
	}
	for _, call := range fake.Calls()[calls:] {
		if strings.HasPrefix(call, "PUT ") {
			t.Errorf("a refused configuration uploaded: %s", call)
		}
	}
}

func TestAComponentIsOperationalUnlessAnOpenNoticeNamesIt(t *testing.T) {
	components := []Component{{ComponentID: "website", Name: "Website"}, {ComponentID: "api", Name: "API"}, {ComponentID: "mail", Name: "Mail"}, {ComponentID: "files", Name: "Files"}}
	resolvedAt := opened
	notice := func(id int64, impact Impact, stage Stage, components ...string) Notice {
		return Notice{NoticeID: id, Impact: impact, AffectedComponentIDs: components, OpenedAt: opened, Updates: []Update{{UpdateID: 1, Stage: stage, Message: "m", PostedAt: opened}}}
	}
	statusesOf := func(notices ...Notice) map[string]OverallStatus {
		result := map[string]OverallStatus{}
		for _, c := range (File{Components: components, Notices: notices}).ComponentStatuses() {
			result[c.Component.ComponentID] = c.Status
		}
		return result
	}
	for name, test := range map[string]struct {
		notices                   []Notice
		website, api, mail, files OverallStatus
	}{
		"no notices":                             {nil, StatusOperational, StatusOperational, StatusOperational, StatusOperational},
		"an outage":                              {[]Notice{notice(1, ImpactOutage, "", "website")}, StatusOutage, StatusOperational, StatusOperational, StatusOperational},
		"a degraded notice":                      {[]Notice{notice(1, ImpactDegraded, StageInvestigating, "api")}, StatusOperational, StatusDegraded, StatusOperational, StatusOperational},
		"an impact written by hand is an outage": {[]Notice{notice(1, "", "", "mail")}, StatusOperational, StatusOperational, StatusOutage, StatusOperational},
		"a resolved notice":                      {[]Notice{{NoticeID: 1, Impact: ImpactOutage, AffectedComponentIDs: []string{"website"}, OpenedAt: opened, ResolvedAt: &resolvedAt}}, StatusOperational, StatusOperational, StatusOperational, StatusOperational},
		"a notice without components":            {[]Notice{notice(1, ImpactOutage, "")}, StatusOperational, StatusOperational, StatusOperational, StatusOperational},
		"maintenance that is only scheduled":     {[]Notice{notice(1, ImpactMaintenance, StageScheduled, "website", "api")}, StatusOperational, StatusOperational, StatusOperational, StatusOperational},
		"maintenance in progress":                {[]Notice{notice(1, ImpactMaintenance, StageInProgress, "website")}, StatusMaintenance, StatusOperational, StatusOperational, StatusOperational},
		"maintenance without a stage":            {[]Notice{notice(1, ImpactMaintenance, "", "website")}, StatusMaintenance, StatusOperational, StatusOperational, StatusOperational},
		"the worst of overlapping notices": {[]Notice{
			notice(1, ImpactMaintenance, StageInProgress, "website", "api", "mail"),
			notice(2, ImpactDegraded, StageIdentified, "website", "api"),
			notice(3, ImpactOutage, StageInvestigating, "website"),
			notice(4, ImpactMaintenance, StageScheduled, "files"),
		}, StatusOutage, StatusDegraded, StatusMaintenance, StatusOperational},
		"a retired component changes nothing": {[]Notice{notice(1, ImpactOutage, "", "retired")}, StatusOperational, StatusOperational, StatusOperational, StatusOperational},
	} {
		got := statusesOf(test.notices...)
		if got["website"] != test.website || got["api"] != test.api || got["mail"] != test.mail || got["files"] != test.files {
			t.Errorf("%s: %v", name, got)
		}
	}
	// Components keep their configured order, and a page without any has none.
	statuses := File{Components: components}.ComponentStatuses()
	if len(statuses) != 4 || statuses[0].Component.ComponentID != "website" || statuses[3].Component.ComponentID != "files" {
		t.Errorf("order: %+v", statuses)
	}
	if got := (File{}).ComponentStatuses(); len(got) != 0 {
		t.Errorf("no components: %+v", got)
	}
	// Only the latest update's stage decides: a maintenance notice that moves
	// from scheduled to in progress starts the maintenance, and the notice
	// closing ends it.
	file := &File{Components: components}
	change(t, file, PostNotice(PostNoticeInput{Title: "Upgrade", Impact: ImpactMaintenance, Stage: StageScheduled, AffectedComponentIDs: []string{"website"}, Message: "Planned."}), opened)
	if got := file.ComponentStatuses()[0].Status; got != StatusOperational {
		t.Errorf("scheduled maintenance: %s", got)
	}
	change(t, file, PostUpdate(PostUpdateInput{NoticeID: 1, Stage: StageInProgress, Message: "Started."}), opened.Add(time.Hour))
	if got := file.ComponentStatuses()[0].Status; got != StatusMaintenance {
		t.Errorf("maintenance in progress: %s", got)
	}
	change(t, file, Resolve(ResolveInput{NoticeID: 1, ClosingMessage: "Done."}), opened.Add(2*time.Hour))
	if got := file.ComponentStatuses()[0].Status; got != StatusOperational {
		t.Errorf("completed maintenance: %s", got)
	}
}

func TestANoticeNamesOnlyComponentsThePageHas(t *testing.T) {
	file := &File{Components: []Component{{ComponentID: "website", Name: "Website"}, {ComponentID: "api", Name: "API"}, {ComponentID: "mail", Name: "Mail"}}}
	in := PostNoticeInput{Title: "Slow", Impact: ImpactDegraded, Message: "Slow."}
	in.AffectedComponentIDs = []string{"cdn"}
	refusal(t, file, PostNotice(in), `the status page has no component "cdn"; its components are website, api, mail`)
	in.AffectedComponentIDs = []string{"api", "cdn"}
	refusal(t, file, PostNotice(in), `no component "cdn"`)
	refusal(t, &File{}, PostNotice(in), `the status page has no component "api"; it has no components`)
	// Each component once, in the order of the page's components.
	in.AffectedComponentIDs = []string{"mail", "website", "mail", "website"}
	change(t, file, PostNotice(in), opened)
	if got := file.Notices[0].AffectedComponentIDs; !slices.Equal(got, []string{"website", "mail"}) {
		t.Errorf("affected components %v", got)
	}
	in.AffectedComponentIDs = nil
	change(t, file, PostNotice(in), opened)
	if got := file.Notices[0].AffectedComponentIDs; got != nil {
		t.Errorf("a notice without components has %v", got)
	}
	// An update names no components, and a retired component keeps its ID as its name.
	file.Components = file.Components[:1]
	if file.ComponentName("website") != "Website" || file.ComponentName("mail") != "mail" {
		t.Errorf("names %q and %q", file.ComponentName("website"), file.ComponentName("mail"))
	}
}

// The page's header links come from status.json, which a person may edit by
// hand, so a link that is not https:// (or mailto: for support) is left out.
func TestThePageShowsItsHeaderLinksAndComponents(t *testing.T) {
	file := File{PageTitle: "Example Inc.", PublishedAt: opened, WebsiteURL: "https://example.com", SupportURL: "mailto:support@example.com",
		Components: []Component{{ComponentID: "website", Name: "Website", Description: "Main site <and> dashboard"}, {ComponentID: "api", Name: "API"}},
		Notices:    []Notice{{NoticeID: 1, Title: "Slow API", Impact: ImpactDegraded, AffectedComponentIDs: []string{"api", "retired"}, OpenedAt: opened, Updates: []Update{{UpdateID: 1, Message: "Slow.", PostedAt: opened}}}}}
	page, err := RenderPage(file)
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if !strings.Contains(html, `<a href="https://example.com">Website</a>`) || !strings.Contains(html, `<a href="mailto:support@example.com">Support</a>`) {
		t.Errorf("the header links:\n%s", html)
	}
	if !strings.Contains(html, `<span class="chip">API</span>`) || !strings.Contains(html, `<span class="chip">retired</span>`) || strings.Count(html, `class="chips"`) != 1 {
		t.Errorf("the notice's chips:\n%s", html)
	}
	if !strings.Contains(html, `<section class="panel" aria-label="Components">`) || !strings.Contains(html, `<span class="tag warn">Degraded</span>`) || !strings.Contains(html, `<span class="tag ok">Operational</span>`) || !strings.Contains(html, "Main site &lt;and&gt; dashboard") {
		t.Errorf("the components panel:\n%s", html)
	}
	// The Components panel sits below the open notices and above Recently resolved.
	resolvedAt := opened.Add(time.Hour)
	file.Notices = append(file.Notices, Notice{NoticeID: 2, Title: "Old problem", Impact: ImpactOutage, AffectedComponentIDs: []string{"website"}, OpenedAt: opened, ResolvedAt: &resolvedAt, Updates: []Update{{UpdateID: 1, Message: "Old.", PostedAt: opened}}})
	page, _ = RenderPage(file)
	if text := pageText(string(page)); !inOrder(text, "Slow API", "Website Main site <and> dashboard Operational", "API Degraded", "Recently resolved", "Old problem", "Website") {
		t.Errorf("order of the panels: %s", text)
	}
	// A page without components has no panel; links that fail the check are left out.
	for _, hand := range []File{
		{PageTitle: "Example Inc.", WebsiteURL: "javascript:alert(1)", SupportURL: "http://example.com"},
		{PageTitle: "Example Inc.", WebsiteURL: "https://user:pass@example.com", SupportURL: "mailto:a@example.com?subject=x"},
	} {
		page, err := RenderPage(hand)
		if err != nil {
			t.Fatal(err)
		}
		if html := string(page); strings.Contains(html, "<nav") || strings.Contains(html, "javascript") || strings.Contains(html, "aria-label=\"Components\"") || strings.Contains(html, "Website") || strings.Contains(html, "Support") {
			t.Errorf("a bad link or an empty components list is shown:\n%s", html)
		}
	}
}
