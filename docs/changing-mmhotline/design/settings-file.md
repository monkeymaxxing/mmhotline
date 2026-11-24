# Settings file

A team's settings live in one file on the MMHotline server,
`/etc/mmhotline/mmhotline_settings.yml`: who is on the team, who is on call,
the alert sources, the rules for running the agent's playbooks, the triage agent and the review agent,
the sending services, the MMHotline-down alarm, the status page, and the server's
addresses.

A team that runs the Monkeymaxxing stack describes the settings file in
the team's systems repository. The team's deploy tool renders the settings
file on the server with the secrets from a vault. A team without a deploy tool edits
`/etc/mmhotline/mmhotline_settings.yml` with `sudoedit` and runs
`sudo mmhotline settings apply`. Nobody changes values from
`/etc/mmhotline/mmhotline_settings.yml` on the pages or with one-off
commands.

What people do themselves stays on the pages: their contact methods, their
unavailability, taking on-call, team mute, claims of incidents, approvals,
status page posts, and SOS incidents they create. A claim tells the team that
a person handles an incident, and MMHotline stops contacting the next
person; an SOS is the urgent alert that wakes the person on call, even at
night.

The [settings file reference](../../reference/settings-file.md) lists every key and
shows a whole file; the [command reference](../../reference/mmhotline-command.md#mmhotline-settings-apply)
describes `settings check`, `apply`, and `show`. The settings-file design
explains how the file applies and why.

On settings pages, one closed section named for the page's subject holds the file's
path, first comment line, and editing instructions. Setup status and next
actions stay above the section, because a long path does not help routine checks.
Change records say **the settings file** without repeating the path.

## Why one file

- **The systems repository is the record.** A change of who is on call or
  which alert source exists is a commit someone reviewed. A page edit would
  leave no trace outside MMHotline's database.
- **Secrets come from the vault.** The monitoring tool on every server signs
  its alerts with the alert source's signing secret, so the secret belongs in
  the vault read by the monitoring tool and MMHotline. A secret MMHotline
  made and showed once could not belong to the shared vault.
- **A new server is the file plus a restore.** Rebuilding MMHotline needs no
  list of commands someone once ran.

## The file

The file is YAML, one mapping. Each key is checked by its path, such as
`triage_agent.reasoning_effort`. An unknown key is refused, so a typo never
passes silently. Leaving out an optional section turns off the section's
feature. The [settings file reference](../../reference/settings-file.md) marks optional
sections and keys. Every key inside a present section is required unless
the reference marks the key optional. Required keys have no default values,
so an agent or a person writing the settings file must supply a value.
Omitted action kinds receive the explicitly reported
[safest rule](#rules-for-running-the-agents-playbooks).

The settings file's first line, when the line is a comment, is shown on the
pages and by `settings show` next to the path, so a person who opens a page learns how the
file is changed, such as
`# Managed by MMX from mmhotline_settings.yml in the systems repository`.

Secrets are inline. The file is at most 1 MiB, which a team with custom
schedule code, at most 64 KiB, and comments never reaches.

## Owner and mode

MMHotline refuses the file at start, on `settings apply`, and at `install`
when the settings file is:

- not a regular file: a symbolic link could point at a file whose owner and
  mode were never checked;
- not owned by root;
- writable by anyone but root;
- readable by others.

The file decides what MMHotline does, and its `fleet_view_command` runs as
root, so only root may change it. MMHotline runs as the Unix user `mmhotline`
and must read the file itself, so the file is `root:mmhotline` with mode
`0640` (CA1). The group adds no new reader of the secrets: `mmhotline.db`,
which the Unix user `mmhotline` owns, already holds the secrets. The triage
agent's user is not in the group, and its sandbox hides `/etc/mmhotline`.

The refusal explains the problem and the reason for refusing the settings
file. For ownership or permission problems, the refusal gives the repair
command: `sudo chown root:mmhotline FILE; sudo chmod 0640 FILE`.

Install creates the `mmhotline` group, so a file written before the first
install is `root:root`. Install gives the settings file group `mmhotline` and mode
`0640`, which install can do safely because only root wrote the settings
file.

On a development machine outside Linux, running MMHotline only for testing,
the settings file owner may also be the developer's Unix user. MMHotline
allows the current Unix user for file ownership and command-socket access
([people and access](people.md)). On Linux, including development machines,
the settings file owner must be root.

## The static check

`mmhotline settings check --settings-file FILE` checks the shape, the keys,
and every value without the network, without a model call, and without root.
`settings check` prints each problem with the problem's key path and exits 1
when any problem exists. Each value is checked with the rule the operation
that applies the value uses, such as
the sign-in email address rule of people or the schedules' own rules, so
one rule lives in one place.

The check skips the owner and mode (CA2): a deploy tool's dry run renders
the file on a laptop, where it cannot belong to `root:mmhotline`. `apply`,
start, and `install` check the settings file's owner and permissions.

Besides problems, the check prints notes: something the file may leave out,
which MMHotline fills with its safest behavior, but which a person should
see. The only note is a kind of action that an inventory's
`action_kinds` leaves out ([rules for running the agent's playbooks](#rules-for-running-the-agents-playbooks)). A note does
not fail the check, which exits 0 with only notes, so a deploy tool's dry
run shows the note without stopping.

A problem never shows a secret's value, only its key path and what is wrong,
such as `sending_services.pushover.application_token: must be one line of
exactly 30 letters and digits`. A YAML syntax error names its line only. The
outputs of `settings show`, the pages, the logs, and startup warnings also
omit secret values. Problems use only key paths and explanations, and YAML
syntax errors use only line numbers. A test puts a recognizable value in
every secret key and looks for the value in each output.

### Model and reasoning effort

A coding agent's `model` and `reasoning_effort` are checked only for their
shape: one model name of at most 200 characters that starts with a letter or
digit and has no spaces, and one lowercase word (CA6). The coding agent
vendors define the accepted model names and reasoning efforts and change
the accepted values every few weeks. A list in MMHotline would refuse a new
model or accept a retired model.

When the setup applies, the agent's test prompt proves the configured model
and reasoning effort. `model` and `reasoning_effort` are
required. MMHotline names no model, so MMHotline has no default to fall back
on.

The triage agent's triage runs and software reviews use the triage agent's
model and reasoning effort. The review agent's playbook reviews use the
review agent's model and reasoning effort. Each agent's setup test and sign-in
check also use the configured model and reasoning effort. The sandbox
passes the model and reasoning effort to the coding agent as separate
arguments, never through a shell. The sandbox refuses requests with a
missing or malformed model or reasoning effort. The model and reasoning
effort therefore cannot become another flag
([the connection to each coding agent](triage-agent.md#the-connection-to-each-coding-agent)).

### Credential types

Claude Code takes `api_key`, an Anthropic API key, or `subscription_token`,
the token `claude setup-token` prints. Codex takes `api_key`, an OpenAI API
key; `access_token`, a Codex access token from a ChatGPT Business or
Enterprise workspace; or `chatgpt_sign_in`, the whole content of `auth.json`
after `codex login`, made on the person's own computer in a new empty folder.
The names follow Codex's own words, "Codex access token" and "Sign in with
ChatGPT", so a reader can find both in OpenAI's docs. An API key is the
recommended choice for both coding agents: it never expires and has no plan
usage limit ([coding agents](triage-agent.md#triage-agent-setup)).
A credential carried in the settings file needs no interactive sign-in step
on the server.

The static check applies these rules:

- A credential that goes into an environment variable, every type but
  `chatgpt_sign_in`, is one line without control characters, at most 4,096
  bytes.
- `chatgpt_sign_in` is one JSON object of at most 64 KiB, with
  `tokens.id_token`, `tokens.access_token`, `tokens.refresh_token`,
  `tokens.account_id`, and `last_refresh`, and an `auth_mode` of `chatgpt`
  or none. It may span several lines, as Codex writes it: write it in single
  quotes on one line, or as a `credential: |` block with the file's lines
  indented. With a template, `{{ .secrets.NAME | toJson }}` quotes any
  credential safely.
- `credential_expires_on`, a date such as `2027-02-06`, is required with
  `access_token` and refused with the other types. MMHotline cannot read the
  end date from the token, so the person copies it from ChatGPT's dialog.
- `review_agent` may not use the triage agent's ChatGPT sign-in, the same
  refresh token or the same `login_owner_id`: "The review agent needs its own
  ChatGPT sign-in. Sign in again in a new empty folder and use that
  auth.json." A review runs while its triage run is active, and two copies of
  one sign-in end each other. Two sign-ins of one account are fine.

## Parts

The file applies in parts. Each part is checked, tested when it has an
outside test, and written on its own, so one part that fails keeps its
earlier setting while the others apply. The [`rules_for_running_agent_playbooks` part](#rules-for-running-the-agents-playbooks)
has an exception for omitted inventories. The output names each part by its
key paths, in this order:

| Part | What applying the part does |
| --- | --- |
| `web_url, web_listen_address, alert_intake_listen_address, cloudflare_access` | Nothing at once: the `mmhotline serve` subcommand takes the four server settings at start ([restarts](#restarts)) |
| `team_name` | Saves the team's name |
| `fleet_view_command` | Tells the dashboard there is a fleet view; the root fleet view unit reads the command from the settings file |
| `people` | Adds, changes, and deactivates people ([people](#people)) |
| `on_call_schedule` | Saves and activates the schedule and its time to claim ([schedule](#the-schedule)) |
| `rules_for_running_agent_playbooks` | Saves each inventory's rules ([rules for running the agent's playbooks](#rules-for-running-the-agents-playbooks)) |
| `services_whose_changes_a_person_must_approve` | Saves the list |
| `alert_sources` | Saves, enables, and disables alert sources ([alert sources](#alert-sources)) |
| `systems_repository_commits.trusted_commits` | Keeps the value as an applied value, which the commit check reads; without the section the value is null and nothing is trusted |
| `triage_agent.commit_signing_public_key, systems_repository_commits.trusted_commit_signing_public_keys` | Saves the public SSH keys the commit check uses before every MMX run and triage agent run; a changed key leaves the checked-through commit as it is. Without the `systems_repository_commits` section no key is saved; without the `triage_agent` section no triage agent key is saved |
| `systems_repository_commits.commits_without_a_trusted_signature_are_approved_by, systems_repository_commits.paths_only_a_person_may_approve` | Keeps the two settings of the triage agent's approval as applied values, which the commit review reads; no outside test. Its note says what the setting means. The background loop's next visit sends MMHotline's warning for each waiting commit the agent may no longer review ([the triage agent reviews a commit](triage-agent.md#the-triage-agent-reviews-a-commit-no-trusted-key-signed)) |
| `triage_agent.coding_agent, triage_agent.credential_type, triage_agent.credential, triage_agent.model, triage_agent.reasoning_effort` | Saves the triage agent's setup after its outside test ([the agents](#the-triage-agent-and-the-review-agent)) |
| `triage_agent.internet_access`, `triage_agent.listed_hosts`, `triage_agent.sos_waits_for_the_agent_minutes`, `triage_agent.warn_waits_for_the_agent_minutes`, `triage_agent.send_a_message_when_the_agent_starts_looking`, `triage_agent.sos_waits_for_a_server_replacement_minutes`, `triage_agent.software_review_interval_hours` | Save the triage agent's own settings, when its section is present. An SOS reads the applied `sos_waits_for_the_agent_minutes` as it opens its incident, and a warn reads the applied `warn_waits_for_the_agent_minutes` as it arrives (0 to 30; the key may be left out, which is 15; [a warn waits for the agent](triage-agent.md#a-warn-waits-for-the-agent)), a new triage session reads `send_a_message_when_the_agent_starts_looking` as it starts (`true` or `false`; the key may be left out, which is `false`; [the start message](triage-agent.md#the-start-message)), and a server replacement reads `sos_waits_for_a_server_replacement_minutes`, 0 to 60, as it starts ([an SOS waits for a server replacement](triage-agent.md#an-sos-waits-for-a-server-replacement)). `software_review_interval_hours` is `off` or 1 to 8,760 hours; its part carries a note, "Scheduled software review: off. ..." or the interval ([the software review interval](triage-agent.md#the-software-review-interval)) |
| `mmhotline_down_alarm.wake_the_person_on_call_after_minutes_without_a_check_in, mmhotline_down_alarm.wake_the_backup_after_minutes_without_a_check_in` | Saves the MMHotline-down alarm's wake times, which reach the Worker at the next check-in, within seconds. They are their own part, so changing them never sets up the MMHotline-down alarm again |
| `review_agent` | Saves the review agent's setup after its outside test |
| `sending_services.pushover`, `sending_services.twilio`, `sending_services.telegram`, `sending_services.slack`, `sending_services.mail_server` | Each saves one sending service's setup after its outside test ([sending services](#sending-services)) |
| `mmhotline_down_alarm` | Sets up the MMHotline-down alarm in Cloudflare ([the MMHotline-down alarm](#the-mmhotline-down-alarm)) |
| `status_page` | Saves the status page's setup after MMHotline posts the current page: `status.json`, `index.html`, and `feed.xml` ([the status page](#the-status-page)) |

People apply before the schedule that names the people. Each part writes the
tables the pages and `show` commands read, through each feature's own
operations. Change records with author attribution identify the settings
file as the author: "changed by the settings file
/etc/mmhotline/mmhotline_settings.yml".

Each `settingsFileParts` entry in `internal/app/settings_file_parts.go`
provides the part's value for change detection and an apply function.
The apply function checks the proposed settings against MMHotline's state,
runs the part's outside test, and saves the settings through the feature's
operations.

### Parts with an outside test

A service outside MMHotline proves the following parts before the parts
are saved:

| Part | Outside test | Left out of the file |
| --- | --- | --- |
| The triage agent's setup | The setup test: one test prompt, run with the file's coding agent, credential, model, and reasoning effort | Removes the triage agent's setup |
| `review_agent` | One test prompt using the coding agent, credential, model, and reasoning effort from `review_agent` | Removes the review agent's setup |
| `sending_services.pushover` | Pushover's limits endpoint | Removes the Pushover setup; refused while contact methods use the Pushover setup |
| `sending_services.twilio` | Twilio's check of the calling number in the account | Removes the Twilio setup; refused while contact methods use the Twilio setup |
| `sending_services.telegram` | Telegram's `getMe`; MMHotline then starts polling | Removes the Telegram setup; refused while contact methods use the Telegram setup |
| `sending_services.slack` | Slack's token checks; MMHotline then starts Socket Mode | Removes the Slack setup; refused while contact methods use the Slack setup |
| `sending_services.mail_server` | Connects to the SMTP server and checks TLS and the sign-in; sends nothing | Removes the mail server setup; refused while contact methods use the mail server setup |
| `mmhotline_down_alarm` | Uploads the MMHotline-down alarm's program, the Worker, to the team's Cloudflare account, sets the Worker's check-in token, turns on the Worker's workers.dev address, saves, and checks in | Checks in with an empty list of messages to send if check-ins stop, then removes the setup. Refused when the Worker cannot be told |
| `status_page` | Posts the current page (`status.json`, `index.html`, and `feed.xml`) to Cloudflare's R2 object storage with `status_page.r2_access_key_id` and `status_page.r2_secret_access_key` | Removes the setup; the bucket and the page stay in Cloudflare |

A part that changed is saved only when its outside test passes. Otherwise
its earlier setting stays, and `settings apply` exits 1 and names the part
with its reason. An unchanged part with no failure since the last successful
apply is not tested again ([change detection](#change-detection)). An apply
that changes one key runs no outside test for unchanged, previously
successful parts. At start, parts with an outside test apply in the
background. MMHotline continues using each part's earlier setting until the
new setting passes the outside test ([start](#start)).

No output, page, log, or run step prints a credential, token, key, or
password, regardless of the part containing the secret. An outside service's answer could
quote a value back. Before displaying or storing a part's failure reason,
MMHotline removes every secret value from the settings file from the reason.
Before recording coding-agent output, MMHotline's run reader removes the
credential used for the run from the coding agent's output.

### A part the file leaves out

Leaving a part's section out of the file, such as `status_page` or a
sending service, applies as "not set up", and MMHotline records that as
the part's setting. `settings apply` and `settings show` then say
`not set up, because the settings file leaves it out.` in place of an
applied time, and a later first setup that fails says
`It stays not set up, as before.` in place of "The earlier setting,
applied at ..., stays." The JSON marks such a part with
`left_out_of_the_settings_file`. Reason: `sending-services list` says
such a service is "not set up", and two commands must not disagree about
the same service in a team's first hour.

### Notes on a part's outcome

A part's outcome can carry notes: lines a person should read, which never
make the part fail. `settings apply` prints the outcome's note lines under
the part, and `settings show` keeps the notes of the last apply. There are
three kinds:

- **The file's notes** from the static check, each under the part its key
  path belongs to, such as the [rules for running the agent's playbooks](#rules-for-running-the-agents-playbooks) note for
  a kind of action the file leaves out. The static-check notes accompany
  the part's outcome also when the part is unchanged, so a note never
  disappears just because nothing changed.
- **A next step,** when a sending service's section successfully configures
  the service for the first time, or again after the section was left out.
  For example:
  "Next step: Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification."
- **What MMHotline lists by itself,** which `settings show` reads from
  MMHotline's state each time, never from the file: under
  `triage_agent.listed_hosts`, the hosts that the latest triage agent run
  listed from the [vendor pages](triage-agent.md#the-vendor-pages-hosts) in
  `software_outside_the_distribution.yml`. For example, `settings show` prints "Also listed
  automatically from software_outside_the_distribution.yml, as of the latest
  run: github.com port 443".

  The note from `settings show` explains why the agent can reach vendor-page
  hosts absent from the settings file's `listed_hosts`.
  The note about automatically allowed
  hosts is not part of the part's value, so the note never changes the
  fingerprint and never
  makes the part apply again ([change detection](#change-detection)).

### Change detection

For each part, MMHotline keeps the SHA-256 of the part's value as JSON, the
apply time, and the latest attempt's failure reason in `settings_file_parts`.
A part whose value matches the value in effect, with no failure since, is
not tested or written again. An apply that changed one key therefore does
not run every outside test. A part that failed is tried again at each apply
because the reason for refusal, such as a claimed incident, may have changed.

The fingerprint is only compared, never shown, because a part's value can
hold a secret. Two parts include additional values in the fingerprint:

- the rules for running the agent's playbooks take the inventories of MMHotline's copy of the systems
  repository, so a new inventory there applies the part again;
- the schedule includes every active member's ID, because schedules save
  people by ID: a person deactivated and listed again is a new member.

### Applying

`sudo mmhotline settings apply` asks the running MMHotline, through its
command socket, to read the settings file at the path given to
`mmhotline serve`. MMHotline checks the settings values and the file's owner
and permissions, then applies each part. Reading the settings file in the
service means the service applies exactly what the service would read at
the next start.

An unreadable or nonregular settings file, or an ownership, permission, or
static-check error, prevents every part from applying. An outside-test
failure affects only the tested part, as described
[above](#parts-with-an-outside-test). The command exits 0
only when every part applied, and otherwise 1, naming each failed part with
its reason.

One apply runs at a time; a second apply waits for the first apply. The command waits
without a time limit, because a part's outside test, such as an agent's test
prompt, can take minutes.

## Restarts

`web_url`, `web_listen_address`, `alert_intake_listen_address`, and
`cloudflare_access` change only when MMHotline restarts (CA5): they decide the
listeners and the sign-in, which serve sets up before answering anything.
When `web_url`, `web_listen_address`, `alert_intake_listen_address`, or
`cloudflare_access` changes, MMHotline answers `settings apply` that a
restart is needed and applies nothing. The command then runs
`systemctl restart mmhotline.service`, waits up to a minute for the command
socket to answer again, and sends the `settings apply` request again.
The command restarts MMHotline automatically so a team without a deploy
tool cannot forget the restart. A deploy tool needs no separate restart
step.

A restart drops no alert. The monitoring tool keeps its alerts in a durable
outbox and sends each again, with the same event ID, after 1 second.
The monitoring tool doubles the delay between retries up to 256 seconds.
MMHotline answers the repeat as a duplicate. Other senders follow the
[ingestion design's retry rule](ingestion.md).

## Start

At start serve reads the file at `--settings-file` and checks its owner and
mode and its values (CA7):

- **The file passes:** `mmhotline serve` opens the listeners and sets up
  sign-in using `web_url`, `web_listen_address`,
  `alert_intake_listen_address`, and `cloudflare_access`.
  Then `serve` applies the parts without an outside test and starts applying
  parts with outside tests in the background. After the synchronous parts
  apply, `serve` tells systemd that MMHotline is ready.
  Each part with an outside test keeps using the earlier setting until the
  new setting passes the test.
- **The file has a problem:** serve starts with the settings it last
  applied, and sends the administrators a warn alert.
  The warning names problems by key path only, so a typo never stops alerts.
  The parts are not applied.
- **The file has problems and no settings have applied yet,** on the very
  first start: serve refuses to start and prints the problems.

The server part contains `web_url`, `web_listen_address`,
`alert_intake_listen_address`, and `cloudflare_access`. The server part and
the fleet view command keep the last applied values, which hold no secret,
in `settings_file_parts`. The saved values let startup use earlier settings
when MMHotline cannot read or validate the settings file.
Every other part's last applied state is in the feature's tables already.

If `serve` cannot bind a listener to a configured address, such as an
address absent from the server, the failure would also stop alerts.
Then `serve` listens with the last applied server settings, records why the
server part failed, and warns the administrators. A later `settings apply`
reports that MMHotline still runs with the earlier values.

## People

`people` lists everyone who uses MMHotline, matched by sign-in email address,
with at least one administrator. One transaction adds each new person,
changes names and roles, and deactivates each person left out, whose history
stays. MMHotline refuses the whole people update if a person to deactivate
owns a claimed incident or has taken on-call. A claimed incident reminds
only the owner. A refusal keeps the earlier people list, names, roles, and
active status unchanged.

A saved schedule naming a person does not prevent the person's
deactivation. The static check confirms that the settings file's schedule
names only people listed in the settings file. The schedule part applies
next and makes the settings file's schedule the only saved schedule.
Refusing deactivation because of the saved schedule would prevent
replacing a rotation member in one edit. The people update would wait for
the schedule to stop naming the departing person. The schedule update
would wait for the people update to add the replacement.

If the schedule part then fails, the old schedule names a deactivated
person. MMHotline contacts the backup, then everyone available, in the
deactivated person's place, as for anyone unavailable.

## The schedule

`on_call_schedule` holds the schedule commands' keys, with only the keys
used by the chosen schedule type. `escalate_if_unclaimed_after_minutes` sets how long
each escalation step waits for someone to claim an SOS.

The settings file's schedule becomes the saved and active configuration of
the chosen type. MMHotline uses the schedules' own validation, including
custom schedule code's run and test cases ([scheduling](scheduling.md)).

The saved configurations of the other types are removed: only the file can
make a schedule active now, so inactive saved configurations would only
keep naming people who may have left. Custom schedule code keeps its last 20 saved versions.

`mmhotline schedules preview --settings-file FILE` previews the file's
schedule before the settings file applies, for the people already on the team.

## Rules for running the agent's playbooks

Each inventory listed in `rules_for_running_agent_playbooks` gets a preset or a rule
for every kind of action.

No inventory goes without rules, and none gets
a silent default (confirmed change 1): an inventory of MMHotline's copy of
the systems repository that the file leaves out fails the part with
"inventory staging has no rules for running the agent's playbooks. Every fix in the
inventory needs a person until you add its rules", and its saved rules become the
safest, every kind at
Get permission, then act with 1 approval.

Omitted inventories are an exception to the normal failed-part rule.
MMHotline saves the listed inventories' rules and the omitted
inventories' safest rules. The `rules_for_running_agent_playbooks` part reports failure for
the omitted inventories. Before MMHotline has a copy of the systems
repository, only the listed inventories apply. Saved rules of an
inventory in neither the file nor the copy are removed. A preset covers
every kind of action.

An inventory's `action_kinds` may leave a kind out. The omitted action kind
gets Get permission, then act with 1 approval. The omitted action kind is
named everywhere, never silently defaulted.
`settings check` prints the missing action-kind rule as a note, and
`settings apply` and `settings show` print the missing action-kind note under
the `rules_for_running_agent_playbooks` part, which still applies. The page of
rules for running the agent's playbooks and `agent-playbook-rules show` also
identify the missing action-kind rule:

```text
rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one
```

The page and `agent-playbook-rules show` say "check_only has no rule; it needs a
person until you add one" for each kind with no saved rule.

The reason: a later MMHotline may add a new kind of action, and an existing
settings file must not stop applying because of the new kind. The new
action kind receives Get permission, then act with 1 approval. The missing
action-kind rule is shown, so a person can configure a different rule.
An inventory with no entry at all still fails the part, as above.

An inventory may also list ordinary playbooks by name under `playbooks`,
each with its own rule, which the playbook takes instead of the rules of its
kinds ([playbooks listed by name](triage-agent.md#playbooks-listed-by-name)).
`settings check` names the key path of each problem, such as
`rules_for_running_agent_playbooks.servers.playbooks.2026-11-03_fix` for a
one-time playbook's name, which can never be listed. Applying the part makes
the saved listed playbooks equal the file's, recorded with the settings file
and the time, like the kinds.

A listed playbook's optional `its_approval_also_approves_in_the_same_session`
names other playbooks listed on the same inventory that a person's approval
of it also approves once later in the same triage session
([one approval for a known sequence](triage-agent.md#one-approval-for-a-known-sequence)).
`settings check` refuses the key with a rule other than
`get_permission_then_act`, because only an approval can cover later runs,
and refuses each named playbook that is not listed on the inventory, names
the playbook itself, or repeats, naming the key path, such as
`rules_for_running_agent_playbooks.servers.playbooks.replace_failed_servers.its_approval_also_approves_in_the_same_session[1]`.

## Alert sources

`alert_sources` lists each alert source with the team's own ID, such as
`monitoring`, a name, one or two signing secrets of 64 hexadecimal
characters, and durations without a check-in before a warning or an SOS
(CA3). The durations are `warn_after_no_check_in_for_minutes` and
`sos_after_no_check_in_for_minutes`
([check-in settings](../../reference/settings-file.md#alert_sources)). The senders' URLs carry
the ID:
`/alert-intake/monitoring` and `/check-in/monitoring`. An ID has 1 to 64
letters, digits, `_`, and `-`, which takes in the 32 hexadecimal characters
of an ID MMHotline made; `system`, `manual`, and `mmhotline_test_sos` name
MMHotline's own alerts and are refused. Two secrets serve while the monitoring tool moves to a new
one.

A source left out is disabled: its secrets stop working at once, and its
alerts and check-ins stay. Listing the disabled alert source again enables
the source with the listed secrets.

## The triage agent and the review agent

Each agent's setup is five keys: `coding_agent`, `credential_type`,
`credential`, `model`, and `reasoning_effort`. A Codex access token adds a
sixth, `credential_expires_on`
([credential types](#credential-types),
[model and reasoning effort](#model-and-reasoning-effort)). For the triage
agent, `coding_agent`, `credential_type`, `credential`,
`credential_expires_on`, `model`, and `reasoning_effort` form one part. The triage agent's internet access, listed
hosts, software review interval, and commit signing public key are four
more. `review_agent` is one part.
`listed_hosts` holds only the hosts the agent needs besides those MMHotline
lists by itself: the coding agent's service, OSV.dev's API, CISA's list of known exploited vulnerabilities on
`raw.githubusercontent.com`, the inventories'
servers, and the hosts of the vendor pages in
`software_outside_the_distribution.yml`
([internet access](triage-agent.md#internet-access)). `listed_hosts` is usually
`[]`.

The optional section `systems_repository_commits` decides which commits of
the systems repository MMX and the triage agent may run, with or without
the triage agent. Left out, nothing is trusted, as during a lock-out. Its
`trusted_commits` is required inside the section:
`signed_by_a_trusted_key_or_reviewed` or `every_commit_on_origin`
([the commit check](triage-agent.md#checking-commits-before-mmx-and-triage-agent-runs)).
The keys `trusted_commits`, `trusted_commit_signing_public_keys`,
`commits_without_a_trusted_signature_are_approved_by`, and
`paths_only_a_person_may_approve` moved there from `triage_agent`; under
`triage_agent`, each is refused with `moved to
systems_repository_commits.KEY`. Reason: a team without the optional
triage agent still needs trusted commits for MMX's commit check.

With `signed_by_a_trusted_key_or_reviewed`,
`systems_repository_commits.trusted_commit_signing_public_keys` is
required, and so is `triage_agent.commit_signing_public_key` when
`triage_agent` is present. The agent's key stays in `triage_agent`: it is
the agent's own, and MMHotline knows the agent's commits by it. Without
`triage_agent` no agent key is needed, since there are no agent commits.
With `every_commit_on_origin`, these keys are refused. Without
`systems_repository_commits`, the agent's key is allowed and unused, so a
lock-out removes one section only. The agent's key is one OpenSSH public
key line, such as the public `.pub` file of the SSH key that signs the
triage agent's commits. The trusted keys are a list of such lines, the
keys of the team's people and machines, such as each person's desk and
util01. As in OpenSSH, everything
after the key is the key's comment, which is optional and may hold any text.
The static check requires a known key type, base64 that decodes to a whole
key, and a matching type string inside the decoded key; its message names
the key path, such as `systems_repository_commits.trusted_commit_signing_public_keys[1]`,
and the part that fails. The triage agent's key in the trusted list is a
problem, because a trusted key's commits pass whatever they change.
Before MMX runs and triage agent runs, MMHotline passes commits a trusted
key signed, passes the triage agent's commits within its folders, and has
every other commit wait for a review
([the commit check](triage-agent.md#checking-commits-before-mmx-and-triage-agent-runs)).
The keys apply together, as one part, without an outside test. MMHotline
uses only the applied keys, never a file in the systems repository. A team
that renders the settings file from a template fills in the keys there.
`settings show` lists the keys' settings part and whether the keys applied;
`settings show` prints no key value.

`systems_repository_commits.commits_without_a_trusted_signature_are_approved_by`
and `systems_repository_commits.paths_only_a_person_may_approve` are
required with `signed_by_a_trusted_key_or_reviewed`. The first is
`a_person` or `a_person_or_the_triage_agent`, which needs the
`triage_agent` section; the second is a list of paths in the systems
repository, as Git names them, an entry ending in `/` a directory, and may
be empty. The static check names the key path of each problem, such as
`systems_repository_commits.paths_only_a_person_may_approve[2]`: another value, an empty
path, a path that starts with `/`, a path with an empty, `.`, or `..` part,
and a path listed twice. With `a_person_or_the_triage_agent`, the triage
agent reviews each commit no trusted key signed that changes no listed path
([the triage agent reviews a commit](triage-agent.md#the-triage-agent-reviews-a-commit-no-trusted-key-signed)).

The outside test is the agent's setup test
([triage agent setup](triage-agent.md#triage-agent-setup)): one test prompt
in the sandbox, run with the file's values. The setup test occupies the
single execution slot shared with triage-agent runs.
When another triage agent run is active, the part fails with "a triage agent run was active, so the new agent setup was not tested. Apply the settings file again after the triage agent run ends", and the earlier setting stays.
The active triage run's session takes priority over the setup test.
Rejecting the setup attempt makes no model call, so the rejected attempt
incurs no model charge.

A section left out removes that agent's setup: without `triage_agent`, no
triage agent runs; without `review_agent`, playbooks are not reviewed.

A Claude Code subscription token lasts one year. MMHotline counts the year
from when the file's credential applied: a change of only `model` or
`reasoning_effort` keeps the date, because the token is the same, and a new
credential starts a new year, with new reminders 30 and 7 days before the
new year ends ([expiry reminders](triage-agent.md#expiry-reminders)).
To renew a token or replace an API key, a person makes the new token or key.
The person puts the new token or key in the file's `credential` (with the
stack, in the vault), and runs `sudo mmhotline settings apply`.
The apply tests the new token or key before saving the credential.
A Codex access token's reminders count to its `credential_expires_on`; a new
date starts them again, as a new token does.

A ChatGPT sign-in changes on the server as Codex refreshes it, so the
setup keeps two values: the file's `credential`, which tells whether the
file changed, and MMHotline's newest copy, which each run gets. Applying the
same file again changes nothing, and MMHotline keeps its newer copy, as
OpenAI says: never overwrite a refreshed copy with the original. New
content is a new sign-in, which the setup test checks. A change of only
`model` or `reasoning_effort` runs the setup test with the newest copy
([Codex with a ChatGPT sign-in](triage-agent.md#codex-with-a-chatgpt-sign-in)).

## Sending services

`sending_services` is required and may be `{}`. Each service in
`sending_services` is optional and forms a separate part. A service that
fails the service's check does not hold back the remaining services.
The outside test is the service's own check
([sending service setup](notifications.md#sending-service-setup)). The
refusals that protect delivery become the part's failure reason, and the
earlier setup stays:

- a section left out while contact methods use the service, such as
  "cannot remove the Pushover setup: 1 Pushover notification contact method
  uses the sending service setup. Remove those contact methods on the Contact methods page, or with
  mmhotline contact-methods remove --contact-method-id ID, first";
- a different Telegram bot or Slack workspace while contact methods use it,
  because their chats belong to the bot or workspace they were connected
  through;
- a new Pushover application token while a Pushover emergency message is
  open, because MMHotline needs the old token to read and cancel the emergency message.

When a service is newly set up, its next step, which connects a contact
method, is a [note](#notes-on-a-parts-outcome) under the part.

## The MMHotline-down alarm

`mmhotline_down_alarm` holds the team's Cloudflare account ID and an account API token
with one permission, Account › Workers Scripts › Edit. MMHotline keeps the
token so an upgrade can upload the new Worker.

The outside test is the MMHotline-down alarm's set-up
([its set-up](mmhotline-down-alarm-and-status-page.md#set-up-1)): MMHotline uploads the
Worker, sets its check-in token, turns on its workers.dev address, saves the
setup, and checks in, trying the first check-in up to 30 times, a second
apart. A first check-in that still fails fails the part, and MMHotline
restores the previous setup in MMHotline's database. For a first set-up,
MMHotline removes the new database setup. The earlier local
setting stays, as for every part with an outside test.

A `mmhotline_down_alarm` section left out makes MMHotline check in with nothing held, so
the Worker wakes nobody, and then remove the setup. Without that check-in,
the Worker would wake the person on call once the wait set by
`wake_the_person_on_call_after_minutes_without_a_check_in` ran out after the
check-ins stopped. When MMHotline cannot clear the Worker's held messages with a
check-in request, the part fails and the setup stays. Apply again, or
delete the Worker in the Cloudflare dashboard and
apply again.

Moving the MMHotline-down alarm to another Cloudflare account is refused while it is set
up in the first: apply the file without the `mmhotline_down_alarm` section first, which
tells the first account's Worker to hold nothing, then add the section back
with the new Cloudflare account.

## The status page

`status_page` holds the page's title, its host name, the R2 bucket's
address, and the R2 Access Key ID and Secret Access Key for the bucket's
R2 API token (`r2_access_key_id` and `r2_secret_access_key`).
`page_title` is the page's title and heading exactly as written, such as
“Example Inc. status”: a team names the page as the team wants the title
read, so MMHotline adds no word to the title.

Three optional keys shape the public page:

- `website_url` and `support_url` add **Website** and **Support** links to
  the page's header. `website_url` is an `https://` address. `support_url` is
  an `https://` address or `mailto:` and one email address. Any other kind of
  address is refused, because a `javascript:` link would run a script on the
  page and an `http://` link would lead visitors to an unencrypted page.
- `components` lists the parts of the team's service that a notice can say it
  affects, such as the website or the API. Each has a `component_id` (lowercase
  letters, digits, hyphens, and underscores, which the command and the notices
  use), a `name`, and an optional `description`. The page lists each component
  with its status, which its open notices give it. At most 50 components keep
  the page scannable. Without `components`, the page has no components panel
  and a notice names none.

Reason these live in the settings file and not on the Status page page: the
settings file is the one place that holds the team's setup, so the team
reviews it and a deploy tool applies it, as for every other part. The
Status page page is for what people post. The apply copies `page_title`,
`host_name`, the links, and the components into `status.json`, because the
public files must not need MMHotline: the page and the Atom feed are built
from `status.json` alone, and the feed's links need the host name.

The R2 bucket, public host name, and URL rule are set up once, from a laptop
(CA8). The command `mmhotline status-page set-up` uses the person's own
Cloudflare API token. The command creates the R2 bucket and connects the
public host name to the bucket. Then the command adds the rule that serves
`index.html` at the public status-page host name without a path
([status page set-up](mmhotline-down-alarm-and-status-page.md#set-up)).
`mmhotline status-page set-up` prints `r2_bucket_url` for the settings file
and the next step: create an R2 API token on Cloudflare's site.
The R2 API token must have Object Read & Write on the newly created
status-page bucket only. Put the token's Access Key ID and Secret Access
Key in the settings file. MMHotline therefore never asks for a token that
can create other tokens, and never stores the person's Cloudflare API token.

Set-up uploads no first page. An object write needs the bucket's R2 access
key, which the person creates only after set-up, so the first
`settings apply` with the `status_page` section publishes the page.

The outside test posts the current page with the file's R2 key: it writes
`status.json`, `index.html`, and `feed.xml` with the file's `page_title`,
`host_name`, links, and components, and creates `status.json` when it is
missing. The notices already in `status.json` stay, including those that name
a component the file no longer lists. A key that cannot write the bucket fails
the part and is never saved. A section left out removes the setup; the
bucket and the page stay in Cloudflare.

## The fleet view command

`fleet_view_command` lists the monitoring tool's fleet view program and
arguments. The first item is the program's absolute path. Each later item
is one complete argument, which may contain spaces. The root fleet view
unit runs
`mmhotline run-fleet-view-command --settings-file FILE` for each read of the
dashboard. The helper `mmhotline run-fleet-view-command` reads the command
from the settings file each time
([operation](operation.md#the-fleet-view-socket)). Before running the root
command, the helper checks the settings file's owner and permissions and
validates `fleet_view_command` against the rules for the program and
arguments above.
A failed file check or invalid command value prevents execution, because
root runs the configured command.

A problem elsewhere in the settings file does not stop
`mmhotline run-fleet-view-command`. The dashboard therefore keeps showing
the servers while the MMHotline service uses earlier applied settings.
The systemd unit hides the database and `backup.env` from the helper and the
fleet view program. The settings file stays readable because the helper
reads the configured command. The fleet view program is the team's own root
program and has no network.

## Why backup.env stays separate

The backup's settings stay in `/etc/mmhotline/backup.env` (CA9):

- Litestream, a separate program, reads `/etc/mmhotline/backup.env` through
  Litestream's systemd unit.
- A restore on a new server reads the backup settings before MMHotline or
  the database exists.
- `mmhotline backup check-uploads` reads the backup settings.

A deploy tool renders `/etc/mmhotline/backup.env` from the vault, just as for
`/etc/mmhotline/mmhotline_settings.yml`. The backup settings therefore need
no separate manual configuration. MMHotline does not use `mmhotline.env`.
The server's addresses and the fleet view command belong in the settings
file.

`MMHOTLINE_FAKE_SENDING_SERVICES_URL` stays an environment variable: it
switches a development MMHotline to the fake sending services, and is not a
team setting.
