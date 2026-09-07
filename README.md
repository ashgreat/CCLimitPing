<p align="center">
  <img src="assets/icon.png" alt="CCLimitPing icon" width="160">
</p>

# CCLimitPing (`limitping`)

**English** | [中文](README.zh-CN.md)

[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![CI](https://github.com/ashgreat/CCLimitPing/actions/workflows/ci.yml/badge.svg)](https://github.com/ashgreat/CCLimitPing/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ashgreat/CCLimitPing?include_prereleases&sort=semver)](https://github.com/ashgreat/CCLimitPing/releases)
![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)
![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey)

Start the next **Claude Code**, **Codex**, or **Spark** rate-limit window the
moment the previous one resets.

> This hardened fork defaults to Claude-only operation, read-only credential
> access, opt-in hooks, verified release downloads, and a managed macOS service.
> The upstream project is [wavever/CCLimitPing](https://github.com/wavever/CCLimitPing).

Claude Code, Codex, and Spark subscription limits run on **5-hour rolling
windows** (plus a weekly cap). A fresh 5h window does not start just because the
previous one reset; it starts when you send the first billable request. If that
happens hours later, the gap is wasted and your window schedule drifts.

`limitping` watches the reset time and sends one tiny request through the
official provider CLI right after rollover. Run it once, keep `watch` in the
foreground, or start a detached `bg` watcher that keeps your window chain alive
after the terminal closes.

```
claude  ✓ pinged (6.6s)
codex   ✓ pinged (13.6s)
spark   ✓ pinged (12.4s)
```

## Highlights

- Keeps 5h windows continuous by pinging as soon as a reset is safely available.
- Runs the way you do: one-shot `ping`, foreground `watch`, or detached
  `bg start` with `bg status`, `bg logs -f`, and `bg stop`.
- Shows 5h and weekly usage, reset countdowns, and background watcher state from
  read-only usage endpoints.
- Triggers Claude Code, Codex, and Spark through their official CLIs using your
  existing logged-in credentials.
- Detects active Claude/Codex turns via CLI hooks; Spark uses the Codex hook
  signal because it runs through the Codex CLI.
- Auto-resumes parked tasks: `limitping continue <provider>` proxies the
  official CLI and types your continue message the moment the 5h limit
  recovers, so an overnight task doesn't sit at the limit until morning.
- Includes dry-run modes, weekly-limit guards, reset buffers, cheap-model
  defaults, macOS notifications, local config, and no telemetry.

## Quick start

```sh
git clone https://github.com/ashgreat/CCLimitPing.git
cd CCLimitPing
go build -o bin/limitping ./cmd/limitping
install -m 0755 bin/limitping ~/.local/bin/limitping
limitping config init
limitping status
limitping ping --dry-run
limitping service install claude  # macOS: starts now and after login/reboot
limitping service status
```

Use dry-run first if you want to inspect what would happen without consuming
provider quota: `limitping ping --dry-run`, `limitping watch --dry-run`, or
`limitping bg start --dry-run`.

## Supported providers

| Provider | Read usage (zero-quota) | Trigger | Auth |
|---|---|---|---|
| **Claude Code** | `…/api/oauth/usage` | Claude Code print mode | OAuth (Keychain / `~/.claude`) |
| **Codex** | `…/backend-api/wham/usage` | interactive Codex CLI | OAuth (`~/.codex/auth.json`) |
| **Spark** | `…/backend-api/wham/usage` (`additional_rate_limits`) | interactive Codex CLI with `gpt-5.3-codex-spark` | OAuth (`~/.codex/auth.json`) |

## How it works

Two cleanly separated jobs:

| Job | Mechanism | Cost |
|-----|-----------|------|
| **Trigger** a new window | the official CLI (Claude print mode / interactive Codex) | a tiny slice of quota (this is the point) |
| **Read** usage & reset times | zero-quota usage endpoints (the same ones CodexBar / community plugins use) | none — never starts a window |

When `watch` sees a 5h window has reset, it first checks whether a Claude/Codex
session is actively mid-turn. If one is, `limitping` waits and re-reads usage
instead of sending its own ping, because that session's next model request will
start the new window naturally. Spark uses the Codex activity signal. This check
relies on the optional [CLI hooks](#active-session-detection-hooks); without
them, `limitping` skips the check and pings as soon as the window resets.

- **Claude**: reads `GET https://api.anthropic.com/api/oauth/usage` using the
  OAuth token from the macOS Keychain (`Claude Code-credentials`) or
  `~/.claude/.credentials.json`. Triggering uses the non-interactive
  `claude -p --model <model> "<prompt>"` path. It returns a dependable process
  status under a LaunchAgent and, in current Claude Code releases, starts the
  same subscription-backed five-hour window. If the usage endpoint returns an ambiguous 429, limitping
  uses the free token-counting endpoint (which does not create a Message) to
  distinguish a real endpoint throttle from Claude Code subscription access
  being disabled.
- **Codex**: reads `GET https://chatgpt.com/backend-api/wham/usage` using the
  OAuth token from `~/.codex/auth.json`. Triggering uses a TTY-backed
  interactive `codex "<prompt>"` session; headless `codex exec` can consume
  tokens without anchoring the subscription-backed Codex window.
- **Spark**: uses the same Codex usage endpoint, OAuth token, hooks, and
  interactive CLI path. It reads the `GPT-5.3-Codex-Spark` entry from
  `additional_rate_limits`, sends the ping with model `gpt-5.3-codex-spark`,
  and appears as a separate `spark` provider.

Claude/Codex tokens are reused from the official tools (no separate login).
Credential refresh and write-back are disabled by default. On a 401, limitping
first reloads the official credential store, then fails closed and asks you to
log in again. Set `refresh_credentials = true` for a provider only if you
explicitly accept automatic OAuth refresh and write-back. Spark reuses Codex's
setting and token.

## Install

`limitping` ships as a single self-contained binary — **no Go required**.

**Reviewed install script** (macOS / Linux):

```sh
git clone https://github.com/ashgreat/CCLimitPing.git
cd CCLimitPing
less install.sh
sh install.sh
```

Downloads the right prebuilt binary and verifies it against the SHA-256 file
published with the [latest release](https://github.com/ashgreat/CCLimitPing/releases/latest),
then installs into `/usr/local/bin` (or `~/.local/bin`). Override with
`LIMITPING_INSTALL_DIR`. The installer never modifies Claude or Codex settings.

**Upgrade** — replace the installed binary with the latest release:

```sh
limitping upgrade
```

Aliases: `limitping up`, `limitping update`.

**Uninstall** — remove the installed binary plus config/cache:

```sh
limitping uninstall
```

Aliases: `limitping rm`, `limitping remove`.

Use `limitping uninstall --keep-config` to preserve `~/.config/limitping` (or
`$XDG_CONFIG_HOME/limitping`).

**Manual download** — grab the archive for your platform from the
[Releases](https://github.com/ashgreat/CCLimitPing/releases) page (`.tar.gz` for
macOS/Linux, `.zip` for Windows):

```sh
tar -xzf limitping_darwin_arm64.tar.gz
sudo mv limitping /usr/local/bin/
```

**From source** (developers, needs Go 1.25+):

```sh
git clone https://github.com/ashgreat/CCLimitPing.git
cd CCLimitPing
go build -o bin/limitping ./cmd/limitping
```

Each provider you enable needs its own credentials: the `claude` / `codex` CLIs
logged in. Spark uses the Codex CLI credentials.

## Usage

```sh
limitping config init          # write ~/.config/limitping/config.toml
limitping status               # show 5h/weekly % + reset countdowns (alias: s)
limitping status --json        # machine-readable JSON for each provider
limitping status -v            # also print raw JSON
limitping ping                 # trigger all enabled providers now (alias: p)
limitping ping claude          # Claude only
limitping ping codex           # Codex only
limitping ping spark           # Spark only
limitping ping --dry-run       # show the commands without sending
limitping watch                # foreground daemon: ping each window at reset (alias: w)
limitping watch claude         # watch only one provider (claude|codex|spark)
limitping watch --live         # optional live heartbeat/status line
limitping watch --dry-run      # log when pings would fire, without sending
limitping schedule codex --at 05:00 --at 13:00  # ping at daily local times
limitping schedule --every 5h  # ping on a fixed interval instead of reset time
limitping redeem --dry-run     # show which Codex reset credit would be spent
limitping redeem               # spend it now (irreversible)
limitping continue codex       # proxy the CLI; auto-resume the task on 5h recovery
limitping continue codex --yolo             # flags after the provider pass through
limitping continue claude --dangerously-skip-permissions
limitping bg start             # run watch in the background, freeing the terminal
limitping bg status            # running? + each watched provider's usage (alias: limitping bg)
limitping bg logs -f           # follow the background watcher's log
limitping bg stop              # stop the background watcher
limitping service install claude # macOS persistent service + AC sleep prevention
limitping service status       # show LaunchAgent status and log paths
limitping service uninstall    # stop/remove service; preserve config and logs
limitping hooks install claude # opt in to active-session detection hooks
limitping hooks uninstall      # remove those hooks
limitping version              # print the version (aliases: v, ver)
limitping upgrade              # update to the latest GitHub release (aliases: up, update)
limitping uninstall            # remove limitping plus config/cache (aliases: rm, remove)
```

Short aliases are also available for config commands: `limitping c i` for
`config init` and `limitping c p` for `config path`.

### Command aliases

`limitping --help` lists aliases inline, for example `ping, p`.

| Command | Aliases |
| --- | --- |
| `status` | `s`, `stat` |
| `ping` | `p` |
| `watch` | `w` |
| `schedule` | `sched` |
| `background` | `bg` |
| `config` | `c`, `cfg` |
| `config init` | `c i` |
| `config path` | `c p` |
| `version` | `v`, `ver` |
| `upgrade` | `up`, `update` |
| `uninstall` | `rm`, `remove` |

`ping` shows the exact command and a live timer (a spinner on a terminal).
The provider CLIs do not expose reliable machine-readable per-ping token or
cost data, so success output normally shows elapsed time only:

```
claude  → claude -p --model haiku .
claude  ✓ pinged (6.6s)
codex   → codex -c model_reasoning_effort=low -m gpt-5.4-mini ok
codex   ✓ pinged (13.6s)
spark   → codex -c model_reasoning_effort=low -m gpt-5.3-codex-spark ok
spark   ✓ pinged (12.4s)
```

Use `status` or `bg status` for the authoritative 5h/weekly window view after a
ping.

Example `status`:

```
claude
  5h     [█████░░░░░]  51.0% used      resets in 3h14m    (Sun 00:10 UTC+8)
  weekly [█████░░░░░]  54.0% used      resets in 7h04m    (Sun 04:00 UTC+8)

codex (plus)
  5h     [██░░░░░░░░]  24.0% used      resets in 3h15m    (Sun 00:11 UTC+8)
  weekly [████░░░░░░]  37.0% used      resets in 111h57m  (Thu 12:53 UTC+8)
  reset credits 1 reset available
    - available, granted Jun 17 17:38, expires Jul 17 17:38 UTC+8 (in 24d6h)
```

Text status defaults to **used** percentage. Set `usage_display = "remaining"` if
you prefer the same mental model as Codex's "Usage remaining" UI.

`status --json` returns the same data as a JSON array (one object per provider),
for scripts and dashboards. Progress chatter is suppressed so stdout stays a
single valid document; a provider that fails to read becomes
`{"provider": "...", "error": "..."}` and the command exits non-zero. Add `-v`
to embed each provider's raw response under `raw`.

A window key (`five_hour` / `weekly`) is omitted when the provider does not
currently enforce that limit — e.g. OpenAI temporarily removed Codex's 5h
window on 2026-07-12, leaving only the weekly cap. Text mode prints
`not currently enforced` for such a window. `watch` rechecks the zero-quota
usage endpoint every 15 minutes without pinging. If it observed a five-hour
window before the reset and that window then disappears, it sends exactly one
ping to anchor the new window; otherwise it treats the account as weekly-only.
The last observed reset is stored as non-secret timing metadata in
`~/.config/limitping/scheduler-state.json`, so a service restart does not lose
the decision context.

```json
[
  {
    "provider": "codex",
    "plan": "plus",
    "five_hour": {
      "used_percent": 24,
      "remaining_percent": 76,
      "active": true,
      "resets_at": "2026-06-17T05:51:45+08:00",
      "remaining_seconds": 11700,
      "window_seconds": 18000
    },
    "weekly": {
      "used_percent": 37,
      "remaining_percent": 63,
      "active": true,
      "resets_at": "2026-06-24T00:51:45+08:00",
      "remaining_seconds": 403020,
      "window_seconds": 604800
    },
    "credits": { "has_credits": false, "unlimited": false, "balance": "0" },
    "reset_credits": {
      "available_count": 1,
      "credits": [
        {
          "status": "available",
          "granted_at": "2026-06-17T17:38:38Z",
          "expires_at": "2026-07-17T17:38:38Z"
        }
      ]
    },
    "limit_reached": false,
    "fetched_at": "2026-06-17T01:00:43+08:00"
  }
]
```

## Configuration

`~/.config/limitping/config.toml` (honors `$XDG_CONFIG_HOME`):

```toml
weekly_threshold = 0.99   # skip pinging when weekly usage >= this (0..1), until weekly reset
reset_buffer     = "10s"  # wait this long after a reset before pinging (ensures rollover)
notify           = true   # macOS notifications on ping/skip/failure
usage_display    = "used" # text status: "used" or "remaining"

[claude]
enabled    = true
refresh_credentials = false # read-only; log in again if the OAuth token expires
prompt     = "."
model      = "haiku"      # cheapest tier; triggering doesn't need a SOTA model
extra_args = []           # extra Claude CLI args; conflicting I/O flags are ignored
align_start = ""          # optional RFC3339 anchor for the first window; empty = start ASAP
continue_prompt = "continue"  # message `continue` injects on 5h recovery; empty = "continue"

[codex]
enabled          = false # this fork defaults to Claude only
refresh_credentials = false
prompt           = "ok"
model            = "gpt-5.4-mini"  # cheapest Codex model for triggering
reasoning_effort = "low"  # "minimal" is rejected when web_search/image_gen tools are enabled
extra_args       = []     # extra Codex CLI args; exec-only flags such as --json are ignored
align_start      = ""
continue_prompt  = "continue"  # message `continue` injects on 5h recovery; empty = "continue"

[spark]
enabled          = false  # opt in; Spark is a separate Codex-backed watch target
prompt           = "ok"
model            = "gpt-5.3-codex-spark"
reasoning_effort = "low"
extra_args       = []
align_start      = ""
```

Top-level keys:

- **`weekly_threshold`** — when the weekly window is at/above this, `watch` stops
  pinging and waits for the weekly reset (unless usable credits exist).
- **`reset_buffer`** — how long to wait after a window's reset time before
  pinging, so the window has definitely rolled over.
- **`usage_display`** — whether text `status` / `bg status` renders each window
  as used percentage or remaining percentage.
- **`align_start`** (per provider) — pin the phase of your windows: set to a
  future RFC3339 time to delay the very first ping until then; afterwards windows
  chain automatically every ~5h.

### Why a cheap model

Triggering a window doesn't depend on the model — **any** billable request starts
the 5h clock — so the ping uses each provider's cheapest model to eat the least of
your budget:

- **Claude → `haiku`**: also avoids the separate weekly Opus bucket.
- **Codex → `gpt-5.4-mini`**: the mini variant (see `~/.codex/models_cache.json`
  for what your plan offers).
- **Spark → `gpt-5.3-codex-spark`**: a Codex-backed Spark target, disabled by
  default so upgrades do not add another quota-consuming ping.

Claude/Codex/Spark don't expose per-model prices at runtime (Anthropic's local
cost cache is empty; Codex's model cache has no price field), so the cheapest
model is a sensible default rather than a live price lookup. Override `model`
per provider if you prefer.

### Active-session detection (hooks)

At a window reset, `watch` avoids pinging while you're actively working — that
turn would start the next window on its own. This relies on **CLI hooks**, which
are never installed implicitly. If they aren't installed, `limitping` skips the
check entirely and pings right at reset (it never guesses from the process list).

To opt in for Claude only:

```sh
limitping hooks install claude
```

This registers limitping's hooks in `~/.claude/settings.json` and
`~/.codex/hooks.json` (your existing settings are preserved; a `.bak` backup is
written). The hooks invoke the hidden `limitping hook <provider>` command on
`UserPromptSubmit` / `PreToolUse` / `PostToolUse` / `Stop` (Claude also
`SessionEnd`) to record whether a session is mid-turn under
`~/.config/limitping/activity/`. Spark runs through the Codex CLI and uses the
Codex hook/activity marker; there is no separate Spark hook config.

> [!NOTE]
> Claude Code loads its hooks automatically — nothing to do there. **Codex**
> gates custom command hooks behind a one-time trust step: run `/hooks` inside
> Codex once to enable them. Remove everything later with
> `limitping hooks uninstall` (also done automatically by `limitping uninstall`).

## Scheduled pings

Use `schedule` when you want **wall-clock pings** instead of reset-aligned
window chaining. It keeps running in the foreground and fires `ping` at the next
configured interval or daily local time:

```sh
limitping schedule codex --at 05:00
limitping schedule codex --at 05:00 --at 13:00 --at 21:00
limitping schedule --at 05:00,13:00,21:00
limitping schedule spark --every 5h --dry-run
```

You can combine `--every` and `--at`; whichever next occurrence comes first is
used. `--at` values are daily local times in `HH:MM` or `HH:MM:SS` form.

## Run `watch` in the background

`watch` runs in the foreground. To free your terminal, run it as a detached
background process with the built-in `bg` command:

```sh
limitping bg start          # start watch detached from the terminal
limitping bg status         # running? pid, uptime, log + each provider's usage (alias: limitping bg)
limitping bg logs -f        # follow the watcher's log (-n N for last N lines)
limitping bg stop           # stop it
```

`watch` defaults to low-power log output. Add `--live` if you want a foreground
heartbeat/status line. `bg start` takes the same optional `[provider]` argument
and `--dry-run` flag as `watch`. Only one watcher (foreground or background) runs
at a time, and background output is written to
`~/.config/limitping/bg.log` (honors `$XDG_CONFIG_HOME`). The process detaches
into its own session, so it survives the shell closing — but it does **not**
restart on reboot.

On macOS, use the managed LaunchAgent instead:

```sh
limitping bg stop                 # required if a detached watcher is running
limitping service install claude  # RunAtLoad + KeepAlive + caffeinate -s
limitping service status
```

The service records the absolute binary path, your current `PATH`, and your home
folder as its working directory. It starts at login, restarts after failure,
and by default uses `caffeinate -s` to prevent
idle system sleep while the Mac is connected to AC power. It cannot run while a
laptop lid is closed or the Mac is otherwise forced to sleep. Use
`--prevent-sleep=false` if you do not want the AC-power sleep assertion. Logs
are under `~/.config/limitping/`. `service status` also reports the watcher PID
and seven-day ping success/failure history. `limitping service uninstall`
removes only the LaunchAgent and preserves configuration and logs.

## Auto-continue a parked task

`watch` and `bg` keep your window chain warm, but they don't resume a task that
has already stalled at the 5h limit. `limitping continue <provider>` does: it
launches the provider's real interactive CLI through a PTY and passes your
terminal straight through, so you drive Codex / Claude Code exactly as usual.
In the background it polls usage and, the moment the 5h limit recovers after
being hit, types your continue message into the session so a long task resumes
itself instead of sitting parked until you come back.

```sh
limitping continue codex                       # drive Codex as usual; auto-resume on recovery
limitping continue codex --yolo                # flags after the provider pass through verbatim
limitping continue claude --dangerously-skip-permissions
```

- The resume message is each provider's `continue_prompt` in config (default
  `"continue"`; set it to e.g. `"继续任务"`). Quit from inside the CLI to exit.
- It only injects on a genuine recovery edge: the 5h window was maxed (or the
  endpoint reported `limit_reached`, or the CLI printed a limit message) and has
  since clearly reset, and the weekly window isn't also exhausted (per
  `weekly_threshold`, credits included) — so it won't resume straight into the
  weekly wall.
- A diagnostic timeline is written to `~/.config/limitping/continue.log`.
- Unix only for now (needs a PTY); on Windows the command reports that it's
  unsupported.

## Cost & caveats

- See [PRIVACY.md](PRIVACY.md) for local data handling and network behavior.
- See [SECURITY.md](SECURITY.md) for vulnerability reporting and credential
  handling notes.
- Triggering **consumes a little quota** (~one ping per 5h ≈ 33/week). The ping
  uses a minimal prompt and low reasoning, so the cost is tiny but non-zero.
- The **usage endpoints are unofficial** and could change; they're read-only and
  isolated per provider for easy patching.
- macOS-first: Keychain reads and notifications are macOS-only. Codex/Spark
  `auth.json` is cross-platform; Claude on Linux uses
  `~/.claude/.credentials.json`; notifications are a no-op off macOS.

## Layout

```
cmd/limitping            CLI entry
internal/config          TOML config
internal/usage           normalized usage model
internal/auth            Claude (Keychain) + Codex/Spark (auth.json) tokens
internal/provider        per-provider ReadUsage (endpoint) + Trigger (CLI)
internal/activity        hook-based active-session state (shared by the hook cmd + scheduler)
internal/pricing         pricing helpers for providers that expose token usage
internal/scheduler       the watch engine (sleep-until-reset, weekly-respect, backoff)
internal/notify          macOS osascript notifications
internal/cli             cobra commands: status, ping, watch, schedule, continue, background, service, config, hooks, upgrade, uninstall, version
```

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) and
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Before submitting:

```sh
gofmt -l .        # should print nothing
go build ./...
go vet ./...
go test ./...
```

Providers are isolated in `internal/provider` behind a small `Provider`
interface (`ReadUsage` + `Trigger`), so adding a new provider is mostly
self-contained provider code plus wiring in `internal/cli` and `internal/config`.

**Releasing** is automated: push a tag and GitHub Actions runs GoReleaser to
build the cross-platform binaries and publish a Release.

```sh
git tag v0.2.0 && git push origin v0.2.0
```

## License

[MIT](LICENSE) © wavever
