# Contributing to MMHotline

Bug reports, ideas, and pull requests are welcome. `CONTRIBUTING.md`
explains how bug reports, ideas, and pull requests become changes to
MMHotline.

## Reporting a bug or asking for a feature

Open an issue on GitHub. For a bug, include:

- what `mmhotline version` prints, which names the release and its commit;
- the operating system and architecture of the server that runs
  `mmhotline serve`;
- the contact methods involved (email, outgoing webhook, Telegram message,
  Slack message, or automated voice call), and the triggering alert, with the alert's severity;
- what you expected, and what MMHotline did, with the messages that
  `mmhotline serve` printed and what the alert's or incident's page showed.

Leave out signing secrets, sending service credentials, and phone numbers.

For a feature, start with the problem: what you are trying to do, and how you
currently handle the problem. The [design plan](docs/changing-mmhotline/design/README.md) shows what MMHotline
intends to do and what is planned, and the
[product principles](docs/changing-mmhotline/design/README.md#product-principles) are what a proposal is
weighed against.

## Reporting a security problem

Do not open an issue for a security problem: everyone can read issues.
MMHotline holds your team's sending service credentials and decides who gets woken
at night. Report the problem privately with **Report a vulnerability** on the
repository's **Security** tab on GitHub. Only MMHotline's maintainers see the
report, and they answer in the private security report on GitHub.

## A pull request is a proposal

MMHotline's maintainers write every change that lands in MMHotline.
MMHotline's maintainers do not merge code from outside the project.
MMHotline decides whether an urgent alert reaches a person, so the origin
of every line is known. No code arrives from a source whose license,
authorship, or tools the project cannot check.

So a pull request is read as a proposal: a precise description of a change,
with code that shows the proposed change. When a maintainer accepts a
proposal, the maintainer writes the change, with its tests and documentation, and credits you in the
commit message:

```text
Proposed by @your-github-name in #123
```

The pull request is then closed with a link to the maintainer's commit
that implements the accepted proposal. A proposal that is not accepted is
closed with the reason.

A proposal is easiest to accept when it:

- changes one thing, and says why;
- includes a test that fails without the change, where a test is possible;
- names its settings, fields, and pages so that a reader of one line knows
  what they mean, as the rest of MMHotline does.

## Where the rules for changes live

| Document | What each document explains |
|---|---|
| [`AGENTS.md`](AGENTS.md) | How changes are made, for people and coding agents alike |
| [Acceptance contracts](docs/changing-mmhotline/acceptance.md) | The workflows MMHotline must get right |
| [Implementation rules](docs/changing-mmhotline/implementation-rules.md) | The rules for MMHotline's own code |
| [Testing guide](docs/changing-mmhotline/testing.md) | How to run the tests, and which tests a change needs |

## License

MMHotline is licensed under the [Apache License 2.0](LICENSE). Whatever you
submit in an issue or a pull request is offered under the Apache License 2.0,
as section 5 of the license says.
