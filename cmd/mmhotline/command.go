package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mmhotline/internal/app"
)

var commandActions = map[string][]string{
	"triage-agent":         {"show", "check-sign-in", "run-software-review", "usage"},
	"review-agent":         {"show", "check-sign-in"},
	"agent-playbook-rules": {"show", "check-whether-a-playbook-needs-a-person"},
	"sending-services":     {"list"},
	"people":               {"list", "create-sign-in-link"},
	"contact-methods":      {"list", "add", "change", "remove", "send-test"},
	"alert-sources":        {"list"},
	"schedules":            {"show", "preview"},
	// status-page set-up runs on a laptop, without MMHotline.
	"status-page": {"show", "set-up", "post", "resolve", "edit", "delete"},
	"down-alarm":  {"show", "send-test"},
	// list and show only read what happened; send-test-sos opens a test;
	// claim and resolve act like the incident page's buttons.
	"incidents":         {"list", "show", "send-test-sos", "claim", "resolve"},
	"alerts":            {"list"},
	"triage-sessions":   {"list", "show"},
	"approval-requests": {"list", "show"},
	// check, trust-through, and mark-reviewed act; list and show only read.
	"commit-reviews": {"check", "trust-through", "mark-reviewed", "list", "show"},
}

func runManagementCommand(group string, args []string) error {
	usage := "usage: mmhotline " + group + " " + strings.Join(commandActions[group], "|") + " [--command-socket PATH] [--json]"
	if group == "triage-agent" {
		// check-preparation runs as root without the command socket.
		usage += "\n       sudo mmhotline triage-agent check-preparation [--json]"
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-help") {
		fmt.Println(usage)
		return nil
	}
	if len(args) == 0 {
		return commandError{errors.New(usage)}
	}
	action := args[0]
	known := false
	for _, candidate := range commandActions[group] {
		if action == candidate {
			known = true
		}
	}
	if !known {
		return commandError{errors.New(usage)}
	}
	flags := flag.NewFlagSet(group+" "+action, flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	// status-page set-up runs on a laptop and reaches no MMHotline.
	socket := new(string)
	if group != "status-page" || action != "set-up" {
		socket = flags.String("command-socket", "/run/mmhotline/command.sock", "local command socket path")
	}
	asJSON := flags.Bool("json", false, "print one JSON object")
	request := app.CommandRequest{Group: group, Action: action, UnixUser: os.Getenv("SUDO_USER")}
	if request.UnixUser == "" {
		request.UnixUser = "root"
	}
	validate := func() error { return nil }
	var secretPath, tokenPath, pushoverUserKeyPath, settingsPath string
	waitForClaimMinutes := defaultWaitForClaimMinutes
	var statusPageSetUp statusPageSetUpInput
	switch group {
	case "agent-playbook-rules":
		validate = agentPlaybookRulesFlags(flags, action, &request.AgentPlaybookRules)
	case "status-page":
		validate = statusPageFlags(flags, action, &request.StatusPage, &statusPageSetUp)
	case "people":
		validate = peopleFlags(flags, action, &request.People)
	case "incidents":
		validate = incidentsFlags(flags, action, &request.Incidents, &waitForClaimMinutes)
	case "alerts":
		validate = alertsFlags(flags, &request.Alerts)
	case "triage-agent":
		validate = triageAgentFlags(flags, action, &request.TriageAgent)
	case "triage-sessions":
		validate = triageSessionsFlags(flags, action, &request.TriageSessions)
	case "approval-requests":
		validate = approvalRequestsFlags(flags, action, &request.ApprovalRequests)
	case "commit-reviews":
		validate = commitReviewsFlags(flags, action, &request.CommitReviews)
	case "contact-methods":
		in := &request.ContactMethods
		if action == "list" || action == "add" {
			flags.StringVar(&in.SignInEmailAddress, "sign-in-email-address", "", "person's sign-in email address")
			flags.BoolVar(&in.SharedWithTeam, "shared-with-team", false, "contact method belongs to the team account, not one person")
		}
		if action != "list" && action != "add" {
			flags.StringVar(&in.ContactMethodID, "contact-method-id", "", "contact method's ID")
		}
		if action == "add" {
			flags.StringVar(&in.ContactMethodType, "contact-method-type", "", "email, outgoing_webhook, automated_voice_call, or pushover_notification")
			flags.StringVar(&in.EmailAddress, "email-address", "", "email destination address")
			flags.StringVar(&in.OutgoingWebhookURL, "outgoing-webhook-url", "", "HTTPS webhook receiver URL")
			flags.StringVar(&tokenPath, "outgoing-webhook-bearer-token-file", "", "path to the file containing the outgoing webhook bearer token")
			flags.StringVar(&secretPath, "outgoing-webhook-signing-secret-file", "", "path where MMHotline writes the new outgoing webhook signing secret with mode 0600")
			flags.StringVar(&pushoverUserKeyPath, "pushover-user-key-file", "", "path to the file containing the personal Pushover user key (30 letters and digits), readable only by the file owner, or - for standard input")
			flags.StringVar(&in.PushoverDeviceName, "pushover-device-name", "", "send only to this one of the person's Pushover devices (up to 25 letters, digits, _ and -); the default is all their devices")
			flags.StringVar(&in.PhoneNumberToCall, "phone-number-to-call", "", "phone number to call, with +, country code, and number, such as +14155550100")
			send := flags.Bool("send-while-unavailable-or-muted", false, "send while the person is unavailable or the team is muted")
			in.SendWhileUnavailableOrMuted = send
		}
		if action == "change" {
			flags.Func("send-while-unavailable-or-muted", "true or false", func(value string) error {
				if value != "true" && value != "false" {
					return errors.New("use true or false")
				}
				parsed, _ := strconv.ParseBool(value)
				in.SendWhileUnavailableOrMuted = &parsed
				return nil
			})
		}
		if action == "add" || action == "change" {
			flags.Func("alert-severities", "comma-separated sos,warn,info; empty means no alerts", func(value string) error {
				values := []string{}
				if value != "" {
					values = strings.Split(value, ",")
				}
				in.AlertSeverities = &values
				return nil
			})
		}
		validate = func() error {
			if action != "list" && action != "add" && in.ContactMethodID == "" {
				return errors.New("--contact-method-id is required")
			}
			if action == "add" {
				if (tokenPath != "" || secretPath != "" || in.OutgoingWebhookURL != "") && in.ContactMethodType != "outgoing_webhook" {
					return errors.New("outgoing webhook flags require --contact-method-type outgoing_webhook")
				}
				if in.EmailAddress != "" && in.ContactMethodType != "email" {
					return errors.New("--email-address requires --contact-method-type email")
				}
				if pushoverUserKeyPath != "" && in.ContactMethodType != "pushover_notification" {
					return errors.New("--pushover-user-key-file requires --contact-method-type pushover_notification")
				}
				if in.PushoverDeviceName != "" && in.ContactMethodType != "pushover_notification" {
					return errors.New("--pushover-device-name requires --contact-method-type pushover_notification")
				}
				if in.ContactMethodType == "pushover_notification" {
					key, err := readSecret(pushoverUserKeyPath, "pushover-user-key-file")
					if err != nil {
						return err
					}
					in.PushoverUserKey = key
				}
				if in.PhoneNumberToCall != "" && in.ContactMethodType != "automated_voice_call" {
					return errors.New("--phone-number-to-call requires --contact-method-type automated_voice_call")
				}
				if tokenPath != "" {
					raw, err := os.ReadFile(tokenPath)
					if err != nil {
						return fmt.Errorf("read webhook bearer token file: %w", err)
					}
					in.OutgoingWebhookBearerToken = strings.TrimSpace(string(raw))
				}
			}
			return nil
		}
	case "schedules":
		if action == "preview" {
			flags.StringVar(&settingsPath, "settings-file", "", "path to the settings file whose on_call_schedule is previewed")
		}
		validate = func() error {
			if action != "preview" {
				return nil
			}
			if settingsPath == "" {
				return errors.New("--settings-file is required")
			}
			file, problems := app.ReadSettingsFile(settingsPath, nil)
			if len(problems) > 0 {
				return fmt.Errorf("the settings file given with --settings-file has problems; correct the listed problems, then run schedules preview again:\n%s", settingsProblemLines(problems))
			}
			schedule := file.Settings.OnCallSchedule
			request.Schedules.Schedule = &schedule
			return nil
		}
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stderr, flagOutput.String())
			return nil
		}
		return commandError{errors.New(flagOutput.String())}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("unexpected positional arguments")}
	}
	if err := validate(); err != nil {
		return commandError{err}
	}
	if group == "status-page" && action == "set-up" {
		return runStatusPageSetUp(statusPageSetUp, *asJSON)
	}
	// Reserve the output file before creating a secret on the server. The
	// incomplete temporary file remains private, then rename publishes it whole.
	var secretFile *os.File
	if secretPath != "" {
		var err error
		secretFile, err = os.CreateTemp(filepath.Dir(secretPath), ".mmhotline-signing-secret-*")
		if err != nil {
			return commandError{fmt.Errorf("prepare temporary outgoing webhook signing secret file: %w", err)}
		}
		defer secretFile.Close()
		defer func() { os.Remove(secretFile.Name()) }()
	}
	// The test of the MMHotline-down alarm may sound the phone once more.
	if group == "down-alarm" && action == "send-test" {
		say(*asJSON, "\n"+downAlarmTestWarning+"\n")
	}
	var result app.CommandResult
	var err error
	if group == "status-page" && statusPageWithoutService() {
		result, err = runStatusPageWithoutService(action, request.StatusPage)
	} else if group == "commit-reviews" && (action == "check" || action == "trust-through") {
		// The check fetches under the copy's lock, which an MMX run can
		// hold for up to an hour; the command waits as long as MMX runs do.
		result, err = sendCommandWithTimeout(context.Background(), *socket, request, 0)
	} else {
		result, err = sendCommand(*socket, request)
	}
	if err != nil {
		return err
	}
	var setupTestError error
	if (group == "triage-agent" || group == "review-agent") && action == "check-sign-in" {
		result, setupTestError = waitSignInCheck(*socket, request.UnixUser, group, result)
	}
	// Exit code 1 says a commit through the checked commit waits for review;
	// the answer still names the trusted commit.
	if check := result.CommitCheck; check != nil && check.TrustedThroughCommitID != check.CheckedCommitID {
		setupTestError = checkAnswer{exitCode: 1}
	}
	// Exit code 1 says a down-alarm test would leave someone unwoken.
	if group == "down-alarm" && action == "send-test" && result.DownAlarm != nil && app.DownAlarmTestFailed(result.DownAlarm.TestSends) {
		setupTestError = checkAnswer{exitCode: 1}
	}
	// Exit code 1 says nobody acknowledged the test SOS on the phone.
	if group == "incidents" && action == "send-test-sos" {
		result, setupTestError = waitForTestSOSClaim(*socket, request.UnixUser, result, time.Duration(waitForClaimMinutes)*time.Minute, *asJSON)
		// Any other error ends the command without an answer to print.
		if answer := (checkAnswer{}); setupTestError != nil && !errors.As(setupTestError, &answer) {
			return setupTestError
		}
	}
	if secretFile != nil {
		shownOnce, file := &result.OutgoingWebhookSigningSecretShownOnce, &result.OutgoingWebhookSigningSecretFile
		if *shownOnce == "" {
			return commandError{errors.New("mmhotline serve returned a command response without a new outgoing webhook signing secret")}
		}
		if err = writeSecretFile(secretFile, secretPath, *shownOnce); err != nil {
			return commandError{err}
		}
		*shownOnce = ""
		*file = secretPath
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
		return setupTestError
	}
	printText(result.Text())
	return setupTestError
}

// printText prints a command's text output. Alert titles, alert messages, and
// triage agent text come from outsiders, and a terminal acts on escape
// sequences, which can rewrite earlier lines, set the window title, or write
// the clipboard. Every control character except the line feed and the tab is
// printed as U+FFFD, so one rule covers every text command. The --json output
// is escaped by encoding/json and does not pass through here.
func printText(text string) { writeText(os.Stdout, text) }

// writeText is printText to out.
func writeText(out io.Writer, text string) {
	fmt.Fprintln(out, strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return '�'
		}
		return r
	}, text))
}
func writeSecretFile(file *os.File, path, secret string) error {
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("set outgoing webhook signing secret file permissions: %w", err)
	}
	if _, err := file.WriteString(secret); err != nil {
		return fmt.Errorf("write outgoing webhook signing secret file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("save outgoing webhook signing secret file to disk: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close outgoing webhook signing secret file: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("move temporary outgoing webhook signing secret file to the requested path: %w", err)
	}
	return nil
}
func sendCommand(socket string, request app.CommandRequest) (app.CommandResult, error) {
	return sendCommandContext(context.Background(), socket, request)
}

func sendCommandContext(ctx context.Context, socket string, request app.CommandRequest) (app.CommandResult, error) {
	return sendCommandWithTimeout(ctx, socket, request, 30*time.Second)
}

// sendCommandWithTimeout sends a command and waits at most timeout for the
// answer; 0 waits without a limit.
func sendCommandWithTimeout(ctx context.Context, socket string, request app.CommandRequest, timeout time.Duration) (app.CommandResult, error) {
	var result app.CommandResult
	raw, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://mmhotline/command", bytes.NewReader(raw))
	if err != nil {
		return result, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, commandError{fmt.Errorf("mmhotline serve is not running at command socket %s", socket)}
		}
		return result, commandError{fmt.Errorf("cannot reach mmhotline serve at command socket %s: %w", socket, err)}
	}
	defer response.Body.Close()
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		return result, commandError{fmt.Errorf("cannot decode the JSON response from mmhotline serve: %w", err)}
	}
	if response.StatusCode != http.StatusOK {
		return result, commandError{errors.New(result.Message)}
	}
	return result, nil
}
