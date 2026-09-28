#!/usr/bin/env bash
# runner_exit_reason / runner_exit_note, their use in supervise_runners,
# runner_version and runner_main_pid
source "$(dirname "$0")/_helper.sh"
mkdir -p "$PID_DIR" "$LOG_DIR"

L="2024-01-01 00:00:00Z"
TAB=$'\t'

# --- runner_exit_reason: the run-helper's messages, verbatim ---------------------
make_log 1 "$L: Listening for Jobs" "Runner listener exit with Session Conflict error, stop the service, no retry needed."
t_eq "conflict$TAB" "$(runner_exit_reason 1)" "session conflict"
make_log 1 "Runner listener exit with deprecated version exit code: 7."
t_eq "deprecated$TAB" "$(runner_exit_reason 1)" "deprecated version"
make_log 1 "Runner listener exit with terminated error, stop the service, no retry needed."
t_eq "terminated$TAB" "$(runner_exit_reason 1)" "terminated"
make_log 1 "Exiting with unknown error code: 42"
t_eq "unknown${TAB}42" "$(runner_exit_reason 1)" "unknown code keeps the code"
make_log 1 "Runner listener exit with retryable error, re-launch runner in 5 seconds."
t_eq "" "$(runner_exit_reason 1)" "retryable error is not an exit reason (run.sh loops)"
make_log 1 "Runner listener exit because of updating, re-launch runner after successful update"
t_eq "" "$(runner_exit_reason 1)" "update is not an exit reason"
make_log 1 "Runner listener exit with Session Conflict error, stop the service, no retry needed." "$L: Starting Runner listener" "$L: Listening for Jobs"
t_eq "" "$(runner_exit_reason 1)" "a lifecycle line after the reason outranks it"
make_log 1 "$L: Running job: build" "$L: Job build completed with result: Succeeded" "Exiting with unknown error code: 3"
t_eq "unknown${TAB}3" "$(runner_exit_reason 1)" "reason after a finished job counts"
t_eq "" "$(runner_exit_reason 99)" "no log -> no reason"

# --- runner_exit_note ---------------------------------------------------------------
case "$(runner_exit_note "conflict$TAB")" in "session conflict"*) t_eq 1 1 "note: conflict" ;; *) t_eq "session conflict..." "$(runner_exit_note "conflict$TAB")" "note: conflict" ;; esac
case "$(runner_exit_note "deprecated$TAB")" in *"--download"*) t_eq 1 1 "note: deprecated says --download" ;; *) t_eq "...--download..." "$(runner_exit_note "deprecated$TAB")" "note: deprecated says --download" ;; esac
t_eq "listener exited with code 42 - see log" "$(runner_exit_note "unknown${TAB}42")" "note: unknown names the code"

# --- supervise_runners: conflict / deprecated quarantine at once ----------------------
is_running() { return 1; }
STARTS="$TEST_TMP_DIR/starts"
: > "$STARTS"
start_runner() { echo "$1" >> "$STARTS"; }
rotate_log() { :; }
make_runner_dir 2 "https://github.com/o/r"
make_runner_dir 3 "https://github.com/o/r"
make_runner_dir 4 "https://github.com/o/r"
make_log 2 "Runner listener exit with Session Conflict error, stop the service, no retry needed."
make_log 3 "Runner listener exit with deprecated version exit code: 7."
make_log 4 "Runner listener exit with terminated error, stop the service, no retry needed."
supervise_runners
t_ok "conflict -> quarantined at once" test -f "$PID_DIR/runner-2.quarantined"
t_ok "deprecated -> quarantined at once" test -f "$PID_DIR/runner-3.quarantined"
case "$(get_lasterr 2)" in "session conflict"*"- quarantined") t_eq 1 1 "conflict lasterr says why" ;; *) t_eq "session conflict... - quarantined" "$(get_lasterr 2)" "conflict lasterr says why" ;; esac
t_fail_ok "terminated is not quarantined" test -f "$PID_DIR/runner-4.quarantined"
t_eq "4" "$(tr '\n' ' ' < "$STARTS" | sed 's/ $//')" "only the terminated runner is restarted"
t_eq "listener exited (terminated, exit 1) - see log" "$(get_lasterr 4)" "terminated lasterr set before the restart"
t_fail_ok "no failcount charged to a quarantined conflict" test -f "$PID_DIR/runner-2.failcount"
source "$SCRIPT_UNDER_TEST"   # restore the stubbed functions

# --- runner_version -----------------------------------------------------------------
make_runner_dir 5
mkdir -p "$RUNNER_BASE_DIR/runner-5/bin.2.330.0"
ln -s "$RUNNER_BASE_DIR/runner-5/bin.2.330.0" "$RUNNER_BASE_DIR/runner-5/bin"
echo "2.320.0" > "$PID_DIR/runner-5.version"
t_eq "2.330.0" "$(runner_version 5)" "bin -> bin.X symlink wins over the version file"
rm "$RUNNER_BASE_DIR/runner-5/bin"
ln -s "bin.2.331.0/" "$RUNNER_BASE_DIR/runner-5/bin"
t_eq "2.331.0" "$(runner_version 5)" "relative symlink with a trailing slash"
make_runner_dir 6
echo "2.320.0" > "$PID_DIR/runner-6.version"
t_eq "2.320.0" "$(runner_version 6)" "falls back to runner-N.version"
make_runner_dir 7
mkdir -p "$RUNNER_BASE_DIR/runner-7/bin"
printf '#!/bin/sh\necho 2.329.0\n' > "$RUNNER_BASE_DIR/runner-7/bin/Runner.Listener"
chmod +x "$RUNNER_BASE_DIR/runner-7/bin/Runner.Listener"
t_eq "2.329.0" "$(runner_version 7)" "asks Runner.Listener --version"
t_eq "2.329.0" "$(cat "$PID_DIR/runner-7.version")" "and caches it"
printf '#!/bin/sh\necho garbage\n' > "$RUNNER_BASE_DIR/runner-7/bin/Runner.Listener"
rm -f "$PID_DIR/runner-7.version"
t_eq "" "$(runner_version 7)" "non-version output is ignored"
t_eq "" "$(runner_version 8)" "unknown runner -> empty"

# --- runner_main_pid ------------------------------------------------------------------
# A parent sh with a sleep child: the root of the tree is the parent
sh -c 'sleep 30 & wait; :' &
PARENT=$!
CHILD=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
    CHILD=$(pgrep -P "$PARENT" 2>/dev/null | head -1) || CHILD=""
    [[ -n "$CHILD" ]] && break
    sleep 0.2
done
runner_procs() { printf '%s\n%s\n' "$CHILD" "$PARENT"; }
t_eq "$PARENT" "$(runner_main_pid 1)" "tree root, not the first pgrep match"

# Two roots: the one running run.sh wins
printf '#!/bin/sh\nsleep 30\n' > "$TEST_TMP_DIR/run.sh"
chmod +x "$TEST_TMP_DIR/run.sh"
"$TEST_TMP_DIR/run.sh" &
RUNSH=$!
sleep 0.3
runner_procs() { printf '%s\n%s\n' "$PARENT" "$RUNSH"; }
t_eq "$RUNSH" "$(runner_main_pid 1)" "prefers the run.sh root"
runner_procs() { :; }
t_fail_ok "no processes -> fails" runner_main_pid 1
# Parents first, so no shell is left to report its sleep being killed
KIDS="$CHILD $(pgrep -P "$RUNSH" 2>/dev/null | tr '\n' ' ')"
kill "$PARENT" "$RUNSH" 2>/dev/null || true
# shellcheck disable=SC2086
kill $KIDS 2>/dev/null || true
wait 2>/dev/null || true
