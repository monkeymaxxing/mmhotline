# Connect Twilio

**Who acts:** an administrator with `sudo` on the MMHotline server, or that
person's coding agent, with whoever holds the team's Twilio account. Then
each person on the team, on their own phone.

**When to use this page:** to have MMHotline phone people for an SOS, such
as a person who may sleep through a notification, or to change the
calling number or the Twilio key.

**What this page does:** you give MMHotline a Twilio API key and a calling
number, add each person's phone number as a contact method, and test that
a call rings through Do Not Disturb.

**Terms:**

- An **SOS** is the urgent alert that wakes the person on call. MMHotline
  phones only for an SOS.
- An **automated voice call** is MMHotline's call: it reads the alert
  twice and hangs up. A call never claims the incident; it tells the
  person to claim it on MMHotline's page, or by acknowledging it in
  Pushover.
- **E.164** is a phone number written as `+`, the country code, and the
  number, such as `+12025550102`.

## Choose the route

- **Connect Twilio for the first time:** follow [Steps](#steps).
- **Twilio is connected, and a person adds their number:** start at
  step 3.
- **Change the key or the calling number, or remove Twilio:** see
  [Change or remove the setup](#change-or-remove-the-setup).

## Before you start

- MMHotline is installed, and you can change its settings file, as
  [Change the settings](change-the-settings.md) says.
- The team has a Twilio account with a phone number that can make voice
  calls. The server can reach Twilio over HTTPS; Twilio never connects to
  the server.
- Each person's number is in a country MMHotline calls: the US, Canada,
  the 27 EU countries, the UK, Norway, Switzerland, or Iceland. Under `+1`,
  the Caribbean countries and US territories are not included, and
  toll-free, premium, and other numbers that are not a person's phone are
  refused.

## Steps

1. **Create a key for MMHotline at Twilio.** In the
   [Twilio Console](https://console.twilio.com):

   1. Select the team's account and copy its **Account SID**, which starts
      with `AC`.
   2. Create a dedicated **Standard** API key for MMHotline, as
      [Twilio's API key instructions](https://www.twilio.com/docs/iam/api-keys)
      say, and keep its SID, which starts with `SK`, and its secret.
      Twilio shows the secret only once.
   3. Choose the phone number that MMHotline calls from: a number of this
      same account that can make voice calls. A Twilio trial account calls
      only numbers verified in the account, so verify each person's number
      there, or upgrade the account.

   Reason for a dedicated key: you can revoke it without changing the
   account's main credentials or other programs that use the account.

2. **Connect it to MMHotline.** Add Twilio to the settings file, under
   `sending_services`, with your values. If `sending_services` already
   holds another service, such as `pushover:`, add `twilio:` beside it,
   indented the same:

   ```yaml
   sending_services:
     twilio:
       account_sid: AC00000000000000000000000000000001
       api_key_sid: SK00000000000000000000000000000001
       phone_number_to_call_from: "+12025550102"
       extra_phone_numbers_on_the_contact_card: []
   ```

   - `phone_number_to_call_from` is the calling number, in E.164, in
     quotes so YAML keeps it as text.
   - `extra_phone_numbers_on_the_contact_card` lists other numbers to put
     on the contact card that people download in step 5, such as
     `["+12025550103"]`, or `[]` for none. MMHotline never calls from
     them; they only join the card.

   Leave out `api_key_secret`: the next command saves it. It is the API
   key's secret, never the account's main auth token.

   Then save the secret and apply the file:

   ```sh
   sudo mmhotline settings set-secret sending_services.twilio.api_key_secret --apply
   ```

   The command waits for the secret: paste it, then press Enter. Nothing
   shows as you paste. A coding agent never composes its own shell line for
   a secret: it gives you this command to run in your own terminal. Reason:
   a secret typed on the command line would stay in the shell history and
   show in the list of running processes.


   **Check:** among the lines it prints are:

   ```text
   sending_services.twilio: applied.
     sending_services.twilio: Next step: Add your own phone number under Contact methods, then download the MMHotline contact card and test Do Not Disturb.
   ```

   MMHotline checked with Twilio that the number belongs to the account
   and can make voice calls, without making a call. Listing the account's
   numbers is also the key's check, because a Standard key cannot read the
   account itself.
   `sudo mmhotline sending-services list` now says
   `Twilio: set up by the settings file ... Checked with Twilio: account AC00000000000000000000000000000001, calling from +12025550102.`

3. **Add each person's number.** In MMHotline, each person opens
   **Contact methods**, enters their number with its country code under
   **Add a phone number**, such as `+1 202 555 0123`, and chooses
   **Add phone number**. The form tells them that adding the number means
   they agree to get alert calls from MMHotline. It accepts `00` in place
   of `+`, ignores spaces, dashes, dots, and parentheses, and stores the
   number in E.164.

   An administrator can add it for a person from the server instead; the
   command takes the number in the same forms as the page:

   ```sh
   sudo mmhotline contact-methods add --sign-in-email-address barbara@example.com \
     --contact-method-type automated_voice_call --phone-number-to-call +12025550123 \
     --alert-severities sos
   ```

   **Check:** the output says the contact method is saved, for SOS only,
   and a test call is queued. Moments later, `contact-methods list` says
   `Last test message: accepted`: Twilio took the call request. The
   call's result shows on the card, as step 4 says.

   ```text
   Contact method type: automated_voice_call. Sends to: +12025550123. Alert severities: sos.
   ...
   Contact method saved. A test message for the contact method is queued. Inspect the last test result to check delivery.
   ```

4. **Answer the test call.** It names who asked for the test and when,
   then says
   **This is a test call from Hotline. Open Hotline to check the
   call result.** twice, then ends. A test opens and claims no incident.

   **Check:** the card's **Last test** on the Contact methods page says
   **Answered (by a person or voicemail)**. Other results are **Calling**,
   **No answer**, **Busy**, and **Call failed**. Answered also counts voicemail,
   so it does not prove the person heard it; step 5 does.

5. **Make the call ring through Do Not Disturb.** On the phone:

   1. On **Contact methods**, choose **Download the MMHotline contact
      card**, and add the contact to the phone's contacts. The card holds
      the calling number and the extra numbers.
   2. Allow that contact through **Do Not Disturb**. On an iPhone, also
      turn on **Emergency Bypass** for it. Saving the contact also matters
      when the iPhone's **Silence Unknown Callers** is on: that setting
      sends calls from numbers that are not in the contacts straight to
      voicemail, without ringing.
   3. Turn on Do Not Disturb, and press **Send test** on the
      **Automated voice call** card. Wait at least a minute after the
      first test.

   **Check:** the phone rings loudly enough to wake the person.

## What MMHotline does with calls

- **Calls are for SOS only,** to the person the SOS is waking at that
  moment. They skip a person who is unavailable, unless the card's
  **Send even when I'm unavailable** is on; team mute always stops them,
  tests included.
- **What a call asks for:** to claim the incident on MMHotline's page, or
  by acknowledging the alert in Pushover; without Pushover connected, the
  page only.
- **A call** speaks English, says the alert twice, rings for 30 seconds,
  and lasts at most two minutes. MMHotline cannot tell a person from an
  answering machine. Reason for the limit: an unanswered call ends by
  itself.
- **Calls repeat** every minute while the SOS waits for the person. After
  30 minutes of calling one number, an answered call makes the next repeat
  wait five minutes, so voicemail does not fill up, and ten minutes
  without a call ends that calling period. One call to a number runs at a
  time; MMHotline waits at most three minutes for its result before the
  next. MMHotline asks Twilio for each call's result; Twilio never calls
  back.
- **Calls start** at most one a second, and calls still waiting stay in
  MMHotline, so a claim cancels them. First calls for an SOS go before
  repeats and tests, and SOS alerts due together may become one call,
  which reads up to three titles and then the number of others.
- **A call whose outcome is unknown** is not repeated at once; the next
  SOS repeat can call again.
- **The spoken title** keeps at most 100 characters, without web
  addresses or long strings of digits, so it stays understandable.
- **When all of a person's calls fail,** their other contact methods for
  SOS get the alert with a note about the failed call.
- **Each person can have up to three numbers,** each once. A number
  belongs to one person and cannot be shared with the team, and only its
  owner changes it on the page; to change a number, remove its card and
  add the new one. Reason for three: one SOS cannot ring too many phones
  by mistake.
- **Test calls** are limited to one a minute and five in 24 hours for
  each number, and 20 in 24 hours for the team, so tests cannot run up
  the Twilio bill. The automatic test when a number is added counts too.

## Change or remove the setup

- **Change the key, the account, or the calling number:** change the keys
  under `sending_services.twilio` and apply. People's numbers stay, since
  they belong to the people. A change that fails Twilio's check changes
  nothing. When the calling number or the extra numbers change, MMHotline
  sends an Info notice to each person with a phone contact method, and the
  Contact methods page shows a notice until they download the new contact
  card. They then add the new numbers to the phone's contact and repeat
  the Do Not Disturb test.
- **Remove Twilio:** remove every automated voice call contact method
  first, then delete the `twilio` section and apply. While contact
  methods still use it, the apply keeps the setup and says
  `cannot remove the Twilio setup: 1 automated voice call contact method uses the sending service setup.`

## If something is different

- **The check says**
  `sending_services.twilio.account_sid: must be AC followed by 32 hexadecimal digits`,
  or the same for `api_key_sid` with `SK`: copy the SID again.
- **The apply says**
  `sending_services.twilio: did not apply: Twilio refused the API key; check the account SID, the API key SID, and the secret.`
  The three must belong together: the key must be an active Standard key
  of the account that owns the calling number. Nothing changed: if Twilio
  was connected before, the old setup keeps calling, and the line ends
  `The earlier setting, applied at ..., stays.`; if it was not, the line
  ends `It stays not set up, as before.`
- **The apply says**
  `the Twilio calling number must be an incoming phone number owned by the configured Twilio account and able to receive voice calls`:
  choose a voice number listed in that account.
- **Adding a number says**
  `enter the phone number with its country code, such as +1 202 555 0123 or +44 7700 900123`:
  add `+` and the country code.
- **Adding a number says**
  `MMHotline can't call phone numbers in Australia yet. Use a Pushover notification instead.`
  The number is outside the countries MMHotline calls; connect that person
  with [Pushover](connect-pushover.md).
- **Adding a number says**
  `enter a person's phone number, not a toll-free or premium number`: use
  the person's own phone number.
- **Any other message:** the
  [Twilio messages table](../when-something-fails.md#other-twilio-messages)
  says what each one means.
