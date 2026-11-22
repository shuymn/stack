package guard

import "syscall"

var getLogicalDrives = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLogicalDrives")

// hostDrives returns the letters of the host's existing volumes, "" when they cannot be read.
func hostDrives() string {
	mask, _, _ := getLogicalDrives.Call()
	var out []byte
	for i := range 26 {
		if mask&(1<<i) != 0 {
			out = append(out, byte('A'+i))
		}
	}
	return string(out)
}
