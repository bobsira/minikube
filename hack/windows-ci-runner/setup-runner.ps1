<#
.SYNOPSIS
    Installs and registers a GitHub Actions self-hosted runner on the Windows CI VM.

.DESCRIPTION
    Downloads the latest GitHub Actions runner, registers it with the kubernetes/minikube
    repository using a registration token obtained from the GitHub API, and installs it
    as a Windows service so it survives reboots.

    Prerequisites:
      - Windows with Hyper-V enabled
      - OpenSSH server running
      - Chocolatey, Go, and kubectl are installed automatically if missing

    Run this script once after the VM is provisioned via runner.bicep.
    Re-run to update or re-register the runner.

.PARAMETER GitHubPAT
    A GitHub Personal Access Token with 'repo' scope (or fine-grained token with
    Actions read/write on kubernetes/minikube). Used to obtain a short-lived
    runner registration token from the GitHub API.

.PARAMETER RunnerName
    Display name for the runner in GitHub Actions. Defaults to the machine hostname.

.PARAMETER RunnerDir
    Directory to install the runner into. Defaults to C:\actions-runner.

.PARAMETER RepoUrl
    GitHub repository URL to register the runner against.

.EXAMPLE
    .\setup-runner.ps1 -GitHubPAT "ghp_xxxxxxxxxxxx"
    .\setup-runner.ps1 -GitHubPAT "ghp_xxxxxxxxxxxx" -RunnerName "windows-hyperv-01"
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$GitHubPAT,

    [string]$RunnerName = $env:COMPUTERNAME,

    [string]$RunnerDir = 'C:\actions-runner',

    [string]$RepoUrl = 'https://github.com/bobsira/minikube'
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# 0. Install prerequisites (Chocolatey, Go, kubectl) and update machine PATH
# ---------------------------------------------------------------------------
if (-not (Get-Command choco -ErrorAction SilentlyContinue)) {
    Write-Host "Installing Chocolatey..."
    Set-ExecutionPolicy Bypass -Scope Process -Force
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
    Invoke-Expression ((New-Object Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))
}

Write-Host "Installing Go, kubectl, Git, and make..."
& C:\ProgramData\chocolatey\bin\choco install golang kubernetes-cli git make -y

Write-Host "Updating machine PATH..."
$machinePath = [System.Environment]::GetEnvironmentVariable('PATH', 'Machine')
$additions = @('C:\ProgramData\chocolatey\bin', 'C:\Program Files\Go\bin')
foreach ($entry in $additions) {
    if ($machinePath -notlike "*$entry*") {
        $machinePath = "$machinePath;$entry"
    }
}
[System.Environment]::SetEnvironmentVariable('PATH', $machinePath, 'Machine')
$env:PATH = "$env:PATH;C:\ProgramData\chocolatey\bin;C:\Program Files\Go\bin"

# ---------------------------------------------------------------------------
# 1. Resolve the latest runner version from the GitHub releases API
# ---------------------------------------------------------------------------
Write-Host "Fetching latest GitHub Actions runner version..."
$releaseInfo = Invoke-RestMethod -Uri 'https://api.github.com/repos/actions/runner/releases/latest' -UseBasicParsing
$runnerVersion = $releaseInfo.tag_name.TrimStart('v')
Write-Host "Latest runner version: $runnerVersion"

# ---------------------------------------------------------------------------
# 2. Download and extract runner package
# ---------------------------------------------------------------------------
$downloadUrl = "https://github.com/actions/runner/releases/download/v${runnerVersion}/actions-runner-win-x64-${runnerVersion}.zip"
$zipPath = "$env:TEMP\actions-runner.zip"

Write-Host "Downloading runner from $downloadUrl ..."
(New-Object Net.WebClient).DownloadFile($downloadUrl, $zipPath)

New-Item -ItemType Directory -Force -Path $RunnerDir | Out-Null

# Stop and uninstall any existing runner service before wiping the directory
$svcScript = Join-Path $RunnerDir 'svc.cmd'
if (Test-Path $svcScript) {
    Write-Host "Stopping existing runner service..."
    Push-Location $RunnerDir
    cmd /c svc.cmd stop 2>$null
    cmd /c svc.cmd uninstall 2>$null
    Pop-Location
}

# Remove existing directory so extraction starts clean
if (Test-Path $RunnerDir) {
    Write-Host "Removing existing runner directory..."
    Remove-Item -Recurse -Force $RunnerDir
}

Write-Host "Extracting runner to $RunnerDir ..."
Add-Type -AssemblyName System.IO.Compression.FileSystem
[System.IO.Compression.ZipFile]::ExtractToDirectory($zipPath, $RunnerDir)
Remove-Item $zipPath

# ---------------------------------------------------------------------------
# 3. Obtain a short-lived runner registration token via GitHub API
# ---------------------------------------------------------------------------
Write-Host "Requesting runner registration token..."
$apiHeaders = @{
    Authorization = "token $GitHubPAT"
    Accept        = 'application/vnd.github.v3+json'
}
# Extract org/repo from URL for the API path
$repoPath = $RepoUrl -replace 'https://github.com/', ''
$tokenResponse = Invoke-RestMethod `
    -Uri "https://api.github.com/repos/$repoPath/actions/runners/registration-token" `
    -Method POST `
    -Headers $apiHeaders
$registrationToken = $tokenResponse.token

# ---------------------------------------------------------------------------
# 4. Configure the runner
# ---------------------------------------------------------------------------
Push-Location $RunnerDir

Write-Host "Configuring runner '$RunnerName' and installing as Windows service..."
& .\config.cmd `
    --url $RepoUrl `
    --token $registrationToken `
    --name $RunnerName `
    --labels 'self-hosted,windows,hyper-v,windows-2022' `
    --runnergroup 'Default' `
    --work '_work' `
    --unattended `
    --replace `
    --runasservice

if ($LASTEXITCODE -ne 0) {
    throw "Runner configuration failed (exit code $LASTEXITCODE)."
}

Pop-Location

# ---------------------------------------------------------------------------
# 5. Configure the runner service to run as LocalSystem for Hyper-V access
# ---------------------------------------------------------------------------
# The default service account (NT AUTHORITY\NETWORK SERVICE) cannot access
# Hyper-V. LocalSystem has the required privileges.
$serviceName = "actions.runner.$($repoPath -replace '/', '-').$RunnerName"
Write-Host "Configuring runner service '$serviceName' to run as LocalSystem..."
sc.exe config $serviceName obj= "LocalSystem"
if ($LASTEXITCODE -ne 0) {
    throw "Failed to reconfigure service account (exit code $LASTEXITCODE)."
}
Restart-Service $serviceName
Write-Host "Runner service restarted as LocalSystem."

Write-Host ""
Write-Host "Runner '$RunnerName' is registered and running."
Write-Host "Labels: self-hosted, windows, hyper-v, windows-2022"
Write-Host "Repository: $RepoUrl"
Write-Host ""
Write-Host "Use in workflows with:"
Write-Host "  runs-on: [self-hosted, windows, hyper-v]"
