# Change the settings

**Who acts:** an administrator with `sudo` on the MMHotline server,
or that person's coding agent. A team whose deploy tool writes the settings
file, such as [MMX](https://github.com/monkeymaxxing/mmx), changes the file
in its repository instead; see [Choose the route](#choose-the-route).

**When to use this page:** to change anything in
`/etc/mmhotline/mmhotline_settings.yml`: the team's people and on-call
schedule, alert sources, sending services, the triage agent, the
MMHotline-down alarm, the status page, or the server's addresses. Each
person changes their own contact methods, unavailability, and taking
on-call on MMHotline's pages, not in this file.

**What this page does:** you edit the settings file, check it, and apply it
to the running MMHotline. MMHotline then uses the new settings, and
nothing else needs a restart by hand.

The [settings file reference](../reference/settings-file.md) lists every
key and its values.

## Choose the route

- **You change the file on the server by hand:** follow
  [Change the settings by hand](#change-the-settings-by-hand).
- **You change a secret on the server:** follow
  [Change a secret](#change-a-secret).
- **A deploy tool writes the file from your repository:** follow
  [Change the settings with a deploy tool](#change-the-settings-with-a-deploy-tool).

## Change a secret

A secret is a token, a password, or a signing secret. Do not paste it into
the file in an editor. This command saves it for you, checks it first, and
never prints it:

```sh
sudo mmhotline settings set-secret sending_services.pushover.application_token --apply
```

The command waits for the secret: paste it, then press Enter. Nothing
shows as you paste. A coding agent pipes the secret in instead, from where
the person put it. [The `mmhotline` command reference](../reference/mmhotline-command.md#mmhotline-settings-set-secret)
lists the secret keys.

A TEAM WHOSE DEPLOY TOOL, SUCH AS MMX, WRITES THE FILE FROM A VAULT DOES NOT USE THIS COMMAND. THE NEXT DEPLOY OVERWRITES THE FILE AND LOSES THE CHANGE. SET THE SECRET IN THE VAULT INSTEAD.

## Change the settings by hand

1. **Open the settings file:**

   ```sh
   sudoedit /etc/mmhotline/mmhotline_settings.yml
   ```

   **Check:** the editor shows the file. If `sudoedit` says the file does
   not exist, MMHotline is not installed on this server yet: follow
   [Install MMHotline](install.md) first.

   A coding agent without a terminal edits the file with any tool that
   keeps its owner and mode, such as a short Python program run with
   `sudo`, then goes on with step 3.

2. **Make the change and save the file.** For example, to give the person
   on call 10 minutes to claim an SOS before MMHotline contacts the backup,
   change one line under `on_call_schedule`:

   ```yaml
   on_call_schedule:
     escalate_if_unclaimed_after_minutes: 10
   ```

3. **Check the file:**

   ```sh
   sudo mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
   ```

   **Check:** the command prints

   ```text
   The settings file /etc/mmhotline/mmhotline_settings.yml has no problems.
   ```

   If it lists problems instead, each line names the key and what is
   wrong. Fix each one, save, and check again. For example, a missing `s`
   at the end of a key name prints:

   ```text
   The settings file /etc/mmhotline/mmhotline_settings.yml has 2 problems:
   on_call_schedule.escalate_if_unclaimed_after_minute: unknown key
   on_call_schedule.escalate_if_unclaimed_after_minutes: required
   ```

   A problem with the whole file has no key name, such as
   `the settings file is not valid YAML: line 1: did not find expected node content`.
   Besides each key and value, the check requires at least one
   `administrator` in `people`, and every sign-in email address in
   `on_call_schedule` must be listed in `people`. The check never prints a
   secret's value.

   Reason for this step: the check finds every mistake in the file at
   once, before the running MMHotline sees the file. The check changes
   nothing, runs no custom schedule code, and calls no outside service;
   `settings apply` does those.

4. **Apply the file:**

   ```sh
   sudo mmhotline settings apply
   ```

   MMHotline applies the file in parts, such as `people` and
   `on_call_schedule`, and prints one line for each part. A part you
   changed says `applied.`, and the parts you did not change say
   `unchanged.` For the change above, the output starts and ends like this:

   ```text
   Settings file: /etc/mmhotline/mmhotline_settings.yml
   Settings file first comment line: Written by hand with sudoedit. Apply a change with sudo mmhotline settings apply.
   web_url, web_listen_address, alert_intake_listen_address, cloudflare_access: unchanged.
   team_name: unchanged.
   fleet_view_command: unchanged.
   people: unchanged.
   on_call_schedule: applied.
   ...
   Every part of the settings file is applied.
   ```

   **Check:** the last line is `Every part of the settings file is
   applied.` If a line says `did not apply`, see
   [A part did not apply](#a-part-did-not-apply).

   Some changes take a few minutes to apply, because MMHotline first tests
   them with an outside service. The command waits until the test ends.

5. **See what MMHotline now uses:**

   ```sh
   sudo mmhotline settings show
   ```

   **Check:** the part you changed says `applied at` with the time of
   step 4, such as:

   ```text
   on_call_schedule: applied at 2026-11-15T03:58:24Z.
   ```

   `settings show` reads what MMHotline applied, not the file, so it shows
   what MMHotline uses even when the file has changed since. Its
   `Last applied:` line says when any part last applied, and a part that
   never applied says `not applied yet.`

## Change the settings with a deploy tool

1. **Change the settings file in your repository,** and have the change
   reviewed as your team reviews other changes.

2. **Check the file before it reaches the server.** The check needs no
   root and no running MMHotline, so it runs on a laptop or in the deploy
   tool's dry run:

   ```sh
   mmhotline settings check --settings-file mmhotline_settings.yml
   ```

   **Check:** the command prints `... has no problems.` and exits 0. It
   exits 1 when the file has problems, and the deploy should stop there.

3. **Deploy the file.** The deploy tool writes
   `/etc/mmhotline/mmhotline_settings.yml` with owner `root`, group
   `mmhotline`, and mode `0640`, then runs `sudo mmhotline settings apply`
   on the server.

   **Check:** `sudo mmhotline settings apply` exits 0 and its last line is
   `Every part of the settings file is applied.` If a line says
   `did not apply`, see [A part did not apply](#a-part-did-not-apply).

   Reason for the owner and mode: MMHotline refuses a settings file that
   someone other than root could change, or that every user could read,
   because the file holds the team's secrets and its
   `fleet_view_command` runs as root.

4. **Start the file with a comment that says how to change it,** such as
   `# Managed by MMX from mmhotline_settings.yml in the systems repository`.
   MMHotline's pages and `settings show` display that first line, so a
   person who reads the settings on a page knows where to change them.

## If something is different

### A part did not apply

A part that did not apply keeps its earlier setting, and the other parts
still apply. The line says why:

```text
sending_services.pushover: did not apply: Checked with Pushover: Pushover answered HTTP 400: application token is invalid. The earlier setting, applied at 2026-11-15T03:54:04Z, stays.
1 part did not apply and keeps its earlier setting: sending_services.pushover.
```

Fix the cause, then run `sudo mmhotline settings apply` again. The
dashboard also tells administrators about each part that did not apply. A
part that failed with no earlier setting says
`No value has ever applied for the settings file part, so the part is not set.`
A part you did not change says `unchanged.`, unless its last apply failed;
then MMHotline tries it again.

These parts are tested with an outside service before MMHotline saves
them, so a wrong token or address never replaces a working one:

| Part | What MMHotline tests |
| --- | --- |
| `sending_services.pushover` | The application token, with Pushover |
| `sending_services.twilio` | The calling number, in the Twilio account |
| `sending_services.telegram` | The bot token, with Telegram |
| `sending_services.slack` | The Slack tokens |
| `sending_services.mail_server` | A connection and sign-in to the mail server; it sends nothing |
| `triage_agent.coding_agent, ...` and `review_agent` | One test prompt to the coding agent, with the file's credential, model, and reasoning effort |
| `mmhotline_down_alarm` | Uploads and checks in with the alarm in Cloudflare |
| `status_page` | Publishes the current page |

Other reasons a part does not apply:

- **You removed a person who owns a claimed incident.** The `people` line
  names the person and the number of claimed incidents. Resolve those
  incidents, or have someone take them over, then apply again.
- **You removed a sending service that contact methods still use.** The
  line names the contact methods. Remove them on the Contact methods page,
  or with `sudo mmhotline contact-methods remove --contact-method-id ID`,
  then apply again.
- **A triage agent run was active** while the agent's setup changed:
  `a triage agent run was active, so the new agent setup was not tested. Apply the settings file again after the triage agent run ends`.
- **You removed the `mmhotline_down_alarm` section, and MMHotline could
  not reach the alarm in Cloudflare.** Removing the section first tells the
  alarm to hold no messages to send if check-ins stop; when that fails, the
  setup stays. Otherwise the alarm would still wake people once MMHotline
  stopped checking in. Fix the connection to Cloudflare and apply again.

### The check lists problems

`settings apply` checks the file the same way, and with any problem it
applies nothing:

```text
on_call_schedule.escalate_if_unclaimed_after_minute: unknown key
on_call_schedule.escalate_if_unclaimed_after_minutes: required
The settings file has problems, so nothing applied and the earlier settings stay. Fix the settings file problems, then apply the settings file again.
```

MMHotline refuses an unknown key, so a misspelled key never passes
silently. Every key inside a section you use is required, unless the
[settings file reference](../reference/settings-file.md) says it is
optional. Leaving out a whole section, such as `status_page`, turns that
feature off.

The check can also print notes under the heading `Notes, which do not stop
MMHotline from applying the settings file:`. A note does not fail the check
or the apply. `settings apply` and `settings show` print notes indented
under their part, such as the next step after a sending service applies
for the first time:

```text
sending_services.pushover: applied at 2026-11-15T03:54:04Z.
  sending_services.pushover: Next step: Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification.
```

### MMHotline restarted

Four keys change only when MMHotline restarts: `web_url`,
`web_listen_address`, `alert_intake_listen_address`, and
`cloudflare_access`. When one of them changed, MMHotline applies nothing at
first. `settings apply` restarts MMHotline with
`systemctl restart mmhotline.service`, waits for it to answer, and then
applies the file, and says so:

```text
Restarting MMHotline to apply the settings that require a restart: web_listen_address.
Settings file: /etc/mmhotline/mmhotline_settings.yml
Settings file first comment line: Written by hand with sudoedit. Apply a change with sudo mmhotline settings apply.
Restarted MMHotline first, because these keys change only when it restarts: web_listen_address.
```

No alert is lost during the restart: a tool that sends alerts sends an
alert again until MMHotline confirms it. If MMHotline does not answer
within one minute after the restart, such as when it cannot use a new
listen address, the command exits 1 and says to read
`sudo journalctl -u mmhotline -n 50`.

### The owner or mode of the file is wrong

```text
/etc/mmhotline/mmhotline_settings.yml can be read by every user on the server (mode 0644), and it holds the team's secrets. Fix the settings file owner and mode with: sudo chown root:mmhotline /etc/mmhotline/mmhotline_settings.yml; sudo chmod 0640 /etc/mmhotline/mmhotline_settings.yml
```

Run the two commands it prints, then apply again. `settings check` skips
the owner and mode, so that it can run on a laptop; `settings apply`,
`install`, and MMHotline's start check them.

### Another apply is running

One apply runs at a time; a second one waits for the first. The command
waits without a time limit, because an outside test can take minutes.
Ctrl-C stops the waiting, not the apply: run `sudo mmhotline settings show`
to see how it ended.

### MMHotline starts with a file that has problems

At start, MMHotline reads the settings file too. If the file has problems
or the wrong owner or mode, MMHotline starts with the settings it last
applied, so alerts keep flowing, and sends the administrators a warning
that names the keys with problems. On the very first start, with no
settings applied yet, MMHotline refuses to start and prints the problems.

### A coding agent reads the results

Each of the three commands takes `--json` and then prints one JSON object
instead of sentences. The exit codes are:

| Command | 0 | 1 | 4 |
| --- | --- | --- | --- |
| `settings check` | No problems, with or without notes | The file has problems | `--settings-file` is missing, or another argument is wrong |
| `settings apply` | Every part applied or was unchanged | The file has problems, or a part did not apply | MMHotline does not answer, or an argument is wrong |
| `settings show` | MMHotline answered | | MMHotline does not answer, or an argument is wrong |

When MMHotline is not running, `settings apply` and `settings show` print
`mmhotline serve is not running at command socket /run/mmhotline/command.sock`.
Start it with `sudo systemctl start mmhotline`.

The [command reference](../reference/mmhotline-command.md#mmhotline-settings-apply) lists the JSON
fields.
