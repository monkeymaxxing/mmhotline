#!/bin/sh
# Runs MMHotline's fuzz targets one at a time, as a release step.
# docs/changing-mmhotline/testing.md, "Fuzzing for a release", says where and how to run it.
set -eu
cd "$(dirname "$0")/.."
trap 'exit 1' HUP INT TERM

print_usage() {
  cat <<'USAGE'
Usage: scripts/run-fuzz-tests.sh [--fuzz-time-per-target DURATION] [--fuzz-workers COUNT]

Run MMHotline's thirteen fuzz targets one at a time from the repository root.

  --fuzz-time-per-target DURATION  Go -fuzztime value per target (default: 10m)
  --fuzz-workers COUNT             Go -parallel value per target (default: 4)
  --help                           Print this help and exit

The script continues after a target fails and exits non-zero if any target
failed, including a target name that matches no fuzz target.
USAGE
}

fuzz_time_per_target=10m
fuzz_workers=4
while [ "$#" -gt 0 ]; do
  case "$1" in
    --fuzz-time-per-target)
      if [ "$#" -lt 2 ]; then
        print_usage >&2
        exit 2
      fi
      fuzz_time_per_target=$2
      shift 2
      ;;
    --fuzz-workers)
      if [ "$#" -lt 2 ]; then
        print_usage >&2
        exit 2
      fi
      fuzz_workers=$2
      shift 2
      ;;
    --help)
      print_usage
      exit 0
      ;;
    *)
      print_usage >&2
      exit 2
      ;;
  esac
done

# MMHotline builds and tests with cgo disabled.
CGO_ENABLED=0
export CGO_ENABLED
# A fuzz worker stopped at the end of a target leaves its last input's
# temporary directories behind, so the run gets its own and removes it.
TMPDIR=$(mktemp -d "${TMPDIR:-/tmp}/mmhotline-fuzz-tests.XXXXXX")
export TMPDIR
trap 'rm -rf "$TMPDIR"' EXIT

fuzz_failed=0
while read -r package_path fuzz_target; do
  printf '\n== Fuzz %s in %s ==\n' "$fuzz_target" "$package_path"
  # go test fuzzes nothing and passes when -fuzz matches no target, so a
  # renamed target would pass unnoticed.
  if ! go test "$package_path" -list="^${fuzz_target}\$" | grep -qx "$fuzz_target"; then
    printf 'Failed: %s in %s: no fuzz target has this name\n' "$fuzz_target" "$package_path"
    fuzz_failed=1
    continue
  fi
  # Go minimizes each input that finds new coverage for up to 60 seconds.
  # Coverage of the same input differs from run to run in most targets,
  # through random IDs, goroutines, and RSA blinding, so with that default
  # the workers spent most of each target minimizing: on a 12-thread
  # server, 94 inputs of FuzzAgentCommitsBundle in 30 seconds instead of
  # 1,426, and 63,891 of FuzzAlertSourceRequestBody instead of 296,995.
  if go test "$package_path" \
    -run='^$' \
    -fuzz="^${fuzz_target}\$" \
    -fuzztime="$fuzz_time_per_target" \
    -fuzzminimizetime=10x \
    -parallel="$fuzz_workers"; then
    printf 'Passed: %s in %s\n' "$fuzz_target" "$package_path"
  else
    printf 'Failed: %s in %s\n' "$fuzz_target" "$package_path"
    fuzz_failed=1
  fi
done <<'FUZZ_TARGETS'
./internal/app FuzzAlertSourceRequestBody
./internal/app FuzzAlertSourceSignature
./internal/cloudflareaccess FuzzCloudflareAccessToken
./internal/app FuzzSlackInteractiveEnvelope
./internal/app FuzzTelegramUpdate
./internal/app FuzzTwilioCallAnswer
./internal/app FuzzTriageAgentRequest
./internal/systemsrepository FuzzAgentCommitsBundle
./internal/actionkinds FuzzMMXExplainOutput
./internal/app FuzzFleetView
./internal/app FuzzChecklistYAML
./internal/statuspage FuzzStatusJSON
./internal/app FuzzCustomScheduleSandboxOutput
FUZZ_TARGETS
exit "$fuzz_failed"
