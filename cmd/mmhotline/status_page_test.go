package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/app"
	"mmhotline/internal/fakecloudflare"
	"mmhotline/internal/statuspage"
)

// TestStatusPagePostFromALaptop posts with only the bucket's R2 access key
// in env vars, as a responder does while the MMHotline server is down.
func TestStatusPagePostFromALaptop(t *testing.T) {
	fake := fakecloudflare.New(t)
	fake.AddBucket("mmhotline-status-status-example-com-18a8ea11")
	bucketURL := fake.Addresses().R2BucketURL(fake.AccountID, "mmhotline-status-status-example-com-18a8ea11")
	bucket := &statuspage.R2Bucket{BucketURL: bucketURL, AccessKeyID: fake.R2AccessKeyID, SecretAccessKey: "laptop-secret"}
	laptop := func(args ...string) (*os.ProcessState, string) {
		t.Helper()
		command := cliProcess(t, append([]string{"status-page"}, args...)...)
		command.Env = append(command.Env, "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL="+bucketURL, "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID="+fake.R2AccessKeyID, "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY=laptop-secret")
		output, _ := command.CombinedOutput()
		t.Logf("status-page %s:\n%s", strings.Join(args, " "), output)
		if strings.Contains(string(output), "laptop-secret") {
			t.Fatal("the secret access key was printed")
		}
		return command.ProcessState, string(output)
	}
	if state, _ := laptop("post", "--title", "Sign-in delays", "--message", "Hello"); state.ExitCode() != 4 {
		t.Fatalf("a post before set-up published a page: exit %d", state.ExitCode())
	}
	if _, _, err := statuspage.Publish(context.Background(), bucket, statuspage.ApplyPageConfig(statuspage.PageConfig{PageTitle: "Example Inc.", HostName: "status.example.com", Components: []statuspage.Component{{ComponentID: "website", Name: "Website"}, {ComponentID: "api", Name: "API"}}}), "Example Inc.", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Without --impact, a new notice is an outage.
	if state, _ := laptop("post", "--title", "Sign-in delays", "--message", "We are looking into a problem."); !state.Success() {
		t.Fatal("post failed")
	}
	if state, _ := laptop("post", "--notice-id", "1", "--stage", "identified", "--message", "The database is moving."); !state.Success() {
		t.Fatal("update failed")
	}
	if state, _ := laptop("post", "--title", "Network work", "--impact", "maintenance", "--stage", "scheduled", "--scheduled-start", "2026-11-22T02:00:00Z", "--scheduled-end", "2026-11-22T03:00:00Z", "--message", "A switch is replaced."); !state.Success() {
		t.Fatal("maintenance post failed")
	}
	if state, output := laptop("resolve", "--notice-id", "1", "--closing-message", "Fixed."); !state.Success() || !strings.Contains(output, "Resolved notice 1, outage") {
		t.Fatalf("resolve failed: %s", output)
	}
	file, err := statuspage.Read(context.Background(), bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Notices) != 2 || file.HostName != "status.example.com" || file.NextNoticeID != 3 {
		t.Fatalf("published %+v", file)
	}
	outage, maintenance := file.Notices[1], file.Notices[0]
	if outage.Title != "Sign-in delays" || outage.Impact != statuspage.ImpactOutage || outage.ResolvedAt == nil || len(outage.Updates) != 3 || outage.Updates[0].Stage != "" || outage.Updates[1].Stage != statuspage.StageIdentified || outage.Updates[2].Stage != statuspage.StageResolved {
		t.Fatalf("the outage %+v", outage)
	}
	if maintenance.Impact != statuspage.ImpactMaintenance || maintenance.ScheduledStartAt == nil || maintenance.ScheduledEndAt == nil || maintenance.ResolvedAt != nil || maintenance.Updates[0].Stage != statuspage.StageScheduled {
		t.Fatalf("the maintenance notice %+v", maintenance)
	}
	if page, _ := fake.Object("mmhotline-status-status-example-com-18a8ea11", "index.html"); !strings.Contains(string(page.Content), "The database is moving.") || !strings.Contains(string(page.Content), "Scheduled maintenance") {
		t.Fatalf("index.html: %s", page.Content)
	}
	// The laptop refuses what the server refuses, with the same words, and
	// changes nothing.
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"post", "--title", "T", "--impact", "degraded", "--stage", "scheduled", "--message", "m"}, "is not a stage of degraded notices"},
		{[]string{"post", "--title", "T", "--scheduled-start", "2026-11-22T02:00:00Z", "--message", "m"}, "only for a maintenance notice"},
		{[]string{"post", "--notice-id", "2", "--stage", "monitoring", "--message", "m"}, "is not a stage of maintenance notices"},
		{[]string{"post", "--notice-id", "1", "--message", "Late."}, "notice 1 is resolved; post a new notice instead"},
		{[]string{"post", "--notice-id", "9", "--message", "m"}, "the status page has no notice 9"},
		{[]string{"resolve", "--notice-id", "2", "--stage", "completed"}, "a stage needs a closing message"},
	} {
		if state, output := laptop(test.args...); state.ExitCode() != 4 || !strings.Contains(output, test.want) {
			t.Errorf("%v: exit %d: %s", test.args, state.ExitCode(), output)
		}
	}
	if again, _ := statuspage.Read(context.Background(), bucket); len(again.Notices) != 2 || len(again.Notices[0].Updates) != 1 || len(again.Notices[1].Updates) != 3 {
		t.Fatalf("a refused post changed the page: %+v", again.Notices)
	}
	command := cliProcess(t, "status-page", "show", "--json")
	command.Env = append(command.Env, "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL="+bucketURL, "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID="+fake.R2AccessKeyID)
	if output, _ := command.CombinedOutput(); command.ProcessState.ExitCode() != 4 || !strings.Contains(string(output), "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY is required") {
		t.Fatalf("show without the secret: %s", output)
	}
}

// --component, given once for each component, tags a new notice, directly in
// the bucket and through the service; the page's components decide which
// IDs are allowed.
func TestStatusPagePostWithComponents(t *testing.T) {
	fake := fakecloudflare.New(t)
	fake.AddBucket("mmhotline-status-status-example-com-18a8ea11")
	bucketURL := fake.Addresses().R2BucketURL(fake.AccountID, "mmhotline-status-status-example-com-18a8ea11")
	bucket := &statuspage.R2Bucket{BucketURL: bucketURL, AccessKeyID: fake.R2AccessKeyID, SecretAccessKey: "laptop-secret"}
	if _, _, err := statuspage.Publish(context.Background(), bucket, statuspage.ApplyPageConfig(statuspage.PageConfig{PageTitle: "Example Inc.", HostName: "status.example.com", Components: []statuspage.Component{{ComponentID: "website", Name: "Website"}, {ComponentID: "api", Name: "API"}, {ComponentID: "mail", Name: "Mail"}}}), "Example Inc.", time.Now()); err != nil {
		t.Fatal(err)
	}
	laptop := func(args ...string) (*os.ProcessState, string) {
		t.Helper()
		command := cliProcess(t, append([]string{"status-page"}, args...)...)
		command.Env = append(command.Env, "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL="+bucketURL, "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID="+fake.R2AccessKeyID, "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY=laptop-secret")
		output, _ := command.CombinedOutput()
		t.Logf("status-page %s:\n%s", strings.Join(args, " "), output)
		return command.ProcessState, string(output)
	}
	state, output := laptop("post", "--title", "Slow API and mail", "--impact", "degraded", "--component", "mail", "--component", "api", "--component", "mail", "--message", "Both are slow.")
	if !state.Success() || !strings.Contains(output, "Open notice 1, degraded: Slow API and mail") || !strings.Contains(output, "Affects API, Mail.") || !strings.Contains(output, "Component API (api): degraded") || !strings.Contains(output, "Component Website (website): operational") {
		t.Fatalf("post with components: %s", output)
	}
	file, err := statuspage.Read(context.Background(), bucket)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Notices[0].AffectedComponentIDs; len(got) != 2 || got[0] != "api" || got[1] != "mail" {
		t.Fatalf("affected components %v, want the page's order, each once", got)
	}
	if page, _ := fake.Object("mmhotline-status-status-example-com-18a8ea11", "index.html"); !strings.Contains(string(page.Content), `<span class="chip">API</span><span class="chip">Mail</span>`) {
		t.Fatalf("index.html lacks the notice's chips: %s", page.Content)
	}
	if state, output := laptop("post", "--title", "Odd", "--component", "cdn", "--message", "m"); state.ExitCode() != 4 || !strings.Contains(output, `the status page has no component "cdn"; its components are website, api, mail`) {
		t.Fatalf("an unknown component: exit %d: %s", state.ExitCode(), output)
	}
	if again, _ := statuspage.Read(context.Background(), bucket); len(again.Notices) != 1 {
		t.Fatalf("a refused post changed the page: %+v", again.Notices)
	}

	// Through the service, the flags arrive as one request: the components in
	// the order given, and none at all when the flag is left out.
	socket := filepath.Join(shortSocketDirectory(t), "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan app.CommandRequest, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request app.CommandRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		json.NewEncoder(w).Encode(app.CommandResult{PeopleCommandResult: app.PeopleCommandResult{Message: "Posted to notice 1."}})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	for _, test := range []struct {
		args []string
		want *[]string
	}{
		{[]string{"post", "--title", "T", "--message", "m"}, nil},
		{[]string{"post", "--title", "T", "--message", "m", "--component", "website", "--component", "api"}, &[]string{"website", "api"}},
	} {
		if output, err := cliProcess(t, append([]string{"status-page"}, append(test.args, "--command-socket", socket)...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", test.args, output, err)
		}
		if request := <-requests; !reflect.DeepEqual(request.StatusPage.AffectedComponentIDs, test.want) {
			t.Errorf("%v sent components %v, want %v", test.args, request.StatusPage.AffectedComponentIDs, test.want)
		}
	}
}

// The post and resolve flags are checked before anything is sent, so a bad
// request fails the same way on the server and on a laptop. A flag that was
// given is never taken for one that was left out: --notice-id 0 and an empty
// --message are refused, not read as a new notice or as no message.
func TestStatusPageFlagsAreCheckedBeforeAnythingIsSent(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"post", "--message", "m"}, "--title is required to post a new notice"},
		{[]string{"post", "--title", "", "--message", "m"}, "--title is required to post a new notice"},
		{[]string{"post", "--title", "T"}, "--message is required"},
		{[]string{"post", "--title", "T", "--message", ""}, "--message is required"},
		{[]string{"post", "--notice-id", "0", "--message", "m"}, "--notice-id must be 1 or more"},
		{[]string{"post", "--notice-id", "-2", "--message", "m"}, "--notice-id must be 1 or more"},
		{[]string{"post", "--notice-id", "1", "--title", "T", "--message", "m"}, "describe a new notice"},
		{[]string{"post", "--notice-id", "1", "--impact", "degraded", "--message", "m"}, "describe a new notice"},
		{[]string{"post", "--notice-id", "1", "--scheduled-start", "2026-11-22T02:00:00Z", "--message", "m"}, "describe a new notice"},
		{[]string{"post", "--notice-id", "1", "--scheduled-end", "2026-11-22T02:00:00Z", "--message", "m"}, "describe a new notice"},
		{[]string{"post", "--notice-id", "1", "--component", "website", "--message", "m"}, "--component"},
		{[]string{"post", "--notice-id", "1", "--component", "", "--message", "m"}, "describe a new notice"},
		{[]string{"post", "--title", "T", "--impact", "maintenance", "--scheduled-start", "tomorrow", "--message", "m"}, "--scheduled-start must be a time such as 2026-11-21T02:00:00Z"},
		{[]string{"post", "--title", "T", "--impact", "maintenance", "--scheduled-end", "5pm", "--message", "m"}, "--scheduled-end must be a time such as 2026-11-21T02:00:00Z"},
		{[]string{"resolve"}, "--notice-id is required"},
		{[]string{"resolve", "--notice-id", "0"}, "--notice-id must be 1 or more"},
		{[]string{"resolve", "--notice-id", "-1", "--closing-message", "Fixed."}, "--notice-id must be 1 or more"},
		{[]string{"resolve", "--notice-id", "1", "--title", "T"}, "flag provided but not defined: -title"},
		{[]string{"post", "--notice-id", "1", "--title", "T", "--message", "m"}, "mmhotline status-page edit"},
		{[]string{"edit", "--title", "T"}, "--notice-id is required"},
		{[]string{"edit", "--notice-id", "0", "--title", "T"}, "--notice-id must be 1 or more"},
		{[]string{"edit", "--notice-id", "1", "--update-id", "0", "--message", "m"}, "--update-id must be 1 or more"},
		{[]string{"edit", "--notice-id", "1", "--update-id", "-3", "--message", "m"}, "--update-id must be 1 or more"},
		{[]string{"edit", "--notice-id", "1"}, "edit needs at least one of --title"},
		{[]string{"edit", "--notice-id", "1", "--update-id", "1"}, "edit with --update-id needs at least one of --message"},
		{[]string{"edit", "--notice-id", "1", "--message", "m"}, "add --update-id to edit one"},
		{[]string{"edit", "--notice-id", "1", "--update-id", "1", "--title", "T"}, "leave out --update-id to edit the notice"},
		{[]string{"edit", "--notice-id", "1", "--component", "api", "--clear-components"}, "--component and --clear-components cannot be given together"},
		{[]string{"edit", "--notice-id", "1", "--update-id", "1", "--stage", "identified", "--clear-stage"}, "--stage and --clear-stage cannot be given together"},
		{[]string{"edit", "--notice-id", "1", "--scheduled-start", "2026-11-22T02:00:00Z", "--clear-schedule"}, "--scheduled-start and --clear-schedule cannot be given together"},
		{[]string{"edit", "--notice-id", "1", "--scheduled-end", "2026-11-22T02:00:00Z", "--clear-schedule"}, "--scheduled-end and --clear-schedule cannot be given together"},
		{[]string{"edit", "--notice-id", "1", "--scheduled-end", "5pm"}, "--scheduled-end must be a time such as"},
		{[]string{"delete"}, "--notice-id is required"},
		{[]string{"delete", "--notice-id", "0"}, "--notice-id must be 1 or more"},
		{[]string{"delete", "--notice-id", "1", "--update-id", "0"}, "--update-id must be 1 or more"},
		{[]string{"delete", "--notice-id", "1", "--title", "T"}, "flag provided but not defined: -title"},
	} {
		if _, err := runStatusPageCapturingOutput(t, "", test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%v: %v, want %q", test.args, err, test.want)
		}
	}
}

// TestStatusPageEditAndDeleteFromALaptop edits and deletes notices and updates
// directly in the R2 bucket, with only the bucket's key in env vars. A flag
// that was given is never taken for one that was left out: --update-id 0 is
// refused and changes nothing.
func TestStatusPageEditAndDeleteFromALaptop(t *testing.T) {
	fake := fakecloudflare.New(t)
	fake.AddBucket("mmhotline-status-status-example-com-18a8ea11")
	bucketURL := fake.Addresses().R2BucketURL(fake.AccountID, "mmhotline-status-status-example-com-18a8ea11")
	bucket := &statuspage.R2Bucket{BucketURL: bucketURL, AccessKeyID: fake.R2AccessKeyID, SecretAccessKey: "laptop-secret"}
	if _, _, err := statuspage.Publish(context.Background(), bucket, statuspage.ApplyPageConfig(statuspage.PageConfig{PageTitle: "Example Inc.", HostName: "status.example.com", Components: []statuspage.Component{{ComponentID: "website", Name: "Website"}, {ComponentID: "api", Name: "API"}}}), "Example Inc.", time.Now()); err != nil {
		t.Fatal(err)
	}
	laptop := func(args ...string) (*os.ProcessState, string) {
		t.Helper()
		command := cliProcess(t, append([]string{"status-page"}, args...)...)
		command.Env = append(command.Env, "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL="+bucketURL, "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID="+fake.R2AccessKeyID, "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY=laptop-secret")
		output, _ := command.CombinedOutput()
		t.Logf("status-page %s:\n%s", strings.Join(args, " "), output)
		return command.ProcessState, string(output)
	}
	read := func() statuspage.File {
		t.Helper()
		file, err := statuspage.Read(context.Background(), bucket)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	for _, args := range [][]string{
		{"post", "--title", "Sign-in delays", "--impact", "degraded", "--component", "api", "--stage", "investigating", "--message", "Wee are looking."},
		{"post", "--notice-id", "1", "--stage", "identified", "--message", "Found it."},
		{"post", "--title", "Test notice", "--message", "Please ignore."},
	} {
		if state, output := laptop(args...); !state.Success() {
			t.Fatalf("%v: %s", args, output)
		}
	}
	// Notice mode: only the given fields change, and the page shows the change.
	if state, output := laptop("edit", "--notice-id", "1", "--title", "Sign-in is slow"); !state.Success() || !strings.Contains(output, "Edited notice 1, directly in the R2 bucket.") || !strings.Contains(output, "Open notice 1, degraded: Sign-in is slow") {
		t.Fatalf("edit the title: %s", output)
	}
	if n := read().Notices[1]; n.Title != "Sign-in is slow" || n.Impact != statuspage.ImpactDegraded || len(n.AffectedComponentIDs) != 1 || len(n.Updates) != 2 {
		t.Fatalf("after --title: %+v", n)
	}
	if state, _ := laptop("edit", "--notice-id", "1", "--component", "website", "--component", "api"); !state.Success() {
		t.Fatal("edit components failed")
	}
	if got := read().Notices[1].AffectedComponentIDs; len(got) != 2 || got[0] != "website" || got[1] != "api" {
		t.Fatalf("after --component: %v", got)
	}
	if state, _ := laptop("edit", "--notice-id", "1", "--clear-components"); !state.Success() {
		t.Fatal("clear components failed")
	}
	if got := read().Notices[1].AffectedComponentIDs; len(got) != 0 {
		t.Fatalf("after --clear-components: %v", got)
	}
	if state, _ := laptop("edit", "--notice-id", "2", "--impact", "maintenance", "--scheduled-start", "2026-11-22T02:00:00Z", "--scheduled-end", "2026-11-22T03:00:00Z"); !state.Success() {
		t.Fatal("edit to maintenance failed")
	}
	if n := read().Notices[0]; n.Impact != statuspage.ImpactMaintenance || n.ScheduledStartAt == nil || n.ScheduledEndAt == nil {
		t.Fatalf("after --impact maintenance: %+v", n)
	}
	if state, _ := laptop("edit", "--notice-id", "2", "--impact", "degraded"); !state.Success() {
		t.Fatal("edit away from maintenance failed")
	}
	if n := read().Notices[0]; n.Impact != statuspage.ImpactDegraded || n.ScheduledStartAt != nil || n.ScheduledEndAt != nil {
		t.Fatalf("a change away from maintenance kept the window: %+v", n)
	}
	// Update mode.
	if state, output := laptop("edit", "--notice-id", "1", "--update-id", "1", "--message", "We are looking."); !state.Success() || !strings.Contains(output, "Edited update 1 of notice 1, directly in the R2 bucket.") {
		t.Fatalf("edit the message: %s", output)
	}
	if u := read().Notices[1].Updates[0]; u.Message != "We are looking." || u.Stage != statuspage.StageInvestigating || u.EditedAt == nil {
		t.Fatalf("after --message: %+v", u)
	}
	if state, _ := laptop("edit", "--notice-id", "1", "--update-id", "2", "--clear-stage"); !state.Success() {
		t.Fatal("clear stage failed")
	}
	if u := read().Notices[1].Updates[1]; u.Stage != "" || u.Message != "Found it." {
		t.Fatalf("after --clear-stage: %+v", u)
	}
	if state, _ := laptop("edit", "--notice-id", "1", "--update-id", "2", "--stage", "monitoring"); !state.Success() || read().Notices[1].Updates[1].Stage != statuspage.StageMonitoring {
		t.Fatal("edit stage failed")
	}

	// Refusals change nothing: --update-id 0 is not "no update", and a
	// notice-level flag does not go with an update.
	snapshot, _ := json.Marshal(read())
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"edit", "--notice-id", "1", "--update-id", "0", "--message", "x"}, "--update-id must be 1 or more"},
		{[]string{"delete", "--notice-id", "1", "--update-id", "0"}, "--update-id must be 1 or more"},
		{[]string{"edit", "--notice-id", "1", "--component", "api", "--clear-components"}, "--component and --clear-components cannot be given together"},
		{[]string{"edit", "--notice-id", "1", "--update-id", "1", "--title", "T"}, "leave out --update-id to edit the notice"},
		{[]string{"edit", "--notice-id", "1", "--update-id", "9", "--message", "x"}, "notice 1 has no update 9"},
		{[]string{"edit", "--notice-id", "1", "--component", "cdn"}, `the status page has no component "cdn"`},
		{[]string{"delete", "--notice-id", "9"}, "the status page has no notice 9"},
	} {
		if state, output := laptop(test.args...); state.ExitCode() == 0 || !strings.Contains(output, test.want) {
			t.Errorf("%v: exit %d: %s", test.args, state.ExitCode(), output)
		}
	}
	if again, _ := json.Marshal(read()); string(again) != string(snapshot) {
		t.Fatalf("a refused request changed the page:\n%s\n%s", snapshot, again)
	}

	// Delete an update, never the last one, then the notice.
	if state, output := laptop("delete", "--notice-id", "1", "--update-id", "1"); !state.Success() || !strings.Contains(output, "Deleted update 1 of notice 1, directly in the R2 bucket.") {
		t.Fatalf("delete the update: %s", output)
	}
	if n := read().Notices[1]; len(n.Updates) != 1 || n.Updates[0].UpdateID != 2 || n.NextUpdateID != 3 {
		t.Fatalf("after deleting update 1: %+v", n)
	}
	if state, output := laptop("delete", "--notice-id", "1", "--update-id", "2"); state.ExitCode() != 4 || !strings.Contains(output, "a notice must keep at least one update; delete the notice instead") {
		t.Fatalf("deleting the last update: exit %d: %s", state.ExitCode(), output)
	}
	if state, output := laptop("delete", "--notice-id", "2"); !state.Success() || !strings.Contains(output, "Deleted notice 2, directly in the R2 bucket.") || strings.Contains(output, "Test notice") {
		t.Fatalf("delete the notice: %s", output)
	}
	file := read()
	if len(file.Notices) != 1 || file.Notices[0].NoticeID != 1 || file.NextNoticeID != 3 {
		t.Fatalf("after deleting notice 2: next %d, %+v", file.NextNoticeID, file.Notices)
	}
	if page, _ := fake.Object("mmhotline-status-status-example-com-18a8ea11", "index.html"); strings.Contains(string(page.Content), "Test notice") || !strings.Contains(string(page.Content), "Sign-in is slow") || !strings.Contains(string(page.Content), "(edited)") {
		t.Fatalf("index.html: %s", page.Content)
	}
	if state, _ := laptop("post", "--title", "Next", "--message", "m"); !state.Success() || read().Notices[0].NoticeID != 3 {
		t.Fatal("the next notice reused a deleted ID")
	}
}

// Through the service, edit and delete send what was given: a flag left out
// stays nil, a clear flag is true only when given, and --update-id 0 is sent
// nowhere because the command refuses it first.
func TestStatusPageEditAndDeleteSendOnlyWhatWasGiven(t *testing.T) {
	socket := filepath.Join(shortSocketDirectory(t), "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan app.CommandRequest, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request app.CommandRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		json.NewEncoder(w).Encode(app.CommandResult{PeopleCommandResult: app.PeopleCommandResult{Message: "Done."}})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	text, number := func(s string) *string { return &s }, func(n int64) *int64 { return &n }
	for _, test := range []struct {
		args []string
		want app.StatusPageCommandRequest
	}{
		{[]string{"edit", "--notice-id", "3", "--title", "New title"}, app.StatusPageCommandRequest{NoticeID: number(3), Title: text("New title")}},
		{[]string{"edit", "--notice-id", "3", "--clear-components", "--clear-schedule"}, app.StatusPageCommandRequest{NoticeID: number(3), ClearComponents: true, ClearSchedule: true}},
		{[]string{"edit", "--notice-id", "3", "--component", "api", "--component", "website"}, app.StatusPageCommandRequest{NoticeID: number(3), AffectedComponentIDs: &[]string{"api", "website"}}},
		{[]string{"edit", "--notice-id", "3", "--update-id", "2", "--clear-stage"}, app.StatusPageCommandRequest{NoticeID: number(3), UpdateID: number(2), ClearStage: true}},
		{[]string{"edit", "--notice-id", "3", "--update-id", "2", "--message", "Fixed typo.", "--stage", "monitoring"}, app.StatusPageCommandRequest{NoticeID: number(3), UpdateID: number(2), Message: text("Fixed typo."), Stage: text("monitoring")}},
		{[]string{"delete", "--notice-id", "3"}, app.StatusPageCommandRequest{NoticeID: number(3)}},
		{[]string{"delete", "--notice-id", "3", "--update-id", "2"}, app.StatusPageCommandRequest{NoticeID: number(3), UpdateID: number(2)}},
	} {
		if output, err := cliProcess(t, append([]string{"status-page"}, append(test.args, "--command-socket", socket)...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", test.args, output, err)
		}
		if request := <-requests; !reflect.DeepEqual(request.StatusPage, test.want) || request.Action != test.args[0] {
			t.Errorf("%v sent %+v, want %+v", test.args, request.StatusPage, test.want)
		}
	}
	// An --update-id 0 never reaches the server.
	if output, err := cliProcess(t, "status-page", "delete", "--notice-id", "3", "--update-id", "0", "--command-socket", socket).CombinedOutput(); err == nil || !strings.Contains(string(output), "--update-id must be 1 or more") {
		t.Fatalf("--update-id 0: %s %v", output, err)
	}
	select {
	case request := <-requests:
		t.Fatalf("a refused command was sent: %+v", request)
	default:
	}
}

// runStatusPageCapturingOutput runs a status-page command in this process,
// with standard input, and returns what it printed. It replaces the
// standard input and output, so it must not run in parallel with other
// tests.
func runStatusPageCapturingOutput(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	inputReader, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { inputWriter.WriteString(input); inputWriter.Close() }()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inputReader, writer
	output := make(chan string)
	go func() {
		raw, _ := io.ReadAll(reader)
		output <- string(raw)
	}()
	runErr := runManagementCommand("status-page", args)
	os.Stdin, os.Stdout = stdin, stdout
	writer.Close()
	inputReader.Close()
	return <-output, runErr
}

// TestStatusPageSetUpFromALaptop runs status-page set-up as a person does
// on a laptop: against the fake Cloudflare API, with no MMHotline and no
// command socket, and with the person's own token from a private file or
// standard input, which it never prints. It prints r2_bucket_url for the
// settings file and the next step, and a second run changes nothing.
func TestStatusPageSetUpFromALaptop(t *testing.T) {
	fake := fakecloudflare.New(t)
	previous := statusPageCloudflare
	statusPageCloudflare = fake.Addresses()
	t.Cleanup(func() { statusPageCloudflare = previous })
	const bucket = "mmhotline-status-status-example-com-18a8ea11"
	tokenFile := filepath.Join(t.TempDir(), "cloudflare-api-token")
	if err := os.WriteFile(tokenFile, []byte(fake.APIToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"set-up", "--cloudflare-account-id", fake.AccountID, "--host-name", "status.example.com", "--cloudflare-api-token-file"}
	if _, err := runStatusPageCapturingOutput(t, "", append(args, tokenFile)...); err == nil || !strings.Contains(err.Error(), "can be read by other users; run chmod 600 "+tokenFile+" first") || len(fake.Calls()) != 0 {
		t.Fatalf("a readable token file was accepted: %v", err)
	}
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runStatusPageCapturingOutput(t, "", append(args, tokenFile)...)
	t.Logf("status-page set-up:\n%s", output)
	bucketURL := fake.Addresses().R2BucketURL(fake.AccountID, bucket)
	want := "The status page is set up in Cloudflare account " + fake.AccountID + ": the R2 bucket " + bucket + ", the host name status.example.com connected to it, and the rule that serves index.html at https://status.example.com/.\n" +
		"r2_bucket_url: " + bucketURL + "\n" +
		"Next step: on Cloudflare's site, open R2, then Manage API tokens, and create an API token with the permission Object Read & Write for the bucket " + bucket + " only. Put r2_bucket_url above and the token's Access Key ID and Secret Access Key in the settings file's status_page section, as r2_bucket_url, r2_access_key_id, and r2_secret_access_key (with a deploy tool, in its vault), then apply the file, with your deploy tool or with sudo mmhotline settings apply on the MMHotline server. The first apply publishes the page.\n"
	if err != nil || output != want {
		t.Fatalf("set-up: %v\n%s", err, output)
	}
	if domains := fake.CustomDomains(bucket); len(domains) != 1 || domains[0] != "status.example.com" || len(fake.TransformRules("zone-example-com")) != 1 {
		t.Fatalf("Cloudflare: host names %v, rules %+v", domains, fake.TransformRules("zone-example-com"))
	}
	if _, published := fake.Object(bucket, "status.json"); published {
		t.Fatal("set-up published a page")
	}
	// The token from standard input; a second run changes nothing.
	calls := len(fake.Calls())
	output, err = runStatusPageCapturingOutput(t, fake.APIToken+"\n", append(args, "-", "--json")...)
	var result app.StatusPageCloudflareSetUp
	if err != nil || json.Unmarshal([]byte(output), &result) != nil || result.R2BucketURL != bucketURL || result.BucketName != bucket || result.PageURL != "https://status.example.com/" || !strings.Contains(result.NextStep, "Object Read & Write for the bucket "+bucket+" only") {
		t.Fatalf("set-up --json: %v\n%s", err, output)
	}
	for _, call := range fake.Calls()[calls:] {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("a second set-up changed something: %s", call)
		}
	}
	// It takes no --page-title, which the settings file has.
	if _, err := runStatusPageCapturingOutput(t, "", append(args, tokenFile, "--page-title", "Example Inc.")...); err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -page-title") {
		t.Fatalf("--page-title: %v", err)
	}
}
