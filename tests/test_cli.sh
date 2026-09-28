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

json=$(state_json cli)
if command -v python3 >/dev/null 2>&1; then
    parsed=$(printf '%s' "$json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
r = {x["id"]: x for x in d["runners"]}
print(d["schema"], d["writer"], len(d["targets"]), d["targets"][0]["label"], d["targets"][1]["listed"],
      r[2]["lasterr"].replace("\n", "|"), r[3]["pid"], r[4]["target"], d["daemon_pid"], r[2]["quarantined"])
' 2>&1)
    t_eq 'x2 cli 2 a/b False crash-looped "5x"|quarantined 4242 None None True' "x$parsed" "state_json is valid JSON with expected fields"
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
t_ok "supervisor_tick writes state.json" test -s "$PID_DIR/state.json"
t_ok "a TUI tick is written as writer tui" grep -q '"writer":"tui"' "$PID_DIR/state.json"
DAEMON_MODE=1
supervisor_tick
t_ok "a daemon tick is written as writer daemon" grep -q '"writer":"daemon","daemon_pid":'"$$" "$PID_DIR/state.json"
DAEMON_MODE=0
t_eq "" "$(ls -A "$PID_DIR" | grep 'state.json.tmp' || true)" "no temp file left behind"
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

# --- parse_bounds_arg ----------------------------------------------------------
parse_bounds_arg "o/r=1-5"
t_eq "https://github.com/o/r 1 5" "$BOUNDS_URL $BOUNDS_MIN $BOUNDS_MAX" "parse_bounds_arg T=MIN-MAX"
parse_bounds_arg "myorg=none"
t_eq "https://github.com/myorg||" "$BOUNDS_URL|$BOUNDS_MIN|$BOUNDS_MAX" "parse_bounds_arg T=none clears"
parse_bounds_arg "o/r=02-03"
t_eq "2 3" "$BOUNDS_MIN $BOUNDS_MAX" "parse_bounds_arg strips leading zeros"
t_fail_ok "parse_bounds_arg rejects MIN > MAX" parse_bounds_arg "o/r=5-1"
t_fail_ok "parse_bounds_arg rejects a single number" parse_bounds_arg "o/r=1"
t_fail_ok "parse_bounds_arg rejects a non-number" parse_bounds_arg "o/r=1-x"
t_fail_ok "parse_bounds_arg rejects a bad target" parse_bounds_arg "a b=1-2"

# --- new verbs: flag parsing ---------------------------------------------------
t_eq "2" "$(run_copy --set-bounds nope >/dev/null 2>&1; echo $?)" "--set-bounds nope -> 2"
t_eq "2" "$(run_copy --add-target >/dev/null 2>&1; echo $?)" "--add-target without a value -> 2"
t_eq "2" "$(run_copy --set-config NOPE=1 >/dev/null 2>&1; echo $?)" "--set-config unknown key -> 2"
t_eq "2" "$(run_copy --set-config MAX_RUNNERS >/dev/null 2>&1; echo $?)" "--set-config without = -> 2"
t_eq "2" "$(run_copy --retry >/dev/null 2>&1; echo $?)" "--retry without an id -> 2"
t_eq "2" "$(run_copy --poll --stop-all >/dev/null 2>&1; echo $?)" "two new verbs -> 2"
t_eq "2" "$(run_copy --relogin </dev/null >/dev/null 2>&1; echo $?)" "--relogin without a terminal -> 2"
t_eq "2" "$(run_copy --stop-all --json >/dev/null 2>&1; echo $?)" "--json with --stop-all -> 2"
help=$(run_copy --help)
for verb in --add-target --remove-target --set-bounds --set-config --retry --auth-check --relogin --poll --gh-status --stop-all --start-all; do
    t_ok "--help mentions $verb" grep -q -- "$verb" <<< "$help"
done

# --- new verbs end to end, with a gh shim on PATH --------------------------------
printf 'RUNNER_NAME_PREFIX="t"\nMAX_RUNNERS="10"\n' > "$CONFIG_FILE"
printf 'a/b\n' > "$TARGETS_FILE"
SHIMDIR=$(make_gh_shim '
case "$*" in
  "auth status"*) exit 0 ;;
  *"repos/o/r"*) echo "{}" ;;
  *"repos/o/gone"*) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  *"-i user"*) printf "HTTP/2.0 200 OK\r\nX-OAuth-Scopes: repo, admin:org\r\n\r\n{\"login\":\"shimuser\"}\n" ;;
  *) echo "{}" ;;
esac')
run_shim() { PATH="$SHIMDIR:$PATH" run_copy "$@"; }

out=$(run_shim --add-target o/r 2>&1); rc=$?
t_eq "0" "$rc" "--add-target o/r -> 0"
t_ok "--add-target prints what it added" grep -q 'added o/r' <<< "$out"
t_ok "--add-target writes the targets file" grep -qx 'o/r' "$TARGETS_FILE"
t_ok "a mutating verb refreshes state.json when no daemon runs" grep -q '"writer":"cli"' "$PID_DIR/state.json"
out=$(run_shim --add-target o/gone 2>&1); rc=$?
t_eq "1" "$rc" "--add-target of a 404 -> 1"
t_ok "the 404 is explained" grep -q '404' <<< "$out"
t_fail_ok "a failed add doesn't touch the targets file" grep -q 'o/gone' "$TARGETS_FILE"
t_eq "2" "$(run_shim --add-target 'a b' >/dev/null 2>&1; echo $?)" "--add-target of an invalid target -> 2"

t_eq "0" "$(run_shim --set-bounds o/r=1-3 >/dev/null 2>&1; echo $?)" "--set-bounds o/r=1-3 -> 0"
t_ok "bounds written to the targets line" grep -qx 'o/r min=1 max=3' "$TARGETS_FILE"
t_eq "2" "$(run_shim --set-bounds o/r=1-11 >/dev/null 2>&1; echo $?)" "--set-bounds over MAX_RUNNERS -> 2"
t_eq "0" "$(run_shim --set-bounds o/r=none >/dev/null 2>&1; echo $?)" "--set-bounds o/r=none -> 0"
t_ok "bounds cleared" grep -qx 'o/r' "$TARGETS_FILE"

echo 4 > "$PID_DIR/want-o+r.txt"
t_eq "0" "$(run_shim --remove-target o/r >/dev/null 2>&1; echo $?)" "--remove-target o/r -> 0"
t_fail_ok "target gone from the file" grep -q 'o/r' "$TARGETS_FILE"
t_fail_ok "its want file removed" test -e "$PID_DIR/want-o+r.txt"
out=$(run_shim --remove-target a/b 2>&1); rc=$?
t_eq "1" "$rc" "--remove-target with runners registered to it -> 1"
t_ok "the refusal says how to scale down" grep -q -- '--scale a/b=0' <<< "$out"

out=$(run_shim --set-config MAX_RUNNERS=7 --set-config AUTOSCALE=1 2>&1); rc=$?
t_eq "0" "$rc" "--set-config (repeated) -> 0"
t_ok "config file has the new value" grep -qx 'MAX_RUNNERS="7"' "$CONFIG_FILE"
t_ok "config file has the second value" grep -qx 'AUTOSCALE="1"' "$CONFIG_FILE"
t_ok "prints the effective value" grep -qx 'MAX_RUNNERS=7' <<< "$out"
t_ok "unrelated keys are kept" grep -qx 'RUNNER_NAME_PREFIX="t"' "$CONFIG_FILE"
t_eq "2" "$(run_shim --set-config REFRESH_INTERVAL=0 >/dev/null 2>&1; echo $?)" "--set-config REFRESH_INTERVAL=0 -> 2"
t_ok "a rejected value is not saved" grep -qx 'REFRESH_INTERVAL="5"' "$CONFIG_FILE"
out=$(run_shim --set-config REFRESH_INTERVAL=99999 2>&1)
t_ok "an out-of-range value is clamped and saved" grep -qx 'REFRESH_INTERVAL="3600"' "$CONFIG_FILE"
t_ok "the clamp is reported" grep -q 'clamped to 3600' <<< "$out"
run_shim --set-config REFRESH_INTERVAL=5 >/dev/null 2>&1

out=$(run_shim --auth-check 2>&1); rc=$?
t_eq "0" "$rc" "--auth-check with a good token -> 0"
t_ok "--auth-check prints the user" grep -q 'shimuser' <<< "$out"
json=$(run_shim --auth-check --json 2>/dev/null)
if command -v python3 >/dev/null 2>&1; then
    t_eq "shimuser ['repo', 'admin:org'] ok" "$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(d["gh"]["user"], d["gh"]["scopes"], d["gh"]["state"])' "$json" 2>&1)" "--auth-check --json"
fi
BADDIR="$TEST_TMP_DIR/badbin"; mkdir -p "$BADDIR"
printf '#!/bin/sh\necho "gh: Bad credentials (HTTP 401)" >&2\nexit 1\n' > "$BADDIR/gh"; chmod +x "$BADDIR/gh"
t_eq "1" "$(PATH="$BADDIR:$PATH" run_copy --auth-check >/dev/null 2>&1; echo $?)" "--auth-check with a rejected token -> 1"
t_eq "auth" "$(cat "$PID_DIR/gh.state")" "and records gh.state=auth"

out=$(run_shim --gh-status 2>&1); rc=$?
t_eq "0" "$rc" "--gh-status -> 0"
t_ok "--gh-status lists each target" grep -q 'a/b' <<< "$out"
t_eq "0" "$(run_shim --poll >/dev/null 2>&1; echo $?)" "--poll without a daemon polls now -> 0"
t_eq "2" "$(run_shim --retry 99 >/dev/null 2>&1; echo $?)" "--retry of an unknown runner -> 2 (same as --start)"
rm -f "$PID_DIR"/runner-*.stopped
t_eq "0" "$(run_shim --stop-all >/dev/null 2>&1; echo $?)" "--stop-all -> 0"
t_ok "--stop-all marks every runner stopped" test -f "$PID_DIR/runner-1.stopped" -a -f "$PID_DIR/runner-3.stopped"
t_fail_ok "no lock left behind by the new verbs" test -e "$LOCK_FILE"
