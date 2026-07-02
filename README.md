# gh-runnermaxxer

A TUI for managing multiple GitHub Actions self-hosted runners on a single machine.

```
  ╔═══════════════════════════════════════════════════╗
  ║            gh-runnermaxxer                        ║
  ╚═══════════════════════════════════════════════════╝

  Target: https://github.com/myorg/myrepo
  Labels: macos, arm64, apple-silicon, docker, high-memory

  Status: 3 running / 4 configured

  Runners:
    ● runner-1 PID 12345 [running: build-and-test]
    ● runner-2 PID 12346 [idle]
    ● runner-3 PID 12347 [idle (done)]
    ○ runner-4 stopped
```

## Features

- **Interactive setup wizard** - guided configuration on first run
- **Self-healing supervisor** - crashed runners restart automatically with exponential backoff; crash-looping runners are quarantined instead of restarting forever
- **GitHub-side health checks** - runners that GitHub reports offline (wedged listener, stale credentials) are recycled even if the local process looks alive
- **Whole-process-tree stop** - stopping a runner kills `Runner.Listener` and workers too, not just the wrapper script, with graceful SIGINT → SIGTERM → SIGKILL escalation
- **Orphan adoption** - if the manager crashes and restarts, it re-adopts still-running runner processes instead of starting duplicates
- **Auto-download runner tarball** - fetches the latest release for your OS/arch and verifies its SHA-256 checksum
- **Log rotation** - runner logs are truncated past a size limit so they can't fill the disk
- **Config validation** - detects invalid URLs, bad values, and offers to fix them; pasted URLs are normalized (trailing `/`, `.git`)
- **Scale runners up/down** with a single keypress - scale-down prefers idle runners and warns before killing one mid-job
- **Auto-detect labels** based on system capabilities (OS, arch, memory, GPU, Docker, WSL, etc.)
- **Live status** showing what each runner is doing (idle, running job, errors, quarantined)
- **Works with repos or orgs** - configure once, spin up runners
- **Uses `gh` CLI** for authentication - no PAT management needed

## Requirements

- bash >= 3.2 (stock macOS bash works)
- [GitHub CLI](https://cli.github.com/) (`gh`) - authenticated with `gh auth login`
- macOS (Intel or Apple Silicon) or glibc-based Linux (Debian, Ubuntu, Fedora, Arch, ..., including WSL2)

Not supported: Windows (the Windows runner uses a different install flow), Alpine/musl (the runner requires glibc), WSL1. The script detects these and fails fast with a clear message. On Linux, if `libicu` is missing the script points you at the runner's bundled `installdependencies.sh`.

## Quick Start

1. **Clone and run**

   ```bash
   git clone https://github.com/YOUR_USERNAME/gh-runnermaxxer.git
   cd gh-runnermaxxer
   chmod +x runnermaxxer.sh
   ./runnermaxxer.sh
   ```

2. **Configure** - on first run, an interactive setup wizard guides you through configuration

3. **Runner tarball** - if none is present, the script offers to download the latest release for your platform (checksum-verified). You can also fetch it explicitly with `./runnermaxxer.sh --download`, or download manually from [actions/runner releases](https://github.com/actions/runner/releases)

4. **Add runners** - press `+` or use `n` to scale to a specific count

## Usage

| Key | Action |
|-----|--------|
| `+` | Add a new runner |
| `-` | Remove a runner |
| `s` | Start all runners |
| `x` | Stop all runners |
| `r` | Restart all runners |
| `n` | Scale to N runners |
| `l` | View runner logs |
| `c` | Check GitHub runner status |
| `e` | Edit configuration |
| `q` | Quit |

## Auto-Detected Labels

Runners are automatically labeled based on detected capabilities:

| Label | Condition |
|-------|-----------|
| `macos`, `linux` | Operating system |
| `arm64`, `x64` | Architecture |
| `apple-silicon` | ARM64 Mac |
| `macos-14`, etc. | macOS version |
| `docker` | Docker installed |
| `metal` | Apple Metal GPU |
| `nvidia`, `gpu` | NVIDIA GPU detected |
| `high-memory` | 16GB+ RAM |
| `32gb-ram` | 32GB+ RAM |
| `8-core` | 8+ CPU cores |

Use these labels in your workflows:

```yaml
jobs:
  build:
    runs-on: [self-hosted, macos, arm64, docker]
```

## Self-Healing Behavior

While the TUI is open, a supervisor runs every refresh tick:

- A runner that dies unexpectedly is restarted with exponential backoff (5s, 10s, 20s, 40s, ...).
- After `MAX_RESTART_ATTEMPTS` consecutive rapid crashes, the runner is **quarantined** (shown as `✖` with the failure reason) so it can't crash-loop forever. Press `s` (start all) or `r` to clear the quarantine and retry.
- A runner stopped on purpose (via `x` or `-`) stays down.
- Every `GH_HEALTH_TICKS` ticks, local runners are cross-checked against the GitHub API; a runner whose process is alive but that GitHub reports offline twice in a row is recycled. This check is skipped when the API is unreachable, so a network outage never triggers mass restarts.
- Runner logs are truncated past `MAX_LOG_SIZE_MB` (a `.log.1` copy is kept).
- Stale PID files (e.g. after a reboot) are detected via process-identity checks, so a recycled PID is never mistaken for a live runner or killed by accident.

Runners are detached processes: quitting the manager can leave them running (you'll be asked), but they are only supervised while the TUI is open.

## CLI Options

```bash
./runnermaxxer.sh [OPTIONS]

Options:
  --setup, -s     Run interactive setup wizard
  --download, -d  Download the latest runner tarball for this platform
  --version, -v   Print version
  --help, -h      Show help message
```

## Configuration

On first run (or with `--setup`), an interactive wizard guides you through setup:

1. Choose target: repository or organization
2. Set runner name prefix (default: hostname)
3. Set maximum runners (default: 20)

Configuration is validated on startup. If issues are detected (invalid URLs, bad values), you'll be prompted to fix them.

### Config File

Configuration is stored in `.runnermaxxer.conf`. You can also copy the sample:

```bash
cp .runnermaxxer.conf.sample .runnermaxxer.conf
# Edit as needed
```

| Variable | Description |
|----------|-------------|
| `REPO_URL` | Repository URL (e.g., `https://github.com/owner/repo`) |
| `ORG_URL` | Organization URL (e.g., `https://github.com/myorg`) |
| `RUNNER_NAME_PREFIX` | Prefix for runner names (default: hostname) |
| `MAX_RUNNERS` | Maximum runners allowed (default: 20) |
| `REFRESH_INTERVAL` | Seconds between automatic status refreshes (default: 5) |
| `MAX_RESTART_ATTEMPTS` | Consecutive crashes before a runner is quarantined (default: 5) |
| `MAX_LOG_SIZE_MB` | Truncate runner logs past this size (default: 10) |
| `GH_HEALTH_TICKS` | GitHub-side health check every N ticks, 0 to disable (default: 12) |

**Note:** Set only ONE of `REPO_URL` or `ORG_URL`, not both.

## Directory Structure

```
gh-runnermaxxer/
├── runnermaxxer.sh              # Main script
├── actions-runner-*.tar.gz      # Runner tarball (you download this)
├── .runnermaxxer.conf.sample    # Sample configuration
├── .runnermaxxer.conf           # Your configuration (auto-created via setup)
└── runners/                     # Runner instances (auto-created)
    ├── runner-1/
    ├── runner-2/
    ├── .pids/
    └── .logs/
```

## Troubleshooting

**"gh CLI is not authenticated"**
```bash
gh auth login
```

**"No runner tarball found"**

Say yes to the auto-download prompt, or run `./runnermaxxer.sh --download`. Manual downloads from https://github.com/actions/runner/releases also work - the script auto-detects tarballs matching your OS/architecture.

**"Token may not have access"**

Ensure your `gh` auth has the `repo` scope (for repository runners) or `admin:org` scope (for organization runners).

**Runner shows "offline" on GitHub**

The supervisor normally recycles these automatically within a couple of minutes. If it persists, check logs with `l` and restart with `r`.

**Runner is quarantined (`✖`)**

It crashed repeatedly in a short window. The failure reason is shown next to it; check logs with `l`, fix the cause, then press `s` to clear the quarantine and retry.

**"Failed to configure runner" on Linux**

Usually missing runner dependencies (libicu etc.). Run the runner's bundled installer: `sudo ./runners/runner-1/bin/installdependencies.sh`

**Orphaned registrations warning**

If unregistering from GitHub fails (e.g. network down during removal), the runner name is recorded in `runners/.orphaned-registrations`. Delete those runners in GitHub Settings → Actions → Runners, then delete the file.

## License

MIT
