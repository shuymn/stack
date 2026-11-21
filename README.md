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

### Upgrading from v10

The v11 `stack` hook denies only raw merges, reads no `[safety]` table (a leftover one in `config.toml` only gets an
unknown-key warning) and checks merge messages only when `[merge].message_checks` lists a check. Replacing `stack`
first would drop the secrets, `gh` publish, catastrophic `rm` and Pencil Generate denies and the merge message check
until the rest is in place. Follow these steps in order, and replace `stack` last:

1. Write `~/.config/stack/safety.toml` from the `[safety]` table of `~/.config/stack/config.toml`, if it has one:
   the same keys (`secrets`, `private_terms`, `deny_pencil_generate`) at the top level, without the `[safety]`
   header (a `[safety]` table in `safety.toml` is an unknown key, and the defaults apply). For example:

   ```toml
   private_terms = "~/.config/stack/private-terms"
   deny_pencil_generate = true
   ```

   Compare custom `secrets` paths, a custom `private_terms` file and an explicit `deny_pencil_generate = false`; a
   key left out takes the v10 default. Copy the values and keep the `[safety]` table in `config.toml`: v10 still
   reads it until step 6, and again after a rollback to v10.
2. Install `stack-public` and check that `command -v stack-public` finds it both in an interactive shell and on the
   scheduled job's `PATH`:

   ```sh
   GOBIN="$HOME/.local/bin" go install ./extensions/public/cmd/stack-public
   ```

3. Install `stack-guard` and wire it (below), then check in a live session that `stack-guard` itself denies each
   moved rule: its reasons start with `stack-guard (<rule>): `, the core hook's with `stack guard (`.

   ```sh
   GOBIN="$HOME/.local/bin" go install ./extensions/guard/cmd/stack-guard
   ```

4. To keep v10's check of `stack pr merge --subject/--body` for local paths and private terms, add
   `message_checks = ["public"]` under `[merge]` in `config.toml`.
5. Before replacing `stack`, check v11 with the applied settings: put a v11 `stack` built from this checkout first
   on a session's `PATH` (not installed) and confirm that the moved rules are denied only with `stack-guard (`
   reasons, a raw `gh pr merge` with `stack guard (raw-merge): `, and that `stack pr merge --body` with a local path
   is refused with `outbound blocked: public` before any `gh` call.
6. Only then replace `stack` and the other extensions (the command below), and install or reinstall
   `stack-improve`; without `stack-public` it stops at `failed:check`. After installing, merge one PR with
   `--body` to confirm the check runs on the installed `PATH`. To roll back, reinstall the v10 `stack` first.

### Install

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/stack ./extensions/observe/cmd/stack-observe \
  ./extensions/auditdue/cmd/stack-audit-due
GOBIN="$HOME/.local/bin" go install ./extensions/public/cmd/stack-public ./extensions/guard/cmd/stack-guard
GOBIN="$HOME/.local/bin" go install ./extensions/improve/cmd/stack-improve
```

Install `stack-improve` after `stack-public`: improve's public check runs `stack-public check`, which reads
`~/.config/stack/safety.toml`. `stack-improve` embeds `extensions/improve/writing.md`; reinstall after changing it.

`stack-guard` is a Claude Code hook of its own, not a stage extension: wire it as a separate `PreToolUse` entry
(matcher `Bash|Read|NotebookRead|Grep|Glob|mcp__pencil__execute`, exec form `stack-guard hook claude pre-tool-use`,
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

Moving from the legacy kaizen cycle, keep improve disabled until the cutover order of
[docs/design/phase3-cycle.md](docs/design/phase3-cycle.md) §6 reaches its opt-in step: the old job stopped, the
private terms copied and verified, and a fresh observe run checked. The scheduled daily run (`launchd`, macOS) is
part of that cutover in `shuymn/dotfiles`.

See [docs/architecture.md](docs/architecture.md) for the full design and migration roadmap.
