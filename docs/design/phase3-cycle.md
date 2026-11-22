# Phase 3 Design: `cycle` Stage (`observe`, `improve`) and Kaizen Retirement

Implementation design for Phase 3 of `docs/architecture.md` §6: replace the legacy kaizen cycle of `shuymn/dotfiles`
with `stack run cycle` and two minimal extensions in this repository, `stack-observe` and `stack-improve` (with
`writing.md`), switch `launchd` to `stack run cycle`, and retire `apply.py`, `ready.py`, the old `improve.py` and
`.claude/kaizen-managed`. It also moves the last callers of `attention-budget.py status` and
`public-check --host-toml` that Phase 2 left in place (`docs/design/phase2-core.md` §7).

Extensions talk to core only through `docs/architecture.md` §3 "Extension Protocol" and the documented CLI. Legacy
sources are cited as `dotfiles:<path>:<line>` at `shuymn/dotfiles` main as of this design (paths under
`scripts/kaizen/` are cited by file name alone); `K` is the legacy state directory `~/.claude/kaizen`.

`docs/design/core-stages.md` (architecture v11) moved improve's public check (§3.1, §3.4 step 12.5, I10) from
`stack check public` to `stack-public check` (same arguments and exit codes; a missing binary is `failed:check`, §6
and C21 there); `gh_model` (§3.2) is unchanged.

## 0. Principles

1. **Stateless observation, one stamp for improvement.** `observe` recomputes a fixed 7-day window on every run and
   keeps no watermark (legacy parity, `cycle.py:2-12`). `improve` keeps one state file per ISO week
   (`docs/architecture.md` §4 S3) and holds one run lock while it runs; there are no attempt directories, journals or
   recovery states.
2. **The owner decides; the cycle reports.** Nothing in the cycle applies a change to the live `~/.claude`, merges,
   or edits the owner's checkout. A change reaches the owner as one pull request; applying a merged change is the
   owner's `make apply` (`docs/architecture.md` §4 S3). Paid runs start only after the owner enables them (§3.4).
3. **No over-defensive machinery** (`docs/architecture.md` §1 drop list): no live swapper, no owner-only merge list,
   no `--restricted` sandbox, no `commit-tree -S`, no recovery state machine, no budget caps. What remains is what
   publishing to a public repository needs: a public target and public evidence only, file tools confined to the
   worktree, a verified single commit, `stack check public`, a read-only Codex check, one open PR at a time, at
   most one model attempt per ISO week (the recorded cost is observed, not capped).
4. **Never block.** The `cycle` stage has no caller to stop. Both extensions answer `pass` or `warn`, never `block`.
   `stack run cycle` then exits 0 when every result is `pass`/`warn`, 1 when another configured extension blocks,
   and 2 when core itself fails (`internal/cli/run.go:60-84`); the wrapper (§3.6) handles each.
5. **macOS is the runtime, every OS compiles.** The extensions are Go in this module, built and tested on all three
   CI runners like core. They read what they find; a missing source on Linux or Windows is a source status, not a
   crash. Scheduling (`launchd`) stays macOS only (`docs/architecture.md` §1).

## 1. Overview

```text
launchd (macOS, daily 06:00)
  └─ scripts/stack-cycle (dotfiles)    cd <dotfiles checkout>; two stages in turn; validate; rename to last.json
       ├─ stack run cycle --ext observe  own 30-min stage
       │    └─ stack-observe            7-day window → ~/.local/state/stack/observe/latest.json + warn findings
       │                                (wrapper checks the result and a fresh latest.json before going on)
       └─ stack run cycle --ext improve  own 30-min stage
            └─ stack-improve            off until ~/.config/stack/improve.toml enables it; run lock; ISO-week stamp;
                                        public evidence → worktree + claude -p → pinned OID → checks + Codex
                                        → push improve/<YYYY-Www> → gh pr create
(jev-classify, Phase 4, is placed by Phase 4; the job names its extensions with --ext, §4.8)
statusline ← ~/.local/state/stack/cycle/last.json        owner ← PR on GitHub, `make apply` after merge
```

## 2. Legacy Inventory

Every behaviour of the legacy cycle, with the v10 decision. "Owner command" rows are commands the owner ran by hand.

| Legacy behaviour (source) | v10 | Why |
|---|---|---|
| Daily launchd job `kaizen-cycle`, 06:00, store copy of `scripts/kaizen`, log file (`dotfiles:nix/home/profiles/dev.nix:78-103`) | **Changed**: job `stack-cycle` runs `scripts/stack-cycle` (§3.6); same time, same log style | §4 S3 seam `launchd -> stack run cycle` |
| One run = apply, observe, watch, ready, weekly, publish; 20-min run deadline, 62 min with the weekly stage (`cycle.py:62,320-359,362-386`) | **Changed**: the job runs `stack run cycle --ext observe`, then `stack run cycle --ext improve`, each under core's own 30-min stage deadline; each extension sets its own absolute deadline before core's (§3.9) | §3 Extension Protocol limits |
| Owner flags `--no-apply`, `--no-gh`, `--no-t3`, `--deadline`, `--home`, tool overrides (`cycle.py:362-386`) | **Dropped**: no live caller passes them (the launchd job passes none, `dev.nix:87-90`); a manual run is `stack run cycle --ext observe` or `--ext improve`; tests put fakes on `PATH` and set `HOME` | the flags served the apply stage and Python tests |
| `KAIZEN_HOME` state-root override (`cycle.py:391`, `statusline.sh.tmpl:154`) | **Dropped**: state lives under `~/.local/state/stack/` (§3.5), derived from `HOME` | tests set `HOME`; no other reader |
| Run date and ledger days in a fixed time zone (`observe.py:49`, `cycle.py:333`) | **Changed**: window is `[now-7d, now)` in UTC; ledger days and the improve week use the local time zone | no private zone constant in a public extension |
| Apply stage: mirror fetch, eligibility, `renameatx_np` swap, journal, backup, cursor `L` (`apply.py:1-1218`, called at `cycle.py:277-293`) | **Dropped**; the owner runs `make apply` after a merge | §1 (no custom live swapper), §4 S3 "Apply via `make apply`" |
| Owner commands `cycle.py adopt`, `resolve`, `apply.py --check` (`cycle.py:16-17,397-410`, `apply.py:1059,1121,1193`) | **Dropped** with the apply stage | same |
| Watch of merged changes, `cycle.py baseline`, `watch.json` (`cycle.py:13-15,138-175`, `watch.json:1-3`) | **Dropped** | §4 S3 retires `watch.json` |
| Regression-unit signal `signals.py` and the probe `scripts/kaizen/probe/` (`signals.py:1-24`, `probe/PREREG.md`) | **Kept** as standalone owner tools, outside the cycle and outside the retirement PR; `probe/build:231` stops importing `public-check` (§2.1, §6 dotfiles PR 2) | they re-measure and re-score past experiments on the current tree; no cycle stage calls them |
| Transcripts source: parent and subagent `*.jsonl`, regular files with mtime ≥ window start (no upper bound), complete lines, per-record window `[since, until)` (`observe.py:84-95,117-201`) | **Kept** (§3.3) | observation is S3's job |
| `owner-reply` signal: owner utterance ≤ 300 chars after an assistant turn, T3 sessions excluded (`observe.py:151-160`) | **Kept** as an unlabelled candidate; classification of corrections is `jev-classify`'s (Phase 4) | §4 Optional Jev |
| `meta-effort` signal: tool calls naming ledgers, `retro.md`, `budget.json`, `attention-budget.py` (`observe.py:39,173-180`) | **Changed**: names `review-ledgers/` and `retro.md` only | the ledger and `retro.md` stay (§5); the budget files are retired |
| `hiden-in-worker`, `reference-unread` signals (`observe.py:175-182,204-207`) | **Kept** (§3.3 states their inputs) | cheap, transcript-only waste signals |
| T3 Code database source, read-only SQLite (`observe.py:213-260,407-408`) | **Changed**: read through the `sqlite3` command-line tool (`-readonly -json`), off when `~/.t3` is absent | the transcripts source skips owner turns of T3 sessions (`observe.py:155-160`), so without it no T3 owner turn is observed (§4.6) |
| Codex sessions source: cwd weights for gh repo selection and a session count (`observe.py:266-297`) | **Changed**: cwd weights kept; the count dropped | a repository worked on only through Codex stays a gh candidate; the count fed only the digest |
| Review-ledger `ledger-miss` source (`observe.py:300-321`) | **Kept**, day boundaries in the local zone | the ledgers stay: `wasure` decides on them and appends to them (`dotfiles:home/dot_claude/skills/wasure/SKILL.md:13,64-66`) |
| gh source: top 10 repos by cwd weight, merged reverts, fix PRs, failed default-branch runs, public provenance; the first gh error ends the source and keeps earlier items (`observe.py:324-378`) | **Kept**, same partial-result rule | only source of public evidence for improve |
| Readiness R1: `chezmoi diff` over the managed list (`ready.py:61-89`) | **Dropped** | needs `.claude/kaizen-managed`; skill drift from session edits is the statusline's `✎` mark (`comp_pending`); drift of `agents/` and settings is applied or shown at the owner's `make apply`, the only path that changes them in v10 |
| R2: launchd job loaded, program paths exist (`ready.py:100-110`) | **Changed**: the statusline shows `cycle never` when the job is installed and no run has finished, and `stale` when `last.json` ages (§3.7) | absence is observable without a check |
| R3: `attention-budget.py status` shows no `OVER` (`ready.py:113-118`) | **Dropped** | §1 no budget ratchets; `stack budget status` is display-only |
| R4: every source read, `gh auth status` passes (`ready.py:121-127`) | **Kept** as observe source statuses and a `gh-auth` finding | observability |
| R5: last run ≤ 3 days, open kaizen PR ≤ 14 days (`ready.py:150-173`) | **Changed**: run age → statusline staleness at 36 h, which is the legacy statusline's own threshold (`statusline.sh.tmpl:162`); PR age → improve `warn` over 14 days, checked on every enabled run (§3.4 step 5) | same signals, no extra check |
| Publish `record.json`, `digest.md`, `status.json` into `K/runs/<id>`, swap `K/current` by rename under `.publish.lock`, prune (`cycle.py:86-121,348-357`) | **Changed**: `scripts/stack-cycle` replaces `~/.local/state/stack/cycle/last.json` only with a validated aggregate and otherwise keeps the previous one and records the failure (§3.6); no per-run history | one aggregate carries every result (§3 Result); no earlier observation is claimed reproducible (transcripts, the live hiden files, the ledgers, gh and the installed hook used by the replay all change), and none is needed: each run reports its own window |
| Japanese digest with decisions list (`cycle.py:178-274`) | **Changed**: each extension's `context` is a short English digest; owner-actionable items are `warn` findings; the statusline count is findings, not fail + decisions | protocol `context`/`findings`; public extension text is English |
| Statusline `kaizen ✗N・判断 N・stale Nd` from `K/current/status.json` (`statusline.sh.tmpl:151-175`) | **Changed**: `cycle` segment from `last.json` (§3.7) | |
| Weekly stage enabled by the file `K/improve-enabled` (`improve.py:175-179`) | **Changed**: enabled only by `enabled = true` in `~/.config/stack/improve.toml` (owner decision, §9 D2); the target repository scopes it with `.stack/improve.toml` (§3.4) | paid runs need an explicit owner switch, not an install |
| `K/.improve.lock` held for the whole weekly stage (`improve.py:182-190,378-387`) | **Kept** as one run lock `~/.local/state/stack/improve/lock`, held from the open-PR check through cleanup and stamp finalisation (§3.4) | the week stamp alone does not serialise runs across a week boundary |
| Attempt directories `K/proposals/<date>-<id>/result.json`, newest-attempt scan (`improve.py:202-332`) | **Changed**: one stamp `~/.local/state/stack/improve/YYYY-Www` holding the outcome | §4 S3 |
| Gate: newest attempt younger than 7 days, `failed:model-unavailable` not counted (`improve.py:53,467-469`) | **Changed**: one attempt per ISO week; the stamp is removed only when the model never started or provably did no billed work | §4 S3 (calendar week; owner accepts a Sunday and a Monday attempt, §9 D2) |
| Gate: uncleared `failed:remote-unknown`, owner `cycle.py clear` (`improve.py:349-366,464-466`) | **Dropped**: an interrupted or ambiguous attempt keeps the week and is reported as "attempt outcome unknown"; the owner deletes the stamp by hand to allow another attempt that week | §1 no recovery state machine |
| Gate: any open `kaizen/` PR of the repo, all pages, head repo full name (`improve.py:473-479`, `ready.py:130-139`) | **Kept**: all pages, head repo full name, prefixes `improve/` and `kaizen/` | one open PR at a time (owner rule, `dotfiles:docs/kaizen.md` §1 principle 6), also across the cutover |
| Public evidence only: public gh items whose `where` is a PR or Actions run URL (`improve.py:64,480-488`) | **Kept**; also the target repository itself must be `PUBLIC` | everything improve publishes must be explained by public material |
| Evidence from any of the owner's top repositories improves the dotfiles instructions (`improve.py:73-87`) | **Kept**: the model judges relevance and returns `change: false` when the evidence does not justify an instruction change; the Codex check asks whether case-specific nouns read as general rules | the instructions serve work in every repository |
| No public evidence: write `proposal.md`, no model, week used (`improve.py:490-499`) | **Changed**: `pass` with context, no stamp, retried the next day at no model cost | a free retry beats waiting a week |
| GitHub source not `ok` → retry tomorrow, but public items already read are used (`improve.py:480-488`) | **Kept**: public items are used whatever the gh status; a non-`ok` gh source only changes the "no public evidence" context | an unread source is not evidence of absence; read items are |
| Preflight: `host.toml`, `private-terms`, signing key, `claude auth status` (`improve.py:502-515,533-548`) | **Changed**: no preflight; a step failing before the model starts removes the stamp and warns. A signing failure surfaces at commit, after billing, as `failed:commit` with the patch kept | the owner's every commit uses the same signer, so a broken one is noticed in daily work; a preflight duplicates it |
| Minimal environment, endpoint env copied from settings because `--restricted` ignores them; credential redaction and leak check (`improve.py:257-281,517-531,605-606`) | **Changed**: `claude -p` loads only the owner's user settings source (endpoint included), auto memory off; file tools confined to the worktree by permission settings; endpoint-credential leak check and redaction **kept**, and extended to the pinned commit, title and body (§3.4 steps 9, 12) | §1 drops `--restricted`; the leak check is an independent guarantee |
| Bare mirror of the public URL, `git archive` temp tree (`improve.py:554-555,596-598`) | **Changed**: fetch of the default branch into `refs/remotes/origin/<default>` in the target checkout, then a detached `git worktree add` of the pinned commit | §4 S3 "plain worktree" |
| `claude -p` with `--safe-mode --restricted --strict-mcp-config`, tools `Read,Edit,Write,Grep,Glob`, JSON schema, Opus xhigh, `--max-budget-usd 50`, 40-min timeout (`improve.py:56-60,633-658`) | **Changed**: same tools, schema, model and effort; no `--max-budget-usd` (owner decision: the run uses the subscription's usage allowance, so the cost is observed from the stamp rather than capped); `--safe-mode` and `--restricted` dropped (the owner's user hooks, including `stack hook claude pre-tool-use`, apply); `--setting-sources user` and `--settings` block project and local settings, auto memory, and file-tool reads and writes outside the worktree (Claude Code ≥ 2.1.314); timeout from the deadline (§3.9) | §1; §3.9 time budget |
| Model `summary` and `watch` ("what to watch after the merge") shown in the digest (`improve.py:88-92`, `cycle.py:242-243`) | **Kept** in the schema; stored in the stamp and shown in `context` | the owner reads them with the PR |
| Diff kept whatever follows the model (`improve.py:600-604`) | **Kept**: every outcome after the model that does not publish stores the redacted patch in the stamp | the owner can finish or discard the change |
| Result checks: schema, success subtype, cited evidence ids ⊆ brief ids (`improve.py:610-611,650-658`) | **Kept** | |
| Tree check: only modifications of existing `100644` files (`improve.py:709-743`) | **Kept** on the worktree before commit and on the committed OID after it, plus: every changed path matches `.stack/improve.toml` `paths` | publishable scope; hooks may change the index |
| Signed commit by `commit-tree -S` with the key path from a private config (`improve.py:760-772`) | **Changed**: plain `git commit -F` in the worktree; the owner's git config signs as it does for every commit; hooks run | §1 drops `commit-tree -S` |
| Checks: one commit, `apply.assess`, prose caps, relative links (`improve.py:774-792,807-826,745-758`) | **Changed**: one commit on the pinned base kept; `assess` and caps dropped; broken relative links are left to the Codex check, whose prompt names them (`improve.py:96-98`) | assess served apply; caps are §1-dropped |
| `public-check --host-toml --private-terms` before push and in recovery (`improve.py:794-805,965`) | **Changed**: `stack check public <base_oid>..<oid> --title-file --body-file --branch` | Phase 2 core (§4.5); host values live in `private-terms` |
| Codex `exec -s read-only` HIGH-finding gate on the frozen candidate (`improve.py:828-855`) | **Kept** (owner decision, §9 D1): runs on the pinned OID's diff and the fixed title and body; any HIGH, failure, timeout or invalid verdict stops publication | a HIGH candidate must not become public before review; a merge-time review cannot undo publication |
| Push with lease to a new `kaizen/improve-<id>` branch, `gh pr create`, lookup and adopt, recovery create (`improve.py:863-971`) | **Changed**: push of exactly the pinned OID, and nothing else, to a new `improve/<YYYY-Www>` with a lease expecting the branch absent, confirmed as a new ref; one `gh pr create`, recorded as a PR only after a read matches head repo, ref, OID and base; an ambiguous result is reported from read-only queries, never resent | §1 no recovery state machine |
| Cost recorded or `unknown`, Codex run count (`cycle.py:228-229,271-273`, `improve.py:143-150,587`) | **Kept** in the stamp and `context`; an `unknown` cost also adds a `cost-unknown` finding | an abnormal run must be visible |
| `writing.md` given to the model from the base commit (`improve.py:62,556,599`; `writing.md:1-9`) | **Changed**: `extensions/improve/writing.md` embedded in `stack-improve`; the caps rules (`writing.md:3,7`) are removed | §2 layout; §1 no budget ratchets |
| Managed-path list `.claude/kaizen-managed` and its mentions in the Codex and pi instructions (`kaizen-managed:1-7`, `home/dot_codex/AGENTS.md.tmpl:4`, `home/dot_pi/agent/AGENTS.md.tmpl:4`) | **Dropped** | §1 (owner-only merge list) |
| Kaizen tests and fakes (`tests/test_kaizen_*.py`, `tests/kaizen_fakes.py`); CI paths (`.github/workflows/automation.yml:3-24`) | **Changed**: tests of retired code are deleted in the last PR, after the guarantees in §7.1 are ported; probe and signals tests stay; the statusline tests in `test_kaizen_cycle.py:771-` move to `tests/test_statusline.py` | |

### 2.1 Callers left by Phase 2

`attention-budget.py status` (`dotfiles:home/dot_claude/hooks/executable_attention-budget.py:158-173`):

| Caller | Migration |
|---|---|
| R3, `ready.py:113-118` (from `cycle.py:337`) | deleted with `ready.py` |
| `improve.py:807-826` executes the hook source for prose caps | deleted with `improve.py` |
| `dotfiles:AGENTS.md:51` validation row ("shows no `OVER`") | the budget clause is removed; `stack budget status` may be run to see sizes, with no pass/fail (v11 removes the command, `core-stages.md` §5.3) |
| `hiden/references/attention.md:5` (usage command) and `:6-7` (caps that only go down) | rewritten around `stack budget status`, display-only, no caps (§5); v11 removes that sentence with the command (`core-stages.md` §5.3) |
| `hiden/SKILL.md:58` (new rules pass the attention budget first) | clause removed (§5) |
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
| `scripts/kaizen/probe/build:231-258` loads it for `host_values`, `private_terms` and `scan`; `scan` both selects filler (`:242-243`, via `clean`) and re-checks every built file (`:257-258`) | `host_values` from `stack-private-terms`; terms from the generated `~/.config/stack/private-terms`; `scan` and its local-path pattern (`public-check:32-33,111-115`) are copied into `probe/build`, and both uses call that copy unchanged. Not `stack check public`: it scans a Git range plus title and body files, not arbitrary text, and the dotfiles CI has no `stack`. The existing build test (`tests/test_kaizen_probe.py:205-`) gains a dummy private term in a test-`HOME` `private-terms` file and a filler item holding it; it must pass, with that item excluded and no hit in the built files, after `public-check` is deleted (D1) |
| `tests/test_kaizen_public_check.py` | deleted; its vectors are ported in `internal/publictext` and `internal/checkpub` (Phase 2 PR 3) |

## 3. Contracts

### 3.1 Layout and language

```text
extensions/
  observe/                 package observe (sources, signals, replay, gh version, latest.json)
    cmd/stack-observe/     main: protocol I/O only
    testdata/              anonymised real-format transcript excerpts, T3 rows, gh answers
  improve/                 package improve (lock, stamp, evidence, worktree, model, checks, Codex, publish)
    cmd/stack-improve/     main
    writing.md             embedded with go:embed
```

**Go, in this module.** Both binaries build with `go install ./extensions/observe/cmd/stack-observe
./extensions/improve/cmd/stack-improve` (with `GOBIN` set to the directory that holds `stack`), so the installed
names match core's `stack-<name>` discovery. They are `package main` + library like `cmd/stack` + `internal/`, run
under `scripts/test` and the Windows CI job, and need no runtime besides the tools they call (`stack`,
`stack-public`, `git`, `gh`, `claude`, `codex`, `sqlite3`). Boundary: `.golangci.yaml` enables `depguard` with two rules: non-test files under
`extensions/` may not import `github.com/shuymn/stack/internal/...`, and nothing under `cmd/` or `internal/` may
import `github.com/shuymn/stack/extensions/...`. Extension tests may import `internal/proc/proctest` and
`internal/gittest` (test infrastructure only). The run lock uses `github.com/gofrs/flock`, already a module
dependency. §4.1 compares the alternatives.

Both read the protocol request from stdin (ignoring `range` and `prompt`), write one response to stdout, write
progress to stderr (core prefixes it), and treat any internal error as `warn` with a finding. When core kills an
extension at the stage deadline, core itself answers a synthetic `warn` for it (`docs/architecture.md` §3 Limits);
the extensions' own deadlines (§3.9) exist so that this does not happen.

**Child processes.** Every tool an extension starts gets a timeout from the extension's deadline. On macOS and Linux
the child starts in its own process group (`SysProcAttr.Setpgid`) and the whole group is killed at the timeout, so
grandchildren (hooks, `git` helpers, `gh` pagers) do not outlive it; after the direct child exits on its own, the
extension also kills what is left of its group, so a grandchild whose parent exited first does not survive either.
On Windows only the direct child is killed; no Phase 3 extension runs a paid or publishing step there (§3.4 step 0).

These guarantees hold for the extension's own timeouts. When the extension itself is killed from outside (a
`SIGKILL`, core's stage deadline, a shutdown), nothing kills its children's groups: they can outlive it, the run lock
(§3.4 step 2) is released with the extension, and the next run that reaches §3.4 step 8 reports the leftover
worktree instead of removing it. Each extension's own deadline comes before core's (§3.9), so that path is abnormal.

Finding shape for both: `{"kind": "<slug>", "where": "<file:line | URL | path>", "detail": "<≤ 300 chars>"}`.

### 3.2 Core addition: `stack --version --json`

Prints `{"version":"<version>","gh_model":"<ghargs.GhVersion>"}` and exits 0; `stack --version` is unchanged.
`stack-observe` needs the `gh` release the guard's argument model was written against (§3.3 gh version) and must not
import `internal/ghargs`. A `stack` built before this change ignores the trailing `--json`, prints its plain version
and exits 0 (`internal/cli/cli.go:154-156`); observe treats output that is not this JSON object as `unknown`. This
is an additive CLI change (`docs/architecture.md` §3 gains it in the same PR) and the only core change of
Phase 3.

### 3.3 `stack-observe`

Input: request (only `stage` is read). Window `[since, until) = [now − 7 d, now)` in UTC. Deadline: an absolute
monotonic deadline 5 min after start, with the last 20 s reserved for writing `latest.json` and the response (§3.9).
A source not finished by the deadline minus the reserve is `status: "timeout"` and the response is `warn`.

**Sources** (run in this order; each independent; a failure marks that source and the others run):

| Source | Reads | Status |
|---|---|---|
| `transcripts` | `~/.claude/projects/*/*.jsonl` and `…/subagents/**/*.jsonl` that are regular files with mtime ≥ `since` (no upper bound: a file appended during the scan keeps its in-window records); complete lines only, each at most 16 MiB (a longer line is skipped and counted); a malformed line is skipped; each record counts only when its own `timestamp` is in `[since, until)` (legacy `observe.py:84-95,117-201`) | `ok`, `fail` (any unreadable file, listed, or a root that is not a directory), `off` when the directory is absent |
| `t3` | `sqlite3 -readonly -json <T3 state database> <query>` (the file legacy opens, `observe.py:422`) with the legacy queries (`observe.py:229-258`); window literals formatted by observe in the legacy format | `off` when `~/.t3` is absent; `fail` when `sqlite3` is missing or errors (non-zero exit, or a diagnostic on stderr at exit 0) |
| `codex` | `~/.codex/sessions/**/rollout-*.jsonl` with mtime ≥ `since`, first line only; session start in the window adds its cwd weight (`observe.py:266-297`) | `ok`, `fail`, `off` when absent |
| `ledgers` | `~/.claude/review-ledgers/*.md` lines starting `- YYYY-MM-DD` that mention a miss (`observe.py:300-321`); the date's local-zone start must be in the window | `ok`, `fail`, `off` when absent |
| `gh` | for the top 10 repos by transcript and Codex cwd weight: `gh repo view`, merged PRs, failed default-branch runs (`observe.py:342-378`); `gh auth status` | `ok`; `fail` at the first gh error (non-zero exit, or empty or wrongly shaped JSON), keeping the items of repos already read (legacy rule, `observe.py:335-339`) and recording `repos_read`; a failing `gh auth status` adds finding `gh-auth` |
| `replay` | §3.3 replay | `ok`, `fail`, `timeout`, `off` when `stack` is not on `PATH` |
| `gh-version` | §3.3 gh version | finding only |

**Owner tally** (context and `latest.json` `tally`, legacy `observe.py:151-168,233-258,407-408`): `owner` counts
typed human utterances of non-T3 parent transcripts and T3 rows that are user-created, not compaction, from
`web`/`mobile` and in a root thread; other T3 user rows count `synthetic` (created by the system or compaction) or
`unknown` (other source or non-root thread). When `t3` is not `ok`, each in-window T3 session seen in the
transcripts (entrypoint `t3-code`) adds one to `unknown`, so unread owner turns are visible as unknown, not absent.

**Signals** (items, de-duplicated by id = first 12 hex of SHA-1 of `signal\0where`, `observe.py:73-76`):
`owner-reply`, `revert`, `fix-pr`, `ci-fail-main`, `ledger-miss`, `meta-effort`, `hiden-in-worker`,
`reference-unread`, with the legacy matching rules cited above. `reference-unread` reads the live
`~/.claude/skills/hiden/references/*.md` and reports each file that no in-window `Read` call names
(`skills/hiden/references/<name>.md` in `file_path`); it is emitted only when `transcripts` is `ok`, with `ts =
until` (`observe.py:204-207,407`). Each item: `id, signal, kind (miss|waste), where, excerpt (≤ 160 chars),
provenance (owner|public|synthetic), repo, ts`.

**Replay of PreToolUse decisions** (owner-approved addition). For every assistant `tool_use` block in a window
transcript whose name is one the guard is wired for (`Bash`, `PowerShell`, `Read`, `NotebookRead`, `Grep`, `Glob`,
`mcp__pencil__execute`; `phase2-core.md` §4.3 wiring):

1. *Recorded decision*, from the `tool_result` with the same `tool_use_id`: content starting
   `PreToolUse:<tool> hook error: stack guard (<rule>)` → `deny:<rule>`; any other `PreToolUse:<tool> hook error:`
   (an older hook, another hook, or several hooks) → `other-deny`, and so is a `<rule>` that is not a guard rule id
   (lowercase letters, digits, `-`); a result that is not a hook error → `no-deny`; no result, a result whose
   content is missing, null or not text, or a user permission refusal → `unknown`. Only `deny:<rule>` and `no-deny` are compared. The exact strings
   are taken from anonymised real transcripts in `testdata/` (§7), since the hook output specification does not fix
   how a transcript records them.
2. *Replayed decision*: payload `{"session_id","transcript_path","cwd","hook_event_name":"PreToolUse","tool_name",
   "tool_input"}` from the record, piped to `stack hook claude pre-tool-use` (the `stack` first on `PATH`, i.e. the
   installed hook) with `STACK_DEBUG=1`, 5 s timeout each (the hook's settings timeout), 4 at a time. Exit 0 with a
   deny JSON naming `stack guard (<rule>)` → `deny:<rule>`; exit 0 with empty stdout and no `stack hook:` line on
   stderr → `no-deny` (the hook denied nothing; it does not say the input was understood). A start failure, timeout,
   non-zero exit, unparsable stdout, a rule that is not a guard rule id, stderr not read in full (past its cap),
   or a `stack hook:` stderr line (the hook failed open, `internal/hookio/hookio.go:104-115`) → `not-comparable`.
   A record whose `cwd` no longer exists, or whose `tool_input` lacks the input the guard needs for that tool, is
   `unrecoverable`: not run and excluded from comparison. The guard returns without a decision or diagnostic when it
   cannot decode the fields it reads or the field it judges is empty (`internal/guard/guard.go:48-60`,
   `secrets.go`, `pencil.go`), so a `Bash` or `PowerShell` call needs a non-empty string `command`, `Read` a `file_path`,
   `NotebookRead` a `notebook_path` or `file_path`, `Glob` a `pattern`, `mcp__pencil__execute` an `input` or
   `edits`, and every file tool string path fields. Identical `(cwd, tool_name, tool_input)` payloads run once, and
   that one replayed decision is compared with each recorded decision separately. A payload's input is re-read
   from its transcript line when it runs; an input that no longer matches the scanned digest is `not-comparable`.
   Payloads run newest first (full timestamp precision) until 100,000 have run (`truncated_cap` counts the records
   of the rest, not unique payloads) or the deadline minus the reserve passes (`truncated_time` counts the records
   of the rest); 5,000 covered ~7% of a real 7-day window in 8.7 s, and the time bound
   is the real limit.
3. *Reported change* (finding `replay-change`, at most 20 listed, all counted in `replay.changed` and per kind in
   `replay.changed_by_kind`): `no-deny → deny:<rule>` (`new_deny`: a false positive or a newly guarded call),
   `deny:<rule> → no-deny` (`lost_deny`), `deny:<a> → deny:<b>` (`rule_changed`).
   `where` is `transcript:line`; `detail` holds only the tool name and the two decisions (rule ids). Deny reasons
   are never copied: they quote input paths and globs (`internal/guard/ghpublish.go:20-30`).

The replay reads only: the hook itself only reads files (body files, symlink targets). A changed decision can also
come from a file that changed since the call (a body file, a `.git` directory), so a finding is a candidate for the
owner to read, not proof of a regression. Replay findings are never public evidence.

**gh version** (owner-approved addition). `gh --version` first line `gh version X.Y.Z …` → `vX.Y.Z`;
`stack --version --json` → `gh_model`. When they differ, finding `gh-version` with both versions and the advice to
check `internal/ghargs` against the installed `gh` (a flag the table lacks makes the guard scan the rest of a call
as literal text, `phase2-core.md` §4.3). `gh` missing, or a `stack` whose `--version --json` output is not the §3.2
object, is finding `gh-version` with `unknown`.

**Response.** `status`: `warn` when any source is `fail`/`timeout` or any `gh-auth`, `replay-change` or
`gh-version` finding exists; else `pass`. `context`: ≤ 4,000 chars, English: window, per-source status and counts,
owner tally, per-signal counts, replay totals (`compared`, `changed` with its three kinds, `not_comparable`,
`unrecoverable`, `truncated_cap`, `truncated_time`) and installed vs modelled gh come first; up to 3 excerpts per
signal fill the rest. `findings`: as above. An error in `findings`, `context` or `latest.json` is a fixed summary of
its cause (`gh auth failed: exit 1`, `gh repo view failed: exit 1`, `sqlite3 failed: error on stderr`), never a
child's stderr, which can quote local paths and inputs.

**File** `~/.local/state/stack/observe/latest.json` (0600, temp file in the same directory + rename, so readers
never see a partial file):

```json
{"version": 1, "window": {"since": "…Z", "until": "…Z"},
 "sources": {"transcripts": {"status": "ok", "files": 0, "records": 0, "error": ""}, "t3": {}, "codex": {},
             "ledgers": {}, "gh": {"status": "ok", "repos_read": 0}, "replay": {}},
 "tally": {"owner": 0, "synthetic": 0, "unknown": 0},
 "items": [{"id": "…", "signal": "revert", "kind": "miss", "where": "https://github.com/o/r/pull/1",
            "excerpt": "…", "provenance": "public", "repo": "o/r", "ts": "…Z"}],
 "replay": {"compared": 0, "changed": 0, "not_comparable": 0, "unrecoverable": 0, "truncated_cap": 0,
            "truncated_time": 0, "changed_by_kind": {"new_deny": 0, "lost_deny": 0, "rule_changed": 0}},
 "gh": {"installed": "v…", "model": "v…"}}
```

The deadline is checked again right before the write starts; past it, observe answers `latest-not-written`
without writing. A local write already started (temp file, sync, rename) runs to completion: it is not
interruptible, and the 20-s reserve covers it. `version` is this file's schema; timestamps are RFC 3339 UTC in whole seconds (`until` is the start time cut to the
second), so `jq`'s `fromdateiso8601` reads them (§3.6). Last writer wins: two overlapping observe runs (a manual one
and the job) can leave the older window as the file; improve's freshness bound (§3.4 step 6) still holds, so this is
accepted. When writing `latest.json` fails, observe answers `warn` with the finding `latest-not-written`, and the
job (§3.6) does not run improve: a file from an earlier run in the same second would otherwise pass its freshness
check.

### 3.4 `stack-improve`

Input: request `repo` (the Git top-level of the cwd; `scripts/stack-cycle` runs from the dotfiles checkout).
Deadline: an absolute monotonic deadline 29 min after start (before core's 30-min stage deadline, since the job runs
improve as its own stage, §3.6), with 60 s reserved for cleanup and the response; every
step's timeout is cut to what the deadline leaves after the reserve and the later mandatory steps (§3.9). Order:

0. **Enabled.** Read `~/.config/stack/improve.toml` (extension config, not core config):
   ```toml
   enabled = true      # required for any gh, git or model call; absent file or any other value = off
   ```
   Off → `pass` "improve disabled" with nothing else called. Invalid TOML → `warn`. On Windows → `pass` "improve
   runs on macOS and Linux" (no process-group kill, §3.1).
1. **Target.** `repo == ""` → `pass` "not in a repository". `remote.origin.url` must be set exactly once (with
   several, fetch uses the first and push every one) and be `https://github.com/<owner>/<name>`,
   `ssh://git@github.com/<owner>/<name>` or `git@github.com:<owner>/<name>`, `.git` optional; any other host,
   including one that only ends in `github.com` (which `observe.py:41` accepted), is not GitHub. Every `gh` call names github.com, whatever `GH_HOST` or gh's default host says (`gh repo
   view github.com/<slug>`, `gh pr create --repo github.com/<slug>`, `gh api --hostname github.com`). `gh repo view
   github.com/<slug> --json nameWithOwner,visibility,defaultBranchRef`: `visibility` must be
   `PUBLIC` and `nameWithOwner` equal to `<slug>` (case-insensitive), else `pass` "target is not a public GitHub
   repository" (gh error → `warn`); with `origin` not one GitHub URL the text names that. `<default>` =
   `defaultBranchRef.name`; there is no `base` setting. `~/.claude/settings.json` is read before improve's first
   child, so its credential values (step 9) mask every stderr line; an unreadable file stops here. If
   `~/.claude/settings.json` lists `permissions.additionalDirectories`, `warn` and stop: they would widen the
   file-tool boundary of step 9. Only the standard configuration directory is supported: improve removes
   `CLAUDE_CONFIG_DIR` from the environment of every `claude` it starts, so the settings it checks here and in the
   credential guard are the ones Claude loads.
2. **Run lock.** Non-blocking exclusive lock on `~/.local/state/stack/improve/lock` (`gofrs/flock`; released by the
   kernel if the process dies, so it never goes stale). Busy → `pass` "another improve run is active". The lock is
   held until step 13 has finished, i.e. through cleanup and stamp finalisation. It covers manual and scheduled runs
   on the same machine; runs on two machines are not serialised (one stamp per machine), which the open-PR gate
   catches once the first run has published.
3. **Base and config.** `git fetch --no-tags --no-write-fetch-head --no-recurse-submodules --refmap= --no-prune
   --no-prune-tags origin +refs/heads/<default>:refs/remotes/origin/<default>` (no `FETCH_HEAD` race, no tag
   updates; `--refmap=` keeps a configured `remote.origin.fetch` from updating other refs, and the two `--no-prune`
   flags override `fetch.prune` and `fetch.pruneTags`), then `base_oid =
   git rev-parse --verify refs/remotes/origin/<default>^{commit}` once; every later step uses `base_oid`, never the
   ref. Config from the pinned commit, `git show <base_oid>:.stack/improve.toml`, so only merged config counts:
   ```toml
   paths = ["home/dot_claude/skills/**", "home/dot_claude/agents/**"]   # required; ** = any directories
   ```
   `**` at the end of a pattern matches at least one segment (`skills/**` never matches a file named `skills`); in
   the middle it matches zero or more. Absent → `pass` "`<repo>` has no .stack/improve.toml", read with `git ls-tree
   <base_oid> -- .stack/improve.toml`, so a failed `git show` of a file that exists is `warn`, not absent. Invalid →
   `warn`. Fetch failure → `warn`.
4. **Open PR gate.** `gh api --paginate --slurp 'repos/<slug>/pulls?state=open&per_page=100'` (every page, as
   `ready.py:130-139`); a PR counts when its head ref starts with `improve/` or `kaizen/` and its
   `head.repo.full_name` equals `<slug>` (case-insensitive; a fork's branch does not count). Any → `pass` naming
   it, or `warn` when it is older than 14 days. gh error or a malformed answer → `warn`.
5. **Week stamp.** `W` = ISO week of the local date, file `~/.local/state/stack/improve/<YYYY>-W<ww>` (e.g.
   `2026-W47`). Any existing stamp means the week is used. A stamp with a final outcome → `pass` stating it, or
   `warn` when the owner must act (`pushed-no-pr`, `publish-unknown`, `proposal:*`, `failed:*`, `cost-unknown`).
   An empty, unparsable or unfinished (`started`) stamp → `warn` "attempt outcome unknown" with whatever fields it
   holds (OID, branch, title, body when step 12.7 recorded them). The owner may delete the stamp by hand to allow
   another attempt this week; nothing deletes it automatically except step 10.
6. **Evidence.** Read `~/.local/state/stack/observe/latest.json`: missing → `pass` "no observation"; `window.until`
   older than 24 h → `pass` "stale observation" (no stamp). Unparsable, unknown `version`, or `window.until` more
   than 5 min in the future → `warn` with a diagnostic (no stamp). Public items = `provenance == "public"` and
   `where` matches `^https://github\.com/[\w.-]+/[\w.-]+/(pull|actions/runs)/\d+$` (`improve.py:64`), whatever
   `sources.gh.status` is. None → `pass` "no public evidence" (no stamp), naming the gh status when it is not `ok`.
   If less time is left than the model minimum (§3.9) → `pass` "not enough time" (no stamp).
7. **Claim the week.** Create the stamp with `O_CREATE|O_EXCL` (0600) and write `{"week","started","repo",
   "base_oid","outcome":"started"}`. From here every exit rewrites the stamp (temp + rename) with its outcome and
   `finished`.
8. **Worktree.** Directories already under `~/.local/state/stack/improve/work/` are leftovers of an abnormal end
   (§3.1), possibly still in use by a surviving child: they are not removed; each adds a `leftover-worktree` finding
   (`warn`) naming its path, for the owner to remove once nothing uses it. Then `git worktree prune` (drops only the
   metadata of worktrees whose directory is gone) and `git worktree add --detach <work>/<W> <base_oid>`; a failure,
   including an existing `<work>/<W>`, is `failed:worktree` before the model; any existing path there, even an
   empty directory Git would accept, is refused before the add and left untouched. A `<work>/<W>` created by the
   add is this run's even when the add then fails (Git registers the worktree before a failing
   `post-checkout` hook), so step 13 removes it. The owner's working tree, index and
   branches are never touched; the fetch updates only `refs/remotes/origin/<default>`. Right before starting
   `claude`, the model minimum (§3.9) is checked again; too little time left → `failed:deadline`, the model never
   started (step 10 removes the stamp).
9. **Model.** `claude -p --setting-sources user --strict-mcp-config --tools Read,Edit,Write,Grep,Glob
   --permission-mode acceptEdits --permission-prompts none --no-session-persistence --settings '<S>' --output-format
   json --json-schema <schema> --model claude-opus-5-5 --effort xhigh`, cwd = the worktree, no
   `--add-dir`, environment inherited minus `CLAUDE_CONFIG_DIR` (step 1) plus `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, timeout from §3.9. `<S>` =
   `{"autoMemoryEnabled":false,"permissions":{"blockReadsOutsideWorkingDirectories":true,
   "blockWritesOutsideWorkingDirectories":true}}`: the file tools refuse paths outside the working directory in
   every permission mode (Claude Code permissions, "Block file-tool reads and writes outside the working
   directories", https://code.claude.com/docs/en/permissions). Prompt on stdin: the evidence brief (`id | signal |
   URL | excerpt` of public items only, `improve.py:589-593`), the embedded `writing.md`, and the task
   (`improve.py:73-87` with the allowed `paths` from the config instead of the fixed directories). Schema as
   `improve.py:88-92` (`change, evidence_ids, summary, watch, commit_message, pr_title, pr_body`).

   *Settings sources.* `--setting-sources user` (https://code.claude.com/docs/en/cli-reference) loads no `project` or
   `local` source. That matters because a linked worktree's `.claude/settings.local.json` is read from the main
   checkout's root (permissions, "Where output styles, hooks, and local settings load from"), so its
   `additionalDirectories` would widen the file-tool boundary, and project settings could add hooks or allow rules.
   Auto memory is off twice (the variable and the `--settings` key, https://code.claude.com/docs/en/memory "Enable or
   disable auto memory"): it lives in one machine-local directory shared by every worktree of the repository and is
   loaded at start. The first approved real run verifies both (P11).

   *What remains* (accepted, stated): from the user source, the endpoint `env` with its auth token and the login
   (needed to call the model at all), the user hooks including `stack hook claude pre-tool-use` (a hard safety
   boundary, `docs/architecture.md` §1), and the user permission rules (only `additionalDirectories` would widen
   them, and step 1 stops on it); the user instructions `~/.claude/CLAUDE.md`, rendered from the public dotfiles
   template, so nothing private reaches the model through them; user skills and agents load but are unusable
   without the `Skill` and `Agent` tools; managed settings, if any, always apply. From the worktree, the checked-in
   `CLAUDE.md`/`AGENTS.md` of the pinned base may still load (public content either way). The files Claude Code
   itself needs outside the worktree are exempt from the block. No MCP server (`--strict-mcp-config` with none
   given), no Bash tool, no extra directory.

   *Credential guard* (kept from `improve.py:517-531,605-606`): the credential set is every value ≥ 8 chars of
   `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` in improve's environment and in the
   `env` of `~/.claude/settings.json`. If any appears in the worktree diff, any string of the structured output or
   an untracked file (read whole, one file at a time, in chunks that overlap by a credential's length), the outcome
   is `proposal:credential` before any commit; an untracked file it cannot read leaves the check incomplete and
   refuses the same way, as does a scan not finished before the reserve (checked between chunks). Step 12.4 checks the published bytes again. Every value
   is replaced by `[redacted]` in everything improve writes: stamp (patch, reason, hits), stderr (including the
   passed-through stderr of `claude`, `git`, `codex`, `gh`), `context` and findings. A value that appears only in a
   child's stderr is redacted and changes no outcome: stderr is never published.

10. **Billing.** The result is complete only when stdout read to EOF is exactly one `type: result` JSON object and
    the child exited on its own: trailing bytes, cut output, a capture failure, a timeout or a signal make it
    incomplete. Cost = `total_cost_usd` when the result is complete, the cost finite and `usage` valid (below), else
    `unknown`. The stamp is
    **removed** (after the child has exited and step 13 has run, still under the lock) only when the `claude` child
    never started (a start failure is `failed:model-start`, or a failure in steps 7-8), or when a complete result
    proves zero cost:
    `total_cost_usd == 0` and `usage` an object holding each of `input_tokens`, `output_tokens`,
    `cache_creation_input_tokens` and `cache_read_input_tokens` as the integer 0 (stricter than legacy, which took
    absent counters as none, `improve.py:296-303,653-656`). Every other case keeps the stamp: a missing or
    unparsable response, `usage` empty (`{}`) or missing a counter, a counter that is not a non-negative integer, a
    non-zero counter, a timeout, a kill; with `usage` incomplete or invalid the cost is `unknown`. `unknown` cost
    adds finding `cost-unknown` and makes the response `warn`.
11. **Result checks.** Not `success`, schema mismatch or timeout → outcome `failed:model-result` /
    `failed:timeout`, `warn`. `change: false` → `no-change`, `pass`. Evidence ids empty or not ⊆ brief ids →
    `proposal:evidence-ids`. `git status --porcelain=v2 -z` must show only modifications of existing `100644` files
    that match `paths` (no add, delete, rename, mode or type change) → else `proposal:tree` (an early failure; step
    12 re-checks the commit). Blank or multi-line title, blank message or body → `proposal:metadata`. Every
    outcome after the model that does not publish stores the redacted `git diff` (≤ 256 KiB, then cut with a
    marker) in the stamp's `patch` field; `proposal:*` and `failed:*` answer `warn`, and each outcome of this run
    that answers `warn` also adds an `improve-outcome` finding with its reason.
12. **Commit, pin, verify, check, publish.** Every `git` improve starts, and `stack-public check`, runs with
    `GIT_NO_REPLACE_OBJECTS=1`, so a replace ref cannot show the checks other objects than the push sends.
    1. *Commit.* Body = model body + `Evidence:` list of the cited URLs (`improve.py:617-621`); title and body are
       written to files once, after the commit and its hooks (step 12.2), and never rebuilt. The model and every
       `git -C` on the candidate (commit, verify, checks, push) run only in this run's worktree, while the
       repository-level operations (fetch, config read, `worktree add`/`remove`, `ls-remote`) run in the target
       checkout; a call without a directory never starts. The files improve itself writes in its private run
       directory (message, title, body, verdict schema) are created new (`O_CREATE|O_EXCL`), so an existing path
       there, a symlink included, is refused rather than written through; that directory is not otherwise
       defended against the owner's hooks or the `stack` binary deliberately replacing its files. `git -C <worktree> commit -a -F <msg>` with the owner's git
       configuration (signing included) and the repository's hooks; never `--no-verify`. Failure →
       `failed:commit`.
    2. *Pin.* `oid = git rev-parse --verify HEAD^{commit}` once; every later step names `oid`, never `HEAD`.
    3. *Verify.* `git rev-list --parents -n 1 <oid>` must be exactly `<oid> <base_oid>`, and every entry of
       `git diff-tree -r -z --no-renames --raw <base_oid> <oid>` must be status `M` with old and new mode `100644`
       and a path matching `paths`. Otherwise → `proposal:commit-scope` (a hook changed the index or the history).
    4. *Credential check.* The credential set of step 9 is matched against the published bytes: `git diff --text
       --no-ext-diff --no-textconv --no-color <base_oid> <oid>` (as `internal/checkpub` reads diffs, so the owner's
       diff drivers cannot hide bytes), the commit message of `oid` (`git show -s --encoding=UTF-8 --format=%B
       <oid>`), and the title and body files; a failed read → `failed:check`. A hit → `proposal:credential`, before
       the public check, the Codex check and any remote write. The guarantees differ by source: a value the model
       wrote is stopped before any commit (step 9); a value a commit hook adds is already in the local commit and is
       stopped before Codex and the push. Removing the worktree drops the commit from every ref, but its objects can stay
       in the shared object store until Git prunes them; erasing them is not part of this guarantee (they never
       leave the machine). `stack-public check` alone does not cover it: a credential is not a private term.
    5. *Public check.* `stack-public check <base_oid>..<oid> --title-file <t> --body-file <b> --branch improve/<W>`.
       Exit 1 → `proposal:public-check` (hits, redacted, in the stamp), exit 2, or a `stack-public` that is missing or
       does not start → `failed:check`.
    6. *Codex check* (§9 D1). If less time is left than the Codex timeout plus push and PR (§3.9) →
       `failed:deadline`. `codex exec --ephemeral --skip-git-repo-check -s read-only -c approval_policy=never -C
       <empty dir> --output-schema <verdict schema> -o <verdict> -` with the legacy review prompt
       (`improve.py:93-109`) over the brief, the commit message of `oid`, the title and body files and
       `git diff <base_oid> <oid>`; its timeout is taken after that input is ready. Any HIGH → `proposal:codex-high`; non-zero exit, timeout, missing or invalid
       verdict → `proposal:codex` (`improve.py:828-855`). Its output only stops; nothing it writes is published.
    7. *Record.* Rewrite the `started` stamp with `oid`, `branch`, `title` and `body` before any remote write.
    8. *Push.* If less time is left than push and PR → `failed:deadline`. From the worktree, so a `pre-push`
       hook runs there and not in the owner's checkout, with a timeout that leaves the PR its full step: `git push
       --porcelain --no-follow-tags
       --recurse-submodules=no origin --force-with-lease=refs/heads/improve/<W>: <oid>:refs/heads/improve/<W>`:
       only that ref is sent (no tags, no submodules), and the empty expected value makes the push fail when the
       branch exists, even when a fast-forward would be possible. Success requires exit 0 and the porcelain line for
       `refs/heads/improve/<W>` with flag `*` (new ref); Git reports a ref already at `oid` as up to date (`=`)
       before it checks the lease, so the exit code alone is not proof. A rejection or any other flag →
       `failed:push` with the porcelain line. `origin` is the target's own remote; a `pushurl` differing from the
       fetch URL is the owner's own configuration and is not checked.
    9. *PR.* `gh pr create --repo github.com/<slug> --base <default> --head improve/<W> --title <title>
       --body-file -` with the checked bytes on stdin: right after `stack-public check`, the title and body files
       are read once, and if they no longer hold what was written → `proposal:public-check`; Codex and gh get those
       bytes and no file is read again. Then one read; create gets at most half
       of the PR step it shares with the read, so the read always has time. The read is made once per run whatever create returned and reused by step 10: `gh api 'repos/<slug>/pulls?state=all&head=<owner>:improve/<W>&per_page=100'`.
       The outcome is `pr <url>` (`pass`, URL in `context`) only when exactly one PR in that answer has
       `head.repo.full_name == <slug>` (case-insensitive), `head.ref == improve/<W>`, `head.sha == oid` and
       `base.ref == <default>`, and that PR's `html_url` is `https://github.com/<slug>/pull/<n>`. `gh pr create` takes a branch name, not an OID, so its exit code alone does not
       prove which commit the PR holds.
    10. *Ambiguous result* (push or PR create fails without a clear rejection or times out, or the read of step 9
       has no unique match). Read-only: `git ls-remote origin refs/heads/improve/<W>` plus the step 9 read (made now
       only if step 9 never ran because the push was ambiguous). Exactly one matching PR → `pr <url>`; an empty read
       and the branch at `oid` → `pushed-no-pr` with the exact `gh pr create` command (title and body are in the
       stamp); several matches, a PR that differs in head repo, OID or base, or a failed query → `publish-unknown`,
       with what the queries showed recorded in the stamp. No resend, no retry; only facts found before the deadline
       are recorded.
13. **Cleanup.** `git worktree remove --force <work>/<W>` only when this run's `git worktree add` created it (step
    8; the worktree is then improve's own), inside the 60-s reserve with 10 s kept for the stamp and the response;
    an add on a leftover `<work>/<W>` leaves the directory alone;
    a failure is a stderr line and a `leftover-worktree` finding, and later runs report it (step 8). Then step 10's
    stamp removal, if due; then the lock is released.

`context` always states the outcome, the week, the cost, `summary` and `watch` when the model returned them, and,
when one exists, the PR URL or the stamp path. `summary`, `watch` and each finding's `where` and `detail` are cut to
300 characters, `context` to 16 KiB and the findings to 50, so the response stays far below core's 1 MiB; the
stamp keeps them whole. Progress on stderr includes the `claude` argv, the environment changes improve makes for
it, and the Claude and Codex timeouts and wall times (P5, P11).

**Stamp file** (`~/.local/state/stack/improve/YYYY-Www`, JSON): `week, started, finished, repo, base_oid,
outcome, cost_usd (number|"unknown"), evidence_ids, summary, watch, oid, branch, title, body, pr,
reason, patch, check_hits, codex_high, codex_runs`; `repo` is `<owner>/<name>`, not the checkout path. With the lock file, the only state improve keeps.

### 3.5 State files

| Path | Writer | Readers | Lifetime |
|---|---|---|---|
| `~/.local/state/stack/cycle/last.json` | `scripts/stack-cycle` (validated, temp + rename) | statusline, owner (`jq -r '.results[].context'`) | replaced only by a valid aggregate |
| `~/.local/state/stack/cycle/last-error` | `scripts/stack-cycle` (temp + rename) | statusline, owner | written on a failed run, removed on a valid one; only the job runs the wrapper, so its presence means the latest run failed |
| `~/.local/state/stack/observe/latest.json` | `stack-observe` | `stack-improve`, owner | overwritten each observe run |
| `~/.local/state/stack/improve/YYYY-Www` | `stack-improve` | `stack-improve`, owner | one per week; `patch` ≤ 256 KiB, the other fields (title, body, check hits, Codex findings) are small but not capped; never pruned automatically |
| `~/.local/state/stack/improve/lock` | `stack-improve` | `stack-improve` | permanent empty file; the lock lives only while a run holds it |
| `~/.local/state/stack/improve/work/<W>/` | `stack-improve` | — | removed at the end of the run; a leftover of an abnormal end is reported, never removed by a run (§3.4 step 8) |
| `~/.config/stack/improve.toml` | owner | `stack-improve` | owner-managed opt-in |

`phase2-core.md` §2.1 already reserves the improve stamp path.

### 3.6 launchd wiring (dotfiles)

`launchd.agents.stack-cycle` in `nix/home/profiles/dev.nix` replaces `kaizen-cycle` in the same commit; cutover step
2 (§6) checks on the machine that the old job is unloaded. `StartCalendarInterval` 06:00, `ProcessType =
Background`, `LowPriorityIO`, log `~/Library/Logs/stack-cycle.log`, `PATH` = the directory holding `stack` and the
extensions, the Home Manager profile (`jq`, `chezmoi`), the mise shims (`claude`, `codex`, `gh`) and the system
directories (`sqlite3`, `git`). Program: `/bin/sh <store copy of scripts/stack-cycle>`:

```sh
#!/bin/sh
# Run observe, then improve, from the dotfiles checkout; keep the last valid result for the statusline.
set -u
out=$HOME/.local/state/stack/cycle
latest=$HOME/.local/state/stack/observe/latest.json
obs="" imp="" all=""
fail() {
  printf 'stack-cycle: %s\n' "$1" >&2
  rm -f ${obs:+"$obs"} ${imp:+"$imp"} ${all:+"$all"}
  if e=$(mktemp "$out/.error.XXXXXX"); then
    { date -u +%Y-%m-%dT%H:%M:%SZ && printf '%s\n' "$1"; } >"$e" && mv -f "$e" "$out/last-error" || rm -f "$e"
  fi
  exit 2
}
# valid NAME FILE CODE: exit 0 and exactly one cycle object with one self-answered result for NAME
valid() {
  [ "$3" -eq 0 ] && jq -e -s --arg n "$1" 'length == 1 and (.[0] | .stage == "cycle"
      and (.status | IN("pass", "warn")) and (.results | type == "array" and length == 1)
      and (.results[0] | .name == $n and .error == "" and (.status | IN("pass", "warn"))
           and (.findings | type == "array")))' "$2" >/dev/null 2>&1
}
mkdir -p "$out" || { printf 'stack-cycle: cannot create %s\n' "$out" >&2; exit 2; }
src=$(chezmoi source-path) || fail "chezmoi source-path failed"
[ -n "$src" ] || fail "chezmoi source-path printed nothing"
root=$(dirname "$src") && cd "$root" || fail "cannot enter the dotfiles checkout"
start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
obs=$(mktemp "$out/.observe.XXXXXX") || fail "mktemp failed"
stack run cycle --ext observe >"$obs"; code=$?
valid observe "$obs" "$code" || fail "observe: exit $code without a valid result"
jq -e '[.results[].findings[]? | objects | select(.kind? == "latest-not-written")] | length == 0' "$obs" \
  >/dev/null || fail "observe could not write latest.json"
jq -e --arg s "$start" '.version == 1 and (.window.until | fromdateiso8601) >= ($s | fromdateiso8601)' \
  "$latest" >/dev/null 2>&1 || fail "observe did not write a fresh latest.json"
imp=$(mktemp "$out/.improve.XXXXXX") || fail "mktemp failed"
stack run cycle --ext improve >"$imp"; code=$?
valid improve "$imp" "$code" || fail "improve: exit $code without a valid result"
all=$(mktemp "$out/.last.XXXXXX") || fail "mktemp failed"
jq -s '{stage: "cycle", status: (if any(.[]; .status == "warn") then "warn" else "pass" end),
        results: [.[].results[]], skipped: []}' "$obs" "$imp" >"$all" || fail "cannot merge the results"
mv -f "$all" "$out/last.json" || fail "cannot replace last.json"
rm -f "$obs" "$imp"
rm -f "$out/last-error" || fail "cannot remove last-error"
exit 0
```

A run is valid only when both calls exit 0 and each prints exactly one `cycle` object whose status is `pass` or
`warn` and whose only result is the expected extension, answered by itself (no synthetic `error`, a `pass`/`warn`
status, a findings array), and observe has written a `latest.json` whose window ends at or after the job started.
Improve runs only after a valid observe. Anything else, including core exit 2 (no output), an extension missing
from `PATH` (core lists it under `skipped`), a crashed extension, an observe `latest-not-written` finding, or a
stale `latest.json`, keeps the previous
`last.json`, replaces `last-error` atomically and exits 2. A valid run merges the two results into one `cycle`
object, replaces `last.json`, removes `last-error` and exits 0. Running the stages by name (`--ext`) leaves
`[stages].cycle` unused by the job; `stack run cycle` itself works on every OS, and only this job is macOS-specific.

### 3.7 Statusline

`comp_cycle` replaces `comp_kaizen` (`statusline.sh.tmpl:151-175`) and reads `~/.local/state/stack/cycle/`:

- `last.json` absent: `last-error` present → `cycle ✗`; the Home Manager `stack-cycle` agent plist exists → `cycle
  never` (the job is installed, no run has succeeded); else nothing (cycle not set up);
- `jq` cannot read `.status` as a string, or `.results` is not an array → `cycle ?`;
- `.status` other than `pass` → `cycle <status> <N>`, `N` = total `findings` entries over `.results` (findings
  listed, not changes counted: observe lists at most 20 `replay-change` findings, its `context` has the total);
- `last-error` present → `✗` appended: the shown result is the previous valid run, the latest failed (a valid run
  removes `last-error`, so no time comparison is needed);
- file mtime (`date -r <file> +%s`, BSD and GNU) older than 36 h → `stale <d>d` appended (or alone when `pass`).

Shown in red; nothing when `pass`, fresh and without `last-error`. Tests move to `tests/test_statusline.py` with
fixture files.

### 3.8 How a proposal reaches the owner

A change is one PR on the target repository's origin, branch `improve/<YYYY-Www>`, created by the owner's own `gh`
and `git` credentials, body ending in the evidence URLs, after the Codex check found no HIGH. The owner reviews it,
merges it in the GitHub UI as for any PR there, and runs `make apply`. A change that could not be published
(`proposal:*`, `pushed-no-pr`, `publish-unknown`, `failed:*`) appears as a `warn` in `last.json` and in the
statusline, with the patch, or the branch, OID, title and body, in the stamp.

### 3.9 Time budget

Core gives each `stack run` stage call one 30-min deadline (`internal/ext/ext.go:48`) and kills only the direct
child at expiry (`phase2-core.md` §2.2). The job (§3.6) therefore makes two calls, `--ext observe` then `--ext
improve`, so each extension has a stage of its own, and each extension sets its own absolute deadline on a
monotonic clock when it starts, before core's, reserves its cleanup and response time, and cuts every child timeout
to fit.

| Extension | Deadline | Reserve | Steps |
|---|---|---|---|
| observe | start + 5 min | 20 s (`latest.json`, response) | sources in order; gh calls 60 s each, cut to the deadline; replay uses what is left (§3.3) |
| improve | start + 29 min | 60 s (worktree removal, stamp, response) | pre-model ≤ 2 min (repo view, fetch 60 s, PR pages, worktree); model; after the model 9 min: commit 1, verify, credential and `stack check public` 1, Codex 5, push 1, PR and its read 1 |

The model's timeout is the time left minus the reserve and the 9 min after it. The model does not start when that
is under 5 min: checked before the stamp (step 6, no stamp) and again right before starting `claude` (step 8,
stamp removed). With a normal pre-model phase the model gets about 17 min (29 − 2 − 9 − 1), against 40 min in
legacy (`improve.py:56`); a timeout keeps the stamp, so the week is used. A model that ends early leaves its unused
time to the Codex check, whose timeout is cut from what is left minus push, PR and the reserve. Before the Codex
check and before the push, improve stops with `failed:deadline` when the rest cannot finish. The first approved run
records the real Claude and Codex durations, both timeouts, the cost or `unknown`, and the outcome (P5), which is
the evidence for changing these numbers.

The other limits: 4 parallel replays and 5 s each (the hook's own settings timeout; 4 is a starting value, P3
records the replay wall time on the real machine); 100,000 payloads (newest first, bounded again by time; 5,000
covered ~7% of a real 7-day window in 8.7 s, and the time bound is the real limit); 20 listed
findings with all counted in `replay.changed` (keeps `context` under 4,000 chars); 24 h freshness (one missed daily
run makes the evidence stale; the next run refreshes it, free); 256 KiB patch (instruction edits are far smaller;
the cut is marked).

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
| **Wrapper validates the aggregate and renames it to `last.json` (chosen)** | no core change; one file holds every result; atomic; the previous valid result survives a failed run | a dotfiles shell script that must check each failure | high |
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
| **`repo` from the request, its default branch, `.stack/improve.toml` read from the pinned base (chosen)** | the target repository declares its own editable paths, reviewed and versioned with it; any other cwd is a no-op; one ref, no config-before-fetch cycle | the launchd job must start in the checkout; a non-default base is not possible | high |
| configurable `base` key | targets a release branch | the key is read from a commit of the ref it names; needs a trusted discovery ref | high |
| `~/.config/stack/improve.toml` with repo path and globs | cwd-independent | scope lives outside the repository it governs; unreviewed | high |

The owner opt-in (`enabled`) lives in `~/.config/stack/improve.toml` because it is a money decision of this machine's
owner, not of the repository.

### 4.5 Replay baseline

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **Decision recorded in the transcript (chosen)** | needs no state; compares what actually happened with what the installed hook decides now, across stack upgrades and the Phase 2 switch | files changed since the call can change a decision (noise, stated) | high |
| Snapshot of the previous run's replay | isolates changes of `stack` itself | state file, and changes are seen only on overlap days | high |

### 4.6 T3 Code source

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **`sqlite3 -readonly -json` command (chosen)** | parity with legacy, no Go dependency; `sqlite3` ships with macOS | depends on a CLI and on T3's internal schema (as legacy did); like legacy's `mode=ro`, a read can create the `-wal`/`-shm` files beside a database in WAL mode (`observe.py:226-228`; SQLite "WAL mode, read-only databases") but writes no rows | high |
| `modernc.org/sqlite` | in-process | large dependency for one query | high |
| drop the source | simplest | the transcripts source skips T3 owner turns, so every owner turn typed in T3 would be lost from `owner-reply` and the tally | high |

### 4.7 gh model version source

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **`stack --version --json` (chosen)** | the value comes from the installed `stack`, so mismatched builds are still right; no `internal/` import | one additive CLI flag | high |
| import `internal/ghargs` in observe | no core change | breaks the boundary; wrong when `stack` and `stack-observe` are built from different commits | high |

### 4.8 Time budget

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **One launchd job, two stage calls (`--ext observe`, then `--ext improve`) (chosen)** | improve has its own 30-min stage (model about 17 min); improve runs only after a valid, fresh observe; one job, one log, one `last.json`; no core change | the job names its extensions, so `[stages].cycle` is unused by it; the wrapper merges two aggregates | high: the wrapper alone changes |
| Both in one `stack run cycle` (`[stages].cycle`) | uses the configured stage; one call | improve shares 30 min with observe and plans 29 min of its own, so core's deadline can kill it mid-publish, leaving its children (§3.1) | high |
| Two launchd jobs | independent schedules | no order guarantee (after sleep, improve can fire before observe), two status paths | high |
| `deadline` field in the request | core-computed budgets | a core change no Phase 3 extension needs | low |

`jev-classify` (Phase 4) is placed by Phase 4, as a third call or inside the observe call.

### 4.9 Run serialisation

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **One `flock` run lock plus the week stamp (chosen)** | the stamp only blocks a second model attempt in the week; the lock keeps the worktree, the open-PR check and cleanup single-run, across week boundaries; released by the kernel on death | one lock file | high |
| week stamp alone | no lock | a Sunday run still in the model and a Monday run both see no open PR, and the later cleanup removes the running worktree | high |
| lock alone | no stamp | no weekly bound on model attempts | high |

## 5. Dotfiles Instruction Changes

| File | Change | Reason |
|---|---|---|
| `skills/hiden/references/attention.md:3-9` | "注意の予算" becomes "注意の項目": items live in the kit's `attention/items.json`; sizes and counts are shown by `stack budget status` (display only); no caps, no "only goes down", no global register; the steward and ledger lines (`:8`) stay | §1 no budget ratchets |
| `skills/hiden/SKILL.md:58` | the clause "足す規則は先に…注意の予算に通す" is removed; the retrospective trigger stays | §2.1 |
| `skills/hiden/references/retro.md`, `orchestration.md:20,40`, `agents/steward.md`, `skills/wasure/SKILL.md` | **unchanged**: the review ledger keeps live readers and writers (`wasure` decides the global review on `global.md` and appends rows, `wasure/SKILL.md:13,64-66`; the retrospective writes rows and calls `wasure`, `retro.md:5,19`; the steward reads it) | a reader remains for each, and observe's `ledger-miss` reads it |
| `skills/hiden/references/commit-pr.md` | a step before push and before `gh pr create`/`edit` in a public repository: `stack check public <base>..HEAD --title-file … --body-file … --branch …` | `phase2-core.md` §4.3 relies on "the commit-pr skill's order" for `--fill`, which the file does not state yet |
| `executable_statusline.sh.tmpl:151-175` | `comp_cycle` (§3.7) | §4 S3 |
| `AGENTS.md:45` | `writing.md` is `extensions/improve/writing.md` in `shuymn/stack`; a run uses the copy embedded in the installed `stack-improve`; reinstall after changing it | §2 layout |
| `AGENTS.md:51` | budget clause removed | §2.1 |
| `home/dot_codex/AGENTS.md.tmpl:4`, `home/dot_pi/agent/AGENTS.md.tmpl:4` | line removed | `.claude/kaizen-managed` retired |
| `docs/kaizen.md` | keeps §1–§3.3 (principles, evidence, roles) and §5 (the probe and post-merge observation, still served by `signals.py` and the probe); §3.4, §4 (first changes, done or superseded) and §6–§8 (cycle, PR plan, rejected options) are replaced by one pointer to this document | the cycle is specified here; the probe keeps its documentation |
| `nix/home/profiles/dev.nix:78-83` comment | describes `stack-cycle` and points to this document | |

## 6. PR Split

`shuymn/stack` (each passes `scripts/test` and the Windows job):

1. **feat: add version json and the extension import boundary** — `stack --version --json`, the `depguard` rules,
   `docs/architecture.md` §3 line. Done when K1–K2 pass.
2. **feat(ext): add observe** — `extensions/observe` (sources, signals, replay, gh version, `latest.json`). Done when
   O1–O10 pass. Needs PR 1.
3. **feat(ext): add improve** — `extensions/improve` with `writing.md` (caps rules removed), run lock, Codex check.
   Done when I1–I16 pass, including the ported legacy guarantees of §7.1. Needs PR 2's `latest.json` schema (§3.3).
4. **docs: install and run the cycle extensions** — README install line and the `~/.config/stack/improve.toml`
   opt-in, `docs/architecture.md` §6 marks Phase 3.

`shuymn/dotfiles` (owner merges each; numbered in cutover order):

1. **feat: run the cycle through stack** — `.stack/improve.toml`, `scripts/stack-cycle`, `stack-cycle` launchd job
   replacing `kaizen-cycle`, `comp_cycle`, `tests/test_statusline.py`, `tests/test_stack_cycle.py`, and
   `scripts/stack-cycle` added to the `automation.yml` path filters (`automation.yml:3-24`). Preconditions: stack
   PRs 1–3 merged and installed; no open `kaizen/` PR. Improve stays off after the merge (no
   `~/.config/stack/improve.toml`). Done when D2–D3 pass.
2. **refactor: generate stack private terms without public-check** — port `host_values` into
   `stack-private-terms`, read the hand-kept list from `~/.config/stack/private-terms.local`, update the chezmoi
   script inputs, give `scripts/kaizen/probe/build` its own copy of `scan` (§2.1), and add `scripts/stack-private-terms`
   to the `automation.yml` path filters. Done when D1 passes.
3. **docs: update skills for the stack cycle** — §5 instruction changes. After dotfiles PR 1.
4. **chore: retire the kaizen cycle** (last) — repository deletions only: `scripts/kaizen/{cycle,observe,improve,
   apply,ready}.py`, `watch.json`, `writing.md`, `public-check`, `scripts/kaizen/__pycache__` if tracked,
   `tests/test_kaizen_{apply,cycle,improve,public_check}.py`, the parts of `tests/kaizen_fakes.py` no remaining test
   uses, `.claude/kaizen-managed`, `home/dot_claude/hooks/executable_attention-budget.py`,
   `home/dot_claude/attention/create_budget.json` and the global `home/dot_claude/attention/items.json` (no reader
   since Phase 2); the deleted paths leave `automation.yml`; `docs/kaizen.md` trimmed (§5). `signals.py`, `probe/`
   and their tests stay. Nothing is added to `home/.chezmoiremove`: deleting a source file leaves its live copy in
   place, and the live copies go on the post-retirement list below. After at least one week of daily `stack-cycle`
   runs with P1–P6 holding and P5 observed once. Done when P7 holds.

**Cutover order** (owner, on the real machine; this is the one order, §9 D2 refers to it). Git order alone does not
order these, because `make switch` loads launchd jobs while `make apply` writes the private terms and the
statusline (`dotfiles:Makefile:80,113`):

1. Install the stack binaries (stack PRs 1–3). Improve is off: no `~/.config/stack/improve.toml`.
2. **Stop and verify the old job.** Merge dotfiles PR 1; `make switch`; verify `launchctl print
   gui/$(id -u)/<kaizen-cycle label>` fails and the `stack-cycle` label is loaded; `make apply` for the statusline.
   The new job may fire from here on; with improve off it only observes.
3. **Copy the private terms and verify P10.** Copy (do not move) `K/private-terms` to
   `~/.config/stack/private-terms.local`; merge dotfiles PR 2; `make apply`; verify P10 (term sets equal). The
   generator skips with exit 0 when `uv` is missing (`run_onchange_after_write-stack-private-terms.sh.tmpl:10-16`)
   and core accepts a missing terms file (`internal/checkpub/checkpub.go:95-99`), so the check is the term-set
   comparison, not the exit code.
4. **Verify observe.** `launchctl kickstart -k gui/$(id -u)/<stack-cycle label>`; verify P1–P3 (improve answers
   `pass` "improve disabled").
5. **Owner opt-in.** The owner writes `enabled = true` to `~/.config/stack/improve.toml`. If the old job made an
   attempt in the current ISO week (newest `K/proposals/<date>-<id>/result.json` `started` in this week), enable it
   from next Monday instead.

**After retirement** (no PR): with the old job stopped and the private-terms copy verified, the orchestrator lists
the exact paths with sizes and the owner decides what to delete (§9 D3); nothing is deleted before that:
- legacy state: `~/.claude/kaizen/` (including `runs/`, `proposals/`, `probe/` results and `private-terms`),
  `~/.local/state/kaizen-apply/` with its backups, `~/.local/state/kaizen-improve/`;
- live copies of retired sources: `~/.claude/hooks/attention-budget.py`, `~/.claude/attention/budget.json`,
  `~/.claude/attention/items.json` (removable later with `home/.chezmoiremove` entries or by hand, as the owner
  decides).

## 7. Test Strategy

- **Unit, table-driven** (`extensions/observe`, `extensions/improve`): signal rules on anonymised transcript excerpts
  and T3 rows in `testdata/`; recorded-decision parsing; stamp states; tree and commit-scope validation; `paths`
  glob matching; config decoding; credential redaction.
- **Real formats**: `testdata/` holds anonymised excerpts of real transcripts (paths, names and ids replaced, the
  JSON shape untouched): a legacy-hook deny, a `stack guard` deny, a result with errors of several hooks, a user
  permission refusal, a `tool_use` with no result, a T3 session. At least one legacy T3 SQLite fixture is run
  through the real `sqlite3` with observe's SQL (skipped when `sqlite3` is absent; present on macOS and the Linux CI
  runner).
- **Fake executables** via `internal/proc/proctest` (test files only): `gh`, `claude`, `codex`, `sqlite3`, `stack`
  with scenarios and argv/stdin logs. **Real git** via `internal/gittest` for fetch, worktree, commit, hooks and push
  (to a local bare remote; commits unsigned via `-c commit.gpgsign=false` in the test repo's config).
- **Replay integration**: the test builds `./cmd/stack` once (`go build` in `TestMain`) and replays real-format
  excerpts, including a call whose input the real hook quotes in its deny reason.
- **Processes**: the multi-process tests build `stack-improve` with the test-only build tag `improvetest`, under
  which the clock comes from `STACK_IMPROVE_NOW`; release builds have no such input.
- **Protocol**: each binary is run through `stack run cycle --ext <name>` with fakes on `PATH` in one test per
  extension, asserting a valid response and `status`, and that core adopts the response (no synthetic `error`).
- **dotfiles**: `tests/test_statusline.py`; `tests/test_stack_cycle.py`; `stack-private-terms` parity;
  `make check` evaluates the launchd job.
- **Measure on the real machine**: observe wall time, replay wall time and counts (P3); on improve's first approved
  attempt, the Claude and Codex durations, both timeouts, the cost or `unknown`, and the outcome (P5), plus the
  effective settings check (P11).

### 7.1 Legacy guarantees ported before deletion

The retirement PR deletes `tests/test_kaizen_improve.py` and `test_kaizen_cycle.py` only after these guarantees have
a test in `extensions/` (row): every page of open PRs and head-repo matching (I4); credential leak and redaction
(I13); the checked OID is the pushed OID, and a new branch only (I10, I11); the stamp after abnormal exits (I2, I6,
I7); no remote write without enough time (I14); one run at a time (I5); a Codex refusal stops publication (I16).

## 8. Acceptance Criteria

### 8.1 Purpose layer

| ID | EARS criterion | Observable result / failure | Verify on the real machine |
|---|---|---|---|
| P1 | When the daily job fires, the system shall run observe and then improve from the dotfiles checkout and replace `last.json` with one `cycle` object in which both answered. | after the fire, `last.json` differs from the copy taken before it; `.stage == "cycle"`; exactly `observe` and `improve` in `.results`, each with empty `error`; no `last-error` | copy `last.json` aside; `launchctl kickstart -k gui/$(id -u)/<label>`; wait for the job to end; `cmp` the copy with `last.json` (must differ); `jq '.status, [.results[] \| {name, error}]' ~/.local/state/stack/cycle/last.json`; `test ! -e ~/.local/state/stack/cycle/last-error` |
| P2 | When observe runs, it shall report the 7-day counts per source and signal and write `latest.json`. | every source `ok` or `off`; `latest.json` window equals the context's; counts match `jq '.items \| group_by(.signal)'` | `stack run cycle --ext observe \| jq -r '.results[0].context'`; compare with `latest.json` |
| P3 | When the installed hook decides a recorded call differently from the transcript, observe shall report a `replay-change` finding; otherwise none for that call. | (a) fixed fixture: a known recorded `stack guard (secrets)` deny replays unchanged and a known recorded `no-deny` call that the current rules deny is reported as `no-deny → deny:<rule>`; (b) real window: `replay.compared > 0`, and the counts (`compared`, `changed` by kind, `not_comparable`, `unrecoverable`, `truncated_*`) and the replay wall time are recorded, not judged | (a) `stack-observe` with `HOME` set to a directory holding only the fixture transcript; (b) the job's run, `jq .replay latest.json` and the log's timing; a changed decision is read by the owner, since files and cwd state can differ from the time of the call |
| P4 | While the `gh` on the job's `PATH` differs from the guard's modelled version, observe shall warn. | finding `gh-version` with both versions; none when equal | run `gh --version` with the launchd job's `PATH` and compare with `stack --version --json`; the observe finding must match that comparison |
| P5 | When improve is enabled, it shall (a) attempt when its preconditions hold, (b) make at most one attempt with unknown or non-zero cost per ISO week, and (c) publish only when every gate passes. | (a) with improve enabled, public evidence, no open improve PR and no stamp this week, a run creates the stamp and starts the model; (b) a second run that week calls no model and reports the stamp; (c) a `pr <url>` outcome has a PR whose files are within `paths` and whose head is the stamp's `oid`; `no-change` and a Codex refusal (`proposal:codex*`) are valid outcomes with no push. The first approved run also records the real Claude and Codex durations, both timeouts, the cost or `unknown`, and the outcome | (a) inspect the stamp after the run; (b) rerun `stack run cycle --ext improve`; (c) `gh pr view --json files,headRefOid` against the stamp; durations from the log |
| P6 | While `last.json` is not `pass`, is older than 36 h, or `last-error` exists, the statusline shall show the `cycle` segment; otherwise nothing. | segment text | edit a copy of `last.json`; touch it to an old date; write a `last-error` |
| P7 | When the retirement PR is merged, no executable file or instruction of the dotfiles repository shall reference the retired kaizen scripts, `attention-budget.py` or `.claude/kaizen-managed`, and the old job shall not be loaded. | grep hits only in allowed places; `launchctl print …kaizen-cycle` fails | `git grep -nE 'scripts/kaizen/(cycle\|observe\|improve\|apply\|ready)\|attention-budget\|kaizen-managed\|public-check' -- ':!tests/test_claude_settings.py' ':!home/.chezmoiremove'`; allowed: the `docs/kaizen.md` pointer; `tests/test_claude_settings.py:31-32` is an intended absence check |
| P8 | While improve runs, the owner's checkout shall be unchanged except `refs/remotes/origin/<default>`, the tracking ref `refs/remotes/origin/improve/<W>` a successful push writes, and Git's object and worktree metadata. | snapshots equal | before and after a manual improve run: `git status --porcelain=v2 -z --untracked-files=all` (no `--branch`: its ahead/behind header moves with remote-tracking refs), `git symbolic-ref -q HEAD`, `git rev-parse HEAD`, `git ls-files -s`, `git for-each-ref` without `refs/remotes/origin/<default>` and `refs/remotes/origin/improve/<W>`, `git stash list`; diff the two snapshots; `git worktree list --porcelain` shows no entry left under `~/.local/state/stack/improve/work/` |
| P9 | Where Linux or Windows is the runtime, `stack run cycle` shall not fail because of the cycle extensions. | both extensions in `.results` with empty `error`; observe `pass`/`warn` with sources `off`; improve `pass` | CI protocol test on all runners; `stack run cycle` on a Linux host |
| P10 | When `stack-private-terms` runs, it shall write the same set of terms as before the port. | term sets equal (header line ignored) and file mode 0600 | regenerate before and after dotfiles PR 2; compare sorted non-comment lines |
| P11 | When improve starts `claude` on the real machine, the model's effective input shall exclude project and local settings (including the main checkout's `additionalDirectories`) and auto memory. | the logged `claude` argv has `--setting-sources user`, the auto memory variable and `--settings` keys; with a dummy `additionalDirectories` entry in the main checkout's `.claude/settings.local.json` and a marker line in the repository's auto memory `MEMORY.md`, the read of a file in that directory comes back as a refused tool result in the session's JSON output (not merely unmentioned), and the marker is absent; positive control: the same probe without `--setting-sources user` and the memory switches, in the same worktree, quotes the marker and reads the file | before the first approved real run, with the owner's approval for two billed probe calls: the owner adds both dummies; the logged argv is run in an improve-style worktree with a probe prompt (read the file; quote any marker line), with `--output-format stream-json` instead of `json` so the `Read` call and its refused result can be matched; the effective environment of that call (no `CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`) is checked from the launcher's log, not from the argv; then the positive control runs without `--setting-sources user` and the memory switches; both dummies are removed afterwards |

### 8.2 Plan layer

| ID | EARS claim | Test | Required assertions |
|---|---|---|---|
| K1 | `stack --version --json` shall print `version` and `gh_model` equal to `ghargs.GhVersion`; `--version` output shall not change. | `cli.TestVersionJSON` | both fields and values; plain output byte-equal to before |
| K2 | Non-test files under `extensions/` shall not import `internal/`, and `cmd/`, `internal/` shall not import `extensions/`; extension tests may import `proctest` and `gittest`. | `golangci-lint` depguard in `scripts/test` | checked once in the PR: a scratch import in each forbidden direction fails lint, an extension test importing `proctest` passes |
| O1 | When a source fails, observe shall mark only that source, keep the other sources' results, and answer `warn`. | `observe.TestSourceIsolation` | unreadable transcript → `fail` listed; gh still queried and its items present; T3 failure keeps transcript items |
| O2 | Observe shall select transcripts by mtime ≥ `since` without an upper bound, count only records in `[since, until)`, and read only complete lines of regular files. | `observe.TestWindow` | boundary timestamps; a file whose mtime moved past `until` by an append keeps its in-window records; unterminated last line skipped; FIFO and symlink listed unreadable |
| O3 | Observe shall emit the eight signals by the legacy rules and de-duplicate by id. | `observe.TestSignals` | one positive and one negative case per signal; duplicate `where` of one signal once; two signals with the same `where` both kept; `reference-unread` absent when transcripts is `fail` |
| O4 | When `~/.t3` exists, observe shall query T3 with `sqlite3 -readonly -json` and the legacy SQL, tally owner, synthetic and unknown rows, and mark `t3` failed when `sqlite3` is missing. | `observe.TestT3`, `observe.TestT3RealSQLite` | argv contains `-readonly`, `-json`, the window literals and the owner conditions; legacy fixture database through real `sqlite3` gives the legacy tallies; missing tool → `fail` and T3 sessions counted `unknown` |
| O5 | Observe shall classify recorded decisions as `deny:<rule>`, `other-deny`, `no-deny` or `unknown` from the matching `tool_result`. | `observe.TestRecordedDecision` | real-format fixtures: stack deny, legacy-hook deny, several hooks, user refusal, no result |
| O6 | Observe shall replay only guard-wired tools with existing cwd and the input the guard needs, through the `stack` on `PATH`, once per unique payload compared with each record, count failures as not comparable, and report each change with tool name and rule ids only. | `observe.TestReplay` (fake stack), `observe.TestReplayRealHook` (built `stack`) | `Edit` not replayed; missing cwd and a `Bash` record without a string `command` → unrecoverable, not run, not compared; duplicates run once and compare twice; start failure, timeout, non-zero exit, bad JSON and a `stack hook:` stderr line → not comparable; three change kinds; a real hook deny whose reason quotes the input path yields a finding without that path; cap → `truncated_cap` |
| O7 | When `gh --version` differs from `gh_model`, observe shall add a `gh-version` finding and answer `warn`; equal versions add none. | `observe.TestGhVersion` | differ, equal, gh missing, a fake old `stack` that prints its plain version and exits 0 → `unknown` |
| O8 | Observe shall write `latest.json` by temp file and rename with mode 0600 and never answer `block`. | `observe.TestLatestFile` | the writer, through a test hook, stops after writing part of the temp file and signals; the test then reads `latest.json` and gets the old bytes exactly, releases the writer, and after the rename reads the complete new file (a writer that overwrites in place fails this); rename failure leaves the old file; mode 0600 on Unix; status ∈ {pass, warn} in all scenarios |
| O9 | When its deadline minus the reserve passes, observe shall stop unfinished sources and children, mark them `timeout`, and answer before core's deadline. | `observe.TestDeadline` (injected clock), protocol test with a 1-s budget | response adopted by core (no synthetic error); unfinished sources `timeout`; replay `truncated_time` counted apart from `truncated_cap` |
| O10 | On macOS and Linux, a child of observe or improve shall not leave processes of its group behind: at its timeout the whole group is killed, and after it exits the rest of its group is killed. | `observe.TestKillGroup` (Unix only) | a fake child that starts a sleeping grandchild and sleeps → after the timeout neither exists; a fake child that starts a sleeping grandchild and exits at once (parent first) → the grandchild is gone after the call returns; external `SIGKILL` of the extension is out of scope (§3.1) |
| I1 | When improve is not enabled, or `repo` is empty, or the pinned base has no `.stack/improve.toml`, improve shall answer `pass`; when not enabled it shall call nothing. | `improve.TestNotConfigured` | no file / `enabled = false` → no gh, git or claude call; empty repo; enabled repo without config → only repo view and fetch |
| I2 | While the week stamp exists, improve shall call no model, and an empty, corrupt or unfinished stamp shall be reported as "attempt outcome unknown". | `improve.TestStampExists` | per final outcome `pass`/`warn`; empty file, bad JSON, `started` without and with `oid`/`branch`/`title`/`body` → `warn` unknown naming the recorded fields; stamp untouched |
| I3 | When `latest.json` is missing or stale, improve shall answer `pass` without a stamp; when it is unparsable, of an unknown version or in the future, `warn` with a diagnostic; public items shall be used whatever the gh status. | `improve.TestEvidence` | each case; gh `fail` with public items → attempt proceeds; private and non-URL items excluded from the brief |
| I4 | When an open `improve/` or `kaizen/` PR of the same repository exists on any page, improve shall not create a stamp, and shall warn when it is older than 14 days. | `improve.TestOpenPR` | match on page 2; fork head with same ref ignored; 13 vs 15 days; gh error → warn |
| I5 | Improve shall hold one run lock from the open-PR check through cleanup and stamp finalisation, so that concurrent runs on one machine make at most one model call and never remove a running worktree. | `improve.TestRunLock` (two real processes, `improvetest` build) | A in week W blocks in fake claude, B starts at W+1 → B `pass` "active", A's worktree intact; A ends unbilled (stamp removed after cleanup), B starts right after → one new claim, no overlap |
| I6 | If the claude child never started or a complete result proves zero cost, improve shall remove the stamp after the child exits and cleanup; otherwise it shall keep it. | `improve.TestBilling` | start failure, worktree failure, too little time right before start → removed; cost 0 with all four counters 0 → removed; kept with cost `unknown` and `cost-unknown`: cost 0 + `usage: {}`, cost 0 + one counter missing, cost 0 + a counter that is a string, negative or fractional; kept: cost 0 + a non-zero counter, missing response, timeout; kept with cost `unknown`: a zero result followed by more bytes (also after a `change: false` result), a zero result from a child killed by a signal, a result without `type: result` |
| I7 | Improve shall run `claude -p` with the stated flags, settings and environment, cwd = worktree, the brief, `writing.md` and the task on stdin, and kill it at its timeout. | `improve.TestModelInvocation` | argv exact (`--setting-sources user`; no `--restricted`, `--safe-mode`, `--add-dir`, `--max-budget-usd`; `--settings` sets `autoMemoryEnabled: false` and blocks reads and writes); env has `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`; stdin contains brief ids and `writing.md`; timeout → `failed:timeout`, cost `unknown`, child gone |
| I8 | If the worktree has anything but modifications of existing `100644` files within `paths`, or evidence ids are not ⊆ brief ids, then improve shall record a proposal with the patch and not commit. | `improve.TestProposal` | add, delete, rename, mode change, type change (file → symlink), out-of-scope path, `**` boundary (`skills` vs `skills2`), empty diff with `change: true`, foreign id; `git rev-parse HEAD` still `base_oid` and no new ref |
| I9 | Improve shall commit with the repository's hooks and the owner's config, without `--no-verify`. | `improve.TestCommit` (real git) | a failing `commit-msg` hook → `failed:commit` with patch; argv has no `--no-verify` |
| I10 | Improve shall verify the pinned OID as one commit on `base_oid` changing only allowed existing files, run the credential check, `stack-public check` and the Codex check on that OID and the fixed title and body, and push exactly that OID. | `improve.TestCommitScope` (real git), `improve.TestPublicCheck` (real `stack-public` built in `TestMain`) | a `pre-commit` hook that succeeds after staging an extra file → `proposal:commit-scope`, no push; HEAD moved after the check → pushed OID is still the pinned one; a `post-commit` hook that replaces the commit (with the extra file) by an allowed one through `git replace` → still `proposal:commit-scope`; a `pre-push` hook that rewrites the body file → `gh pr create` gets the checked body; real `stack-public check` with a dummy private term in the diff, message, title or body → `proposal:public-check`; exit 2, or `stack-public` absent from `PATH` → `failed:check` with no Codex call, push or `gh pr create` |
| I11 | Improve shall push only the pinned OID to a new `improve/<YYYY-Www>`, create one PR with the checked title and body, record a PR only when one read matches head repo, ref, OID and base, and on an ambiguous result report only what read-only queries show. | `improve.TestPublish` (bare remote + fake gh) | branch exists at an ancestor of `oid` (fast-forward possible) → `failed:push`; branch already at `oid` (Git's up-to-date, exit 0) → `failed:push`, no `gh pr create`; argv has `--porcelain --no-follow-tags --recurse-submodules=no`, and an annotated tag reachable from `oid`, with `push.followTags=true`, is not in the bare remote; `gh pr create` argv and body bytes equal the checked bytes; create succeeds but the read shows a fork's PR or another base at `oid` → `publish-unknown` with the read in the stamp; create fails but the read shows the matching PR → `pr <url>`; two matching PRs → `publish-unknown`; branch only → `pushed-no-pr`; query fails → `publish-unknown`; a matching PR without a usable `html_url` → `publish-unknown`; no second create; the fake gh log shows zero PR reads when the push is clearly rejected or up to date, and exactly one when create ran or the push result was ambiguous |
| I12 | Improve shall remove its worktree on every path after creating it, and shall report, not remove, a leftover worktree of an earlier run. | `improve.TestCleanup` | after success, proposal, failure; a leftover directory under `work/` → `leftover-worktree` finding naming it, directory and its files untouched; a leftover at this run's own `<work>/<W>` (same week, stamp released by the owner) → `git worktree add` fails, `failed:worktree`, the directory and its files untouched and no `worktree remove` in the git log; removal happens before the unbilled stamp removal; an add that registers the worktree and then fails (`post-checkout` hook) → worktree and metadata removed; with one minute left the removal still runs |
| I13 | Improve shall refuse to publish when an endpoint credential appears in the tree, the model output, or the pinned commit, title and body, and shall never write a credential value to a commit it makes from model output, a PR, a remote, the stamp, stderr or context. | `improve.TestCredential` | dummy `ANTHROPIC_AUTH_TOKEN` in env and in a settings fixture; model writes it into a file or the title → `proposal:credential`, no commit; a `commit-msg` hook appends it to the message, or a `pre-commit` hook writes it into an allowed file → local commit exists, `proposal:credential`, no `stack check public`, no codex, no push, no ref points at the commit after cleanup; the same with a `diff.external` that prints nothing configured → still `proposal:credential`; fake `claude` prints it only on stderr, or fake `gh repo view` prints the settings value → outcome unchanged, value redacted; a new file holding it past 8 MiB across a 64 KiB read → `proposal:credential`; an unreadable new file → `proposal:credential`; grep of the stamp, captured stderr, the response and the bare remote finds no dummy value |
| I14 | If the time left cannot cover the model minimum, or the Codex check, push and PR, improve shall stop before the model or before any remote write. | `improve.TestDeadline` (injected clock) | too little before the stamp → `pass`, no stamp; too little right before `claude` (slow worktree) → `failed:deadline`, no claude, stamp removed; too little before Codex → `failed:deadline`, no codex, no push; too little before push → no push; Codex input ready with 1 s left → Codex killed at 1 s; gh pr create hangs with one step left → the read still runs |
| I15 | Improve shall answer `pass` and call no model unless the target is a public GitHub repository equal to `origin`, and shall stop when the user settings add working directories. | `improve.TestTarget` | `PRIVATE`/`INTERNAL` → pass, no claude; name mismatch → pass; a look-alike host or two `remote.origin.url` values → pass, no claude, no push; `additionalDirectories` set → warn, no claude |
| I16 | If the Codex check reports a HIGH finding, exits non-zero, times out, or leaves a missing or invalid verdict, improve shall not push and not create a PR. | `improve.TestCodexGate` (real git, fake `codex`, fake `gh`) | the same pinned candidate in each of the five cases → `proposal:codex-high` or `proposal:codex`, patch in the stamp; the bare remote has no `improve/` ref and the `gh` log has no `pr create` |
| D1 | `stack-private-terms` shall write the terms the legacy generator wrote, without importing `public-check`; `probe/build` shall keep excluding private strings without it. | `tests/test_stack_private_terms.py`, `tests/test_kaizen_probe.py` `BuildTest` | fixture host.toml with generic, role and owner values; hand-kept list; `.t3` fragments; term set equal to the legacy output for the same fixture; with `public-check` removed from the test checkout, a dummy private term in the test `HOME`'s generated terms file and in one filler item → that item excluded, the build passes, and no built file contains the term |
| D2 | `comp_cycle` shall render each state of §3.7. | `tests/test_statusline.py` | absent; absent + plist → `cycle never`; absent + `last-error` → `cycle ✗`; unreadable; `.status` null (`{}`); warn with N; stale; `last-error` present with a valid `last.json` → `✗` appended; fresh pass → nothing |
| D3 | `scripts/stack-cycle` shall run improve only after a valid observe with a fresh `latest.json`, replace `last.json` atomically only when both calls are valid, keep the previous one otherwise, remove `last-error` on success and replace it atomically on failure. | `tests/test_stack_cycle.py` (fake `stack`, `chezmoi`) | both valid → merged `last.json` with exactly two results, exit 0, no `last-error`; each of: non-zero exit with valid JSON, empty output, two JSON objects, `{}`, `stage` not `cycle`, `status` `block`, the extension only in `skipped`, a synthetic `error`, a result status outside pass/warn, findings not an array → previous file kept, `last-error` written, exit 2; observe invalid, `latest.json` not rewritten (window before the job start), or an observe finding `{"kind":"latest-not-written",…}` with a same-second old `latest.json` → improve not run; `chezmoi source-path` failing or empty, unwritable state directory → exit 2 without running `stack`; the fake improve `stack` writes half its output and blocks on a FIFO, the test reads `last.json` (old bytes exactly) and `last-error` (absent or complete), then releases it; the same for a `last-error` write blocked half-way (old `last-error` or none, never partial) |

## 9. Owner Decisions

Decided by the owner on 2026-11-21:

- **D1 (external posting): the Codex check stays.** Before improve opens a PR, a read-only `codex exec` reviews the
  pinned commit, title and body; any HIGH finding, failure, timeout or invalid verdict stops publication and keeps
  the patch in the stamp (legacy `improve.py:828-855`, adapted to the pinned OID, §3.4 step 12). Its time is in the
  budget (§3.9).
- **D2 (money): paid runs need an explicit switch.** Improve calls nothing until the owner sets `enabled = true` in
  `~/.config/stack/improve.toml` (extension config, not core). One attempt per ISO week of the local date, across
  all repositories on the machine; a Sunday attempt and a Monday attempt can both spend, which is accepted. A
  timezone change moves the week boundary by at most the zone difference. The cutover order is §6: stop the old
  job, copy and verify the private terms, run the new observe, then enable improve (from next week if the old job
  already attempted this week).
- **D3 (irreversible): nothing is deleted by the cycle or a PR.** After the old job is stopped and the private-terms
  copy is verified, the orchestrator shows the exact list of legacy state paths (§6 "After retirement") and the
  owner decides.

Owner decision: no per-attempt cost cap (`--max-budget-usd` dropped); the recorded cost is observed instead.

Decided without the owner (reasons above): observe and improve as two stage calls of one job (§4.8); Opus xhigh
unchanged; one open PR at a time; the
default branch as the only base; the probe and `signals.py` kept outside the cycle; the T3 source via `sqlite3`;
Codex cwd weights, ledgers, `ledger-miss`, `meta-effort` (narrowed) and `retro.md` kept; the Codex session count
dropped; no daily history beyond `last.json`.

No open questions remain.
