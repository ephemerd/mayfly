package ephemerd

import (
	"strings"
	"testing"
)

func baseInputs() ConfigInputs {
	return ConfigInputs{
		Owner:          "ephpm",
		AppID:          1,
		InstallationID: 2,
		PrivateKeyPath: "/etc/ephemerd/app.pem",
	}
}

func TestRenderConfigMetricsDisabledByDefault(t *testing.T) {
	out, err := RenderConfig(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[metrics]") {
		t.Errorf("metrics section emitted when disabled:\n%s", out)
	}
}

func TestRenderConfigMetricsEnabled(t *testing.T) {
	in := baseInputs()
	in.MetricsEnabled = true
	in.MetricsPort = 9099
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[metrics]") || !strings.Contains(out, "enabled = true") {
		t.Errorf("metrics section missing:\n%s", out)
	}
	if !strings.Contains(out, "port = 9099") {
		t.Errorf("metrics port not honored:\n%s", out)
	}
}

func TestRenderConfigMetricsDefaultPort(t *testing.T) {
	in := baseInputs()
	in.MetricsEnabled = true // port left 0
	out, err := RenderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "port = 9090") {
		t.Errorf("expected default metrics port 9090:\n%s", out)
	}
}

// TestRenderConfigMetricsBindAddr covers the field that decides whether the
// monitoring node can scrape anything at all.
//
// ephemerd binds /metrics to 127.0.0.1 unless told otherwise, so omitting
// bind_addr yields a daemon that answers only itself: Prometheus gets
// "connection refused" and every runner target shows DOWN, while each node
// reports healthy. The fleet ran that way for weeks.
func TestRenderConfigMetricsBindAddr(t *testing.T) {
	base := baseInputs()
	base.MetricsEnabled = true
	base.MetricsPort = 9090

	t.Run("rendered when set", func(t *testing.T) {
		in := base
		in.MetricsBindAddr = "0.0.0.0"
		out, err := RenderConfig(in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `bind_addr = "0.0.0.0"`) {
			t.Errorf("bind_addr missing from rendered config:\n%s", out)
		}
	})

	// The anti-drift half: a fleet that has not opted in must render exactly
	// what it rendered before this field existed, or every node shows config
	// drift on the next plan for no reason.
	t.Run("absent when empty", func(t *testing.T) {
		out, err := RenderConfig(base)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "bind_addr") {
			t.Errorf("bind_addr rendered despite being unset:\n%s", out)
		}
		if !strings.Contains(out, "[metrics]") || !strings.Contains(out, "port = 9090") {
			t.Errorf("the rest of [metrics] should be unchanged:\n%s", out)
		}
	})

	// Nothing metrics-related may leak out when metrics are off entirely.
	t.Run("nothing when metrics disabled", func(t *testing.T) {
		in := base
		in.MetricsEnabled = false
		in.MetricsBindAddr = "0.0.0.0"
		out, err := RenderConfig(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "bind_addr") || strings.Contains(out, "[metrics]") {
			t.Errorf("metrics config rendered while disabled:\n%s", out)
		}
	})
}
