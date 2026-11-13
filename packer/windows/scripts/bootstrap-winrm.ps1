# bootstrap-winrm.ps1 — run from Autounattend FirstLogon on the build VM.
# Brings up WinRM (HTTP/5985, Basic + AllowUnencrypted) so Packer's winrm
# communicator can connect, and installs + starts the QEMU guest agent so the
# Proxmox builder (qemu_agent = true) can read the VM's IP address.

$ErrorActionPreference = 'Continue'
$log = 'C:\Windows\Temp\bootstrap-winrm.log'
function Log($m) { "$(Get-Date -Format o) $m" | Tee-Object -FilePath $log -Append }

Log 'Starting WinRM bootstrap...'

# --- WinRM ---------------------------------------------------------------
try {
    # Make sure the network profiles are Private so quickconfig doesn't refuse.
    Get-NetConnectionProfile -ErrorAction SilentlyContinue |
        ForEach-Object { Set-NetConnectionProfile -InterfaceIndex $_.InterfaceIndex -NetworkCategory Private -ErrorAction SilentlyContinue }

    Set-Service -Name WinRM -StartupType Automatic -ErrorAction SilentlyContinue
    Start-Service -Name WinRM -ErrorAction SilentlyContinue

    # -force + -q so it never prompts; SkipNetworkProfileCheck for Public nets.
    & winrm quickconfig -q -force 2>&1 | Tee-Object -FilePath $log -Append
    & winrm quickconfig -q 2>&1     | Tee-Object -FilePath $log -Append

    & winrm set winrm/config/service '@{AllowUnencrypted="true"}' 2>&1 | Tee-Object -FilePath $log -Append
    & winrm set winrm/config/service/auth '@{Basic="true"}'       2>&1 | Tee-Object -FilePath $log -Append
    & winrm set winrm/config/client/auth '@{Basic="true"}'        2>&1 | Tee-Object -FilePath $log -Append
    & winrm set winrm/config '@{MaxTimeoutms="1800000"}'          2>&1 | Tee-Object -FilePath $log -Append

    Set-Item WSMan:\localhost\Service\AllowUnencrypted $true -ErrorAction SilentlyContinue
    Set-Item WSMan:\localhost\Service\Auth\Basic $true -ErrorAction SilentlyContinue

    # Firewall: allow 5985 explicitly (belt and suspenders over the WinRM rule).
    Enable-NetFirewallRule -DisplayGroup 'Windows Remote Management' -ErrorAction SilentlyContinue
    New-NetFirewallRule -DisplayName 'WinRM 5985 (mayfly)' -Direction Inbound -LocalPort 5985 `
        -Protocol TCP -Action Allow -ErrorAction SilentlyContinue | Out-Null
    netsh advfirewall firewall add rule name="WinRM-HTTP-5985" dir=in action=allow protocol=TCP localport=5985 2>&1 | Out-Null

    Restart-Service WinRM -ErrorAction SilentlyContinue
    Log 'WinRM configured.'
} catch {
    Log "WinRM setup error: $_"
}

# --- QEMU guest agent ----------------------------------------------------
try {
    $msi = $null
    foreach ($d in 'D','E','F','G','H') {
        $cand = "$($d):\guest-agent\qemu-ga-x86_64.msi"
        if (Test-Path $cand) { $msi = $cand; break }
    }
    if ($msi) {
        Log "Installing QEMU guest agent from $msi"
        Start-Process msiexec.exe -ArgumentList "/i `"$msi`" /qn /norestart" -Wait
        foreach ($svc in 'QEMU-GA','qemu-ga','QEMU Guest Agent') {
            $s = Get-Service -Name $svc -ErrorAction SilentlyContinue
            if ($s) {
                Set-Service -Name $s.Name -StartupType Automatic -ErrorAction SilentlyContinue
                Start-Service -Name $s.Name -ErrorAction SilentlyContinue
                Log "Started guest agent service: $($s.Name)"
            }
        }
    } else {
        Log 'QEMU guest agent MSI not found on any CD drive.'
    }
} catch {
    Log "QEMU guest agent install error: $_"
}

Log 'Bootstrap complete.'
