# MMHotline

**Agent-first on-call alerting, maxxed for monkey/human understanding.**

MMHotline gets an urgent alert to a person who can act on it. Your tools
send their alerts to MMHotline. MMHotline works out who is on call and
contacts that person on their phone. If the person does not answer,
MMHotline contacts the backup, and then everyone else who is available.

> [!NOTE]
> **This is an alpha version.** MMHotline has woken us for real test alarms many times. Before you rely on it to wake you, run its test alarm and see it reach your phone. Settings may still change between versions. If anything confuses you or breaks, please tell us: [how to report it](CONTRIBUTING.md). Thanks!

Your coding agent, such as [Claude](https://claude.com/claude-code) or [Codex](https://openai.com/codex/), can write the settings
file and run the commands, or you can do both yourself. You don't need to
have used on-call software before.

MMHotline runs on your own Linux server. It is not a hosted service: the
alerts, the schedule, and the history stay on that server. One installation
serves one team. The people on the team open MMHotline's pages in a browser,
on a phone or a computer.

Three words come up everywhere in MMHotline:

- An **SOS** is the urgent alert that wakes the person on call, even at
  night.
- An **incident** is MMHotline's record of the problem that an SOS reports.
- To **claim** an incident is to say "I'm on it". MMHotline then stops
  contacting the next person.

The server must run [systemd](https://systemd.io/), the service manager of most current Linux
distributions. MMHotline's server checks have passed on Debian 12 and 13 and
on Ubuntu 24.04.

The
[Monkeymaxxing Stack](https://github.com/monkeymaxxing/monkeymaxxing-stack)
is an example of MMHotline set up as part of a whole system.

## The Monkeymaxxing tools

MMHotline is one of the free, open-source Monkeymaxxing tools for running
simple server systems.

| Tool | What it does |
|---|---|
| [MMX](https://github.com/monkeymaxxing/mmx) | Sets up your servers, and makes every later change to them |
| [MMWatchdog](https://github.com/monkeymaxxing/mmwatchdog) | Watches your servers, restarts what fails, and alerts you |
| **MMHotline** (this tool) | Gets each urgent alert to the person on call, and can let your coding agent investigate first |
| [doubleshell](https://github.com/monkeymaxxing/doubleshell) | Carries SSH over WireGuard so you can close port 22 to the internet without a laptop VPN |
| [Monkeymaxxing Stack](https://github.com/monkeymaxxing/monkeymaxxing-stack) | Instructions for your coding agent that build and run a whole production system with the tools above |

You need none of the other tools to use MMHotline. Any tool that can send a
signed HTTP request can send alerts to MMHotline. MMWatchdog has MMHotline
sending built in: give it MMHotline's alert intake address, the alert source
ID, and the signing secret.

## Why MMHotline

### Why on-call software instead of an alerts channel

The first alerting of many teams is a chat channel or an email list: a
script or a monitoring tool posts every problem there. A coding agent will
often set this up too, because one line does it:

```sh
curl -X POST -d '{"text":"myapp does not answer on app01"}' "$SLACK_WEBHOOK_URL"
```

The message reaches everyone and wakes no one. At 3 AM, nobody reads the
channel. In the daytime, each person who reads the message can think that
somebody else has it. Nobody can see whether anyone is working on the
problem. And when the first person does not react, nothing else happens.

MMHotline is on-call software. It makes sure that one person knows about the
problem and has said so:

- **Wake.** An SOS reaches the phone of the person on call as an emergency
  alert that repeats every 30 seconds until the person acknowledges it, or
  as a phone call. You can also add a way of your own to reach a person.
- **One person.** A schedule says who is on call now. MMHotline contacts
  that person first, and everyone else sleeps.
- **Escalate.** If the person on call does not claim the incident in the
  time that you set, MMHotline contacts the backup, and then everyone else
  who is available.
- **Claim.** One tap says "I'm on it". MMHotline stops contacting more
  people, and the team sees who has the incident.
- **Notice silence.** A tool that sends alerts also checks in with
  MMHotline regularly, such as every 30 seconds. When the check-ins stop,
  MMHotline alerts, so a monitoring tool that died does not look like a
  quiet night.
- **Record.** Each incident keeps its history: who was contacted and when,
  who claimed the incident, and how it ended.

In MMHotline, the team's people and schedule are part of one settings file:

```yaml
# Part of /etc/mmhotline/mmhotline_settings.yml
people:
  - name: Barbara Example
    sign_in_email_address: barbara@example.com
    role: administrator

  - name: Carlos Example
    sign_in_email_address: carlos@example.com
    role: member

on_call_schedule:
  schedule_type: weekly_rotation                   # one person is on call for a week
  rotation_sign_in_email_addresses: [barbara@example.com, carlos@example.com]
  backup_option: next_in_rotation                  # the backup is the next person in the rotation
  escalate_if_unclaimed_after_minutes: 1           # contact the backup after 1 minute without a claim

  time_zone: America/New_York
  handoff_weekday: 1                               # the week changes on Monday (Sunday is 0)
  handoff_hour: 9
  handoff_minute: 0
  first_start_date: "2026-11-02"
```

In this example, Carlos is on call this week, and Barbara is the backup. The
one minute before the backup is contacted is short, to keep the example
short. The install guide uses five minutes.

Now a monitoring tool sends an SOS: `myapp` does not answer on the server
`app01`. On the MMHotline server, `sudo mmhotline incidents list` shows the
incident. Part of its output:

```text
SOS incident 6fc18666eecf3e53302bb7c972083e8c: myapp does not answer on app01
Status: Not claimed yet. Opened at 2026-11-15T00:05:03Z.
Being contacted now: Carlos Example. If nobody claims the incident by 2026-11-15T00:06:03Z, MMHotline contacts the next person.
```

Carlos's phone sounds an emergency alert, delivered by [Pushover](https://pushover.net/), a
notification service with an app for the phone. The alert reads:

```text
[SOS] myapp does not answer on app01
The health page of myapp has not answered for 1 minute.

Acknowledge this to tell MMHotline you are handling it. MMHotline then stops contacting other people.
```

Carlos does not answer. After one minute, MMHotline sends the same alert to
Barbara. Barbara acknowledges the alert on her phone, and the incident is
hers:

```text
Status: Claimed. Opened at 2026-11-15T00:05:03Z.
Claimed by Barbara Example at 2026-11-15T00:06:15Z.
```

When `myapp` answers again, the monitoring tool tells MMHotline that the
problem is over, and MMHotline resolves the incident.
`sudo mmhotline incidents show --incident-id 6fc18666eecf3e53302bb7c972083e8c`
then shows the whole history. Part of its output:

```text
Action history:
  2026-11-15T00:05:03Z Opened: SOS incident opened: myapp does not answer on app01. By: Monitoring.
  2026-11-15T00:05:03Z Contacting: Contacting Carlos Example (on call). By: MMHotline.
  2026-11-15T00:06:03Z Escalated: Contacting Barbara Example (backup). By: MMHotline.
  2026-11-15T00:06:03Z Contacting: Contacting Barbara Example (backup). By: MMHotline.
  2026-11-15T00:06:15Z Claimed: Claimed by Barbara Example, who acknowledged the alert in Pushover. By: Barbara Example.
  2026-11-15T00:06:37Z Resolved: Resolved by Monitoring. By: Monitoring.
```

The same history is on the incident's page in the browser.
[The whole settings file](#the-whole-settings-file) shows the rest of the
file.

### If you've used PagerDuty

MMHotline does the central job of [PagerDuty](https://www.pagerduty.com/): a schedule, an alert that
wakes the person on call, escalation, and a record of each incident.
MMHotline differs in these ways:

- **You run it.** PagerDuty is a hosted service. MMHotline is one program
  on your own server, and its data is one [SQLite](https://sqlite.org/) database file on that
  server.
- **No price for each person.** PagerDuty's incident-management plans are
  priced for each user, after a free plan for up to 5 users
  ([PagerDuty's pricing](https://www.pagerduty.com/pricing/), read on
  2026-11-15). MMHotline is free. You pay only the sending services that
  you choose, such as Pushover for emergency alerts or [Twilio](https://www.twilio.com/) for phone
  calls.
- **Your coding agent can answer first.** MMHotline can give each `warn`
  and `sos` alert to your own Claude or Codex before it wakes a person.
- **One team for each installation.** MMHotline has no routing of alerts
  between several teams. A company with several teams runs one MMHotline
  for each team.

## What you get

### Maxxed for monkey/human understanding

Even if your coding agent writes the settings file, you may still want to
read it, sometimes during an outage at 3 AM. So MMHotline's settings have
names that say exactly what they do:

- `escalate_if_unclaimed_after_minutes: 5`
- `sos_after_no_check_in_for_minutes: 5`
- `rotation_sign_in_email_addresses: [barbara@example.com, carlos@example.com]`

The pages follow the same rule. The button that claims an incident is
labeled "Claim: I'm handling the incident", and a bar at the top of every
page says what MMHotline does next by itself, such as "Claim the incident by
17:42, or MMHotline also wakes Priya Natarajan."

### Your coding agent writes and runs it

A coding agent can write the settings file and run the commands, and a
person checks the result. These parts of MMHotline are made for that:

- **One settings file.** The team's people, schedule, alert sources, and
  sending services are in `/etc/mmhotline/mmhotline_settings.yml`, so the
  file can live in your repository and change through review.
- `sudo mmhotline settings check --settings-file FILE` reports every
  problem in the settings file with the name of its key, and never prints a
  secret's value.
- `sudo mmhotline settings apply` applies a changed settings file to the
  running MMHotline.
- `sudo mmhotline incidents list`, `incidents show`, and `alerts list` let
  an agent on the server read what happened. With `--json`, each prints one
  JSON object.

### Your coding agent answers first

MMHotline can give each `warn` and `sos` alert to a **triage agent** before
it wakes a person. The triage agent is your own Claude or Codex. It
looks into the alert, reports what it found, and proposes a fix or runs one.
Rules in the settings file say which fixes the triage agent may run without
a person, and which fixes a person must approve.

MMHotline runs the triage agent as a separate Unix user in a systemd
sandbox. The triage agent cannot read MMHotline's database or the secrets
of the sending services, unless the team lets it run every fix without a
person
([why](docs/how-to/decide-what-the-triage-agent-may-do.md#set-the-rules)).
Every connection it opens from MMHotline's server goes through a proxy that
logs it. On the team's servers it logs in as the user `triage`, and a
connection that a command there opens does not pass the proxy. The triage
agent is optional.

### The right alert wakes the right person

Each person adds their own **contact methods** on MMHotline's pages, and
chooses which severities each contact method gets. The severities are
`info`, `warn`, and `sos`. Only an SOS opens an incident and wakes a person.

| Contact method | Meant to wake a sleeping person? |
| --- | --- |
| Pushover notification | Yes: an emergency alert that repeats until acknowledged |
| Automated voice call, through your Twilio account | Yes |
| Email | No |
| [Slack](https://slack.com/) message | No |
| [Telegram](https://telegram.org/) message | No |
| Outgoing webhook to a program of yours | That depends on your program |

These are the contact methods that MMHotline has built in. An outgoing
webhook gives each alert to a program of yours, so you can add any other way
to reach a person, such as an SMS text message.

A person can claim an incident by acknowledging the Pushover alert, with a
button in Slack or Telegram, or on the incident's page.

### Schedules that fit a small team

A schedule can go by the day of the week, by the hour of the day, or by a
weekly rotation. For anything else, a custom schedule is a short program in
[Starlark](https://github.com/bazelbuild/starlark), a language that reads like Python.

A person who is away sets their own unavailability on the "On call" page,
and a colleague can take over on-call for a time. Both end at the time that
the person sets.

### It notices when a tool goes silent

A tool that sends alerts also sends MMHotline a check-in regularly, such as
every 30 seconds. When a tool's check-ins stop, MMHotline sends a warning,
and later an SOS, after the times that you set. During planned downtime
that you declare, MMHotline sends neither.

### It notices when MMHotline itself is down

If MMHotline's own server is down, MMHotline cannot alert anyone. The
optional **MMHotline-down alarm** covers that case. It is a
[Cloudflare Worker](https://workers.cloudflare.com/), a small program that
Cloudflare runs, in your own Cloudflare
account. MMHotline checks in with it, and
when the check-ins stop, it wakes the person on call and, later, the backup,
as an SOS does.

### A public status page

MMHotline can publish an optional status page for your customers. Cloudflare
serves the page from a storage bucket ([Cloudflare R2](https://www.cloudflare.com/developer-platform/products/r2/)) in your own Cloudflare
account, so the page stays up when your servers are down.

### Ruthlessly simple

MMHotline is one program and one SQLite database file on one server. It
needs no database server. Your server accepts no web connection from the
internet: people reach the pages through a [Cloudflare Tunnel](https://www.cloudflare.com/products/tunnel/), and [Cloudflare Access](https://www.cloudflare.com/zero-trust/products/access/)
signs each person in first. A continuous backup of the SQLite
database file, with [Litestream](https://litestream.io/), is optional.

## When another tool fits better

Choose another tool when you need one of these:

- **Team pages without Cloudflare.** People open MMHotline's pages after
  signing in through Cloudflare Access, which needs a Cloudflare account
  and a domain whose DNS Cloudflare manages. MMHotline still wakes phones
  without it, so a first test needs no Cloudflare.
- **A hosted service.** MMHotline runs on a server that you keep running.
  PagerDuty and similar services run for you.
- **Routing between several teams.** One MMHotline serves one team.
- **SMS text messages without a program of your own.** MMHotline has no SMS
  contact method built in. You can send an SMS from a program of yours
  behind an outgoing webhook.

## The whole settings file

The settings file is `/etc/mmhotline/mmhotline_settings.yml` on the
MMHotline server. The file holds secrets, so it belongs to root: you change
it with `sudoedit`, and other users on the server cannot read it. This is
the whole file of the example above:

```yaml
# /etc/mmhotline/mmhotline_settings.yml
team_name: Operations
web_url: https://ops.example.com                 # the address that people open
web_listen_address: 127.0.0.1:8480               # where MMHotline serves its pages on the server
alert_intake_listen_address: 10.20.0.1:8484      # the private address where your tools send alerts

sending_services:
  pushover:
    application_token: YOUR_30_CHARACTER_APPLICATION_TOKEN

people:
  - name: Barbara Example
    sign_in_email_address: barbara@example.com
    role: administrator

  - name: Carlos Example
    sign_in_email_address: carlos@example.com
    role: member

on_call_schedule:
  schedule_type: weekly_rotation
  rotation_sign_in_email_addresses: [barbara@example.com, carlos@example.com]
  backup_option: next_in_rotation
  escalate_if_unclaimed_after_minutes: 5

  time_zone: America/New_York
  handoff_weekday: 1
  handoff_hour: 9
  handoff_minute: 0
  first_start_date: "2026-11-02"

alert_sources:
  - alert_source_id: monitoring
    name: Monitoring
    signing_secrets: ["SIGNING_SECRET"]            # 64 hexadecimal characters, from openssl rand -hex 32
    warn_after_no_check_in_for_minutes: 2
    sos_after_no_check_in_for_minutes: 5

rules_for_running_agent_playbooks: {}            # empty until you set up the triage agent
services_whose_changes_a_person_must_approve: []
```

An **alert source** is a tool that sends alerts, such as your monitoring
tool. The tool signs each alert with the alert source's signing secret, so
MMHotline knows who sent the alert. A **sending service** is an outside
service that delivers notifications, such as Pushover.

Tools send their alerts to the private address in
`alert_intake_listen_address`, never to the web address. The private address
can be a [WireGuard](https://www.wireguard.com/) address, or `127.0.0.1` when the tool runs on the
MMHotline server.

Run the commands on the MMHotline server:

```sh
sudo mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml   # check the file, without changing anything
sudo mmhotline settings apply    # make the running MMHotline use the settings file
sudo mmhotline incidents list    # show every SOS incident that is not resolved
```

[How MMHotline works](docs/how-it-works.md) explains the parts of
MMHotline and how an alert reaches a person.

## Set it up with your coding agent

You can have your coding agent set up MMHotline. The prompt below tells the
agent to follow this README, check each step before the next, and ask you
when it needs something that only you can give.

Paste this into your agent. Put your own server, login account, host name,
and people in place of the examples:

```text
Set up MMHotline on my server. Follow the README at
https://github.com/monkeymaxxing/mmhotline, including its "For coding
agents" checklist.

My server: util01 at 203.0.113.21. I log in to it as root with my current
SSH setup. People will open MMHotline at ops.example.com, and Cloudflare
manages the DNS of example.com.

The team:
- Barbara Example, barbara@example.com, administrator
- Carlos Example, carlos@example.com

One person is on call for a week, and the other person is the backup.

Run each step's check before the next step. Ask me for each secret when you
need it, and keep secrets out of our chat and out of any repository. Never
compose your own shell line for a secret: give me the command to run in my
own terminal. Tell me exactly what to do in Cloudflare and
in the Pushover app, because I do those parts. At the end, send a test SOS
and ask me whether my phone sounded.
```

The sections below show the steps, if you want to follow along or do them
yourself.

## Install

Before you install, you need:

- **A Linux server** with systemd and `sudo`, and a private network address
  on it where your tools send alerts.
- **A way to wake a person,** such as a Pushover account and the Pushover
  app on each person's phone, or a Twilio account for phone calls.

That is all a first test on one server needs: MMHotline wakes your phone
without Cloudflare. Later, so that people can open MMHotline's pages from
their phones, you add a Cloudflare account with Zero Trust and a domain
whose DNS Cloudflare manages.

On the server, paste this block. It picks the archive for the server's
processor, downloads it with its checksum file, checks it, and unpacks it
into `~/mmhotline-0.5.0`. It does not install anything yet.

```sh
(
set -eu
version=0.5.0
case "$(uname -sm)" in
  "Linux x86_64")  arch=amd64 ;;
  "Linux aarch64") arch=arm64 ;;
  *) echo "MMHotline runs on a Linux server. There is no release for $(uname -sm)."; exit 1 ;;
esac
release=https://github.com/monkeymaxxing/mmhotline/releases/download/v$version
mkdir -p ~/mmhotline-$version && cd ~/mmhotline-$version
curl -fsSLO "$release/mmhotline-$version-linux-$arch.tar.gz"
curl -fsSLO "$release/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf "mmhotline-$version-linux-$arch.tar.gz"
echo "Unpacked in ~/mmhotline-$version/mmhotline-$version-linux-$arch."
echo "Run the next steps from there: cd ~/mmhotline-$version/mmhotline-$version-linux-$arch"
)
```

It prints `mmhotline-0.5.0-linux-amd64.tar.gz: OK` (`arm64` on an ARM
server) and where it unpacked. The block cannot move your shell, so go to
the unpacked folder yourself:

```sh
cd ~/mmhotline-0.5.0/mmhotline-0.5.0-linux-*/
```

IF THE CHECK PRINTS `FAILED`, DELETE THE FOLDER `~/mmhotline-0.5.0` AND DO NOT RUN THE PROGRAM.

Optionally, prove that GitHub built the archive from this repository, with
the GitHub command line tool (`gh`):

```sh
gh attestation verify ~/mmhotline-0.5.0/mmhotline-0.5.0-linux-*.tar.gz --repo monkeymaxxing/mmhotline
```

**By hand.** `uname -sm` prints `Linux x86_64` for the `amd64` archive or
`Linux aarch64` for the `arm64` archive. The archives are
`mmhotline-VERSION-linux-amd64.tar.gz` and
`mmhotline-VERSION-linux-arm64.tar.gz`, and `SHA256SUMS` lists their
checksums. Each unpacks to a folder `mmhotline-VERSION-linux-ARCH` that
holds `mmhotline`.

**Upgrade.** Paste the block again with the new version number in
`version=`. Then run `sudo ./mmhotline install` from the new folder.
[Upgrade MMHotline](docs/how-to/upgrade.md) has the details.

The checksum finds a damaged download. To run a commit that has no release,
[build MMHotline from source](docs/how-to/install.md#build-from-source).

MMHotline's code is on GitHub at
[github.com/monkeymaxxing/mmhotline](https://github.com/monkeymaxxing/mmhotline),
with the release archives, issues, and pull requests. For when GitHub is
down, a mirror at
[gitlab.com/monkeymaxxing/mmhotline](https://gitlab.com/monkeymaxxing/mmhotline)
has the same code and release tags, but no release archives: from the
mirror, build from source.

## First run

On the server:

1. Write `/etc/mmhotline/mmhotline_settings.yml`, with yourself as the
   first administrator.
   [Install MMHotline](docs/how-to/install.md) has
   the steps, and the [settings file reference](docs/reference/settings-file.md) lists every key.
2. Check the settings file and install:

   ```sh
   sudo ./mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
   sudo ./mmhotline install
   ```

   RUN BOTH FROM THE UNPACKED FOLDER, `~/mmhotline-0.5.0/mmhotline-0.5.0-linux-*/`.

   After the install, you can delete the `~/mmhotline-0.5.0` folder.

   **Upgrade.** Paste the download block again with the new version, and
   run `sudo ./mmhotline install` from the new folder. An upgrade keeps the
   database and every row. [Upgrade MMHotline](docs/how-to/upgrade.md) has the
   details.

3. Set up Pushover and add your phone as a contact method, with
   `sudo mmhotline connect-pushover` run in your own terminal.
   [Connect Pushover](docs/how-to/connect-pushover.md)
   has the steps, with the phone settings that let an emergency alert sound
   in silent mode.
4. Put MMHotline behind Cloudflare Access, so that people can open it.
   [Put MMHotline behind Cloudflare Access](docs/how-to/put-mmhotline-behind-cloudflare-access.md)
   has the steps.
5. Send a test SOS and acknowledge it on your phone. The acknowledgement
   claims the test, and MMHotline then closes it by itself.
   [Get started with MMHotline](docs/get-started.md) has
   the steps.

**What the install does to the server.** `mmhotline install` copies the
program to `/usr/local/bin/mmhotline`, creates the Unix users that
MMHotline and its agents run as, and starts MMHotline as a systemd service.
MMHotline serves its pages only on the server itself, at `127.0.0.1:8480`, and takes alerts on the private
address that the settings file names. `mmhotline install` never changes the
contents of the settings file.

[Get started](docs/get-started.md) walks through each step with a worked example, and
the [documentation index](docs/README.md) lists the guides for upgrades, the
continuous backup, the triage agent, the MMHotline-down alarm, and the status page.

## For coding agents

When you set up MMHotline for someone, reach each outcome below and run its
check before moving on.

| Step | Outcome | Check |
| --- | --- | --- |
| 1 | The settings file exists, with the person as the first administrator. | `sudo ./mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml`, run from the unpacked folder, prints that the settings file has no problems. |
| 2 | MMHotline is installed and runs. | `sudo ./mmhotline install` prints the web URL, and `mmhotline version` prints a version. |
| 3 | The person's phone gets notifications. | The person ran `sudo mmhotline connect-pushover` in their own terminal and saw the quiet **MMHotline can reach this phone** notification that MMHotline sent when the contact method was added. `contact-methods list` still says `Emergency test: not yet acknowledged.` |
| 4 | The person can open MMHotline. | The person opened the web URL and signed in through Cloudflare Access. |
| 5 | An SOS wakes the person. | You told the person their phone was about to ring and ran `sudo mmhotline incidents send-test-sos` yourself (with `--sign-in-email-address EMAIL` in a team of more than one person), the person's phone sounded, and the person acknowledged it. `contact-methods list` then says `Emergency test: acknowledged at TIME.`, and `sudo mmhotline incidents list` lists no incident. |

- Follow the rules in [For a coding agent](docs/get-started.md#for-a-coding-agent):
  speak in the person's words, and after the install say only what works
  and the one next step.
- The person does the steps in Cloudflare and in the Pushover app. Say
  exactly what to create and where the value goes.
- Never put a secret's value in the chat, a command line, or a repository.
  Never compose your own shell line for a secret: give the person the
  command to run in their own terminal, such as
  `sudo mmhotline connect-pushover` for Pushover.
  The settings file holds the secrets. Change it with `sudoedit`. Other
  users on the server cannot read it.
- Of the built-in contact methods, only a Pushover notification and an
  automated voice call are meant to wake a person. Do not give a person only
  email or chat for SOS.
- To read what happened, use `sudo mmhotline incidents list`,
  `sudo mmhotline incidents show --incident-id ID`, and
  `sudo mmhotline alerts list`, with `--json`. To claim or resolve with
  only a terminal, run `sudo mmhotline incidents claim --incident-id ID
  --sign-in-email-address ADDRESS` and `sudo mmhotline incidents resolve
  --incident-id ID`. The tool that sent the alert can also resolve it.

## When an SOS arrives

1. **Claim the incident.** Acknowledge the alert in Pushover, or open the
   incident's page and choose "Claim: I'm handling the incident". MMHotline
   stops contacting the next person.
2. **Read the incident's page.** It shows the alert's message, who was
   contacted, and what the triage agent found, when you use one.
3. **Resolve the incident** on its page when the problem is fixed. A tool
   that reported the problem can also resolve the incident itself, by
   sending the alert that says the problem is over.

MMHotline groups the Warning, Info, and resolved messages to one person,
and sends that person at most one such message a minute. So a "resolved"
message can arrive about a minute after the incident was resolved. An SOS
is never held back.

If Cloudflare is down, a person on the server can create a one-time sign-in
link, and open MMHotline through SSH.
[Sign in during a Cloudflare outage](docs/how-to/sign-in-during-a-cloudflare-outage.md)
has the steps.

## How it works

One `mmhotline` program runs on the server as a systemd service. It takes
signed alerts and check-ins from your tools on the private address, and
saves each alert before it answers the tool. For an SOS, it opens an
incident, looks up who is on call, and sends the notification through the
sending service of that person's contact method. A timer contacts the next
person when nobody has claimed the incident in time. Everything is kept in
one SQLite database file on the server.

[How MMHotline works](docs/how-it-works.md) explains each part.

## Documentation

| Guide | Read it when you want to |
|---|---|
| [Documentation index](docs/README.md) | Find the guide for a job, a reference page, or a fix for a message |
| [Get started with MMHotline](docs/get-started.md) | Set up a first working team, from install to a claimed test SOS |
| [How-to guides](docs/README.md) | Install, upgrade, back up, connect sending services and alert sources, and set up the triage agent, the MMHotline-down alarm, and the status page |
| [Reference](docs/reference/mmhotline-command.md) | Look up a command, a settings file key, an installed file, an alert request, or a severity |
| [When something fails](docs/when-something-fails.md) | Find what a message or a symptom means and what to do |
| [How MMHotline works](docs/how-it-works.md) | Understand the parts of MMHotline and how an alert reaches a person |

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) says how to report a bug or propose a
change. [AGENTS.md](AGENTS.md) is the starting point for changing MMHotline
itself. It links the [design plan](docs/changing-mmhotline/design/README.md), the
[acceptance contracts](docs/changing-mmhotline/acceptance.md), the
[validation record](docs/changing-mmhotline/validation.md), the [testing guide](docs/changing-mmhotline/testing.md),
the [implementation rules](docs/changing-mmhotline/implementation-rules.md),
[running locally for development](docs/changing-mmhotline/local-development.md), and the
[documentation map](docs/changing-mmhotline/documentation-map.md).

## License

Apache License 2.0. Included dependencies keep their own licenses.
