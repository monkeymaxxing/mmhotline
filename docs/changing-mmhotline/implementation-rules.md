# MMHotline implementation rules

MMHotline's implementation rules apply to every change. The
implementation rules express ownership and correctness constraints, not claims
about code that already exists. The
[acceptance contracts](acceptance.md) define the required
outcomes; the [design map](design/README.md#design-map) identifies each owner.

[How MMHotline works](../how-it-works.md) explains the current components and
workflows. The [design index](design/README.md) records implementation status.

## One application operation per state change

HTTP form handlers, automation, timers, and
[application code accepting custom schedule
results](../../internal/app/custom_schedule.go) call shared application operations.
HTTP handlers parse, authenticate, authorize, invoke, and render. HTTP handlers
do not implement another copy of escalation or notification routing.

Keep the terms for incident transitions, schedule evaluation, responder
eligibility, and delivery outcomes explicit. Define each set of states and
allowed transitions in one application component responsible for the behavior.

Preserve the distinction between an alert, an incident, notification intent,
an attempt, sending service acceptance, and human acknowledgement.

## Commit before success or external effects

Persist accepted alerts and the state changes made by accepted commands in
`mmhotline.db` before returning success. Commit the application state change
and the associated notification intent together. Failed commands do not partially activate
configuration or silently mutate live state.

An unchanged check-in is the only exception to persisting accepted input before
returning success. Record a check-in that changes nothing about the alert
source in memory, because the next check-in replaces the unchanged check-in
30 seconds later
([check-in tracking in memory](design/monitoring.md#check-in-tracking-in-memory)).

No network call, custom schedule code child process, or long-running
observation holds a state lock or database transaction. Reserve work durably,
copy the reserved work's execution inputs, then perform the bounded external
action. Commit the external
action's result through the application operation responsible for the reserved
work. Each operation commits through `Store.WithTransaction` in the one
`mmhotline.db` database ([storage](design/operation.md#storage)). The store
holds one connection; the SQLite transaction is the lock. Do not replace
transactional state with an application-wide snapshot guarded by a daemon mutex.

Check-in tracking and the scheduler record keep facts from the team's
database in memory
([operation](design/operation.md#what-mmhotline-serve-keeps-in-memory-and-rebuilds-at-start)).

Take the check-in tracking lock before the team's transaction, never inside a
transaction. Hold the check-in tracking lock across transactions only for
coordinated changes to stored and in-memory check-in state.
Update copied database facts in memory after the database commit, never before
the database commit.

The scheduler record has a separate lock. Release the scheduler record lock
before reading or writing the database. After each committed database change,
increment the scheduler's change count. The scheduler record saves each
visit's starting database change count. If the database changes during a
visit, the scheduler runs another visit next round to process the changed state.

## Revalidate asynchronous work

Give background work explicit identities for the relevant alert, incident,
configuration, and target. Carry revisions for state changes that can
invalidate the work.

The application operation responsible for background work rechecks validity
before dispatch and when the external action returns.

A late completion can record that a message was sent. A late completion cannot
resurrect a resolved incident, restore a previous person's incident claim, or
overwrite a new configuration. Copy background-work inputs while holding the
database transaction or the lock protecting the in-memory state.

## One notification scheduler

Alert notification routing saves notification intent for the selected contact
methods in `mmhotline.db`. The shared delivery scheduler owns deadlines,
attempts, retries, cancellation, and limits.

Contact method type adapters perform one attempt and report known results.
SOS repeat policy belongs to the
[incident application code](../../internal/app/incidents.go), which handles
incident transitions.
The incident component creates explicit notification intents for repeats.
Repeat policy does not live in an adapter's private loop.

Keep one scheduler record for the installation's one team. The scheduler visits
after startup, a database change, a due deadline, check-in work, or 10 minutes
without a successful visit.

The periodic visit bounds the effect of a wrong deadline. Retry a failed visit
next round and refresh the team's facts after every visit. Do not write
unchanged state during an idle visit: continuous backup would upload the
unchanged-state writes forever.

Run custom schedule code in a separate loop, using the team's facts, so a
sandbox run never delays incident work.

Use controllable time and clear deadlines. Evaluate schedule timezone/calendar
rules in one place and reuse the rules for previews. Use one responder eligibility function for
coverage, routing, and dispatch at the required decision points.

Team mute is a team delivery gate, separate from personal availability and
coverage selection. The no-coverage fallback cannot bypass team mute.

Coverage changes never silently transfer incident ownership. Recovery schedules
current actionable work rather than replaying missed timer intervals. The
owning rules are in [availability](design/availability.md) and
[operation](design/operation.md#recover-current-work).

Read and change live team state through the shared application operations in
the `mmhotline serve` process.

## Validate boundaries, preserve uncertainty

Validate untrusted input before admission, including authenticated input. Use
typed structures, strict protocol decoding, bounded reads, and
operation-specific limits. Custom schedule code runs in a locked-down Starlark
sandbox. Saving custom schedule configuration requires a successful 14-day
preview and passing test cases.
The `mmhotline serve` process validates every answer from the custom schedule
sandbox child process.

A timeout or lost sending service response is not proof that no side effect
occurred.

Enforce authorization at the operation boundary. Keep secrets out of
diagnostics. Keep secrets out of URLs whenever the interface permits sending
secrets outside the URL. Keep secrets out of process arguments and browser
data. Escape/render text for the output context. Keep database, sending service,
and schedule-code execution boundaries small enough for failure tests without
a separate testing framework.

## Minimal structure

Prefer concrete functions/structs and standard-library facilities. Add an
interface at a real external or testable boundary, not at every package. Keep
helpers with their owner until another real caller needs the helpers. Create packages,
commands, and configuration knobs only when required by a workflow.

Persist only facts needed to recover work, apply product rules, or diagnose
what happened. Derive presentation from the persisted recovery, policy, and
diagnostic facts instead of storing presentation separately. Bound workers, captured output,
pending work, and retained history; the
specific limits and their reasons belong in the owning design.

## The product's name inside other names

Inside every other name, the product is `mmhotline`, written as one word:
commands (`mmhotline serve`), files (`mmhotline_settings.yml`), development
environment variables (`MMHOTLINE_FAKE_SENDING_SERVICES_URL`), request and
response headers (`Mmhotline-Signature`), JSON fields (`mmhotline_alert_id`),
cookies, database columns, and Go names. Use the whole product name inside
other names so each name clearly identifies the product. Text for people says
"MMHotline". Integration examples also use the whole word `mmhotline` inside
names that belong to MMHotline.

Three kinds of names follow other rules: domain names (`mmhotline.example.com`),
the Go module path `mmhotline`, and phone-network caller names,
whose format the phone networks set (the caller name "MMHOTLINE").

## Review and verification

Review changed paths against MMHotline's implementation rules and a concrete
acceptance contract. Use
focused interleaving tests for claim/resolve versus dispatch, configuration
replacement versus custom schedule code completion, and availability changes
versus deadlines. Run the smallest meaningful workflow that proves the public
behavior. Remove obsolete tests when the agreed design changes.
