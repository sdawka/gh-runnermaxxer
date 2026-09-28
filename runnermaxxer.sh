#!/usr/bin/env bash
# ============================================================================
# gh-runnermaxxer - GitHub Actions Self-Hosted Runner Manager
# ============================================================================

# Bash version guard (before set -u so it works under ancient shells)
if [ -z "${BASH_VERSION:-}" ]; then
    echo "Error: this script requires bash" >&2
    exit 1
fi
if [ "${BASH_VERSINFO[0]}" -lt 3 ]; then
    echo "Error: bash >= 3.2 required (found $BASH_VERSION)" >&2
    exit 1
fi

set -euo pipefail

VERSION="3.1.0"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG_FILE="$SCRIPT_DIR/.runnermaxxer.conf"
TARGETS_FILE="$SCRIPT_DIR/.runnermaxxer.targets"

# Defaults (can be overridden by config file or environment)
RUNNER_BASE_DIR="${RUNNER_BASE_DIR:-$SCRIPT_DIR/runners}"
RUNNER_NAME_PREFIX="${RUNNER_NAME_PREFIX:-}"
# Legacy single-target settings (v2). Migrated into the targets file on load.
REPO_URL="${REPO_URL:-}"
ORG_URL="${ORG_URL:-}"
MAX_RUNNERS="${MAX_RUNNERS:-20}"
REFRESH_INTERVAL="${REFRESH_INTERVAL:-5}"
MAX_RESTART_ATTEMPTS="${MAX_RESTART_ATTEMPTS:-5}"
MAX_LOG_SIZE_MB="${MAX_LOG_SIZE_MB:-10}"
GH_HEALTH_TICKS="${GH_HEALTH_TICKS:-12}"   # GitHub-side health check every N refresh ticks (0 = off)
SHARED_TOOL_CACHE="${SHARED_TOOL_CACHE:-1}"  # 1 = all runners share $RUNNER_BASE_DIR/.toolcache
EPHEMERAL_RUNNERS="${EPHEMERAL_RUNNERS:-0}"  # 1 = register new runners with --ephemeral (one job each)
AUTOSCALE="${AUTOSCALE:-0}"                  # 1 = scale projects with min=/max= bounds from GitHub's queue
AUTOSCALE_IDLE_MINUTES="${AUTOSCALE_IDLE_MINUTES:-10}"  # idle this long before an autoscaled project shrinks by one

# Keys a config file may set (everything else is ignored with a warning)
CONFIG_KEYS="RUNNER_NAME_PREFIX MAX_RUNNERS REFRESH_INTERVAL MAX_RESTART_ATTEMPTS MAX_LOG_SIZE_MB GH_HEALTH_TICKS SHARED_TOOL_CACHE EPHEMERAL_RUNNERS AUTOSCALE AUTOSCALE_IDLE_MINUTES"
# Notes about the configuration (ignored keys, clamped values), shown in the
# state snapshot's warnings[]
CONFIG_WARNINGS=()

PID_DIR="$RUNNER_BASE_DIR/.pids"
LOG_DIR="$RUNNER_BASE_DIR/.logs"
LOCK_FILE="$RUNNER_BASE_DIR/.runnermaxxer.lock"
ORPHANS_FILE="$RUNNER_BASE_DIR/.orphaned-registrations"
DAEMON_PID_FILE="$RUNNER_BASE_DIR/.daemon.pid"
OP_LOCK_FILE="$RUNNER_BASE_DIR/.runnermaxxer.op.lock"   # see acquire_op_lock
DAEMON_LOG="$LOG_DIR/runnermaxxer.log"
SERVICE_LABEL="com.gh-runnermaxxer"       # launchd label
SERVICE_UNIT="gh-runnermaxxer.service"    # systemd user unit

LOCK_ACQUIRED=0
OP_LOCK_ACQUIRED=0
TUI_ACTIVE=0
DAEMON_MODE=0
SUPERVISOR_TICK=0
RUNNER_TAR=""
CLI_TARGET=""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
DIM='\033[2m'
NC='\033[0m'

# ============================================================================
# Messaging / traps
# ============================================================================

die() {
    echo -e "${RED}Error: $1${NC}" >&2
    exit 1
}

warn() {
    echo -e "${YELLOW}Warning: $1${NC}" >&2
}

info() {
    echo -e "${BLUE}$1${NC}"
}

cleanup() {
    # Only release the lock if this process owns it, so a second instance
    # that failed preflight doesn't delete the first instance's lock.
    if [[ "$LOCK_ACQUIRED" == "1" ]]; then
        rm -f "$LOCK_FILE"
    fi
    [[ "$OP_LOCK_ACQUIRED" == "1" ]] && rm -f "$OP_LOCK_FILE"
    # Per-process gh scratch files (see gh_api)
    rm -f "$PID_DIR/.gh.last.$$" "$PID_DIR/.qunsat.$$" "$PID_DIR/.gh.hdr.$$" 2>/dev/null || true
    if [[ "$DAEMON_MODE" == "1" && "$(cat "$DAEMON_PID_FILE" 2>/dev/null)" == "$$" ]]; then
        rm -f "$DAEMON_PID_FILE"
    fi
    # Restore terminal state in case we die inside `read -s` or a child
    # mangled the tty.
    if [[ "$TUI_ACTIVE" == "1" ]]; then
        stty sane 2>/dev/null || true
        tput cnorm 2>/dev/null || true
    fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ============================================================================
# Platform detection
# ============================================================================

OS_FAMILY=""
RUNNER_OS=""
RUNNER_ARCH=""
IS_WSL=0

detect_platform() {
    case "$(uname -s)" in
        Darwin)
            OS_FAMILY="macos"
            RUNNER_OS="osx"
            ;;
        Linux)
            OS_FAMILY="linux"
            RUNNER_OS="linux"
            if grep -qi microsoft /proc/version 2>/dev/null; then
                IS_WSL=1
                if uname -r | grep -q -- '-Microsoft$'; then
                    die "WSL1 detected - the GitHub runner requires WSL2 or native Linux"
                fi
            fi
            # The runner is dotnet-based and requires glibc
            local ldd_out
            ldd_out=$(ldd --version 2>&1 || true)
            if echo "$ldd_out" | grep -qi musl; then
                die "musl libc detected (Alpine?) - the GitHub runner requires glibc and cannot run here"
            fi
            ;;
        MINGW*|MSYS*|CYGWIN*)
            die "Windows is not supported by this script - the Windows runner uses a .zip and config.cmd. See https://github.com/actions/runner"
            ;;
        *)
            OS_FAMILY="unknown"
            RUNNER_OS="linux"
            warn "Unknown OS '$(uname -s)' - assuming Linux; things may not work"
            ;;
    esac

    case "$(uname -m)" in
        arm64|aarch64) RUNNER_ARCH="arm64" ;;
        x86_64|amd64)  RUNNER_ARCH="x64" ;;
        *)
            RUNNER_ARCH="x64"
            warn "Unknown architecture '$(uname -m)' - assuming x64"
            ;;
    esac

    # A Rosetta-translated shell on Apple Silicon reports x86_64; the native
    # arch is what matters for the runner binary.
    if [[ "$OS_FAMILY" == "macos" ]]; then
        if [[ "$(sysctl -in sysctl.proc_translated 2>/dev/null || echo 0)" == "1" ]]; then
            RUNNER_ARCH="arm64"
        fi
    fi
}

default_hostname() {
    local h
    h=$(hostname -s 2>/dev/null || echo "${HOSTNAME%%.*}")
    # Sanitize to characters GitHub accepts in runner names
    h=$(echo "$h" | tr -cd 'a-zA-Z0-9_-')
    [[ -z "$h" ]] && h="runner"
    echo "$h"
}

get_cpu_cores() {
    nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 1
}

get_mem_gb() {
    local mem_gb=0
    if [[ "$OS_FAMILY" == "macos" ]]; then
        mem_gb=$(( $(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1073741824 ))
    elif [[ -r /proc/meminfo ]]; then
        mem_gb=$(( $(awk '/^MemTotal/ {print $2; exit}' /proc/meminfo 2>/dev/null || echo 0) / 1048576 ))
    fi
    echo "$mem_gb"
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        echo ""
    fi
}

free_disk_mb() {
    local mb
    mb=$(df -Pk "$RUNNER_BASE_DIR" 2>/dev/null | awk 'NR==2 {print int($4/1024)}' || true)
    [[ "$mb" =~ ^[0-9]+$ ]] || mb=999999
    echo "$mb"
}

# Percent of the filesystem holding $RUNNER_BASE_DIR that is free (empty
# when df can't tell). df -Pk: column 2 = size, 4 = available, in KiB.
disk_free_pct() {
    local pct
    pct=$(df -Pk "$RUNNER_BASE_DIR" 2>/dev/null | awk 'NR==2 && $2 > 0 {print int($4 * 100 / $2)}' || true)
    [[ "$pct" =~ ^[0-9]+$ ]] && echo "$pct"
    return 0
}

# Per-runner disk use (U6): du -sk of _work and _diag into
# runner-N.workmb, in the background so a large _work never stalls a tick.
# Every DISK_USAGE_TICKS ticks for all runners, and right away for a runner
# that has no figure yet. A .pending marker keeps a slow du from being
# started twice (stale after 10 minutes).
DISK_USAGE_TICKS=120
DISK_TICK=0
DISK_LOW_WARNED=0
disk_usage_tick() {
    local all=0 id f d pend age now pct
    now=$(date +%s)
    DISK_TICK=$((DISK_TICK + 1))
    if [[ $DISK_TICK -ge $DISK_USAGE_TICKS ]]; then all=1; DISK_TICK=0; fi
    for id in $(get_runner_ids); do
        f="$PID_DIR/runner-$id.workmb"; pend="$f.pending"
        [[ $all -eq 1 || ! -f "$f" ]] || continue
        if [[ -f "$pend" ]]; then
            age=$(( now - $(file_mtime "$pend") ))
            [[ $age -lt 600 ]] && continue
        fi
        d="$RUNNER_BASE_DIR/runner-$id"
        : > "$pend"
        (
            exec >/dev/null 2>&1 </dev/null
            mb=$(du -sk "$d/_work" "$d/_diag" 2>/dev/null | awk '{s += $1} END {print int(s / 1024)}')
            [[ "$mb" =~ ^[0-9]+$ ]] || mb=0
            echo "$mb" > "$f.tmp.$$.$id" && mv -f "$f.tmp.$$.$id" "$f"
            rm -f "$pend"
        ) &
    done
    # Filesystem: note once when free space drops under 15 %
    pct=$(disk_free_pct)
    if [[ -n "$pct" && $pct -lt 15 ]]; then
        if [[ $DISK_LOW_WARNED -eq 0 ]]; then
            dlog "low disk: ${pct}% free on the runners' filesystem ($(free_disk_mb) MB)"
            DISK_LOW_WARNED=1
        fi
    else
        DISK_LOW_WARNED=0
    fi
    return 0
}

# Whether `date` understands GNU's `-d`. Probed once and cached so hot
# paths (status refresh) don't fork an extra process per call.
DATE_IS_GNU=""
_detect_date_style() {
    [[ -n "$DATE_IS_GNU" ]] && return 0
    if date -d @0 >/dev/null 2>&1; then
        DATE_IS_GNU=1
    else
        DATE_IS_GNU=0
    fi
    return 0
}

# Convert a runner log timestamp ("2026-09-27 14:44:46Z") to epoch
# seconds. Prints nothing and returns 1 on any failure so callers can
# silently omit derived output.
log_ts_to_epoch() {
    local ts="${1%Z}" epoch
    _detect_date_style
    if [[ "$DATE_IS_GNU" == "1" ]]; then
        epoch=$(date -u -d "$ts" +%s 2>/dev/null) || return 1
    else
        epoch=$(date -j -u -f '%Y-%m-%d %H:%M:%S' "$ts" +%s 2>/dev/null) || return 1
    fi
    [[ "$epoch" =~ ^[0-9]+$ ]] || return 1
    echo "$epoch"
    return 0
}

# Render a duration in seconds as a compact "1h05m" / "12m" / "45s" string.
format_duration() {
    local secs="$1" h m s
    [[ "$secs" =~ ^[0-9]+$ ]] || return 1
    h=$(( secs / 3600 ))
    m=$(( (secs % 3600) / 60 ))
    s=$(( secs % 60 ))
    if [[ $h -gt 0 ]]; then
        printf '%dh%02dm\n' "$h" "$m"
    elif [[ $m -gt 0 ]]; then
        printf '%dm\n' "$m"
    else
        printf '%ds\n' "$s"
    fi
    return 0
}

# ============================================================================
# Runner tarball detection / download
# ============================================================================

detect_runner_tarball() {
    local newest="" f
    for f in "$SCRIPT_DIR"/actions-runner-"${RUNNER_OS}"-"${RUNNER_ARCH}"-*.tar.gz; do
        [[ -f "$f" ]] || continue
        if [[ -z "$newest" || "$f" -nt "$newest" ]]; then
            newest="$f"
        fi
    done
    echo "$newest"
}

download_runner_tarball() {
    info "Fetching latest runner release for ${RUNNER_OS}-${RUNNER_ARCH}..."

    local tag
    tag=$(latest_runner_tag refresh) || {
        gh_last_load
        warn "Could not query the latest runner release: $(gh_error_note "$GH_CLASS" "$GH_MSG")"
        return 1
    }

    local ver="${tag#v}"
    local asset="actions-runner-${RUNNER_OS}-${RUNNER_ARCH}-${ver}.tar.gz"

    info "Downloading $asset..."
    if ! "$GH_BIN" release download "$tag" -R actions/runner --pattern "$asset" --dir "$SCRIPT_DIR" --clobber; then
        warn "Download failed. Get it manually from https://github.com/actions/runner/releases"
        return 1
    fi

    # Verify checksum against the SHA published in the release notes
    local expected actual
    expected=$(gh_api "repos/actions/runner/releases/tags/$tag" --jq .body \
        | sed -n "s/.*BEGIN SHA ${RUNNER_OS}-${RUNNER_ARCH} -->\([a-f0-9]\{64\}\)<.*/\1/p" | head -1) || expected=""
    if [[ -n "$expected" ]]; then
        actual=$(sha256_file "$SCRIPT_DIR/$asset")
        if [[ -n "$actual" && "$actual" != "$expected" ]]; then
            rm -f "$SCRIPT_DIR/$asset"
            warn "Checksum mismatch for $asset - deleted. Try again or download manually."
            return 1
        fi
        echo -e "${GREEN}✓${NC} Checksum verified"
    else
        warn "Couldn't find the checksum in the release notes for $tag - installed without verification"
    fi

    # Browsers and some download paths tag files with com.apple.quarantine,
    # which Gatekeeper then applies to the extracted runner binaries
    if [[ "$OS_FAMILY" == "macos" ]] && command -v xattr >/dev/null 2>&1; then
        xattr -d com.apple.quarantine "$SCRIPT_DIR/$asset" 2>/dev/null || true
    fi
    echo -e "${GREEN}✓${NC} Downloaded $asset"
    return 0
}

# Latest actions/runner release tag (e.g. v2.337.0). Cached in
# $RUNNER_BASE_DIR/.latest-runner-tag (epoch + tag, one per line) and reused
# for 24h so this doesn't add a network round-trip to every launch or poll.
# latest_runner_tag [refresh] prints the tag, or nothing (returning 1) when
# it can't be found out; "refresh" skips the cache.
latest_runner_tag() {
    local cache_file="$RUNNER_BASE_DIR/.latest-runner-tag" now cached_epoch cached_tag tag
    now=$(date -u +%s 2>/dev/null) || return 1
    if [[ "${1:-}" != "refresh" && -f "$cache_file" ]]; then
        cached_epoch=$(sed -n '1p' "$cache_file" 2>/dev/null)
        cached_tag=$(sed -n '2p' "$cache_file" 2>/dev/null)
        if [[ "$cached_epoch" =~ ^[0-9]+$ && -n "$cached_tag" && $(( now - cached_epoch )) -lt 86400 ]]; then
            printf '%s\n' "$cached_tag"
            return 0
        fi
    fi
    tag=$(gh_api repos/actions/runner/releases/latest --jq .tag_name) || return 1
    [[ -n "$tag" ]] || return 1
    { echo "$now"; echo "$tag"; } > "$cache_file" 2>/dev/null || true
    printf '%s\n' "$tag"
}

# The cached tag only, never calling gh (for the state snapshot)
cached_latest_runner_tag() {
    local tag
    tag=$(sed -n '2p' "$RUNNER_BASE_DIR/.latest-runner-tag" 2>/dev/null) || tag=""
    [[ -n "$tag" ]] || return 1
    printf '%s\n' "$tag"
}

# Version in a runner tarball's file name (actions-runner-OS-ARCH-VER.tar.gz)
tarball_version() {
    local base
    base=$(basename "${1:-}")
    printf '%s' "$base" | sed -n 's/^actions-runner-[a-z]*-[a-z0-9]*-\(.*\)\.tar\.gz$/\1/p'
}

# Best-effort, non-fatal check: warn once at startup if the shipped tarball
# is older than the latest actions/runner release. Never blocks startup and
# never errors out (offline, no gh auth, etc. all just skip silently).
check_tarball_freshness() {
    [[ -n "$RUNNER_TAR" ]] || return 0
    command -v "$GH_BIN" >/dev/null 2>&1 || return 0

    local base current_ver latest_tag latest_ver
    base=$(basename "$RUNNER_TAR")
    current_ver=$(echo "$base" | sed -n "s/^actions-runner-${RUNNER_OS}-${RUNNER_ARCH}-\(.*\)\.tar\.gz\$/\1/p")
    [[ -n "$current_ver" ]] || return 0

    latest_tag=$(latest_runner_tag) || return 0
    latest_ver="${latest_tag#v}"
    if [[ -n "$latest_ver" && "$latest_ver" != "$current_ver" ]]; then
        echo -e "${YELLOW}Runner tarball is $current_ver, latest is $latest_ver - run ./runnermaxxer.sh --download to update (new runners only)${NC}"
    fi
    return 0
}

# ============================================================================
# Label auto-detection (best-effort: every probe is allowed to fail)
# ============================================================================

detect_labels() {
    local labels=()

    case "$OS_FAMILY" in
        macos) labels[${#labels[@]}]="macos" ;;
        linux)
            labels[${#labels[@]}]="linux"
            if [[ -f /etc/os-release ]]; then
                local distro_id
                distro_id=$(. /etc/os-release 2>/dev/null && echo "${ID:-}") || distro_id=""
                [[ -n "$distro_id" ]] && labels[${#labels[@]}]="$distro_id"
            fi
            [[ "$IS_WSL" == "1" ]] && labels[${#labels[@]}]="wsl"
            ;;
    esac

    case "$RUNNER_ARCH" in
        arm64)
            labels[${#labels[@]}]="arm64"
            [[ "$OS_FAMILY" == "macos" ]] && labels[${#labels[@]}]="apple-silicon"
            ;;
        x64) labels[${#labels[@]}]="x64" ;;
    esac

    if [[ "$OS_FAMILY" == "macos" ]]; then
        local macos_ver
        macos_ver=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1 || true)
        [[ -n "$macos_ver" ]] && labels[${#labels[@]}]="macos-$macos_ver"
    fi

    command -v docker >/dev/null 2>&1 && labels[${#labels[@]}]="docker"

    if [[ "$OS_FAMILY" == "macos" ]]; then
        if system_profiler SPDisplaysDataType 2>/dev/null | grep -q "Metal"; then
            labels[${#labels[@]}]="metal"
        fi
    elif command -v nvidia-smi >/dev/null 2>&1; then
        labels[${#labels[@]}]="gpu"
        labels[${#labels[@]}]="nvidia"
    fi

    local mem_gb cores
    mem_gb=$(get_mem_gb)
    [[ "$mem_gb" -ge 16 ]] && labels[${#labels[@]}]="high-memory"
    [[ "$mem_gb" -ge 32 ]] && labels[${#labels[@]}]="32gb-ram"

    cores=$(get_cpu_cores)
    [[ "$cores" -ge 8 ]] && labels[${#labels[@]}]="8-core"

    echo "${labels[*]}"
    return 0
}

# ============================================================================
# Configuration
# ============================================================================

# ---- Config file parser ---------------------------------------------------
# The config file used to be `source`d, so any line in it ran as shell code
# (C1). It is parsed instead: KEY=VALUE / KEY="VALUE" / KEY='VALUE' lines,
# optional trailing "# comment", for the keys in CONFIG_KEYS (plus the legacy
# REPO_URL/ORG_URL, migrated below). Anything else is ignored with a warning.
CONFIG_LEGACY_KEYS="REPO_URL ORG_URL"

config_known_key() {
    case " $CONFIG_KEYS $CONFIG_LEGACY_KEYS " in *" $1 "*) return 0 ;; esac
    return 1
}

# config_valid_value KEY VALUE - pure. Returns 0 when VALUE is acceptable
# for KEY; the upper bounds are clamped later by sanitize_settings.
config_valid_value() {
    local k=$1 v=$2 re_prefix='^[A-Za-z0-9_-]{1,60}$'
    # shellcheck disable=SC1003  # a literal backslash, not an escape
    case "$v" in *'"'*|*"'"*|*'\'*|*'$'*|*'`'*) return 1 ;; esac
    case "$k" in
        RUNNER_NAME_PREFIX) [[ -z "$v" || "$v" =~ $re_prefix ]] ;;
        MAX_RUNNERS|REFRESH_INTERVAL|MAX_RESTART_ATTEMPTS|MAX_LOG_SIZE_MB)
            [[ "$v" =~ ^[0-9]{1,9}$ ]] && [[ $((10#$v)) -ge 1 ]] ;;
        GH_HEALTH_TICKS|AUTOSCALE_IDLE_MINUTES) [[ "$v" =~ ^[0-9]{1,9}$ ]] ;;
        SHARED_TOOL_CACHE|EPHEMERAL_RUNNERS|AUTOSCALE) [[ "$v" =~ ^[01]$ ]] ;;
        REPO_URL|ORG_URL) [[ -z "$v" ]] || valid_target_url "$(normalize_url "$v")" ;;
        *) return 1 ;;
    esac
}

# The rule for KEY, for error messages
config_rule() {
    case "$1" in
        RUNNER_NAME_PREFIX) echo "letters, digits, _ and -, at most 60 characters (GitHub caps runner names at 64)" ;;
        MAX_RUNNERS) echo "a whole number, 1-500" ;;
        REFRESH_INTERVAL) echo "seconds, 1-3600" ;;
        MAX_RESTART_ATTEMPTS) echo "a whole number, 1-100" ;;
        MAX_LOG_SIZE_MB) echo "megabytes, 1-10000" ;;
        GH_HEALTH_TICKS) echo "a whole number of ticks, 0 (off) to 100000" ;;
        AUTOSCALE_IDLE_MINUTES) echo "minutes, 0-100000" ;;
        SHARED_TOOL_CACHE|EPHEMERAL_RUNNERS|AUTOSCALE) echo "0 or 1" ;;
        *) echo "one of: $CONFIG_KEYS" ;;
    esac
}

# Strip leading zeros from a number ("07" -> 7); anything else unchanged
config_norm_num() {
    if [[ "$1" =~ ^[0-9]{1,9}$ ]]; then echo $((10#$1)); else printf '%s\n' "$1"; fi
}

# config_set KEY VALUE - whitelist + validation, then assign. Returns 1
# (printing why to stderr) and leaves the variable alone otherwise.
config_set() {
    local k=$1 v=$2
    case " $CONFIG_KEYS " in
        *" $k "*) ;;
        *) echo "unknown setting '$k' (settings: $CONFIG_KEYS)" >&2; return 1 ;;
    esac
    [[ "$k" == "RUNNER_NAME_PREFIX" ]] || v=$(config_norm_num "$v")
    if ! config_valid_value "$k" "$v"; then
        echo "invalid value for $k: '$v' - $(config_rule "$k")" >&2
        return 1
    fi
    eval "$k=\$v"
    return 0
}

# Add a note once (sanitize_settings can run more than once per load)
config_warn() {
    local w
    for w in ${CONFIG_WARNINGS[@]+"${CONFIG_WARNINGS[@]}"}; do
        [[ "$w" == "$1" ]] && return 0
    done
    CONFIG_WARNINGS[${#CONFIG_WARNINGS[@]}]="$1"
}

# parse_config_file FILE - assign every valid KEY=VALUE line; warn about the rest
parse_config_file() {
    local f=$1 line k v n=0 name cur
    local re='^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=("([^"]*)"|'"'"'([^'"'"']*)'"'"'|([^[:space:]#"'"'"']*))[[:space:]]*(#.*)?$'
    local re_blank='^[[:space:]]*(#.*)?$'
    [[ -f "$f" ]] || return 0
    name=$(basename "$f")
    while IFS= read -r line || [[ -n "$line" ]]; do
        n=$((n + 1))
        line=${line%$'\r'}
        [[ "$line" =~ $re_blank ]] && continue
        if [[ ! "$line" =~ $re ]]; then
            k=${line%%=*}; k=${k#"${k%%[![:space:]]*}"}
            if [[ "$line" == *=* && "$k" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
                config_warn "$name line $n ($k) ignored: not KEY=VALUE (a plain or quoted value, no \$ or commands)"
            else
                config_warn "$name line $n ignored: not KEY=VALUE"
            fi
            continue
        fi
        k=${BASH_REMATCH[1]}
        v="${BASH_REMATCH[3]}${BASH_REMATCH[4]}${BASH_REMATCH[5]}"
        if [[ "$k" == "RUNNER_BASE_DIR" ]]; then
            config_warn "RUNNER_BASE_DIR in $name is ignored - set RUNNER_BASE_DIR in the environment, not the config file"
            continue
        fi
        if ! config_known_key "$k"; then
            config_warn "ignored unknown key $k in $name"
            continue
        fi
        [[ "$k" == "RUNNER_NAME_PREFIX" ]] || v=$(config_norm_num "$v")
        if ! config_valid_value "$k" "$v"; then
            eval "cur=\${$k:-}"
            config_warn "ignored invalid $k='$v' in $name ($(config_rule "$k")) - using ${cur:-the default}"
            continue
        fi
        eval "$k=\$v"
    done < "$f"
    return 0
}

# mtime of a file (0 when missing), GNU stat first (see rotate_log)
file_mtime() {
    local m
    m=$(stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || echo 0)
    [[ "$m" =~ ^[0-9]+$ ]] || m=0
    echo "$m"
}

# "mtime:size" - a same-second rewrite usually changes the size
file_sig() {
    local sz
    sz=$(stat -c %s "$1" 2>/dev/null || stat -f %z "$1" 2>/dev/null || echo 0)
    echo "$(file_mtime "$1"):$sz"
}

CONFIG_LOADED_MTIME=""
load_config() {
    CONFIG_WARNINGS=()
    parse_config_file "$CONFIG_FILE"
    [[ -z "$RUNNER_NAME_PREFIX" ]] && RUNNER_NAME_PREFIX=$(default_hostname | cut -c1-60)

    # v2 configs named a single REPO_URL/ORG_URL. Targets now live in the
    # targets file, so move it over once and drop it from the config.
    if [[ -n "$REPO_URL$ORG_URL" ]]; then
        local legacy
        legacy=$(normalize_url "${REPO_URL:-$ORG_URL}")
        if valid_repo_url "$legacy" || valid_org_url "$legacy"; then
            add_target_to_file "$legacy"
            info "Moved target $legacy from .runnermaxxer.conf to $(basename "$TARGETS_FILE")"
        fi
        REPO_URL=""; ORG_URL=""
        [[ -f "$CONFIG_FILE" ]] && save_config quiet
    fi
    CONFIG_LOADED_MTIME="$(file_sig "$CONFIG_FILE") $(file_sig "$TARGETS_FILE")"
    return 0
}

# Daemon: pick up --set-config / --set-bounds / --add-target made by other
# processes (the TUI) without a restart. Compares the config and targets
# files' mtimes with the ones seen at the last load. A changed
# REFRESH_INTERVAL applies from the next sleep, GH_HEALTH_TICKS from the
# next poll.
config_reload_if_changed() {
    local now_m
    now_m="$(file_sig "$CONFIG_FILE") $(file_sig "$TARGETS_FILE")"
    if [[ -z "$CONFIG_LOADED_MTIME" ]]; then
        CONFIG_LOADED_MTIME=$now_m
        return 0
    fi
    [[ "$now_m" == "$CONFIG_LOADED_MTIME" ]] && return 0
    load_config >/dev/null 2>&1 || true
    sanitize_settings
    CONFIG_LOADED_MTIME=$now_m
    dlog "configuration reloaded (config or targets file changed)"
    return 0
}

# Strip trailing slashes and a .git suffix so pasted URLs validate
normalize_url() {
    local u="$1"
    # SSH clone URLs (C8): git@github.com:o/r.git, ssh://git@github.com/o/r
    case "$u" in
        git@github.com:*)       u="https://github.com/${u#git@github.com:}" ;;
        ssh://git@github.com/*) u="https://github.com/${u#ssh://git@github.com/}" ;;
    esac
    u="${u%/}"
    u="${u%.git}"
    u="${u%/}"
    echo "$u"
}

# GitHub owner/repo names: letters, digits, '.', '_', '-' (no spaces - the
# target lists are word-split in for loops)
valid_repo_url() { [[ "$1" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; }
valid_org_url()  { [[ "$1" =~ ^https://github\.com/[A-Za-z0-9_.-]+$ ]]; }
# At most 60 characters: GitHub caps runner names at 64, and "-NNN" follows
valid_prefix()   { local re='^[a-zA-Z0-9_-]{1,60}$'; [[ "$1" =~ $re ]]; }

validate_config() {
    local errors=()
    local warnings=()

    if [[ ! -f "$CONFIG_FILE" ]]; then
        errors[${#errors[@]}]="No configuration file found"
        return 1
    fi

    if [[ -n "$MAX_RUNNERS" && ! "$MAX_RUNNERS" =~ ^[0-9]+$ ]]; then
        errors[${#errors[@]}]="MAX_RUNNERS must be a number, got: $MAX_RUNNERS"
    fi

    if [[ -n "$MAX_RUNNERS" && "$MAX_RUNNERS" =~ ^[0-9]+$ ]]; then
        if [[ "$MAX_RUNNERS" -lt 1 ]]; then
            errors[${#errors[@]}]="MAX_RUNNERS must be at least 1"
        elif [[ "$MAX_RUNNERS" -gt 100 ]]; then
            warnings[${#warnings[@]}]="MAX_RUNNERS is very high ($MAX_RUNNERS) - this may cause resource issues"
        fi
    fi

    if [[ -n "$RUNNER_NAME_PREFIX" ]] && ! valid_prefix "$RUNNER_NAME_PREFIX"; then
        errors[${#errors[@]}]="RUNNER_NAME_PREFIX contains invalid characters: $RUNNER_NAME_PREFIX"
        errors[${#errors[@]}]="Use only letters, numbers, underscores, and hyphens, at most 60 characters"
    fi

    if [[ ! "$SHARED_TOOL_CACHE" =~ ^[01]$ ]]; then
        errors[${#errors[@]}]="SHARED_TOOL_CACHE must be 0 or 1, got: $SHARED_TOOL_CACHE"
    fi
    if [[ ! "$EPHEMERAL_RUNNERS" =~ ^[01]$ ]]; then
        errors[${#errors[@]}]="EPHEMERAL_RUNNERS must be 0 or 1, got: $EPHEMERAL_RUNNERS"
    fi
    if [[ ! "$AUTOSCALE" =~ ^[01]$ ]]; then
        errors[${#errors[@]}]="AUTOSCALE must be 0 or 1, got: $AUTOSCALE"
    fi
    if [[ ! "$AUTOSCALE_IDLE_MINUTES" =~ ^[0-9]+$ ]]; then
        errors[${#errors[@]}]="AUTOSCALE_IDLE_MINUTES must be a number, got: $AUTOSCALE_IDLE_MINUTES"
    fi
    if [[ "$AUTOSCALE" == "1" && "$GH_HEALTH_TICKS" == "0" ]]; then
        warnings[${#warnings[@]}]="AUTOSCALE=1 has no effect while GH_HEALTH_TICKS=0 (it runs after each GitHub poll)"
    fi

    if [[ ${#warnings[@]} -gt 0 ]]; then
        local warning
        for warning in "${warnings[@]}"; do
            echo -e "${YELLOW}⚠ $warning${NC}"
        done
    fi

    if [[ ${#errors[@]} -gt 0 ]]; then
        local error
        for error in "${errors[@]}"; do
            echo -e "${RED}✗ $error${NC}"
        done
        return 1
    fi

    return 0
}

save_config() {
    local tmp="$CONFIG_FILE.tmp.$$" k v
    {
        echo "# gh-runnermaxxer configuration"
        echo "# Projects (repos/orgs) are listed in .runnermaxxer.targets, not here."
        for k in $CONFIG_KEYS; do
            eval "v=\${$k:-}"
            # Values are validated on the way in; never write one that could
            # break out of the quotes if something else put it there
            # shellcheck disable=SC1003  # a literal backslash, not an escape
            case "$v" in *'"'*|*'\'*|*'$'*|*'`'*) v="" ;; esac
            printf '%s="%s"\n' "$k" "$v"
        done
    } > "$tmp"
    mv "$tmp" "$CONFIG_FILE"
    CONFIG_LOADED_MTIME="$(file_sig "$CONFIG_FILE") $(file_sig "$TARGETS_FILE")"
    [[ "${1:-}" == "quiet" ]] || echo -e "  ${GREEN}Configuration saved${NC}"
}

run_onboarding() {
    if [[ ! -t 0 ]]; then
        echo "No configuration and no terminal to ask - run --setup interactively, or write .runnermaxxer.conf" >&2
        exit 2
    fi
    echo ""
    echo -e "${BOLD}${CYAN}╔═══════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║         gh-runnermaxxer Setup                     ║${NC}"
    echo -e "${BOLD}${CYAN}╚═══════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${DIM}Let's configure your self-hosted runner environment.${NC}"
    echo ""


    # Step 1: Runner name prefix
    echo -e "${BOLD}Step 1: Runner Name Prefix${NC}"
    echo ""
    local default_prefix
    default_prefix=$(default_hostname)
    echo -e "  Runners will be named: prefix-1, prefix-2, etc."
    echo -e "  ${DIM}Default: $default_prefix${NC}"
    printf '  Runner name prefix [%s]: ' "$default_prefix"
    read -r new_prefix
    [[ -z "$new_prefix" ]] && new_prefix="$default_prefix"

    if ! valid_prefix "$new_prefix"; then
        echo -e "\n  ${RED}Invalid prefix. Use only letters, numbers, underscores, hyphens - at most 60 characters (GitHub caps runner names at 64).${NC}"
        exit 1
    fi

    # Step 2: Max runners
    echo ""
    echo -e "${BOLD}Step 2: Maximum Runners${NC}"
    echo ""
    echo -e "  How many runners can run simultaneously, across all projects?"
    echo -e "  ${DIM}Default: 20${NC}"
    printf '  Max runners [20]: '
    read -r new_max
    [[ -z "$new_max" ]] && new_max="20"

    if [[ ! "$new_max" =~ ^[0-9]+$ ]] || [[ "$new_max" -lt 1 ]]; then
        echo -e "\n  ${RED}Invalid number. Must be 1 or greater.${NC}"
        exit 1
    fi

    # Confirm
    echo ""
    echo -e "${BOLD}Configuration Summary${NC}"
    echo -e "────────────────────────────────────────"
    echo -e "  Prefix:     ${GREEN}$new_prefix${NC}"
    echo -e "  Max:        ${GREEN}$new_max${NC}"
    echo -e "────────────────────────────────────────"
    echo ""
    printf '  Save this configuration? [Y/n]: '
    read -r confirm

    if [[ "$confirm" =~ ^[Nn] ]]; then
        echo -e "\n  ${YELLOW}Aborted. No changes saved.${NC}"
        exit 0
    fi

    RUNNER_NAME_PREFIX="$new_prefix"
    MAX_RUNNERS="$new_max"
    save_config

    echo ""
    echo -e "  ${GREEN}✓ Configuration saved to .runnermaxxer.conf${NC}"
    echo -e "  ${DIM}Next: pick the repositories/organizations to run for, and how many runners each gets.${NC}"
    echo ""
    sleep 1
}

# ============================================================================
# Targets (projects)
# ============================================================================
# A target is a repository or organization that runners register against.
# The list lives in .runnermaxxer.targets, one per line, as owner/repo,
# org-name, or a full https://github.com/... URL (blank lines and '#'
# comments are ignored). Every runner belongs to exactly one target,
# recorded by the runner itself in runner-N/.runner (gitHubUrl), so the
# runner directories on disk are the source of truth for how many runners
# each target currently has. One manager instance supervises all of them.

# Expand a targets-file entry to a full URL (empty for blanks/comments)
target_entry_to_url() {
    local e="$1"
    e="${e#"${e%%[![:space:]]*}"}"
    e="${e%"${e##*[![:space:]]}"}"
    [[ -z "$e" || "$e" == \#* ]] && { echo ""; return 0; }
    case "$e" in
        https://github.com/*) ;;
        git@github.com:*|ssh://git@github.com/*) ;;   # normalize_url maps these
        http://github.com/*)  e="https://${e#http://}" ;;
        github.com/*)         e="https://$e" ;;
        *)                    e="https://github.com/$e" ;;
    esac
    normalize_url "$e"
}

valid_target_url() { valid_repo_url "$1" || valid_org_url "$1"; }

# Split one targets-file line into its entry and optional autoscale bounds:
#   owner/repo min=1 max=5
# Sets TL_ENTRY (the entry text, "" for blank/comment lines), TL_MIN and
# TL_MAX ("" when no bounds). A missing min defaults to 0, a missing max to
# MAX_RUNNERS. A trailing "# comment" is allowed. Returns 1 when the
# suffix is malformed (unknown key,
# non-numeric value, min > max); callers then treat the line as invalid.
split_target_line() {
    local line="$1" rest tok k v mn="" mx="" has=0 toks=()
    TL_ENTRY=""; TL_MIN=""; TL_MAX=""
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" || "$line" == \#* ]] && return 0
    TL_ENTRY="${line%%[[:space:]]*}"
    rest="${line#"$TL_ENTRY"}"
    read -r -a toks <<< "$rest" || true
    for tok in ${toks[@]+"${toks[@]}"}; do
        [[ "$tok" == \#* ]] && break        # trailing comment
        k="${tok%%=*}"; v="${tok#*=}"
        [[ "$tok" == *=* && "$v" =~ ^[0-9]+$ ]] || return 1
        case "$k" in
            min) mn=$v; has=1 ;;
            max) mx=$v; has=1 ;;
            *)   return 1 ;;
        esac
    done
    [[ $has -eq 1 ]] || return 0
    [[ -n "$mn" ]] || mn=0
    [[ -n "$mx" ]] || mx=$MAX_RUNNERS
    [[ $mn -le $mx ]] || return 1
    TL_MIN=$mn; TL_MAX=$mx
    return 0
}

target_type() {
    if valid_repo_url "$1"; then echo "repo"; else echo "org"; fi
}

target_api_endpoint() {
    local path="${1#https://github.com/}"
    if [[ "$(target_type "$1")" == "repo" ]]; then
        echo "repos/$path/actions/runners"
    else
        echo "orgs/$path/actions/runners"
    fi
}

# Can the gh token see this repo/org? Returns gh's status; on failure
# GH_CLASS says why and the target gets an error record.
target_accessible() {
    local path="${1#https://github.com/}" rc=0
    if [[ "$(target_type "$1")" == "repo" ]]; then
        gh_api "repos/$path" >/dev/null || rc=$?
    else
        gh_api "orgs/$path" >/dev/null || rc=$?
    fi
    [[ $rc -eq 0 ]] || target_set_error "$1" "$GH_CLASS" "$GH_MSG"
    return $rc
}

# Short display form: owner/repo, or "org (organization)"
target_label() {
    local path="${1#https://github.com/}"
    if valid_org_url "$1"; then
        echo "$path (organization)"
    else
        echo "$path"
    fi
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# Case-insensitive URL match (GitHub owner/repo names are case-insensitive)
same_target() { [[ -n "$1" && "$(lower "$1")" == "$(lower "$2")" ]]; }

# Print valid target URLs from the targets file, one per line.
# Pass "verbose" to warn about invalid entries (only done once at startup;
# this runs every UI frame otherwise).
load_targets() {
    local line url
    [[ -f "$TARGETS_FILE" ]] || return 0
    while IFS= read -r line || [[ -n "$line" ]]; do
        url=""
        split_target_line "$line" && url=$(target_entry_to_url "$TL_ENTRY") || url="-"
        [[ -z "$url" ]] && continue
        if valid_target_url "$url"; then
            echo "$url"
        elif [[ "${1:-}" == "verbose" ]]; then
            warn "Ignoring invalid entry in $(basename "$TARGETS_FILE"): $line"
        fi
    done < "$TARGETS_FILE"
}

target_in_file() {
    local t
    while IFS= read -r t; do
        same_target "$t" "$1" && return 0
    done < <(load_targets)
    return 1
}

add_target_to_file() {
    target_in_file "$1" && return 0
    if [[ ! -f "$TARGETS_FILE" ]]; then
        echo "# Targets offered at startup (owner/repo, org, or URL; one per line)" > "$TARGETS_FILE"
    elif [[ -s "$TARGETS_FILE" && -n "$(tail -c1 "$TARGETS_FILE")" ]]; then
        echo >> "$TARGETS_FILE"     # file lacked a trailing newline
    fi
    echo "${1#https://github.com/}" >> "$TARGETS_FILE"
}

# Drop every line that resolves to this target; comments/blank lines kept
remove_target_from_file() {
    local tmp="$TARGETS_FILE.tmp.$$" line u
    [[ -f "$TARGETS_FILE" ]] || return 0
    : > "$tmp"
    while IFS= read -r line || [[ -n "$line" ]]; do
        split_target_line "$line" || true
        u=$(target_entry_to_url "$TL_ENTRY")
        if [[ -n "$u" ]] && same_target "$u" "$1"; then
            continue
        fi
        printf '%s\n' "$line" >> "$tmp"
    done < "$TARGETS_FILE"
    mv "$tmp" "$TARGETS_FILE"
}

# Autoscale bounds of a target from the targets file: prints "min max", or
# nothing when its (first) entry has none. Invalid lines are skipped.
target_bounds() {
    local line u
    [[ -f "$TARGETS_FILE" ]] || return 0
    while IFS= read -r line || [[ -n "$line" ]]; do
        split_target_line "$line" || continue
        u=$(target_entry_to_url "$TL_ENTRY")
        [[ -n "$u" ]] && same_target "$u" "$1" || continue
        [[ -n "$TL_MIN" ]] && echo "$TL_MIN $TL_MAX"
        return 0
    done < "$TARGETS_FILE"
    return 0
}

# Set (or with empty min/max, clear) a target's bounds in the targets file.
# The first line naming the target is rewritten in place, keeping its entry
# text (a trailing comment on that line is dropped); the target is
# appended first if it isn't listed.
set_target_bounds() {
    local url=$1 mn=${2:-} mx=${3:-} tmp="$TARGETS_FILE.tmp.$$" line u hit=0
    add_target_to_file "$url"
    : > "$tmp"
    while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ $hit -eq 0 ]] && split_target_line "$line"; then
            u=$(target_entry_to_url "$TL_ENTRY")
            if [[ -n "$u" ]] && same_target "$u" "$url"; then
                if [[ -n "$mn$mx" ]]; then
                    line="$TL_ENTRY min=${mn:-0} max=${mx:-$MAX_RUNNERS}"
                else
                    line="$TL_ENTRY"
                fi
                hit=1
            fi
        fi
        printf '%s\n' "$line" >> "$tmp"
    done < "$TARGETS_FILE"
    mv "$tmp" "$TARGETS_FILE"
    return 0
}

# URL a runner is registered to, from its .runner state file. Falls back
# to the target saved at setup (runner-N.target) when .runner is absent,
# e.g. an ephemeral runner between jobs.
runner_registered_url() {
    local u
    u=$(sed -n 's/.*"gitHubUrl": *"\([^"]*\)".*/\1/p' "$RUNNER_BASE_DIR/runner-$1/.runner" 2>/dev/null | head -1) || u=""
    [[ -z "$u" ]] && { u=$(cat "$PID_DIR/runner-$1.target" 2>/dev/null) || u=""; }
    [[ -n "$u" ]] && normalize_url "$u"
    return 0
}

runner_ids_for_target() {
    local id
    for id in $(get_runner_ids); do
        same_target "$(runner_registered_url "$id")" "$1" && echo "$id"
    done
    return 0
}

# Draining runners are still listed under their target (runner_ids_for_target)
# but don't count toward its size: they are already on their way out.
count_runners_for_target() {
    local n=0 id
    for id in $(runner_ids_for_target "$1"); do
        is_draining "$id" || n=$((n + 1))
    done
    echo $n
}

count_running_for_target() {
    local n=0 id
    for id in $(runner_ids_for_target "$1"); do
        is_draining "$id" && continue
        is_running "$id" && n=$((n + 1))
    done
    echo $n
}

# Runners whose .runner file is missing/unreadable (interrupted setup)
unassigned_runner_ids() {
    local id
    for id in $(get_runner_ids); do
        [[ -z "$(runner_registered_url "$id")" ]] && echo "$id"
    done
    return 0
}

# Targets-file entries in file order, followed by any target that existing
# runners are registered to but that isn't listed (so those runners can
# still be seen and scaled down). Deduplicated case-insensitively.
known_targets() {
    local seen=() t u id dup
    while IFS= read -r t; do
        [[ -z "$t" ]] && continue
        dup=0
        for u in ${seen[@]+"${seen[@]}"}; do
            same_target "$u" "$t" && { dup=1; break; }
        done
        [[ $dup -eq 1 ]] && continue
        seen[${#seen[@]}]="$t"
        echo "$t"
    done < <(load_targets; for id in $(get_runner_ids); do runner_registered_url "$id"; done)
    return 0
}

# Idle/stopped runners of a target first, then busy ones; highest ID first
# within each class. Already-draining runners are skipped (they are going
# away anyway). Prints up to N runner IDs.
pick_victims() {
    local url=$1 n=$2 idle="" busy="" id
    for id in $(runner_ids_for_target "$url" | sort -rn); do
        is_draining "$id" && continue
        if is_busy "$id"; then
            busy="$busy $id"
        else
            idle="$idle $id"
        fi
    done
    echo "$idle $busy" | tr ' ' '\n' | grep -v '^$' | head -n "$n" || true
    return 0
}

# Bring one target to exactly N runners (draining runners excluded). Busy
# runners are drained rather than killed. Prints progress; returns 1 if a
# runner failed to set up or start.
scale_target() {
    local url=$1 want=$2 cur failed=0 id next_id label k want_file
    label=$(target_label "$url")
    cur=$(count_runners_for_target "$url")
    # The requested count, for the state snapshot while this runs and after
    # a partial failure (want 5, have 3); dropped once the target gets there
    want_file="$PID_DIR/want-$(target_key "$url").txt"
    echo "$want" > "$want_file" 2>/dev/null || true

    # Cheapest way back up: cancel pending drains on this target first
    if [[ $want -gt $cur ]]; then
        for id in $(runner_ids_for_target "$url"); do
            [[ $cur -lt $want ]] || break
            is_draining "$id" || continue
            unmark_draining "$id"
            cur=$((cur + 1))
            echo -e "  ${GREEN}✓${NC} runner-$id: drain cancelled, keeping it"
        done
    fi

    if [[ $want -gt $cur ]]; then
        if ! target_accessible "$url"; then
            echo -e "  ${RED}✗${NC} $label: $(gh_error_note "$GH_CLASS" "$GH_MSG")"
            return 1   # want file kept: the snapshot shows want > have
        fi
        echo -e "  ${BLUE}$label: adding $((want - cur)) runner(s)...${NC}"
        for ((k = cur; k < want; k++)); do
            next_id=$(next_free_id)
            clear_failure_state "$next_id"
            if setup_runner "$next_id" "$url" && start_runner "$next_id"; then
                echo -e "    ${GREEN}✓${NC} runner-$next_id"
            else
                echo -e "    ${RED}✗${NC} runner-$next_id failed - not adding more to $label"
                failed=1
                break
            fi
        done
    elif [[ $want -lt $cur ]]; then
        echo -e "  ${BLUE}$label: removing $((cur - want)) runner(s)...${NC}"
        for id in $(pick_victims "$url" $((cur - want))); do
            if is_busy "$id"; then
                mark_draining "$id"
                echo -e "    ${YELLOW}runner-$id is mid-job - draining, it will be removed when the job finishes${NC}"
                continue
            fi
            echo -e "    Removing runner-$id..."
            remove_runner "$id"
        done
    fi
    [[ $failed -eq 0 ]] && rm -f "$want_file"
    return $failed
}

# ----------------------------------------------------------------------------
# Project menu: choose how many runners each target should have
# ----------------------------------------------------------------------------
# Shown at startup (and via 't' in the TUI). ↑/↓ move between projects,
# ←/→ (or +/-, or a digit) change the highlighted project's runner count.
# Enter or 's' applies: each target is scaled up or down to the chosen count.

M_URLS=(); M_CUR=(); M_WANT=(); M_RUN=(); M_LISTED=()
M_SEL=0; M_MSG=""

# Rebuild the rows from disk, keeping pending (unapplied) counts
menu_reload() {
    local old_urls=(${M_URLS[@]+"${M_URLS[@]}"}) old_want=(${M_WANT[@]+"${M_WANT[@]}"})
    local old_cur=(${M_CUR[@]+"${M_CUR[@]}"})
    local t i j cur
    M_URLS=(); M_CUR=(); M_WANT=(); M_RUN=(); M_LISTED=()
    while IFS= read -r t; do
        [[ -z "$t" ]] && continue
        i=${#M_URLS[@]}
        M_URLS[i]="$t"
        cur=$(count_runners_for_target "$t")
        M_CUR[i]=$cur
        M_WANT[i]=$cur
        M_RUN[i]=$(count_running_for_target "$t")
        for ((j = 0; j < ${#old_urls[@]}; j++)); do
            if same_target "${old_urls[j]}" "$t"; then
                # Keep only real pending edits, so a count that changed
                # underneath (autoscaling) isn't shown as a pending change
                [[ "${old_want[j]}" != "${old_cur[j]:-}" ]] && M_WANT[i]=${old_want[j]}
                break
            fi
        done
        if target_in_file "$t"; then M_LISTED[i]=1; else M_LISTED[i]=0; fi
    done < <(known_targets)
    if [[ ${#M_URLS[@]} -gt 0 && $M_SEL -ge ${#M_URLS[@]} ]]; then
        M_SEL=$((${#M_URLS[@]} - 1))
    fi
    [[ $M_SEL -lt 0 ]] && M_SEL=0
    return 0
}

menu_total_want() {
    local i n=0
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do n=$((n + M_WANT[i])); done
    echo $n
}

menu_select_url() {
    local i
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        same_target "${M_URLS[i]}" "$1" && M_SEL=$i
    done
    return 0
}

menu_adjust() {
    local delta=$1 new
    [[ ${#M_URLS[@]} -eq 0 ]] && return 0
    new=$((M_WANT[M_SEL] + delta))
    [[ $new -lt 0 ]] && new=0
    if [[ $delta -gt 0 && $(( $(menu_total_want) + delta )) -gt $MAX_RUNNERS ]]; then
        M_MSG="${YELLOW}MAX_RUNNERS ($MAX_RUNNERS) reached - raise it in .runnermaxxer.conf${NC}"
        return 0
    fi
    M_WANT[M_SEL]=$new
    return 0
}

menu_set() {
    local v=$1 others
    [[ ${#M_URLS[@]} -eq 0 ]] && return 0
    others=$(( $(menu_total_want) - M_WANT[M_SEL] ))
    if [[ $((others + v)) -gt $MAX_RUNNERS ]]; then
        M_MSG="${YELLOW}MAX_RUNNERS ($MAX_RUNNERS) reached - raise it in .runnermaxxer.conf${NC}"
        return 0
    fi
    M_WANT[M_SEL]=$v
    return 0
}

menu_add_target() {
    local entry="" url
    tput cnorm 2>/dev/null || true
    echo ""
    echo -e "  ${DIM}owner/repo, org-name, or https://github.com/...${NC}"
    printf '  Add project: '
    IFS= read -r entry || entry=""
    tput civis 2>/dev/null || true
    [[ -z "$entry" ]] && return 0

    url=$(target_entry_to_url "$entry")
    if ! valid_target_url "$url"; then
        M_MSG="${YELLOW}Invalid target (expected owner/repo, org-name, or a github.com URL)${NC}"
        return 0
    fi
    if target_in_file "$url"; then
        M_MSG="${DIM}$(target_label "$url") is already listed${NC}"
    else
        add_target_to_file "$url"
        if target_accessible "$url"; then
            M_MSG="${GREEN}Added $(target_label "$url")${NC}"
        else
            M_MSG="${YELLOW}Added $(target_label "$url"), but the gh token cannot access it - check it exists and your auth scopes${NC}"
        fi
    fi
    menu_reload
    menu_select_url "$url"
    return 0
}

menu_remove_target() {
    local url
    [[ ${#M_URLS[@]} -eq 0 ]] && return 0
    url="${M_URLS[M_SEL]}"
    if [[ ${M_CUR[M_SEL]} -gt 0 ]]; then
        M_MSG="${YELLOW}$(target_label "$url") still has ${M_CUR[M_SEL]} runner(s) - set it to 0 and apply ('s') first${NC}"
        return 0
    fi
    if [[ ${M_LISTED[M_SEL]} -eq 0 ]]; then
        M_MSG="${DIM}$(target_label "$url") is not in the targets file${NC}"
        return 0
    fi
    remove_target_from_file "$url"
    M_MSG="${DIM}Removed $(target_label "$url") from the list${NC}"
    menu_reload
    return 0
}

# 'b' in the project menu: set or clear the highlighted project's
# autoscale bounds ("min max"; empty clears), saved to the targets file
menu_set_bounds() {
    local url b entry="" mn mx extra
    [[ ${#M_URLS[@]} -eq 0 ]] && return 0
    url="${M_URLS[M_SEL]}"
    b=$(target_bounds "$url")
    tput cnorm 2>/dev/null || true
    echo ""
    echo -e "  ${DIM}Autoscale bounds for $(target_label "$url") as 'min max' (e.g. 1 5); empty clears${b:+ (current: $b)}${NC}"
    printf '  Bounds: '
    IFS= read -r entry || entry=""
    tput civis 2>/dev/null || true
    mn=""; mx=""; extra=""
    read -r mn mx extra <<< "$entry" || true
    if [[ -z "$mn" ]]; then
        [[ -z "$b" ]] && return 0
        set_target_bounds "$url"
        M_MSG="${DIM}Autoscale bounds cleared for $(target_label "$url")${NC}"
    elif [[ "$mn" =~ ^[0-9]+$ && "$mx" =~ ^[0-9]+$ && -z "$extra" && $mn -le $mx ]]; then
        if [[ $mx -gt $MAX_RUNNERS ]]; then
            M_MSG="${YELLOW}max ($mx) exceeds MAX_RUNNERS ($MAX_RUNNERS)${NC}"
            return 0
        fi
        set_target_bounds "$url" "$mn" "$mx"
        M_MSG="${GREEN}$(target_label "$url"): autoscale $mn-$mx${NC}"
        [[ "$AUTOSCALE" == "1" ]] || M_MSG="$M_MSG ${DIM}(enable AUTOSCALE with 'e' for it to take effect)${NC}"
    else
        M_MSG="${YELLOW}Expected two numbers 'min max' with min <= max${NC}"
        return 0
    fi
    menu_reload
    return 0
}

# Dim "auto MIN-MAX" (and with "queue", the cached "queued: N") after a
# project's name
autoscale_tag() {
    local b q out=""
    b=$(target_bounds "$1")
    [[ -n "$b" ]] && out="  ${DIM}auto ${b% *}-${b#* }${NC}"
    if [[ "${2:-}" == "queue" ]]; then
        q=$(cached_queue_depth "$1")
        [[ -n "$q" && "$q" -gt 0 ]] && out="$out  ${DIM}queued: $q${NC}"
    fi
    printf '%s' "$out"
    return 0
}

# Scale every target to its chosen count. Returns 0 when the menu can be
# left (nothing to do, or all changes applied), 1 to stay in the menu.
apply_target_counts() {
    local i total=0 changed=0 busy="" id failed=0
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        total=$((total + M_WANT[i]))
        [[ ${M_WANT[i]} -ne ${M_CUR[i]} ]] && changed=1
    done
    [[ $changed -eq 0 ]] && return 0
    if [[ $total -gt $MAX_RUNNERS ]]; then
        M_MSG="${YELLOW}$total runners exceeds MAX_RUNNERS ($MAX_RUNNERS)${NC}"
        return 1
    fi

    # Busy runners are drained (removed once their job finishes), not killed
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        [[ ${M_WANT[i]} -lt ${M_CUR[i]} ]] || continue
        for id in $(pick_victims "${M_URLS[i]}" $((M_CUR[i] - M_WANT[i]))); do
            is_busy "$id" && busy="$busy runner-$id"
        done
    done

    tput cnorm 2>/dev/null || true
    echo ""
    if [[ -n "$busy" ]]; then
        echo -e "  ${YELLOW}Jobs in progress on:$busy - draining, removed when their jobs finish${NC}"
    fi

    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        [[ ${M_WANT[i]} -eq ${M_CUR[i]} ]] && continue
        scale_target "${M_URLS[i]}" "${M_WANT[i]}" || failed=1
    done

    if [[ $failed -eq 1 ]]; then
        echo -e "  ${YELLOW}Some runners could not be added${NC}"
        sleep 2
        tput civis 2>/dev/null || true
        M_MSG="${YELLOW}Some runners failed - counts below show what exists now. Press 's' to continue anyway.${NC}"
        menu_reload
        return 1
    fi
    echo -e "  ${GREEN}Done!${NC}"
    sleep 1
    return 0
}

render_target_menu() {
    local mode="$1" i name box tag total
    total=$(menu_total_want)

    echo -e "${BOLD}${CYAN}"
    echo "  ╔═══════════════════════════════════════════════════╗"
    echo "  ║            gh-runnermaxxer                        ║"
    echo -e "  ╚═══════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "  ${BOLD}Projects${NC}   ${DIM}runners: $total (max $MAX_RUNNERS)${NC}"
    echo ""

    if [[ ${#M_URLS[@]} -eq 0 ]]; then
        echo -e "  ${DIM}No projects yet. Press 'a' to add a repository or organization.${NC}"
    fi

    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        name=$(target_label "${M_URLS[i]}")
        [[ ${#name} -gt 40 ]] && name="${name:0:37}..."

        if [[ $i -eq $M_SEL ]]; then
            box=$(printf "${BOLD}${CYAN}◂ %2d ▸${NC}" "${M_WANT[i]}")
        else
            box=$(printf "  %2d  " "${M_WANT[i]}")
        fi

        tag=""
        [[ ${M_WANT[i]} -ne ${M_CUR[i]} ]] && tag="$tag  ${YELLOW}${M_CUR[i]} → ${M_WANT[i]}${NC}"
        [[ ${M_RUN[i]} -gt 0 ]] && tag="$tag  ${GREEN}${M_RUN[i]} running${NC}"
        [[ ${M_LISTED[i]} -eq 0 ]] && tag="$tag  ${DIM}(not in targets file)${NC}"
        tag="$tag$(autoscale_tag "${M_URLS[i]}")"

        if [[ $i -eq $M_SEL ]]; then
            printf "  ${CYAN}▸${NC} ${BOLD}%-40s${NC} [%s]%b\n" "$name" "$box" "$tag"
        elif [[ ${M_WANT[i]} -eq 0 ]]; then
            printf "    ${DIM}%-40s${NC} [%s]%b\n" "$name" "$box" "$tag"
        else
            printf "    %-40s [%s]%b\n" "$name" "$box" "$tag"
        fi
    done

    echo ""
    echo -e "  ${BOLD}────────────────────────────────────────────────────${NC}"
    echo -e "  ${CYAN}↑/↓${NC} choose project      ${CYAN}←/→${NC} runners (or ${CYAN}+/-${NC}, ${CYAN}0-9${NC})"
    local quit_label="back"
    [[ "$mode" == "startup" ]] && quit_label="quit"
    echo -e "  ${CYAN}a${NC} add project   ${CYAN}x${NC} remove from list   ${CYAN}Enter${NC}/${CYAN}s${NC} apply & continue   ${CYAN}q${NC} $quit_label"
    echo -e "  ${CYAN}b${NC} autoscale bounds ${DIM}(AUTOSCALE=$AUTOSCALE)${NC}"
    if [[ -n "$M_MSG" ]]; then
        echo ""
        echo -e "  $M_MSG"
    fi
    return 0
}

# One keypress, decoded: up/down/left/right/enter/space/esc/quit or the char.
# $1 (optional): seconds to wait for the first byte; prints "tick" on timeout.
read_key() {
    local k="" rest="" t=1 wait="${1:-}"
    # Fractional read timeouts need bash 4; on 3.2 a bare Esc takes 1s
    [[ "${BASH_VERSINFO[0]}" -ge 4 ]] && t=0.1
    if [[ -n "$wait" ]]; then
        IFS= read -rsn1 -t "$wait" k || { echo "tick"; return 0; }
    else
        IFS= read -rsn1 k || { echo "quit"; return 0; }
    fi
    if [[ "$k" == $'\x1b' ]]; then
        IFS= read -rsn2 -t "$t" rest || rest=""
        case "$rest" in
            '[A'|'OA') echo up ;;
            '[B'|'OB') echo down ;;
            '[C'|'OC') echo right ;;
            '[D'|'OD') echo left ;;
            *) echo esc ;;
        esac
        return 0
    fi
    case "$k" in
        "")  echo enter ;;
        " ") echo space ;;
        *)   echo "$k" ;;
    esac
    return 0
}

# ----------------------------------------------------------------------------
# Generic arrow-key picker
# ----------------------------------------------------------------------------
# Caller fills PICK_ITEMS (display text, may contain colour codes) and,
# optionally, PICK_HEADERS (a group header printed above item i, or "").
# pick_item "Title" "verb" then lets the user move with ↑/↓ (or j/k),
# jump with 1-9, confirm with Enter and cancel with q/Esc. On return
# PICK_INDEX holds the chosen index, or -1 when cancelled (exit 1).

PICK_ITEMS=(); PICK_HEADERS=(); PICK_INDEX=-1

render_pick_list() {
    local title="$1" verb="$2" i
    echo -e "${BOLD}${CYAN}"
    echo "  ╔═══════════════════════════════════════════════════╗"
    echo "  ║            gh-runnermaxxer                        ║"
    echo -e "  ╚═══════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "  ${BOLD}$title${NC}"
    [[ -z "${PICK_HEADERS[0]:-}" ]] && echo ""
    for ((i = 0; i < ${#PICK_ITEMS[@]}; i++)); do
        if [[ -n "${PICK_HEADERS[i]:-}" ]]; then
            echo ""
            echo -e "  ${BOLD}${PICK_HEADERS[i]}${NC}"
        fi
        if [[ $i -eq $PICK_INDEX ]]; then
            echo -e "  ${CYAN}▸${NC} ${PICK_ITEMS[i]}"
        else
            echo -e "    ${PICK_ITEMS[i]}"
        fi
    done
    echo ""
    echo -e "  ${BOLD}────────────────────────────────────────────────────${NC}"
    echo -e "  ${CYAN}↑/↓${NC} choose   ${CYAN}Enter${NC} $verb   ${CYAN}q${NC}/${CYAN}Esc${NC} cancel"
    return 0
}

pick_item() {
    local title="$1" verb="$2" n=${#PICK_ITEMS[@]} key
    PICK_INDEX=0
    [[ $n -eq 0 ]] && { PICK_INDEX=-1; return 1; }

    tput civis 2>/dev/null || true
    while true; do
        draw_frame "$(render_pick_list "$title" "$verb")"
        key=$(read_key)
        case "$key" in
            up|k|K)   [[ $PICK_INDEX -gt 0 ]] && PICK_INDEX=$((PICK_INDEX - 1)) ;;
            down|j|J) [[ $PICK_INDEX -lt $((n - 1)) ]] && PICK_INDEX=$((PICK_INDEX + 1)) ;;
            [1-9])    [[ $key -le $n ]] && PICK_INDEX=$((key - 1)) ;;
            enter|space)
                tput cnorm 2>/dev/null || true
                clear
                return 0
                ;;
            q|Q|esc|quit)
                tput cnorm 2>/dev/null || true
                PICK_INDEX=-1
                clear
                return 1
                ;;
        esac
    done
}

# Fill PICK_ITEMS/PICK_HEADERS with every runner, grouped by project, with
# its live status. PICK_IDS[i] is the runner ID behind row i.
PICK_IDS=()
build_runner_pick_list() {
    local t ids id header
    PICK_ITEMS=(); PICK_HEADERS=(); PICK_IDS=()
    pick_add_group() {
        # $1: header, rest: runner IDs
        header="$1"; shift
        for id in "$@"; do
            PICK_HEADERS[${#PICK_ITEMS[@]}]="$header"; header=""
            PICK_IDS[${#PICK_IDS[@]}]="$id"
            PICK_ITEMS[${#PICK_ITEMS[@]}]="$(render_runner_line "$id" | sed 's/^    //')"
        done
    }
    for t in $(known_targets); do
        ids=$(runner_ids_for_target "$t")
        [[ -n "$ids" ]] && pick_add_group "$(target_label "$t")" $ids
    done
    ids=$(unassigned_runner_ids)
    [[ -n "$ids" ]] && pick_add_group "${YELLOW}Unconfigured (incomplete setup)${NC}" $ids
    return 0
}

# $1: "startup" (q quits the program) or "tui" (q returns to the dashboard)
# $2: optional target URL to highlight initially
# Returns 0 after applying (or when nothing changed), 1 when backed out.
target_menu() {
    local mode="${1:-tui}" preselect="${2:-}" key n
    M_SEL=0; M_MSG=""; M_URLS=(); M_WANT=()
    menu_reload
    [[ -n "$preselect" ]] && menu_select_url "$preselect"

    tput civis 2>/dev/null || true
    while true; do
        n=${#M_URLS[@]}
        draw_frame "$(render_target_menu "$mode")"
        key=$(read_key)
        M_MSG=""
        case "$key" in
            up|k|K)   [[ $M_SEL -gt 0 ]] && M_SEL=$((M_SEL - 1)) ;;
            down|j|J) [[ $M_SEL -lt $((n - 1)) ]] && M_SEL=$((M_SEL + 1)) ;;
            right|+|=|l|L) menu_adjust 1 ;;
            left|-|_|h|H)  menu_adjust -1 ;;
            [0-9])     menu_set "$key" ;;
            a|A) menu_add_target ;;
            x|X) menu_remove_target ;;
            b|B) menu_set_bounds ;;
            enter|s|S)
                if apply_target_counts; then
                    tput cnorm 2>/dev/null || true
                    return 0
                fi
                ;;
            q|Q|quit)
                tput cnorm 2>/dev/null || true
                if [[ "$mode" == "startup" ]]; then
                    clear
                    exit 0
                fi
                return 1
                ;;
        esac
    done
}
# ============================================================================
# Locking / preflight
# ============================================================================

acquire_lock() {
    # Atomic create via noclobber closes the check-then-write race between
    # two simultaneous starts.
    if ( set -o noclobber; echo "$$" > "$LOCK_FILE" ) 2>/dev/null; then
        LOCK_ACQUIRED=1
        return 0
    fi

    local lock_pid
    lock_pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "")
    if [[ -n "$lock_pid" ]] && kill -0 "$lock_pid" 2>/dev/null; then
        return 1
    fi

    # Stale lock: holder is gone
    rm -f "$LOCK_FILE"
    if ( set -o noclobber; echo "$$" > "$LOCK_FILE" ) 2>/dev/null; then
        LOCK_ACQUIRED=1
        return 0
    fi
    return 1
}

preflight_checks() {
    local errors=0

    mkdir -p "$RUNNER_BASE_DIR" "$PID_DIR" "$LOG_DIR"

    # Required commands
    local cmd
    for cmd in "$GH_BIN" tar pgrep pkill ps; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            echo -e "${RED}✗ Missing required command: $cmd${NC}" >&2
            errors=$((errors + 1))
        fi
    done

    # gh authentication
    if command -v "$GH_BIN" >/dev/null 2>&1; then
        if ! "$GH_BIN" auth status >/dev/null 2>&1; then
            echo -e "${RED}✗ gh CLI is not authenticated. Run: gh auth login${NC}" >&2
            errors=$((errors + 1))
            # Recorded so a service install can tell "no token" apart
            # from other startup failures
            mkdir -p "$PID_DIR" 2>/dev/null || true
            echo auth > "$PID_DIR/gh.state" 2>/dev/null || true
            gh_set_last auth "" "gh auth status failed (not logged in, or the token is unreadable here)"
        else
            # Who, which scopes, and per-target permission. A missing scope
            # is recorded against the target (and shown), not fatal: other
            # targets may be fine and the token can be refreshed later.
            gh_auth_probe || true
        fi
    fi

    # Linux runner dependencies (the runner is dotnet-based)
    if [[ "$OS_FAMILY" == "linux" ]] && command -v ldconfig >/dev/null 2>&1; then
        if ! ldconfig -p 2>/dev/null | grep -q libicu; then
            warn "libicu not found - runners may fail to configure."
            echo -e "${DIM}  Fix after first setup: sudo ./runners/runner-1/bin/installdependencies.sh${NC}" >&2
        fi
    fi

    # Disk space
    local disk_mb
    disk_mb=$(free_disk_mb)
    if [[ "$disk_mb" -lt 1024 ]]; then
        warn "Low disk space: ${disk_mb}MB free at $RUNNER_BASE_DIR"
    fi

    # Runner tarball (offer auto-download when missing)
    RUNNER_TAR=$(detect_runner_tarball)
    if [[ -z "$RUNNER_TAR" ]]; then
        echo -e "${YELLOW}! No runner tarball found for ${RUNNER_OS}-${RUNNER_ARCH}${NC}"
        printf '  Download the latest release automatically? [Y/n]: '
        local dl_choice=""
        # Headless (--daemon): no one to ask, so download
        [[ "$DAEMON_MODE" == "1" ]] || read -r dl_choice || true
        if [[ ! "$dl_choice" =~ ^[Nn] ]]; then
            download_runner_tarball || true
            RUNNER_TAR=$(detect_runner_tarball)
        fi
    fi

    if [[ -z "$RUNNER_TAR" ]]; then
        echo -e "${RED}✗ No runner tarball found. Download from:${NC}" >&2
        echo -e "${DIM}  https://github.com/actions/runner/releases${NC}" >&2
        errors=$((errors + 1))
    elif ! tar -tzf "$RUNNER_TAR" >/dev/null 2>&1; then
        echo -e "${RED}✗ Runner tarball is corrupted: $(basename "$RUNNER_TAR")${NC}" >&2
        errors=$((errors + 1))
    else
        echo -e "${GREEN}✓${NC} Found runner: $(basename "$RUNNER_TAR")"
    fi

    if [[ $errors -gt 0 ]]; then
        die "Preflight checks failed ($errors error(s))"
    fi

    if ! acquire_lock; then
        local lock_pid
        lock_pid=$(daemon_pid) && die "$(daemon_running_msg "$lock_pid")"
        lock_pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "?")
        die "Another instance is running (PID $lock_pid). One instance manages every project - press 't' there to add repos/orgs."
    fi
}

# ============================================================================
# GitHub API access
# ============================================================================
# Every API call goes through gh_api, which classifies failures once:
#   GH_CLASS  ok | auth | sso | ratelimit | forbidden | notfound | network | error
#   GH_HTTP   HTTP status parsed from gh's "(HTTP NNN)" suffix, or empty
#   GH_MSG    first line of gh's stderr, without the "gh: " prefix
# gh_api sets these in the calling shell. A caller that captured the output
# with $(...) ran it in a subshell, so it calls gh_last_load afterwards to
# read them back ($$ is the same in subshells, so the per-process file
# .gh.last.$$ carries them across).
# Files in $PID_DIR:
#   gh.last   class<TAB>http<TAB>msg of the most recent call (any process)
#   gh.err    same, for the last *global* failure (auth/sso/ratelimit/
#             network/error); removed by the next successful call.
#             notfound/forbidden are per target (target-KEY.err) instead.
#   gh.state, gh.msg, gh.user, gh.host, gh.scopes, gh.checked
#             written by gh_auth_probe

# `"$GH_BIN" api ...` (not `command gh`) so a gh() function defined by the
# tests still intercepts the call. A service install bakes an absolute path
# in through RUNNERMAXXER_GH.
GH_BIN="${RUNNERMAXXER_GH:-gh}"
GH_CLASS=""; GH_HTTP=""; GH_MSG=""
GH_HDR_FILE=""

# gh_classify RC HTTP MSG - pure; sets GH_CLASS
gh_classify() {
    local rc=$1 http=$2 msg re
    msg=$(lower "$3")
    if [[ "$rc" == "0" ]]; then GH_CLASS=ok; return 0; fi
    re='not logged in|gh auth login|bad credentials|token.*expired|requires authentication'
    if [[ "$rc" == "4" || "$http" == "401" || "$msg" =~ $re ]]; then GH_CLASS=auth; return 0; fi
    re='rate limit'
    if [[ "$http" == "429" ]] || [[ "$http" == "403" && "$msg" =~ $re ]]; then GH_CLASS=ratelimit; return 0; fi
    re='saml'
    if [[ "$http" == "403" && "$msg" =~ $re ]]; then GH_CLASS=sso; return 0; fi
    case "$http" in
        403) GH_CLASS=forbidden; return 0 ;;
        404) GH_CLASS=notfound; return 0 ;;
    esac
    re='dial tcp|no such host|connection refused|timeout|timed out|tls|unreachable|network|error connecting'
    if [[ -z "$http" && "$msg" =~ $re ]]; then GH_CLASS=network; return 0; fi
    GH_CLASS=error
    return 0
}

# gh_set_last CLASS HTTP MSG - record a call result (see the comment above)
gh_set_last() {
    local line
    GH_CLASS=$1; GH_HTTP=$2; GH_MSG=$3
    line=$(printf '%s\t%s\t%s' "$GH_CLASS" "$GH_HTTP" "$GH_MSG")
    mkdir -p "$PID_DIR" 2>/dev/null || true
    printf '%s\n' "$line" > "$PID_DIR/.gh.last.$$" 2>/dev/null || true
    printf '%s\n' "$line" > "$PID_DIR/gh.last" 2>/dev/null || true
    case "$GH_CLASS" in
        ok) rm -f "$PID_DIR/gh.err" ;;
        auth|sso|ratelimit|network|error)
            printf '%s\t%s\n' "$line" "$(date +%s)" > "$PID_DIR/gh.err" 2>/dev/null || true ;;
    esac
    return 0
}

# Read back GH_CLASS/GH_HTTP/GH_MSG after a gh_api call made in a subshell
gh_last_load() {
    local line rest
    line=$(cat "$PID_DIR/.gh.last.$$" 2>/dev/null) || return 0
    GH_CLASS=${line%%$'\t'*}; rest=${line#*$'\t'}
    GH_HTTP=${rest%%$'\t'*}; GH_MSG=${rest#*$'\t'}
    return 0
}

# _gh_run ARGS... : run gh, pass stdout through, classify stderr
_gh_run() {
    local errf rc=0 http msg line
    mkdir -p "$PID_DIR" 2>/dev/null || true
    errf="$PID_DIR/.gh.stderr.$$.$RANDOM"
    [[ -d "$PID_DIR" ]] || errf="${TMPDIR:-/tmp}/runnermaxxer-gh.$$.$RANDOM"
    "$GH_BIN" "$@" 2>"$errf" || rc=$?
    http=$(sed -n 's/.*(HTTP \([0-9][0-9][0-9]\)).*/\1/p' "$errf" 2>/dev/null | head -1) || http=""
    msg=""
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line%$'\r'}"
        [[ -n "${line//[[:space:]]/}" ]] || continue
        msg="${line#gh: }"
        break
    done < "$errf"
    rm -f "$errf"
    msg="${msg:0:200}"
    gh_classify "$rc" "$http" "$msg"
    # SAML SSO: the authorize URL is only in a response header
    if [[ "$GH_CLASS" == "sso" && "$1" == "api" && "${2:-}" != "-i" ]]; then
        local sso
        shift
        sso=$({ "$GH_BIN" api -i "$@" 2>/dev/null || true; } | tr -d '\r' \
            | sed -n 's/^[Xx]-[Gg]it[Hh]ub-[Ss][Ss][Oo]:.*url=\([^ ;]*\).*/\1/p' | head -1) || sso=""
        [[ -n "$sso" ]] && msg="$msg - authorize: $sso"
    fi
    gh_set_last "$GH_CLASS" "$http" "$msg"
    return $rc
}

# gh_api ARGS... : `gh api ARGS`; exit status is gh's
gh_api() {
    _gh_run api "$@"
}

# gh_api_hdr ARGS... : `gh api -i ARGS`; the response headers go to
# $PID_DIR/.gh.hdr.$$ (read them with gh_header, also after a $(...)
# call), the body to stdout
gh_api_hdr() {
    local outf rc=0
    mkdir -p "$PID_DIR" 2>/dev/null || true
    GH_HDR_FILE="$PID_DIR/.gh.hdr.$$"
    outf="$PID_DIR/.gh.out.$$.$RANDOM"
    _gh_run api -i "$@" > "$outf" || rc=$?
    : > "$GH_HDR_FILE"
    # gh prints the status line and headers with CRLF, then a blank line
    tr -d '\r' < "$outf" | awk -v h="$GH_HDR_FILE" '
        hd == 0 && /^$/ { hd = 1; next }
        hd == 0         { print > h; next }
                        { print }'
    rm -f "$outf"
    return $rc
}

# gh_header NAME : value of one response header (case-insensitive) from the
# last gh_api_hdr call (or $GH_HDR_FILE when set); returns 1 when the
# header is absent
gh_header() {
    local v f="${GH_HDR_FILE:-$PID_DIR/.gh.hdr.$$}"
    [[ -f "$f" ]] || return 1
    v=$(tr -d '\r' < "$f" | awk -v n="$(lower "$1")" '
        { k = $0; sub(/:.*/, "", k) }
        tolower(k) == n { sub(/^[^:]*:[ \t]*/, ""); print; found = 1; exit }
        END { exit found ? 0 : 1 }') || return 1
    printf '%s\n' "$v"
}

# One line of human text per class, for lasterr, target errors and output
gh_error_note() {
    local class=$1 msg=${2:-} note
    case "$class" in
        ok)        note="ok" ;;
        auth)      note="gh is not logged in or its token was rejected - run: gh auth login" ;;
        sso)       note="the organization enforces SAML SSO - authorize the gh token for it" ;;
        ratelimit) note="GitHub API rate limit reached - retrying after it resets" ;;
        forbidden) note="GitHub refused access (403) - the token lacks admin rights or scopes" ;;
        notfound)  note="not found or no read access (404) - renamed, deleted, or access lost?" ;;
        network)   note="cannot reach GitHub (network)" ;;
        scope)     note="" ;;
        *)         note="gh failed" ;;
    esac
    if [[ -n "$msg" ]]; then
        if [[ -n "$note" ]]; then note="$note ($msg)"; else note="$msg"; fi
    fi
    [[ -n "$note" ]] || note="$class"
    printf '%s\n' "$note"
}

# ----------------------------------------------------------------------------
# Per-target errors: $PID_DIR/target-KEY.err = class<TAB>msg<TAB>since_epoch
# class: notfound | forbidden | sso | scope | ratelimit | network | error | auth
# ----------------------------------------------------------------------------

target_err_file() { echo "$PID_DIR/target-$(target_key "$1").err"; }

# target_set_error URL CLASS MSG - "since" is kept while the class is unchanged
target_set_error() {
    local f cls=$2 msg=${3:-} since old
    f=$(target_err_file "$1")
    msg=$(printf '%s' "$msg" | tr '\t\n\r' '   ')
    since=$(date +%s)
    old=$(cut -f1,3 "$f" 2>/dev/null) || old=""
    if [[ -n "$old" && "${old%%$'\t'*}" == "$cls" && "${old#*$'\t'}" =~ ^[0-9]+$ ]]; then
        since=${old#*$'\t'}
    fi
    mkdir -p "$PID_DIR" 2>/dev/null || true
    printf '%s\t%s\t%s\n' "$cls" "$msg" "$since" > "$f" 2>/dev/null || true
    return 0
}

# target_clear_error URL [CLASS] - with CLASS, only an error of that class
target_clear_error() {
    local f
    f=$(target_err_file "$1")
    [[ -f "$f" ]] || return 0
    if [[ -n "${2:-}" && "$(cut -f1 "$f" 2>/dev/null)" != "$2" ]]; then
        return 0
    fi
    rm -f "$f"
    return 0
}

# Clear a target's error unless it is of class CLASS
target_clear_error_except() {
    local f
    f=$(target_err_file "$1")
    [[ -f "$f" ]] || return 0
    [[ "$(cut -f1 "$f" 2>/dev/null)" == "$2" ]] && return 0
    rm -f "$f"
    return 0
}

# target_error URL : prints "class<TAB>msg", or nothing
target_error() {
    local f
    f=$(target_err_file "$1")
    [[ -f "$f" ]] || return 0
    cut -f1,2 "$f" 2>/dev/null || true
    return 0
}

# Human text for a target's stored error (empty when none)
target_error_note() {
    local e
    e=$(target_error "$1")
    [[ -n "$e" ]] || return 0
    gh_error_note "${e%%$'\t'*}" "${e#*$'\t'}"
}

# ----------------------------------------------------------------------------
# Registration tokens: short-lived (1 h), single-purpose, minted per runner
# right before config.sh, so the user's own token never reaches its argv
# ----------------------------------------------------------------------------

# _runner_token KIND URL - KIND is registration-token or remove-token
_runner_token() {
    local kind=$1 url=$2 tok rc=0 old
    tok=$(gh_api -X POST "$(target_api_endpoint "$url")/$kind" --jq .token) || rc=$?
    gh_last_load
    if [[ $rc -eq 0 && -z "$tok" ]]; then
        gh_set_last error "" "GitHub returned no $kind"
        rc=1
    fi
    if [[ $rc -ne 0 ]]; then
        # Keep the probe's clearer "scope" diagnosis over a bare 403
        old=$(target_error "$url"); old=${old%%$'\t'*}
        if [[ "$old" != "scope" || "$GH_CLASS" != "forbidden" ]]; then
            target_set_error "$url" "$GH_CLASS" "$GH_MSG"
        fi
        return 1
    fi
    target_clear_error "$url" scope
    printf '%s\n' "$tok"
}

registration_token() { _runner_token registration-token "$1"; }
remove_token()       { _runner_token remove-token "$1"; }

# ----------------------------------------------------------------------------
# Scope probe
# ----------------------------------------------------------------------------
# gh_auth_probe [quiet] - who is logged in, with which scopes, and can the
# token register runners for each known target? Writes gh.* files and per
# target "scope" errors; never exits. Classic tokens list their scopes in
# X-OAuth-Scopes: an org target needs admin:org, a repo target repo.
# Fine-grained PATs / app tokens send no such header, so for those a
# registration token is minted per target instead (403 = not allowed; the
# unused token expires on its own after an hour).
gh_auth_probe() {
    local quiet=${1:-} body rc=0 user scopes host t need has_hdr=1 now line note
    mkdir -p "$PID_DIR" 2>/dev/null || true
    now=$(date +%s)
    host=${GH_HOST:-github.com}
    echo "$host" > "$PID_DIR/gh.host"
    echo "$now" > "$PID_DIR/gh.checked"

    GH_HDR_FILE=""
    body=$(gh_api_hdr user) || rc=$?
    gh_last_load
    if [[ $rc -ne 0 ]]; then
        [[ "$GH_CLASS" == "ok" ]] && GH_CLASS=error
        echo "$GH_CLASS" > "$PID_DIR/gh.state"
        printf '%s\n' "$GH_MSG" > "$PID_DIR/gh.msg"
        [[ "$quiet" == "quiet" ]] || echo -e "${RED}✗ gh: $(gh_error_note "$GH_CLASS" "$GH_MSG")${NC}" >&2
        return 1
    fi
    user=$(printf '%s\n' "$body" | sed -n 's/.*"login": *"\([^"]*\)".*/\1/p' | head -1)
    scopes=$(gh_header X-OAuth-Scopes) || has_hdr=0
    scopes=$(printf '%s' "$scopes" | tr -d ' \t')
    printf '%s\n' "$user" > "$PID_DIR/gh.user"
    printf '%s\n' "$scopes" > "$PID_DIR/gh.scopes"
    echo ok > "$PID_DIR/gh.state"
    rm -f "$PID_DIR/gh.msg"

    for t in $(known_targets); do
        if [[ $has_hdr -eq 1 ]]; then
            need=repo
            [[ "$(target_type "$t")" == "org" ]] && need="admin:org"
            if [[ ",$scopes," == *",$need,"* ]]; then
                target_clear_error "$t" scope
            else
                target_set_error "$t" scope "token lacks the $need scope - run: gh auth refresh -h $host -s $need"
            fi
        elif registration_token "$t" >/dev/null; then
            target_clear_error "$t" scope
        else
            gh_last_load
            if [[ "$GH_CLASS" == "forbidden" ]]; then
                target_set_error "$t" scope "token cannot register runners here (403) - it needs admin rights (fine-grained: 'Administration' for a repo, 'Self-hosted runners' for an org)"
            fi
        fi
    done

    if [[ "$quiet" != "quiet" ]]; then
        if [[ $has_hdr -eq 1 ]]; then
            echo -e "${GREEN}✓${NC} gh: ${user:-?} (${scopes//,/, })"
        else
            echo -e "${GREEN}✓${NC} gh: ${user:-?} (no scope list - fine-grained or app token)"
        fi
        for t in $(known_targets); do
            line=$(target_error "$t")
            [[ -n "$line" ]] || continue
            note=$(gh_error_note "${line%%$'\t'*}" "${line#*$'\t'}")
            echo -e "${YELLOW}⚠ $(target_label "$t"): $note${NC}" >&2
        done
    fi
    return 0
}

# Current gh state for display: a sticky global error wins over the probe
gh_state() {
    local e s
    e=$(cut -f1 "$PID_DIR/gh.err" 2>/dev/null) || e=""
    if [[ -n "$e" ]]; then echo "$e"; return 0; fi
    s=$(cat "$PID_DIR/gh.state" 2>/dev/null) || s=""
    echo "${s:-unknown}"
}


# ============================================================================
# Runner state tracking
# ============================================================================
# Per-runner state files live in $PID_DIR:
#   runner-N.pid          PID of the runner's top-level process
#   runner-N.stopped      stopped on purpose - supervisor must not restart
#   runner-N.failcount    consecutive crash count (drives backoff/quarantine)
#   runner-N.laststart    epoch of last start attempt
#   runner-N.quarantined  crash-looped too many times; needs manual start
#   runner-N.lasterr      last failure reason (shown in UI)
#   runner-N.ghoffline    consecutive "GitHub sees it offline" check count
#   runner-N.target       target URL it was set up for (survives .runner deletion)
#   runner-N.ephemeral    registered with --ephemeral (re-registers after each job)

get_runner_ids() {
    local d
    for d in "$RUNNER_BASE_DIR"/runner-*; do
        [[ -d "$d" ]] || continue
        echo "${d##*/runner-}"
    done | sort -n
}

get_runner_count() {
    get_runner_ids | grep -c . || true
}

runner_procs() {
    # Every process belonging to runner N has $RUNNER_BASE_DIR/runner-N/ on
    # its command line (run.sh, run-helper.sh, Runner.Listener, Runner.Worker).
    pgrep -f "$RUNNER_BASE_DIR/runner-$1/" 2>/dev/null || true
}

is_running() {
    local id=$1
    local pid_file="$PID_DIR/runner-$id.pid"
    local pid
    [[ -f "$pid_file" ]] || return 1
    pid=$(cat "$pid_file" 2>/dev/null) || return 1
    [[ -n "$pid" ]] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    # Guard against PID reuse (e.g. stale .pid files after a reboot): the
    # process must actually be this runner, not whatever recycled the PID.
    ps -p "$pid" -o command= 2>/dev/null | grep -q "runner-$id/" || return 1
    return 0
}

# Desired-state tracking: a runner with a .stopped marker was stopped on
# purpose and must NOT be auto-restarted. Any other runner that isn't
# running is treated as crashed and gets brought back up by the supervisor.
mark_stopped() {
    touch "$PID_DIR/runner-$1.stopped"
}

unmark_stopped() {
    rm -f "$PID_DIR/runner-$1.stopped"
}

is_marked_stopped() {
    [[ -f "$PID_DIR/runner-$1.stopped" ]]
}

# Draining: remove the runner as soon as its current job finishes
mark_draining() {
    touch "$PID_DIR/runner-$1.draining"
    dlog "runner-$1: draining - removed when its job finishes"
}

unmark_draining() {
    rm -f "$PID_DIR/runner-$1.draining"
}

is_draining() {
    [[ -f "$PID_DIR/runner-$1.draining" ]]
}

is_quarantined() {
    [[ -f "$PID_DIR/runner-$1.quarantined" ]]
}

# Registration mode is fixed at setup time: changing EPHEMERAL_RUNNERS
# later does not affect runners that already exist.
is_ephemeral() {
    [[ -f "$PID_DIR/runner-$1.ephemeral" ]]
}

# An ephemeral runner that exited after finishing its one job: the runner
# deleted .runner itself, and the log's newest job-completed line is newer
# than our last re-registration attempt. That exit is normal, not a crash.
ephemeral_job_finished() {
    local id=$1 last
    is_ephemeral "$id" || return 1
    [[ -f "$RUNNER_BASE_DIR/runner-$id/.runner" ]] && return 1
    last=$(tail -200 "$LOG_DIR/runner-$id.log" 2>/dev/null \
        | grep -E 'completed with result|\[runnermaxxer\] re-registering' | tail -1) || last=""
    [[ "$last" == *"completed with result"* ]]
}

# The name a runner is actually registered under on GitHub. Read from its
# .runner state file so a later prefix change doesn't make us misidentify
# runners registered under the old prefix.
registered_name() {
    local id=$1 name
    name=$(sed -n 's/.*"agentName": *"\([^"]*\)".*/\1/p' "$RUNNER_BASE_DIR/runner-$id/.runner" 2>/dev/null | head -1)
    [[ -z "$name" ]] && name="${RUNNER_NAME_PREFIX}-${id}"
    echo "$name"
}

set_lasterr() {
    echo "$2" > "$PID_DIR/runner-$1.lasterr" 2>/dev/null || true
    dlog "runner-$1: $2"
}

# Event log for headless mode: one timestamped line per notable event
# (start/restart/quarantine/recycle/drain/remove) in $DAEMON_LOG. The TUI
# shows these on screen instead, so this is a no-op outside --daemon.
dlog() {
    [[ "$DAEMON_MODE" == "1" ]] || return 0
    printf '%s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >> "$DAEMON_LOG" 2>/dev/null || true
    return 0
}

get_lasterr() {
    cat "$PID_DIR/runner-$1.lasterr" 2>/dev/null || echo ""
}

# Give the runner a clean slate (used on manual start/restart)
clear_failure_state() {
    rm -f "$PID_DIR/runner-$1.failcount" \
          "$PID_DIR/runner-$1.quarantined" \
          "$PID_DIR/runner-$1.lasterr" \
          "$PID_DIR/runner-$1.ghoffline" \
          "$PID_DIR/runner-$1.ghbusy" \
          "$PID_DIR/runner-$1.ghseen"
}

clear_runner_state() {
    clear_failure_state "$1"
    rm -f "$PID_DIR/runner-$1.pid" \
          "$PID_DIR/runner-$1.stopped" \
          "$PID_DIR/runner-$1.draining" \
          "$PID_DIR/runner-$1.laststart" \
          "$PID_DIR/runner-$1.target" \
          "$PID_DIR/runner-$1.name" \
          "$PID_DIR/runner-$1.ephemeral" \
          "$PID_DIR/runner-$1.version"
}

# After a manager crash/restart, PID files may be stale while runner
# processes are still alive (they're detached). Adopt live orphans into
# fresh PID files and drop PID files whose process is gone.
reconcile_state() {
    local id pid real
    for id in $(get_runner_ids); do
        if is_running "$id"; then
            continue
        fi
        real=$(runner_main_pid "$id") || real=""
        if [[ -n "$real" ]]; then
            echo "$real" > "$PID_DIR/runner-$id.pid"
        else
            rm -f "$PID_DIR/runner-$id.pid"
        fi
    done
    return 0
}

count_running() {
    local count=0 id
    for id in $(get_runner_ids); do
        is_running "$id" && count=$((count + 1))
    done
    echo $count
}

# Display status. The log-derived text carries the job name, but when fresh
# GitHub API data is available for this runner and disagrees, the API wins.
get_runner_status() {
    local id=$1 st
    st=$(log_runner_status "$id")
    # GitHub only ever upgrades the display toward busy; a log that says
    # running is trusted even when the (possibly stale) flag disagrees.
    if gh_known "$id" && [[ -f "$PID_DIR/runner-$id.ghbusy" ]]; then
        [[ "$st" == running:* ]] || st="busy (per GitHub)"
    fi
    echo "$st"
}

# GitHub data is fresh when the last successful poll is younger than about
# two poll intervals (min 30s). Polling disabled -> never fresh.
gh_data_fresh() {
    [[ "$GH_HEALTH_TICKS" -gt 0 ]] || return 1
    local ts max now
    ts=$(cat "$PID_DIR/gh-poll.ts" 2>/dev/null || echo "")
    [[ "$ts" =~ ^[0-9]+$ ]] || return 1
    # Two poll intervals (the interval grows with the target count)
    max=$(( $(effective_health_ticks "${HEALTH_N_TARGETS:-0}") * REFRESH_INTERVAL * 2 ))
    [[ $max -lt 30 ]] && max=30
    now=$(date +%s)
    [[ $((now - ts)) -le $max ]]
}

# Fresh GitHub data exists and this runner appeared in the last poll
gh_known() {
    [[ -f "$PID_DIR/runner-$1.ghseen" ]] && gh_data_fresh
}

# Status derived purely from the runner's own log (lifecycle lines)
log_runner_status() {
    local id=$1
    local log_file="$LOG_DIR/runner-$id.log"

    [[ ! -f "$log_file" ]] && { echo "no logs"; return 0; }

    local recent
    recent=$(tail -50 "$log_file" 2>/dev/null || echo "")
    [[ -z "$recent" ]] && { echo "no logs"; return 0; }

    # Only the most recent lifecycle event counts. The runner does not log
    # "Listening for Jobs" again after a job finishes, so an older
    # "Running job:" line must never outrank a later "completed" line.
    local last
    last=$(echo "$recent" \
        | grep -E 'Running job:|Job .* completed with result|Listening for Jobs|Could not connect|Authentication failed|Starting Runner listener|Exiting runner' \
        | tail -1)

    case "$last" in
        *"Running job:"*)
            local job_name ts start_epoch now_epoch elapsed dur suffix
            _parse_job_line "$last"
            job_name=$(printf '%s' "$JOB_NAME" | cut -c1-25)
            suffix=""
            start_epoch=$JOB_START
            if [[ -n "$start_epoch" ]]; then
                now_epoch=$(date -u +%s 2>/dev/null || echo "")
                if [[ "$now_epoch" =~ ^[0-9]+$ ]]; then
                    elapsed=$(( now_epoch - start_epoch ))
                    [[ $elapsed -ge 0 ]] || elapsed=0
                    if dur=$(format_duration "$elapsed" 2>/dev/null) && [[ -n "$dur" ]]; then
                        suffix=" ($dur)"
                    fi
                fi
            fi
            echo "running: $job_name$suffix"
            ;;
        *"Listening for Jobs"*)          echo "idle" ;;
        *"completed with result: "*)     echo "idle (last: ${last##*completed with result: })" ;;
        *"Could not connect"*)           echo "connection error" ;;
        *"Authentication failed"*)       echo "auth error" ;;
        *"Starting Runner listener"*)    echo "starting..." ;;
        *"Exiting runner"*)              echo "exiting" ;;
        *)                               echo "unknown" ;;
    esac
}

# _parse_job_line LINE - from a "Running job:" log line, set JOB_NAME and
# JOB_START (epoch, or empty when the timestamp can't be parsed)
JOB_NAME=""; JOB_START=""
_parse_job_line() {
    local ts
    JOB_NAME=$(printf '%s\n' "$1" | sed 's/.*Running job: //')
    JOB_START=""
    ts=$(printf '%s\n' "$1" | sed -n 's/^\([0-9][0-9-]* [0-9:]*Z\):.*/\1/p')
    if [[ -n "$ts" ]]; then
        JOB_START=$(log_ts_to_epoch "$ts" 2>/dev/null) || JOB_START=""
    fi
    return 0
}

# log_last_job ID - "name<TAB>start_epoch" of the job the runner is on,
# per its log (same "last lifecycle line wins" rule as log_runner_status);
# nothing when it is not running one
log_last_job() {
    local last
    last=$(tail -50 "$LOG_DIR/runner-$1.log" 2>/dev/null \
        | grep -E 'Running job:|Job .* completed with result|Listening for Jobs|Could not connect|Authentication failed|Starting Runner listener|Exiting runner' \
        | tail -1) || last=""
    [[ "$last" == *"Running job:"* ]] || return 0
    _parse_job_line "$last"
    printf '%s\t%s\n' "$JOB_NAME" "$JOB_START"
}

is_busy() {
    is_running "$1" || return 1
    # Either source saying busy counts. The GitHub flag can be up to a poll
    # interval stale, so a job that started since the last poll is only
    # visible in the log; trusting the flag alone would let a scale-down
    # kill that job.
    if gh_known "$1" && [[ -f "$PID_DIR/runner-$1.ghbusy" ]]; then
        return 0
    fi
    [[ "$(log_runner_status "$1")" == running:* ]]
}

# Why did the runner's run.sh exit? run.sh itself exits 0 after the listener
# ends for good (terminated, session conflict, unknown code), so the exit
# status says nothing; its run-helper logs the reason. Prints
# "class<TAB>detail" when the newest relevant log line is an exit reason:
#   conflict    another process holds this registration (exit 5)
#   deprecated  runner too old for GitHub (exit 7)
#   terminated  listener exited with 1
#   unknown     unknown exit code (detail = the code)
#   creds       the listener's credentials were rejected (U9)
# A lifecycle line after it (the runner was relaunched and got going)
# outranks it, as does a retry/update line (run.sh loops on those).
runner_exit_reason() {
    local last
    last=$(tail -40 "$LOG_DIR/runner-$1.log" 2>/dev/null \
        | grep -E 'Session Conflict|deprecated version exit code|exit with terminated error|unknown error code|Authentication failed|[Cc]redentials|retryable error|because of updating|Update finished|Listening for Jobs|Starting Runner listener|Running job:|completed with result' \
        | tail -1) || last=""
    case "$last" in
        *"Authentication failed"*|*[Cc]redentials*) printf 'creds\t\n' ;;
        *"Session Conflict"*)              printf 'conflict\t\n' ;;
        *"deprecated version exit code"*)  printf 'deprecated\t\n' ;;
        *"exit with terminated error"*)    printf 'terminated\t\n' ;;
        *"unknown error code"*)            printf 'unknown\t%s\n' "$(printf '%s' "${last##*code: }" | tr -cd '0-9')" ;;
    esac
    return 0
}

# lasterr text for a runner_exit_reason line
runner_exit_note() {
    case "${1%%$'\t'*}" in
        conflict)   echo "session conflict: another process holds this runner's registration (cloned dir? second manager?) - remove and re-add" ;;
        deprecated) echo "runner version too old for GitHub - run --download, then remove and re-add" ;;
        terminated) echo "listener exited (terminated, exit 1) - see log" ;;
        unknown)    echo "listener exited with code ${1#*$'\t'} - see log" ;;
        creds)      echo "runner credentials rejected - remove and re-add" ;;
    esac
}

# The runner's version: from the bin -> bin.X.Y.Z symlink a self-updated
# runner leaves behind (always current, no process start), else from
# runner-N.version (written at setup), else by asking Runner.Listener once
# (cached). Prints nothing when unknown.
runner_version() {
    local id=$1 dir="$RUNNER_BASE_DIR/runner-$1" t v
    if [[ -L "$dir/bin" ]]; then
        t=$(readlink "$dir/bin" 2>/dev/null) || t=""
        t=${t%/}; t=${t##*/}
        if [[ "$t" == bin.* && -n "${t#bin.}" ]]; then
            echo "${t#bin.}"
            return 0
        fi
    fi
    v=$(cat "$PID_DIR/runner-$id.version" 2>/dev/null) || v=""
    if [[ -z "$v" && -x "$dir/bin/Runner.Listener" ]]; then
        v=$("$dir/bin/Runner.Listener" --version 2>/dev/null | head -1 | tr -d '[:space:]') || v=""
        [[ "$v" =~ ^[0-9][0-9.]*$ ]] || v=""
        [[ -n "$v" ]] && echo "$v" > "$PID_DIR/runner-$id.version" 2>/dev/null
    fi
    [[ -n "$v" ]] && echo "$v"
    return 0
}

# PID to record for a runner's live process family: the root of the tree
# (a process whose parent is not itself a runner process), preferring
# run.sh. The first pgrep match can be Runner.Listener, whose PID changes
# on every self-update.
runner_main_pid() {
    local pids p pp cmd best=""
    pids=$(runner_procs "$1" | tr '\n' ' ')
    pids=${pids% }
    [[ -n "$pids" ]] || return 1
    for p in $pids; do
        pp=$(ps -p "$p" -o ppid= 2>/dev/null | tr -d ' ') || pp=""
        case " $pids " in *" $pp "*) continue ;; esac
        cmd=$(ps -p "$p" -o command= 2>/dev/null) || cmd=""
        if [[ "$cmd" == *run.sh* ]]; then
            echo "$p"
            return 0
        fi
        [[ -n "$best" ]] || best=$p
    done
    [[ -n "$best" ]] || best=${pids%% *}
    echo "$best"
}

# ============================================================================
# Log rotation
# ============================================================================

rotate_log() {
    local f="$LOG_DIR/runner-$1.log"
    [[ -f "$f" ]] || return 0
    local max_bytes=$(( MAX_LOG_SIZE_MB * 1024 * 1024 ))
    # GNU stat first: on GNU, `stat -f%z` is filesystem mode and exits 0
    # with junk output, so it must not come first; macOS stat rejects -c
    # with a nonzero exit, so this chain works on both.
    local sz
    sz=$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f" 2>/dev/null || echo 0)
    [[ "$sz" =~ ^[0-9]+$ ]] || sz=0
    if [[ "$sz" -gt "$max_bytes" ]]; then
        cp "$f" "$f.1" 2>/dev/null || true
        # Truncate in place: the runner holds the fd in append mode, so
        # writing continues correctly; mv alone wouldn't free space.
        : > "$f"
    fi
    return 0
}

# ============================================================================
# Supervisor (self-healing)
# ============================================================================

# Restart any runner that died unexpectedly, with exponential backoff.
# A runner that keeps dying quickly is quarantined after
# MAX_RESTART_ATTEMPTS so it can't crash-loop forever; manual start ('s',
# 'r', '+') clears the quarantine and tries again.
supervise_runners() {
    local now id
    now=$(date +%s)

    for id in $(get_runner_ids); do
        rotate_log "$id"
        # Draining: remove once idle or exited; never restart it
        if is_draining "$id"; then
            if ! is_busy "$id"; then
                dlog "runner-$id: drained (no job running) - removing"
                remove_runner "$id" >/dev/null 2>&1 || true
                unmark_draining "$id"
            fi
            continue
        fi
        is_marked_stopped "$id" && continue

        if is_running "$id"; then
            # Healthy for a while → forget past crashes
            local started
            started=$(cat "$PID_DIR/runner-$id.laststart" 2>/dev/null || echo 0)
            [[ "$started" =~ ^[0-9]+$ ]] || started=0
            if [[ $((now - started)) -ge 60 ]]; then
                rm -f "$PID_DIR/runner-$id.failcount"
            fi
            continue
        fi

        is_quarantined "$id" && continue

        # Ephemeral runner done with its job: re-register and relaunch now,
        # without counting it as a crash or applying backoff.
        if ephemeral_job_finished "$id"; then
            rm -f "$PID_DIR/runner-$id.failcount"
            dlog "runner-$id: ephemeral job finished - re-registering"
            start_runner "$id" >/dev/null 2>&1 || true
            continue
        fi

        # The run-helper logged why the listener stopped. A session conflict
        # or a deprecated version won't fix itself by restarting: quarantine
        # at once with the real reason instead of after five crash rounds.
        local reason
        reason=$(runner_exit_reason "$id")
        case "${reason%%$'\t'*}" in
            conflict|deprecated)
                touch "$PID_DIR/runner-$id.quarantined"
                set_lasterr "$id" "$(runner_exit_note "$reason") - quarantined"
                continue
                ;;
        esac

        local fails last delay
        fails=$(cat "$PID_DIR/runner-$id.failcount" 2>/dev/null || echo 0)
        [[ "$fails" =~ ^[0-9]+$ ]] || fails=0

        if [[ "$fails" -ge "$MAX_RESTART_ATTEMPTS" ]]; then
            touch "$PID_DIR/runner-$id.quarantined"
            set_lasterr "$id" "crash-looped ${fails}x - quarantined (press 's' to retry)"
            continue
        fi

        last=$(cat "$PID_DIR/runner-$id.laststart" 2>/dev/null || echo 0)
        [[ "$last" =~ ^[0-9]+$ ]] || last=0
        delay=$(( 5 * (2 ** fails) ))    # 5s, 10s, 20s, 40s, 80s
        [[ $((now - last)) -lt $delay ]] && continue

        # Count the attempt regardless of outcome; only sustained uptime
        # (>=60s above) clears it. start_runner returning 0 after surviving
        # one second says nothing about a runner that crashes at t+2s.
        echo $((fails + 1)) > "$PID_DIR/runner-$id.failcount"
        # Terminated / unknown exit code: worth a retry, but say why it died
        [[ -n "$reason" ]] && set_lasterr "$id" "$(runner_exit_note "$reason")"
        dlog "runner-$id: not running - restart attempt $((fails + 1))"
        start_runner "$id" >/dev/null 2>&1 || true
    done
    return 0
}

# ---- Rate limit (A5) --------------------------------------------------------
# Poll interval in ticks, scaled with the number of targets that have
# runners so the poll load stays roughly constant (~1 request per target
# per poll): GH_HEALTH_TICKS * max(1, ceil(N/10)). Pure.
effective_health_ticks() {
    local n=${1:-0} f
    f=$(( (n + 9) / 10 ))
    [[ $f -ge 1 ]] || f=1
    echo $(( GH_HEALTH_TICKS * f ))
}
HEALTH_N_TARGETS=0

# gh_rate_ok NEED - 0 when a poll that needs about NEED requests may run.
# Reads /rate_limit (free: it doesn't count against the limit) into
# gh.rate (remaining<TAB>reset<TAB>checked). While the limit is exhausted
# the ratelimit state is kept until the reset time without calling gh.
gh_rate_ok() {
    local need=${1:-1} now out rem reset f="$PID_DIR/gh.rate"
    now=$(date +%s)
    if [[ "$(gh_state)" == "ratelimit" && -f "$f" ]]; then
        reset=$(cut -f2 "$f" 2>/dev/null) || reset=""
        [[ "$reset" =~ ^[0-9]+$ && $now -lt $reset ]] && return 1
    fi
    if ! out=$(gh_api rate_limit --jq '.resources.core | "\(.remaining)\t\(.reset)"'); then
        gh_last_load
        # Rate-limited or offline: skip this poll. Anything else (a
        # rejected token, no such endpoint on some GHES setups) goes on to
        # the normal poll, which re-probes and records per-target errors.
        case "$GH_CLASS" in ratelimit|network) return 1 ;; esac
        return 0
    fi
    rem=${out%%$'\t'*}; reset=${out#*$'\t'}
    [[ "$rem" =~ ^[0-9]+$ && "$reset" =~ ^[0-9]+$ ]] || return 0
    printf '%s\t%s\t%s\n' "$rem" "$reset" "$now" > "$f" 2>/dev/null || true
    if [[ $rem -lt $((need + 20)) ]]; then
        local at
        at=$(date -r "$reset" '+%H:%M' 2>/dev/null || date -d "@$reset" '+%H:%M' 2>/dev/null || echo "$reset")
        gh_set_last ratelimit "" "rate limit exhausted ($rem left), resumes $at"
        dlog "GitHub rate limit low ($rem left) - polling paused until $at (runners keep running)"
        return 1
    fi
    return 0
}

# ---- Sleep/wake guard (S1) ----------------------------------------------------
# A wall-clock jump between ticks means the machine slept (or the process
# was stopped). After waking, runners reconnect slowly and GitHub shows
# them offline for a while, so recycling them would only add churn.
note_tick_time() {
    local now prev
    now=$(date +%s)
    prev=$(cat "$PID_DIR/tick.ts" 2>/dev/null) || prev=""
    echo "$now" > "$PID_DIR/tick.ts" 2>/dev/null || true
    [[ "$prev" =~ ^[0-9]+$ ]] || return 0
    if [[ $((now - prev)) -gt $((3 * REFRESH_INTERVAL)) ]]; then
        echo "$now" > "$PID_DIR/wake.ts" 2>/dev/null || true
        dlog "clock jumped $((now - prev))s (sleep/wake?) - GitHub health recycles paused for 2 min"
    fi
    return 0
}

# True within 120 s of a detected wake
recently_woke() {
    local w
    w=$(cat "$PID_DIR/wake.ts" 2>/dev/null) || return 1
    [[ "$w" =~ ^[0-9]+$ ]] || return 1
    [[ $(( $(date +%s) - w )) -lt 120 ]]
}

# Cross-check local state against GitHub: a runner process can be alive
# while GitHub considers it offline (wedged listener, revoked credentials,
# network partition that never recovered). Two consecutive offline sightings
# → recycle the process. Skipped entirely when the API is unreachable so a
# GitHub/network outage doesn't trigger mass restarts.
check_github_health() {
    local t endpoint rows online id name gbusy cnt now started polled=0 n=0 woke=0
    now=$(date +%s)
    mkdir -p "$PID_DIR" 2>/dev/null || true

    for t in $(known_targets); do
        [[ -n "$(runner_ids_for_target "$t")" ]] && n=$((n + 1))
    done
    HEALTH_N_TARGETS=$n
    # Budget: one listing per target, plus the label lookups autoscale may
    # make (up to 11 per repo). Paused polls leave runners and their
    # markers alone; the markers expire by themselves (gh_data_fresh).
    gh_rate_ok $(( n * (1 + AUTOSCALE * 11) )) || return 0

    # A broken token is re-probed once per poll, so the state recovers by
    # itself after `gh auth login` (the probe is skipped while things work)
    [[ "$(gh_state)" == "ok" ]] || gh_auth_probe quiet || true
    recently_woke && woke=1

    for t in $(known_targets); do
        [[ -n "$(runner_ids_for_target "$t")" ]] || continue
        endpoint=$(target_api_endpoint "$t")
        # One pass: name<TAB>status<TAB>busy per registered runner
        if ! rows=$(gh_api --paginate "$endpoint" --jq '.runners[] | "\(.name)\t\(.status)\t\(.busy)"'); then
            gh_last_load
            target_set_error "$t" "$GH_CLASS" "$GH_MSG"
            # No API view for this target: drop its markers so is_busy and
            # the status column fall back to the logs
            for id in $(runner_ids_for_target "$t"); do
                rm -f "$PID_DIR/runner-$id.ghseen" "$PID_DIR/runner-$id.ghbusy"
            done
            continue
        fi
        # Listing works; a scope error stays until the probe or a
        # registration clears it
        target_clear_error_except "$t" scope
        polled=1
        online=$(printf '%s\n' "$rows" | awk -F'\t' '$2 == "online" { print $1 }')

        for id in $(runner_ids_for_target "$t"); do
            local off_file="$PID_DIR/runner-$id.ghoffline"
            if ! is_running "$id"; then
                rm -f "$off_file" "$PID_DIR/runner-$id.ghseen" "$PID_DIR/runner-$id.ghbusy"
                continue
            fi
            # A draining runner is on its way out; never recycle it
            is_draining "$id" && { rm -f "$off_file"; continue; }

            name=$(registered_name "$id")
            gbusy=$(printf '%s\n' "$rows" | awk -F'\t' -v n="$name" '$1 == n { print $3; exit }')
            if [[ -n "$gbusy" ]]; then
                touch "$PID_DIR/runner-$id.ghseen"
                if [[ "$gbusy" == "true" ]]; then
                    touch "$PID_DIR/runner-$id.ghbusy"
                else
                    rm -f "$PID_DIR/runner-$id.ghbusy"
                fi
            else
                rm -f "$PID_DIR/runner-$id.ghseen" "$PID_DIR/runner-$id.ghbusy"
            fi

            # Grace period: a freshly started runner may not show online yet
            started=$(cat "$PID_DIR/runner-$id.laststart" 2>/dev/null || echo 0)
            [[ "$started" =~ ^[0-9]+$ ]] || started=0
            [[ $((now - started)) -lt 120 ]] && continue
            # Never recycle right after a wake, or a runner that is running
            # a job by either account (the job would be killed)
            [[ $woke -eq 1 ]] && continue
            [[ "$gbusy" == "true" ]] && continue
            [[ "$(log_runner_status "$id")" == running:* ]] && continue

            if printf '%s\n' "$online" | grep -qFx "$name"; then
                rm -f "$off_file"
            else
                cnt=$(cat "$off_file" 2>/dev/null || echo 0)
                [[ "$cnt" =~ ^[0-9]+$ ]] || cnt=0
                cnt=$((cnt + 1))
                if [[ "$cnt" -ge 2 ]]; then
                    set_lasterr "$id" "GitHub reported offline - recycled"
                    stop_runner_procs "$id"
                    start_runner "$id" >/dev/null 2>&1 || true
                    rm -f "$off_file"
                else
                    echo "$cnt" > "$off_file"
                fi
            fi
        done
    done
    [[ $polled -eq 1 ]] && echo "$now" > "$PID_DIR/gh-poll.ts"
    # Fresh API data just arrived: let autoscaling act on it
    autoscale_tick || true
    return 0
}

# ----------------------------------------------------------------------------
# Autoscaling (AUTOSCALE=1, projects with min=/max= bounds only)
# ----------------------------------------------------------------------------

# Filename-safe key for a target: lowercased owner/repo with '/' -> '+'
# ('+' can't appear in GitHub names, so keys never collide)
target_key() {
    local p
    p=$(lower "${1#https://github.com/}")
    echo "${p//\//+}"
}

# Last queue depth seen for a target (cached by autoscale_tick), or empty
cached_queue_depth() {
    local q
    q=$(cut -f1 "$PID_DIR/queue-$(target_key "$1").txt" 2>/dev/null) || q=""
    [[ "$q" =~ ^[0-9]+$ ]] && echo "$q"
    return 0
}

# Last count of queued jobs no runner here could take (labels), or empty
cached_queue_unsatisfiable() {
    local q
    q=$(cut -s -f2 "$PID_DIR/queue-$(target_key "$1").txt" 2>/dev/null) || q=""
    [[ "$q" =~ ^[0-9]+$ ]] && echo "$q"
    return 0
}

# Pure scaling decision. Prints the desired runner count.
#   autoscale_decide CUR BUSY QUEUED MIN MAX IDLE_MINUTES_ELAPSED [CAP]
# CAP is the most this target may have without the sum over all targets
# exceeding MAX_RUNNERS (omit for no global cap). Rules:
#   - queued jobs and no idle runner: grow by QUEUED
#   - an idle runner for >= AUTOSCALE_IDLE_MINUTES and CUR > MIN: shrink by 1
#   - always clamped to MIN..MAX; growth is also clamped to CAP (but CAP
#     never forces a shrink)
autoscale_decide() {
    local cur=$1 busy=$2 queued=$3 mn=$4 mx=$5 elapsed=$6 cap=${7:-} idle want
    local threshold=$AUTOSCALE_IDLE_MINUTES
    [[ "$threshold" =~ ^[0-9]+$ ]] || threshold=10
    idle=$((cur - busy))
    [[ $idle -lt 0 ]] && idle=0
    want=$cur
    if [[ $queued -gt 0 && $idle -eq 0 ]]; then
        want=$((cur + queued))
    elif [[ $idle -gt 0 && $cur -gt $mn && $elapsed -ge $threshold ]]; then
        want=$((cur - 1))
    fi
    [[ $want -gt $mx ]] && want=$mx
    [[ $want -lt $mn ]] && want=$mn
    if [[ -n "$cap" && $want -gt $cur && $want -gt $cap ]]; then
        want=$cap
        [[ $want -lt $cur ]] && want=$cur
    fi
    echo "$want"
}

# Queue depth for a target. Repos: queued workflow runs (one API call).
# Orgs: listing every repo's runs is too expensive, so the signal is "all
# of our runners are busy" (= 1). Returns 1 when the API call fails.
# Labels a job's runs-on may name and this host's runners carry: the
# implicit self-hosted / OS / arch labels plus the detected ones. Lowercase,
# space-separated.
fleet_labels() {
    local l os arch
    l=${CACHED_LABELS:-}
    [[ -n "$l" ]] || l=$(detect_labels 2>/dev/null || true)
    case "$RUNNER_OS" in osx) os=macos ;; *) os=${RUNNER_OS:-} ;; esac
    arch=${RUNNER_ARCH:-}
    lower "self-hosted $os $arch ${l//,/ }"
}

# label_satisfiable "a,b,c" "fleet labels" - pure: every label of the job
# is one the fleet has (case-insensitive)
label_satisfiable() {
    local want rest lab
    want=$(lower "$1")
    rest=$want
    while [[ -n "$rest" ]]; do
        lab=${rest%%,*}
        if [[ "$rest" == *,* ]]; then rest=${rest#*,}; else rest=""; fi
        lab=${lab# }; lab=${lab% }
        [[ -n "$lab" ]] || continue
        case " $2 " in *" $lab "*) ;; *) return 1 ;; esac
    done
    return 0
}

# target_queue_depth URL CUR BUSY - queued jobs this host could run.
# Repos: the queued runs' queued jobs whose labels the fleet satisfies
# (S5); the rest are counted as unsatisfiable into $PID_DIR/.qunsat.$$
# for the caller. Orgs: no queue API for org runners, so "all busy" = 1.
target_queue_depth() {
    local url=$1 cur=$2 busy=$3 p ids id jobs line sat=0 unsat=0 n=0 fleet
    rm -f "$PID_DIR/.qunsat.$$"
    if [[ "$(target_type "$url")" == "repo" ]]; then
        p=${url#https://github.com/}
        ids=$(gh_api "repos/$p/actions/runs?status=queued&per_page=20" \
            --jq '.workflow_runs[].id') || { gh_last_load; target_set_error "$url" "$GH_CLASS" "$GH_MSG"; return 1; }
        fleet=$(fleet_labels)
        for id in $ids; do
            [[ "$id" =~ ^[0-9]+$ ]] || continue
            n=$((n + 1))
            [[ $n -le 10 ]] || break
            jobs=$(gh_api "repos/$p/actions/runs/$id/jobs" \
                --jq '.jobs[] | select(.status=="queued") | (.labels | join(","))') \
                || { gh_last_load; target_set_error "$url" "$GH_CLASS" "$GH_MSG"; return 1; }
            while IFS= read -r line; do
                [[ -n "$line" ]] || continue
                if label_satisfiable "$line" "$fleet"; then sat=$((sat + 1)); else unsat=$((unsat + 1)); fi
            done <<< "$jobs"
        done
        echo "$unsat" > "$PID_DIR/.qunsat.$$" 2>/dev/null || true
        echo "$sat"
    elif [[ $cur -gt 0 && $busy -ge $cur ]]; then
        echo 1
    else
        echo 0
    fi
    return 0
}

# One autoscaling pass over every project with bounds. Prints nothing (the
# caller may be the TUI or a headless daemon); the last action is written
# to $PID_DIR/autoscale.last and passed to dlog when that function exists.
autoscale_tick() {
    [[ "$AUTOSCALE" == "1" ]] || return 0
    local t b mn mx cur busy queued id key idle since now elapsed want
    local total=0 cap maxr i skip new reason note rc
    maxr=$MAX_RUNNERS
    [[ "$maxr" =~ ^[0-9]+$ ]] || maxr=20
    now=$(date +%s)
    mkdir -p "$PID_DIR"

    for t in $(known_targets); do
        total=$((total + $(count_runners_for_target "$t")))
    done

    for t in $(known_targets); do
        b=$(target_bounds "$t")
        [[ -n "$b" ]] || continue
        mn=${b% *}; mx=${b#* }

        # Never override a count the user has changed but not applied yet
        skip=0
        for ((i = 0; i < ${#M_URLS[@]}; i++)); do
            if same_target "${M_URLS[i]}" "$t" && [[ "${M_WANT[i]:-}" != "${M_CUR[i]:-}" ]]; then
                skip=1
            fi
        done
        [[ $skip -eq 1 ]] && continue

        cur=$(count_runners_for_target "$t")
        # Busy flags must come from this poll (a project with no runners has
        # none to report, so it can still be brought up to its minimum)
        [[ $cur -eq 0 ]] || gh_data_fresh || continue
        # Only a running, non-busy runner is idle. Quarantined, stopped or
        # between-jobs runners are neither idle nor busy: fold them into
        # "busy" so they never block growth or trigger a shrink.
        idle=0
        for id in $(runner_ids_for_target "$t"); do
            is_draining "$id" && continue
            is_running "$id" || continue
            is_busy "$id" || idle=$((idle + 1))
        done
        busy=$((cur - idle))
        [[ $busy -lt 0 ]] && busy=0

        key=$(target_key "$t")
        queued=$(target_queue_depth "$t" "$cur" "$busy") || continue
        # satisfiable<TAB>unsatisfiable (the second empty for orgs)
        printf '%s\t%s\n' "$queued" "$(cat "$PID_DIR/.qunsat.$$" 2>/dev/null || true)" > "$PID_DIR/queue-$key.txt"
        rm -f "$PID_DIR/.qunsat.$$"

        idle=$((cur - busy))
        if [[ $idle -gt 0 ]]; then
            since=$(cat "$PID_DIR/idle-since-$key" 2>/dev/null) || since=""
            if [[ ! "$since" =~ ^[0-9]+$ ]]; then
                since=$now
                echo "$now" > "$PID_DIR/idle-since-$key"
            fi
            elapsed=$(( (now - since) / 60 ))
        else
            rm -f "$PID_DIR/idle-since-$key"
            elapsed=0
        fi

        cap=$((maxr - (total - cur)))
        want=$(autoscale_decide "$cur" "$busy" "$queued" "$mn" "$mx" "$elapsed" "$cap")
        [[ $want -eq $cur ]] && continue

        if [[ $want -gt $cur && $queued -gt 0 && $idle -eq 0 && $cur -ge $mn ]]; then
            reason="$queued queued"
        elif [[ $want -lt $cur && $cur -le $mx ]]; then
            reason="idle ${elapsed}m"
        else
            reason="bounds $mn-$mx"
        fi
        rc=0
        scale_target "$t" "$want" >/dev/null 2>&1 || rc=1
        new=$(count_runners_for_target "$t")
        total=$((total - cur + new))
        # A shrink restarts the idle clock: at most one removal per period
        [[ $want -lt $cur ]] && echo "$now" > "$PID_DIR/idle-since-$key"

        note="$(date '+%Y-%m-%d %H:%M:%S') $(target_label "$t") $cur → $want ($reason)"
        [[ $rc -eq 0 ]] || note="$note - failed, now $new"
        echo "$note" > "$PID_DIR/autoscale.last"
        if declare -F dlog >/dev/null 2>&1; then
            dlog "$note" || true
        fi
    done
    return 0
}
# ============================================================================
# Runner lifecycle
# ============================================================================

# setup_runner ID TARGET_URL - extract the tarball and register with GitHub
setup_runner() {
    local id=$1
    local target_url="${2:-}"
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"
    local labels_str

    # Configured already? (.runner is written by config.sh on success -
    # a bare directory can be a leftover from an interrupted setup)
    [[ -f "$runner_dir/.runner" ]] && return 0
    if [[ -d "$runner_dir" ]]; then
        warn "runner-$id directory exists but is not configured - rebuilding"
        rm -rf "$runner_dir"
    fi

    [[ -z "$target_url" ]] && { warn "setup_runner: no target given for runner-$id"; return 1; }
    rm -f "$PID_DIR/runner-$id.target" "$PID_DIR/runner-$id.ephemeral" "$PID_DIR/runner-$id.name" \
          "$PID_DIR/runner-$id.version"

    local disk_mb
    disk_mb=$(free_disk_mb)
    if [[ "$disk_mb" -lt 500 ]]; then
        warn "Refusing to set up runner-$id: only ${disk_mb}MB disk free"
        return 1
    fi

    # A registration token (1 h, single-purpose) rather than the user's own
    # token on config.sh's command line. Fetched before extracting, so a
    # permission problem costs no tar run.
    local tok
    if ! tok=$(registration_token "$target_url"); then
        gh_last_load
        warn "runner-$id: can't get a registration token for $(target_label "$target_url"): $(target_error_note "$target_url")"
        return 1
    fi

    mkdir -p "$runner_dir" || { warn "Failed to create directory"; return 1; }

    if ! tar -xzf "$RUNNER_TAR" -C "$runner_dir" 2>/dev/null; then
        warn "Failed to extract runner tarball"
        rm -rf "$runner_dir"
        return 1
    fi

    labels_str=$(detect_labels)

    local config_args=(
        --unattended
        --name "${RUNNER_NAME_PREFIX}-${id}"
        --url "$target_url"
        --token "$tok"
        --replace
    )

    if [[ -n "$labels_str" ]]; then
        config_args+=(--labels "${labels_str// /,}")
    fi
    [[ "$EPHEMERAL_RUNNERS" == "1" ]] && config_args+=(--ephemeral)

    local config_output
    if ! config_output=$(cd "$runner_dir" && ./config.sh "${config_args[@]}" 2>&1); then
        warn "Failed to configure runner-$id"
        echo -e "${DIM}$config_output${NC}" >&2
        if echo "$config_output" | grep -qi "libicu\|libssl\|Dependencies"; then
            echo -e "${YELLOW}  Hint: install runner dependencies:${NC}" >&2
            echo -e "${DIM}  sudo $runner_dir/bin/installdependencies.sh${NC}" >&2
        fi
        # If registration half-succeeded, try to unregister so we don't
        # leave a ghost runner on GitHub; log it for manual cleanup if not.
        if [[ -f "$runner_dir/.runner" ]]; then
            local rt
            if rt=$(remove_token "$target_url") \
                && (cd "$runner_dir" && ./config.sh remove --token "$rt" >/dev/null 2>&1); then
                :
            else
                echo "${RUNNER_NAME_PREFIX}-${id}" >> "$ORPHANS_FILE"
            fi
        fi
        rm -rf "$runner_dir"
        return 1
    fi

    # Version it was set up with; the runner may self-update later
    # (runner_version prefers what is actually on disk)
    local ver
    ver=$(tarball_version "$RUNNER_TAR")
    [[ -n "$ver" ]] && echo "$ver" > "$PID_DIR/runner-$id.version"

    # Remember the target (and registration mode) outside .runner, which an
    # ephemeral runner deletes after its job.
    echo "$target_url" > "$PID_DIR/runner-$id.target"
    # Remember the registered name so an ephemeral re-registration keeps it
    # even if RUNNER_NAME_PREFIX changes later
    echo "${RUNNER_NAME_PREFIX}-${id}" > "$PID_DIR/runner-$id.name"
    [[ "$EPHEMERAL_RUNNERS" == "1" ]] && touch "$PID_DIR/runner-$id.ephemeral"
    return 0
}

# Re-register an ephemeral runner in its existing directory (no
# re-extraction). After its one job the runner deletes .runner/.credentials
# and GitHub drops the registration, so every relaunch needs config.sh again.
reconfigure_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"
    local log_file="$LOG_DIR/runner-$id.log"
    local target_url tok labels_str config_output

    [[ -f "$runner_dir/.runner" ]] && return 0
    target_url=$(cat "$PID_DIR/runner-$id.target" 2>/dev/null) || target_url=""
    [[ -n "$target_url" ]] || { set_lasterr "$id" "not configured (no saved target)"; return 1; }
    [[ -x "$runner_dir/config.sh" ]] || { set_lasterr "$id" "config.sh not found/executable"; return 1; }

    # Marker line: lets ephemeral_job_finished tell a finished job from a
    # failed re-registration (so the latter still counts as a crash). It
    # goes before the token fetch so a token failure gets backoff too,
    # instead of a retry on every tick.
    echo "[runnermaxxer] re-registering ephemeral runner-$id ($(date '+%Y-%m-%d %H:%M:%S'))" >> "$log_file" 2>/dev/null || true

    if ! tok=$(registration_token "$target_url"); then
        gh_last_load
        set_lasterr "$id" "re-register failed: $(target_error_note "$target_url")"
        return 1
    fi

    labels_str="${CACHED_LABELS:-}"
    [[ -z "$labels_str" ]] && labels_str=$(detect_labels)
    local name
    name=$(cat "$PID_DIR/runner-$id.name" 2>/dev/null) || name=""
    [[ -n "$name" ]] || name="${RUNNER_NAME_PREFIX}-${id}"
    local config_args=(
        --unattended
        --name "$name"
        --url "$target_url"
        --token "$tok"
        --replace
        --ephemeral
    )
    [[ -n "$labels_str" ]] && config_args+=(--labels "${labels_str// /,}")

    if ! config_output=$(cd "$runner_dir" && ./config.sh "${config_args[@]}" 2>&1); then
        printf '%s\n' "$config_output" >> "$log_file" 2>/dev/null || true
        set_lasterr "$id" "re-register failed (view logs with 'l')"
        return 1
    fi
    return 0
}

# Environment for a runner launch (called inside the launch subshell)
runner_env() {
    if [[ "$SHARED_TOOL_CACHE" == "1" ]]; then
        local cache="$RUNNER_BASE_DIR/.toolcache"
        mkdir -p "$cache" 2>/dev/null || true
        # The runner resolves its tool directory from RUNNER_TOOL_CACHE,
        # then RUNNER_TOOLSDIRECTORY, then AGENT_TOOLSDIRECTORY; the
        # toolkit's tool-cache reads RUNNER_TOOL_CACHE. Set both common ones.
        export RUNNER_TOOL_CACHE="$cache" AGENT_TOOLSDIRECTORY="$cache"
    fi
    # Without this, run.sh reports a deprecated-version exit like any other;
    # with it, run-helper logs "deprecated version exit code" and
    # runner_exit_reason can say so
    export ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1
    return 0
}

start_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"
    local pid_file="$PID_DIR/runner-$id.pid"
    local log_file="$LOG_DIR/runner-$id.log"

    # Starting (or restarting) a runner means we want it up: clear any
    # manual-stop marker so the supervisor keeps it alive.
    unmark_stopped "$id"

    is_running "$id" && return 0

    # Adopt a live orphan (e.g. after a manager restart) instead of
    # starting a duplicate that would fight over the registration.
    local orphan
    orphan=$(runner_main_pid "$id") || orphan=""
    if [[ -n "$orphan" ]]; then
        echo "$orphan" > "$pid_file"
        dlog "runner-$id: adopted running process $orphan"
        return 0
    fi

    [[ ! -d "$runner_dir" ]] && { set_lasterr "$id" "runner directory missing"; warn "Runner directory not found"; return 1; }
    if [[ ! -f "$runner_dir/.runner" ]] && is_ephemeral "$id"; then
        reconfigure_runner "$id" || { warn "runner-$id: $(get_lasterr "$id")"; return 1; }
    fi
    [[ ! -f "$runner_dir/.runner" ]] && { set_lasterr "$id" "not configured (incomplete setup)"; warn "runner-$id is not configured"; return 1; }
    [[ ! -x "$runner_dir/run.sh" ]] && { set_lasterr "$id" "run.sh not found/executable"; warn "run.sh not found/executable"; return 1; }

    rotate_log "$id"
    date +%s > "$PID_DIR/runner-$id.laststart"

    # Launch with the runner dir as an absolute path on the command line so
    # is_running/runner_procs can identify the process family later.
    # setsid (where available) detaches it into its own session.
    local pid
    if command -v setsid >/dev/null 2>&1; then
        ( cd "$runner_dir" && runner_env && exec setsid nohup "$runner_dir/run.sh" >> "$log_file" 2>&1 < /dev/null ) &
    else
        ( cd "$runner_dir" && runner_env && exec nohup "$runner_dir/run.sh" >> "$log_file" 2>&1 < /dev/null ) &
    fi
    pid=$!
    echo "$pid" > "$pid_file"

    sleep 1
    if ! kill -0 "$pid" 2>/dev/null; then
        set_lasterr "$id" "process exited immediately (view logs with 'l')"
        warn "Runner-$id failed to start"
        rm -f "$pid_file"
        return 1
    fi

    rm -f "$PID_DIR/runner-$id.lasterr"
    dlog "runner-$id: started (pid $pid)"
    return 0
}

# Kill every process belonging to the runner, not just the top-level
# run.sh: killing only the recorded PID orphans Runner.Listener, which
# stays attached to GitHub and keeps taking jobs. SIGINT first so the
# listener can end its session cleanly, then escalate.
stop_runner_procs() {
    local id=$1
    local pid_file="$PID_DIR/runner-$id.pid"
    local pattern="$RUNNER_BASE_DIR/runner-$id/"

    if [[ -z "$(runner_procs "$id")" ]]; then
        rm -f "$pid_file"
        return 0
    fi

    pkill -INT -f "$pattern" 2>/dev/null || true
    local waited=0
    while [[ -n "$(runner_procs "$id")" && $waited -lt 10 ]]; do
        sleep 1
        waited=$((waited + 1))
    done

    if [[ -n "$(runner_procs "$id")" ]]; then
        pkill -TERM -f "$pattern" 2>/dev/null || true
        waited=0
        while [[ -n "$(runner_procs "$id")" && $waited -lt 5 ]]; do
            sleep 1
            waited=$((waited + 1))
        done
    fi

    pkill -KILL -f "$pattern" 2>/dev/null || true
    rm -f "$pid_file"
    return 0
}

stop_runner() {
    local id=$1
    # Record that this stop was intentional so the supervisor leaves it down.
    mark_stopped "$id"
    stop_runner_procs "$id"
    dlog "runner-$id: stopped"
}

# True when GitHub answers and does not list runner NAME under TARGET_URL,
# or answers 404 for the target itself (deleted repo/org: its runner
# registrations are gone with it)
runner_gone_from_github() {
    local url=$1 name=$2 names
    [[ -n "$url" ]] || return 1
    if ! names=$(gh_api --paginate "$(target_api_endpoint "$url")" --jq '.runners[].name'); then
        gh_last_load
        [[ "$GH_CLASS" == "notfound" ]] && return 0
        return 1
    fi
    ! grep -qFx -- "$name" <<< "$names"
}

remove_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"

    stop_runner "$id"

    if [[ -d "$runner_dir" ]]; then
        if [[ -f "$runner_dir/.runner" ]]; then
            local rt unregistered=0 reg_name reg_url
            reg_name=$(registered_name "$id")
            reg_url=$(runner_registered_url "$id")
            if rt=$(remove_token "$reg_url"); then
                if (cd "$runner_dir" && ./config.sh remove --token "$rt" >/dev/null 2>&1); then
                    unregistered=1
                fi
            else
                gh_last_load
                if [[ "$GH_CLASS" == "notfound" ]]; then
                    # The repo/org itself is gone, and its registrations with it
                    dlog "runner-$id: $(target_label "$reg_url") returns 404 - treating $reg_name as unregistered"
                    unregistered=1
                fi
            fi
            # An ephemeral runner's registration may already be gone
            # (GitHub deletes it after the job), so config.sh remove fails;
            # only record an orphan if GitHub still lists the runner.
            if [[ "$unregistered" != "1" ]] && is_ephemeral "$id" \
                && runner_gone_from_github "$(runner_registered_url "$id")" "$reg_name"; then
                unregistered=1
            fi
            if [[ "$unregistered" != "1" ]]; then
                warn "Failed to unregister $reg_name from GitHub"
                echo "$reg_name" >> "$ORPHANS_FILE"
                echo -e "${DIM}  Recorded in $ORPHANS_FILE - remove it in GitHub Settings → Actions → Runners${NC}" >&2
            fi
        fi
        rm -rf "$runner_dir"
    fi

    rm -f "$LOG_DIR/runner-$id.log" "$LOG_DIR/runner-$id.log.1"
    clear_runner_state "$id"
    dlog "runner-$id: removed"
}

# ============================================================================
# UI
# ============================================================================

render_runner_line() {
    local id=$1 eph=""
    # Ephemeral runners exit and re-register after every job
    is_ephemeral "$id" && eph=" ${DIM}ephemeral${NC}"
    if is_running "$id"; then
        local pid status status_color
        pid=$(cat "$PID_DIR/runner-$id.pid" 2>/dev/null || echo "?")
        status=$(get_runner_status "$id")
        status_color="${DIM}"

        [[ "$status" == running:* || "$status" == busy* ]] && status_color="${CYAN}"
        [[ "$status" == "idle"* ]] && status_color="${DIM}"
        [[ "$status" == *"error"* ]] && status_color="${RED}"

        if is_draining "$id"; then
            echo -e "    ${YELLOW}●${NC} runner-$id$eph ${DIM}PID $pid${NC} ${YELLOW}[draining: ${status#running: }]${NC}"
            return 0
        fi
        echo -e "    ${GREEN}●${NC} runner-$id$eph ${DIM}PID $pid${NC} ${status_color}[$status]${NC}"
    elif is_marked_stopped "$id"; then
        echo -e "    ${RED}○${NC} runner-$id ${DIM}stopped${NC}"
    elif is_quarantined "$id"; then
        echo -e "    ${RED}✖${NC} runner-$id ${RED}quarantined${NC} ${DIM}$(get_lasterr "$id")${NC}"
    else
        local err
        err=$(get_lasterr "$id")
        if [[ -n "$err" ]]; then
            echo -e "    ${YELLOW}◌${NC} runner-$id$eph ${YELLOW}restarting...${NC} ${DIM}($err)${NC}"
        else
            echo -e "    ${YELLOW}◌${NC} runner-$id$eph ${YELLOW}restarting...${NC}"
        fi
    fi
}

# Dim header note: age of the GitHub API data behind runner status
gh_poll_note() {
    local ts
    if [[ "$GH_HEALTH_TICKS" -eq 0 ]]; then
        echo -e "  ${DIM}GitHub: polling off (status from logs)${NC}"
        return 0
    fi
    ts=$(cat "$PID_DIR/gh-poll.ts" 2>/dev/null || echo "")
    if [[ "$ts" =~ ^[0-9]+$ ]]; then
        echo -e "  ${DIM}GitHub: polled $(($(date +%s) - ts))s ago${NC}"
    else
        echo -e "  ${DIM}GitHub: not polled yet${NC}"
    fi
    return 0
}

render_ui() {
    local total running disk_mb t ids id any=0 i name box tag pending=0 want

    total=$(get_runner_count)
    running=$(count_running)
    want=$(menu_total_want)
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        [[ ${M_WANT[i]} -ne ${M_CUR[i]} ]] && pending=1
    done

    echo -e "${BOLD}${CYAN}"
    echo "  ╔═══════════════════════════════════════════════════╗"
    echo "  ║            gh-runnermaxxer                        ║"
    echo -e "  ╚═══════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "  ${DIM}Labels: ${CACHED_LABELS}${NC}"
    gh_poll_note
    echo ""
    if [[ $pending -eq 1 ]]; then
        echo -e "  ${BOLD}Status:${NC} ${GREEN}$running running${NC} / $total configured  ${YELLOW}→ $want after apply${NC} ${DIM}(max $MAX_RUNNERS)${NC}"
    else
        echo -e "  ${BOLD}Status:${NC} ${GREEN}$running running${NC} / $total configured ${DIM}(max $MAX_RUNNERS)${NC}"
    fi

    disk_mb=$(free_disk_mb)
    if [[ "$disk_mb" -lt 1024 ]]; then
        echo -e "  ${RED}⚠ Low disk space: ${disk_mb}MB free${NC}"
    fi
    if [[ -s "$ORPHANS_FILE" ]]; then
        echo -e "  ${YELLOW}⚠ Orphaned GitHub registrations need manual cleanup (see $(basename "$ORPHANS_FILE"))${NC}"
    fi
    echo ""

    # Runners grouped by project; ↑/↓ moves the selector, ←/→ changes the
    # selected project's runner count (applied with Enter)
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        any=1
        t="${M_URLS[i]}"
        name=$(target_label "$t")
        [[ ${#name} -gt 40 ]] && name="${name:0:37}..."
        if [[ $i -eq $M_SEL ]]; then
            box=$(printf "${BOLD}${CYAN}◂ %2d ▸${NC}" "${M_WANT[i]}")
        else
            box=$(printf "  %2d  " "${M_WANT[i]}")
        fi
        tag=""
        [[ ${M_WANT[i]} -ne ${M_CUR[i]} ]] && tag="  ${YELLOW}${M_CUR[i]} → ${M_WANT[i]}${NC}"
        [[ ${M_LISTED[i]} -eq 0 ]] && tag="$tag  ${DIM}(not in targets file)${NC}"
        tag="$tag$(autoscale_tag "$t" queue)"
        if [[ $i -eq $M_SEL ]]; then
            printf "  ${CYAN}▸${NC} ${BOLD}%-40s${NC} [%s]%b\n" "$name" "$box" "$tag"
        elif [[ ${M_CUR[i]} -eq 0 ]]; then
            printf "    ${DIM}%-40s${NC} [%s]%b\n" "$name" "$box" "$tag"
        else
            printf "    ${BOLD}%-40s${NC} [%s]%b\n" "$name" "$box" "$tag"
        fi
        for id in $(runner_ids_for_target "$t"); do
            render_runner_line "$id"
        done
    done

    ids=$(unassigned_runner_ids)
    if [[ -n "$ids" ]]; then
        any=1
        echo -e "  ${YELLOW}Unconfigured (incomplete setup - remove with 'd')${NC}"
        for id in $ids; do
            render_runner_line "$id"
        done
    fi

    if [[ $any -eq 0 ]]; then
        echo -e "  ${YELLOW}No projects configured (press 't' to add one)${NC}"
    fi

    echo ""
    echo -e "  ${BOLD}────────────────────────────────────────────────────${NC}"
    echo -e "  ${CYAN}↑/↓${NC} choose project      ${CYAN}←/→${NC} runners (or ${CYAN}+/-${NC}, ${CYAN}0-9${NC})"
    if [[ $pending -eq 1 ]]; then
        echo -e "  ${CYAN}Enter${NC} ${YELLOW}apply changes${NC}       ${CYAN}Esc${NC} discard changes"
    else
        echo -e "  ${CYAN}Enter${NC} apply changes"
    fi
    echo ""
    echo -e "  ${BOLD}Commands:${NC}"
    echo -e "    ${CYAN}d${NC}  Remove a runner     ${CYAN}l${NC}  View logs"
    echo -e "    ${CYAN}s${NC}  Start all           ${CYAN}x${NC}  Stop all"
    echo -e "    ${CYAN}r${NC}  Restart all         ${CYAN}c${NC}  Check GitHub status"
    echo -e "    ${CYAN}t${NC}  Projects & scaling  ${CYAN}e${NC}  Edit config"
    echo -e "    ${CYAN}q${NC}  Quit"
    if [[ -n "$M_MSG" ]]; then
        echo ""
        echo -e "  $M_MSG"
    fi
    echo ""
    echo -e "  ${DIM}Auto-refreshes every ${REFRESH_INTERVAL}s${NC}"
}

# Repaint in place: clear each line's tail (\033[K) and everything below
# the frame (\033[J) instead of `clear`, so the screen never goes blank
# between frames.
draw_frame() {
    local frame="$1"
    frame="${frame//$'\n'/$'\033[K\n'}"
    printf '\033[H%s\033[K\n\033[J' "$frame"
}

draw_ui() {
    draw_frame "$(render_ui)"
}
next_free_id() {
    local next_id=1
    while [[ -d "$RUNNER_BASE_DIR/runner-$next_id" ]]; do next_id=$((next_id + 1)); done
    echo "$next_id"
}

remove_runner_prompt() {
    local ids
    ids=$(get_runner_ids)
    [[ -z "$ids" ]] && { echo -e "\n  ${YELLOW}No runners${NC}"; sleep 1; return 0; }

    build_runner_pick_list
    pick_item "Remove which runner?" "remove" || return 0
    local id="${PICK_IDS[$PICK_INDEX]}"
    [[ -d "$RUNNER_BASE_DIR/runner-$id" ]] || { echo -e "  ${YELLOW}Not found${NC}"; sleep 1; return 0; }

    local drain=0 url i
    if is_busy "$id"; then
        printf '  %b' "${YELLOW}runner-$id is running a job - [d]rain (remove after current job) / [k]ill now / [c]ancel [D/k/c]: ${NC}"
        local choice=""
        read -r choice || true
        case "$choice" in
            [Kk]*) ;;
            [Cc]*) echo -e "  ${DIM}Cancelled${NC}"; sleep 1; return 0 ;;
            *)     drain=1 ;;
        esac
    fi

    # Drop the target's count along with the runner so the dashboard
    # doesn't show a pending "N-1 → N" change afterwards (a draining runner
    # is already excluded from the count)
    url=$(runner_registered_url "$id")
    if [[ -n "$url" ]] && ! is_draining "$id"; then
        for ((i = 0; i < ${#M_URLS[@]}; i++)); do
            if same_target "${M_URLS[i]}" "$url" && [[ ${M_WANT[i]} -eq ${M_CUR[i]} && ${M_WANT[i]} -gt 0 ]]; then
                M_WANT[i]=$((M_WANT[i] - 1))
            fi
        done
    fi

    if [[ $drain -eq 1 ]]; then
        mark_draining "$id"
        echo -e "  ${YELLOW}runner-$id is draining - it will be removed when the job finishes${NC}"
        sleep 1
        return 0
    fi

    echo -e "  ${BLUE}Removing runner-$id...${NC}"
    remove_runner "$id"
    echo -e "  ${GREEN}Done!${NC}"
    sleep 1
}

start_all() {
    local id
    for id in $(get_runner_ids); do
        echo -e "  Starting runner-$id..."
        clear_failure_state "$id"
        start_runner "$id" || true
    done
    echo -e "  ${GREEN}Done!${NC}"
    sleep 1
}

stop_all() {
    local busy=""
    local id
    for id in $(get_runner_ids); do
        is_busy "$id" && busy="$busy runner-$id"
    done
    if [[ -n "$busy" ]]; then
        printf '\n  %b' "${YELLOW}Jobs in progress on:$busy - stop anyway? [y/N]: ${NC}"
        local sure
        read -r sure
        [[ "$sure" =~ ^[Yy] ]] || { echo -e "  ${DIM}Cancelled${NC}"; sleep 1; return 0; }
    fi

    for id in $(get_runner_ids); do
        echo -e "  Stopping runner-$id..."
        stop_runner "$id" || true
    done
    echo -e "  ${GREEN}Done!${NC}"
    sleep 1
}

restart_all() {
    stop_all
    start_all
}


view_logs() {
    local ids
    ids=$(get_runner_ids)
    [[ -z "$ids" ]] && { echo -e "\n  ${YELLOW}No runners${NC}"; sleep 1; return 0; }

    build_runner_pick_list
    pick_item "View logs for which runner?" "view" || return 0
    local id="${PICK_IDS[$PICK_INDEX]}"

    local log_file="$LOG_DIR/runner-$id.log"
    [[ ! -f "$log_file" ]] && { echo -e "  ${YELLOW}No log file${NC}"; sleep 1; return 0; }

    echo -e "\n  ${DIM}(Ctrl+C to exit)${NC}\n"
    sleep 1
    # A no-op trap (not '', which children inherit as SIG_IGN and would
    # make Ctrl+C unable to kill tail): the child gets default disposition
    # and dies on Ctrl+C while we survive and return to the menu.
    trap ':' INT
    tail -f "$log_file" || true
    trap 'exit 130' INT
}

check_github_status() {
    echo -e "\n  ${BLUE}Checking GitHub runner status...${NC}"
    github_status_body || true
    echo -e "\n  ${DIM}Press any key...${NC}"
    read -rsn1 || true
}

# What GitHub lists for each target, plus the local orphan list. Returns 1
# when any target could not be fetched.
github_status_body() {
    local t endpoint out any=0 rc=0

    for t in $(known_targets); do
        any=1
        echo -e "\n  ${BOLD}$(target_label "$t")${NC}"
        endpoint=$(target_api_endpoint "$t")
        if out=$(gh_api --paginate "$endpoint" --jq '.runners[] | "    \(.name): \(.status)"'); then
            if [[ -n "$out" ]]; then
                echo "$out"
            else
                echo -e "    ${DIM}no runners registered${NC}"
            fi
        else
            gh_last_load
            echo -e "    ${YELLOW}Could not fetch: $(gh_error_note "$GH_CLASS" "$GH_MSG")${NC}"
            rc=1
        fi
    done
    [[ $any -eq 0 ]] && echo -e "  ${YELLOW}No projects configured${NC}"

    if [[ -s "$ORPHANS_FILE" ]]; then
        echo ""
        echo -e "  ${YELLOW}Orphaned registrations recorded locally (failed unregisters):${NC}"
        sed 's/^/    /' "$ORPHANS_FILE"
        echo -e "  ${DIM}Remove them in GitHub Settings → Actions → Runners, then delete $(basename "$ORPHANS_FILE")${NC}"
    fi
    return $rc
}
edit_config() {
    echo ""
    echo -e "  ${BOLD}Configuration${NC}"
    echo ""
    echo -e "  ${CYAN}1${NC}) Projects & runner counts (same as 't')"
    echo -e "  ${CYAN}2${NC}) Change runner name prefix (current: $RUNNER_NAME_PREFIX)"
    echo -e "  ${CYAN}3${NC}) Toggle shared tool cache (current: $SHARED_TOOL_CACHE)"
    echo -e "  ${CYAN}4${NC}) Toggle ephemeral runners for new runners (current: $EPHEMERAL_RUNNERS)"
    echo -e "  ${CYAN}5${NC}) Toggle autoscale for projects with bounds (current: $AUTOSCALE)"
    echo -e "  ${CYAN}6${NC}) Autoscale idle minutes before scaling down (current: $AUTOSCALE_IDLE_MINUTES)"
    echo -e "  ${CYAN}7${NC}) Cancel"
    echo ""
    printf '  Choice: '
    read -rsn1 choice
    echo ""

    case "$choice" in
        1)
            target_menu tui || true
            return 0
            ;;
        2)
            printf '  Runner name prefix (applies to runners added from now on): '
            read -r prefix
            if [[ -n "$prefix" ]]; then
                if valid_prefix "$prefix"; then
                    RUNNER_NAME_PREFIX="$prefix"
                    save_config
                else
                    echo -e "  ${YELLOW}Invalid prefix (letters, numbers, _, - only; at most 60 characters)${NC}"
                fi
            fi
            ;;
        3)
            if [[ "$SHARED_TOOL_CACHE" == "1" ]]; then SHARED_TOOL_CACHE=0; else SHARED_TOOL_CACHE=1; fi
            save_config
            echo -e "  ${DIM}Shared tool cache: $SHARED_TOOL_CACHE (takes effect as runners restart)${NC}"
            ;;
        4)
            if [[ "$EPHEMERAL_RUNNERS" == "1" ]]; then EPHEMERAL_RUNNERS=0; else EPHEMERAL_RUNNERS=1; fi
            save_config
            echo -e "  ${DIM}Ephemeral runners: $EPHEMERAL_RUNNERS (runners added from now on; remove and re-add existing ones to switch)${NC}"
            ;;
        5)
            if [[ "$AUTOSCALE" == "1" ]]; then AUTOSCALE=0; else AUTOSCALE=1; fi
            save_config
            echo -e "  ${DIM}Autoscale: $AUTOSCALE (only projects with bounds - set them with 'b' in the projects menu)${NC}"
            ;;
        6)
            printf '  Idle minutes before an autoscaled project shrinks by one: '
            read -r mins
            if [[ "$mins" =~ ^[0-9]+$ ]]; then
                AUTOSCALE_IDLE_MINUTES=$mins
                save_config
            elif [[ -n "$mins" ]]; then
                echo -e "  ${YELLOW}Expected a whole number of minutes${NC}"
            fi
            ;;
    esac
    sleep 1
}
quit_prompt() {
    local running
    running=$(count_running)
    if [[ "$running" -gt 0 ]]; then
        echo ""
        printf '  %b' "${YELLOW}$running runner(s) still active. [k]eep them running, [x] stop them, [c]ancel: ${NC}"
        local ans
        read -r ans
        case "$ans" in
            k|K|"")
                echo -e "\n  ${DIM}Note: runners left running are NOT supervised (no auto-restart) until you reopen the manager or start --daemon.${NC}"
                sleep 1
                ;;
            x|X)
                local id
                for id in $(get_runner_ids); do
                    echo -e "  Stopping runner-$id..."
                    stop_runner "$id" || true
                done
                ;;
            *)
                return 1
                ;;
        esac
    fi
    clear
    exit 0
}

# Apply pending counts from the dashboard, then hand the screen back
dashboard_apply() {
    local i changed=0
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        [[ ${M_WANT[i]} -ne ${M_CUR[i]} ]] && changed=1
    done
    [[ $changed -eq 0 ]] && { M_MSG="${DIM}Nothing to apply${NC}"; return 0; }
    apply_target_counts || true
    tput cnorm 2>/dev/null || true
    clear
    return 0
}

dashboard_discard() {
    M_URLS=(); M_WANT=()
    menu_reload
    M_MSG="${DIM}Pending changes discarded${NC}"
    return 0
}

# ============================================================================
# Supervisor tick, headless daemon, service install, scripting CLI
# ============================================================================

# One supervision step, shared by the TUI loop and --daemon: restart /
# drain / quarantine runners, and every GH_HEALTH_TICKS ticks cross-check
# them against GitHub. SUPERVISOR_TICK is global so the TUI's 'c' key can
# force a poll on the next tick.
supervisor_tick() {
    [[ "$DAEMON_MODE" == "1" ]] && config_reload_if_changed
    note_tick_time
    supervise_runners || true
    disk_usage_tick || true
    # --poll from another process: poll GitHub on this tick
    if [[ -f "$PID_DIR/poll.request" ]]; then
        rm -f "$PID_DIR/poll.request"
        [[ "$GH_HEALTH_TICKS" -gt 0 ]] && SUPERVISOR_TICK=$(( $(effective_health_ticks "$HEALTH_N_TARGETS") - 1 ))
    fi
    if [[ "$GH_HEALTH_TICKS" -gt 0 ]]; then
        SUPERVISOR_TICK=$((SUPERVISOR_TICK + 1))
        if [[ $SUPERVISOR_TICK -ge $(effective_health_ticks "$HEALTH_N_TARGETS") ]]; then
            SUPERVISOR_TICK=0
            check_github_health || true
        fi
    fi
    # state.json for the Go TUI and --status --json (never calls gh)
    if [[ "$DAEMON_MODE" == "1" ]]; then write_state_snapshot daemon; else write_state_snapshot tui; fi
    return 0
}

# Guard against bad values busy-looping the supervisor or breaking
# arithmetic: reset what isn't a number, clamp what is out of range. Each
# change is noted in CONFIG_WARNINGS (C2) so the TUI can show "clamped".
# _clamp KEY MIN MAX DEFAULT
_clamp() {
    local k=$1 lo=$2 hi=$3 def=$4 v
    eval "v=\${$k:-}"
    if [[ ! "$v" =~ ^[0-9]{1,9}$ ]]; then
        [[ -n "$v" ]] && config_warn "$k='$v' is not a number - using $def"
        v=$def
    else
        v=$((10#$v))
        if [[ $v -lt $lo ]]; then
            config_warn "$k clamped to $lo (was $v)"; v=$lo
        elif [[ $v -gt $hi ]]; then
            config_warn "$k clamped to $hi (was $v)"; v=$hi
        fi
    fi
    eval "$k=\$v"
}

_bool() {
    local k=$1 def=$2 v
    eval "v=\${$k:-}"
    if [[ ! "$v" =~ ^[01]$ ]]; then
        config_warn "$k='$v' must be 0 or 1 - using $def"
        eval "$k=\$def"
    fi
}

sanitize_settings() {
    _clamp REFRESH_INTERVAL 1 3600 5
    _clamp MAX_RESTART_ATTEMPTS 1 100 5
    _clamp MAX_LOG_SIZE_MB 1 10000 10
    _clamp GH_HEALTH_TICKS 0 100000 12
    _clamp MAX_RUNNERS 1 500 20
    _clamp AUTOSCALE_IDLE_MINUTES 0 100000 10
    _bool SHARED_TOOL_CACHE 1
    _bool EPHEMERAL_RUNNERS 0
    _bool AUTOSCALE 0
    return 0
}

# PID of a live --daemon (a stale pid file, or a recycled PID, doesn't count)
daemon_pid() {
    local pid
    pid=$(cat "$DAEMON_PID_FILE" 2>/dev/null) || return 1
    [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null || return 1
    ps -p "$pid" -o command= 2>/dev/null | grep -q -- '--daemon' || return 1
    echo "$pid"
}

daemon_running_msg() {
    echo "a daemon is running (pid $1); use --status / --scale, or stop it with --stop-daemon"
}

# Locking model. The main lock ($LOCK_FILE) is held for the whole life of
# the TUI or the daemon, so only one of them ever supervises. A mutating CLI
# command (--scale/--drain/--stop/--start/--remove) then:
#   - no manager running: takes the main lock itself for its short run, so
#     a TUI/daemon can't start in the middle of it;
#   - daemon running: takes this "operation" lock instead. The daemon takes
#     it too (without waiting) around every supervisor_tick and skips the
#     tick while a CLI command holds it, so the two never touch runner state
#     at the same time. The daemon keeps no state in memory between ticks
#     (everything is re-read from disk), so it simply sees the result on its
#     next tick;
#   - TUI running: refused, since the TUI holds unapplied edits in memory.
# Read-only commands (--status) take no lock at all.
# acquire_op_lock [SECONDS_TO_WAIT]
acquire_op_lock() {
    local wait=${1:-0} lock_pid stale=0
    while true; do
        if ( set -o noclobber; echo "$$" > "$OP_LOCK_FILE" ) 2>/dev/null; then
            OP_LOCK_ACQUIRED=1
            return 0
        fi
        lock_pid=$(cat "$OP_LOCK_FILE" 2>/dev/null || echo "")
        if [[ $stale -eq 0 ]] && { [[ -z "$lock_pid" ]] || ! kill -0 "$lock_pid" 2>/dev/null; }; then
            rm -f "$OP_LOCK_FILE"    # holder is gone
            stale=1
            continue
        fi
        [[ $wait -gt 0 ]] || return 1
        wait=$((wait - 1))
        stale=0
        sleep 1
    done
}

release_op_lock() {
    [[ "$OP_LOCK_ACQUIRED" == "1" ]] && rm -f "$OP_LOCK_FILE"
    OP_LOCK_ACQUIRED=0
    return 0
}

# Take whichever lock a mutating CLI command needs (see acquire_op_lock).
# Sets CLI_SUPERVISED=1 when a daemon will supervise the result.
CLI_SUPERVISED=0
cli_lock() {
    local pid
    acquire_lock && return 0
    if pid=$(daemon_pid); then
        CLI_SUPERVISED=1
        acquire_op_lock 60 && return 0
        die "the daemon (pid $pid) stayed busy for 60s - try again"
    fi
    pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "?")
    die "the manager is open in a terminal (pid $pid) - make the change there, or quit it first"
}

DAEMON_SLEEP_PID=""
run_daemon() {
    echo "$$" > "$DAEMON_PID_FILE"
    trap daemon_on_signal INT TERM
    dlog "daemon started (pid $$, $(get_runner_count) runner(s), tick ${REFRESH_INTERVAL}s)"
    echo "gh-runnermaxxer daemon running (pid $$) - events in $DAEMON_LOG"
    SUPERVISOR_TICK=$((GH_HEALTH_TICKS - 1))   # first tick polls GitHub
    while true; do
        if acquire_op_lock; then
            supervisor_tick
            release_op_lock
        fi
        # Background + wait so SIGTERM is handled at once, not after the sleep
        sleep "$REFRESH_INTERVAL" &
        DAEMON_SLEEP_PID=$!
        wait "$DAEMON_SLEEP_PID" 2>/dev/null || true
    done
}

# Runner processes are detached and deliberately left alive: the next
# manager (daemon or TUI) re-adopts them in reconcile_state. The EXIT trap
# (cleanup) releases the locks and the pid file.
daemon_on_signal() {
    [[ -n "$DAEMON_SLEEP_PID" ]] && { kill "$DAEMON_SLEEP_PID" 2>/dev/null || true; }
    dlog "daemon stopping (signal) - runners left running"
    exit 0
}

cli_stop_daemon() {
    local pid waited=0
    if ! pid=$(daemon_pid); then
        echo "No daemon running"
        return 0
    fi
    kill -TERM "$pid" 2>/dev/null || true
    while kill -0 "$pid" 2>/dev/null && [[ $waited -lt 10 ]]; do
        sleep 1
        waited=$((waited + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
        echo -e "${RED}Daemon (pid $pid) did not exit within 10s${NC}" >&2
        return 1
    fi
    echo "Daemon (pid $pid) stopped; runners keep running, unsupervised"
    if [[ -f "$(service_file_path)" ]]; then
        echo "Note: it is installed as a service, which will start it again. To stop it for good: --uninstall-service"
    fi
    return 0
}

# ----------------------------------------------------------------------------
# Service files (launchd on macOS, systemd --user on Linux). The generators
# take every path as an argument so tests can run them without touching $HOME.
# ----------------------------------------------------------------------------

xml_escape() {
    local s=$1
    s=${s//&/"&amp;"}
    s=${s//</"&lt;"}
    s=${s//>/"&gt;"}
    printf '%s' "$s"
}

# systemd unit values: '%' starts a specifier, so double it
systemd_escape() {
    local s=$1
    s=${s//%/"%%"}
    printf '%s' "$s"
}

# PATH for the service: gh's directory (and any other given dirs) first,
# then the usual system locations, without duplicates
service_path_value() {
    local out="" d
    for d in "$@" /opt/homebrew/bin /usr/local/bin /usr/bin /bin /usr/sbin /sbin; do
        [[ -n "$d" ]] || continue
        case ":$out:" in *":$d:"*) continue ;; esac
        out="${out:+$out:}$d"
    done
    echo "$out"
}

# launchd_plist SCRIPT WORKDIR PATH_VALUE OUT_FILE
launchd_plist() {
    local script workdir pathv out ghv=""
    script=$(xml_escape "$1"); workdir=$(xml_escape "$2")
    pathv=$(xml_escape "$3"); out=$(xml_escape "$4")
    [[ -n "${5:-}" ]] && ghv=$(xml_escape "$5")
    cat << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$SERVICE_LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$script</string>
        <string>--daemon</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$workdir</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$pathv</string>$([[ -n "$ghv" ]] && printf '\n        <key>RUNNERMAXXER_GH</key>\n        <string>%s</string>' "$ghv")
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>30</integer>
    <!-- Runners are children of the daemon: keep them alive when it stops -->
    <key>AbandonProcessGroup</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$out</string>
    <key>StandardErrorPath</key>
    <string>$out</string>
</dict>
</plist>
EOF
}

# systemd_unit SCRIPT WORKDIR PATH_VALUE
systemd_unit() {
    local script bs='\' q='"'
    script=$(systemd_escape "$1")
    script=${script//"$bs"/"$bs$bs"}
    script=${script//"$q"/"$bs$q"}
    script=${script//\$/"\$\$"}
    cat << EOF
[Unit]
Description=gh-runnermaxxer - GitHub Actions self-hosted runner pool

[Service]
Type=simple
ExecStart="$script" --daemon
WorkingDirectory=$(systemd_escape "$2")
Environment="PATH=$(systemd_escape "$3")"$([[ -n "${4:-}" ]] && printf '\nEnvironment="RUNNERMAXXER_GH=%s"' "$(systemd_escape "$4")")
Restart=always
RestartSec=10
# Runners are detached children and must outlive daemon restarts: signal
# only the daemon, not the whole cgroup
KillMode=process

[Install]
WantedBy=default.target
EOF
}

service_file_path() {
    if [[ "$OS_FAMILY" == "macos" ]]; then
        echo "$HOME/Library/LaunchAgents/$SERVICE_LABEL.plist"
    else
        echo "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$SERVICE_UNIT"
    fi
}

install_service() {
    local script gh_bin path_val file uid pid
    script="$SCRIPT_DIR/$(basename "$0")"
    [[ -f "$CONFIG_FILE" ]] || die "No configuration found - run --setup first"
    gh_bin=$(command -v "$GH_BIN" 2>/dev/null) || die "gh CLI not found in PATH"
    # Absolute, so the service never depends on its PATH to find gh (A4)
    case "$gh_bin" in /*) ;; *) gh_bin="$(cd "$(dirname "$gh_bin")" && pwd)/$(basename "$gh_bin")" ;; esac
    local since
    since=$(date +%s)
    path_val=$(service_path_value "$(dirname "$gh_bin")")
    file=$(service_file_path)
    mkdir -p "$LOG_DIR" "$(dirname "$file")"

    case "$OS_FAMILY" in
        macos)
            launchd_plist "$script" "$SCRIPT_DIR" "$path_val" "$LOG_DIR/daemon.out" "$gh_bin" > "$file"
            echo "Wrote $file"
            uid=$(id -u)
            if launchctl bootout "gui/$uid/$SERVICE_LABEL" >/dev/null 2>&1; then
                sleep 1
            fi
            if ! launchctl bootstrap "gui/$uid" "$file" 2>/dev/null; then
                launchctl load -w "$file" || die "launchctl could not load $file"
            fi
            echo "Loaded launchd agent $SERVICE_LABEL (starts at login, restarted if it exits)"
            echo ""
            echo "  Status:  launchctl print gui/$uid/$SERVICE_LABEL | head -20"
            echo "           $script --status"
            echo "  Logs:    tail -f \"$DAEMON_LOG\" \"$LOG_DIR/daemon.out\""
            ;;
        linux)
            command -v systemctl >/dev/null 2>&1 \
                || die "systemctl not found - run '$script --daemon' from your init system instead"
            systemd_unit "$script" "$SCRIPT_DIR" "$path_val" "$gh_bin" > "$file"
            echo "Wrote $file"
            systemctl --user daemon-reload && systemctl --user enable --now "$SERVICE_UNIT" \
                || die "systemctl --user could not start $SERVICE_UNIT"
            echo "Enabled and started $SERVICE_UNIT"
            echo ""
            echo "  Status:  systemctl --user status $SERVICE_UNIT"
            echo "           $script --status"
            echo "  Logs:    journalctl --user -u $SERVICE_UNIT -f"
            echo "           tail -f \"$DAEMON_LOG\""
            if command -v loginctl >/dev/null 2>&1 \
                && [[ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != "yes" ]]; then
                echo ""
                warn "Lingering is off: the service stops when you log out and won't start at boot. Fix: loginctl enable-linger $USER"
            fi
            ;;
        *)
            die "Unsupported OS for --install-service - run '$script --daemon' from your init system instead"
            ;;
    esac
    if pid=$(cat "$LOCK_FILE" 2>/dev/null) && kill -0 "$pid" 2>/dev/null && [[ "$pid" != "$(daemon_pid || true)" ]]; then
        warn "The manager is open in a terminal (pid $pid); the service takes over once you quit it"
        return 0
    fi
    service_smoke_test "$since" "$file"
}

# wait_for_snapshot_tick EPOCH SECONDS - wait until state.json has a
# tick_ts >= EPOCH (a supervisor tick after EPOCH). Returns 1 on timeout.
wait_for_snapshot_tick() {
    local since=$1 left=$2 ts
    while true; do
        ts=$(sed -n 's/.*"tick_ts":\([0-9][0-9]*\).*/\1/p' "$PID_DIR/state.json" 2>/dev/null | head -1) || ts=""
        [[ "$ts" =~ ^[0-9]+$ && $ts -ge $since ]] && return 0
        [[ $left -gt 0 ]] || return 1
        left=$((left - 1))
        sleep 1
    done
}

# True when gh.err records an auth failure at or after EPOCH
service_auth_failed_since() {
    local e cls ts
    e=$(cat "$PID_DIR/gh.err" 2>/dev/null) || return 1
    cls=$(printf '%s\n' "$e" | cut -f1); ts=$(printf '%s\n' "$e" | cut -f4)
    [[ "$cls" == "auth" && "$ts" =~ ^[0-9]+$ && $ts -ge $1 ]]
}

service_token_hint() {
    echo -e "${RED}✗ the daemon cannot read your gh token (the keychain is not available to services).${NC}" >&2
    echo "  Either run once:  gh auth login --insecure-storage" >&2
    echo "  or set GH_TOKEN in the service file: $1 (stored in plain text)" >&2
}

# After install: is the daemon up, ticking, and able to use gh? A keychain
# token that the user's shell can read is often out of reach for a
# launchd/systemd service (S8) - say so plainly. Leaves the service
# installed either way; returns 1 when something is wrong.
service_smoke_test() {
    local since=$1 file=$2 pid="" i st user
    echo ""
    echo "Checking the service..."
    for ((i = 0; i < 15; i++)); do
        pid=$(daemon_pid) && break
        pid=""
        sleep 1
    done
    if [[ -z "$pid" ]] || ! wait_for_snapshot_tick "$since" $((3 * REFRESH_INTERVAL + 10)); then
        if service_auth_failed_since "$since"; then
            service_token_hint "$file"
        elif [[ -z "$pid" ]]; then
            echo -e "${RED}✗ the daemon did not start within 15 s - see $LOG_DIR/daemon.out and $DAEMON_LOG${NC}" >&2
        else
            echo -e "${RED}✗ the daemon (pid $pid) is running but has not completed a tick - see $DAEMON_LOG${NC}" >&2
        fi
        return 1
    fi
    st=$(gh_state)
    user=$(cat "$PID_DIR/gh.user" 2>/dev/null) || user=""
    case "$st" in
        ok)
            echo -e "${GREEN}✓${NC} daemon running (pid $pid), gh: ok${user:+ as $user}"
            ;;
        auth)
            service_token_hint "$file"
            return 1
            ;;
        *)
            echo -e "${YELLOW}⚠ daemon running (pid $pid), gh: $st - $(cat "$PID_DIR/gh.msg" 2>/dev/null || echo "see $DAEMON_LOG")${NC}"
            ;;
    esac
    return 0
}

uninstall_service() {
    local file uid
    file=$(service_file_path)
    if [[ ! -f "$file" ]]; then
        echo "No service installed ($file not found)"
        return 0
    fi
    case "$OS_FAMILY" in
        macos)
            uid=$(id -u)
            launchctl bootout "gui/$uid/$SERVICE_LABEL" >/dev/null 2>&1 \
                || launchctl unload -w "$file" >/dev/null 2>&1 || true
            rm -f "$file"
            ;;
        *)
            systemctl --user disable --now "$SERVICE_UNIT" >/dev/null 2>&1 || true
            rm -f "$file"
            systemctl --user daemon-reload >/dev/null 2>&1 || true
            ;;
    esac
    echo "Removed $file and stopped the service"
    echo "Runners keep running but are unsupervised: open the manager, run --daemon, or stop them with --stop <id>"
    return 0
}

# ----------------------------------------------------------------------------
# --status
# ----------------------------------------------------------------------------

# running | idle | busy | draining | stopped | quarantined | restarting
runner_state() {
    local id=$1
    if is_running "$id"; then
        if is_draining "$id"; then echo "draining"
        elif is_busy "$id"; then echo "busy"
        elif [[ "$(get_runner_status "$id")" == idle* ]]; then echo "idle"
        else echo "running"
        fi
    elif is_draining "$id"; then echo "draining"
    elif is_marked_stopped "$id"; then echo "stopped"
    elif is_quarantined "$id"; then echo "quarantined"
    else echo "restarting"
    fi
    return 0
}

# JSON string literal (quotes included)
json_str() {
    local s=$1 bs='\' q='"'
    s=${s//"$bs"/"$bs$bs"}
    s=${s//"$q"/"$bs$q"}
    s=${s//$'\n'/"${bs}n"}
    s=${s//$'\r'/"${bs}r"}
    s=${s//$'\t'/"${bs}t"}
    if [[ "$s" == *[[:cntrl:]]* ]]; then
        s=$(printf '%s' "$s" | LC_ALL=C tr -d '\000-\037\177')
    fi
    printf '"%s"' "$s"
}

# Number or null
json_num() {
    if [[ "$1" =~ ^[0-9]+$ ]]; then printf '%s' "$1"; else printf 'null'; fi
}

# Snapshot every project and runner into parallel arrays (P_* / R_*), so
# the text and JSON renderers show the same data from one pass.
status_collect() {
    local t id st n conf run busy drain ids now fails last job b e key q
    now=$(date +%s)
    P_URLS=(); P_LISTED=(); P_CONF=(); P_RUN=(); P_BUSY=(); P_DRAIN=()
    P_TYPE=(); P_WANT=(); P_MIN=(); P_MAX=(); P_QUEUED=(); P_UNSAT=()
    P_ERRC=(); P_ERRM=(); P_ERRS=()
    R_IDS=(); R_PROJ=(); R_STATE=(); R_PID=(); R_STATUS=(); R_ERR=(); R_EPH=()
    R_NAME=(); R_JOB=(); R_JOBSTART=(); R_VER=(); R_FAILS=(); R_NEXT=()
    R_WORKMB=(); R_DRAIN=(); R_QUAR=()
    for t in $(known_targets) ""; do
        if [[ -n "$t" ]]; then ids=$(runner_ids_for_target "$t"); else ids=$(unassigned_runner_ids); fi
        conf=0; run=0; busy=0; drain=0
        for id in $ids; do
            st=$(runner_state "$id")
            n=${#R_IDS[@]}
            R_IDS[n]=$id; R_PROJ[n]=$t; R_STATE[n]=$st
            R_PID[n]=""; R_STATUS[n]=""; R_JOB[n]=""; R_JOBSTART[n]=""
            if is_running "$id"; then
                R_PID[n]=$(cat "$PID_DIR/runner-$id.pid" 2>/dev/null || echo "")
                R_STATUS[n]=$(get_runner_status "$id")
                job=$(log_last_job "$id")
                if [[ -n "$job" ]]; then
                    R_JOB[n]=${job%%$'\t'*}; R_JOBSTART[n]=${job#*$'\t'}
                fi
            fi
            R_ERR[n]=$(get_lasterr "$id")
            R_EPH[n]=false
            is_ephemeral "$id" && R_EPH[n]=true
            R_DRAIN[n]=false; is_draining "$id" && R_DRAIN[n]=true
            R_QUAR[n]=false; is_quarantined "$id" && R_QUAR[n]=true
            R_NAME[n]=$(registered_name "$id")
            R_VER[n]=$(runner_version "$id")
            R_WORKMB[n]=$(cat "$PID_DIR/runner-$id.workmb" 2>/dev/null) || R_WORKMB[n]=""
            fails=$(cat "$PID_DIR/runner-$id.failcount" 2>/dev/null) || fails=""
            [[ "$fails" =~ ^[0-9]+$ ]] || fails=0
            R_FAILS[n]=$fails
            R_NEXT[n]=""
            if [[ "$st" == "restarting" ]]; then
                last=$(cat "$PID_DIR/runner-$id.laststart" 2>/dev/null) || last=""
                [[ "$last" =~ ^[0-9]+$ ]] && R_NEXT[n]=$(( last + 5 * (2 ** fails) ))
            fi
            case "$st" in
                draining) drain=$((drain + 1)) ;;
                busy) busy=$((busy + 1)); run=$((run + 1)); conf=$((conf + 1)) ;;
                running|idle) run=$((run + 1)); conf=$((conf + 1)) ;;
                *) conf=$((conf + 1)) ;;
            esac
        done
        [[ -n "$t" ]] || continue
        n=${#P_URLS[@]}
        P_URLS[n]=$t; P_CONF[n]=$conf; P_RUN[n]=$run; P_BUSY[n]=$busy; P_DRAIN[n]=$drain
        P_LISTED[n]=false
        target_in_file "$t" && P_LISTED[n]=true
        P_TYPE[n]=$(target_type "$t")
        key=$(target_key "$t")
        P_WANT[n]=$(cat "$PID_DIR/want-$key.txt" 2>/dev/null) || P_WANT[n]=""
        [[ "${P_WANT[n]}" =~ ^[0-9]+$ ]] || P_WANT[n]=$conf
        b=$(target_bounds "$t")
        P_MIN[n]=""; P_MAX[n]=""
        [[ -n "$b" ]] && { P_MIN[n]=${b% *}; P_MAX[n]=${b#* }; }
        P_QUEUED[n]=$(cached_queue_depth "$t")
        P_UNSAT[n]=$(cached_queue_unsatisfiable "$t")
        P_ERRC[n]=""; P_ERRM[n]=""; P_ERRS[n]=""
        e=$(cat "$(target_err_file "$t")" 2>/dev/null) || e=""
        if [[ -n "$e" ]]; then
            P_ERRC[n]=$(printf '%s\n' "$e" | cut -f1)
            P_ERRM[n]=$(printf '%s\n' "$e" | cut -f2)
            P_ERRS[n]=$(printf '%s\n' "$e" | cut -f3)
        fi
    done
    STATUS_NOW=$now
    return 0
}

json_bool() { if [[ "$1" == "true" || "$1" == "1" ]]; then printf 'true'; else printf 'false'; fi; }

# JSON string, or null when empty
json_str_null() { if [[ -n "$1" ]]; then json_str "$1"; else printf 'null'; fi; }

# JSON array of strings from a comma-separated list
json_str_list() {
    local item sep="" rest=$1
    printf '['
    while [[ -n "$rest" ]]; do
        item=${rest%%,*}
        if [[ "$rest" == *,* ]]; then rest=${rest#*,}; else rest=""; fi
        [[ -n "$item" ]] || continue
        printf '%s%s' "$sep" "$(json_str "$item")"
        sep=","
    done
    printf ']'
}

# state_json WRITER - the schema 2 state document (see docs / the Go TUI's
# internal/state), from the arrays status_collect filled. Reads files only:
# it never calls gh, so the daemon can write it every tick.
state_json() {
    local writer=${1:-cli} i sep dpid now k v tar tarver latest stale free pct
    local gst gmsg rate rrem="" rres="" host el w
    now=${STATUS_NOW:-$(date +%s)}
    if [[ "$DAEMON_MODE" == "1" ]]; then dpid=$$; else dpid=$(daemon_pid || true); fi
    tar=${RUNNER_TAR:-}
    [[ -n "$tar" ]] || tar=$(detect_runner_tarball 2>/dev/null || true)
    tarver=$(tarball_version "$tar")
    latest=$(cached_latest_runner_tag || true); latest=${latest#v}
    stale=false
    [[ -n "$tarver" && -n "$latest" && "$tarver" != "$latest" ]] && stale=true
    free=$(free_disk_mb); pct=$(disk_free_pct)
    gst=$(gh_state)
    if [[ -f "$PID_DIR/gh.err" ]]; then
        gmsg=$(cut -f3 "$PID_DIR/gh.err" 2>/dev/null) || gmsg=""
    else
        gmsg=$(cat "$PID_DIR/gh.msg" 2>/dev/null) || gmsg=""
    fi
    rate=$(cat "$PID_DIR/gh.rate" 2>/dev/null) || rate=""
    if [[ -n "$rate" ]]; then
        rrem=$(printf '%s\n' "$rate" | cut -f1); rres=$(printf '%s\n' "$rate" | cut -f2)
    fi
    host=$(cat "$PID_DIR/gh.host" 2>/dev/null) || host=""
    [[ -n "$host" ]] || host=${GH_HOST:-github.com}

    printf '{"schema":2,"version":%s,"writer":%s,"daemon_pid":%s,"tick_ts":%s,"refresh_interval":%s,"max_runners":%s,' \
        "$(json_str "$VERSION")" "$(json_str "$writer")" "$(json_num "$dpid")" "$now" \
        "$(json_num "$REFRESH_INTERVAL")" "$(json_num "$MAX_RUNNERS")"
    printf '"gh":{"user":%s,"host":%s,"scopes":%s,"state":%s,"message":%s,"checked":%s,"rate_remaining":%s,"rate_reset":%s,"polled_at":%s},' \
        "$(json_str_null "$(cat "$PID_DIR/gh.user" 2>/dev/null || true)")" "$(json_str "$host")" \
        "$(json_str_list "$(cat "$PID_DIR/gh.scopes" 2>/dev/null || true)")" \
        "$(json_str "$gst")" "$(json_str_null "$gmsg")" \
        "$(json_num "$(cat "$PID_DIR/gh.checked" 2>/dev/null || true)")" \
        "$(json_num "$rrem")" "$(json_num "$rres")" \
        "$(json_num "$(cat "$PID_DIR/gh-poll.ts" 2>/dev/null || true)")"
    printf '"tarball":{"version":%s,"latest":%s,"stale":%s},' \
        "$(json_str_null "$tarver")" "$(json_str_null "$latest")" "$stale"
    printf '"disk":{"free_mb":%s,"pct_free":%s},' "$(json_num "$free")" "$(json_num "$pct")"

    printf '"config":{'
    sep=""
    for k in $CONFIG_KEYS; do
        eval "v=\${$k:-}"
        case "$k" in
            RUNNER_NAME_PREFIX) printf '%s"%s":%s' "$sep" "$k" "$(json_str "$v")" ;;
            SHARED_TOOL_CACHE|EPHEMERAL_RUNNERS|AUTOSCALE) printf '%s"%s":%s' "$sep" "$k" "$(json_bool "$v")" ;;
            *) printf '%s"%s":%s' "$sep" "$k" "$(json_num "$v")" ;;
        esac
        sep=","
    done
    printf '},"warnings":['
    sep=""
    for w in ${CONFIG_WARNINGS[@]+"${CONFIG_WARNINGS[@]}"} ${STATE_WARNINGS[@]+"${STATE_WARNINGS[@]}"}; do
        printf '%s%s' "$sep" "$(json_str "$w")"
        sep=","
    done

    printf '],"targets":['
    sep=""
    for ((i = 0; i < ${#P_URLS[@]}; i++)); do
        printf '%s{"url":%s,"label":%s,"type":%s,"listed":%s,"want":%s,"have":%s,"running":%s,"busy":%s,"draining":%s,' \
            "$sep" "$(json_str "${P_URLS[i]}")" "$(json_str "$(target_label "${P_URLS[i]}")")" \
            "$(json_str "${P_TYPE[i]}")" "${P_LISTED[i]}" "${P_WANT[i]}" "${P_CONF[i]}" \
            "${P_RUN[i]}" "${P_BUSY[i]}" "${P_DRAIN[i]}"
        printf '"min":%s,"max":%s,"autoscale":%s,"queued":%s,"unsatisfiable":%s,"error":' \
            "$(json_num "${P_MIN[i]}")" "$(json_num "${P_MAX[i]}")" \
            "$([[ "$AUTOSCALE" == "1" && -n "${P_MIN[i]}" ]] && echo true || echo false)" \
            "$(json_num "${P_QUEUED[i]}")" "$(json_num "${P_UNSAT[i]}")"
        if [[ -n "${P_ERRC[i]}" ]]; then
            printf '{"class":%s,"message":%s,"since":%s}}' "$(json_str "${P_ERRC[i]}")" \
                "$(json_str "$(gh_error_note "${P_ERRC[i]}" "${P_ERRM[i]}")")" "$(json_num "${P_ERRS[i]}")"
        else
            printf 'null}'
        fi
        sep=","
    done

    printf '],"runners":['
    sep=""
    for ((i = 0; i < ${#R_IDS[@]}; i++)); do
        el=""
        [[ "${R_JOBSTART[i]}" =~ ^[0-9]+$ ]] && { el=$(( now - R_JOBSTART[i] )); [[ $el -ge 0 ]] || el=0; }
        printf '%s{"id":%s,"name":%s,"target":%s,"pid":%s,"state":%s,"status":%s,"job":%s,"job_started":%s,"elapsed":%s,"version":%s,' \
            "$sep" "${R_IDS[i]}" "$(json_str "${R_NAME[i]}")" "$(json_str_null "${R_PROJ[i]}")" \
            "$(json_num "${R_PID[i]}")" "$(json_str "${R_STATE[i]}")" "$(json_str "${R_STATUS[i]}")" \
            "$(json_str_null "${R_JOB[i]}")" "$(json_num "${R_JOBSTART[i]}")" "$(json_num "$el")" \
            "$(json_str_null "${R_VER[i]}")"
        printf '"ephemeral":%s,"draining":%s,"quarantined":%s,"fails":%s,"next_retry":%s,"lasterr":%s,"log_path":%s,"work_mb":%s}' \
            "${R_EPH[i]}" "${R_DRAIN[i]}" "${R_QUAR[i]}" "${R_FAILS[i]}" "$(json_num "${R_NEXT[i]}")" \
            "$(json_str_null "${R_ERR[i]}")" "$(json_str "$LOG_DIR/runner-${R_IDS[i]}.log")" \
            "$(json_num "${R_WORKMB[i]}")"
        sep=","
    done
    printf ']}\n'
}

# Warnings about the host, recomputed per snapshot
STATE_WARNINGS=()
state_warnings() {
    local pct
    STATE_WARNINGS=()
    if [[ -s "$ORPHANS_FILE" ]]; then
        STATE_WARNINGS[${#STATE_WARNINGS[@]}]="orphaned GitHub registrations need manual cleanup (see $(basename "$ORPHANS_FILE"))"
    fi
    pct=$(disk_free_pct)
    if [[ -n "$pct" && $pct -lt 15 ]]; then
        STATE_WARNINGS[${#STATE_WARNINGS[@]}]="low disk: ${pct}% free on the runners' filesystem"
    fi
    return 0
}

# write_state_snapshot WRITER - collect and atomically replace
# $PID_DIR/state.json (tmp + mv in the same directory). The temp name has
# the pid in it so the daemon and a CLI process never share one.
write_state_snapshot() {
    local tmp="$PID_DIR/state.json.tmp.$$"
    mkdir -p "$PID_DIR" 2>/dev/null || return 0
    status_collect || return 0
    state_warnings
    if state_json "${1:-cli}" > "$tmp" 2>/dev/null; then
        mv -f "$tmp" "$PID_DIR/state.json"
    else
        rm -f "$tmp"
    fi
    return 0
}

# --status --json: the snapshot file when it is fresh (younger than two
# ticks, written by a daemon/TUI or by the last CLI command), else nothing
# (returns 1) and the caller collects live
status_json_cached() {
    local f="$PID_DIR/state.json" ts now
    [[ -f "$f" ]] || return 1
    ts=$(sed -n 's/.*"tick_ts":\([0-9][0-9]*\).*/\1/p' "$f" 2>/dev/null | head -1) || ts=""
    [[ "$ts" =~ ^[0-9]+$ ]] || return 1
    now=$(date +%s)
    [[ $(( now - ts )) -lt $(( 2 * REFRESH_INTERVAL )) ]] || return 1
    cat "$f"
}

status_text() {
    local i j dpid detail
    if dpid=$(daemon_pid); then
        echo -e "${BOLD}gh-runnermaxxer $VERSION${NC}  daemon: ${GREEN}running (pid $dpid)${NC}"
    else
        echo -e "${BOLD}gh-runnermaxxer $VERSION${NC}  daemon: ${DIM}not running${NC}"
    fi
    [[ ${#R_IDS[@]} -eq 0 && ${#P_URLS[@]} -eq 0 ]] && { echo "No projects or runners configured"; return 0; }
    for ((i = 0; i <= ${#P_URLS[@]}; i++)); do
        # The extra last pass lists runners with no project, if any
        [[ $i -eq ${#P_URLS[@]} ]] && { [[ ${#R_IDS[@]} -gt 0 && -z "${R_PROJ[${#R_IDS[@]}-1]}" ]] || break; }
        echo ""
        if [[ $i -lt ${#P_URLS[@]} ]]; then
            echo -e "${BOLD}$(target_label "${P_URLS[i]}")${NC}  ${DIM}${P_URLS[i]}${NC}"
            echo "  configured ${P_CONF[i]}  running ${P_RUN[i]}  busy ${P_BUSY[i]}  draining ${P_DRAIN[i]}"
        fi
        for ((j = 0; j < ${#R_IDS[@]}; j++)); do
            if [[ $i -lt ${#P_URLS[@]} ]]; then
                [[ "${R_PROJ[j]}" == "${P_URLS[i]}" ]] || continue
            else
                [[ -z "${R_PROJ[j]}" ]] || continue
                [[ $j -eq 0 || -n "${R_PROJ[j-1]}" ]] && echo -e "${YELLOW}Unconfigured (incomplete setup)${NC}"
            fi
            detail=${R_STATUS[j]}
            [[ -n "${R_ERR[j]}" && "${R_STATE[j]}" != running && "${R_STATE[j]}" != idle && "${R_STATE[j]}" != busy ]] \
                && detail="${detail:+$detail - }${R_ERR[j]}"
            printf '  %-10s %-12s %-10s %s\n' "runner-${R_IDS[j]}" "${R_STATE[j]}" \
                "${R_PID[j]:+pid ${R_PID[j]}}" "$detail"
        done
    done
    return 0
}

# ----------------------------------------------------------------------------
# --scale and single-runner commands
# ----------------------------------------------------------------------------

# parse_scale_arg TARGET=N -> SCALE_URL, SCALE_N (TARGET in any --target form)
parse_scale_arg() {
    local t n
    [[ "$1" == *=* ]] || return 1
    t=${1%=*}; n=${1##*=}
    [[ "$n" =~ ^[0-9]+$ ]] || return 1
    SCALE_URL=$(target_entry_to_url "$t")
    valid_target_url "$SCALE_URL" || return 1
    SCALE_N=$((10#$n))
    return 0
}

# Runner total across every target after scaling: scale_total_after URL N
# [URL N...]. Later pairs win for the same target; targets not named keep
# their current size (draining runners excluded, as in scale_target).
scale_total_after() {
    local args=("$@") total=0 i j dup t
    for ((i = 0; i < ${#args[@]}; i += 2)); do
        dup=0
        for ((j = i + 2; j < ${#args[@]}; j += 2)); do
            same_target "${args[i]}" "${args[j]}" && dup=1
        done
        [[ $dup -eq 1 ]] || total=$((total + ${args[i+1]}))
    done
    for t in $(known_targets); do
        dup=0
        for ((i = 0; i < ${#args[@]}; i += 2)); do
            same_target "$t" "${args[i]}" && dup=1
        done
        [[ $dup -eq 1 ]] || total=$((total + $(count_runners_for_target "$t")))
    done
    echo "$total"
}

SCALE_ARGS=()
cli_scale() {
    local total i url n up=0 failed=0
    total=$(scale_total_after "${SCALE_ARGS[@]}")
    if [[ $total -gt $MAX_RUNNERS ]]; then
        echo -e "${RED}Error: that would make $total runners across all projects, over MAX_RUNNERS ($MAX_RUNNERS)${NC}" >&2
        return 1
    fi
    for ((i = 0; i < ${#SCALE_ARGS[@]}; i += 2)); do
        [[ ${SCALE_ARGS[i+1]} -gt $(count_runners_for_target "${SCALE_ARGS[i]}") ]] && up=1
    done
    if [[ $up -eq 1 ]]; then
        RUNNER_TAR=$(detect_runner_tarball)
        [[ -n "$RUNNER_TAR" ]] || die "No runner tarball for ${RUNNER_OS}-${RUNNER_ARCH} - run --download first"
        gh_auth_probe quiet || die "$(gh_error_note "$GH_CLASS" "$GH_MSG")"
    fi
    for ((i = 0; i < ${#SCALE_ARGS[@]}; i += 2)); do
        url=${SCALE_ARGS[i]}; n=${SCALE_ARGS[i+1]}
        # Like the menu: check access before registering anything
        if [[ $n -gt $(count_runners_for_target "$url") ]] && ! target_accessible "$url"; then
            gh_last_load
            echo -e "${RED}Error: can't use $(target_label "$url"): $(gh_error_note "$GH_CLASS" "$GH_MSG")${NC}" >&2
            failed=1
            continue
        fi
        add_target_to_file "$url"
        if [[ $(count_runners_for_target "$url") -eq $n ]]; then
            echo "  $(target_label "$url"): already at $n runner(s)"
            continue
        fi
        scale_target "$url" "$n" || failed=1
    done
    return $failed
}

# cli_runner_cmd drain|stop|start|remove ID
cli_runner_cmd() {
    local action=$1 id=${2#runner-}
    [[ "$id" =~ ^[0-9]+$ && -d "$RUNNER_BASE_DIR/runner-$id" ]] \
        || { echo -e "${RED}Error: no such runner: $2${NC}" >&2; return 2; }
    case "$action" in
        drain)
            mark_draining "$id"
            if is_busy "$id"; then
                echo "runner-$id is mid-job: draining, it will be removed when the job finishes"
            else
                remove_runner "$id"
                echo "runner-$id was not running a job: removed"
            fi
            ;;
        stop)
            stop_runner "$id"
            echo "runner-$id stopped (it stays down until --start)"
            ;;
        start)
            clear_failure_state "$id"
            start_runner "$id" || { echo -e "${RED}runner-$id failed to start: $(get_lasterr "$id")${NC}" >&2; return 1; }
            echo "runner-$id running (pid $(cat "$PID_DIR/runner-$id.pid" 2>/dev/null || echo "?"))"
            ;;
        remove)
            is_busy "$id" && warn "runner-$id is mid-job - killing it (use --drain to let the job finish)"
            remove_runner "$id"
            echo "runner-$id removed"
            ;;
    esac
    return 0
}

# --set-bounds TARGET=MIN-MAX | TARGET=none. Sets BOUNDS_URL, BOUNDS_MIN,
# BOUNDS_MAX (both empty for none). MAX_RUNNERS is checked later, once the
# config is loaded.
parse_bounds_arg() {
    local t b
    [[ "$1" == *=* ]] || return 1
    t=${1%=*}; b=${1##*=}
    BOUNDS_URL=$(target_entry_to_url "$t")
    valid_target_url "$BOUNDS_URL" || return 1
    if [[ "$b" == "none" ]]; then
        BOUNDS_MIN=""; BOUNDS_MAX=""
        return 0
    fi
    [[ "$b" =~ ^([0-9]{1,6})-([0-9]{1,6})$ ]] || return 1
    BOUNDS_MIN=$((10#${BASH_REMATCH[1]})); BOUNDS_MAX=$((10#${BASH_REMATCH[2]}))
    [[ $BOUNDS_MIN -le $BOUNDS_MAX ]] || return 1
    return 0
}

cli_add_target() {
    local url
    url=$(target_entry_to_url "$1")
    valid_target_url "$url" || { echo "Error: invalid target: $1 (expected owner/repo, org, or URL)" >&2; return 2; }
    if ! target_accessible "$url"; then
        gh_last_load
        echo -e "${RED}Error: can't use $(target_label "$url"): $(gh_error_note "$GH_CLASS" "$GH_MSG")${NC}" >&2
        return 1
    fi
    if target_in_file "$url"; then
        echo "$(target_label "$url") is already listed"
    else
        add_target_to_file "$url"
        echo "added $(target_label "$url")"
    fi
    return 0
}

cli_remove_target() {
    local url n key
    url=$(target_entry_to_url "$1")
    valid_target_url "$url" || { echo "Error: invalid target: $1 (expected owner/repo, org, or URL)" >&2; return 2; }
    n=$(count_runners_for_target "$url")
    if [[ $n -gt 0 ]]; then
        echo -e "${RED}Error: $(target_label "$url") still has $n runner(s) - run --scale $(target_label "$url" | cut -d' ' -f1)=0 first${NC}" >&2
        return 1
    fi
    remove_target_from_file "$url"
    key=$(target_key "$url")
    rm -f "$PID_DIR/want-$key.txt" "$PID_DIR/queue-$key.txt" "$PID_DIR/idle-since-$key" "$(target_err_file "$url")"
    echo "removed $(target_label "$url")"
    return 0
}

cli_set_bounds() {
    if [[ -n "$BOUNDS_MAX" && $BOUNDS_MAX -gt $MAX_RUNNERS ]]; then
        echo "Error: max $BOUNDS_MAX is over MAX_RUNNERS ($MAX_RUNNERS)" >&2
        return 2
    fi
    set_target_bounds "$BOUNDS_URL" "$BOUNDS_MIN" "$BOUNDS_MAX"
    if [[ -n "$BOUNDS_MIN" ]]; then
        echo "$(target_label "$BOUNDS_URL"): autoscale between $BOUNDS_MIN and $BOUNDS_MAX"
        [[ "$AUTOSCALE" == "1" ]] || echo -e "${DIM}Note: AUTOSCALE=0, so the bounds apply once it is on (--set-config AUTOSCALE=1)${NC}" >&2
    else
        echo "$(target_label "$BOUNDS_URL"): bounds cleared (fixed count)"
    fi
    return 0
}

# --set-config KEY=VALUE [...]: validate all, then save once
SETCFG_ARGS=()
cli_set_config() {
    local i k v w before=${#CONFIG_WARNINGS[@]}
    for ((i = 0; i < ${#SETCFG_ARGS[@]}; i += 2)); do
        config_set "${SETCFG_ARGS[i]}" "${SETCFG_ARGS[i+1]}" || return 2
    done
    sanitize_settings
    save_config quiet
    for ((i = 0; i < ${#SETCFG_ARGS[@]}; i += 2)); do
        k=${SETCFG_ARGS[i]}
        eval "v=\${$k:-}"
        echo "$k=$v"
    done
    for ((i = before; i < ${#CONFIG_WARNINGS[@]}; i++)); do
        w=${CONFIG_WARNINGS[i]}
        echo -e "${YELLOW}Note: $w${NC}" >&2
    done
    return 0
}

# --auth-check [--json]: read-only. 0 when gh works and no target lacks a scope
cli_auth_check() {
    local t e bad=0 sep="" out
    mkdir -p "$PID_DIR" 2>/dev/null || true
    if [[ $STATUS_JSON -eq 1 ]]; then
        gh_auth_probe quiet >/dev/null 2>&1 || bad=1
    else
        out=$(gh_auth_probe 2>&1) || bad=1
        [[ -n "$out" ]] && echo "$out"
        [[ $bad -eq 1 ]] && echo -e "${RED}✗ gh: $(gh_error_note "$(gh_state)" "$(cat "$PID_DIR/gh.msg" 2>/dev/null || true)")${NC}"
    fi
    for t in $(known_targets); do
        e=$(target_error "$t")
        [[ "${e%%$'\t'*}" == "scope" ]] && bad=1
    done
    if [[ $STATUS_JSON -eq 1 ]]; then
        printf '{"gh":{"user":%s,"host":%s,"scopes":%s,"state":%s,"message":%s},"targets":[' \
            "$(json_str_null "$(cat "$PID_DIR/gh.user" 2>/dev/null || true)")" \
            "$(json_str "$(cat "$PID_DIR/gh.host" 2>/dev/null || echo github.com)")" \
            "$(json_str_list "$(cat "$PID_DIR/gh.scopes" 2>/dev/null || true)")" \
            "$(json_str "$(gh_state)")" \
            "$(json_str_null "$(cat "$PID_DIR/gh.msg" 2>/dev/null || true)")"
        for t in $(known_targets); do
            e=$(target_error "$t")
            printf '%s{"url":%s,"label":%s,"error":' "$sep" "$(json_str "$t")" "$(json_str "$(target_label "$t")")"
            if [[ -n "$e" ]]; then
                printf '{"class":%s,"message":%s}}' "$(json_str "${e%%$'\t'*}")" \
                    "$(json_str "$(gh_error_note "${e%%$'\t'*}" "${e#*$'\t'}")")"
            else
                printf 'null}'
            fi
            sep=","
        done
        printf ']}\n'
    elif [[ $bad -eq 0 ]]; then
        echo -e "${GREEN}✓ every target has the access it needs${NC}"
    fi
    return $bad
}

# --relogin: interactive gh auth login with the scopes the runners need
cli_relogin() {
    local host
    if [[ ! -t 0 ]]; then
        echo "Error: --relogin needs a terminal (gh auth login is interactive)" >&2
        return 2
    fi
    host=$(cat "$PID_DIR/gh.host" 2>/dev/null) || host=""
    [[ -n "$host" ]] || host=${GH_HOST:-github.com}
    "$GH_BIN" auth login -h "$host" -s repo,admin:org,workflow || return 1
    rm -f "$PID_DIR/gh.err"
    mkdir -p "$PID_DIR" 2>/dev/null || true
    gh_auth_probe || return 1
    return 0
}

# --poll: ask the daemon to poll GitHub on its next tick, or poll now
cli_poll() {
    if [[ $CLI_SUPERVISED -eq 1 ]]; then
        touch "$PID_DIR/poll.request"
        echo "the daemon polls GitHub on its next tick"
        return 0
    fi
    rm -f "$PID_DIR/poll.request"
    check_github_health || true
    echo "polled GitHub"
    return 0
}

# --stop-all / --start-all (no prompt: scripts can't answer one)
cli_all() {
    local action=$1 id failed=0 any=0
    for id in $(get_runner_ids); do
        any=1
        if [[ "$action" == "stop" ]]; then
            is_busy "$id" && warn "runner-$id is mid-job - stopping it anyway"
            stop_runner "$id" || failed=1
            echo "runner-$id stopped"
        else
            clear_failure_state "$id"
            if start_runner "$id"; then
                echo "runner-$id running"
            else
                echo -e "${RED}runner-$id failed to start: $(get_lasterr "$id")${NC}" >&2
                failed=1
            fi
        fi
    done
    [[ $any -eq 1 ]] || echo "no runners"
    return $failed
}

CLI_CMD=""; CLI_RUNNER=""; CLI_ARG=""; STATUS_JSON=0
set_cli_cmd() {
    if [[ -n "$CLI_CMD" && "$CLI_CMD" != "$1" ]]; then
        echo "Error: --$CLI_CMD and --$1 can't be combined (one command at a time)" >&2
        exit 2
    fi
    CLI_CMD=$1
}

# Non-interactive commands. Exit status: 0 ok, 1 failed, 2 bad usage.
run_cli() {
    local rc=0
    load_config >&2     # may print a migration note; keep stdout clean for --json
    sanitize_settings
    case "$CLI_CMD" in
        status)
            if [[ $STATUS_JSON -eq 1 ]]; then
                status_json_cached || { status_collect; state_warnings; state_json cli; }
            else
                status_collect
                status_text
            fi
            ;;
        stop-daemon)       cli_stop_daemon || rc=$? ;;
        install-service)   install_service || rc=$? ;;
        uninstall-service) uninstall_service || rc=$? ;;
        auth-check)        cli_auth_check || rc=$? ;;
        relogin)           cli_relogin || rc=$? ;;
        gh-status)         github_status_body || rc=$? ;;
        *)
            [[ -f "$CONFIG_FILE" ]] || die "No configuration found - run --setup first"
            validate_config >&2 || die "Invalid configuration in $CONFIG_FILE - fix it or run --setup"
            mkdir -p "$RUNNER_BASE_DIR" "$PID_DIR" "$LOG_DIR"
            cli_lock
            case "$CLI_CMD" in
                scale)         cli_scale || rc=$? ;;
                add-target)    cli_add_target "$CLI_ARG" || rc=$? ;;
                remove-target) cli_remove_target "$CLI_ARG" || rc=$? ;;
                set-bounds)    cli_set_bounds || rc=$? ;;
                set-config)    cli_set_config || rc=$? ;;
                poll)          cli_poll || rc=$? ;;
                stop-all)      cli_all stop || rc=$? ;;
                start-all)     cli_all start || rc=$? ;;
                *)             cli_runner_cmd "$CLI_CMD" "$CLI_RUNNER" || rc=$? ;;
            esac
            # No daemon will refresh state.json after this change: do it here
            [[ $CLI_SUPERVISED -eq 0 ]] && write_state_snapshot cli
            if [[ $CLI_SUPERVISED -eq 0 && $rc -ne 2 ]] \
                && case "$CLI_CMD" in scale|start|start-all|drain|remove) true ;; *) false ;; esac; then
                echo -e "${DIM}Note: no daemon is running, so runners are not supervised (no auto-restart, drains not finished) - start one with --daemon or --install-service${NC}" >&2
            fi
            ;;
    esac
    return $rc
}

# ============================================================================
# Main
# ============================================================================

# Allow the script to be sourced as a library (e.g. by tests) without running
# the supervisor / argument parsing below. Functions, variables, colours and
# the `trap cleanup EXIT` above are all safe to define under this guard.
if [[ "${RUNNERMAXXER_LIB:-0}" == "1" ]]; then
    return 0 2>/dev/null || exit 0
fi

detect_platform

# Plain output when piped/scripted (and under --daemon, which logs to a file)
if [[ ! -t 1 ]]; then
    RED=''; GREEN=''; YELLOW=''; BLUE=''; CYAN=''; BOLD=''; DIM=''; NC=''
fi

# Handle command-line flags
SKIP_MENU=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --setup|-s)
            load_config
            run_onboarding
            echo -e "${GREEN}Setup complete. Run without --setup to start.${NC}"
            exit 0
            ;;
        --download|-d)
            download_runner_tarball || exit 1
            exit 0
            ;;
        --target|-t)
            [[ -n "${2:-}" ]] || die "--target needs a value (owner/repo, org, or URL)"
            CLI_TARGET="$2"
            shift
            ;;
        --no-menu)
            SKIP_MENU=1
            ;;
        --daemon)
            DAEMON_MODE=1
            ;;
        --status)
            set_cli_cmd status
            ;;
        --json)
            STATUS_JSON=1
            ;;
        --scale)
            [[ -n "${2:-}" ]] || { echo "Error: --scale needs TARGET=N" >&2; exit 2; }
            parse_scale_arg "$2" || { echo "Error: invalid --scale $2 (expected owner/repo=N, org=N, or URL=N)" >&2; exit 2; }
            set_cli_cmd scale
            SCALE_ARGS+=("$SCALE_URL" "$SCALE_N")
            shift
            ;;
        --drain|--stop|--start|--remove|--retry)
            [[ -n "${2:-}" ]] || { echo "Error: $1 needs a runner id" >&2; exit 2; }
            c=${1#--}; [[ "$c" == "retry" ]] && c=start     # --retry = --start
            set_cli_cmd "$c"
            CLI_RUNNER="$2"
            shift
            ;;
        --add-target|--remove-target)
            [[ -n "${2:-}" && "$2" != --* ]] || { echo "Error: $1 needs a target (owner/repo, org, or URL)" >&2; exit 2; }
            set_cli_cmd "${1#--}"
            CLI_ARG="$2"
            shift
            ;;
        --set-bounds)
            [[ -n "${2:-}" ]] || { echo "Error: --set-bounds needs TARGET=MIN-MAX or TARGET=none" >&2; exit 2; }
            parse_bounds_arg "$2" || { echo "Error: invalid --set-bounds $2 (expected owner/repo=MIN-MAX with MIN <= MAX, or owner/repo=none)" >&2; exit 2; }
            set_cli_cmd set-bounds
            shift
            ;;
        --set-config)
            [[ "${2:-}" == *=* ]] || { echo "Error: --set-config needs KEY=VALUE" >&2; exit 2; }
            case " $CONFIG_KEYS " in
                *" ${2%%=*} "*) ;;
                *) echo "Error: unknown setting '${2%%=*}' (settings: $CONFIG_KEYS)" >&2; exit 2 ;;
            esac
            set_cli_cmd set-config
            SETCFG_ARGS+=("${2%%=*}" "${2#*=}")
            shift
            ;;
        --stop-daemon|--install-service|--uninstall-service|--auth-check|--relogin|--poll|--gh-status|--stop-all|--start-all)
            set_cli_cmd "${1#--}"
            ;;
        --version|-v)
            echo "gh-runnermaxxer $VERSION"
            exit 0
            ;;
        --help|-h)
            echo "Usage: ./runnermaxxer.sh [OPTIONS]"
            echo ""
            echo "GitHub Actions Self-Hosted Runner Manager"
            echo ""
            echo "Options:"
            echo "  --setup, -s     Run interactive setup wizard"
            echo "  --download, -d  Download the latest runner tarball for this platform"
            echo "  --target, -t X  Add project X (owner/repo, org, or URL) to the list and highlight it"
            echo "  --no-menu       Skip the project menu at startup and go straight to the dashboard"
            echo "  --version, -v   Print version"
            echo "  --help, -h      Show this help message"
            echo ""
            echo "Headless:"
            echo "  --daemon             Supervise runners without a UI (events: runners/.logs/runnermaxxer.log)"
            echo "  --stop-daemon        Stop a running daemon (runners keep running)"
            echo "  --install-service    Run the daemon at login (launchd on macOS, systemd --user on Linux)"
            echo "  --uninstall-service  Remove that service"
            echo ""
            echo "Scripting (no UI; exit status 0 = ok, 1 = failed, 2 = bad usage):"
            echo "  --status [--json]    Projects and runners with their state"
            echo "  --scale TARGET=N     Set a project's runner count (repeatable; TARGET as for --target)"
            echo "  --drain ID           Remove runner ID once its current job finishes"
            echo "  --stop ID            Stop runner ID (stays down until --start)"
            echo "  --start ID           Start runner ID (clears quarantine)"
            echo "  --remove ID          Unregister and delete runner ID now"
            echo "  --retry ID           Same as --start (retry a quarantined runner)"
            echo "  --stop-all           Stop every runner (even mid-job)"
            echo "  --start-all          Start every runner (clears quarantine)"
            echo "  --add-target TARGET  Add a project after checking gh can reach it"
            echo "  --remove-target TARGET  Remove a project with no runners left"
            echo "  --set-bounds TARGET=MIN-MAX|TARGET=none  Autoscale bounds for a project"
            echo "  --set-config KEY=VALUE   Change a setting (repeatable; see .runnermaxxer.conf.sample)"
            echo "  --auth-check [--json]    Check gh login, scopes, and per-project access"
            echo "  --relogin            Run gh auth login with the scopes runners need (needs a terminal)"
            echo "  --poll               Poll GitHub now (or on the daemon's next tick)"
            echo "  --gh-status          List the runners GitHub has registered per project"
            echo ""
            echo "Configuration:"
            echo "  Copy .runnermaxxer.conf.sample to .runnermaxxer.conf"
            echo "  Or run with --setup for interactive configuration"
            echo "  Projects (repos/orgs) are listed in .runnermaxxer.targets; the startup"
            echo "  menu lets you add projects and set how many runners each one gets"
            echo ""
            exit 0
            ;;
        *)
            die "Unknown option: $1 (see --help)"
            ;;
    esac
    shift
done

if [[ $STATUS_JSON -eq 1 && "$CLI_CMD" != "status" && "$CLI_CMD" != "auth-check" ]]; then
    echo "Error: --json only goes with --status or --auth-check" >&2
    exit 2
fi
if [[ -n "$CLI_CMD" ]]; then
    [[ "$DAEMON_MODE" == "0" ]] || { echo "Error: --daemon can't be combined with --$CLI_CMD" >&2; exit 2; }
    rc=0
    run_cli || rc=$?
    exit $rc
fi

if pid=$(daemon_pid); then
    [[ "$DAEMON_MODE" == "1" ]] && die "a daemon is already running (pid $pid)"
    die "$(daemon_running_msg "$pid")"
fi

echo -e "${BOLD}${CYAN}gh-runnermaxxer${NC}"
echo -e "${DIM}GitHub Actions Self-Hosted Runner Manager${NC}"
echo ""

load_config

# Check if onboarding is needed
needs_onboarding=false

if [[ "$DAEMON_MODE" == "1" ]]; then
    # Headless: nobody to run the setup wizard
    [[ -f "$CONFIG_FILE" ]] || die "No configuration found at $CONFIG_FILE - run --setup first"
    validate_config || die "Invalid configuration in $CONFIG_FILE - fix it or run --setup"
elif [[ ! -f "$CONFIG_FILE" ]]; then
    echo -e "${YELLOW}No configuration found.${NC}"
    if [[ -f "$SCRIPT_DIR/.runnermaxxer.conf.sample" ]]; then
        echo -e "${DIM}Tip: Copy .runnermaxxer.conf.sample to .runnermaxxer.conf${NC}"
        echo -e "${DIM}     or continue below for interactive setup.${NC}"
        echo ""
    fi
    needs_onboarding=true
elif ! validate_config; then
    echo ""
    echo -e "${YELLOW}Configuration issues detected.${NC}"
    [[ -t 0 ]] || die "Invalid configuration in $CONFIG_FILE and no terminal to ask - fix it or run --setup"
    printf 'Would you like to run setup to fix them? [Y/n]: '
    read -r fix_choice
    if [[ ! "$fix_choice" =~ ^[Nn] ]]; then
        needs_onboarding=true
    fi
fi

if [[ "$needs_onboarding" == "true" ]]; then
    run_onboarding
    load_config
fi

cli_url=""
if [[ -n "$CLI_TARGET" ]]; then
    cli_url=$(target_entry_to_url "$CLI_TARGET")
    valid_target_url "$cli_url" || die "Invalid --target: $CLI_TARGET (expected owner/repo, org, or URL)"
    add_target_to_file "$cli_url"
fi

# Report bad lines in the targets file once, up front
load_targets verbose >/dev/null

echo -e "${DIM}Running preflight checks...${NC}"
preflight_checks
check_tarball_freshness || true

# Adopt/clean up state left behind by a previous manager instance
reconcile_state

sanitize_settings

# Labels can't change while running; detect_labels shells out to
# system_profiler etc., too slow to run every frame
CACHED_LABELS=$(detect_labels)

if [[ "$DAEMON_MODE" == "1" ]]; then
    run_daemon     # loops until SIGTERM/SIGINT
fi

TUI_ACTIVE=1

# Project menu: pick which repos/orgs get runners and how many
if [[ "$SKIP_MENU" -eq 0 ]]; then
    clear
    target_menu startup "$cli_url" || true
fi

clear
tput cnorm 2>/dev/null || true
SUPERVISOR_TICK=$((GH_HEALTH_TICKS - 1))   # first loop iteration polls GitHub
M_SEL=0; M_MSG=""; M_URLS=(); M_WANT=()
while true; do
    supervisor_tick
    menu_reload
    draw_ui || true
    printf '  > '
    key=$(read_key "$REFRESH_INTERVAL")
    [[ "$key" != "tick" ]] && M_MSG=""

    case "$key" in
        tick) ;;
        up|k|K)   [[ $M_SEL -gt 0 ]] && M_SEL=$((M_SEL - 1)) ;;
        down|j|J) [[ $M_SEL -lt $((${#M_URLS[@]} - 1)) ]] && M_SEL=$((M_SEL + 1)) ;;
        right|+|=) menu_adjust 1 ;;
        left|-|_)  menu_adjust -1 ;;
        [0-9])     menu_set "$key" ;;
        enter)     dashboard_apply ;;
        esc)       dashboard_discard ;;
        d|D) remove_runner_prompt || true ;;
        s|S) start_all || true ;;
        x|X) stop_all || true ;;
        r|R) restart_all || true ;;
        t|T|n|N) target_menu tui || true; M_URLS=(); M_WANT=() ;;
        l|L) view_logs || true ;;
        c|C) check_github_status || true; SUPERVISOR_TICK=$((GH_HEALTH_TICKS - 1)) ;;  # re-poll next iteration
        e|E) edit_config || true ;;
        q|Q) quit_prompt || true ;;
    esac
done