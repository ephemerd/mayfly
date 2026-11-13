package config

import (
	"strings"
	"testing"
)

func cacheCfg(r RegistryCacheSpec) *Config {
	return &Config{
		GitHub:        GitHub{Owner: "o", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"},
		Proxmox:       Proxmox{URL: "u", TokenID: "t", Secret: "s", Node: "n", Storage: "st"},
		Monitoring:    &MonitoringSpec{},
		RegistryCache: r,
	}
}

func TestRegistryCacheResolvedDefaults(t *testing.T) {
	r := RegistryCacheSpec{Enabled: true}.Resolved()
	if r.Port != 5000 {
		t.Errorf("port default = %d, want 5000", r.Port)
	}
	if r.Upstream != "https://registry-1.docker.io" {
		t.Errorf("upstream default = %q", r.Upstream)
	}
	if r.MaxSizeGB != 20 {
		t.Errorf("max_size_gb default = %d, want 20", r.MaxSizeGB)
	}
	if r.TTL != "168h" {
		t.Errorf("ttl default = %q, want 168h", r.TTL)
	}
}

// Explicit values must survive Resolved untouched.
func TestRegistryCacheResolvedKeepsExplicit(t *testing.T) {
	in := RegistryCacheSpec{
		Enabled: true, Port: 5555, Upstream: "https://ghcr.io",
		MaxSizeGB: 100, TTL: "24h",
	}
	if got := in.Resolved(); got != in {
		t.Errorf("Resolved() mutated explicit values: %+v", got)
	}
}

// fail_open defaults to true: a cache outage must not take CI down.
func TestRegistryCacheFailOpenDefault(t *testing.T) {
	if !(RegistryCacheSpec{}).ResolvedFailOpen() {
		t.Error("fail_open should default to true")
	}
	no := false
	if (RegistryCacheSpec{FailOpen: &no}).ResolvedFailOpen() {
		t.Error("explicit fail_open = false not honored")
	}
}

func TestRegistryCacheMirrorEndpoint(t *testing.T) {
	tests := []struct {
		name   string
		spec   RegistryCacheSpec
		hostIP string
		want   string
	}{
		{"disabled", RegistryCacheSpec{}, "10.0.0.45", ""},
		{"derived from host", RegistryCacheSpec{Enabled: true}, "10.0.0.45", "http://10.0.0.45:5000"},
		{"derived honors port", RegistryCacheSpec{Enabled: true, Port: 5555}, "10.0.0.45", "http://10.0.0.45:5555"},
		{"no host yet", RegistryCacheSpec{Enabled: true}, "", ""},
		{
			"explicit endpoint wins",
			RegistryCacheSpec{Enabled: true, Endpoint: "https://cache.example.com/"},
			"10.0.0.45",
			"https://cache.example.com",
		},
		{
			"explicit endpoint without a host",
			RegistryCacheSpec{Enabled: true, Endpoint: "http://cache.lan:5000"},
			"",
			"http://cache.lan:5000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.MirrorEndpoint(tt.hostIP); got != tt.want {
				t.Errorf("MirrorEndpoint(%q) = %q, want %q", tt.hostIP, got, tt.want)
			}
		})
	}
}

// A config that never mentions the cache must validate exactly as before.
func TestRegistryCacheAbsentValidates(t *testing.T) {
	c := cacheCfg(RegistryCacheSpec{})
	c.Pools = map[string]PoolSpec{"p": {Hostname: "ci.example.com", Size: 1, VLANTag: 10}}
	if err := c.Validate(); err != nil {
		t.Fatalf("config without a registry_cache block should validate: %v", err)
	}
	// ...and a disabled block must not trip any of the cache checks either,
	// even if its other fields are nonsense.
	c.RegistryCache = RegistryCacheSpec{Port: -1, Upstream: "not a url", TTL: "nope"}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled registry_cache should be inert: %v", err)
	}
}

func TestRegistryCacheDefaultsValidate(t *testing.T) {
	if err := cacheCfg(RegistryCacheSpec{Enabled: true}).Validate(); err != nil {
		t.Fatalf("enabled cache with all defaults should validate: %v", err)
	}
}

func TestRegistryCacheValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			"needs a utility host",
			func(c *Config) { c.Monitoring = nil },
			"no [monitoring] section",
		},
		{
			"port out of range",
			func(c *Config) { c.RegistryCache.Port = 70000 },
			"port 70000 out of range",
		},
		{
			"negative cap",
			func(c *Config) { c.RegistryCache.MaxSizeGB = -1 },
			"max_size_gb must be >= 0",
		},
		{
			"upstream not a url",
			func(c *Config) { c.RegistryCache.Upstream = "registry-1.docker.io" },
			"registry_cache.upstream",
		},
		{
			"endpoint not a url",
			func(c *Config) { c.RegistryCache.Endpoint = "10.0.0.45:5000" },
			"registry_cache.endpoint",
		},
		{
			"ttl not a duration",
			func(c *Config) { c.RegistryCache.TTL = "one week" },
			"is not a duration",
		},
		{
			// The whole point of the cap is that it trips before the disk
			// fills; a cap >= the disk cannot.
			"cap larger than the utility disk",
			func(c *Config) { c.RegistryCache.MaxSizeGB = 40 }, // default disk_gb is 40
			"must be smaller than the utility host's disk_gb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cacheCfg(RegistryCacheSpec{Enabled: true})
			tt.mutate(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
