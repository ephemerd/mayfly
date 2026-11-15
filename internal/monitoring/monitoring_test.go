package monitoring

import (
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/state"
)

func TestStackFilesMapsGuestPaths(t *testing.T) {
	files, err := StackFiles()
	if err != nil {
		t.Fatalf("StackFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no stack files returned")
	}
	want := map[string]bool{
		"/opt/monitoring/docker-compose.yml":                          false,
		"/etc/prometheus/prometheus.yml":                              false,
		"/etc/grafana/provisioning/datasources/prometheus.yml":        false,
		"/etc/grafana/provisioning/dashboards/dashboards.yml":         false,
		"/var/lib/grafana-dashboards/ephemerd-fleet.json":             false,
	}
	for _, f := range files {
		if _, ok := want[f.Path]; ok {
			want[f.Path] = true
		}
		if strings.Contains(f.Path, "README") {
			t.Errorf("README.md should not be shipped to the guest: %s", f.Path)
		}
		if len(f.Content) == 0 {
			t.Errorf("empty content for %s", f.Path)
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("expected stack file mapped to %s, not found", path)
		}
	}
}

func TestAssetsHashStable(t *testing.T) {
	if AssetsHash() != AssetsHash() {
		t.Fatal("AssetsHash not deterministic")
	}
	if strings.HasPrefix(AssetsHash(), "invalid:") {
		t.Fatalf("AssetsHash errored: %s", AssetsHash())
	}
}

func sampleStore() *state.Store {
	return &state.Store{
		Pools: []state.Pool{
			{
				Name: "linux-amd64",
				Members: []state.Member{
					{VMID: 101, Hostname: "mfl-linux-amd64-101", IP: "192.0.2.20", OS: "linux", Arch: "amd64"},
					{VMID: 102, Hostname: "mfl-linux-amd64-102", IP: "", OS: "linux", Arch: "amd64"}, // no IP yet
				},
			},
			{
				Name: "mac-arm64",
				Members: []state.Member{
					{Hostname: "192.0.2.78", IP: "192.0.2.78", OS: "darwin", Arch: "arm64"},
				},
			},
		},
	}
}

func TestBuildTargetsSkipsMembersWithoutIP(t *testing.T) {
	targets := BuildTargets(sampleStore(), 9090)
	if len(targets) != 2 {
		t.Fatalf("want 2 targets (member without IP skipped), got %d", len(targets))
	}
	// sorted by address
	if targets[0].Targets[0] != "192.0.2.20:9090" {
		t.Errorf("unexpected first target: %v", targets[0].Targets)
	}
	if targets[0].Labels["pool"] != "linux-amd64" || targets[0].Labels["arch"] != "amd64" {
		t.Errorf("unexpected labels: %v", targets[0].Labels)
	}
	if targets[1].Labels["os"] != "darwin" {
		t.Errorf("expected darwin target, got %v", targets[1].Labels)
	}
}

func TestTargetsHashTracksMembership(t *testing.T) {
	st := sampleStore()
	h1 := TargetsHash(st, 9090)
	// Give the previously-IP-less member an address: the target set changes.
	st.Pools[0].Members[1].IP = "192.0.2.21"
	if TargetsHash(st, 9090) == h1 {
		t.Fatal("TargetsHash did not change when a member gained an IP")
	}
	// Port change also changes the hash.
	if TargetsHash(st, 9091) == TargetsHash(st, 9090) {
		t.Fatal("TargetsHash ignored the scrape port")
	}
}
