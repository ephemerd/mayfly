package cloudinit

import (
	"encoding/base64"
	"strings"
	"testing"
)

func validInputs() Inputs {
	return Inputs{
		Hostname:          "mfl-linux-amd64-101",
		GitHubAppPEM:      []byte("-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n"),
		EphemerdConfig:    "[github]\nowner = \"acme\"\n\n[webhook]\ntunnel = \"cloudflared\"\n",
		EphemerdBinaryURL: "https://example.com/ephemerd.tar.gz",
	}
}

func TestRender(t *testing.T) {
	r, err := Render(validInputs())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no cloudflared artifacts in user-data", func(t *testing.T) {
		stripped := strings.ReplaceAll(r.UserData, "      [webhook]\n      tunnel = \"cloudflared\"", "")
		if strings.Contains(strings.ToLower(stripped), "cloudflared") {
			t.Errorf("user-data still references cloudflared outside ephemerd config:\n%s", r.UserData)
		}
		for _, bad := range []string{"/etc/cloudflared", "cloudflared.service", "creds.json"} {
			if strings.Contains(r.UserData, bad) {
				t.Errorf("user-data contains %q", bad)
			}
		}
	})

	t.Run("config.toml indented under write_files", func(t *testing.T) {
		for _, want := range []string{
			"  - path: /var/lib/ephemerd/config.toml",
			"      [github]",
			"      owner = \"acme\"",
			"      [webhook]",
		} {
			if !strings.Contains(r.UserData, want) {
				t.Errorf("user-data missing line %q:\n%s", want, r.UserData)
			}
		}
	})

	t.Run("kept sections", func(t *testing.T) {
		for _, want := range []string{
			"hostname: mfl-linux-amd64-101",
			"qemu-guest-agent",
			"name: ephemerd",
			"/etc/ephemerd/app.pem",
			"https://example.com/ephemerd.tar.gz",
			"/usr/local/bin/ephemerd install",
		} {
			if !strings.Contains(r.UserData, want) {
				t.Errorf("user-data missing %q", want)
			}
		}
	})

	t.Run("meta-data", func(t *testing.T) {
		for _, want := range []string{
			"instance-id: mfl-linux-amd64-101",
			"local-hostname: mfl-linux-amd64-101",
		} {
			if !strings.Contains(r.MetaData, want) {
				t.Errorf("meta-data missing %q:\n%s", want, r.MetaData)
			}
		}
	})
}

func TestRenderNoCloudflaredInPollingMode(t *testing.T) {
	in := validInputs()
	in.EphemerdConfig = "[github]\nowner = \"acme\"\n"
	r, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(r.UserData), "cloudflared") {
		t.Errorf("user-data references cloudflared:\n%s", r.UserData)
	}
}

func TestRenderLinuxOSExplicit(t *testing.T) {
	implicit, err := Render(validInputs())
	if err != nil {
		t.Fatal(err)
	}
	in := validInputs()
	in.OS = "linux"
	explicit, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if implicit.UserData != explicit.UserData || implicit.MetaData != explicit.MetaData {
		t.Error("OS \"\" and OS \"linux\" render differently")
	}
}

func TestRenderWindows(t *testing.T) {
	in := validInputs()
	in.OS = "windows"
	in.Hostname = "mfl-windows-amd64-101"
	in.EphemerdBinaryURL = "https://example.com/ephemerd.zip"
	r, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("first line", func(t *testing.T) {
		if first := strings.SplitN(r.UserData, "\n", 2)[0]; strings.TrimRight(first, "\r") != "#ps1_sysnative" {
			t.Errorf("first line = %q, want #ps1_sysnative", first)
		}
	})

	t.Run("contains", func(t *testing.T) {
		for _, want := range []string{
			base64.StdEncoding.EncodeToString([]byte(in.EphemerdConfig)),
			base64.StdEncoding.EncodeToString(in.GitHubAppPEM),
			"https://example.com/ephemerd.zip",
			`ephemerd.exe" install`,
			`C:\ProgramData\ephemerd`,
			"bootstrap-error.log",
		} {
			if !strings.Contains(r.UserData, want) {
				t.Errorf("windows user-data missing %q:\n%s", want, r.UserData)
			}
		}
	})

	// The self-healing first-boot reconcile must be present: the Hyper-V +
	// Containers roles, the C: extend (recovery-partition delete + resize), the
	// AtStartup watchdog, and delayed-auto start so a rebooted node comes back.
	t.Run("self-healing reconcile", func(t *testing.T) {
		for _, want := range []string{
			"Enable-WindowsOptionalFeature",
			"Microsoft-Hyper-V",
			"Containers",
			"Resize-Partition",
			"Remove-Partition",
			`winsetup.ps1`,
			"Register-ScheduledTask",
			"mayfly-winsetup",
			"start= delayed-auto",
			"Restart-Computer -Force",
		} {
			if !strings.Contains(r.UserData, want) {
				t.Errorf("windows user-data missing reconcile piece %q:\n%s", want, r.UserData)
			}
		}
	})

	t.Run("no linux bleed-through", func(t *testing.T) {
		for _, bad := range []string{"runcmd:", "apt", "systemctl"} {
			if strings.Contains(r.UserData, bad) {
				t.Errorf("windows user-data contains %q", bad)
			}
		}
	})

	t.Run("meta-data", func(t *testing.T) {
		for _, want := range []string{
			"instance-id: mfl-windows-amd64-101",
			"local-hostname: mfl-windows-amd64-101",
		} {
			if !strings.Contains(r.MetaData, want) {
				t.Errorf("meta-data missing %q:\n%s", want, r.MetaData)
			}
		}
	})
}

func monitoringInputs() MonitoringInputs {
	return MonitoringInputs{
		Hostname: "mfl-monitoring-103",
		Dirs:     []string{"/opt/monitoring"},
		Files: []WriteFile{{
			Path:          "/opt/monitoring/docker-compose.yml",
			Mode:          "0644",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte("services: {}\n")),
		}},
		ComposeDirs: []string{"/opt/monitoring"},
	}
}

// A monitoring-only utility host brings up exactly one compose project and
// enables no extra units.
func TestRenderMonitoringOnly(t *testing.T) {
	r, err := RenderMonitoring(monitoringInputs())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"hostname: mfl-monitoring-103",
		"install -d -m 0755 /opt/monitoring",
		"cd /opt/monitoring && docker compose up -d",
	} {
		if !strings.Contains(r.UserData, want) {
			t.Errorf("user-data missing %q:\n%s", want, r.UserData)
		}
	}
	if strings.Contains(r.UserData, "systemctl daemon-reload") {
		t.Errorf("daemon-reload emitted with no units:\n%s", r.UserData)
	}
}

// Adding the registry cache adds a second compose project and a timer, without
// disturbing the monitoring project.
func TestRenderMonitoringWithExtraService(t *testing.T) {
	in := monitoringInputs()
	in.Dirs = append(in.Dirs, "/opt/registry-cache", "/var/lib/registry-cache")
	in.ComposeDirs = append(in.ComposeDirs, "/opt/registry-cache")
	in.Units = []string{"registry-cache-prune.timer"}

	r, err := RenderMonitoring(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"install -d -m 0755 /var/lib/registry-cache",
		"cd /opt/monitoring && docker compose up -d",
		"cd /opt/registry-cache && docker compose up -d",
		"systemctl daemon-reload",
		"systemctl enable --now registry-cache-prune.timer",
	} {
		if !strings.Contains(r.UserData, want) {
			t.Errorf("user-data missing %q:\n%s", want, r.UserData)
		}
	}
}

func TestRenderMonitoringValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*MonitoringInputs)
		wantErr string
	}{
		{"missing hostname", func(i *MonitoringInputs) { i.Hostname = "" }, "missing hostname"},
		{"missing files", func(i *MonitoringInputs) { i.Files = nil }, "missing stack files"},
		{"missing compose dir", func(i *MonitoringInputs) { i.ComposeDirs = nil }, "missing compose dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := monitoringInputs()
			tt.mutate(&in)
			_, err := RenderMonitoring(in)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RenderMonitoring() err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRenderValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Inputs)
		wantErr string
	}{
		{"missing hostname", func(i *Inputs) { i.Hostname = "" }, "missing hostname"},
		{"missing ephemerd config", func(i *Inputs) { i.EphemerdConfig = "" }, "missing ephemerd_config"},
		{"missing binary url", func(i *Inputs) { i.EphemerdBinaryURL = "" }, "missing ephemerd_binary_url"},
		{"missing pem", func(i *Inputs) { i.GitHubAppPEM = nil }, "missing github_app_pem"},
		{"bad os", func(i *Inputs) { i.OS = "plan9" }, `unsupported os "plan9"`},
		{"windows missing hostname", func(i *Inputs) { i.OS = "windows"; i.Hostname = "" }, "missing hostname"},
		{"windows missing pem", func(i *Inputs) { i.OS = "windows"; i.GitHubAppPEM = nil }, "missing github_app_pem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInputs()
			tt.mutate(&in)
			_, err := Render(in)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Render() err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
