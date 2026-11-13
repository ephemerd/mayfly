package registrycache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// The writable registry behind ephemerd's LAN Actions cache. It lives on the
// same dedicated cache VM as the pull-through cache, as its own Compose
// project, so the two have independent lifecycles. See assets/actions.
const (
	// ActionsComposeDir is the Actions registry's Compose project directory.
	ActionsComposeDir = "/opt/actions-cache"
	// ActionsDataDir is its bind-mounted blob store, separate from DataDir.
	ActionsDataDir = "/var/lib/actions-cache"
	// ActionsEnvPath holds the rendered settings (Compose's .env and the gc
	// unit's EnvironmentFile).
	ActionsEnvPath = ActionsComposeDir + "/.env"
	// ActionsHtpasswdPath is the bcrypt credential file the registry reads.
	ActionsHtpasswdPath = ActionsComposeDir + "/htpasswd"
	// ActionsGCScriptPath is the collector the timer runs.
	ActionsGCScriptPath = "/usr/local/sbin/actions-cache-gc"
	// ActionsGCTimer is the weekly garbage-collection timer.
	ActionsGCTimer = "actions-cache-gc.timer"
	// ActionsContainerName is the registry container.
	ActionsContainerName = "actions-cache"
)

// actionsGuestPath maps an asset under assets/actions to its on-VM location.
func actionsGuestPath(rel string) (path, mode string, keep bool) {
	switch rel {
	case "actions/docker-compose.yml":
		return ActionsComposeDir + "/docker-compose.yml", "0644", true
	case "actions/gc.sh":
		return ActionsGCScriptPath, "0755", true
	case "actions/actions-cache-gc.service", "actions/actions-cache-gc.timer":
		return "/etc/systemd/system/" + strings.TrimPrefix(rel, "actions/"), "0644", true
	}
	return "", "", false
}

// ActionsStackFiles returns the Actions registry's embedded assets mapped to
// their guest paths. The rendered .env and htpasswd are NOT included — they
// carry settings and a secret; see ActionsEnvFile and ActionsHtpasswd.
func ActionsStackFiles() ([]StackFile, error) {
	var out []StackFile
	err := fs.WalkDir(assetsFS, "assets/actions", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		gp, mode, keep := actionsGuestPath(strings.TrimPrefix(p, "assets/"))
		if !keep {
			return nil
		}
		b, err := assetsFS.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, StackFile{Path: gp, Mode: mode, Content: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ActionsStackDirs returns the directories that must exist before the files
// land and the container starts.
func ActionsStackDirs() []string {
	return []string{ActionsComposeDir, ActionsDataDir}
}

// ActionsOptions are the rendered settings for the Actions registry.
type ActionsOptions struct {
	Port      int
	MaxSizeGB int
}

// ActionsEnvFile renders the Actions project's .env. It holds no secret (the
// password only ever exists as a bcrypt hash in htpasswd).
func ActionsEnvFile(o ActionsOptions) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "ACTIONS_REGISTRY_PORT=%d\n", o.Port)
	fmt.Fprintf(&b, "ACTIONS_CACHE_MAX_GB=%d\n", o.MaxSizeGB)
	fmt.Fprintf(&b, "ACTIONS_CACHE_DATA=%s\n", ActionsDataDir)
	fmt.Fprintf(&b, "ACTIONS_CACHE_DIR=%s\n", ActionsComposeDir)
	return []byte(b.String())
}

// ActionsHtpasswd renders the registry's htpasswd file: one bcrypt entry, the
// only format registry:2's htpasswd auth accepts.
//
// bcrypt salts every hash, so the bytes differ on each call. Nothing may
// fingerprint this output — the config hash covers the username and a hash
// of the password instead (see ActionsCredentialFingerprint).
func ActionsHtpasswd(username, password string) ([]byte, error) {
	if username == "" || password == "" {
		return nil, fmt.Errorf("actions registry: username and password are required")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("actions registry: hashing password: %w", err)
	}
	return []byte(username + ":" + string(h) + "\n"), nil
}

// ActionsCredentialFingerprint is a stable, non-reversible stand-in for the
// credentials in config fingerprints, so rotating the password re-pushes the
// stack without the plaintext ever being hashed into recorded state.
func ActionsCredentialFingerprint(username, password string) string {
	h := sha256.Sum256([]byte("actions-cache-credential\x00" + username + "\x00" + password))
	return hex.EncodeToString(h[:])[:16]
}

// ActionsAssetsHash fingerprints the embedded Actions registry assets.
func ActionsAssetsHash() string {
	files, err := ActionsStackFiles()
	if err != nil {
		return "invalid:" + err.Error()
	}
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s|%s\n", f.Path, f.Mode)
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
