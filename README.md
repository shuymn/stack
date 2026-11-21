# stack

A minimal, extensible Go CLI (`stack`) for personal AI coding agent orchestration (Claude Code, Codex, T3 Code, `herdr`) on macOS, Linux, and Windows.

## Overview

- **Minimal Go core (`cmd/stack`, `internal/`):**
  - `stack kit open <slug>` / `stack kit close <slug>`: Mid-session promote and handover via a single mechanism (`$STACK_SESSION_ID` + `<session_id>.kits`).
  - `stack hook claude session-start`: Exports `STACK_SESSION_ID` via `CLAUDE_ENV_FILE` on every start/resume/clear/compact; injects kit state and worker rules, then the entry extensions' contexts, **only on `compact`** (zero misfire on ordinary sessions).
  - `stack hook claude pre-tool-use`: Minimal near-zero-false-positive guard (secrets read, local paths in `gh` write bodies, raw `gh pr merge` deny).
  - `stack pr merge <pr> <head-sha>`: Merges only when head SHA matches and all checks pass (plus optional `.claude/merge-check`), then runs the `merged` extensions after the `MERGED` line.
  - `stack check public`: Detects local paths (macOS/Linux/Windows) and private terms in commit ranges or PR/issue bodies.
  - `stack budget status`: Reports instruction prose sizes and attention item counts (display-only).
  - `stack run <entry|outbound|merged|cycle>`: Runs optional extensions over a uniform `stdin/stdout JSON` contract, with `--dry-run` and `--replay`.
- **Optional extensions (`extensions/`):** `observe`, stateless `improve`, and opt-out System One (`jev`) extensions.
- **Fixtures (`fixtures/`):** Positive/negative cases for `--replay` verification.

## Install

From this checkout, install `stack` and the cycle extensions into one absolute directory on `PATH` (for scheduled
runs, also on the job's `PATH`), so core finds each extension as `stack-<name>`:

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/stack ./extensions/observe/cmd/stack-observe ./extensions/improve/cmd/stack-improve
```

`stack-improve` embeds `extensions/improve/writing.md`; reinstall after changing it.

## Cycle

Run each cycle extension as its own stage, observe first, from the checkout of the repository improve may change:

```sh
stack run cycle --ext observe   # 7-day window; writes ~/.local/state/stack/observe/latest.json
stack run cycle --ext improve   # reads that file; off unless enabled below
```

Check that observe answered without a `latest-not-written` finding before running improve, which can otherwise
use an older `latest.json` (up to 24 hours old).

Improve is off by default. Once enabled it can make paid model calls and open a PR, so the owner opts in on this
machine:

```toml
# ~/.config/stack/improve.toml
enabled = true   # absent file or any other value = off
```

Enabled, it runs on macOS and Linux only. It shares one stamp per local ISO week across repositories under the same
home directory; a Claude attempt with nonzero or unknown cost keeps it. It starts Claude only after verifying that the
single `origin` URL is a public `github.com` repository whose default branch has `.stack/improve.toml` with a
non-empty `paths` list, and published changes modify only existing regular, non-executable files matching those
patterns:

```toml
paths = ["home/dot_claude/skills/**"]
```

Moving from the legacy kaizen cycle, keep improve disabled until the cutover order of
[docs/design/phase3-cycle.md](docs/design/phase3-cycle.md) §6 reaches its opt-in step: the old job stopped, the
private terms copied and verified, and a fresh observe run checked. The scheduled daily run (`launchd`, macOS) is
part of that cutover in `shuymn/dotfiles`.

See [docs/architecture.md](docs/architecture.md) for the full design and migration roadmap.
