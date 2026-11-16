# Run MMHotline locally for development

This guide is for people changing MMHotline's code. For an installation your team
will use, follow [Install MMHotline](../how-to/install.md).
The [testing guide](testing.md) describes automated checks.

The commands below run on your development computer, macOS or Linux, with the
browser on the same computer. MMHotline serves pages on your development
computer at `127.0.0.1:8480`, and you sign in with a one-time sign-in link
instead of Cloudflare Access. A team installation also uses port 8480, so use
a computer without a team installation.

A sending service is the mail server or external service MMHotline uses to send
notifications. A contact method is the address or account that receives
notifications. The contact method type says how delivery works, such as email or a Pushover
notification.
An SOS is the urgent alert that wakes the person on call, even at night.
Claiming an incident tells the team you are handling it, and MMHotline stops
contacting the next person.

## Build and start

Everything in this guide runs on macOS and on Linux. The custom schedule
sandbox runs the team's schedule code in a separate, restricted process.
On macOS, the sandbox runs without seccomp and without the memory limit.
Seccomp restricts which operating-system calls the process can make.
Full sandbox protection needs Linux.

On Linux, open a root shell with `sudo -s` first. Use a root shell in each
terminal for MMHotline or fake sending service commands.

Use Go 1.27.4, selected by `go.mod`. No C compiler is needed.
From the repository root on your development computer, build the application:

```sh
export TMPDIR="${TMPDIR:-/tmp}"
export GOCACHE="$TMPDIR/go-build"
CGO_ENABLED=0 go build -o "$TMPDIR/mmhotline" ./cmd/mmhotline
```

The binary is written to `$TMPDIR/mmhotline`. To use fake sending services,
follow
[Develop without real sending services](#develop-without-real-sending-services)
before starting `serve` below.

For development without a systemd installation, give `serve` a state
directory, a settings file, and a command socket yourself. The command socket
is a local Unix socket through which management commands control `serve`.

On macOS, the command socket accepts the user who runs `serve`, so management
commands run without `sudo`. On Linux, the command socket accepts only root,
as in a team installation. Use the root shell opened above for management
commands.
On Linux, `$TMPDIR/mmhotline-dev-settings.yml` must be owned by root.

First write a settings file that only you can read.
Use the terminal where you ran the build commands:

```sh
install -m 0600 /dev/null "$TMPDIR/mmhotline-dev-settings.yml"
cat > "$TMPDIR/mmhotline-dev-settings.yml" <<'YAML'
# Development settings for the local MMHotline.
team_name: Operations
web_url: http://localhost:8480
web_listen_address: 127.0.0.1:8480
alert_intake_listen_address: 127.0.0.1:8484
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
alert_sources: []
YAML
```

The [settings file reference](../reference/settings-file.md) lists every key.
`$TMPDIR/mmhotline-dev-settings.yml` must be owned by root or by the user who
runs `serve`. No other user may read or change
`$TMPDIR/mmhotline-dev-settings.yml`. `install -m 0600` sets the required
permissions. The settings file controls MMHotline and can contain secrets.
Then create the state and start `serve`. Use your own user on macOS and the
root shell on Linux:

```sh
"$TMPDIR/mmhotline" setup --state-dir "$TMPDIR/mmhotline-dev-state" \
  --settings-file "$TMPDIR/mmhotline-dev-settings.yml"
"$TMPDIR/mmhotline" serve \
  --state-dir "$TMPDIR/mmhotline-dev-state" \
  --settings-file "$TMPDIR/mmhotline-dev-settings.yml" \
  --command-socket "$TMPDIR/mmhotline-dev-command.sock"
```

Setup refuses existing state, and creates the team with
`$TMPDIR/mmhotline-dev-settings.yml`'s `team_name`.
Development state is disposable: to start over, stop `serve`, move
`$TMPDIR/mmhotline-dev-state` away, and run setup again. A newer build
upgrades a development database as it upgrades a team's
([database format and upgrades](design/operation.md#database-format-and-upgrades)).

`serve` requires `--settings-file` and `--command-socket`.
At start, `serve` applies
`$TMPDIR/mmhotline-dev-settings.yml`, which adds Barbara and her schedule.
Then `serve` serves pages on `127.0.0.1:8480` and receives signed alerts from
tools on `127.0.0.1:8484`. Without `cloudflare_access`, `serve` prints
`Sign-in: sign-in links only; the settings file has no cloudflare_access`.

The web URL uses `localhost`, the host name in sign-in links, so links in
notifications open in the browser you sign into MMHotline with.
Socket creation uses mode 0600 even when replacing a stale socket.
A regular file at a socket path is refused.

To change a setting, edit `$TMPDIR/mmhotline-dev-settings.yml`.
In the second terminal, set `TMPDIR` to the value used in the build terminal.
Then apply `$TMPDIR/mmhotline-dev-settings.yml`:
`"$TMPDIR/mmhotline" settings apply --command-socket "$TMPDIR/mmhotline-dev-command.sock"`.
A change of `web_url`, either listen address, or `cloudflare_access` needs a
restart; outside systemd, `settings apply` cannot restart `serve`, so stop it
with Ctrl-C and start it again instead.

In another terminal on your development computer, create a sign-in link for
Barbara:

```sh
export TMPDIR="${TMPDIR:-/tmp}"
"$TMPDIR/mmhotline" people create-sign-in-link --command-socket "$TMPDIR/mmhotline-dev-command.sock" \
  --sign-in-email-address barbara@example.com
```

Every management command needs
`--command-socket "$TMPDIR/mmhotline-dev-command.sock"`, because the default
socket path is the team installation's `/run/mmhotline/command.sock`. See the
[command reference](../reference/mmhotline-command.md).

In your browser, open the printed `Sign-in link` within 10 minutes.
The sign-in link works once, signs the browser in as Barbara for 12 hours,
and opens the dashboard. The browser runs on your development computer, so
skip the printed SSH forward command.
For another sign-in, create a new link. [Sign in during a Cloudflare outage](../how-to/sign-in-during-a-cloudflare-outage.md)
says how, and the [documentation index](../README.md) lists the guides for
each page.

If `serve` runs on a remote Linux development server instead, run the printed
SSH forward command on your computer, with `YOUR_SSH_HOST` replaced by that
server, then open the link in your computer's browser.

## Develop without real sending services

`mmhotline-fake-sending-services` imitates chat, push, and phone services and a webhook
receiver. Email continues to use its saved SMTP setup; use Mailpit to keep
development email local. Start the fake sending services, then point MMHotline at them.
Set up each sending service in the development settings file, even with the
fake services. Run the commands from the repository root on your development computer,
before starting `serve` in the terminal used for the fake sending services:

```sh
export TMPDIR="${TMPDIR:-/tmp}"
export GOCACHE="$TMPDIR/go-build"
CGO_ENABLED=0 go build -o "$TMPDIR/mmhotline-fake-sending-services" ./cmd/mmhotline-fake-sending-services
"$TMPDIR/mmhotline-fake-sending-services" --listen-address 127.0.0.1:8090 &
export MMHOTLINE_FAKE_SENDING_SERVICES_URL='http://127.0.0.1:8090'
```

`mmhotline serve` then sends Telegram, Slack, Pushover, and Twilio requests to
the fake services. MMHotline's web interface shows a development banner with
a link to `http://127.0.0.1:8090/`. The fake sending services page lists what
MMHotline would have sent. Messages listed on the fake sending services page
reach no real person. Open the fake sending services page in your browser to
inspect fake delivery and press fake buttons.

Each service below is set up the same way: add its section under
`sending_services` in `$TMPDIR/mmhotline-dev-settings.yml`, in place of
`sending_services: {}`, then apply the file from another terminal:

```sh
"$TMPDIR/mmhotline" settings apply --command-socket "$TMPDIR/mmhotline-dev-command.sock"
```

`settings apply` checks each new service against the fake, or against
Mailpit for email, and prints the next step under each sending service's part.
The sections below can all sit under one `sending_services` key.

If `serve` runs on a remote development server, forward port 8090 as well.
In a terminal on your computer, run the SSH command below instead of the
printed SSH forward command. The SSH command below forwards port 8480 for
MMHotline's pages and port 8090 for the fake sending services page:

```sh
ssh -N -L 8480:127.0.0.1:8480 -L 8090:127.0.0.1:8090 barbara@devbox.example.com
```

Replace `barbara` with your Linux SSH user and `devbox.example.com` with your
development server's host name.
Leave the terminal running the SSH command open.

### Email with Mailpit

Email uses the saved SMTP setup even in development mode. Run Mailpit on the
development computer, with SMTP at `127.0.0.1:1025` and Mailpit's web interface at
`127.0.0.1:8025`. Set up the mail server in `$TMPDIR/mmhotline-dev-settings.yml`
and apply `$TMPDIR/mmhotline-dev-settings.yml`:

```yaml
sending_services:
  mail_server:
    smtp_server: 127.0.0.1:1025
    smtp_connection_security: unencrypted
    email_from_address: alerts@example.com
```

The Mailpit configuration above needs no SMTP credentials.
Add an email contact method in the MMHotline browser interface, then open
Mailpit's web interface to inspect the email contact method's test message.
If `serve` runs on a remote development server, add
`-L 8025:127.0.0.1:8025` to the SSH forwarding command.
The [mail server guide](../how-to/connect-a-mail-server.md) explains contact methods.

### Telegram

Set up Telegram in `$TMPDIR/mmhotline-dev-settings.yml` with a token the fake
accepts, then apply `$TMPDIR/mmhotline-dev-settings.yml`:

```yaml
sending_services:
  telegram:
    bot_token: "7000000001:fake_bot_token_abcdefghij"
```

The setup check and messages then use fake Telegram. No Telegram setup is
created automatically.

On Contact methods, choose “Connect Telegram”. The link opens the fake's
stand-in for the Telegram app. Press Start to connect a pretend Telegram user.
The fake sending services page lists each message with its buttons.
Press buttons as the pretend Telegram user connected with Start, or as a
stranger, to check authorization.

### Pushover

Set up Pushover in `$TMPDIR/mmhotline-dev-settings.yml` with the fake's
application token, then apply `$TMPDIR/mmhotline-dev-settings.yml`:

```yaml
sending_services:
  pushover:
    application_token: FakePushoverApplicationToken01
```

On Contact methods, add personal user key
`FakePushoverPersonalUserKey001`. The fake also accepts
`FakePushoverPersonalUserKey002` and `FakePushoverPersonalUserKey003`.
The fake sending services page shows emergency messages and their Acknowledge
buttons.

A Pushover receipt tracks an emergency message's acknowledgement
and expiry. MMHotline repeatedly requests the fake receipt's status.
Acknowledging an SOS claims the incidents counted in the acknowledged
Pushover message, as with real Pushover. Acknowledging a test claims no incident.

### Slack

Set up Slack in `$TMPDIR/mmhotline-dev-settings.yml` with the fake's tokens,
then apply `$TMPDIR/mmhotline-dev-settings.yml`:

```yaml
sending_services:
  slack:
    bot_token: xoxb-fake-slack-bot-token
    app_level_token: xapp-fake-slack-app-level-token
```

In the MMHotline browser interface, create a connection code under Contact
methods.
On the fake sending services page, send the connection code as a chosen
Slack user. Use conversation `D0001` for a direct message, or `C0001` and
“Channel mention (use a C conversation ID)” for a shared Slack channel.
Messages appear with buttons you can press as any Slack user.
Socket Mode is Slack's WebSocket connection for incoming messages and
button presses. The fake sends
messages and button presses through MMHotline's outbound Socket Mode connection.

### Automated voice calls

Set up Twilio in `$TMPDIR/mmhotline-dev-settings.yml` with the fake's values,
then apply `$TMPDIR/mmhotline-dev-settings.yml`:

```yaml
sending_services:
  twilio:
    account_sid: AC00000000000000000000000000000001
    api_key_sid: SK00000000000000000000000000000001
    api_key_secret: fake-twilio-api-key-secret
    phone_number_to_call_from: "+12025550102"
    extra_phone_numbers_on_the_contact_card: []
```

The fake also accepts `+12025550103` as the calling number. Setup and
call-result requests go to the fake address. Add a phone contact
method in MMHotline. The fake sending services page lists the calls and buttons
for setting each call's outcome. The Answered, Busy, No answer, and Call failed
buttons change the status in the fake service's record of the call.
MMHotline repeatedly requests the call record to read the call's status.
The fake sends no request to MMHotline.
Other buttons make the fake refuse or lose its next request.

### Outgoing webhooks

In the MMHotline browser interface, add an outgoing webhook contact method
pointing at `http://127.0.0.1:8090/webhook-receiver/example`.
Only fake webhook receiver addresses are allowed as private webhook destinations
in development mode. The fake sending services page shows the requests.

## Check and stop

From the repository root on your development computer, run the
[focused tests](testing.md#test-in-levels). For the whole application suite when
the testing level calls for it:

```sh
CGO_ENABLED=0 go test -timeout 30m ./...
```

Stop the foreground `serve` process with Ctrl-C in its terminal. Stop the fake
sending services with `kill` and the PID printed when its background job starts.
Development state and test secrets are disposable.
Keep development state and test secrets separate from a team's installation.

For writing schedules, see
[custom schedule code](design/scheduling.md#custom-schedule-code), and preview a schedule with `schedules preview --settings-file`.
The triage agent is the team's coding agent, which MMHotline runs to answer
alerts first. `mmhotline install` and the triage agent need a Linux server
with systemd.
