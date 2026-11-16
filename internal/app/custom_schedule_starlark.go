package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// This file runs inside the sandbox child (`mmhotline custom-schedule-sandbox`).
// It evaluates the customer's Starlark on_call(at) for each asked time and
// returns sign-in email addresses exactly as the code returned them, after checking them
// against the team's sign-in email addresses. The parent process validates every answer
// again; nothing here is trusted by the parent.

// The request and response of the sandbox protocol: one JSON request on the
// child's standard input, one JSON response on its standard output.
type customScheduleSandboxRequest struct {
	Code                           string                          `json:"code"`
	TimeZone                       string                          `json:"time_zone"`
	TeamMemberSignInEmailAddresses []string                        `json:"team_member_sign_in_email_addresses"`
	AskedTimes                     []string                        `json:"asked_times"`
	TestCases                      []customScheduleSandboxTestCase `json:"test_cases"`
	MaxStepsPerAnswer              uint64                          `json:"max_steps_per_answer"`
}

// A test case asks about one more moment; the parent compares the answer
// with what the administrator expected.
type customScheduleSandboxTestCase struct {
	AskedTime string `json:"asked_time"`
}

// A nil sign-in email address is Starlark's None: "No one scheduled" for primary_sign_in_email_address, the
// schedule's backup option for backup_sign_in_email_address.
type customScheduleAnswer struct {
	PrimarySignInEmailAddress *string `json:"primary_sign_in_email_address"`
	BackupSignInEmailAddress  *string `json:"backup_sign_in_email_address"`
}

type customScheduleSandboxResponse struct {
	Answers         []customScheduleAnswer   `json:"answers,omitempty"`
	TestCaseAnswers []customScheduleAnswer   `json:"test_case_answers,omitempty"`
	Error           *customScheduleCodeError `json:"error,omitempty"`
}

// Kinds of mistakes in customer code, as the sandbox reports them.
const (
	customScheduleErrorSyntax                    = "syntax"
	customScheduleErrorRuntime                   = "runtime"
	customScheduleErrorTooMuchWork               = "too_much_work"
	customScheduleErrorBadResult                 = "bad_result"
	customScheduleErrorUnknownSignInEmailAddress = "unknown_sign_in_email_address"
)

// For unknown_sign_in_email_address, Message is the sign-in email address itself.
type customScheduleCodeError struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Line      int    `json:"line"`
	AskedTime string `json:"asked_time"`
}

const customScheduleCodeFileName = "schedule code"

const (
	customScheduleNotADictionaryMessage = `on_call must return a dictionary like {"primary_sign_in_email_address": "ana@example.com"}`
)

// evaluateCustomScheduleCode loads the code once and asks on_call(at) about
// every asked time and test case. zones holds every timezone the code may
// use; the sandbox cannot open timezone files once it is locked down.
func evaluateCustomScheduleCode(request customScheduleSandboxRequest, zones map[string]*time.Location) customScheduleSandboxResponse {
	fail := func(e customScheduleCodeError) customScheduleSandboxResponse {
		e.Message = truncateRunes(e.Message, 500)
		return customScheduleSandboxResponse{Error: &e}
	}
	loc := zones[request.TimeZone]
	if loc == nil {
		return fail(customScheduleCodeError{Kind: customScheduleErrorRuntime, Message: "unknown schedule timezone " + request.TimeZone})
	}
	known := map[string]bool{}
	for _, signInEmailAddress := range request.TeamMemberSignInEmailAddresses {
		known[signInEmailAddress] = true
	}
	predeclared := starlark.StringDict{
		"days_since":  starlark.NewBuiltin("days_since", customScheduleDaysSince),
		"weeks_since": starlark.NewBuiltin("weeks_since", customScheduleWeeksSince),
	}
	thread, tooMuchWork := customScheduleThread(request.MaxStepsPerAnswer)
	// Starlark's default file options stay in force: no while loops, no
	// recursion, no top-level control flow. Thread.Load is nil, so load()
	// fails. There is no clock, randomness, file, network, or process access.
	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, customScheduleCodeFileName, request.Code, predeclared)
	if err != nil {
		if *tooMuchWork {
			return fail(customScheduleCodeError{Kind: customScheduleErrorTooMuchWork, Message: "too many steps while loading"})
		}
		return fail(customScheduleStarlarkError(err, ""))
	}
	onCall, ok := globals["on_call"].(*starlark.Function)
	if !ok {
		return fail(customScheduleCodeError{Kind: customScheduleErrorRuntime, Message: "define a function on_call(at)"})
	}
	ask := func(askedTime string) (customScheduleAnswer, *customScheduleCodeError) {
		at, err := time.Parse(time.RFC3339, askedTime)
		if err != nil {
			return customScheduleAnswer{}, &customScheduleCodeError{Kind: customScheduleErrorRuntime, Message: "invalid asked time", AskedTime: askedTime}
		}
		thread, tooMuchWork := customScheduleThread(request.MaxStepsPerAnswer)
		value, err := starlark.Call(thread, onCall, starlark.Tuple{customScheduleMoment{local: at.In(loc), zones: zones}}, nil)
		if err != nil {
			e := customScheduleStarlarkError(err, askedTime)
			if *tooMuchWork {
				e = customScheduleCodeError{Kind: customScheduleErrorTooMuchWork, Message: "too many steps", AskedTime: askedTime}
			}
			return customScheduleAnswer{}, &e
		}
		answer, problem := customScheduleResult(value, known)
		if problem != nil {
			problem.AskedTime = askedTime
		}
		return answer, problem
	}
	var response customScheduleSandboxResponse
	for _, askedTime := range request.AskedTimes {
		answer, problem := ask(askedTime)
		if problem != nil {
			return fail(*problem)
		}
		response.Answers = append(response.Answers, answer)
	}
	for _, test := range request.TestCases {
		answer, problem := ask(test.AskedTime)
		if problem != nil {
			return fail(*problem)
		}
		response.TestCaseAnswers = append(response.TestCaseAnswers, answer)
	}
	return response
}

// customScheduleThread gives each answer, and loading, its own step budget.
func customScheduleThread(maxSteps uint64) (*starlark.Thread, *bool) {
	tooMuchWork := new(bool)
	thread := &starlark.Thread{Name: "on_call", Print: func(*starlark.Thread, string) {}}
	thread.SetMaxExecutionSteps(maxSteps)
	thread.OnMaxSteps = func(t *starlark.Thread) {
		*tooMuchWork = true
		t.Cancel("too many steps")
	}
	return thread, tooMuchWork
}

func customScheduleStarlarkError(err error, askedTime string) customScheduleCodeError {
	var syntaxError syntax.Error
	if errors.As(err, &syntaxError) {
		return customScheduleCodeError{Kind: customScheduleErrorSyntax, Message: syntaxError.Msg, Line: int(syntaxError.Pos.Line)}
	}
	var resolveErrors resolve.ErrorList
	if errors.As(err, &resolveErrors) {
		return customScheduleCodeError{Kind: customScheduleErrorSyntax, Message: resolveErrors[0].Msg, Line: int(resolveErrors[0].Pos.Line)}
	}
	var evalError *starlark.EvalError
	if errors.As(err, &evalError) {
		line := 0
		for i := 0; i < len(evalError.CallStack); i++ {
			frame := evalError.CallStack.At(i)
			if frame.Pos.Filename() == customScheduleCodeFileName {
				line = int(frame.Pos.Line)
				break
			}
		}
		return customScheduleCodeError{Kind: customScheduleErrorRuntime, Message: evalError.Msg, Line: line, AskedTime: askedTime}
	}
	return customScheduleCodeError{Kind: customScheduleErrorRuntime, Message: err.Error(), AskedTime: askedTime}
}

// customScheduleResult accepts only {"primary_sign_in_email_address": ..., "backup_sign_in_email_address": ...}
// with sign-in email addresses on the team, compared case-insensitively after the same
// normalization as sign-in.
func customScheduleResult(value starlark.Value, known map[string]bool) (customScheduleAnswer, *customScheduleCodeError) {
	var answer customScheduleAnswer
	dict, ok := value.(*starlark.Dict)
	if !ok {
		return answer, &customScheduleCodeError{Kind: customScheduleErrorBadResult, Message: customScheduleNotADictionaryMessage}
	}
	hasPrimary := false
	for _, item := range dict.Items() {
		key, isString := starlark.AsString(item[0])
		if !isString {
			key = item[0].String()
		}
		var target **string
		switch {
		case isString && key == "primary_sign_in_email_address":
			target = &answer.PrimarySignInEmailAddress
			hasPrimary = true
		case isString && key == "backup_sign_in_email_address":
			target = &answer.BackupSignInEmailAddress
		default:
			return answer, &customScheduleCodeError{Kind: customScheduleErrorBadResult, Message: fmt.Sprintf("Unknown key %s in the result. Use primary_sign_in_email_address and backup_sign_in_email_address.", truncateRunes(key, 100))}
		}
		if item[1] == starlark.None {
			continue
		}
		signInEmailAddress, isSignInEmailAddress := starlark.AsString(item[1])
		if !isSignInEmailAddress {
			return answer, &customScheduleCodeError{Kind: customScheduleErrorBadResult, Message: customScheduleNotADictionaryMessage}
		}
		signInEmailAddress = strings.ToLower(strings.TrimSpace(signInEmailAddress))
		if !known[signInEmailAddress] {
			return answer, &customScheduleCodeError{Kind: customScheduleErrorUnknownSignInEmailAddress, Message: truncateRunes(signInEmailAddress, 254)}
		}
		*target = &signInEmailAddress
	}
	if !hasPrimary {
		return answer, &customScheduleCodeError{Kind: customScheduleErrorBadResult, Message: customScheduleNotADictionaryMessage}
	}
	return answer, nil
}

// customScheduleMoment is the `at` value: one moment, in local time of one
// timezone. It offers exactly the fields the design lists.
type customScheduleMoment struct {
	local time.Time
	zones map[string]*time.Location
}

var customScheduleWeekdays = [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

func (m customScheduleMoment) String() string {
	return "at(" + m.local.Format("2006-01-02 15:04") + " " + m.local.Location().String() + ")"
}
func (customScheduleMoment) Type() string            { return "at" }
func (customScheduleMoment) Freeze()                 {}
func (customScheduleMoment) Truth() starlark.Bool    { return starlark.True }
func (m customScheduleMoment) Hash() (uint32, error) { return uint32(m.local.Unix()), nil }
func (customScheduleMoment) AttrNames() []string {
	return []string{"date", "day", "hour", "in_time_zone", "minute", "month", "time_of_day", "time_zone", "weekday", "year"}
}
func (m customScheduleMoment) Attr(name string) (starlark.Value, error) {
	switch name {
	case "year":
		return starlark.MakeInt(m.local.Year()), nil
	case "month":
		return starlark.MakeInt(int(m.local.Month())), nil
	case "day":
		return starlark.MakeInt(m.local.Day()), nil
	case "hour":
		return starlark.MakeInt(m.local.Hour()), nil
	case "minute":
		return starlark.MakeInt(m.local.Minute()), nil
	case "weekday":
		return starlark.String(customScheduleWeekdays[m.local.Weekday()]), nil
	case "date":
		return starlark.String(m.local.Format("2006-01-02")), nil
	case "time_of_day":
		return starlark.String(m.local.Format("15:04")), nil
	case "time_zone":
		return starlark.String(m.local.Location().String()), nil
	case "in_time_zone":
		return starlark.NewBuiltin("in_time_zone", m.inTimeZone), nil
	}
	return nil, nil
}

func (m customScheduleMoment) inTimeZone(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &name); err != nil {
		return nil, err
	}
	loc := m.zones[name]
	if loc == nil {
		return nil, fmt.Errorf("in_time_zone: unknown timezone %q (write the timezone name in your code as text, such as \"America/New_York\")", truncateRunes(name, 100))
	}
	return customScheduleMoment{local: m.local.In(loc), zones: m.zones}, nil
}

func customScheduleDaysSince(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return customScheduleCalendarSince(b, args, kwargs, 24*60)
}
func customScheduleWeeksSince(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return customScheduleCalendarSince(b, args, kwargs, 7*24*60)
}

// customScheduleCalendarSince counts whole wall-clock days or weeks from a
// local date (or date and time) to `at`, rounded down and negative before the
// start. Wall-clock counting keeps a 09:00 handoff at 09:00 across daylight
// saving changes.
func customScheduleCalendarSince(b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple, minutesPerUnit int64) (starlark.Value, error) {
	if len(args) != 2 || len(kwargs) != 0 {
		return nil, fmt.Errorf("%s: use %s(start, at)", b.Name(), b.Name())
	}
	start, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("%s: start must be text like \"2026-10-05\" or \"2026-10-05 09:00\"", b.Name())
	}
	at, ok := args[1].(customScheduleMoment)
	if !ok {
		return nil, fmt.Errorf("%s: the second argument must be at", b.Name())
	}
	startWall, err := time.Parse("2006-01-02 15:04", start)
	if err != nil {
		startWall, err = time.Parse("2006-01-02", start)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: start must be text like \"2026-10-05\" or \"2026-10-05 09:00\"", b.Name())
	}
	local := at.local
	atWall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, time.UTC)
	minutes := int64(atWall.Sub(startWall) / time.Minute)
	units := minutes / minutesPerUnit
	if minutes%minutesPerUnit != 0 && minutes < 0 {
		units--
	}
	return starlark.MakeInt64(units), nil
}
