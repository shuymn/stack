package proc

import (
	"slices"
	"strings"
)

// repoVars point git at a repository. A git hook exports some of them, so a child that runs git
// drops them and finds the repository from its working directory instead.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// DropRepoVars returns a copy of env without the repository-location variables (GIT_DIR,
// GIT_WORK_TREE, …), matched case-insensitively; git then finds the repository from its
// working directory.
func DropRepoVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(
			repoVars,
			func(v string) bool { return strings.EqualFold(v, name) },
		)
	})
}
