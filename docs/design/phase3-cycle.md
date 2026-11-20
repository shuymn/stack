# Phase 3 Design: `cycle` Stage (`observe`, `improve`) and Kaizen Retirement

Implementation design for Phase 3 of `docs/architecture.md` §6: replace the legacy kaizen cycle of `shuymn/dotfiles`
with `stack run cycle` and two minimal extensions in this repository, `stack-observe` and `stack-improve` (with
`writing.md`), switch `launchd` to `stack run cycle`, and retire `apply.py`, `ready.py`, the old `improve.py` and
`.claude/kaizen-managed`. It also moves the last callers of `attention-budget.py status` and
`public-check --host-toml` that Phase 2 left in place (`docs/design/phase2-core.md` §7).

Extensions talk to core only through `docs/architecture.md` §3 "Extension Protocol" and the documented CLI. Legacy
sources are cited as `dotfiles:<path>:<line>` at `shuymn/dotfiles` main as of this design; `K` is the legacy state
directory `~/.claude/kaizen`.

## 0. Principles

1. **Stateless observation, one stamp for improvement.** `observe` recomputes a fixed 7-day window on every run and
   keeps no watermark (legacy parity, `dotfiles:scripts/kaizen/cycle.py:2-12`). `improve` keeps exactly one state
   file per ISO week (`docs/architecture.md` §4 S3); there are no attempt directories, journals or recovery states.
2. **The owner decides; the cycle reports.** Nothing in the cycle applies a change to the live `~/.claude`, merges,
   or edits the owner's checkout. A change reaches the owner as one pull request; applying a merged change is the
   owner's `make apply` (`docs/architecture.md` §4 S3).
3. **No over-defensive machinery** (`docs/architecture.md` §1 drop list): no live swapper, no owner-only merge list,
   no `--restricted` sandbox, no `commit-tree -S`, no recovery state machine, no budget caps. What remains is what
   publishing to a public repository needs: public evidence only, `stack check public`, one open PR at a time, a
   spending cap.
4. **Never block.** The `cycle` stage has no caller to stop. Extensions answer `pass` or `warn`, never `block`, so
   `stack run cycle` exits 0 unless core itself fails (exit 2).
5. **macOS is the runtime, every OS compiles.** The extensions are Go in this module, built and tested on all three
   CI runners like core. They read what they find; a missing source on Linux or Windows is a source status, not a
   crash. Scheduling (`launchd`) stays macOS only (`docs/architecture.md` §1).

## 1. Overview

```text
launchd (macOS, daily 06:00)
  └─ scripts/stack-cycle (dotfiles)    cd <dotfiles checkout>; stack run cycle > last.json (temp + rename)
       └─ stack run cycle               [stages].cycle = observe, jev-classify, improve (§5 defaults)
            ├─ stack-observe            7-day window → ~/.local/state/stack/observe/latest.json + warn findings
            ├─ stack-jev-classify       Phase 4; absent = skipped
            └─ stack-improve            ISO-week stamp; public evidence from latest.json → worktree + claude -p
                                        → stack check public → push improve/<YYYY-Www> → gh pr create
statusline ← ~/.local/state/stack/cycle/last.json        owner ← PR on GitHub, `make apply` after merge
```

## 2. Legacy Inventory

Every behaviour of the legacy cycle, with the v10 decision. "Owner command" rows are commands the owner ran by hand.

| Legacy behaviour (source) | v10 | Why |
|---|---|---|
| Daily launchd job `kaizen-cycle`, 06:00, store copy of `scripts/kaizen`, log file (`dotfiles:nix/home/profiles/dev.nix:78-103`) | **Changed**: job `stack-cycle` runs `scripts/stack-cycle` (§3.6); same time, same log style | §4 S3 seam `launchd -> stack run cycle` |
| One run = apply, observe, watch, ready, weekly, publish; 20-min run deadline, 62 min with the weekly stage (`cycle.py:62,320-359,362-386`) | **Changed**: `stack run cycle` runs the configured extensions in order under core's 30-min stage deadline; each extension keeps its own time budget (§3.9) | §3 Extension Protocol limits |
| Run date and ledger days in a fixed time zone (`observe.py:49`, `cycle.py:333`) | **Changed**: window is `[now-7d, now)` in UTC; the improve week is the ISO week of the local date | no private zone constant in a public extension |
| Apply stage: mirror fetch, eligibility, `renameatx_np` swap, journal, backup, cursor `L` (`apply.py:1-1218`, called at `cycle.py:277-293`) | **Dropped**; the owner runs `make apply` after a merge | §1 (no custom live swapper), §4 S3 "Apply via `make apply`" |
| Owner commands `cycle.py adopt`, `resolve`, `apply.py --check` (`cycle.py:16-17,397-410`, `apply.py:1059,1121,1193`) | **Dropped** with the apply stage | same |
| Watch of merged changes, `cycle.py baseline`, `watch.json` (`cycle.py:13-15,138-175`, `watch.json:1-3`) | **Dropped** | §4 S3 retires `watch.json` |
| Regression-unit signal for watch and probe (`signals.py:494-550`) | **Dropped** with watch; the probe tool (`scripts/kaizen/probe/`) is deleted with it (history keeps both) | only watch and the probe used it; the probe also imports `public-check` (`probe/build:231`) |
| Transcripts source: parent and subagent `*.jsonl` by mtime, complete lines only, regular files only (`observe.py:84-95,117-201`) | **Kept** (§3.3) | observation is S3's job |
| `owner-reply` signal: owner utterance ≤ 300 chars after an assistant turn (`observe.py:151-160`) | **Kept** as an unlabelled candidate; classification of corrections is `jev-classify`'s (Phase 4) | §4 Optional Jev |
| `meta-effort` signal: tool calls naming ledgers, `retro.md`, `budget.json`, `attention-budget.py` (`observe.py:39,173-180`) | **Dropped** | every named file is retired by this phase (§5), so the signal measures nothing |
| `hiden-in-worker`, `reference-unread` signals (`observe.py:175-182,204-207`) | **Kept** | cheap, transcript-only waste signals |
| T3 Code database source, read-only SQLite (`observe.py:213-260,419-425`) | **Changed**: read through the `sqlite3` command-line tool (`-readonly -json`), off when `~/.t3` is absent | keeps owner utterances of T3 sessions without a Go SQLite dependency (§4.6) |
| Codex sessions source, used for repo weights and a count (`observe.py:266-297`) | **Dropped** | repo selection uses transcript cwds only; the count fed nothing |
| Review-ledger `ledger-miss` source (`observe.py:300-321`) | **Dropped** | ledgers are retired with `retro.md` (§5) |
| gh source: top 10 repos by cwd weight, merged reverts, fix PRs, failed default-branch runs, public provenance (`observe.py:324-378`) | **Kept** | only source of public evidence for improve |
| Readiness R1: `chezmoi diff` over the managed list (`ready.py:61-89`) | **Dropped** | needs `.claude/kaizen-managed`; drift of the session-edited skills is already shown by the statusline's `✎` mark (`comp_pending`, `dotfiles:home/dot_claude/executable_statusline.sh.tmpl`); drift of `agents/` and settings is no longer reported (the owner's `chezmoi diff`) |
| R2: launchd job loaded, program paths exist (`ready.py:100-110`) | **Changed**: a job that does not run leaves `last.json` stale, which the statusline shows (§3.7) | absence is observable without a check |
| R3: `attention-budget.py status` shows no `OVER` (`ready.py:113-118`) | **Dropped** | §1 no budget ratchets; `stack budget status` is display-only |
| R4: every source read, `gh auth status` passes (`ready.py:121-127`) | **Kept** as observe source statuses and a `gh-auth` finding | observability |
| R5: last run ≤ 3 days, open kaizen PR ≤ 14 days (`ready.py:150-173`) | **Changed**: run age → statusline staleness (36 h); PR age → improve `warn` over 14 days | same signals, no extra check |
| Publish `record.json`, `digest.md`, `status.json` into `K/runs/<id>`, swap `K/current` by rename under `.publish.lock`, prune (`cycle.py:86-121,348-357`) | **Changed**: `scripts/stack-cycle` writes the `stack run cycle` aggregate to `~/.local/state/stack/cycle/last.json` by temp + rename; observe writes `latest.json` the same way | one aggregate already carries every result (§3 Result) |
| Japanese digest with decisions list (`cycle.py:178-274`) | **Changed**: each extension's `context` is a short English digest; owner-actionable items are `warn` findings | protocol `context`/`findings` |
| Statusline `kaizen ✗N・判断 N・stale Nd` from `K/current/status.json` (`statusline.sh.tmpl:151-175`) | **Changed**: `cycle` segment from `last.json` (§3.7) | |
| Weekly stage enabled by the file `K/improve-enabled` (`improve.py:4,175-179`) | **Changed**: installing `stack-improve` on `PATH` enables it; the target repository opts in with `.stack/improve.toml` (§3.4) | §3 discovery: missing = skipped |
| `K/.improve.lock` (`improve.py:182-190,378-387`) | **Dropped**: the exclusive creation of the week stamp is the mutual exclusion | §4 S3 single stamp |
| Attempt directories `K/proposals/<date>-<id>/result.json`, newest-attempt scan (`improve.py:202-332`) | **Changed**: one stamp `~/.local/state/stack/improve/YYYY-Www` holding the outcome | §4 S3 |
| Gate: newest attempt younger than 7 days, `failed:model-unavailable` not counted (`improve.py:53,467-469`) | **Changed**: one attempt per ISO week; the stamp is removed when the model did no billed work | §4 S3 (calendar week, not rolling) |
| Gate: uncleared `failed:remote-unknown`, owner `cycle.py clear` (`improve.py:349-366,464-466`) | **Dropped** | §1 no recovery state machine |
| Gate: any open `kaizen/` PR of the repo, all pages (`improve.py:473-479`, `ready.py:130-139`) | **Kept** with branch prefix `improve/` | one open PR at a time (owner rule, `dotfiles:docs/kaizen.md` §1 principle 6) |
| Public evidence only: public gh items whose `where` is a PR or Actions run URL (`improve.py:64,480-488`) | **Kept** | everything improve publishes must be explained by public material |
| No public evidence: write `proposal.md`, no model, week used (`improve.py:490-499`) | **Changed**: `pass` with context, no stamp, retried the next day at no model cost | a free retry beats waiting a week |
| GitHub source not `ok` → retry tomorrow (`improve.py:482-485`) | **Kept**: improve needs a fresh `latest.json` whose `gh` source is `ok` | an unread source is not evidence of absence |
| Preflight: `host.toml`, `private-terms`, signing key, `claude auth status` (`improve.py:502-515,533-548`) | **Changed**: no preflight; a failing step before the model call removes the stamp and warns | the steps themselves report |
| Endpoint env copied from settings because `--restricted` ignores them; credential redaction and leak check (`improve.py:257-281,517-531,605-606`) | **Dropped**: `claude -p` reads the owner's settings normally | §1 drops `--restricted` |
| Bare mirror of the public URL, `git archive` temp tree (`improve.py:554-555,596-598`) | **Changed**: `git fetch origin <base>` in the target checkout, then a detached `git worktree add` of the fetched commit | §4 S3 "plain worktree" |
| `claude -p` with `--safe-mode --restricted --strict-mcp-config`, tools `Read,Edit,Write,Grep,Glob`, JSON schema, Opus xhigh, `--max-budget-usd 50`, 40-min timeout (`improve.py:56-60,633-658`) | **Changed**: same tools, schema, model, effort and budget; `--safe-mode` and `--restricted` dropped (the owner's hooks, including `stack hook claude pre-tool-use`, apply); 20-min timeout | §1; §3.9 time budget |
| Result checks: schema, success subtype, cited evidence ids ⊆ brief ids (`improve.py:610-611,650-658`) | **Kept** | |
| Tree check: only modifications of existing `100644` files (`improve.py:709-743`) | **Kept**, plus: every changed path matches `.stack/improve.toml` `paths` | publishable scope |
| Signed commit by `commit-tree -S` with the key path from a private config (`improve.py:760-772`) | **Changed**: plain `git commit -F` in the worktree; the owner's git config signs as it does for every commit; hooks run | §1 drops `commit-tree -S` |
| Checks: one commit, `apply.assess`, prose caps, relative links (`improve.py:774-792,807-826,745-758`) | **Dropped** except "one commit" | assess served apply; caps are §1-dropped; links are the reviewer's |
| `public-check --host-toml --private-terms` before push and in recovery (`improve.py:794-805,965`) | **Changed**: `stack check public <base>..HEAD --title-file --body-file --branch` | Phase 2 core (§4.5); host values live in `private-terms` |
| Codex `exec -s read-only` HIGH-finding gate (`improve.py:828-855`) | **Dropped** (owner to confirm, §9 Q1): the PR is reviewed before the owner merges it | the owner merges every improve PR; a pre-PR gate duplicates that review |
| Push with lease to a new `kaizen/improve-<id>` branch, `gh pr create`, lookup and adopt, recovery create (`improve.py:863-971`) | **Changed**: `git push origin HEAD:refs/heads/improve/<YYYY-Www>` without force, then one `gh pr create`; a failure is a `warn` naming the branch and the command to finish | §1 no recovery state machine |
| Cost recorded or `unknown`, Codex run count (`cycle.py:228-229,271-273`, `improve.py:143-150,587`) | **Kept** for Claude cost in the stamp and context; Codex count dropped with the gate | |
| `writing.md` given to the model from the base commit (`improve.py:62,556,599`; `writing.md:1-9`) | **Changed**: `extensions/improve/writing.md` embedded in `stack-improve`; the caps rules (`writing.md:3,7`) are removed | §2 layout; §1 no budget ratchets |
| Managed-path list `.claude/kaizen-managed` and its mentions in the Codex and pi instructions (`kaizen-managed:1-7`, `home/dot_codex/AGENTS.md.tmpl:4`, `home/dot_pi/agent/AGENTS.md.tmpl:4`) | **Dropped** | §1 (owner-only merge list) |
| Kaizen tests and fakes (`tests/test_kaizen_*.py`, `tests/kaizen_fakes.py`); CI paths (`.github/workflows/automation.yml:13,24`) | **Dropped**; the statusline tests in `test_kaizen_cycle.py:771-` move to `tests/test_statusline.py` | |

### 2.1 Callers left by Phase 2

`attention-budget.py status` (`dotfiles:home/dot_claude/hooks/executable_attention-budget.py:158-173`):

| Caller | Migration |
|---|---|
| R3, `ready.py:113-118` (from `cycle.py:337`) | deleted with `ready.py` |
| `improve.py:807-826` executes the hook source for prose caps | deleted with `improve.py` |
| `dotfiles:AGENTS.md:51` validation row ("shows no `OVER`") | the budget clause is removed; `stack budget status` may be run to see sizes, with no pass/fail |
| `hiden/references/attention.md:5` (usage command) and `:6-7` (caps that only go down) | rewritten around `stack budget status`, display-only, no caps (§5) |
| `hiden/SKILL.md:58` ("pass the attention budget first") | clause removed (§5) |
| the hook's own deny text, `attention-budget.py:202` | the file is deleted (it is unwired since Phase 2) |
| `writing.md:3,7` (caps only go down, lower the caps) | rewritten in `extensions/improve/writing.md` |

`stack budget status --json` (left open in `phase2-core.md` §4.6) is not added: the statusline does not show the
budget.

`public-check`:

| Caller | Migration |
|---|---|
| `--host-toml --private-terms`: `improve.py:796-798`, reached from `check()` `:789` and recovery `:965` | `stack-improve` runs `stack check public` (§3.4) |
| `scripts/stack-private-terms:27-44` imports `host_values`, `private_terms` and `git` from it | the ~20 lines of `host_values` (`public-check:76-89`) move into `stack-private-terms`; the hand-kept term list moves from `K/private-terms` to `~/.config/stack/private-terms.local` |
| `home/.chezmoiscripts/run_onchange_after_write-stack-private-terms.sh.tmpl:6` hashes `public-check` and `K/private-terms` as inputs | inputs become `stack-private-terms` and `private-terms.local` |
| `scripts/kaizen/probe/build:231` | deleted with the probe |
| `tests/test_kaizen_public_check.py` | deleted; its vectors are ported in `internal/publictext` and `internal/checkpub` (Phase 2 PR 3) |

## 3. Contracts

### 3.1 Layout and language

```text
extensions/
  observe/                 package observe (sources, signals, replay, gh version, latest.json)
    cmd/stack-observe/     main: protocol I/O only
    testdata/              anonymised transcript excerpts, T3 rows, gh answers
  improve/                 package improve (stamp, evidence, worktree, model, checks, publish)
    cmd/stack-improve/     main
    writing.md             embedded with go:embed
```

**Go, in this module.** Both binaries build with `go install ./extensions/observe/cmd/stack-observe
./extensions/improve/cmd/stack-improve` (with `GOBIN` set to the directory that holds `stack`), so the installed
names match core's `stack-<name>` discovery. They are `package main` + library like `cmd/stack` + `internal/`, run
under `scripts/test` and the Windows CI job, and need no runtime besides the tools they call (`stack`, `git`, `gh`,
`claude`, `sqlite3`). Boundary: `.golangci.yaml` enables `depguard` with two rules: non-test files under
`extensions/` may not import `github.com/shuymn/stack/internal/...`, and nothing under `cmd/` or `internal/` may
import `github.com/shuymn/stack/extensions/...`. Extension tests may import `internal/proc/proctest` and
`internal/gittest` (test infrastructure only). §4.1 compares the alternatives.

Both read the protocol request from stdin (ignoring `range` and `prompt`), write one response to stdout, write
progress to stderr (core prefixes it), and treat any internal error as `warn` with a finding, never a crash without
a response.

Finding shape for both: `{"kind": "<slug>", "where": "<file:line | URL | path>", "detail": "<≤ 300 chars>"}`.

### 3.2 Core addition: `stack --version --json`

Prints `{"version":"<version>","gh_model":"<ghargs.GhVersion>"}` and exits 0; `stack --version` is unchanged.
`stack-observe` needs the `gh` release the guard's argument model was written against (§3.3 gh version) and must not
import `internal/ghargs`. This is an additive CLI change (`docs/architecture.md` §3 gains it in the same PR).

### 3.3 `stack-observe`

Input: request (only `stage` is read). Window `[now − 7 d, now)`. Self-imposed deadline: 5 min (§3.9); on expiry
the sources not finished are `status: "timeout"` and the response is `warn`.

**Sources** (each independent; a failure marks that source and the others run):

| Source | Reads | Status |
|---|---|---|
| `transcripts` | `~/.claude/projects/*/*.jsonl` and `…/subagents/**/*.jsonl` with mtime in the window; complete lines; regular files only (legacy `observe.py:84-95,187-201`) | `ok`, `fail` (any unreadable file, listed), `off` when the directory is absent |
| `t3` | `sqlite3 -readonly -json <T3 state database> <query>` (the file legacy opens, `observe.py:422`) with the legacy queries (`observe.py:229-258`); window literals formatted by observe | `off` when `~/.t3` is absent; `fail` when `sqlite3` is missing or errors |
| `gh` | for the top 10 repos by transcript cwd weight: `gh repo view`, merged PRs, failed default-branch runs (`observe.py:342-378`); `gh auth status` | `fail` on any gh error; a failing `gh auth status` adds finding `gh-auth` |
| `replay` | §3.3 replay | `ok`, `fail`, `off` when `stack` is not on `PATH` |
| `gh-version` | §3.3 gh version | finding only |

**Signals** (items, de-duplicated by id = first 12 hex of SHA-1 of `signal\0where`, `observe.py:73-76`):
`owner-reply`, `revert`, `fix-pr`, `ci-fail-main`, `hiden-in-worker`, `reference-unread`, with the legacy matching
rules cited above. Each item: `id, signal, kind (miss|waste), where, excerpt (≤ 160 chars), provenance
(owner|public|synthetic), repo, ts`.

**Replay of PreToolUse decisions** (owner-approved addition). For every assistant `tool_use` block in a window
transcript whose name is one the guard is wired for (`Bash`, `Read`, `NotebookRead`, `Grep`, `Glob`,
`mcp__pencil__execute`; `phase2-core.md` §4.3 wiring):

1. *Recorded decision*, from the `tool_result` with the same `tool_use_id`: content starting
   `PreToolUse:<tool> hook error: stack guard (<rule>)` → `deny:<rule>`; any other `PreToolUse:<tool> hook error:`
   → `other-deny` (another hook; not compared); a result that is not a hook error → `ran`; no result, or a user
   permission refusal → `unknown` (not compared).
2. *Replayed decision*: payload `{"session_id","transcript_path","cwd","hook_event_name":"PreToolUse","tool_name",
   "tool_input"}` from the record, piped to `stack hook claude pre-tool-use` (the `stack` first on `PATH`, i.e. the
   installed hook), 5 s timeout each (the hook's settings timeout), 4 at a time. Deny JSON → `deny:<rule>` parsed
   from `stack guard (<rule>)`; empty stdout → `ran`. A record whose `cwd` no longer exists is counted
   `unrecoverable` and not run. Identical `(cwd, tool_name, tool_input)` payloads run once. At most 5,000 payloads
   per run, newest first; the rest are counted `truncated`.
3. *Reported change* (finding `replay-change`, at most 20 listed, all counted): `ran → deny:<rule>` (new deny: a false
   positive or a newly guarded call), `deny:<rule> → ran` (lost deny), `deny:<a> → deny:<b>` (rule changed).
   `where` is `transcript:line`; `detail` holds the tool, both decisions and the first 160 chars of the new reason;
   the tool input itself is never copied into findings or `latest.json`.

The replay reads only: the hook itself only reads files (body files, symlink targets). A changed decision can also
come from a file that changed since the call (a body file, a `.git` directory), so a finding is a candidate for the
owner to read, not proof of a regression. Replay findings are never public evidence.

**gh version** (owner-approved addition). `gh --version` first line `gh version X.Y.Z …` → `vX.Y.Z`;
`stack --version --json` → `gh_model`. When they differ, finding `gh-version` with both versions and the advice to
check `internal/ghargs` against the installed `gh` (a flag the table lacks makes the guard scan the rest of a call
as literal text, `phase2-core.md` §4.3). `gh` missing, or a `stack` without `--version --json`, is finding
`gh-version` with `unknown`.

**Response.** `status`: `warn` when any source is `fail`/`timeout` or any `gh-auth`, `replay-change` or
`gh-version` finding exists; else `pass`. `context`: ≤ 4,000 chars, English: window, per-source status and counts,
per-signal counts with up to 3 excerpts each, replay totals (`compared`, `changed`, `unrecoverable`, `truncated`),
installed vs modelled gh. `findings`: as above.

**File** `~/.local/state/stack/observe/latest.json` (0600, temp file in the same directory + rename, so readers
never see a partial file; last writer wins):

```json
{"version": 1, "window": {"since": "…Z", "until": "…Z"},
 "sources": {"transcripts": {"status": "ok", "files": 0, "records": 0, "error": ""}, "t3": {}, "gh": {}, "replay": {}},
 "items": [{"id": "…", "signal": "revert", "kind": "miss", "where": "https://github.com/o/r/pull/1",
            "excerpt": "…", "provenance": "public", "repo": "o/r", "ts": "…Z"}],
 "replay": {"compared": 0, "changed": 0, "unrecoverable": 0, "truncated": 0},
 "gh": {"installed": "v…", "model": "v…"}}
```

`version` is this file's schema; `stack-improve` refuses (`warn`) a version it does not know.

### 3.4 `stack-improve`

Input: request `repo` (the Git top-level of the cwd; `scripts/stack-cycle` runs from the dotfiles checkout). Every
step has its own timeout; the whole run stays under 24 min (§3.9). Order:

1. **Target.** `repo == ""` → `pass` "not in a repository". `origin` must be a GitHub URL (`owner/name` parsed as
   in `observe.py:41`). `git fetch origin <base>` (timeout 60 s; failure → `warn`). Config is read from the
   fetched commit, `git show <fetched>:.stack/improve.toml`, so only merged config counts:
   ```toml
   base = "main"                                                   # optional, default "main"
   paths = ["home/dot_claude/skills/**", "home/dot_claude/agents/**"]   # required; ** = any directories
   ```
   Absent → `pass` "`<repo>` has no .stack/improve.toml". Invalid → `warn`.
2. **Week stamp.** `W` = ISO week of the local date, file `~/.local/state/stack/improve/<YYYY>-W<ww>` (e.g.
   `2026-W47`). If it exists: `pass` with its outcome, or `warn` when the outcome is one the owner must act on
   (`pushed-no-pr`, `proposal:*`, `failed:*`).
3. **Evidence.** Read `~/.local/state/stack/observe/latest.json`: missing, schema unknown, `window.until` older than
   24 h, or `sources.gh.status != "ok"` → `pass` "no fresh observation" (no stamp). Public items = `provenance ==
   "public"` and `where` matches `^https://github\.com/[\w.-]+/[\w.-]+/(pull|actions/runs)/\d+$`
   (`improve.py:64`). None → `pass` "no public evidence" (no stamp).
4. **Open PR gate.** `gh pr list --repo <slug> --state open --limit 200 --json number,headRefName,
   headRepositoryOwner,createdAt,url`; any head `improve/…` owned by `<slug>`'s owner → `pass` naming it, or `warn`
   when it is older than 14 days. gh error → `warn` (no stamp).
5. **Claim the week.** Create the stamp with `O_CREATE|O_EXCL` (0600): `{"week","started","repo","base_oid",
   "outcome":"started"}`. `EEXIST` → `pass` "attempted by another run". From here every exit rewrites the stamp
   (temp + rename) with its outcome; the rule for removing it is in step 7.
6. **Worktree.** Remove leftovers of earlier runs (directories under `~/.local/state/stack/improve/work/`, then
   `git worktree prune`), then `git worktree add --detach <work>/<W> <base_oid>`. The owner's working tree and
   branches are never touched; `fetch` updates only `origin/<base>`.
7. **Model.** `claude -p --strict-mcp-config --tools Read,Edit,Write,Grep,Glob --permission-mode acceptEdits
   --permission-prompts none --no-session-persistence --output-format json --json-schema <schema> --model
   claude-opus-5-5 --effort xhigh --max-budget-usd 50` with cwd = the worktree, environment inherited, timeout
   20 min. Prompt on stdin: the evidence brief (`id | signal | URL | excerpt` of public items only,
   `improve.py:589-593`), the embedded `writing.md`, and the task (`improve.py:73-87` with the allowed `paths` from
   the config instead of the fixed directories). Schema as `improve.py:88-92`. Cost = `total_cost_usd` when finite,
   else `unknown`. If the run did no billed work (cost 0 and no tokens, `improve.py:296-303,653-656`), or failed
   before the model was called (steps 5-6), the stamp is **removed** and the response is `warn`: the week is not
   used. Otherwise the stamp stays, whatever follows.
8. **Result checks.** Not `success`, schema mismatch or timeout → outcome `failed:model-result` /
   `failed:timeout`, `warn`. `change: false` → `no-change`, `pass`. Evidence ids empty or not ⊆ brief ids →
   `proposal:evidence-ids`. `git status --porcelain=v2 -z` must show only modifications of existing `100644` files
   that match `paths` (no add, delete, rename, mode or type change) → else `proposal:tree`. Blank or multi-line
   title, blank message or body → `proposal:metadata`. Every `proposal:*` stores `git diff` (≤ 256 KiB, then cut
   with a marker) in the stamp's `patch` field and answers `warn`.
9. **Commit.** Body = model body + `Evidence:` list of the cited URLs (`improve.py:617-621`). `git -C <worktree>
   commit -a -F <msg>` with the owner's git configuration (signing included) and the repository's hooks; never
   `--no-verify`. Failure → `failed:commit`, `warn`.
10. **Public check.** In the worktree: `stack check public <base_oid>..HEAD --title-file <t> --body-file <b>
    --branch improve/<W>`. Exit 1 → `proposal:public-check` (hits in the stamp), exit 2 → `failed:check`, both `warn`.
11. **Publish.** `git push origin HEAD:refs/heads/improve/<W>` (no force; an existing branch rejects it →
    `failed:push`, `warn`). Then `gh pr create --repo <slug> --base <base> --head improve/<W> --title <title>
    --body-file <b>` with the checked bytes. Success → outcome `pr <url>`, `pass`, URL in `context`. Failure →
    `pushed-no-pr`, `warn` with the exact `gh pr create` command to finish by hand. No lookup, no retry, no adopt.
12. **Cleanup.** `git worktree remove --force <work>/<W>` on every path after step 6 (the worktree is
    improve's own); a failure is a stderr line, and step 6 of a later run removes the leftover.

`context` always states the outcome, the week, the cost and, when one exists, the PR URL or the stamp path.

**Stamp file** (`~/.local/state/stack/improve/YYYY-Www`, JSON): `week, started, finished, repo, base_oid, outcome,
cost_usd (number|"unknown"), evidence_ids, branch, pr, reason, patch, check_hits`. The only state improve keeps.

### 3.5 State files

| Path | Writer | Readers | Lifetime |
|---|---|---|---|
| `~/.local/state/stack/cycle/last.json` | `scripts/stack-cycle` (temp + rename) | statusline, owner (`jq -r '.results[].context'`) | overwritten daily |
| `~/.local/state/stack/observe/latest.json` | `stack-observe` | `stack-improve`, owner | overwritten each observe run |
| `~/.local/state/stack/improve/YYYY-Www` | `stack-improve` | `stack-improve`, owner | one per week; never pruned automatically (tiny) |
| `~/.local/state/stack/improve/work/<W>/` | `stack-improve` | — | removed at the end of the run or by the next run |

`phase2-core.md` §2.1 already reserves the improve stamp path.

### 3.6 launchd wiring (dotfiles)

`launchd.agents.stack-cycle` in `nix/home/profiles/dev.nix` replaces `kaizen-cycle` in the same commit, so the two
cycles never both open PRs: `StartCalendarInterval` 06:00, `ProcessType = Background`, `LowPriorityIO`, log
`~/Library/Logs/stack-cycle.log`, `PATH` = the directory holding `stack` and the extensions, the Home Manager
profile, the mise shims (`claude`, `gh`) and the system directories (`sqlite3`, `git`). Program:
`/bin/sh <store copy of scripts/stack-cycle>`:

```sh
#!/bin/sh
# Run the cycle stage from the dotfiles checkout and keep its aggregate for the statusline.
set -u
cd "$(dirname "$(chezmoi source-path)")" || exit 1
out=$HOME/.local/state/stack/cycle
mkdir -p "$out"
tmp=$(mktemp "$out/.last.XXXXXX")
stack run cycle >"$tmp"
code=$?
mv -f "$tmp" "$out/last.json"
exit "$code"
```

An empty file (core failed with exit 2 before writing) is kept on purpose: the statusline shows it as unreadable.
`stack run cycle` itself works on every OS; only this job is macOS-specific.

### 3.7 Statusline

`comp_cycle` replaces `comp_kaizen` (`statusline.sh.tmpl:151-175`) and reads `~/.local/state/stack/cycle/last.json`:

- file absent → nothing (cycle not set up);
- `jq` cannot read `.status` → `cycle ?`;
- `.status` other than `pass` → `cycle <status> <N>`, `N` = total `findings` over `.results`;
- file mtime (`date -r <file> +%s`, BSD and GNU) older than 36 h → `stale <d>d` appended (or alone when `pass`).

Shown in red; nothing when `pass` and fresh. Tests move to `tests/test_statusline.py` with fixture files.

### 3.8 How a proposal reaches the owner

A change is one PR on the target repository's origin, branch `improve/<YYYY-Www>`, created by the owner's own `gh`
and `git` credentials, body ending in the evidence URLs. The owner reviews it (and may run a Codex review, §9 Q1),
merges it in the GitHub UI as for any PR there, and runs `make apply`. A change that could not be published
(`proposal:*`, `pushed-no-pr`, `failed:*`) appears as a `warn` in `last.json` and in the statusline, with the patch
or the finishing command in the stamp.

### 3.9 Time budget

Core gives the whole `cycle` stage one 30-min deadline and kills only the direct child at expiry (`phase2-core.md`
§2.2), so each extension must finish first and kill its own children. Budgets: observe ≤ 5 min (a whole legacy daily run,
apply and readiness included, takes about 102 s in the cycle log); improve ≤ 24 min: fetch 1 min, model 20 min, commit, check, push and
PR 3 min. `jev-classify` (Phase 4) must fit in what remains or the stage deadline must change; that is Phase 4's
decision. Legacy allowed the model 40 min (`improve.py:56`); whether 20 min suffices is measured in P5.

## 4. Structure Choices

### 4.1 Extension language and packaging

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **Go, same module, depguard boundary (chosen)** | one pinned toolchain (`mise.toml`), CI on three OSes for free, single static binaries on `PATH`, replay and fakes share `proctest` | ports ~700 lines of legacy Python logic (observe, the improve core); boundary is a lint rule, not the compiler | high: binaries are found by name; moving to another module or language changes no contract |
| Go, one module per extension | the compiler forbids `internal/` imports | `go.work` or a test loop in `scripts/test`, duplicated dependency updates, no shared test helpers | high |
| Python scripts (port of `observe.py`/`improve.py`) | least rewrite; legacy tests reusable | a second runtime and test toolchain in this repo, `uv` on the launchd `PATH`, no Windows CI coverage of the same code | high |

### 4.2 Where the statusline's source comes from

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **Wrapper writes the aggregate to `last.json` (chosen)** | no core change; one file holds every result; atomic by rename | a dotfiles shell script | high |
| `stack run --out FILE` in core | testable in Go, no shell | a CLI contract change for one caller | medium |
| observe writes a status file | no wrapper | misses improve's and future extensions' results | high |

### 4.3 How improve gets observe's evidence

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **observe writes `latest.json`, improve reads it with a freshness bound (chosen)** | no protocol change; the owner can read the same file; improve can run alone (`--ext improve`) on a recent observation | a file schema between two extensions (versioned) | high |
| improve queries gh itself | no coupling | duplicates the gh source and its rules | high |
| core passes earlier results in the request | single channel | protocol change; core would interpret extension output | low |

### 4.4 Improve target and scope

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **`repo` from the request + `.stack/improve.toml` read from the fetched base (chosen)** | the target repository declares its own editable paths, reviewed and versioned with it; any other cwd is a no-op | the launchd job must start in the checkout | high |
| `~/.config/stack/improve.toml` with repo path and globs | cwd-independent | scope lives outside the repository it governs; unreviewed | high |
| paths hard-coded in `stack-improve` | no config | couples this repository to the dotfiles layout | high |

### 4.5 Replay baseline

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **Decision recorded in the transcript (chosen)** | needs no state; compares what actually happened with what the installed hook decides now, across stack upgrades and the Phase 2 switch | files changed since the call can change a decision (noise, stated) | high |
| Snapshot of the previous run's replay | isolates changes of `stack` itself | state file, and changes are seen only on overlap days | high |

### 4.6 T3 Code source

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **`sqlite3 -readonly -json` command (chosen)** | parity with legacy, no Go dependency; `sqlite3` ships with macOS | depends on a CLI and on T3's internal schema (as legacy did) | high |
| `modernc.org/sqlite` | in-process | large dependency for one query | high |
| drop the source | simplest | most owner sessions run in T3, so `owner-reply` would miss most owner turns | high |

### 4.7 gh model version source

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **`stack --version --json` (chosen)** | the value comes from the installed `stack`, so mismatched builds are still right; no `internal/` import | one additive CLI flag | high |
| import `internal/ghargs` in observe | no core change | breaks the boundary; wrong when `stack` and `stack-observe` are built from different commits | high |

### 4.8 Time budget

Fixed self-budgets inside the 30-min stage (chosen) versus an optional `deadline` request field (allowed by the
protocol's evolution rule, but a core change no Phase 3 extension strictly needs) versus two launchd invocations
(`--ext observe`, `--ext improve`, each with 30 min; bypasses `[stages].cycle`). Revisit with `jev-classify`.

## 5. Dotfiles Instruction Changes

| File | Change | Reason |
|---|---|---|
| `skills/hiden/references/attention.md:3-9` | "注意の予算" becomes "注意の項目": items live in the kit's `attention/items.json`; sizes and counts are shown by `stack budget status` (display only); no caps, no "only goes down", no global register; the steward is called when an audit is needed, without fixed timings, and nothing is recorded in a ledger (`:8`) | §1 no budget ratchets; `dotfiles:docs/kaizen.md` §4 item 2 |
| `skills/hiden/references/retro.md` | deleted (ledger, retrospective, document pass) with its row in `SKILL.md:41` and the lines `orchestration.md:20,40`; "振り返りが規則を決めたとき" and the budget clause leave `SKILL.md:58`; `retrospectives` leaves the description (`SKILL.md:3`) | observation and retrospection are the cycle's (`docs/kaizen.md` §1 principle 4); the ledger's only machine reader was `observe.py:300-321` |
| `skills/hiden/references/commit-pr.md` | a step before push and before `gh pr create`/`edit` in a public repository: `stack check public <base>..HEAD --title-file … --body-file … --branch …` | `phase2-core.md` §4.3 relies on "the commit-pr skill's order" for `--fill`, which the file does not state yet |
| `agents/steward.md:11,13,15` | the review ledger and ledger lines leave the inputs and questions; the rest stays | ledgers retired |
| `executable_statusline.sh.tmpl:151-175` | `comp_cycle` (§3.7) | §4 S3 |
| `AGENTS.md:45` | `writing.md` is `extensions/improve/writing.md` in `shuymn/stack` | §2 layout |
| `AGENTS.md:51` | budget clause removed | §2.1 |
| `home/dot_codex/AGENTS.md.tmpl:4`, `home/dot_pi/agent/AGENTS.md.tmpl:4` | line removed | `.claude/kaizen-managed` retired |
| `docs/kaizen.md` | keeps §1–§5 (the attention design); §6–§8 (cycle, PR plan, rejected options of the cycle) are replaced by a pointer to this document | the cycle is now specified here |

## 6. PR Split

`shuymn/stack` (each passes `scripts/test` and the Windows job):

1. **feat: add version json and the extension import boundary** — `stack --version --json`, the `depguard` rules,
   `docs/architecture.md` §3 line. Done when K1–K2 pass.
2. **feat(ext): add observe** — `extensions/observe` (sources, signals, replay, gh version, `latest.json`). Done when
   O1–O9 pass. Needs PR 1.
3. **feat(ext): add improve** — `extensions/improve` with `writing.md` (caps rules removed). Done when I1–I12 pass.
   Needs PR 2's `latest.json` schema (§3.3).
4. **docs: install and run the cycle extensions** — README install line, `docs/architecture.md` §6 marks Phase 3.

`shuymn/dotfiles` (owner merges each):

1. **refactor: generate stack private terms without public-check** — port `host_values` into
   `stack-private-terms`, move the hand-kept list to `~/.config/stack/private-terms.local`, update the chezmoi script
   inputs. Independent of the stack PRs. Done when D1 passes.
2. **feat: run the cycle through stack** — `.stack/improve.toml`, `scripts/stack-cycle`, `stack-cycle` launchd job
   replacing `kaizen-cycle`, `comp_cycle`, `tests/test_statusline.py`. Preconditions: stack PRs 1–3 merged and
   installed; no open `kaizen/` PR; `apply.py --check` shows no unresolved journal. Done when D2–D3 pass and P1–P6
   hold on the real machine.
3. **docs: update skills for the stack cycle** — §5 instruction changes. After dotfiles PR 2.
4. **chore: retire the kaizen cycle** (last) — delete `scripts/kaizen/**`, `tests/test_kaizen_*.py`,
   `tests/kaizen_fakes.py`, `.claude/kaizen-managed`, `home/dot_claude/hooks/executable_attention-budget.py`,
   `home/dot_claude/attention/create_budget.json` and the global `attention/items.json` (no reader since Phase 2), the
   `scripts/kaizen/**` paths in `automation.yml`; trim `docs/kaizen.md`. After at least one week of daily
   `stack-cycle` runs with P1–P6 holding. Done when P7 holds. Local state under `K`, `~/.local/state/kaizen-apply`
   and `~/.local/state/kaizen-improve` is left for the owner (§9 Q2).

## 7. Test Strategy

- **Unit, table-driven** (`extensions/observe`, `extensions/improve`): signal rules on anonymised transcript excerpts
  and T3 rows in `testdata/`; recorded-decision parsing; stamp state transitions; tree validation; `paths` glob
  matching; config decoding.
- **Fake executables** via `internal/proc/proctest` (test files only): `gh`, `claude`, `sqlite3`, `stack` with
  scenarios and argv/stdin logs. **Real git** via `internal/gittest` for fetch, worktree, commit and push (to a local
  bare remote; commits unsigned via `-c commit.gpgsign=false` in the test repo's config).
- **Replay integration**: the test builds `./cmd/stack` once (`go build` in `TestMain`) and replays a transcript
  excerpt containing a recorded `stack guard (secrets)` deny and a call that ran; asserts `compared 2, changed 0`;
  then a payload whose recorded result `ran` but now denies yields one `replay-change`.
- **Protocol**: each binary is run through `stack run cycle --ext <name>` with fakes on `PATH` in one test per
  extension, asserting a valid response and `status`.
- **dotfiles**: `tests/test_statusline.py` (absent, unreadable, warn with N, stale, pass); `stack-private-terms`
  parity (same terms as the legacy generator for a fixture `host.toml` and term list); `make check` evaluates the
  launchd job.
- **Measure on the real machine**: observe wall time and replay counts (`compared`, `changed`, `unrecoverable`);
  improve's model duration and cost on its first real attempt.

## 8. Acceptance Criteria

### 8.1 Purpose layer

| ID | EARS criterion | Observable result / failure | Verify on the real machine |
|---|---|---|---|
| P1 | When the daily job fires, the system shall run `stack run cycle` from the dotfiles checkout and replace `last.json`. | `last.json` mtime is today; `.stage == "cycle"`; results for `observe` and `improve` | `launchctl kickstart -k gui/$(id -u)/<label>`; `jq '.status, [.results[].name]' ~/.local/state/stack/cycle/last.json` |
| P2 | When observe runs, it shall report the 7-day counts per source and signal and write `latest.json`. | `context` lists every source `ok` or `off`; file has `version: 1` | `stack run cycle --ext observe \| jq -r '.results[0].context'` |
| P3 | When the installed hook decides a recorded call differently from the transcript, observe shall report a `replay-change` finding; otherwise it shall report none for that call. | `replay.compared > 0`; a known recorded `stack guard (secrets)` deny is unchanged | same run; then put an older `stack` build first on `PATH` and see changes listed |
| P4 | While the installed `gh` differs from the guard's modelled version, observe shall warn. | finding `gh-version` with both versions | today: installed v2.104.0 vs model v2.105.0 → warn |
| P5 | When public evidence exists and no improve PR is open, improve shall open at most one PR per ISO week on `improve/<YYYY-Www>` whose changes are limited to `.stack/improve.toml` `paths`, and record the outcome in the week stamp. | one PR; stamp `pr <url>`; a second run that week calls no model | inspect the PR and stamp; rerun `stack run cycle --ext improve` and see "already attempted" |
| P6 | While `last.json` is not `pass` or older than 36 h, the statusline shall show the `cycle` segment; otherwise nothing. | segment text | edit a copy of `last.json`; touch it to an old date |
| P7 | When the retirement PR is merged, no file of the dotfiles repository shall reference the retired kaizen scripts, `attention-budget.py` or `.claude/kaizen-managed`, and the old job shall not be loaded. | grep empty; `launchctl print …kaizen-cycle` fails | `git grep -nE 'scripts/kaizen\|attention-budget\|kaizen-managed\|public-check'` (only `docs/kaizen.md` pointer allowed) |
| P8 | While improve runs, the owner's checkout (working tree, index, branches) shall be unchanged. | `git status` and `git branch` identical before and after; only `origin/<base>` moved | snapshot both before a manual improve run |
| P9 | Where Linux or Windows is the runtime, `stack run cycle` shall not fail because of the cycle extensions. | exit 0; observe `pass`/`warn` with sources `off` | CI protocol test on all runners; `stack run cycle` on a Linux host |
| P10 | When `stack-private-terms` runs, it shall write the same terms as before the port. | file diff empty | regenerate before and after dotfiles PR 1 and `diff` |

### 8.2 Plan layer

| ID | EARS claim | Test | Required assertions |
|---|---|---|---|
| K1 | `stack --version --json` shall print `version` and `gh_model` equal to `ghargs.GhVersion`; `--version` output shall not change. | `cli.TestVersionJSON` | both fields; plain output unchanged |
| K2 | Non-test files under `extensions/` shall not import `internal/`, and `cmd/`, `internal/` shall not import `extensions/`. | `golangci-lint` depguard in `scripts/test` | a scratch violating import fails lint (checked once in the PR) |
| O1 | When a source fails, observe shall mark only that source and still run the others, and answer `warn`. | `observe.TestSourceIsolation` | transcripts unreadable file → `fail` listed; gh still queried |
| O2 | Observe shall count only records in `[since, until)` and only complete lines of regular files. | `observe.TestWindow` | boundary timestamps; unterminated last line skipped; FIFO and symlink listed unreadable |
| O3 | Observe shall emit the six signals by the legacy rules and de-duplicate by id. | `observe.TestSignals` | one positive and one negative case per signal; duplicate `where` once |
| O4 | When `~/.t3` exists, observe shall query T3 with `sqlite3 -readonly -json`, and mark `t3` failed when `sqlite3` is missing. | `observe.TestT3` | fake sqlite3 argv contains `-readonly`; owner, synthetic, unknown tallies; missing tool → `fail` |
| O5 | Observe shall classify recorded decisions as `deny:<rule>`, `other-deny`, `ran` or `unknown` from the matching `tool_result`. | `observe.TestRecordedDecision` | each class; unmatched id → unknown; user refusal → unknown |
| O6 | Observe shall replay only guard-wired tools with existing cwd through the `stack` on `PATH`, once per unique payload, and report each changed decision without the tool input. | `observe.TestReplay` (fake stack), `observe.TestReplayRealHook` (built `stack`) | `Edit` not replayed; missing cwd → unrecoverable; duplicates run once; three change kinds; finding text has no input; cap → truncated |
| O7 | When `gh --version` differs from `gh_model`, observe shall add a `gh-version` finding and answer `warn`; equal versions add none. | `observe.TestGhVersion` | differ, equal, gh missing, old `stack` without `--json` → `unknown` |
| O8 | Observe shall write `latest.json` by temp file and rename with mode 0600 and never answer `block`. | `observe.TestLatestFile` | no partial file after a killed write (temp left, target intact); mode 0600 on Unix; status ∈ {pass, warn} across all scenarios |
| O9 | When its 5-min budget expires, observe shall stop unfinished sources, mark them `timeout` and answer `warn` before core's deadline. | `observe.TestDeadline` (injected clock and budget) | response written; unfinished sources `timeout` |
| I1 | When `repo` is empty or the fetched base has no `.stack/improve.toml`, improve shall answer `pass` and call neither gh nor claude. | `improve.TestNotConfigured` | fake logs empty except `git fetch` |
| I2 | While the week stamp exists, improve shall call no model and answer `pass`, or `warn` for an outcome the owner must act on. | `improve.TestStampExists` | per outcome |
| I3 | When `latest.json` is missing, stale, of an unknown version, has gh not `ok`, or has no public PR/run item, improve shall answer `pass` without creating a stamp. | `improve.TestEvidence` | each case; private and non-URL items excluded from the brief |
| I4 | When an open `improve/` PR exists, improve shall not create a stamp, and shall warn when it is older than 14 days. | `improve.TestOpenPR` | 13 vs 15 days; fork-owned head ignored; gh error → warn |
| I5 | Improve shall claim the week by exclusive creation before the worktree, and two concurrent runs shall produce one model call. | `improve.TestClaim` | two goroutine runs with fakes → one claude invocation |
| I6 | If the model did no billed work or a step before it failed, then improve shall remove the stamp and warn. | `improve.TestUnbilled` | cost 0 + zero tokens; fetch-after-claim failure; worktree failure |
| I7 | Improve shall run `claude -p` with the stated flags, cwd = worktree, the brief, `writing.md` and the task on stdin, and kill it after 20 min. | `improve.TestModelInvocation` | argv exact (no `--restricted`, no `--safe-mode`); stdin contains brief ids and `writing.md`; timeout → `failed:timeout` |
| I8 | If the tree has anything but modifications of existing `100644` files within `paths`, or evidence ids are not ⊆ brief ids, then improve shall record a proposal with the patch and not commit. | `improve.TestProposal` | add, delete, rename, mode change, out-of-scope path, foreign id; no commit object created |
| I9 | Improve shall commit with the repository's hooks and the owner's config, without `--no-verify`. | `improve.TestCommit` (real git, a failing `commit-msg` hook) | hook ran; failure → `failed:commit` |
| I10 | If `stack check public` exits non-zero, then improve shall not push. | `improve.TestPublicCheck` | exit 1 → `proposal:public-check` with hits; exit 2 → `failed:check`; push absent from logs |
| I11 | Improve shall push without force to `improve/<YYYY-Www>` and create one PR with the checked title and body; if PR creation fails, it shall answer `warn` with the finishing command and not retry. | `improve.TestPublish` (bare remote + fake gh) | existing branch → `failed:push`; argv of `gh pr create`; body file bytes equal checked bytes; `pushed-no-pr` |
| I12 | Improve shall remove its worktree on every path after creating it, and a later run shall remove leftovers. | `improve.TestCleanup` | after success, proposal, failure; leftover dir from a killed run removed and pruned |
| D1 | `stack-private-terms` shall write the terms the legacy generator wrote, without importing `public-check`. | `tests/test_stack_private_terms.py` | fixture host.toml with generic, role and owner values; hand-kept list; `.t3` fragments |
| D2 | `comp_cycle` shall render absent, unreadable, warn-with-N, stale and fresh-pass `last.json` as §3.7 states. | `tests/test_statusline.py` | exact segment text per case |
| D3 | `scripts/stack-cycle` shall replace `last.json` atomically and exit with `stack`'s code. | `tests/test_stack_cycle.py` (fake `stack`, fake `chezmoi`) | pass, block (exit 1, file written), exit 2 (empty file) |

## 9. Open Questions for the Owner

- **Q1 (external posting).** Improve drops the Codex HIGH-finding gate that ran before every legacy PR
  (`improve.py:828-855`). The PR still requires public evidence, `stack check public` and the owner's merge.
  Recommended: drop it. Alternative: keep one `codex exec -s read-only` run between steps 10 and 11 (adds a Codex
  run per PR and one more failure path).
- **Q2 (irreversible).** The retirement PR does not delete local legacy state: `~/.claude/kaizen/` (runs,
  proposals, probe results, the hand-kept term list once copied to `~/.config/stack/private-terms.local`),
  `~/.local/state/kaizen-apply/` (backups of live files the swapper replaced) and `~/.local/state/kaizen-improve/`.
  Deleting them is the owner's call after checking that no unresolved journal entry needs a backup.

Decided without the owner (reasons above): $50 per attempt and Opus xhigh unchanged; at most one attempt per ISO
week and one open PR; the probe and `signals.py` deleted (git history keeps them); the T3 source kept via `sqlite3`;
Codex sessions, ledgers and `meta-effort` dropped; `retro.md` deleted.
