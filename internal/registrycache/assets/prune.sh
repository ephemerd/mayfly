#!/bin/sh
# registry-cache-prune — keep the pull-through cache under its disk cap.
#
# Run periodically by registry-cache-prune.timer. The registry's own TTL
# expiry is the gentle bound; this is the backstop that guarantees the cache
# can never fill the utility host's disk.
#
# When the blob store exceeds REGISTRY_CACHE_MAX_GB the whole store is dropped
# and the registry restarts cold. That is safe by construction: every byte in
# a pull-through cache is re-fetchable from upstream, and it is the only
# eviction that cannot leave the registry referencing blobs that are gone.
#
# Env (from /opt/registry-cache/.env via the unit's EnvironmentFile):
#   REGISTRY_CACHE_MAX_GB  cap in GiB (default 20)
#   REGISTRY_CACHE_DATA    blob store path (default /var/lib/registry-cache)
#   REGISTRY_CACHE_DIR     compose project dir (default /opt/registry-cache)

set -eu

max_gb="${REGISTRY_CACHE_MAX_GB:-20}"
data_dir="${REGISTRY_CACHE_DATA:-/var/lib/registry-cache}"
project_dir="${REGISTRY_CACHE_DIR:-/opt/registry-cache}"

log() { logger -t registry-cache-prune "$@"; echo "$@"; }

[ -d "$data_dir" ] || exit 0

max_kb=$((max_gb * 1024 * 1024))
used_kb=$(du -sk "$data_dir" 2>/dev/null | awk '{print $1}')
[ -n "${used_kb:-}" ] || exit 0

if [ "$used_kb" -le "$max_kb" ]; then
	exit 0
fi

log "cache is $((used_kb / 1024))MiB, over the ${max_gb}GiB cap — dropping blob store"

# Stop the registry before touching its storage so nothing is mid-write, then
# delete only the registry's own subtree (never the mount point itself).
if [ -f "$project_dir/docker-compose.yml" ]; then
	(cd "$project_dir" && docker compose stop registry-cache) || true
fi
rm -rf "${data_dir:?}/docker"
if [ -f "$project_dir/docker-compose.yml" ]; then
	(cd "$project_dir" && docker compose up -d)
fi

log "cache reset; registry restarted cold"
