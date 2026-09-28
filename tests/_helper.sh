#!/usr/bin/env bash
# ============================================================================
# Shared setup for test_*.sh files.
#
# Source this at the top of a test file:
#   source "$(dirname "$0")/_helper.sh"
#
# It creates a fresh temp RUNNER_BASE_DIR, sources runnermaxxer.sh as a
# library (RUNNERMAXXER_LIB=1, so main/argument-parsing never runs and no
# real runner/gh/pgrep/config.sh call ever happens), points TARGETS_FILE and
# CONFIG_FILE at temp paths, and cleans everything up on exit.
# ============================================================================

SCRIPT_UNDER_TEST="${RUNNERMAXXER_SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/runnermaxxer.sh}"

# Assertion helpers. Each test_*.sh file runs as its own bash process (see
# tests/run.sh), so these are plain functions rather than shared counters;
# tests/run.sh tallies totals by grepping "  PASS ...\n"/"  FAIL ..." lines
# out of each file's captured output.
t_eq() {
    # t_eq expected actual "name"
    local expected="$1" actual="$2" name="$3"
    if [[ "$expected" == "$actual" ]]; then
        echo "  PASS $name"
    else
        echo "  FAIL $name"
        echo "       expected: $expected"
        echo "       actual:   $actual"
    fi
}

t_ok() {
    # t_ok "name" cmd...  -- asserts the command SUCCEEDS (exit 0)
    local name="$1"
    shift
    if "$@"; then
        echo "  PASS $name"
    else
        echo "  FAIL $name (command: $*)"
    fi
}

t_fail_ok() {
    # t_fail_ok "name" cmd...  -- asserts the command FAILS (non-zero)
    local name="$1"
    shift
    if ! "$@"; then
        echo "  PASS $name"
    else
        echo "  FAIL $name (expected failure, command: $*)"
    fi
}

TEST_TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/runnermaxxer-test.XXXXXX")"
export RUNNER_BASE_DIR="$TEST_TMP_DIR/runners"
mkdir -p "$RUNNER_BASE_DIR"

export RUNNERMAXXER_LIB=1
# shellcheck source=/dev/null
source "$SCRIPT_UNDER_TEST"

# The script itself installs `trap cleanup EXIT` (harmless when sourced: it
# only removes a lock file this process never acquired, and restores tty
# state that was never touched). Install ours AFTER sourcing so it isn't
# clobbered, and chain to the script's own cleanup too.
_helper_cleanup() {
    cleanup 2>/dev/null || true
    rm -rf "$TEST_TMP_DIR" 2>/dev/null || true
}
trap _helper_cleanup EXIT

# Point plain-variable config/targets files at the temp dir too, since they
# default to living next to the script itself.
CONFIG_FILE="$TEST_TMP_DIR/.runnermaxxer.conf"
TARGETS_FILE="$TEST_TMP_DIR/.runnermaxxer.targets"

# Helper: create a fake runner-N directory with a .runner file whose
# gitHubUrl field is $2 (mimics runner_registered_url's expectations).
make_runner_dir() {
    local id="$1" url="${2:-}"
    mkdir -p "$RUNNER_BASE_DIR/runner-$id"
    if [[ -n "$url" ]]; then
        cat > "$RUNNER_BASE_DIR/runner-$id/.runner" << EOF
{
  "agentName": "runner-$id",
  "gitHubUrl": "$url"
}
EOF
    fi
}

# Helper: write fixture log lines for runner-N.
make_log() {
    local id="$1"
    shift
    mkdir -p "$LOG_DIR"
    printf '%s\n' "$@" > "$LOG_DIR/runner-$id.log"
}

# Helper: PATH shim for gh, for end-to-end runs of the script in a fresh
# process (where function stubs don't exist). SCRIPT_BODY is /bin/sh code
# that sees gh's arguments as "$@". Prints the directory to put first in
# PATH:  PATH="$(make_gh_shim '...'):$PATH"
make_gh_shim() {
    local dir="$TEST_TMP_DIR/bin"
    mkdir -p "$dir"
    printf '#!/bin/sh\n%s\n' "$1" > "$dir/gh"
    chmod +x "$dir/gh"
    echo "$dir"
}

# Helper: a tiny runner tarball whose config.sh records its argv (one line
# per call) in $FAKE_CONFIG_LOG and writes .runner like the real one.
# FAKE_CONFIG_RC sets its exit status for registrations (not for "remove").
# Prints the tarball path.
export FAKE_CONFIG_LOG="$TEST_TMP_DIR/config.argv"
make_fake_tarball() {
    local ver="${1:-2.334.0}" src="$TEST_TMP_DIR/fake-runner-src" out
    rm -rf "$src"; mkdir -p "$src/bin"
    cat > "$src/config.sh" << 'SH'
#!/bin/sh
echo "$*" >> "$FAKE_CONFIG_LOG"
if [ "$1" = remove ]; then rm -f .runner; exit 0; fi
name=""; url=""
while [ $# -gt 0 ]; do
    case "$1" in
        --name) name=$2; shift ;;
        --url)  url=$2; shift ;;
    esac
    shift
done
printf '{\n  "agentName": "%s",\n  "gitHubUrl": "%s"\n}\n' "$name" "$url" > .runner
exit "${FAKE_CONFIG_RC:-0}"
SH
    printf '#!/bin/sh\nsleep 30\n' > "$src/run.sh"
    chmod +x "$src/config.sh" "$src/run.sh"
    out="$TEST_TMP_DIR/actions-runner-osx-arm64-$ver.tar.gz"
    tar -czf "$out" -C "$src" .
    echo "$out"
}
