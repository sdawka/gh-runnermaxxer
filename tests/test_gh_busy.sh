#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

mkdir -p "$PID_DIR"

GH_HEALTH_TICKS=12
REFRESH_INTERVAL=5
# max staleness = GH_HEALTH_TICKS * REFRESH_INTERVAL * 2 = 120s (floor 30s)

# ----------------------------------------------------------------------------
# gh_data_fresh
# ----------------------------------------------------------------------------

rm -f "$PID_DIR/gh-poll.ts"
t_fail_ok "gh_data_fresh is false with no poll file" gh_data_fresh

echo "$(date +%s)" > "$PID_DIR/gh-poll.ts"
t_ok "gh_data_fresh is true with a fresh timestamp" gh_data_fresh

echo "$(( $(date +%s) - 121 ))" > "$PID_DIR/gh-poll.ts"
t_fail_ok "gh_data_fresh is false with a stale timestamp" gh_data_fresh

echo "$(date +%s)" > "$PID_DIR/gh-poll.ts"
GH_HEALTH_TICKS=0
t_fail_ok "gh_data_fresh is false when GH_HEALTH_TICKS=0" gh_data_fresh
GH_HEALTH_TICKS=12

# ----------------------------------------------------------------------------
# gh_known
# ----------------------------------------------------------------------------

echo "$(date +%s)" > "$PID_DIR/gh-poll.ts"
rm -f "$PID_DIR/runner-1.ghseen"
t_fail_ok "gh_known is false without a .ghseen marker" gh_known 1

touch "$PID_DIR/runner-1.ghseen"
t_ok "gh_known is true with a fresh poll and a .ghseen marker" gh_known 1

echo "$(( $(date +%s) - 121 ))" > "$PID_DIR/gh-poll.ts"
t_fail_ok "gh_known is false once the poll data goes stale" gh_known 1
echo "$(date +%s)" > "$PID_DIR/gh-poll.ts"

# ----------------------------------------------------------------------------
# is_busy: prefers the .ghbusy marker when GitHub data is fresh, falls back
# to the log otherwise
# ----------------------------------------------------------------------------

is_running() { return 0; }  # every id is "running" for this section

# Fresh + busy marker present -> busy regardless of the log
make_log 2 "2024-01-01 Listening for Jobs"
touch "$PID_DIR/runner-2.ghseen"
touch "$PID_DIR/runner-2.ghbusy"
t_ok "is_busy is true from the .ghbusy marker even though the log says idle" is_busy 2

# Fresh + no busy marker -> idle even though the log says running
rm -f "$PID_DIR/runner-2.ghbusy"
make_log 2 "2024-01-01 Running job: build"
t_fail_ok "is_busy is false when GitHub data is fresh and no .ghbusy marker exists" is_busy 2

# Not known (no .ghseen) -> falls back to the log
rm -f "$PID_DIR/runner-3.ghseen" "$PID_DIR/runner-3.ghbusy"
make_log 3 "2024-01-01 Running job: build"
t_ok "is_busy falls back to the log (running) when GitHub data isn't known" is_busy 3

make_log 3 "2024-01-01 Listening for Jobs"
t_fail_ok "is_busy falls back to the log (idle) when GitHub data isn't known" is_busy 3

# ----------------------------------------------------------------------------
# get_runner_status override cases
# ----------------------------------------------------------------------------

# Log says running, GitHub says not busy -> "idle"
rm -f "$PID_DIR/runner-4.ghbusy"
touch "$PID_DIR/runner-4.ghseen"
make_log 4 "2024-01-01 Running job: build"
t_eq "idle" "$(get_runner_status 4)" "get_runner_status: log running + GitHub not busy => idle"

# Log says idle, GitHub says busy -> "busy (per GitHub)"
touch "$PID_DIR/runner-5.ghseen"
touch "$PID_DIR/runner-5.ghbusy"
make_log 5 "2024-01-01 Listening for Jobs"
t_eq "busy (per GitHub)" "$(get_runner_status 5)" "get_runner_status: log idle + GitHub busy => busy (per GitHub)"

# ----------------------------------------------------------------------------
# check_github_health: parses name<TAB>status<TAB>busy rows, writes
# .ghseen/.ghbusy markers and gh-poll.ts, and a runner missing from the
# response loses its markers
# ----------------------------------------------------------------------------

rm -rf "$RUNNER_BASE_DIR"/runner-*
rm -f "$PID_DIR"/runner-* "$PID_DIR/gh-poll.ts"

URL="https://github.com/owner/repo"
make_runner_dir 1 "$URL"
make_runner_dir 2 "$URL"

# runner-2 already had markers from a previous poll; the new response won't
# mention it, so it should lose them.
touch "$PID_DIR/runner-2.ghseen" "$PID_DIR/runner-2.ghbusy"

is_running() { [[ "$1" == "1" || "$1" == "2" ]]; }
gh() { printf 'runner-1\tonline\ttrue\n'; }

check_github_health

t_ok ".ghseen is written for a runner present in the gh response" bash -c '[[ -f "'"$PID_DIR"'/runner-1.ghseen" ]]'
t_ok ".ghbusy is written when gh reports busy=true" bash -c '[[ -f "'"$PID_DIR"'/runner-1.ghbusy" ]]'
t_ok "gh-poll.ts is written after a successful poll" bash -c '[[ -f "'"$PID_DIR"'/gh-poll.ts" ]]'
t_fail_ok "a runner missing from the gh response loses its .ghseen marker" bash -c '[[ -f "'"$PID_DIR"'/runner-2.ghseen" ]]'
t_fail_ok "a runner missing from the gh response loses its .ghbusy marker" bash -c '[[ -f "'"$PID_DIR"'/runner-2.ghbusy" ]]'
