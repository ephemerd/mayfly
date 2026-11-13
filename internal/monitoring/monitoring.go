// Package monitoring provides the embedded Prometheus + Grafana stack assets
// and the Prometheus file_sd target set generated from mayfly state. The
// monitoring node is a single long-lived VM (not a runner pool) that scrapes
// every pool member's ephemerd /metrics endpoint over the LAN.
package monitoring

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed all:assets
var assetsFS embed.FS

// Compose project directory on the monitoring VM.
const ComposeDir = "/opt/monitoring"

// TargetsPath is where mayfly writes the file_sd target list; Prometheus
// watches /etc/prometheus/targets/*.json.
const TargetsPath = "/etc/prometheus/targets/runners.json"

// EnvPath holds the Grafana admin password for docker compose (.env in the
// project dir is read automatically).
const EnvPath = ComposeDir + "/.env"

// File is one file to drop onto the monitoring VM.
type File struct {
	Path string // absolute guest path
	Mode string // octal string, e.g. "0644"
}

// guestPath maps an embedded asset path (rooted at "assets/") to its on-VM
// location. Layout mirrors internal/monitoring/assets/README.md.
func guestPath(rel string) (string, bool) {
	switch {
	case rel == "README.md":
		return "", false
	case rel == "docker-compose.yml":
		return ComposeDir + "/docker-compose.yml", true
	case rel == "prometheus.yml":
		return "/etc/prometheus/prometheus.yml", true
	case strings.HasPrefix(rel, "grafana/provisioning/"):
		return "/etc/grafana/provisioning/" + strings.TrimPrefix(rel, "grafana/provisioning/"), true
	case strings.HasPrefix(rel, "grafana/dashboards/"):
		return "/var/lib/grafana-dashboards/" + strings.TrimPrefix(rel, "grafana/dashboards/"), true
	}
	return "", false
}

// StackFile pairs a guest path with the embedded file content.
type StackFile struct {
	Path    string
	Mode    string
	Content []byte
}

// StackFiles returns every embedded stack asset mapped to its guest path.
func StackFiles() ([]StackFile, error) {
	var out []StackFile
	err := fs.WalkDir(assetsFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, "assets/")
		gp, keep := guestPath(rel)
		if !keep {
			return nil
		}
		b, err := assetsFS.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, StackFile{Path: gp, Mode: "0644", Content: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// StackDirs returns the guest directories cloud-init must create before the
// files land (write_files does not always mkdir -p reliably across distros).
func StackDirs() []string {
	return []string{
		ComposeDir,
		"/etc/prometheus",
		"/etc/prometheus/targets",
		"/etc/grafana/provisioning/datasources",
		"/etc/grafana/provisioning/dashboards",
		"/var/lib/grafana-dashboards",
	}
}

// AssetsHash fingerprints the embedded stack so a change to any compose/
// prometheus/grafana asset shows up as monitoring drift on the next apply.
func AssetsHash() string {
	files, err := StackFiles()
	if err != nil {
		return "invalid:" + err.Error()
	}
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\n", f.Path)
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
