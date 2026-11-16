package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mmhotline-fleet-view@.service starts mmhotline run-fleet-view-command as
// root for each connection. It reads fleet_view_command from the settings
// file, runs its words with no shell and with only PATH in the environment,
// and passes on the command's output whatever the command's exit status,
// since a fleet view exits nonzero while a server fails.
func TestRunFleetViewCommand(t *testing.T) {
	skipUnlessSettingsFileOwnerAllowed(t)
	tool := filepath.Join(t.TempDir(), "monitoring-tool")
	script := `#!/bin/sh
printf '{"nodes": []}\n'
printf 'arguments=%s|%s|%s\n' "$1" "$2" "$3"
printf 'PATH=%s\n' "$PATH"
printf 'environment=%s\n' "${MMHOTLINE_TEST_ENVIRONMENT-}"
echo "app002 fails" >&2
exit 1
`
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	base := testSettingsYAML("https://mmhotline.example.com", "127.0.0.1:8480", "10.20.0.1:8484")
	withCommand := func(command string) string {
		return strings.Replace(base, "team_name: Operations\n", "team_name: Operations\nfleet_view_command: "+command+"\n", 1)
	}
	run := func(path string) (int, string, string) {
		t.Helper()
		command := cliProcess(t, "run-fleet-view-command", "--settings-file", path)
		command.Env = append(command.Env, "MMHOTLINE_TEST_ENVIRONMENT=not passed on")
		var output, errors bytes.Buffer
		command.Stdout, command.Stderr = &output, &errors
		_ = command.Run()
		return command.ProcessState.ExitCode(), output.String(), errors.String()
	}
	path := writeTestSettingsFile(t, "", withCommand(`["`+tool+`", fleet, --json, "$HOME|cat"]`))
	code, output, errors := run(path)
	want := "{\"nodes\": []}\narguments=fleet|--json|$HOME|cat\nPATH=" + strings.TrimPrefix(fleetViewCommandPath, "PATH=") + "\nenvironment=\n"
	if code != 0 || output != want || !strings.Contains(errors, "app002 fails") {
		t.Fatalf("a fleet view that exits 1 is still a fleet view: exit %d\noutput:\n%s\nerrors:\n%s", code, output, errors)
	}
	// A problem elsewhere in the file does not stop the fleet view: MMHotline
	// keeps its earlier settings then, and the dashboard its servers.
	path = writeTestSettingsFile(t, "", strings.Replace(withCommand(`["`+tool+`", fleet]`), "team_name: Operations\n", "", 1))
	if code, output, _ := run(path); code != 0 || !strings.HasPrefix(output, `{"nodes": []}`) {
		t.Fatalf("a problem elsewhere stopped the fleet view: exit %d %s", code, output)
	}

	for _, test := range []struct{ settings, refusal string }{
		{base, "has no fleet_view_command"},
		{withCommand("[monitoring-tool, fleet, --json]"), "fleet_view_command[0]: must be the program's absolute path, such as /usr/local/bin/PROGRAM"},
	} {
		code, output, errors := run(writeTestSettingsFile(t, "", test.settings))
		if code != 4 || output != "" || !strings.Contains(errors, test.refusal) {
			t.Fatalf("refusal %q: exit %d\n%s%s", test.refusal, code, output, errors)
		}
	}
	// Root runs what the file says, so a file others could change is refused.
	path = writeTestSettingsFile(t, "", withCommand(`["`+tool+`"]`))
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	if code, output, errors := run(path); code != 4 || output != "" || !strings.Contains(errors, "can be changed by users other than root") {
		t.Fatalf("a group-writable file: exit %d\n%s%s", code, output, errors)
	}
	for _, args := range [][]string{{"run-fleet-view-command"}, {"run-fleet-view-command", "--settings-file", path, "extra"}} {
		if err := cliProcess(t, args...).Run(); err == nil {
			t.Fatalf("%v ran", args)
		}
	}
}
