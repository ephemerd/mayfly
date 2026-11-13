// Builds the Windows Server 2025 template mayfly clones for windows pools:
// mayfly-winserver-2025. Installs virtio drivers, QEMU guest agent,
// Cloudbase-Init (configured for the NoCloud datasource mayfly's cloud-init
// ISO uses), then sysprep /generalize and convert to a Proxmox template.
//
//   cd packer/windows && packer init . && packer build .
//
// EVERYTHING environment-sourced, no defaults: PROXMOX_URL, PROXMOX_TOKEN_ID,
// PROXMOX_SECRET_MAYFLY, PROXMOX_NODE, WINADMIN_PASSWORD. Each is validated, so a
// missing one fails fast instead of building against the wrong cluster.

packer {
  required_plugins {
    proxmox = {
      version = ">= 1.1.3"
      source  = "github.com/hashicorp/proxmox"
    }
  }
}

variable "proxmox_url" {
  type    = string
  default = env("PROXMOX_URL")
  validation {
    condition     = length(var.proxmox_url) > 0
    error_message = "Set PROXMOX_URL (e.g. https://pve.example.com:8006/api2/json) or pass -var proxmox_url=..."
  }
}
variable "proxmox_token_id" {
  type    = string
  default = env("PROXMOX_TOKEN_ID")
  validation {
    condition     = length(var.proxmox_token_id) > 0
    error_message = "Set PROXMOX_TOKEN_ID (e.g. mayfly@pve!mayfly) or pass -var proxmox_token_id=..."
  }
}
variable "proxmox_token_secret" {
  type      = string
  sensitive = true
  default   = env("PROXMOX_SECRET_MAYFLY")
  validation {
    condition     = length(var.proxmox_token_secret) > 0
    error_message = "Set PROXMOX_SECRET_MAYFLY or pass -var proxmox_token_secret=..."
  }
}
variable "proxmox_node" {
  type    = string
  default = env("PROXMOX_NODE") // the node holding windows-server-2025.iso + virtio-win.iso
  validation {
    condition     = length(var.proxmox_node) > 0
    error_message = "Set PROXMOX_NODE to the PVE node holding windows-server-2025.iso and virtio-win.iso."
  }
}
variable "disk_storage" {
  type    = string
  default = "local-lvm"
}
variable "iso_storage" {
  type    = string
  default = "local"
}
// The local Administrator password baked into the template, and therefore the
// password on EVERY VM cloned from it — nothing in the clone path rotates it.
// It had a hardcoded default here and in autounattend.xml until 2026-09-22;
// the live Windows runner was still on it (PasswordLastSet empty) with WinRM,
// RDP and SMB listening on all interfaces. Never give this a default.
variable "winadmin_password" {
  type      = string
  sensitive = true
  default   = env("WINADMIN_PASSWORD")
  validation {
    condition     = length(var.winadmin_password) >= 14
    error_message = "Set WINADMIN_PASSWORD to at least 14 characters; it becomes the Administrator password on every cloned runner."
  }
}

source "proxmox-iso" "win2025" {
  proxmox_url              = var.proxmox_url
  username                 = var.proxmox_token_id
  token                    = var.proxmox_token_secret
  insecure_skip_tls_verify = true
  node                     = var.proxmox_node

  vm_name              = "mayfly-winserver-2025"
  template_name        = "mayfly-winserver-2025"
  template_description = "Windows Server 2025 + virtio + QEMU GA + Cloudbase-Init (built by packer)"
  os                   = "win11"
  cores                = 4
  sockets              = 1
  memory               = 8192
  scsi_controller      = "virtio-scsi-single"
  qemu_agent           = true

  // CRITICAL: Windows Server 2025 (like Win11 24H2) needs x86-64-v2 CPU
  // features; the plugin's default kvm64 model lacks them and Setup
  // reboot-loops in WinPE (0 disk writes, never reaches the GUI). "host"
  // passes the physical CPU through. Clones inherit this from the template.
  cpu_type = "host"

  // OS disk on virtio SCSI. The virtio storage driver is injected in WinPE
  // via Autounattend.xml <DriverPaths>, else Windows setup can't see it.
  disks {
    type         = "scsi"
    disk_size    = "60G"
    storage_pool = var.disk_storage
    format       = "raw"
    cache_mode   = "writeback"
  }

  // e1000 for the BUILD: Windows Server 2025 has an inbox Intel driver, so the
  // NIC works during OOBE/FirstLogon with zero driver injection — WinRM is
  // reachable immediately. (virtio's NetKVM injected in the windowsPE pass does
  // not reliably persist into the installed OS, which left WinRM unreachable.)
  // The post-install provisioner stages ALL virtio drivers into the driver
  // store, so clones can run on a virtio NIC too.
  network_adapters {
    model  = "e1000"
    bridge = "vmbr0"
  }

  // Boot ISO (Windows installer).
  boot_iso {
    type     = "ide"
    iso_file = "local:iso/windows-server-2025.iso"
    unmount  = true
  }

  // virtio-win drivers CD (Autounattend searches it for the storage driver).
  additional_iso_files {
    type     = "ide"
    index    = "3"
    iso_file = "local:iso/virtio-win.iso"
    unmount  = true
  }

  // Autounattend + bootstrap scripts on a generated CD. Windows auto-detects
  // Autounattend.xml at the root of any attached media.
  additional_iso_files {
    type             = "ide"
    index            = "2"
    iso_storage_pool = var.iso_storage
    unmount          = true
    // autounattend.xml is RENDERED, not copied: it carries the Administrator
    // password, so it cannot exist on disk as a literal. cd_content keeps the
    // rendered copy in memory — the old static file meant the password had to
    // be kept in sync with winadmin_password by hand, and both were committed.
    cd_content = {
      "autounattend.xml" = templatefile("./data/autounattend.xml.pkrtpl", {
        winadmin_password = var.winadmin_password
      })
    }
    cd_files = [
      "./scripts/bootstrap-winrm.ps1",
    ]
  }

  // "Press any key to boot from CD" — the prompt appears a few seconds after
  // SeaBIOS POSTs and skips the (empty) scsi0 disk, and only lingers briefly.
  // A single well-timed <enter> is fragile, so wait a beat then tap <enter>
  // several times across the window to guarantee we catch the prompt and boot
  // the Windows installer; Autounattend.xml then drives the rest hands-free.
  boot_wait    = "5s"
  boot_command = ["<enter><wait2s><enter><wait2s><enter><wait2s><enter>"]

  // WinRM comes up via bootstrap-winrm.ps1 (run from Autounattend FirstLogon).
  // Server 2025 install + generalize/first-boot is slow on lab storage, so give
  // it a generous window before Packer gives up waiting for the listener.
  communicator   = "winrm"
  winrm_username = "Administrator"
  winrm_password = var.winadmin_password
  winrm_timeout  = "2h"
  winrm_port     = 5985
  winrm_use_ssl  = false
  winrm_insecure = true

  // Proxmox API task ops (ISO upload, template convert) can be slow too.
  task_timeout = "20m"
}

build {
  sources = ["source.proxmox-iso.win2025"]

  // Stage all virtio drivers into the driver store so the cloned runners have
  // netkvm/vioserial/etc. even though only the storage driver was needed to
  // install.
  provisioner "powershell" {
    // pnputil exits non-zero (commonly 3 / 259) when some globbed INF paths
    // yield nothing or a driver is already present — that is not a failure for
    // us. Packer's powershell runner ends with `exit $LASTEXITCODE`, so we must
    // reset it or the whole build aborts. We swallow pnputil's code and exit 0.
    inline = [
      "Write-Host 'Installing all virtio drivers into the driver store...'",
      "$virtio = (Get-Volume | Where-Object { $_.FileSystemLabel -like 'virtio*' -or (Test-Path (\"$($_.DriveLetter):\\virtio-win-gt-x64.msi\")) } | Select-Object -First 1).DriveLetter",
      "if (-not $virtio) { $virtio = 'E' }",
      "cmd /c \"pnputil /add-driver $($virtio):\\*\\2k25\\amd64\\*.inf /subdirs /install\" 2>&1 | Out-Null",
      "cmd /c \"pnputil /add-driver $($virtio):\\*\\w11\\amd64\\*.inf /subdirs /install\" 2>&1 | Out-Null",
      "Write-Host 'virtio drivers staged.'",
      "$global:LASTEXITCODE = 0",
      "exit 0",
    ]
  }

  // Install the virtio guest tools (balloon service + qemu-ga + signed guest
  // drivers) from the attached virtio-win CD. The balloon service is what
  // reports real in-guest memory usage back to Proxmox; without it the host
  // shows the VM's full RAM allocation as "used" (QEMU resident size) even when
  // the guest is mostly idle. Survives sysprep, so every clone reports
  // accurately and the host can reclaim idle RAM if it ever needs to.
  provisioner "powershell" {
    inline = [
      "$exe = Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=5' | ForEach-Object { Join-Path $_.DeviceID 'virtio-win-guest-tools.exe' } | Where-Object { Test-Path $_ } | Select-Object -First 1",
      "if (-not $exe) { throw 'virtio-win-guest-tools.exe not found on any CD' }",
      "Write-Host \"Installing virtio guest tools from $exe\"",
      "Start-Process -Wait $exe -ArgumentList '/install','/quiet','/norestart'",
      // guest-tools installs the balloon service as Manual; make it Automatic
      // so memory reporting survives reboots on every clone.
      "Set-Service BALLOON -StartupType Automatic -ErrorAction SilentlyContinue",
      "$global:LASTEXITCODE = 0",
      "exit 0",
    ]
  }

  provisioner "powershell" {
    script = "./scripts/install-cloudbase-init.ps1"
  }

  // Upload the Cloudbase-Init configs that point at the NoCloud datasource.
  provisioner "file" {
    source      = "./data/cloudbase-init.conf"
    destination = "C:\\Program Files\\Cloudbase Solutions\\Cloudbase-Init\\conf\\cloudbase-init.conf"
  }
  provisioner "file" {
    source      = "./data/cloudbase-init-unattend.conf"
    destination = "C:\\Program Files\\Cloudbase Solutions\\Cloudbase-Init\\conf\\cloudbase-init-unattend.conf"
  }

  // Remove Windows Defender outright: this is a container-isolated CI runner,
  // and Defender's real-time scanning of containerd's image layers + every
  // job's file I/O pegs MsMpEng at ~100% CPU. Uninstalling the feature (Server
  // supports this, unlike Windows client) deletes MsMpEng entirely. The
  // windows-restart below completes the removal before generalize.
  provisioner "powershell" {
    inline = [
      "Write-Host 'Removing Windows Defender feature...'",
      "Uninstall-WindowsFeature -Name Windows-Defender -ErrorAction SilentlyContinue | Out-Null",
      // Open the ephemerd metrics port so the monitoring node can scrape
      // /metrics over the LAN (Windows Firewall blocks inbound :9090 by
      // default, unlike linux/mac). ephemerd binds the port but never opens
      // the firewall itself.
      "New-NetFirewallRule -DisplayName 'ephemerd-metrics-9090' -Direction Inbound -LocalPort 9090 -Protocol TCP -Action Allow -ErrorAction SilentlyContinue | Out-Null",
      "$global:LASTEXITCODE = 0",
      "exit 0",
    ]
  }

  // ephemerd runs every job in a containerd container wired up with HCN (Host
  // Compute Network) — that needs the Windows Containers feature, without which
  // ephemerd dies at startup with hcnCreateNetwork 0x6d9 (HNS unavailable).
  //
  // It ALSO runs those job containers with Hyper-V ISOLATION, which requires the
  // full Hyper-V role — not just Containers. ephemerd grants each per-job dir to
  // the VM account "NT VIRTUAL MACHINE\Virtual Machines" (SID S-1-5-83-0); that
  // account is registered by the Hyper-V VMMS service. Without the role, VMMS
  // and the SID don't exist and EVERY job dies at runner-env creation with
  //   icacls grant *S-1-5-83-0:M -> exit 1332: No mapping between account names
  //   and security IDs was done
  // (which is exactly what a fresh clone off a Containers-only template did).
  // Enable both features, then reboot so they're fully installed and baked into
  // the generalized template. Clones run on a host with nested virt (cpu=host),
  // so the hypervisor launches on first boot.
  //
  // Baking the roles into the template is the fast path (no first-boot reboot).
  // As a safety net, the Windows cloud-init user-data (winsetup.ps1, in
  // internal/cloudinit/templates) re-checks these roles on every boot and
  // installs+reboots if a stale template is missing them — so a clone off an
  // out-of-date template still self-heals, just with a longer first boot.
  provisioner "powershell" {
    inline = [
      "Write-Host 'Enabling Windows Containers + Hyper-V role...'",
      "Enable-WindowsOptionalFeature -Online -FeatureName Containers -All -NoRestart | Out-Null",
      "Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All -NoRestart | Out-Null",
      "$global:LASTEXITCODE = 0",
      "exit 0",
    ]
  }
  provisioner "windows-restart" {
    restart_timeout = "15m"
  }

  // Generalize (/quit) so Cloudbase-Init runs on first boot of each clone;
  // Packer then powers off via shutdown_command and converts to a template.
  provisioner "powershell" {
    script = "./scripts/sysprep.ps1"
  }
}
