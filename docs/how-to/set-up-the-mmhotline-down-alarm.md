# Set up the MMHotline-down alarm

**Who acts:** an administrator with a Cloudflare account and `sudo` on the
MMHotline server, or that person's coding agent.

**When to use this page:** so that someone is still woken when MMHotline
itself is down: its server dies, or its location loses power or the
internet.

**What this page does:** you give MMHotline a Cloudflare token, and it puts
a small alarm program in your Cloudflare account that wakes the person on
call when MMHotline stops checking in.

**Terms:**

- The **MMHotline-down alarm** is a Cloudflare Worker, a small program that
  Cloudflare runs in your team's own Cloudflare account, outside the
  MMHotline server. Setup is optional and needs no domain.
- MMHotline **checks in** with it every minute and hands it the Pushover
  notifications and automated voice calls to send if the check-ins stop,
  so a change of schedule or contact method reaches it within a minute.

## Before you start

- MMHotline is installed, and you can change its settings file, as
  [Change the settings](change-the-settings.md) says.
- The person on call and the backup have a Pushover notification or an
  automated voice call that gets SOS alerts, as
  [Add contact methods](add-contact-methods.md) says. Reason: only those
  two wake a sleeping person, so the alarm holds only those.
- A Cloudflare account; the free plan is enough.

## Steps

1. **Make a Cloudflare token.** In the Cloudflare dashboard, copy your
   account ID from the account's home page. If you have never opened
   **Workers & Pages**, open it once, so the account gets its
   `workers.dev` address. Under **Manage Account → Account API Tokens**,
   create a custom token with one permission:
   **Account › Workers Scripts › Edit**.

   Make it an account API token, not a token under **My Profile**. Reason:
   MMHotline keeps the token to upload the new alarm after each upgrade,
   and Cloudflare stops a person's own tokens when that person leaves the
   account.

2. **Add the alarm to the settings file,** with your values:

   ```yaml
   mmhotline_down_alarm:
     cloudflare_account_id: 0123456789abcdef0123456789abcdef
     cloudflare_api_token: "YOUR_CLOUDFLARE_WORKERS_API_TOKEN"
     wake_the_person_on_call_after_minutes_without_a_check_in: 10
     wake_the_backup_after_minutes_without_a_check_in: 20
   ```

   - `wake_the_person_on_call_after_minutes_without_a_check_in`: how long
     MMHotline may be silent before the alarm wakes the person on call, 3
     to 60 minutes.
   - `wake_the_backup_after_minutes_without_a_check_in`: when it also wakes
     the backup, at least 5 minutes later. The backup is the one an
     unclaimed SOS escalates to: the named backup, or everyone else
     available with `backup_option: everyone_available`.

   A team whose deploy tool writes the settings file keeps the token in its
   vault.

3. **Apply the file:**

   ```sh
   sudo mmhotline settings apply
   ```

   **Check:** the line `mmhotline_down_alarm: applied.` MMHotline uploaded
   the Worker, turned on its `workers.dev` address, and checked in. A new
   address takes a few seconds to answer, so MMHotline tries the first
   check-in for up to 30 seconds.

4. **Check who would be woken:**

   ```sh
   sudo mmhotline down-alarm show
   ```

   **Check:** it names the Worker and the last check-in, and lists each
   person, how, and after how long:

   ```text
   MMHotline-down alarm Worker mmhotline-down-alarm-ops-example-com-7db8a22e at https://mmhotline-down-alarm-ops-example-com-7db8a22e.example.workers.dev.
   Set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T07:46:30Z.
   Last check-in accepted at 2026-11-15T07:46:30Z.
   If MMHotline stops checking in: Pushover notification to Carlos Example (u...) after 10 minutes.
   If MMHotline stops checking in: Automated voice call to Carlos Example (+12025550124) after 10 minutes.
   If MMHotline stops checking in: Pushover notification to Barbara Example (u...) after 20 minutes.
   ```

   The dashboard's **MMHotline-down alarm** card shows the same.

5. **Send a test:**

   THE PHONE MAY SOUND ONCE MORE. THERE IS NOTHING TO DO.

   ```sh
   sudo mmhotline down-alarm send-test
   ```

   The command prints that line first, before it sends anything. Each phone
   gets an emergency Pushover notification titled **Test of the
   MMHotline-down alarm**, or a call that says it is a test. For a Pushover
   notification, MMHotline then reads Pushover's receipt, which proves that
   Pushover accepted it, and cancels its repeats at once. Reason: an
   emergency notification repeats until someone acknowledges it, and the
   test should not keep the person busy. The command lists each test and
   exits 0 when every service accepted it and every Pushover receipt was
   read and cancelled:

   ```text
   Test: Pushover notification to Carlos Example: accepted by the service (HTTP 200). Pushover accepted the MMHotline-down alarm's notification (receipt r5qkvhu7wg8wwtbm4l5x2dnxzy9c3a). MMHotline cancelled its repeats.
   Test: Automated voice call to Carlos Example: accepted by the service (HTTP 201).
   The MMHotline-down alarm sent a test alert for each held message, and each service accepted it.
   ```

   The dashboard card's **Send a test now** does the same, at most once
   every 10 minutes and 6 times in 24 hours, because each test calls
   everyone on the list.

## What happens when MMHotline is down

- After the first wait, the alarm sends the held notifications to the
  person on call; after the second, to the backup.
- **When MMHotline runs again,** the people it reached get an Info alert,
  "MMHotline runs again: it was down, and the MMHotline-down alarm woke
  you", which lists what the alarm sent.
- **If MMHotline can't reach the alarm for 10 minutes,** such as during a
  Cloudflare outage, the team gets a Warning, and the dashboard card says
  **Not checking in**. Reason: without check-ins, the alarm sends its
  urgent alerts once, then nothing more, even if MMHotline really goes
  down later. `sudo mmhotline down-alarm show` says why check-ins fail.
- **After an upgrade,** MMHotline uploads the new alarm by itself.

## Change or remove the alarm

- **Change the waits:** change the two keys and apply.
- **Turn it off:** delete the `mmhotline_down_alarm` section and apply. The
  line says `mmhotline_down_alarm: not set up, because the settings file leaves it out.`
  MMHotline first tells the alarm to hold nothing. Reason: stopping
  MMHotline without that would wake the person on call after the wait.
  The Worker stays in your Cloudflare account; delete it under
  **Workers & Pages** if you no longer need it.
- **Move it to another Cloudflare account:** turn it off and apply, then
  add the section with the new account's ID and token and apply again.
  MMHotline refuses a direct change of account while the alarm is set up.
- **Before you stop MMHotline on purpose,** such as to remove it, turn the
  alarm off first, as [Remove MMHotline](remove-mmhotline.md) says.

## If something is different

- **The test says**
  `The MMHotline-down alarm sent a test alert for each held message; 3 of 3 were not accepted, so those people would not be woken.`,
  and each test line says `refused by the service (HTTP 400)`, and the
  command exits 1: the alarm reached Pushover or Twilio, which refused the
  message, such as for a wrong user key or phone number. Check those
  contact methods with `sudo mmhotline contact-methods send-test`.
- **A Pushover test line says** `Pushover's answer had no receipt`,
  `Pushover refused the receipt`, or `could not cancel its repeats`, and
  the command exits 1: `no receipt` and `refused the receipt` mean nothing
  proves that Pushover accepted the notification, so check the contact
  method with `sudo mmhotline contact-methods send-test`. `could not cancel
  its repeats` means Pushover accepted it, but the phone may sound again for
  up to 10 minutes; acknowledge the notification in Pushover.
- **The test says**
  `The MMHotline-down alarm holds no message, so it would wake nobody.` and
  exits 1: nobody on call or as backup has a Pushover notification or an
  automated voice call that gets SOS alerts.
- **The apply says**
  `mmhotline_down_alarm: did not apply: the MMHotline-down alarm's Worker was uploaded, but MMHotline's first check-in with the MMHotline-down alarm failed for 30 seconds: ...`:
  the reason follows; the earlier setup stays, or none the first time.
  Apply again once the reason is fixed.
- **`show` lists nobody, or the dashboard says the person on call can't be
  reached:** the person has no Pushover notification or automated voice
  call that gets SOS alerts.
- **Turning it off fails** with the `mmhotline_down_alarm` part not
  applied: the alarm could not be told. Apply again, or delete the Worker
  in the Cloudflare dashboard and apply again.
