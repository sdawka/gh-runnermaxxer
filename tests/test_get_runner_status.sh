#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

# Missing log -> "no logs"
t_eq "no logs" "$(get_runner_status 1)" "missing log file"

# Empty log -> "no logs"
mkdir -p "$LOG_DIR"
: > "$LOG_DIR/runner-2.log"
t_eq "no logs" "$(get_runner_status 2)" "empty log file"

# Idle: last relevant line is "Listening for Jobs"
make_log 3 \
    "2024-01-01 Starting Runner listener" \
    "2024-01-01 Listening for Jobs"
t_eq "idle" "$(get_runner_status 3)" "idle when last line is Listening for Jobs"

# Running: last relevant line is "Running job: build"
make_log 4 \
    "2024-01-01 Listening for Jobs" \
    "2024-01-01 Running job: build"
t_eq "running: build" "$(get_runner_status 4)" "running when last line is Running job"

# Regression: Running job followed by completion must NOT report running
make_log 5 \
    "2024-01-01 Listening for Jobs" \
    "2024-01-01 Running job: build" \
    "2024-01-01 Job build completed with result: Succeeded"
t_eq "idle (last: Succeeded)" "$(get_runner_status 5)" "completed job after running is not reported as running"

# Connection error
make_log 6 "2024-01-01 Could not connect to GitHub"
t_eq "connection error" "$(get_runner_status 6)" "connection error"

# Auth error
make_log 7 "2024-01-01 Authentication failed"
t_eq "auth error" "$(get_runner_status 7)" "auth error"

# Starting
make_log 8 "2024-01-01 Starting Runner listener"
t_eq "starting..." "$(get_runner_status 8)" "starting"

# Exiting
make_log 9 "2024-01-01 Exiting runner"
t_eq "exiting" "$(get_runner_status 9)" "exiting"

# Unknown: no lifecycle line present at all
make_log 10 "2024-01-01 some unrelated chatter"
t_eq "unknown" "$(get_runner_status 10)" "unknown when no lifecycle line matches"

# Job name truncated to 25 chars
make_log 11 "2024-01-01 Running job: this-is-a-very-long-job-name-that-should-be-truncated"
t_eq "running: this-is-a-very-long-job-n" "$(get_runner_status 11)" "job name truncated to 25 chars"
job_name="$(get_runner_status 11)"
job_name="${job_name#running: }"
t_eq "25" "${#job_name}" "truncated job name length is exactly 25"
