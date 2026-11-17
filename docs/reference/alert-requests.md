# Alert requests

This page lists the format of the two requests a tool sends to MMHotline:
an **alert**, which reports a problem, and a **check-in**, which says the
tool still runs. It covers the two URLs, every JSON field, the signature,
the answers, and the limits. An **alert source** is MMHotline's record of
one sending tool; its **signing secret** is 64 hexadecimal characters that
the tool and MMHotline share. To connect a tool step by step, follow
[Connect a tool that sends alerts](../how-to/connect-a-tool-that-sends-alerts.md).
What each severity does after it arrives is in
[Severities and incident states](severities-and-incident-states.md).

## The alert intake address

The alert intake address is where every tool sends, such as
`http://10.20.0.1:8484`.

```sh
sudo mmhotline alert-sources list
```

```text
Alert source Monitoring (monitoring) is active.
Intake URL: http://10.20.0.1:8484/alert-intake/monitoring
Check-in URL: http://10.20.0.1:8484/check-in/monitoring
No check-in has arrived yet. MMHotline starts watching for missing check-ins when the first check-in arrives.
...
Alert intake address: http://10.20.0.1:8484
```

- The address is `http://` plus the settings file's
  `alert_intake_listen_address`, normally a private network address of
  the MMHotline server, such as a WireGuard address.
- It is not the web URL that people open. Reason: Cloudflare Access sits
  in front of the web URL and refuses a request without a person's
  sign-in.
- A tool on the MMHotline server and tools on other servers use the same
  address.
- It is plain HTTP. Reason: every request is signed and every answer that
  matters carries a signed receipt, so the private network needs no TLS.
- The address serves only the two URLs below. Any other path gets
  `404 page not found`.
- The Alert sources page shows the same values under **Connection
  details**. `alert-sources list --json` gives `alert_intake_address`,
  and `intake_url` and `check_in_url` for each source.

A tool needs three values: the alert intake address, the alert source ID,
and the signing secret. A sender you write yourself conventionally reads
`MMHOTLINE_ALERT_INTAKE_URL` (the full intake URL) and
`MMHOTLINE_SOURCE_SIGNING_SECRET` (the 64 hexadecimal characters).

## POST /alert-intake/ALERT_SOURCE_ID

Sends one alert. `ALERT_SOURCE_ID` is the source's `alert_source_id` from
the settings file, such as `monitoring`.

```sh
POST http://10.20.0.1:8484/alert-intake/monitoring
Content-Type: application/json
Mmhotline-Signature: timestamp_unix_seconds=1763196176,hmac_sha256_hex=...
```

```json
{
  "sender_alert_id": "backup-2026-11-15",
  "sender_incident_key": "nightly-backup",
  "severity": "sos",
  "title": "Nightly backup failed",
  "message": "pg_dump exited 1: disk full on /backups.",
  "happened_at": "2026-11-15T03:00:12Z"
}
```

- `Content-Type` must be `application/json`.
- The body is one JSON object, at most 64 KiB, in UTF-8.
- Each field may appear once. A field not listed below, or a field given
  twice, is refused. Names are matched exactly, including case.
- `title` and `message` are required; the other fields are optional.
- MMHotline saves the alert before it answers, so a `200` means the alert
  is safe even if the server stops a moment later.

### sender_alert_id

The tool's own ID for this one alert, so a retry is counted once.

- Type: a string of 1 to 256 bytes. Optional, but use it.
- Keep the same value on every retry of one alert. Give each new alert a
  new value.
- A retry with the same `sender_alert_id` and the same `severity`,
  `title`, `message`, and `sender_incident_key` is answered `200` with
  `"already_received":true` and changes nothing. `happened_at` is not
  compared.
- A different alert that reuses the value is refused:
  `400 sender_alert_id already used for a different alert; give each new alert its own sender_alert_id`.
- Without it, MMHotline recognizes only an identical signed request as a
  repeat. A retry signed again at a new second is a new request, so it
  would be saved as a second alert.
- MMHotline recognizes a repeat for as long as it keeps the alert.

### sender_incident_key

The tool's name for one problem, which ties an SOS to its later alerts.

- Type: a string of 1 to 256 bytes. Optional.
- An `sos` whose key matches an incident that is not resolved joins that
  incident instead of opening another.
- A `resolved-sos` with the key resolves that incident, claimed or not.
- After the incident is resolved, the key matches nothing, so a new
  failure with the same key opens a new incident.
- Use one key per problem. A nightly job that uses one key every night
  joins a new failure to an earlier night's incident while that incident
  is still open.
- An `sos` without a key opens its own incident, which a person resolves.
- The triage agent also uses the key: alerts with the same source and key
  are one problem
  ([what starts or joins a session](../changing-mmhotline/design/triage-agent.md#what-starts-or-joins-a-session)).

### severity

How urgent the alert is.

- Allowed values: `info`, `warn`, `sos`, `resolved-info`, `resolved-warn`,
  `resolved-sos`. [Severities and incident states](severities-and-incident-states.md)
  says what each one does.
- Left out: the alert is saved as `warn`, with the note
  `Received without severity, defaulting to 'warn'`.
- Any other value, or `null`, is refused:
  `400 severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos`.

### title

The one line people see first, in notifications and on pages.

- Type: a string of 1 to 256 characters, not only spaces. Required.
- Error: `400 title must contain 1–256 characters`.

### message

The rest of the alert's text, such as the error output.

- Type: a string of at most 16 KiB. Required, but may be `""`.
- Error: `400 message exceeds 16 KiB or is not UTF-8`.
- MMHotline shows the text as text. It never opens links in it.

### happened_at

When the problem happened, on the tool's own clock.

- Type: an RFC 3339 time, such as `2026-11-15T03:00:12Z`. Optional.
- For display only. Ordering, deadlines, and escalation use the time
  MMHotline received the alert.
- When the alert arrives more than two minutes after `happened_at`, the
  alert's page and notifications show both times, such as
  `Happened at 14:02 UTC. Received at 14:31 UTC (29 minutes later).`
- Error: `400 happened_at must be an RFC 3339 time, such as 2026-09-29T03:00:12Z`.

### The answer to an alert

```json
{"mmhotline_alert_id":"11eb3ef896a5e250a9ebc2527a8990a9","mmhotline_incident_id":"52f9135fc0ee491e0e6c49fc40bedc25","already_received":false}
```

| Field | Meaning |
| --- | --- |
| `mmhotline_alert_id` | MMHotline's ID for the saved alert |
| `mmhotline_incident_id` | The SOS incident the alert opened, joined, or resolved; absent when the alert has no incident |
| `already_received` | `true` when this was a repeat of an alert MMHotline already has |

A `200` says MMHotline saved the alert. It does not say that anyone was
notified or claimed anything.

## POST /check-in/ALERT_SOURCE_ID

Says that the tool still runs. MMHotline starts watching a source for
silence when its first check-in arrives, and alerts when check-ins stop
for the times set in the settings file
([`warn_after_no_check_in_for_minutes` and `sos_after_no_check_in_for_minutes`](settings-file.md#alert_sources)).

```json
{
  "sent_at": "2026-11-15T06:50:00Z",
  "sent_by_node_name": "monitor01",
  "nodes_in_whole_node_maintenance": [
    {
      "node_name": "monitor01",
      "maintenance_until": "2026-11-15T08:00:00Z",
      "reason": "moving the monitoring server",
      "started_by": "barbara"
    }
  ]
}
```

- Send one every 30 seconds. MMHotline's alerts about silence say a
  source "normally checks in with MMHotline every 30 seconds".
- It is signed exactly as an alert is, with the same secret.
- The same 64 KiB body limit applies. Unknown fields are refused, so a
  body meant for alert intake is refused here, and the other way round.
- A check-in is not an alert: it creates no alert and does not count
  toward the alert limits.
- Any signed alert from the source also counts as hearing from it.
  Only a check-in clears an alert about silence that already went out.

### sent_at

When the tool sent the check-in, on its own clock.

- Type: an RFC 3339 time. Required.
- A check-in counts only when its `sent_at` is newer than the newest one
  received. An older or equal one, arriving late or resent, is answered
  `200` and changes nothing: it never replaces what the newer one said,
  and never delays or ends an alarm about missing check-ins.

### sent_by_node_name

The name of the server the check-in comes from, such as `monitor01`.

- Type: a string of 1 to 256 bytes. Required.
- MMHotline uses it to find the sender's own maintenance window in the
  list below.

### nodes_in_whole_node_maintenance

The servers that the tool has in maintenance as a whole.

- Type: a list of at most 1,000 objects. Optional; an empty list or a
  left-out list both mean none.
- Each object: `node_name` (required, 1 to 256 bytes), `maintenance_until`
  (required, RFC 3339 time), `reason` and `started_by` (optional, at most
  256 bytes each).
- Only the window for `sent_by_node_name` matters to MMHotline: it counts
  as planned downtime for the source, so the source's silence wakes nobody
  until `maintenance_until`. Windows for other servers change nothing.
- A window counts for at most 7 days from the check-in that listed it.
  While check-ins keep listing it, that limit moves on. Reason: every
  downtime must end.
- To stay under 64 KiB, a tool with many servers caps the list by its
  encoded size, leaving out `reason` and `started_by` first.
- [Declare planned downtime](../how-to/declare-planned-downtime.md#let-the-tool-report-its-maintenance)
  shows this in use.

### The answer to a check-in

```json
{"check_in_received_at":"2026-11-15T06:52:08Z"}
```

`check_in_received_at` is MMHotline's time of receipt. MMHotline answers
once it has recorded the check-in in memory; it writes the database only
when the check-in changes something about the source.

## Mmhotline-Signature

The request header that proves the request comes from the alert source.

```text
Mmhotline-Signature: timestamp_unix_seconds=1763196176,hmac_sha256_hex=3f1c...e9
```

To compute it:

1. Decode the 64 hexadecimal characters of the signing secret into 32
   bytes.
2. Take the current time in whole Unix seconds, such as `1763196176`.
3. Compute HMAC-SHA256 with the 32 bytes as the key, over the timestamp's
   digits, a `.`, and the exact body bytes you send.
4. Write the header as `timestamp_unix_seconds=` plus the timestamp, a `,`,
   and `hmac_sha256_hex=` plus the HMAC in hexadecimal.

Rules:

- Exactly one `Mmhotline-Signature` header, with exactly these two fields,
  each once, in this order, and nothing else. Anything else is refused
  with `401`.
- The timestamp must be within 5 minutes of the MMHotline server's clock.
  Keep the tool's server clock synchronized, such as with chrony.
- Sign the bytes you send. Encoding the JSON again after signing, even
  with other spacing, breaks the signature.
- The secret itself never travels. Someone who captures a request learns
  nothing that signs a new one. Sending a captured request again within
  the 5 minutes is a repeat and changes nothing (see
  [`sender_alert_id`](#sender_alert_id)).
- While a tool moves to a new secret, the settings file lists both
  secrets, and either one is accepted
  ([Change a source later](../how-to/connect-a-tool-that-sends-alerts.md#change-a-source-later)).

## Mmhotline-Receipt-Signature

The answer header that proves the answer comes from MMHotline and belongs
to this request. MMHotline adds it to `200`, `400`, and `429` answers, only
when it verified the request's signature.

```text
Mmhotline-Receipt-Signature: timestamp_unix_seconds=1763196177,hmac_sha256_hex=9a0b...c4
```

The HMAC-SHA256, with the same 32-byte secret as the request, covers:

```text
mmhotline-receipt.ROUTE.STATUS.REQUEST_HMAC.TIMESTAMP.BODY
```

- `ROUTE` is `alert-intake` or `check-in`, the route the request went to.
- `STATUS` is the HTTP status code, such as `200`.
- `REQUEST_HMAC` is the `hmac_sha256_hex` value of the request's own
  `Mmhotline-Signature`, in lower case.
- `TIMESTAMP` is the receipt's own `timestamp_unix_seconds`.
- `BODY` is the exact answer body, including its final line break.

Reason: another program could answer at MMHotline's address, such as one
that took the port while MMHotline was stopped. A receipt tied to the
route, the status, the request, and the body cannot be forged by echoing
the request, sending it to the other route, reusing an earlier answer, or
changing the status. A tool counts an alert as delivered only with a valid
receipt on a `200`.

## Answers and refusals

Every answer that is not `200` is plain text with a line break at the end.
`Signed` means the answer carries a
[receipt](#mmhotline-receipt-signature).

| Status | Text | Signed | When | What the tool does |
| --- | --- | --- | --- | --- |
| `200` | The JSON answer above | Yes | Saved, or a repeat of a saved alert | Done |
| `400` | `body: Content-Type must be application/json` | No | The `Content-Type` header is missing or not JSON | Retry; fix the sender |
| `400` | `body could not be read` | No | The connection broke while sending the body | Retry |
| `400` | The reason, such as `title and message required` (list below) | Yes | The signature is right, but the body breaks a rule | Stop: the alert is refused for good |
| `401` | `source authentication failed` | No | Unknown or disabled source, wrong secret, a malformed header, or a clock more than 5 minutes off | Retry; check the secret, the source, and both clocks |
| `404` | `404 page not found` | No | A path other than the two above | Fix the URL |
| `413` | `body exceeds 64 KiB` | No | The body is over 64 KiB | Retry; keep alerts within the limits |
| `429` | `admission limit reached; retry later` | Yes | A [limit](#limits) is reached | Retry after `Retry-After`, which is `60` seconds |
| `503` | `could not be committed; retry later` | No | MMHotline could not save the alert | Retry |

The `401` answer is the same for every cause, so a stranger learns nothing
from it. The Alert sources page tells them apart under **Rejected requests
in the last 24 hours**: `Wrong signature`, `The sender's clock is more than
5 minutes off`, `The alert is not valid`, and `Too many alerts`. These
counts live in memory and start again after a restart.

The signed `400` reasons for an alert:

- `unknown or duplicate field "priority"`
- `body must be an object`
- `one JSON object required`
- `title must be a string` (also for any other field that is not a string)
- `title and message required`
- `severity must be one of: info, warn, sos, resolved-info, resolved-warn, resolved-sos`
- `sender_alert_id and sender_incident_key must be nonempty when present`
- `title must contain 1–256 characters`
- `message exceeds 16 KiB or is not UTF-8`
- `sender_alert_id or sender_incident_key exceeds 256 bytes or is not UTF-8`
- `happened_at must be an RFC 3339 time, such as 2026-09-29T03:00:12Z`
- `sender_alert_id already used for a different alert; give each new alert its own sender_alert_id`

The signed `400` reasons for a check-in:

- `unknown field "severity"`
- `a check-in is a JSON object`
- `a check-in is {"sent_at": ..., "sent_by_node_name": ..., "nodes_in_whole_node_maintenance": [...]}`
  (a field of the wrong type, such as `sent_at` that is not a time)
- `one JSON object required`
- `sent_at and sent_by_node_name are required`
- `at most 1000 nodes in nodes_in_whole_node_maintenance`
- `each node in maintenance needs node_name and maintenance_until`

Each refused alert also writes one line to the service's log, which
`sudo journalctl -u mmhotline` shows, such as:

```text
Alert refused from alert source monitoring: sender_alert_id "disk-app01-1", severity "info": invalid input: sender_alert_id already used for a different alert; give each new alert its own sender_alert_id
```

### A refused SOS wakes the team

When MMHotline refuses a correctly signed `sos`, it opens an SOS incident
of its own, so the problem still reaches the person on call:

```text
SOS incident dad26ba76b59defe382db9a14f5fd5c8: MMHotline is refusing SOS alerts from “Monitoring”: the sender's clock is more than 5 minutes off
```

This happens for an invalid alert, a clock more than 5 minutes off, and
the limits on incidents and waiting notifications. Further refusals join
the source's open refusal incident. A request with a wrong signature never
opens anything, so a stranger who knows an alert source ID cannot wake the
team.

## Retries

A tool that sends for real retries each alert until one of these:

- a `200` with a valid receipt: the alert is saved;
- a `400` with a valid receipt: the alert is refused for good;
- the tool's own deadline, at most one day.

Every other answer, and no answer, is a failed attempt. Each retry keeps
the same `sender_alert_id` and signs the body again with a new timestamp.
An unsigned `400` or a `413` cannot prove that MMHotline answered, so the
tool retries those too; a tool that keeps its alerts within the limits
never meets a `413`.

## Limits

| Limit | Counts | Answer |
| --- | --- | --- |
| 120 new alerts per UTC minute, per alert source | Every severity except `sos` and `resolved-sos`; a repeat does not count | `429` |
| 1,000 new `info` and `warn` alerts per UTC hour, for the whole installation | `info`, `warn`, `resolved-info`, `resolved-warn` | `429` |
| 1,000 incidents not resolved | A new `sos` that would open an incident; an `sos` that joins an incident, and a `resolved-sos`, still pass | `429`, and MMHotline's own SOS |
| 10,000 notifications waiting to be sent, of which `info` and `warn` may use 9,000 | Alerts that would queue more | `429`; for an `sos`, also MMHotline's own SOS |
| 250 SOS notifications waiting to be sent, per alert source | A new `sos` from that source | `429`, and MMHotline's own SOS |
| 12 check-ins a minute, per alert source | Check-ins | `429` |
| 100 alert sources per installation | The settings file's `alert_sources`; `settings apply` also counts disabled sources | Refused by `settings check` and `settings apply` |

Reason for the limits: MMHotline is read by people, not a log store. The
limits stop a broken sender from flooding the team, and never stop an
`sos` for rate. When a source sends too many alerts, administrators get at
most one notice an hour:
`Alert source “Monitoring” is sending too many alerts: MMHotline refuses some of them`.
The [ingestion design](../changing-mmhotline/design/ingestion.md#intake-limits-and-rejected-requests)
gives the reasons for each number.

## A full sender example

This example sends one signed `info` alert and one check-in, and checks
each answer's receipt. Run it on the MMHotline server; the same Python
code fits a monitoring tool's alert hook. It assumes the source
`monitoring` from
[Connect a tool that sends alerts](../how-to/connect-a-tool-that-sends-alerts.md).

1. Make the secret in a file only root reads:

   ```sh
   sudo install -d -m 0700 /etc/example-monitoring
   sudo sh -c 'umask 077; openssl rand -hex 32 > /etc/example-monitoring/mmhotline-signing-secret'
   ```

2. Put the same 64 characters in the settings file, from the team's vault
   with a deploy tool, or with `sudoedit`, and apply it:

   ```yaml
   alert_sources:
     - alert_source_id: monitoring
       name: Monitoring
       signing_secrets: ["THE 64 HEXADECIMAL CHARACTERS"]
       warn_after_no_check_in_for_minutes: 2
       sos_after_no_check_in_for_minutes: 5
   ```

   ```sh
   sudo mmhotline settings apply
   ```

3. Save the source's URLs and send:

   ```sh
   sudo mmhotline alert-sources list --json > /tmp/mmhotline-alert-sources.json
   sudo python3 - <<'PYTHON'
   import datetime
   import hashlib
   import hmac
   import json
   import time
   import urllib.error
   import urllib.request
   from pathlib import Path

   sources = json.loads(Path('/tmp/mmhotline-alert-sources.json').read_text())['alert_sources']
   source = next(source for source in sources if source['alert_source_id'] == 'monitoring')
   secret = bytes.fromhex(Path('/etc/example-monitoring/mmhotline-signing-secret').read_text().strip())


   def hmac_hex(message):
       return hmac.new(secret, message, hashlib.sha256).hexdigest()


   def receipt_is_valid(route, status, request_hmac, headers, answer):
       fields = headers.get('Mmhotline-Receipt-Signature', '').split(',')
       if len(fields) != 2 or not fields[0].startswith('timestamp_unix_seconds=') or not fields[1].startswith('hmac_sha256_hex='):
           return False
       timestamp = fields[0].removeprefix('timestamp_unix_seconds=')
       expected = hmac_hex(f'mmhotline-receipt.{route}.{status}.{request_hmac}.{timestamp}.'.encode() + answer)
       return hmac.compare_digest(expected, fields[1].removeprefix('hmac_sha256_hex='))


   def send(route, url, payload):
       body = json.dumps(payload, separators=(',', ':')).encode()
       timestamp = str(int(time.time()))
       request_hmac = hmac_hex(timestamp.encode() + b'.' + body)
       request = urllib.request.Request(url, data=body, headers={
           'Content-Type': 'application/json',
           'Mmhotline-Signature': f'timestamp_unix_seconds={timestamp},hmac_sha256_hex={request_hmac}',
       })
       try:
           with urllib.request.urlopen(request, timeout=15) as response:
               status, headers, answer = response.status, response.headers, response.read()
       except urllib.error.HTTPError as error:
           status, headers, answer = error.code, error.headers, error.read()
       receipt = 'valid receipt' if receipt_is_valid(route, status, request_hmac, headers, answer) else 'NO VALID RECEIPT'
       print(status, receipt, answer.decode().strip())


   send('alert-intake', source['intake_url'], {
       'sender_alert_id': 'monitoring-connection-test',
       'severity': 'info',
       'title': 'Monitoring connected',
       'message': 'The monitoring sender can authenticate to MMHotline.',
   })
   send('check-in', source['check_in_url'], {
       'sent_at': datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
       'sent_by_node_name': 'monitor01',
       'nodes_in_whole_node_maintenance': [],
   })
   PYTHON
   ```

   ```text
   200 valid receipt {"mmhotline_alert_id":"2aab1b569a351793a7e1aaa2fa6a59d3","already_received":false}
   200 valid receipt {"check_in_received_at":"2026-11-15T09:34:16Z"}
   ```

   Run it again and the alert line ends `"already_received":true`, because
   the `sender_alert_id` is the same. With a wrong secret, both lines say
   `401 NO VALID RECEIPT source authentication failed`.

The example needs Python 3.9 or later. It prints the answer and does not
retry; a real sender adds the [retries](#retries). No secret value appears
in `alert-sources list` output or on any page.
