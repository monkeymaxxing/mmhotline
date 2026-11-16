package actionkinds

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// explainExample is what mmx explain prints for an example task, whose
// values the playbook writes: each module argument, known before the run.
func explainExample(t *testing.T, rule pattern, example string) []byte {
	t.Helper()
	var tasks []map[string]any
	if err := yaml.Unmarshal([]byte(example), &tasks); err != nil || len(tasks) != 1 || len(tasks[0]) != 1 {
		t.Fatalf("%s's example is not one task: %v\n%s", rule.module, err, example)
	}
	value, ok := tasks[0][rule.module]
	if !ok {
		t.Fatalf("%s's example is a task of another module:\n%s", rule.module, example)
	}
	var arguments []Argument
	if settings, isMapping := value.(map[string]any); isMapping {
		for name, argument := range settings {
			arguments = append(arguments, Argument{Field: rule.module + "." + name, Value: argument, Status: "literal"})
		}
	} else {
		arguments = append(arguments, Argument{Field: rule.module, Value: value, Status: "literal"})
	}
	task := explainedTask{Path: "example/task_1", Node: "app002", Action: rule.module, ModuleKind: rule.moduleKind, Status: "ready", Arguments: arguments}
	output, err := json.Marshal(map[string]any{"schema_version": 2, "fleet_stages": []any{map[string]any{"node_tasks": []explainedTask{task}}}})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

// The examples the agent and people read are recognized as their kind, so
// the forms they read are the forms the matcher accepts. Every module has
// one; a role call and a task block would need a role or a block of their own.
func TestEveryExampleIsRecognizedAsItsKind(t *testing.T) {
	for _, rule := range patterns {
		if len(rule.examples) == 0 && rule.module != "role" && rule.module != "task_block" {
			t.Errorf("%s has no example", rule.module)
		}
		for _, example := range rule.examples {
			recognition, err := Recognize(explainExample(t, rule, example))
			if err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{rule.kind}) {
				t.Errorf("%s's example is %+v %v, not %s:\n%s", rule.module, recognition, err, rule.kind, example)
			}
		}
	}
}

// The design shows the text the agent reads, so a reader of the design sees
// what the agent and people see.
func TestDesignShowsTheAcceptedTasks(t *testing.T) {
	design, err := os.ReadFile("../../docs/changing-mmhotline/design/triage-agent.md")
	if err != nil {
		t.Fatal(err)
	}
	const start = "<!-- The text below is actionkinds.AcceptedTasksText(); TestDesignShowsTheAcceptedTasks keeps it equal. -->\n\n```text\n"
	_, block, found := strings.Cut(string(design), start)
	block, _, closed := strings.Cut(block, "\n```\n")
	if want := AcceptedTasksText(); !found || !closed || block != want {
		t.Fatalf("docs/changing-mmhotline/design/triage-agent.md must hold, after %q, the accepted tasks:\n\n%s", start, want)
	}
}
