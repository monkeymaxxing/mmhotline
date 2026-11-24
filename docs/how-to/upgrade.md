# Upgrade MMHotline

**Who acts:** someone with `sudo` on the MMHotline server, or that
person's coding agent.

**When to use this page:** a newer MMHotline release is out, and the
server runs an older one.

**What this page does:** you install the new program over the old one.
The settings file, the database, and every incident stay.

**Terms:** the **database** is `/var/lib/mmhotline/mmhotline.db`, where
MMHotline keeps people, incidents, and setup.

## Choose the route

- **A release, by hand:** follow [Steps](#steps).
- **A deploy tool installs MMHotline,** such as MMX: it runs the same
  `mmhotline install` from the new release; the checks below still apply.
- **A commit with no release:** build it as
  [Install MMHotline](install.md#build-from-source) says, then run
  `sudo "$TMPDIR/mmhotline" install` in place of step 3. Its version says
  `modified=true` when the build had changes not yet committed.

## Before you start

- **Back up first if you might go back.** A new release upgrades the
  database's format when it starts, and an older release refuses a
  database a newer one upgraded. Going back needs a backup made before
  the upgrade, as
  [database format and upgrades](../changing-mmhotline/design/operation.md#database-format-and-upgrades)
  says.
- **Alerts during the restart are not taken.** The service restarts
  within a few seconds. A tool that sends an alert again with the same
  `sender_alert_id` gets it counted once, as
  [Connect a tool that sends alerts](connect-a-tool-that-sends-alerts.md)
  says, so let the team's tools retry, or upgrade at a quiet time.

## Steps

1. **Check the version now:**

   ```sh
   mmhotline version
   ```

   **Check:** it prints the running release, such as
   `mmhotline 0.7.0 (COMMIT, linux/amd64, modified=false)`, with the
   release's full commit hash in place of `COMMIT`. The
   release you upgrade to is the version in the archive's name, such as
   `mmhotline-0.7.0-linux-amd64.tar.gz`.

2. **Download, check, and unpack the new release,** as steps 1 and 2 of
   [Install a release](install.md#install-a-release) say: paste the block
   with the new version number in `version=`, then `cd` into the new
   unpacked folder.

3. **Install it,** in the unpacked folder:

   ```sh
   sudo ./mmhotline install
   ```

   **Check:** it exits with status 0, and among its lines are:

   ```text
   Binary: installed or corrected /usr/local/bin/mmhotline
   Database: mmhotline.db already exists; kept
   Service: mmhotline unit updated; already enabled; restarted
   ```

   Install replaces `/usr/local/bin/mmhotline` in one step, updates the
   service unit, and restarts the service. It also creates any new Unix
   user the release needs, and corrects the owner and mode of MMHotline's
   folders and files.

4. **Check the new version runs:**

   ```sh
   mmhotline version
   systemctl is-active mmhotline
   ```

   **Check:** the new version, with `modified=false`, and `active`.
   Opening MMHotline shows the dashboard as before.

5. **If backup is set up,** check that it keeps up after the restart:

   ```sh
   sudo mmhotline backup check-uploads
   ```

   **Check:** it exits 0. Install also rewrites
   `/etc/mmhotline/litestream.yml` and restarts the backup when their
   setup changed.

A second `sudo mmhotline install` changes nothing: its lines say
`already exists`, `already correct`, or `kept`, such as
`Binary: already correct (/usr/local/bin/mmhotline)` and
`Service: mmhotline unit already correct; already enabled; already running`.

## Tighten the triage user on each server

Do this once if the team uses the triage agent and set up the `triage`
user on its servers before the key line started with `restrict`.

**THE UPGRADE DOES NOT CHANGE THE SERVERS. DO THIS ON EVERY SERVER OF
EVERY INVENTORY, OR IN YOUR DEPLOY TOOL'S TEMPLATE.**

1. **On each server, add `restrict` and give the files to root:**

   ```sh
   sudo sed -i 's/^from=/restrict,from=/' /home/triage/.ssh/authorized_keys
   sudo chown root:root /home/triage /home/triage/.ssh /home/triage/.ssh/authorized_keys
   sudo chmod 0755 /home/triage /home/triage/.ssh
   sudo chmod 0644 /home/triage/.ssh/authorized_keys
   sudo grep -c '^restrict,from=' /home/triage/.ssh/authorized_keys
   sudo -u triage test -w /home/triage/.ssh/authorized_keys && echo 'still writable by triage'
   ```

   **Check:** the `grep` prints `1`, and nothing prints
   `still writable by triage`.

2. **On the MMHotline server, check that the agent still logs in:**

   ```sh
   sudo mmhotline triage-agent check-preparation
   ```

   **Check:** line 2 starts with `Present:`. Line 9 starts with `Present:`
   too; if it starts with `Missing:`, it names the server and what is
   still open, and step 1 above fixes it.

3. **Optional: stop `triage` from opening connections out of a server.**
   Add the firewall rule in
   [Set up the triage agent](set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository),
   step 2, under "Optional: stop the `triage` user from opening
   connections out of a server". A command the agent runs on a server then
   cannot send data out or open a connection. It still reads whatever the
   `triage` user can read.

   **DON'T ADD THIS RULE ON THE SERVER WHERE THE TRIAGE AGENT RUNS. IT
   NEEDS TO REACH ITS AI PROVIDER.**

Reason: without `restrict`, the agent could open a tunnel from a server
past MMHotline's logging proxy. While `triage` owns its home, a command the
agent runs could rewrite the key line without `restrict`.
[Set up the triage agent](set-up-the-triage-agent.md#prepare-the-servers-and-the-systems-repository),
step 2, has the details.

## If something is different

- **The program at `/usr/local/bin/mmhotline` was already replaced,** such
  as by a deploy tool: run `sudo mmhotline install`. Install sees that the
  program changed and restarts the service.
- **MMHotline does not start after the upgrade:** read the reason with
  `sudo journalctl -u mmhotline --no-pager -n 30`. A database from a newer
  release is refused there; restore the backup made before the upgrade, as
  [Back up and restore the database](back-up-and-restore-the-database.md#restore) says.
- **An upgrade from a build before the backup had its own user** left
  `/var/lib/mmhotline/.mmhotline.db-litestream`: once
  `sudo mmhotline backup check-uploads` exits 0, delete it with
  `sudo rm -r /var/lib/mmhotline/.mmhotline.db-litestream`. The backup
  keeps its working files elsewhere and continues with no gap.
