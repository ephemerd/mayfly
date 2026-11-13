package engine

import (
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/config"
)

func TestResolveEphemerdURL(t *testing.T) {
	eph := config.Ephemerd{Version: "v0.1.0"}
	tests := []struct {
		os, arch string
		want     string
		wantErr  bool
	}{
		{"linux", "amd64", "ephemerd_v0.1.0_linux_amd64.tar.gz", false},
		{"linux", "arm64", "ephemerd_v0.1.0_linux_arm64.tar.gz", false},
		{"windows", "amd64", "ephemerd_v0.1.0_windows_amd64.zip", false},
		{"darwin", "arm64", "ephemerd_v0.1.0_darwin_arm64.tar.gz", false},
		{"windows", "arm64", "", true},
		{"darwin", "amd64", "", true},
		{"beos", "amd64", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.os+"/"+tt.arch, func(t *testing.T) {
			got, err := resolveEphemerdURL(eph, tt.os, tt.arch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(got, tt.want) {
				t.Errorf("got %q, want suffix %q", got, tt.want)
			}
		})
	}
}
