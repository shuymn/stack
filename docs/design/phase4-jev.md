# Phase 4 Design: Jev Extensions (`jev-semantic`, `jev-router`, `jev-classify`)

Design for issue #57: the optional Jev extensions of `docs/architecture.md` §4, on its stage table and the §3
protocol, at `668ceb6`. Code is cited as `path:line`; Jev facts by URL, and what no primary page confirms is marked
**assumption** (checked with a real key, §7). Code lives under `extensions/`, fixtures under `fixtures/jev/`; core
(`cmd/stack`, `internal/`) does not change.

## 1. Purpose and scope

Jev is TypeSafe's "System One" model: it answers typed questions about one `state` with calibrated probabilities and
writes no text. Three advisory extensions use it: `jev-semantic` flags two kinds of PR risk at `outbound`,
`jev-router` points the agent at the skill references it likely needs at `entry`, and `jev-classify` labels the
week's owner replies at `cycle`. None can block, none is required, and with no key each answers `{"status":"pass"}`.

**In scope:** the three extensions, a shared client, calibration on `fixtures/jev/`, key-less `--replay`, a usage
ledger; in the implementation PRs `docs/architecture.md` §4 (opt-out, `jev-semantic`'s caller), §5 (`jev.toml`,
router PR), §6 item 4 (§9) and the README install line; two `shuymn/dotfiles` PRs wiring the callers (§5). **Out of
scope:** core changes; any gate, redaction, cut or limit on what is sent (owner decision: the key is the opt-in, so
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
| Prior art: pinned `jev-1.14.0`, "at or above" cutoffs fitted per rule, verdicts replayed with no key; pass-to-pass spread median 0.010, p90 0.050, max 0.300; one review round had 2 of 11 findings wrong | https://github.com/mizchi/jev-lint, its `docs/reference.md` |

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
| `TYPESAFE_API_KEY` unset or empty and no `jev` object (§3.4) | `pass`, no context, no file read or written | none |
| Nothing to ask (outbound: empty `range`, since both questions need the diff; entry: no kit and empty `prompt`; cycle: no `owner-reply` item) | `pass`, no context | none |
| Preparation fails (semantic: a git command exits non-zero, is cut or times out) | `warn`, finding `jev-error` naming the command and exit | none |
| A request fails transiently (connect or TLS error, timeout, 429, 529, any 5xx) or is too large (400 `max_tokens_exceeded`, 413) | its questions are unassessed; one stderr line `jev: <n> requests skipped: <cause>` | no retry, no re-split |
| A request fails persistently: any other non-200 (400, 401, 402, 403, 404, 415, 422, …), a body not of the §2 shape, `model` ≠ the pin, a missing or wrongly typed answer, a probability not finite in [0, 1], a body over 1 MiB | `warn`, finding `jev-error` (`detail`: status or fixed cause) and one context line naming the cause and the action (`jev-router: Jev answered 401: check TYPESAFE_API_KEY`) | — |

**Unassessed** work never changes the status: it is a finding `jev-unassessed` (`detail`: which questions, how
much, why) and a context line. A `warn` means the owner must act (a wrong key, a changed API, a retired pin, a git
failure), so an outage does not cry wolf; it loses that run's advice. The key comes only from the environment (as
jev-lint). Details are fixed strings and the HTTP status, never the body, which can quote input.

**Batching.** Provider limits shape batches and never drop input. Packing is decided before sending, from encoded
sizes alone, by one function in `extensions/internal/jev` (also used by §3.6 and the tests). Each request carries
the fixed fields its questions need (semantic: the texts) and units in a fixed order (diff files, kits, items), packed
greedily up to a unit budget of 48 KiB less the encoded fixed fields; a larger unit is split by lines (a longer line
at UTF-8 boundaries) into parts marked `part k of n` that fit. When the fixed fields alone leave no budget, the
questions that need them are unassessed. Encoded questions stay ≤ 16 KiB per request; more go to another request
on the same state. Every sent body is thus determined by the input alone. Up to 4 requests are in flight; results
are assembled in unit order, whatever order they complete in.

**Budgets.** Each extension's budget runs from process start: router 4 s, semantic 10 s, classify 5 min. The work
deadline is the budget less a finalisation reserve (0.3 s, 0.5 s, 20 s) and bounds every cancellable step: git, file
reads, requests, ledger appends; when it passes, the rest is unassessed and the reserve assembles the response (and
classify's file). The encoded response stays under 512 KiB, half core's 1 MiB stdout limit (`internal/ext/ext.go:211`):
findings past it become one `jev-omitted` finding counting them, context lines are cut with a marker. Core's
deadlines are separate (`internal/ext/ext.go:47-50`): entry 10 s at `kit open`, at compaction the earlier of 10 s and
the hook's 55 s cut-off less 2 s, run when ≥ 1 s remains (`internal/session/session.go:255-289`); outbound 120 s
after `public`, whose scan stops at 100 s (`extensions/public/stage.go:29-40`), so semantic usually starts with
≥ 18 s left; cycle 30 min for classify's own call. When core cuts a child it records a synthetic `warn` (`timed out:
stage deadline …`), a distinct outcome from a self-answered partial result; at compaction the router's stderr is
discarded.

### 3.2 What each extension sends and returns (item 2)

All requests use `model: "jev-1.14.0"`. Metadata the extensions generate in sent fields holds no wall-clock time and
no local absolute path; source text is sent as it is.

**`jev-semantic` (outbound).** Read from `repo` (the cwd when `""`). Git runs with the repository-location variables
dropped (as `extensions/internal/checkpub/checkpub.go:244-253`), `GIT_CONFIG_PARAMETERS`, `GIT_CONFIG_COUNT` and its
`GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*` dropped, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL` the null device, and
`-c` overrides that beat repository-local config: `core.quotePath=false`, `diff.noprefix=false`,
`diff.mnemonicPrefix=false`, `diff.suppressBlankEmpty=false`, `diff.relative=false`, `diff.renames=false`,
`diff.context=3`, `diff.interHunkContext=0`, `diff.algorithm=myers`, `diff.orderFile` neutralised,
`i18n.logOutputEncoding=UTF-8`. Commands: `git log --encoding=UTF-8 --no-show-signature --format=%B%x00 <range>`
(messages split on NUL into `commits`) and `git diff --text --no-ext-diff --no-textconv --no-color --no-renames
--no-relative --ignore-submodules=none --submodule=short --full-index --src-prefix=a/ --dst-prefix=b/ -U3 <range>`
(`--full-index` so `core.abbrev` and object count cannot change `index` lines); output is LF-normalised. Fixed
fields are `title`, `body`, `branch` and `commits`; units are the diff's files in diff order. Two `noul` questions:

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
(`home/dot_claude/skills/hiden/references/commit-pr.md`, step 2) already writes the title and body to files and runs
`stack-public check <PR base>..<head> --title-file <t> --body-file <b> --branch <branch>` before each push and PR
create or edit; that call is unchanged. A dotfiles PR adds, after it passes: pin `head` to a commit, resolve
`mb=$(git merge-base <PR base> <head>)` (the comparison GitHub shows for a PR), and run `stack run outbound --ext
jev-semantic --range <mb>..<head>` with the same three text flags; the agent reports the findings and the exit code
does not stop the push. `stack run outbound` already accepts these flags (`docs/architecture.md` §3); no core
change. A bare `@{upstream}..HEAD` run is not the PR comparison; CI is not wired (it would need the key as a secret).

**`jev-router` (entry).** Candidates are the files matching `references` globs from the router's `jev.toml`
(`.config/stack/jev.toml` under the home directory, optional; broken → `warn` and the defaults), default
`skills/*/references/*.md`, under the Claude configuration directory (`$CLAUDE_CONFIG_DIR`, else `.claude` joined
to `os.UserHomeDir()`). They are slash-normalised relative to that directory, de-duplicated and sorted; questions
`ref_<n>` are numbered in that order. Each candidate's summary is its first heading plus first paragraph (LF,
≤ 400 bytes at a UTF-8 boundary; it describes the candidate, it is not evidence). State units, each its own state:
the `prompt` when non-empty, and per kit of the request's `kits` its `slug` with `NORTH.md` and `state.md`
(`internal/kit/render.go:35`) read whole, LF-normalised; the kit `dir` is used to read, never sent. One `noul` per
candidate: "Will the agent working on this state need the reference `<skill>/references/<file>` for its next step?
It covers: <summary>." A candidate's probability is its maximum over the requests. Kits reach the router only with
a valid `STACK_SESSION_ID` (`internal/cli/run.go:260-264`); `kit open` without one sends `kits: []`.

Answer: candidates at or above the cutoff, by probability, ties by path, at most 3, as context:

```text
jev-router (jev-1.14.0): references likely needed now; read each before the step it covers.
- <Claude config dir>/skills/hiden/references/review.md (0.91): <first heading>
```

Paths are the absolute local paths read (local context), never cut: a line over 300 UTF-16 units drops its summary,
and a line that would take the context over 1,000 units is dropped whole (units as core counts,
`internal/session/budget.go:14-16`). Partial coverage adds `(n of m inputs not scored)`, alone when nothing hit; no
hit with full coverage → `pass`, no context. Placement at compaction is best-effort
(`internal/session/session.go:292-309`).

**`jev-classify` (cycle).** Input: the observe file (`.local/state/stack/observe/latest.json` under the home
directory, `extensions/observe/observe.go:601`), fresh when its `window.until` is within 24 h of classify's clock
(missing → `pass` "no observation"; stale → `pass` "stale observation"; unparsable or unknown `version` → `warn`).
Its `owner-reply` items follow observe's rule (`extensions/observe/transcripts.go:369-375`,
`extensions/observe/sources.go:93-104`), with two locator forms, resolved grouped by source:

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
repository from the record's `cwd` (`git rev-parse --path-format=absolute --git-common-dir`, its parent) and encodes
it as Claude Code names project directories (every character outside `[A-Za-z0-9]` → `-`; **assumption**,
observed), honouring `autoMemoryDirectory` in the user settings. Question `dup_<id>` `noul` on `{memory_index,
replies: [{id, reply}]}`: "Does a line of `memory_index` already state the rule that reply `<id>` in `replies`
corrects?" Outcome: `duplicate` (at or above the cutoff), `new` (asked, below it), `unassessed` (no index, cwd or
repository gone, unresolvable T3 cwd, transient or too large, deadline). Only the index is compared.

Output: `.local/state/stack/jev-classify/latest.json` (0600, temp + rename) from every run that read a fresh observe
file: `{version, run: {id, started, finished}, window, complete, model, items: [{id, where, status:
classified|unresolved|unevaluated, outcome, choice, probabilities, duplicate, dup_p, reason}], counts, usage}`; a
run cut by its budget writes `complete: false`; an earlier file is dated by its `window` and `run.finished`.
Context: counts, up to 10 `new` qualified items (`where`, ≤ 160-char excerpt), unresolved and unevaluated counts.

**Cycle wiring (dotfiles).** The supported path, scheduled or manual, is the job's dedicated calls: after improve,
`stack run cycle --ext jev-classify`, so classify never takes improve's time (`docs/design/phase3-cycle.md` §4.8).
Observe's and improve's results stay authoritative: binary missing → under `skipped` in `last.json`; self-answered
`pass`/`warn` → merged; exit ≠ 0, invalid output or a synthetic `error` → a merged `warn` result with that
`error`, and the job still exits 0.

**Settled in the PR 2 / PR 3 design round** (open here): router compaction tests with full, partial and no room;
a fixture-backed T3 reader or `sqlite3` fake for replay; packing the memory index against the replies; an item
split into parts whose answers disagree or are missing; a fake-`stack` test of the wrapper's classify outcomes.

### 3.3 Layout and shared client (item 5)

```text
extensions/internal/jev/          client, packing, record lookup, usage ledger, cutoff selection
extensions/jev/semantic/          package + cmd/stack-jev-semantic/   (name in [stages]: jev-semantic)
extensions/jev/router/            package (incl. jev.toml) + cmd/stack-jev-router/
extensions/jev/classify/          package + cmd/stack-jev-classify/
extensions/jev/cmd/jev-calibrate/ owner-run, needs the key; not a stage binary
fixtures/jev/<extension>/{fixed,cal}/<case>.json
```

One shared package holds transport, validation, packing, replay, metering, cutoff math and the decision functions;
state building, router configuration and classify's file stay with their extension. `net/http` only, bound by the
work deadline; no SDK. The endpoint is a link-time variable only test builds set (`-ldflags -X`), never
configuration. Each body is marshalled once; those bytes are hashed (§3.4) and sent. **Ledger:** each live attempt
appends `{run, time, extension, model, outcome, elapsed_ms, input_tokens}` (`null` without `usage`) to
`.local/state/stack/jev/usage.jsonl`; `run` is a random per-process ID; appends are serialised; an I/O error is one
stderr line and changes no status. Replay never writes it.

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
fixture holds exactly the bodies the live run sends. With `jev` present the extension never calls the network, key
or not; a body with no record → `warn` `jev-unrecorded`. Machine inputs come from the fixture: `repo`, a synthetic
repository the test builds (fixed identity and dates, refs `jev-mb`, `jev-head`, a diverged base) as semantic's cwd;
`jev.references` (router globs over fixture reference stubs) and `kits` dirs relative to the cwd; `jev.observe` and
`jev.now` (classify's input and clock). `<REFS>` in `want` expands (`filepath.Join`) to the stub directory;
classify's artifact is compared with `run.id` asserted non-empty and normalised and run times from `jev.now`. Core
reads only `request` and `expect`; the tests read the rest.

### 3.5 Pinned model and cutoffs (item 3)

Pin `jev-1.14.0` in `extensions/internal/jev`; `jev-latest` is never sent and no config overrides the pin. Fixtures
are public, so every case is synthetic (a small repository plus texts, a kit and reference stubs, transcript and
memory excerpts), Japanese and English. Labels are written by hand with a one-line `why` and reviewed by the owner.
Ground truth is bound to the scored unit, derived from per-unit labels with the live packing:

| Decision | Scored unit | Ground truth | Score | Cutoffs |
|---|---|---|---|---|
| semantic meta-duty | one request | any of its files labelled true | `noul` | 1 |
| semantic unsupported claim | the whole-PR request | case label | `noul` | 1 |
| router relevance | (state unit, candidate) | the unit's relevant-candidate list | `noul` | 1 shared by all candidates |
| classify qualification | item | item label | `probabilities["correction"]`, `probabilities["drift"]` | 1 each, via the decision function |
| classify duplicate | (reply, index) | item label | `noul` | 1 |

`jev-calibrate` (owner, with the key) builds each case's synthetic repository and requests with the extension's own
builders, sends them through the live transport 3 times, writes the first answer (or status) to `records` and all
three scores to `samples`, then refits. Fitting uses the `fit` cases, each single-call score one sample (live
decides on one call): the cutoff is the lowest of 0.50–0.99 (step 0.01) with precision ≥ the target (semantic 0.8,
classify 0.8, router 0.7), true positives > 0 and recall ≥ 0.5; samples within ±0.05 are listed as unstable. Each
decision needs ≥ 6 + 6 positive and negative `fit` cases and ≥ 3 + 3 `heldout` cases, and must meet its precision
target and recall ≥ 0.5 on `heldout`; otherwise it ships disabled and its context says so.
Calibrate refreshes `expect` and `want` of the `cal` fixtures from the first recorded answer; the `fixed` semantic
fixtures (meta-duty hit, unsupported-claim hit, clean, diverged base) keep hand-written `want` it never rewrites, so
lost detection fails them. Replay uses the recorded transport. **Model bump:** one PR changes the pin, re-records,
refits, and reports old vs new on `heldout` (precision, recall, support, router top-3 hits). The synthetic corpus
measures regressions on itself; the ledger does not measure live precision.

### 3.6 Cost and latency estimates (item 6)

From the published price, the §2 sizing estimate and §3.1 packing. Estimates, not measurements:

| Extension | Typical run | Requests | Cost per run | Budget (own / core) |
|---|---|---|---|---|
| semantic | 20 KiB PR | ⌈diff bytes / (48 KiB − texts)⌉ ≈ 1 | ≈ $0.0003; a 112 KiB PR ≈ 3 requests ≈ $0.0012 | 10 s / 120 s shared |
| router | one kit, 31 candidates | per state unit, ⌈questions / 16 KiB⌉ ≈ 1–2 | ≈ $0.0005 | 4 s / 10 s (≥ 1 s at compaction) |
| classify | 150 items, ≈ 2 KiB each | ⌈300 KiB / 48 KiB⌉ + memory ≈ 10 | ≈ $0.008 | 5 min / 30 min |

About 100 pushes, 300 kit opens and 30 cycles a month ≈ $0.4, uncapped; O6 reports what the ledger saw.

## 4. Alternatives

| Choice | Option | Gains | Gives up | Reversibility / when it flips |
|---|---|---|---|---|
| Failure (3.1) | **transient → unassessed, persistent → warn (chosen)** | warn only when the owner must act | an outage loses that run's advice | high; flips if outages hide a broken key |
| Size limits (3.1) | **pack by encoded size, no re-split; too large → unassessed (chosen)** | nothing sent is cut; bodies depend only on input, so replay records them exactly | a request the estimate under-sizes is lost | high; flips if O6 shows `max_tokens_exceeded` often |
| | split again on rejection | recovers those requests | bodies depend on provider answers; replay must record the split path | — |
| Replay (3.4) | **records inside the fixture request, keyed by body hash (chosen)** | self-contained; core's `--replay` unchanged; any input change misses loudly | fixtures grow by the records | high |
| Client (3.3) | **one package `extensions/internal/jev` (chosen)** | one error, packing, record and cutoff contract | a shared change touches three binaries | high |
| Router output | **≤ 3 pointer lines (chosen)** | small; best-effort at compaction | one `Read` per reference; observe's `reference-unread` covers only hiden references over the window and cannot show a pointer was followed | high; flips if pointers are seen ignored |
| | inline the top reference | no extra call | references reach 10 KB, over the 4,500-unit entry block | high |
| Semantic caller | **explicit call beside `stack-public check`, merge-base range (chosen)** | the PR's own comparison and texts | a dotfiles change | high |
| | `[merge].message_checks` | automatic | a missing binary or a cut child refuses the merge; no diff | — |

## 5. Implementation order and PR split (item 7)

1. **feat(jev): shared client and `jev-semantic`** — `extensions/internal/jev`, `extensions/jev/semantic`,
   `jev-calibrate`, `fixtures/jev/semantic/`, generated cutoffs, `docs/architecture.md` §4 ("Active when
   `TYPESAFE_API_KEY` is set"; `jev-semantic` advisory at the PR caller) and §6 item 4 (§9), README install line.
   The owner runs calibrate before merge (O1). Then dotfiles: the `commit-pr` call of §3.2.
2. **feat(jev): `jev-router`** — its design round (§3.2 list), package, `jev.toml` in architecture §5, fixtures.
3. **feat(jev): `jev-classify`** — after #56 closes; its design round, package, fixtures. Then dotfiles: the job
   call of §3.2 and the key in the job's environment. (`[stages]` defaults already name the three.)

## 6. Acceptance criteria

`replay-test` (C12): built binaries, temp home, `stack run <stage> --ext <name> --replay <fixture>`.

### 6.1 Purpose layer

| ID | EARS criterion | Source | Scenario | Observable result / failure | Check |
|---|---|---|---|---|---|
| P1 | While no `TYPESAFE_API_KEY` is set, each `jev-*` shall answer `pass` with no context and make no call. | #57 opt-out | live request, key unset | `pass`, `context` `""`; failure: warn or a ledger line | `env -u TYPESAFE_API_KEY stack run outbound --ext jev-semantic --range <r>`, `stack run entry --ext jev-router`, `stack run cycle --ext jev-classify` |
| P2 | When a PR's merge-base range weakens a test or check without saying so, `jev-semantic` shall answer `warn` with a `jev-meta-duty` finding naming the files; a test deleted only on the base shall not appear. | #57 | `fixed` hit and diverged-base fixtures | the `want` finding; failure: pass or other kinds | `replay-test`; key: O2 |
| P3 | When the PR text claims what the diff does not show and the PR fits one request, `jev-semantic` shall answer `warn` with `jev-unsupported-claim`; when it does not fit, it shall report the question unassessed. | #57; owner decision | "add tests" without tests; a large PR | finding or `jev-unassessed` | `replay-test`; key: O2 |
| P4 | `jev-semantic` shall never make `stack run outbound` exit 1, and `public`'s refusal shall still exit 1. | §1 | live run, both extensions | exit 0 with a semantic `warn`; exit 1 with `public: block` | C15 |
| P5 | When a kit is open in a session and a reference scores at or above the cutoff, the entry context shall list ≤ 3 uncut paths within 1,000 UTF-16 units. | #57 | synthetic kit about a review | the `want` context | `replay-test`; key: O3 |
| P6 | When observe found owner replies, `jev-classify` shall classify each resolvable one, check qualified ones for duplicates, and list unresolved ones separately. (After #56.) | #57 | both locator forms + memory index | its file and context | `replay-test` incl. the artifact; key: O4 |
| P7 | Each extension shall send only the pinned model, with cutoffs from calibration on `fixtures/jev/`. | #57 | any call | `model` `jev-1.14.0` | C10, C11; O1 |
| P8 | When a fixture carries recorded answers, the extension shall answer from them with no network, and `warn` `jev-unrecorded` on a miss. | #57 | key unset; dummy key + failing endpoint | `want` met; deleted record → feature assertion fails | C12 |
| P9 | If Jev fails or answers malformed, the stage shall continue: unassessed on transient or too large, `warn` on persistent, within the budgets. | §3.1 | fake server | per §3.1 | C2, C4; key: O5 |
| P10 | Each live attempt shall add one ledger line; cost is observed, not asserted. | #57 | real runs | lines with known/unknown usage | C14; key: O6 |

### 6.2 Plan layer

| ID | EARS claim | Source | Test |
|---|---|---|---|
| C1 | While the key is empty and no `jev` object is present, `Main` shall write one valid `pass` after reading stdin to EOF, touch no file and open no connection. | P1 | `jev.TestOptOut` per binary |
| C2 | The client shall treat network errors, timeout, 429, 529, 5xx, 400 `max_tokens_exceeded` and 413 as unassessed without retry, and every other non-200, a body over 1 MiB, missing or null fields, model mismatch and non-finite or out-of-range probabilities as persistent, never echoing the body (a sentinel in it is absent from output). | P9 | `jev.TestClientErrors` (`httptest`, slow body) |
| C3 | Packing shall use the budget 48 KiB less the fixed fields, split an oversize unit into ordered parts that fit, keep questions ≤ 16 KiB, mark questions unassessed when the fixed fields leave no budget, and lose no unit. | §3.1 | `jev.TestPack` (25 KiB texts with a small diff; a single file over the budget; captured bodies reassemble every unit) |
| C4 | Each extension shall stop at its work deadline, including slow git or file reads, report the rest unassessed (classify: `complete: false`), answer within its reserve, and keep the response under 512 KiB with `jev-omitted`; core's synthetic `warn` under a shorter parent deadline is a separate outcome. | §3.1 | `<ext>.TestBudget` (fake clock, slow server, delayed preparation, many-batch result accepted by core, shortened core deadline) |
| C5 | Invalid stdin → `warn`; another stage, nothing to ask (empty range, no kits and prompt, no items) → `pass` with no request; results assemble in unit order under out-of-order completion. | §3.1 | `<ext>.TestNothingToAsk`, `jev.TestOrder` |
| C6 | `jev-semantic` shall build identical bodies under config variants (system, global, `GIT_CONFIG_PARAMETERS`, repository-local `diff.suppressBlankEmpty`, `diff.noprefix`, `diff.relative`, `core.abbrev`, `diff.orderFile`), ask `unsupported_claim` only when the whole PR fits, and answer `warn` before any request on a git failure. | P2, P3 | `semantic.TestState` (synthetic repository) |
| C7 | A probability just below, at and above the cutoff shall decide pass, hit, hit; a disabled decision never flags; never `block`. | P2–P4 | `semantic.TestDecision` |
| C8 | `jev-router` shall resolve, normalise and sort candidates, send slugs not dirs, take each candidate's maximum, rank by probability then path, never cut a path, report partial coverage with or without hits, and keep ≤ 1,000 UTF-16 units with astral characters; a broken `jev.toml` gives `warn` and the defaults. | P5 | `router.TestCandidates`, `router.TestContext`, `router.TestConfig` |
| C9 | `jev-classify` shall resolve both locator forms (a tool result between the assistant text and the reply is skipped), report unresolved items, use the fixture clock, send only qualified items to `dup_` with their reply and ID, and write its file atomically. | P6 | `classify.TestRun` (missing, stale, malformed observe file; paused write and failed rename leave the old file whole) |
| C10 | Cutoff selection shall meet the precision target with TP > 0 and recall ≥ 0.5, disable a decision under its fit or held-out floor, be unchanged when held-out samples change, and use the runtime decision function. | P7 | `jev.TestCutoff` |
| C11 | Every record and cutoff table shall name the pin; cutoffs refitted from `samples` shall equal the committed ones; changing the root dir or clock keeps every key, and changing state, a question or the pin changes it. | P7, §3.4 | `jev.TestFixturesPinned` |
| C12 | Every fixture shall, through `replay-test`, give exactly one self-answered result with empty `error`, its `expect` status, its exact `want` findings and context (classify: artifact), and no `jev-unrecorded` or `jev-error` unless wanted; once with the key unset and once with a dummy key and a test endpoint that fails the test on any request; a hit fixture with its records deleted shall fail the `want` assertion. | P2–P8 | `<ext>.TestFixtures` |
| C13 | No package under `cmd/` or `internal/` shall import `extensions/`, and the jev packages shall import no `internal/` package outside test helpers. | owner decision | existing `depguard` rules |
| C14 | Each live attempt shall append one serialised ledger line with run, outcome, elapsed time and known or `null` usage; an I/O error shall be one stderr line with the status unchanged; replay shall write none. | P10 | `jev.TestLedger` |
| C15 | A live `stack run outbound --ext public --ext jev-semantic --range <r>` with built binaries and a test endpoint answering a hit shall exit 0 with both named results (`public: pass`, semantic `warn`) on clean texts, and exit 1 with `public: block` and the semantic result on a texts hit. | P4 | `semantic.TestExitCodes` (non-replay) |

## 7. Test plan

CI never reaches the network: the client against `httptest` servers (C2–C5, C14), the binaries built with a test
endpoint (C12, C15), state building against synthetic repositories (`internal/gittest`, a test-only import). macOS
and Linux run them in `scripts/test`; Windows CI runs `go vet`, `go test -race` and the build
(`.github/workflows/ci.yml:40-55`). Real-key checks are owner steps:

**O1** each PR: `jev-calibrate`; committed cutoffs equal its output; unstable list read. **O2** the §3.2 caller on
a scratch PR deleting an assertion → warn, on a clean change → pass. **O3** `kit open demo` with a review state →
pointer block; `/compact` → restored when room is left. **O4** (after #56) `stack run cycle --ext jev-classify` after
a real observe run with T3 and Claude replies; spot-check outcomes. **O5** `TYPESAFE_API_KEY=invalid` with input at
each stage → `warn` 401; network off → unassessed. **O6** after a week: `usage.jsonl` totals, unknown usage,
`max_tokens_exceeded` count, coverage against runs.

## 8. Risks

- Synthetic fixtures are not the owner's real distribution, and Jev's quality on Japanese is unmeasured
  (https://docs.typesafe.ai/model-jaggedness.md); live precision may be lower and is not measured.
- The 48 KiB budget is an estimate; if it under-sizes, questions come back unassessed (O6 counts them).
- No deprecation policy for pinned IDs: a retired pin gives `warn` at every stage, the signal for the bump PR.
- The memory key encoding and the T3 schema are observed, not documented; a change shows as `unassessed`
  duplicates or `unresolved` items. The launchd job needs the key for classify; without it classify opts out.

## 9. Replacement text for `docs/architecture.md` §6 item 4

> 4. **Phase 4 (Jev extensions):** Add `jev-semantic` -> `jev-router` -> `jev-classify`, each with cutoffs
> calibrated on `fixtures/jev/` and key-less `--replay` verification; `jev-classify` follows the end of Phase 3
> (#56), since only it consumes observe's output. Design: `docs/design/phase4-jev.md`.

## 10. Owner questions

None. Every choice above follows from #57 and the owner decisions, or is a method choice stated with its reason.
