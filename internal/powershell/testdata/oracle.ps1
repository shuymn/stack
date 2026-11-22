# Oracle for TestOracle (oracle_test.go). It parses every corpus case with this engine's own parser
# and reports each CommandAst with the position class of powershell-guard section 3 ('plain' when
# every enclosing construct is one the recognizer walks, else 'opaque:<why>'), decided from the
# engine's AST and static parameter binding, never from the Go recognizer. It also parses the
# static script of each Invoke-Expression and pwsh -Command it finds, resolves command names, reads
# alias and parameter metadata, checks the -p binding of Remove-Item, and runs the native calls it
# may run against the argv stub. It runs nothing else: cmdlets are parse-only, module autoloading
# is off, and a native call runs only as `& $stub $marker <its arguments>` when every argument is
# a literal, a variable read or a string holding only variable reads.
param(
    [Parameter(Mandatory)] [string] $In,
    [Parameter(Mandatory)] [string] $Out
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2
$PSModuleAutoLoadingPreference = 'None'
# The cmdlets the recognizer judges live in these modules and Microsoft.PowerShell.Core.
Import-Module Microsoft.PowerShell.Management, Microsoft.PowerShell.Utility

$utf8 = New-Object System.Text.UTF8Encoding $false
$req = [System.IO.File]::ReadAllText($In, $utf8) | ConvertFrom-Json
$A = 'System.Management.Automation.Language'

function Convert-Element($e) {
    $o = [ordered]@{ k = 'expr'; t = $e.Extent.Text }
    if ($e -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
        $o.k = 'str'; $o.v = $e.Value; $o.q = [string]$e.StringConstantType
    } elseif ($e -is [System.Management.Automation.Language.ConstantExpressionAst]) {
        $o.k = 'str'; $o.v = $e.Extent.Text; $o.q = 'Constant'
    } elseif ($e -is [System.Management.Automation.Language.CommandParameterAst]) {
        $o.k = 'param'; $o.pn = $e.ParameterName
        if ($null -ne $e.Argument) {
            $o.prefix = $e.Extent.Text.Substring(0, $e.Argument.Extent.StartOffset - $e.Extent.StartOffset)
            $o.arg = Convert-Element $e.Argument
        }
    } elseif ($e -is [System.Management.Automation.Language.ArrayLiteralAst]) {
        $o.k = 'array'; $o.items = @($e.Elements | ForEach-Object { Convert-Element $_ })
    } elseif ($e -is [System.Management.Automation.Language.ScriptBlockExpressionAst]) {
        $o.k = 'block'
    }
    $o
}

# Resolve-Name says how this engine resolves a command name without running it: 'cmdlet:<Name>'
# (directly or through an alias), 'function', 'alias:<definition>' for an alias of something
# else, or 'native'.
$resolved = [ordered]@{}
function Resolve-Name([string] $n) {
    if ($resolved.Contains($n)) { return $resolved[$n] }
    $r = 'native'
    if ($n -notmatch '[/:]' -and $n -notmatch '\.exe$' -and ($n -notmatch '\\' -or $n -match '^[A-Za-z_][A-Za-z0-9_.]*\\[^\\]+$')) {
        $found = @(Get-Command -Name ([System.Management.Automation.WildcardPattern]::Escape($n)) -CommandType Alias, Cmdlet, Function -ErrorAction SilentlyContinue)
        if ($found.Count -gt 0) {
            $f = $found[0]
            if ($f.CommandType -eq 'Alias') {
                $t = @(Get-Command -Name $f.Definition -CommandType Cmdlet -ErrorAction SilentlyContinue)
                if ($t.Count -gt 0) { $r = "cmdlet:$($t[0].Name)" } else { $r = "alias:$($f.Definition)" }
            } elseif ($f.CommandType -eq 'Cmdlet') {
                $r = "cmdlet:$($f.Name)"
            } else {
                $r = 'function'
            }
        }
    }
    $resolved[$n] = $r
    $r
}

# blockSets are, per cmdlet whose blocks section 3 walks, the parameter set that runs them (from
# this engine's metadata) and the parameters whose block is walked.
$blockSets = @{}
foreach ($s in @(
        @{ cmdlet = 'ForEach-Object'; set = 'ScriptBlockSet'; walk = @('Process', 'Begin', 'End') },
        @{ cmdlet = 'Where-Object'; set = 'ScriptBlockSet'; walk = @('FilterScript') },
        @{ cmdlet = 'Invoke-Command'; set = 'InProcess'; walk = @('ScriptBlock') })) {
    $ps = @((Get-Command -Name $s.cmdlet -CommandType Cmdlet).ParameterSets | Where-Object { $_.Name -eq $s.set })
    if ($ps.Count -ne 1) { throw "no parameter set $($s.set) for $($s.cmdlet)" }
    $blockSets[$s.cmdlet] = @{ params = @($ps[0].Parameters | ForEach-Object { $_.Name }); walk = $s.walk }
}

# Test-WalkedBlock reports whether section 3 walks $block, an argument of $cmd: the cmdlet runs
# blocks, the engine binds every argument within the parameter set that runs them, and $block is
# bound to a walked parameter (the first positional block binds to the first of them).
function Test-WalkedBlock($cmd, $block) {
    $name = $cmd.GetCommandName()
    if ($null -eq $name) { return $false }
    $r = Resolve-Name $name
    if (-not $r.StartsWith('cmdlet:')) { return $false }
    $set = $blockSets[$r.Substring(7)]
    if ($null -eq $set) { return $false }
    $b = [System.Management.Automation.Language.StaticParameterBinder]::BindCommand($cmd, $true)
    $end = -1
    for ($i = 1; $i -lt $cmd.CommandElements.Count; $i++) {
        if ($cmd.CommandElements[$i].Extent.Text -eq '--') { $end = $i; break }
    }
    if ($end -gt 0 -and (@($b.BindingExceptions.Keys) -join ',') -eq '-') {
        # The static binder reports the end-of-parameters `--` as an unknown parameter, while the
        # engine binds every later argument positionally: bind the command without it.
        $rest = @($cmd.CommandElements | Select-Object -Skip ($end + 1))
        foreach ($e in $rest) {
            if ($e -is [System.Management.Automation.Language.CommandParameterAst]) { return $false }
        }
        $texts = New-Object System.Collections.ArrayList
        $at = -1
        for ($i = 0; $i -lt $cmd.CommandElements.Count; $i++) {
            if ($i -eq $end) { continue }
            if ($cmd.CommandElements[$i] -eq $block) { $at = $texts.Count }
            [void]$texts.Add($cmd.CommandElements[$i].Extent.Text)
        }
        if ($at -lt 0) { return $false }
        $tk = $null; $er = $null
        $re = [System.Management.Automation.Language.Parser]::ParseInput(($texts -join ' '), [ref]$tk, [ref]$er)
        $twin = @(Find-Commands $re)
        if (@($er).Count -gt 0 -or $twin.Count -lt 1 -or $twin[0].CommandElements.Count -ne $texts.Count) { return $false }
        $cmd = $twin[0]
        $block = $cmd.CommandElements[$at]
        $b = [System.Management.Automation.Language.StaticParameterBinder]::BindCommand($cmd, $true)
    }
    if ($b.BindingExceptions.Count -gt 0) { return $false }
    foreach ($k in $b.BoundParameters.Keys) {
        if ($set.params -notcontains $k) { return $false }
    }
    foreach ($w in $set.walk) {
        if ($b.BoundParameters.ContainsKey($w) -and $b.BoundParameters[$w].Value -eq $block) { return $true }
    }
    $false
}

# Get-Context returns 'plain' when every construct around $cmd is one section 3 walks, else
# 'opaque:<why>'.
function Get-Context($cmd) {
    $child = $cmd
    $n = $cmd.Parent
    while ($null -ne $n) {
        if ($n -is [System.Management.Automation.Language.FunctionDefinitionAst]) { return 'opaque:function' }
        if ($n -is [System.Management.Automation.Language.TypeDefinitionAst]) { return 'opaque:type' }
        if ($n -is [System.Management.Automation.Language.ParamBlockAst]) { return 'opaque:param' }
        if ($n -is [System.Management.Automation.Language.InvokeMemberExpressionAst] -and
            $null -ne $n.Arguments -and $n.Arguments -contains $child -and
            -not ($child -is [System.Management.Automation.Language.SubExpressionAst])) {
            return 'opaque:method-argument'
        }
        if ($n -is [System.Management.Automation.Language.ScriptBlockExpressionAst]) {
            $p = $n.Parent
            if ($p -is [System.Management.Automation.Language.CommandParameterAst]) { $p = $p.Parent }
            if ($p -isnot [System.Management.Automation.Language.CommandAst]) { return 'opaque:block-value' }
            $invoked = $p.CommandElements[0] -eq $n
            if (-not $invoked -and -not (Test-WalkedBlock $p $n)) {
                return "opaque:block-argument:$($p.GetCommandName())"
            }
        }
        $child = $n
        $n = $n.Parent
    }
    'plain'
}

# Test-StubArg reports whether an argument may reach the stub: a literal, a variable read, a string
# holding only variable reads, a parameter with such a value, or a list of them.
function Test-StubArg($e) {
    if ($e -is [System.Management.Automation.Language.StringConstantExpressionAst] -or
        $e -is [System.Management.Automation.Language.ConstantExpressionAst] -or
        $e -is [System.Management.Automation.Language.VariableExpressionAst]) { return $true }
    if ($e -is [System.Management.Automation.Language.ExpandableStringExpressionAst]) {
        foreach ($x in $e.NestedExpressions) {
            if ($x -isnot [System.Management.Automation.Language.VariableExpressionAst]) { return $false }
        }
        return $true
    }
    if ($e -is [System.Management.Automation.Language.CommandParameterAst]) {
        return ($null -eq $e.Argument) -or (Test-StubArg $e.Argument)
    }
    if ($e -is [System.Management.Automation.Language.ArrayLiteralAst]) {
        foreach ($x in $e.Elements) { if (-not (Test-StubArg $x)) { return $false } }
        return $true
    }
    $false
}

function Find-Commands($ast) {
    @($ast.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
}

$pwshSwitches = @('-noprofile', '-nop', '-noninteractive', '-noni', '-nologo', '-nol', '-noexit', '-noe', '-mta', '-sta', '-login', '-l')
$pwshValued = @('-executionpolicy', '-ex', '-ep', '-windowstyle', '-w', '-outputformat', '-o', '-of', '-inputformat', '-if', '-workingdirectory', '-wd')

# Get-NestedText returns the one static script a plain command runs: the -Command of
# Invoke-Expression, or of pwsh/powershell -Command|-c after known options; else $null.
function Get-NestedText($cmd) {
    $name = $cmd.GetCommandName()
    if ($null -eq $name) { return $null }
    $r = Resolve-Name $name
    if ($r -eq 'cmdlet:Invoke-Expression') {
        $b = [System.Management.Automation.Language.StaticParameterBinder]::BindCommand($cmd, $true)
        if ($b.BindingExceptions.Count -gt 0 -or -not $b.BoundParameters.ContainsKey('Command')) { return $null }
        $v = $b.BoundParameters['Command'].Value
        if ($v -is [System.Management.Automation.Language.StringConstantExpressionAst]) { return $v.Value }
        return $null
    }
    $base = (($name -split '[\\/]')[-1]) -replace '(?i)\.exe$', ''
    if ($r -ne 'native' -or ($base -ne 'pwsh' -and $base -ne 'powershell')) { return $null }
    $els = $cmd.CommandElements
    for ($i = 1; $i -lt $els.Count; $i++) {
        $e = $els[$i]
        if ($e -is [System.Management.Automation.Language.CommandParameterAst] -and $null -eq $e.Argument) {
            $t = '-' + $e.ParameterName.ToLowerInvariant()
        } elseif ($e -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $t = $e.Value.ToLowerInvariant()
        } else {
            return $null
        }
        if ($t -eq '-c' -or $t -eq '-command') {
            if ($i + 2 -eq $els.Count -and $els[$i + 1] -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
                return $els[$i + 1].Value
            }
            return $null
        }
        if ($pwshValued -contains $t) { $i++; continue }
        if ($pwshSwitches -notcontains $t) { return $null }
    }
    $null
}

# Get-NestedNames returns the static names of the plain commands of the nested scripts that the
# plain commands of $ast run, down to depth 2 (as the recognizer's allowance), in source order.
function Get-NestedNames($ast, [int] $depth) {
    $out = New-Object System.Collections.ArrayList
    if ($depth -ge 2) { return , $out }
    foreach ($cmd in (Find-Commands $ast)) {
        if ((Get-Context $cmd) -ne 'plain') { continue }
        $text = Get-NestedText $cmd
        if ($null -eq $text) { continue }
        $tk = $null; $er = $null
        $inner = [System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$tk, [ref]$er)
        if (@($er).Count -gt 0) { continue }
        foreach ($c in (Find-Commands $inner)) {
            $n = $c.GetCommandName()
            if ($null -ne $n -and (Get-Context $c) -eq 'plain') { [void]$out.Add($n) }
        }
        foreach ($n in (Get-NestedNames $inner ($depth + 1))) { [void]$out.Add($n) }
    }
    , $out
}

$stub = $req.stub
$marker = $req.marker

# Get-StubText returns the script that runs a native call's arguments against the stub, or $null
# when an argument is not one the stub may receive.
function Get-StubText($cmd) {
    for ($i = 1; $i -lt $cmd.CommandElements.Count; $i++) {
        if (-not (Test-StubArg $cmd.CommandElements[$i])) { return $null }
    }
    $text = '& $stub $marker ' + (@($cmd.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.Extent.Text }) -join ' ')
    $tk = $null; $er = $null
    $sb = [System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$tk, [ref]$er)
    $inner = @(Find-Commands $sb)
    if (@($er).Count -gt 0 -or $inner.Count -ne 1) { return $null }
    for ($i = 1; $i -lt $inner[0].CommandElements.Count; $i++) {
        if (-not (Test-StubArg $inner[0].CommandElements[$i])) { return $null }
    }
    $text
}

$cases = New-Object System.Collections.ArrayList
$stubTexts = @{}
foreach ($c in $req.cases) {
    $tokens = $null; $errors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseInput($c.text, [ref]$tokens, [ref]$errors)
    $failed = @($errors).Count -gt 0
    $cmds = New-Object System.Collections.ArrayList
    $all = @(Find-Commands $ast)
    for ($i = 0; $i -lt $all.Count; $i++) {
        $cmd = $all[$i]
        $name = $cmd.GetCommandName()
        $text = $null
        if ($null -ne $name -and -not $failed -and (Resolve-Name $name) -eq 'native') {
            $text = Get-StubText $cmd
        }
        if ($null -ne $text) { $stubTexts["$($c.id)/$i"] = $text }
        [void]$cmds.Add([ordered]@{
            name = $name
            end = $cmd.Extent.EndOffset
            ctx = Get-Context $cmd
            elements = @($cmd.CommandElements | ForEach-Object { Convert-Element $_ })
            eligible = $null -ne $text
        })
    }
    [void]$cases.Add([ordered]@{
        id = $c.id
        errors = @($errors | ForEach-Object { [ordered]@{ id = $_.ErrorId; offset = $_.Extent.StartOffset; text = $_.Extent.Text } })
        commands = @($cmds)
        nested = @((Get-NestedNames $ast 0) | ForEach-Object { $_ })
    })
}
foreach ($n in $req.names) { [void](Resolve-Name $n) }

# Built-in aliases of the cmdlets the recognizer judges, as this host defines them.
$aliasesOf = [ordered]@{}
foreach ($cmdlet in $req.cmdlets) {
    $aliasesOf[$cmdlet] = @(Get-Alias -Definition $cmdlet -ErrorAction SilentlyContinue | ForEach-Object { $_.Name } | Sort-Object)
}

# Parameter metadata of the cmdlets with a binding table.
$params = [ordered]@{}
foreach ($cmdlet in $req.tables) {
    $cm = Get-Command -Name $cmdlet -CommandType Cmdlet
    $params[$cmdlet] = @($cm.Parameters.Values | Sort-Object Name | ForEach-Object {
            [ordered]@{ name = $_.Name; aliases = @($_.Aliases); switch = [bool]$_.SwitchParameter }
        })
}

# Binding of `Remove-Item -p`: refused as ambiguous before anything runs. -WhatIf and a path that
# does not exist make it remove nothing even if it bound.
$pBinding = ''
try {
    Remove-Item -p $req.missing -WhatIf -ErrorAction Stop
    $pBinding = 'bound'
} catch {
    $pBinding = $_.FullyQualifiedErrorId
}

# Native calls run against the stub, one line each; the stub appends its argv to the file named by
# STACK_PS_ORACLE_STUB under the id in STACK_PS_ORACLE_ID. A call whose argument text was already
# run is not run again: stubSame maps its id to the id that ran.
Set-StrictMode -Off # an unset variable in an argument is $null, as in a session
$runs = 0
$ran = New-Object 'System.Collections.Generic.Dictionary[string,string]' ([System.StringComparer]::Ordinal)
$same = New-Object 'System.Collections.Generic.Dictionary[string,string]' ([System.StringComparer]::Ordinal)
foreach ($id in @($stubTexts.Keys | Sort-Object)) {
    $text = $stubTexts[$id]
    if ($ran.ContainsKey($text)) { $same[$id] = $ran[$text]; continue }
    $ran[$text] = $id
    $env:STACK_PS_ORACLE_ID = $id
    & ([scriptblock]::Create($text))
    $runs++
}
Set-StrictMode -Version 2

$result = [ordered]@{
    version = $PSVersionTable.PSVersion.ToString()
    edition = [string]$PSVersionTable.PSEdition
    languageMode = [string]$ExecutionContext.SessionState.LanguageMode
    nativeArgumentPassing = [string](Get-Variable -Name PSNativeCommandArgumentPassing -ValueOnly -ErrorAction SilentlyContinue)
    home = $HOME
    modules = @(Get-Module | ForEach-Object { $_.Name } | Sort-Object)
    cases = @($cases)
    resolved = $resolved
    aliasesOf = $aliasesOf
    params = $params
    pBinding = $pBinding
    stubRuns = $runs
    stubSame = $same
}
[System.IO.File]::WriteAllText($Out, (ConvertTo-Json -InputObject $result -Depth 30 -Compress), $utf8)
