#!/usr/bin/env bash
# gh_auth_probe: user, scopes, and per-target permission
source "$(dirname "$0")/_helper.sh"
mkdir -p "$PID_DIR"
no_color() { RED=''; GREEN=''; YELLOW=''; BLUE=''; CYAN=''; BOLD=''; DIM=''; NC=''; }
no_color

printf 'myorg\no/r\n' > "$TARGETS_FILE"
ORG="https://github.com/myorg"
REPO="https://github.com/o/r"
GH_LOG="$TEST_TMP_DIR/gh.calls"

# --- classic token: scopes from X-OAuth-Scopes -----------------------------------
gh() {
    echo "$*" >> "$GH_LOG"
    printf 'HTTP/2.0 200 OK\r\nX-Oauth-Scopes: repo, read:org\r\n\r\n{"login":"sahil","id":1}\n'
}
: > "$GH_LOG"
out=$(gh_auth_probe 2>&1)
gh_auth_probe quiet
t_eq "sahil" "$(cat "$PID_DIR/gh.user")" "gh.user from the body"
t_eq "repo,read:org" "$(cat "$PID_DIR/gh.scopes")" "gh.scopes comma-separated, spaces stripped"
t_eq "ok" "$(cat "$PID_DIR/gh.state")" "gh.state ok"
t_eq "github.com" "$(cat "$PID_DIR/gh.host")" "gh.host defaults to github.com"
t_ok "gh.checked is an epoch" grep -qE '^[0-9]+$' "$PID_DIR/gh.checked"
t_eq "scope" "$(target_error "$ORG" | cut -f1)" "org target without admin:org gets a scope error"
case "$(target_error "$ORG")" in *"admin:org"*"gh auth refresh"*) t_eq 1 1 "scope error names admin:org and the fix" ;; *) t_eq "...admin:org...gh auth refresh..." "$(target_error "$ORG")" "scope error names admin:org and the fix" ;; esac
t_eq "" "$(target_error "$REPO")" "repo target with repo scope is clear"
t_eq "0" "$(grep -c registration-token "$GH_LOG")" "classic token: no registration token minted"
case "$out" in *"✓ gh: sahil (repo, read:org)"*) t_eq 1 1 "prints a one-line summary" ;; *) t_eq "✓ gh: sahil (repo, read:org)" "$out" "prints a one-line summary" ;; esac
case "$out" in *"myorg (organization): token lacks"*) t_eq 1 1 "prints the target's scope problem" ;; *) t_eq "myorg (organization): token lacks..." "$out" "prints the target's scope problem" ;; esac

# Scope added later: the error clears on the next probe
gh() { printf 'HTTP/2.0 200 OK\r\nX-OAuth-Scopes: repo, admin:org\r\n\r\n{"login":"sahil"}\n'; }
gh_auth_probe quiet
t_eq "" "$(target_error "$ORG")" "scope error clears once admin:org is granted"

# A non-scope error on the target is left alone by the probe
target_set_error "$REPO" notfound "Not Found"
gh_auth_probe quiet
t_eq "notfound" "$(target_error "$REPO" | cut -f1)" "probe leaves a non-scope target error alone"
target_clear_error "$REPO"

# --- fine-grained token: no scope header, probe by minting tokens -------------------
gh() {
    echo "$*" >> "$GH_LOG"
    case "$*" in
        *"-i user"*) printf 'HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n{"login":"sahil"}\n' ;;
        *orgs/myorg/actions/runners/registration-token*) echo "gh: Resource not accessible by personal access token (HTTP 403)" >&2; return 1 ;;
        *registration-token*) echo tok ;;
    esac
}
: > "$GH_LOG"
gh_auth_probe quiet
t_eq "" "$(cat "$PID_DIR/gh.scopes")" "no scope header -> empty gh.scopes"
t_eq "2" "$(grep -c registration-token "$GH_LOG")" "probe mints one registration token per target"
t_eq "scope" "$(target_error "$ORG" | cut -f1)" "403 on the probe -> scope error"
t_eq "" "$(target_error "$REPO")" "successful probe -> clear"
out=$(gh_auth_probe 2>&1)
case "$out" in *"fine-grained"*) t_eq 1 1 "summary says the token has no scope list" ;; *) t_eq "...fine-grained..." "$out" "summary says the token has no scope list" ;; esac

# --- not logged in -------------------------------------------------------------------
gh() { echo "gh: Bad credentials (HTTP 401)" >&2; return 1; }
rc=0; gh_auth_probe quiet || rc=$?
t_eq "1" "$rc" "401 -> probe returns 1"
t_eq "auth" "$(cat "$PID_DIR/gh.state")" "gh.state=auth"
t_eq "Bad credentials (HTTP 401)" "$(cat "$PID_DIR/gh.msg")" "gh.msg keeps gh's message"
t_eq "auth" "$GH_CLASS" "GH_CLASS left at the user call's class"
t_eq "auth" "$(gh_state)" "gh_state=auth"

# --- check_github_health re-probes while the state is broken -----------------------
PROBES=0
gh_auth_probe() { PROBES=$((PROBES + 1)); }
autoscale_tick() { :; }
check_github_health
t_eq "1" "$PROBES" "check_github_health re-probes when gh_state != ok"
rm -f "$PID_DIR/gh.err"; echo ok > "$PID_DIR/gh.state"
gh() { case "$*" in *rate_limit*) printf '5000\t1\n' ;; *) echo '{}' ;; esac; }
check_github_health
t_eq "1" "$PROBES" "check_github_health skips the probe when gh_state = ok"
