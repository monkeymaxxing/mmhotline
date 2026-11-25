# Web interface and workflows

## Shared behavior

Keep the interface legible under incident pressure. Text labels accompany
severity colors/icons. The menu ([the page frame](#the-page-frame)) reaches
every page. Settings has no separate page. Names identify people. Forms have labels,
visible validation, keyboard access, and explicit save feedback. Confirmation/error
updates are exposed to
assistive technology.

The team's [settings file](settings-file.md) sets the people, the schedule
and time to claim, and the alert sources and check-in times. The settings
file also sets rules for running the agent's playbooks and the list of services whose changes a person
must approve. No form changes settings from the settings file. The settings file
also sets the triage agent's setup, internet access, listed hosts, and
software review interval. The review agent's setup,
the sending services, the MMHotline-down alarm, and the status page's setup also come
from the settings file.

Each page shows the settings relevant to the page read-only. Settings pages
keep one closed settings section per page, named for the page's subject.
Repeated paths crowd the setup status and next action.
The settings notice says
“Set in /etc/mmhotline/mmhotline_settings.yml, in {key}.”
If the settings file starts with a comment, the page shows the comment
as the file's first line. The comment can say how the team changes
the settings file. Without a first comment line, the page says an
administrator changes the settings file with sudoedit and runs
`sudo mmhotline settings apply`.

The dashboard tells administrators which parts of the settings file did
not apply and why. The warning matters because MMHotline keeps the earlier
setting, and nothing else would show the failure.

Write for a first-time reader. Each page explains the product's terms (Info,
Warning, SOS, claim, contact method, sending service, triage agent, rule, MMHotline-down alarm) in plain words at each term's first use: a label such as
“Claim: I'm handling the incident” with a line beside the label, or a
setting's help text, never at every later use.

Alert titles and push messages
explain events without requiring knowledge of Info, Warning, SOS, claim,
contact method, sending service, triage agent, rule, or MMHotline-down alarm.
An SOS is the urgent alert that wakes the person on call, even at night;
the pages say so in those
words. “Claim” means only owning an incident: claiming an incident tells the
team you are handling the incident, and MMHotline stops contacting the next person.

“Acknowledge” means only Pushover's
seen-it button. Where acknowledging also claims an incident, the text says
so as a second step.

Server-rendered HTML is the baseline; small JavaScript enhancements can update
who is on call and SOS incident state without full navigation. Authorization and state
transitions belong to the application operations. UI visibility is not an
authorization check. Preserve typed input on failed validation.

Browser error pages use the page frame, a plain heading and reason, and a
link back to the dashboard. An incident or triage-session mutation can link
back to the source page from the route. Unknown paths also have a framed
"Page not found" page. HTTP status codes remain unchanged; 429 keeps its
`Retry-After: 900` header and says to try again in 15 minutes.
The headings are "Check the request" (400), "You cannot make this change"
(403 for a refused action), "Page not found" (404), "The page changed" (409),
"Too many requests" (429), and "Cannot complete the request" (500).
If page rendering itself fails, "Cannot display the page" still offers a
way back, using a frame that needs no database read.

Only browser navigation gets the new error presentation: a navigation
request, or an HTML Accept header when the browser supplies no fetch mode.
Background refreshes, JSON requests, downloads, assets, the intake listener,
and fake services keep their existing error responses. Presenting errors
at the page frame keeps the rule in one place and avoids changing the
sender protocol when a page changes.

## The page frame

Every page has the same frame, so a person woken at night finds the work
needing attention and the way around in the same place on every page.
Development mode has a quiet amber notice because fake sending services
are a warning about the environment, not work in progress.

- **The menu.** A side menu on a wide window; on a phone, a top bar with
  **Menu**. The Menu button opens the links listed below for the side menu.
  The side menu and the phone's top bar name MMHotline, with the team name
  below MMHotline. The menu is organized into three visually separated zones
  so day-to-day actions stand out while rarely-used setup pages stay out of
  the way:

  1. **Day-to-day pages** at the top: Dashboard, Alerts, Approvals, On call,
     and Status page. Approvals shows the number of approval requests and
     unreviewed refused commits waiting for a person. A warned run is not
     counted because the warned run goes ahead unless someone stops the run.
  2. **Settings** below a hairline divider, folded in a collapsible disclosure
     (`Settings`) that stays closed by default on day-to-day pages and opens
     automatically whenever the current page (or one of its subpages) is inside
     Settings. Its quieter sub-links are Alert sources, People, Sending
     services and Triage agent for administrators, and **Playbook rules**
     (linking to `/agent-playbook-rules`, shortened from the page title so
     every menu link stays on one line).
  3. **Personal account** (**Signed in as** the person's name) below a
     hairline divider and anchored at the bottom of the desktop sidebar (and
     kept compact in the phone menu), holding Contact methods and
     Availability.

  The link to the page shown, or to the page's part of MMHotline, such as
  Alerts on one alert's page, is marked with `aria-current` and a left accent
  bar. A person who is not signed in as a member sees no menu.
- **The red bar.** While an SOS incident waits for a claim, including while
  it is held for the triage agent, a red bar at the very top of every page
  names the SOS incident, says the incident is not claimed, gives the
  countdown below, and links to the incident. With several unclaimed SOS
  incidents, the red bar names the oldest unclaimed SOS incident and says
  how many more wait. The
  dashboard lists every SOS that waits, and an incident's own page shows its
  countdown, so the dashboard and incident page do not repeat the countdown
  in the bar. The bar appears and leaves by
  itself, without a reload.
- **The countdown** says what MMHotline does next by itself unless someone
  claims the incident, to the person reading, whom it calls "you": "Claim it
  by 17:42, or MMHotline also wakes Priya Natarajan." The countdown counts
  to the incident's deadline: the end of the hold for the triage agent,
  then each step's time to claim.

  The countdown names only people MMHotline has not
  woken for the incident yet. Once the person on call is woken, the countdown
  names the next person, never the person on call again. When the next step
  wakes nobody new, the countdown says whom MMHotline wakes again. On the
  last step, the countdown says MMHotline keeps waking everyone available
  (or the team's only member) until someone claims the incident.
  The last step has no deadline.

  MMHotline calculates the countdown's next action by running the escalation
  rules on a copy of the incident at the deadline, with coverage at the
  deadline.
  A person's unavailability that ends before the deadline counts. A change
  before the deadline, such as someone marking themselves unavailable,
  shows at the page's next refresh.
- **Times** on the pages are in the team's time zone, the schedule's
  `time_zone`, or UTC without a schedule: to the minute, with the day only
  when the day is not today in the team's time zone. Relative times such as
  "8 minutes ago" or "in 2 minutes" appear where relative times help. A faint line at the foot of every page names the
  zone: "Times are New York time (America/New_York), the team's time zone."
  Notifications keep their own format, and the public status page stays in UTC.
- **One stylesheet**, `/assets/mmhotline.css`, MMHotline's own, with a light
  and a dark look that follow the device's setting. The Content Security
  Policy allows no other style (`style-src 'self'`), so no page has a style
  attribute or a style element.
- **Live sections.** Every page a member sees fetches itself every 10
  seconds and replaces each live section whose content changed, such as the
  red bar, unless the section holds the focus or text the person typed.

## Settings pages

Sending services use separate panels that keep each panel's own height.
A setup page opens with the saved setup's status and the next action.
**Set up · last check passed** describes the saved setup check, not live health.
A later failed settings check appears separately, because a refused replacement
keeps the earlier setup. Failure details stay folded. Settings examples keep
whole lines and scroll inside the example, so a phone never splits a key.
Inline keys can wrap after an underscore.

The Triage agent page leads with setup and sign-in status. Setup test output,
software review details, and network details stay folded to keep actions visible.
The page of rules for running the agent's playbooks lists each action's rule and change details together.
The list stacks on phones, so the change details never disappear past the edge.
The Status page settings show the public link, each open notice with its
impact, components, planned window, and updates, a form to add an update, and
a folded list of recently resolved notices. A form posts a new notice with a
title, an impact, components, a planned window for maintenance, a stage, and a
message. Resolving a notice opens a closing form, so posting updates remains
the main action. Each notice, open or resolved, has three folded forms:
**Edit notice details**, **Edit or delete updates**, and **Delete this
notice**, which needs a ticked confirmation. An edit or delete from a page
that is out of date is refused with a message to reload, and the form keeps
what the person typed.
The MMHotline-down alarm test result lists each recipient and the sending result; an
accepted send does not prove the person heard the alert.

Settings and notice timestamps use the frame's team time zone. The public
status page stays in UTC. Each page keeps all settings paths in the single
closed settings section, including paths recorded as who changed a setting.

## Dashboard

The dashboard answers first what needs a person now, because a person often
opens it on a phone, woken at night. Next the dashboard says who is on call,
what the triage agent does, and whether the MMHotline-down alarm watches MMHotline.
Last the dashboard shows the state of the system. The dashboard follows the
frame's three levels: one loud part, normal panels, and quiet background facts.

- The heading says "Dashboard" with a faint line, "Updated 17:42. This page
  updates by itself." The line is a live section. A dashboard that stopped
  updating, such as on a phone that lost its connection, then shows an old
  time instead of looking current.
- **Raise an SOS by hand**, beside the heading, opens a folded form with
  Title and Message, posted to `/incidents/new`. One line explains raising
  an SOS by hand: "For a problem no tool sent an alert about. It wakes the
  person on call like any other SOS." Creation opens the incident page; a
  refused form comes back with its input. The form is folded because a person
  rarely needs the form, and a panel on every visit would crowd what needs
  a person.
- **Parts of the settings file did not apply**, in amber at the top, for
  administrators only: each part that failed, with its reason, and that
  MMHotline keeps the earlier setting until the file is fixed. The file's
  path, its first line or how an administrator changes it, and
  `sudo mmhotline settings show` are folded under "Where to change the settings". The
  earlier setting then stays, and nothing else would show the settings-apply
  failure; members cannot change the file, so members do not see the
  settings-apply warning.
- **Needs a person**, the page's one loud part, while anything waits:
  - each SOS incident nobody claimed, oldest first, with no limit on how
    many, so the newest is never left out: its title, when it
    opened, and whom MMHotline wakes now, such as "MMHotline is waking
    you", or, while it is held for the triage agent, "The triage agent looks
    into it first, so nobody is woken yet."; then the
    [countdown](#the-page-frame) with its bar and the time left, **Claim
    it**, and **Open**. Claim posts the incident page's claim form, with
    the incident's revision number, and opens the incident. A line at the
    foot explains incident
    responsibility: "Claiming tells the team you are on it, and stops
    MMHotline waking anyone else."
  - each approval request that waits for a person, oldest first, named as
    every page names it: its question, such as “Run “Reboot app002” on
    app002?”, and the playbook's possible targets,
    “It can change app002, in the inventory “production”.”;
    when the triage agent asked, whose approval the request still needs, and
    **Review**, which opens `/approvals/{approval_request_id}`.
  - each commit waiting for review, using the approval row and
    **Review** link to `/approvals/commits/{commit_id}`. The row names the
    commit, says why it waits, that no key the team trusts signed it or
    that the triage agent's key signed it, and gives the discovery time.
    Any team member can review the commit.
  - each warned run that runs by itself at a set time, named with the
    playbook's question, possible target servers, and inventory, as for an
    approval request:
    when it runs, such as "Runs at 12:10, in 7 minutes, unless someone presses Stop.",
    **Stop**, which stops the warned run from the dashboard, and **Open**.

  The box is red while an SOS waits, and amber while only approval requests,
  refused commits, and warned runs wait. Red means a person is being woken. Amber means a
  decision waits.
- **Being handled**, quieter, in blue: each claimed SOS incident, with who
  claimed it, "You" for the viewer, when, and when it opened, linked to its
  page, with no Claim. The person sees at once which SOS still needs
  someone and which has an owner.
- **Warnings held for the triage agent**, quieter, while MMHotline holds a
  warn ([a warn waits for the agent](triage-agent.md#a-warn-waits-for-the-agent)):
  each held warning's title linking its alert page, when it arrived, when the
  hold ends, and "MMHotline holds this warning for up to N minutes while the
  triage agent looks into it; it reaches people then, unless the agent takes
  it over." (for a warn held with an SOS, "MMHotline holds this warning with
  the SOS the triage agent is handling (TITLE); it reaches people if the
  person on call is woken, or at TIME, unless the agent's report covers it or
  it resolves first."). The panel is absent when no warning is held; a delivered or taken
  over warning is on its own alert page.
- **The triage agent reviews**, quieter, in blue: each waiting commit the
  triage agent is reviewing, before any alert about the commit went out,
  linked to its page. A commit under review is not in **Needs a person**:
  a person who saw it there would approve a commit the agent is about to
  judge, or think the agent had given up. The commit moves to **Needs a
  person** when the agent hands it to a person or the review cannot happen.
- **All clear** when nothing needs a person and nothing is being handled:
  "Nothing needs a person.", with one faint line about the SOS resolved
  last: when, by whom ("you" for the viewer, a person, an alert source, the
  triage agent, or MMHotline), and its title, linked to its page.
- Three panels in a row:
  - **On call**: whom an SOS wakes first, "You" for the viewer, or
    "Everyone who is available" with their names while nobody is
    scheduled; until when, and who then; "If nobody claims an SOS within 5
    minutes, MMHotline also wakes Priya Natarajan.", with the backup, or
    "everyone else who is available"; the coverage's own line for an
    exception, such as "The person who took on-call is contacted first.";
    and, while the team is muted, an amber line: "The whole team is muted
    until 18:42: no Pushover notification or automated voice call goes
    out, even for an SOS." The panel links to the on-call page. "Until" and "then" come from the function used by the
    on-call page, so the dashboard and on-call page never disagree.
    MMHotline collects candidate times when coverage can change.
    The schedule's handovers are candidates within the on-call page's
    preview: 24 hours by hour, eight days by day, eight weeks for a weekly
    rotation, and custom schedule code's worked-out shifts.
    Candidate times also include the end of a temporary on-call takeover
    and the ends of people's unavailability. MMHotline evaluates coverage at
    each candidate time.
    The first time that names a different initial SOS responder supplies "until"
    and "then". If no candidate time changes the initial responder, the
    panel names no end.
  - **Triage agent**, when the triage agent is set up or a
    [triage session](triage-agent.md#triage-sessions) is open.
    While a session is open: "Working on" the session's first alert, with
    how many more joined the session, linked to
    [the session's page](triage-agent.md#the-triage-sessions-page);
    since when; the session's phase when no run is going, such as
    "Waiting: the triage agent is not running."; how many steps the session's running or
    latest run took, and the latest step, cut to 300 characters and shown
    in at most four lines.

    With no session open: "Not working on any alert now.", with a link to
    the last session and when the session ended. A team without the triage agent
    sees no such panel.
  - **MMHotline-down alarm**, when the team set up the MMHotline-down alarm
    ([what the team sees](mmhotline-down-alarm-and-status-page.md#what-the-team-sees)).
- **Servers**, then **Recent changes to the systems repository** beside the
  **Checklist**, described below, from data that already exists on the
  server.
  Servers, Recent changes to the systems repository, and Checklist have no
  settings page. When a source is missing or unusable, its part says so in
  one line, such as "The settings file has no
  fleet_view_command, so MMHotline shows no servers." A directory or the
  technical cause, such as "MMHotline looks for its copy at
  /var/lib/mmhotline/systems_repository.", is folded under "Details"; the
  page never becomes an error page.
- At a phone's width every part is a list that wraps; nothing scrolls
  sideways.

The triage agent's panel answers at a glance whether the agent works, on
what, since when, and what the agent does now. The triage agent's panel shows
the latest step only, cut short, and no Stop. Every step and Stop are one
click away on the session's page, where a person sees what led to the step
before stopping the agent.

The triage agent's panel names the session by the session's first alert,
as does the quiet start message to the person on call, when the team turned
it on ([the start message](triage-agent.md#the-start-message)).

The definitions that used to open the dashboard's parts are gone. The
dashboard explains an action in one line where the person acts, such as
beside Claim.
Times follow the frame: the team's time zone, to the minute, with "8
minutes ago" where relative times help.

Every part of the dashboard except the folded form refreshes every 10
seconds. A shared read supplies the monitoring tool's fleet view, recent
systems repository commits, and checklist for every viewer for 10 seconds,
matching the refresh interval.
Any number of open dashboards start the fleet view command, as root, and
Git at most once in 10 seconds. Nothing runs while nobody looks.
Concurrent viewers wait for the shared read of the fleet view, commits,
and checklist instead of starting a read per viewer.

### Servers

Show each server of the [fleet view](#the-monitoring-tools-fleet-view) once,
regardless of group membership, so no failing server is left out.
A server needs attention when the monitoring tool reports a per-server
exit code other than 0 in the fleet view's `nodes[].exit_code`.
Servers needing attention come first, sorted by worst alert severity (`sos`
urgent first, then `warn` warning, then `info` notice) and then alphabetically
by server name.

Each server needing attention gets a card styled for its worst severity: red
for urgent (`sos`), amber for warning (`warn`), and neutral for info (`info`),
with an explicit severity tag ("Urgent", "Warning", or "Info") beside the
server name and group so color is never the sole indicator, and normal readable
body text inside the card. A server's severity is the worst among its silence
severity (when `state` is `silent`, from `silence_alert_severity` or
`report.alert_severity_when_this_node_goes_silent`, defaulting safely to `sos`
when missing) and each attention check's `alert_severity` (defaulting safely to
`sos` when missing or unrecognized so older monitoring tools never downgrade
urgent issues). When a server needing attention is neither silent nor has
checks needing attention, show
"Needs attention (exit code 3) without a failing check; the fleet view on
this server shows why.", treated as `warn` for exit code 3 (unknown monitoring
health) and `sos` otherwise.

For a silent server, first show
"Silent: the server's last report arrived at 09:50" (or "Silent: no report
since the server joined" when no report has arrived), and prefix each
last-known check heading with "Last known: ". Group checks that share a check
key under one plain heading (for example, `disk/root_disk_usage` reads "Root
disk usage" and `systemd_units/no_systemd_unit_has_failed` reads "No systemd
unit has failed"), trim a repeated finding prefix from the message, and insert
wrap opportunities after underscores and slashes so long identifiers do not
break mid-word. Each card footer links to that server's alert history
(`/alerts?q=<server>`) and, when raw check keys or trailing parenthetical URL
references exist, folds them under "Technical details".

Healthy servers follow as small chips with names and groups, sorted by server
name. The heading badge uses the matching severity color and plain wording:
"Healthy" or "All 5 healthy" in green when all servers are healthy; single-
severity counts such as "1 of 4 urgent", "2 of 4 have warnings", or "1 of 4
has a notice"; or mixed counts such as "2 urgent · 1 warning" (including
notices only when present).

When the fleet hub sets findings aside after an investigation, one faint
line follows the chips: "Set aside after an investigation: 1 finding on 2
servers. sudo mmwatchdog fleet set-aside-findings on the fleet hub lists
them." Such a finding no longer makes its server need attention, so the
line keeps it in sight, as MMWatchdog's own fleet view does.

Groups are written in plain words: `app_servers` reads "App servers". The
fleet view's JSON calls servers nodes and groups node groups; the page says
server and group, the words the team's systems repository uses. MMHotline
reads no MMX inventory and knows no group names; in the Monkeymaxxing Stack
each server names its inventory groups.

### Recent changes to the systems repository

The 10 newest commits of the branch that
[MMHotline's copy](triage-agent.md#the-systems-repository-copies) has checked
out, newest first, each with the first line of its message, its commit time,
and the first 12 characters of its ID. Ten fit one screen; the Git host keeps
the rest. MMHotline reads the checked-out branch, not origin's: the commits
that MMHotline's copy has, which MMHotline fast-forwards only to the
[trusted commit](triage-agent.md#mmhotlines-checkout-stays-at-the-trusted-commit),
so the list shows only commits that passed the commit check or that a person
reviewed. Reading refs and objects
takes no lock, so the page never waits for an MMX run. Nothing is fetched.

### Checklist

Read `monkeymaxxing_stack_checklist.yml` at the root of the newest commit
shown in Recent changes to the systems repository. The checklist has two
parts: setup, which the team finishes before the system is handed over, and
later, when it happens, such as a drill, a key rotation, or a new team
member. Each item's `part` is `setup` or `later_when_it_happens`. An item
without `part`, and every app's item, is a setup item.

For the setup items, show "Setup: 3 of 8 done" with a progress bar, and
counts of open items and items done another way (deviated). Below the
counts, show the next setup item needing a person: the first non-deviated
one, in checklist order. Fold all remaining setup items needing a person
under "Show 4 more".

Below them, fold every item for later under "Later, when it happens: 5
items", adding "· 1 due to be verified again" when some are. Each item shows
its status: "When it happens" for an open item, with "Waits for: …"; "Done",
with "Verified 1 Oct 00:00." when `verified_at` reads; "Deviated"; or "Open
again" when its verification is due. The items for later never count toward
setup, never give the next step, and never keep setup from being done.
Without this split, a team that finished setup would see a long open list
of things that wait for their event, as if setup were unfinished. The
Stack's checklist can move an item between the parts, such as
`staging_server` for a team without a staging server, so MMHotline reads
`part`, never the item's `id`.

The setup items needing a person are, in checklist order:

- an `open` item, with "Waits for: {waits_for}";
- a `deviated` item, with "Deviation: {deviation}";
- a `done` item when the current time reaches or passes `verified_at` plus
  `verify_again_every`,
  as "Open again", with "Last verified 19 Oct 04:12; due again since 26 Oct
  04:12 (every 7d).";
- an item MMHotline cannot read, as open, with the reason: a state other than
  `open`, `done`, or `deviated`, a `part` other than `setup` or
  `later_when_it_happens`, or a `verified_at` or `verify_again_every` of a
  done item that MMHotline cannot read. A typing mistake never hides an item:
  an unknown `part` counts as a setup item that needs a person, rather than
  going quietly into either part.

The checklist gives the order for building the system, so the first open
item is the next step. The remaining items needing a person stay one click
away instead of filling a quiet panel. Each item shows its title, its status,
"Protects against: …", and the button
“Copy a question for your coding agent”, which copies “Explain checklist item
'{title}' ({id} in monkeymaxxing_stack_checklist.yml) of my system and what it
protects against.” and says “Copied. Paste it into your coding agent.” When
the browser does not allow copying, the status line shows the question to
copy by hand. The items for later have the same button. With no setup item
needing a person, the part says “Every setup item is done, and none is due to
be verified again.”

The Monkeymaxxing Stack owns the checklist's format. MMHotline reads the
top-level `items` list and each app's list, `apps.{app name}.items`, wherever
each stands in the file; every other section, such as
`what_this_system_does_not_survive`, holds no items.

For each item, MMHotline reads
`id`, `title`, `part`, `state`, `protects_against`, `waits_for`, `deviation`,
`verified_at`, a UTC time such as `2026-11-03T04:12:00Z`, and
`verify_again_every`: a whole number and `h`, `d`, `w`, or the words hours,
days, weeks, months, or years, such as `7d` or `6 months`.

A bare `m` is refused, because `m` could mean minutes or months.
A file that is not YAML, or a checklist that is a symlink, gives one line
with the reason. MMHotline reads the file with `go.yaml.in/yaml/v3`, because
the checklist has folded text and comments.

### The monitoring tool's fleet view

An input contract: what MMHotline reads from the JSON that the settings
file's `fleet_view_command` prints
([operation](operation.md#the-fleet-view-socket)). MMHotline ignores every
unlisted JSON field.

| Field | How MMHotline uses the field |
| --- | --- |
| `nodes` | Required list of the fleet's servers; JSON without `nodes` is refused. An empty list shows “The fleet view lists no servers yet.” |
| `nodes[].name` | The server's name on its tile or chip |
| `nodes[].state` | `silent` adds the silence line |
| `nodes[].exit_code` | 0 is healthy, a chip; any other value makes the server a card that needs attention |
| `nodes[].this_node_is_in_node_groups` | The groups shown with the server, in plain words; empty or missing shows none |
| `nodes[].last_report_received_at` | The time in a silent server's line |
| `nodes[].silence_alert_severity`, `nodes[].report.alert_severity_when_this_node_goes_silent` | The severity (`sos`, `warn`, `info`) of a silent server when `state` is `silent`; missing defaults to `sos` |
| `nodes[].report.summary.attention[]` | Each check that needs attention: `check`, `finding` (a failed unit or CVE, when present), `state`, `alert_severity` (`sos`, `warn`, `info`; missing defaults to `sos`), `since`, and `message` |
| `set_aside_finding_count`, `nodes_with_set_aside_findings` | The set-aside line, when the count is above 0; missing (MMWatchdog before 0.2.0) shows none |

The command's exit status is ignored: a fleet view exits nonzero while a
server fails, and still prints the whole view. Output that is empty, not
JSON, larger than 64 MiB, or not finished within 10 seconds gives "MMHotline
cannot show the servers.", with the cause folded under "Details" and, where
the command's own errors explain the failure, the journal command
`sudo journalctl -u 'mmhotline-fleet-view@*'`. 10 seconds:
a fleet view answers from the hub's memory in about a second even for a
large fleet, and a hung command must not hold the page longer. 64 MiB: a fleet
view takes a few KiB per server, so the limit fits more than ten thousand
servers, and a wrong command that prints without end must not fill the
memory of the process that sends every alert.

## Alert log and details

“Alert history” at `/alerts` lists alerts newest first, with severity labels
Info, Warning, SOS, Resolved info, Resolved warning, and Resolved SOS. Each
entry is a wrapping list item: severity, plain-English display title, receipt
time using the frame's `clock` and relative time, and the named incident the
alert opened, joined, resolved, or relates to. A source notice linked to an
incident is “Related SOS incident”, because a notice does not open or join an
incident. Alerts without an incident say “No SOS incident”. Notices are
labelled “Notice from MMHotline”. Lists keep titles readable on a phone without
squeezing dates into narrow table columns.

Non-system alerts lead with plain English while keeping the exact raw title in
the database:

- When the triage agent has reported on the alert (directly via covered alerts,
  on a prior alert of the same `(source_id, sender_incident_key)` cycle for a
  resolved alert, or via a `triage_agent_report` incident action), `/alerts`
  and `/alerts/{alert_id}` lead with the report's plain-English headline
  (prefixed with “Resolved: ” for `resolved-*` alerts when not already
  prefixed). `/alerts` marks such rows with “Explained by triage agent”.
- When no triage agent summary exists yet, raw `check_group/check_name`
  monitoring slugs inside non-system alert titles are humanized into plain
  words (for example, `disks/root_disk_usage` reads “Root disk usage”), while
  ordinary human titles, URLs, and file paths stay unchanged.
- Whenever the displayed title differs from the stored raw alert title,
  “Original alert: …” appears quietly underneath so technical details remain
  scannable.

Search title/message, filter severity and receipt dates in the team's time
zone, initially show 30 alerts, and load more without losing filters.
A through date includes the whole local calendar day, including a day when
the clocks change, so filtering matches the times the person reads.

Alert details at `/alerts/{alert_id}` lead with the plain-English title (with
the original raw title quietly underneath when they differ). When a triage
agent summary exists, a “Triage agent summary” panel appears at the top with
the plain-English headline, the full report text, and links to “View triage
report” and “Triage session”. Below that come severity, receipt time, meaning,
and the alert's effect on a named incident. A late alert names the sender's
event time and receipt time in the team's time zone, and the delay in minutes.
“Full alert message” folds the raw message—closed by default when a triage
agent summary is shown, and open otherwise—and “Alert IDs and sender details”
folds “Alert ID”, “Alert source ID”, “Sender alert ID”, and “Sender incident
key”. These facts stay available without crowding the alert's outcome. An
unmatched resolution says no unresolved SOS incident matched the sender
incident key and nobody was notified. An SOS incident has its own page, which
[the incident page](incidents.md#the-incident-page) describes. Ordinary alert
details do not show fictitious open/claimed states.

## Schedule

“On call” at `/on-call` answers, for a person who may be woken tonight:
who is on call, whom an SOS wakes and in what order, who is scheduled on
call during each coming week, and what each person can change.
Under the heading, one faint line gives the schedule's rhythm and the
time to claim: “Handover every
Monday at 09:00 · 5 minutes to claim an SOS”. The four built schedule types
are By day of the week, By hour of the day, Weekly rotation, and Custom
schedule. Weekly rotation includes the `next_in_rotation` backup option,
and Custom schedule includes a sandbox. In order:

- **On call now**: the person ("You" for the reader), until when, and who
  follows: “Until 2 Nov 09:00, in 2 days. Then Priya Natarajan.” Until when
  is the first moment at which MMHotline's coverage names someone else: the
  end of an on-call duty someone took, the end of someone's
  unavailability, or a handover of the schedule; the dashboard's On call
  panel names the same moment and person. Beside On call now, **who an SOS
  wakes, in order**: the person on call, “no claim in 5 minutes →” the
  backup, then everyone else who is available, with each unavailable person
  shown as skipped until their end time, and a line on the last step:
  “MMHotline keeps waking everyone who is available until someone claims
  it.” A line says whether someone took on call (“Priya Natarajan took on
  call. The schedule names you.”) or why the schedule's person is not first.
  While the team is muted, an amber line says an SOS wakes nobody until the
  mute ends.
  With the triage agent set up, a faint line says the triage agent gets up
  to 5 minutes with an SOS from an alert source before anyone is woken.
- **Week by week** for a weekly rotation: this week and the coming weeks,
  until every person in the rotation has a coming week, at least four.
  A daily or hourly schedule lists coming handovers instead.
  Consecutive assignments to a person appear as a single interval, from the
  start until the next listed change of person.

  A custom schedule lists the coming shifts the custom code worked out,
  and says plainly when no coming shifts are worked out. Show the last
  failure.
- **Your availability** (`#your-availability`, the menu's Availability
  link): “You are available.” or “You are unavailable until {time}.”, one
  line on what personal unavailability changes (“While you are unavailable,
  MMHotline skips you and wakes Priya Natarajan first.”), “Set yourself
  unavailable”, and “I'm available again” only while unavailable.
- **Taking over on call**: a person on call now is not offered “Take on
  call”; the panel, “Hand over early”, says how someone else takes over.
  Anyone else who is available gets “Take on call”, or “Take on call from
  {name}” while someone took on call; whoever took on call, or an administrator, gets
  “Give on call back to the schedule”
  ([taking on call](availability.md#taking-on-call)).
- **Mute the team**, for administrators, with one line on what muting holds
  back ([team mute](availability.md#team-mute)). A member reads that only an
  administrator can mute the team.
- **The team**: each person, the person's current status (On call, Backup,
  Available, or Unavailable until {time}), what can wake the person at night (“Woken by
  Pushover notification and automated voice call”, or “Nothing set up can
  wake them at night”), and the person's role. The team section is a list, which
  stacks on a phone.
- Folded: **Recent changes** to on call, newest first; for a custom
  schedule, **The schedule code** with its test cases, stored shifts, and
  saved versions; and **Where the schedule is set**: the settings file and
  `on_call_schedule`, the file's first line, and the schedule's settings,
  with adjacent same-person hours merged. A built-in slot is a weekday
  assignment in By day of the week or an hour assignment in By hour of
  the day. When an assignment names no person, the slot reads “No one
  scheduled”.

Each duration input is labelled “For how long”, with examples such as 30m,
2h, or 3d. On call now, the coming weeks, and the team are live sections.

Why the on-call page works this way:

- Every displayed fact comes from the code that acts on the fact, never
  from a second copy of a rule. To display whom an SOS wakes, the page
  simulates escalation on a new incident, as the countdown does.
  The simulation calls `chooseInitialTarget`, then `chooseNextTarget` at
  each step's deadline, using coverage at each simulated time.
  The coming weeks use the schedule code's assignments at each handover.

  The explanation of personal unavailability repeats the new incident's
  wake-order simulation with the reader marked unavailable. A page that
  explained the rules in separate prose drifted from the rules. Before the
  redesign, the page said "No one scheduled: the backup is contacted first"
  under every filled rotation.
- What can wake a person and whether MMHotline wakes the person now are
  two facts, shown apart. Before the redesign, a column, “Can be woken for
  SOS”, said “Yes” for an unavailable person with a working phone, while
  the escalation code skipped the unavailable person.
- The terms are explained at the moment of action, in one line beside the
  action, not in a paragraph on every visit.

A preview runs on the server with `schedules preview --settings-file FILE`
and uses the same rules as live scheduling.
[Custom schedule code](scheduling.md#custom-schedule-code) owns its
messages, errors, and version history.

## People, preferences, and joining

Setup creates the one team and no person; the settings file's `people` adds
everyone, the first administrator included. People sign in through
Cloudflare Access at the settings file's `web_url`, or, during a Cloudflare outage or
without Cloudflare, with a one-time sign-in link created on the server.
With Cloudflare Access configured, missing or failed sign-in returns 401,
“Sign in through Cloudflare Access”, with a sign-in link recovery fold.
Without Cloudflare Access, the 401 page says “Sign in with a sign-in link”
and shows the server instructions unfolded. An Access sign-in for an address that is not a member
returns 403,
“Not a team member”, naming the address and team, and saying that an
administrator adds the address to `people` in the settings file. When MMHotline cannot check Access sign-ins, pages return 503
and point to the sign-in link. Access pages put the next action in a panel.
Access recovery commands stay in closed recovery folds; the 403 page keeps the
settings file path under "Where team members are set". A refused person
sees no menu.

People at `/people` lists each active person's name, sign-in email address,
and role, with “(you)” beside the viewer. Rows stack on phones, keeping full role names visible.
“Where people are set” folds the settings file path and instructions. The list is read-only because the
settings file adds, changes, and removes people
([people](people.md#roles-and-lifecycle)).

The on-call page lists each person's role, current status, and what can
wake the person at night. “Contact methods” at `/contact-methods` supports “Alerts to send through the contact method”,
“Send even when I'm unavailable or the team is muted” (shared: “Send even when
the team is muted”), last test result, test delivery, removal, and shift
notifications. Keep shared contact methods distinct from individual
preferences.

The “Email”, “Outgoing webhook”, “Telegram message”, “Slack message”,
“Pushover notification”, and “Automated voice call” contact method cards
are built.
Each card uses the contact method type's facts
([contact method types](notifications.md#contact-method-types)).
Each card has “Send test”. Saving a new contact method or changing the
contact method's address queues a test.
Each card shows the latest test outcome and failure reason, or “Not tested yet”.

Contact methods use panels that keep their own height. Personal and shared controls come first.
“Add a contact method” folds the add forms and opens after an error or when no personal or shared method exists.
Administrators can expand “Other people's contact methods”; each panel names its owner and offers no personal editing actions.
“Recent notifications” is a compact list, newest first, with five records visible and the remaining loaded records under “Older notifications”.
“Delivery outcome” shows the result and plain reason above “Message as sent”.
The message preserves the recorded text. “Delivery attempts” folds attempt outcomes, times, and diagnostics.
Page times use the team's clock; timestamps inside the recorded message stay unchanged.


- “Add a phone number” creates one automated voice call card for the member's number.
  The form shows the call consent text and queues a test call.
  The page guides the member to download the contact card, allow the
  contact card through Do Not Disturb, and make a test call with Do Not
  Disturb on.
  The automated voice call card on the page offers SOS only; its number
  cannot be edited. Changes to MMHotline's outgoing calling numbers show
  a notice until the member downloads the contact card again.
- Slack: “Connect Slack” (each member) and “Add a shared Slack channel”
  (administrators) show one-time codes. A member sends a code in the team's
  MMHotline Slack app's direct messages. An administrator invites the
  MMHotline Slack app into a channel and mentions the app.
  The Slack message card shows which channel or direct messages it
  sends to; its conversation cannot be edited, only its alerts and “Send
  even when…” setting.
- Telegram (built): “Connect Telegram” (each member) and “Add the MMHotline
  bot to a Telegram group” (administrators). Each shows a page with a
  one-time link to open in Telegram. Slack and Telegram connection pages put instructions and the external action in a panel.
  Codes wrap on phones, and each page says the code or link works once, for 15 minutes.
  The Telegram message card shows which chat it
  sends to; its chat cannot be edited, only its alerts and “Send even when…”
  setting.
- The outgoing webhook card has “Webhook signing secret (hex)”, shown once, “Add a
  second webhook signing secret (for rotation)”, and “Webhook bearer token
  (optional)”.

Sign-in and CSRF behavior follow
[people and access](people.md#authentication-and-recovery).

## Check-ins and planned downtime

**Built.**
[Check-ins and planned downtime](monitoring.md) owns their
behavior.

Every member sees Alert sources; each source's name and “Check-ins and
planned downtime” link open `/alert-sources/{alert_source_id}`.
The source page has an “Alert sources” breadcrumb above the source name.
The source page leads with “Check-ins arriving”, “Waiting for the first
check-in”, “In planned downtime”, “No recent check-in”, or “Disabled”, and
what MMHotline does about missing check-ins. A source with recent signed
alerts but no recent check-in says “Receiving alerts, but no recent
check-in”. Check-ins arriving describes the sending program, not the
health of services the program monitors.

Before the first check-in, the page explains that no check-ins are expected
until the first arrives. Sources that never check in are never watched for
silence. “Missing-check-in rules” shows “Warning” (or “Off”) and “Wake the
person on call”, read-only. The settings-file notice is folded under “Where the alert source is set” to keep file paths out of the operational answer. The source
list reuses each source page's check-in status and shows “Rejected requests in the last 24 hours: {n}” and the reasons.

The “Planned downtime” form keeps “Until” (date and time, required, labelled
with the team's time zone), “Reason (optional)”, and “Start planned
downtime”. The field's default and submitted value use the team's time
zone, so the form agrees with the page's times. A validation error preserves
the chosen time and reason. During page-entered downtime, members see “End
planned downtime now”; sender-listed downtime names the sending server and
says to end maintenance in the sending tool. Disabled sources have no start
form. All members may start and end page-entered downtime. Rules and downtime
sit beside each other on a wide window, each at its own height, and stack on
a phone. Server maintenance details are folded.

## Connecting an alert sender

The team lists the alert source in the settings file's `alert_sources`, with
an alert source ID and a signing secret from the team's vault.
The alert sources page at `/alert-sources` lists source names, current check-in or
disabled state, and rejected requests before folded “Connection details”.
An empty list tells an administrator to add a sending tool or script under
`alert_sources` and points to “Where alert sources are set”. Connection details shows “Alert intake address”
and “Alert source ID”
separately, followed by both full URLs:
`POST <address>/alert-intake/<alert_source_id>` and
`POST <address>/check-in/<alert_source_id>`. Copy the alert source ID, with
MMHotline's alert intake address, into the sender's config; the sender reads
the signing secret from the vault. Send signed alerts and check-ins under the
[intake contract](ingestion.md). To rotate a secret, list the second secret,
update the sender, then remove the old secret from the settings file.
Administrators see “Signing secret ID” and when the secret was added in the
team’s time zone, never the secret value. The ID label distinguishes an
identifier from a credential. Request format and signing instructions are
folded because setup facts are needed less often than checking a sender.

## Reviewing a commit that waits

The Approvals page has a **Commits waiting for review** section, separate
from playbook requests. Each unreviewed commit has a question and **Review**.
The review page at `/approvals/commits/{commit_id}` opens with **Allow MMX
runs past commit COMMIT, which no key the team trusts signed?** or **Allow
MMX runs past commit COMMIT, signed by the triage agent's key?** The page
uses the existing approval panels and amber decision box. The facts name the
full commit ID, author, commit date in the team's time zone, message, and
discovery time, and for the triage agent's commit every refused path and
reason. For the triage agent's commit, **Commit diff** is a closed fold. For
a commit no trusted key signed, the diff is all there is to review, so the
fold starts open, and the decision box says in bold that a commit nobody on
the team made is a possible break-in, to be handled with the team's
break-in procedure instead of a review. A note says when the diff was cut
after 128 KiB.

**Mark commit reviewed** is the only action. The explanation says that
MMHotline then permits MMX runs past the commit. A wrong change must first
be reverted with the person's own commit. Review starts no run; the triage
agent must request the playbook run again. After review, the page gives the
member's name and review time. Reviewed state is quiet, never red.
