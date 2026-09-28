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

# Fresh + no busy marker, but the log says running -> still busy: the flag
# can be a poll interval stale and a job may have started since
rm -f "$PID_DIR/runner-2.ghbusy"
make_log 2 "2024-01-01 Running job: build"
t_ok "is_busy trusts a running log even when a fresh GitHub poll said not busy" is_busy 2

# Fresh + no busy marker + idle log -> not busy
make_log 2 "2024-01-01 Listening for Jobs"
t_fail_ok "is_busy is false when GitHub says not busy and the log is idle" is_busy 2

# Not known (no .ghseen) -> falls back to the log
rm -f "$PID_DIR/runner-3.ghseen" "$PID_DIR/runner-3.ghbusy"
make_log 3 "2024-01-01 Running job: build"
t_ok "is_busy falls back to the log (running) when GitHub data isn't known" is_busy 3

make_log 3 "2024-01-01 Listening for Jobs"
t_fail_ok "is_busy falls back to the log (idle) when GitHub data isn't known" is_busy 3

# ----------------------------------------------------------------------------
# get_runner_status override cases
# ----------------------------------------------------------------------------

# Log says running, GitHub says not busy -> the log wins (flag may be stale)
rm -f "$PID_DIR/runner-4.ghbusy"
touch "$PID_DIR/runner-4.ghseen"
make_log 4 "2024-01-01 Running job: build"
t_eq "running: build" "$(get_runner_status 4 | sed 's/ (.*//')" "get_runner_status: log running + GitHub not busy => still running"

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
gh() { case "$*" in *rate_limit*) printf '4000\t0\n' ;; *) printf 'runner-1\tonline\ttrue\n' ;; esac; }

check_github_health

t_ok ".ghseen is written for a runner present in the gh response" bash -c '[[ -f "'"$PID_DIR"'/runner-1.ghseen" ]]'
t_ok ".ghbusy is written when gh reports busy=true" bash -c '[[ -f "'"$PID_DIR"'/runner-1.ghbusy" ]]'
t_ok "gh-poll.ts is written after a successful poll" bash -c '[[ -f "'"$PID_DIR"'/gh-poll.ts" ]]'
t_fail_ok "a runner missing from the gh response loses its .ghseen marker" bash -c '[[ -f "'"$PID_DIR"'/runner-2.ghseen" ]]'
t_fail_ok "a runner missing from the gh response loses its .ghbusy marker" bash -c '[[ -f "'"$PID_DIR"'/runner-2.ghbusy" ]]'
t_eq "4000" "$(cut -f1 "$PID_DIR/gh.rate")" "the poll records the remaining rate limit in gh.rate"

# ----------------------------------------------------------------------------
# Rate limit: too little budget left -> no listing calls, runners untouched
# ----------------------------------------------------------------------------
CALLS="$TEST_TMP_DIR/gh.calls"
: > "$CALLS"
RESET=$(( $(date +%s) + 600 ))
gh() { echo "$*" >> "$CALLS"; case "$*" in *rate_limit*) printf '3\t%s\n' "$RESET" ;; *) printf 'runner-1\tonline\tfalse\n' ;; esac; }
dlog() { :; }
touch "$PID_DIR/runner-1.ghbusy"
check_github_health
t_eq "0" "$(grep -vc rate_limit "$CALLS")" "remaining 3: no runner listing is fetched"
t_eq "ratelimit" "$(gh_state)" "gh state becomes ratelimit"
case "$(cut -f3 "$PID_DIR/gh.err")" in *"resumes"*) t_eq 1 1 "the message says when polling resumes" ;; *) t_eq "...resumes..." "$(cut -f3 "$PID_DIR/gh.err")" "the message says when polling resumes" ;; esac
t_ok "markers are left alone" test -f "$PID_DIR/runner-1.ghbusy"
: > "$CALLS"
check_github_health
t_eq "0" "$(wc -l < "$CALLS" | tr -d ' ')" "until the reset time, not even rate_limit is called"
echo "3	$(( $(date +%s) - 1 ))	0" > "$PID_DIR/gh.rate"
gh() { echo "$*" >> "$CALLS"; case "$*" in *rate_limit*) printf '4000\t0\n' ;; *) printf 'runner-1\tonline\tfalse\n' ;; esac; }
check_github_health
t_ok "after the reset the poll runs again" grep -q 'actions/runners' "$CALLS"
t_eq "ok" "$(gh_state)" "and the ratelimit state clears"

# effective_health_ticks: GH_HEALTH_TICKS * max(1, ceil(N/10))
GH_HEALTH_TICKS=12
t_eq "12 12 12 24 48" "$(effective_health_ticks 0) $(effective_health_ticks 1) $(effective_health_ticks 10) $(effective_health_ticks 11) $(effective_health_ticks 35)" "effective_health_ticks for 0, 1, 10, 11, 35 targets"
