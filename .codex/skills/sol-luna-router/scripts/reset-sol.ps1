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
$lockPath = Join-Path $ProjectRoot '.codex\sol-luna\.state\sol-thread.lock'

if (Test-Path -LiteralPath $lockPath) {
    throw "A Sol consultation lock exists: $lockPath. Do not reset while a consultation may be running."
}

if (-not (Test-Path -LiteralPath $statePath)) {
    Write-Output 'No persistent CLI Sol state exists for this project.'
    exit 0
}

$old = Get-Content -LiteralPath $statePath -Raw -Encoding UTF8 | ConvertFrom-Json
Remove-Item -LiteralPath $statePath -Force
Write-Output "Removed local persistent Sol pointer. Previous thread id: $($old.thread_id)"
Write-Output 'The old Codex conversation itself was not deleted or archived.'
