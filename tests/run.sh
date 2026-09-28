#!/usr/bin/env bash
# ============================================================================
# Minimal plain-bash test harness for runnermaxxer.sh
# ============================================================================
# No external framework (bats not required). Runs every tests/test_*.sh file
# in its own subshell/process, each of which sources runnermaxxer.sh as a
# library (RUNNERMAXXER_LIB=1) against a fresh temp RUNNER_BASE_DIR.
#
# Usage: ./tests/run.sh

set -uo pipefail

TESTS_DIR="$(cd "$(dirname "$0")" && pwd)"
export RUNNERMAXXER_SCRIPT="$TESTS_DIR/../runnermaxxer.sh"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

TOTAL_PASS=0
TOTAL_FAIL=0
CURRENT_FILE=""

# ----------------------------------------------------------------------------
# Assertion helpers - callable from within a test_*.sh file
# ----------------------------------------------------------------------------

# Note: each test_*.sh file runs as its OWN bash process (see run_one below),
# so t_eq/t_ok/t_fail_ok can't share counters with this process directly.
# They are defined identically in tests/_helper.sh (sourced by every test
# file) and simply print "  PASS name" / "  FAIL name" lines; this harness
# tallies totals by grepping each file's captured output.

# ----------------------------------------------------------------------------
# Run each test file in its own bash process so state never leaks between
# files. Each file prints PASS/FAIL lines (via tests/_helper.sh's t_eq etc.);
# we tally totals by grepping its captured output.
# ----------------------------------------------------------------------------

run_one() {
    local f="$1"
    echo ""
    echo "== $(basename "$f") =="
    bash "$f"
}

FAIL_FILES=()

for f in "$TESTS_DIR"/test_*.sh; do
    [[ -e "$f" ]] || continue
    frc=0
    out=$(run_one "$f" 2>&1) || frc=$?
    # A file that dies part-way (set -e in the sourced script, a typo)
    # would otherwise just report fewer PASS lines
    if [[ $frc -ne 0 ]]; then
        out="$out"$'\n'"  FAIL $(basename "$f") exited with status $frc before finishing"
    fi
    echo "$out"
    # Strip ANSI colour codes before counting (they sit between the marker
    # word and the surrounding spaces, so a plain grep on '  PASS ' misses).
    plain=$(printf '%s\n' "$out" | sed -E 's/\x1b\[[0-9;]*m//g')
    file_pass=$(printf '%s\n' "$plain" | grep -c '  PASS ')
    file_fail=$(printf '%s\n' "$plain" | grep -c '  FAIL ')
    TOTAL_PASS=$((TOTAL_PASS + file_pass))
    TOTAL_FAIL=$((TOTAL_FAIL + file_fail))
    [[ $file_fail -gt 0 ]] && FAIL_FILES+=("$(basename "$f")")
done

echo ""
echo "============================================================"
echo -e "Total: ${GREEN}$TOTAL_PASS passed${NC}, ${RED}$TOTAL_FAIL failed${NC}"
if [[ ${#FAIL_FILES[@]} -gt 0 ]]; then
    echo -e "${RED}Failing files:${NC} ${FAIL_FILES[*]}"
fi

[[ $TOTAL_FAIL -eq 0 ]]
