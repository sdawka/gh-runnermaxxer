#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

# lower
t_eq "hello world" "$(lower "Hello World")" "lower lowercases"
t_eq "already" "$(lower "already")" "lower is a no-op on lowercase input"

# next_free_id: with no runner dirs, first free id is 1
t_eq "1" "$(next_free_id)" "next_free_id with no runner dirs"

# next_free_id skips existing dirs
mkdir -p "$RUNNER_BASE_DIR/runner-1" "$RUNNER_BASE_DIR/runner-2"
t_eq "3" "$(next_free_id)" "next_free_id skips existing runner-1 and runner-2"

# next_free_id fills a gap
mkdir -p "$RUNNER_BASE_DIR/runner-3"
rm -rf "$RUNNER_BASE_DIR/runner-2"
t_eq "2" "$(next_free_id)" "next_free_id fills the lowest free gap"

# get_runner_ids sorts numerically, not lexically (1, 2, 10 - not 1, 10, 2)
rm -rf "$RUNNER_BASE_DIR"/runner-*
mkdir -p "$RUNNER_BASE_DIR/runner-1" "$RUNNER_BASE_DIR/runner-2" "$RUNNER_BASE_DIR/runner-10"
ids=$(get_runner_ids | tr '\n' ',')
t_eq "1,2,10," "$ids" "get_runner_ids sorts numerically"

# is_running: missing pid file
rm -rf "$RUNNER_BASE_DIR"/runner-*
t_fail_ok "is_running is false when the pid file is missing" is_running 1

# is_running: stale pid file pointing at a real but unrelated process
mkdir -p "$PID_DIR"
sleep 60 &
real_pid=$!
echo "$real_pid" > "$PID_DIR/runner-1.pid"
t_fail_ok "is_running is false for a pid whose command line doesn't mention runner-1/" is_running 1
kill "$real_pid" 2>/dev/null || true
wait "$real_pid" 2>/dev/null || true

# is_running: pid file pointing at a pid that doesn't exist at all
echo "999999" > "$PID_DIR/runner-2.pid"
t_fail_ok "is_running is false for a pid that doesn't exist" is_running 2
