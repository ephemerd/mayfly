# install-cloudbase-init.ps1 — download and silently install Cloudbase-Init.
# Runs as a Packer powershell provisioner over WinRM on the build VM.
# We install the service to run as LocalSystem and deliberately do NOT let the
# MSI kick off its own sysprep (no RUN_SYSPREP/unattend options) — mayfly's
# packer sysprep.ps1 handles generalize/shutdown with our own conf.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$msi = "$env:TEMP\CloudbaseInitSetup_x64.msi"
$urls = @(
    'https://www.cloudbase.it/downloads/CloudbaseInitSetup_x64.msi',
    'https://github.com/cloudbase/cloudbase-init/releases/latest/download/CloudbaseInitSetup_x64.msi'
)

$downloaded = $false
foreach ($u in $urls) {
    try {
        Write-Host "Downloading Cloudbase-Init from $u"
        Invoke-WebRequest -UseBasicParsing -Uri $u -OutFile $msi
        if ((Get-Item $msi).Length -gt 1MB) { $downloaded = $true; break }
    } catch {
        Write-Host "  download failed: $_"
    }
}
if (-not $downloaded) { throw 'Failed to download Cloudbase-Init MSI from all sources.' }

Write-Host "Installing Cloudbase-Init ($([math]::Round((Get-Item $msi).Length/1MB,1)) MB)..."
$p = Start-Process msiexec.exe -ArgumentList "/i `"$msi`" /qn /norestart RUN_SERVICE_AS_LOCAL_SYSTEM=1" -Wait -PassThru
if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) {
    throw "msiexec exited with $($p.ExitCode)"
}

$confDir = 'C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf'
if (-not (Test-Path $confDir)) { throw "Cloudbase-Init conf dir not found at $confDir after install." }
Write-Host 'Cloudbase-Init installed.'
