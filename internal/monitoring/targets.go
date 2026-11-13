package monitoring

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ephemerd/mayfly/internal/state"
)

// Target is one Prometheus file_sd entry: an address list plus labels that
// ride along on every scraped series.
type Target struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// BuildTargets turns recorded pool members into Prometheus scrape targets,
// one entry per member that has a discovered LAN IP. port is ephemerd's
// /metrics port. Members without an IP (e.g. a darwin host that never
// reported, or a VM still booting) are skipped — they reappear once Refresh
// records their address.
func BuildTargets(st *state.Store, port int) []Target {
	var targets []Target
	for _, pool := range st.Pools {
		for _, m := range pool.Members {
			// VM members are scraped at their guest-agent-discovered IP.
			// Enrolled bare metal (darwin or linux) has no VMID/agent IP, but
			// its Hostname is the SSH host (an IP or resolvable name) — scrape
			// that.
			addr := m.IP
			if addr == "" && m.VMID == 0 {
				addr = m.Hostname
			}
			if addr == "" {
				continue
			}
			target := fmt.Sprintf("%s:%d", addr, port)
			targets = append(targets, Target{
				Targets: []string{target},
				Labels: map[string]string{
					"pool": pool.Name,
					"host": m.Hostname,
					"os":   m.OS,
					"arch": m.Arch,
				},
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Targets[0] < targets[j].Targets[0] })
	return targets
}

// TargetsJSON renders the file_sd document mayfly writes to TargetsPath.
func TargetsJSON(st *state.Store, port int) ([]byte, error) {
	b, err := json.MarshalIndent(BuildTargets(st, port), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// TargetsHash fingerprints the target set so an unchanged fleet does not
// re-push targets on every apply.
func TargetsHash(st *state.Store, port int) string {
	b, err := TargetsJSON(st, port)
	if err != nil {
		return "invalid:" + err.Error()
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}
