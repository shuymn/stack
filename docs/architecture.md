# `shuymn/stack` Architecture (v11)

## 1. Core Thesis

Single-user personal orchestration foundation built following `extensible-by-design`:
- **The core is the lifecycle and the extension contract.** It defines every stage (§4) and the protocol extensions use (§3), and holds only built-in behaviour that is required for every session with no extension installed, wrong or unsafe if left to extensions, and stable: session identity, kits and their restore at compaction, the extension protocol, and the guarantee that each stage fires (the checked `stack pr merge` and the raw `gh pr merge` deny, without which `merged` could be bypassed). Everything else is an extension or external, listed in §4 "Intentionally not in core". Design: `docs/design/core-stages.md`.
- **Safety extensions guard hard boundaries only (near-zero false positives, irreversible hazards):** secrets reads, local paths and private terms in public text, a direct recursive `rm` whose literal operand is `/`, the home directory, a drive root or a `.git` directory itself, and the owner's Pencil Generate deny. Nothing broader. This is the policy of `stack-guard` and `stack-public`, not a core pillar.
- **Drop over-defensive machinery & wolf-crying alerts:** No owner-only merge list (`.claude/kaizen-managed`), no `readonly_` (0444) live locks, no self-defending budget ratchets, no custom live swapper (`apply.py`), no `--restricted` sandbox / `commit-tree -S` / recovery state machine in `improve`, no `deny-rm.sh` or broad `git` / `deploy` denies.
- **Cross-platform (macOS, Linux, Windows):** `cmd/stack` and `internal/` are pure portable Go verified on `ubuntu-latest`, `macos-latest`, and `windows-latest` in CI. The public-text check (`stack-public`) handles both POSIX (`/Users/...`, `/home/...`) and Windows (`C:\Users\...`) local paths. Scheduled `cycle` runs via `launchd` on macOS only, while `stack run cycle` itself works cross-platform.
- **Boundary with `shuymn/dotfiles`:** `skills/` (Instruction Plane: `SKILL.md`, `references/`, `agents/`) stay in `shuymn/dotfiles` and deploy via `make apply` (chezmoi). `shuymn/stack` owns the Go CLI (`stack`), optional `extensions/`, and `--replay` `fixtures/`.

## 2. Repository Layout

```text
shuymn/stack/
├── cmd/stack/          # Go CLI entrypoint (`stack`)
├── internal/           # Minimal core packages (zero dependency on extensions/)
├── extensions/         # Optional extensions (observe, improve + writing.md, audit-due, public, guard, jev)
├── fixtures/           # Positive/negative fixtures for `--replay`
├── docs/               # Architecture spec
└── scripts/test        # Local & CI completion check
```

## 3. Core CLI Contract (`stack`)

Core runs cleanly with **zero extensions** configured.

```text
stack kit open <slug>                              # Record <slug> in <session_id>.kits (via $STACK_SESSION_ID); output state/items/handover/board + entry extensions
stack kit close <slug>                             # Remove <slug> from <session_id>.kits so completed kits do not resurrect after compaction
stack hook claude session-start                    # All events: export STACK_SESSION_ID via CLAUDE_ENV_FILE when provided; compact only: restore .kits + WORKER_LINE, then entry extensions
stack hook claude pre-tool-use                     # Deny raw `gh pr merge` and its `gh api` merge forms only
stack pr merge <pr> <head-sha> [--merge|--squash|--rebase] ...   # outbound on --subject/--body; verify checked head SHA, all checks passing, optional .claude/merge-check; merged after MERGED
stack run <entry|outbound|merged|cycle> [--dry-run] [--replay FILE] [--ext NAME]... [--range R] [--prompt-file F] [--title-file F] [--body-file F] [--branch B]
stack --version [--json]                           # Plain version; --json prints {"version","gh_model"}, gh_model = the gh release the raw-merge deny models
```

### Extension Protocol (Process Boundary)
Used only for optional extensions (`observe`, `improve`, `audit-due`, `public`, `jev-*`). Core never imports or inspects `extensions/`; it
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
- `stdin`: one UTF-8 JSON object, read to EOF. A live request has four strings: `{ "stage": "entry|outbound|merged|cycle",
  "repo": "...", "range": "...", "prompt": "..." }`. `repo` is the Git top-level of the cwd or `""`; `range` is
  `--range R`, else for outbound the literal `@{upstream}..HEAD` when Git resolves `@{upstream}`, else `""`;
  `prompt` is the `--prompt-file` contents or `""` (`kit open` and compaction send both empty). Every live request
  also has `kits`: `[{"slug","dir"}]`, the open kits of a valid `STACK_SESSION_ID` (`[]` otherwise). Outbound adds
  the strings `title`, `body` and `branch` (from `--title-file`, `--body-file`, `--branch`, or `pr merge`'s
  `--subject`/`--body`); `merged` from `pr merge` adds `merge`: `{"pr","head","oid"}`. `--replay` sends the fixture's
  request object as is once its `stage` matches; the fixture supplies the other fields. Extensions ignore unknown
  fields.
- `stdout`: one UTF-8 JSON object; all stdout bytes count toward a 1 MiB limit: `{ "status": "pass|warn|block", "context": "...", "findings":
  [...] }`. `status` is required; `context` (a string) and `findings` (an array) may be omitted and default to `""`
  and `[]`; unknown fields are ignored. Once core accepts the response, the exit code is ignored (a valid `block`
  counts even with a non-zero exit).
  stderr is forwarded to core's stderr with `[<name>] ` before each line.
- **Limits and failures:** one deadline per stage shared by its extensions (entry 10 s, outbound 120 s, merged 10 s,
  cycle 30 min). Before parsing stdout, core checks for a lookup or start failure, the deadline, a pipe still open when the
  cleanup grace period after exit ends, and oversized stdout; these, an invalid response and an extension left
  unstarted at the deadline become `warn` with a synthetic finding, never `block`.
- **Result:** statuses aggregate as `block > warn > pass` (no result = `pass`). Only `outbound` can block:
  at `entry`, `merged` and `cycle` core records a `block` response as `warn` with a synthetic finding. After writing
  the aggregate, `stack run` exits 1 on `block` and 0 on `pass` or `warn`; with a replay `expect`, 0 or 1 reports
  match or mismatch (usage errors exit 2; `--dry-run` exits 0). `stack kit open` (and compaction, within its budget)
  appends each entry `context` and a `warn` marker; `stack pr merge` refuses on an outbound `block` and prints each
  `merged` context after its `MERGED` line; findings appear only in the `stack run` aggregate.

## 4. Named Stages & Session Lifecycle

The core defines every stage; extensions attach only to these and never add one. Precedence within a stage is the
configured order (`[stages].<stage>`, or `--ext` for `stack run`), run sequentially under one stage deadline;
results aggregate `block > warn > pass`. A stage may have no built-in behaviour; it still runs its extensions.
Session identity and kits are core and not stages: `SessionStart` exports `STACK_SESSION_ID` via `CLAUDE_ENV_FILE`
(when Claude Code provides it) on every event (startup, resume, clear, compact, fork), and on `compact` restores
`<session_id>.kits` and `WORKER_LINE` (`opus-worker.md` + original brief).

| Stage | Fires (position) | Semantic | Can block | Failure of an extension | Extensions |
|---|---|---|---|---|---|
| **`entry`** | after `stack kit open` printed the kit; on `compact` after the restored kits (when at least one kit was restored); `stack run entry` | inject | no | `warn` marker; the kit is unaffected | optional `jev-router` |
| **`outbound`** | `stack run outbound` (push hooks, CI, skills); `stack pr merge` before any `gh` call when `--subject`/`--body` is given | gate | yes | `warn`, does not block | `public`, optional `jev-semantic` |
| **`merged`** | `stack pr merge` after the read-back confirmed `MERGED` on the checked head | observe / inject | no (the merge is irreversible) | `warn` marker after the `MERGED` line; exit code unchanged | `audit-due` |
| **`cycle`** | `launchd` -> `stack run cycle` | observe | no | `warn` | `observe` (+ optional `jev-classify`) and minimal `improve` (plain worktree + `claude -p`, single period stamp file `~/.local/state/stack/improve/YYYY-Www` to avoid daily re-runs when a proposal is not pushed). Apply via `make apply`. |

`merged` fires on every merge `stack pr merge` verifies; the raw-merge deny keeps it the only merge path of an agent
session within the guard's static scope. Tool use is not a core stage: Claude Code's own `PreToolUse` hook list
orders and combines tool-use gates, and `stack-guard` is one entry in it.

Retired legacy pieces: `startup`/`resume` auto-injection, `Stop` mtime block, `cwd` kit discovery,
`worker-signals.json`; `Edit/Write` budget hook, `.claude/kaizen-managed`, the T3 worktree-path worker guess,
`deny-rm.sh`; `apply.py` (1211 lines), `ready.py`, `watch.json`, `improve.py` recovery state machine.

### Intentionally Not in Core

The extension contract's table of contents. Each row fails at least one core condition (required with no extension
installed, unsafe if left to extensions, stable); the reasoning per built-in is in `docs/design/core-stages.md` §1.

| Capability | Why not core | Extension path |
|---|---|---|
| `check public` (local paths, private terms) | optional; secret scanners and hooks are alternatives; the term policy varies | `stack-public`: `outbound` extension, and `stack-public check <range> …` for direct callers |
| Local-path / private-term check of `pr merge --subject/--body` | same policy as `check public` | `outbound` stage, run by `stack pr merge` |
| Secrets read deny, local paths in `gh` publish inputs, catastrophic `rm` guard, Pencil Generate deny (on unless disabled) | optional; Claude Code settings, permission modes and scanners cover them; the lists are per user | `stack-guard hook claude pre-tool-use`, its own `PreToolUse` hook |
| `[safety]` settings | read only by the checks above | `~/.config/stack/safety.toml` |
| `budget status` and a `state.md` byte threshold | display only, no caller; a byte count is not what diverts attention | dropped; the steward judges `state.md` by content; an extension if a caller appears |
| Audit-due signal | its rule (K merges, `state.md` not updated) is policy | `stack-audit-due`: `merged` extension |
| `observe`, `improve` | optional, paid, owner opt-in | `cycle` extensions |
| `jev-*` | optional | `entry` / `outbound` / `cycle` extensions |

### Mid-Session Promote & Handover (Unified)
- **T0 (Ordinary session):** `SessionStart` writes `STACK_SESSION_ID` to `CLAUDE_ENV_FILE` (when provided) and injects nothing (zero misfire).
- **T1 (Promote):** User invokes `/stack` (or `/hiden`); agent runs `stack kit open <slug>`, writes `~/.local/state/stack/kits/<slug>/state.md`, and proceeds.
- **T3 (Handover):** Parent orchestrator launches successor with `Run \`stack kit open <slug>\` and continue this orchestration.` (per-environment handover instructions live in `~/.config/stack/handover/{t3,herdr}.md`).
- **Worker detection:** `agent_type == "opus-worker"`, `WORKER_LINE` in first prompt, or `-worktrees/agent-*` only. Never guess from `~/.t3/worktrees/`.

### Optional Jev (System One) Extensions
- **Opt-out:** Active when `TYPESAFE_API_KEY` / config is present. An installed `jev-*` that lacks its key or config opts out by answering `{"status":"pass"}` with no context.
- `jev-router` (`entry`): scores `references/*.md` for Just-in-Time injection.
- `jev-semantic` (`outbound`): flags meta-duty regression or unsupported claims at CI/PR.
- `jev-classify` (`cycle`): batch-classifies 7-day transcripts for corrections/drift and pre-filters memory duplicates.

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

[stages]
entry = ["jev-router"]
outbound = ["public", "jev-semantic"]
merged = ["audit-due"]
cycle = ["observe", "jev-classify", "improve"]
```

Extensions keep their own settings beside it: `~/.config/stack/safety.toml` (`stack-guard`, `stack-public`; the v10
`[safety]` keys `secrets`, `private_terms` and `deny_pencil_generate` with their v10 defaults), `~/.config/stack/improve.toml`
and `~/.config/stack/audit-due.toml`. A missing or broken `safety.toml` gives its defaults, so a typo never turns a
deny off.

## 6. Migration Roadmap

1. **Phase 1 (dotfiles immediate cleanup):** Stop ordinary-session misfires (`startup`/`resume` auto-injection, `Stop` block, `~/.t3/worktrees/` worker guess) and remove wolf-crying denies (`deny-rm.sh`, overly broad `git`/`deploy` rules) in `shuymn/dotfiles`.
2. **Phase 2 (`shuymn/stack` Go core):** Implement `kit open/close`, `hook claude session-start/pre-tool-use`, `pr merge`, `check public`, `budget status`, `run` with unit/fixture tests passing on macOS, Linux, and Windows CI. Wire into dotfiles and delete the legacy Python hooks they replace; `attention-budget.py status` and `public-check` stay while Phase 3's kaizen scripts still call them.
3. **Phase 3 (retire legacy `kaizen` & switch `cycle`):** Implement minimal `extensions/observe` and `extensions/improve` (with `writing.md`), switch `launchd` to `stack run cycle`, retire `apply.py`/`ready.py`/old `improve.py`/`.claude/kaizen-managed`, and update `skills/` (`attention.md`, `commit-pr.md`, `retro.md`), `agents/steward.md`, and `statusline.sh.tmpl`. Design: `docs/design/phase3-cycle.md`. Status: the extensions are in this repository (install and opt-in in `README.md`); the `launchd` switch, `skills/` updates and kaizen retirement follow in `shuymn/dotfiles` in the cutover order of the design §6.
4. **Phase 4 (Jev extensions):** Add `jev-classify` -> `jev-semantic` -> `jev-router` with `fixtures/` and `--replay` verification.
5. **Core stages (v11):** Give each stage one semantic, add `merged` with `stack-audit-due`, drop the `state.md` byte threshold and `budget status`, and move `check public` and the guards other than the raw-merge deny to `stack-public` and `stack-guard`, adding and wiring each extension before removing the core copy so no guard is ever off. Design and PR order: `docs/design/core-stages.md` §7. Status: designed; v10 behaviour stays in the code until those PRs land.
