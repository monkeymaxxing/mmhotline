# MMHotline documentation map

This map is for people changing MMHotline or its documentation. The map says
which document owns each subject. Start with the [project README](../../README.md)
for installation and use.

## The rules

- **One fact, one owner.** Put each fact's full explanation in the document that owns the fact. Other documents summarize and link to the owner.
- **Links in two directions.** When two documents depend on each other, link each document to the related document. Check the related documents when a shared fact changes.
- **The current build only.** User guides describe what the code does now. Proposed behavior belongs in the [design plan](design/README.md). Follow the [change policy](../../AGENTS.md#change-policy).
- **Names match the product.** Use the [public terminology](../../AGENTS.md#public-terminology) and the exact command names, flags, settings, and page labels.
- **Moves preserve information.** Move each fact to its owner before removing the original passage. Update every link to a moved section.

## Entry points

- [README](../../README.md): for people who have never heard of MMHotline: what MMHotline does, the table of the Monkeymaxxing tools (the same table in every Monkeymaxxing tool's README; change it in all of them together), why to use on-call software instead of an alerts channel, one example with real output, where MMHotline differs from PagerDuty and when another tool fits better, the whole settings file, the setup prompt and checklist for a coding agent, how to install and start, what to do when an SOS arrives, and links to the guides.
- [Agent guidance](../../AGENTS.md): terminology, change policy, implementation boundaries, and links to the owning documents.
- [Contributing](../../CONTRIBUTING.md): how to report a bug, propose an idea, or contribute a change.

## Owner documents

| Document | Owns | Reader |
| --- | --- | --- |
| [Documentation index](../README.md) | The list of every user guide and reference page, grouped by the reader's job. | A user looking for a page |
| [Get started](../get-started.md) | A first working team, from install to a claimed test SOS, with one worked example. | A team setting up MMHotline for the first time |
| [How-to guides](../README.md#install-and-look-after-the-server) | One page per job: installing, upgrading, backing up and restoring, changing the settings file, connecting each sending service and alert source, contact methods, people and the schedule, handling an incident, planned downtime, muting, the triage agent and its commit reviews and software review, keeping the agents signed in, the MMHotline-down alarm, the status page and posting to it from a laptop, and removing MMHotline. Each guide owns its job, such as [Connect Pushover](../how-to/connect-pushover.md) for Pushover and its phone settings. [Connect a sending service](../how-to/connect-a-sending-service.md) owns the steps every sending service shares and outgoing webhooks as a contact method type. | A team member or administrator doing one job |
| [Settings file reference](../reference/settings-file.md) | Every key in the settings file, its values, and the rules for the whole file. | An administrator or a coding agent writing the settings file |
| [Command reference](../reference/mmhotline-command.md) | Local commands including `settings check`, `apply`, and `show` with their output and exit codes, install, the commands that show the triage and review agents, sending services, the MMHotline-down alarm, people, schedules, alert sources, and rules for running the agent's playbooks, the status page's set-up on a laptop and status page posts, the read-only commands for incidents, alerts, triage sessions, and approval requests, flags, permissions, and JSON result fields. | A user, someone automating setup, or a coding agent on the server |
| [Installed files](../reference/installed-files.md) | The Unix users, programs, files, state folders, units, and sockets that `mmhotline install` makes, and who owns each. | An administrator checking or removing an installation |
| [Alert requests](../reference/alert-requests.md) | The alert intake and check-in requests a tool sends, their fields, signatures, answers, retries, and limits. | Someone connecting a tool that sends alerts |
| [Severities and incident states](../reference/severities-and-incident-states.md) | The alert severities, the incident states and how an incident changes state, escalation steps, and triage session states. | A user or a coding agent reading incidents |
| [When something fails](../when-something-fails.md) | Messages that MMHotline's pages and commands print, what each means, and what to do. | A user who sees a message or a symptom |
| [How MMHotline works](../how-it-works.md) | Main parts, where code runs, how data and credentials move, runtime boundaries, and a guide to the source files. | A user learning the system or a contributor reading code |
| [Local development](local-development.md) | Running without the systemd installation, development state and sockets, fake sending services, and local email testing. | Someone changing MMHotline's code |
| [Design plan](design/README.md) | Product purpose and principles, intended scope, feature status, and planned work. The design plan's design map names the document for each behavior. | A maintainer or contributor |
| [Design documents](design/README.md#design-map) | Requirements, behavior, decisions, and defaults for each part of the application. The [settings file](design/settings-file.md) design explains how MMHotline applies the settings file and the reasons for the settings file's design. | Someone changing behavior or reviewing correctness |
| [Acceptance contracts](acceptance.md) | Supported workflows and required outcomes for declaring a feature complete and reviewing the implementation against the contracts. | An implementer or reviewer |
| [Implementation rules](implementation-rules.md) | Code ownership, structure, dependencies, and implementation constraints. | Someone changing code |
| [Testing](testing.md) | Test commands, test levels, fixtures, and which checks prove each contract. The stand-in team member a coding agent plays in tests on a real installation. | Someone running checks |
| [Validation](validation.md) | Current verification evidence and its limits. | A user assessing the build or a reviewer |
| [Sources](sources.md) | External references and how they inform the current design. | Someone checking the design's basis |

## When a fact changes

Update the owner document first. Check the examples linked from the owner
document, the command reference, and any related how-to guide.
A behavior change also needs its owning design, affected acceptance contracts,
tests, and design status to agree.
When a heading moves or changes, check relative links and heading anchors
throughout the repository's documentation. Include README and agent guidance.
Messages from `mmhotline install` also contain documentation links, such as
the link to the settings file reference. Tests for the
documentation links in `mmhotline install` messages are in
`cmd/mmhotline/install_test.go`.
