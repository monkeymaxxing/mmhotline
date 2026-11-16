package app

import (
	"encoding/json"
	"slices"
	"testing"
	"unicode/utf8"
)

// FuzzCustomScheduleSandboxOutput reads fuzzed standard output of the custom
// schedule sandbox child, which runs the team's code and is never trusted.
// Output that breaks the protocol is an error; a mistake in the code is a
// problem of a known kind, with a line from 0 to 1,000,000, a message of at
// most 500 characters, and the asked time in the schedule's time zone;
// either way no answer is used. Accepted output answers every asked time and
// every test case, and names only team members' sign-in email addresses or
// nobody (docs/changing-mmhotline/design/scheduling.md#custom-schedule-code).
func FuzzCustomScheduleSandboxOutput(f *testing.F) {
	request := customScheduleSandboxRequest{TimeZone: "America/New_York", TeamMemberSignInEmailAddresses: []string{"barbara@example.com", "carlos@example.com", "dana@example.com"},
		AskedTimes: []string{"2026-10-05T09:30:00-04:00", "2026-10-10T09:30:00-04:00"}, TestCases: []customScheduleSandboxTestCase{{AskedTime: "2026-10-11T12:00:00-04:00"}}, MaxStepsPerAnswer: customScheduleMaxStepsPerAnswer}
	for _, code := range []string{
		weekdayWeekendCode,
		"def on_call(at):\n    return {\"primary_sign_in_email_address\": None}\n",
		"def on_call(at):\n    fail(\"no schedule for %s\" % at.date)\n",
		"def on_call(at)\n    return {}\n",
		"def on_call(at):\n    return {\"primary_sign_in_email_address\": \"erin@example.com\"}\n",
	} {
		request := request
		request.Code = code
		output, err := json.Marshal(evaluateCustomScheduleCode(request, customScheduleTimeZones(request.TimeZone, code)))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(output)
	}
	f.Add([]byte(`{"answers":[{"primary_sign_in_email_address":"barbara@example.com"}],"test_case_answers":[]} {}`))
	f.Fuzz(func(t *testing.T, output []byte) {
		response, problem, err := readCustomScheduleSandboxResponse(request, output)
		empty := response.Answers == nil && response.TestCaseAnswers == nil && response.Error == nil
		switch {
		case err != nil:
			if !empty || problem != nil {
				t.Fatalf("a protocol error gave %+v and %+v", response, problem)
			}
		case problem != nil:
			kinds := []string{customScheduleErrorSyntax, customScheduleErrorRuntime, customScheduleErrorTooMuchWork, customScheduleErrorBadResult, customScheduleErrorUnknownSignInEmailAddress}
			if !empty || !slices.Contains(kinds, problem.Kind) || problem.Line < 0 || problem.Line > 1_000_000 || utf8.RuneCountInString(problem.Message) > 500 ||
				!problem.AskedAt.IsZero() && problem.AskedAt.Location().String() != request.TimeZone {
				t.Fatalf("the problem %+v with %+v", problem, response)
			}
		default:
			if len(response.Answers) != len(request.AskedTimes) || len(response.TestCaseAnswers) != len(request.TestCases) || response.Error != nil {
				t.Fatalf("accepted %+v", response)
			}
			for _, answer := range append(slices.Clone(response.Answers), response.TestCaseAnswers...) {
				for _, address := range []*string{answer.PrimarySignInEmailAddress, answer.BackupSignInEmailAddress} {
					if address != nil && !slices.Contains(request.TeamMemberSignInEmailAddresses, *address) {
						t.Fatalf("accepted an answer naming %q", *address)
					}
				}
			}
		}
	})
}
