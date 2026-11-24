package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"mmhotline/internal/app"
	"mmhotline/internal/cloudflareaccess"
	"mmhotline/internal/triageagent"
)

type commandError struct{ error }
type reportedFlagError struct{ error }

// checkAnswer ends a check command that printed its answer; the answer is
// the exit code: 1 the check failed, 3 the check cannot tell.
type checkAnswer struct{ exitCode int }

func (answer checkAnswer) Error() string {
	return fmt.Sprintf("the check answered with exit code %d", answer.exitCode)
}

func (e commandError) Unwrap() error { return e.error }

var errInterrupted = errors.New("interrupted")

// version is set at release build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// buildDescription is what `mmhotline version` prints: the release version and
// the Git revision Go recorded in the build, as MMX and the monitoring tool print theirs.
type buildDescription struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
	Modified bool   `json:"modified"`
	Go       string `json:"go"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

func describeBuild() buildDescription {
	build := buildDescription{Version: version, Revision: "unknown", Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				build.Revision = setting.Value
			case "vcs.modified":
				build.Modified = setting.Value == "true"
			}
		}
	}
	return build
}

func runVersion(args []string) error {
	if len(args) == 1 && args[0] == "--json" {
		return json.NewEncoder(os.Stdout).Encode(describeBuild())
	}
	if len(args) != 0 {
		return commandError{errors.New("usage: mmhotline version [--json]")}
	}
	build := describeBuild()
	fmt.Printf("mmhotline %s (%s, %s/%s, modified=%v)\n", build.Version, build.Revision, build.OS, build.Arch, build.Modified)
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errInterrupted) {
		return 130
	}
	var command commandError
	if errors.As(err, &command) {
		return 4
	}
	var answer checkAnswer
	if errors.As(err, &answer) {
		return answer.exitCode
	}
	return 1
}
func main() {
	// The hidden subcommand that runs custom schedule code in a sandbox; the
	// service starts it, never a person.
	if len(os.Args) == 2 && os.Args[1] == app.CustomScheduleSandboxCommand {
		os.Exit(app.RunCustomScheduleSandbox(os.Stdin, os.Stdout, os.Stderr))
	}
	// The commands the triage agent runs inside its run have their own exit
	// codes, which their --help states for the agent.
	if len(os.Args) >= 3 && os.Args[1] == "triage-agent" && triageagent.IsAgentRequestCommand(os.Args[2]) {
		os.Exit(triageagent.RunAgentRequestCommand(os.Args[2], os.Args[3:], os.Stdout, os.Stderr))
	}
	// SSH runs this one as the agent's ProxyCommand and expects 255 on failure.
	if len(os.Args) >= 3 && os.Args[1] == "triage-agent" && os.Args[2] == triageagent.SSHProxyAction {
		os.Exit(triageagent.RunSSHProxyCommand(os.Args[3:], os.Stdin, os.Stdout, os.Stderr))
	}
	// The agent's ssh runs this one with ssh's own arguments, and it becomes
	// the real ssh.
	if len(os.Args) >= 3 && os.Args[1] == "triage-agent" && os.Args[2] == triageagent.SSHAction {
		os.Exit(triageagent.RunSSHCommand(os.Args[3:], os.Stderr))
	}
	err := run(os.Args[1:])
	var reported reportedFlagError
	var answer checkAnswer
	if err != nil && !errors.Is(err, errInterrupted) && !errors.As(err, &reported) && !errors.As(err, &answer) {
		fmt.Fprintf(os.Stderr, "mmhotline: %s\n", err)
	}
	os.Exit(exitCode(err))
}

const usage = "usage: mmhotline install\n       mmhotline setup --state-dir DIR --settings-file FILE\n       mmhotline serve --state-dir DIR --settings-file FILE --command-socket PATH\n       mmhotline settings check --settings-file FILE [--json]\n       mmhotline settings apply|show [--command-socket PATH] [--json]\n       mmhotline triage-agent show|check-sign-in|run-software-review [--command-socket PATH] [--json]\n       mmhotline triage-agent usage --since TIME [--command-socket PATH] [--json]\n       sudo mmhotline triage-agent check-preparation [--json]\n       mmhotline triage-agent request-playbook-run|report|review-commit [--json] (inside a triage agent run only)\n       mmhotline triage-agent ssh-proxy HOST PORT (inside a triage agent run only; SSH runs it)\n       mmhotline triage-agent ssh SSH_ARGUMENTS (inside a triage agent run only; the agent's ssh runs it)\n       mmhotline review-agent show|check-sign-in [--command-socket PATH] [--json]\n       mmhotline agent-playbook-rules show|check-whether-a-playbook-needs-a-person [--command-socket PATH] [--json]\n       mmhotline sending-services list [--command-socket PATH] [--json]\n       sudo mmhotline connect-pushover [--settings-file FILE] [--command-socket PATH]\n       mmhotline people list|create-sign-in-link [--command-socket PATH] [--json]\n       mmhotline contact-methods list|add|change|remove|send-test [--command-socket PATH] [--json]\n       mmhotline alert-sources list [--command-socket PATH] [--json]\n       mmhotline schedules show|preview [--command-socket PATH] [--json]\n       mmhotline down-alarm show|send-test [--command-socket PATH] [--json]\n       mmhotline status-page show|post|resolve|edit|delete [--command-socket PATH] [--json]\n       mmhotline status-page set-up --cloudflare-account-id ID --host-name HOST --cloudflare-api-token-file FILE|- [--json] (on a laptop)\n       mmhotline incidents list|show|send-test-sos|claim|resolve [--command-socket PATH] [--json]\n       mmhotline alerts list [--command-socket PATH] [--json]\n       mmhotline triage-sessions list|show [--command-socket PATH] [--json]\n       mmhotline approval-requests list|show [--command-socket PATH] [--json]\n       mmhotline commit-reviews check|trust-through|mark-reviewed|list|show [--command-socket PATH] [--json]\n       mmhotline backup check-uploads [--json]\n       mmhotline version [--json]\n       mmhotline --help\n       mmhotline COMMAND --help"

// settingsFileOwners are the owners a settings file may have: root on a
// server, which runs MMHotline only on Linux, and on a development machine
// also the developer, as the command socket allows (CheckSettingsFileOwnerAndMode).
func settingsFileOwners() []uint32 {
	if runtime.GOOS == "linux" {
		return []uint32{0}
	}
	return []uint32{0, uint32(os.Geteuid())}
}

func run(args []string) error {
	if len(args) >= 2 && args[0] == "triage-agent" && args[1] == "run-inside-sandbox" {
		return runTriageAgentInsideSandbox(args[2:])
	}
	if len(args) > 0 && args[0] == "run-fleet-view-command" {
		return runFleetViewCommand(args[1:])
	}
	if len(args) > 0 && args[0] == "install" {
		return runInstall(args[1:])
	}
	if len(args) > 0 && args[0] == "version" {
		return runVersion(args[1:])
	}
	// Runs as root, reads the triage agent's home folder, and uses no command
	// socket for its checks, like backup check-uploads.
	if len(args) >= 2 && args[0] == "triage-agent" && args[1] == "check-preparation" {
		return runTriageAgentCheckPreparation(args[2:])
	}
	if len(args) > 0 && args[0] == "backup" {
		return runBackup(args[1:])
	}
	if len(args) > 0 && args[0] == "settings" {
		return runSettings(args[1:])
	}
	if len(args) > 0 && args[0] == "connect-pushover" {
		return connectPushover(args[1:], realSetSecretTerminals())
	}
	if len(args) > 0 && commandActions[args[0]] != nil {
		return runManagementCommand(args[0], args[1:])
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-help") {
		fmt.Println(usage)
		return nil
	}
	if len(args) == 0 || (args[0] != "setup" && args[0] != "serve") {
		return commandError{errors.New(usage)}
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: mmhotline %s --state-dir DIR --settings-file FILE", args[0])
		if args[0] == "serve" {
			fmt.Fprint(flags.Output(), " --command-socket PATH [--triage-agent-run-socket PATH --review-agent-run-socket PATH --triage-agent-proxy-socket PATH] [--fleet-view-socket PATH]")
		}
		fmt.Fprintln(flags.Output(), "\n       --help  Show help")
	}
	dir := flags.String("state-dir", "", "directory for MMHotline's database and state")
	settingsPath := flags.String("settings-file", "", "the settings file, such as "+app.InstalledSettingsFilePath)
	commandSocket := ""
	triageAgentRunSocket, reviewAgentRunSocket, triageAgentProxySocket, fleetViewSocket := "", "", "", ""
	if args[0] == "serve" {
		flags.StringVar(&fleetViewSocket, "fleet-view-socket", "", "systemd socket that runs the settings file's fleet_view_command as root for the dashboard's servers")
		flags.StringVar(&triageAgentRunSocket, "triage-agent-run-socket", "", "triage agent systemd run socket; requires --review-agent-run-socket and --triage-agent-proxy-socket")
		flags.StringVar(&reviewAgentRunSocket, "review-agent-run-socket", "", "review agent systemd run socket; requires --triage-agent-run-socket and --triage-agent-proxy-socket")
		flags.StringVar(&triageAgentProxySocket, "triage-agent-proxy-socket", "", "triage agent logging proxy socket; requires --triage-agent-run-socket and --review-agent-run-socket")
		flags.StringVar(&commandSocket, "command-socket", "", "required local command Unix socket")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stderr, flagOutput.String())
			return nil
		}
		fmt.Fprintf(os.Stderr, "mmhotline: %s", flagOutput.String())
		return commandError{reportedFlagError{err}}
	}
	if *dir == "" {
		return commandError{errors.New("--state-dir is required")}
	}
	if *settingsPath == "" {
		return commandError{errors.New("--settings-file is required")}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("unexpected positional arguments")}
	}
	if args[0] == "setup" {
		file, problems := app.ReadSettingsFile(*settingsPath, settingsFileOwners())
		if len(problems) > 0 {
			return fmt.Errorf("the settings file %s has problems:\n%s", *settingsPath, settingsProblemLines(problems))
		}
		if err := app.Setup(context.Background(), *dir, file.Settings.TeamName); err != nil {
			return err
		}
		fmt.Printf("Created MMHotline state in %s.\n", *dir)
		return nil
	}
	if commandSocket == "" {
		return commandError{errors.New("--command-socket is required")}
	}
	if (triageAgentRunSocket == "") != (triageAgentProxySocket == "") || (triageAgentRunSocket == "") != (reviewAgentRunSocket == "") {
		return commandError{errors.New(app.TriageAgentSocketFlagsTogether)}
	}
	contactMethodTypes, fakeURL, err := contactMethodTypeConfiguration()
	if err != nil {
		return commandError{err}
	}
	store, err := app.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	application := app.New(store)
	application.UseSettingsFile(*settingsPath, settingsFileOwners())
	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	server, err := application.LoadSettingsFileAtStart(ctx)
	if err != nil {
		return err
	}
	webListener, intakeListener, err := listenServerSettings(server)
	if err != nil {
		// An address the file names but this server cannot open, such as
		// one it does not have, would stop alerts: serve starts with the
		// server settings it last applied instead, and warns.
		fallback, found, fallbackErr := application.UseLastAppliedServerSettings(ctx, err.Error())
		if fallbackErr != nil || !found {
			return errors.Join(err, fallbackErr)
		}
		server = fallback
		if webListener, intakeListener, err = listenServerSettings(server); err != nil {
			return err
		}
	}
	defer webListener.Close()
	defer intakeListener.Close()
	var verifier *cloudflareaccess.Verifier
	if access := server.CloudflareAccess; access != nil {
		if verifier, err = cloudflareaccess.NewVerifier(access.TeamDomain, access.AudienceTag); err != nil {
			return err
		}
	}
	// ctx ends at a signal, so a triage agent run's start that waits for the
	// systems repository's lock, or a finished run's commits that wait for
	// it, never hold up the stop.
	if err := application.InitializeTriageAgent(ctx, triageAgentRunSocket, reviewAgentRunSocket, triageAgentProxySocket); err != nil {
		return err
	}
	defer application.CloseTriageAgent()
	if err := application.LoadSendingServiceSetups(context.Background(), contactMethodTypes, fakeURL); err != nil {
		return err
	}
	application.WebURL = server.WebURL
	application.WebListenAddress = server.WebListenAddress
	application.AccessVerifier = verifier
	application.AlertIntakeListenAddress = server.AlertIntakeListenAddress
	application.FleetViewSocket = fleetViewSocket
	if fakeURL != "" {
		app.ShowDevelopmentModeNotice(fakeURL)
	}
	commandListener, err := listenUnixSocket(commandSocket)
	if err != nil {
		return err
	}
	defer commandListener.Close()
	var proxyListener net.Listener
	if triageAgentProxySocket != "" {
		proxyListener, err = listenUnixSocket(triageAgentProxySocket)
		if err != nil {
			return err
		}
		defer proxyListener.Close()
		// /run/mmhotline is 0700: only the sandbox's bind mount reaches this 0666 socket.
		if err = os.Chmod(triageAgentProxySocket, 0666); err != nil {
			return err
		}
	}
	listeners := []net.Listener{webListener, commandListener, intakeListener}
	newServer := func(handler http.Handler) *http.Server {
		return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	}
	commandServer := newServer(checkCommandPeer(application.CommandHandler()))
	commandServer.ConnContext = commandConnectionContext
	// settings apply waits for an earlier apply and for the parts' outside
	// tests; only root reaches this socket, so it gets no write deadline.
	commandServer.WriteTimeout = 0
	servers := []*http.Server{newServer(application.Handler()), commandServer, newServer(application.IntakeHandler())}
	for _, server := range servers {
		defer server.Close()
	}
	if verifier != nil {
		fmt.Fprintf(os.Stderr, "Sign-in: Cloudflare Access for %s, and sign-in links\n", server.CloudflareAccess.TeamDomain)
	} else {
		fmt.Fprintln(os.Stderr, "Sign-in: sign-in links only; the settings file has no cloudflare_access")
	}
	report := func(err error) { fmt.Fprintf(os.Stderr, "mmhotline: %s\n", err) }
	// The settings file's other parts apply before serve says it is ready,
	// so a settings apply after a restart finds them applied.
	if err := application.ApplySettingsFileAtStart(ctx, report); err != nil {
		return err
	}
	// The parts with an outside test apply in the background; they stop
	// with ctx and end before the database closes.
	defer func() { cancelBackground(); application.WaitForSettingsFileAppliedAtStart() }()
	// Every socket now accepts connections; they wait in the backlog until
	// Serve starts below.
	if err := notifySystemdReady(); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	backgroundDone := make(chan struct{})
	go func() {
		defer close(backgroundDone)
		var background sync.WaitGroup
		background.Go(func() { application.RunBackground(ctx, report) })
		// Polls Telegram when the Telegram sending service is set up.
		background.Go(func() { application.RunTelegramPolling(ctx, report) })
		background.Go(func() { application.RunSlackSocketMode(ctx, report) })
		// Checks in with the MMHotline-down alarm every minute when it is set up.
		background.Go(func() { application.RunDownAlarm(ctx, report) })
		background.Wait()
	}()
	defer func() { cancelBackground(); <-backgroundDone }()
	done := make(chan error, len(servers)+1)
	if proxyListener != nil {
		go func() { done <- application.ServeTriageAgentProxy(proxyListener) }()
	}
	for index, server := range servers {
		go func() { done <- server.Serve(listeners[index]) }()
	}
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case receivedSignal := <-signals:
		cancelBackground()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var shutdownErr error
		for _, server := range servers {
			shutdownErr = errors.Join(shutdownErr, server.Shutdown(shutdownContext))
		}
		if receivedSignal == os.Interrupt {
			return errInterrupted
		}
		return shutdownErr
	}
}

// listenServerSettings opens the web and alert intake listeners.
func listenServerSettings(server app.ServerSettings) (net.Listener, net.Listener, error) {
	web, err := net.Listen("tcp", server.WebListenAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("MMHotline cannot listen on web_listen_address %s: %w", server.WebListenAddress, err)
	}
	intake, err := net.Listen("tcp", server.AlertIntakeListenAddress)
	if err != nil {
		web.Close()
		return nil, nil, fmt.Errorf("MMHotline cannot listen on alert_intake_listen_address %s: %w", server.AlertIntakeListenAddress, err)
	}
	return web, intake, nil
}

// contactMethodTypeConfiguration reads the development fake sending
// services' address from the environment: a development switch, not a team
// setting. Every sending service's credentials live in sending service setup
// in the database; only the outgoing webhook contact method type needs none.
func contactMethodTypeConfiguration() (map[string]app.ContactMethodType, string, error) {
	fakeURL := strings.TrimRight(strings.TrimSpace(os.Getenv("MMHOTLINE_FAKE_SENDING_SERVICES_URL")), "/")
	if fakeURL != "" {
		u, err := url.Parse(fakeURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, "", errors.New("MMHOTLINE_FAKE_SENDING_SERVICES_URL must be an http or https URL, such as http://127.0.0.1:8090")
		}
	}
	webhook := app.OutgoingWebhookContactMethodType{}
	if fakeURL != "" {
		webhook.AllowedPrivateURLPrefix = fakeURL + "/webhook-receiver/"
	}
	return map[string]app.ContactMethodType{"outgoing_webhook": webhook}, fakeURL, nil
}
