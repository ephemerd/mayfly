package cloudinit

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var tmplFS embed.FS

type Inputs struct {
	Hostname          string
	OS                string // "linux" (default when empty) or "windows"
	GitHubAppPEM      []byte
	EphemerdConfig    string // full TOML content
	EphemerdBinaryURL string
}

type Rendered struct {
	UserData string
	MetaData string
}

// Render evaluates the user-data and meta-data templates against i.
func Render(i Inputs) (*Rendered, error) {
	if err := i.validate(); err != nil {
		return nil, err
	}

	view := map[string]string{
		"Hostname":               i.Hostname,
		"GitHubAppPEMBase64":     base64.StdEncoding.EncodeToString(i.GitHubAppPEM),
		"EphemerdConfigIndented": indent(i.EphemerdConfig, "      "),
		"EphemerdConfigBase64":   base64.StdEncoding.EncodeToString([]byte(i.EphemerdConfig)),
		"EphemerdBinaryURL":      i.EphemerdBinaryURL,
	}

	userTmpl := "templates/user-data.yaml.tmpl"
	if i.OS == "windows" {
		userTmpl = "templates/user-data-windows.ps1.tmpl"
	}
	user, err := renderFile(userTmpl, view)
	if err != nil {
		return nil, err
	}
	meta, err := renderFile("templates/meta-data.yaml.tmpl", view)
	if err != nil {
		return nil, err
	}
	return &Rendered{UserData: user, MetaData: meta}, nil
}

// WriteFile is one base64-encoded file for cloud-init to drop on a guest.
type WriteFile struct {
	Path          string
	ContentBase64 string
	Mode          string // octal string, e.g. "0644"
}

// MonitoringInputs drives the utility host's cloud-init: the Prometheus +
// Grafana node, plus any other service mayfly colocates there (currently the
// registry pull-through cache).
type MonitoringInputs struct {
	Hostname string
	Dirs     []string
	Files    []WriteFile
	// ComposeDirs are the Docker Compose projects to bring up on first boot,
	// in order. Each service gets its own project so their lifecycles stay
	// independent.
	ComposeDirs []string
	// Units are systemd units to enable + start after the files land (e.g. the
	// cache's prune timer). Empty for a monitoring-only host.
	Units []string
}

// RenderMonitoring evaluates the utility-host user-data (installs Docker,
// drops the stack assets, brings the compose projects up, enables any units)
// plus meta-data.
func RenderMonitoring(i MonitoringInputs) (*Rendered, error) {
	if i.Hostname == "" {
		return nil, fmt.Errorf("cloudinit: monitoring missing hostname")
	}
	if len(i.Files) == 0 {
		return nil, fmt.Errorf("cloudinit: monitoring missing stack files")
	}
	if len(i.ComposeDirs) == 0 {
		return nil, fmt.Errorf("cloudinit: monitoring missing compose dir")
	}
	user, err := renderFile("templates/monitoring.yaml.tmpl", i)
	if err != nil {
		return nil, err
	}
	meta, err := renderFile("templates/meta-data.yaml.tmpl", map[string]string{"Hostname": i.Hostname})
	if err != nil {
		return nil, err
	}
	return &Rendered{UserData: user, MetaData: meta}, nil
}

func (i Inputs) validate() error {
	switch i.OS {
	case "", "linux", "windows":
	default:
		return fmt.Errorf("cloudinit: unsupported os %q", i.OS)
	}
	miss := func(name, val string) error {
		if val == "" {
			return fmt.Errorf("cloudinit: missing %s", name)
		}
		return nil
	}
	for _, e := range []error{
		miss("hostname", i.Hostname),
		miss("ephemerd_config", i.EphemerdConfig),
		miss("ephemerd_binary_url", i.EphemerdBinaryURL),
	} {
		if e != nil {
			return e
		}
	}
	if len(i.GitHubAppPEM) == 0 {
		return fmt.Errorf("cloudinit: missing github_app_pem")
	}
	return nil
}

func renderFile(path string, view any) (string, error) {
	b, err := tmplFS.ReadFile(path)
	if err != nil {
		return "", err
	}
	t, err := template.New(path).Parse(string(b))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, view); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// ModelServerInputs drives the model server's cloud-init: no Docker, just
// the setup files and the ordered first-boot commands that install the GPU
// driver and Ollama (see internal/modelserver).
type ModelServerInputs struct {
	Hostname string
	Files    []WriteFile
	// Commands run in order after the files land. Each is one shell line.
	Commands []string
}

// RenderModelServer evaluates the model-server user-data plus meta-data.
func RenderModelServer(i ModelServerInputs) (*Rendered, error) {
	if i.Hostname == "" {
		return nil, fmt.Errorf("cloudinit: model server missing hostname")
	}
	if len(i.Files) == 0 || len(i.Commands) == 0 {
		return nil, fmt.Errorf("cloudinit: model server missing setup files or commands")
	}
	// Quoted here rather than in the template: the commands carry pipes,
	// brackets and semicolons, and a Go-quoted string is a valid YAML
	// double-quoted scalar, so no command can break the document.
	quoted := make([]string, len(i.Commands))
	for n, c := range i.Commands {
		quoted[n] = strconv.Quote(c)
	}
	user, err := renderFile("templates/modelserver.yaml.tmpl", struct {
		Hostname string
		Files    []WriteFile
		Commands []string
	}{i.Hostname, i.Files, quoted})
	if err != nil {
		return nil, err
	}
	meta, err := renderFile("templates/meta-data.yaml.tmpl", map[string]string{"Hostname": i.Hostname})
	if err != nil {
		return nil, err
	}
	return &Rendered{UserData: user, MetaData: meta}, nil
}
