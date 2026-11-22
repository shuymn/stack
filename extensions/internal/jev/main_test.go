package jev

import (
	"os"
	"testing"

	"github.com/shuymn/stack/internal/gittest"
)

// TestMain guards the real repository: the fixture tests build synthetic repositories.
func TestMain(m *testing.M) {
	os.Exit(gittest.Main(m))
}
