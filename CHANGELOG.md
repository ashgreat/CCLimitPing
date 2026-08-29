# Changelog

All notable changes to this project should be documented here.

This project uses version tags such as `v0.2.0`. Release binaries are published
through GitHub Actions and GoReleaser.

## v0.9.1

- Added `limitping service install|status|uninstall` for a persistent per-user
  macOS LaunchAgent. It starts at login, restarts after failure, preserves the
  invoking shell's `PATH`, and defaults to `caffeinate -s` so idle system sleep
  is prevented while the Mac is connected to AC power.
- Made OAuth credential refresh and write-back opt-in with
  `refresh_credentials = true`; the default usage path is read-only and fails
  closed when the official CLI needs a fresh login. macOS Keychain updates, if
  enabled, send credential data over stdin instead of process arguments.
- Defaulted to Claude-only operation. Codex and Spark remain available through
  explicit configuration or command selection.
- Stopped the installer from modifying Claude/Codex hook settings. Hooks remain
  available through the explicit `limitping hooks install` command.
- Added SHA-256 verification to installation and self-upgrade downloads, plus
  macOS-service, checksum, and credential-mode tests.

## v0.9.0

- New `limitping redeem` spends a banked Codex rate-limit reset credit (with
  `--dry-run` to see which one first). Setting `auto_redeem = true` under
  `[codex]` lets `watch` and `continue` spend one on their own once it is close
  to lapsing — within 24h with usage worth reclaiming, or in its final hour.
  Redeeming is irreversible, so it stays off by default; the backend refuses
  with "nothing to reset" when no window is eligible, so a credit is never
  burned for nothing.
- Reset and expiry times in `status` / `bg status` now carry their UTC offset
  (e.g. `resets in 3h14m (Sun 00:10 UTC+8)`), so a time read from a log or a
  screenshot taken in another zone is unambiguous. The offset is used rather
  than the zone abbreviation because abbreviations collide (CST is both China
  and US Central).
- Claude usage 429s are now disambiguated with Anthropic's free token-counting
  endpoint: an explicit subscription-auth denial gets an actionable
  subscription-access error, while real or inconclusive rate limits retain the
  original 429. Claude pings also no longer report success when the interactive
  CLI displays the same access-disabled error.

## v0.8.0

- Reset credits in `status` / `bg status` now show the remaining lifetime until
  each unredeemed credit expires (e.g. `expires Jul 27 07:50 (in 11d20h)`), so
  a banked reset about to lapse is visible at a glance.
- Localized the text output of `status`, `bg status`, and `ping` for Chinese
  locales (window lines, reset credits, weekday names, error lines), and fixed
  locale detection to honor POSIX precedence so `LC_ALL=en_US` overrides
  `LANG=zh_CN`. English output is unchanged.
- Adapted Codex to the weekly-only limit regime introduced on 2026-07-12 (the
  5h limit is temporarily removed): usage windows are now classified by their
  length instead of their position in the response, a missing window renders as
  "not currently enforced" (its key is omitted from `status --json`), and
  `watch` pings at the weekly reset instead of every 5h while no 5h window is
  enforced. The reset-credit count embedded in the usage response is used as a
  fallback when the detail endpoint is unavailable.

## v0.7.0

- Added `limitping schedule [provider]`, a foreground scheduler that runs pings
  on fixed intervals (`--every 5h`) or at one or more daily local times
  (`--at 05:00 --at 13:00`, or comma-separated values). It reuses the existing
  ping path and supports `--dry-run`.
- Added best-effort Codex reset credit reads from the Codex backend reset-credit
  endpoint. When available, `status` and `status --json` now report available
  reset credits without failing the usage read if that private endpoint is
  unavailable.
- Added `usage_display = "used" | "remaining"` for text `status` / `bg status`,
  and added `remaining_percent` to JSON window output so scripts can consume
  both views.

## v0.6.0

- Added `limitping continue <provider>`, an interactive proxy that launches the
  provider's real CLI through a PTY (your terminal passes straight through) and
  auto-injects a continue message the moment the 5h limit recovers, so a parked
  long task resumes itself instead of waiting for you. Flags after the provider
  pass through verbatim (e.g. `continue codex --yolo`). It only fires on a
  genuine recovery edge and respects the weekly limit (`weekly_threshold`,
  credits included), writing a diagnostic timeline to `continue.log`. The resume
  message is the new per-provider `continue_prompt` config key (default
  `"continue"`). Unix only for now (needs a PTY).
- Shared the "weekly window exhausted" rule between the scheduler and the
  continue proxy via `usage.Usage.WeeklyExhausted`, so both honor
  `weekly_threshold` and usable credits identically.

## v0.5.0

- Added `status --json`, which emits each provider's 5h/weekly usage, plan,
  credits, and reset timing as a JSON array for scripts and dashboards. Progress
  output is suppressed so stdout stays a single valid document, a provider that
  fails to read becomes `{"provider": ..., "error": ...}`, and `-v` embeds the
  raw response under `raw`.

## v0.4.2

- Fixed Codex pings to use the interactive, TTY-backed Codex CLI instead of
  headless `codex exec`, so the ping anchors the subscription-backed 5h window.
- Updated Codex trigger docs and examples to match the interactive command path
  and clarify that per-ping token/cost output is not available in this mode.

## v0.4.1

- Fixed Claude/Codex usage reads to match the official client request shape
  more closely, including provider-specific headers and Codex `chatgpt_base_url`
  handling.
- Added status-aware handling for usage endpoint 429s so `watch` pauses reads
  instead of repeatedly retrying a rate-limited endpoint.
- Fixed usage reads on networks where Go's HTTP/2 client path returns EOF or
  malformed responses by using a dedicated HTTP/1.1 usage client.

## v0.4.0

- `watch` now draws a live status line on an interactive terminal: a spinner
  plus each provider's current state and a live countdown to its next ping,
  redrawn beneath the scrolling log. It auto-disables when output isn't a TTY
  (e.g. `bg`'s log file or a pipe), so logs stay free of control sequences.
- Added `limitping background` (alias `bg`) to run `watch` as a detached
  background process, freeing the terminal: `bg start [provider] [--dry-run]`
  launches it, `bg status` (or bare `bg`) reports pid/uptime/log path plus the
  resolved list of watched providers and each one's current 5h/weekly usage,
  `bg stop` ends it, and `bg logs [-f] [-n N]` shows its output. Only one
  background watcher runs at a time; state and logs live under the config dir
  (`bg.json` / `bg.log`).
- Fixed the Claude trigger: the interactive session now actually submits the
  prompt (and waits for the turn to run) instead of exiting before any message
  was sent, so the 5h window reliably starts.
- Added hook-based active-session detection: `limitping hooks install` /
  `uninstall` registers limitping's hooks in `~/.claude/settings.json` and
  `~/.codex/hooks.json` so `watch` can tell when a session is genuinely mid-turn
  (between a prompt and its `Stop`) rather than just having a live process. The
  install script sets the hooks up automatically. Without hooks, `limitping`
  skips the active-session check and pings as soon as the window resets — there
  is no process-list fallback (it produced false positives from unrelated
  Claude/Codex agent processes).
- Removed the experimental GLM (Zhipu / Z.ai) provider. `limitping` now targets
  Claude Code and Codex only; the `[glm]` config block and `glm` provider
  argument are gone.

## v0.3.0

- Added short command aliases such as `ping` / `p`, `status` / `s`,
  `watch` / `w`, `version` / `v`, `upgrade` / `up`, and `uninstall` / `rm`.
- Updated help output to show command aliases inline and clarify accepted
  `ping` / `watch` provider arguments.
- Added Chinese CLI help text when the system locale is Chinese.
- Documented upgrade, uninstall, and command aliases in the English and Chinese
  READMEs.

## v0.2.1

- `watch` now defers automatic pings while a Claude/Codex CLI task is already
  running, letting that task naturally start the next 5h window.

## v0.2.0

- Switched Claude triggering to the interactive Claude Code CLI so subscription
  window pings keep working after headless print mode moves to Agent SDK/API
  billing.
- Added retry handling for transient usage endpoint failures and removed
  duplicate `status` error output.
- Added `limitping upgrade` / `limitping update` to update the installed binary
  from the latest GitHub release.
- Added `limitping uninstall`, which removes the binary and config/cache by
  default, with `--keep-config` to preserve config/cache.
- Added open-source governance, security, privacy, and contribution guidance.

## v0.1.0

- Initial public release target.
