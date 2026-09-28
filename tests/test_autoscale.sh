#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

MAX_RUNNERS=20
AUTOSCALE_IDLE_MINUTES=10

# ---------------------------------------------------------------------------
# Targets file: optional min=/max= bounds suffix
# ---------------------------------------------------------------------------
cat > "$TARGETS_FILE" << 'EOF'
# comment
owner/repo min=1 max=5
plain/repo
myorg	max=3
https://github.com/x/y   min=2
bad/keys foo=1
bad/order min=4 max=2
bad/value min=one
EOF

loaded=$(load_targets | tr '\n' ',')
t_eq "https://github.com/owner/repo,https://github.com/plain/repo,https://github.com/myorg,https://github.com/x/y," \
    "$loaded" "load_targets strips bounds and drops lines with malformed bounds"

t_eq "1 5" "$(target_bounds https://github.com/owner/repo)" "target_bounds reads min/max"
t_eq "1 5" "$(target_bounds https://github.com/OWNER/Repo)" "target_bounds is case-insensitive"
t_eq "" "$(target_bounds https://github.com/plain/repo)" "target_bounds is empty for a line without bounds"
t_eq "0 3" "$(target_bounds https://github.com/myorg)" "missing min defaults to 0 (tab separator ok)"
t_eq "2 20" "$(target_bounds https://github.com/x/y)" "missing max defaults to MAX_RUNNERS"
t_eq "" "$(target_bounds https://github.com/bad/order)" "min > max gives no bounds"
t_fail_ok "split_target_line rejects unknown keys" split_target_line "bad/keys foo=1"
t_ok "target_in_file matches an entry with bounds" target_in_file "https://github.com/owner/repo"

# add_target_to_file does not duplicate an entry that has bounds
add_target_to_file "https://github.com/owner/repo"
t_eq "1" "$(grep -c '^owner/repo' "$TARGETS_FILE")" "add_target_to_file keeps a single bounded entry"

# set_target_bounds rewrites in place, clears, and appends when missing
set_target_bounds "https://github.com/plain/repo" 2 4
t_eq "2 4" "$(target_bounds https://github.com/plain/repo)" "set_target_bounds adds bounds"
t_ok "set_target_bounds keeps the entry text" grep -qx 'plain/repo min=2 max=4' "$TARGETS_FILE"
set_target_bounds "https://github.com/owner/repo"
t_eq "" "$(target_bounds https://github.com/owner/repo)" "set_target_bounds with no values clears"
t_ok "cleared entry stays listed" grep -qx 'owner/repo' "$TARGETS_FILE"
set_target_bounds "https://github.com/new/one" 0 2
t_eq "0 2" "$(target_bounds https://github.com/new/one)" "set_target_bounds appends an unlisted target"
t_ok "comment survives set_target_bounds" grep -qx '# comment' "$TARGETS_FILE"

remove_target_from_file "https://github.com/plain/repo"
t_fail_ok "remove_target_from_file removes a bounded entry" target_in_file "https://github.com/plain/repo"
t_ok "other bounded entries survive removal" grep -qx 'new/one min=0 max=2' "$TARGETS_FILE"

t_eq "owner+repo" "$(target_key https://github.com/Owner/Repo)" "target_key is lowercased and filename-safe"
t_eq "myorg" "$(target_key https://github.com/myorg)" "target_key for an org"

# ---------------------------------------------------------------------------
# autoscale_decide CUR BUSY QUEUED MIN MAX IDLE_ELAPSED [CAP]
# ---------------------------------------------------------------------------
t_eq "3" "$(autoscale_decide 1 1 2 1 5 0)" "grow by queued when all busy"
t_eq "5" "$(autoscale_decide 3 3 9 1 5 0)" "growth capped at max"
t_eq "2" "$(autoscale_decide 2 1 4 1 5 0)" "no growth while a runner is idle"
t_eq "2" "$(autoscale_decide 2 2 0 1 5 0)" "all busy, nothing queued: unchanged"
t_eq "2" "$(autoscale_decide 3 1 0 1 5 10)" "shrink by one after the idle period"
t_eq "3" "$(autoscale_decide 3 1 0 1 5 9)" "no shrink before the idle period"
t_eq "1" "$(autoscale_decide 1 0 0 1 5 60)" "never below min"
t_eq "1" "$(autoscale_decide 0 0 0 1 5 0)" "brought up to min"
t_eq "5" "$(autoscale_decide 7 7 0 1 5 0)" "brought down to max"
t_eq "2" "$(autoscale_decide 0 0 2 0 5 0)" "min=0 project grows from nothing on a queue"
t_eq "4" "$(autoscale_decide 2 2 5 1 8 0 4)" "growth capped by global MAX_RUNNERS headroom"
t_eq "2" "$(autoscale_decide 2 2 5 1 8 0 1)" "global cap never forces a shrink"
AUTOSCALE_IDLE_MINUTES=0
t_eq "2" "$(autoscale_decide 3 0 0 1 5 0)" "AUTOSCALE_IDLE_MINUTES=0 shrinks immediately"
AUTOSCALE_IDLE_MINUTES=10

# ---------------------------------------------------------------------------
# autoscale_tick with stubbed gh / runner state / scale_target
# ---------------------------------------------------------------------------
mkdir -p "$PID_DIR"
cat > "$TARGETS_FILE" << 'EOF'
auto/repo min=1 max=5
fixed/repo
EOF
make_runner_dir 1 "https://github.com/auto/repo"
make_runner_dir 2 "https://github.com/fixed/repo"

SCALE_CALLS=""
scale_target() { SCALE_CALLS="$SCALE_CALLS $(target_key "$1")=$2"; return 0; }
# Queue stub: GH_QUEUED_JOBS lines of labels, one per queued job, spread
# over one run each (the jobs endpoint prints one line per call)
CACHED_LABELS="macos arm64 apple-silicon"
RUNNER_OS=osx; RUNNER_ARCH=arm64
queue_stub() {
    # queue_stub "labels" "labels" ... -> gh() serving that queue
    QUEUE_JOBS=("$@")
    gh() {
        local i a="$*"
        case "$*" in
            *"runs?status=queued"*) for ((i = 0; i < ${#QUEUE_JOBS[@]}; i++)); do echo $((100 + i)); done ;;
            *"/runs/"*"/jobs"*) i=${a#*runs/}; i=${i%%/*}; echo "${QUEUE_JOBS[$((i - 100))]}" ;;
            *) return 1 ;;
        esac
    }
}
# 3 queued jobs, 2 of which this host can take (the GPU one it can't)
queue_stub "self-hosted,macOS,ARM64" "self-hosted,macos,arm64" "self-hosted,linux,gpu"
is_running() { return 0; }             # every runner has a live process
is_busy() { return 0; }                # every runner busy
gh_data_fresh() { return 0; }
dlog() { DLOG_LINE="$1"; }

AUTOSCALE=0
autoscale_tick
t_eq "" "$SCALE_CALLS" "autoscale_tick does nothing with AUTOSCALE=0"

AUTOSCALE=1
DLOG_LINE=""
out=$(autoscale_tick)
t_eq "" "$out" "autoscale_tick prints nothing"
autoscale_tick
t_eq " auto+repo=3" "$SCALE_CALLS" "scales only the bounded project, by the satisfiable queue depth"
t_eq "2	1" "$(cat "$PID_DIR/queue-auto+repo.txt")" "queue cached per target as satisfiable<TAB>unsatisfiable"
t_eq "2" "$(cached_queue_depth https://github.com/auto/repo)" "cached_queue_depth reads the satisfiable count"
t_eq "1" "$(cached_queue_unsatisfiable https://github.com/auto/repo)" "cached_queue_unsatisfiable reads the rest"
t_eq "" "$(cached_queue_depth https://github.com/fixed/repo)" "no queue cached for an unbounded project"
case "$(cat "$PID_DIR/autoscale.last")" in
    *"auto/repo 1 → 3 (2 queued)") t_eq 1 1 "autoscale.last records the change" ;;
    *) t_eq "... auto/repo 1 → 3 (2 queued)" "$(cat "$PID_DIR/autoscale.last")" "autoscale.last records the change" ;;
esac
t_eq "$(cat "$PID_DIR/autoscale.last")" "$DLOG_LINE" "dlog receives the same line"

# Pending manual change in the menu: hands off
SCALE_CALLS=""
M_URLS=("https://github.com/auto/repo"); M_CUR=(1); M_WANT=(2)
autoscale_tick
t_eq "" "$SCALE_CALLS" "autoscale_tick skips a project with a pending manual change"
M_URLS=(); M_CUR=(); M_WANT=()

# Stale GitHub data: no decision for a project that has runners
SCALE_CALLS=""
gh_data_fresh() { return 1; }
autoscale_tick
t_eq "" "$SCALE_CALLS" "autoscale_tick waits for fresh API data"
gh_data_fresh() { return 0; }

# Queue API failure: no decision
SCALE_CALLS=""
gh() { return 1; }
autoscale_tick
t_eq "" "$SCALE_CALLS" "autoscale_tick skips a project whose queue can't be read"

# Idle runner: records idle-since, shrinks only after the idle period
cat > "$TARGETS_FILE" << 'EOF'
auto/repo min=1 max=5
EOF
make_runner_dir 3 "https://github.com/auto/repo"
queue_stub
is_busy() { return 1; }
SCALE_CALLS=""
rm -f "$PID_DIR/idle-since-auto+repo"
autoscale_tick
t_eq "" "$SCALE_CALLS" "no shrink on the first idle tick"
t_ok "idle-since recorded" test -f "$PID_DIR/idle-since-auto+repo"
echo $(( $(date +%s) - 11 * 60 )) > "$PID_DIR/idle-since-auto+repo"
autoscale_tick
t_eq " auto+repo=1" "$SCALE_CALLS" "shrinks by one after AUTOSCALE_IDLE_MINUTES"
case "$(cat "$PID_DIR/autoscale.last")" in
    *"2 → 1 (idle 11m)") t_eq 1 1 "autoscale.last records the idle shrink" ;;
    *) t_eq "... 2 → 1 (idle 11m)" "$(cat "$PID_DIR/autoscale.last")" "autoscale.last records the idle shrink" ;;
esac

# Global MAX_RUNNERS: other projects' runners count against the cap
cat > "$TARGETS_FILE" << 'EOF'
auto/repo min=1 max=10
fixed/repo
EOF
rm -rf "$RUNNER_BASE_DIR"/runner-3
MAX_RUNNERS=3
queue_stub a,macos a,macos a,macos a,macos a,macos
CACHED_LABELS="macos a"
is_busy() { return 0; }
SCALE_CALLS=""
autoscale_tick
t_eq " auto+repo=2" "$SCALE_CALLS" "growth limited so the total stays within MAX_RUNNERS"
MAX_RUNNERS=20

CACHED_LABELS="macos arm64 apple-silicon"

# label_satisfiable / fleet_labels
FLEET=$(fleet_labels)
t_eq "self-hosted macos arm64 macos arm64 apple-silicon" "$FLEET" "fleet_labels: implicit + detected, lowercase"
t_ok "labels satisfied case-insensitively" label_satisfiable "Self-Hosted,macOS,ARM64" "$FLEET"
t_fail_ok "a label the fleet lacks is unsatisfiable" label_satisfiable "self-hosted,gpu" "$FLEET"
t_ok "a job with no labels is satisfiable" label_satisfiable "" "$FLEET"
t_ok "a custom label the host detected counts" label_satisfiable "apple-silicon" "$FLEET"

# At most 10 runs are inspected
queue_stub macos macos macos macos macos macos macos macos macos macos macos macos
t_eq "10" "$(target_queue_depth https://github.com/auto/repo 1 1)" "target_queue_depth looks at no more than 10 runs"

# Org targets: pressure of 1 when all of our runners are busy
t_eq "1" "$(target_queue_depth https://github.com/myorg 2 2)" "org: all busy counts as 1 queued"
t_eq "0" "$(target_queue_depth https://github.com/myorg 2 1)" "org: an idle runner means no pressure"
t_eq "0" "$(target_queue_depth https://github.com/myorg 0 0)" "org: no runners means no signal"

# Dashboard/menu tag
printf '3\t0\n' > "$PID_DIR/queue-auto+repo.txt"
tag=$(autoscale_tag https://github.com/auto/repo queue)
case "$tag" in *"auto 1-10"*"queued: 3"*) t_eq 1 1 "autoscale_tag shows bounds and queue" ;; *) t_eq "auto 1-10 ... queued: 3" "$tag" "autoscale_tag shows bounds and queue" ;; esac
t_eq "" "$(autoscale_tag https://github.com/fixed/repo queue)" "autoscale_tag is empty for an unbounded project with no queue"
M_URLS=(); M_WANT=(); M_CUR=(); M_SEL=0; M_MSG=""
menu_reload
case "$(render_target_menu tui)" in *"auto 1-10"*) t_eq 1 1 "project menu shows bounds" ;; *) t_eq 1 0 "project menu shows bounds" ;; esac

# menu_reload: a count changed underneath (autoscale) is not a pending edit
M_URLS=("https://github.com/auto/repo"); M_CUR=(5); M_WANT=(5)
menu_reload
t_eq "${M_CUR[0]}" "${M_WANT[0]}" "menu_reload follows the real count when nothing was pending"
M_URLS=("https://github.com/auto/repo"); M_CUR=(5); M_WANT=(7)
menu_reload
t_eq "7" "${M_WANT[0]}" "menu_reload keeps a real pending edit"

# Trailing comments after the entry/bounds
split_target_line "c/d max=2   # quiet repo"
t_eq "c/d|0|2" "$TL_ENTRY|$TL_MIN|$TL_MAX" "split_target_line allows a trailing comment"
split_target_line "e/f # no bounds"
t_eq "e/f||" "$TL_ENTRY|$TL_MIN|$TL_MAX" "trailing comment without bounds"
