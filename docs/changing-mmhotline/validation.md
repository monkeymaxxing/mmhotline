# Current validation

The results below record checks on 2026-10-01, unless another date is given.
The 2026-10-01 checks followed the addition of Telegram, Slack, custom
schedule code, and the fake sending services. Browser checks from
2026-09-30 predate the Outgoing webhook and Telegram message cards.
Newer pages have their check dates in the rows below.
The Go test runs on macOS ARM64 and Linux AMD64 were repeated on 2026-10-27
with Go 1.26.10, with the [release checks](#release-checks) below.
The validation results record dated checks, without claiming every check
ran on the latest build.
The [design status](design/README.md#status) records current feature status.
An SOS is the urgent alert that wakes the person on call, even at night;
to claim an incident is to say "I'm on it", which stops MMHotline
contacting the next person.

## Application checks

| Check | Result |
| --- | --- |
| `CGO_ENABLED=0 go test -count=1 -json ./...` | Passed on macOS ARM64 (2026-10-27, Go 1.26.10): 540 top-level tests, 931 including subtests, zero failures. The Mailpit acceptance test and the two tests that run `mmx` skipped because the required programs were absent. Mailpit is a local test mail server. |
| Application checks: Linux runs | Passed on a Linux AMD64 server (Debian 13, Go 1.26.10, 2026-10-27) as an ordinary user with `MMHOTLINE_TEST_MMX_PROGRAM` set, and as root, where the custom schedule sandbox runs under seccomp and its resource limits. The `node --test` tests of the MMHotline-down alarm's Worker skipped on the Linux server without `node` |
| Race detector | Every Go test under stress: see [release checks](#release-checks) |
| `go vet ./...` and `gofmt -l .` | Passed on macOS ARM64 (2026-10-27); no unformatted files |
| cgo-disabled Linux AMD64 and ARM64 builds | The Linux AMD64 build and the Linux ARM64 build succeeded. cgo lets Go code call C code. `file` confirms statically linked ELF executables |
| Dependency cgo inspection | `CGO_ENABLED=0 go list -deps` reported no selected CgoFiles |
| CLI | `mmhotline --help` and `mmhotline serve --help` exit 0; a missing command or unknown flag prints usage once and exits 4; setup creates `mmhotline.db`; SIGTERM exits 0 and SIGINT exits 130; SIGTERM while the Telegram poller waits in `getUpdates` exits 0 without waiting for the poll to end |
| Generic alert intake | `timestamp_unix_seconds`/`hmac_sha256_hex` signatures (old format, wrong order, and duplicate names rejected), signing-secret rotation, `sender_alert_id`/`sender_incident_key` fields, `mmhotline_alert_id`/`mmhotline_incident_id`/`already_received` receipts, allowed-values errors, strict JSON/text bounds, source rate, duplicates/replays, SOS correlation to unresolved incidents, resolution and atomic rollback pass |
| On call and incidents | By day of the week and By hour of the day pass across daylight saving time changes (DST). Weekly rotation passes for member order, returning to the first member after the last, start date, DST, and `next_in_rotation`. Schedule previews agree with live on-call selection. A save with an outdated schedule configuration revision is refused. Taking on-call, team mute, unavailability, claims, escalation steps, and history and notices that name people and incidents pass |
| Email outbox | Authorization/privacy, suppression, stable request identity/payload, reserve/recheck, uncertain/stale outcomes, retry-aware outcome labels, retry bounds, incident-only notification lists including escalation notices, and restart pass. |
| Contact method types and outgoing webhooks | Contact method type facts decide routing, team-account use, and installation-wide limits; webhook URL rules (https only, public addresses, query hidden from the destination); the signed JSON body and headers checked by the fake receiver; retries with identical bodies; refusal without retry; a loopback address refused at connection time; the 3-failures rule keeping only the newest delivery with one administrator notice that never goes to a webhook; signing-secret rotation and bearer token removal; the 20-webhook limit; and the Contact methods page showing the secret once pass |
| Intake limits and throttle | The per-source and installation-wide hourly limits refuse Warning and Info with `429` while SOS and resolved SOS pass; rejected-request reasons counted in memory for existing sources only and dropped after 24 hours; one MMHotline SOS per source for correctly signed refused SOS alerts (clock off, invalid, 1,000 unresolved incidents), never for a wrong signature; the SOS part of the pending limit; the 100-source limit; a burst becoming one combined message; the one-minute and ten-minute intervals; SOS never delayed or combined; a combined message frozen after its first attempt; webhooks getting every alert up to 60 a minute; tests never throttled pass |
| Receipts and `happened_at` | Receipts signed with the secret that signed the request (including after rotation) on 2xx, 400, and 429; unverified requests and 401 answers unsigned; `happened_at` validated, stored, and shown only when an alert arrived more than two minutes late pass |
| Check-ins and planned downtime | The check-in route passes signed-receipt, strict request and response decoding, wrong-signature, and per-minute-limit checks. Waiting for the first check-in passes, as do Warning then SOS at the configured times and SOS with Warning off. An alert counts as hearing from the alert source. Resumed check-ins resolve an unclaimed SOS, but keep a claimed SOS open with a history note and a quiet message to the incident's owner. After a Warning, resumed check-ins send resolved-warn, the notification that the warning condition ended. |
| Check-in persistence and restart | In-memory check-in tracking passes. Unchanged check-ins cause no database write, before or after a restart. The first check-in, changed content, one minute without hearing from the alert source, and resumed check-ins each cause a write. A maintenance window beyond 7 days is stored again once a day. A restart keeps exact times for an alert source marked as stopped, and counts an unmarked source's silence from the restart. Added and revoked signing secrets and a disabled alert source take effect at once. A check-in racing the stopped mark passes, including under the race detector. |
| Planned downtime | Manually started planned downtime passes with an on-time end and an early end. A sending node is the server that sends the check-ins. Planned downtime from a maintenance window for the sending node outlives the node's check-ins. Manual and maintenance-window downtime are each followed by an SOS one minute after downtime ends. The alert source page's forms pass |
| The team's next deadline in memory | A team with nothing due is not checked. An accepted alert makes the team due at once, including through the real background loop. Retries, the end of unavailability, the end of team mute, and schedule handoffs run at the due times. The next handoff is checked for every schedule kind. |
| Scheduler: incidents and check-ins | For an unclaimed incident, the scheduler checks the team at repeat deadlines to notify again and response deadlines to advance escalation, not between deadlines. Alert-source silence schedules checks after one minute, at the Warning time, at the SOS time, when planned downtime ends, and a minute later. After the silence SOS, further silence work waits until a person resolves the incident. The team is checked periodically after 10 minutes. |
| Scheduler: custom schedules and restart | The custom schedule loop detects an active custom schedule. Each wake-up was checked by disabling the wake-up and seeing the corresponding test fail. After `mmhotline serve` starts, the first team check counts earlier calls as ended and accepts late call reports. A reserved call or email has a stored delivery attempt that the previous process reserved without recording a result. The first team check makes the reserved call and sends the reserved email at once. Restart tests create a new App using the state directory from before the restart and retain the delivery assertions |
| Telegram and claiming from a notification | Account-linking checks pass through fake Telegram. “Connect Telegram” adds a member's private chat, and “Add the MMHotline bot to a Telegram group” adds a shared group for administrators only. Start tokens are stored hashed, single use, refused after 15 minutes, and refused in the wrong kind of chat. A member may have at most 5 unused tokens. Nobody can type or edit a chat ID. |
| Telegram incident buttons | An SOS message carries “Claim this incident (assign it to me)” and “Resolve”, with `callback_data` within 64 bytes. A linked member's press claims (“Claimed by Responder in Telegram”) or resolves (“Resolved by Responder in Telegram”) and replaces the buttons with the incident's state, keeping Resolve after a claim. An unknown or former member gets the “does not know your Telegram account” answer with the incident link. A press from another chat or message, or with made-up data, changes nothing. An incident claimed by someone else is never taken over, and a resolved incident is reported as resolved. A button press racing a browser claim gives one claim. |
| Telegram message combining | SOS chat-summary checks pass: the 11th and 12th SOS in 10 minutes become one “2 more SOS incidents” message without buttons. The next SOS message waits a minute, and after 10 minutes SOS messages go individually again. Combined Warning messages and resolved messages have no buttons. |
| Telegram tests, delivery failures, and polling | Send test passes. An unknown chat fails for good, and a request without an answer is retried. Polling checks pass for the offset, repeated updates after a lost offset, and a clean stop and restart. Failure reports show the last successful `getUpdates`, never the bot token. |
| Claim operation refusals | The claim operation refuses an unknown member, delivery, contact method type without a claim method, or `claimedHow` |
| Status page | Passed on macOS ARM64 (2026-10-27) against `internal/fakecloudflare`, never a real Cloudflare account. R2 is Cloudflare's object storage. The R2 request signer matches AWS's published Signature Version 4 example. Set-up checks pass for the bucket, host name, rewrite rule, unchanged rerun, and preservation of the zone's other rules. A key is saved only after publishing with the key. |
| Status page commands | Posts, updates, and resolves pass from the incident page, the Status page page, the command, and a separate `mmhotline` process with only env vars. Two concurrent posts are kept |
| MMHotline-down alarm | Passed on macOS ARM64 (2026-10-27): the Worker's 15 tests under `node --test` (Node.js 23.11) with fake storage and an alarm. The Go side passed against `internal/fakecloudflare`, never a real Cloudflare account. |
| Local MMHotline-down alarm restart proof | A local proof ran the built `mmhotline serve` against the real `mmhotline_down_alarm_worker.js` hosted by Node.js with fake storage and a clock moved forward. The check-in stored Barbara's Pushover notification for delivery to `https://api.pushover.net/1/messages.json` after 600 seconds. After `serve` stopped, the alarm sent the notification 10 minutes after the last check-in. After a restart, the first check-in's answer listed the send, and MMHotline recorded the all-clear. The stand-in recorded the request and sent nothing |
| Phone calls | Passed with a real Twilio account and a real phone ([real sending services on a phone](#real-sending-services-on-a-phone)). The owning [notification design](design/notifications.md#twilio-phone-calls) specifies setup, spoken calls, polling, pacing, and failure fallback. |
| Public integrated workflow | Alert source, contact method, signed SOS, simulated email acceptance, concurrent claim conflict, resolution, and restart pass |
| Custom schedule code | Schedule correctness checks pass for the `at` fields and helpers, every result rule, and test cases. The checks cover 14 days of quarter hours across New York's spring and autumn daylight saving transitions. Language restrictions pass: no `load`, `while`, recursion, clock, or randomness, with frozen globals and discarded `print` output. Step, time, and (on Linux) memory limits pass, and the parent validates the child's answers. |
| Custom schedule execution and history | Checks pass for bounded concurrent sandbox runs. Checks pass for 20 kept versions, hourly runs, member removal, failure and all-clear notices, shifts running out, and discarding an outdated run. The public editor workflow passes. |
| Custom schedule lockdown on Linux | On a Linux AMD64 server (Go 1.26.8, 2026-10-01), opening a file, opening a socket, and starting a program each kill the child with `SIGSYS`. Every custom schedule test passes under seccomp |
| Triage agent sign-in checks and token reminders | The triage agent runs the team's Claude Code or Codex to investigate alerts on the team's server. Checks passed on macOS ARM64 (2026-10-27) with the fake sandbox and a controllable clock, for the triage agent and the review agent. Reminders pass at 30 days and 7 days before a Claude Code subscription token's year ends, not a second early, to administrators only. Reminders never repeat after a restart, and overdue reminders run at the first team check after a restart. A new credential restarts reminder timing. |
| Codex access token and ChatGPT sign-in | Passed on macOS ARM64 (2026-11-08) against a fake `codex`, never a real token or sign-in. Through the real run inside the sandbox, the fake `codex` reads the lent `$CODEX_HOME/auth.json` (mode 0600), rewrites it after a delay as Codex's refresh does, and is killed halfway through a later write. MMHotline saves the copy the setup test gave back, lends each run the newest copy, keeps it through a change of only the model, saves the refresh sent while Codex still runs, refuses the half-written file, keeps the last whole copy, and leaves no `auth.json` behind. Returned copies for another account, user, or login, with an older `last_refresh`, broken, over 64 KiB, with an added API key, or in another `auth_mode` are refused and named without their content; no output shows a token. The settings check and `settings apply` refuse a review agent sign-in that is the triage agent's. Access token reminders pass at 30 and 7 days before `credential_expires_on`, and a 20-day token gets only the 7-day reminder. The sign-in alert names OpenAI's refresh failures in plain words. The proxy allows `api.openai.com` only for an API key, and `chatgpt.com` and `auth.openai.com` only for an access token or ChatGPT sign-in. Each of these tests was checked by removing the feature code it covers and seeing the test fail. |
| Agent sign-in check timing | Daily checks skip after a successful run within 24 hours, but not after a failed run. Repeated failed checks produce one warn alert, followed by one info alert when a check passes. A sign-in check waits for a triage session, stops for a triage session, and runs again after the triage session. Each timing rule was checked by breaking the rule and seeing the corresponding test fail. |
| Agent sign-in command proof | The built `mmhotline serve`, with a fake sandbox socket, ran passing and failing `check-sign-in` checks for the triage agent and the review agent. MMHotline recorded the warn and info alerts, and served the triage agent page with each agent's sign-in. No real Claude Code token or Codex API key was used |
| Read-only commands | Passed on macOS ARM64 (2026-10-28, Go 1.27.3) through the command handler, with a triage session driven through the fake sandbox: a warn alert with no incident, found by severity and alert source and continued with `--older-than-alert-id`; incidents not resolved, and resolved incidents over a span; an incident with its alerts and its history with the triage agent's report; a session with two runs, each with its own token use, and their steps; a waiting approval request with its node tasks and playbook text, and its decision after an approval; unknown IDs and alert sources refused; the database's change count unchanged by every read. A local `mmhotline serve` with the fake sending services ran every command after five signed alerts; in the local run, `triage-sessions` and `approval-requests` listed none, because the triage agent needs a Linux server with systemd |
| Documentation | On 2026-10-01, 426 local file/anchor links resolved in the checkout used for that check; `git diff --check` passed. The 2026-10-01 link check does not establish that links in the current build resolve. |

The sending service tests use local fixtures and fake sending services.
The sending service tests establish application requests and recovery
behavior. External acceptance
and mailbox receipt need separate evidence. The Linux build check above
cross-compiled Linux AMD64 and ARM64 executables. Cross-compilation alone
does not establish production operation.

## Release checks

The static-analysis and Go stress-test checks of the
[release gates](design/development.md#release-gates) ran on 2026-10-27, on a
development build with the fixes below. On 2026-10-28 MMHotline moved
to Go 1.27.3, the version MMX uses. MMX runs playbooks that configure the
team's servers.
The Go tests on macOS ARM64 and `scripts/static-analysis.sh` passed again
under Go 1.27.3, with govulncheck reporting no vulnerabilities.
On 2026-11-16, before the first release, `scripts/static-analysis.sh`
reported that MMHotline's code reaches GO-2026-6859, an HTTP/1 server crash
in `net/http`, fixed in Go 1.27.4. MMHotline moved to Go 1.27.4, and every
check then reported nothing except the reviewed exceptions.
The stress run below used Go 1.26.10; the
release checks run again on the release commit.

| Check | Result |
| --- | --- |
| `scripts/static-analysis.sh` | Passed on macOS ARM64 (Apple M1 Max, macOS 26.6.2), analyzing the Linux build with cgo disabled under Go 1.26.10. Tools: `go vet`, staticcheck v0.8.2, errcheck v1.30.0, exhaustive v0.15.0, NilAway `v0.0.0-20260918162853-acb8859b9031` with MMX's patch, and govulncheck v1.9.0. Reports contain only the reviewed exceptions listed in the [testing guide](testing.md#static-analysis-for-a-release). |
| Static analysis: fixed vulnerabilities | Raising the toolchain from Go 1.26.8 to Go 1.26.10 fixed 12 standard library advisories reached by MMHotline's code. Advisories included GO-2026-6809 (compromised intermediate certificates, reached by email's TLS) and GO-2026-6617 (HTTP/2 server crash). |
| Static analysis: code fixes | Unused code was removed: a list in `combinedAlerts`, five test helpers, and a test's unread page. Other fixes covered numeric HTTP status codes, an error variable's name, and a struct copy written as a conversion. The triage agent command's exit codes now name the `refused` outcome. Closing the systems repository lock's file already unlocks the lock, so the explicit unlock was removed. |
| Static analysis: accepted exceptions | NilAway's 22 reports concern values guarded in ways NilAway cannot follow. The 17 exception comments each give the reason for accepting the covered reports |
| `scripts/run-go-tests-under-stress.sh` | The ordinary-user run used a Linux AMD64 server: AMD Ryzen 5 3600, 12 threads, Debian 13, Go 1.26.10, and gcc 14.2. With the race detector, `CGO_ENABLED=1`, and `MMHOTLINE_TEST_MMX_PROGRAM` set, 541 tests ran 40 times, 4 runs at once, taking about 75 minutes. Each of three full runs found one failure, fixed before the next full run. The last full run used the final code except for the Telegram test fix, and failed only the Telegram test, once in 40 runs. The three tests that need root passed 40 runs by name as root on a second server. After each fix below, the affected test was rerun successfully by name. On 2026-11-16, before the first release, the same kind of server ran all 839 tests 40 times, 4 runs at once, with the race detector under Go 1.27.4, on a loaded server, in 5 hours 19 minutes. Three tests failed, each from the test, not the product: TestHeldWarnTheAgentCannotHandle (40 of 40 runs) changed the database outside a transaction, so the background loop never looked again; TestBackupCheckUploadsFailsWhenTheDatabaseIsAheadOfLitestream (10 of 40) wrote a capture and the database file within one file-system clock tick, so both carried the same date; TestFullQueueDrainsWhenMuteExpiryCannotEnqueue (10 of 40) waited 5 seconds for a round that takes about 4.4 seconds with the race detector. After the fixes, the three tests passed 40 runs, 4 at once. |
| Stress tests: command sockets | Two command-socket tests failed every time as an ordinary user on Linux. The command-socket tests now skip for an ordinary Linux user, as `TestCLISetupCommandsEndToEnd` does. |
| Stress tests: proxy connection | `TestTriageAgentRunEndClosesPendingProxyRequests` failed 33 of 40 runs because the test ended the run before the proxy registered the connection as pending. The test now waits for the pending connection, and passed 40 of 40 runs under stress. |
| Stress tests: restart fixture | `TestTriageSessionResumesAfterARestart` failed about once in 100 runs. The fake sandbox stopped retaining the run connection, so Go's garbage collector closed the connection and ended the run early. `triageRunFixture` now retains every connection until the test ends, and passed 240 of 240 runs, 8 at once. |
| Stress tests: custom schedule sandbox | The sandbox failed to lock down about once in 160 runs of `TestCustomScheduleVersionHistoryKeepsLast20AndSwitchesBack`, with "failed loading seccomp filter: permission denied". `no_new_privs` was set on one OS thread, but the filter loaded from another thread. The child now holds one thread while setting `no_new_privs` and loading the filter, and passed 480 of 480 runs by name, 8 at once. On the custom schedule sandbox test server, loading a filter from a thread without `no_new_privs` failed with exactly "failed loading seccomp filter: permission denied". |
| Stress tests: Telegram button data | `TestTelegramPressFromAnUnknownAccountOrTheWrongMessage` failed once because made-up button data could equal the real data. Replacing two characters with "AA" changed nothing when the random delivery ID began with "AA", about once in 4,096 runs. The test now changes one character to a different character |
| [`scripts/run-fuzz-tests.sh`](testing.md#fuzzing-for-a-release) | On 2026-10-28, a Linux AMD64 server ran 13 targets for 10 minutes each, with 4 workers, on a development build. The server had an AMD Ryzen 5 3600, 12 threads, 64 GB, Debian 13, and Go 1.27.3. Ten targets passed: FuzzAlertSourceRequestBody (6,728,089 inputs), FuzzAlertSourceSignature (6,924,662), FuzzCloudflareAccessToken (1,360,190), FuzzSlackInteractiveEnvelope (54,693), and FuzzTelegramUpdate (57,730). The remaining passing targets were FuzzTwilioCallAnswer (6,353,322), FuzzAgentCommitsBundle (30,305), FuzzFleetView (6,911,938), FuzzChecklistYAML (6,674,563), and FuzzCustomScheduleSandboxOutput (6,826,274). Three targets found inputs that caused failures. Each failure was fixed before the reruns, and each input was kept as a seed. On 2026-11-16, before the first release, the same server ran all 13 targets again for 10 minutes each, with 4 workers, under Go 1.27.4 on the build that the real sending services were checked with: all 13 passed and found no failing input. |
| Fuzzing: DNS API URL | FuzzMMXExplainOutput found that a Cloudflare DNS-records API URL containing a percent-escape such as `%10` in the zone ID passed the Change DNS records check. The check missed escapes using the usual encoding of the byte, and now refuses any escaped character in the path. The rerun passed 10 minutes with 29,866,250 inputs on the fixed build. |
| Fuzzing: repository path test | FuzzTriageAgentRequest found a test fault: the test treated an accepted path starting with the folder `.....` as outside the repository. After the test fix, the rerun passed 10 minutes with 6,955,452 inputs on the fixed build. |
| Fuzzing: notice matching test | FuzzStatusJSON found a test fault: the test matched notices by ID alone when two notices had one ID. After the test fix, the rerun passed 10 minutes with 15,335,040 inputs on the fixed build. |
| Fuzzing: refused SOS recovery | While the targets were written, FuzzAlertSourceRequestBody found an application fault that dropped an SOS. Refusing a wrong-type field, such as a numeric `sender_alert_id`, prevented whole-body decoding from returning the SOS severity. Without the severity, MMHotline opened no SOS of MMHotline's own for the refused SOS. The refusal path now reads fields independently, so a wrong-type field does not hide the severity |
| Release archives and their [install check](testing.md#release-archive-install-check) | On 2026-10-28, `scripts/build-release-archives.py` built version `0.1.0-rc.1` from a clean checkout of one development commit, using Go 1.27.3 on macOS ARM64. The build produced four archives and `SHA256SUMS`, and a second build of the same commit and version `0.1.0-rc.1` produced identical bytes. The archives target Linux AMD64, Linux ARM64, macOS AMD64, and macOS ARM64. Each archive holds one folder with `mmhotline`, `LICENSE`, and license files for Go and the 13 modules compiled into the archive's executable. `file` reported the Linux AMD64 and Linux ARM64 programs as statically linked. |
| Release archive installation on Linux | A fresh AMD64 test server (2 vCPUs, 4 GB) ran the `linux-amd64` archive's install check for each distribution. Distributions were Debian 12 (systemd 252), Debian 13 (systemd 257), Ubuntu 24.04.4 (systemd 255), and Ubuntu 26.04.1 (systemd 259). On every test server, `sha256sum --check` first printed `OK`. Then `./mmhotline install`, with web address `http://127.0.0.1:8480` and intake address `127.0.0.1:8484`, started the service (`active`). `mmhotline version` printed `mmhotline 0.1.0-rc.1 (…, linux/amd64, modified=false)`. |
| Custom schedule under the installed unit | On 2026-11-15, on a Debian 12 AMD64 test server (systemd 252) installed with `mmhotline install`, every custom schedule preview and apply failed with "failed loading seccomp filter: operation not permitted": the unit's `SystemCallFilter=@system-service` leaves out `seccomp`. The validation of 2026-10-01 had run before the filter arrived on 2026-10-23. With `SystemCallFilter=seccomp` added to `mmhotline.service`, the same server previewed and applied a custom schedule, refused a failing test case, a syntax error, an unknown person, and an unknown key with their messages, and kept the earlier schedule after a failed apply. Step 5 of the [install check](testing.md#release-archive-install-check) now repeats this on every distribution. |
| Release archive extraction and signed alert | Unpacking the archive with `tar --strip-components=1` omitted the wrapping folder, as MMX's `unpack_without_wrapping_folder` does, and gave a program equal to `/usr/local/bin/mmhotline`. A signed SOS from the Python sender in Connect a tool that sends alerts got a 200 response with `mmhotline_alert_id`. |
| Release archive on macOS ARM64 | On macOS 26.6.2 ARM64, `shasum -a 256 --check` printed `OK` for the `macos-arm64` archive. The macOS ARM64 program printed `mmhotline 0.1.0-rc.1 (…, darwin/arm64, modified=false)`. The macOS ARM64 program's `status-page show`, `post`, and `resolve` published and resolved a notice against the Go tests' fake R2. |
| Release archive execution limits and cleanup | The `linux-arm64` program was inspected with `file` only, and the `macos-amd64` program was not run. The test servers were deleted afterwards and checked by ID |

## Sign-in and installation

The sign-in and installation checks ran on 2026-10-27: unit and command tests on a Mac running
macOS ARM64, one browser check on the same Mac, and one real Linux server.

| Check | Result |
| --- | --- |
| Cloudflare Access token | Against a local keys server, tests pass for RS256 signatures, issuer, audience, expiry, not-before times, and the email claim. The issuer identifies the team's Cloudflare Access domain, and the audience identifies the Access application. The email claim is the token field containing the person's sign-in email address. Time checks allow 60 seconds. Malformed and oversized tokens, key rotation, and unavailable keys pass. Signing keys are fetched hourly, with at most one fetch a minute |
| Page sign-in | A verified Access token passes. A sign-in link works once, for 10 minutes, is stored as a hash, sets a 12-hour cookie, and ends when the person is removed. The 401, 403, and 503 pages pass, as do cookies without `Secure` for a link opened at `http://localhost` |
| `mmhotline serve` | A real `serve` process runs with the web and alert-intake listen addresses and the Access settings. The process accepts a sign-in link and an Access token on the web listener, and a signed alert on the intake listener |
| Alerts with a localhost web URL | With the web URL `http://localhost:8480`, an SOS reaches an outgoing webhook with its incident link on that address |
| Install | Against a temporary root with fake `groupadd`, `useradd`, and `systemctl`: the generated units, a missing settings file or one with a problem refused before any change, the settings file given owner root, group `mmhotline`, and mode 0640, an existing `mmhotline` group kept, and unchanged reruns pass |
| Sign-in link in a WebKit browser | A sign-in link for a local `mmhotline serve`, opened in cmux's WebKit browser at `http://localhost`, signed in. The dashboard and the People page opened, and a form posted with the sign-in cookie saved. The local WebKit sign-in check covers cookies without `Secure` for loopback addresses, because WebKit keeps no `Secure` cookie on `http://localhost`. The browser and `serve` ran on the Mac, with no SSH forward or Cloudflare tunnel |
| Real Linux server | A Debian 13 AMD64 test server ran a development build. A refused install created no file. The first successful install created the users, units, and database. The service started under systemd restrictions, with pages on `127.0.0.1:8480` and the intake listener on the server's own address. A rerun changed nothing. |
| Linux sign-in through the Mac | On the Mac, `ssh -L 8480:127.0.0.1:8480` forwarded the Linux server's web listener. On the Linux server, `mmhotline people create-sign-in-link` created a link. Opening the link in cmux's WebKit browser on the Mac signed in. The browser opened the dashboard and People page, then posted a form using the saved sign-in cookie. Reopening the used link showed "already used or has expired". |
| Linux intake and real signing keys | The Linux server's intake route answered `404` on the web port. A signed alert to the intake listener got `200` with a signed receipt. With the team domain set to public `test.cloudflareaccess.com`, the service fetched Cloudflare's real signing keys from inside the systemd restrictions. A token naming a real key ID, but signed by another key, failed with "signature verification failed". A token with an invented key ID failed with "unknown signing key". The server was deleted afterwards |

### Through a real Cloudflare tunnel and Access application

On 2026-10-27, a Debian 13 AMD64 test server ran a development build behind a real `cloudflared` 2026.10.5 tunnel, with a public host name
routed to `http://127.0.0.1:8480` and a real Access application that allowed
one email address.

| Check | Result |
| --- | --- |
| Before the Access application existed | A request through the tunnel reached MMHotline and got its 401 "Sign in through Cloudflare Access" page; a packet capture on the loopback port saw it |
| No sign-in, through Cloudflare | With the Access application in place, Cloudflare answered with a redirect to its login page, and the packet capture on port 8480 saw no packet, so the request never reached MMHotline |
| A forged `Cf-Access-Jwt-Assertion` header, through Cloudflare | Without a Cloudflare session, Cloudflare redirected to its login page and nothing reached MMHotline |
| A token Cloudflare signed | The login-redirect token carries the application's audience tag, but no issuer or email. A separate Python check confirmed the team's real key signed the login-redirect token. Sent through an SSH forward, the login-redirect token was refused with "issuer does not match". MMHotline checks the issuer only after fetching the signing keys and verifying the signature. Reaching the issuer error therefore establishes that MMHotline fetched the team's real keys and accepted Cloudflare's signature |
| Forged tokens, through an SSH forward | A token naming the team's real key ID with the right issuer, audience tag, and email, but signed by another key, failed with "signature verification failed"; an invented key ID failed with "unknown signing key" |
| No token and no cookie, through an SSH forward | The 401 page, naming the public host name and the sign-in link command |

The server, the DNS record, the Access application and its policy, and the
tunnel were deleted afterwards.

A person's sign-in was checked on 2026-10-27 on the MMHotline server of a
three-server test setup with a separate database server, running a
development build behind its own tunnel and Access application. A person signed in to Cloudflare Access with an email code. With
the person's sign-in email address not yet a member, MMHotline answered
with the "Not a team member" page naming the address. The response shows
MMHotline read and verified the sign-in email address in Cloudflare's token.
The journal had no failed-token line. After `people
add` with the person's sign-in email address, a reload opened the dashboard
with no new sign-in.
The 401 for a token without an email, and the refusal after a changed
audience tag, are covered only by the verifier's tests.

## The settings file on Linux

On 2026-10-28, a fresh Debian 13 AMD64 test server ran a development build as root, following a deploy tool's order: create the group
`mmhotline`, write the settings file with owner root, group `mmhotline`, and
mode 0640, then install. The settings file had the server keys, a fleet view
command, one person, a weekly rotation, rules for running the agent's playbooks, the list of services
whose changes a person must approve, and one alert source; no agent, sending service,
MMHotline-down alarm, or status page.

| Check | Result |
| --- | --- |
| Install without the file | Refused with where the file goes; no state directory and no `mmhotline` user were left |
| `settings check` | The file had no problems; a copy whose rules for running the agent's playbooks left out `check_only` printed the note "rules_for_running_agent_playbooks.production: check_only has no rule; it needs a person until you add one" and exited 0 |
| Install | Succeeded, kept the existing group with the `mmhotline` user in it, left the file root:mmhotline 0640, wrote no `mmhotline.env` and no unit that reads one; the service started |
| `settings apply`, nothing changed | Every part unchanged, exit 0; `settings show` exit 0 |
| A new team name | Applied, with no restart |
| New `web_url` and `web_listen_address` | `apply` printed "Restarting MMHotline, because these keys change only when it restarts: web_url, web_listen_address.", a new process answered on `127.0.0.1:8481`, and the rest applied |
| A file every user can read | `apply` refused mode 0644 and printed the corrective `chown` and `chmod` commands. Restarting with the settings file still at 0644 kept MMHotline running with the earlier settings and the alert intake listening. After the settings file returned to 0640, `apply` worked |
| The team's alert source ID | A signed alert to `/alert-intake/monitoring` got `200` with its `mmhotline_alert_id` |
| The fleet view | Reading `/run/mmhotline-fleet-view.sock` returned the output of `fleet_view_command` from the settings file |
| Removed commands | `people add`, `schedules save`, `agent-playbook-rules set`, `alert-sources add`, `triage-agent set-up`, `review-agent set-up`, `sending-services set-up`, `down-alarm set-up`, and `status-page set-r2-access-key` each failed with the command group's usage line |
| Secrets | The alert source's signing secret appeared neither in the service's journal nor in `settings show --json` |

The settings-file test did not run the parts with an outside test.
The Go tests cover the parts with an outside test against fake services and
fake coding agents. The settings-file test also did not check the
administrators' warning after the fallback, which a Go test checks.
The server was deleted afterwards.

## Browser checks

The cgo-disabled application ran on an owned loopback HTTPS endpoint with a
local test certificate. cmux WKWebView checks covered the
dashboard, On call and availability (all three schedule types, a Weekly
rotation preview, team mute, unavailability, and taking-on-call controls), Alert
sources with the full intake URL, Contact methods, Alert history, and an SOS
incident with its action history and notification list. The Python example in
`docs/how-to/connect-a-tool-that-sends-alerts.md` sent a signed SOS to the local HTTPS application.
A repeat returned `already_received: true`. Desktop 1280px and mobile
390px screenshots were
inspected; the mobile document width equalled its viewport. Wording problems
found in the cmux WKWebView page and screenshot checks were fixed, and the
pages were checked again. A complete
native-keyboard/assistive-technology audit has not run. On 2026-10-01,
cmux WKWebView page and screenshot checks covered the Custom schedule
editor with the fake sending services: the prefilled example, Preview
assignments, Save and activate, the stored shifts, and "MMHotline knows who
is on call until", at 1280px and 390px; the test case table scrolls inside its
card at 390px. No real sending service
contact method was contacted during the page and screenshot checks.

## Triage agent on Linux

The triage-agent setup and sandbox checks in the following table ran on
2026-10-26 on a Debian 13 test server. The triage agent's
code and its generated units have not changed since; install's sign-in and
settings steps have. The fence is the operating-system sandbox around
each agent run. The setup and sandbox checks establish the
[triage agent setup and fence](design/triage-agent.md),
with the real-service limits stated below. The setup and sandbox checks do
not establish alert handoff to the agent.

| Check | Result |
| --- | --- |
| Linux installation | `sudo mmhotline install` created the `mmhotline-triage-agent` user, its home, and the run socket and service units |
| Fence probes inside a real run | A fake `codex` ran probes under the agent's own user. MMHotline's database, settings, and runtime directory were inaccessible. The system was read-only with a writable agent home. DNS and direct connections were unavailable. The proxy refused an unlisted host. Only the run's own processes were visible, and sudo was blocked |
| Real Codex with an API key | Setup test passed in 11 seconds through the fence. Of Codex's attempted destinations, only `api.openai.com` was allowed; optional hosts were refused and recorded |
| Claude Code with a dummy API key | Reached only `api.anthropic.com` and failed with “Invalid API key”; the saved setup stayed unchanged |
| Any-site internet access | `any_site` allowed Codex's optional hosts at public addresses |
| Run unit restrictions | `systemd-analyze security` rated the run unit 1.1 (“OK”) |

A Claude Code run with a real credential and a real coding agent given the
settings file's model and reasoning effort were proved on 2026-11-15, as
[real coding agents on a test server](#real-coding-agents-on-a-test-server)
says. Stopping a run has not been proved on a real system. Reasoning effort controls how much the selected
coding agent thinks before answering. Stopping a run, and the model and reasoning effort
on each coding agent's command line, are covered by tests.

On 2026-11-07, a development build, which runs the review agent as
its own Unix user, ran on Debian 12, Debian 13, and Ubuntu 24.04 test
servers. `sudo mmhotline install` created `mmhotline-review-agent` and its
run socket, and a second run changed nothing. `systemd-analyze verify`
accepted every unit. Stand-in triage and review agents ran through the real
run sockets with dummy credentials. From the triage run, the review run's
process was invisible in `/proc`, and every read of its environment, its
private `/tmp` home, its working directory, and its memory failed; nothing
could be planted in its home. A check run as root confirmed that the
review's credential was in its environment, so the probe could have
found it. A symbolic link planted while install changed file owners was
refused every time it reached install, about 19,500 times in one counted
run, and the target file never changed.

### A triage session end to end

On 2026-10-27, a Debian 13 AMD64 test server ran a development build
of MMHotline. The build handled real alerts with real Codex 0.164.0
and an API key.
The test used a reference setup's `run_mmx` and `apply_new_commits`
scripts, and `mmx` from MMX's main branch.

The systems repository's single inventory listed the MMHotline server, so the test managed the
server running MMHotline. A dummy interface gave the MMHotline server the private
address `10.20.0.1` in place of WireGuard. The repository's origin was
reached over SSH.

| Check | Result |
| --- | --- |
| A session from alerts | A triage session groups the alert investigation, while a run is one invocation of the coding agent. A warn alert started a triage session and a run in the same second, and the quiet start message went out (since 0.7.0 it is off unless `triage_agent.send_a_message_when_the_agent_starts_looking` is true). Later alerts resumed the same Codex session |
| Reading the server | The agent read the MMHotline server's journal as the `triage` user over SSH through MMHotline's logging proxy. MMHotline wrote the `~/.ssh/config` used for the SSH connection. The proxy recorded the server's port 22 allowed at `10.20.0.1`. The `triage` user's key allowed connections only from `10.20.0.1`, and was refused from `127.0.0.1` |
| Commits | The agent's signed commits reached the origin through MMHotline's push filter. All 12 commits on the origin verified as signed |
| A fix that waits | A one-off is an MMX playbook written for one fix, containing tasks to run on the server. A one-off written as a `shell` task was judged an unrecognized task and waited for a person under "Routine fixes without permission" |
| A fix that runs | For an SOS about a crashed service, the agent committed a `systemd_service` one-off. MMHotline classified the one-off as "Start or restart a service" with `mmx explain`. MMHotline ran the one-off with `./run_mmx --only-at-commit` from inside MMHotline's service (MMX over HTTPS, `script` with a terminal, `git push` over SSH). MMHotline answered "succeeded", then the agent checked the originally crashed service and reported |
| The SOS wait | The SOS was held for the triage agent and woke the person on call at 5 minutes, since the alert source had not resolved the SOS |
| Reports | Each report became an alert at its severity and was recorded in the history of every incident of the session |
| Two runs at once | Two connections to the run socket started two run units together. Another user's connection was refused |
| The credential | The API key was in no file in the agent's home, `/tmp`, `/var/tmp`, or `/etc`, and in no line of MMHotline's logs |

The 2026-10-27 end-to-end triage test found three faults, fixed after the
test: the task prompt named
`--inventory production` where the repository's inventory is `servers`; a
merge commit was made where a fast-forward was possible; and a resumed run's
token use, and with Claude Code its cost, was recorded as the coding agent
session's running total, with no count of cached input tokens.

## Real coding agents on a test server

On 2026-11-15, a Debian 12 AMD64 test server ran a development
build, with Claude Code 2.1.325 (model `opus`, reasoning effort
`high`, a Claude subscription token) as the triage agent, Codex 0.169.0
(model `gpt-6-astra`, an OpenAI API key) as the review agent, MMX from main
0180943c, and a systems repository with one inventory whose one server was
the test server itself, reached at its private address.

| Check | Result |
| --- | --- |
| Setup tests | The triage agent's test passed in 3 seconds for $0.046, reaching only `api.anthropic.com`; the review agent's passed in 11 seconds. `triage-agent check-sign-in` passed |
| Commit review | The agent approved an unsigned merge-button commit that changed a read-only playbook; asked for a person at warn when the rules file was missing; asked at sos for a commit whose author name differed from the Git host's earlier merges; a change to a `paths_only_a_person_may_approve` path went to a person with a warn at once. `mark-reviewed` and `trust-through` passed, and a repeated `trust-through` changed nothing |
| A triage session | A warn alert reached the agent, which read the server over SSH through the proxy, wrote and validated a one-time playbook, and asked to run it; the review agent approved it, MMHotline pushed the agent's signed commit and ran MMX, the service ran again, and the agent sent an Info report |
| Software review | Without `scheduled_software_review_task.md`, the review failed at once with the documented reason and no cost. With the software review guide's example task: 3 minutes 33 seconds, $0.99, one warn and five info reports, and a playbook request for security fixes, which waited as an unrecognized task and was refused on its page with a reason |

On 2026-11-23, on a Mac, Codex 0.171.0 (model `gpt-6-astra`, an OpenAI
API key) and Claude Code 2.1.330 (model `haiku`, a Claude subscription
token) each ran once with the options MMHotline passes (Claude Code without
`--effort`) in a new empty home.
Then everything in the home except `.codex/sessions/` or
`.claude/projects/` was removed, and each resumed its session and gave back
the word from the first run. Each run of a triage session clears the
agent's home this way
([what a run leaves for the next](design/triage-agent.md#what-a-run-leaves-for-the-next)).

The first runs of the session failed on setup the team's server setup owns,
and each failure named its cause: no `/etc/hosts` name for the server, the
agent's commits unsigned without its `~/.gitconfig`, MMHotline's copy
unable to push to an origin outside `/var/lib/mmhotline`, and MMX's node
runner missing. [Set up the triage agent](../how-to/set-up-the-triage-agent.md)
lists each step.

## The fleet view on Linux

On 2026-10-27, a Debian 13 AMD64 test server (systemd 257, Git 2.47) ran a
development build beside a real fleet hub of the monitoring tool.
A fleet hub supplies the monitoring tool's combined server and check results.
One check of a stopped service was failing.

| Check | Result |
| --- | --- |
| The command root set | `mmhotline-fleet-view@.service` ran, as root, the fleet view command that root set in a root-only file |
| The run's limits | A probe running as the command showed user ID 0 with every capability set empty, no new privileges, a system call filter, only the loopback interface, no IPv4 or IPv6 sockets, and a read-only file system; it could connect to the monitoring tool's root-owned socket of mode 0600, and not to MMHotline's own |
| Output over the socket | The `mmhotline` user read the whole fleet view as JSON through `/run/mmhotline-fleet-view.sock`, and the view's exit status 1 still counted as a successful run |
| The dashboard | Through an SSH forward and a sign-in link, the servers part showed each group red with the failing check, and green after the service started |
| Recent commits | `git rev-list --no-commit-header` printed the commit lines MMHotline reads |

The check found that `CollectMode=` sat under `[Service]`, where systemd
ignores it, so each failed run stayed as a failed unit; it moved to `[Unit]`,
and the same setting in a drop-in left no failed instance after three failing
runs. The server was deleted afterwards.

## Continuous backup to object storage

On 2026-11-15, on a Debian 12 AMD64 test server with Litestream 0.5.18 and a
Backblaze B2 bucket made for the run, a new server's install overwrote the
backup: with `backup.env` in place, install created an empty database and
started the backup, Litestream wrote the empty database as the bucket's
newest transaction, and the documented restore block brought back 0 of 26
alerts. The old state was still in the bucket only at an earlier
transaction. After the fix, on the same server, the whole route passed: the
old server backed up to a new folder and `backup check-uploads` exited 0; on
an empty state (a new server) install exited 1 with the restore and
start-empty steps and left the backup stopped; the restore block brought
back all 26 alerts; a second install started the backup, and
`backup check-uploads` exited 0. Starting empty in a new folder installed
and backed up with exit 0. Go tests cover a refused new database, a bucket
that cannot be listed, and a database a deploy tool restored before install,
which is backed up without listing the bucket.
A fact check of the first fix found that a second install on the
new server started the backup on the empty database, since the database
then existed. The fix keeps a marker file until the bucket check passes. On
the same server, a second install after the refusal refused again, the
restore and the next install brought back all 26 alerts with
`backup check-uploads` exit 0, and starting empty from a refused state in a
new folder installed with exit 0 and removed the marker. Go tests cover the
second install for both refusals.

On 2026-10-27, on a Debian 13 AMD64 test server, a development build
with Litestream 0.5.17 (archive digest checked) replicated `mmhotline.db` to a
Backblaze B2 bucket through B2's S3-compatible endpoint, set in
`/etc/mmhotline/backup.env` as the backup guide says. `sudo mmhotline
install` started `mmhotline-backup.service`, whose first snapshot reached the
bucket within seconds; a new person, an alert source, and a signed alert each
reached the bucket within about a second. Neither the MMHotline service's
journal nor the backup service's journal had a warning or an error.
`litestream restore` to a new path gave a database whose integrity
check passed and which held the person, the alert source, the alert, and its
incident, and `mmhotline serve` on a copy of it listed them. The running
guide's in-place restore ran as written; afterwards the backup resumed from
the replica. A database change made after the in-place restore reached the
bucket and came back in a second restore. Litestream's retention over days,
its later compaction levels, and other architectures were not run.

On 2026-10-28, [`sudo mmhotline backup check-uploads`](../reference/mmhotline-command.md#mmhotline-backup-check-uploads),
built for Linux AMD64 from the commit that added the command, ran as root and read
only on the MMHotline server of a 3-day load test, where Litestream 0.5.17
backed up to object storage while alerts arrived. `sudo mmhotline backup
check-uploads` exited 0 with
`MMHotline's backup holds every change Litestream captured, up to transaction 00000000000011aa.`
With `--json`, `sudo mmhotline backup check-uploads` exited 0 with
`uploads_keep_up` `true`, the newest captured
transaction `00000000000011aa`, and the newest uploaded `00000000000011ab`,
a change captured and uploaded while the check ran. Stopped uploads and
the cases where the check cannot determine upload progress ran only
against a fake `litestream` in the Go tests.
The [release gates](design/development.md#release-gates) run stopped-upload
and indeterminate-result checks on a real bucket when the Litestream
version changes.

On 2026-11-07, on Debian 13 and Ubuntu 24.04 test servers, with Debian 12 for
the first and third checks, a development build ran with Litestream
0.5.17 as the `mmhotline-backup` user, backing up to a local S3-compatible
server with dummy keys. Litestream's working folder was
`/var/lib/mmhotline-backup`, mode 0700, and a process running as
`mmhotline` could neither read it nor see Litestream's environment.
`sudo mmhotline backup check-uploads` exited 0 after writes. It exited 1
with "MMHotline's backup has stopped" about 30 seconds after a write in each
of these cases: when Litestream could not write its working folder, when
the backup service was stopped, and when the service was stopped and
MMHotline was then restarted cleanly, which folds the database's log into
the database file. It returned to 0 once the backup ran again. An upgrade
from the build before the backup user existed needed no manual step:
Litestream continued from the replica's newest transaction with no gap.
The backup guide's restore block, run word for word, gave a backup that
continued with no gap.

On 2026-11-12, on a Debian test server, Litestream 0.5.17 and 0.5.18
(archive digests checked) each replicated a new SQLite database to a local
file replica. Both gave the same `litestream ltx` columns (level, min_txid,
max_txid, size, created), the same captured file names under `ltx/0`, and the
same LTX header bytes apart from the timestamp, which is what `backup
check-uploads` reads. The same day, a test run on another Debian test server
replicated `mmhotline.db` with 0.5.18 to a Backblaze B2 bucket and restored
it; `PRAGMA integrity_check` passed. So the guide moved to 0.5.18.

## The MMHotline-down alarm in a real Cloudflare account

On 2026-10-27, a Debian 13 AMD64 test server ran a development build
with one administrator on call, reached by Pushover on a real phone, and set
up the MMHotline-down alarm in a real Cloudflare account on its free plan.

| Check | Result |
| --- | --- |
| Set-up | Uploaded the Worker with its SQLite-backed Durable Object at migration tag `v1`, set the check-in token as its secret, turned on its workers.dev address, and held the person's Pushover notification after 10 minutes. A Durable Object is Cloudflare-managed storage. The first check-in got Cloudflare's own 404 because the new address was not live yet; it was accepted 12 seconds later, and set-up now tries again for up to 30 seconds |
| Set-up again, and an upgrade | A second set-up re-uploaded the Worker already at `v1`, and a forced upgrade upload from the background loop did too; the secret survived both, and the next check-ins were accepted |
| Cut off | With outgoing HTTPS dropped on the server, check-ins failed. Ten minutes after the last accepted check-in, the Worker sent the held Pushover emergency alarm. The person's phone rang, and the person acknowledged the MMHotline-down alarm's Pushover emergency alarm |
| Back again | After the server reconnected, the check-in response listed the emergency send as delivered with HTTP status 200. MMHotline recorded and sent the all-clear |
| Test send | `down-alarm send-test` delivered the test alarm, HTTP 200 |
| Removal | Removing the set-up left the Worker holding nothing. The Worker, the Worker's Durable Object, and the Worker's address were then deleted |

That real-account run was before `down-alarm send-test` read Pushover's
receipt and cancelled the test's repeats. The receipt, the read, and the
cancel have run only against the fake sending services, and
`incidents send-test-sos` only against them too; neither has run with a real
phone.

The test server and every Cloudflare object were deleted afterwards and
checked by ID. The settings file's `mmhotline_down_alarm` part now starts MMHotline-down alarm set-up
and removal. The settings-file set-up and removal path, including putting
back the earlier setup after a failed first check-in, has not run in a real
Cloudflare account.

## The status page in a real Cloudflare account

On 2026-10-27, the status page ran in a real Cloudflare account with R2,
from a Debian 13 AMD64 test server and a Mac. The first run, on a development
build, set up the page and then failed every post: Go asked R2 for gzip,
R2 answered with a weak ETag, `W/"…"`, and refused it in `If-Match` with
HTTP 412. The rerun on the fixed build, which reads `status.json` with
`Accept-Encoding: identity`, passed.

| Check | Result |
| --- | --- |
| Set-up | Created the bucket, connected a test host name on the team's domain, and added the one URL rewrite rule. Cloudflare activated the certificate and verified domain ownership about a minute later. The bare address served `index.html` |
| R2 key | A first post using S3 credentials derived from the Cloudflare API token published "Everything is working." before MMHotline saved the R2 access credentials |
| Post from the server | Succeeded on the first try; the public page showed the notice with its time in UTC, and `status.json` held it |
| Post from a laptop | The macOS build, with only the three `MMHOTLINE_STATUS_PAGE_R2_*` variables, added an update to the notice posted from the server, straight to the bucket |
| Resolve | The page went back to "Everything is working." with the notice under "Recently resolved" |
| ETags | Read with `identity`, R2 gives a strong ETag, the body's MD5, and accepts it in `If-Match`; read with gzip it gives the weak form, which `If-Match` refuses; each upload answers with a strong ETag |

The bucket and its objects, the custom domain with its DNS record, the
rewrite rule and the ruleset set-up had created, and the test server were
deleted afterwards and checked by ID. The bucket-only R2 token the running
guide describes was not made; the proof used credentials derived from the
API token. The set-up now runs on a laptop, and the settings file's
`status_page` part publishes the first page; neither has run in a real
Cloudflare account.

## Real sending services on a phone

On 2026-11-16, a Debian 12 AMD64 test server ran a development build of
0.1.0 with real Pushover, Telegram, Slack, and Twilio accounts, and one
team member reached on a real iPhone. The server made only outgoing
connections. A test sender sent signed SOS alerts to the alert intake. Each
check below was confirmed both by what MMHotline recorded and by the person.
NAME stands for the member's name in MMHotline's recorded text.

| Check | Result |
| --- | --- |
| Connect | Telegram through the start link on the iPhone, and Slack through the connection code in Slack's web client on a Mac. Each sent its test message. Pushover's test arrived. Twilio checked the calling number and placed the test call to a Romanian mobile number (Twilio's voice dialing permissions allow Romania). |
| Silence Unknown Callers | Before the contact card was saved, the iPhone's **Silence Unknown Callers** sent the first test call to voicemail without ringing, and Twilio reported it as answered. [Connect Twilio](../how-to/connect-twilio.md) now says that saving the contact matters for this setting too. |
| Calls through Do Not Disturb | With the downloaded contact card saved, allowed through Do Not Disturb, and with Emergency Bypass on, a test call rang with Do Not Disturb on. |
| Pushover emergency alert | With Silent Mode on, the SOS's emergency-priority alert sounded. Acknowledging it in Pushover claimed the incident 14 seconds after it opened ("Claimed by NAME, who acknowledged the alert in Pushover"), and the SOS also reached Telegram, Slack, and a call. |
| Telegram buttons | Pressing Claim recorded "Claimed by NAME in Telegram". |
| Slack buttons | In Slack's web client, Claim recorded "Claimed by NAME in Slack" twice, and Resolve on an unclaimed SOS recorded "Resolved by NAME in Slack", 5 seconds after the SOS opened. |
| Resolve after a claim | Added after this check: a claimed message in Telegram or Slack keeps Resolve beside its state. Tests with the fake Telegram and Slack cover it; it was not pressed on a phone. |
| Repeating calls | The first call of an SOS rang out unanswered. MMHotline contacted the person again after one minute, and the person answered the second call, which read the alert. A claim in Telegram 7 seconds later stopped the calls: no call followed in the next two minutes. |

Limits: Slack was tested in its web client only. With voicemail on,
Twilio reports an unanswered call as answered, so the **No answer** result
was not observed; the guide says that **Answered** includes voicemail.
Email was not part of this check; see below.

## Remaining acceptance

Real email acceptance has not been verified. A check needs SMTP
settings, an allowed sender, and a designated recipient. Mail-server acceptance and
actual receipt must be recorded separately.

The three subscription credentials, Claude Code's subscription token,
Codex's access token, and Codex's ChatGPT sign-in, follow the vendors'
documented behavior: Claude Code's `claude setup-token` documentation,
OpenAI's Codex authentication, access token, and CI/CD authentication
guides, and Codex's source at tag `rust-v0.166.0`, the oldest Codex
version MMHotline supports
([supported coding agent versions](design/triage-agent.md#supported-coding-agent-versions)). They are proven against a fake `claude` and a fake `codex`
that act as those documents describe, not with a real token or sign-in. The
sign-in check has not run with a real Anthropic or OpenAI API key either.

Production capacity has not been validated.

Installation from the release archives was checked on a commit before the
release, with a release candidate's version. The
[release checks](#release-checks) run again on the release commit.

Sender alerts and check-ins have been tested through the public
interfaces with fixtures, not through an independently running monitoring
tool.

The [Planned list](design/README.md#planned) records changes still to be
implemented. Full release acceptance is not claimed.
