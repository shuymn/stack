package guard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestHostDrives checks that on Windows the host's volumes include the temp directory's drive.
func TestHostDrives(t *testing.T) {
	got := hostDrives()
	if runtime.GOOS != "windows" {
		if got != "" {
			t.Errorf("hostDrives() = %q on %s, want none", got, runtime.GOOS)
		}
		return
	}
	vol := filepath.VolumeName(os.TempDir())
	if len(vol) != 2 || !strings.Contains(got, strings.ToUpper(vol[:1])) {
		t.Errorf("hostDrives() = %q, want it to hold the temp drive %q", got, vol)
	}
}
