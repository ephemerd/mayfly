package modelserver

import (
	"strings"
	"testing"
)

func content(t *testing.T, spec Spec, path string) string {
	t.Helper()
	for _, f := range Files(spec) {
		if f.Path == path {
			return string(f.Content)
		}
	}
	t.Fatalf("no file at %s", path)
	return ""
}

// The driver branch is the one thing that silently breaks a P40 if it
// drifts: newer branches install cleanly and then see no GPU.
func TestGPUSetupPinsThePascalBranch(t *testing.T) {
	s := content(t, Spec{Port: 11434}, GPUSetupPath)
	for _, want := range []string{"nvidia-headless-${BRANCH}-server", "nvidia-utils-${BRANCH}-server", "BRANCH=580", "linux-headers-$(uname -r)"} {
		if !strings.Contains(s, want) {
			t.Errorf("gpu setup missing %q", want)
		}
	}
}

func TestOllamaListensOnTheConfiguredPort(t *testing.T) {
	s := content(t, Spec{Port: 11500}, OllamaDropIn)
	if !strings.Contains(s, `OLLAMA_HOST=0.0.0.0:11500`) {
		t.Errorf("drop-in does not listen on 0.0.0.0:11500:\n%s", s)
	}
}

func TestEnvCarriesModels(t *testing.T) {
	s := content(t, Spec{Port: 11434, Models: []string{"qwen3:8b", "gemma3:12b"}}, EnvPath)
	if !strings.Contains(s, `MODEL_SERVER_MODELS="qwen3:8b gemma3:12b"`) || !strings.Contains(s, "MODEL_SERVER_PORT=11434") {
		t.Errorf("env file:\n%s", s)
	}
}

// The driver must be installed before Ollama, or Ollama's installer pulls
// NVIDIA's newest driver itself; and the model pull must not block boot.
func TestFirstBootOrder(t *testing.T) {
	cmds := FirstBootCommands()
	idx := func(sub string) int {
		for i, c := range cmds {
			if strings.Contains(c, sub) {
				return i
			}
		}
		t.Fatalf("no first-boot command containing %q", sub)
		return -1
	}
	if idx(GPUSetupPath) > idx("ollama.com/install.sh") {
		t.Error("ollama installs before the pinned driver")
	}
	if !strings.Contains(cmds[idx("start --no-block")], PullUnit) {
		t.Error("model pull is not started without blocking")
	}
	if idx(RebootFlag) != len(cmds)-1 {
		t.Error("the conditional reboot is not the last command")
	}
}

func TestHashTracksSpec(t *testing.T) {
	a := Hash(Spec{Port: 11434, Models: []string{"qwen3:8b"}})
	if a != Hash(Spec{Port: 11434, Models: []string{"qwen3:8b"}}) {
		t.Error("hash is not stable")
	}
	if a == Hash(Spec{Port: 11434, Models: []string{"qwen3:14b"}}) {
		t.Error("hash ignores the model list")
	}
}
