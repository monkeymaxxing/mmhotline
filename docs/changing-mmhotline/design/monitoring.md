# Check-ins and planned downtime

The team's tools send signed alerts to MMHotline. The program sending the alerts, such as the monitoring
tool, also checks in with MMHotline every 30 seconds. A check-in is a
periodic message that says the sending program still runs, so MMHotline can
tell a silent sender from a quiet system.

If the sending program's check-ins stop, MMHotline generates alerts through
the incident and delivery operations used for other alerts. MMHotline sends
a Warning, which wakes nobody, and then an SOS, the urgent alert that wakes
the person on call, even at night.
Claiming an incident tells the team you are handling it, and MMHotline stops
contacting the next person. Check-ins and planned downtime are built.

This page is about an alert source's check-ins with MMHotline. MMHotline's
own check-ins with the MMHotline-down alarm, which watch MMHotline itself, are described in
[the MMHotline-down alarm](mmhotline-down-alarm-and-status-page.md#the-mmhotline-down-alarm).

[Planned downtime](#planned-downtime) suppresses alerts about missing check-ins
until a chosen time. MMHotline does not monitor services or scheduled jobs,
or connect into the team's network. The team's monitoring tool monitors
services and scheduled jobs inside the team's network.

## Check-ins from the sender

A sender sends a signed check-in every 30 seconds to the
[check-in route](ingestion.md#check-ins), using its alert source's
signing secret. A monitoring tool's central server can send for the systems it
monitors; a single monitored server can send for itself. Each message names
the server running the sender, called the sending node.

Each message also lists servers in whole-node maintenance, with an end time for
each server ([check-in body](ingestion.md#check-ins)).
MMHotline uses the listed whole-node maintenance windows for
[planned downtime](#planned-downtime).

Missing check-ins can reveal a datacenter outage, network partition, or
monitoring tool failure that prevents the sender from reporting alerts.

Any signed alert or check-in counts as hearing from the alert source.
So an alert keeps a Warning or SOS about missing check-ins from going out.
Only a check-in clears a Warning or SOS about missing check-ins that already
went out. MMHotline hears alerts from another component using the check-in
sender's alert source, but the alerts do not
mean check-ins have resumed. MMHotline timestamps receipt of the alert or
check-in using MMHotline's clock, never the sender's.

[Check-in tracking in memory](#check-in-tracking-in-memory) says
where MMHotline keeps the alert and check-in receipt times and how a
restart affects the recorded receipt times.

MMHotline starts expecting check-ins from an alert source when the alert
source's first check-in arrives.
Until then its page shows "Waiting for the first check-in", and an alert
source that never sends one, such as the team's own script, is never
expected to check in.

Every team member can see the alert sources and each alert source's page
(`/alert-sources/{alert_source_id}`), which shows its check-ins and
planned downtime. Two keys of each alert source in the
[settings file](settings-file.md#alert-sources) set the timing, and the page
shows them read-only:

| Shown on the page | Settings file key | Stored as | Allowed values |
| --- | --- | --- | --- |
| "Warning" | `warn_after_no_check_in_for_minutes` | `warn_after_no_check_in_for` | 1 to 60 minutes, or `off` |
| "Wake the person on call" | `sos_after_no_check_in_for_minutes` | `sos_after_no_check_in_for` | 1 to 60 minutes, longer than the Warning time when Warning is not `off` |

A check-in can arrive late because of a temporary connection failure; a
Warning after 2 minutes and an SOS after 5, as the example in Connect a tool that sends alerts
has them, allow for that. The
alert source page uses the alert source name and leads with the current
check-in status and MMHotline's response, so a person checking a silent
sender sees the answer before setup facts. “Check-ins arriving” means a
check-in arrived within the last minute. “No recent check-in” means neither
an alert nor a check-in arrived within the last minute. A recent signed
alert without a recent check-in is labelled “Receiving alerts, but no recent
check-in”, because alerts delay silence alerts but do not clear an existing
missing-check-in alert. These labels do not claim the monitored services are
healthy or that a Warning or SOS has already been sent. A disabled source
says “Disabled”. The alert sources list reuses each source page's status, including in-memory receipt times, so list and detail agree.

For an alert source named “Monitoring on util.example.com”, the page shows
“Last check-in from alert source “Monitoring on util.example.com”: 14:02,
1 minute ago.” Times use the team's time zone through the shared page clock.
The “Missing-check-in rules” panel shows the read-only delays. The settings
file path and apply instructions are folded under “Where the alert source is set”.

Missing check-ins produce the alerts below, with the alert source as the
incident key:

- Warning: "“Monitoring on util.example.com” hasn't checked in with
  MMHotline for 2 minutes: it may have stopped, or its server may be down"
- SOS: "“Monitoring on util.example.com” hasn't checked in with MMHotline
  for 5 minutes: it may have stopped, or its server may be down"
- Resolution: "“Monitoring on util.example.com” checks in with MMHotline
  again"

The Warning and the SOS carry the same message: "Alert source “Monitoring
on util.example.com” normally checks in with MMHotline every 30 seconds to show the sending program is running.
Check the sending program, the server running the program, and the network connection from the server to MMHotline."

[When the problem clears](#when-the-problem-clears) says what happens to the
incident.

### Check-in tracking in memory

MMHotline keeps each alert source's latest check-in in process memory.
A check-in causes no database write unless check-in tracking needs a
database change listed below.

For every enabled alert source, MMHotline keeps in memory:

- the alert source's signing secrets, to check the signature of each
  check-in;
- when MMHotline last heard from the alert source, and when the latest
  check-in arrived;
- what the newest check-in said: its `sent_at`, the node that sent it,
  and the nodes in whole-node maintenance;
- how many check-ins arrived in the current UTC clock minute, for the
  [limit of 12 a minute](ingestion.md#check-ins);
- whether the database marks the alert source as stopped.

The two arrival times and the count exist only in memory. The newest
`sent_at` can also be newer in memory than in the database. The remaining
check-in tracking state is a copy of the database. MMHotline reads the database when the first
check-in request, check-in status request, or scheduler visit needs check-in
tracking. MMHotline reads the database again after every change to the
team's alert sources or signing secrets.

**When a check-in arrives,** MMHotline checks the signature and the timestamp
in `Mmhotline-Signature` under the
[signing rules](ingestion.md#alert-source-signing). If the signature and
timestamp pass validation, MMHotline decodes the message. If the message is
valid and the per-minute limit allows another check-in, MMHotline counts
the check-in against the limit. MMHotline then records the arrival time in
memory and answers with the signed receipt.

Once check-in tracking is loaded, MMHotline opens no database for a check-in
that needs none of the database changes below. The next check-in replaces
the current check-in 30 seconds later. Stopping MMHotline loses the recent
arrival times kept only in memory, as the restart rules below describe.

**Check-in tracking writes the database** only for:

- **The alert source's first check-in.** From then on MMHotline expects
  check-ins from the alert source.
- **A check-in that says something new:** another node sent it, or its
  list of nodes in whole-node maintenance differs.
  [Planned downtime](#planned-downtime) depends on the list of nodes in
  whole-node maintenance.
- **Check-ins stopping.** When MMHotline has heard nothing from an
  alert source for 1 minute, it writes when it last heard from the alert
  source and when the last check-in arrived, and marks the alert source
  as stopped (`check_ins_stopped`). 1 minute is the shortest Warning
  time a team can set, so no Warning waits for the mark.
- **Check-ins resuming.** The first check-in from an alert source
  marked as stopped removes the mark. MMHotline handles any earlier
  missing-check-in Warning or SOS in the transaction that removes the
  stopped mark, using the [resumption rules](#when-the-problem-clears).

An alert is written to the database as every alert is
([ingestion](ingestion.md)), and also counts as hearing from the alert source
in memory.

**Noticing that check-ins stopped.** Once a second MMHotline goes
through the alert sources in its memory. MMHotline reads the database only
when an alert source meets a condition below:

- The alert source has just been silent for 1 minute.
- The alert source is marked as stopped, and the next Warning, SOS, or
  planned-downtime deadline has arrived.

For the initial Warning and SOS deadlines, MMHotline adds the configured
delays to the stored time MMHotline last heard from the alert source.
The planned-downtime deadline uses the stored downtime end time. MMHotline
keeps the next Warning, SOS, or planned-downtime deadline in memory. Once
the SOS is out, check-in tracking creates no further alerts about missing
check-ins until a person resolves the SOS incident or a check-in arrives.

**After `mmhotline serve` starts,** the memory of the MMHotline that ran
before is gone. MMHotline reads the team’s database
([what MMHotline keeps in
memory](operation.md#what-mmhotline-serve-keeps-in-memory-and-rebuilds-at-start)):

- **An alert source marked as stopped** keeps its stored times. MMHotline
  counts the marked alert source's silence exactly.
- **An alert source without the mark** has no durable record of the latest
  arrival times. So MMHotline counts the
  unmarked alert source's silence from the moment the new MMHotline read
  the database. Until the alert source's next check-in arrives, the alert
  source's page shows the database read time as the last check-in.

So after a restart, the Warning and the SOS about an alert source
that went silent less than a minute before come late. The Warning and SOS
are late by at most the minute of silence before MMHotline stopped, plus
the time the restart took. The delay also includes the time until the new
MMHotline has read the database. No Warning or SOS comes early, and
none is lost. An
alert source that had been silent for a minute or more is not affected.

## Planned downtime

Planned downtime is a time during which MMHotline does not alert about missing
check-ins from one alert source. Planned downtime always has an end time,
at most 7 days ahead. Planned downtime differs from
[team mute](availability.md#team-mute): team mute stops delivering the team's
notifications while alerts and incidents continue, and planned downtime stops
one alert source's missing check-ins from making alerts at all.

**By hand.** Each alert source page with check-ins has a "Planned
downtime" form: "Until" (labelled with the team's time zone, date and time,
required), "Reason" (optional, at most 200 characters), and the button "Start
planned downtime". While planned downtime is active, the page offers "End
planned downtime now". Any team member may start or end planned downtime.
An Info alert from MMHotline in the alert history records who started or
ended planned downtime:
"{member} started planned downtime for alert source “{name}” until {time}",
"{member} ended planned downtime for alert source “{name}”".
Starting a new planned downtime replaces the active one. The page shows,
for example:

- "Until 15:00, in 1 hour, started by Barbara: moving the database."
  The status panel says MMHotline makes no alerts about missing check-ins
  during downtime and ordinary signed alerts still arrive.

**From a sender's maintenance window.** A whole-node maintenance window
listed for the sending node counts as planned downtime for its check-ins.

MMHotline learns about whole-node maintenance windows from the node list in
each check-in. Each maintenance window counts for at most 7 days from when
the check-in arrived, like planned downtime by hand. A window that ends
later is stored as ending 7 days after the check-in. While check-ins keep
listing the window, MMHotline updates the stored end time once a day.

A check-in older than the newest check-in received (by its `sent_at`) never
replaces the list. Maintenance windows covering only some check groups on
a server do not count. The page shows
the end time in the team’s time zone and names server web-1 as in
maintenance. The page says to end the maintenance window in the sending tool. If the sender stops
checking in during a window, MMHotline keeps honoring the last maintenance
windows received until the windows' end times.

During planned downtime, MMHotline still records check-ins but makes no
alerts about missing check-ins. When planned downtime ends, missing
check-ins send the SOS at the later of the configured SOS deadline and
1 minute after downtime ended, without a Warning first.

## When the problem clears

When check-ins resume:

- A Warning is followed by one resolved-warn alert with the resolution title.
- An unclaimed SOS incident is resolved automatically and sends one
  resolution message.
- A claimed SOS incident stays open, because the person who claimed it may be
  in the middle of a fix. The claimed SOS incident's history gets
  "Check-ins from “{name}” resumed at {time}". The person who claimed the
  SOS incident gets one message that does not wake the person. The person
  who claimed the SOS incident resolves the incident.

If the problem returns while the incident is still open, the new alert joins
that incident, and no Warning goes out
([identity](ingestion.md#identity-replay-and-resolution)). After check-ins
resumed, only a claimed incident is still open, so the new SOS goes to the
person who claimed it, through their SOS contact methods, waking ones
included.

Each time a person resolves an SOS incident while check-ins are still
missing, MMHotline waits for the SOS time to pass since the resolution.
MMHotline then sends a new SOS so a problem nobody is watching never goes
silent for good.

## When MMHotline can't hear

MMHotline cannot receive alerts or check-ins while its process is stopped
or its server is unreachable. MMHotline has no independent intake-outage
detector.
A sender retries alerts under the [intake contract](ingestion.md#alert-intake).

After a restart, check-in tracking is rebuilt from the database.
An alert source already marked as stopped keeps its stored times. For other
sources, the full Warning and SOS times start when MMHotline reads the database,
as described in [check-in tracking in memory](#check-in-tracking-in-memory).
A network failure while MMHotline remains running does not reset the
Warning and SOS timers. Alerts about missing check-ins follow the normal
rules.

## Scheduling and delivery ownership

The scheduler evaluates check-in timers explicitly and bounds its work.
A missed deadline is not a success. Keep only the check-in history needed
for diagnosis. Alerts about missing check-ins flow through the common alert and
incident operations; check-ins never send notifications themselves, keep
their own escalation loops, or add a second retry system.

## Acceptance evidence

Use an injectable clock for Warning and SOS times, planned downtime, and
resumption.

- **Check-ins:** acceptance and rejection (bad signature, or a signed alert
  body with fields invalid for check-in intake), waiting for the first
  check-in, an alert counting as
  hearing from the alert source, Warning then SOS, and resumption with an
  unclaimed and a claimed incident.
- **Check-in tracking in memory:**
  - No database write for a check-in that changes nothing, before and after
    a restart.
  - A database write for each change: the first check-in, changed sending
    node or maintenance list, check-ins stopping, and check-ins resuming.
  - A restart preserves the exact stored times of an alert source marked
    as stopped.
  - After a restart, an unmarked alert source's silence counts from when
    MMHotline reads the database.
  - A check-in signed with an added signing secret is accepted.
  - A check-in signed with a revoked signing secret is refused at once.
  - A check-in from a disabled alert source is refused at once.
  - A check-in that arrives while the alert source is being marked as
    stopped.
- **Planned downtime:** by hand, from `nodes_in_whole_node_maintenance` for
  the sending node, a window that outlives the check-ins, windows for only
  some check groups ignored, and the SOS when
  downtime ends with the problem still there.

Verify the actual alert path, including incident keys and joining an open
incident, not only the timer calculation. Verify restart timing for stopped and recently heard alert sources.
