package engine

import (
	"strings"
	"testing"
)

// growCmd's Windows branch must delete the trailing recovery partition that
// wedges between C: and the resized free space before resizing C: to max —
// otherwise a live disk_gb increase grows the virtual disk but the guest
// partition stays put. It must also stay idempotent (only resize with
// headroom) so a re-apply doesn't fail on an already-max partition.
func TestGrowCmdWindowsExtendsPastRecoveryPartition(t *testing.T) {
	cmd := growCmd("windows")
	joined := strings.Join(cmd, " ")

	for _, want := range []string{
		"Update-Disk -Number 0",
		"Remove-Partition",
		"$_.Offset -gt $c.Offset -and -not $_.DriveLetter",
		"Resize-Partition -DriveLetter C -Size $max",
		"$max -gt (Get-Partition -DriveLetter C).Size",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("windows growCmd missing %q:\n%s", want, joined)
		}
	}
	if cmd[0] != "powershell" {
		t.Errorf("windows growCmd should invoke powershell, got %q", cmd[0])
	}
}

func TestGrowCmdLinuxGrowsRootFS(t *testing.T) {
	joined := strings.Join(growCmd("linux"), " ")
	for _, want := range []string{"growpart /dev/sda 1", "resize2fs /dev/sda1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("linux growCmd missing %q:\n%s", want, joined)
		}
	}
}
