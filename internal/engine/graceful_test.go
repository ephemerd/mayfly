package engine

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{Out: io.Discard}
}

func TestParseActiveJobs(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		want    int
		wantErr bool
	}{
		{
			name: "ephemerd status JSON",
			out: `{
  "active_jobs": 3,
  "draining": false,
  "max_concurrent": 3,
  "status": "ok",
  "uptime": "1h2m3s"
}`,
			want: 3,
		},
		{name: "zero", out: `{"active_jobs": 0}`, want: 0},
		{name: "compact", out: `{"active_jobs":12,"draining":true}`, want: 12},
		{name: "garbage", out: "connection refused", wantErr: true},
		{name: "empty", out: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := parseActiveJobs(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %d", n)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.want {
				t.Errorf("got %d, want %d", n, tc.want)
			}
		})
	}
}

func TestHookPointsAt(t *testing.T) {
	cases := []struct {
		url, host string
		want      bool
	}{
		{"https://ci-linux-amd64.tricorder.cc/webhook/github/ephpm", "ci-linux-amd64.tricorder.cc", true},
		{"https://ci-linux-amd64.tricorder.cc", "ci-linux-amd64.tricorder.cc", true},
		// Another pool's hook must not be cordoned.
		{"https://ci-win-amd64.tricorder.cc/webhook/github/ephpm", "ci-linux-amd64.tricorder.cc", false},
		// Prefix-similar hostnames must not match.
		{"https://ci-linux-amd64.tricorder.cc.evil.example/x", "ci-linux-amd64.tricorder.cc", false},
		{"https://example.com/webhook", "", false},
	}
	for _, tc := range cases {
		if got := hookPointsAt(tc.url, tc.host); got != tc.want {
			t.Errorf("hookPointsAt(%q, %q) = %v, want %v", tc.url, tc.host, got, tc.want)
		}
	}
}

// waitForIdle must return promptly once the probe reports zero jobs.
func TestWaitForIdle_ReturnsOnZero(t *testing.T) {
	e := testEngine(t)
	calls := 0
	e.waitForIdle(context.Background(), func(context.Context) (string, error) {
		calls++
		return `{"active_jobs": 0}`, nil
	}, "p", "n", time.Minute)
	if calls != 1 {
		t.Errorf("probe called %d times, want 1", calls)
	}
}

// A dead daemon must not block the apply that would repair it: two probe
// failures are treated as idle.
func TestWaitForIdle_ProbeFailureTreatedAsIdle(t *testing.T) {
	e := testEngine(t)
	calls := 0
	e.waitForIdle(context.Background(), func(context.Context) (string, error) {
		calls++
		return "", errors.New("daemon down")
	}, "p", "n", time.Minute)
	if calls != 2 {
		t.Errorf("probe called %d times, want 2 (one retry, then proceed)", calls)
	}
}

// Zero timeout = the old immediate-restart behavior: no probing at all.
func TestWaitForIdle_ZeroTimeoutSkips(t *testing.T) {
	e := testEngine(t)
	e.waitForIdle(context.Background(), func(context.Context) (string, error) {
		t.Fatal("probe must not run with zero timeout")
		return "", nil
	}, "p", "n", 0)
}
