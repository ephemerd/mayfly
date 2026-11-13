package cloudflared

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// certClient drives the cloudflared CLI, authenticated by the origin
// certificate from `cloudflared tunnel login` (~/.cloudflared/cert.pem).
// The cert authorizes tunnel lifecycle AND DNS route creation for the
// zone(s) it was issued for — no API token required.
type certClient struct {
	bin string
}

func newCertClient() (*certClient, error) {
	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		return nil, fmt.Errorf("no cloudflare api_token configured and cloudflared CLI not found on PATH (install it or run `cloudflared tunnel login`)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cert := filepath.Join(home, ".cloudflared", "cert.pem")
	if os.Getenv("TUNNEL_ORIGIN_CERT") != "" {
		cert = os.Getenv("TUNNEL_ORIGIN_CERT")
	}
	if _, err := os.Stat(cert); err != nil {
		return nil, fmt.Errorf("no cloudflare api_token configured and %s not found — run `cloudflared tunnel login`", cert)
	}
	return &certClient{bin: bin}, nil
}

// run returns STDOUT only — cloudflared writes version warnings and logs to
// stderr, and callers parse stdout (tokens, JSON). Stderr is folded into the
// error message on failure.
func (c *certClient) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("cloudflared %s: %w\n%s", strings.Join(args, " "),
			err, strings.TrimSpace(stderr.String()+"\n"+stdout.String()))
	}
	return stdout.String(), nil
}

var tunnelIDRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func (c *certClient) CreateTunnel(ctx context.Context, poolName string) (*Tunnel, error) {
	name := "mayfly-" + poolName
	out, err := c.run(ctx, "tunnel", "create", name)
	if err != nil {
		return nil, err
	}
	id := tunnelIDRe.FindString(out)
	if id == "" {
		return nil, fmt.Errorf("could not parse tunnel id from cloudflared output:\n%s", out)
	}

	token, err := c.run(ctx, "tunnel", "token", id)
	if err != nil {
		return nil, err
	}
	return &Tunnel{ID: id, Name: name, Token: strings.TrimSpace(token)}, nil
}

func (c *certClient) CreateDNSRoute(ctx context.Context, fqdn, tunnelID string) error {
	// Idempotent upstream: cloudflared errors on an existing record that it
	// doesn't own, but re-running against its own route succeeds.
	_, err := c.run(ctx, "tunnel", "route", "dns", tunnelID, fqdn)
	return err
}

func (c *certClient) DeleteTunnel(ctx context.Context, fqdn, tunnelID string) error {
	if tunnelID == "" {
		return nil
	}
	if _, err := c.run(ctx, "tunnel", "cleanup", tunnelID); err != nil {
		// Best-effort: stale connections block deletion; cleanup may noop.
		_ = err
	}
	if _, err := c.run(ctx, "tunnel", "delete", "-f", tunnelID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return err
	}
	// The cloudflared CLI has no command to remove a DNS route; the CNAME
	// for fqdn lingers in the zone. Harmless (points at a dead tunnel) but
	// surfaced so it can be cleaned in the dashboard.
	fmt.Fprintf(os.Stderr, "note: cloudflared CLI cannot delete DNS records — remove the %s CNAME in the Cloudflare dashboard if you care\n", fqdn)
	return nil
}

func (c *certClient) TunnelExists(ctx context.Context, tunnelID string) (bool, error) {
	out, err := c.run(ctx, "tunnel", "list", "--output", "json")
	if err != nil {
		return false, err
	}
	// Output may carry a version-warning line before/after the JSON.
	start := strings.Index(out, "[")
	end := strings.LastIndex(out, "]")
	if start < 0 || end < start {
		return false, fmt.Errorf("unexpected cloudflared tunnel list output:\n%s", out)
	}
	var tunnels []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &tunnels); err != nil {
		return false, fmt.Errorf("parse tunnel list: %w", err)
	}
	for _, t := range tunnels {
		if t.ID == tunnelID {
			return true, nil
		}
	}
	return false, nil
}
