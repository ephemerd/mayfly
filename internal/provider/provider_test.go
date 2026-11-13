package provider

import "testing"

// Compile-time assertion that the Proxmox adapter satisfies the interfaces.
var (
	_ Provider = (*Proxmox)(nil)
	_ Instance = (*proxmoxInstance)(nil)
)

// A clone is only reconfigured when the pool asks for network isolation; an
// unconfigured pool must produce the same config write it always did.
func TestNet0Options(t *testing.T) {
	const cloned = "virtio=BC:24:11:00:00:01,bridge=vmbr0"

	tests := []struct {
		name     string
		base     string
		vlanTag  int
		firewall bool
		want     string // "" = no net0 option written at all
	}{
		{name: "neither: no net0 write", base: cloned},
		{name: "tag only", base: cloned, vlanTag: 42, want: "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=42"},
		{name: "firewall only", base: cloned, firewall: true, want: "virtio=BC:24:11:00:00:01,bridge=vmbr0,firewall=1"},
		{name: "tag and firewall", base: cloned, vlanTag: 42, firewall: true, want: "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=42,firewall=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := net0Options(tt.base, tt.vlanTag, tt.firewall)
			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("net0Options() = %+v, want no options", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("net0Options() returned %d options, want 1", len(got))
			}
			if got[0].Name != "net0" {
				t.Errorf("option name = %q, want \"net0\"", got[0].Name)
			}
			if got[0].Value != tt.want {
				t.Errorf("net0 = %q, want %q", got[0].Value, tt.want)
			}
		})
	}
}

// diskSizeGB must pull the provisioned size out of a Proxmox disk config string
// so the resize shrink-guard compares against the live disk, not stale state.
func TestDiskSizeGB(t *testing.T) {
	ok := []struct {
		spec string
		want int
	}{
		{"local-lvm:vm-100-disk-0,size=60G", 60},
		{"local-lvm:vm-100-disk-0,size=1T", 1024},
		{"nvme:vm-9-disk-1,size=512M", 0},              // sub-GB rounds down
		{"local:disk,iothread=1,size=100G,ssd=1", 100}, // size not first field
		{"local-lvm:vm-7-disk-0,size=34359738368", 32}, // bare bytes = 32 GiB
	}
	for _, tc := range ok {
		got, err := diskSizeGB(tc.spec)
		if err != nil {
			t.Errorf("diskSizeGB(%q) errored: %v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("diskSizeGB(%q) = %d, want %d", tc.spec, got, tc.want)
		}
	}

	for _, spec := range []string{"local-lvm:vm-100-disk-0", "", "local:disk,size=abcG"} {
		if _, err := diskSizeGB(spec); err == nil {
			t.Errorf("diskSizeGB(%q) = nil error, want one (no/invalid size)", spec)
		}
	}
}
