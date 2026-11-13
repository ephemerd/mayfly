package github

import (
	"net/url"
	"strings"
)

// HookClass is the verdict for a fleet-webhook reconciliation.
type HookClass int

const (
	// Foreign: not a mayfly/ephemerd hook (host outside base domain) — leave alone.
	Foreign HookClass = iota
	// Live: points at a current pool hostname — keep.
	Live
	// Orphan: within the base domain but not a live pool hostname (old node,
	// dead node, or renamed pool) — prune.
	Orphan
)

// Classify decides a hook's fate. baseDomain is the fleet's zone (e.g.
// "tricorder.cc"); live is the set of current pool hostnames. Only
// workflow_job hooks are ever considered ours.
func Classify(h Hook, baseDomain string, live map[string]bool) HookClass {
	if !h.HasEvent("workflow_job") {
		return Foreign
	}
	host := hookHost(h.Config.URL)
	if host == "" {
		return Foreign
	}
	if live[host] {
		return Live
	}
	if baseDomain != "" && (host == baseDomain || strings.HasSuffix(host, "."+baseDomain)) {
		return Orphan
	}
	return Foreign
}

func hookHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
