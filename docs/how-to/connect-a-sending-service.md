# Connect a sending service

**Who acts:** an administrator with `sudo` on the MMHotline server, or that
person's coding agent, together with whoever holds the team's account at
the service. Then each person on the team adds their own contact method.

**When to use this page:** to choose how MMHotline reaches people, and to
connect a service, change it, or remove it.

**What this page does:** it helps you choose a service, and shows the
steps that every service shares. Each service's own page then gives its
steps in full.

**Terms:**

- A **sending service** is an outside service that MMHotline sends
  notifications through, such as Pushover or Twilio, with the team's setup
  for it in the settings file.
- A **contact method** is one place where a person gets notifications,
  such as their Pushover app or their phone number. A sending service must
  be connected before anyone can add a contact method that uses it.

## Choose the service

| Service | What arrives | Wakes a sleeping person? | Page |
| --- | --- | --- | --- |
| Pushover | A notification in the Pushover app | Yes, with emergency priority, once the phone's sound settings are tested | [Connect Pushover](connect-pushover.md) |
| Twilio | An automated voice call | Yes, once the calling numbers may ring through Do Not Disturb | [Connect Twilio](connect-twilio.md) |
| Mail server | An email | No | [Connect a mail server](connect-a-mail-server.md) |
| Slack | A Slack message | No | [Connect Slack](connect-slack.md) |
| Telegram | A Telegram message | No | [Connect Telegram](connect-telegram.md) |

Connect Pushover first: it is MMHotline's usual way to wake the person on
call. A phone call through Twilio is a second way, such as for a person
who sleeps through notifications. Email and chat are good for reading
alerts, but a message arriving there does not wake anyone, so every person
who can be on call needs Pushover or a phone call for an SOS.

## The steps every service shares

1. **Create what the service needs,** such as an application at Pushover
   or an API key at Twilio, as the service's page says.

2. **Add the service's section under `sending_services`** in the settings
   file, as [Change the settings](change-the-settings.md) says. For
   example:

   ```yaml
   sending_services:
     pushover:
       application_token: YOUR_30_CHARACTER_APPLICATION_TOKEN
   ```

   A team whose deploy tool writes the settings file adds the section in
   its repository, with the secrets in its vault. Each service's page
   under **Sending services** in MMHotline also shows these steps, with
   the settings file key for each value.

3. **Apply the file:**

   ```sh
   sudo mmhotline settings apply
   ```

   **Check:** the service's line says `applied.`, with the next step under
   it, such as:

   ```text
   sending_services.pushover: applied.
     sending_services.pushover: Next step: Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification.
   ```

   Before it saves a new or changed setup, MMHotline checks it with the
   service itself, without sending a notification. Reason: a mistyped
   token or a wrong account then never replaces a working setup. A
   service whose check fails keeps its earlier setup, the other parts of
   the file still apply, and the command names the service and its
   reason, and exits 1.

4. **Each person adds their own contact method,** on MMHotline's
   **Contact methods** page, or an administrator adds it for them with
   `sudo mmhotline contact-methods add`, as
   [Add contact methods](add-contact-methods.md) says. Saving it sends a
   test. Until the
   service is connected, the Contact methods page offers administrators a
   link to the service's setup, and asks other people to ask an
   administrator.

   **Check:** the test arrives, and `sudo mmhotline contact-methods list`
   says `Last test message: accepted`. Accepted means the service took the
   message; only the person can tell whether it woke them.

`sudo mmhotline sending-services list` shows each service and its setup,
never its secrets:

```text
Twilio: set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T05:09:23Z. Checked with Twilio: account AC00000000000000000000000000000001, calling from +12025550102.
Pushover: set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T05:08:50Z. Checked with Pushover: monthly message limit 10000.
```

Administrators see the same on the **Sending services** page, read-only,
with the settings file key for each value. MMHotline keeps each setup,
secrets included, in its database, so protect database backups as secret
data.

## Change or remove a service

- **Change a service:** change its keys in the settings file and apply.
  A change that fails the service's check changes nothing, and the line
  says why.
- **Remove a service:** first remove the contact methods that use it, then
  delete its section and apply. While contact methods still use it,
  MMHotline keeps the setup and says so:

  ```text
  sending_services.pushover: did not apply: cannot remove the Pushover setup: 2 Pushover notification contact methods use the sending service setup. Remove those contact methods on the Contact methods page, or with mmhotline contact-methods remove --contact-method-id ID, first. The earlier setting, applied at 2026-11-15T05:08:44Z, stays.
  ```

  Reason: removing the service would otherwise stop those people's alerts
  without anyone noticing.

Changes to a service need no restart of MMHotline.
