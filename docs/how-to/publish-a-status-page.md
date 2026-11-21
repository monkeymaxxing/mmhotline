# Publish a status page

**Who acts:** an administrator with a Cloudflare account, a laptop, and
`sudo` on the MMHotline server, or that person's coding agent.

**When to use this page:** to give your customers a public page where the
team says when something is wrong, what it does about it, and when the
problem is over.

**What this page does:** you create the page's storage and address in
Cloudflare from your laptop, then connect MMHotline to it.

**Terms:**

- The **status page** is three files in an R2 bucket, Cloudflare's object
  storage, in your own Cloudflare account, served at a host name such as
  `status.example.com`: `index.html`, the page; `feed.xml`, an Atom feed that
  customers can subscribe to in a feed reader; and `status.json`, the same
  notices for programs. It stays readable while the MMHotline server is down.
- A **notice** is one problem or planned maintenance on the page, with a
  title, its updates, and open until someone resolves it.
- A **component** is a part of your service, such as the website or the API,
  that a notice can say it affects. Components are optional.

Setup is optional.

## Before you start

- A Cloudflare account that manages the DNS of the domain for the page's
  host name, such as `example.com` for `status.example.com`. If your main
  domain's DNS is elsewhere, register a separate status domain on
  Cloudflare.
- The `mmhotline` program on your laptop, as
  [Post to the status page](post-to-the-status-page.md#install-mmhotline-on-your-laptop)
  says. Reason: the Cloudflare part of the setup runs on the laptop, and
  the laptop posts to the page when the MMHotline server is down.

## Steps

Steps 1 to 3 happen in the Cloudflare dashboard and on your laptop, steps
4 and 5 on the MMHotline server; copy the bucket URL and the R2 key to the
server by hand, or through your deploy tool's vault.

1. **Make a setup token.** In the Cloudflare dashboard, copy your account
   ID from the account's home page. Under **My Profile → API Tokens**,
   create a custom token with these permissions; under **Zone Resources**,
   choose your host name's zone, such as `example.com`:

   - **Account › Workers R2 Storage › Edit**, to create the bucket;
   - **Zone › Zone › Read**, to find the zone;
   - **Zone › Transform Rules › Edit**, to add the rule that serves
     `index.html`.

   MMHotline uses this token only for the setup and keeps no copy, and you
   delete it right after step 2, so a token under **My Profile** is fine
   here, unlike the long-lived keys in step 3 and in the MMHotline-down
   alarm.

2. **Run the setup on your laptop.** Put only the token in a private file:

   ```sh
   install -m 0600 /dev/null ~/cloudflare-api-token
   nano ~/cloudflare-api-token
   ```

   Then:

   ```sh
   mmhotline status-page set-up --cloudflare-account-id 0123456789abcdef0123456789abcdef \
     --host-name status.example.com --cloudflare-api-token-file ~/cloudflare-api-token
   ```

   **Check:** it prints:

   ```text
   The status page is set up in Cloudflare account 0123456789abcdef0123456789abcdef: the R2 bucket mmhotline-status-status-example-com-18a8ea11, the host name status.example.com connected to it, and the rule that serves index.html at https://status.example.com/.
   r2_bucket_url: https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11
   Next step: on Cloudflare's site, open R2, then Manage API tokens, ...
   ```

   The command talks only to Cloudflare. It also connects the host name to
   the bucket, so you add no DNS record yourself. It creates only what is
   missing, so running it again is safe. Then delete `~/cloudflare-api-token`, and
   the token in the dashboard.

3. **Make the server's R2 key.** In the dashboard, open **R2 → Manage API
   tokens**, and create an **account** API token with the permission
   **Object Read & Write**, for the bucket from step 2 only. Copy its
   **Access Key ID** and **Secret Access Key**.

   Reason for an account token: it keeps working when the person who made
   it leaves the team. MMHotline keeps the key to publish each change.

4. **Add the page to the settings file,** with your values:

   ```yaml
   status_page:
     page_title: Example Inc. status
     host_name: status.example.com
     r2_bucket_url: https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11
     r2_access_key_id: "YOUR_ACCESS_KEY_ID"
     r2_secret_access_key: "YOUR_SECRET_ACCESS_KEY"
   ```

   `page_title` is the page's title and heading, exactly as written.
   `r2_bucket_url` is the line step 2 printed. A team whose deploy tool
   writes the settings file keeps the key in its vault.

   Three optional keys make the page more useful; add the ones you want:

   ```yaml
   status_page:
     website_url: https://example.com
     support_url: mailto:support@example.com
     components:
       - component_id: website
         name: Website
         description: Main site and customer dashboard
       - component_id: api
         name: API
   ```

   `website_url` and `support_url` add **Website** and **Support** links to
   the page's header; they must be `https://` addresses, and `support_url`
   may also be `mailto:` and one email address. `components` lists the parts
   of your service that a notice can name; the page shows each with its
   status. A `component_id` uses lowercase letters, digits, hyphens, and
   underscores, and the posting form and the `--component` flag use it. Keep
   these keys inside the same `status_page` section.

5. **Apply the file:**

   ```sh
   sudo mmhotline settings apply
   ```

   **Check:** `status_page: applied.` The first apply publishes the page
   and its feed, and MMHotline saves the setup only once that worked.
   `sudo mmhotline status-page show` then says:

   ```text
   Status page: https://status.example.com/
   R2 bucket: mmhotline-status-status-example-com-18a8ea11 (https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11)
   Set up by the settings file /etc/mmhotline/mmhotline_settings.yml at 2026-11-15T07:51:11Z.
   Example Inc. status, published at 2026-11-15T07:51:10Z.
   No open notices: the page says that everything is working.
   ```

   With the optional keys from step 4, `show` also lists the links and each
   component, such as `Component Website (website): operational`.

6. **Open the page** at `https://status.example.com/`.

   **Check:** it shows the title, **Working**, and
   `Everything is working.` With the optional keys, it also shows the
   **Website** and **Support** links and a panel that lists each component
   as **Operational**. The header's **Subscribe** link and the footer's
   **Atom feed** link open `https://status.example.com/feed.xml`, which shows
   a feed with no entries. Right after setup, Cloudflare may answer `403` for
   a few minutes, until the host name's certificate is ready. The feed needs
   no extra Cloudflare setup.

Next, set up each responder's laptop to post when the server is down, as
[Post to the status page](post-to-the-status-page.md) says.

## Change or remove the page

- **A new R2 key:** put it in the settings file and apply.
- **A new title, link, or component:** change it in the `status_page`
  section and apply. The apply publishes the page again with the new values
  and keeps every notice. A component you remove stays named on the old
  notices that used it, by its `component_id`.
- **Turn it off in MMHotline:** delete the `status_page` section and apply;
  the line says `status_page: not set up, because the settings file leaves it out.`
  The bucket and the page stay in Cloudflare. Delete them in the
  dashboard if you no longer need them: the bucket's files, the bucket's
  custom domain, the bucket, and, among the zone's URL rewrite rules, only
  the one for your host name.

## If something is different

- **The apply says `status_page: did not apply: ...`:** the R2 key cannot
  write to the bucket; the line says why. Check that the token is for this
  bucket with **Object Read & Write**.
- **The page answers `403`** long after setup: in the dashboard, check
  that the bucket's custom domain says **Active**.
