package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/app"
)

// Each read-only command sends its flags to the service as one request, with
// times in UTC, and refuses a missing ID or a time it cannot read before it
// connects.
func TestReadOnlyCommandFlags(t *testing.T) {
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
		json.NewEncoder(w).Encode(app.CommandResult{PeopleCommandResult: app.PeopleCommandResult{Message: "Listed 0 alerts, newest first."}})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	at := func(value string) time.Time {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed.UTC()
	}
	for _, test := range []struct {
		args []string
		want app.CommandRequest
	}{
		{[]string{"incidents", "list"}, app.CommandRequest{}},
		{[]string{"incidents", "list", "--opened-since", "2026-10-28T09:00:00+02:00", "--opened-before", "2026-10-28T12:00:00Z"}, app.CommandRequest{Incidents: app.IncidentsCommandRequest{OpenedSince: at("2026-10-28T07:00:00Z"), OpenedBefore: at("2026-10-28T12:00:00Z")}}},
		{[]string{"incidents", "show", "--incident-id", "incident-1"}, app.CommandRequest{Incidents: app.IncidentsCommandRequest{IncidentID: "incident-1"}}},
		{[]string{"alerts", "list", "--severity", "warn", "--alert-source-id", "source-1", "--incident-id", "incident-1", "--received-since", "2026-10-28T09:00:00Z", "--received-before", "2026-10-28T10:00:00Z", "--older-than-alert-id", "alert-1"}, app.CommandRequest{Alerts: app.AlertsCommandRequest{Severity: "warn", AlertSourceID: "source-1", IncidentID: "incident-1", ReceivedSince: at("2026-10-28T09:00:00Z"), ReceivedBefore: at("2026-10-28T10:00:00Z"), OlderThanAlertID: "alert-1"}}},
		{[]string{"triage-sessions", "list"}, app.CommandRequest{}},
		{[]string{"triage-sessions", "show", "--triage-session-id", "session-1"}, app.CommandRequest{TriageSessions: app.TriageSessionsCommandRequest{TriageSessionID: "session-1"}}},
		{[]string{"approval-requests", "list"}, app.CommandRequest{}},
		{[]string{"approval-requests", "show", "--approval-request-id", "request-1"}, app.CommandRequest{ApprovalRequests: app.ApprovalRequestsCommandRequest{ApprovalRequestID: "request-1"}}},
		{[]string{"commit-reviews", "list"}, app.CommandRequest{}},
		{[]string{"commit-reviews", "show", "--commit-id", "commit-1"}, app.CommandRequest{CommitReviews: app.CommitReviewsCommandRequest{CommitID: "commit-1"}}},
		{[]string{"commit-reviews", "check", "--commit-id", "commit-1"}, app.CommandRequest{CommitReviews: app.CommitReviewsCommandRequest{CommitID: "commit-1"}}},
		{[]string{"commit-reviews", "mark-reviewed", "--commit-id", "commit-1"}, app.CommandRequest{CommitReviews: app.CommitReviewsCommandRequest{CommitID: "commit-1"}}},
		{[]string{"commit-reviews", "trust-through", "--commit-id", "commit-1"}, app.CommandRequest{CommitReviews: app.CommitReviewsCommandRequest{CommitID: "commit-1"}}},
	} {
		output, err := cliProcess(t, append(test.args, "--command-socket", socket)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", test.args, output, err)
		}
		request := <-requests
		test.want.Group, test.want.Action, test.want.UnixUser = test.args[0], test.args[1], request.UnixUser
		if !reflect.DeepEqual(request, test.want) {
			t.Errorf("%v sent %+v; want %+v", test.args, request, test.want)
		}
	}
	for _, test := range []struct {
		args    []string
		message string
	}{
		{[]string{"incidents", "show"}, "--incident-id is required"},
		{[]string{"triage-sessions", "show"}, "--triage-session-id is required"},
		{[]string{"approval-requests", "show"}, "--approval-request-id is required"},
		{[]string{"commit-reviews", "show"}, "--commit-id is required"},
		{[]string{"commit-reviews", "check"}, "--commit-id is required"},
		{[]string{"commit-reviews", "mark-reviewed"}, "--commit-id is required"},
		{[]string{"commit-reviews", "trust-through"}, "--commit-id is required"},
		{[]string{"alerts", "list", "--received-since", "yesterday"}, "use an RFC 3339 time, such as 2026-10-28T09:00:00Z"},
		{[]string{"incidents", "list", "--opened-since", "2026-10-28"}, "use an RFC 3339 time, such as 2026-10-28T09:00:00Z"},
	} {
		output, err := cliProcess(t, append(test.args, "--command-socket", socket)...).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 4 || !strings.Contains(string(output), test.message) {
			t.Errorf("%v: %s %v", test.args, output, err)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("a refused command reached the service: %+v", request)
	default:
	}
}

// Alert titles, alert messages, and triage agent text come from outsiders.
// The text output must not carry a control character that a terminal acts on,
// and --json keeps its escaped form.
func TestTextOutputReplacesControlCharacters(t *testing.T) {
	socket := filepath.Join(shortSocketDirectory(t), "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	const hostile = "ok\x1b]0;evil\x07\x1b[2J\r\x7f\u009b31m"
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := app.CommandResult{}
		result.Alerts = []app.CommandAlert{{AlertID: "alert-1", Severity: "warn", Title: hostile, Message: "line one\n\tline two " + hostile, PageURL: "https://example.test/alerts/alert-1"}}
		json.NewEncoder(w).Encode(result)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	output, err := cliProcess(t, "alerts", "list", "--command-socket", socket).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v", output, err)
	}
	for _, r := range string(output) {
		if r != '\n' && r != '\t' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) {
			t.Fatalf("text output has control character %U: %q", r, output)
		}
	}
	if !strings.Contains(string(output), "ok") || !strings.Contains(string(output), "line one\n") || !strings.Contains(string(output), "\tline two") {
		t.Errorf("text output lost its text or layout: %q", output)
	}
	output, err = cliProcess(t, "alerts", "list", "--json", "--command-socket", socket).CombinedOutput()
	if err != nil || strings.ContainsRune(string(output), '\x1b') || !strings.Contains(string(output), `\u001b`) {
		t.Errorf("json output changed: %q %v", output, err)
	}
}

// commit-reviews check exits 0 when every commit through the checked commit
// passed or was reviewed, and 1, after printing the answer, when one waits,
// so a scheduled job can stop at the trusted commit.
func TestCommitReviewsCheckExitCode(t *testing.T) {
	socket := filepath.Join(shortSocketDirectory(t), "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	checked, trusted := strings.Repeat("b", 40), strings.Repeat("a", 40)
	answer := make(chan app.CommandCommitCheck, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check := <-answer
		json.NewEncoder(w).Encode(app.CommandResult{CommitCheck: &check})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	args := []string{"commit-reviews", "check", "--commit-id", checked, "--command-socket", socket}

	answer <- app.CommandCommitCheck{CheckedCommitID: checked, TrustedThroughCommitID: checked, CommitsWaitingForReview: []app.CommandCommitReview{}}
	if output, err := cliProcess(t, args...).CombinedOutput(); err != nil || !strings.Contains(string(output), "Trusted through commit "+checked) {
		t.Fatalf("nothing waits: %s %v", output, err)
	}
	answer <- app.CommandCommitCheck{CheckedCommitID: checked, TrustedThroughCommitID: trusted, CommitsWaitingForReview: []app.CommandCommitReview{{CommitID: checked, ReasonKind: "not_signed_by_a_trusted_key", Reason: "which no key the team trusts signed"}}}
	output, err := cliProcess(t, append(args, "--json")...).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), `"trusted_through_commit_id":"`+trusted+`"`) {
		t.Fatalf("a commit waits: %s %v", output, err)
	}
}
