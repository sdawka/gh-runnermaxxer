#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

# check_tarball_freshness parses the version out of the tarball name using
# $RUNNER_OS/$RUNNER_ARCH; detect_platform() never runs under
# RUNNERMAXXER_LIB=1, so set them explicitly to match the fake filename.
RUNNER_OS="osx"
RUNNER_ARCH="arm64"
RUNNER_TAR="/fake/path/actions-runner-osx-arm64-2.334.0.tar.gz"
CACHE_FILE="$RUNNER_BASE_DIR/.latest-runner-tag"
GH_CALLS="$TEST_TMP_DIR/gh_calls"

# ----------------------------------------------------------------------------
# First call: no cache -> calls gh, prints the notice, writes the cache file
# ----------------------------------------------------------------------------

rm -f "$CACHE_FILE" "$GH_CALLS"
gh() { echo "call" >> "$GH_CALLS"; echo "v2.337.0"; }

out=$(check_tarball_freshness)
case "$out" in
    *"2.334.0"*"2.337.0"*) t_eq "1" "1" "check_tarball_freshness prints the notice with old/new versions" ;;
    *) t_eq "notice mentioning 2.334.0 and 2.337.0" "$out" "check_tarball_freshness prints the notice with old/new versions" ;;
esac
t_ok "check_tarball_freshness writes the cache file" bash -c '[[ -f "'"$CACHE_FILE"'" ]]'
t_eq "1" "$(wc -l < "$GH_CALLS" | tr -d ' ')" "check_tarball_freshness calls gh exactly once on a cold cache"

# ----------------------------------------------------------------------------
# Second call within 24h: reuses the cache, does not invoke gh again
# ----------------------------------------------------------------------------

out2=$(check_tarball_freshness)
t_eq "1" "$(wc -l < "$GH_CALLS" | tr -d ' ')" "a second call within 24h does not invoke gh again"
case "$out2" in
    *"2.334.0"*"2.337.0"*) t_eq "1" "1" "the second call still prints the notice from the cached tag" ;;
    *) t_eq "notice mentioning 2.334.0 and 2.337.0" "$out2" "the second call still prints the notice from the cached tag" ;;
esac

# ----------------------------------------------------------------------------
# gh failing with no cache -> prints nothing, returns 0
# ----------------------------------------------------------------------------

rm -f "$CACHE_FILE" "$GH_CALLS"
gh() { echo "call" >> "$GH_CALLS"; return 1; }

out3=$(check_tarball_freshness)
rc=$?
t_eq "" "$out3" "check_tarball_freshness prints nothing when gh fails and there is no cache"
t_eq "0" "$rc" "check_tarball_freshness still returns 0 when gh fails"
t_fail_ok "no cache file is left behind when gh fails" bash -c '[[ -f "'"$CACHE_FILE"'" ]]'
