package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestPoolSpecResolved(t *testing.T) {
	d := Defaults{Cores: 4, MemoryMB: 8192, DiskGB: 60}
	tests := []struct {
		name string
		in   PoolSpec
		want PoolSpec
	}{
		{
			name: "all zero falls back to defaults",
			in:   PoolSpec{Hostname: "ci.example.com", Size: 2},
			want: PoolSpec{Hostname: "ci.example.com", Size: 2, Cores: 4, MemoryMB: 8192, DiskGB: 60},
		},
		{
			name: "explicit values preserved",
			in:   PoolSpec{Hostname: "ci.example.com", Size: 1, Cores: 8, MemoryMB: 16384, DiskGB: 100},
			want: PoolSpec{Hostname: "ci.example.com", Size: 1, Cores: 8, MemoryMB: 16384, DiskGB: 100},
		},
		{
			name: "partial fallback",
			in:   PoolSpec{Hostname: "ci.example.com", Cores: 2},
			want: PoolSpec{Hostname: "ci.example.com", Cores: 2, MemoryMB: 8192, DiskGB: 60},
		},
		{
			name: "repos untouched",
			in:   PoolSpec{Hostname: "ci.example.com", Repos: []string{"a", "b"}},
			want: PoolSpec{Hostname: "ci.example.com", Cores: 4, MemoryMB: 8192, DiskGB: 60, Repos: []string{"a", "b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.Resolved(d)
			if got.Cores != tt.want.Cores || got.MemoryMB != tt.want.MemoryMB || got.DiskGB != tt.want.DiskGB ||
				got.Hostname != tt.want.Hostname || got.Size != tt.want.Size || len(got.Repos) != len(tt.want.Repos) {
				t.Errorf("Resolved() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolvedCloudInitStorage(t *testing.T) {
	tests := []struct {
		name string
		pool PoolSpec
		px   Proxmox
		want string
	}{
		{
			name: "pool override wins over global",
			pool: PoolSpec{CloudInitStorage: "pool-iso"},
			px:   Proxmox{CloudInitStorage: "global-iso"},
			want: "pool-iso",
		},
		{
			name: "global used when pool empty",
			pool: PoolSpec{},
			px:   Proxmox{CloudInitStorage: "global-iso"},
			want: "global-iso",
		},
		{
			name: "empty when neither set (fall back to disk storage)",
			pool: PoolSpec{},
			px:   Proxmox{Storage: "tank"},
			want: "",
		},
		{
			name: "pool override wins even with no global",
			pool: PoolSpec{CloudInitStorage: "pool-iso"},
			px:   Proxmox{},
			want: "pool-iso",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pool.ResolvedCloudInitStorage(tt.px); got != tt.want {
				t.Errorf("ResolvedCloudInitStorage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCloudInitStorageParses(t *testing.T) {
	const src = `
[proxmox]
url = "https://pve:8006"
storage = "tank"
cloud_init_storage = "local"

[pools.linux]
hostname = "ci.example.com"
size = 1
cloud_init_storage = "local-dir"
`
	var c Config
	if _, err := toml.Decode(src, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Proxmox.CloudInitStorage != "local" {
		t.Errorf("proxmox.cloud_init_storage = %q, want %q", c.Proxmox.CloudInitStorage, "local")
	}
	if got := c.Pools["linux"].CloudInitStorage; got != "local-dir" {
		t.Errorf("pool cloud_init_storage = %q, want %q", got, "local-dir")
	}
	// Precedence end-to-end: pool override beats the global value.
	if got := c.Pools["linux"].ResolvedCloudInitStorage(c.Proxmox); got != "local-dir" {
		t.Errorf("ResolvedCloudInitStorage() = %q, want %q", got, "local-dir")
	}
}

func TestProxmoxEndpointsParse(t *testing.T) {
	const src = `
[proxmox]
url = "https://pve1:8006/api2/json"
endpoints = ["https://pve2:8006/api2/json", "https://pve3:8006/api2/json"]
endpoint_failure_threshold = 2
token_id = "mayfly@pve!mayfly"
secret = "x"
node = "pve1"
storage = "tank"
`
	var c Config
	if _, err := toml.Decode(src, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := len(c.Proxmox.Endpoints), 2; got != want {
		t.Fatalf("len(endpoints) = %d, want %d", got, want)
	}
	if got, want := c.Proxmox.Endpoints[0], "https://pve2:8006/api2/json"; got != want {
		t.Errorf("endpoints[0] = %q, want %q", got, want)
	}
	if got, want := c.Proxmox.Endpoints[1], "https://pve3:8006/api2/json"; got != want {
		t.Errorf("endpoints[1] = %q, want %q", got, want)
	}
	if got, want := c.Proxmox.EndpointFailureThreshold, 2; got != want {
		t.Errorf("endpoint_failure_threshold = %d, want %d", got, want)
	}
}

func TestProxmoxEndpointsDefaultEmpty(t *testing.T) {
	// Omitting the new keys leaves the single-endpoint behavior untouched.
	const src = `
[proxmox]
url = "https://pve1:8006/api2/json"
token_id = "mayfly@pve!mayfly"
secret = "x"
node = "pve1"
storage = "tank"
`
	var c Config
	if _, err := toml.Decode(src, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(c.Proxmox.Endpoints) != 0 {
		t.Errorf("endpoints = %v, want empty", c.Proxmox.Endpoints)
	}
	if c.Proxmox.EndpointFailureThreshold != 0 {
		t.Errorf("endpoint_failure_threshold = %d, want 0", c.Proxmox.EndpointFailureThreshold)
	}
}

func TestVLANAndFirewallParse(t *testing.T) {
	const src = `
[proxmox]
url = "https://pve:8006"
storage = "tank"

[pools.isolated]
hostname = "ci.example.com"
size = 1
vlan_tag = 42
firewall = true

[pools.flat]
hostname = "ci-flat.example.com"
size = 1
allow_lan = true
`
	var c Config
	if _, err := toml.Decode(src, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := c.Pools["isolated"].VLANTag; got != 42 {
		t.Errorf("vlan_tag = %d, want 42", got)
	}
	if !c.Pools["isolated"].Firewall {
		t.Errorf("firewall = false, want true")
	}
	// A pool that says nothing stays untagged with the firewall off; allow_lan
	// is the explicit opt-out that lets such a VM pool onto the flat LAN.
	flat := c.Pools["flat"]
	if flat.VLANTag != 0 || flat.Firewall {
		t.Errorf("unset pool = {vlan_tag:%d firewall:%v}, want {0 false}", flat.VLANTag, flat.Firewall)
	}
	if !flat.AllowLAN {
		t.Errorf("allow_lan = false, want true")
	}
	if got := flat.Resolved(Defaults{Cores: 4, MemoryMB: 8192, DiskGB: 60}); got.VLANTag != 0 || got.Firewall {
		t.Errorf("Resolved() must not invent a vlan/firewall, got {%d %v}", got.VLANTag, got.Firewall)
	}
}

func TestPoolSpecResolvedEnroll(t *testing.T) {
	d := Defaults{Cores: 4, MemoryMB: 8192, DiskGB: 60}

	t.Run("linux with members sizes from list and defaults ssh_user", func(t *testing.T) {
		in := PoolSpec{Hostname: "ci.example.com", Members: []string{"a.lan", "b.lan", "c.lan"}}
		got := in.Resolved(d)
		if !got.UsesEnroll() {
			t.Fatalf("linux+members should use enroll path")
		}
		if got.Size != 3 {
			t.Errorf("Size = %d, want 3 (len(members))", got.Size)
		}
		if got.SSHUser != "root" {
			t.Errorf("SSHUser = %q, want default \"root\"", got.SSHUser)
		}
		if got.OS != "linux" || got.Arch != "amd64" {
			t.Errorf("os/arch = %s/%s, want linux/amd64", got.OS, got.Arch)
		}
		if got.Template != "" {
			t.Errorf("enrolled linux pool must get no template, got %q", got.Template)
		}
	})

	t.Run("linux arm64 with members keeps arch and skips template", func(t *testing.T) {
		in := PoolSpec{Hostname: "ci.example.com", Arch: "arm64", Members: []string{"arm1.lan"}}
		got := in.Resolved(d)
		if got.Arch != "arm64" || got.Size != 1 || got.Template != "" {
			t.Errorf("arm64 enroll resolved wrong: arch=%s size=%d template=%q", got.Arch, got.Size, got.Template)
		}
	})

	t.Run("linux without members keeps VM lifecycle (template + explicit size)", func(t *testing.T) {
		in := PoolSpec{Hostname: "ci.example.com", Size: 2}
		got := in.Resolved(d)
		if got.UsesEnroll() {
			t.Fatalf("linux without members must NOT use enroll path")
		}
		if got.Size != 2 {
			t.Errorf("Size = %d, want 2 (unchanged)", got.Size)
		}
		if got.Template != "mayfly-ubuntu-24.04-amd64" {
			t.Errorf("VM linux pool must get a template, got %q", got.Template)
		}
	})

	t.Run("windows never enrolls", func(t *testing.T) {
		in := PoolSpec{Hostname: "ci.example.com", OS: "windows", Template: "t"}
		if in.Resolved(d).UsesEnroll() {
			t.Fatalf("windows pool must never use enroll path")
		}
	})
}

func validBase() Config {
	return Config{
		Proxmox: Proxmox{
			URL: "https://pve:8006", TokenID: "t", Secret: "s", Node: "pve1", Storage: "local-lvm",
		},
		Cloudflare: Cloudflare{APIToken: "t", AccountID: "a", ZoneID: "z", BaseDomain: "example.com"},
		GitHub:     GitHub{Owner: "o", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k.pem"},
	}
}

func TestValidatePools(t *testing.T) {
	tests := []struct {
		name    string
		pools   map[string]PoolSpec
		wantErr string
	}{
		{
			name:  "no pools is valid",
			pools: nil,
		},
		{
			name:  "empty pools is valid",
			pools: map[string]PoolSpec{},
		},
		{
			name: "valid pool",
			pools: map[string]PoolSpec{
				"linux-amd64": {Hostname: "ci-linux-amd64.example.com", Size: 3, VLANTag: 10},
			},
		},
		{
			name: "size zero is valid",
			pools: map[string]PoolSpec{
				"idle": {Hostname: "idle.example.com", Size: 0, VLANTag: 10},
			},
		},
		{
			name: "missing hostname",
			pools: map[string]PoolSpec{
				"linux": {Size: 1},
			},
			wantErr: "missing hostname",
		},
		{
			name: "duplicate hostname",
			pools: map[string]PoolSpec{
				"a": {Hostname: "ci.example.com", Size: 1},
				"b": {Hostname: "ci.example.com", Size: 1},
			},
			wantErr: "already used by pool",
		},
		{
			name: "negative size",
			pools: map[string]PoolSpec{
				"linux": {Hostname: "ci.example.com", Size: -1},
			},
			wantErr: "size must be >= 0",
		},
		{
			name: "windows without template",
			pools: map[string]PoolSpec{
				"win": {Hostname: "ci-win.example.com", OS: "windows"},
			},
			wantErr: "windows pools require an explicit template",
		},
		{
			name: "windows arm64 rejected",
			pools: map[string]PoolSpec{
				"win": {Hostname: "ci-win.example.com", OS: "windows", Arch: "arm64", Template: "t"},
			},
			wantErr: "windows/arm64 is not supported",
		},
		{
			name: "darwin without members",
			pools: map[string]PoolSpec{
				"mac": {Hostname: "ci-mac.example.com", OS: "darwin"},
			},
			wantErr: "darwin pools require members",
		},
		{
			name: "darwin amd64 rejected",
			pools: map[string]PoolSpec{
				"mac": {Hostname: "ci-mac.example.com", OS: "darwin", Arch: "amd64", Members: []string{"mini1.lan"}},
			},
			wantErr: "darwin/amd64 is not supported",
		},
		{
			name: "darwin with template rejected",
			pools: map[string]PoolSpec{
				"mac": {Hostname: "ci-mac.example.com", OS: "darwin", Template: "t", Members: []string{"mini1.lan"}},
			},
			wantErr: "do not apply to darwin",
		},
		{
			name: "members on linux accepted (bare-metal enrollment)",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Members: []string{"host1"}},
			},
		},
		{
			name: "valid linux arm64 enroll pool with ssh_* settings",
			pools: map[string]PoolSpec{
				"arm": {Hostname: "ci-arm.example.com", Arch: "arm64", Members: []string{"arm1.lan", "arm2.lan"}, SSHUser: "luther", SSHKey: "/k"},
			},
		},
		{
			name: "members on windows rejected",
			pools: map[string]PoolSpec{
				"win": {Hostname: "ci-win.example.com", OS: "windows", Template: "t", Members: []string{"host1"}},
			},
			wantErr: "members is only valid for darwin or linux",
		},
		{
			name: "ssh_* on windows rejected",
			pools: map[string]PoolSpec{
				"win": {Hostname: "ci-win.example.com", OS: "windows", Template: "t", SSHUser: "luther"},
			},
			wantErr: "ssh_* settings are only valid for darwin or linux",
		},
		{
			name: "ssh_* without members rejected",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", SSHUser: "luther"},
			},
			wantErr: `ssh_* settings require members`,
		},
		{
			name: "linux enroll pool with template rejected",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Members: []string{"host1"}, Template: "custom"},
			},
			wantErr: "template/node/storage do not apply to enrolled",
		},
		{
			name: "linux enroll pool with node rejected",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Members: []string{"host1"}, Node: "pve-arm"},
			},
			wantErr: "template/node/storage do not apply to enrolled",
		},
		{
			name: "linux enroll pool with mismatched size rejected",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Members: []string{"host1", "host2"}, Size: 5},
			},
			wantErr: "member list is the size",
		},
		{
			name: "valid darwin pool",
			pools: map[string]PoolSpec{
				"mac": {Hostname: "ci-mac.example.com", OS: "darwin", Members: []string{"mini1.lan", "mini2.lan"}, SSHUser: "luther"},
			},
		},
		{
			name: "bad os",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", OS: "beos"},
			},
			wantErr: `os must be "linux", "windows", or "darwin"`,
		},
		{
			name: "bad arch",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Arch: "riscv"},
			},
			wantErr: `arch must be "amd64" or "arm64"`,
		},
		{
			name: "valid windows pool",
			pools: map[string]PoolSpec{
				"win": {Hostname: "ci-win.example.com", OS: "windows", Template: "mayfly-winserver-2025", VLANTag: 10},
			},
		},
		{
			name: "valid linux arm64 pool",
			pools: map[string]PoolSpec{
				"arm": {Hostname: "ci-arm.example.com", Arch: "arm64", Node: "pve-arm", VLANTag: 10},
			},
		},
		{
			name: "valid vlan_tag with firewall",
			pools: map[string]PoolSpec{
				"linux": {Hostname: "ci.example.com", Size: 1, VLANTag: 42, Firewall: true},
			},
		},
		{
			name: "vlan_tag at the top of the range",
			pools: map[string]PoolSpec{
				"linux": {Hostname: "ci.example.com", Size: 1, VLANTag: 4094},
			},
		},
		{
			// Isolation-required guard: a Proxmox VM pool must not silently land
			// its runners on the trusted LAN. Untagged + no opt-out is refused.
			name: "untagged VM pool without allow_lan rejected",
			pools: map[string]PoolSpec{
				"linux": {Hostname: "ci.example.com", Size: 1},
			},
			wantErr: "provisions Proxmox VMs but sets no vlan_tag",
		},
		{
			name: "untagged VM pool with allow_lan is valid",
			pools: map[string]PoolSpec{
				"linux": {Hostname: "ci.example.com", Size: 1, AllowLAN: true},
			},
		},
		{
			name: "untagged windows pool without allow_lan rejected",
			pools: map[string]PoolSpec{
				"win": {Hostname: "ci-win.example.com", OS: "windows", Template: "t"},
			},
			wantErr: "provisions Proxmox VMs but sets no vlan_tag",
		},
		{
			name: "untagged windows pool with allow_lan is valid",
			pools: map[string]PoolSpec{
				"win": {Hostname: "ci-win.example.com", OS: "windows", Template: "t", AllowLAN: true},
			},
		},
		{
			// Enrolled bare metal has no mayfly-controlled vNIC, so the guard must
			// not fire on it even though it carries no vlan_tag.
			name: "enrolled linux pool without vlan_tag is exempt",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Members: []string{"host1"}},
			},
		},
		{
			name: "darwin pool without vlan_tag is exempt",
			pools: map[string]PoolSpec{
				"mac": {Hostname: "ci-mac.example.com", OS: "darwin", Members: []string{"mini1.lan"}},
			},
		},
		{
			name: "allow_lan on an enrolled pool rejected",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Members: []string{"host1"}, AllowLAN: true},
			},
			wantErr: "allow_lan does not apply to enrolled",
		},
		{
			name: "allow_lan on a darwin pool rejected",
			pools: map[string]PoolSpec{
				"mac": {Hostname: "ci-mac.example.com", OS: "darwin", Members: []string{"mini1.lan"}, AllowLAN: true},
			},
			wantErr: "allow_lan does not apply to enrolled",
		},
		{
			name: "vlan_tag above the range rejected",
			pools: map[string]PoolSpec{
				"linux": {Hostname: "ci.example.com", Size: 1, VLANTag: 4095},
			},
			wantErr: "vlan_tag must be between 1 and 4094",
		},
		{
			name: "negative vlan_tag rejected",
			pools: map[string]PoolSpec{
				"linux": {Hostname: "ci.example.com", Size: 1, VLANTag: -1},
			},
			wantErr: "vlan_tag must be between 1 and 4094",
		},
		{
			name: "vlan_tag on an enrolled pool rejected",
			pools: map[string]PoolSpec{
				"p": {Hostname: "ci.example.com", Members: []string{"host1"}, VLANTag: 42},
			},
			wantErr: "vlan_tag/firewall do not apply to enrolled",
		},
		{
			name: "firewall on a darwin pool rejected",
			pools: map[string]PoolSpec{
				"mac": {Hostname: "ci-mac.example.com", OS: "darwin", Members: []string{"mini1.lan"}, Firewall: true},
			},
			wantErr: "vlan_tag/firewall do not apply to enrolled",
		},
		{
			name: "uppercase pool name",
			pools: map[string]PoolSpec{
				"Linux": {Hostname: "ci.example.com"},
			},
			wantErr: "lowercase alphanumeric",
		},
		{
			name: "underscore in pool name",
			pools: map[string]PoolSpec{
				"linux_amd64": {Hostname: "ci.example.com"},
			},
			wantErr: "lowercase alphanumeric",
		},
		{
			name: "leading hyphen in pool name",
			pools: map[string]PoolSpec{
				"-linux": {Hostname: "ci.example.com"},
			},
			wantErr: "lowercase alphanumeric",
		},
		{
			name: "trailing hyphen in pool name",
			pools: map[string]PoolSpec{
				"linux-": {Hostname: "ci.example.com"},
			},
			wantErr: "lowercase alphanumeric",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Pools = tt.pools
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestScopedValidation covers the single-node loader's relaxed validation:
// LoadForNode (skipCredentials) must not require GitHub App / pool-token
// credentials, while the full Validate() still enforces them.
func TestScopedValidation(t *testing.T) {
	tests := []struct {
		name string
		// mutate strips a credential from an otherwise-valid config.
		mutate func(*Config)
		// wantErr is the substring the FULL Validate() must report.
		wantErr string
	}{
		{
			name:    "missing github.app_id",
			mutate:  func(c *Config) { c.GitHub.AppID = 0 },
			wantErr: "missing github.app_id",
		},
		{
			name:    "missing github.installation_id",
			mutate:  func(c *Config) { c.GitHub.InstallationID = 0 },
			wantErr: "missing github.installation_id",
		},
		{
			name:    "missing github.private_key_path",
			mutate:  func(c *Config) { c.GitHub.PrivateKeyPath = "" },
			wantErr: "missing github.private_key_path",
		},
		{
			name:    "missing github.owner",
			mutate:  func(c *Config) { c.GitHub.Owner = "" },
			wantErr: "missing github.owner",
		},
		{
			name: "missing pool github_extra token",
			mutate: func(c *Config) {
				c.Pools = map[string]PoolSpec{
					"linux": {
						Hostname: "ci.example.com", Size: 1, VLANTag: 10,
						GitHubExtra: []GitHubTarget{{Owner: "other"}},
					},
				}
			},
			wantErr: "missing token (set token or token_env)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Full validation must still reject the missing credential.
			full := validBase()
			tt.mutate(&full)
			if err := full.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
			// Scoped validation (single-node) must accept it.
			scoped := validBase()
			tt.mutate(&scoped)
			if err := scoped.validate(loadOptions{skipCredentials: true}); err != nil {
				t.Fatalf("validate(skipCredentials) = %v, want nil", err)
			}
		})
	}

	// A structural error (not a credential) must still fail even under the
	// scoped loader — skipCredentials only relaxes credential checks.
	t.Run("scoped still validates structure", func(t *testing.T) {
		c := validBase()
		c.Pools = map[string]PoolSpec{"Bad_Name": {Hostname: "ci.example.com"}}
		if err := c.validate(loadOptions{skipCredentials: true}); err == nil ||
			!strings.Contains(err.Error(), "lowercase alphanumeric") {
			t.Fatalf("validate(skipCredentials) = %v, want structural error", err)
		}
	})
}
