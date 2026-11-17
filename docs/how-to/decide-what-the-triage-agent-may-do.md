# Decide what the triage agent may do

**Who acts:** an administrator with `sudo` on the MMHotline server sets
the rules. Any person on the team approves, refuses, or stops a playbook
run.

**When to use this page:** to choose which fixes MMHotline runs for the
triage agent without asking anyone, which ones it runs after a warning, and
which ones wait for a person; and when a fix waits for you.

**What this page does:** you give each inventory a rule for each kind of
action, list the services nobody may touch without a person, check a
playbook against both, and approve, refuse, or stop the agent's runs.

**Terms:**

- A **playbook** is an MMX file of tasks to run on servers. The agent
  writes a **one-time playbook** for one problem, such as
  `one_time_playbooks/2026-11-15_start_lab_web_on_web1.yml`, or asks for
  one of the team's own, such as `playbooks/restart_lab_web.yml`.
- A **kind of action** is what MMHotline counts a task as, from what
  `mmx explain` says the task does: `start_or_restart_a_service`,
  `free_disk_space`, `pull_a_container_image_again`, `reboot_a_server`,
  `block_a_network_range`, `change_dns_records`, `check_only`, and
  `unrecognized_task`, any task MMHotline cannot match to a kind.
- A **rule** says what MMHotline does with a fix of that kind:
  - **Get permission, then act** (`get_permission_then_act`): wakes the
    person on call with an SOS and runs only after 1 or 2 people approve.
  - **Warn, wait, then act** (`warn_wait_then_act`): sends a warning, waits
    1 to 1440 minutes, and runs unless someone presses Stop.
  - **Act, then report** (`act_then_report`): runs at once; the agent
    reports what it did.
- A **preset** sets every kind of one inventory at once.
- **Services whose changes a person must approve** are systemd units that
  no playbook may start, stop, restart, or change without a person, on
  every inventory.

A playbook takes the strictest rule of its kinds. When a review agent is
set up, it must approve a playbook before any rule applies, and a playbook
it refuses three times in a row waits for a person. The
[triage agent design](../changing-mmhotline/design/triage-agent.md#rules-for-running-the-agents-playbooks-and-kinds-of-action)
says how MMHotline matches each task to a kind.

## Choose the route

- **Set the rules:** [Set the rules](#set-the-rules).
- **See what the rules do to a playbook:**
  [Check whether a playbook needs a person](#check-whether-a-playbook-needs-a-person).
- **A fix waits for your approval:**
  [Approve or refuse a playbook run](#approve-or-refuse-a-playbook-run).
- **A warning says a fix will run, and it should not:**
  [Stop a playbook run before it runs](#stop-a-playbook-run-before-it-runs).

## Before you start

- The triage agent is set up, as
  [Set up the triage agent](set-up-the-triage-agent.md) says.
- `mmx` 0.1.0 or later is in `/usr/local/bin` on the MMHotline server.
  Reason: MMHotline judges every playbook by what `mmx explain` lists, never
  by its name or by what the agent says about it.
- You know the inventories in the systems repository, such as
  `inventories/production.yml`. Reason: every inventory needs an entry.

## Set the rules

1. **Give each inventory a preset** in the settings file, with
   `sudoedit /etc/mmhotline/mmhotline_settings.yml`:

   ```yaml
   rules_for_running_agent_playbooks:
     production:
       preset: routine_fixes_without_permission
   ```

   | Kind of action | `permission_for_every_fix` | `routine_fixes_without_permission` | `every_fix_without_permission` |
   | --- | --- | --- | --- |
   | Start or restart a service | Get permission (1) | Act, then report | Act, then report |
   | Free disk space | Get permission (1) | Act, then report | Act, then report |
   | Pull a container image again | Get permission (1) | Act, then report | Act, then report |
   | Reboot a server | Get permission (1) | Get permission (1) | Act, then report |
   | Block a network range | Get permission (1) | Get permission (1) | Act, then report |
   | Change DNS records | Get permission (1) | Get permission (1) | Act, then report |
   | Check only | Act, then report | Act, then report | Act, then report |
   | Unrecognized task | Get permission (1) | Get permission (1) | Act, then report |

   `routine_fixes_without_permission` is the recommended start. MMHotline
   runs the routine fixes that cannot lose data, and asks a person before
   anything that can make a server, a network, or a service unreachable, or
   that MMHotline cannot recognize.

   A routine fix is narrow. Start or restart a service starts only a plain
   service, such as `nginx` or `shop-worker.service`, never a target such as
   `poweroff.target`, a `systemd-*` unit, or the operating system's own
   power, shell, network, firewall, or update services. Free disk space keeps
   at least one day and 100 MiB of the journal, and removes only images with
   no name left. Anything else is an unrecognized task, which a person can
   approve. To make a person decide every change to another service, list
   the service in step 2.

   **`every_fix_without_permission` GIVES ROOT ON EVERY SERVER OF THE
   INVENTORY TO ANYONE WHO CAN PUT TEXT IN FRONT OF THE AGENT.**

   The agent reads alerts and logs, and anyone on the internet can put a
   line in a web server's log. Text that misleads the agent can then make
   it run any playbook without a person: any command as root on every
   server, or a task that copies MMHotline's own database to a server,
   where the agent reads it. The database holds the sending services'
   tokens and the agents' credentials. When MMHotline's own server is in
   the inventory, the agent can also claim and resolve incidents there with
   `mmhotline incidents claim` and `resolve`, which stops MMHotline
   contacting people about them. The same holds for
   `unrecognized_task: {rule: act_then_report}`. A review agent lowers the
   chance, but does not rule it out. Use this preset only for an inventory
   where that is acceptable, such as a test inventory.

   **To choose each kind yourself,** give the inventory `action_kinds`
   instead of `preset`:

   ```yaml
   rules_for_running_agent_playbooks:
     production:
       action_kinds:
         start_or_restart_a_service: {rule: act_then_report}
         free_disk_space: {rule: warn_wait_then_act, warning_wait_minutes: 10}
         pull_a_container_image_again: {rule: act_then_report}
         reboot_a_server: {rule: get_permission_then_act, approvals_needed: 1}
         block_a_network_range: {rule: get_permission_then_act, approvals_needed: 1}
         change_dns_records: {rule: get_permission_then_act, approvals_needed: 2}
         check_only: {rule: act_then_report}
         unrecognized_task: {rule: get_permission_then_act, approvals_needed: 1}
   ```

   A kind you leave out needs a person, with 1 approval, until you add it.
   Reason: a missing rule must never let a fix run without a person.

2. **List the services nobody may touch without a person,** such as a
   database service that a person may stop on purpose:

   ```yaml
   services_whose_changes_a_person_must_approve: [pgautofailover.service]
   ```

   Use `[]` for none. A playbook that starts, stops, restarts, or changes a
   listed service waits for a person on every inventory, whatever its
   rules.

3. **Apply the file:**

   ```sh
   sudo mmhotline settings apply
   ```

   **Check:** among its lines:

   ```text
   rules_for_running_agent_playbooks: applied.
   services_whose_changes_a_person_must_approve: applied.
   Every part of the settings file is applied.
   ```

4. **Read the saved rules:**

   ```sh
   sudo mmhotline agent-playbook-rules show --inventory production
   ```

   **Check:** one line for the service list and one line for each kind,
   with its rule and when it changed:

   ```text
   Services whose changes a person must approve, on every inventory: lab-database.service; changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
   Rules for running the agent's playbooks on inventory production:
   Start or restart a service (start_or_restart_a_service): Act, then report (act_then_report); changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
   ...
   Reboot a server (reboot_a_server): Get permission, then act (1 approval) (get_permission_then_act); changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
   ...
   Unrecognized task (unrecognized_task): Get permission, then act (1 approval) (get_permission_then_act); changed by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T08:39:15Z.
   ```

   Every member can read the same on the page **Rules for running the
   agent's playbooks**, linked from the Triage agent page. Its section
   "What MMHotline counts as each kind of action" lists the tasks of each
   kind.

### Give one playbook its own rule

An ordinary playbook of the team's, `playbooks/NAME.yml`, may have its own
rule on an inventory. The listed rule replaces the strictest rule of its
kinds, even for a task on a listed service. A one-time playbook cannot be
listed.

```yaml
rules_for_running_agent_playbooks:
  production:
    preset: routine_fixes_without_permission
    playbooks:
      restart_lab_web: {rule: act_then_report}
```

**One approval for a known sequence.** The triage agent replaces a failed
server with three playbook runs in one triage session:
`replace_failed_servers`, then `every_server`, then
`destroy_replaced_servers`. When all three need a person's approval, list
them so the person approves once:

```yaml
rules_for_running_agent_playbooks:
  servers:
    preset: routine_fixes_without_permission
    playbooks:
      replace_failed_servers:
        rule: get_permission_then_act
        starts_a_server_replacement: true
        its_approval_also_approves_in_the_same_session: [every_server, destroy_replaced_servers]
      every_server: {rule: get_permission_then_act}
      destroy_replaced_servers: {rule: get_permission_then_act}
```

- `its_approval_also_approves_in_the_same_session` works only with
  `rule: get_permission_then_act`. Each named playbook must be listed on
  the same inventory and must not name the playbook itself. The approval
  page says so before the click. A second request for the same playbook,
  a request in a later session, and one on another inventory ask a person
  as usual.
- `starts_a_server_replacement: true` marks the playbook that starts a
  replacement. While it runs, a waiting SOS waits up to
  `sos_waits_for_a_server_replacement_minutes` longer. Without the mark,
  that setting does nothing.

## Check whether a playbook needs a person

Before the agent asks, see what MMHotline would answer. The check runs
only `mmx explain` and changes nothing: it records no request, asks for no
approval, sends no alert, and runs no playbook. The playbook must be in a
commit that MMHotline's copy already holds: after you push it, run
`sudo mmhotline commit-reviews check --commit-id COMMIT_ID`, which fetches
it. The check exits 0 for each of its three answers.

```sh
sudo mmhotline agent-playbook-rules check-whether-a-playbook-needs-a-person \
  --inventory production --playbook playbooks/restart_lab_web.yml
```

**Check:** the first line is one of three:

- `Needs a person: Get permission, then act (1 approval).`
- `Runs without a person unless someone presses Stop: Warn, wait 10 minutes, then act.`
- `Runs without a person: Act, then report.`

The lines after it say why, and list each task with its kind. A playbook
that only starts a service, with Start or restart a service set to Act,
then report:

```text
Runs without a person: Act, then report.
Why: Every kind of action of the playbook is set to Act, then report on the inventory “production”, so MMHotline runs it without a person.
Checked playbooks/restart_lab_web.yml in the inventory “production” at commit a4fce954720b97d948bdb9f80640cbc5ee5ede5e of MMHotline's copy of the systems repository.
Node tasks, as mmx explain listed them, with their kinds of action:
  start/task_1 on web1: systemd_service; Start or restart a service
...
```

With a review agent set up, a playbook that would run without a person
also gets the line
`A review agent would also review the playbook before the playbook runs. The check command does not ask the review agent.`

A playbook that stops a listed service, `lab-database.service`:

```text
Needs a person: Get permission, then act (1 approval).
Why: A node task acts on a service on the list of services whose changes a person must approve, so the playbook waits for a person whatever the rules for its kinds of action, ...
...
Tasks on services whose changes a person must approve:
  stop/task_1 (systemd_service) stops lab-database.service on web1, which is on the list of services whose changes a person must approve
...
Tasks MMHotline did not recognize, and why:
  stop/task_1 (systemd_service): systemd_service.state must be running
```

The task is also unrecognized: Start or restart a service only starts.
Stopping a service is never a routine fix.

## Approve or refuse a playbook run

When a fix waits for a person, MMHotline wakes the person on call with an
SOS. Its title starts with **Approval needed to run**, then names the
playbook, the inventory, and the servers the playbook can change. The
message starts with the agent's short reason and links the request's page.

1. **Open the request,** from the notification, from the amber box on the
   incident page (**Review first**), or from **Approvals** in the main menu
   (**Review**). On the server, `sudo mmhotline approval-requests list`
   lists the open requests.

2. **Read the request's page,** in this order:
   - **Why the triage agent asks:** the agent's own diagnosis, which
     MMHotline has not checked.
   - **What it changes, step by step**, and **Why it needs a person**: the
     kinds of change and the rule.
   - **The playbook's text, and any files it copies**, at the commit
     MMHotline checked, such as a unit file that a `file` task installs.
     A binary or long file shows its size and SHA-256 instead.
   - **Review agent**, and **Earlier versions the review agent refused**,
     when the review agent refused it.
   - **Steps on services whose changes a person must approve.**

3. **Choose Approve and run** to let it run. With 2 approvals needed,
   another person must approve too; the page then says
   **You approved it.** MMHotline runs the playbook within a few seconds, or
   after another MMX run ends, and the page says how the run went.

   **Or refuse:** enter a **Reason for refusing**, such as
   `Reboot after the nightly backup finishes.`, and choose **Refuse**. One
   refusal ends the request, and nothing runs.

**Check:** the request's page says how the run went, or that you refused
it. MMHotline sends the result, or your reason, to the triage agent, which
goes on working. When every alert of the incident was MMHotline's own
approval or warning alert, and no other request of the incident still
waits, waits to run, or runs, MMHotline resolves the incident; otherwise,
claim and resolve it as usual.

A request stays open until someone decides. Refuse one that is no longer
needed. When the systems repository changed between the approval and the
run, MMHotline does not run the approved version: it checks the new version
and asks again, or runs it if the rules allow that without a person.

## Stop a playbook run before it runs

A fix under **Warn, wait, then act** sends a Warning, which wakes nobody,
such as
`MMHotline will run one_time_playbooks/2026-11-03_free_disk_space_on_app002.yml in the inventory “production” for the triage agent at 03:25 UTC unless someone presses Stop: it can change app002`.

1. **Open the run's page** from the link at the end of the warning, or the
   incident page, whose amber box counts down, such as **Runs in 7
   minutes**, or **Approvals**.
2. **Read** what the playbook changes and its text, as for an approval.
3. **Optionally enter a Reason for stopping,** such as
   `The backup is still running.`, **and choose Stop.** On **Approvals**,
   **Stop** beside the run stops it in one press, without a reason.

**Check:** the page says the run was stopped, and nothing runs. MMHotline
tells the agent who stopped it and why.

Stop works only before the run time. To stop such fixes for good, change
the inventory's rules in the settings file.

## If something is different

- **`settings apply` fails the `rules_for_running_agent_playbooks` part and
  names an inventory:** the settings file has no entry for an inventory in
  MMHotline's copy. Every fix there needs a person until you add the entry.
- **`settings check` or `settings apply` prints**
  `rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one`:
  the inventory's `action_kinds` leaves that kind out. The part still
  applies. Add the kind if a person should not be needed.
- **The check exits 4 and says**
  `mmhotline: MMHotline cannot tell what playbooks/restart_lab_web.yml in the inventory “production” does: mmx explain failed: ...`:
  MMX cannot read the playbook, such as for a misspelled task. The agent's
  request for the same playbook would be refused the same way. Fix the
  playbook with `mmx validate`.
- **The check is refused with**
  `MMHotline refused the request: the playbook path ...`: the path must be
  `playbooks/NAME.yml` or `one_time_playbooks/YYYY-MM-DD_what_it_does.yml`.
- **A task shows as Unrecognized task with a reason,** such as
  `systemd_service.start_at_boot is not an allowed argument for Start or restart a service (...)`:
  the task writes something the kind does not allow, so the playbook takes
  the Unrecognized task rule.
- **Every playbook request is refused, and MMHotline says to upgrade
  MMX:** the `mmx` in `/usr/local/bin` is older than 0.1.0.
- **A fix you expected to run waits for a person:** it acts on a listed
  service, or one of its tasks is unrecognized. The request's **Why it
  needs a person** names which.
