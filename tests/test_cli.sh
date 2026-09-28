#!/usr/bin/env bash
# Daemon / service / scripting-CLI helpers
source "$(dirname "$0")/_helper.sh"

no_color() { RED=''; GREEN=''; YELLOW=''; BLUE=''; CYAN=''; BOLD=''; DIM=''; NC=''; }
no_color
# Re-source the script to undo function stubs, restoring the helper's paths
# and EXIT trap (sourcing re-installs the script's own trap)
reload_lib() {
    source "$SCRIPT_UNDER_TEST"
    no_color
    CONFIG_FILE="$TEST_TMP_DIR/.runnermaxxer.conf"
    TARGETS_FILE="$TEST_TMP_DIR/.runnermaxxer.targets"
    trap _helper_cleanup EXIT
}
mkdir -p "$PID_DIR" "$LOG_DIR"

# --- parse_scale_arg --------------------------------------------------------
parse_scale_arg "octo/repo=3"
t_eq "https://github.com/octo/repo 3" "$SCALE_URL $SCALE_N" "parse_scale_arg owner/repo=N"
parse_scale_arg "octo-org=0"
t_eq "https://github.com/octo-org 0" "$SCALE_URL $SCALE_N" "parse_scale_arg org=0"
parse_scale_arg "https://github.com/a/b.git=07"
t_eq "https://github.com/a/b 7" "$SCALE_URL $SCALE_N" "parse_scale_arg URL=N (normalised, leading zero)"
t_fail_ok "parse_scale_arg rejects a missing count" parse_scale_arg "octo/repo"
t_fail_ok "parse_scale_arg rejects a non-numeric count" parse_scale_arg "octo/repo=x"
t_fail_ok "parse_scale_arg rejects an empty target" parse_scale_arg "=3"
t_fail_ok "parse_scale_arg rejects spaces in the target" parse_scale_arg "a b=2"

# --- scale_total_after ------------------------------------------------------
make_runner_dir 1 "https://github.com/a/b"
make_runner_dir 2 "https://github.com/a/b"
make_runner_dir 3 "https://github.com/c/d"
t_eq "6" "$(scale_total_after https://github.com/a/b 5)" "scale_total_after replaces one target, keeps others"
t_eq "5" "$(scale_total_after https://github.com/e/f 2)" "scale_total_after adds a new target to existing ones"
t_eq "2" "$(scale_total_after https://github.com/A/B 9 https://github.com/a/b 1)" "scale_total_after: later pair wins, case-insensitive"
touch "$PID_DIR/runner-2.draining"
t_eq "3" "$(scale_total_after https://github.com/e/f 1)" "scale_total_after ignores draining runners"
rm -f "$PID_DIR/runner-2.draining"

# --- json_str / json_num ----------------------------------------------------
t_eq '"plain"' "$(json_str plain)" "json_str plain"
t_eq '"a \"q\" \\ b"' "$(json_str 'a "q" \ b')" "json_str escapes quotes and backslashes"
t_eq '"l1\nl2\tx"' "$(json_str $'l1\nl2\tx')" "json_str escapes newline and tab"
t_eq '"ab"' "$(json_str $'a\033b')" "json_str drops other control characters"
t_eq '"running: build (3m)"' "$(json_str 'running: build (3m)')" "json_str keeps ordinary punctuation"
t_eq "42" "$(json_num 42)" "json_num number"
t_eq "null" "$(json_num '')" "json_num empty -> null"

# --- runner_state -----------------------------------------------------------
make_runner_dir 4 ""                   # unassigned (no .runner)
touch "$PID_DIR/runner-1.stopped"
touch "$PID_DIR/runner-2.quarantined"
set_lasterr 2 $'crash-looped "5x"\nquarantined'
t_eq "stopped" "$(runner_state 1)" "runner_state stopped"
t_eq "quarantined" "$(runner_state 2)" "runner_state quarantined"
t_eq "restarting" "$(runner_state 3)" "runner_state restarting (not running, no marker)"
touch "$PID_DIR/runner-3.draining"
t_eq "draining" "$(runner_state 3)" "runner_state draining"
rm -f "$PID_DIR/runner-3.draining"

# Fake "running" for runner 3 to exercise idle/busy/running
is_running() { [[ "$1" == "3" ]]; }
echo 4242 > "$PID_DIR/runner-3.pid"
make_log 3 "2024-01-01 00:00:00Z: Listening for Jobs"
t_eq "idle" "$(runner_state 3)" "runner_state idle"
make_log 3 "2024-01-01 00:00:00Z: Running job: build"
t_eq "busy" "$(runner_state 3)" "runner_state busy"
make_log 3 "2024-01-01 00:00:00Z: Starting Runner listener"
t_eq "running" "$(runner_state 3)" "runner_state running (neither idle nor busy)"

# --- status_collect / status_json ------------------------------------------
echo "a/b" > "$TARGETS_FILE"
status_collect
t_eq "https://github.com/a/b https://github.com/c/d" "${P_URLS[*]}" "status_collect projects: listed then runner-only"
t_eq "true false" "${P_LISTED[*]}" "status_collect listed flags"
t_eq "2 1" "${P_CONF[*]}" "status_collect configured counts"
t_eq "0 1" "${P_RUN[*]}" "status_collect running counts"
t_eq "1 2 3 4" "${R_IDS[*]}" "status_collect runner ids"
t_eq "stopped quarantined running restarting" "${R_STATE[*]}" "status_collect runner states"
t_eq "" "${R_PROJ[3]}" "status_collect unassigned runner has no project"

json=$(status_json)
if command -v python3 >/dev/null 2>&1; then
    parsed=$(printf '%s' "$json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
r = {x["id"]: x for x in d["runners"]}
print(len(d["projects"]), d["projects"][0]["label"], d["projects"][1]["listed"],
      r[2]["last_error"].replace("\n", "|"), r[3]["pid"], r[4]["project"], d["daemon_pid"])
' 2>&1)
    t_eq 'x2 a/b False crash-looped "5x"|quarantined 4242 None None' "x$parsed" "status_json is valid JSON with expected fields"
else
    echo "  (python3 not found - skipping JSON parse check)"
fi
t_ok "status_text runs" eval "status_text >/dev/null"
t_eq "1" "$(status_text | grep -c 'Unconfigured')" "status_text lists unassigned runners once"
reload_lib     # restore the real is_running

# --- cli_runner_cmd argument checks ----------------------------------------
rc=0; cli_runner_cmd stop 99 2>/dev/null || rc=$?
t_eq "2" "$rc" "cli_runner_cmd: unknown runner -> exit 2"
rc=0; cli_runner_cmd stop "runner-x" 2>/dev/null || rc=$?
t_eq "2" "$rc" "cli_runner_cmd: non-numeric id -> exit 2"
rc=0; ( set_cli_cmd status; set_cli_cmd scale ) 2>/dev/null || rc=$?
t_eq "2" "$rc" "set_cli_cmd refuses two different commands"
rc=0; ( set_cli_cmd scale; set_cli_cmd scale ) 2>/dev/null || rc=$?
t_eq "0" "$rc" "set_cli_cmd allows repeating --scale"

# --- supervisor_tick --------------------------------------------------------
SUP=0; GHC=0
supervise_runners() { SUP=$((SUP + 1)); }
check_github_health() { GHC=$((GHC + 1)); }
GH_HEALTH_TICKS=3; SUPERVISOR_TICK=$((GH_HEALTH_TICKS - 1))
supervisor_tick
t_eq "1 1" "$SUP $GHC" "supervisor_tick: first tick polls GitHub"
supervisor_tick; supervisor_tick
t_eq "3 1" "$SUP $GHC" "supervisor_tick: no poll on the next two ticks"
supervisor_tick
t_eq "4 2" "$SUP $GHC" "supervisor_tick: polls every GH_HEALTH_TICKS"
GH_HEALTH_TICKS=0
supervisor_tick
t_eq "5 2" "$SUP $GHC" "supervisor_tick: GH_HEALTH_TICKS=0 never polls"
reload_lib

# --- dlog -------------------------------------------------------------------
rm -f "$DAEMON_LOG"
DAEMON_MODE=0; dlog "nothing"
t_fail_ok "dlog is a no-op outside daemon mode" test -e "$DAEMON_LOG"
DAEMON_MODE=1; dlog "hello"; set_lasterr 7 "boom"
DAEMON_MODE=0
t_eq "2" "$(grep -c . "$DAEMON_LOG")" "dlog writes one line per event (incl. set_lasterr)"
t_ok "dlog lines are timestamped" grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:]{8} runner-7: boom$' "$DAEMON_LOG"

# --- daemon_pid -------------------------------------------------------------
t_fail_ok "daemon_pid: no pid file" daemon_pid
echo 999999 > "$DAEMON_PID_FILE"
t_fail_ok "daemon_pid: dead pid" daemon_pid
echo $$ > "$DAEMON_PID_FILE"
t_fail_ok "daemon_pid: live pid that isn't a daemon (PID reuse)" daemon_pid
sh -c 'sleep 30; :' fake --daemon &
fake=$!
sleep 0.2
echo "$fake" > "$DAEMON_PID_FILE"
t_eq "$fake" "$(daemon_pid)" "daemon_pid: live --daemon process"
kill "$fake" 2>/dev/null || true; wait "$fake" 2>/dev/null || true
rm -f "$DAEMON_PID_FILE"

# --- run_daemon: SIGTERM exits 0, releases locks/pid file, logs --------------
# Own process (so its EXIT trap is the script's cleanup); supervisor_tick is
# stubbed, so nothing but files under the temp RUNNER_BASE_DIR is touched.
rm -f "$DAEMON_LOG"
bash -c 'source "$1"; supervisor_tick() { echo tick >> "$RUNNER_BASE_DIR/ticks"; }
         DAEMON_MODE=1; REFRESH_INTERVAL=1; acquire_lock; run_daemon' _ "$SCRIPT_UNDER_TEST" >/dev/null 2>&1 &
dpid=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do [[ -s "$RUNNER_BASE_DIR/ticks" ]] && break; sleep 0.3; done
t_eq "$dpid" "$(cat "$DAEMON_PID_FILE" 2>/dev/null)" "run_daemon writes .daemon.pid"
kill -TERM "$dpid"
rc=0; wait "$dpid" || rc=$?
t_eq "0" "$rc" "run_daemon exits 0 on SIGTERM"
t_fail_ok "run_daemon removes .daemon.pid on exit" test -e "$DAEMON_PID_FILE"
t_fail_ok "run_daemon releases the main lock on exit" test -e "$LOCK_FILE"
t_fail_ok "run_daemon releases the op lock on exit" test -e "$OP_LOCK_FILE"
t_ok "run_daemon logs start and stop" grep -q 'daemon stopping' "$DAEMON_LOG"
rm -f "$RUNNER_BASE_DIR/ticks"

# --- acquire_op_lock --------------------------------------------------------
t_ok "acquire_op_lock takes a free lock" acquire_op_lock
t_eq "$$" "$(cat "$OP_LOCK_FILE")" "op lock holds our pid"
t_fail_ok "acquire_op_lock fails while held by a live process" acquire_op_lock
release_op_lock
t_fail_ok "release_op_lock removes the file" test -e "$OP_LOCK_FILE"
echo 999999 > "$OP_LOCK_FILE"
t_ok "acquire_op_lock replaces a stale lock" acquire_op_lock
release_op_lock

# --- service files ----------------------------------------------------------
t_eq "/x/bin:/usr/bin:/opt/homebrew/bin:/usr/local/bin:/bin:/usr/sbin:/sbin" \
    "$(service_path_value /x/bin /usr/bin)" "service_path_value: given dirs first, no duplicates"

plist=$(launchd_plist "/Users/me/R&D <x>/runnermaxxer.sh" "/Users/me/R&D <x>" "/opt/homebrew/bin:/usr/bin" "/tmp/daemon.out")
t_ok "plist escapes & and <" grep -qF '<string>/Users/me/R&amp;D &lt;x&gt;/runnermaxxer.sh</string>' <<< "$plist"
t_ok "plist runs --daemon" grep -qF '<string>--daemon</string>' <<< "$plist"
t_ok "plist has RunAtLoad" grep -qA1 'RunAtLoad' <<< "$plist"
t_eq "3" "$(grep -A1 -E 'RunAtLoad|KeepAlive|AbandonProcessGroup' <<< "$plist" | grep -c '<true/>')" "plist RunAtLoad/KeepAlive/AbandonProcessGroup are true"
t_ok "plist sets PATH" grep -qF '<string>/opt/homebrew/bin:/usr/bin</string>' <<< "$plist"
t_eq "2" "$(grep -c '<string>/tmp/daemon.out</string>' <<< "$plist")" "plist sends stdout and stderr to daemon.out"
if command -v plutil >/dev/null 2>&1; then
    printf '%s\n' "$plist" > "$TEST_TMP_DIR/t.plist"
    t_ok "plutil -lint accepts the plist" plutil -lint -s "$TEST_TMP_DIR/t.plist"
fi

unit=$(systemd_unit '/home/me/100% "odd" $dir/runnermaxxer.sh' "/home/me/100%" "/usr/local/bin:/usr/bin")
t_ok "unit ExecStart quotes and escapes the script path" \
    grep -qxF 'ExecStart="/home/me/100%% \"odd\" $$dir/runnermaxxer.sh" --daemon' <<< "$unit"
t_ok "unit WorkingDirectory doubles %" grep -qxF 'WorkingDirectory=/home/me/100%%' <<< "$unit"
t_ok "unit sets PATH" grep -qxF 'Environment="PATH=/usr/local/bin:/usr/bin"' <<< "$unit"
t_ok "unit restarts always" grep -qx 'Restart=always' <<< "$unit"
t_ok "unit leaves runners alive on stop" grep -qx 'KillMode=process' <<< "$unit"
t_ok "unit is wanted by default.target" grep -qx 'WantedBy=default.target' <<< "$unit"

t_eq "/h/Library/LaunchAgents/com.gh-runnermaxxer.plist" "$(HOME=/h OS_FAMILY=macos service_file_path)" "service_file_path macOS"
t_eq "/h/.config/systemd/user/gh-runnermaxxer.service" "$(HOME=/h OS_FAMILY=linux XDG_CONFIG_HOME='' service_file_path)" "service_file_path Linux"
t_eq "/x/systemd/user/gh-runnermaxxer.service" "$(HOME=/h OS_FAMILY=linux XDG_CONFIG_HOME=/x service_file_path)" "service_file_path Linux honours XDG_CONFIG_HOME"

# --- flag parsing (usage errors exit 2 before any config/runner access) -----
t_eq "2" "$(RUNNERMAXXER_LIB=0 bash "$SCRIPT_UNDER_TEST" --json >/dev/null 2>&1; echo $?)" "--json without --status -> 2"
t_eq "2" "$(RUNNERMAXXER_LIB=0 bash "$SCRIPT_UNDER_TEST" --status --stop 1 >/dev/null 2>&1; echo $?)" "two commands -> 2"
t_eq "2" "$(RUNNERMAXXER_LIB=0 bash "$SCRIPT_UNDER_TEST" --scale nope >/dev/null 2>&1; echo $?)" "bad --scale -> 2"
t_eq "2" "$(RUNNERMAXXER_LIB=0 bash "$SCRIPT_UNDER_TEST" --daemon --status >/dev/null 2>&1; echo $?)" "--daemon with a command -> 2"
t_ok "--help documents the new flags" grep -q -- '--install-service' <<< "$(RUNNERMAXXER_LIB=0 bash "$SCRIPT_UNDER_TEST" --help)"

# --- end to end on a copy of the script in the temp dir, so its config and
# targets files are the temp ones (and no config file exists) ---------------
cp "$SCRIPT_UNDER_TEST" "$TEST_TMP_DIR/rm.sh"
run_copy() { RUNNERMAXXER_LIB=0 bash "$TEST_TMP_DIR/rm.sh" "$@"; }
json=$(run_copy --status --json 2>/dev/null || true)
if command -v python3 >/dev/null 2>&1; then
    t_ok "--status --json prints valid JSON" python3 -c 'import json,sys; json.loads(sys.argv[1])' "$json"
fi
text=$(run_copy --status 2>&1 || true)
t_ok "--status lists runners" grep -q 'runner-1 ' <<< "$text"
t_eq "0" "$(printf '%s' "$text" | grep -c $'\033' || true)" "--status piped has no colour codes"
t_eq "1" "$(run_copy --daemon >/dev/null 2>&1; echo $?)" "--daemon without a config exits 1"
t_eq "1" "$(run_copy --stop 1 >/dev/null 2>&1; echo $?)" "--stop without a config exits 1"
t_eq "0" "$(run_copy --stop-daemon >/dev/null 2>&1; echo $?)" "--stop-daemon with no daemon exits 0"
t_fail_ok "none of these leave a lock behind" test -e "$LOCK_FILE"
