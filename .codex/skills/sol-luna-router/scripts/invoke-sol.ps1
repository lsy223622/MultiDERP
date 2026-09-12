[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Medium', 'High', 'Review', 'GoalPlan', 'GoalCheckpoint', 'GoalReplan', 'GoalReview')]
    [string]$Mode,

    [Parameter(Mandatory = $true)]
    [string]$InputFile,

    [string]$ProjectRoot
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Resolve-ProjectRoot {
    param([string]$ExplicitRoot)

    if ($ExplicitRoot) {
        return (Resolve-Path -LiteralPath $ExplicitRoot).Path
    }

    # scripts/ -> sol-luna-router/ -> skills/ -> .codex/ -> project root
    $candidate = Join-Path $PSScriptRoot '..\..\..\..'
    return (Resolve-Path -LiteralPath $candidate).Path
}

function Read-JsonFile {
    param([string]$Path)
    return (Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json)
}

function Write-State {
    param(
        [string]$Path,
        [string]$ThreadId,
        [string]$Model,
        [string]$Effort,
        [string]$Transport,
        [string]$CreatedAt
    )

    $state = [ordered]@{
        thread_id    = $ThreadId
        model        = $Model
        effort       = $Effort
        transport    = $Transport
        created_at   = $CreatedAt
        last_used_at = (Get-Date).ToString('o')
    }

    $state | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $Path -Encoding UTF8
}

function Test-LockToken {
    param(
        [string]$Path,
        [string]$Token
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        return $false
    }

    try {
        $content = Get-Content -LiteralPath $Path -Raw -Encoding UTF8
        return $content -match ("(?m)^token=" + [regex]::Escape($Token) + "\s*$")
    }
    catch {
        return $false
    }
}

$ProjectRoot = Resolve-ProjectRoot $ProjectRoot
$WorkflowDir = Join-Path $ProjectRoot '.codex\sol-luna'
$StateDir = Join-Path $WorkflowDir '.state'
$LogsDir = Join-Path $WorkflowDir 'logs'
$SettingsPath = Join-Path $WorkflowDir 'settings.json'
$ContractPath = Join-Path $WorkflowDir 'SOL_CONTRACT.md'
$ContextPath = Join-Path $WorkflowDir 'PROJECT_CONTEXT.md'
$DecisionsPath = Join-Path $WorkflowDir 'DECISIONS.md'
$UserControlPath = Join-Path $WorkflowDir 'USER_CONTROL.md'
$StatePath = Join-Path $StateDir 'sol-thread.json'
$LockPath = Join-Path $StateDir 'sol-thread.lock'
$LastResponsePath = Join-Path $WorkflowDir 'last-sol-response.md'
$lockToken = [guid]::NewGuid().ToString('N')
$lockAcquired = $false
$lockTokenWritten = $false

foreach ($required in @($SettingsPath, $ContractPath, $ContextPath, $DecisionsPath, $UserControlPath, $InputFile)) {
    if (-not (Test-Path -LiteralPath $required)) {
        throw "Required workflow file not found: $required"
    }
}

New-Item -ItemType Directory -Force -Path $StateDir, $LogsDir | Out-Null

$codex = Get-Command codex -ErrorAction SilentlyContinue
if (-not $codex) {
    throw "Codex CLI ('codex') is not available on PATH. Use Codex App independent thread tools, or install/expose the Codex CLI before using this fallback."
}

$settings = Read-JsonFile $SettingsPath
$model = [string]$settings.sol_model
$effort = [string]$settings.sol_effort
$sandbox = if ($settings.PSObject.Properties.Name -contains 'sol_sandbox') { [string]$settings.sol_sandbox } else { 'workspace-write' }

if ([string]::IsNullOrWhiteSpace($model) -or [string]::IsNullOrWhiteSpace($effort)) {
    throw 'settings.json must define sol_model and sol_effort.'
}

$handoff = Get-Content -LiteralPath $InputFile -Raw -Encoding UTF8
if ([string]::IsNullOrWhiteSpace($handoff)) {
    throw 'The Sol handoff is empty.'
}

# Prevent concurrent writers from corrupting one persistent Codex session.
$lockStream = $null
try {
    try {
        $lockStream = [System.IO.File]::Open($LockPath, [System.IO.FileMode]::CreateNew, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
        $lockAcquired = $true
        $lockBytes = [System.Text.Encoding]::UTF8.GetBytes("token=$lockToken`npid=$PID`ntime=$((Get-Date).ToString('o'))`n")
        $lockStream.Write($lockBytes, 0, $lockBytes.Length)
        $lockStream.Flush()
        $lockTokenWritten = $true
    }
    catch [System.IO.IOException] {
        $ioMessage = $_.Exception.Message
        if (Test-Path -LiteralPath $LockPath) {
            $lockOwner = Get-Content -LiteralPath $LockPath -Raw -Encoding UTF8 -ErrorAction SilentlyContinue
            throw "Another Sol consultation appears to be active (lock: $LockPath).`n$lockOwner"
        }

        throw "Could not acquire the Sol consultation lock; the lock operation failed but no lock file is present. No lock was removed. Original error: $ioMessage"
    }
    catch {
        throw
    }

    $existingState = $null
    if (Test-Path -LiteralPath $StatePath) {
        $existingState = Read-JsonFile $StatePath
    }

    if ($existingState -and
        $existingState.PSObject.Properties.Name -contains 'transport' -and
        [string]$existingState.transport -ne 'cli') {
        throw "Persistent Sol state belongs to the '$($existingState.transport)' transport. Use that transport's thread tools instead of the CLI fallback."
    }

    $threadId = if ($existingState) { [string]$existingState.thread_id } else { '' }
    $createdAt = if ($existingState -and $existingState.created_at) { [string]$existingState.created_at } else { (Get-Date).ToString('o') }

    $modeInstruction = switch ($Mode) {
        'Medium' { 'This is a MEDIUM consultation. Use the supplied evidence package; own the consequential design decisions and planning.' }
        'High'   { 'This is a HIGH consultation. Own discovery and planning. Inspect the repository yourself and do not assume the Luna framing identifies the relevant subsystem or root cause.' }
        'Review' { 'This is a HIGH implementation review. Inspect the actual current diff/files yourself and return PASS, FIX, or BLOCKED according to the persistent Sol contract.' }
        'GoalPlan' { 'This is a multi-step GOAL planning request. Own decomposition at the appropriate discovery depth. Produce a versioned work-item plan, assign each item LOW/MEDIUM/HIGH expected complexity, dependencies, acceptance criteria, assumptions, and re-consult triggers per the persistent Sol contract.' }
        'GoalCheckpoint' { 'This is a GOAL execution checkpoint. Reconcile new execution evidence with the existing goal plan, revise affected plan details/complexity when needed, and identify the next safe executable work item(s).' }
        'GoalReplan' { 'This is a GOAL structural replan. A user override or other structural premise changed the plan. Treat the newest user directive as authoritative, preserve still-valid work, increment the plan version as appropriate, and reclassify affected work items without needless churn.' }
        'GoalReview' { 'This is a GOAL-level final review. Inspect the actual current repository state/diff and verify the original goal and latest plan acceptance criteria. Return PASS, FIX, or BLOCKED.' }
    }

    if ([string]::IsNullOrWhiteSpace($threadId)) {
        $contract = Get-Content -LiteralPath $ContractPath -Raw -Encoding UTF8
        $prompt = @"
You are being initialized as this repository's persistent Sol High senior-engineer thread.

Read and follow this project contract for this and future turns:

--- SOL CONTRACT ---
$contract
--- END SOL CONTRACT ---

The workflow/control and durable shared context files are:
- .codex/sol-luna/USER_CONTROL.md
- .codex/sol-luna/PROJECT_CONTEXT.md
- .codex/sol-luna/DECISIONS.md

$modeInstruction

--- HANDOFF ---
$handoff
--- END HANDOFF ---
"@
    }
    else {
        $prompt = @"
Continue as this repository's persistent Sol High senior engineer. Keep your prior project mental model, but verify material assumptions against the current repository. Re-read .codex/sol-luna/SOL_CONTRACT.md, USER_CONTROL.md, and the durable context files when needed.

$modeInstruction

--- HANDOFF ---
$handoff
--- END HANDOFF ---
"@
    }

    $timestamp = Get-Date -Format 'yyyyMMdd-HHmmss-fff'
    $rawLogPath = Join-Path $LogsDir "$timestamp-$($Mode.ToLowerInvariant()).jsonl"
    $stderrPath = Join-Path $LogsDir "$timestamp-$($Mode.ToLowerInvariant()).stderr.txt"

    $args = @(
        'exec',
        '--model', $model,
        '--json',
        '--sandbox', $sandbox,
        '-C', $ProjectRoot,
        '-c', "model_reasoning_effort=$effort"
    )

    if ([string]::IsNullOrWhiteSpace($threadId)) {
        $args += '-'
    }
    else {
        $args += @('resume', $threadId, '-')
    }

    $outputLines = @($prompt | & $codex.Source @args 2> $stderrPath)
    $exitCode = $LASTEXITCODE
    $outputLines | Set-Content -LiteralPath $rawLogPath -Encoding UTF8

    if ($exitCode -ne 0) {
        $stderr = if (Test-Path -LiteralPath $stderrPath) { Get-Content -LiteralPath $stderrPath -Raw -Encoding UTF8 } else { '' }
        throw "Codex Sol consultation failed with exit code $exitCode.`n$stderr"
    }

    $actualThreadId = $null
    $lastAgentMessage = $null

    foreach ($line in $outputLines) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try {
            $event = $line | ConvertFrom-Json -ErrorAction Stop
        }
        catch {
            continue
        }

        if ($event.type -eq 'thread.started' -and $event.thread_id) {
            $actualThreadId = [string]$event.thread_id
        }

        if ($event.type -eq 'item.completed' -and $event.item -and $event.item.type -eq 'agent_message' -and $event.item.text) {
            $lastAgentMessage = [string]$event.item.text
        }
    }

    if ([string]::IsNullOrWhiteSpace($actualThreadId)) {
        throw "Codex completed without exposing a thread.started thread_id. Raw log: $rawLogPath"
    }

    # Fail closed if 'resume <id>' unexpectedly created a different session.
    if (-not [string]::IsNullOrWhiteSpace($threadId) -and $actualThreadId -ne $threadId) {
        throw "Persistent Sol resume returned a different thread id. Expected '$threadId', got '$actualThreadId'. Refusing to silently lose Sol context. Inspect $rawLogPath and reset the Sol state explicitly if needed."
    }

    if ([string]::IsNullOrWhiteSpace($lastAgentMessage)) {
        throw "No final agent_message was found in Codex JSON output. Raw log: $rawLogPath"
    }

    Write-State -Path $StatePath -ThreadId $actualThreadId -Model $model -Effort $effort -Transport 'cli' -CreatedAt $createdAt
    $lastAgentMessage | Set-Content -LiteralPath $LastResponsePath -Encoding UTF8

    Write-Output $lastAgentMessage
}
finally {
    if ($lockStream) {
        $lockStream.Dispose()
        $lockStream = $null
    }
    if ($lockAcquired -and $lockTokenWritten -and (Test-LockToken -Path $LockPath -Token $lockToken)) {
        Remove-Item -LiteralPath $LockPath -Force -ErrorAction SilentlyContinue
    }
}
