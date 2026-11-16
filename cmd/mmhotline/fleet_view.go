package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"mmhotline/internal/app"
)

// fleetViewCommandPath is the search path the fleet view command gets, and
// all of its environment: the command names its program by absolute path,
// and nothing of MMHotline's settings reaches it.
const fleetViewCommandPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// runFleetViewCommand is what mmhotline-fleet-view@.service starts as root
// for each connection from MMHotline; a person never runs it. It reads
// fleet_view_command from the settings file each time, so a change applies
// at the next read. The file must pass its owner and mode check, because
// root runs what it says. A problem elsewhere in the file does not stop
// the fleet view: MMHotline keeps running with its earlier settings then,
// and the dashboard keeps showing the servers. The command runs with no
// shell, its input is the null device, its output goes to the connection,
// and its errors go to the journal.
func runFleetViewCommand(args []string) error {
	flags := flag.NewFlagSet("run-fleet-view-command", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("settings-file", "", "the settings file")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *path == "" {
		return commandError{errors.New("usage: mmhotline run-fleet-view-command --settings-file FILE; mmhotline-fleet-view@.service starts it")}
	}
	file, problems := app.ReadSettingsFile(*path, settingsFileOwners())
	for _, problem := range problems {
		if problem.KeyPath == "" || strings.HasPrefix(problem.KeyPath, "fleet_view_command") {
			return commandError{fmt.Errorf("the settings file %s: %s", *path, problem)}
		}
	}
	words := file.Settings.FleetViewCommand
	if len(words) == 0 {
		return commandError{fmt.Errorf("the settings file %s has no fleet_view_command", *path)}
	}
	command := exec.Command(words[0], words[1:]...)
	command.Env = []string{fleetViewCommandPath}
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	err := command.Run()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		// A fleet view exits with the worst server's status while it prints
		// the whole view; MMHotline reads the view, so this run succeeded.
		return nil
	}
	return err
}
