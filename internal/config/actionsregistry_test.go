package config

import (
	"strings"
	"testing"
)

// actionsCfg is a config with a dedicated, VLAN-tagged cache VM big enough for
// both registries, running the Actions registry with an inline password.
func actionsCfg() *Config {
	c := cacheCfg(RegistryCacheSpec{
		Enabled:   true,
		MaxSizeGB: 20,
		VM:        &RegistryCacheVMSpec{Template: "tmpl", VLANTag: 2, DiskGB: 200},
		Actions:   &ActionsRegistrySpec{Enabled: true, Password: "s3cret"},
	})
	c.Pools = map[string]PoolSpec{"p": {Hostname: "ci.example.com", Size: 1, VLANTag: 2}}
	return c
}

func TestActionsRegistryResolvedDefaults(t *testing.T) {
	a := ActionsRegistrySpec{Enabled: true}.Resolved()
	if a.Port != 5001 || a.MaxSizeGB != 150 || a.Username != "ephemerd" {
		t.Fatalf("defaults = port %d, cap %d, user %q; want 5001, 150, ephemerd", a.Port, a.MaxSizeGB, a.Username)
	}
}

// password_env wins over the inline value, the way github_extra's token_env does.
func TestActionsRegistryPasswordEnvWins(t *testing.T) {
	t.Setenv("MAYFLY_TEST_ACTIONS_PW", "from-env")
	a := ActionsRegistrySpec{PasswordEnv: "MAYFLY_TEST_ACTIONS_PW", Password: "inline"}
	if got := a.ResolvedPassword(); got != "from-env" {
		t.Fatalf("ResolvedPassword() = %q, want from-env", got)
	}
	a.PasswordEnv = "MAYFLY_TEST_ACTIONS_PW_UNSET"
	if got := a.ResolvedPassword(); got != "inline" {
		t.Fatalf("ResolvedPassword() with an unset env = %q, want the inline fallback", got)
	}
}

func TestActionsRegistryValidates(t *testing.T) {
	if err := actionsCfg().Validate(); err != nil {
		t.Fatalf("a dedicated cache VM with the Actions registry should validate: %v", err)
	}
}

func TestActionsRegistryEndpoint(t *testing.T) {
	r := actionsCfg().RegistryCache
	if got := r.ActionsEndpoint("192.0.2.224"); got != "http://192.0.2.224:5001" {
		t.Fatalf("ActionsEndpoint = %q", got)
	}
	if got := r.ActionsEndpoint(""); got != "" {
		t.Fatalf("ActionsEndpoint with no address = %q, want empty (never guess)", got)
	}
	r.Actions.Enabled = false
	if got := r.ActionsEndpoint("192.0.2.224"); got != "" {
		t.Fatalf("ActionsEndpoint while disabled = %q, want empty", got)
	}
}

func TestActionsRegistryValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			"needs a dedicated VM",
			func(c *Config) { c.RegistryCache.VM = nil },
			"registry_cache.actions: needs a [registry_cache.vm]",
		},
		{
			// An anonymous writable registry on the runner VLAN would let any
			// job plant cache entries for every other repo.
			"password required",
			func(c *Config) { c.RegistryCache.Actions.Password = "" },
			"set password_env (preferred) or password",
		},
		{
			"named env var is empty",
			func(c *Config) {
				c.RegistryCache.Actions.Password = ""
				c.RegistryCache.Actions.PasswordEnv = "MAYFLY_TEST_ACTIONS_PW_NEVER_SET"
			},
			"password_env MAYFLY_TEST_ACTIONS_PW_NEVER_SET is empty",
		},
		{
			"port clashes with the pull-through cache",
			func(c *Config) { c.RegistryCache.Actions.Port = 5000 },
			"is the pull-through cache's port",
		},
		{
			"username breaks htpasswd",
			func(c *Config) { c.RegistryCache.Actions.Username = "a:b" },
			"must not contain ':'",
		},
		{
			// Both blob stores share the cache VM's one disk.
			"both caps must fit the disk",
			func(c *Config) { c.RegistryCache.VM.DiskGB = 150 }, // 20 + 150 + 10 > 150
			"max_size_gb + actions.max_size_gb (170) plus 10GB for the OS must fit the cache VM's disk_gb (150)",
		},
		{
			"pool opt-in needs the registry",
			func(c *Config) {
				c.RegistryCache.Actions.Enabled = false
				c.Pools = map[string]PoolSpec{"p": {Hostname: "ci.example.com", Size: 1, ActionsCache: true}}
			},
			"actions_cache = true needs [registry_cache.actions]",
		},
		{
			// ephemerd's Actions cache is Linux-only.
			"windows pool refused",
			func(c *Config) {
				c.Pools = map[string]PoolSpec{"w": {Hostname: "ci-win.example.com", Size: 1, OS: "windows", Template: "win", ActionsCache: true}}
			},
			"actions_cache is Linux-only",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := actionsCfg()
			tt.mutate(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// A linux pool opting in is accepted.
func TestActionsCachePoolOptIn(t *testing.T) {
	c := actionsCfg()
	c.Pools = map[string]PoolSpec{"p": {Hostname: "ci.example.com", Size: 1, VLANTag: 2, ActionsCache: true}}
	if err := c.Validate(); err != nil {
		t.Fatalf("linux pool with actions_cache should validate: %v", err)
	}
}
