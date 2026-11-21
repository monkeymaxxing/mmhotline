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
// above every earlier notice's and above next_notice_id, and no other notice
// has it (docs/changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md).
// The file MMHotline then saves reads back as the same file, and the page
// renders from it, also for a notice without updates.
func FuzzStatusJSON(f *testing.F) {
	designExample := `{
  "page_title": "Example Inc.",
  "host_name": "status.example.com",
  "published_at": "2026-10-26T08:09:30Z",
  "next_notice_id": 2,
  "notices": [
    {
      "notice_id": 1,
      "title": "Sign-in delays",
      "impact": "degraded",
      "opened_at": "2026-10-26T08:00:00Z",
      "updated_at": "2026-10-26T08:09:30Z",
      "resolved_at": null,
      "next_update_id": 3,
      "updates": [
        {"update_id": 1, "stage": "investigating", "message": "We are looking into a problem.", "posted_at": "2026-10-26T08:00:00Z"},
        {"update_id": 2, "message": "Sign-in fails for some customers. We are moving the database to its replica.", "posted_at": "2026-10-26T08:09:30Z", "edited_at": "2026-10-26T08:10:00Z"}
      ]
    }
  ]
}`
	// The arguments: the file, the notice ID, the message, the stage, the kind
	// of change (0 new notice, 1 update, 2 resolve), and the new notice's impact.
	f.Add([]byte(designExample), int64(0), "We are looking into a problem.", "investigating", uint8(0), "outage")
	f.Add([]byte(designExample), int64(0), "Planned.", "scheduled", uint8(0), "maintenance")
	f.Add([]byte(designExample), int64(1), "The database runs on its replica.", "identified", uint8(1), "")
	f.Add([]byte(designExample), int64(1), "", "", uint8(2), "")
	f.Add([]byte(designExample), int64(1), "Fixed.", "resolved", uint8(2), "")
	f.Add([]byte(`{"page_title":"Example Inc.","next_notice_id":9,"notices":[{"notice_id":3,"opened_at":"2026-10-26T08:00:00+05:30","resolved_at":"2026-10-26T09:00:00Z","updates":[]}]}`), int64(3), "Again", "", uint8(1), "")
	f.Add([]byte(`{"page_title":"Example Inc.","notices":[{"notice_id":3,"impact":"outage","opened_at":"2026-10-26T08:00:00Z","resolved_at":null,"updates":[]},{"notice_id":3,"opened_at":"2026-10-26T08:00:00Z","resolved_at":null,"updates":[]}]}`), int64(3), "Twice", "", uint8(1), "")
	f.Add([]byte(`{"page_title":"Example Inc.","notices":null}`), int64(0), " \n ", "", uint8(0), "outage")
	f.Add([]byte(`not JSON`), int64(0), "x", "", uint8(0), "outage")
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte, noticeID int64, message, stage string, kind uint8, impact string) {
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
		var change Change
		switch kind % 3 {
		case 0:
			change = PostNotice(PostNoticeInput{Title: "Fuzzed", Impact: Impact(impact), Stage: Stage(stage), Message: message})
		case 1:
			change = PostUpdate(PostUpdateInput{NoticeID: noticeID, Stage: Stage(stage), Message: message})
		default:
			change = Resolve(ResolveInput{NoticeID: noticeID, Stage: Stage(stage), ClosingMessage: message})
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
		if kind%3 == 0 {
			for _, notice := range earlier.Notices {
				if notice.NoticeID >= changed {
					t.Fatalf("the new notice %d is not above notice %d", changed, notice.NoticeID)
				}
			}
			if changed < earlier.NextNoticeID || file.NextNoticeID != changed+1 {
				t.Fatalf("the new notice %d and next_notice_id %d, from %d", changed, file.NextNoticeID, earlier.NextNoticeID)
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
		if after.Updates[index].Message != update.Message || after.Updates[index].UpdateID != update.UpdateID {
			return false
		}
	}
	return true
}
