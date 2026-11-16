# Review a commit

**Who acts:** any person on the team reviews a commit on the Approvals
page. An administrator with `sudo` on the MMHotline server sets the
trusted keys and runs the commands on this page.

**When to use this page:** when an alert says a commit of the systems
repository waits for review; when you set up which commits the team
trusts; or when a scheduled job must not apply a commit nobody trusts.

**What this page does:** you tell MMHotline whose signed commits pass, let
the triage agent review the others, and review a commit that waits for a
person.

**Terms:**

- A **trusted key** is a person's or a machine's SSH signing key, listed in
  the settings file. A commit one of them signed passes at once.
- A **commit that waits** is any other commit: an unsigned one, such as from
  the Git host's merge button, or one signed by a key the settings file
  does not list, such as an attacker's. No MMX run goes past it until it is
  reviewed. The triage agent works from the commit before it, and so does
  MMHotline's own copy: the dashboard's recent changes, `mmx nodes`, and
  `mmx explain` see nothing of the waiting commit until it is reviewed.
- The **commit review** is a triage agent run that reads each waiting
  commit's message, changed paths, and diff, and approves it or hands it to
  a person.
- The **trusted commit** is the newest commit through which every commit
  passed or was reviewed.

MMHotline checks each new commit before every MMX run and at the start of
every triage agent run. The triage agent's own commits pass when its key
signed them and they change only `one_time_playbooks/` and
`triage_agent_notes/`. The
[triage agent design](../changing-mmhotline/design/triage-agent.md#checking-commits-before-mmx-and-triage-agent-runs)
explains the check.

## Choose the route

- **Set up which commits pass:** [Set up](#set-up).
- **Old commits wait, such as in a repository older than MMHotline, or
  after an upgrade or a restore:**
  [Vouch for the history once](#vouch-for-the-history-once).
- **An alert says a commit waits:** [Review a commit that waits](#review-a-commit-that-waits).
- **Nobody on the team made the commit:**
  [When nobody on the team made the commit](#when-nobody-on-the-team-made-the-commit).
- **A scheduled job applies the repository:**
  [Check before a scheduled job applies](#check-before-a-scheduled-job-applies).

## Before you start

- The triage agent is set up, as
  [Set up the triage agent](set-up-the-triage-agent.md) says. Reason: the
  trusted keys are part of its `triage_agent` section; without the
  section, every MMX run and every commit check is refused.
- Each person, and each machine that commits, such as `util01`, signs
  commits with an SSH key. On each person's computer:

  ```sh
  git config --global gpg.format ssh
  git config --global user.signingkey ~/.ssh/id_ed25519.pub
  git config --global commit.gpgsign true
  ```

  **Check:** a new commit shows `Good "git" signature` in
  `git log --show-signature -1`, once Git knows the allowed signers.
- `triage_agent_rules.md` is at the top of the systems repository. Reason:
  without it, the agent hands every commit to a person.

## Set up

1. **List the keys and choose who may approve the others,** in the
   `triage_agent` section of the settings file:

   ```yaml
   triage_agent:
     trusted_commit_signing_public_keys:
       - "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKkV3VQGyLmAYtl/HR7x93Qw22ugM6k9OhTepuWlHFPr Barbara's desk"
       - "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPY0ynRIHnG58nyyazSZJWIqQ0zGxWHo5AF4GMnzX3fM util01"
     commits_without_a_trusted_signature_are_approved_by: a_person_or_the_triage_agent
     paths_only_a_person_may_approve:
       - mmhotline_settings.yml
       - triage_agent_rules.md
       - AGENTS.md
       - roles/every_server/files/sudoers_triage # triage's sudo rules on the servers, wherever your repository keeps them
     # ...the rest of the section
   ```

   - `trusted_commit_signing_public_keys`: one public key line for each
     person's desk and each machine that commits. When MMHotline's copy
     signs its own merges, list that key too. Never list the triage
     agent's key here.
   - `commits_without_a_trusted_signature_are_approved_by`:
     `a_person_or_the_triage_agent` lets the agent approve an ordinary
     change, such as a merge from the Git host's merge button. `a_person`
     leaves every such commit to people.
   - `paths_only_a_person_may_approve`: the files that set the agent's own
     rules and powers, such as its sudo rules, and the settings file that
     holds the trusted keys. An entry ending in `/` is a folder. Keep the
     list short; the agent judges every other change.

2. **Apply the file:**

   ```sh
   sudo mmhotline settings apply
   ```

   **Check:** among its lines:

   ```text
   triage_agent.commit_signing_public_key, triage_agent.trusted_commit_signing_public_keys: applied.
   triage_agent.commits_without_a_trusted_signature_are_approved_by, triage_agent.paths_only_a_person_may_approve: applied.
     triage_agent.commits_without_a_trusted_signature_are_approved_by: The triage agent reviews each commit no trusted key signed and approves an ordinary change; a person reviews the triage agent's own commits outside its folders, and every commit that changes a path of triage_agent.paths_only_a_person_may_approve.
   ```

   MMHotline uses only the applied keys and setting.

3. **Check the newest commit:**

   ```sh
   sudo mmhotline commit-reviews check --commit-id a4fce954720b97d948bdb9f80640cbc5ee5ede5e
   ```

   Give the full commit ID. The command fetches `origin` into
   MMHotline's copy and checks every commit through it.

   **Check:** it exits 0 and says:

   ```text
   Checked commit a4fce954720b97d948bdb9f80640cbc5ee5ede5e.
   Trusted through commit a4fce954720b97d948bdb9f80640cbc5ee5ede5e: every commit through it passed the check or a person reviewed it.
   ```

   A new systems repository signed from its first commit passes with
   nothing to do. If older commits wait, vouch for them once, as below.

### Vouch for the history once

The first check reads the whole history against today's keys. A system
that already runs has commits from a person who left, or early unsigned
commits, and all of them would wait. Vouch for that history once, with a
commit of `origin`'s branch, such as the one a person's own run from a
desk just brought:

```sh
sudo mmhotline commit-reviews trust-through --commit-id 542076eb7fada68b9ab0785ad3adb41f20d98fde
```

**Check:** it exits 0 and says:

```text
Checked commit 542076eb7fada68b9ab0785ad3adb41f20d98fde.
Trusted through commit 542076eb7fada68b9ab0785ad3adb41f20d98fde: every commit through it passed the check or a person reviewed it.
Marked every commit through 542076e trusted, and 1 commit that waited for review as reviewed.
```

The team gets one Info alert, such as
`root on util01 marked every commit through 542076e trusted`. Later
commits go through the check as usual. Running it again for the same
commit changes nothing and says
`Every commit through 542076e is already trusted: the checked-through commit 542076e holds it. Nothing changed.`

Run it after a person's first run on a new or rebuilt server, after an
upgrade of MMHotline to a release with this check, and after MMHotline's
database was reset or restored from an older backup.

## What happens to a commit that waits

With `a_person_or_the_triage_agent`, the triage agent reviews each commit
no trusted key signed, in a commit review, and decides. Each commit gets
exactly one alert.

- **The agent approves an ordinary change.** The team gets one Info alert,
  such as
  `The triage agent marked commit 13f91dd reviewed: Approve 13f91dd: one-time playbook to start lab-web.service on web1, plus a triage note`,
  and nobody needs to act. MMX runs go past the commit. In a real run,
  for a merge-button commit:

  ```text
  Reviewed by the triage agent at 2026-11-15T08:53:58Z.
  The triage agent approved the commit at 2026-11-15T08:53:58Z, in commit review run fe0fd5409c17b54ddbcfafb755285f48. Its report:
  Approve: PR 9 merge adds /srv to the read-only disk check playbook
  ```

- **The agent hands the commit to a person,** with its report at the level
  it chose: **sos**, which wakes the person on call, when the commit
  changes who can get in or looks like nobody on the team made it; **warn**
  when it is unsure. In a real run, a merge whose author name differed from
  the Git host's own merges went out at sos:

  ```text
  The triage agent decided that a person must review the commit, with its report at alert severity sos at 2026-11-15T08:53:05Z, in commit review run ad5b8bc55f39b6b7e802e189619ee356. Its report:
  Commit b2647ce: harmless /var disk check, but the merge author name does not match the Git host's
  ```

- **A commit the agent may not approve** goes straight to a person, with
  MMHotline's warning, such as
  `Review needed for commit 542076e, which no key the team trusts signed`.
  That is every commit with `a_person`, every commit that changes a path of
  `paths_only_a_person_may_approve`, and the agent's own commit that
  changes paths outside its two folders. The commit's page says why, such
  as
  `Needs review: AGENTS.md is on triage_agent.paths_only_a_person_may_approve, so only a person may approve the commit.`
- **When the agent cannot review the commit,** because its run failed or
  ended without a decision, MMHotline sends the same warning, once, with
  the reason.

While the agent reviews a commit, the dashboard lists it under **The
triage agent reviews**. Once a person must decide, it moves to **Needs a
person**. **Approvals** lists the commits the agent approved, newest
first, so a person can read what the agent let through.

## Review a commit that waits

1. **Open the commit's page:** the alert links it, or open **Approvals**,
   then **Review** under **Commits waiting for review**. On the server,
   `sudo mmhotline commit-reviews list` lists the waiting commits, and
   `sudo mmhotline commit-reviews show --commit-id COMMIT_ID` shows one with
   its diff and the agent's report.

2. **Read the commit facts, the agent's report, and the Commit diff.** For
   the agent's own commit, read every path and reason. When the page says
   the diff was cut, read the whole diff in your clone of the systems
   repository.

3. **If a change is wrong,** revert it with your own signed commit first,
   and push the revert.

4. **Press Mark commit reviewed.** On the server:

   ```sh
   sudo mmhotline commit-reviews mark-reviewed --commit-id 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1
   ```

   **Check:** the commit's page, or the command, names the reviewer, such
   as `Reviewed by the mmhotline command, run by root at 2026-11-15T08:52:39Z.`,
   and MMHotline's warning resolves.

The review lets MMX runs and the agent's next run go past the commit. It
starts no playbook run: the agent asks again in its next run.

## When nobody on the team made the commit

If nobody on the team made a commit that no trusted key signed, someone may
have broken in. **Do not mark it reviewed.** MMX runs stay stopped before
it, and the agent keeps working from the commit before it. Follow your
team's break-in procedure.

## Check before a scheduled job applies

A scheduled job that applies the systems repository, such as on `util01`,
asks MMHotline before it applies a commit, and applies only through the
trusted commit:

```sh
sudo mmhotline commit-reviews check --commit-id 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1
```

**Check:** exit 0 means every commit through it passed. Exit 1 means a
commit waits; the job applies only through the commit named on the
`Trusted through commit` line:

```text
Checked commit 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1.
Trusted through commit a4fce954720b97d948bdb9f80640cbc5ee5ede5e: every commit through it passed the check or a person reviewed it.
Commit 5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1, which no key the team trusts signed.
Author: Git host <noreply@githost.example>. Commit date: 2026-11-15T08:51:26Z. Found by MMHotline: 2026-11-15T08:51:26Z.
Message: Merge pull request 7: add a disk check playbook
Waits for review at https://ops.example.com/approvals/commits/5c281a5cc65bb8cb9a6ecbe9d2e4f8bd524453c1.
1 commit waits for a person's review at https://ops.example.com/approvals.
```

With `--json`, the trusted commit is `commit_check.trusted_through_commit_id`.
The job must not hold MMHotline's lock,
`/var/lib/mmhotline/systems_repository.lock`, while it calls the command,
because the command takes that lock to fetch. The
[command reference](../reference/mmhotline-command.md#mmhotline-commit-reviews-check) lists every
field and exit code.

## If something is different

- **The agent's report says**
  `... a person must review it: my rules file is missing`: the systems
  repository has no `triage_agent_rules.md` in a trusted commit. Add it,
  signed by a trusted key, as
  [Set up the triage agent](set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository)
  says, then review the waiting commit.
- **The agent's own commits wait for review,** and its report says
  `my start playbook waits for a person to review my commits`: the agent
  does not sign its commits. Set up its `.gitconfig`, as
  [Set up the triage agent](set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository)
  says, step 3.
- **Every MMX run and `commit-reviews check` is refused** and says so: the
  settings file has no `triage_agent` section, such as during a lock-out,
  so no keys apply. After the section returns, the check goes on from
  where it stopped.
- **`check` or `trust-through` is refused for the commit ID:** MMHotline's
  copy lacks that commit after the fetch, or, for `trust-through`, the
  commit is not on `origin`'s branch. Push it first, and give the full ID.
- **The command waits and does not return:** an MMX run, or a person's
  helper, holds `/var/lib/mmhotline/systems_repository.lock`. The command
  goes on once the lock is free.
