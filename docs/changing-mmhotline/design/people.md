# People, teams, and access

## Members and roles

A member is a person allowed to use the installation's one team. The
installation's SQLite database stores each member's stable `id`,
`sign_in_email_address`, `name`, `role`, and optional `removed_at` time. The
sign-in email address is the address the person's Cloudflare Access login gives
([authentication](#authentication-and-recovery)). MMHotline trims the sign-in
email address and stores the address in lower case.

Each active member's sign-in email address is unique among active members.
A sign-in email address has 3–254
characters, exactly one `@` with text on both sides, and no spaces or control
characters. MMHotline sends no mail to the sign-in email address.
MMHotline only compares the stored sign-in email address with the address a
sign-in names. The sign-in
email address requirements check address shape, not deliverability.

The name says "sign-in" because an email contact method's address already
uses `--email-address`. Names contain 1–100 Unicode characters after
trimming surrounding space. Roles are
`administrator` and `member`.

Administrators and members can inspect alerts, history, on-call coverage and people. Administrators and
members can create, claim, resolve, or escalate incidents. An SOS is the
urgent alert that wakes the person on call, even at night; claiming an
incident tells the team you are handling it, and MMHotline stops contacting
the next person.
Each administrator or member manages personal contact methods belonging to
that administrator or member.
Administrators also open the administration pages, such as the Triage agent
page and Sending services. Administrators manage shared contact methods,
which belong to the whole team, and team mute.

Personal contact methods are visible to their owner and administrators;
only the owner edits personal contact methods. The people, schedule, alert sources, rules for running
the agent's playbooks, agents' setups, sending services, MMHotline-down alarm, and status page come
from the [settings file](settings-file.md), which only root changes.

Other members see names, sign-in email addresses, roles, and availability, not
personal contact method addresses.

The read-only People page lists names, sign-in email addresses, and full role names.
Each person's row wraps on phones instead of hiding the role in a wide table.
“(you)” identifies the signed-in person. “Where people are set” folds the settings file path and first-line instructions.
The fold keeps the team list first while retaining the information needed to change people.

Shared contact methods belong to the whole team; they have no member owner.
The team has a name, stored in `team_info`, and no ID. Shared chat and email
subscriptions are separate from the people who can answer an incident.
[Availability](availability.md#taking-on-call) owns taking on-call and
conflicts over taking on-call.

## Setup and first administrator

`mmhotline setup --state-dir DIR --settings-file FILE`, and `install` on a
server, create `mmhotline.db` with one `team_info` row containing the settings
file's `team_name`, 1–100 Unicode characters after trimming surrounding
space. Setup refuses to replace existing state and creates no person.

The settings file's `people` lists everyone who uses MMHotline, with at least
one administrator. The settings file's check refuses a file without an
administrator. When `serve` starts, the `people` part adds the listed
people:

```yaml
people:
  - name: Alice Example
    sign_in_email_address: alice@example.com
    role: administrator
```

Alice can then open MMHotline at the settings file's `web_url` and sign in
through Cloudflare Access with `alice@example.com`, or with a sign-in link
created on the server. There is no public joining route or invitation link:
a sign-in works only for a person the settings file lists.

## Authentication and recovery

A person signs in through Cloudflare Access, or, when Cloudflare is down or
not used, with a one-time sign-in link created on the server. Cloudflare
Access sign-ins and sign-in links reach the web listener set by the
settings file's `web_listen_address`, normally
`127.0.0.1:8480` ([listeners](operation.md#listeners)). MMHotline has no
passwords and no sign-out control.

### Sign-in through Cloudflare Access

`cloudflared`, Cloudflare's tunnel connector, runs on the MMHotline server and
forwards the team's host name, such as `ops.example.com`, to
`http://127.0.0.1:8480`. A Cloudflare Access application covers the team's
MMHotline host name, such as `ops.example.com`. Cloudflare asks each person
to sign in before the request reaches the server. Sign-in uses a one-time
email code or the company's single sign-on such as Google or GitHub.
The tunnel is a connection the server opens, so the
server accepts no web connection from the internet.

Access adds a signed token to every request in the `Cf-Access-Jwt-Assertion`
header. MMHotline trusts a request only when the Access token passes
verification:

- The token is at most 16 KiB and uses RS256. MMHotline checks the token's
  signature against Cloudflare's public signing keys before reading any
  token claim.
- The issuer is exactly `https://` followed by the Cloudflare Access team
  domain, such as `example.cloudflareaccess.com`. The token's audience
  identifies the intended Access application and must contain the
  application's audience tag.
- The server time minus 60 seconds must be at or before the token's expiry
  time. The server time plus 60 seconds must be at or after the token's
  not-before time, when present. The allowance accounts for clock differences
  between Cloudflare and the server.
- The `email` claim is not empty. The lower-cased `email` claim gives the
  person's sign-in email address.

The keys come from `https://<team domain>/cdn-cgi/access/certs` and are
cached in memory. MMHotline fetches the keys and never uses configured keys,
because
Cloudflare rotates signing keys about every six weeks and keeps old keys
valid for seven days. A configured key would break sign-in at the next
rotation.

When verifying a request's token, MMHotline fetches the keys again if the
cached keys are over an hour old or the token names an unknown key.
MMHotline fetches keys at most once a minute. Invented key IDs therefore never
turn into a flood of requests to Cloudflare, and while the key address is
down, pages do not each wait on the key address. A failed fetch keeps the cached keys.

The once-a-minute limit has one visible cost: within a minute of a fetch, a token
signed with a key that fetch did not include is refused, and reloading the
page after that minute works if MMHotline can fetch the required key.
Each fetch allows 10 seconds, reads at most
1 MiB, and keeps only RSA keys of at least 2,048 bits.

A token that fails any check returns 401, a "Sign in through Cloudflare
Access" page that links to the settings file's `web_url` and names the
sign-in link
command. When MMHotline has no cached key for the token and cannot fetch the
keys, pages return 503, "Cannot check sign-ins right now", with the command
to create a sign-in link on the server.

A valid token for a sign-in email address without an active member returns
403, "Not a team member", naming the sign-in email address and team.

The page asks the person to contact an administrator to add the sign-in
email address to `people` and apply the settings file. The settings file
path and first-line instructions stay under "Where team members are set".
With Cloudflare Access configured, the 401 page keeps server recovery commands under "Sign in during an outage".
Without Cloudflare Access, the 401 page says "Sign in with a sign-in link" and shows the link and SSH forwarding instructions unfolded.
The page offers only configured sign-in methods, so a sign-in link remains easy to find without Access. The 503 page keeps the command under "Create a sign-in link on the
server". These closed folds keep the next action readable on a phone while
retaining the commands for someone with server access.

The service log records "Sign-in refused: ADDRESS is not a member of the team",
so an administrator on the server can see who was refused.
A refused sign-in link logs
"Sign-in link refused: the link was already used or has expired". A failed token
check logs "Cloudflare Access sign-in failed:" with the reason. The
401 and 403 pages show no menu: a person who is not signed in as a member
has no page to go to.

The pages listen on a loopback TCP port. Listening on loopback is safe
because MMHotline trusts no unsigned identity: every page request needs a
valid Access token or a sign-in cookie. An Access token is signed by
Cloudflare, and a sign-in cookie is a random secret held only by the browser
that opened the link, so a
local program that reaches the port cannot claim to be anyone.

The settings file's `cloudflare_access` holds the team domain and the audience
tag, each required when the section is present.
[Listeners](operation.md#listeners) defines the `team_domain` and
`audience_tag` settings. Without `cloudflare_access`, people sign in only
with sign-in links, and MMHotline ignores a `Cf-Access-Jwt-Assertion` header
if a request carries one: with no team to
check it against, the header proves nothing.

### Sign-in links

During a Cloudflare outage, or for a team without Cloudflare, a person reaches
the web listener's port through an SSH port forward, which carries no
Access header.
Someone with root on the server runs:

```sh
sudo mmhotline people create-sign-in-link --sign-in-email-address alice@example.com
```

The `create-sign-in-link` command prints a one-time link such as
`http://localhost:8480/sign-in/TOKEN`, the UTC expiry time, and the forward
command,
`ssh -L 8480:127.0.0.1:8480 YOUR_SSH_HOST`. The local port and forwarding
destination `127.0.0.1:8480` come from the web listen address.
`YOUR_SSH_HOST` is a placeholder for Alice's SSH destination, not a host
derived from the web listen address.

On her own computer, Alice runs the forward command and opens the link.
The link works once, within 10 minutes. Opening the sign-in link sets a
sign-in cookie for 12 hours and opens the dashboard. The
[command reference](../../reference/mmhotline-command.md#mmhotline-people-list) owns the output fields.

- **10 minutes** for the link, because it is printed in a terminal and could
  be copied from there.
- **12 hours** for the cookie, so one long incident needs no second link.
- **Only the root command creates a link.** Root access to the team's server
  is the recovery authority, as for the [local people command](#local-people-command);
  the pages cannot create a sign-in link.
- **Hashes only.** The `browser_sign_ins` table stores the SHA-256 of each
  link token and cookie token, never the token, so a copy of the database or
  its backup signs nobody in. Creating a link also deletes expired rows.
- **Removal ends sign-ins at once.** Removing a person deletes their links
  and cookies in the same transaction, and every check also requires an
  active member.
- **Access wins.** A request with a `Cf-Access-Jwt-Assertion` header, when
  the settings file has `cloudflare_access`, ignores any sign-in cookie, so the person
  Cloudflare signed in is the person who acts.
- **SSH forwards browser requests to the tunnel's web port.** Browser access
  uses TCP rather than a Unix socket in MMHotline's private socket directory,
  `/run/mmhotline` (mode 0700, owned by the `mmhotline` user). An SSH forward
  runs as the SSH user, which cannot open `/run/mmhotline`.

A used or expired link returns 401, "Sign-in link unavailable", with the
command to create another, under "Create a new sign-in link". A valid link
still opens the dashboard directly, without a second confirmation page.

### Cookie names follow the address the browser used

MMHotline names the sign-in and CSRF cookies for the address in the request's host:

- Through Cloudflare, at any address that is not loopback, the sign-in cookie
  is `__Host-mmhotline-sign-in` and the CSRF cookie is `__Host-mmhotline-csrf`,
  with `Secure`.
- On a loopback address (`localhost`, or an IP address such as `127.0.0.1` or
  `::1`), the SSH-forward path, the sign-in cookie is `mmhotline-sign-in`
  and the CSRF cookie is `mmhotline-csrf`, without `Secure`.

The sign-in and CSRF cookies use `HttpOnly`, `SameSite=Strict`, and path `/`
for Cloudflare and loopback access.

The loopback names drop `Secure` because WebKit, the engine in Safari, keeps
no `Secure` cookie on `http://localhost`, and the `__Host-` prefix requires
`Secure`. With `Secure`, a sign-in link opened in Safari would sign nobody in.

### Checks on every request

`RequireMember` rechecks active membership and the required role inside the
same transaction as each operation. Removing a person or changing a role
therefore takes effect on the next request, including requests from an
already open page.

MMHotline sets the CSRF cookie and includes a matching token in browser
forms. MMHotline checks the submitted token against the cookie and uses
`http.NewCrossOriginProtection` to reject cross-origin form submissions.
A malicious site could otherwise submit a form to MMHotline, and the browser would send
the form with the person's Cloudflare Access session. The CSRF cookie lasts
30 days, independently of identity, so long-open forms stay usable.
The CSRF cookie's name and attributes follow the
[address the browser used](#cookie-names-follow-the-address-the-browser-used).

## Roles and lifecycle

Everyone can list people. Applying the settings file's `people` section
adds, changes, and removes members in one transaction, matching members by
sign-in email address ([settings file](settings-file.md#people)). MMHotline
adds each new person, changes names and roles, and deactivates each person
the file leaves out. A duplicate sign-in email address in the file is
refused by its check.
The error names the two `people` entries with the duplicate sign-in email
address. The file names at least one administrator, so the
team always has an administrator.

A deactivation and the whole people part are refused while the
person owns a claimed incident or has taken on-call. MMHotline sends
reminders about a claimed incident only to the incident's owner. Removing
the owner would leave the incident waiting with nobody told.

The settings application failure message says to resolve the incident,
have someone take over the incident, or end the person's manually taken
on-call duty first.
The stored member list, names, and roles remain unchanged until the blocking
incident ownership or manually taken duty ends and a later settings
application succeeds.

A saved schedule that names the person does not prevent MMHotline from
deactivating the person. The file's schedule names only people listed in
`people`. Applying the `on_call_schedule` section comes next, saves the
file's schedule, and removes saved schedules of other types. Refusing
would prevent changing people and schedule together in one edit, such as
replacing a person in the rotation. If applying `on_call_schedule` then
fails, MMHotline contacts
the backup, then everyone available, in the deactivated
person's place, as for anyone unavailable.

Removal marks the member row with `removed_at` and marks personal contact
methods removed. Removal cancels queued delivery work through
the existing delivery cancellation operation and deletes the
person's sign-in links and cookies. Removed people
cannot sign in, act through notification buttons, or receive new assignments.

Outgoing notification requests already sent keep their delivery records.
MMHotline cancels open Pushover emergencies for the removed person.
A phone call already placed cannot be recalled.

The member row remains because history, deliveries, and claims reference
stable member IDs. Incident audit entries also retain recorded actor names.
History can therefore show the removed person's name without copying a new
name field into every kind of historical record. Adding the removed
person's sign-in email address back to `people` in the settings file creates
a new member ID. Uniqueness applies to active memberships. Historical names
remain attached to the old member ID.

## People and team pages

People at `/people` appears in the main navigation. Everyone sees each active
member's name, sign-in email address, and role. The page marks the member
record of the signed-in viewer. The page has no forms: it says
"Set in /etc/mmhotline/mmhotline_settings.yml, in
people", with the settings file's first line when the line is a comment.
The comment can explain how the team changes the settings file.

### What can wake each person

The on-call page lists each member's name, role, and current availability:
On call, Backup, Available, or Unavailable until {time}.

The page also lists what can wake each member at night: personal Pushover
notification and automated voice call contact methods that are not failing.
Examples are “Woken by Pushover notification and automated voice call” and
“Nothing set up can wake them at night”. Availability and the ability of a
contact method to wake a person are separate facts. An unavailable person
with a working phone is still skipped for an SOS.

Pushover with iOS Critical Alerts is the default for waking a responder.

Each person adds a personal Pushover user key, or a phone number, under
Contact methods. The displayed statement about waking a person records the
contact method, not proof that the phone made a sound. The person must check
the emergency test on the phone.

## Local people command

The [command reference](../../reference/mmhotline-command.md#mmhotline-people-list) owns the people
subcommands, `list` and `create-sign-in-link`, with their flags, JSON fields,
and examples. Every command group uses the command Unix socket and the same
application operations as the pages.

The command socket is mode 0600 from creation. The server checks the
connection's peer user ID: only root on Linux, including Linux development
systems, and the service's own user on non-Linux development systems.
File permissions alone are insufficient because the service's user owns the
socket. A refused peer gets “Run the mmhotline command with sudo.”

A trusted command principal is the application's identity for a command
from an accepted peer. The command identity records `SUDO_USER`, or
`root` when absent, and acts as an administrator. The server trusts the
recorded user only after the kernel identifies the peer.

The trusted command can also edit any person's contact methods. Authority to
edit any person's contact methods comes from root access to the team's
server, not membership or a browser role.
Page permissions remain unchanged. Where an operation records an author or
sends a test message naming the initiator, the author is
“the mmhotline command, run by alice”, using `SUDO_USER` or `root`.
The command author has no member ID. For settings-file changes, an
operation's author record or test message names “the settings file
/etc/mmhotline/mmhotline_settings.yml”, also without a member ID.

## Contact method tests

Contact methods can receive alerts as soon as the contact methods are
saved. Adding a contact method or changing a contact method's address queues
a test message in the save transaction. Each contact method's card shows the contact
method's last test result.
[Notifications](notifications.md#test-and-manage-a-contact-method)
owns the workflow and test limits.

Only the team's own people and trusted local commands add addresses on the
team's server, so verification protects nobody. A test message catches a wrong address when a person reads the test
message.

## Acceptance evidence

Check Cloudflare Access token verification: signature before claims, issuer,
audience, the clock allowance, the email claim, key rotation, the fetch
limits, and unavailable keys. Check sign-in links: single use, expiry, the
cookie lifetime, removal ending links and cookies, and the Access header
winning over a cookie. Check missing and unknown identity, case-insensitive
membership, next-request removal, member restrictions, duplicate sign-in
email addresses, the claimed-incident and taken-on-call protections of a
deactivation, retained historical names, and CSRF. Apply people through the
settings file, and exercise People and the local command through their public
interfaces. Check private socket creation and kernel peer
credentials separately from `cloudflared` and Cloudflare Access on a real
server. [Testing](../testing.md) owns the test commands.
