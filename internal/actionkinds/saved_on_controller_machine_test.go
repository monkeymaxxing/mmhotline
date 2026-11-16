package actionkinds

import (
	"reflect"
	"strings"
	"testing"
)

// Each file a fleet stage saves on the controller machine, MMHotline's own
// server, is an unrecognized task, whatever the fleet stage's node tasks are
// and also when it selects no servers, which still writes a template entry's
// file. The reason names the files, or says MMX knows them only during the
// run, and they come before the node tasks.
func TestSavingOnTheControllerMachineIsUnrecognized(t *testing.T) {
	const show = `{"path":"check_caddy/task_1","node":"app002","action":"show","status":"ready","arguments":[{"field":"show","value":"Caddy answers again.","status":"resolved"}]}`
	stage := func(path, nodes, nodeTasks, saved string) string {
		return `{"path":"` + path + `",` + nodes + `,"source":{"file":"one_time_playbooks/x.yml","line":2,"column":5,"field":"fleet_stages[0]"},"node_tasks":[` + nodeTasks + `],"save_on_controller_machine":[` + saved + `]}`
	}
	template := func(path, files string) string {
		return `{"template":"one_time_playbooks/hook.tmpl","template_file":"/var/lib/mmhotline/systems_repository/one_time_playbooks/hook.tmpl","path":"` + path + `",` + files + `,"permissions_mode":"0755","source":{"file":"one_time_playbooks/x.yml","line":7,"column":9,"field":"fleet_stages[0].save_on_controller_machine[0]"}}`
	}
	output := func(files string) string {
		return `{"output":"version","path":"versions/{{ .node.name }}.txt",` + files + `,"permissions_mode":"0644","source":{"file":"one_time_playbooks/x.yml","line":7,"column":9,"field":"fleet_stages[0].save_on_controller_machine[0]"}}`
	}
	const servers = `"selection":"app002","nodes":["app002"]`
	const noServers = `"selection":"retired","left_out_because_it_selects_no_nodes":true`
	saves := func(stage, reason string) Unrecognized {
		return Unrecognized{Task{Path: "fleet stage " + stage, Action: "save_on_controller_machine"}, reason}
	}
	for _, test := range []struct {
		name   string
		stages []string
		kinds  []Kind
		want   []Unrecognized
	}{
		{"a show-only fleet stage that saves a Git hook in MMHotline's copy",
			[]string{stage("check_caddy", servers, show, template(".git/hooks/pre-commit", `"files":[{"file":"/var/lib/mmhotline/systems_repository/.git/hooks/pre-commit"}]`))},
			[]Kind{CheckOnly, UnrecognizedTask},
			[]Unrecognized{saves("check_caddy", "the fleet stage writes /var/lib/mmhotline/systems_repository/.git/hooks/pre-commit on MMHotline's server, with permissions_mode 0755")}},
		{"a path MMX resolves only during the run",
			[]string{stage("save", servers, show, template("saved/{{ .outputs.find.app002.place }}.txt", `"files_resolved_during_run":true`))},
			[]Kind{CheckOnly, UnrecognizedTask},
			[]Unrecognized{saves("save", "the fleet stage writes files on MMHotline's server whose names MMX knows only during the run, from the path saved/{{ .outputs.find.app002.place }}.txt, with permissions_mode 0755")}},
		// MMX writes a template entry's file even when its fleet stage
		// selects no servers and is left out of the run.
		{"a fleet stage that selects no servers",
			[]string{stage("check_caddy", servers, show, ""), stage("retired_servers", noServers, "", template("/var/lib/mmhotline/.ssh/authorized_keys", `"files":[{"file":"/var/lib/mmhotline/.ssh/authorized_keys"}]`))},
			[]Kind{CheckOnly, UnrecognizedTask},
			[]Unrecognized{saves("retired_servers", "the fleet stage writes /var/lib/mmhotline/.ssh/authorized_keys on MMHotline's server, with permissions_mode 0755")}},
		{"a playbook whose only fleet stage selects no servers",
			[]string{stage("retired_servers", noServers, "", template("run_mmx", `"files":[{"file":"/var/lib/mmhotline/systems_repository/run_mmx"}]`))},
			[]Kind{UnrecognizedTask},
			[]Unrecognized{saves("retired_servers", "the fleet stage writes /var/lib/mmhotline/systems_repository/run_mmx on MMHotline's server, with permissions_mode 0755")}},
		{"an output entry for five servers",
			[]string{stage("versions", `"selection":"app","nodes":["app001","app002","app003","app004","app005"]`, show, output(`"files":[{"node":"app001","file":"/v/app001.txt"},{"node":"app002","file":"/v/app002.txt"},{"node":"app003","file":"/v/app003.txt"},{"node":"app004","file":"/v/app004.txt"},{"node":"app005","file":"/v/app005.txt"}]`))},
			[]Kind{CheckOnly, UnrecognizedTask},
			[]Unrecognized{saves("versions", "the fleet stage writes /v/app001.txt, /v/app002.txt, /v/app003.txt, and 2 more on MMHotline's server, with permissions_mode 0644")}},
		{"an output entry of a fleet stage that selects no servers",
			[]string{stage("check_caddy", servers, show, ""), stage("versions", noServers, "", output(`"files":[]`))},
			[]Kind{CheckOnly, UnrecognizedTask},
			[]Unrecognized{saves("versions", "the fleet stage writes a file on MMHotline's server for each server the fleet stage selects (none now), from the path versions/{{ .node.name }}.txt, with permissions_mode 0644")}},
	} {
		recognition, err := Recognize([]byte(`{"schema_version":2,"project":"systems","fleet_stages":[` + strings.Join(test.stages, ",") + `]}`))
		if err != nil || !reflect.DeepEqual(recognition.Kinds, test.kinds) || !reflect.DeepEqual(recognition.UnrecognizedTasks, test.want) {
			t.Errorf("%s: %+v %v\nwant %v %+v", test.name, recognition, err, test.kinds, test.want)
		}
	}

	// The saved files come before the node tasks MMHotline did not
	// recognize, so the bound on an answer never leaves them out.
	shell := `{"path":"fix/task_1","node":"app002","action":"shell","module_kind":"core","status":"ready"}`
	recognition, err := Recognize([]byte(`{"schema_version":2,"fleet_stages":[` + stage("fix", servers, shell, "") + `,` + stage("check_caddy", servers, show, template(".git/hooks/pre-commit", `"files":[{"file":"/srv/systems/.git/hooks/pre-commit"}]`)) + `]}`))
	if err != nil || len(recognition.UnrecognizedTasks) != 2 || recognition.UnrecognizedTasks[0].Action != "save_on_controller_machine" || recognition.UnrecognizedTasks[1].Path != "fix/task_1" {
		t.Errorf("order: %+v %v", recognition, err)
	}
}
