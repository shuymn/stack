# Kit Contracts Design: One Owner per Kit and Session Artifact

The repository whose code reads or writes an artifact owns its format. `stack` reads or writes the kit files,
`sessions/<id>.kits`, the restored text and the audit-due line in code, so their contracts are stated in this
repository. The owner's setup in `shuymn/dotfiles` (the hiden skill, the steward agent, hooks) supplies the usage
policy: when and why to use them, pointing to the owning section for the format. This note indexes the contracts,
states the warning about ignored attention items, and holds the acceptance criteria.

## 1. Where the contracts live

Each contract is stated once, in the section that designed its code, beside its EARS claims and tests; §2 lists
them, as `powershell-guard.md` §8 lists its own.

## 2. Where each contract is stated

| Artifact | Writer | Code readers | Outside readers | Contract |
|---|---|---|---|---|
| Kit dir `<kit root>/<slug>/`, `state.md`, `NORTH.md` | the orchestrator (content); `kit open` creates the dir (`internal/cli/kit.go:79-91`) | renderer (`internal/kit/render.go:104-138`), `stack-audit-due` hash (`extensions/auditdue/auditdue.go:227`) | steward reads them by instruction | `phase2-core.md` §2.1 (kit root), §4.1.1 (rendering); content and headings: hiden |
| `attention/items.json` | the orchestrator | renderer (`internal/kit/render.go:175-242`) | hiden's retrospective reads the rendered block (`shuymn/dotfiles` `home/dot_claude/skills/hiden/references/retro.md`) | `phase2-core.md` §4.1.1 "Attention items"; diagnostics: §4 |
| `bin/board` | the orchestrator, optional | renderer (`internal/kit/render.go:374-422`), compact (`internal/session/session.go:206-236`) | none | `phase2-core.md` §4.1.1, §4.2 step 2 |
| Restored text (`kit open` stdout, compact `additionalContext`) | `stack` | none | hiden checks `<orchestration-state>` after a handover and `<orchestrator-attention-items path=…>` in a retrospective | `phase2-core.md` §4.1.1 (parts, tags, `kit open`), §4.2 steps 5–7 (budget, compact output, entry points compared) |
| `sessions/<id>.kits` | `stack kit open/close` (`internal/kit/store.go:103-134,198-215`) | `kit.Store` under the lock (`internal/kit/store.go:46-77`) | the ask-owner hook (`shuymn/dotfiles` `home/dot_claude/hooks/executable_ask-owner-checklist.sh`), steward (`shuymn/dotfiles` `home/dot_claude/agents/steward.md`); steward's caller lookup is best-effort, and its instructions say how it reports the session it inspected or an unresolved caller | `phase2-core.md` §4.1 "Read-only format for external readers"; why it is read directly: §3.1 |
| The audit-due line | `stack-audit-due` (`extensions/auditdue/auditdue.go:222`), printed by `pr merge` (`internal/merge/merged.go:83-93`) | none | the orchestrator by instruction (`shuymn/dotfiles` `home/dot_claude/skills/hiden/references/attention.md`, `commit-pr.md`) | `core-stages.md` §4.7 "The due line"; why it names no dir: §3.2 |
| Steward request and report | the orchestrator; steward | none | — | `shuymn/dotfiles` (`agents/steward.md`, hiden); `core-stages.md` §4.8 only points there |
| `~/.local/state/stack/cycle/` | the dotfiles cycle wrapper | none | the dotfiles statusline | reserved in `phase2-core.md` §2.1; contract: `shuymn/dotfiles` `docs/stack-cycle.md` (§5.2) |

## 3. Decisions

### 3.1 `sessions/<id>.kits` is read directly

Keep the file interface: steward has only file tools, and the ask-owner hook checks presence by metadata alone, so
it needs no additional `stack` process on a hot path. Revisit with a read command when the file needs a new field, or when a reader that
can run commands needs the resolved kit dirs.

### 3.2 The audit-due line names no dir

Keep the line stable: its consumer matches the text, so adding the kit dir would break it, and would put a local
absolute path into merge output that may be pasted into public text. The consumer resolves the dir as
`core-stages.md` §4.7 states.

## 4. Warning about ignored attention items

Malformed items can silently remove a rule the orchestrator relies on after compaction (the re-read rule of
`phase4-jev.md` §3.2 counts only when its block appears). `kit open` reports the problem, because it is the moment
the caller sees stderr and can fix the file; when hiden seeds the file after the first `kit open` of a new kit, that
open cannot diagnose it, and a later one (a reopen or a handover) does. At compaction stdout is the restore and
stderr does not reach the agent's context, so compaction adds no item diagnostic to the restored text or stderr,
including with `STACK_DEBUG=1`. `internal/kit` reports why it ignored items, without changing what it renders;
`internal/cli/kit.go` prints it. The malformed files and elements are those `phase2-core.md` §4.1.1 ignores. Item
indexes are 0-based (the JSON array index).

| ID | EARS claim | Test |
|---|---|---|
| W1 | When `stack kit open` renders a kit whose `attention/items.json` is readable but is a malformed file (`phase2-core.md` §4.1.1), it shall write exactly one line `stack: <path>: attention items ignored: <reason>` to stderr. | `cli.TestKitOpenItemsWarning`, one case each with its reason string: invalid JSON, top-level `null`, `123`, `"x"`, `[]`, `{}` (`items` missing), `{"items": null}`, `{"items": "x"}`; control: no file → no line |
| W2 | When `items` is an array holding malformed elements (`phase2-core.md` §4.1.1), `stack kit open` shall write exactly one line `stack: <path>: <n> of <m> attention items ignored (first: item <i>: <reason>)` to stderr and still render the valid items. | same test, mixing valid `orchestrator` and `all` items with a `null` element (`{"items": [null, …]}`; a top-level `null` file is W1's), `3`, an item without `rule`, `{"role": 1, "rule": "x"}`, `{"role": "all", "rule": 2}`: asserts one line, `n`, `m`, the first index and reason, and the surviving bullets in order; `kit.TestItemsDiagnostics` for each reason |
| W3 | While the file is absent or every element is well-formed, `stack kit open` shall write nothing about the items, including for items whose role is neither `orchestrator` nor `all`, an empty `rule`, and an empty `items` array. | same test: a `worker` item (no block, silent), an empty `all` rule (`- `, silent), `{"items": []}` (no block, silent) |
| W4 | In every W1–W3 case, the warning shall affect stderr only; stdout and the exit code follow the normal `kit open` contract. | same test, comparing stdout and the exit code with fixed expectations of the renderer's output, stderr captured separately, including a number past `float64` and a repeated `items` key in another letter case, which render as Go's JSON decoding gives them |
| W5 | Session-start shall not write any item diagnostic. | `cli.TestSessionStartItems`: `cli.Run` of `hook claude session-start` with a compact payload and a malformed file, with `STACK_DEBUG` unset and set to `1`: no diagnostic on stderr or in `additionalContext` |

## 5. Related contracts

### 5.1 Improve writing guide

A target repository can supply the rules for editing its own files: `phase3-cycle.md` §3.4 step 3 and I7 state the
`writing` key of `.stack/improve.toml`, read from the pinned base commit, kept outside the editable `paths`, with the
embedded `writing.md` as the fallback when the key is absent. `shuymn/dotfiles` sets it to `.stack/writing.md`, so an
edit takes effect at the next run after it merges. Revisit the decision if a second target wants the same rules (a
shared default in `stack-improve` fits that better) or improve stops editing agent instructions; removing the
embedded fallback would change the prompt of every target without the key, so it is the owner's decision.

### 5.2 Cycle summary files

`shuymn/dotfiles` owns the cycle wrapper's summary files and their validation, freshness and display rules; see its
`docs/stack-cycle.md`. `phase2-core.md` §2.1 reserves the directory so no `stack` file lands there, and
`phase3-cycle.md` owns the extension protocol and the `observe/latest.json` contract. Revisit the decision if a
reader outside `shuymn/dotfiles` needs the summary or `stack` gains its own scheduled runner; having
`stack run cycle` write the files would be a public behaviour change for the owner to decide.

## 6. Accepted limits

| Limit | Why it does not harm the purpose |
|---|---|
| Ignored items get no marker in the restore | valid state and valid items still render; §4 reports the loss when the orchestrator can fix the file, and the stated shape lets a loss seen after compaction be diagnosed |
| A lockless `.kits` read racing a session's first `kit open` or last `kit close` can see either side (`phase2-core.md` §4.1), so the ask-owner hook may show or skip its checklist for that one question | one reminder more or less on one question |
| The due line names a dotfiles agent | it is matched text; its meaning is stated without depending on that agent |

## 7. Acceptance criteria

### 7.1 Purpose layer

| ID | Acceptance condition | Source | Observable result | Check on the real CLI |
|---|---|---|---|---|
| A1 | The accepted `attention/items.json` shape, restored roles and ignored inputs are stated in `stack` and match what `kit open` renders. | #69 item 1 | a valid item rendered; each ignored case without a block | scratch `HOME`, `stack kit open demo` with each case of `phase2-core.md` §4.1.1 |
| A2 | The tags hiden inspects (`<orchestration-state>`, `<orchestrator-attention-items path=…>`) and the `NORTH.md`-only case are stated in `stack` and match both entry points. | #69 item 2 | both tags at `kit open` and after compaction; nothing on other sources | a kit with `NORTH.md` and an `orchestrator` item: `stack kit open demo`; with the kit recorded under the payload's session id (`STACK_SESSION_ID=<id>` on `kit open`), pipe a compact payload with that `session_id` to `stack hook claude session-start`, then the same payload with `"source":"startup"` (no output); the rest of the §4.2 step 7 matrix is checked by L3 |
| A3 | A reader following `phase2-core.md` §4.1 gets the recorded slugs, which `stack` filters by dir existence before it restores them. | #69 item 3 | the file lists recorded kits in order, a kit whose dir was removed included, while compaction restores only the others; the file is gone after the last close | `STACK_SESSION_ID=x stack kit open a`, `… b`, remove `b`'s dir, read the file (`a`, `b`), pipe a compact payload for `x` (only `a` restored); `… close a`, `… close b`, the file is gone |
| A4 | The due line's `<slug>` names the kit dir under the root the merge used. | #69 item 4 | as `core-stages.md` P14 | `core-stages.md` P14 |
| A5 | `stack` docs carry no steward report format or dispatch rule. | #69 item 5 | the docs only point to `shuymn/dotfiles` for them | review the changed docs for a report length, a report or evidence format, or when and how steward is started |
| A6 | Each related artifact points to the contract maintained by its code owner, with when to revisit that decision and which changes need the owner's decision. | #69 open questions | §5 | follow the links in §5 and read each subsection's revisit and owner-decision sentence |

### 7.2 Plan layer

| ID | EARS claim | Source | Verified by |
|---|---|---|---|
| L1 | The renderer shall render the items whose `role` is exactly `orchestrator` or `all` and whose `rule` is a string, in file order, and render no block for a malformed file (`phase2-core.md` §4.1.1). | `phase2-core.md` §4.1.1 | `kit.TestRenderStateAndItems` (roles, a non-string rule, a non-object element), `kit.TestRenderNewKit` (invalid JSON), `kit.TestItemsDiagnostics` (every file-level case, including a top-level array, `null` and a non-array `items`) |
| L2 | Verify the `NORTH.md`-only case in `phase2-core.md` §4.1.1. | `phase2-core.md` §4.1.1 | `kit.TestRenderNorthOnly`; built CLI (A2) |
| L3 | Compact restore shall emit the tags and markers of `phase2-core.md` §4.2 step 6 within 9,500 UTF-16 units, and other sources nothing. | `phase2-core.md` §4.2 | `session.TestWorker` (worker line, `<worker-rules>`, `<original-brief>`), `session.TestCompact` (kits, `<orchestration-board>`, `[kits omitted: .kits locked]`), `session.TestReservedMarkers` and `session.TestMultiKitReservation` (`[state omitted: …]`, `[brief omitted: …]`), `session.TestBudgetUTF16` (`[...cut N chars]`, the total), `session.TestBoards` (board not launched without room), `session.TestCompactEntry`, `session.TestCompactEntryDeadline`, `session.TestEntryOmittedMarker` (entry blocks, `<orchestration-handover>`, `[entry skipped: deadline]`, `[entry context omitted: …]`), `session.TestExport` (other sources print nothing), `session.TestRestoreMarkers` (the items block, `[kits omitted: .kits unreadable]`, `[board omitted: …]`, `[board skipped: deadline]`, `[items omitted: …]`, `[handover omitted: …]`, `[worker rules omitted: …]`), `session.TestCompactEntryMarkers` (`[entry extensions skipped: …]`, `[entry context omitted: …]` from an injected extension response; C19's empty `[stages].entry` invokes no extension) |
| L4 | Verify the `.kits` read/write contract in `phase2-core.md` §4.1. | §3.1 | `kit.TestStore`, `kit.TestStoreReadsCRLFAndSkipsInvalidLines`, `kit.TestStoreTempFileIsUnique` |
| L5 | Verify the due-line context and its placement after `MERGED` as specified in `core-stages.md` §4.7. | §3.2 | `auditdue.TestK`, `auditdue.TestStateUnchanged` (the context), `merge.TestMergedStage` (printing after `MERGED`) |
| L6 | The `kits` sent to live `merged` and `entry` requests shall name dirs under the configured kit root. | §3.2 | `cli.TestRequestKits` (default root), `cli.TestRelativeKitRoot` and `session.TestRelativeKitRoot` (a relative root) |
| L7 | Each contract shall be stated once, and the docs shall add no local path (the `public-text` rule of this public repository). | §1; AGENTS.md | §2 read against the sections it names; `stack-public check <base>..<head>` on the review range passes (POSIX and Windows home paths, private terms) |
