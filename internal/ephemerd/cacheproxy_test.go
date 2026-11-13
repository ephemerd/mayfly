package ephemerd

import (
	"strings"
	"testing"
)

// allCacheProxies is every gateway cache-proxy section mayfly can render, paired
// with the ConfigInputs field that turns it on. Adding a proxy to
// ephemerd.cacheProxies without adding it here leaves it untested — and
// TestRenderConfigCacheProxyTableCoversEveryProxy fails until it is added.
var allCacheProxies = []struct {
	section string
	enable  func(*ConfigInputs)
}{
	{"module_proxy", func(in *ConfigInputs) { in.ModuleProxyEnabled = true }},
	{"cargo_proxy", func(in *ConfigInputs) { in.CargoProxyEnabled = true }},
	{"composer_proxy", func(in *ConfigInputs) { in.ComposerProxyEnabled = true }},
	{"npm_proxy", func(in *ConfigInputs) { in.NpmProxyEnabled = true }},
	{"pip_proxy", func(in *ConfigInputs) { in.PipProxyEnabled = true }},
	{"pub_proxy", func(in *ConfigInputs) { in.PubProxyEnabled = true }},
	{"ghrel_proxy", func(in *ConfigInputs) { in.GhrelProxyEnabled = true }},
}

// enableAllCacheProxies flips every cache-proxy toggle on.
func enableAllCacheProxies(in *ConfigInputs) {
	for _, p := range allCacheProxies {
		p.enable(in)
	}
}

// TestRenderConfigCacheProxyTableCoversEveryProxy keeps the test table honest:
// it must name exactly the proxies the renderer knows about.
func TestRenderConfigCacheProxyTableCoversEveryProxy(t *testing.T) {
	rendered := cacheProxies(ConfigInputs{})
	if len(rendered) != len(allCacheProxies) {
		t.Fatalf("renderer knows %d proxies, test table has %d — update allCacheProxies",
			len(rendered), len(allCacheProxies))
	}
	for i, p := range rendered {
		if p.section != allCacheProxies[i].section {
			t.Errorf("proxy %d: renderer has %q, test table has %q", i, p.section, allCacheProxies[i].section)
		}
	}
}

// TestRenderConfigCacheProxyEnabled renders each proxy in isolation and checks
// it emits its own block and no other proxy's.
func TestRenderConfigCacheProxyEnabled(t *testing.T) {
	for _, p := range allCacheProxies {
		t.Run(p.section, func(t *testing.T) {
			in := baseInputs()
			in.OS = "linux"
			p.enable(&in)
			out, err := RenderConfig(in)
			if err != nil {
				t.Fatal(err)
			}
			want := "\n[" + p.section + "]\nenabled = true\n"
			if !strings.Contains(out, want) {
				t.Errorf("missing %q block:\n%s", p.section, out)
			}
			for _, other := range allCacheProxies {
				if other.section == p.section {
					continue
				}
				if strings.Contains(out, "["+other.section+"]") {
					t.Errorf("enabling %s also emitted [%s]:\n%s", p.section, other.section, out)
				}
			}
		})
	}
}

// TestRenderConfigCacheProxiesOmittedWhenDisabled proves a proxy left unset puts
// nothing in the config — the default for every fleet that has not opted in.
func TestRenderConfigCacheProxiesOmittedWhenDisabled(t *testing.T) {
	in := baseInputs()
	in.OS = "linux"
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range allCacheProxies {
		if strings.Contains(out, "["+p.section+"]") {
			t.Errorf("[%s] emitted while disabled:\n%s", p.section, out)
		}
	}
}

// TestRenderConfigCacheProxiesSkippedOnWindows: every cache proxy listens on the
// bridge gateway, which a Hyper-V-isolated Windows job container cannot route
// to. Injecting their env there hangs the build, so none may render on windows.
func TestRenderConfigCacheProxiesSkippedOnWindows(t *testing.T) {
	in := baseInputs()
	in.OS = "windows"
	enableAllCacheProxies(&in)
	in.DindEnabled = true
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range allCacheProxies {
		if strings.Contains(out, "["+p.section+"]") {
			t.Errorf("[%s] emitted on windows:\n%s", p.section, out)
		}
	}
	// dind is not a gateway listener and must still render.
	if !strings.Contains(out, "[dind]") {
		t.Errorf("windows lost its [dind] block:\n%s", out)
	}
}

// TestRenderConfigCacheProxyOrderIsStable guards the ConfigHash. The rendered
// config is fingerprinted, so any nondeterminism in proxy ordering (a map range,
// say) would change every node's hash between runs and make `mayfly plan` report
// permanent phantom drift across the whole fleet.
func TestRenderConfigCacheProxyOrderIsStable(t *testing.T) {
	in := baseInputs()
	in.OS = "linux"
	enableAllCacheProxies(&in)
	first, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		out, err := RenderConfig(in)
		if err != nil {
			t.Fatal(err)
		}
		if out != first {
			t.Fatalf("render %d differs from the first — proxy order is not deterministic:\n--- first ---\n%s\n--- got ---\n%s", i, first, out)
		}
	}
	// And the order is the documented one, not merely self-consistent.
	want := "\n[module_proxy]\nenabled = true\n" +
		"\n[cargo_proxy]\nenabled = true\n" +
		"\n[composer_proxy]\nenabled = true\n" +
		"\n[npm_proxy]\nenabled = true\n" +
		"\n[pip_proxy]\nenabled = true\n" +
		"\n[pub_proxy]\nenabled = true\n" +
		"\n[ghrel_proxy]\nenabled = true\n"
	if !strings.Contains(first, want) {
		t.Errorf("cache proxies not rendered in the documented order:\n%s", first)
	}
}

// Goldens captured by running the renderer at the commit BEFORE npm/pip/pub/
// ghrel were added (git show HEAD:internal/ephemerd/config.go, built and run
// standalone). They are the anti-drift contract: mayfly re-renders every node's
// config.toml and hashes it, so a single byte added to a fleet that enabled none
// of the new proxies would show up as config drift on every node of every pool
// and trigger a fleet-wide ephemerd restart for no reason.
const (
	goldenNoProxies = `[github]
owner = "ephpm"
app_id = 1
installation_id = 2
private_key_path = "/etc/ephemerd/app.pem"

[log]
level = "info"
`

	goldenPreExistingProxies = `[github]
owner = "ephpm"
app_id = 1
installation_id = 2
private_key_path = "/etc/ephemerd/app.pem"

[module_proxy]
enabled = true

[cargo_proxy]
enabled = true

[composer_proxy]
enabled = true

[log]
level = "info"
`
)

// TestRenderConfigNoProxiesByteIdenticalToPreChange is the anti-drift test: a
// fleet that enables no proxy at all must render exactly the bytes it rendered
// before the new proxies existed.
func TestRenderConfigNoProxiesByteIdenticalToPreChange(t *testing.T) {
	for _, os := range []string{"", "linux", "windows"} {
		t.Run("os="+os, func(t *testing.T) {
			in := baseInputs()
			in.OS = os
			out, err := RenderConfig(in)
			if err != nil {
				t.Fatal(err)
			}
			if out != goldenNoProxies {
				t.Errorf("rendered config drifted from pre-change bytes\n--- want ---\n%q\n--- got ---\n%q", goldenNoProxies, out)
			}
		})
	}
}

// TestRenderConfigExistingProxiesByteIdenticalToPreChange covers the fleets that
// DO enable the proxies mayfly already supported: their config must also be
// unchanged, which pins the new sections to render strictly after the old ones.
func TestRenderConfigExistingProxiesByteIdenticalToPreChange(t *testing.T) {
	in := baseInputs()
	in.OS = "linux"
	in.ModuleProxyEnabled = true
	in.CargoProxyEnabled = true
	in.ComposerProxyEnabled = true
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if out != goldenPreExistingProxies {
		t.Errorf("rendered config drifted from pre-change bytes\n--- want ---\n%q\n--- got ---\n%q", goldenPreExistingProxies, out)
	}
}
