# Post to the status page

**Who acts:** any person on the team, in MMHotline's pages; someone with
`sudo` on the MMHotline server; or a responder with the `mmhotline`
program on their laptop when the server is down.

**When to use this page:** customers are affected by a problem, and you
want to tell them, update them, and say when it is over.

**What this page does:** you open a notice on the public status page, add
updates, resolve it, and fix a mistake in a notice that is already public.

**Terms:**

- A **notice** is one problem or planned maintenance on the status page,
  with a title, its updates, and open until resolved.
- A notice's **impact** is **Outage**, **Degraded performance**, or
  **Scheduled maintenance**. The worst impact among the open notices sets
  the banner at the top of the page.
- A **stage** is an optional label on an update. For an outage or degraded
  notice it is Investigating, Identified, Monitoring, or Resolved. For
  maintenance it is Scheduled, In progress, or Completed.
- A **component** is a part of your service, such as the website or the API,
  that the settings file lists. A notice can say which components it
  affects.

The page is set up as [Publish a status page](publish-a-status-page.md)
says. Customers see only what someone posts: an SOS posts nothing by itself.
They can follow the page by opening it, or by subscribing to its Atom feed,
`https://status.example.com/feed.xml`, in a feed reader. The page's
**Subscribe** link opens it.

## Choose the route

- **From an incident, in MMHotline:** [From MMHotline's pages](#from-mmhotlines-pages).
- **From the server's command line:** [From the server](#from-the-server).
- **The MMHotline server is down:** [From your laptop](#from-your-laptop).
- **A notice or update is wrong:** [Fix a mistake](#fix-a-mistake).

## From MMHotline's pages

1. **On the incident's page,** choose **Post to the status page**. It
   publishes an outage notice titled `Service disruption` that says
   `We are looking into a problem.`, and opens the **Status page** page.
   While that notice is open, the incident's page shows
   `Notice 1, posted from this incident, is open on the status page.`
   and a link to update or resolve it, instead of the button. Without an
   incident, open **Status page** from the main menu and use **Post a new
   notice**: give a title, choose the impact, tick the components it
   affects (when the page has components), and write the first message.
   For maintenance, set the planned start and end, which you enter in your
   team's time zone. The stage of the first update defaults to
   Investigating, or Scheduled for maintenance.
2. **Add your own words** under the notice, with **Post the update**. Choose
   a stage if it helps, such as Identified, or leave it as it is.
3. **When the problem is over,** open **Resolve this notice**, add an
   optional last update such as `Everything works again.`, and choose
   **Resolve this notice**. The last update's stage is Resolved, or
   Completed for maintenance.

The button shows only once the status page is set up. The public page
opens with a banner that says `Everything is working.` or, while a notice is
open, `Scheduled maintenance`, `Some systems are experiencing issues.`, or
`We are experiencing a service outage.` Its times are in the visitor's
local time zone, or in UTC when the browser runs no scripts.

**Check:** the public page shows the notice, its title, its impact, and its
updates, and, once resolved, says `Everything is working.` with the notice
under **Recently resolved**. The page's feed, `feed.xml`, lists the notice
as `[Outage] Service disruption`, then `[Resolved] Service disruption`.

## Fix a mistake

A wrong word or a notice posted by mistake is public, so fix it at once.
On the **Status page** page, each notice, open or resolved (under **Recently
resolved**), has three folded forms:

- **Edit notice details:** change the title, the impact, the components, or
  the planned start and end. Changing the impact away from maintenance
  removes the planned window.
- **Edit or delete updates:** change an update's stage or message, or delete
  an update. The public page marks an edited update `(edited)`. A notice
  keeps at least one update; to remove the only one, delete the notice.
- **Delete this notice:** tick the confirmation and choose **Delete this
  notice**. The notice and all its updates leave the public page and the
  feed. This cannot be undone.

**Check:** the public page shows the change within about a minute.

If the page says `notice N changed while you were editing it; reload the
page and try again`, someone else changed the notice after you opened the
page. Reload, look at what changed, and make your change again. Your typed
text stays in the form until you reload.

## From the server

```sh
sudo mmhotline status-page post --title "Sign-in is slow" --impact degraded --component api \
  --stage investigating --message "Sign-in is slow for some customers. We are looking into it."
sudo mmhotline status-page post --title "Database upgrade" --impact maintenance --stage scheduled \
  --scheduled-start 2026-11-21T02:00:00Z --scheduled-end 2026-11-21T04:00:00Z \
  --message "We will upgrade the database. Orders pause for a few minutes."
sudo mmhotline status-page post --notice-id 1 --stage identified --message "Sign-in works again for most customers."
sudo mmhotline status-page resolve --notice-id 1 --closing-message "Everything works again."
sudo mmhotline status-page edit --notice-id 1 --title "Slow sign-in"
sudo mmhotline status-page edit --notice-id 1 --update-id 2 --message "Sign-in works again for most customers. We are watching it."
sudo mmhotline status-page delete --notice-id 1 --update-id 2
sudo mmhotline status-page delete --notice-id 1
```

Without `--notice-id`, `post` opens a new notice and needs `--title`;
`--impact` is `outage` unless you give `degraded` or `maintenance`, and
`--component` repeats for each component. For maintenance, add
`--scheduled-start` and `--scheduled-end`, such as `2026-11-21T02:00:00Z`.
`--stage` is optional, and without it the message has no stage. Give
`--stage scheduled` to announce maintenance ahead of time: the components it
affects stay **Operational** on the page until a later update has the stage
`in_progress`. The [command reference](../reference/mmhotline-command.md#mmhotline-status-page-post)
lists every flag.

**Check:** each prints the page's state, such as

```text
Open notice 1, degraded: Sign-in is slow
  Affects API.
  Opened at 2026-11-15T07:51:26Z.
  Update 1, investigating, at 2026-11-15T07:51:26Z: Sign-in is slow for some customers. We are looking into it.
```

then `Posted to notice 1.`, `Notice 1 is resolved.`, `Edited notice 1.`,
`Edited update 2 of notice 1.`, `Deleted update 2 of notice 1.`, or
`Deleted notice 1.` The number after `Update` is what `--update-id` takes.
`sudo mmhotline status-page show` shows the page now, with the updates of open
notices only. `sudo mmhotline status-page show --json` lists every notice,
resolved ones too, with the whole history and each update's ID. `delete` asks
nothing first.

## From your laptop

When the MMHotline server is down, the laptop writes straight to the
page's bucket, with no MMHotline service. Set it up before an outage; it
can post only after the server's first `settings apply` published the
page.

### Install mmhotline on your laptop

Use the release that runs on the server, which `mmhotline version` on the
server names; the commands below use `0.8.1`. A Mac with Apple silicon uses
the `macos-arm64` archive, an Intel Mac `macos-amd64`; `uname -m` prints
`arm64` or `x86_64`. A Linux laptop uses the Linux archives, as
[Install MMHotline](install.md#install-a-release) says. On the Mac:

```sh
curl --fail --location --remote-name-all \
  https://github.com/monkeymaxxing/mmhotline/releases/download/v0.8.1/mmhotline-0.8.1-macos-arm64.tar.gz \
  https://github.com/monkeymaxxing/mmhotline/releases/download/v0.8.1/SHA256SUMS
shasum -a 256 --check --ignore-missing SHA256SUMS
tar -xzf mmhotline-0.8.1-macos-arm64.tar.gz
sudo install -d -m 0755 /usr/local/bin
sudo install -m 0755 mmhotline-0.8.1-macos-arm64/mmhotline /usr/local/bin/mmhotline
mmhotline version
```

**Check:** `shasum` prints `mmhotline-0.8.1-macos-arm64.tar.gz: OK`;
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
mmhotline status-page post --title "Main server down" --message "Our main server is down. We are moving to the backup."
mmhotline status-page post --notice-id 1 --message "Sign-in works again for most customers."
mmhotline status-page resolve --notice-id 1 --closing-message "Everything works again."
mmhotline status-page edit --notice-id 1 --title "Main server outage"
mmhotline status-page delete --notice-id 1 --update-id 2
```

`post` without `--notice-id` opens a new notice and needs `--title`; the
page's components are listed in `show`, and `--component` takes a
component's ID, as on the server. Each command prints the
page's state with each open notice's number, such as
`Open notice 1, outage: Main server down`, so use the number it
shows. **Check:** each ends with `directly in the R2 bucket`, such as
`Posted to notice 1, directly in the R2 bucket.`,
`Notice 1 is resolved, directly in the R2 bucket.`, and
`Edited notice 1, directly in the R2 bucket.` With
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
- **The command says `--title is required to post a new notice`:** without
  `--notice-id`, `post` opens a new notice, which needs a title. To add an
  update to an open notice, give its `--notice-id`.
- **The command says the status page has no component:** `--component`
  takes a `component_id` from the settings file's `status_page` section;
  the message lists the ones the page has.
- **The command refuses an update to a resolved notice:** a resolved notice
  takes no more updates. Open a new notice, or fix the old one with `edit`.
