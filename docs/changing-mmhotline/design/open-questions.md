# Implementation decisions

This register contains unresolved requirements for release and recovery.
Settled behavior belongs in the design document that owns it. The
[defaults index](defaults.md) links to current choices; the
[design index](README.md#planned) lists planned product work.

## Q10 Recovery, retries, and retained history

[Operation](operation.md#recover-current-work) defines restart recovery.
[Notifications](notifications.md#delivery-records) defines delivery attempts,
retry identity, and uncertain outcomes.

History retention is not defined. A release must state which alerts, audit
entries, and delivery attempts are retained.
[Operation](operation.md#continuous-backup-and-restore) defines optional
Litestream backup, restore effects, and the absence of fixed recovery-time
or data-loss targets. A process restart and an older-database restore remain
different recovery paths.

## Q15 Platforms, deployment, and operating envelope

[Development](development.md) defines cgo-disabled builds and Linux sandbox
checks. Release support must name the supported operating systems and
architectures and measured operating envelope:
team size, incident rate, notification fan-out, sending service outages, and recovery
targets. A build alone does not establish support for a platform or workload.

## Q16 Release

MMHotline is licensed under the [Apache License 2.0](../../../LICENSE).
[Development](development.md#release-archives) defines the version scheme
and the release archives; the [Install MMHotline](../../how-to/install.md)
downloads them from the repository's GitHub releases. Compatibility
obligations from the first release remain undecided.
