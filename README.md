# gh-runnermaxxer

A TUI for managing multiple GitHub Actions self-hosted runners on a single machine.

```
  ╔═══════════════════════════════════════════════════╗
  ║            gh-runnermaxxer                        ║
  ╚═══════════════════════════════════════════════════╝

  Labels: macos, arm64, apple-silicon, docker, high-memory

  Status: 3 running / 4 configured

  myorg/myrepo
    ● runner-1 PID 12345 [running: build-and-test (12m)]
    ● runner-2 PID 12346 [idle]
  myorg/other-repo
    ● runner-3 PID 12347 [idle (last: Succeeded)]
    ○ runner-4 stopped
  myorg (organization)  (no runners)
```

One instance manages runners for any number of repositories and organizations. At startup a project menu lets you pick how many runners each one gets:

```
  Projects   runners: 4 (max 20)

  ▸ myorg/myrepo                             [◂  2 ▸]
    myorg/other-repo                         [   2  ]
    myorg (organization)                     [   1  ]  0 → 1

  ↑/↓ choose project      ←/→ runners (or +/-, 0-9)
  a add project   x remove from list   Enter/s apply & continue   q quit
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
- **Scale runners up/down** with a single keypress - scale-down prefers idle runners
- **Drain mode** - scaling down never aborts a job: a busy runner is marked *draining* (shown in yellow), stops counting toward its project, and is deregistered and removed as soon as its current job finishes. Scaling back up cancels pending drains before setting up new runners
- **Auto-detect labels** based on system capabilities (OS, arch, memory, GPU, Docker, WSL, etc.)
- **Live status** showing what each runner is doing (idle, running job with elapsed time, errors, quarantined)
- **Runner tarball freshness check** - at startup, warns (once, non-fatally) if the shipped tarball is older than the latest `actions/runner` release, so you know new runners are being extracted from a stale version; the latest-release lookup is cached for 24h
- **Multiple projects in one instance** - run runners for several repositories and organizations side by side; each runner is registered to exactly one of them and the dashboard groups runners by project
- **Project menu at startup** - arrow-key menu listing your projects from `.runnermaxxer.targets`: ↑/↓ picks a project, ←/→ sets its runner count; `a` adds a new repo/org on the spot; Enter applies by adding or removing runners per project
- **Headless daemon and service install** - `--daemon` supervises without a UI; `--install-service` sets it up as a launchd agent (macOS) or systemd user service (Linux)
- **Scripting CLI** - `--status [--json]`, `--scale owner/repo=N`, `--drain/--stop/--start/--remove ID`, with meaningful exit codes
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

2. **Configure** - on first run, an interactive setup wizard asks for a runner name prefix and a global runner cap

3. **Runner tarball** - if none is present, the script offers to download the latest release for your platform (checksum-verified). You can also fetch it explicitly with `./runnermaxxer.sh --download`, or download manually from [actions/runner releases](https://github.com/actions/runner/releases). Once one is present, every startup checks (at most once per 24h) whether it's fallen behind the latest release and prints a one-line notice with the update command if so - new runners are always extracted from whatever tarball is on disk, so a stale one goes unnoticed otherwise

4. **Pick projects and counts** - in the startup menu press `a` to add a repository or organization, use ↑/↓ to pick it and ←/→ to set how many runners it gets, then Enter to apply and open the dashboard

## Usage

| Key | Action |
|-----|--------|
| `↑`/`↓` | Select a project |
| `←`/`→` (or `+`/`-`, `0-9`) | Change the selected project's runner count |
| `Enter` | Apply pending runner-count changes (`Esc` discards them) |
| `d` | Remove a specific runner (arrow-key picker); a busy runner can be drained (default), killed now, or left alone |
| `s` | Start all runners |
| `x` | Stop all runners |
| `r` | Restart all runners |
| `t` | Projects & scaling menu (add repos/orgs, set runner counts) |
| `l` | View runner logs |
| `c` | Check GitHub runner status (also triggers a fresh busy/online poll) |
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

While the TUI (or `--daemon`) is running, a supervisor runs every refresh tick:

- A runner that dies unexpectedly is restarted with exponential backoff (5s, 10s, 20s, 40s, ...).
- After `MAX_RESTART_ATTEMPTS` consecutive rapid crashes, the runner is **quarantined** (shown as `✖` with the failure reason) so it can't crash-loop forever. Press `s` (start all) or `r` to clear the quarantine and retry.
- A runner stopped on purpose (via `x` or `-`) stays down.
- Every `GH_HEALTH_TICKS` ticks, local runners are cross-checked against the GitHub API; a runner whose process is alive but that GitHub reports offline twice in a row is recycled. This check is skipped when the API is unreachable, so a network outage never triggers mass restarts. The first check runs on the first tick after startup.
- The same poll records GitHub's authoritative `busy` flag for each runner. While that data is fresh (younger than about two poll intervals, minimum 30s), it decides whether a runner is busy (used when scaling down, stopping, or restarting) and corrects the status column: a runner whose log says it is running a job but GitHub reports idle is shown as `idle`, and one GitHub reports busy but the log does not is shown as `busy (per GitHub)`. When the data is stale, the project could not be fetched, or `GH_HEALTH_TICKS=0`, status falls back to parsing the runner log. The dashboard header shows how old the data is (`GitHub: polled 12s ago`).
- Runner logs are truncated past `MAX_LOG_SIZE_MB` (a `.log.1` copy is kept).
- Stale PID files (e.g. after a reboot) are detected via process-identity checks, so a recycled PID is never mistaken for a live runner or killed by accident.

Runners are detached processes: quitting the manager can leave them running (you'll be asked), but they are only supervised while the TUI or the [daemon](#running-as-a-service) is running.

## CLI Options

```bash
./runnermaxxer.sh [OPTIONS]

Options:
  --setup, -s     Run interactive setup wizard
  --download, -d  Download the latest runner tarball for this platform
  --target, -t X  Add project X (owner/repo, org, or URL) to the list and highlight it
  --no-menu       Skip the project menu at startup and go straight to the dashboard
  --version, -v   Print version
  --help, -h      Show help message

Headless:
  --daemon             Supervise runners without a UI (events: runners/.logs/runnermaxxer.log)
  --stop-daemon        Stop a running daemon (runners keep running)
  --install-service    Run the daemon at login (launchd on macOS, systemd --user on Linux)
  --uninstall-service  Remove that service

Scripting (no UI; exit status 0 = ok, 1 = failed, 2 = bad usage):
  --status [--json]    Projects and runners with their state
  --scale TARGET=N     Set a project's runner count (repeatable; TARGET as for --target)
  --drain ID           Remove runner ID once its current job finishes
  --stop ID            Stop runner ID (stays down until --start)
  --start ID           Start runner ID (clears quarantine)
  --remove ID          Unregister and delete runner ID now
```

## Running as a Service

`--daemon` runs the same supervisor as the TUI (restarts, backoff, quarantine, GitHub health checks, drains) with no terminal UI. It needs a valid `.runnermaxxer.conf` (run `--setup` first; there is no onboarding in headless mode) and never shows the project menu - set runner counts with `--scale` or the TUI beforehand. Each notable event (runner started, restarted, quarantined, recycled, draining, removed, daemon start/stop) is appended as one timestamped line to `runners/.logs/runnermaxxer.log`.

On SIGTERM/SIGINT the daemon exits cleanly and **leaves runners running**; the next daemon or TUI re-adopts them. `--stop-daemon` sends SIGTERM and waits up to 10s.

To start it automatically at login and restart it if it dies:

```bash
./runnermaxxer.sh --install-service
```

- **macOS**: writes `~/Library/LaunchAgents/com.gh-runnermaxxer.plist` (`RunAtLoad`, `KeepAlive`, `AbandonProcessGroup` so runners outlive the daemon) and loads it with `launchctl`. `PATH` includes the directory `gh` was found in at install time. Daemon stdout/stderr go to `runners/.logs/daemon.out`.
  - Status: `launchctl print gui/$(id -u)/com.gh-runnermaxxer`
  - Logs: `tail -f runners/.logs/runnermaxxer.log runners/.logs/daemon.out`
- **Linux**: writes `~/.config/systemd/user/gh-runnermaxxer.service` (`Restart=always`, `KillMode=process` so runners outlive the daemon) and runs `systemctl --user daemon-reload && systemctl --user enable --now gh-runnermaxxer.service`. User services stop when you log out unless lingering is enabled: `loginctl enable-linger $USER`.
  - Status: `systemctl --user status gh-runnermaxxer`
  - Logs: `journalctl --user -u gh-runnermaxxer -f` and `runners/.logs/runnermaxxer.log`

`--uninstall-service` stops and removes the service (runners keep running, unsupervised). Because the service restarts the daemon, use `--uninstall-service` rather than `--stop-daemon` to stop it for good. `gh` must be authenticated for the user the service runs as (`gh auth status`).

The daemon and the TUI share one lock, so only one of them supervises at a time. Opening the TUI while the daemon runs prints `a daemon is running (pid N); use --status / --scale, or stop it with --stop-daemon` and exits. If the TUI is open when the service starts, the service's daemon exits and is retried until you quit the TUI.

## Scripting / CLI

These commands never open the UI, print plain text when stdout is not a terminal, and exit `0` on success, `1` on failure, `2` on bad usage.

```bash
./runnermaxxer.sh --status                 # human-readable
./runnermaxxer.sh --status --json | jq .   # machine-readable
./runnermaxxer.sh --scale myorg/myrepo=3 --scale myorg=1
./runnermaxxer.sh --drain 4                # remove runner-4 after its current job
./runnermaxxer.sh --stop 2 && ./runnermaxxer.sh --start 2
./runnermaxxer.sh --remove runner-5        # "runner-" prefix is optional
```

`--status --json` prints one object:

```json
{"version":"3.0.0","daemon_pid":4242,"max_runners":20,"github_polled_at":1727430000,
 "projects":[{"url":"https://github.com/myorg/myrepo","label":"myorg/myrepo","listed":true,
              "configured":2,"running":2,"busy":1,"draining":0}],
 "runners":[{"id":1,"project":"https://github.com/myorg/myrepo","state":"busy","pid":12345,
             "status":"running: build (3m)","ephemeral":false,"last_error":null}]}
```

`state` is one of `running`, `idle`, `busy`, `draining`, `stopped`, `quarantined`, `restarting`; `pid` and `daemon_pid` are `null` when not running; `project` is `null` for a runner whose setup never completed; `listed` is false for a project that has runners but isn't in `.runnermaxxer.targets`.

`--scale TARGET=N` accepts the same target forms as `--target` (`owner/repo`, `org`, or a URL), adds the target to `.runnermaxxer.targets` if needed, and adds or removes runners like the project menu does (busy runners are drained, not killed). It refuses changes that would put the total across all projects over `MAX_RUNNERS`.

Locking: `--status` is read-only and takes no lock. The mutating commands (`--scale`, `--drain`, `--stop`, `--start`, `--remove`) take the manager lock when nothing else is running, and refuse to run while the TUI is open (make the change there). While the daemon is running they instead take a short-lived operation lock that the daemon also takes around each supervisor tick, so a command and a tick never act on runners at the same time; the daemon picks up the result on its next tick. Without a daemon, runners started this way are not supervised until you start one.

## Configuration

On first run (or with `--setup`), an interactive wizard guides you through setup:

1. Set runner name prefix (default: hostname)
2. Set maximum runners across all projects (default: 20)

Projects themselves are chosen in the menu that follows (see [Multiple Projects](#multiple-projects)).

Configuration is validated on startup. If issues are detected (invalid URLs, bad values), you'll be prompted to fix them.

### Config File

Configuration is stored in `.runnermaxxer.conf`. You can also copy the sample:

```bash
cp .runnermaxxer.conf.sample .runnermaxxer.conf
# Edit as needed
```

| Variable | Description |
|----------|-------------|
| `RUNNER_NAME_PREFIX` | Prefix for runner names (default: hostname) |
| `MAX_RUNNERS` | Maximum runners allowed across all projects (default: 20) |
| `REFRESH_INTERVAL` | Seconds between automatic status refreshes (default: 5) |
| `MAX_RESTART_ATTEMPTS` | Consecutive crashes before a runner is quarantined (default: 5) |
| `MAX_LOG_SIZE_MB` | Truncate runner logs past this size (default: 10) |
| `GH_HEALTH_TICKS` | GitHub-side health check every N ticks, 0 to disable (default: 12) |
| `SHARED_TOOL_CACHE` | `1` = all runners share one tool cache in `runners/.toolcache` (default: 1). See [Shared Tool Cache](#shared-tool-cache) |
| `EPHEMERAL_RUNNERS` | `1` = register new runners with `--ephemeral`, one job per registration (default: 0). See [Ephemeral Runners](#ephemeral-runners) |

`SHARED_TOOL_CACHE` and `EPHEMERAL_RUNNERS` can also be toggled from the dashboard with `e`.

### Shared Tool Cache

Every runner is a full copy of the runner package, and by default each keeps its own tool cache in `_work/_tool`, so `actions/setup-node`, `setup-python`, etc. download the same toolchain once per runner. With `SHARED_TOOL_CACHE=1` every runner is launched with `RUNNER_TOOL_CACHE` and `AGENT_TOOLSDIRECTORY` pointing at `runners/.toolcache`. The runner resolves its tool directory from `RUNNER_TOOL_CACHE` (falling back to `RUNNER_TOOLSDIRECTORY`, then `AGENT_TOOLSDIRECTORY`; see `HostContext.cs` in [actions/runner](https://github.com/actions/runner)) and the setup-* actions read `RUNNER_TOOL_CACHE`; both are set to be safe. A value set in a runner's own `.env` file overrides this.

- The change applies as runners (re)start (`r` restarts all).
- Concurrent **first-time** installs of the same tool version by several runners can race (two jobs extracting into the same directory at once). Once a version is cached, sharing it is safe. If a job fails oddly during a tool install, re-run it, or pre-warm the cache with a single runner.
- Deleting `runners/.toolcache` is safe (ideally while no jobs are running); tools are simply re-downloaded on next use.

### Ephemeral Runners

With `EPHEMERAL_RUNNERS=1`, new runners are registered with `--ephemeral`: each registration takes exactly one job, then the runner exits, GitHub deletes the registration, and the runner removes its local `.runner`/`.credentials`. This gives every job a registration of its own (no state carried in the runner's credentials between jobs; note the `_work` directory and tool cache on disk are still reused).

The manager treats that exit as normal: the supervisor re-runs `config.sh` in the same runner directory (no re-extraction) and relaunches it right away, without counting it towards the crash/quarantine limit. The runner's project is remembered in `runners/.pids/runner-N.target`, so it stays listed under its project while between jobs. Ephemeral runners are tagged `ephemeral` in the dashboard, which is why they briefly show `restarting...` after each job. Removing one whose registration GitHub already deleted is fine and is not recorded as an orphan.

The mode is fixed when a runner is set up: changing `EPHEMERAL_RUNNERS` only affects runners added afterwards. To switch an existing runner, remove it and add it again (e.g. scale its project down and back up).

Older configs that still contain `REPO_URL`/`ORG_URL` are migrated automatically: the URL is appended to `.runnermaxxer.targets` and removed from the config.

### Multiple Projects

Each self-hosted runner registers against exactly one repository or organization. To serve several projects from one machine you run several runners, and a single manager instance supervises all of them (the lock file prevents a second instance on purpose).

Projects are listed in `.runnermaxxer.targets` (see `.runnermaxxer.targets.sample`), one per line as `owner/repo`, `org-name`, or a full URL:

```
sdawka/mountpain
myorg/other-repo
myorg
```

You rarely need to edit it by hand: the **project menu** at startup (and `t` in the dashboard) manages it.

| Key | Action |
|-----|--------|
| `↑`/`↓` (or `k`/`j`) | Move between projects |
| `←`/`→` (or `h`/`l`, `+`/`-`, `0`-`9`) | Change the highlighted project's runner count |
| `a` | Add a repository or organization (also appended to `.runnermaxxer.targets`) |
| `x` | Remove the highlighted project from the list (only when it has no runners) |
| `Enter` / `s` | Apply: each project is scaled up or down to the chosen count, then open the dashboard |
| `q` | Quit (at startup) or go back (from the dashboard) |

Applying scales each project independently: new runners are registered to that project, and scale-downs unregister idle runners first (you are warned before a busy runner is killed). The counts shown come from the runner directories on disk, so a project set to `0` simply has no runners; setting it back up re-registers fresh ones. Runners registered to a project that is not in the targets file still appear (marked as such) so they can be scaled down.

`./runnermaxxer.sh --target owner/repo` adds a project from the command line and highlights it in the menu; `--no-menu` skips the menu when the current runners are already what you want.

In the dashboard, `+` asks which project to add a runner to when there is more than one; `-` removes a runner by number regardless of project.

## Directory Structure

```
gh-runnermaxxer/
├── runnermaxxer.sh              # Main script
├── actions-runner-*.tar.gz      # Runner tarball (you download this)
├── .runnermaxxer.conf.sample    # Sample configuration
├── .runnermaxxer.conf           # Your configuration (auto-created via setup)
├── .runnermaxxer.targets        # Projects (repos/orgs) shown in the startup menu
└── runners/                     # Runner instances (auto-created)
    ├── runner-1/
    ├── runner-2/
    ├── .pids/
    ├── .logs/                   # runner-N.log, runnermaxxer.log (daemon events), daemon.out
    ├── .daemon.pid              # PID of a running --daemon
    └── .toolcache/              # Shared tool cache (SHARED_TOOL_CACHE=1)
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

## Tests

```bash
./tests/run.sh
```

A tiny plain-bash test harness (no dependencies, bats not required) that sources `runnermaxxer.sh` as a library against a temporary runner directory and exercises its pure/near-pure functions (URL/target parsing, the project menu, log-status parsing, key decoding, etc.) without ever touching real runners or calling `gh`.

## License

MIT
