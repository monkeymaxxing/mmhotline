package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mmhotline/internal/app"
	"mmhotline/internal/cloudflareapi"
	"mmhotline/internal/statuspage"
)

// readSecret reads a credential from standard input when path is "-", or
// from a file that only its owner can read. A credential never arrives as a
// flag value, because other users on the server can read a program's
// arguments.
func readSecret(path, flagName string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--%s is required: a file only its owner can read, or - for standard input", flagName)
	}
	var raw []byte
	var err error
	if path == "-" {
		raw, err = readSecretFromStandardInput(os.Stdin, os.Stderr, pasteTheValuePrompt, turnOffTerminalEcho)
	} else {
		var info os.FileInfo
		if info, err = os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("--%s %s can be read by other users; run chmod 600 %s first", flagName, path, path)
		}
		if err == nil {
			raw, err = os.ReadFile(path)
		}
	}
	if err != nil {
		return "", fmt.Errorf("read --%s: %w", flagName, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("--%s gave an empty value", flagName)
	}
	return value, nil
}

// pasteTheValuePrompt is the hidden prompt of a command that reads one
// secret value.
const pasteTheValuePrompt = "Paste the value, then press Enter. Nothing shows as you paste: "

// readSecretFromStandardInput reads a secret from input. A pipe is read to
// its end, as before. A terminal shows prompt, shows nothing as the person
// pastes, and one line, ending with Enter, is the whole secret: Ctrl-D is
// not needed, and would close the person's shell if typed after Enter.
func readSecretFromStandardInput(input *os.File, prompts io.Writer, prompt string, turnOffEcho func(*os.File) (func(), bool)) ([]byte, error) {
	restore, isTerminal := turnOffEcho(input)
	if !isTerminal {
		return io.ReadAll(io.LimitReader(input, 64<<10))
	}
	defer restore()
	fmt.Fprint(prompts, prompt)
	line, err := readOneLine(input)
	fmt.Fprintln(prompts)
	return line, err
}

// readOneLine reads input up to and including the next line feed, at most
// 64 KiB, one byte at a time: nothing after the line is read, so the next
// question of the same command reads the person's next answer.
func readOneLine(input io.Reader) ([]byte, error) {
	var line []byte
	var one [1]byte
	for len(line) < 64<<10 {
		count, err := input.Read(one[:])
		if count == 1 {
			line = append(line, one[0])
			if one[0] == '\n' {
				return line, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return line, nil
		}
		if err != nil {
			return line, err
		}
	}
	return line, nil
}

// statusPageSetUpInput is what status-page set-up takes. The API token is
// the person's own, used for the set-up only and never stored.
type statusPageSetUpInput struct {
	cloudflareAccountID, hostName, cloudflareAPIToken string
}

// componentFlag collects each --component given, in order.
type componentFlag []string

func (c *componentFlag) String() string      { return strings.Join(*c, ",") }
func (c *componentFlag) Set(id string) error { *c = append(*c, id); return nil }

// statusPageFlags defines the flags of a status-page action. The values of
// post, resolve, edit, and delete go into in, only those the person gave: flags.Visit says
// which, so a flag left out is never taken for an empty or zero value.
func statusPageFlags(flags *flag.FlagSet, action string, in *app.StatusPageCommandRequest, setUp *statusPageSetUpInput) func() error {
	var tokenPath string
	var title, impact, stage, message, closingMessage, scheduledStart, scheduledEnd string
	var noticeID, updateID int64
	var clearComponents, clearSchedule, clearStage bool
	var components componentFlag
	switch action {
	case "set-up":
		flags.StringVar(&setUp.cloudflareAccountID, "cloudflare-account-id", "", "the team's Cloudflare account ID, 32 hexadecimal characters")
		flags.StringVar(&setUp.hostName, "host-name", "", "the page's public host name in one of the account's zones, such as status.example.com")
		flags.StringVar(&tokenPath, "cloudflare-api-token-file", "", "path to the file containing your Cloudflare API token, readable only by the file owner, or - for standard input; the token is used for set-up only and never stored")
	case "post":
		flags.StringVar(&message, "message", "", "the message, 1 to 5,000 characters")
		flags.Int64Var(&noticeID, "notice-id", 0, "ID of an open notice to update with --message and --stage; without --notice-id, post a new notice")
		flags.StringVar(&title, "title", "", "a new notice's title, 1 to 140 characters on one line (required for a new notice)")
		flags.StringVar(&impact, "impact", "", "a new notice's impact: outage (the default), degraded, or maintenance")
		flags.Var(&components, "component", "a component ID the new notice affects, as the settings file's status_page components list it; repeat it for more than one")
		flags.StringVar(&stage, "stage", "", "optional stage of the message: investigating, identified, monitoring, or resolved for outage and degraded notices; scheduled, in_progress, or completed for maintenance")
		flags.StringVar(&scheduledStart, "scheduled-start", "", "a new maintenance notice's planned start, RFC 3339, such as 2026-11-21T02:00:00Z")
		flags.StringVar(&scheduledEnd, "scheduled-end", "", "a new maintenance notice's planned end, RFC 3339, after the planned start")
	case "resolve":
		flags.Int64Var(&noticeID, "notice-id", 0, "the notice to resolve")
		flags.StringVar(&closingMessage, "closing-message", "", "optional last update, such as \"Sign-in works again.\"")
		flags.StringVar(&stage, "stage", "", "stage of the closing message, which needs --closing-message; the default is resolved, or completed for maintenance")
	case "edit":
		flags.Int64Var(&noticeID, "notice-id", 0, "the notice to edit, open or resolved")
		flags.Int64Var(&updateID, "update-id", 0, "edit this update of the notice, with --message, --stage, or --clear-stage; without --update-id, edit the notice itself")
		flags.StringVar(&title, "title", "", "the notice's new title, 1 to 140 characters on one line")
		flags.StringVar(&impact, "impact", "", "the notice's new impact: outage, degraded, or maintenance; a change away from maintenance removes its planned window")
		flags.Var(&components, "component", "a component ID the notice affects, as the settings file's status_page components list it; repeat it for more than one; the components given replace the notice's components")
		flags.BoolVar(&clearComponents, "clear-components", false, "remove all components from the notice")
		flags.StringVar(&scheduledStart, "scheduled-start", "", "the maintenance notice's new planned start, RFC 3339, such as 2026-11-21T02:00:00Z")
		flags.StringVar(&scheduledEnd, "scheduled-end", "", "the maintenance notice's new planned end, RFC 3339, after the planned start")
		flags.BoolVar(&clearSchedule, "clear-schedule", false, "remove the notice's planned start and end")
		flags.StringVar(&message, "message", "", "the update's new message, 1 to 5,000 characters (with --update-id)")
		flags.StringVar(&stage, "stage", "", "the update's new stage: investigating, identified, monitoring, or resolved for outage and degraded notices; scheduled, in_progress, or completed for maintenance (with --update-id)")
		flags.BoolVar(&clearStage, "clear-stage", false, "remove the update's stage (with --update-id)")
	case "delete":
		flags.Int64Var(&noticeID, "notice-id", 0, "the notice to delete, open or resolved, with all its updates")
		flags.Int64Var(&updateID, "update-id", 0, "delete only this update of the notice; a notice keeps at least one update")
	}
	return func() error {
		var err error
		switch action {
		case "set-up":
			for _, required := range []struct{ value, name string }{{setUp.cloudflareAccountID, "cloudflare-account-id"}, {setUp.hostName, "host-name"}} {
				if required.value == "" {
					return fmt.Errorf("--%s is required", required.name)
				}
			}
			setUp.cloudflareAPIToken, err = readSecret(tokenPath, "cloudflare-api-token-file")
		case "post", "resolve", "edit", "delete":
			flags.Visit(func(given *flag.Flag) {
				switch given.Name {
				case "notice-id":
					in.NoticeID = &noticeID
				case "update-id":
					in.UpdateID = &updateID
				case "clear-components":
					in.ClearComponents = clearComponents
				case "clear-schedule":
					in.ClearSchedule = clearSchedule
				case "clear-stage":
					in.ClearStage = clearStage
				case "title":
					in.Title = &title
				case "impact":
					in.Impact = &impact
				case "component":
					affected := []string(components)
					in.AffectedComponentIDs = &affected
				case "stage":
					in.Stage = &stage
				case "message":
					in.Message = &message
				case "closing-message":
					in.ClosingMessage = &closingMessage
				case "scheduled-start":
					in.ScheduledStartAt = &scheduledStart
				case "scheduled-end":
					in.ScheduledEndAt = &scheduledEnd
				}
			})
			// The same check the server and a laptop's direct post make, so a
			// bad request fails here, before either.
			switch action {
			case "post":
				_, err = in.PostChange()
			case "resolve":
				_, err = in.ResolveChange()
			case "edit":
				_, err = in.EditChange()
			case "delete":
				_, err = in.DeleteChange()
			}
		}
		return err
	}
}

// statusPageCloudflare is where status-page set-up reaches Cloudflare;
// tests point it at internal/fakecloudflare.
var statusPageCloudflare = cloudflareapi.CloudflareAddresses()

// runStatusPageSetUp sets the status page up in the team's Cloudflare
// account from the person's laptop, without MMHotline (CA8), and prints
// r2_bucket_url for the settings file and the next step.
func runStatusPageSetUp(in statusPageSetUpInput, asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := app.SetUpStatusPageInCloudflare(ctx, statusPageCloudflare, in.cloudflareAccountID, in.hostName, in.cloudflareAPIToken)
	if err != nil {
		return commandError{err}
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	printText(result.Text())
	return nil
}

// The status page's environment variables on a responder's laptop.
const (
	statusPageBucketURLEnvVar       = "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL"
	statusPageAccessKeyIDEnvVar     = "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID"
	statusPageSecretAccessKeyEnvVar = "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY"
)

// statusPageWithoutService reports whether the status page command posts
// straight to the R2 bucket: on a responder's laptop, while the MMHotline
// server may be down, given only an R2 access key for that one bucket.
func statusPageWithoutService() bool { return os.Getenv(statusPageBucketURLEnvVar) != "" }

// runStatusPageWithoutService does show, post, resolve, edit, and delete with the same
// statuspage.Publish, and the same changes from the request, that MMHotline's
// service uses.
func runStatusPageWithoutService(action string, in app.StatusPageCommandRequest) (app.CommandResult, error) {
	var result app.CommandResult
	bucketURL, err := statuspage.CheckBucketURL(os.Getenv(statusPageBucketURLEnvVar))
	if err != nil {
		return result, commandError{fmt.Errorf("%s: %w", statusPageBucketURLEnvVar, err)}
	}
	bucket := &statuspage.R2Bucket{BucketURL: bucketURL, AccessKeyID: os.Getenv(statusPageAccessKeyIDEnvVar), SecretAccessKey: os.Getenv(statusPageSecretAccessKeyEnvVar)}
	for _, required := range []struct{ value, name string }{{bucket.AccessKeyID, statusPageAccessKeyIDEnvVar}, {bucket.SecretAccessKey, statusPageSecretAccessKeyEnvVar}} {
		if required.value == "" {
			return result, commandError{fmt.Errorf("%s is required with %s", required.name, statusPageBucketURLEnvVar)}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var file statuspage.File
	var noticeID int64
	switch action {
	case "show":
		file, err = statuspage.Read(ctx, bucket)
	case "post", "resolve", "edit", "delete":
		build, doneMessage := in.PostChange, func() string { return fmt.Sprintf("Posted to notice %d, directly in the R2 bucket.", noticeID) }
		switch action {
		case "resolve":
			build, doneMessage = in.ResolveChange, func() string { return fmt.Sprintf("Notice %d is resolved, directly in the R2 bucket.", noticeID) }
		case "edit", "delete":
			build = in.EditChange
			if action == "delete" {
				build = in.DeleteChange
			}
			doneMessage = func() string {
				return strings.TrimSuffix(in.EditOrDeleteDoneMessage(action, noticeID), ".") + ", directly in the R2 bucket."
			}
		}
		change, refusal := build()
		if refusal != nil {
			return result, commandError{refusal}
		}
		file, noticeID, err = statuspage.Publish(ctx, bucket, change, "", time.Now())
		result.Message = doneMessage()
	}
	if err != nil && file.PageTitle == "" {
		if errors.Is(err, statuspage.ErrRefused) {
			err = errors.New(strings.TrimPrefix(err.Error(), statuspage.ErrRefused.Error()+": "))
		}
		return result, commandError{err}
	}
	view := &app.StatusPageView{R2BucketURL: bucketURL, PublishedStatusJSON: &file}
	if err != nil {
		view.ReadError = err.Error()
	}
	result.StatusPage, result.StatusPageNoticeID = view, noticeID
	return result, nil
}
