# Mute the team

**Who acts:** an administrator of MMHotline, in MMHotline's pages.

**When to use this page:** for planned work that will set off alerts you
already expect, such as a migration across many servers, when no phone
should ring for a while.

**What this page does:** you stop every Pushover notification and
automated voice call to the whole team until a chosen time, and start
them again.

**Terms:**

- A **team mute** stops the phones for everyone. Alerts still arrive,
  incidents still open, and escalation deadlines still pass; only waking
  people stops.
- Times on MMHotline's pages are in the team's time zone, which the foot
  of each page names.

## Choose the route

- **The whole team should not be woken:** follow [Steps](#steps).
- **One monitoring tool will be silent, such as while its server moves:**
  declare planned downtime for that tool instead, as
  [Declare planned downtime](declare-planned-downtime.md) says. Its other
  alerts still wake people.
- **One person can't respond:** that person sets themselves unavailable,
  as [Be away or take over on call](be-away-or-take-over-on-call.md) says.

## Steps

1. **Open On call.** Under **Mute the team**, replace the `2h` in
   **For how long** with how long, such as `15m`, and choose
   **Mute the team**.

   **Check:** the panel says `Muted until 02:17`, and **On call now** says:

   ```text
   The team is muted until 02:17: MMHotline sends no Pushover notifications or automated voice calls, so an SOS wakes nobody until then.
   ```

   The dashboard tells everyone
   `The whole team is muted until 02:17: no Pushover notification or automated voice call goes out, even for an SOS.`
   The alert history records a Warning titled
   `Barbara Example muted the team until 2026-11-15 07:16:51 UTC: nobody is woken`.
   Others see the panel's state but no form.
   `sudo mmhotline schedules show` ends with the same sentence, with
   `team_muted_until` in its `--json` output.

2. **Do the work.** While the team is muted:

   - **Pushover notifications and automated voice calls** go to nobody,
     tests included, also when nobody else is available. No setting on a
     contact method changes this.
   - **Email, Slack, Telegram, and outgoing webhooks** send only where
     the contact method's **Send even when ...** box allows it during a
     mute, as [Add contact methods](add-contact-methods.md#change-a-contact-method)
     says.
   - **The incident page** lists each notification it held back as
     **Not sent**, with the reason `the team is muted`.

3. **Unmute the team** when the work is done: on On call, choose
   **Unmute the team**. Otherwise the mute ends by itself at its end time.

   **Check:** the alert history has
   `Barbara Example unmuted the team: MMHotline wakes people again`; a mute
   that runs out has `The team mute ended: MMHotline wakes people again`.

## What happens when the mute ends

- **An SOS still open and unclaimed** wakes at once the people of the
  step it has reached, by Pushover and phone call. If its time to claim ran
  out during the mute, that is the next step, such as the backup.
- **MMHotline does not replay** every notification it held back; it
  continues each open incident from where it stands.
- **Personal unavailability** stays as each person set it.

The mute does not resolve or hand over any incident, and a request
already sent to a phone cannot be called back.

## Lengths

A mute lasts from one minute to 30 days, written as a number and a unit:
`15m`, `2h`, `1h30m`, or `3d`. It starts at once and always ends. To
change its end time, mute again with a new length; the new mute replaces
the old one.

## If something is different

- **There is no form under Mute the team,** only
  `Only an administrator can mute the team.`: ask an administrator.
- **The page says**
  `The On call page changed while you had it open. Check the page, then choose again.`:
  someone else changed the mute meanwhile; your change was not saved.
- **The page says** `duration must be between one minute and 30 days`,
  or `write the duration as a number and a unit, such as 30m, 2h, or 3d`:
  fix the length.
