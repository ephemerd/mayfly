package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func sampleStore() *Store {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	return &Store{
		Pools: []Pool{
			{
				Name:     "linux-amd64",
				Hostname: "ci-linux-amd64.example.com",
				TunnelID: "tun-1",
				Token:    "tok-1",
				Secret:   "sec-1",

				Members: []Member{
					{VMID: 101, Hostname: "mfl-linux-amd64-101", PVENode: "pve1", Cores: 4, MemoryMB: 8192, DiskGB: 60, Template: "tmpl", VLANTag: 42, Firewall: true, ConfigHash: "hash-1", CreatedAt: now},
					{VMID: 102, Hostname: "mfl-linux-amd64-102", PVENode: "pve2", Cores: 4, MemoryMB: 8192, DiskGB: 60, Template: "tmpl", ConfigHash: "hash-1", CreatedAt: now},
				},
			},
			{
				Name:     "windows",
				Hostname: "ci-windows.example.com",
				TunnelID: "tun-2",
				Token:    "tok-2",
				Secret:   "sec-2",
			},
		},
	}
}

func TestPath(t *testing.T) {
	dir := setHome(t)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".mayfly", "state.yaml")
	if p != want {
		t.Fatalf("Path() = %q, want %q", p, want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	setHome(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pools) != 0 {
		t.Fatalf("expected empty store, got %+v", s)
	}
}

func TestRoundTrip(t *testing.T) {
	setHome(t)
	orig := sampleStore()
	if err := orig.Save(); err != nil {
		t.Fatal(err)
	}

	p, _ := Path()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("state file is not regular: %v", fi.Mode())
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(got.Pools))
	}
	pool, ok := got.FindPool("linux-amd64")
	if !ok {
		t.Fatal("FindPool(linux-amd64) not found")
	}
	if pool.TunnelID != "tun-1" || pool.Token != "tok-1" || pool.Secret != "sec-1" {
		t.Fatalf("pool fields lost in round-trip: %+v", pool)
	}
	if len(pool.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(pool.Members))
	}
	m, ok := pool.FindMember(102)
	if !ok {
		t.Fatal("FindMember(102) not found")
	}
	if m.Hostname != "mfl-linux-amd64-102" || m.PVENode != "pve2" || m.ConfigHash != "hash-1" ||
		m.Cores != 4 || m.MemoryMB != 8192 || m.DiskGB != 60 || m.Template != "tmpl" {
		t.Fatalf("member fields lost in round-trip: %+v", m)
	}
	if !m.CreatedAt.Equal(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("created_at lost: %v", m.CreatedAt)
	}
	// Member 102 was untagged with no firewall — the zero values must survive.
	if m.VLANTag != 0 || m.Firewall {
		t.Fatalf("untagged member gained NIC isolation in round-trip: %+v", m)
	}

	// Member 101 carries the VLAN/firewall isolation it was built with; without
	// this round-trip the planner can't tell a changed tag from the original
	// and a vlan_tag change plans nothing.
	m101, ok := pool.FindMember(101)
	if !ok {
		t.Fatal("FindMember(101) not found")
	}
	if m101.VLANTag != 42 || !m101.Firewall {
		t.Fatalf("vlan_tag/firewall lost in round-trip: got {vlan:%d fw:%v}, want {42 true}", m101.VLANTag, m101.Firewall)
	}
}

// TestMemberHandleBackfill proves the Handle rail is backward compatible:
// a member recorded with only a VMID (every state file written before Handle
// existed) comes back from Load with Handle == strconv.Itoa(VMID), and a
// member written WITH a Handle keeps it verbatim.
func TestMemberHandleBackfill(t *testing.T) {
	t.Run("legacy member without handle is backfilled from vmid", func(t *testing.T) {
		setHome(t)
		orig := &Store{Pools: []Pool{{
			Name:    "linux-amd64",
			Members: []Member{{VMID: 101, Hostname: "mfl-linux-amd64-101", PVENode: "pve1"}},
		}}}
		if err := orig.Save(); err != nil {
			t.Fatal(err)
		}
		// Handle is omitempty, so the file on disk must look exactly like a
		// pre-Handle state file: no "handle:" key at all.
		p, _ := Path()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "handle:") {
			t.Fatalf("empty Handle leaked into state file:\n%s", b)
		}

		got, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		pool, ok := got.FindPool("linux-amd64")
		if !ok {
			t.Fatal("FindPool(linux-amd64) not found")
		}
		m, ok := pool.FindMember(101)
		if !ok {
			t.Fatal("FindMember(101) not found")
		}
		if m.Handle != "101" {
			t.Fatalf("Handle = %q, want %q (backfilled from VMID)", m.Handle, "101")
		}
		if m.VMID != 101 {
			t.Fatalf("VMID changed by backfill: %d", m.VMID)
		}
	})

	t.Run("raw pre-handle yaml is backfilled", func(t *testing.T) {
		// A hand-written file in the exact shape mayfly wrote before Handle
		// existed, so the test does not depend on Save's own omitempty behavior.
		dir := setHome(t)
		p := filepath.Join(dir, ".mayfly", "state.yaml")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		legacy := "pools:\n" +
			"    - name: linux-amd64\n" +
			"      hostname: ci.example.com\n" +
			"      tunnel_id: tun-1\n" +
			"      token: tok-1\n" +
			"      secret: sec-1\n" +
			"      members:\n" +
			"        - vmid: 7\n" +
			"          hostname: mfl-linux-amd64-7\n" +
			"          pve_node: pve1\n" +
			"          os: linux\n" +
			"          arch: amd64\n" +
			"          cores: 2\n" +
			"          memory_mb: 2048\n" +
			"          disk_gb: 20\n" +
			"          template: tmpl\n" +
			"          config_hash: h\n" +
			"          created_at: 2026-07-11T12:00:00Z\n"
		if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Pools) != 1 || len(got.Pools[0].Members) != 1 {
			t.Fatalf("unexpected store shape: %+v", got)
		}
		m := got.Pools[0].Members[0]
		if m.VMID != 7 || m.Handle != "7" {
			t.Fatalf("got {vmid:%d handle:%q}, want {7 \"7\"}", m.VMID, m.Handle)
		}
	})

	t.Run("explicit handle round-trips unchanged", func(t *testing.T) {
		setHome(t)
		orig := &Store{Pools: []Pool{{
			Name: "linux-amd64",
			Members: []Member{
				{VMID: 101, Handle: "101", Hostname: "mfl-linux-amd64-101"},
				// A non-numeric handle (what a cloud backend would record) must
				// never be overwritten by the backfill.
				{VMID: 102, Handle: "i-0abc123", Hostname: "mfl-linux-amd64-102"},
			},
		}}}
		if err := orig.Save(); err != nil {
			t.Fatal(err)
		}
		got, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		pool, ok := got.FindPool("linux-amd64")
		if !ok {
			t.Fatal("FindPool(linux-amd64) not found")
		}
		m101, ok := pool.FindMember(101)
		if !ok {
			t.Fatal("FindMember(101) not found")
		}
		if m101.Handle != "101" {
			t.Fatalf("Handle = %q, want %q", m101.Handle, "101")
		}
		m102, ok := pool.FindMember(102)
		if !ok {
			t.Fatal("FindMember(102) not found")
		}
		if m102.Handle != "i-0abc123" {
			t.Fatalf("Handle = %q, want %q", m102.Handle, "i-0abc123")
		}
	})
}

func TestFindAndRemove(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, s *Store)
	}{
		{
			name: "FindPool missing",
			run: func(t *testing.T, s *Store) {
				if _, ok := s.FindPool("nope"); ok {
					t.Fatal("found nonexistent pool")
				}
			},
		},
		{
			name: "FindPool returns mutable pointer",
			run: func(t *testing.T, s *Store) {
				p, _ := s.FindPool("windows")
				p.Secret = "changed"
				p2, _ := s.FindPool("windows")
				if p2.Secret != "changed" {
					t.Fatal("FindPool did not return pointer into store")
				}
			},
		},
		{
			name: "RemovePool existing",
			run: func(t *testing.T, s *Store) {
				if !s.RemovePool("windows") {
					t.Fatal("RemovePool returned false")
				}
				if len(s.Pools) != 1 {
					t.Fatalf("got %d pools, want 1", len(s.Pools))
				}
				if s.RemovePool("windows") {
					t.Fatal("second RemovePool returned true")
				}
			},
		},
		{
			name: "FindMember missing",
			run: func(t *testing.T, s *Store) {
				p, _ := s.FindPool("linux-amd64")
				if _, ok := p.FindMember(999); ok {
					t.Fatal("found nonexistent member")
				}
			},
		},
		{
			name: "RemoveMember",
			run: func(t *testing.T, s *Store) {
				p, _ := s.FindPool("linux-amd64")
				if !p.RemoveMember(101) {
					t.Fatal("RemoveMember returned false")
				}
				if len(p.Members) != 1 || p.Members[0].VMID != 102 {
					t.Fatalf("unexpected members after remove: %+v", p.Members)
				}
				if p.RemoveMember(101) {
					t.Fatal("second RemoveMember returned true")
				}
			},
		},
		{
			name: "FindMemberByName hit",
			run: func(t *testing.T, s *Store) {
				pool, m, ok := s.FindMemberByName("mfl-linux-amd64-102")
				if !ok {
					t.Fatal("not found")
				}
				if pool.Name != "linux-amd64" || m.VMID != 102 {
					t.Fatalf("got pool %q vmid %d", pool.Name, m.VMID)
				}
			},
		},
		{
			name: "FindMemberByName miss",
			run: func(t *testing.T, s *Store) {
				if _, _, ok := s.FindMemberByName("nope"); ok {
					t.Fatal("found nonexistent member by name")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, sampleStore())
		})
	}
}
