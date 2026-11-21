package statuspage

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func text(s string) *string { return &s }

// newEditableFile has a degraded notice that names a retired component, a
// maintenance notice with a window, and components website and api.
func newEditableFile(t *testing.T) *File {
	t.Helper()
	file := &File{Components: []Component{{ComponentID: "website", Name: "Website"}, {ComponentID: "api", Name: "API"}}}
	change(t, file, PostNotice(PostNoticeInput{Title: "Slow", Impact: ImpactDegraded, Message: "First."}), opened)
	change(t, file, PostNotice(PostNoticeInput{Title: "Upgrade", Impact: ImpactMaintenance, Stage: StageScheduled, Message: "Planned.", ScheduledStartAt: pointer(opened.Add(24 * time.Hour)), ScheduledEndAt: pointer(opened.Add(25 * time.Hour))}), opened)
	// Notice 1 names the retired component "old" and the listed one "api".
	file.Notices[1].AffectedComponentIDs = []string{"api", "old"}
	return file
}

func TestEditNoticeChangesOnlyWhatIsGiven(t *testing.T) {
	file := newEditableFile(t)
	later := opened.Add(time.Hour)
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 1, Title: text("  Slow sign-in ")}), later)
	notice := file.Notices[1]
	if notice.Title != "Slow sign-in" || notice.Impact != ImpactDegraded || !reflect.DeepEqual(notice.AffectedComponentIDs, []string{"api", "old"}) || !notice.UpdatedAt.Equal(later) || len(notice.Updates) != 1 {
		t.Fatalf("after a title edit: %+v", notice)
	}
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 1, Title: text("")}), "the title must have 1 to 140 characters")
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 1, Title: text("a\nb")}), "the title must have 1 to 140 characters")
	impact := Impact("bad")
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 1, Impact: &impact}), "the impact must be")
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 9, Title: text("x")}), "no notice 9")
}

func TestEditNoticeKeepsARetiredComponentAndChecksNewOnes(t *testing.T) {
	file := newEditableFile(t)
	// A retired component the notice already names may stay, after the listed
	// ones in the page's order; the others are replaced.
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 1, AffectedComponentIDs: &[]string{"old", "website", "api", "website"}}), opened.Add(time.Hour))
	if got := file.Notices[1].AffectedComponentIDs; !reflect.DeepEqual(got, []string{"website", "api", "old"}) {
		t.Fatalf("components %v", got)
	}
	// A new retired or unknown component is refused.
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 1, AffectedComponentIDs: &[]string{"website", "gone"}}), `the status page has no component "gone"`)
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 2, AffectedComponentIDs: &[]string{"old"}}), `the status page has no component "old"`)
	// The retired component can be removed, and an empty list clears all.
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 1, AffectedComponentIDs: &[]string{"api"}}), opened.Add(2*time.Hour))
	if got := file.Notices[1].AffectedComponentIDs; !reflect.DeepEqual(got, []string{"api"}) {
		t.Fatalf("components %v", got)
	}
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 1, AffectedComponentIDs: &[]string{}}), opened.Add(3*time.Hour))
	if got := file.Notices[1].AffectedComponentIDs; len(got) != 0 {
		t.Fatalf("components %v", got)
	}
}

func TestEditNoticeScheduleFollowsTheImpact(t *testing.T) {
	file := newEditableFile(t)
	degraded, maintenance := ImpactDegraded, ImpactMaintenance
	// Away from maintenance, the planned window goes.
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 2, Impact: &degraded}), opened.Add(time.Hour))
	if n := file.Notices[0]; n.Impact != ImpactDegraded || n.ScheduledStartAt != nil || n.ScheduledEndAt != nil {
		t.Fatalf("after leaving maintenance: %+v", n)
	}
	// A planned time with a non-maintenance impact is refused.
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 2, Impact: &degraded, SetScheduledStartAt: true, ScheduledStartAt: pointer(opened)}), "only for a maintenance notice")
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 2, SetScheduledEndAt: true, ScheduledEndAt: pointer(opened)}), "only for a maintenance notice")
	// To maintenance with a window in one edit.
	start, end := opened.Add(48*time.Hour), opened.Add(49*time.Hour)
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 2, Impact: &maintenance, SetScheduledStartAt: true, ScheduledStartAt: &start, SetScheduledEndAt: true, ScheduledEndAt: &end}), opened.Add(2*time.Hour))
	if n := file.Notices[0]; !n.ScheduledStartAt.Equal(start) || !n.ScheduledEndAt.Equal(end) {
		t.Fatalf("window %+v", n)
	}
	// The end must stay after the start; changing one time keeps the other.
	refusal(t, file, EditNotice(EditNoticeInput{NoticeID: 2, SetScheduledEndAt: true, ScheduledEndAt: pointer(start.Add(-time.Minute))}), "the planned end must be after the planned start")
	later := end.Add(time.Hour)
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 2, SetScheduledEndAt: true, ScheduledEndAt: &later}), opened.Add(3*time.Hour))
	if n := file.Notices[0]; !n.ScheduledStartAt.Equal(start) || !n.ScheduledEndAt.Equal(later) {
		t.Fatalf("window %+v", n)
	}
	// Setting one time to nil removes it; ClearSchedule removes both.
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 2, SetScheduledStartAt: true}), opened.Add(4*time.Hour))
	if n := file.Notices[0]; n.ScheduledStartAt != nil || n.ScheduledEndAt == nil {
		t.Fatalf("window %+v", n)
	}
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 2, ClearSchedule: true}), opened.Add(5*time.Hour))
	if n := file.Notices[0]; n.ScheduledStartAt != nil || n.ScheduledEndAt != nil || n.Impact != ImpactMaintenance {
		t.Fatalf("window %+v", n)
	}
}

func TestEditAndDeleteRefuseAStaleView(t *testing.T) {
	file := newEditableFile(t)
	stale := opened.Add(-time.Minute)
	current := file.Notices[1].UpdatedAt
	for name, c := range map[string]Change{
		"edit notice":   EditNotice(EditNoticeInput{NoticeID: 1, ExpectedUpdatedAt: &stale, Title: text("x")}),
		"edit update":   EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 1, ExpectedUpdatedAt: &stale, Message: text("x")}),
		"delete update": DeleteUpdate(DeleteUpdateInput{NoticeID: 1, UpdateID: 1, ExpectedUpdatedAt: &stale}),
		"delete notice": DeleteNotice(DeleteNoticeInput{NoticeID: 1, ExpectedUpdatedAt: &stale}),
	} {
		before := reflectCopy(file)
		if _, err := c(file, opened.Add(time.Hour)); !errors.Is(err, ErrConflict) || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "notice 1 changed while you were editing it; reload the page and try again") {
			t.Errorf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(before, file) {
			t.Errorf("%s changed the file", name)
		}
	}
	// A matching time, even with another zone or sub-second part, is accepted.
	same := current.In(time.FixedZone("x", 3600)).Add(400 * time.Millisecond)
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 1, ExpectedUpdatedAt: &same, Title: text("ok")}), opened.Add(time.Hour))
	// The edit moved updated_at, so the old token is stale now.
	if _, err := EditNotice(EditNoticeInput{NoticeID: 1, ExpectedUpdatedAt: &current, Title: text("again")})(file, opened.Add(2*time.Hour)); !errors.Is(err, ErrConflict) {
		t.Errorf("a reused token: %v", err)
	}
}

func reflectCopy(file *File) *File {
	copied := *file
	copied.Notices = make([]Notice, len(file.Notices))
	for i, n := range file.Notices {
		n.Updates = append([]Update(nil), n.Updates...)
		copied.Notices[i] = n
	}
	return &copied
}

func TestEditUpdateMarksItEditedAndChangesTheStage(t *testing.T) {
	file := newEditableFile(t)
	change(t, file, PostUpdate(PostUpdateInput{NoticeID: 1, Stage: StageIdentified, Message: "Cause found."}), opened.Add(time.Minute))
	later := opened.Add(time.Hour)
	change(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, Message: text(" Cause found: a disk. ")}), later)
	notice := file.Notices[1]
	if u := notice.Updates[1]; u.Message != "Cause found: a disk." || u.Stage != StageIdentified || u.EditedAt == nil || !u.EditedAt.Equal(later) || !notice.UpdatedAt.Equal(later) || notice.Updates[0].EditedAt != nil {
		t.Fatalf("after a message edit: %+v", notice.Updates)
	}
	// SetStage with an empty stage clears it; without SetStage, Stage is ignored.
	change(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, Stage: StageMonitoring}), later)
	if got := file.Notices[1].Updates[1].Stage; got != StageIdentified {
		t.Fatalf("stage without SetStage: %q", got)
	}
	change(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, SetStage: true}), later)
	if got := file.Notices[1].Updates[1].Stage; got != "" {
		t.Fatalf("cleared stage: %q", got)
	}
	change(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, SetStage: true, Stage: StageMonitoring}), later)
	if got := file.Notices[1].Updates[1].Stage; got != StageMonitoring {
		t.Fatalf("stage: %q", got)
	}
	refusal(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, SetStage: true, Stage: StageScheduled}), `the stage "scheduled" is not a stage of degraded notices`)
	refusal(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, Message: text("  ")}), "the message must have 1 to 5000 characters")
	refusal(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 7, Message: text("x")}), "notice 1 has no update 7")
}

func TestEditUpdateWithNothingNewIsNotMarkedEdited(t *testing.T) {
	file := newEditableFile(t)
	before := reflectCopy(file)
	change(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 1, Message: text(" First. "), SetStage: true}), opened.Add(time.Hour))
	if !reflect.DeepEqual(before, file) {
		t.Fatalf("an unchanged save changed the notice:\n%+v\n%+v", before.Notices[1], file.Notices[1])
	}
}

func TestEditUpdateKeepsAStageThatNoLongerFitsTheImpact(t *testing.T) {
	file := newEditableFile(t)
	change(t, file, PostUpdate(PostUpdateInput{NoticeID: 1, Stage: StageIdentified, Message: "Cause found."}), opened.Add(time.Minute))
	impact := ImpactMaintenance
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 1, Impact: &impact}), opened.Add(2*time.Minute))
	// Fixing the message with the stage kept works; changing to another stage
	// that does not fit is still refused.
	change(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, Message: text("Cause found: a disk."), SetStage: true, Stage: StageIdentified}), opened.Add(time.Hour))
	if u := file.Notices[1].Updates[1]; u.Message != "Cause found: a disk." || u.Stage != StageIdentified || u.EditedAt == nil {
		t.Fatalf("after the edit: %+v", u)
	}
	refusal(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 2, SetStage: true, Stage: StageMonitoring}), `the stage "monitoring" is not a stage of maintenance notices`)
}

func TestAHandEditedSubSecondUpdatedAtDoesNotBlockAnEdit(t *testing.T) {
	file := newEditableFile(t)
	file.Notices[1].UpdatedAt = opened.Add(500 * time.Millisecond)
	expected := opened
	change(t, file, EditNotice(EditNoticeInput{NoticeID: 1, ExpectedUpdatedAt: &expected, Title: text("Slow sign-in")}), opened.Add(time.Hour))
	if file.Notices[1].Title != "Slow sign-in" {
		t.Fatalf("notice: %+v", file.Notices[1])
	}
}

func TestEditAndDeleteUpdateRefuseADuplicateUpdateID(t *testing.T) {
	file := &File{Notices: []Notice{{NoticeID: 1, Impact: ImpactOutage, Updates: []Update{{UpdateID: 1, Message: "a"}, {UpdateID: 1, Message: "b"}, {UpdateID: 2, Message: "c"}}}}}
	refusal(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 1, Message: text("x")}), "more than one update 1")
	refusal(t, file, DeleteUpdate(DeleteUpdateInput{NoticeID: 1, UpdateID: 1}), "more than one update 1")
	file.Notices = append(file.Notices, file.Notices[0])
	refusal(t, file, DeleteNotice(DeleteNoticeInput{NoticeID: 1}), "more than one notice 1")
}

func TestDeleteUpdateKeepsAtLeastOneAndNeverReusesAnID(t *testing.T) {
	file := newEditableFile(t)
	refusal(t, file, DeleteUpdate(DeleteUpdateInput{NoticeID: 1, UpdateID: 1}), "a notice must keep at least one update; delete the notice instead")
	change(t, file, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Second."}), opened.Add(time.Minute))
	later := opened.Add(time.Hour)
	change(t, file, DeleteUpdate(DeleteUpdateInput{NoticeID: 1, UpdateID: 2}), later)
	notice := file.Notices[1]
	if len(notice.Updates) != 1 || notice.Updates[0].UpdateID != 1 || notice.NextUpdateID != 3 || !notice.UpdatedAt.Equal(later) {
		t.Fatalf("after deleting the newest update: %+v", notice)
	}
	change(t, file, PostUpdate(PostUpdateInput{NoticeID: 1, Message: "Third."}), later.Add(time.Minute))
	if got := file.Notices[1].Updates[1].UpdateID; got != 3 {
		t.Fatalf("the next update got ID %d, a deleted ID is reused", got)
	}
	// An older update can go too; the first one stays the notice's start.
	change(t, file, DeleteUpdate(DeleteUpdateInput{NoticeID: 1, UpdateID: 1}), later.Add(time.Hour))
	if n := file.Notices[1]; len(n.Updates) != 1 || n.Updates[0].UpdateID != 3 || n.NextUpdateID != 4 {
		t.Fatalf("after deleting the first update: %+v", n)
	}
}

func TestDeleteNoticeNeverReusesAnID(t *testing.T) {
	file := newEditableFile(t)
	refusal(t, file, DeleteNotice(DeleteNoticeInput{NoticeID: 9}), "no notice 9")
	change(t, file, DeleteNotice(DeleteNoticeInput{NoticeID: 2}), opened.Add(time.Hour))
	if len(file.Notices) != 1 || file.Notices[0].NoticeID != 1 || file.NextNoticeID != 3 {
		t.Fatalf("after deleting the newest notice: next %d, %+v", file.NextNoticeID, file.Notices)
	}
	if id := change(t, file, PostNotice(incident("again")), opened.Add(2*time.Hour)); id != 3 {
		t.Fatalf("the next notice got ID %d, a deleted ID is reused", id)
	}
	// A file without next_notice_id still never reuses the deleted ID.
	bare := &File{Notices: []Notice{{NoticeID: 4}, {NoticeID: 2}}}
	change(t, bare, DeleteNotice(DeleteNoticeInput{NoticeID: 4}), opened)
	if id := change(t, bare, PostNotice(incident("x")), opened); id != 5 {
		t.Fatalf("got ID %d", id)
	}
}

func TestThePageMarksAnEditedUpdateAndKeepsItsMessage(t *testing.T) {
	file := newEditableFile(t)
	change(t, file, EditUpdate(EditUpdateInput{NoticeID: 1, UpdateID: 1, Message: text("Line one.\n  Line  two.")}), opened.Add(time.Hour))
	page, err := RenderPage(*file)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(page), "(edited)"); got != 1 {
		t.Fatalf("the page has %d (edited) notes, want 1 (only the edited update)", got)
	}
	if !strings.Contains(string(page), "<span class=\"edited\">(edited)</span> Line one.\n  Line  two.</div>") {
		t.Fatalf("the edited update's message does not keep its text:\n%s", page)
	}
}
