[CmdletBinding()]
param(
    [string]$ProjectRoot
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Resolve-ProjectRoot {
    param([string]$ExplicitRoot)

    if ($ExplicitRoot) {
        return (Resolve-Path -LiteralPath $ExplicitRoot).Path
    }

    $candidate = Join-Path $PSScriptRoot '..\..\..\..'
    return (Resolve-Path -LiteralPath $candidate).Path
}

$ProjectRoot = Resolve-ProjectRoot $ProjectRoot
$WorkflowDir = Join-Path $ProjectRoot '.codex\sol-luna'
$SolState = Join-Path $WorkflowDir '.state\sol-thread.json'
$GoalState = Join-Path $WorkflowDir '.state\goal-state.md'

Write-Output "Project: $ProjectRoot"
Write-Output ''
Write-Output '=== Persistent Sol thread ==='
if (Test-Path -LiteralPath $SolState) {
    Get-Content -LiteralPath $SolState -Raw -Encoding UTF8 | Write-Output
}
else {
    Write-Output '(not created yet)'
}

Write-Output ''
Write-Output '=== Goal execution state ==='
if (Test-Path -LiteralPath $GoalState) {
    Get-Content -LiteralPath $GoalState -Raw -Encoding UTF8 | Write-Output
}
else {
    Write-Output '(no active/persisted Goal Mode state)'
}
