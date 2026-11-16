# Write a custom schedule

**Who acts:** an administrator with `sudo` on the MMHotline server, or that
person's coding agent.

**When to use this page:** the built-in schedules in
[Set up people and the on-call schedule](set-up-people-and-the-on-call-schedule.md)
cannot say who is on call, such as with holidays, a weekend person, or a
rotation that skips someone.

**What this page does:** you write a small function that names who is on
call at a given time, give it test cases, preview it, and apply it.

**Terms:**

- **Starlark** is a small language that reads like Python. MMHotline runs
  the team's Starlark function in a locked-down child process, with no
  access to files, the network, or the clock.
- A **test case** is a local time with the person you expect on call then.
  MMHotline runs every test case before it uses the code.

## A first schedule

Barbara is on call on weekdays and Carlos at weekends. In the settings
file, `on_call_schedule` becomes:

```yaml
on_call_schedule:
  escalate_if_unclaimed_after_minutes: 5
  schedule_type: custom_schedule_code
  time_zone: America/New_York
  backup_option: everyone_available
  custom_schedule_code: |
    # Weekdays: Barbara. Weekends: Carlos.
    def on_call(at):
        if at.weekday in ["saturday", "sunday"]:
            return {"primary_sign_in_email_address": "carlos@example.com"}
        return {"primary_sign_in_email_address": "barbara@example.com"}
  custom_schedule_test_cases:
    - local_time: "2026-11-21T10:00"
      expected_primary_sign_in_email_address: carlos@example.com
    - local_time: "2026-11-23T10:00"
      expected_primary_sign_in_email_address: barbara@example.com
```

MMHotline asks `on_call(at)` about each quarter hour of the next 14 days,
and `at` says which moment it asks about. The function answers with the
person on call then, by sign-in email address. The two test cases say that
Saturday 21 November at 10:00 is Carlos's and Monday 23 November is
Barbara's.

Preview it:

```sh
sudo mmhotline schedules preview --settings-file /etc/mmhotline/mmhotline_settings.yml
```

**Check:** it lists each change, in UTC, and saves nothing:

```text
At 2026-11-15T06:15:00Z, primary: carlos@example.com; backup option: everyone_available; backup: everyone available.
At 2026-11-16T05:00:00Z, primary: barbara@example.com; backup option: everyone_available; backup: everyone available.
At 2026-11-21T05:00:00Z, primary: carlos@example.com; backup option: everyone_available; backup: everyone available.
At 2026-11-23T05:00:00Z, primary: barbara@example.com; backup option: everyone_available; backup: everyone available.
At 2026-11-28T05:00:00Z, primary: carlos@example.com; backup option: everyone_available; backup: everyone available.
Schedule preview complete. Nothing was saved.
```

The first line is now, a Sunday. 05:00 UTC is midnight in New York, so the
weekend ends at midnight on Sunday night.

Then apply it:

```sh
sudo mmhotline settings apply
```

**Check:** it says `on_call_schedule: applied.`, and
`sudo mmhotline schedules show` prints the code, its test cases, and who
is on call now:

```text
Test at local time 2026-11-21T10:00: expected primary carlos@example.com; backup not checked.
Test at local time 2026-11-23T10:00: expected primary barbara@example.com; backup not checked.
On call now: Carlos Example (carlos@example.com). Backup now: everyone else available: Barbara Example (barbara@example.com).
```

In MMHotline, **On call** lists the **Coming shifts**, and its folded
**The schedule code** shows the code, the test cases, the stored shifts,
and the last 20 saved versions.

## How to write the rules

- **Order matters:** the function returns at the first rule that
  matches, so put the narrowest rule first, such as a holiday before
  the weekend.
- **Compare dates as text:** `at.date == "2026-12-25"` or
  `at.date in HOLIDAYS`.
- **Count weeks from a handoff:** `weeks_since("2026-11-16 09:00", at) % 2`
  alternates two people each Monday at 09:00; `%` gives 0 or 1 also
  before the start date. Until 09:00 on Monday, the previous week's
  person stays on call.
- **Test both sides of each handoff,** such as 08:59 and 09:00 on the
  Monday, and both ends of a holiday.
- **People first:** the code may name only people already applied, so
  apply a new person in `people` before the schedule that names them.

## A larger example

A weekly rotation with a Monday 09:00 handoff, where Dana has weekends and
holidays, and the next person in the rotation is the backup:

```python
ROTATION = ["ana@example.com", "ben@example.com", "chen@example.com"]
HOLIDAYS = ["2026-12-25", "2027-01-01"]

def on_call(at):
    if at.date in HOLIDAYS or at.weekday in ["saturday", "sunday"]:
        return {"primary_sign_in_email_address": "dana@example.com"}
    week = weeks_since("2026-10-05 09:00", at)
    primary = ROTATION[week % len(ROTATION)]
    backup = ROTATION[(week + 1) % len(ROTATION)]
    return {"primary_sign_in_email_address": primary, "backup_sign_in_email_address": backup}
```

## What the code can use

`at` is the moment asked about, in local time in `time_zone`:

- `at.year`, `at.month`, `at.day`, `at.hour`, `at.minute`: numbers.
- `at.weekday`: `"monday"` to `"sunday"`.
- `at.date`, such as `"2026-11-21"`, and `at.time_of_day`, such as
  `"09:30"`. Both compare correctly as text, so
  `at.time_of_day >= "09:00"` works.
- `at.time_zone`: such as `"America/New_York"`.
- `at.in_time_zone("Europe/Berlin")`: the same moment in another time
  zone, for a team that hands over between time zones. Write the name
  as quoted text in the code, at most 50 names; a name built while the
  code runs is unknown.

Two helpers count local calendar time from a local date, or a date and
time: `days_since("2026-10-05", at)` and `weeks_since("2026-10-05 09:00", at)`.
They give whole days or weeks, rounded down, and a negative number before
the start. They count wall-clock time, so a 09:00 handoff stays at 09:00
across daylight saving changes.

## What the code returns

A dictionary with only these keys:

- `primary_sign_in_email_address`: a team member's sign-in email address,
  matched ignoring letter case and surrounding spaces, or `None` for
  nobody scheduled, which the page shows as **No one scheduled**. Then MMHotline wakes the backup first,
  then everyone available.
- `backup_sign_in_email_address`, optional: the backup at that time. Left
  out or `None`, the schedule's `backup_option` applies:
  `everyone_available`, or `fixed_person` with
  `backup_sign_in_email_address` in `on_call_schedule`;
  `next_in_rotation` is not offered for a custom schedule.

Unavailability, taking on call, and the backup work as for the built-in
schedules.

## Test cases

Each test case has `local_time`, such as `"2026-11-21T10:00"`,
`expected_primary_sign_in_email_address` (`""` for nobody), and optionally
`expected_backup_sign_in_email_address`. Write one for each rule in the
code, such as each holiday and each weekend, so a later change that breaks
a rule fails the preview. Up to 50 test cases. A test case uses the quarter
hour that holds its time; a time skipped by daylight saving uses the next
valid minute, and a repeated time its first occurrence. On the On call
page, a test case without an expected backup shows **Not checked**.

## Rules MMHotline keeps

- **Quarter hours.** A custom schedule changes only on the quarter hour.
  Reason: MMHotline works the answers out ahead of time, every 15
  minutes for 14 days, so an SOS never waits for the code.
- **Hourly runs.** MMHotline works the shifts out again every hour, after
  every apply, and at once when a person is removed. **On call** says
  `MMHotline knows who is on call until ...` and shows the last failure.
- **Daylight saving:** a handoff happens once. Through a repeated hour,
  the person on call at the end of the first pass stays on call; a
  handoff in a skipped time happens at the next quarter hour.
- **A failure keeps the earlier schedule.** A preview or an apply whose
  code or test case fails changes nothing.
- **Limits:** the code at most 64 KiB, with no control characters but
  tab and line feed and no text-direction marks, at most 100,000 steps for each
  answer, and 5 seconds for each run. Previews and applies together run at
  most 60 times an hour; the hourly runs do not count.
- **No clock, no randomness, no files.** There is no `now()`, `load()`,
  `while`, or recursion, and `print` output is dropped. Reason: the
  preview, the test cases, and the stored shifts then show exactly what
  will happen.

## If something is different

- **The preview says**
  `The test case for 2026-11-21T10:00 expects the primary barbara@example.com, and the code gives carlos@example.com. Change the code or the test case, then preview again.`
  Decide which one is wrong: the code, or what you expected.
- **The preview says**
  `Your code has a mistake on line 4: got newline, want ':'`: the line
  number counts from the first line of the code. Here, an `if` line lacks
  its colon.
- **The preview says**
  `on_call returned a sign-in email address that is not a team member's sign-in email address: dana@example.com. This happened for Sun 2026-11-15 01:15 EST.`
  Add the person to `people` and apply first, or fix the address.
- **The preview says**
  `Unknown key primary in the result. Use primary_sign_in_email_address and backup_sign_in_email_address.`
  Use the full key names.
- **The apply says**
  `on_call_schedule: did not apply: Your code has a mistake on line 4: ...`
  with `The earlier setting ... stays`: the earlier schedule is still in
  use; fix the code and apply again.
- **Administrators get the Warning**
  `MMHotline cannot work out who is on call: the custom schedule code failed`:
  an hourly run failed, so the stored shifts stay in use. A second Warning
  comes when fewer than 24 hours of shifts remain. After they run out,
  On call says
  `Your custom schedule code has no answer for this time, so the backup is contacted first, then everyone available.`
  Fix the code and apply it; a working run sends
  `The custom schedule code works again: MMHotline knows who is on call`.
- **The preview says**
  `You have run your code a lot in the last hour. Try again later.`:
  previews and applies are limited to 60 an hour; wait.
- **The preview says**
  `on_call must return a dictionary like {"primary_sign_in_email_address": "ana@example.com"}`:
  return a dictionary on every path through the function.
- **The preview says** `Your code did too much work for ...`,
  `Your code took too long to run.`, or
  `Your code used too much memory.`: make the code simpler, such as with
  fewer loops.
- **The full rules:** the
  [custom schedule design](../changing-mmhotline/design/scheduling.md#custom-schedule-code)
  covers daylight saving, the sandbox, and every message.
