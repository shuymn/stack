# stack

A minimal, extensible Go CLI (`stack`) for personal AI coding agent orchestration (Claude Code, Codex, T3 Code, `herdr`) on macOS, Linux, and Windows.

## Overview

- **Minimal Go core (`cmd/stack`, `internal/`):**
  - `stack kit open <slug>` / `stack kit close <slug>`: Mid-session promote and handover via a single mechanism (`$STACK_SESSION_ID` + `<session_id>.kits`).
  - `stack hook claude session-start`: Exports `STACK_SESSION_ID` via `CLAUDE_ENV_FILE` on every start/resume/clear/compact; injects kit state and worker rules, then the entry extensions' contexts, **only on `compact`** (zero misfire on ordinary sessions).
  - `stack hook claude pre-tool-use`: Minimal near-zero-false-positive guard (secrets read, local paths in `gh` write bodies, raw `gh pr merge` deny).
  - `stack pr merge <pr> <head-sha>`: Merges only when head SHA matches and all checks pass (plus optional `.claude/merge-check`), then runs the `merged` extensions after the `MERGED` line.
  - `stack check public`: Detects local paths (macOS/Linux/Windows) and private terms in commit ranges or PR/issue bodies.
  - `stack run <entry|outbound|merged|cycle>`: Runs optional extensions over a uniform `stdin/stdout JSON` contract, with `--dry-run` and `--replay`.
- **Optional extensions (`extensions/`):** `observe`, stateless `improve`, `public` (`stack-public`: the local-path and private-term check as an `outbound` extension, and `stack-public check <base>..<head>` for direct callers such as improve; reads `~/.config/stack/safety.toml`), `guard` (`stack-guard hook claude pre-tool-use`: the secrets, `gh` publish, catastrophic `rm` and Pencil Generate denies as a `PreToolUse` hook of its own; reads `~/.config/stack/safety.toml`), and opt-out System One (`jev`) extensions.
- **Fixtures (`fixtures/`):** Positive/negative cases for `--replay` verification.

## Install

From this checkout, install `stack` and the extensions into one absolute directory on `PATH` (for scheduled
runs, also on the job's `PATH`), so core finds each extension as `stack-<name>`:

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/stack ./extensions/observe/cmd/stack-observe \
  ./extensions/auditdue/cmd/stack-audit-due
```

Install `stack-public` and `stack-improve` last, in this order. Improve's public check runs `stack-public check`,
which reads `~/.config/stack/safety.toml` instead of the `[safety]` table of `config.toml`; a private-terms file
named only in `config.toml` would otherwise drop out of the check.

1. Write `~/.config/stack/safety.toml` from the effective `[safety]` table of `~/.config/stack/config.toml`: the
   same keys (`secrets`, `private_terms`, `deny_pencil_generate`) at the top level, without the `[safety]` header
   (a `[safety]` table in `safety.toml` is an unknown key, and the defaults apply). For example:

   ```toml
   private_terms = "~/.config/stack/private-terms"
   deny_pencil_generate = true
   ```

   Compare custom `secrets` paths, a custom `private_terms` file and an explicit `deny_pencil_generate = false`; a
   key left out takes the same default in both files. Keep the table in `config.toml` too: `stack` still reads it.
2. Install `stack-public` and check that `command -v stack-public` finds it both in an interactive shell and on the
   scheduled job's `PATH`:

   ```sh
   GOBIN="$HOME/.local/bin" go install ./extensions/public/cmd/stack-public
   ```

3. Only then install or reinstall `stack-improve`; without `stack-public` it stops at `failed:check`:

   ```sh
   GOBIN="$HOME/.local/bin" go install ./extensions/improve/cmd/stack-improve
   ```

`stack-improve` embeds `extensions/improve/writing.md`; reinstall after changing it.

Install `stack-guard` once `safety.toml` exists (step 1); before that it applies the defaults, without custom
`secrets` entries:

```sh
GOBIN="$HOME/.local/bin" go install ./extensions/guard/cmd/stack-guard
```

`stack-guard` is a Claude Code hook of its own, not a stage extension: wire it as a separate `PreToolUse` entry
(matcher `Bash|Read|NotebookRead|Grep|Glob|mcp__pencil__execute`, exec form `stack-guard hook claude pre-tool-use`,
`timeout: 5`) and never list it in `[stages]`, where it prints usage and exits 2. Until the core hook drops its copy
of the same rules, both hooks deny them.

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

Moving from the legacy kaizen cycle, keep improve disabled until the cutover order of
[docs/design/phase3-cycle.md](docs/design/phase3-cycle.md) §6 reaches its opt-in step: the old job stopped, the
private terms copied and verified, and a fresh observe run checked. The scheduled daily run (`launchd`, macOS) is
part of that cutover in `shuymn/dotfiles`.

See [docs/architecture.md](docs/architecture.md) for the full design and migration roadmap.
