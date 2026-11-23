# Phase 4 Design: Jev Extensions (`jev-semantic`, `jev-router`, `jev-classify`)

Design for issue #57: the optional Jev extensions of `docs/architecture.md` §4, on its stage table and the §3
protocol, at `668ceb6`. Code is cited as `path:line`; Jev facts by URL, and what no primary page confirms is marked
**assumption** (checked with a real key, §7). Code lives under `extensions/`, fixtures under `fixtures/jev/`; core
(`cmd/stack`, `internal/`) does not change.

## 1. Purpose and scope

Jev is TypeSafe's "System One" model: it answers typed questions about one `state` with calibrated probabilities and
writes no text. Three advisory extensions use it: `jev-semantic` flags two kinds of PR risk at `outbound`,
`jev-router` is deferred (§3.2), and `jev-classify` labels the week's owner replies at `cycle`. The extensions are
optional and advisory.

Phase 4 covers the extension code, shared client, calibration, keyless replay, usage ledger and caller wiring (§5).
The hiden kit convention for the compaction attention item (§3.2) is tracked separately in `shuymn/dotfiles`.

**Out of scope:** core changes; any gate, redaction, cut or limit on what is sent (owner decision: the key is the opt-in, so
provider limits only shape batches); spending caps (cost is observed, as improve's); `box`, images, long context;
the Workers AI and OpenRouter routes; `[merge].message_checks` wiring; improve's evidence (public only,
`docs/design/phase3-cycle.md` §3.4 step 6; classify stays report-only).

## 2. Backend facts

| Fact | Source |
|---|---|
| `POST https://api.typesafe.ai/v1/systemone`, `Authorization: Bearer <key>`, JSON body `model` (required), `state` (string, object or array), `questions` (name → question, ≥ 1), optional `assets` | https://docs.typesafe.ai/api/system-one.md |
| Question `type` `noul` (optional `criteria` `{"true": …, "false": …}`), `choice` (`criteria` label → description, 1–255), `score` (`criteria` array of 1–10 levels); `instructions` string or structure; answers are keyed by the submitted question names | same |
| Answers: `noul` → `{"type","noul": p}` (no confidence); `choice` → `choice`, `confidence`, `probabilities`; top-level `model` (the versioned ID that answered) and `usage` `{input_tokens, output_tokens}` | same |
| Pinned IDs `jev-1.14.0` (newest, from 2026-10-14) and `jev-1.13.0`; aliases `jev-latest` and `jev-preview` move; "send that version's ID" when thresholds are tuned; no deprecation policy stated | https://docs.typesafe.ai/models.md |
| 64k tokens per request (state + all questions), 32k per question (state + that question), no cap on the question count; header `TypeSafe-Danger-Zone-Long-Context: true` raises both to 262k, "slower and less accurate" (not used); SDKs honour `retry-after` | same |
| $0.042 per 1M input tokens; output free | same; https://developers.cloudflare.com/ai/models/typesafe/jev/ |
| Body ≤ 32 MiB; 400 (one form is `max_tokens_exceeded`: "longer than the model's context length"), 401, 413, 415, 422 (`detail` array), 429, 529; back off on 429/529 | https://docs.typesafe.ai/api/errors.md |
| One state with N questions is billed once; one benchmark of 13 questions over a ~54k-char article: 0.24 s, $0.000497, run-to-run std dev ≤ 0.015 | https://docs.typesafe.ai/cookbooks/parallel_questions.md |

**Prior art** (another project's practice, not a backend fact; https://github.com/mizchi/jev-lint, its
`docs/reference.md`): it pins `jev-1.14.0`, fits "at or above" cutoffs per rule and replays verdicts with no key, as
§3.4–§3.5 do; its measured pass-to-pass score spread (median 0.010, p90 0.050, max 0.300) is the scale of §3.5's
δ = 0.05.

**Assumptions:** 70–500 ms per call (#57; only the benchmark above is published); an over-long request's 400 body
contains `max_tokens_exceeded` (the page names the error, not its field); 4 requests in flight stay under the rate
limits. **Sizing estimate** (not a bound): about one token per Japanese character and per 3–4 bytes of code, so
48 KiB of encoded state is roughly 11–16k tokens, half the per-question limit.

## 3. Decisions

### 3.1 Opt-out, failure, batching and budgets (item 1)

Every `jev-*` reads stdin to EOF first. Then:

| Condition | Answer | Network |
|---|---|---|
| Stdin is not one JSON object with a string `stage` | `warn`, finding `jev-error` "invalid request", context the same line | none |
| `stage` is not the extension's own | `pass` (as `extensions/public/stage.go:95`) | none |
| Live request (no `jev` field) and no key (see Key below) | `pass`, no context or writes | none |
| Live request (no `jev` field), no usable `TYPESAFE_API_KEY`, so the lookup falls back to an unusable key file (see Key below) | `warn`, finding `jev-error` (`detail`: the fixed cause) and a context line giving the cause and fix. No key content or ledger line. | none |
| Nothing to ask (outbound: empty `range` or a diff with no files, since both questions need the diff; entry: no kit or no candidate; cycle: no `owner-reply` item) | `pass`; no context, except cycle, which writes its empty report and names any coverage gap (a failed observe source, a disabled decision) in its context, §3.2 | none |
| Preparation fails (semantic: a git command exits non-zero or its output is cut) | `warn`, finding `jev-error` naming the command and exit | none |
| Preparation reaches the work deadline (a slow git) | the questions are unassessed (`preparation deadline`) | none |
| Connection or TLS failure, timeout, HTTP 429, HTTP 500 or higher (including 529), HTTP 413, or HTTP 400 naming `max_tokens_exceeded` | Questions are unassessed; one stderr line per cause, `jev: <n> requests skipped: <cause>`. | no retry or re-split |
| Other non-200, invalid §2 response envelope, model mismatch, or response over 1 MiB | `warn` with `jev-error`, the fixed cause or HTTP status, and the action. For 401 or 403, check the key source described below. | redirects are never followed |
| Missing or malformed answer, including a probability outside finite [0, 1] | `warn` with `jev-error` for that question. Valid answers still decide. | — |

Non-200 responses are classified at the headers, with one exception. For HTTP 400, inspect up to 64 KiB of the body
for `max_tokens_exceeded`, including bytes received before an early read error. See the
[TypeSafe error reference](https://docs.typesafe.ai/api/errors#errors).

**Unassessed** work never changes the status: it is a finding `jev-unassessed` (`detail`: which questions, how
much, why) and a context line (the router: the finding only, §3.2). A `warn` means the owner must act (a wrong key, a changed API, a retired pin, a git
failure), so an outage does not cry wolf; it loses that run's advice. Details are fixed strings and the HTTP status,
never the body, which can quote input.

**Key.** A non-blank `TYPESAFE_API_KEY` takes precedence and skips the file lookup. Otherwise use
`~/.config/stack/jev/key` and trim surrounding whitespace. The file, or its symlink target, must be readable, regular,
at most 4 KiB and non-blank. A dangling symlink is an unusable file. On Unix the file must be owned by the current user
and have no group or other permission bits. The check covers the file's mode and owner and excludes ACLs and parent
directories. On Windows the resolver relies on profile ACLs and performs no mode or owner check. A stage with no usable
environment value and an absent key file or unknown home directory opts out. `jev-calibrate` exits 2 for a missing or
unusable key or unknown home directory. Replay skips key lookup (§3.4). A `null` `jev` field is an invalid replay and
produces `jev-error` without a live call. See [key setup and guard coverage](../../README.md#install).

**Batching.** Provider limits shape batches and never drop input. Packing is decided before sending, from encoded
sizes alone, by one function in `extensions/internal/jev` (also used by §3.6 and the tests). Each request carries
the fixed fields its questions need (semantic: the texts) and units in a fixed order (diff files, kits, items),
packed greedily up to a unit budget of 48 KiB less the encoded fixed fields; a larger unit is split by lines (a
longer line at UTF-8 boundaries) into parts marked `part k of n`, each measured in its complete encoded state.
Input that fits one request whole is sent whole, whatever the budget. Only when splitting is needed: a budget
under 1 KiB, or a smallest non-empty part that cannot fit, makes the questions that need it unassessed. Questions stay ≤ 16 KiB per request; more go to another request on the same state, and a question over 16 KiB by itself stays unasked. Every body is thus
determined by the input alone. Up to 4 requests are in flight; results are assembled in unit order.

**Unit-question mode** (classify; `extensions/internal/jev/pack.go` gains it). `Unit` gains `Fields` (further keys of
the unit's element, encoded in order after its name, for classify's `owner`, `assistant` and `reply`) and
`Questions`, and `Input` gains `Whole`. With `Whole`, a unit is never split: one that does not fit a request whole,
with the fixed fields, is unpacked and its questions are unassessed (`too large`), so the 1 KiB and smallest-part
rules above do not apply. A unit's questions are asked only on the request carrying it, and a request closes when
its next unit would pass either the 48 KiB state budget or the 16 KiB question budget, so no state is sent twice.
An input uses either shared questions (semantic, the mode above, unchanged) or unit questions, never both.

**Budgets.** Each extension's budget runs from process start: router 4 s, semantic 10 s, classify 5 min. The work
deadline is the process budget less a finalisation reserve (router 0.3 s, semantic 0.5 s, classify 20 s). Requests
end 0.1 s before it to leave room for the bounded ledger append (§3.3). At the deadline, remaining work is unassessed.
The reserve covers the response and classify's file. Children get a 0.2 s pipe wait and a timeout 0.2 s before the
work deadline, so killing git and reaping its pipes fit inside the budget. The response stays under 512 KiB, half core's 1 MiB stdout limit
(`internal/ext/ext.go:211`): further findings become one `jev-omitted` count, context lines are cut with a marker.
Core's stage deadlines and its synthetic `warn` for a child it cuts, which differs from a self-answered partial
result, are separate: `docs/architecture.md` §3, with the deadline of entry at compaction in `core-stages.md` §4.3.
In a run of the default outbound selection (`public`, then `jev-semantic`), semantic gets what `public` leaves of the
120 s window, and `public` stops its scan 100 s after its own start (`extensions/public/stage.go:29-40`). hiden's
caller runs `--ext jev-semantic` alone (below), so semantic starts at the beginning of the window. At compaction the
router's stderr is discarded.

### 3.2 What each extension sends and returns (item 2)

All requests use `model: "jev-1.14.0"`; generated metadata in them has no wall-clock time or local absolute path.

**`jev-semantic` (outbound).** Read from `repo` (the cwd when `""`) with two git commands: the messages of
`<range>`, one per commit in Git log order, and the patch of `<range>` with three lines of context, `a/`/`b/` prefixes,
full blob ids, no renames and no external diff or text conversion, both LF-normalised. Git runs with the
repository-location variables dropped (as `extensions/internal/proc/repovars.go`), system, global and
environment-supplied config off, and command-line overrides that beat repository-local config, so a body depends
only on the repository's content and `<range>` for the configuration variants C6 exercises. Inputs outside them
are not neutralised: a `.gitattributes` diff driver with its own `xfuncname`, for example, changes the hunk headers.
The exact arguments and overrides are in `extensions/jev/semantic/git.go`. Fixed fields are `title`, `body`, `branch` and `commits`; units are the diff's
files in diff order. Two `noul` questions:

- `meta_duty_regression`. Instructions: "Does `diff` remove or weaken a duty the project performs on itself,
  without `title`, `body` or `commits` stating and justifying that change?" Criteria true: "deletes or loosens a
  test assertion, skips or deletes a test, drops a step of the completion check (`scripts/test`, hooks, CI), removes
  or narrows a safety rule or deny, adds `--no-verify` or error suppression, or turns a must into a may in agent
  instructions (`AGENTS.md`, `CLAUDE.md`, skills), and the texts say nothing of it"; false: "no such change, or the
  texts state it and why". The duty list is `docs/architecture.md` §1 (hard boundaries), `core-stages.md` §0.4
  ("nothing … loses behaviour between PRs") and "do not weaken assertions, skip tests or suppress errors". Examples:
  an assertion deleted under "refactor: simplify tests" (true); the same with body "drop the duplicate assertion;
  `TestX` covers it" (false). The judgment is local: asked on every packed request; a hit names its files.
- `unsupported_claim`. Instructions: "Do `title`, `body` or `commits` claim something `diff` does not support?"
  Criteria true: "claims tests, docs or files the diff does not touch, an issue fixed by code that is not changed,
  verification or a measured number with no evidence in the texts or the diff, or 'no behaviour change' where the
  diff changes behaviour"; false: "every claim is visible in the diff or carries its evidence". Examples: "add tests
  for X" with no `_test.go` change (true); "rename Foo to Bar" with exactly that rename (false). A PR-wide absence
  question: **it is asked only when the texts and the whole diff fit one request**, which then carries both
  questions; otherwise, or when that request comes back too large, it is unassessed, never judged on a part.

Answer: each `p ≥ cutoff` adds a finding `{"kind":"jev-meta-duty"|"jev-unsupported-claim","where":"<files or
\"message\">","detail":"p=0.83 ≥ 0.71 (jev-1.14.0)"}` and a context line; any hit → `warn`, else `pass`. Never
`block`: the answer is probabilistic and coarse, which is not a hard boundary (`docs/architecture.md` §1).

**Supported caller.** The hiden `commit-pr` step in `shuymn/dotfiles`
(`home/dot_claude/skills/hiden/references/commit-pr.md`, step 2) owns the command and how its result is reported. On
a PR create or edit, after `stack-public check` passed and when `stack-jev-semantic` is on `PATH`, it sends
`<merge-base>..<pinned head>` with the same title, body and branch inputs. The merge-base range is the comparison
GitHub shows for a PR, so a test that only the base added does not show as deleted; with no merge base the caller
reports that semantic was skipped. The agent reports the findings as advice, which never stops the push or the
PR; `stack-public check` still gates both. CI is not wired (it would need the key as a secret). The caller is agent
instruction text without an automated test; its merge-base step is checked by O2 and the extension side by the
diverged-base fixture (P2).

**`jev-router` (entry).** Deferred until the trigger below. Hiden already routes by step type and calls for
re-reading the reference after compaction. The router targets a missed re-read of a single step named in restored kit
state. Both `kit open` and compaction provide kit state without a prompt. Observe's window-wide `reference-unread`
signal cannot establish whether the reference was read before that step.

**Prerequisite.** A kit counts only when compaction restores, in that kit's `orchestrator-attention-items`
block, the orchestrator rule to re-read, after compaction, the hiden reference of the step in progress. hiden
supplies and words the rule; [`phase2-core.md` §4.1.1](phase2-core.md#411-kit-context-renderer-shared-with-compact-restore)
defines the format. **Trigger:** two distinct
cases, recorded in two separate retrospectives, of a step run after compaction in such a kit whose rule lives in that
step's hiden reference, where that reference was not read between the compact boundary and the step's first action (a read after that
action still counts as a miss), and where the kit state at the boundary names that step in progress (a miss the
router would answer `none` to does not count). Each case is a review-ledger row naming the session, the compact
boundary, the expected reference and the step's first action (transcript positions); the planned dotfiles change
adds this check to the hiden retrospective so a miss with no other deviation is still found. The owner asking also resumes it. Until then `[stages].entry` keeps naming `jev-router` and the absent
binary is skipped (`internal/ext/ext.go:120-122`). When built, it targets that case: one pointer per kit, to the
reference of the step in progress, `none` when the state does not identify one.

- *Candidates.* The `.md` files directly in `skills/hiden/references/` under the Claude configuration directory
  (`$CLAUDE_CONFIG_DIR`, else `.claude` joined to `os.UserHomeDir()`, so no home path is written in code and a
  Windows profile resolves the same way), sorted by name, labelled `ref_<n>` in that order. Only hiden's: its table
  routes by kind of step, which is what a kit's state can name; another skill's references are routed by that
  skill's own `SKILL.md` once it is invoked, which no kit state shows. There is no `jev.toml` and no entry in
  `docs/architecture.md` §5; a second routed skill would add one (its globs; missing → defaults, broken → `warn` and
  defaults). Each candidate's description is its first heading (without `#`) and first paragraph, LF, ≤ 400 bytes at
  a UTF-8 boundary; it describes the candidate, it is not evidence. No directory or no file → `pass`, no call.
- *State and question.* `jev.Request` gains `kits` (`[{slug, dir}]`). One request per kit, in request order: state
  `{slug, north, state}` with `NORTH.md` and `state.md` (`internal/kit/render.go:35`) read whole and LF-normalised,
  a missing file `""`; the kit `dir` is used to read, never sent. `prompt` is never sent (core sends none; a `stack
  run entry --prompt-file` run is scored on its kits alone). Kits reach the router only with a valid
  `STACK_SESSION_ID` (`internal/cli/run.go:260-264`); `kit open` without one sends `kits: []`. One `choice` question
  `current_ref`, instructions "Which reference covers the step in progress on this kit, as its state names it?",
  criteria `ref_<n>` → "`<file>`: <description>" and `none` → "the state names no single step in progress (done,
  waiting, a conditional wait, or several workstreams with none marked in progress), or no listed reference covers
  it". A kit whose state does not fit one request is unassessed, never split (the step in progress is one line among
  the parts), an exception to §3.1's splitting; a question over 16 KiB is unasked and unassessed.
- *Decision.* A kit's pointer is its `choice` when that is not `none` and `probabilities[choice]` ≥ the cutoff; one
  decision function serves runtime and calibration (§3.5).
- *Output.* Pointers in kit order, as context:

  ```text
  jev-router (jev-1.14.0): reference for each kit's step in progress; re-read it before continuing.
  - <slug>: <Claude config dir>/skills/hiden/references/review.md (0.82): <first heading>
  ```

  The whole context, header included, stays within 1,000 UTF-16 units (as `internal/session/budget.go:14-16`
  counts). A pointer line over 300 units drops its heading; pointers are added in kit order while the total fits, and
  the first that does not fit is omitted with every later one, the header then ending `(k omitted: size)`. With a
  pointer and some kits unassessed, the header ends `(n of m kits not scored)`; with both, `(k omitted: size)` comes
  first. Paths are the absolute local paths
  read and are never cut in the router's output. The router bounds its lines itself, so it writes them through a
  no-cut line method that `jev.Reply` gains, not through the shared 1,000-byte line `cut`
  (`extensions/internal/jev/protocol.go:31`). No pointer → `pass`, no context. Unassessed kits (deadline,
  transient, too large) are the `jev-unassessed` finding only: entry context is the agent's working text, and "not
  scored" gives it nothing to do; a persistent failure still answers `warn` with its context line, since the owner
  must act.
- *Placement.* `kit open` prints the kit before the router result. On compaction, core restores the kit, boards and
  handover first, then allocates space for the router and displays it between the boards and handover. The router
  never displaces restored content. Core may show the block whole, cut it, show an omission marker or log the drop,
  per [core entry placement](core-stages.md#43-entry-at-compaction). A cut can end inside a path.
- *Deadlines.* Use the router and core deadlines in §3.1. If the router's own deadline comes first, it returns any
  pointers already found. Core's shorter deadline produces its timeout `warn`. If core skips entry before launch,
  `[entry skipped: deadline]` appears when it fits.

**`jev-classify` (cycle).** Report-only: it reads the week's owner replies, labels them and writes a local report;
it never blocks and never feeds improve.

*Input.* The observe file (`.local/state/stack/observe/latest.json` under the home directory). Missing, or stale
(`window.until` more than 24 h before classify's clock) → `pass` saying so, no write. Unparsable, unknown `version`,
or `window.until` more than 5 min after the clock (as improve treats a future window) → `warn`. Its `owner-reply`
items follow observe's rule (a reply of at most 300 characters after an assistant turn,
`extensions/observe/transcripts.go:369-375`, `sources.go:93-104`). One observed week held 932 (674 T3 on 67
threads, 258 transcript in 19 files; 92% Japanese), the scale this section is sized for.

*Resolution.* Sources are read before any request, each once, and every target is checked against observe's item:
the reply's text, whitespace-collapsed and cut as observe cuts it (160 characters), must equal the item's `excerpt`,
and its timestamp, truncated to the second, the item's `ts`; otherwise the item is `unresolved` (`record changed`).
All 932 observed items passed this check. Both readers sort records into the same kinds, with observe's owner
provenance:

- `path:line` (Claude transcripts), split on the last `:`. Lines are numbered as observe numbers them: from 1,
  newline-terminated, a line over 16 MiB skipped but counted, a final unterminated line not counted
  (`extensions/observe/transcripts.go:214-254`). A *prompt* is a user record whose content is a string and that is
  not meta or a tool result; a compaction summary (`isCompactSummary`) with string content is a prompt even when it is
  also marked meta. A prompt is an *owner turn* when it also passes observe's typed-command
  filter (`typedText`, `extensions/observe/transcripts.go:525-540`) and its origin is `human`, as observe counts
  owners (`transcripts.go:360-372`). A task notification, a typed command, a compaction summary or a prompt of another origin stays a
  prompt. Any other user record is a *tool result*. An assistant record is *text* when it has `text` blocks, else a
  *tool call*.
- `t3:<thread>:<message>`. Thread IDs contain `:` (205 of the week's 674 locators), and T3 message IDs can too
  (owner-created rows observed are colon-free UUIDs; 7 server-created user rows are not), so no split point is
  assumed. One `sqlite3 -readonly -json` query per run reads the user rows in observe's window (observe's own
  bounds, `sources.go:114-121`); classify forms each row's locator in Go with observe's own function, `t3:` +
  `thread_id` + `:` + `jsonScalar(payload.id)` (a string as is, a number in shortest decimal, `null` as empty,
  `sources.go:124-135`; moved to a package both import, so producer and consumer cannot drift), and matches it to the item: 674 of 674 matched once; two rows forming one locator make the
  item `unresolved` (`ambiguous locator`). One query per matched thread then reads that thread's rows ordered by
  `created_at`, then `message_id`. A user row is an *owner turn* only under observe's rule (`createdBy` `user`,
  `creationSource` `web` or `mobile`, no `idleCompaction`, not a `/compact` command, `sources.go:84-90`); another
  `createdBy` `user` row (server-created, a compaction command) is a *prompt*; an agent-created row a *tool result*.
  An assistant row is *text* when its `text` is non-empty, else a *tool call*. One more query per run gives each
  thread's cwd (its `worktreePath`, else its project's `workspace_root`; **assumption**, the observed schema). The
  database is opened read-only as observe opens it.

*Turn boundary*, the same for both, walking back from the reply:

1. Until the first assistant record (text or tool call), only tool results and owner turns are passed over (the
   owner wrote twice in a row, and each reply is an item of its own). Any other prompt met there ends the walk:
   `unresolved` (`intervening prompt`), since the reply may answer the turn that prompt opened, not the older one.
2. From that record, the walk collects the turn back to its opener, the nearest prompt or owner turn. `assistant` is
   the turn's closing message: its last run of text records, back to the nearest tool call or tool result. A turn
   with no text record is `unresolved` (`no assistant text`); the walk never crosses the opener into an older turn.
3. `owner` is the opener's text when the opener is an owner turn, and `""` when it is another prompt or the source
   begins before any opener; the item's `opener` (`owner`, `prompt`, `compaction`, `none`) says which, so a reader
   can tell an answer whose opening instruction was compacted away from one that had none. A compaction summary is
   thus a boundary: the walk never carries a pre-compaction instruction into a post-compaction turn.

Outcomes: a missing file, database, thread or record → `unresolved` (`source gone`); the per-run locator query
failing → every T3 item `unresolved` (`sqlite3 failed`); a thread query failing → that thread's items only; the cwd
query failing → T3 items still classified, their duplicate check `unassessed` (`cwd unknown`); transcripts are
unaffected by T3. The week: 921 resolved; 8 `intervening prompt` (4 per source) and 3 `no assistant text` (turns
of tool calls and thinking only); 81 with `owner: ""` (42 T3 turns with no row before them that opens a turn, 39
transcript turns opened by a task notification, a compaction summary (3) or another non-owner prompt); closing messages median 1.3 KiB,
largest 18 KiB; owner turns median 95 B, largest 6.6 KiB. Anything the walk cannot attribute is `unresolved` with
its reason, never attached to another turn.

*Classification.* Units are the resolved items `{id, owner, assistant, reply}`, newest first by observe's `ts`
(ties in observe's item order); a deadline cut thus loses the oldest replies. The sent `id` is `r<n>`, the item's
position in that order, so generated metadata in a body holds no local path or observe ID and the bodies are the
same under another home directory; the reply, owner and assistant texts are sent whole and as written, paths
included (the key is the opt-in, §1). No text is cut to a byte bound: a scope rule at the end of a long owner turn
decides drift as much as one at its start, and sending the 22 owner turns over 2 KiB whole costs about 100 KiB a
week. The one selection is the closing message, not the whole turn (§4). Each item has its own `choice` question
`kind_r<n>`, asked only on the request carrying it (§3.1 unit-question mode): instructions "What does reply `r<n>`
in `items` do, given the `assistant` message it answers and the `owner` turn before that message?", criteria

- `correction`: "says the agent's approach, assumption, reading of the task or claim was wrong, or states a standing
  rule for how the agent should work";
- `drift`: "brings the agent back to the scope or goal the owner set (see `owner`), after `assistant` went past or
  away from it";
- `approval`: "accepts, permits, picks an offered option or acknowledges a result (OK, merged, CI passed)";
- `request`: "asks a question, reports a status or a defect in what was built, or gives a new task or idea, without
  saying the agent worked wrongly";
- `noise`: "not about the agent's work: an input test, a lone ID, hash, link or attachment, a fragment".

`owner` is what separates drift from a new request: the same `assistant` and reply are drift when the owner had
ruled the action out and a request when the owner had asked for it. In a hand-labelled random 80 of the week, 40
were questions, requests or defect reports, 14 approvals, 9 noise, 8 corrections, 4 drift and 5 between correction
and request; naming `request` and `noise` gives that majority a place of its own, and a standing rule counts as a
correction because a memory line is what the duplicate check compares it with.

*Decision.* One function, used at runtime, in calibration and on held-out cases: an item **qualifies** when
`probabilities["correction"] + probabilities["drift"]` reaches the qualification cutoff. Summing keeps a reply that
both corrects and restores scope from splitting its probability below two separate cutoffs. Its published kind,
the larger of the two (`correction` on a tie), is advisory: calibration grades qualification, not the kind.
Otherwise the item is `unqualified`, its raw `choice` reported. An item that does not fit one request whole is
`unevaluated` (`too large`), never split: a part has no reply to judge (as the router's kit). A missing or malformed
`kind_` answer is §3.1's per-question `warn` and leaves its item `unevaluated` (`malformed answer`); an unassessed
request leaves its items `unevaluated` with its reason (`transient`, `too large`, `deadline`). With the
qualification decision disabled (§3.5) no request is sent and every resolved item is `unevaluated`
(`classification disabled`); sources are still read and the report written.

*Duplicates.* Claude Code keeps auto memory in `<config>/projects/<project>/memory/`, `MEMORY.md` its index;
`<project>` is derived from the Git repository, so worktrees and subdirectories share it, and outside a repository
from the project root (https://code.claude.com/docs/en/memory "Storage location"). Classify assesses only the
memory location it can verify cheaply, and reports every other case as `unassessed` with its reason:

1. *Layout.* Git runs in the cwd with the repository-location variables (`proc.DropRepoVars`) and the discovery
   limits (`GIT_CEILING_DIRECTORIES`, `GIT_DISCOVERY_ACROSS_FILESYSTEM`) dropped, and only
   two layouts are recognised from `git rev-parse --show-toplevel` (`top`): an ordinary checkout, whose `top/.git`
   is a directory, its own main checkout; and a linked worktree, whose `top/.git` is a file `gitdir:
   <common>/worktrees/<name>` where `<common>` is a directory named `.git`, that entry's `gitdir` file points back
   to `top/.git`, and the main checkout is the parent of `<common>`. A cwd outside any repository (git reports none)
   is its own main checkout. Anything else (a submodule, a separate Git directory, a gitfile pointing at another
   checkout's `.git`, a worktree not registered back, a removed main checkout) → `unassessed` (`unsupported
   layout`); a removed cwd → `unassessed` (`cwd gone`).
2. *Settings.* `<config>` is `$CLAUDE_CONFIG_DIR`, else `.claude` under `os.UserHomeDir()`, as the router's
   candidates (§3.2). `autoMemoryDirectory` is honoured only from `<config>/settings.json`, absolute or starting
   `~/` (`~` the home directory), naming the directory that holds `MEMORY.md`. A repository can also set it in
   `.claude/settings.json` or `.claude/settings.local.json`, which Claude Code reads at the launch directory, the
   checkout and, for a linked worktree's local settings, the main checkout (as Phase 3 found,
   `docs/design/phase3-cycle.md` §3.4 "Settings sources"), and honours only under workspace trust and its
   read-boundary setting, neither visible to classify. So either file setting it in the item's cwd, `top` or the
   main checkout → `unassessed` (`project memory override`). A relative value, or a malformed or unreadable
   settings file among those read → `unassessed` (`settings unreadable`). Managed (policy) settings, `--settings`
   files and `CLAUDE_CODE_PROJECT_DIR_NAME` belong to the writing session and are invisible here (§8).
3. *Index.* Otherwise the directory is `<config>/projects/<project>/memory/`, `<project>` the main checkout's path
   with every non-`[A-Za-z0-9]` byte replaced by `-` (**assumption**, observed). No project directory yet, or one
   without `MEMORY.md` or with an empty one → `new` (`no memory yet`), no request; an unreadable index →
   `unassessed` (`index unreadable`).

The week's 932 items: 692 under a directory with an index, 171 under one without, 69 with no project directory yet,
none with the cwd gone, an override or an unsupported layout. The index sent is what Claude Code loads at session start, its first 200 lines or 25 KB,
whichever comes first (a session can read further on demand). The observed indexes are small (22, median 270 B,
largest 2.5 KB). Each qualified item is its own `dup_` request, so a body depends on one reply alone: state
`{memory_index, owner, assistant, reply}`, each text whole as in `kind_`: the closing message gives a reply such as
"not that" its object, and the opening owner turn the scope a drift reply restores, which decides which index line
it repeats;
one `noul` question `dup_r<n>`, instructions "Does `memory_index` already cover every required rule of reply
`r<n>`? First settle from the `owner` turn and the `assistant` message which of the agent's changes the reply refers
to and what it asks: a reply that undoes or stops that change ('roll that back', 'なしにして') asks for the
instruction the change broke, from `owner` or a standing rule, plus any further instruction the reply states. A
rule is an action (what the agent must do or not do) under a condition (when it applies); a rule that only sets
this task's scope (what to change this time) is task-local. The required rules are all the reply asks, leaving out
a task-local rule when the reply also asks a reusable rule that a line covers. A line covers a rule when it states
the same condition and the same action, in any language or wording; a line requiring a result covers the fix that
restores it in this case, while a method the reply sets for every case is a rule of its own; a task-local rule is
covered by a line stating it as a standing rule or by a general line that applies to every case of its kind. A line
sharing a rule's topic, object or keyword does not cover it when it governs another object of the same kind,
applies only under another condition, asks a different action about the same thing, or is a rule the agent's action
also broke while the reply asks for another.", criteria true "the conversation establishes at least one required rule,
settles any referent needed to interpret it, and for each required rule at least one line of the index covers it; an
explicit standing instruction needs no earlier change to refer to", false "any of these does not hold: no required
rule is established, a referent needed to read one is not settled, or a required rule has no covering line".

*Rules the question settles.* The required rules are defined once and both criteria use them: the false criterion is the
true one's complement, so `duplicate` needs positive evidence, and runtime `new` means **coverage not established**:
a required rule uncovered, or the rules or their referent left unsettled. A reply restating a covered
rule and setting a new standing one ("leave vendor alone, and never regenerate fixtures on a version bump") is
`new`; a task-local scope drops out only beside a covered reusable rule, so an overlap whose correction is stated
reads `duplicate`, while a reply asking only a scope needs a line for that scope. An undo is read through the
conversation, never through the index: "revert that" after a rounding change the owner ruled out repeats a line
forbidding rounding changes, and "undo that" after a message reporting two changes, with no owner turn naming one,
is `new` even when the index forbids one of them. A fix for this case (split this PR, put the wrapper back) is
covered by a line requiring the result (a size limit, the shared wrapper); a method for every case is not. The four
kinds a related line can differ by are those behind every topic-related false positive among the `fit` cases
(§3.5): a retry count kept in settings beside where schedules live, and build numbers beside where release builds
are cut (another object); private storage beside what tag-only changes leave alone (another condition); not cutting
a build yet beside where builds are cut, and, as a margin-test false positive only (`fit-b12`), the logger choice
beside the log format (another action); a links-only scope beside the heading style the rewrite also broke (a rule
also broken). The other false positive, `fit-a10`, has no evident topic match; its score does not show which line
the model used. The question names the kinds, not those cases' objects, and its examples are not corpus replies or
rules.

A `dup_` body that does not fit is `unassessed` (`too large`). Outcome: `duplicate` (at or above the cutoff),
`new` (below it: **coverage not established in the assessed index**; topic files are not read and an undetected match stays
possible), `unassessed` (`transient`, `deadline`, `malformed answer`, `request failed`, `duplicate check disabled`,
or a lookup reason above). A persistent request failure (§3.1) marks only the items of that body: a failed `kind_`
request leaves its items `unevaluated` (`request failed`), a failed `dup_` request its item `unassessed`
(`request failed`); other requests' results are kept, and the run answers `warn` as below.

*Order and deadline.* No request is sent before every source is read, since the newest-first order needs all items.
A deadline during reading sends nothing: items already `unresolved` keep that reason, and every other item, read or
not, is `unevaluated` (`preparation deadline`). Then work runs in groups, newest first: a group is the next 4
`kind_` requests (one round at 4 in flight, about 70 items), then the `dup_` requests of the items it qualified,
before the next group; each planned body is attempted once. A deadline cut thus leaves the newest replies fully
checked and the oldest `unevaluated` (`deadline`); at most one group's qualified items are left `unassessed`
(`deadline`). A fixed reserve for the duplicate phase was rejected: its size depends on how many items qualify,
unknown until classification has run. Qualified items whose duplicate check is `unassessed`, for any reason, are
never dropped: they are counted and listed.

*Volume.* Every resolved reply is sent, unless it is too large for one request, qualification is disabled
(§3.5), or the deadline comes first: no prefilter or cap. §1 rules out narrowing what is sent, the savings are a
few cents a month, and a bare link, hash or one-word reply can be a correction of an earlier choice. The observed
week packs into about 53 `kind_` requests (the 809-byte question fills the 16 KiB question budget before the state's
48 KiB in about half of them). The `dup_` count depends on what qualifies; as a sizing scenario only, the hand-labelled
share (15–21%) gives 140–200 requests, an upper side since a `no memory yet` item needs none. Together ≈ 1.2M input
tokens (§3.6), well inside the 5-min budget. A week that outgrows it reaches the deadline as above; if O4 or O6
shows that, the first step is raising classify's own budget within the cycle's 30 min, the next reusing a label from
an earlier complete run whose input body hashes the same, rechecking the current index.

*Status and output.* A run answers `pass`, whatever it found; hits are not findings. It answers `warn` only for an
unparsable observe file or unknown `version`, a future `window.until`, a persistent API error (§3.1, including a
malformed answer), or a report not written (`latest-not-written`, as observe's, with the cause and the context line
"report not written"). Unassessed and unevaluated work is the §3.1 `jev-unassessed` finding, which changes no status.
Every run that read a fresh observe file writes `.local/state/stack/jev-classify/latest.json` (0600), a week with
no owner reply included, so an older report never stands for the current week:

```text
{version, run: {id, started, finished}, window, sources: {transcripts, t3}, complete, model,
 decisions: {qualification, duplicate}, counts,
 items: [{id, where, status: classified|unresolved|unevaluated, reason, opener, choice, probabilities, qualified, kind,
          excerpt, duplicate: duplicate|new|unassessed, dup_p, dup_reason}], usage}
```

`items` holds every `owner-reply` item exactly once, under observe's ID and locator; a qualified item also carries
its reply's `excerpt` (≤ 160 characters, as observe cuts it), so every candidate past the context's ten is readable
in the report. `sources` copies observe's status of its `transcripts` and `t3` sources. `decisions` says which are
`enabled` or `disabled`. `reason` explains `unresolved` and `unevaluated`, `dup_reason` an `unassessed` or a lookup
`new`.

**`complete`** is defined per item, over the work that item required. An item is *done* when it is `unresolved`;
`unevaluated` for `classification disabled`; `classified` and unqualified; or qualified with a duplicate outcome,
`duplicate`, `new`, or `unassessed` for a lookup reason (`cwd gone`, `cwd unknown`, `unsupported layout`,
`project memory override`, `settings unreadable`, `index unreadable`) or `duplicate check disabled`. It is not done
when `unevaluated` for any other reason or `unassessed` for `transient`, `deadline`, `malformed answer`,
`request failed` or `too large`. `complete` is true when every item is done, so an empty week is complete, and a removed worktree, a
failed observe source or a disabled decision never makes a healthy run look incomplete. Those are coverage, shown
beside it: `sources`, `decisions`, `counts.unresolved` and `counts.dup_unassessed_lookup`, and named in the context
of every run whose report is published, an empty week included (§3.1).

*Publication.* The report is first written to a temp file in its directory and synced; then classify takes
`latest.lock` through `gofrs/flock`, as improve's run lock (`extensions/improve/improve.go:284-293`), released by
the kernel if the process dies. Acquisition has its own cutoff, the work deadline plus half the finalisation
reserve (§3.1: 10 s of classify's 20 s), retried with `TryLockContext` until then, leaving the other half for the
compare, the rename and the response. Still busy at the cutoff → the temp file is removed and the run answers
`warn` `latest-not-written` (`report lock busy`). Under the lock the existing report is read, and it is replaced
only when its `window.until` is earlier than this run's, or equal with its `run.started` not later; otherwise this
run's report is dropped (its temp file removed), since a newer report already stands: its context drops the stale
summary and candidates for a notice, while its findings and their warnings (a persistent failure's action line)
stay. An existing report that cannot be read (not missing, not malformed) is a failure, never replaced. A failed
rename leaves the old file whole and answers `warn` `latest-not-written`. On Windows `os.Rename` replaces the file, but Go
promises no atomicity there; the Windows-machine check (§7) covers it.

*Context.* The counts; `new` qualified items, newest first, then qualified items whose duplicate check is
`unassessed`, each marked `(dup unassessed: <reason>)`; up to 10 lines in all, each `<kind> <where>: <excerpt>`, then
`(+k more in the report)`; the unresolved and unevaluated counts; and, in every run whose report is published, an
empty week included, any failed observe source, any disabled decision (`<decision> is disabled: <reason>`; for the
duplicate check while qualification is enabled, followed by `; candidates are not checked against memory`, an
empty week included, since it states what any week's list would be; with qualification disabled too there are no
candidates and the suffix is left out), and `complete: false` with the items not
done and why. A dropped run's context is the drop notice and its warnings only.

**Cycle wiring (dotfiles).** The supported path, scheduled or manual, is a dedicated call in the cycle job,
`stack run cycle --ext jev-classify` (`docs/design/phase3-cycle.md` §4.8), made right after a valid observe with a
fresh `latest.json` and before improve, so the report depends on observe alone and is produced whatever improve
does. The job publishes `last.json` only when observe and improve are both valid (`shuymn/dotfiles`
`docs/stack-cycle.md`); then the merged `last.json` lists the results in call order, observe, `jev-classify`, improve, and its status is `warn` when any is.
Classify's own outcome never fails the job: missing → `skipped`; self-answered → merged; exit ≠ 0 or invalid output
→ a merged `warn` result named `jev-classify` with the fixed error `invalid output`; a synthetic `error` → a merged
`warn` result keeping core's error text. The dotfiles wrapper test (`tests/test_stack_cycle.py`, a fake `stack` on
`PATH`) covers each (C17).

### 3.3 Layout and shared client (item 5)

```text
extensions/internal/jev/          client, packing, record lookup, usage ledger, cutoff selection
extensions/jev/semantic/          package + cmd/stack-jev-semantic/   (name in [stages]: jev-semantic)
extensions/jev/router/            package + cmd/stack-jev-router/   (deferred, §3.2)
extensions/jev/classify/          package + cmd/stack-jev-classify/
extensions/jev/cmd/jev-calibrate/ owner-run, needs the key; not a stage binary
fixtures/jev/<extension>/{fixed,cal}/<case>.json
```

One shared package holds transport, validation, packing, replay, metering, cutoff math and the decision functions;
state building, router candidates and classify's file stay with their extension. `net/http` only, bound by the
work deadline; no SDK. The endpoint is a link-time variable only test builds set (`-ldflags -X`), never
configuration. Each body is marshalled once; those bytes are hashed (§3.4) and sent. **Ledger:** each live attempt
appends `{run, time, extension, model, outcome, elapsed_ms, input_tokens}` (outcome `ok`, `max_tokens_exceeded`,
`transient` or `error`; tokens `null` without a `usage.input_tokens` count) to `.local/state/stack/jev/usage.jsonl`; `run` is a random
per-process ID; appends are serialised and the caller waits at most 0.1 s for each (a line abandoned before it is
written is dropped, one already being written may finish later); a slow or failed append is one stderr line and
changes no status. Replay never writes.

### 3.4 `--replay` without a key (item 4)

A fixture is a `--replay` file whose request carries a `jev` object, which live requests never have: core sends the
request as is (`internal/ext/request.go:108-119`, `internal/cli/run.go:145-157`) and extensions ignore unknown
fields. Core gives no other replay marker, so the offline guarantee holds for a fixture with a valid `jev` object.

```json
{"request": {"stage": "outbound", "repo": "", "range": "jev-mb..jev-head", "prompt": "", "title": "…",
             "body": "…", "branch": "", "jev": {"records": {"sha256:<a>": {"answer": {…}},
                                                 "sha256:<b>": {"status": 400, "error": "max_tokens_exceeded"}}}},
 "expect": {"status": "warn"},
 "want": {"findings": [{"kind": "jev-meta-duty", "where": "…", "detail": "…"}], "context": "…"},
 "repo": {"base": {"scripts/test": "…"}, "commits": [{"message": "…", "files": {"scripts/test": "…"}}]},
 "label": {"files": {"scripts/test": {"meta_duty_regression": true}}, "unsupported_claim": false,
           "split": "fit", "why": "…"},
 "samples": {"sha256:<a>": {"meta_duty_regression": [0.83, 0.85, 0.82]}}}
```

A record maps the SHA-256 of a sent body to its answer or non-200 status; packing depends only on the input, so a
fixture holds every body the live run sends (a classify `cal` fixture also holds the calibration-only `dup_`
bodies of §3.5, which replay never asks). With `jev` present the extension never calls the network, key or not;
a body with no record → `warn` `jev-unrecorded`. Machine inputs come from the fixture: `repo` (a synthetic repository
the test builds with fixed identity and dates, refs `jev-mb`, `jev-head`, a diverged base; semantic's cwd);
`jev.references` (the reference stub directory) and `kits` relative to the cwd. `<REFS>` in `want` expands to the
stub directory.

For classify, what the test writes to disk is a top-level fixture field, as semantic's `repo`: `observe` (the
observe file's content), `files` (transcripts and other files, by path relative to the cwd) and `memory` (index
text by repository or directory, written where classify's lookup places it, so the fixture follows the lookup and a
decoy index at a wrong location stays unread; the lookup itself is pinned on fixed paths by C9). `request.jev` holds
only what the extension reads at run time: `records`, `now` (the clock), `observe` (the file's path), `claude` (the
Claude configuration directory) and `t3` (per thread, its cwd and the rows its queries return), paths relative to the cwd; in
replay, relative locator paths and cwds in the observe file, transcripts and `t3` rows also resolve against the
cwd, so no fixture names an absolute path. Replay reads `jev.t3` in place of running `sqlite3`, the one step
replaced: no T3 database fits a JSON fixture, and no runner is assumed to have `sqlite3`; the queries are C9's,
against a fake and the real `sqlite3`. `SetRecords` replaces only `request.jev.records`, keeping the other `jev`
keys. Classify's `run.id` is asserted non-empty then normalised, its times come from `jev.now`. Core reads only
`request` and `expect`.

### 3.5 Pinned model and cutoffs (item 3)

Pin `jev-1.14.0` in `extensions/internal/jev`; `jev-latest` is never sent and no config overrides the pin. Fixtures
are public, so every case is synthetic (a small repository plus texts, a kit and reference stubs, transcript and
memory excerpts), Japanese and English. Labels are written by hand with a one-line `why` and reviewed by the owner;
classify's corpus is instead checked by a reviewer model and frozen before any score is seen (below).
Ground truth is bound to the scored unit, derived from per-unit labels with the live packing:

| Decision | Scored unit | Ground truth | Score | Cutoffs |
|---|---|---|---|---|
| semantic meta-duty | one request (cases that split a file are fixed-only, not fitted) | any of its files labelled true | `noul` | 1 |
| semantic unsupported claim | the whole-PR request | case label | `noul` | 1 |
| router current reference | kit (distinct kit states) | the labelled reference or `none` | `probabilities[choice]` of `current_ref`, stored before any cutoff, mapped below | 1 |
| classify qualification | item | correcting (`correction`, `drift` or an overlap) or not | `probabilities["correction"] + probabilities["drift"]` | 1, via the decision function |
| classify duplicate | correcting item (reply, its index) | duplicate label | `noul` | 1 |

`jev-calibrate` (owner, with the key) builds each `cal` case's synthetic repository and requests with the extension's own
builders, sends them through the live transport 3 times, writes the first answer (or status) to `records` and all
three scores to `samples` (`null` for an attempt with no score), then refits; when the first attempt has no
replayable outcome (e.g. a timeout before headers), it writes no fixture and no table and exits non-zero asking for a rerun. Fitting uses the `fit` cases, each single-call score one sample (live
decides on one call; a planned call with no score, e.g. a size rejection, is a negative prediction, never
dropped): the cutoff `t` is the lowest of 0.50–0.95 (step 0.01) that passes a margin test with the predeclared
δ = 0.05, where a positive is a hit only at a score ≥ t + δ, a negative is a false positive already at ≥ t − δ and a
missing score stays a miss, with precision ≥ the target (semantic 0.8, classify 0.8, router 0.7), true positives > 0
and recall ≥ 0.5. The floor stays 0.50 because a score under it leans no, and easy negatives would otherwise pull the
cutoff down to where live false positives rise. A cutoff on the floor is acceptable: the margin test already
counts a positive only at ≥ t + δ and a negative as a false positive from t − δ, so a fit passing at 0.50 reaches
its target with hits counted only from 0.55 and every negative from 0.45 counted against it. A held-out miss is
evidence that the question does not separate its cases, and the duplicate decision's miss prompted revising its
question (§3.2). A cutoff rule chosen because it would have cleared a held-out result already seen (§4 records one)
would need a fresh held-out week to validate it, so the rule stays. The same rule serves every decision; a precision slack, a higher floor, a wider δ and the cutoff of highest fit precision were rejected (§4).
The runtime decision stays score ≥ t; samples within ±0.05 of it are
listed as unstable. Each decision needs ≥ 6 + 6 positive and negative `fit` cases and ≥ 3 + 3 `heldout` cases, and
must meet its precision target and recall ≥ 0.5 on `heldout`; otherwise, or when no cutoff passes the margin test,
it ships disabled, its context says so and the calibrate report flags it for review. Each run reports, per decision,
the cutoff and the baseline without a margin (the lowest of 0.50–0.99 with plain score ≥ t) with their `fit` and
`heldout` counts, the margin-test result (`REVIEW` when no cutoff passes it), and the cases whose decision flips
across the 3 calls, by language, at the fitted cutoff or, when none passes, at the baseline if there is one.
The semantic corpus holds, per decision, at least 12 + 12 positive and negative `fit` cases and exactly 8 + 8
`heldout` cases (a case carries both labels and counts for each), every class balanced between Japanese and English
(`heldout` exactly 4 + 4, `fit` to within one, as a near-duplicate found in `heldout` joins its partner in `fit`) and
at least 40% hard, labels written
before scoring, translations and near-duplicates (a `group`) in one split, no file changed from the same starting
content in both splits outside a group, and a `heldout` case once inspected moved to `fit`, so new `heldout` cases
are fresh. Hard meta-duty positives weaken an assertion, suppress
lint, drop a CI check or weaken an agent instruction quietly; hard negatives are equivalent refactors and explicitly
justified removals, in code and in agent instructions. Hard unsupported-claim positives overstate verification, claim
unsupported behaviour or mismatch title and diff subtly; hard negatives are evidence-backed claims and accurate terse
descriptions (e.g. "speed up tests" for a diff that does). The router's calibrate stores, for each call, its `choice` and `probabilities[choice]` before any cutoff
applies, and fitting maps them to samples: a choice of the labelled reference is a positive at its score; a choice of
another reference is a negative at its score and, when the label is a reference, also a positive with no score (a
recall miss); `none` is an unscored sample, positive when the label is a reference. A wrong choice thus counts as a
false positive under the margin test whenever it reaches t − δ (at t = 0.60, a wrong choice at 0.57 is a false
positive and a right one at 0.63 a miss). Cases are counted once each, by their ground-truth label (distinct kits),
never by the extra samples a wrong choice adds, so the shared counter counts a case by its label. Its corpus follows
the same rule with kit states as cases: at least 12 + 12 `fit` and 8 + 8 `heldout` kits labelled with a reference
and with `none`, the positives covering at least 6 references; hard positives name the step in progress only
indirectly ("addressing review r1 findings" for `review.md`), hard negatives are multi-workstream kits with none
marked in progress, conditional waits ("CI pending, then merge"), and kits that mention a reference's topic only in
done or decided lines. Its reference stubs copy the first heading and first paragraph of the public hiden
references, the only text that enters the question, and record the `shuymn/dotfiles` commit they came from; a later
change to those lines makes the live question differ from the calibrated one until calibrate runs again. Calibrate refreshes `expect`
and `want` of the `cal` fixtures (matched exactly) and never rewrites a `fixed` fixture, whose records are written
by hand so a live answer cannot erase the scenario it checks; a placeholder cutoff table or pending calibration case
fails C11 in CI (`CI=true`) and skips the check locally, naming the calibration command. The `fixed` semantic fixtures (meta-duty hit, unsupported-claim
hit, clean, diverged base, split file, one batch hit with another rejected for size) keep hand-written `want`,
matched on status, finding `kind` and `where`, non-empty context with hits, and no `jev-unrecorded`/`jev-error`
(not probability, cutoff or pin text), so lost detection fails them; they run with test cutoffs injected by the test build (`-ldflags -X`, as the endpoint; no runtime override),
so a decision that ships disabled does not break them (shipped eligibility is C10/C11's).

Classify's corpus is synthetic weeks: each `cal` fixture is one observe file of 20–40 items with its transcripts,
T3 rows and memory indexes (§3.4), so its `kind_` bodies are packed as a live week's, and every item in a fixture
belongs to one split. Each item has a corpus-wide case ID, used in `samples` and the report, a kind label and, when
it corrects (`correction` or `drift`), a duplicate label `duplicate` or `new` against its directory's index.
Counts: `fit` at least 24 correcting items (at least 8 corrections, 8 drift and 4 overlaps; 12 `duplicate`, 12
`new`) and at least 16 others (at least 4 each of approval, request and noise); `heldout` exactly 16 correcting items
(6 corrections, 6 drift, 4 overlaps; 8 `duplicate`, 8 `new`) and exactly 16 others (6 requests, 5 approvals, 5
noise). The qualification decision thus has at least 24 + 16 `fit` and exactly 16 + 16 `heldout` cases, the
duplicate decision at least 12 + 12 and exactly 8 + 8; each `heldout` side is half Japanese, half English, though
the owner's real replies are 92% Japanese, and the per-language report shows a weak Japanese result. An overlap
both corrects and restores scope; its kind label is `drift` when the reply names the scope or goal, else
`correction`, and it is a qualification positive either way (the reported kind is not scored). At least 40% are
hard: corrections asked as a question ("that is ignored on this OS, isn't it?"), standing rules that never say
"wrong" ("reply in Japanese"), approvals that open with "no" ("no, that is fine"), defect reports on what was built
(`request`), and noise that reads as text (an input-method test sentence); and at least 4 antecedent pairs, the
same `assistant` and reply under two `owner` turns, one ruling the action out (drift) and one asking for it
(request), each pair a `group` in one split. A duplicate label follows the `dup_` question (§3.2): `duplicate` only
when the texts settle the reply's referent and a line covers each of its required rules, `new` only when an
identifiable required rule is uncovered. A reply whose rules or referent the texts leave unsettled reads `new` at
runtime but is no gold case: it is not drafted into `fit` or `heldout`, and the example of that behaviour is the prose
one in §3.2, with no corpus case or calibrate path of its own. `dup_kind` names a hard duplicate category: for a
`duplicate` item `restatement` (a Japanese restatement of an English line), `paraphrase`, `general` (a general line
covers it) or `multi` (two reusable rules, each covered by a separate line); for a `new` item one of the four kinds
the criteria name, `object`, `condition`, `action` or `broken`, or `partial` (one rule covered, another reusable
rule not). It is required on the held-out items that fill the `dup_kind` quotas below and optional elsewhere;
wherever present it matches the item's `dup` (a positive category only on `duplicate`, a negative one only on
`new`) and implies `hard: true`. `undo` marks a reply that only undoes or stops a change. Paraphrases and items
sharing one memory rule stay in one split. Indexes are 3–20 lines, as observed. Cases are drafted from these quotas
and the label definitions, never from a real reply, path or name, with labels and `why` written and checked against
the label definitions by a reviewer model before any score is seen; they freeze then. A label changed after scores
were seen (the owner may still relabel in the PR) does not keep its case: a held-out case relabelled that way is
replaced by a fresh one, since rerunning on the same case is no longer an independent check. A `fit` case whose
label the definitions do not settle is replaced the same way: `fit-a13`, whose "never delete changelog entries"
may or may not be what "never rewrite the changelog history" forbids, gives way to `fit-a23`, drafted from the
definitions in the same role (English, not hard, a drift overlap labelled `duplicate`, no group, in `fit-a`'s
`p2`), whose correction has the same condition and action as one `p2` line other than the changelog line and whose
scope part is task-local; the implementation PR regenerates the fixture. The real week sets only the quotas.

A held-out fixture is inspected as a whole once any of its cases is (its scores, records or texts read to diagnose
a decision, beyond the figures and unstable lines every run reports): its items share `kind_` bodies and one split,
so it moves to `fit` whole. The fixtures are `fit-a`, `fit-b`, `fit-c` (the former `heldout-a`, moved to `fit`
whole after its duplicate scores were inspected; `fit-c<nn>` keeps the number of `held-a<nn>`) and `heldout-b`.
`heldout-b` is one fresh week of exactly the `heldout` counts above. Its held-out classes are `duplicate`, `new` and
the others, 4 + 4, 4 + 4 and 8 + 8 by language. Its `duplicate` items hold at least 5 hard positives (2
`restatement`, 1 `paraphrase`, 1 `general`, 1 `multi`) and at least one `undo` reply, which may fill one of those;
its `new` items at least one hard negative of each kind, `object`, `condition`, `action`, `broken` and `partial`,
one of them an overlap. No `fit` case is `partial` or `multi`, so `heldout-b` is the first measure of them. The
`general` line may be a scope rule only if it is worded independently and states a substantively different rule
from the general scope rule of `fit-a11` and `fit-b10`, which stays measured in `fit` and in O4. It holds 2
antecedent pairs of its own, one `en` and one `ja`, whose request halves count among the 6 requests, and at least
40% hard items. Its indexes and objects are new: no index line and no corrected object is taken from a `fit`
fixture. The reviewer model checks each label, `why`, `dup_kind` and `undo` against the definitions, each
`duplicate` naming the covering line for each of its required rules, each `new` naming an uncovered required rule
the conversation establishes, and each hard negative the line it shares a
topic with and how it differs, and confirms that each held-out positive's rules are distinct from every `fit`
rule; the cases freeze before the first score. The `dup_` question, the cutoff rule and the Activation criteria
below are fixed before any `heldout-b` score is seen; when its aggregate result leads to changing any of them,
`heldout-b` moves to `fit` as well and a fresh held-out week validates the change.

`jev-calibrate` records every `kind_` body as the runtime packs it. Samples for the duplicate decision come from
the 3 calls on each ground-truth correcting item, whatever its `kind_` request answered (a 413 included). After
fitting, it also records the `dup_` body of every item the
first `kind_` answer qualifies at the fitted cutoff, so the records hold the union of the two sets; since a `dup_`
body carries one reply, a record never depends on which other items qualified, and replay finds every body the
runtime sends.

**Activation.** The two decisions passing their own gates is not enough, since they can pass while the report
lists nothing new: if qualification finds the 8 held-out duplicates and misses the 8 held-out `new` items, its
precision is 1.0 and recall 0.5, the duplicate decision can pass too, and the `new` list is empty. So on `heldout`
calibrate also scores the list the report would show, end to end, from the first answers with the provisional
cutoffs (those of decisions that passed their own gates), before it sets any enabled bit. Which list is scored
follows the duplicate decision's gate alone, and whether anything is listed follows qualification's:

| Qualification gate | Duplicate gate | `list` | Listed | Scored against (`support`) | `scored` |
|---|---|---|---|---|---|
| pass | pass | `new` | qualified, with a `dup_` score below its cutoff | items labelled correcting and `new` | true |
| pass | fail | `qualified` | qualified | items labelled correcting; recall also on those labelled `new` | true |
| fail | pass | `new` | nothing | as in the first row | false |
| fail | fail | `qualified` | nothing | as in the second row | false |

`tp`, `fp`, `fn`, precision and recall count the listed items against `support`; a missing `kind_` or `dup_` score
is a miss, and a `dup_` score equal to its cutoff is `duplicate`, so not in the `new` list. With `list` `qualified`
the figures also carry `new_support` and `new_recall`, the listed share of the items labelled `new`; both are
written whenever `list` is `qualified`, a `0` included, and left out with `new`. Equality of figures is by value:
`Acceptance.Holds` and `VerifyClassify` compare the fields' values (an `Equal` method when a field is a pointer
kept for that presence), never pointers. The gate is
precision 0.8 and recall 0.5, and with `list` `qualified` also `new_recall` 0.5, so a list of known corrections
alone (the 8 duplicates found, the 8 `new` missed) fails. The output also gives the figures per language, reported
but not a gate. When qualification passed its own gate and the end-to-end gate fails, qualification ships disabled
(§3.2: sources read, report written, items `unevaluated`, `classification disabled`) with the reason
"end-to-end `<list>` list on heldout: precision <p>, recall <r>[, new recall <n>], support <s> miss the target
0.80 / 0.50; REVIEW", unless the owner accepts the result in the cutoff table (`accepted`: the pin, a hash of the
`heldout` fixtures' cases, records, repository layouts and samples, the figures with their `list`, and the owner's
one-line reason). Qualification thus ships enabled only when it passes its own gate and either the scored list
passes or a matching acceptance holds. The acceptance applies only in that case and holds only while all three match; any change to the
pin, the evaluation inputs, the figures or `list` voids it. When qualification failed its own gate it keeps its own
reason and the calibrate report's own-gate `REVIEW`; the end-to-end figures (`scored: false`, every labelled item a
miss) are reported and there is nothing to accept. A duplicate decision failing its own gate ships disabled in
every row.

With the duplicate check disabled, every qualified item is `unassessed` (`duplicate check disabled`, a done reason,
so it alone never makes `complete` false), no `dup_` request is sent, and the owner sees the counts (`n qualified
(0 new, 0 duplicate, n dup unassessed)`), up to 10 candidates newest first, each `<kind> <where>: <excerpt> (dup
unassessed: duplicate check disabled)`, then `(+k more in the report)`, and the line `the duplicate check is
disabled: <reason>; candidates are not checked against memory`. The purpose, surfacing new rules from the week's
corrections, keeps its recall in the report: the duplicate check's harmful error is a new rule marked `duplicate`
and hidden, which a disabled check never makes, and the `new_recall` gate holds qualification to finding the new
ones. The 10 context lines are not so protected: they share their slots with corrections the index already states,
and a newer known correction can push a new rule into `(+k more in the report)`. Triage therefore happens in the
report, where every candidate's excerpt is listed and the owner rules out known ones against an index of a few
lines (median 270 B, largest 2.5 KB observed; the index read now, which may differ from what the replying session
loaded). A known correction listed is itself worth seeing: the reply repeats a rule present in the current index.

The `fixed` classify fixtures are matched per item by ID, on `where`, `status`, `reason`, `opener`, `qualified`,
`kind`, `excerpt`, `duplicate` and `dup_reason`, and on status, finding kinds, `complete`, `sources`, `counts` and the
context (lines, order and markers), so a dropped or swapped row fails. They are:

- resolution, each asserting the exact `owner` and `assistant`: both locator forms, with a T3 thread ID holding
  `:` and numeric and `null` payload IDs whose expected locators come from observe's `jsonScalar`; a tool result
  between the assistant text and the reply; two owner turns in a row after a turn with text; a tool-only turn
  followed by two owner replies (both `no assistant text`, the older turn's text not borrowed); a server-created
  row opening a turn (`owner: ""`); a finished assistant turn, then a server-created prompt, then the owner's reply
  before any answer (`intervening prompt`, not the older turn); a qualified reply in a transcript turn opened by a
  compaction summary (`owner: ""`, `opener: compaction`, carried into the artifact); a reply edited since observe (`record changed`); a
  thread absent from `jev.t3`;
- decisions: a known `duplicate`; a known `new` whose `dup_` record, deleted alone, fails the fixture; an approval;
  the antecedent pair, whose bodies differ only in `owner`, with the scope rule at the end of a 6 KiB owner turn;
  a duplicate pair where only the opening instruction differs, so the same reply restores a scope one index line
  states (`duplicate`) or one it does not (`new`); a duplicate whose referent sits at the start of a long
  closing message; and, in a build with the duplicate check disabled (`no-duplicate`), a qualified reply its index
  states and a newer qualified `new` one, both `unassessed` (`duplicate check disabled`), no `dup_` request,
  `complete: true`, the counts `2 qualified (0 new, 0 duplicate, 2 dup unassessed)`, both candidate rows newest
  first with their marker, and the disabled line ending exactly `; candidates are not checked against memory`;
- memory, each with a decoy index where a wrong lookup would land: an index in the main checkout's project
  directory with a decoy at a linked worktree's; a checkout `.claude/settings.json` setting `autoMemoryDirectory`;
  a linked worktree whose main checkout's `.claude/settings.local.json` sets it (both `project memory override`); a
  separate Git directory named `.git`, and a gitfile pointing at another checkout's `.git` (both `unsupported
  layout`); a directory without `MEMORY.md` and a main checkout with no project directory (both `new`, no
  request); a removed cwd (`cwd gone`);
- failures and shape: a `kind_` batch rejected for size; a missing `kind_` answer (`warn`); 12 qualified candidates
  (10 context lines, `(+2 more in the report)`, all 12 excerpts in the report); an empty healthy week; an empty
  week with observe's T3 source failed (`complete: true`, `sources.t3` failed); an empty week with qualification
  disabled and a failed source (`complete: true`, both gaps in the context); an empty week in a build with both
  decisions disabled (`no-decisions`: both disabled lines present, the duplicate one ending at its reason, without
  the suffix); an empty week in a `no-duplicate` build (the duplicate line with the suffix); a `no-duplicate` week with
  a missing `kind_` answer (`complete: false` for those items, though every qualified item's duplicate reason is
  done); an all-unresolved week.

Replay uses the recorded transport. **Model
bump:** one PR changes the pin, re-records, refits, and reports old vs new on `heldout` (precision, recall, support,
router pointer hits). The corpus measures regressions on itself; the ledger does not measure live precision.

### 3.6 Cost and latency estimates (item 6)

From the published price, the §2 sizing estimate and §3.1 packing. Estimates, not measurements:

| Extension | Typical run | Requests | Cost per run | Budget (own / core) |
|---|---|---|---|---|
| semantic | 20 KiB PR | ⌈diff bytes / (48 KiB − texts)⌉ ≈ 1 | ≈ $0.0003; a 112 KiB PR ≈ 3 requests ≈ $0.0012 | 10 s / 120 s shared |
| router (when built) | one kit of 2–13 KB, 12 candidates (≈ 6 KiB question) | 1 per kit | ≈ $0.0002–0.0006 | 4 s / 10 s (≥ 1 s at compaction) |
| classify | the observed week: 932 replies, 921 resolved; units ≈ 2.0 MiB (owner turn, closing message, reply, all whole), `kind_` questions 809 B each | ≈ 53 `kind_` (about half closed by the 16 KiB question budget) + one `dup_` per qualified item needing a check, 140–200 as a sizing scenario (§3.2) → ≈ 190–250 | `kind_` ≈ 2.8 MiB + `dup_` ≈ 0.5–0.7 MiB ≈ 1.2M tokens ≈ $0.05 | 5 min / 30 min |

Classify's requests at 4 in flight are about 14 `kind_` rounds and 35–50 small `dup_` rounds, under a minute at §2's
assumed latency; resolution is one scan of the week's transcripts (138 MiB observed), one locator query, one cwd
query and one query per T3 thread. The daily job re-reads a 7-day window, so each reply is classified up to 7 times.
With all three built, about 100 pushes, 300 kit opens and compactions and 30 cycles a month ≈ $1.7, classify ≈ $1.5
of it, uncapped; O6 reports what the ledger saw against the number of runs.

## 4. Alternatives

| Choice | Option | Gains | Gives up | Reversibility / when it flips |
|---|---|---|---|---|
| Failure (3.1) | **transient → unassessed, persistent → warn (chosen)** | warn only when the owner must act | an outage loses that run's advice | high; flips if outages hide a broken key |
| Size limits (3.1) | **pack by encoded size, no re-split; too large → unassessed (chosen)** | nothing sent is cut; bodies depend only on input, so replay records them exactly | a request the estimate under-sizes is lost | high; flips if O6 shows `max_tokens_exceeded` often |
| | split again on rejection | recovers those requests | bodies depend on provider answers; replay must record the split path | — |
| Replay (3.4) | **records inside the fixture request, keyed by body hash (chosen)** | self-contained; core's `--replay` unchanged; any input change misses loudly | fixtures grow by the records | high |
| Client (3.3) | **one package `extensions/internal/jev` (chosen)** | one error, packing, record and cutoff contract | a shared change touches three binaries | high |
| Router question (§3.2) | `choice` with `none` | One question per kit covers a single active step or a wait | References that apply together can split the probability below the 0.50 floor | Revisit per-candidate `noul` if calibration disables the decision for low recall |
| Router output (§3.2) | A pointer | A reference can reach 10 KB, beyond core's 4,500-unit entry block limit | The agent must read the reference | Revisit if agents ignore the pointers |
| Classify volume (§3.2) | **send every resolved reply, newest first; the deadline leaves the oldest `unevaluated` (chosen)** | no reply dropped unread; the observed week costs ≈ $0.05 and well under the budget | approvals and noise (≈ 30% of a sample) are paid for | high; flips toward reusing labels of an earlier complete run if runs hit the deadline |
| | a deterministic prefilter (short, ID- or link-only replies) | a few cents a month | drops input before sending, which §1 rules out; a bare link, hash or "not that" can be a correction | — |
| | a per-run item cap | a bounded run | the cut is arbitrary and repeats daily | — |
| Classify labels (§3.2) | **five: `correction`, `drift`, `approval`, `request`, `noise` (chosen)** | the majority (questions, requests) has a label of its own instead of filling `other` | a larger question per item (809 B) | high; flips if calibration shows `request` absorbing corrections |
| | four, with `other` | a smaller question | half the observed replies share one label with no description | — |
| Classify qualification (§3.2) | **one cutoff on `p(correction) + p(drift)` (chosen)** | a reply that both corrects and restores scope cannot fall between two cutoffs; one decision to calibrate | the kind reported is the larger label, not scored | high |
| | a cutoff per label, the higher wins | each label tuned apart | an overlap splits its probability and qualifies as neither | — |
| Classify context (§3.2) | **the closing message, the opening owner turn and the reply, each whole (chosen)** | `owner` separates drift from a new request; nothing within a sent text is lost; the week's units total ≈ 2.0 MiB, the largest 18 KiB | earlier messages of the turn and tool output are not seen | high; flips if calibration shows drift needs more of the turn |
| | the whole assistant turn | all the agent did | long T3 turns reach 285 KiB; 14 items of the week would not fit one request | — |
| | the closing message alone | smallest | the same reply is drift or a request depending on what the owner asked, which it cannot see | — |
| | byte-bounded excerpts (owner ≤ 2 KiB, closing message's last 1 KiB) | ≈ 0.3 MiB less a week | a scope rule or referent past the bound vanishes from the wire, turning drift into a request or a duplicate into `new`; 22 owner turns of the week exceed 2 KiB | — |
| Classify item size (§3.2) | **never split; too large → `unevaluated` (chosen)** | a reply is judged with its whole context; no part answers to combine | an item over 48 KiB is lost (the largest observed is 18 KiB) | high |
| | split, then combine part answers (max, or `unevaluated` on disagreement) | no item lost | a part without the reply has nothing to judge, so its answer is noise | — |
| Duplicate request (§3.2) | **one reply per `dup_` request, with its whole closing message (chosen)** | a body depends on its reply alone, so calibration records what runtime sends whatever else qualifies; "not that" has its object | one request per qualified item (140–200 in the sizing scenario) | high |
| | the directory's qualified replies in one request | fewer requests | a body depends on which other items qualified, so calibration's bodies miss at replay (`jev-unrecorded`) | — |
| Duplicate question (§3.2) | **required rules defined once (all the reply asks, a task-local scope dropped only beside a covered reusable rule); `duplicate` only when a line covers each and the referent is settled from the conversation; four named kinds of related line that do not cover (chosen)** | true and false criteria cannot both hold; excludes the kinds of line behind every topic-related `fit` false positive; a new standing rule beside a covered one stays `new`; an undo keeps the rule it restores, never inferred from the index | a longer one-shot question per `dup_` body (a recall risk, §8); an overlap whose correction is covered reads `duplicate` though its scope is not stated | high; flips if fresh held-out false positives come from a kind of line the criteria do not name, or `restatement`, `multi` or `undo` positives start to miss |
| | the topic-only exclusion ("a line that only shares its topic") | a shorter question | a line on the same topic stating another rule still reads as a match (7 false-positive calls on 3 `fit` cases at 0.50, scores up to 0.91) | — |
| | one stated rule is enough | a short criterion for overlaps | hides a new standing rule asked beside a covered one | — |
| | one question per index line | a judgment per line | a body's question count grows with the index; a general line covering a specific reply is judged line by line out of context | — |
| Cutoff selection (§3.5) | **lowest cutoff passing the margin test, floor 0.50, δ 0.05, for every decision (chosen)** | decided on `fit` alone; a cutoff on the floor already counts hits only from 0.55 and negatives from 0.45 | a decision whose question does not separate its cases passes fit thinly and misses on `heldout` | high; revisit only with evidence from a fresh held-out week, never from one already seen |
| | a slack of one case (3 samples) added to fit false positives | a thin fit cannot pass | chosen with the seen held-out result in view (on the old question it alone lifts held-out duplicate precision past 0.80); sized on negatives while precision moves with hits; disables a thin decision with no false positive (6 positive cases, 9 hits: 9/12 = 0.75) | — |
| | a higher floor (0.60) | a uniform guard | arbitrary; moves every decision, and meta-duty's held-out recall is 0.50 at 0.50, so it risks disabling a passing decision | — |
| | a wider δ | a wider band of doubt | the predeclared δ matches the measured pass-to-pass spread (§2); widening it after a miss is fitting to the seen result | — |
| | the cutoff of highest fit precision | the most fit margin | chases a few cases among 12–13 and gives up recall (0.68 on the committed duplicate samples, fit recall 0.90) | — |
| Held-out refresh (§3.5) | **an inspected held-out fixture moves to `fit` whole; a fresh week replaces it (chosen)** | one split per fixture and its `kind_` packing kept; no judgment of which cases were seen | 32 fresh cases to draft and review, not 16 | high |
| | move only the inspected cases | fewer fresh cases | splits a fixture across splits and repacks its `kind_` bodies; rests on a record of who read which score | — |
| Activation without the duplicate check (§3.5) | **when the duplicate decision fails its gate, gate qualification on the `qualified` list, with recall also on the items labelled `new` (chosen)** | the week's corrections reach the owner, none hidden, and a list of known corrections alone fails | known rules are listed beside new ones and share the 10 context lines; triage happens in the report | high; the `new` list applies again once the duplicate decision passes |
| | keep qualification disabled until the duplicate decision passes | no list unchecked against memory | a secondary filter's miss withholds every candidate | — |
| Classify deadline (§3.2) | **groups of 4 `kind_` requests, each followed by its `dup_` requests (chosen)** | a cut leaves the newest items fully checked; no reserve to size | no overlap between a group's `dup_` and the next `kind_` | high |
| | a fixed reserve for the `dup_` phase | `kind_` runs uninterrupted | the reserve's size depends on how many qualify, unknown in advance | — |
| Classify order in the job (§3.2) | **after observe, before improve (chosen)** | the report depends on observe alone and appears when improve fails or times out | improve starts up to 5 min later | high |
| | after improve | improve first | an invalid improve ends the job before classify, losing the week's report | — |
| Classify replay of T3 (§3.4) | **rows in the fixture, read in place of `sqlite3` (chosen)** | runs on every OS; the parser is the live one | the SQL is tested apart (C9) | high |
| | a fake `sqlite3` on `PATH` in the replay test | the subprocess path runs too | a test binary per OS in a fixture-driven test; the fake still cannot check the SQL | — |
| | a database built with the real `sqlite3` | the SQL runs end to end | skips where `sqlite3` is absent | — |
| Turn walk (§3.2) | **pass over only tool results and the reply's own consecutive owner messages; any other prompt → `unresolved` (`intervening prompt`) (chosen)** | a reply is never attached to a turn it may not answer; one rule for both readers | 8 of the week's 932 replies go unclassified | high; flips if `intervening prompt` grows large in O4 |
| | pass over every prompt until assistant output | resolves those 8 | a reply sent after a server prompt and before its answer borrows the older turn silently | — |
| Memory layout (§3.2) | **recognise only an ordinary checkout and a registered linked worktree; anything else `unassessed` (chosen)** | two cheap checks on files; only an ordinary checkout (`.git` directory at the top) and a linked worktree registered back to its common directory are looked up; an encoding collision or an override classify cannot see stays possible (§8) | submodules and separate Git directories get no duplicate check (none in the week) | high |
| | derive the root from `--git-common-dir` for any layout | covers every layout | a gitfile or separate metadata named `.git` can point the lookup at another checkout's index | — |
| Report lock (§3.2) | **`gofrs/flock`, as improve, bounded acquisition (chosen)** | released by the kernel on death; no stale-lock rule for a paused writer to slip through | one dependency already in the module | high |
| | an exclusive lock file with a stale age | no dependency | a writer paused past the age resumes after another has published, and an older report can win; a crash blocks runs until the age passes | — |
| Memory override (§3.2) | **`autoMemoryDirectory` from user settings only; a checkout setting it → `unassessed` (chosen)** | never scores an index Claude Code would ignore through the checked settings files; no trust or read-boundary state to reconstruct | a repository's legitimate override gets no duplicate check | high; flips if classify can read Claude Code's trust state |
| | honour the checkout's settings files by precedence | covers repository overrides | Claude Code applies workspace trust and its read boundary to them, which classify cannot see, so a wrong index can mark a new item `duplicate` | — |
| Semantic caller | **explicit call beside `stack-public check`, merge-base range (chosen)** | the PR's own comparison and texts | a dotfiles change | high |
| | `[merge].message_checks` | automatic | a missing binary or a cut child refuses the merge; no diff | — |

## 5. Implementation order and PR split (item 7)

1. `jev-semantic` and the shared client are implemented. See §3.2 for the hiden `commit-pr` caller.
2. `jev-classify` follows #56 and can proceed while the router is deferred. Wire the cycle call and its live key in
   `shuymn/dotfiles` (§3.2).
3. `jev-router` starts when the §3.2 trigger is met. Add its fixtures and calibrated cutoffs. The owner runs
   calibration before merge (O1).

## 6. Acceptance criteria

`replay-test` (C12): built binaries, temp home, `stack run <stage> --ext <name> --replay <fixture>`.

### 6.1 Purpose layer

| ID | EARS criterion | Source | Scenario | Observable result / failure | Check |
|---|---|---|---|---|---|
| P1 | While no key is available (`TYPESAFE_API_KEY` unset or blank after trimming, and no key file or an unknown home directory), each built `jev-*` shall answer `pass` with no context and make no call. | #57 opt-out | live request, key unset and no key file (unsetting the variable alone does not opt out while the file exists) | `pass`, `context` `""`; failure: warn or a ledger line | each built one under `env -u TYPESAFE_API_KEY` with no `~/.config/stack/jev/key`: `stack run outbound --ext jev-semantic --range <r>`, `stack run cycle --ext jev-classify`, and `stack run entry --ext jev-router` once built |
| P2 | When a PR's merge-base range weakens a test or check without saying so, `jev-semantic` shall answer `warn` with a `jev-meta-duty` finding naming the files; a test added only on the base shall not show as deleted. | #57 | `fixed` hit and diverged-base fixtures | the `want` finding; failure: pass or other kinds | `replay-test`; key: O2 |
| P3 | When the PR text claims what the diff does not show and the PR fits one request, `jev-semantic` shall answer `warn` with `jev-unsupported-claim`; when it does not fit, it shall report the question unassessed. | #57; owner decision | "add tests" without tests; a large PR | finding or `jev-unassessed` | `replay-test`; key: O2 |
| P4 | `jev-semantic` shall never make `stack run outbound` exit 1, and `public`'s refusal shall still exit 1. | §1 | live run, both extensions | exit 0 with a semantic `warn`; exit 1 with `public: block` | C15 |
| P5 | (When built, §3.2.) When an open kit's state names a single step in progress whose hiden reference scores at or above the cutoff, the entry context shall point at that reference, one path per kit, uncut in the router's output and whenever shown whole, within 1,000 UTF-16 units in total (any pointer that does not fit, the first included, is omitted with a count), at `kit open` and, room permitting, after compaction; a kit with several workstreams and none in progress, or a conditional wait, shall get no pointer. | #57; §3.2 trigger | synthetic kit reviewing a PR; a multi-workstream kit; a conditional-wait kit | the `want` context | `replay-test`, C16; key: O3 |
| P6 | When observe found owner replies, `jev-classify` shall classify each resolvable one that fits a request newest first with its opening owner turn, while qualification is enabled and before its deadline (C4, C7), give each qualified one a duplicate outcome, asking `dup_` alone against its directory's memory index only where §3.2 calls for a request (while the duplicate check is disabled, none: every qualified item `unassessed`, `duplicate check disabled`), report every item once with separate classification and duplicate reasons, list qualified `new` items and those whose duplicate check is unassessed, answer `pass` unless the owner must act, and write a report for every fresh week, an empty one included, never over a newer week's. (After #56.) | #57 | both locator forms, a known duplicate and `new`, an approval, an unresolved item, an antecedent pair, an empty week, the duplicate check disabled | each item's outcome and reasons, `complete`, `sources` and the context | `replay-test` incl. the artifact (C12); budget cut: C4; key: O4 |
| P7 | Each extension shall send only the pinned model, with cutoffs from calibration on `fixtures/jev/`. | #57 | any call | `model` `jev-1.14.0` | C10, C11; O1 |
| P8 | When a fixture carries recorded answers, the extension shall answer from them with no network, and `warn` `jev-unrecorded` on a miss. | #57 | key unset; dummy key + failing endpoint | `want` met; deleted record → feature assertion fails | C12 |
| P9 | If Jev fails or answers malformed, the stage shall continue: unassessed on transient or too large, `warn` on persistent, within the budgets. | §3.1 | fake server | per §3.1 | C2, C4; key: O5 |
| P10 | Each live attempt shall try to add one ledger line within its bounded wait (§3.3); cost is observed, not asserted (#57 asks for estimates, §3.6). | #57; §3.3 | real runs | lines with known/unknown usage | C14; key: O6 |
| P11 | While the key is available and `stack-jev-classify` is on `PATH`, when the cycle job has a fresh observe result, the owner shall get that week's classify report whatever improve does; when observe and improve are both valid, the job's `last.json` shall carry classify's own outcome (a classify `warn` makes the status `warn`, a classify `pass` leaves it to the others), and classify's outcome shall never fail the job. | §3.2 cycle wiring | improve timeout; classify pass; classify warn; classify missing | report written; `last.json` status | C17 |

### 6.2 Plan layer

| ID | EARS claim | Source | Test |
|---|---|---|---|
| C1 | Each built extension shall follow §3.1’s key precedence, opt-out and key-error contract after reading stdin to EOF. Replay shall skip key lookup and reject `jev: null` without a live call. | P1 | `jev.TestOptOut` per binary (classify: a fresh observe file with resolvable items and an endpoint spy that fails the test on any request; no report, ledger line or context), `jev.TestKey*`, `semantic.TestKeyFile` |
| C2 | The client shall apply §3.1’s request-level and question-level failure rules, including partial HTTP 400 bodies, and shall omit response body contents from output. | P9 | `jev.TestClientErrors` (`httptest`, slow body) |
| C3 | Packing shall use the budget 48 KiB less the fixed fields, split an oversize unit into ordered parts measured in their complete encoded state, keep questions ≤ 16 KiB, send input that fits whole even under a 1 KiB budget, mark questions unassessed only when splitting is needed and the budget is under 1 KiB or the smallest part cannot fit, and lose no unit. In unit-question mode (§3.1) it shall never split, encode each unit's fields in order, ask each unit question only on the request carrying its unit, close a request at whichever budget its next unit would pass first, and give every unit exactly one disposition (packed once, or unpacked as too large). The router packs one kit and uses packing only to measure it. | §3.1 | `jev.TestPack` (25 KiB texts with a small diff; a file over the budget; budgets just above and below 1 KiB, with a small diff that fits whole below it; escaped runes and `part k of n` framing at the boundary; captured bodies reassemble every unit), `jev.TestPackUnits` (units filling the question budget first, then the state budget first, each exactly at and one byte past it; a 60 KiB unit; the exact multiset of bodies and the unit-to-question membership of each; semantic's plans unchanged) |
| C4 | Each extension shall stop at its work deadline, report the rest unassessed (classify: `complete: false`), answer within its reserve, and keep the response under 512 KiB with `jev-omitted`; core's synthetic `warn` under a shorter parent deadline is a separate outcome. | §3.1 | `<ext>.TestBudget`: slow git → no request, questions unassessed (`preparation deadline`, classified by the deadline before the exit code), answer by 10 s, with a child holding its pipe open so a longer pipe wait fails; slow HTTP → answered batches kept, rest unassessed; many-batch result accepted by core; shortened core deadline; classify, with items given oldest first so order is not inherited: a deadline in preparation after one source was read (nothing sent; items already `unresolved` keep their reason, every other item, read or not, `unevaluated`, `preparation deadline`); during a later `kind_` group with a recent `new` item already checked (it stays `new` and listed, the older items `unevaluated`); during a group's `dup_` requests (its qualified items `unassessed`, `deadline`, listed); each with `complete: false` and the file written within the reserve; the report lock released just before its cutoff (published before the process deadline) and held past it (`latest-not-written`, `report lock busy`, answered before the process deadline) |
| C5 | Invalid stdin → `warn`; another stage, nothing to ask (empty range, a range whose diff has no files, no kits or no router candidate, no items) → `pass` with no request; results assemble in unit order under out-of-order completion. | §3.1 | `<ext>.TestNothingToAsk`, `jev.TestOrder` |
| C6 | `jev-semantic` shall build identical bodies under config variants (system, global, `GIT_CONFIG_PARAMETERS`, repository-local `diff.suppressBlankEmpty`, `diff.noprefix`, `diff.relative`, `diff.indentHeuristic` on a patch whose hunks move, `core.abbrev`, `diff.orderFile`), ask `unsupported_claim` only when the whole PR fits, and answer `warn` before any request on a git failure. | P2, P3 | `semantic.TestState` (synthetic repository) |
| C7 | A probability just below, at and above the cutoff shall decide pass, hit, hit; a disabled decision never flags; never `block`. Classify's decision shall qualify on the summed probability at the cutoff, take the larger label as kind and `correction` on a tie, send no `kind_` request when qualification is disabled and no `dup_` request when the duplicate decision is, and still write the report. | P2–P4, P6 | `semantic.TestDecision`, `classify.TestDecision` (sum just below, at and above; equal labels; each decision disabled) |
| C8 | When built, the router shall follow §3.2’s candidate, state and decision rules. Its output shall satisfy the UTF-16 budget with astral characters, preserve whole paths and report unassessed kits as specified there. | P5 | `router.TestCandidates`, `router.TestState`, `router.TestContext` |
| C9 | `jev-classify` shall resolve both locator forms as §3.2 states (T3 locators formed as observe forms them, observe's line numbering and owner provenance, the turn boundary, the excerpt and timestamp check, and each query failure's outcome), report each unresolved item with its reason; send items newest first under `r<n>` IDs, so generated metadata carries no path or observe ID; find the memory directory only as §3.2 establishes it; run each group's `dup_` requests after its `kind_` requests, each with one qualified reply and no unqualified one and the §3.2 question; write the report under the lock only over an older one; and build the context as §3.2 states. | P6 | `classify.TestResolve` (asserting the exact `owner` and `assistant` of each item, in both readers: a rewritten line; a tool result before the reply; a compaction summary between a finished turn and the reply, `intervening prompt`, and opening the answered turn, `owner` `""` with `opener` `compaction`; two owner turns in a row after a turn with text; a tool-only turn followed by two owner replies, both `no assistant text`; a server-created row opening a turn, `owner` `""`; a finished turn, then a server-created prompt, then the reply, `intervening prompt`; a task notification before the reply, `intervening prompt`; a source starting mid-turn; a missing file; the locator query, a thread query and the cwd query each failing), `classify.TestT3` (fake `sqlite3`: argv, failure, timeout), `classify.TestT3RealSQLite` (the real SQL on a database built with the real `sqlite3`: thread and message IDs holding `:`; two rows forming one locator, `ambiguous locator`; a numeric and a `null` payload `id`, the expected locators computed by observe's `jsonScalar` as oracle; a deleted thread; a missing message in an existing thread; an intervening owner row; tied `created_at`; both cwd sources. The authoritative job is `ubuntu-latest`, whose image lists SQLite; `macos-latest` uses the system `sqlite3`; the test logs `sqlite3 --version`, and with `CI=true` on those OSes fails rather than skipping), `classify.TestMemoryDir` (ordinary checkout, registered linked worktree, subdirectory, non-repository cwd; submodule, separate Git directory and one named `.git`, a gitfile pointing at another checkout's `.git`, a worktree not registered back → `unsupported layout`; removed cwd; no project directory → `new`; user `autoMemoryDirectory` absolute, with `~/`, relative → `unassessed`; either settings file setting it in the cwd, the checkout or the main checkout → `project memory override`, each with a decoy index where it points; malformed user settings; `CLAUDE_CONFIG_DIR`; inherited `GIT_DIR`; a decoy index at the cwd's encoding; the encoding on fixed POSIX and Windows paths), `classify.TestState` (oldest-first input sent newest first; IDs; the same bodies under two roots; an embedded path in a reply sent as written), `classify.TestPhases` (fake endpoint over two groups, order within a phase free: the exact multiset of attempted bodies per phase, group 1 `kind_`, group 1 `dup_`, group 2 `kind_`, group 2 `dup_`, each phase starting only after the previous one's requests all returned; one `{owner, assistant, reply}` per `dup_` body; no unqualified item; no body attempted twice), `classify.TestContext` (order, markers, the 10-line bound, `(+k more…)` with every candidate's excerpt in the report; `complete` and coverage for an empty healthy week, an empty week with a failed source, an all-unresolved week, a removed worktree; the duplicate check disabled: every candidate marked `duplicate check disabled`, `complete` true, the disabled line ending `; candidates are not checked against memory` on a week with candidates and on an empty week, and, with both decisions disabled, the duplicate-disabled line present without it, and a failed `kind_` request still `complete: false`), `classify.TestDupQuestion` (the `dup_r<n>` request: the state keys `memory_index`, `owner`, `assistant`, `reply` in order, and the question's name, type, instructions and both criteria exactly as §3.2 states them), `classify.TestPublish` (replacement; a failed rename keeps the old file whole and answers `latest-not-written`; two runs contending inside compare-and-rename; two runs finishing in reverse order, for different windows and for the same window, keep the newer and leave no temp file of the dropped run; the lock held past the bound → `latest-not-written`; a lock left by a killed process does not block; an empty week replaces an older report), `classify.TestRun` (missing and stale observe file → `pass`, no write; malformed, unknown `version` and a `window.until` 6 min ahead → `warn`; a week with `new` items → `pass`, no hit finding; a persistent failure (401) on one `kind_` and one `dup_` request beside successful ones → `warn`, those items `request failed` and not done, the other candidates kept; a paused write leaves the old file whole) |
| C10 | Calibration shall apply §3.5’s fit, eligibility, corpus and reporting rules, one cutoff rule for every decision. Changing only held-out scores shall leave the fitted cutoff unchanged and may disable the decision. When built, the router shall count distinct kits by ground truth and score wrong choices under §3.5’s false-positive and recall rules. For classify it shall check the corpus counts, splits, groups, languages, held-out `dup_kind` and `undo` quotas (`dup_kind` matching `dup` and implying `hard`) and the held-out pairs (one `en`, one `ja`), record `kind_` bodies and the `dup_` union, and fill `end_to_end` as §3.5’s table states for each combination of the two own gates; it shall enable qualification exactly when qualification passes its own gate and (the scored list on `heldout`, `new` or `qualified` with `new_recall`, scored with the provisional cutoffs, meets its target, or a matching owner acceptance holds), comparing figures by value. | P7 | `jev.TestCutoff`, `jev.TestCorpus`, `calibrate.TestReport`, `calibrate.TestClassifyCorpus` (a corpus one case short in each count; held-out `duplicate` 5 + 3 and `new` 3 + 5 by language, keeping 8 + 8 correcting; a held-out hard-negative kind missing; a held-out positive `dup_kind` missing; no held-out `undo` positive; a `dup_kind` whose polarity mismatches `dup`, and one on an item not `hard`; no antecedent pair in `heldout`, and two of one language, while `fit` meets the pair floor), `calibrate.TestClassify` (a first answer qualifying an unlabelled item records its `dup_` body; a qualified true-`new` item whose first `dup_` score is missing is a miss (not a score of 0), and one whose first `dup_` score equals the cutoff is `duplicate`, so not listed; each of the four own-gate combinations, asserting `list`, `scored`, `support`, `fn`, the reason and `REVIEW`; qualification finding all 8 held-out duplicates and missing all 8 `new` items ships qualification disabled both with the duplicate decision passing (`new` list empty) and with it disabled (`qualified` list at precision 1.0, recall 0.5, `new_recall` 0); the duplicate decision failing its gate with the `qualified` list meeting every target ships qualification enabled, the duplicate disabled and `list` `qualified`; qualification passing its 3-call held-out gate while its first-answer `qualified` list fails, from per-attempt scores; qualification eligible with an empty list; a recorded acceptance ships it enabled only after qualification passed its own gate, never after it failed, and a changed pin, `heldout` hash, figure or `list` alone voids it, while a `new_recall` of 0 round-trips through the table and still matches, and the table JSON itself has the `new_support` and `new_recall` keys, `new_recall: 0` included, with `list` `qualified` and neither key with `new`; the report prints O1's per-case lines for a held-out case with a `dup_kind` and for an `undo` positive without one, each with its ID, expected label, category and three attempt outcomes, a missing attempt shown as `missing`, and scores labelled as such when no cutoff fits; the end-to-end score uses the provisional cutoffs, before any enabled bit is set), `router.TestSamples` (when built) |
| C11 | Every answer record and cutoff table shall name the pin, and every status record shall match a rebuilt body hash; cutoffs refitted from `samples` shall equal the committed ones; changing the root dir, or the clock within observe's freshness, keeps every key, and changing state, a question or the pin changes it. | P7, §3.4 | `jev.TestFixturesPinned` (semantic and classify fixtures; for classify it rebuilds the §3.5 record union and checks that every body replay sends has a record) |
| C12 | Every `fixed` fixture and every recorded `cal` fixture (a pending one has no `expect` yet) shall, through `replay-test`, give exactly one self-answered result with empty `error`, its `expect` status, its `want` (`cal`: exact findings and context, classify: the artifact too; `fixed`: §3.5 matching, classify's including `complete` and the context), and no `jev-unrecorded` or `jev-error` unless wanted; once with the key unset and once with a dummy key and a test endpoint that fails the test on any request; a hit fixture with its records deleted shall fail the `want` assertion. The mixed fixture keeps the hit and reports the rejected batch's questions unassessed with no `jev-error`, and its live counterpart against a fake endpoint sends exactly one request per planned body. | P2–P9, P6 | `<ext>.TestFixtures`, `semantic.TestMixedLive` |
| C13 | No package under `cmd/` or `internal/` shall import `extensions/`, and the jev packages shall import no `internal/` package outside test helpers. | owner decision | existing `depguard` rules |
| C14 | Each live attempt shall try to record its outcome under §3.3’s ledger contract. Replay shall write no ledger line. | P10 | `jev.TestLedger` |
| C15 | A live `stack run outbound --ext public --ext jev-semantic --range <r>` with built binaries and a test endpoint answering a hit shall exit 0 with both named results (`public: pass`, semantic `warn`) on clean texts, and exit 1 with `public: block` and the semantic result on a texts hit. | P4 | `semantic.TestExitCodes` (non-replay) |
| C16 | When built, with built `stack` and `stack-jev-router`, a temporary home, test cutoffs, a test endpoint answering a pointer, no handover and the 9,500-unit restore budget filled by several blocks (worker rules, kits' `NORTH.md`, `state.md` and items), `stack hook claude session-start` on a `compact` payload shall produce each of the four §3.2 placement outcomes ([core entry placement](core-stages.md#43-entry-at-compaction)) in a separate case. In each case, removing the entry contribution from decoded `additionalContext` shall yield the same restore as `entry = []`. | P5 | `router.TestCompaction` (non-replay) |
| C17 | After a valid observe with a fresh `latest.json`, the dotfiles job shall call observe, `jev-classify`, then improve, in that order, and classify's outcome shall never fail the job. When observe and improve are valid, `last.json` shall list the results in call order with status `warn` when any is: all passing → status `pass` with classify's result listed; classify missing → under `skipped`; a classify-only `warn` with the others passing → status `warn`; exit ≠ 0 or invalid output → a `warn` result named `jev-classify` with `invalid output`; a synthetic `error` → a `warn` result keeping that error. With improve invalid, classify shall still have run and `last.json` stay unchanged (exit 2); with observe invalid, neither runs. | P11 | `tests/test_stack_cycle.py` in `shuymn/dotfiles` (fake `stack` on `PATH` recording calls, one case per outcome) |

## 7. Test plan

CI never reaches the network: the client against `httptest` servers (C2–C5, C14), the binaries built with a test
endpoint (C12, C15, C16), state building against synthetic repositories (`internal/gittest`, a test-only import). macOS
and Linux run them in `scripts/test`; Windows CI runs `go vet`, `go test -race` and the build
(`.github/workflows/ci.yml:40-55`). Classify's report replacement while a reader holds the old file open, which
Windows CI does not exercise, is checked on a Windows machine. Real-key checks are owner steps:

**O1** each PR: `jev-calibrate`; committed cutoffs equal its output; unstable list read; for classify, the per-case held-out duplicate outcomes, printed by the calibrate report and read only after the §3.5 freeze: for each held-out correcting item its ID, expected label, `dup_kind` and `undo`, and its three attempts' outcomes at the fitted cutoff (`duplicate`, `new`, or `missing`), or, when no cutoff fits, its three scores labelled as scores. **O2** the §3.2 caller on
a scratch PR deleting an assertion → warn, on a clean change → pass, and on a scratch PR whose base gained a test
after the branch point (base and head diverged) → no finding that the test was deleted. **O3** (when built) `kit open demo` with a state
reviewing a PR → pointer to `review.md`; `/compact` → restored when room is left. **O4** (after #56) `stack run cycle --ext jev-classify` after a real observe run with T3 and Claude replies: `complete` true, or false only with its cause shown, and elapsed under the 5-min budget; coverage gaps (`sources`, unresolved, lookup `unassessed`) checked against their causes, not counted as failure. Before opening the report, draw 40 items at random and label them by hand (kind, and duplicate against the index for correcting ones); then compare with the report, giving agreement per label with its support (support under 10 stated as such), and check every unresolved, unassessed and duplicate outcome in the sample against its source. **O5** `TYPESAFE_API_KEY=invalid` with input at
each stage with a built extension (entry once the router is built) → `warn` 401; network off → unassessed. **O6** after a week: `usage.jsonl` totals, unknown usage,
`max_tokens_exceeded` count, and coverage against the number of runs counted from each stage's own output (the cycle
log, hook runs), since a run with no attempt writes no ledger line.

## 8. Risks

- Synthetic fixtures are not the owner's real distribution, and Jev's quality on Japanese is unmeasured
  (https://docs.typesafe.ai/model-jaggedness.md); live precision may be lower and is not measured.
- The 48 KiB budget is an estimate; if it under-sizes, questions come back unassessed (O6 counts them).
- No deprecation policy for pinned IDs: a retired pin gives `warn` at every stage, the signal for the bump PR.
- The memory lookup rests on observed behaviour: the project-directory encoding and the T3 schema are not
  documented, and two paths can encode to one directory; managed settings, `--settings` files and
  `CLAUDE_CODE_PROJECT_DIR_NAME` of the session that wrote the memory are invisible to classify. A wrong directory
  shows as `new` (`no memory yet`) or a wrong `duplicate`; C9's decoy and O4's audit are the checks.
- `new` means coverage not established in the index as loaded at session start (a required rule uncovered, or the
  reply's rules or referent unsettled); a rule kept only in a topic file, or past the
  index's first 200 lines or 25 KB, reads as `new`. A repository that overrides the memory location in its own
  settings gets no duplicate check (`project memory override`).
- The published kind is advisory; qualification, the duplicate decision and the scored list (`new`, or `qualified`
  while the duplicate check is disabled) are calibrated.
- The `dup_` criteria name the kinds of related line seen among the `fit` cases; a line related in another way can
  still read as a match. The highest-scoring observed misses are a rule the agent's action also broke (`fit-a03`) and
  a line under another condition (`fit-c07`), both about 0.9 under the topic-only wording; `heldout-b` holds one
  case of each kind, and of `partial` and `multi`, which no `fit` case has, to measure them.
- The `dup_` question is long (about 1.5 KB, several interacting clauses) and System One answers it in one shot with
  no reasoning text, so a reply with a covering line can still read `new`: a recall risk, measured by the positive
  categories in O1.
- The launchd job needs the key for classify; without it classify opts out.

## 9. Owner questions

None. Every choice above follows from #57 and the owner decisions, or is a method choice stated with its reason.
