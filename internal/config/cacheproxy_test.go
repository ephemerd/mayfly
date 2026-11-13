package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// TestCacheProxyTogglesParse pins the TOML section names. A typo in a `toml:"…"`
// tag has no compile-time or decode-time symptom — BurntSushi/toml drops unknown
// keys silently — so a misnamed toggle would parse to false forever and the
// proxy would just never turn on, with the operator's config looking correct.
func TestCacheProxyTogglesParse(t *testing.T) {
	const src = `
[module_proxy]
enabled = true

[cargo_proxy]
enabled = true

[composer_proxy]
enabled = true

[npm_proxy]
enabled = true

[pip_proxy]
enabled = true

[pub_proxy]
enabled = true

[ghrel_proxy]
enabled = true
`
	var c Config
	if _, err := toml.Decode(src, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, tc := range []struct {
		section string
		got     bool
	}{
		{"module_proxy", c.ModuleProxy.Enabled},
		{"cargo_proxy", c.CargoProxy.Enabled},
		{"composer_proxy", c.ComposerProxy.Enabled},
		{"npm_proxy", c.NpmProxy.Enabled},
		{"pip_proxy", c.PipProxy.Enabled},
		{"pub_proxy", c.PubProxy.Enabled},
		{"ghrel_proxy", c.GhrelProxy.Enabled},
	} {
		if !tc.got {
			t.Errorf("[%s] enabled = true did not parse — check the toml tag", tc.section)
		}
	}
}

// TestCacheProxyTogglesDefaultOff: an empty config must leave every proxy off,
// so existing fleets pick up nothing new on upgrade.
func TestCacheProxyTogglesDefaultOff(t *testing.T) {
	var c Config
	if _, err := toml.Decode(``, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, tc := range []struct {
		section string
		got     bool
	}{
		{"module_proxy", c.ModuleProxy.Enabled},
		{"cargo_proxy", c.CargoProxy.Enabled},
		{"composer_proxy", c.ComposerProxy.Enabled},
		{"npm_proxy", c.NpmProxy.Enabled},
		{"pip_proxy", c.PipProxy.Enabled},
		{"pub_proxy", c.PubProxy.Enabled},
		{"ghrel_proxy", c.GhrelProxy.Enabled},
	} {
		if tc.got {
			t.Errorf("[%s] defaulted to enabled", tc.section)
		}
	}
}
