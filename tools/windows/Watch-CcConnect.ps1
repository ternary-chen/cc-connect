[CmdletBinding()]
param(
    [string]$ProjectRoot = "",
    [Parameter(Mandatory = $true)]
    [string]$ConfigPath,
    [ValidateRange(10, 3600)]
    [int]$IntervalSeconds = 30,
    [ValidateRange(1, 65535)]
    [int]$ManagementPort = 9820,
    [switch]$Once
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($ProjectRoot)) {
    $ProjectRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
}

$ProjectRoot = (Resolve-Path -LiteralPath $ProjectRoot).Path
$ConfigPath = (Resolve-Path -LiteralPath $ConfigPath).Path
$CommandRoot = Join-Path $ProjectRoot "cmd\cc-connect"
if (-not (Test-Path -LiteralPath $CommandRoot -PathType Container)) {
    throw "cc-connect command directory not found: $CommandRoot"
}

$GoExecutable = (Get-Command go -ErrorAction Stop).Source
$RuntimeRoot = Join-Path $ProjectRoot ".runtime\watchdog"
New-Item -ItemType Directory -Path $RuntimeRoot -Force | Out-Null
$WatchdogLog = Join-Path $RuntimeRoot "watchdog.log"
$MutexName = "Local\CcConnectLocalSourceWatchdog"
$Mutex = New-Object System.Threading.Mutex($false, $MutexName)
$HasMutex = $false
$script:ProcessInspectionFailed = $false

function Write-WatchdogLog {
    param(
        [ValidateSet("INFO", "WARN", "ERROR")]
        [string]$Level,
        [string]$Message
    )

    if ((Test-Path -LiteralPath $WatchdogLog) -and
        (Get-Item -LiteralPath $WatchdogLog).Length -ge 5MB) {
        $BackupLog = "$WatchdogLog.1"
        Move-Item -LiteralPath $WatchdogLog -Destination $BackupLog -Force
    }

    $Timestamp = Get-Date -Format "yyyy-MM-ddTHH:mm:ss.fffK"
    Add-Content -LiteralPath $WatchdogLog -Encoding UTF8 -Value "$Timestamp [$Level] $Message"
}

function Get-MatchingCcConnectProcess {
    $script:ProcessInspectionFailed = $false
    try {
        return @(
            Get-CimInstance Win32_Process -Filter "Name = 'cc-connect.exe'" -ErrorAction Stop |
                Where-Object {
                    $CommandLine = [string]$_.CommandLine
                    $CommandLine.IndexOf("--config", [System.StringComparison]::OrdinalIgnoreCase) -ge 0 -and
                    $CommandLine.IndexOf($ConfigPath, [System.StringComparison]::OrdinalIgnoreCase) -ge 0
                }
        )
    }
    catch {
        $script:ProcessInspectionFailed = $true
        Write-WatchdogLog -Level "ERROR" -Message "Cannot inspect cc-connect processes: $($_.Exception.GetType().Name)"
        return @()
    }
}

function Test-ManagementListener {
    $Client = New-Object System.Net.Sockets.TcpClient
    try {
        $AsyncResult = $Client.BeginConnect("127.0.0.1", $ManagementPort, $null, $null)
        $ConnectedInTime = $AsyncResult.AsyncWaitHandle.WaitOne(1500)
        if (-not $ConnectedInTime) {
            return $false
        }
        $Client.EndConnect($AsyncResult)
        return $Client.Connected
    }
    catch {
        return $false
    }
    finally {
        $Client.Dispose()
    }
}

function Start-LocalCcConnect {
    $Stamp = Get-Date -Format "yyyyMMdd-HHmmss"
    $StdoutLog = Join-Path $RuntimeRoot "cc-connect-$Stamp.stdout.log"
    $StderrLog = Join-Path $RuntimeRoot "cc-connect-$Stamp.stderr.log"

    try {
        $Runner = Start-Process `
            -FilePath $GoExecutable `
            -ArgumentList @("run", "-tags", "no_pi", "./cmd/cc-connect", "--config", $ConfigPath) `
            -WorkingDirectory $ProjectRoot `
            -RedirectStandardOutput $StdoutLog `
            -RedirectStandardError $StderrLog `
            -WindowStyle Hidden `
            -PassThru
    }
    catch {
        Write-WatchdogLog -Level "ERROR" -Message "Failed to launch go run: $($_.Exception.GetType().Name); stderr=$StderrLog"
        return $false
    }

    Write-WatchdogLog -Level "INFO" -Message "Launch requested; go_pid=$($Runner.Id); stdout=$StdoutLog; stderr=$StderrLog"
    $Deadline = (Get-Date).AddSeconds(45)
    do {
        Start-Sleep -Milliseconds 500
        $Matches = @(Get-MatchingCcConnectProcess)
        if ($Matches.Count -gt 0) {
            $CcConnectPid = $Matches[0].ProcessId
            $ListenerDeadline = (Get-Date).AddSeconds(20)
            do {
                if (Test-ManagementListener) {
                    Write-WatchdogLog -Level "INFO" -Message "cc-connect healthy; pid=$CcConnectPid; port=$ManagementPort"
                    return $true
                }
                Start-Sleep -Seconds 1
            } while ((Get-Date) -lt $ListenerDeadline)

            Write-WatchdogLog -Level "WARN" -Message "cc-connect started but management port is not listening; pid=$CcConnectPid; port=$ManagementPort"
            return $false
        }

        $Runner.Refresh()
        if ($Runner.HasExited) {
            Write-WatchdogLog -Level "ERROR" -Message "go run exited before cc-connect became ready; exit_code=$($Runner.ExitCode); stderr=$StderrLog"
            return $false
        }
    } while ((Get-Date) -lt $Deadline)

    Write-WatchdogLog -Level "ERROR" -Message "Timed out waiting for cc-connect; go_pid=$($Runner.Id); stderr=$StderrLog"
    return $false
}

try {
    try {
        $HasMutex = $Mutex.WaitOne(0, $false)
    }
    catch [System.Threading.AbandonedMutexException] {
        $HasMutex = $true
    }

    if (-not $HasMutex) {
        Write-WatchdogLog -Level "WARN" -Message "Another watchdog instance is already running; exiting"
        exit 0
    }

    $ConfigText = Get-Content -LiteralPath $ConfigPath -Encoding UTF8 -Raw
    if ($ConfigText -notmatch '(?m)^\s*busy_message_mode\s*=\s*"steer"\s*$') {
        Write-WatchdogLog -Level "WARN" -Message "Configured busy_message_mode is not steer; the watchdog will not modify it"
    }

    $BootTime = (Get-CimInstance Win32_OperatingSystem -ErrorAction SilentlyContinue).LastBootUpTime
    Write-WatchdogLog -Level "INFO" -Message "Watchdog started; project=$ProjectRoot; config=$ConfigPath; boot_time=$BootTime"

    $LastState = ""
    $LaunchFailureLatched = $false
    while ($true) {
        $Matches = @(Get-MatchingCcConnectProcess)
        $ListenerReady = Test-ManagementListener

        if ($script:ProcessInspectionFailed) {
            $State = "inspection_failed"
            if ($State -ne $LastState) {
                Write-WatchdogLog -Level "ERROR" -Message "Process inspection failed; automatic launch is suppressed to avoid a duplicate instance"
            }
        }
        elseif ($Matches.Count -gt 0 -and $ListenerReady) {
            $State = "healthy"
            $LaunchFailureLatched = $false
            if ($State -ne $LastState) {
                $Pids = ($Matches | ForEach-Object { $_.ProcessId }) -join ","
                Write-WatchdogLog -Level "INFO" -Message "Healthy; pid=$Pids; port=$ManagementPort"
            }
        }
        elseif ($Matches.Count -gt 0) {
            $State = "process_without_listener"
            if ($State -ne $LastState) {
                $Pids = ($Matches | ForEach-Object { $_.ProcessId }) -join ","
                Write-WatchdogLog -Level "WARN" -Message "Process exists but management port is unavailable; pid=$Pids; port=$ManagementPort; no automatic termination will occur"
            }
        }
        elseif ($ListenerReady) {
            $State = "foreign_listener"
            if ($State -ne $LastState) {
                Write-WatchdogLog -Level "WARN" -Message "Port $ManagementPort is occupied without a matching cc-connect process; launch skipped"
            }
        }
        else {
            $State = "stopped"
            if ($State -ne $LastState) {
                Write-WatchdogLog -Level "WARN" -Message "Matching cc-connect process is absent"
            }

            if (-not $LaunchFailureLatched) {
                $Started = Start-LocalCcConnect
                if (-not $Started) {
                    $LaunchFailureLatched = $true
                    Write-WatchdogLog -Level "ERROR" -Message "Automatic launch failed; retries are disabled until the watchdog restarts or a healthy instance is started manually"
                }
            }
        }

        $LastState = $State
        if ($Once) {
            break
        }
        Start-Sleep -Seconds $IntervalSeconds
    }
}
finally {
    if ($HasMutex) {
        $Mutex.ReleaseMutex()
    }
    $Mutex.Dispose()
}
