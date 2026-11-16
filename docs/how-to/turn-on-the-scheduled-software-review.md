# Turn on the scheduled software review

**Who acts:** an administrator with `sudo` on the MMHotline server, who can
push to the team's systems repository, or that person's coding agent.

**When to use this page:** so that the triage agent checks the servers'
software for known flaws and new releases on a schedule, and tells the team
what needs an update, without anyone asking.

**What this page does:** you write the review's task in the systems
repository, run one review by hand to learn what it costs, and then set
how often reviews run.

**Terms:**

- The **scheduled software review** starts the triage agent at the
  interval you choose, with a task your team writes. It is off until you
  turn it on, because each review is one run of the coding agent, which
  your team pays for. While it is off, the agent wakes only for alerts.
- The **review's task** is `scheduled_software_review_task.md` at the top
  of the systems repository: which software to check, and how.
- **OSV.dev** is a public database of known flaws in packages. **CISA's
  list of known exploited vulnerabilities** names the flaws attackers are
  known to use. CISA is the United States' cybersecurity agency.

## Before you start

- The triage agent is set up and its sign-in works, as
  [Set up the triage agent](set-up-the-triage-agent.md) says.
- `triage_agent_rules.md` in the systems repository says how to rank a
  finding. Reason: the example task refers to its sections "The software
  review" and "Levels of vulnerability findings", which the starting rules
  file in [Set up the triage agent](set-up-the-triage-agent.md) has; fit
  them to your team.
- Optionally, `software_outside_the_distribution.yml` in the systems
  repository lists the programs installed from outside the distribution,
  each with its `installed_version_command`,
  `installed_on_server_groups`, `releases_page`, and
  `security_advisories_page`. MMHotline lets the agent reach the host of
  each listed page.

## Steps

1. **Write the review's task.** Copy the [example task](#the-example-task)
   into `scheduled_software_review_task.md` at the top of the systems
   repository. Ask your coding agent to fit it to your servers, such as to
   add container images or leave out a check you do not need. Commit,
   signed, and push.

   **Check:** `git log --oneline -1 -- scheduled_software_review_task.md`
   names your commit, and the file is at most 64 KiB.

   Reason: at each review's start, MMHotline reads the file as the review's
   starting commit holds it, at most 64 KiB, and quotes it as the team's
   instructions. MMHotline adds the rules file, the time of the last
   review that finished, and the report commands.

2. **Run one review now,** while the schedule is still off:

   ```sh
   sudo mmhotline triage-agent run-software-review
   ```

   The command returns once the review has started. A review takes a few
   minutes.

   **Check:** the command exits 0 and prints the agent's state, with
   `Latest software review ...: running`. The review is refused while
   another run is active; try again when it ends.

3. **Wait for the review to end, then read its cost:**

   ```sh
   sudo mmhotline triage-agent show
   sudo mmhotline triage-agent show --json
   ```

   **Check:** once the review has ended, `show` prints lines such as:

   ```text
   Latest software review 7b57cd61a8b0eb0866b0b14fa8814381: finished; started by the mmhotline command, run by root at 2026-11-15T09:13:27Z.
   Software review ended at: 2026-11-15T09:17:00Z
   Software review reports: 0 at sos, 1 at warn, 5 at info.
   Software review usage: Cost in US dollars: 0.986704
   ```

   In the `--json` output, `triage_agent.latest_software_review`
   holds `state`, `started_at`, `ended_at`, `failure_reason` for a failed
   review,
   `cost_in_us_dollars` for Claude Code, the token counts `input_tokens`,
   `cached_input_tokens`, and `output_tokens`, and
   `report_counts_by_alert_severity`, how many reports the review sent at
   each level.

   `cost_in_us_dollars` is the cost at API prices. Codex reports only the
   token counts: multiply each by your model's price.

4. **Work out the monthly cost.** Multiply one review's cost by the reviews
   per day, 24 divided by the interval in hours. For example, in a measured
   run on 2026-11-11, a review of two servers on a quiet day, with Claude
   Code and Opus, took 2 minutes 25 seconds and cost $1.12 at API prices:

   | Interval | Per day | Per month |
   | --- | --- | --- |
   | Every 24 hours | $1.12 | about $34 |
   | Every 6 hours | $4.48 | about $134 |

   The first review, when every advisory is new, costs more: $1.97 in the
   same run. On 2026-11-12, a first review of two servers that had never
   installed security updates, with 51 flaws Debian had already fixed,
   took 6 minutes 59 seconds and cost $2.32; the quiet review after it
   cost $1.01. On 2026-11-15, a first review of one Debian 12 server with
   13 fixable flaws took 3 minutes 33 seconds and cost $0.99. Servers that
   install updates by themselves keep reviews near the quiet-day cost.

5. **Set the interval** in the settings file, such as every 24 hours, and
   apply:

   ```yaml
   triage_agent:
     software_review_interval_hours: 24
   ```

   The interval is a whole number of hours, from 1 to 8760, a year.

   ```sh
   sudo mmhotline settings apply
   sudo mmhotline triage-agent show
   ```

   **Check:** both print `Scheduled software review: every 24 hours`, the
   apply adding `, with the team's task in scheduled_software_review_task.md in the systems repository.` The
   **Software review** section of the Triage agent page shows the
   interval, when the next review is due, and the latest review.

   When no review has run before, the first starts right after the
   interval applies. A review started by hand counts too: the next starts
   one interval after it.

## What a review sends

Each finding reaches the team as the agent's report, at the level
`triage_agent_rules.md` gives it: Info, Warning, or SOS. A report names the
program or package, the server, the installed and fixed versions, the
advisory, whether CISA's list names it, and the remedy. The agent may also
ask MMHotline to run a playbook, which follows the team's rules, as
[Decide what the triage agent may do](decide-what-the-triage-agent-may-do.md)
says.

Alerts come first: a triage session that needs the agent stops a running
review after the agent's current step.

## Turn the review off

Set `software_review_interval_hours: off` and apply.

**Check:** `settings apply` prints the note
`triage_agent.software_review_interval_hours: Scheduled software review: off. The triage agent wakes only for alerts; sudo mmhotline triage-agent run-software-review still runs one review, such as to learn what one costs.`
and `triage-agent show` prints
`Scheduled software review: off; set by the settings file /etc/mmhotline/mmhotline_settings.yml at ...`
and
`A software review runs only when an administrator runs sudo mmhotline triage-agent run-software-review.`

## The example task

The example reads the programs in `software_outside_the_distribution.yml`,
the servers' Debian and Ubuntu packages through OSV.dev, and CISA's list.
MMHotline lets every run reach OSV.dev's API, `raw.githubusercontent.com`,
and the hosts of the vendor pages in
`software_outside_the_distribution.yml`, so the example needs no
`listed_hosts`.

````markdown
# Scheduled software review

Check the servers' software for known flaws and new releases. Follow "The
software review" and "Levels of vulnerability findings" in
triage_agent_rules.md.

## Programs from outside the distribution

Read software_outside_the_distribution.yml. For each program, run its
installed_version_command over SSH on each server of its
installed_on_server_groups, such as `ssh app001 'caddy version'`. The
inventories in inventories/ list the servers of each group; all means every
server. Compare each installed version with the newest release on the
program's releases_page, and read its security_advisories_page.

## Debian and Ubuntu packages

On each server, list the installed source packages and their versions, and
read the release:

    ssh app001 "dpkg-query --show --showformat='\${db:Status-Abbrev} \${source:Package} \${source:Version}\n'" | awk '$1 == "ii" {print $2, $3}' | sort -u
    ssh app001 cat /etc/os-release

Leave out every package whose name starts with linux: a kernel flaw has one
remedy, a new kernel and a reboot, which the monitoring tool's checks cover.
Ask OSV.dev in batches of at most 1,000 queries. The ecosystem is
Debian:VERSION_ID on Debian, such as Debian:13, and Ubuntu:VERSION_ID:LTS on
an Ubuntu LTS release, such as Ubuntu:24.04:LTS:

    curl --silent --request POST https://api.osv.dev/v1/querybatch --header 'Content-Type: application/json' --data '{"queries":[{"package":{"ecosystem":"Debian:13","name":"openssl"},"version":"3.5.1-1"}]}'

Results come in query order. When a result has next_page_token, repeat its
query with "page_token" set to that value. Read each flaw with
GET https://api.osv.dev/v1/vulns/ID for its fixed version and aliases, such
as a CVE ID.

## Known exploited flaws

Look up each flaw's CVE ID in CISA's list of known exploited
vulnerabilities. Save the list once, then search it; each entry of its
vulnerabilities names a flaw by cveID:

    curl --silent --output known_exploited_vulnerabilities.json https://raw.githubusercontent.com/cisagov/kev-data/main/known_exploited_vulnerabilities.json

## Reports

Send each finding as its own report at its level in "Levels of
vulnerability findings": the program or package, the server, the installed
and fixed versions, the advisory or OSV.dev ID, whether CISA's list names
it, and the remedy.
````

## If something is different

- **`show` gives a failed review, with**
  `The scheduled software review did not start: the systems repository has no scheduled_software_review_task.md, the review's task that says which software to check. Add the file (see "Turn on the scheduled software review" in MMHotline's documentation, https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/turn-on-the-scheduled-software-review.md), or set software_review_interval_hours to off.`:
  add the task file, as in step 1. A task file that is empty or larger
  than 64 KiB fails the same way. Such a review costs nothing, and the
  next one starts one interval later.
- **`run-software-review` fails with** `A triage agent run is already active.`:
  wait until the run ends, then run the command again.
- **A report says the logging proxy refused a host,** such as the host a
  vendor page redirects to: add the host to `triage_agent.listed_hosts`,
  such as `[{host: packages.example.com, port: 443}]`, apply, and
  start a review with `sudo mmhotline triage-agent run-software-review`.
  The Triage agent page's **Recent connections** shows the refusal and
  its reason.
- **The cost is higher than you expected:** read the review's steps on the
  Triage agent page, and ask your coding agent to shorten the task, such as
  to leave out a check. Or set a longer interval.
