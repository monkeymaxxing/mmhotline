package actionkinds

import (
	"fmt"
	"strings"
)

// What each kind of action accepts, from the table the matcher uses, for the
// triage agent's task, the page of rules for running the agent's playbooks, and the design:
// docs/changing-mmhotline/design/triage-agent.md#the-kinds-of-action. The texts avoid the
// design's term "node task", because people read them on the page.

// AcceptedTasks is what one kind of action accepts: the tasks of each module
// that are of the kind.
type AcceptedTasks struct {
	Kind    Kind
	Modules []AcceptedModule
}

// AcceptedModule is the tasks of one module that a kind accepts.
type AcceptedModule struct {
	Module string
	// Arguments are the module arguments a task may write; nil allows any.
	Arguments []string
	// Conditions must all hold, in plain words.
	Conditions []string
	// Examples are tasks in YAML, as a playbook's node_tasks list them.
	Examples []string
}

// AcceptedTasksRules says what every task of a kind must do, before what
// each module's tasks must do.
const AcceptedTasksRules = `MMHotline reads each task of a playbook from mmx explain, never from the playbook's name or the agent's words, and sorts each task into a kind of action. A task is of a kind only when all of these hold:
- the task's module is one of MMX's core modules listed below, not a module of the team's own or an official extra module;
- mmx explain lists the task as ready: MMX knows before the run that the task runs, which a when usually prevents;
- the task writes no argument outside its module's list;
- each value a condition reads is known before the run: not a secret and not another task's result.
Each task inside a role or a task block is sorted by itself. Every other task is an Unrecognized task, and so is each file a fleet stage saves on MMHotline's server with save_on_controller_machine.`

// AcceptedTasksOfEachKind lists what each kind accepts, in the order of
// Kinds(); Unrecognized task accepts no module.
func AcceptedTasksOfEachKind() []AcceptedTasks {
	var kinds []AcceptedTasks
	for _, kind := range Kinds() {
		accepted := AcceptedTasks{Kind: kind}
		for _, rule := range patterns {
			if rule.kind != kind {
				continue
			}
			module := AcceptedModule{Module: rule.module, Arguments: rule.arguments, Examples: rule.examples}
			for _, condition := range rule.conditions {
				module.Conditions = append(module.Conditions, condition.text)
			}
			accepted.Modules = append(accepted.Modules, module)
		}
		if len(accepted.Modules) > 0 {
			kinds = append(kinds, accepted)
		}
	}
	return kinds
}

// AcceptedTasksText says, for the triage agent, what each kind accepts:
// AcceptedTasksRules, then for each kind its modules with their arguments,
// conditions, and examples.
func AcceptedTasksText() string {
	paragraphs := []string{AcceptedTasksRules}
	for _, accepted := range AcceptedTasksOfEachKind() {
		var kind strings.Builder
		fmt.Fprintf(&kind, "%s (%s):", accepted.Kind.Title(), accepted.Kind)
		for _, module := range accepted.Modules {
			fmt.Fprintf(&kind, "\n- %s. Arguments: %s.", module.Module, module.ArgumentsText())
			if len(module.Conditions) > 0 {
				kind.WriteString(" Conditions: " + strings.Join(module.Conditions, "; ") + ".")
			}
			for _, example := range module.Examples {
				kind.WriteString(" Example:\n\n    " + strings.ReplaceAll(example, "\n", "\n    ") + "\n")
			}
		}
		paragraphs = append(paragraphs, strings.TrimSuffix(kind.String(), "\n"))
	}
	return strings.Join(paragraphs, "\n\n")
}

// ArgumentsText lists the arguments, such as "name, state", or says that any
// are accepted.
func (module AcceptedModule) ArgumentsText() string {
	if module.Arguments == nil {
		return "any"
	}
	return strings.Join(module.Arguments, ", ")
}
