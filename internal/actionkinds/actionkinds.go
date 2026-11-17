// Package actionkinds recognizes what an MMX playbook does from the node
// tasks and the files saved on the controller machine that `mmx explain
// PLAYBOOK --inventory NAME --json` lists, never from the playbook's name or
// the agent's words:
// docs/changing-mmhotline/design/triage-agent.md#rules-for-running-the-agents-playbooks-and-kinds-of-action.
package actionkinds

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Kind is one kind of action a node task can be.
type Kind string

const (
	StartOrRestartAService   Kind = "start_or_restart_a_service"
	FreeDiskSpace            Kind = "free_disk_space"
	PullAContainerImageAgain Kind = "pull_a_container_image_again"
	RebootAServer            Kind = "reboot_a_server"
	BlockANetworkRange       Kind = "block_a_network_range"
	ChangeDNSRecords         Kind = "change_dns_records"
	CheckOnly                Kind = "check_only"
	UnrecognizedTask         Kind = "unrecognized_task"
)

var titles = map[Kind]string{
	StartOrRestartAService:   "Start or restart a service",
	FreeDiskSpace:            "Free disk space",
	PullAContainerImageAgain: "Pull a container image again",
	RebootAServer:            "Reboot a server",
	BlockANetworkRange:       "Block a network range",
	ChangeDNSRecords:         "Change DNS records",
	CheckOnly:                "Check only",
	UnrecognizedTask:         "Unrecognized task",
}

// Kinds lists every kind in the order people see them.
func Kinds() []Kind {
	return []Kind{StartOrRestartAService, FreeDiskSpace, PullAContainerImageAgain, RebootAServer, BlockANetworkRange, ChangeDNSRecords, CheckOnly, UnrecognizedTask}
}

// Title names the kind for people, such as "Reboot a server"; it is empty for
// a value that is no kind.
func (kind Kind) Title() string { return titles[kind] }

// Recognition is what one playbook does on one inventory.
type Recognition struct {
	// Kinds are the kinds of the playbook's node tasks, each once, in the
	// order of Kinds().
	Kinds []Kind
	// UnrecognizedTasks are the files the playbook's fleet stages save on the
	// controller machine, first, then the node tasks that matched no other
	// kind, with why, so the agent learns how to write each differently and
	// a person who approves sees why it waits.
	UnrecognizedTasks []Unrecognized
	// Tasks are every node task in the order mmx explain lists them, which
	// the review agent reads.
	Tasks []Task
}

// Task is one node task as mmx explain lists it.
type Task struct {
	Path   string // such as free_disk_space/task_1
	Node   string // the server it runs on, such as app002
	Action string // the module, or role, task_block, show, or set_vars
	// ModuleKind is core, official_extra, project_starlark, or
	// project_executable, and empty for role, task_block, show, and set_vars.
	ModuleKind string
	Status     string // ready, conditional, or skipped
	Arguments  []Argument
}

// Unrecognized is a node task that matched no other kind, or one
// save_on_controller_machine entry of a fleet stage, whose Path is "fleet
// stage PATH" and Action save_on_controller_machine.
type Unrecognized struct {
	Task
	// Reason says why in plain words, such as "state must be running".
	Reason string
}

// The parts of mmx explain's JSON that recognition reads: MMX's
// internal/config/inspect.go and arguments.go own the format. Version 2 is
// the first that lists save_on_controller_machine.
type explanation struct {
	SchemaVersion int `json:"schema_version"`
	FleetStages   []struct {
		Path                    string                     `json:"path"`
		NodeTasks               []explainedTask            `json:"node_tasks"`
		SaveOnControllerMachine []savedOnControllerMachine `json:"save_on_controller_machine"`
	} `json:"fleet_stages"`
}

// savedOnControllerMachine is one save_on_controller_machine entry of a fleet
// stage: after the fleet stage's nodes finish, MMX writes its files on the
// controller machine, which is MMHotline's own server. MMX lists it on every
// fleet stage, also one that selects no nodes, which still writes a template
// entry's file.
type savedOnControllerMachine struct {
	Path  string `json:"path"` // as written
	Files []struct {
		File string `json:"file"` // absolute
	} `json:"files"`
	// FilesResolvedDuringRun: MMX knows the files only during the run.
	FilesResolvedDuringRun bool   `json:"files_resolved_during_run"`
	PermissionsMode        string `json:"permissions_mode"`
}
type explainedTask struct {
	Path   string `json:"path"`
	Node   string `json:"node"`
	Action string `json:"action"`
	// ModuleKind is core, official_extra, project_starlark, or
	// project_executable, and absent for role, task_block, show, and set_vars.
	ModuleKind string `json:"module_kind"`
	// Status is ready when MMX knows before the run that the task runs, and
	// conditional or skipped when a when decides it.
	Status    string     `json:"status"`
	Arguments []Argument `json:"arguments"`
}

// Argument is one argument of a node task as mmx explain lists it.
type Argument struct {
	// Field is the module's name and the argument's, such as
	// systemd_service.name, or a part of one, such as
	// ufw.incoming_rules[0].from; or a setting of the task, such as when.
	Field string `json:"field"`
	Value any    `json:"value"`
	// Status is literal or resolved when MMX knew the value before the run;
	// runtime, secret, condition, conditional, and skipped values are not
	// known or not shown.
	Status string `json:"status"`
}

// pattern is the rule for the node tasks of one module: such a task is of
// kind when its module kind is moduleKind, it writes no module argument
// outside arguments (nil allows any), and every condition holds. The triage
// agent and people read the same table (AcceptedTasksText), so what they
// read is what the matcher does; a test checks that each example is
// recognized as its kind.
type pattern struct {
	module     string
	kind       Kind
	moduleKind string
	arguments  []string
	conditions []condition
	// examples are node tasks of the pattern in YAML, as a playbook's
	// node_tasks list them.
	examples []string
}

// condition is one rule of a pattern: text says it in plain words, and holds
// checks it.
type condition struct {
	text  string
	holds func(explainedTask) bool
}

// A task setting outside the module only narrows when the task runs, and MMX
// lists a loop's tasks one by one.
var taskSettings = []string{"when", "loop"}

// patterns are in the order people read them, by kind in the order of
// Kinds().
var patterns = []pattern{
	// MMX's systemd_service has no restarted state: restart_now restarts a
	// running service once, and the marker file keeps a rerun from
	// restarting it again.
	{module: "systemd_service", kind: StartOrRestartAService, moduleKind: "core",
		arguments: []string{"name", "state", "restart_now", "run_only_once_marker_file", "in_systemd_of_user_account"},
		conditions: []condition{
			{"systemd_service.state must be running", func(task explainedTask) bool {
				state, ok := task.read("state")
				return ok && state == "running"
			}},
			{"systemd_service.restart_now must be true or false", func(task explainedTask) bool {
				restart, ok := task.read("restart_now")
				return !task.writes("restart_now") || ok && (restart == true || restart == false)
			}},
			{"systemd_service.restart_now: true requires run_only_once_marker_file", func(task explainedTask) bool {
				restart, _ := task.read("restart_now")
				_, marker := task.read("run_only_once_marker_file")
				return restart != true || marker
			}},
		},
		examples: []string{`- systemd_service:
    name: caddy
    state: running
    restart_now: true
    run_only_once_marker_file: /var/lib/mmx/commands_run_once/2026-11-03_restart_caddy`}},
	{module: "systemd_journal_vacuum", kind: FreeDiskSpace, moduleKind: "core",
		arguments: []string{"keep_at_most_size", "keep_newer_than"},
		examples: []string{`- systemd_journal_vacuum:
    keep_at_most_size: 500MiB`}},
	{module: "podman_unused_images", kind: FreeDiskSpace, moduleKind: "core",
		arguments: []string{"remove", "in_image_store_of_user_account"},
		examples: []string{`- podman_unused_images:
    remove: dangling_images_only`}},
	{module: "docker_unused_images", kind: FreeDiskSpace, moduleKind: "core",
		arguments: []string{"remove", "in_image_store_of_user_account"},
		examples: []string{`- docker_unused_images:
    remove: dangling_images_only`}},
	{module: "apt_downloaded_package_files", kind: FreeDiskSpace, moduleKind: "core",
		arguments: []string{"state"},
		conditions: []condition{{"apt_downloaded_package_files.state must be absent", func(task explainedTask) bool {
			state, ok := task.read("state")
			return ok && state == "absent"
		}}},
		examples: []string{`- apt_downloaded_package_files:
    state: absent`}},
	// Old files elsewhere, such as a database's, can be the data itself.
	// Within /var/log, only the files logrotate leaves behind are old logs:
	// deleting a live log that a process still has open frees no space (the
	// process keeps writing to the deleted file), and fresh logs are the
	// evidence of an incident. So the age must be at least a day and every
	// pattern must name rotated logs. Any other delete is not refused, only
	// left to a person to approve.
	{module: "delete_old_files", kind: FreeDiskSpace, moduleKind: "core",
		arguments: []string{"directory", "name_patterns", "older_than", "include_subdirectories"},
		conditions: []condition{
			{"delete_old_files.directory must be /var/log itself (include_subdirectories: true also selects log directories below /var/log)", func(task explainedTask) bool {
				directory, ok := task.read("directory")
				text, isText := directory.(string)
				return ok && isText && isVarLog(text)
			}},
			{"delete_old_files.older_than must be at least 1d: a fresh log is the evidence of an incident", func(task explainedTask) bool {
				olderThan, ok := task.read("older_than")
				text, isText := olderThan.(string)
				return ok && isText && atLeastOneDay(text)
			}},
			{"delete_old_files.name_patterns must be only rotated-log patterns (" + strings.Join(rotatedLogPatterns, ", ") + "): a live log that a process has open frees no space when deleted", func(task explainedTask) bool {
				patterns, ok := task.read("name_patterns")
				list, isList := patterns.([]any)
				if !ok || !isList || len(list) == 0 {
					return false
				}
				for _, item := range list {
					text, isText := item.(string)
					if !isText || !slices.Contains(rotatedLogPatterns, text) {
						return false
					}
				}
				return true
			}},
		},
		examples: []string{`- delete_old_files:
    directory: /var/log
    name_patterns: ["*.gz", "*.[0-9]"]
    older_than: 7d
    include_subdirectories: true`}},
	// Podman logs in to the registry that image names, so a login from the
	// vault would reach any registry the agent writes: only a login and a
	// registry known before the run, with no secret, run without a person.
	{module: "podman_image", kind: PullAContainerImageAgain, moduleKind: "core",
		arguments: []string{"image", "pull_image", "registry_username", "registry_password", "in_image_store_of_user_account"},
		conditions: []condition{
			{"podman_image.pull_image must be always", func(task explainedTask) bool {
				pull, ok := task.read("pull_image")
				return ok && pull == "always"
			}},
			{"podman_image.image, registry_username, and registry_password must be known before the run, so not a secret: Podman sends the login to the registry that image names, which the playbook chooses", func(task explainedTask) bool {
				return task.knownBeforeRun("image", "registry_username", "registry_password")
			}},
		},
		examples: []string{`- podman_image:
    image: registry.example.com/shop/web:2.4
    pull_image: always`}},
	// reboot's two check commands are left out: they are programs, which an
	// approval of "Reboot a server" does not cover.
	{module: "reboot", kind: RebootAServer, moduleKind: "core",
		arguments: []string{"skip_if_this_path_exists", "run_only_if_this_path_exists", "wait_at_most"},
		examples: []string{`- reboot:
    wait_at_most: 10m`}},
	// Only blocks of whole sources: no port, so no service is opened or
	// closed for some, and no inventory group, which stands for the team's
	// own servers.
	{module: "ufw", kind: BlockANetworkRange, moduleKind: "core",
		arguments: []string{"state", "incoming_rules_not_listed", "incoming_rules"},
		conditions: []condition{
			{"ufw.state must be present or left out", func(task explainedTask) bool {
				state, ok := task.read("state")
				return !task.writes("state") || ok && state == "present"
			}},
			{"ufw.incoming_rules_not_listed must be keep", func(task explainedTask) bool {
				notListed, ok := task.read("incoming_rules_not_listed")
				return ok && notListed == "keep"
			}},
			{"ufw.incoming_rules must give each rule an action of deny or reject and a from value; only protocol and incoming_interface may also be set, so port and from_inventory_group are not allowed", blocksWholeSources},
		},
		examples: []string{`- ufw:
    incoming_rules_not_listed: keep
    incoming_rules:
      - action: deny
        protocol: tcp_and_udp
        from: 203.0.113.0/24`}},
	// trusted_certificate_authority_file and
	// dangerously_skip_server_certificate_check are left out, and the Host
	// header is refused, so the team's Cloudflare token goes only to
	// Cloudflare's DNS records API.
	{module: "http", kind: ChangeDNSRecords, moduleKind: "core",
		arguments: []string{"url", "method", "headers", "json_body", "body", "expected_http_status_codes", "changed_when"},
		conditions: []condition{
			{"http.url must be https://api.cloudflare.com/client/v4/zones/ZONE_ID/dns_records or a path below the zone's dns_records path, with no .. part and no escaped character", func(task explainedTask) bool {
				address, ok := task.read("url")
				text, isText := address.(string)
				return ok && isText && cloudflareDNSRecordsURL(text)
			}},
			{"http.headers must be known before the run and must not set Host: a Host header asks Cloudflare's servers for another site, which would receive the team's token", setsNoHostHeader},
		},
		examples: []string{`- http:
    url: https://api.cloudflare.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records/372e67954025e0ba6aaa6d586b9e0b59
    method: PATCH
    headers:
      Authorization: "Bearer {{ .secrets.cloudflare_api_token }}"
    json_body:
      content: 198.51.100.4`}},
	{module: "assert", kind: CheckOnly, moduleKind: "core",
		arguments: []string{"that", "message"},
		examples: []string{`- assert:
    that:
      equals: ["{{ .node.distribution.id }}", debian]
    message: This fix is written for Debian.`}},
	// read_file is not check only: MMX reads the file as root, and its content
	// would reach the agent, which must never read a secret such as an app's
	// env file; it is an unrecognized task, so a person decides.
	// wait_for's command_succeeds and its settings are left out: the command
	// is a program, which can change anything. url_responds and tcp_host name
	// the host the server connects to, and their text reaches that host and
	// its name servers, so they must hold no secret. MMX refuses a secret
	// there during the run too; MMHotline does not rely on that.
	{module: "wait_for", kind: CheckOnly, moduleKind: "core",
		arguments: []string{"tcp_port_accepts_connections", "tcp_port_refuses_connections", "tcp_host", "path_exists", "path_is_missing", "url_responds", "trusted_certificate_authority_file", "dangerously_skip_server_certificate_check", "docker_container_is_healthy", "docker_compose_project_is_ready", "podman_container_is_healthy", "podman_container_of_user_account", "expected_http_status_codes", "response_body_contains", "wait_at_most"},
		conditions: []condition{{"wait_for.url_responds and tcp_host must be known before the run, so not a secret: the server connects to the host they name, which the playbook chooses", func(task explainedTask) bool {
			return task.knownBeforeRun("url_responds", "tcp_host")
		}}},
		examples: []string{`- wait_for:
    url_responds: http://127.0.0.1:8080/
    wait_at_most: 60s`}},
	{module: "show", kind: CheckOnly,
		examples: []string{`- show: Caddy answers again.`}},
	{module: "set_vars", kind: CheckOnly,
		examples: []string{`- set_vars:
    restarted_service: caddy`}},
	// A role call and a node task block do nothing themselves; MMX lists
	// their node tasks after them, and each is judged by itself. An example
	// would need a role or a block of its own.
	{module: "role", kind: CheckOnly},
	{module: "task_block", kind: CheckOnly},
}

// patternOf finds the pattern of a module, or of role, task_block, show, or
// set_vars.
func patternOf(module string) (pattern, bool) {
	for _, rule := range patterns {
		if rule.module == module {
			return rule, true
		}
	}
	return pattern{}, false
}

// Recognize reads the output of mmx explain PLAYBOOK --inventory NAME --json.
// Each file a fleet stage saves on the controller machine is an unrecognized
// task: no kind covers writing a file on MMHotline's own server, such as a
// role in its systems repository copy, changing a later MMX run as the
// mmhotline user. Controller-file entries come first, so no bound leaves them out.
func Recognize(output []byte) (Recognition, error) {
	var explained explanation
	if err := json.Unmarshal(output, &explained); err != nil {
		return Recognition{}, fmt.Errorf("mmx explain printed JSON MMHotline cannot read: %w", err)
	}
	switch explained.SchemaVersion {
	case 2:
	case 1:
		return Recognition{}, errors.New("mmx explain printed schema_version 1, which does not list the files a playbook saves on MMHotline's server; upgrade MMX to 0.1.0 or later, whose mmx explain lists them")
	default:
		return Recognition{}, fmt.Errorf("mmx explain printed schema_version %d; MMHotline reads version 2", explained.SchemaVersion)
	}
	found := map[Kind]bool{}
	var recognition Recognition
	var nodeTasks []Unrecognized
	for _, stage := range explained.FleetStages {
		for _, saved := range stage.SaveOnControllerMachine {
			found[UnrecognizedTask] = true
			recognition.UnrecognizedTasks = append(recognition.UnrecognizedTasks, Unrecognized{Task{Path: "fleet stage " + stage.Path, Action: "save_on_controller_machine"}, saved.reason()})
		}
		for _, task := range stage.NodeTasks {
			kind, reason := task.kind()
			found[kind] = true
			listed := Task(task)
			recognition.Tasks = append(recognition.Tasks, listed)
			if kind == UnrecognizedTask {
				nodeTasks = append(nodeTasks, Unrecognized{listed, reason})
			}
		}
	}
	recognition.UnrecognizedTasks = append(recognition.UnrecognizedTasks, nodeTasks...)
	if len(found) == 0 {
		return Recognition{}, errors.New("mmx explain lists no node task and no file saved on MMHotline's server for the playbook")
	}
	for _, kind := range Kinds() {
		if found[kind] {
			recognition.Kinds = append(recognition.Kinds, kind)
		}
	}
	return recognition, nil
}

// maximumSavedFilesNamed keeps a reason readable in an alert: an output entry
// writes one file for each server, and the card's playbook text has them all.
const maximumSavedFilesNamed = 3

// reason names the files the entry writes on MMHotline's server, such as "the
// fleet stage writes /var/lib/mmhotline/systems_repository/.git/hooks/pre-commit on
// MMHotline's server, with permissions_mode 0755".
func (saved savedOnControllerMachine) reason() string {
	mode := ", with permissions_mode " + saved.PermissionsMode
	switch {
	case saved.FilesResolvedDuringRun:
		return "the fleet stage writes files on MMHotline's server whose names MMX knows only during the run, from the path " + saved.Path + mode
	case len(saved.Files) == 0:
		return "the fleet stage writes a file on MMHotline's server for each server the fleet stage selects (none now), from the path " + saved.Path + mode
	}
	var files []string
	for index, file := range saved.Files {
		if index == maximumSavedFilesNamed {
			files = append(files, fmt.Sprintf("%d more", len(saved.Files)-index))
			break
		}
		files = append(files, file.File)
	}
	switch len(files) {
	case 1:
		return "the fleet stage writes " + files[0] + " on MMHotline's server" + mode
	case 2:
		return "the fleet stage writes " + files[0] + " and " + files[1] + " on MMHotline's server" + mode
	}
	return "the fleet stage writes " + strings.Join(files[:len(files)-1], ", ") + ", and " + files[len(files)-1] + " on MMHotline's server" + mode //nolint:nilaway // files holds at least one name here, because the len(saved.Files) == 0 case returned above.
}

// Kind is the node task's kind of action, by the rule Recognize applies.
func (task Task) Kind() Kind {
	kind, _ := explainedTask(task).kind()
	return kind
}

// kind is the task's kind and, for an unrecognized task, why it is of no
// other kind, in plain words: the first rule it breaks.
func (task explainedTask) kind() (Kind, string) {
	rule, ok := patternOf(task.Action)
	if !ok {
		return UnrecognizedTask, "no kind covers the module " + task.Action
	}
	if task.ModuleKind != rule.moduleKind {
		return UnrecognizedTask, task.Action + " in this task is " + moduleKindText(task.ModuleKind) + "; only MMX's core module counts"
	}
	if rule.arguments != nil {
		// For a task that is not ready, MMX lists only the arguments that
		// can hold a reference, so an argument outside the pattern, such as
		// wait_for's command_succeeds, would not show.
		if task.Status != "ready" {
			return UnrecognizedTask, "MMX knows the task's arguments only during the playbook run (status " + task.Status + ")"
		}
		for _, argument := range task.Arguments {
			name, isModuleArgument := task.argumentName(argument.Field)
			switch {
			case isModuleArgument && !slices.Contains(rule.arguments, name):
				return UnrecognizedTask, task.Action + "." + name + " is not an allowed argument for " + rule.kind.Title() + " (" + strings.Join(rule.arguments, ", ") + ")"
			case !isModuleArgument && !slices.Contains(taskSettings, argument.Field):
				return UnrecognizedTask, "the task's setting " + argument.Field + " is not accepted by any kind of action; only " + strings.Join(taskSettings, " and ") + " go with a module's arguments"
			}
		}
	}
	for _, condition := range rule.conditions {
		if !condition.holds(task) {
			return UnrecognizedTask, condition.text
		}
	}
	return rule.kind, ""
}

// moduleKindText names a module kind of mmx explain for people.
func moduleKindText(moduleKind string) string {
	switch moduleKind {
	case "project_starlark", "project_executable":
		return "a project module"
	case "official_extra":
		return "an official extra module"
	case "":
		return "no module"
	}
	return "a module of kind " + moduleKind
}

// argumentName names the module argument a field belongs to, such as
// incoming_rules for ufw.incoming_rules[0].from.
func (task explainedTask) argumentName(field string) (string, bool) {
	rest, ok := strings.CutPrefix(field, task.Action+".")
	if !ok {
		return "", false
	}
	if end := strings.IndexAny(rest, ".["); end >= 0 {
		rest = rest[:end]
	}
	return rest, rest != ""
}

func (task explainedTask) writes(name string) bool {
	for _, argument := range task.Arguments {
		if argumentName, ok := task.argumentName(argument.Field); ok && argumentName == name {
			return true
		}
	}
	return false
}

// read returns a module argument's value only when MMX knew it, and every
// part of it, before the run: a pattern decides only on values that cannot
// change by the time the playbook runs.
func (task explainedTask) read(name string) (any, bool) {
	var value any
	found := false
	for _, argument := range task.Arguments {
		if argumentName, ok := task.argumentName(argument.Field); !ok || argumentName != name {
			continue
		}
		if argument.Status != "literal" && argument.Status != "resolved" {
			return nil, false
		}
		if argument.Field == task.Action+"."+name {
			value, found = argument.Value, true
		}
	}
	return value, found
}

// knownBeforeRun reports whether MMX knew each named module argument the task
// writes, and every part of it, before the run, as read requires; a secret,
// or a value known only during the run, which can hold a secret, is not
// known. An argument the task does not write counts as known.
func (task explainedTask) knownBeforeRun(names ...string) bool {
	for _, name := range names {
		if _, known := task.read(name); task.writes(name) && !known {
			return false
		}
	}
	return true
}

// setsNoHostHeader accepts http's headers when MMX knew them before the run
// and none is Host, in any case. MMX's http sends a Host header as the
// request's host, so Cloudflare's servers would be asked for another site.
// mmx explain lists headers written one by one as http.headers.NAME, and
// headers written as one reference as http.headers.
func setsNoHostHeader(task explainedTask) bool {
	for _, argument := range task.Arguments {
		if name, ok := task.argumentName(argument.Field); !ok || name != "headers" {
			continue
		}
		if header, isOne := strings.CutPrefix(argument.Field, task.Action+".headers."); isOne {
			if strings.EqualFold(header, "Host") {
				return false
			}
			continue
		}
		headers, known := task.read("headers")
		list, isMap := headers.(map[string]any)
		if !known || !isMap {
			return false
		}
		for header := range list {
			if strings.EqualFold(header, "Host") {
				return false
			}
		}
	}
	return true
}

// rotatedLogPatterns are the file names logrotate leaves behind: compressed,
// numbered, and ".old" copies, never the live log.
var rotatedLogPatterns = []string{"*.gz", "*.xz", "*.zst", "*.bz2", "*.[0-9]", "*.[0-9].gz", "*.old"}

// atLeastOneDay reads older_than as MMX does: a count of days such as 7d, or a
// Go duration such as 36h or 90m.
func atLeastOneDay(text string) bool {
	var duration time.Duration
	if days, isDays := strings.CutSuffix(text, "d"); isDays {
		count, err := strconv.ParseInt(days, 10, 64)
		if err != nil || count <= 0 || count > math.MaxInt64/int64(24*time.Hour) {
			return false
		}
		duration = time.Duration(count) * 24 * time.Hour
	} else {
		var err error
		if duration, err = time.ParseDuration(text); err != nil {
			return false
		}
	}
	return duration >= 24*time.Hour
}

// isVarLog accepts exactly /var/log. MMX's delete_old_files refuses a
// directory with a symlink in its path and never crosses into another
// filesystem below it, but it works inside a directory that is itself a mount
// point, such as a bind mount of other data at /var/log/x; only /var/log
// itself rules that out. Its log subdirectories are reached with
// include_subdirectories.
func isVarLog(directory string) bool {
	return directory == "/var/log" || directory == "/var/log/"
}

// blocksWholeSources accepts ufw's incoming_rules when each rule denies or
// rejects a whole source.
func blocksWholeSources(task explainedTask) bool {
	rules, ok := task.read("incoming_rules")
	list, isList := rules.([]any)
	if !ok || !isList {
		return false
	}
	for _, item := range list {
		rule, isRule := item.(map[string]any)
		if !isRule || (rule["action"] != "deny" && rule["action"] != "reject") {
			return false
		}
		if from, isText := rule["from"].(string); !isText || from == "" {
			return false
		}
		for key := range rule {
			if key != "action" && key != "from" && key != "protocol" && key != "incoming_interface" {
				return false
			}
		}
	}
	return true
}

// cloudflareDNSRecordsURL accepts Cloudflare's DNS records API of one zone,
// https://api.cloudflare.com/client/v4/zones/ZONE_ID/dns_records and the
// paths under it. A ".." part or an escaped character in the path is
// refused: either could lead Cloudflare to another API, such as an account's
// members, with the same token.
func cloudflareDNSRecordsURL(address string) bool {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.cloudflare.com" || parsed.User != nil || parsed.EscapedPath() != parsed.Path {
		return false
	}
	parts := strings.Split(parsed.Path, "/")
	if len(parts) < 6 || parts[0] != "" || parts[1] != "client" || parts[2] != "v4" || parts[3] != "zones" || parts[5] != "dns_records" {
		return false
	}
	for _, part := range parts[4:] {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
