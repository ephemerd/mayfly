package pve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
)

// versionHandler answers go-proxmox's Client.Version (GET /version) with a
// minimal valid payload, standing in for a healthy cluster node.
func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"version":"8.0.0","release":"8.0","repoid":"test"}}`))
	}
}

// deadEndpointURL returns the URL of an httptest server that has already been
// closed, so any connection to it is refused — a stand-in for a down PVE node.
func deadEndpointURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// TestNewFailsOverToAdditionalEndpoint proves New wires the v0.9.0
// WithAdditionalEndpoints failover when Endpoints is set: a request to a down
// primary is answered by a healthy additional endpoint.
func TestNewFailsOverToAdditionalEndpoint(t *testing.T) {
	good := httptest.NewServer(versionHandler())
	defer good.Close()

	c := New(config.Proxmox{
		URL:       deadEndpointURL(t), // primary refuses
		Endpoints: []string{good.URL}, // healthy failover target
		TokenID:   "id",
		Secret:    "secret",
		Node:      "node-a",
	})

	v, err := c.API.Version(context.Background())
	if err != nil {
		t.Fatalf("Version with a down primary + healthy endpoint: got error %v, want success via failover", err)
	}
	if v.Version != "8.0.0" {
		t.Errorf("version = %q, want %q", v.Version, "8.0.0")
	}
}

// TestNewWithoutEndpointsDoesNotFailOver proves the option is only added when
// Endpoints is configured: with no additional endpoints a down primary simply
// fails, i.e. the single-endpoint behavior is unchanged.
func TestNewWithoutEndpointsDoesNotFailOver(t *testing.T) {
	c := New(config.Proxmox{
		URL:     deadEndpointURL(t),
		TokenID: "id",
		Secret:  "secret",
		Node:    "node-a",
	})

	if _, err := c.API.Version(context.Background()); err == nil {
		t.Fatal("Version against a down primary with no endpoints: got nil error, want a transport failure")
	}
}
