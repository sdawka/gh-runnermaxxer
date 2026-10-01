# gh-runnermaxxer

A terminal UI for running many GitHub Actions self-hosted runners on one machine, across any number of repositories and organizations.

```
gh-runnermaxxer 4.0.0 · tick 2s ago · gh: you (repo, admin:org)
  PROJECT / RUNNER         PID     STATE        JOB
◂ ▾ myorg/myrepo           2/2                  auto 1-5  queued 3
    ● mac-1                4121    idle
    ● mac-2                4122    busy         build (12m)
    ✖ mac-5                -       quarantined
  ▾ myorg/other            2/2
    ● mac-3                4123    idle         idle (last: Succeeded)
    ◌ mac-4                -       backoff 18s
  ▾ myorg (org)            0/1/0                ⚠ scope
────────────────────────────────────────────────────────────────────────────────
── event log ───────────────────────────────────────────────────────────────────
2026-09-28T12:00:41 myorg/myrepo: queued 3, scaling 2 -> 2 (max 5)
2026-09-28T12:00:42 mac-4: exited unexpectedly (fails 2), retry in 20s
↑↓ move  ←→ count  Enter apply  Esc discard  d drain  x stop  s start  r restart
```

## Quick Start

```bash
git clone https://github.com/sdawka/gh-runnermaxxer.git && cd gh-runnermaxxer && ./start
```

That's it. `./start` builds the `runnermaxxer` binary (if it's missing or out of date) and opens it. Everything else happens inside the program:

1. On first run, a short setup screen asks for a runner name prefix and the maximum number of runners.
2. Press `a` to add a repository or organization, `←`/`→` to choose how many runners it gets, and `Enter` to apply. The GitHub Actions runner for your OS/arch is downloaded (checksum-verified) the first time it's needed.
3. While the program is open, it supervises your runners: restarting crashed ones, recycling ones GitHub reports offline, and autoscaling if you've enabled it.

**One program, one process.** Runners run only while `runnermaxxer` is open. Quitting (`q`) stops them:

- If no runner is busy, they're stopped right away.
- If jobs are running, you choose: **Stop now** (cancels those jobs), **Wait for jobs, then quit** (runners finish their current job first), or **Cancel**.

Runners stay registered with GitHub, so the next `./start` brings them all back. If the program is killed outright, the next launch adopts any runners that were left running.

## Requirements

- [Go](https://go.dev/dl/) 1.26+ (to build it)
- [GitHub CLI](https://cli.github.com/) (`gh`), authenticated with `gh auth login`
- macOS (Intel or Apple Silicon) or glibc-based Linux (Debian, Ubuntu, Fedora, Arch, ..., including WSL2)

Not supported: Windows, Alpine/musl (the runner requires glibc), WSL1. These are detected and fail fast with a clear message. On Linux, if `libicu` is missing you're pointed at the runner's bundled `installdependencies.sh`.

## Features

- **Many projects, one machine** - runners for several repositories and organizations side by side, grouped by project
- **Self-healing supervisor** - crashed runners restart with exponential backoff; crash-looping runners are quarantined instead of restarting forever
- **GitHub-side health checks** - runners GitHub reports offline (wedged listener, stale credentials) are recycled even if the local process looks alive
- **Drain, don't kill** - scaling down never aborts a job: a busy runner is marked *draining* and removed as soon as its current job finishes
- **Autoscaling within bounds** (opt-in) - give a project `min`/`max` bounds and it grows when GitHub has queued runs and shrinks after runners sit idle
- **Auto-detected labels** - OS, arch, memory, GPU, Docker, etc. (see [Labels](#auto-detected-labels))
- **Crash-safe** - if the program is killed, the next launch re-adopts still-running runners instead of starting duplicates
- **Housekeeping** - auto-downloaded, checksum-verified runner tarball with stale-version warnings; log rotation; config validation
- **Uses `gh` for auth** - no personal access tokens to manage

## Using the TUI

Arrows and vi-style `hjkl` both work. Shifted letters act on every runner instead of the one under the cursor.

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Move cursor |
| `←`, `→`, `+`/`=`, `-`/`_`, `0`-`9` | Change the selected project's pending runner count |
| `Enter` | Apply pending changes (`Esc` discards them) |
| `a` | Add a repository or organization |
| `t` (or `n`) | Projects & scaling screen |
| `d` | Drain (remove) the runner under the cursor; on a project row, opens a picker |
| `D` | Remove the runner under the cursor now (no drain); always confirms |
| `x`/`X` | Stop the runner under the cursor / stop all |
| `s` | Start the runner under the cursor (clears quarantine) |
| `S` | Start all runners |
| `r`/`R` | Restart the runner under the cursor / restart all |
| `l` | Runner logs |
| `L` | Event log |
| `c` | Check runner status on GitHub |
| `e` | Edit configuration |
| `g` | Download the latest runner tarball (when the stale-tarball banner shows) |
| `/` | Filter rows |
| `?` | Help |
| `q`, `ctrl+c` | Quit and stop all runners (asks first if jobs are running or you have unapplied changes) |

**Projects screen (`t`)**: same keys, plus `x` removes the project from the list (only when it has no runners), `b` sets autoscale bounds, and `Enter`/`s` applies and returns to the dashboard. `q`/`Esc` discards pending changes and goes back.

TUI flags (pass them to `./start`, e.g. `./start --no-unicode`): `--no-unicode` (ASCII glyphs), `--dir DIR` (where config, projects and runners live; `./start` uses the repo directory), `--debug FILE` (debug log), `--version`.

## Auto-Detected Labels

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

```yaml
jobs:
  build:
    runs-on: [self-hosted, macos, arm64, docker]
```

## Self-Healing

While the program is open, every tick (`REFRESH_INTERVAL` seconds):

- A runner that dies unexpectedly is restarted with exponential backoff (5s, 10s, 20s, 40s, ...).
- After `MAX_RESTART_ATTEMPTS` consecutive rapid crashes, the runner is **quarantined** (`✖`, with the failure reason). Press `s` to clear the quarantine and retry.
- A runner you stopped on purpose stays down.
- Every `GH_HEALTH_TICKS` ticks, runners are cross-checked against the GitHub API. A runner whose process is alive but that GitHub reports offline twice in a row is recycled. If the API is unreachable this check is skipped, so a network outage never triggers mass restarts.
- The same poll records GitHub's authoritative `busy` flag, which decides whether a runner is busy when scaling down, stopping, or restarting.
- Runner logs are truncated past `MAX_LOG_SIZE_MB` (a `.log.1` copy is kept).
- Stale PID files (e.g. after a reboot) are detected via process-identity checks, so a recycled PID is never mistaken for a live runner.

## Configuration

The setup wizard writes `.runnermaxxer.conf`; edit it from the TUI with `e`, or by hand (see `.runnermaxxer.conf.sample`).

| Variable | Description |
|----------|-------------|
| `RUNNER_NAME_PREFIX` | Prefix for runner names (default: hostname) |
| `MAX_RUNNERS` | Maximum runners across all projects (default: 20) |
| `REFRESH_INTERVAL` | Seconds between supervisor ticks (default: 5) |
| `MAX_RESTART_ATTEMPTS` | Consecutive crashes before a runner is quarantined (default: 5) |
| `MAX_LOG_SIZE_MB` | Truncate runner logs past this size (default: 10) |
| `GH_HEALTH_TICKS` | GitHub health check every N ticks, 0 to disable (default: 12) |
| `SHARED_TOOL_CACHE` | `1` = all runners share one tool cache (default: 1). See [Shared tool cache](#shared-tool-cache) |
| `EPHEMERAL_RUNNERS` | `1` = one job per registration (default: 0). See [Ephemeral runners](#ephemeral-runners) |
| `AUTOSCALE` | `1` = autoscale projects that have bounds (default: 0). See [Autoscaling](#autoscaling) |
| `AUTOSCALE_IDLE_MINUTES` | Minutes a project must have an idle runner before autoscaling removes one (default: 10) |

### Projects

Projects live in `.runnermaxxer.targets`, one per line as `owner/repo`, `org-name`, or a full URL. You rarely need to edit it: `a` and the projects screen (`t`) manage it for you.

```
myorg/myrepo
myorg/other-repo
myorg
```

Each runner registers to exactly one project. Scale-downs remove idle runners first and drain busy ones.

### Autoscaling

With `AUTOSCALE=1`, projects with bounds (set with `b` on the projects screen, or in `.runnermaxxer.targets`) resize themselves after every GitHub poll:

```
myorg/busy-repo min=1 max=5
myorg/quiet-repo max=2        # min defaults to 0
myorg/fixed-repo              # no bounds: fixed count, never autoscaled
```

- **Up**: when the project has queued workflow runs and no idle runner, it grows by the queue depth, up to `max`.
- **Down**: after it has had an idle runner continuously for `AUTOSCALE_IDLE_MINUTES`, it shrinks by one (busy runners are drained, never killed).
- It never goes outside `min`/`max` or over `MAX_RUNNERS` in total. Projects without bounds are never touched.

Limitations: a repo's queue depth counts *every* queued run, including runs waiting for GitHub-hosted runners or for labels these runners don't have, so keep `max` modest on repos that mix runner types. For an **organization**, the signal is only "all our runners for the org are busy", so an org grows one runner per poll while saturated and needs `min` >= 1 to start at all.

### Shared tool cache

By default (`SHARED_TOOL_CACHE=1`) every runner points `RUNNER_TOOL_CACHE` and `AGENT_TOOLSDIRECTORY` at `runners/.toolcache`, so `setup-node`, `setup-python`, etc. download each toolchain once instead of once per runner. Concurrent *first-time* installs of the same tool version can race; if a job fails oddly during a tool install, re-run it. Deleting `runners/.toolcache` is safe.

### Ephemeral runners

With `EPHEMERAL_RUNNERS=1`, new runners register with `--ephemeral`: each registration takes exactly one job, then the supervisor re-registers and relaunches it (not counted as a crash). Ephemeral runners briefly show `restarting...` after each job. The mode is fixed when a runner is set up; to switch an existing runner, scale its project down and back up.

## Directory Structure

```
gh-runnermaxxer/
├── start                        # The one way to run it
├── cmd/runnermaxxer/            # Program entry point (built to bin/runnermaxxer)
├── internal/                    # engine (supervisor), ui (TUI), state, logtail
├── actions-runner-*.tar.gz      # Runner tarball (downloaded automatically)
├── .runnermaxxer.conf           # Your configuration (created by setup)
├── .runnermaxxer.targets        # Your projects
└── runners/                     # Runner instances (auto-created)
    ├── runner-1/ ...
    ├── .pids/                   # PID files (used to re-adopt runners after a crash)
    ├── .logs/                   # runner-N.log, runnermaxxer.log (event log)
    └── .toolcache/              # Shared tool cache
```

## Troubleshooting

**"gh CLI is not authenticated"** - run `gh auth login`.

**"Token may not have access" / `⚠ scope`** - your `gh` auth needs the `repo` scope for repository runners or `admin:org` for organization runners: `gh auth refresh -s admin:org`.

**"another runnermaxxer is already managing ..."** - only one copy can run per directory. Switch to the window that's already running it, or quit it first.

**Runner shows "offline" on GitHub** - the supervisor normally recycles it within a couple of minutes. If it persists, check logs with `l` and restart with `r`.

**Runner is quarantined (`✖`)** - it crashed repeatedly. Check logs with `l`, fix the cause, then press `s` to retry.

**"Failed to configure runner" on Linux** - usually missing runner dependencies: `sudo ./runners/runner-1/bin/installdependencies.sh`.

**Orphaned registrations warning** - if unregistering from GitHub failed, the runner name is recorded in `runners/.orphaned-registrations`. Delete those runners in GitHub Settings → Actions → Runners, then delete the file.

## Development

```bash
make build                 # build bin/runnermaxxer
make test                  # unit + golden tests
make test-integration      # also starts real (fake) runner processes, gh stubbed out
```

## Legacy bash UI

Earlier versions shipped an interactive dashboard written in bash. It has been retired in favour of this self-contained Go program and is archived on the [`bash-ui`](https://github.com/sdawka/gh-runnermaxxer/tree/bash-ui) branch.

## License

MIT
