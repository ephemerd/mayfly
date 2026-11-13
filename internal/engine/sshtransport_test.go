package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// isolateHome points os.UserHomeDir at a temp dir so known_hosts writes stay
// out of the developer's real ~/.ssh.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows
	return home
}

func testHostKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func addr(t *testing.T) net.Addr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", "192.0.2.10:22")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// First contact with an unseen host must succeed and record the key, so the
// next connection is verified rather than blindly accepted forever.
func TestHostKeyCallback_TOFURecordsUnknownHost(t *testing.T) {
	home := isolateHome(t)
	key := testHostKey(t)

	if err := hostKeyCallback()("node-d.lan:22", addr(t), key); err != nil {
		t.Fatalf("first contact should be accepted: %v", err)
	}

	buf, err := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	if err != nil {
		t.Fatalf("known_hosts should have been created: %v", err)
	}
	if !strings.Contains(string(buf), "node-d.lan") {
		t.Errorf("known_hosts missing the hostname:\n%s", buf)
	}

	// Second connection with the same key must verify against the record.
	if err := hostKeyCallback()("node-d.lan:22", addr(t), key); err != nil {
		t.Errorf("known host with unchanged key should verify: %v", err)
	}
}

// The whole point of recording keys: a host that comes back with a different
// key is a hard failure, never a silent re-TOFU.
func TestHostKeyCallback_ChangedKeyIsRejected(t *testing.T) {
	isolateHome(t)
	original := testHostKey(t)
	impostor := testHostKey(t)

	if err := hostKeyCallback()("node-d.lan:22", addr(t), original); err != nil {
		t.Fatalf("seeding first key: %v", err)
	}

	err := hostKeyCallback()("node-d.lan:22", addr(t), impostor)
	if err == nil {
		t.Fatal("a changed host key must be rejected, not accepted")
	}
	// The message has to tell an operator how to resolve a legitimate rebuild,
	// or they will reach for InsecureIgnoreHostKey instead.
	if !strings.Contains(err.Error(), "ssh-keygen -R") {
		t.Errorf("error should suggest the remedy, got: %v", err)
	}
}

// Distinct hosts must not collide.
func TestHostKeyCallback_SeparateHostsIndependent(t *testing.T) {
	isolateHome(t)
	cb := hostKeyCallback()
	k1, k2 := testHostKey(t), testHostKey(t)

	if err := cb("a.lan:22", addr(t), k1); err != nil {
		t.Fatalf("host a: %v", err)
	}
	if err := cb("b.lan:22", addr(t), k2); err != nil {
		t.Fatalf("host b should TOFU independently: %v", err)
	}
	if err := cb("a.lan:22", addr(t), k1); err != nil {
		t.Errorf("host a should still verify: %v", err)
	}
}

func TestAuthMethods_NoCredentialIsAnError(t *testing.T) {
	isolateHome(t) // no identities, no agent
	t.Setenv("SSH_AUTH_SOCK", "")

	if _, err := authMethods(SSHTarget{User: "luther", Host: "h"}); err == nil {
		t.Error("expected an error when no key, agent, or password is available")
	}
}

func TestAuthMethods_PasswordOnly(t *testing.T) {
	isolateHome(t)
	t.Setenv("SSH_AUTH_SOCK", "")

	methods, err := authMethods(SSHTarget{User: "luther", Host: "h", Password: "pw"})
	if err != nil {
		t.Fatalf("password alone should be sufficient: %v", err)
	}
	// password + keyboard-interactive
	if len(methods) != 2 {
		t.Errorf("got %d methods, want 2 (password and keyboard-interactive)", len(methods))
	}
}

func TestAuthMethods_MissingKeyPathIsAnError(t *testing.T) {
	isolateHome(t)
	_, err := authMethods(SSHTarget{User: "l", Host: "h", KeyPath: "/nonexistent/key"})
	if err == nil {
		t.Fatal("an explicitly configured key that does not exist must be an error, not a silent fallback")
	}
}

// A key configured AND a password set should offer both: the key may exist
// locally without being in the target's authorized_keys yet, which is exactly
// the bootstrap case.
func TestAuthMethods_KeyAndPasswordBothOffered(t *testing.T) {
	home := isolateHome(t)
	keyPath := filepath.Join(home, "id_ed25519")
	writeTestKey(t, keyPath)

	methods, err := authMethods(SSHTarget{User: "l", Host: "h", KeyPath: keyPath, Password: "pw"})
	if err != nil {
		t.Fatalf("authMethods: %v", err)
	}
	if len(methods) != 3 {
		t.Errorf("got %d methods, want 3 (key, password, keyboard-interactive)", len(methods))
	}
}

func writeTestKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
}
