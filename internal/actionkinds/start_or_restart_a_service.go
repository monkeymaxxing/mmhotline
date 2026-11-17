package actionkinds

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// What Start or restart a service may start without a person:
// docs/changing-mmhotline/design/triage-agent.md#the-kinds-of-action.
//
// The rule is an allow-list of a shape with a short list of names refused.
// MMHotline cannot list the team's own services, such as caddy or
// shop-worker, when it reads mmx explain, so a list of allowed names is not
// possible. The shape allows only what the team's services look like: a
// plain .service, never a target, another unit type, or a template instance.
// That alone refuses poweroff.target, reboot.target, and every other way to
// start a system state. What is left of the operating system's own units is
// refused by name: systemd's units (systemd-*, and their D-Bus aliases
// dbus-org.*), and the units listed below. Systemd resolves an alias only to
// a unit of the same type, so an alias of a .service is a .service. A team
// adds any other service it wants a person to decide to
// services_whose_changes_a_person_must_approve.

// plainServiceName is the name of a service without its .service: letters,
// digits, and _ . -, starting with a letter, a digit, or _. It leaves out
// systemd's @ (a template instance), : and \ (escaped paths, as mount units
// use), and every path separator.
var plainServiceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// refusedServicePrefixes start the names of systemd's own units, such as
// systemd-poweroff.service and systemd-reboot.service, and of their D-Bus
// aliases, such as dbus-org.freedesktop.login1.service; plymouth's, such as
// plymouth-reboot.service, take over the console for a shutdown; cloud-init's
// run the server's first-boot setup again.
var refusedServicePrefixes = []string{"systemd-", "dbus-org.", "plymouth-", "cloud-"}

// refusedServices are the operating system's services whose start does
// what Start or restart a service must not do without a person:
//   - shut down or reboot: poweroff, reboot, halt, kexec, and shutdown, and
//     the init scripts a system without systemd's masks turns into services
//     (sendsigs, umountfs, umountnfs, umountroot, killprocs, single, rc, rcS);
//   - open a root shell or leave the running system: emergency, rescue,
//     debug-shell, console-getty, getty-static, and rc-local, which runs
//     /etc/rc.local;
//   - cut the server off the network, or change the firewall, which Block a
//     network range covers: networking, NetworkManager, dbus and dbus-broker
//     (logind and the network tools stop answering), nftables, firewalld,
//     iptables, ip6tables, netfilter-persistent, and ufw;
//   - install packages: apt-daily-upgrade, apt-daily, unattended-upgrades,
//     packagekit, and dnf-automatic.
var refusedServices = []string{
	"poweroff", "reboot", "halt", "kexec", "shutdown", "sendsigs", "umountfs", "umountnfs", "umountroot", "killprocs", "single", "rc", "rcs",
	"emergency", "rescue", "debug-shell", "console-getty", "getty-static", "rc-local",
	"networking", "networkmanager", "dbus", "dbus-broker", "nftables", "firewalld", "iptables", "ip6tables", "netfilter-persistent", "ufw",
	"apt-daily-upgrade", "apt-daily", "unattended-upgrades", "packagekit", "dnf-automatic",
}

// startableService accepts a service name that Start or restart a service
// may start or restart without a person. A name without a unit type is a
// .service, as MMX and systemctl read it (SystemdUnitName). Systemd's unit
// names are case-sensitive; the names are compared in any case, so a name
// that only differs in case is refused too.
func startableService(name string) bool {
	unit := SystemdUnitName(name)
	service, isService := strings.CutSuffix(unit, ".service")
	if !isService || len(unit) > maximumSystemdUnitNameLength || !plainServiceName.MatchString(service) {
		return false
	}
	lower := strings.ToLower(service)
	for _, prefix := range refusedServicePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return !slices.Contains(refusedServices, lower)
}

// runOnceDirectory is where MMX's documentation keeps the marker files of
// run_only_once_marker_file; MMX creates it.
const runOnceDirectory = "/var/lib/mmx/commands_run_once"

// runOnceMarkerName is a marker file's name: letters, digits, and _ . -,
// not starting with a dot.
var runOnceMarkerName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// runOnceMarkerFile accepts a marker file directly in runOnceDirectory. MMX
// writes the marker as root and creates its missing directories, so a marker
// such as /etc/nologin would lock out every login but root's. The path must be
// written plainly: no "..", ".", or empty part. No kind of action makes a
// symlink, so a symlink in the directory would be a person's.
func runOnceMarkerFile(marker string) bool {
	return path.Clean(marker) == marker && path.Dir(marker) == runOnceDirectory && runOnceMarkerName.MatchString(path.Base(marker))
}
