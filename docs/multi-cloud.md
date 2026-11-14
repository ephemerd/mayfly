# Multi-Cloud Node Pools

> Status: design / strategy. Goal: evolve mayfly from a homelab Proxmox provisioner
> into a placement engine that can run ephemerd node pools **wherever compute is
> cheapest**, across clouds and the homelab at the same time, with Windows/Mac
> nested-virt pools wherever they're actually possible.

## 1. The goal

- **Cheapest possible compute** for the bulk of the fleet (Linux amd64/arm64 CI jobs).
- **Nested virt where it's hard**: Windows (Hyper-V-isolated containers) and macOS
  (Vz). These stay on bare metal or the homelab, because most clouds can't do them
  cheaply or at all.
- **Pools anywhere**: a single mayfly config describes pools that live in AWS, Azure,
  Hetzner, Scaleway, *and* the homelab simultaneously, each placed where its
  workload is cheapest/possible.
- **One control plane** reaching all of them, regardless of NAT, CGNAT, or country.

This is the step from "homelab tool" to "distributed fleet provisioner."

## 2. The insight that makes global pools work

**GitHub Actions self-hosted runners are outbound-only.** A runner long-polls
`api.github.com` / the pipelines service over HTTPS and pulls its jobs. It needs **no
inbound public IP and no open ports** — only outbound 443. So the *job host can live
anywhere*: any cloud, any region, behind any NAT, in the homelab on CGNAT. Location
is free for the job path.

What is *not* free, and is where all the real engineering is:

1. **Control path** — mayfly reaching each node to install/configure ephemerd, push
   the GitHub App PEM, start/drain it. Needs reachability *to* the node.
2. **Shared-services path** — runners pulling from the registry pull-through cache,
   the Actions cache, and the model server. A single homelab cache is slow or
   unroutable from a pool in Frankfurt. (We've already seen this: the registry cache
   went to a **0% hit ratio** after the VLAN rollout because runners couldn't route
   to it. Going multi-cloud makes that worse unless caches are regional or fronted.)

Everything below is about making those two paths work from anywhere.

## 3. Provider capability matrix

| Provider | Linux amd64/arm64 | Spot / cheap-ephemeral | Windows nested Hyper-V | macOS (Vz) | Native autoscaler |
|---|---|---|---|---|---|
| **AWS** | ✅ (Graviton arm64) | ✅ Spot (2-min notice) | ⚠️ `.metal` only | ⚠️ Dedicated Host, 24h min, no spot | ASG / Spot Fleet |
| **Azure** | ✅ (Ampere arm64) | ✅ Spot (**30-sec** notice) | ✅ **on shared VMs** (Dv3+/Dsv5) | ❌ | VMSS + Spot Priority Mix |
| **GCP** | ✅ | ✅ Spot | ❌ (nested = Linux KVM L1 only) | ❌ | Managed Instance Groups |
| **Hetzner** | ✅ (Ampere arm64, cheapest flat) | flat-rate so cheap spot is moot; bare-metal auction | ✅ on **bare metal** (your own CPU) | ❌ | none — mayfly reconcile is the scaler |
| **Scaleway** | ✅ (Ampere arm64) | Elastic Metal hourly, no notice period | ✅ on **Elastic Metal** | ✅ **Apple silicon, €0.11/hr hourly** | Instance pools (limited) |
| **Homelab (Proxmox)** | ✅ | sunk cost = free | ✅ (already running) | ✅ Mac mini (already running) | mayfly reconcile |

Key takeaways:

- **Windows nested Hyper-V in a *shared* cloud VM: only Azure.** Everywhere else needs
  bare metal (your own CPU → nesting is never the question) or the homelab.
- **macOS is always bare-metal Apple hardware** (Apple EULA). Vz runs natively on all
  of them; it's purely a price/lifecycle choice. Scaleway is the cheapest hourly;
  the homelab Mac mini is the cheapest absolute.
- **GCP can't do Windows nested virt at all** (its nesting is Linux-KVM-L1 only).

## 4. Cost landscape — cheapest compute by workload

Prices move; treat these as *relative* orientation (checked 2026-11).

**Linux arm64 (the bulk of CI):**
- **Oracle Cloud always-free**: 4 OCPU / 24 GB Ampere, *free*. Good for an always-on
  control/service node or a small permanent runner.
- **Hetzner CAX (Ampere)**: ~€4–16/mo flat (CAX31 8 vCPU/16 GB ≈ $0.018/hr). Cheapest
  *flat-rate* arm by a wide margin — makes Graviton look expensive. No spot, but the
  flat price already beats most spot.
- **AWS Graviton spot / Azure Ampere spot**: elastic burst capacity, pricier per-core
  than Hetzner flat but scales to zero. Use for spiky load on top of a Hetzner/Oracle
  baseline.

**Linux amd64:** same story — Hetzner flat (incl. dirt-cheap bare-metal auction
servers) for baseline, hyperscaler spot for burst.

**Windows (nested Hyper-V):**
- **Homelab**: sunk hardware, effectively free — keep it here if you have the box.
- **Azure Spot Dsv5**: cheapest *elastic* cloud option, but 30-sec eviction notice.
- **Hetzner / Scaleway Elastic Metal**: own the CPU, hourly billing, no nesting
  asterisks; more than a shared VM but no 24h floor like EC2 `.metal`/Mac.

**macOS (Vz):**
- **Homelab Mac mini**: cheapest absolute (already running).
- **Scaleway Apple silicon**: ~€0.11/hr, true hourly — by far the most spot-like Mac.
- **AWS EC2 Mac**: avoid — Dedicated Host, 24h minimum, no spot.

**Rule of thumb:** cheap Linux goes to the cheapest flat/spot cloud; expensive-in-
cloud OSes (Windows nested, Mac) stay in the homelab unless you specifically need
cloud elasticity, in which case Azure (Windows) and Scaleway (Mac) are the escape
hatches.

## 5. Recommended placement strategy

| Workload | First choice | Cloud escape hatch |
|---|---|---|
| Linux arm64 baseline | Oracle free + Hetzner flat | Azure/AWS arm spot for burst |
| Linux amd64 baseline | Hetzner flat / bare-metal auction | AWS/Azure spot for burst |
| Windows (Hyper-V isolation) | **Homelab** | Azure Spot Dsv5, or Hetzner/Scaleway Elastic Metal |
| macOS (Vz) | **Homelab Mac mini** | Scaleway Apple silicon |
| Shared services (monitoring, registry cache, model server, GPU) | **Homelab Proxmox** | regional cache replicas (see §6) |

The fleet becomes **multi-cloud by OS and by cost**, not single-provider. Service VMs
and the GPU model server stay on Proxmox (hostpci is Proxmox-specific); only runner
pools fan out.

## 6. Networking: reaching pools anywhere

### Job path — already solved
Runner → GitHub is outbound HTTPS. Works from any cloud/NAT/country with zero inbound.
Nothing to build.

### Control path — mayfly → node
mayfly needs to SSH/exec into each node. Three transports, pick per pool:

1. **Public IP + SSH** — simplest. But exposes SSH to the internet, costs a public
   IPv4 (AWS now bills for them), and doesn't work behind CGNAT or a homelab NAT.
   Fine for cloud pools with a locked-down security group; bad for homelab/CGNAT.
2. **Cloudflare Tunnel (`cloudflared`)** — the node dials *out* and becomes reachable
   by a stable hostname with **no inbound ports**. Works behind any NAT, CGNAT, and
   the homelab firewall. mayfly's existing SSH transport just targets the tunnel
   hostname. This is the robust "pools anywhere" answer.
3. **Tailscale / WireGuard mesh** — every node + the control plane on one overlay,
   each node gets a stable overlay IP. Arguably simpler than Cloudflare for pure
   control-plane reach, and no third-party HTTP proxy in the path. Strong alternative
   to (2).

Recommendation: make node-reach a **pluggable transport** (public-IP-SSH |
cloudflared | tailscale) chosen per pool. Homelab and CGNAT pools use a tunnel/mesh;
cloud pools with public IPs can use direct SSH.

### Shared-services path — the "global load balancing" question
This is where "Cloudflared load balancing with public IPs all over the world" actually
lives — for the **caches and services**, not the runners.

- **Cloudflare Load Balancing** is a real product: it health-checks origin *pools* and
  steers each client by **geo/latency/weight/session affinity**. Origins can be raw
  public IPs *or* Cloudflare Tunnels. So yes — in theory a Cloudflare LB can front
  registry-cache / actions-cache / model-server origins that live anywhere in the
  world (even behind tunnels, no public IP needed) and route each runner to the
  nearest healthy one. The model server (`homeai.tricorder.cc`) already sits behind
  Cloudflare, so this is a known-good pattern here.
- **Caveat — don't hairpin big pulls through a tunnel.** Registry image pulls are
  high-bandwidth; routing a Frankfurt pool's pulls through a tunnel back to a homelab
  cache is slow and can hit throughput/egress limits. For caches, prefer **regional
  replicas close to each pool** (a cache per cloud region, selected by pool config)
  over one globally-fronted origin. Use Cloudflare LB for *control-plane / low-
  bandwidth* services and small/warm caches; use regional cache replicas for the
  heavy image/layer traffic.

So the honest answer to "will Cloudflare LB work with public IPs from all over the
world?": **yes for service discovery and failover, with geo-steering** — but size the
caches regionally so you're not pulling gigabytes across oceans through a proxy.

## 7. What mayfly has to become

mayfly already has the right bones: a `provider.Provider` / `provider.Instance`
abstraction (one Proxmox impl today, hardwired at `engine.go`), a desired-state
reconcile loop, drift tiers, rolling replace, and an SSH `NodeExecutor`. The work is
*generalizing* that, not inventing it.

**The unlock: mayfly's reconcile loop is the cross-cloud autoscaler.** Instead of
leaning on each cloud's native scaler (ASG / VMSS / MIG), mayfly declares "pool X wants
N members" and provisions/replaces across *any* provider. Native scalers become
optional per-provider optimizations, not requirements — which is the only way to get
uniform autoscaling semantics over Hetzner + homelab + hyperscaler at once.

Phased work:

- **Phase 0 — opaque instance handle.** Replace the Proxmox `int` VMID in
  `provider.Instance` and the state file with an opaque string handle (`i-…`,
  `vm-…`, tunnel hostname). No behavior change; unblocks every non-Proxmox provider.
- **Phase 1 — AWS provider** (already specced): RunInstances, stock Ubuntu AMI +
  user-data (ephemerd install + config, **no secret in user-data**), mayfly SSHes in
  and pushes the PEM, EC2 API for IPs, per-pool `market = spot|on_demand` +
  `on_demand_base`. Linux arm64/amd64 only.
- **Phase 2 — provider registry + general per-pool placement.** Generalize the
  per-pool `provider` / `region` / `market` config beyond Proxmox+AWS. Add:
  - **Hetzner** (cheapest flat arm/amd baseline; bare-metal for Windows nesting),
  - **Azure** (Windows nested Hyper-V on spot; VMSS Spot Priority Mix maps 1:1 onto
    our `market` + `on_demand_base` knob if we ever use native scale sets),
  - **Scaleway** (Apple silicon Mac + Elastic Metal).
- **Phase 3 — networking model.** Pluggable node-reach transport (public-IP-SSH |
  cloudflared | tailscale) per pool; regional shared-service discovery (nearest cache)
  and/or Cloudflare LB fronting for low-bandwidth services.

Cross-cutting robustness for "pools anywhere":

- **Per-provider fail-soft.** One cloud's API being down (or a region unreachable)
  must not abort the whole `apply` — same principle as the VLAN preflight fail-soft we
  already shipped. Reconcile each pool independently.
- **State records provider + region + handle** per member, so a replace goes back to
  the same place and a lost member is re-created in the right cloud.
- **Generous deadlines on every remote call.** WAN/tunnel latency is far worse than
  LAN; a remote op with no deadline wedges a slot (cf. the macOS SSH-setup hang that
  held the sole Mac slot for 10h). Every cross-cloud exec needs a deadline.
- **Per-provider interruption handling.** AWS = 2-min drain; **Azure = 30-sec →
  abort/requeue the in-flight job, don't try to drain**; Hetzner flat / homelab = no
  eviction. Keep an on-demand/base floor (Spot Priority Mix style) so an eviction
  never empties a pool.
- **Per-provider credentials** in config (cloud API keys), kept out of user-data and
  out of anything IMDS-readable; secrets pushed post-boot over the control transport.

## 8. Open questions

- **Transport default**: Cloudflare Tunnel vs Tailscale for the control plane? Tailscale
  is simpler and keeps us off an HTTP proxy; Cloudflare reuses what already fronts the
  model server. Possibly support both.
- **Cache topology**: one Cloudflare-LB'd origin set vs a cache replica per cloud
  region. Leaning regional replicas for image pulls, LB for the rest.
- **Billing/opt-in guardrails**: spinning up cloud pools spends real money — mayfly
  should make per-pool spend explicit and bounded (max members, max price).
- **Windows-in-cloud**: is Azure spot's 30-sec notice acceptable for Windows CI, or do
  we keep Windows homelab-only and treat Azure as burst-overflow?
