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

VERSION="2.0.0"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG_FILE="$SCRIPT_DIR/.runnermaxxer.conf"
TARGETS_FILE="$SCRIPT_DIR/.runnermaxxer.targets"

# Defaults (can be overridden by config file or environment)
RUNNER_BASE_DIR="${RUNNER_BASE_DIR:-$SCRIPT_DIR/runners}"
RUNNER_NAME_PREFIX="${RUNNER_NAME_PREFIX:-}"
REPO_URL="${REPO_URL:-}"
ORG_URL="${ORG_URL:-}"
MAX_RUNNERS="${MAX_RUNNERS:-20}"
REFRESH_INTERVAL="${REFRESH_INTERVAL:-5}"
MAX_RESTART_ATTEMPTS="${MAX_RESTART_ATTEMPTS:-5}"
MAX_LOG_SIZE_MB="${MAX_LOG_SIZE_MB:-10}"
GH_HEALTH_TICKS="${GH_HEALTH_TICKS:-12}"   # GitHub-side health check every N refresh ticks (0 = off)

PID_DIR="$RUNNER_BASE_DIR/.pids"
LOG_DIR="$RUNNER_BASE_DIR/.logs"
LOCK_FILE="$RUNNER_BASE_DIR/.runnermaxxer.lock"
ORPHANS_FILE="$RUNNER_BASE_DIR/.orphaned-registrations"

LOCK_ACQUIRED=0
TUI_ACTIVE=0
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
    tag=$(gh api repos/actions/runner/releases/latest --jq .tag_name 2>/dev/null) || {
        warn "Could not query the latest runner release (network/auth issue?)"
        return 1
    }

    local ver="${tag#v}"
    local asset="actions-runner-${RUNNER_OS}-${RUNNER_ARCH}-${ver}.tar.gz"

    info "Downloading $asset..."
    if ! gh release download "$tag" -R actions/runner --pattern "$asset" --dir "$SCRIPT_DIR" --clobber; then
        warn "Download failed. Get it manually from https://github.com/actions/runner/releases"
        return 1
    fi

    # Verify checksum against the SHA published in the release notes
    local expected actual
    expected=$(gh api "repos/actions/runner/releases/tags/$tag" --jq .body 2>/dev/null \
        | sed -n "s/.*BEGIN SHA ${RUNNER_OS}-${RUNNER_ARCH} -->\([a-f0-9]\{64\}\)<.*/\1/p" | head -1)
    if [[ -n "$expected" ]]; then
        actual=$(sha256_file "$SCRIPT_DIR/$asset")
        if [[ -n "$actual" && "$actual" != "$expected" ]]; then
            rm -f "$SCRIPT_DIR/$asset"
            warn "Checksum mismatch for $asset - deleted. Try again or download manually."
            return 1
        fi
        echo -e "${GREEN}✓${NC} Checksum verified"
    else
        warn "Could not find published checksum - skipping verification"
    fi

    echo -e "${GREEN}✓${NC} Downloaded $asset"
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

load_config() {
    if [[ -f "$CONFIG_FILE" ]]; then
        # shellcheck disable=SC1090
        source "$CONFIG_FILE"
    fi
    [[ -z "$RUNNER_NAME_PREFIX" ]] && RUNNER_NAME_PREFIX=$(default_hostname)
    return 0
}

# Strip trailing slashes and a .git suffix so pasted URLs validate
normalize_url() {
    local u="$1"
    u="${u%/}"
    u="${u%.git}"
    u="${u%/}"
    echo "$u"
}

valid_repo_url() { [[ "$1" =~ ^https://github\.com/[^/]+/[^/]+$ ]]; }
valid_org_url()  { [[ "$1" =~ ^https://github\.com/[^/]+$ ]]; }
valid_prefix()   { [[ "$1" =~ ^[a-zA-Z0-9_-]+$ ]]; }

validate_config() {
    local errors=()
    local warnings=()

    if [[ ! -f "$CONFIG_FILE" ]]; then
        errors[${#errors[@]}]="No configuration file found"
        return 1
    fi

    if [[ -n "$REPO_URL" && -n "$ORG_URL" ]]; then
        errors[${#errors[@]}]="Both REPO_URL and ORG_URL are set - only one should be configured"
    fi

    if [[ -z "$REPO_URL" && -z "$ORG_URL" ]]; then
        errors[${#errors[@]}]="No target configured - set either REPO_URL or ORG_URL"
    fi

    if [[ -n "$REPO_URL" ]] && ! valid_repo_url "$REPO_URL"; then
        errors[${#errors[@]}]="Invalid REPO_URL format: $REPO_URL"
        errors[${#errors[@]}]="Expected: https://github.com/owner/repo"
    fi

    if [[ -n "$ORG_URL" ]] && ! valid_org_url "$ORG_URL"; then
        errors[${#errors[@]}]="Invalid ORG_URL format: $ORG_URL"
        errors[${#errors[@]}]="Expected: https://github.com/org-name"
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
        errors[${#errors[@]}]="Use only letters, numbers, underscores, and hyphens"
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
    local tmp="$CONFIG_FILE.tmp.$$"
    cat > "$tmp" << EOF
# gh-runnermaxxer configuration
REPO_URL="$REPO_URL"
ORG_URL="$ORG_URL"
RUNNER_NAME_PREFIX="$RUNNER_NAME_PREFIX"
MAX_RUNNERS="$MAX_RUNNERS"
REFRESH_INTERVAL="$REFRESH_INTERVAL"
MAX_RESTART_ATTEMPTS="$MAX_RESTART_ATTEMPTS"
MAX_LOG_SIZE_MB="$MAX_LOG_SIZE_MB"
EOF
    mv "$tmp" "$CONFIG_FILE"
    echo -e "  ${GREEN}Configuration saved${NC}"
}

run_onboarding() {
    echo ""
    echo -e "${BOLD}${CYAN}╔═══════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║         gh-runnermaxxer Setup                     ║${NC}"
    echo -e "${BOLD}${CYAN}╚═══════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${DIM}Let's configure your self-hosted runner environment.${NC}"
    echo ""

    # Step 1: Target
    echo -e "${BOLD}Step 1: Runner Target${NC}"
    echo "  Where should runners register?"
    local saved_repo="$REPO_URL" saved_org="$ORG_URL"
    REPO_URL=""; ORG_URL=""
    if ! pick_target; then
        REPO_URL="$saved_repo"; ORG_URL="$saved_org"
        echo -e "\n  ${RED}No target chosen.${NC}"
        echo -e "  ${DIM}Run the script again to retry.${NC}"
        exit 1
    fi
    local new_repo_url="$REPO_URL" new_org_url="$ORG_URL"
    REPO_URL="$saved_repo"; ORG_URL="$saved_org"

    # Step 2: Runner name prefix
    echo ""
    echo -e "${BOLD}Step 2: Runner Name Prefix${NC}"
    echo ""
    local default_prefix
    default_prefix=$(default_hostname)
    echo -e "  Runners will be named: prefix-1, prefix-2, etc."
    echo -e "  ${DIM}Default: $default_prefix${NC}"
    printf '  Runner name prefix [%s]: ' "$default_prefix"
    read -r new_prefix
    [[ -z "$new_prefix" ]] && new_prefix="$default_prefix"

    if ! valid_prefix "$new_prefix"; then
        echo -e "\n  ${RED}Invalid characters. Use only letters, numbers, underscores, hyphens.${NC}"
        exit 1
    fi

    # Step 3: Max runners
    echo ""
    echo -e "${BOLD}Step 3: Maximum Runners${NC}"
    echo ""
    echo -e "  How many runners can run simultaneously?"
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
    if [[ -n "$new_repo_url" ]]; then
        echo -e "  Target:     ${GREEN}$new_repo_url${NC} (repository)"
    else
        echo -e "  Target:     ${GREEN}$new_org_url${NC} (organization)"
    fi
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

    REPO_URL="$new_repo_url"
    ORG_URL="$new_org_url"
    RUNNER_NAME_PREFIX="$new_prefix"
    MAX_RUNNERS="$new_max"
    save_config

    echo ""
    echo -e "  ${GREEN}✓ Configuration saved to .runnermaxxer.conf${NC}"
    echo ""
    sleep 1
}

get_target_url() {
    if [[ -n "$REPO_URL" ]]; then
        echo "$REPO_URL"
    elif [[ -n "$ORG_URL" ]]; then
        echo "$ORG_URL"
    else
        echo ""
    fi
}

get_target_type() {
    if [[ -n "$REPO_URL" ]]; then
        echo "repo"
    elif [[ -n "$ORG_URL" ]]; then
        echo "org"
    else
        echo ""
    fi
}

get_api_endpoint() {
    local target_url target_type api_path
    target_url=$(get_target_url)
    target_type=$(get_target_type)
    [[ -z "$target_url" ]] && { echo ""; return 0; }
    api_path="${target_url#https://github.com/}"
    if [[ "$target_type" == "repo" ]]; then
        echo "repos/$api_path/actions/runners"
    else
        echo "orgs/$api_path/actions/runners"
    fi
}

# ============================================================================
# Target selection
# ============================================================================
# .runnermaxxer.targets lists candidate targets, one per line, as
# owner/repo, org-name, or a full https://github.com/... URL. Blank lines
# and '#' comments are ignored. Startup shows these as a menu.

# Expand a targets-file entry to a full URL (empty for blanks/comments)
target_entry_to_url() {
    local e="$1"
    e="${e#"${e%%[![:space:]]*}"}"
    e="${e%"${e##*[![:space:]]}"}"
    [[ -z "$e" || "$e" == \#* ]] && { echo ""; return 0; }
    case "$e" in
        https://github.com/*) ;;
        http://github.com/*)  e="https://${e#http://}" ;;
        github.com/*)         e="https://$e" ;;
        *)                    e="https://github.com/$e" ;;
    esac
    normalize_url "$e"
}

# Print valid target URLs from the targets file, one per line
load_targets() {
    local line url
    [[ -f "$TARGETS_FILE" ]] || return 0
    while IFS= read -r line || [[ -n "$line" ]]; do
        url=$(target_entry_to_url "$line")
        [[ -z "$url" ]] && continue
        if valid_repo_url "$url" || valid_org_url "$url"; then
            echo "$url"
        else
            warn "Ignoring invalid entry in $(basename "$TARGETS_FILE"): $line"
        fi
    done < "$TARGETS_FILE"
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

# Set REPO_URL/ORG_URL from a URL or owner/repo entry. 1 if not valid.
set_target() {
    local url
    url=$(target_entry_to_url "$1")
    if valid_repo_url "$url"; then
        REPO_URL="$url"; ORG_URL=""
    elif valid_org_url "$url"; then
        ORG_URL="$url"; REPO_URL=""
    else
        return 1
    fi
    return 0
}

# Interactive picker. Shows the targets file as a numbered menu with the
# current target preselected, plus a manual-entry option. Sets
# REPO_URL/ORG_URL. Returns 0 when a target is set, 1 when nothing chosen.
pick_target() {
    local current targets=() t i choice url
    current=$(get_target_url)

    while IFS= read -r t; do
        [[ -n "$t" ]] && targets[${#targets[@]}]="$t"
    done < <(load_targets)

    echo ""
    echo -e "  ${BOLD}Runner target${NC}"
    if [[ ${#targets[@]} -eq 0 ]]; then
        echo -e "  ${DIM}Tip: list repos/orgs in $(basename "$TARGETS_FILE") to pick from a menu${NC}"
    else
        echo ""
        i=0
        local default_idx=""
        for t in "${targets[@]}"; do
            i=$((i + 1))
            if [[ "$t" == "$current" ]]; then
                default_idx=$i
                echo -e "  ${CYAN}$i${NC}) $(target_label "$t") ${GREEN}(current)${NC}"
            else
                echo -e "  ${CYAN}$i${NC}) $(target_label "$t")"
            fi
        done
        echo -e "  ${CYAN}m${NC}) Enter a repository or organization manually"
        echo ""
        if [[ -n "$default_idx" ]]; then
            printf '  Choice [%s]: ' "$default_idx"
        elif [[ -n "$current" ]]; then
            echo -e "  ${DIM}Current target $(target_label "$current") is not in the list; Enter keeps it${NC}"
            printf '  Choice [keep current]: '
        else
            printf '  Choice: '
        fi
        read -r choice || true

        if [[ -z "$choice" ]]; then
            [[ -n "$current" ]] && return 0
            echo -e "  ${YELLOW}No target selected${NC}"
            return 1
        fi
        if [[ "$choice" =~ ^[0-9]+$ ]] && [[ "$choice" -ge 1 && "$choice" -le ${#targets[@]} ]]; then
            set_target "${targets[$((choice - 1))]}"
            return 0
        fi
        if [[ ! "$choice" =~ ^[Mm]$ ]]; then
            echo -e "  ${YELLOW}Invalid choice${NC}"
            [[ -n "$current" ]] && return 0
            return 1
        fi
    fi

    echo ""
    echo -e "  ${DIM}owner/repo, org-name, or https://github.com/...${NC}"
    if [[ -n "$current" ]]; then
        printf '  Target [%s]: ' "$(target_label "$current")"
    else
        printf '  Target: '
    fi
    read -r url || true
    if [[ -z "$url" ]]; then
        [[ -n "$current" ]] && return 0
        echo -e "  ${YELLOW}No target entered${NC}"
        return 1
    fi
    if ! set_target "$url"; then
        echo -e "  ${YELLOW}Invalid target (expected owner/repo, org-name, or a github.com URL)${NC}"
        [[ -n "$current" ]] && return 0
        return 1
    fi
    return 0
}

# URL a runner is registered to, from its .runner state file
runner_registered_url() {
    sed -n 's/.*"gitHubUrl": *"\([^"]*\)".*/\1/p' "$RUNNER_BASE_DIR/runner-$1/.runner" 2>/dev/null | head -1 || true
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# Runners keep serving whatever they were registered to, regardless of the
# config file. After a target change, unregister stale runners from the old
# target and set them up again on the new one; those that were up come back
# up, those that were down stay down.
retarget_runners() {
    local target_url id reg stale=() busy=""
    target_url=$(get_target_url)
    [[ -z "$target_url" ]] && return 0

    for id in $(get_runner_ids); do
        reg=$(runner_registered_url "$id")
        [[ -z "$reg" ]] && continue
        if [[ "$(lower "$reg")" != "$(lower "$target_url")" ]]; then
            stale[${#stale[@]}]="$id"
            is_busy "$id" && busy="$busy runner-$id"
        fi
    done
    [[ ${#stale[@]} -eq 0 ]] && return 0

    echo ""
    echo -e "  ${YELLOW}${#stale[@]} runner(s) are registered to a different target"\
            "(e.g. $(runner_registered_url "${stale[0]}"))${NC}"
    [[ -n "$busy" ]] && echo -e "  ${YELLOW}Jobs in progress on:$busy - they will be interrupted${NC}"
    printf '  Re-register them to %s? [Y/n]: ' "$(target_label "$target_url")"
    local ans=""
    read -r ans || true
    if [[ "$ans" =~ ^[Nn] ]]; then
        echo -e "  ${DIM}Left as-is: they keep serving their old target until re-registered${NC}"
        sleep 1
        return 0
    fi

    local was_running ok=0 failed=0
    for id in "${stale[@]}"; do
        was_running=0
        is_running "$id" && was_running=1
        echo -e "  Re-registering runner-$id..."
        remove_runner "$id"
        if setup_runner "$id"; then
            ok=$((ok + 1))
            if [[ "$was_running" == "1" ]]; then
                clear_failure_state "$id"
                start_runner "$id" || true
            else
                mark_stopped "$id"
            fi
        else
            failed=$((failed + 1))
        fi
    done
    if [[ $failed -gt 0 ]]; then
        echo -e "  ${GREEN}$ok re-registered${NC}, ${RED}$failed failed${NC}"
    else
        echo -e "  ${GREEN}$ok re-registered${NC}"
    fi
    sleep 1
    return 0
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
    for cmd in gh tar pgrep pkill ps; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            echo -e "${RED}✗ Missing required command: $cmd${NC}" >&2
            errors=$((errors + 1))
        fi
    done

    # gh authentication
    if command -v gh >/dev/null 2>&1; then
        if ! gh auth status >/dev/null 2>&1; then
            echo -e "${RED}✗ gh CLI is not authenticated. Run: gh auth login${NC}" >&2
            errors=$((errors + 1))
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
        read -r dl_choice || true
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

    # Target URL
    if [[ -z "$(get_target_url)" ]]; then
        echo -e "${YELLOW}! No repository or organization configured${NC}" >&2
        echo -e "${DIM}  Press 'e' to configure after startup${NC}" >&2
    fi

    if [[ $errors -gt 0 ]]; then
        die "Preflight checks failed ($errors error(s))"
    fi

    if ! acquire_lock; then
        local lock_pid
        lock_pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "?")
        die "Another instance is running (PID $lock_pid)"
    fi
}

get_pat() {
    local token
    token=$(gh auth token 2>/dev/null) || return 1
    [[ -z "$token" ]] && return 1
    echo "$token"
}

validate_token() {
    local target_url target_type api_path
    target_url=$(get_target_url)
    target_type=$(get_target_type)

    [[ -z "$target_url" ]] && return 1

    api_path="${target_url#https://github.com/}"
    if [[ "$target_type" == "repo" ]]; then
        gh api "repos/$api_path" >/dev/null 2>&1 || return 1
    elif [[ "$target_type" == "org" ]]; then
        gh api "orgs/$api_path" >/dev/null 2>&1 || return 1
    fi
    return 0
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

is_quarantined() {
    [[ -f "$PID_DIR/runner-$1.quarantined" ]]
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
}

get_lasterr() {
    cat "$PID_DIR/runner-$1.lasterr" 2>/dev/null || echo ""
}

# Give the runner a clean slate (used on manual start/restart)
clear_failure_state() {
    rm -f "$PID_DIR/runner-$1.failcount" \
          "$PID_DIR/runner-$1.quarantined" \
          "$PID_DIR/runner-$1.lasterr" \
          "$PID_DIR/runner-$1.ghoffline"
}

clear_runner_state() {
    clear_failure_state "$1"
    rm -f "$PID_DIR/runner-$1.pid" \
          "$PID_DIR/runner-$1.stopped" \
          "$PID_DIR/runner-$1.laststart"
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
        real=$(runner_procs "$id" | head -1)
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

get_runner_status() {
    local id=$1
    local log_file="$LOG_DIR/runner-$id.log"

    [[ ! -f "$log_file" ]] && { echo "no logs"; return 0; }

    local recent
    recent=$(tail -50 "$log_file" 2>/dev/null || echo "")
    [[ -z "$recent" ]] && { echo "no logs"; return 0; }

    if echo "$recent" | grep -q "Running job:"; then
        local job_name
        job_name=$(echo "$recent" | grep "Running job:" | tail -1 | sed 's/.*Running job: //' | cut -c1-25)
        echo "running: $job_name"
    elif echo "$recent" | grep -q "Listening for Jobs"; then
        echo "idle"
    elif echo "$recent" | grep -q "Job .* completed"; then
        echo "idle (done)"
    elif echo "$recent" | grep -q "Could not connect"; then
        echo "connection error"
    elif echo "$recent" | grep -q "Authentication failed"; then
        echo "auth error"
    elif echo "$recent" | grep -q "Starting Runner listener"; then
        echo "starting..."
    elif echo "$recent" | grep -q "Exiting runner"; then
        echo "exiting"
    else
        echo "unknown"
    fi
}

is_busy() {
    is_running "$1" && [[ "$(get_runner_status "$1")" == running:* ]]
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
    [[ -z "$(get_target_url)" ]] && return 0
    local now id
    now=$(date +%s)

    for id in $(get_runner_ids); do
        rotate_log "$id"
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
        start_runner "$id" >/dev/null 2>&1 || true
    done
    return 0
}

# Cross-check local state against GitHub: a runner process can be alive
# while GitHub considers it offline (wedged listener, revoked credentials,
# network partition that never recovered). Two consecutive offline sightings
# → recycle the process. Skipped entirely when the API is unreachable so a
# GitHub/network outage doesn't trigger mass restarts.
check_github_health() {
    local endpoint online id name cnt now started
    endpoint=$(get_api_endpoint)
    [[ -z "$endpoint" ]] && return 0

    online=$(gh api --paginate "$endpoint" --jq '.runners[] | select(.status == "online") | .name' 2>/dev/null) || return 0
    now=$(date +%s)

    for id in $(get_runner_ids); do
        local off_file="$PID_DIR/runner-$id.ghoffline"
        if ! is_running "$id"; then
            rm -f "$off_file"
            continue
        fi
        # Grace period: a freshly started runner may not show online yet
        started=$(cat "$PID_DIR/runner-$id.laststart" 2>/dev/null || echo 0)
        [[ "$started" =~ ^[0-9]+$ ]] || started=0
        [[ $((now - started)) -lt 120 ]] && continue

        name=$(registered_name "$id")
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
    return 0
}

# ============================================================================
# Runner lifecycle
# ============================================================================

setup_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"
    local target_url labels_str

    # Configured already? (.runner is written by config.sh on success -
    # a bare directory can be a leftover from an interrupted setup)
    [[ -f "$runner_dir/.runner" ]] && return 0
    if [[ -d "$runner_dir" ]]; then
        warn "runner-$id directory exists but is not configured - rebuilding"
        rm -rf "$runner_dir"
    fi

    target_url=$(get_target_url)
    [[ -z "$target_url" ]] && { warn "No target URL configured"; return 1; }

    local disk_mb
    disk_mb=$(free_disk_mb)
    if [[ "$disk_mb" -lt 500 ]]; then
        warn "Refusing to set up runner-$id: only ${disk_mb}MB disk free"
        return 1
    fi

    local pat
    pat=$(get_pat) || { warn "Failed to get token - run: gh auth login"; return 1; }

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
        --pat "$pat"
        --replace
    )

    if [[ -n "$labels_str" ]]; then
        config_args+=(--labels "${labels_str// /,}")
    fi

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
            (cd "$runner_dir" && ./config.sh remove --pat "$pat" >/dev/null 2>&1) \
                || echo "${RUNNER_NAME_PREFIX}-${id}" >> "$ORPHANS_FILE"
        fi
        rm -rf "$runner_dir"
        return 1
    fi

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
    orphan=$(runner_procs "$id" | head -1)
    if [[ -n "$orphan" ]]; then
        echo "$orphan" > "$pid_file"
        return 0
    fi

    [[ ! -d "$runner_dir" ]] && { set_lasterr "$id" "runner directory missing"; warn "Runner directory not found"; return 1; }
    [[ ! -f "$runner_dir/.runner" ]] && { set_lasterr "$id" "not configured (incomplete setup)"; warn "runner-$id is not configured"; return 1; }
    [[ ! -x "$runner_dir/run.sh" ]] && { set_lasterr "$id" "run.sh not found/executable"; warn "run.sh not found/executable"; return 1; }

    rotate_log "$id"
    date +%s > "$PID_DIR/runner-$id.laststart"

    # Launch with the runner dir as an absolute path on the command line so
    # is_running/runner_procs can identify the process family later.
    # setsid (where available) detaches it into its own session.
    local pid
    if command -v setsid >/dev/null 2>&1; then
        ( cd "$runner_dir" && exec setsid nohup "$runner_dir/run.sh" >> "$log_file" 2>&1 < /dev/null ) &
    else
        ( cd "$runner_dir" && exec nohup "$runner_dir/run.sh" >> "$log_file" 2>&1 < /dev/null ) &
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
}

remove_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"

    stop_runner "$id"

    if [[ -d "$runner_dir" ]]; then
        if [[ -f "$runner_dir/.runner" ]]; then
            local pat unregistered=0 reg_name
            reg_name=$(registered_name "$id")
            if pat=$(get_pat 2>/dev/null); then
                # --pat (not --token): config.sh exchanges the PAT for a
                # removal token itself, same as during setup.
                if (cd "$runner_dir" && ./config.sh remove --pat "$pat" >/dev/null 2>&1); then
                    unregistered=1
                fi
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
}

# ============================================================================
# UI
# ============================================================================

render_ui() {
    local total running target_url disk_mb

    total=$(get_runner_count)
    running=$(count_running)
    target_url=$(get_target_url)

    echo -e "${BOLD}${CYAN}"
    echo "  ╔═══════════════════════════════════════════════════╗"
    echo "  ║            gh-runnermaxxer                        ║"
    echo -e "  ╚═══════════════════════════════════════════════════╝${NC}"
    echo ""

    if [[ -n "$target_url" ]]; then
        echo -e "  ${DIM}Target: $target_url${NC}"
    else
        echo -e "  ${YELLOW}Target: Not configured (press 'e' to set up)${NC}"
    fi

    echo -e "  ${DIM}Labels: ${CACHED_LABELS}${NC}"
    echo ""
    echo -e "  ${BOLD}Status:${NC} ${GREEN}$running running${NC} / $total configured"

    disk_mb=$(free_disk_mb)
    if [[ "$disk_mb" -lt 1024 ]]; then
        echo -e "  ${RED}⚠ Low disk space: ${disk_mb}MB free${NC}"
    fi
    if [[ -s "$ORPHANS_FILE" ]]; then
        echo -e "  ${YELLOW}⚠ Orphaned GitHub registrations need manual cleanup (see $(basename "$ORPHANS_FILE"))${NC}"
    fi
    echo ""

    if [[ $total -gt 0 ]]; then
        echo -e "  ${BOLD}Runners:${NC}"
        local id
        for id in $(get_runner_ids); do
            if is_running "$id"; then
                local pid status status_color
                pid=$(cat "$PID_DIR/runner-$id.pid" 2>/dev/null || echo "?")
                status=$(get_runner_status "$id")
                status_color="${DIM}"

                [[ "$status" == running:* ]] && status_color="${CYAN}"
                [[ "$status" == "idle"* ]] && status_color="${DIM}"
                [[ "$status" == *"error"* ]] && status_color="${RED}"

                echo -e "    ${GREEN}●${NC} runner-$id ${DIM}PID $pid${NC} ${status_color}[$status]${NC}"
            elif is_marked_stopped "$id"; then
                echo -e "    ${RED}○${NC} runner-$id ${DIM}stopped${NC}"
            elif is_quarantined "$id"; then
                echo -e "    ${RED}✖${NC} runner-$id ${RED}quarantined${NC} ${DIM}$(get_lasterr "$id")${NC}"
            else
                local err
                err=$(get_lasterr "$id")
                if [[ -n "$err" ]]; then
                    echo -e "    ${YELLOW}◌${NC} runner-$id ${YELLOW}restarting...${NC} ${DIM}($err)${NC}"
                else
                    echo -e "    ${YELLOW}◌${NC} runner-$id ${YELLOW}restarting...${NC}"
                fi
            fi
        done
    else
        echo -e "  ${DIM}No runners configured${NC}"
    fi

    echo ""
    echo -e "  ${BOLD}────────────────────────────────────────────────────${NC}"
    echo -e "  ${BOLD}Commands:${NC}"
    echo -e "    ${CYAN}+${NC}  Add runner          ${CYAN}-${NC}  Remove runner"
    echo -e "    ${CYAN}s${NC}  Start all           ${CYAN}x${NC}  Stop all"
    echo -e "    ${CYAN}r${NC}  Restart all         ${CYAN}l${NC}  View logs"
    echo -e "    ${CYAN}n${NC}  Scale to N          ${CYAN}c${NC}  Check GitHub status"
    echo -e "    ${CYAN}e${NC}  Edit config         ${CYAN}q${NC}  Quit"
    echo ""
    echo -e "  ${DIM}Auto-refreshes every ${REFRESH_INTERVAL}s${NC}"
}

draw_ui() {
    # Render the full frame off-screen first, then repaint in place.
    # Clearing each line's tail (\033[K) and everything below the frame
    # (\033[J) replaces `clear`, so the screen never goes blank between
    # frames.
    local frame
    frame=$(render_ui)
    frame="${frame//$'\n'/$'\033[K\n'}"
    printf '\033[H%s\033[K\n\033[J' "$frame"
}

next_free_id() {
    local next_id=1
    while [[ -d "$RUNNER_BASE_DIR/runner-$next_id" ]]; do next_id=$((next_id + 1)); done
    echo "$next_id"
}

add_runner() {
    local target_url
    target_url=$(get_target_url)

    if [[ -z "$target_url" ]]; then
        echo -e "\n  ${YELLOW}Configure a target first (press 'e')${NC}"
        sleep 2
        return 1
    fi

    if [[ "$(get_runner_count)" -ge "$MAX_RUNNERS" ]]; then
        echo -e "\n  ${YELLOW}Already at MAX_RUNNERS ($MAX_RUNNERS)${NC}"
        sleep 2
        return 1
    fi

    local next_id
    next_id=$(next_free_id)

    echo -e "\n  ${BLUE}Setting up runner-$next_id...${NC}"
    if ! setup_runner "$next_id"; then
        echo -e "  ${RED}Failed to setup runner-$next_id${NC}"
        sleep 2
        return 1
    fi

    echo -e "  ${BLUE}Starting runner-$next_id...${NC}"
    clear_failure_state "$next_id"
    if ! start_runner "$next_id"; then
        echo -e "  ${RED}Failed to start runner-$next_id${NC}"
        sleep 2
        return 1
    fi

    echo -e "  ${GREEN}Done!${NC}"
    sleep 1
}

remove_runner_prompt() {
    local ids
    ids=$(get_runner_ids)
    [[ -z "$ids" ]] && { echo -e "\n  ${YELLOW}No runners${NC}"; sleep 1; return 0; }

    printf '\n  Remove which runner? [%s]: ' "$(echo "$ids" | tr '\n' ' ')"
    read -r id

    [[ -z "$id" ]] && return 0
    [[ ! "$id" =~ ^[0-9]+$ ]] && { echo -e "  ${YELLOW}Invalid ID${NC}"; sleep 1; return 0; }
    [[ ! -d "$RUNNER_BASE_DIR/runner-$id" ]] && { echo -e "  ${YELLOW}Not found${NC}"; sleep 1; return 0; }

    if is_busy "$id"; then
        printf '  %b' "${YELLOW}runner-$id is running a job - remove anyway? [y/N]: ${NC}"
        local sure
        read -r sure
        [[ "$sure" =~ ^[Yy] ]] || { echo -e "  ${DIM}Cancelled${NC}"; sleep 1; return 0; }
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

scale_to() {
    local target_url
    target_url=$(get_target_url)

    if [[ -z "$target_url" ]]; then
        echo -e "\n  ${YELLOW}Configure a target first (press 'e')${NC}"
        sleep 2
        return 0
    fi

    printf '\n  Scale to how many runners?: '
    read -r count
    [[ ! "$count" =~ ^[0-9]+$ ]] && { echo -e "  ${YELLOW}Invalid number${NC}"; sleep 1; return 0; }

    if [[ $count -gt $MAX_RUNNERS ]]; then
        echo -e "  ${YELLOW}Max $MAX_RUNNERS runners${NC}"
        sleep 1
        return 0
    fi

    local current failed=0
    current=$(get_runner_count)

    if [[ $count -gt $current ]]; then
        local to_add=$((count - current))
        echo -e "  ${BLUE}Adding $to_add runners...${NC}"
        local i
        for ((i=0; i<to_add; i++)); do
            local next_id
            next_id=$(next_free_id)
            echo -e "    Setting up runner-$next_id..."
            clear_failure_state "$next_id"
            if setup_runner "$next_id" && start_runner "$next_id"; then
                echo -e "    ${GREEN}✓${NC} runner-$next_id"
            else
                echo -e "    ${RED}✗${NC} runner-$next_id failed"
                failed=$((failed + 1))
            fi
        done
    elif [[ $count -lt $current ]]; then
        local to_remove=$((current - count))
        echo -e "  ${BLUE}Removing $to_remove runners...${NC}"

        # Prefer idle/stopped runners as victims; only touch busy ones if
        # we must, and highest ID first within each class.
        local idle_ids="" busy_ids="" id
        for id in $(get_runner_ids | sort -rn); do
            if is_busy "$id"; then
                busy_ids="$busy_ids $id"
            else
                idle_ids="$idle_ids $id"
            fi
        done

        local victims
        victims=$(echo "$idle_ids $busy_ids" | tr ' ' '\n' | grep -v '^$' | head -n "$to_remove")

        for id in $victims; do
            if is_busy "$id"; then
                echo -e "    ${YELLOW}⚠ runner-$id is mid-job - its job will be aborted${NC}"
            fi
            echo -e "    Removing runner-$id..."
            remove_runner "$id"
        done
    fi

    if [[ $failed -gt 0 ]]; then
        echo -e "  ${YELLOW}Done with $failed failure(s)${NC}"
    else
        echo -e "  ${GREEN}Done!${NC}"
    fi
    sleep 1
}

view_logs() {
    local ids
    ids=$(get_runner_ids)
    [[ -z "$ids" ]] && { echo -e "\n  ${YELLOW}No runners${NC}"; sleep 1; return 0; }

    printf '\n  View logs for which runner? [%s]: ' "$(echo "$ids" | tr '\n' ' ')"
    read -r id

    [[ -z "$id" ]] && return 0
    [[ ! "$id" =~ ^[0-9]+$ ]] && { echo -e "  ${YELLOW}Invalid ID${NC}"; sleep 1; return 0; }

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
    local endpoint
    endpoint=$(get_api_endpoint)

    [[ -z "$endpoint" ]] && { echo -e "\n  ${YELLOW}No target configured${NC}"; sleep 2; return 0; }

    echo -e "\n  ${BLUE}Checking GitHub runner status...${NC}"

    if ! gh api --paginate "$endpoint" --jq '.runners[] | "  \(.name): \(.status)"' 2>/dev/null; then
        echo -e "  ${YELLOW}Could not fetch (may need admin access)${NC}"
    fi

    if [[ -s "$ORPHANS_FILE" ]]; then
        echo ""
        echo -e "  ${YELLOW}Orphaned registrations recorded locally (failed unregisters):${NC}"
        sed 's/^/    /' "$ORPHANS_FILE"
        echo -e "  ${DIM}Remove them in GitHub Settings → Actions → Runners, then delete $(basename "$ORPHANS_FILE")${NC}"
    fi

    echo -e "\n  ${DIM}Press any key...${NC}"
    read -rsn1 || true
}

edit_config() {
    echo ""
    echo -e "  ${BOLD}Configuration${NC}"
    echo ""

    echo -e "  Current target: ${DIM}$(get_target_url || echo 'none')${NC}"
    echo ""
    echo -e "  ${CYAN}1${NC}) Change target (repository or organization)"
    echo -e "  ${CYAN}2${NC}) Change runner name prefix (current: $RUNNER_NAME_PREFIX)"
    echo -e "  ${CYAN}3${NC}) Cancel"
    echo ""
    printf '  Choice: '
    read -rsn1 choice
    echo ""

    case "$choice" in
        1)
            local before
            before=$(get_target_url)
            if pick_target && [[ "$(get_target_url)" != "$before" ]]; then
                save_config
                if validate_token; then
                    retarget_runners
                else
                    echo -e "  ${YELLOW}Warning: token cannot access $(get_target_url) - check gh auth scopes${NC}"
                    echo -e "  ${DIM}Existing runners were left on their old target${NC}"
                fi
            fi
            ;;
        2)
            printf '  Runner name prefix: '
            read -r prefix
            if [[ -n "$prefix" ]]; then
                if valid_prefix "$prefix"; then
                    RUNNER_NAME_PREFIX="$prefix"
                    save_config
                else
                    echo -e "  ${YELLOW}Invalid prefix (letters, numbers, _, - only)${NC}"
                fi
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
                echo -e "\n  ${DIM}Note: runners left running are NOT supervised (no auto-restart) until you reopen the manager.${NC}"
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

# ============================================================================
# Main
# ============================================================================

detect_platform

# Handle command-line flags
case "${1:-}" in
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
        shift 2
        [[ -n "${1:-}" ]] && die "Unknown option: $1 (see --help)"
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
        echo "  --target, -t X  Use target X (owner/repo, org, or URL) and skip the target menu"
        echo "  --version, -v   Print version"
        echo "  --help, -h      Show this help message"
        echo ""
        echo "Configuration:"
        echo "  Copy .runnermaxxer.conf.sample to .runnermaxxer.conf"
        echo "  Or run with --setup for interactive configuration"
        echo "  List repos/orgs in .runnermaxxer.targets to choose one at startup"
        echo ""
        exit 0
        ;;
    "")
        ;;
    *)
        die "Unknown option: $1 (see --help)"
        ;;
esac

echo -e "${BOLD}${CYAN}gh-runnermaxxer${NC}"
echo -e "${DIM}GitHub Actions Self-Hosted Runner Manager${NC}"
echo ""

load_config

# Check if onboarding is needed
needs_onboarding=false

if [[ ! -f "$CONFIG_FILE" ]]; then
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

# Target selection: --target wins; otherwise offer the targets-file menu
startup_target_before=$(get_target_url)
if [[ -n "$CLI_TARGET" ]]; then
    set_target "$CLI_TARGET" || die "Invalid --target: $CLI_TARGET (expected owner/repo, org, or URL)"
elif [[ "$needs_onboarding" != "true" ]] && [[ -n "$(load_targets)" ]]; then
    pick_target || true
fi
if [[ "$(get_target_url)" != "$startup_target_before" ]]; then
    save_config
fi

echo -e "${DIM}Running preflight checks...${NC}"
preflight_checks

# Adopt/clean up state left behind by a previous manager instance
reconcile_state

if [[ -n "$(get_target_url)" ]]; then
    if validate_token; then
        # Existing runners may still be registered to a previous target
        retarget_runners
    else
        echo -e "${YELLOW}Warning: token cannot access $(get_target_url) - check gh auth scopes${NC}"
        sleep 1
    fi
fi

# Guard against bad values busy-looping the UI or breaking arithmetic
[[ "$REFRESH_INTERVAL" =~ ^[0-9]+$ && "$REFRESH_INTERVAL" -ge 1 ]] || REFRESH_INTERVAL=5
[[ "$MAX_RESTART_ATTEMPTS" =~ ^[0-9]+$ && "$MAX_RESTART_ATTEMPTS" -ge 1 ]] || MAX_RESTART_ATTEMPTS=5
[[ "$MAX_LOG_SIZE_MB" =~ ^[0-9]+$ && "$MAX_LOG_SIZE_MB" -ge 1 ]] || MAX_LOG_SIZE_MB=10
[[ "$GH_HEALTH_TICKS" =~ ^[0-9]+$ ]] || GH_HEALTH_TICKS=12

# Labels can't change while running; detect_labels shells out to
# system_profiler etc., too slow to run every frame
CACHED_LABELS=$(detect_labels)

TUI_ACTIVE=1
clear
tick=0
while true; do
    supervise_runners || true
    if [[ "$GH_HEALTH_TICKS" -gt 0 ]]; then
        tick=$((tick + 1))
        if [[ $tick -ge $GH_HEALTH_TICKS ]]; then
            tick=0
            check_github_health || true
        fi
    fi
    draw_ui || true
    printf '  > '
    key=""
    read -rsn1 -t "$REFRESH_INTERVAL" key || true

    case "$key" in
        +|=) add_runner || true ;;
        -|_) remove_runner_prompt || true ;;
        s|S) start_all || true ;;
        x|X) stop_all || true ;;
        r|R) restart_all || true ;;
        n|N) scale_to || true ;;
        l|L) view_logs || true ;;
        c|C) check_github_status || true ;;
        e|E) edit_config || true ;;
        q|Q) quit_prompt || true ;;
    esac
done
