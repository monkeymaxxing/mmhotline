# SOS incidents and escalation

An SOS is the urgent alert that wakes the person on call, even at night.
Claiming an incident tells the team you are handling it, and MMHotline stops
contacting the next person.

Incident actions and escalation deadlines are built. [The incident
page](#the-incident-page) lists the incident's notifications. SOS phone calls
and Pushover emergency messages are built. Claiming from a
notification is built and used by Pushover acknowledgements, Telegram and Slack
buttons. Calls tell responders to claim in MMHotline, or to acknowledge the
alert in Pushover, which also claims the incident.

## Separate incident state from delivery

An SOS alert starts an incident requiring human action. Claiming an incident
means someone owns it; answering a call or a sending service accepting a request is
not automatically a claim. A responder claims in the dashboard or from the
notification itself: with the “Claim this incident (assign it to me)” button on
a Telegram or Slack message, or by acknowledging a Pushover emergency, which
also claims the incident
([claiming from a notification](#claiming-from-a-notification)).

| Current state | Action | Result |
| --- | --- | --- |
| open | Claim this incident | claimed; record actor, time, and the optional [note](#a-note-with-a-claim-or-a-resolve); stop SOS repeats |
| claimed by someone else | Take over this incident | claimed; transfer ownership and record the previous owner, the new owner, and the optional note. The current owner is not offered the takeover action. Claiming your own claim changes nothing |
| open or claimed | Resolve | resolved; record actor/source, time, and the optional note; cancel pending SOS work |
| open or claimed | Escalate to another person | open, targeted at the person chosen in the escalation action. A fresh time to claim starts. Any previous owner loses ownership |
| resolved | A stale scheduled attempt completes | Retain delivery evidence; do not change incident state or restart escalation |

Automatic external resolution follows [ingestion correlation](ingestion.md).
When check-ins resume, MMHotline handles the incident by the rules in
[monitoring](monitoring.md): an unclaimed incident resolves automatically; a
claimed incident stays claimed for the person who claimed the incident.

A new SOS with the `sender_incident_key` of a claimed incident joins the
incident ([ingestion correlation](ingestion.md#identity-replay-and-resolution)).
The incident stays claimed, and MMHotline sends the new SOS to the person who
claimed the incident. MMHotline sends it once for each joining SOS, through
that person's contact methods subscribed to SOS, waking ones such as a
Pushover emergency or a phone call included. Availability and team mute apply
as for any SOS message. The incident history records "Another SOS about the
incident arrived. MMHotline sent it to the person who claimed the incident."
The reason: nobody else is being woken for a claimed incident, so a problem
that comes back before anyone resolves the incident would otherwise wake
nobody. The person claimed the incident, so the repeat is for them. A new SOS
that joins an open incident wakes nobody new, because the incident's
escalation is already waking people.

When an [approval request ends](triage-agent.md#when-the-request-ends),
MMHotline also resolves an open or claimed incident under the following
conditions:

- Every alert in the incident is MMHotline's own approval or warning alert.
- No other request on the incident waits, is approved but not
  yet run, or is running.

An incident containing a sender's alert or a triage agent report stays for
a person to resolve.

The triage agent cannot resolve incidents.
Any signed-in team member may claim, resolve, or manually escalate the team's
incidents, subject to the current revision.

The UI shows open as “Not claimed yet”, claimed as “Claimed”, and resolved as
“Resolved”.

## Incident information

Keep the following information for each SOS incident:

- Stable incident ID, opening alert ID, team/source identity, current status.
- Claimed by, Being contacted now, and escalation step. Human ownership
  and the most recently contacted person are different facts.
- Creation, claim, and resolution times; the actor/source of each transition.
- Primary, backup, and team notification timestamps, plus the current deadline.
- A chronological audit containing:
  - claims, takeovers from a previous owner (called reclaims), and
    resolutions, with the acting person's note when supplied;
  - escalations, notifications, suppression, and relevant failure results;
  - waits for the triage agent, reports, and agent runs that could not finish;
  - approvals of the agent's playbook runs, warnings before playbook runs,
    and Stops of warned playbook runs;
  - a person's Stop of the triage agent.

Structured records make actor, time, operation, and outcome inspectable.
Do not use mutable display names as historical identities. The current build has no
automatic history-retention or member-deletion workflow.

History uses names and titles, with stable IDs stored alongside: “Contacting
Alice (on call)”, “Contacting Bob (backup)”, “Contacting everyone available”,
“Contacting Alice (only team member)”, “Contacting Carol (chosen by Dave)”,
“Claimed by Alice”, “Taken over by Bob from Alice”, and “Escalated to Carol by
Dave”. A repeat says “Contacting Alice again”. A person MMHotline cannot
wake gets “Could not wake Alice: Alice has no contact method that wakes
people for SOS alerts”, once per escalation step, and the incident page says
“MMHotline could not wake Alice: …”. A claim or resolve with a note
shows the note after the history entry's own text, on the incident page and in
`mmhotline incidents show`: “Resolved by Alice”, then
`Note from Alice: “This was a test.”`. Messages and emails never print
member or incident IDs.

## Initial notification and repeats

When MMHotline accepts and saves an SOS alert, identify eligible responders
using [scheduling](scheduling.md) and [availability](availability.md).
Queue the SOS notification and ordinary subscribed contact methods.
When the SOS joins a triage session, the first waking step waits for the
triage agent ([triage sessions](#triage-sessions)).

Each escalation step gets a full time to claim from when the step starts.
The default time to claim is five minutes. A configured time to claim must be
from one to 60 minutes.
The settings file's `on_call_schedule.escalate_if_unclaimed_after_minutes` sets the
time to claim
([time to claim](../../reference/mmhotline-command.md#mmhotline-schedules-show)); the pages show
the time to claim read-only. The application operation validates configured
windows: whole minutes, because repeats come every minute; at least one, so a
woken person has time to see the call and claim; at most 60, because a longer
wait leaves an unanswered SOS with one person for so long that escalation
no longer protects the incident.

A step stores its end time when it starts, so a changed time to claim reaches only
the steps that start after the change, in open incidents too. A step already
under way keeps its end time. The incident page's countdown keeps counting
to the current step's stored end time, as in “Claim the incident by 09:05,
or MMHotline also wakes Bob.” For example, an SOS
opened at 09:00 under the default five minutes reaches the backup at 09:05
even if an administrator sets two minutes at 09:01. The backup then has
until 09:07.

During a step, SOS notifications to its target repeat every minute until the
incident is claimed, resolved, or moves to the next step. Phone calls repeat.
Pushover repeats an emergency itself. While the emergency's receipt stays
open, MMHotline sends no new message for the counted incident and contact
method. After the emergency receipt expires,
the next eligible minute repeat sends a new emergency. Email and chat get
the SOS once as an ordinary subscribed alert. Ordinary email/chat delivery
retries and SOS repeat notifications are different jobs. A transport retry must
not accidentally create another escalation step or another incident.

A claim or resolution cancels queued SOS notifications for the incident.
A request already sent to a sending service has its result recorded. Pushover
emergencies are cancelled when none of their counted incidents still wait
for the contact method owner; a phone call already placed cannot be recalled.

### Pushover emergency messages

**Built.** [Notifications](notifications.md#pushover) owns priorities, receipt
polling, and cancellation. Pushover follows the same active SOS targets and
team-mute gate as phone calls. SOS messages due together combine.
A Pushover acknowledgement claims exactly the incidents counted by the
emergency message, as the contact method owner.
Incidents opened after the send are not counted.

**One open emergency per counted incident and contact method.** Minute
repeats send nothing to a contact method while an emergency for that incident
is open. An expired, unacknowledged receipt permits a fresh message on the
next minute repeat. Allowing only one open Pushover emergency per incident
and contact method complements the phone call's one-call rule below.

### SOS phone calls

**Built.** [Notifications](notifications.md#sos-phone-calls) owns what a call
says and how it sounds.

- **One call about every waiting incident.** A call to a person is about every
  SOS incident waiting for the person, not one call per incident.
  The person opens Pushover or MMHotline to claim. Each minute's repeat for each
  incident still makes a delivery record: the notification due for that
  incident, even when several deliveries share a call. A repeat due while a
  call to the person waits joins the queued call instead of creating another.
  When MMHotline submits the call request to the sending service, the call
  counts the incidents joined to the queued call. An incident claimed or
  resolved before submission leaves the queued call. The delivery used to
  send the combined call belongs to one incident. If the sending delivery's
  incident is claimed, the oldest joined incident supplies the sending
  delivery.
- **One call to a number at a time.** A call is active from its creation until
  the sending service reports its final status, or for 3 minutes if no report
  arrives. A repeat that comes due while a call to that number is active waits
  (“Waiting: another call to this number is still going”) and becomes due when
  the call ends. A call that got no call ID from the sending service does not
  hold up the next repeat. Calls to the same number from two members' contact
  methods count as calls to one number. After restart, MMHotline resumes
  polling known active calls instead of assuming those calls ended.
- **Voicemail pacing.** During the first 30 minutes of calling a number about
  an SOS, the number is called every minute no matter what.
  After the first 30 minutes, calls answered by a person or by voicemail
  slow to one every 5 minutes for the number.
  Calls that ring unanswered cost nothing and stay every
  minute. A call with no final status counts as answered for the
  voicemail pacing rule. The calling period ends after 10 minutes without
  an SOS call to the number. The next SOS starts a new 30 minutes.

## SOS is never refused or delayed for rate

**Built.** SOS and resolved-SOS alerts are exempt from the intake limits on new alerts
([intake](ingestion.md#alert-intake)); they are bounded by the 1,000
unresolved incidents a team may have. Incidents opened under MMHotline's
internal `system` source are exempt from the unresolved-incident limit.
The background visit and the dashboard read every open incident, with no
limit, so an incident past the 1,000th still repeats and escalates. The
limit already bounds how many incidents intake opens; a second limit when
reading would only leave out the newest incident, which may be MMHotline's
own SOS about refused alerts.
The delivery throttle never delays an
SOS
([notifications](notifications.md#combining-and-the-delivery-throttle)). If a
correctly signed SOS alert is refused, MMHotline opens one SOS of its own for
the team: “MMHotline is refusing SOS alerts from “{source}”: {reason}”
([intake limits](ingestion.md#intake-limits-and-rejected-requests)).

## Escalation path

An unclaimed SOS keeps contacting responders until claimed or resolved, subject
to an administrator's explicit [team mute](availability.md#team-mute).

1. Contact the primary responder unless unavailable.
2. When the primary's time to claim ends, contact the eligible backup.
3. When the backup's time to claim ends, contact everyone available.

Everyone available means every eligible individual responder at once, not a
phone number on the special shared contact-method account.
With no members at all, the everyone-available step has nobody to contact.
The incident stays open, and the history records the lack of members once.
The everyone-available step follows the team, so the first added member
is contacted at the next visit.
During the everyone-available step, notifications repeat every minute until
someone claims or resolves the incident.
The escalation step values are `primary`, `backup`, `everyone_available`,
`only_member`, and `chosen_person`. Before the first waking step, the
escalation step is `triage_agent` while an SOS waits for the triage agent.
The database refuses any other step value when it is saved. A step that the
code does not know would otherwise fail every background visit for the
incident.

A person can be woken only by a contact method that wakes people, an
automated voice call or a Pushover notification, that receives SOS alerts.
When a round of waking reaches a person without one, MMHotline records
“Could not wake {name}: {name} has no contact method that wakes people for
SOS alerts” and leaves that person out of the round. When nobody in the
step can be woken, the next step starts at once, without waiting out the
time to claim, because waiting would wake nobody. The everyone-available
step and a one-person team's step have no next step. Their rounds keep
repeating every minute and wake anyone who adds a waking contact method.
The history line is recorded once per step. The first time an incident
cannot wake someone, MMHotline sends the team's administrators one warning
notice, “MMHotline could not wake {names} for an SOS incident: {title}”, so
the missing contact method is found and added.

Eligibility is confirmed again before every attempt. If an open incident's
target becomes unavailable, the incident moves to the next step at once.
Select a step's target from who is currently on call when the step starts. A later
change in who is on call does not replace a still-eligible target during its time to claim, and it
never transfers a claimed incident. Transferring a claim requires an
explicit incident action.

For a one-person team, keep retrying the single eligible responder rather than
pretending to escalate to a different person. A one-person team gets no "Nobody claimed an
SOS incident in time" alert: the step has no deadline, because there is
nobody else to contact, and the alert would only reach the single eligible
responder, who is already being contacted again and again.

For any team, if nobody is eligible, contact every member of the team,
ignoring unavailability. Record the reason: an SOS with no available
responder justifies waking someone. The dashboard
shows the lack of available responders. Contacting all members despite
unavailability never bypasses team mute.

On escalation, generate a warn alert and email titled
“Nobody claimed an SOS
incident in time, so MMHotline contacts {name}: {title}”, or, when a member
escalates by hand, “{member} asked MMHotline to contact {name} about an SOS
incident: {title}”.
Other team members can see that someone has not responded
and volunteer.

## Manual escalation

The incident page offers **Escalate to someone else**, under **More**, with a
list of the team's members and **Wake this person now**. An unavailable member
is disabled, with the time the member is available again; a former member is not
listed, because there is nobody to wake. Record who escalated, to whom, and when. Re-escalation remains
possible after a previous escalation. The operation must identify the current
incident revision so concurrent actions receive a definite outcome rather than
silently overwrite one another. A stale action says: “This incident changed
before your action was saved. Reload the page and try again.”

## The incident page

A person woken at night opens `/incidents/{id}` from a notification, often on
a phone, and must see within seconds what is wrong, who is woken, what
MMHotline does next by itself, and how to claim the incident.
The decide box first explains the problem, whom MMHotline woke, the next
automatic action, and how to claim the incident. The remaining sections
follow in the order below on phone and desktop layouts:

- **The heading**: Dashboard / Alerts / SOS incident; the tags SOS and the
  state, Not claimed, Claimed, or Resolved; the title; and a faint line with
  when it opened, in the team's time and as "8 minutes ago", and who sent it:
  the alert source's name, the person who raised it by hand, the triage agent,
  or MMHotline. When MMHotline raised the SOS to ask for an approval, a line
  below the line giving the sender and opening time explains the approval
  and links the request.
- **The decide box** is the page's main point of attention. While the
  SOS waits for a claim, the box is red. The decide box contains, in order:
  - whom MMHotline woke and when ("You are on call. MMHotline woke you at
    03:17.", "MMHotline is waking Priya Natarajan."), or that the triage
    agent looks first and nobody is woken yet;
  - the countdown sentence and bar
    ([the countdown](interface.md#the-page-frame));
  - one note field;
  - **Claim: I'm handling the incident**;
  - **Resolve**;
  - **More**, containing **Escalate to someone else** and a link to the
    Status page panel.

  One line says "Claiming tells
  the team you are on it, and stops MMHotline waking anyone else."
  Once claimed, the decide box is blue, because the work continues but
  nobody needs waking. The box names who claimed the incident and when.
  The owner gets **Resolve**. Anyone else gets **Take over: I'm handling the incident**
  and **Resolve**.

  Once resolved, the decide box is quiet gray, never red, and offers no
  action. The box names who resolved the incident, when, and how: on the
  page, in a notification, or through the source's resolve alert.
  The box also shows the note supplied with the resolution.
- **Where it is**: SOS received; woken, with each person and the time
  MMHotline first woke them, or held while the triage agent looks first;
  waiting for a claim, with whom the next step wakes and when, or claimed;
  and resolved.
- The triage agent's panel ([below](#the-triage-agent-on-the-incident-page))
  and the approvals ([below](#approval-requests)).
- **What happened**, one list, oldest first, of the incident's history, the
  alerts about it, and its rounds of waking. Each line says what happened in
  plain words, to the reader. A round of waking says whom MMHotline woke,
  in which role, and how each notification went. When every notification
  of the round went unsent, as when someone claimed the incident first, the
  round says whom MMHotline was about to wake, because nobody was woken.

  Minute repeats waking people already woken fold into the first round,
  with a repeat count and the last repeat's time. Grouping repeats prevents
  minute entries from burying the remaining history.
  MMHotline's own escalation and its first round are one line ("Nobody had
  claimed it, so MMHotline woke Priya Natarajan (backup)."), referring to
  the incident displayed on the page. The escalation line links MMHotline's
  alert to the team about the incident. A note shows with the writer's name.

  The list ends, in italics, with the countdown sentence at the countdown's
  time.
- **Folded**, closed: the opening alert's full message, the triage agent's
  steps, the notifications sent, and the identifiers (incident ID, alert
  source ID, sender incident key, and the opening alert's ID). Each
  notification gives its time, its contact method type and person, what it carried, and its
  outcome, which links the delivery page. A notification not sent says "Not sent" and
  why, in plain words, such as "someone claimed the incident".
- **People**: who is on call and the backup, whom the incident woke and when,
  whom the next step wakes and when, and who can't be woken now and until
  when. **Where it came from** describes the incident's origin: who sent
  the opening alert, the incident's sender incident key, the opening alert,
  and how many alerts followed. **Status page** appears when the team's
  public status page is set up, with **Post to the status page**.

History and notification reasons keep MMHotline's stored short values,
such as a step's name or "incident claimed". The incidents command prints
the stored values. The page translates the stored values into plain words
without changing the records. The heading's tags, the decide box, the
incident's progress under **Where it is**, the panels,
What happened, and the folded steps and notifications refresh by themselves.

Reasons for the choices, from the redesign of 2026-10-30:

- **One note field**, which Claim and Resolve both send, without JavaScript.
  The Claim and Resolve buttons belong to one form through the `form` attribute, and Resolve
  sets `formaction`. Two identical fields side by side left people unsure
  which note went with which action.
- **The approval in short.** A person woken at night decides from the
  playbook, where the playbook runs, what the playbook changes, and why the
  approval request waits for a person. **Review first** opens the request's
  own page, with the whole request, each task, the agent's diagnosis, and
  the playbook's text.
- **The status page panel does not read the published page.** What customers
  see now lives only in the team's R2 bucket. Reading the published
  content at every 10-second refresh would make the most urgent page wait
  on Cloudflare. The panel links
  the Status page, which reads the published content from the R2 bucket.
- **More opens below Resolve**, over the following content. The page's refresh leaves a live section
  alone while a folded part of the section is open, so choosing a responder
  is not interrupted.

## Claim and resolve experience

SOS emails link to the incident page at the settings file's `web_url`, where Cloudflare
Access signs the person in, or a sign-in cookie from a
[sign-in link](people.md#sign-in-links) does. Without Cloudflare Access
authentication or a valid sign-in cookie, the page returns
401 with the ways to sign in.

The incident page lists the incident's notifications, folded under
"Notifications sent"; What happened shows the rounds of waking among the
incident's notifications
([the incident page](#the-incident-page)). The dashboard keeps claimed
incidents visible until resolved. Resolving an incident sends one
resolved-sos notification to the relevant subscriptions,
never by phone call. A resolving alert from the source sends no separate
subscription notification
([resolution](ingestion.md#identity-replay-and-resolution)).

### A note with a claim or a resolve

On the incident page, the decide box has one optional field, **Note for the
team (optional)**, which Claim (or Take over) and Resolve both send, with the
hint "Claim and Resolve both keep it in the history. The triage agent reads
it the next time it looks into this." The optional note
lets the person explain the claim, takeover, or resolution to the team and
the [triage agent](triage-agent.md#notes-from-the-team). For example,
the note can say "This was a test." or "The backup job does this every night."

- A note is one line of plain text. MMHotline trims surrounding spaces.
  The limit is 500 characters, matching a refusal's reason, because a note
  gives a short reason rather than a report. Validation rejects notes
  under the following conditions:
  - More than 500 characters: "an incident note must be at most 500 characters; the submitted note has 612".
  - A line break, tab, or another control character: "a note is one line
    of plain text, without a line break, a tab, or another control character".

  An overlong note or a note containing a control character leaves the
  incident unchanged. One line reads consistently in the history, in a
  terminal, and in the agent's prompt.
- A blank note field adds no note to the action's history entry.
  The form's field is `note`, so a script that
  posts the page's forms, such as a
  [stand-in person's](../testing.md#a-stand-in-person) `curl`, sends
  `note=` too.
- The note belongs to the history entry created by the person's claim,
  takeover, or resolution. The history entry keeps the only copy of the
  note. What happened shows the note with the action and writer's name.

A note needs the incident page. Pushover claims and Slack and Telegram
claims and resolves take no note. No `mmhotline` command claims or resolves,
because a command run as root acting as a person would be a new question of
trust.

The page refreshes its live sections every 10 seconds; a section with a
note being typed in the section is not replaced, so the note stays until
the person sends or clears the note.

### Claiming from a notification

**Built** and used by Pushover acknowledgements and Telegram and Slack buttons.
Pushover acknowledgements and Telegram and Slack claim buttons use one incident
operation, `ClaimIncidentsFromNotification`, given the team, the
delivery (the notification), the member, and how the claim arrived. The
incidents a notification was about are the delivery's own incident and, for a
combined message, those of the deliveries sent together with it. Pushover
freezes the notification's incident list before sending and retains the
incident list with the receipt. For each incident counted by the notification:

- **Open:** the incident is claimed for the member acting on the notification.
- **Already claimed by the member acting on the notification:** the claim
  succeeds without change.
- **Claimed by someone else:** the incident is never taken over.
  The answer is “Already claimed by {name}”.
- **Resolved:** the answer says the incident is resolved.

The member is the owner of the Pushover notification contact method or the Slack or
Telegram user linked to a member
([notifications](notifications.md#chat-outgoing-webhooks-and-email)). The operation
reads the incident's current revision because an acknowledgement or button
cannot know the revision. The state-specific claim rules above make reading
the current revision safe.
`ClaimIncidentsFromNotification` reuses the dashboard's
claim and resolve steps inside one team transaction, so a button press and a
browser claim racing for the same incident give one claim and never a
takeover. The acting member must still belong to the team. A delivery
supports claims only when the contact method type supports a claim action.
History says how the claim
arrived: “Claimed by Alice in Slack”, “Claimed by Alice in Telegram”, or “Claimed by Alice, who
acknowledged the alert in Pushover”.

Resolving from a notification (`ResolveIncidentsFromNotification`) is possible
only with the Resolve button in Slack or Telegram, that is, a contact method type whose
claim method is a button, under the ordinary resolve rules (“Resolved by Alice
in Telegram”). A Pushover acknowledgement never resolves.

## Triage sessions

When the triage agent is set up, an SOS alert also joins the open
[triage session](triage-agent.md#triage-sessions), and so its incident
belongs to the open session. Each incident's alerts identify the incident, and
each session records the alerts that joined the session. Sessions hold
alerts rather than incidents because warn alerts have no incident.

Such an SOS gives the agent the settings file's
`triage_agent.sos_waits_for_the_agent_minutes`, such as 3 minutes, before
anyone is woken. With 0, the SOS waits not at all and escalates at once
([an SOS waits for the agent](triage-agent.md#an-sos-waits-for-the-agent)).
The SOS incident opens with the escalation step `triage_agent`: nobody is
contacted, and the incident's deadline, the wait after the SOS arrived, is
when MMHotline starts the first waking step. Email and chat get the SOS at
once as an ordinary subscribed alert. The first step starts sooner when the
agent sends a report with SOS severity or cannot help.
While the agent replaces a server, the deadline moves to the
replacement's start plus `triage_agent.sos_waits_for_a_server_replacement_minutes`,
and a failed run of the replacement starts the first step at once
([an SOS waits for a server replacement](triage-agent.md#an-sos-waits-for-a-server-replacement)).
A claim or resolution before the first waking step prevents waking.

From the first waking step, escalation follows the ordinary path.
The first time to claim and repeats start when the first waking step
starts. Later steps, including backup and everyone available, follow the
ordinary escalation path. Each later step starts a full time to claim.
An SOS that a person creates by hand never waits.
An SOS that joins a session which already woke the person on call
neither waits nor wakes them again; its first waking step starts at once,
silently, and its time to claim runs as usual
([an SOS waits for the agent](triage-agent.md#an-sos-waits-for-the-agent)).

The incident's history records the wait, as "Held for the triage agent until
03:15 UTC", and how it ended, as "The triage agent's time ran out; waking the
person on call" or the reason it ended sooner. The incident's history also
records each report the agent sends about the incident's alerts, and the
session's hand-over report
([reports to people](triage-agent.md#reports-to-people)), as "Triage agent
report", and each of the session's runs that failed, as "Triage agent could
not run", with the reason.

A report at sos wakes nobody a second time while the incident's own
escalation contacts the person on call. A report at sos never reopens or
escalates a claimed incident ([reports to people](triage-agent.md#reports-to-people)). A
session normally waits for every incident in the session to resolve before
ending. Taking over the session ends the session after the hand-over.
The session also ends if the triage agent can no longer run.

The agent's [software review](triage-agent.md#the-software-review) has no
session. A software review's report at sos opens an SOS incident of the
report's own, which records the report and starts escalation at once.

### The triage agent on the incident page

The page of each incident of a session has a panel for the triage agent,
which refreshes with the page's other live sections. The triage agent panel
shows the session the incident's newest alert joined:

- what the agent found: the first paragraph of its latest report about the
  incident, under "What the triage agent found", with a line that these are
  the agent's findings, which MMHotline has not checked, and the whole report
  folded;
- the session's state as a tag, with the corresponding explanation:
  - Working since when: "The triage agent is looking into the incident
    now";
  - Waiting: "Waiting: the triage agent is not running";
  - Stopping: "Stopping: the triage agent is finishing the current step";
  - Handing over: "Handing over: the triage agent writes a report and a
    note about what the triage agent found, did, and was about to do";
  - Ended: when the session ended and why;
- **Watch the session**, a link to
  [the session's own page](triage-agent.md#the-triage-sessions-page), which
  lists all its alerts, runs, and reports.

The run that is running, or else the latest run, has its latest 50 steps,
oldest first, folded under "The triage agent's steps": each with its time,
kind (message, command, or tool), and text of at most 2,000 characters, and
a line that says how many earlier steps are not shown. The page refreshes
every 10 seconds, and 50 steps keep each refresh near 100 KB; the coding
agent's session files keep the whole transcript.

While the session is open and nobody pressed Stop, the panel has **Take over
from the agent**. Any signed-in member may press **Take over from the agent**.
Pressing **Take over from the agent** takes the session over from the agent
([a person takes over](triage-agent.md#a-person-takes-over)):
the agent finishes its current step, hands over in one more run, and its
session ends. A short line beside **Take over from the agent** explains the
stop and hand-over. While the SOS waits for the agent, the line also says
that pressing **Take over from the agent** wakes the person on call at once.
Taking over changes no incident's state; claiming and resolving stay a person's own
actions.

The session's own page shows the session with **Stop after the current
step**, or **Stop the triage agent** when no run is going. Pressing the
session page's Stop returns the person to the session page.
Each incident of the session records the take-over as "Triage agent stopped",
with the member. What happened displays "Barbara took over from the triage
agent." An SOS released from the triage wait by the take-over records
"Barbara stopped the triage agent; waking the person on call".
What happened displays "The hold for the triage agent ended.
Barbara stopped the triage agent." The agent's hand-over report is a "Triage
agent report" like any other. An incident none of whose alerts joined a
session has no such panel.

### Approval requests

When the agent asks for a playbook whose rule is "Get permission,
then act", MMHotline asks for [approval](triage-agent.md#approvals) at sos.
The "Approval needed" alert joins an open incident from the asking run's
work, or a claimed incident if no incident is open. A run's work includes
the run's alerts and the incidents inherited through playbook results.
A run carrying neither alerts nor playbook results uses the latest session
run that carried alerts. With no unresolved
incident, the alert opens an SOS incident, exactly as a report at sos does.
Every incident of the session that waits for the agent stops waiting and
wakes the person on call. The approval alert never waits for the agent.

Each incident page of the session shows each of the session's approval
requests waiting for a person in short, in an amber box. Each approval box contains:

- The request's question and what the playbook can change, in which
  inventory, as every page names a request: “Run “Reboot app002” on
  app002?” and “It can change app002, in the inventory “production”.”
- The kinds of action.
- Why approval is required: the rule, tasks affecting services
  whose changes a person must approve, tasks MMHotline cannot check, and the review
  agent's answer.
- **Approve and run**, or **Approve** when another approval is still needed.
  Approving returns the person to the incident page.
- **Review first**, which opens the request's own page.

A decided request appears as one line containing the question linked to
the request page, the state, and the time.
The approvals section refreshes with the page's other live sections.

The request's own page
([approving or refusing](triage-agent.md#approving-or-refusing)) shows:

- The playbook's path and whole text at the commit MMHotline evaluated
  when judging the run request.
- The playbook's node tasks, with each task's node, module, and arguments.
- The kinds of action, inventory, and evaluated commit.
- The review agent's answer, when a review ran, and the triage agent's
  diagnosis.
- Earlier playbook versions the review agent refused consecutively before
  the version in the displayed request.
- The rule, required approvals, and each decision.
- After a decision, the outcome.

While the request waits, the request page offers **Approve and run** and
**Refuse**, which asks for a short reason. A software review's request
appears on the incident opened by the request.

Before a request waits for a person, the review agent may have refused
earlier versions of the proposed playbook, which went back to the triage
agent
([refusal rounds](triage-agent.md#refusal-rounds)). The history of each
incident of the run's work that is not resolved records each such refusal as
"Review agent refused a playbook run", with the playbook, the commit, the
round, and the reason. A review agent refusal wakes nobody.

The history of the incident the alert went with records the request as
"Approval needed", each decision as "Playbook run approved" or "Playbook run
refused" with the member and any reason, the run's result as "Approved
playbook run", and "Result not sent to the triage agent" when the agent's
session had ended. A decision or a result never changes the incident's state:
resolving the incident stays a person's action, and an open request stays
decidable after the incident is resolved.

### Warned runs

When the agent asks for a playbook whose rule is "Warn, wait, then
act", MMHotline [warns the team](triage-agent.md#warn-wait-then-act) at warn:
"MMHotline will run PLAYBOOK in the inventory “INVENTORY” for the triage agent at 03:25 UTC
unless someone presses Stop: it can change SERVERS". The warning goes with an
open incident of the asking run's work, else a claimed incident, else a
resolved incident, and links that incident's page. The playbook-run warning wakes nobody: an
incident that waits for the agent keeps waiting, because nobody needs to act
for the proposed playbook to run. The warning's message links the run's own
page. Warn alerts have no incident. When the run's work has no incident,
only the link to the run's page remains.

Each incident page of the session shows a warned run that can still be
stopped in short, in an amber box: when the warned playbook runs and the time
left, such as "Runs in 7 minutes", what the playbook runs and changes, as
for an approval, **Stop**
with an optional **Reason for stopping (optional)**, and **Review first**,
which opens the warned run's own page ([Stop](triage-agent.md#stop)).
The warned run's page shows the playbook, node tasks, kinds, inventory,
commit, and review evaluated for the run, as the approval request page does.
The warned run's page also shows the rule and warning wait, then:

- when MMHotline runs the proposed playbook automatically, in the team's
  time and as a countdown,
  such as "MMHotline runs it at 03:25, in 7 minutes, unless someone presses
  Stop." The page's refresh keeps
  the countdown current;
- **Stop**, with an optional reason, until the run time;
- after a Stop, who stopped the warned playbook run, when, and why;
- from the run time onward, whether the warned playbook run runs now,
  is running, or ran. The page offers no Stop after the run time.

The history records the warning as "Playbook run warning", a Stop as
"Playbook run stopped" with the member and any reason, and the run's result as
"Warned playbook run". As for an approval, the warning, Stop, and playbook
run result do not change the incident's state.

## Concurrency and restart

Save each incident state transition and pending notification in one database
transaction. The background visit handles each open incident in its own
savepoint. An incident whose handling fails is undone and reported in the
server log, and the other incidents still escalate in the same visit.
One stored incident that cannot be handled must not stop escalation for
every incident. A worker receives the incident ID, revision, target, and purpose
of the notification attempt. Immediately before starting a notification
side effect, confirm the incident state and availability decision again.

After completion, record the result against the notification attempt the
worker started. Do not apply an obsolete escalation decision to a newer
incident state.

Restore deadlines and unfinished work after restart, then apply
[current-work recovery](operation.md#recover-current-work). An overdue step
advances to the next eligible step with a fresh time to claim.
Downtime does not manufacture completed primary or backup windows or a
backlog of calls. Record uncertain sending service outcomes honestly.
[Notifications](notifications.md#sending-service-results) owns sending service reports and
retries, separately from incident deadlines.

## Acceptance evidence

Use a controllable clock and notification boundary.
Cover primary success, backup, everyone available, and repeated
notifications to the team. Cover a target becoming ineligible mid-step,
one-person teams, and no available responders. Cover a person with no
waking contact method, an incident past the 1,000th, and one incident whose
handling fails while the others escalate. Cover escalating a claimed
incident, claim/resolve racing a deadline, and manual handoff.
Cover two incidents targeting one phone, sending service timeout, and
restart. For the phone and chat contact method types, also cover:

- One call about several incidents and directing people to claim in
  MMHotline or acknowledge in Pushover.
- An incident opened during a call and voicemail pacing.
- Claiming from each contact method type against open, claimed, and
  resolved incidents.
- An SOS arriving while intake limits refuse other alerts.

Add a real sending service workflow. Fake sending services do not establish
actual delivery or acknowledgement.
