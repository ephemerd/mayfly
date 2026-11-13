package ephemerd

import (
	"strings"
	"testing"
)

// A fleet with no pull-through cache must render exactly what it rendered
// before the mirror existed.
func TestRenderConfigRegistryMirrorOmittedByDefault(t *testing.T) {
	out, err := RenderConfig(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"[registry_mirror]", "endpoint", "fallback_to_origin"} {
		if strings.Contains(out, bad) {
			t.Errorf("mirror artifact %q emitted with no mirror configured:\n%s", bad, out)
		}
	}
}

func TestRenderConfigRegistryMirrorEnabled(t *testing.T) {
	in := baseInputs()
	in.RegistryMirror = NewRegistryMirror("http://10.0.0.45:5000", true)
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[registry_mirror]",
		"enabled = true",
		`endpoint = "http://10.0.0.45:5000"`,
		"fallback_to_origin = true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mirror block missing %q:\n%s", want, out)
		}
	}
}

// ephemerd derives plaintext-vs-TLS from the endpoint scheme, so mayfly must
// never emit a TLS-relaxing knob of its own for either scheme.
func TestRenderConfigRegistryMirrorHTTPSNotInsecure(t *testing.T) {
	in := baseInputs()
	in.RegistryMirror = NewRegistryMirror("https://cache.example.com", true)
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `endpoint = "https://cache.example.com"`) {
		t.Errorf("https endpoint not rendered:\n%s", out)
	}
	if strings.Contains(out, "insecure") {
		t.Errorf("insecure emitted for an https mirror:\n%s", out)
	}
}

func TestRenderConfigRegistryMirrorFailClosed(t *testing.T) {
	in := baseInputs()
	in.RegistryMirror = NewRegistryMirror("http://10.0.0.45:5000", false)
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "fallback_to_origin = false") {
		t.Errorf("fallback_to_origin = false not rendered:\n%s", out)
	}
}

// An empty endpoint is the "cache off / address unknown" signal and must
// render nothing, even if the other fields are set.
func TestRenderConfigRegistryMirrorEmptyEndpoint(t *testing.T) {
	in := baseInputs()
	in.RegistryMirror = RegistryMirror{FailOpen: true}
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[registry_mirror]") {
		t.Errorf("mirror block emitted with no endpoint:\n%s", out)
	}
}

// Unlike the gateway cache proxies, the mirror applies on windows too: it is
// an ordinary LAN endpoint, not a bridge-gateway listener.
func TestRenderConfigRegistryMirrorOnWindows(t *testing.T) {
	in := baseInputs()
	in.OS = "windows"
	in.ModuleProxyEnabled = true
	in.RegistryMirror = NewRegistryMirror("http://10.0.0.45:5000", true)
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[registry_mirror]") {
		t.Errorf("mirror block missing on windows:\n%s", out)
	}
	if strings.Contains(out, "[module_proxy]") {
		t.Errorf("gateway proxy leaked onto windows:\n%s", out)
	}
}
