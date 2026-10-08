package broker

import (
	"embed"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf16"
)

// The broker's PowerShell scripts, one per operation. They are fixed
// text, embedded in the binary: a caller's input reaches them only as
// fields of the JSON object the prelude reads from stdin into $a.
//
//go:embed scripts/*.ps1
var scriptFS embed.FS

// Script names.
const (
	scriptGetVM       = "get-vm"
	scriptConfigureVM = "configure-vm"
	scriptStartVM     = "start-vm"
	scriptStopVM      = "stop-vm"
	scriptRemoveVM    = "remove-vm"
	scriptNewVHD      = "new-vhd"
	scriptSetupStatus = "setup-status"
	scriptSetupApply  = "setup-apply"
)

// Script is the text of the embedded script name.
func Script(name string) (string, error) {
	if strings.ContainsAny(name, `/\.`) {
		return "", fmt.Errorf("no broker script %q", name)
	}
	b, err := scriptFS.ReadFile("scripts/" + name + ".ps1")
	if err != nil {
		return "", fmt.Errorf("no broker script %q", name)
	}
	return string(b), nil
}

// prelude wraps every script: errors stop it, the JSON object on stdin
// becomes $a, and a failure prints {"error": "..."} and exits 1. The
// script's own last output is one compressed JSON object.
const prelude = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$WarningPreference = 'SilentlyContinue'
$InformationPreference = 'SilentlyContinue'
try {
    $a = [Console]::In.ReadToEnd() | ConvertFrom-Json
    & {
%s
    }
} catch {
    [Console]::Out.WriteLine((@{ error = [string]$_.Exception.Message } | ConvertTo-Json -Compress))
    exit 1
}
`

// EncodedCommand is what powershell.exe -EncodedCommand takes for the
// script name: the prelude around the script's fixed text, as base64 of
// UTF-16LE. Only embedded text goes into it, never a caller's input.
func EncodedCommand(name string) (string, error) {
	body, err := Script(name)
	if err != nil {
		return "", err
	}
	src := fmt.Sprintf(prelude, body)
	u := utf16.Encode([]rune(src))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
