package actionkinds

import (
	"reflect"
	"testing"
)

// ProjectFiles reads each path a task reads from the controller machine,
// from the role's directory inside a role and the project root otherwise,
// and never a path that leaves the project.
func TestProjectFiles(t *testing.T) {
	output := []byte(`{"schema_version":2,"fleet_stages":[{"path":"fix","node_tasks":[
{"path":"fix/task_1","action":"directory","module_kind":"core","status":"ready","source":{"file":"one_time_playbooks/x.yml"},"arguments":[{"field":"directory.path","value":"/etc/x.d","status":"resolved"},{"field":"directory.source","value":"conf.d","status":"literal"}]},
{"path":"fix/role/task_1","action":"authorized_keys","module_kind":"core","status":"ready","source":{"file":"roles/access/main.yml"},"arguments":[{"field":"authorized_keys.key_files_on_controller_machine","value":["keys/a.pub","../../keys/b.pub","../../../etc/passwd"],"status":"literal"},{"field":"authorized_keys.key_directories_on_controller_machine","value":["team"],"status":"literal"}]},
{"path":"fix/task_3","action":"systemd_unit","module_kind":"core","status":"ready","source":{"file":"playbooks/web.yml"},"arguments":[{"field":"systemd_unit.name","value":"web.service","status":"resolved"},{"field":"systemd_unit.template","value":"templates/web.service","status":"literal"}]},
{"path":"fix/task_4","action":"show","status":"ready","arguments":[{"field":"show","value":"source","status":"resolved"}]}]}]}`)
	files, err := ProjectFiles(output)
	want := []ProjectFile{
		{TaskPath: "fix/task_1", Field: "directory.source", Written: "conf.d", Path: "conf.d", Directory: true, KnownBeforeRun: true},
		{TaskPath: "fix/role/task_1", Field: "authorized_keys.key_files_on_controller_machine", Written: "keys/a.pub", Path: "roles/access/keys/a.pub", KnownBeforeRun: true},
		{TaskPath: "fix/role/task_1", Field: "authorized_keys.key_files_on_controller_machine", Written: "../../keys/b.pub", Path: "keys/b.pub", KnownBeforeRun: true},
		{TaskPath: "fix/role/task_1", Field: "authorized_keys.key_files_on_controller_machine", Written: "../../../etc/passwd", KnownBeforeRun: true},
		{TaskPath: "fix/role/task_1", Field: "authorized_keys.key_directories_on_controller_machine", Written: "team", Path: "roles/access/team", Directory: true, KnownBeforeRun: true},
		{TaskPath: "fix/task_3", Field: "systemd_unit.template", Written: "templates/web.service", Path: "templates/web.service", KnownBeforeRun: true},
	}
	if err != nil || !reflect.DeepEqual(files, want) {
		t.Fatalf("%v\n%+v\nwant\n%+v", err, files, want)
	}
}
