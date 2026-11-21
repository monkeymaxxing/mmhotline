package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/triageagent"
)

// withoutCloseIdleConnections is a RoundTripper that is not a transport with
// CloseIdleConnections, so httptest.Server.Close leaves it alone.
type withoutCloseIdleConnections struct{ http.RoundTripper }

// The test binary stands in for the mmhotline binary: the sandbox runner starts
// os.Executable() with the hidden subcommand, and a fake coding agent runs
// mmhotline triage-agent request-playbook-run or report.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "triage-agent" && triageagent.IsAgentRequestCommand(os.Args[2]) {
		os.Exit(triageagent.RunAgentRequestCommand(os.Args[2], os.Args[3:], os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 3 && os.Args[1] == "triage-agent" && os.Args[2] == triageagent.SSHProxyAction {
		os.Exit(triageagent.RunSSHProxyCommand(os.Args[3:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) == 2 && os.Args[1] == CustomScheduleSandboxCommand {
		os.Exit(RunCustomScheduleSandbox(os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) == 3 && os.Args[1] == "custom-schedule-sandbox-lockdown-probe" {
		os.Exit(customScheduleLockdownProbe(os.Args[2]))
	}
	// One empty home for the whole run, so Git, ssh-keygen, and the other
	// programs the tests start never read the developer's own ~/.gitconfig or
	// ~/.ssh. A per-test t.Setenv would keep every test that starts them from
	// running in parallel. Nothing under test writes to it, and no test reads
	// it back: the fake sandbox and MMX get their own HOME.
	home, err := os.MkdirTemp("", "mmhotline-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	// httptest.Server.Close calls CloseIdleConnections on http.DefaultTransport
	// when it has that method. Parallel tests share the transport, so one
	// test's server closing would break another test's request in flight to its
	// own server ("transport connection broken"). Without the method, each
	// server still closes its own connections.
	http.DefaultTransport = withoutCloseIdleConnections{http.DefaultTransport}
	code := m.Run()
	removeTestSigningFiles()
	os.RemoveAll(home)
	os.Exit(code)
}

type customScheduleTeam struct {
	app           *App
	admin, member Principal
	// Member IDs and sign-in email addresses: Barbara is the administrator.
	barbara, carlos, dana                                    string
	barbaraSignInEmailAddress, carlosInputSignInEmailAddress string
	danaInputSignInEmailAddress                              string
	clock                                                    *time.Time
	carlosSignInEmailAddress, danaSignInEmailAddress         string
}

// newCustomScheduleTeam adds Barbara, Carlos, and Dana as members
// with sign-in email addresses. The clock starts Monday 2026-10-05 07:00 UTC.
func newCustomScheduleTeam(t *testing.T) *customScheduleTeam {
	t.Helper()
	a := foundationApp(t)
	clock := time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return clock }
	admin := memberFixture(t, a, "Barbara@Example.com").Principal
	h := &customScheduleTeam{app: a, admin: admin, clock: &clock, barbaraSignInEmailAddress: "barbara@example.com", carlosInputSignInEmailAddress: "carlos@example.com", danaInputSignInEmailAddress: "dana@example.com"}
	h.barbara = memberIDForUser(t, a, admin)
	h.carlos, h.carlosSignInEmailAddress = addTeamMember(t, a, h.carlosInputSignInEmailAddress, "Carlos", RoleMember)
	h.dana, h.danaSignInEmailAddress = addTeamMember(t, a, h.danaInputSignInEmailAddress, "Dana", RoleMember)
	h.member = Principal{SignInEmailAddress: h.carlosSignInEmailAddress}
	return h
}

func memberIDForUser(t *testing.T, a *App, p Principal) string {
	t.Helper()
	m, err := a.CurrentMember(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// addTeamMember creates a member through the shared people operation.
func addTeamMember(t *testing.T, a *App, signInEmailAddress, name, role string) (string, string) {
	t.Helper()
	member := addPerson(t, a, signInEmailAddress, name, role)
	return member.ID, member.SignInEmailAddress
}

func (h *customScheduleTeam) advance(d time.Duration) { *h.clock = h.clock.Add(d) }

func (h *customScheduleTeam) coverage(t *testing.T) Coverage {
	t.Helper()
	c, err := h.app.CurrentCoverage(context.Background(), h.admin)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *customScheduleTeam) revision(t *testing.T) int64 {
	t.Helper()
	state, err := h.app.Schedules(context.Background(), h.admin)
	if err != nil {
		t.Fatal(err)
	}
	return state.Revision
}

func (h *customScheduleTeam) versions(t *testing.T) []CustomScheduleCodeVersion {
	t.Helper()
	var versions []CustomScheduleCodeVersion
	if err := h.app.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		versions, err = ReadCustomScheduleCodeVersions(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return versions
}

func (h *customScheduleTeam) background(t *testing.T) {
	t.Helper()
	if err := h.app.CustomScheduleBackgroundRound(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *customScheduleTeam) alertTitles(t *testing.T) []string {
	t.Helper()
	var titles []string
	if err := h.app.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT title FROM alerts ORDER BY rowid`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var title string
			if err = rows.Scan(&title); err != nil {
				return err
			}
			titles = append(titles, title)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return titles
}

func customConfig(code string) ScheduleConfig {
	return ScheduleConfig{Kind: customScheduleKind, TimeZone: "America/New_York", BackupKind: "everyone_available", CustomScheduleCode: code}
}

// Carlos is on call on weekdays, Dana on weekends; Barbara is the backup.
const weekdayWeekendCode = `
def on_call(at):
    if at.weekday in ["saturday", "sunday"]:
        return {"primary_sign_in_email_address": "dana@example.com", "backup_sign_in_email_address": "barbara@example.com"}
    return {"primary_sign_in_email_address": "Carlos@Example.com ", "backup_sign_in_email_address": "barbara@example.com"}
`

func evaluate(t *testing.T, code string, askedTimes ...string) customScheduleSandboxResponse {
	t.Helper()
	request := customScheduleSandboxRequest{Code: code, TimeZone: "America/New_York", TeamMemberSignInEmailAddresses: []string{"barbara@example.com", "carlos@example.com"}, AskedTimes: askedTimes, MaxStepsPerAnswer: customScheduleMaxStepsPerAnswer}
	return evaluateCustomScheduleCode(request, customScheduleTimeZones(request.TimeZone, code))
}

func signInEmailAddressOf(answer customScheduleAnswer) string {
	if answer.PrimarySignInEmailAddress == nil {
		return "None"
	}
	return *answer.PrimarySignInEmailAddress
}

func TestCustomScheduleCodeFieldsAndHelpers(t *testing.T) {
	t.Parallel()
	code := `
def on_call(at):
    other = at.in_time_zone("Europe/Berlin")
    fields = [at.year, at.month, at.day, at.hour, at.minute, at.weekday, at.date, at.time_of_day, at.time_zone, other.time_zone, other.hour, other.date]
    expected = [2026, 10, 5, 9, 30, "monday", "2026-10-05", "09:30", "America/New_York", "Europe/Berlin", 15, "2026-10-05"]
    if fields != expected:
        fail("fields: %s" % fields)
    if days_since("2026-10-01", at) != 4 or days_since("2026-10-05 09:31", at) != -1 or days_since("2026-10-05 09:30", at) != 0:
        fail("days_since")
    if weeks_since("2026-09-28 09:00", at) != 1 or weeks_since("2026-10-12", at) != -1 or weeks_since("2026-10-05 09:31", at) != -1:
        fail("weeks_since")
    print("print output is dropped")
    return {"primary_sign_in_email_address": "barbara@example.com"}
`
	response := evaluate(t, code, "2026-10-05T09:30:00-04:00")
	if response.Error != nil || len(response.Answers) != 1 || signInEmailAddressOf(response.Answers[0]) != "barbara@example.com" || response.Answers[0].BackupSignInEmailAddress != nil {
		t.Fatalf("%+v %+v", response, response.Error)
	}
	// Wall-clock days: a 09:00 handoff stays at 09:00 across the November
	// daylight saving change in New York (2026-11-01).
	response = evaluate(t, `
def on_call(at):
    if days_since("2026-10-31 09:00", at) != 1:
        fail("across DST")
    return {"primary_sign_in_email_address": None}
`, "2026-11-01T09:00:00-05:00")
	if response.Error != nil || response.Answers[0].PrimarySignInEmailAddress != nil {
		t.Fatalf("dst days: %+v", response.Error)
	}
}

func TestCustomScheduleCodeRestrictionsAndResults(t *testing.T) {
	t.Parallel()
	at := "2026-10-05T09:30:00-04:00"
	for _, tc := range []struct {
		name, code, kind, message string
		line                      int
	}{
		{"load", "load(\"x.star\", \"y\")\ndef on_call(at):\n    return {\"primary_sign_in_email_address\": None}\n", customScheduleErrorRuntime, "load", 1},
		{"while", "def on_call(at):\n    while True:\n        pass\n", customScheduleErrorSyntax, "while", 2},
		{"recursion", "def f(n):\n    return f(n)\ndef on_call(at):\n    return f(1)\n", customScheduleErrorRuntime, "called recursively", 2},
		{"no now", "def on_call(at):\n    return {\"primary_sign_in_email_address\": now()}\n", customScheduleErrorSyntax, "undefined: now", 2},
		{"no on_call", "x = 1\n", customScheduleErrorRuntime, "on_call(at)", 0},
		{"not a dictionary", "def on_call(at):\n    return \"barbara@example.com\"\n", customScheduleErrorBadResult, customScheduleNotADictionaryMessage, 0},
		{"missing primary", "def on_call(at):\n    return {\"backup_sign_in_email_address\": None}\n", customScheduleErrorBadResult, customScheduleNotADictionaryMessage, 0},
		{"unknown key", "def on_call(at):\n    return {\"primary_sign_in_email_address\": None, \"primary\": 1}\n", customScheduleErrorBadResult, "Unknown key primary in the result. Use primary_sign_in_email_address and backup_sign_in_email_address.", 0},
		{"unknown sign-in email address", "def on_call(at):\n    return {\"primary_sign_in_email_address\": \"Mallory@Example.com\"}\n", customScheduleErrorUnknownSignInEmailAddress, "mallory@example.com", 0},
		{"too much work", "def on_call(at):\n    x = 0\n    for i in range(1000000):\n        x += i\n    return {\"primary_sign_in_email_address\": None}\n", customScheduleErrorTooMuchWork, "too many steps", 0},
		{"too much work loading", "def work():\n    x = 0\n    for i in range(1000000):\n        x += i\n    return x\nX = work()\ndef on_call(at):\n    return {\"primary_sign_in_email_address\": None}\n", customScheduleErrorTooMuchWork, "loading", 0},
		{"unknown timezone", "def on_call(at):\n    zone = \"Europe/\" + \"Paris\"\n    at.in_time_zone(zone)\n    return {\"primary_sign_in_email_address\": None}\n", customScheduleErrorRuntime, "unknown timezone", 3},
		{"no randomness module", "def on_call(at):\n    return {\"primary_sign_in_email_address\": random.choice([])}\n", customScheduleErrorSyntax, "undefined: random", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := evaluate(t, tc.code, at)
			e := response.Error
			if e == nil || e.Kind != tc.kind || !strings.Contains(e.Message, tc.message) || e.Line != tc.line || len(response.Answers) != 0 {
				t.Fatalf("%+v", e)
			}
		})
	}
	// Frozen globals: on_call cannot keep state between answers.
	response := evaluate(t, "SEEN = []\ndef on_call(at):\n    SEEN.append(1)\n    return {\"primary_sign_in_email_address\": None}\n", at)
	if response.Error == nil || !strings.Contains(response.Error.Message, "frozen") {
		t.Fatalf("globals were mutable: %+v", response.Error)
	}
	problem := &CustomScheduleCodeProblem{Kind: customScheduleErrorTooMuchWork, AskedAt: time.Date(2026, 10, 5, 9, 30, 0, 0, time.FixedZone("EDT", -4*3600))}
	if problem.Error() != "Your code did too much work for Mon 2026-10-05 09:30 EDT. Make it simpler." {
		t.Fatal(problem.Error())
	}
	problem = &CustomScheduleCodeProblem{Kind: customScheduleErrorSyntax, Line: 7, Message: "got newline, want primary expression"}
	if problem.Error() != "Your code has a mistake on line 7: got newline, want primary expression" {
		t.Fatal(problem.Error())
	}
	problem = &CustomScheduleCodeProblem{Kind: customScheduleErrorUnknownSignInEmailAddress, Message: "mallory@example.com"}
	if problem.Error() != "on_call returned a sign-in email address that is not a team member's sign-in email address: mallory@example.com" {
		t.Fatal(problem.Error())
	}
}

func TestCustomScheduleSandboxResponseIsNeverTrusted(t *testing.T) {
	t.Parallel()
	carlos := "carlos@example.com"
	mallory := "mallory@example.com"
	request := customScheduleSandboxRequest{TimeZone: "UTC", TeamMemberSignInEmailAddresses: []string{carlos}, AskedTimes: []string{"2026-10-05T09:30:00Z"}}
	for name, response := range map[string]customScheduleSandboxResponse{
		"unknown sign-in email address":        {Answers: []customScheduleAnswer{{PrimarySignInEmailAddress: &mallory}}},
		"unknown backup Sign-in email address": {Answers: []customScheduleAnswer{{PrimarySignInEmailAddress: &carlos, BackupSignInEmailAddress: &mallory}}},
		"too few answers":                      {},
		"answers and an error":                 {Answers: []customScheduleAnswer{{}}, Error: &customScheduleCodeError{Kind: customScheduleErrorRuntime}},
		"unknown error kind":                   {Error: &customScheduleCodeError{Kind: "shell"}},
		"invalid error time":                   {Error: &customScheduleCodeError{Kind: customScheduleErrorRuntime, AskedTime: "yesterday"}},
		"negative line":                        {Error: &customScheduleCodeError{Kind: customScheduleErrorRuntime, Line: -1}},
		"test answers uncalled":                {Answers: []customScheduleAnswer{{}}, TestCaseAnswers: []customScheduleAnswer{{}}},
	} {
		if _, err := validateCustomScheduleSandboxResponse(request, response); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	problem, err := validateCustomScheduleSandboxResponse(request, customScheduleSandboxResponse{Answers: []customScheduleAnswer{{PrimarySignInEmailAddress: &carlos}}})
	if err != nil || problem != nil {
		t.Fatal(problem, err)
	}
}

func TestCustomScheduleRefusesControlAndTextDirectionCharacters(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	for name, character := range map[string]string{"carriage return": "\r", "escape": "\x1b[2K", "C1 control": "\u0085", "right-to-left override": "\u202e", "isolate": "\u2066", "Arabic letter mark": "\u061c"} {
		code := "def on_call(at):\n    # note" + character + "\n    return {\"primary_sign_in_email_address\": \"barbara@example.com\"}\n"
		_, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, customConfig(code))
		if !errors.Is(err, ErrInvalid) || publicError(err) != "line 2 has a control or text-direction character; remove it" {
			t.Errorf("%s: %v", name, err)
		}
	}
	tabs := "def on_call(at):\n\treturn {\"primary_sign_in_email_address\": \"barbara@example.com\"}  # café ✓\n"
	if preview, err := h.app.PreviewCustomScheduleCode(context.Background(), h.admin, customConfig(tabs)); err != nil || preview.Problem != nil {
		t.Fatalf("tabs and ordinary text: %v %+v", err, preview.Problem)
	}
}

func TestCustomScheduleSandboxSlotsBoundConcurrencyAndHonorCancellation(t *testing.T) {
	t.Parallel()
	slots := newCustomScheduleSandboxSlots()
	ctx := context.Background()
	first, err := slots.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := slots.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := slots.acquire(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waiting, stopWaiting := context.WithTimeout(ctx, 25*time.Millisecond)
	defer stopWaiting()
	if release, err := slots.acquire(waiting); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("third concurrent run: %v", err)
	}
	first()
	third, err := slots.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second()
	third()
	if len(slots.slots) != 0 {
		t.Fatalf("leaked %d slots", len(slots.slots))
	}
}

func (h *customScheduleTeam) save(t *testing.T, c ScheduleConfig) CustomScheduleSaveResult {
	t.Helper()
	result, err := h.app.SaveCustomScheduleCode(context.Background(), h.admin, c, h.revision(t))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCustomScheduleHourlyRunsFailuresMemberRemovalAndRunningOut(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	ctx := context.Background()
	config := customConfig(weekdayWeekendCode)
	config.BackupKind = "fixed_person"
	config.BackupMemberID = h.barbara
	h.save(t, customConfig(weekdayWeekendCode))
	config.CustomScheduleCode = strings.ReplaceAll(weekdayWeekendCode, `, "backup_sign_in_email_address": "barbara@example.com"`, "")
	h.save(t, config)
	var until int64
	readUntil := func() {
		if err := h.app.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT shifts_until FROM custom_schedule_timeline_state`).Scan(&until)
		}); err != nil {
			t.Fatal(err)
		}
	}
	readUntil()
	firstUntil := until
	// Nothing is due within the hour; after an hour the shifts move ahead.
	h.advance(30 * time.Minute)
	h.background(t)
	if readUntil(); until != firstUntil {
		t.Fatal("ran before the hour")
	}
	h.advance(30 * time.Minute)
	h.background(t)
	if readUntil(); until != firstUntil+3600 {
		t.Fatalf("hourly run: %d -> %d", firstUntil, until)
	}
	// Dana is removed from the team: the code now names a sign-in email address that is
	// not on the team, so the run fails right away, the stored shifts stay,
	// and administrators are told once.
	if err := removePerson(t, h.app, h.danaInputSignInEmailAddress); err != nil {
		t.Fatal(err)
	}
	h.background(t)
	h.background(t)
	failed := 0
	for _, title := range h.alertTitles(t) {
		if strings.HasSuffix(title, "the custom schedule code failed") || strings.HasSuffix(title, "the custom schedule code still fails") {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failure notices: %v", h.alertTitles(t))
	}
	if readUntil(); until != firstUntil+3600 {
		t.Fatal("a failed run changed the stored shifts")
	}
	// On the weekend the stored shift names Dana, who is gone: the fixed
	// backup is contacted first.
	*h.clock = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if live := h.coverage(t); live.AssignedPrimaryID != h.dana || !reflect.DeepEqual(live.PrimaryIDs, []string{h.barbara}) {
		t.Fatalf("removed member's shift: %+v", live)
	}
	// Hourly failures with fewer than 24 hours of shifts left: one more.
	*h.clock = time.Unix(until, 0).Add(-20 * time.Hour)
	h.background(t)
	h.advance(time.Hour)
	h.background(t)
	failed = 0
	for _, title := range h.alertTitles(t) {
		if strings.HasSuffix(title, "the custom schedule code failed") || strings.HasSuffix(title, "the custom schedule code still fails") {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("second failure notice: %v", h.alertTitles(t))
	}
	// The stored shifts run out: No one scheduled, with the reason.
	*h.clock = time.Unix(until, 0).Add(time.Minute)
	live := h.coverage(t)
	if live.AssignedPrimaryID != "" || !reflect.DeepEqual(live.PrimaryIDs, []string{h.barbara}) || live.Reason != customScheduleNoAnswerReason {
		t.Fatalf("ran out: %+v", live)
	}
	// Dana is back: the next run works again and says so.
	addPerson(t, h.app, h.danaInputSignInEmailAddress, "Dana", RoleMember)
	h.background(t)
	titles := h.alertTitles(t)
	if titles[len(titles)-1] != "The custom schedule code works again: MMHotline knows who is on call" || h.coverage(t).Reason == customScheduleNoAnswerReason {
		t.Fatalf("works again: %v %+v", titles, h.coverage(t))
	}
}

// Saving code that works ends an earlier failure, so the page no longer
// shows it and the next failure is announced again.

func TestCustomScheduleSavedCodeEndsAnEarlierFailure(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	ctx := context.Background()
	h.save(t, customConfig(weekdayWeekendCode))
	failRun := func() {
		t.Helper()
		state, _ := h.app.Schedules(ctx, h.admin)
		for _, c := range state.Configs {
			if c.Kind == customScheduleKind {
				if err := h.app.commitCustomScheduleRun(ctx, c, "", CustomSchedulePreview{}, errors.New("sandbox did not start"), h.app.Now()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	failures := func() int {
		n := 0
		for _, title := range h.alertTitles(t) {
			if strings.HasSuffix(title, "the custom schedule code failed") || strings.HasSuffix(title, "the custom schedule code still fails") {
				n++
			}
		}
		return n
	}
	failRun()
	h.save(t, customConfig(weekdayWeekendCode+"# fixed\n"))
	var failingSince int64
	var message string
	if err := h.app.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT failing_since,last_failure_message FROM custom_schedule_timeline_state`).Scan(&failingSince, &message)
	}); err != nil || failingSince != 0 || message != "" {
		t.Fatalf("after a working save: %d %q %v", failingSince, message, err)
	}
	failRun()
	if n := failures(); n != 2 {
		t.Fatalf("failure notices: %v", h.alertTitles(t))
	}
}

func TestCustomScheduleStaleRunIsDiscarded(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	ctx := context.Background()
	h.save(t, customConfig(weekdayWeekendCode))
	state, _ := h.app.Schedules(ctx, h.admin)
	var started ScheduleConfig
	for _, c := range state.Configs {
		if c.Kind == customScheduleKind {
			started = c
		}
	}
	signInEmailAddresses, _ := h.app.customScheduleMemberSignInEmailAddresses(ctx)
	preview, err := h.app.workOutCustomSchedule(ctx, started, signInEmailAddresses, h.app.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Meanwhile the administrator saves a new configuration.
	onlyBarbara := customConfig("def on_call(at):\n    return {\"primary_sign_in_email_address\": \"barbara@example.com\"}\n")
	h.save(t, onlyBarbara)
	if err = h.app.commitCustomScheduleRun(ctx, started, customScheduleMemberSignInEmailAddressesSHA256(signInEmailAddresses), preview, nil, h.app.Now()); err != nil {
		t.Fatal(err)
	}
	if got := h.coverage(t).AssignedPrimaryID; got != h.barbara {
		t.Fatalf("stale run overwrote the new shifts: %s", got)
	}
}

func TestCustomScheduleSaveActivatesAndCoverageReadsStoredShifts(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	ctx := context.Background()
	config := customConfig(weekdayWeekendCode)
	config.CustomScheduleTestCases = []CustomScheduleTestCase{{When: "2026-10-10T12:00", ExpectedPrimaryMemberID: h.dana, ExpectedBackupMemberID: h.barbara}}
	if usesCode, err := h.app.usesCustomScheduleCode(ctx); err != nil || usesCode {
		t.Fatalf("the custom schedule loop has work for a team without custom schedule code: %v %v", usesCode, err)
	}
	result := h.save(t, config)
	if result.VersionID == "" {
		t.Fatal("saved version has no ID")
	}
	if usesCode, err := h.app.usesCustomScheduleCode(ctx); err != nil || !usesCode {
		t.Fatalf("the custom schedule loop does not find the team with an active custom schedule: %v %v", usesCode, err)
	}
	version := h.versions(t)[0]
	if version.ID != result.VersionID || version.Config.CustomScheduleCode != config.CustomScheduleCode || version.Config.Revision != h.revision(t) || version.CreatedByMemberID != h.barbara || version.CreatedAt.IsZero() {
		t.Fatalf("version record: %+v", version)
	}
	live := h.coverage(t)
	if live.ScheduleKind != customScheduleKind || live.AssignedPrimaryID != h.carlos || !reflect.DeepEqual(live.PrimaryIDs, []string{h.carlos}) || !reflect.DeepEqual(live.BackupIDs, []string{h.barbara}) {
		t.Fatalf("coverage: %+v", live)
	}
	// The handoff notice reads the stored shifts: Saturday 00:00 New York (04:00 UTC).
	if err := h.app.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	*h.clock = time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	if err := h.app.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.coverage(t).AssignedPrimaryID; got != h.dana {
		t.Fatalf("weekend primary %s", got)
	}
	if n := scalarTeam(t, h.app, `SELECT count(*) FROM alerts WHERE title='Dana is on call now, by the schedule' AND message LIKE 'Scheduled primary is now Dana%'`); n != 1 {
		t.Fatalf("handoff notices: %d", n)
	}
	// Editing code and timezone activates the freshly validated configuration.
	swapped := customConfig(strings.NewReplacer("dana@example.com", "carlos@example.com", "Carlos@Example.com", "dana@example.com").Replace(weekdayWeekendCode))
	swapped.TimeZone = "UTC"
	h.save(t, swapped)
	*h.clock = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if got := h.coverage(t).AssignedPrimaryID; got != h.carlos {
		t.Fatalf("weekend primary after swap %s", got)
	}
	// A failing test case stops the save; the schedule stays as it was.
	broken := swapped
	broken.CustomScheduleTestCases = []CustomScheduleTestCase{{When: "2026-10-10T12:00", ExpectedPrimaryMemberID: h.dana}}
	before := h.revision(t)
	if _, err := h.app.SaveCustomScheduleCode(ctx, h.admin, broken, before); !errors.Is(err, ErrInvalid) || publicError(err) != "The test case for 2026-10-10T12:00 expects the primary dana@example.com, and the code gives carlos@example.com. Change the code or the test case, then preview again." {
		t.Fatalf("failed test case saved: %v", err)
	}
	broken = customConfig("def on_call(at):\n    return {\"primary_sign_in_email_address\": \"unknown@example.com\"}\n")
	if _, err := h.app.SaveCustomScheduleCode(ctx, h.admin, broken, before); !errors.Is(err, ErrInvalid) {
		t.Fatalf("failed preview saved: %v", err)
	}
	if len(h.versions(t)) != 2 || h.coverage(t).AssignedPrimaryID != h.carlos {
		t.Fatal("failed saves changed the versions or live coverage")
	}
	if _, err := h.app.SaveCustomScheduleCode(ctx, h.member, swapped, before); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member saved: %v", err)
	}
	if _, err := h.app.SaveCustomScheduleCode(ctx, h.admin, swapped, before-1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if h.revision(t) != before {
		t.Fatal("refused saves changed the schedule")
	}
}

func TestCustomScheduleVersionHistoryKeepsLast20(t *testing.T) {
	t.Parallel()
	h := newCustomScheduleTeam(t)
	first := h.save(t, customConfig(weekdayWeekendCode))
	onlyBarbara := customConfig("def on_call(at):\n    return {\"primary_sign_in_email_address\": \"barbara@example.com\"}\n")
	latest := h.save(t, onlyBarbara)
	// More than 20 successful saves still activate, and keep only the latest 20.
	for range customScheduleVersionsKept - 1 {
		latest = h.save(t, onlyBarbara)
	}
	versions := h.versions(t)
	if len(versions) != customScheduleVersionsKept || versions[0].ID != latest.VersionID {
		t.Fatalf("history: %+v", versions)
	}
	for _, version := range versions {
		if version.ID == first.VersionID {
			t.Fatal("oldest version was not pruned")
		}
	}
	if h.coverage(t).AssignedPrimaryID != h.barbara {
		t.Fatal("latest save is not active")
	}
}
