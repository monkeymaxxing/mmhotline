package statuspage

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"os"
	"regexp"
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

// pageText is what a reader of index.html sees, without its head and tags,
// in one line.
func pageText(page string) string {
	body := page[strings.Index(page, "<body>"):]
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
	_, id, err := Publish(ctx, bucket, PostNotice("We are looking into a problem."), "Example Inc.", opened)
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
	if !inOrder(text, "Problem now", "We are working on a problem.", "Started 26 October 2026, 08:00 UTC", "We are looking into a problem.") {
		t.Errorf("index.html with an open notice: %s", text)
	}
	if strings.Contains(text, "Everything is working.") || strings.Contains(text, " ago") {
		t.Errorf("index.html with an open notice: %s", text)
	}

	if _, _, err = Publish(ctx, bucket, PostUpdate(1, "Sign-in fails for some customers.\nWe are moving the database."), "", opened.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Publish(ctx, bucket, Resolve(1, "Sign-in works again."), "", opened.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	file, page = publishedFile(t, fake)
	notice := file.Notices[0]
	if notice.ResolvedAt == nil || !notice.ResolvedAt.Equal(opened.Add(30*time.Minute)) || len(notice.Updates) != 3 {
		t.Fatalf("resolved notice %+v", notice)
	}
	if notice.Updates[1].Message != "Sign-in fails for some customers.\nWe are moving the database." || !notice.Updates[1].PostedAt.Equal(opened.Add(9*time.Minute)) {
		t.Fatalf("update kept as %+v", notice.Updates[1])
	}
	// Resolved, the page says everything works, and lists the notice under
	// Recently resolved with when it was open and its updates, newest first.
	text = pageText(page)
	if !inOrder(text, "Everything is working.", "Last updated 26 October 2026, 08:30 UTC", "Recently resolved", "Resolved", "26 October 2026, 08:00 to 08:30 UTC",
		"08:30", "Sign-in works again.", "08:09", "Sign-in fails for some customers.", "08:00", "We are looking into a problem.") || strings.Contains(text, "Problem now") {
		t.Errorf("index.html after the resolve: %s", text)
	}
	if !file.PublishedAt.Equal(opened.Add(30 * time.Minute)) {
		t.Fatalf("published_at %s", file.PublishedAt)
	}

	_, _, err = Publish(ctx, bucket, PostUpdate(1, "Too late."), "", opened.Add(time.Hour))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "post a new notice instead") {
		t.Fatalf("update to a resolved notice: %v", err)
	}
	_, id, err = Publish(ctx, bucket, PostNotice("A new problem."), "", opened.Add(time.Hour))
	if err != nil || id != 2 {
		t.Fatalf("second notice %d: %v", id, err)
	}
	file, _ = publishedFile(t, fake)
	if len(file.Notices) != 2 || file.Notices[0].NoticeID != 2 || file.Notices[1].NoticeID != 1 {
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
	if _, _, err := Publish(ctx, bucket, PostNotice("We are looking into a problem."), "Example Inc.", late.In(newYork)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Publish(ctx, bucket, PostUpdate(1, "Sign-in fails.\nWe are moving the database."), "", late.Add(25*time.Minute).In(newYork)); err != nil {
		t.Fatal(err)
	}
	_, page := publishedFile(t, fake)
	if text := pageText(page); !inOrder(text, "Problem now", "Started 26 October 2026, 23:50 UTC · last update 27 Oct 00:15 UTC",
		"27 Oct 00:15", "Sign-in fails. We are moving the database.", "23:50", "We are looking into a problem.", "Times are in UTC.") {
		t.Fatalf("index.html: %s", text)
	}
	// The message keeps its line break.
	if !strings.Contains(page, "Sign-in fails.\nWe are moving the database.") {
		t.Fatalf("index.html changed the message's line breaks:\n%s", page)
	}
}

// The published page's styles are MMHotline's own stylesheet's rules,
// copied: each rule of status_page.css appears in internal/app/mmhotline.css
// as written, so the public page keeps MMHotline's look and dark mode.
func TestPageStylesAreTheStylesheetsRules(t *testing.T) {
	stylesheet, err := os.ReadFile("../app/mmhotline.css")
	if err != nil {
		t.Fatal(err)
	}
	words := func(css string) string { return strings.Join(strings.Fields(css), " ") }
	whole := words(string(stylesheet))
	// Each top-level rule, an @media block whole, outside comments.
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(pageStylesheet, "")
	depth, start := 0, 0
	for index, character := range css {
		switch character {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				if rule := words(css[start : index+1]); !strings.Contains(whole, rule) {
					t.Errorf("status_page.css has a rule that internal/app/mmhotline.css does not have as written; copy it from there: %s", rule)
				}
				start = index + 1
			}
		}
	}
	if page, err := RenderPage(File{PageTitle: "Example Inc."}); err != nil || !strings.Contains(string(page), words(".state .big { font-size: 24px;")) {
		t.Fatalf("index.html does not carry the styles: %v\n%s", err, page)
	}
}

func TestStatusJSONFieldNames(t *testing.T) {
	bucket, fake := testBucket(t)
	if _, _, err := Publish(context.Background(), bucket, PostNotice("We are looking into a problem."), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	object, _ := fake.Object("mmhotline-status-example-com", "status.json")
	for _, want := range []string{`"page_title": "Example Inc."`, `"published_at": "2026-10-26T08:00:00Z"`, `"notice_id": 1`, `"opened_at": "2026-10-26T08:00:00Z"`, `"resolved_at": null`, `"message": "We are looking into a problem."`, `"posted_at": "2026-10-26T08:00:00Z"`} {
		if !strings.Contains(string(object.Content), want) {
			t.Errorf("status.json lacks %s:\n%s", want, object.Content)
		}
	}
}

func TestPostRefusesBadInputAndAMissingPage(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, PostNotice("Hello"), "", opened); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("post before set-up: %v", err)
	}
	if _, ok := fake.Object("mmhotline-status-example-com", "status.json"); ok {
		t.Fatal("a refused post uploaded status.json")
	}
	for _, message := range []string{"", "   \n", strings.Repeat("x", 5001)} {
		if _, _, err := Publish(ctx, bucket, PostNotice(message), "Example Inc.", opened); !errors.Is(err, ErrRefused) {
			t.Errorf("message of %d characters: %v", len(message), err)
		}
	}
	if _, _, err := Publish(ctx, bucket, PostNotice(strings.Repeat("é", 5000)), "Example Inc.", opened); err != nil {
		t.Fatalf("5,000 characters: %v", err)
	}
	if _, _, err := Publish(ctx, bucket, PostUpdate(9, "Hello"), "", opened); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no notice 9") {
		t.Fatalf("unknown notice: %v", err)
	}
	if _, err := CheckPageTitle("Two\nlines"); !errors.Is(err, ErrRefused) {
		t.Fatalf("two-line title: %v", err)
	}
}

func TestPageShowsMessagesAsText(t *testing.T) {
	bucket, fake := testBucket(t)
	if _, _, err := Publish(context.Background(), bucket, PostNotice(`<script>alert("x")</script>`), "Example <b>Inc.</b>", opened); err != nil {
		t.Fatal(err)
	}
	_, page := publishedFile(t, fake)
	if strings.Contains(page, "<script>alert") || strings.Contains(page, "<b>Inc.</b>") || !strings.Contains(page, "&lt;script&gt;") {
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
		PostNotice("Posted from a laptop at the same time.")(&file, opened.Add(time.Minute))
		content, _ := json.Marshal(file)
		c.fake.PutObject("mmhotline-status-example-com", "status.json", content)
	}
	return http.DefaultTransport.RoundTrip(request)
}

func TestPostsAtTheSameTimeAreBothKept(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, PostNotice("First."), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	bucket.Client = &http.Client{Transport: &changeBeforeFirstWrite{fake: fake}}
	if _, _, err := Publish(ctx, bucket, PostUpdate(1, "Second."), "", opened.Add(2*time.Minute)); err != nil {
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
		if _, _, err := Publish(request.Context(), o.other, PostNotice("A second problem."), "", opened.Add(31*time.Minute)); err != nil {
			return nil, err
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}

func TestPageShowsTheLatestStatusJSONAfterPostsAtTheSameMoment(t *testing.T) {
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, PostNotice("First."), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	other := *bucket
	bucket.Client = &http.Client{Transport: &otherPostBeforeIndexHTML{other: &other}}
	if _, _, err := Publish(ctx, bucket, Resolve(1, "Fixed."), "", opened.Add(30*time.Minute)); err != nil {
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
	_, _, err := Publish(context.Background(), bucket, PostNotice("Hello"), "Example Inc.", opened)
	if err == nil || !strings.Contains(err.Error(), "check whether the R2 access key has read and write permissions for the status page's bucket") || strings.Contains(err.Error(), "fake-secret") {
		t.Fatalf("wrong key: %v", err)
	}
}
