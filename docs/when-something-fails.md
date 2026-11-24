# When something fails

This page lists what you may see when something goes wrong, what it means,
and what to do. Find the place where you saw the problem, then the message.
Each entry quotes the message as MMHotline, or the service it talks to,
prints it; `...` stands for the part that changes. The how-to page that each
entry links to has the full steps.

Where a message names a command, run it on the MMHotline server, here
`util01`. The examples use the team Operations at `https://ops.example.com`.

- [The browser pages](#the-browser-pages)
- [`settings check` or `settings apply`](#settings-check-or-settings-apply)
- [`mmhotline install`](#mmhotline-install)
- [Alerts that didn't arrive or didn't wake anyone](#alerts-that-didnt-arrive-or-didnt-wake-anyone)
- [Sending services and contact methods](#sending-services-and-contact-methods)
- [The backup](#the-backup)
- [The down alarm and the status page](#the-down-alarm-and-the-status-page)
- [The triage agent](#the-triage-agent)

## The browser pages

### "Not a team member"

The page says `The sign-in email address ADDRESS is not a member of Operations.`
Cloudflare Access let the person in, but the address is not in the settings
file's `people`. MMHotline's log says
`Sign-in refused: ADDRESS is not a member of the team`.

Sign in with the address listed in `people`. Or an administrator adds the
shown address to `people` and applies the file, as
[Set up people and the on-call schedule](how-to/set-up-people-and-the-on-call-schedule.md)
says. The two addresses must match exactly, apart from letter case.

### "Sign in through Cloudflare Access"

The page says `Open MMHotline through Cloudflare Access to sign in.` The
request reached MMHotline without a Cloudflare Access sign-in that
MMHotline accepts.

Open the host name, such as `https://ops.example.com`, not the server's own
address. If you did, check that `team_domain` and `audience_tag` in the
settings file match Cloudflare's values exactly, as
[Put MMHotline behind Cloudflare Access](how-to/put-mmhotline-behind-cloudflare-access.md)
says.

### "Cannot check sign-ins right now"

The page says `MMHotline cannot check Cloudflare Access sign-ins right now.`
MMHotline could not get your team domain's keys from Cloudflare, usually
during a Cloudflare outage.

Sign in with a sign-in link until Cloudflare answers again, as
[Sign in during a Cloudflare outage](how-to/sign-in-during-a-cloudflare-outage.md)
says.

### "Sign-in link unavailable"

The page says `This sign-in link was already used or has expired.` A
sign-in link works once, within 10 minutes.

Make a new link and open it at once, as
[Sign in during a Cloudflare outage](how-to/sign-in-during-a-cloudflare-outage.md)
says.

### A Cloudflare error page instead of MMHotline

The host name shows Cloudflare's own error page, not the sign-in page or
MMHotline. The Cloudflare tunnel does not reach MMHotline.

Check that the tunnel's connector is connected, that its route's service
URL is `http://127.0.0.1:8480`, and that `systemctl is-active mmhotline`
prints `active`. See
[Put MMHotline behind Cloudflare Access](how-to/put-mmhotline-behind-cloudflare-access.md).

### "The page changed"

Someone changed the same thing while you had the page open, and MMHotline
did not save your action. Reason: an action made on an older page must
never overwrite another person's claim or change. The page gives the
reason, such as:

- `The incident changed before your action was saved. Reload the page and try again.`
- `The On call page changed while you had it open. Check the page, then choose again.`

Reload the page, read what changed, and choose again. See
[Handle an incident](how-to/handle-an-incident.md).

A failed action on an incident offers **Back to the incident**, and a
failed **Stop** of a triage session offers **Back to the triage session**.
A failed **Raise an SOS by hand** form keeps your title and message, so you
correct the form and send it again.

### "You cannot make this change"

Your role does not allow the action, or the form was open too long. The
page may say
`Your sign-in does not allow this action, or the form has expired. Open the page again before trying the action.`

Open the page again and repeat the action. If it fails again, ask an
administrator. Some pages say so directly, such as
`Only an administrator can mute the team.` on the On call page.

### "Too many requests"

The page ends with `Try again in 15 minutes.` You reached a limit, such as
the number of contact methods a person may add, or 60 custom schedule
previews in an hour. The reason comes first.

Wait, or remove what you no longer need, such as an unused contact method.

### "Page not found"

The page says `MMHotline could not find the page or record you asked for.`
The link is wrong, or the record was removed. Choose
**Back to the dashboard**.

### "Parts of the settings file did not apply"

Administrators see this at the top of the dashboard. Each listed part kept
its earlier setting, and the line says why. The other parts applied.

Fix the cause in the settings file and run `sudo mmhotline settings apply`
again. See [a part did not apply](#part-did-not-apply-) below.

### "The settings file has no fleet_view_command, so MMHotline shows no servers."

The dashboard's **Servers** part has no command to read the monitoring
tool's servers from. **Details** gives the technical cause when a command is
set but fails.

Add `fleet_view_command`, as
[Connect a tool that sends alerts](how-to/connect-a-tool-that-sends-alerts.md#show-the-tools-servers-on-the-dashboard)
says.

## `settings check` or `settings apply`

### "unknown key" or "required"

```text
on_call_schedule.escalate_if_unclaimed_after_minute: unknown key
on_call_schedule.escalate_if_unclaimed_after_minutes: required
The settings file has problems, so nothing applied and the earlier settings stay. Fix the settings file problems, then apply the settings file again.
```

Each line starts with the key path. MMHotline refuses an unknown key, so a
misspelled key never passes silently, and every key inside a section you
use is required unless the [settings file reference](reference/settings-file.md)
says it is optional. With any problem, nothing applies.

Fix each line, then run `sudo mmhotline settings check` and
`sudo mmhotline settings apply`. See
[Change the settings](how-to/change-the-settings.md#the-check-lists-problems).

### "PART: did not apply: ..."

```text
sending_services.pushover: did not apply: Checked with Pushover: Pushover answered HTTP 400: application token is invalid. The earlier setting, applied at 2026-11-15T03:54:04Z, stays.
1 part did not apply and keeps its earlier setting: sending_services.pushover.
```

That part kept its earlier setting; the other parts applied. The reason
follows `did not apply:`. The end of the line says what stays:

- `The earlier setting, applied at ..., stays.`: the old setup keeps
  working.
- `It stays not set up, as before.`: the part was not set up, and still is
  not.
- `No value has ever applied for the settings file part, so the part is not set.`

A sending service that was never set up can also end with
`The earlier setting, applied at ..., stays.`, where the time is when the
apply first recorded it as not set up. The earlier setting is then "not set
up".

Fix the cause and apply again. The entries below cover the common causes.
See [Change the settings](how-to/change-the-settings.md#a-part-did-not-apply).

### "can be read by every user on the server"

```text
/etc/mmhotline/mmhotline_settings.yml can be read by every user on the server (mode 0644), and it holds the team's secrets. Fix the settings file owner and mode with: sudo chown root:mmhotline /etc/mmhotline/mmhotline_settings.yml; sudo chmod 0640 /etc/mmhotline/mmhotline_settings.yml
```

The settings file holds secrets, so MMHotline refuses it with any other
owner or mode. Run the two commands the message prints, then apply again.
`settings check` skips this test, so that it can run on a laptop.

### "mmhotline serve is not running at command socket /run/mmhotline/command.sock"

MMHotline's service is not running, so `settings apply` and `settings show`
have nothing to talk to. Start it with `sudo systemctl start mmhotline`. If
it stops again, read why with
`sudo journalctl -u mmhotline --no-pager -n 30`.

### "people: did not apply: ... owns 1 claimed incident"

```text
people: did not apply: Dana Example owns 1 claimed incident. Resolve the claimed incidents or have someone take over the claimed incidents before removing the person.
```

Removing the person would leave the incident with nobody. The same happens
while the person has taken on call. The earlier people stay.

Resolve the incident or take it over, as
[Handle an incident](how-to/handle-an-incident.md#take-over-a-claimed-incident)
says, or give on call back, then apply again.

### "cannot remove the ... setup: 1 ... contact method uses the sending service setup"

Such as
`cannot remove the Twilio setup: 1 automated voice call contact method uses the sending service setup`.
You removed a sending service that contact methods still use, so the setup
stays.

Remove those contact methods on the Contact methods page, or with
`sudo mmhotline contact-methods remove --contact-method-id ID`, then apply
again. See [Connect a sending service](how-to/connect-a-sending-service.md#change-or-remove-a-service).

### "a triage agent run was active, so the new agent setup was not tested"

The full line ends
`Apply the settings file again after the triage agent run ends`. MMHotline
tests a new agent setup with one prompt, and does not interrupt a running
session for it. Apply again after the run ends.

### "MMHotline gave no successful answer on its command socket ... of the restart"

You changed a key that applies only at a restart: `web_url`,
`web_listen_address`, `alert_intake_listen_address`, or `cloudflare_access`.
`settings apply` restarted MMHotline, which did not answer within one
minute, often because it cannot use a new listen address.

Read why with `sudo journalctl -u mmhotline -n 50`, fix the file, and apply
again. See [Change the settings](how-to/change-the-settings.md#mmhotline-restarted).

### The apply waits and prints nothing

Another apply is running; one apply runs at a time, and an outside test can
take minutes. Wait. Ctrl-C stops only the waiting; run
`sudo mmhotline settings show` to see how the other apply ended.

### Warning "The settings file has problems; MMHotline uses its earlier settings"

Administrators get this Warning when MMHotline starts and the settings
file has problems or the wrong owner or mode. MMHotline keeps the settings
it last applied, so alerts keep flowing. When MMHotline cannot listen with
the file's `web_url`, listen addresses, or `cloudflare_access`, such as on
an address the server does not have, the Warning is "MMHotline uses its
earlier server settings" instead.

Run `sudo mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml`,
fix what it lists, and apply. On the very first start, with nothing applied
yet, MMHotline refuses to start and prints the problems.

## `mmhotline install`

### "install requires root; run sudo mmhotline install"

Or `install requires Linux; ...`, or
`install requires running systemd at /run/systemd/system; ...`. Install
checks these before it changes anything.

Run it with `sudo` on the Linux server, which must have booted with
systemd. See [Install MMHotline](how-to/install.md).

### "install requires the settings file /etc/mmhotline/mmhotline_settings.yml"

Install needs the settings file first. It made no change and exits 4.

Write the file as [Install MMHotline](how-to/install.md) says, then run
the install again.

### The install lists settings problems

Install checks the settings file as `settings check` does, and also its
owner and mode. It changed nothing. Fix each key it names, and run the
install again. See [`settings check` or `settings apply`](#settings-check-or-settings-apply).

### "systemctl restart mmhotline failed"

MMHotline did not start. Install prints the service's last log lines; the
last ones say why, such as:

```text
MMHotline cannot listen on alert_intake_listen_address 10.20.0.9:8484: listen tcp 10.20.0.9:8484: bind: cannot assign requested address
```

Here the address is not one of the server's own; `ip -brief addr` lists
them. Fix the cause, then run the install again. A message that starts
`mmhotline command socket did not answer within` means the same: read the
log lines it prints.

### "Backup: /etc/mmhotline/backup.env exists but litestream is missing"

You wrote the backup settings, but Litestream is not installed. Install
Litestream 0.5.18, as
[Back up and restore the database](how-to/back-up-and-restore-the-database.md#set-up)
says, step 1, and run the install again.

### "Backup: not started: this server's database is new, and the bucket ... already holds a backup"

Install created a new, empty database, and the bucket holds a backup of
another server. Starting the backup would save the empty database as the
newest backup. Install stops on purpose and leaves the backup stopped.

Restore the backup, or choose an empty folder in
`MMHOTLINE_BACKUP_REPLICA_URL`, as
[Back up and restore the database](how-to/back-up-and-restore-the-database.md#restore-on-a-new-server)
says. A message with `MMHotline could not list the bucket` means install
could not check the bucket; fix `backup.env`, then run the install again.

### `sha256sum` prints "FAILED"

With `sha256sum: WARNING: 1 computed checksum did NOT match`. The download
is damaged or is not the release. Delete it and download again; never run
it. See [Install MMHotline](how-to/install.md).

### "mmhotline.db has database format ..., and this program knows formats up to ..."

The journal says this when MMHotline does not start after you installed an
older release. A newer release upgraded the database, and an older program
cannot read it.

Install that newer release again, or restore the backup made before the
upgrade, as [Upgrade MMHotline](how-to/upgrade.md) says.

## Alerts that didn't arrive or didn't wake anyone

### The tool gets "401 source authentication failed"

The secret is wrong, the alert source is disabled, or the tool's clock is
more than 5 minutes away from the MMHotline server's.

Compare the secret with the settings file; check that
`sudo mmhotline alert-sources list` says the source is active; check both
clocks. The Alert sources page shows
**Rejected requests in the last 24 hours:** with the reasons, such as
`Wrong signature` or `The sender's clock is more than 5 minutes off`. See
[Connect a tool that sends alerts](how-to/connect-a-tool-that-sends-alerts.md).

### The tool gets "400 sender_alert_id already used for a different alert"

The tool reused an alert ID for a new alert. Give each new alert its own
`sender_alert_id`, and keep the same one only when retrying the same
alert. An answer of `200` with `"already_received":true` is not a failure:
MMHotline counted a retried alert once.

### The tool gets "429 admission limit reached; retry later"

The tool sent too many alerts in a short time. The Alert sources page
counts them as `Too many alerts`. The tool retries; check why it sends so
many.

### The tool gets no answer or a refused connection

The tool cannot reach the alert intake address. Check the network path and
`alert_intake_listen_address`. A request sent to the web address, such as
`https://ops.example.com`, is refused by Cloudflare Access.

### "MMHotline could not wake Carlos Example for the SOS incident ..."

Administrators get this Warning, and the incident page says
`MMHotline could not wake Carlos Example: ...`:

```text
MMHotline could not wake Carlos Example for the SOS incident “Backup failed: test SOS”. Nothing set up can wake them: a person needs an automated voice call or a Pushover notification that receives SOS alerts. Until they add one on their Contact methods page, MMHotline skips them when it wakes people for an SOS.
```

Only those two contact methods wake a sleeping person. MMHotline moves on
to the next person at once. The person adds one, as
[Add contact methods](how-to/add-contact-methods.md) says.

### "The team is muted until ...: ... an SOS wakes nobody until then."

An administrator muted the team, so MMHotline sends no Pushover
notifications or automated voice calls. Other alerts still go out. The mute
ends by itself; an administrator can end it sooner, as
[Mute the team](how-to/mute-the-team.md) says.

### A notification says "Suppressed"

The notification's line says why, such as `the person is unavailable` or
`the team is muted`. Tests follow the same rules, and a contact method's
last test may say
`Last test message: suppressed (queued at 2026-11-15T07:10:02Z): team mute.`

The person ends their unavailability, as
[Be away or take over on call](how-to/be-away-or-take-over-on-call.md)
says, or an administrator ends the mute. A contact method whose
**Send even when ...** box is ticked is sent anyway.

### The SOS arrived, but the phone stayed silent

The service delivered it, but the phone did not ring: a notification that
arrives silently wakes nobody. Set up Pushover's Critical Alerts, or the
phone's Do Not Disturb exception for the calling number, and test both
muted and with Do Not Disturb on, as
[Connect Pushover](how-to/connect-pushover.md#steps) says, step 5, and
[Connect Twilio](how-to/connect-twilio.md) says.

### A test was accepted, but nothing arrived

Check the destination's spam folder, quarantine, or notification settings.
A sending service's acceptance does not prove that a person saw or heard
the message.

### "“Monitoring” hasn't checked in with MMHotline for 5 minutes"

The full title ends `it may have stopped, or its server may be down`. The
tool that sends alerts stopped checking in, so MMHotline cannot know
whether it still watches anything. A Warning comes first, then an SOS.

Check the tool and its server. The SOS resolves by itself when check-ins
return, unless someone claimed it. For planned work, declare planned
downtime first, as
[Declare planned downtime](how-to/declare-planned-downtime.md) says. See
[How the silence alerts work](how-to/connect-a-tool-that-sends-alerts.md#how-the-silence-alerts-work).

### The tool's alerts still arrive during planned downtime

Planned downtime stops only the alerts about missing check-ins. To stop
phones ringing, an administrator mutes the team, as
[Mute the team](how-to/mute-the-team.md) says.

### "MMHotline cannot work out who is on call: the custom schedule code failed"

An hourly run of the custom schedule code failed, so the stored shifts stay
in use. When they run out, On call says
`Your custom schedule code has no answer for this time, so the backup is contacted first, then everyone available.`

Fix the code and apply it, as
[Write a custom schedule](how-to/write-a-custom-schedule.md) says. A
working run sends
`The custom schedule code works again: MMHotline knows who is on call`.

## Sending services and contact methods

`settings check` and `settings apply` print a problem with a sending
service after its key path, such as `sending_services.twilio.account_sid`.
A sending service that fails its outside test keeps its earlier setup, as
[a part did not apply](#part-did-not-apply-) says. Each service's how-to
page, from [Connect a sending service](how-to/connect-a-sending-service.md),
covers the common messages; this section also lists the rest.

### "Pushover answered HTTP 400: application token is invalid"

At apply: Pushover does not know the application token. The words after `HTTP 400:` are Pushover's own; MMHotline relays them. Copy the whole
token again, keeping upper and lower case, from the application's page at
Pushover. See [Connect Pushover](how-to/connect-pushover.md).

### "Pushover answered HTTP 400: user identifier is invalid"

In a test or delivery result: the words after `HTTP 400:` are Pushover's own, which MMHotline relays. The person's user key is wrong, or is the
application's token. Remove the contact method and add it again with the
person's own user key.

### "Twilio refused the API key; check the account SID, the API key SID, and the secret"

The three must belong together: an active Standard API key of the Twilio
account that owns the calling number. See
[Connect Twilio](how-to/connect-twilio.md).

### "the Twilio calling number must be an incoming phone number owned by the configured Twilio account and able to receive voice calls"

Choose a voice number listed in the Twilio account that `account_sid`
names.

### "the mail server does not offer STARTTLS, which smtp_connection_security: starttls requires"

The server, its port, or the security setting does not match. Ask the mail
service which one it uses. See
[Connect a mail server](how-to/connect-a-mail-server.md).

### "The Slack connection code expired or was already used."

The full reply is
`The Slack connection code expired or was already used. Choose Connect Slack under Contact methods for a new code.`
A code works once, within 15 minutes. Make a new one and send it at once.
See [Connect Slack](how-to/connect-slack.md).

### "This MMHotline link has expired or was already used."

Telegram's bot says this for a connection link older than 15 minutes, or
used before. Make a new link under Contact methods and open it at once. See
[Connect Telegram](how-to/connect-telegram.md).

### The Slack code or Telegram Start gets no answer

For Slack, check that the code went to the app's direct messages, or, for a
shared channel, with a mention of the app in a channel the app was invited
to. For Telegram, check that no other program reads the bot's updates and
no webhook is set for the bot.

The service log, `sudo journalctl -u mmhotline --no-pager -n 30`, shows a
failing Slack Socket Mode connection, or `Telegram getUpdates` failures with
the last good read. MMHotline reconnects by itself and logs when it works
again.

### "No answer from ..." or "no readable answer from ..."

MMHotline does not know whether the message arrived. Check the phone or the
conversation before you send another test; a retry may bring a second copy.
MMHotline does not resend a Pushover emergency at once, to avoid two
emergencies; the next SOS repeat can send a new one.

### "... asked MMHotline to send more slowly (HTTP 429)"

The service limits how fast MMHotline may send. MMHotline retries by
itself; the delivery list shows the next attempt.

### "Pushover acknowledgement came from another user; no incidents were claimed. Add a personal Pushover user key."

The contact method uses a Pushover delivery group's key, so MMHotline
cannot tell who acknowledged. Replace it with the person's own user key.

### "the webhook URL must start with https://"

Or `the webhook URL must be a public internet address`. MMHotline sends
webhooks only to public `https://` addresses. Reason: an alert could
otherwise reach programs on the server's private network. See
[Add contact methods](how-to/add-contact-methods.md#send-alerts-to-a-program).

### The Contact methods page has no form for a kind

That sending service is not set up yet; members may see
**Ask an administrator to set up Telegram**, or the same for another
service. An administrator connects it, as
[Connect a sending service](how-to/connect-a-sending-service.md) says.

### Other Pushover messages

| Message | What it means and what to do |
| --- | --- |
| `sending_services.pushover.application_token: must be one line of exactly 30 letters and digits, ...` | Copy the application's whole API token, keeping case. |
| `Pushover user key must contain exactly 30 letters and digits` | Copy the person's whole user key, keeping case. |
| `Pushover did not report a monthly message limit` | Pushover's answer was incomplete. Check the application at Pushover, and apply again when Pushover works. |
| `Pushover answered HTTP 400: ...` | Read Pushover's reason. Check the application token, the personal user key, and whether the Pushover app is signed in on an active device. |
| `Pushover answered HTTP 429: ...` | Pushover reports a quota or rate limit. Check the application's usage at Pushover; MMHotline retries. |
| `could not connect to Pushover`, or HTTP 5xx | Check the server's outgoing HTTPS and Pushover's status. MMHotline retries. |
| `Pushover accepted the emergency message without a usable receipt` | MMHotline cannot track or cancel that emergency. Check and acknowledge it on the phone. |
| `Not sent: the Pushover message or MMHotline link exceeds Pushover's limits` | Check the length of `web_url`; the incident link must fit Pushover's limit. |
| `you already added this Pushover notification contact method` | Use the existing card. |
| `each person can add at most 3 Pushover notification contact methods ...` | Remove an unused one first. |

An emergency receipt is Pushover's reference for one emergency message;
without it, MMHotline cannot check or cancel the message.

### Other Twilio messages

| Message | What it means and what to do |
| --- | --- |
| `Twilio account SID must be AC followed by 32 hexadecimal digits` | Copy the account SID of the account that owns the calling number. |
| `Twilio API key SID must be SK followed by 32 hexadecimal digits` | Copy the API key's SID. |
| `Twilio calling number must be in E.164 format` | Write `+`, the country code, and digits only. |
| `Extra phone numbers on the contact card must be comma-separated E.164 numbers` | Fix the extra numbers, or write `[]` for none. |
| `MMHotline could not reach Twilio for the calling number check` | Check the server's outgoing HTTPS. |
| `Twilio answered HTTP ... to the calling number check`, or `Twilio returned unreadable calling numbers` | Check Twilio's status and the account's numbers, then apply again. |
| `enter the phone number with its country code, ...` | Add `+` and the country code. |
| `a number in the US or Canada has 10 digits after +1` | Check the area code and the whole number. |
| `enter a person's phone number, not a toll-free or premium number` | Use the person's own phone number. |
| `MMHotline can't call phone numbers in ... yet. ...` | The country is not one MMHotline calls. Use a [Pushover notification](how-to/connect-pushover.md). |
| `you already added ...`, or `you can add at most 3 phone numbers. Remove an existing phone number first` | Use the existing card, or remove an unused number. |
| `Waiting: another call to this number is still going` | MMHotline starts no second call to a number while one rings. Let it finish. |
| `Twilio answered HTTP 429` | Twilio refused to start a call because requests came too fast. MMHotline tries again after a second. |
| `Twilio refused MMHotline's credentials (HTTP ...)` | The key was revoked or is wrong. Replace it in `sending_services.twilio` and apply. |
| `Refused by Twilio: the number cannot be reached (error 21211)` | Check the person's number, and that the Twilio account may call it. |
| `No answer from Twilio`, or `Twilio accepted the request without an ID` | The outcome is unknown. Check the phone before another test. |
| `the automated voice call contact method was tested ... times today. Try again tomorrow` | The limit of 5 test calls for one contact method in 24 hours. |
| `this installation's automated voice call contact methods were tested ... times today. Try again tomorrow` | The limit of 20 test calls for the whole installation in 24 hours. |

A call that Twilio accepted, or a voicemail result, does not prove the
phone rings through Do Not Disturb; test that on the phone.

### Other mail server messages

| Message | What it means and what to do |
| --- | --- |
| `smtp_server must be a host name and port` | Write the host and port, such as `smtp.example.com:587`. |
| `smtp_connection_security must be starttls, implicit_tls, or unencrypted` | Write one of the three exactly. |
| `email_from_address must be one plain ASCII email address, ...` | Write the address alone, without a name such as `MMHotline <...>`. |
| `smtp_user_name and smtp_password are required together, or both must be empty` | Write both, or leave both empty for a relay that needs no sign-in. |
| `SMTP credentials must be one line of UTF-8 text without control characters` | Remove line breaks from the user name or password. |
| `SMTP setup check: ...` | Read the reason that follows: the connection, certificate, sign-in, or the server's reply. |
| `the mail server offers no supported authentication method; MMHotline uses PLAIN or LOGIN` | Use a relay that offers PLAIN or LOGIN. |
| `enter one plain ASCII email address` | When adding an address: enter it alone, without a name. |
| `SMTP sender: ...`, or `SMTP recipient: ...` | The mail server refused the sender or the recipient. Check its reason. |
| `mail server acceptance reply: ...` | The outcome may be unknown. Check the mailbox before another test. |

### Other Slack messages

| Message | What it means and what to do |
| --- | --- |
| `Slack requires a bot token starting xoxb- and an app-level token starting xapp-` | At apply: copy each token into its own field. |
| `sending_services.slack.bot_token: must be the Bot User OAuth Token, which starts with xoxb-` | The two tokens are swapped or copied from the wrong place. The app-level token starts with `xapp-`. |
| `Checked with Slack: ...`, then a refusal | Read Slack's reason. Check the installed app, both tokens, and `connections:write`. |
| `Slack's WebSocket did not answer with hello`, or `could not open the Slack WebSocket` | Check outgoing WebSocket access and the app's Socket Mode setting. |
| `Slack did not return the workspace, bot user, and app identity` | Check both tokens, and that the app was made from MMHotline's manifest. |
| `the Slack app-level token belongs to a different app` | Both tokens must come from the same MMHotline app. |
| `Send a personal connection code in a direct message to the MMHotline app. ...` | The code went to the wrong conversation, and is still valid. Send it again as the reply says. |
| `Refused by Slack: ...` | Read Slack's error code. Check that the app is installed, invited to the channel, and may post there. |
| `Slack had a temporary problem: ...` | MMHotline retries; the delivery list shows the next attempt. |
| `Slack answered HTTP ...` | The outcome may be unknown. Check the conversation before another test. |

### Other Telegram messages

| Message | What it means and what to do |
| --- | --- |
| `sending_services.telegram.bot_token: must be the token from BotFather: ...` | Copy BotFather's whole token, not the bot's username. |
| `Checked with Telegram: Telegram answered HTTP ...` | Read Telegram's reason. Replace a revoked token with the current one. |
| `Telegram getMe did not return a bot ID and username`, or `Telegram getMe returned a different bot ID from the token` | Check that the token belongs to the bot you mean, and apply again. |
| `This MMHotline link is for a private chat with the bot. ...` | Open the personal link in the private chat with the bot, and press Start. |
| `This MMHotline link adds the bot to a group. Open the link again and choose a group.` | Use the group link to choose a group. |
| `MMHotline could not connect this chat: the person who made the link no longer has permission to connect the chat.` | A current member makes a new personal link; an administrator makes a group link. |
| `MMHotline could not connect this chat: this installation has reached its limit of contact methods.` | Ask an administrator to remove unused contact methods. |
| `MMHotline could not connect this chat. Try again in a minute.` | Try again. If it keeps failing, an administrator reads the service log. |
| `MMHotline does not know your Telegram account. Connect the account under Contact methods, or claim the incident in MMHotline` | Connect your personal Telegram account under Contact methods before you use an incident button, or claim the incident in MMHotline. |
| `MMHotline cannot use this button. Open MMHotline to see the incident.` | Open the incident link instead. The button may belong to an unrecorded duplicate message. |
| `MMHotline could not apply the incident action. Try again, or open the incident in MMHotline.` | Check the incident in MMHotline, and act there if needed. |
| `Refused by Telegram (HTTP ...): ...` | Read Telegram's reason, such as a person who blocked the bot. |
| HTTP 5xx from Telegram | Check outgoing HTTPS and Telegram's status. A retry may bring a second copy. |

## The backup

`sudo mmhotline backup check-uploads` exits 0 when the backup keeps up, 1
when it has stopped, and 3 when it can't tell. See
[Check the backup](how-to/back-up-and-restore-the-database.md#check-the-backup).

### "MMHotline's backup has stopped: ..."

Exit 1. Litestream did not capture a change within 30 seconds. The
`mmhotline-backup` service may be stopped. Read why with
`sudo journalctl -u mmhotline-backup`, fix it, and run
`sudo systemctl restart mmhotline-backup`. A stopped `mmhotline-backup.service` fails the check once MMHotline writes a change.

### "MMHotline's backup is missing changes captured ... minutes ago"

Exit 1. Litestream captured changes, but could not upload them to the
bucket for more than 5 minutes, such as after a revoked key. Litestream
keeps running and only logs the error. Read
`sudo journalctl -u mmhotline-backup`, fix the key or bucket in
`/etc/mmhotline/backup.env`, and run
`sudo systemctl restart mmhotline-backup`.

### "Cannot list MMHotline's backup: ..."

Exit 3. The bucket can't be listed. Check the key and
`MMHOTLINE_BACKUP_REPLICA_URL` in `/etc/mmhotline/backup.env`, then run
`sudo systemctl restart mmhotline-backup`.

### "Litestream has captured no change of the database, so the backup has not started"

Exit 3. The backup service has not run yet. Check it with
`sudo systemctl status mmhotline-backup`, and run `sudo mmhotline install`
again if the service does not exist.

### "MMHotline's backup is not set up: ..."

Exit 3. `/etc/mmhotline/backup.env` is missing. Set up the backup as
[Back up and restore the database](how-to/back-up-and-restore-the-database.md#set-up)
says, or stop the monitoring check if you chose no backup.

### The journal says "Content-MD5 OR x-amz-checksum- HTTP header is required"

The bucket has Object Lock on, and refuses every upload. Use a bucket
without Object Lock.

### The restore block stops with an error

Leave `mmhotline` and `mmhotline-backup` stopped, fix the error, and run
the block again. The old state stays in the saved folder. See
[Restore](how-to/back-up-and-restore-the-database.md#restore).

## The down alarm and the status page

### Warning "MMHotline cannot reach the MMHotline-down alarm"

The dashboard's **MMHotline-down alarm** card says **Not checking in**.
MMHotline's check-ins with the alarm in Cloudflare have failed for 10
minutes, such as during a Cloudflare outage. Without check-ins, the alarm
acts as if MMHotline were down: it sends its urgent alerts once, then
nothing more, even if MMHotline really goes down later.

Run `sudo mmhotline down-alarm show`, which says why; **Why** on the card
says the same. See
[Set up the MMHotline-down alarm](how-to/set-up-the-mmhotline-down-alarm.md#what-happens-when-mmhotline-is-down).

### "the MMHotline-down alarm wakes nobody"

The card says
`If MMHotline goes quiet, the MMHotline-down alarm wakes nobody: nobody on call gets SOS alerts by Pushover or automated voice call.`
A test says
`The MMHotline-down alarm holds no message, so it would wake nobody.` and
exits 1.

The person on call and the backup add a Pushover notification or an
automated voice call that gets SOS alerts, as
[Add contact methods](how-to/add-contact-methods.md) says.

### "... were not accepted, so those people would not be woken."

Such as
`The MMHotline-down alarm sent a test alert for each held message; 3 of 3 were not accepted, so those people would not be woken.`,
with `refused by the service (HTTP 400)` on each line. Pushover or Twilio
refused the message, such as for a wrong user key or phone number. Check
those contact methods with `sudo mmhotline contact-methods send-test`.

### "mmhotline_down_alarm: did not apply: ... first check-in with the MMHotline-down alarm failed for 30 seconds: ..."

The Worker was uploaded, but its first check-in failed; the reason follows.
The earlier setup stays, or none the first time. Fix the reason, such as
the Cloudflare token's permission, and apply again. See
[Set up the MMHotline-down alarm](how-to/set-up-the-mmhotline-down-alarm.md).

### An Info alert "MMHotline runs again: it was down, and the MMHotline-down alarm woke you"

MMHotline was down, and the alarm woke the people it lists. Find out why
MMHotline stopped with `sudo journalctl -u mmhotline --no-pager -n 100`.

### "status_page: did not apply: ..."

The status page's key cannot write to its bucket; the line says why. Check
that the token is for this bucket, with **Object Read & Write**. See
[Publish a status page](how-to/publish-a-status-page.md).

### The status page answers "403"

In the Cloudflare dashboard, check that the bucket's custom domain says
**Active**.

### "The status page is not set up."

On the server: the settings file has no `status_page` section. On a
laptop, the command instead says
`mmhotline serve is not running at command socket /run/mmhotline/command.sock`:
the `.env` file with the status page's key was not loaded in this
terminal. See [Post to the status page](how-to/post-to-the-status-page.md).

## The triage agent

### `settings apply` fails the triage agent's setup

The line for `triage_agent` says why the test prompt failed, such as for a
model name the coding agent's service does not know. The earlier setup
stays. Fix the file and apply again. See
[Set up the triage agent](how-to/set-up-the-triage-agent.md).

### "A triage agent run is already active."

`sudo mmhotline triage-agent check-sign-in` or `run-software-review` says
this while a triage session or another run uses the agent. Alerts come
first, so the command does not wait. Run it again after the run ends.

### Warning "The triage agent cannot sign in to ..."

The coding agent's credential no longer works. A ChatGPT sign-in used
elsewhere also says
`Another copy of this ChatGPT sign-in was used, so OpenAI ended this one. Sign in again just for MMHotline, and use that sign-in nowhere else.`
Warnings such as "The triage agent's Claude Code token expires on ..."
come 30 and 7 days before a token ends.

Replace the credential, as
[Keep the agents signed in](how-to/keep-the-agents-signed-in.md) says. If
the sign-in check works but runs fail with an API key, the account may
have no credit left; look at the account with the vendor.

### "MMHotline refused the connection: The host could not be resolved."

The agent's steps show this when the server's name is missing from
`util01`'s `/etc/hosts`. Add it, as
[Set up the triage agent](how-to/set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository)
says, step 1.

### "connect to a server by the server's inventory name, such as ssh db07"

The agent's steps show this when the agent tried an IP address. MMHotline
lists each server only by its inventory name, such as `web1`, so the agent
connects by name.

### "Could not prepare the systems repository: ... detected dubious ownership ..."

MMHotline's copy of the systems repository has a folder on the server as
`origin`. Until it is fixed, the agent's requests fail with
`MMHotline could not push your commits ...`. Clone it again from the Git
host over SSH, as
[Set up the triage agent](how-to/set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository)
says, step 6.

### The agent reports that it waits, or that a file is missing

The agent's report names the cause in its own words. The usual ones:

- **Its start playbook waits for a person to review its commits:** the
  agent does not sign its commits. Set up its `.gitconfig` (preparation
  step 3), then review the waiting commits.
- **Its rules file is missing:** add `triage_agent_rules.md` at the top of
  the systems repository, in a commit signed by a trusted key (step 4).
- **It cannot read the system journal:** add `triage` to the group
  `systemd-journal` on that server (step 2).
- **MMX's node runner is missing:** put the runner for the server's
  processor beside `mmx` in `/usr/local/bin` (step 7).

The steps are in
[Set up the triage agent](how-to/set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository).

### "Review needed for commit ..., which no key the team trusts signed"

MMX runs stop before that commit until a person reviews it, or a key the
team trusts signs it. Review it, as
[Review a commit](how-to/review-a-commit.md) says.

### "... no systems_repository_commits section, so MMHotline trusts no commit ..."

`settings show` prints this note, and every MMX run and
`commit-reviews check` is refused with it, while the applied settings have
no `systems_repository_commits` section, such as after an upgrade that
moved the commit keys there. Add the section, as
[Review a commit](how-to/review-a-commit.md#set-up) says, and apply the
settings.

### "everyone is locked out: the file /etc/mmhotline/everyone-locked-out exists ..."

The team's break-in procedure created the lock-out file. While it exists,
MMHotline runs no triage agent, review agent, or MMX for the agent, trusts
no commit, and sends people every alert at once; incident histories say
"Everyone is locked out". Alerting and the pages work as usual. When the
break-in is handled, the team's procedure removes the file, and MMHotline
lets the agent work again within a second, without a restart
([everyone locked out](changing-mmhotline/design/operation.md#everyone-locked-out)).

### "... check_only has no rule; it needs a person until you add one"

Such as
`rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one`.
This is a note, and the settings still apply. Add the kind of action to
the inventory's rules if a person should not be needed. See
[Decide what the triage agent may do](how-to/decide-what-the-triage-agent-may-do.md).

### "inventory ... has no rules for running the agent's playbooks"

The `rules_for_running_agent_playbooks` part did not apply for that
inventory: the settings file has no entry for an inventory in MMHotline's
copy. Every fix there needs a person until you add its rules. The rules
for the listed inventories still applied.

### "... upgrade MMX to 0.1.0 or later ..."

MMHotline refuses every playbook request: the `mmx` in `/usr/local/bin` is
too old to say which files a playbook saves. Install MMX 0.1.0 or later.

### A fix you expected to run waits for a person

The playbook acts on a listed service, or one of its tasks is not
recognized. The request's **Why it needs a person** names which. See
[Decide what the triage agent may do](how-to/decide-what-the-triage-agent-may-do.md#check-whether-a-playbook-needs-a-person).

### "The scheduled software review did not start: the systems repository has no scheduled_software_review_task.md"

The review has no task to follow. A task file that is empty or larger than
64 KiB fails the same way. Such a review costs nothing. Add the task file,
as [Turn on the scheduled software review](how-to/turn-on-the-scheduled-software-review.md)
says, or set `software_review_interval_hours` to `off`.

### A report says the logging proxy refused a host

The agent tried a host that is not listed, such as the host a vendor page
redirects to. The Triage agent page's **Recent connections** shows the
refusal. Add the host to `triage_agent.listed_hosts`, such as
`[{host: packages.example.com, port: 443}]`, and apply.
