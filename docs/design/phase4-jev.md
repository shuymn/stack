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
δ = 0.05, and in one of its review rounds 2 of 11 findings were wrong, which is why every Jev answer here is advice
and never a block (§3.2).

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
| Nothing to ask (outbound: empty `range` or a diff with no files, since both questions need the diff; entry: no kit or no candidate; cycle: no `owner-reply` item) | `pass`, no context | none |
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

**Budgets.** Each extension's budget runs from process start: router 4 s, semantic 10 s, classify 5 min. The work
deadline is the process budget less a finalisation reserve (router 0.3 s, semantic 0.5 s, classify 20 s). Requests
end 0.1 s before it to leave room for the bounded ledger append (§3.3). At the deadline, remaining work is unassessed.
The reserve covers the response and classify's file. Children get a 0.2 s pipe wait and a timeout 0.2 s before the
work deadline, so killing git and reaping its pipes fit inside the budget. The response stays under 512 KiB, half core's 1 MiB stdout limit
(`internal/ext/ext.go:211`): further findings become one `jev-omitted` count, context lines are cut with a marker.
Core's deadlines are separate (`internal/ext/ext.go:47-50`), one per stage run and shared by its extensions in
order: entry 10 s at `kit open`, at compaction `min(10 s, time left to the hook's 55 s cut-off) − 2 s`, run when
≥ 1 s remains (`internal/session/session.go:255-289`); outbound 120 s from the start of the stage. In a run of the
default selection (`public`, then `jev-semantic`), semantic gets what `public` leaves of that window: `public` stops
its scan 100 s after its own start (`extensions/public/stage.go:29-40`), usually leaving semantic ≥ 18 s. hiden's
caller runs `--ext jev-semantic` alone (below), so semantic starts at the beginning of the window; cycle 30 min. When core cuts a child it records a synthetic `warn`, distinct from a self-answered partial
result; at compaction the router's stderr is discarded.

### 3.2 What each extension sends and returns (item 2)

All requests use `model: "jev-1.14.0"`; generated metadata in them has no wall-clock time or local absolute path.

**`jev-semantic` (outbound).** Read from `repo` (the cwd when `""`) with two git commands: the messages of
`<range>`, one per commit, newest first, and the patch of `<range>` with three lines of context, `a/`/`b/` prefixes,
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
(`home/dot_claude/skills/hiden/references/commit-pr.md`, step 2) writes the title and body to files and runs
`stack-public check <PR base>..<head> --title-file <t> --body-file <b> --branch <branch>` before each push and PR
create or edit. When a PR is created or edited, that check passed and `stack-jev-semantic` is on `PATH`, it pins
`head` to a commit, resolves `mb=$(git merge-base <PR base> <head>)` (the comparison GitHub shows for a PR) and runs
`stack run outbound --ext jev-semantic --range <mb>..<head>` with the same three text flags; with no merge base it
skips semantic and says so. The agent reports the findings, and the exit code does not stop the push. Starting the
range at the merge base keeps a test that only the base added from showing as deleted. CI is not wired (it would
need the key as a secret). The caller is agent instruction text without an automated test; its merge-base step is
checked by O2 and the extension side by the diverged-base fixture (P2).

**`jev-router` (entry).** Deferred until the trigger below. Hiden already routes by step type and calls for
re-reading the reference after compaction. The router targets a missed re-read of a single step named in restored kit
state. Both `kit open` and compaction provide kit state without a prompt. Observe's window-wide `reference-unread`
signal cannot establish whether the reference was read before that step.

**Prerequisite.** Each kit restores an orchestrator rule to re-read the hiden reference of the step in progress
after compaction, as an item of its `attention/items.json` in the format of
[`phase2-core.md` §4.1.1](phase2-core.md#411-kit-context-renderer-shared-with-compact-restore); hiden's kit
convention puts it there and words it. It counts only for a kit whose restore output shows it in its
`orchestrator-attention-items` block; a kit whose file renders no such item does not count. **Trigger:** two distinct
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

**`jev-classify` (cycle).** Input: the observe file (`.local/state/stack/observe/latest.json` under the home
directory), fresh when `window.until` is within 24 h of classify's clock (missing or stale → `pass` saying so;
unparsable or unknown `version` → `warn`). Its `owner-reply` items follow observe's rule
(`extensions/observe/transcripts.go:369-375`, `sources.go:93-104`), two locator forms, resolved grouped by source:

- `path:line` (Claude transcripts), split on the last `:`: the user record at that line gives the reply; the text
  blocks of the assistant records since the previous genuine user turn (a user record that is not a tool result,
  as observe skips them, `extensions/observe/transcripts.go:318-322`) give the assistant turn.
- `t3:<thread>:<message>`: one `sqlite3 -readonly -json` query per thread on the T3 database observe reads gives the
  message and the assistant message before it.

An item whose source, record or preceding assistant turn is gone is `unresolved`, reported separately with its
reason. Units are items `{id, assistant, reply}` in source order; `kind_<id>` is a `choice` of `correction` ("says
the agent's last action or claim was wrong and what to do instead"), `drift` ("brings the agent back to the requested
scope or goal it left"), `approval`, `other`. One decision function, used at runtime, in calibration and on held-out
cases: an item **qualifies** as `correction` or `drift` when `probabilities[label]` reaches that label's cutoff (both
→ the higher), else it is `unqualified` with its raw `choice` reported. Only qualified items get the duplicate check.

**Duplicates.** Claude Code keeps auto memory in `projects/<project>/memory/` under the Claude configuration
directory, `<project>` derived from the Git repository so all worktrees share it, `MEMORY.md` its index, or
`autoMemoryDirectory` when set (https://code.claude.com/docs/en/memory "Storage location"). Classify resolves the
repository from the record's `cwd` (`git rev-parse --path-format=absolute --git-common-dir`, its parent), encodes it
as Claude Code names project directories (non-`[A-Za-z0-9]` → `-`; **assumption**, observed) and honours the user
settings' `autoMemoryDirectory`. Question `dup_<id>` `noul` on `{memory_index,
replies: [{id, reply}]}`: "Does a line of `memory_index` already state the rule that reply `<id>` in `replies`
corrects?" Outcome: `duplicate` (at or above the cutoff), `new` (asked, below it), `unassessed` (no index, cwd or
repository gone, unresolvable T3 cwd, transient or too large, deadline). Only the index is compared.

Output: `.local/state/stack/jev-classify/latest.json` (0600, temp + rename) from every run that read a fresh observe
file: `{version, run: {id, started, finished}, window, complete, model, items: [{id, where, status:
classified|unresolved|unevaluated, outcome, choice, probabilities, duplicate, dup_p, reason}], counts, usage}`; a
run cut by its budget writes `complete: false`; an earlier file is dated by its `window` and `run.finished`.
Context: counts, up to 10 `new` qualified items (`where`, ≤ 160-char excerpt), unresolved and unevaluated counts.

**Cycle wiring (dotfiles).** The supported path, scheduled or manual, is the job's dedicated call after improve,
`stack run cycle --ext jev-classify` (`docs/design/phase3-cycle.md` §4.8); observe's and improve's results stay
authoritative: missing → `skipped`; self-answered → merged; exit ≠ 0, invalid output or a synthetic `error` → a
merged `warn` result with that `error`, and the job still exits 0.

**Open for the `jev-classify` design round:** a T3 reader or `sqlite3` fake and a fixture memory index with a known duplicate for replay; packing the index
against the replies; an item split into parts whose answers disagree or are missing; a fake-`stack` wrapper test.

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
fixture holds exactly the live run's bodies. With `jev` present the extension never calls the network, key or not;
a body with no record → `warn` `jev-unrecorded`. Machine inputs come from the fixture: `repo` (a synthetic repository
the test builds with fixed identity and dates, refs `jev-mb`, `jev-head`, a diverged base; semantic's cwd);
`jev.references` (the reference stub directory) and `kits` relative to the cwd; `jev.observe` and `jev.now`. `<REFS>`
in `want` expands to the stub directory; classify's `run.id` is asserted non-empty then normalised, its times come
from `jev.now`. Core reads only `request` and `expect`.

### 3.5 Pinned model and cutoffs (item 3)

Pin `jev-1.14.0` in `extensions/internal/jev`; `jev-latest` is never sent and no config overrides the pin. Fixtures
are public, so every case is synthetic (a small repository plus texts, a kit and reference stubs, transcript and
memory excerpts), Japanese and English. Labels are written by hand with a one-line `why` and reviewed by the owner.
Ground truth is bound to the scored unit, derived from per-unit labels with the live packing:

| Decision | Scored unit | Ground truth | Score | Cutoffs |
|---|---|---|---|---|
| semantic meta-duty | one request (cases that split a file are fixed-only, not fitted) | any of its files labelled true | `noul` | 1 |
| semantic unsupported claim | the whole-PR request | case label | `noul` | 1 |
| router current reference | kit (distinct kit states) | the labelled reference or `none` | `probabilities[choice]` of `current_ref`, stored before any cutoff, mapped below | 1 |
| classify qualification | item | item label | `probabilities["correction"]`, `probabilities["drift"]` | 1 each, via the decision function |
| classify duplicate | (reply, index) | item label | `noul` | 1 |

`jev-calibrate` (owner, with the key) builds each `cal` case's synthetic repository and requests with the extension's own
builders, sends them through the live transport 3 times, writes the first answer (or status) to `records` and all
three scores to `samples` (`null` for an attempt with no score), then refits; when the first attempt has no
replayable outcome (e.g. a timeout before headers), it writes no fixture and no table and exits non-zero asking for a rerun. Fitting uses the `fit` cases, each single-call score one sample (live
decides on one call; a planned call with no score, e.g. a size rejection, is a negative prediction, never
dropped): the cutoff `t` is the lowest of 0.50–0.95 (step 0.01) that passes a margin test with the predeclared
δ = 0.05, where a positive is a hit only at a score ≥ t + δ, a negative is a false positive already at ≥ t − δ and a
missing score stays a miss, with precision ≥ the target (semantic 0.8, classify 0.8, router 0.7), true positives > 0
and recall ≥ 0.5. The floor stays 0.50 because a score under it leans no, and easy negatives would otherwise pull the
cutoff down to where live false positives rise. The runtime decision stays score ≥ t; samples within ±0.05 of it are
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
so a decision that ships disabled does not break them (shipped eligibility is C10/C11's). Replay uses the recorded transport. **Model
bump:** one PR changes the pin, re-records, refits, and reports old vs new on `heldout` (precision, recall, support,
router pointer hits). The corpus measures regressions on itself; the ledger does not measure live precision.

### 3.6 Cost and latency estimates (item 6)

From the published price, the §2 sizing estimate and §3.1 packing. Estimates, not measurements:

| Extension | Typical run | Requests | Cost per run | Budget (own / core) |
|---|---|---|---|---|
| semantic | 20 KiB PR | ⌈diff bytes / (48 KiB − texts)⌉ ≈ 1 | ≈ $0.0003; a 112 KiB PR ≈ 3 requests ≈ $0.0012 | 10 s / 120 s shared |
| router (when built) | one kit of 2–13 KB, 12 candidates (≈ 6 KiB question) | 1 per kit | ≈ $0.0002–0.0006 | 4 s / 10 s (≥ 1 s at compaction) |
| classify | 150 items, ≈ 2 KiB each | ⌈300 KiB / 48 KiB⌉ + memory ≈ 10 | ≈ $0.008 | 5 min / 30 min |

With all three built, about 100 pushes, 300 kit opens and compactions and 30 cycles a month ≈ $0.4, uncapped; O6 reports what the ledger
saw.

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
| P6 | When observe found owner replies, `jev-classify` shall classify each resolvable one, check qualified ones for duplicates, and list unresolved ones separately. (After #56.) | #57 | both locator forms + memory index | its file and context | `replay-test` incl. the artifact; key: O4 |
| P7 | Each extension shall send only the pinned model, with cutoffs from calibration on `fixtures/jev/`. | #57 | any call | `model` `jev-1.14.0` | C10, C11; O1 |
| P8 | When a fixture carries recorded answers, the extension shall answer from them with no network, and `warn` `jev-unrecorded` on a miss. | #57 | key unset; dummy key + failing endpoint | `want` met; deleted record → feature assertion fails | C12 |
| P9 | If Jev fails or answers malformed, the stage shall continue: unassessed on transient or too large, `warn` on persistent, within the budgets. | §3.1 | fake server | per §3.1 | C2, C4; key: O5 |
| P10 | Each live attempt shall try to add one ledger line within its bounded wait; cost is observed, not asserted. | #57 | real runs | lines with known/unknown usage | C14; key: O6 |

### 6.2 Plan layer

| ID | EARS claim | Source | Test |
|---|---|---|---|
| C1 | Each built extension shall follow §3.1’s key precedence, opt-out and key-error contract after reading stdin to EOF. Replay shall skip key lookup and reject `jev: null` without a live call. | P1 | `jev.TestOptOut` per binary, `jev.TestKey*`, `semantic.TestKeyFile` |
| C2 | The client shall apply §3.1’s request-level and question-level failure rules, including partial HTTP 400 bodies, and shall omit response body contents from output. | P9 | `jev.TestClientErrors` (`httptest`, slow body) |
| C3 | Packing shall use the budget 48 KiB less the fixed fields, split an oversize unit into ordered parts measured in their complete encoded state, keep questions ≤ 16 KiB, send input that fits whole even under a 1 KiB budget, mark questions unassessed only when splitting is needed and the budget is under 1 KiB or the smallest part cannot fit, and lose no unit; the router never splits a kit (§3.2) and uses packing only to measure it. | §3.1 | `jev.TestPack` (25 KiB texts with a small diff; a file over the budget; budgets just above and below 1 KiB, with a small diff that fits whole below it; escaped runes and `part k of n` framing at the boundary; captured bodies reassemble every unit) |
| C4 | Each extension shall stop at its work deadline, report the rest unassessed (classify: `complete: false`), answer within its reserve, and keep the response under 512 KiB with `jev-omitted`; core's synthetic `warn` under a shorter parent deadline is a separate outcome. | §3.1 | `<ext>.TestBudget`: slow git → no request, questions unassessed (`preparation deadline`, classified by the deadline before the exit code), answer by 10 s, with a child holding its pipe open so a longer pipe wait fails; slow HTTP → answered batches kept, rest unassessed; many-batch result accepted by core; shortened core deadline |
| C5 | Invalid stdin → `warn`; another stage, nothing to ask (empty range, a range whose diff has no files, no kits or no router candidate, no items) → `pass` with no request; results assemble in unit order under out-of-order completion. | §3.1 | `<ext>.TestNothingToAsk`, `jev.TestOrder` |
| C6 | `jev-semantic` shall build identical bodies under config variants (system, global, `GIT_CONFIG_PARAMETERS`, repository-local `diff.suppressBlankEmpty`, `diff.noprefix`, `diff.relative`, `diff.indentHeuristic` on a patch whose hunks move, `core.abbrev`, `diff.orderFile`), ask `unsupported_claim` only when the whole PR fits, and answer `warn` before any request on a git failure. | P2, P3 | `semantic.TestState` (synthetic repository) |
| C7 | A probability just below, at and above the cutoff shall decide pass, hit, hit; a disabled decision never flags; never `block`. | P2–P4 | `semantic.TestDecision` |
| C8 | When built, the router shall follow §3.2’s candidate, state and decision rules. Its output shall satisfy the UTF-16 budget with astral characters, preserve whole paths and report unassessed kits as specified there. | P5 | `router.TestCandidates`, `router.TestState`, `router.TestContext` |
| C9 | `jev-classify` shall resolve both locator forms (a tool result between the assistant text and the reply is skipped), report unresolved items, use the fixture clock, send only qualified items to `dup_` with their reply and ID, and write its file atomically. | P6 | `classify.TestRun` (missing, stale, malformed observe file; paused write and failed rename leave the old file whole) |
| C10 | Calibration shall apply §3.5’s fit, eligibility, corpus and reporting rules. Changing only held-out scores shall leave the fitted cutoff unchanged and may disable the decision. When built, the router shall count distinct kits by ground truth and score wrong choices under §3.5’s false-positive and recall rules. | P7 | `jev.TestCutoff`, `jev.TestCorpus`, `calibrate.TestReport`, `router.TestSamples` (when built) |
| C11 | Every answer record and cutoff table shall name the pin, and every status record shall match a rebuilt body hash; cutoffs refitted from `samples` shall equal the committed ones; changing the root dir or clock keeps every key, and changing state, a question or the pin changes it. | P7, §3.4 | `jev.TestFixturesPinned` |
| C12 | Every `fixed` fixture and every recorded `cal` fixture (a pending one has no `expect` yet) shall, through `replay-test`, give exactly one self-answered result with empty `error`, its `expect` status, its `want` (`cal`: exact findings and context, classify: artifact; `fixed`: §3.5 matching), and no `jev-unrecorded` or `jev-error` unless wanted; once with the key unset and once with a dummy key and a test endpoint that fails the test on any request; a hit fixture with its records deleted shall fail the `want` assertion. The mixed fixture keeps the hit and reports the rejected batch's questions unassessed with no `jev-error`, and its live counterpart against a fake endpoint sends exactly one request per planned body. | P2–P9 | `<ext>.TestFixtures`, `semantic.TestMixedLive` |
| C13 | No package under `cmd/` or `internal/` shall import `extensions/`, and the jev packages shall import no `internal/` package outside test helpers. | owner decision | existing `depguard` rules |
| C14 | Each live attempt shall try to record its outcome under §3.3’s ledger contract. Replay shall write no ledger line. | P10 | `jev.TestLedger` |
| C15 | A live `stack run outbound --ext public --ext jev-semantic --range <r>` with built binaries and a test endpoint answering a hit shall exit 0 with both named results (`public: pass`, semantic `warn`) on clean texts, and exit 1 with `public: block` and the semantic result on a texts hit. | P4 | `semantic.TestExitCodes` (non-replay) |
| C16 | When built, with built `stack` and `stack-jev-router`, a temporary home, test cutoffs, a test endpoint answering a pointer, no handover and the 9,500-unit restore budget filled by several blocks (worker rules, kits' `NORTH.md`, `state.md` and items), `stack hook claude session-start` on a `compact` payload shall produce each of the four §3.2 placement outcomes ([core entry placement](core-stages.md#43-entry-at-compaction)) in a separate case. In each case, removing the entry contribution from decoded `additionalContext` shall yield the same restore as `entry = []`. | P5 | `router.TestCompaction` (non-replay) |

## 7. Test plan

CI never reaches the network: the client against `httptest` servers (C2–C5, C14), the binaries built with a test
endpoint (C12, C15, C16), state building against synthetic repositories (`internal/gittest`, a test-only import). macOS
and Linux run them in `scripts/test`; Windows CI runs `go vet`, `go test -race` and the build
(`.github/workflows/ci.yml:40-55`). Real-key checks are owner steps:

**O1** each PR: `jev-calibrate`; committed cutoffs equal its output; unstable list read. **O2** the §3.2 caller on
a scratch PR deleting an assertion → warn, on a clean change → pass, and on a scratch PR whose base gained a test
after the branch point (base and head diverged) → no finding that the test was deleted. **O3** (when built) `kit open demo` with a state
reviewing a PR → pointer to `review.md`; `/compact` → restored when room is left. **O4** (after #56) `stack run cycle --ext jev-classify` after
a real observe run with T3 and Claude replies; spot-check outcomes. **O5** `TYPESAFE_API_KEY=invalid` with input at
each stage with a built extension (entry once the router is built) → `warn` 401; network off → unassessed. **O6** after a week: `usage.jsonl` totals, unknown usage,
`max_tokens_exceeded` count, coverage against runs.

## 8. Risks

- Synthetic fixtures are not the owner's real distribution, and Jev's quality on Japanese is unmeasured
  (https://docs.typesafe.ai/model-jaggedness.md); live precision may be lower and is not measured.
- The 48 KiB budget is an estimate; if it under-sizes, questions come back unassessed (O6 counts them).
- No deprecation policy for pinned IDs: a retired pin gives `warn` at every stage, the signal for the bump PR.
- The memory key encoding and the T3 schema are observed, not documented; a change shows as `unassessed`
  duplicates or `unresolved` items. The launchd job needs the key for classify; without it classify opts out.

## 9. Owner questions

None. Every choice above follows from #57 and the owner decisions, or is a method choice stated with its reason.
