# Contact methods, routing, and delivery

Email, outgoing webhook, Telegram message, Slack message, Pushover notification,
and automated voice call contact methods are built. Automatic test messages,
severity subscriptions, a shared delivery outbox, and the delivery throttle
are also built. Calls use the team's optional Twilio account.
Real sending service acceptance has not yet been verified
([validation](../validation.md)).

An SOS is the urgent alert that wakes the person on call, even at night.
Claiming an incident tells the team
you are handling it, and MMHotline stops contacting the next person; claiming
from a notification uses Pushover acknowledgements and Telegram and Slack
buttons. Calls direct responders to Pushover or MMHotline to claim.

Monitoring tools use the alert sources of the team's settings file
([connecting a monitoring tool](#connecting-a-monitoring-tool)).

## Contact methods and subscriptions

A contact method is one address that MMHotline sends notifications to,
owned by a person or the team account, such as a phone number to call.
Each contact method has a stable ID, a contact method type, settings, and a
`send_while_unavailable_or_muted` setting. A person may have several contact
methods, within the contact method type and installation limits.
The page is “Contact methods” at `/contact-methods`, with
`/contact-methods/save`, `/contact-methods/remove`,
and `/contact-methods/test` actions. Panels say “Yours” or “Shared with the
team”; “Alerts to send through the contact method” offers Info, Warning, and SOS, except on an
automated voice call card, which offers SOS only.

A contact method type says what the person receives: `email`,
`automated_voice_call`, `pushover_notification`, `slack_message`,
`telegram_message`, or `outgoing_webhook`. Each type except
`outgoing_webhook` goes through one sending service.

| Card title | Contact method type value | Value label | Field |
| --- | --- | --- | --- |
| Email | `email` | Email address | `email_address` |
| Automated voice call | `automated_voice_call` | Phone number | `phone_number_to_call` |
| Pushover notification | `pushover_notification` | Pushover user key | `pushover_user_key` |
| Slack message | `slack_message` | Slack channel or direct message | `slack_conversation_id` |
| Telegram message | `telegram_message` | Telegram chat | `telegram_chat_id` |
| Outgoing webhook | `outgoing_webhook` | Outgoing webhook URL | `outgoing_webhook_url` |

The contact method type value is stored on the contact method and on each delivery.

Subscriptions attach a base severity to a contact method. A subscription is
unique per contact method and severity. The subscribed contact method receives
the base severity and the corresponding resolved severity, with one
exception: a resolved SOS message goes only to a contact method that
MMHotline sent something about that incident
([routing rules](#routing-rules), step 6).
A person can use different contact method types for different severities.

| Base severity | Default subscriptions |
| --- | --- |
| info | Personal Pushover and email, and shared information chat |
| warn | Personal Pushover and email, and shared main alert chat |
| sos | Personal Pushover and email, optional phone call, and shared main alert chat |

A warn from an alert source that starts or joins a triage session may wait
before its notifications are queued: with the settings file's
`triage_agent.warn_waits_for_the_agent_minutes` above 0 (15 when left out),
MMHotline saves the alert and queues nothing until the hold ends, unless the
triage agent's own report or request that names the warn takes it over, in
which case nothing is ever queued for the warn
([a warn waits for the agent](triage-agent.md#a-warn-waits-for-the-agent)).
When the hold ends, a background step queues the warn as any warn is, and
the contact methods subscribed then receive it; with the delivery queue
full, it waits for room. An SOS whose first waking step waits for the agent
queues its email and chat the same way, when the wait is over
([an SOS waits for the agent](triage-agent.md#an-sos-waits-for-the-agent)).
Info alerts and notices from MMHotline are never held this way.

Pushover and phone calls follow the active SOS target. An SOS subscription on
Pushover or automated voice call contact methods wakes a person only while an
escalation step contacts that person
([incidents](incidents.md#initial-notification-and-repeats)). Email,
Slack message, Telegram message, and outgoing webhook contact methods receive
an SOS as an ordinary subscribed alert.

## Local contact-method commands

The [command reference](../../reference/mmhotline-command.md#mmhotline-contact-methods-add) owns local
list, add, change, remove, and send-test syntax and JSON fields.
The trusted root command manages personal and shared methods through the same save, remove, and test operations as the pages.
The Contact methods page still permits only owners to edit personal methods. Command saves identify the command and Unix user in
the queued test message, without inventing a member ID. Preference-only changes
queue no test. Phone numbers retain their existing constraints.

Telegram and Slack still require the person's sending service connection
on the Contact methods page.
The command refuses direct creation and gives the full
page URL. Webhooks accept bearer tokens from a file. New webhook signing
secrets can be written to an atomic mode-0600 file, so a setup agent can connect
the receiver without exposing the secret in arguments or its conversation.

## Sending service setup

A sending service is an outside service that MMHotline sends notifications
through: the team's mail server, Twilio, Pushover, Slack, or Telegram. Each
sending service is set up in the team's
[settings file](settings-file.md#sending-services),
under `sending_services`, and `sudo mmhotline settings apply` checks it with
the service and saves it in MMHotline's database. Outgoing webhooks need no
sending service.

| Sending service name | Title | Contact method type |
| --- | --- | --- |
| `mail_server` | Mail server | `email` |
| `twilio` | Twilio | `automated_voice_call` |
| `pushover` | Pushover | `pushover_notification` |
| `slack` | Slack | `slack_message` |
| `telegram` | Telegram | `telegram_message` |

Sending service setups live in `mmhotline.db`, alongside contact methods and
delivery intent. The database keeps the setup that passed the service check
and was saved, not the file's latest values. A changed setup serves only
after the service accepts the setup. The earlier setup keeps serving when
the service refuses the replacement setup. The backup
replica therefore holds sending service tokens, as it already holds webhook bearer
tokens. Protect the replica as secret data.

Each contact method type that needs a sending service declares:

- Setup fields: names, labels, secret status, optional status, whitespace
  handling, and help text.
- Numbered guide steps with sending service links.
- The next step after setup.
- A sending service check, which returns non-secret settings or the sending
  service's reason for refusing the setup.

Shared pages and command output use the field declarations, guide steps,
next step, and check result without testing for a contact method type by name.

The shared operations list setups, check and save a setup, and remove a
setup. `mmhotline` reads the settings file and calls the shared setup
operations. Each sending service is a separately applied part of the settings
file. A changed part is saved only after
the sending service accepts the setup. A failed check keeps the current setup.
MMHotline records the check failure as the part's failure reason.
Secrets are stored separately from non-secret
settings and are never shown, returned, or logged after saving.

`/sending-services` lists sending services with their saved settings, read
only. One closed **Where sending services are set up** section holds the settings path and
first comment line, so each service panel stays focused on status.
`/sending-services/{sending_service}` is the
guided page for one service, with “All sending services” above the page title and a settings fold naming the service.
The guide explains what to create at the service, and the
settings file key each value goes in. The saved setup's status and next
step come first. **Set up · last check passed** records a successful setup
check, not continuous health. **Last successful check** folds away the
service's account identifiers and diagnostic details. A later failed settings check appears separately
with whether the earlier setup remains in use, and folded failure details.
Settings examples scroll inside the example to keep keys intact on phones.
The navigation shows Sending services
to administrators. Contact methods links administrators to the guided page
when a required sending service is absent; members see a request to ask an
administrator to set up that sending service.

Leaving a service out of the settings file is refused while contact methods
use that sending service, and the setup stays. The reason names the count
and explains how to remove the contact methods using the service.
For example: "cannot remove the Pushover setup: 1 Pushover notification
contact method uses the sending service setup. Remove those
contact methods on the Contact methods page, or with mmhotline
contact-methods remove --contact-method-id ID, first", so configuration
changes cannot silently drop alerts. When a service is newly set up,
`settings apply` prints the newly configured sending service's next step
under the part, such as "Next step:
Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification."

Setup changes rebuild the running contact method type instances from the database without
a restart. The delivery outbox, pages, Telegram poller, and Slack Socket Mode connection
use the current contact method type instances. The [operation design](operation.md#configuration-and-secrets)
owns storage and concurrent access. The
[command reference](../../reference/mmhotline-command.md#mmhotline-sending-services-list) owns the command flags and
public JSON fields.

## Contact method types

**Built** for every contact method type.

Each contact method type is one small piece of Go code that sends one
delivery attempt. The code declares the capabilities and presentation labels
listed below.
The contact method card, routing, tests, and delivery list read the declared
capabilities and labels. Shared code never tests
for a contact method type by name.
A contact method type may also pace its own deliveries (one call to a number at a time,
Twilio's request rate) and record a call's progress; the
shared outbox calls the contact method type at the pacing and progress-recording
points.

| Contact method type | How the contact method is added | How a person claims from the contact method | Team account may use the contact method |
| --- | --- | --- | --- |
| Email | Save an email address | The signed-in incident page | Yes |
| Automated voice call | Set up Twilio in the settings file, then add a phone number | Open MMHotline, or acknowledge the alert in Pushover, which also claims the incident | No |
| Pushover notification | Save a personal user key | Acknowledging the emergency message also claims the incident | No |
| Slack message | Connect through Slack | “Claim this incident (assign it to me)” button | Yes |
| Telegram message | Connect through Telegram | “Claim this incident (assign it to me)” button | Yes |
| Outgoing webhook | Save an outgoing webhook URL | Cannot claim | Yes |


Besides how a contact method is added, how a person claims, and whether the
team account may use the method, each type declares:

- **Wakes people** (Pushover and phone call): follows the active SOS
  target, and team mute always stops delivery.
- **Combines messages** (every contact method type except webhook): several waiting
  messages to one contact method may go out as one
  ([combining](#combining-and-the-delivery-throttle)).
- **Retry after an unknown outcome:** whether the outbox may send again when
  the result is unknown. Email, webhook, Telegram, and Slack may; SOS phone
  calls do not, except calls reserved before `mmhotline serve` restarts
  ([restart recovery](#sos-phone-calls)).
  Pushover may retry an unknown Info or Warning outcome, but
  never an unknown emergency send (an SOS).
- **Accepted label:** the delivery list's words for an accepted request, such
  as “Accepted by the mail server”.
- **Wakes sleeping people** (Pushover with iOS Critical Alerts, or phone call):
  decides what the on-call page says can wake a person at night. The person
  must test the phone settings.
- **Combines SOS escalation** (Pushover and phone call): SOS messages to one contact
  method that are due together go as one message about all of them.
- **Value fixed** (phone call): added with its own form; the number
  cannot be edited, only removed and added again.
- **SOS only** (phone call): never Info, Warning, or resolved messages.

A contact method type authenticates responses through its outbound connection:
Slack's button presses arrive over its Socket Mode connection, and Twilio's
call results come from MMHotline's own requests. The contact method type then
records a delivery report or claims incidents from the notification.
A contact method type never changes an incident directly.

Adding a contact method type requires one Go file with tests and one entry
in the list of contact method types.
The new type also needs one handler in the development fake sending services
server ([testing](../testing.md)). The card is built from the contact method
type's facts.

Each contact method keeps settings as JSON text and any secret separately.
The contact method type validates the settings, which use the explicit field
names above. A secret is shown once and never again.

An email contact method stores the email address in the JSON settings.
The contact method also keeps `sent_to`, the plain description of
the destination (a webhook's scheme and host). A delivery copies
`sent_to`, and a queued delivery is sent only while the contact method still
matches the delivery.

In development, one setting points chat and phone
sending services at a local fake sending services server.
Email still uses its saved SMTP setup: configure Mailpit to
inspect development email. The interface explains the development setup and
links to the fake
sending services page. [Operation](operation.md#fake-sending-services-for-development) and
[testing](../testing.md) own the details.

Alerts from the triage agent carry a [report summary](triage-agent.md#reports-to-people)
in every contact method type's notification, with "Open MMHotline for the
whole report." and the usual page link. The pages keep the report. Sending the summary instead of the full report
also applies to escalation, fallback, and resolved notifications of an
incident opened by a report; other senders keep their whole alert message.

## Pushover

Pushover notifications are the default for waking a responder. Each person
uses a personal Pushover account and enables iOS Critical Alerts. Sending service
acceptance cannot establish whether the person's phone made a sound.

An administrator creates a [Pushover application](https://pushover.net/apps/build)
named “MMHotline” and puts its API token in the settings file's
`sending_services.pushover.application_token`; MMHotline saves it as the
secret `pushover_application_token`. The check calls
`GET /1/apps/limits.json?token=TOKEN`. Invalid tokens are refused; the reported
monthly message limit is saved as `pushover_monthly_message_limit`.

The saved secret token identifies the application. A new token replaces the
saved application token and keeps every Pushover notification contact method.
A user key works with any Pushover application. A team replaces
the token when someone who knew the token leaves, or after a leak.
A token Pushover refuses is not saved, so the previously saved application
token keeps sending.

An emergency receipt is Pushover's reference for one emergency message.
The token replacement fails the
`sending_services.pushover` part while an emergency receipt is open, because
MMHotline needs the old token to read and cancel the receipt.
The SOS expiry is 1,500 seconds.

The MMHotline-down alarm's held messages carry the application token, so MMHotline's
next MMHotline-down alarm check-in, within 5 seconds, replaces the held messages.

A person adds a personal `pushover_user_key` on Contact methods. The root
command can also add the key for a person named by sign-in email address, with
`contact-methods add`. Without sending service setup, the Contact methods page
and the root command refuse the save:
"Pushover is not set up yet: an administrator adds sending_services.pushover
to the settings file /etc/mmhotline/mmhotline_settings.yml and applies the settings file;
https://ops.example.com/sending-services/pushover says how". A key has exactly 30
case-sensitive ASCII letters and digits. Shared team contact methods are not
allowed. Each person may keep three keys, like three phone numbers, to bound
accidental notification fan-out. Duplicate keys for one person are refused.

A Pushover contact method may also name one device, `pushover_device_name`
(optional, in the same settings JSON), so a person with several devices gets
alerts on one. The name follows Pushover's rule: up to 25 letters, digits, `_`
and `-`. Saving asks Pushover's `users/validate` for the user's devices and
refuses a name not among them, listing the names returned. Every request
for the contact method sends it as Pushover's `device`: deliveries, the
contact test, the test SOS, and the MMHotline-down alarm's held requests.
`SentTo` stays the user key, since acknowledgements are matched to it; display
adds "on device NAME". Reason: the acknowledgement still comes from the
person's own user, so it claims, unlike a group key.
Only the owner edits personal contact methods through the browser.

New Pushover notification contact methods subscribe to SOS, Warning, and Info, and follow
personal availability and team mute. Pushover is often a person's only contact
method, so with SOS alone every Warning and Info alert would reach nobody;
Warning goes at normal priority, which keeps the person's quiet hours in
Pushover, and Info at low priority, which is silent. A person can turn each
off.
Team mute always stops Pushover, including emergencies already repeating.

**A quiet test, and the proof that a phone wakes its owner.** A Pushover
contact method's test, sent when it is saved and by Send test, is an
ordinary notification: Pushover accepting it means only that MMHotline can
reach the phone, so `Last test message: accepted` keeps that meaning. Only an
emergency proves a phone sounds in silent mode, and a person should
acknowledge one emergency during setup, not two. The proof is the test SOS
(`incidents send-test-sos --sign-in-email-address`, [the test SOS](incidents.md#the-test-sos)); the suggested command leaves out the flag in a team of one person, where the test goes to that person anyway. MMHotline derives "emergency test
acknowledged" from stored data and keeps no new state: a Pushover receipt
that is `acknowledged` on a delivery to the contact method, for an incident
of the reserved test alert source whose history says the owner claimed it by
acknowledging in Pushover, or for an emergency contact test sent before tests
became quiet. An acknowledgement from another Pushover user (a group key)
never counts, because it cannot claim. Until proved, `contact-methods add`
prints a capitals line, and `contact-methods list` and the Contact methods
page say `Emergency test: not yet acknowledged`, so a dropped alert is not
hidden behind a quiet test.

| Message | Pushover priority | Emergency retry and expiry |
| --- | --- | --- |
| SOS | Emergency (`2`) | Retry every 30 seconds; expire after 1,500 seconds |
| Test of a contact method (title “MMHotline can reach this phone”) | Normal (`0`) | None; needs no acknowledgement |
| Warning | Normal (`0`) | None |
| Resolved message of an SOS that waited for a server replacement | Normal (`0`) | None |
| Info and other resolved messages | Low (`-1`), without sound or vibration | None |

An SOS that waited while the triage agent replaced a server sent a warn
alert at normal priority as the replacement started
([an SOS waits for a server replacement](triage-agent.md#an-sos-waits-for-a-server-replacement)),
so its resolved message goes at normal priority too: the person who heard
the start hears the end. If MMHotline woke the person before the SOS
resolved, the resolved message keeps this priority and has the ordinary
text. If nobody was woken, the message is the start warn's partner
([the end of a replacement](triage-agent.md#an-sos-waits-for-a-server-replacement)):

```text
[Resolved SOS] Your site works again: The server db02 stopped answering
The triage agent's server replacement fixed it; nobody needed to be woken.
```

Pushover gets it only when it got the SOS, an escalation, or the start warn
(routing rule 7). The Pushover contact method type chooses every priority in
one place.

MMHotline sets no sound; the person chooses the sound in Pushover. The retry
is Pushover's minimum, to wake a sleeping person again soon. Pushover repeats
an emergency at most 50 times, so with 30-second retries the phone stops
sounding after 1,500 seconds (25 minutes). The SOS expiry is set to 1,500
seconds. A later expiry would leave the receipt open while the phone is silent,
and MMHotline sends no
new emergency while a receipt is open, so a waiting SOS would go quiet. Once
the receipt expires, MMHotline's next eligible minute repeat of the SOS
escalation sends a new emergency
([repeat timer](incidents.md#initial-notification-and-repeats)).
MMHotline cancels the receipt earlier when the incident is claimed or resolved.
An SOS that joins a claimed incident and goes to the person who claimed it
keeps ringing until that person acknowledges it or the incident is resolved.
The ten-minute test expiry bounds the test if the person never acknowledges.
Acknowledging a test claims nothing and sends no claim result.

**One open emergency per counted incident and contact method.** While an
emergency receipt is open, minute repeats of any incident included in the
emergency message create no new Pushover message to the receipt's contact
method. Other waiting incidents may have their own emergency.
SOS messages due together combine, and the receipt
keeps exactly the incident IDs that message counted. When an unacknowledged
message expires, the next eligible minute repeat sends a fresh emergency.

For one alert, the title field names the alert and the message starts with
the alert's message, without repeating the title. Warning and Info keep the
notes and "At:" lines. An SOS keeps the acknowledgement instruction.
If nothing remains in the message, MMHotline sends "Open MMHotline for details."

MMHotline calls `POST /1/messages.json`. Titles keep at most 250 Unicode
characters; message text keeps at most 1,024. Only alert text is shortened.
A combined message lists whole alert titles while the titles fit, then
“And {N} more.” for the remaining alerts
as a combined email does.
The supplementary `url` is the incident page and `url_title` is “Open in
MMHotline”; the URL is never shortened. Pushover limits the URL to 512
characters. An SOS ends “Acknowledge this to tell MMHotline you are handling it. MMHotline then stops contacting other people.”, or, for a
combined message, “Acknowledge this to tell MMHotline you are handling all {N}. MMHotline then stops contacting other people.”

Each open receipt has a persisted next-poll time. The shared team scheduler
calls `GET /1/receipts/RECEIPT.json?token=TOKEN` no faster than every five
seconds. The deadline moves before the request, so a restart or concurrent
scheduler check of the team cannot poll early. Closed receipts have no
deadline. An installation with no open receipt makes no Pushover receipt
requests or receipt writes.

An acknowledgement claims exactly the counted incidents as the contact
method's owner through the shared claim-from-notification operation. Another
person's claim is never taken over. One low-priority result message says
“You claimed …”, “Already claimed by Bob”, or “This incident is already
resolved.” Receipt closure, claims, and result-message intent commit together.

A receipt acknowledged by another Pushover user never claims as the owner;
the key must be personal, not a delivery-group key.

A claim or resolution through any contact method type can make a receipt due
for cancellation. Cancellation is due when no incident counted by the receipt
still waits for the contact method's owner. A new SOS sent to the person who
claimed its incident still waits for that person.
MMHotline uses `POST /1/receipts/RECEIPT/cancel.json` with the application token.
Removed contact methods, changed destinations, removed members, personal
unavailability, and escalation away from the person also stop the old emergency.
A failed cancellation is retried on a persisted five-second deadline.

`status: 1` means “Accepted by Pushover”. Other HTTP 4xx responses fail with
Pushover's error text, except 429. HTTP 429, 5xx, and definite connection
failures use the shared outbox retries. No readable answer is an unknown
outcome. An emergency with an unknown outcome is never resent, even after
restart; the next incident repeat may send a new message. Info and Warning
may be retried. Without a returned receipt, MMHotline cannot poll or cancel
an emergency that Pushover may have accepted.

The [message API](https://pushover.net/api) and
[receipt API](https://pushover.net/api/receipts) own the sending service fields and limits.

### Connecting Pushover in one command

`sudo mmhotline connect-pushover` connects Pushover and adds one person's
phone at the person's own prompts
([acceptance A31b](../acceptance.md)). The command exists because the two
Pushover secrets must be typed by the person, where a coding agent never
sees them, and the two commands that do the job one secret each are long
enough (86 and about 140 characters) that a coding agent composed its own
shell line that wrote the secrets to files, and the person had to rejoin a
wrapped line by hand. The two long commands stay, for scripts.

The command adds no operation of its own. The token goes through
`settings set-secret`'s code with `--apply`, so the settings part's outside
test is the token check and the file keeps its owner and mode. The phone
goes through `contact-methods add`, so the contact method is saved, checked
and tested as from the page. Decisions:

- The token is saved, then checked by the apply, not checked first by the
  command. Reason: one check of the token, in the service, which holds
  the sending services' address (a development fake, or Pushover). A token
  Pushover refuses stays in the file and fails the part at each apply
  until replaced; the command says so and asks nothing more.
- The user key is checked through the command socket's
  `contact-methods check-pushover-user-key`, which only connect-pushover
  sends. It calls `users/validate` with the applied token and answers the
  device names. Reason: the device question comes before the save, and
  only the service holds the applied token. A key Pushover refuses (HTTP
  4xx) stops the command; Pushover not answering is not a refusal, as for
  the save, and the device question is then skipped.
- People come from the settings file's `people`, which the apply just
  applied. An unknown sign-in email address or device name is asked
  again, with the list, rather than ending the command, because the token
  is already saved and a person should not paste it again for a typing
  mistake.
- The command refuses without a terminal on standard input. Reason: a
  pipe means a script or an agent, which has the two long commands.

## Phone numbers: automated voice calls

**Built.** Real Twilio acceptance is pending
([validation](../validation.md)).

Adding a phone number creates an “Automated voice call” contact method subscribed to SOS.
The member types the number on the Contact methods page under “Add a phone number” (`/contact-methods/phone-number`), with
its country code: `+1 202 555 0123`, or `0044 7700 900123`; spaces, dashes,
dots, and parentheses are ignored, and the number is stored in E.164 form
(`+12025550123`). Only the owner adds a personal number, and the
team account never has a phone number. A member cannot add a phone number twice. The
number on a card cannot be edited: to change it, the member removes the card
and adds the new number. Phone calls are for SOS only: the automated voice call card
offers only SOS, and saving Info or Warning on it is refused.

The transaction that saves the phone number also queues a test call.
The number can receive SOS calls immediately.

MMHotline replaces control characters in the spoken text with spaces, because
a sender can put them in a title (for example the color codes in a forwarded
log line), and they make the call's instructions to Twilio invalid, so the call could fail.

The phone number form says: “By adding this number you agree to get alert
calls from MMHotline.”

**Countries.** MMHotline accepts numbers in the US, Canada, the 27 EU
countries, the UK, Norway, Switzerland, and Iceland. Other numbers are refused
when saved: “MMHotline can't call phone numbers in {country}
yet. Use a Pushover notification instead.” The refusal points to Pushover,
the other contact method meant to wake a person, not to email or chat.
Under +1, the area codes of the Caribbean countries and the US territories
(Puerto Rico, Guam, and the others) count as other countries, and toll-free,
premium (900), interexchange (700), and personal communications (5XX) numbers
are refused as not a person's phone. A calling code
MMHotline has no name for is named by its number (“the country with calling
code +224”).

**Numbers and caller ID.** Calls come from the calling number saved in the
`twilio` sending service setup. MMHotline sets no caller name: the outgoing
call carries only the number. The person's phone shows the name “MMHotline”
once the contact card below is saved in its contacts.

**The contact card.** “Download the MMHotline contact card”
(`/mmhotline-contact-card.vcf`, a vCard 3.0 named “MMHotline”) holds every
calling number, plus the `extra_phone_numbers_on_the_contact_card` saved in sending service setup, so a person can let all of them through Do Not Disturb or Emergency
Bypass. A call from a number missing from the card would be silenced on a
sleeping phone.
MMHotline remembers which version of the card each member
downloaded. When the card's numbers change, every member with an automated voice call
contact method is told once (an Info notice, “Download the MMHotline contact
card again”), and the contact methods page shows a notice until each member downloads the
contact card again. After a member adds an automated voice call contact method, the Contact
methods page guides the member until the first contact card download.
The steps are: download the contact card, allow the contact through Do Not
Disturb, then turn on Do Not Disturb and press “Send test” on the automated voice
call card.

**Waking people.** The on-call page shows whether a member has a Pushover
notification or automated voice call contact method that is not failing
([what can wake each person](people.md#what-can-wake-each-person)).

A member can add at most 3 phone numbers, limiting accidental call fan-out.

## Twilio phone calls

**Built**, against fake Twilio. The team supplies its own Twilio account.

The `twilio` sending service setup stores `twilio_account_sid`,
`twilio_api_key_sid`, the secret `twilio_api_key_secret`,
`twilio_phone_number_to_call_from`, and optional comma-separated `extra_phone_numbers_on_the_contact_card`.
Both the calling number and extra contact card numbers use E.164 format. A dedicated API
key can be revoked without changing the account's master credential used by
other applications. A standard API key, the narrowest kind that can make
calls, cannot read the account resource itself, so setup checks the calling
number instead. The setup check lists the account's IncomingPhoneNumbers
for the calling number and requires the number to belong to the account
and have voice capability. Twilio refuses the IncomingPhoneNumbers request
for a wrong key or secret.
A failed check preserves the current setup.

The account and calling number may change while contact methods exist: the
recipient numbers belong to people, not to Twilio. Changed calling or extra contact card
numbers send the existing contact card notice. Leaving the setup out of the
settings file still requires removing its contact methods first.
The setup and contact card checks prevent silent alert loss.

- **A call refused with HTTP 429.** Twilio made no call. MMHotline tries again
  a second later, up to the delivery's eight attempts. Every attempt takes a
  request slot. Other failed calls wait for the next SOS repeat.
- **Throughput.** MMHotline makes at most one Twilio call a second. Work waits
  in MMHotline, where a claim can cancel the delivery. The shared delivery
  worker tries to reserve a Twilio request slot again a second later.
  First contacts for an SOS go before repeats and test calls.
- **When phone calls fail.** If every call contact method tried for
  a person in the current escalation step failed definitively, with none
  pending or accepted, their other personal contact methods subscribed to SOS
  get the SOS once for that step. The message says “MMHotline could not reach
  you with an automated voice call about this SOS.” A failed call about several incidents
  triggers the check for definitive failure of every phone contact method
  for each incident, including incidents that joined the call. The
  deliveries that joined the call fail with it
  ([combining](#combining-and-the-delivery-throttle)).

Email uses the installation's SMTP server, set in the settings file's
`sending_services.mail_server`.

## SOS phone calls

**Built.** When calls are made, and how often they repeat, belongs to
[incidents](incidents.md#initial-notification-and-repeats).

- A call rings for 30 seconds and lasts at most 2 minutes (`TimeLimit` 120).
  There is no answering-machine detection.
- The call's markup (TwiML) is sent inline with two `Say` elements followed
  by `Hangup`. The call never accepts a claim. The spoken instructions name
  Pushover only when Pushover is set up on the installation.
- English only, in the sending service's default voice, with short
  sentences. MMHotline removes URLs, runs of 6 or more digits, and entire
  runs of 8 or more hex characters containing a digit from spoken titles.
  The spoken title then keeps the first 100 characters.

What the call says about one incident:

Calls say "Hotline" and "Watchdog", without the leading MM, in the built-in
sentences and in an alert's own text alike, because a text-to-speech voice
mangles "MMHotline" and "MMWatchdog". Written text (push messages, pages,
email, SMS) keeps the full names. MMX stays as it is.

> Hello. This is Hotline. An SOS incident needs someone now. Team:
> {team}. {title}. Open Hotline and claim it, or acknowledge the alert in
> Pushover, which also claims it. Claiming makes you responsible and stops the
> calls.

About several incidents:

> Hello. This is Hotline. {N} SOS incidents need someone now. Team:
> {team}. First: {title}. Second: {title}. Third: {title}. And {M} more.
> Open Hotline and claim them, or acknowledge the alert in Pushover, which
> also claims them. Claiming makes you responsible and stops the calls.

Without Pushover setup, the ending is “Open Hotline and claim it.
Claiming makes you responsible and stops the calls.”, or the same with
“them”. The whole message is spoken twice, then
Twilio hangs up. People claim on the signed-in incident page, or acknowledge
the alert in Pushover, which also claims the incident.

Only phone calls read the team's name (from `team_info`). If MMHotline cannot
read the team name, the attempt fails and the shared outbox retries the
attempt.

A call whose outcome is unknown is never retried; the next repeat is the
retry. One kind of call is the exception: a call reserved before `mmhotline serve` starts again. MMHotline cannot know whether
the reserved call was made, and makes the reserved call at once
([reserved deliveries](#email-implementation)).

## Sending service results

Twilio uses no sending service callbacks. The team's server accepts no public
connections, so Twilio cannot send a report to the server.
MMHotline fetches
Twilio's Call resource instead. No public address or callback origin is needed.
Chat actions follow their contact method type's transport, described below.

Each fetched final result goes through the shared delivery report operation.
The attempt's stored sending service ID identifies the call.
Repeated results cannot move the delivery backwards. `completed` means answered by
a person or voicemail. `busy`, `no-answer`, `failed`, and `canceled` also
end polling.
Canceled calls count as no answer, matching the existing delivery outcome.

The team scheduler fetches each active call no more often than every five
seconds, stopping at a final status or three minutes after creation. The
three-minute bound also releases a number if Twilio never supplies a result.
The scheduler keeps poll deadlines in memory and rebuilds them from active
attempts after restart, waiting five seconds before the first fetch.
The in-memory deadlines prevent overlapping requests without database writes
for unchanged sending service responses.
An idle installation makes no Twilio requests and writes no polling state.

## Chat, outgoing webhooks, and email

Email, webhook, Telegram, and Slack are built.

Connecting a contact method in MMHotline is quicker than copying its settings from
anywhere else:

- **Slack:** the team's own MMHotline Slack app. An administrator creates the
  app and puts its tokens in the settings file. Each member sends a connection code in the app's direct
  messages; an administrator mentions the app with a code in a shared channel.
- **Telegram:** the MMHotline bot. The member opens a link and presses Start,
  or an administrator adds the bot to a group. No bot token or chat ID is
  entered.
- **Email:** an address. An administrator sets up the SMTP server in the settings file; members
  enter only their email addresses.
- **Outgoing webhook:** a URL, pasted once; the only contact method type with anything to copy.

Slack and Telegram messages about an SOS carry
“Claim this incident (assign it to me)” and “Resolve” buttons.
A press acts through the same incident operations as the
dashboard ([claiming](#claiming-from-a-notification)).

### Slack

Slack uses the team's own app and an outbound Socket Mode connection. The
server needs no public address. Real Slack acceptance remains pending
([validation](../validation.md)). Chat apps notify, claim, and resolve an SOS;
they do not manage schedules, members, or other incident actions.

An administrator opens `/sending-services/slack` and follows its manifest link.
Slack's app creation page receives the complete manifest, already filled in:
app and bot named MMHotline, Socket Mode and interactivity enabled, and App
Home's messages tab enabled and writable. Bot scopes are `chat:write`,
`im:history`, and `app_mentions:read`; bot events are `message.im` and
`app_mention`. Token rotation is disabled. The administrator chooses the
workspace, installs the app, and copies its Bot User OAuth Token (`xoxb-`).
Under Basic Information, the administrator generates an app-level token
(`xapp-`) with `connections:write`, then puts the bot token and app-level token
in the settings file's `sending_services.slack`, as `bot_token` and
`app_level_token`.

The setup fields are secret `slack_bot_token` and `slack_app_level_token`.
The sending service check calls `auth.test` with the bot token, then opens one Socket
Mode connection with the app-level token, reads the app ID from its `hello`
(`connection_info.app_id`), and closes it. Slack's `bots.info` would give the
app ID too, but it needs the `users:read` scope, which lets the app read every
member's profile; the manifest keeps the three scopes MMHotline uses. Public settings are `slack_workspace_id`,
`slack_workspace_name`, `slack_bot_user_id`, and `slack_app_id`. Tokens live
only in the setup's secret settings, never on contact methods. A replacement
workspace fails the `sending_services.slack` part while Slack message
contact methods exist, because those conversations belong to the current
workspace. Removal or a workspace change
clears connection codes and Slack user links. New tokens for the workspace
in the currently saved Slack setup preserve connection codes and Slack user
links. Setup changes take effect without restarting `serve`.

- **Connect Slack** on Contact methods posts to `/connect-slack` with
  `slack_connection=personal_direct_message`. The page shows a one-time code
  and `https://slack.com/app_redirect?app={app_id}&team={workspace_id}`.
  The member opens the app's direct messages and sends the code there.
- **Add a shared Slack channel**, for administrators, uses
  `slack_connection=shared_channel`. The administrator invites the app into
  the channel and mentions it with the code, such as `@MMHotline CODE`.
  Only a channel mention may consume a shared-channel connection code.
- **Connection codes** are random 43-character URL-safe values. Each works
  once for 15 minutes. Only the connection code's SHA-256 hash is stored.
  A member may hold at most five unused codes, bounding accidental connection
  attempts. The wrong
  conversation kind leaves the code usable.

  Expired, unknown, or reused
  codes add no contact method and link nobody. Consumption, contact creation,
  the queued test, and linking the sending Slack user commit together.
  Only the first user to send a code can link through that code. A new code
  can connect an existing conversation without duplicating its contact method.
- **Connected conversations** are personal direct messages or channels shared
  with the team. New methods receive Info, Warning, and SOS, even during
  personal unavailability or team mute, like Telegram. The member changes
  subscriptions on the card. The destination description names the Slack
  conversation ID and workspace; no extra profile-reading scope is needed
  for presentation. Conversation IDs cannot be edited by hand.
- **User links** associate a Slack workspace and user with a current member.
  A fresh code from another member can move the sending Slack user's
  association to the new member. A removed member cannot act through an old
  Slack link.
- **Sending** always uses `chat.postMessage` and the setup's bot token.
  The delivery records workspace, conversation, and returned message `ts`.
  Text is plain in a section block of at most 3,000 characters; fallback text
  escapes `&`, `<`, and `>`. Only the alert message is shortened, preserving
  the time, notes, SOS instruction, and incident link. Acceptance means
  “Posted to Slack”. HTTP 429, `service_unavailable`, and `ratelimited` are
  retried. HTTP 5xx, an unreadable or missing answer, `internal_error`,
  `fatal_error`, and `request_timeout` are unknown outcomes and retried.
  A repeated chat message is harmless. Other refusals fail with Slack's code.
- **Buttons** appear only on an SOS message about one incident: “Claim this
  incident (assign it to me)” and “Resolve”. Values contain `c.` or `r.` and the delivery ID.
  Socket Mode authenticates the source, so button values containing the action
  and delivery ID need no signature.
  A press must match the delivery's workspace, conversation, and message
  timestamp, and the delivery must have been sent with buttons. Copied
  buttons on another message cannot act. Only a Slack user linked to a
  current member can claim or resolve through the shared incident operations.
  Unknown users receive an ephemeral reply with the Contact methods and
  incident links, including users from another workspace.
- **After a press**, `chat.update` replaces
  only the pressed message's buttons with one state button that opens the
  incident. After a claim, a working Resolve button stays beside the state
  button, with the original Resolve button's value, so the person who
  claimed can resolve from the chat without signing in. After a resolve, only
  the state button remains. `chat.postEphemeral` tells
  the presser the result, including another member's existing claim or an
  already resolved incident. Slack does not guarantee ephemeral delivery to
  an inactive user; the incident page remains authoritative.

The standard library has no WebSocket client. `github.com/coder/websocket`
provides the one client and the fake server's matching protocol, without
transitive dependencies or cgo. The ISC license for
`github.com/coder/websocket` is included in
[third-party notices](../../../THIRD_PARTY_NOTICES.md). A small protocol library
avoids maintaining WebSocket framing, TLS upgrade, ping/pong, and cancellation.
[Operation](operation.md#sending-service-results-and-chat-buttons) owns connection
lifetime, acknowledgements, and reconnect delays. Slack's
[manifest reference](https://docs.slack.dev/reference/app-manifest/),
[Socket Mode guide](https://docs.slack.dev/apis/events-api/using-socket-mode/),
and [message API](https://docs.slack.dev/reference/methods/chat.postMessage/)
own sending service fields and limits.

### Telegram

**Built**, through the MMHotline bot and the Telegram Bot API; real Telegram
acceptance is pending ([validation](../validation.md)).

An administrator follows the numbered guide at `/sending-services/telegram`:
create a bot in [BotFather](https://t.me/BotFather) and put its token in the
settings file's `sending_services.telegram.bot_token`. `settings apply`
checks and saves the bot token. MMHotline calls `getMe` and stores
`telegram_bot_username` and `telegram_bot_id` from Telegram, with
`telegram_bot_token` stored only in secret settings. The
next step, printed under the part, is to connect your own Telegram chat
under Contact methods; saving that contact method sends the test message.

Changing to a different bot fails the `sending_services.telegram` part while
Telegram message contact methods exist.
The digits before the token's colon identify the bot; existing chats belong
to that bot, so replacing the bot would stop alerts. A new token for the same
bot after `/revoke` in BotFather is accepted. Setting up Telegram starts the
poller, and removing an unused setup stops the poller, without restarting
`serve`. Removing setup or changing bots clears old connection links and
Telegram user links. An old link must not connect a chat to the replacement
bot. Rotating a token for the bot in the currently saved Telegram setup
keeps the connections.

- **Connect Telegram** (any member) and **Add the MMHotline bot to a Telegram
  group** (administrators) are buttons on Contact methods. Each posts to
  `/connect-telegram` and shows a page with the link
  `https://t.me/{bot}?start={token}` or `?startgroup={token}`. The page links
  instead of redirecting, because the page's form-action rule lets a form's
  answer lead only to MMHotline itself. The bot's username comes from
  the stored result of Telegram's setup `getMe` check.
- **The start token** is 43 URL-safe characters (Telegram allows 64), single
  use, and valid for 15 minutes. Only the start token's SHA-256 hash is stored,
  in `mmhotline.db` (`telegram_start_tokens`). The `/start` message carries
  the token, which identifies the member and the kind of contact method to
  create. A member holds
  at most 5 unused links at a time.

  A personal link works only in a private
  chat with the bot and a group link only in a group; the wrong kind of chat
  gets a plain answer and leaves the link usable. Token consumption, contact
  creation, and user linking share one transaction. Repeating `/start` with
  the consumed token from the chat that consumed the token within the
  15 minutes changes nothing.
- **Pressing Start** adds the chat as a contact method: the member's own, or,
  for a group, one shared with the team. The new contact method subscribes
  to Info, Warning, and SOS. Like email, the contact method sends when the
  person is unavailable or the team is muted. The owner changes the subscriptions
  and the unavailable-or-muted setting on the contact method's card.

  The contact method's “Sent to” names the chat, such as
  `Telegram chat with Barbara` or `Telegram group “Ops alerts”`.
  The chat itself cannot be edited: nobody types a chat ID, so nobody can
  point alerts at someone else's chat. The bot answers in the chat, saying the chat is connected.
- **Linking.** The Telegram user who pressed Start (for a group, the
  administrator who added the bot) is linked to the member who made the link,
  in the database (`telegram_user_links`). When another member creates a
  connection link, connecting through the new link moves the Telegram user
  association to that member. Only the Telegram user who used a start link
  first is ever linked through it: in a group everyone sees the
  `/start` message, and someone else repeating it links nobody.
- **A group that becomes a supergroup** (Telegram does this as a group grows)
  gets a new chat ID; MMHotline moves the group's contact method to it when
  Telegram announces the change, and deliveries already waiting go there.
- MMHotline asks Telegram for new messages and button presses
  (long polling), so Telegram needs no inbound endpoint
  ([operation](operation.md#sending-service-results-and-chat-buttons)).
- **Buttons.** An SOS message about one incident carries “Claim this
  incident (assign it to me)” and “Resolve”. A button's `callback_data` names the action, the
  delivery (`c:` or `r:`, then the delivery ID in base64url: 24
  bytes of Telegram's 64). The delivery records the chat and message it was
  sent as, and a press counts only when it comes from exactly that message.
  Only a Telegram user linked to a current member may press; anyone else gets:
  “MMHotline does not know your Telegram account. Connect the account under Contact methods, or claim the incident in MMHotline: {incident link}”. A button
  belongs to the message the button was sent with.
  Combined and “{N} more” messages have no buttons.
  A press on the wrong message or a message sent without buttons answers
  “MMHotline cannot use this button. Open MMHotline to see the incident.”
- **After a press** the pressed message's buttons become one button that
  opens the incident and shows its state: “Claimed by Alice”, “Resolved by
  Alice”, or “Resolved”. After a claim, a working “Resolve” button stays
  beside it, with the same `callback_data` as the original Resolve button,
  so the person who claimed can resolve from the chat without signing in.
  After a resolve, only the state button remains. The person who pressed sees the result (“Claimed by
  Alice”, “Already claimed by Bob”, “This incident is already resolved.”).
  Other copies of the message are not updated.
- **Sending.** Messages are plain text of at most 4,000 UTF-16 code units,
  the unit Telegram counts in. A longer alert message is shortened, never the
  time, notes, SOS instruction, or link after it. Every Bot API call has a
  time limit, so a hung connection cannot stop button presses and connections.
  The poller decodes each update separately, so one unreadable update cannot
  stop button presses and connections.

  Telegram taking the message is
  “Sent to Telegram”. HTTP 429 fails and is retried; a
  5xx answer or no answer is an unknown outcome, retried too, because Telegram
  has no idempotency key and a repeated chat message is harmless. When a lost
  answer causes a second copy of an SOS message, only the copy MMHotline
  recorded has working buttons. Other refusals (a chat the bot was removed
  from, a blocked bot) fail with Telegram's own description.

### Outgoing webhook

**Built.** An outgoing webhook contact method sends each alert as a JSON `POST` to the
team's URL. Webhooks are one way: nobody claims from one. A new webhook
can send at once; Send test shows whether the receiver accepts the webhook.

- The URL must start with `https://`. The destination shown to other members,
  in delivery records, and in notices is the scheme and host only, because a
  URL's path or query may hold a token. The full URL appears only in the edit
  form, to the people who may edit the webhook.
- The request has a 10-second timeout and follows no redirects. Only public
  internet addresses are allowed. MMHotline refuses loopback, private,
  link-local (including cloud metadata addresses), carrier-grade NAT, and
  special ranges. The webhook address rule covers IPv4, IPv6, and IPv4
  written as IPv6.

  MMHotline checks the address at the
  moment it connects, so a second DNS answer cannot redirect the request. A
  webhook request leaves from the machine running MMHotline. In development
  mode a webhook may point only at the fake webhook receiver.
- Body fields, omitted when empty: `mmhotline_delivery_id`, `delivery_purpose`,
  `severity`, `title`, `message`, `notes_from_mmhotline`,
  `mmhotline_alert_id`, `mmhotline_incident_id`, `incident_status`, `incident_url`,
  `alert_url`, `alert_received_at`.
- Signed with `Mmhotline-Signature: timestamp_unix_seconds=...,hmac_sha256_hex=...`,
  using the [alert-intake scheme](ingestion.md#alert-source-signing).
  The key is a “Webhook signing secret (hex)”, shown once.
  The `Mmhotline-Delivery-Id` header lets a receiver drop repeats.
  An optional “Webhook bearer token
  (optional)” is sent as `Authorization: Bearer`; saving it empty removes it.
- Rotation: “Add a second webhook signing secret (for rotation)” shows a new
  secret once. The older secret keeps signing until it is revoked, so the
  receiver can first accept the current and second signing secrets.
  The old secret is then revoked, and the second signing secret signs
  requests. A webhook always keeps one secret.
- A 2xx answer is accepted (“Accepted by the webhook”, with the HTTP code as
  the detail). 408, 429, 5xx (“The webhook answered HTTP 503”), and
  connection failures (“Could not connect to the webhook”) are retried.
  Retries use the same body on the shared schedule (at most 8 attempts, the wait doubling from 5 seconds to at most 5 minutes, so about 10 minutes in all; never after 23 hours). No answer
  within 10 seconds is an unknown outcome, retried too. Other answers,
  redirects included, fail (“Refused by the webhook (HTTP 404)”).
- A webhook that failed 3 times in a row keeps only its newest pending
  delivery; SOS alerts and tests are never dropped. Older pending non-SOS
  deliveries, except tests, finish as “Not sent: the contact method has been failing since {time}”, and administrators
  are told once per failing streak, as a Warning from MMHotline (“MMHotline cannot deliver alerts to {host}”). A success, or a changed URL, starts a new count; results
  for an earlier URL do not count. If the notice cannot be recorded, the
  delivery's own result is still saved.
- An installation has at most 20 outgoing webhook contact methods. Notices from MMHotline to
  administrators never go to webhooks, which programs read, not people.

## Claiming from a notification

**Built** as one operation, used by Pushover acknowledgements, the Telegram and Slack
buttons. Phone calls direct people to Pushover or MMHotline. SOS emails link to the signed-in incident page.

A person claims from the notification itself:

- **Pushover:** acknowledging an emergency claims exactly the incidents it counted,
  as the contact method owner. A test claims nothing.
- **Slack and Telegram:** “Claim this incident (assign it to me)” and
  “Resolve”, pressed by a linked member.

Pushover acknowledgements and Slack and Telegram button actions use one
incident operation ([incidents](incidents.md#claiming-from-a-notification)).
An open incident is claimed. An incident claimed by someone else is never
taken over. The answer names who holds the incident or says the incident is
resolved. Resolving from a notification is possible
only in Slack and Telegram, never by Pushover acknowledgement.

## Combining and the delivery throttle

**Built**, including the SOS chat rule and chat buttons, which Telegram and
Slack use.

Common-sense throttles keep a misconfiguration, the team's or MMHotline's,
from turning into more messages than a person can read.
No throttle ever stops or delays an SOS, and
nothing accepted is dropped from history.

For every contact method type except webhook:

- Warning, Info, and resolved messages go at most one a minute to each contact
  method. Anything more waits and goes out as one combined message. After 10
  messages to one contact method in an hour, one combined message goes every 10
  minutes.
- A waiting message says: “Waiting until {time} to send the notification together with other messages to the contact method”. A new Warning, Info, or
  resolved message for a contact method with a waiting combined message
  joins the waiting message and finishes at once, so throttling never piles up
  pending deliveries.
  Once the combined message goes, the outcome says
  “Sent together with {n} other alerts”. Each delivery keeps its own record
  and links to the combined message. A message
  that has had its first attempt takes no more joiners, so its retries send
  the same content.
- A combined message can end without being sent. It can fail for good, be
  cancelled (for example, the contact method was removed), or be suppressed
  (for example, the team is muted). The messages in it then end the same
  way, so the record never says they were sent. Each one says why, for
  example “Not sent: the message it was combined into failed: {reason}”.
  When the outcome is unknown and MMHotline will not send again, the
  messages in it are unknown too. A combined SOS escalation message that is
  cancelled or suppressed is different: the incidents in it still need
  waking, so the oldest one becomes a new message and the others join it.
- A combined email's subject is “[MMHotline] {n} new alerts”. The email lists
  up to 20 alerts, newest first, each with its link, then “See all”.
- Test messages are never throttled.
- Combined chat messages have no buttons.
- A server replacement's START warn and the resolved message of an SOS that
  waited for a replacement are never delayed or combined: they tell the
  person when they will be woken and that the outage ended, and each comes
  once per replacement, so neither can be the blast the throttle guards
  against.
- SOS is never delayed by the Warning, Info, and resolved-message throttle
  and never combined with Warning or Info. The first 10 SOS chat messages
  in 10 minutes to one contact method go
  one by one with their buttons; after that, one message says “{N} more SOS
  incidents. Open MMHotline: {link}”, linking to the dashboard. SOS Pushover messages and phone
  calls follow their own rules above.
- In the SOS chat rule, a chat is a contact method type whose claim method
  is a button (Slack, Telegram).
  The first “{N} more” message goes at once. An SOS arriving while a
  “{N} more” message waits joins the waiting message.
  An SOS arriving after the previous combined SOS chat message was sent
  starts the next combined message. The next message waits until the later
  of the SOS arrival and one minute after the previous combined SOS chat
  message was sent. The one-minute interval is
  `contact_method_minimum_message_interval`.

  An SOS storm therefore gives
  10 messages with buttons, then at most one
  “{N} more” message a minute. N counts the SOS incidents the message stands
  for. The people on call are still reached at once by the SOS escalation.

Webhooks receive alerts individually. Each webhook sends at most 60 non-SOS
alerts a minute. Beyond the 60-alert-per-minute limit, alerts other than SOS
are skipped with “Not sent: this webhook got more than 60
alerts in one minute”. SOS is never skipped.

The constants are `contact_method_minimum_message_interval` (1 minute),
`contact_method_slow_down_after_messages_per_hour` (10),
`contact_method_slowed_message_interval` (10 minutes), and
`webhook_maximum_alerts_per_minute` (60). Limits on new alerts belong to
[intake](ingestion.md#alert-intake).

## Email implementation

Email sends one message per delivery over SMTP for personal and shared
contact methods.
An administrator puts the mail server in the settings file's
`sending_services.mail_server`, as `/sending-services/mail_server` shows.
The checked setup lives in the database, including its secret password. No restart is needed. The settings file's `web_url` supplies
the address for email links. Email setup is required before saving an email contact
method. Missing setup prevents the save; no new test delivery is queued.

| Settings file key under `sending_services.mail_server` | Meaning |
| --- | --- |
| `smtp_server` | Host name or IP address and port, such as `smtp.example.com:587` |
| `smtp_connection_security` | Required: `starttls`, `implicit_tls`, or `unencrypted`; no default |
| `smtp_user_name` | Optional user name; requires `smtp_password` |
| `smtp_password` | Secret password; requires `smtp_user_name`; spaces belong to the password |
| `email_from_address` | Plain ASCII address, without a display name, at most 254 bytes |

`starttls` starts in plain text and requires the server to offer STARTTLS before
continuing. `implicit_tls` encrypts from the first byte. STARTTLS and implicit
TLS check the certificate and host name against the system authorities,
with TLS 1.2 or later. Server replies are bounded to 1 MiB per conversation
to keep an
unexpected greeting or refusal from exhausting the service’s memory. The
SMTP greeting uses the machine’s host name, or `localhost` when unavailable.
Diagnostics keep the server’s reply code and text on one line, cut after
500 bytes, with the password and its authentication encodings removed.
Truncating diagnostics, keeping replies on one line, and removing credentials
keep pages and delivery records readable without exposing credentials.
`unencrypted` is allowed for any server, including a private relay. The
Sending services page says that the password and alerts cross the network
unencrypted. Connection security is explicit so the operator sees whether
SMTP permits unencrypted traffic.

The SMTP user name and password may be empty for a relay without
authentication. Otherwise MMHotline uses PLAIN when offered, then LOGIN.
Credentials must be UTF-8 text without control characters.
The setup check connects, greets, applies the
chosen security, authenticates if configured, and quits without sending an
envelope or message. A refusal includes the server's reply and preserves the
previous setup. The guided page at `/sending-services/mail_server` ends by
adding a personal email contact method,
which sends the test message.

Sender and recipient addresses are plain ASCII addresses without display names.
Messages show `MMHotline <email_from_address>` as From. Each message has a
UTF-8 plain-text body, base64 transfer encoding folded at 76 characters, an
encoded non-ASCII Subject, Date, and `Auto-Submitted: auto-generated` to prevent
automatic replies. Base64 preserves the exact body, including line endings
and trailing whitespace. Message-ID is `<delivery-id@sender-domain>`; retries
keep the recorded sender and therefore the same Message-ID. Duplicate mail
can still arrive: Message-ID lets mail clients recognize copies, but does not
make SMTP idempotent.

The whole SMTP conversation has a 30-second limit, including connection,
TLS, authentication, message transfer, and acceptance.
The 30-second conversation limit allows mail-server checks time to finish
and avoids duplicates from giving up too early. A 250
reply after DATA is “Accepted by the mail server”, without proving mailbox
receipt. A failed QUIT after acceptance changes nothing. A 4xx reply is a
failed, retryable attempt; a 5xx reply fails permanently with the server's
reply text. A connection failure before message transfer is failed and
retryable. A lost connection during or after transfer without an acceptance
reply is uncertain and retried: a duplicate email is preferable to a lost
alert. The outbox owns all retries.

Email has `send_while_unavailable_or_muted` enabled by default, with
Info/Warning/SOS subscriptions initially selected in the form. A user can turn
each off. SOS emails are sent as ordinary alert notifications; they are never
repeated by SOS timers. SOS emails link to the incident page, where the person
[signs in](people.md#authentication-and-recovery) through Cloudflare Access
or with a sign-in link.

Each installation permits 1,000 active contact methods and 10,000 pending alert deliveries.
Automatic save tests bypass the pending-delivery limit so a backlog cannot prevent
contact method setup.
Admission that would exceed the pending limit fails in its existing
transaction.
No accepted alert is left without notification intent. Suppressed and finished records do not
consume pending capacity. Part of the pending limit is reserved for SOS, so
ordinary deliveries can never block SOS admission. History
retention is not defined; see
[recovery and retained history](open-questions.md#q10-recovery-retries-and-retained-history).

The 10,000 pending deliveries have three limits:

- Ordinary deliveries stop at 9,000. The last 1,000 are only for SOS.
- Each alert source may have at most 250 pending SOS deliveries. SOS
  escalation deliveries count toward this share, but the share never
  refuses them, because their incident is already accepted. A new SOS from
  an alert source at its share is refused with `429`. MMHotline then opens
  its SOS about the refusal ([intake](ingestion.md#alert-intake)).
- MMHotline's own SOS is never refused by these limits. This SOS says that
  an alert source stopped checking in, or that MMHotline refuses an alert
  source's SOS alerts. A full queue is exactly when people need it.
- MMHotline's messages about the triage agent's run limit, and a playbook
  run's result it sends to a person at the limit, are never refused by
  these limits either. They go in the transaction that wakes the SOS the
  limit sends straight to people, and a refusal would roll the wake back
  ([the run limit](triage-agent.md#once-the-limit-holds)).

Why these numbers:

- 250 is a quarter of the SOS reserve. Four alert sources must flood at
  once to use up the reserve. 250 still holds 25 incidents' messages to 10
  contact methods subscribed to SOS. At about one delivery a second, 250
  deliveries take about 4 minutes to send.
- MMHotline's own SOS stays bounded without a limit. At most one is
  unresolved per alert source. When someone has claimed the incident, a
  new refusal adds no message to that person while an earlier one still
  waits to be sent. The incident history still records each refusal.
- The run limit's messages stay bounded too: each needs counted agent
  runs to change the limit's state, or a playbook run that an agent's run
  asked for.
  So a sender refused again and again cannot fill the queue with MMHotline's
  messages.

Due SOS deliveries go before older ordinary ones, so an
SOS never waits behind a team's backlog. Among due SOS deliveries, the
alert source with the fewest due SOS deliveries goes first. So one alert
source's flood never puts another alert source's SOS behind its whole
backlog. The deliveries of one alert source go oldest first. The worker reserves one delivery
durably. The worker confirms membership, contact method
revision, incident relevance, and availability again immediately before
dispatch. A delivery that cannot be checked fails with the reason, and the
error is reported in the server log. The worker always picks the same due
delivery first, so a delivery that kept failing the check would otherwise
hold up every delivery behind it. The sending service call runs outside SQLite transactions.

An email reservation expires after
45 seconds, leaving time to save the result of a 30-second SMTP conversation;
other contact method type reservations expire after 30 seconds.
Other contact method types keep the shared 15-second attempt limit unless their facts say
otherwise. After an interrupted worker, the attempt becomes uncertain before
a retry. After `mmhotline serve` starts, it does not wait for reservation expiry:
the worker that reserved a delivery is gone, so on its first visit to a team
the background scheduler records every reserved attempt as uncertain. Results
record what actually happened without changing incident state or a
replacement contact method.

Retries preserve the delivery ID, the sender the first attempt used,
recipient address, message, and public origin.
(If a worker stops during the first attempt before
recording the sender, the retry uses the sender configured when the retry
starts.)

After an interrupted worker, only contact method types that may resend after an unknown outcome
are retried. A phone call is not: when its reservation runs out, the worker
may well have made the call and only failed to save the result, and the next
repeat is the retry. A phone call reserved before `mmhotline serve` starts again is different. Nothing
can tell whether that call was made, so it is made again at once: a duplicate
is better than a dropped alert.

There are at most eight attempts: start with five seconds of backoff, double
the backoff up to five minutes, and stop retrying 23 hours after the first attempt. The 23-hour horizon bounds stale retries while
retaining ordinary pending work across a short outage. Network failures during message transfer are
uncertain; known rejection is failed; successful sending service admission is accepted.
Retrying is permitted only for the
adapter's retryable outcomes.

Exhausted uncertainty remains visible. SOS
intents with the same incident revision and contact method coalesce while an
intent for the incident revision and contact method is pending or in flight, preventing missed timer ticks from accumulating.
Webhooks use the shared retry schedule.
When any contact method has 3 consecutive failed or uncertain attempts
(a Telegram group the bot was removed from, an email address that
bounces at the sending service), administrators are told once per failing streak with
a Warning from MMHotline: “MMHotline cannot deliver alerts to {destination}”,
for example “MMHotline cannot deliver alerts to Slack channel #ops-alerts in
the Acme workspace”. For a webhook the destination is the host name. A
member's own contact method is named by its owner, such as “Alice's Pushover
notification”, never by its phone number, email address, or Pushover user
key: every member reads the alert history, and a Pushover user key would let
anyone with a Pushover application send that member notifications.

## Monitoring tools

A monitoring tool sends signed alerts to MMHotline. MMHotline routes each alert
by severity and who is on call, and escalates an unanswered SOS. If the tool
also sends directly to the contact methods MMHotline would notify, people
receive duplicate notifications.

### Connecting a monitoring tool

The team lists the alert source in the settings file's `alert_sources` with
an alert source ID, such as `monitoring`. The signing secret comes from the
team's vault, which the monitoring tool also reads
([settings file](settings-file.md#alert-sources)). The tool sends signed alerts to
`POST /alert-intake/{alert_source_id}` and signed check-ins every 30 seconds
to `POST /check-in/{alert_source_id}`. [Ingestion](ingestion.md) defines
the request bodies and signatures.

A check-in lists the sender's node name and any whole-node maintenance
windows.
A window for the sending node counts as planned downtime for missing
check-ins. [Monitoring](monitoring.md) defines the timers and downtime
rules.

To rotate the secret, the team adds a second secret to the source's
`signing_secrets` and applies the file, replaces the secret in the
monitoring tool, then removes the old secret from the file. MMHotline accepts
the old and replacement signing secrets while the settings file lists the
old and replacement secrets.

## Routing rules

1. Map the alert to its base severity and find subscribed contact methods.
2. Apply the alert's recipient rule: ordinary subscribers, active SOS targets,
   or the explicitly affected users for a system notification. Pushover messages
   and phone calls about an SOS go only to active SOS targets.
3. Require a contact method that passed validation when saved and has not
   been removed.
4. Suppress recipients who are unavailable unless the recipient's contact
   method has
   `send_while_unavailable_or_muted` enabled. Record the reason.
5. Apply [team mute](availability.md#team-mute) to personal and shared
   contact methods: suppress Pushover and phone calls; permit other contact method types only
   with `send_while_unavailable_or_muted`.
6. Never make a phone call for resolved-sos, regardless of subscriptions.
7. Send a resolved-sos message only to a contact method that MMHotline
   attempted to send an SOS or a warning about the incident: the SOS
   itself, an escalation, or a warning on the incident, such as a server
   replacement's start. Record the others as suppressed, with the reason
   "nothing about the incident was sent to this contact method".
8. Persist the resulting notification intent and hand it to shared delivery.

**Why step 7 exists.** A resolved message tells the end of something the
person was told about. A person whose Pushover never got the SOS, because
the SOS waited for the triage agent and the agent fixed the problem, has
no beginning to end, and "TITLE is ok again" would confuse them. Email,
which gets the SOS at once, still gets its resolved message.

**Which deliveries count.** A delivery counts when MMHotline tried to send
it or still means to send it: every state except suppressed and cancelled,
and a cancelled delivery that was attempted first. A delivery that ended
failed or uncertain counts, because MMHotline tried and the person may have
been reached; "MMHotline attempted to send it" is the rule, not "it
arrived". Suppressed and cancelled-before-sending deliveries, such as an
SOS message to Pushover that follows the active target, never counted.
Info does not count: the quiet note that the triage agent is looking into an
SOS says nothing is needed, so its end needs no message.

**A resolved-warn of warnings never sent.** A resolved-warn pairs with a
sender's own warns of the same alert source and `sender_incident_key`, not
with an incident, so step 7 does not apply to it. The same reason holds in
its own form: when every warn it ends since the previous resolved-warn of
that key was held for the triage agent and never sent (its alert resolved
while held, or the agent took it over), MMHotline sends the resolved-warn to
nobody, and its alert page says so. A resolved-warn with no such warn, or
with any warn MMHotline sent or still holds, goes as usual.

`send_while_unavailable_or_muted` defaults to enabled for nonintrusive
email and chat and disabled for Pushover and phone calls.
`send_while_unavailable_or_muted` bypasses personal unavailability, but the
setting does not turn an ineligible person into the on-call
responder or bypass contact method permissions.

The team account owns shared contact methods such as a chatroom or group email.
The team account cannot log in and never has Pushover notification or
automated voice call contact methods. Team mute applies to the team account
as well as individual recipients.

## Message content

Include severity, title, message, a relevant alert/incident link, timestamp,
and meaningful normalization notes.
SOS messages identify the required action.
Escalation and taking-on-call notices identify the initiator and who is on call.
Render user content safely for the contact method type. Do not log credentials,
sending service and connection tokens in operational diagnostics.

Messages are plain text. Each contact method type shortens titles to its own limits
([SOS phone calls](#sos-phone-calls)). A sending service limit must
produce an inspectable result.

## Delivery records

Represent each delivery to one contact method separately from an incident's
state. Preserve a stable delivery ID, purpose, `sent_to`,
attempt number, sending service reference, timestamps, outcome, next retry time, and
a bounded diagnostic.

| Delivery state | Meaning |
| --- | --- |
| queued | Waiting for the delivery's scheduled attempt time or an available worker |
| sending | A worker has started the attempt |
| accepted | The sending service or the outgoing webhook's receiver took the request |
| delivered | The sending service reported receipt, where the contact method type offers it |
| failed | The attempt definitely did not go out |
| uncertain | A timeout or lost response left the outcome unknown |
| cancelled | No longer needed, such as after a claim, before the delivery was sent |
| suppressed | Not sent because of mute, unavailability, or a routing rule; the reason is recorded |

A sending service's acceptance is not proof of delivery, and delivery is not a human
response. Whether a call was answered is recorded as a detail of its attempt.
Human acknowledgement is the incident's claim, never a delivery state.

Show sends, retries, failures, uncertain outcomes, repeated SOS attempts, and
downstream receipt separately.

All contact method types share persistence, scheduling, bounds, and retry ownership. Contact method type
adapters perform one delivery attempt; they do not run their own
background retry loops. Use stable request identities for sending services supporting
idempotency. Do not promise exactly-once external delivery when an interrupted
request leaves its outcome unknown.

Delivery purposes and their UI labels are `subscribed_alert` (“Alert”),
`sos_escalation` (“SOS: contacting the on-call person”), `system_notice`
(“Notice from MMHotline”), and
`contact_method_test` (“Test”). Display outcomes from the state and
`next_attempt`: “Waiting to send”, “Sending”, the contact method type's accepted label
(“Accepted by the mail server”, “Accepted by the webhook”),
“Failed, will retry at {time}”, “Failed, no more retries”,
“Unknown (no answer from the sending service), will retry at {time}”,
“Unknown (no answer from the sending service), no more retries”, or “Cancelled”.
The displayed outcomes do not add a new state machine.
The SOS incident page lists the incident's deliveries under “Notifications
sent”. The incident-specific delivery list is built.

Besides email and outgoing webhooks, automated voice calls, Slack, Telegram,
and Pushover add contact method type labels. A phone call's label comes from
the call result:

| Contact method type | Labels |
| --- | --- |
| Automated voice call | “Calling”, “Answered (by a person or voicemail)”, “No answer”, “Busy”, “Call failed”, “Waiting: another call to this number is still going” |
| Slack message | “Posted to Slack” |
| Telegram message | “Sent to Telegram” |
| Pushover notification | “Accepted by Pushover” |

### Reading delivery outcomes

The Contact methods page lists the newest five of the 50 loaded notifications.
“Older notifications” folds the remaining records without another query.
The incident page, Contact methods page, and Delivery outcome page share the
recipient phrase, such as “Pushover notification to Priya Natarajan”.
The signed-in person reads “you”. Summaries name each recipient without
showing personal destinations, so a reader can distinguish each person's notifications.
Shared contact methods keep the shared destination. A compact list keeps outcomes and reasons readable on a phone.
Five visible records keep history from burying the contact method controls.

Suppressed and cancelled notifications say “Not sent” with a plain reason.
MMHotline stores a short reason code, such as `sos_escalation step changed`,
and pages translate the code when they render: “MMHotline moved on to the next
person”. The incident page and the Contact methods page use one translation.

Delivery outcome shows the result and reason first, then a separate “Message as sent” panel.
The panel preserves the recorded message, including whitespace and any UTC timestamp within the message.
Queued and retry times use the team's time zone. “Delivery attempts” folds attempt times, outcomes, and diagnostics.
Delivery outcome links back to the incident when the delivery belongs to an incident.
The acceptance explanation appears only after acceptance or delivery, and stays hidden while a phone call is calling.
Acceptance does not prove that a person read the notification or claimed the incident.

## Test and manage a contact method

Every saved contact method is ready for alerts immediately. Saving a new contact
method or changing its destination queues exactly one `contact_method_test`
delivery in the save transaction. The test names who added or changed the contact
method and when, and says it tests the contact method, not an incident.
Changing only subscriptions or availability preferences queues no test. Saving
unchanged settings preserves the revision and pending deliveries.
Changing the destination cancels the messages still waiting for the old one,
including a combined message and the messages combined into it. Changing only
subscriptions or availability preferences keeps every waiting message,
including one waiting for a retry: the worker checks availability again before
sending each one. A message already queued for a severity the person then
unticks still goes. It was wanted when it was queued, and an extra message
costs less than a lost one. Every change still gives the contact method a new
revision, so two people saving it at once get a conflict.

Only the team's own people add addresses on its server, so verification protects
nobody. A test message catches a wrong address when a person looks at it.

Automatic save tests bypass queue capacity, so a backed-up
outbox cannot refuse setup. Phone call save tests follow the daily call-test limits below. Sending service delivery happens after the save and cannot
make the save fail. Tests follow the contact method's delivery availability rules.
The Send test button names its initiator and never creates an incident.
The Send test button permits
one test per contact method per minute, counting automatic tests too. Phone call
button presses also follow the daily call-test limits.

The contact methods page shows subscriptions, the last test result, “Send even
when I'm unavailable or the team is muted” (shared methods: “Send even when the
team is muted”), Send test, and Remove. Pushover and phone calls instead label the availability
option “Send even when I'm unavailable” and state that team mute always stops
the contact method. The test delivery's subject is “MMHotline test
message”. The card shows the latest test delivery's outcome and reason, or
“Not tested yet” when no test exists. Phone calls are never made during
team mute.

Your personal and shared contact methods appear before the folded “Add a contact method” forms.
The add forms open when no editable or shared contact method exists, or after a form error.
A failed or uncertain last test links to the delivery outcome and shows the failure reason.
Webhook signing secret rotation and bearer token controls are folded within the webhook panel.
The once-only signing secret remains visible immediately after creation.
Panels keep their own height so short contact methods have no stretched empty space.
Administrators can expand “Other people's contact methods”; each personal panel names its owner and remains read-only.
The separation keeps the signed-in person's controls ahead of contact methods the person cannot change.

Every contact method type has Send test.

For automated voice calls, the test call names who saved or requested the
call and when, then says “This is a test call from Hotline. Open M M
Hotline to check the call result.” The full message is said twice and the
call hangs up. A test never creates or claims an incident.

The Send test button permits at most 5 calls per contact method in the last
24 hours, and 20 across the installation. The automatic test made when a phone
number is added counts against the same limits. When a limit is reached, the
phone number is still added, no test call is made, and the page says “Test not
sent:” with the reason, for example that the installation's calls were tested
20 times today and to try again tomorrow. Without this limit, a member could
add and remove a number again and again, and the team's Twilio account would
call it each time.

[People](people.md#members-and-roles) defines contact method permissions.
Replacing or removing a contact method invalidates its pending work. A queued
message is never retargeted to a different address because an ID was reused.

## Acceptance evidence

Test every base/resolved pairing, multiple contact method type subscriptions, duplicate
subscriptions, unavailable recipients, team mute,
`send_while_unavailable_or_muted`, shared contact methods, newly saved
addresses, resolved-sos call suppression, sending service errors, retry identity, and
stale results after contact method/incident changes. Verify real sending service
acceptance separately from final receipt where the sending service exposes it.

For phone and chat contact method types, also cover automatic save tests, Send test limits,
refused countries, repeated and out-of-order sending service reports, call-failure
fallback, call pacing and 429 retries, and the ordinary-message throttle never delaying an SOS.
Cover buttons pressed by unlinked or other-workspace users, plus webhook
signatures and address rules. Fake sending services never count as evidence of real
delivery ([testing](../testing.md)).

For a monitoring tool, verify creating an alert source, showing its signing
secret once, adding a second signing secret, accepting both active secrets, revoking the
old secret, and disabling the alert source.
