package main

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmhotline/internal/app"
	"mmhotline/internal/fakesendingservices"
)

// connectPushoverServe is a running mmhotline serve, with the fake
// sending services, for connect-pushover to talk to.
type connectPushoverServe struct {
	settingsPath, socket string
}

// startConnectPushoverServe starts serve with the settings file whose
// people are people, and no sending service yet.
func startConnectPushoverServe(t *testing.T, people string) connectPushoverServe {
	t.Helper()
	skipUnlessCommandSocketClientAllowed(t)
	skipUnlessSettingsFileOwnerAllowed(t)
	directory := shortSocketDirectory(t)
	settings := strings.Replace(testSettingsYAML("http://localhost:8480", tcpListenAddress(t), tcpListenAddress(t)), `people:
  - name: Ana Silva
    sign_in_email_address: ana@example.com
    role: administrator`, "people:\n"+people, 1)
	serve := connectPushoverServe{settingsPath: writeTestSettingsFile(t, filepath.Join(directory, "mmhotline_settings.yml"), settings), socket: filepath.Join(directory, "command.sock")}
	state := filepath.Join(directory, "state")
	if err := app.Setup(t.Context(), state, "Operations"); err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(fakesendingservices.New())
	t.Cleanup(fake.Close)
	process := cliProcess(t, "serve", "--state-dir", state, "--settings-file", serve.settingsPath, "--command-socket", serve.socket)
	process.Env = append(process.Env, "MMHOTLINE_FAKE_SENDING_SERVICES_URL="+fake.URL)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.Process.Signal(syscall.SIGTERM); process.Wait() })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := sendCommand(serve.socket, app.CommandRequest{Group: "settings", Action: "show", UnixUser: "root"}); err == nil {
			return serve
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not start")
		}
	}
}

const onePerson = `  - name: Ana Silva
    sign_in_email_address: ana@example.com
    role: administrator`

const twoPeople = onePerson + `
  - name: Bob Example
    sign_in_email_address: bob@example.com
    role: member`

type connectPushoverRun struct {
	setSecretRun
	settingsPath string
}

// connectPushoverAt runs connect-pushover against serve with a person's
// answers, one a line, at a terminal.
func (serve connectPushoverServe) run(t *testing.T, answers ...string) *connectPushoverRun {
	t.Helper()
	run := &connectPushoverRun{settingsPath: serve.settingsPath}
	terminals := setSecretTerminalsFor(t, &run.setSecretRun, false, strings.Join(answers, "\n")+"\n")
	terminals.StdinIsTerminal = true
	terminals.TurnOffEcho = func(*os.File) (func(), bool) { return func() {}, true }
	run.err = connectPushover([]string{"--settings-file", serve.settingsPath, "--command-socket", serve.socket}, terminals)
	return run
}

// pushoverContactMethods are the Pushover contact methods of the person
// with address, as contact-methods list shows them.
func (serve connectPushoverServe) pushoverContactMethods(t *testing.T, address string) []app.CommandContactMethod {
	t.Helper()
	listed, err := sendCommand(serve.socket, app.CommandRequest{Group: "contact-methods", Action: "list", UnixUser: "root", ContactMethods: app.ContactMethodsCommandRequest{SignInEmailAddress: address}})
	if err != nil {
		t.Fatal(err)
	}
	var found []app.CommandContactMethod
	for _, method := range listed.ContactMethods {
		if method.ContactMethodType == "pushover_notification" {
			found = append(found, method)
		}
	}
	return found
}

const (
	emailQuestion  = "Your sign-in email address: "
	deviceQuestion = "Send alerts to which device? Press Enter for all devices: "
)

func TestConnectPushoverRefusesWithoutATerminalAndChangesNothing(t *testing.T) {
	path := writeTestSettingsFile(t, "", setSecretSourceFile)
	run := &setSecretRun{}
	terminals := setSecretTerminalsFor(t, run, false, fakesendingservices.PushoverApplicationToken+"\n")
	run.err = connectPushover([]string{"--settings-file", path, "--command-socket", "/nonexistent.sock"}, terminals)
	var refused commandError
	if !errors.As(run.err, &refused) || run.err.Error() != connectPushoverNeedsATerminal {
		t.Fatalf("err=%v", run.err)
	}
	if readFile(t, path) != setSecretSourceFile || run.stdout.Len() != 0 || run.prompt.Len() != 0 {
		t.Fatalf("output %q %q", run.stdout.String(), run.prompt.String())
	}
}

// One person and one device: no question but the two hidden ones.
func TestConnectPushoverWithOnePersonAndOneDeviceAsksOnlyForTheTwoSecrets(t *testing.T) {
	serve := startConnectPushoverServe(t, onePerson)
	run := serve.run(t, fakesendingservices.PushoverApplicationToken, fakesendingservices.PushoverOneDeviceUserKey)
	if run.err != nil {
		t.Fatalf("%v\n%s", run.err, run.everything())
	}
	if strings.Contains(run.prompt.String(), emailQuestion) || strings.Contains(run.prompt.String(), deviceQuestion) {
		t.Fatalf("asked more than the secrets: %s", run.prompt.String())
	}
	if !strings.Contains(run.prompt.String(), "Pushover application token (nothing shows as you paste): ") || !strings.Contains(run.prompt.String(), "Your Pushover user key (nothing shows as you paste): ") {
		t.Fatalf("prompts: %s", run.prompt.String())
	}
	if !strings.Contains(run.stdout.String(), "Adding the phone of Ana Silva (ana@example.com), the one person in the settings file.") {
		t.Fatalf("output: %s", run.stdout.String())
	}
	if !strings.Contains(readFile(t, serve.settingsPath), fakesendingservices.PushoverApplicationToken) {
		t.Fatal("the token is not in the settings file")
	}
	methods := serve.pushoverContactMethods(t, "ana@example.com")
	if len(methods) != 1 || !strings.HasSuffix(methods[0].SendsTo, "ending in ey01") {
		t.Fatalf("contact methods: %+v", methods)
	}
}

// Several people and devices: the sign-in email address and the device are
// asked; an unknown answer is refused and asked again.
func TestConnectPushoverWithSeveralPeopleAndDevices(t *testing.T) {
	serve := startConnectPushoverServe(t, twoPeople)
	token := fakesendingservices.PushoverApplicationToken

	t.Run("a token Pushover refuses stops before the phone", func(t *testing.T) {
		run := serve.run(t, "FakePushoverApplicationToken99", "bob@example.com", fakesendingservices.PushoverUserKey)
		if run.err == nil || !strings.Contains(run.stdout.String(), "Pushover refused the application token, so MMHotline did not start using it.") {
			t.Fatalf("err=%v\n%s", run.err, run.everything())
		}
		if strings.Contains(run.prompt.String(), emailQuestion) || len(serve.pushoverContactMethods(t, "bob@example.com")) != 0 {
			t.Fatalf("went on after the refused token: %s", run.prompt.String())
		}
	})

	t.Run("a named device is saved; unknown answers are asked again", func(t *testing.T) {
		run := serve.run(t, token, "nobody@example.com", "BOB@example.com", fakesendingservices.PushoverUserKey, "laptop", "tablet")
		if run.err != nil {
			t.Fatalf("%v\n%s", run.err, run.everything())
		}
		if strings.Count(run.prompt.String(), emailQuestion) != 2 || strings.Count(run.prompt.String(), deviceQuestion) != 2 {
			t.Fatalf("prompts: %s", run.prompt.String())
		}
		for _, want := range []string{
			`Nobody in the settings file's people has the sign-in email address "nobody@example.com". The sign-in email addresses are: ana@example.com, bob@example.com.`,
			"Your Pushover account has these devices: phone, tablet.",
			`Pushover lists no device named "laptop". Type one of: phone, tablet, or press Enter for all devices.`,
			"Your phone is added. A quiet test notification is on its way to it now.",
		} {
			if !strings.Contains(run.stdout.String(), want) {
				t.Fatalf("missing %q in:\n%s", want, run.stdout.String())
			}
		}
		// The person reads plain words, not the contact method's fields.
		if strings.Contains(run.stdout.String(), "Contact method type:") || strings.Contains(run.stdout.String(), "queued") {
			t.Fatalf("connect-pushover printed the contact method's fields:\n%s", run.stdout.String())
		}
		methods := serve.pushoverContactMethods(t, "bob@example.com")
		if len(methods) != 1 || !strings.HasSuffix(methods[0].SendsTo, "ending in y001 on device tablet") {
			t.Fatalf("contact methods: %+v", methods)
		}
	})

	t.Run("a re-run reports the phone already added and adds no second one", func(t *testing.T) {
		run := serve.run(t, token, "bob@example.com", fakesendingservices.PushoverUserKey, "")
		if run.err != nil || !strings.Contains(run.stdout.String(), "Bob Example already has a Pushover contact method with this user key, so MMHotline kept it as it is.") {
			t.Fatalf("err=%v\n%s", run.err, run.everything())
		}
		if methods := serve.pushoverContactMethods(t, "bob@example.com"); len(methods) != 1 {
			t.Fatalf("contact methods: %+v", methods)
		}
	})

	t.Run("Enter means all devices", func(t *testing.T) {
		run := serve.run(t, token, "ana@example.com", fakesendingservices.PushoverOtherUserKey, "")
		if run.err != nil {
			t.Fatalf("%v\n%s", run.err, run.everything())
		}
		methods := serve.pushoverContactMethods(t, "ana@example.com")
		if len(methods) != 1 || !strings.HasSuffix(methods[0].SendsTo, "ending in y002") {
			t.Fatalf("contact methods: %+v", methods)
		}
	})

	t.Run("a user key Pushover refuses says the token is saved", func(t *testing.T) {
		const unknownKey = "FakePushoverPersonalUserKey999"
		run := serve.run(t, token, "ana@example.com", unknownKey)
		if run.err == nil || !strings.Contains(run.err.Error(), "Pushover refused the user key: user identifier is invalid") || !strings.Contains(run.err.Error(), connectPushoverTokenIsSaved) {
			t.Fatalf("err=%v\n%s", run.err, run.everything())
		}
		if strings.Contains(run.prompt.String(), deviceQuestion) || len(serve.pushoverContactMethods(t, "ana@example.com")) != 1 {
			t.Fatalf("went on after the refused key: %s", run.prompt.String())
		}
		if strings.Contains(run.everything(), unknownKey) || strings.Contains(run.everything(), token) {
			t.Fatalf("a secret is in the output: %s", run.everything())
		}
	})
}
