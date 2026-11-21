# Testing and acceptance

The [acceptance contracts](acceptance.md) define correctness.
The current build tests individual Go operations, database persistence, and
workflows through the public HTTP interfaces. The
[validation record](validation.md) states exactly which scope has passed.
There is no `tests/` directory and no VM or container suite yet. The
[Planned list](design/README.md#planned) owns planned product work.
An SOS is the urgent alert that wakes the person on call, even at night;
to claim an incident is to say "I'm on it", which stops MMHotline
contacting the next person.

## Contract map

| Contract | Focused tests | Public workflow |
| --- | --- | --- |
| A01 People/access | Cloudflare Access token checks and key fetching, sign-in links and cookies, member roles, at least one administrator and unique sign-in email addresses in the settings file's people, a schedule that names only listed people, cancellation and history after person removal, CSRF lifetime, immediate contact method use, automatic save tests, last test results, Send test and phone number limits | Setup creates one team and no person; the settings file's people part adds, changes, and deactivates people; people list and create-sign-in-link over local sockets, a sign-in link and an Access token on the running web listener, the read-only People page, 401/403/503 pages, loss of page access on the next request after removal, reject forbidden direct requests |
| A02 Ingestion | Schema, bounds, normalization, `happened_at`, signatures and rotation, receipt signatures, duplicate and correlation rules, per-source and installation-wide limits with the SOS exemption | Verify signed requests on the two source-ID routes of the intake listener, the routes' absence from the web listener, and 404 with extra path segments; send signed generic alerts, including shared incident keys and a single resolution, with replay and resolution cases; restart and inspect receipts |
| A03 Routing | Severity subscriptions, suppression, `send_while_unavailable_or_muted`, contact method state, contact method type facts, delivery throttle and combined messages, webhook signatures and egress; sending service results arrive over MMHotline’s own authenticated polling and Socket Mode connections; repeated and late reports change nothing | One alert reaches Pushover notification, automated voice call, Slack message, Telegram message, and outgoing webhook contact methods through fake sending services; email uses local SMTP fixtures and the optional Mailpit check; resolved-sos produces no call; a burst of Warning alerts arrives as combined messages; SOS bypasses the ordinary throttle; chat SOS flood handling is checked separately |
| A04 SOS lifecycle | Legal transitions, revisions, ownership/audit; the test SOS (`internal/app/test_sos_test.go`: `TestTestSOSWakesThePersonOnCallAtOnceAndSkipsTheTriageAgent`, `TestClaimingTheTestSOSResolvesIt`, `TestANewTestSOSClosesTheUnclaimedEarlierTest`; `cmd/mmhotline/send_test_sos_test.go`: the claim output and exit 0, exit 1 with the advice, the time limit flag) | Create, claim/reclaim, escalate, resolve from two browsers |
| A05 Escalation | Controllable time, the default and a changed time to claim, eligibility, call overlap, one call about several incidents, voicemail pacing, one/no responder, team mute | Unanswered primary reaches backup/fallback when the primary's time to claim ends; a changed time to claim applies only to steps starting after the change; calls direct responders to Pushover or MMHotline; a Pushover acknowledgement or a Slack or Telegram button claims exactly the incidents the notification was about and cancels pending repeats; no-coverage fallback respects team mute |
| A06 Schedules | Boundaries, timezone/DST policy, fallback, config preservation, stored custom schedule shifts | Preview/save By day of the week, By hour of the day, Weekly rotation and Custom schedule; switch away and back; observe a handoff |
| A07 Availability | Expiry, preserved unavailability, explicit replacement of whoever took on-call, and conflicts, unchanged incident ownership | Set availability, team mute and taking on-call through two browsers; unavailability survives team unmute; unmute schedules current work once |
| A08 Watching from outside | Warning and SOS times after the last check-in a sender sends, planned downtime by hand and from sender-listed maintenance windows, automatic resolution of an unclaimed SOS, correlation | A sender stops checking in, an SOS opens, and check-ins resuming resolves the SOS, across a restart. Planned downtime prevents missing-check-in alerts. The SOS follows after planned downtime ends if check-ins are still missing. |
| A09 Team logic | Signed alert intake and webhook boundaries; Starlark limits, output validation, preview/live agreement, sandbox lockdown, immediate activation after validation and the last 20 saved configurations | Run the team’s logic on its own systems through signed alerts and outgoing webhook contact methods; save and activate valid custom schedule code immediately; refuse failed previews and test cases while keeping the current schedule active |
| A10 Recovery | Failed commits, stale completions, sends without a known sending service outcome, overdue steps/repeats, retained ordinary deliveries | Restart at admission/dispatch/result boundaries; advance an expired step with a fresh time to claim, avoid missed-repeat bursts, and preserve ordinary pending work |
| A11 Interface | Input retention, escaping, role actions, date/filter semantics | Keyboard/browser navigation and concurrent-browser updates on desktop/mobile widths |
| A12 Builds/operation | cgo-disabled dependency/build checks; setup creates only `mmhotline.db`, saves the settings file's team name, and refuses existing state; opening the store requires `mmhotline.db` to be a regular file and contain exactly one `team_info` row; serve refuses a missing `--settings-file` and, with nothing applied yet, a settings file with problems; transaction rollback and commit-only scheduler notifications | Build artifacts, setup one team, start serve with a settings file's web and alert intake listen addresses, Cloudflare Access settings, and command socket, peer checks, and sending service credentials, diagnose, and stop cleanly |
| A13 Installation/backup | Pure generated-unit checks; refused prerequisites with remedies; a missing settings file, or one with a problem, refused before any change; the settings file given root, group `mmhotline`, and mode 0640; repeat installs preserve files and make no change calls; a first install and its rerun run no programs but `groupadd`, `useradd`, and `systemctl`; upgrades restart; backup requires backup settings and the Litestream program; Litestream's working files outside the state directory, in the backup unit's state directory, as `litestream.yml` sets them and the check reads them; readiness timeout includes journal lines; `backup check-uploads` against a fake `litestream` and captured files of set ages: caught up, 30 seconds behind, a change missing for 12 minutes, an empty backup, a list of another level with its columns in another order, nothing captured, a real SQLite database ahead of the captured change's LTX header (exit 1), a capture of a log with other salts (exit 1), a capture made during the wait (exit 0), a log copied into the database file after the newest capture (exit 1), and exits 3 for a failed list that never shows a credential, a missing `backup.env`, a missing `litestream`, and a list without `max_txid`; the database format: `TestADatabaseOfEachFormatUpgradesToTheSetupSchema` (a database built from format 1 in `internal/app/testdata`, with rows, opens with a new database's tables, columns, indexes, and triggers and keeps the rows) and `TestOpenRefusesADatabaseItCannotUpgrade` (no format number, and a newer format) | On Linux, install twice, upgrade the binary, save a custom schedule under systemd hardening, sign in through Cloudflare Access over a real tunnel and with a sign-in link over an SSH forward, and try the documented Litestream restore with real replica uploads |
| A14 Restart recovery | Restart recovery of notification work saved in the database, fresh missing-check-in times for recently heard sources, retained times for sources already marked silent | Restart with unfinished deliveries; confirm eligible saved work resumes, recently heard sources get full Warning and SOS times, and stopped sources keep recorded times |
| A27 MMHotline-down alarm | The Worker's alarm, sends, resends, retries, refusals when stored, and token check under `node --test`; held messages, check-in timing, team mute, the all-clear, the failing alert, the upgrade upload | The settings file's `mmhotline_down_alarm` part against the fake Cloudflare API: set-up, a first check-in that keeps failing putting back the earlier setup, another account refused, and a section left out holding nothing first and refused while the Worker cannot be told; show and send-test through the command handler, the dashboard card and test button; `TestDownAlarmTestIsProvedFromPushoversReceiptAndItsRepeatsAreCancelled` (the receipt read once, the repeats cancelled, a voice call unchanged, and each failure: no receipt, a refused receipt, a failed cancel), the Worker's receipt tests under `node --test`, and the capitals line before a send (`TestDownAlarmSendTestWarnsBeforeItSends`) |
| A18 Commit check | `internal/systemsrepository`: `TestCommitsThatNeedReview` (trusted, triage notes, triage outside its folders, a triage root and merge, unsigned, unknown key, lost checkpoints, every changed path once), `TestATrustedMergeOfAnUnsignedCommitIsTrustedOnlyBeforeTheMerge`, `TestARunFromABaseBehindOriginGetsNothingNewerAndStillPushes`. `internal/app`: `TestMMXWaitsForEveryCommitNoTrustedKeySigned` (one warning, one review, then MMX runs), `TestMMXRefusesTriageSignedCommitUntilMemberReviews`, `TestMemberCanReviewRefusedCommitOnApprovalsPage`, `TestChangedKeysKeepTheCheckedThroughCommit`, `TestTheCheckoutStopsAtTheTrustedCommit`, `TestAWaitingInventoryChangeAddsNoSSHTarget`, `TestThePromptsNoteComesFromTheTrustedCommit`, `TestJudgingWhileACommitWaitsUsesTheTrustedCommitAndMMXWaits`, `TestTrustThroughVouchesForEveryEarlierCommit` (a removed key's and an unsigned old commit trusted, one info alert, a later commit still checked, a commit off the branch refused, a commit behind the checkpoint changing nothing). The triage agent's approval, in `triage_agent_commit_review_runs_test.go` with the fake sandbox and real signatures: `TestTheTriageAgentApprovesAnOrdinaryCommit` (no alert before the review, one run from the trusted commit with the commit quoted as data, approve records the agent and run, one info alert and nothing else, checkpoint and checkout move, MMX then runs, the page, Approvals list, Triage agent page, and `commit-reviews list`/`show` text and JSON), `TestCommitsTheTriageAgentMayNotApproveWaitForAPerson` (a person-only path and the triage key's own commit get the warning at once, never reach the prompt, approve refused naming the paths, playbook requests refused), `TestNeedsAPersonSendsTheAgentsReportAsTheOneAlert` (the report at sos with the review link, no warning, the commit waits), `TestAFailedCommitReviewSendsTheWarningOnce` (once, with the reason, also after a restart), `TestWithAPersonNoCommitReviewStarts` (today's warning, no run; a setting changed mid-review refuses approve and warns), `TestACommitReviewStopsForATriageSession`, `TestTheCommitReviewPromptQuotesTheDiffAndIsCut`; `internal/triageagent`: `TestAgentRequestCommandArgumentChecks`, `TestSandboxRelaysACommitReview`; settings: `TestEachSettingsProblemNamesItsKeyPath` | `TestARunStartsFromTheTrustedCommitWhileACommitWaits`: a run while an unsigned commit waits gets the trusted base, its copy lacks the commit, its commits reach origin, and MMX still waits. Fixture commits are signed with a fixed test key the team trusts; the agent signs with the triage key |
| A29 Read what happened | `TestReadOnlyCommandsShowWhatHappened` drives a triage session through the fake sandbox: a warn alert with no incident, two runs with steps and usage, the agent's report, and an approval request, then reads each through the command handler, with refusals of unknown IDs and alert sources, and checks that the database's change count did not move; `TestReadOnlyCommandFlags` checks each command's flags in the request and the refusals before connecting; `TestCommitReviewsCommands` checks `commit-reviews check`, `list`, `show`, and `mark-reviewed` through the command handler, with text and JSON giving the same facts, refusals of a missing commit and a name that is no commit ID, and an older commit leaving the checkpoint; `TestCommitReviewsCheckExitCode` checks the exit codes 0 and 1 | `TestCLISetupCommandsEndToEnd` lists a signed info alert with no incident and the empty incident list from a running `serve` |
| A30 Settings file | See [Settings file checks](#settings-file-checks) for validation, applying parts, secret handling, and sandbox requests. | See [Settings file checks](#settings-file-checks) for command and process workflows. |
| A26 Status page | Signer against AWS's example, `status.json` history and fields, page rendering in golden files, the page's local-time script under `node --test`, the Atom feed's structure and golden files, refusals, edits and deletes, posts at the same time, older `index.html` and `feed.xml` files that arrive last, a failed upload of one file, bucket names | The laptop's `status-page set-up` against the fake Cloudflare API; the settings file's `status_page` part, saved only after it published the current page with the file's key and `page_title`; posts from the incident page, the Status page page, the command, and a laptop process with only env vars |

## Settings file checks

The A30 checks cover validation, applying settings, and public commands.
The following files cover the A30 checks.

- `internal/app/settings_file_test.go` checks each problem's key path,
  unknown keys, invalid YAML, and the settings file's owner and mode.
- `internal/app/settings_file_apply_test.go` checks each part applying,
  failed parts keeping earlier state, and unchanged parts avoiding writes.
  The tests check the rule for an inventory without an entry,
  deactivation and refusal of deactivation, and an omitted alert source.
  The tests also check the response requiring restart, startup fallback
  with a warning, and refusal of the first start.
- `internal/app/settings_file_parts_test.go` uses the fake sandbox, fake
  sending services, an SMTP fixture, and `internal/fakecloudflare`.
  The settings-parts tests cover the following cases:
  - `TestSettingsFileAppliesTheAgentsThroughTheirSetupTests` runs each
    agent's setup test with the configured coding agent, credential, model,
    and reasoning effort. Unchanged parts run no test. A refused model
    preserves earlier setup. A token's year survives model changes and
    restarts with a new credential. An active triage run fails the part with a reason.
    Omitting an agent section removes the agent's setup.
  - `TestEveryAgentRunCarriesTheModelAndReasoningEffort` checks a software
    review, a triage session, the triage agent's sign-in check, the review
    agent's sign-in check, and a playbook review.
  - `TestSettingsFileAppliesEachSendingService` checks Pushover, Twilio,
    Telegram, Slack, and the mail server through each service's setup check.
    The tests check each service's next steps.
    Unchanged services run no check. A refused mail server preserves setup.
    Removing Pushover is refused while a contact method uses Pushover.
    Omitting unused Twilio, Telegram, Slack, and mail-server setups removes
    the omitted setups.
  - `TestSettingsFileAppliesTheDownAlarmAndTheStatusPage` applies the Worker
    and page from the file. Unchanged settings make no Cloudflare or R2
    call. A refused R2 key preserves the earlier key. Omitting `mmhotline_down_alarm`
    and `status_page` removes the respective setups.
  - `TestAnActionKindTheFileLeavesOutIsNamed` checks the note in check,
    apply, and show. The omitted kind has no saved setting. Listing the
    kind removes the note.
- `internal/app/down_alarm_test.go` and `internal/app/status_page_test.go`
  check MMHotline-down alarm setup, refusals, removal, and failed first check-ins
  restoring earlier setup. The status-page tests also check the Cloudflare
  setup function used on a laptop.
- `internal/app/settings_file_secrets_test.go` puts recognizable values in
  every secret key. The tests search outputs, pages, commands, stored
  failure reasons, and agent runs for secrets. The fakes refuse every
  marked value, and the fake sandbox quotes the credential back.
- `internal/triageagent/sandbox_test.go` records Claude Code and Codex
  command lines with `--model`/`--effort` or `-m`/`-c model_reasoning_effort`.
  The checks cover triage start (`TestSandboxFinishesWithStdinPrompt`),
  resume (`TestSandboxResumeArguments`), and review start
  (`TestSandboxPassesModelAndReasoningEffort`). `TestSandboxInvalidRequests`
  refuses missing or malformed model and effort values.

The public command and process checks for A30 are:

- `cmd/mmhotline/settings_test.go` checks `settings check` exit codes,
  output, and notes. The file tests `settings apply` against a fake command
  socket, replacing the restart function with a test function.
  A command result containing only a note still exits 0.
- `cmd/mmhotline/install_test.go` checks install refusing a missing
  settings file.
- `cmd/mmhotline/status_page_test.go` runs
  `TestStatusPageSetUpFromALaptop` against the fake Cloudflare API,
  without a command socket.
- `cmd/mmhotline/command_test.go` checks that removed commands are refused
  as unknown.
- `cmd/mmhotline/main_test.go` and `cmd/mmhotline/triage_agent_test.go`
  start a real `serve` process. Startup applies the sending services,
  triage agent, and review agent from the settings file.

## Status page checks

`internal/statuspage` checks the R2 request signer against the GET Object
example in AWS's Signature Version 4 documentation.
The package tests the following behavior against
`internal/fakecloudflare`'s R2 bucket:

- Posting, updating, and resolving notices preserves the whole history and
  field names in `status.json`. The page uses absolute UTC times and
  escapes text.
- Empty and over-long messages, updates to resolved notices, and posts
  before setup are refused.
- A second post changes `status.json` between the first post's read and
  write. The saved history keeps the first and second posts
  (`TestPostsAtTheSameTimeAreBothKept`).
- A second post completes between the first post's save of `status.json`
  and arrival of the first post's older `index.html`. The final page uses
  the latest `status.json`
  (`TestPageShowsTheLatestStatusJSONAfterPostsAtTheSameMoment`).
  A second post that completes between the uploads of `index.html` and
  `feed.xml` leaves both files matching the latest `status.json`
  (`TestPageAndFeedShowTheLatestStatusJSONAfterAPostBetweenTheirUploads`),
  and a post that keeps being overtaken says so and keeps the change
  (`TestAPostThatKeepsBeingOvertakenSaysSoAndKeepsTheChange`).
- A failed upload of `index.html` does not stop the upload of `feed.xml`, and
  the reverse; the error names the file that failed and says the change is
  saved in `status.json` (`feed_test.go`).
- `feed.xml` is read back as Atom 1.0: the feed's ID, title, author,
  updated time, and links, an empty feed, entries with the state in the title,
  plain text with components, planned window, stages, and `(edited)`, the
  cap of 20 entries in order of change with ties broken by notice number, and
  escaping of `<`, `&`, and `>`. Two golden files in
  `internal/statuspage/testdata/feeds/` pin the exact XML; run
  `go test ./internal/statuspage -run TestFeedGolden -update` to rewrite
  them.
- `local_time_test.go` runs `node --test local_time_test.mjs`, so
  `go test ./internal/statuspage` also tests the page's local-time script
  when `node` is installed, and skips that test when it is not.

`internal/app/status_page_test.go` checks the Worker and bucket names: the
same for the same host name, different for host names that differ only in
punctuation, and within Cloudflare's naming rules for long host names
(`TestCloudflareNamesDifferForEachHostName`).

`internal/app/status_page_test.go` runs Cloudflare setup against the fake's
v4 API. The checks cover the bucket, host name, and rewrite rule.
A rerun changes nothing and preserves another host's rule.
The tests check refused permission, a host name outside the account, and
that no API token is stored.

`internal/app/status_page_test.go` applies the settings file's `status_page`
part by posting the current page with the configured R2 key and
`page_title`. The post creates `status.json` if the bucket lacks `status.json`. A wrong R2 key fails the `status_page` part and is not saved. The application tests also check posts
from the incident page's button and the Status page page.

`cmd/mmhotline/status_page_test.go` runs the laptop's `status-page set-up`
against the fake, with no command socket: its output and `--json`, a token
file other users can read refused, `-` reading standard input, `--page-title`
refused, and a second run that changes nothing.
`cmd/mmhotline/status_page_test.go` posts, updates, and resolves from a
separate `mmhotline` process with credentials supplied only through
`MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL`,
`MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID`, and
`MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY`.

`internal/fakecloudflare` follows Cloudflare's documentation; a real account
has not been used.

## MMHotline-down alarm checks

`internal/downalarm/mmhotline_down_alarm_worker_test.mjs` tests the Worker's own logic
under `node --test` (Node.js 22.15 or later) with a fake of the Durable
Object's storage and alarm and a fake send function.
Like Cloudflare's storage, the fake storage refuses more than 128 keys in
one write or delete. The tests check the following cases:

- After check-ins stop, each held message sends once after the message's
  delay. A check-in before the delay moves the alarm. An unchanged check-in
  writes one row.
- A send with an unknown outcome is sent again. No answer, 429, and 5xx
  cause retries at the due time and every 30 seconds for 10 minutes after
  the original due time. After a 30-second wait for an answer ends, the
  next retry waits another 30 seconds. A final refusal stops retries.
- A check-in stops sends in progress, but a late delivery is still recorded.
- Each alarm run attempts at most 40 sends. The second run attempts the
  41st held message while 40 earlier messages still get no answer.
- 200 held messages are stored, sent once each, and removed.
- A header value with a control character, and other broken rules, are
  refused when stored. Earlier held messages and the alarm are kept.
- The token check runs before the Durable Object. Test sends are also
  checked.

`go test ./internal/downalarm` runs the Worker tests. When `node` is not
installed, the Go test skips the Worker tests and prints the command to run.

`internal/app/down_alarm_test.go` applies the settings file's `mmhotline_down_alarm`
part against `internal/fakecloudflare`, whose stand-in Worker checks the
token and field names. The application tests cover:

- The upload declares the Durable Object class in a Cloudflare migration
  only for a new Worker. Uploads preserve the secret. Failed first
  check-ins restore the earlier setup.
- Changing accounts is refused while the MMHotline-down alarm is set up. The tests
  also check setup refusals.
- Held Pushover notifications and calls have the real addresses and fields.
  Check-ins run every minute and immediately after a handoff. Delays extend
  past team mute.
- An all-clear reaches only people the MMHotline-down alarm reached, once each.
  Failed check-ins cause one warn alert after 10 minutes. A successful
  check-in after the failure causes a recovery alert.
- An upgrade uploads the Worker. Omitting `mmhotline_down_alarm` is refused while the
  Worker is unreachable. The dashboard card and "Send a test now" work.

## Access and listener checks

`internal/cloudflareaccess/verifier_test.go` checks the Cloudflare Access
token against keys from a local test server: the RS256 signature, checked
before the claims; the issuer, the audience, expiry and not-before times with
60 seconds of clock allowance, and a non-empty email; malformed tokens and
tokens over 16 KiB, refused without fetching keys; key rotation, a new fetch
after an hour, and at most one fetch a minute for an unknown key or after a
failure; cached keys kept when a fetch fails; unusable keys skipped; and the
team domain and audience tag rules.

`internal/app/browser_sign_ins_test.go` checks page sign-in. A member's token
opens the dashboard; a token for an unknown address gets the 403 page; a
missing token or one signed by another key gets the 401 page with the sign-in
link instructions. A sign-in link works once and for 10 minutes, is stored only
as a hash, is logged by person but never by token, and sets a 12-hour cookie;
a failed cookie save leaves the link usable. Removing a person ends their links
and cookies, and a link for a removed or unknown address is refused. A request
with an Access header is judged by the header alone, including the 503 page
when keys cannot be fetched; with no Access settings, the header is ignored
and the cookie applies. A link opened at `http://localhost` through an SSH
forward gets cookies without `Secure` that still open pages and save a People
form (`TestSignInLinkThroughSSHForwardUsesPlainHTTPCookies`). The web listener
has no intake routes, and alert source results and the Alert sources page show
only the alert intake address. `internal/app/access_sign_in_test.go` holds the
shared local keys server and token signing.

`internal/app/people_test.go` checks missing identity on each main page,
unknown identity, immediate removal, administrator restrictions, duplicate sign-in email
addresses, last-administrator protection, saved schedules, retained history,
CSRF, and signed intake-only routes with or without an Access token. Existing
public HTTP workflows use a member fixture through the application operation
and a client that adds a signed Access token. With an `http://localhost` web
URL, for a team without Cloudflare, alerts still go out with links to that URL
(`TestDeliveryWithLocalhostWebURL` in
`internal/app/contact_method_type_outgoing_webhook_test.go`).

`cmd/mmhotline/main_test.go` checks the root-peer decision, socket permissions,
stale socket replacement, and serve's refusal of a missing settings file and,
with nothing applied yet, of one with problems.
`TestCLISetupCommandsEndToEnd` in `cmd/mmhotline/main_test.go` starts a
real `serve` process with a settings file that has `web_listen_address`,
`alert_intake_listen_address`, and
`cloudflare_access`; the keys
address goes to a local test server (`cmd/mmhotline/access_sign_in_test.go`).
`TestCLISetupCommandsEndToEnd` checks the 401 answer without identity,
the local people commands including
`create-sign-in-link`, a sign-in link and its cookie on the web listener, an
Access token, a signed alert on the intake listener, and the startup sign-in
notice. Linux command integration needs a root test process, because both the
command socket and the settings file's owner must be root there; macOS
development uses the service user. A Linux run is required to prove `SO_PEERCRED`; a macOS
run exercises `LOCAL_PEERCRED`. The command and listener tests in
`cmd/mmhotline/main_test.go` do not establish that a real
Cloudflare tunnel and Access application deliver the token, or that a real SSH
forward carries a sign-in link.

`cmd/mmhotline/install_test.go` runs install against a temporary root with
fake `groupadd`, `useradd`, and `systemctl`.
`cmd/mmhotline/install_test.go` checks the generated units, the settings
file's owner, group, and mode, the refusals listed in the contract map, and
that a web URL on `http://127.0.0.1` gets no next step about `cloudflared`.

## Test layers

Use package-level Go tests for deterministic logic with explicit time. Use real
temporary stores for persistence and small local HTTP/process fixtures for
external boundaries. Avoid a test that merely repeats the implementation or
freezes obsolete behavior.

Workflow tests run the built public interfaces with isolated state. A helper
that calls a private transition directly is not an end-to-end workflow. Test
the cgo-disabled artifacts; run the race detector separately, as
[Go tests under stress](#go-tests-that-never-depend-on-the-machines-speed) does.

Golden page tests (`TestGoldenPages` in `internal/app`,
`internal/statuspage`, and `internal/fakesendingservices`) render each HTML
page in fixed states and compare it with
`testdata/pages/<package>/<page>__<state>.html`, apart from whitespace that
does not show; package `internal/htmlgolden` says exactly what the comparison
ignores. Each test's `goldenPages` table lists the states it covers and the
states it does not, with the reason. After an intended page change, run the
test with `-update` and review the changed golden files.

Platform tests run selected artifacts on the agreed production targets.
Sending service tests use designated contact methods to establish request
acceptance, actual notification receipt, and observed call outcomes separately,
as described in [fake sending services](#fake-sending-services).
Record the scope and outcome in validation.md.

## Test in levels

Run the smallest set that can show whether a change works. Expand only when
a larger run can find a fault that the smaller run cannot.

- While iterating, run `go test` for the packages touched by the change.
- After an ordinary change, run only the affected workflow tests.
- At the end of an implementation change, run its new and changed tests once.
  If shared code affects every workflow, also run all workflow tests once.
- Platform and real sending service acceptance checks establish behavior that local
  fixtures cannot prove. Run the affected acceptance checks before a release.
- After a failure, fix the fault and rerun the failed tests. Expand only if the
  fix affects additional behavior.
- Documentation-only changes require link and consistency checks, with no
  application suite on a server.
- When a run is slow, find where the time goes before running more tests.
- A real-server proof of a refusal also reruns the refused command without
  resetting the state, as a person would. Reason: the first fix for a new
  server's backup refused only the install that created the database, and
  a proof that reset the state between runs missed it.

Use `GOCACHE="$TMPDIR/go-build"` for local Go commands. The
[local development guide](local-development.md#build-and-start) shows the cgo-disabled build.

Run the whole Go suite with a 30-minute limit:

```sh
go test -timeout 30m ./...
```

Reason: `internal/app` has 732 tests that start Git, shell scripts, and the
test binary. They took about 7.5 minutes one after another on a MacBook
(measured 2026-11-21), and a busy machine pushed an earlier, shorter run
past `go test`'s default 10-minute limit, which fails the package with
`panic: test timed out after 10m0s` while no test is at fault. Most tests
now call `t.Parallel`, and the package takes about 3 minutes on the same
MacBook. The time still depends mostly on the machine's load: on 2026-11-15,
with the MacBook's load average near 100 from other sessions, the same
package took 24 minutes, and four MMX run tests that measure seconds failed
(`TestMMXRunTimeLimit` and three others), then passed when run alone. A
suite slower than about 10 minutes, or a timing test that fails, means
checking `uptime` before looking for a slow test.

Parallel tests share one process, so a test stays serial, with no
`t.Parallel`, when it changes something the process shares and restores it
before it returns: environment variables (`t.Setenv`), the working
directory, the `log` output, a package variable such as a time limit, or
the copy lock's file when it checks that the lock is free. A test whose
result is a wall-clock time of seconds, such as the MMX run time limit
tests, also stays serial. Go finishes every serial top-level test before
it resumes any parallel one, so a serial test never overlaps a parallel
one. A test that starts Git, shell scripts, or the test binary begins with
`parallelStartingProcesses(t)` instead of `t.Parallel()`, which lets four
such tests run at once: with every test at once, each start is slower, and
tests that wait ten seconds for a fake agent or MMX run failed. `TestMain`
gives the whole run one empty `HOME`. It also wraps `http.DefaultTransport`
so that `httptest.Server.Close` does not call its `CloseIdleConnections`:
one test's server closing would otherwise break another test's request.

## Static analysis for a release

Run static analysis only as a [release step](design/development.md#release-gates),
from the repository root on the development machine:

```sh
scripts/static-analysis.sh
```

`scripts/static-analysis.sh` installs pinned tools in a temporary directory,
prints every report, and exits non-zero if any check fails.
`scripts/static-analysis.sh` needs Go and network access.
`scripts/static-analysis.sh` builds tools for the development machine, then
analyzes files compiled for Linux with cgo disabled, as MMHotline runs
(`GOOS=linux`, `CGO_ENABLED=0`). `scripts/static-analysis.sh` runs the following commands,
with the installed tool directory on each tool's path:

```sh
go vet ./...
staticcheck ./...
errcheck -ignoretests -exclude errcheck_excluded_functions.txt ./...
exhaustive -d ./...
nilaway -test=false -include-pkgs=mmhotline ./...
govulncheck ./...
```

The pinned versions are staticcheck v0.8.2, errcheck v1.30.0, exhaustive
v0.15.0, govulncheck v1.9.0, and NilAway
`go.uber.org/nilaway@v0.0.0-20260918162853-acb8859b9031`. For exhaustive
v0.15.0, `-d` treats a default branch as exhaustive. MMHotline uses its Go
dependencies as published, with no patched copies, so `govulncheck` checks
the module directly. `govulncheck` fails only for an advisory whose
vulnerable function MMHotline's code reaches. An advisory in the standard
library is fixed by raising the `toolchain` line in `go.mod`.

No tool checks the MMHotline-down alarm's Worker, `internal/downalarm/mmhotline_down_alarm_worker.js`.
The MMHotline-down alarm's Worker has no npm dependencies, so a vulnerability check has
nothing to read. The Worker's behavior is tested under `node --test`
([MMHotline-down alarm checks](#mmhotline-down-alarm-checks)).

Reviewed exceptions have their reasons beside them: `staticcheck.conf`
excludes ST1005 because MMHotline's errors are shown to people, so many are
full sentences. The `lint:ignore S1005` comment in
`internal/app/contact_method_type_slack_message.go` keeps an explicit
discard that errcheck requires. Errcheck exceptions are in
`errcheck_excluded_functions.txt` for whole functions and in commented `_ =`
assignments for individual calls. NilAway exceptions are `//nolint:nilaway`
comments at the end of the reported line, or on the line before an `if`
statement or a function, which they then cover whole. Each exception has a
one-line reason. Errcheck and NilAway leave out test files.
`scripts/static-analysis.sh` builds NilAway from the pinned source with
`scripts/nilaway-call-through-function-variable.patch` applied. The patch
comes from MMX. The patch file explains the fault and removal condition.

## Fuzzing for a release

Fuzz tests change input bytes to find inputs that break a parser or a check.
Each target feeds one kind of outside input to the function that reads or
checks it, and asserts what the requirement says, not only that nothing
panics. Run the fuzz targets as a
[release step](design/development.md#release-gates),
from the repository root in a writable copy of the working tree, with its
`.git`, on a Linux test server:

```sh
scripts/run-fuzz-tests.sh
```

The script runs thirteen targets, one at a time, for 10 minutes each with
four worker processes: about 2 hours and 20 minutes. To change the time per
target and number of worker processes:

```sh
scripts/run-fuzz-tests.sh --fuzz-time-per-target 30s --fuzz-workers 2
```

`scripts/run-fuzz-tests.sh --fuzz-time-per-target 30s --fuzz-workers 2`
gives each target 30 seconds and two workers. The script prints each result,
continues after a failure, and exits non-zero if any target
fails. A name in `scripts/run-fuzz-tests.sh`'s target list that matches no
fuzz target fails too: `go test` would pass the unmatched target name without
fuzzing anything.

`scripts/run-fuzz-tests.sh` limits minimization of each input finding new
coverage to 10 runs (`-fuzzminimizetime=10x`). Most targets' coverage differs
from run to run of the same input, through random IDs, goroutines, and RSA
blinding,
so with Go's default of 60 seconds the workers spent most of each target's
allotted time shrinking inputs and ran up to fifteen times fewer inputs;
the script names the measurements. A failing input is then minimized less,
and still reproduces.
Go runs in a temporary directory of the script's own, which it removes at
the end: a fuzz worker stopped at the end of a target leaves its last
input's temporary directories behind.

| Target | Package | Outside input | Asserts |
| --- | --- | --- | --- |
| `FuzzAlertSourceRequestBody` | `internal/app` | A body on the alert intake listener | No body is both an alert and a check-in; a refusal is `ErrInvalid` with an empty result; an accepted alert keeps the documented bounds and reads back the same; a check-in's maintenance windows persist, with end times capped at 7 days after check-in receipt; a correctly signed SOS that is refused still reads as an SOS, so MMHotline opens an SOS incident about refusing the sender's SOS |
| `FuzzAlertSourceSignature` | `internal/app` | The `Mmhotline-Signature` header | A correct signature verifies and names its secret; a clock more than 5 minutes off is distinguished from an incorrect signature; a body changed after signing is refused; only the canonical header parses |
| `FuzzCloudflareAccessToken` | `internal/cloudflareaccess` | A Cloudflare Access token, signed with the verifier's key | No key fetch; an email address lowercased and trimmed, or `ErrInvalidToken`; an accepted token has the team's issuer and audience and is not expired; any one byte changed is refused |
| `FuzzSlackInteractiveEnvelope` | `internal/app` | A Socket Mode envelope | A press claims or resolves only when the Slack message matches the delivery and contains the pressed button. The pressing Slack user must be linked to a current member in the delivery's workspace. |
| `FuzzTelegramUpdate` | `internal/app` | A `getUpdates` item | A Telegram press claims or resolves only from the delivery's message and button, by the Telegram user linked to a current member. |
| `FuzzTwilioCallAnswer` | `internal/app` | Twilio's answer to a call fetch | An answer about another call, or with an unlisted status, reports nothing; an ended call reports one of the four call results |
| `FuzzTriageAgentRequest` | `internal/app` | A triage agent request's fields | Short refusals; a playbook path inside the systems repository; an inventory name of one argument; a report with a valid severity and 1 byte to 64 KiB; the MMX name stands for exactly the requested file |
| `FuzzAgentCommitsBundle` | `internal/systemsrepository` | The agent's commits, as any tree Git holds, bundled and possibly damaged | Origin moves only when MMHotline pushes; a push changes only regular files in `one_time_playbooks/` and `triage_agent_notes/`; such commits are pushed |
| `FuzzMMXExplainOutput` | `internal/actionkinds` | `mmx explain --json` output | Every task listed once; kinds are those of the tasks, each task judged alone; an extra module argument, or an unknown run, makes a task unrecognized; Change DNS records only for one zone's DNS records API |
| `FuzzFleetView` | `internal/app` | The fleet view command's output | Healthy servers and servers needing attention form separate lists, each sorted by server name; each server appears once |
| `FuzzChecklistYAML` | `internal/app` | `monkeymaxxing_stack_checklist.yml` | Only a `done` item is hidden. If `verify_again_every` is present, hiding also requires a valid `verified_at` RFC 3339 timestamp and a parseable interval; each item judged alone |
| `FuzzStatusJSON` | `internal/statuspage` | `status.json` and one post | A post keeps every notice and update; a new notice's ID is above all others; the saved file reads back the same and renders |
| `FuzzCustomScheduleSandboxOutput` | `internal/app` | The sandbox child's standard output | A protocol error or a well-formed problem report returns no schedule answers; accepted answers cover every asked time and test case and name only team members |

The Slack, Telegram, and Git bundle targets set up a new team or new
repositories for each input, so a failing input reproduces on its own. The
Git bundle target needs `git`.

Go writes a failing input in the target's package directory, under
`testdata/fuzz/<Target>/`. Preserve the failing-input file before replacing
the working-tree copy. Reproduce the failure with the command Go prints,
with the package path:

```sh
go test ./internal/app -run='^FuzzAlertSourceRequestBody/<input-name>$' -count=1
```

Replace `<input-name>` with the file name Go printed. Decide from the
requirement whether the fault is in MMHotline or in the target's assertion,
and fix it at its root cause, never by loosening an assertion until it
passes. Rerun the saved failing input and the affected tests. Run the fuzz
target that found the failure again for 10 minutes. Then commit the fix
and saved input together. A file name that says what the input holds helps
the next reader.

Ordinary `go test ./...` runs the supplied seeds and the committed inputs
without fuzzing. Keep small inputs that protect distinct faults. Go keeps the generated corpus in the build cache. Never commit the generated
corpus.

## Go tests that never depend on the machine's speed

A Go test must pass on a loaded machine as the test does on an idle machine, including
beside other tests. A test that passes idle and fails once in a while loaded
is broken, also when a rerun passes. Such a failure looks like a fault of
MMHotline, costs an investigation each time, and teaches people to ignore a
failed run.

- **Wait for the event, not for a time.** A test that expects something to
  happen polls for it with a deadline far above the normal time. The wait
  returns as soon as the event happens, so a long deadline adds no delay
  when the event occurs.
  A fixed sleep followed by an assertion that something happened is a fault.
  So is a step that assumes another goroutine has finished its work because
  a write to the goroutine returned. A `net.Pipe` write returns once the
  receiving goroutine has read the bytes, before the receiving goroutine
  necessarily acts on the bytes.
- **The test controls every time span that its outcome depends on.** When
  the outcome depends on a window, an interval, or a delay of MMHotline
  itself, the test holds the span open until its own work is done and then
  ends it, or sets the span far above what that work takes. A short real
  span that the test's own work must fit into is a fault, and so is an exact
  count of periodic events within a real time.
- **An upper limit on elapsed time is wide.** The elapsed-time assertion
  proves only that something ended instead of hanging or waiting for a
  default: at least ten times the
  normal time and at least 3 seconds, and below the time that the fault
  would take.
- **A wait that proves that something did not happen** may be a fixed
  sleep. The fixed sleep cannot cause a failure because the machine is slow,
  but may miss an unexpected event occurring after the sleep ends.
  Keep the fixed sleep short. A lower limit on elapsed time is fine too,
  because real time never runs faster.
- **A test that runs in parallel shares no process-wide state with other
  tests.** A test that calls `t.Parallel()` must not share the standard
  logger, environment variables, the working directory, or package
  variables with other tests.
- **A fixture keeps what it opens until the test ends.** Go's garbage
  collector closes a network connection or file that nothing references
  any more, at a moment no test chooses. A fake sandbox that hands a
  connection to a test keeps it until the test's cleanup, as
  `triageRunFixture` does.
- **Prove timing independence under stress.**
  [`scripts/run-go-tests-under-stress.sh`](../../scripts/run-go-tests-under-stress.sh)
  builds every Go test package with the race detector and runs the tests
  with Go's own `stress` tool (`golang.org/x/tools/cmd/stress` v0.48.0):
  by default 40 runs, 4 at once, which loads the machine. The race detector
  needs cgo and a C compiler, so the script sets `CGO_ENABLED=1` only when
  building test packages for stress runs with the race detector.
  Product builds and ordinary tests keep cgo disabled.
  Run the tests that a change adds or changes by name, and every test
  before a release:

  ```sh
  scripts/run-go-tests-under-stress.sh --tests-matching '^TestTriageAgentRunEndClosesPendingProxyRequests$'
  scripts/run-go-tests-under-stress.sh
  ```

  Run the whole suite from a writable copy of the working tree, with its
  `.git`, on a Linux test machine. Set
  `MMHOTLINE_TEST_MMX_PROGRAM` to an `mmx` program for Linux, so that the
  two tests that run `mmx` run too. The Linux command socket accepts only
  root, so `TestCLISetupCommandsEndToEnd`,
  `TestServeTriageAgentSocketsAndSetupWorkflow`, and the Telegram case of
  `TestCLISignalShutdownExitCodes` skip for non-root users.
  Run the named tests as root:

  ```sh
  sudo -H env PATH="$PATH" scripts/run-go-tests-under-stress.sh --tests-matching '^(TestCLISetupCommandsEndToEnd|TestCLISignalShutdownExitCodes|TestServeTriageAgentSocketsAndSetupWorkflow)$'
  ```

  A measured full run took about 75 minutes on a 12-thread server.
  Run named tests locally while iterating.
  `--runs N` changes the number of runs, `--runs-at-once N` changes how many
  run at once, and `--tests-matching REGEXP` selects tests as `go test -run`
  does. Exit 0 means every run passed; the script removes its work directory.
  Exit 1 lists each failed test and names the files that keep the whole
  output of each failed run. Exit 2 means no test matches.
- **A test that failed once is fixed at once.** Reproduce the observed failure
  with `scripts/run-go-tests-under-stress.sh --tests-matching REGEXP`.
  Find the cause, fix the fault under the timing rules in
  [Go tests that never depend on the machine's speed](#go-tests-that-never-depend-on-the-machines-speed),
  and prove the fix with the selected stress-test command.
  Never add a retry of a failed assertion, a skip on a slow machine, or a
  longer sleep.

The [release gates](design/development.md#release-gates) require every Go
test to pass under stress.

## SMTP and Mailpit

SMTP unit tests run a small loopback mail server. The SMTP unit tests check
setup without sending, sending service refusals, PLAIN and LOGIN, required
STARTTLS, implicit TLS, and certificates. The SMTP tests check address and
credential rules, exact body encoding, headers, and stable Message-ID.
The SMTP tests also check retryable 4xx replies, permanent 5xx replies, lost
acceptance replies, and connection cancellation. Email is not handled
by `MMHOTLINE_FAKE_SENDING_SERVICES_URL`; set up Mailpit for development email.

The acceptance test requires a [Mailpit v1.31.6 binary](https://github.com/axllent/mailpit/releases/tag/v1.31.6).
Run from the repository root:

```sh
MMHOTLINE_TEST_MAILPIT_BINARY=/absolute/path/to/mailpit \
  CGO_ENABLED=0 GOCACHE="$TMPDIR/go-build" \
  go test ./internal/app -run '^TestEmailMailpitAcceptance$' -count=1 -v
```

`MMHOTLINE_TEST_MAILPIT_BINARY` must name a Mailpit v1.31.6 binary to run the
Mailpit acceptance test. Without the variable, the skip message says
`MMHOTLINE_TEST_MAILPIT_BINARY must name a Mailpit v1.31.6 binary to run the Mailpit acceptance test.`
`TestEmailMailpitAcceptance` starts isolated Mailpit processes on loopback
ports for `unencrypted` and `starttls` with authentication.
The Mailpit acceptance test sets up email through the application operation.
The Mailpit acceptance test sends a contact-method test through the
database-backed delivery queue (the outbox).
The Mailpit acceptance test reads recipient, subject, body, and Message-ID
through Mailpit's raw-message API.

The test creates its certificate and trusts it only through the test
contact method type's TLS config; production uses system authorities. Each test stops
its own Mailpit process. No real mailbox or relay is contacted.

## Fake sending services

The fake sending services imitate chat, push, and phone services and a webhook
receiver. Email uses local SMTP fixtures in unit tests and Mailpit for
development:

- `internal/fakesendingservices` runs in process for Go tests, which drive
  the fake sending services through the package API.
- `cmd/mmhotline-fake-sending-services --listen-address 127.0.0.1:8090` is a separate
  program for local development and HTTP-driven checks through
  `GET /fake-api/sent` and `POST /fake-api/actions/...`.
  `mmhotline-fake-sending-services` is never linked into `mmhotline`.

**`internal/fakesendingservices` provides a webhook receiver, Telegram,
Slack, Pushover, and Twilio.** The webhook receiver
answers 200 unless `POST /fake-api/actions/webhook-receiver/{name}/answer`
(or `SetWebhookAnswer` from `internal/fakesendingservices`) sets another
HTTP status code or makes the webhook receiver never answer.

The fake Twilio accepts inline TwiML at
`{fake}/twilio/2010-04-01/Accounts/{AccountSid}/Calls.json` and answers call
fetches at `Calls/{CallSid}.json`. The fake extracts the spoken message from
the supplied TwiML. Setup checks use the authenticated, filtered
`IncomingPhoneNumbers.json?PhoneNumber=...` listing for account ownership and
voice capability. The fake credentials are
`fakesendingservices.TwilioAccountSID`, `TwilioAPIKeySID`, and `TwilioAPIKeySecret`;
the corresponding values are `AC00000000000000000000000000000001`,
`SK00000000000000000000000000000001`, and `fake-twilio-api-key-secret`.
Calling numbers `+12025550102` and `+12025550103` belong to the fake account
and support voice.

The fake enforces one call a second with HTTP 429. `AnswerCall` selects
`answered`, `voicemail`, `no_answer`, `busy`, or `call_failed`;
`ReportCallStatus` sets the raw Twilio status. `AnswerCall` and
`ReportCallStatus` send no request to MMHotline. `PhoneCalls` exposes the
spoken message, current status, and fetch
times. `SetNumberBehavior` refuses invalid numbers or geographic permissions;
`SetNextSendingServiceAction` refuses a request or loses its response. Tests supply
`SetPhoneClock` for deterministic time.

The fake's `POST /fake-api/actions/phone` accepts `call_answer`,
`number_behavior`, `next_request`, and `rate_limit`. `GET /fake-api/sent` lists
accepted sends. Set up `twilio` through the settings file even in fake mode; setup alone makes no call. The fake sending services page can select
Answered, Busy, No answer,
or Call failed and MMHotline learns that result on a later poll.

Custom schedule code tests start the sandbox child: the test binary stands in
for `mmhotline` and runs `custom-schedule-sandbox` from `TestMain`. The lockdown
test, which proves that opening a file, opening a socket, and starting a
program each kill the child, and the memory limit test run only on Linux: run
`go test -timeout 30m ./internal/app` on a Linux AMD64 server for every Go upgrade.

The fake Telegram answers the Bot API calls MMHotline makes (`getMe`,
`sendMessage`, `editMessageReplyMarkup`, `answerCallbackQuery`, and
`getUpdates` with long polling and offsets) at `{fake}/telegram/bot{token}/`,
with Telegram's refusals for an unknown chat, button data over 64 bytes, an
edit that changes nothing, and a button press answered twice. Tests play a
Telegram user through the package's `TelegramStart` (pressing Start on a
connect link, in a private chat or a group) and `TelegramPressButton`, and
read `TelegramMessages` (with each message's current buttons) and
`TelegramCallbackAnswers`. For HTTP-driven checks, use
`POST /fake-api/actions/telegram/start` (`telegram_user_id`,
`telegram_user_first_name`, `start_token`, and for a group
`telegram_group_chat_id` and `telegram_group_title`) and
`POST /fake-api/actions/telegram/press` (`telegram_user_id`, `chat_id`,
`message_id`, `callback_data`). In development the connect link opens the
fake's stand-in for t.me, `{fake}/telegram/t.me/{bot}`, with a Start button,
and the fake sending services page lists Telegram messages with buttons to press.

The fake Slack, at `{fake}/slack`, answers `auth.test`,
`apps.connections.open`, `chat.postMessage`, `chat.update`, and
`chat.postEphemeral`. The fake Slack's Socket Mode WebSocket sends `hello`,
events, button presses, and disconnect envelopes, and records acknowledgements. Use
`fakesendingservices.SlackBotToken` and `SlackAppLevelToken` for setup; wrong tokens
are refused. `SlackSendMessage` simulates a chosen user's direct message or
mention of the bot. `SlackPressButton` simulates a user pressing a message
button, and `SlackDisconnect` requests a
reconnect. `SlackMessages`, `SlackReplies`, `SlackAcknowledged`, and
`SlackConnections` expose the sending service outcomes. The fake enforces
conversation access and limits for Block Kit, Slack's message layout format.

The fake sending services page shows setup tokens and forms to send a code as a
chosen Slack user, then lists posted messages and their buttons. For HTTP
checks, `POST /fake-api/actions/slack/message` accepts `slack_user_id`,
`slack_conversation_id`, `slack_message`, and `slack_message_kind`
(`direct_message` or `mention`). `POST /fake-api/actions/slack/press` accepts
`slack_user_id`, `slack_workspace_id`, `slack_channel_id`, `message_ts`, and
`action_id`. All Slack events flow over the connection opened by MMHotline.

The fake Pushover implements message creation, application limits, receipt
status, and cancellation at `{fake}/pushover/1/`. Use
`fakesendingservices.PushoverApplicationToken` and the personal keys
`PushoverUserKey`, `PushoverOtherUserKey`, or `PushoverThirdUserKey`.
`SetPushoverClock` controls expiry and the enforced five-second receipt
interval. `PushoverMessages` and `PushoverReceipts` expose accepted messages
and polling times. `PushoverAcknowledge` simulates acknowledging an emergency
on the phone. `PushoverExpire` marks an emergency receipt as expired.
The fake sending services page has an Acknowledge button for each open emergency,
using `POST /fake-api/actions/pushover/acknowledge` with `receipt` in a form.

`MMHOTLINE_FAKE_SENDING_SERVICES_URL` points chat, push, and phone adapters at the
fake: `{fake}/twilio`, `{fake}/slack`, `{fake}/telegram`, and
`{fake}/pushover`. Sending service credentials still come from the settings
file's `sending_services`, which `settings apply` checks against the fake. Outgoing webhook contact methods may then target
`{fake}/webhook-receiver/{name}`, and the web interface shows "Development
mode: chat and calls use fake sending services. Set up email against Mailpit."

The fake implements only what MMHotline uses, and each handler cites the
sending service documentation it follows. `internal/fakesendingservices`
enforces sending service rate limits and can
simulate answered calls, no answer, busy, permanent and geographic failures,
and lost responses. `mmhotline-fake-sending-services` provides the
"What MMHotline would have sent" page, which has a button
for each of the Twilio actions (the webhook receiver's answers are set
through `/fake-api/actions/webhook-receiver/{name}/answer`).

The current [40-member Go scenario](../../internal/app/phone_scenario_test.go)
contacts one team with 40 members at once. Each member's initial call starts
within 45 seconds under enforced sending service limits. The test advances
scheduler rounds one second at a time.

A fake result is never delivery evidence. Real sending service acceptance requires
designated recipients and explicit checks of actual receipt or call behavior.
[Validation](validation.md) records the checks performed and their limits.

## Required concurrency scenarios

- Claim or resolve while a repeat/escalation notification waits in the queue,
  is reserved by a worker before sending, or has a sending service request
  in progress.
- Two authorized responders act on the same incident.
- Availability or schedule changes while a notification delivery attempt is
  being prepared.
- Team mute starts after reservation; unmute and an overdue repeat happen
  together; personal unavailability remains active after team mute ends.
- Two members take on-call from the same person; a coverage change leaves a claimed
  incident's owner unchanged. An open incident keeps the current target
  while the member remains eligible.
- Custom schedule code finishes after its configuration is replaced.
- A Pushover acknowledgement, a Slack or Telegram button, and a browser claim race
  for the same incidents; nobody takes over an incident another member
  claimed.
- A contact method value/user is changed or removed while an old attempt completes.
- Restart with an uncertain sending service result and an expired response deadline.
- Restart after several repeat intervals and shift boundaries: recover current
  work without a catch-up burst or skipping eligible escalation responders.

Choose deterministic barriers around the relevant boundaries instead of long
sleeps. Test a concrete supported sequence; do not build exhaustive matrices
before the first feature works.

## Release archive install check

Run the release archive install check as a
[release step](design/development.md#release-gates), on
the archives that [`scripts/build-release-archives.py`](design/development.md#release-archives)
built from the release's commit. The release archive install check needs
one fresh AMD64 test server for
each distribution: Debian 12, Debian 13, Ubuntu 24.04, and Ubuntu 26.04. On
each server, follow [Install MMHotline on the Linux server](../how-to/install.md)
with the archive and `SHA256SUMS` copied from the build machine:

1. `sha256sum --check --ignore-missing SHA256SUMS` prints `OK` for the
   `amd64` archive.
2. [Write the settings file](../how-to/install.md) with
   `team_name: Release check`, `web_url: http://127.0.0.1:8480`, and
   `alert_intake_listen_address: 127.0.0.1:8484`, then, in the unpacked
   folder, `sudo ./mmhotline install` succeeds.
3. `systemctl is-active mmhotline` prints `active`, and `mmhotline version`
   prints the release's version, its commit, and `modified=false`.
4. [Send a generic alert](../how-to/connect-a-tool-that-sends-alerts.md) returns a
   response with `mmhotline_alert_id`.
5. Put a custom schedule in the settings file, such as the first one in
   [Write a custom schedule](../how-to/write-a-custom-schedule.md#a-first-schedule)
   with the file's own administrator in place of both people. Then
   `sudo mmhotline schedules preview --settings-file /etc/mmhotline/mmhotline_settings.yml`
   lists shifts and ends with `Schedule preview complete. Nothing was saved.`,
   and `sudo mmhotline settings apply` prints `on_call_schedule: applied.`
   Reason: the custom schedule's child process loads its own seccomp
   filter, which only the installed unit's system call filter can block;
   the Go tests run the child outside systemd. On 2026-11-15 a unit
   without `SystemCallFilter=seccomp` failed every custom schedule on
   installed servers while every test passed.

The `linux-arm64` archive is built and inspected with `file`, which must
report a statically linked AArch64 program; an ARM64 server runs the check
only before a major release. On a Mac with Apple silicon, unpack the
`macos-arm64` archive after `shasum -a 256 --check --ignore-missing SHA256SUMS`;
its `mmhotline version` prints the release's version, its commit, and
`modified=false`, and `status-page show`, `post`, and `resolve`, with the
`MMHOTLINE_STATUS_PAGE_R2_BUCKET_URL` (the status page's R2 bucket URL),
`MMHOTLINE_STATUS_PAGE_R2_ACCESS_KEY_ID` (the bucket's access key ID), and
`MMHOTLINE_STATUS_PAGE_R2_SECRET_ACCESS_KEY` (the bucket's secret access
key), work against a status page
bucket. Record the commit, the Go version, and each result in
[validation](validation.md#release-checks).

## A stand-in person

Some proofs on a real installation need a person: someone MMHotline contacts
for an SOS, who claims and resolves it, and who approves or refuses the triage
agent's playbook runs. A stand-in person is a team member that a coding agent
plays, so installation checks of SOS handling and playbook approvals wake
no real person. The stand-in has no contact method. MMHotline selects the
stand-in as the responder, but sends no notification: the history says
MMHotline could not wake the stand-in, and the team's administrators get one
email or chat notice per incident about it.
The coding agent conducting the test reads what arrived with the
[read commands](../reference/mmhotline-command.md#commands-that-read-what-happened) and acts through the
pages a person uses, posting their forms with `curl`.

Run the commands below on the MMHotline server, as a user with `sudo`, in a
directory only that user can read, because it holds sign-in cookies:
`mkdir -m 700 ~/stand-in && cd ~/stand-in`. In the examples, Barbara
(`barbara@example.com`) is the real person on call.

### Add the stand-in and put it on call

The stand-in goes in the [settings file](../reference/settings-file.md), like
every person: in the systems repository when a deploy tool renders the file,
or with `sudoedit /etc/mmhotline/mmhotline_settings.yml` on the server. Keep
the team's `people` and `on_call_schedule` as they are before the change, to
put them back when the proofs end; with a deploy tool, the commit before the
change keeps them. Add the stand-in to `people`:

```yaml
people:
  # ...the team's people, as before...
  - name: Stand-in for Barbara
    sign_in_email_address: stand-in-1@example.invalid
    role: member
```

A sign-in email address only identifies a person. MMHotline sends nothing
to the sign-in email address. The `.invalid` top-level domain is reserved
and never receives mail, so the address cannot belong to anyone.
Add no contact method for the stand-in.

Next, make the stand-in the primary every day, with everyone available as the
backup, in place of the team's `on_call_schedule`:

```yaml
on_call_schedule:
  escalate_if_unclaimed_after_minutes: 1
  schedule_type: day_of_week
  time_zone: UTC
  handoff_hour: 0
  handoff_minute: 0
  assignment_sign_in_email_addresses: [stand-in-1@example.invalid, stand-in-1@example.invalid,
    stand-in-1@example.invalid, stand-in-1@example.invalid, stand-in-1@example.invalid,
    stand-in-1@example.invalid, stand-in-1@example.invalid]
  backup_option: everyone_available
```

Then check and apply the file on the server:

```sh
sudo mmhotline settings check --settings-file /etc/mmhotline/mmhotline_settings.yml
sudo mmhotline settings apply
```

An SOS then selects the stand-in first. The stand-in has no contact method
that wakes people, so MMHotline moves at once to the next step, everyone
available, without waiting out the time to claim;
[Keep the real people off](#keep-the-real-people-off) makes the stand-in the
only person available.

### Use the pages from a script

The agent signs in as the stand-in with a
[sign-in link](../reference/mmhotline-command.md#mmhotline-people-create-sign-in-link), which works once, for 10
minutes:

```sh
link=$(sudo mmhotline people create-sign-in-link --sign-in-email-address stand-in-1@example.invalid --json \
  | python3 -c 'import json, sys; print(json.load(sys.stdin)["sign_in_link_url"])')
web=${link%%/sign-in/*}
curl -sS -c stand-in-cookies.txt -o /dev/null -w '%{http_code}\n' "$link"
```

The link's address, such as `http://localhost:8480`, is MMHotline's web listen
address on the server itself, so `curl` there needs no Cloudflare Access;
`web` keeps that address for the commands below. The last command prints
`303`, MMHotline's redirect to the dashboard, and saves the sign-in cookie in
`stand-in-cookies.txt`, curl's cookie file. The cookie lasts 12 hours; then
create a new link. From another computer, first run the link's SSH forward
command there, as in
[Sign in during a Cloudflare outage](../how-to/sign-in-during-a-cloudflare-outage.md).

Every page form has a hidden `csrf` field, whose value must match the csrf
cookie MMHotline sets when it shows a page. An incident's forms also have a
hidden `revision` field, the incident's current revision; an action on an older
revision is refused. So the agent reads a page with the cookie file, which
also keeps the csrf cookie, takes the `csrf` and `revision` fields from the
page, and reads the page again before each incident action. Each `curl`
command below prints the HTTP status code of MMHotline's answer:

- `200`: the page was read.
- `303`: MMHotline saved the action, or opened the sign-in link, and redirects
  to a page.
- `400`: a field is missing or not valid, such as an empty reason for refusing.
- `401`: the sign-in cookie is missing or expired; create a new sign-in link.
- `403`: the `csrf` field does not match the csrf cookie; read the page again
  with the cookie file used for the refused request.
- `409`: the incident changed after the page was read, or the request was
  already decided; read the page again.

### Keep the real people off

At the everyone-available step, MMHotline contacts every available member, so
make every member other than the stand-in unavailable for the length of the
proofs, at most 30 days. No command sets another person's availability.
Barbara can set her availability herself: on her On call page, under Your
availability, enter `30d` in For how long and choose Set yourself unavailable.
Without her, the agent signs in as Barbara and posts the availability form:

```sh
link=$(sudo mmhotline people create-sign-in-link --sign-in-email-address barbara@example.com --json \
  | python3 -c 'import json, sys; print(json.load(sys.stdin)["sign_in_link_url"])')
curl -sS -c barbara-cookies.txt -o /dev/null -w '%{http_code}\n' "$link"
curl -sS -b barbara-cookies.txt -c barbara-cookies.txt -o on-call.html -w '%{http_code}\n' "$web/on-call"
csrf=$(grep -o 'name="csrf" value="[^"]*"' on-call.html | head -n 1 | cut -d'"' -f4)
revision=$(grep -o '<h2>Your availability</h2>.*' on-call.html \
  | grep -o 'name="revision" value="[0-9]*"' | head -n 1 | cut -d'"' -f4)
curl -sS -b barbara-cookies.txt -o /dev/null -w '%{http_code}\n' --data-urlencode "csrf=$csrf" \
  --data-urlencode "revision=$revision" -d action=unavailable -d duration=30d "$web/on-call"
curl -sS -b barbara-cookies.txt "$web/on-call" | grep -o 'You are unavailable until [^<]*'
```

The On call page has other forms with their own `revision`,
such as Team mute, so the agent takes the first `revision` field after the
Your availability heading. MMHotline records Barbara as the person who made herself
unavailable. Unavailability ends by itself at the printed time.

While Barbara is unavailable, MMHotline never contacts her for an SOS, and no
SOS reaches her Pushover notifications or phone calls. A contact method with
`send_while_unavailable_or_muted` enabled, the
default for one added on the pages, still sends the alerts it takes: Warning
and Info, and on email, Slack, Telegram, or an outgoing webhook also each SOS,
as a message that wakes nobody. For quiet proofs, list her contact methods
with `sudo mmhotline contact-methods list --sign-in-email-address barbara@example.com`
and turn `send_while_unavailable_or_muted` off with
`sudo mmhotline contact-methods change --contact-method-id ID --send-while-unavailable-or-muted false`.
Contact methods shared with the team, such as a team Slack channel, keep
getting the alerts they take.

Keep the stand-in available and on the team while the real people are
unavailable. With nobody available, MMHotline contacts every member, including
unavailable ones ([escalation path](design/incidents.md#escalation-path)).

### See what arrived and when

The read commands answer the questions of a proof:

- **What arrived:** `sudo mmhotline alerts list --json` lists the newest
  alerts, with each alert's `severity`, `title`, `alert_source_id`, and
  `incident_id` when it opened or joined an SOS incident. Warn and info alerts
  open no incident, so they appear only in the alert list.
  `sudo mmhotline incidents list --json` lists the SOS incidents not resolved,
  with each incident's `incident_id` and `status`.
- **When:** an alert's `received_at` is when MMHotline received it, and
  `happened_at` when its sender says it happened. An incident has `opened_at`,
  `claimed_at`, and `resolved_at`, and each entry of its `history` has
  `occurred_at`.
- **Who is contacted now:** `sudo mmhotline incidents show --incident-id ID --json`
  gives `escalation_step` and `being_contacted_now`. While the triage agent
  works on an SOS, the step is `triage_agent`, `held_for_triage_agent` is
  `true`, and `being_contacted_now` is `No one yet: the triage agent is working
  on it`. Then the step is `everyone_available` at once, and
  `being_contacted_now` must be `Stand-in for Barbara` alone. `history`
  records each step, such as `Could not wake Stand-in for Barbara: Stand-in
  for Barbara has no contact method that wakes people for SOS alerts`, once
  for the on-call step and once for everyone available. An entry that says
  `Nobody is available; contacting every member` means the stand-in was not
  available, and real people were contacted.
- **What waits for approval:** `sudo mmhotline approval-requests list --json`
  lists the requests with `state` `open`, each with its
  `approval_request_id`, `asked_at`, `playbook`, `inventory`, and
  `incident_id`. After a decision, `sudo mmhotline approval-requests show
  --approval-request-id ID --json` gives `state` `approved` or `refused`, and
  `decisions` with the stand-in's name.

### Show what the list of services does

[`check-whether-a-playbook-needs-a-person`](../reference/mmhotline-command.md#mmhotline-agent-playbook-rules-check-whether-a-playbook-needs-a-person)
shows on the real installation whether a playbook needs a person when the
triage agent asks for it, without a triage run, and changes nothing.
`check-whether-a-playbook-needs-a-person` checks as the agent's request is
checked, so a proof uses the command to show the effect of the
[list of services whose changes a person must approve](design/triage-agent.md#services-whose-changes-a-person-must-approve)
without waiting for the agent. The approval alert and the stand-in's
approval still need the agent's own request. With `pgautofailover` on the
list and Start or restart a service set to Act, then report, as the
recommended preset sets it, check a playbook that restarts it and one that
restarts another service:

```sh
for playbook in one_time_playbooks/2026-11-03_restart_postgres_on_db01.yml \
    one_time_playbooks/2026-11-03_restart_caddy_on_app002.yml; do
  sudo mmhotline agent-playbook-rules check-whether-a-playbook-needs-a-person \
      --inventory production --playbook "$playbook" --json \
    | python3 -c 'import json, sys; j = json.load(sys.stdin)["whether_a_playbook_needs_a_person"]; print(j["playbook"], j["commit_id"]); print("needs_a_person:", j["needs_a_person"], "-", j["rule_title"]); print(j["reason"])'
done
```

The first prints `needs_a_person: True - Get permission, then act (1 approval)`
with the list as the reason, and the second
`needs_a_person: False - Act, then report`. The playbooks must be in a commit
MMHotline's copy already holds, because the command fetches nothing; commit
them to the systems repository first, and the copy takes them at the start of
the next triage run or MMX run. The printed commit is the one checked. A
review agent, when one is set up, is not asked.

### Claim, resolve, approve, and refuse

To claim an SOS incident, read its page, then post the claim form, with the
incident's ID from `incidents list` in place of the example:

```sh
incident=cd1eec890f8a52f59c04f3f6982767a2
curl -sS -b stand-in-cookies.txt -c stand-in-cookies.txt -o incident.html -w '%{http_code}\n' \
  "$web/incidents/$incident"
csrf=$(grep -o 'name="csrf" value="[^"]*"' incident.html | head -n 1 | cut -d'"' -f4)
revision=$(grep -o 'name="revision" value="[0-9]*"' incident.html | head -n 1 | cut -d'"' -f4)
curl -sS -b stand-in-cookies.txt -o /dev/null -w '%{http_code}\n' --data-urlencode "csrf=$csrf" \
  --data-urlencode "revision=$revision" \
  --data-urlencode "note=A stand-in person claimed this SOS for a test." "$web/incidents/$incident/claim"
```

`note` is the page's optional **Note for the team (optional)**: one line of at most 500
characters, or `note=` for none. MMHotline keeps it in the incident's history
and gives it once to the triage agent: to the next run of this incident's
triage session, or else to the first run of a later session with this
incident or an alert of the same cause
([notes from the team](design/triage-agent.md#notes-from-the-team)), so a
proof's note tells the agent the SOS was a test. A longer note, or one with a
line break, gets `400`.

The claim changes the incident's revision. To resolve the incident, read the
page again and post the resolve form:

```sh
curl -sS -b stand-in-cookies.txt -c stand-in-cookies.txt -o incident.html -w '%{http_code}\n' \
  "$web/incidents/$incident"
csrf=$(grep -o 'name="csrf" value="[^"]*"' incident.html | head -n 1 | cut -d'"' -f4)
revision=$(grep -o 'name="revision" value="[0-9]*"' incident.html | head -n 1 | cut -d'"' -f4)
curl -sS -b stand-in-cookies.txt -o /dev/null -w '%{http_code}\n' --data-urlencode "csrf=$csrf" \
  --data-urlencode "revision=$revision" \
  --data-urlencode "note=This was a test with a stand-in person." "$web/incidents/$incident/resolve"
```

`incidents show` then gives `status` `claimed` and `claimed_by`
`Stand-in for Barbara`, and after the second post `resolved`; the `history`
entries `claimed` and `resolved` have the two notes in `note`.

To approve or refuse a playbook run, read the request's page, then post its
form, with the request's ID from `approval-requests list` in place of the
example. A refusal needs a reason of at most 500 characters:

```sh
approval=c00d4226c324e48948914145dd319875
curl -sS -b stand-in-cookies.txt -c stand-in-cookies.txt -o approval.html -w '%{http_code}\n' \
  "$web/approvals/$approval"
csrf=$(grep -o 'name="csrf" value="[^"]*"' approval.html | head -n 1 | cut -d'"' -f4)
curl -sS -b stand-in-cookies.txt -o /dev/null -w '%{http_code}\n' --data-urlencode "csrf=$csrf" \
  "$web/approvals/$approval/approve"
```

```sh
curl -sS -b stand-in-cookies.txt -o /dev/null -w '%{http_code}\n' --data-urlencode "csrf=$csrf" \
  --data-urlencode "reason=Refused by the stand-in to test a refusal." "$web/approvals/$approval/refuse"
```

A rule that needs 2 approvals needs two different people: add a
second stand-in, `stand-in-2@example.invalid`, with its own cookie file.

`TestStandInPersonActsThroughThePages` in
`internal/app/stand_in_person_test.go` follows the stand-in person steps on the
real web handler, with the examples' patterns for reading the fields. A change
to a form field or a route used by `TestStandInPersonActsThroughThePages`
fails the test.

### What a pass does not prove

A proof with a stand-in shows that MMHotline chose the stand-in, and that the
claim, resolve, approve, and refuse forms work. A stand-in proof does not prove:

- delivery to a device: nothing is sent to the stand-in, so no sending service,
  Pushover notification, phone call, or message takes part;
- a device's own claim, such as acknowledging a Pushover emergency, pressing a
  key in a phone call, or a Slack or Telegram button;
- the [MMHotline-down alarm](../how-to/set-up-the-mmhotline-down-alarm.md): it holds only the Pushover
  notifications and phone calls of the person on call, and the stand-in has
  none, so it holds nothing. If MMHotline itself goes down while the stand-in
  is on call, nobody is woken;
- Cloudflare Access: the agent signs in with a sign-in link;
- anything the server's own monitoring sends outside MMHotline.

### When the proofs end

Put the team's `people` and `on_call_schedule` back as they were, without
the stand-in, and apply the file; with a deploy tool, revert the change in
the systems repository. Leaving the stand-in out deactivates it. While it is
on the team and available, an SOS at a time when every real person is
unavailable reaches the stand-in alone, which wakes nobody, instead of every
member. Barbara chooses I'm available again on her On call
page, or waits for the end time. Last, delete `~/stand-in`, which holds the
cookies.

## Continuous integration

On GitHub, [`.github/workflows/checks.yml`](../../.github/workflows/checks.yml)
runs on every push and pull request, as MMX's and MMWatchdog's workflows do.
It runs `go test -timeout 30m ./...` on Linux and macOS, with Node.js 22 so
that the MMHotline-down alarm's Worker tests run too. On Linux, it also runs
`go vet ./...` and a `gofmt` check of `cmd` and `internal`, and builds the
four release archives with the development version and checks them against
`SHA256SUMS`. The workflow pins each action to a commit hash, and each job
checks that Go runs the `toolchain` that `go.mod` names.

The release checks stay off GitHub's runners: static analysis, Go tests
under stress, fuzzing, and the checks on real servers and sending services
run before a release, as [release gates](design/development.md#release-gates)
says. Tests that need a root test process, an `mmx` program, or Mailpit skip
on GitHub's runners, as they do on a developer's machine without them.

## Repository checks

Before committing specification changes, check relative file/anchor links,
question references, document ownership, and consistency of status claims.
Read edited sections as a whole and remove duplicate/conflicting statements.

Check changed pages in a browser against a locally served build: navigation,
forms, actions, console errors, and desktop/mobile layout. Keep temporary
screenshots and exploratory check scripts outside the repository.

## Test isolation

Use a temporary directory for disposable fixtures and build outputs, and set
`GOCACHE="$TMPDIR/go-build"`. Give test servers explicit loopback addresses
and ports; stop only processes started by the test. Never reuse another
application's state, databases, credentials, or cleanup targets. Ordinary tests
use SMTP fixtures or Mailpit for email and fake sending services for chat, push, and
calls. Never send a real phone call, chat message, push notification, or email
as a side effect of an ordinary test invocation.
