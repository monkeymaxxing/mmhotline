package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"mmhotline/internal/actionkinds"
)

// The services whose changes a person must approve: one list for the installation,
// for every inventory. A playbook with a node task that acts on one of them
// waits for a person whatever the rules for its kinds:
// docs/changing-mmhotline/design/triage-agent.md#services-whose-changes-a-person-must-approve. An empty
// list, the default, leaves the decision to the rules for running the agent's playbooks and the
// agent's instructions.

// servicesWhoseChangesAPersonMustApproveSchema is part of the setup schema.
// No row means nobody has set the list, which is then empty.
const servicesWhoseChangesAPersonMustApproveSchema = `
CREATE TABLE services_whose_changes_a_person_must_approve (
 id INTEGER PRIMARY KEY CHECK(id=1),
 service_names TEXT NOT NULL CHECK(json_type(service_names)='array'),
 changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL
);
`

// ServicesWhoseChangesAPersonMustApprove is the list as saved: unit names such as
// pgautofailover.service, in the order given.
type ServicesWhoseChangesAPersonMustApprove struct {
	ServiceNames []string `json:"service_names"`
	// ChangedAt and ChangedBy are absent while nobody has set the list.
	ChangedAt *time.Time `json:"changed_at,omitempty"`
	ChangedBy string     `json:"changed_by,omitempty"`
}

// Text is the list in one line, as agent-playbook-rules show prints it.
func (list ServicesWhoseChangesAPersonMustApprove) Text() string {
	names := "none"
	if len(list.ServiceNames) > 0 {
		names = strings.Join(list.ServiceNames, ", ")
	}
	line := "Services whose changes a person must approve, on every inventory: " + names
	if list.ChangedAt == nil {
		return line + "; never set."
	}
	return fmt.Sprintf("%s; changed by %s at %s.", line, list.ChangedBy, commandTime(*list.ChangedAt))
}

func readServicesWhoseChangesAPersonMustApprove(ctx context.Context, tx *sql.Tx) (ServicesWhoseChangesAPersonMustApprove, error) {
	list := ServicesWhoseChangesAPersonMustApprove{ServiceNames: []string{}}
	var names string
	var changedAt int64
	err := tx.QueryRowContext(ctx, `SELECT service_names,changed_at,changed_by FROM services_whose_changes_a_person_must_approve WHERE id=1`).Scan(&names, &changedAt, &list.ChangedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return list, nil
	}
	if err != nil {
		return list, err
	}
	if err = json.Unmarshal([]byte(names), &list.ServiceNames); err != nil {
		return list, err
	}
	at := time.Unix(changedAt, 0).UTC()
	list.ChangedAt = &at
	return list, nil
}

// ServicesWhoseChangesAPersonMustApprove reads the list. Every member may read it.
func (a *App) ServicesWhoseChangesAPersonMustApprove(ctx context.Context, p Principal) (ServicesWhoseChangesAPersonMustApprove, error) {
	var list ServicesWhoseChangesAPersonMustApprove
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		list, err = readServicesWhoseChangesAPersonMustApprove(ctx, tx)
		return err
	})
	return list, err
}

// servicesWhoseChangesAPersonMustApproveUnit is the unit name saved for one listed name: a
// name without a unit type gets .service, as systemctl reads it, so
// pgautofailover and pgautofailover.service name one service, which may be
// listed once. units are the names listed before it.
func servicesWhoseChangesAPersonMustApproveUnit(name string, units []string) (string, error) {
	if err := actionkinds.CheckSystemdUnitName(name); err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	unit := actionkinds.SystemdUnitName(name)
	if slices.Contains(units, unit) {
		twice := unit + " is listed twice"
		if service, isService := strings.CutSuffix(unit, ".service"); isService {
			twice += "; a name without a unit type means a .service, so " + service + " is " + unit
		}
		return "", fmt.Errorf("%w: %s", ErrInvalid, twice)
	}
	return unit, nil
}

// SetServicesWhoseChangesAPersonMustApprove replaces the whole list. An empty list is
// valid.
func (a *App) SetServicesWhoseChangesAPersonMustApprove(ctx context.Context, p Principal, names []string) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		person, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		units := []string{}
		for _, name := range names {
			unit, err := servicesWhoseChangesAPersonMustApproveUnit(name, units)
			if err != nil {
				return err
			}
			units = append(units, unit)
		}
		encoded, err := json.Marshal(units)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO services_whose_changes_a_person_must_approve(id,service_names,changed_at,changed_by) VALUES(1,?,?,?)
ON CONFLICT(id) DO UPDATE SET service_names=excluded.service_names,changed_at=excluded.changed_at,changed_by=excluded.changed_by`, string(encoded), a.Now().Unix(), person.Name)
		return err
	})
}

// taskOnAServiceWhoseChangesAPersonMustApprove is a node task that makes its playbook wait
// for a person whatever the rules for running the agent's playbooks.
type taskOnAServiceWhoseChangesAPersonMustApprove struct {
	actionkinds.Task
	// ServiceName is the listed unit the task acts on; empty when MMX knows
	// the unit's name only during the run.
	ServiceName string
	// Reason says why in plain words, such as "restarts
	// pgautofailover.service on db01, which is on the list of services whose
	// changes a person must approve".
	Reason string
}

// tasksOnServicesWhoseChangesAPersonMustApprove is the one rule of the list, which the
// decision and the approval card both use: the node tasks that act on a
// listed service and, while the list has any, those that act on a unit whose
// name MMX knows only during the run, which could be a listed one. With an
// empty list, no task needs a person for it.
func tasksOnServicesWhoseChangesAPersonMustApprove(recognition actionkinds.Recognition, serviceNames []string) []taskOnAServiceWhoseChangesAPersonMustApprove {
	if len(serviceNames) == 0 {
		return nil
	}
	var tasks []taskOnAServiceWhoseChangesAPersonMustApprove
	for _, task := range recognition.UnitTasks() {
		switch {
		case task.Unit == "":
			tasks = append(tasks, taskOnAServiceWhoseChangesAPersonMustApprove{task.Task, "", task.Does + " a unit on " + task.Node + " whose name MMX knows only when the task runs, so the unit could be on the list of services whose changes a person must approve"})
		case slices.Contains(serviceNames, task.Unit):
			tasks = append(tasks, taskOnAServiceWhoseChangesAPersonMustApprove{task.Task, task.Unit, task.Does + " " + task.Unit + " on " + task.Node + ", which is on the list of services whose changes a person must approve"})
		}
	}
	return tasks
}

// changeServicesWhoseChangesAPersonMustApproveText tells the agent and people how the list
// changes.
const changeServicesWhoseChangesAPersonMustApproveText = "An administrator changes the list in services_whose_changes_a_person_must_approve in MMHotline's settings file."

// servicesWhoseChangesAPersonMustApproveText names the tasks on services whose changes a person
// must approve, each with why, such as " Task restart_postgres/task_1 restarts
// pgautofailover.service on db01, which is on the list of services whose
// changes a person must approve." and how the list changes; past
// maximumTasksNamedInAnswer it counts the rest. With none, it is empty.
func servicesWhoseChangesAPersonMustApproveText(tasks []taskOnAServiceWhoseChangesAPersonMustApprove) string {
	if len(tasks) == 0 {
		return ""
	}
	var entries []string
	for index, task := range tasks {
		if index == maximumTasksNamedInAnswer {
			entries = append(entries, fmt.Sprintf("And %d more such tasks.", len(tasks)-index))
			break
		}
		entries = append(entries, "Task "+task.Path+" "+task.Reason+".")
	}
	return " " + strings.Join(append(entries, changeServicesWhoseChangesAPersonMustApproveText), " ")
}
