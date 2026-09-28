# Turning runnermaxxer into a proper TUI: research and failure-mode audit

Date: 2026-09-28. Sources: five research passes over `runnermaxxer.sh` (3.6k lines), the
`actions/runner` `run.sh`/`run-helper.sh` shipped in `runners/runner-3`, GitHub REST docs, and
the Bubble Tea / ratatui / Textual ecosystems. Every claim about the current script below was
re-verified against the code; claims from the research passes that did not survive verification
are listed at the end so they don't get re-reported.

## 1. Recommendation

Keep the bash supervisor as the engine. Build the TUI as a separate **Go + Bubble Tea** client
that talks to the supervisor over a machine-readable state file and the existing scripting CLI.

Why this split:

- The hard, tested logic (supervision, drain, ephemeral re-register, autoscale, locks, service
  install, 17 test files) is already in bash and already has a headless `--daemon` mode. A rewrite
  of that is where the bugs would come from, not the UI.
- The current script's UI is ~600 lines of hand-rolled ANSI (`read_key`, `render_ui`,
  `pick_item`, modal prompts) that fights bash 3.2 and has no layout engine. Every new pane or
  modal costs more than it should.
- Bubble Tea gives multi-pane layout (Lip Gloss), a table widget, viewport for log tail,
  text inputs for forms, resize and mouse handling, and ships as a single static binary
  (~10-15 MB, not the 40-60 MB one research pass claimed). Distribution via goreleaser +
  Homebrew tap. ratatui is an equally good choice if the maintainer prefers Rust; Textual and
  Ink are ruled out by the runtime dependency.
- The TUI can be closed and reopened without touching runners. That is the single biggest UX
  gain over today, where quitting the TUI raises the "leave runners running?" prompt.

### Interface between daemon and TUI

Today's `--status --json` forks bash, reads every state file, and runs `pgrep`/`ps` per
runner. Polling it every 500 ms (as one research pass suggested) is too heavy. Instead:

1. **State snapshot file.** Each supervisor tick, the daemon writes `$PID_DIR/state.json`
   atomically (write tmp, `mv`). It contains what `status_collect` already gathers plus the
   new fields the TUI needs: gh auth state, rate-limit remaining, tarball version vs latest,
   per-runner Runner.Listener version, daemon PID, last tick timestamp, last error per target.
   The TUI watches it with fsnotify (fall back to 1 s polling). Cost: zero extra forks.
2. **Commands** go through the existing CLI (`--scale`, `--drain`, `--stop`, `--start`,
   `--remove`) plus a few new ones the TUI needs (`--add-target`, `--remove-target`,
   `--set-bounds`, `--set-config KEY=VALUE`, `--retry ID` to clear quarantine, `--relogin`).
   The op-lock protocol already serialises these against the daemon tick. Optional later step:
   a Unix socket so commands don't fork bash, but the CLI is enough for v1.
3. **Liveness.** `last_tick` in the snapshot lets the TUI show "daemon stalled 45 s" instead
   of rendering stale data as if it were live. If the daemon isn't running, the TUI offers to
   start it (`--daemon`) or install the service.
4. **Logs.** The TUI tails `$LOG_DIR/runner-N.log` directly. Rotation already truncates in
   place, so a tail that re-seeks to 0 on shrink is sufficient.

### Screen layout

```
┌ gh-runnermaxxer ── daemon 41213 ── tick 2s ago ── gh: sahil ✓ (repo, admin:org) ── rate 4812/5000 ┐
│ tarball 2.334.0  latest 2.337.0 ⚠   fleet: 2.337.0 ×3   disk 41% free                              │
├────────────────────────────────────────────┬─────────────────────────────────────────────────────┤
│ PROJECT / RUNNER    PID    STATE   JOB      │ runner-3 log                                        │
│ ▾ myorg/myrepo  2/2 (min1 max5, auto)      │ 2026-09-28 14:27:45Z: Running job: build            │
│   ● runner-1     4121   idle              │ 2026-09-28 14:28:01Z: Job build completed: Succeeded│
│   ● runner-2     4122   busy    build 12m │ …                                                    │
│ ▾ myorg/other   1/2                        │                                                     │
│   ● runner-3     4123   idle              │                                                     │
│   ◌ runner-4     -      backoff 20s  ✖ exited immediately                                       │
│ ▾ myorg (org)   0/1  ⚠ token lacks admin:org                                                    │
├────────────────────────────────────────────┴─────────────────────────────────────────────────────┤
│ ↑↓/jk move  ←→ count  Enter apply  d drain  x stop  s start  a add project  e config  l logs  ? │
└───────────────────────────────────────────────────────────────────────────────────────────────────┘
```

The status bar is the payoff of this work: most failure modes below become a persistent,
actionable line there rather than a `die()`, a swallowed `2>/dev/null`, or a generic
"crash-looped, press s".

## 2. Failure modes, ranked

Legend: **Gap** = not handled today; **Partial** = handled but wrong or incomplete; **OK** =
handled, TUI should surface it. Line numbers refer to `runnermaxxer.sh` at commit 659d049.

### 2.1 gh authentication and API

| # | Failure | Today | What to do |
|---|---------|-------|------------|
| A1 | **Token lacks `admin:org`** (gh's default login grants `repo, read:org, gist, workflow`). `target_accessible` (665) does a GET that succeeds with `read:org`, so an org target passes the menu check and fails only inside `config.sh`, whose output goes to the runner log with a generic "re-register failed". | **Gap** | At startup and after login: `gh api -i user`, read `X-OAuth-Scopes`. Show scopes in the status bar. Block org targets with a clear "run `gh auth refresh -s admin:org`" line. Note: fine-grained PATs and GitHub App tokens return no `X-OAuth-Scopes`; for those, probe by calling `POST .../actions/runners/registration-token` (403 = no permission) instead. |
| A2 | **Full user OAuth token passed on the config.sh command line** (`--pat "$pat"`, 2097, 2162, 2310). Visible in `ps` for the duration of config.sh, and grants far more than needed. | **Gap** | Mint a registration token via `gh api -X POST repos/O/R/actions/runners/registration-token` (1 h TTL, single-purpose) and pass `--token`. Same for removal (`remove-token`). This also gives the 403/404 status directly, which distinguishes "no admin" (403) from "repo doesn't exist / no read access" (404). |
| A3 | **Token expired or revoked mid-session.** Every gh call is `2>/dev/null`; `check_github_health` (1833) treats auth failure, network failure, and rate limit identically and simply skips. Runners keep running (their own `.credentials` are independent) but scale-up, ephemeral re-register, drain-removal and autoscale silently stop working. | **Gap** | Classify gh failures once, centrally: parse `gh api -i` status line or `gh` stderr into `auth` (401/"not logged in"), `forbidden` (403 + SAML `X-GitHub-SSO` header → show the SSO authorize URL), `notfound` (404), `ratelimit` (403 + `X-RateLimit-Remaining: 0` or 429 → sleep until `X-RateLimit-Reset`), `network` (curl exit / DNS). Write the class and message into the snapshot; TUI shows it in the bar with a keybinding to re-login. |
| A4 | **Headless daemon can't read the token.** `gh auth token` works under launchd/systemd only if the token is in gh's plaintext config; keyring-stored tokens (macOS default when available) are not reachable without a session. Preflight checks `gh auth status` but the service unit gets a minimal PATH and no keychain. | **Partial** | Service install should verify `gh auth token` succeeds *in the unit's environment* (run it via `launchctl`/`systemd-run`), and the unit should set PATH to include gh's location. Offer `GH_TOKEN` env in the unit as the documented daemon path. |
| A5 | **Rate limiting.** Load is per target, not per runner: health poll every `GH_HEALTH_TICKS×REFRESH_INTERVAL` (default 60 s) + one queued-runs call per autoscaled repo. ~10 targets ≈ 1,200 req/h, fine; 50 targets with autoscale ≈ 6,000 req/h, over the 5,000/h limit. No backoff exists. | **Gap** | Read `X-RateLimit-Remaining`/`Reset` from one call per tick (`gh api rate_limit` is free). Scale the poll interval with target count. Show remaining in the bar. Stop polling, not the runners, when exhausted. |
| A6 | **Registration token TTL** (1 h). Fetched immediately before use in `setup_runner`, so fine today; the risk appears if the TUI batch-applies many runners with one token. | OK | Fetch per runner (already the case). Keep it that way. |
| A7 | **Repo renamed / transferred / deleted / archived.** `.runner`'s `gitHubUrl` keeps the old URL; health check then sees a 404 and drops markers (1835-1845) with no message. Autoscale for that target silently stops. | **Gap** | On 404 for a target that has runners, set a target-level error in the snapshot: "GitHub returns 404 for myorg/old-name: renamed, deleted, or access lost". GitHub follows repo renames on the API, so compare the `full_name` returned by `gh api repos/O/R` with the configured one and offer to rewrite the target. |
| A8 | **GHES / `GH_HOST`.** URLs are hard-coded to `https://github.com` (437-438); enterprise-level runners unsupported. | Gap, low priority | Document as unsupported; validate that `gh auth status` host matches the target host when adding one. |

### 2.2 Runner binary lifecycle (download, update, versions)

| # | Failure | Today | What to do |
|---|---------|-------|------------|
| U1 | **Wrapper exit reasons are never inspected.** The runner's own `run.sh` handles self-update internally (run-helper exits 2, run.sh relaunches), so the recorded run.sh PID survives an update and updates are *not* counted as crashes. But run.sh exits **0** on: listener exit 1 ("terminated"), exit 5 ("session conflict", i.e. the same runner registration is connected elsewhere: a cloned dir or two managers), and unknown codes; exit 7 = deprecated version. The supervisor sees "not running", restarts with backoff, and quarantines after 5 with "crash-looped". | **Gap** | The supervisor should read the last run-helper line from the log (`Session Conflict`, `terminated error`, `deprecated version`, `retryable error`) and set a specific `lasterr`: "session conflict: another process holds this runner's registration (cloned dir? second manager?)". Session conflict should quarantine immediately, not after 5 rounds. Set `ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1` in `runner_env` so exit 7 is distinguishable. |
| U2 | **Orphan adoption picks `pgrep … | head -1`** (2202). If that PID is `Runner.Listener` rather than `run.sh` (PID order is not guaranteed after a manager restart), the next self-update *does* exit that PID and gets counted as a crash. | **Partial** | Adopt the process whose command is `run.sh` (or the lowest ancestor in the tree), not the first match. |
| U3 | **Stale tarball → mixed fleet versions.** Shipped tarball is 2.334.0; runner-3 already self-updated to 2.337.0. New runners extract 2.334.0, register, then immediately self-update (downloads ~100 MB each). GitHub refuses runners more than a few releases behind ("runner version too old", listener exits and run-helper logs it). Freshness check (309) is warn-once. | **Partial** | Snapshot should include each runner's version (read `bin/Runner.Listener --version` once per start, cache) and the latest tag. `--download` should be offered from the TUI when the tarball is stale; new runners should be extracted from the newest tarball on disk. |
| U4 | **Checksum parsing** greps `<!-- BEGIN SHA … -->` markers out of the release body (285-302). Works today and is GitHub's actual format, but a body change breaks downloads with "checksum mismatch". | Partial | Keep, but on a parse failure (no marker found) say "couldn't find checksum in release notes" rather than "mismatch", and offer `--download --skip-verify` with a warning. |
| U5 | **Rosetta shell** on Apple Silicon: `uname -m` reports `x86_64`, tool downloads the x64 runner. | **Gap** | `sysctl -n sysctl.proc_translated` = 1 → arch is arm64. |
| U6 | **Disk growth.** `_work/` (job checkouts, tool downloads) and `_diag/` grow unbounded; only the manager's own log is rotated. Disk full mid-job wedges every runner on the box. Pre-check is 500 MB before extraction only. | **Gap** | Show per-runner `_work`+`_diag` size and filesystem free % in the snapshot; warn at 85 %; offer "clean `_work` of idle runners" and "prune `_diag` older than N days" actions. |
| U7 | **Failed self-update leaves `bin.2.x`/`externals.2.x` staging dirs** and `_diag/SelfUpdate-*.log.failed`. | Gap, low | Detect `.failed` and surface as a runner warning; leave cleanup to the runner. |
| U8 | **Gatekeeper.** Only matters if the tarball is downloaded via a browser; curl/gh do not set `com.apple.quarantine`. | OK | Strip the xattr defensively in `download_runner_tarball` and on `detect_runner_tarball`. |
| U9 | **Corrupted or cloned runner dir.** `.runner` missing → "not configured"; `.credentials` corrupt → listener exits, generic crash loop; cloned dir → session conflict (see U1). | Partial | Validate `.runner` is JSON with the expected `gitHubUrl`; treat listener-side credential errors from the log as "re-register needed" and offer it. |

### 2.3 Supervision, state, and platform

| # | Failure | Today | What to do |
|---|---------|-------|------------|
| S1 | **Health-check recycle has no busy guard** (1870-1884). Two consecutive "offline" sightings → `stop_runner_procs` → SIGINT/TERM/KILL over 15 s, which cancels a running job. GitHub reports a runner that is running a job as online, so this mostly can't fire mid-job, except around laptop sleep/wake where the listener reconnects late. 120 s grace after start exists. | Partial | Skip recycle if `ghbusy` was set within the last poll or a `Running job:` log line is newer than the last `completed` line. Suppress the health check for ~2 min after wake (macOS: `pmset -g log` or compare wall clock vs monotonic tick gap). |
| S2 | **Stop kills jobs after 15 s.** `stop_runner_procs` escalates INT→TERM→KILL with 10 s + 5 s waits. Both INT and TERM make the listener cancel the current job, so "stop" is always job-cancelling. Drain is the safe path and exists, but `x` (stop all) and `r` (restart all) are one keypress away. | OK by design | TUI: "stop"/"restart" on a busy runner must show a confirm modal naming the job that will be cancelled, defaulting to drain. |
| S3 | **Drain has no timeout and is lost on manager restart only if the marker is** — it isn't; `.draining` is a file, so it survives. A drain waits forever on a wedged runner that GitHub reports busy. | Partial | Show drain age in the table; offer "force stop" after the user chooses. |
| S4 | **Ephemeral re-register retries every tick without backoff when `gh auth token` fails** (reconfigure returns before writing its marker line at 2149, so `ephemeral_job_finished` stays true). Cheap (local gh call, no API), but noisy and hides the real error behind "no gh token". | Partial | Write the marker before `get_pat`, so the failure counts as a crash and gets backoff; the auth error then surfaces through A3. |
| S5 | **Autoscale scales up on queued runs that no runner can satisfy** (label mismatch, `runs-on` typo). Bounded by `max`, but wastes runners. Org targets use a heuristic (all busy → 1) since there's no org-level queue endpoint. | Partial | Read `runs-on` labels from queued jobs (`/actions/runs/{id}/jobs`, `labels[]`) and only count jobs whose labels are a subset of the fleet's. Show "N queued, M unsatisfiable" in the project row. |
| S6 | **Lock semantics.** Both `LOCK_FILE` and `OP_LOCK_FILE` are noclobber pidfiles with stale-holder reclaim, so SIGKILL is handled. Residual risk is PID reuse after reboot: a stale lock whose PID now belongs to any live process blocks startup. | Partial | Store `pid:starttime` (from `ps -o lstart=`) and compare both. Same fix applies to `is_running`'s substring match on `runner-N/`, which is fine in practice but could be made exact with the start time. |
| S7 | **Two managers.** TUI + daemon are mutually excluded by the lock, and the CLI routes through the op lock. Fine. A second *machine* with the same `RUNNER_NAME_PREFIX` registering to the same repo with `--replace` silently steals the registration; the first machine's runner then hits session conflict (U1). | Gap | Default prefix is hostname, which mostly avoids this. Detect via U1's session-conflict classification and say so. |
| S8 | **launchd/systemd environment.** PATH in the unit may not include `gh` (Homebrew's `/opt/homebrew/bin`), HOME may be wrong, keychain unavailable (A4). systemd user units die at logout without `loginctl enable-linger`. | Partial | `install_service` should bake the absolute path to `gh` into the unit, check linger on Linux, and run a smoke test (`--status`) through the service manager after install. |
| S9 | **Log rotation** copies then truncates in place (`: > "$f"`), which is correct for an append-mode fd. | OK | Nothing. |
| S10 | **bash 3.2.** No bash-4-only constructs found (no `declare -A`, `${x,,}`, `mapfile`). | OK | Nothing. |
| S11 | **Non-tty stdin.** `run_onboarding` and the menus use bare `read`; under launchd/CI the script hangs on first run rather than failing. | Gap | `[[ -t 0 ]]` guard: no tty → print what's missing and exit 2. Moot once the TUI is a separate binary and the daemon is strictly non-interactive. |

### 2.4 Config, targets, and flows

| # | Failure | Today | What to do |
|---|---------|-------|------------|
| C1 | **Config is `source`d** (407). Any shell in `.runnermaxxer.conf` executes. Values are written unescaped by `save_config` (501-519). | Gap | Parse `KEY="value"` lines with a whitelist of keys and a regex per value; never `source`. |
| C2 | **Numeric config unvalidated.** `REFRESH_INTERVAL=0` busy-loops; non-numeric values break arithmetic. `sanitize_settings` clamps some. | Partial | Clamp everything on load; show the clamped value with a note in the config screen. |
| C3 | **Runner name > 64 chars** rejected by GitHub; `valid_prefix` checks characters only. | Gap | Cap prefix so `prefix-NN` ≤ 64. |
| C4 | **Partial batch apply.** `apply_target_counts` sets up runners sequentially; a failure at runner 3 of 5 leaves 2 set up and a generic message. | Partial | Pre-flight the batch (token, scopes, disk, target access) then apply; report per-runner results in a modal. |
| C5 | **Targets removed from file while runners exist** are kept as "known" from disk, so nothing breaks; they just can't be edited. | OK | Show them as "(not in targets file)" with an option to re-add or drain. |
| C6 | **Label drift** after a hardware change; labels are set only at registration. | Gap, low | Show current vs detected labels per runner; "re-label" action via `PATCH .../runners/{id}/labels`. |
| C7 | **Stale labels / label count.** GitHub caps labels at 100 per runner; `detect_labels` emits ~10, so this is not reachable. | OK | Nothing. |
| C8 | **SSH URLs** (`git@github.com:o/r.git`) rejected. | Gap, trivial | Normalise in `normalize_url`. |
| C9 | **`--scale` on an inaccessible target** skips the `target_accessible` check the menu does. | Gap | Same pre-flight as C4. |

## 3. Suggested order of work

1. **Daemon-side plumbing first**, all in bash, all testable with the existing harness:
   state snapshot file; central gh error classifier (A3) used by every gh call; registration
   tokens instead of `--pat` (A2); scope check (A1); wrapper-exit classification (U1, U2);
   runner version in snapshot (U3); config parser instead of `source` (C1); the new CLI verbs.
2. **TUI v1** in Go: dashboard table + status bar + log pane + confirm modals, driving the
   CLI. Ship alongside the bash script; `runnermaxxer.sh` keeps its current UI as the fallback
   until v1 is stable, then the bash UI code (~600 lines) is deleted.
3. **Robustness pass**: rate-limit awareness (A5), sleep/wake guard (S1), disk usage (U6),
   label-aware autoscale (S5), service-install smoke test (S8).

## 4. Research claims rejected after verification

Listed so they are not re-raised.

- "Self-update restarts are counted as crashes." No: `run.sh` loops internally; the recorded
  PID survives an update. The real gap is U1/U2.
- "Log rotation loses lines / needs `mv`." No: truncation in place is correct for an
  append-mode fd, and `mv` would not free space. See S9.
- "Locks aren't released on SIGKILL." Both locks reclaim dead holders. Residual risk is PID
  reuse only (S6).
- "Bash 4 features are used." None found (S10).
- "Ephemeral re-registration races a lingering registration." `--replace` is passed; no race.
- "`gh auth token --json scopes`." Not a real flag; use the `X-OAuth-Scopes` header.
- "Runners are in the TUI's process group and die on Ctrl-C." `setsid nohup` is used (2224).
- "Go binary is 40-60 MB." Typical Bubble Tea binaries are 10-15 MB.
- "200 runners → 24k req/h." API load scales with targets, not runners (A5).
