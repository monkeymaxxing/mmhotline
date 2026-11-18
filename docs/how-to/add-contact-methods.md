# Add contact methods

**Who acts:** each person on the team, for their own contact methods, in
MMHotline's pages. An administrator also adds the team's shared contact
methods, and can add a person's contact method from the server.

**When to use this page:** to choose where your alerts reach you, to
change which alerts go where, or to send alerts to the team's own program.

**What this page does:** you add a contact method, check its test, and set
which alerts it gets.

**Terms:**

- A **contact method** is one place where alerts reach you, such as your
  Pushover app, your phone number, your email address, or a chat. Each
  one has a **contact method type**, such as `pushover_notification`.
- An alert's **severity** is SOS (wakes the person on call), Warning, or
  Info. Each contact method gets the severities you choose.
- An **outgoing webhook** sends alerts to a program as signed requests,
  such as to the team's ticket system. A program cannot claim an incident.

## Choose the route

Each kind needs its sending service connected first; the Contact methods
page says when one is missing.

| Contact method | Wakes you? | How to add it |
| --- | --- | --- |
| Pushover notification | Yes | [Connect Pushover](connect-pushover.md), from step 3 |
| Automated voice call | Yes | [Connect Twilio](connect-twilio.md), from step 3 |
| Email | No | [Connect a mail server](connect-a-mail-server.md), from step 3 |
| Slack message | No | [Connect Slack](connect-slack.md), from step 3 |
| Telegram message | No | [Connect Telegram](connect-telegram.md), from step 3 |
| Outgoing webhook, to a program | No | [Send alerts to a program](#send-alerts-to-a-program) |

Every person who can be on call needs a Pushover notification or a phone
call: a message in email or chat does not wake anyone.

## Change a contact method

On **Contact methods**, each of your contact methods has a card:

- **Alerts to send through the contact method:** tick SOS, Warning, and
  Info as you want them, and choose the card's save button. The add forms
  have the same boxes.
- **Send even when ...:** for Pushover and phone calls, **Send even when
  I'm unavailable**; team mute always stops these two. For email, chat,
  and webhooks, **Send even when I'm unavailable or the team is muted**, ticked
  at first. For a contact method shared with the team, which nobody is
  away from, **Send even when the team is muted**.
- **Send test** sends a test now; at most one a minute for each contact
  method. A test opens no incident. A Pushover test is a quiet
  notification: it proves Pushover took the message, not that the phone
  wakes you. A Pushover card also says **Emergency test: not yet
  acknowledged** until you run
  `sudo mmhotline incidents send-test-sos --sign-in-email-address EMAIL`
  with your sign-in email address (in a team of one person, without the
  flag) and acknowledge the alert. That works
  for a new team member too, whether or not they are on call.
- **Last test** shows how the latest test went; choose it to read the
  delivery outcome. **Remove** removes the contact method.

Expand **Add a contact method** for the forms to add one, and for what an
administrator must connect first. A new contact method gets alerts as
soon as it is saved. Slack and Telegram are connected only by the person
themselves, from Slack or Telegram; other kinds an administrator can add
for a person from the server.

**Recent notifications**, below the cards, lists the five newest
notifications, newest first, naming each person and contact method type; **Older
notifications** shows up to 50. A notification that was skipped says
**Not sent** and why.

Only the owner changes a personal contact method. Everyone sees the
contact methods shared with the team; only administrators change them.
Administrators also see **Other people's contact methods**.

From the server, `sudo mmhotline contact-methods list` lists every contact
method, and `sudo mmhotline contact-methods send-test --contact-method-id ID`
and `sudo mmhotline contact-methods remove --contact-method-id ID` act on
one. The [command reference](../reference/mmhotline-command.md#mmhotline-contact-methods-add) lists
every flag.

## Send Pushover alerts to one device only

By default Pushover sends each alert to all of your devices. To use only
one, put its name in **Pushover device name** on the add form or on the
card. A name has up to 25 letters, digits, `_` and `-`. MMHotline checks it
with Pushover and refuses a name your Pushover account does not list.
Leave it empty to use all devices again. Your acknowledgement still comes
from your own Pushover user, so it still claims the incident. The steps are
in [Connect Pushover](connect-pushover.md#send-to-one-device-only).

## Send alerts to a program

An outgoing webhook sends each alert as a JSON `POST` request from the
MMHotline server to a program that the team runs, such as a ticket system or a chat bridge. The program
must be reachable at a public `https://` address.

1. **Add the webhook.** On **Contact methods**, under
   **Add outgoing webhook contact method**, enter the program's address,
   such as `https://example.com/alerts-from-mmhotline`. If the program
   requires one, enter **Webhook bearer token (optional)**; MMHotline sends
   it as `Authorization: Bearer ...`. Tick the alerts to send. For the
   team's own program, an administrator ticks **Shared with the team**, so
   it does not depend on one person; a team mute then stops it unless
   **Send even when the team is muted** is ticked. Then choose
   **Add outgoing webhook**.

   **Check:** the page shows the signing secret once:

   ```text
   Save your webhook signing secret now
   The webhook signing secret is shown only once.
   Signing secret ID: 22ba98caeebdfe3cdfc424bd63ae38e9
   Webhook signing secret (hex): THE 64 HEXADECIMAL CHARACTERS
   ```

2. **Give the signing secret to the program** through a private route,
   such as the program's secret store. The program checks each request's
   `Mmhotline-Signature` header with it, so it knows the request came from
   MMHotline unchanged: it decodes the secret into 32 bytes and computes
   HMAC-SHA256 over the timestamp, a dot, and the exact request body.
   [Outgoing webhooks](../reference/outgoing-webhooks.md) describes the
   request, its fields, and how to check the signature.

3. **Check the test request.** Saving sent the program a test, such as:

   ```json
   {"mmhotline_delivery_id":"859aa2fcb7d1a1721a69d04977fb4ab1","delivery_purpose":"contact_method_test","title":"MMHotline test message","message":"Barbara Example added this contact method at 2026-11-15T05:43:53Z.\n\nThis is a test of this contact method, not an incident."}
   ```

   **Check:** the card's **Last test** says **Accepted by the webhook**
   with the program's HTTP code, and `sudo mmhotline contact-methods list`
   says `Last test message: accepted (queued at 2026-11-15T05:43:53Z): HTTP 200.`
   Accepted means the program answered with a 2xx code; check that it
   also handled the alert. Once the program checks signatures, press
   **Send test** to try a signed request again. Links in the requests start
   with the settings file's `web_url`.

**How MMHotline sends:** it follows no redirects and waits at most 10
seconds for an answer. A request sent again carries the same
`Mmhotline-Delivery-Id` header, so the program can ignore a repeat.

**Rotate the signing secret:** under **Webhook signing secrets and bearer
token** on the card, add a second signing secret, make the program accept
both, then revoke the old one. The older secret signs every request until
you revoke it, and one secret always remains. Saving an empty bearer token
removes it.

From the server, `sudo mmhotline contact-methods add` with
`--contact-method-type outgoing_webhook` adds one. It reads the bearer
token from `--outgoing-webhook-bearer-token-file`, a private file holding
only the token, without surrounding spaces, and writes the new signing
secret to `--outgoing-webhook-signing-secret-file`, in place of printing
it: a file with mode 0600, in a folder that must exist, replacing any file
already there. Use this route when the secret must go into a file, so it
never shows on a screen.

## If something is different

- **Adding a webhook says** `the webhook URL must start with https://`, or
  `the webhook URL must be a public internet address`: MMHotline sends only
  to public `https://` addresses, never to private or loopback ones. Reason: an alert could otherwise reach
  programs on the server's private network.
- **The Contact methods page has no form for a kind,** or asks you to ask
  an administrator: that sending service is not connected yet. An
  administrator connects it, as
  [Connect a sending service](connect-a-sending-service.md) says.
- **A test did not go out:** tests follow the same rules as alerts, so
  check the person's unavailability, team mute, and the card's
  **Send even when ...** box. Sending the test says only that it is
  queued; the last test result then says why, such as
  `Last test message: suppressed (queued at 2026-11-15T07:10:02Z): team mute.`
- **A test was accepted, but nothing arrived:** check the destination's
  spam folder, quarantine, or notification settings.
