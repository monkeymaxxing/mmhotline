# Keep the agents signed in

**Who acts:** an administrator with `sudo` on the MMHotline server, or that
person's coding agent. A team whose deploy tool writes the settings file
changes the credential in its vault instead.

**When to use this page:** when MMHotline says an agent cannot sign in, or
that a token expires soon; or to check a sign-in by hand.

**What this page does:** you check each agent's sign-in, and replace an
expiring or broken credential in the settings file, which MMHotline tests
before it saves.

**Terms:**

- The **agents** are the triage agent and, if the team set one up, the
  review agent, as [Set up the triage agent](set-up-the-triage-agent.md)
  says. Each has its own `credential` in the settings file.
- The **sign-in check** asks the coding agent's service whether the
  credential still works. MMHotline runs it once a day for each agent,
  unless a run of that agent succeeded in the last 24 hours.

## What MMHotline tells you

- **A sign-in that fails:** administrators get a Warning, such as
  "The triage agent cannot sign in to Codex", and an Info alert when it
  works again.
- **A Claude Code subscription token** lasts one year from when the
  settings file's credential applied. Administrators get a Warning, such as
  "The triage agent's Claude Code token expires on 2027-10-27", 30 days and
  7 days before. A change of only `model` or `reasoning_effort` keeps the
  date.
  Apply only a token you just made with `claude setup-token`: MMHotline
  counts the year from the apply, so an older token would end before
  MMHotline's reminders.
- **A Codex access token** gets the same two reminders before its
  `credential_expires_on`, such as
  "The triage agent's Codex token expires on 2027-02-06". A token that
  applies with 30 days or less left gets only the 7-day reminder.
- **A Codex ChatGPT sign-in** has no end date: Codex refreshes it during
  the daily runs, and MMHotline keeps each refreshed copy.
- **An API key** never expires. The check shows whether the vendor accepts
  the key, not whether the account has credit left.


## Choose the route

- **See whether each sign-in works:** [Check a sign-in](#check-a-sign-in).
- **Renew a Claude Code token, or replace an API key:**
  [Replace a credential](#replace-a-credential).
- **Renew a Codex access token:**
  [Renew a Codex access token](#renew-a-codex-access-token).
- **A Codex ChatGPT sign-in stopped working:**
  [Make a ChatGPT sign-in for MMHotline](#make-a-chatgpt-sign-in-for-mmhotline).

## Before you start

- You can change the settings file, as
  [Change the settings](change-the-settings.md) says.
- For a Claude Code token: a computer with Claude Code, signed in to the
  team's Claude account.

## Check a sign-in

1. **Read each agent's sign-in:**

   ```sh
   sudo mmhotline triage-agent show
   sudo mmhotline review-agent show
   ```

   **Check:** each names its sign-in and when it was last checked, and
   for a subscription token its end:

   ```text
   Sign-in to Claude Code: works; last checked at 2026-11-15T08:51:06Z.
   Subscription token expires at: 2027-11-15T08:51:06Z
   ```

   ```text
   Sign-in to Codex: works; last checked at 2026-11-15T08:55:26Z.
   ```

   For a Codex access token, `show` prints `Codex access token expires on:`
   with the date.
   The Triage agent page shows the same.

2. **Check a sign-in now,** if you need to know at once:

   ```sh
   sudo mmhotline triage-agent check-sign-in
   sudo mmhotline review-agent check-sign-in
   ```

   The command waits for the check, at most 5 minutes.

   **Check:** it exits 0 and prints the setup with lines such as:

   ```text
   Sign-in to Claude Code: works; last checked at 2026-11-15T09:13:09Z.
   Latest sign-in check 2259e0af096ab39907a4d32f7d3f324d: passed; run state: finished; started by the mmhotline command, run by root at 2026-11-15T09:13:07Z.
   ```

   A failing check prints `fails` with the reason and exits non-zero.

   The Triage agent page's **Check the sign-in now** does the same. A check
   by hand counts as the daily check, so the next one is due 24 hours
   later.

## Replace a credential

Use this for a Claude Code subscription token before it expires, and for
an API key that stopped working.

1. **Get the new credential.** For a Claude Code token, on a computer with
   Claude Code signed in to the team's Claude account:

   ```sh
   claude setup-token
   ```

   For an API key, make a new key at Anthropic or OpenAI.

2. **Put it in the settings file,** with
   `sudoedit /etc/mmhotline/mmhotline_settings.yml`: in
   `triage_agent.credential` for the triage agent, or
   `review_agent.credential` for the review agent.

3. **Apply the file,** or let the deploy tool apply it:

   ```sh
   sudo mmhotline settings apply
   ```

   MMHotline runs the setup test with the new credential before it saves
   it.

   **Check:** the agent's line ends in `applied.`, such as
   `triage_agent.coding_agent, triage_agent.credential_type, triage_agent.credential, triage_agent.credential_expires_on, triage_agent.model, triage_agent.reasoning_effort: applied.`
   For a token, `sudo mmhotline triage-agent show` prints a new
   `Subscription token expires at:` one year later, and the reminders start
   again.

When the test fails, the old credential stays, the line says why, and
`settings apply` exits 1.

## Renew a Codex access token

1. Make a new token on ChatGPT's **Access tokens** page, with the longest
   end date your workspace allows.
2. Put the token in `credential` and its end date in
   `credential_expires_on`, such as `2027-02-06`.
3. Apply, as in [Replace a credential](#replace-a-credential), step 3.

**Check:** `sudo mmhotline triage-agent show` prints
`Codex access token expires on:` with the new date.

## Make a ChatGPT sign-in for MMHotline

Use a ChatGPT sign-in only when an API key is not possible. Only one place
may use the sign-in: if anything else uses it, it stops working, and the
alert says why when OpenAI gave a reason, such as
"Another copy of this ChatGPT sign-in was used, so OpenAI ended this one."
Use a ChatGPT account that has no connected apps, because the agent holds
this sign-in while it runs.

1. **Sign in, in a new empty folder** on your own computer:

   ```sh
   export CODEX_HOME="$(mktemp -d)"
   codex login
   cat "$CODEX_HOME/auth.json"
   rm -rf "$CODEX_HOME" && unset CODEX_HOME
   clear
   ```

   `cat` prints the sign-in, a secret: copy it, then `clear` the terminal
   so the text does not stay on screen.

   Delete the folder as shown; do not run `codex logout`, which would end
   MMHotline's sign-in too. Your own Codex sign-in is separate and keeps
   working.

2. **Put the printed text in `credential`,** with
   `credential_type: chatgpt_sign_in`, as a block:

   ```yaml
   credential_type: chatgpt_sign_in
   credential: |
     {
       "auth_mode": "chatgpt",
       ...the rest of the printed text, each line indented...
     }
   ```

   The review agent needs a sign-in of its own: repeat these steps for it.

3. **Apply,** as in [Replace a credential](#replace-a-credential), step 3.

   **Check:** the agent's line ends in `applied.`, and `show` prints
   `Sign-in to Codex: works; last checked at TIME.`

MMHotline keeps the newest copy of the sign-in after each run. Leave the
settings file's copy as it is: applying the same text again changes
nothing. Never reuse an earlier text after a sign-in stopped working: its
refresh token was already used, so the setup test fails and the earlier
setup stays.

## If something is different

- **`check-sign-in` fails with** `A triage agent run is already active.`:
  wait until the run ends, then check again. A triage session that needs
  the agent also stops a running check, because alerts come first.
- **`settings apply` says**
  `a triage agent run was active, so the new agent setup was not tested. Apply the settings file again after the triage agent run ends`:
  apply again after the run ends.
- **The check works, but the agent's runs fail for an API key:** the
  account may have no credit left, which the check cannot see. Look at the
  account at Anthropic or OpenAI.
- **The setup test fails with the new credential:** the old one stays.
  Check that you copied the whole token or key, and that it belongs to
  the coding agent named in `coding_agent`.
