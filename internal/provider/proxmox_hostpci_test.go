package provider

import (
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

func optMap(opts []proxmox.VirtualMachineOption) map[string]any {
	m := map[string]any{}
	for _, o := range opts {
		m[o.Name] = o.Value
	}
	return m
}

// No devices must add no options at all, so every existing pool clones
// exactly as before: no balloon change, no machine change.
func TestHostPCIOptions_NoneIsANoop(t *testing.T) {
	opts, err := hostPCIOptions(nil)
	if err != nil || len(opts) != 0 {
		t.Fatalf("hostPCIOptions(nil) = %v, %v; want no options", opts, err)
	}
}

// A plain compute card (the P40 case): the device, ballooning off, and the
// machine type left alone — no pcie means no reason to move off i440fx.
func TestHostPCIOptions_ComputeCardKeepsMachineType(t *testing.T) {
	opts, err := hostPCIOptions([]HostPCI{{Mapping: "p40"}})
	if err != nil {
		t.Fatal(err)
	}
	m := optMap(opts)
	if m["hostpci0"] != "mapping=p40" {
		t.Errorf("hostpci0 = %v, want mapping=p40", m["hostpci0"])
	}
	if m["balloon"] != 0 {
		t.Errorf("balloon = %v, want 0: passthrough pins guest RAM", m["balloon"])
	}
	if _, ok := m["machine"]; ok {
		t.Errorf("machine = %v, want it untouched without pcie", m["machine"])
	}
}

// PCIe on any device must switch the VM to q35, and the flags render in the
// order Proxmox documents them.
func TestHostPCIOptions_PCIeSwitchesToQ35(t *testing.T) {
	off := false
	opts, err := hostPCIOptions([]HostPCI{
		{Host: "0000:07:00.0"},
		{Host: "04:00.0", PCIe: true, ROMBar: &off, XVGA: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := optMap(opts)
	if m["hostpci0"] != "0000:07:00.0" {
		t.Errorf("hostpci0 = %v", m["hostpci0"])
	}
	if m["hostpci1"] != "04:00.0,pcie=1,rombar=0,x-vga=1" {
		t.Errorf("hostpci1 = %v", m["hostpci1"])
	}
	if m["machine"] != "q35" {
		t.Errorf("machine = %v, want q35 when a device asks for pcie", m["machine"])
	}
	if m["balloon"] != 0 {
		t.Errorf("balloon = %v, want 0", m["balloon"])
	}
}

func TestHostPCIOptions_RejectsAmbiguousDevices(t *testing.T) {
	for name, d := range map[string]HostPCI{
		"both":    {Host: "07:00.0", Mapping: "p40"},
		"neither": {PCIe: true},
	} {
		if _, err := hostPCIOptions([]HostPCI{d}); err == nil {
			t.Errorf("%s: no error, want one", name)
		}
	}
	many := make([]HostPCI, 17)
	for i := range many {
		many[i] = HostPCI{Mapping: "x"}
	}
	if _, err := hostPCIOptions(many); err == nil || !strings.Contains(err.Error(), "16") {
		t.Errorf("17 devices: err = %v, want the 16-device limit", err)
	}
}
