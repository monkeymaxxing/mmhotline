# Status page and MMHotline-down alarm

Two optional parts of MMHotline run in the team's own Cloudflare account,
not on the MMHotline server, because each part must keep working while the
MMHotline server is down:

- the **status page**, a public page where the team tells its own customers
  that it knows about a problem, what it is doing, and when it is over;
- the **MMHotline-down alarm**, which wakes the person on call when MMHotline's check-ins
  stop reaching the MMHotline-down alarm. For example, the MMHotline server dies, or the
  server's location goes down or is cut off from the internet.

Without a Cloudflare account, the rest of MMHotline works the same.
Nothing in the status page or MMHotline-down alarm depends on a server that anyone
other than the team runs.
An SOS is the urgent alert that wakes the person on call, even at night;
to claim an incident is to say "I'm on it", which stops MMHotline
contacting the next person.

## The status page

### What customers see

The page lives at a host name the team chooses, such as
`https://status.example.com/`. The public status page's title and heading
are the settings file's `page_title` exactly as written, such as
"Example Inc. status". MMHotline adds no word, so the page reads as the team
named it. The public status page shows:

- each open notice, newest first, in a red box marked "Problem now": "We
  are working on a problem." The box gives the notice's start time and
  latest update. All updates appear below,
  newest first, each with the message text and posting time;
- "Everything is working." in a green box when no notice is open, with when
  the page was last updated;
- the 5 most recently resolved notices, under "Recently resolved", each with
  when the notice was open and the notice's updates;
- a last line that says the times are in UTC and links `status.json` for
  programs.

Times are absolute and in UTC, never "5 minutes ago": the files change only
at the next post, so a relative time would be wrong a minute later. A
notice's first line gives the date, such as "26 October 2026, 08:09 UTC";
the notice's updates give the time of day, with the day when the update date
differs from the notice's first date, such as "27 Oct 02:00". Messages are plain text: the page
shows messages as written, with their line breaks, and never as HTML.

The page looks like MMHotline's own pages, in light and dark. Cloudflare
serves the public status page as a static file, outside MMHotline's HTTP
server and Content Security Policy. The public status page must remain
styled while MMHotline is down, so the page cannot depend on loading
MMHotline's stylesheet. The public status page's single style element
embeds `internal/statuspage/status_page.css`.

The rule set in `internal/statuspage/status_page.css` is copied as written
from `internal/app/mmhotline.css` for the public status page. A test fails
when a rule in `internal/statuspage/status_page.css` differs from
`internal/app/mmhotline.css`.

The page has no list of components such as "API" and "Dashboard", no uptime
bars, and no email or text messages to subscribers. The public status page
shows only what a person posted. MMHotline's alerts never appear on the
public status page automatically, because an alert can be a false alarm.
The team decides what customers read.

### Two files in an R2 bucket

The page is two files in one R2 bucket, Cloudflare's object storage, in the
team's account:

- `index.html`, the page for people;
- `status.json`, the notices for programs and coding agents.

No program runs for the page. Cloudflare serves the bucket at the team's host
name, so the page stays readable while the MMHotline server, or the team's
whole hosting provider, is down. Cloudflare's free plan includes 10 GB of
storage, 1 million writes, and 10 million reads a month (checked 2026-10-26).

`status.json` holds the whole notice history and is the page's only state.
MMHotline's database keeps no copy of the notices. A post therefore works the
same from the MMHotline server and from a responder's laptop:

1. read `status.json` and its ETag, the version tag R2 gives each upload.
   The read requests `status.json` without compression
   (`Accept-Encoding: identity`) so R2 returns an ETag usable in `If-Match`;
2. apply one change: a new notice, an update to an open notice, or resolving
   a notice;
3. upload `status.json` on the condition that its ETag is unchanged
   (`If-Match`), or, for the first upload, that it does not exist yet
   (`If-None-Match: *`);
4. build `index.html` from the new `status.json` and upload it;
5. read `status.json` again. When the `status.json` ETag differs from the
   ETag saved by this post, rebuild `index.html` from the newer `status.json`
   and upload `index.html` again. Repeat the read, ETag comparison, rebuild,
   and upload, up to 3 uploads of `index.html` in all.

The uncompressed read in step 1 avoids an R2 ETag failure. For a read that
accepts gzip, R2 returns a weak ETag, `W/"…"`, then refuses the weak ETag in
`If-Match`. In the real proof (2026-10-27), every post after the first failed
because R2 refused the weak ETag.

When another post changed `status.json` between this post's read and its
upload, R2 refuses the upload with HTTP 412, and the post starts again from
step 1 with the newer `status.json`, up to 3 attempts in total. Two
responders who post at the same moment therefore both appear on the page;
neither post overwrites the concurrent post.

`index.html` is uploaded without a condition, so another post's newer page
can arrive before the current post's older page. For example, Barbara resolves the
last notice and saves `status.json`; Bob posts a new notice and uploads his
page; then Barbara's page arrives, saying "Everything is working." Step 5
replaces Barbara's outdated page: Barbara's post reads Bob's `status.json`
and uploads a page built from Bob's notices. The post that uploads
`index.html` last reads `status.json` after the upload. When the final read
confirms `status.json` has not changed, the last successful `index.html`
upload shows the latest notices. When `index.html` fails to upload after
`status.json` succeeded, or `status.json` still changed after the third
upload, the post is saved, and the poster is told that the page may show the
saved notice only after the next post.

`status.json` and `index.html` are uploaded with
`Cache-Control: public, max-age=60`, so a cache serving `status.json` or
`index.html` shows a post within about a minute.

`status.json`:

```json
{
  "page_title": "Example Inc.",
  "published_at": "2026-10-26T08:09:30Z",
  "notices": [
    {
      "notice_id": 1,
      "opened_at": "2026-10-26T08:00:00Z",
      "resolved_at": null,
      "updates": [
        {"message": "We are looking into a problem.", "posted_at": "2026-10-26T08:00:00Z"},
        {"message": "Sign-in fails for some customers. We are moving the database to its replica.", "posted_at": "2026-10-26T08:09:30Z"}
      ]
    }
  ]
}
```

- `notices` is newest first. A new notice's `notice_id` is one more than the
  highest so far.
- `resolved_at` is `null` while a notice is open.
- `updates` is oldest first. A resolved notice takes no more updates; a new
  problem gets a new notice.
- Times are RFC 3339 in UTC, to the second.

The Go package `internal/statuspage` is the one place that reads, changes,
and uploads the files. MMHotline's service and the command on a laptop call
`internal/statuspage`.

### Posting

Any member may post, from three places:

- **The incident page.** When the status page is set up, an SOS incident's
  page has a "Post to the status page" button. One click publishes the
  generic notice "We are looking into a problem." and opens MMHotline's
  editing page, titled "Status page", where the responder adds their own
  words. While the notice an incident posted is open, the incident page
  shows that notice and a link to update or resolve it instead of the
  button, and a second post from that incident is refused. Reason: a person
  under stress presses twice, and customers would read two identical
  notices. The link lives in MMHotline's database, table
  `status_page_incident_notices` (database format 2), never in the public
  `status.json`. The row is reserved before the post is published, so two
  quick presses publish once. MMHotline marks the row resolved whenever it
  reads or publishes a page that no longer shows the notice open, which
  covers a notice resolved from a laptop. A responder may still post one
  notice for several incidents from the Status page page.
- **The Status page page** is MMHotline's editing page for public notices.
  Each open notice has a form to add an update and a form to resolve the
  notice, with an optional closing message. **Resolve this notice** opens
  the closing form, so posting an update remains the main action.
  A separate form posts a new notice. Recently resolved notices stay in a
  closed section because resolved notices need no immediate action.
  Notice times on the editing page use the team's time zone. The public
  page stays in UTC. One closed **Where status page settings are set** section holds the
  status page's settings path and editing instructions.
- **The command** `mmhotline status-page post` and `resolve`, on the
  MMHotline server. The command sends a request to the running service's
  local management API through a Unix socket, called the
  [command socket](operation.md#listeners), as other management commands do.

A message has 1 to 5,000 characters after spaces and line breaks at its start
and end are removed: long enough for any status update, short enough to read
on a phone.

### Posting while the MMHotline server is down

The same `mmhotline` program posts from a responder's laptop with no
MMHotline service; each release has
[macOS archives](development.md#release-archives) for a Mac. Posting from a
laptop needs only three environment variables:

| Environment variable | Holds |
| --- | --- |
| `MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL` | The bucket's S3 API address, such as `https://ACCOUNT_ID.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11`; `mmhotline status-page show` on the server prints it |
| `MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID` | The Access Key ID of an R2 API token |
| `MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY` | The R2 API token's Secret Access Key |

When `MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL` is set, `show`, `post`, and
`resolve` go straight to the bucket, through the same `internal/statuspage`
code. A laptop never starts a page: when `status.json` is missing, the post
is refused, so a mistyped bucket address cannot create a second page.

The R2 token on a laptop can be limited to Object Read & Write on the status
page's bucket, so a program that copies the key from the laptop exposes only
the status page. The command
needs only the program a responder already has. An alternative phone form
would run in the MMHotline-down alarm's Cloudflare Worker, the Cloudflare-hosted program
that watches MMHotline's check-ins. A phone form behind Cloudflare Access
would require extra Worker code and Access setup.

### Why R2's S3 API, and its access key

Cloudflare's v4 API creates the bucket and connects the host name with an
API token. MMHotline writes the files through R2's S3-compatible API for two
reasons. First, an S3 key can be limited to the objects of the page's
bucket (Workers R2 Storage Bucket Item Write), so a key copied from a laptop
exposes only the status page. Second, the S3 API's conditional writes (`If-Match`, and
`If-None-Match: *` for a new `status.json`) keep two posts from overwriting
each other. Any Cloudflare API token with R2 permissions is also an S3 key:
the access key ID is the token's `id`, and the secret access key is the
SHA-256 of the token's value, in hexadecimal
([R2 API tokens](https://developers.cloudflare.com/r2/api/tokens/)). On
2026-11-11, keys made this way from a user token and from an
account-owned token limited to one bucket wrote a test bucket, and both
conditional writes answered `412` as expected.
MMHotline signs its S3 requests with AWS
Signature Version 4, which a small signer in `internal/statuspage` makes.
The signer is checked against the example in AWS's documentation; MMHotline
adds no dependency for it.

### Set-up

The status page is set up in two steps: once in Cloudflare, from a laptop,
and then in the settings file on the MMHotline server.

`mmhotline status-page set-up` runs on a laptop, or any computer with the
`mmhotline` program, with no MMHotline service and no command socket (CA8):

```sh
mmhotline status-page set-up --cloudflare-account-id ID --host-name HOST --cloudflare-api-token-file FILE
```

`mmhotline status-page set-up` takes the team's Cloudflare account ID, the
host name, and the person's own Cloudflare API token. The command reads the
token from a file only the file's owner can read, or from standard input
with `-`. The `mmhotline status-page set-up` command:

1. finds the zone in the account that holds the host name, such as
   `example.com` for `status.example.com`. The host name's zone must be on
   Cloudflare in the account passed to `mmhotline status-page set-up`;
   a team that keeps its main domain's DNS
   elsewhere can register a separate status domain on Cloudflare, which a
   DNS fault on the main domain then cannot hide;
2. creates the bucket, named after the host name: `mmhotline-status-`, the
   host name with hyphens for its dots, and the first 8 hexadecimal
   characters of the SHA-256 of the lowercase host name, such as
   `mmhotline-status-status-example-com-18a8ea11` for `status.example.com`.
   The same host name always gives the same bucket. The digest gives host
   names that differ only in punctuation, such as `status.eu.example.com`
   and `status-eu.example.com`, different buckets, so two teams' pages never
   share a bucket within an account. R2 allows 63 characters, so the middle
   part of a long host name is cut short;
3. connects the host name to the bucket, which makes the bucket public at
   that host name only;
4. adds one URL rewrite rule to the zone that serves `/index.html` when a
   reader opens the bare address `/`, because R2 does not serve an index
   file by itself. The rule's expression is
   `(http.host eq "status.example.com" and http.request.uri.path eq "/")`.
   The zone's other rewrite rules stay unchanged.

Each setup step first checks whether the step is done, so running set-up again for the
same host name changes only what is missing. Set-up keeps no record between
runs: another host name gets its own bucket, named after it, with no
`status.json` yet, and the old bucket and host name stay until someone
removes the old bucket and host name in the dashboard.

Set-up also runs while the zone is still pending at Cloudflare, before the
domain's name servers point to Cloudflare. Set-up finds a pending zone as
set-up finds an active zone. If Cloudflare refuses a step for a pending zone,
set-up stops with Cloudflare's reason. Resources already made by set-up in
Cloudflare stay, and running set-up again once the zone is active does only the rest.
Readers reach the page only once the zone is active, because until then the
domain's DNS does not answer from Cloudflare.

The setup API token is used only for the setup calls and never stored. Set-up prints the bucket's S3 API address and the next step:

```text
The status page is set up in Cloudflare account 0123456789abcdef0123456789abcdef: the R2 bucket mmhotline-status-status-example-com-18a8ea11, the host name status.example.com connected to it, and the rule that serves index.html at https://status.example.com/.
r2_bucket_url: https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status-status-example-com-18a8ea11
Next step: on Cloudflare's site, open R2, then Manage API tokens, and create an API token with the permission Object Read & Write for the bucket mmhotline-status-status-example-com-18a8ea11 only. Put r2_bucket_url above and the token's Access Key ID and Secret Access Key in the settings file's status_page section, as r2_bucket_url, r2_access_key_id, and r2_secret_access_key (with a deploy tool, in its vault), then apply the file, with your deploy tool or with sudo mmhotline settings apply on the MMHotline server. The first apply publishes the page.
```

Posting needs only the R2 access key, so MMHotline keeps the narrower
credential. Set-up cannot create the R2 object-access token because token
creation needs permission to create any token in the account. MMHotline
never asks for permission to create tokens.

Set-up uploads no first page. Writing an object needs the bucket's R2
access key, which the person creates only after set-up. The settings file's
`status_page` section then carries `page_title`, `host_name`,
`r2_bucket_url`, and the key's two values, and the first
`sudo mmhotline settings apply` with that section posts the current page:
the apply writes `status.json` and `index.html` with the file's `page_title`, and
creates `status.json` when the file is missing. The setup is saved only when
publishing the current page worked, so a key that cannot write the bucket
is refused and never saved
([the settings file](settings-file.md#the-status-page)).
Leaving out the settings file's `status_page` section removes the status
page setup from MMHotline.
The bucket and public page stay in Cloudflare.

The rewrite rule comes from community guides. The rule's effect on a real
zone is part of the real proof, which needs a real account.

## The MMHotline-down alarm

### How the MMHotline-down alarm works

The MMHotline-down alarm is a Cloudflare Worker, a small program that Cloudflare runs,
with one Durable Object: a Cloudflare object with its own SQLite storage and
one timer, which Cloudflare calls an alarm.

1. Every 60 seconds, MMHotline sends the Worker a check-in with its held
   messages. A held message is a ready-made HTTPS request that MMHotline's
   delivery code built, with a delay, such as "send this Pushover
   notification after 10 minutes of silence". MMHotline also checks in
   within 5 seconds when the held messages change, such as at a handoff.
2. Each accepted check-in replaces the stored held messages and moves the
   alarm to the first delay after that check-in.
3. When the check-ins stop, the alarm fires, and the Worker sends each held
   message when its delay has passed.
4. When MMHotline comes back, the answer to the first accepted check-in lists
   what the Worker sent. MMHotline then sends the all-clear through the
   normal delivery path.

For example, MMHotline checks in at 03:00:00 with a Pushover notification
to Barbara after 600 seconds and one to Bob after 1,200 seconds, and the
server dies at 03:00:30. The alarm fires at 03:10:00 and sends Barbara's
notification, then at 03:20:00 Bob's.

The Worker answers at its workers.dev address, so the MMHotline-down alarm needs no
domain. Cloudflare's free plan (checked 2026-10-26) allows 100,000 Worker
requests a day; a check-in every minute is 1,440. Durable Objects allow
100,000 written rows a day; an unchanged check-in writes one.

### The held messages

MMHotline builds the held messages for whoever is on call when MMHotline
builds the check-in:

- the primary's waking contact methods after the settings file's
  `wake_the_person_on_call_after_minutes_without_a_check_in`, such as 10 minutes;
- the backup's after `wake_the_backup_after_minutes_without_a_check_in`, such as 20 minutes. The backup is whoever an
  unclaimed SOS escalates to: the named backup, or, when the backup option is
  `everyone_available`, everyone else available. The MMHotline-down alarm
  fires only when MMHotline cannot escalate, so it escalates the same way an
  SOS does. Otherwise a team on the default backup option would have only
  the person on call between a stopped MMHotline and an unnoticed outage
  (decided 2026-11-13). The alarm cannot see whether the person on call
  answered, because the answer goes to Pushover or the phone, so the backup
  step wakes everyone else available even when the person on call is already
  at work. A team that wants fewer people woken names one backup or raises
  the backup's wait;
- when nobody is scheduled, everyone available after the person on call's
  wait, as for an SOS. When nobody is available, every member.

The MMHotline-down alarm uses MMHotline's SOS recipient selection, called coverage, to
follow rotations, schedule handoffs, taking on-call, and personal
unavailability. The MMHotline-down alarm has no settings of its own.

A waking contact method is a Pushover notification or an automated voice
call subscribed to SOS. MMHotline uses only Pushover and automated voice
calls to wake a sleeping person. Email cannot be held as an HTTPS request,
because MMHotline sends email through the team's own mail server using
SMTP. MMHotline treats chat messages and outgoing webhooks as ordinary
notifications, rather than wake-up methods. A webhook's signature also
needs the time of sending.

Each contact method type builds its held request with the same code that
builds its delivery requests:

- **Pushover:** an emergency-priority notification that repeats every 30
  seconds until acknowledged, for 25 minutes, titled "MMHotline is not
  checking in", saying "MMHotline for Operations stopped checking in with
  the MMHotline-down alarm. MMHotline may be down or cut off from the internet, so MMHotline cannot send urgent alerts. Check the MMHotline
  server.", with a link to
  MMHotline;
- **Automated voice call:** a call through the team's Twilio account that
  gives the warning above.

The message names no time, so a held message stays the same from one
check-in to the next. Each held message also carries a test request for
"Send a test now". The test request uses the held message's notification or
call type, with text identifying the notification or call as a test.

A test calls and notifies everyone on the list at once, through the team's
Twilio and Pushover accounts. So "Send a test now" on the dashboard starts a
test at most once every 10 minutes, and at most 6 times in 24 hours, across
the team. Otherwise any member could make everyone's phone ring again and
again, at the team's cost. Ten minutes is enough to fix a contact method and
test again, and 6 a day covers a setup session. A refused test says when the
next one can start, such as "Each test calls everyone on the MMHotline-down
alarm's list, so a test can start once every 10 minutes. Try again in 4
minutes." `sudo mmhotline down-alarm send-test` has no limit, because root
already holds the accounts' credentials. MMHotline counts the tests in
memory, so a restart starts the count again.

A test of a Pushover emergency is proved from Pushover's receipt. The Worker
reads the start of each accepted test's answer, at most 4,096 characters, and
returns the `receipt` field as `pushover_receipt` when it is 30 letters and
digits, as MMHotline's own check of a receipt (`pushoverKeyPattern`) requires;
a held message with no such receipt gets no field, so a voice call's test is
unchanged. MMHotline then reads the receipt with the Pushover application
token it holds, which records that Pushover accepted the notification, and
cancels the emergency at once, so it does not repeat for the test's 10 minutes.
Each person's result says Pushover accepted the notification and MMHotline
cancelled its repeats, or the part that failed: `no_receipt`,
`receipt_refused_by_pushover`, or `cancel_failed`. Each failure makes the
command exit 1. Reason for cancelling: a repeating test would keep a person
busy with nothing to do. The command and the dashboard say, in capitals and
before anything is sent, that the phone may sound once more: the emergency
sounds when it arrives, before MMHotline cancels it.

During team mute, which stops Pushover and phone calls, each delay starts
when team mute ends, rounded up to the next minute.
The MMHotline-down alarm therefore wakes nobody during a mute, and still wakes the
person on call when MMHotline died during a mute.

A held message's ID names the primary or backup notification step and the
contact method, such as `primary-contact-method-0f3a...`, so the sends lead
back to a person.

### The check-in

```http
POST /check-in HTTP/1.1
Host: mmhotline-down-alarm-ops-example-com-7db8a22e.example-team.workers.dev
Authorization: Bearer CHECK_IN_TOKEN
Content-Type: application/json

{"held_messages":[{"held_message_id":"primary-contact-method-0f3a5c...","delay_seconds":600,
  "request":{"method":"POST","url":"https://api.pushover.net/1/messages.json",
    "headers":{"Content-Type":"application/x-www-form-urlencoded"},"body":"token=...&user=...&priority=2&..."},
  "test_request":{"method":"POST","url":"https://api.pushover.net/1/messages.json","headers":{...},"body":"..."}}]}
```

The check-in token is 64 random hexadecimal characters that set-up makes
and sets as the Worker's secret `MMHOTLINE_LIFELINE_CHECK_IN_TOKEN`. The
secret keeps the feature's earlier name, the Lifeline: an upgrade keeps the
secrets an installed Worker has, so a new name would leave that Worker
without its token. The Worker compares the request's bearer token with the stored check-in token
in constant time before a request reaches the Durable Object. A wrong token
gets 401, a Worker without its secret 503, and every other path 404.

The answer:

```json
{
  "check_in_accepted_at": "2026-10-27T03:30:00Z",
  "held_messages_stored": 2,
  "sends": [
    {"send_id": 1, "held_message_id": "primary-contact-method-0f3a5c...",
     "silence_started_at": "2026-10-27T03:00:00Z", "due_at": "2026-10-27T03:10:00Z",
     "attempts": 1, "send_state": "finished", "send_outcome": "delivered",
     "http_status_code": 200, "finished_at": "2026-10-27T03:10:01Z"}
  ]
}
```

`sends` lists the sends of the last 7 days, at most 200, newest first.
`send_id` grows with each send. MMHotline keeps the highest
`send_id` MMHotline has handled. The next check-in lists sends from a lost
response while the sends remain within the 7-day and 200-send retention limits.

**Refused before storage.** When a check-in arrives, the Durable Object
checks every held message against the following rules. If any field breaks
a rule, the Durable Object refuses the entire check-in with HTTP 400.
Refusal leaves the previously stored held messages and scheduled alarm
unchanged:

- `held_message_id` has 1 to 128 lowercase letters, digits, and hyphens,
  unique in the check-in;
- `delay_seconds` is a whole number from 180 to 2,147,483,647. The lower
  bound is the shortest wake time, 3 minutes, three missed check-ins, so one
  lost check-in or a short network fault wakes nobody; the upper bound is the
  largest 32-bit integer;
- each request's `method` is `GET`, `POST`, `PUT`, or `PATCH`, and its `url`
  is `https://` with no user name, password, or `#` part;
- header names are HTTP tokens, never `Host`, `Content-Length`,
  `Transfer-Encoding`, `Connection`, `Keep-Alive`, `Proxy-Authorization`,
  `Proxy-Connection`, `TE`, `Trailer`, or `Upgrade`, which the Cloudflare
  Workers runtime sets;
- a header value has no control character other than a tab, as RFC 9110
  requires. A value with a line break would otherwise make the send fail at
  the moment the alert matters, after MMHotline's check-ins stopped reaching
  the Worker;
- one held message has at most 16 KiB in JSON, and a check-in at most
  1 MiB.

The answer names the field, such as `held_messages[1].request.headers`, and
never repeats a value.

### When the Worker sends

- At each alarm, the Worker starts a send for every held message whose
  delay has passed and has no send since the last accepted check-in.
  The Worker then attempts every send due, at most 40 at once. The Workers
  free plan allows 50 outgoing requests for each run. The Worker schedules
  another alarm immediately for the remaining due sends. Sends never
  attempted go first, then retries ordered by earliest scheduled attempt
  time. Unanswered sends therefore cannot occupy all 40 places while an
  unattempted send waits: with 41 held messages, the 41st goes in the second
  run.
- A send is saved, with its next attempt 30 seconds later, before its
  request goes out. When the run ends before the Worker records the outcome, such
  as when Cloudflare moves the object, the send is due again and goes again.
  A duplicate alert is acceptable; a dropped one is not. Saving a send before
  delivery allows repeated attempts after an interrupted run. Cloudflare's
  alarm also allows repeated execution: Cloudflare runs the alarm at least
  once and retries up to 6 times.
- A `2xx` answer is `delivered`. No answer, `429`, or `5xx` is tried again
  30 seconds after the attempt ended, until 10 minutes after the due time,
  then `not_delivered_within_10_minutes`. Any other answer is
  `refused_by_service` and is not tried again. The Worker never follows a
  redirect and waits 30 seconds for an answer, so a request that gets no
  answer is tried again a minute after the request started.
- Cloudflare's storage takes at most 128 keys in one write or delete, so
  the Worker writes a larger set in parts: a check-in with hundreds of
  held messages, or an alarm that starts hundreds of sends, still works.
  The parts go in an order that is safe when a write stops part way: an
  alarm saves the next send ID before the new sends, so no send ID is used
  twice, and a check-in saves its own record after its held messages, so
  the earlier check-in stays whole.
- A check-in that arrives while sends are in progress stops the sends
  (`stopped_by_check_in`): MMHotline runs again and reaches people itself.
  A request already out that then succeeds is still recorded as
  `delivered`.

The Worker's functions receive the Durable Object's storage operations,
the current time or a clock function, and a function that sends an HTTPS
request. The storage operations read, write, list, and delete records, and
set or delete the alarm. `mmhotline_down_alarm_worker_test.mjs` supplies fake storage
and alarm, a clock, and delivery functions to test the Worker under
`node --test`. The Worker source file, `internal/downalarm/mmhotline_down_alarm_worker.js`,
needs no build step and no npm package.

### The all-clear and failing check-ins

When an accepted check-in's answer lists sends that MMHotline has not
handled, MMHotline records one info alert, "MMHotline runs again: it was
down, and the MMHotline-down alarm woke you", for the people whose contact methods the
sends went to.
The all-clear lists each send, its due time, and its outcome. MMHotline
delivers the all-clear through each recipient's contact methods subscribed
to info, rather than SOS
([subscriptions](notifications.md#contact-methods-and-subscriptions)).
A Pushover contact method gets info alerts by default. The alert and the
new highest `send_id` are saved in one transaction.

After set-up finds no earlier MMHotline-down alarm setup in MMHotline's database, the
first accepted check-in records every send the Worker lists as handled.
MMHotline sends no all-clear for the first check-in response. The Worker
keeps send records for 7 days, so a new local setup can receive records
from an earlier setup. For example, leaving out the settings file's
`mmhotline_down_alarm` section and then adding the section back replaces the check-in
token. A new database also has no earlier local setup.

Send records retained from the earlier setup were already announced, or
belong to a database that no longer runs. Suppressing the first all-clear
prevents MMHotline from announcing the earlier setup's sends again.

When check-ins fail for 10 minutes, MMHotline raises one warn alert,
"MMHotline cannot reach the MMHotline-down alarm", with the reason. Without check-ins,
the Worker cannot tell a failing check-in from an outage: its alarm fires
from the last accepted check-in and sends each held message once. After
that, the Worker has nothing more to send, so nobody is woken again if
MMHotline really goes down before check-ins work again.
When a check-in is accepted again, an info alert
says so: "MMHotline reaches the MMHotline-down alarm again: if MMHotline goes down, the
person on call is woken". MMHotline logs the first failure and the
recovery, not every failed minute. If Cloudflare and the MMHotline server
are both down at the same time, nobody is woken; both outages must overlap,
which is rare.

### When MMHotline stops checking in on purpose

A check-in tells the MMHotline-down alarm that MMHotline sends its alerts. The
background loop sends them: it escalates incidents and makes the delivery
attempts. The loop ends a round every second. MMHotline can still answer
alert senders while the loop fails on every visit or hangs on one send.
Nobody would then be woken, and check-ins alone would keep the MMHotline-down alarm
quiet.

So MMHotline stops checking in when the background loop has not finished a
round without a failed visit for 2 minutes. The MMHotline-down alarm then wakes the
person on call after the wake time, as for an outage. MMHotline logs the
reason once, and the dashboard shows it as a failing check-in:
"MMHotline stopped checking in on purpose: its background work, which sends
alerts, has not finished a round since ...". After 10 minutes, the warn
alert about failing check-ins follows, with the same reason. The next
check-in after a successful round is accepted again, and the all-clear
follows as after an outage.

The 2 minutes are well above a normal round: even a visit whose every
sending service call waits for its time limit ends within a minute. The
2 minutes are also below the shortest wake time, 3 minutes. With a
check-in every minute, the MMHotline-down alarm wakes the person on call at most
3 minutes plus the wake time after the loop stops.

Times of accepted check-ins stay in MMHotline's memory, not its database.
Writing a timestamp every minute would make continuous backup upload a
database change every minute indefinitely.
After a restart, the dashboard says that no check-in happened yet
until the first check-in, a few seconds later.

### What the team sees

The dashboard's MMHotline-down alarm panel, while the MMHotline-down alarm is set up, is tagged
"Checking in" in green, "Not checking in" in amber while check-ins fail, or
"Starting" before the first check-in. The dashboard's MMHotline-down alarm panel shows when the last check-in was accepted, using MMHotline's
own clock, such as "MMHotline last checked in just now". While check-ins
fail, the MMHotline-down alarm panel shows when failures began, and that the MMHotline-down alarm
acts as if MMHotline were down: it sends its urgent alerts once, then
nothing more, even if MMHotline really goes down later. The check-in failure reason is folded under
"Why".

The dashboard's MMHotline-down alarm panel lists whom the MMHotline-down alarm would wake if
MMHotline's check-ins stopped reaching the Worker, and after how many
minutes. If no recipient would be woken, the MMHotline-down alarm panel says so in
amber.

When a person on call, or a named backup, would get no held message, the
administrators get one warn alert: "The MMHotline-down alarm cannot wake
everyone on call". The alert names each such person and the reason. One
reason is that the person has no Pushover notification or automated voice
call subscribed to SOS. Another is that a contact method cannot be held,
such as a Pushover notification with an incomplete user key, with the
contact method type's own message. When the MMHotline-down alarm would hold no
message at all, the alert names everyone it should wake. MMHotline sends the alert again only when the list of
people changes. The list it last sent stays in memory, so a restart may
send the alert once more. The MMHotline-down alarm panel also shows what the MMHotline-down alarm last sent and a
"Send a test now" button with "Each person above gets a test alert now."
The settings file's path is folded under "Where the MMHotline-down alarm is set up". The panel has no
paragraph that defines the MMHotline-down alarm: "If MMHotline goes quiet, the MMHotline-down alarm
wakes:" says what it is for where it matters. The button checks in first,
then asks the Worker to send each held message's test request now. Tests
are not retried and not listed as sends. On the server,
`mmhotline down-alarm show` displays the MMHotline-down alarm panel's information without
checking in or sending tests. The `send-test` command checks in, then asks
the Worker to send test requests, as the dashboard button does.

### Set-up

The settings file's `mmhotline_down_alarm` section holds the team's Cloudflare account
ID and an account API token limited to Account › Workers Scripts › Edit.
When the section is new or changed, `sudo mmhotline settings apply` sets the
MMHotline-down alarm up ([the settings file](settings-file.md#the-mmhotline-down-alarm)):

1. reads the account's workers.dev subdomain. An account that has none yet
   gets a workers.dev subdomain when someone opens Workers & Pages in the
   dashboard once;
   set-up says so;
2. uploads the Worker, `mmhotline_down_alarm_worker.js`, embedded in the `mmhotline`
   program, as one ES module, with a binding `MMHOTLINE_DOWN_ALARM` to its one
   SQLite-backed Durable Object class `Lifeline`, the feature's earlier
   name, which installed Workers keep. The first upload creates
   the class with the migration tag `v1`; a Worker already at `v1` gets no
   migration, as Cloudflare's `wrangler` tool does. Every upload keeps the
   Worker's secrets (`keep_bindings`);
3. sets the check-in token as the Worker's secret;
4. turns on the Worker's workers.dev address, with the Worker's preview
   addresses turned off (`previews_enabled: false`);
5. saves the set-up and checks in at once. A new workers.dev address
   answers with Cloudflare's own 404 until the Worker is live there, which
   took 12 seconds in the
   [real-account MMHotline-down alarm verification](../validation.md#the-mmhotline-down-alarm-in-a-real-cloudflare-account)
   (2026-10-27). Set-up therefore tries the first check-in up to 30 times, a
   second apart. If the first check-in still fails after 30 attempts, the
   `mmhotline_down_alarm` part fails with the reason. MMHotline restores the
   previous MMHotline-down alarm setup, or no setup for a first set-up, so the earlier
   setting stays.

The Worker is named after the host name of MMHotline's web address, as
the status page's bucket is: `mmhotline-down-alarm-`, the host name with
hyphens for its dots, and the first 8 hexadecimal characters of the SHA-256
of the lowercase host name, such as
`mmhotline-down-alarm-ops-example-com-7db8a22e` for `https://ops.example.com`.
A set-up after a lost database replaces the Worker previously used for
MMHotline's web host name, instead of leaving a Worker behind that would
wake people. A second team's MMHotline in the first team's Cloudflare
account gets a separate Worker, even when the web host names differ only
in punctuation, such as `ops-eu.example.com` beside `ops.eu.example.com`.
A Worker with a workers.dev address has a name of at most 63 characters,
so the middle part of a long host name is cut short.

Cloudflare allows only 54 characters while a Worker's preview addresses
are on, and a request that turns on the workers.dev address without
`previews_enabled` turns previews on. MMHotline never uses a preview
address, so set-up turns previews off in the same request. Before this
change, a 61-character name from a real host name was refused with
error 100315 (2026-11-04). Shortening the name was the other choice, but a
set-up again with a changed name would leave the earlier Worker behind,
still able to wake people; with previews off, the name stays the same.

A changed `mmhotline_down_alarm` section that keeps the existing MMHotline-down alarm setup's
Cloudflare account also keeps the Worker's name and check-in token.
A failed set-up therefore never locks out the running MMHotline.
MMHotline refuses to move the MMHotline-down alarm while the MMHotline-down alarm is configured in
the existing Cloudflare account. Apply the settings file without the
`mmhotline_down_alarm` section first, then add the section back with the destination
account.

MMHotline keeps the API token in its database, like the sending services'
credentials, so an upgraded MMHotline uploads its new Worker by itself:
when the Worker's digest differs from the one last uploaded, the check-in
loop uploads the Worker, and tries again an hour later after a failure. When the
uploads have failed for a day, the administrators get one warn alert, "MMHotline
cannot update the MMHotline-down alarm, which still runs an older version", with the
reason and the fix, and an info alert once an upload works again. Until
then the older Worker keeps working, so a day of Cloudflare trouble passes
quietly. The usual cause is an API token that Cloudflare no longer accepts.
For example, Cloudflare invalidates a person's API tokens when the person
is removed from the account, so the MMHotline-down alarm guide asks for an account API
token.

When the settings file leaves the `mmhotline_down_alarm` section out, `settings apply`
first checks in with no held messages, then deletes the local MMHotline-down alarm
setup from MMHotline's database. Without the empty check-in, the Worker would wake the
person on call once the person on call's wait ran out after the check-ins
stopped. If the empty
check-in fails, the `mmhotline_down_alarm` part fails and the local setup
stays, unless the Worker is already gone (404). Apply again, or delete the
Worker in the Cloudflare dashboard and apply again.

## Defaults and limits

| Choice | Value | Reason |
| --- | --- | --- |
| Message length | 1 to 5,000 characters | Long enough for any update, short enough to read on a phone |
| Page title | 1 to 100 characters on one line | A company or product name in a heading |
| Resolved notices on the page | The 5 most recent; `status.json` keeps all | The page stays short; programs read the whole history |
| Retries when another post changed `status.json` | Up to 3 attempts at saving `status.json`, and up to 3 uploads of `index.html` | Two people rarely post in the same second; more retries only delay an error |
| `Cache-Control` of both files | `public, max-age=60` | A post shows within about a minute where a cache keeps the files |
| Largest `status.json` read | 16 MiB | Years of real notices; a bounded read protects the poster from a wrong file |
| MMHotline-down alarm delays | The settings file's `wake_the_person_on_call_after_minutes_without_a_check_in`, 3 to 60 minutes, and `wake_the_backup_after_minutes_without_a_check_in`, at least 5 minutes more; 5 and 10 are a good start, and are the delays before the settings file applies values | Teams weigh a stopped MMHotline differently: a bank may want 5 minutes, a side project may accept 30. MMHotline checks in every minute and at once when it starts; a restart adds under a second and a server reboot leaves at most about 2.5 minutes without a check-in (measured 2026-11-11), so below 3 minutes a reboot would wake someone. Above 60, a stopped MMHotline would go unnoticed for over an hour. The backup waits at least 5 minutes more so the person on call can answer first. The wake times are their own settings part, which the next check-in carries to the Worker, so changing them never sets up the MMHotline-down alarm again |
| Check-in interval | Every 60 seconds, and within 5 seconds of a change to held messages, such as who is on call or the recipients' contact methods | 1,440 Worker requests a day, under 2% of the free plan; a handoff reaches the MMHotline-down alarm at once |
| Held message delay | 180 to 2,147,483,647 seconds | The shortest wake time, three missed check-ins, at least; the largest 32-bit integer at most |
| Sizes | 16 KiB per held message, 1 MiB per check-in | Bounds what one faulty check-in can write |
| "Send a test now" | At most once every 10 minutes and 6 times in 24 hours for the team; `down-alarm send-test` has no limit | Each test calls everyone on the list through the team's Twilio account. Ten minutes is enough to fix a contact method and test again; 6 a day covers a setup session. Root already holds the credentials |
| Sends per alarm run | 40: sends never attempted first, then the longest-waiting | The free plan allows 50 outgoing requests per run; sends without an answer never crowd out the rest |
| Retries of a send | 30 seconds after each attempt ended, until 10 minutes after the due time | A later wake-up is no longer timely |
| Worker and bucket names | `mmhotline-down-alarm-` with MMHotline's web host name, or `mmhotline-status-` with the public status page's host name, and 8 hexadecimal characters of the respective host name's SHA-256; at most 63 characters | A repeated setup with an unchanged MMHotline web host name finds the existing Worker. An unchanged public status page host name finds the existing bucket. Distinct host names give separate resources within an account. Cloudflare limits Worker and bucket names to 63 characters |
| Send log | 7 days, at most 200 sends | MMHotline handles each send at its first accepted check-in |
| Stop checking in while the background loop fails | After 2 minutes without a round that ended with no failed visit | A round normally ends every second, and a slow visit within a minute; the MMHotline-down alarm still wakes the person on call well within the wake time plus 3 minutes ([details](#when-mmhotline-stops-checking-in-on-purpose)) |
| Alert about people the MMHotline-down alarm cannot wake | Once for each changed list of people | Administrators fix the contact methods; repeating an unchanged list only adds noise |
| Alert about failing check-ins | After 10 minutes, once | Short faults pass quietly; a lasting one means the MMHotline-down alarm has sent its held messages and has nothing left to send if MMHotline goes down |
| Upload of an upgraded Worker | At each check-in round after an upgrade; an hour after a failure; administrators told after a day of failures | MMHotline automatically uploads the Worker bundled with the installed MMHotline version. The older Worker keeps working, so only a lasting upload failure needs a person |
