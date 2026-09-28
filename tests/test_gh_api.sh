#!/usr/bin/env bash
# gh_api wrapper, error classifier, header parsing, per-target errors
source "$(dirname "$0")/_helper.sh"
mkdir -p "$PID_DIR"

# --- gh_classify truth table (pure) -----------------------------------------
cls() { gh_classify "$1" "$2" "$3"; echo "$GH_CLASS"; }
t_eq "ok"        "$(cls 0 "" "")" "classify: rc 0 -> ok"
t_eq "auth"      "$(cls 4 "" "To get started with GitHub CLI, please run:  gh auth login")" "classify: rc 4 -> auth"
t_eq "auth"      "$(cls 1 401 "Bad credentials (HTTP 401)")" "classify: HTTP 401 -> auth"
t_eq "auth"      "$(cls 1 "" "You are not logged into any GitHub hosts. Run gh auth login to authenticate.")" "classify: not logged in text -> auth"
t_eq "ratelimit" "$(cls 1 403 "API rate limit exceeded for user ID 1. (HTTP 403)")" "classify: 403 + rate limit -> ratelimit"
t_eq "ratelimit" "$(cls 1 403 "You have exceeded a secondary rate limit (HTTP 403)")" "classify: 403 + secondary rate limit -> ratelimit"
t_eq "ratelimit" "$(cls 1 429 "Too Many Requests (HTTP 429)")" "classify: 429 -> ratelimit"
t_eq "sso"       "$(cls 1 403 "Resource protected by organization SAML enforcement. (HTTP 403)")" "classify: 403 + SAML -> sso"
t_eq "forbidden" "$(cls 1 403 "Resource not accessible by integration (HTTP 403)")" "classify: plain 403 -> forbidden"
t_eq "notfound"  "$(cls 1 404 "Not Found (HTTP 404)")" "classify: 404 -> notfound"
t_eq "network"   "$(cls 1 "" "error connecting to api.github.com")" "classify: error connecting -> network"
t_eq "network"   "$(cls 1 "" "Get \"https://api.github.com/user\": dial tcp: lookup api.github.com: no such host")" "classify: dial tcp -> network"
t_eq "error"     "$(cls 1 "" "unexpected end of JSON input")" "classify: anything else -> error"
t_eq "error"     "$(cls 1 500 "Server Error (HTTP 500)")" "classify: 500 -> error"

# --- gh_api: stdout passed through, stderr classified ------------------------
gh() { echo body; echo "gh: Not Found (HTTP 404)" >&2; return 1; }
rm -f "$PID_DIR/gh.last" "$PID_DIR/gh.err"
rc=0; out=$(gh_api repos/o/r) || rc=$?
t_eq "body" "$out" "gh_api passes stdout through"
t_eq "1" "$rc" "gh_api returns gh's status"
gh_api repos/o/r >/dev/null || true
t_eq "notfound 404 Not Found (HTTP 404)" "$GH_CLASS $GH_HTTP $GH_MSG" "gh_api sets GH_CLASS/GH_HTTP/GH_MSG in the calling shell"
t_ok "gh.last written" test -s "$PID_DIR/gh.last"
t_eq "notfound" "$(cut -f1 "$PID_DIR/gh.last")" "gh.last records the class"
t_fail_ok "gh.err not written for a target-specific class" test -e "$PID_DIR/gh.err"

# From a $(...) subshell the variables are lost; gh_last_load restores them
GH_CLASS=""; GH_HTTP=""; GH_MSG=""
x=$(gh_api repos/o/r) || true
t_eq "" "$GH_CLASS" "subshell call leaves the parent's GH_CLASS alone"
gh_last_load
t_eq "notfound|404|Not Found (HTTP 404)" "$GH_CLASS|$GH_HTTP|$GH_MSG" "gh_last_load reads the subshell's result back"

# Empty HTTP field survives the round trip (no tab collapsing)
gh() { echo "gh: dial tcp: i/o timeout" >&2; return 1; }
x=$(gh_api user) || true
gh_last_load
t_eq "network||dial tcp: i/o timeout" "$GH_CLASS|$GH_HTTP|$GH_MSG" "gh_last_load keeps an empty HTTP field"
t_ok "network failure is global: gh.err written" test -s "$PID_DIR/gh.err"

gh() { echo "gh: Bad credentials (HTTP 401)" >&2; return 1; }
gh_api user >/dev/null || true
t_eq "auth" "$(cut -f1 "$PID_DIR/gh.err")" "401 lands in gh.err"
t_eq "auth" "$(gh_state)" "gh_state reports the sticky global error"
gh() { echo '{}'; }
gh_api user >/dev/null
t_eq "ok" "$GH_CLASS" "successful call -> ok"
t_fail_ok "successful call removes gh.err" test -e "$PID_DIR/gh.err"
t_eq "unknown" "$(gh_state)" "gh_state is unknown before any probe"

# stderr is not leaked to the caller's stderr
gh() { echo "gh: boom" >&2; return 1; }
t_eq "" "$(gh_api x 2>&1 >/dev/null || true)" "gh_api swallows gh's stderr"
t_eq "0" "$(ls -A "$PID_DIR" | grep -c '^\.gh\.stderr' || true)" "no stderr scratch files left behind"

# GH_BIN override
fakegh() { echo "fake $*"; }
GH_BIN=fakegh
t_eq "fake api user" "$(gh_api user)" "GH_BIN selects the gh binary"
GH_BIN=gh

# --- gh_api_hdr / gh_header ---------------------------------------------------
gh() { printf 'HTTP/2.0 200 OK\r\nContent-Type: application/json\r\nX-Oauth-Scopes: repo, admin:org\r\n\r\n{"login":"sahil"}\n'; }
body=$(gh_api_hdr user)
t_eq '{"login":"sahil"}' "$body" "gh_api_hdr prints only the body"
t_eq "repo, admin:org" "$(gh_header X-OAuth-Scopes)" "gh_header reads a header case-insensitively and strips CR"
t_eq "application/json" "$(gh_header content-type)" "gh_header lower-case name"
t_fail_ok "gh_header returns 1 for an absent header" gh_header X-GitHub-SSO

printf 'HTTP/2.0 200 OK\r\nX-OAuth-Scopes: \r\n' > "$TEST_TMP_DIR/hdr"
GH_HDR_FILE="$TEST_TMP_DIR/hdr"
t_ok "gh_header succeeds for a present but empty header" gh_header X-OAuth-Scopes
t_eq "" "$(gh_header X-OAuth-Scopes)" "empty header value"

# SAML: the authorize URL comes from the X-GitHub-SSO header of a retry
gh() {
    if [[ "$2" == "-i" ]]; then
        printf 'HTTP/2.0 403 Forbidden\r\nX-GitHub-SSO: required; url=https://github.com/orgs/o/sso?authorization_request=1\r\n\r\n{}\n'
    fi
    echo "gh: Resource protected by organization SAML enforcement. (HTTP 403)" >&2
    return 1
}
gh_api orgs/o >/dev/null || true
t_eq "sso" "$GH_CLASS" "SAML 403 -> sso"
case "$GH_MSG" in
    *"authorize: https://github.com/orgs/o/sso?authorization_request=1") t_eq 1 1 "sso message carries the authorize URL" ;;
    *) t_eq "... authorize: URL" "$GH_MSG" "sso message carries the authorize URL" ;;
esac

# --- gh_error_note --------------------------------------------------------------
case "$(gh_error_note auth)" in *"gh auth login"*) t_eq 1 1 "auth note tells how to log in" ;; *) t_eq 1 0 "auth note tells how to log in" ;; esac
case "$(gh_error_note notfound "Not Found")" in *"404"*"(Not Found)") t_eq 1 1 "notfound note appends the message" ;; *) t_eq 1 0 "notfound note appends the message" ;; esac
t_eq "token lacks admin:org" "$(gh_error_note scope "token lacks admin:org")" "scope note is the message itself"

# --- per-target errors ----------------------------------------------------------
U="https://github.com/o/r"
target_set_error "$U" notfound "Not Found"
t_eq "notfound	Not Found" "$(target_error "$U")" "target_error prints class<TAB>msg"
t_ok "error file named by target_key" test -f "$PID_DIR/target-o+r.err"
echo "$(printf 'notfound\tNot Found\t100')" > "$PID_DIR/target-o+r.err"
target_set_error "$U" notfound "Not Found again"
t_eq "100" "$(cut -f3 "$PID_DIR/target-o+r.err")" "same class keeps its since timestamp"
target_set_error "$U" forbidden $'multi\tline\nmsg'
t_eq "forbidden	multi line msg" "$(target_error "$U")" "tabs/newlines in the message are flattened"
t_fail_ok "new class resets since" test "$(cut -f3 "$PID_DIR/target-o+r.err")" = 100
target_clear_error "$U" scope
t_ok "clear with a different class keeps the error" test -f "$PID_DIR/target-o+r.err"
target_clear_error_except "$U" forbidden
t_ok "clear_except of the same class keeps it" test -f "$PID_DIR/target-o+r.err"
target_clear_error "$U"
t_eq "" "$(target_error "$U")" "target_clear_error removes it"

# --- call sites -------------------------------------------------------------------
gh() { echo "gh: Not Found (HTTP 404)" >&2; return 1; }
rc=0; target_accessible "$U" || rc=$?
t_eq "1 notfound" "$rc $GH_CLASS" "target_accessible: 404 -> 1, GH_CLASS notfound"
t_eq "notfound" "$(target_error "$U" | cut -f1)" "target_accessible records the target error"
target_clear_error "$U"

t_ok "runner_gone_from_github: 404 for the target counts as gone" runner_gone_from_github "$U" mac-1
gh() { echo "gh: Must have admin rights (HTTP 403)" >&2; return 1; }
t_fail_ok "runner_gone_from_github: 403 is not gone" runner_gone_from_github "$U" mac-1
gh() { printf 'mac-2\n'; }
t_ok "runner_gone_from_github: listed without the name -> gone" runner_gone_from_github "$U" mac-1
t_fail_ok "runner_gone_from_github: listed -> not gone" runner_gone_from_github "$U" mac-2

# target_queue_depth failure records a target error
gh() { echo "gh: API rate limit exceeded (HTTP 403)" >&2; return 1; }
rc=0; target_queue_depth "$U" 1 1 >/dev/null || rc=$?
t_eq "1" "$rc" "target_queue_depth fails on an API error"
t_eq "ratelimit" "$(target_error "$U" | cut -f1)" "target_queue_depth records the class"
target_clear_error "$U"

# latest_runner_tag: cache, refresh, failure
rm -f "$RUNNER_BASE_DIR/.latest-runner-tag"
CALLS="$TEST_TMP_DIR/tag_calls"; : > "$CALLS"
gh() { echo call >> "$CALLS"; echo v2.337.0; }
latest_runner_tag >/dev/null
t_eq "v2.337.0" "$(latest_runner_tag)" "latest_runner_tag prints the tag"
t_eq "1" "$(grep -c . "$CALLS")" "latest_runner_tag uses its 24h cache"
latest_runner_tag refresh >/dev/null
t_eq "2" "$(grep -c . "$CALLS")" "latest_runner_tag refresh skips the cache"
t_eq "v2.337.0" "$(cached_latest_runner_tag)" "cached_latest_runner_tag reads the cache"
t_eq "2.334.0" "$(tarball_version /x/actions-runner-osx-arm64-2.334.0.tar.gz)" "tarball_version parses the file name"
