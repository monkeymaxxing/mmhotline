# The settings file

This page lists every key of the settings file,
`/etc/mmhotline/mmhotline_settings.yml`: its type, its allowed values, and
what `mmhotline settings check` says when a value is wrong. A **section** is
a key that holds other keys, such as `people` or `triage_agent`. A **part**
is a group of keys that `settings apply` applies together and reports on
one line, such as `people: applied.`. To edit, check, and apply the file
step by step, see [Change the settings](../how-to/change-the-settings.md).
Each key below links to the how-to page that sets it up and shows its
example.

## A minimal settings file

This file has every required key and no optional section. It sets up one
administrator, a weekly rotation, and one alert source:

```yaml
# Written by hand with sudoedit. Apply a change with sudo mmhotline settings apply.
team_name: Operations
web_url: https://ops.example.com
web_listen_address: 127.0.0.1:8480
alert_intake_listen_address: 10.20.0.1:8484

rules_for_running_agent_playbooks: {}
services_whose_changes_a_person_must_approve: []
sending_services: {}

people:
  - name: Barbara Example
    sign_in_email_address: barbara@example.com
    role: administrator

on_call_schedule:
  escalate_if_unclaimed_after_minutes: 5
  schedule_type: weekly_rotation
  time_zone: America/New_York
  handoff_hour: 9
  handoff_minute: 0
  handoff_weekday: 1
  first_start_date: "2026-11-02"
  rotation_sign_in_email_addresses: [barbara@example.com]
  backup_option: everyone_available

alert_sources:
  - alert_source_id: monitoring
    name: Monitoring
    signing_secrets: ["YOUR_64_HEXADECIMAL_CHARACTER_SIGNING_SECRET"]
    warn_after_no_check_in_for_minutes: 2
    sos_after_no_check_in_for_minutes: 5
```

With a real signing secret in place,
`sudo mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml`
prints:

```text
The settings file /etc/mmhotline/mmhotline_settings.yml has no problems.
```

[Get started](../get-started.md) writes this file in step 2.

## Rules for the whole file

| Rule | Detail |
| --- | --- |
| Format | YAML, one section of keys at the top. |
| Size | At most 1 MiB. Reason: the largest value, custom schedule code, is at most 64 KiB. |
| Owner and mode | MMHotline refuses a file whose owner is not an allowed owner (`root`), that group or other users can write (any of mode bits `0022`), or that other users can read (mode bit `0004`). The install's choice is owner `root`, group `mmhotline`, mode `0640`, so the `mmhotline` user can read it. Reason: the file decides what MMHotline does, `fleet_view_command` runs as root, and the file holds the team's secrets. `settings check` skips this rule, so a deploy tool can check a copy on a laptop; `settings apply`, `install`, and MMHotline's start apply it. [Installed files](installed-files.md#etcmmhotlinemmhotline_settingsyml) says who sets the owner. |
| Unknown keys | Refused, so a typo never passes silently. |
| Required keys | Every top-level key without "optional" below is required. Inside a section that is present, every key is required unless its table says optional. |
| Defaults | None, except `triage_agent.warn_waits_for_the_agent_minutes`, which is 0 when left out, and an action kind that `action_kinds` leaves out, which needs a person ([rules_for_running_agent_playbooks](#rules_for_running_agent_playbooks)). Reason: the person or agent who writes the file states every setting. `warn_waits_for_the_agent_minutes` left out keeps a file written before the key existed working: nothing is held, as before. |
| Optional sections | Leaving out an optional section, such as `status_page`, turns its feature off. `settings apply` then reports the part as `not set up, because the settings file leaves it out.` |
| First line | When the first line is a comment, the pages and `settings show` print it next to the file's path. Use it to say how the file is changed, such as `# Managed by MMX from mmhotline_settings.yml in the systems repository`. |
| Secrets | Values marked secret below never appear in a problem, a page, a log, or command output. A YAML syntax error names only its line. |

These problems apply to every key. `settings check` prints each as
`KEY_PATH: PROBLEM` and exits 1:

| Problem text | Cause |
| --- | --- |
| `required` | A required key is missing. |
| `unknown key` | The key is not on this page. |
| `unknown key; it was renamed to rules_for_running_agent_playbooks` | An old key name. The same text names the new key for `trust_settings`, `trust_setting` (now `rule`), `response_window_minutes` (now `escalate_if_unclaimed_after_minutes`), `services_that_always_need_a_person` (now `services_whose_changes_a_person_must_approve`), and `lifeline` (now `mmhotline_down_alarm`). |
| `has no value` | The key is present with nothing after the colon. |
| `appears twice` | The key is repeated in one section. |
| `must be a section of keys, each on its own line as key: value` | A section has a single value or a list. |
| `must be a list, such as [a, b], or [] for none` | A list key has a single value. |
| `must be a single value, not a list or a section` | A text key has a list or a section. |
| `must be a whole number` | A number key has text or a fraction. |
| `must be true or false` | A switch has another value. |

Problems with the whole file have no key path:
`the settings file is not valid YAML: ...` (with the line),
`the settings file is empty`,
`the settings file exceeds MMHotline's limit of 1 MiB`,
`the settings file must be a section of keys, each on its own line as key: value, such as team_name: Example Operations`
(the file holds a list or a single value), and
`MMHotline cannot read the settings file: ...` (the system's reason follows, such as permission denied).
The owner and mode refusals are
`... is owned by user ID N, not root. Only root may change it ...`,
`... can be changed by users other than root (mode 0664). Only root may change it ...`, and
`... can be read by every user on the server (mode 0644), and it holds the team's secrets. Fix the settings file owner and mode with: sudo chown root:mmhotline /etc/mmhotline/mmhotline_settings.yml; sudo chmod 0640 /etc/mmhotline/mmhotline_settings.yml`.

An example with several problems, from a real check:

```text
team_nam: unknown key
lifeline: unknown key; it was renamed to mmhotline_down_alarm
team_name: required
on_call_schedule.escalate_if_unclaimed_after_minutes: must be a whole number of minutes from 1 to 60
on_call_schedule: the first start date must be a date such as 2026-11-02 on the handoff weekday (0 is Sunday)
alert_sources[0].sos_after_no_check_in_for_minutes: the time before waking the person on call must be longer than the time before a warning
```

A key path shows each list item by its position from 0, such as
`people[1].role`. A key gets one problem at a time; fix it and check again.

## The top-level keys

| Key | Required | Section below |
| --- | --- | --- |
| `team_name` | Yes | [team_name](#team_name) |
| `web_url` | Yes | [web_url](#web_url) |
| `web_listen_address` | Yes | [web_listen_address](#web_listen_address) |
| `alert_intake_listen_address` | Yes | [alert_intake_listen_address](#alert_intake_listen_address) |
| `cloudflare_access` | Optional | [cloudflare_access](#cloudflare_access) |
| `fleet_view_command` | Optional | [fleet_view_command](#fleet_view_command) |
| `people` | Yes | [people](#people) |
| `on_call_schedule` | Yes | [on_call_schedule](#on_call_schedule) |
| `alert_sources` | Yes, may be `[]` | [alert_sources](#alert_sources) |
| `systems_repository_commits` | Optional | [systems_repository_commits](#systems_repository_commits) |
| `triage_agent` | Optional | [triage_agent](#triage_agent) |
| `review_agent` | Optional | [review_agent](#review_agent) |
| `rules_for_running_agent_playbooks` | Yes, may be `{}` | [rules_for_running_agent_playbooks](#rules_for_running_agent_playbooks) |
| `services_whose_changes_a_person_must_approve` | Yes, may be `[]` | [services_whose_changes_a_person_must_approve](#services_whose_changes_a_person_must_approve) |
| `sending_services` | Yes, may be `{}` | [sending_services](#sending_services) |
| `mmhotline_down_alarm` | Optional | [mmhotline_down_alarm](#mmhotline_down_alarm) |
| `status_page` | Optional | [status_page](#status_page) |

`settings apply` applies the parts in this order, one line each:
the four server and sign-in keys together, `team_name`,
`fleet_view_command`, `people`, `on_call_schedule`,
`rules_for_running_agent_playbooks`,
`services_whose_changes_a_person_must_approve`, `alert_sources`, the
`systems_repository_commits` parts, the triage agent's parts, the MMHotline-down alarm's wake times,
`review_agent`, each sending service, `mmhotline_down_alarm`, and
`status_page`. People apply before the schedule that names them. A part
that has not changed is not applied again. A part that MMHotline refuses
keeps its earlier setting, and the other parts still apply. The command
exits 0 only when every part applied.

MMHotline's pages show the settings file's values read-only. Each settings
page folds the file's path, first comment line, and editing instructions
under a heading that names the settings. Personal actions stay on the
pages: contact methods, unavailability, taking on-call, team mute, claims,
approvals, status page posts, and incidents.

## Server and sign-in

`web_url`, `web_listen_address`, `alert_intake_listen_address`, and
`cloudflare_access` apply as one part and change only when MMHotline
restarts. When one of them changes, `settings apply` restarts MMHotline
with `systemctl restart mmhotline.service` first, then applies the rest
([why](../changing-mmhotline/design/settings-file.md#restarts)).

### team_name

The team's name, shown on every page and in alerts.

```yaml
team_name: Operations
```

| Fact | Value |
| --- | --- |
| Required | Yes |
| Type | Text, 1 to 100 characters; spaces around it are dropped |
| Default | None |
| Wrong value | `team_name: team name must contain 1–100 characters` |
| Set up in | [Get started](../get-started.md), step 2, and [Install MMHotline](../how-to/install.md) |

`mmhotline install` names the new database after `team_name`. A later
change applies as its own part.

### web_url

The address people open. Links in pages, notifications, and command
output start with it.

```yaml
web_url: https://ops.example.com
```

| Fact | Value |
| --- | --- |
| Required | Yes |
| Type | An origin: scheme, host name, and optional port, with no path, query, or user name. A trailing `/` is dropped. |
| Allowed values | Any `https://` origin, such as `https://ops.example.com`. For a team without Cloudflare that opens MMHotline through an SSH forward: `http://localhost:PORT` or `http://127.0.0.1:PORT`, where the port is a whole number from 1 to 65535 and must be written. |
| Default | None |
| Wrong value | `web_url: must be an HTTPS origin, such as https://mmhotline.example.com, or http://localhost:PORT or http://127.0.0.1:PORT` |
| Takes effect | At a restart, which `settings apply` does |
| Set up in | [Get started](../get-started.md), step 2, and [Put MMHotline behind Cloudflare Access](../how-to/put-mmhotline-behind-cloudflare-access.md) |

### web_listen_address

The host and port where MMHotline serves its pages over plain HTTP on the
server.

```yaml
web_listen_address: 127.0.0.1:8480
```

| Fact | Value |
| --- | --- |
| Required | Yes |
| Type | `host:port`, with a port from 0 to 65535 |
| Allowed values | Normally `127.0.0.1:8480`. Reason: only programs on the server, such as `cloudflared` and SSH forwards, then reach the pages. |
| Default | None |
| Wrong value | `web_listen_address: must be a host:port address, such as 127.0.0.1:8480` |
| Takes effect | At a restart, which `settings apply` does |
| Set up in | [Get started](../get-started.md), step 2 |

### alert_intake_listen_address

The host and port where MMHotline accepts signed alerts and check-ins.
Only those two routes answer there; every other path returns 404.

```yaml
alert_intake_listen_address: 10.20.0.1:8484
```

| Fact | Value |
| --- | --- |
| Required | Yes |
| Type | `host:port`, with a port from 0 to 65535 |
| Allowed values | An address of the MMHotline server on its private network, such as a WireGuard address. `127.0.0.1` works when every tool that sends alerts runs on the same server. |
| Default | None |
| Wrong value | `alert_intake_listen_address: must be a host:port address, such as 127.0.0.1:8480` |
| Takes effect | At a restart, which `settings apply` does |
| Set up in | [Get started](../get-started.md), step 2, and [Connect a tool that sends alerts](../how-to/connect-a-tool-that-sends-alerts.md) |

`settings check` cannot know the server's addresses. An address that is
not the server's own passes the check, and MMHotline then fails to start
with
`MMHotline cannot listen on alert_intake_listen_address 10.20.0.9:8484: listen tcp 10.20.0.9:8484: bind: cannot assign requested address`.
[Install MMHotline](../how-to/install.md#if-something-is-different) says
what to do.

### cloudflare_access

Sign-in through Cloudflare Access. MMHotline checks the token that
Cloudflare Access signs for each request.

```yaml
cloudflare_access:
  team_domain: example.cloudflareaccess.com
  audience_tag: YOUR_APPLICATION_AUDIENCE_TAG
```

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `cloudflare_access` | Optional | Section. Without it, people sign in only with sign-in links. | |
| `cloudflare_access.team_domain` | Yes | The Cloudflare Access team domain, a host name without `https://`, path, port, or spaces | `the Cloudflare Access team domain must be a host name without a URL scheme, path, port, or spaces` |
| `cloudflare_access.audience_tag` | Yes | The Access application's Application Audience (AUD) Tag: ASCII letters and digits | `the Cloudflare Access audience tag must contain only ASCII letters and digits` (or `... must not be empty and must contain only ASCII letters and digits`) |

Takes effect at a restart, which `settings apply` does. At start,
MMHotline logs which sign-in methods it accepts:
`Sign-in: Cloudflare Access for example.cloudflareaccess.com, and sign-in links`,
or `Sign-in: sign-in links only; the settings file has no cloudflare_access`.

Set up in [Put MMHotline behind Cloudflare Access](../how-to/put-mmhotline-behind-cloudflare-access.md).
Without the section, see
[Sign in during a Cloudflare outage](../how-to/sign-in-during-a-cloudflare-outage.md).

### fleet_view_command

The command that prints your monitoring tool's fleet view as JSON. The
dashboard shows servers that need attention, with their failing checks,
and healthy servers by name, from the command's output.

```yaml
fleet_view_command: [/usr/local/bin/MONITORING_TOOL, fleet, --json]
```

| Fact | Value |
| --- | --- |
| Required | Optional. Without it, the dashboard says in one line that there is no fleet view. |
| Type | A list of words: the program, then each argument as its own item |
| Allowed values | The first item is the program's absolute path. No item is empty or holds a control character. |
| Default | None |
| Wrong value | `fleet_view_command: must list the program and its arguments, such as [/usr/local/bin/PROGRAM, fleet, --json]`; `fleet_view_command[0]: must be the program's absolute path, such as /usr/local/bin/PROGRAM`; `fleet_view_command[N]: must be one word without control characters` |
| Set up in | [Connect a tool that sends alerts](../how-to/connect-a-tool-that-sends-alerts.md#show-the-tools-servers-on-the-dashboard) |

- The command runs with no shell, so quotes, variables, and pipes in its
  words mean nothing.
- The command runs as root, through systemd, in the unit
  `mmhotline-fleet-view@.service`. Reason: a monitoring tool's fleet view
  usually admits only root, and MMHotline itself runs as `mmhotline` and
  cannot gain privileges. MMHotline can only request a run of the
  configured command; only root can change the command, because only root
  can change the settings file.
- Each run is one connection to `/run/mmhotline-fleet-view.sock`, which only the `mmhotline` user may open.
- Each run has a read-only file system, no network except local Unix
  sockets, only `PATH` in its environment, and 20 seconds before systemd
  ends it. MMHotline's database, the backup's files, `backup.env`, and the
  triage agent's home are hidden from it.
- The root unit reads the command from the settings file at every run.
  Run `sudo mmhotline settings apply` after adding the command, so the
  dashboard knows there is one. A problem elsewhere in the settings file
  does not stop the fleet view.
- Errors go to the journal: `sudo journalctl -u 'mmhotline-fleet-view@*'`.
- [The design](../changing-mmhotline/design/interface.md#the-monitoring-tools-fleet-view)
  lists the JSON fields MMHotline reads.

## People and the schedule

### people

Everyone who uses MMHotline. A person's sign-in email address identifies
the person at sign-in.

```yaml
people:
  - name: Barbara Example
    sign_in_email_address: barbara@example.com
    role: administrator
  - name: Carlos Example
    sign_in_email_address: carlos@example.com
    role: member
```

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `people` | Yes | A list with at least one `administrator` | `people: needs at least one person with role administrator, who opens the administration pages` |
| `people[].name` | Yes | Text, 1 to 100 characters | `person name must contain 1–100 characters` |
| `people[].sign_in_email_address` | Yes | The address the person's Cloudflare Access sign-in gives: 3 to 254 characters, one `@` with text on both sides, no spaces. Saved in lowercase. Unique. | `sign-in email address must have 3–254 characters, exactly one @ with text on both sides, and no spaces or control characters`; `carlos@example.com is also the sign-in email address of people[0]; each person must have a unique sign-in email address` |
| `people[].role` | Yes | `administrator` or `member`. Administrators also open the administration pages, such as the Triage agent page, and see other people's contact methods. | `person role must be administrator or member` |

- A person removed from the list can no longer sign in, and their contact
  methods are removed; their history stays.
- MMHotline refuses to remove a person who owns a claimed incident or has
  taken on-call, and the part keeps its earlier list, such as:
  `people: did not apply: Bruno Costa owns 1 claimed incident(s). Resolve the claimed incidents or have someone take over the claimed incidents before removing the person. The earlier setting, applied at 2026-11-02T09:00:00Z, stays.`

Set up in [Set up people and the on-call schedule](../how-to/set-up-people-and-the-on-call-schedule.md).

### on_call_schedule

Who is on call, who is the backup, and how long each escalation step
waits for a claim. Each schedule type uses its own keys, and a key another
type uses is refused.

| Key | Required | Type and allowed values |
| --- | --- | --- |
| `on_call_schedule.escalate_if_unclaimed_after_minutes` | Yes | 1 to 60: how long each escalation step, such as the person on call and then the backup, waits for someone to claim an SOS before MMHotline contacts the next step. The wait starts again at each step. |
| `on_call_schedule.schedule_type` | Yes | `weekly_rotation`, `day_of_week`, `hour_of_day`, or `custom_schedule_code` |
| `on_call_schedule.time_zone` | Yes | A named time zone, such as `America/New_York` |
| `on_call_schedule.backup_option` | Yes | `everyone_available`, `fixed_person`, or, for `weekly_rotation` only, `next_in_rotation` |
| `on_call_schedule.backup_sign_in_email_address` | With `fixed_person` only | The fixed backup's sign-in email address |
| `on_call_schedule.handoff_hour`, `on_call_schedule.handoff_minute` | `weekly_rotation`, `day_of_week` | The local handoff time, 0 to 23 and 0 to 59 |
| `on_call_schedule.handoff_weekday` | `weekly_rotation` | 0 (Sunday) to 6 |
| `on_call_schedule.first_start_date` | `weekly_rotation` | The date the first person starts, in quotes, such as `"2026-11-02"`, on the handoff weekday |
| `on_call_schedule.rotation_sign_in_email_addresses` | `weekly_rotation` | The rotation's people in order, each once |
| `on_call_schedule.assignment_sign_in_email_addresses` | `day_of_week`, `hour_of_day` | 7 sign-in email addresses, Sunday first, or 24, hour 0 first; `""` for no assigned person |
| `on_call_schedule.custom_schedule_code` | `custom_schedule_code` | Starlark code with a function `on_call(at)`, at most 64 KiB |
| `on_call_schedule.custom_schedule_test_cases` | `custom_schedule_code` | At most 50 test cases, or `[]`. Each has `local_time`, such as `"2026-11-02T09:30"`, `expected_primary_sign_in_email_address` (`""` for no one), and optionally `expected_backup_sign_in_email_address`. |

Every sign-in email address in `on_call_schedule` must name a person in
`people`.

What `settings check` says when a value is wrong:

| Problem | Text |
| --- | --- |
| Escalation wait out of range | `on_call_schedule.escalate_if_unclaimed_after_minutes: must be a whole number of minutes from 1 to 60` |
| Unknown type | `on_call_schedule.schedule_type: must be day_of_week, hour_of_day, weekly_rotation, or custom_schedule_code` |
| A key the type needs | `on_call_schedule.first_start_date: required for a weekly_rotation schedule` |
| A key the type does not use | `on_call_schedule.handoff_weekday: a day_of_week schedule does not use the setting. Remove the unused setting` |
| Fixed backup without a person | `on_call_schedule.backup_sign_in_email_address: required with backup_option fixed_person` |
| A backup person without `fixed_person` | `on_call_schedule.backup_sign_in_email_address: used only with backup_option fixed_person. Remove the backup_sign_in_email_address setting` |
| A person not in `people` | `dana@example.com is not the sign-in email address of any person listed in people` |
| Time zone | `on_call_schedule: the schedule needs a named time zone, such as America/New_York`, or `the time zone X is unknown; use a name such as America/New_York` |
| Start date | `on_call_schedule: the first start date must be a date such as 2026-11-02 on the handoff weekday (0 is Sunday)` |
| Rotation | `rotation needs at least one member`; `select a person for each rotation slot and do not list a person more than once` |
| Assignments | `a day_of_week schedule has 7 assignment slots, one for each day of the week, Sunday first`; `an hour_of_day schedule has 24 assignment slots, one for each hour of the day, 0 first` |
| Handoff time | `the handoff hour must be 0 to 23, and the handoff minute 0 to 59` |
| Backup option | `the backup option must be everyone_available, fixed_person, or next_in_rotation` (for a weekly rotation), else `the backup option must be everyone_available or fixed_person` |
| Custom code | `write your code, with a function on_call(at)`; `code must be text of at most 64 KiB`; `line N has a control or text-direction character; remove it`; `at most 50 test cases`; `test case time must look like 2026-10-05T09:30` |

Applying the part saves and activates the schedule. To see whose turn the
file's schedule gives before you apply it, run
`sudo mmhotline schedules preview --settings-file /etc/mmhotline/mmhotline_settings.yml`.
Taking on-call and unavailability stay on the On call page.

Set up in
[Set up people and the on-call schedule](../how-to/set-up-people-and-the-on-call-schedule.md),
and for `custom_schedule_code`, in
[Write a custom schedule](../how-to/write-a-custom-schedule.md).

## Alert sources

### alert_sources

The programs that send alerts and check-ins, such as a monitoring tool.
Each source signs its requests with its signing secret.

```yaml
alert_sources:
  - alert_source_id: monitoring
    name: Monitoring
    signing_secrets: ["YOUR_64_HEXADECIMAL_CHARACTER_SIGNING_SECRET"]
    warn_after_no_check_in_for_minutes: 2
    sos_after_no_check_in_for_minutes: 5
```

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `alert_sources` | Yes | A list of at most 100 sources; may be `[]` | `alert_sources: lists 101 alert sources; an installation has at most 100` |
| `alert_sources[].alert_source_id` | Yes | Your own ID, 1 to 64 letters, digits, `_` and `-`, such as `monitoring`; unique; not `system`, `manual`, or `mmhotline_test_sos`. The sender's URLs end with it: `/alert-intake/monitoring` and `/check-in/monitoring`. | `must be 1 to 64 letters, digits, _ and -, such as monitoring`; `system names MMHotline's own alerts; choose another ID`; `monitoring is also the ID of alert_sources[0]; each alert source must have a unique ID` |
| `alert_sources[].name` | Yes | Text, 1 to 100 characters, shown on pages and in alerts | `alert source name must contain 1–100 characters` |
| `alert_sources[].signing_secrets` | Yes, secret | One signing secret of 64 hexadecimal characters, which `openssl rand -hex 32` prints, or two different ones while the sender moves to a new one | `must list one signing secret, or two while the sender moves to a new one`; `signing_secrets[0]: must be 64 hexadecimal characters, such as the output of openssl rand -hex 32`; `signing_secrets[1]: is the same secret as signing_secrets[0]` |
| `alert_sources[].warn_after_no_check_in_for_minutes` | Yes | 1 to 60, or `off`: a warn alert after this many minutes without a check-in | `must be a whole number of minutes from 1 to 60, or off` |
| `alert_sources[].sos_after_no_check_in_for_minutes` | Yes | 1 to 60, and longer than the warning: an SOS after this many minutes without a check-in | `the time before waking the person on call must be 1 to 60 minutes`; `the time before waking the person on call must be longer than the time before a warning` |

- A source removed from the list is disabled: its signing secrets stop
  working, and its alerts and check-ins stay.
- To move a sender to a new signing secret, list both secrets, apply, give
  the sender the new one, then remove the old one and apply again.
- `sudo mmhotline alert-sources list` prints each source's intake and
  check-in URLs.

Set up in [Connect a tool that sends alerts](../how-to/connect-a-tool-that-sends-alerts.md);
[Change a source later](../how-to/connect-a-tool-that-sends-alerts.md#change-a-source-later)
covers the secret change.

## The systems repository

### systems_repository_commits

Which commits of the systems repository MMHotline trusts: MMX runs only
past a trusted commit when it asks MMHotline's commit check
(`sudo mmhotline commit-reviews check`), and the triage agent's runs and
playbook runs use only trusted commits. The section works with or without
the triage agent.

Leaving the section out trusts no commit: the commit check refuses, and
so does every MMX run for the triage agent. A lock-out leaves the section
out for this reason. Leaving it out keeps the checked-through commit, so
the check does not read the whole history again when the section returns.

| Key | Required | Type and allowed values |
| --- | --- | --- |
| `systems_repository_commits.trusted_commits` | Yes | Which commits on origin pass. `signed_by_a_trusted_key_or_reviewed`: a commit passes when a trusted key signed it, or when a person (or, when allowed, the triage agent) reviewed it. `every_commit_on_origin`: every commit on origin's branch passes with no check, so none waits. Use `every_commit_on_origin` only when origin is on the team's own server, only keys that already hold root on every server can push to it, and the `triage` user on the servers cannot write to it. With origin on a Git host such as GitHub, it would give everyone who can push there root on every server. Switching back does not check the commits that arrived meanwhile. |
| `systems_repository_commits.trusted_commit_signing_public_keys` | Required with `signed_by_a_trusted_key_or_reviewed`; left out with `every_commit_on_origin` | The public key lines of the team's people and machines, such as each person's desk and util01. A commit no listed key signed waits for a review before any MMX run goes past it. Never the triage agent's own key. May be `[]`. |
| `systems_repository_commits.commits_without_a_trusted_signature_are_approved_by` | Required with `signed_by_a_trusted_key_or_reviewed`; left out with `every_commit_on_origin` | `a_person`, or `a_person_or_the_triage_agent`: the triage agent then reviews each such commit and approves an ordinary change. `a_person_or_the_triage_agent` needs the `triage_agent` section. |
| `systems_repository_commits.paths_only_a_person_may_approve` | Required with `signed_by_a_trusted_key_or_reviewed`; left out with `every_commit_on_origin` | Paths in the systems repository, as Git names them, that only a person may approve; an entry ending in `/` is a directory. List the files that set the triage agent's own powers and the trusted keys. May be `[]`. |

The triage agent's own key, `triage_agent.commit_signing_public_key`, stays
in [triage_agent](#triage_agent): it is required there with
`signed_by_a_trusted_key_or_reviewed` and refused with
`every_commit_on_origin`. Without the `triage_agent` section no agent key
is needed, and only the trusted keys pass a commit.

What `settings check` says when a value is wrong, each after its key path:

| Key | Text |
| --- | --- |
| `trusted_commits` | `required`; `must be signed_by_a_trusted_key_or_reviewed or every_commit_on_origin` |
| `trusted_commit_signing_public_keys`, `commits_without_a_trusted_signature_are_approved_by`, `paths_only_a_person_may_approve`, and `triage_agent.commit_signing_public_key`, with `every_commit_on_origin` | `used only with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed; remove it` |
| The same three keys of this section, left out with `signed_by_a_trusted_key_or_reviewed` | `required` |
| `trusted_commit_signing_public_keys[N]` | `must be one OpenSSH public key line: the key type, such as ssh-ed25519, then the key, then an optional comment`; `starts with "X", which is not an SSH key type: ...`; `has a key that is not valid base64: ...`; `must be one OpenSSH public key line, without a line break or a NUL character`; `is the triage agent's key, triage_agent.commit_signing_public_key; list only the keys of the team's people and machines, because the triage agent's commits may change only one_time_playbooks/ and triage_agent_notes/` |
| `commits_without_a_trusted_signature_are_approved_by` | `must be a_person or a_person_or_the_triage_agent`; without the `triage_agent` section, `a_person_or_the_triage_agent needs the triage_agent section: without it, use a_person` |
| `paths_only_a_person_may_approve[N]` | `must be a path in the systems repository, such as AGENTS.md, or a directory ending in /, such as roles/every_server/files/`; `"/etc/sudoers" must be relative to the systems repository, ...`; `... must not have an empty, ".", or ".." part, ...`; `AGENTS.md is listed twice` |
| `triage_agent.trusted_commits`, `triage_agent.trusted_commit_signing_public_keys`, `triage_agent.commits_without_a_trusted_signature_are_approved_by`, `triage_agent.paths_only_a_person_may_approve` | `moved to systems_repository_commits.KEY`, with the key's name in place of KEY |

Notes that `settings check` and `settings apply` print, which never fail
the check:

- Under `systems_repository_commits.trusted_commits` with
  `every_commit_on_origin`: `MMHotline trusts every commit on origin: ...`.
- Under `systems_repository_commits.commits_without_a_trusted_signature_are_approved_by`,
  what the setting means, such as
  `A person reviews every commit no trusted key signed before MMX runs past it.`
  With `a_person_or_the_triage_agent` and an empty
  `paths_only_a_person_may_approve`, the note adds
  `The list is empty, so the triage agent may approve a commit that changes its own rules or the trusted keys: list the files that set them.`

The keys apply as these parts: `trusted_commits`; the signing keys,
together with `triage_agent.commit_signing_public_key`; and the two commit
approval settings. MMHotline uses only the applied values.

Set up in [Review a commit](../how-to/review-a-commit.md).

## The agents

### triage_agent

The triage agent: the team's coding agent, which investigates alerts and
asks MMHotline to run playbooks. Leaving the section out removes the
triage agent's setup.

| Key | Required | Type and allowed values |
| --- | --- | --- |
| `triage_agent.commit_signing_public_key` | Required with `systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed`; left out with `every_commit_on_origin`; unused without `systems_repository_commits` | The public key line the triage agent signs its commits with, from `/var/lib/mmhotline-triage-agent/.ssh/id_ed25519.pub`. MMHotline knows the agent's commits by it: they pass only within `one_time_playbooks/` and `triage_agent_notes/`. |
| `triage_agent.coding_agent` | Yes | `claude_code` or `codex` |
| `triage_agent.credential_type` | Yes | For `claude_code`: `api_key` or `subscription_token`. For `codex`: `api_key`, `access_token` (a Codex access token from a ChatGPT Business or Enterprise workspace), or `chatgpt_sign_in` (a ChatGPT sign-in made just for MMHotline). `api_key` is the most dependable. |
| `triage_agent.credential` | Yes, secret | An API key, the token from `claude setup-token`, or a Codex access token: one line, at most 4,096 bytes. For `chatgpt_sign_in`: the whole content of Codex's `auth.json` after `codex login`, at most 64 KiB. |
| `triage_agent.credential_expires_on` | With `access_token` only | The Codex access token's end date, such as `2027-02-06`, as ChatGPT showed it. MMHotline reminds administrators 30 and 7 days before. |
| `triage_agent.model` | Yes | A model name of at most 200 characters without spaces, such as `opus`. MMHotline checks only the shape; the agent's setup test proves the model works. |
| `triage_agent.reasoning_effort` | Yes | One lowercase word, such as `high`. Claude Code takes `low`, `medium`, `high`, `xhigh`, or `max`; `codex debug models` lists Codex's levels. MMHotline checks only the shape. |
| `triage_agent.internet_access` | Yes | `listed_hosts_only`, or `any_site`, which also allows unlisted hosts at public addresses, through MMHotline's logging proxy |
| `triage_agent.listed_hosts` | Yes | Extra hosts the agent may reach, each `{host: NAME, port: N}`, such as `[{host: packages.example.com, port: 443}]`; usually `[]`. Do not list the hosts MMHotline adds by itself: it lists the coding agent's service, OSV.dev's API, CISA's list of known exploited vulnerabilities on `raw.githubusercontent.com`, each inventory server at port 22, and the vendor pages' hosts in `software_outside_the_distribution.yml`. |
| `triage_agent.software_review_interval_hours` | Yes | `off`, or 1 to 8760 hours between scheduled software reviews |
| `triage_agent.sos_waits_for_the_agent_minutes` | Yes | 0 to 15 minutes an SOS from an alert source waits for the agent before MMHotline wakes the person on call. 0 wakes the person at once while the agent still looks. 5 is a good start: the wait gives the agent a chance to fix the problem before anyone is woken. An SOS that joins a triage session which already woke the person on call does not wait or wake them again. |
| `triage_agent.warn_waits_for_the_agent_minutes` | Optional; left out means 0 | 0 to 15 minutes MMHotline holds a warning that starts a triage session, or joins one while its SOS waits for the agent. 0 holds nothing. 3 makes a routine fix reach people as one message, the agent's report. The rules for a held warning are below the table. |
| `triage_agent.sos_waits_for_a_server_replacement_minutes` | Yes | 0 to 60 minutes a waiting SOS keeps waiting after the agent starts replacing a server, counted from the replacement's start. 0 turns the longer wait off. MMHotline sends one warn alert as the replacement starts. |

How `warn_waits_for_the_agent_minutes` works:

- MMHotline delivers a held warning at the end of the wait.
- If the agent asked for a playbook run or sent a report of any severity,
  even `info`, in that session meanwhile, the agent's request or report
  takes the warning over, and MMHotline sends nothing for the warning. So
  an agent that a log misleads into reporting "all fine" also keeps the
  warning from people.
- MMHotline delivers the warning at once when the agent cannot take it: the
  agent is not set up, its sandbox is not installed, MMHotline's copy of the
  systems repository is missing, or its run fails or ends without a request
  or report.
- A warning that joins a session while an SOS waits is held together with
  that SOS. An SOS keeps its own wait.

Limits and their reasons:

- `sos_waits_for_the_agent_minutes` and `warn_waits_for_the_agent_minutes`
  stop at 15. Reason: a stuck or wrong agent must not hold an SOS or a
  warning longer.
- `sos_waits_for_a_server_replacement_minutes` stops at 60. Reason: one MMX
  run ends after one hour, so a replacement cannot take longer. 10 is a
  good start: a measured replacement by the agent took about 5 to 6
  minutes.

What `settings check` says when a value is wrong, each after its key path:

| Key | Text |
| --- | --- |
| `commit_signing_public_key` | `required with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed, so MMHotline knows the triage agent's commits`; `used only with systems_repository_commits.trusted_commits: signed_by_a_trusted_key_or_reviewed; remove it`; `must be one OpenSSH public key line: the key type, such as ssh-ed25519, then the key, then an optional comment`, and the other key problems of [systems_repository_commits](#systems_repository_commits) |
| `coding_agent` | `must be claude_code or codex` |
| `credential_type` | `must be api_key or subscription_token for claude_code`, or `must be api_key or access_token or chatgpt_sign_in for codex` |
| `credential` | `CREDENTIAL_TYPE requires a credential` (when `credential` is empty, with the credential type you wrote in place of CREDENTIAL_TYPE, such as `api_key requires a credential`); `credential must be at most 4,096 bytes`; `credential must be one line without control characters`; for `chatgpt_sign_in`, `credential must be the whole content of Codex's auth.json after codex login, but it ...` with the reason |
| `credential_expires_on` | `is required with credential_type access_token: ...`; `must be a date such as 2027-02-06`; `is only for credential_type access_token; ...` |
| `model` | `must be a model name of at most 200 characters without spaces, such as opus; the coding agent's setup test checks whether the service recognizes the model name` |
| `reasoning_effort` | `must be a lowercase word, such as high; the coding agent's setup test checks whether the model supports the reasoning effort` |
| `internet_access` | `must be listed_hosts_only or any_site` |
| `listed_hosts[N]` | `port must be 1 to 65535`; `host must be a DNS name or an IP address`; `Loopback addresses are never allowed.` (also link-local, unspecified, and multicast addresses); `packages.example.com:443 is listed twice` |
| `software_review_interval_hours` | `must be off, or a whole number of hours from 1 to 8760` |
| `sos_waits_for_the_agent_minutes` | `must be a whole number of minutes from 0 to 15: 0 wakes the person on call at once, and a stuck or wrong agent must not hold an SOS longer than 15 minutes` |
| `warn_waits_for_the_agent_minutes` | `must be a whole number of minutes from 0 to 15: 0 holds no warning, and a stuck or wrong agent must not hold a warning longer than 15 minutes` |
| `sos_waits_for_a_server_replacement_minutes` | `must be a whole number of minutes from 0 to 60: 0 turns the longer wait off, and an MMX run longer than its one-hour limit cannot finish a replacement` |

Notes that `settings check` and `settings apply` print, which never fail
the check:

- Under `triage_agent.software_review_interval_hours`:
  `Scheduled software review: off. The triage agent wakes only for alerts; sudo mmhotline triage-agent run-software-review still runs one review, such as to learn what one costs.`,
  or `Scheduled software review: every 24 hours, with the team's task in ...`.

The keys apply as these parts: `commit_signing_public_key`, with the
trusted keys of [systems_repository_commits](#systems_repository_commits);
the agent setup (`coding_agent`, `credential_type`,
`credential`, `credential_expires_on`, `model`, `reasoning_effort`); and
each other key on its own. The agent setup runs one test prompt first and
is saved only when the test passes. While another triage agent run is
active, the agent setup does not apply; apply again after the run ends.

Start with `software_review_interval_hours: off` and test what one review costs first.
A playbook listed with `starts_a_server_replacement: true` starts the replacement; MMHotline wakes the person on call at once when a run of it fails.
How one approval can cover a known sequence of playbooks: [Decide what the triage agent may do](../how-to/decide-what-the-triage-agent-may-do.md#set-the-rules).

Set up in [Set up the triage agent](../how-to/set-up-the-triage-agent.md#set-up-the-agent).
The signing keys and approvals: [systems_repository_commits](#systems_repository_commits).
The credential: [Keep the agents signed in](../how-to/keep-the-agents-signed-in.md).
`software_review_interval_hours`:
[Turn on the scheduled software review](../how-to/turn-on-the-scheduled-software-review.md).

### review_agent

A second coding agent, which must approve each playbook the triage agent
asks to run before the team's rules apply. It has its own Unix user and
runs with no systems repository.

```yaml
review_agent:
  coding_agent: codex
  credential_type: api_key
  credential: "YOUR_OPENAI_API_KEY"
  model: YOUR_MODEL
  reasoning_effort: high
```

| Fact | Value |
| --- | --- |
| Required | Optional. Leaving it out removes the review agent's setup. |
| Keys | `coding_agent`, `credential_type`, `credential`, `credential_expires_on`, `model`, and `reasoning_effort`, with the same values and messages as in [triage_agent](#triage_agent) |
| Extra rule | Two `chatgpt_sign_in` credentials must be different sign-ins: `review_agent.credential: The review agent needs its own ChatGPT sign-in. Sign in again in a new empty folder and use that auth.json.` Reason: two copies of one sign-in end each other when either refreshes. |
| Applies | As one part, after one test prompt passes |
| Set up in | [Set up the triage agent](../how-to/set-up-the-triage-agent.md#add-the-review-agent) |

| Key | Required | Type and allowed values |
| --- | --- | --- |
| `review_agent.credential` | Yes, secret | The same kind of value as `triage_agent.credential` |

### rules_for_running_agent_playbooks

For each inventory, which playbook runs the triage agent may start alone
and which need a person.

```yaml
rules_for_running_agent_playbooks:
  production:
    preset: routine_fixes_without_permission
```

| Key | Required | Type and allowed values |
| --- | --- | --- |
| `rules_for_running_agent_playbooks` | Yes; may be `{}` | One entry per inventory, named as in `inventories/NAME.yml`: letters, digits, `_`, and `-`, at most 100 characters |
| `<inventory>.preset` | `preset` or `action_kinds`, not both | `permission_for_every_fix`, `routine_fixes_without_permission`, or `every_fix_without_permission` |
| `<inventory>.action_kinds` | `preset` or `action_kinds`, not both | A rule for each kind of action: `start_or_restart_a_service`, `free_disk_space`, `pull_a_container_image_again`, `reboot_a_server`, `block_a_network_range`, `change_dns_records`, `check_only`, `unrecognized_task` |
| `<inventory>.action_kinds.<kind>.rule` | Yes | `get_permission_then_act`, `warn_wait_then_act`, or `act_then_report` |
| `...rule` with `approvals_needed` | With `get_permission_then_act` only | 1 or 2 |
| `...rule` with `warning_wait_minutes` | With `warn_wait_then_act` only | 1 to 1440 |
| `<inventory>.playbooks` | Optional | Ordinary playbooks listed by name, NAME of `playbooks/NAME.yml`, each with a `rule` as for a kind. A listed playbook takes its own rule instead of the strictest rule of its kinds, even when a task acts on a service whose changes a person must approve. A one-time playbook cannot be listed. |
| `<inventory>.playbooks.<name>.its_approval_also_approves_in_the_same_session` | Optional, with `get_permission_then_act` only | Other playbooks listed on the same inventory that a person's approval of this one also approves, once each, later in the same triage session |
| `<inventory>.playbooks.<name>.starts_a_server_replacement` | Optional | `true` marks the playbook whose run starts a server replacement, so a waiting SOS waits `triage_agent.sos_waits_for_a_server_replacement_minutes` longer |

Every inventory in MMHotline's copy of the systems repository needs an
entry. An inventory without one fails the part, which still applies the
inventories it lists, and every fix on the missing inventory needs a person
until you add the entry:
`The rules for running the agent's playbooks applied for the inventories listed in the settings file. Any inventory without rules requires a person to approve every fix until you add its rules.`

An action kind that `action_kinds` leaves out needs a person, with 1
approval. Reason: a newer MMHotline may add a kind, and an older file must
still apply. `settings check` and `settings apply` print a note, which does
not fail the check:

```text
rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one
```

What `settings check` says when a value is wrong, each after its key path:

| Problem | Text |
| --- | --- |
| Inventory name | `the inventory name "..." may have only letters, digits, "_", and "-", and at most 100 characters` |
| Both or neither | `has preset and action_kinds. Specify only preset or only action_kinds`; `needs preset or action_kinds` |
| Preset | `must be permission_for_every_fix, routine_fixes_without_permission, or every_fix_without_permission` |
| Kind | `unknown kind of action; the kinds are start_or_restart_a_service, ...` |
| Rule | `rule must be get_permission_then_act, warn_wait_then_act, or act_then_report`; `warning_wait_minutes must be 1 to 1440 with warn_wait_then_act`; `warning_wait_minutes goes only with warn_wait_then_act`; `approvals_needed must be 1 or 2`; `approvals_needed goes only with get_permission_then_act` |
| Playbook name | `must be NAME of a playbook file playbooks/NAME.yml, starting with a letter or "_", then letters, digits, "_", or "-"; a one-time playbook in one_time_playbooks/ cannot be listed` |
| Also-approves list | `goes only with rule get_permission_then_act: ...`; `names replace_failed_servers itself; an approval approves its own playbook already`; `every_server is named twice; each named playbook is approved once`; `every_server is not listed under rules_for_running_agent_playbooks.servers.playbooks; list it there with its own rule, so the approval covers only a playbook a person chose by name` |

Set up in [Decide what the triage agent may do](../how-to/decide-what-the-triage-agent-may-do.md);
listed playbooks in
[Give one playbook its own rule](../how-to/decide-what-the-triage-agent-may-do.md#give-one-playbook-its-own-rule).

### services_whose_changes_a_person_must_approve

The systemd units that no playbook may start, stop, restart, or change
without a person, on every inventory.

```yaml
services_whose_changes_a_person_must_approve: [pgautofailover.service]
```

| Fact | Value |
| --- | --- |
| Required | Yes; may be `[]` |
| Type | A list of systemd unit names. A name without a unit type means a `.service`, so `pgautofailover` is `pgautofailover.service`. |
| Default | None |
| Wrong value | `"..." is not a systemd unit name: a unit name has only letters, digits, and the characters _ . @ : \ -, and starts with a letter, a digit, or _`; `systemd allows at most N characters in a unit name, ...`; `pgautofailover.service is listed twice; a name without a unit type means a .service, so pgautofailover is pgautofailover.service` |
| Set up in | [Decide what the triage agent may do](../how-to/decide-what-the-triage-agent-may-do.md) |

## Sending services

### sending_services

The accounts MMHotline sends notifications through. Each service is an
optional section inside `sending_services`, and each applies as its own
part after the service's own check passes; a failed check keeps the
earlier setup.

```yaml
sending_services: {}
```

| Fact | Value |
| --- | --- |
| Required | Yes; may be `{}` |
| Sections | `pushover`, `twilio`, `telegram`, `slack`, `mail_server`, each optional |
| Set up in | [Connect a sending service](../how-to/connect-a-sending-service.md) |

- A service left out while contact methods use it is refused, and the
  setup stays, such as:
  `cannot remove the Pushover setup: 1 Pushover notification contact method uses the sending service setup. Remove those contact methods on the Contact methods page, or with mmhotline contact-methods remove --contact-method-id ID, first`.
  A different Telegram bot or Slack workspace is also refused while
  contact methods use the existing one, and a new Pushover application
  token is refused while a Pushover emergency message is open.
- When a service is newly set up, `settings apply` prints its next step
  under its part, such as
  `sending_services.pushover: Next step: Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification.`

### sending_services.pushover

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `application_token` | Yes, secret | The API token of the team's Pushover application: 30 letters and digits | `must be one line of exactly 30 letters and digits, as Pushover shows the application's API token` |

Set up in [Connect Pushover](../how-to/connect-pushover.md).

### sending_services.twilio

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `account_sid` | Yes | `AC` and 32 hexadecimal digits | `must be AC followed by 32 hexadecimal digits` |
| `api_key_sid` | Yes | A dedicated API key: `SK` and 32 hexadecimal digits | `must be SK followed by 32 hexadecimal digits` |
| `api_key_secret` | Yes, secret | The API key's secret: one line, at most 4,096 bytes | `required`; `must be at most 4,096 bytes`; `must be one line` |
| `phone_number_to_call_from` | Yes | The Twilio voice number, in E.164 form: `+`, the country code, and the number, without spaces | `must be an E.164 phone number, such as +12025550102` |
| `extra_phone_numbers_on_the_contact_card` | Yes; may be `[]` | More E.164 numbers for the contact card that people save | `must be an E.164 phone number, such as +12025550102` |

Set up in [Connect Twilio](../how-to/connect-twilio.md).

### sending_services.telegram

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `bot_token` | Yes, secret | The token from BotFather, Telegram's bot-management chat: digits, a colon, then letters, digits, `_` and `-` | `must be the token from BotFather: digits, a colon, and letters, digits, _ and -` |

Set up in [Connect Telegram](../how-to/connect-telegram.md).

### sending_services.slack

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `bot_token` | Yes, secret | The Bot User OAuth Token, which starts with `xoxb-` | `must be the Bot User OAuth Token, which starts with xoxb-` |
| `app_level_token` | Yes, secret | The app-level token with `connections:write`, which starts with `xapp-` | `must be the app-level token, which starts with xapp-` |

Set up in [Connect Slack](../how-to/connect-slack.md).

### sending_services.mail_server

| Key | Required | Type and allowed values |
| --- | --- | --- |
| `smtp_server` | Yes | `host:port`, such as `smtp.example.com:587` |
| `smtp_connection_security` | Yes | `starttls`, `implicit_tls`, or `unencrypted` |
| `email_from_address` | Yes | One plain ASCII email address, such as `alerts@example.com` |
| `smtp_user_name` | Optional | One line; set together with `smtp_password`, or neither |
| `smtp_password` | Optional, secret | One line; set together with `smtp_user_name`, or neither |

`settings check` reports a wrong value under `sending_services.mail_server`:
`smtp_server must be a host name and port, such as smtp.example.com:587`;
`smtp_connection_security must be starttls, implicit_tls, or unencrypted`;
`email_from_address must be one plain ASCII email address, such as alerts@example.com`;
`smtp_user_name and smtp_password are required together, or both must be empty`;
`SMTP credentials must be one line of UTF-8 text without control characters`.

Set up in [Connect a mail server](../how-to/connect-a-mail-server.md).

## Cloudflare features

### mmhotline_down_alarm

The MMHotline-down alarm: a Cloudflare Worker in the team's own Cloudflare
account that wakes the person on call when MMHotline stops checking in.

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `mmhotline_down_alarm` | Optional | Section. Leaving it out tells the Worker to hold no notifications, then removes the setup. | |
| `cloudflare_account_id` | Yes | 32 lowercase hexadecimal characters | `the Cloudflare account ID must be 32 lowercase hexadecimal characters, as the dashboard shows it` |
| `cloudflare_api_token` | Yes, secret | An account API token with one permission, Account › Workers Scripts › Edit: 1 to 1,024 bytes, one line, no white space. MMHotline keeps it to upload the new Worker after an upgrade. | `the Cloudflare API token must be one line of text` |
| `wake_the_person_on_call_after_minutes_without_a_check_in` | Yes | 3 to 60; 5 is a good start | `must be a whole number of minutes from 3 to 60: MMHotline checks in every minute and at once when it starts, so a shorter wait would wake someone for a server reboot, and a longer one would leave a stopped MMHotline unnoticed for over an hour` |
| `wake_the_backup_after_minutes_without_a_check_in` | Yes | At least 5 more than the person on call's wait; 10 is a good start | `must be a whole number of minutes, at least 5 more than wake_the_person_on_call_after_minutes_without_a_check_in (10), so the person on call has time to answer before the backup is woken` |

A server reboot leaves at most about 2.5 minutes without a check-in, so a wait under 3 minutes would wake someone for a reboot.

The two wake times apply as their own part, which takes effect within
seconds. The account and token apply as the `mmhotline_down_alarm` part,
after MMHotline uploads the Worker and checks in.

Set up in [Set up the MMHotline-down alarm](../how-to/set-up-the-mmhotline-down-alarm.md).

### status_page

The public status page, which MMHotline publishes to a Cloudflare R2
bucket.

| Key | Required | Type and allowed values | Wrong value |
| --- | --- | --- | --- |
| `status_page` | Optional | Section. Leaving it out removes the setup from MMHotline; the bucket and the page stay in Cloudflare. | |
| `page_title` | Yes | The page's title and heading, shown exactly as written: 1 to 100 characters on one line, such as `Example Inc. status` | `the page title must have 1 to 100 characters on one line` |
| `host_name` | Yes | The page's host name, such as `status.example.com` | `the host name must be a DNS name such as status.example.com` |
| `r2_bucket_url` | Yes | The bucket's S3 API address, as the laptop's `status-page set-up` printed it | `the R2 bucket URL must be the bucket's S3 API address, such as https://ACCOUNT_ID.r2.cloudflarestorage.com/BUCKET_NAME` (or `... must end with the bucket's name, ...`) |
| `r2_access_key_id` | Yes, secret | The Access Key ID of an R2 API token with Object Read & Write on that bucket only: letters and digits | `must be letters and digits, as Cloudflare shows the R2 Access Key ID` |
| `r2_secret_access_key` | Yes, secret | That token's Secret Access Key: one line, at most 4,096 bytes | `required`; `must be at most 4,096 bytes`; `must be one line` |
| `website_url` | Optional | A **Website** link in the page's header: an `https://` address with a host name and no user name or password, at most 500 characters, such as `https://example.com`. Left out, the page has no Website link. | `the website URL must be an https:// address with a host name and no user name or password, such as https://example.com` |
| `support_url` | Optional | A **Support** link in the page's header: an `https://` address, or `mailto:` and one email address with no name or query, at most 500 characters, such as `mailto:support@example.com`. Left out, the page has no Support link. | `the support URL must be an https:// address or mailto: and one email address, such as https://example.com/support or mailto:support@example.com` |
| `components` | Optional | A list of at most 50 parts of your service that a notice can say it affects, such as `[{component_id: website, name: Website}, {component_id: api, name: API, description: "Orders and sign-in"}]`. The page lists each with its status. Left out, the page has no components panel, and a notice names none. | `at most 50 components are allowed, ...` |
| `components[].component_id` | Yes | 1 to 64 lowercase letters, digits, hyphens, and underscores, starting with a letter or digit. Unique. The command's `--component` flag takes it. | `components[N].component_id must have 1 to 64 lowercase letters, digits, hyphens, and underscores, starting with a letter or digit`; `components[N].component_id "api" is used twice` |
| `components[].name` | Yes | The name the page shows: 1 to 100 characters on one line | `components[N].name must have 1 to 100 characters on one line` |
| `components[].description` | Optional | One line under the name on the page, at most 200 characters | `components[N].description must have at most 200 characters on one line` |

```yaml
status_page:
  page_title: Example Inc. status
  host_name: status.example.com
  r2_bucket_url: https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11
  r2_access_key_id: "YOUR_ACCESS_KEY_ID"
  r2_secret_access_key: "YOUR_SECRET_ACCESS_KEY"
  website_url: https://example.com
  support_url: mailto:support@example.com
  components:
    - component_id: website
      name: Website
      description: Main site and customer dashboard
    - component_id: api
      name: API
```

The part applies after MMHotline publishes the current page with the
file's R2 key. The apply writes `page_title`, `host_name`, the links, and
the components into `status.json`, and uploads `index.html` and `feed.xml`
built from it. The notices already on the page stay, even those that name a
component you removed from the list: the page then shows that component's
ID where it would show its name.

Set up in [Publish a status page](../how-to/publish-a-status-page.md).
