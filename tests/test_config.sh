#!/usr/bin/env bash
# shellcheck disable=SC2218  # real function is used first, then stubbed later on purpose
# Config parser (no `source`), validation, clamping, save round trip, reload
source "$(dirname "$0")/_helper.sh"
mkdir -p "$PID_DIR" "$LOG_DIR"
SAMPLE="$(dirname "$SCRIPT_UNDER_TEST")/.runnermaxxer.conf.sample"

reset_defaults() {
    RUNNER_NAME_PREFIX=""; MAX_RUNNERS=20; REFRESH_INTERVAL=5; MAX_RESTART_ATTEMPTS=5
    MAX_LOG_SIZE_MB=10; GH_HEALTH_TICKS=12; SHARED_TOOL_CACHE=1; EPHEMERAL_RUNNERS=0
    AUTOSCALE=0; AUTOSCALE_IDLE_MINUTES=10; REPO_URL=""; ORG_URL=""
    CONFIG_WARNINGS=()
}
warnings() { printf '%s\n' ${CONFIG_WARNINGS[@]+"${CONFIG_WARNINGS[@]}"}; }

# --- the shipped sample parses cleanly ---------------------------------------------
reset_defaults
cp "$SAMPLE" "$CONFIG_FILE"
parse_config_file "$CONFIG_FILE"
t_eq "my-runner 20 5 5 10 12 1 0 0 10" \
    "$RUNNER_NAME_PREFIX $MAX_RUNNERS $REFRESH_INTERVAL $MAX_RESTART_ATTEMPTS $MAX_LOG_SIZE_MB $GH_HEALTH_TICKS $SHARED_TOOL_CACHE $EPHEMERAL_RUNNERS $AUTOSCALE $AUTOSCALE_IDLE_MINUTES" \
    "sample: every key loaded"
t_eq "0" "${#CONFIG_WARNINGS[@]}" "sample: no warnings"

# --- hostile and malformed lines ------------------------------------------------------
reset_defaults
PWNED="$TEST_TMP_DIR/pwned"
cat > "$CONFIG_FILE" << EOF
MAX_RUNNERS="5" # note
AUTOSCALE=1
RUNNER_NAME_PREFIX='x-y'
EVIL=\$(touch $PWNED)
REFRESH_INTERVAL=abc
RUNNER_BASE_DIR=/x
GH_HEALTH_TICKS="\$(touch $PWNED)"
MAX_LOG_SIZE_MB="3"; touch $PWNED
touch $PWNED
   # indented comment
NOPE=1
EPHEMERAL_RUNNERS="07"
EOF
printf 'AUTOSCALE_IDLE_MINUTES="4"\r\n' >> "$CONFIG_FILE"
parse_config_file "$CONFIG_FILE"
t_fail_ok "nothing in the file is executed" test -e "$PWNED"
t_eq "5" "$MAX_RUNNERS" "double-quoted value with a trailing comment"
t_eq "1" "$AUTOSCALE" "bare value"
t_eq "x-y" "$RUNNER_NAME_PREFIX" "single-quoted value"
t_eq "5" "$REFRESH_INTERVAL" "invalid value keeps the default"
t_eq "12" "$GH_HEALTH_TICKS" "command substitution in quotes rejected"
t_eq "10" "$MAX_LOG_SIZE_MB" "trailing command after the value rejects the line"
t_eq "0" "$EPHEMERAL_RUNNERS" "07 is not a bool"
t_eq "4" "$AUTOSCALE_IDLE_MINUTES" "CRLF line endings tolerated"
w=$(warnings)
t_ok 'EVIL=$(...) line warned' grep -q 'line 4 (EVIL) ignored' <<< "$w"
t_ok "unknown key warned" grep -q 'ignored unknown key NOPE' <<< "$w"
t_ok "invalid value warned with the rule" grep -q "ignored invalid REFRESH_INTERVAL='abc'.*seconds" <<< "$w"
t_ok "RUNNER_BASE_DIR warned" grep -q 'set RUNNER_BASE_DIR in the environment' <<< "$w"
t_ok "non KEY=VALUE line warned" grep -q 'not KEY=VALUE' <<< "$w"

# --- config_set -----------------------------------------------------------------------
reset_defaults
t_fail_ok "config_set MAX_RUNNERS 0 fails" config_set MAX_RUNNERS 0
t_fail_ok "config_set rejects a 61-character prefix" config_set RUNNER_NAME_PREFIX "$(printf 'a%.0s' $(seq 1 61))"
t_ok "config_set accepts a 60-character prefix" config_set RUNNER_NAME_PREFIX "$(printf 'a%.0s' $(seq 1 60))"
t_ok "config_set GH_HEALTH_TICKS 0 ok" config_set GH_HEALTH_TICKS 0
t_eq "0" "$GH_HEALTH_TICKS" "config_set assigns"
t_fail_ok "config_set rejects an unknown key" config_set EVIL 1
t_fail_ok "config_set rejects RUNNER_BASE_DIR" config_set RUNNER_BASE_DIR /x
t_fail_ok "config_set rejects \$ in a value" config_set RUNNER_NAME_PREFIX 'a$b'
t_fail_ok "config_set rejects a bool out of range" config_set AUTOSCALE 2
config_set MAX_RUNNERS 007
t_eq "7" "$MAX_RUNNERS" "config_set strips leading zeros"
msg=$(config_set REFRESH_INTERVAL 0 2>&1 || true)
case "$msg" in *"seconds, 1-3600"*) t_eq 1 1 "config_set prints the rule" ;; *) t_eq "...seconds, 1-3600..." "$msg" "config_set prints the rule" ;; esac

# --- sanitize_settings clamps and notes it ----------------------------------------------
reset_defaults
REFRESH_INTERVAL=99999; MAX_RUNNERS=0; GH_HEALTH_TICKS=x; AUTOSCALE=5
sanitize_settings
t_eq "3600" "$REFRESH_INTERVAL" "REFRESH_INTERVAL clamped to 3600"
t_eq "1" "$MAX_RUNNERS" "MAX_RUNNERS clamped to 1"
t_eq "12" "$GH_HEALTH_TICKS" "non-number reset to default"
t_eq "0" "$AUTOSCALE" "bad bool reset"
w=$(warnings)
t_ok "clamp noted in CONFIG_WARNINGS" grep -q 'REFRESH_INTERVAL clamped to 3600' <<< "$w"
sanitize_settings
t_eq "1" "$(warnings | grep -c 'REFRESH_INTERVAL clamped')" "a second sanitize doesn't duplicate the note"

# --- save_config round trip ------------------------------------------------------------
reset_defaults
RUNNER_NAME_PREFIX="rt-host"; MAX_RUNNERS=33; REFRESH_INTERVAL=7; MAX_RESTART_ATTEMPTS=4
MAX_LOG_SIZE_MB=12; GH_HEALTH_TICKS=0; SHARED_TOOL_CACHE=0; EPHEMERAL_RUNNERS=1
AUTOSCALE=1; AUTOSCALE_IDLE_MINUTES=3
save_config quiet
t_ok "save_config writes KEY=\"VALUE\"" grep -qx 'MAX_RUNNERS="33"' "$CONFIG_FILE"
before="$RUNNER_NAME_PREFIX $MAX_RUNNERS $REFRESH_INTERVAL $MAX_RESTART_ATTEMPTS $MAX_LOG_SIZE_MB $GH_HEALTH_TICKS $SHARED_TOOL_CACHE $EPHEMERAL_RUNNERS $AUTOSCALE $AUTOSCALE_IDLE_MINUTES"
reset_defaults
parse_config_file "$CONFIG_FILE"
after="$RUNNER_NAME_PREFIX $MAX_RUNNERS $REFRESH_INTERVAL $MAX_RESTART_ATTEMPTS $MAX_LOG_SIZE_MB $GH_HEALTH_TICKS $SHARED_TOOL_CACHE $EPHEMERAL_RUNNERS $AUTOSCALE $AUTOSCALE_IDLE_MINUTES"
t_eq "$before" "$after" "save_config then parse_config_file round-trips every key"
t_eq "0" "${#CONFIG_WARNINGS[@]}" "a saved config parses without warnings"
RUNNER_NAME_PREFIX='a"b$(x)'
save_config quiet
t_ok "save_config never writes a quote-breaking value" grep -qx 'RUNNER_NAME_PREFIX=""' "$CONFIG_FILE"

# --- legacy REPO_URL is still migrated -----------------------------------------------------
reset_defaults
rm -f "$TARGETS_FILE"
printf 'REPO_URL="https://github.com/o/legacy.git"\nMAX_RUNNERS="3"\n' > "$CONFIG_FILE"
load_config >/dev/null 2>&1
t_ok "legacy REPO_URL moved to the targets file" grep -qx 'o/legacy' "$TARGETS_FILE"
t_fail_ok "and dropped from the config" grep -q REPO_URL "$CONFIG_FILE"
t_eq "3" "$MAX_RUNNERS" "other keys kept through the migration"

# --- run_onboarding without a terminal ------------------------------------------------------
rc=0; out=$( (run_onboarding) < /dev/null 2>&1 ) || rc=$?
t_eq "2" "$rc" "run_onboarding without a tty exits 2"
case "$out" in *"Setup"*) t_eq "no banner" "$out" "no banner printed" ;; *) t_eq 1 1 "no banner printed" ;; esac

# --- config_reload_if_changed ---------------------------------------------------------------
LOADS="$TEST_TMP_DIR/loads"
: > "$LOADS"
load_config() { echo x >> "$LOADS"; }
dlog() { :; }
CONFIG_LOADED_MTIME=""
config_reload_if_changed
t_eq "0" "$(wc -l < "$LOADS" | tr -d ' ')" "first call just records the signature"
config_reload_if_changed
t_eq "0" "$(wc -l < "$LOADS" | tr -d ' ')" "unchanged files -> no reload"
echo '# changed' >> "$CONFIG_FILE"
touch -t 203001010000 "$CONFIG_FILE"
config_reload_if_changed
t_eq "1" "$(wc -l < "$LOADS" | tr -d ' ')" "changed config -> reload"
echo 'o/r' >> "$TARGETS_FILE"
config_reload_if_changed
t_eq "2" "$(wc -l < "$LOADS" | tr -d ' ')" "changed targets file -> reload"
config_reload_if_changed
t_eq "2" "$(wc -l < "$LOADS" | tr -d ' ')" "and only once"
