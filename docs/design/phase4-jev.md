# Phase 4 Design: Jev Extensions (`jev-semantic`, `jev-router`, `jev-classify`)

Design for issue #57: the optional Jev extensions of `docs/architecture.md` §4 "Optional Jev (System One)
Extensions", on the stage table of §4 and the protocol of §3, at `668ceb6`. Code is cited as `path:line`; Jev facts
are cited by URL and anything not confirmed in a primary page is marked **assumption** (checked by the owner with a
real key, §8 O-steps). Everything here lives under `extensions/`; core (`cmd/stack`, `internal/`) does not change.

## 1. Purpose and scope

Jev is TypeSafe's "System One" model: it answers typed questions about one `state` with calibrated probabilities and
writes no text. Three advisory extensions use it: `jev-semantic` flags two kinds of PR risk at `outbound`,
`jev-router` points the agent at the skill references it likely needs at `entry`, and `jev-classify` labels the
week's owner replies at `cycle`. None can block, none is required, and with no key each answers `{"status":"pass"}`.

**In scope:** the three extensions, a shared client package, calibration on `fixtures/jev/`, key-less `--replay`,
a usage ledger, and the replacement text of `docs/architecture.md` §6 item 4 (§9; the implementation PR edits
architecture.md). **Out of scope:** any core change; any gate, redaction or limit on what is sent (owner decision:
having the key is the opt-in); spending caps (cost is observed, as improve's); Jev's `box` questions and images;
the Workers AI and OpenRouter routes; feeding classify's output into improve's model (§10 Q1).

## 2. Backend facts

| Fact | Source |
|---|---|
| `POST https://api.typesafe.ai/v1/systemone`, `Authorization: Bearer <key>`, JSON body `model` (required), `state` (string, object or array), `questions` (name → question, ≥ 1), optional `assets` | https://docs.typesafe.ai/api/system-one.md |
| Question `type` `noul` (optional `criteria` `{"true": …, "false": …}`), `choice` (`criteria` label → description, 1–255), `score` (`criteria` array of 1–10 levels); `instructions` string or structure | same |
| Answers: `noul` → `{"type","noul": p}` (no confidence); `choice` → `choice`, `confidence`, `probabilities`; `score` → `score`, `confidence`, `legend`, `probabilities`; top-level `model` (the versioned ID that answered) and `usage` `{input_tokens, output_tokens}` | same |
| Pinned IDs `jev-1.14.0` (newest, from 2026-10-14) and `jev-1.13.0`; aliases `jev-latest` and `jev-preview` move with releases; "send that version's ID" when thresholds are tuned; no deprecation policy stated | https://docs.typesafe.ai/models.md |
| Context: 64k tokens per request (state + all questions), 32k per question (state + that question); no cap on the question count | same |
| $0.042 per 1M input tokens; output free | same; https://developers.cloudflare.com/ai/models/typesafe/jev/ |
| Errors: 400 (incl. `max_tokens_exceeded`), 401, 413 (> 32 MiB body), 415, 422 (`detail` array), 429, 529; back off on 429/529; no `Retry-After` documented | https://docs.typesafe.ai/api/errors.md |
| One state with N questions is billed once; batched 13 questions over a ~54k-char article: 0.24 s, $0.000497; run-to-run std dev ≤ 0.015 | https://docs.typesafe.ai/cookbooks/parallel_questions.md |
| Prior art: pinned `jev-1.14.0`, "at or above" cutoffs fitted per rule on labelled fixtures, verdicts cached by content and replayed with no key; pass-to-pass noise median 0.010, p90 0.050, max 0.300; "about one finding in five was wrong" | https://github.com/mizchi/jev-lint and its `docs/reference.md` |

**Assumptions** (not in a primary page): latency 70–500 ms per call (#57; the docs give only the 0.24 s example);
question names accept `[a-z0-9_]{1,64}`; an unknown question name or an unanswered question never appears in a 200;
the token count of Japanese text (references and kit state are Japanese) is about one token per character.

## 3. Decisions

### 3.1 Opt-out and failure contract (item 1)

Every `jev-*` reads stdin to EOF first. Then:

| Condition | Answer | Network |
|---|---|---|
| `TYPESAFE_API_KEY` unset or empty, and the request has no `jev` object (§3.4) | `pass`, no context, no findings, no file read or written | none |
| Nothing to judge (outbound: empty range and empty `title`/`body`; entry: no kit state and empty `prompt`; cycle: no `owner-reply` item) | `pass`, no context | none |
| Transient: connect or TLS error, own timeout, 429, 529, other 5xx | `pass`, no context; one stderr line `jev: skipped: <cause>`; ledger record | one call, no retry |
| Persistent: 401/403, 400 (after §3.2's one halving on `max_tokens_exceeded`), 413, 415, 422, a body that is not the §2 shape, `model` ≠ the pin, a missing or wrongly typed answer, a probability not finite in [0, 1] | `warn`, finding `{"kind":"jev-error","where":"<extension>","detail":"<status or fixed cause>"}`, no context | — |

The key comes only from the environment, never from a file (as jev-lint); `jev.toml` (§3.3) holds no secret.
Transient failures pass because the next push, kit open or day asks again and the session is then exactly as with
no key; a `warn` means the owner must act (a wrong key, a changed API, a pin the service no longer serves), so it
does not cry wolf. Error details are fixed strings and the HTTP status, never the response body, which can quote
input. Own timeouts sit inside each stage's deadline (`internal/ext/ext.go:47-50`): router 4 s of entry's 10 s,
semantic 10 s of outbound's 120 s, classify 5 min of cycle's 30 min with 30 s per call.

### 3.2 What each extension sends and returns (item 2)

All use `model: "jev-1.14.0"`, one request per state, every question of that state in it (billed once, §2).

**`jev-semantic` (outbound).** State, an object: `title`, `body`, `branch` from the request; `commits`, the
messages of `git log --format=%B <range>`; `diff`, `git diff --text --no-ext-diff --no-textconv --no-color
<range>` run in `repo` (as `extensions/internal/checkpub` reads diffs). The state is cut to 64 KiB (≈ 16k code
tokens, ≈ 22k Japanese): whole files are dropped from the end and named in `omitted_files`. On 400
`max_tokens_exceeded` it asks once more at half the cap. `range` `""` (as at `stack pr merge`) sends only the texts.
Two `noul` questions:

- `meta_duty_regression`. Instructions: "Does `diff` remove or weaken a duty the project performs on itself,
  without `title`, `body` or `commits` stating and justifying that change?" Criteria true: "deletes or loosens a
  test assertion, skips or deletes a test, drops a step of the completion check (`scripts/test`, hooks, CI), removes
  or narrows a safety rule or deny, adds `--no-verify` or error suppression, or turns a must into a may in agent
  instructions (`AGENTS.md`, `CLAUDE.md`, skills), and the texts say nothing of it"; false: "no such change, or the
  texts state it and why". The duty list is `docs/architecture.md` §1 (hard boundaries), `core-stages.md` §0.4
  ("nothing … loses behaviour between PRs") and the rule "do not weaken assertions, skip tests or suppress errors".
  Example inputs: a diff deleting `require.Equal(t, want, got)` titled "refactor: simplify tests" (true); the same
  diff with body "drop the duplicate assertion; `TestX` covers it" (false); a diff removing `golangci-lint` from
  `scripts/test` titled "ci: speed up" (true).
- `unsupported_claim`. Instructions: "Do `title`, `body` or `commits` claim something `diff` does not support?"
  Criteria true: "claims tests, docs or files the diff does not touch, an issue fixed by code that is not changed,
  verification or a measured number with no evidence in the texts or the diff, or 'no behaviour change' where the
  diff changes behaviour"; false: "every claim is visible in the diff or carries its evidence". Examples: "add tests
  for X" with no `_test.go` change (true); "verified on Windows" with no CI or log reference (true); "rename Foo to
  Bar" with exactly that rename (false).

Answer: each `p ≥ cutoff` adds a finding `{"kind":"jev-meta-duty"|"jev-unsupported-claim","where":"<range or
\"message\">","detail":"p=0.83 ≥ 0.71 (jev-1.14.0)"}` and a context line `jev-semantic: possible <kind> (p=…):
check the PR text against the diff`; any hit → `warn`, else `pass`. It never answers `block`: the answer has no
location and prior art saw one finding in five wrong, which is not a hard boundary (`docs/architecture.md` §1).

**`jev-router` (entry).** Candidates are the files matching `references` globs from `jev.toml`, default
`skills/*/references/*.md` under the Claude configuration directory (`$CLAUDE_CONFIG_DIR`, else `.claude` under
`os.UserHomeDir()`, so no home path is written anywhere); 31 files on the owner's machine today. Each candidate's
summary is its first heading plus its first paragraph, cut to 400 bytes on a UTF-8 boundary. State: `prompt` (when
non-empty) and, for each kit of the request's `kits`, `NORTH.md` and `state.md` (`internal/kit/render.go:35`), each
cut to its first 8 KiB. One `noul` per candidate, named `ref_<n>` with a table back to the path: "Will the agent
working on this state need the reference `<skill>/references/<file>` for its next step? It covers: <summary>."
Answer: candidates with `p ≥ cutoff`, highest first, at most 3, as context:

```text
jev-router (jev-1.14.0): references likely needed now; read each before the step it covers.
- <path> (0.91): <first heading>
```

Each line ≤ 300 chars, the context ≤ 1,000 chars, `pass` (none → `pass`, no context). The budget follows from the
compaction restore: an entry block is capped at 4,500 chars inside a 9,500-char total shared with the kits
(`internal/session/budget.go:14-16`, `internal/session/session.go:293-305`), so 1,000 chars always fits when the
restore leaves room, and `kit open` prints the same text. Request ≈ 4k state + 31 × 200 tokens ≈ 10k tokens.

**`jev-classify` (cycle, after #56).** Input: `owner-reply` items of the observe file (`.local/state/stack/observe/
latest.json` under the home directory, `extensions/observe/observe.go:601`), freshness as improve (24 h). Per item it
re-reads the transcript at `where` (`path:line`, `extensions/observe/transcripts.go:372`): the reply (≤ 300 chars,
observe's `shortReply`) and the tail of the last assistant text before it (≤ 1,500 chars). Items are grouped by
project directory into states of ≤ 40 items (`[{id, assistant, reply}]`). Questions: `kind_<id>` `choice`, criteria
`correction` ("says the agent's last action or claim was wrong and what to do instead"), `drift` ("brings the agent
back to the requested scope or goal it left"), `approval`, `other`. For each item labelled `correction` or `drift`
at or above its cutoff, a second request on the project's `memory/MEMORY.md` (≤ 8 KiB, beside the transcript's
project directory) asks `dup_<id>` `noul`: "Does a line of the memory index already state the rule this reply
corrects?" Output: `.local/state/stack/jev-classify/latest.json` (0600, temp + rename) `{version, window, model,
items: [{id, where, label, p, duplicate, dup_p}], usage}`; context: counts per label and up to 10 non-duplicate
corrections (`where` and ≤ 160-char excerpt); `pass`, or `warn` only per §3.1. It feeds improve nothing in Phase 4:
improve takes only public evidence (`docs/design/phase3-cycle.md` §3.4 step 6) and owner replies are owner text; the
owner reads the result in `last.json` and retros (§10 Q1). The dotfiles job runs it as a third call,
`stack run cycle --ext jev-classify`, after improve, so it never takes improve's time, and records an invalid
result as a `warn` in `last.json` instead of failing the run.

### 3.3 Layout and shared client (item 5)

```text
extensions/internal/jev/     client, record lookup, usage ledger, jev.toml, cutoff selection
extensions/jev/semantic/     package + cmd/stack-jev-semantic/   (name in [stages]: jev-semantic)
extensions/jev/router/       package + cmd/stack-jev-router/
extensions/jev/classify/     package + cmd/stack-jev-classify/
extensions/jev/cmd/jev-calibrate/   owner-run, needs the key; not a stage binary
fixtures/jev/<extension>/<case>.json
```

One shared package: one HTTP client, error classes, response validation, record format and cutoff rule, so the
three extensions cannot drift apart. `net/http` only, with the request bound by `context.WithTimeout` and the
response read through a 1 MiB limit; no SDK (the docs' SDKs are Python and JavaScript). The base URL is a field
for tests (`httptest`), not configuration. `jev.toml` (`.config/stack/jev.toml` under the home directory, optional)
has one key, `references` (router globs); a broken file → `warn` and the defaults. Each call appends
`{time, extension, model, input_tokens, outcome}` to `.local/state/stack/jev/usage.jsonl`: cost is observed there.

### 3.4 `--replay` without a key (item 4)

A fixture is a normal `--replay` file whose request carries a `jev` object, which live requests never have: core
sends the request as is (`internal/ext/request.go:108-119`, `internal/cli/run.go:145-157`) and extensions
ignore unknown fields.

```json
{"request": {"stage": "outbound", "repo": "", "range": "3f1c2ab^..3f1c2ab", "prompt": "", "title": "…",
             "body": "…", "branch": "", "jev": {"answers": {"sha256:<hex>": {"model": "jev-1.14.0",
             "answers": {…}, "usage": {…}}}}},
 "expect": {"status": "warn"},
 "label": {"meta_duty_regression": true, "unsupported_claim": false, "why": "…",
           "scores": {"meta_duty_regression": [0.83, 0.85, 0.82], "unsupported_claim": [0.10, 0.12, 0.09]}}}
```

The key is the SHA-256 of the exact JSON body the client would send (Go's `encoding/json`, fixed field order), so
any change to state building, questions or the pin misses. With `jev` present the extension never calls the network,
key or not: a hit is answered from the record, a miss → `warn`, finding `jev-unrecorded` with the key, so a replay
`expect` of `pass` fails visibly. Inputs the live path reads from the machine come from the fixture: `jev.references`
(router globs, relative to the cwd), `jev.observe` (classify's latest.json path); kit dirs in `kits` and semantic
ranges are relative to the repository root, so fixtures run from a clone of this repository (semantic ranges name
its public commits). `label` and `scores` are ignored by core (`ParseReplay` reads `request` and `expect`).

### 3.5 Pinned model and cutoffs (item 3)

Pin `jev-1.14.0` in `extensions/internal/jev`; `jev-latest` is never sent and no config overrides the pin, since the
cutoffs belong to it. Fixtures are public, so cases are public commits of this repository (semantic) or synthetic,
anonymised texts written for the fixture (router kit states and reference stubs, classify replies). Labels are
written by hand in `label` with a one-line `why`, by the implementer, and the owner reviews them in the PR. The
owner runs `go run ./extensions/jev/cmd/jev-calibrate fixtures/jev/<extension>` with the key: it builds each case's
request with the extension's own state builder, asks 3 times, writes the first answer to `jev.answers`, the three
probabilities to `label.scores`, and the generated `cutoffs.go` of that extension. Rule per question: on the mean of
the three passes, the lowest cutoff in 0.50–0.99 (step 0.01) whose precision is at least the target (semantic 0.8,
classify 0.8, router 0.7; a router miss only costs a skipped pointer); recall is printed; cases within ±0.05 of the
cutoff (prior-art p90 noise) are listed as unstable; fewer than 6 positive or 6 negative cases, or no cutoff meeting
the target → the question ships disabled (never flags). A model bump is one PR: change the pin, rerun calibrate on
every fixture, commit the new answers, scores and cutoffs together; a test fails when any record's `model`, or the
cutoff table's model, differs from the pin, and recomputes every cutoff from `label.scores` with no key.

### 3.6 Cost and latency (item 6)

| Extension | Input tokens per run (cap) | Cost per run at $0.042/M | Calls | Own timeout / stage deadline |
|---|---|---|---|---|
| semantic | ≤ 22k, one halved retry ≤ 11k | ≤ $0.0014; typical 5k → $0.0002 | 1 (2 on overflow) | 10 s / 120 s, after `public` (scan stops at 100 s, `extensions/public/stage.go:31-40`) |
| router | ≈ 10k | ≈ $0.0004 | 1 | 4 s / 10 s; at compaction core's remaining time (≥ 1 s) can be shorter |
| classify | ≈ 150 items × 600 tokens + memory checks ≈ 120k | ≈ $0.005 per daily run | ⌈items/40⌉ + projects | 30 s per call, 5 min total / 30 min |

With ~100 pushes, ~300 kit opens and 30 cycles a month the total is under $0.30 a month; nothing caps it, and the
ledger records it. Latency: a typical call is under 1 s (0.24 s for 13 questions over 54k chars, §2).

## 4. Alternatives

| Choice | Option | Gains | Gives up | Reversibility / when it flips |
|---|---|---|---|---|
| Failure (3.1) | **transient pass, persistent warn (chosen)** | warn only when the owner must act | a down service is silent except stderr and the ledger | high; flips if outages hide a broken key for long |
| | always warn | every failure visible | a marker per kit open during an outage (wolf-crying) | high |
| Replay (3.4) | **answers inside the fixture request, keyed by body hash (chosen)** | one self-contained file; uses core's `--replay` unchanged; any input change misses loudly | fixtures grow by the recorded answers (KBs) | high |
| | env `STACK_JEV_ANSWERS=<file>` | live and replay share inputs | a second file to keep in step; an env path in every check | high |
| | cache by state only (jev-lint style) | survives question rewording | rewording silently reuses stale answers | high |
| Client (3.3) | **one package `extensions/internal/jev` (chosen)** | one error, record and cutoff contract | a shared change touches three binaries | high |
| | per-extension client | independent | three copies of validation and replay | high |
| Router output | **≤ 3 pointer lines (chosen)** | fits every budget; the agent reads with `Read`, which observe's `reference-unread` counts | one more tool call per reference | high; flips if pointers are seen ignored |
| | inline the top reference | no extra call | references reach 10 KB, over the 4,500-char entry block | high |
| Semantic | **warn, never block (chosen)** | no false-positive refusals | a real regression can be pushed | high; owner can list it in `[merge].message_checks`, where a warn still merges |
| Classify place | **third `--ext` call after improve (chosen)** | improve's time untouched | its result is one day later for any future consumer | high |
| | inside observe's call | one call | shares observe's 5-min plan | high |

## 5. Implementation order and PR split (item 7)

1. **feat(jev): shared client and `jev-semantic`** — `extensions/internal/jev`, `extensions/jev/semantic`,
   `jev-calibrate`, `fixtures/jev/semantic/` (≥ 6 + 6 cases per question from this repository's history), generated
   cutoffs, `docs/architecture.md` §6 item 4 (§9) and §4 "Opt-out" ("Active when `TYPESAFE_API_KEY` is set").
   The owner runs calibrate before merge (O1).
2. **feat(jev): `jev-router`** — package, fixtures (synthetic kits and reference stubs), cutoffs.
3. **feat(jev): `jev-classify`** — after #56 closes; package, fixtures, cutoffs; dotfiles PR adding the third call
   and the key to the job's environment.

Each PR leaves `[stages]` defaults untouched (they already name the three, `docs/architecture.md` §5); a binary not
installed is skipped, so merging order and installing are independent.

## 6. Acceptance criteria

### 6.1 Purpose layer

| ID | EARS criterion | Source | Scenario | Observable result / failure | Check |
|---|---|---|---|---|---|
| P1 | While no `TYPESAFE_API_KEY` is set, each `jev-*` shall answer `pass` with no context and make no call. | #57 opt-out; §4 | live request, key unset | `"status":"pass"`, `context` `""`; failure: warn or a ledger line | `env -u TYPESAFE_API_KEY stack run outbound --ext jev-semantic` (and `entry`, `cycle`) → exit 0, pass |
| P2 | When a range weakens a test or check without saying so, `jev-semantic` shall answer `warn` with a `jev-meta-duty` finding. | #57 semantic | fixture of a deleted assertion titled "refactor" | warn + finding; failure: pass | key-less: `stack run outbound --replay fixtures/jev/semantic/<case>.json` exits 0 = match; key: O2 |
| P3 | When the PR text claims what the diff does not show, `jev-semantic` shall answer `warn` with a `jev-unsupported-claim` finding. | #57 semantic | "add tests" with no test change | as P2 | as P2 |
| P4 | `jev-semantic` shall never make `stack run outbound` exit 1. | §1 hard boundaries | any answer | exit 0 | all semantic fixtures replayed; exit 1 only on `expect` mismatch |
| P5 | When a kit is opened and Jev rates a reference at or above its cutoff, the entry context shall list at most 3 reference paths within 1,000 chars, at `kit open` and after compaction. | #57 router | synthetic kit about a PR review | pointer block after the kit; failure: none, > 3 or cut | key-less: `stack run entry --replay fixtures/jev/router/<case>.json`; key: O3 (`kit open`, `/compact`) |
| P6 | When observe found owner replies, `jev-classify` shall label each `correction`, `drift`, `approval` or `other` and mark corrections already in memory. (After #56.) | #57 classify | synthetic replies + memory index | classify `latest.json` and context counts | key-less: `stack run cycle --replay fixtures/jev/classify/<case>.json`; key: O4 |
| P7 | Each extension shall send only the pinned model, and its cutoffs shall be the calibration result on `fixtures/jev/`. | #57 pin and cutoffs | any call | `model` `jev-1.14.0` in ledger and records | `go test ./extensions/...`; O1 output equals committed cutoffs |
| P8 | When a fixture's request carries recorded answers, the extension shall answer from them without a key or network, and `warn` on an unrecorded request. | #57 replay | replay with key unset, network off | match; unrecorded → `jev-unrecorded` | every fixture replayed with key unset |
| P9 | If Jev fails, times out or answers malformed, the stage shall continue: `pass` on a transient failure and `warn` on a persistent one, within the stage deadline. | #57; §3.1 | fake server | per §3.1 | package tests (C2); key: O5 (wrong key → warn) |
| P10 | Each call shall record its input tokens, and per-run cost shall stay near §3.6. | #57 cost | real runs | `usage.jsonl` lines | key: O6 after a week |

### 6.2 Plan layer

| ID | EARS claim | Source | Test |
|---|---|---|---|
| C1 | While the key is empty and no `jev` object is present, `Main` shall write `{"status":"pass"}` after reading stdin to EOF and touch no file. | P1 | `jev.TestOptOut` per binary (fake HOME stays empty) |
| C2 | The client shall classify network errors, own timeout, 429, 529, 5xx as transient and 401/403/400/413/415/422, bad JSON, model mismatch, missing or mistyped answers and non-finite or out-of-range probabilities as persistent, never echoing the body. | P9 | `jev.TestClientErrors` (`httptest`) |
| C3 | The record key shall be the SHA-256 of the exact body; with `jev` present no request shall be sent. | P8 | `jev.TestRecord` (server that fails the test on any call) |
| C4 | `jev-semantic` shall build its state from `title`, `body`, `branch`, commit messages and the diff of `range`, cut at 64 KiB by whole files with `omitted_files`, and ask once more at half the cap on `max_tokens_exceeded`. | §3.2 | `semantic.TestState` (`gittest` repo) |
| C5 | A probability at or above the cutoff shall flag, below shall not; a disabled question never flags; the answer is never `block`. | P2–P4 | `semantic.TestDecision` |
| C6 | `jev-router` shall resolve candidates from `$CLAUDE_CONFIG_DIR` or the home directory and `jev.toml` globs, summarise each in ≤ 400 bytes and send no request without kit state or prompt. | P5 | `router.TestCandidates` |
| C7 | The router context shall hold ≤ 3 lines ordered by probability, ≤ 1,000 chars, and be empty when none passes. | P5 | `router.TestContext` |
| C8 | `jev-classify` shall read only `owner-reply` items of a fresh observe file, batch ≤ 40 per state, ask `dup_` only for flagged items, and write its file atomically. | P6 | `classify.TestRun` |
| C9 | Cutoff selection shall pick the lowest step meeting the precision target on mean scores, list cases within ±0.05, and disable a question under 6 + 6 cases. | P7 | `jev.TestCutoff` |
| C10 | Every fixture record and cutoff table shall name the pin, and cutoffs recomputed from `label.scores` shall equal the committed ones. | P7 | `jev.TestFixturesPinned` |
| C11 | Every fixture shall replay to its `expect` with the key unset. | P2, P3, P5, P6, P8 | `<ext>.TestFixtures` (runs `Main` on each fixture from the repository root) |
| C12 | Each call shall append one ledger line with `input_tokens` and outcome. | P10 | `jev.TestLedger` |
| C13 | No package under `cmd/` or `internal/` shall import `extensions/`, and the jev packages shall import no `internal/` package. | owner decision | existing `depguard` rules |

## 7. Test plan

CI never reaches the network: the client is tested against `httptest` servers (C2, C3, C12), the extensions through
`Main` with recorded fixtures (C11), state building against `internal/gittest` repositories (test-only import,
allowed), all under `scripts/test` on the three OSes. Real-key checks are owner steps:

- **O1** each PR: `jev-calibrate` on its fixtures; committed cutoffs equal the output; unstable list read.
- **O2** semantic: `stack run outbound --ext jev-semantic --range <r> --title-file … --body-file …` on a scratch
  branch that deletes an assertion → warn; on a clean change → pass; ledger line written.
- **O3** router: `kit open demo` with a state about a review → pointer block; `/compact` → the same block restored.
- **O4** classify (after #56): `stack run cycle --ext jev-classify` after a real observe run.
- **O5** `TYPESAFE_API_KEY=invalid` → `warn` `jev-error` 401 at each stage; network off → `pass` with stderr line.
- **O6** after a week: sum `usage.jsonl` against §3.6.

## 8. Risks

- Fixtures are public, so router and classify are calibrated on synthetic texts, not the owner's real
  distribution; real precision may be lower. The ledger and O6 show the volume; a recalibration adds cases.
- Jev's quality on Japanese text (references, kit state, replies) is unmeasured (https://docs.typesafe.ai/model-jaggedness.md); the fixtures include Japanese cases so calibration measures it.
- Noise near a cutoff (p90 0.05, max 0.30 in prior art) flips single answers; the unstable list makes it visible.
- No deprecation policy for pinned IDs: when `jev-1.14.0` is retired, calls return a persistent error → `warn`
  at every stage, which is the signal to run the model-bump PR.
- At compaction core may leave the router under 4 s; a kill then shows core's synthetic warn marker once.
- Semantic starts after `public`; a `public` scan that runs into its 100 s limit leaves semantic less than 10 s,
  and core's deadline answers for it (rare: `public` is a local git scan).
- The launchd job's environment needs the key for classify; without it classify opts out silently (P1).

## 9. Replacement text for `docs/architecture.md` §6 item 4

> 4. **Phase 4 (Jev extensions):** Add `jev-semantic` -> `jev-router` -> `jev-classify`, each with cutoffs
> calibrated on `fixtures/jev/` and key-less `--replay` verification; `jev-classify` follows the end of Phase 3
> (#56), since only it consumes observe's output. Design: `docs/design/phase4-jev.md`.

## 10. Owner questions

- **Q1** May `jev-classify`'s labelled corrections enter improve's model brief? They are owner text, and improve
  today sends only public evidence to a model whose output becomes a public PR. Default without an answer: no;
  classify reports to `last.json` only.
