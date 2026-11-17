# Post to the status page

**Who acts:** any person on the team, in MMHotline's pages; someone with
`sudo` on the MMHotline server; or a responder with the `mmhotline`
program on their laptop when the server is down.

**When to use this page:** customers are affected by a problem, and you
want to tell them, update them, and say when it is over.

**What this page does:** you open a notice on the public status page, add
updates, and resolve it.

**Terms:** a **notice** is one problem on the status page, with its
updates, open until resolved. The page is set up as
[Publish a status page](publish-a-status-page.md) says. Customers see
only what someone posts: an SOS posts nothing by itself.

## Choose the route

- **From an incident, in MMHotline:** [From MMHotline's pages](#from-mmhotlines-pages).
- **From the server's command line:** [From the server](#from-the-server).
- **The MMHotline server is down:** [From your laptop](#from-your-laptop).

## From MMHotline's pages

1. **On the incident's page,** choose **Post to the status page**. It
   publishes `We are looking into a problem.` as a new notice, and opens
   the **Status page** page. While that notice is open, the incident's
   page shows `Notice 1, posted from this incident, is open on the status page.`
   and a link to update or resolve it, instead of the button. Without an
   incident, open **Status page** from the main menu and
   use **Post a new notice**, with your own first words.
2. **Add your own words** under the notice, with **Post the update**.
3. **When the problem is over,** open **Resolve this notice**, add an
   optional last update such as `Everything works again.`, and choose
   **Resolve this notice**.

The button shows only once the status page is set up. The public page
shows **Problem now** while a notice is open; its times are in UTC.

**Check:** the public page shows the notice and its updates, and, once
resolved, says `Everything is working.` with the notice under
**Recently resolved**.

## From the server

```sh
sudo mmhotline status-page post --message "Sign-in is slow for some customers. We are looking into it."
sudo mmhotline status-page post --notice-id 1 --message "Sign-in works again for most customers."
sudo mmhotline status-page resolve --notice-id 1 --closing-message "Everything works again."
```

**Check:** each prints the page's state, such as
`Open notice 1, opened at 2026-11-15T07:51:26Z. Latest update, at 2026-11-15T07:51:26Z: Sign-in is slow for some customers. We are looking into it.`,
then `Posted to notice 1.` or `Notice 1 is resolved.`
`sudo mmhotline status-page show` shows the page now. Without
`--notice-id`, `post` opens a new notice.

## From your laptop

When the MMHotline server is down, the laptop writes straight to the
page's bucket, with no MMHotline service. Set it up before an outage; it
can post only after the server's first `settings apply` published the
page.

### Install mmhotline on your laptop

Use the release that runs on the server, which `mmhotline version` on the
server names; the commands below use `0.3.1`. A Mac with Apple silicon uses
the `macos-arm64` archive, an Intel Mac `macos-amd64`; `uname -m` prints
`arm64` or `x86_64`. A Linux laptop uses the Linux archives, as
[Install MMHotline](install.md#install-a-release) says. On the Mac:

```sh
curl --fail --location --remote-name-all \
  https://github.com/monkeymaxxing/mmhotline/releases/download/v0.3.1/mmhotline-0.3.1-macos-arm64.tar.gz \
  https://github.com/monkeymaxxing/mmhotline/releases/download/v0.3.1/SHA256SUMS
shasum -a 256 --check --ignore-missing SHA256SUMS
tar -xzf mmhotline-0.3.1-macos-arm64.tar.gz
sudo install -d -m 0755 /usr/local/bin
sudo install -m 0755 mmhotline-0.3.1-macos-arm64/mmhotline /usr/local/bin/mmhotline
mmhotline version
```

**Check:** `shasum` prints `mmhotline-0.3.1-macos-arm64.tar.gz: OK`;
otherwise stop. Download with `curl`, not a browser. Reason: macOS refuses
to open a program a browser downloaded, because Apple has not notarized
it. After each upgrade of the server, install the new release here too.

### Give the laptop its own key

In the Cloudflare dashboard, under **R2 → Manage API tokens**, each
responder creates their own **user** API token with **Object Read &
Write** for the page's bucket only. Reason: you can then revoke one
responder's key alone, and a user token stops when that person leaves the
account. Put it in a file only you can read:

```sh
mkdir -p ~/.config/mmhotline
install -m 0600 /dev/null ~/.config/mmhotline/status-page.env
nano ~/.config/mmhotline/status-page.env
```

and write these three lines, the bucket URL being the settings file's
`r2_bucket_url`, which `sudo mmhotline status-page show` also prints:

```sh
export MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL=https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11
export MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID=ACCESS_KEY_ID
export MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY=SECRET_ACCESS_KEY
```

### Post

Load the file in each new terminal, then work as on the server:

```sh
. ~/.config/mmhotline/status-page.env
mmhotline status-page show
mmhotline status-page post --message "Our main server is down. We are moving to the backup."
mmhotline status-page post --notice-id 1 --message "Sign-in works again for most customers."
mmhotline status-page resolve --notice-id 1 --closing-message "Everything works again."
```

`post` without `--notice-id` opens a new notice; each command prints the
page's state with each open notice's number, such as
`Open notice 1, opened at 2026-11-15T07:51:26Z. ...`, so use the number it
shows. **Check:** each ends with `directly in the R2 bucket`, such as
`Posted to notice 1, directly in the R2 bucket.` and
`Notice 1 is resolved, directly in the R2 bucket.` With
`MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL` set, these commands never contact a
MMHotline server. When the server runs again, it reads the page from the
bucket, so it sees your posts.

## If something is different

- **On the server, the command says the status page is not set up:** the
  `status_page` section is missing from the settings file.
- **On the laptop, the command says**
  `mmhotline serve is not running at command socket /run/mmhotline/command.sock`:
  the `.env` file was not loaded in this terminal, so the command looked
  for a MMHotline server. Load it with `. ~/.config/mmhotline/status-page.env`.
- **The command lists all flags:** `--notice-id` needs a number, such as
  `--notice-id 1`; `show` names each open notice's number.
