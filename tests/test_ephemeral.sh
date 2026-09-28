#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

mkdir -p "$PID_DIR" "$LOG_DIR"

URL="https://github.com/owner/repo"

# ----------------------------------------------------------------------------
# runner_registered_url falls back to runner-N.target when .runner is missing
# ----------------------------------------------------------------------------

make_runner_dir 1   # no url -> no .runner file written
echo "$URL" > "$PID_DIR/runner-1.target"
t_eq "$URL" "$(runner_registered_url 1)" "runner_registered_url falls back to runner-N.target when .runner is missing"

# .runner still wins when present
make_runner_dir 2 "$URL"
echo "https://github.com/owner/other" > "$PID_DIR/runner-2.target"
t_eq "$URL" "$(runner_registered_url 2)" "runner_registered_url prefers .runner over runner-N.target when both exist"

# Neither present -> empty
make_runner_dir 3
t_eq "" "$(runner_registered_url 3)" "runner_registered_url is empty with neither .runner nor runner-N.target"

# ----------------------------------------------------------------------------
# is_ephemeral marker
# ----------------------------------------------------------------------------

t_fail_ok "is_ephemeral is false without the marker" is_ephemeral 4
touch "$PID_DIR/runner-4.ephemeral"
t_ok "is_ephemeral is true with the marker" is_ephemeral 4

# ----------------------------------------------------------------------------
# ephemeral_job_finished truth table
# ----------------------------------------------------------------------------

# Not ephemeral -> false, regardless of anything else
rm -f "$PID_DIR/runner-5.ephemeral"
make_runner_dir 5   # no .runner
make_log 5 "2024-01-01 12:00:00Z: Job build completed with result: Succeeded"
t_fail_ok "ephemeral_job_finished is false when the runner isn't ephemeral" ephemeral_job_finished 5

# Ephemeral, but .runner is present (still registered) -> false
touch "$PID_DIR/runner-6.ephemeral"
make_runner_dir 6 "$URL"   # writes .runner
make_log 6 "2024-01-01 12:00:00Z: Job build completed with result: Succeeded"
t_fail_ok "ephemeral_job_finished is false while .runner still exists" ephemeral_job_finished 6

# Ephemeral, .runner gone, completed line newer than the re-register marker -> true
touch "$PID_DIR/runner-7.ephemeral"
make_runner_dir 7   # no .runner (deleted itself after the job)
make_log 7 \
    "2024-01-01 11:00:00Z: [runnermaxxer] re-registering ephemeral runner-7 (2024-01-01 11:00:00)" \
    "2024-01-01 12:00:00Z: Job build completed with result: Succeeded"
t_ok "ephemeral_job_finished is true when the completed line is newer than the re-register marker" ephemeral_job_finished 7

# Ephemeral, .runner gone, but the re-register marker is the newest relevant
# line (a fresh re-register attempt, job not actually finished yet) -> false
touch "$PID_DIR/runner-8.ephemeral"
make_runner_dir 8
make_log 8 \
    "2024-01-01 12:00:00Z: Job build completed with result: Succeeded" \
    "2024-01-01 13:00:00Z: [runnermaxxer] re-registering ephemeral runner-8 (2024-01-01 13:00:00)"
t_fail_ok "ephemeral_job_finished is false when the re-register marker is the newest line" ephemeral_job_finished 8

# ----------------------------------------------------------------------------
# runner_env exports both tool-cache variables and creates the directory
# ----------------------------------------------------------------------------

(
    SHARED_TOOL_CACHE=1
    runner_env
    t_eq "$RUNNER_BASE_DIR/.toolcache" "$RUNNER_TOOL_CACHE" "runner_env exports RUNNER_TOOL_CACHE"
    t_eq "$RUNNER_BASE_DIR/.toolcache" "$AGENT_TOOLSDIRECTORY" "runner_env exports AGENT_TOOLSDIRECTORY"
    t_ok "runner_env creates the shared tool-cache directory" bash -c '[[ -d "'"$RUNNER_BASE_DIR"'/.toolcache" ]]'
)

(
    SHARED_TOOL_CACHE=0
    unset RUNNER_TOOL_CACHE AGENT_TOOLSDIRECTORY 2>/dev/null || true
    runner_env
    t_eq "" "${RUNNER_TOOL_CACHE:-}" "runner_env does not export RUNNER_TOOL_CACHE when SHARED_TOOL_CACHE=0"
    t_eq "" "${AGENT_TOOLSDIRECTORY:-}" "runner_env does not export AGENT_TOOLSDIRECTORY when SHARED_TOOL_CACHE=0"
)
