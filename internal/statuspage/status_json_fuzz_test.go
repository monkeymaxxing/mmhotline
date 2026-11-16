package statuspage

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// FuzzStatusJSON reads a fuzzed status.json from the bucket, which a laptop
// with the R2 keys can also write, and applies one post as Publish does: a
// new notice, an update, or a resolution. A file that does not read gives no
// notices. A refused post is ErrRefused. After a post the file still holds
// every notice it held, each with its updates in order; a new notice's ID is
// one more than the highest so far, and no other notice has it
// (docs/changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md). The file MMHotline then saves
// reads back as the same file, and the page renders from it.
func FuzzStatusJSON(f *testing.F) {
	designExample := `{
  "page_title": "Example Inc.",
  "published_at": "2026-10-26T08:09:30Z",
  "notices": [
    {
      "notice_id": 1,
      "opened_at": "2026-10-26T08:00:00Z",
      "resolved_at": null,
      "updates": [
        {"message": "We are looking into a problem.", "posted_at": "2026-10-26T08:00:00Z"},
        {"message": "Sign-in fails for some customers. We are moving the database to its replica.", "posted_at": "2026-10-26T08:09:30Z"}
      ]
    }
  ]
}`
	f.Add([]byte(designExample), int64(0), "We are looking into a problem.", false)
	f.Add([]byte(designExample), int64(1), "The database runs on its replica.", false)
	f.Add([]byte(designExample), int64(1), "", true)
	f.Add([]byte(`{"page_title":"Example Inc.","notices":[{"notice_id":3,"opened_at":"2026-10-26T08:00:00+05:30","resolved_at":"2026-10-26T09:00:00Z","updates":[]}]}`), int64(3), "Again", false)
	f.Add([]byte(`{"page_title":"Example Inc.","notices":null}`), int64(0), " \n ", false)
	f.Add([]byte(`not JSON`), int64(0), "x", false)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte, noticeID int64, message string, resolve bool) {
		file, err := decodeStatusJSON(data)
		if err != nil {
			if file.Notices != nil || file.PageTitle != "" {
				t.Fatalf("a refusal gave the file %+v", file)
			}
			return
		}
		before, err := json.Marshal(file)
		if err != nil {
			return // A file whose times cannot be written again; Publish reports it.
		}
		var earlier File
		if err := json.Unmarshal(before, &earlier); err != nil {
			t.Fatal(err)
		}
		change := Post(noticeID, message)
		if resolve {
			change = Resolve(noticeID, message)
		}
		changed, err := change(&file, now)
		if err != nil {
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("a post failed without a refusal: %v", err)
			}
			return
		}
		// A file another writer edited may hold two notices with one ID, so
		// each earlier notice is matched in order, not by its ID alone.
		next := 0
		for _, notice := range earlier.Notices {
			for next < len(file.Notices) && !keepsUpdates(file.Notices[next], notice) {
				next++
			}
			if next == len(file.Notices) {
				t.Fatalf("the post lost notice %d or changed its updates", notice.NoticeID)
			}
			next++
		}
		if noticeID == 0 && !resolve {
			for _, notice := range earlier.Notices {
				if notice.NoticeID >= changed {
					t.Fatalf("the new notice %d is not above notice %d", changed, notice.NoticeID)
				}
			}
			if len(file.Notices) != len(earlier.Notices)+1 || file.Notices[0].NoticeID != changed {
				t.Fatalf("the new notice %d is not the newest of %d", changed, len(file.Notices))
			}
		}
		file.PublishedAt = now
		saved, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			t.Fatalf("the changed file cannot be saved: %v", err)
		}
		again, err := decodeStatusJSON(saved)
		if err != nil {
			t.Fatalf("the saved file does not read back: %v", err)
		}
		if savedAgain, err := json.MarshalIndent(again, "", "  "); err != nil || !bytes.Equal(savedAgain, saved) {
			t.Fatalf("the saved file reads back as another file: %v\n%s\n%s", err, saved, savedAgain)
		}
		if _, err := RenderPage(file); err != nil {
			t.Fatalf("the page does not render: %v", err)
		}
	})
}

// keepsUpdates reports whether after is the notice before, with its updates
// in order and perhaps more after them.
func keepsUpdates(after, before Notice) bool {
	if after.NoticeID != before.NoticeID || len(after.Updates) < len(before.Updates) {
		return false
	}
	for index, update := range before.Updates {
		if after.Updates[index].Message != update.Message {
			return false
		}
	}
	return true
}
