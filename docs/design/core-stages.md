# Core Stages Design: Stages and the Extension Boundary

Design for issues #30 (keep only stages and the extension contract in the core) and #28 (signal a due audit after
a verified merge), applied to `docs/architecture.md` v10 and the code at `5f057a7`. It changes the approved design,
so `docs/architecture.md` moves to v11 with it. Code is cited as `path:line` at that commit; skill text in
`shuymn/dotfiles` as `dotfiles:<path>:<line>`; the decision rule is the "Quick Decision Guide" of the
`extensible-by-design` skill (`~/.claude/skills/extensible-by-design/SKILL.md`).

## 0. Principles

1. **The core is the lifecycle and the contract.** A built-in behaviour stays in core only when all three hold:
   (R) it is required for every session with no extension installed, (U) it is wrong or unsafe if left to
   extensions, (S) its contract is stable. Everything else is an extension, or external, and is listed in the
   negative-space table (§3) with its reason and path.
2. **Each stage has one semantic.** A stage is a gate (can block) or an injection/observation point (cannot). The
   core enforces that, so an extension author reads the semantic from the stage table and never from the caller.
3. **Extensions attach only to stages the core defines.** They never add a stage. A new stage is a core change and
   follows the protocol's evolution rule (`docs/architecture.md` §3 "Evolution").
4. **Nothing the owner uses today loses behaviour between PRs.** Checks move by adding the extension first, wiring
   it, verifying it live, and only then removing the core copy (§7).
5. **Hard-boundary policy moves with the checks.** "Near-zero false positives, irreversible hazards only, no
   wolf-crying" (`docs/architecture.md:6-9`) stops being a pillar of the core and becomes the policy of the safety
   extensions (§3).

## 1. Inventory of Current Built-ins

Verdict per behaviour against R, U and S (§0.1). "Core" needs all three; one failing condition is enough for
"Extension".

| Built-in (file:line) | R | U | S | Verdict |
|---|---|---|---|---|
| `STACK_SESSION_ID` export on every SessionStart (`internal/session/session.go:54-80`) | yes: every stage that needs the session (kits, `merged`) keys on it | yes: identity must be one value shared by core and every extension | yes: one env var, unchanged since v10 | **Core** |
| `kit open/close`, `<id>.kits` store and lock (`internal/cli/kit.go:55-139`, `internal/kit/store.go:46-200`) | yes | yes: the record decides what compaction restores; a second writer would race the lock | yes | **Core** |
| Kit context renderer: state files, items, handover, board (`internal/kit/render.go:90-216`) | yes: the kit is what `kit open` and compaction return | yes: compaction runs inside a hook with a hard output limit; the renderer and the budget must agree | yes | **Core** |
| Compact restore and its 9,500-char budget (`internal/session/session.go:115-170`, `internal/session/budget.go`) | yes | yes: only the core sees the whole budget; it is where essence is actually lost (#28 comment) | yes | **Core** |
| Worker detection and brief restore (`internal/session/session.go:214-260`) | yes: no-op for non-workers, and a worker that compacts without it loses its brief | yes: it shares the compact budget with the kits | yes: marker and agent types are config | **Core** |
| 3,000-byte `state.md` note (`internal/kit/render.go:24-25,123-126`) | no: a size number is not what diverts attention (#28 comment) | no | no: the threshold is policy | **Removed** (§5.3) |
| Extension discovery, protocol, deadlines, aggregation (`internal/ext/ext.go:27-418`, `internal/ext/request.go`) | yes | yes: it is the contract | yes: evolution rule | **Core** |
| `stack run <stage>` (`internal/cli/run.go:31-80`) | yes: the stage runner | yes | yes | **Core** |
| Entry stage after `kit open` (`internal/cli/run.go:216-238`) | yes: the stage must fire | yes | yes | **Core** |
| `stack pr merge` checked head, checks, `merge-check`, read-back (`internal/merge/merge.go:157-232`) | yes: the only merge path, so `merged` fires (#30) | yes: a merge is irreversible and the stage would be skipped otherwise | yes: v10 contract; repository policy already plugs in through `.claude/merge-check` | **Core** |
| Raw `gh pr merge` deny (`internal/guard/ghmerge.go:20-102`, order at `internal/guard/guard.go:68-81`) | yes: without it `merged` can be bypassed and its extensions silently skipped (#30) | yes | yes: the rule is fixed; the gh model it parses is versioned (`internal/ghargs/table.go:3`) | **Core** |
| `stack --version --json` `gh_model` (`internal/cli/cli.go:197-210`) | yes: reports the model of the raw-merge deny | yes | yes | **Core** |
| Secrets read deny (`internal/guard/secrets.go:40-321`) | no: optional; Claude Code permission rules and modes cover reads | no: a guard that is missing fails open today too (`hookio` fail-open) | no: the list is per user (`[safety].secrets`) | **Extension** (`stack-guard`) |
| Local paths in `gh` publish inputs (`internal/guard/ghpublish.go:13-49`) | no | no | no: shares the term and path policy with `check public` | **Extension** (`stack-guard`) |
| Catastrophic `rm` (`internal/guard/rmroot.go:15-83`) | no | no: permission modes and sandboxing cover it | yes, but R fails | **Extension** (`stack-guard`) |
| Pencil Generate deny (`internal/guard/pencil.go:31-173`) | no: one owner's tool | no | no: owner decision, tool-specific | **Extension** (`stack-guard`) |
| `stack check public` (`internal/checkpub/checkpub.go:126-383`, `internal/cli/checkpublic.go:17-83`) | no: optional; secret scanners and hooks are alternatives | no | no: the private-term policy varies | **Extension** (`stack-public`) |
| Local path / private term check of `pr merge --subject/--body` (`internal/merge/merge.go:133-150`) | no | no | no: same policy as `check public` | **Extension**: the `outbound` stage, run by `pr merge` (§4.4) |
| `[safety]` config table (`internal/config/config.go:52-57,80-84,160-163`) | no: only the moved checks read it | no | no | **Extension config** (`~/.config/stack/safety.toml`, §4.6) |
| `stack budget status` (`internal/budget/budget.go:60-262`) | no: display only, no program caller (§6) | no | no | **Removed** (§5.3) |

The foundations `shell`, `ghargs`, `paths` and `hookio` hold no policy: they parse inputs. The raw-merge deny
needs `shell`, `ghargs` and `gqlparser`, so they stay in core; `stack-guard` imports the same packages (§4.5).

## 2. Stage Table

The core defines these four stages. Precedence within a stage is the configured order (`[stages].<stage>`, or the
repeatable `--ext` of `stack run`); extensions run sequentially in that order under one stage deadline; results
aggregate `block > warn > pass`. A stage may have no built-in behaviour; it still runs its extensions.

| Stage | Fires (position) | Semantic | Can block | Deadline | Failure of an extension |
|---|---|---|---|---|---|
| `entry` | after `stack kit open` printed the kit; on SessionStart `compact` after the restored kits, when the session has at least one kit; `stack run entry` | inject | no | 10 s | `warn` marker after the kit; kit output unaffected |
| `outbound` | `stack run outbound` (push hooks, CI, skills); `stack pr merge` before any `gh` call when `--subject` or `--body` is given | gate | yes: `stack run` exits 1, `pr merge` refuses | 120 s | `warn`; does not block |
| `merged` | `stack pr merge` after the read-back confirmed `MERGED` on the checked head with a full merge OID (both the direct and `--auto` paths); `stack run merged` | observe / inject | no: the merge is irreversible | 10 s | `warn` marker after the `MERGED` line; exit code unchanged |
| `cycle` | `launchd` → `stack run cycle` (macOS); `stack run cycle` anywhere | observe | no | 30 min | `warn` |

**Block at a stage that cannot block.** At `entry`, `merged` and `cycle` the core reports a `block` response as
`warn` with the synthetic finding `{"extension":<name>,"error":"block is not allowed at stage <stage>"}`. Today
`stack run entry` and `stack run cycle` exit 1 on `block` (`internal/cli/run.go:75-77`) while `kit open` shows it
without enforcing it (`internal/cli/run.go:244-249`); after this change `stack run` exits 1 only at `outbound`.
`--replay` fixtures compare the aggregate after this rule.

**Guarantee that each stage fires.** `entry` fires on every `kit open` and every compact that restores a kit;
`merged` fires on every merge `stack pr merge` verifies, and the raw-merge deny makes `stack pr merge` the only
merge path an agent session has within the guard's static scope (`docs/design/phase2-core.md` §0.4, §6). Merges
outside it (the GitHub UI, another machine, a `gh` alias) do not fire `merged`; that is accepted, as the stage
observes the agent's own work. `outbound` fires where its callers run it; the core guarantees the `pr merge` call
site only. `cycle` fires where the owner schedules it.

**Tool use is not a core stage.** Claude Code's `PreToolUse` hook list already orders, filters (matchers) and
combines (any deny wins) tool-use gates; §4.5 and §8.1 explain why the core does not duplicate it.

## 3. Intentionally Not in Core

This table is the extension contract's table of contents; `docs/architecture.md` §4 carries it.

| Capability | Why not core | Extension path |
|---|---|---|
| `check public` (local paths, private terms) | optional; secret scanners and hooks are alternatives; the term policy varies | `stack-public`: `outbound` extension, and `stack-public check` for direct callers |
| Local-path / private-term check of `pr merge --subject/--body` | same policy as `check public` | `outbound` stage, run by `stack pr merge` |
| Secrets read deny, local paths in `gh` publish inputs, catastrophic `rm` guard, Pencil Generate deny | optional; Claude Code settings, permission modes and scanners cover them; the lists are per user | `stack-guard hook claude pre-tool-use`, its own `PreToolUse` hook (external to the stages) |
| `[safety]` settings | read only by the checks above | `~/.config/stack/safety.toml`, read by `stack-guard` and `stack-public` |
| `budget status` and the `state.md` byte threshold | display only, no program caller; a byte count is not what diverts attention | dropped; the steward judges `state.md` by content (#28); an extension if a caller appears |
| Audit-due signal | its rule (K merges, `state.md` not updated) is policy | `stack-audit-due`: `merged` extension |
| `observe`, `improve` | optional, paid, owner opt-in | `cycle` extensions (already) |
| `jev-*` | optional | `entry` / `outbound` / `cycle` extensions (already) |

**Policy of the safety extensions** (moved from `docs/architecture.md:6-9`): guard only irreversible,
near-zero-false-positive hazards; no owner-only merge lists, live locks, budget ratchets, recovery state machines or
broad denies. The rules themselves are unchanged (`docs/design/phase2-core.md` §4.3, §4.5).

## 4. Contracts

### 4.1 Protocol additions

All additions follow the evolution rule: new optional request fields, safe to ignore, distinct from existing keys;
`merged` is a new stage that runs only when selected.

- **Stages:** `entry|outbound|merged|cycle` in the request's `stage`, `STACK_STAGE` and `stack run <stage>`.
- **`kits`** (every live request): `[{"slug": "<slug>", "dir": "<absolute kit dir>"}]`, the kits listed in
  `<id>.kits` for a valid `STACK_SESSION_ID` whose directory exists, in `.kits` order; `[]` without a session id.
  Read under the `.kits` lock with a 1 s timeout (as compact, `docs/design/phase2-core.md` §4.2 step 2); on a
  timeout it is `[]` and core writes `stack: <id>.kits locked; kits not sent` to stderr. Extensions get session
  identity and kits from core and never parse `config.toml` or the sessions directory themselves.
- **`merge`** (`merged` live requests): `{"pr": <int>, "head": "<full H>", "oid": "<merge OID>"}`. `stack run
  merged` without `--replay` sends no `merge` field.
- **`title`, `body`, `branch`** (`outbound` live requests): strings, `""` when absent. `stack run outbound` gains
  `--title-file F`, `--body-file F` (UTF-8, resolved as `check public` resolves them today, exit 2 otherwise) and
  `--branch B`. `stack pr merge` sends `--subject` as `title` and `--body` as `body`, with `range` `""`.
- **Deadline:** `merged` 10 s.
- **Block demotion:** §2.

### 4.2 `stack pr merge` and the `merged` stage

1. Steps 1–8 unchanged (`docs/design/phase2-core.md` §4.4), except that step 1 no longer scans `--subject`/`--body`.
2. **Outbound for the merge message.** When `--subject` or `--body` is given, after step 1 and before the first
   `gh` call, run `[stages].outbound` with the request of §4.1. `block` → exit 1 before any `gh` call, stderr
   `stack pr merge: outbound blocked: <name>: <context>` per blocking result; `warn` or `pass` → continue, with each
   warn marker on stderr. Without those flags the stage is not run. With no outbound extension installed nothing is
   checked.
3. **After a verified merge** (`reportMerged`, `internal/merge/merge.go:236-251`, and its `--auto` path): print
   `MERGED <H> <oid>` first, exactly as today, then run `[stages].merged` unless the context is already cancelled
   (SIGINT/SIGTERM: the owner asked to stop; the merge result stands). For each result that is not `pass` or has a
   non-empty `context`, print to stdout, after the `MERGED` line, the `warn` marker
   `[extension <name>: warn: <reason>]` when it warned, then its trimmed `context`. Exit 0 whatever the results.
   With no extension installed or selected, stdout and the exit code are byte-identical to today.
4. Refusals and "result unknown" outcomes never run `merged`.

### 4.3 `entry` at compaction

Step 2 of `docs/design/phase2-core.md` §4.2 gains a last step: when at least one kit was restored, run
`[stages].entry` (10 s deadline, after the boards' 40 s, inside the hook's 60 s timeout) with the request of §4.1
(`prompt` and `range` `""`). Each result that `kit open` would show becomes one piece placed after the boards,
capped at 4,500 chars like a kit block, cut with `[...cut N chars]`, or replaced by
`[entry context omitted: context limit: <name>]` when not even its marker fits. Entry contexts reserve nothing:
they come back only when the kits, brief and handover left room. This makes compaction return what `kit open`
showed, which v10 did not (C19 of `docs/design/phase2-core.md` asserted that no extension runs; it changes with this
design).

### 4.4 `stack-public` (outbound extension)

`extensions/public`, binary `stack-public`. Two modes:

- **Stage mode** (no arguments, the protocol): scans `range` (when non-empty), `title`, `body` and `branch` exactly
  as `stack check public` does today (`docs/design/phase2-core.md` §4.5). Hits → `block`, `context` the hit lines
  (`check public: <where>: …`), one finding per hit. Its own errors (unreadable or non-UTF-8 private terms, a git
  failure, a range git cannot resolve) → `block` with the error as context: it fails closed by its own choice.
- **CLI mode** `stack-public check <base>..<head> [--title-file F] [--body-file F] [--branch B]`: the same flags,
  stdout, stderr lines and exit codes (0 clean, 1 hits, 2 usage, input, config or git error) as `stack check public`
  today. Direct callers use this mode, so a missing binary fails closed (command not found) instead of being skipped
  as a stage extension would be.

`internal/publictext` and `internal/checkpub` move to `extensions/internal/` (Go's `internal` rule keeps them
private to `extensions/`); `stack-guard` and `stack-public` share `publictext`.

### 4.5 `stack-guard` (external hook)

`extensions/guard`, binary `stack-guard`, run by Claude Code as `stack-guard hook claude pre-tool-use`, wired in the
owner's settings as its own `PreToolUse` entry with matcher `Bash|Read|NotebookRead|Grep|Glob|mcp__pencil__execute`,
exec form, `timeout: 5`, `onFailure: "continue"`. It holds the secrets, gh-publish, rmroot and pencil rules of
`docs/design/phase2-core.md` §4.3 unchanged, fails open like the core hook (`hookio`), and reads
`~/.config/stack/safety.toml` (§4.6). It is not a stage extension: invoked without arguments it prints usage and
exits 2, so a `guard` name put in `[stages]` by mistake shows up as a `warn`. The core hook keeps the raw-merge rule
only; Claude Code runs both hooks of a matching event in parallel and denies when either denies.

Import boundary: `stack-guard` imports the policy-free foundations `internal/shell`, `internal/ghargs`,
`internal/paths` and `internal/hookio`. `.golangci.yaml` `depguard` gains an allow list for
`extensions/guard/**` (non-test) naming exactly those four; every other extension keeps "protocol and documented
CLI only". Copying the 1k-line shell and gh models would let the raw-merge deny and the guard drift apart on the same
gh version; moving them to a public `pkg/` tree would rename every core import for the same coupling.

### 4.6 `~/.config/stack/safety.toml`

The v10 `[safety]` table as its own top-level file read by both safety extensions: the keys `secrets`,
`private_terms` and `deny_pencil_generate`, unchanged in meaning and defaults (`internal/config/config.go:80-84`:
the T3 secrets directory and `.dev.vars`; `~/.config/stack/private-terms`; `true`).

Missing file or key → the default. A broken file → the defaults, as the core hook does today
(`internal/config/config.go:209-211`), so a typo never turns a deny off; `stack-public check` exits 2 instead, as
`check public` does on a broken config. Core drops `[safety]`; a leftover `[safety]` table in `config.toml` gets
the existing unknown-key warning and changes nothing.

### 4.7 `stack-audit-due` (merged extension)

`extensions/auditdue`, binary `stack-audit-due`, deterministic, no model or network call, `status` always `pass`
except on its own errors.

- **Silent without a kit.** No `merge` field (a manual `stack run merged`) or empty `kits` → `pass`, no context, no
  write.
- **State file** `<kit dir>/audit-due.json`, one per kit: `{"merges": <merges since the last due signal>,
  "state": "<sha256 of the kit state at the previous counted merge>", "oid": "<last counted merge OID>"}`. Written
  to a temp file in the kit dir and renamed. No lock: the orchestrator merges one PR at a time; a lost increment
  under concurrent merges only delays an advisory line.
- **Kit state hash:** SHA-256 over, for `NORTH.md` then `state.md`, the file name, NUL, its bytes or `absent`,
  NUL. The same kit-state files the renderer reads (`internal/kit/render.go:37`), so a kit that keeps its essence
  in `NORTH.md` is judged the same way.
- **Per kit in `kits`, per merge:** if `merge.oid` equals the stored `oid`, nothing changes (a replay or retry does
  not count twice). Otherwise `n = merges + 1` and `h` = the current hash. Due when `n >= K` (reason
  `<n> merges since the last audit`), or else when the state file existed and `h` equals its `state` (reason
  `merged without a state update since the previous merge`). Due → write `{0, h, oid}` and add the line
  `audit due (<reason>): run steward on kit <slug>`; not due → write `{n, h, oid}`. A missing file starts the
  count with this merge and is never due by the second reason.
- **K:** default 5; `~/.config/stack/audit-due.toml` `merges = <int >= 1>` overrides it; an invalid value →
  `warn` and the default.
- **Errors** (kit dir unreadable, state file unwritable or not valid JSON): `warn` with one finding; a corrupt state
  file is replaced by a fresh one, so the warn appears once.
- **Output:** one line per due kit, nothing otherwise. With one kit open, at most one line per merge.

### 4.8 Steward (shuymn/dotfiles, listed for the split only)

`agents/steward.md` gains the output of #28 (one `on-track — continue: <next step>` line; otherwise findings with
file:line evidence, about 5 lines, ending with the next step) and makes the kit's state file an explicit audit
target at any size (#28 comment). The hiden orchestration text starts steward in the background with only the kit
path when a due line appears. Not specified further here.

## 5. Decisions

### 5.1 Tool-use gate (#30 "To decide" 1)

Measured on Apple M4 Pro, macOS, `stack` built from `5f057a7`, warm cache, wall time of the whole process from a Go
harness, 4 representative payloads in rotation (`git status && git diff --stat`; `cd docs && gh pr create … --body-file
../README.md`; `Read docs/architecture.md`; `mise exec -- go test … | tail`), two runs:

| Measurement | n | p50 | p95 | p99 |
|---|---|---|---|---|
| `stack hook claude pre-tool-use` today (all five rules) | 600 | 3.65–3.69 ms | 4.91–5.04 ms | 5.47–5.51 ms |
| A trivial extension process (read stdin, print `{"status":"pass"}`) | 300 | 2.16–2.20 ms | 2.86–2.99 ms | 3.05–3.21 ms |
| Hook, then one trivial extension, sequential | 300 | 5.79–5.89 ms | 7.52–7.94 ms | 8.29–8.61 ms |
| Hook, then a second full hook process, sequential | 300 | 7.27–7.34 ms | 9.72–10.00 ms | 10.64–11.08 ms |

The issue's "about 10 ms" p95 is the two-process sequential figure on this machine; the single hook is about 5 ms.
Spawning one extension per call adds about 2.2 ms p50 / 3 ms p95 before it does any work.

| Option | Gains | Gives up | Reversibility | Pencil deny on by default |
|---|---|---|---|---|
| **(a)** core stage `tool-use` runs extensions | one registry (`[stages]`), `--replay` fixtures for guards | +3 ms p95 per extension per tool call, serial on the hottest path; the request must carry Claude Code's hook payload, binding the protocol to a harness schema that changes (fails S); duplicates Claude Code's own matcher, ordering and deny merging; the protocol's failure mode (`warn`) still allows on a broken guard, so nothing is safer | adding a stage is easy, removing a stage from the protocol is not | `[stages].tool-use = ["guard"]` default and the binary installed; a missing binary is silently skipped |
| **(b)** guards move to `stack-guard`, wired as its own `PreToolUse` hook (**chosen**) | core keeps one rule and loses `tdewolff/parse` and the private-term path; no new protocol surface; Claude Code runs the two hooks in parallel, so added latency is about max(core, guard) ≈ today's 5 ms p95, not the sum; the guard is replaceable by any other hook or settings rule | one more process per matching call (CPU, not wall time); guards are not visible to `stack run --dry-run`; the guard shares parser packages with core (§4.5) | full: the binary can be folded back, or a `tool-use` stage added later without breaking anything | `stack-guard` defaults `deny_pencil_generate = true` with no file or a broken one (§4.6); the owner's settings wire it in the same dotfiles PR that installs it, and that PR's settings test asserts the entry (§7) |
| **(c)** guards stay built in until a second consumer appears | zero cost, zero migration | fails R and S for four rules; contradicts #30's acceptance "shall run no content check" with no extension installed | trivially reversible | unchanged |

(b) is chosen: it satisfies #30 at no measurable latency cost and does not add a stage whose contract would depend
on one harness. (a) becomes worth reconsidering only when a second harness needs the same guards through one
interface.

### 5.2 Post-merge stage name and the audit-due contract

- **Name `merged`.** It names the position (after the verified merge) the way `entry` and `outbound` do; it is the
  name #28 suggests. Contract §2, §4.2.
- **State file** `<kit>/audit-due.json` inside each kit (§4.7): the signal belongs to the kit, and the kit dir is
  already the extension-visible place for kit state (`kits[].dir`).
- **K = 5**, the issue's default: over the 26 merges of the run #28 cites, that is five audits instead of one, each a
  background steward of about one line when on track.
- **"state.md not updated" by content hash, not mtime.** A hash compares what the steward cares about (content);
  mtime changes on saves without changes, on checkouts and syncs, and has filesystem-dependent resolution, so tests
  would need sleeps. The files are small; hashing costs nothing measurable. The comparison is with the previous
  counted merge, because the orchestrator updates the state after each merge (`dotfiles:home/dot_claude/skills/hiden/references/attention.md:15`):
  two merges with no update between them are the drift signal.

### 5.3 `budget status` and the byte threshold

Removed. Without the byte note `stack budget status` lists kit sizes and per-role item counts
(`internal/budget/budget.go:129-200`); `kit open` already renders the orchestrator items, and `items.json` is plain
JSON. No program calls it (§6). The renderer drops the size note at both call sites (`kit open` and compact,
`internal/kit/render.go:123-126`). The compaction budget (`docs/design/phase2-core.md` §4.2 step 5) stays: it is
where essence is actually lost, and it already marks what it omits.

### 5.4 Migration order

§7. The invariant: from the dotfiles PR that wires `stack-guard` until the core removal, both hooks deny the moved
rules (duplicate denies are harmless); the core copy goes only after the guard is verified live (P7, P8).

## 6. Callers

| Caller | Today | After |
|---|---|---|
| `extensions/improve/publish.go:198-220` (step 12.5) | `stack check public <base>..<oid> --title-file --body-file --branch`; exit 1 → `proposal:public-check`, other failure → `failed:check` | `stack-public check …`, same arguments and exit codes; a missing binary is a start failure → `failed:check` (fail closed). `stack-public` joins the tools improve needs (`docs/design/phase3-cycle.md` §3.1) |
| `dotfiles:home/dot_claude/skills/hiden/references/commit-pr.md:7` | `stack check public <base>..<head> --title-file … --body-file … --branch …` before each push and PR edit | `stack-public check …`, same arguments |
| `dotfiles:scripts/stack-private-terms:5` | docstring: writes the terms `stack check public` rejects | wording: `stack-public`; file path unchanged (`private_terms` default) |
| `dotfiles:home/dot_claude/skills/hiden/references/attention.md:5` | says per-role counts are shown by `stack budget status` (display only) | sentence removed; the items stay in `attention/items.json` |
| `stack pr merge --subject/--body` callers | refused with exit 2 on a local path or private term | refused with exit 1 by `stack-public` at `outbound`, before any `gh` call; not checked when `stack-public` is not installed |
| `README.md:10-13`, `AGENTS.md:7`, `cmd/stack/main_test.go:75` | describe or test `check public`, `budget status`, the five-rule guard | updated in the PRs that move them |

No script, hook or statusline calls `stack budget status` (`grep` of this repository and `shuymn/dotfiles`); the
dotfiles hiden text already says to write only what later decisions depend on, without a byte number
(`dotfiles` commit `ac1392b9`), so #28's dotfiles wording change is done. `dotfiles:home/dot_claude/attention/create_budget.json`
belongs to the legacy `attention-budget.py`, retired by Phase 3, and is not a caller.

## 7. PR Split

`shuymn/stack` PRs pass `scripts/test` and the Windows CI job; dotfiles PRs pass their own tests.

1. **docs: design core stages and extension boundary** — this note, `docs/architecture.md` v11.
2. **feat: give each stage one semantic** — block demotion at `entry`/`cycle` (§2), `kits` request field (§4.1),
   entry at compaction (§4.3). Done when C1–C4 pass.
3. **feat: add the merged stage** — `merged` (stage, 10 s, `merge` field, `stack run merged`), `stack pr merge`
   runs it (§4.2 step 3), default `[stages].merged = ["audit-due"]`. Done when C5–C7 pass.
4. **feat(ext): add audit-due** — `extensions/auditdue`, README install line. Done when C8–C11 pass.
5. dotfiles **docs: audit on the merged stage's due signal** — steward output (§4.8), orchestrator starts steward in
   the background on a due line, `attention.md:5` sentence removed, `stack-audit-due` added to the install command.
   After PR 4.
6. **refactor: drop the state.md byte threshold and budget status** — `StateLimit`, `internal/budget`, the
   command, docs. After PR 5 (its last textual caller). Done when C12 passes.
7. **feat(ext): add stack-public** — `extensions/public` (both modes), `stack run outbound --title-file/--body-file/--branch`,
   improve switched to `stack-public check`, default `[stages].outbound = ["public", "jev-semantic"]`.
   `publictext`/`checkpub` copied into `extensions/internal/`; core keeps
   its copies until PR 10. Done when C13–C15 pass.
8. **feat(ext): add stack-guard** — `extensions/guard`, `safety.toml` reader, depguard allow list (§4.5). The core
   hook is unchanged, so both deny. Done when C16–C17 pass.
9. dotfiles **feat(claude): wire stack-guard and stack-public** — install both binaries, add the `stack-guard`
   `PreToolUse` entry (settings test asserts it), `commit-pr.md` and `stack-private-terms` to `stack-public`. After
   PRs 7–8. Then `make apply` and the live checks P7–P9 on the owner's machine.
10. **refactor!: keep only the raw-merge deny in the core hook** — remove the four rules, `check public`,
    `[safety]`, core `publictext`/`checkpub`, `tdewolff/parse`; `pr merge --subject/--body` goes through
    `outbound` (§4.2 step 2). Only after PR 9's live checks pass. Done when C18–C20 pass.
11. dotfiles **chore(claude): narrow the stack hook matcher to Bash** — after PR 10 is installed. Optional: the core
    hook allows every non-Bash call anyway.

Order: 1 → 2 → 3 → 4 → 5 → 6, and 7 → 8 → 9 → 10 → 11 (the second chain may start after 2). Guards stay active
throughout: before PR 9 only the core hook has them, between PR 9 and PR 10 both do, after PR 10 only `stack-guard`
does, and PR 10 is not installed before PR 9 is applied.

## 8. Alternatives

### 8.1 Tool-use gate

§5.1. Also rejected: running `stack-guard` from the core hook as a sub-process (it is (a) without the registry: the
serial cost and no gain over a second hook entry).

### 8.2 How direct callers reach the public check

- `stack run outbound --ext public …` and reading its exit code: rejected. A missing extension is skipped and the
  stage passes, and a timeout becomes `warn` with exit 0, so improve's and the skill's fail-closed check would open
  silently.
- A core `stack check public` alias that runs the extension: rejected; the core would still name a content check.
- A `--strict` flag on `stack run` that fails on `skipped`/`warn`: possible, but a core addition for one caller;
  the CLI mode needs none.

### 8.3 The merge message check

- Dropping it (callers run `stack-public` on the texts first): rejected; it loses behaviour silently, against #30
  acceptance 5.
- Running `stack-public` directly from `pr merge`: rejected; the core would know a named extension.

### 8.4 Audit-due detection

- mtime instead of a hash: §5.2.
- Comparing with the state at the last due signal: rejected; after one update the rule would stay silent for the
  rest of the K window, however many merges followed without an update.

## 9. Acceptance Criteria

### 9.1 Purpose layer

| ID | EARS criterion | Source | Observable result / failure | Verify on the real thing |
|---|---|---|---|---|
| P1 | The architecture shall define every lifecycle stage in the core, each with one semantic, position, precedence, deadline and failure mode. | #30 acceptance 1 | `docs/architecture.md` §4 stage table | read §4 against §2 here |
| P2 | Extensions shall attach only to stages the core defines. | #30 acceptance 2 | an unknown stage is a usage or config error | `stack run tool-use` → exit 2 |
| P3 | When no extension is installed, the core shall provide every stage, session identity, kits and their restore, the checked merge and the raw-merge deny, and shall run no content check. | #30 acceptance 4 | no deny for a secret `Read`; deny for `gh pr merge`; `stack check public` unknown | `PATH` without `stack-*`, empty `HOME`: pipe a `Read .dev.vars` and a `gh pr merge 1` payload to the core hook; `stack run outbound` → pass with `skipped` |
| P4 | When `stack pr merge` verifies a merge and no `merged` extension is installed, its output and exit code shall be unchanged. | #28 acceptance 1 | stdout exactly `MERGED <H> <oid>`; exit 0 | merge a scratch-repo PR with `stack-audit-due` off `PATH` |
| P5 | While no kit is open in the session, the audit-due extension shall print nothing. | #28 acceptance 2 | stdout is only the `MERGED` line | ordinary session, scratch PR merge with the extension installed |
| P6 | When a kit is open and K merges have happened since the last due signal, or a merge happened without a kit state update since the previous merge, the extension shall print exactly one due line; otherwise nothing. | #28 acceptance 3 | `audit due (…): run steward on kit <slug>` after `MERGED` | `stack kit open demo`, merge two scratch PRs without touching `state.md` → line after the second; edit, merge → nothing |
| P7 | When `stack-guard` is wired, the secrets, gh-publish, rmroot and Pencil rules shall deny as before. | #30 acceptance 5 | the deny reasons of `docs/design/phase2-core.md` P2, P4, P14, P18 | live session after dotfiles PR 9: the phase 2 live checks of those rows |
| P8 | While `safety.toml` is absent or invalid, `stack-guard` shall deny a Pencil `Generate(` call. | owner decision (Pencil) | deny | live Pencil session with no file, then with a broken file |
| P9 | When `stack-public check` gets the inputs of a `stack check public` run, it shall produce the same stderr lines and exit code. | #30 acceptance 5, To decide 2 | identical output | the phase 2 P12 seeded repo with both binaries before PR 10 |
| P10 | When `stack-public` is installed and `pr merge` gets `--subject` or `--body` with a local path or private term, it shall refuse before any `gh` call. | #30 acceptance 5 | exit 1, hit named | scratch PR, `--body` with a home path |
| P11 | The kit context shall carry no size note, and compaction shall keep its 9,500-char budget. | #28 comment | no `bytes, over` text | `kit open` on a kit with a 10 kB `state.md`; `/compact` |
| P12 | When a session with open kits compacts, it shall restore the entry contexts after the kits within the budget. | #30 stage table | entry block after compaction | live: a stub entry extension, `kit open demo`, `/compact` |
| P13 | Where a stage cannot block, a `block` response shall be reported as `warn` and `stack run` shall exit 0. | #30 stage table | `"status":"warn"` with the finding | stub `stack-demo` answering block, `stack run cycle --ext demo` |

### 9.2 Plan layer

| ID | EARS claim | Test | Required assertions |
|---|---|---|---|
| C1 | When an extension answers `block` at `entry`, `merged` or `cycle`, `ext` shall record `warn` with the stage finding. | `ext.TestBlockDemotion` | per stage; `outbound` keeps `block`; finding text |
| C2 | `stack run` shall exit 1 on `block` only at `outbound`. | `cli.TestRunBlockExit` | four stages; replay `expect` compared after demotion |
| C3 | When `STACK_SESSION_ID` is valid, every live request shall carry `kits` with the existing open kits in `.kits` order; otherwise `[]`. | `cli.TestRequestKits` | unset id → `[]`; a listed kit whose dir is gone is left out; `.kits` lock held past 1 s → `[]` plus the stderr note |
| C4 | When compact restores at least one kit, session-start shall run the entry stage and place each result after the boards, cut or omitted within the budget. | `session.TestCompactEntry` (replaces the "no extension" assertion of C19 of `docs/design/phase2-core.md`) | fake extension invoked once with `kits`; no kits → not invoked; long context cut with marker; total ≤ 9,500 UTF-16 units |
| C5 | When `pr merge` verifies a merge, it shall print the `MERGED` line before running `merged` and exit 0 whatever the results. | `merge.TestMergedStage` (fake gh, fake extension) | stdout order; warn marker and context after the line; extension `block` → shown as warn, exit 0; direct and `--auto` paths |
| C6 | If `pr merge` refuses or cannot confirm the merge, or is cancelled, it shall not run `merged`. | `merge.TestMergedStageNotRun` | refusal, unknown, cancel: the fake extension's log is empty |
| C7 | When no `merged` extension is installed, `pr merge` stdout shall be byte-identical to today. | `merge.TestMergedNoExtension` | exact bytes |
| C8 | While the request has no `merge` field or `kits` is empty, `stack-audit-due` shall print nothing and write nothing. | `auditdue.TestSilent` | no file created |
| C9 | When K merges have been counted since the last due line, `stack-audit-due` shall print one due line and reset the count. | `auditdue.TestK` | K from default and from `audit-due.toml`; invalid K → warn and default |
| C10 | When a merge follows the previous counted merge with an unchanged kit state hash, `stack-audit-due` shall print one due line; the first merge of a kit shall not. | `auditdue.TestStateUnchanged` | `state.md` only, `NORTH.md` only, both; changed bytes → silent; same `oid` twice → counted once |
| C11 | If the state file is corrupt or unwritable, `stack-audit-due` shall answer `warn` with one finding. | `auditdue.TestErrors` | corrupt → replaced, second run silent |
| C12 | The kit renderer shall not add a size note. | `kit.TestRender` | a state file over 3,000 bytes renders verbatim; `stack budget status` → unknown command, exit 2 |
| C13 | `stack-public check` shall match the `stack check public` contract. | `public.TestCheck*` (the moved `checkpub` and `cli` check-public tables, real git) | every existing case, same stderr and exit codes |
| C14 | In stage mode `stack-public` shall answer `block` on a hit or its own error and `pass` otherwise. | `public.TestStage` | hit in range, `title`, `body`, `branch`; unreadable terms → block; empty range scans texts only |
| C15 | `stack run outbound --title-file/--body-file/--branch` shall send `title`, `body`, `branch`. | `cli.TestRunOutboundTexts` | fake extension's stdin log; non-UTF-8 file → exit 2 |
| C16 | `stack-guard hook claude pre-tool-use` shall decide the four moved rules as the core hook does. | `guard` goldens moved to `extensions/guard/testdata/` | every existing golden except raw-merge passes unchanged |
| C17 | While `safety.toml` is absent or invalid, `stack-guard` shall apply the v10 `[safety]` defaults. | `guard.TestSafetyConfig` | Pencil deny and secrets deny under no file and under a broken file |
| C18 | The core hook shall deny raw merges and allow everything else. | `guard.TestRawMerge` (kept), `cli.TestPreToolUseCoreOnly` | a `Read .dev.vars`, `rm -rf /` and Pencil `Generate()` payload → no output |
| C19 | When `--subject` or `--body` is given, `pr merge` shall run `outbound` before any `gh` call and refuse on `block`. | `merge.TestOutboundMessage` | gh argv log empty on block; warn continues; no flag → stage not run |
| C20 | Core shall not import or name any content check. | `depguard` + `cli.TestCommands` | `check public` absent from usage; `internal/publictext` gone; `go list -deps ./cmd/stack` has no `tdewolff` |

## 10. Owner Questions

None. Every public behaviour change here follows from #28 and #30: the `merged` stage and its output, `check
public` and the guards becoming extensions (with the CLI name `stack-public check` and the `stack-guard` hook),
`budget status` and the byte note removed, `block` demoted at stages that cannot block, and entry contexts restored
at compaction (the #30 stage table lists "compaction restore" under `entry`). One side effect to note: a local path
in `pr merge --subject/--body` now exits 1 (a stage block) instead of 2.
