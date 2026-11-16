# Severities and incident states

This page lists the alert severities and what each one does, the states of
an SOS **incident** and how it moves between them, the **escalation steps**
that decide whom MMHotline wakes, and the states of a **triage session**.
An incident is MMHotline's record of the problem an SOS reports; to
**claim** it is to say "I'm on it", which stops MMHotline waking more
people. A triage session is the triage agent's work on a stretch of
trouble. To handle an incident step by step, follow
[Handle an incident](../how-to/handle-an-incident.md). How a tool sends a
severity is in [Alert requests](alert-requests.md#severity).

## Severities

A tool sets the severity in each alert. Pages and notifications show the
severities as **Info**, **Warning**, and **SOS**.

| Severity | Shown as | Opens an incident | Wakes people | Reaches |
| --- | --- | --- | --- | --- |
| `info` | Info | No | No | Contact methods subscribed to Info |
| `warn` | Warning | No | No | Contact methods subscribed to Warning |
| `sos` | SOS | Yes | Yes, the person on call, then further steps | Waking contact methods of the person being woken; other contact methods subscribed to SOS at once |
| `resolved-info` | Resolved info | No | No | Contact methods subscribed to Info |
| `resolved-warn` | Resolved warning | No | No | Contact methods subscribed to Warning |
| `resolved-sos` | Resolved SOS | Resolves one | No | Contact methods that were sent something about the incident |

Each person chooses, on each contact method, which of SOS, Warning, and
Info it gets ([Add contact methods](../how-to/add-contact-methods.md)).
A subscription to a severity also brings its `resolved-` form.
Unavailability and team mute hold back notifications as
[Be away or take over on call](../how-to/be-away-or-take-over-on-call.md)
and [Mute the team](../how-to/mute-the-team.md) say.

### info

Useful information that needs nobody now, such as "Monitoring connected".

- Goes to every contact method on the team that is subscribed to Info.
- Never starts or joins a triage session.

### warn

Something needs attention, but not by waking someone, such as a disk at
91%.

- Goes to every contact method on the team that is subscribed to Warning.
- An alert sent without a severity is saved as `warn`, with the note
  `Received without severity, defaulting to 'warn'`.
- When the triage agent is set up and the settings file's
  `triage_agent.warn_waits_for_the_agent_minutes` is above 0, a Warning
  that starts a triage session waits that long before it is sent, so the
  agent can deal with it first
  ([a warn waits for the agent](../changing-mmhotline/design/triage-agent.md#a-warn-waits-for-the-agent)).
- Forty Warnings stay forty alerts; MMHotline combines their messages
  rather than opening anything.

### sos

A problem that needs a person as soon as possible, even at night.

- Opens an incident, or joins the open or claimed incident with the same
  `sender_incident_key` from the same alert source.
- Wakes people only through a contact method that wakes: a Pushover
  notification or an automated voice call that gets SOS alerts, and only
  while an [escalation step](#escalation-steps) is waking that person.
  Email, Slack, Telegram, and outgoing webhooks get the SOS once, at once,
  as an ordinary alert.
- Repeats every minute to the person being woken, until someone claims or
  resolves the incident, or the next step starts.
- Is never refused or delayed for rate. If MMHotline must refuse a
  correctly signed SOS, it opens an SOS of its own about the refusal
  ([alert requests](alert-requests.md#a-refused-sos-wakes-the-team)).
- An SOS that joins an **open** incident wakes nobody new, because the
  incident is already waking people. An SOS that joins a **claimed**
  incident goes to the person who claimed it, once for each joining SOS,
  through that person's contact methods subscribed to SOS, waking ones
  included. Reason: nobody else is being woken for a claimed incident, and
  the problem came back.

### resolved-info and resolved-warn

Say that something an `info` or `warn` reported is over.

- Go to the contact methods subscribed to Info or Warning.
- Match nothing: MMHotline keeps no open Warning to close.
- A `resolved-warn` whose every earlier Warning with the same key was held
  for the triage agent and never sent goes to nobody, and its alert page
  says so.

### resolved-sos

Says that the problem an SOS reported is over.

- Resolves the open or claimed incident whose SOS came from the same
  alert source with the same `sender_incident_key`. The incident's history
  says the alert source resolved it.
- Goes only to contact methods that MMHotline sent something about the
  incident, so nobody gets an "is over" for a beginning they never saw.
  Never makes a phone call.
- With no matching incident, the alert is saved with the note
  `No active incident matched resolution key "never-opened".` and sends
  nothing.

### Alerts that MMHotline sends itself

MMHotline also sends alerts of its own, from the alert source ID `system`:

- When an alert source stops checking in: a Warning after
  `warn_after_no_check_in_for_minutes`, then an SOS after
  `sos_after_no_check_in_for_minutes`, such as
  `“Monitoring” hasn't checked in with MMHotline for 5 minutes: it may have stopped, or its server may be down`.
  When check-ins return, an unclaimed SOS about the silence resolves by
  itself; a claimed one stays with the person who claimed it.
- Notices about its own work, such as
  `Nobody claimed an SOS incident in time, so MMHotline contacts Barbara Example: ...`,
  as Warnings or Info. Notices about MMHotline's own limits go only to
  administrators.

An SOS that a person raises on the dashboard has the alert source ID
`manual`. The triage agent's reports arrive as alerts at the severity the
agent chose.

## Incident states

Only an SOS has an incident. The `status` field of `mmhotline incidents
list --json` uses the first column; pages use the second.

| `status` | On pages | Meaning |
| --- | --- | --- |
| `open` | Not claimed yet | Nobody has said they are handling it; MMHotline is waking people, or waiting for the triage agent |
| `claimed` | Claimed | A person handles it; MMHotline wakes nobody else |
| `resolved` | Resolved | The problem is over; all waking stopped |

```sh
sudo mmhotline incidents list
```

```text
SOS incident 52f9135fc0ee491e0e6c49fc40bedc25: Backup failed: test SOS
Status: Not claimed yet. Opened at 2026-11-15T03:56:59Z.
```

On the dashboard, an open incident is under **Needs a person**, a claimed
one under **Being handled**.

### open

- Starts when an SOS opens the incident.
- MMHotline works through the [escalation steps](#escalation-steps).
- Any person on the team may claim, resolve, or escalate it.

### claimed

- **Claim this incident** (on the page, **Claim: I'm handling the
  incident**) moves `open` to `claimed`. MMHotline records who and when,
  and stops the SOS's repeats.
- A person can also claim from a notification: by acknowledging a
  Pushover emergency, or with **Claim this incident (assign it to me)** in
  Slack or Telegram. Answering an automated voice call does not claim.
- **Take over: I'm handling the incident** moves the claim from another
  person to you. The person who holds it does not see the button.
- A schedule handoff never moves a claim.

### resolved

- **Resolve**, by any person, moves `open` or `claimed` to `resolved`.
- A tool's `resolved-sos` with the incident's key also resolves it.
- An SOS about a tool's silence resolves itself when check-ins return, if
  nobody claimed it.
- Nothing reopens a resolved incident. A new SOS with the same key opens a
  new incident.

### Changes between states

| From | Action | To |
| --- | --- | --- |
| `open` | Claim | `claimed` |
| `claimed` | Take over, by another person | `claimed`, by that person |
| `open` or `claimed` | Resolve, or a matching `resolved-sos` | `resolved` |
| `open` or `claimed` | **Escalate to someone else**, then **Wake this person now** | `open`, waking the chosen person with a fresh time to claim; any claim ends |
| `claimed` | A new SOS with the same key | `claimed`, and the SOS goes to the person who claimed it |

A claim or resolve on the page can carry a one-line note of at most 500
characters, which the history keeps. No command claims or resolves an
incident.

An action made on an older state of the page is refused with HTTP status
409 and `The incident changed before your action was saved. Reload the page and try again.`
Reason: an action never overwrites another person's change it did not see.

The triage agent cannot claim or resolve an incident.

## Escalation steps

While an incident is `open`, its escalation step says whom MMHotline is
waking. `mmhotline incidents list --json` gives it as `escalation_step`.

| `escalation_step` | Whom MMHotline wakes |
| --- | --- |
| `triage_agent` | Nobody yet: the SOS waits for the triage agent |
| `primary` | The person on call |
| `backup` | The schedule's backup |
| `everyone_available` | Every available person on the team at once |
| `only_member` | The only person on the team |
| `chosen_person` | The person someone chose with **Wake this person now** |

### triage_agent

When the triage agent is set up, an SOS from a tool waits first, for the
settings file's `triage_agent.sos_waits_for_the_agent_minutes`, 0 to 15,
or while the agent replaces a server, for
`triage_agent.sos_waits_for_a_server_replacement_minutes`, 0 to 60.

- The page says `The triage agent looks first, so MMHotline has woken nobody yet.`
- Email and chat get the SOS at once.
- The wait ends early when the agent sends a report at SOS, cannot help,
  or a person presses **Take over from the agent**. Then `primary` starts.
- An SOS that a person raises by hand never waits.
- An SOS that joins a triage session that already woke the person on call
  neither waits nor wakes them again: its first step starts at once,
  silently, and escalates as usual if nobody claims it.
- A Pushover notification never gets an SOS that the agent fixed without a
  server replacement, so Pushover gets no resolved message for it. Email, which
  gets the SOS at once, still does.

### primary, backup, and everyone_available

The normal order. Each step gets the full time to claim, the settings
file's `on_call_schedule.escalate_if_unclaimed_after_minutes`, 1 to 60
minutes. When it runs out, the next step starts.

1. `primary`: the person on call, unless unavailable.
2. `backup`: the backup the schedule names.
3. `everyone_available`: everyone available, at once. This step repeats
   every minute until someone claims or resolves the incident.

- A step's end time is fixed when the step starts. A changed time to
  claim reaches only steps that start later.
- A person with no waking contact method for SOS is skipped, with
  `Could not wake Carlos Example: Carlos Example has no contact method that wakes people for SOS alerts`
  in the history. When nobody in a step can be woken, the next step starts
  at once. Administrators get one warning the first time.
- If the person being woken becomes unavailable, the next step starts at
  once.
- If nobody at all is available, MMHotline wakes every person on the team,
  ignoring unavailability. Team mute still holds.
- Each move to a new step sends the team a Warning, such as
  `Nobody claimed an SOS incident in time, so MMHotline contacts Barbara Example: Backup failed: test SOS`.

### only_member

A team of one person: MMHotline keeps waking that person every minute.
There is no next step and no "Nobody claimed" warning, because there is
nobody else.

### chosen_person

Someone chose a person under **Escalate to someone else** and pressed
**Wake this person now**. The incident becomes unclaimed, and the chosen
person gets a fresh time to claim. The team gets a Warning such as
`Carlos Example asked MMHotline to contact Barbara Example about an SOS incident: Backup failed: test SOS`.

## Triage session states

A triage session holds the Warning and SOS alerts that arrive while the
triage agent works, and the agent's runs on them. One session is open at a
time. `mmhotline triage-sessions show --json` gives the state as `phase`;
the session's page and the incident page show the text.

| `phase` | Tag | What the page says |
| --- | --- | --- |
| `working` | Working | `Working: the triage agent is running. The run steps appear below as the steps happen.` |
| `waiting` | Waiting | `Waiting: the triage agent is not running. MMHotline resumes the triage agent when another alert or the result of a playbook run arrives.` |
| `stopping` | Stopping | `Stopping: the triage agent is finishing the current step.` |
| `handing_over` | Handing over | `Handing over: the triage agent writes a report and a note about what the triage agent found, did, and was about to do. MMHotline refuses the triage agent's new playbook-run requests during the hand-over.` |
| `ended` | Ended | `The triage session ended at TIME: REASON` |

```sh
sudo mmhotline triage-sessions show --triage-session-id d0c87b6008262060142800e45f592aab
```

```text
Triage session d0c87b6008262060142800e45f592aab: Waiting: the triage agent is not running. MMHotline resumes the triage agent when another alert or the result of a playbook run arrives.
```

### working and waiting

- A Warning or SOS from a tool, or an SOS a person raised, starts a session
  when none is open, or joins the open one. Info alerts, resolved alerts,
  MMHotline's notices about its own work, and the agent's own reports never
  do.
- The session is `working` while a run of the agent is going, and
  `waiting` between runs. A new alert, or the result of a playbook run
  that waited for a person, starts the next run.

### Warnings held for the triage agent

With `triage_agent.warn_waits_for_the_agent_minutes` above 0, MMHotline
holds a Warning that starts a triage session, and a Warning that joins a
session while an SOS of it waits for the agent. The dashboard's panel
**Warnings held for the triage agent** and the warning's page say:

- `MMHotline holds this warning for up to 3 minutes while the triage agent looks into it; it reaches people then, unless the agent takes it over.`
- For a Warning that joined a session, `MMHotline holds this warning with the SOS the triage agent is handling (TITLE); it reaches people if the person on call is woken, or at TIME, unless the agent's report covers it or it resolves first.`
  MMHotline holds that Warning with the SOS.

After the hold, the page says one of these:

- `MMHotline delivered this warning at TIME.` with the reason.
- `Taken over by the triage agent's report at TIME. MMHotline sent nothing for this warning.`
  or `Taken over by the triage agent's playbook run request at TIME. ...`,
  with a link to the report or to the triage session. A taken-over Warning
  sends nothing: the agent's report, or the Warning about its playbook run,
  is the message people get.

[The settings file](settings-file.md#triage_agent) says how the hold ends.

### stopping and handing_over

- A person presses **Take over from the agent** on the incident page, or
  **Stop after the current step** (or **Stop the triage agent**) on the
  session's page. The session becomes `stopping` while the agent finishes
  its current step.
- Then `handing_over`: MMHotline runs the agent once more to write a
  report and a note for `triage_agent_notes/` in the systems repository.
- From the Stop on, MMHotline refuses the agent's new playbook-run
  requests. An SOS that still waited for the agent wakes the person on
  call at once.

### ended

A session ends for one of these reasons, which the page quotes:

- `Every incident of the session was resolved, and no alert joined it for 30 minutes.`
- `An alert about a new problem arrived after the session's work was done, and started a new session.`
  A problem is an alert source plus `sender_incident_key`.
- `A person took over: Barbara Example stopped the triage agent.`
  (or, when the agent had not begun, `The triage agent never began, so there was nothing to hand over.`)
  The page shows a separate line, `Barbara Example pressed Stop at 2026-11-03 03:10:00 UTC.`

The next Warning or SOS starts a new session, which starts from the
agent's notes. [The triage agent design](../changing-mmhotline/design/triage-agent.md#triage-sessions)
gives the full rules.
