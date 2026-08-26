[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$TaskName = "cc-connect-local-watchdog",
    [string]$WatchdogPath = "",
    [Parameter(Mandatory = $true)]
    [string]$ConfigPath,
    [switch]$StartNow,
    [switch]$Unregister
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($WatchdogPath)) {
    $WatchdogPath = Join-Path $PSScriptRoot "Watch-CcConnect.ps1"
}

if ($Unregister) {
    if ($PSCmdlet.ShouldProcess($TaskName, "Unregister scheduled task")) {
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop
        Write-Host "Scheduled task removed: $TaskName"
    }
    exit 0
}

$WatchdogPath = (Resolve-Path -LiteralPath $WatchdogPath).Path
$ConfigPath = (Resolve-Path -LiteralPath $ConfigPath).Path
$PowerShellExecutable = Join-Path $PSHOME "powershell.exe"
$UserId = "$env:USERDOMAIN\$env:USERNAME"
$ProjectRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$ActionArguments = "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$WatchdogPath`" -ConfigPath `"$ConfigPath`""

$Action = New-ScheduledTaskAction `
    -Execute $PowerShellExecutable `
    -Argument $ActionArguments `
    -WorkingDirectory $ProjectRoot
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User $UserId
$Principal = New-ScheduledTaskPrincipal -UserId $UserId -LogonType Interactive -RunLevel Limited
$Settings = New-ScheduledTaskSettingsSet `
    -MultipleInstances IgnoreNew `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -StartWhenAvailable `
    -ExecutionTimeLimit ([TimeSpan]::Zero)

if ($PSCmdlet.ShouldProcess($TaskName, "Register current-user logon watchdog")) {
    Register-ScheduledTask `
        -TaskName $TaskName `
        -Action $Action `
        -Trigger $Trigger `
        -Principal $Principal `
        -Settings $Settings `
        -Description "Keep the local cc-connect source runtime available after user logon." `
        -Force | Out-Null

    Write-Host "Scheduled task registered: $TaskName"
    if ($StartNow) {
        Start-ScheduledTask -TaskName $TaskName
        Write-Host "Scheduled task started: $TaskName"
    }
}
