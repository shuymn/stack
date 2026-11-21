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
stack run <entry|outbound|cycle> [--dry-run] [--replay FILE] [--ext NAME]... [--range R] [--prompt-file F]
stack --version [--json]                           # Plain version; --json prints {"version","gh_model"}, gh_model = the gh release the guard models
```

### Extension Protocol (Process Boundary)
Used only for optional extensions (`observe`, `improve`, `jev-*`). Core never imports or inspects `extensions/`; it
talks to an extension only through this contract. Details (validation commands, usage errors, plan format, replay
fixtures) are in `docs/design/phase2-core.md` §4.7.
- **Evolution:** existing fields, types, defaults, statuses, stages and meanings are kept. New fields are optional,
  safe to ignore and distinct from existing keys (also case-insensitively); a new stage runs only when selected. An
  incompatible change needs an explicit protocol selection that keeps this contract for current extensions. No
  version field until a second dialect exists.
- **Discovery:** `<name>` matches `^[a-z0-9][a-z0-9-]*$`; names come from `[stages].<stage>`, or only from the
  repeatable `--ext NAME` for `stack run`, and run in that order. Core picks the first runnable `stack-<name>` in the
  absolute `PATH` entries (on Windows `stack-<name>.exe`; `PATHEXT` is ignored). A missing name is skipped without a
  warning; `stack run` lists it under `skipped` (or in the `--dry-run` plan).
- **Invocation:** no arguments; cwd is the invoker's; the environment is inherited, with `STACK_STAGE` set to the
  stage; `STACK_SESSION_ID` is passed through when set.
- `stdin`: one UTF-8 JSON object, read to EOF. A live request has four strings: `{ "stage": "entry|outbound|cycle",
  "repo": "...", "range": "...", "prompt": "..." }`. `repo` is the Git top-level of the cwd or `""`; `range` is
  `--range R`, else for outbound the literal `@{upstream}..HEAD` when Git resolves `@{upstream}`, else `""`;
  `prompt` is the `--prompt-file` contents or `""` (`kit open` sends both empty). `--replay` sends the fixture's
  request object as is once its `stage` matches; the fixture supplies the other fields. Extensions ignore unknown
  fields.
- `stdout`: one UTF-8 JSON object; all stdout bytes count toward a 1 MiB limit: `{ "status": "pass|warn|block", "context": "...", "findings":
  [...] }`. `status` is required; `context` (a string) and `findings` (an array) may be omitted and default to `""`
  and `[]`; unknown fields are ignored. Once core accepts the response, the exit code is ignored (a valid `block`
  counts even with a non-zero exit).
  stderr is forwarded to core's stderr with `[<name>] ` before each line.
- **Limits and failures:** one deadline per stage shared by its extensions (entry 10 s, outbound 120 s, cycle 30
  min). Before parsing stdout, core checks for a lookup or start failure, the deadline, a pipe still open when the
  cleanup grace period after exit ends, and oversized stdout; these, an invalid response and an extension left
  unstarted at the deadline become `warn` with a synthetic finding, never `block`.
- **Result:** statuses aggregate as `block > warn > pass` (no result = `pass`). After writing the aggregate,
  `stack run` exits 1 on `block` and 0 on `pass` or `warn`; with a replay `expect`, 0 or 1 reports match or
  mismatch (usage errors exit 2; `--dry-run` exits 0). `stack kit open` appends each entry `context` and a marker
  for `warn` or `block` without enforcing them; findings appear only in the `stack run` aggregate.

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
- **Opt-out:** Active when `TYPESAFE_API_KEY` / config is present. An installed `jev-*` that lacks its key or config opts out by answering `{"status":"pass"}` with no context.
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
3. **Phase 3 (retire legacy `kaizen` & switch `cycle`):** Implement minimal `extensions/observe` and `extensions/improve` (with `writing.md`), switch `launchd` to `stack run cycle`, retire `apply.py`/`ready.py`/old `improve.py`/`.claude/kaizen-managed`, and update `skills/` (`attention.md`, `commit-pr.md`, `retro.md`), `agents/steward.md`, and `statusline.sh.tmpl`. Design: `docs/design/phase3-cycle.md`. Status: the extensions are in this repository (install and opt-in in `README.md`); the `launchd` switch, kaizen retirement and `skills/` updates follow in `shuymn/dotfiles` in the cutover order of the design §6.
4. **Phase 4 (Jev extensions):** Add `jev-classify` -> `jev-semantic` -> `jev-router` with `fixtures/` and `--replay` verification.
