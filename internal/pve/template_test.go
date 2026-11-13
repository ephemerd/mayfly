package pve

import "testing"

// BuildNet0 is what decides whether a runner lands on the flat LAN or an
// isolated VLAN, so pin the exact string for every combination of the two
// knobs — including the untouched case, which must stay byte-identical to the
// NIC the template hands the clone.
func TestBuildNet0(t *testing.T) {
	// The value a fresh clone inherits: template model/bridge plus the MAC
	// Proxmox generated for the clone.
	const cloned = "virtio=BC:24:11:00:00:01,bridge=vmbr0"

	tests := []struct {
		name     string
		base     string
		vlanTag  int
		firewall bool
		want     string
	}{
		{
			name: "neither: inherited nic untouched",
			base: cloned,
			want: "virtio=BC:24:11:00:00:01,bridge=vmbr0",
		},
		{
			name:    "tag only",
			base:    cloned,
			vlanTag: 42,
			want:    "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=42",
		},
		{
			name:     "firewall only",
			base:     cloned,
			firewall: true,
			want:     "virtio=BC:24:11:00:00:01,bridge=vmbr0,firewall=1",
		},
		{
			name:     "tag and firewall",
			base:     cloned,
			vlanTag:  42,
			firewall: true,
			want:     "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=42,firewall=1",
		},
		{
			name: "empty base falls back to the template default",
			want: DefaultNet0,
		},
		{
			name:     "empty base with both knobs",
			vlanTag:  100,
			firewall: true,
			want:     "virtio,bridge=vmbr0,tag=100,firewall=1",
		},
		{
			name:    "non-default bridge is preserved",
			base:    "virtio=BC:24:11:00:00:02,bridge=vmbr1",
			vlanTag: 7,
			want:    "virtio=BC:24:11:00:00:02,bridge=vmbr1,tag=7",
		},
		{
			name:    "existing tag is replaced, not duplicated",
			base:    "virtio,bridge=vmbr0,tag=10",
			vlanTag: 20,
			want:    "virtio,bridge=vmbr0,tag=20",
		},
		{
			name: "existing tag and firewall are cleared when unset",
			base: "virtio,bridge=vmbr0,tag=10,firewall=1",
			want: "virtio,bridge=vmbr0",
		},
		{
			name:     "unrelated options survive",
			base:     "virtio=BC:24:11:00:00:03,bridge=vmbr0,mtu=9000,queues=4",
			vlanTag:  30,
			firewall: true,
			want:     "virtio=BC:24:11:00:00:03,bridge=vmbr0,mtu=9000,queues=4,tag=30,firewall=1",
		},
		{
			name:    "zero and negative tags are treated as untagged",
			base:    cloned,
			vlanTag: -1,
			want:    "virtio=BC:24:11:00:00:01,bridge=vmbr0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildNet0(tt.base, tt.vlanTag, tt.firewall); got != tt.want {
				t.Errorf("BuildNet0(%q, %d, %v) = %q, want %q", tt.base, tt.vlanTag, tt.firewall, got, tt.want)
			}
		})
	}
}

// BridgeFromNet0 decides which bridge the vlan_tag preflight checks — a clone
// inherits its bridge from the template's net0 — so pin the extraction, incl.
// the fallbacks for an empty or bridge-less value.
func TestBridgeFromNet0(t *testing.T) {
	tests := []struct {
		name string
		net0 string
		want string
	}{
		{"clone-inherited nic", "virtio=BC:24:11:00:00:01,bridge=vmbr0", "vmbr0"},
		{"non-default bridge", "virtio=BC:24:11:00:00:01,bridge=vmbr1,tag=42", "vmbr1"},
		{"template default", DefaultNet0, "vmbr0"},
		{"empty falls back to default", "", DefaultBridge},
		{"no bridge clause falls back", "virtio=BC:24:11:00:00:01", DefaultBridge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BridgeFromNet0(tt.net0); got != tt.want {
				t.Errorf("BridgeFromNet0(%q) = %q, want %q", tt.net0, got, tt.want)
			}
		})
	}
}
