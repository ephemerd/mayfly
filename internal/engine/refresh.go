package engine

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// RefreshResult describes drift found between recorded state and live infra.
type RefreshResult struct {
	Findings []DriftFinding
}

type DriftFinding struct {
	Pool   string
	Member string // member hostname (or vmid as string for VMs)
	Kind   string // "vm-missing" | "vm-resources" | "tunnel-missing" | "ssh-unreachable" | "ephemerd-missing"
	Detail string
}

// Render prints findings terraform-style, before the plan proper.
func (r *RefreshResult) Render(w io.Writer) {
	for _, f := range r.Findings {
		id := f.Pool
		if f.Member != "" {
			id += " " + f.Member
		}
		fmt.Fprintf(w, "  ! drift pool.%s: %s\n", id, f.Detail)
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(w)
	}
}

// Refresh reconciles the in-memory state store against live infrastructure,
// MUTATING e.St so the subsequent Diff produces corrective actions. Returns
// the findings for plan rendering. sshProbe controls the (slow) darwin SSH
// probes; PVE and Cloudflare checks always run.
func (e *Engine) Refresh(ctx context.Context, sshProbe bool) (*RefreshResult, error) {
	res := &RefreshResult{}
	if err := e.refreshVMs(ctx, res); err != nil {
		return nil, err
	}
	if e.Cfg.Monitoring != nil {
		// Only worth the per-VM guest-agent round trips when something scrapes
		// these addresses.
		e.refreshMemberIPs(ctx)
	}
	e.refreshMemberVersions(ctx, res)
	if err := e.refreshTunnels(ctx, res); err != nil {
		return nil, err
	}
	if sshProbe {
		e.refreshEnrolled(ctx, res)
	}
	return res, nil
}

// liveVM is the subset of the PVE qemu list response the refresh compares.
type liveVM struct {
	Cores int
	MemMB int
}

// refreshVMs lists VMs once per (backend, node) referenced by state and
// reconciles every VM member against the result. Skipped entirely for
// darwin-only fleets so the Proxmox client (possibly unconfigured) is never
// touched.
func (e *Engine) refreshVMs(ctx context.Context, res *RefreshResult) error {
	// (provider, node) pairs to list, one entry per pool's placement.
	type target struct {
		prov config.Provider
		node string
	}
	targets := map[target]bool{}
	for i := range e.St.Pools {
		pool := &e.St.Pools[i]
		prov := e.Cfg.Pools[pool.Name].ResolvedProvider()
		for _, m := range pool.Members {
			if m.VMID > 0 {
				targets[target{prov: prov, node: m.PVENode}] = true
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}

	// live is keyed by provider then node so members reconcile only against
	// their own backend's listing.
	live := map[config.Provider]map[string]map[int]liveVM{}
	for t := range targets {
		prov, err := e.providerByName(t.prov)
		if err != nil {
			return fmt.Errorf("refresh: %s backend: %w", t.prov, err)
		}
		infos, err := prov.ListInstances(ctx, t.node)
		if err != nil {
			return fmt.Errorf("refresh: list VMs on %s/%q: %w", t.prov, t.node, err)
		}
		byID := make(map[int]liveVM, len(infos))
		for _, in := range infos {
			byID[in.ID] = liveVM{Cores: in.Cores, MemMB: in.MemMB}
		}
		if live[t.prov] == nil {
			live[t.prov] = map[string]map[int]liveVM{}
		}
		live[t.prov][t.node] = byID
	}

	for i := range e.St.Pools {
		pool := &e.St.Pools[i]
		prov := e.Cfg.Pools[pool.Name].ResolvedProvider()
		res.Findings = append(res.Findings, reconcilePoolVMs(pool, live[prov])...)
	}
	return nil
}

// reconcilePoolVMs applies VM drift decisions to one pool, mutating its
// member list, and returns the findings. Pure given the live snapshot.
func reconcilePoolVMs(pool *state.Pool, live map[string]map[int]liveVM) []DriftFinding {
	var findings []DriftFinding
	kept := pool.Members[:0]
	for _, m := range pool.Members {
		if m.VMID <= 0 {
			kept = append(kept, m)
			continue
		}
		lv, exists := live[m.PVENode][m.VMID]
		act := reconcileVM(m, exists, lv.Cores, lv.MemMB)
		if act.Kind != "" {
			findings = append(findings, DriftFinding{
				Pool:   pool.Name,
				Member: strconv.Itoa(m.VMID),
				Kind:   act.Kind,
				Detail: act.Detail,
			})
		}
		if act.Remove {
			continue
		}
		if act.Kind == "vm-resources" {
			m.Cores, m.MemoryMB = act.Cores, act.MemMB
		}
		kept = append(kept, m)
	}
	pool.Members = kept
	return findings
}

// vmAction is reconcileVM's decision: an empty Kind means no drift.
type vmAction struct {
	Kind   string
	Detail string
	Remove bool // drop the member from state
	Cores  int  // recorded values to adopt when Kind == "vm-resources"
	MemMB  int
}

func reconcileVM(m state.Member, liveExists bool, liveCores, liveMemMB int) vmAction {
	if !liveExists {
		return vmAction{
			Kind:   "vm-missing",
			Detail: fmt.Sprintf("vm %d gone from PVE (deleted outside mayfly; will provision a replacement)", m.VMID),
			Remove: true,
		}
	}
	// A zero from the list endpoint means the field wasn't populated —
	// don't invent drift from missing data.
	cores, memMB := m.Cores, m.MemoryMB
	if liveCores > 0 {
		cores = liveCores
	}
	if liveMemMB > 0 {
		memMB = liveMemMB
	}
	if cores == m.Cores && memMB == m.MemoryMB {
		return vmAction{}
	}
	return vmAction{
		Kind: "vm-resources",
		Detail: fmt.Sprintf("live cores=%d mem=%dMB, recorded cores=%d mem=%dMB (will power-cycle back to spec)",
			cores, memMB, m.Cores, m.MemoryMB),
		Cores: cores,
		MemMB: memMB,
	}
}

// refreshMemberIPs re-discovers each VM member's LAN IP via the guest agent so
// Prometheus scrape targets track DHCP changes and pre-monitoring members get
// an address on first apply. Best-effort: a powered-off or agentless VM keeps
// its recorded IP.
func (e *Engine) refreshMemberIPs(ctx context.Context) {
	for i := range e.St.Pools {
		pool := &e.St.Pools[i]
		prov, err := e.providerForPool(pool.Name)
		if err != nil {
			continue // backend unavailable: keep recorded IPs
		}
		for j := range pool.Members {
			m := &pool.Members[j]
			if m.VMID <= 0 {
				continue
			}
			inst, err := prov.GetInstance(ctx, m.PVENode, m.VMID)
			if err != nil {
				continue
			}
			if ip, err := inst.PrimaryIPv4(ctx); err == nil && ip != "" {
				m.IP = ip
			}
		}
	}
}

// refreshTunnels verifies every pool's tunnel still exists. A gone tunnel is
// surfaced but never self-corrected: recreating it would invalidate every
// member's run token, which only a destroy + re-apply (or manual fix) heals.
func (e *Engine) refreshTunnels(ctx context.Context, res *RefreshResult) error {
	for i := range e.St.Pools {
		pool := &e.St.Pools[i]
		if pool.TunnelID == "" {
			continue
		}
		exists, err := e.CF.TunnelExists(ctx, pool.TunnelID)
		if err != nil {
			return fmt.Errorf("refresh: tunnel for pool %s: %w", pool.Name, err)
		}
		if !exists {
			res.Findings = append(res.Findings, DriftFinding{
				Pool: pool.Name,
				Kind: "tunnel-missing",
				Detail: fmt.Sprintf("tunnel %s deleted from Cloudflare; members hold tokens for the old tunnel — run `mayfly destroy %s` then re-apply, or restore it manually",
					pool.TunnelID, pool.Name),
			})
		}
	}
	return nil
}

const enrollProbeScript = "test -x /usr/local/bin/ephemerd && /usr/local/bin/ephemerd status || echo MISSING"

// enrollProbeTimeout caps each SSH probe so one powered-off box doesn't
// stall the whole plan (SSHRun's own internal timeout is 5 minutes).
const enrollProbeTimeout = 15 * time.Second

// refreshEnrolled SSH-probes every enrolled bare-metal member (darwin Mac mini
// or linux box — both have no VMID) and surfaces reachability / ephemerd-missing
// drift. VM members are handled by refreshVMs via the guest agent instead.
func (e *Engine) refreshEnrolled(ctx context.Context, res *RefreshResult) {
	for i := range e.St.Pools {
		pool := &e.St.Pools[i]
		for j := range pool.Members {
			m := &pool.Members[j]
			if m.VMID != 0 {
				continue // VM member: probed via PVE, not SSH
			}
			probeCtx, cancel := context.WithTimeout(ctx, enrollProbeTimeout)
			out, err := SSHRun(probeCtx, sshTarget(pool, m.Hostname), enrollProbeScript, nil)
			cancel()
			act := reconcileDarwin(err, out)
			if act.Kind == "" {
				// Healthy: the probe ran `ephemerd status`, so the live
				// version is right there. Correct state from it.
				if spec, ok := resolvedPoolSpec(e.Cfg, pool.Name); ok {
					if h, herr := parseNodeHealth(out); herr == nil {
						if f, ok := adoptFinding(pool.Name, m.Hostname, m, h.Version, DesiredBinary(e.Cfg, spec)); ok {
							res.Findings = append(res.Findings, f)
						}
					}
				}
				continue
			}
			if act.ClearConfig {
				m.ConfigHash = ""
			}
			res.Findings = append(res.Findings, DriftFinding{
				Pool:   pool.Name,
				Member: m.Hostname,
				Kind:   act.Kind,
				Detail: act.Detail,
			})
		}
	}
}

// darwinAction is reconcileDarwin's decision: an empty Kind means healthy.
type darwinAction struct {
	Kind        string
	Detail      string
	ClearConfig bool // zero ConfigHash so Diff plans a config-tier re-enroll
}

func reconcileDarwin(probeErr error, output string) darwinAction {
	if probeErr != nil {
		// Host may just be powered off — surface it, don't churn state.
		return darwinAction{
			Kind:   "ssh-unreachable",
			Detail: "unreachable over SSH (host off? state left as-is): " + probeErr.Error(),
		}
	}
	if strings.Contains(output, "MISSING") {
		return darwinAction{
			Kind:        "ephemerd-missing",
			Detail:      "ephemerd missing (will re-enroll)",
			ClearConfig: true,
		}
	}
	return darwinAction{}
}

// versionProbeTimeout bounds one live `ephemerd status` read during refresh,
// so a node with a wedged guest agent costs the plan seconds, not minutes.
const versionProbeTimeout = 20 * time.Second

// versionProbeParallel caps concurrent guest-agent probes.
const versionProbeParallel = 4

// refreshMemberVersions reads the ephemerd version each VM member actually
// runs and corrects state.Member.Ephemerd from it. That is what reconciles an
// `ephemerd upgrade` run directly on a node, outside mayfly: without it, state
// would keep the old version and plan would offer to upgrade a node that is
// already current (or, before binary and config drift were split, to restart
// it). A member that does not answer keeps its recorded value.
func (e *Engine) refreshMemberVersions(ctx context.Context, res *RefreshResult) {
	type job struct {
		pool *state.Pool
		m    *state.Member
		want string
		prov provider.Provider
	}
	var jobs []job
	for i := range e.St.Pools {
		pool := &e.St.Pools[i]
		spec, ok := resolvedPoolSpec(e.Cfg, pool.Name)
		if !ok {
			continue
		}
		want := DesiredBinary(e.Cfg, spec)
		for j := range pool.Members {
			m := &pool.Members[j]
			if m.VMID <= 0 {
				continue // enrolled: probed over SSH by refreshEnrolled
			}
			prov, err := e.providerForPool(pool.Name)
			if err != nil {
				continue
			}
			jobs = append(jobs, job{pool: pool, m: m, want: want, prov: prov})
		}
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		findings []DriftFinding
	)
	sem := make(chan struct{}, versionProbeParallel)
	for _, jb := range jobs {
		wg.Add(1)
		go func(jb job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
			defer cancel()
			inst, err := jb.prov.GetInstance(pctx, jb.m.PVENode, jb.m.VMID)
			if err != nil {
				return
			}
			h, err := ProbeNodeHealth(pctx, instanceExecutor(inst, jb.m.Hostname, jb.m.OS), jb.m.OS)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if f, ok := adoptFinding(jb.pool.Name, strconv.Itoa(jb.m.VMID), jb.m, h.Version, jb.want); ok {
				findings = append(findings, f)
			}
		}(jb)
	}
	wg.Wait()
	sort.Slice(findings, func(a, b int) bool {
		if findings[a].Pool != findings[b].Pool {
			return findings[a].Pool < findings[b].Pool
		}
		return findings[a].Member < findings[b].Member
	})
	res.Findings = append(res.Findings, findings...)
}

// adoptFinding applies adoptLiveVersion to m and, when it corrected a value
// state had recorded, describes the correction. Silently filling in an
// unknown version is not drift worth reporting.
func adoptFinding(pool, member string, m *state.Member, reported, want string) (DriftFinding, bool) {
	old, changed := adoptLiveVersion(m, reported, want)
	if !changed || old == "" {
		return DriftFinding{}, false
	}
	return DriftFinding{
		Pool:   pool,
		Member: member,
		Kind:   "ephemerd-version",
		Detail: fmt.Sprintf("node reports ephemerd %s but state recorded %s (upgraded outside mayfly; state corrected)",
			m.Ephemerd, describeBinary(old)),
	}, true
}
