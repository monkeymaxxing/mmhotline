package main

import (
	"bufio"
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
		raw, err = readSecretFromStandardInput(os.Stdin, os.Stderr, turnOffTerminalEcho)
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

// readSecretFromStandardInput reads a secret from input. A pipe is read to
// its end, as before. A terminal shows nothing as the person pastes, and one
// line, ending with Enter, is the whole secret: Ctrl-D is not needed, and
// would close the person's shell if typed after Enter.
func readSecretFromStandardInput(input *os.File, prompts io.Writer, turnOffEcho func(*os.File) (func(), bool)) ([]byte, error) {
	restore, isTerminal := turnOffEcho(input)
	if !isTerminal {
		return io.ReadAll(io.LimitReader(input, 64<<10))
	}
	defer restore()
	fmt.Fprint(prompts, "Paste the value, then press Enter. Nothing shows as you paste: ")
	line, err := bufio.NewReader(io.LimitReader(input, 64<<10)).ReadBytes('\n')
	fmt.Fprintln(prompts)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return line, err
}

// statusPageSetUpInput is what status-page set-up takes. The API token is
// the person's own, used for the set-up only and never stored.
type statusPageSetUpInput struct {
	cloudflareAccountID, hostName, cloudflareAPIToken string
}

func statusPageFlags(flags *flag.FlagSet, action string, in *app.StatusPageCommandRequest, setUp *statusPageSetUpInput) func() error {
	var tokenPath string
	switch action {
	case "set-up":
		flags.StringVar(&setUp.cloudflareAccountID, "cloudflare-account-id", "", "the team's Cloudflare account ID, 32 hexadecimal characters")
		flags.StringVar(&setUp.hostName, "host-name", "", "the page's public host name in one of the account's zones, such as status.example.com")
		flags.StringVar(&tokenPath, "cloudflare-api-token-file", "", "path to the file containing your Cloudflare API token, readable only by the file owner, or - for standard input; the token is used for set-up only and never stored")
	case "post":
		flags.StringVar(&in.Message, "message", "", "the message, 1 to 5,000 characters")
		flags.Int64Var(&in.NoticeID, "notice-id", 0, "ID of an open notice to update with --message; without --notice-id, post a new notice")
	case "resolve":
		flags.Int64Var(&in.NoticeID, "notice-id", 0, "the notice to resolve")
		flags.StringVar(&in.ClosingMessage, "closing-message", "", "optional last update, such as \"Sign-in works again.\"")
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
		case "post":
			if in.Message == "" {
				return errors.New("--message is required")
			}
		case "resolve":
			if in.NoticeID == 0 {
				return errors.New("--notice-id is required")
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

// runStatusPageWithoutService does show, post, and resolve with the same
// statuspage.Publish that MMHotline's service uses.
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
	case "post":
		file, noticeID, err = statuspage.Publish(ctx, bucket, statuspage.Post(in.NoticeID, in.Message), "", time.Now())
		result.Message = fmt.Sprintf("Posted to notice %d, directly in the R2 bucket.", noticeID)
	case "resolve":
		file, noticeID, err = statuspage.Publish(ctx, bucket, statuspage.Resolve(in.NoticeID, in.ClosingMessage), "", time.Now())
		result.Message = fmt.Sprintf("Notice %d is resolved, directly in the R2 bucket.", noticeID)
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
