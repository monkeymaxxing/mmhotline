# Availability, team mute, and taking on-call

## Three controls

| Control | Purpose | End condition |
| --- | --- | --- |
| Team mute | Stop notifications for the whole team: Pushover and phone calls always, other contact methods unless they are set to send while muted | The team mute end time |
| Set yourself unavailable | Exclude yourself from on-call duty and limit notifications | `unavailable_until` |
| Take on call | Take the team's on-call duties temporarily | The end time chosen when taking on call |

Team mute, personal unavailability, and taking on-call are separate controls.
Taking on-call changes who is on call. Claiming an incident accepts ownership
of one SOS incident. An SOS is the urgent alert that wakes the person on call,
even at night; claiming an incident tells the
team you are handling it, and MMHotline stops contacting the next person.
"Claim" always means owning an incident, so the interface never uses "Claim"
for on-call duty. The interface shows the duration and effective result of
each action.

## Personal unavailability

Unavailability starts immediately and expires at a visible instant. There is no future start date.
The duration controls accept one minute through 30 days,
including 15m, 2h, 1h30m, and whole days such as 3d. The range of one minute
through 30 days also applies to team mute and taking on-call. Thirty days
covers a long absence while keeping team mute, personal unavailability, and
taking on-call visibly distinct from a schedule change.
Reject invalid durations without changing state. Ending a control clears only
that control.

Unavailable people are skipped for on call. An unavailable person's contact
methods send only with “Send even when I'm unavailable or the team is muted”
enabled (`send_while_unavailable_or_muted`). Display the end time on the
unavailable person's profile and in the team list.

On the on-call page, the
“Your availability” section says “You are available.” or
“You are unavailable until {time}.”, and in one line what MMHotline does
while the person is unavailable, worked out by the escalation code with
the person counted as unavailable. For example: “While you are unavailable, MMHotline
skips you and wakes Priya Natarajan first.” The “Your availability” action
is “Set yourself unavailable”. Show “I'm available again” only while
unavailable. Label each duration input “For how long”, with examples such as
30m, 2h, or 3d.

Commit the change before displaying confirmation. A person's action on
personal unavailability produces a warn notice with the duration, routed
using the new state. The UI always shows the committed result.
Contact methods with `send_while_unavailable_or_muted` enabled can deliver
the notice, subject to team mute.

## Team mute

Team mute is one team setting: an end time, actor, and revision. Only an
administrator can start, replace, or end team mute. Team mute applies to every
member and shared contact method, including unavailable members, without
changing anyone's unavailability. Ending team mute early leaves personal settings
intact.

While team mute is active, suppress Pushover and phone calls, even if the
contact method's “Send even when I'm unavailable” setting is enabled. Email,
chat, and webhooks continue only when their contact method has
`send_while_unavailable_or_muted` enabled. Individuals cannot opt out of team mute. The SOS fallback when nobody is
available does not bypass team mute. Keeping team mute in the fallback
separates deliberate maintenance silence from accidentally having no responder.

Team mute is also not
[planned downtime](monitoring.md#planned-downtime), which stops alerts about
one alert source's missing check-ins. Team mute covers every alert but
stops only alert delivery.

Continue admitting alerts, recording incidents, and evaluating escalation
deadlines. Team mute suppresses delivery; it neither resolves an incident
nor changes responder eligibility or ownership. Recheck team mute before
dispatch, so queued work cannot bypass a newly started mute. An external
request already sent cannot be recalled. Cancel active Pushover emergencies
under the [receipt cancellation rules](notifications.md#pushover).

The team-mute form on the on-call page submits the action value `team_mute`.
A delivery suppressed by team mute stores `team mute` in the delivery's reason
field. On the on-call page, the section is “Mute the team”, with
“Muted until {time}” or “Not muted” beside the section heading.
The “Mute the team” section's explanation says: “For planned work. While the
team is muted, MMHotline sends no Pushover notifications or automated voice
calls.
Other contact methods send only if ‘Send even when the team is muted’ is
on.” The action is “Mute the team”. Show “Unmute the team” only while muted.

While muted, the on-call page also says that an SOS wakes nobody until the
mute ends.
Notices say “{name} muted the team until {time}: nobody is
woken”, “{name} unmuted the team: MMHotline wakes people again”, or “The team
mute ended: MMHotline wakes people again”.

Starting mute records a warn notice with the mute's duration and initiating
administrator. Ending mute records resolved-warn with a link to the affected
history.

Team-mute notices follow the
[routing rules](notifications.md#routing-rules) after the change. The team-mute notices' unmute link
reaches an authenticated administrator action, not a public toggle.

At expiry or explicit unmute, reconcile current state once. For each open,
unclaimed incident, evaluate any overdue escalation transition first. Then
make an SOS notification to the resulting escalation step's eligible targets
due immediately, combining any equivalent queued work.
Do not reannounce claimed/resolved incidents or replay ordinary
notifications and SOS repeats suppressed during mute. Future repeats
continue normally.

[Recovery](operation.md#recover-current-work) owns reconciling overdue
escalation and making only the current SOS notification due across restart.

## Taking on call

A user can take the team's on-call duties for a duration, for example during
a deployment. The form action value for taking on call is `claim_on_call`.
Giving on call back uses `end_on_call_claim`. The internal names remain from
before the displayed wording changed.
Taking on call from another member uses `take_from_member_id`.
The on-call page's “On call now” says whether the person who took on call is
woken first (“Priya Natarajan took on call. The
schedule names Daniel Reyes.”) or the schedule is used because that person
is unavailable or no longer on the team.

A person who is on call now is not offered “Take on call”. The
“Hand over early” panel tells the person on call
“You are on call until {time}.” and says that, to hand over sooner,
the incoming member signs in and chooses Take on call on the page.

Anyone else who is available reads “Taking on call makes MMHotline
wake you first for a new SOS, until the time you choose.” and gets “Take on
call”; while someone took it, the panel says “{name} took on call until
{time}.” and the action is “Take on call from {name}”. A person who is not
available reads “Make yourself available before taking on call.” The person who took on-call, or an administrator, gets
“Give on call back to the schedule” while the on-call duty lasts.

Taking on call records a warn alert.
Notices say “{name} takes
on-call until {time}”, and “On-call is back with the schedule” when someone
gives on-call back or the on-call period ends. Giving on call back, or its time ending,
returns on call to the current schedule and availability.

At most one person per installation has taken on-call at a time. An eligible
member may take on-call for themselves. Taking on-call from another person requires
the explicit “Take on call from {name}” action, `take_from_member_id`, and the
expected revision. The person who took on-call or an administrator may give on-call
back to the schedule.
Creation, replacement, duration changes, and ending check the revision in the
team transaction. A simultaneous stale action gets a conflict and the current
on-call state, never a silent overwrite.

If the person who took on call becomes unavailable, show “{name} took on call
but is unavailable or no longer on the team, so the schedule is used.” Use the schedule and its
backup options. Taking on-call lasts until the duty is given back or expires, so
becoming available before expiry restores its effect visibly. Expiry always uses the
current schedule.

On-call changes apply to new incidents and future escalation target selection.
On-call changes never transfer a claimed incident. An open incident's current
eligible target retains the time to claim. Loss of eligibility follows the
normal escalation rule. Moving an existing incident to the new responder is
an explicit “Escalate to someone else” or “Take over: I'm handling the incident” action.
Record who changed who is on call and why.

## Persistence and audit

Persist the controls and their actors/times. Expiration is based on the stored
end time after restart. Expiration does not require an in-memory timer to
have survived.
An expiry action must run once logically even if multiple scheduler ticks find
the expiry action due. Record manual changes and expirations separately from delivered notices.
Personal changes, team mute, and taking on-call each use their own
revision so a stale form cannot silently overwrite a newer change. Expiry also
advances the affected revision. Team mute and taking-on-call history retain the
last end time after expiry so recovery can identify the completed change.

## Acceptance evidence

Test the three controls independently, expiration/restart, invalid durations,
`send_while_unavailable_or_muted`, on-call fallback, unavailability surviving
team unmute, shared contact methods, team mute with nobody eligible, and one
current notification rather than a catch-up burst on unmute. Test explicit
replacement of whoever took on-call, concurrent conflicts, permissions to give
on-call back, a personally unavailable person who took on-call, and a claimed
incident retaining its owner. Exercise an availability change while an SOS
attempt is being prepared and while a sending service request is already in
flight.
