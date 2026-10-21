param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[a-zA-Z0-9][a-zA-Z0-9_.-]*$')]
    [string]$Profile,
    [Parameter(Mandatory = $true)]
    [string]$MinikubeHome,
    [Parameter(Mandatory = $true)]
    [DateTimeOffset]$Since,
    [switch]$VerifyDeleted
)

$ErrorActionPreference = 'Stop'
$vmNames = @($Profile, "$Profile-m02")
$remaining = @()
$inventoryError = $null
$vms = @()
Write-Output "Host cleanup diagnostics since $($Since.UtcDateTime.ToString('o')); verifyDeleted=$VerifyDeleted"

try {
    $vms = @(Get-VM -ErrorAction Stop | Where-Object { $_.Name -in $vmNames })
    $vms | Select-Object Name, Id, State, Status, OperationalStatus, AutomaticCheckpointsEnabled |
        ConvertTo-Json -Depth 4 | Write-Output
    foreach ($vm in $vms) {
        $remaining += "VM: $($vm.Name)"
        try {
            Write-Output "Checkpoints for $($vm.Name)"
            Get-VMSnapshot -VM $vm -ErrorAction Stop |
                Select-Object Name, Id, CreationTime, SnapshotType | ConvertTo-Json -Depth 4 | Write-Output
            Write-Output "Attached disks for $($vm.Name)"
            Get-VMHardDiskDrive -VM $vm -ErrorAction Stop |
                Select-Object Path, ControllerType, ControllerNumber, ControllerLocation |
                ConvertTo-Json -Depth 4 | Write-Output
        } catch {
            Write-Warning "VM detail diagnostics failed for $($vm.Name): $_"
        }
    }
} catch {
    $inventoryError = $_
    Write-Warning "VM inventory failed: $_"
}

foreach ($relativePath in @("profiles\$Profile", "machines\$Profile", "machines\$Profile-m02")) {
    $path = Join-Path $MinikubeHome $relativePath
    $exists = Test-Path -LiteralPath $path -ErrorAction Stop
    Write-Output "Cleanup path exists=${exists}: $path"
    if ($exists) {
        $remaining += "Path: $path"
    }
}

$identifiers = @($vmNames) + @($vms | ForEach-Object { $_.Id.ToString() })
$eventPattern = ($identifiers | ForEach-Object { [regex]::Escape($_) }) -join '|'
foreach ($logName in @('Microsoft-Windows-Hyper-V-VMMS-Admin', 'Microsoft-Windows-Hyper-V-Worker-Admin')) {
    Write-Output "Recent profile events in $logName"
    try {
        Get-WinEvent -FilterHashtable @{ LogName = $logName; StartTime = $Since.UtcDateTime } -MaxEvents 100 -ErrorAction Stop |
            Where-Object { $_.Message -match $eventPattern } |
            Select-Object TimeCreated, Id, LevelDisplayName, Message | ConvertTo-Json -Depth 4 | Write-Output
    } catch {
        if ($_.FullyQualifiedErrorId -like 'NoMatchingEventsFound*') {
            Write-Output 'No recent events'
        } else {
            Write-Warning "Hyper-V event diagnostics failed: $_"
        }
    }
}

if ($inventoryError) {
    throw "Cannot verify VM removal: $inventoryError"
}
if ($VerifyDeleted) {
    if ($remaining.Count -gt 0) {
        throw "Cleanup left resources: $($remaining -join '; ')"
    }
    Write-Output "Verified removal of both VMs and profile/machine directories for $Profile"
}
