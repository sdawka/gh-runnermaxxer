#!/bin/bash
set -euo pipefail

# ============================================================================
# gh-runnermaxxer - GitHub Actions Self-Hosted Runner Manager
# ============================================================================

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG_FILE="$SCRIPT_DIR/.runnermaxxer.conf"

# Defaults (can be overridden by config file or environment)
RUNNER_BASE_DIR="${RUNNER_BASE_DIR:-$SCRIPT_DIR/runners}"
RUNNER_NAME_PREFIX="${RUNNER_NAME_PREFIX:-$(hostname -s)}"
REPO_URL="${REPO_URL:-}"
ORG_URL="${ORG_URL:-}"
MAX_RUNNERS="${MAX_RUNNERS:-20}"
REFRESH_INTERVAL="${REFRESH_INTERVAL:-5}"

PID_DIR="$RUNNER_BASE_DIR/.pids"
LOG_DIR="$RUNNER_BASE_DIR/.logs"
LOCK_FILE="$RUNNER_BASE_DIR/.runnermaxxer.lock"

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
# Auto-detection functions
# ============================================================================

detect_runner_tarball() {
    local os arch pattern

    case "$(uname -s)" in
        Darwin) os="osx" ;;
        Linux)  os="linux" ;;
        *)      os="linux" ;;
    esac

    case "$(uname -m)" in
        arm64|aarch64) arch="arm64" ;;
        x86_64|amd64)  arch="x64" ;;
        *)             arch="x64" ;;
    esac

    pattern="actions-runner-${os}-${arch}-*.tar.gz"

    # Find the newest matching tarball
    local tarball
    tarball=$(ls -t "$SCRIPT_DIR"/$pattern 2>/dev/null | head -1)

    if [[ -n "$tarball" ]]; then
        echo "$tarball"
    else
        echo ""
    fi
}

detect_labels() {
    local labels=()

    # OS
    case "$(uname -s)" in
        Darwin) labels+=("macos") ;;
        Linux)
            labels+=("linux")
            if [[ -f /etc/os-release ]]; then
                # shellcheck disable=SC1091
                source /etc/os-release
                [[ -n "${ID:-}" ]] && labels+=("$ID")
            fi
            ;;
    esac

    # Architecture
    case "$(uname -m)" in
        arm64|aarch64) labels+=("arm64" "apple-silicon") ;;
        x86_64|amd64)  labels+=("x64") ;;
    esac

    # macOS version
    if [[ "$(uname -s)" == "Darwin" ]]; then
        local macos_ver
        macos_ver=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)
        [[ -n "$macos_ver" ]] && labels+=("macos-$macos_ver")
    fi

    # Docker available?
    command -v docker &>/dev/null && labels+=("docker")

    # Check for GPU (basic)
    if [[ "$(uname -s)" == "Darwin" ]]; then
        system_profiler SPDisplaysDataType 2>/dev/null | grep -q "Metal" && labels+=("metal")
    elif command -v nvidia-smi &>/dev/null; then
        labels+=("gpu" "nvidia")
    fi

    # High memory? (>16GB)
    local mem_gb
    if [[ "$(uname -s)" == "Darwin" ]]; then
        mem_gb=$(( $(sysctl -n hw.memsize) / 1073741824 ))
    else
        mem_gb=$(( $(grep MemTotal /proc/meminfo | awk '{print $2}') / 1048576 ))
    fi
    [[ $mem_gb -ge 16 ]] && labels+=("high-memory")
    [[ $mem_gb -ge 32 ]] && labels+=("32gb-ram")

    # CPU cores
    local cores
    if [[ "$(uname -s)" == "Darwin" ]]; then
        cores=$(sysctl -n hw.ncpu)
    else
        cores=$(nproc)
    fi
    [[ $cores -ge 8 ]] && labels+=("8-core")

    echo "${labels[*]}"
}

# ============================================================================
# Core functions
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
    rm -f "$LOCK_FILE"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

load_config() {
    if [[ -f "$CONFIG_FILE" ]]; then
        # shellcheck disable=SC1090
        source "$CONFIG_FILE"
    fi
}

validate_config() {
    local errors=()
    local warnings=()

    # Check if config file exists
    if [[ ! -f "$CONFIG_FILE" ]]; then
        errors+=("No configuration file found")
        return 1
    fi

    # Check if both REPO_URL and ORG_URL are set (ambiguous)
    if [[ -n "$REPO_URL" && -n "$ORG_URL" ]]; then
        errors+=("Both REPO_URL and ORG_URL are set - only one should be configured")
    fi

    # Check if neither is set
    if [[ -z "$REPO_URL" && -z "$ORG_URL" ]]; then
        errors+=("No target configured - set either REPO_URL or ORG_URL")
    fi

    # Validate REPO_URL format
    if [[ -n "$REPO_URL" ]]; then
        if [[ ! "$REPO_URL" =~ ^https://github\.com/[^/]+/[^/]+$ ]]; then
            errors+=("Invalid REPO_URL format: $REPO_URL")
            errors+=("Expected: https://github.com/owner/repo")
        fi
    fi

    # Validate ORG_URL format
    if [[ -n "$ORG_URL" ]]; then
        if [[ ! "$ORG_URL" =~ ^https://github\.com/[^/]+$ ]]; then
            errors+=("Invalid ORG_URL format: $ORG_URL")
            errors+=("Expected: https://github.com/org-name")
        fi
    fi

    # Validate MAX_RUNNERS is a number
    if [[ -n "$MAX_RUNNERS" && ! "$MAX_RUNNERS" =~ ^[0-9]+$ ]]; then
        errors+=("MAX_RUNNERS must be a number, got: $MAX_RUNNERS")
    fi

    # Validate MAX_RUNNERS is reasonable
    if [[ -n "$MAX_RUNNERS" && "$MAX_RUNNERS" =~ ^[0-9]+$ ]]; then
        if [[ "$MAX_RUNNERS" -lt 1 ]]; then
            errors+=("MAX_RUNNERS must be at least 1")
        elif [[ "$MAX_RUNNERS" -gt 100 ]]; then
            warnings+=("MAX_RUNNERS is very high ($MAX_RUNNERS) - this may cause resource issues")
        fi
    fi

    # Validate RUNNER_NAME_PREFIX
    if [[ -n "$RUNNER_NAME_PREFIX" ]]; then
        if [[ ! "$RUNNER_NAME_PREFIX" =~ ^[a-zA-Z0-9_-]+$ ]]; then
            errors+=("RUNNER_NAME_PREFIX contains invalid characters: $RUNNER_NAME_PREFIX")
            errors+=("Use only letters, numbers, underscores, and hyphens")
        fi
    fi

    # Print warnings
    if [[ ${#warnings[@]} -gt 0 ]]; then
        for warning in "${warnings[@]}"; do
            echo -e "${YELLOW}⚠ $warning${NC}"
        done
    fi

    # Print errors and return failure if any
    if [[ ${#errors[@]} -gt 0 ]]; then
        for error in "${errors[@]}"; do
            echo -e "${RED}✗ $error${NC}"
        done
        return 1
    fi

    return 0
}

run_onboarding() {
    echo ""
    echo -e "${BOLD}${CYAN}╔═══════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║         gh-runnermaxxer Setup                     ║${NC}"
    echo -e "${BOLD}${CYAN}╚═══════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${DIM}Let's configure your self-hosted runner environment.${NC}"
    echo ""

    # Step 1: Target type
    echo -e "${BOLD}Step 1: Runner Target${NC}"
    echo ""
    echo "  Where should runners register?"
    echo ""
    echo -e "  ${CYAN}1${NC}) A specific repository"
    echo -e "  ${CYAN}2${NC}) An entire organization"
    echo ""
    echo -e "  Choice [1/2]: \c"
    read -r target_choice

    local new_repo_url=""
    local new_org_url=""

    case "$target_choice" in
        1)
            echo ""
            echo -e "  ${DIM}Example: https://github.com/myorg/myrepo${NC}"
            echo -e "  Repository URL: \c"
            read -r new_repo_url
            if [[ ! "$new_repo_url" =~ ^https://github\.com/[^/]+/[^/]+$ ]]; then
                echo -e "\n  ${RED}Invalid format. Expected: https://github.com/owner/repo${NC}"
                echo -e "  ${DIM}Run the script again to retry.${NC}"
                exit 1
            fi
            ;;
        2)
            echo ""
            echo -e "  ${DIM}Example: https://github.com/myorg${NC}"
            echo -e "  Organization URL: \c"
            read -r new_org_url
            if [[ ! "$new_org_url" =~ ^https://github\.com/[^/]+$ ]]; then
                echo -e "\n  ${RED}Invalid format. Expected: https://github.com/org-name${NC}"
                echo -e "  ${DIM}Run the script again to retry.${NC}"
                exit 1
            fi
            ;;
        *)
            echo -e "\n  ${RED}Invalid choice${NC}"
            exit 1
            ;;
    esac

    # Step 2: Runner name prefix
    echo ""
    echo -e "${BOLD}Step 2: Runner Name Prefix${NC}"
    echo ""
    local default_prefix
    default_prefix=$(hostname -s)
    echo -e "  Runners will be named: prefix-1, prefix-2, etc."
    echo -e "  ${DIM}Default: $default_prefix${NC}"
    echo -e "  Runner name prefix [$default_prefix]: \c"
    read -r new_prefix
    [[ -z "$new_prefix" ]] && new_prefix="$default_prefix"

    if [[ ! "$new_prefix" =~ ^[a-zA-Z0-9_-]+$ ]]; then
        echo -e "\n  ${RED}Invalid characters. Use only letters, numbers, underscores, hyphens.${NC}"
        exit 1
    fi

    # Step 3: Max runners
    echo ""
    echo -e "${BOLD}Step 3: Maximum Runners${NC}"
    echo ""
    echo -e "  How many runners can run simultaneously?"
    echo -e "  ${DIM}Default: 20${NC}"
    echo -e "  Max runners [20]: \c"
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
    echo -e "  Save this configuration? [Y/n]: \c"
    read -r confirm

    if [[ "$confirm" =~ ^[Nn] ]]; then
        echo -e "\n  ${YELLOW}Aborted. No changes saved.${NC}"
        exit 0
    fi

    # Save config
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

save_config() {
    cat > "$CONFIG_FILE" << EOF
# gh-runnermaxxer configuration
REPO_URL="$REPO_URL"
ORG_URL="$ORG_URL"
RUNNER_NAME_PREFIX="$RUNNER_NAME_PREFIX"
MAX_RUNNERS="$MAX_RUNNERS"
EOF
    echo -e "  ${GREEN}Configuration saved${NC}"
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

preflight_checks() {
    local errors=0

    mkdir -p "$RUNNER_BASE_DIR"

    # Required commands
    for cmd in gh tar; do
        if ! command -v "$cmd" &>/dev/null; then
            echo -e "${RED}✗ Missing required command: $cmd${NC}" >&2
            errors=$((errors + 1))
        fi
    done

    # gh authentication
    if command -v gh &>/dev/null; then
        if ! gh auth status &>/dev/null; then
            echo -e "${RED}✗ gh CLI is not authenticated. Run: gh auth login${NC}" >&2
            errors=$((errors + 1))
        fi
    fi

    # Runner tarball
    RUNNER_TAR=$(detect_runner_tarball)
    if [[ -z "$RUNNER_TAR" ]]; then
        echo -e "${RED}✗ No runner tarball found. Download from:${NC}" >&2
        echo -e "${DIM}  https://github.com/actions/runner/releases${NC}" >&2
        errors=$((errors + 1))
    elif ! tar -tzf "$RUNNER_TAR" &>/dev/null; then
        echo -e "${RED}✗ Runner tarball is corrupted${NC}" >&2
        errors=$((errors + 1))
    else
        echo -e "${GREEN}✓${NC} Found runner: $(basename "$RUNNER_TAR")"
    fi

    # Target URL
    if [[ -z "$(get_target_url)" ]]; then
        echo -e "${YELLOW}! No repository or organization configured${NC}" >&2
        echo -e "${DIM}  Press 'e' to configure after startup${NC}" >&2
    fi

    # Lock file
    if [[ -f "$LOCK_FILE" ]]; then
        local lock_pid
        lock_pid=$(cat "$LOCK_FILE" 2>/dev/null || echo "")
        if [[ -n "$lock_pid" ]] && kill -0 "$lock_pid" 2>/dev/null; then
            echo -e "${RED}✗ Another instance is running (PID $lock_pid)${NC}" >&2
            errors=$((errors + 1))
        else
            rm -f "$LOCK_FILE"
        fi
    fi

    if [[ $errors -gt 0 ]]; then
        die "Preflight checks failed ($errors error(s))"
    fi

    echo $$ > "$LOCK_FILE"
}

init_dirs() {
    mkdir -p "$RUNNER_BASE_DIR" "$PID_DIR" "$LOG_DIR"
}

get_pat() {
    local token
    token=$(gh auth token 2>/dev/null) || die "Failed to get token from gh CLI"
    [[ -z "$token" ]] && die "gh auth token returned empty"
    echo "$token"
}

validate_token() {
    local target_url target_type api_path
    target_url=$(get_target_url)
    target_type=$(get_target_type)

    [[ -z "$target_url" ]] && return 1

    if [[ "$target_type" == "repo" ]]; then
        api_path=$(echo "$target_url" | sed 's|https://github.com/||')
        gh api "repos/$api_path" &>/dev/null || return 1
    elif [[ "$target_type" == "org" ]]; then
        api_path=$(echo "$target_url" | sed 's|https://github.com/||')
        gh api "orgs/$api_path" &>/dev/null || return 1
    fi
    return 0
}

# ============================================================================
# Runner management
# ============================================================================

get_runner_ids() {
    ls -d "$RUNNER_BASE_DIR"/runner-* 2>/dev/null | sed 's/.*runner-//' | sort -n
}

get_runner_count() {
    ls -d "$RUNNER_BASE_DIR"/runner-* 2>/dev/null | wc -l | tr -d ' '
}

is_running() {
    local id=$1
    local pid_file="$PID_DIR/runner-$id.pid"
    [[ -f "$pid_file" ]] && kill -0 "$(cat "$pid_file")" 2>/dev/null
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

# Restart any runner that died unexpectedly. Runners that were stopped via
# a command keep their .stopped marker and are left alone.
supervise_runners() {
    [[ -z "$(get_target_url)" ]] && return
    for id in $(get_runner_ids); do
        if ! is_running "$id" && ! is_marked_stopped "$id"; then
            start_runner "$id" >/dev/null 2>&1 || true
        fi
    done
}

count_running() {
    local count=0
    for id in $(get_runner_ids); do
        is_running "$id" && count=$((count + 1))
    done
    echo $count
}

get_runner_status() {
    local id=$1
    local log_file="$LOG_DIR/runner-$id.log"

    [[ ! -f "$log_file" ]] && { echo "no logs"; return; }

    local recent
    recent=$(tail -50 "$log_file" 2>/dev/null || echo "")
    [[ -z "$recent" ]] && { echo "no logs"; return; }

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

setup_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"
    local target_url labels_str

    [[ -d "$runner_dir" ]] && return 0

    target_url=$(get_target_url)
    [[ -z "$target_url" ]] && { warn "No target URL configured"; return 1; }

    local pat
    pat=$(get_pat) || { warn "Failed to get PAT"; return 1; }

    mkdir -p "$runner_dir" || { warn "Failed to create directory"; return 1; }

    if ! tar -xzf "$RUNNER_TAR" -C "$runner_dir" 2>/dev/null; then
        warn "Failed to extract runner tarball"
        rm -rf "$runner_dir"
        return 1
    fi

    labels_str=$(detect_labels)

    local original_dir="$PWD"
    cd "$runner_dir" || return 1

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
    if ! config_output=$(./config.sh "${config_args[@]}" 2>&1); then
        warn "Failed to configure runner-$id"
        echo -e "${DIM}$config_output${NC}" >&2
        cd "$original_dir"
        rm -rf "$runner_dir"
        return 1
    fi

    cd "$original_dir"
    return 0
}

start_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"
    local pid_file="$PID_DIR/runner-$id.pid"

    # Starting (or restarting) a runner means we want it up: clear any
    # manual-stop marker so the supervisor keeps it alive.
    unmark_stopped "$id"

    is_running "$id" && return 0

    [[ ! -d "$runner_dir" ]] && { warn "Runner directory not found"; return 1; }
    [[ ! -x "$runner_dir/run.sh" ]] && { warn "run.sh not found/executable"; return 1; }

    local original_dir="$PWD"
    cd "$runner_dir" || return 1

    nohup ./run.sh >> "$LOG_DIR/runner-$id.log" 2>&1 &
    local pid=$!
    echo "$pid" > "$pid_file"

    sleep 0.5
    if ! kill -0 "$pid" 2>/dev/null; then
        warn "Runner-$id failed to start"
        rm -f "$pid_file"
        cd "$original_dir"
        return 1
    fi

    cd "$original_dir"
    return 0
}

stop_runner() {
    local id=$1
    local pid_file="$PID_DIR/runner-$id.pid"
    # Record that this stop was intentional so the supervisor leaves it down.
    mark_stopped "$id"
    [[ ! -f "$pid_file" ]] && return 0
    local pid
    pid=$(cat "$pid_file")
    kill "$pid" 2>/dev/null || true
    for _ in {1..10}; do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
    kill -9 "$pid" 2>/dev/null || true
    rm -f "$pid_file"
}

remove_runner() {
    local id=$1
    local runner_dir="$RUNNER_BASE_DIR/runner-$id"

    stop_runner "$id"

    if [[ -d "$runner_dir" ]]; then
        local original_dir="$PWD"
        cd "$runner_dir" || return 1

        local pat
        if pat=$(get_pat 2>/dev/null); then
            ./config.sh remove --token "$pat" 2>/dev/null || warn "Failed to unregister (may need manual cleanup)"
        fi

        cd "$original_dir"
        rm -rf "$runner_dir"
    fi

    rm -f "$LOG_DIR/runner-$id.log"
    rm -f "$PID_DIR/runner-$id.stopped"
}

# ============================================================================
# UI
# ============================================================================

render_ui() {
    local total running target_url

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
    echo ""

    if [[ $total -gt 0 ]]; then
        echo -e "  ${BOLD}Runners:${NC}"
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
            else
                echo -e "    ${YELLOW}◌${NC} runner-$id ${YELLOW}restarting...${NC}"
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

add_runner() {
    local target_url
    target_url=$(get_target_url)

    if [[ -z "$target_url" ]]; then
        echo -e "\n  ${YELLOW}Configure a target first (press 'e')${NC}"
        sleep 2
        return 1
    fi

    local next_id=1
    while [[ -d "$RUNNER_BASE_DIR/runner-$next_id" ]]; do next_id=$((next_id + 1)); done

    echo -e "\n  ${BLUE}Setting up runner-$next_id...${NC}"
    if ! setup_runner "$next_id"; then
        echo -e "  ${RED}Failed to setup runner-$next_id${NC}"
        sleep 2
        return 1
    fi

    echo -e "  ${BLUE}Starting runner-$next_id...${NC}"
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
    [[ -z "$ids" ]] && { echo -e "\n  ${YELLOW}No runners${NC}"; sleep 1; return; }

    echo -e "\n  Remove which runner? [$(echo "$ids" | tr '\n' ' ')]: \c"
    read -r id

    [[ -z "$id" ]] && return
    [[ ! "$id" =~ ^[0-9]+$ ]] && { echo -e "  ${YELLOW}Invalid ID${NC}"; sleep 1; return; }
    [[ ! -d "$RUNNER_BASE_DIR/runner-$id" ]] && { echo -e "  ${YELLOW}Not found${NC}"; sleep 1; return; }

    echo -e "  ${BLUE}Removing runner-$id...${NC}"
    remove_runner "$id"
    echo -e "  ${GREEN}Done!${NC}"
    sleep 1
}

start_all() {
    for id in $(get_runner_ids); do
        echo -e "  Starting runner-$id..."
        start_runner "$id"
    done
    echo -e "  ${GREEN}Done!${NC}"
    sleep 1
}

stop_all() {
    for id in $(get_runner_ids); do
        echo -e "  Stopping runner-$id..."
        stop_runner "$id"
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
        return
    fi

    echo -e "\n  Scale to how many runners?: \c"
    read -r count
    [[ ! "$count" =~ ^[0-9]+$ ]] && { echo -e "  ${YELLOW}Invalid number${NC}"; sleep 1; return; }

    if [[ $count -gt $MAX_RUNNERS ]]; then
        echo -e "  ${YELLOW}Max $MAX_RUNNERS runners${NC}"
        sleep 1
        return
    fi

    local current failed=0
    current=$(get_runner_count)

    if [[ $count -gt $current ]]; then
        local to_add=$((count - current))
        echo -e "  ${BLUE}Adding $to_add runners...${NC}"
        for ((i=0; i<to_add; i++)); do
            local next_id=1
            while [[ -d "$RUNNER_BASE_DIR/runner-$next_id" ]]; do next_id=$((next_id + 1)); done
            echo -e "    Setting up runner-$next_id..."
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
        for id in $(get_runner_ids | tail -n "$to_remove"); do
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
    [[ -z "$ids" ]] && { echo -e "\n  ${YELLOW}No runners${NC}"; sleep 1; return; }

    echo -e "\n  View logs for which runner? [$(echo "$ids" | tr '\n' ' ')]: \c"
    read -r id

    [[ -z "$id" ]] && return
    [[ ! "$id" =~ ^[0-9]+$ ]] && { echo -e "  ${YELLOW}Invalid ID${NC}"; sleep 1; return; }

    local log_file="$LOG_DIR/runner-$id.log"
    [[ ! -f "$log_file" ]] && { echo -e "  ${YELLOW}No log file${NC}"; sleep 1; return; }

    echo -e "\n  ${DIM}(Ctrl+C to exit)${NC}\n"
    sleep 1
    trap '' INT
    tail -f "$log_file" || true
    trap 'exit 130' INT
}

check_github_status() {
    local target_url target_type api_path
    target_url=$(get_target_url)
    target_type=$(get_target_type)

    [[ -z "$target_url" ]] && { echo -e "\n  ${YELLOW}No target configured${NC}"; sleep 2; return; }

    echo -e "\n  ${BLUE}Checking GitHub runner status...${NC}"

    api_path=$(echo "$target_url" | sed 's|https://github.com/||')

    local endpoint
    if [[ "$target_type" == "repo" ]]; then
        endpoint="repos/$api_path/actions/runners"
    else
        endpoint="orgs/$api_path/actions/runners"
    fi

    if ! gh api "$endpoint" --jq '.runners[] | "  \(.name): \(.status)"' 2>/dev/null; then
        echo -e "  ${YELLOW}Could not fetch (may need admin access)${NC}"
    fi

    echo -e "\n  ${DIM}Press any key...${NC}"
    read -rsn1
}

edit_config() {
    echo ""
    echo -e "  ${BOLD}Configuration${NC}"
    echo ""

    echo -e "  Current target: ${DIM}$(get_target_url || echo 'none')${NC}"
    echo ""
    echo -e "  ${CYAN}1${NC}) Configure for a repository"
    echo -e "  ${CYAN}2${NC}) Configure for an organization"
    echo -e "  ${CYAN}3${NC}) Change runner name prefix (current: $RUNNER_NAME_PREFIX)"
    echo -e "  ${CYAN}4${NC}) Cancel"
    echo ""
    echo -e "  Choice: \c"
    read -rsn1 choice

    case "$choice" in
        1)
            echo -e "\n  Repository URL (e.g., https://github.com/owner/repo): \c"
            read -r url
            if [[ "$url" =~ ^https://github.com/.+/.+ ]]; then
                REPO_URL="$url"
                ORG_URL=""
                save_config
            else
                echo -e "  ${YELLOW}Invalid URL${NC}"
            fi
            ;;
        2)
            echo -e "\n  Organization URL (e.g., https://github.com/myorg): \c"
            read -r url
            if [[ "$url" =~ ^https://github.com/[^/]+$ ]]; then
                ORG_URL="$url"
                REPO_URL=""
                save_config
            else
                echo -e "  ${YELLOW}Invalid URL${NC}"
            fi
            ;;
        3)
            echo -e "\n  Runner name prefix: \c"
            read -r prefix
            if [[ -n "$prefix" ]]; then
                RUNNER_NAME_PREFIX="$prefix"
                save_config
            fi
            ;;
    esac
    sleep 1
}

# ============================================================================
# Main
# ============================================================================

# Handle command-line flags
case "${1:-}" in
    --setup|-s)
        load_config
        run_onboarding
        echo -e "${GREEN}Setup complete. Run without --setup to start.${NC}"
        exit 0
        ;;
    --help|-h)
        echo "Usage: ./runnermaxxer.sh [OPTIONS]"
        echo ""
        echo "GitHub Actions Self-Hosted Runner Manager"
        echo ""
        echo "Options:"
        echo "  --setup, -s    Run interactive setup wizard"
        echo "  --help, -h     Show this help message"
        echo ""
        echo "Configuration:"
        echo "  Copy .runnermaxxer.conf.sample to .runnermaxxer.conf"
        echo "  Or run with --setup for interactive configuration"
        echo ""
        exit 0
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
    echo -e "Would you like to run setup to fix them? [Y/n]: \c"
    read -r fix_choice
    if [[ ! "$fix_choice" =~ ^[Nn] ]]; then
        needs_onboarding=true
    fi
fi

if [[ "$needs_onboarding" == "true" ]]; then
    run_onboarding
    load_config
fi

echo -e "${DIM}Running preflight checks...${NC}"
preflight_checks
init_dirs

if [[ -n "$(get_target_url)" ]]; then
    if ! validate_token; then
        echo -e "${YELLOW}Token validation warning${NC}"
        sleep 1
    fi
fi

# Guard against a bad value busy-looping the UI
[[ "$REFRESH_INTERVAL" =~ ^[0-9]+$ && "$REFRESH_INTERVAL" -ge 1 ]] || REFRESH_INTERVAL=5

# Labels can't change while running; detect_labels shells out to
# system_profiler etc., too slow to run every frame
CACHED_LABELS=$(detect_labels)

clear
while true; do
    supervise_runners
    draw_ui
    echo -e "  > \c"
    key=""
    read -rsn1 -t "$REFRESH_INTERVAL" key || true

    case "$key" in
        +|=) add_runner ;;
        -|_) remove_runner_prompt ;;
        s|S) start_all ;;
        x|X) stop_all ;;
        r|R) restart_all ;;
        n|N) scale_to ;;
        l|L) view_logs ;;
        c|C) check_github_status ;;
        e|E) edit_config ;;
        q|Q) clear; exit 0 ;;
    esac
done
