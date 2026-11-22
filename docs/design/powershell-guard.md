# PowerShell Guard Design: Shell Rules for the `PowerShell` Tool

Design for issue #44, against `docs/design/phase2-core.md` (§0, §2.3, §3, §4.3, §6) and `docs/design/core-stages.md`
(§4.5, §4.10) at `ccba4e2`. Design only: the implementation PR applies the doc changes of §8 and the code of §6.

## 1. Purpose

Both PreToolUse deciders return early for any tool other than `Bash` (`internal/guard/guard.go` `Decide`;
`extensions/guard/guard.go` `Decide` has no `PowerShell` case). A session can offer both the `Bash` tool and the
`PowerShell` tool, so a command denied through `Bash` (a secret read, a local path published through `gh`, a raw
`gh pr merge`, a recursive delete of home or a drive root) passes through `PowerShell`. This design extends the
raw-merge, secrets, gh-publish and rmroot rules to `PowerShell` with the same rule IDs and reason prefixes and the
same hard-boundary policy: near-zero false positives, irreversible hazards only, fail open.

## 2. Scope

In scope: the four rules above for `tool_name == "PowerShell"` on every host OS the tool runs on (Windows, and
Linux, macOS and WSL where Claude Code enables it, per Claude Code's tools reference); the `PowerShell` tool in
`stack-observe` replay; the matcher change in dotfiles (stated here, made there).

Out of scope: a full PowerShell parser (issue #44); `CLAUDE_ENV_FILE`/`STACK_SESSION_ID` in PowerShell-only
sessions (issue #44); the pencil rule (not a shell rule); every construct §8's "Out of static scope" text lists.

**Tool input.** No recorded PowerShell call exists in this repository's testdata. Claude Code's hooks reference
documents `tool_input.command` for `PowerShell`; the schema in the installed Claude Code 2.1.x also has `timeout`,
`description`, `run_in_background` and `dangerouslyDisableSandbox`, the `Bash` tool's fields. The deciders read only
`command`. W1 (§9) confirms it from a recorded transcript and a live hook run.

## 3. Decision 1: how PowerShell text is judged

§3 of phase2-core asks that a check judging another language be built on that language's own parser. For PowerShell
that parser is `System.Management.Automation.Language.Parser`, which exists only inside `pwsh`/`powershell.exe`; no
Go library parses PowerShell that this design relies on (the implementation PR re-checks).

| Option | Gains | Gives up | Reversibility | Cost (estimates) |
|---|---|---|---|---|
| **(a)** Go recognizer of a bounded PowerShell subset: tokens, statements, and only the command positions listed below (**chosen**) | in-process, no runtime dependency, identical on every CI OS; anything outside the subset is skipped (fail open) | not the real parser: unsupported constructs are misses; a mis-modelled construct could judge a call pwsh reads differently | full: the `powershell.Parse` contract (§6) can later be backed by (b) | estimated 1–1.5k lines with tests; a grammar subset to keep in sync with 5.1 and 7 |
| **(b)** run pwsh's parser (`[Parser]::ParseInput` over `-EncodedCommand`) per hook call | the language's own AST | a PowerShell process start on every PreToolUse call, in both hooks, inside a 5 s timeout; 5.1 and 7 parse differently; no `pwsh` = no guard; dev machines need `pwsh` installed | full | per-call latency of a PowerShell start (estimated well above the ~5 ms hook of core-stages §5.1; unmeasured), plus an IPC protocol |
| **(c)** reuse `shell.Parse` (mvdan bash) | no new code | wrong language: backtick, `&`, `{…}`, `@(…)`, `$env:X`, here-strings differ; contradicts §3 outright | full | false denies and misses |

(a) is chosen, and this is an explicit exception to §3's parser rule, approved on two grounds: the parser exists
only in a child process (a per-call start-up cost on the hottest hook path) and that process is a runtime dependency
the core otherwise avoids (§3's `cygpath` row makes the same call). The recognizer keeps §3's aim, no calls found
inside quotes, by tokenizing as PowerShell does and never matching regexes over raw text. (c) is rejected.

PowerShell 5.1 and 7 are the test oracles: on Windows, a corpus of the §7.2 commands is parsed by both
(`[Parser]::ParseInput`) and the extracted command names and argv are compared with `powershell.Parse` (W2). The
choice flips to (b) on a material syntax or argv disagreement the subset cannot be narrowed around, if end-to-end
Windows timings of both real hooks with (b) stay near today's hook, or if Claude Code's payload starts carrying a
parsed AST (a third option with no child process). A binding or provider disagreement would survive a switch to
the AST and is fixed in the rule, not the parser.

**Boundary rule.** The recognizer judges a finite, named list of command positions and operand forms: the ones
this section and §4 list. Everything else is opaque and fails open (no call, no operand), and §8's "Out of static
scope" text names the opaque categories. A missed construct is added only by extending this list with its own paired
tests, never by widening a general rule.

**Tokens.** `'…'` (`''` escapes; also `‘’`), `"…"` (backtick escapes, `""`; also `“”`), here-strings whose opener
`@'`/`@"` is followed by optional horizontal whitespace and a newline and whose closer `'@`/`"@` stands at column 0,
`#` comments at token start, `<#…#>` block comments, backtick escapes in barewords (`` g`h `` is `gh`) and
backtick-newline continuation. File redirections (`>`, `>>`, `2>`, `*>`, `3>`…, with their target word) are
consumed and never enter `Args`; merging redirections (`2>&1`, `*>&1`, …) are complete tokens with no target, so
`gh pr merge 7 2>&1 --help` keeps `--help` and passes; a `$(…)` inside a target is still walked for calls (bash parity), so
`Write-Output ok > $(gh pr merge 7; 'out.txt')` denies and `> out.txt` adds nothing.

**Statements.** Separators: newline, `;`, `|`, `&&`, `||`, trailing `&`. A statement's command is:

- its first token when that is a bareword other than a language keyword; for `return`, `throw` and `exit` the
  pipeline after the keyword;
- the token after the `&` or `.` call operator, quoted or not;
- inside `(…)`, `$(…)`, `@(…)` and `$(…)` of an expandable string or here-string, the first token of each inner
  statement;
- the right side of `=` or a compound assignment operator only in assignment position: the statement starts with a
  variable or `[type]$x` lvalue, or the `=` is a key's value inside `@{…}`;
- inside a script block only for exactly these blocks: an invoked block (`& {…}`, `. {…}`); the bodies of
  control-flow statements (`if`/`elseif`/`else`, `foreach`, `for`, `while`, `do`, `switch`, `try`/`catch`/`finally`,
  `trap`); the first positional block or `-Process`/`-Begin`/`-End` block of `ForEach-Object` (`%`, `foreach`); the
  first positional block or `-FilterScript` of `Where-Object` (`?`, `where`); and the first positional block or
  `-ScriptBlock` of a local `Invoke-Command` (`icm`) whose only other parameters are `-ArgumentList`, `-NoNewScope` and common parameters; any other parameter shape (`-ComputerName`, `-HostName`, `-Session`, `-VMName`, …) is opaque. Any
  other block is opaque: `$p = {…}`, a block inside an expression, `Write-Output { gh pr merge 7 } | Get-Member`,
  `-ArgumentList { … }`, remote `Invoke-Command`, and `Start-Job` (its own working directory).

Statements starting with a variable, number, string, `[type]` or `(` are expressions with no command of their own.
In argument mode `=` and `:` are argument text: `--disable-auto=false`, `-f key=val` and `-Name:value` stay one word
(a colon-bound value keeps its quoting and known-variable mark). Commas differ by command kind: for cmdlet binding an
unquoted `,`, with or without spaces, splits a list (`-Path a,C:\` is two elements), a quoted `'a,C:\'` is one
literal; native argv keeps `a,b` as one word. Command names compare case-insensitively after the `.exe` and
module-qualifier strip (`Microsoft.PowerShell.Management\Remove-Item`). Parameters starting with `–` `—` `―` count as
dashes for cmdlet parameters only, never rewritten in native argv.

**Command kind.** Each call keeps its kind through name normalisation: *cmdlet* (a cmdlet name, module-qualified or
not, or a built-in alias of one on that host) or *native* (any other name, and always an explicit path or a name
ending in `.exe`). `& 'C:\Program Files\Git\usr\bin\rm.exe' -r '~'` is native, never `Remove-Item`.

**`--%`.** Stripped from argv. The verbatim tail ends at a CR or LF, quoted or not, or at an unquoted `|`, `&&` or
`||` (the 7.x tokenizer's boundaries; `;` and a quoted `"a|b"` stay in the tail), split at whitespace outside double
quotes (`"`, `“`, `”`, `„`); a word holding one is dynamic (its argv depends on the native-argument mode), and so is
one holding `%NAME%`; what
follows the boundary is parsed normally, so `git --% status && gh pr merge 7` denies.

**Words.** `powershell.Word` keeps `Text`, `Static`, its quoting (bare, single, double, here-string), the parameter
it binds to after cmdlet binding, its `,`-list group, and a known-variable mark. A word is static when it holds no
unescaped `$`, no leading `@` and no subexpression, and, in native argv on non-Windows hosts, no unquoted wildcard
(`*`, `?`, `[`, backtick-escaped or not: PowerShell globs the unescaped bareword there). A bare or double-quoted `$HOME`, `${HOME}`, `$env:USERPROFILE`,
`${env:USERPROFILE}`, `$env:SystemDrive` or `${env:SystemDrive}` (names case-insensitive, `` `$ `` escaped is
literal), alone or followed by static path text, is a known-variable word; `'$HOME'` is the literal text `$HOME`. A
known variable that the script assigns before the word (`$HOME =`, `$env:USERPROFILE =`, `Set-Variable`, `Set-Item
env:…`) is dynamic. Uses: rmroot recognises all of them symbolically (§4.4). Native argv gets only the home forms
substituted, with the hook's home as their one known binding: `$HOME`/`${HOME}` on every host, and
`$env:USERPROFILE`/`${env:USERPROFILE}` on Windows only (on other hosts it is an ordinary environment variable, so
dynamic). `$env:SystemDrive` is never substituted into native argv (the hook does not know the process's system
drive), so such a `gh` input is dynamic and skipped.

**Nested scripts.** Each nested script carries its dialect. `Invoke-Expression`/`iex` and `pwsh`/`powershell`
`-Command`/`-c` with one static string are parsed by `powershell.Parse`; `bash`/`sh`/`zsh -c` with one static string
is a nested Bash script, judged by the deciders' Bash path (Bash rules, Git Bash resolver) at the call's known cwd.
The depth allowance (2, as in bash) is shared: each `Nested` entry records the depth already consumed, and
`shell.ParseDepth` (§6) continues from it. Each dialect keeps its own error handling: nested Bash yields its valid
prefix as `shell.Parse` does, nested PowerShell follows the failure table below. Deferring cross-shell recursion
instead would leave `bash -c 'gh pr merge 1'` from PowerShell unguarded.

**Failure boundaries.** Each is tested through hook stdout (C6):

| Failure | Result |
|---|---|
| payload `tool_input` undecodable, `command` not a string or empty | allow, no output (both hooks) |
| top-level lexing fails (unclosed quote or here-string, unbalanced brackets) | no calls: raw-merge, gh-publish and rmroot judge nothing (pwsh runs nothing of a script it cannot parse); secrets still scans the whole text, so a mention denies even though the engine would reject the script (bash parity) |
| a nested PowerShell script fails to lex | only that nested script is skipped; the outer calls stand, so `gh pr merge 7; iex 'Write-Output "'` denies |
| a nested Bash script has a syntax error | Bash's own rule: the complete lines before the error line are judged, so `bash -c 'gh pr merge 7`⏎`if ('` denies |

**String literals** are argument values, never commands, outside the positions above: `echo "gh pr merge 1"`
passes raw-merge; `echo "$(gh pr merge 1)"` and `& 'gh' pr merge 1` deny. Secrets scans every byte regardless.

## 4. Decision 2: matching per rule

Host semantics come from an injected OS parameter (`Windows` or not), defaulting to `runtime.GOOS`, so Linux CI runs
both sets. On Windows: case-insensitive names and paths, `\`≡`/`, drive roots, and `rm`, `rmdir`, `ri`, `del`,
`erase`, `rd` are `Remove-Item` aliases. Elsewhere: the host's case rule (`paths`' fold on darwin, none on Linux),
`/` roots only, and `rm`/`rmdir` are native commands (PowerShell defines them as aliases only on Windows; `ri`,
`del`, `erase` and `rd` stay aliases, confirmed by the 7 oracle, W2). Path syntax follows the command kind, not only
the host: `Remove-Item` and its real aliases use FileSystem-provider syntax (`\` and `/` both separators) on every
host, so `Remove-Item -Recurse '.\.git'` on Linux is judged; native commands use the host's native path syntax.

### 4.1 raw-merge (core)

A call whose name is `gh` (`gh`, `gh.exe`, `GH.EXE` on Windows, `& gh`, `& 'C:\Program Files\GitHub CLI\gh.exe'`)
is turned into a `shell.Call` (§6) and goes through `ghargs.Parse` and `rawMerge` exactly as for Bash: `gh pr merge`
without effective help or `--disable-auto` (so `--help=false` and `--disable-auto=false` deny), `gh api` PUT
`…/pulls/<n>/merge`, `gh api graphql` merge mutations inline or from `-F query=@q.graphql`/`--input`, read through
the PowerShell resolver. `gh pr view`, `gh pr merge --help` and a string mentioning `gh pr merge` pass.

### 4.2 secrets (`stack-guard`)

Any mention of a configured entry (the examples assume a `~` entry `~/.config/app/token` and a bare entry) anywhere
in the command text denies, with the existing `mentions` boundary rule
(`~` entries by their home-relative part, bare entries as tokens, `/` in an entry matching `\`). On Windows case is
folded and runs of `/` and `\` count as one separator; elsewhere the host rule applies, as for Bash. So on Windows
`~\.config\app\token`, `C:/Users/<name>/.CONFIG/APP/…`, `$env:USERPROFILE\.config\\app\token`, `Get-Content
.\.DEV.VARS` and `type .dev.vars` deny; on Linux `Get-Content .DEV.VARS` passes (a different file). `.dev.vars.example`
passes. There is no handoff exemption (§5). `Write-Output 'Copy .dev.vars.example to .dev.vars'` denies, as the same
`echo` does through Bash: mention-rule parity, not a new false positive. The rule ID and `stack-guard (secrets): `
prefix are kept; only the advice is tool-specific (§5).

### 4.3 gh-publish (`stack-guard`)

The gh calls of §4.1 go through `ghPublish`: published flags, `--body-file F`/`-F F`, `--notes-file`, `gh api`
fields and `--input`, and the literal scan after an unknown flag, as §4.3 of phase2-core, hits being the paths the
existing `publictext` matcher recognizes. The unknown-flag fallback can deny `gh pr create --bogus -b 'C:\Users\<name>\log'`
that the installed gh would reject: a Bash parity trade-off, kept. The issue's "command text" is read as these
published inputs, so `cd 'C:\Users\<name>\repo'; gh pr create -t x -b y` passes, as in bash.

Stdin for `-F -`/`--input -`: a pipeline whose element directly before the gh call is one static string or
here-string (`@'`⏎`…`⏎`'@ | gh pr comment 1 -F -`) is `StdinText`; an expandable here-string with `$(…)` is dynamic.
Any other pipe, including `Get-Content F | gh …` (encoding and provider semantics), is dynamic and skipped.

Body files resolve against the call's known cwd: payload `cwd`, changed only by `Set-Location`/`cd`/`sl`/`chdir X
&&` with a static `X`; `cd` with no argument, any other location change, `Push-Location`/`pushd` and
`Pop-Location`/`popd` make it unknown. A native `~` follows §4.4's native rule: on non-Windows hosts an unquoted
leading `~` is home (PowerShell 7 expands it for native commands); on Windows a native `~` input is skipped as
unknown (W3), with no home substituted.

### 4.4 rmroot (`stack-guard`)

A cmdlet-kind `Remove-Item` call (name or alias, module-qualified or not) with `-Recurse` effective denies, on
every host, when a path operand is the home directory, a drive root or an existing `.git` directory. The `.git` case
extends issue #44's list to the full rmroot boundary of AGENTS.md (same rule, same reason). A native `rm` (`rm` on
non-Windows hosts, `/bin/rm`, `rm.exe`, any explicit path) goes through the Bash `rmArgs`/`rootKind` matcher with
native argv semantics, since it is the binary the Bash rule guards: home-form known variables reach it substituted;
on non-Windows an unquoted leading `~` is home and a quoted `'~'` is literal; on Windows a quoted `'~'` is literal
(`& 'C:\…\rm.exe' -r '~'` passes) and an unquoted native `~` is opaque until W3 establishes whether the installed
PowerShell expands it.

- **Binding** follows PowerShell over a table of `Remove-Item`'s parameters, their aliases (`LiteralPath`: `PSPath`,
  `LP`) and the common parameters: an exact name or alias wins, else a unique prefix (`-r`, `-Rec`, `-RECURSE`); an
  ambiguous or unknown parameter makes pwsh refuse the call, which is then not judged. `--` ends parameters.
  `-Recurse:$false` is not recursive; `-WhatIf` (and `-WhatIf:$true`) skips the call, `-WhatIf:$false` does not.
  `-rf` is no parameter, so on Windows `rm -rf ~` passes (pwsh refuses it).
- **Operands:** the values of `-Path`/`-LiteralPath`, each element of their `,` list; when neither is named, only the
  word at positional index 0 (with its `,` list); a further positional word makes pwsh refuse the call (no other
  `Remove-Item` parameter is positional), so it is not judged. Values of `-Filter`, `-Include` and `-Exclude` are
  never operands, so `Remove-Item -Recurse -Exclude a, .git .\build` and `Remove-Item -Recurse .\build -Filter
  '.git'` pass.
- **Home:** `~`, `~\`, `~/` (quoted or not: the FileSystem provider expands `~`), the known variables `$HOME`,
  `${HOME}`, `$env:USERPROFILE`, `${env:USERPROFILE}` with at most trailing separators or `.`, and any static path
  equal to the payload home under the host comparison. `'$HOME'` is a directory named `$HOME` and passes.
- **Drive roots (Windows):** `C:\`, `C:/`, `c:\.`, and bare `\`, `/`, `\.`, `/.` (the root of the current drive,
  whatever that drive is, so a root even with an unknown cwd; a non-FileSystem current location is out of scope, and
  a script that runs `New-PSDrive` leaves bare roots opaque, since the current drive may then be a custom one; this
  also skips a bare root that is still on the OS volume, a limitation kept for the no-false-positive rule), and
  `$env:SystemDrive\`, `${env:SystemDrive}\` (recognised symbolically). `C:` alone is the current directory of drive C
  and is not judged.
- **Allowed:** `Remove-Item -Recurse .\build`, `Remove-Item -Recurse .\build ~` (two positionals: pwsh refuses it),
  `Remove-Item ~\x -Recurse`, `Remove-Item C:\` without `-Recurse`, `Remove-Item -Recurse .git\hooks`, a worktree's
  `.git` file, `echo "Remove-Item -Recurse C:\"`.

## 5. Decision 3: the secrets handoff

| Option | Gains | Gives up | Reversibility |
|---|---|---|---|
| **(A)** deny every mention (**chosen**) | no exemption logic; one rule text; the same mention rule as Bash | a PowerShell-only session cannot pass a secret file's value to a command through the PowerShell tool | loosening to (B) later breaks nothing |
| **(B)** narrow exemption: only the exact `--flag (Get-Content -Raw -LiteralPath F)` value of a consumer's argument, that source range masked; `Write-Output`, file output, extra reads and assign-then-print stay denied | parity with the owner's Bash handoff in PowerShell-only sessions | exact-form ranges in the recognizer; the consumer and value form (`-Raw` keeps trailing newlines) need their own acceptance tests | tightening back to (A) breaks commands relying on it |

(A) is chosen: it is reversible toward (B) without breaking anything; it keeps the Bash mention rule's behaviour;
in a dual-tool session, #44's motivating case, the Bash handoff remains; and PowerShell has no command-scoped
environment prefix (`$env:K = …` persists for the rest of the script) while `"$(Get-Content F)"` joins lines with
spaces, so no form is a faithful equivalent of `K="$(cat F)" cmd`. It flips to (B) when observe or a live session
shows a PowerShell-only session needing a handoff. PowerShell denies carry this advice: "Commands must not show a
secret file. No PowerShell form passes a secret file; use the Bash tool's `K="$(cat <file>)" cmd` handoff where it
exists, or ask the user."

## 6. Decision 4: code location and dispatch

| Item | Change |
|---|---|
| `internal/powershell` (new, core) | `Parse(cmd, cwd, home string, os OS) Result` with `Calls []Call` (`Word`s of §3, binding info), `Nested []Script` (dialect, text, cwd, consumed depth), `Err`. `Call.ShellCall()` adapts a call to `shell.Call` for `ghargs`, raw-merge, gh-publish and the native-`rm` path. The adapter applies §3's native-argv rules and nothing else: home-form known variables substituted per host (§3 Words), `$env:SystemDrive` left dynamic; an unquoted leading `~` gets `Tilde: true` on non-Windows hosts and a quoted one `Tilde: false`; on Windows an unquoted native `~` word is dynamic. `shell.Word` and every Bash consumer stay unchanged. In core because raw-merge needs it and core never imports `extensions/`. |
| `internal/shell` | `ParseDepth(cmd, cwd, home, depth)`: `Parse` continuing from the depth already consumed, for nested Bash scripts (§3); `Parse` is `ParseDepth(…, 0)`. |
| `internal/paths` | `ResolvePowerShell(home, cwd, p string, windows bool)`: no Git Bash mapping; relative joins with the host separator (real file reads work on every CI OS); on Windows `\x`/`/x` rooted on the cwd's drive (unknown without one), `C:x` unknown; plus `Norm`/`Same` with an explicit `windows` flag for the rmroot and home comparisons. |
| `internal/guard` `Decide` | `Bash` → `shell.Parse` as now; `PowerShell` → same `command` decode, `powershell.Parse`, `rawMerge` over the adapted gh calls, then each nested Bash script through the Bash path; `readInput`/`readFile` take the resolver of the call's dialect. |
| `extensions/guard` `Decide` | `case "PowerShell"`: decode as `Bash`; `secretsPowerShell` (§4.2), `ghPublish` (§4.3), `rmRootPowerShell` (§4.4) in that order, first deny wins, then nested Bash scripts through `decideBash`. Reasons keep `stack-guard (<rule>): `. |
| `.golangci.yaml` depguard | the `extension-guard` allow list gains `internal/powershell$`; core-stages §4.5's import sentence names it. |
| `extensions/observe` | `wiredTools` gains `PowerShell`; `guardInput` treats it as `Bash` (`command` non-empty). `replay.go` already pipes `tool_name` and `tool_input` to both hooks. Same PR as the deciders, so a replayed PowerShell call never meets hooks that ignore it. |
| dotfiles (out of this repo) | core hook matcher `Bash` → `Bash\|PowerShell`; `stack-guard` matcher adds `\|PowerShell`; that PR's settings test asserts both. Until both the release and the matchers are installed, a real `PowerShell` call is still allowed. |

## 7. Acceptance criteria

### 7.1 Purpose layer

Real-CLI check: build both binaries into a temp dir inside the worktree, set `USERPROFILE`/`HOME` to a temp home
whose `safety.toml` lists `.dev.vars`, and pipe a payload:
`printf '%s' '{"hook_event_name":"PreToolUse","tool_name":"PowerShell","cwd":"<tmp>","tool_input":{"command":"<cmd>"}}' | <tmp>/stack hook claude pre-tool-use`
(likewise `<tmp>/stack-guard hook claude pre-tool-use`); deny = the JSON reason, allow = empty stdout, exit 0 always.

| ID | EARS criterion | Source | Scenario → observable result / failure | Real-CLI check |
|---|---|---|---|---|
| U1 | When a `PowerShell` tool call runs `gh pr merge`, the core hook shall deny it with the `stack guard (raw-merge): ` reason. | #44 acceptance 1 | `& gh.exe pr merge 7 --squash` → reason names `stack pr merge`; failure: empty stdout | `stack`, that command and `gh pr merge --help` (empty) |
| U2 | When a `PowerShell` tool call mentions a configured secret path in any separator or case form the host treats as the same path, `stack-guard` shall deny it with the `stack-guard (secrets): ` reason. | #44 acceptance 2 | Windows: `Get-Content .\.DEV.VARS`, `type C:/work/repo/.dev.vars` → deny | `stack-guard`, both commands |
| U3 | When a `PowerShell` tool call recursively removes the home directory, a drive root or an existing `.git` directory, `stack-guard` shall deny it with the `stack-guard (rmroot): ` reason. | #44 acceptance 3, AGENTS.md | `Remove-Item -Recurse -Force $HOME`, `ri -r C:\`, `rm -r .git` → deny | `stack-guard`; on Windows with the real home |
| U4 | When a `PowerShell` tool call publishes, through `gh`, a path the existing `publictext` matcher recognizes (flag, `--body-file` or piped literal here-string), `stack-guard` shall deny it with the `stack-guard (gh-publish): ` reason. | #44 proposal 3 | `gh pr create -t t --body-file b.md` with a home path in `b.md`; a here-string with real newlines piped to `gh pr comment 1 -F -` → deny | `stack-guard`, `b.md` in `<tmp>` |
| U5 | When a `PowerShell` tool call matches none of these rules, both hooks shall allow it with no output. | #44 acceptance 4 | §7.2 C8 commands → empty stdout, exit 0, each paired with a deny of the same shape | both binaries |
| U6 | If the `PowerShell` tool input cannot be decoded, then both hooks shall allow it, as they do for `Bash`. | #44 acceptance 5 | `"tool_input":"x"`, `{"command":7}`, `{}` → empty stdout, exit 0 | both binaries |

### 7.2 Plan layer

Every allow case is paired with a deny fixture that differs only in the behaviour under test.

| ID | EARS claim | Source | Test and required assertions |
|---|---|---|---|
| C1 | `powershell.Parse` shall return the command calls of the supported positions of §3, with exact argv, and none for literal quoted text outside `$(…)`. | §3 | `powershell.TestParse`, exact argv per case: `;`, `\|`, `&&`, `\|\|`, newline, continuation; `& 'x y\gh.exe'`, `. gh`, `` g`h ``; `return gh pr merge 7` (call) vs `Write-Output note=gh pr merge 7` (one call, `Write-Output`); `& { gh pr merge 7 }`, `if ($ok) { gh pr merge 7 }`, `try { gh pr merge 7 } catch {}` and `ForEach-Object { gh … }` (calls) vs `$p = { gh … }` and `Write-Output { gh pr merge 7 } \| Get-Member` (only the outer `Write-Output` and `Get-Member` calls); `icm -ScriptBlock { gh pr merge 7 }` (call) vs `icm -ScriptBlock { 'x' } -ArgumentList { gh pr merge 7 }`, `icm -ComputerName h { gh … }`, `icm -HostName h -ScriptBlock { gh … }`, positional `icm h -ScriptBlock { gh … }` and `Start-Job { gh … }` (none); `gh pr merge 7 2>&1 --help` argv keeps `--help`; `@{ Result = gh pr merge 7 }` (call) vs `@{ Result = 'gh pr merge 7' }` (none); `$x = gh …` (call) vs `echo result = gh …`; `--disable-auto=false`, `-f k=v`, native `a,b` one word; `-Path a,C:\` two elements vs `'a,C:\'` one; redirections absent from argv, `> $(gh pr merge 7; 'out.txt')` (call) vs `> out.txt` (none); here-string closer only at column 0 (indented `'@` inside the body does not close), opener with trailing spaces accepted; `--%` stripped, tail ends at `\|`, `&&`, `\|\|` or newline but not `;` or a quoted `"a\|b"`, `%USERPROFILE%` dynamic, the statement after the boundary parsed; command kind: `Remove-Item` vs `& 'C:\x\rm.exe'` vs Linux `rm`; known-variable words vs `'$HOME'` vs a prior `$HOME =`, `-LiteralPath:"$HOME"` vs ``-LiteralPath:"`$HOME"``, `${env:SystemDrive}`; nested `iex` and `bash -c` with their dialect and consumed depth; `$x`, `@a`, `(…)` words dynamic |
| C2 | When a PowerShell gh call merges, the core decider shall deny with the raw-merge reason. | U1 | `guard.TestRawMergePowerShell`: `gh pr merge 1`, `GH.EXE pr merge 1` (Windows), `& gh pr merge`, `gh -R o/r pr merge 1`, `--help=false`, `--disable-auto=false`, `gh api -X PUT repos/o/r/pulls/1/merge`, inline graphql mutation, `cd sub && gh api graphql -F query=@q.graphql`, and on Windows CI a current-drive-rooted `-F query=@\…\q.graphql` that plain `paths.Resolve` rejects (fails if it is used), `"$(gh pr merge 1)"`, `bash -c 'gh pr merge 1'`, `bash -c 'gh pr merge 7`⏎`if ('` (valid prefix), `git --% status && gh pr merge 7`, a valid here-string after `gh pr merge 7`; depth boundary pair: `iex "bash -c 'gh pr merge 7'"` (depth 2, deny) vs the extra wrapper inside Bash, `iex 'bash -c ''bash -c "gh pr merge 7"'''` (asserting the inner Bash text reaches the depth check) (beyond depth, allow; fails if the depth resets at the dialect boundary); `gh pr merge 7 2>&1 --help` (allow); allow `gh pr view 1`, `--help`, `--disable-auto`, `echo "gh pr merge"`, `# gh pr merge` |
| C3 | When a PowerShell command mentions a secret in a form the host treats as the same path, `stack-guard` shall deny with the secrets reason and the PowerShell advice. | U2, §4.2 | `guard.TestSecretsPowerShell`, case and separator independently, OS injected: Windows `.DEV.VARS` deny, Linux `.DEV.VARS` allow; `~\.config\app\token`, `.config\\app//token`, an absolute configured entry in `/` and `\` forms; mention in a comment and in a string; `.dev.vars.example` allow; no handoff form passes |
| C4 | When a PowerShell gh call publishes a path `publictext` recognizes, `stack-guard` shall deny with the gh-publish reason. | U4, §4.3 | `guard.TestGhPublishPowerShell`, every hit a path `publictext` recognizes, built at run time (the repo's public check rejects home-shaped literals in source) and used in both halves of each pair: `-b`, `--body-file` after `cd sub &&` vs after `cd sub;` (allow, cwd unknown), here-string with real newlines → `-F -` vs inert here-string (allow) vs expandable here-string with `$()` (dynamic, allow), `Get-Date \| gh … -F -` (allow), `Get-Content b.md \| gh … -F -` with a hit in `b.md` (allow, deferred); `-F ~\b.md` with a hit in the home copy: Windows skipped (allow), Linux `-F ~/b.md` deny vs `-F '~/b.md'` (allow); `-F "$env:SystemDrive\b.md"` (dynamic, allow); Linux `-F "$HOME/b.md"` with a hit in the home copy (deny) vs `-F "$env:USERPROFILE/b.md"` with `USERPROFILE` naming a clean copy (allow, dynamic on Linux); `cd '<built path>'; gh pr create -t x -b y` allow vs the same path in `-b` deny; the body file's own absolute path as argument allow vs the file holding that path deny; `--bogus` unknown-flag fallback deny |
| C5 | When a PowerShell `Remove-Item` with effective `-Recurse` targets home, a drive root or an existing `.git` directory by one of the §4.4 forms, `stack-guard` shall deny with the rmroot reason. | U3, §4.4 | `guard.TestRmRootPowerShell`, OS injected: canonical `Remove-Item` and an explicit native executable independently, each alias incl. module-qualified; `-r`, `-Rec`, `-Recurse:$true` vs `:$false`; `-WhatIf` (allow) vs `-WhatIf:$false` (deny); no `-Recurse` (allow); `-Path`, `-LiteralPath`, `-LP`, `-PSPath`, positional 0, `-Path a, C:\`, `-Path a,C:\` (deny) vs `-Path 'a,C:\'` (allow), `--`; `~`, `'~'`, `$HOME`, `${HOME}`, `"$env:USERPROFILE\"` vs `'$HOME'` (allow) vs `$env:USERPROFILE = 'C:\work\x'; Remove-Item -Recurse $env:USERPROFILE` (allow); `C:\`, `c:/`, `\`, `/` with unknown cwd, `$env:SystemDrive\`, `${env:SystemDrive}\`, `C:` (allow); `.git` directory vs worktree `.git` file vs `.git\hooks` on every OS via temp dirs; `-Exclude a, .git .\build`, `.\build -Filter '.git'` and `.\build ~` (allow); Linux `Remove-Item -Recurse '.\.git'` deny; `-p` (ambiguous, allow); Linux: native `rm -rf $HOME` and `rm -rf ~` deny, `rm -rf '~'` allow, `ri -r /` deny; Windows: `& 'C:\x\rm.exe' -r '~'` and `-r ~` allow (opaque until W3); `New-PSDrive …; Remove-Item -Recurse \` allow vs `Remove-Item -Recurse \` deny |
| C6 | If the input is undecodable, the top level fails to lex, or a nested script fails, then the deciders shall apply the §3 failure boundaries. | U6, §3 | hook-stdout tests in both packages: string `tool_input`, numeric `command`, `{}` → empty; `gh pr merge 7; Write-Output "` → empty (core); `Get-Content .dev.vars; Write-Output "` → secrets deny; `Remove-Item -Recurse $HOME; Write-Output "` → empty `stack-guard` stdout; `gh pr merge 7; iex 'Write-Output "'` → raw-merge deny |
| C7 | The core decider shall deny no PowerShell input that only a `stack-guard` rule covers. | core-stages §4.5 | the existing cross-golden check over the new `extensions/guard` PowerShell goldens |
| C8 | When a PowerShell command matches no rule, both hooks shall print nothing. | U5 | goldens `allow-powershell-*` in both packages, each next to its deny pair: `Get-Content README.md`, `Remove-Item -Recurse .\build`, `gh pr view 5`, `echo "gh pr merge"`, `git status; git diff --stat` |
| C9 | When observe scans `PowerShell` tool calls, it shall replay each through both hooks like a `Bash` call. | §6 observe | `observe.TestReplayRealHook`: a transcript with a core-only PowerShell deny (raw-merge) and an extension-only one (rmroot), both matched; exact `Compared` and `Unrecoverable` totals; a `{}` input counted as not replayable with no hook invocation |
| C10 | `paths.ResolvePowerShell` and the `windows`-flagged `Norm`/`Same` shall resolve PowerShell paths without Git Bash forms. | §6 | `paths.TestResolvePowerShell`: relative join opens the real file on every OS; Windows `\x` on the cwd's drive, unknown without one; `C:x` unknown; `/c` with cwd `D:\work` resolves to `D:\c` on Windows (not drive C); `C:\Users\<Name>` ≡ `c:/users/<name>` only with `windows` |

## 8. Doc updates the implementation PR makes

phase2-core §2.3, **Hook scope** paragraph, replaced by: "**Hook scope.** The shell rules apply to Claude Code's
`Bash` and `PowerShell` tools. `PowerShell` commands are judged by raw-merge, secrets, gh-publish and rmroot through
the bounded PowerShell recognizer of `docs/design/powershell-guard.md` (§3 there), not a PowerShell parser: a command
outside its subset is skipped by the call-based rules, and a command it cannot lex is judged by secrets only. Without
Git Bash the SessionStart hook receives no `CLAUDE_ENV_FILE`, so `STACK_SESSION_ID` is unset and `kit open` takes its
unrecorded path (§4.1). File-tool rules (`Read`, `Grep`, …) apply on every OS."

phase2-core §3, new Library Choices row: "| PowerShell command analysis | hand-written recognizer
(`internal/powershell`) | pwsh's `Parser.ParseInput` in a child process; `mvdan.cc/sh` | Approved exception to the
parser rule: PowerShell's parser exists only inside pwsh, a process start per hook call and a runtime dependency;
bash parsing misreads backticks, `&` and script blocks. The recognizer tokenizes as PowerShell does, judges only
listed command positions, fails open, and is checked against PowerShell 5.1 and 7 (powershell-guard §3). |", and the
rule's last clause becomes "(local paths in prose, Git Bash path forms, the PowerShell subset)".

phase2-core §6, "Out of static scope" row, Where-it-appears cell replaced by: "`gh` aliases, `xargs`, scripts,
`python -c`, `$VAR` words, `cat f \| gh … -F -`, the unparsable tail after a syntax error (secrets still scan it as
text). `PowerShell`, all call rules: a script that fails to lex (secrets still scan it), constructs outside the
recognizer's listed command positions (script blocks other than the listed ones, calls inside method-call
arguments other than `$()`), `cmd /c`, `Start-Process`, .NET/COM calls, splatting, variables other than the known
home and drive names; raw-merge and gh-publish: `gh` reached only through those; gh-publish: file-producing pipes
(`Get-Content F \| gh … -F -`), a native `~` argument on Windows, `$env:USERPROFILE` in native argv on other hosts, cwd after `;`, `Push-Location` or a bare `cd`; rmroot: pipeline
input to `Remove-Item`, globs (`C:\*`), provider-qualified, `\\?\`, UNC and non-FileSystem paths (`HKCU:\`),
`-LiteralPath '~'` until W3, an unquoted native `~` on Windows until W3, bare roots after `New-PSDrive`; secrets: deliberate obfuscation (backtick in a name, built strings)"; Coverage cell
"documented, not guarded (§0.4, §2.3)".

phase2-core §4.3: the Tools column adds `PowerShell` to secrets, gh publish, raw merge and rmroot with a pointer to
powershell-guard §4, and the settings-wiring line adds `PowerShell`. core-stages §4.5: the matcher and the import
allow list (§6). phase3-cycle §3.3: the replayed tool list adds `PowerShell`.

## 9. Test plan and risks

Goldens (payload `tool_name: "PowerShell"`, `@@CWD@@` cwd): `internal/guard/testdata/deny-raw-merge-powershell`,
`deny-raw-merge-powershell-graphql-file` (`.files`), `allow-powershell-string`;
`extensions/guard/testdata/deny-secrets-powershell`, `deny-gh-publish-powershell-body-file` (`.files`),
`deny-gh-publish-powershell-herestring`, `deny-rmroot-powershell-home`, `allow-rmroot-powershell-build`,
`allow-powershell-get-content-readme`. Table tests of §7.2 carry the per-form cases with the OS injected. No real
recorded PowerShell input exists yet; the first Windows transcript lines replace the synthetic `tool_input` in one
golden per rule. `mise exec -- scripts/test` on all three CI OSes.

Risks: the subset diverges from pwsh on a construct (miss or false deny; the oracle comparison and flip of §3);
5.1 vs 7 differences (`&&`/`||` are 5.1 parse errors, so judging them only denies what would not run; on 5.1 the
`cd X &&` cwd form does not exist, so a `cd sub; gh … --body-file b.md` stays a miss); `Remove-Item` parameter table
drift (a new parameter starting with `r` makes `-r` ambiguous, so the rule over-judges only a call pwsh refuses).

Left for Windows hardware (not chased from macOS):

- W1: record a real `PowerShell` tool call and a live hook run; confirm `tool_name`, `tool_input` fields and launch.
- W2: oracle run of the §7.2 corpus through PowerShell 5.1 and 7 `[Parser]::ParseInput`, comparing command names and
  argv with `powershell.Parse` (native argv, incl. `--%` tails, through an argv-printing stub); confirm the Unix alias set on PowerShell 7 for Linux and the `-p` ambiguity.
- W3: `~` expansion by `Remove-Item -LiteralPath '~'` and by native arguments on 5.1 and 7; adopt a form only once
  confirmed.
- W4: U1–U6 against `stack.exe`/`stack-guard.exe` with the dotfiles matchers in a live dual-tool session, and both
  hooks' p95 on a PowerShell payload.
