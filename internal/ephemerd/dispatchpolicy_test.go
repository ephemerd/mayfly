package ephemerd

import (
	"strings"
	"testing"
)

// policyInputs is the minimum RenderConfig needs to produce output.
func policyInputs() ConfigInputs {
	return ConfigInputs{
		Owner:          "ephpm",
		Repos:          []string{"ephemerd", "ephpm"},
		AppID:          1,
		InstallationID: 2,
		PrivateKeyPath: `C:\app.pem`,
	}
}

// section returns the body of the named TOML table from out, bounded so a key
// belonging to a LATER section cannot satisfy a Contains assertion. header is
// matched with its brackets and trailing newline, e.g. "[github.dispatch_policy]".
func section(t *testing.T, out, header string) string {
	t.Helper()
	_, body, ok := strings.Cut(out, header+"\n")
	if !ok {
		t.Fatalf("no %s section rendered in:\n%s", header, out)
	}
	if i := strings.Index(body, "\n["); i >= 0 {
		body = body[:i]
	}
	return body
}

// TestDispatchPolicyOmittedWhenUnset is the load-bearing default: a fleet that
// has never configured an allowlist must render exactly the config it rendered
// before this feature existed. In particular no empty list may appear —
// ephemerd's dispatchAllowed guards each field with `len(...) > 0`, so
// `allowed_repos = []` means "allow everything", the opposite of how an
// operator reading it would interpret the line.
func TestDispatchPolicyOmittedWhenUnset(t *testing.T) {
	in := policyInputs()
	in.ExtraTargets = []ExtraTarget{{
		Owner: "luthermonson",
		Repos: []string{"shipmates"},
		Token: "ghp_x",
	}}

	out, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	for _, unwanted := range []string{
		"dispatch_policy",
		"allowed_repos",
		"required_labels",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unset policy rendered %q:\n%s", unwanted, out)
		}
	}
}

// TestDispatchPolicyRendersPrimaryTarget checks the [github] block's allowlist,
// including the exact key spellings. ephemerd silently ignores unknown TOML
// keys, so a misspelled key yields a config that looks locked down and enforces
// nothing — the same failure mode as the fail_open/fallback_to_origin bug.
func TestDispatchPolicyRendersPrimaryTarget(t *testing.T) {
	in := policyInputs()
	in.DispatchPolicy = DispatchPolicy{
		AllowedRepos:   []string{"ephemerd", "ephpm"},
		RequiredLabels: []string{"ephemerd"},
	}

	out, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	body := section(t, out, "[github.dispatch_policy]")
	for _, want := range []string{
		`allowed_repos = ["ephemerd", "ephpm"]`,
		`required_labels = ["ephemerd"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("[github.dispatch_policy] missing %s\nblock was:\n%s", want, body)
		}
	}
}

// TestDispatchPolicyOnlySetFieldRendered keeps a half-configured policy honest:
// setting required_labels alone must not emit `allowed_repos = []`.
func TestDispatchPolicyOnlySetFieldRendered(t *testing.T) {
	in := policyInputs()
	in.DispatchPolicy = DispatchPolicy{RequiredLabels: []string{"ephemerd"}}

	out, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	body := section(t, out, "[github.dispatch_policy]")
	if !strings.Contains(body, `required_labels = ["ephemerd"]`) {
		t.Errorf("required_labels not rendered:\n%s", body)
	}
	if strings.Contains(out, "allowed_repos") {
		t.Errorf("unset allowed_repos was rendered:\n%s", out)
	}
}

// TestDispatchPolicyPrecedesGitHubExtra pins the TOML structure. A sub-table
// header closes its parent to further bare keys, so if
// [github.dispatch_policy] were emitted after the [[github_extra]] array the
// primary owner/repos keys would still parse but any [github] key written after
// it would not — and, worse, the policy could bind to the wrong table.
func TestDispatchPolicyPrecedesGitHubExtra(t *testing.T) {
	in := policyInputs()
	in.DispatchPolicy = DispatchPolicy{RequiredLabels: []string{"ephemerd"}}
	in.ExtraTargets = []ExtraTarget{{Owner: "luthermonson", Token: "ghp_x"}}

	out, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	pol := strings.Index(out, "[github.dispatch_policy]")
	extra := strings.Index(out, "[[github_extra]]")
	if pol < 0 || extra < 0 {
		t.Fatalf("expected both sections:\n%s", out)
	}
	if pol > extra {
		t.Errorf("[github.dispatch_policy] must precede [[github_extra]]:\n%s", out)
	}
	// It must also follow every bare [github] key.
	if pol < strings.Index(out, `private_key_path`) {
		t.Errorf("[github.dispatch_policy] emitted before the [github] keys:\n%s", out)
	}
}

// TestExtraTargetDispatchPolicyRendered is the mayfly#23-shaped regression for
// this control: ephemerd builds a SEPARATE provider per target and applies that
// target's own DispatchPolicy with no inheritance from [github], so an extra
// target rendered without one dispatches for everything it tracks even while
// the primary owner is gated. On this fleet that ungated target is
// luthermonson/shipmates — a public repo with forks.
func TestExtraTargetDispatchPolicyRendered(t *testing.T) {
	in := policyInputs()
	in.DispatchPolicy = DispatchPolicy{RequiredLabels: []string{"ephemerd"}}
	in.ExtraTargets = []ExtraTarget{{
		Owner: "luthermonson",
		Repos: []string{"shipmates"},
		Token: "ghp_x",
		DispatchPolicy: DispatchPolicy{
			AllowedRepos:   []string{"shipmates"},
			RequiredLabels: []string{"ephemerd"},
		},
	}}

	out, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	body := section(t, out, "[github_extra.dispatch_policy]")
	for _, want := range []string{
		`allowed_repos = ["shipmates"]`,
		`required_labels = ["ephemerd"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("[github_extra.dispatch_policy] missing %s\nblock was:\n%s", want, body)
		}
	}
	// The sub-table must come last within its [[github_extra]] element: a bare
	// key written after the header would land in the sub-table instead of the
	// target. Bound the element at its own sub-table header and check the
	// target's own keys all landed before it.
	_, elem, _ := strings.Cut(out, "[[github_extra]]\n")
	head, _, ok := strings.Cut(elem, "[github_extra.dispatch_policy]")
	if !ok {
		t.Fatal("policy header not inside the [[github_extra]] element")
	}
	for _, want := range []string{`owner = "luthermonson"`, `repos = ["shipmates"]`, `token = "ghp_x"`} {
		if !strings.Contains(head, want) {
			t.Errorf("target key %s must precede its dispatch_policy sub-table\ngot:\n%s", want, head)
		}
	}
}

// TestExtraTargetDispatchPolicyPerTarget confirms each [[github_extra]] element
// gets its OWN block, and that a target left unset emits nothing rather than
// picking up its neighbour's.
func TestExtraTargetDispatchPolicyPerTarget(t *testing.T) {
	in := policyInputs()
	in.ExtraTargets = []ExtraTarget{
		{
			Owner:          "luthermonson",
			Token:          "ghp_a",
			DispatchPolicy: DispatchPolicy{RequiredLabels: []string{"ephemerd"}},
		},
		{Owner: "someoneelse", Token: "ghp_b"},
	}

	out, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if n := strings.Count(out, "[github_extra.dispatch_policy]"); n != 1 {
		t.Errorf("want exactly 1 extra-target policy block, got %d:\n%s", n, out)
	}
	// The one block must belong to the FIRST element: everything after the
	// second [[github_extra]] header must be policy-free.
	_, second, ok := strings.Cut(out, `owner = "someoneelse"`)
	if !ok {
		t.Fatalf("second target not rendered:\n%s", out)
	}
	if strings.Contains(second, "dispatch_policy") {
		t.Errorf("policy leaked into the unset target:\n%s", second)
	}
}
