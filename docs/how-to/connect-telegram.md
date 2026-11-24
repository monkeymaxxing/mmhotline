# Connect Telegram

**Who acts:** an administrator of MMHotline with a Telegram account, with
someone with `sudo` on the MMHotline server, or that person's coding
agent. Then each person who wants alerts in Telegram.

**When to use this page:** to have MMHotline send alerts to Telegram, in a
person's chat with the team's bot or a group shared with the team, with
buttons to claim and resolve an SOS.

**What this page does:** you create the team's Telegram bot, connect it to
MMHotline, and each person connects their own Telegram chat.

**Terms:** a **bot** is a Telegram account that a program runs; here,
MMHotline. Telegram messages do not wake anyone: use
[Pushover](connect-pushover.md) or [phone calls](connect-twilio.md) for
that.

## Before you start

- MMHotline is installed and behind Cloudflare Access, and you can change
  its settings file, as [Change the settings](change-the-settings.md)
  says.
- The server can reach `api.telegram.org` over HTTPS. Telegram never
  connects to the server.

## Steps

1. **Create the bot.** In Telegram, open a chat with
   [BotFather](https://t.me/BotFather), Telegram's bot for creating bots,
   send `/newbot`, and choose a name, such as **MMHotline**, and a
   username. Copy the token that BotFather answers with.

   Use a new bot for this MMHotline only, and keep its token private.
   Reason: the token lets a program send as the bot and read its messages
   and button presses.

2. **Connect it to MMHotline.** Save the token in the settings file and
   apply the file:

   ```sh
   sudo mmhotline settings set-secret sending_services.telegram.bot_token --apply
   ```

   The command waits for the token: paste it, then press Enter. Nothing
   shows as you paste. A coding agent never composes its own shell line for
   a secret: it gives you this command to run in your own terminal. Reason:
   a secret typed on the command line would stay in the shell history and
   show in the list of running processes.


   **Check:** among the lines it prints are:

   ```text
   sending_services.telegram: applied.
     sending_services.telegram: Next step: Connect your own Telegram chat under Contact methods. Saving the contact method sends a test message.
   ```

   MMHotline checked the token with Telegram's `getMe` and saved the bot's
   username and ID, without sending a message, and started reading the
   bot's updates. A refused check prints Telegram's reason and keeps any
   earlier setup. `sudo mmhotline sending-services list` says
   `Telegram: set up by the settings file ... Checked with Telegram: bot @mmhotline_fake_bot.`,
   with your bot's username.

   MMHotline keeps the setup, secrets included, in its database: protect
   database backups as secret data. A team whose deploy tool writes the
   settings file keeps the secrets in its vault. **Sending services → Telegram**
   shows the setup read-only, with each key. Changes need no restart.

3. **Each person connects their own Telegram chat;** an administrator
   cannot do it for them, because the person proves the chat is theirs by
   pressing Start in it. In MMHotline, they open
   **Contact methods**, choose **Connect Telegram**, follow
   **Open Telegram** on the device where they use Telegram, and press
   **Start** in the chat with the bot.

   **Check:** the bot answers
   `MMHotline is connected. Your alerts will arrive in this chat.`, then
   sends **MMHotline test message**; the answer alone is not the test.
   The card's **Last test** says **Sent to Telegram**: Telegram took the
   message, which does not prove anyone read it. `sudo mmhotline contact-methods list`
   shows the new contact method:

   ```text
   Contact method type: telegram_message. Sends to: Telegram chat with Carlos. Alert severities: info,sos,warn.
   ```

   The link works once, for 15 minutes. Connecting also tells MMHotline
   which Telegram user is which person, so the buttons act as that person.
   A Telegram chat can be added only this way: `contact-methods add`
   answers with the Contact methods page's address.

4. **Optionally, connect a group shared with the team.** An administrator
   chooses **Add the MMHotline bot to a Telegram group** on Contact methods,
   follows **Open Telegram**, and adds the bot to the group.

   **Check:** the bot says in the group
   `MMHotline is connected. Team alerts will arrive in this group.`, and
   the test message follows. The Telegram user who used the link acts as
   that administrator; other people still connect their own chats before
   they can use the buttons.

A person's own Telegram chat gets SOS and Warning, and a team group also
Info, also while the person is unavailable or the team is muted; change
both on the card.

## What MMHotline does with Telegram

- **An SOS about one incident** has the buttons
  **Claim this incident (assign it to me)** and **Resolve**. Only a
  Telegram user connected to a current member can use them; anyone else is
  asked to connect under Contact methods, or to open the incident, so a
  group member cannot act as an unknown person. A claim never takes over
  another person's claim, and the incident's history names who acted
  through Telegram. After a press, that message's buttons become one
  button that opens the incident and shows its state. After a claim, **Resolve**
  stays beside it, so the person who claimed can resolve from Telegram
  without signing in. Other copies stay as they were.
- **MMHotline asks Telegram for new messages and button presses** over and
  over, which Telegram calls long polling. So set no webhook for the bot,
  and run only one MMHotline per bot: Telegram hands each update to one
  reader only, and another one would take MMHotline's.
- **Several waiting alerts** may arrive as one message, with links to the
  incidents in place of buttons.
- **Messages are plain text,** at most 4,000 UTF-16 units, below
  Telegram's limit, where most emoji count as two, and shortened while
  keeping the time, notes, the SOS instruction, and the incident link.
- **A message Telegram did not confirm** is sent again, so a lost answer
  can lead to a second copy rather than a lost alert; only the copy
  Telegram confirmed has working buttons.
- **A group that Telegram upgrades to a supergroup** gets a new chat ID;
  MMHotline follows it, and waiting messages go to the new chat.
- **Links** last 15 minutes, so a copied link is not usable for long, and
  each person can hold five unused links. A personal link cannot connect
  a group, or the reverse; a link opened in the wrong kind of chat stays
  usable in the right one.

## Change or remove the setup

- **A new token for the same bot,** such as after BotFather's `/revoke`:
  put it in the settings file and apply. Every connection stays.
- **Another bot, or removing Telegram:** first remove every Telegram
  contact method, because the chats belong to the old bot; then change or
  delete the `telegram` section and apply. That stops reading the bot's
  updates, and clears old links and the links between Telegram users and
  people, so people connect again to a new bot.

## If something is different

- **The check says**
  `sending_services.telegram.bot_token: must be the token from BotFather: digits, a colon, and letters, digits, _ and -`:
  copy BotFather's whole token, not the bot's username.
- **Pressing Start gets no answer:** check that no other program reads the
  bot's updates and no webhook is set for it. The service log,
  `sudo journalctl -u mmhotline --no-pager -n 30`, names
  `Telegram getUpdates` failures with their time and the last good read,
  and says when reading works again.
- **The bot says**
  `This MMHotline link has expired or was already used. In MMHotline, open Contact methods and make a new link.`:
  make a new link and open it within 15 minutes.
- **Any other message:** the
  [Telegram messages table](../when-something-fails.md#other-telegram-messages)
  says what each one means.
