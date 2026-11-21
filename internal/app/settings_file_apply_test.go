package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"

	"mmhotline/internal/fakecloudflare"
	"mmhotline/internal/fakesendingservices"
	"mmhotline/internal/triageagent"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/systemsrepository"
)

// settingsFileFixture is a team whose settings come from a settings file,
// as serve runs it. Each part with an outside test reaches a fake: the fake
// sending services, the fake Cloudflare API, and a fake sandbox that
// answers each setup test (settingsFileFakes).
type settingsFileFixture struct {
	t     *testing.T
	a     *App
	store *Store
	path  string
	fakes *settingsFileFakes
}

// settingsFileFakes are the fakes the settings file's outside tests reach.
type settingsFileFakes struct {
	cloudflare *fakecloudflare.Server
	// sendingServicesURL is the fake sending services' address.
	sendingServicesURL string
	mu                 sync.Mutex
	// runRequests are the run requests the fake sandbox received.
	runRequests []triageagent.Request
	// answer is the fake sandbox's final event for a run; by default it
	// passes the setup test of the run's agent.
	answer func(triageagent.Request) triageagent.Event
}

// settingsFileFakeBucket is the example's status page bucket in the fake
// Cloudflare account.
const settingsFileFakeBucket = "status"

func newSettingsFileFixture(t *testing.T, text string) *settingsFileFixture {
	t.Helper()
	dir := t.TempDir()
	if err := Setup(context.Background(), dir, "Fixture team"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sendingServices := httptest.NewServer(fakesendingservices.New())
	t.Cleanup(sendingServices.Close)
	fakes := &settingsFileFakes{cloudflare: fakecloudflare.New(t), sendingServicesURL: sendingServices.URL}
	fakes.cloudflare.AddBucket(settingsFileFakeBucket)
	f := &settingsFileFixture{t: t, store: store, path: writeSettingsFile(t, ""), fakes: fakes}
	f.write(text)
	f.restart()
	return f
}

// restart is a new MMHotline process on the same database, which has not
// read the file yet.
func (f *settingsFileFixture) restart() {
	f.t.Helper()
	f.a = New(f.store)
	f.a.UseSettingsFile(f.path, []uint32{uint32(os.Getuid())})
	f.a.Cloudflare = f.fakes.cloudflare.Addresses()
	if err := f.a.LoadSendingServiceSetups(context.Background(), nil, f.fakes.sendingServicesURL); err != nil {
		f.t.Fatal(err)
	}
	runs := triageRunFixture(f.t, f.a)
	go func() {
		for run := range runs {
			f.fakes.mu.Lock()
			f.fakes.runRequests = append(f.fakes.runRequests, run.request)
			answer := f.fakes.answer
			f.fakes.mu.Unlock()
			event := setupTestPassed(run.request)
			if answer != nil {
				event = answer(run.request)
			}
			json.NewEncoder(run.connection).Encode(event)
		}
	}()
}

// setupTestPassed is the coding agent's answer to its agent's setup test
// prompt, which passes the setup test and the sign-in check.
func setupTestPassed(request triageagent.Request) triageagent.Event {
	sentence := triageAgentTestSentence
	if request.AgentRole == triageagent.ReviewAgentRole {
		sentence = reviewAgentTestSentence
	}
	return triageagent.Event{Event: "finished", Answer: sentence + "."}
}

// requests are the run requests the fake sandbox has received.
func (f *settingsFileFixture) requests() []triageagent.Request {
	f.fakes.mu.Lock()
	defer f.fakes.mu.Unlock()
	return slices.Clone(f.fakes.runRequests)
}

// answerRuns sets the fake sandbox's final event for each run.
func (f *settingsFileFixture) answerRuns(answer func(triageagent.Request) triageagent.Event) {
	f.fakes.mu.Lock()
	defer f.fakes.mu.Unlock()
	f.fakes.answer = answer
}

// start reads and applies the file as serve does at start, and waits for
// the parts with an outside test, which apply in the background.
func (f *settingsFileFixture) start() ServerSettings {
	f.t.Helper()
	server, err := f.a.LoadSettingsFileAtStart(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.a.ApplySettingsFileAtStart(context.Background(), func(err error) { f.t.Error(err) }); err != nil {
		f.t.Fatal(err)
	}
	f.a.WaitForSettingsFileAppliedAtStart()
	return server
}

// write replaces the file's contents and keeps its owner and mode. The
// example's values for outside services become the fakes': its Pushover
// token, its Cloudflare token, and its R2 bucket and key.
func (f *settingsFileFixture) write(text string) {
	f.t.Helper()
	for _, pair := range [][2]string{
		{"azGDORePK8gMaC0QOYAMyEEuzJnyUi", fakesendingservices.PushoverApplicationToken},
		{"example-cloudflare-workers-token", f.fakes.cloudflare.APIToken},
		{"https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/status", f.fakes.cloudflare.Addresses().R2BucketURL(f.fakes.cloudflare.AccountID, settingsFileFakeBucket)},
		{`r2_access_key_id: "0123456789abcdef0123456789abcdef"`, `r2_access_key_id: "` + f.fakes.cloudflare.R2AccessKeyID + `"`},
	} {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	if err := os.WriteFile(f.path, []byte(text), 0o640); err != nil {
		f.t.Fatal(err)
	}
}

func (f *settingsFileFixture) apply() SettingsFileView {
	f.t.Helper()
	view, err := f.a.ApplySettingsFile(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return view
}

// states is each part's state: applied, unchanged, failed, or
// never_applied, with the failure reason after a colon.
func states(view SettingsFileView) map[string]string {
	states := map[string]string{}
	for _, part := range view.Parts {
		states[part.Part] = part.State
		if part.FailureReason != "" {
			states[part.Part] += ": " + part.FailureReason
		}
	}
	return states
}

// replaced is the example with each pair's first text replaced by the
// second, and fails when the example lacks one.
func replaced(t *testing.T, text string, pairs ...string) string {
	t.Helper()
	for index := 0; index+1 < len(pairs); index += 2 {
		if !strings.Contains(text, pairs[index]) {
			t.Fatalf("the settings have no %q", pairs[index])
		}
		text = strings.Replace(text, pairs[index], pairs[index+1], 1)
	}
	return text
}

const exampleMonitoringSecret = "5f0c2d6e8a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"

// sendSignedAlert sends an info alert as the alert source, signed with the
// secret.
func sendSignedAlert(a *App, sourceID, secret, senderAlertID string) error {
	body := []byte(`{"sender_alert_id":"` + senderAlertID + `","severity":"info","title":"Disk is filling up","message":"80 percent"}`)
	_, _, _, err := a.AdmitGeneric(context.Background(), sourceID, alertSignature(secret, a.Now(), body), body)
	return err
}

func TestSettingsFileAppliesEachPartAtStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	server := f.start()
	if server.WebURL != "https://hotline.example.com" || server.WebListenAddress != "127.0.0.1:8480" || server.AlertIntakeListenAddress != "10.20.0.1:8484" || server.CloudflareAccess == nil || server.CloudflareAccess.TeamDomain != "example.cloudflareaccess.com" {
		t.Fatalf("server settings %+v", server)
	}
	a, root := f.a, Principal{UnixUser: "root"}
	changedBy := "the settings file " + f.path
	var team string
	if err := a.Store.db.QueryRow(`SELECT name FROM team_info`).Scan(&team); err != nil || team != "Example Operations" {
		t.Fatalf("team name %q, %v", team, err)
	}
	people, err := a.TeamMembers(ctx, root)
	if err != nil || len(people) != 1 || people[0].Name != "Ana Silva" || people[0].SignInEmailAddress != "ana@example.com" || people[0].Role != RoleAdministrator {
		t.Fatalf("people %+v, %v", people, err)
	}
	schedules, err := a.Schedules(ctx, root)
	if err != nil || schedules.ActiveKind != "weekly_rotation" || len(schedules.Configs) != 1 || !slices.Equal(schedules.Configs[0].RotationMemberIDs, []string{people[0].ID}) || schedules.Configs[0].FirstStartDate != "2026-11-02" {
		t.Fatalf("schedules %+v, %v", schedules, err)
	}
	if window, err := a.EscalateIfUnclaimedAfter(ctx, root); err != nil || window != 5*time.Minute {
		t.Fatalf("time to claim %s, %v", window, err)
	}
	rules, err := a.AgentPlaybookRules(ctx, root, "production")
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules for running the agent's playbooks %+v, %v", rules, err)
	}
	preset, _ := findAgentPlaybookRulesPreset("routine_fixes_without_permission")
	for _, kind := range rules[0].ActionKinds {
		if kind.AgentPlaybookRule != preset.settings[kind.ActionKind] || kind.ChangedBy != changedBy {
			t.Fatalf("rule %+v", kind)
		}
	}
	services, err := a.ServicesWhoseChangesAPersonMustApprove(ctx, root)
	if err != nil || !slices.Equal(services.ServiceNames, []string{"pgautofailover.service"}) || services.ChangedBy != changedBy {
		t.Fatalf("services %+v, %v", services, err)
	}
	if err := sendSignedAlert(a, "monitoring", exampleMonitoringSecret, "settings-1"); err != nil {
		t.Fatalf("an alert signed with the file's secret: %v", err)
	}
	sources, err := a.Sources(ctx, root)
	if err != nil || len(sources) != 1 || sources[0].Name != "Monitoring" || !sources[0].Active {
		t.Fatalf("alert sources %+v, %v", sources, err)
	}
	checkIns, err := a.AlertSourceCheckInStatus(ctx, root, "monitoring")
	if err != nil || checkIns.WarnAfterNoCheckInFor != 2*time.Minute || checkIns.SOSAfterNoCheckInFor != 5*time.Minute {
		t.Fatalf("check-in times %+v, %v", checkIns, err)
	}
	if !slices.Equal(a.fleetViewCommand(), []string{"/usr/local/bin/MONITORING_TOOL", "fleet", "--json"}) {
		t.Fatalf("fleet view command %q", a.fleetViewCommand())
	}
	shown, err := a.SettingsFileState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range shown.Parts {
		if part.State != "applied" {
			t.Errorf("part %s is %s", part.Part, part.State)
		}
	}
	if shown.FirstCommentLine == "" || shown.LastAppliedAt == nil || !strings.Contains(shown.Text(), "people: applied at ") {
		t.Fatalf("settings show:\n%s", shown.Text())
	}
}

func TestSettingsFileAppliesTheTriageAgentsOwnSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, replaced(t, exampleSettingsYAML,
		"internet_access: listed_hosts_only", "internet_access: any_site",
		"listed_hosts: []", "listed_hosts: [{host: github.com, port: 443}, {host: osv.example.org, port: 8443}]",
		"software_review_interval_hours: 24", "software_review_interval_hours: 6"))
	f.start()
	var signingKey string
	if err := f.a.Store.db.QueryRow(`SELECT commit_signing_public_key FROM triage_agent_commit_check_state`).Scan(&signingKey); err != nil || signingKey != testTriageAgentCommitSigningPublicKey {
		t.Fatalf("commit signing public key %q, %v", signingKey, err)
	}
	view, err := f.a.TriageAgentState(ctx, Principal{UnixUser: "root"})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, host := range view.ListedHosts {
		if host.AddedBy == "the settings file "+f.path {
			listed = append(listed, host.Host)
		}
	}
	if view.InternetAccess != "any_site" || !slices.Equal(listed, []string{"github.com", "osv.example.org"}) || view.SoftwareReviewIntervalHours != 6 {
		t.Fatalf("internet access %s, listed %q, interval %d", view.InternetAccess, listed, view.SoftwareReviewIntervalHours)
	}
	f.write(replaced(t, exampleSettingsYAML, "listed_hosts: []", "listed_hosts: [{host: github.com, port: 443}]"))
	f.apply()
	if view, err = f.a.TriageAgentState(ctx, Principal{UnixUser: "root"}); err != nil {
		t.Fatal(err)
	}
	listed = nil
	for _, host := range view.ListedHosts {
		if host.AddedBy != "" {
			listed = append(listed, host.Host)
		}
	}
	if view.InternetAccess != "listed_hosts_only" || !slices.Equal(listed, []string{"github.com"}) || view.SoftwareReviewIntervalHours != 24 {
		t.Fatalf("after the change: internet access %s, listed %q, interval %d", view.InternetAccess, listed, view.SoftwareReviewIntervalHours)
	}
	// The part's outcome says plainly whether the scheduled review runs, and
	// off turns it off.
	intervalNotes := func(applied SettingsFileView) []SettingsNote {
		for _, part := range applied.Parts {
			if part.Part == "triage_agent.software_review_interval_hours" {
				return part.Notes
			}
		}
		return nil
	}
	if notes := intervalNotes(f.apply()); len(notes) != 1 || notes[0].Note != "Scheduled software review: every 24 hours, with the team's task in scheduled_software_review_task.md in the systems repository." {
		t.Fatalf("notes with 24 hours: %+v", notes)
	}
	f.write(replaced(t, exampleSettingsYAML, "software_review_interval_hours: 24", "software_review_interval_hours: off"))
	if notes := intervalNotes(f.apply()); len(notes) != 1 || notes[0].Note != "Scheduled software review: off. The triage agent wakes only for alerts; sudo mmhotline triage-agent run-software-review still runs one review, such as to learn what one costs." {
		t.Fatalf("notes with off: %+v", notes)
	}
	if view, err = f.a.TriageAgentState(ctx, Principal{UnixUser: "root"}); err != nil || view.SoftwareReviewIntervalHours != softwareReviewIntervalOff || view.SoftwareReviewIntervalChangedBy != "the settings file "+f.path {
		t.Fatalf("after off: interval %d, set by %q, %v", view.SoftwareReviewIntervalHours, view.SoftwareReviewIntervalChangedBy, err)
	}
}

// TestUnchangedPartsAreNotWrittenAgain: a part whose value is the one in
// effect is neither tested nor written again; only a changed part applies.
func TestUnchangedPartsAreNotWrittenAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	before, err := f.a.ServicesWhoseChangesAPersonMustApprove(ctx, Principal{UnixUser: "root"})
	if err != nil {
		t.Fatal(err)
	}
	f.a.Now = func() time.Time { return time.Now().Add(time.Hour) }
	view := f.apply()
	for part, state := range states(view) {
		if state != "unchanged" {
			t.Errorf("part %s of an unchanged file: %s", part, state)
		}
	}
	after, err := f.a.ServicesWhoseChangesAPersonMustApprove(ctx, Principal{UnixUser: "root"})
	if err != nil || !after.ChangedAt.Equal(*before.ChangedAt) {
		t.Fatalf("the unchanged list was saved again: %v then %v, %v", before.ChangedAt, after.ChangedAt, err)
	}
	f.write(replaced(t, exampleSettingsYAML, "[pgautofailover.service]", "[pgautofailover.service, caddy]"))
	got := states(f.apply())
	for part, state := range got {
		want := "unchanged"
		if part == "services_whose_changes_a_person_must_approve" {
			want = "applied"
		}
		if state != want {
			t.Errorf("part %s: %s, want %s", part, state, want)
		}
	}
	if after, err = f.a.ServicesWhoseChangesAPersonMustApprove(ctx, Principal{UnixUser: "root"}); err != nil || !slices.Equal(after.ServiceNames, []string{"pgautofailover.service", "caddy.service"}) {
		t.Fatalf("services %+v, %v", after, err)
	}
}

// TestAFailedPartKeepsItsEarlierSetting: the people part fails as a whole
// when a person it would deactivate owns a claimed incident, while the other
// parts apply; once the incident is resolved, the next apply deactivates
// the person and keeps their history.
func TestAFailedPartKeepsItsEarlierSetting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	withBruno := replaced(t, exampleSettingsYAML, "    role: administrator\n", "    role: administrator\n  - name: Bruno Costa\n    sign_in_email_address: bruno@example.com\n    role: member\n")
	f := newSettingsFileFixture(t, withBruno)
	f.start()
	bruno := Principal{SignInEmailAddress: "bruno@example.com"}
	alert, err := f.a.ManualSOS(ctx, bruno, "Disk full on db01", "")
	if err != nil {
		t.Fatal(err)
	}
	incident, err := f.a.GetIncident(ctx, bruno, alert.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if incident, err = f.a.ClaimIncident(ctx, bruno, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	// Bruno leaves, Carla joins, and the team is renamed, in one edit.
	f.write(replaced(t, exampleSettingsYAML,
		"team_name: Example Operations", "team_name: Example Platform",
		"    role: administrator\n", "    role: administrator\n  - name: Carla Diaz\n    sign_in_email_address: carla@example.com\n    role: member\n"))
	view := f.apply()
	got := states(view)
	if got["team_name"] != "applied" || !strings.HasPrefix(got["people"], "failed: Bruno Costa owns 1 claimed incident. Resolve the claimed incidents or have someone take over the claimed incidents before removing the person") {
		t.Fatalf("parts %v", got)
	}
	if failed := view.FailedParts(); !slices.Equal(failed, []string{"people"}) {
		t.Fatalf("failed parts %q", failed)
	}
	people, err := f.a.TeamMembers(ctx, Principal{UnixUser: "root"})
	if err != nil || len(people) != 2 || people[1].Name != "Bruno Costa" {
		t.Fatalf("the earlier people did not stay: %+v, %v", people, err)
	}
	text := view.Text()
	if !strings.Contains(text, "people: did not apply: Bruno Costa owns 1 claimed incident. Resolve the claimed incidents or have someone take over the claimed incidents before removing the person. The earlier setting, applied at ") || !strings.Contains(text, "team_name: applied.") {
		t.Fatalf("apply's text:\n%s", text)
	}
	shown, err := f.a.SettingsFileState(ctx)
	if err != nil || !strings.Contains(shown.Text(), "people: did not apply: Bruno Costa owns 1 claimed incident") {
		t.Fatalf("settings show:\n%s\n%v", shown.Text(), err)
	}
	if _, err = f.a.ResolveIncident(ctx, bruno, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if got := states(f.apply()); got["people"] != "applied" || got["team_name"] != "unchanged" {
		t.Fatalf("after the resolution: %v", got)
	}
	people, err = f.a.TeamMembers(ctx, Principal{UnixUser: "root"})
	if err != nil || len(people) != 2 || people[0].Name != "Ana Silva" || people[1].Name != "Carla Diaz" {
		t.Fatalf("people %+v, %v", people, err)
	}
	history, err := f.a.membersForHistory(ctx, Principal{UnixUser: "root"})
	if err != nil || !slices.ContainsFunc(history, func(m Member) bool { return m.Name == "Bruno Costa" }) {
		t.Fatalf("Bruno's history is gone: %+v, %v", history, err)
	}
	if _, err := f.a.CurrentMember(ctx, bruno); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Bruno can still sign in: %v", err)
	}
}

// TestPeopleAndScheduleChangeInOneEdit: a person takes over another's place
// in the rotation in one edit, which applies whole.
func TestPeopleAndScheduleChangeInOneEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, replaced(t, exampleSettingsYAML,
		"    role: administrator\n", "    role: administrator\n  - name: Bruno Costa\n    sign_in_email_address: bruno@example.com\n    role: member\n",
		"rotation_sign_in_email_addresses: [ana@example.com]", "rotation_sign_in_email_addresses: [ana@example.com, bruno@example.com]"))
	f.start()
	f.write(replaced(t, exampleSettingsYAML,
		"    role: administrator\n", "    role: administrator\n  - name: Carla Diaz\n    sign_in_email_address: carla@example.com\n    role: member\n",
		"rotation_sign_in_email_addresses: [ana@example.com]", "rotation_sign_in_email_addresses: [ana@example.com, carla@example.com]"))
	view := f.apply()
	if failed := view.FailedParts(); len(failed) != 0 {
		t.Fatalf("parts %v", states(view))
	}
	people, err := f.a.TeamMembers(ctx, Principal{UnixUser: "root"})
	if err != nil || len(people) != 2 || people[1].Name != "Carla Diaz" {
		t.Fatalf("people %+v, %v", people, err)
	}
	schedules, err := f.a.Schedules(ctx, Principal{UnixUser: "root"})
	if err != nil || len(schedules.Configs) != 1 || !slices.Equal(schedules.Configs[0].RotationMemberIDs, []string{people[0].ID, people[1].ID}) {
		t.Fatalf("schedules %+v, %v", schedules, err)
	}
}

// TestTheScheduleIsTheOnlySavedSchedule: the file's schedule replaces every
// saved schedule, so a saved schedule of another type no longer names
// people who may have left.
func TestTheScheduleIsTheOnlySavedSchedule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	f.write(replaced(t, exampleSettingsYAML,
		"  schedule_type: weekly_rotation\n", "  schedule_type: day_of_week\n",
		"  handoff_weekday: 1\n  first_start_date: \"2026-11-02\"\n  rotation_sign_in_email_addresses: [ana@example.com]\n", "  assignment_sign_in_email_addresses: [ana@example.com, ana@example.com, \"\", ana@example.com, ana@example.com, ana@example.com, ana@example.com]\n"))
	if got := states(f.apply()); got["on_call_schedule"] != "applied" {
		t.Fatalf("parts %v", got)
	}
	schedules, err := f.a.Schedules(ctx, Principal{UnixUser: "root"})
	if err != nil || schedules.ActiveKind != "day_of_week" || len(schedules.Configs) != 1 || schedules.Configs[0].Assignments[2] != "" {
		t.Fatalf("schedules %+v, %v", schedules, err)
	}
}

// TestAnInventoryWithoutAgentPlaybookRules: an inventory of MMHotline's copy of
// the systems repository that the file leaves out fails the rules part,
// which names it, and gets the safest setting for every kind; the listed
// inventories apply.
func TestAnInventoryWithoutAgentPlaybookRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	inventories := filepath.Join(f.store.Dir, systemsrepository.DirectoryName, "inventories")
	if err := os.MkdirAll(inventories, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"production", "staging"} {
		if err := os.WriteFile(filepath.Join(inventories, name+".yml"), []byte("nodes: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.start()
	shown, err := f.a.SettingsFileState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := states(shown)["rules_for_running_agent_playbooks"]; got != "failed: inventory staging has no rules for running the agent's playbooks. Every fix in the inventory needs a person until you add its rules" {
		t.Fatalf("rules part: %s", got)
	}
	if !strings.Contains(shown.Text(), "rules_for_running_agent_playbooks: did not apply: inventory staging has no rules for running the agent's playbooks. Every fix in the inventory needs a person until you add its rules. The rules for running the agent's playbooks applied for the inventories listed in the settings file. Any inventory without rules requires a person to approve every fix until you add its rules.") {
		t.Fatalf("settings show:\n%s", shown.Text())
	}
	for _, inventory := range []string{"production", "staging"} {
		rules, err := f.a.AgentPlaybookRules(ctx, Principal{UnixUser: "root"}, inventory)
		if err != nil {
			t.Fatal(err)
		}
		preset, _ := findAgentPlaybookRulesPreset("routine_fixes_without_permission")
		for _, kind := range rules[0].ActionKinds {
			want := safestAgentPlaybookRule
			if inventory == "production" {
				want = preset.settings[kind.ActionKind]
			}
			if kind.AgentPlaybookRule != want || kind.ChangedAt == nil {
				t.Errorf("%s, %s: %+v", inventory, kind.ActionKind, kind)
			}
		}
	}
	// An unchanged file with the inventory still left out keeps failing.
	if got := states(f.apply())["rules_for_running_agent_playbooks"]; !strings.HasPrefix(got, "failed: inventory staging") {
		t.Fatalf("rules part again: %s", got)
	}
	f.write(replaced(t, exampleSettingsYAML, "    preset: routine_fixes_without_permission\n", "    preset: routine_fixes_without_permission\n  staging:\n    preset: every_fix_without_permission\n"))
	if got := states(f.apply())["rules_for_running_agent_playbooks"]; got != "applied" {
		t.Fatalf("rules part with staging: %s", got)
	}
	rules, err := f.a.AgentPlaybookRules(ctx, Principal{UnixUser: "root"}, "staging")
	if err != nil || rules[0].ActionKinds[0].Rule != actThenReport {
		t.Fatalf("staging %+v, %v", rules, err)
	}
}

// TestAKindWithoutASavedSettingNeedsAPerson: no inventory gets a silent
// default; every kind of one with no saved setting waits for a person.
func TestAKindWithoutASavedSettingNeedsAPerson(t *testing.T) {
	t.Parallel()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	rules, err := f.a.AgentPlaybookRules(context.Background(), Principal{UnixUser: "root"}, "testing")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range rules[0].ActionKinds {
		if kind.AgentPlaybookRule != (AgentPlaybookRule{Rule: getPermissionThenAct, ApprovalsNeeded: 1}) || kind.ChangedAt != nil {
			t.Errorf("%s: %+v", kind.ActionKind, kind)
		}
		if kind.ActionKind == actionkinds.CheckOnly && !strings.HasSuffix(kind.Text(), "; check_only has no rule; it needs a person until you add one.") {
			t.Errorf("text %q", kind.Text())
		}
	}
}

// TestAnAlertSourceLeftOutIsDisabled: its secrets stop working at once, its
// alerts stay, and listing it again enables it.
func TestAnAlertSourceLeftOutIsDisabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	if err := sendSignedAlert(f.a, "monitoring", exampleMonitoringSecret, "before"); err != nil {
		t.Fatal(err)
	}
	withoutSources := exampleSettingsYAML[:strings.Index(exampleSettingsYAML, "alert_sources:")] + "alert_sources: []\n"
	f.write(withoutSources)
	if got := states(f.apply())["alert_sources"]; got != "applied" {
		t.Fatalf("alert sources part: %s", got)
	}
	if err := sendSignedAlert(f.a, "monitoring", exampleMonitoringSecret, "after"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an alert from the disabled source: %v", err)
	}
	sources, err := f.a.Sources(ctx, Principal{UnixUser: "root"})
	if err != nil || len(sources) != 1 || sources[0].Active || len(sources[0].Secrets) != 0 {
		t.Fatalf("sources %+v, %v", sources, err)
	}
	alerts, err := f.a.ListAlerts(ctx, Principal{UnixUser: "root"}, AlertFilter{SourceID: "monitoring", Limit: 10})
	if err != nil || len(alerts) != 1 {
		t.Fatalf("the source's alerts %+v, %v", alerts, err)
	}
	// Listed again with a second secret, while the sender moves to it.
	const newSecret = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	f.write(replaced(t, exampleSettingsYAML, `signing_secrets: ["`+exampleMonitoringSecret+`"]`, `signing_secrets: ["`+exampleMonitoringSecret+`", "`+newSecret+`"]`))
	f.apply()
	for index, secret := range []string{exampleMonitoringSecret, newSecret} {
		if err := sendSignedAlert(f.a, "monitoring", secret, "again-"+secret[:4]); err != nil {
			t.Fatalf("secret %d after listing the source again: %v", index, err)
		}
	}
}

// TestARestartKeyChangeAppliesNothing: settings apply answers that a
// restart is needed, naming the keys, and applies nothing, so the command
// restarts MMHotline first.
func TestARestartKeyChangeAppliesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	f.write(replaced(t, exampleSettingsYAML,
		"web_listen_address: 127.0.0.1:8480", "web_listen_address: 127.0.0.1:8490",
		"  audience_tag: 0123456789abcdef", "  audience_tag: fedcba9876543210",
		"team_name: Example Operations", "team_name: Example Platform"))
	view := f.apply()
	if !slices.Equal(view.RestartNeededFor, []string{"web_listen_address", "cloudflare_access"}) || len(view.Parts) != 0 {
		t.Fatalf("view %+v", view)
	}
	var team string
	if err := f.a.Store.db.QueryRowContext(ctx, `SELECT name FROM team_info`).Scan(&team); err != nil || team != "Example Operations" {
		t.Fatalf("team name %q, %v", team, err)
	}
	// After the restart, the new process starts with the new keys and
	// applies the rest; asked again, it finds everything applied.
	f.restart()
	if server := f.start(); server.WebListenAddress != "127.0.0.1:8490" {
		t.Fatalf("server after the restart %+v", server)
	}
	view = f.apply()
	if len(view.RestartNeededFor) != 0 || len(view.FailedParts()) != 0 {
		t.Fatalf("after the restart %+v", view)
	}
}

// TestAStartWithABrokenFileUsesTheEarlierSettings (CA7): serve starts with
// the settings it last applied and warns the administrators, naming the
// problems by key path only.
func TestAStartWithABrokenFileUsesTheEarlierSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	f.write(replaced(t, exampleSettingsYAML,
		"web_listen_address: 127.0.0.1:8480", "web_listen_address: 127.0.0.1:8490",
		"team_name: Example Operations", "team_name: Example Operations\nteam_nmae: Typo Operations",
		`credential: "sk-ant-oat01-example-triage-agent-credential"`, `credential: "sk-ant-oat01-a secret with a bell \a"`))
	f.restart()
	server := f.start()
	if server.WebListenAddress != "127.0.0.1:8480" || !slices.Equal(f.a.fleetViewCommand(), []string{"/usr/local/bin/MONITORING_TOOL", "fleet", "--json"}) {
		t.Fatalf("the start did not use the earlier settings: %+v, %q", server, f.a.fleetViewCommand())
	}
	var title, message string
	if err := f.a.Store.db.QueryRowContext(ctx, `SELECT title,message FROM alerts WHERE severity='warn' AND source_id='system' AND title LIKE 'The settings file has problems%'`).Scan(&title, &message); err != nil {
		t.Fatal(err)
	}
	if title != "The settings file has problems; MMHotline uses its earlier settings" || !strings.Contains(message, "has problems at: team_nmae; triage_agent.credential. Alerts keep flowing with the earlier settings.") {
		t.Fatalf("warning %q: %q", title, message)
	}
	if strings.Contains(message, "Typo Operations") || strings.Contains(message, "sk-ant-oat01") {
		t.Fatalf("the warning shows a value: %q", message)
	}
	// A file that someone other than root could change is refused the same way.
	f.write(exampleSettingsYAML)
	if err := os.Chmod(f.path, 0o666); err != nil {
		t.Fatal(err)
	}
	f.restart()
	if server = f.start(); server.WebListenAddress != "127.0.0.1:8480" {
		t.Fatalf("server %+v", server)
	}
	if err := f.a.Store.db.QueryRowContext(ctx, `SELECT message FROM alerts WHERE severity='warn' AND source_id='system' AND title LIKE 'The settings file has problems%' ORDER BY sequence DESC LIMIT 1`).Scan(&message); err != nil || !strings.Contains(message, "has problems at: the file as a whole, such as its owner and mode.") {
		t.Fatalf("warning %q, %v", message, err)
	}
	if view := f.apply(); len(view.Problems) != 1 || !strings.Contains(view.Problems[0].Problem, "can be changed by users other than root") {
		t.Fatalf("apply of a file anyone can change: %+v", view.Problems)
	}
}

func TestTheFirstStartRefusesABrokenFile(t *testing.T) {
	t.Parallel()
	f := newSettingsFileFixture(t, replaced(t, exampleSettingsYAML, "  reasoning_effort: high\n  internet_access", "  internet_access"))
	_, err := f.a.LoadSettingsFileAtStart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "has problems, and MMHotline has never applied a settings file, so it cannot start:\ntriage_agent.reasoning_effort: required") {
		t.Fatalf("first start: %v", err)
	}
}

// TestAnAddressServeCannotOpenUsesTheEarlierOne: serve listens with the
// server settings it last applied, and the server part says why.
func TestAnAddressServeCannotOpenUsesTheEarlierOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	f.write(replaced(t, exampleSettingsYAML, "alert_intake_listen_address: 10.20.0.1:8484", "alert_intake_listen_address: 10.99.0.1:8484"))
	f.restart()
	if server, err := f.a.LoadSettingsFileAtStart(ctx); err != nil || server.AlertIntakeListenAddress != "10.99.0.1:8484" {
		t.Fatalf("server %+v, %v", server, err)
	}
	reason := "MMHotline cannot listen on alert_intake_listen_address 10.99.0.1:8484: cannot assign requested address"
	server, found, err := f.a.UseLastAppliedServerSettings(ctx, reason)
	if err != nil || !found || server.AlertIntakeListenAddress != "10.20.0.1:8484" {
		t.Fatalf("fallback %+v, %t, %v", server, found, err)
	}
	if err := f.a.ApplySettingsFileAtStart(ctx, func(err error) { t.Error(err) }); err != nil {
		t.Fatal(err)
	}
	f.a.WaitForSettingsFileAppliedAtStart()
	shown, err := f.a.SettingsFileState(ctx)
	if err != nil || states(shown)[settingsServerPart.name] != "failed: "+reason {
		t.Fatalf("server part %v, %v", states(shown), err)
	}
	if view := f.apply(); !slices.Equal(view.RestartNeededFor, []string{"alert_intake_listen_address"}) {
		t.Fatalf("apply after the fallback %+v", view)
	}
}
