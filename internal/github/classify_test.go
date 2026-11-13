package github

import "testing"

func hook(url string, events ...string) Hook {
	h := Hook{Events: events}
	h.Config.URL = url
	return h
}

func TestClassify(t *testing.T) {
	live := map[string]bool{"ci-mac-arm64.tricorder.cc": true}
	base := "tricorder.cc"

	tests := []struct {
		name string
		hook Hook
		want HookClass
	}{
		{"live pool hook", hook("https://ci-mac-arm64.tricorder.cc/webhook/github", "workflow_job"), Live},
		{"orphan old node", hook("https://mac.tricorder.cc/webhook/github", "workflow_job"), Orphan},
		{"orphan bare domain", hook("https://tricorder.cc/webhook/github", "workflow_job"), Orphan},
		{"foreign ngrok", hook("https://abc.ngrok-free.app/webhook/github", "workflow_job"), Foreign},
		{"foreign other domain", hook("https://ci.example.com/webhook/github", "workflow_job"), Foreign},
		{"non-workflow_job ignored", hook("https://mac.tricorder.cc/x", "push"), Foreign},
		{"live host but push event", hook("https://ci-mac-arm64.tricorder.cc/x", "push"), Foreign},
		{"garbage url", hook("://bad", "workflow_job"), Foreign},
		{"suffix trap not matched", hook("https://eviltricorder.cc/webhook/github", "workflow_job"), Foreign},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.hook, base, live); got != tt.want {
				t.Errorf("Classify() = %v, want %v", got, tt.want)
			}
		})
	}
}
