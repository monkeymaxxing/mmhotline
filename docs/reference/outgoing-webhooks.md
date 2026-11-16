# Outgoing webhooks

This page lists what a program receives from an outgoing webhook contact
method, and what MMHotline does with the program's answer. An **outgoing
webhook** sends each alert as a signed JSON `POST` request from the
MMHotline server to a program the team runs, such as a ticket system. The
program that gets the requests is the **receiver**. A receiver cannot claim
an incident. To add the webhook on the **Contact methods** page, see
[Send alerts to a program](../how-to/add-contact-methods.md#send-alerts-to-a-program).

## The request

MMHotline sends one `POST` request for each alert, test, and notice that the
webhook gets. Webhooks never combine alerts into one message.

```text
POST /alerts-from-mmhotline HTTP/1.1
Host: example.com
Content-Type: application/json
User-Agent: MMHotline webhook
Mmhotline-Signature: timestamp_unix_seconds=1794732176,hmac_sha256_hex=50a5cbe076fd7921b03ff4352e2e60a62767612b7c355644809136c16ef2cc3c
Mmhotline-Delivery-Id: 5d0c6c1e8a2b4f7e9c3d1a0b2e4f6a8c
Authorization: Bearer YOUR_BEARER_TOKEN

{"mmhotline_delivery_id":"5d0c6c1e8a2b4f7e9c3d1a0b2e4f6a8c","delivery_purpose":"subscribed_alert","severity":"sos","title":"web1 is down","message":"HTTP check failed 3 times in a row","mmhotline_alert_id":"a1b2c3d4e5f60718293a4b5c6d7e8f90","mmhotline_incident_id":"0f1e2d3c4b5a69788796a5b4c3d2e1f0","incident_status":"open","incident_url":"https://ops.example.com/incidents/0f1e2d3c4b5a69788796a5b4c3d2e1f0","alert_url":"https://ops.example.com/alerts/a1b2c3d4e5f60718293a4b5c6d7e8f90","alert_received_at":"2026-11-15T08:42:50Z"}
```

MMHotline's own code built this body and signature from example values,
with the example signing secret in
[Check the signature](#check-the-signature).

| Header | Value |
| --- | --- |
| `Content-Type` | Always `application/json` |
| `User-Agent` | Always `MMHotline webhook` |
| `Mmhotline-Signature` | The signature; see [Mmhotline-Signature](#mmhotline-signature) |
| `Mmhotline-Delivery-Id` | The delivery's ID, the same as the body's `mmhotline_delivery_id` |
| `Authorization` | `Bearer` and the bearer token; sent only when the webhook has a bearer token |

### Mmhotline-Signature

The header that proves the request came from MMHotline unchanged.

```text
Mmhotline-Signature: timestamp_unix_seconds=1794732176,hmac_sha256_hex=50a5cbe0...c3c
```

- `timestamp_unix_seconds` is the time MMHotline sent this attempt, in
  whole Unix seconds.
- `hmac_sha256_hex` is HMAC-SHA256 in lower-case hexadecimal. The key is
  the webhook signing secret, decoded from 64 hexadecimal characters into
  32 bytes. The HMAC covers the timestamp's digits, a `.`, and the exact
  body bytes.
- This is the same scheme alert sources use to sign alerts
  ([Mmhotline-Signature for alert requests](alert-requests.md#mmhotline-signature)).
- Each retry is signed again with a new timestamp. The body stays the same.
- The secret itself never travels in a request.

### Mmhotline-Delivery-Id

The header that names one delivery, so the receiver can ignore a repeat.
Every attempt of one delivery carries the same ID. A repeat can happen
when MMHotline got no answer and sends the delivery again
([Retries](#retries)).

### Authorization

The header that carries the optional **Webhook bearer token (optional)**,
as `Authorization: Bearer YOUR_BEARER_TOKEN`. The bearer token is separate
from the signing secret: a receiver may check either one, or both.

- At most 1024 printable characters, without spaces. Surrounding
  whitespace is removed.
- **Save webhook bearer token** with an empty field removes the bearer
  token, and the header is no longer sent.
- A new bearer token is used from the next attempt on, including retries
  of deliveries that are already waiting.

## The JSON body

One JSON object. MMHotline leaves out every field that is empty, except
`mmhotline_delivery_id` and `delivery_purpose`. A receiver should ignore
fields it does not know.

| Field | Value |
| --- | --- |
| `mmhotline_delivery_id` | The delivery's ID: 32 hexadecimal characters |
| `delivery_purpose` | Why MMHotline sent the request; see [delivery_purpose](#delivery_purpose) |
| `severity` | `info`, `warn`, `sos`, `resolved-info`, `resolved-warn`, or `resolved-sos`; left out for a test ([Severities](severities-and-incident-states.md#severities)) |
| `title` | The alert's title. For a test: `MMHotline test message` |
| `message` | The alert's message. For a triage agent report, the report's summary. For a test, the test text |
| `notes_from_mmhotline` | A note MMHotline added to the alert, such as `Received without severity, defaulting to 'warn'` |
| `mmhotline_alert_id` | The alert's ID |
| `mmhotline_incident_id` | The incident's ID; only an SOS has an incident |
| `incident_status` | `open`, `claimed`, or `resolved`, at the moment MMHotline queued the delivery ([Incident states](severities-and-incident-states.md#incident-states)) |
| `incident_url` | The incident's page: `web_url`, `/incidents/`, and the incident ID |
| `alert_url` | The alert's page: `web_url`, `/alerts/`, and the alert ID |
| `alert_received_at` | When MMHotline received the alert, in UTC, such as `2026-11-15T08:42:50Z` |

The links start with the settings file's
[`web_url`](settings-file.md#web_url). The body is copied when the delivery
is queued, so every retry sends the same bytes.

A test request, as a receiver got it in a real run:

```json
{"mmhotline_delivery_id":"859aa2fcb7d1a1721a69d04977fb4ab1","delivery_purpose":"contact_method_test","title":"MMHotline test message","message":"Barbara Example added this contact method at 2026-11-15T05:43:53Z.\n\nThis is a test of this contact method, not an incident."}
```

### delivery_purpose

| Value | Sent when |
| --- | --- |
| `subscribed_alert` | An alert arrived at a severity the webhook gets |
| `system_notice` | MMHotline raised an alert of its own, such as an alert source that stopped checking in |
| `contact_method_test` | Someone added the webhook, changed its URL, or pressed **Send test** |

Reason for the list: notices about MMHotline's own work that are meant for
a person, such as notices to administrators, never go to webhooks, because
programs read them. A webhook gets an SOS once, as an ordinary alert; it
is never part of the steps that wake the person on call.

## Check the signature

A receiver checks each request in this order:

1. Read the raw body bytes before parsing the JSON. MMHotline escapes
   some characters, such as `<` as `<`, so JSON encoded again does not
   match.
2. Read `timestamp_unix_seconds` and `hmac_sha256_hex` from
   `Mmhotline-Signature`.
3. Compute HMAC-SHA256 with each signing secret the receiver holds, and
   compare it in constant time. One match is enough.
4. Refuse a timestamp far from the receiver's clock, such as more than 5
   minutes. Retries carry a fresh timestamp, so a window does not refuse
   them.
5. Ignore a `Mmhotline-Delivery-Id` the receiver has already handled.

A short receiver check in Python, with only the standard library:

```python
import hashlib, hmac, time

def mmhotline_request_is_genuine(signature_header, body, signing_secrets_hex):
    fields = dict(part.split("=", 1) for part in signature_header.split(","))
    timestamp = fields["timestamp_unix_seconds"]
    if abs(time.time() - int(timestamp)) > 300:
        return False
    for secret_hex in signing_secrets_hex:
        expected = hmac.new(bytes.fromhex(secret_hex),
                            timestamp.encode() + b"." + body,
                            hashlib.sha256).hexdigest()
        if hmac.compare_digest(expected, fields["hmac_sha256_hex"]):
            return True
    return False
```

With the example signing secret
`00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff`, the
body in [The request](#the-request), and the timestamp `1794732176`, the
HMAC is
`50a5cbe076fd7921b03ff4352e2e60a62767612b7c355644809136c16ef2cc3c`.
Use this to test a receiver, with the clock check turned off. Never use
the example secret for a real webhook.

## Signing secrets

The key MMHotline signs every request with. MMHotline makes the first
signing secret when the webhook is added, and shows the
**Webhook signing secret (hex)** once, with its **Signing secret ID**.

- A signing secret is 32 random bytes, shown as 64 hexadecimal characters.
  The signing secret ID is 32 hexadecimal characters.
- MMHotline never shows a signing secret again. A lost signing secret is
  replaced by rotation.
- From the server, `mmhotline contact-methods add` prints the signing
  secret, or writes it to `--outgoing-webhook-signing-secret-file`
  ([mmhotline contact-methods add](mmhotline-command.md#mmhotline-contact-methods-add)).
- A database backup holds the signing secrets and bearer tokens. Protect
  backups as secret data.

### Rotation with two secrets

A webhook holds one or two signing secrets. The oldest signing secret signs
every request. The card's **Webhook signing secrets and bearer token**
section marks it `(signs requests)`.

1. **Add a second webhook signing secret (for rotation).** MMHotline shows
   the new secret once. The old secret keeps signing.
2. Make the receiver accept the old and the new signing secret.
3. **Revoke this webhook signing secret** on the old one. The new secret
   signs from the next attempt on.

| You try | MMHotline says |
| --- | --- |
| Add a third signing secret | `the outgoing webhook already has two signing secrets; revoke a signing secret before adding another` |
| Revoke the only signing secret | `an outgoing webhook needs at least one signing secret; add a second signing secret before revoking the remaining signing secret` |

Reason: a receiver that accepts both secrets during the change never
refuses a genuine request, and a webhook always signs.

## Rules for the webhook URL

MMHotline sends only to a public internet address over `https://`.

- The URL starts with `https://`, has a host name, and is at most 2048
  characters. It has no user name, password, or `#` fragment.
- The address must be a public internet address. MMHotline refuses
  loopback, private, link-local (which includes cloud metadata addresses),
  carrier-grade NAT, documentation, and other special ranges, for IPv4,
  IPv6, and IPv4 written as IPv6.
- MMHotline checks the address when it connects, after DNS. A host name
  that resolves to a private address fails then, even when the URL was
  accepted.
- MMHotline follows no redirects. A `3xx` answer counts as a refusal.
- MMHotline verifies the receiver's TLS certificate and uses no proxy.
- The request leaves from the MMHotline server.
- Other members see only the scheme and host, such as
  `https://example.com/…`, because a path or query may hold a token. The
  full URL shows only in the edit form, to the people who may edit the
  webhook.
- An installation has at most 20 outgoing webhook contact methods.

Reason: alerts must not reach programs on the server's private network.

| Saving a URL that | MMHotline says |
| --- | --- |
| Is not a full URL | `enter the full webhook URL, such as https://example.com/alerts-from-mmhotline` |
| Does not start with `https://` | `the webhook URL must start with https://` |
| Is a non-public IP address | `the webhook URL must be a public internet address` |
| Has a bearer token with a space or over 1024 characters | `the webhook bearer token must be at most 1024 printable characters without spaces` |
| Would be the 21st webhook | `an installation can have at most 20 outgoing webhook contact methods` |

## The receiver's answer

MMHotline waits at most 10 seconds for an answer, and reads and ignores up
to 64 KiB of the answer body. The HTTP status code decides the result:

| Receiver's answer | Result a person sees | Retried? |
| --- | --- | --- |
| Any `2xx` | `Accepted by the webhook`, with the code, such as `HTTP 200` | No |
| `408`, `429`, or any `5xx` | `The webhook answered HTTP 503` (with the real code) | Yes |
| Any other code, redirects included | `Refused by the webhook (HTTP 404)` (with the real code) | No |
| Could not connect | `Could not connect to the webhook` | Yes |
| No answer within 10 seconds | `No answer from the webhook within 10 seconds` | Yes |
| Connection closed before an answer | `The webhook connection broke before an answer` | Yes |

**Accepted by the webhook** means only that the receiver answered `2xx`.
Accepted does not prove the receiver handled the alert. Answer `2xx` only
after the receiver has stored the request, and answer within 10 seconds.

A request with no answer has an unknown outcome: the receiver may have got
it. MMHotline sends the delivery again, with the same
`Mmhotline-Delivery-Id`.

## Retries

MMHotline retries a delivery on the results marked "Yes" above.

- The first retry waits 5 seconds. Each wait doubles, up to 5 minutes.
- A delivery has at most 8 attempts, about 10 minutes from the first
  attempt, and never more than 23 hours.
- Every retry sends the same body, with the same `Mmhotline-Delivery-Id`
  and a new signature timestamp.
- Each attempt uses the signing secrets and bearer token saved at that
  moment.
- A person sees the state on the card and on the delivery's page, such as
  `Failed, will retry at ...`, `Failed, no more retries`, or, after no
  answer, `Unknown (no answer from the sending service), will retry at ...`.

## Delivery limits

The limits keep a broken receiver or a flood of alerts from piling up
requests.

- **60 alerts a minute.** A webhook gets at most 60 alerts and notices in
  one clock minute. More are not sent, with the reason
  `Not sent: this webhook got more than 60 alerts in one minute`. An SOS
  and a test are never held back.
- **After 3 failed attempts in a row,** the webhook keeps only its newest
  waiting delivery. Older waiting deliveries end as
  `Not sent: the contact method has been failing since {time}`. An SOS and
  a test are never dropped.
- **Administrators are told once** for each run of failures, with a
  Warning from MMHotline: `MMHotline cannot deliver alerts to example.com`.
- An accepted request starts the count again. So does a changed URL;
  results for an earlier URL do not count.

## Other results

These results mean MMHotline did not send the request. None of them is
retried.

| Result | Meaning |
| --- | --- |
| `Not sent: the webhook address is not a public internet address` | The host name resolved to a non-public address when MMHotline connected |
| `Not sent: the webhook URL is invalid` | The saved URL could not make a request |
| `Not sent: the webhook settings are incomplete` | The saved settings or secrets could not be read |
| `Not sent: the webhook signing secret is invalid` | The saved signing secret is not 32 bytes of hexadecimal |
| `Not sent: could not encode the webhook body` | The body could not be written as JSON |

The last four point to damaged saved data. Remove the webhook and add it
again.
