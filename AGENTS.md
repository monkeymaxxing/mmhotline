# MMHotline development

[Design plan](docs/changing-mmhotline/design/README.md) owns product principles, status, and
remaining work. [Acceptance contracts](docs/changing-mmhotline/acceptance.md) define the workflows
required for completion and review. The [documentation map](docs/changing-mmhotline/documentation-map.md)
identifies each document's reader and subject.
Each behavior has one owning design document. Read the design document for
the behavior before changing code, tests, or requirements.

Apply the [Monkeymaxxing purpose](docs/changing-mmhotline/design/README.md#purpose-within-monkeymaxxing)
to implementation choices. Minimize the total work of building,
understanding, operating, and recovering MMHotline while meeting MMHotline's
real requirements.

[Implementation decisions](docs/changing-mmhotline/design/open-questions.md) are work to resolve
as each feature is built, not a queue of questions for the user. Choose sensible
defaults for reversible engineering details and ordinary product behavior,
consistent with the product principles.
Record the choice and a short reason in the design document for the affected
behavior. Update affected contracts and tests. Add an entry linking to the
recorded choice and reason in the [defaults review list](docs/changing-mmhotline/design/defaults.md).

Continue implementation without waiting for approval in the defaults review.
Do not add a configuration option merely because a default might change.

Ask about a choice only when it materially changes the product promise, scope,
team privacy, commercial commitments, or accepted data-loss guarantees.
Bring a recommendation with the concrete tradeoff. Historical alternatives are
references, not authority. Remove resolved questions and
superseded alternatives from `docs/changing-mmhotline/design/open-questions.md`.

[How MMHotline works](docs/how-it-works.md) explains components and workflows.
Follow [implementation rules](docs/changing-mmhotline/implementation-rules.md) and
[testing](docs/changing-mmhotline/testing.md). Run the whole Go suite with
`go test -timeout 30m ./...`; `internal/app` can pass `go test`'s
default 10-minute limit on a busy machine
([why](docs/changing-mmhotline/testing.md#test-in-levels)). [Validation](docs/changing-mmhotline/validation.md) records
the evidence for the current build.

## Public terminology

Use the notification design's terms:
[contact method and contact method type](docs/changing-mmhotline/design/notifications.md#contact-methods-and-subscriptions),
and [sending service](docs/changing-mmhotline/design/notifications.md#sending-service-setup).
Use the people design's
[sign-in email address](docs/changing-mmhotline/design/people.md#members-and-roles), never
"email address" alone for a person, and
[sign-in link](docs/changing-mmhotline/design/people.md#sign-in-links).

## Change policy

- An upgrade keeps the team's database and every row. A schema change adds
  tables and columns through an upgrade step, may rebuild one table to allow
  a new CHECK value, and never renames or removes anything
  ([database format and upgrades](docs/changing-mmhotline/design/operation.md#database-format-and-upgrades)).
- Implement the current agreed design directly. Delete superseded code and
  replace tests that assert discarded behavior.
- Do not add compatibility layers, legacy readers, deprecation paths, or
  cross-release matrices without a concrete requirement agreed with the user.
  Reset disposable test state.
- Test current workflows and meaningful failures. Keep verification
  proportional to the change; finish the initial feature before broad fault
  matrices and long soaks.
- Keep design, implementation, tests, and usage documentation consistent.
  Only mark a feature built when the required checks pass.
- Documents describe the current design and build. Remove completed plans,
  fixed findings, superseded evidence, and session recaps. Keep temporary
  evidence and task plans outside the repository.

## Implementation boundaries

- Write the application and built-in integrations in Go. Prefer the standard
  library and small explicit dependencies. Product builds use CGO_ENABLED=0,
  including all dependencies.
- The team’s logic runs on the team’s systems; MMHotline does not run
  the team’s executables, with four exceptions. Custom schedule code and the
  triage agent run in sandboxes.

  [Custom schedule code](docs/changing-mmhotline/design/scheduling.md#custom-schedule-code)
  runs in a locked-down Starlark sandbox.
  The team's coding agent runs as the
  [triage agent](docs/changing-mmhotline/design/triage-agent.md#the-fence), as a separate Unix
  user in a systemd sandbox.

  The third exception is the `mmx` program. MMX
  [runs the triage agent's playbooks](docs/changing-mmhotline/design/triage-agent.md#running-mmx-for-the-agent)
  as the `mmhotline` user under MMHotline's service restrictions.
  `mmx` receives an environment of only `HOME` and `PATH`.
  MMX runs, and the triage agent's copy holds, only commits signed by a key
  the team trusts (for example a person's desk, or util01's own signed
  commits pushed from a separate clone), the agent's own commits within
  `one_time_playbooks/` and `triage_agent_notes/`, and commits a person
  reviewed or, when the settings allow, the triage agent approved ([the commit check](docs/changing-mmhotline/design/triage-agent.md#checking-commits-before-mmx-and-triage-agent-runs)).

  The fourth exception is the command root sets in the settings file's
  `fleet_view_command`. Systemd runs the fleet view command as root in a
  separate restricted unit for the dashboard
  ([the fleet view socket](docs/changing-mmhotline/design/operation.md#the-fleet-view-socket)).
- Keep each behavior's logic in one place in the application.
  HTTP handlers, UI actions, and automation call the same application
  operations. Share notification scheduling and
  persistence across contact method types.
- Product design belongs in docs/changing-mmhotline/design/. Temporary task plans, review notes,
  and evidence stay outside the repository. Do not put changing project status
  in `AGENTS.md`.
