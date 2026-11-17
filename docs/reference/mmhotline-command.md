# The mmhotline command

This page lists every `mmhotline` command: its flags, its exit codes, and
the fields of its `--json` result. Most commands run on the MMHotline
server, as root, and talk to the running service through its **command
socket**, a private Unix socket that only root can reach. A few run
anywhere, such as `settings check` on a laptop. Each entry links to the
how-to page that uses the command step by step;
[Change the settings](../how-to/change-the-settings.md) is the one most
people start with.

## Common flags, results, and exit codes

Every command of these groups takes the common flags: `people`,
`sending-services`, `triage-agent`, `review-agent`, `agent-playbook-rules`,
`contact-methods`, `alert-sources`, `schedules`, `status-page`,
`down-alarm`, `incidents`, `alerts`, `triage-sessions`,
`approval-requests`, and `commit-reviews`. `settings apply` and
`settings show` take them too. The exceptions are named in their entries:
`status-page set-up` and `triage-agent check-preparation` take only
`--json` and `--help`, and the commands
that systemd, SSH, or the triage agent run take none.

| Flag | Meaning |
| --- | --- |
| `--command-socket PATH` | The command socket. Default `/run/mmhotline/command.sock` |
| `--json` | Print one JSON object instead of sentences |
| `--help` | Show the command's flags. It needs no running service |

- `mmhotline --help` lists every command group. `mmhotline GROUP --help`
  lists the actions of one group, such as `mmhotline schedules --help`.
- Flags come after the action: `mmhotline schedules show --json`.
- Never put a secret in an argument: other users on the server can read
  a program's arguments. The flags that take a secret read it from a file
  or from standard input instead. On a terminal, standard input shows
  nothing as you paste, and one line ending with Enter is the whole
  secret; a pipe is read to its end.
- Use `sudo` for every command that uses the command socket.

**Who the command acts as.** A command run through `sudo` acts for the
whole team, also on another person's personal contact methods. Its
actions are recorded as "the mmhotline command, run by barbara" when
`SUDO_USER` is `barbara`, or as `root` without `sudo`. They carry no team
member ID. Changes from the settings file are recorded as "the settings
file /etc/mmhotline/mmhotline_settings.yml". On the pages, only a personal
contact method's owner edits that method.

**Results.** Every successful command returns `message`, one sentence
about the result. Every group except `settings`, `alert-sources`,
`down-alarm`, and `status-page` also returns `web_url`, the settings
file's `web_url`. Optional fields and empty lists may be absent unless an
entry says "present also when empty". Times are RFC 3339 UTC strings, such
as `2026-11-15T08:51:06Z`. Successful JSON goes to standard output.

**Text output** replaces every control character except the line feed and
the tab with the replacement character `�` (U+FFFD). Reason: alert
titles, alert messages, and the triage agent's text come from other people
and programs, and a terminal acts on escape sequences in them, which can
rewrite earlier lines, set the window title, or write to the clipboard.
`--json` keeps the original characters, escaped, such as `\u001b`.

**Exit codes**, unless an entry says otherwise:

| Exit code | Meaning |
| --- | --- |
| `0` | Success, or `--help` |
| `1` | An installation, service, or setup failure |
| `4` | MMHotline refused the command, the arguments are wrong, or the command socket does not answer |
| `130` | Interrupted, such as with Ctrl-C |

A refused command prints MMHotline's reason on standard error, also with
`--json`. A missing command socket says `mmhotline serve is not running at
command socket /run/mmhotline/command.sock`.

## mmhotline install

Installs MMHotline on the Linux server where it runs, from the settings
file `/etc/mmhotline/mmhotline_settings.yml`.

```sh
sudo mmhotline install
```

[Install MMHotline](../how-to/install.md) runs it step by step and shows
its output; [Upgrade MMHotline](../how-to/upgrade.md) runs it again with a
new program.

- **Flags:** only `--help`, which needs no root and checks and changes
  nothing.
- **Needs:** Linux, root, running systemd, and the settings file.
- **Refuses, and changes nothing,** until the settings file exists and
  passes the owner and mode check and the same checks as
  [`settings check`](#mmhotline-settings-check). The refusal names the
  settings file's path and lists each problem with its key path.
- **What it does:** creates the `mmhotline` Unix user, installs the
  program, and creates the database with the file's `team_name` when the
  database is missing. It sets the settings file's owner to `root`, group
  `mmhotline`, and mode `0640`, so MMHotline can read the file and only
  root can change it. It starts the service, the triage agent's run
  socket, and the fleet view socket.
  [Installed files](installed-files.md) lists every file it writes.
- **What it leaves alone:** the settings file's contents, which the team
  writes; an existing database and its team name (a changed `team_name`
  applies with [`settings apply`](#mmhotline-settings-apply)); and
  `cloudflared`, Cloudflare Access, and the private network.
- **The backup unit:** install creates `mmhotline-backup.service` only
  when `/etc/mmhotline/backup.env` and the `litestream` program exist. See
  [Back up and restore the database](../how-to/back-up-and-restore-the-database.md).
- **Restarts the service** when the program or a generated unit changed,
  the database is new, the settings file's owner or mode was corrected, or
  the service was stopped. An unchanged rerun changes nothing.
- **Output:** one line for each step, then `MMHotline:` and the web
  address. When `web_url` is an `https://` address and the settings file
  has no `cloudflare_access`, it prints the next step:
  [Put MMHotline behind Cloudflare Access](../how-to/put-mmhotline-behind-cloudflare-access.md).
- **To upgrade:** run `sudo ./mmhotline install` from the folder that holds
  the new program.

## mmhotline version

Prints the program's version, the Git revision it was built from, and its
system.

```sh
mmhotline version
```

```text
mmhotline 0.1.0 (COMMIT, linux/amd64, modified=false)
```

- `COMMIT` stands for the full hash of the commit the program was built
  from, and `modified=true` means the checkout had uncommitted changes.
- **Flags:** only `--json`. Any other argument prints the usage and exits
  4.
- Needs no `sudo` and no running service, so another program, such as a
  check that compares installed versions with the latest releases, can
  run it.
- **JSON fields:** `version`, `revision`, `modified`, `go`, `os`, `arch`.

Used in [Install MMHotline](../how-to/install.md),
[Upgrade MMHotline](../how-to/upgrade.md), and
[Post to the status page](../how-to/post-to-the-status-page.md).

## mmhotline backup check-uploads

Checks whether MMHotline's continuous backup keeps up: whether Litestream
captured every change of the database, and whether the backup in object
storage holds every change Litestream captured.

```sh
sudo mmhotline backup check-uploads
```

```text
MMHotline's backup holds every change Litestream captured, up to transaction 0000000000000002.
```

[Back up and restore the database](../how-to/back-up-and-restore-the-database.md#check-the-backup)
explains when to run it and what to do with each answer.

- **Flags:** `--json` and `--help` only.
- **Runs as root** on the MMHotline server. It never connects to
  `mmhotline serve` and changes nothing.
- **Reads** `/etc/mmhotline/backup.env`, the database's write-ahead log
  `/var/lib/mmhotline/mmhotline.db-wal`, and the changes Litestream keeps
  in `/var/lib/mmhotline-backup/mmhotline.db-litestream/ltx/0/` until they
  are uploaded. It lists the backup with `litestream ltx`, found on `PATH`
  and started with only the variables of `backup.env` and `PATH`.
- **Waits** up to 30 seconds for Litestream to capture a transaction the
  log holds.
- **Prints** one line, or one JSON object with `--json`, whatever the
  answer.

**Exit codes** (this command is a check, so its codes differ):

| Exit code | Line |
| --- | --- |
| `0` | `MMHotline's backup holds every change Litestream captured, up to transaction 00000000000004d2.` |
| `0` | `MMHotline's backup is 30 seconds behind, within 5 minutes: the newest in the backup is transaction 00000000000004d1; Litestream has captured up to 00000000000004d2.` |
| `1` | `MMHotline's backup has stopped: the database has changes that Litestream has not captured after waiting 30 seconds; Litestream has captured up to transaction 0000000000000004. Read why with: sudo journalctl -u mmhotline-backup` |
| `1` | `MMHotline's backup is missing changes captured 12 minutes ago: the newest in the backup is transaction 00000000000004d1; Litestream has captured up to 00000000000004d2. Read why with: sudo journalctl -u mmhotline-backup` |
| `3` | `MMHotline's backup is not set up: /etc/mmhotline/backup.env is missing. To set it up, follow` and the guide's address |
| `3` | `Litestream has captured no change of the database, so the backup has not started;` and how to check and start it |
| `3` | `Cannot list MMHotline's backup:` and Litestream's last error line, with each `backup.env` value of 8 characters or more replaced by its variable's name, such as `$LITESTREAM_SECRET_ACCESS_KEY` |
| `4` | The arguments are wrong |

It also exits 3 when Litestream is missing, when `backup.env` cannot be
read (run the check with `sudo`) or sets no
`MMHOTLINE_BACKUP_REPLICA_URL`, when the newest captured change has no LTX
header, and when the list has no `max_txid` column.

**JSON fields.** A transaction is Litestream's transaction ID, as
Litestream writes it: 16 hexadecimal digits.

| Field | Meaning |
| --- | --- |
| `message` | The line from the table above |
| `uploads_keep_up` | `true` for exit 0, `false` for exit 1, `null` when the check cannot tell |
| `newest_captured_transaction` | The newest transaction Litestream captured. Absent before the first, and when the check cannot tell |
| `newest_uploaded_transaction` | The newest transaction in the backup. Absent when the backup holds none, when Litestream did not capture the database's changes (the check then lists nothing), and when the check cannot tell. It can be newer than `newest_captured_transaction`, because Litestream may capture and upload between the check's local read and its listing of the backup |
| `oldest_missing_change_captured_at` | When Litestream captured the oldest change the backup lacks. Absent when the backup lacks none |

## mmhotline setup

Creates MMHotline's database, `mmhotline.db`, with the settings file's
`team_name` and no people. `sudo mmhotline install` runs it for you; run it
by hand only on a development machine.

```sh
sudo mmhotline setup --state-dir /var/lib/mmhotline \
  --settings-file /etc/mmhotline/mmhotline_settings.yml
```

```text
Created MMHotline state in /var/lib/mmhotline.
```

| Flag | Meaning |
| --- | --- |
| `--state-dir DIR` | Required. The folder for the database |
| `--settings-file FILE` | Required. The settings file. Setup checks it, its owner and mode included |
| `--help` | Show the flags |

- Refuses when the folder already holds MMHotline's state, and when the
  settings file has problems, which it lists.
- Prints sentences, never JSON.
- Exit codes: 4 for a missing flag; 1 for a refused setup.
- [Local development](../changing-mmhotline/local-development.md#build-and-start)
  runs setup and serve on a development machine.

## mmhotline serve

Runs MMHotline: the pages, the alert intake, and the command socket.
systemd runs it as `mmhotline.service`; a person runs it only on a
development machine.

```sh
sudo mmhotline serve --state-dir /var/lib/mmhotline \
  --settings-file /etc/mmhotline/mmhotline_settings.yml \
  --command-socket /run/mmhotline/command.sock
```

| Flag | Meaning |
| --- | --- |
| `--state-dir DIR` | Required. The folder that holds the database |
| `--settings-file FILE` | Required. The installed service passes `/etc/mmhotline/mmhotline_settings.yml` |
| `--command-socket PATH` | Required. The command socket, which checks that the caller is root |
| `--triage-agent-run-socket PATH`, `--review-agent-run-socket PATH`, `--triage-agent-proxy-socket PATH` | Give all three or none. Without them, triage agent and review agent runs are unavailable. The installed service gives all three |
| `--fleet-view-socket PATH` | The socket where systemd runs the settings file's `fleet_view_command` for the dashboard's servers. The installed service gives `/run/mmhotline-fleet-view.sock`. Without it, the dashboard shows no servers |
| `--help` | Show the flags |

- **Listens on** `web_listen_address` for the pages, over plain HTTP, such
  as `127.0.0.1:8480`, and on `alert_intake_listen_address` for signed
  alerts and check-ins, such as `10.20.0.1:8484`.
- **Prints its sign-in methods** on standard error at start:
  `Sign-in: Cloudflare Access for example.cloudflareaccess.com, and sign-in links`,
  or `Sign-in: sign-in links only; the settings file has no cloudflare_access`.
  With `cloudflare_access`, serve checks the token Cloudflare Access adds
  to each page request.
- **Reads the settings file at start** and checks its owner, mode, and
  values. `web_url`, `web_listen_address`, `alert_intake_listen_address`,
  and `cloudflare_access` change only at a start. Parts without an outside
  test apply before serve tells systemd it is ready; parts with one apply
  in the background, as [`settings apply`](#mmhotline-settings-apply)
  would.
- **A settings file with problems:** serve starts with the settings it
  last applied and sends the administrators a warn alert that names each
  problem by key path. On the very first start, with nothing applied yet,
  serve refuses to start, exits 1, and prints
  `the settings file FILE has problems, and MMHotline has never applied a settings file, so it cannot start:`
  and each problem.
- **A listen address serve cannot open,** such as one the server does not
  have: serve falls back to the addresses it last applied, and warns.
  The [settings file design](../changing-mmhotline/design/settings-file.md#start)
  gives the reasons.
- **Exit codes:** 4 for a missing `--state-dir`, `--settings-file`, or
  `--command-socket`, a socket flag that needs the other two, or an
  unexpected argument.
- **Development switch:** the environment variable
  `MMHOTLINE_FAKE_SENDING_SERVICES_URL` points chat, push, and phone
  sending services at the
  [fake sending services](../changing-mmhotline/local-development.md#develop-without-real-sending-services).

## mmhotline settings check

Checks a settings file's shape, keys, and values, without a network or a
running MMHotline. It runs anywhere, as any user, such as on a laptop in a
deploy tool's dry run.

```sh
mmhotline settings check --settings-file mmhotline_settings.yml
```

```text
The settings file /etc/mmhotline/mmhotline_settings.yml has 1 problem:
sending_services.pushover.application_token: must be one line of exactly 30 letters and digits, as Pushover shows the application's API token
```

[Change the settings](../how-to/change-the-settings.md) uses it before
each apply.

| Flag | Meaning |
| --- | --- |
| `--settings-file FILE` | Required. The file to check |
| `--json` | Print one JSON object |
| `--help` | Show the flags |

- **Prints** each problem with its key path and what is wrong, never a
  secret's value. Other problem texts look like
  `on_call_schedule.escalate_if_unclaimed_after_minutes: must be a whole number of minutes from 1 to 60`
  and
  `alert_sources[0].signing_secrets[0]: must be 64 hexadecimal characters, such as the output of openssl rand -hex 32`.
  A problem of the whole file has no key path, such as
  `the settings file is not valid YAML: line 1: did not find expected node content`.
- **A file without problems** prints
  `The settings file mmhotline_settings.yml has no problems.`
- **Notes** follow the problems: something the file leaves out, which
  MMHotline fills with its safest behavior, but which a person should see.
  A note never fails the check, so a dry run shows it and continues. For
  example, a kind of action that an inventory's `action_kinds` leaves out:

  ```text
  The settings file mmhotline_settings.yml has no problems.
  Notes, which do not stop MMHotline from applying the settings file:
  rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one
  ```

- **What it checks:** every rule of `settings apply` that needs no saved
  state: keys, value types, and each value; at least one administrator in
  `people`; and every sign-in email address in `on_call_schedule` listed in
  `people`.
- **What it skips:** the file's owner and mode, because a dry run writes
  the file on a laptop without root (`settings apply`, serve's start, and
  `install` check them); custom schedule code; and every outside service.
  `settings apply` runs those.

| Exit code | Meaning |
| --- | --- |
| `0` | No problems, with or without notes |
| `1` | The file has problems |
| `4` | `--settings-file` is missing, or another argument is wrong |

**JSON fields:**

| Field | Meaning |
| --- | --- |
| `settings_file` | The path given with `--settings-file` |
| `problems` | Each problem: `key_path` and `problem`. Present also when empty |
| `notes` | Each note: `key_path` and `note`. Present also when empty |

## mmhotline settings apply

Tells the running MMHotline to read its settings file, check it, and apply
each part that changed.

```sh
sudo mmhotline settings apply
```

```text
people: did not apply: Dana Example owns 1 claimed incident(s). Resolve the claimed incidents or have someone take over the claimed incidents before removing the person. The earlier setting, applied at 2026-11-15T06:17:42Z, stays.
on_call_schedule: applied.
1 part did not apply and keeps its earlier setting: people.
```

[Change the settings](../how-to/change-the-settings.md) walks through an
apply and each thing that can go differently.

| Flag | Meaning |
| --- | --- |
| `--command-socket PATH` | The command socket. Default `/run/mmhotline/command.sock` |
| `--json` | Print one JSON object; the fields are under [Settings results](#settings-results) |
| `--help` | Show the flags |

- **Which file:** the file at the path `serve` was given, normally
  `/etc/mmhotline/mmhotline_settings.yml`. MMHotline checks its owner and
  mode, then its values.
- **A file with problems applies nothing.** The command prints each
  problem, as `settings check` does, then
  `The settings file has problems, so nothing applied and the earlier settings stay. Fix the settings file problems, then apply the settings file again.`
- **A file someone other than root could change, or every user could
  read,** is refused with the command that fixes it:

  ```text
  /etc/mmhotline/mmhotline_settings.yml can be read by every user on the server (mode 0644), and it holds the team's secrets. Fix the settings file owner and mode with: sudo chown root:mmhotline /etc/mmhotline/mmhotline_settings.yml; sudo chmod 0640 /etc/mmhotline/mmhotline_settings.yml
  ```

- **Parts.** MMHotline checks each part against what it saved and writes
  each part on its own. A part that fails keeps its earlier setting while
  the other parts apply. A part whose value is already in effect is
  `unchanged`, and is not written again, unless an attempt to apply it
  failed after its last success.
- **One apply at a time.** A second apply waits for the first. The
  command waits without a time limit, because an outside test can take
  minutes. Ctrl-C stops the wait, not the apply.

**The output** starts with `Settings file:` and the path, and
`Settings file first comment line:` when the file starts with a comment.
Then comes **one line for each part**, in the order they apply, people
before the schedule that names them:

1. `web_url, web_listen_address, alert_intake_listen_address, cloudflare_access`
2. `team_name`
3. `fleet_view_command`
4. `people`
5. `on_call_schedule`
6. `rules_for_running_agent_playbooks`
7. `services_whose_changes_a_person_must_approve`
8. `alert_sources`
9. `triage_agent.commit_signing_public_key, triage_agent.trusted_commit_signing_public_keys`
10. `triage_agent.commits_without_a_trusted_signature_are_approved_by, triage_agent.paths_only_a_person_may_approve`
11. `triage_agent.coding_agent, triage_agent.credential_type, triage_agent.credential, triage_agent.credential_expires_on, triage_agent.model, triage_agent.reasoning_effort`
12. `triage_agent.internet_access`
13. `triage_agent.listed_hosts`
14. `triage_agent.sos_waits_for_the_agent_minutes`
15. `triage_agent.warn_waits_for_the_agent_minutes`
16. `triage_agent.sos_waits_for_a_server_replacement_minutes`
17. `triage_agent.software_review_interval_hours`
18. `mmhotline_down_alarm.wake_the_person_on_call_after_minutes_without_a_check_in, mmhotline_down_alarm.wake_the_backup_after_minutes_without_a_check_in`
19. `review_agent`
20. `sending_services.pushover`
21. `sending_services.twilio`
22. `sending_services.telegram`
23. `sending_services.slack`
24. `sending_services.mail_server`
25. `mmhotline_down_alarm`
26. `status_page`

Each line ends in one of these:

- `applied.`
- `unchanged.`
- `not set up, because the settings file leaves it out.`
- `did not apply:`, the reason, then when the earlier setting applied:
  `The earlier setting, applied at 2026-11-15T03:54:04Z, stays.`, or
  `It stays not set up, as before.`, or
  `No value has ever applied for the settings file part, so the part is not set.`

The last line is `Every part of the settings file is applied.`, or
`1 part did not apply and keeps its earlier setting:` and the parts.

**Notes** come on the lines under a part, indented by two spaces. A note
never makes the command fail. They have three sources:

- **The static check's notes,** under the part their key path belongs to,
  also when the part is unchanged. MMHotline saves them from the last
  apply. Example:
  `rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one`.
- **The next step** after a sending service applies for the first time;
  the note starts with `Next step:`:

  ```text
  sending_services.mail_server: applied.
    sending_services.mail_server: Next step: Add your own email address under Contact methods. Saving the contact method sends a test message.
  ```

- **Hosts listed automatically,** under `triage_agent.listed_hosts`, only
  in [`settings show`](#mmhotline-settings-show).

Other notes the parts print:

- Under `triage_agent.software_review_interval_hours` while the review is
  off:
  `Scheduled software review: off. The triage agent wakes only for alerts; sudo mmhotline triage-agent run-software-review still runs one review, such as to learn what one costs.`
- Under `triage_agent.commits_without_a_trusted_signature_are_approved_by`
  with `a_person_or_the_triage_agent`:
  `The triage agent reviews each commit no trusted key signed and approves an ordinary change; a person reviews the triage agent's own commits outside its folders, and every commit that changes a path of triage_agent.paths_only_a_person_may_approve.`

**The parts with an outside test** are saved only when the test passes:

| Part | Outside test |
| --- | --- |
| `triage_agent.coding_agent, ...` (part 11) | One test prompt, run with the file's coding agent, credential, model, and reasoning effort. It must answer `MMHotline triage agent test passed`. The server ends the test after 5 minutes |
| `review_agent` | The same, with the review agent's setup, in a run like a review's. It must answer `MMHotline review agent test passed` |
| `sending_services.pushover` | Pushover's limits endpoint |
| `sending_services.twilio` | Twilio's check of the calling number in the account |
| `sending_services.telegram` | Telegram's `getMe`. MMHotline then starts polling: it keeps asking Telegram for updates |
| `sending_services.slack` | Slack's token checks. MMHotline then starts Socket Mode, a connection that receives Slack messages and button presses |
| `sending_services.mail_server` | A connection to the mail server that checks TLS and the sign-in, and sends nothing |
| `mmhotline_down_alarm` | Uploads the Worker, sets its check-in token, turns on its `workers.dev` address, saves the setup, and checks in |
| `status_page` | Posts the current page with the file's R2 key |

- A setup test that cannot start because another triage agent run is
  active fails the part with
  `a triage agent run was active, so the new agent setup was not tested. Apply the settings file again after the triage agent run ends`.
  A failed setup test names the reason and the coding agent's answer, when
  there is one.
- A section left out removes that setup. A sending service that contact
  methods still use is refused and keeps its setup:
  `sending_services.pushover: did not apply: cannot remove the Pushover setup: 2 Pushover notification contact methods use the sending service setup. Remove those contact methods on the Contact methods page, or with mmhotline contact-methods remove --contact-method-id ID, first. The earlier setting, applied at 2026-11-15T05:08:44Z, stays.`
  Leaving out `mmhotline_down_alarm` makes `settings apply` check in with
  nothing held, then remove the setup. An MMHotline-down alarm whose Worker
  cannot be told to hold nothing is refused the same way
  ([settings file design](../changing-mmhotline/design/settings-file.md#parts-with-an-outside-test)).

**The rules part is the only part that writes the rest of its value when
it fails.** When an inventory in MMHotline's copy of the systems
repository has no entry in `rules_for_running_agent_playbooks`, the line
is:

```text
rules_for_running_agent_playbooks: did not apply: inventory staging has no rules for running the agent's playbooks. Every fix in the inventory needs a person until you add its rules. The rules for running the agent's playbooks applied for the inventories listed in the settings file. Any inventory without rules requires a person to approve every fix until you add its rules.
```

**Restart.** `web_url`, `web_listen_address`,
`alert_intake_listen_address`, and `cloudflare_access` change only when
MMHotline restarts. When one of them changed, MMHotline applies nothing
and names the changed keys. The command then:

1. prints
   `Restarting MMHotline to apply the settings that require a restart: web_listen_address.`;
2. runs `systemctl restart mmhotline.service`;
3. waits up to one minute for the command socket to answer;
4. applies again, and the result says
   `Restarted MMHotline first, because these keys change only when it restarts: web_listen_address.`

When the socket does not answer within the minute, or MMHotline still
runs with the earlier values, such as when it could not open a new listen
address, the command exits 1 and says to read
`sudo journalctl -u mmhotline -n 50`. A restart drops no alert: senders
send an alert again until MMHotline answers it
([settings file design](../changing-mmhotline/design/settings-file.md#restarts)).

| Exit code | Meaning |
| --- | --- |
| `0` | Every part applied or was unchanged |
| `1` | The file has problems, a part did not apply, or the restart failed |
| `4` | The command socket does not answer, or the arguments are wrong |

## mmhotline settings show

Shows the settings file's path, its first comment line, when it last
applied, and each part as applied or not, with why.

```sh
sudo mmhotline settings show
```

```text
on_call_schedule: applied at 2026-11-15T03:58:24Z.
```

That is one line of the output, as
[Change the settings](../how-to/change-the-settings.md) shows it. The
whole output starts with these lines, then has one line for each part, in
the order of [`settings apply`](#mmhotline-settings-apply):

```text
Settings file: /etc/mmhotline/mmhotline_settings.yml
Settings file first comment line: Managed by MMX from mmhotline_settings.yml in the systems repository
Last applied: 2026-11-15T03:58:24Z
```

- **Flags:** the common flags.
- **Reads MMHotline's record,** not the settings file, so it shows what
  MMHotline uses even when the file has changed since.
- **`Last applied`** is when any part last applied. A part that never
  applied says `not applied yet.`
- **Notes:** each part's notes saved by the last apply, under the part.
  Under `triage_agent.listed_hosts`, `settings show` adds the vendor pages'
  hosts that the latest triage agent run listed from
  `software_outside_the_distribution.yml`, such as
  `triage_agent.listed_hosts: Also listed automatically from software_outside_the_distribution.yml, as of the latest run: github.com port 443`.
  Reason: a short `listed_hosts` in the file must not hide what the agent
  reaches. That note comes from MMHotline's run record, and never changes
  the part
  ([settings file design](../changing-mmhotline/design/settings-file.md#notes-on-a-parts-outcome)).
- **Commit signing keys:** `settings show` lists each applied commit
  signing key with its type, comment, and fingerprint. With the triage
  agent set up and no keys, it says
  `No commit signing keys are applied, so MMHotline refuses every MMX run and commit check.`
- The dashboard shows administrators the parts that failed.
- **Exit code:** `0` whatever the parts' states.

### Settings results

The `--json` result of `settings apply` and `settings show`. It has
`message` and `settings_file`, and no `web_url`.

| Field | Meaning |
| --- | --- |
| `settings_file.settings_file` | The settings file's path, as serve was given it |
| `settings_file.first_comment_line` | The file's first line without its `#`, when that line is a comment; empty otherwise |
| `settings_file.last_applied_at` | `show` only: the latest time any part applied. Absent before any part applied |
| `settings_file.problems` | `apply` only: the file's problems, each with `key_path` and `problem`. With any, nothing applied |
| `settings_file.restart_needed_for` | The restart keys that changed. The command restarts MMHotline, so its output has this field only when the restart did not take the new values |
| `settings_file.restarted_for` | The restart keys that changed, when the command restarted MMHotline before it applied the rest |
| `settings_file.parts` | Each part, in the order they apply |
| `settings_file.parts[].part` | The part's key paths, such as `people` or `triage_agent.listed_hosts` |
| `settings_file.parts[].state` | `applied`, `unchanged` (apply only), `failed`, or `never_applied` (show only) |
| `settings_file.parts[].applied_at` | When the setting in effect applied. Absent when none ever did |
| `settings_file.parts[].failure_reason`, `settings_file.parts[].failed_at` | Why and when the latest attempt failed; only with `failed` |
| `settings_file.parts[].left_out_of_the_settings_file` | `true` when the setting in effect is the section left out of the file, so the part is not set up; with `failed`, that is the setting that stays. The text then says `not set up, because the settings file leaves it out.` or `It stays not set up, as before.` in place of a time |
| `settings_file.parts[].notes` | The notes under the part, each with `key_path` and `note`. Absent when there are none |

## mmhotline people list

Lists every active person: the settings file's `people`, as it last
applied.

```sh
sudo mmhotline people list
```

```text
Barbara Example (barbara@example.com) is administrator.
Carlos Example (carlos@example.com) is member.
Listed 2 people. Open MMHotline at https://ops.example.com.
```

[Set up people and the on-call schedule](../how-to/set-up-people-and-the-on-call-schedule.md)
adds people with the settings file and checks them with this command.

- **Flags:** the common flags.
- Roles are `administrator` and `member`.

**JSON fields:**

| Field | Meaning |
| --- | --- |
| `people` | The active people |
| `people[].id` | The person's stable internal member ID. Commands take the sign-in email address instead |
| `people[].sign_in_email_address` | The sign-in email address, in lower case |
| `people[].name` | The person's display name |
| `people[].role` | `administrator` or `member` |

## mmhotline people create-sign-in-link

Creates a one-time sign-in link, so a person can open MMHotline without
Cloudflare Access: during a Cloudflare outage, or for a team that runs
without Cloudflare.

```sh
sudo mmhotline people create-sign-in-link --sign-in-email-address barbara@example.com
```

```text
Sign-in link: http://localhost:8480/sign-in/TOKEN
Sign-in link valid until (UTC): 2026-11-15T07:33:04Z
SSH forward command: ssh -L 8480:127.0.0.1:8480 YOUR_SSH_HOST
This sign-in link works once.
```

`TOKEN` stands for the random token in the real link.
[Sign in during a Cloudflare outage](../how-to/sign-in-during-a-cloudflare-outage.md)
walks through the whole procedure.

| Flag | Meaning |
| --- | --- |
| `--sign-in-email-address ADDRESS` | Required. An active team member's sign-in email address |
| Common flags | As above |

- **How the person uses it:** on their own computer, they run the SSH
  forward command with `YOUR_SSH_HOST` replaced by the host name they use
  for SSH to the server, then open the link there. The port and host in
  the command come from `web_listen_address`.
- **The link works once, for 10 minutes,** because it is printed in a
  terminal where someone could copy it. Opening it sets a sign-in cookie
  for 12 hours, long enough for a long incident, and opens the dashboard.
  A used or expired link shows how to create a new one.
- **Refused** for an address with no active member, such as
  `no member has sign-in email address nobody@example.com`, exit 4.
- **Safety:** MMHotline stores only a SHA-256 hash of each token, and the
  service log names the person but never the token. Removing the person
  from `people` ends their links and cookies at once, when the file
  applies. With `cloudflare_access` in the settings file, a page request
  that carries a Cloudflare Access token is judged by that token alone and
  ignores any sign-in cookie.
- **Sign-in email addresses:** MMHotline removes whitespace at both ends
  and stores the address in lower case. The stored address has 3 to 254
  characters, exactly one `@` with text on each side, and no spaces or
  control characters. With Cloudflare Access, it must be the address
  Cloudflare Access returns for the person.

**JSON fields:** the fields of [`people list`](#mmhotline-people-list),
and:

| Field | Meaning |
| --- | --- |
| `sign_in_link_url` | The one-time link, `http://localhost:PORT/sign-in/TOKEN` |
| `sign_in_link_valid_until` | The link's expiry, 10 minutes after it was made |
| `ssh_forward_command` | The SSH forward to run on the person's computer |

## mmhotline sending-services list

Lists every sending service, set up or not, with its saved settings and
never a secret. A **sending service** is the mail server or outside
service MMHotline sends notifications through.

```sh
sudo mmhotline sending-services list
```

```text
Pushover: set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T05:08:50Z. Checked with Pushover: monthly message limit 10000.
```

That is one line of the output.
[Connect a sending service](../how-to/connect-a-sending-service.md) sets
each one up in the settings file's `sending_services`, which
[`settings apply`](#mmhotline-settings-apply) tests and saves.

- **Flags:** the common flags.
- To test a sending service, add a contact method that uses it, or run
  [`contact-methods send-test`](#mmhotline-contact-methods-send-test) for
  one that exists.
- The Sending services page shows administrators the same setups, without
  edits.
- The saved tokens, the Twilio API key secret, and the mail server
  password are never returned.

**JSON fields:**

| Field | Meaning |
| --- | --- |
| `sending_service_setups` | One record for each sending service; no secret |
| `sending_service_setups[].sending_service` | `mail_server`, `telegram`, `slack`, `twilio`, or `pushover` |
| `sending_service_setups[].sending_service_title` | The name the pages show: `Mail server`, `Telegram`, `Slack`, `Twilio`, or `Pushover` |
| `sending_service_setups[].is_set_up` | Whether the sending service is set up |
| `sending_service_setups[].settings` | The settings that are not secret; empty before setup |
| `sending_service_setups[].settings.smtp_server` | The mail server's host and port |
| `sending_service_setups[].settings.smtp_connection_security` | `starttls`, `implicit_tls`, or `unencrypted` |
| `sending_service_setups[].settings.smtp_user_name` | The mail server user name; empty without sign-in |
| `sending_service_setups[].settings.email_from_address` | The sender address, without a display name |
| `sending_service_setups[].settings.telegram_bot_username` | The bot's username from Telegram, without `@`; present after setup |
| `sending_service_setups[].settings.telegram_bot_id` | The bot's numeric ID from Telegram; present after setup |
| `sending_service_setups[].settings.slack_workspace_id`, `.slack_workspace_name`, `.slack_bot_user_id`, `.slack_app_id` | The workspace, bot user, and app, as Slack confirmed them |
| `sending_service_setups[].settings.twilio_account_sid` | The account that owns the calling number |
| `sending_service_setups[].settings.twilio_api_key_sid` | The API key used for calls and polling |
| `sending_service_setups[].settings.twilio_phone_number_to_call_from` | The calling number, in E.164 form: `+`, country code, and number |
| `sending_service_setups[].settings.extra_phone_numbers_on_the_contact_card` | Comma-separated E.164 numbers, or an empty string |
| `sending_service_setups[].settings.pushover_monthly_message_limit` | The monthly message limit from Pushover; present after setup |
| `sending_service_setups[].set_up_at` | When it was set up; absent before setup |
| `sending_service_setups[].set_up_by` | `the settings file` and its path; absent before setup |

## mmhotline triage-agent show

Shows the triage agent's setup, whether its sign-in works, its internet
access and listed hosts, its latest setup test, its software reviews, and
its recent connections. The **triage agent** is the team's Claude Code or
Codex, run as a separate Unix user in a systemd sandbox on the MMHotline
server.

```sh
sudo mmhotline triage-agent show
```

```text
Triage agent sandbox installed: true.
Triage agent: Claude Code with a subscription token, model opus, reasoning effort high (coding_agent claude_code, credential_type subscription_token, model opus, reasoning_effort high), set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:51:06Z.
Sign-in to Claude Code: works; last checked at 2026-11-15T08:51:06Z.
Subscription token expires at: 2027-11-15T08:51:06Z
Internet access: listed_hosts_only
Listed host: api.anthropic.com:443; listed because: Claude Code's model API.
Listed host: api.osv.dev:443; listed because: OSV.dev's API.
Listed host: raw.githubusercontent.com:443; listed because: CISA's list of known exploited vulnerabilities, from CISA's copy on GitHub.
Setup test bbd54461c72208a751219d09b0050026: passed; run state: finished; purpose: setup_test; coding agent: claude_code; credential type: subscription_token.
...
Scheduled software review: off; set by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:51:06Z.
```

[Set up the triage agent](../how-to/set-up-the-triage-agent.md) sets it up
and reads this output;
[Keep the agents signed in](../how-to/keep-the-agents-signed-in.md) reads
its sign-in lines.

- **Flags:** the common flags.
- Only administrators use the Triage agent page; the command calls the
  same administrator-only operations with root's command access.
- The setup comes from the settings file's `triage_agent` section
  ([settings file](settings-file.md#triage_agent)); no command changes it.

**Credentials.** The setup's `coding_agent` and `credential_type`:

| `coding_agent` | `credential_type` | The credential |
| --- | --- | --- |
| `claude_code` | `api_key` | An Anthropic API key |
| `claude_code` | `subscription_token` | A token from `claude setup-token`. `show` prints `Subscription token expires at:`, one year after the credential applied |
| `codex` | `api_key` | An OpenAI API key |
| `codex` | `access_token` | A Codex access token from a ChatGPT Business or Enterprise workspace. `show` prints `Codex access token expires on:` with the settings file's `credential_expires_on` |
| `codex` | `chatgpt_sign_in` | The whole content of `auth.json` after `codex login`, made just for MMHotline |

Administrators get a reminder 30 days and 7 days before a subscription
token or Codex access token expires
([design](../changing-mmhotline/design/triage-agent.md#expiry-reminders)).
No credential is ever returned.

**Listed hosts.** MMHotline lists these hosts itself:

- the coding agent's service hosts, OSV.dev's API, and
  `raw.githubusercontent.com`, for CISA's list of known exploited
  vulnerabilities
  ([design](../changing-mmhotline/design/triage-agent.md#known-exploited-flaws));
- at the start of each run in the systems repository, each server in the
  inventories of MMHotline's copy, by name at port 22, so the agent can
  read the servers over SSH
  ([design](../changing-mmhotline/design/triage-agent.md#reading-the-servers-over-ssh));
- at the same time, the host of each vendor page in
  `software_outside_the_distribution.yml`, from each program's
  `releases_page` and `security_advisories_page`, at port 443 or the port
  the address names
  ([design](../changing-mmhotline/design/triage-agent.md#the-vendor-pages-hosts)).

Only runs in the systems repository reach the inventory servers and the
vendor pages' hosts; setup tests, sign-in checks, and review agent runs do
not. The settings file's `triage_agent.listed_hosts` adds other hosts,
each a host and a port from 1 to 65535, and neither adds nor removes the
automatic ones. With `internet_access: listed_hosts_only`, the agent
reaches only listed hosts; `any_site` also allows unlisted hosts at public
internet addresses. Listed hosts may use private addresses with either
value. Loopback, link-local, unspecified, and multicast addresses are
always refused.

`show` prints one line for each listed host, with why, and when the
inventory servers and vendor pages' hosts were listed:

```text
Listed host: app001:22; listed because: A server in the inventory “production”.
Listed host: github.com:443; listed because: The releases page of caddy and of cloudflared, and the security advisories page of caddy, from software_outside_the_distribution.yml.
Inventory servers and vendor pages' hosts listed at the start of the latest run: 2026-10-28 09:00:00 UTC. A server or a page removed since stays listed until the next run.
```

When the latest run left a vendor page out, a line says why, such as
`At the start of the latest run, MMHotline could not list the host of every vendor page in software_outside_the_distribution.yml: the releases_page of caddy, http://github.com/caddyserver/caddy/releases, is not an https:// address with a host`.
When MMHotline cannot read the file, it lists no vendor page hosts, and
that line names the failure. The run continues either way.

**The setup test.** A setup test passes when its run finishes without a
failure reason and the answer contains
`MMHotline triage agent test passed`. JSON has no `passed` field; the text
output and the page work it out. Only one triage agent run is active at a
time, and a setup test or sign-in check of the triage agent or the review
agent takes that one place.

**JSON fields.** The result has `message`, `web_url`, and `triage_agent`;
the fields below are inside `triage_agent`.

| Field | Meaning |
| --- | --- |
| `sandbox_installed` | Whether the run socket and logging proxy socket are configured. It reports configuration, not the sandbox's health |
| `setup` | The saved setup. Absent before setup or after removal. Never holds a credential |
| `setup.coding_agent`, `setup.credential_type` | From the table above |
| `setup.model`, `setup.reasoning_effort` | Passed to the coding agent on every run |
| `setup.set_up_at`, `setup.set_up_by` | When MMHotline saved the credential, and `the settings file` and its path. Changing only the model or reasoning effort keeps `set_up_at` |
| `setup.credential_expires_at` | When a `subscription_token` stops working, 365 days after `set_up_at`; absent for other types |
| `setup.sign_in_last_checked_at` | The end of the agent's latest successful run or latest failed sign-in check, whichever is later. A passed setup test counts |
| `setup.sign_in_works` | `false` when that latest run is a failed sign-in check |
| `setup.sign_in_failure_reason` | Why that sign-in check failed; empty when sign-in works |
| `commit_signing_public_key` | The applied `triage_agent.commit_signing_public_key`, a public key; absent while none is applied. [`check-preparation`](#mmhotline-triage-agent-check-preparation) compares it with the agent's git settings |
| `internet_access` | `listed_hosts_only` or `any_site` |
| `listed_hosts` | The automatic and added hosts |
| `listed_hosts[].host`, `listed_hosts[].port` | The host and port |
| `listed_hosts[].listed_because` | Why it is listed, such as `A server in the inventory “production”` |
| `listed_hosts[].added_by` | For a host from the settings file, `the settings file` and its path; absent for automatic hosts |
| `hosts_from_the_systems_repository_listed_at` | When the latest run in the systems repository started and listed the inventory servers and vendor pages' hosts. Absent before any such run |
| `inventory_servers_not_listed_because` | Why that run could not list every inventory server, such as a failed `mmx nodes`; absent when it listed them all |
| `vendor_page_hosts_not_listed_because` | Why that run left a vendor page out; absent when it left none out |
| `latest_setup_test` | The latest setup test; absent when none ran |
| `latest_setup_test.id`, `.purpose` | The run ID, and `setup_test` |
| `latest_setup_test.coding_agent`, `.credential_type` | What the test tested, which can differ from the saved setup |
| `latest_setup_test.coding_agent_session_id` | The coding agent's session ID; empty until reported |
| `latest_setup_test.state` | `running`, `finished`, `stopped`, or `failed`. `finished` alone does not mean it passed |
| `latest_setup_test.started_at`, `.started_by`, `.ended_at` | Start, author, and end; `ended_at` absent until recorded |
| `latest_setup_test.answer`, `.failure_reason` | The coding agent's answer, and why the test did not pass; empty when not available |
| `latest_setup_test.cost_in_us_dollars` | The reported cost; absent when not reported |
| `latest_setup_test.input_tokens`, `.output_tokens` | Reported tokens; zero when not reported |
| `latest_setup_test.cached_input_tokens` | The part of `input_tokens` read from the model service's cache, which costs less; zero when not reported |
| `latest_setup_test.steps` | Each step: `sequence`, `occurred_at`, `kind` (`message`, `command`, or `tool`), and `text`, cut to at most 2,000 characters |
| `latest_sign_in_check` | The latest sign-in check, with the fields of `latest_setup_test` and the purpose `sign_in_check`. `started_by` is `MMHotline` for the daily check. A check stopped for a triage session has the state `stopped`. Absent when none ran |
| `software_review_interval_hours` | `"off"`, or the hours between scheduled software reviews, 1 to 8760. Also `"off"` before the settings file set the key |
| `software_review_interval_changed_at`, `software_review_interval_changed_by` | When the setting applied, and `the settings file` and its path; absent while not set |
| `next_software_review_due_at` | One interval after the latest counted review started. Absent while the review is off, and before any counted review, since one is then due at once. A due review still waits while a triage agent run is active |
| `latest_software_review` | The latest software review, without its steps; absent when none ran |
| `latest_software_review.run_id`, `.state` | The run ID; `running`, `finished`, `stopped`, or `failed` |
| `latest_software_review.started_at`, `.started_by`, `.ended_at` | Start; `MMHotline` or the person who ran `run-software-review`; end, absent until recorded |
| `latest_software_review.failure_reason` | Why it failed or stopped, such as a stop for a triage session; empty when it finished |
| `latest_software_review.cost_in_us_dollars`, `.input_tokens`, `.output_tokens`, `.cached_input_tokens` | As for `latest_setup_test` |
| `latest_software_review.report_counts_by_alert_severity.sos`, `.warn`, `.info` | How many reports the review sent at each alert severity. A refused report does not count |
| `latest_commit_review` | The latest commit review run, which reviews commits no trusted key signed, with the fields of `latest_software_review` and `started_by` `MMHotline`. Absent when none ran |
| `recent_connections` | The latest 50 connection attempts, newest first |
| `recent_connections[].id`, `.run_id` | The connection and its run |
| `recent_connections[].started_at`, `.ended_at` | Start and end; end absent until recorded |
| `recent_connections[].host`, `.port`, `.address` | The requested host and port, and the checked IP address, empty if refused before the name was looked up |
| `recent_connections[].decision`, `.refusal_reason` | `allowed` or `refused`, and why |
| `recent_connections[].bytes_sent`, `.bytes_received` | Bytes each way, updated when the connection ends |
| `recent_connections[].can_add_listed_host` | Whether the page offers to add the refused destination to `triage_agent.listed_hosts`; never for a review agent run, which a listed host does not let through |

## mmhotline triage-agent check-sign-in

Runs the triage agent's sign-in check now and waits for it.

```sh
sudo mmhotline triage-agent check-sign-in
```

The output is that of [`triage-agent show`](#mmhotline-triage-agent-show),
with `Sign-in to Claude Code: works; last checked at TIME.` or `fails` and
the reason. [Keep the agents signed in](../how-to/keep-the-agents-signed-in.md#check-a-sign-in)
uses it.

- **Flags:** the common flags.
- **The check:** an API key is checked with the vendor's models list; any
  other credential with the setup test's prompt, as the
  [daily sign-in check](../changing-mmhotline/design/triage-agent.md#the-daily-sign-in-check)
  does. It counts as the daily check: the next is due 24 hours later.
- **Waits** at most 5 minutes, checking every 2 seconds.
- **Refused** while another triage agent run is active:
  "A triage agent run is already active." A triage session that needs the
  run stops the check after the agent's current step, and the command
  fails with that reason.
- A failed check sends the administrators a warn alert; the next
  successful run sends an info alert.
- **JSON:** the fields of `triage-agent show`, and `triage_agent_run_id`,
  the check's run ID.

| Exit code | Meaning |
| --- | --- |
| `0` | The sign-in works |
| `4` | The check failed, did not finish within 5 minutes, or was refused |

## mmhotline triage-agent run-software-review

Starts one software review now, also while the scheduled review is off,
such as to learn what one review costs.

```sh
sudo mmhotline triage-agent run-software-review
```

[Turn on the scheduled software review](../how-to/turn-on-the-scheduled-software-review.md)
uses it.

- **Flags:** the common flags.
- **Returns once the run has started,** without waiting for the review,
  which takes minutes.
- **Counts as a review:** the next scheduled one is due one interval after
  it started.
- **Refused** while another triage agent run is active: "A triage agent
  run is already active." A triage session that needs the run stops the
  review after the agent's current step.
- The review uses the team's task in `scheduled_software_review_task.md`
  in the systems repository. A task file that is missing, empty, or over
  64 KiB ends the review at once as failed; `show` prints the reason as
  `Software review failure reason:`.
- **What one review costs:** wait until `show` prints
  `Software review ended at:`, then read `latest_software_review` in
  `show --json`: `cost_in_us_dollars` when the coding agent reports it,
  and the token counts.
- The review's reports reach people as alerts; `show` prints how many it
  sent at each alert severity.
- **JSON:** the fields of `triage-agent show`, and `triage_agent_run_id`,
  the review's run ID.

## mmhotline triage-agent check-preparation

Checks the eight things a team prepares by hand before the triage agent
works, and prints one line for each: `Present:` or `Missing:`.

```sh
sudo mmhotline triage-agent check-preparation
```

```text
Present: 1. Server names resolve: web1 -> 10.0.0.11.
Present: 2. The triage user accepts the agent's key: triage@ on web1 accepts /var/lib/mmhotline-triage-agent/.ssh/id_ed25519.
...
Present: 8. mmx and its node runners are installed: /usr/local/bin/mmx, /usr/local/bin/mmx-runner-linux-amd64.
All 8 checks are present.
```

[Set up the triage agent](../how-to/set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository)
runs it after the preparation steps.

| Flag | Meaning |
| --- | --- |
| `--json` | Print one JSON object |
| `--help` | Show the flags and exit codes |

- **Runs as root, without the command socket for its checks.** Reason: it
  reads the triage agent's home folder, `/var/lib/mmhotline-triage-agent`
  (mode `0700`), and logs in with the agent's private key and with
  MMHotline's own, which the sandboxed `mmhotline serve` cannot do. It
  asks the running MMHotline only for the applied setup and commit signing
  key. It takes no `--command-socket`.
- **Changes nothing,** and never prints a private key, a token, or a
  credential from the settings.
- **The checks,** each with the preparation step that fixes it:
  1. Each server name in the inventories resolves to an address that is
     not loopback (step 1).
  2. The `triage` user on each server accepts the agent's key,
     `id_ed25519` in its home (step 2).
  3. The agent's `.gitconfig` signs commits with SSH, using the applied
     `triage_agent.commit_signing_public_key` (step 3). This line stays
     missing until the agent is set up.
  4. `triage_agent_rules.md` exists in MMHotline's copy of the systems
     repository (step 4).
  5. The copy's `ssh/known_hosts` holds a host key for each server
     (step 4).
  6. MMHotline's own key logs in to each server, as the inventory's
     `ssh_user` or `root` (step 5).
  7. The copy's `origin` is an SSH address, and
     `git ls-remote --heads origin` works as the `mmhotline` user (step 6).
  8. `mmx` is installed, with `mmx-runner-linux-amd64` or
     `mmx-runner-linux-arm64` beside it for each server's processor
     (step 7).
- **A missing line** ends with `Fix:` and the how-to page's address with
  the step. A check whose input is missing, such as no copy or no servers,
  is missing with the reason. The last line is `All 8 checks are present.`
  or, such as, `7 of 8 checks are present; 1 is missing.`

| Exit code | Meaning |
| --- | --- |
| `0` | All eight are present |
| `1` | At least one is missing |
| `3` | It cannot run, such as without root |
| `4` | The arguments are wrong |

**JSON fields:** `checks`, one for each check:

| Field | Meaning |
| --- | --- |
| `checks[].number`, `checks[].name` | The check's number and name, such as `1` and `Server names resolve` |
| `checks[].present` | Whether the check passed |
| `checks[].detail` | What the check found |
| `checks[].fix` | The how-to page's address and step that fixes a missing check |

## mmhotline triage-agent run-inside-sandbox

systemd starts this command for each triage agent run; people do not run
it.

```text
mmhotline triage-agent run-inside-sandbox --proxy-socket PATH
```

- `--proxy-socket PATH` is required: the logging proxy socket mounted in
  the sandbox. Its installed path is
  `/run/mmhotline-triage-agent-proxy.sock`.
- It takes `--help` and none of the common flags. It exchanges run
  messages over standard input and output, not the command socket.

## mmhotline triage-agent ssh-proxy

SSH runs this command inside a triage agent run, as the `ProxyCommand` of
the agent's `~/.ssh/config`; people do not run it.

```text
mmhotline triage-agent ssh-proxy HOST PORT
```

- Carries one SSH connection through MMHotline's logging proxy.
- Refuses an IP address and says to connect by the server's name in the
  inventory. Refuses outside a run.
- When the proxy refuses, it exits 255 and prints the proxy's reason on
  standard error.
- It takes `--help` and none of the common flags.

## mmhotline triage-agent ssh

The agent's `ssh` runs this command: each triage agent run puts it first
on the agent's search path. People do not run it.

```text
mmhotline triage-agent ssh SSH_ARGUMENTS
```

- Drops each `-F` option and the file it names, with a notice on standard
  error, then replaces itself with the real `ssh` and the remaining
  arguments, so the agent's `~/.ssh/config` applies
  ([the agent's ssh](../changing-mmhotline/design/triage-agent.md#the-agents-ssh)).
- Exits 255 outside a run. It takes `ssh`'s arguments and has no `--help`
  of its own.

## mmhotline triage-agent request-playbook-run

The triage agent runs this command inside a run to ask MMHotline to run an
MMX playbook; people do not run it
([requests from the agent](../changing-mmhotline/design/triage-agent.md#requests-from-the-agent)).

```text
mmhotline triage-agent request-playbook-run --playbook PATH --inventory NAME --diagnosis-file FILE [--json]
```

- `FILE` holds the agent's diagnosis in Markdown, at most 64 KiB. The
  review agent and the people who approve read it.
- The request carries the agent's commits, which MMHotline pushes when
  they change nothing outside `one_time_playbooks/` and
  `triage_agent_notes/`.

| Exit code | Meaning |
| --- | --- |
| `0` | The playbook ran and succeeded |
| `1` | The playbook ran and failed |
| `3` | The request waits for a person |
| `4` | Refused or invalid, or the run is ending. Also outside a run |

## mmhotline triage-agent report

The triage agent runs this command inside a run to send a report, which
MMHotline records with the run and sends as an alert; people do not run
it.

```text
mmhotline triage-agent report --alert-severity info|warn|sos --report-file FILE [--json]
```

- `FILE` holds the report in Markdown, at most 64 KiB.
- Exit codes: `0` recorded; `4` refused or invalid, or the run is ending,
  or outside a run.

## mmhotline triage-agent review-commit

The triage agent runs this command in a commit review run to give its
decision on a commit no trusted key signed; people do not run it.

```text
mmhotline triage-agent review-commit --commit-id COMMIT_ID --decision approve|needs_a_person --report-file FILE [--alert-severity info|warn|sos] [--json]
```

- `approve` marks the commit reviewed and sends one info alert with the
  report. `needs_a_person` leaves the commit waiting and sends the report
  at `--alert-severity`, which `needs_a_person` requires and `approve`
  refuses.
- `FILE` holds the report in Markdown, at most 64 KiB.
- Exit codes: `0` recorded; `4` refused, such as `approve` of a commit that
  changes a path only a person may approve, or invalid, or outside a run
  ([the agent's decision](../changing-mmhotline/design/triage-agent.md#the-agents-decision)).

[Review a commit](../how-to/review-a-commit.md) says what happens to the
decision.

## mmhotline run-fleet-view-command

systemd starts this command as root, from `mmhotline-fleet-view@.service`,
for each connection to the fleet view socket; people do not run it.

```text
mmhotline run-fleet-view-command --settings-file /etc/mmhotline/mmhotline_settings.yml
```

- Reads `fleet_view_command` from the settings file on each connection and
  runs its words without a shell. The command gets only `PATH` in its
  environment and the null device as input; its output goes to the
  connection, and its errors to the journal.
- Exits 0 whatever the fleet view command's exit status, because a fleet
  view exits nonzero while a server fails. Exits 1 when the command cannot
  start at all.
- Exits 4 when `--settings-file` is missing, when `fleet_view_command` is
  missing or invalid, or when the settings file fails its owner and mode
  check: root runs the command, so only root may change the file.
  Problems in other settings do not stop it.

[Connect a tool that sends alerts](../how-to/connect-a-tool-that-sends-alerts.md#show-the-tools-servers-on-the-dashboard)
sets `fleet_view_command`.

## mmhotline review-agent show

Shows the review agent's setup, whether its sign-in works, its latest
setup test, and its latest sign-in check. The **review agent** is an
optional second coding agent that must approve each playbook the triage
agent asks to run, before the rules for running the agent's playbooks
apply.

```sh
sudo mmhotline review-agent show
```

```text
Triage agent sandbox installed: true.
Review agent: Codex with an API key, model gpt-6-astra, reasoning effort high (coding_agent codex, credential_type api_key, model gpt-6-astra, reasoning_effort high), set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:55:26Z.
Sign-in to Codex: works; last checked at 2026-11-15T08:55:26Z.
Setup test 17916abc6746d58900e9d6953b6202d4: passed; run state: finished; purpose: review_agent_setup_test; coding agent: codex; credential type: api_key.
...
```

[Set up the triage agent](../how-to/set-up-the-triage-agent.md#add-the-review-agent)
adds it.

- **Flags:** the common flags.
- **Setup:** the settings file's `review_agent` section, with the triage
  agent's five setup keys and their rules. Its setup test runs in
  `settings apply`. Leaving the section out removes it, and playbooks are
  no longer reviewed.
- **Reviews:** when the review agent refuses, its reason goes back to the
  triage agent, which may revise the playbook and ask again. After the
  third refusal in a row on a problem, the request waits for a person. Any
  coding agent may review, also the triage agent's own; one from another
  vendor is recommended, because it is less likely to share the triage
  agent's mistakes
  ([design](../changing-mmhotline/design/triage-agent.md#the-review-agent)).
- **JSON:** `message`, `web_url`, and `review_agent`, with
  `sandbox_installed`, `setup`, `latest_setup_test`, and
  `latest_sign_in_check`, in the fields of
  [`triage-agent show`](#mmhotline-triage-agent-show).
  `latest_setup_test.purpose` is `review_agent_setup_test`, and
  `latest_sign_in_check.purpose` is `review_agent_sign_in_check`.

## mmhotline review-agent check-sign-in

Runs the review agent's sign-in check now, as a review runs, and waits for
it, as [`triage-agent check-sign-in`](#mmhotline-triage-agent-check-sign-in)
does, with the same exit codes.

```sh
sudo mmhotline review-agent check-sign-in
```

- **Flags:** the common flags.
- **JSON:** the fields of `review-agent show`, and `review_agent_run_id`,
  the check's run ID.

Used in [Keep the agents signed in](../how-to/keep-the-agents-signed-in.md#check-a-sign-in).

## mmhotline agent-playbook-rules show

Shows the rules for running the agent's playbooks: for each inventory and
kind of action, whether MMHotline runs a playbook the triage agent asked
for without a person. It also shows the services whose changes a person
must approve.

```sh
sudo mmhotline agent-playbook-rules show --inventory production
```

```text
Services whose changes a person must approve, on every inventory: lab-database.service; changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
Rules for running the agent's playbooks on inventory production:
Start or restart a service (start_or_restart_a_service): Act, then report (act_then_report); changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
...
Reboot a server (reboot_a_server): Get permission, then act (1 approval) (get_permission_then_act); changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
...
The rules for running the agent's playbooks and the services whose changes a person must approve are set in /etc/mmhotline/mmhotline_settings.yml, in rules_for_running_agent_playbooks and services_whose_changes_a_person_must_approve.
```

[Decide what the triage agent may do](../how-to/decide-what-the-triage-agent-may-do.md)
sets the rules. The
[design](../changing-mmhotline/design/triage-agent.md#rules-for-running-the-agents-playbooks-and-kinds-of-action)
owns the kinds of action, the presets, and how a playbook's rule is
chosen.

| Flag | Meaning |
| --- | --- |
| `--inventory NAME` | Optional. Show only this inventory: letters, digits, `_`, and `-`, at most 100 characters |
| Common flags | As above |

- Without `--inventory`, it lists every inventory in MMHotline's copy of
  the systems repository and every inventory with saved rules.
- **An inventory without an entry** in `rules_for_running_agent_playbooks`
  fails that part of `settings apply`, and every kind of action on it uses
  Get permission, then act with 1 approval until the entry is added.
- **A kind of action an entry's `action_kinds` leaves out** has no saved
  rule and uses Get permission, then act with 1 approval. The part still
  applies, with a note. `show` and the page add
  `check_only has no rule; it needs a person until you add one` to that
  kind's line.
- Members read the same rules on the page of rules for running the
  agent's playbooks.

**JSON fields:** `message`, `web_url`, and:

| Field | Meaning |
| --- | --- |
| `services_whose_changes_a_person_must_approve.service_names` | The unit names, such as `pgautofailover.service`, in the order given; an empty list when none |
| `services_whose_changes_a_person_must_approve.changed_at`, `.changed_by` | When the list was saved, and `the settings file` and its path; absent while not set |
| `agent_playbook_rules` | One entry for each inventory |
| `agent_playbook_rules[].inventory` | The inventory's name |
| `agent_playbook_rules[].inventory_file_exists` | Whether MMHotline's copy has `inventories/<inventory>.yml` |
| `agent_playbook_rules[].action_kinds` | One entry for each kind of action: `start_or_restart_a_service`, `free_disk_space`, `pull_a_container_image_again`, `reboot_a_server`, `block_a_network_range`, `change_dns_records`, `check_only`, and `unrecognized_task` |
| `action_kinds[].action_kind`, `action_kinds[].action_kind_title` | The kind's ID and name, such as `reboot_a_server` and "Reboot a server" |
| `action_kinds[].rule`, `action_kinds[].rule_title` | `get_permission_then_act`, `warn_wait_then_act`, or `act_then_report`, and its name with its wait or approvals, such as "Warn, wait 10 minutes, then act" |
| `action_kinds[].warning_wait_minutes` | Minutes to wait after the warning; only with `warn_wait_then_act` |
| `action_kinds[].approvals_needed` | People who must approve, 1 or 2; only with `get_permission_then_act` |
| `action_kinds[].changed_at`, `action_kinds[].changed_by` | When the rule was saved, and `the settings file` and its path; absent while the kind has no saved rule |
| `agent_playbook_rules[].listed_playbooks` | The playbooks listed by name under the inventory's `playbooks` in the settings file, in name order; an empty list when none |
| `listed_playbooks[].playbook_name` | NAME of `playbooks/NAME.yml` |
| `listed_playbooks[].rule`, `.rule_title`, `.warning_wait_minutes`, `.approvals_needed`, `.changed_at`, `.changed_by` | As in `action_kinds` |
| `listed_playbooks[].its_approval_also_approves_in_the_same_session` | The listed playbooks its approval also approves once in the same triage session; absent when none. The text adds "its approval also approves in the same session: every_server, destroy_replaced_servers" after the rule |
| `listed_playbooks[].starts_a_server_replacement` | `true` when a run of the playbook starts a server replacement, during which an SOS of the session waits longer; absent otherwise |

## mmhotline agent-playbook-rules check-whether-a-playbook-needs-a-person

Shows whether a playbook needs a person when the triage agent asks to run
it on an inventory, and why. It changes nothing.

```sh
sudo mmhotline agent-playbook-rules check-whether-a-playbook-needs-a-person \
  --inventory production --playbook playbooks/stop_lab_database.yml
```

```text
Needs a person: Get permission, then act (1 approval).
Why: A node task acts on a service on the list of services whose changes a person must approve, so the playbook waits for a person whatever the rules for its kinds of action, with the number of approvals required by the strictest rule, and at least 1 approval: Get permission, then act (1 approval).
Checked playbooks/stop_lab_database.yml in the inventory “production” at commit a4fce954720b97d948bdb9f80640cbc5ee5ede5e of MMHotline's copy of the systems repository.
Node tasks, as mmx explain listed them, with their kinds of action:
  stop/task_1 on web1: systemd_service; Unrecognized task
Rules for the playbook's kinds of action on the inventory “production”:
  Unrecognized task (unrecognized_task): Get permission, then act (1 approval) (get_permission_then_act); changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
Tasks on services whose changes a person must approve:
  stop/task_1 (systemd_service) stops lab-database.service on web1, which is on the list of services whose changes a person must approve
  An administrator changes the list in services_whose_changes_a_person_must_approve in MMHotline's settings file.
Tasks MMHotline did not recognize, and why:
  stop/task_1 (systemd_service): systemd_service.state must be running
MMHotline checked the playbook as it checks a triage agent's request to run it, and changed nothing: it ran only mmx explain, and recorded no request, asked for no approval, sent no alert, and ran no playbook.
```

[Decide what the triage agent may do](../how-to/decide-what-the-triage-agent-may-do.md#check-whether-a-playbook-needs-a-person)
uses it.

| Flag | Meaning |
| --- | --- |
| `--playbook PATH` | Required. The playbook's path in the systems repository, as the triage agent gives it: `playbooks/NAME.yml` or `one_time_playbooks/YYYY-MM-DD_what_it_does.yml` |
| `--inventory NAME` | Required. NAME of `inventories/NAME.yml` |
| Common flags | As above |

- **How it checks:** exactly as MMHotline checks the agent's request
  ([design](../changing-mmhotline/design/triage-agent.md#checking-whether-a-playbook-needs-a-person)):
  it runs `mmx explain` in MMHotline's copy of the systems repository, at
  the copy's current commit. It fetches nothing, so the playbook must be
  in a commit the copy already holds.
- **Changes nothing:** no request, no approval, no alert, no playbook run.
- **The first line** is one of three:

  | First line | What MMHotline does with the agent's request |
  | --- | --- |
  | `Needs a person: Get permission, then act (1 approval).` | Runs the playbook only after people approve it, 1 or 2 as the rule says |
  | `Runs without a person unless someone presses Stop: Warn, wait 10 minutes, then act.` | Warns the team, waits, then runs it unless someone presses Stop |
  | `Runs without a person: Act, then report.` | Runs it at once |

- **With a review agent set up,** a playbook that would run without a
  person gets this line after `Why:`:
  `A review agent would also review the playbook before the playbook runs. The check command does not ask the review agent.`
  A playbook that needs a person is not reviewed and gets no such line.
- **Refused, exit 4,** where the agent's request would be refused, with
  the same message: a path outside `playbooks/` and `one_time_playbooks/`,
  an invalid inventory name, or a playbook MMX cannot read. For example:
  `MMHotline refused the request: the playbook path "one_time_playbooks/../deploy.yml" must not have an empty, ".", or ".." part.`
- **Waits** while an MMX run holds the copy's lock, and times out after
  30 seconds; it works again once the MMX run ends.

**JSON fields:** `message`, `web_url`, and
`whether_a_playbook_needs_a_person`:

| Field | Meaning |
| --- | --- |
| `playbook`, `inventory` | What was checked |
| `needs_a_person` | `true` only for `get_permission_then_act`; `false` for `act_then_report`, and for `warn_wait_then_act`, which runs unless someone presses Stop |
| `rule`, `rule_title` | The rule the playbook takes, with `approvals_needed` for `get_permission_then_act` and `warning_wait_minutes` for `warn_wait_then_act` |
| `reason` | Why, in plain words |
| `listed_playbook` | The rule a person listed for the playbook by name, in the fields of `show`'s `listed_playbooks`; absent when not listed |
| `review_agent_would_review_the_playbook` | `true` when a review agent is set up and the playbook would run without a person |
| `commit_id` | The commit of MMHotline's copy that `mmx explain` read |
| `tasks` | Each node task: `path`, `node` (the server it runs on), `module`, `action_kind`, and `action_kind_title` |
| `action_kinds` | The playbook's kinds of action with their rules, in the fields of `show`'s `action_kinds` |
| `tasks_on_services_whose_changes_a_person_must_approve` | `path`, `node`, `module`, `service_name`, and `reason`, as in [`approval-requests show`](#mmhotline-approval-requests-show). Present also when empty |
| `unrecognized_tasks` | `path`, `module`, and `reason`, as in `approval-requests show`. Present also when empty |

## mmhotline contact-methods list

Lists contact methods: all of them, one person's, or the team's shared
ones.

```sh
sudo mmhotline contact-methods list --sign-in-email-address barbara@example.com
```

```text
Contact method fe9d9b2757973d600bf0142391667e35 belongs to barbara@example.com.
Contact method type: email. Sends to: barbara@example.com. Alert severities: info,sos,warn.
Send while unavailable or muted: true. Last test message: accepted (queued at 2026-11-15T05:41:53Z).
Listed 1 contact method.
```

[Add contact methods](../how-to/add-contact-methods.md) uses every
`contact-methods` command.

| Flag | Meaning |
| --- | --- |
| `--sign-in-email-address ADDRESS` | Optional. Only this person's contact methods |
| `--shared-with-team` | Optional. Only the team's shared contact methods |
| Common flags | As above |

**JSON fields** of every `contact-methods` command: `message`, `web_url`,
and:

| Field | Meaning |
| --- | --- |
| `contact_methods` | The listed methods, or the one a save or test affected. `remove` returns none |
| `contact_methods[].contact_method_id` | The ID that `change`, `remove`, and `send-test` take |
| `contact_methods[].sign_in_email_address` | The owner; empty for a shared method |
| `contact_methods[].shared_with_team` | Whether the method belongs to the team |
| `contact_methods[].contact_method_type` | `email`, `outgoing_webhook`, `automated_voice_call`, `pushover_notification`, `telegram_message`, or `slack_message` |
| `contact_methods[].sends_to` | The destination the pages show. A webhook's path and query are hidden, because they may hold tokens |
| `contact_methods[].alert_severities` | The alert severities the method gets |
| `contact_methods[].send_while_unavailable_or_muted` | Whether it sends while the person is unavailable or the team is muted, within the Pushover and phone rules |
| `contact_methods[].last_test_message` | The latest test message, or `null` |
| `contact_methods[].last_test_message.delivery_state` | Such as `queued`, `accepted`, `failed`, or `uncertain` |
| `contact_methods[].last_test_message.delivery_state_explanation` | Why, such as an error, a wait, or the service's answer (`HTTP 200`) |
| `contact_methods[].last_test_message.queued_at` | When the test was queued |
| `contact_methods[].pushover_acknowledgement_problem` | Present when an acknowledgement in Pushover came from another Pushover user, so it did not claim an incident, such as when the key is a group key. It says the problem and the fix: remove the contact method and add it again with the person's own user key. `list` prints it as a `Problem:` line |
| `pushover_group_key_warning` | `add` of a Pushover method only, when Pushover says the key is a group key: the capitals warning, which the command prints on its own line. Absent when the key is a user key or Pushover did not answer |
| `outgoing_webhook_signing_secret_id` | `add` of a webhook only: the new signing secret's ID |
| `outgoing_webhook_signing_secret_shown_once` | `add` of a webhook only, without an output file: the new signing secret in hex |
| `outgoing_webhook_signing_secret_file` | `add` of a webhook only, with an output file: its path, in place of the secret |

## mmhotline contact-methods add

Adds a contact method for a person or for the team, and queues a test
message.

```sh
sudo mmhotline contact-methods add --sign-in-email-address barbara@example.com \
  --contact-method-type email --email-address barbara@example.com \
  --alert-severities sos,warn,info --send-while-unavailable-or-muted
```

```text
Contact method fe9d9b2757973d600bf0142391667e35 belongs to barbara@example.com.
Contact method type: email. Sends to: barbara@example.com. Alert severities: info,sos,warn.
Send while unavailable or muted: true. Last test message: queued (queued at 2026-11-15T05:41:53Z).
Contact method saved. A test message for the contact method is queued. Inspect the last test result to check delivery.
```

| Flag | Meaning |
| --- | --- |
| `--sign-in-email-address ADDRESS` or `--shared-with-team` | Required: the owner, one person or the team |
| `--contact-method-type TYPE` | Required: `email`, `outgoing_webhook`, `automated_voice_call`, or `pushover_notification` |
| `--alert-severities VALUES` | Comma-separated `sos`, `warn`, `info`, in any order, without repeats. An empty string gets no alerts. Required, except for Pushover, which defaults to `sos,warn,info`. Pass `--alert-severities sos` for SOS only. An automated voice call takes only `sos` |
| `--send-while-unavailable-or-muted` | Optional. Without it, the method follows the person's unavailability and the team's mute. A team mute always silences Pushover and phone calls, even with this flag |
| `--email-address ADDRESS` | With `email`: the address |
| `--outgoing-webhook-url URL` | With `outgoing_webhook`: the receiver's `https://` URL |
| `--outgoing-webhook-bearer-token-file FILE` | With `outgoing_webhook`, optional: a file holding only the bearer token; surrounding whitespace is trimmed |
| `--outgoing-webhook-signing-secret-file FILE` | With `outgoing_webhook`, optional: where to write the new signing secret, instead of printing it |
| `--phone-number-to-call NUMBER` | With `automated_voice_call`: E.164 form, such as `+12025550123`. Personal methods only |
| `--pushover-user-key-file FILE\|-` | With `pushover_notification`: a file only its owner can read, or `-` for standard input, holding the person's own user key, exactly 30 letters and digits. A Pushover delivery-group key is saved, with a warning, but its acknowledgements cannot claim an incident. Personal methods only; needs the Pushover sending service |
| Common flags | As above |

- **The test message** is queued in the same save, and the method gets
  alerts at once. A queued test is not proof of delivery; read the last
  test result with `list`. An automated voice call's test counts against
  the daily test limits; at the limit, the method is still added, no test
  is queued, and `message` says `Test not sent:` and why.
- **Telegram and Slack** are connected by the person on the Contact
  methods page. Asking for one returns that instruction with the page's
  full address.
- **Phone numbers** keep the page's limits on allowed countries,
  duplicates, and numbers per person.
- **The Pushover user key** never goes on the command line, where it
  would stay in the shell history and the list of running processes. The
  command request and the stored settings call it `pushover_user_key`;
  results show it in `sends_to`. Saving queues an emergency test alert.
- **The webhook signing secret** is made once and shown once. The output
  file gets mode `0600` and holds only the secret, without a newline. The
  command writes a private temporary file in the same folder first, then
  renames it over the path, which replaces an existing file. The folder
  must exist.
- **Refused, exit 4,** for a flag of another contact method type, a key
  that is not 30 letters and digits, or a duplicate, such as
  `you already added this Pushover notification contact method`.

## mmhotline contact-methods change

Changes a contact method's alert severities, or whether it sends while the
person is unavailable or the team is muted.

```sh
sudo mmhotline contact-methods change --contact-method-id fe9d9b2757973d600bf0142391667e35 --alert-severities sos
```

| Flag | Meaning |
| --- | --- |
| `--contact-method-id ID` | Required |
| `--alert-severities VALUES` | As for `add` |
| `--send-while-unavailable-or-muted true\|false` | Give one or both of these two flags |
| Common flags | As above |

A change queues no new test message.

## mmhotline contact-methods remove

Removes a contact method.

```sh
sudo mmhotline contact-methods remove --contact-method-id fe9d9b2757973d600bf0142391667e35
```

- **Flags:** `--contact-method-id ID`, required, and the common flags.
- Removing the contact methods that use a sending service lets
  `settings apply` remove that sending service.

## mmhotline contact-methods send-test

Sends a contact method a test message now.

```sh
sudo mmhotline contact-methods send-test --contact-method-id 2c35b3af0c71bc6da2ea6e1c9cd3ce59
```

```text
Contact method 2c35b3af0c71bc6da2ea6e1c9cd3ce59 belongs to barbara@example.com.
Contact method type: pushover_notification. Sends to: FakePushoverPersonalUserKey001. Alert severities: info,sos,warn.
Send while unavailable or muted: false. Last test message: queued (queued at 2026-11-15T05:08:44Z).
Test message queued; inspect the last test result for delivery.
```

- **Flags:** `--contact-method-id ID`, required, and the common flags.
- It keeps the page's test rate and phone-call cost limits.
- [Set up the MMHotline-down alarm](../how-to/set-up-the-mmhotline-down-alarm.md)
  and [Connect Pushover](../how-to/connect-pushover.md) use it to check a
  contact method.

## mmhotline alert-sources list

Lists the alert sources, each with its addresses and check-in state, and
never a signing secret's value. An **alert source** is one sender of
signed alerts and check-ins, such as a monitoring tool.

```sh
sudo mmhotline alert-sources list
```

```text
Alert source Monitoring (monitoring) is active.
Intake URL: http://10.20.0.1:8484/alert-intake/monitoring
Check-in URL: http://10.20.0.1:8484/check-in/monitoring
No check-in has arrived yet. MMHotline starts watching for missing check-ins when the first check-in arrives.
...
Alert intake address: http://10.20.0.1:8484
```

[Connect a tool that sends alerts](../how-to/connect-a-tool-that-sends-alerts.md)
adds sources with the settings file's `alert_sources`
([settings file](settings-file.md#alert_sources)) and reads this list;
[Declare planned downtime](../how-to/declare-planned-downtime.md) reads
its downtime. [Alert requests](alert-requests.md) shows a sender that uses
these addresses.

- **Flags:** the common flags only.
- **A source left out of the settings file** is turned off: its signing
  secrets stop working at once, and its alerts and check-ins stay. Listing
  it again turns it back on with the listed secrets.
- **Senders send to the alert intake address,** never to the web URL: the
  web URL sits behind Cloudflare Access, which refuses a sender without a
  person's sign-in. A sender on the MMHotline server uses the same
  address as senders elsewhere.
- The Alert sources page shows the same addresses.

**JSON fields:** `message`, `alert_intake_address`, and `alert_sources`;
no `web_url`.

| Field | Meaning |
| --- | --- |
| `alert_intake_address` | Once, for every source: `http://` and `alert_intake_listen_address`, such as `http://10.20.0.1:8484` |
| `alert_sources[].alert_source_id` | The source's ID, such as `monitoring` |
| `alert_sources[].name` | Its display name |
| `alert_sources[].active` | Whether signed requests are accepted; `false` for a source the settings file no longer lists |
| `alert_sources[].signing_secrets` | Each secret's `signing_secret_id` and `created_at`, when it was first saved; never the value |
| `alert_sources[].intake_url` | The intake address, `/alert-intake/`, and the source ID |
| `alert_sources[].check_in_url` | The intake address, `/check-in/`, and the source ID |
| `alert_sources[].check_in_state` | The check-in state, below |

`check_in_state` matches the source's page:

| Field | Meaning |
| --- | --- |
| `expected` | At least one check-in has arrived |
| `last_check_in_at` | The latest check-in's receipt time |
| `last_heard_at` | The latest receipt of an alert or check-in |
| `sent_by_node_name` | The server the last check-in named |
| `warn_after_no_check_in_for_minutes` | Minutes without a check-in before a Warning; `0` means off |
| `sos_after_no_check_in_for_minutes` | Minutes without a check-in before an SOS |
| `nodes_in_whole_node_maintenance` | The maintenance windows the sender listed, each with `node_name`, `maintenance_until`, and optional `reason` and `started_by` |
| `planned_downtime` | The downtime in effect, or `null` |
| `planned_downtime.planned_downtime_id` | The ID of downtime a team member set; empty for downtime the sender listed |
| `planned_downtime.started_by_name` | Who started the downtime |
| `planned_downtime.sender_maintenance_node_name` | The server whose sender-listed window gives the downtime; empty for downtime set by hand |
| `planned_downtime.until`, `planned_downtime.reason` | When the downtime ends, and why |

## mmhotline schedules show

Shows the saved on-call schedule, and who an SOS wakes first now and who
next, with unavailability and taking on-call counted, as the dashboard's
**On call** panel shows.

```sh
sudo mmhotline schedules show
```

```text
Active schedule type: weekly_rotation. Schedule revision: 2.
Time to claim before MMHotline escalates: 5 minutes. Each escalation step waits this long for a claim before the next escalation step starts.
Saved weekly_rotation schedule in America/New_York. Handoff: 09:00; weekday: 1 (Sunday is 0); first start date: 2026-11-02. Backup option: everyone_available; fixed backup: no one scheduled.
Rotation position 1: barbara@example.com.
Rotation position 2: carlos@example.com.
On call now: Carlos Example (carlos@example.com). Backup now: everyone else available: Barbara Example (barbara@example.com).
Listed the saved schedule, set in /etc/mmhotline/mmhotline_settings.yml, in on_call_schedule.
```

[Set up people and the on-call schedule](../how-to/set-up-people-and-the-on-call-schedule.md)
sets the schedule; [Be away or take over on call](../how-to/be-away-or-take-over-on-call.md)
and [Mute the team](../how-to/mute-the-team.md) read the `On call now`
line.

- **Flags:** the common flags.
- **The schedule** comes from the settings file's `on_call_schedule`
  ([settings file](settings-file.md#on_call_schedule)).
  `settings apply` saves and activates it and removes saved schedules of
  other types. The backup is where an SOS escalates; nothing else sets
  escalation.
- **The `On call now` line** says `On call now: nobody.` when nobody is
  scheduled or available, and `everyone available:` or
  `everyone else available:` when the schedule calls for everyone. It
  adds the reason when coverage differs from the plain schedule, such as
  `The person who took on-call is contacted first.`, and while the team is
  muted, that an SOS wakes nobody until the mute ends.
- **People** are named by sign-in email address. MMHotline saves member
  IDs and turns them back into addresses when it reads them.

**The time to claim** is
`on_call_schedule.escalate_if_unclaimed_after_minutes`, 1 to 60: how long
each escalation step, such as the person on call and then the backup,
waits for a claim before MMHotline contacts the next step
([incident design](../changing-mmhotline/design/incidents.md#initial-notification-and-repeats)).
Each step stores its end time when it starts. A new time to claim applies
to every step that starts later, also the next step of an open incident;
the current step keeps its end time, which the incident page shows. For
example, an SOS opened at 09:00 with five minutes still reaches the
backup at 09:05 when two minutes applies at 09:01; the backup then has
until 09:07.

**JSON fields:** `message`, `web_url`, and `schedule_state`:

| Field | Meaning |
| --- | --- |
| `schedule_state.active_schedule_type` | The active type; empty before the schedule first applied |
| `schedule_state.revision` | The schedule's revision; it grows with each save |
| `schedule_state.escalate_if_unclaimed_after_minutes` | The time to claim |
| `schedule_state.schedule_configurations` | The saved schedule, as a list of one object, below; empty before the schedule first applied |
| `schedule_state.on_call_now` | Who an SOS wakes now, below |

`schedule_configurations[]` uses the names of `on_call_schedule`'s keys:

| Field | Meaning |
| --- | --- |
| `schedule_type` | `day_of_week`, `hour_of_day`, `weekly_rotation`, or `custom_schedule_code` |
| `time_zone` | An IANA time zone name, such as `America/New_York` |
| `handoff_hour`, `handoff_minute` | The local handoff time, 0–23 and 0–59; daily and weekly schedules |
| `assignment_sign_in_email_addresses` | Seven daily slots, Sunday first, or 24 hourly slots, midnight first. An empty address means nobody is scheduled first for an SOS then. Empty for rotation and custom |
| `rotation_sign_in_email_addresses` | The rotation, in order; empty for other types |
| `handoff_weekday` | Sunday `0` to Saturday `6`; weekly rotation |
| `first_start_date` | The first person's local start date, `YYYY-MM-DD`, on the handoff weekday |
| `backup_option` | `everyone_available`, `fixed_person`, or, for a weekly rotation only, `next_in_rotation` |
| `backup_sign_in_email_address` | The fixed backup with `fixed_person`; empty otherwise |
| `custom_schedule_code` | The Starlark code; custom schedules only. Its results `primary_sign_in_email_address` and `backup_sign_in_email_address` are sign-in email addresses ([Write a custom schedule](../how-to/write-a-custom-schedule.md)) |
| `custom_schedule_test_cases` | Each with `local_time`, `expected_primary_sign_in_email_address`, and `expected_backup_sign_in_email_address` |

`on_call_now`:

| Field | Meaning |
| --- | --- |
| `at` | The time the answer is for |
| `on_call` | Who an SOS wakes first, each with `name` and `sign_in_email_address`; present also when empty |
| `on_call_is_everyone_available` | `true` when that is everyone available |
| `backup` | Who it wakes next, each with `name` and `sign_in_email_address`; present also when empty |
| `backup_is_everyone_available` | `true` when that is everyone else available |
| `reason` | Why coverage differs from the plain schedule, such as when nobody is scheduled; absent otherwise |
| `team_muted_until` | While the team is muted, when the mute ends: until then, no Pushover notification or automated voice call goes out, so an SOS wakes nobody |

## mmhotline schedules preview

Works out a settings file's schedule before it applies, and saves nothing.

```sh
sudo mmhotline schedules preview --settings-file /etc/mmhotline/mmhotline_settings.yml
```

```text
At 2026-11-15T06:18:01Z, primary: carlos@example.com; backup option: fixed_person; backup: barbara@example.com.
At 2026-11-16T14:00:00Z, primary: barbara@example.com; backup option: fixed_person; backup: barbara@example.com.
At 2026-11-21T14:00:00Z, primary: carlos@example.com; backup option: fixed_person; backup: barbara@example.com.
Schedule preview complete. Nothing was saved.
```

[Set up people and the on-call schedule](../how-to/set-up-people-and-the-on-call-schedule.md)
and [Write a custom schedule](../how-to/write-a-custom-schedule.md) use
it.

| Flag | Meaning |
| --- | --- |
| `--settings-file FILE` | Required. The file whose `on_call_schedule` to preview |
| Common flags | As above |

- **How it works:** it reads the file's `on_call_schedule` and asks the
  running MMHotline to work out the assignments.
- **Refused, exit 4,** for a file with problems, which it lists as
  [`settings check`](#mmhotline-settings-check) does; and for a sign-in
  email address the `people` part has not applied yet:
  `unknown sign-in email address dana@example.com; the schedule names only people already on the team, so apply the settings file with those people first`.
  Custom schedule code runs in its sandbox with its test cases, and a
  failed run or test case is refused with the reason, as `settings apply`
  refuses it.
- **The period:** the next 24 hours for hourly assignments, eight future
  handoffs for daily assignments and weekly rotations, and 14 days for
  custom code; the On call page covers the same.
- **It shows the schedule's own answer.** Personal unavailability and
  taking on call count only when an incident needs someone; see
  [`schedules show`](#mmhotline-schedules-show) for the live answer.

**JSON fields:** `message`, `web_url`, and `preview`, the current
assignment and each later change:

| Field | Meaning |
| --- | --- |
| `preview[].at` | When the change happens |
| `preview[].primary_sign_in_email_address` | Who is scheduled first; empty means nobody |
| `preview[].backup_option` | How the backup is chosen |
| `preview[].backup_sign_in_email_address` | The chosen or fixed backup; empty when the backup is everyone available |

## mmhotline down-alarm show

Shows the MMHotline-down alarm: its Worker, the last accepted check-in or
the failing check-ins, who it would wake, and what it last sent. The
**MMHotline-down alarm** is a Cloudflare Worker in the team's own
Cloudflare account that wakes the person on call when MMHotline stops
checking in with it.

```sh
sudo mmhotline down-alarm show
```

```text
MMHotline-down alarm Worker mmhotline-down-alarm-ops-example-com-7db8a22e at https://mmhotline-down-alarm-ops-example-com-7db8a22e.example.workers.dev.
Set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T07:46:30Z.
Last check-in accepted at 2026-11-15T07:46:30Z.
If MMHotline stops checking in: Pushover notification to Carlos Example (u...) after 10 minutes.
If MMHotline stops checking in: Automated voice call to Carlos Example (+12025550124) after 10 minutes.
If MMHotline stops checking in: Pushover notification to Barbara Example (u...) after 20 minutes.
```

[Set up the MMHotline-down alarm](../how-to/set-up-the-mmhotline-down-alarm.md)
sets it up with the settings file's `mmhotline_down_alarm` and uses both
`down-alarm` commands. The
[design](../changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md#the-mmhotline-down-alarm)
explains how it works.

- **Flags:** the common flags.
- Any member may show the alarm and send a test.
- Without the alarm, it prints `The MMHotline-down alarm is not set up.`

**JSON fields:** `message` and `mmhotline_down_alarm`; no `web_url`. Results
never include the Cloudflare API token, the check-in token, or a held
message's request.

| Field | Meaning |
| --- | --- |
| `mmhotline_down_alarm.is_set_up` | Whether the alarm is set up |
| `mmhotline_down_alarm.cloudflare_account_id`, `.worker_name`, `.worker_url` | The account, the Worker's name, and its `workers.dev` address |
| `mmhotline_down_alarm.set_up_at`, `.set_up_by` | When setup last ran, and `the settings file` and its path |
| `mmhotline_down_alarm.last_check_in_accepted_at` | The last accepted check-in since MMHotline started |
| `mmhotline_down_alarm.check_ins_failing_since`, `.last_check_in_error` | Present while check-ins fail: since when, and why |
| `mmhotline_down_alarm.held_messages` | Who would be woken: `person_name`, `contact_method_type`, `sent_to`, and `delay_minutes` |
| `mmhotline_down_alarm.recent_sends` | The sends the alarm reported in its answer to the last accepted check-in: `send_id`, `person_name`, `contact_method_type`, `due_at`, `send_outcome`, and `http_status_code` |
| `mmhotline_down_alarm.test_sends` | `send-test` only: each test's `person_name`, `contact_method_type`, `send_outcome`, `http_status_code`, and, for Pushover, the receipt fields [`send-test`](#mmhotline-down-alarm-send-test) lists |

`send_outcome` is `delivered`, `refused_by_service`,
`not_delivered_within_10_minutes`, `stopped_by_check_in`, or, for a test,
`no_answer` or `service_unavailable`.

## mmhotline down-alarm send-test

Checks in, then has the MMHotline-down alarm send a test of each held
message now, such as an emergency Pushover notification titled "Test of
the MMHotline-down alarm". For each Pushover notification, MMHotline then
reads Pushover's receipt, which proves that Pushover accepted the
notification, and cancels the notification's repeats at once.

THE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO.

The command prints that line first, before it sends anything. With `--json`
it goes to standard error, so standard output stays one JSON object.

```sh
sudo mmhotline down-alarm send-test
```

```text
Test: Pushover notification to Carlos Example: accepted by the service (HTTP 200). Pushover accepted the MMHotline-down alarm's notification (receipt r5qkvhu7wg8wwtbm4l5x2dnxzy9c3a). MMHotline cancelled its repeats.
Test: Automated voice call to Carlos Example: refused by the service (HTTP 401).
Test: Pushover notification to Barbara Example: refused by the service (HTTP 400).
The MMHotline-down alarm sent a test alert for each held message; 2 of 3 were not accepted, so those people would not be woken. Check the contact methods with sudo mmhotline contact-methods send-test.
```

The output starts with the lines of
[`down-alarm show`](#mmhotline-down-alarm-show). Each test line says what the
service answered. For a Pushover notification the service accepted, it adds
one of these sentences:

- `Pushover accepted the MMHotline-down alarm's notification (receipt R). MMHotline cancelled its repeats.`
- `Pushover's answer had no receipt, so nothing proves that Pushover accepted the notification.`
- `Pushover refused the receipt, so nothing proves that Pushover accepted the notification.`
  and Pushover's reason.
- `Pushover accepted the MMHotline-down alarm's notification (receipt R), but MMHotline could not cancel its repeats.`
  and the reason. THE PHONE MAY SOUND AGAIN FOR UP TO 10 MINUTES. Acknowledge
  the notification in Pushover.

A voice call's test line stays as it was. The last line is one of:

- `The MMHotline-down alarm sent a test alert for each held message, and each service accepted it.`
- `... and each service accepted it, but Pushover's receipt did not prove 1 of the Pushover notifications. Read the lines above for what to check.`
- `... 1 of 3 was not accepted, so that person would not be woken. Check the contact method with sudo mmhotline contact-methods send-test.`,
  or the line above for several.
- `The MMHotline-down alarm holds no message, so it would wake nobody.`

- **Flags:** the common flags.
- **JSON:** the fields of `down-alarm show`, with `test_sends`. Each test
  send has the fields below, besides those of `recent_sends`.

| Field of each `test_sends` entry | Meaning |
| --- | --- |
| `pushover_receipt` | The receipt in Pushover's answer; for a Pushover notification only, and absent when the answer had none |
| `pushover_receipt_outcome` | For a Pushover notification the service accepted: `accepted_and_repeats_cancelled`, `no_receipt`, `receipt_refused_by_pushover`, or `cancel_failed`. Absent for a voice call and for a notification the service refused |
| `pushover_receipt_problem` | Why a `receipt_refused_by_pushover` or `cancel_failed` happened; absent otherwise |

| Exit code | Meaning |
| --- | --- |
| `0` | Every test was accepted (`send_outcome` `delivered`), and each Pushover receipt was read and cancelled |
| `1` | A test was not accepted, a Pushover receipt did not prove the notification or its repeats were not cancelled, or the alarm holds no message |
| `4` | Refused, such as when the alarm is not set up, or wrong arguments |

## mmhotline status-page set-up

Creates the status page in the team's Cloudflare account, from a laptop,
and prints what the settings file needs next. The **status page** is a
public page where the team tells its customers about a problem.

```sh
mmhotline status-page set-up --cloudflare-account-id 0123456789abcdef0123456789abcdef \
  --host-name status.example.com --cloudflare-api-token-file ~/cloudflare-api-token
```

```text
The status page is set up in Cloudflare account 0123456789abcdef0123456789abcdef: the R2 bucket mmhotline-status-status-example-com-18a8ea11, the host name status.example.com connected to it, and the rule that serves index.html at https://status.example.com/.
r2_bucket_url: https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11
Next step: on Cloudflare's site, open R2, then Manage API tokens, and create an API token with the permission Object Read & Write for the bucket mmhotline-status-status-example-com-18a8ea11 only. Put r2_bucket_url above and the token's Access Key ID and Secret Access Key in the settings file's status_page section, as r2_bucket_url, r2_access_key_id, and r2_secret_access_key (with a deploy tool, in its vault), then apply the file, with your deploy tool or with sudo mmhotline settings apply on the MMHotline server. The first apply publishes the page.
```

[Publish a status page](../how-to/publish-a-status-page.md) runs it
step by step. The
[design](../changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md#the-status-page)
explains how the page works.

| Flag | Meaning |
| --- | --- |
| `--cloudflare-account-id ID` | Required. The team's Cloudflare account ID, 32 hexadecimal characters |
| `--host-name HOST` | Required. The page's public host name, in one of the account's zones |
| `--cloudflare-api-token-file FILE\|-` | Required. A file only its owner can read, or `-` for standard input, holding the person's own Cloudflare API token. A file other users can read is refused with the `chmod 600` command that fixes it |
| `--json` | Print one JSON object |
| `--help` | Show the flags |

- **Runs anywhere** with the `mmhotline` program; needs no MMHotline
  service and no command socket, and talks only to Cloudflare. It takes no
  `--command-socket`.
- **The token** needs Account › Workers R2 Storage › Edit, Zone › Zone ›
  Read, and Zone › Transform Rules › Edit, for the host name's zone. The
  command uses it only during setup and never stores it.
- **What it creates,** each only when missing: the R2 bucket, the host
  name's connection to it, and the URL rewrite rule. A rerun completes
  only what is missing. When Cloudflare refuses a step, it stops with
  Cloudflare's reason and exits 4; what it already created stays.
- **It uploads no first page,** because writing to the bucket needs the R2
  key the person makes next. The page's title is the settings file's
  `page_title`.
- **JSON fields:** `cloudflare_account_id`, `host_name`, `bucket_name`,
  `r2_bucket_url`, `page_url`, and `next_step`.

## mmhotline status-page show

Shows the status page's setup, its address, and its open and recently
resolved notices.

```sh
sudo mmhotline status-page show
```

```text
Status page: https://status.example.com/
R2 bucket: mmhotline-status-status-example-com-18a8ea11 (https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11)
Set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T07:51:11Z.
Example Inc. status, published at 2026-11-15T07:51:10Z.
No open notices: the page says that everything is working.
```

[Post to the status page](../how-to/post-to-the-status-page.md) uses
`show`, `post`, and `resolve`, on the server and from a laptop.

- **Flags:** the common flags.
- Any member may post.
- Without the status page, it prints `The status page is not set up.`

**From a laptop.** With these three environment variables set, `show`,
`post`, and `resolve` go straight to the R2 bucket, with no MMHotline
service and no command socket. Their output then starts with
`Status page in R2 bucket` and the bucket's URL.

| Environment variable | Holds |
| --- | --- |
| `MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL` | The bucket's URL for its API that works like Amazon S3's; `r2_bucket_url` in `status-page show` |
| `MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID` | The R2 API token's Access Key ID |
| `MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY` | The R2 API token's Secret Access Key |

The bucket URL alone is refused: the two keys are required with it.

**JSON fields:** `message` and `status_page`; no `web_url`. `post` and
`resolve` also return `notice_id`, the notice they changed. Results never
include the R2 Secret Access Key or an API token.

| Field | Meaning |
| --- | --- |
| `status_page.is_set_up` | Whether the settings file's `status_page` section applied on this MMHotline; `false` from a laptop |
| `status_page.page_url` | The page's address, such as `https://status.example.com/` |
| `status_page.host_name`, `.cloudflare_account_id` | The settings file's `host_name`, and the account that `r2_bucket_url` names |
| `status_page.bucket_name`, `.r2_bucket_url` | The R2 bucket and its S3 API address |
| `status_page.r2_access_key_id` | The saved R2 Access Key ID; absent before the section applied |
| `status_page.set_up_at`, `.set_up_by` | When the section last applied, and `the settings file` and its path |
| `status_page.published_status_json` | The published `status.json`: `page_title`, `published_at`, and `notices` ([its format](../changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md#two-files-in-an-r2-bucket)) |
| `status_page.read_error` | Why `status.json` could not be read, or why `index.html` may not show a saved post until the next post |

## mmhotline status-page post

Opens a notice on the status page, or adds an update to an open notice.

```sh
sudo mmhotline status-page post --message "We are looking into a problem."
sudo mmhotline status-page post --notice-id 1 --message "Sign-in works again for most customers."
```

The output is that of [`status-page show`](#mmhotline-status-page-show),
then `Posted to notice 1.`, or `Posted to notice 1, directly in the R2
bucket.` from a laptop.

| Flag | Meaning |
| --- | --- |
| `--message TEXT` | Required. 1 to 5,000 characters |
| `--notice-id N` | Optional. Add the message as an update to open notice N; without it, a new notice opens |
| Common flags | As above |

- Each post uploads `status.json` and `index.html` to the bucket.
- From a laptop, a post is refused while the bucket has no `status.json`,
  which the first `settings apply` with the `status_page` section
  creates.

## mmhotline status-page resolve

Resolves a notice, with an optional last update.

```sh
sudo mmhotline status-page resolve --notice-id 1 --closing-message "Everything works again."
```

The output is that of [`status-page show`](#mmhotline-status-page-show),
then `Notice 1 is resolved.`, or `Notice 1 is resolved, directly in the R2
bucket.` from a laptop.

| Flag | Meaning |
| --- | --- |
| `--notice-id N` | Required. The notice to resolve |
| `--closing-message TEXT` | Optional. Added as the notice's last update |
| Common flags | As above |

## Commands that read what happened

The `incidents`, `alerts`, `triage-sessions`, and `approval-requests`
commands let a person, or the team's coding agent, read what happened over
SSH on the MMHotline server. The pages sit behind Cloudflare Access, which
an SSH session cannot pass. Each command shows the facts of a page and
calls the same operation that page calls: the dashboard and incident page,
**Alerts**, the incident page's triage agent panel, and **Approvals**.

- Except `incidents send-test-sos`, they change nothing. To claim, resolve,
  approve, refuse, or stop, use the pages, because each of those actions
  records a person.
- Each reads in at most a few transactions the size of a page's, so it
  holds the database no longer than opening the page does.
- They need root's command access; any member sees the same facts on the
  pages.
- `TIME` is an RFC 3339 time, such as `2026-10-28T09:00:00Z` or
  `2026-10-28T11:00:00+02:00`. A span includes its start and excludes its
  end.

## mmhotline incidents list

Lists SOS incidents: by default every one not resolved, newest first, as
the dashboard does.

```sh
sudo mmhotline incidents list
```

```text
SOS incident acc896d49ce5b2ef40cd2c944da4f08f: Nightly backup of db001 failed
Status: Not claimed yet. Opened at 2026-11-15T04:40:40Z.
Being contacted now: Barbara Example.
Page: https://ops.example.com/incidents/acc896d49ce5b2ef40cd2c944da4f08f

Listed 1 SOS incident not resolved, newest first.
```

[Handle an incident](../how-to/handle-an-incident.md) and
[Back up and restore the database](../how-to/back-up-and-restore-the-database.md#restore)
use it.

| Flag | Meaning |
| --- | --- |
| `--opened-since TIME` | Optional. Every incident opened at or after `TIME`, resolved or not |
| `--opened-before TIME` | Optional. Every incident opened before `TIME`, resolved or not |
| Common flags | As above |

With one or both time flags, it lists the incidents opened in that span,
at most the newest 1,000, and ends with a line such as
`Listed 2 SOS incidents opened since 2026-10-28T00:00:00Z, resolved or not, newest first.`

**JSON fields:** `message`, `web_url`, and `incidents`, present also when
empty. Each incident:

| Field | Meaning |
| --- | --- |
| `incident_id`, `title`, `message` | The incident, and its opening alert's title and message |
| `status` | `open` (shown as "Not claimed yet"), `claimed`, or `resolved` ([states](severities-and-incident-states.md)) |
| `opened_at`, `claimed_at`, `resolved_at` | When it opened, was last claimed, and was resolved; absent until then |
| `claimed_by` | The name of the person who claimed it; absent while nobody has |
| `escalation_step` | `primary`, `backup`, `everyone_available`, `only_member`, `chosen_person`, or `triage_agent`, the step that waits for the [triage agent](../changing-mmhotline/design/incidents.md#triage-sessions) |
| `held_for_triage_agent` | `true` while the incident waits for the triage agent and nobody is contacted yet |
| `being_contacted_now` | While open: who is contacted, such as `Barbara Example` or `No one yet: the triage agent is working on it` |
| `next_person_contacted_at` | While open: when MMHotline contacts the next person unless someone claims it, or, while held, when it wakes the person on call; absent when there is no next step |
| `opening_alert_id`, `alert_source_id` | The alert that opened it, and its alert source |
| `sender_incident_key` | The sender's incident key; absent when the sender gave none |
| `page_url` | The incident page |

## mmhotline incidents show

Shows one SOS incident with its message, its triage session, its action
history, and its newest 100 alerts.

```sh
sudo mmhotline incidents show --incident-id acc896d49ce5b2ef40cd2c944da4f08f
```

The output starts with the lines of `incidents list`, then adds one line
each for the message and the triage session, then:

```text
Action history:
  2026-10-28T01:01:03Z Opened: SOS incident opened: Nightly backup of db001 failed. By: Backups.
  2026-10-28T01:01:03Z Contacting: Contacting Barbara Example (only team member). By: MMHotline.
Related alerts, newest first:
  2026-10-28T01:01:03Z SOS 02692794cbcd552987b12e1bbe245467: Nightly backup of db001 failed (SOS incident cd1eec890f8a52f59c04f3f6982767a2)
```

A claim or resolve with a note ends with the note, such as
`Resolved: Resolved by Barbara Example. Note from Barbara Example: “This was a test.” By: Barbara Example.`
[Handle an incident](../how-to/handle-an-incident.md) uses it.

| Flag | Meaning |
| --- | --- |
| `--incident-id ID` | Required |
| Common flags | As above |

**JSON fields:** `message`, `web_url`, and `incident`, with the fields of
[`incidents list`](#mmhotline-incidents-list) and:

| Field | Meaning |
| --- | --- |
| `triage_session_id` | The triage session the incident's newest alert joined, as the incident page shows; absent when none did |
| `alerts` | The incident's newest 100 alerts, newest first, in the fields of [`alerts list`](#mmhotline-alerts-list). The message names the command for older ones |
| `history` | The incident's history, oldest first, which the incident page's **What happened** says in words |
| `history[].kind`, `history[].label` | Such as `claimed` or `triage_agent_report`, and a short name, such as `Triage agent report` |
| `history[].message`, `history[].occurred_at` | What happened, and when |
| `history[].by` | The person, alert source, triage agent, or `MMHotline` |
| `history[].related_alert_id` | The alert that caused the entry, when one did |
| `history[]` with `kind` `pushover_acknowledgement_did_not_claim` | A Pushover acknowledgement came from another Pushover user, so it claimed nothing: `Barbara Example's phone acknowledged, but the acknowledgement came from another Pushover user, so it did not claim the incident. The contact method uses a key that isn't Barbara Example's own Pushover user key, such as a group key.` Its time is when MMHotline sent the message |
| `history[].note` | What the person wrote with a claim or resolve; absent when they wrote none |

## mmhotline incidents send-test-sos

Has MMHotline send itself a test SOS, wakes the person on call as a real
SOS does, and waits for someone to acknowledge it on the phone. Use it to
prove that an SOS reaches a phone. It is the one command of this group that
opens an incident.

```sh
sudo mmhotline incidents send-test-sos
```

```text
MMHotline sent a test SOS to the person on call. Waiting up to 10 minutes for someone to acknowledge it on the phone.
Incident: https://ops.example.com/incidents/acc896d49ce5b2ef40cd2c944da4f08f
Barbara Example claimed the test by acknowledging the alert in Pushover, at 2026-11-15T04:41:20Z.
MMHotline resolved the test after the claim, at 2026-11-15T04:41:20Z.
The test SOS worked: a person acknowledged it on the phone.
```

What the test does:

- It opens an SOS incident titled `Test of the alerts. Nothing is wrong:
  acknowledge this.` Its alert source ID is `mmhotline_test_sos`, which
  marks it as a test. Only a test resolves itself.
- It pages whoever is on call exactly as a real SOS does: the same contact
  methods, repeats, and backup timing.
- It skips the triage agent. Nothing is wrong, so the agent has nothing to
  look into, and the test must not wait for it.
- When someone claims the test, MMHotline resolves it at once and writes
  `Resolved by MMHotline after the claim, because this incident was a test`
  into its history. Acknowledging in Pushover, the page, Telegram, and
  Slack all count as a claim. MMHotline sends nobody a message that the
  test is resolved.
- A test that nobody claimed stays open. The next `send-test-sos` resolves
  it first, so two tests never ring a phone at once.
- It does not pass through MMWatchdog or any alert source. MMWatchdog's
  signed check-ins prove that path every minute, and MMHotline warns when
  they stop.

| Flag | Meaning |
| --- | --- |
| `--wait-for-claim-minutes N` | Optional. How long to wait for a claim. A whole number from 1 up. Default 10 |
| Common flags | As above |

Reason for the default: it gives the person on call a whole step of the
default time to claim (5 minutes) and the start of the backup's step. A
phone that has not sounded by then has a setup problem, and waiting longer
does not fix it. A team that sets a longer time to claim can raise the
limit. The minimum of 1 is there because 0 would wait for nothing.

IF NOBODY CLAIMS THE TEST IN TIME, THE COMMAND EXITS 1 AND THE INCIDENT STAYS
OPEN. CHECK FIRST THAT THE PUSHOVER APP IS INSTALLED AND SIGNED IN ON THE
PHONE, AND THAT CRITICAL ALERTS ARE ALLOWED FOR PUSHOVER IN THE PHONE'S
SETTINGS.

```text
Nobody acknowledged the test SOS within 10 minutes. The incident is still open, so MMHotline keeps trying to reach the person on call. Check first that the Pushover app is installed and signed in on the phone, and that critical alerts are allowed for Pushover in the phone's settings. Then run the command again: a new test closes this one.
```

**JSON fields:** `message`, `web_url`, and `test_sos`:

| Field | Meaning |
| --- | --- |
| `test_sos.incident_id`, `.incident_page_url` | The test incident, and its page |
| `test_sos.waited_for_claim_minutes` | The time limit the command used |
| `test_sos.claimed` | `true` when a person claimed the test in time |
| `test_sos.claimed_by`, `.claimed_how`, `.claimed_at` | Who claimed it, how (such as `by acknowledging the alert in Pushover`; absent for the incident page), and when |
| `test_sos.resolved_at` | When MMHotline resolved the test after the claim |

| Exit code | Meaning |
| --- | --- |
| `0` | A person claimed the test, and MMHotline resolved it |
| `1` | Nobody claimed the test within the time limit, or someone resolved it without claiming it |
| `4` | Refused, or the command socket does not answer |
| `130` | Interrupted, such as with Ctrl-C. The test stays open until the next test or someone resolves it |

## mmhotline alerts list

Lists the newest 100 alerts that match, newest first. Warn and info alerts
open no incident, so this is where to find them.

```sh
sudo mmhotline alerts list --severity warn
```

```text
2026-11-15T08:51:45Z Warning 048bd52b16047999b0cee4512075e678: The AI triage agent reports: Disk check playbook (PR 7) looks harmless, but a person must review it: my rules file is missing
```

That is the first line of one alert. Each alert has a line with its
received time, severity, ID, and title, then a line with its alert source
and page, then its message. The list ends with a line such as
`Listed 1 alert, newest first.`
[Back up and restore the database](../how-to/back-up-and-restore-the-database.md#restore)
uses it.

| Flag | Meaning |
| --- | --- |
| `--severity SEVERITY` | `info`, `warn`, `sos`, `resolved-info`, `resolved-warn`, or `resolved-sos` ([severities](severities-and-incident-states.md)) |
| `--alert-source-id ID` | An alert source's ID from `alert-sources list`; `system` for MMHotline's own alerts, such as the triage agent's reports; `manual` for SOS incidents a person created; `mmhotline_test_sos` for the test SOS of `incidents send-test-sos`. Another ID is refused, because an empty list would look like a source that went quiet |
| `--incident-id ID` | Only the alerts of this SOS incident |
| `--received-since TIME`, `--received-before TIME` | Only alerts received in that span |
| `--older-than-alert-id ID` | Continue after a list of 100: give the oldest listed alert's ID, which the list's message names. Reason: a burst arrives within one second, and a time would skip or repeat alerts in it |
| Common flags | As above |

**JSON fields:** `message`, `web_url`, and `alerts`, present also when
empty. Each alert:

| Field | Meaning |
| --- | --- |
| `alert_id`, `severity`, `title`, `message` | The alert as received |
| `notes` | MMHotline's note about the alert, such as a default severity; absent when none |
| `received_at` | When MMHotline received it |
| `happened_at` | When it happened on the sender's clock; only when the sender gave the time |
| `alert_source_id` | The alert source's ID, or `system` or `manual` |
| `incident_id` | The SOS incident it opened or joined; absent when none |
| `sender_alert_id`, `sender_incident_key` | The sender's IDs, when it gave them |
| `page_url` | The alert's page |

## mmhotline triage-sessions list

Lists the newest 50 triage sessions, newest first. A **triage session** is
the triage agent's work on a stretch of trouble: the alerts, and the runs
in which the agent looked into them
([design](../changing-mmhotline/design/triage-agent.md#triage-sessions)).

```sh
sudo mmhotline triage-sessions list
```

```text
Triage session 89fda90d8160696485d427a1d654db1c: Waiting: the triage agent is not running. MMHotline resumes the triage agent when another alert or the result of a playbook run arrives.
Started at 2026-11-15T08:56:16Z. Alerts: 2. Runs: 2. SOS incidents: none.
Listed 1 triage session, newest first.
```

[Set up the triage agent](../how-to/set-up-the-triage-agent.md#what-a-triage-session-looks-like)
reads sessions with both commands.

- **Flags:** the common flags.
- An older incident's session: `incidents show` gives its
  `triage_session_id`.

**JSON fields:** `message`, `web_url`, and `triage_sessions`, present also
when empty. Each session:

| Field | Meaning |
| --- | --- |
| `triage_session_id`, `started_at` | The session, and when its first alert started it |
| `ended_at`, `ended_because` | When and why it ended; absent while open |
| `phase` | `working`, `waiting`, `stopping`, `handing_over`, or `ended`, as the [incident page](../changing-mmhotline/design/incidents.md#the-triage-agent-on-the-incident-page) names them |
| `phase_text` | The session page's line for the phase |
| `taken_over` | Who pressed Stop and when, such as `Barbara pressed Stop at 2026-11-03 03:10:00 UTC`; absent when nobody did |
| `hand_over_run_id` | The run in which the agent handed over after a Stop; absent before |
| `alert_count`, `run_count` | How many alerts and runs |
| `incident_ids` | The session's SOS incidents, by the last alert from each to join the session, newest first |

## mmhotline triage-sessions show

Shows one triage session with its alerts, its SOS incidents, and its runs,
each run with its latest 50 steps.

```sh
sudo mmhotline triage-sessions show --triage-session-id 89fda90d8160696485d427a1d654db1c
```

```text
Triage session 89fda90d8160696485d427a1d654db1c: Waiting: the triage agent is not running. MMHotline resumes the triage agent when another alert or the result of a playbook run arrives.
Started at 2026-11-15T08:56:16Z. Alerts: 2. Runs: 2. SOS incidents: none.
Alerts, oldest first:
  2026-11-15T08:56:15Z Warning ecbc50fea74cc1b695d8ec8028355a67: lab-web.service on web1 is not running; sent in run b52fc544df25dfe3d85c77343caee088
  2026-11-15T08:58:28Z Warning dc0af42deeb7f2f68967254e5bf19676: lab-web.service on web1 is still not running; sent in run 8a3b1687b4e4087ff040ad6987ae1dc2
SOS incidents:
Runs, oldest first:
  Run b52fc544df25dfe3d85c77343caee088: finished. Started at 2026-11-15T08:56:16Z, ended 2026-11-15T08:57:10Z. Cost in US dollars: 0.233324
    2026-11-15T08:56:19Z command: cat triage_agent_rules.md; echo ----; cat AGENTS.md; ls -la; ...
```

| Flag | Meaning |
| --- | --- |
| `--triage-session-id ID` | Required |
| Common flags | As above |

Each run shows its latest 50 steps, as the incident page does; the coding
agent's session files keep the whole transcript.

**JSON fields:** `message`, `web_url`, and `triage_session`, with the
fields of [`triage-sessions list`](#mmhotline-triage-sessions-list) and:

| Field | Meaning |
| --- | --- |
| `alerts` | The session's alerts, oldest first, in the fields of `alerts list`, with `joined_at` and `sent_in_run_id`, the run that carried the alert to the agent, absent while the alert waits for the next run |
| `incidents` | The session's SOS incidents, in the fields of `incidents list` |
| `runs` | The session's runs, oldest first, in the fields of `triage-agent show`'s `latest_setup_test`: `id`, `purpose` (`triage_session`), `state`, `started_at`, `ended_at`, `failure_reason`, the run's own `cost_in_us_dollars` or tokens, `steps` with the latest 50, and `earlier_steps_not_shown` |

## mmhotline approval-requests list

Lists the playbook runs that wait for a person, newest first, as the
**Approvals** page does: open approval requests, and warned runs that have
not started. An **approval request** is a playbook run the triage agent
asked for that waits for a person, because its
[rule](../changing-mmhotline/design/triage-agent.md#rules-for-running-the-agents-playbooks)
is "Get permission, then act". A warned run, under "Warn, wait, then act",
is listed so that a person can stop it before it runs.

```sh
sudo mmhotline approval-requests list
```

```text
Playbook run e63b3c6fe869617623da0ebcb22d1fba: one_time_playbooks/2026-11-15_install_security_fixes_brotli_openssl_expat_apt_sasl_on_web1.yml in the inventory “production”. Waiting for approval. Asked at 2026-11-15T09:16:02Z.
Listed 1 playbook run waiting for a person, newest first. Team members approve, refuse, or stop playbook runs at https://ops.example.com/approvals.
```

[Decide what the triage agent may do](../how-to/decide-what-the-triage-agent-may-do.md#approve-or-refuse-a-playbook-run)
approves and refuses on the page.

- **Flags:** the common flags.
- **JSON:** `message`, `web_url`, and `approval_requests`, present also
  when empty, each in the fields of
  [`approval-requests show`](#mmhotline-approval-requests-show).

## mmhotline approval-requests show

Shows one approval request or warned run, waiting or decided, with what it
would run.

```sh
sudo mmhotline approval-requests show --approval-request-id e63b3c6fe869617623da0ebcb22d1fba
```

```text
Playbook run e63b3c6fe869617623da0ebcb22d1fba: one_time_playbooks/2026-11-15_install_security_fixes_brotli_openssl_expat_apt_sasl_on_web1.yml in the inventory “production”. Refused. Asked at 2026-11-15T09:16:02Z.
Rule: Get permission, then act (1 approval). Commit: cc72003b1e209a56dd79d94ca48972db9d58ba69.
SOS incident: f9a143aca619c1e2a0d8a555e9f04548
Kinds of action in the playbook: Check only, Unrecognized task.
What the playbook can change on each server, as listed by mmx explain:
  install_security_fixes_on_web1/task_1 on web1: assert (core), ready
...
```

After those lines come the triage session, "Why the triage agent asks"
with the agent's diagnosis, the playbook's text at the checked commit, and
the Approvals page's address.

| Flag | Meaning |
| --- | --- |
| `--approval-request-id ID` | Required |
| Common flags | As above |

**JSON fields:** `message`, `web_url`, and `approval_request`:

| Field | Meaning |
| --- | --- |
| `approval_request_id`, `playbook`, `inventory`, `commit_id` | The request, and the playbook and inventory at the commit MMHotline judged |
| `rule` | `get_permission_then_act`, with `approvals_needed`, 1 or 2; or `warn_wait_then_act`, with `warning_wait_minutes` and `runs_at` |
| `listed_playbook_rule` | When the rule is one a person listed for the playbook by name, says so, such as "The playbook reboot_servers_after_updates is listed in rules_for_running_agent_playbooks for the inventory “servers” with Warn, wait 10 minutes, then act."; absent otherwise. The text adds it after `Rule:` |
| `its_approval_also_approves_in_the_same_session` | The listed playbooks this approval also approves once later in its triage session ([one approval for a known sequence](../changing-mmhotline/design/triage-agent.md#one-approval-for-a-known-sequence)); absent when none. The text adds "Its approval also approves, once each later in the same triage session: …" |
| `covered_by_approval_request_id`, `covered_by` | For a run an earlier approval covered: that approval request, and who approved it; absent otherwise. The text adds "Approved without a new request: …" |
| `state` | `open`, `approved`, `refused`, `stopped`, `running`, `ran_and_succeeded`, `ran_and_failed`, `could_not_run`, or `stale` ([states](../changing-mmhotline/design/triage-agent.md#what-mmhotline-keeps-for-approvals)) |
| `state_label` | The state in words, such as `Waiting for approval` or `Runs in 7 minutes unless someone presses Stop` |
| `asked_at`, `state_changed_at` | When MMHotline asked, and when the state last changed |
| `incident_id`, `triage_session_id` | The incident of the request's alert, and the triage session that asked; absent when none |
| `kinds_of_action` | What the playbook does, such as `reboot_a_server` |
| `tasks` | What the playbook would run, as `mmx explain` lists it: `path`, `node`, `module`, `module_kind`, `status`, and `arguments` with `field`, `value`, and `status` |
| `unrecognized_tasks` | What MMHotline did not recognize: `path`, `module` (or `save_on_controller_machine` for a file a fleet stage saves on MMHotline's server), and `reason`, such as `systemd_service.start_at_boot is not an allowed argument for Start or restart a service (…)` |
| `tasks_on_services_whose_changes_a_person_must_approve` | The tasks on [services whose changes a person must approve](../changing-mmhotline/design/triage-agent.md#services-whose-changes-a-person-must-approve), with the current list: `path`, `node`, `module`, `service_name` (absent when MMX knows the unit's name only during the run), and `reason`. Present also when empty |
| `playbook_text` | The playbook's text at the commit |
| `review_agent_answer` | The review agent's answer, when one ran |
| `diagnosis` | The triage agent's Markdown explanation: findings, cause, evidence, and why the playbook fixes the problem |
| `earlier_versions_refused_by_the_review_agent` | When the review agent's refusals made the request wait, the versions it refused in a row before this one, oldest first ([refusal rounds](../changing-mmhotline/design/triage-agent.md#refusal-rounds)): `playbook`, `inventory`, `commit_id`, `diagnosis`, `review_agent_reason`, and `playbook_text`. Present also when empty. The text lists them under "Earlier versions the review agent refused, oldest first" |
| `decisions` | Each person's `member_name`, `decision` (`approve` or `refuse`), `reason`, and `decided_at` |
| `stopped_by`, `stop_reason` | For a warned run a person stopped |
| `result_message` | What happened, once decided or run |
| `result_for_the_agent` | `none`, `waits_for_the_next_run`, `sent`, or `could_not_reach_the_agent` |

## mmhotline commit-reviews check

Checks every new commit of the systems repository through a commit, as
MMHotline does before each MMX run and each triage agent run in the
repository ([the check](../changing-mmhotline/design/triage-agent.md#checking-commits-before-mmx-and-triage-agent-runs)).

```sh
sudo mmhotline commit-reviews check --commit-id 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1
```

```text
Checked commit 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1.
Trusted through commit a4fce954720b97d948bdb9f80640cbc5ee5ede5e: every commit through it passed the check or a person reviewed it.
Commit 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1, which no key the team trusts signed.
Author: Git host <noreply@githost.example>. Commit date: 2026-11-15T08:51:26Z. Found by MMHotline: 2026-11-15T08:51:26Z.
Message: Merge pull request 7: add a disk check playbook
Waits for review at https://ops.example.com/approvals/commits/5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1.
1 commit waits for a person's review at https://ops.example.com/approvals.
```

[Review a commit](../how-to/review-a-commit.md) uses every
`commit-reviews` command.

| Flag | Meaning |
| --- | --- |
| `--commit-id COMMIT_ID` | Required. A full commit ID, such as the commit `util01` is about to apply |
| Common flags | As above |

- **The rule:** a commit signed by a key in
  `triage_agent.trusted_commit_signing_public_keys` passes. A commit the
  triage agent's key signed passes when it changes only the agent's two
  folders. Every other commit waits for a review on the Approvals page.
  With `triage_agent.commits_without_a_trusted_signature_are_approved_by:
  a_person_or_the_triage_agent`, the triage agent reviews a commit no
  trusted key signed that changes no path of
  `triage_agent.paths_only_a_person_may_approve`, and approves it or
  decides that a person must review it
  ([design](../changing-mmhotline/design/triage-agent.md#the-triage-agent-reviews-a-commit-no-trusted-key-signed)).
- **What it does:** fetches origin into MMHotline's copy under the copy's
  lock, as runs do, then checks every commit from the last checked commit
  through `COMMIT_ID`. Each commit that newly waits gets one alert:
  MMHotline's warning at once, or, for a commit the triage agent reviews,
  the agent's approval or report.
- **Waits** without a time limit while an MMX run or a person's helper
  holds the copy's lock, `/var/lib/mmhotline/systems_repository.lock`. A
  caller that holds the lock itself must release it first.
- **Refused** for a `COMMIT_ID` the copy lacks after the fetch.
- A scheduled job on the server, such as `util01`'s job that applies the
  systems repository, runs `check` before applying a commit, and applies
  only through `trusted_through_commit_id`.

| Exit code | Meaning |
| --- | --- |
| `0` | Every commit through `COMMIT_ID` passed or was reviewed |
| `1` | A commit waits for review; the answer is printed first |
| `4` | Refused, or wrong arguments |

**JSON fields:** `message`, `web_url`, and `commit_check`:

| Field | Meaning |
| --- | --- |
| `commit_check.checked_commit_id` | The commit the check went through |
| `commit_check.trusted_through_commit_id` | The newest commit on the checked commit's first-parent line through which every commit passed or was reviewed: the checked commit when nothing waits, the commit before a merge that brings in a waiting commit, and empty when no commit is trusted yet |
| `commit_check.commits_waiting_for_review` | The commits through the checked commit that wait, newest first, in the fields of [`commit-reviews show`](#mmhotline-commit-reviews-show) without `diff`. Present also when empty |

## mmhotline commit-reviews trust-through

Trusts a commit of origin's branch and every commit before it, such as
after a person's first run on a new or rebuilt server.

```sh
sudo mmhotline commit-reviews trust-through --commit-id a4fce954720b97d948bdb9f80640cbc5ee5ede5e
```

[Review a commit](../how-to/review-a-commit.md#vouch-for-the-history-once)
uses it.

| Flag | Meaning |
| --- | --- |
| `--commit-id COMMIT_ID` | Required. A full commit ID of origin's branch |
| Common flags | As above |

- Fetches origin without checking, then makes `COMMIT_ID` the
  checked-through commit. Each commit before it that waited is recorded as
  reviewed by the Unix user through the trust-through, and one info alert
  tells the team, such as
  `root on util01 marked every commit through abc1234 trusted`. The
  checkout then moves to the trusted commit, and later commits go through
  the check.
- A `COMMIT_ID` at or before the checked-through commit changes nothing,
  and the message says so.
- Refused for a `COMMIT_ID` the copy lacks after the fetch, or one not on
  origin's branch.
- Waits for the copy's lock, answers, and exits as `check` does, and
  returns `commit_check`, with `message` saying what changed.

## mmhotline commit-reviews mark-reviewed

Marks a waiting commit reviewed, as the page's **Mark commit reviewed**
does. It is recorded as "the mmhotline command, run by barbara".

```sh
sudo mmhotline commit-reviews mark-reviewed --commit-id 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1
```

- **Flags:** `--commit-id COMMIT_ID`, required, and the common flags.
- Marking a reviewed commit changes nothing.
- **JSON:** `message`, `web_url`, and `commit_review`, in the fields of
  [`commit-reviews show`](#mmhotline-commit-reviews-show).

## mmhotline commit-reviews list

Lists the commits waiting for review, oldest found first, then the latest
50 commits the triage agent approved, newest first, as the Approvals page
lists them. It changes nothing.

```sh
sudo mmhotline commit-reviews list
```

- **Flags:** the common flags.
- **Output:** each commit as [`commit-reviews show`](#mmhotline-commit-reviews-show)
  prints it, without the diff.
- **JSON:** `message`, `web_url`, `commit_reviews`, and
  `commits_the_triage_agent_approved`, newest first, each present also
  when empty, in the fields of `commit-reviews show`.

## mmhotline commit-reviews show

Shows one commit that waits or waited, with its diff, who reviewed it, and
the triage agent's decision and report. It changes nothing.

```sh
sudo mmhotline commit-reviews show --commit-id 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1
```

```text
Commit 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1, which no key the team trusts signed.
Author: Git host <noreply@githost.example>. Commit date: 2026-11-15T08:51:26Z. Found by MMHotline: 2026-11-15T08:51:26Z.
Message: Merge pull request 7: add a disk check playbook
Waits for review at https://ops.example.com/approvals/commits/5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1.
The triage agent decided that a person must review the commit, with its report at alert severity warn at 2026-11-15T08:51:45Z, in commit review run a9653b004da1f20f02a6cc2473c5da73. Its report:
...
Diff:
diff --git a/playbooks/check_disk.yml b/playbooks/check_disk.yml
...
```

| Flag | Meaning |
| --- | --- |
| `--commit-id COMMIT_ID` | Required |
| Common flags | As above |

**JSON fields:** `message`, `web_url`, and `commit_review`:

| Field | Meaning |
| --- | --- |
| `commit_id` | The commit's full ID |
| `reason_kind` | `not_signed_by_a_trusted_key`: unsigned, signed by a key the settings file does not list, or a signature Git cannot verify. `triage_agent_change_needs_review`: signed by the triage agent's key, and changes paths outside its folders or adds a file type it may not add |
| `reason` | The reason in words, such as `which no key the team trusts signed` |
| `author`, `committed_at`, `message` | The commit's author, date, and message |
| `paths_that_need_review` | Each `path` and `reason`: for `triage_agent_change_needs_review`, the paths outside the agent's folders; for a commit no trusted key signed, the changed paths of `triage_agent.paths_only_a_person_may_approve`, as applied now; otherwise empty |
| `changed_paths` | Every path the commit changes, against each parent of a merge |
| `found_at` | When MMHotline first found the commit |
| `reviewed_by`, `reviewed_at` | Who reviewed it and when, such as `Barbara Example`, `the mmhotline command, run by barbara`, `the mmhotline command, run by root, marking every commit through abc1234 trusted`, or `the triage agent`; absent while it waits |
| `reviewer_kind` | `member`, `unix_user`, `trust_through`, or `triage_agent`; absent while it waits |
| `triage_agent_review` | The triage agent's latest decision; absent when it gave none |
| `triage_agent_review.run_id`, `.decided_at` | The commit review run, and when MMHotline recorded the decision |
| `triage_agent_review.decision` | `approve`, or `needs_a_person`, which leaves the commit waiting |
| `triage_agent_review.alert_severity` | With `needs_a_person`, the severity the report went out at; absent with `approve`, which sends one info alert |
| `triage_agent_review.report` | The agent's whole report |
| `review_url` | The commit's page under Approvals |
| `diff`, `diff_truncated` | `show` and `mark-reviewed` only: the diff against each parent, cut after 128 KiB with `diff_truncated` `true` |
