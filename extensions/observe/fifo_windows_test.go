//go:build windows

package observe

import "errors"

func mkfifo(string) error { return errors.ErrUnsupported }
