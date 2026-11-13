#!/bin/sh
# actions-cache-gc — reclaim disk on the writable Actions cache registry.
#
# Run weekly by actions-cache-gc.timer. ephemerd evicts cache entries by
# deleting their manifests; the blobs those manifests referenced stay on disk
# until a garbage collection removes them. registry:2 can only collect safely
# while nothing writes, so the registry is stopped for the run — a few minutes
# during which ephemerd's shim reports cache misses and jobs build cold. No job
# fails because of it.
#
# After collecting, the same hard cap backstop as the pull-through cache: if
# the store is still over ACTIONS_CACHE_MAX_GB the whole store is dropped. An
# Actions cache is a cache — every entry is rebuildable by the job that made
# it — so a cold start is always safe.
#
# THE REGISTRY ALWAYS COMES BACK UP. Once it has been stopped, the EXIT trap
# restarts it however this script ends — a failed gc, a failed rm, a failed
# du, or a signal. Under `set -eu` any failing step would otherwise exit
# straight past the restart and leave the cache down until next week's run.
#
# Env (from /opt/actions-cache/.env via the unit's EnvironmentFile):
#   ACTIONS_CACHE_MAX_GB  cap in GiB (default 150)
#   ACTIONS_CACHE_DATA    blob store path (default /var/lib/actions-cache)
#   ACTIONS_CACHE_DIR     compose project dir (default /opt/actions-cache)

set -eu

max_gb="${ACTIONS_CACHE_MAX_GB:-150}"
data_dir="${ACTIONS_CACHE_DATA:-/var/lib/actions-cache}"
project_dir="${ACTIONS_CACHE_DIR:-/opt/actions-cache}"

log() { logger -t actions-cache-gc "$@" || true; echo "$@"; }
size_mib() { du -sm "$data_dir" 2>/dev/null | awk '{print $1}' || true; }

[ -f "$project_dir/docker-compose.yml" ] || exit 0
cd "$project_dir"

restart() {
	status=$?
	trap - EXIT INT TERM
	if ! docker compose up -d actions-cache; then
		log "FAILED to restart the actions-cache registry; it is down until fixed"
		exit 1
	fi
	exit "$status"
}

before="$(size_mib)"
# Armed before the stop: a stop that half-fails must still be followed by an up.
trap restart EXIT
trap 'exit 130' INT TERM
docker compose stop actions-cache

# A one-off container from the same service definition: same image, same
# storage mount, no published port.
if ! docker compose run --rm --no-deps actions-cache \
	garbage-collect --delete-untagged=true /etc/docker/registry/config.yml; then
	log "garbage-collect failed; restarting the registry unchanged"
fi

used="$(size_mib)"
if [ -n "$used" ] && [ "$used" -gt $((max_gb * 1024)) ]; then
	log "store is ${used}MiB after gc, over the ${max_gb}GiB cap — dropping it"
	rm -rf "${data_dir:?}/docker"
fi

log "gc done: ${before:-?}MiB -> $(size_mib)MiB"
# restart() runs on the way out.
