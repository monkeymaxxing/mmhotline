package actionkinds

import (
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// FuzzMMXExplainOutput reads fuzzed `mmx explain --json` output, which the
// agent's playbook shapes. A refusal gives no recognition. A recognition
// lists every node task once, in order; its kinds are those of its tasks,
// each judged alone, once each and in the order of Kinds(); its
// unrecognized tasks are each file a fleet stage saves on the controller
// machine, which makes Unrecognized task one of its kinds, then the node
// tasks whose kind is Unrecognized task. A module
// task recognized as a kind becomes an unrecognized task when it writes one
// more module argument, or when MMX does not know before the run whether it
// runs. A task recognized as Change DNS records sends the team's Cloudflare
// token only to the DNS records API of one zone on api.cloudflare.com, by a
// path that needs no escaping and with no Host header. A recognized task sends
// no value that may be a secret to a machine the playbook chooses: podman_image's
// image and registry login, and wait_for's url_responds and tcp_host, are
// known before the run (docs/changing-mmhotline/design/triage-agent.md#rules-for-running-the-agents-playbooks-and-kinds-of-action).
func FuzzMMXExplainOutput(f *testing.F) {
	var everyTask []string
	for _, sample := range sampleOneOffs {
		f.Add(explainOutput(sample.tasks...))
		everyTask = append(everyTask, sample.tasks...)
	}
	f.Add(explainOutput(everyTask...))
	f.Add(explainOutput(`{"path":"t/task_1","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.script","value":"rm -rf /","status":"literal"}]}`))
	f.Add(explainOutput(`{"path":"t/task_1","action":"http","module_kind":"core","status":"ready","arguments":[{"field":"http.url","value":"https://api.cloudflare.com/client/v4/zones/z/dns_records/../../accounts","status":"resolved"}]}`))
	f.Add(explainOutput(`{"path":"t/task_1","action":"podman_image","module_kind":"core","status":"ready","arguments":[{"field":"podman_image.image","value":"attacker.example/x:1","status":"resolved"},{"field":"podman_image.pull_image","value":"always","status":"literal"},{"field":"podman_image.registry_password","value":null,"status":"secret"},{"field":"podman_image.registry_username","value":"u","status":"resolved"}]}`))
	f.Add(explainOutput(`{"path":"t/task_1","action":"wait_for","module_kind":"core","status":"ready","arguments":[{"field":"wait_for.tcp_host","value":null,"status":"secret"},{"field":"wait_for.tcp_port_accepts_connections","value":443,"status":"literal"}]}`))
	f.Add(explainOutput(`{"path":"t/task_1","action":"http","module_kind":"core","status":"ready","arguments":[{"field":"http.headers.Host","value":"attacker.example","status":"resolved"},{"field":"http.url","value":"https://api.cloudflare.com/client/v4/zones/z/dns_records","status":"resolved"}]}`))
	f.Add([]byte(`{"schema_version":2,"fleet_stages":[]}`))
	f.Add([]byte(`{"schema_version":2,"fleet_stages":[{"path":"check_caddy","node_tasks":[{"path":"check_caddy/task_1","action":"show","status":"ready"}],"save_on_controller_machine":[{"template":"hook.tmpl","path":".git/hooks/pre-commit","files":[{"file":"/srv/systems/.git/hooks/pre-commit"}],"permissions_mode":"0755"}]}]}`))
	f.Fuzz(func(t *testing.T, output []byte) {
		recognition, err := Recognize(output)
		if err != nil {
			if recognition.Kinds != nil || recognition.Tasks != nil || recognition.UnrecognizedTasks != nil {
				t.Fatalf("a refusal gave the recognition %+v", recognition)
			}
			return
		}
		var explained explanation
		if err := json.Unmarshal(output, &explained); err != nil {
			t.Fatalf("recognized output that does not read: %v", err)
		}
		var tasks []explainedTask
		var saved []string
		for _, stage := range explained.FleetStages {
			tasks = append(tasks, stage.NodeTasks...)
			for range stage.SaveOnControllerMachine {
				saved = append(saved, "fleet stage "+stage.Path)
			}
		}
		if len(recognition.Tasks) != len(tasks) {
			t.Fatalf("%d tasks listed for %d node tasks", len(recognition.Tasks), len(tasks))
		}
		if len(recognition.UnrecognizedTasks) < len(saved) {
			t.Fatalf("%d unrecognized tasks for %d files saved on the controller machine", len(recognition.UnrecognizedTasks), len(saved))
		}
		for index, path := range saved {
			if entry := recognition.UnrecognizedTasks[index]; entry.Path != path || entry.Action != "save_on_controller_machine" || entry.Reason == "" {
				t.Fatalf("saved file %d listed as %+v, not in %s", index, entry, path)
			}
		}
		found := map[Kind]bool{UnrecognizedTask: len(saved) > 0}
		var unrecognized []Task
		for index, task := range tasks {
			if !sameTask(recognition.Tasks[index], Task(task)) {
				t.Fatalf("task %d listed as %+v, not %+v", index, recognition.Tasks[index], task)
			}
			kind := kindAlone(t, task)
			found[kind] = true
			if kind == UnrecognizedTask {
				unrecognized = append(unrecognized, Task(task))
			}
			checkKindOfOneTask(t, task, kind)
		}
		var kinds []Kind
		for _, kind := range Kinds() {
			if found[kind] {
				kinds = append(kinds, kind)
			}
		}
		if !slices.Equal(recognition.Kinds, kinds) || len(recognition.UnrecognizedTasks) != len(saved)+len(unrecognized) {
			t.Fatalf("kinds %v and %d unrecognized tasks, want %v and %d", recognition.Kinds, len(recognition.UnrecognizedTasks), kinds, len(saved)+len(unrecognized))
		}
		for index := range unrecognized {
			if got := recognition.UnrecognizedTasks[len(saved)+index]; !sameTask(got.Task, unrecognized[index]) || got.Reason == "" {
				t.Fatalf("unrecognized task %d: %+v, want %+v with a reason", index, got, unrecognized[index])
			}
		}
	})
}

func sameTask(a, b Task) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(encodedA) == string(encodedB)
}

// kindAlone recognizes a playbook of this one task.
func kindAlone(t *testing.T, task explainedTask) Kind {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"schema_version": 2, "fleet_stages": []any{map[string]any{"node_tasks": []explainedTask{task}}}})
	if err != nil {
		t.Fatal(err)
	}
	recognition, err := Recognize(encoded)
	if err != nil || len(recognition.Kinds) != 1 {
		t.Fatalf("the task %+v alone: %+v, %v", task, recognition, err)
	}
	return recognition.Kinds[0]
}

func checkKindOfOneTask(t *testing.T, task explainedTask, kind Kind) {
	t.Helper()
	if kind == UnrecognizedTask || task.ModuleKind == "" {
		return
	}
	extended := task
	extended.Arguments = append(slices.Clone(task.Arguments), Argument{Field: task.Action + ".an_argument_no_pattern_allows", Value: true, Status: "literal"})
	if added := kindAlone(t, extended); added != UnrecognizedTask {
		t.Fatalf("the %s task %+v with one more module argument is still %s", kind, task, added)
	}
	conditional := task
	conditional.Status = "conditional"
	if notReady := kindAlone(t, conditional); notReady != UnrecognizedTask {
		t.Fatalf("the %s task %+v is still %s when MMX does not know whether it runs", kind, task, notReady)
	}
	for _, argument := range task.Arguments {
		name, _ := task.argumentName(argument.Field)
		known := argument.Status == "literal" || argument.Status == "resolved"
		leavesTheServer := task.Action == "podman_image" && (name == "image" || name == "registry_username" || name == "registry_password") ||
			task.Action == "wait_for" && (name == "url_responds" || name == "tcp_host")
		if leavesTheServer && !known {
			t.Fatalf("the %s task %+v sends %s, which may be a secret, to a machine the playbook chooses", kind, task, argument.Field)
		}
		if task.Action != "http" || name != "headers" {
			continue
		}
		headers, isMap := argument.Value.(map[string]any)
		header, isOne := strings.CutPrefix(argument.Field, "http.headers.")
		if isOne && strings.EqualFold(header, "Host") || !isOne && (!known || !isMap) || slices.ContainsFunc(slices.Collect(maps.Keys(headers)), func(key string) bool { return strings.EqualFold(key, "Host") }) {
			t.Fatalf("Change DNS records with the header %s %+v, which could ask Cloudflare's servers for another site", argument.Field, argument)
		}
	}
	if kind != ChangeDNSRecords {
		return
	}
	address, _ := task.read("url")
	text, _ := address.(string)
	parsed, err := url.Parse(text)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.cloudflare.com" || parsed.User != nil || parsed.EscapedPath() != parsed.Path {
		t.Fatalf("Change DNS records for the address %q", text)
	}
	parts := strings.Split(parsed.Path, "/")
	if len(parts) < 6 || strings.Join(parts[:4], "/") != "/client/v4/zones" || parts[5] != "dns_records" || slices.ContainsFunc(parts[4:], func(part string) bool { return part == "" || part == "." || part == ".." }) {
		t.Fatalf("Change DNS records for the path %q", parsed.Path)
	}
}
