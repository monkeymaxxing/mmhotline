package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"mmhotline/internal/app"
)

const connectPushoverUsage = "usage: sudo mmhotline connect-pushover [--settings-file FILE] [--command-socket PATH]"

// connectPushoverNeedsATerminal refuses a run without a person at the
// prompts. Reason: the command exists so that a person types the two
// Pushover secrets at hidden prompts, where a coding agent never sees them.
const connectPushoverNeedsATerminal = "connect-pushover asks at hidden prompts; run it yourself in a terminal (ssh -t for a remote server). Scripts use settings set-secret and contact-methods add."

// connectPushoverTokenIsSaved follows each failure after the token applied.
const connectPushoverTokenIsSaved = "The application token is saved and applied, so running sudo mmhotline connect-pushover again is fine."

// connectPushover sets up Pushover and adds the person's phone in one run:
// the team's application token, saved and applied exactly as
// `settings set-secret sending_services.pushover.application_token --apply`
// does, then the person's user key, added exactly as `contact-methods add
// --contact-method-type pushover_notification` does. It stores nothing of
// its own.
func connectPushover(args []string, terminals setSecretTerminals) error {
	flags := flag.NewFlagSet("connect-pushover", flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	settingsFile := flags.String("settings-file", app.InstalledSettingsFilePath, "the settings file that gets the application token")
	socket := flags.String("command-socket", installCommandSocket, "local command socket path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(terminals.Stdout, connectPushoverUsage)
			fmt.Fprint(terminals.Stdout, flagOutput.String())
			return nil
		}
		return commandError{errors.New(flagOutput.String())}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("unexpected positional arguments\n" + connectPushoverUsage)}
	}
	if !terminals.StdinIsTerminal {
		return commandError{errors.New(connectPushoverNeedsATerminal)}
	}
	out := terminals.Stdout
	unixUser := os.Getenv("SUDO_USER")
	if unixUser == "" {
		unixUser = "root"
	}

	// 1. The application token. set-secret refuses a malformed token before
	// it saves anything; settings apply checks the token with Pushover.
	fmt.Fprintln(out, "Copy the API token from your MMHotline application's page on pushover.net.")
	terminals.ValuePrompt = "Pushover application token (nothing shows as you paste): "
	terminals.Apply = func() error { return runSettings([]string{"apply", "--command-socket", *socket}) }
	err := settingsSetSecret([]string{"sending_services.pushover.application_token", "--apply", "--settings-file", *settingsFile}, terminals)
	var answer checkAnswer
	if err != nil && !errors.As(err, &answer) {
		return err
	}
	file, problems := app.ReadSettingsFile(*settingsFile, nil)
	if err != nil {
		if len(problems) > 0 {
			fmt.Fprintln(out, "MMHotline saved the token in the settings file but did not apply it, because the file has the problems listed above. Fix them, then run sudo mmhotline connect-pushover again.")
			return err
		}
		failed, showErr := pushoverPartFailed(*socket, unixUser)
		if showErr != nil {
			return showErr
		}
		if failed {
			fmt.Fprintln(out, "Pushover refused the application token, so MMHotline did not start using it. Copy the API token again from your MMHotline application's page on pushover.net, then run sudo mmhotline connect-pushover again.")
			return err
		}
		// Another part failed and is named above; Pushover applied.
	}

	// 2. Whose phone.
	person, err := askWhosePhone(file.Settings.People, terminals)
	if err != nil {
		return err
	}

	// 3. The person's user key, checked with Pushover, which lists its devices.
	fmt.Fprintln(out, `Copy your user key from the top of pushover.net after you sign in, labelled "Your User Key". It is not the application's token.`)
	raw, err := readSecretFromStandardInput(terminals.Stdin, terminals.Prompt, "Your Pushover user key (nothing shows as you paste): ", terminals.TurnOffEcho)
	key := strings.TrimSpace(string(raw))
	if err != nil || key == "" {
		return commandError{errors.New("MMHotline did not get a user key. " + connectPushoverTokenIsSaved)}
	}
	checked, err := sendCommand(*socket, app.CommandRequest{Group: "contact-methods", Action: "check-pushover-user-key", UnixUser: unixUser, ContactMethods: app.ContactMethodsCommandRequest{PushoverUserKey: key}})
	if err != nil {
		return commandError{fmt.Errorf("%w. Copy your user key again from the top of pushover.net. %s", err, connectPushoverTokenIsSaved)}
	}

	// 4. Which device, when the account has more than one.
	device, err := askWhichDevice(checked.PushoverDeviceNames, terminals)
	if err != nil {
		return err
	}

	// 5. The contact method, as contact-methods add saves it: it sends the
	// quiet test notification and prints the same lines.
	added, err := sendCommand(*socket, app.CommandRequest{Group: "contact-methods", Action: "add", UnixUser: unixUser, ContactMethods: app.ContactMethodsCommandRequest{
		SignInEmailAddress: person.SignInEmailAddress, ContactMethodType: "pushover_notification", PushoverUserKey: key, PushoverDeviceName: device}})
	if err != nil && strings.Contains(err.Error(), app.ContactMethodAlreadyAdded) {
		fmt.Fprintf(out, "%s already has a Pushover contact method with this user key, so MMHotline kept it as it is. To send to another device, remove it on the Contact methods page, then run this command again.\n", person.Name)
		return nil
	}
	if err != nil {
		return commandError{fmt.Errorf("MMHotline did not add the phone: %w. %s", err, connectPushoverTokenIsSaved)}
	}
	// The person reads this, so it says what happened in plain words; the
	// contact method's own fields (its ID, type, queue state) are for
	// contact-methods add and list.
	first, rest, _ := strings.Cut(added.Message, "\n\n")
	if reason, ok := strings.CutPrefix(first, "Contact method saved. "); ok && reason != "A test message for the contact method is queued. Inspect the last test result to check delivery." {
		first = "Your phone is added, but the quiet test notification was not sent: " + reason
	} else {
		first = "Your phone is added. A quiet test notification is on its way to it now."
	}
	writeText(out, strings.TrimSpace(first+"\n\n"+rest))
	return nil
}

// pushoverPartFailed asks the running MMHotline whether the settings
// file's Pushover part failed at the last apply.
func pushoverPartFailed(socket, unixUser string) (bool, error) {
	shown, err := sendCommand(socket, app.CommandRequest{Group: "settings", Action: "show", UnixUser: unixUser})
	if err != nil || shown.SettingsFile == nil {
		return false, err
	}
	return slices.ContainsFunc(shown.SettingsFile.Parts, func(part app.SettingsFilePartOutcome) bool {
		return part.Part == "sending_services.pushover" && part.State == "failed"
	}), nil
}

// askWhosePhone is the settings file's one person, or the person whose
// sign-in email address the person types. An unknown address is refused
// with the list of addresses, and asked again.
func askWhosePhone(people []app.SettingsPerson, terminals setSecretTerminals) (app.SettingsPerson, error) {
	if len(people) == 1 {
		fmt.Fprintf(terminals.Stdout, "Adding the phone of %s (%s), the one person in the settings file.\n", people[0].Name, people[0].SignInEmailAddress)
		return people[0], nil
	}
	var addresses []string
	for _, person := range people {
		addresses = append(addresses, person.SignInEmailAddress)
	}
	for {
		fmt.Fprint(terminals.Prompt, "Your sign-in email address: ")
		line, err := readOneLine(terminals.Stdin)
		answer := strings.TrimSpace(string(line))
		if err != nil || len(line) == 0 {
			return app.SettingsPerson{}, commandError{errors.New("MMHotline did not get a sign-in email address. " + connectPushoverTokenIsSaved)}
		}
		for _, person := range people {
			if strings.EqualFold(person.SignInEmailAddress, answer) {
				return person, nil
			}
		}
		fmt.Fprintf(terminals.Stdout, "Nobody in the settings file's people has the sign-in email address %q. The sign-in email addresses are: %s.\n", answer, strings.Join(addresses, ", "))
	}
}

// askWhichDevice is the device name the person chooses, or "" for all
// devices. With one device or none, it asks nothing. An unknown name is
// asked again.
func askWhichDevice(devices []string, terminals setSecretTerminals) (string, error) {
	if len(devices) < 2 {
		return "", nil
	}
	fmt.Fprintf(terminals.Stdout, "Your Pushover account has these devices: %s.\n", strings.Join(devices, ", "))
	for {
		fmt.Fprint(terminals.Prompt, "Send alerts to which device? Press Enter for all devices: ")
		line, err := readOneLine(terminals.Stdin)
		answer := strings.TrimSpace(string(line))
		if err != nil || len(line) == 0 {
			return "", commandError{errors.New("MMHotline did not get an answer about the device. " + connectPushoverTokenIsSaved)}
		}
		if answer == "" || slices.Contains(devices, answer) {
			return answer, nil
		}
		fmt.Fprintf(terminals.Stdout, "Pushover lists no device named %q. Type one of: %s, or press Enter for all devices.\n", answer, strings.Join(devices, ", "))
	}
}
