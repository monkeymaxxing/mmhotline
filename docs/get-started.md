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
  `curl`.
- **A private network address on the server** where your tools will send
  alerts, such as the WireGuard address `10.20.0.1`. `ip -brief addr` lists
  the server's addresses.
- **Cloudflare, which a first test does not need.** For a first test,
  keep `web_url=http://localhost:8480` in step 2's block and skip step 7: see
  [The team has no Cloudflare](#if-something-is-different). So that people
  can open MMHotline's pages from their phones, you add later a
  Cloudflare account with Zero Trust and a domain whose DNS Cloudflare
  manages, such as `example.com`. People then sign in through Cloudflare
  Access, at a host name such as `ops.example.com`.
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

1. **Download the release and check it.** On the server, paste this
   block. It picks the archive for the server's processor, downloads it with
   its checksum file, checks it, and unpacks it into `~/mmhotline-0.4.0`.
   It does not install anything yet.

   ```sh
   (
   set -eu
   version=0.4.0
   case "$(uname -sm)" in
     "Linux x86_64")  arch=amd64 ;;
     "Linux aarch64") arch=arm64 ;;
     *) echo "MMHotline runs on a Linux server. There is no release for $(uname -sm)."; exit 1 ;;
   esac
   release=https://github.com/monkeymaxxing/mmhotline/releases/download/v$version
   mkdir -p ~/mmhotline-$version && cd ~/mmhotline-$version
   curl -fsSLO "$release/mmhotline-$version-linux-$arch.tar.gz"
   curl -fsSLO "$release/SHA256SUMS"
   sha256sum --check --ignore-missing SHA256SUMS
   tar -xzf "mmhotline-$version-linux-$arch.tar.gz"
   echo "Unpacked in ~/mmhotline-$version/mmhotline-$version-linux-$arch."
   echo "Run the next steps from there: cd ~/mmhotline-$version/mmhotline-$version-linux-$arch"
   )
   ```

   **Check:** it prints `mmhotline-0.4.0-linux-amd64.tar.gz: OK` (`arm64`
   on an ARM server). Reason: the check proves the download is the
   release, unchanged.

   IF IT PRINTS `FAILED`, DELETE THE FOLDER `~/mmhotline-0.4.0` AND DO NOT RUN THE PROGRAM.

   The block cannot move your shell. Go to the unpacked folder yourself,
   and stay there for the steps that run `./mmhotline`:

   ```sh
   cd ~/mmhotline-0.4.0/mmhotline-0.4.0-linux-*/
   ```

   Optionally, prove that GitHub built the archive from this repository,
   with the GitHub command line tool (`gh`):

   ```sh
   gh attestation verify ~/mmhotline-0.4.0/mmhotline-0.4.0-linux-*.tar.gz --repo monkeymaxxing/mmhotline
   ```

   By hand: `uname -sm` prints `Linux x86_64` for the `amd64` archive or
   `Linux aarch64` for the `arm64` archive. A newer release works the same
   way: change `version=` in the block.

2. **Write the settings file.** MMHotline reads the team's settings from
   one file, `/etc/mmhotline/mmhotline_settings.yml`, which the
   [settings file reference](reference/settings-file.md) describes key by
   key. The block below writes a first file for you. Change the five lines
   after `set -eu` to yours, then paste the whole block:

   - `team_name`: your team's name, as MMHotline's pages show it.
   - `name` and `email`: you, with the email address you will sign in with.
   - `private_address`: the server's private address, where your tools
     send alerts. `ip -brief addr` lists the server's addresses. A private
     address starts with `10.`, `192.168.`, or `172.16.` to `172.31.`.
   - `web_url`: keep `http://localhost:8480` for a first test without
     Cloudflare. With Cloudflare, write the address people will open, at
     the host name you put behind Cloudflare Access in step 7, such as
     `https://ops.example.com`.

   The next two lines fill themselves: the server's time zone, and last
   Monday as the day the on-call schedule starts.

   ```sh
   (
   set -eu
   team_name="Operations"
   name="Barbara Example"
   email=barbara@example.com
   private_address=10.20.0.1
   web_url=http://localhost:8480
   time_zone=$(timedatectl show --property=Timezone --value 2>/dev/null || true)
   first_monday=$(date -d 'last monday' +%F)
   file=/etc/mmhotline/mmhotline_settings.yml
   if sudo test -e "$file"; then
     echo "$file already exists, so this block changed nothing. To change it, see Change the settings."
     exit 1
   fi
   sudo install -d -m 0755 /etc/mmhotline
   sudo install -m 0640 /dev/null "$file"
   sudo tee "$file" > /dev/null <<EOF
   # Change a setting with sudoedit, then run: sudo mmhotline settings apply
   team_name: "$team_name"
   web_url: $web_url
   web_listen_address: 127.0.0.1:8480
   alert_intake_listen_address: $private_address:8484

   rules_for_running_agent_playbooks: {}
   services_whose_changes_a_person_must_approve: []
   sending_services: {}

   people:
     - name: "$name"
       sign_in_email_address: $email
       role: administrator

   on_call_schedule:
     escalate_if_unclaimed_after_minutes: 5
     schedule_type: weekly_rotation
     time_zone: ${time_zone:-UTC}
     handoff_hour: 9
     handoff_minute: 0
     handoff_weekday: 1
     first_start_date: "$first_monday"
     rotation_sign_in_email_addresses: [$email]
     backup_option: everyone_available

   alert_sources:
     - alert_source_id: monitoring
       name: Monitoring
       signing_secrets: ["THE 64 HEXADECIMAL CHARACTERS"]
       warn_after_no_check_in_for_minutes: 2
       sos_after_no_check_in_for_minutes: 5
   EOF
   echo "Wrote $file: $name is on call every week from Monday $first_monday, 09:00 ${time_zone:-UTC}."
   )
   ```

   **Check:** it prints `Wrote /etc/mmhotline/mmhotline_settings.yml:`
   with your name and last Monday's date.

   What the file says:
   - you are the team's only person, an `administrator`, on call every
     week from Monday 09:00. With `backup_option: everyone_available`, an
     SOS that nobody claims in time wakes everyone else who is available.
     You add the team and its rotation after this page.
   - `signing_secrets` holds a placeholder: step 3 puts in the real secret.
   - `web_listen_address` stays as it is: MMHotline serves its pages only
     on the server itself, where Cloudflare's connector reaches them.
   - The three empty settings are for parts you set up later, such as the
     triage agent, the team's coding agent that investigates alerts.

   MMHotline refuses a `web_url` that does not start with `https://`,
   except `http://localhost:8480` and `http://127.0.0.1:8480`. A coding
   agent without a terminal writes the same file with any tool that keeps
   its owner and mode.

3. **Make the alert source's signing secret.** Your monitoring tool signs
   each alert with this secret. MMHotline makes it, saves it in the
   settings file, and writes a copy for your monitoring tool. Run this in
   the unpacked folder from step 1:

   ```sh
   sudo ./mmhotline settings set-secret 'alert_sources[monitoring].signing_secrets' --make-new --write-to-file /root/monitoring-signing-secret
   ```

   **Check:** it prints
   `Saved alert_sources[monitoring].signing_secrets in /etc/mmhotline/mmhotline_settings.yml.`
   and `sudo wc -c /root/monitoring-signing-secret` prints `65`: 64
   characters and a line break. `sudo ls -l /etc/mmhotline/` shows
   `-rw-r-----` and `root` for `mmhotline_settings.yml`.

   The command never prints the secret. Reason: a secret never goes in a
   chat, a command line, or a repository, and the settings file holds the
   team's secrets. The command ends with `Run: sudo mmhotline settings apply`.
   Do not run that yet: step 5 installs MMHotline and applies the file.

4. **Check the settings file.**

   RUN THIS IN THE UNPACKED FOLDER, `~/mmhotline-0.4.0/mmhotline-0.4.0-linux-*/`. IF YOU ARE ELSEWHERE, `./mmhotline` IS NOT FOUND.

   ```sh
   sudo ./mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
   ```

   **Check:** it prints
   `The settings file /etc/mmhotline/mmhotline_settings.yml has no problems.`
   Otherwise each line names a key and what is wrong; fix the file and
   check again.

5. **Install MMHotline.** Stay in the unpacked folder from step 1:

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

   **Check:** the output ends with the `MMHotline:` line, followed by a
   `Next:` line when `web_url` starts with `https://` (with
   `http://localhost:8480`, nothing follows), and
   `mmhotline version` prints the release, such as
   `mmhotline 0.4.0 (COMMIT, linux/amd64, modified=false)`, with the
   release's full commit hash in place of `COMMIT`.

   The install copied the program to `/usr/local/bin/mmhotline` and
   started MMHotline as a systemd service; from now on, run `mmhotline`
   without `./`. You no longer need the downloaded files.
   [Install MMHotline](how-to/install.md#what-the-install-does) says what
   else it made.

6. **Connect Pushover and add your phone.** On the Pushover website,
   create an application named MMHotline, as
   [the Pushover guide](how-to/connect-pushover.md#steps) shows, and copy
   its API token. Then run this one command on the server. It asks for the
   token and for your Pushover **user key**, saves the token, and adds your
   phone as your **contact method**, the place where you get
   notifications:

   ```sh
   sudo mmhotline connect-pushover
   ```

   From your laptop, run it through SSH with `-t`, which gives the command
   your terminal:

   ```sh
   ssh -t barbara@server.example.com sudo mmhotline connect-pushover
   ```

   RUN THIS YOURSELF IN YOUR TERMINAL. YOUR CODING AGENT CAN'T TYPE AT A HIDDEN PROMPT.

   A coding agent never composes its own shell line for a secret: it gives
   you this command to run.

   RUN THE COMMAND FIRST. PASTE EACH VALUE ONLY WHEN IT ASKS FOR IT.

   Pasted at the shell prompt instead, a value runs as a command and stays
   in the shell history. The command asks, in this order:

   - `Pushover application token (nothing shows as you paste):` Paste the
     application's API token, then press Enter. Nothing shows as you
     paste. MMHotline saves the token in the settings file and applies the
     file, which checks the token with Pushover.
   - `Your sign-in email address:` Only when `people` in the settings file
     lists more than one person. With one person, the command says whose
     phone it adds.
   - `Your Pushover user key (nothing shows as you paste):` Your own user
     key is at the top of pushover.net after you sign in, labelled "Your
     User Key". It is 30 characters and differs from the application's
     token. MMHotline checks it with Pushover.
   - `Send alerts to which device? Press Enter for all devices:` Only when
     your Pushover account has more than one device. Press Enter for all
     of them, or type one device's name, such as `phone`, for alerts on
     that device only.

   Reason: the token and the key are secrets. At a hidden prompt, they
   never show on the screen or in a coding agent's chat, and they stay out
   of the shell history and the list of running processes.

   USE YOUR OWN PUSHOVER USER KEY, NOT A GROUP KEY. AN ACKNOWLEDGEMENT FROM A GROUP CANNOT CLAIM THE INCIDENT.

   A group key comes from a delivery group's page. A group key still wakes
   phones, but acknowledging on a phone does not claim the incident for
   you. MMHotline warns you when you add one.

   **Check:** among the lines it prints are:

   ```text
   sending_services.pushover: applied.
     sending_services.pushover: Next step: Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification.
   ```

   and `Every part of the settings file is applied.` MMHotline checked the
   token with Pushover before it started using it. The command then adds
   your phone, and ends with:

   ```text
   Contact method saved. A test message for the contact method is queued. Inspect the last test result to check delivery.

   THIS PHONE HAS NOT PROVED IT WAKES YOU YET. RUN: sudo mmhotline incidents send-test-sos --sign-in-email-address barbara@example.com
   ```

   Your phone shows a quiet Pushover notification titled
   **MMHotline can reach this phone**. There is nothing to acknowledge.
   `sudo mmhotline contact-methods list` then says
   `Last test message: accepted`, which means Pushover took the message.
   It keeps saying `Emergency test: not yet acknowledged.` until step 8
   proves that the phone wakes you.

   If a step fails, the command says what to do. Running it again is safe:
   it replaces the token, and keeps a phone that is already added.

   Before you rely on it at night, follow
   [Make an emergency alert wake you](how-to/connect-pushover.md)
   on the phone, so the alert sounds in silent mode too.

7. **Put MMHotline behind Cloudflare Access.** Follow
   [Put MMHotline behind Cloudflare Access](how-to/put-mmhotline-behind-cloudflare-access.md),
   steps 1 to 6, then come back here. Step 8 works without it from
   the server, but the links in notifications open only once it is done.
   In short: install `cloudflared` on
   the server, create a tunnel that sends `ops.example.com` to
   `http://127.0.0.1:8480`, create an Access application that lets your
   company's people sign in, and add the two values Cloudflare gives you
   to the settings file.

   **Check:** you opened `https://ops.example.com` on your phone, signed
   in at Cloudflare, and MMHotline's dashboard opened.

8. **Send a test SOS.** This is the one test that proves your phone wakes
   you. MMHotline sends itself an SOS, wakes you (with
   `--sign-in-email-address`, you are woken whether or not you are on
   call), and waits for you to acknowledge it on your phone.

   **IF YOUR CODING AGENT RUNS THIS COMMAND, IT TELLS YOU FIRST: "YOUR
   PHONE IS ABOUT TO SOUND. ACKNOWLEDGE IT."** Its chat waits until
   you do, so you can't ask it then.

   ```sh
   sudo mmhotline incidents send-test-sos --sign-in-email-address barbara@example.com
   ```

   **Check:** your phone sounds with a Pushover notification titled
   **Test of the alerts. Nothing is wrong: acknowledge this.** Acknowledge
   it. The command then prints who acknowledged it, how, and when, and
   exits 0:

   ```text
   MMHotline sent a test SOS only to Barbara Example (barbara@example.com), whether or not they are on call. Waiting up to 10 minutes for someone to acknowledge it on the phone.
   Incident: https://ops.example.com/incidents/acc896d49ce5b2ef40cd2c944da4f08f
   Barbara Example claimed the test by acknowledging the alert in Pushover, at 2026-11-15T04:41:20Z.
   MMHotline resolved the test after the claim, at 2026-11-15T04:41:20Z.
   The test SOS worked: a person acknowledged it on the phone.
   ```

   `sudo mmhotline contact-methods list` now says
   `Emergency test: acknowledged at 2026-11-15T04:41:20Z.` for your phone.
   MMHotline closes the test itself, so there is nothing more to claim or
   resolve. `sudo mmhotline incidents list` prints
   `Listed 0 SOS incidents not resolved, newest first.`

   IF YOU ACKNOWLEDGE BUT THE CONTACT METHOD HOLDS A PUSHOVER GROUP KEY, THE
   COMMAND EXITS 1 AT ONCE AND SAYS TO ADD THE CONTACT METHOD AGAIN WITH YOUR
   OWN USER KEY.

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
  and `sudo mmhotline alerts list`, each with `--json`. To claim or resolve
  with only a terminal, use `sudo mmhotline incidents claim` and
  `sudo mmhotline incidents resolve`. The tool that sent the alert can also
  resolve it.
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

  Do step 2 first. The install made no change.
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
- **The team has no Cloudflare:** keep `web_url=http://localhost:8480`
  in step 2's block, skip step 7, and open MMHotline with a sign-in link, as
  [Sign in during a Cloudflare outage](how-to/sign-in-during-a-cloudflare-outage.md)
  says. The links in notifications then open only on a computer that runs
  that SSH forward. To claim or resolve an incident with no browser at all,
  run `sudo mmhotline incidents claim` and `sudo mmhotline incidents resolve`
  on the server, as
  [Handle an incident](how-to/handle-an-incident.md#claim-and-resolve-from-the-servers-terminal)
  says.
- **A page says "Not a team member" or "Sign in through Cloudflare
  Access":** see
  [Put MMHotline behind Cloudflare Access](how-to/put-mmhotline-behind-cloudflare-access.md#if-something-is-different).
