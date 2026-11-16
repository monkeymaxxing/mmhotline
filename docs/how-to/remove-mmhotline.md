# Remove MMHotline

**Who acts:** someone with `sudo` on the MMHotline server, or that
person's coding agent.

**When to use this page:** to stop MMHotline for a while, or to remove it
from the server for good.

**What this page does:** you stop the service, or remove its services,
program, Unix users, and files. [Installed files](../reference/installed-files.md)
lists each one.

**Terms:** a **unit** is a systemd service or socket that
`mmhotline install` set up.

## Choose the route

- **Stop it for a while,** such as for server work:
  [Stop and start](#stop-and-start).
- **Remove it for good:** [Remove everything](#remove-everything). First
  set up on-call alerting elsewhere: once MMHotline stops, nobody is woken.

## Stop and start

```sh
sudo systemctl stop mmhotline
```

While it is stopped, MMHotline takes no alerts and sends no
notifications. If the MMHotline-down alarm is set up, it wakes the person
on call after its delay, as for any stop. Start it again with:

```sh
sudo systemctl start mmhotline
```

**Check:** `sudo systemctl status mmhotline --no-pager` says
`active (running)`, `sudo journalctl -u mmhotline --no-pager -n 30` shows
the start, and `sudo mmhotline people list` lists the team.

## Remove everything

1. **Turn off the MMHotline-down alarm,** if set up: remove the
   `mmhotline_down_alarm` section from the settings file and run
   `sudo mmhotline settings apply`. Reason: MMHotline first tells the
   alarm to hold no notifications; otherwise it wakes the person on call
   after its delay once MMHotline stops. If the apply says the
   `mmhotline_down_alarm` part did not apply, the alarm could not be told:
   apply again, or delete the Worker in the Cloudflare dashboard and apply
   again. See
   [the MMHotline-down alarm](set-up-the-mmhotline-down-alarm.md).

2. **Keep what you may need,** such as the settings file and a copy of the
   database, made while the service is stopped:

   ```sh
   sudo systemctl stop mmhotline
   sudo cp /etc/mmhotline/mmhotline_settings.yml /var/lib/mmhotline/mmhotline.db ~/
   ```

   The database holds the sending services' secrets: keep the copy
   private, or delete it.

3. **Stop and disable the services:**

   ```sh
   sudo systemctl disable --now mmhotline.service mmhotline-triage-agent-run.socket mmhotline-review-agent-run.socket mmhotline-fleet-view.socket
   ```

   With backup set up, add `mmhotline-backup.service`.

4. **Remove the units and the program:**

   ```sh
   sudo rm /etc/systemd/system/mmhotline.service \
     /etc/systemd/system/mmhotline-triage-agent-run.socket \
     /etc/systemd/system/mmhotline-triage-agent-run@.service \
     /etc/systemd/system/mmhotline-review-agent-run.socket \
     /etc/systemd/system/mmhotline-review-agent-run@.service \
     /etc/systemd/system/mmhotline-fleet-view.socket \
     /etc/systemd/system/mmhotline-fleet-view@.service \
     /usr/local/bin/mmhotline
   sudo systemctl daemon-reload
   ```

   With backup set up, also remove
   `/etc/systemd/system/mmhotline-backup.service`.

5. **Remove the Unix users:**

   With backup set up, first `sudo userdel mmhotline-backup`. Then:

   ```sh
   sudo userdel -r mmhotline-triage-agent
   sudo userdel mmhotline-review-agent
   sudo userdel mmhotline
   ```

   `-r` also removes the triage agent's home,
   `/var/lib/mmhotline-triage-agent`. It may print
   `userdel: mmhotline-triage-agent mail spool (/var/mail/mmhotline-triage-agent) not found`,
   which is harmless: the user never had mail. If `getent group mmhotline` still
   prints a line, such as when a deploy tool made the group, run
   `sudo groupdel mmhotline`.

6. **Remove the data and settings:**

   ```sh
   sudo rm -r /var/lib/mmhotline /etc/mmhotline
   ```

   `/etc/mmhotline` also holds `backup.env`, with the backup bucket's
   keys, when backup is set up. With backup set up, also remove
   `/var/lib/mmhotline-backup`. The backup in
   object storage stays; delete it there if you no longer need it.

**Check:** `systemctl list-unit-files 'mmhotline*'` lists nothing, and
`id mmhotline` says there is no such user. Only systemd's runtime files
under `/run` remain, and a restart removes them.

## Also undo, outside the server

- The Cloudflare Tunnel and the Access application, if MMHotline was
  behind Cloudflare Access.
- The MMHotline-down alarm's Cloudflare Worker, in the Cloudflare
  dashboard, and the status page, if set up.
- The tools that send alerts and check-ins: stop them, or point them at
  the new alerting service.
- The sending services' accounts and apps, such as the Pushover
  application, the Twilio API key, the Slack app, and the Telegram bot.
