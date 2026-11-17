# Connect Pushover

**Who acts:** an administrator with `sudo` on the MMHotline server, or that
person's coding agent, with whoever holds the team's Pushover account.
Then each person on the team, on their own phone.

**When to use this page:** to have MMHotline wake people through the
Pushover app on their phones, or to replace the team's Pushover token.

**What this page does:** you create a Pushover application for the team,
connect it to MMHotline, add each person's phone as a contact method, and
test that an emergency alert wakes them, also in silent mode.

**Terms:**

- An **SOS** is the urgent alert that wakes the person on call. Pushover
  sends it at emergency priority, which repeats until the person
  acknowledges it.
- The **application token** identifies the team's MMHotline application at
  Pushover, the sender. A person's **user key** identifies that person,
  the receiver. Each is 30 letters and digits, with upper and lower case,
  and both are private.
- A **contact method** is one place where a person gets notifications,
  here one person's user key.

## Choose the route

- **Connect Pushover for the first time:** follow [Steps](#steps).
- **Pushover is connected, and a person adds their phone:** start at
  step 3.
- **Replace the token or remove Pushover:** see
  [Change or remove the setup](#change-or-remove-the-setup).

## Before you start

- MMHotline is installed, and you can change its settings file, as
  [Change the settings](change-the-settings.md) says.
- The server can reach `api.pushover.net` over HTTPS.
- Each person has a Pushover account and the Pushover app, signed in on
  their phone, and is listed in `people` in the settings file.

## Steps

1. **Create the team's application at Pushover.** Sign in at
   [pushover.net](https://pushover.net),
   [create an application](https://pushover.net/apps/build) named
   **MMHotline**, and copy its API token.

   **Check:** the token is 30 letters and digits.

2. **Connect it to MMHotline.** Save the token in the settings file and
   apply the file:

   ```sh
   sudo mmhotline settings set-secret sending_services.pushover.application_token --apply
   ```

   Each command waits for the value: paste it, then press Enter. Nothing
   shows as you paste. A coding agent pipes the value in instead, from
   where the person put it. Reason: a secret typed on the command line
   would stay in the shell history and show in the list of running
   processes.

   The command checks the token's form before it saves anything, and
   never prints the token.

   **Check:** among the lines it prints are:

   ```text
   sending_services.pushover: applied.
     sending_services.pushover: Next step: Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification.
   ```

   MMHotline checked the token with Pushover, and saved the application's
   monthly message limit, without sending a notification.
   `sudo mmhotline sending-services list` now says
   `Pushover: set up by the settings file ... Checked with Pushover: monthly message limit 10000.`

3. **Add each person's phone.** Each person copies their own **User Key**
   from the main page of pushover.net after signing in. Then, in MMHotline,
   they open **Contact methods**, paste the key into **Pushover user key**
   under **Add Pushover notification**, and choose
   **Add Pushover notification**.

   An administrator can add it for a person from the server instead, with
   that person's sign-in email address from `people`:

   ```sh
   sudo mmhotline contact-methods add --sign-in-email-address barbara@example.com \
     --contact-method-type pushover_notification --pushover-user-key-file -
   ```

   The command waits for the key: paste it, then press Enter. Nothing
   shows as you paste. A coding agent pipes the key in instead. Reason: a
   key on the command line would stay in the shell history.

   USE YOUR OWN PUSHOVER USER KEY, NOT A GROUP KEY. AN ACKNOWLEDGEMENT FROM A GROUP CANNOT CLAIM THE INCIDENT.

   A group key still wakes phones, so MMHotline accepts it. But
   acknowledging on a phone does not claim the incident for the person,
   because MMHotline must know whose acknowledgement it is. Claiming on
   the incident's page still works. When you add a group key, MMHotline
   says so in capitals. To fix it, remove the contact method and add it
   again with the person's own user key.

   **Check:** the command, or the page, says the contact method is saved
   and a test is queued (with a group key, it also prints the warning
   above):

   ```text
   Contact method saved. A test message for the contact method is queued. Inspect the last test result to check delivery.

   THIS PHONE HAS NOT PROVED IT WAKES YOU YET. RUN: sudo mmhotline incidents send-test-sos --sign-in-email-address barbara@example.com
   ```

   The last line stays true until step 5: the test saved here is a quiet
   notification, and only a test SOS proves the phone wakes the person.
   `sudo mmhotline contact-methods list` keeps saying `Emergency test: not
   yet acknowledged.` for the contact method until then.

   The new contact method gets SOS, Warning, and Info alerts at once,
   because Pushover is often a person's only contact method. A person can
   change that on its card, or an administrator with `--alert-severities`.
   Like every personal contact method, it skips times the person is
   unavailable, unless the card's **Send even when I'm unavailable** is
   on, and team mute always stops it. Links in the notifications open
   MMHotline, where Cloudflare Access signs the person in with their
   sign-in email address from `people`.

4. **Check that the test arrived.** The person's phone shows a quiet
   notification titled **MMHotline can reach this phone**, at normal
   priority. It needs no acknowledgement.

   **Check:** the card's **Last test** on the Contact methods page says
   **Accepted by Pushover**, and `sudo mmhotline contact-methods list`
   says:

   ```text
   Send while unavailable or muted: false. Last test message: accepted (queued at 2026-11-15T05:08:44Z).
   ```

   Accepted means Pushover took the message. It does not prove the phone
   wakes the person; step 5 does.

5. **Make an emergency alert wake the person.** A notification that
   arrives silently does not wake anyone. Without Critical Alerts, an
   emergency alert arrives silently on a muted iPhone, and Pushover's
   default sound is a soft bell.

   On an iPhone:

   1. In the Pushover app's settings, turn on **Critical Alerts** for
      emergency priority, and accept Apple's permission prompt. Nothing
      needs changing in the iPhone's own Settings. Critical Alerts for
      high priority can stay off: MMHotline sends nothing at high
      priority.
   2. In the app's sound settings, choose an alarming sound and raise its
      volume. Pushover plays Critical Alerts at the app's volume, not the
      phone's.
   3. Turn on **Silent Mode**. On the server, send the person a test SOS
      (see below), and acknowledge the alert when it sounds.
   4. Turn Silent Mode off and **Do Not Disturb** on, and send a test SOS
      again.

   On Android, follow Pushover's
   [Overriding Do Not Disturb on Android](https://support.pushover.net/i258-overriding-do-not-disturb-on-android),
   which differs by phone maker, choose an alarming sound, and run the same
   two tests: muted, then with Do Not Disturb on.

   Send the test SOS from the server:

   ```sh
   sudo mmhotline incidents send-test-sos --sign-in-email-address barbara@example.com
   ```

   It wakes that one person, whether or not they are on call, and nobody
   else. It sends an emergency like a real SOS; acknowledging it claims
   only the test, which MMHotline closes itself. Run it once for each of
   the two phone settings above. **Send test** on the Pushover card, and
   `sudo mmhotline contact-methods send-test --contact-method-id ID`, send
   only the quiet notification again. They do not prove the wake.

   **Check:** in both tests, the phone sounds loudly enough to wake the
   person. A test that arrives silently, or too quietly, fails. After the
   first acknowledged test SOS,
   `sudo mmhotline contact-methods list` says
   `Emergency test: acknowledged at 2026-11-15T05:20:01Z.` for the
   contact method.

## What MMHotline sends through Pushover

| Message | Pushover priority | Repeats |
| --- | --- | --- |
| SOS | Emergency | Every 30 seconds, for up to 25 minutes per message |
| Test of the contact method | Normal, no acknowledgement needed | None |
| Warning | Normal, which respects Pushover's quiet hours | None |
| Info and resolved alerts | Low, without sound or vibration | None |

The 30-second repeat is Pushover's shortest. Each person chooses the
sound in Pushover. A test SOS (`incidents send-test-sos`) is an SOS in
this table.

- **Acknowledging an SOS in Pushover claims the incident** for the
  contact method's owner, the same as **Claim** on the incident's page. It
  never takes over another person's claim. MMHotline checks for
  acknowledgements at most every five seconds.
- **Repeats stop** when the incident is claimed or resolved, the person
  becomes unavailable, the incident moves on to someone else, the contact
  method is removed or its user key changed, or an administrator mutes the
  team. Team mute stops
  even the repeats already sounding.
- **The 25-minute limit** comes from Pushover, which repeats an emergency
  at most 50 times. If the SOS still needs the person after that,
  MMHotline sends a fresh one.
- **Several SOS alerts due at once** may arrive as one notification; acknowledging it
  claims every listed incident that can still be claimed.
- **Long alerts are shortened** to Pushover's limits of 250 characters for
  the title and 1,024 for the text, keeping the claim instruction and the
  **Open in MMHotline** link. A link longer than Pushover's 512 characters
  is not shortened into a broken address; the message fails instead.
- **Each person can have up to three Pushover contact methods,** and a
  key only once, so one alert does not reach many copies by mistake. A
  Pushover contact method belongs to one person, cannot be shared with
  the team, and only its owner changes it on the page.

## Change or remove the setup

**Replace the token** when someone who knew it leaves the team, or when it
may have leaked. A person's user key works with any application, so every
contact method stays.

1. [Create a new application](https://pushover.net/apps/build) named
   **MMHotline**, and copy its token.
2. Save the new token and apply the file:

   ```sh
   sudo mmhotline settings set-secret sending_services.pushover.application_token --apply
   ```

   **Check:** the line says `sending_services.pushover: applied.`
3. Send yourself a test with **Send test**, or
   `sudo mmhotline contact-methods send-test --contact-method-id ID`, and
   check that it arrives from the new application.
4. If the MMHotline-down alarm is set up, its held messages use the new
   token within 5 seconds; `sudo mmhotline down-alarm send-test` sends a
   test through it.

   THE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO.

MMHotline stops using the old token at once, but cannot revoke it at
Pushover: anyone who still has it can send through the old application.

If the apply says
`sending_services.pushover: did not apply: wait for MMHotline to close the open Pushover emergency messages before replacing or removing the application.`,
an emergency is still repeating on someone's phone, an SOS or a test of
the MMHotline-down alarm, and MMHotline needs the old token to stop it.
The rest of the file applied, and the old token keeps sending meanwhile.
Apply again once each one is acknowledged, cancelled, or expired: a
down-alarm test expires within 10 minutes, an SOS within 25.

**Remove Pushover:** remove every Pushover contact method first, then
delete the `pushover` section and apply. While contact methods still use
it, the apply keeps the setup and says
`cannot remove the Pushover setup: 2 Pushover notification contact methods use the sending service setup.`

## If something is different

- **The check says**
  `sending_services.pushover.application_token: must be one line of exactly 30 letters and digits, as Pushover shows the application's API token`:
  copy the whole token again, keeping upper and lower case.
- **The apply says**
  `sending_services.pushover: did not apply: Checked with Pushover: Pushover answered HTTP 400: application token is invalid. The earlier setting, applied at ..., stays.`
  Pushover does not know the token. Copy it again from the application's
  page at Pushover. Nothing changed: if Pushover was connected before, the
  old token keeps sending; if it was not, the line ends
  `It stays not set up, as before.`
- **Adding the contact method says**
  `Pushover user key must contain exactly 30 letters and digits`: copy the
  whole user key again.
- **Adding it says** `you already added this Pushover notification contact method`:
  the person already has that key; use the existing card.
- **The last test says `failed`** with
  `Pushover answered HTTP 400: user identifier is invalid`: the user key is
  wrong, or is the application's token. Remove the contact method and add
  it again with the person's own user key.
- **No test arrives,** but nothing failed: check that the person is not
  marked unavailable and the team is not muted, since tests follow the
  same rules as alerts.
- **Any other message:** the
  [Pushover messages table](../when-something-fails.md#other-pushover-messages)
  says what each one means.
