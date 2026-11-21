package app

import (
	"context"
	"database/sql"
	"io"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmhotline/internal/statuspage"
)

var statusPageVersionPattern = regexp.MustCompile(`name="expected_updated_at" value="([^"]+)"`)

func TestStatusPageWebEditsKeepRetiredComponentsAndRefuseStaleForms(t *testing.T) {
	t.Parallel()
	a, fake := statusPageApp(t)
	clock := atomic.Int64{}
	clock.Store(time.Now().Unix())
	a.Now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	advance := func() { clock.Add(10) }
	ctx := context.Background()
	member := memberFixture(t, a, "responder@example.com")
	page := setUpStatusPageWithComponents(t, a, fake)
	if _, _, err := a.PostToStatusPage(ctx, member.Principal, statuspage.PostNoticeInput{Title: "Slow sign-in", Impact: statuspage.ImpactDegraded, AffectedComponentIDs: []string{"website", "api"}, Stage: statuspage.StageInvestigating, Message: "First."}); err != nil {
		t.Fatal(err)
	}
	// The settings file stops listing the API: notice 1 keeps naming it.
	page.Components = page.Components[:1]
	if reason := applyStatusPage(t, a, page); reason != "" {
		t.Fatal(reason)
	}
	advance()
	if _, _, err := a.PostStatusPageUpdate(ctx, member.Principal, statuspage.PostUpdateInput{NoticeID: 1, Stage: statuspage.StageIdentified, Message: "Second."}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)

	statusPageBody := func() string { t.Helper(); return getPage(t, client, server.URL+"/status-page") }
	version := func(body string) string {
		t.Helper()
		found := statusPageVersionPattern.FindStringSubmatch(body)
		if found == nil {
			t.Fatalf("the page has no expected_updated_at:\n%s", body)
		}
		return found[1]
	}
	token := pageToken(t, client, server.URL, "/status-page")
	send := func(path string, form url.Values, want int) string {
		t.Helper()
		form.Set("csrf", token)
		response := postForm(t, client, server.URL, path, form, want)
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return string(data)
	}
	body := statusPageBody()
	// The edit form offers the listed component and the retired one, checked
	// and marked removed, so saving a new title does not drop it.
	for _, want := range []string{`name="affected_component_ids" value="website" checked>`, `name="affected_component_ids" value="api" checked> api (removed)`, "Edit notice details", "Edit or delete updates", "Delete this notice"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	oldVersion := version(body)

	// An edit that keeps the checked boxes keeps the retired component.
	advance()
	send("/status-page/notices/1/edit", url.Values{"expected_updated_at": {oldVersion}, "title": {"Slow sign-in page"}, "impact": {"degraded"}, "affected_component_ids": {"website", "api"}}, 303)
	notice := publishedStatusFile(t, fake).Notices[0]
	if notice.Title != "Slow sign-in page" || len(notice.AffectedComponentIDs) != 2 || notice.AffectedComponentIDs[1] != "api" || !notice.UpdatedAt.Equal(a.Now()) || len(notice.Updates) != 2 || notice.Updates[0].EditedAt != nil {
		t.Fatalf("after editing the title: %+v", notice)
	}

	// The form of the page before that edit is stale: refused with 409, nothing
	// changes, and the page shows what was typed with the fold open.
	body = send("/status-page/notices/1/edit", url.Values{"expected_updated_at": {oldVersion}, "title": {"Typed again"}, "impact": {"degraded"}}, 409)
	if !strings.Contains(body, "notice 1 changed while you were editing it; reload the page and try again") || !strings.Contains(body, `value="Typed again"`) || !strings.Contains(body, `<details class="fold" open><summary>Edit notice details`) {
		t.Fatalf("the stale form:\n%s", body)
	}
	if got := publishedStatusFile(t, fake).Notices[0].Title; got != "Slow sign-in page" {
		t.Fatalf("a stale form changed the title to %q", got)
	}
	// A form without its version is refused, so no edit skips the check.
	send("/status-page/notices/1/edit", url.Values{"title": {"No version"}, "impact": {"degraded"}}, 400)
	// A form without the CSRF token, or from a browser nobody signed in, is refused.
	if response, err := client.PostForm(server.URL+"/status-page/notices/1/delete", url.Values{"expected_updated_at": {oldVersion}, "confirm_delete": {"1"}}); err != nil || response.StatusCode != 403 {
		t.Fatalf("a delete without the CSRF token: %v %v", response, err)
	}
	if _, _, err := a.DeleteStatusPageNotice(ctx, Principal{SignInEmailAddress: "stranger@example.com"}, statuspage.DeleteNoticeInput{NoticeID: 1}); err == nil {
		t.Fatal("a stranger deleted a notice")
	}

	// Edit update 2: message and stage change, and the page marks it edited.
	advance()
	current := version(statusPageBody())
	send("/status-page/notices/1/updates/2/edit", url.Values{"expected_updated_at": {current}, "stage": {"monitoring"}, "message": {"Second, corrected."}}, 303)
	update := publishedStatusFile(t, fake).Notices[0].Updates[1]
	if update.Message != "Second, corrected." || update.Stage != statuspage.StageMonitoring || update.EditedAt == nil || !update.EditedAt.Equal(a.Now()) {
		t.Fatalf("after editing the update: %+v", update)
	}
	if body := statusPageBody(); !strings.Contains(body, "(edited ") {
		t.Fatalf("the page does not mark the edited update:\n%s", body)
	}
	// A refused edit keeps the typed message, with the updates fold open.
	current = version(statusPageBody())
	body = send("/status-page/notices/1/updates/2/edit", url.Values{"expected_updated_at": {current}, "stage": {"scheduled"}, "message": {"Typed\nin a hurry."}}, 400)
	if !strings.Contains(body, "is not a stage of degraded notices") || !strings.Contains(body, "Typed\nin a hurry.</textarea>") || !strings.Contains(body, `<details class="fold" open><summary>Edit or delete updates`) {
		t.Fatalf("the refused update edit:\n%s", body)
	}
	// An update that does not exist is refused.
	send("/status-page/notices/1/updates/9/edit", url.Values{"expected_updated_at": {current}, "message": {"x"}}, 400)
	send("/status-page/notices/1/updates/0/edit", url.Values{"expected_updated_at": {current}, "message": {"x"}}, 404)

	// Delete update 1 from a stale form, then from a current one.
	send("/status-page/notices/1/updates/1/delete", url.Values{"expected_updated_at": {oldVersion}}, 409)
	advance()
	current = version(statusPageBody())
	send("/status-page/notices/1/updates/1/delete", url.Values{"expected_updated_at": {current}}, 303)
	notice = publishedStatusFile(t, fake).Notices[0]
	if len(notice.Updates) != 1 || notice.Updates[0].UpdateID != 2 || notice.NextUpdateID != 3 {
		t.Fatalf("after deleting update 1: %+v", notice)
	}
	// The last update cannot be deleted, and the page no longer offers it.
	current = version(statusPageBody())
	if body := send("/status-page/notices/1/updates/2/delete", url.Values{"expected_updated_at": {current}}, 400); !strings.Contains(body, "a notice must keep at least one update; delete the notice instead") {
		t.Fatalf("deleting the last update:\n%s", body)
	}
	if body := statusPageBody(); strings.Contains(body, "Delete update") {
		t.Fatalf("the page offers to delete the only update:\n%s", body)
	}

	// A resolved notice can be fixed and deleted too. The delete needs its box
	// ticked, and a refusal reopens the Recently resolved fold.
	advance()
	if _, _, err := a.ResolveStatusPageNotice(ctx, member.Principal, statuspage.ResolveInput{NoticeID: 1}); err != nil {
		t.Fatal(err)
	}
	current = version(statusPageBody())
	advance()
	send("/status-page/notices/1/edit", url.Values{"expected_updated_at": {current}, "title": {"Slow sign-in, fixed"}, "impact": {"degraded"}, "affected_component_ids": {"api"}}, 303)
	if file := publishedStatusFile(t, fake); file.Notices[0].Title != "Slow sign-in, fixed" || file.Notices[0].ResolvedAt == nil || len(file.Notices[0].AffectedComponentIDs) != 1 {
		t.Fatalf("after editing a resolved notice: %+v", file.Notices[0])
	}
	current = version(statusPageBody())
	body = send("/status-page/notices/1/delete", url.Values{"expected_updated_at": {current}}, 400)
	if !strings.Contains(body, "tick the box to confirm that notice 1 is deleted") || !strings.Contains(body, `<details class="more" open>`) {
		t.Fatalf("an unconfirmed delete:\n%s", body)
	}
	if len(publishedStatusFile(t, fake).Notices) != 1 {
		t.Fatal("an unconfirmed delete removed the notice")
	}
	send("/status-page/notices/1/delete", url.Values{"expected_updated_at": {current}, "confirm_delete": {"1"}}, 303)
	file := publishedStatusFile(t, fake)
	if len(file.Notices) != 0 || file.NextNoticeID != 2 {
		t.Fatalf("after deleting the notice: next %d, %+v", file.NextNoticeID, file.Notices)
	}
	if index, _ := fake.Object(statusPageBucket, "index.html"); strings.Contains(string(index.Content), "Slow sign-in") {
		t.Fatalf("index.html still shows the deleted notice: %s", index.Content)
	}
}

// statusPageIncidentLink reads the resolved flag of an incident's link to its
// notice, and whether the link exists.
func statusPageIncidentLink(t *testing.T, a *App, incidentID string) (noticeID int64, resolved, found bool) {
	t.Helper()
	err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT notice_id,resolved FROM status_page_incident_notices WHERE incident_id=?`, incidentID).Scan(&noticeID, &resolved)
		found = err == nil
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return noticeID, resolved, found
}

// A read of status.json that began before another responder's post saved
// notice N does not know N yet; it must not free the incident's button.
func TestStatusPageAStaleReadNeverMarksANewerIncidentNoticeResolved(t *testing.T) {
	t.Parallel()
	a, _ := statusPageApp(t)
	ctx := context.Background()
	link := func(incidentID string, noticeID int64) {
		t.Helper()
		if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO status_page_incident_notices(incident_id,notice_id,resolved) VALUES(?,?,0)`, incidentID, noticeID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	link("being-posted", 0)
	link("posted-after-the-read", 2)
	link("deleted-on-a-laptop", 1)
	link("still-open", 3)
	link("resolved-on-a-laptop", 4)
	now := time.Now().UTC()
	resolvedAt := now
	// A read that began before notice 2 was saved: next_notice_id is 2, and
	// the file lists no notice. Notice 1 is gone, so its link is resolved;
	// notices 2, 3, and 4 are newer than the read, so their links stay.
	staleRead := statuspage.File{NextNoticeID: 2, Notices: []statuspage.Notice{}}
	if err := a.markResolvedIncidentNotices(ctx, staleRead); err != nil {
		t.Fatal(err)
	}
	for incidentID, want := range map[string]bool{"being-posted": false, "deleted-on-a-laptop": true, "posted-after-the-read": false, "still-open": false, "resolved-on-a-laptop": false} {
		if _, resolved, found := statusPageIncidentLink(t, a, incidentID); !found || resolved != want {
			t.Errorf("stale read, %s: resolved %v, want %v", incidentID, resolved, want)
		}
	}
	// A current read: notice 3 is open and notice 4 resolved; notice 2 is gone.
	current := statuspage.File{NextNoticeID: 5, Notices: []statuspage.Notice{{NoticeID: 3, OpenedAt: now}, {NoticeID: 4, OpenedAt: now, ResolvedAt: &resolvedAt}}}
	if err := a.markResolvedIncidentNotices(ctx, current); err != nil {
		t.Fatal(err)
	}
	for incidentID, want := range map[string]bool{"being-posted": false, "deleted-on-a-laptop": true, "posted-after-the-read": true, "still-open": false, "resolved-on-a-laptop": true} {
		if _, resolved, found := statusPageIncidentLink(t, a, incidentID); !found || resolved != want {
			t.Errorf("current read, %s: resolved %v, want %v", incidentID, resolved, want)
		}
	}
	if noticeID, resolved, _ := statusPageIncidentLink(t, a, "being-posted"); noticeID != 0 || resolved {
		t.Errorf("the reservation changed: notice %d, resolved %v", noticeID, resolved)
	}
}

// A notice deleted straight in the bucket, from a laptop, frees its
// incident's button the next time MMHotline reads the page.
func TestStatusPageADirectLaptopDeletionFreesTheIncidentButton(t *testing.T) {
	t.Parallel()
	a, fake := statusPageApp(t)
	ctx := context.Background()
	member := memberFixture(t, a, "responder@example.com")
	incident, err := a.ManualSOS(ctx, member.Principal, "Sign-in fails", "")
	if err != nil {
		t.Fatal(err)
	}
	setUpStatusPageWithKey(t, a, fake)
	if _, noticeID, err := a.PostIncidentToStatusPage(ctx, member.Principal, incident.IncidentID); err != nil || noticeID != 1 {
		t.Fatalf("the incident's notice %d: %v", noticeID, err)
	}
	if _, _, err := a.PostIncidentToStatusPage(ctx, member.Principal, incident.IncidentID); err == nil {
		t.Fatal("a second press posted again while the notice is open")
	}
	var setup statusPageSetup
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		setup, _, err = readStatusPageSetup(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := statuspage.Publish(ctx, a.statusPageBucket(setup), statuspage.DeleteNotice(statuspage.DeleteNoticeInput{NoticeID: 1}), "", a.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StatusPage(ctx, member.Principal); err != nil {
		t.Fatal(err)
	}
	if _, resolved, found := statusPageIncidentLink(t, a, incident.IncidentID); !found || !resolved {
		t.Fatalf("the link after the laptop deleted its notice: found %v, resolved %v", found, resolved)
	}
	// The button posts a new notice, which never reuses the deleted ID.
	if _, noticeID, err := a.PostIncidentToStatusPage(ctx, member.Principal, incident.IncidentID); err != nil || noticeID != 2 {
		t.Fatalf("the new notice %d: %v", noticeID, err)
	}
}

func TestStatusPageEditAndDeleteRequestsAreCheckedWhereverTheyRun(t *testing.T) {
	t.Parallel()
	text, number := pointerTo[string], pointerTo[int64]
	for name, test := range map[string]struct {
		request StatusPageCommandRequest
		action  string
		want    string
	}{
		"edit without a notice":                   {StatusPageCommandRequest{Title: text("T")}, "edit", "--notice-id is required"},
		"edit notice 0":                           {StatusPageCommandRequest{NoticeID: number(0), Title: text("T")}, "edit", "--notice-id must be 1 or more"},
		"edit update 0":                           {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(0), Message: text("m")}, "edit", "--update-id must be 1 or more"},
		"edit update -1":                          {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(-1), Message: text("m")}, "edit", "--update-id must be 1 or more"},
		"delete update 0":                         {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(0)}, "delete", "--update-id must be 1 or more"},
		"delete notice 0":                         {StatusPageCommandRequest{NoticeID: number(0)}, "delete", "--notice-id must be 1 or more"},
		"delete without a notice":                 {StatusPageCommandRequest{}, "delete", "--notice-id is required"},
		"edit a notice with nothing to change":    {StatusPageCommandRequest{NoticeID: number(1)}, "edit", "edit needs at least one of --title"},
		"edit an update with nothing to change":   {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(1)}, "edit", "edit with --update-id needs at least one of --message"},
		"a message without an update":             {StatusPageCommandRequest{NoticeID: number(1), Message: text("m")}, "edit", "add --update-id to edit one"},
		"a stage without an update":               {StatusPageCommandRequest{NoticeID: number(1), Title: text("T"), Stage: text("identified")}, "edit", "add --update-id to edit one"},
		"clear the stage without an update":       {StatusPageCommandRequest{NoticeID: number(1), ClearStage: true}, "edit", "add --update-id to edit one"},
		"a title on an update":                    {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(1), Title: text("T"), Message: text("m")}, "edit", "leave out --update-id to edit the notice"},
		"clear the schedule on an update":         {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(1), ClearSchedule: true, Message: text("m")}, "edit", "leave out --update-id to edit the notice"},
		"components and clear components":         {StatusPageCommandRequest{NoticeID: number(1), AffectedComponentIDs: &[]string{"api"}, ClearComponents: true}, "edit", "--component and --clear-components cannot be given together"},
		"stage and clear stage":                   {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(1), Stage: text("identified"), ClearStage: true}, "edit", "--stage and --clear-stage cannot be given together"},
		"scheduled start and clear schedule":      {StatusPageCommandRequest{NoticeID: number(1), ScheduledStartAt: text("2026-11-22T02:00:00Z"), ClearSchedule: true}, "edit", "--scheduled-start and --clear-schedule cannot be given together"},
		"scheduled end and clear schedule":        {StatusPageCommandRequest{NoticeID: number(1), ScheduledEndAt: text("2026-11-22T02:00:00Z"), ClearSchedule: true}, "edit", "--scheduled-end and --clear-schedule cannot be given together"},
		"an empty stage":                          {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(1), Stage: text("")}, "edit", "--stage must not be empty"},
		"a scheduled time that is not a time":     {StatusPageCommandRequest{NoticeID: number(1), ScheduledEndAt: text("soon")}, "edit", "--scheduled-end must be a time such as"},
		"delete conflicts do not apply to delete": {StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(0), ClearStage: true}, "delete", "--update-id must be 1 or more"},
	} {
		var err error
		if test.action == "edit" {
			_, err = test.request.EditChange()
		} else {
			_, err = test.request.DeleteChange()
		}
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v, want %q", name, err, test.want)
		}
	}
	// What is allowed.
	for name, request := range map[string]StatusPageCommandRequest{
		"a title":                 {NoticeID: number(1), Title: text("T")},
		"clear components":        {NoticeID: number(1), ClearComponents: true},
		"clear the schedule":      {NoticeID: number(1), ClearSchedule: true},
		"clear an update's stage": {NoticeID: number(1), UpdateID: number(2), ClearStage: true},
		"an update's message":     {NoticeID: number(1), UpdateID: number(2), Message: text("m")},
		"an update's stage":       {NoticeID: number(1), UpdateID: number(2), Stage: text("monitoring")},
		"components and a title":  {NoticeID: number(1), Title: text("T"), AffectedComponentIDs: &[]string{"api"}},
		"schedule start and end":  {NoticeID: number(1), ScheduledStartAt: text("2026-11-22T02:00:00Z"), ScheduledEndAt: text("2026-11-22T03:00:00Z")},
	} {
		if _, err := request.EditChange(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStatusPageCommandEditAndDelete(t *testing.T) {
	t.Parallel()
	a, fake := statusPageApp(t)
	clock := atomic.Int64{}
	clock.Store(time.Now().Unix())
	a.Now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	setUpStatusPageWithComponents(t, a, fake)
	run := func(action string, request StatusPageCommandRequest, want int) (CommandResult, string) {
		t.Helper()
		clock.Add(10)
		return commandRequest(t, a, CommandRequest{Group: "status-page", Action: action, StatusPage: request}, want)
	}
	text, number := pointerTo[string], pointerTo[int64]
	run("post", StatusPageCommandRequest{Title: text("Slow API"), Impact: text("degraded"), AffectedComponentIDs: &[]string{"api"}, Stage: text("investigating"), Message: text("We are looking.")}, 200)
	run("post", StatusPageCommandRequest{NoticeID: number(1), Stage: text("identified"), Message: text("Found it.")}, 200)
	before := publishedStatusFile(t, fake)

	// Notice mode changes only what is given.
	result, _ := run("edit", StatusPageCommandRequest{NoticeID: number(1), Title: text("Slow API requests")}, 200)
	if result.Message != "Edited notice 1." || result.StatusPageNoticeID != 1 || !strings.Contains(result.Text(), "Slow API requests") {
		t.Fatalf("edit notice: %q\n%s", result.Message, result.Text())
	}
	notice := publishedStatusFile(t, fake).Notices[0]
	if notice.Title != "Slow API requests" || notice.Impact != statuspage.ImpactDegraded || len(notice.AffectedComponentIDs) != 1 || len(notice.Updates) != 2 || notice.UpdatedAt.Equal(before.Notices[0].UpdatedAt) {
		t.Fatalf("after --title: %+v", notice)
	}
	run("edit", StatusPageCommandRequest{NoticeID: number(1), ClearComponents: true}, 200)
	if got := publishedStatusFile(t, fake).Notices[0].AffectedComponentIDs; len(got) != 0 {
		t.Fatalf("after --clear-components: %v", got)
	}
	if _, raw := run("edit", StatusPageCommandRequest{NoticeID: number(1), Impact: text("degraded"), ScheduledStartAt: text("2026-11-22T02:00:00Z")}, 400); !strings.Contains(raw, "only for a maintenance notice") {
		t.Fatalf("a window with a degraded impact: %s", raw)
	}

	// Update mode.
	result, _ = run("edit", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(2), Message: text("Found it: a full disk.")}, 200)
	if result.Message != "Edited update 2 of notice 1." {
		t.Fatalf("edit update: %q", result.Message)
	}
	update := publishedStatusFile(t, fake).Notices[0].Updates[1]
	if update.Message != "Found it: a full disk." || update.Stage != statuspage.StageIdentified || update.EditedAt == nil {
		t.Fatalf("after --message: %+v", update)
	}
	run("edit", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(2), ClearStage: true}, 200)
	if got := publishedStatusFile(t, fake).Notices[0].Updates[1]; got.Stage != "" || got.Message != "Found it: a full disk." {
		t.Fatalf("after --clear-stage: %+v", got)
	}

	// Refusals change nothing, whether the command or the page's rules refuse.
	snapshot := publishedStatusFile(t, fake)
	for name, test := range map[string]struct {
		action  string
		request StatusPageCommandRequest
		want    string
	}{
		"update ID 0":                {"edit", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(0), Message: text("x")}, "--update-id must be 1 or more"},
		"delete update ID 0":         {"delete", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(0)}, "--update-id must be 1 or more"},
		"an unknown update":          {"edit", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(7), Message: text("x")}, "notice 1 has no update 7"},
		"an unknown notice":          {"delete", StatusPageCommandRequest{NoticeID: number(7)}, "the status page has no notice 7"},
		"a stage of another kind":    {"edit", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(1), Stage: text("scheduled")}, "is not a stage of degraded notices"},
		"an unknown component":       {"edit", StatusPageCommandRequest{NoticeID: number(1), AffectedComponentIDs: &[]string{"cdn"}}, "its components are website, api"},
		"a window on a degraded one": {"edit", StatusPageCommandRequest{NoticeID: number(1), ScheduledEndAt: text("2026-11-22T03:00:00Z")}, "only for a maintenance notice"},
	} {
		if _, raw := run(test.action, test.request, 400); !strings.Contains(raw, test.want) {
			t.Errorf("%s: %s", name, raw)
		}
	}
	if after := publishedStatusFile(t, fake); len(after.Notices) != 1 || !after.Notices[0].UpdatedAt.Equal(snapshot.Notices[0].UpdatedAt) {
		t.Fatalf("a refused request changed the page: %+v", after.Notices)
	}

	// Delete: an update, never the last one; then the notice, never its ID.
	result, _ = run("delete", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(1)}, 200)
	if result.Message != "Deleted update 1 of notice 1." {
		t.Fatalf("delete update: %q", result.Message)
	}
	if _, raw := run("delete", StatusPageCommandRequest{NoticeID: number(1), UpdateID: number(2)}, 400); !strings.Contains(raw, "a notice must keep at least one update; delete the notice instead") {
		t.Fatalf("deleting the last update: %s", raw)
	}
	result, _ = run("delete", StatusPageCommandRequest{NoticeID: number(1)}, 200)
	if result.Message != "Deleted notice 1." {
		t.Fatalf("delete notice: %q", result.Message)
	}
	if file := publishedStatusFile(t, fake); len(file.Notices) != 0 || file.NextNoticeID != 2 {
		t.Fatalf("after deleting the notice: next %d, %+v", file.NextNoticeID, file.Notices)
	}
	if result, _ := run("post", StatusPageCommandRequest{Title: text("Again"), Message: text("m")}, 200); result.StatusPageNoticeID != 2 {
		t.Fatalf("the next notice got ID %d", result.StatusPageNoticeID)
	}
}

// A refused edit of an update keeps the text in that update's own box only: the
// "Add an update" box stays empty. An update to a notice that another
// responder resolved meanwhile has no form on the reloaded page, so the page
// shows the text beside the error instead of dropping it. A stage the notice's
// impact no longer has stays selected on an update's edit form.
func TestStatusPageDraftsStayInTheirOwnFormAndNothingTypedIsLost(t *testing.T) {
	t.Parallel()
	a, fake := statusPageApp(t)
	ctx := context.Background()
	member := memberFixture(t, a, "responder@example.com")
	setUpStatusPageWithKey(t, a, fake)
	if _, _, err := a.PostToStatusPage(ctx, member.Principal, statuspage.PostNoticeInput{Title: "Slow sign-in", Impact: statuspage.ImpactDegraded, Stage: statuspage.StageInvestigating, Message: "First."}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.PostStatusPageUpdate(ctx, member.Principal, statuspage.PostUpdateInput{NoticeID: 1, Stage: statuspage.StageIdentified, Message: "Second."}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, member.Principal.SignInEmailAddress)
	token := pageToken(t, client, server.URL, "/status-page")
	send := func(path string, form url.Values, want int) string {
		t.Helper()
		form.Set("csrf", token)
		response := postForm(t, client, server.URL, path, form, want)
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return string(data)
	}
	version := func() string {
		t.Helper()
		found := statusPageVersionPattern.FindStringSubmatch(getPage(t, client, server.URL+"/status-page"))
		if found == nil {
			t.Fatal("the page has no expected_updated_at")
		}
		return found[1]
	}

	// A refused edit of update 2 shows the text once, in update 2's box.
	body := send("/status-page/notices/1/updates/2/edit", url.Values{"expected_updated_at": {version()}, "stage": {"scheduled"}, "message": {"Typed in a hurry."}}, 400)
	if got := strings.Count(body, "Typed in a hurry."); got != 1 {
		t.Fatalf("the typed text appears %d times, want 1 (only in the edit form):\n%s", got, body)
	}
	if strings.Contains(body, "Your text was not saved") {
		t.Fatalf("the page has the form, but shows the text apart:\n%s", body)
	}

	// The notice changes to maintenance: update 2's stage identified no longer
	// fits, yet it stays selected, and a corrected message is saved with it.
	send("/status-page/notices/1/edit", url.Values{"expected_updated_at": {version()}, "title": {"Slow sign-in"}, "impact": {"maintenance"}}, 303)
	if body := getPage(t, client, server.URL+"/status-page"); !strings.Contains(body, `<option value="identified" selected>Identified</option>`) {
		t.Fatalf("the edit form of update 2 lost its stage:\n%s", body)
	}
	send("/status-page/notices/1/updates/2/edit", url.Values{"expected_updated_at": {version()}, "stage": {"identified"}, "message": {"Second, corrected."}}, 303)
	if update := publishedStatusFile(t, fake).Notices[0].Updates[1]; update.Message != "Second, corrected." || update.Stage != statuspage.StageIdentified {
		t.Fatalf("after the edit: %+v", update)
	}

	// Another responder resolves the notice while a person types an update.
	if _, _, err := a.ResolveStatusPageNotice(ctx, member.Principal, statuspage.ResolveInput{NoticeID: 1}); err != nil {
		t.Fatal(err)
	}
	body = send("/status-page/notices/1/updates", url.Values{"stage": {""}, "message": {"The update I was writing."}}, 400)
	if !strings.Contains(body, "is resolved; post a new notice instead") || !strings.Contains(body, `<textarea readonly rows="6">The update I was writing.</textarea>`) {
		t.Fatalf("the lost update text:\n%s", body)
	}
}

// A browser that disconnects after the incident's reservation is saved must
// not leave the reservation at notice_id 0, which would block the button for
// good: the post finishes and its notice is linked.
func TestPostIncidentToStatusPageFinishesWhenTheCallerGivesUp(t *testing.T) {
	t.Parallel()
	a, fake := statusPageApp(t)
	member := memberFixture(t, a, "responder@example.com")
	incident, err := a.ManualSOS(context.Background(), member.Principal, "Sign-in fails", "")
	if err != nil {
		t.Fatal(err)
	}
	setUpStatusPageWithKey(t, a, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The first request to R2 signs with the clock, after the reservation is
	// saved: the caller gives up there.
	var armed atomic.Bool
	now := a.Now
	a.Now = func() time.Time {
		if armed.Load() {
			cancel()
		}
		return now()
	}
	armed.Store(true)
	_, noticeID, err := a.PostIncidentToStatusPage(ctx, member.Principal, incident.IncidentID)
	armed.Store(false)
	if ctx.Err() == nil {
		t.Fatal("the test never cancelled the caller")
	}
	if err != nil || noticeID != 1 {
		t.Fatalf("notice %d: %v", noticeID, err)
	}
	if linked, _, found := statusPageIncidentLink(t, a, incident.IncidentID); !found || linked != noticeID {
		t.Fatalf("the incident's link: notice %d, found %v", linked, found)
	}
	if file := publishedStatusFile(t, fake); len(file.Notices) != 1 || file.Notices[0].NoticeID != 1 {
		t.Fatalf("status.json: %+v", file.Notices)
	}
}
