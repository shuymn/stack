# `shuymn/stack` Architecture (v10)

## 1. Core Thesis

Single-user personal orchestration foundation built following `extensible-by-design`:
- **Hard safety boundaries only (near-zero false positives, irreversible hazards):**
  1. Secrets read protection (`.dev.vars`, `~/.t3/userdata/secrets`) and local paths in public text (`check public`).
  2. Catastrophic deletion guard: a direct recursive `rm` whose literal operand is `/`, the home directory, a drive root, or a `.git` directory itself. Nothing broader.
- **Drop over-defensive machinery & wolf-crying alerts:** No owner-only merge list (`.claude/kaizen-managed`), no `readonly_` (0444) live locks, no self-defending budget ratchets, no custom live swapper (`apply.py`), no `--restricted` sandbox / `commit-tree -S` / recovery state machine in `improve`, no `deny-rm.sh` or broad `git` / `deploy` denies.
- **Cross-platform (macOS, Linux, Windows):** `cmd/stack` and `internal/` are pure portable Go verified on `ubuntu-latest`, `macos-latest`, and `windows-latest` in CI. `check public` handles both POSIX (`/Users/...`, `/home/...`) and Windows (`C:\Users\...`) local paths. Scheduled `cycle` runs via `launchd` on macOS only, while `stack run cycle` itself works cross-platform.
- **Boundary with `shuymn/dotfiles`:** `skills/` (Instruction Plane: `SKILL.md`, `references/`, `agents/`) stay in `shuymn/dotfiles` and deploy via `make apply` (chezmoi). `shuymn/stack` owns the Go CLI (`stack`), optional `extensions/`, and `--replay` `fixtures/`.

## 2. Repository Layout

```text
shuymn/stack/
├── cmd/stack/          # Go CLI entrypoint (`stack`)
├── internal/           # Minimal core packages (zero dependency on extensions/)
├── extensions/         # Optional extensions (observe, improve + writing.md, jev)
├── fixtures/           # Positive/negative fixtures for `--replay`
├── docs/               # Architecture spec
└── scripts/test        # Local & CI completion check
```

## 3. Core CLI Contract (`stack`)

Core runs cleanly with **zero extensions** configured.

```text
stack kit open <slug>                              # Record <slug> in <session_id>.kits (via $STACK_SESSION_ID); output state/items/handover/board + entry extensions
stack kit close <slug>                             # Remove <slug> from <session_id>.kits so completed kits do not resurrect after compaction
stack hook claude session-start                    # All events: export STACK_SESSION_ID via CLAUDE_ENV_FILE when provided; compact only: restore .kits + WORKER_LINE
stack hook claude pre-tool-use                     # Deny secrets read, local paths in `gh` write bodies, raw `gh pr merge`, catastrophic `rm`, Pencil Generate (on unless disabled)
stack pr merge <pr> <head-sha> [--merge|--squash|--rebase] ...   # Verify checked head SHA, all checks passing, optional .claude/merge-check
stack check public <range> [--title-file F --body-file F --branch B]
stack budget status                                # Display-only byte & attention item counts
stack run <entry|outbound|cycle> [--dry-run] [--replay FILE] [--ext NAME]
```

### Extension Protocol (Process Boundary)
Used only for optional extensions (`observe`, `improve`, `jev-*`).
- `stdin`: JSON `{ "stage": "entry|outbound|cycle", "repo": "...", "range": "...", "prompt": "..." }`
- `stdout`: JSON `{ "status": "pass|warn|block", "context": "...", "findings": [...] }`

## 4. Three Named Stages & Session Lifecycle

| Stage | Seam | Responsibility | Retired Legacy Pieces |
|---|---|---|---|
| **S1 `entry`** | `stack kit open/close` + `SessionStart` (`compact` only injects) | Export `STACK_SESSION_ID` via `CLAUDE_ENV_FILE` (when Claude Code provides it) on every event (startup, resume, clear, compact, fork). On `compact`, restore `<session_id>.kits` and `WORKER_LINE` (`opus-worker.md` + original brief). Optional `jev-router`. | `startup`/`resume` auto-injection, `Stop` mtime block, `cwd` kit discovery, `worker-signals.json` |
| **S2 `outbound`** | CI + `stack pr merge` + minimal `PreToolUse` | Deterministic `check public`, `budget status`, checked-head `pr merge`, secrets read deny. Optional `jev-semantic`. | `Edit/Write` budget hook, `.claude/kaizen-managed`, `~/.t3/worktrees/` worker guess, `deny-rm.sh` |
| **S3 `cycle`** | `launchd` -> `stack run cycle` | `observe` (+ optional `jev-classify`) and minimal `improve` (plain worktree + `claude -p`, single period stamp file `~/.local/state/stack/improve/YYYY-Www` to avoid daily re-runs when a proposal is not pushed). Apply via `make apply`. | `apply.py` (1211 lines), `ready.py`, `watch.json`, `improve.py` recovery state machine |

### Mid-Session Promote & Handover (Unified)
- **T0 (Ordinary session):** `SessionStart` writes `STACK_SESSION_ID` to `CLAUDE_ENV_FILE` (when provided) and injects nothing (zero misfire).
- **T1 (Promote):** User invokes `/stack` (or `/hiden`); agent runs `stack kit open <slug>`, writes `~/.local/state/stack/kits/<slug>/state.md`, and proceeds.
- **T3 (Handover):** Parent orchestrator launches successor with `Run \`stack kit open <slug>\` and continue this orchestration.` (per-environment handover instructions live in `~/.config/stack/handover/{t3,herdr}.md`).
- **Worker detection:** `agent_type == "opus-worker"`, `WORKER_LINE` in first prompt, or `-worktrees/agent-*` only. Never guess from `~/.t3/worktrees/`.

### Optional Jev (System One) Extensions
- **Opt-out:** Active when `TYPESAFE_API_KEY` / config is present; silently skipped when absent.
- `jev-router` (S1): scores `references/*.md` for Just-in-Time injection.
- `jev-semantic` (S2): flags meta-duty regression or unsupported claims at CI/PR.
- `jev-classify` (S3): batch-classifies 7-day transcripts for corrections/drift and pre-filters memory duplicates.

## 5. Configuration (`~/.config/stack/config.toml`)

```toml
version = 1

[kit]
root = "~/.local/state/stack/kits"

[worker]
agent_types = ["opus-worker"]
marker = "Follow the body of ~/.claude/agents/opus-worker.md"
body = "~/.claude/agents/opus-worker.md"

[handover]
T3CODE_THREAD_ID = "~/.config/stack/handover/t3.md"
HERDR_ENV = "~/.config/stack/handover/herdr.md"

[safety]
secrets = ["~/.t3/userdata/secrets", ".dev.vars"]
private_terms = "~/.config/stack/private-terms"
deny_pencil_generate = true

[stages]
entry = ["jev-router"]
outbound = ["jev-semantic"]
cycle = ["observe", "jev-classify", "improve"]
```

## 6. Migration Roadmap

1. **Phase 1 (dotfiles immediate cleanup):** Stop ordinary-session misfires (`startup`/`resume` auto-injection, `Stop` block, `~/.t3/worktrees/` worker guess) and remove wolf-crying denies (`deny-rm.sh`, overly broad `git`/`deploy` rules) in `shuymn/dotfiles`.
2. **Phase 2 (`shuymn/stack` Go core):** Implement `kit open/close`, `hook claude session-start/pre-tool-use`, `pr merge`, `check public`, `budget status`, `run` with unit/fixture tests passing on macOS, Linux, and Windows CI. Wire into dotfiles and delete the legacy Python hooks they replace; `attention-budget.py status` and `public-check` stay while Phase 3's kaizen scripts still call them.
3. **Phase 3 (retire legacy `kaizen` & switch `cycle`):** Implement minimal `extensions/observe` and `extensions/improve` (with `writing.md`), switch `launchd` to `stack run cycle`, retire `apply.py`/`ready.py`/old `improve.py`/`.claude/kaizen-managed`, and update `skills/` (`attention.md`, `commit-pr.md`, `retro.md`), `agents/steward.md`, and `statusline.sh.tmpl`. Design: `docs/design/phase3-cycle.md`.
4. **Phase 4 (Jev extensions):** Add `jev-classify` -> `jev-semantic` -> `jev-router` with `fixtures/` and `--replay` verification.
