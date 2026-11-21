package proc

import (
	"slices"
	"testing"
)

func TestDropRepoVars(t *testing.T) {
	env := []string{
		"PATH=/bin", "GIT_DIR=a", "git_work_tree=b", "GIT_INDEX_FILE=c", "GIT_COMMON_DIR=d",
		"GIT_OBJECT_DIRECTORY=e", "GIT_ALTERNATE_OBJECT_DIRECTORIES=f", "GIT_NAMESPACE=g",
		"GIT_TRACE=1", "GIT_DIRX=h",
	}
	orig := slices.Clone(env)
	got := DropRepoVars(env)
	if want := []string{"PATH=/bin", "GIT_TRACE=1", "GIT_DIRX=h"}; !slices.Equal(got, want) {
		t.Errorf("DropRepoVars = %q, want %q", got, want)
	}
	if !slices.Equal(env, orig) {
		t.Errorf("input changed: %q", env)
	}
}
