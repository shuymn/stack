package powershell

import (
	"path"
	"regexp"
	"strings"
)

// alias is a built-in alias; windows marks one PowerShell defines on Windows only.
type alias struct {
	name    string
	windows bool
}

var aliases = map[string]alias{
	"%":       {"ForEach-Object", false},
	"foreach": {"ForEach-Object", false},
	"?":       {"Where-Object", false},
	"where":   {"Where-Object", false},
	"icm":     {"Invoke-Command", false},
	"iex":     {"Invoke-Expression", false},
	"cd":      {"Set-Location", false},
	"sl":      {"Set-Location", false},
	"chdir":   {"Set-Location", false},
	"pushd":   {"Push-Location", false},
	"popd":    {"Pop-Location", false},
	"ri":      {"Remove-Item", false},
	"del":     {"Remove-Item", false},
	"erase":   {"Remove-Item", false},
	"rd":      {"Remove-Item", false},
	"rm":      {"Remove-Item", true},
	"rmdir":   {"Remove-Item", true},
	"echo":    {"Write-Output", false},
	"write":   {"Write-Output", true},
	"gc":      {"Get-Content", false},
	"type":    {"Get-Content", false},
	"cat":     {"Get-Content", true},
	"sv":      {"Set-Variable", false},
	"set":     {"Set-Variable", false},
	"si":      {"Set-Item", false},
	"sajb":    {"Start-Job", false},
	"saps":    {"Start-Process", false},
	"start":   {"Start-Process", true},
	"gci":     {"Get-ChildItem", false},
	"dir":     {"Get-ChildItem", false},
	"ls":      {"Get-ChildItem", true},
	"gm":      {"Get-Member", false},
	"ndr":     {"New-PSDrive", false},
	"mount":   {"New-PSDrive", true},
}

// verbs are the approved verbs plus ForEach and Where, so `Verb-Noun` names a cmdlet.
var verbs = func() map[string]bool {
	m := map[string]bool{}
	for v := range strings.FieldsSeq(`add approve assert backup block build checkpoint clear close
		compare complete compress confirm connect convert convertfrom convertto copy debug deny deploy
		disable disconnect dismount edit enable enter exit expand export find foreach format get grant
		group hide import initialize install invoke join limit lock measure merge mount move new open
		optimize out ping pop protect publish push read receive redo register remove rename repair
		request reset resize resolve restart restore resume revoke save search select send set show
		skip split start step stop submit suspend switch sync test trace unblock undo uninstall
		unlock unprotect unpublish unregister update use wait watch where write`) {
		m[v] = true
	}
	return m
}()

// canonical spells the cmdlets the recognizer judges.
var canonical = func() map[string]string {
	m := map[string]string{}
	for n := range strings.FieldsSeq(`Remove-Item Set-Location Push-Location Pop-Location
		Invoke-Command Invoke-Expression ForEach-Object Where-Object Set-Variable Set-Item
		Write-Output Get-Content Start-Job Get-Member New-PSDrive`) {
		m[strings.ToLower(n)] = n
	}
	return m
}()

var (
	cmdletRe = regexp.MustCompile(`^([A-Za-z]+)-[A-Za-z][A-Za-z0-9]*$`)
	moduleRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

// classify returns the command kind and normalised name of a command name word.
func classify(name Word, os OS) (Kind, string) {
	if !name.Static {
		return Native, ""
	}
	t := name.Text
	if i := strings.LastIndexByte(t, '\\'); i > 0 && moduleRe.MatchString(t[:i]) &&
		isCmdletName(t[i+1:]) {
		return Cmdlet, cmdletName(t[i+1:])
	}
	if strings.ContainsAny(t, `/\:`) {
		return Native, exeBase(t)
	}
	lower := strings.ToLower(t)
	if strings.HasSuffix(lower, ".exe") {
		return Native, exeBase(t)
	}
	if a, ok := aliases[lower]; ok && (!a.windows || os == Windows) {
		return Cmdlet, a.name
	}
	if isCmdletName(t) {
		return Cmdlet, cmdletName(t)
	}
	return Native, t
}

func isCmdletName(s string) bool {
	m := cmdletRe.FindStringSubmatch(s)
	return m != nil && verbs[strings.ToLower(m[1])]
}

func cmdletName(s string) string {
	if c, ok := canonical[strings.ToLower(s)]; ok {
		return c
	}
	return s
}

// exeBase returns the last component of a command path, without `.exe`.
func exeBase(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
		name = name[:len(name)-4]
	}
	return name
}

// param is one cmdlet parameter.
type param struct {
	name     string
	aliases  []string
	isSwitch bool
}

// table is a cmdlet's parameters for binding.
type table struct {
	params []param
	// positional names the parameters positional arguments bind to, in order.
	positional []string
	// remaining takes the positional arguments past positional ("" when there is none).
	remaining string
	// rejectMixedPaths: `-Path` and `-LiteralPath` are in different parameter sets, so pwsh
	// refuses a call that binds both, also when one of them would only take a positional argument
	// (`Remove-Item -LiteralPath '.git'build` is PositionalParameterNotFound, 5.1 and 7.7).
	rejectMixedPaths bool
}

var commonParams = []param{
	{"Verbose", []string{"vb"}, true},
	{"Debug", []string{"db"}, true},
	{"ErrorAction", []string{"ea"}, false},
	{"WarningAction", []string{"wa"}, false},
	{"InformationAction", []string{"infa"}, false},
	{"ProgressAction", []string{"proga"}, false},
	{"ErrorVariable", []string{"ev"}, false},
	{"WarningVariable", []string{"wv"}, false},
	{"InformationVariable", []string{"iv"}, false},
	{"OutVariable", []string{"ov"}, false},
	{"OutBuffer", []string{"ob"}, false},
	{"PipelineVariable", []string{"pv"}, false},
}

var shouldProcess = []param{
	{"WhatIf", []string{"wi"}, true},
	{"Confirm", []string{"cf"}, true},
}

func withCommon(sp bool, ps ...param) []param {
	ps = append(ps, commonParams...)
	if sp {
		ps = append(ps, shouldProcess...)
	}
	return ps
}

var (
	literalPath = param{"LiteralPath", []string{"PSPath", "LP"}, false}

	tables = map[string]*table{
		"Remove-Item": {
			params: withCommon(true,
				param{"Path", nil, false}, literalPath,
				param{"Filter", nil, false}, param{"Include", nil, false},
				param{"Exclude", nil, false}, param{"Recurse", nil, true},
				param{"Force", nil, true}, param{"Credential", nil, false},
				param{"Stream", nil, false}),
			positional:       []string{"Path"},
			rejectMixedPaths: true,
		},
		"Set-Location": {
			params: withCommon(false,
				param{"Path", nil, false}, literalPath,
				param{"PassThru", nil, true}, param{"StackName", nil, false}),
			positional:       []string{"Path"},
			rejectMixedPaths: true,
		},
		"New-PSDrive": {
			params: withCommon(true,
				param{"Name", nil, false}, param{"PSProvider", []string{"PSP"}, false},
				param{"Root", nil, false}, param{"Description", nil, false},
				param{"Scope", nil, false}, param{"Persist", nil, true},
				param{"Credential", nil, false}),
			positional: []string{"Name", "PSProvider", "Root", "Description"},
		},
		"Invoke-Expression": {
			params:     withCommon(false, param{"Command", nil, false}),
			positional: []string{"Command"},
		},
		"ForEach-Object": {
			params: withCommon(true,
				param{"Process", nil, false}, param{"Begin", nil, false},
				param{"End", nil, false}, param{"InputObject", nil, false},
				param{"MemberName", nil, false}, param{"ArgumentList", []string{"Args"}, false},
				param{"RemainingScripts", nil, false}, param{"Parallel", nil, false},
				param{"ThrottleLimit", nil, false}, param{"TimeoutSeconds", nil, false},
				param{"AsJob", nil, true}, param{"UseNewRunspace", nil, true}),
			positional: []string{"Process"},
			remaining:  "RemainingScripts",
		},
		"Where-Object": {
			params: withCommon(false,
				param{"FilterScript", nil, false}, param{"Property", nil, false},
				param{"Value", nil, false}, param{"InputObject", nil, false}),
			positional: []string{"FilterScript"},
		},
		"Invoke-Command": {
			params: withCommon(
				false,
				param{"ScriptBlock", []string{"Command"}, false},
				param{"ArgumentList", []string{"Args"}, false},
				param{"NoNewScope", nil, true},
				param{"InputObject", nil, false},
				param{"ComputerName", []string{"Cn"}, false},
				param{"Session", nil, false},
				param{"HostName", nil, false},
				param{"VMName", nil, false},
				param{"VMId", []string{"VMGuid"}, false},
				param{"ContainerId", nil, false},
				param{"ConnectionUri", []string{"URI", "CU"}, false},
				param{"FilePath", []string{"PSPath"}, false},
				param{"Credential", nil, false},
				param{"Port", nil, false},
				param{"UseSSL", nil, true},
				param{"AsJob", nil, true},
				param{"HideComputerName", []string{"HCN"}, true},
				param{"JobName", nil, false},
				param{"ThrottleLimit", nil, false},
				param{"InDisconnectedSession", []string{"Disconnected"}, true},
				param{"SessionName", nil, false},
				param{"SessionOption", nil, false},
				param{"Authentication", nil, false},
				param{"CertificateThumbprint", nil, false},
				param{"ConfigurationName", nil, false},
				param{"ApplicationName", nil, false},
				param{"EnableNetworkAccess", nil, true},
				param{"AllowRedirection", nil, true},
				param{
					"RunAsAdministrator",
					nil,
					true,
				},
				param{"KeyFilePath", []string{"IdentityFilePath"}, false},
				param{"Subsystem", nil, false},
				param{"UserName", nil, false},
				param{"SSHTransport", nil, true},
				param{"SSHConnection", nil, false},
				param{"Options", nil, false},
				param{"RemoteDebug", nil, true},
				param{"ConnectingTimeout", nil, false},
			),
			positional: []string{"ScriptBlock"},
		},
	}
)

// lookup resolves a written parameter name: an exact name or alias, else a unique prefix.
func (t *table) lookup(name string) (param, bool) {
	n := strings.ToLower(name)
	for _, p := range t.params {
		if strings.ToLower(p.name) == n {
			return p, true
		}
		for _, a := range p.aliases {
			if strings.ToLower(a) == n {
				return p, true
			}
		}
	}
	var found *param
	for i, p := range t.params {
		match := strings.HasPrefix(strings.ToLower(p.name), n)
		for _, a := range p.aliases {
			match = match || strings.HasPrefix(strings.ToLower(a), n)
		}
		if match {
			if found != nil {
				return param{}, false // ambiguous
			}
			found = &t.params[i]
		}
	}
	if found == nil {
		return param{}, false
	}
	return *found, true
}

// item is one argument as written: a parameter token (with its colon-bound value), or a value
// (one word, or the elements of a `,` list).
type item struct {
	words []Word
	param bool
	colon bool
}

// bind binds items to t as PowerShell does and sets each word's Param. It reports false when pwsh
// refuses the call.
func (t *table) bind(items []item) bool {
	bound := map[string]bool{}
	var positionals []int
	endParams := false
	for i := 0; i < len(items); i++ {
		it := items[i]
		if !it.param || endParams {
			positionals = append(positionals, i)
			continue
		}
		pw := &items[i].words[0]
		if !it.colon && pw.Text == "--" {
			endParams = true
			continue
		}
		p, ok := t.lookup(paramName(pw))
		if !ok || bound[p.name] {
			return false
		}
		bound[p.name] = true
		setParam(items[i].words, p.name)
		switch {
		case it.colon || p.isSwitch:
		case i+1 < len(items) && !items[i+1].param:
			i++
			setParam(items[i].words, p.name)
		default:
			return false // a parameter that takes a value is missing it
		}
	}
	next := 0
	for _, i := range positionals {
		for next < len(t.positional) && bound[t.positional[next]] {
			next++
		}
		switch {
		case next < len(t.positional):
			bound[t.positional[next]] = true
			setParam(items[i].words, t.positional[next])
			next++
		case t.remaining != "":
			setParam(items[i].words, t.remaining)
		default:
			return false
		}
	}
	return !t.rejectMixedPaths || !bound["Path"] || !bound["LiteralPath"]
}

func setParam(ws []Word, name string) {
	for i := range ws {
		ws[i].Param = name
	}
}

// paramName returns the name written in a parameter token, without its dash and colon.
func paramName(w *Word) string {
	s := w.Raw
	if w.Flag != "" {
		s = w.Flag
	}
	s = strings.TrimSuffix(s, ":")
	for _, d := range dashes {
		if strings.HasPrefix(s, d) {
			return s[len(d):]
		}
	}
	return s
}

// dashes are the characters PowerShell accepts as the dash of a parameter.
var dashes = []string{"-", "–", "—", "―"}
