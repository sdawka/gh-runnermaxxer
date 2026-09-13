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

VERSION="3.0.0"

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

# GitHub owner/repo names: letters, digits, '.', '_', '-' (no spaces - the
# target lists are word-split in for loops)
valid_repo_url() { [[ "$1" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; }
valid_org_url()  { [[ "$1" =~ ^https://github\.com/[A-Za-z0-9_.-]+$ ]]; }
valid_prefix()   { [[ "$1" =~ ^[a-zA-Z0-9_-]+$ ]]; }

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
# Projects (repos/orgs) are listed in .runnermaxxer.targets, not here.
RUNNER_NAME_PREFIX="$RUNNER_NAME_PREFIX"
MAX_RUNNERS="$MAX_RUNNERS"
REFRESH_INTERVAL="$REFRESH_INTERVAL"
MAX_RESTART_ATTEMPTS="$MAX_RESTART_ATTEMPTS"
MAX_LOG_SIZE_MB="$MAX_LOG_SIZE_MB"
GH_HEALTH_TICKS="$GH_HEALTH_TICKS"
EOF
    mv "$tmp" "$CONFIG_FILE"
    [[ "${1:-}" == "quiet" ]] || echo -e "  ${GREEN}Configuration saved${NC}"
}

run_onboarding() {
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
        echo -e "\n  ${RED}Invalid characters. Use only letters, numbers, underscores, hyphens.${NC}"
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
        http://github.com/*)  e="https://${e#http://}" ;;
        github.com/*)         e="https://$e" ;;
        *)                    e="https://github.com/$e" ;;
    esac
    normalize_url "$e"
}

valid_target_url() { valid_repo_url "$1" || valid_org_url "$1"; }

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

# Can the gh token see this repo/org?
target_accessible() {
    local path="${1#https://github.com/}"
    if [[ "$(target_type "$1")" == "repo" ]]; then
        gh api "repos/$path" >/dev/null 2>&1
    else
        gh api "orgs/$path" >/dev/null 2>&1
    fi
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
        url=$(target_entry_to_url "$line")
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
        u=$(target_entry_to_url "$line")
        if [[ -n "$u" ]] && same_target "$u" "$1"; then
            continue
        fi
        printf '%s\n' "$line" >> "$tmp"
    done < "$TARGETS_FILE"
    mv "$tmp" "$TARGETS_FILE"
}

# URL a runner is registered to, from its .runner state file
runner_registered_url() {
    local u
    u=$(sed -n 's/.*"gitHubUrl": *"\([^"]*\)".*/\1/p' "$RUNNER_BASE_DIR/runner-$1/.runner" 2>/dev/null | head -1) || u=""
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

count_runners_for_target() {
    runner_ids_for_target "$1" | grep -c . || true
}

count_running_for_target() {
    local n=0 id
    for id in $(runner_ids_for_target "$1"); do
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

# Prompt for a project when there is more than one; sets CHOSEN_TARGET.
# Returns 1 when nothing was chosen.
CHOSEN_TARGET=""
choose_target() {
    local prompt="$1" targets=() t i=0 choice
    CHOSEN_TARGET=""
    while IFS= read -r t; do
        [[ -n "$t" ]] && targets[${#targets[@]}]="$t"
    done < <(known_targets)

    if [[ ${#targets[@]} -eq 0 ]]; then
        echo -e "\n  ${YELLOW}No projects yet - press 't' to add one${NC}"
        sleep 2
        return 1
    fi
    if [[ ${#targets[@]} -eq 1 ]]; then
        CHOSEN_TARGET="${targets[0]}"
        return 0
    fi

    echo ""
    for t in "${targets[@]}"; do
        i=$((i + 1))
        echo -e "  ${CYAN}$i${NC}) $(target_label "$t")"
    done
    printf '  %s [1-%d]: ' "$prompt" "${#targets[@]}"
    read -r choice || true
    if [[ "$choice" =~ ^[0-9]+$ ]] && [[ "$choice" -ge 1 && "$choice" -le ${#targets[@]} ]]; then
        CHOSEN_TARGET="${targets[$((choice - 1))]}"
        return 0
    fi
    [[ -n "$choice" ]] && { echo -e "  ${YELLOW}Invalid choice${NC}"; sleep 1; }
    return 1
}

# Idle/stopped runners of a target first, then busy ones; highest ID first
# within each class. Prints up to N runner IDs.
pick_victims() {
    local url=$1 n=$2 idle="" busy="" id
    for id in $(runner_ids_for_target "$url" | sort -rn); do
        if is_busy "$id"; then
            busy="$busy $id"
        else
            idle="$idle $id"
        fi
    done
    echo "$idle $busy" | tr ' ' '\n' | grep -v '^$' | head -n "$n" || true
    return 0
}

# Bring one target to exactly N runners. Prints progress; returns 1 if a
# runner failed to set up or start.
scale_target() {
    local url=$1 want=$2 cur failed=0 id next_id label k
    label=$(target_label "$url")
    cur=$(count_runners_for_target "$url")

    if [[ $want -gt $cur ]]; then
        if ! target_accessible "$url"; then
            echo -e "  ${RED}✗${NC} $label: the gh token cannot access it - check it exists and your auth scopes"
            return 1
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
                echo -e "    ${YELLOW}⚠ runner-$id is mid-job - its job will be aborted${NC}"
            fi
            echo -e "    Removing runner-$id..."
            remove_runner "$id"
        done
    fi
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
                M_WANT[i]=${old_want[j]}
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

    # Warn before killing runners mid-job
    for ((i = 0; i < ${#M_URLS[@]}; i++)); do
        [[ ${M_WANT[i]} -lt ${M_CUR[i]} ]] || continue
        for id in $(pick_victims "${M_URLS[i]}" $((M_CUR[i] - M_WANT[i]))); do
            is_busy "$id" && busy="$busy runner-$id"
        done
    done

    tput cnorm 2>/dev/null || true
    echo ""
    if [[ -n "$busy" ]]; then
        printf '  %b' "${YELLOW}Jobs in progress on:$busy - they will be aborted. Continue? [y/N]: ${NC}"
        local sure=""
        read -r sure || true
        if [[ ! "$sure" =~ ^[Yy] ]]; then
            tput civis 2>/dev/null || true
            M_MSG="${DIM}Cancelled - nothing changed${NC}"
            return 1
        fi
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
    if [[ -n "$M_MSG" ]]; then
        echo ""
        echo -e "  $M_MSG"
    fi
    return 0
}

# One keypress, decoded: up/down/left/right/enter/space/esc/quit or the char
read_key() {
    local k="" rest="" t=1
    # Fractional read timeouts need bash 4; on 3.2 a bare Esc takes 1s
    [[ "${BASH_VERSINFO[0]}" -ge 4 ]] && t=0.1
    IFS= read -rsn1 k || { echo "quit"; return 0; }
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

    if [[ $errors -gt 0 ]]; then
        die "Preflight checks failed ($errors error(s))"
    fi

    if ! acquire_lock; then
        local lock_pid
        lock_pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "?")
        die "Another instance is running (PID $lock_pid). One instance manages every project - press 't' there to add repos/orgs."
    fi
}

get_pat() {
    local token
    token=$(gh auth token 2>/dev/null) || return 1
    [[ -z "$token" ]] && return 1
    echo "$token"
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
    local t endpoint online id name cnt now started
    now=$(date +%s)

    for t in $(known_targets); do
        [[ -n "$(runner_ids_for_target "$t")" ]] || continue
        endpoint=$(target_api_endpoint "$t")
        online=$(gh api --paginate "$endpoint" --jq '.runners[] | select(.status == "online") | .name' 2>/dev/null) || continue

        for id in $(runner_ids_for_target "$t"); do
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

render_runner_line() {
    local id=$1
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
}

render_ui() {
    local total running disk_mb t ids id any=0

    total=$(get_runner_count)
    running=$(count_running)

    echo -e "${BOLD}${CYAN}"
    echo "  ╔═══════════════════════════════════════════════════╗"
    echo "  ║            gh-runnermaxxer                        ║"
    echo -e "  ╚═══════════════════════════════════════════════════╝${NC}"
    echo ""
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

    # Runners grouped by project
    for t in $(known_targets); do
        any=1
        ids=$(runner_ids_for_target "$t")
        if [[ -z "$ids" ]]; then
            echo -e "  ${DIM}$(target_label "$t")  (no runners)${NC}"
            continue
        fi
        echo -e "  ${BOLD}$(target_label "$t")${NC}"
        for id in $ids; do
            render_runner_line "$id"
        done
    done

    ids=$(unassigned_runner_ids)
    if [[ -n "$ids" ]]; then
        any=1
        echo -e "  ${YELLOW}Unconfigured (incomplete setup - remove with '-')${NC}"
        for id in $ids; do
            render_runner_line "$id"
        done
    fi

    if [[ $any -eq 0 ]]; then
        echo -e "  ${YELLOW}No projects configured (press 't' to add one)${NC}"
    fi

    echo ""
    echo -e "  ${BOLD}────────────────────────────────────────────────────${NC}"
    echo -e "  ${BOLD}Commands:${NC}"
    echo -e "    ${CYAN}+${NC}  Add runner          ${CYAN}-${NC}  Remove runner"
    echo -e "    ${CYAN}s${NC}  Start all           ${CYAN}x${NC}  Stop all"
    echo -e "    ${CYAN}r${NC}  Restart all         ${CYAN}l${NC}  View logs"
    echo -e "    ${CYAN}t${NC}  Projects & scaling  ${CYAN}c${NC}  Check GitHub status"
    echo -e "    ${CYAN}e${NC}  Edit config         ${CYAN}q${NC}  Quit"
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

add_runner() {
    choose_target "Add a runner to which project?" || return 0

    if [[ "$(get_runner_count)" -ge "$MAX_RUNNERS" ]]; then
        echo -e "\n  ${YELLOW}Already at MAX_RUNNERS ($MAX_RUNNERS)${NC}"
        sleep 2
        return 1
    fi

    local next_id
    next_id=$(next_free_id)

    echo -e "\n  ${BLUE}Setting up runner-$next_id for $(target_label "$CHOSEN_TARGET")...${NC}"
    if ! setup_runner "$next_id" "$CHOSEN_TARGET"; then
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
    local t endpoint out any=0

    echo -e "\n  ${BLUE}Checking GitHub runner status...${NC}"

    for t in $(known_targets); do
        any=1
        echo -e "\n  ${BOLD}$(target_label "$t")${NC}"
        endpoint=$(target_api_endpoint "$t")
        if out=$(gh api --paginate "$endpoint" --jq '.runners[] | "    \(.name): \(.status)"' 2>/dev/null); then
            if [[ -n "$out" ]]; then
                echo "$out"
            else
                echo -e "    ${DIM}no runners registered${NC}"
            fi
        else
            echo -e "    ${YELLOW}Could not fetch (may need admin access)${NC}"
        fi
    done
    [[ $any -eq 0 ]] && echo -e "  ${YELLOW}No projects configured${NC}"

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
    echo -e "  ${CYAN}1${NC}) Projects & runner counts (same as 't')"
    echo -e "  ${CYAN}2${NC}) Change runner name prefix (current: $RUNNER_NAME_PREFIX)"
    echo -e "  ${CYAN}3${NC}) Cancel"
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

# Adopt/clean up state left behind by a previous manager instance
reconcile_state

# Guard against bad values busy-looping the UI or breaking arithmetic
[[ "$REFRESH_INTERVAL" =~ ^[0-9]+$ && "$REFRESH_INTERVAL" -ge 1 ]] || REFRESH_INTERVAL=5
[[ "$MAX_RESTART_ATTEMPTS" =~ ^[0-9]+$ && "$MAX_RESTART_ATTEMPTS" -ge 1 ]] || MAX_RESTART_ATTEMPTS=5
[[ "$MAX_LOG_SIZE_MB" =~ ^[0-9]+$ && "$MAX_LOG_SIZE_MB" -ge 1 ]] || MAX_LOG_SIZE_MB=10
[[ "$GH_HEALTH_TICKS" =~ ^[0-9]+$ ]] || GH_HEALTH_TICKS=12
[[ "$MAX_RUNNERS" =~ ^[0-9]+$ && "$MAX_RUNNERS" -ge 1 ]] || MAX_RUNNERS=20

# Labels can't change while running; detect_labels shells out to
# system_profiler etc., too slow to run every frame
CACHED_LABELS=$(detect_labels)

TUI_ACTIVE=1

# Project menu: pick which repos/orgs get runners and how many
if [[ "$SKIP_MENU" -eq 0 ]]; then
    clear
    target_menu startup "$cli_url" || true
fi

clear
tput cnorm 2>/dev/null || true
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
        t|T|n|N) target_menu tui || true ;;
        l|L) view_logs || true ;;
        c|C) check_github_status || true ;;
        e|E) edit_config || true ;;
        q|Q) quit_prompt || true ;;
    esac
done