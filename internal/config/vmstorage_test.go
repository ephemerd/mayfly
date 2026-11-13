package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// vm_storage parses off a pool and is left empty when unset — empty is the
// backward-compatible LINKED-clone default, so an existing pool that never
// mentions vm_storage must decode to "".
func TestVMStorageParses(t *testing.T) {
	const src = `
[proxmox]
url = "https://pve:8006"
storage = "local"

[pools.node-b]
hostname = "ci-node-b.example.com"
size = 1
vlan_tag = 20
vm_storage = "local-lvm"

[pools.plain]
hostname = "ci-plain.example.com"
size = 1
vlan_tag = 21
`
	var c Config
	if _, err := toml.Decode(src, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := c.Pools["node-b"].VMStorage; got != "local-lvm" {
		t.Errorf("pool vm_storage = %q, want %q", got, "local-lvm")
	}
	// Unset stays empty — the historical linked-clone behavior.
	if got := c.Pools["plain"].VMStorage; got != "" {
		t.Errorf("unset vm_storage = %q, want empty (linked clone)", got)
	}
}

// A VM pool that sets vm_storage validates fine.
func TestVMStorageValidatesOnVMPool(t *testing.T) {
	c := validBase()
	c.Pools = map[string]PoolSpec{
		"node-b": {Hostname: "ci-node-b.example.com", Size: 1, VLANTag: 20, VMStorage: "local-lvm"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a VM pool with vm_storage to be valid", err)
	}
}

// A windows hypervisor pool provisions a VM disk just like linux, so it must
// ACCEPT vm_storage — only darwin and enrolled-linux (no mayfly disk) reject it.
func TestVMStorageValidatesOnWindowsPool(t *testing.T) {
	c := validBase()
	c.Pools = map[string]PoolSpec{
		"win": {Hostname: "ci-win.example.com", OS: "windows", Template: "mayfly-winserver-2025", Size: 1, VLANTag: 20, VMStorage: "local-lvm"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a windows pool with vm_storage to be valid", err)
	}
}

// vm_storage is a Proxmox VM-disk placement knob; enrolled darwin bare metal
// has no mayfly-provisioned disk, so setting it is a config error — the same
// class of rejection template/node/storage already get on darwin.
func TestVMStorageRejectedOnDarwinPool(t *testing.T) {
	c := validBase()
	c.Pools = map[string]PoolSpec{
		"mac": {Hostname: "ci-mac.example.com", OS: "darwin", Members: []string{"mini1.lan"}, VMStorage: "local-lvm"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "do not apply to darwin") {
		t.Fatalf("Validate() = %v, want vm_storage rejected on a darwin pool", err)
	}
}

// Likewise for an enrolled (members = [...]) linux pool: it enrolls existing
// bare metal, so a VM-disk storage is meaningless.
func TestVMStorageRejectedOnEnrolledLinuxPool(t *testing.T) {
	c := validBase()
	c.Pools = map[string]PoolSpec{
		"arm": {Hostname: "ci-arm.example.com", Members: []string{"arm1.lan"}, VMStorage: "local-lvm"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "do not apply to enrolled") {
		t.Fatalf("Validate() = %v, want vm_storage rejected on an enrolled linux pool", err)
	}
}
