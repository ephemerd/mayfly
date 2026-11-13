package pve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// newTestClient wires a pve.Client to a mock Proxmox API served by h. The
// go-proxmox client uses the base URL verbatim (path = baseURL + "/nodes/..."),
// so the handler matches those raw paths.
func newTestClient(h http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(h)
	api := proxmox.NewClient(srv.URL, proxmox.WithAPIToken("id", "secret"))
	return &Client{API: api, Node: "node-a"}, srv
}

// mockPVE serves the two endpoints BridgeVLANAware hits: node status (the
// reachability probe) and the per-bridge network read. status==0 means the node
// answers normally; otherwise it fails the status call so the node reads as
// unreachable. bridges maps an interface name to the bridge_vlan_aware value it
// reports; an interface absent from the map returns HTTP 500, exactly as
// Proxmox does for a bridge that genuinely does not exist.
func mockPVE(statusCode int, bridges map[string]int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			if statusCode != 0 {
				w.WriteHeader(statusCode)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{}}`))
		case strings.Contains(r.URL.Path, "/network/"):
			parts := strings.Split(r.URL.Path, "/network/")
			iface := parts[len(parts)-1]
			aware, ok := bridges[iface]
			if !ok {
				// Proxmox returns a 500 ("interface 'x' does not exist") for a
				// missing bridge; go-proxmox surfaces that as a plain error.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			if aware == 1 {
				_, _ = w.Write([]byte(`{"data":{"bridge_vlan_aware":1}}`))
			} else {
				_, _ = w.Write([]byte(`{"data":{}}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// BridgeVLANAware is the safety linchpin: the ErrHostUnreachable sentinel must
// attach ONLY to a genuine reachability failure, never to the "bridge exists
// but is not VLAN-aware" verdict (which must be a clean false) and never to a
// "bridge genuinely absent" read failure (which must stay a HARD error so the
// preflight can't silently skip a misconfigured pool).
func TestBridgeVLANAwareSentinelScope(t *testing.T) {
	tests := []struct {
		name            string
		statusCode      int            // 0 = node answers; else status-call HTTP code
		bridges         map[string]int // iface -> bridge_vlan_aware
		bridge          string         // interface to query
		wantAware       bool
		wantErr         bool
		wantUnreachable bool // errors.Is(err, ErrHostUnreachable)
	}{
		{
			name:      "reachable vlan-aware bridge",
			bridges:   map[string]int{"vmbr0": 1},
			bridge:    "vmbr0",
			wantAware: true,
		},
		{
			name:      "reachable non-vlan-aware bridge is a clean false verdict, not unreachable",
			bridges:   map[string]int{"vmbr0": 0},
			bridge:    "vmbr0",
			wantAware: false,
			// aware==false with err==nil: a definite verdict, no sentinel.
		},
		{
			name:            "genuinely absent bridge is a HARD error, not unreachable",
			bridges:         map[string]int{"vmbr0": 1}, // vmbr9 not present
			bridge:          "vmbr9",
			wantErr:         true,
			wantUnreachable: false,
		},
		{
			name:            "unreachable node (status call fails) is flagged unreachable",
			statusCode:      http.StatusBadGateway,
			bridges:         map[string]int{"vmbr0": 1},
			bridge:          "vmbr0",
			wantErr:         true,
			wantUnreachable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, srv := newTestClient(mockPVE(tt.statusCode, tt.bridges))
			defer srv.Close()

			aware, err := c.BridgeVLANAware(context.Background(), "node-a", tt.bridge)
			if (err != nil) != tt.wantErr {
				t.Fatalf("BridgeVLANAware() err=%v, wantErr=%v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrHostUnreachable); got != tt.wantUnreachable {
				t.Fatalf("errors.Is(err, ErrHostUnreachable)=%v, want %v (err=%v)", got, tt.wantUnreachable, err)
			}
			if !tt.wantErr && aware != tt.wantAware {
				t.Fatalf("aware=%v, want %v", aware, tt.wantAware)
			}
		})
	}
}

// A node that is entirely unreachable (connection refused, not just an HTTP
// error body) must also flag ErrHostUnreachable — the real down-host case.
func TestBridgeVLANAwareConnectionRefused(t *testing.T) {
	c, srv := newTestClient(mockPVE(0, map[string]int{"vmbr0": 1}))
	srv.Close() // point the client at a dead listener

	_, err := c.BridgeVLANAware(context.Background(), "node-a", "vmbr0")
	if err == nil {
		t.Fatal("BridgeVLANAware() against a dead server = nil, want error")
	}
	if !errors.Is(err, ErrHostUnreachable) {
		t.Fatalf("connection-refused error not flagged unreachable: %v", err)
	}
}
