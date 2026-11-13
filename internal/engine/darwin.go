package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/ephemerd"
	"github.com/ephemerd/mayfly/internal/state"
)

// Darwin pool members are bare metal (Mac minis): no hypervisor channel
// exists, so the transport is SSH. Auth resolution, most to least
// compatible:
//
//  1. default    → system ssh binary: agent, ~/.ssh identities, ssh_config
//                  (Host aliases, ProxyJump, ports) all apply
//  2. ssh_key    → system ssh -i <key> with IdentitiesOnly
//  3. ssh_password → embedded Go SSH client (system ssh can't take a
//                  password non-interactively); bypasses ssh_config and
//                  host-key verification
//
// Commands run under `sudo sh` on the remote; the account needs
// passwordless sudo.

const sshTimeout = 5 * time.Minute

// SSHTarget carries everything needed to reach one darwin member.
type SSHTarget struct {
	User     string
	Host     string
	Port     int    // 0 = default (22 / ssh_config)
	KeyPath  string // pin one identity
	Password string // switches to the embedded client
}

func sshTarget(pool *state.Pool, host string) SSHTarget {
	return SSHTarget{
		User:     pool.SSHUser,
		Host:     host,
		Port:     pool.SSHPort,
		KeyPath:  pool.SSHKey,
		Password: pool.SSHPassword,
	}
}

// SSHRun executes a shell script on the target via sudo, feeding stdin.
// Exposed so cmd/mayfly's node ops can share the transport.
//
// A caller that already bounded the work keeps its own deadline — node ops pass
// a per-command budget, and `node drain` legitimately needs longer than the
// default cap. sshTimeout only applies when nobody set one.
func SSHRun(ctx context.Context, t SSHTarget, script string, stdin []byte) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sshTimeout)
		defer cancel()
	}
	return sshRunEmbedded(ctx, t, script, stdin)
}

// sshRunEmbedded executes script as root over the embedded Go SSH client.
// Auth resolution and host-key policy live in sshtransport.go.
func sshRunEmbedded(ctx context.Context, t SSHTarget, script string, stdin []byte) (string, error) {
	port := t.Port
	if port == 0 {
		port = 22
	}
	auths, err := authMethods(t)
	if err != nil {
		return "", err
	}
	conf := &gossh.ClientConfig{
		User:            t.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback(),
		Timeout:         30 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", t.Host, port)
	client, err := gossh.Dial("tcp", addr, conf)
	if err != nil {
		return "", fmt.Errorf("ssh %s@%s: %w", t.User, addr, err)
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer func() { _ = session.Close() }()

	if stdin != nil {
		session.Stdin = bytes.NewReader(stdin)
	}
	// Separate buffers: x/crypto/ssh copies stdout and stderr from two
	// concurrent goroutines, so pointing both at one bytes.Buffer is a data
	// race that silently drops output (nondeterministic, command-dependent —
	// it manifested as empty results from `node exec` on SSH nodes). Combine
	// after the command has fully finished.
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	combined := func() string { return stdout.String() + stderr.String() }

	done := make(chan error, 1)
	go func() { done <- session.Run("sudo sh -c " + shellQuote(script)) }()
	select {
	case err := <-done:
		if err != nil {
			return combined(), fmt.Errorf("ssh %s@%s: %w\n%s", t.User, addr, err, strings.TrimSpace(combined()))
		}
		return combined(), nil
	case <-ctx.Done():
		_ = session.Signal(gossh.SIGKILL)
		return combined(), ctx.Err()
	}
}

// shellQuote single-quotes a script for the remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
		}
	}
	return p
}

// enrollMember converges one SSH bare-metal host (darwin Mac mini or linux
// box): writes config + PEM, downloads the OS/arch binary, runs `ephemerd
// install` (which lays down the launchd plist on darwin / systemd unit on
// linux), then enables + (re)starts the service. Idempotent — also used for
// config-tier updates and upgrades. The OS-specific bits (release asset and
// service manager) are injected via spec.OS; everything else is shared.
func (e *Engine) enrollMember(ctx context.Context, pool *state.Pool, spec config.PoolSpec, host string) error {
	e.logf("pool %s: enrolling %s via ssh (%s/%s, %s)", pool.Name, host, spec.OS, spec.Arch, pool.SSHUser)

	binURL, err := resolveEphemerdURL(EphemerdSource(e.Cfg.Ephemerd, spec), spec.OS, spec.Arch)
	if err != nil {
		return err
	}
	ephCfg, err := ephemerd.RenderConfig(e.configInputs(pool, spec, spec.OS))
	if err != nil {
		return err
	}

	target := sshTarget(pool, host)
	steps := []struct {
		script string
		stdin  []byte
	}{
		// /usr/local/bin doesn't exist on a fresh Apple Silicon install
		// (Homebrew moved to /opt/homebrew); harmless to (re)create on linux.
		{"mkdir -p /var/lib/ephemerd /etc/ephemerd /usr/local/bin && chmod 700 /var/lib/ephemerd", nil},
		{"cat > /var/lib/ephemerd/config.toml && chmod 600 /var/lib/ephemerd/config.toml", []byte(ephCfg)},
		{"cat > /etc/ephemerd/app.pem && chmod 600 /etc/ephemerd/app.pem", e.GH.PrivateKeyPEM},
		{fmt.Sprintf(
			"set -e\n"+
				"curl -fsSL -o /tmp/ephemerd.tar.gz %q\n"+
				"tar -C /usr/local/bin -xzf /tmp/ephemerd.tar.gz ephemerd\n"+
				"chmod 0755 /usr/local/bin/ephemerd\n"+
				"rm -f /tmp/ephemerd.tar.gz\n"+
				"/usr/local/bin/ephemerd install\n"+
				enrollServiceStart(spec.OS),
			binURL), nil},
	}
	for _, s := range steps {
		if _, err := SSHRun(ctx, target, s.script, s.stdin); err != nil {
			return err
		}
	}

	if m, ok := pool.FindMemberByHost(host); ok {
		m.ConfigHash = ConfigHash(e.Cfg, e.St, spec, pool, e.GH.PrivateKeyPEM)
		m.Ephemerd = DesiredBinary(e.Cfg, spec)
		return nil
	}
	pool.Members = append(pool.Members, state.Member{
		Hostname:   host,
		OS:         spec.OS,
		Arch:       spec.Arch,
		ConfigHash: ConfigHash(e.Cfg, e.St, spec, pool, e.GH.PrivateKeyPEM),
		Ephemerd:   DesiredBinary(e.Cfg, spec),
		CreatedAt:  time.Now().UTC(),
	})
	return nil
}

// enrollServiceStart returns the shell fragment that enables + (re)starts the
// ephemerd service after `ephemerd install` has written the unit. `ephemerd
// install` itself lays down the platform service definition (launchd plist on
// darwin, systemd unit on linux — the same install command the VM cloud-init
// path invokes), so mayfly only has to (re)start it.
func enrollServiceStart(osName string) string {
	if osName == "linux" || osName == "" {
		// Mirror the VM cloud-init convention (systemctl enable --now
		// ephemerd) so an enrolled box and a provisioned VM run the service
		// identically and survive a reboot.
		return "systemctl daemon-reload\n" +
			"systemctl enable ephemerd\n" +
			"systemctl restart ephemerd"
	}
	// darwin: launchd via ephemerd's own subcommands.
	return "/usr/local/bin/ephemerd restart || /usr/local/bin/ephemerd start"
}

// unenrollMember drains and uninstalls ephemerd; the host itself is untouched.
// Uninstall failures are logged, not fatal — the box may simply be off, and
// state removal is the authoritative action. Works for darwin and linux
// bare-metal alike (ephemerd's own drain/uninstall handle the service).
func (e *Engine) unenrollMember(ctx context.Context, pool *state.Pool, host string) error {
	e.logf("pool %s: unenrolling %s", pool.Name, host)
	script := "/usr/local/bin/ephemerd drain || true; /usr/local/bin/ephemerd uninstall || true"
	if _, err := SSHRun(ctx, sshTarget(pool, host), script, nil); err != nil {
		e.logf("pool %s: warning: uninstall on %s failed (host offline?): %v", pool.Name, host, err)
	}
	pool.RemoveMemberByHost(host)
	return nil
}
