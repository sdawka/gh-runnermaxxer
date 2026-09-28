#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

mkdir -p "$PID_DIR" "$LOG_DIR"

# ----------------------------------------------------------------------------
# build_runner_pick_list: PICK_IDS in project order, headers only on the
# first runner of each group
# ----------------------------------------------------------------------------

cat > "$TARGETS_FILE" << 'EOF'
owner/projA
owner/projB
EOF

make_runner_dir 1 "https://github.com/owner/projA"
make_runner_dir 2 "https://github.com/owner/projA"
make_runner_dir 3 "https://github.com/owner/projB"

build_runner_pick_list

ids=$(printf '%s,' "${PICK_IDS[@]}")
t_eq "1,2,3," "$ids" "build_runner_pick_list lists runners in project order"

t_eq "3" "${#PICK_ITEMS[@]}" "build_runner_pick_list produces one row per runner"

case "${PICK_HEADERS[0]}" in
    *"owner/projA"*) t_eq "1" "1" "row 0 (first of group A) has a header naming projA" ;;
    *) t_eq "header naming owner/projA" "${PICK_HEADERS[0]}" "row 0 (first of group A) has a header naming projA" ;;
esac
t_eq "" "${PICK_HEADERS[1]}" "row 1 (second of group A) has no header"
case "${PICK_HEADERS[2]}" in
    *"owner/projB"*) t_eq "1" "1" "row 2 (first of group B) has a header naming projB" ;;
    *) t_eq "header naming owner/projB" "${PICK_HEADERS[2]}" "row 2 (first of group B) has a header naming projB" ;;
esac

# ----------------------------------------------------------------------------
# render_ui runs without error with a couple of fake targets/runners, and
# shows the selector marker plus the pending "→" tag when M_WANT != M_CUR
# ----------------------------------------------------------------------------

is_running() { return 0; }
free_disk_mb() { echo 5000; }
get_runner_status() { echo "idle"; }

CACHED_LABELS="test-labels"
M_URLS=("https://github.com/owner/projA" "https://github.com/owner/projB")
M_CUR=(1 2)
M_WANT=(3 2)   # projA has a pending change, projB does not
M_LISTED=(1 1)
M_SEL=0
M_MSG=""

out_file="$TEST_TMP_DIR/render_ui.out"
render_ui > "$out_file" 2>&1
rc=$?
t_eq "0" "$rc" "render_ui exits successfully"

t_ok "render_ui output contains the selector marker" bash -c 'grep -qF "▸" "'"$out_file"'"'
t_ok "render_ui output contains the pending change tag (→)" bash -c 'grep -qF "→" "'"$out_file"'"'
t_ok "render_ui output contains both project labels" bash -c 'grep -qF "projA" "'"$out_file"'" && grep -qF "projB" "'"$out_file"'"'
