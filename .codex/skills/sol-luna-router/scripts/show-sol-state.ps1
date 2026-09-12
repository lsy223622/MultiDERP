[CmdletBinding()]
param(
    [string]$ProjectRoot
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not $ProjectRoot) {
    $ProjectRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..\..\..\..')).Path
}
else {
    $ProjectRoot = (Resolve-Path -LiteralPath $ProjectRoot).Path
}

$statePath = Join-Path $ProjectRoot '.codex\sol-luna\.state\sol-thread.json'
if (-not (Test-Path -LiteralPath $statePath)) {
    Write-Output 'No persistent CLI Sol thread has been created for this project yet.'
    exit 0
}

Get-Content -LiteralPath $statePath -Raw -Encoding UTF8
