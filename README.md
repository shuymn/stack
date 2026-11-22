# stack

A minimal, extensible Go CLI (`stack`) for personal AI coding agent orchestration (Claude Code, Codex, T3 Code, `herdr`) on macOS, Linux, and Windows.

## Overview

- **Minimal Go core (`cmd/stack`, `internal/`):**
  - `stack kit open <slug>` / `stack kit close <slug>`: Mid-session promote and handover via a single mechanism (`$STACK_SESSION_ID` + `<session_id>.kits`).
  - `stack hook claude session-start`: Exports `STACK_SESSION_ID` via `CLAUDE_ENV_FILE` on every start/resume/clear/compact; injects kit state and worker rules, then the entry extensions' contexts, **only on `compact`** (zero misfire on ordinary sessions).
  - `stack hook claude pre-tool-use`: Denies a raw merge (`gh pr merge`, or a merge through `gh api`) so that `stack pr merge` stays the only merge path; allows everything else.
  - `stack pr merge <pr> <head-sha>`: Merges only when head SHA matches and all checks pass (plus optional `.claude/merge-check`), then runs the `merged` extensions after the `MERGED` line. With `--subject` or `--body`, it first runs the outbound extensions listed in `[merge].message_checks` (default none) and refuses before any `gh` call when one blocks or cannot answer; a `pass` or `warn` from each lets the merge continue.
  - `stack run <entry|outbound|merged|cycle>`: Runs optional extensions over a uniform `stdin/stdout JSON` contract, with `--dry-run` and `--replay`.
- **Optional extensions (`extensions/`):** `observe`, stateless `improve`, `public` (`stack-public`: the local-path and private-term check as an `outbound` extension, and `stack-public check <base>..<head>` for direct callers such as improve; reads `~/.config/stack/safety.toml`), `guard` (`stack-guard hook claude pre-tool-use`: the secrets, `gh` publish, catastrophic `rm` and Pencil Generate denies as a `PreToolUse` hook of its own; reads `~/.config/stack/safety.toml`), and opt-out System One (`jev`) extensions.
- **Fixtures (`fixtures/`):** Positive/negative cases for `--replay` verification.

## Install

Install `stack` and the extensions from this checkout into one absolute directory on `PATH` (for scheduled runs,
also on the job's `PATH`), so core finds each extension as `stack-<name>`.

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/stack ./extensions/observe/cmd/stack-observe \
  ./extensions/auditdue/cmd/stack-audit-due
GOBIN="$HOME/.local/bin" go install ./extensions/public/cmd/stack-public ./extensions/guard/cmd/stack-guard
GOBIN="$HOME/.local/bin" go install ./extensions/improve/cmd/stack-improve
GOBIN="$HOME/.local/bin" go install ./extensions/jev/semantic/cmd/stack-jev-semantic
```

Install `stack-improve` after `stack-public`: improve's public check runs `stack-public check`, which reads
`~/.config/stack/safety.toml`. `stack-improve` embeds `extensions/improve/writing.md`; reinstall after changing it.

`stack-jev-semantic` answers `pass` without `TYPESAFE_API_KEY`; the key opts in to live use, and it then sends the
PR's texts and diff to Jev for the questions it can assess under an enabled decision, warning on a likely hit. The
committed placeholder cutoffs disable both decisions, so nothing is sent until `jev-calibrate` has run. It embeds
its cutoffs (`extensions/jev/semantic/cutoffs.json`); reinstall after `jev-calibrate` changes them.

`stack-guard` is a Claude Code hook of its own, not a stage extension: wire it as a separate `PreToolUse` entry
(matcher `Bash|PowerShell|Read|NotebookRead|Grep|Glob|mcp__pencil__execute`, exec form `stack-guard hook claude pre-tool-use`,
`timeout: 5`, `onFailure: "continue"`) and never list it in `[stages]`, where it prints usage and exits 2. Without
`safety.toml` it applies the defaults, without custom `secrets` entries. The core hook does not have these rules:
without `stack-guard` wired, nothing denies them.

To check `stack pr merge` messages for local paths and private terms, list `public` in `~/.config/stack/config.toml`
(`[merge]` `message_checks = ["public"]`) with `stack-public` installed; a listed check that blocks, is missing or
fails refuses the merge.

Keep `public` first in `[stages].outbound` (the default order): its own 100 s limit, which answers `block` before
the stage's 120 s deadline turns a slow check into a `warn`, holds only when no extension runs before it.

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

The scheduled daily run (`launchd`, macOS) lives in `shuymn/dotfiles`: each morning the `stack-cycle` job runs
observe and, when observe wrote a fresh `latest.json`, improve (which does nothing unless enabled as above).

See [docs/architecture.md](docs/architecture.md) for the full design and migration roadmap.
