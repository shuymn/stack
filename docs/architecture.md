# `shuymn/stack` Architecture (v11)

## 1. Core Thesis

Single-user personal orchestration foundation built following `extensible-by-design`:
- **The core is the lifecycle and the extension contract.** It defines every stage (§4) and the protocol extensions use (§3), and holds only built-in behaviour that is required for every session with no extension installed, wrong or unsafe if left to extensions, and stable: session identity, kits and their restore at compaction, the extension protocol, and the guarantee that each stage fires (the checked `stack pr merge` and the raw `gh pr merge` deny, without which `merged` could be bypassed). Restore means the contract (which parts compaction restores, in which order, within one budget, with a marker for each omitted part); what a kit, worker or handover contains is config and kit files. Everything else is an extension or external, listed in §4 "Intentionally not in core". Design: `docs/design/core-stages.md`.
- **Safety extensions guard hard boundaries only (near-zero false positives, irreversible hazards):** secrets reads, local paths and private terms in public text, a direct recursive `rm` whose literal operand is `/`, the home directory, a drive root or a `.git` directory itself, and the owner's Pencil Generate deny. Nothing broader. This is the policy of `stack-guard` and `stack-public`, not a core pillar.
- **Drop over-defensive machinery & wolf-crying alerts:** No owner-only merge list (`.claude/kaizen-managed`), no `readonly_` (0444) live locks, no self-defending budget ratchets, no custom live swapper (`apply.py`), no `--restricted` sandbox / `commit-tree -S` / recovery state machine in `improve`, no `deny-rm.sh` or broad `git` / `deploy` denies.
- **Cross-platform (macOS, Linux, Windows):** `cmd/stack` and `internal/` are pure portable Go verified on `ubuntu-latest`, `macos-latest`, and `windows-latest` in CI. The public-text check (`stack-public`) handles both POSIX (`/Users/...`, `/home/...`) and Windows (`C:\Users\...`) local paths. Scheduled `cycle` runs via `launchd` on macOS only, while `stack run cycle` itself works cross-platform.
- **Boundary with `shuymn/dotfiles`:** `skills/` (Instruction Plane: `SKILL.md`, `references/`, `agents/`) stay in `shuymn/dotfiles` and deploy via `make apply` (chezmoi). `shuymn/stack` owns the Go CLI (`stack`), optional `extensions/`, and `--replay` `fixtures/`. The side whose code reads or writes an artifact owns its format: what `stack` reads or writes and the owner's setup also uses (kit files, `sessions/<id>.kits`, the restored text, the audit-due line) is specified here, listed in `docs/design/kit-contracts.md` §2, and `shuymn/dotfiles` says only when and why to use it.

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
stack pr merge <pr> <head-sha> [--merge|--squash|--rebase] ...   # [merge].message_checks (fail-closed) on --subject/--body; verify checked head SHA, all checks passing, optional .claude/merge-check; merged after MERGED
stack run <entry|outbound|merged|cycle> [--dry-run] [--replay FILE] [--ext NAME]... [--range R] [--prompt-file F] [--title-file F] [--body-file F] [--branch B]
                                                   # --title-file/--body-file/--branch: outbound only, not with --replay (exit 2)
stack --version [--json]                           # Plain version; --json prints {"version","gh_model"}, gh_model = the gh release the raw-merge deny models (kept for stack-observe)
```

`stack-guard hook claude pre-tool-use` is a bundled Claude Code hook built from `extensions/guard`, wired as its own
`PreToolUse` entry; it is not a stage extension and the core never runs it. It and the core share `internal/shell`,
`internal/ghargs`, `internal/paths` and `internal/hookio`, a narrow in-repo dependency allowed by `depguard` for
`extensions/guard` only; every other extension uses only the protocol and the documented CLI. Its deny reasons start
with `stack-guard (<rule>): `. With both hooks, an input matching the raw-merge rule and a moved rule is denied by
both, and the reason shown may differ from v10's single first-rule reason.

**Compatibility exceptions of v11** (`docs/design/core-stages.md` §4.9): `stack check public` and `stack budget
status` are removed (`stack-public check` replaces the first); a `block` answer at `entry` or `cycle` becomes `warn`
with `error` set and exit 0, so replay fixtures of those stages expecting `block` move to `warn`; `pr merge`
checks a merge message only when `[merge].message_checks` lists a check (v10 checked it by default; the default
`[]` follows from running no content check with zero extensions), and a message refused there exits 1 instead of
v10's 2; the default `[stages].outbound` gains `public`, so `stack run outbound` with nothing installed lists it
under `skipped`.

### Extension Protocol (Process Boundary)
Used only for optional extensions (`observe`, `improve`, `audit-due`, `public`, `jev-*`); `stack-guard` is a hook, not a stage extension. Core never imports or inspects `extensions/`; it
talks to an extension only through this contract. Details (validation commands, usage errors, plan format, replay
fixtures) are in `docs/design/phase2-core.md` §4.7.
- **Evolution:** existing fields, types, defaults, statuses, stages and meanings are kept. New fields are optional,
  safe to ignore and distinct from existing keys (also case-insensitively); a new stage runs only when selected. An
  incompatible change needs an explicit protocol selection that keeps this contract for current extensions. No
  version field until a second dialect exists. The v11 changes listed above under "Compatibility exceptions of v11"
  (`block` demotion at `entry` and `cycle`, and the changed `[stages].outbound` default) are the deliberate
  exceptions to this rule, made once with the stage table rather than by protocol selection.
- **Discovery:** `<name>` matches `^[a-z0-9][a-z0-9-]*$`; names come from `[stages].<stage>` (at `stack pr merge`'s message
  check, from `[merge].message_checks` instead), or only from the repeatable `--ext NAME` for `stack run`, and run in
  that order. Core picks the first runnable `stack-<name>` in the
  absolute `PATH` entries (on Windows `stack-<name>.exe`; `PATHEXT` is ignored). A missing name is skipped without a
  warning; `stack run` lists it under `skipped` (or in the `--dry-run` plan).
- **Invocation:** no arguments; cwd is the invoker's; the environment is inherited, with `STACK_STAGE` set to the
  stage; `STACK_SESSION_ID` is passed through when set.
- `stdin`: one UTF-8 JSON object, read to EOF. A live request has four strings: `{ "stage": "entry|outbound|merged|cycle",
  "repo": "...", "range": "...", "prompt": "..." }`. `repo` is the Git top-level of the cwd or `""`; `range` is
  `--range R`, else for outbound the literal `@{upstream}..HEAD` when Git resolves `@{upstream}`, else `""`
  (always `""` for the outbound run of `pr merge`, which checks only the message);
  `prompt` is the `--prompt-file` contents or `""` (`kit open` and compaction send both empty). Live `entry` and
  `merged` requests also have `kits`: `[{"slug","dir"}]`, the open kits of a valid `STACK_SESSION_ID` (`[]`
  otherwise). Outbound adds the strings `title`, `body` and `branch` (from `--title-file`, `--body-file`, `--branch`,
  or `pr merge`'s `--subject`/`--body`, `""` when absent); `merged` from `pr merge` adds `merge`:
  `{"pr","head","oid"}`. `--replay` sends the fixture's
  request object as is once its `stage` matches; the fixture supplies the other fields. Extensions ignore unknown
  fields.
- `stdout`: one UTF-8 JSON object; all stdout bytes count toward a 1 MiB limit: `{ "status": "pass|warn|block", "context": "...", "findings":
  [...] }`. `status` is required; `context` (a string) and `findings` (an array) may be omitted and default to `""`
  and `[]`; unknown fields are ignored. Once core accepts the response, the exit code is ignored (a valid `block`
  counts even with a non-zero exit).
  stderr is forwarded to core's stderr with `[<name>] ` before each line.
- **Limits and failures:** one deadline per stage shared by its extensions (entry 10 s, outbound 120 s, merged 10 s,
  cycle 30 min). Entry at compaction also ends by the hook's 55 s cut-off, after which no further work starts and
  the hook writes the built restore before Claude Code's 60 s, and runs only when its runner deadline (below) is at
  least 1 s away; there and at `merged` run by `stack pr merge`, whose 10 s include request preparation (a manual
  `stack run merged` prepares its request outside the 10 s, like every `stack run` stage), the runner's deadline is the stage's end less the 2 s pipe-reaping allowance, so reaping
  fits inside the stage. An extension not started by the deadline is left unstarted. Before parsing stdout, core checks for a lookup or start failure, the deadline, a pipe still open when the
  cleanup grace period after exit ends, and oversized stdout; these, an invalid response and an extension left
  unstarted at the deadline become `warn` with a synthetic finding, never `block`.
- **Result:** statuses aggregate as `block > warn > pass` (no result = `pass`). Only `outbound` can block:
  at `entry`, `merged` and `cycle` core records a `block` response as `warn` with a synthetic finding and `error`
  set, so consumers treat it as an extension failure. After writing
  the aggregate, `stack run` exits 1 on `block` and 0 on `pass` or `warn`; with a replay `expect`, 0 or 1 reports
  match or mismatch (usage errors exit 2; `--dry-run` exits 0). `stack kit open` (and compaction, within its budget)
  appends each entry `context` and a `warn` marker; `stack pr merge` refuses on a `block` from a check listed in `[merge].message_checks`
  and also when a listed check is missing or fails (the merge is irreversible), and prints each `merged` context after its
  `MERGED` line; findings appear only in the `stack run` aggregate.

## 4. Named Stages & Session Lifecycle

The core defines every stage; extensions attach only to these and never add one. Precedence within a stage is the
configured order (`[stages].<stage>`, `[merge].message_checks` for `stack pr merge`'s message check, or `--ext`
for `stack run`), run sequentially under one stage deadline;
results aggregate `block > warn > pass`. A stage may have no built-in behaviour; it still runs its extensions.
Session identity and kits are core and not stages: `SessionStart` exports `STACK_SESSION_ID` via `CLAUDE_ENV_FILE`
(when Claude Code provides it) on every event (startup, resume, clear, compact, fork), and on `compact` restores
`<session_id>.kits` and `WORKER_LINE` (`opus-worker.md` + original brief).

| Stage | Fires (position) | Semantic | Can block | Failure of an extension | Extensions |
|---|---|---|---|---|---|
| **`entry`** | after `stack kit open` printed the kit; on `compact` after the restored kits (when at least one kit was restored), reusing the restored session id and kits, when the hook's cut-off leaves time for it (otherwise `[entry skipped: deadline]`); `stack run entry` | inject | no | `warn` marker; the kit and the restore are unaffected | optional `jev-router` |
| **`outbound`** | `stack run outbound` (push hooks, CI, skills) with `[stages].outbound`; `stack pr merge` before any `gh` call when `--subject`/`--body` is given, with `[merge].message_checks` (default `[]`: nothing runs) | gate | yes | `warn`, does not block; at `pr merge` a listed check that is missing or fails refuses the merge | `public`, optional `jev-semantic` |
| **`merged`** | `stack pr merge` at its verified-success point (read-back of `MERGED` on the checked head with a full merge OID, direct or `--auto`, including the final read-back), unless cancelled by SIGINT/SIGTERM; `stack run merged` | observe / inject | no (the merge is irreversible) | `warn` marker after the `MERGED` line; exit code unchanged | `audit-due` |
| **`cycle`** | `launchd` -> `stack run cycle` (macOS); `stack run cycle` anywhere | observe | no | `warn` | `observe` (+ optional `jev-classify`) and minimal `improve` (plain worktree + `claude -p`, single period stamp file `~/.local/state/stack/improve/YYYY-Www` to avoid daily re-runs when a proposal is not pushed). Apply via `make apply`. |

`merged` fires on every merge `stack pr merge` verifies, except after a cancel (the owner asked to stop; the merge
result stands); the raw-merge deny keeps it the only merge path of an agent
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
| Local-path / private-term check of `pr merge --subject/--body` | same policy as `check public` | outbound extensions listed in `[merge].message_checks` (the owner lists `public`), run by `stack pr merge` |
| Secrets read deny, local paths in `gh` publish inputs, catastrophic `rm` guard, Pencil Generate deny (on unless disabled) | optional; Claude Code settings, permission modes and scanners cover them; the lists are per user | `stack-guard hook claude pre-tool-use`, a bundled hook with its own `PreToolUse` entry (§3) |
| `[safety]` settings | read only by the checks above | `~/.config/stack/safety.toml` |
| `budget status` and a `state.md` byte threshold | display only, no caller; a byte count is not what diverts attention | dropped; the steward judges `state.md` by content; an extension if a caller appears |
| Audit-due signal | its rule (K merges, kit state not updated) is policy | `stack-audit-due`: `merged` extension |
| `observe`, `improve` | optional, paid, owner opt-in | `cycle` extensions |
| `jev-*` | optional | `entry` / `outbound` / `cycle` extensions |

### Mid-Session Promote & Handover (Unified)
- **T0 (Ordinary session):** `SessionStart` writes `STACK_SESSION_ID` to `CLAUDE_ENV_FILE` (when provided) and injects nothing (zero misfire).
- **T1 (Promote):** User invokes `/stack` (or `/hiden`); agent runs `stack kit open <slug>`, writes `~/.local/state/stack/kits/<slug>/state.md`, and proceeds.
- **T3 (Handover):** Parent orchestrator launches successor with `Run \`stack kit open <slug>\` and continue this orchestration.` (per-environment handover instructions live in `~/.config/stack/handover/{t3,herdr}.md`).
- **Worker detection:** `agent_type == "opus-worker"`, `WORKER_LINE` in first prompt, or `-worktrees/agent-*` only. Never guess from `~/.t3/worktrees/`.

### Optional Jev (System One) Extensions
- **Opt-in by key:** With no key, installed Jev extensions answer a live request (no `jev` replay field) with `pass` without a call. See the [setup instructions](../README.md#install) and the [key and failure contract](design/phase4-jev.md#31-opt-out-failure-batching-and-budgets-item-1).
- `jev-router` (`entry`): deferred. It would point to the hiden reference for each kit's step in progress. The default stage skips the absent binary. See the [router trigger](design/phase4-jev.md#32-what-each-extension-sends-and-returns-item-2).
- `jev-semantic` (`outbound`): warns on possible meta-duty regressions and unsupported claims. The hiden `commit-pr` step calls it for PR creation and editing after `stack-public check` passes. CI runs without a live Jev key. See the [supported caller and request contract](design/phase4-jev.md#32-what-each-extension-sends-and-returns-item-2).
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

[merge]
message_checks = []                   # outbound extensions pr merge requires for --subject/--body; the owner sets ["public"]
```

Extensions keep their own settings beside it: `~/.config/stack/safety.toml` (`stack-guard`, `stack-public`; the
top-level keys `secrets`, `private_terms` and `deny_pencil_generate`. See the
[built-in secret entries](design/core-stages.md#46-configstacksafetytoml)), `~/.config/stack/improve.toml`
and `~/.config/stack/audit-due.toml`. A missing `safety.toml` or key gives its default. A broken `safety.toml` means
the default rules apply (custom entries are not kept until it is fixed), so a typo never turns a deny off:
`stack-guard` denies by the defaults, `stack-public check` exits 2 and `stack-public` at a stage answers `block`.
Arrays in `[stages]` and `[merge]` replace the default. `[stages].outbound` is the selection for `stack run
outbound`, where a missing extension is skipped; `[merge].message_checks` is separate, and every name in it is
required: `stack pr merge --subject/--body` refuses before any `gh` call when one is missing, fails or blocks.
`stack pr merge` loads and validates this file before any `gh` call, so a broken file exits 2.

## 6. Migration Roadmap

1. **Phase 1 (dotfiles immediate cleanup):** Stop ordinary-session misfires (`startup`/`resume` auto-injection, `Stop` block, `~/.t3/worktrees/` worker guess) and remove wolf-crying denies (`deny-rm.sh`, overly broad `git`/`deploy` rules) in `shuymn/dotfiles`.
2. **Phase 2 (`shuymn/stack` Go core):** Implement `kit open/close`, `hook claude session-start/pre-tool-use`, `pr merge`, `check public`, `budget status` (both left the core in item 5), `run` with unit/fixture tests passing on macOS, Linux, and Windows CI. Wire into dotfiles and delete the legacy Python hooks they replace; `public-check` stays while the legacy kaizen scripts still call it.
3. **Phase 3 (retire legacy `kaizen` & switch `cycle`):** Implement minimal `extensions/observe` and `extensions/improve` (with `writing.md`), switch `launchd` to `stack run cycle`, retire `apply.py`/`ready.py`/old `improve.py`/`.claude/kaizen-managed`, and update `skills/` (`attention.md`, `commit-pr.md`, `retro.md`), `agents/steward.md`, and `statusline.sh.tmpl`. Design: `docs/design/phase3-cycle.md`. Status: the extensions, the `stack-cycle` `launchd` job, the `skills/` updates and improve's opt-in are done; the last step, retiring the kaizen scripts in `shuymn/dotfiles` (design §6, dotfiles PR 4), waits for at least a week of daily `stack-cycle` runs with the design's P1–P6 holding and P5 (a real improve attempt) observed once.
4. **Phase 4 (Jev extensions):** `jev-semantic` is implemented. `jev-classify` follows Phase 3 (#56) because it consumes observe's output. `jev-router` is deferred under its own trigger. See the [Phase 4 design](design/phase4-jev.md) for calibration, offline replay and implementation order.
5. **Core stages (v11):** Done, wired in `shuymn/dotfiles` and installed: each stage has one semantic, `merged` runs `stack-audit-due`, the `state.md` byte threshold and `budget status` are gone, and `check public` and every guard but the raw-merge deny live in `stack-public` and `stack-guard`, with `pr merge` running `[merge].message_checks`. The guards also cover Claude Code's `PowerShell` tool (`docs/design/powershell-guard.md`), checked on Windows PowerShell 5.1 and PowerShell 7 by the `powershell-oracle` CI job. Design: `docs/design/core-stages.md`.
