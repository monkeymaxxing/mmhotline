package actionkinds

import (
	"fmt"
	"regexp"
)

// The node tasks that act on a systemd unit, which MMHotline compares with
// the team's list of services whose changes a person must approve:
// docs/changing-mmhotline/design/triage-agent.md#services-whose-changes-a-person-must-approve.

// systemdUnitNameCharacters are the characters MMX accepts in a unit name,
// as systemd does: letters, digits, and _ . @ : \ -, starting with a letter,
// a digit, or _. MMX's internal/modules/unit.go owns both patterns.
var (
	systemdUnitNameCharacters = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:\\-]*$`)
	systemdUnitNameWithKind   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:\\-]*\.(service|timer|path|socket|mount|target|swap|slice|automount)$`)
)

// maximumSystemdUnitNameLength is systemd's own limit for a unit name with
// its unit type: UNIT_NAME_MAX is 256 bytes with the closing zero byte.
const maximumSystemdUnitNameLength = 255

// SystemdUnitName is the unit a name stands for, as systemctl and MMX read
// it: a name without a unit type means a .service, so pgautofailover is
// pgautofailover.service.
func SystemdUnitName(name string) string {
	if systemdUnitNameWithKind.MatchString(name) {
		return name
	}
	return name + ".service"
}

// CheckSystemdUnitName says, in plain words, why name is no systemd unit
// name, or returns nil.
func CheckSystemdUnitName(name string) error {
	if !systemdUnitNameCharacters.MatchString(name) {
		return fmt.Errorf("%q is not a systemd unit name: a unit name has only letters, digits, and the characters _ . @ : \\ -, and starts with a letter, a digit, or _", name)
	}
	if length := len(SystemdUnitName(name)); length > maximumSystemdUnitNameLength {
		return fmt.Errorf("systemd allows at most %d characters in a unit name, including the unit type; the unit name with its unit type has %d", maximumSystemdUnitNameLength, length)
	}
	return nil
}

// UnitTask is a node task that acts on a systemd unit: a systemd_service or
// a systemd_unit task.
type UnitTask struct {
	Task
	// Unit is the unit the task names, as SystemdUnitName reads it, such as
	// pgautofailover.service; empty when MMX knows the name only during the
	// run.
	Unit string
	// Does says what the task does to the unit, such as restarts.
	Does string
}

// UnitTasks lists the node tasks that act on a systemd unit, in the order
// mmx explain lists them, except those MMX knows it skips. A module of the
// team's own with the name systemd_service counts too: MMHotline cannot tell
// what it does to the unit it names.
func (recognition Recognition) UnitTasks() []UnitTask {
	var tasks []UnitTask
	for _, task := range recognition.Tasks {
		if (task.Action != "systemd_service" && task.Action != "systemd_unit") || task.Status == "skipped" {
			continue
		}
		explained := explainedTask(task)
		listed := UnitTask{Task: task, Does: explained.doesToUnit()}
		if name, known := explained.read("name"); known {
			if text, isText := name.(string); isText && text != "" {
				listed.Unit = SystemdUnitName(text)
			}
		}
		tasks = append(tasks, listed)
	}
	return tasks
}

// doesToUnit says what a systemd_service or systemd_unit task does to its
// unit, in plain words, from the values MMX knows before the run.
func (task explainedTask) doesToUnit() string {
	if task.Action == "systemd_unit" {
		return "changes the unit file of"
	}
	restart, _ := task.read("restart_now")
	state, _ := task.read("state")
	switch {
	case restart == true:
		return "restarts"
	case state == "running":
		return "starts"
	case state == "stopped":
		return "stops"
	}
	return "changes"
}
