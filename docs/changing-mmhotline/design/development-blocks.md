# Development ownership

MMHotline runs as one Go process for one team. The current build uses one
SQLite database, `mmhotline.db`. Shared application operations
own mutations. One durable notification outbox owns delivery work.
[Architecture](../../how-it-works.md) explains the components;
[implementation rules](../implementation-rules.md) explain how to change them.

## Application boundary

HTTP handlers, interface actions, and automation call the same application
operations. A team transaction records an accepted alert, the alert's incident
change, and the alert's delivery intent together. No handler recreates the
application operations' rules or queues notifications after committing the alert.

## Shared boundaries

- Stable member, alert, and incident IDs preserve attribution. Authorization
  for a mutation reads membership inside the transaction that makes the change.
- The delivery-intent operation uses the caller's team transaction. Notification
  delivery never depends on a second commit succeeding later.
- Coverage evaluation supplies eligible responders and suppression reasons.
  Team mute remains a separate gate. Coverage evaluation sends no notifications.
- Contact method type adapters make one attempt. Shared delivery code owns
  reservations, retries, and known or unknown outcomes. Incident operations
  own escalation and SOS repeats.
- Explicit time inputs make handoffs, deadlines, and recovery testable without
  waiting for the clock. All application work runs on the same server.
- The custom schedule child process returns schedule answers. The parent
  MMHotline process validates the returned answers before converting the
  answers into shifts for storage. Incident work reads stored shifts and
  never waits for user-written schedule code.

Keep transaction, authorization, coverage, delivery, incident, time, and
schedule-code responsibilities concrete. Reuse existing operations before
adding a layer, and do not create tables or abstractions for unbuilt features.

## Integration and remaining gates

Focused tests cover each operation. Integration tests cover the boundaries:
sign in, submit a signed SOS, inspect simulated delivery, claim or resolve from
two browsers, restart, and verify the stored state. Browser checks cover
keyboard navigation and mobile layouts.

Real sending service acceptance and actual receipt remain separate checks
requiring configured sending services and designated contact methods.
A fake sending service result never proves real sending service acceptance
or actual receipt.
[Validation](../validation.md) records the evidence;
the [design index](README.md#planned) lists planned product work.
