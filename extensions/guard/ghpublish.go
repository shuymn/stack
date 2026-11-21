package guard

import (
	"github.com/shuymn/stack/extensions/internal/publictext"
	"github.com/shuymn/stack/internal/ghargs"
)

const publishAdvice = "Use repository-relative paths or leave the path out."

// ghPublish denies a gh call whose published inputs (§4.3 table) contain a local path. Body files
// and stdin are read; an input that is not statically known is skipped. After a parse error the
// words from the failing one on are scanned as text.
func ghPublish(calls []ghCall, opts Options) (Decision, bool) {
	for _, c := range calls {
		if c.gh.Help || c.gh.DryRun { // gh shows help or a preview and publishes nothing
			continue
		}
		for _, in := range c.gh.Published {
			text, ok := readInput(in, c.call, opts.Home)
			if !ok {
				continue
			}
			if hits := publictext.LocalPaths(text); len(hits) > 0 {
				return deny(RuleGhPublish, "gh %s publishes the local path %q through %s. %s",
					c.gh.Command, hits[0], source(in), publishAdvice)
			}
		}
		for _, w := range c.gh.Literal {
			if hits := publictext.LocalPaths(w); len(hits) > 0 {
				return deny(RuleGhPublish,
					"gh %s: parsing stopped at %q (flags as of gh %s), and the words from there "+
						"on, which may be published, contain the local path %q. %s",
					c.gh.Command, c.gh.Literal[0], ghargs.GhVersion, hits[0], publishAdvice)
			}
		}
	}
	return Decision{}, false
}

func source(in ghargs.Input) string {
	switch in.Kind {
	case ghargs.File:
		return in.Flag + " file " + in.Path
	case ghargs.Stdin:
		return in.Flag + " - (stdin)"
	case ghargs.Text:
	}
	return in.Flag
}
