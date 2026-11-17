package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"mmhotline/internal/app"
)

// setSecretTerminals is where set-secret reads the value and shows its
// answers; tests replace it.
type setSecretTerminals struct {
	Stdin  *os.File
	Stdout io.Writer
	Prompt io.Writer
	// StdoutIsTerminal is true when a person's screen shows Stdout.
	StdoutIsTerminal bool
	TurnOffEcho      func(*os.File) (func(), bool)
	// Apply runs sudo mmhotline settings apply.
	Apply func() error
}

func realSetSecretTerminals() setSecretTerminals {
	return setSecretTerminals{Stdin: os.Stdin, Stdout: os.Stdout, Prompt: os.Stderr, StdoutIsTerminal: fileIsTerminal(os.Stdout), TurnOffEcho: turnOffTerminalEcho,
		Apply: func() error { return runSettings([]string{"apply"}) }}
}

var signingSecretKeyPathPattern = regexp.MustCompile(`^alert_sources\[[^\]]+\]\.signing_secrets$`)

const settingsSetSecretUsage = "usage: sudo mmhotline settings set-secret KEY_PATH [--settings-file FILE] [--make-new [--write-to-file PATH]] [--apply]"

// settingsSetSecret changes one secret key of the settings file. Reason for
// each refusal before any change: the secret must never be half-saved, and
// a new signing secret must not pass through a coding agent's chat.
func settingsSetSecret(args []string, terminals setSecretTerminals) error {
	flags := flag.NewFlagSet("settings set-secret", flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	settingsFile := flags.String("settings-file", app.InstalledSettingsFilePath, "the settings file to change")
	makeNew := flags.Bool("make-new", false, "make a new random signing secret for an alert source")
	writeToFile := flags.String("write-to-file", "", "with --make-new, write the new signing secret to this new file, which only root can read")
	applyAfter := flags.Bool("apply", false, "run sudo mmhotline settings apply after saving")
	keyPath := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		keyPath, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(terminals.Stdout, settingsSetSecretUsage)
			fmt.Fprint(terminals.Stdout, flagOutput.String())
			return nil
		}
		return commandError{errors.New(flagOutput.String())}
	}
	if keyPath == "" && flags.NArg() == 1 {
		keyPath = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return commandError{errors.New("unexpected positional arguments\n" + settingsSetSecretUsage)}
	}
	if keyPath == "" {
		return commandError{errors.New("KEY_PATH is required, such as sending_services.pushover.application_token\n" + settingsSetSecretUsage)}
	}
	if refusal := app.SettingsSecretKeyPathRefusal(keyPath); refusal != "" {
		return commandError{errors.New(refusal)}
	}
	if *writeToFile != "" && !*makeNew {
		return commandError{errors.New("--write-to-file works only with --make-new")}
	}
	if *makeNew {
		if !signingSecretKeyPathPattern.MatchString(keyPath) {
			return commandError{errors.New("--make-new works only for a signing secret: alert_sources[ID].signing_secrets, where ID is the alert source's ID")}
		}
		if *writeToFile == "" && !terminals.StdoutIsTerminal {
			return commandError{errors.New("MMHotline shows a new signing secret only on a person's terminal, so it never passes through a coding agent's chat. Run this command yourself, or use --write-to-file PATH.")}
		}
		if _, err := os.Lstat(*writeToFile); *writeToFile != "" && err == nil {
			return commandError{fmt.Errorf("%s already exists, so MMHotline does not overwrite it. Remove it first, then run the command again.", *writeToFile)}
		}
	}

	raw, err := os.ReadFile(*settingsFile)
	if err != nil {
		return fmt.Errorf("MMHotline cannot read the settings file; run the command with sudo: %w", err)
	}
	value, err := newSecretValue(*makeNew, terminals)
	if err != nil {
		return err
	}
	edit, problems, err := app.EditSettingsSecret(raw, keyPath, value)
	if err != nil {
		return commandError{err}
	}
	if len(problems) > 0 {
		var lines []string
		for _, problem := range problems {
			lines = append(lines, problem.String())
		}
		return commandError{fmt.Errorf("The value is not valid, so MMHotline changed nothing:\n%s", strings.Join(lines, "\n"))}
	}
	if *writeToFile != "" {
		if err := writeNewSecretFile(*writeToFile, value); err != nil {
			return err
		}
	}
	if err := app.WriteSettingsFileText(*settingsFile, edit.Text); err != nil {
		if *writeToFile != "" {
			os.Remove(*writeToFile)
		}
		return err
	}

	out := terminals.Stdout
	fmt.Fprintf(out, "Saved %s in %s.\n", keyPath, *settingsFile)
	if signingSecretKeyPathPattern.MatchString(keyPath) && edit.KeptEarlierSigningSecret {
		fmt.Fprintln(out, "The source's earlier signing secret still works, so its sender keeps working until it uses the new one.")
	}
	switch {
	case *writeToFile != "":
		fmt.Fprintf(out, "Wrote the new signing secret to %s, owned by root, readable only by root. The monitoring tool reads it there: set its signing secret file setting, such as MMWatchdog's mmhotline_signing_secret_file, to %s.\n", *writeToFile, *writeToFile)
	case *makeNew:
		fmt.Fprintln(out, "Copy this new signing secret into the monitoring tool's setting now. MMHotline will not show it again:")
		fmt.Fprintln(out, value)
	}
	_, remaining := app.ReadSettingsFile(*settingsFile, nil)
	if len(remaining) > 0 {
		fmt.Fprintf(out, "The settings file still has problems, so it cannot be applied yet:\n%s\n", settingsProblemLines(remaining))
		if *applyAfter {
			return checkAnswer{exitCode: 1}
		}
		fmt.Fprintln(out, "After you fix them, run: sudo mmhotline settings apply")
		return nil
	}
	fmt.Fprintf(out, "The settings file %s has no problems.\n", *settingsFile)
	if *applyAfter {
		return terminals.Apply()
	}
	fmt.Fprintln(out, "Run: sudo mmhotline settings apply")
	return nil
}

// newSecretValue is 32 random bytes as 64 lowercase hexadecimal
// characters, the form of a signing secret, or the value the person pastes.
func newSecretValue(makeNew bool, terminals setSecretTerminals) (string, error) {
	if makeNew {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("MMHotline cannot make a random signing secret: %w", err)
		}
		return hex.EncodeToString(random), nil
	}
	raw, err := readSecretFromStandardInput(terminals.Stdin, terminals.Prompt, terminals.TurnOffEcho)
	if err != nil {
		return "", fmt.Errorf("MMHotline cannot read the value: %w", err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", commandError{errors.New("the value is empty, so MMHotline changed nothing")}
	}
	return value, nil
}

// writeNewSecretFile creates path for root alone, and fails when it exists,
// so a file that the monitoring tool already reads is never overwritten.
func writeNewSecretFile(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return commandError{fmt.Errorf("%s already exists, so MMHotline does not overwrite it. Remove it first, then run the command again.", path)}
		}
		return fmt.Errorf("MMHotline cannot create %s: %w", path, err)
	}
	if os.Geteuid() == 0 {
		if err = file.Chown(0, 0); err != nil {
			file.Close()
			os.Remove(path)
			return err
		}
	}
	if _, err = file.WriteString(value + "\n"); err == nil {
		err = file.Close()
	} else {
		file.Close()
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("MMHotline cannot write %s: %w", path, err)
	}
	return nil
}
