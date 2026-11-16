# Operation, persistence, and recovery

## Deployment boundary

One installation serves one team on its own server. A company with several
teams runs several installations. The team's settings live in one file,
`/etc/mmhotline/mmhotline_settings.yml`, which a deploy tool renders from the
systems repository and the team's vault, or an administrator edits with
`sudoedit`
([settings file](settings-file.md)). Setup creates the one team and its
database file ([storage](#storage)); the settings file's `people` adds the
people ([people](people.md#setup-and-first-administrator)). MMHotline receives signed alerts and check-ins from the team's tools on its private network address. People reach
MMHotline's pages through `cloudflared`, Cloudflare's tunnel connector.
`cloudflared` opens an outbound connection to Cloudflare.
The MMHotline server accepts no web connection from the internet
([sign-in](people.md#authentication-and-recovery)).
MMHotline makes outbound requests to the services that deliver MMHotline's
notifications. Twilio call status uses outbound polling. MMHotline opens an
outbound Socket Mode WebSocket to Slack for messages and button presses.
An SOS is the urgent alert that wakes the person on call, even at night;
to claim an incident is to say "I'm on it", which stops MMHotline
contacting the next person.

The application uses Go with cgo disabled, SQLite through the pure-Go
`modernc.org/sqlite` driver, and the standard library's `net/http` and
`html/template` for the web interface. The server administrator supplies a
Linux server with running systemd and sending service credentials.
For sign-in through Cloudflare Access, the administrator also supplies
`cloudflared` on the MMHotline server.
Cloudflare Access sign-in also requires an Access application for the team's
host name.
`sudo mmhotline install` configures the service from the settings file
([installation](#installation-and-setup)).

## Listeners

`mmhotline serve` takes `--state-dir DIR`, `--settings-file FILE`, and
`--command-socket PATH`, plus the two triage agent sockets and the fleet view
socket described below. The installed unit passes
`--settings-file /etc/mmhotline/mmhotline_settings.yml`. Four keys of the
settings file decide the listeners and the sign-in, and change only when
`serve` restarts ([restarts](settings-file.md#restarts)):

| Key | Meaning |
| --- | --- |
| `web_url` | The address people open; links in notifications and command output use it. An HTTPS origin such as `https://ops.example.com`, or `http://localhost:PORT` or `http://127.0.0.1:PORT` for a team without Cloudflare that opens MMHotline through an SSH forward |
| `web_listen_address` | The `host:port` where the pages listen, normally `127.0.0.1:8480` |
| `alert_intake_listen_address` | The `host:port` for the two signed sender routes: the server's private network address, such as `10.20.0.1:8484` |
| `cloudflare_access` | Optional: `team_domain`, the bare host name `<team name>.cloudflareaccess.com` with no `https://`, and `audience_tag`, the Access application's "Application Audience (AUD) Tag", from the Cloudflare dashboard: Zero Trust, Access controls, Applications, Configure the application, Additional settings. The `audience_tag` contains only ASCII letters and digits |

Without `cloudflare_access`, people sign in only with
[sign-in links](people.md#sign-in-links). At start, `serve` writes which
applies to its error output: "Sign-in: Cloudflare Access for DOMAIN, and
sign-in links" or "Sign-in: sign-in links only; the settings file has no
cloudflare_access". A settings file with a problem at start makes `serve` use
the server keys last applied. A listen address `serve` cannot open also
causes fallback to the server keys last applied
([start](settings-file.md#start)).

For pages, intake, and management, `serve` has two TCP listeners and one
Unix socket. The triage agent and fleet view sockets are additional sockets,
described below.

- **The web listener**, at `web_listen_address`, serves every page
  and `GET /sign-in/{token}` over plain HTTP. `cloudflared` forwards the
  team's host name to the web listener. An SSH forward reaches the web
  listener during a Cloudflare outage. The web listener has no sender routes.
  A loopback port is safe because page requests, except `GET /sign-in/{token}`,
  need a signed Cloudflare Access token or a sign-in cookie, which a
  local program cannot forge ([sign-in](people.md#sign-in-through-cloudflare-access)).
  The loopback address is the normal address, because only `cloudflared` and SSH
  forwards on the MMHotline server need to reach the pages.
- **The intake listener**, at `alert_intake_listen_address`, serves
  only `POST /alert-intake/{alert_source_id}` and
  `POST /check-in/{alert_source_id}`, over plain HTTP. All other paths
  return 404. The intake listener ignores Access tokens and sign-in cookies.
  Requests and receipts are signed, so the private network, normally
  WireGuard, needs no TLS. Bind the intake listener to the intended private interface.
  [Ingestion](ingestion.md#private-network-intake) explains why the sender
  routes are not on the web listener.
- **The command socket**, at `--command-socket`, serves only the local
  management API, after checking kernel peer credentials. Root is required on
  Linux because socket ownership alone would also admit the service user.
  On non-Linux development systems, the command socket admits the user
  running `serve`. The command socket has no write deadline:
  `settings apply` waits for any earlier settings-file application to finish,
  then for the outside tests of the settings-file parts. On Linux, only root reaches
  the command socket.

Cloudflare provides HTTPS for people; MMHotline has no TLS certificate flags.
Pages and the `mmhotline` command-line program call the same application
operations.

The command socket is created with mode 0600. MMHotline sets the umask before
listening so the socket is never briefly accessible to other users. A socket
file left by an earlier run is replaced; regular files are refused.

The optional triage agent and review agent use three more Unix sockets.
Supply all three of
`--triage-agent-run-socket PATH`, `--review-agent-run-socket PATH`, and
`--triage-agent-proxy-socket PATH` to `serve`, or none. Installation supplies
all three. Without the socket arguments,
the triage agent page and command can show state, but cannot start a setup
test.

Systemd listens on `/run/mmhotline-triage-agent/run.sock` and
`/run/mmhotline-review-agent/run.sock`, each owned by `mmhotline` with mode
0600. MMHotline connects to the first to start one sandboxed triage agent run,
and to the second to start one sandboxed review agent run. MMHotline itself
listens on `/run/mmhotline/triage-agent-proxy.sock` for the logging proxy.
The proxy socket has mode 0666 inside the mode-0700 `/run/mmhotline` directory;
other users cannot traverse `/run/mmhotline`. Systemd mounts only the proxy
socket into each run, so each agent's separate user can reach the proxy without
reaching MMHotline's other files. The [triage agent design](triage-agent.md#the-fence)
owns the sandbox and connection rules.

### The fleet view socket

The dashboard's [servers](interface.md#servers)
come from the monitoring tool's fleet view on the MMHotline server.
The monitoring tool's local socket admits only root. MMHotline's service has
`NoNewPrivileges=yes`, so it cannot use
`sudo`, and it should not: that service handles every alert and page. So
MMHotline starts the fleet view the way it starts triage runs, through a
socket that systemd owns. MMHotline never names the monitoring product. The settings file's `fleet_view_command`
holds the program and arguments as a list. The first item is the program's
absolute path, such as `[/usr/local/bin/MONITORING_TOOL, fleet, --json]`.

- Systemd listens on `/run/mmhotline-fleet-view.sock`, owned by `mmhotline`
  with mode 0600, so only MMHotline's user may connect. Each connection
  starts one instance of `mmhotline-fleet-view@.service` as root.
- The service runs `/usr/local/bin/mmhotline run-fleet-view-command
  --settings-file /etc/mmhotline/mmhotline_settings.yml`, which reads
  `fleet_view_command` from the settings file for each connection.
  `run-fleet-view-command` executes the first list item as the program and
  passes the remaining items as arguments, without a shell.
  The fleet view program receives standard input from the null device and
  writes standard output to the socket connection and standard error to the
  journal. The fleet view program's environment contains only `PATH`.
  The program must be an absolute path, so a search path never decides what
  runs as root. A changed command applies on the next fleet view socket
  connection, with no restart. Only root can change
  `/etc/mmhotline/mmhotline_settings.yml`. Before executing the command,
  run-fleet-view-command checks the settings file's owner and mode.
  MMHotline can only ask for one run of the command, with no input.
  A problem elsewhere in `/etc/mmhotline/mmhotline_settings.yml` does not
  stop the fleet view
  ([settings file](settings-file.md#the-fleet-view-command)). A fleet view
  exits nonzero while a server fails, so run-fleet-view-command ignores the
  command's exit status.
- The service keeps the file system read-only, hides MMHotline's state,
  `backup.env`, runtime files, and the triage agent's home, and allows only
  Unix sockets, because the fleet view comes from the monitoring tool's local
  socket. The settings file stays readable, because the service reads it; the
  command, the team's own root program, could read it too, but has no network
  to send it anywhere. Like MMHotline's own unit, the service also takes away
  every Linux capability and access to host devices. The service prohibits
  changing kernel settings, creating namespaces, and making privileged
  system calls.
- `RuntimeMaxSec=20` ends a hung command: twice MMHotline's 10-second wait,
  so a slow but working run is never cut short first. `MaxConnections=2` lets
  one new run start while a timed-out one is still being ended, and no more.
  `CollectMode=inactive-or-failed` removes a failed instance, so runs every
  10 seconds never pile up as failed units.
- `serve` connects only when the applied settings file has a
  `fleet_view_command` and `--fleet-view-socket PATH` is given.
  `serve` reads until the command ends, at most 10 seconds and 64 MiB.
  One read serves every viewer for 10 seconds
  ([interface](interface.md#the-monitoring-tools-fleet-view)
  gives the reasons and the fields read).

## What mmhotline serve keeps in memory and rebuilds at start

The database is the only durable record for MMHotline. `mmhotline serve`
also keeps these derived facts in memory:

- **Current contact method type instances.** Contact method type adapters
  read saved sending service setup.
  Startup builds the adapters from the database. Successful setup and
  removal rebuild the adapters in the running `mmhotline serve` process.
  Readers share one complete current set, so a delivery, page, or poller
  never sees a partially rebuilt set.
- **Check-in tracking.** Each alert source's signing secrets and latest
  check-in. An unchanged check-in causes no database write
  ([monitoring](monitoring.md#check-in-tracking-in-memory)).
- **The team's scheduler record.** The count of committed database changes,
  the count at the last successful scheduler visit, and the facts read after
  a visit: the next deadline and whether the active schedule uses custom
  schedule code.
  The record also tracks the last successful visit time, whether the first
  visit succeeded, whether a visit failed, and whether unfinished deliveries
  have been taken over.
  The next deadline covers notifications, retries, SOS responses and repeats,
  the end of a person's unavailability, the end of
  the team's notification mute, the end of a person's temporary on-call
  assignment, and scheduled changes of who is on call.

The background scheduler sends notifications, advances incidents, and checks
check-in times. About once a second, the scheduler
visits the installation's one team if the first visit has not succeeded,
a database transaction changed something, a deadline is due, or check-in
tracking has work.
Check-in work becomes due after one minute of silence, at the alert source's
Warning or SOS time, or when planned downtime ends. The scheduler also
visits the team 10 minutes after the last successful visit.
Visiting at least every 10 minutes bounds the effect of a wrong deadline.
Each visit dispatches at most one delivery attempt and one due Pushover
receipt action.
The scheduler then reads the team's facts again.
A failed visit is tried again on the next scheduler round. An idle visit writes nothing: continuous backup must not upload
database changes when nothing happened.

At startup, the scheduler visits the team to rebuild its facts. Requests can
be answered before the startup scheduler visit. A request loads missing
check-in facts first.
Before the first delivery, MMHotline takes over unfinished work from the
previous process ([recover current work](#recover-current-work)). Custom schedule
code has a separate loop, active only when the active schedule uses custom
schedule code. Every 5 seconds the loop checks the team's facts,
refreshing the facts after database changes.
The custom schedule loop runs independently of the incident
scheduler, so sandbox execution never delays incident work.

## Sending service results and chat buttons

No sending service connects to MMHotline; it opens every sending service connection itself.
Twilio uses outbound Call resource polling, with no inbound endpoint.
The shared scheduler fetches only active calls, at least five seconds apart,
until a final status or three minutes after creation. Stored attempts supply
the call IDs and creation times. In-memory deadlines avoid database writes
for unchanged responses. When `serve` rebuilds poll deadlines after a
restart, the deadlines place the first fetch five seconds later.
An idle installation makes no Twilio requests and writes no polling state.
[Notifications](notifications.md#sending-service-results) owns delivery outcomes.
MMHotline receives direct messages, mentions, and button presses from Slack
over an outbound Socket Mode connection, and from Telegram over long polling.

- **Slack** uses `apps.connections.open` and then the returned WebSocket URL.
  `serve` runs one connection beside its scheduler while Slack is set up.
  MMHotline's WebSocket reader acknowledges every received `envelope_id`
  before application work. A queue of 32 decoded envelopes lets
  acknowledgements continue while an API request waits for Slack's response,
  with bounded memory. Each envelope is at most 1 MiB,
  allowing full message blocks without unlimited input allocation. Application work is bounded to 30
  seconds; API requests allow 10 seconds. The authenticated `hello` must name
  the saved app ID, so an app-level token from
  another app cannot handle events.
  After a `disconnect` envelope or a closed connection, MMHotline obtains a
  fresh URL and reconnects. Pauses grow from one second to at most 30 seconds,
  preventing a tight failure loop while bounding recovery delay. Connection and handling
  failures share one server-log limit: at most one report every 10 minutes,
  even across setup changes. Saving setup cancels the old Slack connection
  loop and waits for the loop to stop before starting the replacement.
  Removing unused setup and shutting down close the connection.
  Slack requires no inbound route or request signature.
  If MMHotline's API request to reply to a Slack event fails, MMHotline
  reports the failure to the server log without discarding acknowledged
  envelopes.
  The application checks the press's workspace, conversation, and message
  against the delivery's saved sending service reference.
  The pressing Slack user must match a saved link to a current member
  in the delivery's workspace.
- **Telegram** needs no inbound endpoint. MMHotline long-polls the bot's
  `getUpdates`, keeping the offset in memory. A repeated update is harmless.
  Telegram keeps unconfirmed updates for 24 hours, so `mmhotline serve` picks them
  up after a restart. `mmhotline serve` runs the poller beside the background
  scheduler whenever Telegram is set up. Saving a setup starts or updates the
  poller; removing the setup stops it, cancelling the waiting request. Shutdown
  also stops polling. The old polling loop finishes before a new polling loop starts,
  so two loops never poll the same bot together. No service restart is required
  for setup changes. Each `getUpdates` waits up to 25 seconds for an update;
  after a failure the poller tries again in 5 seconds.
  An update that fails to be handled is reported and skipped, so the failed
  update cannot block later updates. A person presses the button again.

## Storage

MMHotline keeps one SQLite database, `mmhotline.db`, in the state directory.
The `mmhotline.db` database holds the team's name in the single row of
`team_info`, members and roles,
the hashes of sign-in links and cookies (`browser_sign_ins`), sending service setups, contact methods and subscriptions, Telegram start tokens, chat
connections, schedules, availability, alert sources and secrets, planned downtime, alerts,
incidents and history, and the delivery queue and attempts. The team has no ID.

The `sending_service_setups` table has one row per set-up sending service. `sending_service` is the
primary key, such as `telegram`. `settings_json` holds the non-secret settings
object; `secret_settings_json` holds the secret settings object. `set_up_at`
is the setup time in Unix seconds. `set_up_by` identifies the source of the
saved sending service setup as the settings file
/etc/mmhotline/mmhotline_settings.yml. Telegram's
non-secret settings are `telegram_bot_username` and numeric `telegram_bot_id`.
Telegram's secret setting is `telegram_bot_token`. There is no second setup file.

Setup creates the database and refuses to replace an existing `mmhotline.db`.
Opening state requires an existing regular `mmhotline.db` file with exactly
one `team_info` row.
Missing or invalid state tells the server administrator to run setup first.

### Database format and upgrades

An upgrade keeps the database and every row. A team installs a new release
over its old one and keeps its incidents, history, people's contact methods,
alert sources, and setups, so nobody adds their phone again.

The database format is the shape of `mmhotline.db`'s tables, numbered in
SQLite's `user_version`. Format 1 is the first release's. Setup creates the
newest format whole and records its number. Opening the database runs, in
order, each upgrade step that the database has not had, and each step
records the new number in the same transaction, so a step runs once, and an
interrupted upgrade leaves the database at the last format it finished.
After the release, the schema changes only in these ways:

- A new format adds tables and columns, with defaults for the existing rows.
- To allow a new value in a CHECK, a step may rebuild that one table,
  copying every row, because SQLite cannot change a CHECK in place.
- Nothing is renamed or removed. A table or column keeps its name even when
  the product's word for it changes: renaming would cost an upgrade step and
  a risk to every installation, for a name only developers read.

Opening refuses, and changes nothing, in two cases. A database without a
format number was set up by a build from before the first release, which
MMHotline cannot upgrade: the refusal says to move the state directory away
and run setup. A database of a newer format than the program knows was
written by a newer release, and an older program would misread it: the
refusal says to install that release or a newer one.

`internal/app/testdata/database_format_1.sql` keeps format 1 unchanged. A
test builds a database from it with rows, opens it, and requires the same
tables, columns, indexes, and triggers as a new database, with the rows
kept, so a schema change without its upgrade step fails the test.

The store opens one database connection with WAL journaling,
`synchronous=FULL`, a five-second busy timeout, foreign keys enabled, and
immediate transactions. SQLite's WAL and shared-memory files are working files;
[backup](#setup-backup-and-restore) describes continuous replication and safe
manual copies.

- **Each operation has one transaction.** An accepted alert, claim,
  escalation, or delivery result commits with its associated notification
  intent before reporting success.
  Consuming a Telegram start token, creating the chat contact
  method, and linking the Telegram user to the member commit together too.
- **Membership authorizes people.** A browser request names the person by
  sign-in email address, from a verified Cloudflare Access token or a sign-in
  cookie. Membership and role are checked in the transaction that
  performs the action. People commands use the application operation with a trusted local
  administrator identity.
  A record ID grants no access.
- **Sending service results identify their delivery.** Twilio call fetches use the
  attempt's stored call ID. Each Slack button's `value` carries an action
  and delivery ID and must match the recorded message. Telegram button data
  carries an action and delivery ID; a press must come from the recorded chat
  and message. A Telegram start token identifies the member and connection kind.
- **Restore needs one database.** Back up `mmhotline.db`; the token and contact
  method records are in the same file.
- **Memory is rebuilt.** Check-in tracking and the team's next deadline
  are rebuilt from the database after `mmhotline serve` starts and updated
  after commits. Unchanged check-ins cause no database write
  ([monitoring](monitoring.md#check-in-tracking-in-memory)).
  The database remains the only durable record.

## Durable facts

The database persists accepted alert identity, incident state and revision,
action history, coverage configuration, availability deadlines,
member/contact method state,
what each alert source's newest check-in said, when an alert source
whose check-ins stopped was last heard from,
planned downtime, notification intent, and attempt outcomes.
The persisted alert, incident, action, coverage, availability, member, contact
method, check-in, downtime, notification, and attempt facts support recovery,
diagnosis, policy, and history. Credential material is stored separately from human-readable alert records.

For an accepted mutation, commit its state and associated notification intent
as one logical operation before reporting success. A UI success banner or
receiver HTTP 2xx response must not precede a failed
durable write. External calls occur after intent is recorded, after database
transactions end and after locks protecting application state are released.

## Jobs and external side effects

Notifications use a shared durable queue, also called an outbox. The background
scheduler makes at most one delivery attempt per visit. Contact method type adapters do not
own separate queues. Every attempt has a stable identity and
explicit target, purpose, incident revision when relevant, deadline, and
outcome. Retries preserve the original queued delivery's identity.
SOS repeats are explicitly new attempts under incident policy.

Before an attempt starts, recheck whether the work is still eligible. After the attempt
completes, record its result without letting a stale result undo a claim,
resolution, reassignment, or contact method change. Sending service reports
only move a delivery forward, so a repeated or late report changes nothing.
No table of sending service event IDs is kept.
Cancellation cannot recall a sending service
request that has already been sent; expose what is known.

### Recover current work

After `mmhotline serve` starts, reload durable facts and reconcile unfinished attempts
before scheduling new work for the incident and recipient of each unfinished
attempt. Apply current membership, contact methods, availability, team mute,
and incident revisions.

- Retain ordinary undelivered notification intent under the retry policy;
  current-work recovery does not discard accepted pending deliveries.
- Cancel obsolete SOS work for claimed/resolved incidents or superseded
  targets. Record known and uncertain external results without undoing newer
  state.
- On the first visit to the team, take over what the previous process left
  unfinished in the database. Reserved attempts, recorded as being sent but
  lacking a saved result, count as “outcome unknown”.
  A reserved call or email is sent at once. A call with a known sending service ID
  stays active until polling finds its final status or three minutes have
  passed since the call's creation.
- For an open incident whose response deadline expired, advance to the next
  eligible escalation step once. Give the next step a full time to claim
  starting now.
  Skip unavailable targets under normal escalation rules, but do not skip
  eligible responders merely because several hypothetical windows fit inside
  the outage. At the repeating `everyone_available` step, retain the normal
  repeating target. For a one-person team, also retain the normal repeating target
  instead of inventing another escalation step.
- If only a repeat is overdue, make one current repeat due per incident revision
  and contact method, coalescing equivalent queued work. Schedule the
  next repeat from the time MMHotline reconciles the overdue repeat.
  Do not generate one job per missed interval.
- Recompute who is on call and whose unavailability has ended once per
  current-work recovery pass. Record the actual resulting change rather
  than replaying every historical shift boundary.
- Resume the triage session whose run the stop or restart cut short, with the
  alerts that run carried
  ([triage agent](triage-agent.md#after-a-restart)).

Team unmute uses the incident reconciliation described above.
A normal restart preserves when each alert source marked as stopped was
last heard from. An alert source without the mark was
heard less than a minute before the restart, and the unmarked source's silence
counts from the first database read after restart ([monitoring](monitoring.md#check-in-tracking-in-memory)). So a
restart never makes an alert source look healthy after a minute or more of
silence. The full Warning and SOS silence
thresholds start when MMHotline rebuilds check-in tracking after restart,
only for an alert source without the stopped mark.
MMHotline does not automatically detect or compensate for
periods when intake was unreachable
([monitoring](monitoring.md#when-mmhotline-cant-hear)).

[Notifications](notifications.md) owns each contact method type's retries and what happens
after an unknown sending service outcome. The
[intake limits](ingestion.md#intake-limits-and-rejected-requests) bound pending
deliveries. Restart recovery does not generate a burst of missed notifications.

## Diagnostics

The dashboard shows what needs a person, the SOS incidents being handled,
and current coverage. Below, the dashboard shows the servers, the newest
commits of MMHotline's copy of the systems repository, and the checklist's
open and deviated items. A missing source appears as one line naming the
missing source, with the technical cause folded.
An incident page lists the incident's deliveries and delivery status under
“Notifications sent”. Alert source pages show check-ins, planned downtime,
and rejected-request counts.
The [web interface](interface.md) owns the dashboard, incident pages, and
alert source pages and the labels on each page.
`mmhotline serve` reports background and sending service errors to its error output.
A panic in one round of a background loop is reported the same way, with
its stack, and the round counts as failed. The background visit, the
custom schedule loop, the MMHotline-down alarm check-in, and the handling of one
Telegram update or Slack envelope each recover from a panic. Otherwise one
bad stored row could stop all of `mmhotline serve`, intake included, at
every start. A failed visit is retried the next second, and a delivery
whose send panicked is sent again when its reservation ends.
`mmhotline serve` also writes each refused Cloudflare Access token to
standard error with the reason, such as "token audience does not include the configured Cloudflare Access audience tag", and
each sign-in link created, with the
person's name and sign-in email address.
The last successful Telegram `getUpdates` is kept in process memory.
While polling fails, `mmhotline serve` writes the last successful Telegram
`getUpdates` time to standard error with the failure:
“Telegram getUpdates is failing since {time} (last successful
getUpdates: {time})”, at once and then at most every 10 minutes, and says when
polling works again.
Delivery attempts keep diagnostic results separately from incident actions.
Do not publish credentials or secret request bodies when sharing diagnostics.

## Configuration and secrets

The triage agent's saved credential lives in `mmhotline.db` and its database
backup. MMHotline sends the credential over the run socket for each run; the
run sets the saved credential in the coding agent's environment, never in
an agent-owned file.
The credential comes from the settings file. See
[triage agent setup](triage-agent.md#triage-agent-setup) for credential types.

The settings file holds the team's settings and their secrets, such as alert
sources' signing secrets, inline: a deploy tool renders it from the systems
repository with the secrets from its vault. The settings file is owned by
root, read by MMHotline's group, and closed to other users.
MMHotline refuses a settings file without the required owner and permissions
([owner and mode](settings-file.md#owner-and-mode)). No problem message,
`settings show` output, page, or log shows a secret's value.
Each settings-file part is checked before applying.
A part that fails keeps the part's earlier setting and says why
([parts](settings-file.md#parts)). Before saving contact method changes,
MMHotline's contact method operation validates the proposed settings with
the contact method type's rules. Team settings are separate
from people's own preferences and incident state.

Sending service keys, signing secrets, webhook signing secrets, and bearer tokens
never belong in the repository or committed
example state. Tests use synthetic credentials and isolated stores.
Keep secrets outside the repository and restrict access to the account that
runs MMHotline. [Running MMHotline](../../reference/settings-file.md) lists
every key of the settings file.

Setup for the mail server, Telegram, Slack, Pushover, and Twilio comes from
the settings file's `sending_services`, one part for each service; the
administrator's guided Sending services page says what to create at each
service and which key each value goes in. MMHotline validates the token with
Telegram before saving Telegram's returned bot username and ID.
During mail server setup, MMHotline checks the SMTP connection, TLS, and
optional authentication, then closes the connection without sending.
SMTP settings and the
secret password are stored separately, as are Telegram settings and its token ([sending service setup](notifications.md#sending-service-setup)).
The settings file's `web_url` supplies the incident links in messages and
buttons.

The database holds the sending service setup that applied, and the settings
file says what it should be. `serve`
rebuilds its current contact method type instances at startup and after each successful
setup or removal. Startup reads saved rows. A save builds the replacement from
exactly the checked settings committed to the sending service's database row.
Removal drops the adapter instance for the removed sending service setup.
A setup lock serializes commits and replacement of the running adapter map.
A read/write lock publishes one immutable map at a time.
Readers keep the map without holding the lock
during sending service requests. A delivery, page, or poller already using
an adapter instance can continue using the instance.
The Telegram poller and Slack connection follow the current setup without a restart. Failed checks do
not replace saved setup or running adapters. Refusing removal or a bot change
while contact methods use Telegram prevents silent alert loss.

Slack setup stores the bot token and app-level token separately from the
checked workspace and app identity in `mmhotline.db`.
Contact methods keep conversation identifiers only.
The database backup therefore restores the Slack setup with the conversations.

Phone calls use the team's optional Twilio account, set up in the settings
file's `sending_services.twilio`. The account SID, dedicated API key SID,
calling number, and optional extra phone numbers on the MMHotline contact card
are public setup fields. The API key secret is stored
separately from the public fields. The public fields and API key secret live
in `mmhotline.db` and the database backup. No environment credentials or
public URL for Twilio to send call-status callbacks are needed.
See [Twilio setup](notifications.md#twilio-phone-calls) for
the sending service check and contact card changes.

Twilio's geographic permissions must allow the accepted countries.
Twilio's geographic permissions must block premium and high-risk ranges.
MMHotline itself refuses North American
toll-free, premium, 700, and 5XX numbers; restrictions inside other accepted
countries depend on Twilio's permissions.

### Fake sending services for development

**Built** for a webhook receiver, Telegram, Slack, Pushover, and Twilio.
`mmhotline-fake-sending-services` is a separate development program that imitates
Telegram, Slack, Pushover, Twilio, and a webhook receiver.
Email uses its saved SMTP setup even in development mode. Configure Mailpit,
whose web interface shows the email. The `mmhotline-fake-sending-services` web page
explains that email uses Mailpit through saved SMTP setup.
`mmhotline-fake-sending-services` is never linked into `mmhotline` and never
deployed. `MMHOTLINE_FAKE_SENDING_SERVICES_URL`
points chat, push, and phone sending service adapters at
`mmhotline-fake-sending-services`. The fake services' test credentials still
require database-backed sending service setup. Telegram is not set up
automatically: put `fake-telegram-bot-token` in the settings file and run
`settings apply`. Setup
checks and deliveries then contact the fake Telegram. While fake mode is set,
the fake webhook receiver is the only private address allowed by MMHotline's
outgoing webhook HTTP client's destination restrictions.
[Local development](../local-development.md#develop-without-real-sending-services)
owns interactive setup. [Testing](../testing.md#fake-sending-services) describes
the automated fixtures.

## Sending service setup and diagnostics

Each team configures its own sending services. [Connect a sending service](../../how-to/connect-a-sending-service.md)
describe database-backed sending service setup. Create the team's Slack app through
the manifest link on MMHotline's Slack sending service page and save the
app's bot and app-level tokens. Messages and buttons
arrive through the outbound Socket Mode connection. Twilio geographic permissions must block premium
and high-risk ranges in the accepted countries.

While running, check sending service backlog, calls whose final status is still unknown,
Telegram's last successful `getUpdates`, and how carriers label the calling
numbers. The fake sending services test sending service outcomes; they do not prove real delivery.

## Setup, backup, and restore

### Installation and setup

`sudo mmhotline install` owns the Linux installation and takes no flags.
`install` checks Linux, root, and running systemd, then the settings file
`/etc/mmhotline/mmhotline_settings.yml`, before making changes: the file must
exist, pass the owner and mode check, and pass the static check
([settings file](settings-file.md)). A missing file, or a settings file with a problem,
stops install with a message that says where the file goes, lists each
problem by key path, and links to the settings file reference. Install never writes the
settings file's contents: the contents belong to the team and the team's deploy tool.

Install creates the no-login `mmhotline` system user and its home
and state directory, `/var/lib/mmhotline`. The state directory is owned by
`mmhotline`, group `mmhotline`, with mode 0770, and the database files have
mode 0660, so that the backup's own user, in group `mmhotline`, can write the
database ([continuous backup](#continuous-backup-and-restore)). Install
changes the owner and mode of each file through the file it opened, with
`O_NOFOLLOW`: the `mmhotline` user can rename entries in the state directory
while root installs, and a change by path could follow a symbolic link put
there to another file. Install copies the invoked binary to
`/usr/local/bin/mmhotline` with root ownership and mode 0755, using a temporary
file and rename. If install is invoked from `/usr/local/bin/mmhotline`,
install needs no copy.

Install then gives the settings file owner `root`, group `mmhotline`, and mode
0640, so MMHotline's user reads it and only root changes it. A settings file
written before the first install may not yet have group `mmhotline`. Install
creates the `mmhotline` group when needed. The settings file must pass the root-owner
and mode check before install sets the group.

Install also creates the no-login `mmhotline-triage-agent` system user and its
mode-0700 home, `/var/lib/mmhotline-triage-agent`. Install writes
`mmhotline-triage-agent-run.socket` and `mmhotline-triage-agent-run@.service`
in `/etc/systemd/system`, owned by root with mode 0644. The socket unit is
enabled and started; each connection starts a separate sandboxed service
instance as the agent's user. The [triage agent design](triage-agent.md#one-systemd-service-instance-per-run)
explains why systemd starts each run rather than MMHotline switching users.

Install also creates the no-login `mmhotline-review-agent` system user, with
its own group and no home directory, and writes
`mmhotline-review-agent-run.socket` and `mmhotline-review-agent-run@.service`
the same way. A review agent run has the triage agent run's sandbox, as
`mmhotline-review-agent`. The two agents share no user, group, home, or
writable directory
([the review agent's isolation](triage-agent.md#the-review-agent)).

Install writes `mmhotline-fleet-view.socket` and
`mmhotline-fleet-view@.service` in `/etc/systemd/system`, owned by root with
mode 0644, and enables and starts the socket
([the fleet view socket](#the-fleet-view-socket)). Install writes the fleet
view socket and service unit files on every install, whether or not the
settings file has a `fleet_view_command`:
with the units in place, adding the command later needs only
`settings apply`. Without the command nothing connects, and an idle socket
costs nothing.

When missing, `mmhotline.db` is created through the same setup operation as
`mmhotline setup --state-dir DIR --settings-file FILE`, with the settings
file's `team_name`: valid UTF-8 containing 1–100 Unicode code points after
trimming surrounding space.
Existing state stays; a changed `team_name` applies with `settings apply`.
Direct setup refuses to replace existing state. Neither `install` nor
`setup` creates a person. The settings file's `people` part adds the people
when `serve` starts.

The generated `mmhotline.service` unit belongs to MMHotline.
Install replaces `mmhotline.service` when the unit's contents differ.
The `mmhotline.service` unit runs
`mmhotline serve --state-dir /var/lib/mmhotline --settings-file /etc/mmhotline/mmhotline_settings.yml`
with the command, triage agent, and fleet view sockets, and loads no
environment file. The unit includes the binary checksum, so install detects
upgrades by comparing the generated unit with the installed unit.
The comparison works even when install runs from the installed path,
without another state file or a database write. An unchanged rerun makes no changes.
Changes to the binary or unit, a new database, a settings file whose owner or
mode install corrected, or a stopped service cause a restart; the unit is
enabled when needed. A change of the settings file's contents applies with
`sudo mmhotline settings apply`, which restarts MMHotline itself when a key
that needs a restart changed ([restarts](settings-file.md#restarts)).

The application runs as `mmhotline` and reads the settings file through its
group. Systemd
creates `/run/mmhotline` with mode 0700. The `mmhotline.service` unit has
`Type=notify`. `serve` tells systemd when the listeners accept connections
and the settings-file parts without an outside test have applied.
Only after readiness does `systemctl start` or `systemctl restart` return.
A command run immediately afterward can therefore reach the command socket,
as required by restore, setup playbooks, and `settings apply` after restarting.
The unit uses `NoNewPrivileges=yes`,
`ProtectSystem=strict`, writable `/var/lib/mmhotline`, `ProtectHome=yes`,
and `PrivateTmp=yes`. The `mmhotline.service` unit also restricts access and
operations MMHotline never uses, so a fault reaches less of the server.
The unit removes every Linux capability and access to host devices,
kernel logs, and other users' processes in `/proc`.
The unit prohibits changing kernel settings, loading or unloading kernel
modules, changing control groups, changing the clock or host name, and
creating namespaces.
The unit also prohibits realtime scheduling, creating set-user-ID files,
and memory that is writable and executable.
Network families other than Unix, IPv4, and IPv6, and privileged system calls
are prohibited (`SystemCallFilter=@system-service ~@privileged`).
Resource-limit calls stay allowed because the child processes running custom
schedule code set limits before installing the sandbox's seccomp filter. For the same children, `mmhotline.service` alone also allows the
`seccomp` system call (`SystemCallFilter=seccomp`), which
`@system-service` leaves out: without it, every custom schedule fails with
"failed loading seccomp filter: operation not permitted".
The backup unit applies the restrictions listed for `mmhotline.service`.
`systemd-analyze security mmhotline` rates the unit 1.4, "OK".
Both units restart the service 2 seconds after it stops (`Restart=always`,
`RestartSec=2`), with no start limit (`StartLimitIntervalSec=0`). By
default, systemd stops restarting a service that failed 5 times in 10
seconds. A fault that repeats at every start would then leave MMHotline
down, intake included, or the backup stopped, until a person started it.
Install waits at most 30 seconds for a successful response to `people list`
over the command socket, and includes recent journal lines on timeout.
Install then sets up
[backup](#continuous-backup-and-restore) when configured and prints the web
URL. When `web_url` is an HTTPS address and
`/etc/mmhotline/mmhotline_settings.yml` has no `cloudflare_access`, install
prints the next step: set up `cloudflared` and a
Cloudflare Access application for that URL, add `cloudflare_access` to the
settings file, and run `sudo mmhotline settings apply`.

### Continuous backup and restore

Litestream remains a separate program and systemd service. Litestream's
cloud SDKs and build requirements stay out of MMHotline's cgo-disabled
application.
The documented version is 0.5.18. Moving from 0.5.17 was checked on
2026-11-12: both versions give the same `litestream ltx` columns, the same
`ltx/0` file names, and the same LTX header, which `backup check-uploads`
reads, and 0.5.18 replicated to and restored from a real B2 bucket. It runs
with
`litestream replicate -config CONFIG_PATH` and
`litestream restore -o PATH REPLICA_URL`.

When `/etc/mmhotline/backup.env` and the Litestream program both exist,
install creates the no-login `mmhotline-backup` system user, with primary group
`mmhotline` and no home directory, writes `/etc/mmhotline/litestream.yml`,
and writes, enables, and starts `mmhotline-backup.service`. The service
runs as `mmhotline-backup`, loads environment variables only from `backup.env`, and
replicates
`/var/lib/mmhotline/mmhotline.db` to `${MMHOTLINE_BACKUP_REPLICA_URL}` as
`litestream.yml` says. Install restarts the service when the unit or
`litestream.yml` changed.

When install creates the database, because none existed, it leaves the
marker file `/var/lib/mmhotline/new-database-waits-for-the-backup-bucket-check`.
While the marker exists, each install first lists the bucket with
`litestream ltx`. If the bucket already holds a backup, or cannot be listed,
install writes the backup unit but does not start it, and fails with the
steps to restore or to start empty in a new folder; once the listing shows
no backup, install removes the marker and starts the backup. The marker
lives in the state directory, so the restore block, which replaces that
directory, removes it, and a database a deploy tool restores before install
never has one. Reason: on a new server, Litestream would save the new, empty
database as the bucket's newest state, and the restore would then bring
back the empty database. A database that existed before install, such as
one a deploy tool restored, is backed up at once. The restore block starts
the backup on the restored database; the next install enables it.
Install searches `PATH` for Litestream and uses the executable's absolute
path in the backup unit.
A missing program with existing backup settings stops install with
installation guidance.
Missing backup settings print `Backup: not set up` and the documentation URL.
The installer reads no backup credentials; systemd loads `backup.env` for
Litestream. Object-storage credentials never enter `mmhotline serve`'s
environment.

Litestream runs as its own user so that `mmhotline serve` cannot read the
keys from Litestream's process. A process can read the environment and memory
of another process of the same user through `/proc`, and
`ProtectProc=invisible` hides only other users' processes. A systemd
credential (`LoadCredential=`) would not help, because Litestream holds the
keys in its memory and in its environment either way. Litestream writes the
database, so group `mmhotline` may write the state directory and the
database files. SQLite gives a new WAL file the database file's mode, so
both users can always open it.

Litestream keeps its working files, copies of the changes it captured until
they are uploaded, in `/var/lib/mmhotline-backup/mmhotline.db-litestream`.
Systemd creates `/var/lib/mmhotline-backup` for the backup unit
(`StateDirectory=mmhotline-backup`, mode 0700), owned by `mmhotline-backup`.
By default Litestream keeps the files beside the database, in the state
directory. There the files kept the owner of whichever user ran Litestream
before. When the backup changed from `mmhotline` to its own user,
Litestream could no longer write them: it kept running and captured
nothing. Giving the files to `mmhotline-backup` as root would mean walking a
directory that the `mmhotline` user can change, where a symbolic link could
lead the change to another file. In a directory only `mmhotline-backup` can
write, no step as root ever changes the files' owner, after an upgrade or
a restore. Litestream 0.5.18 sets the folder only in its configuration
file (`meta-path`), not in the replicate command's arguments, so install
writes `/etc/mmhotline/litestream.yml`, root-owned, mode 0644, with no
credential. Litestream puts the replica URL from `backup.env` in place of
`${MMHOTLINE_BACKUP_REPLICA_URL}` when it reads the file.

Litestream starts an empty working folder, after an upgrade or a restore,
by fetching the backup's newest level-0 change and numbering the next
changes after it. The backup's transaction numbers continue without a gap.
The restore procedure therefore moves the old working folder aside with
the old state: the old folder can hold transactions the backup never
received, and numbering after them would leave a gap in the backup.

`backup.env` stays a separate file, not part of the settings file:
Litestream, a separate program, reads it through its own unit; a restore on
a new server reads it before MMHotline or its database exists; and
`mmhotline backup check-uploads` reads it. A deploy tool renders `backup.env`
with secrets from the team's vault, as for
`/etc/mmhotline/mmhotline_settings.yml`
([settings file](settings-file.md#why-backupenv-stays-separate)).

The replica includes saved sending service tokens and webhook bearer tokens, so
restrict access to the replica as well as the local database. The settings
file and `backup.env` remain separate files and need their own
secure copies; a deploy tool renders both again from the systems repository
and its vault.

Backup is optional. A tried restore is encouraged and is not an installation
gate, because the team's data can be recreated if backup was failing.
Replication is asynchronous: a restore can lose any changes not yet uploaded.
There is no fixed recovery-time or maximum-data-loss promise. Restore contains
only the facts in the replica; effects outside that snapshot can repeat.
The usual [current-work recovery](#recover-current-work) applies on startup.

The [backup guide](../../how-to/back-up-and-restore-the-database.md#restore) owns the whole restore procedure:
stop `mmhotline.service` and `mmhotline-backup.service`, preserve the old
state directory including WAL, and Litestream's old working folder,
restore the database into a fresh state directory,
set the restored state directory and files' owner and group to `mmhotline`,
with mode 0770 for the directory and 0660 for the database, then start `mmhotline.service` and `mmhotline-backup.service`. MMHotline adds no
restore command and one read-only backup check, below. Keep separate secure
copies of the settings file and `backup.env`. A running backup service is not evidence of a
usable object-storage replica: the check shows whether uploads keep up, and
only a tried restore shows that the replica restores.

For a manual copy, stop `mmhotline.service` and `mmhotline-backup.service`
cleanly before copying `mmhotline.db`.
Closing the database checkpoints its WAL. After an abrupt stop, restart and
shut down cleanly before copying; never discard a remaining WAL.

#### The upload check

When uploads fail, such as after a revoked key, with a bucket that refuses
them, or without a network, Litestream logs the error and keeps running, so
upload failures do not make `mmhotline-backup.service` fail or notify the
team. Litestream also keeps running when it cannot capture the database's
changes, such as when it cannot write its working files. The age of the newest
upload proves nothing either: an idle MMHotline writes nothing, by design,
so continuous backup uploads nothing for hours. MMHotline therefore adds one
read-only check, [`sudo mmhotline backup check-uploads`](../../reference/mmhotline-command.md#mmhotline-backup-check-uploads),
for a monitoring tool to run every 10 minutes.

Litestream keeps each change it captured from the database as a local file
named by its transaction IDs, `<min_txid>-<max_txid>.ltx` with 16 lowercase
hexadecimal digits each, in `/var/lib/mmhotline-backup/mmhotline.db-litestream/ltx/0/`,
until the change is uploaded and compacted; it always keeps the newest file.

The check first compares the database with what Litestream captured.
Comparing the backup with Litestream's captured changes alone passes a
Litestream that keeps running but captures nothing: the check would compare
an old captured change with an old upload and find them equal. SQLite
appends each committed transaction to the database's write-ahead log,
`/var/lib/mmhotline/mmhotline.db-wal`, and Litestream captures from that
log. Each captured file's LTX header records how far into which log
Litestream captured: the log offset and size, and the two salts that SQLite
gives the log each time it starts the log over. The check reads the log the
way SQLite and Litestream read it, frame by frame, checking each frame's
salts and running checksum, and finds the end of the newest committed
transaction. The newest captured file covers that transaction when it has
the log's salts and reaches at least as far. Litestream looks at the log
every second, so a just-written transaction may not be captured yet. The
check then waits up to 30 seconds, rereading the captured files four times
a second. The check passes this step when a captured file covers the
transaction, or when Litestream captures a newer change from a log with
other salts: Litestream starts the log over only after capturing all of it.
Otherwise the backup stopped: exit 1, before listing the backup. Thirty
seconds also covers a Litestream that systemd is restarting, and the check
of a stopped backup still ends within a minute.

A log with no committed transaction cannot show what Litestream missed.
A running Litestream always leaves a committed transaction in the log: its
open read transaction keeps SQLite from emptying the log, and right after
Litestream empties the log itself, it writes to the log and captures that
write. An empty or missing log therefore means SQLite copied the log into
the database file without Litestream, such as at a clean restart of
`mmhotline serve` while Litestream was stopped or failing. The check then
waits, the same way, until Litestream's newest captured file is no older
than the database file's last write. A missing database file has nothing
to capture.

The check then reads the local `<min_txid>-<max_txid>.ltx` filenames and
modification times again, since Litestream may have captured more during
the wait, and lists the backup
with `litestream ltx REPLICA_URL` and reads its `max_txid` column by the
header's name, not by position. Uploads keep up when the backup holds the
newest captured transaction, or when every captured file the backup lacks
was written no more than 5 minutes ago: exit 0. A captured file the backup
lacks that is older than 5 minutes means uploads stopped: exit 1. When the
check cannot tell, it exits 3: backup is not set up, Litestream is missing,
the directory is empty or missing, the newest captured file has no LTX
header, the list fails, or the list has no `max_txid` column. An empty directory means the backup never started,
because a running backup captures the database within seconds of starting,
for example when `mmhotline install` was not run again after `backup.env`
was added; answering 0 there would pass a backup that never ran.
Litestream uploads a change within about a second; 5 minutes leaves room for
a slow upload and for a short outage during which Litestream keeps retrying
uploads, and a stopped backup is still found within one 10-minute
monitoring interval.
The check lists level 0 only, Litestream's default for `ltx`, so
each run is usually one list request: Litestream reads the highest uploaded
transaction ID from the backup's level-0 files and keeps the newest file when it
compacts, so level 0 holds the newest uploaded transaction.

The exit codes fit the command's scheme, where 1 is a failure, 4 a refused
command or bad arguments, and 3 is otherwise unused.
For `mmhotline backup check-uploads`, 1 means
only that the backup stopped, in capture or in upload; every other failure of the check, such as an
unreadable file, exits 3, so a monitoring tool never reads "cannot tell" as
a stopped backup. A monitoring tool that distinguishes stopped uploads from
an unknown upload state reads 1 as failed and 3 as unknown. A monitoring tool
without separate failed and unknown outcomes alerts on exit codes 1 and 3.

The check is a local command run as root, never a request over the command
socket. `mmhotline backup check-uploads` reads `backup.env` directly,
as systemd's `EnvironmentFile=` reads the lines in the backup guide.
The command passes the environment variables read from `backup.env`, with
`PATH`,
only to the `litestream` process it starts. Object-storage credentials still
never enter `mmhotline serve`. When the list fails, the check prints
Litestream's last error line with each `backup.env` value of 8 characters
or more replaced by the variable's name. Values shorter than 8 characters
are left unchanged. Replacing short flags and words such as a region would
garble the line.
The check reads, as root, files in directories that the `mmhotline` and
`mmhotline-backup` users can write, so it opens each file without following
a symbolic link and reads only regular files.
A stopped `mmhotline-backup.service` fails the check once MMHotline writes
a change, so the monitoring tool needs no separate check that the service
is active.

The check depends on Litestream 0.5's local directory layout, its LTX
header (`github.com/superfly/ltx` 0.5.2: the log offset at byte 48, the
size at byte 56, and the salts at bytes 64 and 68), and the output of its
`ltx` command. Whenever the documented Litestream version changes, the
[release gates](development.md#release-gates) rerun
`mmhotline backup check-uploads` on a real bucket.

## Acceptance evidence

Test failed admission writes, restart with open/claimed/resolved incidents,
pending notifications, in-flight uncertain calls, expired availability, stale
stale custom schedule or worker completions, and failed candidate activation.

Verify call polling against the fake sending services and real delivery separately.
Keep application, platform, sending service, and manual backup/restore evidence
distinguishable in the [validation record](../validation.md).
