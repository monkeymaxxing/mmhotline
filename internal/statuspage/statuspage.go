// Package statuspage builds and publishes a team's public status page: two
// files, index.html and status.json, in an R2 bucket in the team's own
// Cloudflare account. status.json holds the whole notice history and is the
// page's only state. A post reads status.json, applies one change, and
// uploads both files. MMHotline's service and the mmhotline command on a
// responder's laptop both post through Publish, so the files are always
// built the same way (docs/changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md).
package statuspage

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrRefused marks a post that breaks a rule of the page, such as an empty
// message or an update to a resolved notice. Callers show its text.
var ErrRefused = errors.New("refused")

// ErrNotPublished means the bucket holds no status.json yet: the page was
// never published by set-up on the MMHotline server.
var ErrNotPublished = errors.New("the status page has no status.json yet; apply the settings file with a status_page section on the MMHotline server to publish the page")

const (
	// maximumMessageCharacters keeps one update readable on a phone; it is
	// the limit people see on MMHotline's form and in the command's help.
	maximumMessageCharacters = 5000
	// maximumPageTitleCharacters fits a company or product name in a heading.
	maximumPageTitleCharacters = 100
	// recentlyResolvedOnPage is how many resolved notices index.html shows;
	// status.json keeps them all.
	recentlyResolvedOnPage = 5
	// maximumStatusJSONBytes bounds the read of status.json. A notice with 20
	// updates at their longest is about 300 KB in JSON, so 16 MiB holds years
	// of real notices.
	maximumStatusJSONBytes = 16 << 20
	// publishAttempts bounds the retries when another post changed
	// status.json between this post's read and write, and the uploads of
	// index.html when another post changed status.json meanwhile.
	publishAttempts = 3
)

// File is status.json: the page's title, when it was last published, and
// every notice, newest first.
type File struct {
	PageTitle   string    `json:"page_title"`
	PublishedAt time.Time `json:"published_at"`
	Notices     []Notice  `json:"notices"`
}

// Notice is one problem the team tells its customers about. ResolvedAt is
// nil while the notice is open.
type Notice struct {
	NoticeID   int64      `json:"notice_id"`
	OpenedAt   time.Time  `json:"opened_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	Updates    []Update   `json:"updates"`
}

// Update is one message of a notice, shown as plain text with its line breaks.
type Update struct {
	Message  string    `json:"message"`
	PostedAt time.Time `json:"posted_at"`
}

// OpenNotices returns the notices that are not resolved, newest first.
func (f File) OpenNotices() []Notice {
	var open []Notice
	for _, n := range f.Notices {
		if n.ResolvedAt == nil {
			open = append(open, n)
		}
	}
	return open
}

// ResolvedNotices returns at most limit resolved notices, most recently
// resolved first.
func (f File) ResolvedNotices(limit int) []Notice {
	var resolved []Notice
	for _, n := range f.Notices {
		if n.ResolvedAt != nil {
			resolved = append(resolved, n)
		}
	}
	sort.SliceStable(resolved, func(i, j int) bool { return resolved[i].ResolvedAt.After(*resolved[j].ResolvedAt) })
	if len(resolved) > limit {
		resolved = resolved[:limit]
	}
	return resolved
}

// Change is one post: it changes the file in place at the time now, and
// returns the ID of the notice it changed.
type Change func(file *File, now time.Time) (int64, error)

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

func checkedMessage(message, label string, optional bool) (string, error) {
	message = strings.TrimSpace(message)
	length := utf8.RuneCountInString(message)
	if !utf8.ValidString(message) || length > maximumMessageCharacters || (length == 0 && !optional) {
		return "", refused("the %s must have 1 to %d characters", label, maximumMessageCharacters)
	}
	return message, nil
}

// CheckPageTitle returns the title without surrounding spaces, or why it is refused.
func CheckPageTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	length := utf8.RuneCountInString(title)
	if !utf8.ValidString(title) || length == 0 || length > maximumPageTitleCharacters || strings.ContainsAny(title, "\r\n") {
		return "", refused("the page title must have 1 to %d characters on one line", maximumPageTitleCharacters)
	}
	return title, nil
}

// PostNotice opens a new notice with its first message.
func PostNotice(message string) Change {
	return func(file *File, now time.Time) (int64, error) {
		message, err := checkedMessage(message, "message", false)
		if err != nil {
			return 0, err
		}
		var id int64 = 1
		for _, n := range file.Notices {
			if n.NoticeID >= id {
				id = n.NoticeID + 1
			}
		}
		file.Notices = append([]Notice{{NoticeID: id, OpenedAt: now, Updates: []Update{{Message: message, PostedAt: now}}}}, file.Notices...)
		return id, nil
	}
}

func findNotice(file *File, noticeID int64) (*Notice, error) {
	for index := range file.Notices {
		if file.Notices[index].NoticeID == noticeID {
			return &file.Notices[index], nil
		}
	}
	return nil, refused("the status page has no notice %d", noticeID)
}

// Post is what `mmhotline status-page post` and the page's forms do: a new
// notice when noticeID is 0, otherwise an update to that notice.
func Post(noticeID int64, message string) Change {
	if noticeID == 0 {
		return PostNotice(message)
	}
	return PostUpdate(noticeID, message)
}

// PostUpdate adds a message to an open notice. Earlier messages stay on the
// page as the notice's history.
func PostUpdate(noticeID int64, message string) Change {
	return func(file *File, now time.Time) (int64, error) {
		message, err := checkedMessage(message, "message", false)
		if err != nil {
			return 0, err
		}
		notice, err := findNotice(file, noticeID)
		if err != nil {
			return 0, err
		}
		if notice.ResolvedAt != nil {
			return 0, refused("notice %d is resolved; post a new notice instead", noticeID)
		}
		notice.Updates = append(notice.Updates, Update{Message: message, PostedAt: now})
		return noticeID, nil
	}
}

// Resolve marks a notice resolved, with an optional closing message as its
// last update. Resolving a resolved notice changes nothing.
func Resolve(noticeID int64, closingMessage string) Change {
	return func(file *File, now time.Time) (int64, error) {
		closingMessage, err := checkedMessage(closingMessage, "closing message", true)
		if err != nil {
			return 0, err
		}
		notice, err := findNotice(file, noticeID)
		if err != nil {
			return 0, err
		}
		if notice.ResolvedAt != nil {
			return noticeID, nil
		}
		resolved := now
		notice.ResolvedAt = &resolved
		if closingMessage != "" {
			notice.Updates = append(notice.Updates, Update{Message: closingMessage, PostedAt: now})
		}
		return noticeID, nil
	}
}

// SetPageTitle changes the page's title; set-up uses it.
func SetPageTitle(title string) Change {
	return func(file *File, _ time.Time) (int64, error) {
		title, err := CheckPageTitle(title)
		if err != nil {
			return 0, err
		}
		file.PageTitle = title
		return 0, nil
	}
}

// Read returns the published status.json, or ErrNotPublished.
func Read(ctx context.Context, bucket *R2Bucket) (File, error) {
	file, _, err := read(ctx, bucket)
	return file, err
}

func read(ctx context.Context, bucket *R2Bucket) (File, string, error) {
	data, etag, err := bucket.get(ctx, "status.json")
	if err != nil {
		return File{}, "", err
	}
	file, err := decodeStatusJSON(data)
	if err != nil {
		return File{}, "", err
	}
	return file, etag, nil
}

// decodeStatusJSON reads status.json as the bucket holds it.
func decodeStatusJSON(data []byte) (File, error) {
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, errors.New("status.json in the bucket is not valid JSON; MMHotline does not overwrite it")
	}
	if file.Notices == nil {
		file.Notices = []Notice{}
	}
	return file, nil
}

// Publish applies change to status.json and uploads both files. When
// status.json is missing, newPageTitle starts a new file; without one,
// Publish refuses with ErrNotPublished, so a laptop cannot start a second
// page. Another post that changed status.json between this post's read and
// its write makes the write fail its If-Match check; Publish then reads the
// newer file and applies change again, so neither post is lost. The File it
// returns is the newest status.json it saw.
func Publish(ctx context.Context, bucket *R2Bucket, change Change, newPageTitle string, now time.Time) (File, int64, error) {
	now = now.UTC().Truncate(time.Second)
	for attempt := 1; ; attempt++ {
		file, etag, err := read(ctx, bucket)
		if errors.Is(err, errObjectMissing) {
			if newPageTitle == "" {
				return File{}, 0, ErrNotPublished
			}
			file, etag, err = File{PageTitle: newPageTitle, Notices: []Notice{}}, "", nil
		}
		if err != nil {
			return File{}, 0, err
		}
		noticeID, err := change(&file, now)
		if err != nil {
			return File{}, 0, err
		}
		file.PublishedAt = now
		content, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return File{}, 0, err
		}
		etag, err = bucket.put(ctx, "status.json", append(content, '\n'), "application/json", etag)
		if errors.Is(err, errPreconditionFailed) && attempt < publishAttempts {
			continue
		}
		if errors.Is(err, errPreconditionFailed) {
			return File{}, 0, errors.New("status.json kept changing, so the post was not saved; post again")
		}
		if err != nil {
			return File{}, 0, err
		}
		file, err = publishPage(ctx, bucket, file, etag)
		return file, noticeID, err
	}
}

// publishPage uploads index.html built from file, the status.json saved with
// the given ETag. Another post may save a newer status.json and upload its
// index.html before this one arrives, so after each upload publishPage reads
// status.json again; when it changed, it builds index.html from the newer
// file and uploads it again. The index.html uploaded last therefore shows
// the latest status.json. publishPage returns the newest file it read.
func publishPage(ctx context.Context, bucket *R2Bucket, file File, etag string) (File, error) {
	for upload := 1; ; upload++ {
		page, err := RenderPage(file)
		if err != nil {
			return file, err
		}
		if _, err := bucket.put(ctx, "index.html", page, "text/html; charset=utf-8", ""); err != nil {
			return file, fmt.Errorf("the post is saved in status.json, but MMHotline could not confirm the index.html upload, so the status page may show the post only after the next post: %w", err)
		}
		latest, latestETag, err := read(ctx, bucket)
		if err != nil {
			return file, fmt.Errorf("the post is saved in status.json, but MMHotline could not check whether index.html shows the latest notices; the status page may need another post to show the saved notices: %w", err)
		}
		if latestETag == etag {
			return file, nil
		}
		if upload == publishAttempts {
			return latest, errors.New("the post is saved in status.json, but status.json kept changing during the index.html upload; the status page may need another post to show the latest notices")
		}
		file, etag = latest, latestETag
	}
}

// The page's times are absolute and in UTC, never "5 minutes ago", because
// the files change only at the next post. A notice's first line gives the
// whole date and time, such as "26 October 2026, 08:00 UTC"; its updates
// below give the time of day, with the day when it is not the notice's
// first: "08:09", "27 Oct 02:00".

func publishedTime(t time.Time) string { return t.UTC().Format("2 January 2006, 15:04 UTC") }

func sameDay(t, day time.Time) bool {
	return t.UTC().Format(time.DateOnly) == day.UTC().Format(time.DateOnly)
}

// updateTime writes the time of an update of a notice that opened on day.
func updateTime(t, day time.Time) string {
	if sameDay(t, day) {
		return t.UTC().Format("15:04")
	}
	return t.UTC().Format("2 Jan 15:04")
}

// timeSpan writes when a notice was open, such as "26 October 2026, 08:00
// to 08:30 UTC".
func timeSpan(from, to time.Time) string {
	if sameDay(from, to) {
		return from.UTC().Format("2 January 2006, 15:04") + " to " + to.UTC().Format("15:04 UTC")
	}
	return publishedTime(from) + " to " + publishedTime(to)
}

func isoTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// newestFirst returns the updates of a notice, newest first, as the page shows them.
func newestFirst(updates []Update) []Update {
	result := make([]Update, len(updates))
	for index, update := range updates {
		result[len(updates)-1-index] = update
	}
	return result
}

// pageStylesheet is the page's styles, inlined in index.html.
//
//go:embed status_page.css
var pageStylesheet string

// pageTemplate is index.html. Its heading is page_title exactly as written.
// An open notice is a problem now, with its updates newest first; with
// none, everything is working. The most recently resolved notices follow.
var pageTemplate = template.Must(template.New("index.html").Funcs(template.FuncMap{"publishedTime": publishedTime, "updateTime": updateTime, "timeSpan": timeSpan, "isoTime": isoTime, "newestFirst": newestFirst}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.PageTitle}}</title>
<style>
{{.Stylesheet}}</style>
</head>
<body>
<main class="public stack">
<h1>{{.PageTitle}}</h1>
{{- range .Open}}{{$opened := .OpenedAt}}{{$updates := newestFirst .Updates}}
<section class="state sos">
<span class="tag sos">Problem now</span>
<p class="big">We are working on a problem.</p>
<p class="faint small">Started <time datetime="{{isoTime .OpenedAt}}">{{publishedTime .OpenedAt}}</time>{{if gt (len $updates) 1}}{{with index $updates 0}} · last update <time datetime="{{isoTime .PostedAt}}">{{updateTime .PostedAt $opened}} UTC</time>{{end}}{{end}}</p>
<ol class="timeline">
{{- range $index, $update := $updates}}
<li{{if eq $index 0}} class="sos"{{end}}><time datetime="{{isoTime .PostedAt}}">{{updateTime .PostedAt $opened}}</time><span class="dot"></span><div>{{.Message}}</div></li>
{{- end}}
</ol>
</section>
{{- else}}
<section class="state ok">
<span class="tag ok">Working</span>
<p class="big">Everything is working.</p>
<p class="faint small">Last updated <time datetime="{{isoTime .PublishedAt}}">{{publishedTime .PublishedAt}}</time></p>
</section>
{{- end}}
{{- with .Resolved}}
<section class="resolved stack">
<h2>Recently resolved</h2>
{{- range .}}{{$opened := .OpenedAt}}
<article class="panel">
<div class="row spread"><span class="tag">Resolved</span><span class="faint small">{{timeSpan .OpenedAt .ResolvedAt}}</span></div>
<ol class="timeline">
{{- range $index, $update := newestFirst .Updates}}
<li{{if eq $index 0}} class="person"{{end}}><time datetime="{{isoTime .PostedAt}}">{{updateTime .PostedAt $opened}}</time><span class="dot"></span><div>{{.Message}}</div></li>
{{- end}}
</ol>
</article>
{{- end}}
</section>
{{- end}}
<footer class="faint small">Times are in UTC. The same notices for programs: <a href="status.json">status.json</a></footer>
</main>
</body>
</html>
`))

// RenderPage builds index.html from status.json's content: the open notices,
// or "Everything is working." when there are none, then the most recently
// resolved notices.
func RenderPage(file File) ([]byte, error) {
	var page bytes.Buffer
	err := pageTemplate.Execute(&page, struct {
		PageTitle      string
		Stylesheet     template.CSS
		PublishedAt    time.Time
		Open, Resolved []Notice
	}{file.PageTitle, template.CSS(pageStylesheet), file.PublishedAt, file.OpenNotices(), file.ResolvedNotices(recentlyResolvedOnPage)})
	return page.Bytes(), err
}
