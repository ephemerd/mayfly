package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func modelServerCfg(m ModelServerSpec) *Config {
	return &Config{
		GitHub:      GitHub{Owner: "o", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"},
		Proxmox:     Proxmox{URL: "u", TokenID: "t", Secret: "s", Node: "n", Storage: "st"},
		ModelServer: m,
	}
}

func p40VM() *ModelServerVMSpec {
	return &ModelServerVMSpec{Template: "mayfly-ubuntu-24.04-amd64", HostPCI: []string{"p40"}}
}

func TestModelServerResolvedDefaults(t *testing.T) {
	m := ModelServerSpec{Enabled: true}.Resolved()
	if m.Port != 11434 {
		t.Errorf("port default = %d, want 11434", m.Port)
	}
	if len(m.Models) != 1 || m.Models[0] != "qwen3:8b" {
		t.Errorf("models default = %v, want [qwen3:8b]", m.Models)
	}
	vm := ModelServerVMSpec{}.Resolved()
	if vm.Cores != 4 || vm.MemoryMB != 8192 || vm.DiskGB != 100 {
		t.Errorf("vm defaults = cores %d mem %d disk %d, want 4/8192/100", vm.Cores, vm.MemoryMB, vm.DiskGB)
	}
}

// The block parses from TOML the way the README documents it.
func TestModelServerParses(t *testing.T) {
	var c Config
	_, err := toml.Decode(`
[model_server]
enabled = true
models = ["qwen3:14b"]
[model_server.vm]
node = "node-b"
template = "mayfly-ubuntu-24.04-amd64"
vlan_tag = 2
hostpci = ["p40"]
`, &c)
	if err != nil {
		t.Fatal(err)
	}
	m := c.ModelServer
	if !m.Active() || m.VM.Node != "node-b" || m.VM.VLANTag != 2 || m.VM.HostPCI[0] != "p40" || m.Models[0] != "qwen3:14b" {
		t.Errorf("parsed = %+v vm %+v", m, m.VM)
	}
}

func TestModelServerDisabledIsNotValidated(t *testing.T) {
	// A half-written block must not fail the config while it is switched off.
	if err := modelServerCfg(ModelServerSpec{Port: -1}).Validate(); err != nil {
		t.Errorf("disabled block failed validation: %v", err)
	}
	if err := modelServerCfg(ModelServerSpec{Enabled: true, VM: p40VM()}).Validate(); err != nil {
		t.Errorf("minimal valid block failed: %v", err)
	}
}

func TestModelServerValidation(t *testing.T) {
	vm := func(mut func(*ModelServerVMSpec)) *ModelServerVMSpec {
		v := p40VM()
		mut(v)
		return v
	}
	tests := []struct {
		name    string
		m       ModelServerSpec
		wantErr string
	}{
		{"no vm", ModelServerSpec{Enabled: true}, "no [model_server.vm]"},
		{"no gpu", ModelServerSpec{Enabled: true, VM: vm(func(v *ModelServerVMSpec) { v.HostPCI = nil })}, "without a GPU"},
		{"no template", ModelServerSpec{Enabled: true, VM: vm(func(v *ModelServerVMSpec) { v.Template = "" })}, "template is required"},
		{"bad hostpci", ModelServerSpec{Enabled: true, VM: vm(func(v *ModelServerVMSpec) { v.HostPCI = []string{"07:00.0;rm"} })}, "neither a PCI address"},
		{"bad vlan", ModelServerSpec{Enabled: true, VM: vm(func(v *ModelServerVMSpec) { v.VLANTag = 5000 })}, "vlan_tag 5000"},
		{"bad port", ModelServerSpec{Enabled: true, Port: 70000, VM: p40VM()}, "port 70000"},
		{"bad model", ModelServerSpec{Enabled: true, Models: []string{"qwen3; reboot"}, VM: p40VM()}, "not a valid model name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := modelServerCfg(tt.m).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestIsPCIAddress(t *testing.T) {
	for _, s := range []string{"0000:07:00.0", "07:00.0", "07:00", "0000:0a:1f.7"} {
		if !IsPCIAddress(s) {
			t.Errorf("IsPCIAddress(%q) = false", s)
		}
	}
	for _, s := range []string{"p40", "tesla-p40", "07:00.8", "7:00.0", ""} {
		if IsPCIAddress(s) {
			t.Errorf("IsPCIAddress(%q) = true", s)
		}
	}
}

func TestModelServerEndpoint(t *testing.T) {
	m := ModelServerSpec{Enabled: true, VM: p40VM()}
	if got := m.Endpoint("192.0.2.50"); got != "http://192.0.2.50:11434/v1" {
		t.Errorf("Endpoint = %q", got)
	}
	if got := m.Endpoint(""); got != "" {
		t.Errorf("Endpoint with no address = %q, want empty", got)
	}
	if got := (ModelServerSpec{}).Endpoint("192.0.2.50"); got != "" {
		t.Errorf("disabled Endpoint = %q, want empty", got)
	}
}
