package ephemerd

import (
	"strings"
	"testing"
)

func TestRenderConfig(t *testing.T) {
	base := ConfigInputs{
		Owner:          "acme",
		AppID:          123,
		InstallationID: 456,
		PrivateKeyPath: "/etc/ephemerd/app.pem",
	}
	tests := []struct {
		name        string
		mutate      func(*ConfigInputs)
		wantErr     string
		contains    []string
		notContains []string
	}{
		{
			name:   "polling mode omits webhook block",
			mutate: func(in *ConfigInputs) {},
			contains: []string{
				"[github]", `owner = "acme"`, "app_id = 123", "installation_id = 456", "[log]",
			},
			notContains: []string{"[webhook]", "cloudflared"},
		},
		{
			name: "webhook mode emits full block",
			mutate: func(in *ConfigInputs) {
				in.TunnelToken = "tok"
				in.TunnelHostname = "ci.example.com"
				in.WebhookSecret = "shh"
			},
			contains: []string{
				"[webhook]",
				`tunnel = "cloudflared"`,
				`cloudflared_token = "tok"`,
				`cloudflared_hostname = "ci.example.com"`,
				`secret = "shh"`,
				"port = 8080",
				"pool = true",
			},
		},
		{
			name: "repos rendered",
			mutate: func(in *ConfigInputs) {
				in.Repos = []string{"repo1", "repo2"}
			},
			contains: []string{`repos = ["repo1", "repo2"]`},
		},
		{
			name: "token without hostname",
			mutate: func(in *ConfigInputs) {
				in.TunnelToken = "tok"
				in.WebhookSecret = "shh"
			},
			wantErr: "missing tunnel hostname",
		},
		{
			name: "token without secret",
			mutate: func(in *ConfigInputs) {
				in.TunnelToken = "tok"
				in.TunnelHostname = "ci.example.com"
			},
			wantErr: "missing webhook secret",
		},
		{
			name:    "missing owner",
			mutate:  func(in *ConfigInputs) { in.Owner = "" },
			wantErr: "missing owner",
		},
		{
			name:    "missing app credentials",
			mutate:  func(in *ConfigInputs) { in.AppID = 0 },
			wantErr: "missing github app credentials",
		},
		{
			name: "default images rendered in github block",
			mutate: func(in *ConfigInputs) {
				in.DefaultImageLinux = "ephpm/ephemerd:runner-ci-linux"
				in.DefaultImageWindows = "ephpm/ephemerd:runner-ci-windows"
			},
			contains: []string{
				`default_image_linux = "ephpm/ephemerd:runner-ci-linux"`,
				`default_image_windows = "ephpm/ephemerd:runner-ci-windows"`,
			},
		},
		{
			name:        "images omitted when unset",
			mutate:      func(in *ConfigInputs) {},
			notContains: []string{"default_image_linux", "default_image_windows"},
		},
		{
			name: "dind with privileged",
			mutate: func(in *ConfigInputs) {
				in.DindEnabled = true
				in.DindAllowPrivileged = true
			},
			contains: []string{"[dind]", "enabled = true", "allow_privileged = true"},
		},
		{
			name:        "dind off omits block and privilege",
			mutate:      func(in *ConfigInputs) { in.DindAllowPrivileged = true },
			notContains: []string{"[dind]", "allow_privileged"},
		},
		{
			name:     "module proxy block",
			mutate:   func(in *ConfigInputs) { in.ModuleProxyEnabled = true },
			contains: []string{"[module_proxy]", "enabled = true"},
		},
		{
			name: "vm.linux enabled=false renders even with no sizing",
			mutate: func(in *ConfigInputs) {
				no := false
				in.LinuxVM = VMResources{Enabled: &no}
			},
			contains: []string{"[vm.linux]", "enabled = false"},
		},
		{
			name: "vm.linux enabled omitted when nil",
			mutate: func(in *ConfigInputs) {
				in.LinuxVM = VMResources{MemoryMB: 5632}
			},
			contains:    []string{"[vm.linux]", "memory_mb = 5632"},
			notContains: []string{"enabled = false"},
		},
		{
			name: "job_timeout renders under [runner]",
			mutate: func(in *ConfigInputs) {
				in.MaxConcurrent = 4
				in.JobTimeout = "2h"
			},
			contains: []string{"[runner]", "max_concurrent = 4", `job_timeout = "2h"`},
		},
		{
			name:     "job_timeout alone still emits [runner]",
			mutate:   func(in *ConfigInputs) { in.JobTimeout = "1h30m" },
			contains: []string{"[runner]", `job_timeout = "1h30m"`},
		},
		{
			name:     "orphan_grace renders sweep sub-table",
			mutate:   func(in *ConfigInputs) { in.OrphanGrace = "90m" },
			contains: []string{"[runner.orphan_sweep]", `grace = "90m"`},
		},
		{
			name:        "timeouts omitted when unset",
			mutate:      func(in *ConfigInputs) {},
			notContains: []string{"job_timeout", "orphan_sweep", "grace ="},
		},
		{
			name: "cargo and composer proxy blocks",
			mutate: func(in *ConfigInputs) {
				in.CargoProxyEnabled = true
				in.ComposerProxyEnabled = true
			},
			contains: []string{"[cargo_proxy]", "[composer_proxy]", "enabled = true"},
		},
		{
			name:        "proxies omitted when unset",
			mutate:      func(in *ConfigInputs) {},
			notContains: []string{"[cargo_proxy]", "[composer_proxy]", "[module_proxy]"},
		},
		{
			name: "gateway proxies skipped on windows",
			mutate: func(in *ConfigInputs) {
				in.OS = "windows"
				in.ModuleProxyEnabled = true
				in.CargoProxyEnabled = true
				in.ComposerProxyEnabled = true
				in.DindEnabled = true
			},
			// dind still renders on windows; the gateway proxies do not.
			contains:    []string{"[dind]"},
			notContains: []string{"[module_proxy]", "[cargo_proxy]", "[composer_proxy]"},
		},
		{
			name: "gateway proxies render on linux",
			mutate: func(in *ConfigInputs) {
				in.OS = "linux"
				in.ModuleProxyEnabled = true
			},
			contains: []string{"[module_proxy]"},
		},
		{
			name: "github_extra target rendered",
			mutate: func(in *ConfigInputs) {
				in.ExtraTargets = []ExtraTarget{{
					Owner: "luthermonson", Repos: []string{"mayfly"}, Token: "ghp_x",
				}}
			},
			contains: []string{
				"[[github_extra]]",
				`owner = "luthermonson"`,
				`repos = ["mayfly"]`,
				`token = "ghp_x"`,
			},
		},
		{
			name: "github_extra missing token errors",
			mutate: func(in *ConfigInputs) {
				in.ExtraTargets = []ExtraTarget{{Owner: "luthermonson"}}
			},
			wantErr: "missing token",
		},
		{
			name: "github_extra missing owner errors",
			mutate: func(in *ConfigInputs) {
				in.ExtraTargets = []ExtraTarget{{Token: "ghp_x"}}
			},
			wantErr: "missing owner",
		},
		{
			name: "vm sections render both guest VMs",
			mutate: func(in *ConfigInputs) {
				in.MacOSVM = VMResources{CPUs: 4, MemoryMB: 5120}
				in.LinuxVM = VMResources{MemoryMB: 3072, DiskSizeGB: 60}
			},
			contains: []string{
				"[vm.macos]", "cpus = 4", "memory_mb = 5120",
				"[vm.linux]", "memory_mb = 3072", "disk_size_gb = 60",
			},
		},
		{
			name:   "vm section omits zero fields",
			mutate: func(in *ConfigInputs) { in.MacOSVM = VMResources{MemoryMB: 5120} },
			// only the set field appears; cpus/disk are left to ephemerd's default.
			contains:    []string{"[vm.macos]", "memory_mb = 5120"},
			notContains: []string{"cpus =", "disk_size_gb =", "[vm.linux]"},
		},
		{
			name:        "vm sections omitted when unset",
			mutate:      func(in *ConfigInputs) {},
			notContains: []string{"[vm.macos]", "[vm.linux]"},
		},
		{
			name: "windows runner container sizing renders",
			mutate: func(in *ConfigInputs) {
				in.WindowsRunnerMemoryMB = 16384
				in.WindowsRunnerCPUs = 8
			},
			contains: []string{"[runner.windows]", "memory_mb = 16384", "cpus = 8"},
		},
		{
			name:        "windows runner omitted when unset",
			mutate:      func(in *ConfigInputs) {},
			notContains: []string{"[runner.windows]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			tt.mutate(&in)
			out, err := RenderConfig(in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("RenderConfig() err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, bad := range tt.notContains {
				if strings.Contains(out, bad) {
					t.Errorf("output should not contain %q:\n%s", bad, out)
				}
			}
		})
	}
}

func TestRenderConfigWebhookOrdering(t *testing.T) {
	out, err := RenderConfig(ConfigInputs{
		Owner: "acme", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k.pem",
		TunnelToken: "tok", TunnelHostname: "h.example.com", WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := strings.Index(out, "[github]")
	wh := strings.Index(out, "[webhook]")
	lg := strings.Index(out, "[log]")
	if !(gh >= 0 && gh < wh && wh < lg) {
		t.Fatalf("unexpected section order (github=%d webhook=%d log=%d):\n%s", gh, wh, lg, out)
	}
}
