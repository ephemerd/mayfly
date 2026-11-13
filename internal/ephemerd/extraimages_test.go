package ephemerd

import (
	"strings"
	"testing"
)

// extraImageInputs is the minimum RenderConfig needs to produce output.
func extraImageInputs() ConfigInputs {
	return ConfigInputs{
		Owner:          "ephpm",
		AppID:          1,
		InstallationID: 2,
		PrivateKeyPath: `C:\app.pem`,
	}
}

// TestExtraTargetInheritsDefaultImages is the regression for the shipmates
// finding: ephemerd builds a separate provider per target and resolves a job's
// image from the provider that served it, so an extra target rendered without
// image defaults falls through to the runtime's bare-servercore fallback. Every
// luthermonson/shipmates Windows job ran on servercore with no toolchain, which
// is what made actions/setup-go time out extracting a Go it should never have
// had to download.
func TestExtraTargetInheritsDefaultImages(t *testing.T) {
	in := extraImageInputs()
	in.DefaultImageLinux = "ephpm/ephemerd:runner-ci-linux"
	in.DefaultImageWindows = "ephpm/ephemerd:runner-ci-windows"
	in.ExtraTargets = []ExtraTarget{{
		Owner: "luthermonson",
		Repos: []string{"shipmates"},
		Token: "ghp_x",
	}}

	got, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}

	_, extra, ok := strings.Cut(got, "[[github_extra]]")
	if !ok {
		t.Fatal("no [[github_extra]] block rendered")
	}
	// Bound the block so a key from a LATER section can't satisfy the check.
	if i := strings.Index(extra, "\n["); i >= 0 {
		extra = extra[:i]
	}

	for _, want := range []string{
		`default_image_windows = "ephpm/ephemerd:runner-ci-windows"`,
		`default_image_linux = "ephpm/ephemerd:runner-ci-linux"`,
	} {
		if !strings.Contains(extra, want) {
			t.Errorf("[[github_extra]] block missing %s\nblock was:\n%s", want, extra)
		}
	}
}

// TestExtraTargetOmitsUnsetImages keeps the rendering honest: a node with no
// defaults configured must not gain empty keys, which ephemerd would read as
// an explicit empty image rather than "unset".
func TestExtraTargetOmitsUnsetImages(t *testing.T) {
	in := extraImageInputs()
	in.ExtraTargets = []ExtraTarget{{
		Owner: "luthermonson",
		Repos: []string{"shipmates"},
		Token: "ghp_x",
	}}

	got, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if strings.Contains(got, `default_image_linux = ""`) ||
		strings.Contains(got, `default_image_windows = ""`) {
		t.Errorf("rendered an empty default image key:\n%s", got)
	}
}
