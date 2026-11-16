# Specification sources

The [design plan](design/README.md) maps current requirements to their owning
documents and records feature status. Read the owning design before changing
behavior. The [acceptance contracts](acceptance.md) define
the outcomes that must hold; the [validation record](validation.md) records
which checks have passed and what those checks prove.

| Subject | Current owner |
| --- | --- |
| Signed alert fields, authentication, receipts, and limits | [Ingestion](design/ingestion.md) |
| SOS state, ownership, repeats, and escalation | [Incidents](design/incidents.md) |
| Contact methods, subscriptions, delivery attempts, and throttling | [Notifications](design/notifications.md) |
| Membership, sign-in, and permissions | [People](design/people.md) |
| Schedule types, timezones, previews, and custom schedule sandbox | [Scheduling](design/scheduling.md) |
| Unavailability, taking on-call, and team mute | [Availability](design/availability.md) |
| Check-ins and planned downtime | [Monitoring](design/monitoring.md) |
| Screens and actions | [Interface](design/interface.md) |
| Storage and restart recovery | [Operation](design/operation.md) |
| Components and contributor rules | [Architecture](../how-it-works.md), [implementation rules](implementation-rules.md) |
| Executable checks and acceptance evidence | [Testing](testing.md), [validation](validation.md) |

[Planned](design/README.md#planned) is the list of planned product work.
Current designs and executable tests are the reference for the current
MMHotline build. Private drafts and other applications are not requirements
for MMHotline.
