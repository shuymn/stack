# stack

A minimal, extensible Go CLI (`stack`) for personal AI coding agent orchestration (Claude Code, Codex, T3 Code, `herdr`) on macOS, Linux, and Windows.

## Overview

- **Minimal Go core (`cmd/stack`, `internal/`):**
  - `stack kit open <slug>` / `stack kit close <slug>`: Mid-session promote and handover via a single mechanism (`$STACK_SESSION_ID` + `<session_id>.kits`).
  - `stack hook claude session-start`: Exports `STACK_SESSION_ID` via `CLAUDE_ENV_FILE` on every start/resume/clear/compact; injects kit state and worker rules **only on `compact`** (zero misfire on ordinary sessions).
  - `stack hook claude pre-tool-use`: Minimal near-zero-false-positive guard (secrets read, local paths in `gh` write bodies, raw `gh pr merge` deny).
  - `stack pr merge <pr> <head-sha>`: Merges only when head SHA matches and all checks pass (plus optional `.claude/merge-check`).
  - `stack check public`: Detects local paths (macOS/Linux/Windows) and private terms in commit ranges or PR/issue bodies.
  - `stack budget status`: Reports instruction prose sizes and attention item counts (display-only).
  - `stack run <entry|outbound|cycle>`: Runs optional extensions over a uniform `stdin/stdout JSON` contract, with `--dry-run` and `--replay`.
- **Optional extensions (`extensions/`):** `observe`, stateless `improve`, and opt-out System One (`jev`) extensions.
- **Fixtures (`fixtures/`):** Positive/negative cases for `--replay` verification.

## Install

Install `stack` and the cycle extensions into one directory on `PATH`, so core finds each extension as
`stack-<name>`:

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/stack ./extensions/observe/cmd/stack-observe ./extensions/improve/cmd/stack-improve
```

`stack-improve` embeds `extensions/improve/writing.md`; reinstall after changing it.

## Cycle

Run each cycle extension as its own stage, from the checkout of the repository improve may change:

```sh
stack run cycle --ext observe   # 7-day window; writes ~/.local/state/stack/observe/latest.json
stack run cycle --ext improve   # off unless enabled below
```

`stack-improve` makes paid model calls and opens a PR, so it calls nothing until the owner opts in on this machine:

```toml
# ~/.config/stack/improve.toml
enabled = true   # absent file or any other value = off
```

Enabled, it makes at most one attempt per ISO week, on macOS and Linux only, and only in a public GitHub
repository whose default branch has `.stack/improve.toml` listing the paths it may change:

```toml
paths = ["home/dot_claude/skills/**"]
```

The scheduled daily run (`launchd`, macOS) lives in `shuymn/dotfiles`. Contracts:
[docs/design/phase3-cycle.md](docs/design/phase3-cycle.md).

See [docs/architecture.md](docs/architecture.md) for the full design and migration roadmap.
