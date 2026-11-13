# registry cache assets

Static assets for the container registry **pull-through cache** that runs on
the fleet's utility host — the same VM `[monitoring]` provisions for Prometheus
and Grafana. Embedded into the mayfly binary via `go:embed` and written onto
the VM by cloud-init (first boot) or the guest agent (subsequent applies).

The cache is a stock `registry:2` in proxy mode. It is its own Compose project,
so its lifecycle is independent of the monitoring stack.

## On-VM layout

| Host path | Mode | Purpose |
|-----------|------|---------|
| `/opt/registry-cache/docker-compose.yml` | 0644 | from `docker-compose.yml` |
| `/opt/registry-cache/.env` | 0600 | rendered by mayfly (may carry upstream creds) |
| `/var/lib/registry-cache/` | 0755 | blob store, bind-mounted at `/var/lib/registry` |
| `/usr/local/sbin/registry-cache-prune` | 0755 | from `prune.sh` |
| `/etc/systemd/system/registry-cache-prune.service` | 0644 | oneshot that runs the script |
| `/etc/systemd/system/registry-cache-prune.timer` | 0644 | every 15 min |

## Ports

| Publish | Service | Scope |
|---------|---------|-------|
| `0.0.0.0:${REGISTRY_PORT}` (default 5000) | registry proxy | LAN — every runner node pulls through it |

## Bounding the cache

Two mechanisms, deliberately:

1. **`REGISTRY_PROXY_TTL`** — the registry expires blobs that have not been
   re-pulled within the TTL. Continuous, cheap, no restarts. Requires
   `REGISTRY_STORAGE_DELETE_ENABLED=true`, which the compose file sets.
2. **`registry-cache-prune`** — a timer measures the blob store and, if it is
   over `REGISTRY_CACHE_MAX_GB`, stops the registry, deletes the store, and
   starts it cold.

The second is blunt on purpose. Every byte here is re-fetchable from upstream,
and wholesale deletion is the only eviction that cannot leave the registry
referencing blobs that no longer exist. In steady state the TTL keeps the cache
well under the cap and the timer never fires.

## Environment (`.env`, written by mayfly)

| Key | From config |
|-----|-------------|
| `REGISTRY_PORT` | `registry_cache.port` |
| `REGISTRY_UPSTREAM` | `registry_cache.upstream` |
| `REGISTRY_TTL` | `registry_cache.ttl` |
| `REGISTRY_UPSTREAM_USERNAME` / `_PASSWORD` | `registry_cache.username` / `password` |
| `REGISTRY_CACHE_MAX_GB` | `registry_cache.max_size_gb` |
| `REGISTRY_CACHE_DATA` / `REGISTRY_CACHE_DIR` | fixed paths, for the prune script |

## Operating

```sh
# is it serving?
curl -s http://<utility-host>:5000/v2/_catalog

# how big is it?
du -sh /var/lib/registry-cache

# force a size check now
systemctl start registry-cache-prune.service

# logs
docker logs registry-cache
journalctl -u registry-cache-prune
```

## Actions cache registry (dedicated cache VM only)

With `[registry_cache.actions]` enabled, the cache VM also runs a second,
**writable** `registry:2` (`assets/actions/`, Compose project
`/opt/actions-cache`, blobs in `/var/lib/actions-cache`, port 5001). It backs
ephemerd's LAN GitHub Actions cache (ephemerd #215): ephemerd stores each cache
entry as an OCI artifact and evicts per repo by deleting manifests.

- Authenticated with htpasswd (bcrypt, written by mayfly). Only ephemerd holds
  the credential; jobs never do.
- `actions-cache-gc.timer` runs weekly: stops the registry, runs
  `registry garbage-collect --delete-untagged=true`, drops the store if it is
  still over `max_size_gb`, and restarts. Jobs miss the cache for those minutes.

```sh
curl -s -u ephemerd:<password> http://<cache-vm>:5001/v2/_catalog
du -sh /var/lib/actions-cache
systemctl start actions-cache-gc.service
journalctl -u actions-cache-gc
```
