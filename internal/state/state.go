package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Store struct {
	Pools      []Pool      `yaml:"pools"`
	Monitoring *Monitoring `yaml:"monitoring,omitempty"`
	// RegistryCache records the dedicated pull-through cache VM. Nil when the
	// cache is off or colocated on the monitoring host, which is where it
	// lived before [registry_cache.vm] existed — so an untouched fleet keeps
	// exactly the state file it had.
	RegistryCache *RegistryCache `yaml:"registry_cache,omitempty"`
	// ModelServer records the GPU model-server VM. Nil when [model_server]
	// is off.
	ModelServer *ModelServer `yaml:"model_server,omitempty"`
}

// ModelServer records the model-server VM: Ollama on a VM that holds a
// passed-through GPU.
type ModelServer struct {
	VMID     int    `yaml:"vmid"`
	Hostname string `yaml:"hostname"`
	PVENode  string `yaml:"pve_node"`
	// IP is the address the fleet reaches the server on — on the runners'
	// VLAN when the VM is tagged onto it.
	IP       string `yaml:"ip"`
	Cores    int    `yaml:"cores"`
	MemoryMB int    `yaml:"memory_mb"`
	DiskGB   int    `yaml:"disk_gb"`
	Template string `yaml:"template"`
	VLANTag  int    `yaml:"vlan_tag,omitempty"`
	// HostPCI is the passthrough set the VM was created with. A change to
	// it replaces the VM rather than editing it in place: re-cabling a GPU
	// into a running guest is not something to do quietly.
	HostPCI    []string  `yaml:"hostpci,omitempty"`
	ConfigHash string    `yaml:"config_hash"` // fingerprint of the server's setup assets + spec
	CreatedAt  time.Time `yaml:"created_at"`
}

// RegistryCache records the dedicated cache VM: the container-registry
// pull-through cache running on a VM of its own rather than on the utility
// host. It exists so the cache can sit on the runners' VLAN while monitoring
// stays on the LAN.
type RegistryCache struct {
	VMID     int    `yaml:"vmid"`
	Hostname string `yaml:"hostname"`
	PVENode  string `yaml:"pve_node"`
	// IP is the address runner nodes are pointed at. On a VLAN-tagged cache VM
	// this is an address on the runners' VLAN, not the LAN.
	IP         string    `yaml:"ip"`
	Cores      int       `yaml:"cores"`
	MemoryMB   int       `yaml:"memory_mb"`
	DiskGB     int       `yaml:"disk_gb"`
	Template   string    `yaml:"template"`
	VLANTag    int       `yaml:"vlan_tag,omitempty"`
	// VMStorage is the storage the VM's disk lives on when [registry_cache.vm]
	// vm_storage moved it; empty = where the clone put it.
	VMStorage  string    `yaml:"vm_storage,omitempty"`
	ConfigHash string    `yaml:"config_hash"` // fingerprint of the cache stack + spec
	CreatedAt  time.Time `yaml:"created_at"`
}

// Monitoring records the single Prometheus + Grafana node. Nil when no
// monitoring node is provisioned.
type Monitoring struct {
	VMID       int       `yaml:"vmid"`
	Hostname   string    `yaml:"hostname"`
	PVENode    string    `yaml:"pve_node"`
	IP         string    `yaml:"ip"` // LAN IPv4 (for reference; Grafana on :3000)
	Cores      int       `yaml:"cores"`
	MemoryMB   int       `yaml:"memory_mb"`
	DiskGB     int       `yaml:"disk_gb"`
	Template   string    `yaml:"template"`
	TunnelID   string    `yaml:"tunnel_id,omitempty"` // set when Grafana is exposed via cloudflared
	Token      string    `yaml:"token,omitempty"`
	ConfigHash string    `yaml:"config_hash"` // fingerprint of the stack assets + spec
	TargetHash string    `yaml:"target_hash"` // fingerprint of the scrape target set
	CreatedAt  time.Time `yaml:"created_at"`
}

type Pool struct {
	Name     string `yaml:"name"`
	Hostname string `yaml:"hostname"`
	TunnelID string `yaml:"tunnel_id"`
	Token    string `yaml:"token"`
	Secret   string `yaml:"secret"`
	// Darwin (SSH-enrolled) pool transport settings. The password lives
	// here alongside the tunnel token and webhook secret — the state file
	// is 0600 and already credential-bearing — so a pool removed from the
	// config can still be unenrolled.
	SSHUser     string   `yaml:"ssh_user,omitempty"`
	SSHKey      string   `yaml:"ssh_key,omitempty"`
	SSHPort     int      `yaml:"ssh_port,omitempty"`
	SSHPassword string   `yaml:"ssh_password,omitempty"`
	Members     []Member `yaml:"members"`
}

// FindMemberByHost locates a bare-metal member by its SSH host (stored in
// Hostname; darwin members have no VMID).
func (p *Pool) FindMemberByHost(host string) (*Member, bool) {
	for i := range p.Members {
		if p.Members[i].Hostname == host {
			return &p.Members[i], true
		}
	}
	return nil, false
}

// RemoveMemberByHost removes a bare-metal member by SSH host.
func (p *Pool) RemoveMemberByHost(host string) bool {
	for i := range p.Members {
		if p.Members[i].Hostname == host {
			p.Members = append(p.Members[:i], p.Members[i+1:]...)
			return true
		}
	}
	return false
}

// Member records what each VM was actually built with. VM-level fields are
// stored as plain values (not hashes) so the state file stays readable and
// the planner can print precise transitions like "cores 4 → 8".
type Member struct {
	VMID int `yaml:"vmid"`
	// Handle is the backend's opaque instance identity (Proxmox: the decimal
	// VMID as a string; cloud backends: their native id). Empty in state files
	// written before this field; Load backfills it from VMID so legacy state
	// keeps working unchanged.
	Handle   string `yaml:"handle,omitempty"`
	Hostname string `yaml:"hostname"`
	PVENode  string `yaml:"pve_node"`
	IP       string `yaml:"ip,omitempty"` // LAN IPv4 (guest-agent discovered); scrape target
	OS       string `yaml:"os"`
	Arch     string `yaml:"arch"`
	Cores    int    `yaml:"cores"`
	MemoryMB int    `yaml:"memory_mb"`
	DiskGB   int    `yaml:"disk_gb"`
	Template string `yaml:"template"`
	// VLANTag / Firewall record the runner vNIC isolation this VM was actually
	// built with (Proxmox net0 "tag=<n>" / "firewall=1"). They are NIC
	// properties written at clone time, so a change to the pool's vlan_tag or
	// firewall forces a member REPLACE — the planner compares these against the
	// desired spec. Zero values = the historical untagged, no-firewall NIC, so
	// members provisioned before this field existed compare equal to an
	// unconfigured pool and never drift spuriously.
	VLANTag  int  `yaml:"vlan_tag,omitempty"`
	Firewall bool `yaml:"firewall,omitempty"`
	// VMStorage records the storage the VM disk was actually placed on. It is a
	// clone-time property (empty = the historical linked clone on the template's
	// storage; non-empty = a full clone onto that storage), so a change to the
	// pool's vm_storage forces a member REPLACE — the planner compares this
	// against the desired spec, exactly like VLANTag. Empty for members
	// provisioned before this field existed, which compare equal to an
	// unconfigured (linked-clone) pool and never drift spuriously.
	VMStorage string `yaml:"vm_storage,omitempty"`
	// ConfigHash fingerprints the files mayfly delivers to the node: the
	// rendered ephemerd config and the GitHub App PEM. Hashes written before
	// the binary was tracked separately (16 bare hex characters) also folded
	// in the [ephemerd] binary selection; the planner still recognises those.
	ConfigHash string `yaml:"config_hash"`
	// Ephemerd is the ephemerd binary this member is known to run: a release
	// tag ("v0.2.20"), or "url:<binary_url>" for a pinned artifact. It is
	// tracked apart from ConfigHash because the two drift for different
	// reasons and are fixed by different actions: a version bump is an
	// upgrade, a config change is a config push + restart. Empty means
	// unknown (state written before the field existed, until a refresh reads
	// the live version off the node).
	Ephemerd  string    `yaml:"ephemerd,omitempty"`
	CreatedAt time.Time `yaml:"created_at"`
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mayfly", "state.yaml"), nil
}

func Load() (*Store, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s Store
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	// Backfill Handle for members recorded before the field existed, so every
	// loaded member carries both identities regardless of state-file vintage.
	for i := range s.Pools {
		for j := range s.Pools[i].Members {
			if m := &s.Pools[i].Members[j]; m.Handle == "" {
				m.Handle = strconv.Itoa(m.VMID)
			}
		}
	}
	return &s, nil
}

func (s *Store) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	sort.Slice(s.Pools, func(i, j int) bool { return s.Pools[i].Name < s.Pools[j].Name })
	for i := range s.Pools {
		m := s.Pools[i].Members
		sort.Slice(m, func(a, b int) bool { return m[a].VMID < m[b].VMID })
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) FindPool(name string) (*Pool, bool) {
	for i := range s.Pools {
		if s.Pools[i].Name == name {
			return &s.Pools[i], true
		}
	}
	return nil, false
}

func (s *Store) RemovePool(name string) bool {
	for i := range s.Pools {
		if s.Pools[i].Name == name {
			s.Pools = append(s.Pools[:i], s.Pools[i+1:]...)
			return true
		}
	}
	return false
}

func (p *Pool) FindMember(vmid int) (*Member, bool) {
	for i := range p.Members {
		if p.Members[i].VMID == vmid {
			return &p.Members[i], true
		}
	}
	return nil, false
}

func (p *Pool) RemoveMember(vmid int) bool {
	for i := range p.Members {
		if p.Members[i].VMID == vmid {
			p.Members = append(p.Members[:i], p.Members[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Store) FindMemberByName(hostname string) (*Pool, *Member, bool) {
	for i := range s.Pools {
		for j := range s.Pools[i].Members {
			if s.Pools[i].Members[j].Hostname == hostname {
				return &s.Pools[i], &s.Pools[i].Members[j], true
			}
		}
	}
	return nil, nil, false
}
