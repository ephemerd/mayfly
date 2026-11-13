# monitoring assets

Static config/asset files for the ephemerd Prometheus + Grafana monitoring
stack. These are embedded into a Go binary via `go:embed` and written onto a
single Ubuntu 24.04 VM by cloud-init, then started with Docker Compose.

## On-VM host layout (cloud-init must create exactly these)

| Host path | Type | Container mount | Notes |
|-----------|------|-----------------|-------|
| `/etc/prometheus/prometheus.yml` | file | `prometheus:/etc/prometheus/prometheus.yml` (ro) | from `prometheus.yml` |
| `/etc/prometheus/targets/` | dir | `prometheus:/etc/prometheus/targets` (ro) | file_sd dir; a Go program writes `runners.json` here |
| `/etc/grafana/provisioning/` | dir | `grafana:/etc/grafana/provisioning` (ro) | from `grafana/provisioning/` |
| `/var/lib/grafana-dashboards/` | dir | `grafana:/var/lib/grafana/dashboards` (ro) | from `grafana/dashboards/` |
| `<compose dir>/docker-compose.yml` | file | n/a | e.g. `/opt/monitoring/docker-compose.yml` |

Contents written by cloud-init from the embedded assets:

```
/etc/prometheus/prometheus.yml                                  <- prometheus.yml
/etc/grafana/provisioning/datasources/prometheus.yml            <- grafana/provisioning/datasources/prometheus.yml
/etc/grafana/provisioning/dashboards/dashboards.yml             <- grafana/provisioning/dashboards/dashboards.yml
/var/lib/grafana-dashboards/ephemerd-fleet.json                 <- grafana/dashboards/ephemerd-fleet.json
```

## Named Docker volumes (created by Compose, persistent)

| Volume | Container mount | Purpose |
|--------|-----------------|---------|
| `prometheus-data` | `prometheus:/prometheus` | TSDB (15d retention) |
| `grafana-data` | `grafana:/var/lib/grafana` | Grafana sqlite/state |

## Ports

| Publish | Service | Scope |
|---------|---------|-------|
| `127.0.0.1:9090` | Prometheus | localhost only (Grafana reaches it at `http://prometheus:9090` over the compose network) |
| `0.0.0.0:3000` | Grafana | LAN-accessible |

## Environment

`docker compose up` reads `GRAFANA_ADMIN_PASSWORD` from the shell/systemd
environment (defaults to `admin` if unset).

## Datasource / dashboard wiring

The provisioned Prometheus datasource has a fixed `uid: prometheus`. Every panel
and templating variable in `ephemerd-fleet.json` references
`{"type":"prometheus","uid":"prometheus"}`, so the dashboard imports cleanly with
no `__inputs`/`__requires` prompts.

## Target file format (written by the Go regenerator)

`/etc/prometheus/targets/runners.json` is standard Prometheus file_sd, e.g.:

```json
[
  {
    "targets": ["10.0.0.11:9090"],
    "labels": { "pool": "linux-amd64", "host": "runner-01", "os": "linux", "arch": "amd64" }
  }
]
```
