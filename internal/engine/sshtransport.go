package engine

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// This file holds the auth and host-key policy for the single SSH transport.
//
// Everything goes through the embedded client. The system `ssh` binary was
// previously used for key auth and the embedded client only for passwords,
// which left the two paths with different host-key policies — and the weaker
// one (no verification at all) was the password path, i.e. the one used for
// first contact with an unprovisioned host. Host-key policy now lives in
// exactly one place: hostKeyCallback below.
//
// What the binary gave us for free and this does not: ~/.ssh/config Host
// aliases, ProxyJump, and per-host options. Agent support is preserved via
// SSH_AUTH_SOCK. If a bastion is ever needed, it goes here.

// defaultIdentities are tried, in order, when no ssh_key is configured and the
// agent has nothing — matching what openssh would attempt.
var defaultIdentities = []string{
	"~/.ssh/id_ed25519",
	"~/.ssh/id_ecdsa",
	"~/.ssh/id_rsa",
}

// authMethods resolves credentials most- to least-specific: an explicit key,
// then the agent, then the stock identity files, then a password.
//
// All available methods are offered rather than just the first that loads;
// sshd picks. A key that exists locally but is not in the target's
// authorized_keys should fall through to the password rather than failing the
// connection, which is precisely the bootstrap case.
func authMethods(t SSHTarget) ([]gossh.AuthMethod, error) {
	var methods []gossh.AuthMethod

	if t.KeyPath != "" {
		signer, err := loadSigner(expandHome(t.KeyPath))
		if err != nil {
			return nil, err
		}
		methods = append(methods, gossh.PublicKeys(signer))
	} else {
		if a := agentSigners(); a != nil {
			methods = append(methods, a)
		}
		var signers []gossh.Signer
		for _, id := range defaultIdentities {
			if s, err := loadSigner(expandHome(id)); err == nil {
				signers = append(signers, s)
			}
		}
		if len(signers) > 0 {
			methods = append(methods, gossh.PublicKeys(signers...))
		}
	}

	if t.Password != "" {
		methods = append(methods, gossh.Password(t.Password))
		// Some sshd configs advertise only keyboard-interactive for passwords.
		methods = append(methods, gossh.KeyboardInteractive(
			func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range qs {
					ans[i] = t.Password
				}
				return ans, nil
			}))
	}

	if len(methods) == 0 {
		return nil, errors.New("no ssh credential available: set ssh_key or ssh_password, " +
			"or run an ssh-agent")
	}
	return methods, nil
}

// agentSigners connects to a running ssh-agent, or returns nil.
func agentSigners() gossh.AuthMethod {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	return gossh.PublicKeysCallback(agent.NewClient(conn).Signers)
}

func loadSigner(path string) (gossh.Signer, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading ssh key %s: %w", path, err)
	}
	signer, err := gossh.ParsePrivateKey(buf)
	if err != nil {
		var pm *gossh.PassphraseMissingError
		if errors.As(err, &pm) {
			return nil, fmt.Errorf("ssh key %s is passphrase-protected and mayfly cannot prompt; "+
				"add it to an ssh-agent or use an unencrypted key", path)
		}
		return nil, fmt.Errorf("parsing ssh key %s: %w", path, err)
	}
	return signer, nil
}

// knownHostsPath is where TOFU entries are recorded. Deliberately the standard
// user file, so a host mayfly has seen is a host `ssh` also trusts, and vice
// versa — two files would mean two answers to "have we seen this host".
//
// Resolved via os.UserHomeDir rather than expandHome("~/..."): on Windows
// filepath.Join("~", ".ssh") yields `~\.ssh`, which expandHome's "~/" prefix
// test does not match, silently producing a path relative to the working
// directory. That writes host keys into whatever directory mayfly happens to
// be run from — and, worse, reads them back from there, so verification would
// depend on cwd.
func knownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".ssh", "known_hosts")
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

var knownHostsMu sync.Mutex

// hostKeyCallback verifies against ~/.ssh/known_hosts, recording an unseen
// host on first contact.
//
// This is trust-on-first-use, matching StrictHostKeyChecking=accept-new — the
// policy the system-ssh path already had. It is a real improvement only over
// the old password path, which used InsecureIgnoreHostKey and therefore
// verified nothing at all.
//
// A CHANGED key is a hard error, never an auto-update: that is the case TOFU
// exists to catch. Resolving it is a deliberate `ssh-keygen -R <host>`.
func hostKeyCallback() gossh.HostKeyCallback {
	path := knownHostsPath()

	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()

		// Re-read per call: cheap, and it keeps a long-running apply honest if
		// the file changes underneath it.
		checker, err := knownhosts.New(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			// No file yet — first host mayfly has ever touched.
			return appendKnownHost(path, hostname, remote, key)
		}

		err = checker(hostname, remote, key)
		if err == nil {
			return nil
		}

		var kerr *knownhosts.KeyError
		if errors.As(err, &kerr) && len(kerr.Want) == 0 {
			// Unknown host (no entry at all) — TOFU.
			return appendKnownHost(path, hostname, remote, key)
		}
		// Known host presenting a different key, or a revoked key.
		return fmt.Errorf("host key verification failed for %s: %w\n"+
			"if this host was legitimately rebuilt, run: ssh-keygen -R %s",
			hostname, err, knownhosts.Normalize(hostname))
	}
}

func appendKnownHost(path, hostname string, remote net.Addr, key gossh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Record both the name we dialed and the resolved address, so a later
	// connection by either does not re-prompt.
	addrs := []string{knownhosts.Normalize(hostname)}
	if remote != nil {
		if ra := knownhosts.Normalize(remote.String()); ra != addrs[0] {
			addrs = append(addrs, ra)
		}
	}
	line := knownhosts.Line(addrs, key)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
