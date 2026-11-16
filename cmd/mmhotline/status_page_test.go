package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
	laptop := func(args ...string) *os.ProcessState {
		t.Helper()
		command := cliProcess(t, append([]string{"status-page"}, args...)...)
		command.Env = append(command.Env, "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL="+bucketURL, "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID="+fake.R2AccessKeyID, "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY=laptop-secret")
		output, _ := command.CombinedOutput()
		t.Logf("status-page %s:\n%s", strings.Join(args, " "), output)
		if strings.Contains(string(output), "laptop-secret") {
			t.Fatal("the secret access key was printed")
		}
		return command.ProcessState
	}
	if state := laptop("post", "--message", "Hello"); state.ExitCode() != 4 {
		t.Fatalf("a post before set-up published a page: exit %d", state.ExitCode())
	}
	if _, _, err := statuspage.Publish(context.Background(), bucket, statuspage.SetPageTitle("Example Inc."), "Example Inc.", time.Now()); err != nil {
		t.Fatal(err)
	}
	if state := laptop("post", "--message", "We are looking into a problem."); !state.Success() {
		t.Fatal("post failed")
	}
	if state := laptop("post", "--notice-id", "1", "--message", "The database is moving."); !state.Success() {
		t.Fatal("update failed")
	}
	if state := laptop("resolve", "--notice-id", "1", "--closing-message", "Fixed."); !state.Success() {
		t.Fatal("resolve failed")
	}
	file, err := statuspage.Read(context.Background(), bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Notices) != 1 || file.Notices[0].ResolvedAt == nil || len(file.Notices[0].Updates) != 3 {
		t.Fatalf("published %+v", file)
	}
	if page, _ := fake.Object("mmhotline-status-status-example-com-18a8ea11", "index.html"); !strings.Contains(string(page.Content), "The database is moving.") {
		t.Fatalf("index.html: %s", page.Content)
	}
	command := cliProcess(t, "status-page", "show", "--json")
	command.Env = append(command.Env, "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL="+bucketURL, "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID="+fake.R2AccessKeyID)
	if output, _ := command.CombinedOutput(); command.ProcessState.ExitCode() != 4 || !strings.Contains(string(output), "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY is required") {
		t.Fatalf("show without the secret: %s", output)
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
