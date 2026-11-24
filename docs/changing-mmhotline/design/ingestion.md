# Alert ingestion

Signed JSON intake for `generic` alert sources is built, including the route,
signing, validation, deduplication, and matching SOS alerts to incidents.
Built limits are 120 new alerts a minute per alert source, 1,000 unresolved
incidents, 10,000 pending deliveries per installation, and 250 pending SOS
deliveries per alert source. Also built:
the `happened_at` field, signed receipts, the intake limits and the SOS
exemption, the SOS MMHotline opens when MMHotline refuses an SOS, the rejected
requests panel, and the check-in route. The alert-intake and check-in routes
use only the alert source ID; extra path segments return 404.
The alert sources come from
the team's [settings file](settings-file.md#alert-sources). An SOS is the
urgent alert that wakes the person on call, even at night; claiming an
incident tells the team you are handling it, and MMHotline stops contacting
the next person.

## Private-network intake

`mmhotline serve` answers the two signed sender routes only on its intake
listener, a plain HTTP `host:port` set by the settings file's required
`alert_intake_listen_address`. Bind the intake listener to the server's
private network address, such as `10.20.0.1:8484`, a WireGuard address in a
typical setup. The intake listener serves nothing else; pages and every
other path return 404. Cloudflare Access tokens and sign-in cookies have no
effect on the intake listener.
The [web listener](operation.md#listeners) has no sender routes.

The sender routes are kept off the web address because that address sits
behind Cloudflare Access, which refuses a request that carries no person's
login, so an intake URL there would never work for a sender. Your monitoring
tool on the MMHotline server and on every other server sends to the one
intake address. Every request is signed and every successful answer carries
a signed receipt, so the private network needs no TLS or other protection.

## Alert sources in the settings file

The settings file's `alert_sources` lists each alert source: the team's own
`alert_source_id`, a `name`, one or two `signing_secrets`, its check-in
times, `warn_after_no_check_in_for_minutes` and
`sos_after_no_check_in_for_minutes`, and the optional `repeated_warnings`
([repeated warnings](#repeated-warnings)). Applying the settings file saves and enables each listed source, and disables
each source the settings file leaves out. A disabled source's signing secrets
stop working at once, and the source's alerts and check-ins stay.
Listing a disabled source again enables the source with the listed secrets.
A signing secret still stored for the source keeps its ID and creation time.

The secrets come from the team's vault, not from MMHotline, because the
monitoring tool on every server signs with them and reads them from the same
vault ([why one file](settings-file.md#why-one-file)).

The [command reference](../../reference/mmhotline-command.md#mmhotline-alert-sources-list) owns
`alert-sources list` and its JSON fields. `alert-sources list` returns the
alert intake address, `alert_intake_address`, such as `http://10.20.0.1:8484`,
built from `alert_intake_listen_address`.
The command gives each alert source an `intake_url` and `check_in_url`.
The command includes secret IDs and creation times, never stored secret
values, and the current check-in state shown on each alert source's page.
The web list folds connection values under “Connection details” and labels
the administrator-only identifier “Signing secret ID”, so an identifier
cannot be mistaken for a credential. The settings-file path stays under
“Where alert sources are set”; request-format instructions are folded separately.

## Alert vocabulary

An alert is an immutable record received from an alert source, created by hand
(SOS), or generated as a notice from MMHotline. The alert record keeps the
source, receipt time, severity, title, message, and normalization notes.
An SOS additionally opens an
[incident](incidents.md) or joins an existing incident's history.
Notification attempts are separate records.

| Severity | Meaning | Resolved alert |
| --- | --- | --- |
| info | Useful information; no immediate intervention required | resolved-info |
| warn | Something needs attention but should not wake someone | resolved-warn |
| sos | A problem requiring a response as soon as possible | resolved-sos |

Only SOS has an incident lifecycle. Resolved-info and resolved-warn are
informational alerts delivered to contact methods subscribed to `info` and
`warn`, respectively. A contact method's subscription receives the base
severity and the resolved variant
([subscriptions](notifications.md#contact-methods-and-subscriptions)).
MMHotline does not match resolved-info or resolved-warn to earlier alerts or
maintain open warning incidents.

Missing severity becomes warn. Add the visible note: “Received without severity, defaulting to 'warn'”.
An unknown or malformed severity is a validation error; the severity is not silently
treated as a missing value. The error says
`severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos`.
Empty sender fields return
`sender_alert_id and sender_incident_key must be nonempty when present`.

## Alert intake

Every alert source belongs to a team and has a kind. The built kind is
`generic`: signed JSON, as below. An administrator creates an alert source
through the [settings file](settings-file.md#alert-sources).
The [alert sources page](interface.md#connecting-an-alert-sender) is read-only.
A monitoring tool sends ordinary generic alerts; MMHotline requires no
monitoring-tool-specific format. Every alert source posts to one route:

~~~text
POST /alert-intake/{alert_source_id}
~~~

The alert source ID is the team's own, such as `monitoring`: 1 to 64 ASCII
letters, digits, `_`, and `-`. A 32-character hexadecimal ID generated by
MMHotline before the settings file existed also satisfies the ID pattern.
`system`, `manual`, and `mmhotline_test_sos` name MMHotline's own alerts, the
SOS incidents a person creates, and the test SOS of `incidents send-test-sos`,
and are refused. The ID names the alert source; the signature
authenticates the request. A sender is set up
with three values: the alert intake address, such as `http://10.20.0.1:8484`
([private-network intake](#private-network-intake)); the alert source ID; and
the signing secret. One MMHotline serves one team. A company with several
on-call teams runs one MMHotline per team, and a sender alerts a team by
sending to that team's MMHotline.

The body:

~~~json
{
  "sender_alert_id": "backup-2026-09-29",
  "sender_incident_key": "nightly-backup",
  "severity": "sos",
  "title": "Nightly backup failed",
  "message": "pg_dump exited 1: disk full on /backups.",
  "happened_at": "2026-09-29T03:00:12Z"
}
~~~

The intake format permits omitting `sender_alert_id`, the sender's alert ID.
The sender retry rule below requires an unchanged alert ID across attempts,
so MMHotline stores one alert.
`sender_incident_key` is optional: the sender's key for one problem, so a
later `resolved-sos` with the same key closes the incident
([identity](#identity-replay-and-resolution)).

`happened_at` is optional: when the problem happened, as an RFC 3339 time on
the sender's clock. `happened_at` is for display only. Receipt time,
ordering, deadlines, and escalation all use MMHotline's clock. When the
receipt comes more than two minutes after `happened_at`, the alert page
shows the sender’s event time and receipt time through the shared page clock
in the team’s time zone, with the delay in minutes. The alert’s notifications
keep their UTC format: “Happened at 14:02 UTC. Received at 14:31 UTC
(29 minutes later).”

Validate title/message types and lengths, request size, content type, and
severity before admission. The body limit is 64 KiB, which holds a 16 KiB
message. A title is required and has 1–256 Unicode characters; the message is
a required string of at most 16 KiB in UTF-8 and may be empty. Optional
`sender_alert_id` and `sender_incident_key` fields are nonempty UTF-8 strings
of at most 256 bytes each. Optional `happened_at` is an RFC 3339 time. The
64 KiB body, 256-character title, 16 KiB message, and 256-byte sender-field
limits accommodate a short alert plus useful diagnostic text while keeping
storage and rendering bounded; [defaults](defaults.md) lists them for review.
Reject oversized fields without truncating identifiers or admitting an alert.
Treat text as text when rendering HTML, messages, or diagnostics. Do not fetch
arbitrary message URLs as part of accepting an alert. The
[intake limits](#intake-limits-and-rejected-requests) bound how many alerts
are admitted.

| Response | When |
| --- | --- |
| `200` with the acceptance receipt | Accepted and committed, or a duplicate of an accepted alert |
| `400` naming the field | Invalid body, or a `sender_alert_id` already used for a different alert; signed when the request's signature was verified |
| `400`, unsigned | The `Content-Type` is not `application/json`, or the body could not be read |
| `401`, unsigned | Unknown source, bad signature, or the sender's clock more than 5 minutes off, answered identically |
| `413`, unsigned | Body over 64 KiB, alert or check-in |
| `429` | Rate limited; signed when the request's signature was verified |
| `503` | The alert could not be committed; the sender sends it again later |

A successful response is
`{"mmhotline_alert_id":"...","mmhotline_incident_id":"...","already_received":false}`.
Omit `mmhotline_incident_id` when there is no incident; `already_received` is true
for a duplicate. `mmhotline_alert_id` identifies the durable alert. For a
warn folded into an earlier one, it is the earlier warn's ID, and the answer
adds `"recorded_as_repeat_of_earlier_alert":true`
([repeated warnings](#repeated-warnings)).
The successful response is not proof of delivery to a contact method or
acknowledgement.
Rejected input creates no accepted alert. Responses also carry a receipt
signature
([receipts](#receipts)).

A sender states a retry deadline. Until the retry deadline, the sender retries
unless MMHotline returns a `200` with a valid receipt or a signed `400`.
A verified `200` confirms acceptance; a signed `400` confirms permanent
rejection. Every attempt carries the alert's unchanged `sender_alert_id`,
so MMHotline keeps one alert.

Every response except a verified `200` or signed `400` is a failed attempt
and requires a retry before the deadline. An unsigned `400` and a `413`
cannot prove that MMHotline answered ([receipts](#receipts)). A sender
that fits its alerts to the limits before sending never meets a `413`, so
one comes from a fault that a later attempt may not have, and sending again
costs only a duplicate request.

### Intake limits and rejected requests

Built. An alert source admits at most 120 new alerts per UTC minute.
Authenticated duplicates identify the original accepted alert without
consuming the allowance of 120 new alerts per alert source per UTC minute.
An installation admits at most 1,000 unresolved incidents and 10,000 pending
deliveries, with at most 250 pending SOS deliveries per alert source. The incident limit rejects creation of a new incident, but allows
alerts that join or resolve an existing incident.
Reaching an applicable limit rejects admission with `429` before any partial
alert is committed.

- An installation admits at most 1,000 new Warning and Info alerts per UTC
  hour: every alert except SOS and resolved-SOS counts. Beyond the
  installation's hourly limit or the per-source minute limit,
  MMHotline answers `429` with `Retry-After`, and the sender retries later.
- SOS and resolved-SOS alerts are also exempt from the per-source limit.
  Creating a new SOS incident is subject to the installation's limit of
  1,000 unresolved incidents. One phone call is about all of a
  person's waiting incidents ([incidents](incidents.md)). Part of the
  10,000 pending deliveries is reserved for SOS, so ordinary deliveries never
  block SOS admission ([notifications](notifications.md)): ordinary
  deliveries stop at 9,000.
- Each alert source may have at most 250 pending SOS deliveries. Beyond
  this share, MMHotline refuses a new SOS from the alert source with `429`.
  So one alert source cannot take the room that other alert sources' SOS
  alerts need ([the queue limits](notifications.md)).
- A warn from an alert source is saved and acknowledged as any alert is.
  When the settings file's `triage_agent.warn_waits_for_the_agent_minutes` is
  above 0 (15 when left out), the triage agent is set up, and no person has
  taken the open triage session over, MMHotline queues no notification for
  the warn at intake: the background loop delivers it, or the agent takes it
  over ([a warn waits for the agent](triage-agent.md#a-warn-waits-for-the-agent)).
- An SOS whose first waking step waits for the triage agent queues no
  email or chat at intake either, so the alert source's share of pending SOS
  deliveries does not refuse it then; they go when the wait is over
  ([an SOS waits for the agent](triage-agent.md#an-sos-waits-for-the-agent)).
- If MMHotline refuses a correctly signed SOS, MMHotline opens an SOS
  incident for the team about the refusal.
  Examples include an invalid alert, a sender's clock more than 5 minutes
  off, 1,000 unresolved incidents already in the installation, or an alert
  source with 250 SOS notifications waiting to be sent.
  The refusal incident is titled
  "MMHotline is refusing SOS alerts from “{source}”: {reason}".
  Further refusals from the source whose SOS was refused join the source's
  open refusal incident.
  A request whose signature is more than 5 minutes off, and whose alert
  MMHotline already received, opens nothing: the alert was handled when it
  first arrived. Its replay key, the `sender_alert_id` or else the
  signature, is kept with the alert. Without this rule, someone who captured
  an SOS request on the private network could send it again after each
  refusal incident is resolved and wake the person on call each time. A
  sender's retry signs the body again with the current time
  ([retries](../../reference/alert-requests.md#retries)), so this changes
  nothing for an honest sender. A new alert from a sender whose clock is
  off still opens the refusal incident.

  The refusal incident is exempt from the unresolved incident limit, with
  at most one open refusal incident per alert source. It is also exempt
  from the pending-delivery limits, so it opens and notifies people even
  when the queue is full. When someone has claimed the refusal incident, a
  further refusal goes to that person only when no earlier message to that
  person is still waiting to be sent.
  A request with a wrong signature never opens anything, so a stranger
  who knows an alert source ID cannot wake the team. Every refused alert
  writes one line to the service log, "Alert refused from alert source ID:
  sender_alert_id "…", severity "…": reason", so a refusal can be traced on
  the server. When an invalid field precedes `severity` in the JSON body,
  MMHotline still reads the severity of a correctly signed request.
  MMHotline uses the severity to decide whether to open a refusal SOS.
- A correctly signed SOS is never refused because nobody can be contacted.
  For a team with no members yet, MMHotline keeps the incident open with the
  history line "Nobody to contact yet: the team has no members".
  MMHotline contacts the first person added at the scheduler's next
  evaluation of the team's notifications.
  On 2026-10-27 a fresh `util01` refused a real SOS with HTTP 400 in that
  state, and the sender dropped the SOS.
- An installation has at most 100 alert sources.

Each alert source page shows one panel, “Rejected requests in the last 24
hours: {n}”, with the reasons: too many alerts, wrong signature, the sender's
clock more than 5 minutes off, or an alert that is not valid. The signature is
checked before the clock, so a correctly signed request from a sender with a
wrong clock is told apart. Administrators are told at most once an hour per alert source:
"Alert source “{name}” is sending too many alerts: MMHotline
refuses some of them". The counts
are kept in memory in the server process, for existing alert sources only, so garbage
requests cannot force database writes. The counts start again after a
restart.
Notices about MMHotline's own limits go only to administrators' contact
methods, never to webhooks.

### Repeated warnings

Built. An alert source's `repeated_warnings` setting in the settings file
says what intake does with a warn that repeats an earlier one:

- `keep_each`, the default when the setting is left out: every warn is
  its own alert, delivered as usual. Reason for the default: for most
  senders a changed message under one key ("retry underway", then "restore
  required") is new news. MMWatchdog's fleet hub must also keep every
  warning: its warning that a new server has a finding has the same key and
  first line as the first warning, and must be investigated.
- `fold_by_sender_incident_key_and_first_message_line`: repeats are folded
  into the earlier warn. This is for a sender such as MMWatchdog's nodes
  sending directly while the fleet hub is down, under their own alert
  source, such as `mmwatchdog_nodes_while_the_hub_is_down`. Thousands of
  servers then send one finding, with the same key and the same exact
  finding on the first line of the message. Without folding, the burst uses
  up the 120 alerts a minute of the source and the 1,000 Warning and Info
  alerts an hour of the installation, and unrelated warnings are refused for
  the rest of the hour.

For a folding source, a warn is a **repeat** when it has a
`sender_incident_key`, and an earlier warn of the same alert source has the
same key and the same first line of its message, was received within the
last hour, and is unresolved. Unresolved means that no `resolved-warn` with
the same source and key arrived after it, the same matching intake uses for
resolved messages. The first line is the message up to its first line break.
An alert without a severity is the warn intake saves it as.

- The earlier warn's repeat count goes up by one, and it keeps the repeat's
  title as the last repeat's title. No alert row is added, so storage stays
  bounded. The alert's page and `alerts list` show the count and the title.
- The repeat is not delivered again, and starts or joins nothing.
- A repeat is recognized before both intake limits, so it counts toward
  neither, and a burst of repeats leaves room for unrelated alerts.
- A warn without a key is never a repeat: two keyless warnings may be
  unrelated. A new exact finding under the key has a different first line,
  so it is a new alert, delivered and investigated. An SOS is never folded,
  and SOS alerts never count toward the limits anyway.
- A `resolved-warn` ends the folding window, so the next outage starts
  fresh. So does the hour: a finding that lasts longer is delivered again
  once an hour, so people hear it goes on, and the installation's hourly
  limit sees at most one alert an hour for it.
- A check without an exact finding, such as an HTTPS check, puts its own
  failure message on the first line. A message that differs only in a
  number, such as a response time, is then not folded. That errs toward an
  extra alert.

The answer to the sender for a repeat is a `200`, as for an accepted alert,
with the earlier warn's ID:
`{"mmhotline_alert_id":"<the earlier warn's ID>","already_received":false,"recorded_as_repeat_of_earlier_alert":true}`.
The existing fields keep their meaning, so a sender needs no change.
`recorded_as_repeat_of_earlier_alert` appears only when true, so the person
reading a sender's log can tell why the ID names another alert.

**A folded repeat keeps its retry identity.** Its replay key (the
`sender_alert_id`, or else the signature) and a SHA-256 of its severity,
title, message, and key are kept with the earlier warn's ID for 24 hours,
and checked before folding and before both limits. A sender's retry after a
lost receipt is answered as the duplicate it is,
`{"mmhotline_alert_id":"<the earlier warn's ID>","already_received":true,"recorded_as_repeat_of_earlier_alert":true}`,
and is never admitted as new or counted again, even after a `resolved-warn`
ended the window. A different alert that reuses the `sender_alert_id` is
refused with today's signed `400`, "sender_alert_id already used for a
different alert". Reason for 24 hours: a sender retries until its retry
deadline, minutes or hours after the first attempt. Each alert source keeps
at most 20,000 such identities, about 10 MB at about 500 bytes each: room
for 5,000 servers each sending 4 findings in a day. Past the bound a repeat
is still folded and counted, without its retry identity. A retry of it is
then folded again, which only adds one to the repeat count.

## Alert source signing

Senders sign each request with their signing secret; the secret itself never
travels:

~~~text
Mmhotline-Signature: timestamp_unix_seconds=<unix seconds>,hmac_sha256_hex=<hex HMAC-SHA256(secret, timestamp + "." + raw body)>
~~~

The header contains `timestamp_unix_seconds` followed by `hmac_sha256_hex`,
each exactly once. Anything else returns 401.
MMHotline rejects a bad signature, or a timestamp more
than five minutes from its clock, before admission, and compares signatures in
constant time. A signing secret is 32 random bytes, written in the settings
file as 64 hexadecimal characters, such as the output of
`openssl rand -hex 32`. Senders decode the hexadecimal representation before
signing. A source can hold two secrets at once, so rotation adds the new one
to the file, updates the sender, and removes the old one without dropping
alerts.

The Alert sources page, `/alert-sources`, shows every member the sources,
read-only. The Alert sources page displays
"Set in /etc/mmhotline/mmhotline_settings.yml, in
alert_sources" and the settings file's first line when the line is a comment.

The Alert sources page shows “Alert intake address”,
built from `alert_intake_listen_address`, and the alert source ID as separate
values. Below the alert intake address and alert source ID, show the full
URLs:
`POST <address>/alert-intake/<alert_source_id>` and
`POST <address>/check-in/<alert_source_id>`. Administrators also see each
signing secret's ID and creation time, never the secret's value. A source the file
no longer lists is shown as disabled. Sender examples use
`MMHOTLINE_ALERT_INTAKE_URL` for the full alert-intake URL and
`MMHOTLINE_SOURCE_SIGNING_SECRET` for the hexadecimal signing secret,
decoded to bytes before signing.

Someone who captures a request learns nothing that authenticates new requests. A replay
inside the tolerance
is a duplicate alert and changes nothing: the sender alert ID deduplicates it,
and an alert without a `sender_alert_id` is identified by its signature.

## Check-ins

**Built.** A sender checks in with MMHotline every 30 seconds: each
check-in is a signed message that says the sender still runs
([monitoring](monitoring.md#check-ins-from-the-sender) owns what
MMHotline does with it). An alert source sends check-ins to MMHotline.
MMHotline also sends check-ins to the
[MMHotline-down alarm](mmhotline-down-alarm-and-status-page.md#the-mmhotline-down-alarm).
An alert source's check-ins use the check-in route:

~~~text
POST /check-in/{alert_source_id}
~~~

A check-in request is signed with the alert source's signing secret and
`Mmhotline-Signature`, exactly as alerts are. Check-in requests go to the
alert intake address. The body names
the node that sent it (`sent_by_node_name`, so MMHotline knows which listed
maintenance window is the sender's own) and lists the nodes currently in
whole-node maintenance. The sender leaves out an empty maintenance list:

~~~json
{
  "sent_at": "2026-09-30T14:02:30Z",
  "sent_by_node_name": "util01",
  "nodes_in_whole_node_maintenance": [
    {
      "node_name": "app001",
      "maintenance_until": "2026-09-30T15:02:00Z",
      "reason": "moving the database",
      "started_by": "barbara"
    }
  ]
}
~~~

`sent_at` and `sent_by_node_name` are required; the list holds at most 1,000
nodes, each with `node_name` and `maintenance_until`. The body limit is the
same 64 KiB as for alerts, and a larger body gets an unsigned `413`, so the
sender caps the list by its encoded size, leaving out `reason` and
`started_by` first. The check-in and alert-intake routes decode strictly.
A body signed for check-in is rejected by alert intake, and a body signed
for alert intake is rejected by check-in.
A check-in is not an alert: the check-in creates no alert and does not
count toward the alert limits. An alert source may send at most 12 check-ins a minute, counted in memory; more are refused with `429`. A
successful answer is `{"check_in_received_at":"..."}`, signed as a
[receipt](#receipts).

MMHotline answers a check-in once it has recorded the check-in in
memory. MMHotline commits to the database before answering only when the
check-in changes something about the alert source
([check-in tracking in memory](monitoring.md#check-in-tracking-in-memory)).
Every accepted alert-intake request is committed to the database before the
answer.

## Receipts

**Built.** MMHotline
signs every `2xx` response to alert intake and check-ins, keyed with the
signing secret that signed the request:

~~~text
Mmhotline-Receipt-Signature: timestamp_unix_seconds=<unix seconds>,hmac_sha256_hex=<hex HMAC-SHA256(secret, "mmhotline-receipt." + route + "." + status + "." + request_hmac_hex + "." + timestamp + "." + raw response body)>
~~~

`route` is `alert-intake` or `check-in`, `status` is the HTTP status code
(for example `200`), and `request_hmac_hex` is the `hmac_sha256_hex` of the
request's own `Mmhotline-Signature`.
A receipt authenticates the request, the route used, the response status,
and the response body. Another program might answer at MMHotline's address,
for example by taking MMHotline's port while MMHotline is stopped.
The receipt prevents the answering program from forging a response by:

- echoing the signed request back as a response;
- replaying an alert-intake request to check-in, or a check-in request to
  alert intake, to collect a signed rejection;
- reusing an answer to an earlier request;
- changing the response's status code.

A sender treats an unsigned or wrongly signed `2xx` as a failed attempt
and sends again later. The sender therefore counts an alert as accepted by
MMHotline only when MMHotline provides a valid signed `2xx` answer.
MMHotline also signs `400` and `429` answers to a request whose signature
MMHotline verified. The signature lets the sender distinguish MMHotline's
permanent rejection or rate limit from an answer by someone else.
`401` and `503` are never signed. A `413` is
never signed either: MMHotline refuses an oversized body before reading it,
so it cannot check the signature; a sender that fits its alerts to the
limits before sending never meets one.

MMHotline recognizes an alert it already has by its alert source and
`sender_alert_id` for as long as it holds the alert
([identity](#identity-replay-and-resolution)). A sender sends an alert again
for at most one day, so a rule that deletes old alerts must keep the alerts
longer than one day.

## Built-in alert source types

The built alert source type is `generic`. The team's own programs send signed
alerts in the JSON format described under [alert intake](#alert-intake).
MMHotline does not run executables supplied by senders.

## Identity, replay, and resolution

Internal alert and incident IDs are stable and distinct from the sender's
keys. A sender's key is not an authorization credential. Titles are never used
to match alerts.

- **Alerts** are identified by alert source and sender alert ID, or by the
  request signature when no sender alert ID is present. A repeat returns
  the original acceptance in a newly signed response. A repeat changes
  nothing.
- **A reused sender alert ID.** A repeat must say what the stored alert
  says: the same `severity`, `title`, `message`, and `sender_incident_key`.
  A missing severity counts as `warn`, as it was stored. `happened_at` is
  not compared, because a sender may set it again on a retry. An alert that
  reuses a sender alert ID with different content is refused with a signed
  `400`, "sender_alert_id already used for a different alert", and is never
  answered as already received. A refused SOS opens MMHotline's own SOS, as
  any refused SOS does ([intake limits](#intake-limits-and-rejected-requests)).
  The comparison reads the stored alert's own fields, so it covers every
  stored alert and needs no extra column. A repeat folded into an earlier
  warn has no row of its own, so its content is kept as a SHA-256 for 24
  hours ([repeated warnings](#repeated-warnings)).
- **Incidents** are keyed by source and sender incident key, and the key
  matches any unresolved incident (open or claimed). An SOS whose key has no
  unresolved incident opens one. Another SOS with the key of an open incident
  joins that incident's history and does not contact responders again: the
  incident's escalation is already waking people. Another SOS with the key
  of a claimed incident joins its history and goes to the person who claimed
  the incident, once for each joining SOS, through that person's contact
  methods subscribed to SOS, waking ones included. Nobody else is being woken
  for a claimed incident, and the problem came back
  ([incidents](incidents.md)).
- **Resolution.** A `resolved-sos` with the key of an unresolved incident
  resolves the incident, with the source as the actor. The incident
  transition sends the single resolved-sos notification. The resolving
  alert is not routed separately.
- **An unmatched resolution** is stored as an accepted alert in the alert
  history, with a note naming the unmatched key, and sends no notification.
  The sender still gets `200`.
- **After resolution**, the key no longer matches, so a new failure with the
  same key opens a new incident; nothing reopens.
- **An SOS without an incident key** opens its own incident, which a person
  resolves.

## Other alert producers

Manual SOS creation and alerts about missing check-ins
([monitoring](monitoring.md)) use the same alert admission and incident operations, with
an attributable source. Changes of the scheduled primary person on call
([handoffs](scheduling.md#preview-and-activation)), escalation
([incidents](incidents.md#escalation-path)), and changes to personal
availability, team mute, or who has taken on-call
([availability](availability.md)) generate notices from MMHotline.
The linked designs specify each notice's severity.
Handoff, escalation, and availability notices are warning or info and never
create SOS incidents. MMHotline also opens an SOS when it refuses an SOS
([intake limits](#intake-limits-and-rejected-requests)).

The triage agent's reports are alerts from MMHotline too, at the severity
the agent chose. A report at sos opens an SOS incident only when the incidents it concerns
include no unresolved one ([triage agent](triage-agent.md#reports-to-people)).

## Acceptance evidence

Exercise the HTTP alert-intake route on the private-network intake listener
for `generic` sources: valid and invalid payloads,
missing severity, bad and expired signatures, the old and new signing
secrets during rotation,
an unknown source ID, the body limit, persistence failure, source validation failures,
duplicates and replays, an SOS joining its
unresolved incident, resolution by key, an unmatched resolution, a failure
after resolution, and the answers of the alert-intake and check-in routes.
Verify that an alert confirmed by a successful receipt survives a process
restart and remains inspectable.

For [repeated warnings](#repeated-warnings), test that a `keep_each` source,
and a source without the setting, delivers a second warning with the same
key and first line; that 5,000 repeats in one minute from a folding source
are delivered once while an unrelated warn from the same source and one from
another source are accepted; that a keyless warn, a new first line, and an
SOS are never folded; that a `resolved-warn` and the hour end the window;
that a retry of a folded repeat is a duplicate and a reused
`sender_alert_id` with different content is refused; and that the settings
check refuses an unknown value.

Also test `happened_at` validation and the gap
message; the check-in route, including a body signed for the alert-intake
route;
receipt signatures on each signed status and their absence on `401` and
`503`; the installation-wide Warning and Info limit, the 100
alert sources, and SOS passing the per-source minute limit and the
installation-wide Warning and Info hourly limit; the SOS MMHotline opens when
it refuses an SOS; and the rejected requests panel, with bad-signature counts
never written to the database.
