# sysprep.ps1 — generalize the build VM so every Proxmox clone gets a unique
# SID/identity and Cloudbase-Init runs on first boot.
#
# We point sysprep at Cloudbase-Init's Unattend.xml (installed by the MSI),
# which re-arms the cloudbase-init service to run during the specialize pass
# of the next boot, consuming mayfly's NoCloud cidata ISO.
#
# IMPORTANT: we use /quit, NOT /shutdown. sysprep's own /shutdown left the
# guest hung at the ACPI power-off (generalize completed, CPU idle, never
# powered off) which stalled the build. With /quit, sysprep generalizes and
# returns control here; we then schedule a forced `shutdown /s /f` a few
# seconds out so this provisioner can exit 0 cleanly first, and the forced
# power-off reliably reaches the OFF state. The Proxmox builder waits for the
# VM to stop, then converts the (generalized) VM to a template.

$ErrorActionPreference = 'Stop'

$cbDir    = 'C:\Program Files\Cloudbase Solutions\Cloudbase-Init'
$unattend = Join-Path $cbDir 'conf\Unattend.xml'
$sysprep  = 'C:\Windows\System32\Sysprep\sysprep.exe'

if (-not (Test-Path $unattend)) {
    throw "Cloudbase-Init Unattend.xml not found at $unattend"
}

# Clear any prior sysprep state that can block a re-run.
Remove-Item 'C:\Windows\System32\Sysprep\Panther' -Recurse -Force -ErrorAction SilentlyContinue

Write-Host "Running sysprep /generalize /oobe /quit /unattend:$unattend"
& $sysprep /generalize /oobe /quit "/unattend:$unattend"
# sysprep /quit often leaves $LASTEXITCODE empty even on success, so only treat
# a genuine non-zero code as failure (empty/0 = OK). Do NOT power off here — the
# Proxmox builder's own "Stopping VM" step powers the generalized VM off and
# converts it to a template (sysprep's own /shutdown hung, which is why we /quit).
$code = $LASTEXITCODE
if ($code -and $code -ne 0) { throw "sysprep exited with $code" }

Write-Host "Sysprep generalize complete (exit code: '$code'); Packer will power off."
exit 0
