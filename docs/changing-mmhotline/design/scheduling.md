# On-call scheduling

All four schedule types are built: By day of the week, By hour of the day,
Weekly rotation with the `next_in_rotation` backup option, and Custom schedule
with its sandbox. The team's schedule comes from the settings file's
`on_call_schedule` ([settings file](settings-file.md#the-schedule)). An SOS
is the urgent alert that wakes the person on call, even at night; claiming an
incident tells the team you are handling the incident, and MMHotline stops
contacting the next person.

## Who is on call

Each team has one active schedule, a primary responder, and an optional
backup. Without a specific eligible assignment, coverage falls back to the
team of eligible people. The special team account owns shared contact
methods. The team account is not a person who can answer a call.

The decision order is whoever took on-call, active schedule, eligible
backup, then eligible team.

The displayed assignment names the scheduled person. Effective eligible
coverage names the responders MMHotline selects to contact. Show why a
scheduled person has been bypassed. If nobody is eligible, display the lack
of eligible responders explicitly. An SOS then contacts every member
([escalation](incidents.md#escalation-path)), subject to team mute.

[Availability](availability.md#taking-on-call) defines explicit replacement
and ending of a taken-on-call assignment. Coverage and incident ownership are
separate. Schedule edits, handoffs, and taking on-call do not move claimed
incidents or silently replace the eligible responder an open incident is
currently contacting.

The four schedule types are By day of the week (`day_of_week`), By hour of the
day (`hour_of_day`), Weekly rotation (`weekly_rotation`), and Custom schedule
(`custom_schedule_code`). The settings file names one type and only the keys
that type uses. Applying the settings file performs the following steps:

1. MMHotline validates the configuration with the rules below.
2. MMHotline saves the configuration under the selected schedule type.
3. MMHotline makes the selected schedule type active.
4. MMHotline removes the saved configurations of the other schedule types.

Only the settings file can make a schedule active. Keeping inactive
configurations would leave saved references to people who may have left.
A failed apply leaves the current schedule active, and `settings apply`
names the reason.

## The settings file and the schedule commands

`on_call_schedule` holds the schedule's configuration keys. The following
keys identify people by sign-in email address:

- `assignment_sign_in_email_addresses`
- `rotation_sign_in_email_addresses`
- `backup_sign_in_email_address`
- In custom test cases, `expected_primary_sign_in_email_address` and
  `expected_backup_sign_in_email_address`.

`escalate_if_unclaimed_after_minutes` sets how long each escalation step waits for a
claim. The [incident design](incidents.md#initial-notification-and-repeats)
owns the time to claim. The server translates every address to and from
stored member IDs. Addresses make the file readable without an ID lookup.

The settings file's static check refuses an address absent from the file's
`people`. MMHotline preserves Starlark source verbatim because the custom
schedule's answers already use sign-in email addresses.

During `settings apply`, MMHotline applies `people` before `on_call_schedule`,
so a new person exists before the schedule names the person. Applying
`on_call_schedule` calls the schedule save operation for the selected type,
using the team's current schedule revision. Custom configurations require
the sandbox's full run and passing test cases.
The saved version and the schedule's audit message name
"the settings file /etc/mmhotline/mmhotline_settings.yml"
as the author, with an empty member ID.

The backup remains the escalation destination.

The [command reference](../../reference/mmhotline-command.md#mmhotline-schedules-show) owns
`schedules show` and `schedules preview --settings-file FILE`, and their JSON.
Preview reads the file's `on_call_schedule` and works out its assignments
without saving anything, for people already on the team. The page and command
evaluate assignments at matching preview instants: current time and exact
boundaries for the next 24 hours of hourly assignments, eight daily handoffs,
or eight weekly handoffs.
Custom code previews 14 days. The command merges unchanged adjacent
assignments and returns each actual change.

## By day of the week

Assign a primary user to each weekday, interpreted in the schedule timezone.
Configure a shift-change time with hour and minute. A Monday assignment with a
09:00 handoff covers Monday 09:00 through Tuesday 09:00 in the schedule
timezone.
Before Monday's handoff, Sunday's assignment still applies.

The week-start preference affects presentation. Store weekday identity
explicitly so reordering the displayed week does not reassign people.

Configuration includes:

- Named IANA schedule timezone.
- Shift-change time and weekday-to-user assignments.
- Backup option and optional fixed backup user.
- Configuration revision; each save records who changed the schedule and when
  in the audit history.

Assignments reference stable member IDs. A name change must not
silently break a built-in schedule.

## By hour of the day

Assign a primary user to each of the 24 local hours in the schedule timezone.
The pattern repeats daily. Each assignment applies on a half-open interval:
start is included and end is excluded. Merge adjacent assignments to the same
person in the displayed timeline, while preserving exact boundaries.

Configuration includes the timezone, hour-to-user assignments, backup option,
optional fixed backup, and configuration revision. By hour of the day uses
whole-hour slots; sub-hour slots are not supported.

## Weekly rotation

**Built.**

Configure an ordered list of at least one member and a handoff weekday and
time. Use a named IANA timezone. Set the date the first person starts.
The start date must have the handoff weekday. Starting with the first
person, each member is on call for one week from the handoff time until the
next handoff. After the last member, repeat from the first.

Let k count weeks since the start date, starting at zero. Week k starts at
the handoff time on the start date plus
k local calendar weeks. Select the member at list index k modulo the list
length, so the list repeats after the last member.

Use local calendar weeks. As for By day of the week, a handoff in a missing
local minute occurs at the next valid minute. A handoff in a repeated minute
occurs at the first occurrence, without switching back. `schedules preview`
previews the rotation with the same evaluator as live on-call selection.
Store the member order, backup option, and configuration revision with the
rotation.

## Backup options

- **A fixed person** (`fixed_person`): a selected person is the backup.
- **Next person in the rotation** (`next_in_rotation`): the next member in the
  ordered list; available only for Weekly rotation.
- **Everyone available** (`everyone_available`): use all eligible members
  without selecting a specific backup.

The final escalation step is always everyone available.

Exclude duplicate primary/backup targets from a notification step. Eligibility
uses the current availability state and is checked again before every attempt
of an active incident ([escalation](incidents.md#escalation-path)).

## Custom schedule code

**Built.** The limits bound the sandbox’s CPU, memory, and output so custom
schedule code cannot exhaust the server’s resources.
[Routine choices](#routine-choices-for-custom-schedule-code) lists the details
chosen while building custom schedule code.

Custom schedule code is for on-call patterns the built-in types cannot express,
such as holidays or a weekend person. The team writes a Starlark function,
`on_call(at)`, in the settings file's `on_call_schedule.custom_schedule_code`,
that says who is primary, and optionally backup, at one moment. MMHotline runs
the function with the pure-Go `go.starlark.net` interpreter in a locked-down
child process. An SOS never waits for custom schedule code.
MMHotline works out the answers for the next 14 days ahead of time and stores
the answers as shifts. Live decisions read the stored shifts. Applying the
file activates the code at once after a successful 14-day run and passing test
cases.

```python
# Weekly rotation, handoff Monday 09:00. Dana has weekends and holidays.
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

### The function and its inputs

`at` is the moment MMHotline is asking custom schedule code to evaluate.
Live coverage reads the stored shift for the current time. There is no
`now()` and no other access to the real clock, so the preview, the stored
shifts, and the test cases show exactly what will happen.

`at` has these fields, in local time in the schedule timezone:

- `at.year`, `at.month`, `at.day`, `at.hour`, `at.minute`: integers.
- `at.weekday`: `"monday"` to `"sunday"`.
- `at.date`: for example `"2026-10-05"`. `at.time_of_day`: for example
  `"09:30"`. The `at.date` and `at.time_of_day` fields compare correctly as
  text.
- `at.time_zone`: the named IANA timezone, for example `"America/New_York"`.
- `at.in_time_zone(name)`: the same moment as an `at` value in another
  timezone, for teams that transfer on-call duties between people in
  different timezones. An unknown timezone is an error.

Two helpers count local calendar time: `days_since(start, at)` and
`weeks_since(start, at)` give whole local calendar days or weeks since a local
date (or date and time), rounded down, and negative before the start.
The `days_since` and `weeks_since` helpers count wall-clock days, so a 09:00
handoff stays at 09:00 across daylight saving changes. No additional helpers
are provided. There is no `load()` or randomness, and `print` output is dropped.
Starlark's defaults stay in force: no `while` loops and no recursion.

`on_call(at)` returns a dictionary with only the keys
`primary_sign_in_email_address` and `backup_sign_in_email_address`:

- `primary_sign_in_email_address` is required and is a member's sign-in email
  address, or `None` for "No one scheduled".
- `backup_sign_in_email_address` is optional. When the backup field is missing
  or `None`, the schedule's backup option applies: A fixed person or Everyone
  available. Next person in the rotation is not offered for Custom schedule.

The `primary_sign_in_email_address` and `backup_sign_in_email_address`
fields carry [sign-in email addresses](people.md#members-and-roles).
Matching addresses returned by `on_call(at)` ignores case and surrounding
space. An address outside the active team is an error that names the
address. When storing shifts, MMHotline turns the returned addresses into
stable member IDs.

Availability, taking on-call, the backup, and everyone available apply to the
stored answer exactly as for the built-in types.

### Working out the answers ahead

One sandbox run asks `on_call(at)` for assignments at quarter hours
(:00, :15, :30, :45 local time) over the next 14 days: 1,344 intervals.
MMHotline merges equal
neighboring answers into shifts and stores them in the database
(`custom_schedule_timeline`). MMHotline recomputes the answers every hour,
after every save, and right away when a member is removed.
Live coverage (`CoverageAt`), handoff notices, SOS incidents, and the on-call
page read the stored shifts, so an SOS never waits for custom schedule code.

15 minutes is the finest granularity for custom schedules: custom schedules
change only on the quarter hour. The quarter-hour limit is a documented limitation.
By day of the week and Weekly rotation keep handoffs at any minute.
By hour of the day uses whole-hour slots.

Daylight saving time follows the built-in types: a handoff happens once and
never switches back. When the clock goes back and an hour repeats, each local
time is asked about once, in the first pass. Through the second pass, the
person on call at the end of the first pass stays on call. For example, with
a handoff from Alice to Bob at 01:30 on the night the clock goes back from
02:00 to 01:00, Bob takes over at the first 01:30 and stays on call through
the repeated hour; Alice is not on call again. An hourly run that starts in
the second pass asks about the last quarter hour of the first pass too.
The hourly run retains the person on call at the end of the first pass.
Skipped local times are never asked about, so a handoff in a skipped time
happens at the next quarter hour that exists.

Sandbox runs happen on the server running `mmhotline serve`. A run that finishes after the
configuration changed is discarded: its result is committed only if the
configuration revision it started from is still current.

The hourly runs have their own background loop, apart from incident work, so a
sandbox run never delays an SOS. Every 5 seconds the loop checks whether the
team’s active schedule uses custom schedule code. The background loop detects
removed members or changed sign-in email addresses through a fingerprint of
the team's member IDs and sign-in email addresses. When the fingerprint
changes, the background loop runs the code right away. Removing a member
needs no separate step.

### When the code fails

- **At preview or save:** any error, exceeded limit, invalid result, unknown
  sign-in email address, or failed test case is shown, and the version cannot
  be saved. The error includes a source line or relevant local time when
  available. The current schedule stays
  active.
- **During an hourly run:** the stored shifts stay in force, because the
  saved custom schedule code produced the stored shifts. Administrators get
  one Warning, "MMHotline cannot work out who is on call: the custom schedule code failed",
  and a second Warning when
  fewer than 24 hours of stored shifts remain.
  When a run succeeds again, administrators get
  "The custom schedule code works again: MMHotline knows who is on call".
- **When the stored shifts run out:** the time counts as "No one scheduled".
  The backup is contacted first, then everyone available, then every member
  ([escalation](incidents.md#escalation-path)). The on-call page and the
  coverage reason say "Your custom schedule code has no answer for this time,
  so the backup is contacted first, then everyone available."

### Limits

The limits are named constants that bound resource use on the MMHotline server:

- Code: at most 64 KiB, with no control characters other than tab and line
  feed and no text-direction marks or overrides, so the code a reader sees
  is the code that runs. Test cases: at most 50.
- Work: at most 100,000 Starlark steps for each answer, and at most 100,000
  Starlark steps for loading the code.
- Time: 5 seconds of wall-clock time for the whole run. If the run exceeds
  the 5-second wall-clock limit, the parent process kills the child's process
  group. The child also has a 6-second CPU limit.
- CPU and memory: one CPU (`GOMAXPROCS=1`), a Go memory limit of 192 MiB, and
  `RLIMIT_DATA` of 256 MiB. Open files: `RLIMIT_NOFILE` of 8.
- Output: standard output at most 1 MiB; standard error at most 16 KiB, shown
  to the server administrator only.
- Concurrency: at most 2 sandbox runs at once in the installation.
  Preview and background work share the limit. The child lowers its whole
  process group to nice 19, so schedule work has low CPU priority.
- Runs: previews, saves, and activations together at most 60 per installation per
  hour, counted in process memory; the hourly runs do not count. Then:
  "You have run your code a lot in the last hour. Try again later."

### The sandbox

The `mmhotline` binary runs itself again as the hidden subcommand
`mmhotline custom-schedule-sandbox`. The child reads one JSON request on standard
input, writes one JSON response on standard output, and exits. The request
holds `code`, `time_zone`, `team_member_sign_in_email_addresses`, `asked_times`, `test_cases`,
and `max_steps_per_answer`. The response holds either `answers` or an `error`
with `kind` (`syntax`, `runtime`, `too_much_work`, `bad_result`, or
`unknown_sign_in_email_address`), `message`, `line`, and `asked_time`. The parent decodes the
response strictly and validates every answer again; it never trusts the
child's output. The parent decides from the exit status whether the time or
memory limit was exceeded: killed at 5 seconds, or ended by `SIGXCPU` or
`SIGKILL`, means "took too long"; a Go runtime exit (status 2) that reports
running out of memory means "used too much memory".

The parent also classifies a segmentation fault with exit status 2 as
"used too much memory". With Go 1.26's Green Tea garbage collector,
`RLIMIT_DATA` refusing the runtime's memory sometimes causes a segmentation
fault instead of "out of memory". Nothing else in the child can cause a
segmentation fault. The parent still writes the crash text to the service's
standard error for the server administrator.

Race-detector builds, which only tests use, raise `RLIMIT_DATA` for the
detector's shadow memory. Any other failure, such as exit status 3 when the
child cannot lock itself down, is a failure of
MMHotline, not of the code: the parent writes it, with the child's standard
error, to the service's standard error for the server administrator.

The protocol has two routine details. Each test case is sent as
`{"asked_time": ...}`, and the child answers test cases in
`test_case_answers`, beside `answers`; the parent compares the test-case
answers with what the
administrator expected. An `unknown_sign_in_email_address` error's `message`
is the sign-in email address itself.

On Linux the child is locked down before it runs any custom schedule code:

- The parent starts the sandbox child with an empty environment, working
  directory `/`, and only standard input, output, and error open.
  The child has a separate process group and dies when the parent dies.
- The child sets its resource limits (`RLIMIT_DATA`, `RLIMIT_CPU`,
  `RLIMIT_FSIZE` 0, `RLIMIT_CORE` 0, and a low `RLIMIT_NOFILE`), reads the
  whole request, sets `no_new_privs`, and installs a seccomp allowlist on all
  threads with the pure-Go `elastic/go-seccomp-bpf` (cgo off).
  The child holds one OS thread while setting `no_new_privs` and installing
  the allowlist, because the kernel requires `no_new_privs` on the installing
  thread. Any system call outside the allowlist, such as opening a file,
  opening a socket, or starting a program, kills the process.
- If seccomp cannot be installed, the run fails. A failed hourly run keeps the
  stored shifts.

The allowlist permits the Go runtime to run Starlark, write standard output,
and exit. The allowlist permits:

- Memory operations, but never executable memory: `mmap` and `mprotect`
  without `PROT_EXEC`.
- Futexes.
- Signals only to the child's own threads: `tgkill` to the child's process.
- Thread creation with `clone` only when `CLONE_THREAD` is set.
- `prctl` only to name the runtime's own memory (`PR_SET_VMA_ANON_NAME`).
  Go 1.25 and later name memory on kernels with memory-naming support,
  such as Ubuntu 26.04's.

The child calls `runtime.GOMAXPROCS(1)` first, which also stops the runtime
from re-reading the cgroup CPU limit, a file read the
lockdown forbids. Before the lockdown the child starts the
runtime's network poller, which the garbage collector's timers need, and
sets its local timezone to UTC so nothing reads `/etc/localtime` later. The
lockdown test runs on the Linux test servers for every Go upgrade and every
new target distribution, because a kernel feature can change what the
runtime calls.

On macOS, which is for development only, the child runs without seccomp,
without a parent-death signal, and without `RLIMIT_DATA` (macOS refuses it
and would not enforce it), so "used too much memory" is detected only on
Linux.

Starlark itself has no file, network, or process features. The sandbox is the
boundary that keeps code mistakes from hanging or crashing the service.

### Saving and version history

Applying the settings file saves the custom schedule by running the code
for the next 14 days and checking every configured test case.
Only a successful run can save and activate the configuration.
The save records the configuration, stored shifts, and audit change in one
team transaction. MMHotline rejects the save if the submitted
schedule revision differs from the team's current schedule revision.
Any failed run or test case leaves the active schedule and version history
unchanged, and the schedule part of `settings apply` says why.

MMHotline keeps the last 20 saved versions, newest first. Each version holds
its ID, save time, author member ID, and complete schedule configuration,
including the schedule revision, code, timezone, backup option, and test cases.
The active configuration stays in the schedule table; the timeline table
holds the active configuration's current shifts. Version history does not
duplicate the active configuration's current shifts.
The on-call page lists when each version was saved.

### Preview, test cases, and the page

`sudo mmhotline schedules preview --settings-file FILE` runs the file's code
for 14 days and its test cases, and prints the merged shifts, without saving
anything. A failed run or test case is refused with the failure message.

- Test cases, up to 50, each have `local_time`, the expected primary, and
  optionally the expected backup. Test cases are saved with the configuration
  and run on every preview and save.
- The on-call page shows the following information:

  - The saved code.
  - Test cases, with "Not checked" for an expected backup left out.
  - Stored shifts, with local time, UTC, primary, and backup.
  - "MMHotline knows who is on call until {time}".
  - The last failure.
  - The saved versions.

  The on-call page has no editor because the settings file sets the code.
  Every member sees the saved code, test cases, stored shifts, coverage end
  time, last failure, and saved versions on the on-call page.

Error messages:

- "Your code has a mistake on line {line}: {message}"
- "Your code did too much work for {local time}. Make it simpler."
- "Your code took too long to run."
- "Your code used too much memory."
- "on_call returned a sign-in email address that is not a team member's sign-in email address: {sign_in_email_address}"
- "on_call must return a dictionary like {"primary_sign_in_email_address": "ana@example.com"}"
- "Unknown key {key} in the result. Use primary_sign_in_email_address and backup_sign_in_email_address."

The team runs its own monitoring logic separately and connects the monitoring
logic through signed alerts or outbound webhook contact methods. Sandboxed
custom schedule code is an exception to keeping the team's logic outside
MMHotline.

### Routine choices for custom schedule code

The details in "Routine choices for custom schedule code" were chosen while
building custom schedule code. The [defaults review list](defaults.md)
links to "Routine choices for custom schedule code".

- **Timezones in the code.** `at.in_time_zone(name)` works for timezone names
  written in the code as quoted text, such as `"America/New_York"`: the
  locked-down child cannot read timezone files, so it loads those names (at
  most 50) before the lockdown. A name built at run time is an unknown
  timezone.
- **Test cases.** A test case first selects the quarter hour containing the
  test's local time, as live coverage does. If the selected local time is
  skipped, use the next valid minute. If the selected local time repeats,
  use the first occurrence. "Expected backup (optional)" is compared with
  the backup that applies: the code's `backup_sign_in_email_address`, or
  else the fixed backup.
  An empty "Expected backup (optional)" field shows "Not checked".
  A wrong backup shows "Failed: your code returned {name} as the
  backup". The first failed test case stops the save and is named, such
  as "The test case for 2026-11-21T10:00 expects the primary
  barbara@example.com, and the code gives carlos@example.com. Change the
  code or the test case, then preview again.", so the reader knows which
  of the two to change.
- **Test case times** look like `2026-10-05T09:30`, the local time in the
  schedule timezone.
- **More messages.** A mistake without a line number reads "Your code has a
  mistake: {message}". Too much work while loading reads "Your code did too
  much work before on_call was asked anything. Make it simpler." A mistake,
  an invalid result, or an unknown sign-in email address found while asking about one moment
  ends with "This happened for {local time}." A failure of MMHotline itself
  shows the ordinary "The operation could not be saved. Please try again.";
  in an hourly run it reads "MMHotline could not run the code this time. It
  tries again every hour."
- **Code text.** The code is stored as the settings file has it.
- **Failure notices.** All three notices ("MMHotline cannot work out who is on
  call: the custom schedule code failed", "MMHotline knows who is on call for
  less than 24 hours: the custom schedule code still fails", and "The custom
  schedule code works again: MMHotline knows who is on call") are Warnings to administrators, so the all-clear reaches
  the same contact methods as the failure. The schedule page also shows the
  last failure.
- **The page.** "MMHotline knows who is on call until {time}" uses the
  schedule timezone.

## Timezones and daylight-saving time

Use the schedule timezone to interpret local schedule rules and calculate
handoff boundaries. Store absolute instants in UTC and preserve the named
timezone plus local schedule rules. Compute boundaries from
the local calendar; adding 24 elapsed hours is not a substitute for “next local
09:00”. Display local conversions and label the two timezones when the timezones differ.

For a By day of the week or Weekly rotation handoff in a missing local minute,
hand off at the next valid minute. For example, a 02:30 handoff in a spring
transition that jumps from 01:59 to 03:00 occurs at 03:00. If the handoff
minute repeats, use its first occurrence and do not switch back when the clock
repeats.

By hour of the day schedules assign the same person to both occurrences of a repeated
hour. A missing hour has no elapsed interval and is skipped. Assigning
repeated hours to the scheduled person and skipping missing hours keep
the local week and day patterns intact without manufacturing extra shifts.
The live evaluator and preview use the handoff and missing-hour and
repeated-hour rules in "Timezones and daylight-saving time".
The binary includes the named timezone database. Custom schedule code
follows the handoff rules at quarter-hour granularity, retaining the last
assignment from the first pass throughout a repeated hour
([working out the answers ahead](#working-out-the-answers-ahead)).

## Preview and activation

The [on-call page](interface.md#schedule) lists who the active schedule
names in the coming weeks, handovers, or shifts. At each handover, the page
asks the live schedule evaluator which person the schedule names.
The page keeps the schedule's settings read-only in an expandable section,
with "Set in
/etc/mmhotline/mmhotline_settings.yml, in on_call_schedule" and the first
line of `/etc/mmhotline/mmhotline_settings.yml`.

`schedules preview` uses the built-in schedule evaluator used for live
coverage, or runs the custom schedule code. Preview evaluates explicit
instants, including cross-midnight and daylight-saving time cases.

Validate required fields, valid timezone/time ranges, user identities, backup
selection, and custom schedule code with its run and test cases. An empty
built-in slot is labelled “No one scheduled”. For an empty slot, use an
eligible fixed backup when configured, otherwise the eligible team; the
on-call page shows whom an SOS then wakes from the escalation code itself.
The schedule evaluator never carries forward the previous slot's person.
Save and activate the validated configuration in one team transaction,
using the team's schedule revision to reject a concurrent save.
Each save advances the team's schedule
revision and records the author, revision, and time in the audit history;
configurations do not have a separate activation-time field.

The on-call page's team list says what can wake each person at night
([people](people.md#what-can-wake-each-person)).

A scheduled-primary change produces one Info alert. Consecutive slots
assigned to the same person produce no extra handoff notice. Contact methods subscribed to `info` receive the handoff
alert under the [normal routing rules](notifications.md#routing-rules).
Schedule edits establish a new baseline and retain the audit entry.

Taking-on-call and personal-availability actions keep their own notices
instead of producing duplicate handoff alerts.

Persist the last observed assignment and schedule revision. After an
interruption, record at most one change to the current assignment. Never
replay all elapsed shifts.
A newly saved schedule revision has no past handoff to announce.
Handoff notices identify scheduled coverage and explain that taking on-call
and personal availability still apply.

## Acceptance evidence

Test exact boundaries, before/after the handoff, week wrap, adjacent
hour blocks, cross-timezone display, both DST transitions, an invalid
schedule refused with the previously active schedule kept, weekly rotation
order and start dates, all fallback paths, and user changes. Use explicit times rather than
sleeping through a shift.

For custom schedule code, also test:

- Stored 14-day shifts at quarter hours, including both daylight-saving
  transitions.
- Failures at preview, save, hourly run, and shifts running out.
- Every limit.
- A run discarded because the configuration changed while the run was in
  progress.
- Member removal.
- Immediate activation on a successful save.
- Failed runs and test cases leaving the schedule unchanged.
- Saved version history and the 20-version limit.

A helper-process test proves that opening a file, opening a socket, and
starting a program each kill the sandbox child. The helper-process test
runs on the Linux test servers for every Go upgrade, because the Go
runtime's system calls can change.
