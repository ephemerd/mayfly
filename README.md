# mayfly

Declaratively manage a fleet of ephemeral, self-hosted GitHub Actions runners on your own hardware.

mayfly is a single Go CLI that provisions and reconciles runner nodes across a Proxmox cluster (and bare-metal Macs), wires them up to GitHub through Cloudflare tunnels, and keeps the whole fleet matching a declarative config — Terraform-style `plan` / `apply` / `destroy`. Each node runs [ephemerd](https://github.com/ephpm/ephemerd), which executes every CI job in a fresh, isolated, single-use environment and throws it away afterward.

If you've ever wanted GitHub-hosted-runner ergonomics on hardware you own — Linux, Windows, and macOS, all managed from one config file — that's mayfly.

---

## Why

- **Ephemeral by default.** Every job gets a clean, isolated runner that's destroyed when the job finishes. No state bleed between builds, no snowflake runners to babysit.
- **Declarative.** You describe the fleet you want in one TOML file; `mayfly plan` shows the diff, `mayfly apply` makes it so. State lives in `~/.mayfly/state.yaml`.
- **Multi-platform from one tool.** Linux (amd64/arm64), Windows (amd64), and macOS (Apple Silicon) runners, side by side.
- **No inbound ports, no SSH into VMs.** Webhooks arrive over Cloudflare tunnels; mayfly drives VMs through the Proxmox QEMU guest agent, not SSH. (Bare-metal Macs are the one exception — they're enrolled over SSH since mayfly can't provision the hardware.)
- **Cheap changes stay cheap.** Adding a repo restarts a service in seconds; only a template change forces a rebuild. mayfly always picks the least-disruptive action.

## How it works

```
  GitHub  --workflow_job webhook-->  Cloudflare Tunnel  -->  ephemerd (on a runner node)
                                                                  |
                                            registers a JIT runner + spins up an isolated
                                            container for exactly this one job, then tears it down
```

- A **pool** is a group of identical runners for one OS/arch (e.g. `linux-amd64`). Each pool gets one Cloudflare tunnel, one DNS name, and one shared webhook secret.
- GitHub delivers every `workflow_job` event to every pool's webhook. Each node's scheduler claims **only** the jobs whose `runs-on` labels match its platform, so a Windows job never lands on a Linux box.
- ephemerd registers a **just-in-time (single-use) runner** per queued job and runs it in an isolated container (native containers on Linux/Windows; per-job VMs on macOS). Nothing survives the job.
- mayfly **provisions the hosts**: it clones a Proxmox template, feeds it cloud-init (NoCloud), installs and starts ephemerd, and manages everything afterward through the guest agent — no SSH into the VM.

## Supported runners

| Platform | Arch | How it's provisioned |
|---|---|---|
| Linux | amd64 / arm64 | Proxmox VM from an auto-built Ubuntu cloud-image template (cloud-init) |
| Windows | amd64 | Proxmox VM from a Packer-built Windows Server template (Cloudbase-Init) |
| macOS | arm64 | Bare-metal Mac enrolled over SSH (mayfly can't create the hardware) |

## Quick start

**Prerequisites**

- A [Proxmox VE](https://www.proxmox.com/) host with an API token (for Linux/Windows pools).
- A Cloudflare account with a domain, for tunnels + DNS (via the `cloudflared` CLI with an origin cert, or an API token).
- A [GitHub App](https://docs.github.com/en/apps) installed on the org/repos you want runners for (used to register JIT runners and manage webhooks).
- `ephemerd` releases you want to run, from [github.com/ephpm/ephemerd](https://github.com/ephpm/ephemerd).

**Install**

```sh
go install github.com/luthermonson/mayfly/cmd/mayfly@latest
# or build from source:
git clone https://github.com/luthermonson/mayfly
cd mayfly && go build -o mayfly ./cmd/mayfly
```

**Configure**

```sh
cp config.example.toml ~/.mayfly/config.toml
# edit ~/.mayfly/config.toml — Proxmox creds, GitHub App, and your pools
```

**Run**

```sh
mayfly plan      # preview the changes (Terraform-style diff)
mayfly apply     # reconcile the fleet to match config.toml
mayfly destroy   # tear down a pool, or the whole fleet
```

A minimal one-pool fleet:

```toml
[proxmox]
url = "https://pve.example.com:8006/api2/json"
token_id = "mayfly@pve!mayfly"
secret = "..."
node = "pve1"
storage = "local-lvm"

[github]
owner = "your-org"          # app_id / installation_id / key via env or config

[ephemerd]
version = "v0.1.0"

[pools.linux-amd64]
hostname = "ci-linux-amd64.example.com"
size = 1
repos = ["my-repo", "friend/project"]   # omit for org-level runners
```

See [`config.example.toml`](config.example.toml) for every option, including arm64, Windows, macOS, and the monitoring node.

## The change model

`mayfly apply` always chooses the cheapest action that satisfies the diff:

| You change | mayfly does | Cost |
|---|---|---|
| `repos`, creds, hostname | rewrite config + restart ephemerd (pool webhooks cordoned while each node drains) | seconds, caches kept |
| `[ephemerd] version` or a pool's `ephemerd_version` | ephemerd self-upgrade on each member (drains in-daemon, no webhook cordon) | seconds, caches kept |
| `cores` / `memory_mb` | drain + power-cycle | ~30s, caches kept |
| `disk_gb` increase | live resize + grow filesystem | no reboot |
| `size` up/down | provision / drain + destroy members | — |
| template change or `disk_gb` decrease | rolling member replacement | the only rebuilds |
| `--replace <pool>` | force-rebuild a wedged pool | escape hatch |

Before every plan, mayfly refreshes recorded state against live Proxmox and Cloudflare so drift (a manually-deleted VM, a resized disk, an `ephemerd upgrade` run directly on a node) is reconciled automatically. Each member line in the plan says why it is marked, so a real config change can be told apart from a version bump.

### Placing the VM disk on a separate storage (`vm_storage`)

By default mayfly **linked-clones** a pool's template, which forces every runner disk onto the template's storage. Templates need an import-capable *dir* storage (e.g. `local`), so a node whose only large storage is a block thinpool (`local-lvm`) is stuck with a tiny VM disk on the small dir store — the disk-full failure mode on the `node-b` node, where `local` is ~85 GiB but `local-lvm` is 794 GiB.

Set `vm_storage` on a pool to put the VM disk on a different storage via a **full clone**:

```toml
[pools.linux-amd64]
hostname = "ci-linux-amd64.example.com"
vlan_tag = 20
storage    = "local"       # template lives here (needs import content)
vm_storage = "local-lvm"   # runner disk is copied here (bigger, block thinpool)
```

`vm_storage` unset (the default) keeps the historical linked clone — existing pools are unaffected byte-for-byte. When set, the clone is a full clone: the disk is **copied** onto `vm_storage` (slower to provision, uses real space there) instead of merely referencing the template's, and can then be grown to whatever `disk_gb` asks for. `vm_storage` must accept disk images (`images`/`rootdir` content). It is per-pool only — there is deliberately no global default, since that would silently make every pool a full clone. Setting `vm_storage` equal to the pool's own disk `storage` is legal but pointless: it does a same-storage full copy for a placement a linked clone already gives — leave it empty in that case.

`vm_storage` is a clone-time property, so changing it on an existing pool forces a **rolling member replacement** (like `template` or `vlan_tag`) — that is how existing runners migrate onto the new storage, not just newly-scaled ones.

## macOS runner setup

mayfly can't create Mac hardware, so a `darwin` pool **enrolls** an existing Mac over SSH. mayfly then installs and manages ephemerd on it (binary, config, launchd service) exactly like a VM — but the one-time host prep below is on you. Do it once per Mac.

**1. Enable Remote Login (SSH)**

```sh
sudo systemsetup -setremotelogin on
```
(or System Settings → General → Sharing → **Remote Login**). If you scope Remote Login to specific users, make sure the service account from step 2 is included.

**2. Create a dedicated service account with passwordless sudo**

ephemerd needs a local user with passwordless `sudo` (it manages containerd/launchd and per-job VMs). A dedicated account keeps this off your personal login.

```sh
# create the user (no login password; SSH-key auth only)
sudo sysadminctl -addUser ephemerd -fullName "ephemerd CI"

# passwordless sudo
echo 'ephemerd ALL=(ALL) NOPASSWD: ALL' | sudo tee /etc/sudoers.d/ephemerd
sudo chmod 440 /etc/sudoers.d/ephemerd
sudo visudo -cf /etc/sudoers.d/ephemerd   # sanity-check syntax
```

**3. Install your SSH public key**

On your workstation, use a dedicated key (e.g. `ssh-keygen -t ed25519 -f ~/.ssh/mayfly_ed25519`), then authorize its **public** key on the Mac:

```sh
sudo -u ephemerd mkdir -p /Users/ephemerd/.ssh
sudo -u ephemerd tee /Users/ephemerd/.ssh/authorized_keys < mayfly_ed25519.pub
sudo -u ephemerd chmod 700 /Users/ephemerd/.ssh
sudo -u ephemerd chmod 600 /Users/ephemerd/.ssh/authorized_keys
```

Verify it works non-interactively before enrolling:

```sh
ssh -i ~/.ssh/mayfly_ed25519 -o BatchMode=yes ephemerd@<mac-host> 'sudo -n true && echo ok'
```

**4. Auto power-on after a power failure**

So the Mac rejoins the fleet on its own after an outage:

```sh
sudo pmset autorestart 1     # power on when AC returns
sudo pmset -a sleep 0        # optional: never sleep (a CI host shouldn't nap)
```
(GUI equivalent: System Settings → Energy → **Start up automatically after a power failure**.) Verify with `pmset -g | grep autorestart`.

**5. Enroll it**

Point a `darwin` pool at the host and `mayfly apply`:

```toml
[pools.mac-arm64]
hostname = "ci-mac-arm64.example.com"
os = "darwin"
members = ["mac-mini.lan"]
ssh_user = "ephemerd"
ssh_key = "~/.ssh/mayfly_ed25519"
```

Removing a host from `members` uninstalls ephemerd but otherwise leaves the machine untouched.

## Lending runners to a friend

A pool can serve **more than one GitHub owner** — so a friend can "borrow" your runners for their own org/repos. ephemerd manages the plumbing: in pool mode it *adopts-or-creates* the `workflow_job` webhook on each of their repos and points it at your tunnel. Your friend **never configures a webhook** — they just grant access and change their `runs-on`.

### What you need from your friend

**1. Access — one of:**

- **Install your GitHub App** (cleanest — self-service and revocable). They open your App's install link, pick which repos to share, approve. You get an `installation_id` for their account. Requires your App to be Public.
- **A fine-grained personal access token**, scoped to only the repos they're lending, with these repository permissions:

  ```text
  Administration   Read and write   # register/remove the self-hosted (JIT) runners
  Actions          Read and write   # read queued jobs, manage runs
  Webhooks         Read and write   # adopt/create the workflow_job webhook
  Contents         Read             # resolve the job's container image
  Metadata         Read             # mandatory baseline (auto-selected)
  ```

  Or a **classic PAT** with just:

  ```text
  repo
  workflow
  ```

  (classic `repo` already includes the admin, webhook, and contents access).

  > This token is **control-plane only** — ephemerd uses it to register runners, wire the webhook, and poll for jobs. The jobs themselves run with the ephemeral runner token GitHub mints per job, *not* this token. So they don't need to grant anything for what their CI actually does.

**2. Point their workflows at the runners:**

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]   # instead of ubuntu-latest
```

That's the whole ask on their side — no webhook setup, no runner registration, no secrets added to their repos.

### What you do (operator)

Add them as an extra GitHub target on the pool and `apply`:

```toml
[pools.linux-amd64]
hostname = "ci-linux-amd64.example.com"
repos = ["your", "repos"]

[[pools.linux-amd64.github_extra]]     # your friend, borrowing the runners
owner = "buddys-org"
installation_id = 987654321            # their install of your App
# token = "ghp_..."                    # ...or a PAT instead
repos = ["their-repo"]                 # omit = all their repos
```

`apply` reconciles the runner's ephemerd config; it starts polling/registering for their repos with that credential and their `self-hosted` jobs land in fresh ephemeral containers on your box.

### Gating dispatch (optional, recommended for public repos)

GitHub's **"require approval for all outside collaborators"** setting (Settings → Actions → General) is the primary control that stops a fork PR from executing on your runners. Turn it on first — nothing below replaces it.

Underneath it, ephemerd supports an allowlist over the webhooks that dispatch a runner. mayfly renders it from config:

```toml
[github.dispatch_policy]
required_labels = ["ephemerd"]          # beyond the mandatory "self-hosted"
allowed_repos = ["repo-a", "repo-b"]    # bare names, never "owner/repo"
```

Unset means no restriction. Setting both requires an allowed repo **and** at least one required label. The label is the useful half for a public repo: pick something your own workflows opt into (`runs-on: [self-hosted, linux, ephemerd]`) that a fork would have to guess.

Scope, tightest wins:

| Where | Effect |
|---|---|
| `[github.dispatch_policy]` | fleet-wide default |
| `[pools.<name>.dispatch_policy]` | merged per-field over the fleet default — a pool can tighten, never loosen |
| `[pools.<name>.github_extra.dispatch_policy]` | that borrower's own gate |

Extra targets are **separate providers inside ephemerd with their own policy**, so a borrowed target left unset dispatches freely even when your own repos are gated. mayfly passes `required_labels` down to them automatically; `allowed_repos` is never inherited, because ephemerd matches bare repo names scoped to the target's owner and another owner's list would match nothing and block every job. Give a borrower their own `allowed_repos` if you want one.

Trust runs both ways — tell them:
- **It's shared capacity.** Their jobs count against the pool's `max_concurrent` alongside yours.
- **You're running their code on your hardware** — ephemeral and container-isolated, but only lend to people you'd trust with that.
- **They're trusting you too.** As the runner host you can read each job's per-job `GITHUB_TOKEN` and any secrets the workflow decrypts, because the job runs in a container on your machine. The ephemeral-container isolation protects jobs from each other, not from the host. This is inherent to all self-hosted runners — only borrow/lend between people who trust each other with that.

## Commands

| Command | What it does |
|---|---|
| `mayfly plan` | Show what `apply` would change, without changing anything |
| `mayfly apply` | Reconcile the fleet to match `[pools.*]` in config |
| `mayfly destroy [pool]` | Destroy a pool, or the entire fleet |
| `mayfly node list` | List all pool members |
| `mayfly node status <node>` | Show ephemerd status on a node |
| `mayfly node logs <node>` | Tail a node's ephemerd logs (`-n` lines, `-f` follow) |
| `mayfly node doctor <node>` | Run a health + isolation battery on a node (PASS/FAIL per check) |
| `mayfly node drain <node>` | Gracefully drain a node |
| `mayfly node upgrade <node>` | Upgrade ephemerd on a node (`--dry-run` to see the plan) |
| `mayfly node reconfigure <node>` | Re-render one node's ephemerd config from its pool and restart ephemerd, without a full `apply` (`--no-restart` to stage only) |
| `mayfly node resize <node>` | Resize a node's CPU/memory/disk and power-cycle it (`--cores`/`--memory-mb`/`--disk-gb`; hypervisor nodes only, disk grow-only) |
| `mayfly node reboot <node>` | Gracefully reboot a node (drain, then power-cycle; waits for the VM to actually stop, hard-stopping it if needed, before starting). `--require-drain` refuses when the node cannot be drained (e.g. a dead guest agent) instead of interrupting running jobs |
| `mayfly node power {stop\|start\|reset} <node>` | Hard power control straight to the hypervisor — recovers a hung guest agent |
| `mayfly node exec <node> -- <cmd>` | Run a command inside a node |
| `mayfly node exec --script <file\|-> <node> [-- args]` | Run a local script (or stdin) on a node; its bytes are shipped as data, so no shell re-parses its quotes. Also reaches the `registry-cache` and `monitoring` VMs |

`node exec --script` reads the script itself and writes it into the guest as a
file (guest-agent file-write, chunked past ~32 KiB; over SSH it rides the
session's stdin), then runs it by path and removes it. Linux and macOS run it
with `sh`, or with its own interpreter if it starts with `#!`. Windows picks by
extension: `.cmd`/`.bat` run under `cmd.exe`, everything else (`.ps1`, other
names, stdin) under `powershell -File`. Use this instead of quoting a script
through PowerShell or Git Bash into `node exec -- sh -c '...'`.

Every `mayfly node` subcommand reaches the node the way that node's pool is
built — the QEMU guest agent for Proxmox VMs, SSH for enrolled bare metal — so
the same command works across the whole fleet.

Single-node commands (`node status`/`drain`/`upgrade`/`exec`) talk to one node
and authenticate to nothing, so they **do not require GitHub App or pool-token
credentials**. You can run them without exporting `GITHUB_TOKEN`, `EPHPM_APP_ID`,
or `EPHPM_INSTALLATION_ID`; the config's structure (and any Proxmox/transport
the target needs) is still validated. Only `plan`/`apply` — which register
runners and dispatch jobs — require the full set of pool credentials.
| `mayfly jobs <node>` | Show ephemerd jobs on a node |
| `mayfly webhooks list` / `prune` | Inspect and reconcile fleet webhooks across the org |
| `mayfly template sync` | Create the base Proxmox templates |

## Monitoring (optional)

Add a `[monitoring]` block and `mayfly apply` provisions a single long-lived VM running **Prometheus + Grafana**, turns on ephemerd's `/metrics` endpoint across the fleet, and auto-generates scrape targets from state (so new runners appear in Grafana as pools scale). Prometheus scrapes over the LAN — never through the tunnel — and Grafana ships with pre-built ephemerd dashboards on `:3000`.

That VM is the fleet's **utility host**: it is not a runner pool (no ephemerd, no webhook, never scales), and mayfly colocates other shared LAN services on it — currently the registry cache below.

## Registry pull-through cache (optional)

Every job pulls its runner image. On a busy pool that is the same image, over and over, off the internet — tens of GB a day of duplicated traffic, and a fast track to Docker Hub's pull-rate limit. Add a `[registry_cache]` block and mayfly runs a **pull-through cache** on the utility host and points every runner node at it:

```toml
[monitoring]
grafana_admin_password = "..."

[registry_cache]
enabled = true
```

That's the whole config. `mayfly apply` then:

- installs a stock `registry:2` in proxy mode on the utility host, as its own Docker Compose project (`/opt/registry-cache`), so it starts on boot and is independent of the monitoring stack;
- renders the mirror into each node's ephemerd `config.toml`, deriving the URL from the utility host's own LAN address — nothing about your addressing is baked into mayfly;
- keeps the cache **bounded**: the registry expires blobs not re-pulled within `ttl`, and a systemd timer drops the blob store outright if it ever exceeds `max_size_gb`. A pull-through cache holds nothing that can't be re-fetched, so the hard cap is safe — and an unbounded cache on a host that also stores your metrics is not.

| Key | Default | What it does |
|---|---|---|
| `enabled` | `false` | Turn the cache on |
| `port` | `5000` | LAN port the cache listens on |
| `upstream` | `https://registry-1.docker.io` | Registry being cached (one upstream per cache) |
| `max_size_gb` | `20` | Hard disk cap; must be less than the host's `disk_gb` |
| `ttl` | `"168h"` | Expire blobs not re-pulled within this window |
| `username` / `password` | — | Optional upstream credentials (lifts Docker Hub's anonymous limit) |
| `endpoint` | derived | Override the URL runners use (DNS name, https front end) |
| `fail_open` | `true` | Cache down → pull from upstream instead of failing the job |

The cache needs a `[monitoring]` section, since that is what declares the host. On the very first `apply` — the one that creates the utility host — the host has no address yet, so runner nodes pick up the mirror on the **next** apply; `mayfly plan` warns when this is the case. Setting `endpoint` explicitly skips the wait. Removing `enabled = true` later stops the cache and reclaims its disk on the next apply.

## Building blocks

mayfly is built on:

- [urfave/cli](https://github.com/urfave/cli) — the command surface
- [luthermonson/go-proxmox](https://github.com/luthermonson/go-proxmox) — Proxmox API + QEMU guest agent
- [ephemerd](https://github.com/ephpm/ephemerd) — the runner daemon each node runs
- Cloudflare tunnels for webhook ingress, cloud-init / Cloudbase-Init for first-boot provisioning, and a GitHub App for JIT runner registration

## Status

mayfly is a personal home-lab project, actively used to run CI across a small Proxmox cluster and a Mac mini. Expect sharp edges. Issues and PRs welcome.
