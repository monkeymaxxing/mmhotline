package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mmhotline/internal/app"
	"mmhotline/internal/fakesendingservices"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestContactMethodTypeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, fake string
		invalid    bool
	}{
		{name: "no_sending_services"},
		{name: "fake_sending_services", fake: "http://127.0.0.1:8090"},
		{name: "fake_sending_services_bad_url", fake: "ftp://127.0.0.1", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MMHOTLINE_FAKE_SENDING_SERVICES_URL", tc.fake)
			contactMethodTypes, fakeURL, err := contactMethodTypeConfiguration()
			if (err != nil) != tc.invalid {
				t.Fatal(err)
			}
			if tc.invalid {
				return
			}
			if contactMethodTypes["email"] != nil || contactMethodTypes["telegram_message"] != nil {
				t.Fatal("database-backed contact method types must be set up through sending service setup")
			}
			if fakeURL != tc.fake {
				t.Fatal("fake URL changed")
			}
			webhook := contactMethodTypes["outgoing_webhook"].(app.OutgoingWebhookContactMethodType)
			if (tc.fake != "") != (webhook.AllowedPrivateURLPrefix == tc.fake+"/webhook-receiver/") {
				t.Fatal("webhook prefix")
			}
		})
	}
}

// TestMain lets the test binary stand in for mmhotline when the custom
// schedule sandbox runner starts os.Executable().
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == app.CustomScheduleSandboxCommand {
		os.Exit(app.RunCustomScheduleSandbox(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestCLIHelperProcess(t *testing.T) {
	if endpoint := os.Getenv("MMHOTLINE_TEST_ACCESS_CERTS_URL"); endpoint != "" {
		certsURL, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		http.DefaultTransport = cliAccessCertsTransport{next: http.DefaultTransport, certsURL: certsURL}
	}
	if os.Getenv("MMHOTLINE_CLI_TEST_PROCESS") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"mmhotline"}, os.Args[index+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing CLI arguments")
}
func cliProcess(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, append([]string{"-test.run=^TestCLIHelperProcess$", "--"}, args...)...)
	command.Env = append(os.Environ(), "MMHOTLINE_CLI_TEST_PROCESS=1", "MMHOTLINE_FAKE_SENDING_SERVICES_URL=", "MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL=", "MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID=", "MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY=")
	return command
}

// Other programs, such as the triage agent's software review, read
// the installed version from `mmhotline version`.
func TestCLIVersion(t *testing.T) {
	output, err := cliProcess(t, "version").Output()
	if err != nil || !strings.HasPrefix(string(output), "mmhotline "+version+" (") {
		t.Fatalf("version: %q %v", output, err)
	}
	output, err = cliProcess(t, "version", "--json").Output()
	var build map[string]any
	if err != nil || json.Unmarshal(output, &build) != nil || build["version"] != version || build["os"] != runtime.GOOS || build["revision"] == "" {
		t.Fatalf("version --json: %q %v", output, err)
	}
	command := cliProcess(t, "version", "extra")
	if err = command.Run(); command.ProcessState.ExitCode() != 4 {
		t.Fatalf("version with an argument: %v", err)
	}
}

func TestCLIHelpExitCodesAndSetup(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"setup", "--help"}, {"serve", "--help"}} {
		output, err := cliProcess(t, args...).CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--state-dir") || !strings.Contains(string(output), "--settings-file") || strings.Contains(string(output), "-data") {
			t.Fatalf("help %v: %s %v", args, output, err)
		}
	}
	for _, args := range [][]string{nil, {"unknown"}, {"setup"}, {"setup", "--unknown"}, {"setup", "--state-dir", "state", "extra"}, {"serve", "--state-dir", "state"}, {"serve", "--state-dir", "state", "--unknown", "bad"}} {
		command := cliProcess(t, args...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		output := stderr.String()
		if stdout.Len() != 0 || !strings.HasPrefix(output, "mmhotline: ") || strings.Count(output, "mmhotline: ") != 1 || strings.Count(output, "usage: mmhotline") > 1 || strings.Count(output, "flag provided but not defined") > 1 {
			t.Fatalf("error output %v: stdout=%q stderr=%q", args, stdout.String(), output)
		}
		if (len(args) == 0 || args[0] == "unknown" || strings.Contains(output, "flag provided but not defined")) && strings.Count(output, "usage: mmhotline") != 1 {
			t.Fatalf("missing usage %v: %s", args, output)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 4 {
			t.Fatalf("usage %v: %s %v", args, output, err)
		}
	}
	skipUnlessSettingsFileOwnerAllowed(t)
	good := testSettingsYAML("https://mmhotline.example.com", "127.0.0.1:0", "127.0.0.1:0")
	for _, name := range []string{`"  "`, strings.Repeat("x", 101)} {
		path := writeTestSettingsFile(t, "", strings.Replace(good, "team_name: Operations", "team_name: "+name, 1))
		output, err := cliProcess(t, "setup", "--state-dir", filepath.Join(t.TempDir(), "state"), "--settings-file", path).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "team_name: team name must contain 1–100 characters") {
			t.Fatalf("invalid name %q: %s %v", name, output, err)
		}
	}
	settingsPath := writeTestSettingsFile(t, "", good)
	dir := filepath.Join(t.TempDir(), "state")
	output, err := cliProcess(t, "setup", "--state-dir", dir, "--settings-file", settingsPath).CombinedOutput()
	if err != nil || string(output) != "Created MMHotline state in "+dir+".\n" {
		t.Fatalf("setup: %s %v", output, err)
	}
	for _, name := range []string{"mmhotline.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	output, err = cliProcess(t, "setup", "--state-dir", dir, "--settings-file", settingsPath).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.HasPrefix(string(output), "mmhotline: ") {
		t.Fatalf("setup existing state: %s %v", output, err)
	}
}
func TestCLISignalShutdownExitCodes(t *testing.T) {
	fake := httptest.NewServer(fakesendingservices.New())
	defer fake.Close()
	for _, test := range []struct {
		name   string
		signal os.Signal
		want   int
		// fakeSendingServices runs the Telegram poller, which is waiting in a
		// long getUpdates when the signal arrives.
		fakeSendingServices bool
	}{{"SIGTERM", syscall.SIGTERM, 0, false}, {"SIGINT", os.Interrupt, 130, false}, {"SIGTERM while polling Telegram", syscall.SIGTERM, 0, true}} {
		t.Run(test.name, func(t *testing.T) {
			if test.fakeSendingServices {
				skipUnlessCommandSocketClientAllowed(t)
			}
			skipUnlessSettingsFileOwnerAllowed(t)
			socketDir := shortSocketDirectory(t)
			webAddress := tcpListenAddress(t)
			commandSocket := filepath.Join(socketDir, "command.sock")
			dir := filepath.Join(t.TempDir(), "state")
			if err := app.Setup(context.Background(), dir, "Fixture team"); err != nil {
				t.Fatal(err)
			}
			settings := testSettingsYAML("https://mmhotline.example.com", webAddress, "127.0.0.1:0")
			if test.fakeSendingServices {
				settings = strings.Replace(settings, "sending_services: {}", "sending_services:\n  telegram:\n    bot_token: fake-telegram-bot-token", 1)
			}
			settingsPath := writeTestSettingsFile(t, "", settings)
			command := cliProcess(t, "serve", "--state-dir", dir, "--settings-file", settingsPath, "--command-socket", commandSocket)
			if test.fakeSendingServices {
				command.Env = append(command.Env, "MMHOTLINE_FAKE_SENDING_SERVICES_URL="+fake.URL)
			}
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if command.ProcessState == nil {
					command.Process.Kill()
					command.Wait()
				}
			})
			client := tcpHTTPClient(webAddress)
			defer client.CloseIdleConnections()
			ready := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				response, err := client.Get("http://mmhotline/people")
				if err == nil {
					response.Body.Close()
					ready = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				t.Fatal("server did not become ready")
			}
			if test.fakeSendingServices {
				// An apply waits for the Telegram part, which serve applies in
				// the background at start and which then starts polling.
				out, err := cliProcess(t, "settings", "apply", "--command-socket", commandSocket).CombinedOutput()
				if err != nil {
					t.Fatalf("set up before signal: %s %v", out, err)
				}
				// Let the poller reach its long getUpdates.
				time.Sleep(300 * time.Millisecond)
			}
			signalled := time.Now()
			if err := command.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			err := command.Wait()
			got := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				got = exit.ExitCode()
			}
			if got != test.want {
				t.Fatalf("shutdown: %d want %d: %s", got, test.want, output.String())
			}
			if !strings.Contains(output.String(), "Sign-in: sign-in links only; the settings file has no cloudflare_access") {
				t.Fatal("missing links-only startup notice")
			}
			if time.Since(signalled) > 5*time.Second {
				t.Fatalf("shutdown took %s", time.Since(signalled))
			}
		})
	}
}

func TestSlackRequiresDatabaseSetup(t *testing.T) {
	for _, fakeURL := range []string{"", "http://127.0.0.1:8090"} {
		t.Setenv("MMHOTLINE_FAKE_SENDING_SERVICES_URL", fakeURL)
		contactMethodTypes, _, err := contactMethodTypeConfiguration()
		if err != nil || contactMethodTypes["slack_message"] != nil {
			t.Fatalf("Slack must use checked database setup: %v", err)
		}
	}
}

func shortSocketDirectory(t *testing.T) string {
	t.Helper()
	// macOS Unix socket paths must fit in 104 bytes; testing.T's default path is longer.
	directory, err := os.MkdirTemp("/tmp", "mmhotline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return directory
}

func TestCommandPeerAuthorization(t *testing.T) {
	for _, item := range []struct {
		system       string
		peer, server uint32
		allowed      bool
	}{
		{"linux", 0, 501, true}, {"linux", 501, 501, false}, {"linux", 502, 501, false},
		{"darwin", 501, 501, true}, {"darwin", 502, 501, false},
	} {
		if err := authorizeCommandPeer(item.peer, item.server, item.system); (err == nil) != item.allowed || err != nil && err.Error() != "Run the mmhotline command with sudo." {
			t.Fatalf("%+v: %v", item, err)
		}
	}
	reached := false
	handler := checkCommandPeer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "http://mmhotline/command", strings.NewReader(`{"unix_user":"root"}`)))
	if reached || response.Code != 403 || !strings.Contains(response.Body.String(), "Run the mmhotline command with sudo.") {
		t.Fatal("command handler accepted a request without verified peer credentials")
	}
}

// A claim stops MMHotline contacting people, so on a server only root may
// claim or resolve through the command socket: never the triage agent's or
// the review agent's user, the triage user, or the mmhotline user that runs
// MMX for the agent, even when the request names root as its Unix user. Fails
// if the peer check lets any of them through.
func TestOnlyRootClaimsOrResolvesThroughTheCommandSocket(t *testing.T) {
	const mmhotlineUser = 990
	for _, body := range []string{
		`{"unix_user":"root","group":"incidents","action":"claim","incidents":{"incident_id":"4f2a","sign_in_email_address":"alex@example.com"}}`,
		`{"unix_user":"root","group":"incidents","action":"resolve","incidents":{"incident_id":"4f2a"}}`,
	} {
		for _, peer := range []struct {
			name    string
			uid     uint32
			allowed bool
		}{
			{"root", 0, true},
			{"mmhotline, which runs MMX for the agent", mmhotlineUser, false},
			{"mmhotline-triage-agent", 991, false},
			{"mmhotline-review-agent", 992, false},
			{"triage", 1001, false},
		} {
			reached := false
			handler := checkCommandPeer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			request := httptest.NewRequest("POST", "http://mmhotline/command", strings.NewReader(body))
			request = request.WithContext(context.WithValue(request.Context(), commandPeerContextKey{}, commandPeer{authorizeCommandPeer(peer.uid, mmhotlineUser, "linux")}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if reached != peer.allowed || !peer.allowed && response.Code != http.StatusForbidden {
				t.Fatalf("%s sent %s: reached the command handler %v, status %d", peer.name, body, reached, response.Code)
			}
		}
	}
}

func TestUnixSocketsArePrivateAndReplaceStaleSockets(t *testing.T) {
	path := filepath.Join(shortSocketDirectory(t), "command.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	listener, err := listenUnixSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode: %v %v", info, err)
	}
	regular := filepath.Join(filepath.Dir(path), "keep.txt")
	if err = os.WriteFile(regular, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := listenUnixSocket(regular); err == nil {
		listener.Close()
		t.Fatal("replaced a regular file")
	}
	content, err := os.ReadFile(regular)
	if err != nil || string(content) != "keep" {
		t.Fatal("non-socket file changed")
	}
}

// skipUnlessCommandSocketClientAllowed skips a test that sends commands over
// the command socket, which on Linux accepts only root.
func skipUnlessCommandSocketClientAllowed(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		t.Skip("command socket requires a root client on Linux")
	}
}

// skipUnlessSettingsFileOwnerAllowed skips a test that runs setup or serve
// with a settings file the test writes: on Linux the file must be owned by
// root, and on another system it may be owned by the user who runs it.
func skipUnlessSettingsFileOwnerAllowed(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		t.Skip("on Linux the settings file must be owned by root, so setup and serve need a root test process")
	}
}

func TestCLISetupCommandsEndToEnd(t *testing.T) {
	skipUnlessCommandSocketClientAllowed(t)
	directory := shortSocketDirectory(t)
	certsURL, signIn := cliAccessSignIn(t)
	state := filepath.Join(directory, "state")
	webAddress, intakeAddress, commandSocket := tcpListenAddress(t), tcpListenAddress(t), filepath.Join(directory, "command.sock")
	const signingSecret = "5f0c2d6e8a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
	settings := strings.Replace(testSettingsYAML("https://util01.example-tailnet.ts.net", webAddress, intakeAddress), `people:
  - name: Ana Silva
    sign_in_email_address: ana@example.com
    role: administrator`, `cloudflare_access:
  team_domain: example.cloudflareaccess.com
  audience_tag: test123
people:
  - name: Bob
    sign_in_email_address: BOB@example.com
    role: administrator
  - name: Ana Silva
    sign_in_email_address: ana@example.com
    role: member`, 1)
	settings = strings.Replace(settings, "rotation_sign_in_email_addresses: [ana@example.com]", "rotation_sign_in_email_addresses: [bob@example.com]", 1)
	// The sending services apply as serve starts, each through its own
	// check against the fake sending services.
	settings = strings.Replace(settings, "sending_services: {}", `sending_services:
  pushover:
    application_token: `+fakesendingservices.PushoverApplicationToken+`
  twilio:
    account_sid: `+fakesendingservices.TwilioAccountSID+`
    api_key_sid: `+fakesendingservices.TwilioAPIKeySID+`
    api_key_secret: `+fakesendingservices.TwilioAPIKeySecret+`
    phone_number_to_call_from: "`+fakesendingservices.TwilioPhoneNumberToCallFrom+`"
    extra_phone_numbers_on_the_contact_card: ["+12025550199"]
  telegram:
    bot_token: fake-telegram-bot-token`, 1)
	settings = strings.Replace(settings, "alert_sources: []", `alert_sources:
  - alert_source_id: monitoring
    name: Monitoring
    signing_secrets: ["`+signingSecret+`"]
    warn_after_no_check_in_for_minutes: 2
    sos_after_no_check_in_for_minutes: 5`, 1)
	settingsPath := writeTestSettingsFile(t, filepath.Join(directory, "mmhotline_settings.yml"), settings)
	output, err := cliProcess(t, "setup", "--state-dir", state, "--settings-file", settingsPath).CombinedOutput()
	if err != nil {
		t.Fatalf("setup: %s %v", output, err)
	}
	serve := cliProcess(t, "serve", "--state-dir", state, "--settings-file", settingsPath, "--command-socket", commandSocket)
	fake := httptest.NewServer(fakesendingservices.New())
	t.Cleanup(fake.Close)
	// serve tells systemd it is ready the way systemd's Type=notify expects.
	notifySocket := filepath.Join(directory, "notify.sock")
	notifications, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifySocket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer notifications.Close()
	serve.Env = append(serve.Env, "MMHOTLINE_FAKE_SENDING_SERVICES_URL="+fake.URL, "NOTIFY_SOCKET="+notifySocket, "MMHOTLINE_TEST_ACCESS_CERTS_URL="+certsURL)
	var serverOutput bytes.Buffer
	serve.Stdout = &serverOutput
	serve.Stderr = &serverOutput
	if err = serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if serve.ProcessState == nil {
			serve.Process.Signal(syscall.SIGTERM)
			serve.Wait()
		}
	})
	notifications.SetReadDeadline(time.Now().Add(5 * time.Second))
	notification := make([]byte, 64)
	length, err := notifications.Read(notification)
	if err != nil || string(notification[:length]) != "READY=1" {
		t.Fatalf("readiness notification: %q %v; server output: %s", notification[:length], err, serverOutput.String())
	}
	// Once ready, the first request reaches the server without waiting.
	client := tcpHTTPClient(webAddress)
	defer client.CloseIdleConnections()
	response, err := client.Get("http://mmhotline/people")
	if err != nil {
		t.Fatalf("web right after ready: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("web without identity: %d", response.StatusCode)
	}
	for _, path := range []string{commandSocket} {
		info, e := os.Stat(path)
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("socket %s mode: %v %v", path, info, e)
		}
	}
	people := func(args ...string) string {
		t.Helper()
		args = append([]string{"people"}, args...)
		args = append(args, "--command-socket", commandSocket)
		command := cliProcess(t, args...)
		command.Env = append(command.Env, "SUDO_USER=server-admin")
		output, e := command.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, output, e)
		}
		return string(output)
	}
	// The people of the settings file applied as serve started.
	var listed app.PeopleCommandResult
	if err = json.Unmarshal([]byte(people("list", "--json")), &listed); err != nil || len(listed.People) != 2 || listed.People[0].SignInEmailAddress != "ana@example.com" || listed.People[1].SignInEmailAddress != "bob@example.com" || listed.People[1].Role != "administrator" {
		t.Fatalf("list: %+v %v", listed, err)
	}
	request, _ := http.NewRequest("GET", "http://mmhotline/people", nil)
	var link app.CommandResult
	if err = json.Unmarshal([]byte(people("create-sign-in-link", "--sign-in-email-address", "bob@example.com", "--json")), &link); err != nil {
		t.Fatal(err)
	}
	linkURL, _ := url.Parse(link.SignInLinkURL)
	linkRequest, _ := http.NewRequest("GET", "http://mmhotline"+linkURL.Path, nil)
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	signedIn, err := noRedirect.Do(linkRequest)
	if err != nil {
		t.Fatal(err)
	}
	signedIn.Body.Close()
	if signedIn.StatusCode != 303 || len(signedIn.Cookies()) != 1 {
		t.Fatalf("sign-in link: %d", signedIn.StatusCode)
	}
	request.AddCookie(signedIn.Cookies()[0])
	// The recovery cookie works on the running TCP service without Access.
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("cookie sign-in web: %d", response.StatusCode)
	}
	signIn(request, "bob@example.com")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("Sign-in web: %d", response.StatusCode)
	}

	command := func(args ...string) string {
		t.Helper()
		args = append(args, "--command-socket", commandSocket)
		process := cliProcess(t, args...)
		process.Env = append(process.Env, "SUDO_USER=server-admin")
		output, err := process.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, output, err)
		}
		return string(output)
	}
	// An apply waits for the parts serve applies in the background at
	// start, and finds each unchanged.
	var applied app.CommandResult
	if output := command("settings", "apply", "--json"); json.Unmarshal([]byte(output), &applied) != nil || applied.SettingsFile == nil || len(applied.SettingsFile.FailedParts()) != 0 || strings.Contains(output, "fake-telegram-bot-token") || strings.Contains(output, fakesendingservices.TwilioAPIKeySecret) {
		t.Fatalf("settings apply: %s", output)
	}
	listOutput := command("sending-services", "list", "--json")
	var setupResult app.CommandResult
	if err = json.Unmarshal([]byte(listOutput), &setupResult); err != nil {
		t.Fatal(err)
	}
	setups := map[string]app.SendingServiceSetup{}
	for _, setup := range setupResult.SendingServiceSetups {
		setups[setup.SendingService] = setup
	}
	telegram, phoneSetup := setups["telegram"], setups["twilio"]
	if !telegram.IsSetUp || telegram.Settings["telegram_bot_username"] != fakesendingservices.TelegramBotUsername || telegram.SetUpBy != "the settings file "+settingsPath || strings.Contains(listOutput, "fake-telegram-bot-token") || !setups["pushover"].IsSetUp {
		t.Fatalf("sending services: %s", listOutput)
	}
	if !phoneSetup.IsSetUp || phoneSetup.Settings["twilio_account_sid"] != fakesendingservices.TwilioAccountSID || phoneSetup.Settings["twilio_api_key_sid"] != fakesendingservices.TwilioAPIKeySID || phoneSetup.Settings["twilio_phone_number_to_call_from"] != fakesendingservices.TwilioPhoneNumberToCallFrom || phoneSetup.Settings["extra_phone_numbers_on_the_contact_card"] != "+12025550199" || strings.Contains(listOutput, fakesendingservices.TwilioAPIKeySecret) {
		t.Fatalf("phone setup JSON: %s", listOutput)
	}
	command("contact-methods", "add", "--sign-in-email-address", "bob@example.com", "--contact-method-type", "automated_voice_call", "--phone-number-to-call", "+15555550123", "--alert-severities", "sos", "--json")
	pushoverUserKeyFile := filepath.Join(t.TempDir(), "pushover-user-key")
	if err = os.WriteFile(pushoverUserKeyFile, []byte(fakesendingservices.PushoverUserKey), 0o600); err != nil {
		t.Fatal(err)
	}
	pushover := command("contact-methods", "add", "--sign-in-email-address", "bob@example.com", "--contact-method-type", "pushover_notification", "--pushover-user-key-file", pushoverUserKeyFile, "--alert-severities", "sos,warn,info", "--json")
	var contactResult app.CommandResult
	if err = json.Unmarshal([]byte(pushover), &contactResult); err != nil || len(contactResult.ContactMethods) != 1 || contactResult.ContactMethods[0].LastTestMessage == nil {
		t.Fatalf("Pushover contact method: %s %v", pushover, err)
	}
	// The settings file's alert source signs with its own secret.
	rawSecret, err := hex.DecodeString(signingSecret)
	if err != nil {
		t.Fatal(err)
	}
	sources := command("alert-sources", "list", "--json")
	var sourceResult app.CommandResult
	if err = json.Unmarshal([]byte(sources), &sourceResult); err != nil || len(sourceResult.AlertSources) != 1 || sourceResult.AlertSources[0].AlertSourceID != "monitoring" || strings.Contains(sources, signingSecret) {
		t.Fatalf("alert sources: %s %v", sources, err)
	}
	body := []byte(`{"sender_alert_id":"setup-command-alert","severity":"info","title":"Monitoring connected","message":"Signed with the command-created secret"}`)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, rawSecret)
	mac.Write([]byte(stamp + "."))
	mac.Write(body)
	alertRequest, _ := http.NewRequest("POST", sourceResult.AlertSources[0].IntakeURL, bytes.NewReader(body))
	alertRequest.Header.Set("Content-Type", "application/json")
	alertRequest.Header.Set("Mmhotline-Signature", "timestamp_unix_seconds="+stamp+",hmac_sha256_hex="+hex.EncodeToString(mac.Sum(nil)))
	alertResponse, err := http.DefaultClient.Do(alertRequest)
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		AlertID string `json:"mmhotline_alert_id"`
	}
	decodeErr := json.NewDecoder(alertResponse.Body).Decode(&accepted)
	alertResponse.Body.Close()
	if alertResponse.StatusCode != 200 || decodeErr != nil || accepted.AlertID == "" {
		t.Fatalf("signed alert: HTTP %d %+v %v", alertResponse.StatusCode, accepted, decodeErr)
	}
	// The info alert opened no incident; the alert list shows it.
	var alerts app.CommandResult
	if listed := command("alerts", "list", "--alert-source-id", "monitoring", "--json"); json.Unmarshal([]byte(listed), &alerts) != nil || len(alerts.Alerts) != 1 || alerts.Alerts[0].AlertID != accepted.AlertID || alerts.Alerts[0].Severity != "info" || alerts.Alerts[0].IncidentID != "" {
		t.Fatalf("alert list: %s", listed)
	}
	if listed := command("incidents", "list"); listed != "Listed 0 SOS incidents not resolved, newest first.\n" {
		t.Fatalf("incident list: %q", listed)
	}
	shown := command("schedules", "show", "--json")
	var schedules app.CommandResult
	if err = json.Unmarshal([]byte(shown), &schedules); err != nil || schedules.ScheduleState == nil || schedules.ScheduleState.ActiveScheduleType != "weekly_rotation" || len(schedules.ScheduleState.ScheduleConfigurations) != 1 || schedules.ScheduleState.ScheduleConfigurations[0].RotationSignInEmailAddresses[0] != "bob@example.com" {
		t.Fatalf("schedule round trip: %s %v", shown, err)
	}
	if schedules.ScheduleState.EscalateIfUnclaimedAfterMinutes != 5 {
		t.Fatalf("time to claim before any change: %s", shown)
	}
	if err = serve.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err = serve.Wait(); err != nil {
		t.Fatalf("serve: %s %v", serverOutput.String(), err)
	}
	if !strings.Contains(serverOutput.String(), "Sign-in: Cloudflare Access for example.cloudflareaccess.com, and sign-in links") {
		t.Fatal("missing Access sign-in startup notice")
	}
	missing := filepath.Join(directory, "missing.sock")
	output, err = cliProcess(t, "people", "list", "--command-socket", missing).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 4 || !strings.Contains(string(output), "mmhotline serve is not running") || !strings.Contains(string(output), missing) {
		t.Fatalf("missing socket: %s %v", output, err)
	}
}

func tcpListenAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}
func tcpHTTPClient(address string) *http.Client {
	return &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, network, unused string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}}}
}

// serve needs a settings file, and with nothing applied yet refuses one
// with problems, naming each with its key path.
func TestServeNeedsASettingsFileItCanUse(t *testing.T) {
	output, err := cliProcess(t, "serve", "--state-dir", "unused", "--command-socket", "unused.sock").CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 4 || !strings.Contains(string(output), "--settings-file is required") {
		t.Fatalf("serve without a settings file: %s %v", output, err)
	}
	skipUnlessSettingsFileOwnerAllowed(t)
	good := testSettingsYAML("https://mmhotline.example.com", "127.0.0.1:0", "127.0.0.1:0")
	for _, test := range []struct {
		name, from, to, problem string
	}{
		{"invalid web address", "web_listen_address: \"127.0.0.1:0\"", "web_listen_address: localhost", "web_listen_address: must be a host:port address"},
		{"invalid intake address", "alert_intake_listen_address: \"127.0.0.1:0\"", "alert_intake_listen_address: localhost:65536", "alert_intake_listen_address: must be a host:port address"},
		{"web URL with a path", "web_url: https://mmhotline.example.com", "web_url: https://mmhotline.example.com/path", "web_url: must be an HTTPS origin"},
		{"domain alone", "team_name: Operations\n", "team_name: Operations\ncloudflare_access:\n  team_domain: example.cloudflareaccess.com\n", "cloudflare_access.audience_tag: required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := app.Setup(context.Background(), dir, "Fixture team"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(good, test.from) {
				t.Fatalf("the test settings have no %q", test.from)
			}
			path := writeTestSettingsFile(t, "", strings.Replace(good, test.from, test.to, 1))
			output, err := cliProcess(t, "serve", "--state-dir", dir, "--settings-file", path, "--command-socket", filepath.Join(shortSocketDirectory(t), "command.sock")).CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "MMHotline has never applied a settings file, so it cannot start") || !strings.Contains(string(output), test.problem) {
				t.Fatalf("serve: %s %v", output, err)
			}
		})
	}
}
