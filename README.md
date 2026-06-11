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
- **Config validation** - detects invalid URLs, bad values, and offers to fix them
- **Scale runners up/down** with a single keypress
- **Auto-detect labels** based on system capabilities (OS, arch, memory, GPU, Docker, etc.)
- **Auto-detect runner tarball** - finds the correct one for your platform
- **Live status** showing what each runner is doing (idle, running job, errors)
- **Works with repos or orgs** - configure once, spin up runners
- **Uses `gh` CLI** for authentication - no PAT management needed

## Requirements

- [GitHub CLI](https://cli.github.com/) (`gh`) - authenticated with `gh auth login`
- GitHub Actions runner tarball for your platform

## Quick Start

1. **Download the runner tarball** from [actions/runner releases](https://github.com/actions/runner/releases)

   ```bash
   # Example for macOS ARM64
   curl -LO https://github.com/actions/runner/releases/download/v2.320.0/actions-runner-osx-arm64-2.320.0.tar.gz
   ```

2. **Clone and run**

   ```bash
   git clone https://github.com/YOUR_USERNAME/gh-runnermaxxer.git
   cd gh-runnermaxxer
   chmod +x runnermaxxer.sh
   ./runnermaxxer.sh
   ```

3. **Configure** - on first run, an interactive setup wizard guides you through configuration

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

## CLI Options

```bash
./runnermaxxer.sh [OPTIONS]

Options:
  --setup, -s    Run interactive setup wizard
  --help, -h     Show help message
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

Download from https://github.com/actions/runner/releases - the script auto-detects tarballs matching your OS/architecture.

**"Token may not have access"**

Ensure your `gh` auth has the `repo` scope (for repository runners) or `admin:org` scope (for organization runners).

**Runner shows "offline" on GitHub**

The runner process may have crashed. Check logs with `l` and restart with `r`.

## License

MIT
