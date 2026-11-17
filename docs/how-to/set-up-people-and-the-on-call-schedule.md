# Set up people and the on-call schedule

**Who acts:** an administrator with `sudo` on the MMHotline server, or that
person's coding agent.

**When to use this page:** to add or remove a person, change a person's
role, or choose who is on call and when.

**What this page does:** you list the team's people and the on-call
schedule in the settings file, preview the schedule, and apply both.

**Terms:**

- The **person on call** is the one an SOS wakes first. The **backup** is
  woken next if nobody claims the SOS in time; after the backup,
  MMHotline wakes everyone else who is available.
- The **time to claim** is how long each of these steps waits for someone
  to claim the SOS before the next step, such as 5 minutes.
- A **handoff** is the moment on call passes to the next person.

## Choose the route

| You want | Schedule type | Where |
| --- | --- | --- |
| Each person on call for a week, in turn | `weekly_rotation` | [Steps](#steps) |
| A fixed person for each weekday | `day_of_week` | [Other schedule types](#other-schedule-types) |
| A fixed person for each hour of the day, such as across time zones | `hour_of_day` | [Other schedule types](#other-schedule-types) |
| Anything else, such as holidays or a weekend person | `custom_schedule_code` | [Write a custom schedule](write-a-custom-schedule.md) |
| To cover for someone or be away for a while | none: no settings change | [Be away or take over on call](be-away-or-take-over-on-call.md) |

## Before you start

- MMHotline is installed and behind Cloudflare Access, and you can change
  its settings file, as [Change the settings](change-the-settings.md)
  says.
- Each new person can sign in through the team's Cloudflare Access
  application, and you know the email address that sign-in gives them.
  Reason: that address is how MMHotline knows who is signing in.

## Steps

1. **List the people.** In the settings file, `people` names everyone who
   uses MMHotline, with at least one administrator:

   ```yaml
   people:
     - name: Barbara Example
       sign_in_email_address: barbara@example.com
       role: administrator
     - name: Carlos Example
       sign_in_email_address: carlos@example.com
       role: member
     - name: Dana Example
       sign_in_email_address: dana@example.com
       role: member
   ```

   - `name`: shown on pages and in alerts.
   - `sign_in_email_address`: the address the person's Cloudflare Access
     sign-in gives. MMHotline stores it in lower case.
   - `role`: `administrator` or `member`. Administrators also open the
     administration pages, such as **Sending services**, and see other
     people's contact methods.

   To add a person and put them in the schedule, apply `people` first,
   then change the schedule: the preview in step 4 knows only people
   already applied.

   A new person adds their phone as a contact method
   ([Add contact methods](add-contact-methods.md)). Then run
   `sudo mmhotline incidents send-test-sos --sign-in-email-address EMAIL`
   with their sign-in email address, whether or not they are on call: it
   wakes only them, and proves their phone wakes them.

   Cloudflare Access decides who may reach MMHotline at all; this list
   decides who is on the team. Nobody needs an invitation or a password.

2. **Write the schedule.** For a weekly rotation:

   ```yaml
   on_call_schedule:
     escalate_if_unclaimed_after_minutes: 5
     schedule_type: weekly_rotation
     time_zone: America/New_York
     handoff_hour: 9
     handoff_minute: 0
     handoff_weekday: 1
     first_start_date: "2026-11-02"
     rotation_sign_in_email_addresses: [barbara@example.com, carlos@example.com, dana@example.com]
     backup_option: everyone_available
   ```

   - `escalate_if_unclaimed_after_minutes`: the time to claim, 1 to 60.
   - `time_zone`: the schedule's time zone, such as `Europe/Berlin`.
     Handoffs follow its local time, also across daylight saving changes,
     and MMHotline's pages show times in it.
   - `handoff_hour`, `handoff_minute`, `handoff_weekday`: the local
     handoff time, 0 to 23 and 0 to 59; weekday 0 is Sunday, 1 Monday.
   - `first_start_date`: the date the first person in the list starts,
     on the handoff weekday.
   - `rotation_sign_in_email_addresses`: the people in turn, each once.
     Each one must be in `people`.
   - `backup_option`: `everyone_available`; `next_in_rotation`, the next
     person in the list, for a weekly rotation only; or `fixed_person`, with
     `backup_sign_in_email_address: barbara@example.com`.

3. **Check the file:**

   ```sh
   sudo mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
   ```

   **Check:** it says
   `The settings file /etc/mmhotline/mmhotline_settings.yml has no problems.`

4. **Preview the schedule** when it names only people already on the team:

   ```sh
   sudo mmhotline schedules preview --settings-file /etc/mmhotline/mmhotline_settings.yml
   ```

   **Check:** it lists who is on call from each handoff, in UTC, and saves
   nothing:

   ```text
   At 2026-11-15T06:17:43Z, primary: carlos@example.com; backup option: everyone_available; backup: everyone available.
   At 2026-11-16T14:00:00Z, primary: dana@example.com; backup option: everyone_available; backup: everyone available.
   At 2026-11-23T14:00:00Z, primary: barbara@example.com; backup option: everyone_available; backup: everyone available.
   ```

   The first line is now; 14:00 UTC is 09:00 in New York. A schedule that
   names a person not yet applied is refused with
   `apply the settings file with those people first`, because the preview
   uses the running team.

5. **Apply the file:**

   ```sh
   sudo mmhotline settings apply
   ```

   **Check:** among the lines it prints are:

   ```text
   people: applied.
   on_call_schedule: applied.
   ```

   Then `sudo mmhotline schedules show` ends with who an SOS wakes now:

   ```text
   On call now: Carlos Example (carlos@example.com). Backup now: everyone else available: Barbara Example (barbara@example.com), Dana Example (dana@example.com).
   ```

6. **Tell each new person** to open MMHotline's address and sign in, then
   add a contact method that wakes them, as
   [Add contact methods](add-contact-methods.md) says. Until they do, an
   SOS cannot wake them.

   **Check:** in MMHotline, **On call** shows the schedule: **Week by
   week** for a weekly rotation, **Coming handovers** for the other
   built-in types. Its folded **Where the schedule is set** shows what the
   settings file sets. **People** lists each person, email address, and
   role. **The team** on On call names each person with what can wake them at night,
   such as `Woken by Pushover notification`. A person with nothing listed
   there cannot be woken.

## Other schedule types

Replace the type's keys in `on_call_schedule`; the other keys stay.

- **By day of the week:** 7 entries, a person for each weekday, Sunday first, with one
  handoff time. A Monday entry with a 09:00 handoff lasts until Tuesday at
  09:00. Use `""` for a day with nobody scheduled.

  ```yaml
    schedule_type: day_of_week
    handoff_hour: 9
    handoff_minute: 0
    assignment_sign_in_email_addresses: [carlos@example.com, barbara@example.com, barbara@example.com, barbara@example.com, barbara@example.com, barbara@example.com, carlos@example.com]
    backup_option: fixed_person
    backup_sign_in_email_address: barbara@example.com
  ```

- **By hour of the day:** `schedule_type: hour_of_day` and
  `assignment_sign_in_email_addresses` with 24 entries, hour 0 first, the
  same every day. It has no handoff keys.

A time with nobody scheduled shows **No one scheduled** and wakes the
backup first, then everyone available; the previous person is not kept.
If nobody at all is available, an SOS wakes every person. Handoffs follow
the time zone's daylight saving changes, as
[timezones and daylight-saving time](../changing-mmhotline/design/scheduling.md#timezones-and-daylight-saving-time)
describes.

## What a change does

- **A schedule change** affects new SOS alerts and the next escalation
  steps. An incident someone already claimed stays theirs, and a person
  an open SOS is waking now keeps their time to claim.
- **A changed name or role** takes effect when the file is applied.
- **Removing a person:** leave them out of `people` and of the schedule,
  and apply. Their browser access ends on their next request, their
  sign-in links stop working, their contact methods are removed, and
  queued notifications to them are cancelled; a phone call already
  placed cannot be recalled. Their name stays in history.

`sudo mmhotline people list` lists the people, and
`sudo mmhotline people create-sign-in-link` makes a sign-in link, as the
[people command reference](../reference/mmhotline-command.md#mmhotline-people-list) says.

## If something is different

- **The check says**
  `on_call_schedule.rotation_sign_in_email_addresses[2]: erin@example.com is not the sign-in email address of any person listed in people`:
  add the person to `people`, or fix the address.
- **The check says**
  `on_call_schedule: the first start date must be a date such as 2026-11-02 on the handoff weekday (0 is Sunday)`:
  choose a `first_start_date` on the `handoff_weekday`.
- **The apply says**
  `people: did not apply: Dana Example owns 1 claimed incident. Resolve the claimed incidents or have someone take over the claimed incidents before removing the person.`
  Reason: removing the person would leave the incident with nobody. Resolve
  it, or take it over as
  [Handle an incident](handle-an-incident.md#take-over-a-claimed-incident)
  says, then apply again. The same happens while the person has taken on
  call: give it back to the schedule first, as
  [Be away or take over on call](be-away-or-take-over-on-call.md) says.
  The other parts of the file still apply, and the earlier people stay.
- **A person cannot sign in:** their `sign_in_email_address` must match
  the address their Cloudflare Access sign-in gives exactly, apart from
  letter case.
