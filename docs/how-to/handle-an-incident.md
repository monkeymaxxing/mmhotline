# Handle an incident

**Who acts:** any person on the team who was woken by an SOS, or who
sees one waiting. Every person on the team can claim, resolve, or hand on
an incident.

**When to use this page:** your phone sounded with an SOS from MMHotline,
or the dashboard shows an SOS under **Needs a person**.

**What this page does:** you tell the team that you are handling the
problem (you **claim** the incident), so MMHotline stops waking more
people. Then you fix the problem and **resolve** the incident.

**Terms:** an **SOS** is the urgent alert that wakes the person on call. An
**incident** is MMHotline's record of the problem that an SOS reports.
[Severities and incident states](../reference/severities-and-incident-states.md)
lists the severities and the states an incident goes through. Times
on MMHotline's pages are in the team's time zone, which the foot of each
page names.

## Choose the route

- **You were woken, or you see an unclaimed SOS:** follow
  [Claim and resolve an incident](#claim-and-resolve-an-incident).
- **Someone else should handle it:** see
  [Wake another person now](#wake-another-person-now).
- **Someone else has claimed it, and you take it over:** see
  [Take over a claimed incident](#take-over-a-claimed-incident).
- **The page says the triage agent looks first:** see
  [The triage agent is working on it](#the-triage-agent-is-working-on-it).
- **Something is wrong, but no tool sent an alert:** see
  [Raise an SOS by hand](#raise-an-sos-by-hand).

## Claim and resolve an incident

1. **Open the incident.** Open the link in the notification, or open
   MMHotline and choose the incident under **Needs a person** on the
   dashboard.

   **Check:** the incident's page, `/incidents/ID`, shows the SOS, who was
   woken, and what MMHotline does next by itself, with a bar that runs out
   at that time, such as:

   ```text
   SOS · Not claimed
   “Monitoring” hasn't checked in with MMHotline for 5 minutes: it may have stopped, or its server may be down
   You are on call. MMHotline woke you at 23:02.
   Claim the incident by 23:12, or MMHotline also wakes Barbara Example.
   ```

   If MMHotline does not open, Cloudflare may be down. Either claim from
   the server's terminal, as in
   [Claim and resolve from the server's terminal](#claim-and-resolve-from-the-servers-terminal),
   or see
   [Sign in during a Cloudflare outage](sign-in-during-a-cloudflare-outage.md).

2. **Claim the incident.** Optionally write a line for the team in
   **Note for the team (optional)**, such as `Checking the monitoring
   server.`, then choose **Claim: I'm handling the incident**.

   **Check:** the page says:

   ```text
   SOS · Claimed
   You claimed the incident at 23:02. MMHotline wakes nobody else about the incident.
   ```

   Reason to claim first, before you look into the problem: the claim stops
   the countdown and the SOS's repeats, so nobody else is woken while you
   work, and the team sees who has the incident. On the dashboard, the
   incident moves from **Needs a person** to **Being handled**.

   You can also claim without opening the page: acknowledge the Pushover
   alert, or press **Claim this incident (assign it to me)** in Slack or
   Telegram. Answering an automated voice call does not claim the
   incident. A note needs the page: a claim made in a notification has
   none. A note is one line of at most 500 characters, and **What
   happened** keeps it after the claim or resolve. The triage agent, when
   the team has one, reads it once, the next time it looks into this
   incident or its cause.

3. **Read what the alert says, and fix the problem.** The incident page
   folds the alert's whole text under **The alert's full message**.
   **What happened** lists, oldest first, the SOS, whom MMHotline woke and
   how each notification went, later alerts about the same problem, and
   each action with its note. Rounds that wake the same people again show
   as one line with a count, and the list ends with what MMHotline does
   next by itself. The steps above it show where the incident is:
   received, woken, waiting for a claim or claimed, and resolved.

4. **Resolve the incident** when the problem is over. Optionally write what
   you did in **Note for the team (optional)**, such as
   `The monitoring service had stopped; restarted it.`, then choose
   **Resolve**.

   **Check:** the page says:

   ```text
   SOS · Resolved
   You resolved the incident at 23:02.
   Your note: “The monitoring service had stopped; restarted it.”
   ```

   Resolving stops all the incident's waking. A notification already handed
   to Pushover or another service can still arrive. MMHotline then sends a
   "Resolved SOS" message to the people it woke.
   MMHotline sends one person at most one message of this kind a minute,
   so the message can arrive up to a minute later.

   The tool that sent the SOS can also resolve the incident itself, when it
   sees the problem end. **What happened** then says the alert source
   resolved it. An SOS about a tool's silence resolves itself when the
   tool's check-ins return, if nobody has claimed it.

## Claim and resolve from the server's terminal

Use this when you cannot open the page, or when a coding agent on the
server does the work. You need SSH access and `sudo` on the MMHotline
server. You need no browser and no Cloudflare.

1. **Find the incident ID.**

   ```sh
   sudo mmhotline incidents list
   ```

   **Check:** each incident shows its ID on its first line, such as
   `SOS incident acc896d49ce5b2ef40cd2c944da4f08f: Disk of db001 is 99% full`.

2. **Claim it,** with the sign-in email address of the member who takes it:

   ```sh
   sudo mmhotline incidents claim --incident-id ID --sign-in-email-address barbara@example.com
   ```

   **Check:** the last line says
   `Barbara Example now owns SOS incident ID. MMHotline stopped contacting people about it.`
   Reason for the address: your Unix user is not a member, and a claimed
   incident belongs to a member. The history names both the member and your
   Unix user.

3. **Fix the problem, then resolve it.** Add `--note` for a line the team
   reads in the history.

   ```sh
   sudo mmhotline incidents resolve --incident-id ID --note "Restarted the monitoring service."
   ```

   **Check:** the last line says `SOS incident ID is resolved.`

An incident that someone else owns is taken over, as on the page. A test
SOS resolves itself after the claim. The
[command reference](../reference/mmhotline-command.md#mmhotline-incidents-claim)
lists every message.

## Wake another person now

1. On the incident page, open **More**, choose the person under
   **Escalate to someone else**, and choose **Wake this person now**.

   **Check:** the page says that MMHotline is waking that person, and
   **What happened** says `You chose to wake ... now.`

The incident becomes unclaimed, also when you had claimed it, and the
other person gets a fresh time to claim. They must claim it to take it on.
You can escalate again later. A person who is unavailable shows in the
list with the time they are available again, and cannot be chosen; the
page's **People** panel lists them under **Can't be woken**.

## Take over a claimed incident

When someone else has claimed the incident, the page offers
**Take over: I'm handling the incident** instead of the claim button.
Choosing it moves the incident to you. The person who holds the incident
does not see the button.

## The triage agent is working on it

When the team has set up the triage agent, an SOS from a tool goes to the
agent first, and the page says
`The triage agent looks first, so MMHotline has woken nobody yet.` The
countdown says when MMHotline wakes the person on call, at most 5 minutes
after the SOS arrived, unless the team chose another wait.

- **You can claim, resolve, or escalate at any time** while the agent
  works, as on any incident.
- **To stop the agent and handle the problem yourself,** choose **Take over
  from the agent**. The panel says
  `Stopping: the triage agent is finishing the current step.`, and **What
  happened** says that you took over. MMHotline refuses the agent's new
  requests to run playbooks from then on; a request already decided goes
  on as decided. When the step ends, the panel says **Handing over**, and
  the agent writes what it found, did, and planned next. That report
  appears in **What happened** of each incident of the session, and the
  agent's note goes to `triage_agent_notes/` in the team's systems
  repository, where the next session starts from it. The panel then says
  the session ended: **A person took over**.

  Taking over does not claim the incident: choose **Claim: I'm handling
  the incident** as well. If nobody was woken yet, taking over wakes the
  person on call at once. A playbook run that waits for its warning's time
  is stopped with the reason `Took over from the triage agent.`; a run that
  waits for approval stays open for any person to approve or refuse.
- **The agent's latest findings** show in a panel on the incident page,
  under **What the triage agent found**, with what the agent does now.
  **The triage agent's steps**, folded below, lists each step as it
  happens, such as the commands it runs on the servers; the page refreshes
  every ten seconds. **Watch the session** opens the session's own page.
- **An SOS that joins a session that already woke you** says
  `MMHotline already woke you for this incident's triage session, so this incident wakes nobody now.`
  It does not wake you again, but it escalates as usual if nobody claims
  it in time.

When the agent is replacing a failed server, the SOS waits longer, by the
settings file's `triage_agent.sos_waits_for_a_server_replacement_minutes`,
and you get one warning such as:

```text
[Warning] Your site is down: The server db02 stopped answering
The triage agent is replacing the server. You will be woken only if the replacement fails or is not done by 03:22 UTC. Follow the steps on the incident's page.
```

To wake someone at once, open **More** and choose **Wake this person now**,
as in [Wake another person now](#wake-another-person-now). The page says
`The triage agent is replacing a server, so MMHotline has woken nobody yet.`,
and the countdown says when MMHotline wakes you, such as
`MMHotline wakes you at 03:22 unless the replacement ends first.`
If the replacement fails, the agent asks for a person, or the time runs
out, MMHotline wakes you at once, and the message starts with why, such as
`MMHotline is waking you now. The server replacement took longer than 10 minutes.`

When the site recovers and nobody was woken, the resolved message comes in
Pushover at normal priority, like the warning:
`Your site works again: The server db02 stopped answering. The triage agent's server replacement fixed it; nobody needed to be woken.`
If MMHotline did wake you, the ordinary resolved message comes instead. An
SOS that the agent fixed without replacing a server sends no resolved
message to Pushover, because Pushover never got the SOS; email, which gets
the SOS at once, still gets one.

## Raise an SOS by hand

1. On the dashboard, choose **Raise an SOS by hand**, beside the heading,
   enter **Title** and **Message**, and choose **Raise the SOS**.

   **Check:** the new incident's page opens, and MMHotline wakes the person
   on call as for any SOS.

Raising the SOS does not claim it for you: claim it if you handle it.
MMHotline records who raised it.

## If something is different

- **Nobody claims in time:** when the countdown ends, MMHotline wakes the
  backup, and after the backup's time, everyone who is available. Each step
  gets the full time to claim, which the settings file sets in
  `on_call_schedule.escalate_if_unclaimed_after_minutes`.
- **The page says**
  `The incident changed before your action was saved. Reload the page and try again.`
  Someone else acted on the incident at the same moment. Reload the page,
  read what changed, and choose again. Reason: MMHotline refuses (with HTTP
  status 409) an action made on an older state of the page, so it never
  overwrites another person's claim.
- **You want the history on the server, or for a coding agent:**
  `sudo mmhotline incidents list` lists the incidents that are not
  resolved, and `sudo mmhotline incidents show --incident-id ID` prints one
  incident with its history, notes included. Both take `--json`. To claim
  or resolve without the page, see
  [Claim and resolve from the server's terminal](#claim-and-resolve-from-the-servers-terminal).
