# Oracle for TestOracle (oracle_test.go): parses every corpus case with this engine's own parser,
# reports each CommandAst, resolves command names, reads parameter metadata, checks the -p binding
# of Remove-Item, and runs the native calls it may run against the argv stub. It runs nothing else:
# cmdlets are parse-only, and a native call runs only as `& $stub <its arguments>`.
param(
    [Parameter(Mandatory)] [string] $In,
    [Parameter(Mandatory)] [string] $Out
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2

$utf8 = New-Object System.Text.UTF8Encoding $false
$req = [System.IO.File]::ReadAllText($In, $utf8) | ConvertFrom-Json
$L = 'System.Management.Automation.Language'

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

# executes names the commands whose script blocks the recognizer may walk (by parameter shape).
$executes = @('foreach-object', '%', 'foreach', 'where-object', '?', 'where', 'invoke-command', 'icm')

# Get-Context names where a command sits: 'plain' when every enclosing construct is one the
# recognizer walks, 'cond:<name>' inside a block argument of an executing cmdlet, or 'opaque:<why>'.
function Get-Context($cmd) {
    $ctx = 'plain'
    $child = $cmd
    $n = $cmd.Parent
    while ($null -ne $n) {
        if ($n -is [System.Management.Automation.Language.FunctionDefinitionAst]) { return 'opaque:function' }
        if ($n -is [System.Management.Automation.Language.InvokeMemberExpressionAst] -and
            $null -ne $n.Arguments -and $n.Arguments -contains $child -and
            -not ($child -is [System.Management.Automation.Language.SubExpressionAst])) {
            return 'opaque:method-argument'
        }
        if ($n -is [System.Management.Automation.Language.ScriptBlockExpressionAst]) {
            $p = $n.Parent
            if ($p -is [System.Management.Automation.Language.CommandAst]) {
                if ($p.CommandElements[0] -eq $n) {
                    # an invoked block: & { } or . { }
                } else {
                    $name = $p.GetCommandName()
                    $base = if ($null -ne $name) { ($name -split '\\')[-1].ToLowerInvariant() } else { '' }
                    if ($executes -contains $base) { $ctx = "cond:$name" } else { return "opaque:block-argument:$name" }
                }
            } else {
                return 'opaque:block-value'
            }
        }
        $child = $n
        $n = $n.Parent
    }
    $ctx
}

$unsafe = @(
    [System.Management.Automation.Language.CommandAst],
    [System.Management.Automation.Language.ScriptBlockExpressionAst],
    [System.Management.Automation.Language.InvokeMemberExpressionAst],
    [System.Management.Automation.Language.SubExpressionAst],
    [System.Management.Automation.Language.ParenExpressionAst],
    [System.Management.Automation.Language.ArrayExpressionAst],
    [System.Management.Automation.Language.HashtableAst]
)

# Test-Inert reports that an argument element holds only literals and variable reads.
function Test-Inert($e) {
    $bad = $e.FindAll({ param($a) foreach ($t in $unsafe) { if ($a -is $t) { return $true } }; $false }, $true)
    @($bad).Count -eq 0
}

$cases = New-Object System.Collections.ArrayList
$names = @{}
foreach ($c in $req.cases) {
    $tokens = $null; $errors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseInput($c.text, [ref]$tokens, [ref]$errors)
    $cmds = New-Object System.Collections.ArrayList
    $all = $ast.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true)
    foreach ($cmd in $all) {
        $name = $cmd.GetCommandName()
        if ($null -ne $name) { $names[$name] = $true }
        $els = @($cmd.CommandElements | ForEach-Object { Convert-Element $_ })
        $inert = $true
        for ($i = 1; $i -lt $cmd.CommandElements.Count; $i++) {
            if (-not (Test-Inert $cmd.CommandElements[$i])) { $inert = $false }
        }
        [void]$cmds.Add([ordered]@{
            name = $name
            op = [string]$cmd.InvocationOperator
            end = $cmd.Extent.EndOffset
            ctx = Get-Context $cmd
            elements = $els
            inert = $inert
            args = @($cmd.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.Extent.Text })
        })
    }
    [void]$cases.Add([ordered]@{
        id = $c.id
        errors = @($errors | ForEach-Object { [ordered]@{ id = $_.ErrorId; offset = $_.Extent.StartOffset; text = $_.Extent.Text } })
        commands = @($cmds)
    })
}
foreach ($n in $req.names) { $names[$n] = $true }

# Resolve each name without running it: 'cmdlet:<Name>' (directly or through an alias), 'function',
# 'alias:<definition>' for an alias of something else, or 'native'.
$resolved = [ordered]@{}
foreach ($n in @($names.Keys | Sort-Object)) {
    $r = 'native'
    if ($n -notmatch '[/:]' -and $n -notmatch '\.exe$' -and ($n -notmatch '\\' -or $n -match '^[A-Za-z_][A-Za-z0-9_.]*\\[^\\]+$')) {
        $found = @(Get-Command -Name ([System.Management.Automation.WildcardPattern]::Escape($n)) -CommandType Alias, Cmdlet, Function -ErrorAction SilentlyContinue)
        if ($found.Count -gt 0) {
            $f = $found[0]
            if ($f.CommandType -eq 'Alias') {
                # ResolvedCommand stays empty until the target's module is loaded.
                $t = @(Get-Command -Name $f.Definition -CommandType Cmdlet -ErrorAction SilentlyContinue)
                if ($t.Count -gt 0) { $r = "cmdlet:$($t[0].Name)" } else { $r = "alias:$($f.Definition)" }
            } elseif ($f.CommandType -eq 'Cmdlet') {
                $r = "cmdlet:$($f.Name)"
            } else {
                $r = "function"
            }
        }
    }
    $resolved[$n] = $r
}

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
        [ordered]@{ name = $_.Name; aliases = @($_.Aliases); switch = $_.SwitchParameter }
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

$result = [ordered]@{
    version = $PSVersionTable.PSVersion.ToString()
    edition = [string]$PSVersionTable.PSEdition
    languageMode = [string]$ExecutionContext.SessionState.LanguageMode
    nativeArgumentPassing = [string](Get-Variable -Name PSNativeCommandArgumentPassing -ValueOnly -ErrorAction SilentlyContinue)
    home = $HOME
    cases = @($cases)
    resolved = $resolved
    aliasesOf = $aliasesOf
    params = $params
    pBinding = $pBinding
    stubRuns = 0
    stubSame = @{}
}
$json = ConvertTo-Json -InputObject $result -Depth 30 -Compress
[System.IO.File]::WriteAllText($Out, $json, $utf8)

# Native calls with inert arguments run against the stub, one line each; the stub appends its argv
# to the file named by STACK_PS_ORACLE_STUB under the id in STACK_PS_ORACLE_ID. A call whose
# argument text was already run is not run again: stubSame maps its id to the id that ran.
Set-StrictMode -Off # an unset variable in an argument is $null, as in a session
$stub = $req.stub
$runs = 0
$ran = New-Object 'System.Collections.Generic.Dictionary[string,string]' ([System.StringComparer]::Ordinal)
$same = New-Object 'System.Collections.Generic.Dictionary[string,string]' ([System.StringComparer]::Ordinal)
foreach ($c in $cases) {
    if (@($c.errors).Count -gt 0) { continue }
    for ($i = 0; $i -lt @($c.commands).Count; $i++) {
        $cmd = $c.commands[$i]
        if ($null -eq $cmd.name -or $resolved[$cmd.name] -ne 'native' -or -not $cmd.inert) { continue }
        $text = '& $stub ' + ($cmd.args -join ' ')
        $tk = $null; $er = $null
        $sb = [System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$tk, [ref]$er)
        $inner = @($sb.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
        if (@($er).Count -gt 0 -or $inner.Count -ne 1 -or -not (@($inner[0].CommandElements | Select-Object -Skip 1 | Where-Object { -not (Test-Inert $_) }).Count -eq 0)) {
            continue
        }
        $id = "$($c.id)/$i"
        if ($ran.ContainsKey($text)) { $same[$id] = $ran[$text]; continue }
        $ran[$text] = $id
        $env:STACK_PS_ORACLE_ID = $id
        & ([scriptblock]::Create($text))
        $runs++
    }
}
$result.stubRuns = $runs
$result.stubSame = $same
$json = ConvertTo-Json -InputObject $result -Depth 30 -Compress
[System.IO.File]::WriteAllText($Out, $json, $utf8)
