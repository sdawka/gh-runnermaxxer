#!/usr/bin/env bash
# state.json (schema 2): write_state_snapshot / state_json / status_json_cached
source "$(dirname "$0")/_helper.sh"
mkdir -p "$PID_DIR" "$LOG_DIR"

REPO="https://github.com/o/r"
ORG="https://github.com/myorg"
printf 'o/r min=1 max=5\nmyorg\n' > "$TARGETS_FILE"

# Runner 1: repo, "running" a job; runner 2: org, quarantined; runner 3: unassigned
make_runner_dir 1 "$REPO"
make_runner_dir 2 "$ORG"
make_runner_dir 3 ""
echo "4121" > "$PID_DIR/runner-1.pid"
echo "2.334.0" > "$PID_DIR/runner-1.version"
make_log 1 "$(date -u -v-2M '+%Y-%m-%d %H:%M:%SZ' 2>/dev/null || date -u -d '2 minutes ago' '+%Y-%m-%d %H:%M:%SZ'): Running job: build"
is_running() { [[ "$1" == "1" ]]; }
touch "$PID_DIR/runner-2.quarantined"
set_lasterr 2 "session conflict - quarantined"
echo 3 > "$PID_DIR/runner-2.failcount"
echo 5 > "$PID_DIR/want-o+r.txt"
target_set_error "$ORG" scope "token lacks the admin:org scope"
echo sahil > "$PID_DIR/gh.user"
echo "repo,admin:org" > "$PID_DIR/gh.scopes"
echo ok > "$PID_DIR/gh.state"
echo 1790612000 > "$PID_DIR/gh.checked"
: > "$RUNNER_BASE_DIR/actions-runner-osx-arm64-2.334.0.tar.gz"
RUNNER_TAR="$RUNNER_BASE_DIR/actions-runner-osx-arm64-2.334.0.tar.gz"
printf '%s\nv2.337.0\n' "$(date +%s)" > "$RUNNER_BASE_DIR/.latest-runner-tag"
echo "runner-9" > "$ORPHANS_FILE"
gh() { echo "unexpected gh call: $*" >> "$TEST_TMP_DIR/gh.calls"; return 1; }

write_state_snapshot daemon
t_ok "state.json written" test -s "$PID_DIR/state.json"
t_eq "" "$(ls -A "$PID_DIR" | grep 'state.json.tmp' || true)" "no temp file left"
t_fail_ok "the snapshot never calls gh" test -s "$TEST_TMP_DIR/gh.calls"

if command -v python3 >/dev/null 2>&1; then
    check() {  # check EXPR EXPECTED DESC
        local got
        got=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
t = {x["url"]: x for x in d["targets"]}
r = {x["id"]: x for x in d["runners"]}
print(eval(sys.argv[2]))
' "$PID_DIR/state.json" "$1" 2>&1)
        t_eq "$2" "$got" "$3"
    }
    check 'd["schema"]' 2 "schema 2"
    check 'd["writer"]' daemon "writer daemon"
    check 'd["version"]' "$VERSION" "version"
    check 'd["tick_ts"] > 0' True "tick_ts set"
    check 'd["refresh_interval"]' "$REFRESH_INTERVAL" "refresh_interval"
    check 'd["gh"]["user"]' sahil "gh.user"
    check 'd["gh"]["scopes"]' "['repo', 'admin:org']" "gh.scopes is an array"
    check 'd["gh"]["state"]' ok "gh.state"
    check 'd["gh"]["checked"]' 1790612000 "gh.checked"
    check 'd["gh"]["rate_remaining"]' None "rate_remaining null until PR4"
    check 'd["tarball"]' "{'version': '2.334.0', 'latest': '2.337.0', 'stale': True}" "tarball stale"
    check 'isinstance(d["disk"]["free_mb"], int)' True "disk.free_mb"
    check 'sorted(d["config"].keys()) == sorted("'"$CONFIG_KEYS"'".split())' True "config has all keys"
    check 'd["config"]["MAX_RUNNERS"]' "$MAX_RUNNERS" "config values are numbers"
    check 'any("orphaned" in w for w in d["warnings"])' True "orphan warning"
    check 't["'"$REPO"'"]["want"], t["'"$REPO"'"]["have"]' "(5, 1)" "want from want-KEY.txt, have from dirs"
    check 't["'"$REPO"'"]["min"], t["'"$REPO"'"]["max"], t["'"$REPO"'"]["type"]' "(1, 5, 'repo')" "bounds and type"
    check 't["'"$REPO"'"]["error"]' None "no error -> null"
    check 't["'"$ORG"'"]["want"], t["'"$ORG"'"]["min"]' "(1, None)" "want falls back to have; no bounds -> null"
    check 't["'"$ORG"'"]["error"]["class"]' scope "target error class"
    check '"admin:org" in t["'"$ORG"'"]["error"]["message"]' True "target error message"
    check 'd["targets"][0]["autoscale"]' False "autoscale false while AUTOSCALE=0"
    check 'r[1]["name"], r[1]["pid"], r[1]["job"], r[1]["version"]' "('runner-1', 4121, 'build', '2.334.0')" "runner name (from .runner), pid, job, version"
    check '100 <= r[1]["elapsed"] <= 200' True "elapsed from the job start"
    check 'r[1]["log_path"].endswith("/runner-1.log")' True "log_path"
    check 'r[2]["quarantined"], r[2]["fails"], r[2]["lasterr"]' "(True, 3, 'session conflict - quarantined')" "quarantined runner"
    check 'r[3]["target"], r[3]["pid"], r[3]["lasterr"], r[3]["job"]' "(None, None, None, None)" "unassigned runner nulls"
    check 'r[3]["state"], r[3]["next_retry"] is None' "('restarting', True)" "restarting runner without a laststart: next_retry null"
else
    echo "  (python3 not found - skipping JSON checks)"
fi

# next_retry = laststart + 5 * 2^fails
echo 1000 > "$PID_DIR/runner-3.laststart"
echo 2 > "$PID_DIR/runner-3.failcount"
status_collect
t_eq "1020" "${R_NEXT[2]}" "next_retry = laststart + 5*2^fails"
t_eq "" "${R_NEXT[0]}" "no next_retry for a running runner"

# --- status_json_cached ----------------------------------------------------------------
REFRESH_INTERVAL=5
t_ok "fresh snapshot is served" eval "status_json_cached >/dev/null"
t_eq "$(cat "$PID_DIR/state.json")" "$(status_json_cached)" "served verbatim"
old=$(( $(date +%s) - 11 ))
sed "s/\"tick_ts\":[0-9]*/\"tick_ts\":$old/" "$PID_DIR/state.json" > "$PID_DIR/state.json.x"
mv "$PID_DIR/state.json.x" "$PID_DIR/state.json"
t_fail_ok "a snapshot older than 2 ticks is not served" status_json_cached
rm -f "$PID_DIR/state.json"
t_fail_ok "no snapshot -> not served" status_json_cached

# A CLI writer: daemon_pid comes from daemon_pid (none here)
write_state_snapshot cli
t_ok "writer cli, no daemon" grep -q '"writer":"cli","daemon_pid":null' "$PID_DIR/state.json"

# --- disk: filesystem free (df shim) and per-runner usage ----------------------------
DFDIR="$TEST_TMP_DIR/dfbin"; mkdir -p "$DFDIR"
printf '#!/bin/sh\necho "Filesystem 1024-blocks Used Available Capacity Mounted on"\necho "/dev/x 1000000 900000 100000 90%% /"\n' > "$DFDIR/df"
chmod +x "$DFDIR/df"
t_eq "10" "$(PATH="$DFDIR:$PATH" disk_free_pct)" "disk_free_pct from df -Pk columns 2 and 4"
t_eq "97" "$(PATH="$DFDIR:$PATH" free_disk_mb)" "free_disk_mb from the same table"
DLOGF="$TEST_TMP_DIR/dlog"
dlog() { echo "$1" >> "$DLOGF"; }
(
    PATH="$DFDIR:$PATH"
    state_warnings
    case "${STATE_WARNINGS[*]}" in *"low disk: 10% free"*) t_eq 1 1 "low disk shows in warnings[]" ;; *) t_eq "...low disk: 10% free..." "${STATE_WARNINGS[*]}" "low disk shows in warnings[]" ;; esac
    rm -rf "$RUNNER_BASE_DIR"/runner-*
    disk_usage_tick; disk_usage_tick
    t_eq "1" "$(grep -c 'low disk' "$DLOGF")" "the daemon logs low disk once, not every tick"
)

rm -rf "$RUNNER_BASE_DIR"/runner-*
rm -f "$PID_DIR"/runner-*
make_runner_dir 1 "$REPO"
mkdir -p "$RUNNER_BASE_DIR/runner-1/_work" "$RUNNER_BASE_DIR/runner-1/_diag"
dd if=/dev/zero of="$RUNNER_BASE_DIR/runner-1/_work/blob" bs=1024 count=2048 2>/dev/null
DISK_TICK=0
disk_usage_tick
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    [[ -f "$PID_DIR/runner-1.workmb" ]] && break
    sleep 0.2
done
t_ok "disk_usage_tick writes runner-N.workmb in the background" test -f "$PID_DIR/runner-1.workmb"
t_ok "workmb counts _work (2 MB)" test "$(cat "$PID_DIR/runner-1.workmb")" -ge 1
for _ in 1 2 3 4 5 6 7 8 9 10; do [[ -f "$PID_DIR/runner-1.workmb.pending" ]] || break; sleep 0.2; done
t_fail_ok "the pending marker is cleared" test -f "$PID_DIR/runner-1.workmb.pending"
echo 999 > "$PID_DIR/runner-1.workmb"
disk_usage_tick
sleep 0.5
t_eq "999" "$(cat "$PID_DIR/runner-1.workmb")" "within the throttle window the figure is not recomputed"
DISK_TICK=$((DISK_USAGE_TICKS - 1))
disk_usage_tick
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    [[ "$(cat "$PID_DIR/runner-1.workmb")" != "999" ]] && break
    sleep 0.2
done
t_eq "2" "$(cat "$PID_DIR/runner-1.workmb")" "every DISK_USAGE_TICKS ticks it is recomputed"
status_collect
t_eq "2" "${R_WORKMB[0]}" "status_collect reads work_mb"
wait 2>/dev/null || true
