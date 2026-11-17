# MMHotline: design and implementation plan

MMHotline is open-source on-call software that a team runs on its own server.
MMHotline receives signed alerts and check-ins from the team's tools, works out
who is on call, and contacts that person. An SOS is the urgent alert that
wakes the person on call, even at night. To claim an incident is to say
"I'm on it", which stops MMHotline contacting the next person. An unanswered
SOS escalates to the backup, then everyone available. One installation serves one team; a company
with several teams runs several installations. The team's settings live in
one file on the server, which its deploy tool or an administrator changes.

The design documents describe the current build. `docs/changing-mmhotline/design/README.md` owns product principles, feature status, and
planned work. The [acceptance contracts](../acceptance.md)
define the outcomes required for completion and review. Start with the
[project README](../../../README.md) to install MMHotline.

## Design map

| Document | Owns |
| --- | --- |
| [Ingestion](ingestion.md) | Alert vocabulary, signed intake, receipts, limits, source identity, deduplication, and SOS correlation |
| [Incidents and escalation](incidents.md) | SOS state, ownership, claims, resolution, repeats, and escalation |
| [Notifications](notifications.md) | Contact methods, subscriptions, routing, attempts, and contact method type behavior |
| [Scheduling](scheduling.md) | Schedule types, timezones, previews, backups, handoffs, and custom schedule code |
| [Availability](availability.md) | Personal unavailability, team mute, and taking on-call |
| [People and access](people.md) | Sign-in through Cloudflare Access and sign-in links, people, roles, contact method ownership, and trusted local command access |
| [Check-ins and planned downtime](monitoring.md) | Alerts about missing check-ins, maintenance windows, and restart timing |
| [Interface](interface.md) | Pages, actions, display states, navigation, and accessibility |
| [Triage agent](triage-agent.md) | The fence around the team's coding agent, logging proxy, internet access, triage agent setup, runs, and the connection to each coding agent. Triage sessions, requests, rules for running the agent's playbooks, the review agent, keeping the triage and review agents' sign-in working, and MMX runs for the agent. |
| [Status page and MMHotline-down alarm](mmhotline-down-alarm-and-status-page.md) | The public status page and MMHotline-down alarm in the team's Cloudflare account. The status page's two files, posting from MMHotline and from a laptop, and set-up. The MMHotline-down alarm's Worker, held messages, check-ins, sends, the all-clear, and set-up. |
| [Settings file](settings-file.md) | The settings file, owner and mode, static check, parts and how each part applies, change detection, restarts, startup with a broken file, and why `backup.env` stays separate. |
| [Operation](operation.md) | Deployment boundary, storage, jobs, recovery, secrets, setup, and diagnostics |
| [Development](development.md) | Go/build policy, organization, and test and release checks |
| [Implementation decisions](open-questions.md) | Engineering decisions and their owning documents |
| [Defaults](defaults.md) | Index of current implementation defaults |

[How MMHotline works](../../how-it-works.md) explains the main components.
The [implementation rules](../implementation-rules.md) govern code structure;
[testing](../testing.md) describes verification, and
[validation](../validation.md) records its results.

## Intended product

The team's tools send signed generic alerts and check in with MMHotline
every 30 seconds. A check-in tells MMHotline that a sender is still running. Check-ins
can list maintenance windows, which suppress alerts about expected silence.
MMHotline receives monitoring results rather than running monitoring checks
or connecting to systems for monitoring. The triage agent can read monitored
servers over SSH through MMHotline's proxy ([triage agent](triage-agent.md#reading-the-servers-over-ssh)).

MMHotline delivers email, outgoing webhooks, Telegram messages, Slack messages,
Pushover notifications, and automated voice calls. The alert severities are
`info`, `warn`, and `sos`. The resolved variants, `resolved-info`,
`resolved-warn`, and `resolved-sos`, report that a problem cleared. Only SOS opens an incident requiring action.

The web interface uses `net/http` and `html/template` with small JavaScript
enhancements. Storage uses one `mmhotline.db` SQLite database
([storage](operation.md#storage)).

## Purpose within Monkeymaxxing

Monkeymaxxing teaches Ruthlessly Simple
Systems: help developers own quiet, understandable production
systems that consume as little money, engineering time, talent, and attention
as their real requirements allow. Money, engineering time, talent, and
attention should grow the company and serve the people who use the company's
products.

MMHotline must apply that principle to both the team's experience and
MMHotline's own implementation and operation. MMHotline must get an important problem to someone who can act,
explain what happened, and recover predictably. Keep the ongoing work of building and operating MMHotline small.
Choose the simplest implementation that meets the actual requirement; add
complexity only when a concrete need earns its full cost. Easy recovery,
security, and reliable alert delivery are requirements for MMHotline too.

## Product principles

- MMHotline is self-hosted, open-source on-call software. The team supplies
  its sending service credentials. Phone calls use the team's optional
  Twilio account.
- SOS means a problem that requires an immediate response. Ordinary
  information must not wake someone up.
- Recover current work after a restart. Recently heard alert sources get
  their full Warning and SOS times before alerts about missing check-ins.
  Sources already marked silent keep their recorded times
  ([monitoring](monitoring.md)).
- A duplicate is better than a dropped alert. Where MMHotline cannot know
  whether a notification went out, after a restart, it sends the
  notification again. A person may get the same notification twice; a
  notification is never left out to avoid a duplicate
  ([recovery](operation.md#recover-current-work)).
- Make the primary responder, backup, and person who claimed the incident
  and next escalation obvious. Show what
  happened and why a notification was sent or suppressed.
- Keep notification preferences on contact methods: a person may want email
  for information and a phone call for SOS incidents.
- Keep the incident lifecycle small. Only SOS has an open/claimed/resolved
  lifecycle; ordinary information and warning alerts remain a useful log.
- Make handoffs, taking on-call, and team mute easy to understand.
- Keep the core small. The team’s logic runs on the team’s systems.
  MMHotline runs two kinds of team program in sandboxes: custom
  schedule code in a locked-down Starlark sandbox, and the team's coding
  agent as the [triage agent](triage-agent.md), as its own Unix user with
  every connection through MMHotline's logging proxy. MMHotline also runs
  the `mmx` program for the systems repository's playbooks
  ([MMX runs](triage-agent.md#running-mmx-for-the-agent)).
  For the dashboard, MMHotline asks systemd to run the
  one command root sets in the settings file's `fleet_view_command`
  ([operation](operation.md#the-fleet-view-socket)). MMHotline runs no team
  programs beyond custom schedule code, the coding agent, `mmx`,
  and the configured fleet view command.
- Keep the team's settings in one file that root owns, so the systems
  repository and vault are the record, and nobody changes the server by hand
  ([settings file](settings-file.md)).
- Humans and agents should be able to inspect and operate the same product.
  Define authenticated automation interfaces with the workflows they serve.
- Write the application and built-in integrations in Go. All dependencies,
  including transitive dependencies, must build with CGO_ENABLED=0. No C
  compiler, C headers, or third-party C libraries are required by the product.

## Specification and development

Start with the [design map](#design-map), then read the
document owning the behavior being changed. [AGENTS.md](../../../AGENTS.md) defines the
change policy. [Implementation rules](../implementation-rules.md) define the
code boundaries; [testing](../testing.md) maps the [acceptance contracts](../acceptance.md) to the
tests that verify the contracts. [Validation](../validation.md) records the
checks performed and their limits. The [documentation map](../documentation-map.md)
identifies the reader and subject of each document.

## Status

“Built” means implemented and covered by application tests. The “Built” label
does not establish successful delivery through a real sending service.
[Validation](../validation.md) records the verification scope.

| Feature | Built behavior |
| --- | --- |
| Application and persistence | One-server Go application, a loopback web listener, an intake-only HTTP listener on the private network, and one SQLite database. The command Unix socket checks the connecting process's kernel credentials and requires root. Application operations are durable, and current work recovers after restart. |
| Settings file | `/etc/mmhotline/mmhotline_settings.yml`, owned by root and read by MMHotline's group. `settings check` reports every problem by key path and never prints a secret's value. `settings apply` applies the settings file's parts, each on its own, through the command socket. Change detection skips unchanged parts, and only one apply runs at a time. `settings show` reads the applied settings and each part's outcome through the command socket. Changing a restart key requires a restart. The `settings apply` command restarts MMHotline when a restart setting changes. MMHotline reads the settings file at start. If the file has a problem, MMHotline uses the last applied settings and warns administrators. If no settings have applied yet, MMHotline refuses to start with the invalid file. Server keys, team name, fleet view command, people, schedule and time to claim, rules for running the agent's playbooks, services whose changes a person must approve, and alert sources. The triage agent's internet access, listed hosts, and software review interval also apply independently. Each part with an outside test is saved only after the outside test passes. At startup, parts with an outside test apply in the background. The triage and review agents' setups, including model and reasoning effort, each run the agent's setup test. Each of the five sending services runs a service-specific check. MMHotline-down alarm setup runs in Cloudflare, and status-page setup posts the current page. Notes appear under each part's outcome. Notes identify omitted action kinds in rules for running the agent's playbooks and the next step for a newly configured sending service. Tested in Go, and used on real Linux servers at a cloud provider. |
| Setup, sign-in, and people | Setup creates one team from the settings file's team name and no person. Sign-in uses Cloudflare Access's verified token email claim or one-time sign-in links through an SSH forward. The People page is read-only. The settings file adds, changes, and deactivates people while retaining historical names. Deactivation is refused while a person owns a claimed incident or has taken on-call. Local commands list people and create sign-in links after checking the connecting process's Unix credentials. Sign-in was proved on a real Linux server with a sign-in link through an SSH forward. MMHotline used Cloudflare's real signing keys. Sign-in was also proved with tokens Cloudflare Access itself signed, including a person's own email sign-in. |
| Linux installation and optional backup | Repeatable `sudo mmhotline install` refuses until the settings file exists and passes checks. Install gives the settings file group `mmhotline` and mode 0640, and creates the database with the settings file's team name. Install sets up a protected systemd service, preserves state, and restarts the service on upgrades. When configured, a separate Litestream unit copies `mmhotline.db` to the configured object-storage replica. A read-only check tests whether uploads keep up. Linux installation and object-storage restore need platform verification. |
| Triage agent | Claude Code or Codex runs as a separate Unix user in a systemd sandbox, with restricted filesystem and network access. Every run receives the settings file's model and reasoning effort. All connections pass through the logging proxy, with `listed_hosts_only` or `any_site` internet access. At each run's start, MMHotline lists servers from every inventory in MMHotline's systems repository copy as proxy destinations at port 22. The triage agent reads the listed servers over SSH through the proxy. Hosts from vendor pages in `software_outside_the_distribution.yml` are also listed at each run's start for the software review. Setup test, runs, administrator page and command are built. Linux installation was proved on a Debian 13 test server ([evidence](../validation.md#triage-agent-on-linux)). The triage agent requests playbook runs. MMHotline reads action kinds from `mmx explain` and per-inventory rules for running the agent's playbooks from the settings file. An inventory without an entry, or an action kind without a saved setting, uses Get permission, then act with 1 approval. The page and command display the rules for running the agent's playbooks. The settings file's list of services whose changes a person must approve also appears on the page of rules for running the agent's playbooks. A playbook that acts on a listed service waits for a person. Any coding agent may be the optional review agent. An agent from a different vendor is recommended. After MMHotline reads the rules for running the agent's playbooks, the review agent must approve each playbook that could run without a person, with the triage agent's diagnosis, in the review agent's own empty home. The first two consecutive refusals return to the triage agent to revise before a person decides. MMHotline runs allowed playbooks directly with MMX under the copy's lock. For Get permission, then act, MMHotline sends an `sos` alert requesting approval. Any member may decide on the incident page or Approvals page. A request needing 2 approvals requires two different people. MMHotline persists the request and decision, then runs the approved playbook at the judged commit. The playbook's result or refusal reason reaches the agent in the session's next run. For Warn, wait, then act, MMHotline sends a `warn` alert before the run. The warning shows the run time and countdown, with Stop available to any member before the run time. MMHotline persists the warned run and runs the playbook when the run time arrives. The playbook's result or Stop reaches the agent in the session's next run. Each incident page shows the session's live agent steps with "Stop after the current step". The session page shows steps with "Stop after the current step", and lists the session's alerts, runs, and reports. A session containing only `warn` alerts can also be stopped. The dashboard shows the agent's current work, start time, and latest step. After Stop, the agent hands over the note and report in a final run. The hand-over run cannot run playbooks, then the session ends. Sessions hand `warn` and `sos` alerts to the agent, with one active triage run across MMHotline. Each run gets the task prompt, and later runs resume the agent session. Reports reach people at the report's alert severity. A quiet start message goes to the person on call. An SOS waits for the agent before anyone is woken, for the settings file's `triage_agent.sos_waits_for_the_agent_minutes`, 0 to 15. A session ends after the session's incidents are resolved. Each finding becomes a separate alert at the finding's severity. The scheduled software review is off until the settings file sets an interval of 1 to 8,760 hours instead of `off`; its task is the team's `scheduled_software_review_task.md`, and `run-software-review` runs one review to learn its cost. The next review starts one interval after the previous review started. A due review waits while a triage run is active. If a session needs the agent during a review, MMHotline stops the review after the current step and starts the session's run. A command starts a software review immediately. The Triage agent page shows the interval, next due time, and latest review. Administrators receive reminders 30 and 7 days before a Claude Code subscription token's year ends. MMHotline checks each agent's sign-in daily unless a run of the agent succeeded in the last 24 hours. If a triage session needs the agent during a check, MMHotline stops the check after the current step. Administrators get one alert when sign-in fails and one when sign-in works again. The page or `check-sign-in` command starts a check immediately. |
| Local command | [Commands](../../reference/mmhotline-command.md) check, apply, and show settings, manage contact methods, and start agent sign-in checks and software reviews. Commands display people, alert sources, schedules, rules for running the agent's playbooks, sending services, and triage and review agent setups. One command socket requires root and shares application operations with the pages. Results are text or JSON. Commands use secret files. Commands preview a settings file's schedule and [read incidents, alerts, triage sessions, and approval requests](../../reference/mmhotline-command.md#commands-that-read-what-happened). People and coding agents on the server use local commands when unable to pass Cloudflare Access. |
| Alert ingestion | The settings file defines alert sources with the team's IDs and signing secrets. A source accepts two signing secrets during rotation. Applying settings disables an existing source omitted from the settings file. Intake includes signed generic alerts, strict validation, deduplication, SOS correlation, `happened_at`, signed receipts, intake limits, and rejected-request counts. SOS and resolved-SOS bypass the per-source rate limit and installation-wide hourly limit. Refusing a correctly signed SOS opens an MMHotline SOS about the refusal. |
| Sending service setup | Email, Telegram, Slack, Pushover, and Twilio setups come from the settings file. Each setup is checked with the service before MMHotline saves the setup in the database. Read-only guided administrator pages name each settings file key, and reads expose no secrets. Removing a sending-service setup is refused while contact methods use the service. Setup changes rebuild running adapters without restarting MMHotline, so deliveries, Telegram polling, and Slack Socket Mode use the current setup. |
| Contact methods and routing | Email over SMTP, signed webhooks with address checks, Telegram polling, Pushover emergency priority with receipts, and Twilio setup and calls with status polling. Slack's outbound Socket Mode connection receives messages and button actions. One-time codes connect a Slack direct message or shared channel to a contact method. Saved contact methods are ready for immediate use. Saving a new contact method or changing a destination queues a test message, and the last test result is shown. Subscriptions, a durable outbox, a delivery throttle, and combined messages are built. |
| Chat and phone actions | Telegram and Slack “Claim this incident (assign it to me)” and “Resolve” buttons. Phone calls tell people to claim in MMHotline, or to acknowledge the alert in Pushover. Acknowledging the Pushover alert also claims the incident. |
| SOS lifecycle | Claims, takeover, resolution, manual escalation, revisions, and audit history are built. The lifecycle includes primary/backup/everyone_available deadlines and repeats. |
| Pushover | Personal Pushover contact methods, emergency SOS, quiet tests, and ordinary priorities. SOS notifications due together for a contact method combine into one emergency message. MMHotline records receipt acknowledgements and cancels obsolete emergency messages. Open emergency receipts have saved next-poll times, and MMHotline polls only due receipts, no faster than every five seconds. While a Pushover emergency receipt remains open, counted incidents create no new Pushover repeat message to the contact method. |
| SOS phone calls | One call about all waiting incidents, one active call per number, voicemail pacing, Send test limits, and a downloadable contact card. |
| Scheduling | By day of the week, By hour of the day, Weekly rotation with `next_in_rotation` backup, and Custom schedule. The settings file's `on_call_schedule` sets the schedule and time to claim. The command previews a settings file's schedule. Scheduling also covers handoff notices and which people a phone call can wake. |
| Custom schedule code | Starlark sandbox with Linux lockdown, stored shifts covering the next 14 days, and test cases. Applying settings runs the custom code for 14 days and checks the test cases before saving and activating the schedule. MMHotline retains the last 20 saved versions. |
| Availability | Personal unavailability, team mute, and taking on-call each have an end time and revision. Expiry ends the control, and a stale revision rejects a concurrent change. MMHotline recalculates coverage and current eligible incident notifications rather than replaying suppressed notifications ([availability rules](availability.md)). |
| Check-ins | Signed route, timing settings from the settings file, in-memory tracking, Warning and SOS. Resumed check-ins send resolved-warn for a Warning and automatically resolve an unclaimed SOS. A claimed SOS stays open, records the resumption, and sends a quiet message to the owner. |
| Planned downtime | Any team member may declare downtime for one alert source's missing check-ins. Check-ins can also list maintenance windows for an entire node, meaning a server identified by the sender. A maintenance window for the sending node suppresses missing-check-in alerts for the alert source. |
| Web interface | Dashboard, alert history, incident details and deliveries, the schedule and availability, alert sources, contact methods, Sending services for administrators, and People. Settings from the settings file appear read-only, with "Set in /etc/mmhotline/mmhotline_settings.yml" and the settings file's first line. The dashboard tells administrators about each part of the settings file that did not apply. |
| Dashboard's system parts | Each server in the monitoring tool's fleet view appears once: a red tile explains why a server needs attention, or a chip marks a healthy server. A root-run, socket-activated systemd unit reads the fleet view. Install writes the unit. The dashboard shows the newest commits in MMHotline's systems repository copy. The copy's `monkeymaxxing_stack_checklist.yml` supplies progress and the next item. The other open and deviated items appear in a collapsed section, each with a question to copy for the coding agent. One read of the fleet view, repository commits, and checklist serves all viewers for 10 seconds. Each missing dashboard data source gets one line explaining the problem. The `mmhotline-fleet-view.socket` and `mmhotline-fleet-view@.service` units still need a check on a real Linux server. |
| Status page | Optional public page in the team's Cloudflare account. Cloudflare serves `index.html` and `status.json` from an R2 object-storage bucket at the team's host name. On a laptop, `mmhotline status-page set-up` creates the bucket, host name, and URL rewrite rule. On the server, the settings file's `status_page` section configures the page, and the first apply publishes the page. Members post, update, and resolve notices from the incident page, the Status page page, or the command. The commands work on the server or on a laptop with only an R2 access key for the bucket. Tested against a fake of Cloudflare's API and R2, and set up in real Cloudflare accounts. |
| MMHotline-down alarm | An optional Cloudflare Worker watches MMHotline's check-ins from the team's Cloudflare account. A Worker is a small program Cloudflare runs. The MMHotline-down alarm's single Durable Object holds SQLite state and a timer, called an alarm, that sends notifications when MMHotline goes silent. The Worker is embedded in `mmhotline` and uploaded when the settings file's `mmhotline_down_alarm` section applies, using a token limited to Workers. If the initial check-in never succeeds, MMHotline restores the previous MMHotline-down alarm setup, or no setup for a first installation. MMHotline checks in every minute and within 5 seconds when held notifications change, such as at a handoff. Held notifications are ready-made Pushover requests and automated voice calls. If no check-in arrives, the alarm sends held notifications to the primary, and later to the backup an unclaimed SOS escalates to, after the settings file's wake times, such as 5 and 10 minutes after the last accepted check-in. During team mute, the delays start when mute ends, rounded up to the next minute. The Durable Object's alarm runs in Cloudflare and sends due held notifications. An unknown send outcome causes a resend. When storing a held message, the Worker refuses a control character in a header value. MMHotline sends the all-clear through the normal delivery path. After 10 minutes of failed check-ins, MMHotline raises a Warning for subscribed contact methods under normal routing rules. MMHotline uploads an upgraded Worker. The dashboard card offers "Send a test now". If the settings file omits `mmhotline_down_alarm`, MMHotline first checks in with no held messages, then removes the saved setup. The Worker's logic is tested under `node --test`, and the Go side is tested against a fake Cloudflare API. The Worker and Go code are tested together on a local stand-in, and the alarm was proved in a real Cloudflare account in November 2026: with MMHotline stopped, it woke the person on call and then everyone else available at the set wake times. |
| Development verification | Go unit, persistence, public HTTP workflow tests, local SMTP fixtures, and fake webhook, Telegram, Slack, Pushover, and Twilio services. Optional Mailpit acceptance checks are built. |

## Planned

- Remaining triage agent work: a fuller view of its reports and its playbook
  requests with their MMX runs on the incident page.
  Triage already handles `warn` and `sos` alerts, and rules for running the agent's playbooks decide
  whether a fix needs a person's permission. The [Status section](#status)
  lists the built restrictions, setup, runs, requests, rules for running the agent's playbooks,
  review agent, MMX runs, approvals, warnings and Stop, and triage sessions.
  Session start messages, SOS waiting, live steps, "Take over from the agent"
  on incident pages, and "Stop after the current step" on session pages are
  built. The dashboard's agent panel, hand-over, software review, daily
  sign-in checks, and token expiry reminders are also built
  ([triage agent](triage-agent.md)).

## Definition of done

- Implement each contract through the applicable browser actions, HTTP
  routes, and local commands, including meaningful failure and restart
  behavior ([contract map](../testing.md#contract-map)).
- Pass the focused tests and matching workflows listed in
  [testing](../testing.md) for the relevant build and platform.
- Keep usage documentation, feature status, and validation results consistent with the build.
