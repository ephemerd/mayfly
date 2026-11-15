package cloudflared

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"

	"github.com/luthermonson/mayfly/internal/config"
)

// Provider provisions pool-scoped Cloudflare resources: one tunnel + one DNS
// route per pool. The tunnel run token is handed to ephemerd, which manages
// the cloudflared subprocess itself on each pool member.
//
// Two implementations: the API client (api_token + account_id + zone_id in
// config) and the cert client (shells out to the cloudflared CLI using the
// origin certificate from `cloudflared tunnel login` — no API token needed).
type Provider interface {
	CreateTunnel(ctx context.Context, poolName string) (*Tunnel, error)
	CreateDNSRoute(ctx context.Context, fqdn, tunnelID string) error
	DeleteTunnel(ctx context.Context, fqdn, tunnelID string) error
	TunnelExists(ctx context.Context, tunnelID string) (bool, error)
}

// Client is the API-token implementation of Provider.
type Client struct {
	api       *cloudflare.Client
	accountID string
	zoneID    string
}

type Tunnel struct {
	ID    string
	Name  string
	Token string // input to ephemerd's webhook.cloudflared_token
}

// New picks the implementation: explicit API credentials win; otherwise fall
// back to the cloudflared CLI + origin cert.
func New(cfg config.Cloudflare) (Provider, error) {
	if cfg.APIToken != "" && cfg.APIToken != "CHANGE-ME" {
		return &Client{
			api:       cloudflare.NewClient(option.WithAPIToken(cfg.APIToken)),
			accountID: cfg.AccountID,
			zoneID:    cfg.ZoneID,
		}, nil
	}
	return newCertClient()
}

// CreateTunnel provisions a locally-managed cfd_tunnel named "mayfly-<pool>"
// and returns its run token.
func (c *Client) CreateTunnel(ctx context.Context, poolName string) (*Tunnel, error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("generate tunnel secret: %w", err)
	}

	name := "mayfly-" + poolName
	resp, err := c.api.ZeroTrust.Tunnels.Cloudflared.New(ctx, zero_trust.TunnelCloudflaredNewParams{
		AccountID:    cloudflare.F(c.accountID),
		Name:         cloudflare.F(name),
		ConfigSrc:    cloudflare.F(zero_trust.TunnelCloudflaredNewParamsConfigSrcLocal),
		TunnelSecret: cloudflare.F(base64.StdEncoding.EncodeToString(secretBytes)),
	})
	if err != nil {
		return nil, fmt.Errorf("create tunnel %s: %w", name, err)
	}
	if resp.ID == "" {
		return nil, errors.New("cloudflare returned empty tunnel id")
	}

	token, err := c.api.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, resp.ID, zero_trust.TunnelCloudflaredTokenGetParams{
		AccountID: cloudflare.F(c.accountID),
	})
	if err != nil {
		return nil, fmt.Errorf("fetch tunnel token: %w", err)
	}

	return &Tunnel{ID: resp.ID, Name: name, Token: *token}, nil
}

// TunnelExists reports whether the tunnel is still live. Cloudflare
// soft-deletes tunnels (DeletedAt set, Get still succeeds), so both a 404
// and a set deletion timestamp count as gone.
func (c *Client) TunnelExists(ctx context.Context, tunnelID string) (bool, error) {
	resp, err := c.api.ZeroTrust.Tunnels.Cloudflared.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredGetParams{
		AccountID: cloudflare.F(c.accountID),
	})
	if err != nil {
		var apiErr *cloudflare.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("get tunnel %s: %w", tunnelID, err)
	}
	if resp == nil || !resp.DeletedAt.IsZero() {
		return false, nil
	}
	return true, nil
}

// CreateDNSRoute publishes a proxied CNAME fqdn -> <tunnel-id>.cfargotunnel.com.
// Idempotent: an existing record for fqdn is left alone.
func (c *Client) CreateDNSRoute(ctx context.Context, fqdn, tunnelID string) error {
	existing, err := c.api.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(c.zoneID),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(fqdn)}),
	})
	if err != nil {
		return fmt.Errorf("list existing records for %s: %w", fqdn, err)
	}
	if existing != nil && len(existing.Result) > 0 {
		return nil
	}

	target := tunnelID + ".cfargotunnel.com"
	_, err = c.api.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.F(c.zoneID),
		Body: dns.CNAMERecordParam{
			Name:    cloudflare.F(fqdn),
			Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
			Content: cloudflare.F(target),
			TTL:     cloudflare.F(dns.TTL(1)),
			Proxied: cloudflare.F(true),
			Comment: cloudflare.F("mayfly: tunnel " + tunnelID),
		},
	})
	if err != nil {
		return fmt.Errorf("create CNAME %s -> %s: %w", fqdn, target, err)
	}
	return nil
}

// DeleteTunnel removes the DNS record for fqdn and the tunnel itself.
// Missing resources are treated as success (idempotent teardown).
func (c *Client) DeleteTunnel(ctx context.Context, fqdn, tunnelID string) error {
	list, err := c.api.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(c.zoneID),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(fqdn)}),
	})
	if err != nil {
		return fmt.Errorf("list %s: %w", fqdn, err)
	}
	if list != nil {
		for _, r := range list.Result {
			if _, err := c.api.DNS.Records.Delete(ctx, r.ID, dns.RecordDeleteParams{
				ZoneID: cloudflare.F(c.zoneID),
			}); err != nil {
				return fmt.Errorf("delete DNS record %s: %w", r.ID, err)
			}
		}
	}

	if tunnelID != "" {
		if _, err := c.api.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, tunnelID, zero_trust.TunnelCloudflaredDeleteParams{
			AccountID: cloudflare.F(c.accountID),
		}); err != nil {
			return fmt.Errorf("delete tunnel %s: %w", tunnelID, err)
		}
	}
	return nil
}
