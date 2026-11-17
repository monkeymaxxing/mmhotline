# Development and build policy

## Go and dependencies

The application is Go, including built-in alert source types, notification
integrations, and the built-in schedule types.
Prefer the standard library; add a small dependency when it solves an actual
requirement clearly. Every dependency, including transitive ones, must build
with `CGO_ENABLED=0` on supported targets.
A Go wrapper around a required C library does not satisfy the policy.

The application uses one Go module and a small entry point. Packages follow
concrete responsibilities: application operations, storage, contact method type adapters,
and the custom schedule sandbox. Do not create an empty framework for future
features.

Storage is SQLite through `modernc.org/sqlite`
([storage](operation.md#storage)).

The dashboard reads the systems repository's checklist with `go.yaml.in/yaml/v3`, the version MMX
uses, because the checklist is YAML with folded text and comments
([interface](interface.md#checklist)).

The interface is server-rendered with `net/http` and
[templ](web-pages.md) (`github.com/a-h/templ`, MIT license), plus small
browser enhancements. templ compiles pages to Go, so a broken page fails the
build instead of a request. Keep build
and runtime dependencies explicit. Define API and command contracts and
examples with their implementation.

## Development process

- The [README](../../../README.md) is the user entry point. The [design plan](README.md)
  owns product principles; [acceptance contracts](../acceptance.md) own required workflows.
  The [documentation map](../documentation-map.md) identifies every document's reader and subject.
- Design documents describe built behavior. The design documents'
  [index](README.md#planned) lists planned work separately.
- Implementation rules make ownership, state transitions, and external effects
  easy to follow in one bounded review.
- Tests verify user workflows and meaningful failures. Update implementation,
  tests, and owning documentation together.
- Usage and schedule-code guides describe current behavior.
- Validation distinguishes local checks, platform checks, and real sending
  service acceptance.
  A fake sending service cannot prove real delivery.
- Before the first release, there is no compatibility obligation to disposable
  development state. Delete superseded requirements and code.

## Build and test commands

The module pins Go 1.27.0 with toolchain Go 1.27.4. Run the formatting, vet,
test, and build commands below from the repository root:

```sh
export GOCACHE="${TMPDIR:-/tmp}/go-build"
go tool templ generate
gofmt -l .
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -timeout 30m ./...
CGO_ENABLED=0 go build -o "${TMPDIR:-/tmp}/mmhotline" ./cmd/mmhotline
```

`go tool templ generate` must leave `git status` unchanged: the generated
`*_templ.go` files are committed ([web pages](web-pages.md)). `gofmt -l .`
must print nothing. Vet and tests must pass. The build writes the
`mmhotline` program into the temporary directory, with cgo disabled.

Run platform and sandbox checks on a Linux AMD64 server. Inspect release
artifacts for unexpected dynamic dependencies and run them in clean target
environments. A successful cross-build alone does not prove platform support.

The Go race detector may run separately; it requires cgo and a C compiler on
supported hosts. Using cgo for the race detector is a test-tool exception,
not a product dependency.
Normal application builds and tests remain cgo-disabled.

Prefer Go tests and small shell workflows. Browser checks exercise the actual
interface. No second application language or harness framework is required.

## Release archives

A release is four archives and their checksums: two for Linux servers, and
two for a responder's Mac. The Mac
[posts to the status page](mmhotline-down-alarm-and-status-page.md#posting-while-the-mmhotline-server-is-down)
while the MMHotline server is down. `scripts/build-release-archives.py`
builds the four archives and `SHA256SUMS` from one commit,
on macOS or Linux with Go, Git, and Python 3:

```sh
scripts/build-release-archives.py --version 0.1.0 --commit v0.1.0 \
  --output-dir "$HOME/mmhotline-releases/0.1.0"
```

`--version` is the release's semantic version without a `v`, such as `0.1.0`
or `0.1.0-rc.1`. The example uses Git tag `v0.1.0` for release `0.1.0`.
`--commit` names the commit, by tag or hash.
On GitHub, pushing a tag such as `v0.1.0` runs
[`.github/workflows/release.yml`](../../../.github/workflows/release.yml):
it refuses the development version that `cmd/mmhotline/main.go` names,
builds the archives with this script from the tagged commit, checks them
against `SHA256SUMS`, attests where each archive was built, and attaches
the archives and `SHA256SUMS` to a GitHub release of the tag. A version
with a hyphen, such as `0.1.0-rc.1`, becomes a pre-release.
`scripts/build-release-archives.py` refuses an output directory that already
holds the release's files. The script writes:

- `mmhotline-0.1.0-linux-amd64.tar.gz` for x86-64 servers and
  `mmhotline-0.1.0-linux-arm64.tar.gz` for AArch64 servers.
- `mmhotline-0.1.0-macos-arm64.tar.gz` for a Mac with Apple silicon and
  `mmhotline-0.1.0-macos-amd64.tar.gz` for a Mac with an Intel processor.
  The name says `macos`, as MMX's archives do; Go calls the system `darwin`.
- `SHA256SUMS`, one line for each archive, in the format that
  `sha256sum --check` and `shasum -a 256 --check` read.

Each archive holds one folder, such as `mmhotline-0.1.0-linux-amd64/`, with:

- `mmhotline`, the program, mode 0755. A Linux program is statically linked;
  a macOS program links only the system library, as every macOS program
  does, and carries the ad hoc signature Go's linker gives it. Apple has not
  notarized the macOS `mmhotline` program.
- `LICENSE`, MMHotline's Apache License 2.0.
- `dependency_license_notices/go/LICENSE`, the license of Go, whose standard
  library is compiled into the program.
- `dependency_license_notices/MODULE_PATH/`, for each Go module compiled into
  the program for the archive's operating system and processor architecture.
  The directory contains the `LICENSE`, `LICENCE`, `COPYING`, and
  `NOTICE` files at the module's root, such as
  `dependency_license_notices/modernc.org/sqlite/LICENSE-SQLITE`. The program's
  own build information, read with `go version -m`, lists the modules. A
  module without such a file stops the build, so a new dependency never
  ships without its license.

`scripts/build-release-archives.py` clones the local checkout's Git repository
into a temporary directory and checks out the commit named by `--commit`.
The release script then builds each program with the toolchain that `go.mod`
names, `CGO_ENABLED=0`, `-trimpath`, and `-ldflags "-X main.version=VERSION"`.
The release script stops when the program's build information names a commit
different from the commit named by `--commit`, a modified checkout, or cgo.

The reasons:

- **A clean checkout of a named commit.** Uncommitted changes and untracked
  files never reach a release. Go records the commit and an unmodified
  checkout in the program, so `mmhotline version` prints the release's
  version and commit.
- **One folder in each archive.** Unpacking never scatters files in the
  current directory. An MMX `unpack_archive` task with
  `unpack_without_wrapping_folder: true` leaves the folder out, so the program
  lands at `RELEASE_DIRECTORY/mmhotline`.
- **The program and license notices only.** `mmhotline install` needs nothing
  else on the server. The documentation is read where the source is
  published.
- **macOS archives for the status page only.** A responder posts from a
  laptop when the server is down, so the laptop needs the program; the laptop
  runs no service, and `mmhotline install` works only on Linux. The
  [Post to the status page](../../how-to/post-to-the-status-page.md#from-your-laptop) downloads
  the macOS release archive with `curl`, because a browser marks a download as
  quarantined and macOS then refuses to open a program that Apple has not
  notarized.
- **No build record beside the archives.** `mmhotline version --json` prints
  the version, the commit, whether the checkout was modified, and the Go
  version from the program itself, and `SHA256SUMS` holds the archives'
  hashes.
- **Rebuilding a commit with an unchanged release version produces identical
  archive bytes.** Every archive entry has the commit's time, root as owner,
  and a fixed mode and order. A developer can rebuild the published commit
  and version, then compare the rebuilt archive's checksum with the published
  archive's checksum.

Only a run of the program shows the program's version.
Go's linker ignores `-X` for a variable that does not exist.
A renamed `version` variable would leave
`mmhotline version` reporting `0.1.0-dev` even though `go build` succeeded.
The [release archive install
check](../testing.md#release-archive-install-check) reads the installed
program's version on each supported distribution, and the macOS program's on
a Mac.

## Environments and external actions

Keep disposable binaries, browser captures, test stores, and sending service fixtures
in a temporary directory. Isolate the development and test runs' ports,
processes, and credentials from real team state.

Development and ordinary tests use the built
[fake sending services](../testing.md#fake-sending-services): an in-process package for Go
tests and the separate `mmhotline-fake-sending-services` program for development and
Linux test suites. `MMHOTLINE_FAKE_SENDING_SERVICES_URL` points chat, push, and phone
adapters at the fake Telegram, Slack, Pushover, and Twilio services.

Email still uses the saved SMTP setup.
Use Mailpit to keep development email local.

Real sending service tests require a
designated account and contact method, with explicit scope for paid calls.
Record the real sending service tests' results in
[validation](../validation.md).

## Release gates

A release requires contract workflows on the selected artifacts, real contact method type
evidence, documented operation and recovery, checksums, a version, and automated
checks. Broad fault matrices and long soaks are release
checks, not routine gates for every edit.

Additional static analysis and fuzzing run at release. `staticcheck`,
`errcheck`, `exhaustive`, `nilaway`, and `govulncheck` report nothing except
reviewed exceptions, each with a one-line reason beside it, through
`scripts/static-analysis.sh`. Every Go test passes under stress:
`scripts/run-go-tests-under-stress.sh` runs every test 40 times, 4 runs at
once, with the race detector, on a Linux test machine. A test that fails
once is fixed by the [rules in the testing
guide](../testing.md#go-tests-that-never-depend-on-the-machines-speed),
never retried or skipped. Fuzz tests cover each place where outside input
enters MMHotline:

- Alert intake and check-in bodies, and the `Mmhotline-Signature` check.
- Cloudflare Access tokens.
- Slack interactive actions.
- Telegram updates.
- Twilio's responses when MMHotline fetches a call's status.
- The triage agent's requests and the Git bundles of the agent's commits.
- `mmx explain` output.
- The fleet view.
- The checklist.
- The status page's `status.json`.
- The custom schedule sandbox's output.

`scripts/run-fuzz-tests.sh` runs each fuzz test for 10 minutes on a Linux test
server. Fix failing inputs at their root cause and keep them as ordinary
regression tests. The [testing guide](../testing.md#fuzzing-for-a-release)
owns test commands.

Whenever the Litestream version documented in
[continuous backup and restore](operation.md#continuous-backup-and-restore)
changes, run
[`sudo mmhotline backup check-uploads`](operation.md#the-upload-check) on a
test server that backs up to a real bucket with the documented version,
before the release. The check depends on Litestream 0.5's local directory
layout for captured database changes, the LTX header of a captured change,
its `meta-path` configuration setting, and the uploaded transaction IDs in
`ltx` output ([data read by the check](operation.md#the-upload-check)).
`sudo mmhotline backup check-uploads` must exit 0 while uploads keep up.
Then test stopped uploads and stopped capture:

1. Use a key in `backup.env` that may list the bucket but not write to the
   bucket.
   Restart `mmhotline-backup`, then make a change.
   `sudo mmhotline backup check-uploads` must exit 1 once the change is more
   than 5 minutes old.
2. With a wrong key, `sudo mmhotline backup check-uploads` must exit 3 and
   show Litestream's error without the key.
3. With the right key, take write permission on Litestream's captured
   changes away from Litestream:
   `sudo chmod 0500 /var/lib/mmhotline-backup/mmhotline.db-litestream/ltx/0`.
   Make a change. `sudo mmhotline backup check-uploads` must exit 1 with
   `MMHotline's backup has stopped` within about 30 seconds. Give the
   permission back with `sudo chmod 0700` on the same directory.

Record the results in
[validation](../validation.md#continuous-backup-to-object-storage).
