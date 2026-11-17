# Get started with MMHotline

**Who acts:** you, or your coding agent, on the Linux server that will run
MMHotline, with `sudo`. You do the parts in Cloudflare and in the Pushover
app yourself; a coding agent tells you what to create there.

**What this page does:** you install MMHotline, put your phone on call, and
have MMHotline send a test SOS that wakes you. You acknowledge it on your
phone, and MMHotline closes it. It takes about an hour, most of it in
Cloudflare.

**Terms:** an **SOS** is the urgent alert that wakes the person on call,
even at night. An SOS opens an **incident**, MMHotline's record of the
problem. You **claim** an incident to tell the team you are handling it,
and you **resolve** it when the problem is over. An **alert source** is a
tool that sends alerts to MMHotline, such as your monitoring tool.

## Before you start

- **A Linux server with systemd,** such as Debian 12 or 13 or Ubuntu 24.04,
  which MMHotline is tested on, and an account that can use `sudo`, with
  `curl` and Python 3.
- **A private network address on the server** where your tools will send
  alerts, such as the WireGuard address `10.20.0.1`. `ip -brief addr` lists
  the server's addresses.
- **A Cloudflare account with Zero Trust,** and a domain whose DNS
  Cloudflare manages, such as `example.com`. People sign in to MMHotline
  through Cloudflare Access, at a host name such as `ops.example.com`. A
  team without Cloudflare can still follow this page: see
  [If something is different](#if-something-is-different).
- **A Pushover account,** and the Pushover app on your phone, signed in.
  Pushover sends the notification that wakes you; the phone opens
  MMHotline in its browser and needs no other app. A team can also wake
  people with phone calls through Twilio, set up later. Only a Pushover
  notification and a phone call are meant to wake a person: never give
  someone only email or chat for an SOS.

The examples use the team name `Operations`, the person Barbara Example,
the host name `ops.example.com`, and the private address `10.20.0.1`. Put
in your own.

## Steps

1. **Download the release and check it.** On the server, download the
   archive for the server's processor and the checksum file, check the
   archive, and unpack it:

   ```sh
   curl --fail --location --remote-name-all \
     https://github.com/monkeymaxxing/mmhotline/releases/download/v0.1.0/mmhotline-0.1.0-linux-amd64.tar.gz \
     https://github.com/monkeymaxxing/mmhotline/releases/download/v0.1.0/SHA256SUMS
   sha256sum --check --ignore-missing SHA256SUMS
   tar -xzf mmhotline-0.1.0-linux-amd64.tar.gz
   cd mmhotline-0.1.0-linux-amd64
   ```

   Use the newest release's version in place of `0.1.0`. `amd64` is for a
   server where `uname -m` prints `x86_64`. Where it prints `aarch64`, use
   `arm64` in both file names.

   **Check:** `sha256sum` prints
   `mmhotline-0.1.0-linux-amd64.tar.gz: OK`. Reason: the check proves the
   download is the release, unchanged. If it prints `FAILED`, delete the
   download and do not run it.

2. **Make the alert source's signing secret.** Your monitoring tool signs
   each alert with this secret:

   ```sh
   sudo sh -c 'umask 077; openssl rand -hex 32 > /root/monitoring-signing-secret'
   ```

   **Check:** `sudo wc -c /root/monitoring-signing-secret` prints `65`:
   64 characters and a line break.

3. **Write the settings file.** MMHotline reads the team's settings from
   one file, `/etc/mmhotline/mmhotline_settings.yml`, which the
   [settings file reference](reference/settings-file.md) describes key by
   key. Make the folder and an
   empty file that only root can read, and open it:

   ```sh
   sudo install -d -m 0755 /etc/mmhotline
   sudo install -m 0640 /dev/null /etc/mmhotline/mmhotline_settings.yml
   sudoedit /etc/mmhotline/mmhotline_settings.yml
   ```

   Paste the file below, then change the values to yours:

   - `web_url`: the address people will open, at the host name you put
     behind Cloudflare Access in step 7.
   - `alert_intake_listen_address`: the server's private address, with the
     port `8484`.
   - `people`: you, with the email address you will sign in with at
     Cloudflare, as `administrator`.
   - `on_call_schedule`: puts you on call every week from Monday 09:00 in
     your time zone. `first_start_date` must be a Monday. With
     `backup_option: everyone_available`, an SOS that nobody claims in time
     wakes everyone else who is available. Start with yourself alone; you
     add the team and its rotation after this page.
   - `web_url` is also the start of the links in notifications.
   - `signing_secrets`: leave `THE 64 HEXADECIMAL CHARACTERS` for now; the
     command below puts in the secret from step 2.

   ```yaml
   # Written by hand with sudoedit. Apply a change with sudo mmhotline settings apply.
   team_name: Operations
   web_url: https://ops.example.com
   web_listen_address: 127.0.0.1:8480
   alert_intake_listen_address: 10.20.0.1:8484

   rules_for_running_agent_playbooks: {}
   services_whose_changes_a_person_must_approve: []
   sending_services: {}

   people:
     - name: Barbara Example
       sign_in_email_address: barbara@example.com
       role: administrator

   on_call_schedule:
     escalate_if_unclaimed_after_minutes: 5
     schedule_type: weekly_rotation
     time_zone: America/New_York
     handoff_hour: 9
     handoff_minute: 0
     handoff_weekday: 1
     first_start_date: "2026-11-02"
     rotation_sign_in_email_addresses: [barbara@example.com]
     backup_option: everyone_available

   alert_sources:
     - alert_source_id: monitoring
       name: Monitoring
       signing_secrets: ["THE 64 HEXADECIMAL CHARACTERS"]
       warn_after_no_check_in_for_minutes: 2
       sos_after_no_check_in_for_minutes: 5
   ```

   Keep `web_listen_address` as it is: MMHotline serves its pages only on
   the server itself, where Cloudflare's connector reaches them. The three
   empty settings are for parts you set up later, such as the triage
   agent, the team's coding agent that investigates alerts. A coding agent
   without a terminal writes the file with any tool that keeps its owner
   and mode.

   Save the file, then put the secret in without showing it on the screen:

   ```sh
   sudo python3 -c "from pathlib import Path; f = Path('/etc/mmhotline/mmhotline_settings.yml'); f.write_text(f.read_text().replace('THE 64 HEXADECIMAL CHARACTERS', Path('/root/monitoring-signing-secret').read_text().strip()))"
   ```

   Reason: a secret never goes in a chat, a command line, or a repository.
   The settings file holds the team's secrets, and only root and MMHotline
   can read it.

   **Check:** `sudo grep -c 'THE 64' /etc/mmhotline/mmhotline_settings.yml`
   prints `0`, and `sudo ls -l /etc/mmhotline/` shows
   `-rw-r-----` and `root` for `mmhotline_settings.yml`.

4. **Check the settings file:**

   ```sh
   sudo ./mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
   ```

   **Check:** it prints
   `The settings file /etc/mmhotline/mmhotline_settings.yml has no problems.`
   Otherwise each line names a key and what is wrong; fix the file and
   check again.

5. **Install MMHotline:**

   ```sh
   sudo ./mmhotline install
   ```

   ```text
   Checks: Linux, root, and systemd confirmed
   Unix user: created mmhotline with no login shell
   ...
   Database: created mmhotline.db for Operations
   Settings file: set owner root, group mmhotline, and mode 0640, so MMHotline can read the settings file and only root can change the settings file: /etc/mmhotline/mmhotline_settings.yml
   ...
   MMHotline: https://ops.example.com
   Next: set up cloudflared and a Cloudflare Access application for https://ops.example.com, ...
   ```

   **Check:** the output ends with the `MMHotline:` and `Next:` lines, and
   `mmhotline version` prints the release, such as
   `mmhotline 0.1.0 (COMMIT, linux/amd64, modified=false)`, with the
   release's full commit hash in place of `COMMIT`.

   The install copied the program to `/usr/local/bin/mmhotline` and
   started MMHotline as a systemd service; from now on, run `mmhotline`
   without `./`. You no longer need the downloaded files.
   [Install MMHotline](how-to/install.md#what-the-install-does) says what
   else it made.

6. **Connect Pushover.** On the Pushover website, create an application
   named MMHotline, as [the Pushover guide](how-to/connect-pushover.md)
   shows, and copy its API token. In the settings file, replace
   `sending_services: {}` with:

   ```yaml
   sending_services:
     pushover:
       application_token: YOUR_30_CHARACTER_APPLICATION_TOKEN
   ```

   Then apply the file:

   ```sh
   sudo mmhotline settings apply
   ```

   **Check:** among the lines it prints are:

   ```text
   sending_services.pushover: applied.
     sending_services.pushover: Next step: Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends an emergency test alert.
   ```

   and it ends with `Every part of the settings file is applied.`
   MMHotline checked the token with Pushover before it saved it.

7. **Add your phone.** A **contact method** is where a person gets
   notifications, here Pushover on your phone. Your Pushover **user key**,
   on the Pushover website's main page after you sign in, is 30 characters
   and differs from the application's token. Add it as your contact
   method, with your own sign-in email address from `people`:

   ```sh
   sudo mmhotline contact-methods add --sign-in-email-address barbara@example.com \
     --contact-method-type pushover_notification \
     --pushover-user-key-file -
   ```

   The command waits for the key: paste it, press Enter, then Ctrl-D. A
   coding agent pipes the key in instead, from where the person put it.
   Reason: a key typed on the command line would stay in the shell history
   and show in the list of running processes.

   **Check:** your phone gets an emergency test alert that sounds until you
   acknowledge it in Pushover, and `sudo mmhotline contact-methods list`
   then says `Last test message: accepted`.

   Before you rely on it at night, follow
   [Make an emergency alert wake you](how-to/connect-pushover.md)
   on the phone, so the alert sounds in silent mode too.

8. **Put MMHotline behind Cloudflare Access.** Follow
   [Put MMHotline behind Cloudflare Access](how-to/put-mmhotline-behind-cloudflare-access.md),
   steps 1 to 6, then come back here. Steps 9 and 10 work without it from
   the server, but the links in notifications open only once it is done.
   In short: install `cloudflared` on
   the server, create a tunnel that sends `ops.example.com` to
   `http://127.0.0.1:8480`, create an Access application that lets your
   company's people sign in, and add the two values Cloudflare gives you
   to the settings file.

   **Check:** you opened `https://ops.example.com` on your phone, signed
   in at Cloudflare, and MMHotline's dashboard opened.

9. **Send a test SOS.** MMHotline sends itself an SOS, wakes the person on
   call, and waits for you to acknowledge it on your phone:

   ```sh
   sudo mmhotline incidents send-test-sos
   ```

   **Check:** your phone sounds with a Pushover notification titled
   **Test of the alerts. Nothing is wrong: acknowledge this.** Acknowledge
   it. The command then prints who acknowledged it, how, and when, and
   exits 0:

   ```text
   MMHotline sent a test SOS to the person on call. Waiting up to 10 minutes for someone to acknowledge it on the phone.
   Incident: https://ops.example.com/incidents/acc896d49ce5b2ef40cd2c944da4f08f
   Barbara Example claimed the test by acknowledging the alert in Pushover, at 2026-11-15T04:41:20Z.
   MMHotline resolved the test after the claim, at 2026-11-15T04:41:20Z.
   The test SOS worked: a person acknowledged it on the phone.
   ```

   MMHotline closes the test itself, so there is nothing more to claim or
   resolve. `sudo mmhotline incidents list` prints
   `Listed 0 SOS incidents not resolved, newest first.`

   IF THE PHONE STAYS SILENT, THE COMMAND EXITS 1 AFTER 10 MINUTES. CHECK
   THAT THE PUSHOVER APP IS INSTALLED AND SIGNED IN ON THE PHONE, AND THAT
   CRITICAL ALERTS ARE ALLOWED FOR PUSHOVER IN THE PHONE'S SETTINGS.

   Commands print times in UTC; MMHotline's pages show them in the team's
   time zone. `sudo mmhotline schedules show` says who the schedule puts
   on call now:
   `On call now: Barbara Example (barbara@example.com). Backup now: nobody else is available.`

   The test SOS starts inside MMHotline. It does not pass through your
   monitoring tool or MMWatchdog. The signed check-ins of MMWatchdog prove
   that path every minute, and MMHotline warns when they stop.

You now know that MMHotline wakes you and that your phone's
acknowledgement reaches MMHotline. To see the pages a real incident uses,
read [Handle an incident](how-to/handle-an-incident.md).

## What to do next

- **Connect your monitoring tool:** give it the address, the alert source
  ID, and the secret, and turn on its check-ins, as
  [Connect a tool that sends alerts](how-to/connect-a-tool-that-sends-alerts.md)
  says.
- **Add your team and its schedule** to the settings file, as
  [Set up people and the on-call schedule](how-to/set-up-people-and-the-on-call-schedule.md)
  says: each person in `people`, and the people who take turns in
  `rotation_sign_in_email_addresses`. Each person then adds their own
  phone on the Contact methods page, as
  [Add contact methods](how-to/add-contact-methods.md) says. Until they do, an SOS cannot wake
  them, and MMHotline tells the administrators that it could not wake
  them.
- **Have the team's coding agent investigate alerts first, be woken when
  MMHotline itself is down, or publish a status page:** see
  [Set up the triage agent](how-to/set-up-the-triage-agent.md),
  [Set up the MMHotline-down alarm](how-to/set-up-the-mmhotline-down-alarm.md),
  and [Publish a status page](how-to/publish-a-status-page.md).
- **Read what happened from the server, or from a coding agent:**
  `sudo mmhotline incidents list`, `sudo mmhotline incidents show --incident-id ID`,
  and `sudo mmhotline alerts list`, each with `--json`. No command claims
  or resolves an incident: a person does that, or the tool that sent the
  alert resolves it.
- **Back up the database continuously,** so a lost server does not lose
  the team's history: [Continuous backup](how-to/back-up-and-restore-the-database.md).
  The install's `Backup: not set up` line points there too.

## If something is different

- **`sha256sum` prints `FAILED`** and
  `sha256sum: WARNING: 1 computed checksum did NOT match`: the download is
  damaged or is not the release. Delete it and download again.
- **The install says the settings file is missing:**

  ```text
  mmhotline: install requires the settings file /etc/mmhotline/mmhotline_settings.yml; create the settings file as root with sudoedit /etc/mmhotline/mmhotline_settings.yml and set owner root and mode 0640; ...
  ```

  Do step 3 first. The install made no change.
- **The install ends with `systemctl restart mmhotline failed`,** and the
  journal lines it prints include
  `MMHotline cannot listen on alert_intake_listen_address 10.20.0.9:8484: listen tcp 10.20.0.9:8484: bind: cannot assign requested address`:
  the address is not one of the server's own. Correct
  `alert_intake_listen_address`, and run `sudo ./mmhotline install` again.
  Running the install again is always safe; it keeps what is already
  correct.
- **`contact-methods list` says `Last test message: failed`** with
  `Pushover answered HTTP 400: user identifier is invalid`: the user key is
  wrong, or is the application's token. Remove the contact method with
  `sudo mmhotline contact-methods remove --contact-method-id ID`, using the
  ID that `contact-methods list` prints, and add it again with the right
  key.
- **The phone does not sound, but the test was `accepted`:** Pushover took
  the message, so the phone's settings stop it. Follow
  [Make an emergency alert wake you](how-to/connect-pushover.md).
- **The team has no Cloudflare:** write `web_url: http://localhost:8480`
  in step 3, skip step 8, and open MMHotline with a sign-in link, as
  [Sign in during a Cloudflare outage](how-to/sign-in-during-a-cloudflare-outage.md)
  says. The links in notifications then open only on a computer that runs
  that SSH forward.
- **A page says "Not a team member" or "Sign in through Cloudflare
  Access":** see
  [Put MMHotline behind Cloudflare Access](how-to/put-mmhotline-behind-cloudflare-access.md#if-something-is-different).
