# PowerShell Guard Design: Shell Rules for the `PowerShell` Tool

Design for issue #44: the core hook judges raw merges, and `stack-guard` judges secrets, gh-publish and rmroot, for
the `PowerShell` tool as for `Bash`. It extends `docs/design/phase2-core.md` (§2.3, §3, §4.3, §6) and
`docs/design/core-stages.md` (§4.5); those documents state the resulting contracts (§8 here).

## 1. Purpose

A session can offer both the `Bash` tool and the `PowerShell` tool. A hook that judged only `Bash` would let
a command denied through `Bash` (a secret read, a local path published through `gh`, a raw `gh pr merge`, a recursive
delete of home or a drive root) pass through `PowerShell`. This design extends the
raw-merge, secrets, gh-publish and rmroot rules to `PowerShell` with the same rule IDs and reason prefixes and the
same hard-boundary policy: near-zero false positives, irreversible hazards only, fail open.

## 2. Scope

In scope: the four rules above for `tool_name == "PowerShell"` on every host OS the tool runs on (Windows, and
Linux, macOS and WSL where Claude Code enables it, per Claude Code's tools reference); the `PowerShell` tool in
`stack-observe` replay; the matchers in dotfiles (stated here, set there).

Out of scope: a full PowerShell parser; `CLAUDE_ENV_FILE`/`STACK_SESSION_ID` in PowerShell-only sessions; the pencil
rule (not a shell rule); every construct that phase2-core §6 "Out of static scope" lists.

**Tool input.** Claude Code (2.1.330, recorded on Windows) sends `tool_name: "PowerShell"` with a `tool_input` of
`command` and an optional `description` (one recorded call has none); the tool schema also has `timeout`,
`run_in_background` and `dangerouslyDisableSandbox`, the `Bash` tool's fields. The deciders read only `command`.
Exec-form hooks (`command` plus `args`) are started directly by `claude.exe`, with no shell between. The recorded
inputs back one golden per rule (§9).

## 3. Decision 1: how PowerShell text is judged

§3 of phase2-core asks that a check judging another language be built on that language's own parser. For PowerShell
that parser is `System.Management.Automation.Language.Parser`, which exists only inside `pwsh`/`powershell.exe`; no
Go library parses PowerShell that this design relies on.

| Option | Gains | Gives up | Reversibility | Cost (estimates) |
|---|---|---|---|---|
| **(a)** Go recognizer of a bounded PowerShell subset: tokens, statements, and only the command positions listed below (**chosen**) | in-process, no runtime dependency, identical on every CI OS; anything outside the subset is skipped (fail open) | not the real parser: unsupported constructs are misses; a mis-modelled construct could judge a call pwsh reads differently | full: the `powershell.Parse` contract (§6) can later be backed by (b) | estimated 1–1.5k lines with tests; a grammar subset to keep in sync with 5.1 and 7 |
| **(b)** run pwsh's parser (`[Parser]::ParseInput` over `-EncodedCommand`) per hook call | the language's own AST | a PowerShell process start on every PreToolUse call, in both hooks, inside a 5 s timeout; 5.1 and 7 parse differently; no `pwsh` = no guard; dev machines need `pwsh` installed | full | per-call latency of a PowerShell start (estimated well above the ~5 ms hook of core-stages §5.1; unmeasured), plus an IPC protocol |
| **(c)** reuse `shell.Parse` (mvdan bash) | no new code | wrong language: backtick, `&`, `{…}`, `@(…)`, `$env:X`, here-strings differ; contradicts §3 outright | full | false denies and misses |

(a) is chosen, and this is an explicit exception to §3's parser rule, approved on two grounds: the parser exists
only in a child process (a per-call start-up cost on the hottest hook path) and that process is a runtime dependency
the core otherwise avoids (§3's `cygpath` row makes the same call). The recognizer keeps §3's aim, no calls found
inside quotes, by tokenizing as PowerShell does and never matching regexes over raw text. (c) is rejected. In-process
recognition keeps the hook's cost that of a `Bash` call: on a Windows desktop under load both hooks' p95 on a
PowerShell payload equals the `Bash` one (idle p50 32–46 ms), far under the 5 s hook timeout.

PowerShell 5.1 and 7 are the test oracles (C11): the §7.2 commands are parsed by each installed engine
(`[Parser]::ParseInput`; 5.1 and 7 on Windows, 7 on Linux) and the command names, kinds and argv it finds are
compared with `powershell.Parse`; native calls also run against an argv-printing stub and their argv is compared
with `Call.ShellCall()`. Every declared case passes on Windows PowerShell 5.1.26100, PowerShell 7.7.0 (Windows) and
7.6.5 (Linux); every engine behaviour this document calls measured was measured on these versions ("5.1" and "7.7"
are the Windows engines). The choice flips to (b) on a material syntax or argv disagreement the subset cannot be narrowed around, if end-to-end
Windows timings of both real hooks with (b) stay near today's hook, or if Claude Code's payload starts carrying a
parsed AST (a third option with no child process). A binding or provider disagreement would survive a switch to
the AST and is fixed in the rule, not the parser.

**Boundary rule.** The recognizer judges a finite, named list of command positions and operand forms: the ones
this section and §4 list. Everything else is opaque and fails open (no call, no operand), and phase2-core §6 "Out of static
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
literal; native argv keeps `a,b` as one word. A token that starts with a quote ends at its closing quote, for
cmdlets and native commands alike (5.1 and 7.7): `'a'b`, `"a"b` and `"a"$x` are two arguments and `gh pr 'merge'7`
runs `gh pr merge 7`, while `a'b'` is one word `ab` and `'a''b'` one word `a'b`. A member access or index right
after such a token is part of it, an expression, so the word is dynamic (`'--help'.foo` is `$null` and passes no
argument, `'--help'[0]` passes `-`; `'a'. x` has no member and stays three words). A native `,` right after such a
token makes an array the engines pass differently (`'a',b`: 5.1 `a,b`, 7.7 `'a',b`), so that word is dynamic, and
each quote-started element of it also ends at its quote (`'a','b'c d` is the array, `c`, `d` on both engines, so a
trailing `'a','b'--help` stays its own argument). Command names compare case-insensitively after the `.exe` and
module-qualifier strip (`Microsoft.PowerShell.Management\Remove-Item`). Parameters starting with `–` `—` `―` count as
dashes for cmdlet parameters only, never rewritten in native argv.

**Command kind.** Each call keeps its kind through name normalisation: *cmdlet* (a cmdlet name, module-qualified or
not, or a built-in alias of one on that host) or *native* (any other name, and always an explicit path or a name
ending in `.exe`). `& 'C:\Program Files\Git\usr\bin\rm.exe' -r '~'` is native, never `Remove-Item`.

**`--%`.** The stop-parsing token is a token that does not start with a quote and whose decoded value is `--%`: `--%`,
the backtick-escaped `` `--% `` and quotes inside such as `-'-%'`, `--'%'` or `-"-%"` (both engines, argv stub), as a
whole token followed by a blank; `a`--%` is an ordinary word and `'--'%` two words, `--` and `%`. A cmdlet keeps the token as a value and gets the rest of the tail as one
literal argument (`Write-Output a --% $x b` writes `a`, `--%`, `$x b`), so `` Remove-Item -Recurse -Path `--% ~ `` is
refused (PositionalParameterNotFound) and not judged. A native command has it stripped from argv. The verbatim tail
ends at a CR or LF, quoted or not, or at an unquoted
`|`, `&&` or `||` (the 7.x tokenizer's boundaries; `;` and a quoted `"a|b"` stay in the tail), split at whitespace
outside double quotes (`"`, `“`, `”`, `„`); a word holding one is dynamic, since the engines differ (5.1 passes
`"c d"` as one argument `c d` and `x"c d"y` as `xc dy`, 7.7 as `"c`, `d"` and `x"c`, `d"y`, for `--%` and `` `--% ``
alike), and so is one holding `%NAME%`; what follows the boundary is parsed normally, so `git --% status && gh pr
merge 7` denies and ``Write-Output `--% ; gh pr merge 7`` passes. A token directly followed by `;`
(``Write-Output `--%; gh pr merge 7``) is the stop-parsing token too, for both engines and the recognizer, so the `gh`
text is part of the tail and that passes as well.

A native argument that starts with a quote and whose decoded value is `--%` (`'--%'`, `"--%"`, `` "--`%" ``, a
here-string) is not the stop-parsing token: parsing goes on (variables still expand), but pwsh 5.1 and 7.7 drop that
argument from argv and pass every later argument joined into the command line as written (measured with an argv
stub). So the recognizer drops it, splits each later static value at blanks (`"merge 7"` becomes `merge`, `7`), and
leaves a later `~` as written: `gh pr '--%' "merge 7"` denies and `gh pr merge 7 '--%' --help` passes.

One rule covers what the engines pass differently after it. From the first later argument whose boundaries or
value can differ between 5.1 and 7.7 to the end of the call, every word is **ambiguous**: a value holding `"`, `,`,
a tab (5.1 splits there, 7.7 does not; neither splits at other Unicode spaces) or a line break (5.1 joins
`'--body=a"b' --help` into one argument, 7.7 passes two), a `%NAME%` (expanded to an unknown value), an empty value
(an argument on 7.7, none on 5.1), a bare parameter token such as `-h` or `-h=true` (5.1 drops it, 7.7 passes an
empty value, and both keep the quotes of the argument after it), a word that is not static (a variable or
interpolation, whose value may hold any of these), an array, another quoted `--%` (dropped too) and a later
`--%` or `` `--% `` (its tail joins the region). An ambiguous word keeps its static text, so checks still match
it, but nothing in the region can prove a call harmless: raw-merge never counts a `--help`, `--disable-auto` or
`--dry-run` found there (`gh pr merge 7 --squash '--%' '--body=a"b' --help` denies); rmroot still matches its
operands (`& rm.exe -r '--%' '' .git` denies); gh-publish still reads its body files (`gh pr comment 1 '--%'
'--body=a"b' --body-file=b.md` denies). The over-denies this accepts stay inside the region:
`gh pr merge 7 '--%' '' --help` and `gh pr merge 7 '--%' '--%' --help` deny although both engines pass a separate
`--help`. A cmdlet keeps such a value as an ordinary argument (`Write-Output a '--%' b`); a dynamic word whose value
may be `--%` (`"--$('%')"`, which both engines also drop) is not modelled, nor is the engine an explicit
`powershell.exe`/`pwsh` wrapper names.

Known differences kept: a cmdlet's `--%` tail is trimmed and an empty one dropped, where both engines pass it
untrimmed and `Write-Output --%` passes an empty value (no rule judges that text); and in a native array whose
closing quoted element follows a bare one (`a,'b'c d`, `'a',b,'c'd e`), the text glued to that closing quote stays
in the dynamic array word, where both engines pass it as the next argument (a separate `--help` there is not
counted, an over-deny).

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

A native **home `~` word** is one whose first part is unquoted, outside a `--%` tail and not after a quoted `--%`,
and whose decoded value (after quotes and backtick escapes) is exactly `~`, `~/…`, or on Windows `~\…`, holding no
whitespace (Unicode spaces such as U+00A0 and line breaks included): `~`, `` `~ ``, `` ~\`. ``, `` ~`\ ``, `~'\x'` and `~"\x"` all are. PowerShell 7 expands it for native commands
on Linux and macOS, and on Windows from 7.6, where the feature is on by default (Microsoft's documentation lists
`PSNativeWindowsTildeExpansion` as experimental from 7.5-preview.2 and mainstream from 7.6-preview.6; 7.7 expands
it, measured with an argv stub); Windows PowerShell 5.1 never expands it, and PowerShell 7.0–7.5 on Windows pass it
as written. Everything else stays as written: a quote-started `'~'` or
`"~"`, `~user`, `~foo/x` (also spelt `~'foo'/x`), `x~`, a value holding whitespace (`~'\a b'`), a `~` in a `--%` tail,
and a `~` after a quoted `'--%'` (joined as written, see `--%`). A home form whose value holds a `"` is dynamic
(unmeasured). Because the Windows result depends on the engine version, the native rules use the home word
differently:

- the native-`rm` path of rmroot (§4.4) takes it as home on every host. A miss would be an irreversible deletion of
  home (`& rm.exe -r ~`) on the default engine, and the only false deny is a directory literally named `~` on 5.1 or
  7.0–7.5, which `.\~` names instead;
- gh inputs (gh-publish body files, §4.3, and raw-merge's `gh api graphql --input`, §4.1) take it as home on
  non-Windows hosts and as dynamic on Windows: there the file gh opens is `<home>\…` or `.\~\…` depending on the
  engine, so judging either copy could deny a clean call or pass a published one.

When the hook knows no home, a home `~` word names an unknown file and is not judged; it is never read as a literal
`~` under the cwd.

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
| a script nests deeper than the syntax recursion cap (200 levels of brackets, blocks, hash values and assignments; distinct from the nested-script depth 2) | no calls, as for a lexing failure (fail open) |
| a nested PowerShell script fails to lex | only that nested script is skipped; the outer calls stand, so `gh pr merge 7; iex 'Write-Output "'` denies |
| a nested Bash script has a syntax error | Bash's own rule: the complete lines before the error line are judged, so `bash -c 'gh pr merge 7`⏎`if ('` denies |

**String literals** are argument values, never commands, outside the positions above: `echo "gh pr merge 1"`
passes raw-merge; `echo "$(gh pr merge 1)"` and `& 'gh' pr merge 1` deny. Secrets scans every byte regardless.

## 4. Decision 2: matching per rule

Host semantics come from an injected OS parameter (`Windows` or not), defaulting to `runtime.GOOS`, so Linux CI runs
both sets. On Windows: case-insensitive names and paths, `\`≡`/`, drive roots, `rm`, `rmdir`, `ri`, `del`,
`erase`, `rd` are `Remove-Item` aliases, and `mount` and `ndr` are `New-PSDrive` aliases. Elsewhere: the host's
case rule (`paths`' fold on darwin, none on Linux), `/` roots only, and `rm`, `rmdir` and `mount` are native
commands (PowerShell defines them as aliases only on Windows; `ri`, `del`, `erase`, `rd` and `ndr` stay aliases;
the oracle asserts this set per host). Path syntax follows the command kind, not only
the host: `Remove-Item` and its real aliases use FileSystem-provider syntax (`\` and `/` both separators) on every
host, so `Remove-Item -Recurse '.\.git'` on Linux is judged; native commands use the host's native path syntax.

### 4.1 raw-merge (core)

A call whose name is `gh` (`gh`, `gh.exe`, `GH.EXE` on Windows, `& gh`, `& 'C:\Program Files\GitHub CLI\gh.exe'`)
is turned into a `shell.Call` (§6) and goes through `ghargs.Parse` and `rawMerge` exactly as for Bash: `gh pr merge`
without effective help or `--disable-auto` (so `--help=false` and `--disable-auto=false` deny), `gh api` PUT
`…/pulls/<n>/merge`, `gh api graphql` merge mutations inline or from `-F query=@q.graphql`/`--input`, read through
the PowerShell resolver; a home `~` word in such a file argument follows §3 Words (home on non-Windows hosts,
skipped on Windows). `gh pr view`, `gh pr merge --help` and a string mentioning `gh pr merge` pass.

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
`Pop-Location`/`popd` make it unknown. Cwd updates are tracked for at most 32 steps and 4096 bytes; beyond that the
cwd is unknown and call collection continues. The byte check is a conservative estimate made before building the path
(from the current cwd, home and target lengths), so a very long home or cwd can leave a short result unknown. A native home `~` word
follows §3 Words: on non-Windows hosts `-F ~/b.md` reads the home copy, on Windows `-F ~\b.md` and `-F ~/b.md` are
skipped (the file depends on the engine), and a quoted `'~\b.md'` names a file under the cwd on every host. A `~`
in inline text (`-b ~\notes.md`) is published text, not a path: `publictext` does not recognize `~` forms, so it
passes.

### 4.4 rmroot (`stack-guard`)

A cmdlet-kind `Remove-Item` call (name or alias, module-qualified or not) with `-Recurse` effective denies, on
every host, when a path operand is the home directory, a drive root or an existing `.git` directory, the full rmroot
boundary of AGENTS.md (same rule, same reason). The operand, after its
trailing separators and terminal `.` components are trimmed, must end in a `.git` component that `Lstat` finds to be
a directory; `..` is not collapsed (through a symlink a lexical collapse can name another directory), so forms such
as `.git/hooks/..` are not judged, and `.git/.` checks a `.git` symlink itself, not its target. A native `rm` (`rm` on
non-Windows hosts, `/bin/rm`, `rm.exe`, any explicit path) goes through the Bash `rmArgs`/`rootKind` matcher with
native argv semantics, since it is the binary the Bash rule guards: home-form known variables reach it substituted,
and a home `~` word (§3 Words) is home on every host, so `& 'C:\…\rm.exe' -r ~`, `-r ~\`, `` -r `~ ``,
`` -r ~\`. `` and `-r ~"\"` deny while `-r '~'`, `-r ~\src` and `& rm.exe --% -r ~` pass, and `-r ~'foo'/.git` is
the literal `~foo/.git` under the cwd. A nested script is judged by the same rule
whatever engine runs it, so `powershell.exe -Command '& rm.exe -r ~'` (5.1, which passes `~` as written) denies:
the old-engine false deny above.

- **Binding** follows PowerShell over a table of `Remove-Item`'s parameters, their aliases (`LiteralPath`: `PSPath`,
  `LP`) and the common parameters: an exact name or alias wins, else a unique prefix (`-r`, `-Rec`, `-RECURSE`); an
  ambiguous or unknown parameter makes pwsh refuse the call, which is then not judged (`-p` is ambiguous on all
  three measured engines). `-Path` and `-LiteralPath`
  are in different parameter sets, so a call binding both, also through a positional `Path` next to a named
  `-LiteralPath` (`Remove-Item -Recurse -LiteralPath '.git'build`), is refused (PositionalParameterNotFound on 5.1
  and 7.7). `--` ends parameters.
  `-Recurse:$false` is not recursive; `-WhatIf` (and `-WhatIf:$true`) skips the call, `-WhatIf:$false` does not.
  `-rf` is no parameter, so on Windows `rm -rf ~` passes (pwsh refuses it).
- **Operands:** the values of `-Path`/`-LiteralPath`, each element of their `,` list; when neither is named, only the
  word at positional index 0 (with its `,` list); a further positional word makes pwsh refuse the call (no other
  `Remove-Item` parameter is positional), so it is not judged. Values of `-Filter`, `-Include` and `-Exclude` are
  never operands, so `Remove-Item -Recurse -Exclude a, .git .\build` and `Remove-Item -Recurse .\build -Filter
  '.git'` pass.
- **Home:** `~`, `~\`, `~/` (quoted or not: the FileSystem provider expands `~`; on 5.1 and 7.7 `Remove-Item`
  removes home this way even with a directory named `~` in the cwd) through `-Path` and positional 0. Through `-LiteralPath`
  (`-LP`, `-PSPath`, and each element of its list) only the measured forms count: on Windows `~` followed by nothing
  but separators and `.` components (`'~'`, `~\`, `'~/'`, `'~\.'`, `'~/./'`, all reaching home on 5.1 and 7.7) is
  home, and any other value starting with `~` (`'~\x'`, `'~user'`, `'~.'`) is not judged. `-LP` binds on 7.7, while
  5.1 refuses it (NamedParameterNotFound); judging it denies only a call 5.1 would not run; on other hosts nothing was measured, so `~`, `~/…` and `~\…` are not judged and `'~user'` is a
  literal name. As for bare roots, a non-FileSystem current location is out of scope. Also the known variables `$HOME`,
  `${HOME}`, `$env:USERPROFILE`, `${env:USERPROFILE}` with at most trailing separators or `.`, and any static path
  equal to the payload home under the host comparison. `'$HOME'` is a directory named `$HOME` and passes.
- **Drive roots (Windows):** `C:\`, `C:/`, `c:\.`, and bare `\`, `/`, `\.`, `/.` (the root of the current drive,
  whatever that drive is, so a root even with an unknown cwd; a non-FileSystem current location is out of scope, and
  a script that runs `New-PSDrive` (`ndr`, and `mount` on Windows) leaves bare roots opaque, since the current drive may then be a custom one;
  this also skips a bare root that is still on the OS volume, a limitation kept for the no-false-positive rule), and
  `$env:SystemDrive\`, `${env:SystemDrive}\` (recognised symbolically). A drive the script declares with a static
  `New-PSDrive -Name X` is a custom drive, so paths on `X:` are opaque, as resolved operands (a relative one under a
  cwd on `X:` too); a name that is not static makes every letter opaque, and a call pwsh refuses to bind declares
  nothing, and so does a declaration named after an existing volume: a letter of the host's logical drives
  (`GetLogicalDrives`, read by the hook on Windows) or the drive of the payload cwd or home (`New-PSDrive` fails
  with DriveAlreadyExists, so that root and bare roots stay judged). Declarations are collected script-wide, not in
  statement order and not by scope, so a later declaration also makes an earlier `X:\` opaque, and a declaration
  inside a block that shadows an existing drive in that child scope still leaves the drive judged (over-judging
  only that contrived form). Another volume's root (`C:\`) still denies. `C:` alone is the current directory of drive C
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

| Item | Role |
|---|---|
| `internal/powershell` (core) | `Parse(cmd, cwd, home string, os OS) Result` with `Calls []Call` (`Word`s of §3, binding info), `Nested []Script` (dialect, text, cwd, consumed depth), `Err`. `Call.ShellCall()` adapts a call to `shell.Call` for `ghargs`, raw-merge and gh-publish, and `Call.ShellCallRm()` for the native-`rm` path only. The adapter applies §3's native-argv rules and nothing else: home-form known variables substituted per host (§3 Words), `$env:SystemDrive` left dynamic; a home `~` word gets `Tilde: true`, except in `ShellCall` on Windows, where it is dynamic; a home form holding `"` is dynamic; a quote-started `~`, `~user`, a value holding a blank and a `~` after a quoted `'--%'` get `Tilde: false` (§3 Words). `shell.Word` and every Bash consumer stay unchanged. In core because raw-merge needs it and core never imports `extensions/`. |
| `internal/shell` | `ParseDepth(cmd, cwd, home, depth)`: `Parse` continuing from the depth already consumed, for nested Bash scripts (§3); `Parse` is `ParseDepth(…, 0)`. |
| `internal/paths` | `ResolvePowerShell(home, cwd, p string, expand, windows bool)`: with `expand`, `~`, `~/…` and on Windows `~\…` are home, and unknown when home is ""; without it they are literal names; no Git Bash mapping; relative joins with the host separator (real file reads work on every CI OS); on Windows `\x`/`/x` rooted on the cwd's drive (unknown without one), `C:x` unknown; plus `Norm`/`Same` with an explicit `windows` flag for the rmroot and home comparisons. |
| `internal/guard` `Decide` | `Bash` → `shell.Parse`; `PowerShell` → same `command` decode, `powershell.Parse`, `rawMerge` over the adapted gh calls, then each nested Bash script through the Bash path; `readInput`/`readFile` take the resolver of the call's dialect. |
| `extensions/guard` `Decide` | `case "PowerShell"`: decode as `Bash`; `secretsPowerShell` (§4.2), `ghPublish` (§4.3), `rmRootPowerShell` (§4.4) in that order, first deny wins, then nested Bash scripts through `decideBash`. Reasons keep `stack-guard (<rule>): `. |
| `.golangci.yaml` depguard | the `extension-guard` allow list includes `internal/powershell$`; core-stages §4.5's import sentence names it. |
| `extensions/observe` | `wiredTools` includes `PowerShell`; `guardInput` treats it as `Bash` (`command` non-empty). `replay.go` pipes `tool_name` and `tool_input` to both hooks. |
| dotfiles (out of this repo) | core hook matcher `Bash\|PowerShell`; the `stack-guard` matcher includes `PowerShell`; the dotfiles settings test asserts both. Without the matcher (or with a release older than this design) a real `PowerShell` call reaches no PowerShell rule and is allowed. |

## 7. Acceptance criteria

### 7.1 Purpose layer

Real-CLI check: build both binaries into a temp dir inside the worktree, set `USERPROFILE`/`HOME` to a temp home
whose `safety.toml` lists `.dev.vars`, write the PowerShell command to a file `<tmp>/cmd.ps1` with an editor, and
pipe a payload built with `jq`:
`jq -nc --arg cwd '<tmp>' --rawfile cmd '<tmp>/cmd.ps1' '{hook_event_name:"PreToolUse",tool_name:"PowerShell",cwd:$cwd,tool_input:{command:$cmd}}' | <tmp>/stack hook claude pre-tool-use`
(likewise `<tmp>/stack-guard hook claude pre-tool-use`); deny = the JSON reason, allow = empty stdout, exit 0 always.
Keep the command in the file: pasting it into a JSON string (`printf`, `echo`) can make the payload invalid (an
unescaped `\` in a Windows path), which both hooks allow silently, or change the command (`\t` becomes a tab); and
pasting it into a shell argument lets the shell consume its quotes (`& 'gh'` becomes `& gh`). Save the file as UTF-8
without a BOM (Windows PowerShell 5.1 writes UTF-16 or a BOM by default, and the hooks then allow everything), and run
the pipeline with `set -o pipefail` so a `jq` error is not read as an allow.

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
| C1 | `powershell.Parse` shall return the command calls of the supported positions of §3, with exact argv, and none for literal quoted text outside `$(…)`. | §3 | `powershell.TestParse`, exact argv per case: `;`, `\|`, `&&`, `\|\|`, newline, continuation; `& 'x y\gh.exe'`, `. gh`, `` g`h ``; `return gh pr merge 7` (call) vs `Write-Output note=gh pr merge 7` (one call, `Write-Output`); `& { gh pr merge 7 }`, `if ($ok) { gh pr merge 7 }`, `try { gh pr merge 7 } catch {}` and `ForEach-Object { gh … }` (calls) vs `$p = { gh … }` and `Write-Output { gh pr merge 7 } \| Get-Member` (only the outer `Write-Output` and `Get-Member` calls); `icm -ScriptBlock { gh pr merge 7 }` (call) vs `icm -ScriptBlock { 'x' } -ArgumentList { gh pr merge 7 }`, `icm -ComputerName h { gh … }`, `icm -HostName h -ScriptBlock { gh … }`, positional `icm h -ScriptBlock { gh … }` and `Start-Job { gh … }` (none); `gh pr merge 7 2>&1 --help` argv keeps `--help`; `@{ Result = gh pr merge 7 }` (call) vs `@{ Result = 'gh pr merge 7' }` (none); `$x = gh …` (call) vs `echo result = gh …`; `--disable-auto=false`, `-f k=v`, native `a,b` one word; a token starting with a quote ends at its closing quote (`'pr'merge "7"x 'a''b'c a'b' "a"$x`, cmdlet `'a'b c'd'`), native `'a',b` dynamic and `'a','b'c d` the array, `c`, `d`, a quoted run with blanks in a `--%` tail dynamic; a cmdlet's `--%`/`` `--% `` kept with its tail as one argument; after a quoted `--%` (also `` "--`%" ``) the region keeps static text (`git '--%' a '' b` is `a`, `b`) and `powershell.TestAmbiguousRegion` marks it from an empty value, `"`, `%X%`, `$x`, an array, a second quoted `--%` or a later `--%`/`` `--% `` to the end of the call, and nothing before it; `-Path a,C:\` two elements vs `'a,C:\'` one; redirections absent from argv, `> $(gh pr merge 7; 'out.txt')` (call) vs `> out.txt` (none); here-string closer only at column 0 (indented `'@` inside the body does not close), opener with trailing spaces accepted; `--%` stripped, tail ends at `\|`, `&&`, `\|\|` or newline but not `;` or a quoted `"a\|b"`, `%USERPROFILE%` dynamic, the statement after the boundary parsed; `` `--% `` a stop-parsing token vs `a`--%` a word; a quoted `--%` value (`'--%'`, `"--%"`, here-string) dropped, while `-'-%'` is the stop-parsing token, from native argv with later values split at blanks, an empty one dropped and one holding `"` kept as text in the ambiguous region, vs a cmdlet's `'--%'` kept; one token only (`'--'%` is two words, `--` and `%`); `` Write-Output `--%; gh pr merge 7 `` and the native form stop parsing at `;`; command kind: `Remove-Item` vs `& 'C:\x\rm.exe'` vs Linux `rm`; `powershell.TestShellCall`: `~`, `~/x`, `` `~ `` and on Windows `~\`, `~\b.md` are Tilde on both hosts through `ShellCallRm` and dynamic through `ShellCall` on Windows, vs `'~'`, `"~"`, `x~`, `~user`, `~foo/.git`, a `--%` tail and a `~` after `'--%'` (literal) and `~"\x"`, `~'/x'` (dynamic); known-variable words vs `'$HOME'` vs a prior `$HOME =`, `-LiteralPath:"$HOME"` vs ``-LiteralPath:"`$HOME"``, `${env:SystemDrive}`; nested `iex` and `bash -c` with their dialect and consumed depth; `$x`, `@a`, `(…)` words dynamic |
| C2 | When a PowerShell gh call merges, the core decider shall deny with the raw-merge reason. | U1 | `guard.TestRawMergePowerShell`: `gh pr merge 1`, `GH.EXE pr merge 1` (Windows), `& gh pr merge`, `gh -R o/r pr merge 1`, `--help=false`, `--disable-auto=false`, `gh api -X PUT repos/o/r/pulls/1/merge`, inline graphql mutation, `cd sub && gh api graphql -F query=@q.graphql`, and `guard.TestRawMergePowerShellTilde`: Linux `--input ~/q.json` with the mutation only in the home copy (deny) vs `'~/q.json'` (allow), Windows `--input ~\q.json` and `~/q.json` (allow, skipped); on Windows CI a current-drive-rooted `-F query=@\…\q.graphql` that plain `paths.Resolve` rejects (fails if it is used), `"$(gh pr merge 1)"`, `bash -c 'gh pr merge 1'`, `bash -c 'gh pr merge 7`⏎`if ('` (valid prefix), `git --% status && gh pr merge 7`, ``git `--% x && gh pr merge 7``, a valid here-string after `gh pr merge 7`; quoted `--%`: `gh '--%' pr merge 7`, `& gh.exe "--%" pr merge 7`, `gh pr '--%' "merge 7"`, `& gh.exe '--%' pr "merge 7 --squash"` (deny) vs `gh '--%' pr view 7`, `gh pr merge 7 '--%' --help` (help, allow); quote-started tokens: `gh pr 'merge'7`, `gh pr "merge"7`, `gh 'pr'merge 7` (deny) vs `gh pr 'view'7`, `gh pr merge 7 'a'--help`, `gh pr merge 'x'--help` (help, allow) and ``Write-Output `--% ; gh pr merge 7`` (`;` in the tail, allow); `` gh pr "--`%" "merge 7" ``, `gh pr merge 7 --squash '--%' '--body=a"b' --help`, `gh pr merge 7 '--%' %X% --help` (deny: help unproven); `guard.TestRawMergePowerShellNoNewAllow`: every review probe the base recognizer denied still denies on both hosts (quoted arrays, `"--body=$title"`, a later `--%`/`` `--% ``, `'' --help`, `'--%' '--%' --help`, a tab, `-h`, `-h=true`, `-s '--help'`, `'--help'.foo`, `'--help'[0]`, the explicit `powershell.exe -Command` wrapper), except the measured flips where both engines pass a harmless argv (`'a','b'--help`, `'--%' --help`, `'--%' x --help`, `'a'--help`) vs `gh pr merge 7 '--%' x --help`, `gh pr merge 7 'a','b'--help` (help, allow); depth boundary pair: `iex "bash -c 'gh pr merge 7'"` (depth 2, deny) vs the extra wrapper inside Bash, `iex 'bash -c ''bash -c "gh pr merge 7"'''` (asserting the inner Bash text reaches the depth check) (beyond depth, allow; fails if the depth resets at the dialect boundary); `gh pr merge 7 2>&1 --help` (allow); allow `gh pr view 1`, `--help`, `--disable-auto`, `echo "gh pr merge"`, `# gh pr merge` |
| C3 | When a PowerShell command mentions a secret in a form the host treats as the same path, `stack-guard` shall deny with the secrets reason and the PowerShell advice. | U2, §4.2 | `guard.TestSecretsPowerShell`, case and separator independently, OS injected: Windows `.DEV.VARS` deny, Linux `.DEV.VARS` allow; `~\.config\app\token`, `.config\\app//token`, an absolute configured entry in `/` and `\` forms; mention in a comment and in a string; `.dev.vars.example` allow; no handoff form passes |
| C4 | When a PowerShell gh call publishes a path `publictext` recognizes, `stack-guard` shall deny with the gh-publish reason. | U4, §4.3 | `guard.TestGhPublishPowerShell`, every hit a path `publictext` recognizes, built at run time (the repo's public check rejects home-shaped literals in source) and used in both halves of each pair: `-b`, `--body-file` after `cd sub &&` vs after `cd sub;` (allow, cwd unknown), here-string with real newlines → `-F -` vs inert here-string (allow) vs expandable here-string with `$()` (dynamic, allow), `Get-Date \| gh … -F -` (allow), `Get-Content b.md \| gh … -F -` with a hit in `b.md` (allow, deferred); with a hit in the home copy and a clean literal `~` copy under the cwd: Windows `-F ~\b.md`, `-F ~/b.md` (skipped, allow) and `-F '~\b.md'`, `-F "~/b.md"` (allow) vs `-F "$HOME/b.md"` (deny), Linux `-F ~/b.md` deny vs `-F '~/b.md'` (allow); with no known home and a hit in the literal `~` copy, `-F ~/hit.md` (allow, unknown) vs `-F '~/hit.md'` (deny) on both hosts; `gh pr comment 1 '--%' --body-file=b.md` and, in the ambiguous region, `gh pr comment 1 '--%' '--body=a"b' --body-file=b.md` (deny, an accepted over-deny); `guard.TestPowerShellNoNewAllow` (also C5): every review probe the base recognizer denied still denies on that host, except `gh pr comment 1 -F 'b'.md` (both engines evaluate `'b'.md` to `$null` and pass only `-F`); `-F "$env:SystemDrive\b.md"` (dynamic, allow); Linux `-F "$HOME/b.md"` with a hit in the home copy (deny) vs `-F "$env:USERPROFILE/b.md"` with `USERPROFILE` naming a clean copy (allow, dynamic on Linux); `cd '<built path>'; gh pr create -t x -b y` allow vs the same path in `-b` deny; the body file's own absolute path as argument allow vs the file holding that path deny; `--bogus` unknown-flag fallback deny |
| C5 | When a PowerShell `Remove-Item` with effective `-Recurse` targets home, a drive root or an existing `.git` directory by one of the §4.4 forms, `stack-guard` shall deny with the rmroot reason. | U3, §4.4 | `guard.TestRmRootPowerShell`, OS injected: canonical `Remove-Item` and an explicit native executable independently, each alias incl. module-qualified; `-r`, `-Rec`, `-Recurse:$true` vs `:$false`; `-WhatIf` (allow) vs `-WhatIf:$false` (deny); no `-Recurse` (allow); `-Path`, `-LiteralPath`, `-LP`, `-PSPath`, positional 0, `-Path a, C:\`, `-Path a,C:\` (deny) vs `-Path 'a,C:\'` (allow), `--`; `~`, `'~'`, `$HOME`, `${HOME}`, `"$env:USERPROFILE\"` vs `'$HOME'` (allow) vs `$env:USERPROFILE = 'C:\work\x'; Remove-Item -Recurse $env:USERPROFILE` (allow); `C:\`, `c:/`, `\`, `/` with unknown cwd, `$env:SystemDrive\`, `${env:SystemDrive}\`, `C:` (allow); `.git` directory vs worktree `.git` file vs `.git\hooks` on every OS via temp dirs; `-Exclude a, .git .\build`, `.\build -Filter '.git'` and `.\build ~` (allow); Linux `Remove-Item -Recurse '.\.git'` deny; `-p` (ambiguous, allow); Linux: native `rm -rf $HOME` and `rm -rf ~` deny, `rm -rf '~'` allow, `ri -r /` deny; Windows `-LiteralPath '~'`, `-LP ~\`, `-PSPath '~/'`, `-LiteralPath a, '~'` deny vs the same with `-WhatIf` or without `-Recurse`, `'.\~'`, `'~\child'`, `'~\.'`, `'~user'` (allow), Linux `-LiteralPath '~'`, `-LP ~/`, `'~\'` (allow); Windows native: `& 'C:\x\rm.exe' -r ~`, `-r ~\`, `rm.exe -rf ~/`, `` & rm.exe -r `~ ``, `iex '& rm.exe -r ~'`, `powershell.exe -Command '& rm.exe -r ~'` (deny, the 5.1 false deny), `-r ~"\"`, `` -r ~\`. ``, `` -r ~`\ `` (deny) vs `-r '~'`, `-r "~"`, `-r ~\src`, `-r x~`, `-r ~user`, `--% -r ~`, `'--%' -r ~` (allow); both hosts: `& rm.exe -r ~'foo'/.git` with a `~foo/.git` directory under the cwd (deny), `-LiteralPath '.git'build` (refused, allow), `` -Path `--% ~ ``, `-Path --% ~`, `` `--% ~ `` (refused, allow); Windows `-LiteralPath '~\.'`, `'~/./'` (deny) vs `'~.'` (allow); with no known home and a `~\.git` directory under the cwd, `& rm.exe -r ~/.git` (allow, unknown) on both hosts vs Linux `'~/.git'` (deny); `New-PSDrive …; Remove-Item -Recurse \` allow vs `Remove-Item -Recurse \` deny |
| C6 | If the input is undecodable, the top level fails to lex, or a nested script fails, then the deciders shall apply the §3 failure boundaries. | U6, §3 | hook-stdout tests in both packages: string `tool_input`, numeric `command`, `{}` → empty; `gh pr merge 7; Write-Output "` → empty (core); `Get-Content .dev.vars; Write-Output "` → secrets deny; `Remove-Item -Recurse $HOME; Write-Output "` → empty `stack-guard` stdout; `gh pr merge 7; iex 'Write-Output "'` → raw-merge deny |
| C7 | The core decider shall deny no PowerShell input that only a `stack-guard` rule covers. | core-stages §4.5 | the existing cross-golden check over the new `extensions/guard` PowerShell goldens |
| C8 | When a PowerShell command matches no rule, both hooks shall print nothing. | U5 | goldens `allow-powershell-*` in both packages, each next to its deny pair: `Get-Content README.md`, `Remove-Item -Recurse .\build`, `gh pr view 5`, `echo "gh pr merge"`, `git status; git diff --stat` |
| C9 | When observe scans `PowerShell` tool calls, it shall replay each through both hooks like a `Bash` call. | §6 observe | `observe.TestReplayRealHook`: a transcript with a core-only PowerShell deny (raw-merge) and an extension-only one (rmroot), both matched; exact `Compared` and `Unrecoverable` totals; a `{}` input counted as not replayable with no hook invocation |
| C10 | `paths.ResolvePowerShell` and the `windows`-flagged `Norm`/`Same` shall resolve PowerShell paths without Git Bash forms. | §6 | `paths.TestResolvePowerShell`: relative join opens the real file on every OS; Windows `\x` on the cwd's drive, unknown without one; `C:x` unknown; `/c` with cwd `D:\work` resolves to `D:\c` on Windows (not drive C); `C:\Users\<Name>` ≡ `c:/users/<name>` only with `windows`; with `expand` `~/b.md` is home and unknown with no home, without it a name under the cwd; on POSIX `~\b.md`, `~user`, `~foo/x` are names |
| C11 | Where a PowerShell engine is installed, `powershell.Parse` shall return, for each §7.2 command, exactly the calls the engine's own parser finds at the §3 positions and in the nested scripts §3 parses, with the engine's static argv (and command kind outside nested scripts), and `ShellCall()` the argv a native program receives. | §3, §4 | `powershell.TestOracle`, run only with `STACK_PS_ORACLE=1` (the non-required `powershell-oracle` CI job on `windows-latest` and `ubuntu-latest`). Corpus: C1's tables, the command lists of C2–C5 and positive controls. `testdata/oracle.ps1` (one engine process per version, module autoloading off) classifies each command's position by §3 from the engine's AST and static parameter binding (an invoked block, control-flow bodies, the walked blocks of `ForEach-Object`, `Where-Object` and a local `Invoke-Command` bound within the parameter set that runs them; everything else opaque) and parses the static script of `Invoke-Expression` and `pwsh -Command` down to depth 2. `testdata/oracle_cases.json` declares those names per command and the engines that reject it; `-update-oracle` writes only new commands from a Windows run of both engines, never from `Parse`, and a changed or removed declaration needs `-update-oracle-changes` and prints the difference. Each case's verdict is pass, fail or inconclusive (engine missing or failing, timeout, not `FullLanguage`, malformed version, a required output field missing, null or of the wrong type, a case missing, a position the binding cannot decide, nested scripts included, a module loaded by a lookup, an AST type that names no AST class, a requested alias list or parameter table missing or empty, a stub record missing for a plain native call whose arguments are literals or variable reads or present for any other call, a positive control failing); the test fails on any fail or inconclusive. A missed call, a call from an opaque position, an extra call (static or dynamic name), a static word that differs and a word the engine reads as static that `Parse` makes dynamic all fail; the only declared exception is the unquoted wildcard on non-Windows hosts (`dynamicUnix`). Native calls run only against the test binary as stub, behind a marker argument; words starting with an unquoted `~` are not compared. The engine's elements map to argv by the measured rules of §3 (a `--%` tail, a cmdlet's kept `--%` and one tail argument, a native quoted `--%` dropped with later values split at spaces), and the ambiguous region after a quoted `--%` is compared with neither the AST nor the stub (both parsers drop a parameter token there). Documented differences, asserted by name: 5.1 rejects `&&`, `\|\|` and a trailing `&`; the engine rejects a bare command inside method arguments (only the commands before the first error are compared); 5.1 legacy argument passing of a word holding `"`; `-UseTransaction` (5.1) and Where-Object's comparison parameters absent from the binding tables. Also the alias set of §4 per host and `Remove-Item -p` ambiguous by binding and by parameter metadata |

## 8. Where the contracts are stated

- phase2-core §2.3 **Hook scope**: both shell tools, the recognizer instead of a parser, the SessionStart limits
  without Git Bash.
- phase2-core §3 Library Choices: the PowerShell row, the approved exception to the parser rule.
- phase2-core §4.3: the `PowerShell` tool in the secrets, gh-publish, raw-merge and rmroot rows and in the settings
  wiring; the rmroot row keeps Git Bash path semantics to `Bash` and points here (§4.4) for `PowerShell`.
- phase2-core §6 "Out of static scope": the per-rule list of PowerShell forms that are not judged.
- core-stages §4.5: which hook owns which rule, both matchers, the import allow list.
- phase3-cycle §3.3: `PowerShell` among the replayed tools.

## 9. Test plan and risks

Goldens (payload `tool_name: "PowerShell"`, `@@CWD@@` cwd): `internal/guard/testdata/deny-raw-merge-powershell`,
`deny-raw-merge-powershell-graphql-file` (`.files`), `allow-powershell-string`;
`extensions/guard/testdata/deny-secrets-powershell`, `deny-gh-publish-powershell-body-file` (`.files`),
`deny-gh-publish-powershell-herestring`, `deny-rmroot-powershell-home`, `allow-rmroot-powershell-build`,
`allow-powershell-get-content-readme`. Table tests of §7.2 carry the per-form cases with the OS injected. One golden
per rule carries a `tool_input` recorded on Windows (§2), unchanged: `deny-raw-merge-powershell`
(`& gh.exe pr merge 7 --squash`), `deny-secrets-powershell` (`type C:/work/repo/.dev.vars`; the recorded
`Get-Content .\.DEV.VARS` denies only where case folds, so the C3 table test covers it), `deny-rmroot-powershell-home`
(`Remove-Item -Recurse -Force $HOME`) and `deny-gh-publish-powershell-body-file` (`gh pr create -t t --body-file
b.md`, its `b.md` built from `.files`). `mise exec -- scripts/test` on all three CI OSes; the engine comparison of
§3 (C11) runs in the non-required `powershell-oracle` CI job.

Risks: the subset diverges from pwsh on a construct (miss or false deny; the oracle comparison and flip of §3);
5.1 vs 7 differences (`&&`, `||` and a trailing `&` are 5.1 parse errors, so judging them only denies what would not run; 5.1 passes a native word holding `"` split or unquoted; on 5.1 the
`cd X &&` cwd form does not exist, so a `cd sub; gh … --body-file b.md` stays a miss); the Windows native `~`
expansion depends on the engine version (§3 Words); `Remove-Item` parameter table drift (a new parameter starting
with `r` makes `-r` ambiguous, so the rule over-judges only a call pwsh refuses).
