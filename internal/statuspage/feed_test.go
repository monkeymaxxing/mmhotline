package statuspage

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakecloudflare"
	"mmhotline/internal/htmlgolden"
)

// parsedFeed is feed.xml read back as an Atom reader would read it.
type parsedFeed struct {
	XMLName xml.Name `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string   `xml:"id"`
	Title   string   `xml:"title"`
	Updated string   `xml:"updated"`
	Author  struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Links []struct {
		Rel  string `xml:"rel,attr"`
		Type string `xml:"type,attr"`
		Href string `xml:"href,attr"`
	} `xml:"link"`
	Entries []struct {
		ID        string `xml:"id"`
		Title     string `xml:"title"`
		Updated   string `xml:"updated"`
		Published string `xml:"published"`
		Link      struct {
			Rel  string `xml:"rel,attr"`
			Type string `xml:"type,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
		Content struct {
			Type string `xml:"type,attr"`
			Text string `xml:",chardata"`
		} `xml:"content"`
	} `xml:"entry"`
}

func parseFeed(t *testing.T, data []byte) parsedFeed {
	t.Helper()
	var feed parsedFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		t.Fatalf("feed.xml is not Atom XML: %v\n%s", err, data)
	}
	return feed
}

// feedFixture is a page with an open outage, a resolved degraded notice that
// names components and has an edited update, and a maintenance notice with a
// planned window.
func feedFixture() File {
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	resolvedAt := at(29, 9, 30)
	completedAt := at(27, 3, 0)
	return File{
		PageTitle: "Example Inc. status", HostName: "status.example.com", PublishedAt: at(30, 12, 0),
		Components: []Component{{ComponentID: "website", Name: "Website"}, {ComponentID: "api", Name: "API"}},
		Notices: []Notice{
			{NoticeID: 3, Title: "Orders are delayed", Impact: ImpactOutage, AffectedComponentIDs: []string{"website", "retired"}, OpenedAt: at(30, 11, 0), UpdatedAt: at(30, 11, 40), NextUpdateID: 3, Updates: []Update{
				{UpdateID: 1, Stage: StageInvestigating, Message: "Orders are delayed.\nWe are looking into it.", PostedAt: at(30, 11, 0), EditedAt: pointer(at(30, 11, 5))},
				{UpdateID: 2, Stage: StageIdentified, Message: "We found the cause: a full disk.", PostedAt: at(30, 11, 40)},
			}},
			{NoticeID: 2, Title: "Sign-in failures", Impact: ImpactDegraded, AffectedComponentIDs: []string{"api"}, OpenedAt: at(28, 22, 0), UpdatedAt: resolvedAt, ResolvedAt: &resolvedAt, NextUpdateID: 3, Updates: []Update{
				{UpdateID: 1, Stage: StageInvestigating, Message: "Sign-ins fail for some customers.", PostedAt: at(28, 22, 0)},
				{UpdateID: 2, Stage: StageResolved, Message: "Sign-ins work again.", PostedAt: resolvedAt},
			}},
			{NoticeID: 4, Title: "Database upgrade", Impact: ImpactMaintenance, ScheduledStartAt: pointer(at(31, 2, 0)), ScheduledEndAt: pointer(at(31, 4, 0)), OpenedAt: at(29, 12, 0), UpdatedAt: at(29, 12, 0), NextUpdateID: 2, Updates: []Update{
				{UpdateID: 1, Stage: StageScheduled, Message: "We will upgrade the database.", PostedAt: at(29, 12, 0)},
			}},
			{NoticeID: 1, Title: "Network maintenance", Impact: ImpactMaintenance, OpenedAt: at(26, 9, 0), UpdatedAt: completedAt, ResolvedAt: &completedAt, NextUpdateID: 2, Updates: []Update{
				{UpdateID: 1, Stage: StageCompleted, Message: "The switch is replaced.", PostedAt: completedAt},
			}},
		},
	}
}

// checkFeedGolden compares feed.xml with its golden file byte for byte; the
// feed has no whitespace that varies. Running with -update rewrites the file.
func checkFeedGolden(t *testing.T, name string, feed []byte) {
	t.Helper()
	path := filepath.Join("testdata", "feeds", name+".xml")
	if htmlgolden.Updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, feed, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run the test with -update to write it)", name, err)
	}
	if !bytes.Equal(want, feed) {
		t.Errorf("%s differs from %s:\n%s", name, path, feed)
	}
}

func TestFeedGolden(t *testing.T) {
	for name, file := range map[string]File{
		"feed__no_notices": {PageTitle: "Example Inc. status", HostName: "status.example.com", PublishedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), Notices: []Notice{}},
		"feed__open_resolved_and_maintenance_notices": feedFixture(),
	} {
		feed, err := RenderFeed(file)
		if err != nil {
			t.Fatal(err)
		}
		checkFeedGolden(t, name, feed)
	}
}

func TestFeedIsAnAtomFeedEvenWithoutNotices(t *testing.T) {
	feed, err := RenderFeed(File{PageTitle: "Example Inc. status", HostName: "status.example.com", PublishedAt: opened})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(feed), `<?xml version="1.0" encoding="utf-8"?>`+"\n"+`<feed xmlns="http://www.w3.org/2005/Atom">`) {
		t.Fatalf("feed.xml starts: %.120s", feed)
	}
	parsed := parseFeed(t, feed)
	if parsed.ID != "https://status.example.com/" || parsed.Title != "Example Inc. status" || parsed.Updated != "2026-10-26T08:00:00Z" || parsed.Author.Name != "Example Inc. status" || len(parsed.Entries) != 0 {
		t.Fatalf("empty feed: %+v", parsed)
	}
	if len(parsed.Links) != 2 ||
		parsed.Links[0].Rel != "self" || parsed.Links[0].Type != "application/atom+xml" || parsed.Links[0].Href != "https://status.example.com/feed.xml" ||
		parsed.Links[1].Rel != "alternate" || parsed.Links[1].Type != "text/html" || parsed.Links[1].Href != "https://status.example.com/" {
		t.Fatalf("feed links: %+v", parsed.Links)
	}
}

func TestFeedEntriesDescribeEachNotice(t *testing.T) {
	feed, err := RenderFeed(feedFixture())
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseFeed(t, feed)
	if parsed.Updated != "2026-10-30T12:00:00Z" || len(parsed.Entries) != 4 {
		t.Fatalf("feed: %+v", parsed)
	}
	// Newest change first: 3 (30 Oct 11:40), 2 (29 Oct 9:30), 4 (29 Oct 12:00) sorts before 2.
	var ids, titles []string
	for _, entry := range parsed.Entries {
		ids = append(ids, entry.ID)
		titles = append(titles, entry.Title)
	}
	wantIDs := []string{"https://status.example.com/#notice-3", "https://status.example.com/#notice-4", "https://status.example.com/#notice-2", "https://status.example.com/#notice-1"}
	wantTitles := []string{"[Outage] Orders are delayed", "[Maintenance] Database upgrade", "[Resolved] Sign-in failures", "[Completed] Network maintenance"}
	if strings.Join(ids, "\n") != strings.Join(wantIDs, "\n") || strings.Join(titles, "\n") != strings.Join(wantTitles, "\n") {
		t.Fatalf("entry ids %v and titles %v", ids, titles)
	}
	first := parsed.Entries[0]
	if first.Updated != "2026-10-30T11:40:00Z" || first.Published != "2026-10-30T11:00:00Z" || first.Link.Rel != "alternate" || first.Link.Type != "text/html" || first.Link.Href != first.ID || first.Content.Type != "text" {
		t.Fatalf("first entry: %+v", first)
	}
	text := first.Content.Text
	if !inOrder(text, "Status: Open", "Impact: Outage", "Affected: Website, retired", "2026-10-30 11:40 UTC · Identified", "We found the cause", "2026-10-30 11:00 UTC · Investigating (edited)", "Orders are delayed.\nWe are looking into it.") {
		t.Fatalf("first entry's text:\n%s", text)
	}
	if text := parsed.Entries[1].Content.Text; !inOrder(text, "Status: Open", "Impact: Maintenance", "Planned: 31 October 2026, 02:00 to 31 October 2026, 04:00 UTC", "Scheduled") {
		t.Fatalf("maintenance entry's text:\n%s", text)
	}
	if text := parsed.Entries[2].Content.Text; !inOrder(text, "Status: Resolved", "Impact: Degraded", "Affected: API", "Resolved", "Sign-ins work again.", "Investigating") {
		t.Fatalf("resolved entry's text:\n%s", text)
	}
	if text := parsed.Entries[3].Content.Text; !strings.Contains(text, "Status: Completed") {
		t.Fatalf("completed entry's text:\n%s", text)
	}
}

func TestFeedListsTheTwentyMostRecentlyChangedNoticesAndBreaksTiesByID(t *testing.T) {
	file := File{PageTitle: "Example Inc. status", HostName: "status.example.com", PublishedAt: opened}
	for id := int64(1); id <= 25; id++ {
		file.Notices = append(file.Notices, Notice{NoticeID: id, Title: fmt.Sprintf("Notice %d", id), Impact: ImpactDegraded, OpenedAt: opened, UpdatedAt: opened.Add(time.Duration(id) * time.Minute)})
	}
	// Notices 7 and 8 changed at the same moment; the higher ID is first.
	file.Notices[6].UpdatedAt = file.Notices[7].UpdatedAt
	feed, err := RenderFeed(file)
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseFeed(t, feed)
	if len(parsed.Entries) != 20 || parsed.Entries[0].Title != "[Degraded] Notice 25" || parsed.Entries[19].Title != "[Degraded] Notice 6" {
		t.Fatalf("%d entries, first %q, last %q", len(parsed.Entries), parsed.Entries[0].Title, parsed.Entries[len(parsed.Entries)-1].Title)
	}
	for index, entry := range parsed.Entries {
		if entry.Title == "[Degraded] Notice 8" && parsed.Entries[index+1].Title != "[Degraded] Notice 7" {
			t.Fatalf("notices changed at the same moment: %q then %q", entry.Title, parsed.Entries[index+1].Title)
		}
	}
}

func TestFeedEscapesTextFromNotices(t *testing.T) {
	file := File{PageTitle: "A&B <status>", HostName: "status.example.com", PublishedAt: opened, Notices: []Notice{
		{NoticeID: 1, Title: "Sign-in <b>fails</b> & more", Impact: ImpactOutage, OpenedAt: opened, UpdatedAt: opened, Updates: []Update{
			{UpdateID: 1, Message: "<script>alert(1)</script> a & b > c", PostedAt: opened},
		}},
	}}
	feed, err := RenderFeed(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(feed), "<script>") || strings.Contains(string(feed), "<b>") || strings.Contains(string(feed), "A&B") {
		t.Fatalf("feed.xml does not escape text:\n%s", feed)
	}
	parsed := parseFeed(t, feed)
	if parsed.Title != "A&B <status>" || parsed.Entries[0].Title != "[Outage] Sign-in <b>fails</b> & more" || !strings.Contains(parsed.Entries[0].Content.Text, "<script>alert(1)</script> a & b > c") {
		t.Fatalf("feed.xml does not read back as written: %+v", parsed)
	}
}

func TestFeedNeedsAHostName(t *testing.T) {
	if _, err := RenderFeed(File{PageTitle: "Example Inc. status"}); err == nil || !strings.Contains(err.Error(), "host name") {
		t.Fatalf("a feed without a host name: %v", err)
	}
}

func TestAPageWithoutAHostNameUploadsNoFeed(t *testing.T) {
	bucket, fake := testBucket(t)
	if _, _, err := Publish(context.Background(), bucket, PostNotice(incident("First.")), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	if _, found := fake.Object("mmhotline-status-example-com", "feed.xml"); found {
		t.Fatal("a feed was uploaded for a page without a host name")
	}
	if _, found := fake.Object("mmhotline-status-example-com", "index.html"); !found {
		t.Fatal("index.html was not uploaded")
	}
}

// publishedWithHostName starts a page whose status.json has a host name, as
// settings apply makes it, with one open notice.
func publishedWithHostName(t *testing.T) (*R2Bucket, *fakecloudflare.Server) {
	t.Helper()
	bucket, fake := testBucket(t)
	ctx := context.Background()
	if _, _, err := Publish(ctx, bucket, ApplyPageConfig(PageConfig{PageTitle: "Example Inc.", HostName: "status.example.com"}), "Example Inc.", opened); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Publish(ctx, bucket, PostNotice(incident("First.")), "", opened.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return bucket, fake
}

// publishedFeed is the stored feed.xml, after checking its content type.
func publishedFeed(t *testing.T, fake *fakecloudflare.Server) []byte {
	t.Helper()
	object, found := fake.Object("mmhotline-status-example-com", "feed.xml")
	if !found {
		t.Fatal("feed.xml was not uploaded")
	}
	if object.ContentType != "application/atom+xml; charset=utf-8" {
		t.Fatalf("feed.xml's content type is %q", object.ContentType)
	}
	return object.Content
}

func TestAPostUploadsTheFeedWithThePage(t *testing.T) {
	_, fake := publishedWithHostName(t)
	file, page := publishedFile(t, fake)
	wantFeed, err := RenderFeed(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(publishedFeed(t, fake)) != string(wantFeed) {
		t.Fatalf("feed.xml does not match status.json:\n%s", publishedFeed(t, fake))
	}
	if !strings.Contains(page, `href="feed.xml"`) {
		t.Fatalf("index.html does not link feed.xml:\n%s", page)
	}
}

// otherPostBeforeFeed is a second responder whose whole post runs between this
// post's index.html upload and its feed.xml upload, each time `times` allows.
type otherPostBeforeFeed struct {
	other *R2Bucket
	times int
	posts int
}

func (o *otherPostBeforeFeed) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/feed.xml") && o.posts < o.times {
		o.posts++
		if _, _, err := Publish(request.Context(), o.other, PostNotice(incident(fmt.Sprintf("Another problem %d.", o.posts))), "", opened.Add(time.Duration(10+o.posts)*time.Minute)); err != nil {
			return nil, err
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}

func TestPageAndFeedShowTheLatestStatusJSONAfterAPostBetweenTheirUploads(t *testing.T) {
	bucket, fake := publishedWithHostName(t)
	other := *bucket
	bucket.Client = &http.Client{Transport: &otherPostBeforeFeed{other: &other, times: 1}}
	if _, _, err := Publish(context.Background(), bucket, Resolve(ResolveInput{NoticeID: 1, ClosingMessage: "Fixed."}), "", opened.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	file, page := publishedFile(t, fake)
	if len(file.Notices) != 2 || file.Notices[1].ResolvedAt == nil {
		t.Fatalf("status.json lost a post: %+v", file.Notices)
	}
	wantPage, _ := RenderPage(file)
	wantFeed, _ := RenderFeed(file)
	if page != string(wantPage) || string(publishedFeed(t, fake)) != string(wantFeed) {
		t.Fatalf("index.html or feed.xml does not show the latest status.json:\n%s", publishedFeed(t, fake))
	}
	if !strings.Contains(string(publishedFeed(t, fake)), "Another problem 1.") {
		t.Fatal("feed.xml misses the other post")
	}
}

func TestAPostThatKeepsBeingOvertakenSaysSoAndKeepsTheChange(t *testing.T) {
	bucket, fake := publishedWithHostName(t)
	other := *bucket
	bucket.Client = &http.Client{Transport: &otherPostBeforeFeed{other: &other, times: publishAttempts}}
	_, _, err := Publish(context.Background(), bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Second."}), "", opened.Add(30*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "saved in status.json") || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("error: %v", err)
	}
	file, _ := publishedFile(t, fake)
	notice, err := findNotice(&file, 1)
	if err != nil || len(notice.Updates) != 2 || len(file.Notices) != 4 {
		t.Fatalf("the update or another post was not kept in status.json: %v %+v", err, file.Notices)
	}
}

// failingUpload answers a PUT of one file with an R2 error and records which
// files were sent.
type failingUpload struct {
	failingKey string
	sent       []string
}

func (f *failingUpload) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPut {
		key := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		f.sent = append(f.sent, key)
		if key == f.failingKey {
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<Error><Code>InternalError</Code></Error>")), Request: request}, nil
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}

func TestAFailedIndexHTMLUploadDoesNotSkipTheFeed(t *testing.T) {
	bucket, fake := publishedWithHostName(t)
	transport := &failingUpload{failingKey: "index.html"}
	bucket.Client = &http.Client{Transport: transport}
	_, noticeID, err := Publish(context.Background(), bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Second."}), "", opened.Add(30*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "saved in status.json") || !strings.Contains(err.Error(), "upload of index.html,") || strings.Contains(err.Error(), "feed.xml") {
		t.Fatalf("error: %v", err)
	}
	if noticeID != 1 || strings.Join(transport.sent, " ") != "status.json index.html feed.xml" {
		t.Fatalf("notice %d, uploads %v", noticeID, transport.sent)
	}
	if !strings.Contains(string(publishedFeed(t, fake)), "Second.") {
		t.Fatal("feed.xml does not show the post")
	}
}

func TestAFailedFeedUploadIsNamed(t *testing.T) {
	bucket, fake := publishedWithHostName(t)
	transport := &failingUpload{failingKey: "feed.xml"}
	bucket.Client = &http.Client{Transport: transport}
	_, _, err := Publish(context.Background(), bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Second."}), "", opened.Add(30*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "saved in status.json") || !strings.Contains(err.Error(), "upload of feed.xml,") || strings.Contains(err.Error(), "index.html and") {
		t.Fatalf("error: %v", err)
	}
	_, page := publishedFile(t, fake)
	if !strings.Contains(page, "Second.") {
		t.Fatal("index.html does not show the post")
	}
}

func TestBothFailedUploadsAreNamed(t *testing.T) {
	bucket, _ := publishedWithHostName(t)
	bucket.Client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPut && !strings.HasSuffix(request.URL.Path, "/status.json") {
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}
		return http.DefaultTransport.RoundTrip(request)
	})}
	_, _, err := Publish(context.Background(), bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Second."}), "", opened.Add(30*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "upload of index.html and feed.xml,") {
		t.Fatalf("error: %v", err)
	}
}

// A failed feed.xml upload must not stop the re-read of status.json. Here
// another post saves a newer status.json and uploads its files after this
// post's index.html went up, then this post's feed.xml upload fails; the post
// reads status.json, sees the newer one, and repairs both files.
func TestAFailedFeedUploadStillRechecksStatusJSONAndRepairsBothFiles(t *testing.T) {
	bucket, fake := publishedWithHostName(t)
	other := *bucket
	fails := true
	bucket.Client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/feed.xml") && fails {
			fails = false
			if _, _, err := Publish(request.Context(), &other, PostNotice(incident("Newer outage.")), "", opened.Add(10*time.Minute)); err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}
		return http.DefaultTransport.RoundTrip(request)
	})}
	if _, _, err := Publish(context.Background(), bucket, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Second."}), "", opened.Add(30*time.Minute)); err != nil {
		t.Fatalf("the repair should have succeeded: %v", err)
	}
	file, page := publishedFile(t, fake)
	if len(file.Notices) != 2 {
		t.Fatalf("status.json lost a post: %+v", file.Notices)
	}
	wantPage, _ := RenderPage(file)
	wantFeed, _ := RenderFeed(file)
	if page != string(wantPage) || string(publishedFeed(t, fake)) != string(wantFeed) {
		t.Fatalf("index.html or feed.xml does not show the latest status.json:\n%s", publishedFeed(t, fake))
	}
	if !strings.Contains(page, "Newer outage.") || !strings.Contains(string(publishedFeed(t, fake)), "Newer outage.") {
		t.Fatal("a file lost the newer outage")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
