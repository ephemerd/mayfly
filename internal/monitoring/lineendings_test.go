package monitoring

import (
	"bytes"
	"io/fs"
	"testing"
)

// The embedded assets ship to Linux VMs byte for byte, and go:embed takes the
// working-copy bytes. A CRLF checkout (core.autocrlf=true on Windows) would
// turn a script's shebang into "/bin/sh\r", which the kernel cannot exec.
// .gitattributes pins these paths to LF; this catches a checkout that lost it.
func TestEmbeddedAssetsAreLF(t *testing.T) {
	err := fs.WalkDir(assetsFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assetsFS.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("\r")) {
			t.Errorf("%s contains a carriage return; it must be checked out with LF line endings", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
