package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"mmhotline/internal/app"
)

const settingsUsage = `usage: mmhotline settings check --settings-file FILE [--json]
       sudo mmhotline settings apply [--command-socket PATH] [--json]
       sudo mmhotline settings show [--command-socket PATH] [--json]`

// restartMMHotline restarts the installed service for settings apply, when
// a key that changes only at a restart changed (CA5). Tests replace it.
var restartMMHotline = func(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "systemctl", "restart", "mmhotline.service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart mmhotline.service failed: %w\n%s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// settingsRestartWait bounds how long settings apply waits for the
// restarted MMHotline to answer on its command socket. systemctl restart
// returns once serve is ready, so this only covers a slow start; a longer
// wait means serve did not start.
var settingsRestartWait = time.Minute

// runSettings runs the settings commands: check reads a file itself, with no
// root and no network; apply and show ask the running MMHotline, which
// reads the file at the path serve was given.
func runSettings(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-help") {
		fmt.Println(settingsUsage)
		return nil
	}
	if len(args) == 0 || (args[0] != "check" && args[0] != "apply" && args[0] != "show") {
		return commandError{errors.New(settingsUsage)}
	}
	action := args[0]
	flags := flag.NewFlagSet("settings "+action, flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	asJSON := flags.Bool("json", false, "print one JSON object")
	var path, socket string
	if action == "check" {
		flags.StringVar(&path, "settings-file", "", "the settings file to check, such as /etc/mmhotline/mmhotline_settings.yml")
	} else {
		flags.StringVar(&socket, "command-socket", installCommandSocket, "local command socket path")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stderr, flagOutput.String())
			return nil
		}
		return commandError{errors.New(flagOutput.String())}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("unexpected positional arguments")}
	}
	if action == "check" {
		if path == "" {
			return commandError{errors.New("--settings-file is required")}
		}
		return checkSettingsFile(path, *asJSON)
	}
	ctx := context.Background()
	request := app.CommandRequest{Group: "settings", Action: action, UnixUser: os.Getenv("SUDO_USER")}
	if request.UnixUser == "" {
		request.UnixUser = "root"
	}
	// An apply waits for an earlier apply and for the parts' outside tests,
	// so it has no time limit; Ctrl-C stops the wait.
	result, err := sendCommandWithTimeout(ctx, socket, request, 0)
	if err != nil {
		return err
	}
	view := result.SettingsFile
	if view == nil {
		return commandError{errors.New("mmhotline serve returned a command response without settings_file")}
	}
	if restartFor := view.RestartNeededFor; action == "apply" && len(restartFor) > 0 {
		if !*asJSON {
			fmt.Printf("Restarting MMHotline to apply the settings that require a restart: %s.\n", strings.Join(restartFor, ", "))
		}
		if err := restartMMHotline(ctx); err != nil {
			return err
		}
		if err := waitForCommandSocket(ctx, socket, request.UnixUser); err != nil {
			return err
		}
		if result, err = sendCommandWithTimeout(ctx, socket, request, 0); err != nil {
			return err
		}
		if view = result.SettingsFile; view == nil {
			return commandError{errors.New("mmhotline serve returned a command response without settings_file")}
		}
		if len(view.RestartNeededFor) > 0 {
			return fmt.Errorf("after restarting, MMHotline still uses the previous values for settings %s; check the service log with sudo journalctl -u mmhotline -n 50", strings.Join(view.RestartNeededFor, ", "))
		}
		view.RestartedFor = restartFor
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
	} else {
		printText(result.Text())
	}
	if action == "apply" && (len(view.Problems) > 0 || len(view.FailedParts()) > 0) {
		return checkAnswer{exitCode: 1}
	}
	return nil
}

// checkSettingsFile prints each problem of the file with its key path, and
// answers with exit code 1 when it has any. Notes, such as a kind of action
// without a setting, print too and do not fail the check, so a deploy
// tool's dry run shows them. It skips the owner and mode check (CA2): a
// laptop that renders the file cannot make root its owner.
func checkSettingsFile(path string, asJSON bool) error {
	file, problems := app.ReadSettingsFile(path, nil)
	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			SettingsFile string                `json:"settings_file"`
			Problems     []app.SettingsProblem `json:"problems"`
			Notes        []app.SettingsNote    `json:"notes"`
		}{path, append([]app.SettingsProblem{}, problems...), append([]app.SettingsNote{}, file.Notes...)}); err != nil {
			return err
		}
	} else {
		if len(problems) == 0 {
			fmt.Printf("The settings file %s has no problems.\n", path)
		} else {
			count := fmt.Sprintf("%d problems", len(problems))
			if len(problems) == 1 {
				count = "1 problem"
			}
			fmt.Printf("The settings file %s has %s:\n%s\n", path, count, settingsProblemLines(problems))
		}
		if len(file.Notes) > 0 {
			fmt.Println("Notes, which do not stop MMHotline from applying the settings file:")
			for _, note := range file.Notes {
				fmt.Println(note.String())
			}
		}
	}
	if len(problems) > 0 {
		return checkAnswer{exitCode: 1}
	}
	return nil
}

// settingsProblemLines is one problem a line, each with its key path.
func settingsProblemLines(problems []app.SettingsProblem) string {
	var lines []string
	for _, problem := range problems {
		lines = append(lines, problem.String())
	}
	return strings.Join(lines, "\n")
}

// waitForCommandSocket waits until the restarted MMHotline answers on its
// command socket, at most settingsRestartWait.
func waitForCommandSocket(ctx context.Context, socket, unixUser string) error {
	ctx, cancel := context.WithTimeout(ctx, settingsRestartWait)
	defer cancel()
	for {
		_, err := sendCommandWithTimeout(ctx, socket, app.CommandRequest{Group: "settings", Action: "show", UnixUser: unixUser}, 5*time.Second)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("MMHotline gave no successful answer on its command socket %s within %s of the restart: %w; check the service log with sudo journalctl -u mmhotline -n 50", socket, settingsRestartWait, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
