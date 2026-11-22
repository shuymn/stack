# Phase 2 Design: `stack` Go Core

Implementation design for the core CLI specified in `docs/architecture.md` §3–§5 (v10). Scope is Phase 2 of
`docs/architecture.md:95`: `kit open/close`, `hook claude session-start/pre-tool-use`, `pr merge`, `check public`,
`budget status`, `run`, and the extension process protocol. Wiring into dotfiles and deleting the legacy Python hooks
is a follow-up in `shuymn/dotfiles` (§7).

Legacy sources are cited by their deployed path (`~/.claude/...`, deployed from `shuymn/dotfiles:home/dot_claude/`,
byte-identical today) and `dotfiles:scripts/kaizen/public-check`.

`docs/design/core-stages.md` (architecture v11) supersedes parts of this design: §4.1.1's size note, §4.2 step 2's
"No extensions run", §4.3's rules other than raw-merge, §4.4 step 1 (merge messages are checked only by the extensions
listed in the new `[merge].message_checks`, default `[]`), §4.5, §4.6, §4.7's stages, request fields and
`block` handling (a `block` at `entry` or `cycle` becomes `warn` with exit 0, and replay fixtures expecting `block`
there move to `warn`), the `[safety]` table and the matching C10–C12, C14–C15, C19 (its extension assertion only),
C26 (its message clause) and C27; the list is in that design's §4.10.

## 0. Principles

1. **Hooks fail open, commands fail closed.** `stack hook ...` never breaks a session: undecodable stdin, an internal
   error or a panic yields no stdout and exit 0 (`~/.claude/hooks/orchestration-context.py:341`). A broken config in a
   hook falls back to the §5 defaults, so the secrets rule never turns off because of a typo; a file the hook cannot
   read drops only the part that needed it. Only an explicit deny or compact context writes hook JSON. Non-hook
   commands that guard an irreversible step (`pr merge`, `check public`) refuse on anything they cannot verify.
2. **Zero config, zero extensions.** Every command runs with no `config.toml` and no extension installed; defaults
   are §5 verbatim (`docs/architecture.md:65-90`), incl. `[safety].deny_pencil_generate = true`. Nothing under
   `cmd/stack` or `internal/` imports or execs anything from `extensions/` by path.
3. **Hard boundaries only.** Rules: secrets read, local paths in published text, merging other than through the
   checked-head path, catastrophic `rm` of `/`, home, a drive root or a `.git` directory (`docs/architecture.md:7-8`),
   and the Pencil Generate rule (owner decision, §4.3). Nothing else is denied (`docs/architecture.md:6-9`).
4. **Static scope, stated.** Shell rules judge what the parsed command statically shows. User `gh` aliases, `xargs`,
   arbitrary scripts, interpreter code strings (`python -c`), variable expansions and dynamic pipes are outside it and
   listed in §6 rather than guessed at. Where precision would need new machinery, the rule keeps legacy behaviour.
5. **One portable implementation.** No build-tagged Unix/Windows split in this repo except executable resolution
   (§2.2), Git Bash path forms and the case-folding switch (§2.1); file locking is delegated to `gofrs/flock` (§3).

## 1. Package Layout

```text
cmd/stack/            main: os.Args/stdio/env -> cli.Run -> os.Exit. Nothing else.
internal/
  cli/                subcommand routing, interspersed flag parsing, usage text, error -> exit-code mapping,
                      composition of kit open + entry stage
  paths/              home dir, `~` expansion, state/config locations, path comparison (component boundaries,
                      per-OS case folding), slug / session-id validation, agent-worktree pattern   (stdlib, leaf)
  config/             config.toml load, §5 defaults, validation, unknown-key warnings        (-> paths)
  hookio/             Claude Code hook stdin decode, deny / additionalContext encoders,
                      FailOpen wrapper (recover + swallow)                                    (stdlib, leaf)
  proc/               Runner over os/exec, executable resolution, deadlines, output caps and streaming;
                      proctest/ = re-exec fake executables                                    (stdlib, leaf)
  shell/              Bash command -> per-statement []Call (static argv words, bound heredoc/redirects, known cwd)
                      + handoff ranges, recursing into `bash -c` / `eval` literals and bash heredocs (mvdan.cc/sh, leaf)
  ghargs/             gh argv model: global flags, subcommand aliases, per-command flag table parsed with
                      pflag; publish-input table (§4.3)                                       (spf13/pflag, leaf)
  publictext/         local-path matcher (POSIX + Windows) and private-term matcher           (stdlib, leaf)
  guard/              pre-tool-use decision: secrets.go, ghpublish.go, ghmerge.go (gqlparser), rmroot.go,
                      pencil.go (tdewolff/parse js)          (-> shell, ghargs, publictext, paths, config)
  kit/                <session_id>.kits store + OS lock (gofrs/flock), kit dir, context renderer
                      (state/items/handover/board)                                            (-> paths, config, proc)
  session/            session-start: CLAUDE_ENV_FILE export, compact restore, worker detection,
                      transcript first-prompt reader, context budget                          (-> kit, hookio, config, paths)
  github/             typed wrappers over `gh` and `git` for pr merge                         (-> proc)
  merge/              pr merge flow                                                           (-> github, proc, paths, publictext)
  checkpub/           check public: streams range texts from git, scans with publictext       (-> proc, publictext, config)
  budget/             budget status (display only)                                            (-> config, paths, kit)
  ext/                extension discovery, process protocol, aggregation, replay              (-> proc, config)
```

Dependency direction: `cmd/stack -> cli -> feature packages -> foundations (paths, config, hookio, proc, shell,
ghargs, publictext, github)`. Foundations never import feature packages; no package imports `cli`. Feature-to-feature
edges are `session -> kit` (compact restore reuses the renderer) and `budget -> kit` (kit listing and `.kits`). `kit`
does not import `ext`: `cli` runs `kit open`, then the entry stage, and appends each extension `context` with the
renderer's block format.

Lint fit (`.golangci.yaml`, `default: all`): wrapcheck applies at stdlib and third-party boundaries only (internal
packages are ignored, `.golangci.yaml:33-35`); sentinel errors are package-level `errors.New` (err113);
`gochecknoglobals` is disabled, and compiled regexps stay the only package-level vars besides `version` by
convention. Every subprocess goes through `exec.CommandContext` in `proc`, which carries the single
`//nolint:gosec // G204: no shell; executables are gh/git or user-owned paths (kit board, merge-check, stack-<name>
on PATH) that the user already controls, argv is passed as a list`. Files under a kit dir or the sessions dir are
opened through `os.Root` (Go 1.24+), which removes traversal and G304 concerns: a symlink inside a kit that points
outside the kit dir is no longer followed (legacy read it) and renders as an unreadable-file marker.

## 2. Shared Foundations

### 2.1 Paths and config (`internal/paths`, `internal/config`)

| Item | Value (all OSes) |
|---|---|
| Home | `os.UserHomeDir()` (`$HOME`; `%USERPROFILE%` on Windows) |
| Config | `~/.config/stack/config.toml` (Windows: `%USERPROFILE%\.config\stack\config.toml`). Literal §5 path, not `os.UserConfigDir`, so handover paths in §5 stay valid. XDG vars are not consulted. |
| State root | `~/.local/state/stack` |
| Kit root | `[kit].root`, default `~/.local/state/stack/kits`; a relative root is resolved against the config file's directory, as handover paths are, so `kit open` and compact name the same dir from any cwd; kit dir `<root>/<slug>` |
| Session kits | `~/.local/state/stack/sessions/<id>.kits` + `<id>.lock`; no config key, no GC (files are tiny) |
| Improve stamp | `~/.local/state/stack/improve/YYYY-Www` (Phase 3; not used in core) |

- `Expand(p)`: only `~`, `~/…`, `~\…` expand with `Home`; `~user` is left verbatim.
  Config values use this form only. Shell words carry whether their leading `~` was unquoted (§4.3): only an unquoted
  one expands, as in bash; `'~'` names a directory called `~`.
- `Resolve(cwd, p)`: `Expand` (when allowed), then on Windows the Git Bash forms (one leading `/`) are mapped
  **before** absolute-path classification, which `paths` does itself per OS rather than with `filepath.IsAbs` (that
  would treat a Git Bash `/` as relative to the current drive): `/` is the Git Bash root, which is not a drive root and is not translated: `Resolve` returns it as the
  marker `ShellRoot` (rmroot treats it as a root; file reads under it are unknown and skipped). `/<drive>` and
  `/<drive>/…` → `<drive>:\…`, except that a path whose `..` climbs above the drive mount is classified by its
  cleaned form (`/c/..` is `ShellRoot`); finally a relative path is joined to `cwd`; a Windows drive-relative `C:x` is unknown. Neither function removes `..` lexically, because past a symlink that would name another file; only `Norm` cleans, for comparison. After `cd /` the known cwd is
  unknown on Windows.
  `check public` file arguments use the same function, so the `/c/…` and `~` forms Git Bash passes open the real file.
- `Same(a, b)` / `Within(p, root)` / `HasComponent(p, name)`: compare slash-normalised paths **by component**, so
  `.t3/userdata/secrets-example` is not within `.t3/userdata/secrets`. Case is folded only where the platform file
  system is case-insensitive by default (`darwin`, `windows`); on Linux `.DEV.VARS` is a different file and passes.
- `ValidSlug`: `^[a-z0-9][a-z0-9._-]{0,63}$`, no `..`, no trailing `.` (Windows strips it, so `demo.` would alias
  `demo`), and the part before the first `.` is not a Windows device name (`con`, `nul`, `aux`, `prn`, `com1`–`com9`,
  `lpt1`–`lpt9`), so `con.txt` is rejected too.
- `ValidSessionID`: `^[A-Za-z0-9_-]{1,128}$` (Claude Code ids are UUIDs). Guards the `.kits` file name and the line
  written to `CLAUDE_ENV_FILE`, which bash sources. The file name uses the lower-cased id, so one UUID in two cases
  maps to one file on every OS; the exported value is the id as given.
- `IsAgentWorktree(p)`: a path component ending in `-worktrees` followed by a component `agent-<name>` with a
  non-empty name (`docs/architecture.md:57`); `…-worktrees/agent-` alone does not match.

Config parsing uses `github.com/BurntSushi/toml` (§3). Missing file = defaults. TOML syntax error: non-hook commands
exit 2 citing the file and the line from `toml.ParseError`. `version` other than `1` (valid TOML): exit 2 citing the
file and the key `version`. Hooks use defaults in both cases. Each schema key is decoded by its exact name
(BurntSushi matches struct fields case-insensitively); every other key in `MetaData.Keys()`, case variants included,
is ignored with one stderr warning (non-hook commands only). Arrays replace defaults (`stages.entry = []` disables entry
extensions).

`[handover]` keeps file order via `MetaData.Keys()`; the first active key wins, so `T3CODE_THREAD_ID` beats
`HERDR_ENV` with the defaults. A key is active when its env var is non-empty, except `HERDR_ENV`, which is active only
when exactly `1` (`orchestration-context.py:74`: `HERDR_ENV=0` means no handover).

`[safety].deny_pencil_generate` is a §5 default (`true`, owner decision): only an explicit `false` turns the Pencil
rule off (§4.3). There is no toggle for the other rules.

### 2.2 Processes (`internal/proc`)

`Runner` interface: `Run(ctx, Cmd{Name, Args, Dir, Env, Stdin []byte, Stderr io.Writer, MaxStdout}) (Result, error)`
and `Stream(ctx, Cmd, func(io.Reader) error) error` for unbounded output (`check public`). Stdin is in memory, so
feeding it cannot block past the bounds below. `Stderr`, when set, receives all of stderr as it arrives (extensions
pass it through, §4.7) and must not block; otherwise `Run` captures up to 64 KiB. The runner owns the stdio pipes:
when the context ends it kills the child, and once `Wait` returns (the child's exit observed, also after a timeout
kill) it waits at most `WaitDelay` (2 s) for the pipes, then closes its ends so a child whose grandchild still holds
them cannot hang `stack`. This neither kills descendants nor stops their I/O; on Windows killing the child leaves
grandchildren running. Plain `os/exec` with writer `Stdout`/`Stderr` and `WaitDelay` returns one error: it reports
`ErrWaitDelay` only when the run otherwise succeeded, keeps an earlier `ExitError` or `ctx.Err()`, and drops copy
errors after a failed exit, so an extension that exits non-zero while a grandchild holds the pipe would go unreported
(§4.7 ignores the exit code when stdout is a valid response). `Result` therefore reports each condition separately:
exit status, `TimedOut` (killed at the context deadline, `ExitCode` -1 on every OS), `WaitDelayExceeded` (a pipe cut
after `WaitDelay`, independent of the exit status), `StderrErr` (the first error forwarding stderr) and `Truncated`
(stdout over `MaxStdout`, 1 MiB default); callers evaluate these **before** using stdout.

`ResolveExecutable(dir, name)`: Unix — regular file this process may execute (effective permission, as
`exec.LookPath` checks it). Windows — only `<name>.exe` is runnable; a `<name>` without extension or with
`.cmd`/`.bat`/`.ps1` is "found but not runnable" (`os/exec` does not run batch files reliably). A directory or a dangling
symlink under the name is also not runnable, and a stat error other than "does not exist" (e.g. an unsearchable `.claude`) is
"cannot verify", never "absent", so merge-check refuses on it. `LookPathExe(name)` (extensions) walks `PATH` itself: on Unix the first runnable `name`; on Windows
the first `name.exe` (never `PATHEXT`, so a `.cmd` is not picked), and if none exists but `name`, `name.cmd`,
`name.bat` or `name.ps1` does, "found but not runnable". Relative `PATH` entries (incl. empty and `.`) are skipped and the
walk continues (Go's `exec.LookPath` instead returns `exec.ErrDot` for such a match). Per caller: kit board → `[board not runnable on this OS: <path>]` marker, skipped;
`.claude/merge-check` → merge refused; extension → `warn` result with a synthetic finding. Shelled-out commands are
`gh`, `git`, the kit board, `.claude/merge-check`, and extensions (`stack-<name>`).

### 2.3 Hook IO (`internal/hookio`)

Decodes the whole stdin JSON (no size cap, so a large valid payload is still judged) into the fields core reads: `session_id`, `transcript_path`, `cwd`,
`hook_event_name`, `source`, `agent_type`, `tool_name`, `tool_input` (kept as `json.RawMessage`, decoded per tool).
Output is buffered and written once at the end:

- deny: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":…}}`
  (`~/.claude/hooks/guard-agent-actions.py:45-50`)
- context: `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":…}}`
  (`~/.claude/hooks/orchestration-context.py:215-216`)

`FailOpen(fn)` recovers panics and maps every error to "no output, exit 0"; errors go to stderr only when
`STACK_DEBUG=1`. Legacy `guard-agent-actions.py` raised on malformed JSON; exiting 0 there is a deliberate change.

**Hook scope.** The shell rules apply to Claude Code's `Bash` and `PowerShell` tools. `PowerShell` commands are
judged by raw-merge, secrets, gh-publish and rmroot through the bounded PowerShell recognizer of
`docs/design/powershell-guard.md` (§3 there), not a PowerShell parser: a command outside its subset is skipped by the
call-based rules, and a command it cannot lex is judged by secrets only. Without Git Bash the SessionStart hook
receives no `CLAUDE_ENV_FILE`, so `STACK_SESSION_ID` is unset and `kit open` takes its unrecorded path (§4.1).
File-tool rules (`Read`, `Grep`, …) apply on every OS.

## 3. Library Choices

A check that judges another language or format (TOML, bash, `gh` argv, GraphQL, JavaScript) is built on what that
language's own parser returns; hand-written text analysis covers only what no parser reports (local paths in prose,
Git Bash path forms, the PowerShell subset).

| Need | Choice | Alternatives considered | Reason |
|---|---|---|---|
| TOML | `github.com/BurntSushi/toml` | `pelletier/go-toml/v2` | Both TOML 1.0 and both report error positions (`ParseError`; Pelletier's `DecodeError.Position()`); BurntSushi also gives `MetaData.Keys()` in file order (exact-key unknown-key warnings and `[handover]` order) and `Primitive` per-key decoding, with no transitive deps. |
| Bash command analysis | `mvdan.cc/sh/v3/syntax` | legacy regex splitting (`guard-agent-actions.py:53-66`) | The regex split breaks inside quotes and loses heredoc bodies. The `shfmt` AST gives calls, assignments, redirects, heredocs and `$(…)`. `Word.Lit()` returns only unquoted literals, so `shell` evaluates a word as static when it consists of `Lit`, `SglQuoted` and `DblQuoted` parts holding only literals; any expansion makes the word dynamic. Parsing follows bash's input unit: bash reads and executes a whole input line (with any heredoc bodies it opens) before reading the next, and a syntax error anywhere in a line stops that line from running. So the command is parsed whole; on a syntax error it is re-parsed up to the end of the last complete line before the error line (heredoc bodies attached), and only those statements are judged; the error line and later text are not judged as calls. A command bash rejects is first parsed again as zsh, the
shell the Bash tool may run (§4.3). No regex fallback (it would restore the false positives). |
| PowerShell command analysis | hand-written recognizer (`internal/powershell`) | pwsh's `Parser.ParseInput` in a child process; `mvdan.cc/sh` | Approved exception to the parser rule: PowerShell's parser exists only inside pwsh, a process start per hook call and a runtime dependency; bash parsing misreads backticks, `&` and script blocks. The recognizer tokenizes as PowerShell does, judges only listed command positions, fails open, and is checked against PowerShell 5.1 and 7 (powershell-guard §3). |
| `.kits` lock | `github.com/gofrs/flock` | hand-rolled `LockFileEx` / `flock(2)` behind build tags; `O_EXCL` lock file with stale reclaim | An OS lock is released when the process exits, so no time-based reclaim (which could break a live writer) is needed. flock wraps both OS calls behind one API with `TryLockContext`, is widely used, and keeps this repo free of build-tagged lock code; hand-rolling it would duplicate that for no gain. |
| `gh` argv (`ghargs`) | `github.com/spf13/pflag` + hand-written flag table | hand-written pflag-like parser; importing `cli/cli/v2` commands | gh parses argv with cobra/pflag, so pflag matches it on combined shorthands, `-b=x`, `--name=<bool>`, a value flag taking a following `-x`, and `--`. `cli/cli/v2` pulls in ~170 modules and pins gh's flags to our version, not the installed gh. The table lists name, shorthand, bool/value and repeatability for every flag of the inspected subcommands as of gh v2.105.0 (§4.3). |
| GraphQL documents (raw merge) | `github.com/vektah/gqlparser/v2/parser`, no schema | hand-written lexer; `graph-gophers/graphql-go` (server, needs a schema); `graphql-go/graphql/language/parser` (standalone, no schema) | `ParseQuery` parses a document without a schema and keeps `Field.Alias` apart from `Field.Name`, with fragments and inline fragments, so block strings, escapes, comments and aliases are the spec parser's job. The compiled packages (`parser`, `lexer`, `ast`, `gqlerror`) import nothing outside the module. `graphql-go/graphql`'s parser would also fit (no schema, no deps) but its last release is from 2023, while gqlparser is released regularly and backs gqlgen. |
| JavaScript (Pencil rule) | `github.com/tdewolff/parse/v2/js` | hand-written lexer; `dop251/goja/parser` (heavy deps); esbuild (not importable) | Telling a regex literal from division (`/"/; Generate()`) needs parser context a string/comment lexer lacks. `js.Parse` + `js.Walk` give call expressions, incl. inside nested templates; `js.Lexer` serves the best-effort parse-error fallback (§4.3). No runtime deps. |
| CLI flags (`stack` itself) | stdlib `flag` + an interspersed-args helper | `spf13/pflag`, `spf13/cobra` | Seven commands, few flags; `stack pr merge 12 abc --squash` needs flags after positionals. The helper keeps a value flag's next token as its value even when it starts with `-` (`--body "--x"`) and treats every token after `--` as positional. Kept for now although `ghargs` brings in pflag, which handles both natively; replacing the helper with pflag is a separate refactor decided after PR 4. Cobra is too much for seven commands. |
| JSON | stdlib `encoding/json` (+ `bufio` for JSONL) | `encoding/json/v2` | v1 is stable on the pinned toolchain; decode only the fields read, `tool_input` as `json.RawMessage`. |
| Local paths in text (`publictext`) | hand-written, stdlib `regexp` (RE2) + `unicode` boundary checks | gitleaks/betterleaks rules, trufflehog (AGPL) | Secret scanners detect credentials and use home paths only as allowlists; no library detects home-directory paths. RE2 `\w` and `\b` are ASCII-only, so the token-start rule of public-check is checked on the preceding rune with `unicode.IsLetter/IsDigit` and `資料/home/demo` keeps passing as in Python (`public-check:32`). Parity vectors: `dotfiles:tests/test_kaizen_public_check.py`. |
| Git Bash path forms (`paths`) | hand-written | MSYS2 `cygpath` (runtime tool) | No maintained Go library converts MSYS paths; shelling out adds a runtime dependency to hook latency. Rules follow MSYS2's conversion of `/` and `/<drive>` (§2.1). |
| Fake `gh`/extensions in tests | TestMain re-exec (`proctest`) | bash scripts on PATH (legacy tests); stdlib helper-process pattern; `rogpeppe/go-internal/testscript` | Bash fakes do not run on `windows-latest`; the test binary copied as `gh.exe` does. The helper-process pattern needs the test to build the argv, but fakes are found by path and must survive `Cmd.Env` replacement; testscript sets a process-wide `PATH` and has no per-path scenarios, call log or grandchild. |

`git` is real in tests (present on all three runners); `gh` is always faked.

## 4. Command Contracts

Exit codes, all commands: `0` ok, `1` refused / finding / block, `2` usage, input validation, config or environment
error. Hook commands always exit `0`.

### 4.1 `stack kit open <slug>` / `stack kit close <slug>`

- Inputs: `$STACK_SESSION_ID`, config, kit dir.
- `open`: validates slug (exit 2); creates `<root>/<slug>/` (0o700) if absent, `<root>` resolved as in §2.1 (a
  relative one against the config file's directory, never the cwd); prints the kit context (§4.1.1) to
  stdout. With a valid `STACK_SESSION_ID`, adds the slug to `sessions/<id>.kits` (one slug per line, de-duplicated,
  order kept). Without it (PowerShell sessions, environments without the hook): prints the context, writes
  `stack: STACK_SESSION_ID unset; <slug> not recorded and will not be restored after compaction` to stderr, records
  nothing, exits 0, so a handover still delivers its context.
- `close`: removes the slug from `<id>.kits` (idempotent; deletes the file when empty); never touches the kit dir.
  Without `STACK_SESSION_ID`: exit 2 (there is no record to change).
- After a successful `open`, `cli` runs the `entry` stage (§4.7) with `prompt = ""`, `range = ""` and appends each
  result's `context`; `block` there is shown, not enforced. `kit open` is the only path that runs entry extensions
  implicitly; `stack run entry` runs them explicitly.
- **Concurrency.** Every access to `<id>.kits`, read or write, holds an exclusive OS lock on `<id>.lock`
  (`gofrs/flock`, `TryLockContext` polling every 25 ms). The OS releases it when the holder exits, so there is no
  time-based stale reclaim; the lock file itself is never deleted. `open`/`close` wait up to 5 s; on timeout they
  exit 1 with `stack: <id>.kits is locked by another process` and change nothing (`open` then prints no context either,
  so the caller retries the whole command). A write goes to a temp file in the same dir, its handle is closed, and it is
  `os.Rename`d over the old file. Go documents `os.Rename` as atomic only on Unix; on Windows a rename fails while
  another handle is open, which the lock rules out because readers hold it too and close their handle before
  unlocking. Different ids never share a file or lock.

#### 4.1.1 Kit context renderer (shared with compact restore)

In order, each in a tagged block with its absolute path: `NORTH.md`, `state.md` (one-line size note over 3,000
bytes, `orchestration-context.py:187`), kit items from `<kit>/attention/items.json` (roles `orchestrator`/`all`;
malformed file = block omitted), the handover file selected by `[handover]` (§2.1; a relative path is resolved against the
config file's directory), and the board (`<kit>/bin/board`
via `ResolveExecutable`, cwd = kit dir, stdout minus a leading repeat of the state files, failures as a
`[board exit N: …]` marker; `orchestration-context.py:193-212`). A new kit (no `state.md`) prints the kit path and
"write state.md here". A file that cannot be read becomes `[unreadable: <path>]` and the other blocks still render.
`kit open` output is uncapped except the board's stdout, which keeps the runner's 1 MiB cap (§2.2) and
is followed by `[board output truncated]` when cut; compact restore applies the budget of §4.2.

### 4.2 `stack hook claude session-start`

Stdin: SessionStart payload. Env: `CLAUDE_ENV_FILE`.

1. **Export, every event.** For any `source` (`startup`, `resume`, `clear`, `compact`, `fork`, or a future value) with
   a valid `session_id`, when Claude Code provides `CLAUDE_ENV_FILE`, append one line
   `export STACK_SESSION_ID='<id>'`, preceded by a newline when the file does not end in one (so a last line
   without one cannot swallow the assignment). When `CLAUDE_ENV_FILE` is unset or empty (e.g. PowerShell sessions, §2.3), append
   nothing and continue (a compact still runs step 2); `kit open` then takes its unrecorded path (§4.1). Repeated lines for the same id are harmless
   (the last assignment is the value). Ordinary `--resume`/`--continue` keep the session id and so its `.kits`;
   `--fork-session` (`source: "fork"`) starts a new id with no kits. Nothing is printed except on `compact`
   (`docs/architecture.md:49,54`).
2. **Compact only.** Take the `<id>.lock` with a 1 s timeout, read `<id>.kits`, release, and render each listed kit
   whose dir exists. If the lock is not acquired, the kit part is omitted (`[kits omitted: .kits locked]`) and the
   worker part below is still restored. Independently, if the session is a worker (step 3), restore the worker body (frontmatter
   stripped, CRLF-tolerant) and the original brief (`orchestration-context.py:276-300`). A broken transcript
   loses only the brief; an unreadable kit loses only its blocks (`orchestration-context.py:288-300`). No extensions
   run. Boards run after everything else is placed (step 5), each only while its block can still fit; one that cannot
   is not launched and leaves `[board omitted: context limit: <path>]` when that fits. One deadline of 40 s covers all
   boards (`orchestration-context.py:47`); once it passes, no further board is launched and each remaining one gets
   `[board skipped: deadline]`. A board's leading repeat of the state of any restored kit is dropped, as legacy did.
3. **Worker detection** (`docs/architecture.md:57`), any of: a line of the first prompt starts with `[worker].marker`;
   `cwd` passes `IsAgentWorktree`; `agent_type` in `[worker].agent_types`. `agent_type` is an auxiliary signal: it is
   present when the session itself runs as that agent (`claude --agent …`); in-process subagents are not assumed to
   receive SessionStart. Detected without a readable brief: body only.
4. **First prompt** (port of `orchestration-context.py:258-273`): read the transcript JSONL with `bufio.Reader` (no
   line limit), trimming `\r`; skip records that are not `type: "user"` or are `isMeta`, `isCompactSummary` or
   `isSidechain`; `message.content` as a string is the prompt; as a block array, the `text` blocks joined by blank
   lines are the prompt; an array with no text block but a `tool_result` block is skipped (next record); any other
   array yields `""`. A malformed line ends the read with no brief.
5. **Budget: 9,500 chars** (`orchestration-context.py:43`), counting everything emitted: block tags, path headers
   and markers included. Chars are UTF-16 code units, the string length Claude Code compares with its 10,000-char
   hook output limit; cuts fall on code-point boundaries. Before any text is placed, the brief reserves
   `[brief omitted: context limit]`, a `.kits` failure its marker, and every `NORTH.md`/`state.md` block of every kit
   `[state omitted: context limit: <path>]` (bare kit markers such as `[new kit: …]` reserve themselves), so no kit or
   brief disappears silently; a reserved marker is replaced only by a longer rendering. Then, in priority order: (1) the worker rules body, whole when it fits, else cut at its tail with `[...cut N chars]` (legacy puts rules first);
   (2) the brief, verbatim if it fits in the remainder, else cut at its tail and ended with `[...cut N chars]`;
   (3) kits: first, per kit in `.kits` order, the block header and the first 600 chars of each of its
   `NORTH.md`/`state.md` blocks, so with several kits every kit keeps its start; then, again in `.kits` order, the
   rest of those blocks and the items blocks, each capped at 4,500 chars as legacy; (4) the handover block, which
   appears once per compact, not per kit, and the board blocks. Display order is rules, brief, then per kit its state,
   items and board blocks, then the handover. A block that does not fit is cut with the same marker, or keeps its
   omitted marker when not even its tags fit. A later pass never shrinks text an earlier pass placed.

Settings wiring (dotfiles): `SessionStart`, no matcher, exec form (argv, no shell) of the pinned Claude Code
version, `timeout: 60`, failure-continue (`onFailure: "continue"` where that version supports it) so a launch failure
or timeout is also fail-open.

### 4.3 `stack hook claude pre-tool-use`

Stdin: PreToolUse payload. Reads config and, for gh publishes and GraphQL queries, the files the command names.
Stdout: deny JSON or nothing. Rules are evaluated in table order; the first deny wins.

**Shell calls.** `shell.Parse(cmd, cwd)` parses by bash input lines (§3): on a syntax error it keeps the statements of the
complete lines before the error line, with their heredocs bound. The Bash tool runs the user's shell, so a command that
bash rejects is parsed again with mvdan's zsh dialect (`syntax.LangZsh`) and judged as zsh would run it; the error-line
cut applies only when both dialects fail. For each statement it returns each simple command
with: argv0 after stripping `env` (and its assignments/flags), `command`, `nohup`, `time`, `mise exec --` and leading
assignments; static or dynamic words, each static word marked when its leading `~` is unquoted; its stdin source (a
bound heredoc or here-string, or a literal `< F`); and its known cwd. The known cwd starts as payload `cwd` and changes
only across `cd X && …` with a static `X` (the rest of that `&&` chain runs only if `cd` succeeded); after
`cd X; …`, `cd X || …` or any non-static `cd` it is unknown. It recurses into `bash|sh|zsh -c '<literal>'`,
`eval '<literal>'`, a bash/sh/zsh call whose stdin is a literal heredoc, and `$(…)`/backticks, to depth 2. Quoted text
and heredoc bodies of other commands never become calls (`guard-agent-actions.py:62-66`). `Parse` also returns the
source ranges of **handoff substitutions**: a `$(…)` whose whole body is exactly one `cat F` or `< F` with a single
static operand and no other redirect, and which is (part of) the value word of a variable assignment (`X="$(cat F)"`,
`export X=…`, `env X=… cmd`) or of a flag (`--flag="$(cat F)"`, or the word right after a word starting with `-` other than `--`, before
any `--`, as in `--token "$(cat F)"` or `-H "Bearer $(cat F)"`), determined from the AST. Calls whose argv0 is
`echo`, `printf` or `cat` never get the flag-value form, so `echo -n "$(cat F)"` is not a handoff. Two stdin forms are
handoffs too: the whole word of a here-string (`cmd <<< "$(cat F)"`), and an argument of an `echo`/`printf` without
redirects whose output feeds a pipe (`printf '%s\n' "$(cat F)" | sudo -S …`); `echo`/`printf` writing to the terminal
or a file is not. A quoted heredoc fed to `bash`/`sh`/`zsh` keeps the handoffs of its script; a script passed as a
here-string (`bash <<< '…'`) does not, so a secret mentioned there is denied.

| Rule | Tools | Decision |
|---|---|---|
| **secrets** | `Read` `file_path`, `NotebookRead` `notebook_path`/`file_path`, `Grep` `path` + `glob`, `Glob` `path` + `pattern`, `Bash`, `PowerShell` (powershell-guard §4.2) | Paths are `Resolve`d against payload `cwd`. Deny when a path is `Within` a `~` entry (`~/.config/app/token`), has a component equal to a bare entry (`.dev.vars`), or, for `Read`/`NotebookRead`, its `EvalSymlinks` target does. Glob components deny when they equal a bare entry or contain its literal text and `path.Match` it (`.dev.vars*` denies; `*`, `*.example` pass). A `Grep.glob` field is split as Claude Code splits it (whitespace and commas outside `{…}`); each pattern starting with `!` excludes files and is skipped, each other one is judged. A Glob/Grep pattern that mentions an entry's text, as the Bash rule below reads it, also denies (legacy parity). With an unknown home a `~` entry matches any path holding its home-relative components. **Bash** (legacy rule, `guard-agent-actions.py:33-34,94`): any mention of a secret entry in the command text denies — a `~` entry by its home-relative part (`.config/app/token`), a bare entry as a token (`.dev.vars`, not followed by a name rune, so `.dev.vars.example` passes), both on component boundaries with the case rule of §2.1 — in words, quotes, heredoc bodies and any unparsable tail alike, because even a "harmless" listing can print a value. The only exemption is the text inside handoff substitution ranges (`shell.Parse` above). Everything outside those ranges, including redirections and nested calls, is scanned as usual, so `X="$(cat .dev.vars)" cmd < .dev.vars` denies. |
| **gh publish** | `Bash`, `PowerShell` (powershell-guard §4.3) | For each call whose argv0 basename is `gh`/`gh.exe`, `ghargs` parses global `-R/--repo`, resolves aliases, and collects the published inputs of the table below. Deny on a `publictext` local-path hit (`guard-agent-actions.py:69-90`). Body files are `Resolve`d against the call's known cwd and read whole without a size cap if they are regular files; unknown cwd, an unreadable file or anything else (a FIFO, a device, a directory: opening a FIFO would block) = that input skipped. Only the argument word naming a body file is excluded from scanning (it is not published, `guard-agent-actions.py:74-78`); any hit inside the file's content denies, even when it equals the file's own path. |
| **raw merge** | `Bash`, `PowerShell` (powershell-guard §4.1) | Deny `gh pr merge` unless help is effectively shown or `--disable-auto` is effectively true. Deny `gh api` with effective method `PUT` whose endpoint, normalised as `gh` does (leading `/` stripped, so `/repos/…` and `repos/…` are one endpoint; an absolute URL's scheme, host and Enterprise `api/v3/` prefix and any query string are stripped too), is `repos/<o>/<r>/pulls/<n>/merge`. Deny `gh api graphql` whose query (inline or `@file`), parsed by `gqlparser`'s `parser.ParseQuery` without a schema, has a `mutation` operation whose selection set, following fragment spreads and inline fragments of the same document, selects a field whose name (not alias) is `mergePullRequest` or `enablePullRequestAutoMerge`. A query, a comment, a string or an alias that mentions the name passes. A query that `gqlparser` cannot parse is not judged (allowed). This is a local decision: the rule is a static guard against the agent's own mistakes, and that GitHub also rejects every document `gqlparser` rejects is assumed, not verified. `GH_REPO` does not change the decision. Reason names `stack pr merge <pr> <head-sha> [--merge\|--squash\|--rebase]`. |
| **rmroot** | `Bash`, `PowerShell` (powershell-guard §4.4) | **Bash** (this cell; `PowerShell` follows powershell-guard §4.4 instead: `Remove-Item` binding, FileSystem-provider paths, and native `rm` with PowerShell's native-argument rules, no Git Bash forms): deny a call whose argv0 basename is `rm` with an effective recursive flag (`-r`, `-R`, `--recursive`, incl. combined shorthands such as `-rf`; option parsing stops at `--`, after which every word is an operand) when any static operand, `Resolve`d against the call's known cwd, is `/`, the user's home, a drive root, or an existing **directory** whose last component is `.git`. `~`/`~/` count only unquoted (`rm -rf '~'` removes a directory named `~` and passes). On Windows the literal `/` (Git Bash root) counts as a root, `/c`, `/c/` resolve to drive roots as in §2.1, as do `C:/` and quoted `'C:\'`. A linked worktree's `.git` file does not match, and a relative operand under an unknown cwd is not judged. No child paths (`.git/hooks`, `~/src`), globs (`/*`) or scripts. |
| **pencil** | `mcp__pencil__execute` | Unless `deny_pencil_generate = false`: deny when the executed code contains a call expression whose callee name — an identifier, or the property of a member expression — starts with `generate`, case-insensitively (`Generate(…)`, `x.Generate(…)`, `generateImage(…)`). This keeps the legacy coverage (`\bgenerate\w*\s*\(` with `re.IGNORECASE`, `guard-agent-actions.py:42`) while judging only real calls; a user helper named `generate*` is denied as before, and the reason tells the agent to rename it. `input` is wrapped as `(async function(){\n…\n})` so top-level `return`/`await` parse, then parsed with `tdewolff/parse/v2/js` and walked, which reaches calls inside `${…}` of nested templates. So `Print("Generate()")`, `// Generate()` and `/x/.test(s)` pass and `Generate({…})` and `/"/; Generate()` deny. **Token fallback** (on a parse error, and for every `edits[].replace`): the same package's `js.Lexer` token stream; deny on an identifier token starting with `generate` (case-insensitive) followed by `(`, outside string, template and comment tokens. It is best-effort where `/` is ambiguous: `Lexer.Next` emits `/` as division and only the parser rescans it as a regex in expression context, so the fallback rescans with `Lexer.RegExp` only when the previous significant token cannot end an expression (start of input, an operator or opening punctuator, or a keyword such as `return`); after `)`, `]`, `}` or an identifier it stays division, and a regex there (`if (x) /Generate(a)/.test(s)`) is read as code and may deny. **`edits[].replace` coverage limit:** Pencil applies each `find`/`replace` to the failed snippet and reruns it, but the payload does not carry that snippet, so the patched code cannot be parsed; each `replace` is judged on its own with the token fallback. A replacement that closes an existing string, inserts a call and opens the next string (`"); Generate(x); ("`) lexes as string contents in isolation and passes, where the legacy raw-text regex denied it. `description`, `uri` and other tools are not inspected. Field names are those of the pencil MCP `execute` schema (`input`: JavaScript snippet; `edits`: `find`/`replace` patches). |

**`ghargs` flag model.** Each inspected subcommand's argv is parsed with `spf13/pflag` (gh's own parser, via cobra)
over a hand-written flag table listing every flag of that command (name, shorthand, bool or value, repeatable), so no
known flag can swallow a value. The table models gh v2.105.0; PR 4 checks it against that version's flag
declarations. Repeatable flags are registered as gh registers them (`gh api` `-f/--raw-field` and `-F/--field` are
`StringArray`), and every value is scanned, so a clean second `-f` cannot hide a local path in the first. Semantics:
`--name=value`; `--name value` only for value flags; combined shorthands (`-sd`, where a value flag letter takes the
rest or the next token); booleans accept `--name=<bool>` with `strconv.ParseBool` values; every token after `--` is
positional. A token is only a flag where the model says so: `--help` as the value of `--subject` is a value. A flag
missing from the table (a newer gh) makes pflag stop with an unknown-flag error; the call is then denied if any
published input collected before the error has a hit, and otherwise every remaining word is scanned as literal text
for local paths, so parsing never silently stops before a later guarded input. Help is effective when `-h`/`--help`
parses as a true boolean flag (`-h=false` and `--help=false` are not help); `--disable-auto` likewise
(`--disable-auto=false` merges). An effective help skips every rule for that call (gh only prints help), and an
effective `pr create --dry-run` skips its publish inputs (gh prints the pull request and returns before creating it). `gh api` method: `-X/--method` value, else `POST` when any field or
`--input` is given, else `GET`.

| gh command (aliases) | Published inputs | File / stdin inputs |
|---|---|---|
| `pr create` (`pr new`), `issue create` (`issue new`) | `-t/--title`, `-b/--body`; the alt text after `#` in each `--attach` value (it becomes image alt text in the body), on every command here that gh v2.105.0 gives `--attach` (`pr`/`issue` `create`, `edit`, `comment`) | `-F/--body-file F`; `F` = `-` → call stdin |
| `pr edit`, `issue edit` | `-t/--title`, `-b/--body` | `-F/--body-file` |
| `pr comment`, `issue comment`, `pr review` | `-b/--body` | `-F/--body-file` |
| `release create` (`release new`), `release edit` | `-t/--title`, `-n/--notes` | `-F/--notes-file`; asset operands excluded |
| `api` with effective method other than `GET` | `-f/--raw-field` and `-F/--field` values | `-F k=@F` (`@-` → stdin), `--input F` (`-` → stdin) |
| `api graphql` (merge rule only) | `query` from `-f`/`-F` | `-F query=@F`; `--input F` JSON |

Excluded: read-only `gh api` (`GET`, no fields), `--jq`/`--template`/`-H` values, `-R` values, release asset paths,
the file path of an `--attach` value (before `#`).
Call stdin is the bound heredoc/here-string or literal `< F`; a pipe is dynamic and skipped. `--fill`,
`--fill-first` and `--fill-verbose` copy commit text: covered only when `check public` ran on the same range before
publishing (the commit-pr skill's order), not by this hook.

Settings wiring (dotfiles): matcher `Bash|PowerShell|Read|NotebookRead|Grep|Glob|mcp__pencil__execute`,
exec form, `timeout: 5`, failure-continue as in §4.2.

### 4.4 `stack pr merge <pr> <head-sha> [--merge|--squash|--rebase] [--delete-branch] [--subject S] [--body B] [--allow-no-checks] [--auto]`

Ports `~/.claude/bin/merge-pr` minus the owner-only list (`merge-pr:82-159,178-183`). Run
inside a checkout of the PR's repo; allowed from agent worktrees too (approval is the caller's duty). Step 1 failures
exit 2; later refusals exit 1 with a one-line reason on stderr.

1. `<pr>` decimal; `<head-sha>` 7–40 hex (`merge-pr:21-22`); `GH_REPO` unset (`merge-pr:23`); at most one method;
   `--subject`/`--body` contain no `publictext` local path and no configured private term.
2. `git rev-parse --show-toplevel`; running inside a checkout is a precondition, so when it fails or prints nothing
   (e.g. started in the `.git` directory) the merge is refused before any `gh` call. `gh` runs with this cwd
   throughout.
3. `gh pr view <pr> --json headRefOid,baseRefName,state,isDraft,mergeable`, retried 5× at 3 s while
   `mergeable == UNKNOWN` (`merge-pr:42-46`); `headRefOid` starts with `<head-sha>`; `OPEN`; not draft;
   `MERGEABLE`. From here on the **full** `headRefOid` (`H`) is the checked SHA.
4. Method: given, or the single one `gh repo view --json nameWithOwner,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed`
   allows (`merge-pr:54-61`).
5. Non-squash: no `fixup!`/`amend!`/`squash!` headline in `gh pr view <pr> --json commits` (`merge-pr:62-67`).
6. `<root>/.claude/merge-check <pr> <H>`: absent → continue; present but not runnable on this OS (incl. a
   non-executable file on Unix) → refuse; non-zero → refuse (`merge-pr:69-71`). A placed check is treated as
   required; legacy skipped a non-executable one.
7. `gh api graphql … isMergeQueueEnabled` must be `false` (`merge-pr:173-176`).
8. Checks of `H`, read last, right before the merge, so a rerun during the earlier steps is seen: `gh api graphql`
   with `repository(owner,name){ object(oid: H){ ... on Commit { oid statusCheckRollup { contexts(first: 100, after: $c) { totalCount pageInfo nodes { CheckRun{name status
   conclusion} StatusContext{context state} } } } } } }`, paging to the end. The response `oid` must equal `H`. Every
   CheckRun must be `COMPLETED` with conclusion `SUCCESS`, `NEUTRAL` or `SKIPPED`; every StatusContext `SUCCESS`.
   A confirmed empty set (commit found, rollup `null` or `totalCount 0`) refuses unless `--allow-no-checks`; any query
   error refuses. Chosen over `gh pr checks` (reads whatever head the PR has at that moment and returns no SHA) and
   over REST `commits/{sha}/check-runs` + `/status` (two paginated calls) because one query is pinned to `H` and
   covers both check runs and commit statuses (`merge-pr:161-170`).
9. Without `--auto`: `gh pr merge <pr> <method> --match-head-commit H [--delete-branch] [--subject S] [--body B]`; GitHub rejects the
   merge if the head moved after step 8. Then, **whatever its exit code**, read back
   `gh pr view <pr> --json state,headRefOid,mergeCommit` (`merge-pr:189-207`) on its own 30 s deadline, also when
   SIGINT/SIGTERM (which cancel the context and kill the running child) arrived after `gh pr merge` started.
   `MERGED`, `headRefOid == H` and a merge OID that is a full SHA (40 or 64 lower-case hex) → stdout
   `MERGED <H> <merge-oid>`, exit 0, plus a stderr note if `gh pr merge` exited non-zero (e.g. branch deletion failed
   after the merge), so the caller does not retry. `OPEN` → exit 1, not merged. A failed read-back or any other
   state → exit 1 with `result unknown — check the PR before retrying`, never an invitation to retry.

10. `--auto` (for repositories whose rules only let GitHub merge after merge intent, e.g. a required status that an
   automerge gate posts on `auto_merge_enabled`; an admin bypass is not used). Steps 1–8 run unchanged, and step 3
   also reads `autoMergeRequest`: one already active at preflight (e.g. the owner's) is refused. No lock: callers
   must not run concurrent `--auto` attempts on the same PR. Then `gh pr merge <pr> <method> --auto --match-head-commit H …`: gh either merges
   directly (merge state `CLEAN`/`HAS_HOOKS`/`UNSTABLE`) or enables auto-merge, both with `expectedHeadOid: H`, so a
   head other than `H` at submission is rejected by GitHub. Read-back decides the outcome, never gh's exit code:
   - Every read here fetches `state`, `headRefOid`, `mergeCommit` and `autoMergeRequest` (explicit `null` = none;
     a missing field is unreadable). Read immediately after gh returns (any exit code, also after a cancel), then every 20 s, under one absolute 15-minute deadline that
     also bounds each read; an unreadable or malformed read is retried within the deadline (`merge-pr:185-198`).
   - `MERGED` + `H` + full merge OID → `MERGED <H> <oid>`, exit 0 (stderr note on a non-zero gh, as in step 9).
   - `MERGED` with another head → exit 1 naming both SHAs: inspect the PR (a merge cannot be undone).
   - Stop conditions, checked after a read that is not a merge: a non-zero gh exit; `OPEN` on `H` with no
     `autoMergeRequest` (nothing will merge it); a head other than `H` while `OPEN`; `CLOSED`; the deadline; or
     SIGINT/SIGTERM after gh started. Each attempts `gh pr merge <pr> --disable-auto` on a fresh 30 s context, then read back
     once more on its own 30 s deadline, whatever the disable returned. That read wins: `MERGED` is judged as above;
     `OPEN` with no `autoMergeRequest` → exit 1 `not merged; auto-merge disabled`; anything else →
     exit 1 `result unknown — auto-merge may still be enabled; check the PR before retrying`.
   - `--delete-branch` acts only on gh's direct-merge path; after a deferred merge the branch is left to the
     repository's auto-delete setting.

   **Accepted gap:** while auto-merge is active, GitHub can merge a later head that a writer pushed; GitHub disables
   auto-merge only on a push by a user without write access, and the gate re-evaluates the new head. Accepted because
   in this owner's repositories only the owner, their agents and their bots push to PR branches, the first read that
   sees a moved head triggers a disable attempt, and a merge of any other head is reported as a failure. Step 8 is a snapshot
   of the contexts already on `H`; GitHub enforces the required contexts at merge time. If a gate run already posted
   failure on `H`, rerun the failed CI and the gate for `H` (or push a new head) before retrying. The raw-merge rule
   keeps denying `gh pr merge --auto` and `enablePullRequestAutoMerge` outside `stack pr merge`.

No config needed. Not done: owner-only list, chezmoi lookup, shallow-unshallow, base-moved recheck (all served the
list), and the delegated-worktree refusal.

### 4.5 `stack check public <base>..<head> [--title-file F] [--body-file F] [--branch B]`

Ports `dotfiles:scripts/kaizen/public-check` (`:5-20,118-164`). Range must be `<base>..<head>` with non-empty ends
not starting/ending in `.` (`:130-132`). For each commit from `git rev-list --reverse <range>`: message
(`git show -s --format=%B`), destination paths (`git show --format= -M --no-relative --diff-merges=first-parent
--name-only -z --diff-filter=ACMRT`), and added lines of `--patch --text --no-color --no-ext-diff --no-textconv`
(lines split on `\n` only, so a bare CR or U+2028 stays inside the line, `:98-108`). Plus title, body, branch. Git
output is consumed with `proc.Stream` line by line: no size cap, so a large lockfile diff is scanned in full, and
nothing passes because output was cut.

Matched: `publictext` local paths and each line of `[safety].private_terms` (case-insensitive substring; `#` lines
and blanks ignored; file missing = no terms; present but unreadable, a directory, or not UTF-8 = exit 2,
`:51-60,92-95`). Title and body files must be UTF-8 (exit 2 otherwise). File arguments are `Resolve`d (§2.1), so the
`/c/…` and `~` forms Git Bash passes open the real file. Output: each hit once, in order, on stderr as
`check public: <where>: local path '<hit>'` / `private term '<term>'`; exit 1 if any. Git failure: exit 2.

`publictext` local paths (POSIX from `:32-33`, Windows added). A hit starts a token (start of text, or the preceding
rune is not a Unicode letter/digit and not `.`, `-`, `_`) and has a name component of Unicode letters/digits and
`._-` ending at `/`, `\` or a non-name rune:
- `/Users/<name>`, `/home/<name>`, `/Volumes/<name>`;
- `C:\Users\<name>` and `C:/Users/<name>` (any drive, case-insensitive, one or more `\`, so JSON-escaped `\\` too);
- `/c/Users/<name>` and `/mnt/c/Users/<name>` (Git Bash / WSL forms);
- `/tmp/claude-<digits>/`, macOS per-user temp (`/private/var/folders/…`).

Only syntactically identifiable placeholders are not names: a name component that is exactly `...` or `…`, or that
starts with `<` (`<name>`; `{…}`, `$…`, `%…%` already start with a non-name rune). Any other name matches, including
a one-character one (legacy parity), so tests and docs build real-name-shaped literals at run time or use
`<name>`. Each form has its own test case. `publictext.TestRepoTree` runs the matcher over `git ls-files` plus
`docs/design/phase2-core.md` named explicitly (it is committed before PR 1, after which it is also tracked); all must
pass, `docs/architecture.md:10` included. The `.t3/{scratch,worktrees,userdata}/` fragments of the legacy guard are not
in the core matcher: they match this repo's own §5 defaults; the dotfiles private-terms file can list them.

### 4.6 `stack budget status`

Display only, exit 0 (2 on usage). For each kit under the kit root (marking those in `$STACK_SESSION_ID`'s `.kits`):
bytes of `NORTH.md` / `state.md` with a note over the 3,000-byte threshold, and item counts from
`attention/items.json` in one column per role present plus an `all` column (`role: all` items are counted there, so a
register holding only `all` items shows them; `attention-budget.py:80-86`). No caps file, no ratchet, no `pre` hook,
no non-zero exit for "over" (`docs/architecture.md:36`). Kit root missing: prints "no kits". The global register
`~/.claude/attention/items.json` and prose sizes are out of scope: legacy `attention-budget.py status` keeps serving
kaizen until Phase 3; `--json` is decided together with the `statusline.sh.tmpl` caller.

### 4.7 `stack run <entry|outbound|cycle> [--dry-run] [--replay FILE] [--ext NAME]... [--range R] [--prompt-file F]`

v11 adds the `merged` stage, the `kits`, `title`, `body`, `branch` and `merge` request fields, and `block` demotion
at stages that cannot block, with its compatibility exception (`docs/design/core-stages.md` §2, §4.1, §4.9).

- **Selection:** names from `[stages].<stage>` (defaults §5), or exactly `NAME` with `--ext` (repeatable).
- **Names:** each name (config or `--ext`) must be a single file-name component matching `^[a-z0-9][a-z0-9-]*$`;
  anything else (`../x`, `a/b`, `C:x`) is a config error or a usage error (exit 2).
- **Discovery:** `stack-<name>` on `PATH` via `proc.LookPathExe` (§2.2; the `gh-<name>` / `git-<name>` convention),
  never as a direct path and never through `PATHEXT`. Not found = **skip**, listed under `skipped`, no warning
  (`docs/architecture.md` §3 Extension Protocol). Found but not runnable (§2.2) or failing to start = `warn`. Core never looks inside
  `extensions/`. An installed `jev-*` without its key or config must itself answer `pass` with no context: v10 §4's
  silent opt-out is the extension's contract.
- **Request (stdin):** `{"stage","repo","range","prompt"}` (`docs/architecture.md` §3 Extension Protocol). `repo` = repo toplevel or
  `""`; `range` = `--range R`, else for outbound `@{upstream}..HEAD` when an upstream exists, else `""`; `prompt` =
  contents of `--prompt-file F` or `""`. An explicit `--range` that git cannot resolve or an unreadable/non-UTF-8
  `--prompt-file` exits 2. Env adds `STACK_STAGE` and passes `STACK_SESSION_ID` through.
- **Execution:** sequential, stage order; cwd = invoker's cwd; timeout per stage (entry 10 s, outbound 120 s,
  cycle 30 min); stdout cap 1 MiB; stderr passed through prefixed `[<name>] `.
- **Response (stdout):** `{"status":"pass|warn|block","context":string,"findings":[…]}`; `findings` kept as raw
  JSON. Evaluation order: `TimedOut`, `WaitDelayExceeded`, `Truncated` → **warn** even if the bytes received so far
  are valid JSON; only then parse. Non-JSON, trailing garbage, unknown status, crash without a valid response →
  **warn**. Every warn core produces carries a synthetic finding `{"extension":<name>,"error":<reason>}`. The exit code is ignored
  when stdout is a valid response.
- **Aggregation:** `block > warn > pass`; no result (all skipped) = `pass`. Stdout:
  `{"stage","status","results":[{"name","status","context","findings","error"}],"skipped":[…]}`. Exit after writing the aggregate: `pass`/`warn`
  0, `block` 1. Warn-not-block for broken extensions and listing `skipped` are design choices of this doc.
- `--dry-run`: print the resolved plan (`name → path | skipped | not runnable`) and the request, run no extension,
  exit 0. Git reads needed to build the plan are allowed.
- `--replay FILE`: FILE is `{"request":{…},"expect":{"status":…}}` (`expect` optional); the request is sent verbatim
  instead of being built from the environment. `request.stage` differing from the CLI stage exits 2. With `expect`,
  exit 1 when the aggregate status differs. Fixtures for real extensions live in `fixtures/<ext>/*.json` (Phase 4);
  core tests use `internal/ext/testdata/`.

## 5. Legacy Parity

| Legacy behaviour (source) | v10 | Why |
|---|---|---|
| Secrets deny for Bash/Read/Grep/Glob/NotebookRead (`guard-agent-actions.py:21,33-38,94,124-127`) | **Changed**: config-driven list; component boundaries (`secrets-example` passes); case folded only on darwin/windows; relative paths against `cwd`; `Grep.glob` added; Bash keeps the legacy any-mention rule, with the `$(cat F)` exemption narrowed to an AST-identified single `cat F`/`< F` substitution that is an assignment or flag value, the whole word of a here-string, or an argument of a redirect-free `echo`/`printf` feeding a pipe (the owner's `sudo -S` password workflow: the parity replay's only new secrets denies were these forms); Glob/Grep patterns are also scanned as text; symlink target for Read | §1 item 1, §5 `[safety].secrets` |
| Local paths in gh write command/body files (`guard-agent-actions.py:39,69-90`) | **Changed**: per-command published inputs instead of all words; adds `pr new`/`release new`, `--attach` alt text (every `pr`/`issue` command that takes it), every value of repeated `-f`/`-F`, `-R` before subcommand, bound heredoc/`< F` stdin, `--notes-file`, `-F k=@f`, `cd X &&` resolution, Windows paths; hits inside a body file deny even when equal to its own path; excludes asset paths and read-only `gh api`; shared matcher with `check public` | §1, §3 pre-tool-use |
| Raw `gh pr merge` deny (`guard-agent-actions.py:40-41,99-103`) | **Kept**, on effective flags; `--disable-auto` allowed (intentional change: legacy denied it, but gh returns after disabling auto-merge without merging); `gh api` PUT merge (with or without leading `/`, or as an absolute URL), calls on a line only zsh parses, and GraphQL merge mutations (incl. `query=@file`; parsed with gqlparser; mentions in strings, comments, aliases or read queries pass) added | §3; same hazard |
| `deny-rm.sh` broad rm deny (dotfiles, removed in Phase 1) | **Dropped**; narrow `rmroot` (`/`, home, drive root, a `.git` directory itself) added | §1 item 2 vs. §1 drop list |
| Delegated-worktree deny of git push / gh writes / wrangler / publish (`guard-agent-actions.py:22-32,97-115`) | **Dropped** | §1 (no broad git/deploy denies), §4 S2 retires `~/.t3/worktrees/` guess |
| Pencil Generate deny on any `mcp__pencil__*` JSON (`guard-agent-actions.py:42,128-132`) | **Changed**: on by default, `false` disables; only executed code of `mcp__pencil__execute`, a parsed call whose callee name starts with `generate` (identifier or member property, case-insensitive); unparsable `input` and each `edits[].replace` judged by the best-effort token fallback, so a call that a replacement inserts between string fragments of the unseen snippet passes (legacy denied it) | owner decision |
| SessionStart injection on startup/resume/clear (`orchestration-context.py:14-17,288-302`) | **Dropped**; every event (incl. `fork`) only exports `STACK_SESSION_ID` | §4 S1, T0 |
| cwd/ancestor kit discovery, `worker-signals.json` (`orchestration-context.py:94-145`) | **Dropped**; `<session_id>.kits` via explicit `kit open` | §4 S1 |
| Compact restore of kit state/items/board/handover (`orchestration-context.py:219-255`) | **Kept** for kits listed in `.kits`; no extensions; `os.Root` stops following out-of-kit symlinks | §3, §4 S1 |
| Built-in T3/herdr handover text (`orchestration-context.py:55-75`) | **Changed**: text from `~/.config/stack/handover/*.md`, rewritten to use `stack kit open`; `HERDR_ENV=1` exact, T3 first | §4 T3, §5 |
| Global register items + budget line + macro QUESTIONS in injection (`orchestration-context.py:49-52,160-173,222`) | **Dropped** (instruction-plane text belongs in skills) | §1, §4 S2 |
| Compact WORKER_LINE restore, first-prompt selection (`orchestration-context.py:258-285`) | **Kept** incl. tool_result skip and block arrays; marker from config; plus worktree/`agent_type` signals; explicit truncation priority | §4 worker detection |
| Listed-worker rules injection (`orchestration-context.py:223-228`) | **Dropped** | depends on `worker-signals.json` |
| Stop mtime block (`orchestration-context.py:314-330`) | **Dropped** | §4 S1 retired |
| Never fail a session (`orchestration-context.py:23,341`) | **Kept** for both hooks; new for the guard (legacy raised on bad JSON) | constraint |
| `merge-pr` head/state/draft/mergeable/method/fixup/checks/merge-queue/merge-check/readback (`merge-pr`) | **Kept**; checks pinned to the full head OID; readback after non-zero `gh pr merge`; non-runnable merge-check refuses | §3 pr merge |
| `merge-pr --auto` wait and disable (`merge-pr:74-80,192-205`) | **Kept** (§4.4 step 10): submission pinned to `H`; 15-min read-back wait; disable then re-read on a moved head, `CLOSED`, timeout or cancel; an existing auto-merge request is refused; a merge of another head is reported, not prevented | dotfiles needs merge intent for its required gate status, and the owner cannot approve their own PRs |
| `merge-pr` `.claude/kaizen-managed` + chezmoi + unshallow + base-moved recheck (`merge-pr:82-159,178-183`) | **Dropped** | §1 (owner-only merge list) |
| `merge-pr` refuses in delegated worktree (`merge-pr:24-26`) | **Dropped** | §4 S2 |
| public-check range/message/paths/added lines/title/body/branch, private terms (`public-check:118-164`) | **Kept**, streamed; `--title` → `--title-file`; Windows paths; Unicode boundaries; one-character names still match; `.t3` fragments to private terms | §3 check public |
| public-check `--host-toml` values (`public-check:76-89`) | **Dropped** from core; dotfiles moves the values into private-terms; legacy script stays for kaizen callers | §5 has only `private_terms` |
| Budget `status` (`attention-budget.py:158-173`) | **Changed**: kit-based counts with an `all` column, exit always 0; global register/prose out of scope | §3 display-only |
| Budget `ratchet`, `pre` hook, self-protection (`attention-budget.py:176-245`) | **Dropped** | §1 |

## 6. Hazard Inventory

| Hazard class | Where it appears | Coverage |
|---|---|---|
| `~` / home forms | config values, `--body-file ~/x`, `rm -rf '~'`, secrets entries, worker body, Git Bash `/`, `/c`, `/c/Users/…` | `paths.Expand` for config (USERPROFILE on Windows); shell words expand only an unquoted `~`; `paths.Resolve` maps Git Bash forms before its own absolute-path check; `~user` untouched |
| Separators / case | payload paths on Windows (`C:\…`), case-insensitive FS | component comparison; case fold on darwin/windows only |
| Symlinked secret dir | `Read` of a link into the secrets dir | `EvalSymlinks` on Read/NotebookRead. Bash use through a link is a static-scope gap. |
| CRLF | worker body frontmatter, `.kits`, private-terms, handover files, transcripts | readers trim `\r`; added-line splitting keeps bare CR by design; temp repos use `core.autocrlf=false` (Windows runners default to `true`) |
| Invalid UTF-8 | body/title files, private-terms | `check public` exit 2; hook: scanned as bytes, never errors |
| Shell forms | `gh -R x pr merge`, `/opt/…/gh`, `gh.exe`, `env A=1 gh`, `mise exec -- gh`, `bash -c '…'`, `bash <<'EOF'`, `$(gh …)`, `a && gh …`, `cd d && gh …` | `shell` AST, parsed statement by statement (a later syntax error keeps earlier decisions) + argv0 basename + `ghargs` + recursion depth 2; cwd only across `cd X &&` |
| Out of static scope | `gh` aliases, `xargs`, scripts, `python -c`, `$VAR` words, `cat f \| gh … -F -`, the unparsable tail after a syntax error (secrets still scan it as text). `PowerShell`, all call rules: a script that fails to lex (secrets still scan it), constructs outside the recognizer's listed command positions (script blocks other than the listed ones, calls inside method-call arguments other than `$()`, parameter defaults, even of an invoked block: `& { param($x=$(gh …)) }`), `cmd /c`, `Start-Process`, .NET/COM calls, splatting, variables other than the known home and drive names, syntax errors outside lexing that the engine rejects (a bare command inside method-call arguments: its calls are still judged, over-judging a script that runs nothing), a native argument holding `"` under Windows PowerShell 5.1's legacy argument passing (the program receives it split or unquoted, not as the static word); raw-merge and gh-publish: `gh` reached only through those, a native home `~` file argument on Windows (the file gh opens depends on the engine version, powershell-guard §3 Words), a home `~` form whose value holds `"`; gh-publish: file-producing pipes (`Get-Content F \| gh … -F -`), `~` in inline text (`gh pr comment 1 -b ~\notes.md` publishes a literal `~`, which `publictext` does not match), `$env:USERPROFILE` in native argv on other hosts, cwd after `;`, `Push-Location` or a bare `cd`; rmroot: pipeline input to `Remove-Item`, globs (`C:\*`), provider-qualified, `\\?\`, UNC and non-FileSystem paths (`HKCU:\`), a `-LiteralPath` value starting with `~` other than `~` with trailing separators on Windows and a `~`, `~/…` or `~\…` one on other hosts (unmeasured), a native home `~` form whose value holds `"`, bare roots and paths on drives declared by `New-PSDrive` anywhere in the script (also before the declaration), `.git` forms that need a `..` collapse (`.git/hooks/..`); secrets: deliberate obfuscation (backtick in a name, built strings). Native argv is modelled for the measured forms in powershell-guard §3; other engine-specific argument forms after a quoted `--%` or adjacent to quote-started tokens are judged by their static text and may differ from the engine | documented, not guarded (§0.4, §2.3) |
| Publish sources | §4.3 table, `stack pr merge --subject/--body` | per-command inputs; `--fill*` only via prior `check public` |
| Merge bypasses | `gh pr merge` flags, `gh api …/pulls/N/merge`, GraphQL mutations (inline or `query=@file`) | denied after `gh` endpoint normalisation and a GraphQL parse (`gqlparser`); an unparsable query is not judged; a query from a pipe or variable is a static-scope gap |
| Head races | PR head moves between view, checks and merge | checks by OID `H`, `--match-head-commit H`, readback compares full `H` |
| Path traversal | slug, session id, `.kits` name, env-file line | `ValidSlug`, `ValidSessionID`, `os.Root` |
| Concurrent `.kits` writers | two processes with one session id | OS lock on `<id>.lock` (`gofrs/flock`, released on exit) held by readers and writers; temp file closed before rename; timeout = explicit failure, compact omits kits |
| Executables on Windows | kit board, merge-check, extensions | `.exe` only, explicit `<name>.exe` PATH walk (no `PATHEXT`); single-component extension names; board marker, merge-check refuse, extension warn |
| Hanging children | board, extensions, gh | context deadline + kill; runner-owned pipes closed 2 s after `Wait` returns (child exit observed, also after a timeout kill), reported as `WaitDelayExceeded` (descendants survive); compact stops launching at its deadline |
| Huge inputs | transcripts, body files, git patches, extension stdout | `bufio.Reader`/streaming (guard body files: a whole read of regular files only) with no pass-on-cap; extension stdout capped at 1 MiB → warn |
| Local paths in this repo's text | Go defaults (`~/.t3/userdata/secrets`), docs, tests | tilde forms are not matched; only `...`/`…`/`<name>` placeholders allowed; real-name literals built at run time (`dotfiles:tests/test_kaizen_public_check.py:15-17`); a test scans the tracked tree plus this document |

## 7. PR Split

Each PR passes `scripts/test` on Linux/macOS and `go vet` + `go test -race` + build on Windows (`.github/workflows/ci.yml`).

1. **feat: add paths, config, and CLI dispatch** — `internal/paths`, `internal/config` (+ BurntSushi/toml),
   `internal/cli` routing with usage/exit codes; `main.go` delegates. Done when: C1–C4 pass at the config level
   (defaults incl. `deny_pencil_generate = true`, syntax/version errors); `--version` unchanged.
2. **feat: add hook IO and process runner** — `internal/hookio`, `internal/proc`, `internal/proc/proctest`. Done
   when: C5 passes; a copied fake `gh.exe` runs on Windows CI; a timeout with a pipe-holding grandchild returns
   within 2 s of `Wait` returning after the kill, and a non-zero exit with a pipe-holding grandchild reports both the exit status and `WaitDelayExceeded`.
3. **feat: add check public** — `internal/publictext`, `internal/checkpub`, `stack check public`. Done when: the
   legacy public-check cases (`dotfiles:tests/test_kaizen_public_check.py:111-260`, minus every case that depends on
   `--host-toml` or the `--host-toml --private-terms` pair) pass as Go tables against real temp repos, plus C6–C8.
4. **feat: add pre-tool-use guard** — `internal/shell` (+ mvdan.cc/sh), `internal/ghargs` (+ spf13/pflag), `internal/guard`
   (secrets, gh publish, raw merge + vektah/gqlparser, rmroot), `stack hook claude pre-tool-use`. Done when: C9–C14 pass on golden
   payloads in `internal/guard/testdata/`, the CLI config fallback of C2 yields the secrets deny, and malformed stdin
   is silent.
5. **feat: add Pencil Generate deny** — `guard/pencil.go` (+ tdewolff/parse) wired to the PR 1 config key. Done when: C15 passes and
   the CLI config fallback of C2 yields the Pencil deny. `edits[].replace` is judged only by the best-effort token
   fallback (the patched snippet is not available), and §4.3 and §5 state that limit.
6. **feat: add kit open and close** — `internal/kit` (store, lock, renderer, board), `stack kit open/close`. Done
   when: C16–C17 pass.
7. **feat: add session-start hook** — `internal/session`, `stack hook claude session-start`. Done when: C18–C20 pass;
   C19's "no extension on compact" uses a fake `stack-<name>` on `PATH` that logs any call (PR 8 reruns it).
8. **feat: add extension protocol and run** — `internal/ext`, `stack run`, entry stage after `kit open`. Done when:
   C21–C22 pass.
9. **feat: add checked-head pr merge** — `internal/github`, `internal/merge`, `stack pr merge`. Done when: C23–C26
   pass.
10. **feat: add budget status** — `internal/budget`, `stack budget status`. Done when: C27 passes.

Order: PR 1–3, then PR 9 and PR 4+5 (PR 9 lands before the guard ships, because the raw-merge deny points to
`stack pr merge`), with PR 6 → 7 in parallel; then the dotfiles switch of both hooks. PR 8 and PR 10 are not
prerequisites of the switch.

Dotfiles switch (in `shuymn/dotfiles`, not this repo): wire both hooks and the merge wrapper call sites; ship
`~/.config/stack/config.toml` only if a default must change; place the existing private terms (including the
`.t3` fragments and the private `host.toml` values) in `~/.config/stack/private-terms` and the T3/herdr handover
texts, rewritten to use `stack kit open`, in `~/.config/stack/handover/`; re-open live kits explicitly (cwd discovery
is gone); run the parity replay (§8); then delete `guard-agent-actions.py`, `orchestration-context.py` and
`bin/merge-pr`, and unwire the `attention-budget.py` hooks. `attention-budget.py status` (used by `ready.py`, called
from `cycle.py`) and `public-check --host-toml --private-terms` (used by `improve.py`) stay until Phase 3 migrates
those callers.

## 8. Test Strategy

- **Unit, table-driven** for `paths`, `publictext`, `shell`, `ghargs`, `guard` rules, config decoding, protocol
  parsing, aggregation, context budget. Literal local paths in test sources are built at run time.
- **Golden payloads:** `internal/guard/testdata/*.json` and `internal/session/testdata/` hold Claude Code payloads
  and transcript excerpts (anonymised) with expected stdout; `-update` regenerates. Each golden asserts both the
  decision and, for denies, the rule name in the reason.
- **Real git:** `checkpub` tests create temp repos (`git init`, `-c core.autocrlf=false`, fixed author env).
- **Fake executables:** `proctest.Install(t, "gh", scenario)` copies the test binary to `t.TempDir()/gh[.exe]` and
  returns its absolute path. Each package's `TestMain` calls `proctest.Main()` before `m.Run`; a copy that finds its
  JSON scenario file next to its own executable answers argv from it (`argv → stdout, stderr, exit, sleep, spawn
  grandchild`) and appends argv and stdin to a log the test asserts on. Detection does not use the environment, so a
  fake still works when the code under test replaces `Cmd.Env`. Code under test receives the resolved path
  through the `Runner`/lookup seam; tests that must change `PATH` (discovery) are not parallel, because setting `PATH`
  in `Cmd.Env` does not change the parent's `PATH` lookup. The same helper fakes extensions and boards.
- **Processes:** the inherited-pipe test spawns a grandchild that keeps stdout open after the child exits and
  measures the return time from `Wait` returning (child exit observed, after the kill on timeout), not from start. `.kits` concurrency tests start separate
  `stack` processes: contending writers, a writer against a contending reader, and interleaved open/close; on Windows
  they also assert the temp handle is closed before rename (no sharing violation). Windows cancellation is tested by injecting a context; console
  Ctrl+C delivery is not claimed.
- **CLI:** `cli.Run(ctx, args, Env{Stdin, Stdout, Stderr, Getenv, Home})` in-process, so tests never mutate global
  env except where `exec` needs `PATH`.
- **Measure (design targets, not v10 requirements):** (1) pre-tool-use wall time (`BenchmarkDecide` < 20 ms
  in-process; real hook process < 100 ms); (2) parity: replay historical `PreToolUse` Bash/Read payloads from
  `~/.claude/projects/**/*.jsonl` through the legacy hook and `stack`, counting as evidence only payloads whose cwd and
  referenced body files are still recoverable; expected differences are the dropped worktree rules, the per-command gh
  inputs, the narrowed secrets rule and rmroot; (3) false positives: each `stack` deny on that corpus that is not a
  legacy deny is reviewed.

## 9. Acceptance Criteria

### 9.1 Purpose layer

| ID | EARS criterion | Source | Observable result / failure | Verify on the real thing |
|---|---|---|---|---|
| P1 | While no config file exists, every `stack` command shall run with §5 defaults. | `docs/architecture.md:27,65` | exit 0/1 as normal, no config error | `HOME=$(mktemp -d) stack budget status`; same home, pipe a Read-secret payload to pre-tool-use → deny JSON |
| P2 | When a `Read`, `Grep`, `Glob` or `NotebookRead` call targets a configured secret, or a `Bash` command mentions one outside a handoff substitution, the hook shall deny it. | §1, §3 | Claude Code shows the deny reason; file not read | live session: ask to `Read` a `.dev.vars` in a scratch repo; ask to `cat` it |
| P3 | When a secret file appears only inside a `$(cat F)` / `$(< F)` substitution that is the value of a variable assignment or of a flag, the secrets rule shall not return deny. | `guard-agent-actions.py:6-9` | command runs | live: `TOKEN="$(cat .dev.vars)" true`; `cmd --token "$(cat .dev.vars)"` |
| P4 | When a `gh` publish command's statically known published input (flag, body file, bound heredoc or `< F`) contains a local path, the hook shall deny it. | §1, §3 | deny reason quotes the hit | live, scratch repo: `gh pr create --body-file b.md` with a home path in `b.md`; `gh -R o/r issue comment 1 --body-file - <<'EOF'` / `see <home path>/notes` / `EOF` |
| P5 | When `gh pr merge` is run without effective `--help` or `--disable-auto`, in any `-R` placement, the hook shall deny it and name `stack pr merge`. | §3 | deny; `gh pr merge --help` runs | live session |
| P6 | If hook stdin cannot be decoded, then the hook shall print nothing and exit 0. | constraint | session continues | `printf 'x' \| stack hook claude pre-tool-use; echo $?` → `0`, empty stdout |
| P7 | If the config is invalid, then hooks shall apply §5 defaults. | constraint | secrets still denied | broken `config.toml`, Read-secret payload → deny JSON |
| P8 | If one kit file, board or transcript cannot be read, then the hook shall omit only that part. | constraint | other blocks present | compact payload with unreadable `state.md` and a valid brief |
| P9 | When any SessionStart event with a valid id fires (incl. `fork`) and `CLAUDE_ENV_FILE` is provided, the hook shall export `STACK_SESSION_ID`; it shall inject nothing except on compact. | §4 S1, T0 | `echo $STACK_SESSION_ID` equals the session id; no context on startup | live: new session, run the echo; inspect transcript |
| P10 | When a session compacts, the hook shall restore the kits opened and not closed under that session id, when the `.kits` lock is acquired within 1 s (else the kit part is omitted with a marker). | §3, §4 | kit block present after `/compact`; absent after `kit close`; absent in a fork | live: `kit open demo`, `/compact`; close, `/compact` again |
| P11 | When a worker session compacts, the hook shall restore `opus-worker.md` whole and the original brief, verbatim when both fit in 9,500 chars and otherwise cut with a marker. | §4 worker detection | rules and brief visible after compact | live: first prompt starting with the marker; `/compact` |
| P12 | When `check public` finds a local path (POSIX or Windows) or private term in a range or metadata, it shall exit 1 naming each hit. | §1, §3 | stderr lists hits; exit 1; clean → 0; bad range → 2 | real repo branch with a seeded commit; Windows CI via `stack.exe` |
| P13 | When `pr merge` merges without `--auto`, it shall merge only the checked full head SHA, after every check of that SHA passed (or none exist and `--allow-no-checks` is given) and `.claude/merge-check` is absent or passed; with `--auto`, steps 1–8 shall check that SHA, the submission shall require it, and success shall require a verified merge of it; a merge of any other head (GitHub cannot pin the head it later merges while auto-merge is active) is reported as a failure (§4.4 step 10). | §3 | stdout `MERGED <H> …`; refusals exit 1 | scratch GitHub repo PR: stale SHA, failing check, no checks without the flag, refusing merge-check, then success; `--auto`: a dotfiles PR whose gate status is absent until auto-merge is enabled merges `H` |
| P14 | When a direct `rm` with a recursive flag targets `/`, home, a drive root or an existing `.git` directory, the hook shall deny it. | §1 item 2 | deny reason | live: `rm -rf .git` in a scratch clone |
| P15 | While no extension is installed, `stack run <stage>` shall pass and list the skipped names (design choice). | §3, §4 | exit 0, `"status":"pass"` | `stack run outbound` on a clean machine |
| P16 | When an extension returns block, `stack run` shall exit 1; when it times out or returns malformed output, it shall warn and exit 0 (design choice). | §3 protocol | JSON aggregate | stub `stack-demo` on PATH |
| P17 | Where Windows is the runtime, the Go CLI and hook stdin/stdout handling shall behave as on POSIX, incl. `C:\Users\<name>\` detection. | §1, AGENTS.md | `windows-latest` green | CI job + `stack.exe check public` on a seeded repo |
| P18 | While `deny_pencil_generate` is not `false`, the hook shall deny a Pencil `execute` whose code calls a function or method named `generate*` (case-insensitive; not in strings, comments or regex literals). Unparsable `input` and each `edits[].replace` are judged by the best-effort token fallback; a call inserted by a replacement between string fragments of the unseen snippet is not covered. | owner decision | deny unless disabled | live Pencil MCP session |
| P19 | `budget status` shall display sizes and counts and never exit non-zero for being over. | §3 | exit 0 | run with an oversized `state.md` |

### 9.2 Plan layer

| ID | EARS claim | Test | Required assertions |
|---|---|---|---|
| C1 | When the config file is absent, `config.Load` shall return §5 defaults. | `config.TestLoadMissingFile` | every §5 default compared, incl. `deny_pencil_generate = true` |
| C2 | If the config has a syntax error or `version != 1`, then non-hook commands shall exit 2, and hooks shall use defaults. | `config.TestLoadInvalid`, `cli.TestHookConfigFallback` | syntax → file + line; version → file + key; hooks get defaults (PR 1, config level); the CLI deny JSON under a broken config is asserted in PR 4 (secrets) and PR 5 (Pencil) |
| C3 | `paths.Expand`/`Resolve` shall expand `~`, `~/`, `~\` (USERPROFILE on Windows), leave `~user`, and map `/c/…` on Windows. | `paths.TestExpand`, `paths.TestResolve` | the Windows result opens the real file |
| C4 | `paths` comparison shall use component boundaries and fold case only on darwin/windows. | `paths.TestCompare` | `secrets-example` not within `secrets`; `C:\Users\<Name>\.T3` ≡ `c:/users/<name>/.t3` on Windows; `.DEV.VARS` ≠ `.dev.vars` on Linux (separate case) |
| C5 | When `FailOpen`'s function fails to decode, errors or panics, it shall write no stdout and return 0. | `hookio.TestFailOpen` | stdout exactly empty, exit 0, per cause |
| C6 | `publictext` shall match POSIX, Windows (incl. `\\`), Git Bash and WSL home paths with Unicode token boundaries, and not placeholders. | `publictext.TestLocalPath`, `publictext.TestRepoTree` | real-name hit per OS form, incl. a one-character name (built at run time); `資料/home/demo`, `/home/...`, `/home/…`, `/home/<name>`, `home/dot_x`, `~/x` pass; tracked tree plus this document pass |
| C7 | `check public` shall scan messages, destination paths and added lines of every commit, and title/body/branch, without a size cap. | `checkpub.TestRange*` (real git) | one isolated hit each in an intermediate commit, a merge's added line, a rename destination, each metadata; a hit after 2 MiB of patch |
| C8 | If private-terms exists but is unreadable, a directory or not UTF-8, then `check public` shall exit 2. | `checkpub.TestPrivateTermsUnreadable` | one case each |
| C9 | `shell.Parse` shall return, statement by statement, calls behind wrappers, `bash -c`, bash heredocs, `$(…)`, `&&`, with static words, bound stdin and known cwd, and none for quoted mentions. | `shell.TestParse` | quoted argv evaluates static; `$VAR` word marked dynamic; `cd d && gh` cwd = `<cwd>/d`; `cd d; gh` and `cd d || gh` cwd unknown; `'~'` not expandable; a statement before a later syntax error is returned, the tail is not; a line only zsh parses (`${(f)x}`) is returned; handoff ranges only for single-operand `cat F`/`< F` in assignment or flag values, a whole here-string word, or a redirect-free `echo`/`printf` feeding a pipe |
| C10 | When a file tool targets a secret, `guard` shall deny. | `guard.TestSecretsFileTools` | `Read` relative to `cwd`, symlink target, `Grep.path`, `Grep.glob`, `Glob.pattern`; `.dev.vars.example` and `secrets-example` pass |
| C11 | When a Bash call names a secret as a file outside the exceptions, `guard` shall deny. | `guard.TestSecretsBash` | deny `cat .dev.vars`, `cat < .dev.vars`, `bash -c 'cat .dev.vars'`, `printf %s "$(cat .dev.vars)"`, `ls .dev.vars`, `echo "never read .dev.vars"`, `K="$(cat .dev.vars \| tee x)" cmd`, `K="$(cat .dev.vars)" cmd < .dev.vars`, `cat .dev.vars` before a later syntax error, `echo -n "$(cat .dev.vars)"`, `cmd -- "$(cat .dev.vars)"`, `printf '%s\n' "$(cat .dev.vars)"` (terminal), `echo "$(cat .dev.vars)" > f`, `cmd <<< "pw $(cat .dev.vars)"`; allow `K="$(cat .dev.vars)" cmd`, `export K="$(< .dev.vars)"`, `--token "$(cat .dev.vars)"`, `--token="$(cat .dev.vars)"`, `cmd <<< "$(cat .dev.vars)"`, `printf '%s\n' "$(cat .dev.vars)" \| sudo -S -v`, `cat .dev.vars.example` |
| C12 | When a gh publish input contains a local path, `guard` shall deny. | `guard.TestGhPublish` | deny per source: `--body`, `--body-file` after `cd sub &&`, `--body-file - <<EOF`, `< F`, `pr new`, `release new --notes`, `api -X POST -F k=@f`, `api -X POST -f a=<home path> -f b=clean` (repeated field, hit in the first), `pr create --attach './s.png#<home path>'` (alt text), an unknown flag before `--body <home path>` (literal scan of the rest); allow release asset abs path, `gh api … --jq` GET, the body file's own absolute path as the argument; deny a body file whose content contains its own path |
| C13 | `guard` shall deny `gh pr merge` unless help or `--disable-auto` is effective, and `gh api` merge forms. | `guard.TestRawMerge` | deny all `-R` placements, `GH_REPO=x gh pr merge`, `--disable-auto=false`, `--subject --help`, `-- --help`, `api -X PUT /repos/…/pulls/1/merge` and `repos/…/pulls/1/merge` (both forms), `gh pr merge 1` on a line before a later syntax error, graphql mutation via `-f query=`, `-F query=@q.graphql` and `--input` JSON, `enablePullRequestAutoMerge` in a mutation, `gh pr merge -h=false` and `--help=false`; allow `--help`, `-h`, `--disable-auto`, `--disable-auto=true`, `gh pr view`, a mutation whose unrelated fragment (not spread into it) selects `mergePullRequest`, a read query whose string literal or `#` comment mentions `mergePullRequest`, a mutation using `mergePullRequest:` only as an alias, an unparsable query; deny the field inside a named fragment or an inline fragment of a mutation, and under an alias (`m: mergePullRequest(…)`) |
| C14 | `guard` shall deny recursive `rm` of `/`, home, a drive root or a `.git` directory, and nothing else. | `guard.TestRmRoot` | deny `rm -rf /`, `rm -fr ~`, `rm -r -- ~/`, `rm -rf -- /`, `rm -rf .git` (a directory), `cd / && rm -rf .` (resolution test only: GNU `rm` itself refuses `.`); Windows: `rm -Rf 'C:\'`, `rm -rf C:/`, `rm -rf /`, `rm -rf /c`, `rm -rf /c/`; allow `rm -rf '~'`, `rm -rf .git` where `.git` is a linked worktree's file, `cd /; rm -rf .` and `cd / \|\| rm -rf .` (cwd unknown), `rm -rf .git/hooks`, `rm -f .git`, `rm -rf ./build`, `echo rm -rf /` |
| C15 | `guard` shall deny Pencil `Generate(` in executed code unless disabled. | `guard.TestPencil` | no config (default on) → deny; explicit `false` → allow; `Generate({…})` deny vs `Print("Generate()")`, a template literal whose text holds `Generate()`, vs `${Generate(…)}` inside a nested template → deny, `// Generate()` and `/* Generate() */` allow; `/"/; Generate()` (regex literal) deny; top-level `await Generate()` and `return Generate()` deny; `x.Generate()` and `generateImage()` deny; `regenerate()` allow; token fallback on an unparsable snippet: a real `Generate(a)` call → deny, and `x = /Generate(a)/` (regex after `=`) → allow, as separate assertions; `edits[].replace` judged by the token fallback: `Generate(x)` → deny, `"Generate(x)"` → allow, and the coverage limit `"); Generate(x); ("` → allow (documented gap); `Generate(` only in `description` → allow |
| C16 | `kit open` shall record the slug once per session id and `kit close` shall remove it, with same-id updates serialised. | `kit.TestStore`, `kit.TestStoreProcesses`, `cli.TestKitOpenClose` | invalid slugs (`demo.`, `con.txt`, `..`) exit 2; separate processes opening `a` and `b` under one id → both listed; a writer against a contending reader and interleaved open/close keep the list consistent; lock held by another process past the timeout → exit 1 with reason, file unchanged; different ids independent; no `STACK_SESSION_ID` → context on stdout, warning on stderr, exit 0, no file |
| C17 | The renderer shall select the handover by `[handover]` order and render unreadable parts as markers. | `kit.TestRender` | T3 beats herdr; `HERDR_ENV=0` → none, `=1` → herdr; out-of-kit symlink → marker; board `.cmd` on Windows → marker |
| C18 | For every source, when `CLAUDE_ENV_FILE` is provided, session-start shall append the export line; it shall print nothing except on compact. | `session.TestExport` | sources incl. `fork`; sourcing the file yields the id; stdout empty; `CLAUDE_ENV_FILE` unset → nothing written, exit 0, and a compact worker payload still restores the worker part |
| C19 | When source is `compact`, session-start shall render the kits of that id only, without extensions. | `session.TestCompact` | resume→compact (same id) restores; fork→compact (new id) restores nothing; close then compact → absent; `.kits` lock held → kits omitted, worker part still restored; fake `stack-<name>` on `PATH` never invoked |
| C20 | For a detected worker, compact shall restore body and brief within the 9,500-char budget in the stated priority. | `session.TestWorker`, `session.TestMultiKitReservation`, `session.TestReservedMarkers`, `session.TestBudgetUTF16` | each signal alone; string, block-array, tool_result-first and CRLF transcripts; rules whole and long brief verbatim with kits cut; a brief alone over 9,500 chars → rules whole, brief cut with marker, total incl. tags and markers ≤ 9,500; a rules body alone over 9,500 → rules cut with marker, then only the brief and kit omitted markers, total ≤ 9,500; broken transcript keeps kits; unreadable kit keeps brief; several kits (`NORTH.md` and `state.md`) → each file keeps its header and first 600 chars when they fit, else its omitted marker naming its path; `.kits locked` and brief markers survive a long brief or rules; budget counted in UTF-16 units |
| C21 | `ext` shall skip missing extensions, warn on unrunnable/timeout/WaitDelay/truncated/malformed output, reject non-component names, and aggregate block > warn > pass. | `ext.TestRun*` (proctest) | valid JSON then hang → warn; JSON and exit per case; mixed statuses; `--ext ../x` → exit 2; Windows: only `stack-x.cmd` on PATH → warn not runnable; a `.cmd` earlier on PATH than `stack-x.exe` → the `.exe` runs |
| C22 | `--replay` shall send the fixture request, compare `expect`, and reject a stage mismatch. | `cli.TestRunReplay` | match → expected status; status mismatch → 1; stage mismatch → 2; fake's stdin log equals the fixture request |
| C23 | If any pre-merge step fails, then `pr merge` shall not invoke `gh pr merge`. | `merge.TestRefusals` (argv log) | one fake scenario per step; checks query OID ≠ `H` refuses; confirmed-empty vs query-error distinguished |
| C24 | On success `pr merge` shall merge the full checked SHA with the chosen method. | `merge.TestMerge` | argv has `--match-head-commit <full H>`, method; checks query used `H`; stdout `MERGED <H> <oid>` |
| C25 | `pr merge` shall treat `.claude/merge-check` absent as pass, and not runnable or non-zero as refusal. | `merge.TestMergeCheck` | three separate cases |
| C26 | When `gh pr merge` exits non-zero, `pr merge` shall read back and report merged only for `MERGED` + `H` + merge OID; it shall refuse `--subject/--body` with a local path. | `merge.TestReadback`, `merge.TestMessageLocalPath` | exit 0 + stderr note vs exit 1; empty or non-SHA merge OID → exit 1 |
| C27 | `budget status` shall show byte sizes and per-role counts incl. `all`, and exit 0 regardless of sizes. | `budget.TestStatus` | exact bytes and counts asserted; `all`-only register shows its count; oversized → exit 0 |
| C28 | When `--auto` is given, `pr merge` shall send `--auto --match-head-commit <full H>` only after steps 1–8 pass (and no auto-merge request is active), report success only for a read-back of `MERGED` + `H` + full merge OID, and on every stop condition attempt to disable auto-merge and decide by a final read-back. | `merge.TestAuto` | fake gh, injected clock, no real sleeps; exact exit code, stdout and argv order for: existing request refused; immediate merge; delayed merge; non-zero gh then verified merge; non-zero gh then open → disable without polling; open on `H` with no request → stop; merged other head; moved head → disable; `CLOSED`; unreadable polls to the deadline; disable fails then `OPEN` + `null` → not merged, then active or unreadable request → unknown, then `MERGED` `H` → success; cancel → disable |
