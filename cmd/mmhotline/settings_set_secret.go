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
	"time"

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
	// StdinIsTerminal is true when a person types at Stdin; connect-pushover
	// refuses to run without one.
	StdinIsTerminal bool
	TurnOffEcho     func(*os.File) (func(), bool)
	// ValuePrompt is the hidden prompt for the value; empty is
	// pasteTheValuePrompt.
	ValuePrompt string
	// Apply runs sudo mmhotline settings apply.
	Apply func() error
}

func realSetSecretTerminals() setSecretTerminals {
	return setSecretTerminals{Stdin: os.Stdin, Stdout: os.Stdout, Prompt: os.Stderr, StdoutIsTerminal: fileIsTerminal(os.Stdout), StdinIsTerminal: fileIsTerminal(os.Stdin), TurnOffEcho: turnOffTerminalEcho,
		Apply: func() error { return runSettings([]string{"apply"}) }}
}

var signingSecretKeyPathPattern = regexp.MustCompile(`^alert_sources\[[^\]]+\]\.signing_secrets$`)

const settingsSetSecretUsage = "usage: sudo mmhotline settings set-secret KEY_PATH [--settings-file FILE] [--make-new [--write-to-file PATH]] [--apply]\n       sudo mmhotline settings set-secret alert_sources[ID].signing_secrets --drop-earlier-secrets [--apply]"

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
	dropEarlier := flags.Bool("drop-earlier-secrets", false, "keep only the alert source's latest signing secret and remove the others; makes and reads no new value")
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
	if *dropEarlier {
		if *makeNew || *writeToFile != "" {
			return commandError{errors.New("--drop-earlier-secrets works alone: it makes and reads no new value, so it cannot go with --make-new or --write-to-file")}
		}
		if !signingSecretKeyPathPattern.MatchString(keyPath) {
			return commandError{errors.New("--drop-earlier-secrets works only for a signing secret: alert_sources[ID].signing_secrets, where ID is the alert source's ID")}
		}
		if stdinHoldsAValue(terminals.Stdin) {
			return commandError{errors.New("--drop-earlier-secrets takes no value, but a value arrived on standard input. MMHotline changed nothing.")}
		}
		return dropEarlierSigningSecrets(*settingsFile, keyPath, *applyAfter, terminals)
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
	prompt := terminals.ValuePrompt
	if prompt == "" {
		prompt = pasteTheValuePrompt
	}
	raw, err := readSecretFromStandardInput(terminals.Stdin, terminals.Prompt, prompt, terminals.TurnOffEcho)
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

// stdinHoldsAValue is true when standard input is a pipe or file that has
// a byte to read; it never waits, because a terminal and an open pipe with
// nothing in it count as no value. Reason: a secret given with the drop
// would otherwise be ignored, and the person would think it was saved.
func stdinHoldsAValue(stdin *os.File) bool {
	if stdin == nil {
		return false
	}
	info, err := stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return false
	}
	if info.Mode().IsRegular() {
		return info.Size() > 0
	}
	// A goroutine reads, because not every standard input accepts a
	// read deadline. If the writer never writes, the read ends with the process.
	got := make(chan int, 1)
	go func() {
		var one [1]byte
		count, _ := stdin.Read(one[:])
		got <- count
	}()
	select {
	case count := <-got:
		return count > 0
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// dropEarlierSigningSecrets keeps only the source's latest signing secret.
func dropEarlierSigningSecrets(settingsFile, keyPath string, applyAfter bool, terminals setSecretTerminals) error {
	raw, err := os.ReadFile(settingsFile)
	if err != nil {
		return fmt.Errorf("MMHotline cannot read the settings file; run the command with sudo: %w", err)
	}
	text, dropped, err := app.DropEarlierSigningSecrets(raw, keyPath)
	if err != nil {
		return commandError{err}
	}
	if err := app.WriteSettingsFileText(settingsFile, text); err != nil {
		return err
	}
	sourceID := signingSecretKeyPathPattern.FindString(keyPath)
	sourceID = strings.TrimSuffix(strings.TrimPrefix(sourceID, "alert_sources["), "].signing_secrets")
	out := terminals.Stdout
	fmt.Fprintf(out, "Dropped %d earlier signing secret(s) of alert source %s. Only the latest one still works: make sure its sender uses it.\n", dropped, sourceID)
	_, remaining := app.ReadSettingsFile(settingsFile, nil)
	if len(remaining) > 0 {
		fmt.Fprintf(out, "The settings file still has problems, so it cannot be applied yet:\n%s\n", settingsProblemLines(remaining))
		if applyAfter {
			return checkAnswer{exitCode: 1}
		}
		fmt.Fprintln(out, "After you fix them, run: sudo mmhotline settings apply")
		return nil
	}
	fmt.Fprintf(out, "The settings file %s has no problems.\n", settingsFile)
	if applyAfter {
		return terminals.Apply()
	}
	fmt.Fprintln(out, "Run: sudo mmhotline settings apply")
	return nil
}
