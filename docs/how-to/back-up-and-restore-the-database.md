# Back up and restore the database

**Who acts:** someone with `sudo` on the MMHotline server, or that
person's coding agent, with access to the team's object storage.

**When to use this page:** to keep a copy of MMHotline's database off the
server, to check that the copy keeps up, or to restore it.

**What this page does:** you set up Litestream to copy each change to an
S3-compatible bucket within about a second, check it, and restore from it.

**Terms:**

- The **database** is `/var/lib/mmhotline/mmhotline.db`: people,
  incidents, contact methods, and the sending services' setup and
  secrets.
- **Litestream** is a separate program that copies each SQLite change to
  object storage as it happens. MMHotline runs it as its own service,
  `mmhotline-backup`, under its own Unix user.
- A **bucket** is a storage area at an S3-compatible object storage
  service.

Backup is optional. Without it, a lost server loses MMHotline's history;
the team can still recreate the setup from the settings file.

## Choose the route

- **Set up the backup:** [Set up](#set-up).
- **Check that it keeps up, such as from a monitoring tool:**
  [Check the backup](#check-the-backup).
- **Get the database back on the same server:** [Restore](#restore).
- **Rebuild on a new server from the backup:**
  [Restore on a new server](#restore-on-a-new-server).

## Before you start

- A bucket at an S3-compatible object storage service, with **Object Lock
  off**, and a key that may list, read, write, and delete in it. Reason:
  Litestream 0.5.18's uploads are refused by a bucket with Object Lock, and
  Litestream deletes old files as it compacts the backup. A lock would
  only guard against someone with root on the server, who already holds
  the bucket's key.
- The bucket's S3 endpoint and region, from the storage service.

## Set up

1. **Install Litestream 0.5.18** on the server. For an `x86_64` server:

   ```sh
   curl --fail --location --output /tmp/litestream-0.5.18-linux-x86_64.tar.gz \
     https://github.com/benbjohnson/litestream/releases/download/v0.5.18/litestream-0.5.18-linux-x86_64.tar.gz
   echo "d3687b9fd0f2e9426bbd3da828429ff3f1dfc5c9b74d54fdf028e4ee510e7dea  /tmp/litestream-0.5.18-linux-x86_64.tar.gz" | sha256sum -c -
   tar -xzf /tmp/litestream-0.5.18-linux-x86_64.tar.gz -C /tmp litestream
   sudo install -o root -g root -m 0755 /tmp/litestream /usr/local/bin/litestream
   litestream version
   ```

   **Check:** `sha256sum` prints
   `/tmp/litestream-0.5.18-linux-x86_64.tar.gz: OK`, and
   `litestream version` prints `0.5.18`. If the check fails, the download
   is not the release: do not install it. For another processor, use its
   archive and the digest from the release's `checksums.txt`.

2. **Write the backup settings,** apart from the settings file:

   ```sh
   sudo install -o root -g root -m 0600 /dev/null /etc/mmhotline/backup.env
   sudoedit /etc/mmhotline/backup.env
   ```

   with your bucket, endpoint, region, and key:

   ```ini
   MMHOTLINE_BACKUP_REPLICA_URL="s3://BUCKET/mmhotline?endpoint=https://YOUR-S3-ENDPOINT&region=YOUR-REGION"
   LITESTREAM_ACCESS_KEY_ID="YOUR_ACCESS_KEY_ID"
   LITESTREAM_SECRET_ACCESS_KEY="YOUR_SECRET_ACCESS_KEY"
   ```

   - `MMHOTLINE_BACKUP_REPLICA_URL`: Litestream's replica URL, the bucket
     and a folder in it, here `mmhotline`, with the endpoint and region.
   - Write `KEY=value` lines without `export`, and keep the file owned by
     root with mode 0600. Reason: it holds the bucket's key, which only
     the backup service reads; MMHotline's own service never gets it.

   Other storage services can use their own Litestream URL and key
   variables, as [Litestream's S3 guide](https://litestream.io/guides/s3/)
   says.

3. **Run install** to start the backup. Install sets up the backup only
   when both `backup.env` and Litestream exist; with `backup.env` but no
   Litestream it fails and says to install Litestream.

   ```sh
   sudo mmhotline install
   ```

   **Check:** among its lines are:

   ```text
   Unix user: created mmhotline-backup with no login shell
   Service: mmhotline-backup unit updated; enabled; restarted
   Backup: Litestream service configured; check that uploads keep up with sudo mmhotline backup check-uploads
   ```

   Then `sudo journalctl -u mmhotline-backup --no-pager -n 30` shows
   `replicating to` with your bucket, and soon `snapshot complete`.

4. **Check that it keeps up:**

   ```sh
   sudo mmhotline backup check-uploads
   ```

   **Check:** it exits 0 and says:

   ```text
   MMHotline's backup holds every change Litestream captured, up to transaction 0000000000000002.
   ```

5. **Keep a copy of the two settings files elsewhere,**
   `/etc/mmhotline/mmhotline_settings.yml` and
   `/etc/mmhotline/backup.env`, such as in the team's vault. Reason: the
   backup holds only the database, and the database holds secrets, so
   treat the bucket as secret too.

After you change `backup.env`, run `sudo systemctl restart mmhotline-backup`.

## Check the backup

A running backup service proves nothing by itself: when uploads fail,
such as after a revoked key, Litestream logs the error and keeps running.
`sudo mmhotline backup check-uploads` checks the backup itself. It
changes nothing, and waits up to 30 seconds for Litestream to catch up.

| Exit code | Meaning | What to do |
| --- | --- | --- |
| `0` | The backup holds every change, or the rest was captured less than 5 minutes ago | Nothing |
| `1` | The backup has stopped: a change was not captured within 30 seconds, or a change captured more than 5 minutes ago is not in the bucket | Read `sudo journalctl -u mmhotline-backup` |
| `3` | It can't tell: backup not set up, Litestream missing, nothing captured yet, or the bucket can't be listed | Read the line, which names the reason and never a key |
| `4` | The command's arguments are wrong | Run `mmhotline backup check-uploads --help` |

For example, with the backup service stopped and a change made:

```text
MMHotline's backup has stopped: the database has changes that Litestream has not captured after waiting 30 seconds; Litestream has captured up to transaction 0000000000000004. Read why with: sudo journalctl -u mmhotline-backup
```

Have a monitoring tool run the check as root every 10 minutes and alert on
exit 1 or 3. Reason: the age of the newest upload proves nothing, since an
idle MMHotline writes nothing for hours. Each run lists the bucket once.
`--json` prints the result as one JSON object, whose fields the
[command reference](../reference/mmhotline-command.md#mmhotline-backup-check-uploads) lists. A
stopped backup service is caught once MMHotline writes a change.

## Restore

A restore brings back the changes that reached the bucket. Changes after
the last upload are lost, and notifications sent after it may be sent
again; at start, MMHotline continues the pending work the restored
database holds. Try it once on a test server with your own bucket before
you depend on it. If the server's disk is failing, restore on a new
server instead, as [Restore on a new server](#restore-on-a-new-server)
says.

1. **Note what to expect back,** if MMHotline still runs:
   `sudo mmhotline incidents list` and `sudo mmhotline alerts list`.

2. **Run this whole block** on the server. It stops MMHotline and the
   backup, keeps the old state in a new folder, restores into a fresh
   folder, and starts both again:

   ```sh
   sudo bash <<'SH'
   set -eu
   set -a
   . /etc/mmhotline/backup.env
   set +a
   systemctl stop mmhotline mmhotline-backup
   saved_state_dir=$(mktemp -d /var/lib/mmhotline-before-restore.XXXXXXXX)
   printf 'Old state will be kept at %s/state\n' "$saved_state_dir"
   mv /var/lib/mmhotline "$saved_state_dir/state"
   if [ -e /var/lib/mmhotline-backup/mmhotline.db-litestream ]; then
     mv /var/lib/mmhotline-backup/mmhotline.db-litestream "$saved_state_dir/litestream"
   fi
   install -d -o mmhotline -g mmhotline -m 0770 /var/lib/mmhotline
   litestream restore -o /var/lib/mmhotline/mmhotline.db "$MMHOTLINE_BACKUP_REPLICA_URL"
   chown -R mmhotline:mmhotline /var/lib/mmhotline
   chmod 0660 /var/lib/mmhotline/mmhotline.db
   systemctl start mmhotline mmhotline-backup
   SH
   ```

   **Check:** it prints the folder that keeps the old state, such as
   `Old state will be kept at /var/lib/mmhotline-before-restore.XXXXXXXX/state`
   with eight random characters, and exits 0. The block also sets aside
   Litestream's old working files, because they describe the old database
   and may hold changes that never reached the bucket.

3. **Check what came back:**

   ```sh
   sudo mmhotline incidents list
   sudo mmhotline alerts list
   sudo journalctl -u mmhotline -u mmhotline-backup --no-pager -n 30
   ```

   **Check:** the incidents and alerts from step 1 are there, and the
   journal shows the backup `replicating to` your bucket, with no error.

4. **Check the backup again:** `sudo mmhotline backup check-uploads`
   exits 0. The backup continues from the bucket's newest change, so the
   transaction number it names is one higher than before.

Once the restore is checked, the saved folder can go:
`sudo rm -r /var/lib/mmhotline-before-restore.XXXXXXXX`, with your
folder's name. Each restore leaves one such folder.

## Restore on a new server

1. **Install MMHotline on the new server,** as
   [Install MMHotline](install.md) says, with the old server's settings
   file, Litestream as in [Set up](#set-up) step 1, and the old
   `backup.env` in place. Then `sudo mmhotline install`.

   **Check:** install fails on purpose, leaves the backup stopped, and
   says:

   ```text
   mmhotline: Backup: not started: this server's database is new, and the bucket in MMHOTLINE_BACKUP_REPLICA_URL already holds a backup, up to transaction 0000000000000001. Starting the backup now would save the empty database as the newest backup. To bring the backup back, restore it as https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/back-up-and-restore-the-database.md#restore says; the restore starts the backup. To start empty instead, set MMHOTLINE_BACKUP_REPLICA_URL in /etc/mmhotline/backup.env to a folder that holds no backup, such as s3://BUCKET/mmhotline-new, and run sudo mmhotline install again
   ```

   Reason: on a new server, Litestream would otherwise save the new,
   empty database as the bucket's newest state, and the restore would
   bring back nothing.

   Running install again changes nothing until you restore or choose
   another folder: the new database stays marked as new, by the file
   `/var/lib/mmhotline/new-database-waits-for-the-backup-bucket-check`,
   until the bucket check passes. Reason: otherwise a second install would
   back up the empty database after all.

2. **Run the restore block** from [Restore](#restore), step 2.

3. **Run install again:** `sudo mmhotline install`.

   **Check:** it exits 0 with `Database: mmhotline.db already exists; kept`
   and `Backup: Litestream service configured`, the alerts and people are
   back, and `sudo mmhotline backup check-uploads` exits 0.

A deploy tool that restores `mmhotline.db` before it runs install gets no
refusal: install did not create that database, so it backs it up at once.

**To start empty instead,** in a bucket that holds an old backup: set
`MMHOTLINE_BACKUP_REPLICA_URL` to a folder that holds no backup, such as
`s3://BUCKET/mmhotline-new`, and run `sudo mmhotline install`. The old
backup stays in its folder. If the bucket can't be listed, install
refuses too, and says why; fix `backup.env` and run install again, which
checks the bucket again.

## If something is different

- **The restore block stops with an error:** leave `mmhotline` and
  `mmhotline-backup` stopped, fix the error, and run the block again. The
  old state stays in the saved folder.
- **The journal says**
  `Content-MD5 OR x-amz-checksum- HTTP header is required`: the bucket
  has Object Lock on. Use a bucket without it.
- **`check-uploads` exits 3 and says the backup can't be listed:** check
  the key and the replica URL in `backup.env`, then
  `sudo systemctl restart mmhotline-backup`.
- **A manual copy instead of Litestream:** stop `mmhotline` and
  `mmhotline-backup` cleanly first, then copy `mmhotline.db`. Never throw
  away a `mmhotline.db-wal` file left by an abrupt stop; start and stop
  MMHotline cleanly before copying.
