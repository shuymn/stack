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

See [docs/architecture.md](docs/architecture.md) for the full design and migration roadmap.
