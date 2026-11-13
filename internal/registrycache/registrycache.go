// Package registrycache provides the embedded assets for the container
// registry pull-through cache that runs on the fleet's utility host — the
// same long-lived VM the [monitoring] section provisions for Prometheus and
// Grafana.
//
// The cache is a stock `registry:2` in proxy mode. Runner nodes point their
// container runtime at it, so a job image crosses the WAN once and is then
// served from the LAN for every subsequent job.
//
// It runs as its own Docker Compose project (/opt/registry-cache), separate
// from the monitoring project, so enabling or disabling the cache never
// disturbs Prometheus or Grafana. Its blob store is bounded: the registry
// expires un-repulled blobs on a TTL, and a systemd timer drops the store
// outright if it ever exceeds the configured cap.
package registrycache

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

const (
	// ComposeDir is the cache's Compose project directory on the utility host.
	ComposeDir = "/opt/registry-cache"
	// DataDir is the bind-mounted blob store. A host path (rather than a named
	// volume) so the prune script and the operator have a stable path.
	DataDir = "/var/lib/registry-cache"
	// EnvPath holds the rendered settings; Compose reads .env from the project
	// dir automatically, and the prune unit loads it as its EnvironmentFile.
	EnvPath = ComposeDir + "/.env"
	// PruneScriptPath is the size-cap enforcer invoked by the timer.
	PruneScriptPath = "/usr/local/sbin/registry-cache-prune"
	// PruneTimer is the systemd timer that runs it.
	PruneTimer = "registry-cache-prune.timer"
	// ContainerName is the registry container (stable, for operator commands).
	ContainerName = "registry-cache"
)

// StackFile pairs a guest path with the embedded file content.
type StackFile struct {
	Path    string
	Mode    string // octal string, e.g. "0644"
	Content []byte
}

// guestPath maps an embedded asset path (rooted at "assets/") to its on-VM
// location and mode. Layout mirrors internal/registrycache/assets/README.md.
func guestPath(rel string) (path, mode string, keep bool) {
	switch rel {
	case "README.md":
		return "", "", false
	case "docker-compose.yml":
		return ComposeDir + "/docker-compose.yml", "0644", true
	case "prune.sh":
		return PruneScriptPath, "0755", true
	case "registry-cache-prune.service", "registry-cache-prune.timer":
		return "/etc/systemd/system/" + rel, "0644", true
	}
	return "", "", false
}

// StackFiles returns every embedded cache asset mapped to its guest path.
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
		gp, mode, keep := guestPath(rel)
		if !keep {
			return nil
		}
		b, err := assetsFS.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, StackFile{Path: gp, Mode: mode, Content: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// StackDirs returns the guest directories that must exist before the files
// land and before the container starts (the bind mount must not be created by
// Docker as a root-owned surprise).
func StackDirs() []string {
	return []string{ComposeDir, DataDir}
}

// Options are the rendered settings for one cache instance.
type Options struct {
	Port      int
	Upstream  string
	TTL       string
	MaxSizeGB int
	Username  string
	Password  string
}

// EnvFile renders the project .env consumed by both Compose and the prune
// unit. It is written 0600 because it can carry upstream credentials.
func EnvFile(o Options) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "REGISTRY_PORT=%d\n", o.Port)
	fmt.Fprintf(&b, "REGISTRY_UPSTREAM=%s\n", o.Upstream)
	fmt.Fprintf(&b, "REGISTRY_TTL=%s\n", o.TTL)
	fmt.Fprintf(&b, "REGISTRY_UPSTREAM_USERNAME=%s\n", o.Username)
	fmt.Fprintf(&b, "REGISTRY_UPSTREAM_PASSWORD=%s\n", o.Password)
	fmt.Fprintf(&b, "REGISTRY_CACHE_MAX_GB=%d\n", o.MaxSizeGB)
	fmt.Fprintf(&b, "REGISTRY_CACHE_DATA=%s\n", DataDir)
	fmt.Fprintf(&b, "REGISTRY_CACHE_DIR=%s\n", ComposeDir)
	return []byte(b.String())
}

// AssetsHash fingerprints the embedded cache assets so editing the compose
// file, the prune script, or a unit shows up as utility-host drift on the
// next apply.
func AssetsHash() string {
	files, err := StackFiles()
	if err != nil {
		return "invalid:" + err.Error()
	}
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s|%s\n", f.Path, f.Mode)
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
