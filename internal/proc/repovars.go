package proc

import (
	"slices"
	"strings"
)

// repoVars point git at a repository. A git hook exports some of them, so a command that runs git
// or gh on the repository around its working directory drops them.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// DropRepoVars returns env without the repository-location variables (GIT_DIR, GIT_WORK_TREE, …),
// which a git hook exports; git then finds the repository from its working directory. Names match
// case-insensitively, as Windows treats them.
func DropRepoVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(
			repoVars,
			func(v string) bool { return strings.EqualFold(v, name) },
		)
	})
}
