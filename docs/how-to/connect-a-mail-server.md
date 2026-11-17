# Connect a mail server

**Who acts:** an administrator with `sudo` on the MMHotline server, or that
person's coding agent, with whoever runs the team's mail server or mail
service. Then each person who wants alerts by email.

**When to use this page:** to have MMHotline send alerts by email, such as
to read them during the day, or to change the mail server.

**What this page does:** you connect MMHotline to the team's mail server,
and each person adds their email address as a contact method.

**Terms:** **SMTP** is how programs hand email to a mail server. A
**relay** is a mail server that accepts mail from the team's servers.
Email does not wake anyone: use [Pushover](connect-pushover.md) or
[phone calls](connect-twilio.md) for that.

## Before you start

- MMHotline is installed, and you can change its settings file, as
  [Change the settings](change-the-settings.md) says.
- From the mail service or its administrator: the SMTP server's host name
  and port, its connection security, a user name and password if it
  requires them, and a sender address it lets MMHotline use, such as
  `alerts@example.com`.
- The server can reach the SMTP server.

## Steps

1. **Get the sender address allowed.** At the mail service, allow the
   sender address to send through the SMTP server, and get the user name
   and password, if it needs them. A relay that needs no sign-in has
   neither.

2. **Connect it to MMHotline.** Add the mail server to the settings file,
   under `sending_services`, with your values:

   ```yaml
   sending_services:
     mail_server:
       smtp_server: smtp.example.com:587
       smtp_connection_security: starttls
       smtp_user_name: alerts@example.com
       email_from_address: alerts@example.com
   ```

   - `smtp_server`: the host name and port.
   - `smtp_connection_security`: `starttls`, which switches to encryption
     after connecting, usually on port 587; `implicit_tls`, encrypted from
     the start, usually on port 465; or `unencrypted`, which sends the
     password and alerts across the network unencrypted.
   - `smtp_user_name` and `smtp_password`: both, or neither for a relay
     without sign-in. Leave out `smtp_password`: the next command saves it.
     The password is one line; spaces are part of it.
   - `email_from_address`: the address alone, without a name. Messages
     come from `MMHotline <alerts@example.com>`.

   Then save the password and apply the file. Skip this command for a
   relay without sign-in, and run `sudo mmhotline settings apply`:

   ```sh
   sudo mmhotline settings set-secret sending_services.mail_server.smtp_password --apply
   ```

   The command waits for the password: paste it, then press Enter. Nothing
   shows as you paste. A coding agent never composes its own shell line for
   a secret: it gives you this command to run in your own terminal. Reason:
   a secret typed on the command line would stay in the shell history and
   show in the list of running processes.


   **Check:** among the lines it prints are:

   ```text
   sending_services.mail_server: applied.
     sending_services.mail_server: Next step: Add your own email address under Contact methods. Saving the contact method sends a test message.
   ```

   MMHotline connected to the mail server, checked its encryption and
   sign-in, and closed the connection without sending a message. It checks
   the server's certificate against the certificates trusted on the
   MMHotline server, including that the certificate covers the host name
   in `smtp_server`. It signs in with PLAIN or LOGIN. A refused check
   prints the mail server's reason on that line and keeps any earlier
   setup.

   MMHotline keeps the setup, secrets included, in its database: protect
   database backups as secret data. A team whose deploy tool writes the
   settings file keeps the secrets in its vault. **Sending services → Mail server**
   shows the setup read-only, with each key. Changes need no restart.

3. **Add each person's email address,** contact method type `email`. In MMHotline, each person opens
   **Contact methods**, enters their address under
   **Add email contact method**, chooses the alerts to send, and chooses
   **Add email**. The contact method gets alerts at once. New email
   contact methods get Info, Warning, and SOS,
   also while the person is unavailable or the team is muted; the person
   can change both on the card.

   An administrator can add it for a person from the server instead:

   ```sh
   sudo mmhotline contact-methods add --sign-in-email-address barbara@example.com \
     --contact-method-type email --email-address barbara@example.com \
     --alert-severities sos,warn,info --send-while-unavailable-or-muted
   ```

   Administrators can also add an email address shared with the team, such
   as a team mailbox, on the Contact methods page.

   **Check:** the output says the contact method is saved and a test is
   queued:

   ```text
   Contact method type: email. Sends to: barbara@example.com. Alert severities: info,sos,warn.
   Send while unavailable or muted: true. Last test message: queued (queued at 2026-11-15T05:41:53Z).
   Contact method saved. A test message for the contact method is queued. Inspect the last test result to check delivery.
   ```

4. **Check the test email.** The mailbox gets **MMHotline test message**
   from the sender address.

   **Check:** the card's **Last test** says **Accepted by the mail
   server**, and `sudo mmhotline contact-methods list` says
   `Last test message: accepted`. Accepted means the mail server took the
   message, not that it reached the mailbox: if it is not there, look in
   spam or quarantine.

## What MMHotline does with email

- **An SOS email** links to the incident's page, where the person signs in
  through Cloudflare Access and can claim it. The links start with the
  settings file's `web_url`.
- **New and resolved alerts** follow the card's chosen severities.
- **Several waiting alerts** may arrive as one email, to keep the mailbox
  quiet.
- **Sending** waits at most 30 seconds for the mail server, so a slow
  mail server cannot hold up MMHotline's other notifications. A temporary
  refusal (4xx) or a failed connection is retried, starting after 5
  seconds and doubling up to 5 minutes, at most 8 times within 23 hours. A
  permanent refusal (5xx) is final.
- **A reply lost after the mail server accepted** a message can lead to a
  second copy, because MMHotline cannot know that the first one arrived.
- **Tests and alerts wait in MMHotline's database,** so a restart loses
  none.

## Change or remove the setup

- **Change the mail server:** change the keys under
  `sending_services.mail_server` and apply. A change that fails the check
  changes nothing.
- **Remove email:** first remove every email contact method; owners remove
  their own, administrators the shared ones, and root can remove any with
  `sudo mmhotline contact-methods remove --contact-method-id ID`. Then
  delete the `mail_server` section and apply. While email contact methods
  remain, the apply keeps the setup.

## If something is different

- **The check says**
  `sending_services.mail_server: smtp_connection_security must be starttls, implicit_tls, or unencrypted`:
  write one of the three exactly.
- **The check says**
  `sending_services.mail_server: email_from_address must be one plain ASCII email address, such as alerts@example.com`:
  write the address alone, without a name such as `MMHotline <...>`.
- **The apply says**
  `sending_services.mail_server: did not apply: SMTP setup check: the mail server does not offer STARTTLS, which smtp_connection_security: starttls requires.`
  The server, its port, or the security setting does not match; ask the
  mail service which one it uses.
- **Adding an address says** `enter one plain ASCII email address`: enter
  the address alone, without a name.
- **Any other message:** the
  [mail server messages table](../when-something-fails.md#other-mail-server-messages)
  says what each one means.
