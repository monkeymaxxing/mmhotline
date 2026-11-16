# Connect a tool that sends alerts

**Who acts:** an administrator with `sudo` on the MMHotline server, or
that person's coding agent, together with whoever sets up the tool. The
tool can be a monitoring program, a script, or a scheduled job.

**When to use this page:** a tool should reach the person on call through
MMHotline: its urgent problems wake the person, and MMHotline notices when
the tool itself goes silent.

**What this page does:** you add the tool to MMHotline's settings file as
an **alert source**, give the tool three values, send a test SOS from the
server, and turn on the tool's **check-ins**.

**Terms:**

- An **alert source** is MMHotline's record of one tool that sends alerts.
- A **signing secret** is 64 hexadecimal characters that the tool and
  MMHotline share. The tool signs each request with it, so MMHotline knows
  the request came from that tool.
- The **alert intake address** is where tools send alerts, such as
  `http://10.20.0.1:8484`: a private network address of the MMHotline
  server. It is not the address that people open, because Cloudflare Access
  would refuse a tool.
- A **check-in** is a short signed message that says the tool still runs.
  When check-ins stop, MMHotline alerts, so a dead monitoring tool does not
  look like a quiet night.

## Choose the route

- **The tool is [MMWatchdog](https://github.com/monkeymaxxing/mmwatchdog):**
  MMWatchdog has MMHotline sending built in. Follow
  [Connect MMWatchdog](#connect-mmwatchdog).
- **Any other tool or script:** follow
  [Connect a tool](#connect-a-tool).
- **Show the monitoring tool's servers on MMHotline's dashboard:** see
  [Show the tool's servers on the dashboard](#show-the-tools-servers-on-the-dashboard).

## Before you start

- MMHotline is installed, and the person on call has a Pushover
  notification or an automated voice call on their Contact methods page.
  Without one, an SOS wakes nobody: MMHotline then sends the
  administrators the warning "MMHotline could not wake ... for an SOS
  incident".
- The tool's server can reach the MMHotline server's alert intake address,
  such as over WireGuard. `alert_intake_listen_address` in the settings
  file sets it. A tool on the MMHotline server itself uses the same address.
- Run the tool on a different server from MMHotline when you can. A
  server that dies takes both down, and then nothing notices.

## Connect a tool

1. **Make a signing secret in a file that only root reads:**

   ```sh
   sudo sh -c 'umask 077; openssl rand -hex 32 > /root/monitoring-signing-secret'
   ```

   **Check:** `sudo wc -c /root/monitoring-signing-secret` prints 65: 64
   characters and a line break.

2. **Add the alert source to the settings file.** Open the file with
   `sudoedit /etc/mmhotline/mmhotline_settings.yml` and add the tool under
   `alert_sources`, with the secret from step 1 in place of
   `THE 64 HEXADECIMAL CHARACTERS`:

   ```yaml
   alert_sources:
     - alert_source_id: monitoring                      # your ID: 1 to 64 letters, digits, _ and -
       name: Monitoring                                 # shown on pages and in alerts
       signing_secrets: ["THE 64 HEXADECIMAL CHARACTERS"]
       warn_after_no_check_in_for_minutes: 2            # a warning, which wakes nobody
       sos_after_no_check_in_for_minutes: 5             # an SOS, which wakes the person on call
   ```

   Every key is required. `system` and `manual` are refused as IDs,
   because they name MMHotline's own alerts. A source normally has one
   signing secret; it has two only while the tool moves to a new one. The
   two silence times say how long the tool may stay without a check-in;
   the warning first allows for a passing network failure, and each takes
   1 to 60 minutes.

   **A tool that never checks in,** such as a nightly job, is never
   reported silent: MMHotline starts watching for silence only when a
   source's first check-in arrives. The two keys are still required; write,
   for example, `warn_after_no_check_in_for_minutes: off` and
   `sos_after_no_check_in_for_minutes: 60`.

   Then check and apply the file, as
   [Change the settings](change-the-settings.md) says:

   ```sh
   sudo mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
   sudo mmhotline settings apply
   ```

   **Check:** the apply prints `alert_sources: applied.` and ends with
   `Every part of the settings file is applied.`

3. **Read the tool's addresses:**

   ```sh
   sudo mmhotline alert-sources list
   ```

   ```text
   Alert source Monitoring (monitoring) is active.
   Intake URL: http://10.20.0.1:8484/alert-intake/monitoring
   Check-in URL: http://10.20.0.1:8484/check-in/monitoring
   No check-in has arrived yet. MMHotline starts watching for missing check-ins when the first check-in arrives.
   ...
   Alert intake address: http://10.20.0.1:8484
   ```

   **Check:** the first line says `is active`. The Alert sources page shows
   the same addresses under **Connection details**. Neither shows the
   secret.

4. **Send a test SOS from the MMHotline server.** This Python program signs
   one SOS with the secret from step 1 and sends it as the tool would. For
   another source, change the secret file's path and the ID at the end of
   the URL; the example's ID is `monitoring`:

   ```sh
   sudo python3 - <<'PYTHON'
   import hashlib, hmac, json, time, urllib.request
   from pathlib import Path

   signing_secret = bytes.fromhex(Path('/root/monitoring-signing-secret').read_text().strip())
   body = json.dumps({
       "sender_alert_id": "backup-test-" + str(time.time_ns()),
       "sender_incident_key": "backup-test",
       "severity": "sos",
       "title": "Backup failed: test SOS",
       "message": "This is a test. Claim the incident in MMHotline, then resolve it.",
   }, separators=(",", ":")).encode()
   timestamp = str(int(time.time()))
   signature = hmac.new(signing_secret, timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
   request = urllib.request.Request("http://10.20.0.1:8484/alert-intake/monitoring", data=body, headers={
       "Content-Type": "application/json",
       "Mmhotline-Signature": f"timestamp_unix_seconds={timestamp},hmac_sha256_hex={signature}",
   })
   with urllib.request.urlopen(request, timeout=15) as response:
       print(response.read().decode())
   PYTHON
   ```

   ```text
   {"mmhotline_alert_id":"11eb3ef896a5e250a9ebc2527a8990a9","mmhotline_incident_id":"52f9135fc0ee491e0e6c49fc40bedc25","already_received":false}
   ```

   **Check:** the answer has an `mmhotline_incident_id`, which an answer
   has only when the alert opened or joined an incident, and the person on
   call's phone sounds. `sudo mmhotline schedules show` names that person,
   with unavailability and taking on-call counted, as the dashboard's
   **On call** panel does:

   ```text
   On call now: Carlos Example (carlos@example.com). Backup now: Barbara Example (barbara@example.com).
   ```

   `sudo mmhotline incidents list` shows the incident:

   ```text
   SOS incident 52f9135fc0ee491e0e6c49fc40bedc25: Backup failed: test SOS
   Status: Not claimed yet. Opened at 2026-11-15T03:56:59Z.
   ```

   If the answer is an error, see [If something is different](#if-something-is-different).

5. **End the test SOS.** Either the person on call claims and resolves it
   on the incident's page, as [Handle an incident](handle-an-incident.md)
   says, or you resolve it as the tool would: run the program from step 4
   again with `"severity": "resolved-sos"`. An alert with
   `resolved-sos` resolves the open incident whose SOS has the same
   `sender_incident_key`, here `backup-test`.

   **Check:** `sudo mmhotline incidents list` prints
   `Listed 0 SOS incidents not resolved, newest first.`

6. **Give the tool its three values:** the alert intake address
   (`http://10.20.0.1:8484`), the alert source ID (`monitoring`), and the
   signing secret. Give the secret through a file that only the tool's
   account reads, or through your deploy tool's vault, never through a
   chat or a command line. The tool sends alerts to the intake URL and
   check-ins to the check-in URL from step 3: the alert intake address
   followed by `/alert-intake/` or `/check-in/` and the source ID. If you
   write the sender yourself, the convention for its two settings is
   `MMHOTLINE_ALERT_INTAKE_URL` and `MMHOTLINE_SOURCE_SIGNING_SECRET`.

7. **Make the tool check in every 30 seconds.** A check-in is a signed
   request to the check-in URL, signed the same way as the alert in step 4,
   with this body:

   ```json
   {"sent_at":"2026-11-15T04:02:47Z","sent_by_node_name":"monitor01","nodes_in_whole_node_maintenance":[]}
   ```

   MMHotline answers `{"check_in_received_at":"2026-11-15T04:02:47Z"}`.

   **Check:** `sudo mmhotline alert-sources list` shows the check-in, and
   the source's **Check-ins and planned downtime** page on the Alert
   sources page shows its time in the team's time zone:

   ```text
   Last check-in received: 2026-11-15T04:02:47Z. Sent from server: monitor01.
   ```

   MMHotline's alerts about silence say that a source "normally checks in
   with MMHotline every 30 seconds", and MMWatchdog sends at that pace.
   MMHotline refuses more than 12 check-ins a minute from one source.

## The whole settings file of this example

A team with a monitoring tool that checks in, and a nightly backup job that
does not, has this settings file. Each secret is the 64 characters made in
step 1 for that tool:

```yaml
# Written by hand with sudoedit. Apply a change with sudo mmhotline settings apply.
team_name: Operations
web_url: https://ops.example.com
web_listen_address: 127.0.0.1:8480
alert_intake_listen_address: 10.20.0.1:8484

sending_services:
  pushover:
    application_token: YOUR_30_CHARACTER_APPLICATION_TOKEN

people:
  - name: Barbara Example
    sign_in_email_address: barbara@example.com
    role: administrator

  - name: Carlos Example
    sign_in_email_address: carlos@example.com
    role: member

on_call_schedule:
  schedule_type: weekly_rotation
  rotation_sign_in_email_addresses: [barbara@example.com, carlos@example.com]
  backup_option: next_in_rotation
  escalate_if_unclaimed_after_minutes: 15

  time_zone: America/New_York
  handoff_weekday: 1
  handoff_hour: 9
  handoff_minute: 0
  first_start_date: "2026-11-02"

alert_sources:
  - alert_source_id: monitoring
    name: Monitoring
    signing_secrets: ["THE 64 HEXADECIMAL CHARACTERS"]
    warn_after_no_check_in_for_minutes: 2
    sos_after_no_check_in_for_minutes: 5

  - alert_source_id: backup_script
    name: Nightly backup
    signing_secrets: ["THE 64 HEXADECIMAL CHARACTERS"]
    warn_after_no_check_in_for_minutes: off
    sos_after_no_check_in_for_minutes: 60

rules_for_running_agent_playbooks: {}
services_whose_changes_a_person_must_approve: []
```

## Connect MMWatchdog

1. **Do steps 1 to 3 of [Connect a tool](#connect-a-tool) for MMHotline's
   side,** with the secret made on the MMWatchdog fleet hub, or on the one
   MMWatchdog node that sends alerts, as
   [MMWatchdog's MMHotline destination](https://github.com/monkeymaxxing/mmwatchdog/blob/main/docs/modules/mmhotline.md#set-up)
   says. Use an ID that names the sender, such as `mmwatchdog_fleet_hub`.
2. **Add the `mmhotline` destination in MMWatchdog** with the alert intake
   address, the alert source ID, and the secret file, then validate and
   reload MMWatchdog, as that page says. MMWatchdog then sends its alerts
   and a check-in every 30 seconds by itself.

   **Check:** within a minute, `sudo mmhotline alert-sources list` shows
   `Last check-in received:` for the source.
3. **When MMHotline runs on the MMWatchdog fleet hub's server,** show
   MMWatchdog's servers on the dashboard, as
   [Show the tool's servers on the dashboard](#show-the-tools-servers-on-the-dashboard)
   says, with `fleet_view_command: [/usr/local/bin/mmwatchdog, fleet, --json]`.

## Show the tool's servers on the dashboard

The dashboard's **Servers** part can show your monitoring tool's servers:
those that need attention first, with their failing checks, then the
healthy ones by name. It needs a command on the MMHotline server that
prints the tool's fleet as JSON.

1. **Add the command to the settings file,** each argument as its own list
   item, the program first with its absolute path:

   ```yaml
   fleet_view_command: [/usr/local/bin/mmwatchdog, fleet, --json]
   ```

   No shell reads the command, so quotes, variables, and pipes in it mean
   nothing.

2. **Apply the file** with `sudo mmhotline settings apply`, so the
   dashboard knows the command exists.

   **Check:** the dashboard's **Servers** part shows the servers. If it
   says why it cannot, the command's errors are in
   `sudo journalctl -u 'mmhotline-fleet-view@*'`.

Reason for how it runs: such a command needs root, and MMHotline runs as
its own user, which cannot gain root. So MMHotline asks systemd to run the
command as root, through `/run/mmhotline-fleet-view.sock`, which only the
`mmhotline` user may open. The command runs with a read-only file system,
no network but local sockets, only `PATH` in its environment, and a
20-second limit. Only root can set the command, because only root can
change the settings file; MMHotline can only ask for a run of it. The
command is read from the settings file at each run, so a change works at
the next run, and a problem elsewhere in the settings file does not stop
it.

## Change a source later

- **Move the tool to a new signing secret** without missing an alert:
  1. List both secrets, `signing_secrets: ["OLD", "NEW"]`, and apply.
  2. Give the tool the new secret.
  3. Check that the tool's requests still succeed: its check-ins still
     appear in `sudo mmhotline alert-sources list`.
  4. Remove the old secret, `signing_secrets: ["NEW"]`, and apply. From
     then on, the old secret gets `401 source authentication failed`.
- **Stop accepting a tool:** remove it from `alert_sources` and apply.
  `alert_sources: []` is allowed. The source's card on the Alert sources
  page and `sudo mmhotline alert-sources list` then say it is disabled,
  `Alert source Monitoring (monitoring) is disabled.`, and its requests get
  `401`. Its alerts and incidents stay in the history. Listing it again
  turns it back on.
- **Change when silence alerts:** change the two
  `..._after_no_check_in_for_minutes` values and apply. Each takes 1 to 60
  minutes. `warn_after_no_check_in_for_minutes: off` sends no warning. With
  a warning, the SOS time must be longer, so the warning comes first.
- **The tool's server goes down on purpose:** declare planned downtime, so
  its silence wakes nobody; see
  [planned downtime](declare-planned-downtime.md).

## If something is different

| The tool gets | What it means | What to do |
| --- | --- | --- |
| `401 source authentication failed` | The secret is wrong, the source is disabled, or the tool's clock is more than 5 minutes away from the MMHotline server's | Compare the secret with the settings file; check that `alert-sources list` says `is active`; check both clocks |
| `400 sender_alert_id already used for a different alert; give each new alert its own sender_alert_id` | The tool reused an alert ID for a new alert | Give each new alert its own `sender_alert_id` |
| `200` with `"already_received":true` | The tool sent the same alert again | Nothing: a retry of one alert keeps its `sender_alert_id`, and MMHotline counts it once |
| A refused connection, or no answer | The tool cannot reach the alert intake address | Check the network path and `alert_intake_listen_address`; a request to the web address is refused by Cloudflare Access |

The Alert sources page shows **Rejected requests in the last 24 hours:**
with the reasons, for requests that MMHotline refused.

**The SOS arrived, but no phone sounded.** MMHotline sent the
administrators a warning, such as:

```text
MMHotline could not wake Carlos Example for the SOS incident “Backup failed: test SOS”. Nothing set up can wake them: a person needs an automated voice call or a Pushover notification that receives SOS alerts. Until they add one on their Contact methods page, MMHotline skips them when it wakes people for an SOS.
```

The person adds a Pushover notification or an automated voice call on
their Contact methods page. Until then, MMHotline moves on to the next
person at once.

## How the silence alerts work

MMHotline starts watching a source when its first check-in arrives; a
source that never checks in is never reported silent. Any signed alert from
the source also counts as hearing from it. With the values from step 2, a
tool that stops checking in brings, 2 minutes after its last check-in:

```text
[Warning] “Monitoring” hasn't checked in with MMHotline for 2 minutes: it may have stopped, or its server may be down
```

and 5 minutes after its last check-in, an SOS to the person on call:

```text
[SOS] “Monitoring” hasn't checked in with MMHotline for 5 minutes: it may have stopped, or its server may be down
```

When the check-ins come back, an unclaimed SOS about the silence resolves
by itself. A claimed one stays with the person who claimed it, who gets a
notice and resolves it. Only a check-in clears an alert about silence that
already went out; a signed alert does not.

The source's **Check-ins and planned downtime** page, from the Alert
sources page, says where the source stands: **Waiting for the first
check-in**, check-ins arriving, **Receiving alerts, but no recent
check-in**, no recent check-in, or planned downtime. Under
**Missing-check-in rules** it shows the **Warning** and **Wake the person
on call** times.

A tool can also list servers in maintenance in its check-ins
(`nodes_in_whole_node_maintenance`). Maintenance of the server that sends
the check-ins counts as planned downtime for the source.

## Rules for the requests a tool sends

- **Severities:** `info` and `warn` reach people by their contact methods'
  choices and wake nobody; `sos` opens an incident and wakes the person on
  call. `resolved-info`, `resolved-warn`, and `resolved-sos` say a problem
  is over.
- **`sender_alert_id`** (optional, but use it): the tool's own ID for one
  alert. Keep it when retrying the same alert; use a new one for each new
  alert. Without it, MMHotline recognizes a retry only by its identical
  signed request.
- **`sender_incident_key`** (optional): the tool's name for one problem.
  Another SOS with the same key joins the open incident, and a
  `resolved-sos` with the key resolves it, claimed or not. Choose one key
  for each problem: a nightly job that uses one key every night joins a
  new failure to an earlier night's incident while that incident is still
  open.
- **The signature:** the header `Mmhotline-Signature` holds exactly
  `timestamp_unix_seconds=...,hmac_sha256_hex=...`, in that order. The
  HMAC-SHA256 covers the timestamp, a `.`, and the body. The timestamp must
  be within 5 minutes of the MMHotline server's clock.
- **The answer:** MMHotline saves an alert before it answers, so a `200`
  means the alert is safe. The answer is signed with the same secret. A
  tool that runs for real retries an alert until it gets a correctly signed
  answer or a refusal; the test program in step 4 does not check the
  answer's signature.

The [alert requests reference](../reference/alert-requests.md) lists every
field, limit, and answer, and how to check the answer's signature.
