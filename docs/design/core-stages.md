# Core Stages Design: Stages and the Extension Boundary

Design for issues #30 (keep only stages and the extension contract in the core) and #28 (signal a due audit after
a verified merge), applied to `docs/architecture.md` v10 and the code at `5f057a7`. It changes the approved design,
so `docs/architecture.md` moves to v11 with it, and it overrides the items of `docs/design/phase2-core.md` and
`docs/design/phase3-cycle.md` listed in §4.10. Code is cited as `path:line` at that commit; skill text in
`shuymn/dotfiles` as `dotfiles:<path>:<line>`; the decision rule is the "Quick Decision Guide" of the
`extensible-by-design` skill (`~/.claude/skills/extensible-by-design/SKILL.md`).

## 0. Principles

1. **The core is the lifecycle and the contract.** A built-in behaviour stays in core only when all three hold:
   (R) it is required for every session with no extension installed, (U) it is wrong or unsafe if left to
   extensions, (S) its contract is stable. Everything else is an extension, or external, and is listed in the
   negative-space table (§3) with its reason and path. One exception: existing CLI output that describes a core
   behaviour stays as compatibility (`gh_model`, §1).
2. **Each stage has one semantic.** A stage is a gate (can block) or an injection/observation point (cannot). The
   core enforces that, so an extension author reads the semantic from the stage table and never from the caller.
3. **Extensions attach only to stages the core defines.** They never add a stage. A new stage is a core change and
   follows the protocol's evolution rule (`docs/architecture.md` §3 "Evolution").
4. **Nothing the owner uses today loses behaviour between PRs.** Checks move by adding the extension first, wiring
   it, verifying it live, and only then removing the core copy (§7). The deliberate exceptions are listed in §4.9.
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
| Restore contract: what compaction restores (the session's kits, a detected worker's body and brief, the handover, the kits' boards), in which order, within the 9,500-char budget, with an omission marker for every part that does not fit (`internal/session/session.go:115-170`, `internal/session/budget.go`, `internal/kit/render.go:90-216`) | conditionally: a no-op without a kit or worker, required as soon as one exists, since compaction is where essence is lost (#28 comment) | yes: compaction runs inside a hook with a hard output limit; only the core sees the whole budget | yes | **Core** |
| Replaceable restore content: which state files a kit has (`NORTH.md`, `state.md`, `internal/kit/render.go:37`), the worker marker, agent types and body (`[worker]`), the handover files per environment (`[handover]`), each kit's board command | no: each is the owner's choice | no | no: varies per user | **Config and kit files** read by the core contract; the built-in `-worktrees/agent-*` worker signal (`internal/session/session.go:214-260`) stays as the one non-configurable detection rule |
| 3,000-byte `state.md` note (`internal/kit/render.go:24-25,123-126`) | no: a size number is not what diverts attention (#28 comment) | no | no: the threshold is policy | **Removed** (§5.3) |
| Extension discovery, protocol, deadlines, aggregation (`internal/ext/ext.go:27-418`, `internal/ext/request.go`) | yes | yes: it is the contract | yes: evolution rule | **Core** |
| `stack run <stage>` (`internal/cli/run.go:31-80`) | yes: the stage runner | yes | yes | **Core** |
| Entry stage after `kit open` (`internal/cli/run.go:216-238`) | yes: the stage must fire | yes | yes | **Core** |
| `stack pr merge` checked head, checks, `merge-check`, read-back (`internal/merge/merge.go:157-232`) | yes: the only merge path, so `merged` fires (#30) | yes: a merge is irreversible and the stage would be skipped otherwise | yes: v10 contract; repository policy already plugs in through `.claude/merge-check` | **Core** |
| Git environment cleanup `DropRepoVars` (`internal/checkpub/checkpub.go:219-229`, called by `internal/cli/prmerge.go:41` and `internal/cli/run.go:180`) and input file resolution (`readInputFile`, `internal/cli/checkpublic.go:71-83`; `ReadUTF8`, `internal/checkpub/checkpub.go:113-122`) | yes: `pr merge` and every stage request find the repository from the cwd, never from a git hook's `GIT_DIR`; `stack run outbound --title-file` reads files | yes | yes | **Core**, moved out of `checkpub` into core packages before `checkpub` leaves (§7 PR 7) |
| Raw `gh pr merge` deny (`internal/guard/ghmerge.go:20-102`, order at `internal/guard/guard.go:68-81`) | yes: without it `merged` can be bypassed and its extensions silently skipped (#30) | yes | yes: the rule is fixed; the gh model it parses is versioned (`internal/ghargs/table.go:3`) | **Core** |
| `stack --version --json` `gh_model` (`internal/cli/cli.go:197-210`) | no: a diagnostic, not an input of the deny | no | yes | **Core, kept CLI compatibility**: it describes the core deny's gh model, and `stack-observe` consumes it (`docs/design/phase3-cycle.md` §3.2); removing it would break that documented contract for no gain |
| Secrets read deny (`internal/guard/secrets.go:40-321`) | no: optional; Claude Code permission rules and modes cover reads | no: a guard that is missing fails open today too (`hookio` fail-open) | no: the list is per user (`[safety].secrets`) | **Bundled hook** (`stack-guard`) |
| Local paths in `gh` publish inputs (`internal/guard/ghpublish.go:13-49`) | no | no | no: shares the term and path policy with `check public` | **Bundled hook** (`stack-guard`) |
| Catastrophic `rm` (`internal/guard/rmroot.go:15-83`) | no | no: permission modes and sandboxing cover it | yes, but R fails | **Bundled hook** (`stack-guard`) |
| Pencil Generate deny (`internal/guard/pencil.go:31-173`) | no: one owner's tool | no | no: owner decision, tool-specific | **Bundled hook** (`stack-guard`) |
| `stack check public` (`internal/checkpub/checkpub.go:126-383`, `internal/cli/checkpublic.go:17-83`) | no: optional; secret scanners and hooks are alternatives | no | no: the private-term policy varies | **Extension** (`stack-public`) |
| Local path / private term check of `pr merge --subject/--body` (`internal/merge/merge.go:133-150`) | no | no | no: same policy as `check public` | **Extension**: the outbound extensions listed in `[merge].message_checks`, run by `pr merge` (§4.2) |
| `[safety]` config table (`internal/config/config.go:52-57,80-84,160-163`) | no: only the moved checks read it | no | no | **Extension config** (`~/.config/stack/safety.toml`, §4.6) |
| `stack budget status` (`internal/budget/budget.go:60-262`) | no: display only, no program caller (§6) | no | no | **Removed** (§5.3) |

The foundations `paths` and `hookio` parse inputs and hold no policy. `shell` and `ghargs` are not policy-free:
`shell` derives the secret-handoff facts the secrets rule judges (`internal/shell/shell.go:9`), and `ghargs`
selects which `gh` inputs are published (`internal/ghargs/ghargs.go:100`). They stay in core because the raw-merge
deny needs `shell`, `ghargs` and `gqlparser`; `stack-guard` uses the same packages as a narrow in-repo dependency
(§4.5).

## 2. Stage Table

The core defines these four stages. Precedence within a stage is the configured order (`[stages].<stage>`, for
`pr merge`'s message checks `[merge].message_checks`, or the repeatable `--ext` of `stack run`); extensions run
sequentially in that order under one stage deadline; results aggregate `block > warn > pass`. A stage may have no
built-in behaviour; it still runs its extensions.

| Stage | Fires (position) | Semantic | Can block | Deadline | Failure of an extension |
|---|---|---|---|---|---|
| `entry` | after `stack kit open` printed the kit; on SessionStart `compact` after the restored kits, when the session has at least one kit and the hook has time left for it (§4.3); `stack run entry` | inject | no | 10 s; at compaction also bounded by the hook's cut-off less the pipe-reaping allowance (§4.3) | `warn` marker after the kit; kit output unaffected |
| `outbound` | `stack run outbound` (push hooks, CI, skills) with `[stages].outbound`; `stack pr merge` before any `gh` call when `--subject` or `--body` is given, with `[merge].message_checks` | gate | yes: `stack run` exits 1, `pr merge` refuses | 120 s | `warn`, does not block; at `pr merge` the merge is refused instead (§4.2) |
| `merged` | `stack pr merge` at its one verified-success point (read-back of `MERGED` on the checked head with a full merge OID, on the direct path and on every `--auto` success, including the final read-back after auto-merge was disabled), unless the run was cancelled; `stack run merged` | observe / inject | no: the merge is irreversible | 10 s from the stage's start; run by `pr merge`, including request preparation and pipe reaping (§4.2); `stack run merged` prepares its request outside it | `warn` marker after the `MERGED` line; exit code unchanged |
| `cycle` | `launchd` → `stack run cycle` (macOS); `stack run cycle` anywhere | observe | no | 30 min | `warn` |

**Block at a stage that cannot block.** At `entry`, `merged` and `cycle` the core records a `block` response as a
result with `status` `warn`, the extension's `context` and findings kept, the synthetic finding
`{"extension":<name>,"error":"block is not allowed at stage <stage>"}` appended, and `error` set to that reason. A
demoted result is therefore an extension failure like any other: `kit open` shows it with a `warn` marker, and the
dotfiles `stack-cycle` wrapper, which accepts only results with an empty `.error`
(`dotfiles:scripts/stack-cycle:15-20`), rejects it, records `last-error` and keeps the previous `last.json`, as
it did for v10's exit 1. `stack run` exits 1 only at `outbound`, and `--replay` compares `expect` with the aggregate
after demotion. Today `stack run entry` and `stack run cycle` exit 1 on `block` (`internal/cli/run.go:75-77`) and a
fixture of those stages expecting `block` matches; this is a compatibility exception (§4.9).

**Guarantee that each stage fires.** `entry` fires on every `kit open` and on every compact that restores a kit
and still has the time of §4.3 left after the restore (otherwise its place shows `[entry skipped: deadline]`);
`merged` fires on every merge `stack pr merge` verifies, except after a cancel, and the raw-merge deny makes
`stack pr merge` the only merge path an agent session has within the guard's static scope
(`docs/design/phase2-core.md` §0.4, §6). Merges outside it (the GitHub UI, another machine, a `gh` alias) do not
fire `merged`; that is accepted, as the stage observes the agent's own work. `outbound` fires where its callers run
it; the core guarantees the `pr merge` call site only, and there only when `[merge].message_checks` lists an
extension. Within a firing stage, an extension not started by the stage's deadline is left unstarted with a `warn`.
`cycle` fires where the owner schedules it.

**Tool use is not a core stage.** Claude Code's `PreToolUse` hook list already orders, filters (matchers) and
combines (any deny wins) tool-use gates; §4.5 and §8.1 explain why the core does not duplicate it.

## 3. Intentionally Not in Core

This table is the extension contract's table of contents; `docs/architecture.md` §4 carries it.

| Capability | Why not core | Extension path |
|---|---|---|
| `check public` (local paths, private terms) | optional; secret scanners and hooks are alternatives; the term policy varies | `stack-public`: `outbound` extension, and `stack-public check` for direct callers |
| Local-path / private-term check of `pr merge --subject/--body` | same policy as `check public` | outbound extensions listed in `[merge].message_checks` (the owner lists `public`), run by `stack pr merge` |
| Secrets read deny, local paths in `gh` publish inputs, catastrophic `rm` guard, Pencil Generate deny | optional; Claude Code settings, permission modes and scanners cover them; the lists are per user | `stack-guard hook claude pre-tool-use`, a bundled Claude Code hook of its own (not a stage extension) |
| `[safety]` settings | read only by the checks above | `~/.config/stack/safety.toml`, read by `stack-guard` and `stack-public` |
| `budget status` and the `state.md` byte threshold | display only, no program caller; a byte count is not what diverts attention | dropped; the steward judges `state.md` by content (#28); an extension if a caller appears |
| Audit-due signal | its rule (K merges, kit state not updated) is policy | `stack-audit-due`: `merged` extension |
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
- **`kits`** (`entry` and `merged` live requests only, the stages whose known consumers use it: entry contexts per
  kit and `stack-audit-due`): `[{"slug": "<slug>", "dir": "<absolute kit dir>"}]`, the kits listed in `<id>.kits`
  for a valid `STACK_SESSION_ID` whose directory exists, in `.kits` order; `[]` without a session id. Read under
  the `.kits` lock with a 1 s timeout (as compact, `docs/design/phase2-core.md` §4.2 step 2); on a timeout it is
  `[]` and core writes `stack: <id>.kits locked; kits not sent` to stderr. At compaction the list is the one the
  restore already read (§4.3). Extensions get session identity and kits from core and never parse `config.toml` or
  the sessions directory themselves. `outbound` and `cycle` requests carry no `kits`.
- **`merge`** (`merged` live requests from `pr merge`): `{"pr": <int>, "head": "<full H>", "oid": "<merge OID>"}`.
  `stack run merged` without `--replay` sends no `merge` field.
- **`title`, `body`, `branch`** (`outbound` live requests): strings, `""` when absent. `stack run outbound` gains
  `--title-file F`, `--body-file F` and `--branch B`. Files are UTF-8 and resolved as `check public` resolves them
  today (`~`, and Git Bash `/c/…` forms on Windows; the resolver stays in core, §1); an unresolvable, unreadable or
  non-UTF-8 file exits 2. An explicitly empty value (`--branch=`, an empty file) sends `""`, like an absent flag.
  The flags are usage errors (exit 2) at any other stage and together with `--replay`, which sends the fixture's
  request as is (as `--range` and `--prompt-file` are today, `internal/cli/run.go:36`). `stack pr merge` sends
  `--subject` as `title`, `--body` as `body`, `branch` `""` and `range` `""`.
- **Deadline:** `merged` 10 s.
- **Block demotion:** §2.

### 4.2 `stack pr merge` and the `merged` stage

1. Steps 1–8 unchanged (`docs/design/phase2-core.md` §4.4), except that step 1 no longer scans `--subject`/`--body`
   and no longer reads the private terms. `pr merge` still loads and validates `config.toml` after its arguments
   and before any `gh` call, as v10 does on its way to the terms (`internal/cli/checkpublic.go:62`): a broken
   `config.toml` exits 2 before `gh` with or without message flags, and that one load supplies
   `[merge].message_checks` and `[stages].merged`. A broken private-terms file no longer refuses a merge without
   message flags (v10 exited 2, `internal/cli/prmerge.go:68-71`); with message flags the terms are read only by a
   message check that uses them (`stack-public` answers `block` on an unreadable file, §4.4).
2. **Message checks, fail-closed.** `[merge].message_checks` (core config, default `[]`) lists the outbound
   extensions the owner requires for a merge message. When `--subject` or `--body` is given (an empty value
   included) and the list is not empty, after the repository top level is found (`internal/merge/merge.go:165`) and
   before the first `gh` call, `pr merge` runs those extensions, in that order, as an `outbound` stage run: stage
   `outbound`, its 120 s deadline, the request of §4.1 with `repo` that top level. It never reads
   `[stages].outbound`, the selection for push hooks, CI and skills, where a missing or failing extension only warns
   and an optional `jev-semantic` may sit. Every listed extension is required, and the merge is irreversible, so
   `pr merge` exits 1 before any `gh` call when:
   - the aggregate is `block`: stderr `stack pr merge: outbound blocked: <name>: <context>` per blocking result;
   - a listed extension could not answer: it is missing (listed under `skipped`), not runnable, failed to start,
     timed out, was left unstarted at the deadline, or answered invalid or oversized output (any result with a
     non-empty `error`): stderr `stack pr merge: outbound could not check the message: <name>: <reason>`.

   A valid `warn` or `pass` from each listed extension continues, each warn marker on stderr. With the default `[]`
   nothing runs and the merge continues, so the checked merge works with zero extensions installed and the default
   configuration (#30 acceptance 4); a merge without message flags never runs the checks. The owner's dotfiles
   wiring (§7 PR 9) writes `message_checks = ["public"]`, which keeps v10's local-path and private-term check of
   every merge message for the owner (#30 acceptance 5). v10 checked merge messages by default; v11 checks them
   only when configured, as the consequence of acceptance 4 (§4.9).
3. **After a verified merge.** `reportMerged` (`internal/merge/merge.go:236-251`) is the one success point: the
   direct path reaches it from its read-back (`internal/merge/merge.go:230`), and the `--auto` path from
   `judgeMerged` (`internal/merge/merge.go:389-400`) both while polling and in the final read-back after a stop
   (`internal/merge/merge.go:299-317`). There `pr merge` prints `MERGED <H> <oid>` first, exactly as today, then
   runs `[stages].merged`, unless the context is already cancelled (SIGINT/SIGTERM: the owner asked to stop; the
   merge result stands and `merged` is not run). A stop by the `--auto` wait deadline is not a cancel: a success
   found by the final read-back after it runs `merged`. The request reuses what `pr merge` verified: `repo` is the
   top level it found, `merge` holds the PR, the checked head and the read-back's merge OID, and no further git or
   `gh` call is made. `pr merge` first resolves the selected `merged` extensions on `PATH`; when the selection is empty or every
   name is missing (not found on `PATH`) it does nothing more (no `.kits` read, no stage run), so stdout, stderr
   and the exit code are byte-identical to today. When a name is found but not runnable (no execute permission, a
   lookup error) and none is runnable, it prints that name's `warn` marker after the `MERGED` line without reading
   `.kits` and exits 0, as the runner reports it at every stage (`internal/ext/ext.go:223`). Otherwise the stage's end is 10 s after it starts and covers the `.kits` read, the request encoding,
   every extension and the reaping of their pipes: the runner gets a deadline that is that end less its pipe-reaping
   allowance (`defaultWaitDelay`, 2 s, `internal/proc/proc.go:28`), so an extension that runs to the deadline and
   leaves a pipe open to a grandchild is still reaped by the end. An extension not started by the deadline is left
   unstarted with a `warn`. For each result that is not `pass` or has a non-empty `context`, print to stdout, after
   the `MERGED` line, the `warn` marker `[extension <name>: warn: <reason>]` when it warned, then its trimmed
   `context`. Exit 0 whatever the results, also when stdout is closed and those writes fail. The 10 s bounds the stage
   run (from the `.kits` read to the last reaped pipe); printing its results follows it and is not counted. The bound
   assumes that the caller reads `pr merge`'s stdout and stderr and that the session's `.kits` file is a regular
   file; the operating system's kill and wait latency (milliseconds) is outside it. A manual
   `stack run merged` keeps `stack run`'s request preparation (git discovery, up to 30 s,
   `internal/ext/request.go:69`) outside the 10 s, as every `stack run` stage does.
4. Refusals and "result unknown" outcomes never run `merged`.

### 4.3 `entry` at compaction

Step 2 of `docs/design/phase2-core.md` §4.2 gains a last step: when at least one kit was restored, run
`[stages].entry` once.

- **Request:** reuses the restore's inputs: `STACK_SESSION_ID` set in the extensions' environment to the payload's
  `session_id` (the line appended to `CLAUDE_ENV_FILE` does not reach this hook's own children, and an inherited
  value may name another session), `kits` the list the restore read under the `.kits` lock (no second read), `repo`,
  `prompt` and `range` `""`. No git call is made, so a slow repository cannot delay it.
- **Time:** the hook has a cut-off 55 s after it started (Claude Code's `timeout: 60` less 5 s to format, encode and
  write the output, which must finish by the host's 60 s). The cut-off is the point after which no further work starts and the hook writes the restore built
  so far: it bounds the entry stage, not the restore's own file reads and board commands, which keep their v10
  bounds (`docs/design/phase2-core.md` §4.2). Once the restore is built, the stage's deadline is the earlier of 10 s
  from its start and the cut-off, less the runner's pipe-reaping allowance (2 s, `internal/proc/proc.go:28`), so an
  extension that runs to that deadline and leaves a pipe open to a grandchild is reaped by the cut-off (the 1 s
  minimum below is measured on that deadline, after the allowance is subtracted). The stage
  runs only when that deadline is at least 1 s away; the deadline covers request encoding and every extension, and
  an extension not started by it is left unstarted with a `warn`. When the stage does not run, its place shows
  `[entry skipped: deadline]` if that fits.
- **Restore first:** the rules, brief, kits, boards and handover are placed before the stage starts and are never
  changed by it. Whatever the extensions do (hang, crash, flood stdout, leave a pipe open), the hook writes the
  already-built restore, plus what entry added: no work starts after the cut-off, and the output is written before
  the host's 60 s.
- **Placement:** each result that `kit open` would show becomes one piece after the boards, in stage order, capped
  at 4,500 chars like a kit block and cut with `[...cut N chars]` on a code-point boundary, or replaced by
  `[entry context omitted: context limit: <name>]` when the cut piece does not fit. Entry contexts reserve
  nothing: they come back only when the kits, brief and handover left room. When not even that marker fits, the
  result leaves no text in the context and the hook writes `stack: entry context of <name> dropped: context limit`
  to stderr; this happens only when the restore already filled the budget, so the restore itself is intact.

This makes compaction return what `kit open` showed, which v10 did not (C19 of `docs/design/phase2-core.md`
asserted that no extension runs; only that assertion changes, §4.10).

### 4.4 `stack-public` (outbound extension)

`extensions/public`, binary `stack-public`. Two modes:

- **Stage mode** (no arguments, the protocol): scans `range` (when non-empty), `title`, `body` and `branch` exactly
  as `stack check public` does today (`docs/design/phase2-core.md` §4.5). Hits → `block`, `context` the hit lines
  (`check public: <where>: …`), one finding per hit. The response must stay under the protocol's 1 MiB stdout limit,
  or core would record it as an oversized `warn` and `stack run outbound` would exit 0 on a hit
  (`internal/ext/ext.go:262-273`). So stage mode caps what it writes: `context` and findings carry the hits in scan
  order while the encoded response stays under 512 KiB, each hit line itself cut to 1,000 bytes on a UTF-8
  boundary, and the context ends with the trailer `check public: N more hits` when hits were left out. With at
  least one hit it always answers a valid `block`, with at least one hit line. Its own errors (a broken
  `safety.toml`, unreadable or non-UTF-8 private terms, a git failure, a range git cannot resolve) → `block` with
  the error as context, under the same cap: it fails closed by its own choice.
  Stage mode stops its own git work 100 s after it starts and then answers `block` with the hits found so far, so
  the answer reaches core before the 120 s stage deadline when `public` runs first (the default order). That limit
  bounds git; `safety.toml` and the private-terms file are the owner's files on local disk and are read without one.
- **CLI mode** `stack-public check <base>..<head> [--title-file F] [--body-file F] [--branch B]`: the same flags,
  stdout, stderr lines and exit codes (0 clean, 1 hits, 2 usage, input, config or git error) as `stack check public`
  today, every hit printed with no cap. Direct callers use this mode, so a missing binary fails closed (command not
  found) instead of being skipped as a stage extension would be.

`internal/publictext` and `internal/checkpub` move to `extensions/internal/` (Go's `internal` rule keeps them
private to `extensions/`); `stack-guard` and `stack-public` share `publictext`. The moved `checkpub` keeps its own
`DropRepoVars` and file reader for its git calls and CLI mode; the core's copies stay in core (§1).

### 4.5 `stack-guard` (bundled hook)

`extensions/guard`, binary `stack-guard`, run by Claude Code as `stack-guard hook claude pre-tool-use`, wired in the
owner's settings as its own `PreToolUse` entry with matcher `Bash|Read|NotebookRead|Grep|Glob|mcp__pencil__execute`,
exec form, `timeout: 5`, `onFailure: "continue"`. It is a hook bundled with this repository, not a stage
extension: invoked without arguments it prints usage and exits 2, so a `guard` name put in `[stages]` by mistake
shows up as a `warn`. It holds the secrets, gh-publish, rmroot and pencil rules of `docs/design/phase2-core.md`
§4.3 unchanged in what they deny, in that order, fails open like the core hook (`hookio`), and reads
`~/.config/stack/safety.toml` (§4.6). Its deny reasons start with `stack-guard (<rule>): `, distinct from the core
hook's `stack guard (raw-merge): `, so a live session shows which hook denied.

The core hook keeps the raw-merge rule only; Claude Code runs both hooks of a matching event in parallel and
denies when either denies. In v10 one Bash input matching several rules got the first deny in the order secrets →
gh-publish → raw-merge → rmroot (`internal/guard/guard.go:68-81`). With two hooks, an input matching raw-merge and a
moved rule is denied by both, and the reason Claude Code shows may differ from v10's; the input stays denied
(§4.9).

Import boundary: `stack-guard` imports `internal/shell`, `internal/ghargs`, `internal/paths` and `internal/hookio`,
a narrow dependency inside this repository, not a public API. `.golangci.yaml` `depguard` gains an allow list for
`extensions/guard/**` (non-test) naming exactly those four; every other extension keeps the existing deny of
`github.com/shuymn/stack/internal` ("protocol and documented CLI only", `.golangci.yaml:26-34`). Copying the
1k-line shell and gh models would let the raw-merge deny and the guard drift apart on the same gh version; moving
them to a public `pkg/` tree would rename every core import for the same coupling.

### 4.6 `~/.config/stack/safety.toml`

The v10 `[safety]` table as its own top-level file read by both safety extensions: the keys `secrets`,
`private_terms` and `deny_pencil_generate`, unchanged in meaning and defaults (`internal/config/config.go:80-84`:
the T3 secrets directory and `.dev.vars`; `~/.config/stack/private-terms`; `true`).

Missing file or key → the default. A broken file → the default rules apply, as the core hook does today
(`internal/config/config.go:209-211`); custom entries in the file are not kept until it is fixed. `stack-guard`
then denies by the defaults; `stack-public check` exits 2, as `check public` does on a broken config, and
`stack-public` in stage mode answers `block`. Core drops `[safety]`; a leftover `[safety]` table in `config.toml`
gets the existing unknown-key warning and changes nothing. The owner's effective `[safety]` values are copied into
`safety.toml` before any caller switches to a safety extension (§7 PR 7).

### 4.7 `stack-audit-due` (merged extension)

`extensions/auditdue`, binary `stack-audit-due`, deterministic, no model or network call, `status` always `pass`
except on its own errors.

- **Silent without a kit.** No `merge` field (a manual `stack run merged`) or empty `kits` → `pass`, empty context,
  no file created or changed.
- **State file** `<kit dir>/audit-due.json`, one per kit: `{"merges": <merges since the last due signal>,
  "state": "<sha256 of the kit state at the previous counted merge>", "oid": "<last counted merge OID>"}`. Written
  to a temp file in the kit dir and renamed, so it is always whole.
- **Approximate counting, no lock.** One orchestrator per kit merges one PR at a time, the normal case. Two merges
  on one kit at the same time may read the same count, so a due line may come one merge late or twice; the line is
  advisory, and neither outcome loses a merge or corrupts the file.
- **Kit state hash:** SHA-256 over, for `NORTH.md` then `state.md`, the file name, NUL, its bytes or `absent`,
  NUL. Both files are the kit state the renderer shows (`internal/kit/render.go:37`) and the steward reads, so
  editing either is a state update; a kit that keeps its essence in `NORTH.md` is judged the same way.
- **Per kit in `kits` order, per merge, each kit on its own:** if `merge.oid` equals the stored `oid`, nothing
  changes (a retry of the same merge does not count twice; only the previous OID is remembered). Otherwise
  `n = merges + 1` and `h` = the current hash. Due when `n >= K` (reason `<n> merges since the last due signal`),
  or else when the state file existed and `h` equals its `state` (reason
  `merged without a state update since the previous merge`). Due → write `{0, h, oid}` and add the line
  `audit due (<reason>): run steward on kit <slug>`; not due → write `{n, h, oid}`. The first merge of a kit (no
  state file) starts the count at 1 and is never due by the second reason; with `K = 1` it is due by the first.
- **K:** default 5; `~/.config/stack/audit-due.toml` `merges = <int >= 1>` overrides it; an invalid value →
  `warn` and the default.
- **Errors** (kit dir unreadable, state file unwritable or not valid JSON): `warn` with one finding naming the kit;
  the other kits are still processed and their due lines still printed. A corrupt state file is replaced by a fresh
  one, so its warn appears once.
- **Output:** one line per due kit, in `kits` order, nothing otherwise. With one kit open, at most one line per
  merge.
- **Best effort.** The reset is written before core accepts the response; if the stage deadline passes or stdout
  is lost after that, the count restarts without a line shown. A `--replay` fixture is sent as is, so a fixture that
  names a real kit's `dir` and a `merge` would change its state: replay fixtures use a throwaway kit directory.

### 4.8 Steward (shuymn/dotfiles, listed for the split only)

`agents/steward.md` gains the output of #28 (one `on-track — continue: <next step>` line; otherwise findings with
file:line evidence, about 5 lines, ending with the next step) and makes the kit's state file an explicit audit
target at any size (#28 comment). The hiden orchestration text starts steward in the background with only the kit
path when a due line appears. Acceptance: P14.

### 4.9 Compatibility exceptions

Public behaviour that changes on purpose, each from #28 or #30 or decided here:

| Input or caller | v10 | v11 | Why |
|---|---|---|---|
| `stack run entry/cycle` with a `block` answer | `block`, exit 1; `expect: block` replay matches | `warn` with `error` set, exit 0; `expect: block` is a mismatch (exit 1) | #30: each stage has one semantic. Fixtures of those stages expecting `block` move to `warn` in PR 2 (none in `fixtures/` today); `stack-cycle` keeps rejecting the result (§2) |
| `pr merge --subject/--body` with the default configuration | message checked for local paths and private terms | not checked (`[merge].message_checks = []`); merges | #30 acceptance 4: with no extension installed the core runs no content check, and the checked merge must work; the owner's wiring lists `public` (§4.2) |
| `pr merge --subject/--body` with a local path or private term, `public` listed | exit 2, diagnostic naming the flag | exit 1, `outbound blocked: public: …` | a refusal at a gate stage exits 1, like `stack run outbound` |
| `pr merge --subject/--body` when a listed message check is missing or fails | (no stage) | refused, exit 1, before any `gh` call | fail-closed for the irreversible merge (§4.2) |
| `pr merge` with a message another listed check blocks | merged | refused | the owner listed it as required (§4.2) |
| `pr merge` without message flags and a broken private-terms file | exit 2 | merges | nothing to check; a broken `config.toml` still exits 2 before `gh` |
| `stack run outbound` with nothing installed and the default `[stages].outbound` | `skipped` `["jev-semantic"]` | `skipped` `["public","jev-semantic"]` | `public` joins the default selection in front of `jev-semantic` (§7 PR 7) |
| `stack check public`, `stack budget status` | commands | unknown command, exit 2 | moved to `stack-public check`; removed (§5.3) |
| A Bash input matching raw-merge and a moved rule | one deny reason, first in rule order | denied by both hooks; the reason shown may differ | two hooks (§4.5) |

### 4.10 What v11 overrides in the phase designs

- `docs/design/phase2-core.md`: §4.1.1 (no size note); §4.2 step 2 "No extensions run" and C19's assertion that a
  fake extension is never invoked (entry at compaction, §4.3; C19's other assertions on ids, the lock, fork and
  close, and C20's restore priority stay); §4.3 (the core hook keeps only raw-merge; the other rules move to
  `stack-guard`, C10–C12, C14–C15 move with them); §4.4 step 1 and C26's message clause (`[merge].message_checks`,
  §4.2); §4.5 (`stack-public check`); §4.6 and C27 (removed); §4.7 (stages, request fields, block demotion, §4.9);
  §2.1's `[safety]` table (§4.6) and its new `[merge]` table and `[stages].outbound` default (§4.2, §7 PR 7).
- `docs/design/phase3-cycle.md`: §3.1's tool list and §3.4 step 12.5 call `stack-public check`; I10 runs the real
  `stack-public` instead of `stack check public` (§7 PR 7). `gh_model` (§3.2) is unchanged. §3.3 replay and O6 pipe
  each call to `stack` and then `stack-guard` (skipped after a `stack` deny; a deny of either decides, rule read
  from the first), and a `stack guard (<rule>)` or `stack-guard (<rule>)` reason names the same rule; with
  `stack-guard` not on `PATH` a warn finding `replay-hook-missing` names it and a recorded deny that `stack` alone
  no longer makes is `not-comparable`.

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

The issue's "about 10 ms" p95 matches the two-process sequential figure on this machine; the single hook is about
5 ms. Spawning one extension per call adds about 2.2 ms p50 / 3 ms p95 before it does any work. The two-hook figure
for (b) is a prediction from these numbers, not a measurement: Claude Code starts the hooks in parallel, so the
expected wall time is near the slower of the two processes. PR 9's live checks may measure it.

| Option | Gains | Gives up | Reversibility | Pencil deny on by default |
|---|---|---|---|---|
| **(a)** core stage `tool-use` runs extensions | one registry (`[stages]`), `--replay` fixtures for guards | +3 ms p95 per extension per tool call, serial on the hottest path; the request must carry Claude Code's hook payload, binding the protocol to a harness schema that changes (fails S); duplicates Claude Code's own matcher, ordering and deny merging; the protocol's failure mode (`warn`) still allows on a broken guard, so nothing is safer | adding a stage is easy, removing a stage from the protocol is not | `[stages].tool-use = ["guard"]` default and the binary installed; a missing binary is silently skipped |
| **(b)** guards move to `stack-guard`, wired as its own `PreToolUse` hook (**chosen**) | core keeps one rule and loses `tdewolff/parse` and the private-term path; no new protocol surface; the hooks run in parallel, so the added wall time is expected near max(core, guard) rather than the sum; the guard is replaceable by any other hook or settings rule | one more process per matching call (CPU); guards are not visible to `stack run --dry-run`; the guard shares parser packages with core (§4.5); a multi-rule deny may show a different reason (§4.9) | full: the binary can be folded back, or a `tool-use` stage added later without breaking anything | `stack-guard` defaults `deny_pencil_generate = true` with no file or a broken one (§4.6); the owner's settings wire it in the same dotfiles PR that installs it, and that PR's settings test asserts the entry (§7) |
| **(c)** guards stay built in until a second consumer appears | zero cost, zero migration | fails R and S for four rules; contradicts #30's acceptance "shall run no content check" with no extension installed | trivially reversible | unchanged |

(b) is chosen: it satisfies #30 without a predicted wall-time cost and does not add a stage whose contract would
depend on one harness. (a) becomes worth reconsidering only when a second harness needs the same guards through one
interface.

### 5.2 Post-merge stage name and the audit-due contract

- **Name `merged`.** It names the position (after the verified merge) the way `entry` and `outbound` do; it is the
  name #28 suggests. Contract §2, §4.2.
- **State file** `<kit>/audit-due.json` inside each kit (§4.7): the signal belongs to the kit, and the kit dir is
  already the extension-visible place for kit state (`kits[].dir`).
- **K = 5**, the issue's default. Over the 26 merges of the run #28 cites, with one kit whose state is updated
  after every merge and a steward run per due line, that is five background audits instead of one, each about one
  line when on track.
- **Kit state not updated, by content hash, not mtime.** A hash compares what the steward cares about (content);
  mtime changes on saves without changes, on checkouts and syncs, and has filesystem-dependent resolution, so tests
  would need sleeps. The files are small; hashing costs nothing measurable. Both `NORTH.md` and `state.md` are
  hashed because both are the kit state (§4.7).
- **Deviation from #28: the comparison point.** #28 asks whether `state.md` was updated since the last due signal;
  this design compares with the previous counted merge instead. The orchestrator updates the state after each
  merge (`dotfiles:home/dot_claude/skills/hiden/references/attention.md:15`), so two merges with no update between
  them are the drift signal, while comparing with the last due signal goes silent for the rest of the K window
  after one update (§8.4). The consequence: one update after a due signal, then a merge without an update, is due
  at that merge although fewer than K merges passed.
- **Counting is approximate** (§4.7): no lock, since the normal case is one orchestrator per kit.

### 5.3 `budget status` and the byte threshold

Removed. Without the byte note `stack budget status` lists kit sizes and per-role item counts
(`internal/budget/budget.go:129-200`); `kit open` already renders the orchestrator items, and `items.json` is plain
JSON. No program calls it (§6). The renderer drops the size note at both call sites (`kit open` and compact,
`internal/kit/render.go:123-126`). The compaction budget (`docs/design/phase2-core.md` §4.2 step 5) stays: it is
where essence is actually lost, and it already marks what it omits.

### 5.4 Migration order

§7. The invariants: no caller switches to a safety extension before the owner's `[safety]` values are in
`safety.toml` and the extension binary is on every `PATH` that caller runs with; from the dotfiles PR that wires
`stack-guard` until the core removal, both hooks deny the moved rules (duplicate denies are harmless); the core copy
goes only after `stack-guard` is proven to deny by itself with the applied settings (P7, P8) and PR 10's wiring is
proven in an isolated setup (P16).

## 6. Callers

| Caller | Today | After |
|---|---|---|
| `extensions/improve/publish.go:198-220` (step 12.5) | `stack check public <base>..<oid> --title-file --body-file --branch`; exit 1 → `proposal:public-check`, other failure → `failed:check` | `stack-public check …`, same arguments and exit codes; a missing binary is a start failure → `failed:check`, so no Codex check, push or PR follows. `stack-public` joins the tools improve needs (`docs/design/phase3-cycle.md` §3.1) |
| `dotfiles:home/dot_claude/skills/hiden/references/commit-pr.md:7` | `stack check public <base>..<head> --title-file … --body-file … --branch …` before each push and PR edit | `stack-public check …`, same arguments; a binary that does not start stops the push or PR edit like a hit |
| `dotfiles:scripts/stack-private-terms:5` | docstring: writes the terms `stack check public` rejects | wording: `stack-public`; file path unchanged (`private_terms` default) |
| `dotfiles:home/dot_claude/skills/hiden/references/attention.md:5` | says per-role counts are shown by `stack budget status` (display only) | sentence removed; the items stay in `attention/items.json` |
| `stack pr merge --subject/--body` callers | refused with exit 2 on a local path or private term | with `[merge].message_checks = ["public"]` (the owner's wiring): refused with exit 1 by `stack-public`, or when a listed check cannot answer, before any `gh` call; with the default `[]`: not checked (§4.2) |
| `dotfiles:scripts/stack-cycle:15-20` | rejects a cycle result with `block` or a non-empty `.error` | unchanged: a demoted `block` carries `.error` and is still rejected (§2) |
| `README.md:10-13`, `AGENTS.md:7`, `cmd/stack/main_test.go:75` | describe or test `check public`, `budget status`, the five-rule guard | updated in the PRs that move them |

No script, hook or statusline calls `stack budget status` (`grep` of this repository and `shuymn/dotfiles`); the
dotfiles hiden text already says to write only what later decisions depend on, without a byte number
(`dotfiles` commit `ac1392b9`), so #28's dotfiles wording change is done. `dotfiles:home/dot_claude/attention/create_budget.json`
belongs to the legacy `attention-budget.py`, retired by Phase 3, and is not a caller.

## 7. PR Split

`shuymn/stack` PRs pass `scripts/test` and the Windows CI job; dotfiles PRs pass their own tests.

1. **docs: design core stages and extension boundary** — this note, `docs/architecture.md` v11.
2. **feat: give each stage one semantic** — block demotion at `entry`/`cycle` (§2) with fixture migration (§4.9),
   `kits` request field (§4.1), entry at compaction (§4.3). Done when C1–C4 pass.
3. **feat: add the merged stage** — `merged` (stage, 10 s, `merge` field, `stack run merged`), `stack pr merge`
   runs it (§4.2 step 3), default `[stages].merged = ["audit-due"]`. Done when C5–C7 pass.
4. **feat(ext): add audit-due** — `extensions/auditdue`, README install line. Done when C8–C11 pass.
5. dotfiles **docs: audit on the merged stage's due signal** — steward output (§4.8), orchestrator starts steward in
   the background on a due line, `attention.md:5` sentence removed, `stack-audit-due` added to the install command.
   After PR 4. Done when P14 passes.
6. **refactor: drop the state.md byte threshold and budget status** — `StateLimit`, `internal/budget`, the
   command, docs. After PR 5 (its last textual caller). Done when C12 passes.
7. **feat(ext): add stack-public** — `extensions/public` (both modes, reading `safety.toml`), `stack run outbound
   --title-file/--body-file/--branch`, improve switched to `stack-public check`, default `[stages].outbound =
   ["public", "jev-semantic"]`, the core config key `[merge].message_checks` (default `[]`, validated as stage
   names, read by nothing until PR 10, so the wiring of PR 9 loads without an unknown-key warning), README install
   line. `DropRepoVars` and the input file reader move from `checkpub` to core packages
   with their existing regression tests (`pr merge` and stage requests ignore a hook's `GIT_DIR`); `publictext` and
   `checkpub` are copied into `extensions/internal/`, and core keeps its copies until PR 10. Done when C13–C15 and
   C21 pass, including phase 3 I10 with the real `stack-public` binary and "binary missing → `failed:check`, no
   Codex, push or PR". **Install order** (owner): (a) write `~/.config/stack/safety.toml` from the effective
   `[safety]` of `config.toml` and compare custom `secrets` paths, a custom `private_terms` file and an explicit
   `deny_pencil_generate = false` (the table stays in `config.toml` while the core still reads it); (b) install
   `stack-public` and check it resolves both in an interactive shell and on the `launchd` job's `PATH`
   (`docs/design/phase3-cycle.md` §3.6); (c) only then reinstall `stack-improve`.
8. **feat(ext): add stack-guard** — `extensions/guard`, `safety.toml` reader, `stack-guard (<rule>): ` reasons,
   depguard allow list (§4.5). The core hook is unchanged, so both deny. Done when C16–C17 pass.
9. dotfiles **feat(claude): wire stack-guard and stack-public** — install both binaries, add the `stack-guard`
   `PreToolUse` entry (settings test asserts it), `[merge].message_checks = ["public"]` in the owner's
   `config.toml`, `commit-pr.md` (including that a `stack-public` that does not start stops the step) and
   `stack-private-terms` to `stack-public`. After PRs 7–8 and PR 7's install order. Then `make
   apply` and the live checks P7–P9 with the applied settings and installed binaries, and P16 (PR 10's wiring in an
   isolated setup).
10. **refactor!: keep only the raw-merge deny in the core hook** — remove the four rules, `check public`,
    `[safety]`, core `publictext`/`checkpub`, `tdewolff/parse`; `pr merge --subject/--body` runs
    `[merge].message_checks` (§4.2 step 2). Only after PR 9's live checks pass. Done when C18–C20 pass, the
    owner's applied `config.toml` has `[merge].message_checks = ["public"]` and `stack-public` resolves on the
    `PATH` the owner's sessions use, and P10 passes on a real `pr merge` with that configuration. Rollback: if a guard check fails after PR 10 is installed, reinstall the pre-PR-10 core (full guard)
    first, then fix the wiring.
11. dotfiles **chore(claude): narrow the stack hook matcher to Bash** — after PR 10 is installed. Optional: the core
    hook allows every non-Bash call anyway.

Order: 1 → 2 → 3 → 4 → 5 → 6, and 7 → 8 → 9 → 10 → 11 (the second chain may start after 2). Guards stay active
throughout: before PR 9 only the core hook has them, between PR 9 and PR 10 both do, after PR 10 only `stack-guard`
does, and PR 10 is not installed before PR 9 is applied and verified.

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
  the CLI mode needs none. `pr merge` applies the same strictness internally (§4.2) because it is a core call site.

### 8.3 The merge message check

- Dropping it (callers run `stack-public` on the texts first): rejected; it loses behaviour silently, against #30
  acceptance 5.
- Running `stack-public` directly from `pr merge`: rejected; the core would know a named extension.
- Gating the merge with `[stages].outbound`: rejected. That selection serves push hooks, CI and skills, where
  missing extensions are skipped, and holds optional ones such as `jev-semantic`; making each of them required at
  `pr merge` would refuse every merge message until all are installed, and dropping them from the default would
  silence them for `stack run outbound`. A separate required list keeps both.
- `message_checks` defaulting to `["public"]`: rejected; with no extension installed every merge message would be
  refused, against #30 acceptance 4. The owner opts in through the dotfiles wiring.
- Treating a missing or failing extension as `warn` at `pr merge`, as `stack run outbound` does: rejected; a
  selected check that cannot run would let an irreversible merge publish the message unchecked.

### 8.4 Audit-due detection

- mtime instead of a hash: §5.2.
- Comparing with the state at the last due signal (#28's text): rejected; after one update the rule would stay
  silent for the rest of the K window, however many merges followed without an update.
- A per-kit lock for an exact count: rejected; the line is advisory and one orchestrator per kit is the normal
  case (§4.7).
- An OID history or delivery acknowledgement: rejected; the previous OID covers retries of the same merge, and a
  lost line costs one late audit.

## 9. Acceptance Criteria

### 9.1 Purpose layer

| ID | EARS criterion | Source | Observable result / failure | Verify on the real thing |
|---|---|---|---|---|
| P1 | The architecture shall define every lifecycle stage in the core, each with one semantic, position, precedence, deadline and failure mode, and list every capability left out of core with the core condition it fails. | #30 acceptance 1 | `docs/architecture.md` §4 stage table and "Intentionally Not in Core" | read §4 against §1–§3 here |
| P2 | Extensions shall attach only to stages the core defines. | #30 acceptance 2 | an unknown stage is a usage or config error | `stack run tool-use` → exit 2; `[stages].tool-use` → unknown-key warning |
| P3 | When no extension is installed, the core shall provide every stage, session identity, kits and their restore, the checked merge and the raw-merge deny, and shall run no content check. | #30 acceptance 4 | no deny for a secret `Read`; deny for `gh pr merge`; `stack check public` unknown; a local path in outbound texts passes | `PATH` without `stack-*`, empty `HOME`: pipe a `Read .dev.vars` and a `gh pr merge 1` payload to the core hook; `stack run outbound --body-file` with a home path → pass with `skipped`; no `config.toml`, `pr merge --body <home path>` on a scratch PR → merged |
| P4 | When `stack pr merge` verifies a merge and no `merged` extension is installed, its output and exit code shall be unchanged. | #28 acceptance 1 | stdout exactly `MERGED <H> <oid>`, stderr unchanged; exit 0 | merge a scratch-repo PR with `stack-audit-due` off `PATH`, without message flags |
| P5 | While no kit is open in the session, the audit-due extension shall print nothing. | #28 acceptance 2 | stdout is only the `MERGED` line | ordinary session, scratch PR merge with the extension installed |
| P6 | When a kit is open, its merges run one at a time, and the extension's state write and core's acceptance of its response succeed, the extension shall print exactly one due line on the merge that reaches K since the last due signal, or that follows the previous counted merge without a kit state update; otherwise nothing. | #28 acceptance 3, with the comparison point of §5.2 | `audit due (…): run steward on kit <slug>` after `MERGED` | `stack kit open demo`, merge two scratch PRs without touching the kit state → line after the second; edit `state.md`, merge → nothing |
| P7 | When `stack-guard` is wired, the secrets, gh-publish, rmroot and Pencil rules shall each be denied by `stack-guard` itself. | #30 acceptance 5 | a `stack-guard (<rule>): ` deny per event type: Bash, Read, NotebookRead, Grep, Glob, `mcp__pencil__execute` | live session after dotfiles PR 9 with the applied settings and the installed binary (both hooks active): the phase 2 live checks of P2, P4, P14, P18, each showing the `stack-guard` reason in the hook output (`claude --debug` log) |
| P8 | While `safety.toml` is absent or invalid, `stack-guard` shall deny a Pencil `Generate(` call. | owner decision (Pencil) | `stack-guard (pencil): ` deny | live Pencil session with no file, then with a broken file |
| P9 | When `stack-public check` gets the inputs of a `stack check public` run, it shall produce the same stderr lines and exit code. | #30 acceptance 5, To decide 2 | identical output | the phase 2 P12 seeded repo with both binaries before PR 10 |
| P10 | When `pr merge` gets `--subject` or `--body` and `[merge].message_checks` lists `public`, it shall merge only when `stack-public` answers without a hit, and refuse before any `gh` call otherwise. | #30 acceptance 5 | hit → exit 1, hit named, no `gh` call; `stack-public` missing → exit 1, `could not check`, no `gh` call | scratch PR: `--body` with a home path; then a clean `--body` → merged |
| P11 | The kit context shall carry no size note, and compaction shall keep its 9,500-char budget. | #28 comment | no `bytes, over` text; compact output ≤ 9,500 UTF-16 units | `kit open` on a kit with a 10 kB `state.md`; `/compact` and measure the hook's `additionalContext` in UTF-16 units |
| P12 | When a session with open kits compacts and the hook has the time of §4.3 left after the restore, it shall restore the entry contexts after the kits within the budget. | #30 stage table | entry block after compaction | live: a stub entry extension, `kit open demo`, `/compact` |
| P13 | Where a stage cannot block, a `block` response shall be reported as `warn` and `stack run` shall exit 0. | #30 stage table | `"status":"warn"` with the finding and `error` | stub `stack-demo` answering block, `stack run cycle --ext demo` |
| P14 | When a due line appears, the orchestrator shall start steward in the background with only the kit path, and steward shall answer with one `on-track — continue:` line or about 5 lines of findings with file:line evidence ending with the next step. | #28 acceptance (steward) | the steward request and its answer | after dotfiles PR 5: a kit with an up-to-date state → the one line; a kit whose `state.md` names a finished item as open → its file:line and a next step |
| P15 | Each stage shall fire at every call site of §2, except where its deadline rule skips it. | #30 stage table | one request per call site in the stub's log | a logging stub selected at every stage and in `[merge].message_checks` in a temporary config: `kit open`, `/compact` (well inside the hook's time), `pr merge --body` and the merge itself on a scratch PR, `stack run cycle`, `stack run merged` |
| P16 | Before PR 10 is installed, its wiring shall deny the four moved rules through `stack-guard` alone and check merge messages through `stack-public`. | #30 acceptance 5, §5.4 | `stack-guard (<rule>): ` denies; raw merge denied by `stack guard (raw-merge): `; `pr merge --body <home path>` refused by `public` before `gh` | an isolated Claude Code config with PR 10's `stack` build, the `stack-guard` entry and PR 9's `config.toml` (`message_checks = ["public"]`): the P7 checks and a scratch-PR merge with a home path in `--body` |

### 9.2 Plan layer

| ID | EARS claim | Test | Required assertions |
|---|---|---|---|
| C1 | When an extension answers `block` at `entry`, `merged` or `cycle`, `ext` shall record `warn` with the stage finding and `error`. | `ext.TestBlockDemotion` | per stage; `outbound` keeps `block`; finding text; `context` and findings kept |
| C2 | `stack run` shall exit 1 on `block` only at `outbound`. | `cli.TestRunBlockExit` | four stages; replay `expect` compared after demotion; `--title-file`/`--body-file`/`--branch` at a non-outbound stage or with `--replay` → exit 2 |
| C3 | When `STACK_SESSION_ID` is valid, every live `entry` and `merged` request shall carry `kits` with the existing open kits in `.kits` order; otherwise `[]`. | `cli.TestRequestKits` | unset id → `[]`; a listed kit whose dir is gone is left out; `.kits` lock held past 1 s → `[]` plus the stderr note; `outbound` and `cycle` requests have no `kits` |
| C4 | When compact restores at least one kit and the stage deadline of §4.3 is at least 1 s away, session-start shall run the entry stage once, place each result after the boards, cut or omitted within the budget, without changing the restore, start no work after the cut-off, and write its output before the host's 60 s. | `session.TestCompactEntry` (replaces only the "no extension" assertion of C19 of `docs/design/phase2-core.md`; injected clock or start time) | fake extension invoked once with the restored `kits` and the payload's id in `STACK_SESSION_ID` when the inherited env names another id; no kits → not invoked; a fake `git` that sleeps is never called; boards running to their deadline → entry bounded by the remaining time, no work started after the cut-off and the hook's output written before 60 s; stage deadline just over 1 s away → invoked; just under 1 s → not invoked, `[entry skipped: deadline]`; an extension that runs to the deadline and leaves a pipe open to a grandchild → the next extension is not started, the stage has returned by the cut-off and the hook's output is written before 60 s; a hanging extension → the restore is written whole; long context with astral-plane chars cut on a code-point boundary with marker; total ≤ 9,500 UTF-16 units; no room for the marker → no entry text and the stderr note; C19's lock, fork and close cases still pass |
| C5 | When `pr merge` verifies a merge, it shall write the `MERGED` line before starting `merged` and exit 0 whatever the results. | `merge.TestMergedStage` (fake gh, fake extension) | the test's stdout writer and the fake extension append to one ordered log, and the `MERGED` write precedes the extension's start; warn marker and context after the line; extension `block` → shown as warn, exit 0; direct path, `--auto` while polling, and `--auto` success found by the final read-back after the wait deadline; request `repo` and `merge` from the verified run with no extra git or gh call; a slow first extension → the next is not started after the deadline; a first extension that runs to the deadline and leaves a pipe open to a grandchild → the next is not started and the stage run returns within 10 s of its start (the results' printing follows); stdout closed → exit 0 |
| C6 | If `pr merge` refuses or cannot confirm the merge, or is cancelled before its success point, it shall not run `merged`. | `merge.TestMergedStageNotRun` | refusal, unknown, cancel then success found by the final read-back: the fake extension's log is empty; cancel after `merged` started → the merge still reports success |
| C7 | When no `merged` extension is installed, `pr merge` stdout, stderr and exit code shall be byte-identical to today, under the same `[merge].message_checks` configuration. | `merge.TestMergedNoExtension` | exact stdout, stderr and exit code, with `STACK_SESSION_ID` set, its `.kits` file present and its lock held by the test: the `.kits` file is not read and no lock note appears; a `merged` name found but not executable → `MERGED` line, then its warn marker, exit 0, `.kits` not read |
| C8 | While the request has no `merge` field or `kits` is empty, `stack-audit-due` shall answer `pass` with empty context and write nothing. | `auditdue.TestSilent` | both branches separately: `pass`, `context` `""`, no file created, an existing `audit-due.json` byte-identical |
| C9 | When K merges have been counted since the last due line, `stack-audit-due` shall print one due line and reset the count. | `auditdue.TestK` | the kit state changes before every merge, so only the count condition can fire; K from default and from `audit-due.toml`; `K = 1` → due on the first merge; invalid K → warn and default; two kits → separate counts and one line each in `kits` order; two processes on one kit at once → the file stays valid JSON and the count rises by one or two |
| C10 | When a merge follows the previous counted merge with an unchanged kit state hash, `stack-audit-due` shall print one due line; the first merge of a kit shall not. | `auditdue.TestStateUnchanged` | K set above the merges made, so only the state condition can fire; `state.md` only, `NORTH.md` only, both; changed bytes → silent; same `oid` twice → counted once; first merge → silent |
| C11 | If a kit's state file is corrupt or unwritable, `stack-audit-due` shall answer `warn` with one finding for that kit and still process the other kits. | `auditdue.TestErrors` | corrupt → replaced; the next run has no corrupt warn (it may still be due by the rules); one broken kit and one due kit → warn plus the due line of the other |
| C12 | The kit renderer shall not add a size note. | `kit.TestRender`, `cli.TestKitOpen` | a state file over 3,000 bytes renders verbatim in `kit open`'s output; `stack budget status` → unknown command, exit 2 |
| C13 | `stack-public check` shall match the `stack check public` contract. | `public.TestCheck*` (the moved `checkpub` and `cli` check-public tables, real git) | every existing case, same stderr and exit codes; a broken `safety.toml` → exit 2 |
| C14 | In stage mode `stack-public` shall answer `block` on a hit or its own error and `pass` otherwise. | `public.TestStage`, `cli.TestRunOutboundPublicLarge` | hit in range, `title`, `body`, `branch`; unreadable terms → block; broken `safety.toml` → block; empty range scans texts only; a range whose distinct hits would produce over 1 MiB of diagnostics → a valid `block` under the cap with the `N more hits` trailer, and `stack run outbound --ext public` on it exits 1; `stack-public check` on the same range prints every hit |
| C15 | `stack run outbound --title-file/--body-file/--branch` shall send `title`, `body`, `branch`. | `cli.TestRunOutboundTexts` | fake extension's stdin log; `~` resolved; empty file and `--branch=` → `""`; non-UTF-8 file → exit 2 |
| C16 | The `stack-guard` binary shall decide the four moved rules as the core hook does, with its own reason prefix. | `guard.TestBinary` (`stack-guard` built in `TestMain`, synthetic payloads on stdin) plus the goldens moved to `extensions/guard/testdata/` | every existing golden except raw-merge, reasons with `stack-guard (`; argv `hook claude pre-tool-use` and no-argument usage exit 2; `safety.toml` read from `HOME`; malformed payload → no output, exit 0 (fail-open); allowed secret handoffs (`K="$(cat .dev.vars)" cmd`, `--token "$(cat .dev.vars)"`); publish via `--body-file`, `--body-file - <<EOF`, release asset and `--jq` exclusions; Windows `rm -rf C:/` and a linked worktree's `.git` file; Pencil real call, string and `deny_pencil_generate = false`; a payload matching raw-merge and secrets → `stack-guard` denies secrets and the core binary denies raw-merge |
| C17 | While `safety.toml` is absent or invalid, `stack-guard` shall apply the v10 `[safety]` defaults. | `guard.TestSafetyConfig` | Pencil deny and secrets deny under no file and under a broken file; a broken file's custom secret path is not denied |
| C18 | The core hook shall deny raw merges and allow everything else. | `guard.TestRawMerge` (kept), `cli.TestPreToolUseCoreOnly` | a `Read .dev.vars`, `rm -rf /` and Pencil `Generate()` payload → no output |
| C19 | When `--subject` or `--body` is given and `[merge].message_checks` is not empty, `pr merge` shall run the listed extensions at `outbound` before any `gh` call and refuse unless each answered without `block` or error; it shall validate `config.toml` before any `gh` call in every case. | `merge.TestOutboundMessage`, `cli.TestPRMergeConfig` (real `stack-public` built in `TestMain`, fake gh) | no `config.toml` and no `stack-*` on `PATH`, `--body` with a home path → gh called, merged; explicit `message_checks = []` → gh called; `public` listed but not on `PATH` → exit 1, gh argv log empty; `public` listed, real binary, hit → exit 1, log empty; clean message → gh called; `[stages].outbound` naming a missing extension with `message_checks = []` → gh called; an extension that hangs past the deadline → exit 1, log empty; a valid `warn` continues; no message flag → checks not run; broken `config.toml` → exit 2, log empty, with and without message flags; broken private-terms file, no message flag → gh called |
| C20 | Core shall not import or name any content check. | `depguard` + `cli.TestCommands` | `check public` absent from usage; `internal/publictext` gone; `go list -deps ./cmd/stack` has no `tdewolff`; `extensions/` other than `guard` still denied `internal/`; P3's zero-extension checks |
| C21 | Improve shall publish only after `stack-public check` passes on the pinned OID and fixed texts. | phase 3 I10 (`improve.TestPublicCheck` with the real `stack-public` built in `TestMain`) | I10's existing assertions with `stack-public`; `stack-public` absent from `PATH` → `failed:check`, and the fake Codex, `git push` and `gh pr create` logs are empty |

The existing regression tests of `DropRepoVars` and the input file reader move with them to their core packages in
PR 7 and keep their assertions (a hook's `GIT_DIR` and `GIT_WORK_TREE` are ignored by `pr merge` and stage request
building).

## 10. Owner Questions

None. Every public behaviour change here follows from #28 and #30 or is decided here and listed in §4.9: the
`merged` stage and its output, `check public` and the guards becoming extensions (with the CLI name `stack-public
check` and the `stack-guard` hook), `budget status` and the byte note removed, `block` demoted at stages that cannot
block, entry contexts restored at compaction (the #30 stage table lists "compaction restore" under `entry`), the
fail-closed merge message check with exit 1, `kits` sent only to `entry` and `merged`, the multi-rule deny display,
and the audit-due comparison point (§5.2). That includes the merge message check: v10 checked merge messages by
default, v11 checks them only when `[merge].message_checks` lists a check. This follows from #30 acceptance 4 (with
no extension installed the core runs no content check and the checked merge still works), and the owner keeps the
v10 check through the dotfiles wiring (§4.2, §7 PR 9), so it needs no owner question.
