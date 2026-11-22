//go:build !windows

package guard

// hostDrives returns "": hosts other than Windows have no drive letters.
func hostDrives() string { return "" }
