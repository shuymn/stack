package kit

import (
	"fmt"
	"strings"

	"github.com/shuymn/stack/internal/ext"
)

// EntryBlock renders one entry stage result as `kit open` shows it, and reports whether it is
// shown at all: a passing result without context is not. The context goes in an
// `extension-context` block naming the executable, preceded by a warn marker when the result
// warned; a result without context, or from no executable, is a bare marker.
func EntryBlock(r ext.Result) (Block, bool) {
	if r.Status == ext.StatusPass && r.Context == "" {
		return Block{}, false
	}
	note := ""
	if r.Status != ext.StatusPass {
		reason := r.Error
		if reason == "" {
			reason = r.Status
		}
		note = fmt.Sprintf("[extension %s: %s: %s]", r.Name, ext.StatusWarn, reason)
	}
	body := strings.TrimSpace(r.Context)
	if body == "" || r.Path == "" {
		return Block{Body: strings.TrimSpace(note + "\n" + body)}, true
	}
	if note != "" {
		body = note + "\n" + body
	}
	return Block{Tag: "extension-context", Path: r.Path, Body: body}, true
}
