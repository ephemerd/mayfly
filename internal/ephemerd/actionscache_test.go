package ephemerd

import (
	"strings"
	"testing"
)

// No opt-in, no block: a node that does not use the Actions cache renders what
// it did before the feature existed.
func TestRenderConfigActionsCacheOmittedByDefault(t *testing.T) {
	out, err := RenderConfig(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[actions_cache]") {
		t.Errorf("actions_cache rendered with nothing configured:\n%s", out)
	}
}

func TestRenderConfigActionsCacheEnabled(t *testing.T) {
	in := baseInputs()
	in.OS = "linux"
	in.ActionsCache = ActionsCache{Registry: "http://192.0.2.224:5001", Username: "ephemerd", Password: "s3cret"}
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	// Key names must match ephemerd's ActionsCacheConfig tags exactly —
	// unknown keys are dropped without a word.
	for _, want := range []string{
		"[actions_cache]",
		"enabled = true",
		`registry = "http://192.0.2.224:5001"`,
		`username = "ephemerd"`,
		`password = "s3cret"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("actions_cache block missing %q:\n%s", want, out)
		}
	}
}

// ephemerd's Actions cache is Linux-only: a windows or darwin node must never
// get the block, even if a caller hands it an input.
func TestRenderConfigActionsCacheLinuxOnly(t *testing.T) {
	for _, osName := range []string{"windows", "darwin"} {
		in := baseInputs()
		in.OS = osName
		in.ActionsCache = ActionsCache{Registry: "http://192.0.2.224:5001", Username: "u", Password: "p"}
		out, err := RenderConfig(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "[actions_cache]") {
			t.Errorf("%s node got an [actions_cache] block:\n%s", osName, out)
		}
	}
}
