# Install MMHotline

**Who acts:** an administrator with `sudo` on the Linux server that will
run MMHotline, or that person's coding agent, or the team's deploy tool.

**When to use this page:** to install MMHotline on a server, by hand or
from a deploy tool, from a release or from source.
[Get started](../get-started.md) does the same install as part of the
first hour, and also sets up Pushover, Cloudflare Access, and a test SOS.

**What this page does:** you check the server, download and check a
release, write the settings file, and install. MMHotline then runs as a
systemd service.

**Terms:** the **settings file**, `/etc/mmhotline/mmhotline_settings.yml`,
holds the team's settings: its people, its schedule, and the tools that
send alerts. The install reads it and never changes its contents.

## Choose the route

- **Install a release by hand:** follow [Install a release](#install-a-release).
- **A deploy tool installs it,** such as [MMX](https://github.com/monkeymaxxing/mmx):
  the tool does the same steps. It writes the settings file from your
  repository, owner `root`, group `mmhotline` once that group exists, mode
  `0640`, and runs `mmhotline install` from the unpacked release.
- **Run a commit that has no release,** such as a change you are making:
  build it first, as [Build from source](#build-from-source) says, then
  follow the steps from step 4.
- **MMHotline is already installed, and you want a newer release:** see
  [Upgrade MMHotline](upgrade.md).

## Before you start

- A Linux server with systemd, such as Debian 12 or 13 or Ubuntu 24.04,
  which MMHotline is tested on. MMHotline needs systemd for its
  service and for the restricted units its agents run in.
- `sudo`, `curl`, and, for the test sender in the other pages, Python 3.
- A private network address on the server, such as a WireGuard address,
  where your tools will send alerts. If every tool that sends alerts runs
  on this server, `127.0.0.1` works too.
- Outbound internet access, for Cloudflare and the sending services.

## Install a release

1. **Download the archive, check it, and unpack it.** Paste this block.
   It picks the archive for the server's processor, downloads it with its
   checksum file, checks it, and unpacks it into `~/mmhotline-0.8.0`. To
   install a newer release, change `version=`.

   ```sh
   (
   set -eu
   version=0.8.0
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

   **Check:** it prints `mmhotline-0.8.0-linux-amd64.tar.gz: OK` (`arm64`
   on an ARM server), then where it unpacked.

   Reason: `SHA256SUMS` holds the checksum of each archive in the release,
   so the check finds a damaged or replaced download before you run it.

   IF IT PRINTS `FAILED`, DELETE THE FOLDER `~/mmhotline-0.8.0` AND DO NOT RUN THE PROGRAM.

   By hand: `uname -sm` prints `Linux x86_64` for the `amd64` archive or
   `Linux aarch64` for the `arm64` archive.

2. **Go into the unpacked folder:**

   ```sh
   cd ~/mmhotline-0.8.0/mmhotline-0.8.0-linux-*/
   ```

   **Check:** `ls` shows `LICENSE`, `dependency_license_notices`, and the
   program `mmhotline`.

   The block cannot move your shell, so this step is yours. Stay in this
   folder for the steps that run `./mmhotline`.

3. **Optional: prove who built the archive.** With the GitHub command line
   tool (`gh`):

   ```sh
   gh attestation verify ~/mmhotline-0.8.0/mmhotline-0.8.0-linux-*.tar.gz --repo monkeymaxxing/mmhotline
   ```

   **Check:** it says the verification succeeded. Reason: the checksum
   proves the download is the release; this also proves GitHub built it
   from this repository.

4. **Write the settings file,** if it does not exist yet. Make the folder
   and an empty file that only root can read and change, and open it. The
   install in step 6 then lets MMHotline read it too:

   ```sh
   sudo install -d -m 0755 /etc/mmhotline
   sudo install -m 0640 /dev/null /etc/mmhotline/mmhotline_settings.yml
   sudoedit /etc/mmhotline/mmhotline_settings.yml
   ```

   Write the first settings: the team, its addresses, you as the first
   administrator, a schedule, and an alert source. Step 3 of
   [Get started](../get-started.md) has a whole first file and says which
   values to change; [the settings file reference](../reference/settings-file.md)
   lists every key.

   Reason for the owner and mode: the file holds the team's secrets, and
   its `fleet_view_command` runs as root, so only root may change it, and,
   after the install, only root and MMHotline may read it.

5. **Check the settings file.**

   RUN THIS IN THE UNPACKED FOLDER FROM STEP 2. IF YOU ARE ELSEWHERE, `./mmhotline` IS NOT FOUND.

   ```sh
   sudo ./mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
   ```

   **Check:** it prints
   `The settings file /etc/mmhotline/mmhotline_settings.yml has no problems.`

6. **Install:**

   ```sh
   sudo ./mmhotline install
   ```

   The install prints one line for each thing it checks or makes. On a new
   server, the output starts and ends like this:

   ```text
   Checks: Linux, root, and systemd confirmed
   Unix user: created mmhotline with no login shell
   Unix user: created mmhotline-triage-agent with no login shell
   Unix user: created mmhotline-review-agent with no login shell
   ...
   Database: created mmhotline.db for Operations
   Settings file: set owner root, group mmhotline, and mode 0640, so MMHotline can read the settings file and only root can change the settings file: /etc/mmhotline/mmhotline_settings.yml
   ...
   Backup: not set up; see https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/back-up-and-restore-the-database.md#set-up
   MMHotline: https://ops.example.com
   Next: set up cloudflared and a Cloudflare Access application for https://ops.example.com, add cloudflare_access to /etc/mmhotline/mmhotline_settings.yml, and apply the file, with your deploy tool or with sudo mmhotline settings apply (https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/put-mmhotline-behind-cloudflare-access.md)
   ```

   **Check:** the command exits 0, and

   ```sh
   systemctl is-active mmhotline
   mmhotline version
   ```

   print `active` and the release, such as
   `mmhotline 0.8.0 (COMMIT, linux/amd64, modified=false)`, with the
   release's full commit hash in place of `COMMIT`.

   From now on, run the installed `mmhotline`, without `./`. You can
   delete the downloaded files.

7. **Go on with the `Next:` line:** put MMHotline behind Cloudflare Access,
   as [Put MMHotline behind Cloudflare Access](put-mmhotline-behind-cloudflare-access.md)
   says, so people can open it. Until then, nobody can open MMHotline's
   pages except with a sign-in link, as
   [Sign in during a Cloudflare outage](sign-in-during-a-cloudflare-outage.md)
   says.

## What the install does

- It copies the program to `/usr/local/bin/mmhotline`.
- It creates three Unix users with no login: `mmhotline`, which runs
  MMHotline, and `mmhotline-triage-agent` and `mmhotline-review-agent`,
  which the team's coding agents run as, if the team sets them up.
- It creates the database `/var/lib/mmhotline/mmhotline.db` for the team
  named in `team_name`, with the `people` of the settings file, if the
  database does not exist yet. One installation serves one team. An existing
  database stays, with every row.
- It gives the settings file the group `mmhotline` and mode `0640`, so
  MMHotline can read it and only root can change it.
- It writes systemd units and starts them: `mmhotline.service`, and the
  sockets through which MMHotline starts agent runs and the dashboard's
  server view. With `backup.env` and Litestream in place, it also writes
  the backup's service, as [Back up and restore the database](back-up-and-restore-the-database.md)
  says.

MMHotline then serves its pages only on the server itself, at
`web_listen_address` (`127.0.0.1:8480`), and takes alerts at
`alert_intake_listen_address`. Nothing listens on a public address.
[Install files](../reference/installed-files.md) lists each file and its owner
and mode.

**Running the install again is safe.** It keeps what is already correct
and says so, such as
`Binary: already correct (/usr/local/bin/mmhotline)` and
`Database: mmhotline.db already exists; kept`. It restarts MMHotline only
when the program or a unit changed, or MMHotline was stopped. Reason: a
deploy tool can run the install on every deploy.

## Build from source

Build on the server, with [Go 1.27.4](https://go.dev/dl/) and Git
installed; `go.mod` selects that Go toolchain. No C compiler is needed.

```sh
git clone https://github.com/monkeymaxxing/mmhotline.git
cd mmhotline
export GOCACHE="${TMPDIR:-/tmp}/go-build"
CGO_ENABLED=0 go build -o "${TMPDIR:-/tmp}/mmhotline" ./cmd/mmhotline
```

When GitHub is down, clone the mirror,
`https://gitlab.com/monkeymaxxing/mmhotline.git`, instead. To build a
release, run `git checkout v0.8.0` after `cd mmhotline`, with the release's
version in place of `0.8.0`.

**Check:** `"${TMPDIR:-/tmp}/mmhotline" version` prints the version. Then
follow [Install a release](#install-a-release) from step 4, with
`sudo "${TMPDIR:-/tmp}/mmhotline"` in place of `sudo ./mmhotline`.

Release archives are built by `scripts/build-release-archives.py`, as
[Release archives](../changing-mmhotline/design/development.md#release-archives)
describes.

## If something is different

- **`sha256sum` prints `FAILED`** and
  `sha256sum: WARNING: 1 computed checksum did NOT match`: the download is
  damaged or is not the release. Delete it and download again; never run
  it.
- **The install says the settings file is missing:**
  `mmhotline: install requires the settings file /etc/mmhotline/mmhotline_settings.yml; ...`
  Do step 4. The install made no change and exits 4.
- **The install lists settings problems:** it checks the file as step 5
  does, and also its owner and mode, changes nothing, and names each key with a problem. Fix the file
  and run the install again.
- **The install ends with `systemctl restart mmhotline failed`:** MMHotline
  did not start. The install prints the service's last log lines, and the
  last of them says why, such as
  `MMHotline cannot listen on alert_intake_listen_address 10.20.0.9:8484: listen tcp 10.20.0.9:8484: bind: cannot assign requested address`,
  when the address is not one of the server's own (`ip -brief addr` lists
  them). Fix the cause, then run the install again.
- **The install says `install requires root; run sudo mmhotline install`,**
  or that it requires Linux, or running systemd at `/run/systemd/system`:
  run it with `sudo` on the Linux server, which must have booted with
  systemd. It checks these before it changes anything.
