package actionkinds

import (
	"encoding/json"
	"reflect"
	"testing"
)

// serviceTask is a systemd_service task that starts name, with the marker
// file marker when marker is not empty, which restarts the unit once.
func serviceTask(name, marker string) string {
	quoted, _ := json.Marshal(name)
	arguments := `{"field":"systemd_service.name","value":` + string(quoted) + `,"status":"resolved"},{"field":"systemd_service.state","value":"running","status":"literal"}`
	if marker != "" {
		quotedMarker, _ := json.Marshal(marker)
		arguments += `,{"field":"systemd_service.restart_now","value":true,"status":"literal"},{"field":"systemd_service.run_only_once_marker_file","value":` + string(quotedMarker) + `,"status":"resolved"}`
	}
	return `{"path":"fix/task_1","action":"systemd_service","module_kind":"core","status":"ready","arguments":[` + arguments + `]}`
}

const goodMarker = "/var/lib/mmx/commands_run_once/2026-11-17_restart_nginx"

// Start or restart a service accepts only a plain service of the team's:
// starting a target, systemd's own units, or the operating system's power,
// shell, network, and firewall units would power off, reboot, cut off, or
// open a server without a person. Each such task is an unrecognized task, so
// the inventory's rule for Unrecognized task decides.
func TestStartOrRestartAServiceAcceptsOnlyPlainServices(t *testing.T) {
	// Written out, not read from the table, so a run before the fix fails.
	const plainServiceOnly = "systemd_service.name must be a plain service of the team's, known before the run: a name ending in .service or with no unit type, with only letters, digits, and _ . -, and not a template instance (@), a systemd-* or dbus-org.* unit, or an operating system unit whose start shuts down, reboots, opens a shell, changes the network or firewall, or installs updates, such as poweroff, reboot, emergency, networking, or apt-daily-upgrade"
	for _, name := range []string{
		"nginx", "nginx.service", "shop-worker", "caddy", "postgresql", "php8.2-fpm", "shop_worker.service",
	} {
		recognition, err := Recognize(explainOutput(serviceTask(name, goodMarker)))
		if err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{StartOrRestartAService}) {
			t.Errorf("%s: %+v %v; want Start or restart a service", name, recognition, err)
		}
	}
	for _, name := range []string{
		// Targets, which start or isolate a whole system state.
		"poweroff.target", "reboot.target", "halt.target", "kexec.target", "emergency.target", "rescue.target",
		"ctrl-alt-del.target", "shutdown.target", "default.target", "multi-user.target", "exit.target",
		"soft-reboot.target", "suspend.target", "hibernate.target", "sleep.target",
		// systemd's own units, and their D-Bus aliases.
		"systemd-poweroff.service", "systemd-reboot.service", "systemd-halt.service", "systemd-kexec.service",
		"systemd-soft-reboot.service", "systemd-suspend.service", "systemd-exit.service", "systemd-networkd",
		"SYSTEMD-poweroff.service", "dbus-org.freedesktop.login1.service",
		// The operating system's units that shut down, open a shell, cut the
		// network, or change the firewall or packages; init scripts of the
		// same names on a system that keeps them.
		"poweroff", "reboot", "halt", "kexec", "shutdown", "emergency.service", "rescue", "debug-shell.service",
		"sendsigs", "umountfs", "networking", "NetworkManager.service", "dbus", "nftables", "firewalld",
		"netfilter-persistent", "apt-daily-upgrade.service", "unattended-upgrades", "cloud-init", "plymouth-reboot.service",
		// Other unit types, instances of templates, and names MMX or systemd
		// read in other ways.
		"tmp.mount", "swapfile.swap", "backup.timer", "docker.socket", "user.slice", "foo.path", "proc-sys-fs-binfmt_misc.automount",
		"getty@tty1.service", "wg-quick@wg0", "serial-getty@ttyS0.service", "foo@.service",
		`srv-my\x2ddata`, "a:b", "../poweroff.target", "/etc/systemd/system/poweroff.target", "nginx service", "", "-nginx", ".service",
	} {
		recognition, err := Recognize(explainOutput(serviceTask(name, "")))
		if err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{UnrecognizedTask}) || len(recognition.UnrecognizedTasks) != 1 || recognition.UnrecognizedTasks[0].Reason != plainServiceOnly {
			t.Errorf("%q: %+v %v\nwant the reason %q", name, recognition, err, plainServiceOnly)
		}
	}
	// A name MMX knows only during the run could be any unit.
	runtime := `{"path":"fix/task_1","action":"systemd_service","module_kind":"core","status":"ready","arguments":[{"field":"systemd_service.name","value":null,"reference":"{{ .vars.unit }}","status":"runtime"},{"field":"systemd_service.state","value":"running","status":"literal"}]}`
	if recognition, err := Recognize(explainOutput(runtime)); err != nil || len(recognition.UnrecognizedTasks) != 1 || recognition.UnrecognizedTasks[0].Reason != plainServiceOnly {
		t.Errorf("a name known only during the run: %+v %v", recognition, err)
	}
}

// MMX writes the marker file as root and creates its missing directories, so
// a marker outside MMX's own run-once directory could create any file, such
// as /etc/nologin, which locks out every login but root's.
func TestTheMarkerFileStaysInMMXsRunOnceDirectory(t *testing.T) {
	const runOnceDirectoryOnly = "systemd_service.run_only_once_marker_file must be known before the run and name a file directly in /var/lib/mmx/commands_run_once/, whose name has only letters, digits, and _ . -: MMX writes the marker file as root, so a marker elsewhere, such as /etc/nologin, could lock people out"
	for _, marker := range []string{goodMarker, "/var/lib/mmx/commands_run_once/restart_nginx.2"} {
		if recognition, err := Recognize(explainOutput(serviceTask("nginx", marker))); err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{StartOrRestartAService}) {
			t.Errorf("%s: %+v %v", marker, recognition, err)
		}
	}
	for _, marker := range []string{
		"/etc/nologin", "/run/nologin", "/etc/sudoers.d/x", "/etc/apt/apt.conf.d/99x", "/var/lib/mmx_markers/x", "/var/lib/mmx/commands_run_once",
		"/var/lib/mmx/commands_run_once/", "/var/lib/mmx/commands_run_once/../../../../etc/nologin", "/var/lib/mmx/commands_run_once/./x",
		"/var/lib/mmx/commands_run_once//x", "/var/lib/mmx/commands_run_once/a/b", "/var/lib/mmx/commands_run_once/..", "/var/lib/mmx/commands_run_once/.hidden",
		"var/lib/mmx/commands_run_once/x", "/var/lib/mmx/commands_run_once/a b",
	} {
		recognition, err := Recognize(explainOutput(serviceTask("nginx", marker)))
		if err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{UnrecognizedTask}) || len(recognition.UnrecognizedTasks) != 1 || recognition.UnrecognizedTasks[0].Reason != runOnceDirectoryOnly {
			t.Errorf("%q: %+v %v\nwant the reason %q", marker, recognition, err, runOnceDirectoryOnly)
		}
	}
	// A marker file without a restart is still written by no one, but it is
	// checked the same way, so no form of the task names a file elsewhere.
	noRestart := `{"path":"fix/task_1","action":"systemd_service","module_kind":"core","status":"ready","arguments":[{"field":"systemd_service.name","value":"nginx","status":"resolved"},{"field":"systemd_service.run_only_once_marker_file","value":"/etc/nologin","status":"resolved"},{"field":"systemd_service.state","value":"running","status":"literal"}]}`
	if recognition, err := Recognize(explainOutput(noRestart)); err != nil || len(recognition.UnrecognizedTasks) != 1 || recognition.UnrecognizedTasks[0].Reason != runOnceDirectoryOnly {
		t.Errorf("a marker file without a restart: %+v %v", recognition, err)
	}
}
