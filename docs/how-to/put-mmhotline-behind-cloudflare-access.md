# Put MMHotline behind Cloudflare Access

**Who acts:** an administrator of the team's Cloudflare account, in the
Cloudflare dashboard, together with an administrator with `sudo` on the
MMHotline server, or that person's coding agent. A coding agent does the
server steps and tells the person what to create in Cloudflare.

**When to use this page:** after the install, so that people can open
MMHotline on their phones and computers; or to change who may sign in.

**What this page does:** you connect a host name, such as
`ops.example.com`, the host name of `web_url` that links in notifications
use, to MMHotline through Cloudflare, and make Cloudflare
ask each person to sign in first. Then you open MMHotline on your phone.

**Terms:**

- **Cloudflare Access** is Cloudflare's sign-in gate for private web
  applications. A person signs in with a one-time code sent by email, or
  with the company's single sign-on, such as Google or GitHub.
- A **Cloudflare Tunnel** carries requests for the host name to the
  server. Its connector, `cloudflared`, runs on the server and connects out
  to Cloudflare, so the server accepts no web connection from the
  internet.
- The **team domain** is your Zero Trust account's sign-in address, such
  as `example.cloudflareaccess.com`. The **Application Audience (AUD)
  Tag** is the ID of one Access application. MMHotline needs both to check
  each sign-in.

## Choose the route

- **The team uses Cloudflare:** follow [Steps](#steps).
- **The team has no Cloudflare,** or wants none yet: write
  `web_url: http://localhost:8480` in the settings file and leave out
  `cloudflare_access`. People then sign in with a sign-in link through an
  SSH forward, as
  [Sign in during a Cloudflare outage](sign-in-during-a-cloudflare-outage.md)
  says. Each link works once and needs someone with `sudo` on the server.
- **Only change who may sign in:** see [Who can sign in](#who-can-sign-in).

## Before you start

- MMHotline is installed, as [Install MMHotline](install.md) says, and
  `web_url` in the settings file is the host name you will use, such as
  `https://ops.example.com`.
- The Cloudflare account has Zero Trust, and Cloudflare manages the DNS of
  the domain, such as `example.com`.
- The server can reach Cloudflare on the internet.

Cloudflare renames its dashboard's menus from time to time, so the steps
say what to create more than where to click.

## Steps

1. **Install `cloudflared` on the server** from Cloudflare's package
   repository, so it updates with the server's other packages. On Debian
   or Ubuntu:

   ```sh
   sudo mkdir -p --mode=0755 /usr/share/keyrings
   curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg | sudo tee /usr/share/keyrings/cloudflare-main.gpg >/dev/null
   echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' | sudo tee /etc/apt/sources.list.d/cloudflared.list
   sudo apt-get update && sudo apt-get install cloudflared
   ```

   For another distribution, follow
   [Cloudflare's package page](https://pkg.cloudflare.com/).

   **Check:** `cloudflared --version` prints a version, such as
   `cloudflared version 2026.11.0 (built 2026-11-12-12:17 UTC)`.

2. **Create the tunnel.** In the Cloudflare dashboard, under Zero Trust,
   create a Cloudflare Tunnel of the `cloudflared` type, named after the
   server, such as `util01`. The dashboard then shows install commands.
   `cloudflared` is already installed, so on the server run only the one
   that starts with `sudo cloudflared service install`, followed by a long
   token. It runs the connector as the `cloudflared` systemd service.

   **Check:** the dashboard shows the tunnel's connector as connected, and
   `systemctl is-active cloudflared` on the server prints `active`.

3. **Send the host name to MMHotline.** Add a route to the tunnel: a
   published application with the public host name `ops.example.com` and
   the service URL `http://127.0.0.1:8480`.

   **Check:** the route is listed on the tunnel. `127.0.0.1:8480` is
   `web_listen_address` in the settings file; if you changed that, use
   yours.

4. **Create the Access application.** Create a self-hosted Access
   application whose domain is `ops.example.com`, with an Allow policy
   that names your company, such as emails ending in `@example.com`, or a
   group from your company's sign-in provider.

   **Check:** opening `https://ops.example.com` in a browser shows
   Cloudflare's sign-in page, not MMHotline.

   Reason: never use a rule that lets in everyone who signs in. With the
   one-time email code, that is anyone with an email address, and only
   MMHotline's own check of the team's people would stand between the
   internet and its pages.

5. **Give MMHotline the two values.** Copy from Cloudflare:

   - the team domain, under **Zero Trust → Settings**, as a bare host
     name, without `https://`;
   - the application's **Application Audience (AUD) Tag**: under
     **Zero Trust → Access controls → Applications**, choose **Configure**
     on the application, then open **Additional settings**.

   Open the settings file with `sudoedit /etc/mmhotline/mmhotline_settings.yml`,
   and add this section at the top level, not indented, after
   `alert_intake_listen_address`:

   ```yaml
   cloudflare_access:
     team_domain: example.cloudflareaccess.com
     audience_tag: YOUR_APPLICATION_AUDIENCE_TAG
   ```

   Then apply the file and read MMHotline's log:

   ```sh
   sudo mmhotline settings apply
   sudo journalctl -u mmhotline --no-pager -n 20
   ```

   **Check:** the apply restarts MMHotline first and says so, because
   `cloudflare_access` changes only at a restart:

   ```text
   Restarting MMHotline to apply the settings that require a restart: cloudflare_access.
   ...
   Restarted MMHotline first, because these keys change only when it restarts: cloudflare_access.
   ```

   It ends with `Every part of the settings file is applied.`, and the log
   says:

   ```text
   Sign-in: Cloudflare Access for example.cloudflareaccess.com, and sign-in links
   ```

6. **Open MMHotline.** On your phone, open `https://ops.example.com`, and
   sign in at Cloudflare with your sign-in email address: the one in
   `people` in the settings file.

   **Check:** MMHotline's dashboard opens, with `Signed in as` and your
   name in the menu. MMHotline has no password and no sign-out of its own;
   Cloudflare keeps you signed in.

## Who can sign in

Two checks must both pass:

1. **Cloudflare Access** lets the person in, by the application's Allow
   policy.
2. **MMHotline** finds the email address that Access passes on among the
   `people` in the settings file. Cloudflare signs each request's token,
   and MMHotline checks the signature with the keys of your team domain,
   so nobody can claim an address without signing in at Cloudflare.

To let a new person in, add them to `people` and apply the file; if the
Allow policy does not cover their address, also change the policy. To
remove someone, remove them from `people` and apply: their browsers are
signed out at once, even while Cloudflare still lets them through.

## If something is different

- **A page titled "Not a team member"** (HTTP status 403) says
  `The sign-in email address ADDRESS is not a member of Operations.` Access
  let the person in, but the address is not in `people`. Sign in with the
  address in `people`, or add the shown address to `people` and apply.
  The page's **Where team members are set** part names the settings file
  and how to apply it. MMHotline's log says
  `Sign-in refused: ADDRESS is not a member of the team`.
- **A page titled "Sign in through Cloudflare Access"** (HTTP status 401)
  says `Open MMHotline through Cloudflare Access to sign in.` The request
  reached MMHotline without a token MMHotline accepts. Check that you
  opened the host name, not the server's own address, and that
  `team_domain` and `audience_tag` in the settings file match Cloudflare's
  values exactly. The page's **Sign in during an outage** part gives the
  sign-in link command.
- **A page titled "Cannot check sign-ins right now"** (HTTP status 503)
  says `MMHotline cannot check Cloudflare Access sign-ins right now.`
  MMHotline could not get your team domain's keys from Cloudflare. Use a
  sign-in link, as
  [Sign in during a Cloudflare outage](sign-in-during-a-cloudflare-outage.md)
  says, until Cloudflare answers again.
- **The host name shows a Cloudflare error page,** not the sign-in page or
  MMHotline: the tunnel does not reach MMHotline. Check that the tunnel's
  connector is connected, that its route's service URL is
  `http://127.0.0.1:8480`, and that `systemctl is-active mmhotline` prints
  `active`.
