# Kit Contracts Design: One Owner per Kit and Session Artifact

Design for issue #69 at `e33f018`. Rule from the issue: whoever reads or writes an artifact in code owns its format;
an artifact nobody reads in code is discarded or left to the side that uses it by hand. `stack` reads or writes the
kit files, `sessions/<id>.kits`, the restored text and the audit-due line in code, so their contracts are stated in
this repository. The owner's setup in `shuymn/dotfiles` (the hiden skill, the steward agent, hooks) says only when and
why to use them, and points to the owning section for the format. Code is cited as `path:line` at `e33f018`; dotfiles text as
`dotfiles:<path>:<line>`. This note holds the decisions, the index, the open questions and the acceptance criteria;
the contracts themselves are in the sections §2 lists.

## 1. Where the contracts live

| Option | Gains | Loses | Reversibility |
|---|---|---|---|
| (a) A new note that states every kit and session contract | one page for outside readers | a second statement of what `phase2-core.md` §4.1–§4.2 and `core-stages.md` §4.7 already specify, next to the code's design and acceptance criteria; the two drift as #66 drifted across repositories | easy (docs only) |
| **(b) Fold each contract into the section that designed its code, and list them here (chosen)** | one statement per contract, beside its EARS claims and tests; matches how the repository already states contracts (`powershell-guard.md` §8 lists where its contracts are stated) | an outside reader follows one link from §2 | easy (docs only) |

(b) is chosen: the repository states a contract where its code was designed and keeps a design note for the
decision and the index. Changing to (a) later means moving text, not changing behaviour.

## 2. Where each contract is stated

| Artifact | Writer | Code readers | Outside readers | Contract |
|---|---|---|---|---|
| Kit dir `<kit root>/<slug>/`, `state.md`, `NORTH.md` | the orchestrator (content); `kit open` creates the dir (`internal/cli/kit.go:79-91`) | renderer (`internal/kit/render.go:95-128`), `stack-audit-due` hash (`extensions/auditdue/auditdue.go:227`) | steward reads them by instruction | `phase2-core.md` §2.1 (kit root), §4.1.1 (rendering); content and headings: hiden |
| `attention/items.json` | the orchestrator | renderer (`internal/kit/render.go:161-201`) | none in code | `phase2-core.md` §4.1.1 "Attention items" |
| `bin/board` | the orchestrator, optional | renderer (`internal/kit/render.go:234-282`), compact (`internal/session/session.go:206-236`) | none | `phase2-core.md` §4.1.1, §4.2 step 2 |
| Restored text (`kit open` stdout, compact `additionalContext`) | `stack` | none | hiden checks `<orchestration-state>` after a handover and `<orchestrator-attention-items path=…>` in a retrospective | `phase2-core.md` §4.1.1 (parts, tags, `kit open`), §4.2 steps 5–7 (budget, compact output, entry points compared) |
| `sessions/<id>.kits` | `stack kit open/close` (`internal/kit/store.go:103-134,198-215`) | `kit.Store` under the lock (`internal/kit/store.go:46-77`) | the ask-owner hook (`dotfiles:home/dot_claude/hooks/executable_ask-owner-checklist.sh:17-33`), steward (`dotfiles:home/dot_claude/agents/steward.md:9`) | `phase2-core.md` §4.1 "Read-only format for external readers" |
| The audit-due line | `stack-audit-due` (`extensions/auditdue/auditdue.go:222`), printed by `pr merge` | none | the orchestrator by instruction (`dotfiles:home/dot_claude/skills/hiden/references/attention.md:7`, `commit-pr.md:15`) | `core-stages.md` §4.7 "The due line" |
| Steward request and report | the orchestrator; steward | none | — | `shuymn/dotfiles` (`agents/steward.md`, hiden); `core-stages.md` §4.8 only points there |
| `~/.local/state/stack/cycle/` | the dotfiles cycle wrapper | none | the dotfiles statusline | reserved in `phase2-core.md` §2.1; contract: §5.2 |

## 3. Decisions

### 3.1 `attention/items.json`

The shape, the restored roles and every ignored input are stated once, in `phase2-core.md` §4.1.1. Items for roles
`stack` does not restore may stay in the file: the format is shared with whatever else reads it, and `stack`
renders only its own audience.

Ignored items are reported as §4 states, for these reasons: an ignored register silently loses a rule the
orchestrator relies on after compaction (the re-read rule of `phase4-jev.md` §3.2 counts only when its block
appears); `kit open` is the one moment the orchestrator sees stderr and can fix the file; at compaction stdout is
the restore, and stderr does not reach the agent's context.

### 3.2 Restore output

The parts, tags and markers are stated in `phase2-core.md` §4.1.1, the compact budget and output in §4.2 steps 5–6,
and which entry point emits what in §4.2 step 7, the single place hiden points to. The contract states the budget as
the allocator delivers it, including the case where markers alone fill the budget and later kits leave no text,
rather than a guarantee the code does not keep. A `NORTH.md`-only kit keeps its state block at both entry points
when room remains (checked on the built CLI), so the agent sees its state. When the budget is used up first, the agent
sees a `[state omitted: …]` marker or no text for that kit, which is accepted (§6).

### 3.3 `sessions/<id>.kits`

| Option | Gains | Loses | Reversibility | Cost |
|---|---|---|---|---|
| **Document a stable read-only format (chosen)** | serves both readers as they are: steward has only file tools (`dotfiles:home/dot_claude/agents/steward.md:4`), and the hook avoids process starts on a hot path (`executable_ask-owner-checklist.sh:11-12`) | the format is frozen; readers skip the lock and see the races §6 lists | a read command can be added later without changing the file | docs only |
| Add `stack kit list` (slugs, and dirs under the configured root) and switch readers | lock, validation and dir resolution in one place | steward cannot run it; the hook pays a process start per question; one more CLI surface | removing a command is breaking | code, tests, two dotfiles changes |

Revisit with a read command when the file needs a new field, or when a reader that can run commands needs the kit
dirs.

### 3.4 The audit-due line

The line keeps its text: it is matched by its consumer, and its meaning can be stated without changing it. The kit it
names is defined by the root the merge itself used, because that is the dir the extension counted. The consumer
resolves the current config, on the premise that the root is not edited from the start of `pr merge` until the
consumer has resolved the dir; a snapshot of the merge's config would guard only against that edit, so none is added.
How the consumer finds the dir is in `core-stages.md` §4.7.
Rejected: adding the kit dir to the line (§6).

### 3.5 Steward out of `stack`

`core-stages.md` §4.8 no longer states the steward report format or how it is started; it points to
`shuymn/dotfiles`. P14 checks only what `stack` emits; the steward's acceptance moves to the companion PR (§8).

## 4. Warning about ignored attention items

`internal/kit` reports why it ignored items, without changing what it renders; `internal/cli/kit.go` prints it.
Item indexes are 0-based (the JSON array index). hiden writes the register after the first `kit open` of a new kit, so the warning catches a malformed file when the kit is opened again (a reopen or a handover), not on
creation.

| ID | EARS claim | Test |
|---|---|---|
| W1 | When `stack kit open` renders a kit whose `attention/items.json` is readable but is not a JSON object whose `items` is an array, it shall write exactly one line `stack: <path>: attention items ignored: <reason>` to stderr. | `cli.TestKitOpenItemsWarning`, one case each with its reason string: invalid JSON, top-level `null`, `123`, `"x"`, `[]`, `{}` (`items` missing), `{"items": null}`, `{"items": "x"}`; control: no file → no line |
| W2 | When `items` is an array holding elements that are not objects, or whose `role` or `rule` is missing, `null` or not a string, `stack kit open` shall write exactly one line `stack: <path>: <n> of <m> attention items ignored (first: item <i>: <reason>)` to stderr and still render the valid items. | same test, mixing valid `orchestrator` and `all` items with a `null` element (`{"items": [null, …]}`; a top-level `null` file is W1's), `3`, an item without `rule`, `{"role": 1, "rule": "x"}`, `{"role": "all", "rule": 2}`: asserts one line, `n`, `m`, the first index and reason, and the surviving bullets in order; `kit.TestItemsDiagnostics` for each reason |
| W3 | While the file is absent or every element is well-formed, `stack kit open` shall write nothing about the items, including for items whose role is neither `orchestrator` nor `all`, an empty `rule`, and an empty `items` array. | same test: a `worker` item (no block, silent), an empty `all` rule (`- `, silent), `{"items": []}` (no block, silent) |
| W4 | `stack kit open` shall keep its stdout byte-identical and its exit code unchanged in every W1–W3 case. | same test, comparing stdout and the exit code with fixed expectations of the renderer's output, stderr captured separately, including a number past `float64` and a repeated `items` key in another letter case, which render as Go's JSON decoding gives them |
| W5 | Session-start shall not write any item diagnostic. | `cli.TestSessionStartItems`: `cli.Run` of `hook claude session-start` with a compact payload and a malformed file, with `STACK_DEBUG` unset and set to `1`: no diagnostic on stderr or in `additionalContext` |

## 5. Open questions

### 5.1 Where `writing.md` lives

`extensions/improve/writing.md` holds rules for editing the owner's skills and agents (it names hiden, `wasure`,
`home/dot_claude/**` and a dotfiles skill), and `stack-improve` embeds it in its prompt
(`extensions/improve/model.go:20-21,205`). Dotfiles editors read it from this repository
(`dotfiles:AGENTS.md:45`), and every edit needs a `stack-improve` reinstall.

| Option | Gains | Loses | Reversibility | Cost |
|---|---|---|---|---|
| (a) Keep it embedded | improve's prompt is self-contained; no new config | a generic extension carries one target's rules; edits cross repositories and need a reinstall; the target's editors read another repository | — | none |
| **(b) The target may supply it (recommended)** | the repository whose files are edited owns their rules; an edit takes effect at the next run; existing configurations without the key see no change | an optional field in improve's repository contract and one new error that only that field can reach; the embedded file stays until a later decision | drop the key | one `stack` PR (field, read, tests, I7 extended) and one dotfiles PR (copy the initial guide to a path outside the editable globs, set the key, update `AGENTS.md:45`) |
| (c) Point the model at a file in the worktree by name | no config | the model may not read it; nothing checks it | easy | small |

(b) in detail, as a follow-up: `.stack/improve.toml` gains an optional `writing = "<path>"`, a repository-relative
path read at the base commit. `git ls-tree` at the base commit must show it as a regular-file blob (mode `100644` or
`100755`), stricter than the `" blob "` check `.stack/improve.toml` gets (`extensions/improve/improve.go:358-367`),
which also passes a symlink (`120000`) and would then read the link target's name as the guide. With the key absent,
the embedded guide is used as today. With the key set, that file replaces the embedded guide; a path that `paths`
matches (improve could rewrite its own rules), a symlink, a tree, a missing path or a failed lookup or read is
`warn` and stop. The initial guide is a copy of the embedded one, which stays as the fallback. The order is safe
either way: today's `repoConfig` ignores unknown keys, so dotfiles can set the key first; a configuration that
already holds an unrelated `writing` key would give it this meaning.

Recommendation (b). With the embedded fallback the change is additive: no prompt of an existing configuration
without the key changes, and the new error needs the new key, so it is a method choice. It becomes an owner decision if the embedded default is
dropped (targets without the key would lose the writing section). It would change if a second target wanted the same
rules (a shared default in `stack-improve` fits that better), or if improve stopped editing agent instructions.

### 5.2 The cycle summary files

`~/.local/state/stack/cycle/last.json` and `last-error` are written by `dotfiles:scripts/stack-cycle` and read by the
dotfiles statusline; no `stack` code touches them, yet `phase3-cycle.md` §3.5–§3.7 specifies them, including a copy
of the script.

| Option | Gains | Loses | Reversibility | Cost |
|---|---|---|---|---|
| **(a) Move the contract to dotfiles (recommended)** | writer and reader in one repository with their tests (`dotfiles:tests/test_stack_cycle.py`, `test_statusline.py`); no copy of a script to keep in sync | the files stay under `stack`'s state root while `stack` does not own them; `phase2-core.md` §2.1 now reserves `cycle/` for the wrapper so no `stack` file lands there | docs only | one doc edit here, one note in dotfiles |
| (b) `stack run cycle` writes them | the state root holds only `stack`'s files; any runner gets a summary | core gains persisted state and the wrapper's validity policy (one result per named extension, a fresh `latest.json`), which fails the core conditions of `core-stages.md` §0.1; a new public behaviour of `stack run` | a written file is hard to take back | code, tests, a dotfiles change |
| (c) Keep both | no work | the drift #69 names | — | none |

Under (a), the wrapper's validation, freshness and display rules (`phase3-cycle.md` §3.6–§3.7) move with the files;
`phase3-cycle.md` keeps the extension protocol, the `observe/latest.json` contract and that the job runs
`stack run cycle --ext observe` then `--ext improve`. Recommendation (a). It would change if a reader outside dotfiles
needed the summary, or if `stack` gained its own scheduled runner. (a) changes no behaviour and is a method choice;
(b) would be a public behaviour change for the owner to decide.

## 6. Accepted side effects and rejected options

| Side effect | Why it does not harm the purpose |
|---|---|
| Ignored items get no marker in the restore; which entry point reports them is §4 | valid state and valid items still render; the report of §4 comes when the orchestrator can fix the file, and the stated shape lets a loss seen after compaction be diagnosed |
| Freezing the `.kits` format | it is already what both readers depend on; a future change gets a read command (§3.3) instead of a silent break |
| Lockless `.kits` readers see no fresh snapshot: a read racing a session's first `kit open` or last `kit close` can see either side, so the ask-owner hook may show or skip the checklist for that one question | the effect is one reminder more or less on one question |
| Steward's caller lookup through `.kits` is best-effort: when two sessions list the slug, or one closes it while steward reads, steward can pick the other session's transcript | `.kits` records kits, not callers, so no reader can do better from it; the companion PR (§8) requires steward to name the session it inspected on every lookup, or say it could not establish the caller, so an on-track line is not mistaken for an audit of the right session |
| On Windows, a reader holding the file open can make `kit open`'s rename or the last `kit close`'s remove fail (exit 2): the open may leave a new empty kit dir and no record, the close leaves the slug recorded; nothing retries | the command reports the failure and changes no other record; rerunning it succeeds once the reader has closed the file; the hook only stats the file and content readers hold it briefly (`phase2-core.md` §4.1) |
| A compact whose budget is used up before a kit's state block leaves a `[state omitted: …]` marker or no text for that kit | the loss is visible to an agent that expects the kit (no state block for it), and that agent can rerun `stack kit open <slug>`, whose output has no total budget; hiden's explicit `<orchestration-state>` check runs after `kit open` in a handover session, not after compaction; a larger budget would exceed Claude Code's hook output limit |
| The due line names a dotfiles agent | it is matched text; its meaning is stated without depending on that agent |
| A `NORTH.md`-only kit still gets `[new kit: …]` | a prompt to write `state.md` next to the state block, not a failure; `stack-audit-due` hashes the missing file as `absent` |
| The steward report is no longer accepted in this repository | `stack` never reads it; the companion PR accepts it (§8) |

| Rejected option | Reason |
|---|---|
| Add the kit dir to the due line | the line is matched text, so the change breaks its consumer; it would also put a local absolute path into merge output that may be pasted into public text, and the consumer can already resolve the dir (`core-stages.md` §4.7) |
| A `.kits` read command | §3.3 |
| `stack run cycle` writing the summary files | §5.2 (b) |

## 7. Acceptance criteria

### 7.1 Purpose layer

| ID | Acceptance condition | Source | Observable result | Check on the real CLI |
|---|---|---|---|---|
| A1 | The accepted `attention/items.json` shape, restored roles and ignored inputs are stated in `stack` and match what `kit open` renders. | #69 item 1 | a valid item rendered; each ignored case without a block | scratch `HOME`, `stack kit open demo` with each case of `phase2-core.md` §4.1.1 |
| A2 | The tags hiden inspects (`<orchestration-state>`, `<orchestrator-attention-items path=…>`) and the `NORTH.md`-only case are stated in `stack` and match both entry points. | #69 item 2 | both tags at `kit open` and after compaction; nothing on other sources | a kit with `NORTH.md` and an `orchestrator` item: `stack kit open demo`; with the kit recorded under the payload's session id (`STACK_SESSION_ID=<id>` on `kit open`), pipe a compact payload with that `session_id` to `stack hook claude session-start`, then the same payload with `"source":"startup"` (no output); the rest of the §4.2 step 7 matrix is checked by L3 |
| A3 | A reader following `phase2-core.md` §4.1 gets the recorded slugs, which `stack` filters by dir existence before it restores them. | #69 item 3 | the file lists recorded kits in order, a kit whose dir was removed included, while compaction restores only the others; the file is gone after the last close | `STACK_SESSION_ID=x stack kit open a`, `… b`, remove `b`'s dir, read the file (`a`, `b`), pipe a compact payload for `x` (only `a` restored); `… close a`, `… close b`, the file is gone |
| A4 | The due line's `<slug>` names the kit dir under the root the merge used. | #69 item 4 | as `core-stages.md` P14 | `core-stages.md` P14 |
| A5 | `stack` docs carry no steward report format or dispatch rule. | #69 item 5 | the changed docs only point to `shuymn/dotfiles` for them | read every doc this branch changes (`git diff --name-only origin/main -- docs`) for a report length, a report or evidence format, or when and how steward is started |
| A6 | Each open question has a recommendation with its condition and whether the owner decides. | #69 open questions | §5 | read §5 |

### 7.2 Plan layer

| ID | EARS claim | Source | Verified by |
|---|---|---|---|
| L1 | The renderer shall render items whose `role` is `orchestrator` or `all` and whose `rule` is a string, in file order, and render no block for a malformed file. | §3.1 | `kit.TestRenderStateAndItems` (roles, a non-string rule, a non-object element), `kit.TestRenderNewKit` (invalid JSON), `kit.TestItemsDiagnostics` (every file-level case, including a top-level array, `null` and a non-array `items`) |
| L2 | When a kit has `NORTH.md` and no `state.md`, the renderer shall render the `NORTH.md` block and the `[new kit: …]` marker. | §3.2 | `kit.TestRenderNorthOnly`; built CLI (A2) |
| L3 | Compact restore shall emit the tags and markers of `phase2-core.md` §4.2 step 6 within 9,500 UTF-16 units, and other sources nothing. | §3.2 | `session.TestWorker` (worker line, `<worker-rules>`, `<original-brief>`), `session.TestCompact` (kits, `<orchestration-board>`, `[kits omitted: .kits locked]`), `session.TestReservedMarkers` and `session.TestMultiKitReservation` (`[state omitted: …]`, `[brief omitted: …]`), `session.TestBudgetUTF16` (`[...cut N chars]`, the total), `session.TestBoards` (board not launched without room), `session.TestCompactEntry`, `session.TestCompactEntryDeadline`, `session.TestEntryOmittedMarker` (entry blocks, `<orchestration-handover>`, `[entry skipped: deadline]`, `[entry context omitted: …]`), `session.TestExport` (other sources print nothing), `session.TestRestoreMarkers` (the items block, `[kits omitted: .kits unreadable]`, `[board omitted: …]`, `[board skipped: deadline]`, `[items omitted: …]`, `[handover omitted: …]`, `[worker rules omitted: …]`), `session.TestCompactEntryMarkers` (`[entry extensions skipped: …]`, `[entry context omitted: …]` from a real entry result; C19's empty `[stages].entry` invokes no extension) |
| L4 | `kit open`/`close` shall write `<id>.kits` as LF-terminated valid slugs, de-duplicated in the order added, through a rename, and delete it when empty; `kit.Store` shall read it trimming each line and skip blank, invalid and repeated lines. | §3.3 | `kit.TestStore`, `kit.TestStoreReadsCRLFAndSkipsInvalidLines`, `kit.TestStoreTempFileIsUnique` |
| L5 | `stack-audit-due` shall answer a JSON response whose `context` is exactly the due line with the request's slug, which `pr merge` prints after `MERGED`. | §3.4 | `auditdue.TestK`, `auditdue.TestStateUnchanged` (the context), `merge.TestMergedStage` (printing after `MERGED`) |
| L6 | The `kits` sent to `merged` and `entry` shall name dirs under the configured kit root. | §3.4 | `cli.TestRequestKits` (default root), `cli.TestRelativeKitRoot` and `session.TestRelativeKitRoot` (a relative root) |
| L7 | Each contract shall be stated once, and the docs shall add no local path (the `public-text` rule of this public repository). | §1; AGENTS.md | §2 read against the sections it names; `stack-public check origin/main..HEAD` passes (POSIX and Windows home paths, private terms) |

## 8. Dotfiles companion PR (#1076)

The hiden cleanup in `shuymn/dotfiles` changes, as a consequence of this design (sections are in `shuymn/stack`):

- `skills/hiden/references/attention.md:16`: keep when to seed the orchestrator's re-read item and its wording; the
  JSON shape becomes a pointer to `docs/design/phase2-core.md` §4.1.1. `attention.md:5`: drop `worker` and
  `reviewer` as roles `stack` restores, or say that only `orchestrator` and `all` are restored.
- `attention.md:18`: "only after compaction … nothing at start, resume or clear" becomes a pointer to
  `docs/design/phase2-core.md` §4.2 step 7.
- `attention.md:14`: the kit dir is `<kit root>/<slug>` under the configured `[kit].root`, not a fixed
  `~/.local/state/stack/kits/<name>/`. `attention.md:7` and `commit-pr.md:15`: start steward with the dir of the
  line's `<slug>` found as `docs/design/core-stages.md` §4.7 "The due line" says, and point there for the line's
  meaning instead of restating it.
- `references/retro.md:14`: the `<orchestrator-attention-items>` tag and its `path` attribute become a pointer to
  `docs/design/phase2-core.md` §4.1.1; the check still matches the `path` of the same kit whose restored state named
  the step.
- `agents/steward.md`: owns its request and report format (taken out of `core-stages.md` §4.8) and keeps auditing the
  whole state file at any size (#28); its `sessions/*.kits` grep follows `docs/design/phase2-core.md` §4.1 (a
  non-empty file, one slug per line, `<id>` lower-cased in the file name, so matching it to a transcript name relies
  on lower-case session ids). That lookup is best-effort: on every lookup, steward names the session it
  inspected, or says it could not establish the caller. Its acceptance, formerly P14 here and from #28, moves
  to that PR.
- `hooks/executable_ask-owner-checklist.sh:7-8,17-33`: cite `docs/design/phase2-core.md` §4.1 as the format it relies
  on (presence by metadata only; the file exists only while it lists a slug; the id is lower-cased).
- `AGENTS.md:45`: follows the §5.1 decision.
- `scripts/stack-cycle` and the statusline: take over the cycle summary contract if §5.2 (a) is chosen.
