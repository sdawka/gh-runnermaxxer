#!/usr/bin/env bash
# Registration / removal tokens instead of passing the user's token to config.sh
source "$(dirname "$0")/_helper.sh"
mkdir -p "$PID_DIR" "$LOG_DIR"

GH_LOG="$TEST_TMP_DIR/gh.calls"
: > "$GH_LOG"
gh() { echo "$*" >> "$GH_LOG"; echo tok123; }

REPO="https://github.com/o/r"
ORG="https://github.com/myorg"

# --- registration_token / remove_token ----------------------------------------
t_eq "tok123" "$(registration_token "$REPO")" "registration_token prints the token"
t_eq "api -X POST repos/o/r/actions/runners/registration-token --jq .token" "$(tail -1 "$GH_LOG")" "repo: POST repos/O/R/actions/runners/registration-token"
registration_token "$ORG" >/dev/null
t_eq "api -X POST orgs/myorg/actions/runners/registration-token --jq .token" "$(tail -1 "$GH_LOG")" "org: POST orgs/O/actions/runners/registration-token"
remove_token "$REPO" >/dev/null
t_eq "api -X POST repos/o/r/actions/runners/remove-token --jq .token" "$(tail -1 "$GH_LOG")" "remove_token uses remove-token"

gh() { echo "gh: Must have admin rights to Repository. (HTTP 403)" >&2; return 1; }
rc=0; registration_token "$REPO" >/dev/null || rc=$?
t_eq "1" "$rc" "registration_token fails on 403"
t_eq "forbidden" "$(cut -f1 "$PID_DIR/target-o+r.err")" "403 recorded as a forbidden target error"

# A scope diagnosis from the probe is not overwritten by the 403 it predicts
target_set_error "$REPO" scope "token lacks the repo scope"
registration_token "$REPO" >/dev/null || true
t_eq "scope" "$(cut -f1 "$PID_DIR/target-o+r.err")" "an existing scope error survives a 403"
gh() { echo tok123; }
registration_token "$REPO" >/dev/null
t_fail_ok "a successful registration clears the scope error" test -e "$PID_DIR/target-o+r.err"

gh() { :; }     # exit 0 but no token
rc=0; registration_token "$REPO" >/dev/null || rc=$?
t_eq "1" "$rc" "an empty token is a failure"
target_clear_error "$REPO"

# --- setup_runner end to end against a fake tarball ------------------------------
RUNNER_TAR=$(make_fake_tarball 2.334.0)
RUNNER_NAME_PREFIX="mac"
EPHEMERAL_RUNNERS=0
detect_labels() { echo "macos arm64"; }
free_disk_mb() { echo 99999; }
: > "$GH_LOG"; rm -f "$FAKE_CONFIG_LOG"
gh() { echo "$*" >> "$GH_LOG"; echo tok123; }

t_ok "setup_runner succeeds" setup_runner 1 "$REPO"
argv=$(cat "$FAKE_CONFIG_LOG")
case "$argv" in *"--token tok123"*) t_eq 1 1 "config.sh gets --token with the registration token" ;; *) t_eq "--token tok123" "$argv" "config.sh gets --token with the registration token" ;; esac
case "$argv" in *"--pat"*) t_eq "no --pat" "$argv" "config.sh never gets --pat" ;; *) t_eq 1 1 "config.sh never gets --pat" ;; esac
case "$argv" in *"--name mac-1 --url $REPO"*"--labels macos,arm64"*) t_eq 1 1 "config.sh gets name, url and labels" ;; *) t_eq "--name mac-1 --url $REPO ... --labels macos,arm64" "$argv" "config.sh gets name, url and labels" ;; esac
t_eq "2.334.0" "$(cat "$PID_DIR/runner-1.version")" "runner-N.version seeded from the tarball name"
t_eq "$REPO" "$(cat "$PID_DIR/runner-1.target")" "runner-N.target written"
t_eq "mac-1" "$(cat "$PID_DIR/runner-1.name")" "runner-N.name written"
t_eq "1" "$(grep -c registration-token "$GH_LOG")" "one registration token per runner"

# Token failure: nothing extracted
gh() { echo "gh: Must have admin rights (HTTP 403)" >&2; return 1; }
rc=0; setup_runner 2 "$REPO" 2>/dev/null || rc=$?
t_eq "1" "$rc" "setup_runner fails when no token can be minted"
t_fail_ok "no runner dir is created on a token failure" test -d "$RUNNER_BASE_DIR/runner-2"
msg=$(setup_runner 2 "$REPO" 2>&1 || true)
case "$msg" in *"403"*) t_eq 1 1 "the warning names the 403" ;; *) t_eq "...403..." "$msg" "the warning names the 403" ;; esac
target_clear_error "$REPO"

# config.sh fails after writing .runner: unregistered with a remove token
: > "$GH_LOG"; rm -f "$FAKE_CONFIG_LOG" "$ORPHANS_FILE"
gh() { echo "$*" >> "$GH_LOG"; echo tok123; }
export FAKE_CONFIG_RC=1
rc=0; setup_runner 3 "$REPO" >/dev/null 2>&1 || rc=$?
unset FAKE_CONFIG_RC
t_eq "1" "$rc" "setup_runner fails when config.sh fails"
t_eq "1" "$(grep -c remove-token "$GH_LOG")" "a remove token is minted for the half-registered runner"
t_ok "config.sh remove --token is called" grep -qx 'remove --token tok123' "$FAKE_CONFIG_LOG"
t_fail_ok "no orphan recorded when the cleanup worked" test -s "$ORPHANS_FILE"
t_fail_ok "runner dir removed" test -d "$RUNNER_BASE_DIR/runner-3"

# --- reconfigure_runner: marker line before the token fetch (backoff, S4) -----------
mkdir -p "$RUNNER_BASE_DIR/runner-4"
cp "$TEST_TMP_DIR/fake-runner-src/config.sh" "$RUNNER_BASE_DIR/runner-4/"
echo "$REPO" > "$PID_DIR/runner-4.target"
touch "$PID_DIR/runner-4.ephemeral"
: > "$LOG_DIR/runner-4.log"
gh() { echo "gh: Must have admin rights (HTTP 403)" >&2; return 1; }
t_fail_ok "reconfigure_runner fails without a token" reconfigure_runner 4
t_ok "the re-register marker is logged even when the token fetch fails" grep -q 're-registering ephemeral runner-4' "$LOG_DIR/runner-4.log"
case "$(get_lasterr 4)" in "re-register failed: "*"403"*) t_eq 1 1 "lasterr names the class" ;; *) t_eq "re-register failed: ...403..." "$(get_lasterr 4)" "lasterr names the class" ;; esac
t_fail_ok "a failed re-register is not mistaken for a finished job" ephemeral_job_finished 4
target_clear_error "$REPO"

gh() { echo tok123; }
rm -f "$FAKE_CONFIG_LOG"
t_ok "reconfigure_runner succeeds with a token" reconfigure_runner 4
case "$(cat "$FAKE_CONFIG_LOG")" in *"--token tok123 --replace --ephemeral"*) t_eq 1 1 "re-register passes --token and --ephemeral" ;; *) t_eq "--token tok123 --replace --ephemeral" "$(cat "$FAKE_CONFIG_LOG")" "re-register passes --token and --ephemeral" ;; esac

# --- remove_runner ----------------------------------------------------------------
stop_runner() { :; }
make_runner_dir 5 "$REPO"
cp "$TEST_TMP_DIR/fake-runner-src/config.sh" "$RUNNER_BASE_DIR/runner-5/"
rm -f "$ORPHANS_FILE" "$FAKE_CONFIG_LOG"
gh() { echo tok123; }
remove_runner 5 2>/dev/null
t_ok "remove_runner unregisters with a remove token" grep -qx 'remove --token tok123' "$FAKE_CONFIG_LOG"
t_fail_ok "no orphan after a clean removal" test -s "$ORPHANS_FILE"

make_runner_dir 6 "$REPO"
cp "$TEST_TMP_DIR/fake-runner-src/config.sh" "$RUNNER_BASE_DIR/runner-6/"
gh() { echo "gh: Not Found (HTTP 404)" >&2; return 1; }
remove_runner 6 2>/dev/null
t_fail_ok "remove_runner: target gone (404) -> no orphan recorded" test -s "$ORPHANS_FILE"
t_fail_ok "runner dir deleted" test -d "$RUNNER_BASE_DIR/runner-6"

make_runner_dir 7 "$REPO"
cp "$TEST_TMP_DIR/fake-runner-src/config.sh" "$RUNNER_BASE_DIR/runner-7/"
gh() { echo "gh: Must have admin rights (HTTP 403)" >&2; return 1; }
remove_runner 7 2>/dev/null
t_ok "remove_runner: 403 -> orphan recorded" grep -qx 'runner-7' "$ORPHANS_FILE"
