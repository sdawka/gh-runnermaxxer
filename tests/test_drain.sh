#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

mkdir -p "$PID_DIR"

URL="https://github.com/owner/repo"

# ----------------------------------------------------------------------------
# mark_draining / is_draining / unmark_draining
# ----------------------------------------------------------------------------

t_fail_ok "is_draining is false before marking" is_draining 1
mark_draining 1
t_ok "is_draining is true after mark_draining" is_draining 1
unmark_draining 1
t_fail_ok "is_draining is false after unmark_draining" is_draining 1

# ----------------------------------------------------------------------------
# clear_runner_state removes the draining marker
# ----------------------------------------------------------------------------

mark_draining 2
t_ok "sanity: runner-2 is draining before clear_runner_state" is_draining 2
clear_runner_state 2
t_fail_ok "clear_runner_state removes the draining marker" is_draining 2

# ----------------------------------------------------------------------------
# count_runners_for_target excludes draining runners while
# runner_ids_for_target still lists them
# ----------------------------------------------------------------------------

make_runner_dir 10 "$URL"
make_runner_dir 11 "$URL"
make_runner_dir 12 "$URL"

t_eq "3" "$(count_runners_for_target "$URL")" "count_runners_for_target counts all runners before draining"

mark_draining 11

ids=$(runner_ids_for_target "$URL" | tr '\n' ',')
t_eq "10,11,12," "$ids" "runner_ids_for_target still lists a draining runner"
t_eq "2" "$(count_runners_for_target "$URL")" "count_runners_for_target excludes the draining runner"

unmark_draining 11

# ----------------------------------------------------------------------------
# pick_victims skips draining runners
# ----------------------------------------------------------------------------

is_busy() { return 1; }  # everyone idle
mark_draining 11
result=$(pick_victims "$URL" 10 | tr '\n' ',')
t_eq "12,10," "$result" "pick_victims excludes the draining runner"
unmark_draining 11

# ----------------------------------------------------------------------------
# supervise_runners removes a draining runner once is_busy is false, and
# does not restart it
# ----------------------------------------------------------------------------

rm -rf "$RUNNER_BASE_DIR"/runner-*
rm -f "$PID_DIR"/runner-*

make_runner_dir 20 "$URL"
mark_draining 20

REMOVED_LOG="$TEST_TMP_DIR/removed.log"
START_LOG="$TEST_TMP_DIR/started.log"
: > "$REMOVED_LOG"
: > "$START_LOG"

rotate_log() { :; }
is_running() { return 1; }
remove_runner() { echo "$1" >> "$REMOVED_LOG"; }
start_runner() { echo "$1" >> "$START_LOG"; }

# Not busy -> removed, drain marker cleared, never restarted
is_busy() { return 1; }
supervise_runners
t_eq "20" "$(cat "$REMOVED_LOG" | tr -d '\n')" "supervise_runners removes a draining runner once it is not busy"
t_fail_ok "supervise_runners clears the draining marker after removal" is_draining 20
t_eq "" "$(cat "$START_LOG")" "supervise_runners never restarts a draining runner"

# Busy -> left alone, still draining, not removed
# (remove_runner is stubbed above and never actually deletes the directory,
# so clear it manually to keep this block isolated from the previous one)
rm -rf "$RUNNER_BASE_DIR"/runner-*
rm -f "$PID_DIR"/runner-*
: > "$REMOVED_LOG"
: > "$START_LOG"
make_runner_dir 21 "$URL"
mark_draining 21
is_busy() { return 0; }
supervise_runners
t_eq "" "$(cat "$REMOVED_LOG")" "supervise_runners leaves a busy draining runner alone"
t_ok "a busy draining runner is still draining afterwards" is_draining 21
t_eq "" "$(cat "$START_LOG")" "a busy draining runner is not restarted either"
