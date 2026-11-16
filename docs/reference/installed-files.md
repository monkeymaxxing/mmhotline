# Installed files

This page lists every file, directory, Unix user, systemd unit, and socket
that `sudo mmhotline install` creates or uses on the MMHotline server, with
its owner and mode, what it holds, and who reads it. It also lists the
files the continuous backup adds and the files a person adds by hand to
prepare the triage agent. A **unit** is a systemd service or socket file
under `/etc/systemd/system`. A **socket** here is a Unix socket file
through which one process asks another for something. To install step by
step, see [Install MMHotline](../how-to/install.md).

## What install prints

`mmhotline install` checks each item and prints one line for it: what it
changed, or that the item was already correct. An unchanged rerun writes
no file and leaves running services alone. A changed program or unit, a
new database, or a corrected settings file restarts `mmhotline.service`,
and so does a stopped `mmhotline.service`. Install then waits up to 30
seconds for the command socket to answer. When MMHotline does not start,
install prints the last 30 lines of the service's journal.

On a new server, from a real run, the first lines are:

```text
Checks: Linux, root, and systemd confirmed
Unix user: created mmhotline with no login shell
Unix user: created mmhotline-triage-agent with no login shell
Unix user: created mmhotline-review-agent with no login shell
Triage agent home: installed or corrected /var/lib/mmhotline-triage-agent
Service: mmhotline-triage-agent-run.socket unit updated; enabled; restarted
Service: mmhotline-review-agent-run.socket unit updated; enabled; restarted
Service: mmhotline-fleet-view.socket unit updated; enabled; restarted
Binary: installed or corrected /usr/local/bin/mmhotline
Data directory: installed or corrected /var/lib/mmhotline
Database: created mmhotline.db for Operations
Settings file: set owner root, group mmhotline, and mode 0640, so MMHotline can read the settings file and only root can change the settings file: /etc/mmhotline/mmhotline_settings.yml
```

Then come a line for `mmhotline.service`, such as
`Service: mmhotline unit updated; enabled; restarted`, the `Backup:` line,
the `MMHotline:` line with `web_url`, and, without `cloudflare_access` in
the settings file, a `Next:` line.

An unchanged rerun, from a real run:

```text
Checks: Linux, root, and systemd confirmed
Unix user: mmhotline already exists
Unix user: mmhotline-triage-agent already exists
Unix user: mmhotline-review-agent already exists
Triage agent home: already correct (/var/lib/mmhotline-triage-agent)
Service: mmhotline-triage-agent-run.socket unit already correct; already enabled; already running
Service: mmhotline-review-agent-run.socket unit already correct; already enabled; already running
Service: mmhotline-fleet-view.socket unit already correct; already enabled; already running
Binary: already correct (/usr/local/bin/mmhotline)
Data directory: already correct (/var/lib/mmhotline)
Database: mmhotline.db already exists; kept
Settings file: already has owner root, group mmhotline, and mode 0640: /etc/mmhotline/mmhotline_settings.yml
Service: mmhotline unit already correct; already enabled; already running
Backup: not set up; see https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/back-up-and-restore-the-database.md#set-up
MMHotline: https://ops.example.com
Next: set up cloudflared and a Cloudflare Access application for https://ops.example.com, add cloudflare_access to /etc/mmhotline/mmhotline_settings.yml, and apply the file, with your deploy tool or with sudo mmhotline settings apply (https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/put-mmhotline-behind-cloudflare-access.md)
```

With the backup set up, the `Backup:` line is
`Backup: Litestream service configured; check that uploads keep up with sudo mmhotline backup check-uploads`,
after a `Service: mmhotline-backup ...` line.

Before it changes anything, install refuses to run without Linux, root,
systemd at `/run/systemd/system`, or a settings file that passes the
check. Install also refuses a symbolic link, or a file of another kind, at
any path below where it expects a regular file or a directory, and says
to move it aside.

## Unix users

All four users are system users with the shell `/usr/sbin/nologin`.
Install creates each one that is missing and keeps an existing one.

### mmhotline

| Fact | Value |
| --- | --- |
| Created with | `groupadd --system --force mmhotline`, then `useradd --system --gid mmhotline --home-dir /var/lib/mmhotline --no-create-home --shell /usr/sbin/nologin mmhotline` |
| Group | `mmhotline`. An existing group stays, such as one a deploy tool made so it could write the settings file with that group before the first install. |
| Home | `/var/lib/mmhotline` |
| Runs | `mmhotline.service`, and the MMX runs MMHotline starts for the triage agent |

### mmhotline-triage-agent

| Fact | Value |
| --- | --- |
| Created with | `useradd --system --user-group --home-dir /var/lib/mmhotline-triage-agent --no-create-home --shell /usr/sbin/nologin mmhotline-triage-agent` |
| Group | Its own, `mmhotline-triage-agent`. It is not in group `mmhotline`. |
| Home | `/var/lib/mmhotline-triage-agent` |
| Runs | Each triage agent run, in `mmhotline-triage-agent-run@.service` |

### mmhotline-review-agent

| Fact | Value |
| --- | --- |
| Created with | `useradd --system --user-group --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin mmhotline-review-agent` |
| Group | Its own, `mmhotline-review-agent` |
| Home | None. Each review gets a new empty home in its run's private `/tmp`, which systemd discards when the run ends. |
| Runs | Each review agent run, in `mmhotline-review-agent-run@.service` |

Reason for a separate user: a triage run, which may run code planted in
the triage agent, can then neither see a review run's processes nor write
anything the review agent reads.

### mmhotline-backup

| Fact | Value |
| --- | --- |
| Created | Only when `/etc/mmhotline/backup.env` exists and `litestream` is on the search path |
| Created with | `useradd --system --gid mmhotline --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin mmhotline-backup` |
| Group | `mmhotline`, so Litestream may write the database and SQLite's working files |
| Home | None |
| Runs | `mmhotline-backup.service` (Litestream) |

Reason for a separate user: MMHotline's own process then cannot read the
bucket's keys in Litestream's process.

## Programs

### /usr/local/bin/mmhotline

| Fact | Value |
| --- | --- |
| Owner and mode | `root:root`, `0755` |
| Holds | The MMHotline program |
| Written by | Install, which copies the program it runs from, through a new file renamed into place. When install runs from `/usr/local/bin/mmhotline` itself, it only corrects the owner and mode. |
| Read by | `mmhotline.service`, the agent run units, and the fleet view unit, which all run this path; people, for `sudo mmhotline ...` commands |

### Programs install and MMHotline look for

Install does not put these in place; the how-to pages do.

| Program | Looked for | Used by | Set up in |
| --- | --- | --- | --- |
| `litestream` | On the search path of `sudo mmhotline install`, such as `/usr/local/bin/litestream`. Install writes the path it finds into `mmhotline-backup.service`. | The backup | [Back up and restore the database](../how-to/back-up-and-restore-the-database.md#set-up) |
| `mmx` and `mmx-runner-linux-amd64` or `mmx-runner-linux-arm64` | `/usr/local/bin`, then `/usr/bin` | MMHotline, to read and run the triage agent's playbooks | [Set up the triage agent](../how-to/set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository), step 7 |
| `claude`, or `codex` with `codex-code-mode-host` and `rg` | `/usr/local/bin` | The agent runs | [Set up the triage agent](../how-to/set-up-the-triage-agent.md#set-up-the-agent), step 1 |

## Settings

### /etc/mmhotline/

| Fact | Value |
| --- | --- |
| Owner and mode | `root`, `0755` when install creates it. Install does not change an existing directory. |
| Holds | The settings file, and with the backup, `backup.env` and `litestream.yml` |

### /etc/mmhotline/mmhotline_settings.yml

| Fact | Value |
| --- | --- |
| Owner and mode | `root:mmhotline`, `0640`. Install sets them; before the first install, the group may not exist yet, so the file starts as `root:root`. |
| Holds | Every team setting that is not one person's own, including secrets. [The settings file](settings-file.md) lists its keys. |
| Written by | A person with `sudoedit`, or the team's deploy tool. Install never changes the contents and refuses to run when the file is missing or has problems. |
| Read by | `mmhotline.service` at start and on `settings apply`, through the group; install; the fleet view unit at each run, as root |

MMHotline refuses the file when it is not a regular file, not owned by
root, writable by others than root, or readable by every user
([why](../changing-mmhotline/design/settings-file.md#owner-and-mode)).

### /etc/mmhotline/backup.env

| Fact | Value |
| --- | --- |
| Owner and mode | `root:root`, `0600` |
| Holds | The backup bucket's address, `MMHOTLINE_BACKUP_REPLICA_URL`, and its key, such as `LITESTREAM_ACCESS_KEY_ID` and `LITESTREAM_SECRET_ACCESS_KEY`, as `KEY=value` lines |
| Written by | A person, as [Back up and restore the database](../how-to/back-up-and-restore-the-database.md#set-up) says. Install never writes it; its presence turns the backup on. |
| Read by | systemd, which passes it to `mmhotline-backup.service` only; install, to list the bucket before it starts the backup of a new database. `mmhotline.service` never gets it, and the fleet view unit hides it. |

### /etc/mmhotline/litestream.yml

| Fact | Value |
| --- | --- |
| Owner and mode | `root:root`, `0644` |
| Written by | Install, with the backup unit, when the backup is set up. It replaces different contents. |
| Holds | Litestream's configuration: the database, where Litestream keeps its working files, and the replica URL, which Litestream reads from `backup.env` when it starts. No credential. |
| Read by | Litestream, in `mmhotline-backup.service` |

The whole file:

```yaml
# Managed by mmhotline install.
dbs:
  - path: /var/lib/mmhotline/mmhotline.db
    meta-path: /var/lib/mmhotline-backup/mmhotline.db-litestream
    replica:
      url: "${MMHOTLINE_BACKUP_REPLICA_URL}"
```

## State

### /var/lib/mmhotline/

| Fact | Value |
| --- | --- |
| Owner and mode | `mmhotline:mmhotline`, `0770` |
| Holds | The database, and the files the triage agent preparation adds: MMHotline's SSH key, its copy of the systems repository, and the MMX vault passphrase ([below](#files-the-triage-agent-preparation-adds)) |
| Read by | `mmhotline.service`, which may write only here and in its private `/tmp`; Litestream, through the group. The agent runs and the fleet view cannot see it. |

The group may write so that the backup's own user can write the database.

### /var/lib/mmhotline/mmhotline.db

| Fact | Value |
| --- | --- |
| Owner and mode | `mmhotline:mmhotline`, `0660`, and the same for SQLite's working files beside it, `mmhotline.db-wal` and `mmhotline.db-shm` |
| Holds | All of the team's state: people, schedules, alerts, incidents, contact methods, and the applied settings, including the sending services' tokens and webhook bearer tokens |
| Written by | Install creates it, named after `team_name`, only when it is missing. An existing database stays, with every row. |
| Read by | `mmhotline.service`; Litestream; `sudo mmhotline backup check-uploads` |

### /var/lib/mmhotline/new-database-waits-for-the-backup-bucket-check

| Fact | Value |
| --- | --- |
| Owner and mode | `root:root`, `0600` |
| Holds | One line: `This database was created by mmhotline install. Install checks that the backup bucket holds no earlier backup before it starts the backup, then removes this file.` |
| Written by | Install, when it creates a new database |
| Read by | Install, on this run and every later run |
| Removed by | Install, once `backup.env` exists and the bucket folder holds no backup |

While the marker exists and the bucket already holds a backup, install
does not start the backup and says:
`Backup: not started: this server's database is new, and the bucket in MMHOTLINE_BACKUP_REPLICA_URL already holds a backup, up to transaction ... Starting the backup now would save the empty database as the newest backup. ...`.
Reason: on a new server, Litestream would save the empty database as the
bucket's newest state, and a restore would then bring back nothing. A
restore replaces the state directory and so removes the marker; a
database a deploy tool restored before install never had one.
[Restore on a new server](../how-to/back-up-and-restore-the-database.md#restore-on-a-new-server)
uses this.

### /var/lib/mmhotline-triage-agent/

| Fact | Value |
| --- | --- |
| Owner and mode | `mmhotline-triage-agent:mmhotline-triage-agent`, `0700` |
| Holds | The triage agent's home: its SSH key and Git settings from the preparation, its copy of the systems repository, the coding agent's own files, and the files MMHotline writes at the start of each run ([below](#files-mmhotline-writes-in-the-triage-agents-home)) |
| Written by | Install creates it and corrects its owner and mode. The triage agent runs write in it. |
| Read by | The triage agent runs; `sudo mmhotline triage-agent check-preparation`. The review agent runs and the fleet view cannot see it. |

### /var/lib/mmhotline-backup/

| Fact | Value |
| --- | --- |
| Owner and mode | `mmhotline-backup`, `0700`. systemd creates it for the backup unit. |
| Holds | `mmhotline.db-litestream/`: Litestream's working files, copies of the database changes it captured, kept until they are uploaded |
| Read by | Litestream. The `mmhotline` user cannot write it; the fleet view cannot see it. |

## Units

Install writes each unit as `root:root`, mode `0644`, and replaces
different contents. After a change it runs `systemctl daemon-reload`.
Each unit starts with the line `# Managed by mmhotline install.`. Install
enables each unit and starts or restarts it when needed.

### mmhotline.service

| Fact | Value |
| --- | --- |
| Runs as | `mmhotline` |
| Runs | `mmhotline serve`, below |
| Restarts | Always, 2 seconds after a failure, with no limit on repeated restarts |
| First line | `# Managed by mmhotline install. Binary SHA256: ...`, so a rerun of install from the installed path still sees a new program |

```text
/usr/local/bin/mmhotline serve --state-dir /var/lib/mmhotline --settings-file /etc/mmhotline/mmhotline_settings.yml --command-socket /run/mmhotline/command.sock --triage-agent-run-socket /run/mmhotline-triage-agent/run.sock --review-agent-run-socket /run/mmhotline-review-agent/run.sock --triage-agent-proxy-socket /run/mmhotline/triage-agent-proxy.sock --fleet-view-socket /run/mmhotline-fleet-view.sock
```

The file system is read-only for the service except
`/var/lib/mmhotline` and its private `/tmp`. The service cannot see home
directories, cannot gain privileges, and has no capabilities. Its
system call filter allows the seccomp call and resource limits, which
custom schedules use. systemd creates `/run/mmhotline/` for it.

MMHotline serves its pages over plain HTTP at `web_listen_address` and
takes signed alerts at `alert_intake_listen_address`; it listens on no
public address unless the settings file names one.

The private network needs no TLS, but signatures do not encrypt alert contents.

### mmhotline-triage-agent-run.socket and mmhotline-triage-agent-run@.service

| Fact | Value |
| --- | --- |
| Socket | Listens at `/run/mmhotline-triage-agent/run.sock` and starts one service instance per connection, at most 4 at once |
| Service runs as | `mmhotline-triage-agent` |
| Service runs | `/usr/local/bin/mmhotline triage-agent run-inside-sandbox --proxy-socket /run/mmhotline-triage-agent-proxy.sock`, with `HOME` and the working directory `/var/lib/mmhotline-triage-agent` |
| Limits | No network of its own; its only way out is MMHotline's logging proxy socket, mounted at `/run/mmhotline-triage-agent-proxy.sock`. Writes only its home and private `/tmp`. Cannot see `/var/lib/mmhotline`, `/etc/mmhotline`, or `/run/mmhotline`. Ended after one hour. |

Reason: MMHotline cannot switch users, so systemd starts each run as the
agent's user, in its own control group.

### mmhotline-review-agent-run.socket and mmhotline-review-agent-run@.service

| Fact | Value |
| --- | --- |
| Socket | Listens at `/run/mmhotline-review-agent/run.sock`, one service instance per connection, at most 4 at once |
| Service runs as | `mmhotline-review-agent` |
| Service runs | The same command as a triage agent run |
| Limits | Those of a triage agent run, and it also cannot see `/var/lib/mmhotline-triage-agent`. Writes only its private `/tmp`, where its new home is. |

### mmhotline-fleet-view.socket and mmhotline-fleet-view@.service

| Fact | Value |
| --- | --- |
| Socket | Listens at `/run/mmhotline-fleet-view.sock`, one service instance per connection, at most 2 at once |
| Service runs as | root |
| Service runs | `/usr/local/bin/mmhotline run-fleet-view-command --settings-file /etc/mmhotline/mmhotline_settings.yml`, which runs the settings file's `fleet_view_command` with no shell |
| Limits | Read-only file system, Unix sockets only, ended after 20 seconds. Cannot see `/var/lib/mmhotline`, `/var/lib/mmhotline-backup`, `/var/lib/mmhotline-triage-agent`, `/etc/mmhotline/backup.env`, or `/run/mmhotline`. |
| Written | On every install, whether or not the settings file has `fleet_view_command`, so adding the command needs only `settings apply` |

[fleet_view_command](settings-file.md#fleet_view_command) says more.

### mmhotline-backup.service

| Fact | Value |
| --- | --- |
| Written | Only when `/etc/mmhotline/backup.env` exists and `litestream` is found |
| Runs as | `mmhotline-backup`, group `mmhotline` |
| Runs | `"LITESTREAM_PATH" replicate -config /etc/mmhotline/litestream.yml`, with the path install found |
| Reads | `backup.env`, as its environment, and `litestream.yml` |
| Restarts | Always, 2 seconds after a failure; install also restarts it when `litestream.yml` changed |
| Limits | MMHotline's own restrictions: read-only file system except `/var/lib/mmhotline` and its state directory `/var/lib/mmhotline-backup` |

When `backup.env` exists but `litestream` is missing, install stops with
`Backup: /etc/mmhotline/backup.env exists but litestream is missing; install Litestream 0.5.18 as ... says, and rerun sudo mmhotline install`.
After you change `backup.env`, run `sudo systemctl restart mmhotline-backup`.

## Sockets

| Socket | Owner and mode | Made by | Who connects |
| --- | --- | --- | --- |
| `/run/mmhotline/command.sock` | `mmhotline`, `0600`, in `/run/mmhotline/` (`mmhotline`, `0700`) | `mmhotline serve` | `sudo mmhotline ...` commands. MMHotline checks that the connecting process is root. |
| `/run/mmhotline/triage-agent-proxy.sock` | `mmhotline`, `0666`, in the `0700` directory, which blocks other users | `mmhotline serve` | Each agent run, through systemd's mount at `/run/mmhotline-triage-agent-proxy.sock`. Every site the agent reaches goes through this logging proxy. |
| `/run/mmhotline-triage-agent/run.sock` | `mmhotline:mmhotline`, `0600` | systemd, from the triage agent socket unit | MMHotline, to start a triage agent run |
| `/run/mmhotline-review-agent/run.sock` | `mmhotline:mmhotline`, `0600` | systemd, from the review agent socket unit | MMHotline, to start a review agent run |
| `/run/mmhotline-fleet-view.sock` | `mmhotline:mmhotline`, `0600` | systemd, from the fleet view socket unit | MMHotline, to get the fleet view for the dashboard |

The files under `/run` disappear at a restart of the server; systemd and
MMHotline make them again.

## Files the triage agent preparation adds

A person or the team's deploy tool adds these once, as
[Prepare the servers and the systems repository](../how-to/set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository)
says. Install neither creates nor removes them.
`sudo mmhotline triage-agent check-preparation` checks eight of them,
prints Present or Missing for each, and names the step that fixes a
missing one.

On the MMHotline server:

| Path | Owner and mode | Holds | Read by | Step |
| --- | --- | --- | --- | --- |
| `/etc/hosts` lines | `root` | Each inventory server's name with its private address, such as `10.0.0.11 web1` | The logging proxy, which looks up the names the agent connects to | 1 |
| `/var/lib/mmhotline-triage-agent/.ssh/` | `mmhotline-triage-agent`, `0700` | The agent's SSH key, `id_ed25519` and `id_ed25519.pub` | The agent's `ssh`, to log in to the servers as `triage`; the agent's Git, to sign commits | 2 |
| `/var/lib/mmhotline-triage-agent/.gitconfig` | `mmhotline-triage-agent` | The agent's Git name and email, and SSH signing with `id_ed25519.pub` for every commit | The agent's Git | 3 |
| `/var/lib/mmhotline/.ssh/` | `mmhotline`, `0700` | MMHotline's own SSH key, `id_ed25519`, and `known_hosts` with the Git host's key | MMHotline's Git, to fetch and push the systems repository; MMX, to reach the servers as the inventory's `ssh_user` | 5, 6 |
| `/var/lib/mmhotline/systems_repository/` | `mmhotline` | MMHotline's clone of the team's systems repository | MMHotline and MMX | 6 |
| `/var/lib/mmhotline/mmx_vault_passphrase` | `mmhotline`, `0600` | The MMX vault's passphrase, when the team's playbooks use a vault | MMX, only when a task needs the vault | 6 |
| `/usr/local/bin/mmx`, `/usr/local/bin/mmx-runner-linux-amd64` | `root`, `0755` | MMX and its node runner | MMHotline; MMX copies the runner to each server | 7 |

On each server of each inventory:

| Path | Holds | Step |
| --- | --- | --- |
| The Unix user `triage`, in group `systemd-journal`, with no sudo or only read-only commands | The account the agent investigates as | 2 |
| `/home/triage/.ssh/authorized_keys` | The agent's public key, after `from="10.0.0.5" `, the MMHotline server's private address | 2 |
| The inventory `ssh_user`'s `authorized_keys`, such as `/root/.ssh/authorized_keys` | MMHotline's public key, after the same `from=` | 5 |

In the systems repository: `triage_agent_rules.md` at the top, which the
agent reads at the start of every run, and `ssh/known_hosts`, with each
server's host key under its inventory address (step 4).

## Files MMHotline writes in the triage agent's home

MMHotline writes these at the start of each triage agent run and replaces
them each time:

| Path in `/var/lib/mmhotline-triage-agent` | Mode | Holds |
| --- | --- | --- |
| `.ssh/config` | `0600` | Every host through the logging proxy, as the user `triage`, with the agent's key, the systems repository's `ssh/known_hosts`, and no prompts |
| `.mmhotline/bin/ssh` | `0700` | The agent's `ssh`, first on its search path, which runs `mmhotline triage-agent ssh` |
| `.mmhotline/bash_env` | `0600` | Puts that `ssh` first again in a bash login shell |
| `systems_repository/` | | The agent's own copy of the systems repository, at the commit MMHotline hands the run; the agent's new commits go back to MMHotline from here |
| `.codex/` | | Codex's own files, with Codex as the coding agent |

Each run also makes `/tmp/mmhotline-triage-agent-requests-*/request.sock`,
mode `0600`, in its private `/tmp`, where the agent sends its requests,
such as a playbook run. systemd discards it with the run.
