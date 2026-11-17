# Sign in during a Cloudflare outage

**Who acts:** a person on the team who has SSH access to the MMHotline
server, with someone with `sudo` there to make the link; often the same
person.

**When to use this page:** MMHotline's usual address does not open
because Cloudflare is down, or your team runs MMHotline without
Cloudflare.

**What this page does:** you open MMHotline in your own browser through
an SSH connection to the server, with a one-time sign-in link, without
Cloudflare.

**Terms:**

- An **SSH port forward** makes a port on your computer reach a port on
  the server through your SSH connection; here, `localhost:8480` on your
  computer reaches MMHotline on the server.
- A **sign-in link** signs one person in once, and expires after 10
  minutes.

## Before you start

- You can log in to the MMHotline server over SSH from the computer whose
  browser you will use.
- Your sign-in email address is in `people` in the settings file,
  `/etc/mmhotline/mmhotline_settings.yml`, as last applied.

## Steps

1. **Make the link on the server,** with your own sign-in email address:

   ```sh
   sudo mmhotline people create-sign-in-link --sign-in-email-address barbara@example.com
   ```

   **Check:** it prints the link, when it expires in UTC, and the SSH
   forward command:

   ```text
   Sign-in link: http://localhost:8480/sign-in/TOKEN
   Sign-in link valid until (UTC): 2026-11-15T07:33:04Z
   SSH forward command: ssh -L 8480:127.0.0.1:8480 YOUR_SSH_HOST
   This sign-in link works once.
   ```

   `TOKEN` is a long random value. Reason for the 10 minutes: someone
   could copy the link from the terminal.

2. **Start the forward on your own computer,** with `YOUR_SSH_HOST`
   replaced by how you log in to the server, and leave it running:

   ```sh
   ssh -L 8480:127.0.0.1:8480 barbara@util01.example.com
   ```

3. **Open the link** in your computer's browser within the 10 minutes.

   **Check:** the link takes the browser to the dashboard, whose menu says
   `Signed in as Barbara Example`.
   The browser stays signed in for 12 hours at `http://localhost:8480`
   while the forward runs, long enough for one long incident.

4. **Work from `http://localhost:8480`.** Links in notifications still
   point to the usual address, such as `https://ops.example.com`, which is
   down; open incidents from the dashboard instead.

## Claim without a browser

To claim or resolve an incident and nothing else, you need no sign-in link
and no forward. Run `sudo mmhotline incidents claim` on the server, as
[Handle an incident](handle-an-incident.md#claim-and-resolve-from-the-servers-terminal)
says.

## What keeps this safe

- MMHotline keeps only a hash of each link, so a copy of the database or
  its backup signs nobody in.
- Each link works once.
- Removing a person from `people` ends their sign-in links and their
  signed-in browsers at once.
- The service log names who signed in with a link, never the link.
- **While Cloudflare Access still works,** a request that comes through
  it is judged by its Cloudflare sign-in alone, so open the link through
  the SSH forward, not through the usual address.

## If something is different

- **The browser shows** `Sign-in link unavailable` with
  `This sign-in link was already used or has expired.`: make a new link
  as in step 1 and open it within 10 minutes. The page shows the command.
- **The command says**
  `mmhotline: no member has sign-in email address nobody@example.com`:
  use the address in `people`, or add the person as
  [Set up people and the on-call schedule](set-up-people-and-the-on-call-schedule.md)
  says.
- **The browser can't reach `localhost:8480`:** the forward is not
  running, or a program on your computer already uses port 8480; run the
  forward again with another local port, such as `-L 8481:127.0.0.1:8480`,
  and change the link's port to match.
- **The forward command names another port:** it comes from the settings
  file's `web_listen_address`; use the command as printed.
- **For a script:** add `--json` to get `sign_in_link_url`,
  `sign_in_link_valid_until`, and `ssh_forward_command`.
