# Connect Slack

**Who acts:** an administrator of MMHotline who can create and install an
app in the team's Slack workspace, with someone with `sudo` on the
MMHotline server, or that person's coding agent. Then each person who
wants alerts in Slack.

**When to use this page:** to have MMHotline post alerts in Slack, in a
person's direct messages or a channel shared with the team, with buttons
to claim and resolve an SOS.

**What this page does:** you create the team's MMHotline app in Slack,
connect it to MMHotline, and each person connects their own Slack account.

**Terms:** **Socket Mode** is Slack's way for an app to receive messages
and button presses over a connection the app opens itself, so the
MMHotline server needs no address that Slack can reach. Slack messages do
not wake anyone: use [Pushover](connect-pushover.md) or
[phone calls](connect-twilio.md) for that.

## Before you start

- MMHotline is installed and behind Cloudflare Access, and you can change
  its settings file, as [Change the settings](change-the-settings.md)
  says.
- The server can reach Slack's API over HTTPS and open an outgoing
  WebSocket connection.

## Steps

1. **Create the Slack app from MMHotline's manifest.** In MMHotline, open
   **Sending services → Slack**, and follow **Create the Slack app**. Slack
   opens with MMHotline's app manifest, the app's settings and
   permissions, already filled in. Choose the team's workspace, create the
   app, and install it to the workspace. Then copy two tokens:

   - the **Bot User OAuth Token**, which starts with `xoxb-`;
   - an app-level token with the `connections:write` permission, which you
     generate under the app's **Basic Information**; it starts with
     `xapp-`.

   Keep the manifest's settings, and use MMHotline's link rather than an
   app made by hand. Reason: they turn on Socket Mode, the buttons, and
   direct messages to the app, request `chat:write`, `im:history`, and
   `app_mentions:read`, receive the `message.im` and `app_mention` events,
   and leave token rotation off; MMHotline relies on each.

2. **Connect it to MMHotline.** Save the two tokens in the settings file.
   The first command makes the `slack` section, so apply only after the
   second:

   ```sh
   sudo mmhotline settings set-secret sending_services.slack.bot_token
   sudo mmhotline settings set-secret sending_services.slack.app_level_token --apply
   ```

   Each command waits for a token: paste it, then press Enter. Nothing
   shows as you paste. A coding agent pipes the value in instead, from
   where the person put it. Reason: a secret typed on the command line
   would stay in the shell history and show in the list of running
   processes.

   The first command says the settings file still has a problem, because
   the app-level token is missing. The second command fixes it.

   **Check:** among the lines it prints are:

   ```text
   sending_services.slack: applied.
     sending_services.slack: Next step: Connect your own Slack account under Contact methods. Saving the contact method sends a test message.
   ```

   MMHotline checked the bot token with Slack's `auth.test`, opened Socket
   Mode, and saved the workspace, bot user, and app, without posting a
   message. A refused check prints Slack's reason and keeps any earlier
   setup.
   `sudo mmhotline sending-services list` says
   `Slack: set up by the settings file ... Checked with Slack: workspace Acme (T0001).`

   MMHotline keeps the setup, secrets included, in its database: protect
   database backups as secret data. A team whose deploy tool writes the
   settings file keeps the secrets in its vault. **Sending services → Slack**
   shows the setup read-only, with each key. Changes need no restart.

3. **Each person connects their own Slack account;** an administrator
   cannot do it for them, because the person proves the account is
   theirs by sending a code from it. In MMHotline, they open
   **Contact methods** and choose **Connect Slack**. The page shows a
   connection code:

   ```text
   Connect your Slack direct messages
   Open direct messages with the MMHotline app and send the connection code:
   0YIFSV_ka_FpGAhHjhF3zyaQ-sNV8zdDjtt_MKW2ZTk
   ```

   They follow **Open MMHotline in Slack** to the app's direct messages,
   and send only the code there.

   **Check:** the app answers in Slack
   `Slack is connected. Alerts will arrive in this conversation.`, then
   posts **MMHotline test message**; the answer alone is not the test. The
   card's **Last test** says **Posted to Slack**: Slack took the message,
   which does not prove anyone read it. `sudo mmhotline contact-methods list`
   shows the new contact method:

   ```text
   Contact method type: slack_message. Sends to: Slack direct messages with UBARBARA in Acme. Alert severities: info,sos,warn.
   ```

   The code works once, for 15 minutes. Connecting also tells MMHotline
   which Slack user is which person, so the buttons act as that person.
   A Slack conversation can be added only this way: `contact-methods add`
   answers with the Contact methods page's address.

4. **Optionally, connect a channel shared with the team.** An
   administrator chooses **Add a shared Slack channel** on Contact methods,
   invites the MMHotline app into the channel, and posts the code with a
   mention of the app, as the page shows:
   `@MMHotline cpB25CCOQB9sWx86aSKJRfotKX_HVE_mJy8DCBXvWC0`.

   **Check:** the test message arrives in the channel. The Slack user who
   posted the code acts as that administrator; other people still connect
   their own Slack accounts before they can use the buttons. Post a code
   only in the conversation you mean to connect.

New Slack contact methods get Info, Warning, and SOS, also while the
person is unavailable or the team is muted; change both on the card.

## What MMHotline does with Slack

- **An SOS about one incident** has the buttons
  **Claim this incident (assign it to me)** and **Resolve**. Only a Slack
  user connected to a current member can use them; anyone else gets a
  private reply with links to Contact methods and the incident. A claim
  never takes over another person's claim. After a press, that message
  shows the result. After a claim, **Resolve** stays beside it, so the
  person who claimed can resolve from Slack without signing in. Other
  copies stay as they were. If no reply comes,
  the incident's page shows where it stands.
- **Several waiting alerts** may arrive as one message, with links to the
  incidents in place of buttons.
- **Messages are plain text,** shortened to fit Slack's 3,000-character
  limit for a section while keeping
  the time, notes, the SOS instruction, and the incident link.
- **A message Slack did not confirm** is sent again, so a lost answer can
  lead to a second copy rather than a lost alert. MMHotline also waits and
  retries when Slack asks it to slow down.
- **Codes** last 15 minutes, so a copied code is not usable for long, and
  each person can hold five unused codes. A code sent in the
  wrong kind of conversation stays usable in the right one, and a new code
  for a conversation already connected adds no second contact method.

## Change or remove the setup

- **New tokens for the same workspace,** such as after a reinstall: put
  them in the settings file and apply. Every connection stays.
- **Another workspace, or removing Slack:** first remove every Slack
  contact method, because the conversations belong to the old workspace;
  then change or delete the `slack` section and apply. That also clears
  unused codes and the links between Slack users and people, so people
  connect again for a new workspace.

## If something is different

- **The check says**
  `sending_services.slack.bot_token: must be the Bot User OAuth Token, which starts with xoxb-`,
  or `sending_services.slack.app_level_token: must be the app-level token, which starts with xapp-`:
  the two tokens are swapped or copied from the wrong place.
- **The code gets no answer in Slack:** check that it went to the app's
  direct messages, or, for a shared channel, with a mention of the app in
  a channel the app was invited to. The service log,
  `sudo journalctl -u mmhotline --no-pager -n 30`, shows a failing Socket
  Mode connection; MMHotline reconnects by itself.
- **Slack says**
  `The Slack connection code expired or was already used. Choose Connect Slack under Contact methods for a new code.`:
  make a new code and send it within 15 minutes.
- **Any other message:** the
  [Slack messages table](../when-something-fails.md#other-slack-messages)
  says what each one means.
