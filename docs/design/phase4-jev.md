# Phase 4 Design: Jev Extensions (`jev-semantic`, `jev-router`, `jev-classify`)

Design for issue #57: the optional Jev extensions of `docs/architecture.md` §4 "Optional Jev (System One)
Extensions", on the stage table of §4 and the protocol of §3, at `668ceb6`. Code is cited as `path:line`; Jev facts
are cited by URL and anything not confirmed in a primary page is marked **assumption** (checked by the owner with a
real key, §7 O-steps). Everything here lives under `extensions/`; core (`cmd/stack`, `internal/`) does not change.

## 1. Purpose and scope

Jev is TypeSafe's "System One" model: it answers typed questions about one `state` with calibrated probabilities and
writes no text. Three advisory extensions use it: `jev-semantic` flags two kinds of PR risk at `outbound`,
`jev-router` points the agent at the skill references it likely needs at `entry`, and `jev-classify` labels the
week's owner replies at `cycle`. None can block, none is required, and with no key each answers `{"status":"pass"}`.

**In scope:** the three extensions, a shared client package, calibration on `fixtures/jev/`, key-less `--replay`,
a usage ledger, and, in the implementation PRs, `docs/architecture.md` §4 (opt-out sentence, `jev-semantic`'s
caller) and §6 item 4 (§9), and the README install line. Two `shuymn/dotfiles` PRs wire the callers (§5).
**Out of scope:** any core change; any gate, redaction, cut or limit on what is sent (owner decision: having the key
is the opt-in, so provider limits only shape batches); spending caps (cost is observed, as improve's); Jev's `box`
questions, images and long-context mode; the Workers AI and OpenRouter routes; `[merge].message_checks` wiring;
changing improve's evidence (it takes only public evidence, `docs/design/phase3-cycle.md` §3.4 step 6, and
classify stays report-only).

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

**Assumptions:** a call takes 70–500 ms (#57; only the benchmark above is published); the 400 body of an
over-long request contains the string `max_tokens_exceeded` (the page names the error, not its field); Japanese text
is about one token per character and code about one per 3–4 bytes, so 48 KiB of encoded state is at most ≈ 16k
tokens for Japanese; 4 requests in flight stay under the account's rate limits.

## 3. Decisions

### 3.1 Opt-out and failure contract (item 1)

Every `jev-*` reads stdin to EOF first. Then:

| Condition | Answer | Network |
|---|---|---|
| Stdin is not one JSON object with a string `stage` | `warn`, finding `jev-error` "invalid request", context the same line | none |
| `stage` is not the extension's own | `pass` (as `extensions/public/stage.go:95`) | none |
| `TYPESAFE_API_KEY` unset or empty and no `jev` object (§3.4) | `pass`, no context, no file read or written | none |
| Nothing to ask (outbound: empty `range`, since both questions need the diff; entry: no kit and empty `prompt`; cycle: no `owner-reply` item) | `pass`, no context | none |
| A request fails transiently: connect or TLS error, timeout, 429, 529, any 5xx | its units are unassessed (below); one stderr line `jev: <n> requests skipped: <cause>` | no retry |
| A request fails persistently: any other non-200 (400 other than the batching split below, 401, 402, 403, 404, 413, 415, 422, …), a body not of the §2 shape, `model` ≠ the pin, a missing or wrongly typed answer, a probability not finite in [0, 1] | `warn`, finding `jev-error` (`detail`: status or fixed cause) and one context line naming the cause and the action (`jev-router: Jev answered 401: check TYPESAFE_API_KEY`) | — |

**Unassessed** work never changes the status: it is a finding `jev-unassessed` (`detail`: what, how much, why) and,
at outbound and cycle, a context line. A `warn` therefore means the owner must act (a wrong key, a changed API, a
retired pin) and does not cry wolf during an outage; an outage loses that push's or kit open's advice, which is
accepted. The key comes only from the environment (as jev-lint); no file holds it. Details are fixed strings and the
HTTP status, never the response body, which can quote input. The response read is capped at 1 MiB (larger →
persistent).

**Batching.** Provider limits shape batches and never drop input. One rule in `extensions/internal/jev`, used by all
three extensions, the cost estimator (§3.6) and the tests: inputs are units in a fixed order (files, kits, items);
units are packed greedily into requests whose encoded state is ≤ 48 KiB and whose encoded questions are ≤ 16 KiB
(more questions on the same state go to another request); a unit over 48 KiB is split at line boundaries (a line
over it at UTF-8 boundaries) into parts marked `part k of n`; a 400 `max_tokens_exceeded` splits that request in two
and sends both halves. Up to 4 requests are in flight. A unit whose requests did not all answer before the budget
ends, or failed transiently, is unassessed.

**Budgets.** Each extension's budget runs from process start and covers git, file reads, requests, splits and its
file writes: router 4 s, semantic 10 s, classify 5 min. Each request's timeout is the remaining budget less a
reserve (router 0.3 s, semantic 0.5 s, classify 20 s for its file and response). Core's deadlines are separate
(`internal/ext/ext.go:47-50`): entry 10 s at `kit open`, and at compaction the earlier of 10 s and the hook's 55 s
cut-off less the 2 s pipe allowance, run only when ≥ 1 s remains (`internal/session/session.go:255-289`); outbound
120 s shared after `public`, whose scan stops at 100 s (`extensions/public/stage.go:29-40`), so semantic usually
starts with ≥ 18 s left; cycle 30 min for classify's own call. When core cuts a child it records a synthetic `warn`
(`timed out: stage deadline …`), shown at entry as `[extension jev-router: warn: …]` when room is left; at
compaction the router's stderr is discarded.

### 3.2 What each extension sends and returns (item 2)

All requests use `model: "jev-1.14.0"`. Sent fields hold no wall-clock time and no local absolute path.

**`jev-semantic` (outbound).** Read from `repo` (the cwd when `""`) with git isolated: repository-location
variables dropped (as `extensions/internal/checkpub`), `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL` the null device,
and fixed options (`git log --format=%B --encoding=UTF-8 --no-show-signature <range>`; `git diff --text --no-ext-diff
--no-textconv --no-color --no-renames --src-prefix=a/ --dst-prefix=b/ -U3 --diff-algorithm=myers <range>`); output
is LF-normalised. Texts are `title`, `body`, `branch` and `commits`; diff units are the files in diff order. Two
`noul` questions:

- `meta_duty_regression`. Instructions: "Does `diff` remove or weaken a duty the project performs on itself,
  without `title`, `body` or `commits` stating and justifying that change?" Criteria true: "deletes or loosens a
  test assertion, skips or deletes a test, drops a step of the completion check (`scripts/test`, hooks, CI), removes
  or narrows a safety rule or deny, adds `--no-verify` or error suppression, or turns a must into a may in agent
  instructions (`AGENTS.md`, `CLAUDE.md`, skills), and the texts say nothing of it"; false: "no such change, or the
  texts state it and why". The duty list is `docs/architecture.md` §1 (hard boundaries), `core-stages.md` §0.4
  ("nothing … loses behaviour between PRs") and the rule "do not weaken assertions, skip tests or suppress errors".
  Examples: a diff deleting an equality assertion titled "refactor: simplify tests" (true); the same diff with body
  "drop the duplicate assertion; `TestX` covers it" (false); a diff removing `golangci-lint` from `scripts/test`
  titled "ci: speed up" (true). This judgment is local to a part of the diff, so every request carries the texts and
  a packed set of diff units; a hit names that request's files.
- `unsupported_claim`. Instructions: "Do `title`, `body` or `commits` claim something `diff` does not support?"
  Criteria true: "claims tests, docs or files the diff does not touch, an issue fixed by code that is not changed,
  verification or a measured number with no evidence in the texts or the diff, or 'no behaviour change' where the
  diff changes behaviour"; false: "every claim is visible in the diff or carries its evidence". Examples: "add tests
  for X" with no `_test.go` change (true); "verified on Windows" with no CI or log reference (true); "rename Foo to
  Bar" with exactly that rename (false). This is a PR-wide absence question, so **it is asked only when the texts and
  the whole diff fit one request** (≤ 48 KiB and no `max_tokens_exceeded`); otherwise it is unassessed
  (`unsupported_claim not assessed: the diff (12 files, 112 KiB) exceeds one question`), never judged on a part.

When texts alone exceed 24 KiB (half a request), both questions are unassessed. Answer: each `p ≥ cutoff` adds a
finding `{"kind":"jev-meta-duty"|"jev-unsupported-claim","where":"<files or \"message\">","detail":"p=0.83 ≥ 0.71
(jev-1.14.0)"}` and a context line; any hit → `warn`, else `pass`. Never `block`: the answer is probabilistic and
coarse, which is not a hard boundary (`docs/architecture.md` §1).

**Supported caller.** The hiden `commit-pr` step in `shuymn/dotfiles`
(`home/dot_claude/skills/hiden/references/commit-pr.md`, step 2) already writes the title and body to files and runs `stack-public check <PR base>..<head> --title-file <t>
--body-file <b> --branch <branch>` before each push and PR create or edit. A dotfiles PR adds, right after it passes,
`stack run outbound --ext jev-semantic --range <PR base>..<head> --title-file <t> --body-file <b> --branch <branch>`,
whose findings the agent reports and whose exit code does not stop the push. `stack run outbound` already accepts an
explicit `--range` (git must resolve it) and the three text flags (`docs/architecture.md` §3, `internal/cli/run.go`);
no core change is needed. A bare `stack run outbound` with `@{upstream}..HEAD` is not the PR comparison and is not
the supported path. CI is not wired (it would need the key as a repository secret).

**`jev-router` (entry).** Candidates are the files matching `references` globs from `jev.toml`, default
`skills/*/references/*.md`, under the Claude configuration directory (`$CLAUDE_CONFIG_DIR`, else `.claude` joined
to `os.UserHomeDir()`). They are slash-normalised relative to that directory, de-duplicated and sorted; questions
`ref_<n>` are numbered in that order. Each candidate's summary is its first heading plus first paragraph (LF,
≤ 400 bytes at a UTF-8 boundary; it describes the candidate, it is not evidence). State units: the `prompt` when
non-empty, and per kit of the request's `kits` its `slug` with `NORTH.md` and `state.md` (`internal/kit/render.go:35`)
read whole, LF-normalised; the kit `dir` is used to read, never sent. One `noul` per candidate: "Will the agent
working on this state need the reference `<skill>/references/<file>` for its next step? It covers: <summary>." A
candidate's probability is its maximum over the requests. Kits reach the router only with a valid `STACK_SESSION_ID`
(`internal/cli/run.go:260-264`); `kit open` without one sends `kits: []` and the router asks nothing.

Answer: candidates at or above the cutoff, by probability, ties by path, at most 3, as context:

```text
jev-router (jev-1.14.0): references likely needed now; read each before the step it covers.
- <Claude config dir>/skills/hiden/references/review.md (0.91): <first heading>
```

Paths are the absolute local paths of the files read (local context, never published). Sizes are in UTF-16 units
as core counts them (`internal/session/budget.go:14-16`): each line ≤ 300, the context ≤ 1,000, with `(n of m
inputs not scored: deadline)` appended when coverage was partial. At `kit open` it prints whole; at compaction
placement is best-effort: core places it, cuts it with a marker, or omits it within the 4,500-unit block and the
9,500-unit total (`internal/session/session.go:292-309`). No candidate passes → `pass`, no context.

**`jev-classify` (cycle).** Input: the observe file (`.local/state/stack/observe/latest.json` under the home
directory, `extensions/observe/observe.go:601`), fresh when its `window.until` is within 24 h of classify's clock
(missing → `pass` "no observation"; stale → `pass` "stale observation"; unparsable or unknown `version` → `warn`).
Its `owner-reply` items are observe's own rule (short owner turns after an assistant turn, `extensions/observe/
transcripts.go:369-375`, `extensions/observe/sources.go:93-104`), with two locator forms, resolved grouped by source
so each transcript or thread is read once:

- `path:line` (Claude transcripts), split on the last `:`: the user record at that line gives the reply, and the
  text blocks of the assistant records since the previous user record give the assistant turn, read whole.
- `t3:<thread>:<message>`: one `sqlite3 -readonly -json` query per thread on the T3 database observe reads gives the
  message and the assistant message before it.

An item whose file, line, row or preceding assistant turn is gone is `unresolved` (reported separately with its
reason, never classified). Units are items `{id, assistant, reply}` in source order. Questions: `kind_<id>`
`choice` with criteria `correction` ("says the agent's last action or claim was wrong and what to do instead"),
`drift` ("brings the agent back to the requested scope or goal it left"), `approval`, `other`.

**Duplicates.** For each item labelled `correction` or `drift`, the memory index of its project: Claude Code keeps
auto memory in `projects/<project>/memory/` under the Claude configuration directory, `<project>` derived from the
Git repository so all worktrees share it, `MEMORY.md` its index, or `autoMemoryDirectory` when set
(https://code.claude.com/docs/en/memory "Storage location"). Classify resolves the repository from the record's
`cwd` (`git rev-parse --path-format=absolute --git-common-dir`, its parent) and encodes it as Claude Code names
project directories (every character outside `[A-Za-z0-9]` → `-`; **assumption**, observed), honouring
`autoMemoryDirectory` in the user settings. State: `{memory_index, replies: [{id, reply}]}`, units the index (split
when large) and the replies; question `dup_<id>` `noul`: "Does a line of `memory_index` already state the rule that
reply `<id>` in `replies` corrects?"; probability = maximum over parts. Outcome: `duplicate` (at or above the
cutoff), `new` (every part answered, below it), `unassessed` (no index file, cwd or repository gone, a T3 item whose
thread has no resolvable cwd, transient failure or deadline). Only the index is compared, not the topic files it
links.

Output: `.local/state/stack/jev-classify/latest.json` (0600, temp + rename), written by every run that read a fresh
observe file: `{version, run: {id, started, finished}, window (observe's), complete, model, items: [{id, where,
status: classified|unresolved|unevaluated, label, probabilities, duplicate, dup_p, reason}], counts, usage}`. A run
cut by its budget writes `complete: false` with the rest `unevaluated`; a run that writes nothing leaves the earlier
file, whose `window` and `run.finished` date it. Context: counts per label and outcome, up to 10 `new` corrections
(`where`, ≤ 160-char excerpt), and the unresolved and unevaluated counts.

**Cycle wiring (dotfiles).** The supported scheduled path is the job's dedicated calls: after improve, the job runs
`stack run cycle --ext jev-classify` (classify never takes improve's time; `[stages].cycle` is unused by the job,
`docs/design/phase3-cycle.md` §4.8). The observe and improve results stay authoritative: binary missing → listed
under `skipped` in `last.json`; opted out or self-answered `pass`/`warn` → its result is merged; exit ≠ 0, invalid
output or a synthetic `error` → a result `{name: "jev-classify", status: "warn", error: "<cause>"}` is merged and
the job still exits 0. A bare `stack run cycle` with the default order runs classify before improve inside one
30-minute deadline; that path is not the scheduled one.

### 3.3 Layout and shared client (item 5)

```text
extensions/internal/jev/          client, batching, record lookup, usage ledger, jev.toml, cutoff selection
extensions/jev/semantic/          package + cmd/stack-jev-semantic/   (name in [stages]: jev-semantic)
extensions/jev/router/            package + cmd/stack-jev-router/
extensions/jev/classify/          package + cmd/stack-jev-classify/
extensions/jev/cmd/jev-calibrate/ owner-run, needs the key; not a stage binary
fixtures/jev/<extension>/<case>.json
```

One shared package holds transport, validation, batching, replay, metering and cutoff math, so the three cannot
drift apart; state building, router configuration and classify's file stay with their extension. `net/http` only,
the default transport (so `HTTPS_PROXY` is honoured), each request bound by a context deadline; no SDK. The base
URL is a field for tests, not configuration. Each request body is marshalled once; those bytes are hashed (§3.4) and
sent. `jev.toml` (`.config/stack/jev.toml` under the home directory, optional) has one key, `references`; a broken
file → `warn` and the defaults. **Ledger:** each live attempt appends `{run, time, extension, model, outcome,
elapsed_ms, input_tokens}` (`input_tokens` `null` when the request ended without `usage`, e.g. a timeout after
sending) to `.local/state/stack/jev/usage.jsonl`; `run` is a random ID per process. Replay never writes it.

### 3.4 `--replay` without a key (item 4)

A fixture is a `--replay` file whose request carries a `jev` object, which live requests never have: core sends the
request as is (`internal/ext/request.go:108-119`, `internal/cli/run.go:145-157`) and extensions ignore unknown
fields. Core gives no other replay marker, so the offline guarantee holds for a fixture with a valid `jev` object.

```json
{"request": {"stage": "outbound", "repo": "", "range": "jev-base..jev-head", "prompt": "", "title": "…",
             "body": "…", "branch": "", "jev": {"answers": {"sha256:<hex>": [{"model": "jev-1.14.0",
             "answers": {…}, "usage": {…}}, {…}, {…}]}}},
 "expect": {"status": "warn"},
 "want": {"findings": [{"kind": "jev-meta-duty", "where": "…", "detail": "…"}], "context": "…"},
 "repo": {"base": {"scripts/test": "…"}, "commits": [{"message": "…", "files": {"scripts/test": "…"}}]},
 "label": {"meta_duty_regression": true, "unsupported_claim": false, "split": "fit", "why": "…"}}
```

The key is the SHA-256 of the marshalled body; each record holds the calibration runs, and replay answers from the
first. With `jev` present the extension never calls the network, key or not: a miss → `warn`, finding
`jev-unrecorded` with the key. Machine inputs come from the fixture: `repo` is a synthetic repository the test
builds (fixed identity, dates and refs `jev-base`/`jev-head`) and the cwd for semantic; `jev.references` (router
globs relative to a fixture directory of reference stubs), `kits` dirs relative to the cwd; `jev.observe` (classify's
input file) and `jev.now` (classify's clock, so freshness does not expire). `want`, `repo` and `label` are read by
the tests; core reads only `request` and `expect`.

### 3.5 Pinned model and cutoffs (item 3)

Pin `jev-1.14.0` in `extensions/internal/jev`; `jev-latest` is never sent and no config overrides the pin. Fixtures
are public, so every case is synthetic: a semantic case is a small repository plus texts, a router case a kit and
reference stubs, a classify case transcript and memory excerpts written for it, Japanese and English. Labels are
written by hand with a one-line `why`; the owner reviews them in the PR. Calibration units and cutoffs:

| Decision | Unit | Score | Cutoffs |
|---|---|---|---|
| semantic meta-duty | one request (texts + one packed diff set) | `noul` | 1 |
| semantic unsupported claim | one whole-PR request | `noul` | 1 |
| router relevance | one (state, candidate) pair | `noul` | 1 shared by all candidates |
| classify label | one item | `probabilities["correction"]`, `probabilities["drift"]` | 1 each; an item is the flagged label with the higher probability, else the model's `choice` |
| classify duplicate | one (reply, index) pair | `noul` | 1 |

`jev-calibrate` (owner, with the key) builds each case's request with the extension's own builder, asks 3 times and
records all three answers. Fitting uses the `fit` cases and every single-call answer as one sample (live decides on
one call): the cutoff is the lowest of 0.50–0.99 (step 0.01) with precision ≥ the target (semantic 0.8, classify
0.8, router 0.7), true positives > 0 and recall ≥ 0.5; samples within ±0.05 of it are listed as unstable. Fewer
than 6 positive or 6 negative fit cases, or no cutoff meeting all three → the decision ships disabled and its
context says so. A fixture's `expect` and `want` are what the extension decides on the first recorded answer;
`label` is ground truth. **Model bump:** one PR changes the pin, re-records every fixture, refits, and reports old
vs new on the `heldout` cases (precision, recall, support, router top-3 hits); the new pin must meet the targets
there. A public synthetic corpus measures regressions on that corpus; the ledger does not measure live precision.

### 3.6 Cost and latency estimates (item 6)

From the published price, the §2 token assumptions and §3.1 batching. Estimates, not measurements:

| Extension | Typical run | Requests | Cost per run | Budget (own / core) |
|---|---|---|---|---|
| semantic | 20 KiB PR, one request | ⌈packed bytes / 48 KiB⌉, +1 per split | ≈ $0.0003; a 112 KiB PR ≈ 3 requests ≈ $0.0012 | 10 s / 120 s shared |
| router | one kit, 31 candidates | per state unit × ⌈questions / 16 KiB⌉ ≈ 1–2 | ≈ $0.0005 | 4 s / 10 s (shorter at compaction) |
| classify | 150 items, ≈ 2 KiB each | ⌈300 KiB / 48 KiB⌉ + memory ≈ 10 | ≈ $0.008 | 5 min / 30 min |

About 100 pushes, 300 kit opens and 30 cycles a month ≈ $0.4; nothing caps it, and O6 reports what the ledger saw.
Latency per call is the 70–500 ms assumption; 4 in flight keep a typical router run under 1 s.

## 4. Alternatives

| Choice | Option | Gains | Gives up | Reversibility / when it flips |
|---|---|---|---|---|
| Failure (3.1) | **transient → unassessed, persistent → warn (chosen)** | warn only when the owner must act | an outage loses that run's advice | high; flips if outages hide a broken key |
| | always warn | every failure visible | a marker per kit open during an outage | high |
| Size limits (3.1) | **pack and split, report unassessed (chosen)** | nothing sent is cut; absence is never judged on part of the evidence | more requests on large inputs | high |
| | cut to a cap | one request | silently misses evidence (owner decision rejects it) | — |
| Replay (3.4) | **answers inside the fixture request, keyed by body hash (chosen)** | self-contained; core's `--replay` unchanged; any input change misses loudly | fixtures grow by the records | high |
| | env file of answers | live and replay share inputs | a second file to keep in step | high |
| Client (3.3) | **one package `extensions/internal/jev` (chosen)** | one error, batching, record and cutoff contract | a shared change touches three binaries | high |
| | per-extension client | independent | three copies | high |
| Router output | **≤ 3 pointer lines (chosen)** | fits every budget | one `Read` per reference; observe's `reference-unread` covers only hiden references over the window and cannot show a pointer was followed | high; flips if pointers are seen ignored |
| | inline the top reference | no extra call | references reach 10 KB, over the 4,500-unit entry block | high |
| Semantic caller | **explicit call beside `stack-public check` (chosen)** | the real PR comparison and texts | a dotfiles change | high |
| | `[merge].message_checks` | automatic | a missing binary or a cut child refuses the merge; no diff | — |

## 5. Implementation order and PR split (item 7)

1. **feat(jev): shared client and `jev-semantic`** — `extensions/internal/jev`, `extensions/jev/semantic`,
   `jev-calibrate`, `fixtures/jev/semantic/`, generated cutoffs, `docs/architecture.md` §4 ("Active when
   `TYPESAFE_API_KEY` is set"; `jev-semantic` advisory at the PR caller) and §6 item 4 (§9), README install line.
   The owner runs calibrate before merge (O1). Then dotfiles: the `commit-pr` call of §3.2.
2. **feat(jev): `jev-router`** — package, fixtures, cutoffs.
3. **feat(jev): `jev-classify`** — after #56 closes; package, fixtures, cutoffs. Then dotfiles: the third job call
   of §3.2 and the key in the job's environment.

`[stages]` defaults already name the three (`docs/architecture.md` §5); a binary not installed is skipped.

## 6. Acceptance criteria

`replay-test` below means the fixture test of C12: built `stack` and `stack-jev-*`, temp home, `PATH` of those
binaries and `git`, `stack run <stage> --ext <name> --replay <fixture>`.

### 6.1 Purpose layer

| ID | EARS criterion | Source | Scenario | Observable result / failure | Check |
|---|---|---|---|---|---|
| P1 | While no `TYPESAFE_API_KEY` is set, each `jev-*` shall answer `pass` with no context and make no call. | #57 opt-out | live request, key unset | `pass`, `context` `""`; failure: warn or a ledger line | `env -u TYPESAFE_API_KEY stack run outbound --ext jev-semantic --range <r>` (and `entry`, `cycle --ext jev-classify`) |
| P2 | When a PR range weakens a test or check without saying so, `jev-semantic` shall answer `warn` with a `jev-meta-duty` finding naming the files. | #57 | synthetic repo deleting an assertion, titled "refactor" | the `want` finding; failure: pass or other kinds | `replay-test`; key: O2 |
| P3 | When the PR text claims what the diff does not show and the PR fits one question, `jev-semantic` shall answer `warn` with `jev-unsupported-claim`; when it does not fit, it shall report the question unassessed. | #57; owner decision | "add tests" without tests; a large PR | finding or `jev-unassessed` | `replay-test`; key: O2 |
| P4 | `jev-semantic` shall never make `stack run outbound` exit 1, and `public`'s refusal shall still exit 1. | §1 | both selected | exit codes | `replay-test` with `--ext public --ext jev-semantic` |
| P5 | When a kit is open in a session and a reference scores at or above the cutoff, the entry context shall list ≤ 3 readable paths within 1,000 UTF-16 units. | #57 | synthetic kit about a review | the `want` context; failure: none, > 3, unreadable path | `replay-test`; key: O3 (`kit open`, `/compact`) |
| P6 | When observe found owner replies, `jev-classify` shall label each resolvable one, mark corrections `duplicate`/`new`/`unassessed`, and list unresolved ones separately. (After #56.) | #57 | both locator forms + memory index | its file and context | `replay-test` incl. the artifact; key: O4 |
| P7 | Each extension shall send only the pinned model, with cutoffs from calibration on `fixtures/jev/`. | #57 | any call | `model` `jev-1.14.0` | C10, C11; O1 output equals the committed cutoffs |
| P8 | When a fixture carries recorded answers, the extension shall answer from them with no network, and `warn` `jev-unrecorded` on a miss. | #57 | key unset; dummy key + failing proxy | `want` met; deleted record → feature assertion fails | `replay-test` (C12) |
| P9 | If Jev fails or answers malformed, the stage shall continue: unassessed on transient, `warn` on persistent, within the budgets. | §3.1 | fake server | per §3.1 | C2, C4; key: O5 |
| P10 | Each live attempt shall add one ledger line; cost is observed, not asserted. | #57 | real runs | lines with known/unknown usage | C14; key: O6 |

### 6.2 Plan layer

| ID | EARS claim | Source | Test |
|---|---|---|---|
| C1 | While the key is empty and no `jev` object is present, `Main` shall write one valid `pass` after reading stdin to EOF, touch no file and open no connection. | P1 | `jev.TestOptOut` per binary |
| C2 | The client shall treat network errors, timeout, 429, 529 and 5xx as transient and every other non-200, a body over 1 MiB, missing or null fields, model mismatch and non-finite or out-of-range probabilities as persistent, never echoing the body (a sentinel in it is absent from output). | P9 | `jev.TestClientErrors` (`httptest`, slow body) |
| C3 | Packing shall keep each state ≤ 48 KiB and questions ≤ 16 KiB, split an oversize unit into ordered parts, and split a request in two on `max_tokens_exceeded`, losing no unit. | §3.1 | `jev.TestPack` (captured bodies reassemble every unit) |
| C4 | Each extension shall stop at its budget from process start, including slow git or file reads, report the rest unassessed (classify: `complete: false`), and answer before core's deadline. | §3.1 | `<ext>.TestBudget` (fake clock, slow server, delayed preparation, shortened core deadline) |
| C5 | Invalid stdin → `warn`; another stage, nothing to ask (empty range, no kits and prompt, no items) → `pass` with no request. | §3.1 | `<ext>.TestNothingToAsk` |
| C6 | `jev-semantic` shall build texts and diff units with isolated git, ask `meta_duty_regression` per packed set, ask `unsupported_claim` only when the whole PR fits one request, and report it unassessed otherwise. | P2, P3 | `semantic.TestState` (synthetic repo with hostile user config: `diff.noprefix`, `diff.algorithm`) |
| C7 | A probability just below, at and above the cutoff shall decide pass, hit, hit; a disabled decision never flags; never `block`. | P2–P4 | `semantic.TestDecision` |
| C8 | `jev-router` shall resolve, normalise and sort candidates, send slugs not dirs, take each candidate's maximum, rank by probability then path, and keep ≤ 3 lines and ≤ 1,000 UTF-16 units with astral characters. | P5 | `router.TestCandidates`, `router.TestContext` |
| C9 | `jev-classify` shall resolve both locator forms per source, report unresolved items (deleted transcript, row), use a fixture clock for freshness, ask `dup_` only for flagged items with their reply and ID, and write its file atomically. | P6 | `classify.TestRun` (missing, stale, malformed observe file; paused write and failed rename leave the old file whole) |
| C10 | Cutoff selection shall meet the precision target with TP > 0 and recall ≥ 0.5, disable under 6 + 6 cases, and differ from the next step on a crafted corpus. | P7 | `jev.TestCutoff` |
| C11 | Every record and cutoff table shall name the pin; cutoffs refitted from the records shall equal the committed ones; changing the root dir or clock keeps every key, and changing state, a question or the pin changes it. | P7, §3.4 | `jev.TestFixturesPinned` |
| C12 | Every fixture shall, through `replay-test`, give exactly one self-answered result with empty `error`, its `expect` status, its exact `want` findings and context (classify: artifact), and no `jev-unrecorded` or `jev-error` unless wanted; once with the key unset and once with a dummy key and an `HTTPS_PROXY` that fails the test on any connection; a hit fixture with its records deleted shall fail the `want` assertion. | P2–P8 | `<ext>.TestFixtures` |
| C13 | A broken `jev.toml` shall give `warn` and the default globs. | §3.3 | `router.TestConfig` |
| C14 | Each live attempt shall append one ledger line with run, outcome, elapsed time and known or `null` usage; replay shall write none. | P10 | `jev.TestLedger` |
| C15 | No package under `cmd/` or `internal/` shall import `extensions/`, and the jev packages shall import no `internal/` package outside test helpers. | owner decision | existing `depguard` rules |

## 7. Test plan

CI never reaches the network: the client against `httptest` servers (C2–C4, C14), the extensions through
`replay-test` with built binaries (C12), state building against synthetic repositories (`internal/gittest`, a
test-only import). macOS and Linux run them in `scripts/test`; Windows CI runs `go vet`, `go test -race` and the
build (`.github/workflows/ci.yml:40-55`). Real-key checks are owner steps:

- **O1** each PR: `jev-calibrate` on its fixtures; committed cutoffs equal the output; unstable list read.
- **O2** semantic: the §3.2 caller on a scratch PR that deletes an assertion → warn; on a clean change → pass.
- **O3** router: `kit open demo` in a session with a state about a review → pointer block; `/compact` → restored
  when room is left.
- **O4** classify (after #56): `stack run cycle --ext jev-classify` after a real observe run with T3 and Claude
  replies; spot-check labels and `duplicate`.
- **O5** `TYPESAFE_API_KEY=invalid` with a non-empty input at each stage → `warn` `jev-error` 401; network off →
  unassessed and the stderr line.
- **O6** after a week: totals from `usage.jsonl`, attempts with unknown usage, coverage against runs.

## 8. Risks

- Synthetic fixtures are not the owner's real distribution; real precision may be lower and is not measured.
- Jev's quality on Japanese text is unmeasured (https://docs.typesafe.ai/model-jaggedness.md); fixtures include
  Japanese cases so calibration measures it on them.
- No deprecation policy for pinned IDs: a retired pin gives `warn` at every stage, the signal for the bump PR.
- The memory directory key encoding and the T3 schema are observed, not documented; a change makes duplicates
  `unassessed` or items `unresolved`, which the counts show.
- The launchd job needs the key in its environment for classify; without it classify opts out (P1).

## 9. Replacement text for `docs/architecture.md` §6 item 4

> 4. **Phase 4 (Jev extensions):** Add `jev-semantic` -> `jev-router` -> `jev-classify`, each with cutoffs
> calibrated on `fixtures/jev/` and key-less `--replay` verification; `jev-classify` follows the end of Phase 3
> (#56), since only it consumes observe's output. Design: `docs/design/phase4-jev.md`.

## 10. Owner questions

None. Every choice above follows from #57 and the owner decisions, or is a method choice stated with its reason.
