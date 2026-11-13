package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func decodeCfg(t *testing.T, s string) *Config {
	t.Helper()
	var c Config
	if _, err := toml.Decode(s, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &c
}

// TestDispatchPolicyDecodes checks the mayfly-side TOML shape operators write.
func TestDispatchPolicyDecodes(t *testing.T) {
	c := decodeCfg(t, `
[github]
owner = "ephpm"

[github.dispatch_policy]
required_labels = ["ephemerd"]

[pools.linux-amd64]
hostname = "ci.example.com"

[pools.linux-amd64.dispatch_policy]
allowed_repos = ["ephemerd", "ephpm"]

[[pools.linux-amd64.github_extra]]
owner = "luthermonson"
token = "ghp_x"

[pools.linux-amd64.github_extra.dispatch_policy]
allowed_repos = ["shipmates"]
`)
	if got := c.GitHub.DispatchPolicy.RequiredLabels; len(got) != 1 || got[0] != "ephemerd" {
		t.Errorf("github.dispatch_policy.required_labels = %v", got)
	}
	p := c.Pools["linux-amd64"]
	if got := p.DispatchPolicy.AllowedRepos; len(got) != 2 {
		t.Errorf("pool allowed_repos = %v", got)
	}
	if len(p.GitHubExtra) != 1 {
		t.Fatalf("github_extra = %v", p.GitHubExtra)
	}
	if got := p.GitHubExtra[0].DispatchPolicy.AllowedRepos; len(got) != 1 || got[0] != "shipmates" {
		t.Errorf("github_extra dispatch_policy.allowed_repos = %v", got)
	}
}

// TestDispatchPolicyIsZero pins the unset default that everything downstream
// keys off.
func TestDispatchPolicyIsZero(t *testing.T) {
	if !(DispatchPolicy{}).IsZero() {
		t.Error("empty policy should be zero")
	}
	if (DispatchPolicy{AllowedRepos: []string{"x"}}).IsZero() {
		t.Error("allowed_repos set should not be zero")
	}
	if (DispatchPolicy{RequiredLabels: []string{"x"}}).IsZero() {
		t.Error("required_labels set should not be zero")
	}
}

// TestResolvedDispatchPolicyMergesPerField is the anti-loosening property: a
// pool that overrides only one field must keep the fleet-wide value of the
// other. A whole-block override would let a pool that sets allowed_repos
// silently drop the global required_labels.
func TestResolvedDispatchPolicyMergesPerField(t *testing.T) {
	g := GitHub{DispatchPolicy: DispatchPolicy{
		AllowedRepos:   []string{"ephemerd"},
		RequiredLabels: []string{"ephemerd"},
	}}
	p := PoolSpec{DispatchPolicy: DispatchPolicy{AllowedRepos: []string{"ephpm"}}}

	got := p.ResolvedDispatchPolicy(g)
	if len(got.AllowedRepos) != 1 || got.AllowedRepos[0] != "ephpm" {
		t.Errorf("pool allowed_repos should win, got %v", got.AllowedRepos)
	}
	if len(got.RequiredLabels) != 1 || got.RequiredLabels[0] != "ephemerd" {
		t.Errorf("global required_labels should be inherited, got %v", got.RequiredLabels)
	}
}

// TestResolvedDispatchPolicyUnsetStaysUnset: no policy anywhere must resolve to
// the zero value, which renders nothing.
func TestResolvedDispatchPolicyUnsetStaysUnset(t *testing.T) {
	if got := (PoolSpec{}).ResolvedDispatchPolicy(GitHub{}); !got.IsZero() {
		t.Errorf("want zero policy, got %+v", got)
	}
}

// TestExtraTargetInheritsRequiredLabels closes the gap ephemerd leaves: it
// applies each target's own policy with no inheritance, so an extra target left
// unset would be fully ungated while the primary owner is locked down.
func TestExtraTargetInheritsRequiredLabels(t *testing.T) {
	g := GitHub{DispatchPolicy: DispatchPolicy{RequiredLabels: []string{"ephemerd"}}}
	p := PoolSpec{}
	tgt := GitHubTarget{Owner: "luthermonson"}

	got := p.EffectiveDispatchPolicyFor(tgt, g)
	if len(got.RequiredLabels) != 1 || got.RequiredLabels[0] != "ephemerd" {
		t.Errorf("required_labels should be inherited, got %v", got.RequiredLabels)
	}
}

// TestExtraTargetNeverInheritsAllowedRepos is the black-hole guard. ephemerd
// matches allowed_repos against the webhook's BARE repository name, and those
// names are scoped to the target's owner — copying the primary owner's repo
// list onto another owner's target would match nothing and block every job it
// serves.
func TestExtraTargetNeverInheritsAllowedRepos(t *testing.T) {
	g := GitHub{DispatchPolicy: DispatchPolicy{
		AllowedRepos:   []string{"ephemerd", "ephpm"},
		RequiredLabels: []string{"ephemerd"},
	}}
	p := PoolSpec{DispatchPolicy: DispatchPolicy{AllowedRepos: []string{"ephemerd"}}}
	tgt := GitHubTarget{Owner: "luthermonson", Repos: []string{"shipmates"}}

	got := p.EffectiveDispatchPolicyFor(tgt, g)
	if len(got.AllowedRepos) != 0 {
		t.Errorf("allowed_repos must never be inherited across owners, got %v", got.AllowedRepos)
	}
}

// TestExtraTargetExplicitPolicyWins: an explicit target policy is used as
// written, including its own labels.
func TestExtraTargetExplicitPolicyWins(t *testing.T) {
	g := GitHub{DispatchPolicy: DispatchPolicy{RequiredLabels: []string{"ephemerd"}}}
	tgt := GitHubTarget{
		Owner: "luthermonson",
		DispatchPolicy: DispatchPolicy{
			AllowedRepos:   []string{"shipmates"},
			RequiredLabels: []string{"shipmates-ci"},
		},
	}

	got := (PoolSpec{}).EffectiveDispatchPolicyFor(tgt, g)
	if len(got.RequiredLabels) != 1 || got.RequiredLabels[0] != "shipmates-ci" {
		t.Errorf("explicit required_labels should win, got %v", got.RequiredLabels)
	}
	if len(got.AllowedRepos) != 1 || got.AllowedRepos[0] != "shipmates" {
		t.Errorf("explicit allowed_repos should be kept, got %v", got.AllowedRepos)
	}
}

// TestExtraTargetUnsetPolicyStaysUnset: with no policy anywhere, an extra
// target must render nothing (ephemerd's historical behavior).
func TestExtraTargetUnsetPolicyStaysUnset(t *testing.T) {
	got := (PoolSpec{}).EffectiveDispatchPolicyFor(GitHubTarget{Owner: "x"}, GitHub{})
	if !got.IsZero() {
		t.Errorf("want zero policy, got %+v", got)
	}
}

func TestDispatchPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		policy  DispatchPolicy
		wantErr string
	}{
		{"ok", DispatchPolicy{AllowedRepos: []string{"ephemerd"}, RequiredLabels: []string{"ephemerd"}}, ""},
		{"empty policy", DispatchPolicy{}, ""},
		// The high-value guard: ephemerd matches repository.name, so
		// "owner/repo" matches nothing and black-holes the target.
		{"owner-qualified repo", DispatchPolicy{AllowedRepos: []string{"ephpm/ephemerd"}}, "bare repo name"},
		{"empty repo", DispatchPolicy{AllowedRepos: []string{""}}, "allowed_repos[0] is empty"},
		{"blank label", DispatchPolicy{RequiredLabels: []string{"  "}}, "required_labels[0] is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.policy.validate("here")
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want no error, got %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(errs[0].Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", errs[0], tt.wantErr)
			}
		})
	}
}

// TestValidateReachesEveryPolicy makes sure the validation is actually wired at
// all three scopes, not just defined.
func TestValidateReachesEveryPolicy(t *testing.T) {
	base := func() *Config {
		return &Config{
			GitHub: GitHub{Owner: "ephpm", AppID: 1, InstallationID: 2, PrivateKeyPath: "k.pem"},
			Pools: map[string]PoolSpec{"p": {
				Hostname:    "ci.example.com",
				Members:     []string{"host1"},
				GitHubExtra: []GitHubTarget{{Owner: "luthermonson", Token: "ghp_x"}},
			}},
		}
	}
	bad := DispatchPolicy{AllowedRepos: []string{"owner/repo"}}

	t.Run("global", func(t *testing.T) {
		c := base()
		c.GitHub.DispatchPolicy = bad
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "github: dispatch_policy") {
			t.Errorf("want a github-scoped policy error, got %v", err)
		}
	})
	t.Run("pool", func(t *testing.T) {
		c := base()
		p := c.Pools["p"]
		p.DispatchPolicy = bad
		c.Pools["p"] = p
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `pool "p": dispatch_policy`) {
			t.Errorf("want a pool-scoped policy error, got %v", err)
		}
	})
	t.Run("extra target", func(t *testing.T) {
		c := base()
		p := c.Pools["p"]
		p.GitHubExtra[0].DispatchPolicy = bad
		c.Pools["p"] = p
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `github_extra[0]: dispatch_policy`) {
			t.Errorf("want a target-scoped policy error, got %v", err)
		}
	})
	t.Run("clean config still validates", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Errorf("baseline config should validate, got %v", err)
		}
	})
}
