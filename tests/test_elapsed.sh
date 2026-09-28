#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

mkdir -p "$LOG_DIR"

# ----------------------------------------------------------------------------
# log_ts_to_epoch round-trips a known timestamp on this platform
# ----------------------------------------------------------------------------

FIXED_EPOCH=1700000000
# Render the same epoch the way runnermaxxer.sh itself would, GNU or BSD.
ts_str=$(date -u -d "@$FIXED_EPOCH" '+%Y-%m-%d %H:%M:%S' 2>/dev/null) \
    || ts_str=$(date -u -r "$FIXED_EPOCH" '+%Y-%m-%d %H:%M:%S')

t_eq "$FIXED_EPOCH" "$(log_ts_to_epoch "${ts_str}Z")" "log_ts_to_epoch round-trips a known epoch"
t_fail_ok "log_ts_to_epoch fails on garbage input" log_ts_to_epoch "not a timestamp"

# ----------------------------------------------------------------------------
# format_duration
# ----------------------------------------------------------------------------

t_eq "45s" "$(format_duration 45)" "format_duration: 45s"
t_eq "12m" "$(format_duration 720)" "format_duration: 720s -> 12m"
t_eq "1h05m" "$(format_duration 3900)" "format_duration: 3900s -> 1h05m"
t_eq "0s" "$(format_duration 0)" "format_duration: 0s"

# ----------------------------------------------------------------------------
# get_runner_status appends an elapsed-time suffix for a running job whose
# log timestamp is recent, and omits it when the timestamp is malformed
# ----------------------------------------------------------------------------

# 65s ago -> format_duration(65) == "1m"
recent_ts=$(date -u -d "@$(( $(date -u +%s) - 65 ))" '+%Y-%m-%d %H:%M:%S' 2>/dev/null) \
    || recent_ts=$(date -u -r "$(( $(date -u +%s) - 65 ))" '+%Y-%m-%d %H:%M:%S')

make_log 1 "${recent_ts}Z: Running job: build"
t_eq "running: build (1m)" "$(get_runner_status 1)" "get_runner_status appends (1m) elapsed suffix for a recent running job"

# Malformed timestamp prefix -> log_ts_to_epoch can't parse it -> no suffix
make_log 2 "not-a-real-timestamp Running job: build"
t_eq "running: build" "$(get_runner_status 2)" "get_runner_status omits the suffix when the timestamp is malformed"
