param([switch]$IncludeLogs)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

[ordered]@{
    utc = [DateTime]::UtcNow.ToString('o')
    processors = @(Get-CimInstance Win32_Processor |
        Select-Object Name, NumberOfLogicalProcessors, MaxClockSpeed)
    memory = Get-CimInstance Win32_OperatingSystem |
        Select-Object TotalVisibleMemorySize, FreePhysicalMemory
    processes = @(Get-Process |
        Where-Object ProcessName -in @('containerd', 'containerd-shim-runhcs-v1', 'MsMpEng', 'kubelet', 'System') |
        Select-Object Id, ProcessName, CPU, StartTime, WorkingSet64)
} | ConvertTo-Json -Depth 5 -Compress

if (-not $IncludeLogs) { return }

$containerd = Get-CimInstance Win32_Service -Filter "Name='containerd'"
$containerd | Select-Object Name, State, PathName | ConvertTo-Json -Compress
$match = [regex]::Match($containerd.PathName, '--log-file(?:=|\s+)(?:"([^"]+)"|(\S+))')
if (-not $match.Success) {
    throw 'The containerd service does not specify a --log-file destination'
}
$containerdLog = if ($match.Groups[1].Success) { $match.Groups[1].Value } else { $match.Groups[2].Value }
$kubelet = Get-ItemProperty -LiteralPath 'HKLM:\SYSTEM\CurrentControlSet\Services\kubelet\Parameters'

foreach ($path in @($containerdLog, $kubelet.AppStdout, $kubelet.AppStderr) | Select-Object -Unique) {
    if ([string]::IsNullOrWhiteSpace($path) -or -not (Test-Path -LiteralPath $path)) {
        Write-Warning "Windows runtime log is unavailable: '$path'"
        continue
    }
    Write-Output "=== Windows runtime log: $path ==="
    Get-Content -LiteralPath $path -Tail 3000
}
