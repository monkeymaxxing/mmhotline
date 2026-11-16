package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmhotline/internal/triageagent"
)

// secretMarker is in the value of every text secret, and hexSecretMarker in
// every signing secret, so a test can look for any secret in what MMHotline
// shows.
const (
	secretMarker    = "Zq7SECRET"
	hexSecretMarker = "c0ffeec0ffee"
)

// settingsWithEverySecret is a settings file with every section and a
// marked value in every secret key. With broken, each secret's value is
// refused, so the check reports a problem at each secret key.
func settingsWithEverySecret(t *testing.T, broken bool) string {
	text := func(name string) string {
		if broken {
			return `"` + secretMarker + name + `\a"`
		}
		return `"` + secretMarker + name + `"`
	}
	// prefixed is a marked token in its service's form, such as Slack's
	// xoxb- prefix, so only the broken one is refused.
	prefixed := func(prefix, name string) string {
		if broken {
			return `"` + prefix + secretMarker + name + `\a"`
		}
		return `"` + prefix + secretMarker + name + `"`
	}
	pushover := secretMarker + "pushover" + strings.Repeat("0", 30-len(secretMarker)-len("pushover"))
	if broken {
		pushover += "!"
	}
	signingSecret := strings.Repeat(hexSecretMarker, 5) + "c0ff"
	if broken {
		signingSecret = secretMarker + "signing"
	}
	return replaced(t, exampleSettingsYAML,
		`credential: "sk-ant-oat01-example-triage-agent-credential"`, "credential: "+text("triage"),
		`credential: "sk-example-review-agent-credential"`, "credential: "+text("review"),
		`    application_token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi"`, `    application_token: "`+pushover+`"
  twilio:
    account_sid: AC00000000000000000000000000000001
    api_key_sid: SK00000000000000000000000000000001
    api_key_secret: `+text("twilio")+`
    phone_number_to_call_from: "+12025550102"
    extra_phone_numbers_on_the_contact_card: []
  telegram:
    bot_token: `+prefixed("123456789:", "telegram000")+`
  slack:
    bot_token: `+prefixed("xoxb-", "slackbot")+`
    app_level_token: `+prefixed("xapp-", "slackapp")+`
  mail_server:
    smtp_server: 127.0.0.1:1
    smtp_connection_security: starttls
    smtp_user_name: alerts
    smtp_password: `+text("smtp")+`
    email_from_address: alerts@example.com`,
		`cloudflare_api_token: "example-cloudflare-workers-token"`, "cloudflare_api_token: "+text("cloudflare"),
		`r2_access_key_id: "0123456789abcdef0123456789abcdef"`, "r2_access_key_id: "+text("r2key"),
		`r2_secret_access_key: "example-r2-secret-access-key"`, "r2_secret_access_key: "+text("r2secret"),
		`signing_secrets: ["`+exampleMonitoringSecret+`"]`, `signing_secrets: ["`+signingSecret+`"]`)
}

// secretKeyPaths walks the settings and returns the key path of every
// secret, failing when a secret's value lacks its marker or a section is
// left out, so a new secret key fails this test until it is covered.
func secretKeyPaths(t *testing.T, value reflect.Value, path string) []string {
	t.Helper()
	var paths []string
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() && value.Type().Elem().Kind() == reflect.Struct {
			t.Fatalf("section %s is left out of the settings with every secret", path)
		}
		if value.IsNil() {
			return nil
		}
		return secretKeyPaths(t, value.Elem(), path)
	case reflect.Struct:
		for index := range value.NumField() {
			field := value.Type().Field(index)
			name, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			fieldPath := joinKeyPath(path, name)
			if options == "inline" {
				fieldPath = path
			}
			if slices.Contains(strings.Split(field.Tag.Get("settings"), ","), "secret") {
				values := []string{fmt.Sprint(value.Field(index).Interface())}
				if field.Type.Kind() == reflect.Slice {
					values = value.Field(index).Interface().([]string)
				}
				for _, secret := range values {
					if !strings.Contains(secret, secretMarker) && !strings.Contains(secret, hexSecretMarker) {
						t.Fatalf("secret %s holds %q, without a marker", fieldPath, secret)
					}
				}
				paths = append(paths, fieldPath)
				continue
			}
			paths = append(paths, secretKeyPaths(t, value.Field(index), fieldPath)...)
		}
	case reflect.Slice:
		for index := range value.Len() {
			paths = append(paths, secretKeyPaths(t, value.Index(index), fmt.Sprintf("%s[%d]", path, index))...)
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			paths = append(paths, secretKeyPaths(t, value.MapIndex(key), joinKeyPath(path, key.String()))...)
		}
	default:
		// Booleans, numbers and the other kinds hold no secret keys.
	}
	return paths
}

// showsNoSecret fails when what holds any secret's marker.
func showsNoSecret(t *testing.T, where, what string) {
	t.Helper()
	for _, marker := range []string{secretMarker, hexSecretMarker} {
		if strings.Contains(what, marker) {
			t.Errorf("%s shows a secret:\n%s", where, what)
		}
	}
}

// TestNoOutputShowsASecretValue puts a recognizable value in every secret
// key and looks for it in the problems of settings check, the output of
// settings apply and show, the pages, the commands, the log, and the
// warning at start (confirmed change 3).
func TestNoOutputShowsASecretValue(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	ctx := context.Background()

	file, problems := ReadSettingsFile(writeSettingsFile(t, settingsWithEverySecret(t, false)), nil)
	if len(problems) != 0 {
		t.Fatalf("problems in the settings with every secret: %q", problemTexts(problems))
	}
	secretPaths := secretKeyPaths(t, reflect.ValueOf(file.Settings), "")
	if len(secretPaths) != 12 {
		t.Fatalf("secret keys %q", secretPaths)
	}

	// settings check: a problem at each secret key, and none shows its value.
	_, problems = ReadSettingsFile(writeSettingsFile(t, settingsWithEverySecret(t, true)), nil)
	var problemPaths []string
	for _, problem := range problems {
		problemPaths = append(problemPaths, problem.KeyPath)
		showsNoSecret(t, "settings check", problem.String())
	}
	for _, path := range secretPaths {
		if !slices.ContainsFunc(problemPaths, func(problemPath string) bool {
			return problemPath == path || strings.HasPrefix(problemPath, path+"[") || strings.HasPrefix(path, problemPath+".")
		}) {
			t.Errorf("no problem at %s: %q", path, problemTexts(problems))
		}
	}

	// settings apply and show, the commands, and the pages. Each part with
	// an outside test runs it against the fakes, which refuse the marked
	// values, and the fake sandbox quotes the credential back in its
	// answer, as a coding agent may.
	f := newSettingsFileFixture(t, settingsWithEverySecret(t, false))
	f.answerRuns(func(request triageagent.Request) triageagent.Event {
		return triageagent.Event{Event: "failed", Reason: "Invalid API key: " + request.Credential}
	})
	f.start()
	configureAccessSignIn(t, f.a)
	view := f.apply()
	for _, part := range []string{triageAgentSetupPartName, "review_agent", "sending_services.pushover", "sending_services.twilio", "sending_services.telegram", "sending_services.slack", "sending_services.mail_server", "mmhotline_down_alarm", "status_page"} {
		if !strings.HasPrefix(states(view)[part], "failed: ") {
			t.Errorf("%s ran no outside test that refused the marked values: %q", part, states(view)[part])
		}
	}
	if !strings.Contains(states(view)["review_agent"], "Invalid API key: [credential removed]") {
		t.Errorf("the review agent's reason: %q", states(view)["review_agent"])
	}
	encoded, _ := json.Marshal(view)
	showsNoSecret(t, "settings apply", view.Text()+string(encoded))
	shown, err := f.a.SettingsFileState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(shown)
	showsNoSecret(t, "settings show", shown.Text()+string(encoded))
	var stored string
	if err := f.a.Store.db.QueryRowContext(ctx, `SELECT group_concat(applied_values||' '||failure_reason||' '||notes,' ') FROM settings_file_parts`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	showsNoSecret(t, "settings_file_parts", stored)
	var runs string
	if err := f.a.Store.db.QueryRowContext(ctx, `SELECT coalesce(group_concat(answer||' '||failure_reason,' '),'')||' '||coalesce((SELECT group_concat(text,' ') FROM triage_agent_run_steps),'') FROM triage_agent_runs`).Scan(&runs); err != nil || !strings.Contains(runs, "[credential removed]") {
		t.Fatalf("the agents' runs %q, %v", runs, err)
	}
	showsNoSecret(t, "the agents' runs", runs)
	// A reason that quotes one of the file's secrets back, as an outside
	// service may, shows it removed.
	quoting := settingsFilePart{name: "mmhotline_down_alarm", value: fileValue(func(s Settings) any { return s.DownAlarm }), apply: func(_ context.Context, _ *App, s Settings) error {
		return fmt.Errorf("%w: Cloudflare answered: token "+s.DownAlarm.CloudflareAPIToken+" is unknown", ErrInvalid)
	}}
	f.write(replaced(t, settingsWithEverySecret(t, false), "Zq7SECRETcloudflare", "Zq7SECRETcloudflare-quoted"))
	file, _ = ReadSettingsFile(f.path, nil)
	outcomes, err := f.a.applySettingsParts(ctx, file.Settings, nil, []settingsFilePart{quoting})
	if err != nil || len(outcomes) != 1 || outcomes[0].FailureReason != "Cloudflare answered: token [secret removed] is unknown" {
		t.Fatalf("a reason that quotes a secret: %+v %v", outcomes, err)
	}
	f.write(settingsWithEverySecret(t, false))
	commands := httptest.NewServer(f.a.CommandHandler())
	defer commands.Close()
	for _, request := range []CommandRequest{
		{Group: "settings", Action: "show"}, {Group: "settings", Action: "apply"}, {Group: "alert-sources", Action: "list"},
		{Group: "people", Action: "list"}, {Group: "triage-agent", Action: "show"}, {Group: "agent-playbook-rules", Action: "show"},
		{Group: "review-agent", Action: "show"}, {Group: "sending-services", Action: "list"}, {Group: "down-alarm", Action: "show"}, {Group: "status-page", Action: "show"},
	} {
		raw, _ := json.Marshal(request)
		response, err := http.Post(commands.URL+"/command", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", request.Group, request.Action, response.StatusCode, body)
		}
		showsNoSecret(t, request.Group+" "+request.Action, string(body))
	}
	pages := httptest.NewTLSServer(f.a.Handler())
	defer pages.Close()
	client := pages.Client()
	client.Jar, _ = cookiejar.New(nil)
	useSignInEmailAddress(client, "ana@example.com")
	for _, page := range []string{"/dashboard", "/people", "/on-call", "/alert-sources", "/alert-sources/monitoring", "/agent-playbook-rules", "/triage-agent", "/sending-services", "/sending-services/pushover", "/sending-services/twilio", "/sending-services/telegram", "/sending-services/slack", "/sending-services/mail_server", "/status-page"} {
		response, err := client.Get(pages.URL + page)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !strings.Contains(string(body), "Set in <code>"+f.path+"</code>") && page != "/dashboard" && page != "/triage-agent" {
			t.Fatalf("%s: %d\n%s", page, response.StatusCode, body)
		}
		showsNoSecret(t, page, string(body))
	}

	// The warning at a start with a broken file, and the first start's refusal.
	f.write(settingsWithEverySecret(t, true))
	f.restart()
	f.start()
	var message string
	if err := f.a.Store.db.QueryRowContext(ctx, `SELECT group_concat(title||' '||message,' ') FROM alerts WHERE source_id='system'`).Scan(&message); err != nil || !strings.Contains(message, "triage_agent.credential") {
		t.Fatalf("warning %q, %v", message, err)
	}
	showsNoSecret(t, "the warning at start", message)
	first := newSettingsFileFixture(t, settingsWithEverySecret(t, true))
	_, err = first.a.LoadSettingsFileAtStart(ctx)
	if err == nil {
		t.Fatal("the first start accepted a broken file")
	}
	showsNoSecret(t, "the first start's refusal", err.Error())
	showsNoSecret(t, "the log", logged.String())
}
