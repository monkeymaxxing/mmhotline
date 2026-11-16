# MMHotline documentation

Each page answers one question. Start with [Get started](get-started.md)
if you have not run MMHotline yet.

## Start here

- [Get started](get-started.md): install MMHotline on one server, put it
  behind Cloudflare Access, and send and claim your first SOS.
- [How MMHotline works](how-it-works.md): the parts, how an alert becomes
  an incident that wakes someone, and why each part is built the way it is.
- [When something fails](when-something-fails.md): a message or a
  surprise, what it means, and what to do.

## Install and look after the server

- [Install MMHotline](how-to/install.md): the release, the settings file,
  and `mmhotline install`.
- [Put MMHotline behind Cloudflare Access](how-to/put-mmhotline-behind-cloudflare-access.md):
  the team's sign-in in front of every page.
- [Change the settings](how-to/change-the-settings.md): edit the settings
  file, check it, and apply it.
- [Back up and restore the database](how-to/back-up-and-restore-the-database.md):
  a copy of every change off the server, a check that it keeps up, and a
  restore on the same or a new server.
- [Upgrade MMHotline](how-to/upgrade.md): install a newer release.
- [Sign in during a Cloudflare outage](how-to/sign-in-during-a-cloudflare-outage.md):
  reach the pages when Cloudflare Access is down.
- [Remove MMHotline](how-to/remove-mmhotline.md): take MMHotline off a
  server.

## People, contact methods, and the schedule

- [Set up people and the on-call schedule](how-to/set-up-people-and-the-on-call-schedule.md):
  who is on the team, who is on call, and who is the backup.
- [Write a custom schedule](how-to/write-a-custom-schedule.md): a schedule
  rule the built-in rotations cannot express.
- [Connect a sending service](how-to/connect-a-sending-service.md): choose
  among [Pushover](how-to/connect-pushover.md),
  [Twilio](how-to/connect-twilio.md),
  [a mail server](how-to/connect-a-mail-server.md),
  [Slack](how-to/connect-slack.md), and
  [Telegram](how-to/connect-telegram.md).
- [Add contact methods](how-to/add-contact-methods.md): how each person is
  reached, and for which severities.
- [Be away or take over on call](how-to/be-away-or-take-over-on-call.md):
  mark yourself unavailable, or take on-call from someone.

## Alerts and incidents

- [Connect a tool that sends alerts](how-to/connect-a-tool-that-sends-alerts.md):
  an alert source, its signing secret, and its check-ins.
- [Handle an incident](how-to/handle-an-incident.md): claim it, escalate
  it, and resolve it.
- [Declare planned downtime](how-to/declare-planned-downtime.md): silence
  an alert source while you work on its servers.
- [Mute the team](how-to/mute-the-team.md): stop every notification for a
  while, and what that risks.

## When MMHotline itself is down, and your customers

- [Set up the MMHotline-down alarm](how-to/set-up-the-mmhotline-down-alarm.md):
  someone is still woken when MMHotline's server dies.
- [Publish a status page](how-to/publish-a-status-page.md): a public page
  for your customers, in your own Cloudflare account.
- [Post to the status page](how-to/post-to-the-status-page.md): open,
  update, and resolve a notice, also from a laptop.

## The triage agent

- [Set up the triage agent](how-to/set-up-the-triage-agent.md): prepare
  the servers and the systems repository, and give MMHotline your team's
  Claude Code or Codex.
- [Decide what the triage agent may do](how-to/decide-what-the-triage-agent-may-do.md):
  which fixes run without a person, and approving or refusing the others.
- [Review a commit](how-to/review-a-commit.md): the commits that wait before MMX
  runs past them.
- [Turn on the scheduled software review](how-to/turn-on-the-scheduled-software-review.md):
  learn what one review costs, then set the interval.
- [Keep the agents signed in](how-to/keep-the-agents-signed-in.md): replace
  a credential before it ends.

## Reference

- [The mmhotline command](reference/mmhotline-command.md): every command,
  flag, exit code, and JSON field.
- [The settings file](reference/settings-file.md): every key of
  `/etc/mmhotline/mmhotline_settings.yml`.
- [Alert requests](reference/alert-requests.md): the format a tool uses to
  send alerts and check-ins, and how to sign them.
- [Severities and incident states](reference/severities-and-incident-states.md):
  sos, warn, and info, and how an incident moves from unclaimed to resolved.
- [Outgoing webhooks](reference/outgoing-webhooks.md): the request
  MMHotline sends to a webhook contact method, and how to check its
  signature.
- [Installed files](reference/installed-files.md): every file, user, and
  unit that `mmhotline install` creates.

## Changing MMHotline itself

- [Changing MMHotline](changing-mmhotline/design/README.md): the design,
  testing, validation record, and acceptance contracts, for people who
  change MMHotline's own code.
