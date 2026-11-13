package cloudinit

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The commands carry pipes, brackets and semicolons; the rendered document
// must still parse, and each command must come back byte-for-byte.
func TestRenderModelServer_CommandsSurviveYAML(t *testing.T) {
	cmds := []string{
		"/usr/local/sbin/mayfly-gpu-setup",
		"curl -fsSL https://ollama.com/install.sh | sh",
		"if [ -f /run/mayfly-reboot-required ]; then systemctl reboot; fi",
		`echo "quoted: value" # not a comment`,
	}
	r, err := RenderModelServer(ModelServerInputs{
		Hostname: "mfl-model-server-105",
		Files:    []WriteFile{{Path: "/etc/mayfly/model-server.env", Mode: "0644", ContentBase64: "eA=="}},
		Commands: cmds,
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hostname string   `yaml:"hostname"`
		Packages []string `yaml:"packages"`
		RunCmd   []string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(r.UserData), &doc); err != nil {
		t.Fatalf("user-data is not valid YAML: %v\n%s", err, r.UserData)
	}
	if doc.Hostname != "mfl-model-server-105" {
		t.Errorf("hostname = %q", doc.Hostname)
	}
	for _, p := range doc.Packages {
		if p == "docker.io" {
			t.Error("the model server installs Docker; it has no use for it")
		}
	}
	if len(doc.RunCmd) != len(cmds)+1 || doc.RunCmd[0] != "systemctl enable --now qemu-guest-agent" {
		t.Fatalf("runcmd = %q", doc.RunCmd)
	}
	for i, c := range cmds {
		if doc.RunCmd[i+1] != c {
			t.Errorf("runcmd[%d] = %q, want %q", i+1, doc.RunCmd[i+1], c)
		}
	}
}

func TestRenderModelServer_RequiresInputs(t *testing.T) {
	if _, err := RenderModelServer(ModelServerInputs{Hostname: "h"}); err == nil {
		t.Error("no files or commands: want an error")
	}
	if _, err := RenderModelServer(ModelServerInputs{Files: []WriteFile{{}}, Commands: []string{"x"}}); err == nil {
		t.Error("no hostname: want an error")
	}
}
