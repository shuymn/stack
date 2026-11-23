# Kit Contracts Design: One Owner per Kit and Session Artifact

Design for issue #69 at `e33f018`. Rule from the issue: whoever reads or writes an artifact in code owns its format;
an artifact nobody reads in code is discarded or left to the side that uses it by hand. `stack` reads or writes the
kit files, `sessions/<id>.kits`, the restored text and the audit-due line in code, so their contracts are stated in
this repository. The owner's setup in `shuymn/dotfiles` (the hiden skill, the steward agent, hooks) says only when and
why to use them, and points here for the format. Code is cited as `path:line` at `e33f018`; dotfiles text as
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
| Restored text (`kit open` stdout, compact `additionalContext`) | `stack` | none | hiden checks `<orchestration-state>` after a handover and `<orchestrator-attention-items path=…>` in a retrospective | `phase2-core.md` §4.1.1 (tags, `kit open`), §4.2 steps 5–6 (budget, compact output) |
| `sessions/<id>.kits` | `stack kit open/close` (`internal/kit/store.go:103-134,198-215`) | `kit.Store` under the lock (`internal/kit/store.go:46-77`) | the ask-owner hook (`dotfiles:home/dot_claude/hooks/executable_ask-owner-checklist.sh:17-33`), steward (`dotfiles:home/dot_claude/agents/steward.md:9`) | `phase2-core.md` §4.1 "Read-only format for external readers" |
| `audit due (<reason>): run steward on kit <slug>` | `stack-audit-due` (`extensions/auditdue/auditdue.go:222`), printed by `pr merge` | none | the orchestrator by instruction (`dotfiles:home/dot_claude/skills/hiden/references/attention.md:7`, `commit-pr.md:15`) | `core-stages.md` §4.7 "The due line" |
| Steward request and report | the orchestrator; steward | none | — | `shuymn/dotfiles` (`agents/steward.md`, hiden); `core-stages.md` §4.8 only points there |

## 3. Decisions

### 3.1 `attention/items.json`

Stated in `phase2-core.md` §4.1.1: the accepted shape `{"items": [{"role": "…", "rule": "…"}]}`, the rendered roles
(`orchestrator` and `all`, exact and case-sensitive), file order, the verbatim rule, and every input the code
ignores today without a marker (invalid JSON, a top-level array, `items` missing, `null` or not an array, an item
that is not an object, a non-string `role` or `rule`). Items for other roles are allowed and rendered nowhere, so
hiden may keep `worker` or `reviewer` items only if something else reads them; nothing in `stack` does.

A warning at `stack kit open` is worth adding (§4): the silent case loses a rule the orchestrator relies on after
compaction (the re-read rule of `phase4-jev.md` §3.2 counts only when its block appears), and `kit open` is the one
moment the orchestrator sees stderr and can fix the file. Compaction stays silent: its stdout is the restore, and
stderr there does not reach the agent's context.

### 3.2 Restore output

Stated in `phase2-core.md` §4.1.1 (block format, every part's tag and markers, the `NORTH.md`-only case, the
`kit open` output) and §4.2 steps 5–6 (budget and compact output). Which entry point emits what:

| Text | `stack kit open <slug>` | SessionStart, `source: "compact"` | Other SessionStart sources |
|---|---|---|---|
| `<orchestration-state>` (`NORTH.md`, `state.md`), `[new kit: …]`, `<orchestrator-attention-items>` | the opened kit, stdout | each kit in `.kits` order | nothing |
| `<orchestration-handover>` | yes | once, after every kit, when at least one kit was restored | nothing |
| `<orchestration-board>` and board markers | yes | yes, while its block fits | nothing |
| `<extension-context>` and `[extension <name>: warn: …]` | each `entry` result | each `entry` result, room permitting | nothing |
| Worker line, `<worker-rules>`, `<original-brief>` | never | for a detected worker | nothing |
| `[kits omitted: …]`, `[… omitted: context limit…]`, `[...cut N chars]`, `[entry skipped: deadline]` | never | as they apply | nothing |
| Budget | uncapped, except a board's stdout (1 MiB) | 9,500 UTF-16 units in all; state and items bodies capped at 4,500; 600 per state file reserved per kit | — |

Every other source (`startup`, `resume`, `clear`, `fork`) only exports `STACK_SESSION_ID` and prints nothing
(`internal/session/session.go:88-90`). A kit with `NORTH.md` and no `state.md` renders the `NORTH.md` block and the
`[new kit: …]` marker, both at `kit open` and at compaction (checked on the built CLI).

### 3.3 `sessions/<id>.kits`

| Option | Gains | Loses | Reversibility | Cost |
|---|---|---|---|---|
| **Document a stable read-only format (chosen)** | serves both readers as they are: steward has only file tools (`dotfiles:home/dot_claude/agents/steward.md:4`), and the hook avoids process starts on a hot path (`executable_ask-owner-checklist.sh:11-12`) | the format is frozen: a change is a breaking change; readers skip the lock | a read command can be added later without changing the file | docs only |
| Add `stack kit list` (slugs, and dirs under the configured root) and switch readers | lock, validation and dir resolution in one place | steward cannot run it; the hook pays a process start per question; one more CLI surface | removing a command is breaking | code, tests, two dotfiles changes |

The format is in `phase2-core.md` §4.1. The lockless read is safe on Unix (the write is a rename); on Windows a
reader keeping the file open can make a concurrent `kit open` or `close` fail (exit 2), so readers open, read and
close at once or only stat the file. Revisit with a read command when the file needs a new field or a reader that can
run commands needs the kit dirs.

### 3.4 The audit-due line

Stated in `core-stages.md` §4.7 "The due line": its meaning (kit `<slug>` of the merging session is due an audit by
the K-merge or unchanged-state rule), that it is advisory, and that `<slug>` is a `kits` entry whose dir is
`<kit root>/<slug>` under the configured `[kit].root` (`internal/cli/run.go:300-307`, `paths.FromConfig`). A
consumer that does not read the config uses the dir `kit open` printed for that slug, never the default root. The
line keeps naming "steward": it is matched text, and changing it would break its consumer for no gain in what the
signal means. Adding the dir to the line was rejected for the same reason; the orchestrator already has the dir from
`kit open`.

### 3.5 Steward out of `stack`

`core-stages.md` §4.8 no longer states the steward report format or how it is started; it points to
`shuymn/dotfiles`. P14 now checks only what `stack` emits (the slug resolves under the configured root); the steward
report is accepted in the dotfiles companion PR (§8).

## 4. Follow-up code change: warn about ignored attention items

Not part of this PR. `internal/kit` reports why it ignored items; `internal/cli/kit.go` prints it; compaction is
unchanged.

| ID | EARS claim | Test |
|---|---|---|
| W1 | When `stack kit open` renders a kit whose `attention/items.json` is readable but is not a JSON object whose `items` is an array, it shall write one line `stack: <path>: attention items ignored: <reason>` to stderr. | `cli.TestKitOpenItemsWarning`: invalid JSON, top-level array, `items` missing, `null`, a string |
| W2 | When `items` holds elements that are not objects or whose `role` or `rule` is not a string, `stack kit open` shall write one line `stack: <path>: <n> of <m> attention items ignored (first: item <i>: <reason>)` to stderr and still render the valid items. | same test; `kit.TestItemsDiagnostics` for the reasons |
| W3 | While every item is well-formed, including items whose role is neither `orchestrator` nor `all`, `stack kit open` shall write nothing about the items. | same test: a `worker` item and an empty `rule` |
| W4 | `stack kit open` shall keep its stdout byte-identical and its exit code unchanged in W1–W3. | same test, comparing with the output before the change |
| W5 | Session-start shall not write any item diagnostic, with or without debug output. | `session.TestCompact` with a malformed file |

## 5. Open questions

### 5.1 Where `writing.md` lives

`extensions/improve/writing.md` holds rules for editing the owner's skills and agents (it names hiden, `wasure`,
`home/dot_claude/**` and a dotfiles skill), and `stack-improve` embeds it in its prompt
(`extensions/improve/model.go:20-21,205`). Dotfiles editors read it from this repository
(`dotfiles:AGENTS.md:45`), and every edit needs a `stack-improve` reinstall.

| Option | Gains | Loses | Reversibility | Cost |
|---|---|---|---|---|
| (a) Keep it embedded | improve's prompt is self-contained; no new config | a generic extension carries one target's rules; edits cross repositories and need a reinstall; the target's editors read another repository | — | none |
| **(b) The target supplies it (recommended):** an optional `writing = "<path>"` in `.stack/improve.toml`, read with `git show <base OID>:<path>` as `.stack/improve.toml` is (`extensions/improve/improve.go:358-367`); absent key → no writing section; a missing or unreadable file, or a path that `paths` matches (improve could rewrite its own rules) → `warn` and stop, as an invalid `.stack/improve.toml` does | the repository whose files are edited owns their rules; an edit takes effect at the next run; `stack-improve` names no target | an optional field in improve's repository contract; a small code change and tests | the key can be dropped and the file embedded again | one `stack` PR (config field, read, tests, I7 updated) and one dotfiles PR (move the file, set the key, update `AGENTS.md:45`) |
| (c) Point the model at a file in the worktree by name | no config | the model may not read it; nothing checks it | easy | small |

Recommendation (b). It would change if a second target repository wanted the same rules (then a shared default in
`stack-improve` is the better place), or if improve stopped editing agent instructions. The proposals improve makes
for the owner do not change (same rules, read from another place), so this is a method choice; it adds an optional,
evolution-compatible field to `stack-improve`'s repository contract.

### 5.2 The cycle summary files

`~/.local/state/stack/cycle/last.json` and `last-error` are written by `dotfiles:scripts/stack-cycle` and read by the
dotfiles statusline; no `stack` code touches them, yet `phase3-cycle.md` §3.5–§3.7 specifies them, including a copy
of the script.

| Option | Gains | Loses | Reversibility | Cost |
|---|---|---|---|---|
| **(a) Move the contract to dotfiles (recommended):** `phase3-cycle.md` keeps that the job runs `stack run cycle --ext observe` then `--ext improve` and what makes a run valid at the stack boundary (exit code, one `cycle` object, a self-answered result), and points to the wrapper for the files; the script copy and the statusline rules go | writer and reader in one repository with their tests (`dotfiles:tests/test_stack_cycle.py`, `test_statusline.py`); no copy of a script to keep in sync | the files stay under `stack`'s state root while `stack` does not own them (a later `stack` file there must avoid `cycle/`) | docs only | one doc edit here, one note in dotfiles |
| (b) `stack run cycle` writes them | the state root holds only `stack`'s files; any runner gets a summary | core gains persisted state and the wrapper's validity policy (one result per named extension, a fresh `latest.json`), which fails the core conditions of `core-stages.md` §0.1; a new public behaviour of `stack run` | a written file is hard to take back | code, tests, a dotfiles change |
| (c) Keep both | no work | the drift #69 names | — | none |

Recommendation (a). It would change if a reader outside dotfiles needed the summary, or if `stack` gained its own
scheduled runner. (a) changes no behaviour and is a method choice; (b) would be a public behaviour change for the
owner to decide.

## 6. Accepted side effects

| Side effect | Why it does not harm the purpose |
|---|---|
| Ignored items stay silent until §4 lands | the restore still delivers state and every valid item; the shape is now stated, so a wrong file is found by reading the contract |
| Freezing the `.kits` format | it is already what both readers depend on; a future change gets a read command (§3.3) instead of a silent break |
| Lockless `.kits` readers see a list that may be one update old, and on Windows can make a concurrent `kit open`/`close` exit 2 | the hook only gates a reminder and steward picks a transcript by heuristic, so a one-update lag changes neither; the failed command changes nothing and is retried |
| The due line names a dotfiles agent | it is matched text; its meaning is stated without depending on that agent |
| A `NORTH.md`-only kit still gets `[new kit: …]` | a prompt to write `state.md`, not a failure; `stack-audit-due` hashes the missing file as `absent` |
| The steward report is no longer accepted in this repository | `stack` never reads it; the dotfiles companion PR accepts it (§8) |

## 7. Acceptance criteria

### 7.1 Purpose layer

| ID | Acceptance condition | Source | Observable result | Check on the real CLI |
|---|---|---|---|---|
| A1 | The accepted `attention/items.json` shape, rendered roles and ignored inputs are stated in `stack` and match what `kit open` renders. | #69 item 1 | a valid item rendered; each ignored case without a block | scratch `HOME`, `stack kit open demo` with each case of `phase2-core.md` §4.1.1 |
| A2 | Every tag and marker hiden inspects, and which entry point emits it, is stated in `stack` and matches both entry points, including the `NORTH.md`-only case. | #69 item 2 | the §3.2 table holds | `stack kit open demo` on a `NORTH.md`-only kit; pipe `{"session_id":…,"source":"compact"}` to `stack hook claude session-start` and to it with `"source":"startup"` (no output) |
| A3 | A reader following `phase2-core.md` §4.1 gets the slugs `stack` restores. | #69 item 3 | the file lists open kits in order; it is gone after the last close | `STACK_SESSION_ID=x stack kit open a`, `… b`, `… close a`, read the file; close `b`, the file is gone |
| A4 | The due line's `<slug>` names a kit dir under the configured kit root. | #69 item 4 | the line's slug and the dir `kit open` printed | `core-stages.md` P14 |
| A5 | `stack` docs carry no steward report format or dispatch rule. | #69 item 5 | — | `rg -n 'on-track' docs` finds nothing |
| A6 | Each open question has a recommendation with its condition and whether the owner decides. | #69 open questions | §5 | read §5 |

### 7.2 Plan layer

| ID | EARS claim | Source | Verified by |
|---|---|---|---|
| L1 | The renderer shall render items whose `role` is `orchestrator` or `all` and whose `rule` is a string, in file order, and render no block for a malformed file. | §3.1 | `kit.TestRenderStateAndItems`, `kit.TestRenderNewKit` (no test yet for a top-level array or a non-array `items`: added with §4) |
| L2 | When a kit has `NORTH.md` and no `state.md`, the renderer shall render the `NORTH.md` block and the `[new kit: …]` marker. | §3.2 | built CLI (A2); `kit.TestRender` covers `NORTH.md` with an unreadable `state.md` only; a unit test is added with §4 |
| L3 | Compact restore shall emit the tags and markers of `phase2-core.md` §4.2 step 6 within 9,500 UTF-16 units, and other sources nothing. | §3.2 | `session.TestCompact`, `session.TestWorker`, `session.TestBudgetUTF16`, `session.TestReservedMarkers`, `session.TestExport` |
| L4 | `kit open`/`close` shall write `<id>.kits` as LF-terminated valid slugs, de-duplicated in the order added, through a rename, and delete it when empty; readers shall skip blank, invalid and repeated lines and CR. | §3.3 | `kit.TestStore`, `kit.TestStoreReadsCRLFAndSkipsInvalidLines`, `kit.TestStoreTempFileIsUnique` |
| L5 | `stack-audit-due` shall print exactly `audit due (<reason>): run steward on kit <slug>` with the request's slug. | §3.4 | `auditdue.TestK`, `auditdue.TestStateUnchanged` |
| L6 | The `kits` sent to `merged` shall name dirs under the configured kit root. | §3.4 | `cli.TestRequestKits`, `session.TestRelativeKitRoot` |
| L7 | The docs shall state each contract once and contain no new local path. | §1 | `rg -n '/Users/\|/home/' docs` shows no new hit; §2 index read against the sections it names |

## 8. Dotfiles companion PR (#1076)

The hiden cleanup in `shuymn/dotfiles` changes, as a consequence of this design:

- `skills/hiden/references/attention.md:16`: keep when to seed the orchestrator's re-read item and its wording; the
  JSON shape becomes a pointer to `stack` `docs/design/phase2-core.md` §4.1.1. `attention.md:5`: drop `worker` and
  `reviewer` as roles `stack` restores, or say that only `orchestrator` and `all` are restored.
- `attention.md:18`: "only after compaction … nothing at start, resume or clear" becomes a pointer to
  `docs/design/kit-contracts.md` §3.2.
- `attention.md:14`: the kit dir is the one `stack kit open` prints (configured `[kit].root`), not a fixed
  `~/.local/state/stack/kits/<name>/`; `attention.md:7`: start steward with that dir for the line's `<slug>`, and
  point to `docs/design/core-stages.md` §4.7 for the line's meaning instead of restating it. Same for
  `commit-pr.md:15`.
- `references/retro.md:14`: the `<orchestrator-attention-items>` tag and its `path` attribute become a pointer to
  `phase2-core.md` §4.1.1.
- `agents/steward.md`: owns its request and report format (taken out of `core-stages.md` §4.8); its
  `sessions/*.kits` grep follows `phase2-core.md` §4.1 (a non-empty file, one slug per line); its acceptance (the
  former P14 steward part) moves to that PR.
- `hooks/executable_ask-owner-checklist.sh:7-8,17-33`: cite `phase2-core.md` §4.1 as the format it relies on (the
  file exists only while it lists a kit; the id is lower-cased).
- `AGENTS.md:45`: follows the §5.1 decision.
