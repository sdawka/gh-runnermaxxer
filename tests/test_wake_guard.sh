#!/usr/bin/env bash
# Sleep/wake guard and busy guard for the GitHub-offline recycle (S1)
source "$(dirname "$0")/_helper.sh"
mkdir -p "$PID_DIR" "$LOG_DIR"
REFRESH_INTERVAL=5
GH_HEALTH_TICKS=12
dlog() { echo "$1" >> "$TEST_TMP_DIR/dlog"; }

# --- supervisor_tick notices a clock jump ------------------------------------------
supervise_runners() { :; }
check_github_health() { :; }
write_state_snapshot() { :; }
echo $(( $(date +%s) - 4 )) > "$PID_DIR/tick.ts"
supervisor_tick
t_fail_ok "a normal gap between ticks is not a wake" test -f "$PID_DIR/wake.ts"
t_ok "tick.ts is refreshed every tick" test "$(cat "$PID_DIR/tick.ts")" -ge $(( $(date +%s) - 1 ))
echo $(( $(date +%s) - 60 )) > "$PID_DIR/tick.ts"
supervisor_tick
t_ok "a 60 s gap at a 5 s interval writes wake.ts" test -f "$PID_DIR/wake.ts"
t_ok "and logs it" grep -q 'clock jumped' "$TEST_TMP_DIR/dlog"
t_ok "recently_woke right after" recently_woke
echo $(( $(date +%s) - 121 )) > "$PID_DIR/wake.ts"
t_fail_ok "recently_woke is false after 2 minutes" recently_woke
rm -f "$PID_DIR/wake.ts" "$PID_DIR/tick.ts"
supervisor_tick
t_fail_ok "no tick.ts yet (first tick) is not a wake" test -f "$PID_DIR/wake.ts"
source "$SCRIPT_UNDER_TEST"
dlog() { echo "$1" >> "$TEST_TMP_DIR/dlog"; }

# --- check_github_health recycle guards --------------------------------------------
URL="https://github.com/owner/repo"
make_runner_dir 1 "$URL"
RECYCLED="$TEST_TMP_DIR/recycled"
is_running() { [[ "$1" == "1" ]]; }
stop_runner_procs() { echo "$1" >> "$RECYCLED"; }
start_runner() { :; }
autoscale_tick() { :; }
echo ok > "$PID_DIR/gh.state"
BUSYFLAG=false
gh() { case "$*" in *rate_limit*) printf '4000\t0\n' ;; *) printf 'runner-1\toffline\t%s\n' "$BUSYFLAG" ;; esac; }

# offline_twice: the second offline sighting in a row would recycle
offline_twice() {
    : > "$RECYCLED"
    echo 1 > "$PID_DIR/runner-1.ghoffline"
    check_github_health
}

make_log 1 "2024-01-01 00:00:00Z: Listening for Jobs"
offline_twice
t_eq "1" "$(cat "$RECYCLED")" "baseline: offline twice -> recycled"

date +%s > "$PID_DIR/wake.ts"
offline_twice
t_eq "" "$(cat "$RECYCLED")" "just woke -> not recycled"
t_eq "1" "$(cat "$PID_DIR/runner-1.ghoffline")" "and the offline counter is not advanced"
t_ok "markers still updated after a wake" test -f "$PID_DIR/runner-1.ghseen"
rm -f "$PID_DIR/wake.ts"

make_log 1 "2024-01-01 00:00:00Z: Running job: build"
offline_twice
t_eq "" "$(cat "$RECYCLED")" "log shows a running job -> not recycled"
make_log 1 "2024-01-01 00:00:00Z: Listening for Jobs"

BUSYFLAG=true
offline_twice
t_eq "" "$(cat "$RECYCLED")" "GitHub says busy -> not recycled"
BUSYFLAG=false

offline_twice
t_eq "1" "$(cat "$RECYCLED")" "guards lifted -> recycled again"
