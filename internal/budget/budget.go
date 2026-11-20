// Package budget implements `stack budget status`: a display-only table of each kit's state file
// sizes and attention item counts. Being over a threshold is shown as a note, never as an error.
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/shuymn/stack/internal/kit"
	"github.com/shuymn/stack/internal/paths"
)

const (
	// roleAll is the role whose items apply to every role; they are counted in their own column.
	roleAll = "all"
	// unknown fills a cell whose value could not be read.
	unknown = "?"
	// absent fills the size cell of a state file that does not exist.
	absent = "-"
	// gap is the number of spaces between table columns.
	gap = 2
)

// Options selects what Status reads.
type Options struct {
	// Root is the absolute kit root.
	Root string
	// Home locates the sessions dir holding `<SessionID>.kits`.
	Home string
	// SessionID, when non-empty, marks the kits recorded for that session.
	SessionID string
	// Stderr receives warnings that do not stop the display.
	Stderr io.Writer
}

// kitRow is one kit's line of the table.
type kitRow struct {
	slug  string
	open  bool
	sizes []string
	// counts holds item counts by role, roleAll included; nil when items.json could not be read.
	counts map[string]int
	notes  []string
}

// Status writes the table of every kit under o.Root to w, or "no kits" when there is none. It is
// display only: a kit root that cannot be listed is shown with its reason, and only a failed write
// is an error.
func Status(ctx context.Context, w io.Writer, o Options) error {
	slugs, err := listKits(o.Root)
	if err != nil {
		_, err := fmt.Fprintf(w, "kit root: %s\ncannot list kits: %v\n", o.Root, err)
		return wrapWrite(err)
	}
	if len(slugs) == 0 {
		_, err := fmt.Fprintln(w, "no kits")
		return wrapWrite(err)
	}
	open := openKits(ctx, o)
	rows := make([]kitRow, len(slugs))
	for i, slug := range slugs {
		rows[i] = readKit(paths.KitDir(o.Root, slug), slug)
		rows[i].open = slices.Contains(open, slug)
	}
	return wrapWrite(write(w, o.Root, rows))
}

func wrapWrite(err error) error {
	if err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
}

// listKits returns the sorted slugs of the directories under root; a missing root has none.
func listKits(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		// Windows reports a file used as a directory as "path not found"; only a root that is
		// really absent has no kits.
		if _, serr := os.Lstat(root); errors.Is(serr, fs.ErrNotExist) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read kit root: %w", err)
	}
	var slugs []string
	for _, e := range entries {
		if !paths.ValidSlug(e.Name()) {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, e.Name())); err == nil && info.IsDir() {
			slugs = append(slugs, e.Name())
		}
	}
	return slugs, nil // ReadDir sorts by name
}

// openKits returns the slugs recorded for o.SessionID. A failure only costs the marks, so it is a
// warning.
func openKits(ctx context.Context, o Options) []string {
	if o.SessionID == "" {
		return nil
	}
	open, err := (kit.Store{Home: o.Home}).List(ctx, o.SessionID)
	if err != nil {
		if o.Stderr != nil {
			_, _ = fmt.Fprintf(o.Stderr, "stack: %v; session kits are not marked\n", err)
		}
		return nil
	}
	return open
}

// readKit measures the kit at dir. Files are opened through an os.Root on dir, as the renderer
// does, so a symlink leaving the kit is unreadable rather than followed.
func readKit(dir, slug string) kitRow {
	row := kitRow{slug: slug}
	root, err := os.OpenRoot(dir)
	if err != nil {
		row.notes = append(row.notes, slug+" is unreadable")
		for range kit.StateFiles {
			row.sizes = append(row.sizes, unknown)
		}
		return row
	}
	defer func() { _ = root.Close() }()

	for _, name := range kit.StateFiles {
		info, err := kit.Stat(root, name)
		switch {
		case errors.Is(err, kit.ErrMissing):
			row.sizes = append(row.sizes, absent)
			continue
		case err != nil:
			row.sizes = append(row.sizes, unknown)
			row.notes = append(row.notes, slug+"/"+name+" is unreadable")
			continue
		case !info.Mode().IsRegular():
			row.sizes = append(row.sizes, unknown)
			row.notes = append(row.notes, slug+"/"+name+" is not a regular file")
			continue
		}
		n := info.Size()
		row.sizes = append(row.sizes, strconv.FormatInt(n, 10))
		if n > kit.StateLimit {
			row.notes = append(row.notes,
				fmt.Sprintf("%s/%s is %d bytes, over %d", slug, name, n, kit.StateLimit))
		}
	}
	row.counts, row.notes = countItems(root, slug, row.notes)
	return row
}

// countItems counts the items of `attention/items.json` by their string role; an `all` item is
// counted under `all` only (`attention-budget.py:80-86` spread it over the capped roles, and there
// are no caps here). A missing file counts nothing; an unreadable or malformed one yields nil
// counts and a note.
func countItems(root *os.Root, slug string, notes []string) (map[string]int, []string) {
	label := slug + "/" + filepath.ToSlash(kit.ItemsFile)
	counts := map[string]int{}
	data, err := kit.ReadFile(root, kit.ItemsFile)
	if errors.Is(err, kit.ErrMissing) {
		return counts, notes
	}
	if err != nil {
		return nil, append(notes, label+" is unreadable")
	}
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil, append(notes, label+" is not valid JSON")
	}
	for _, raw := range doc.Items {
		var item struct {
			Role any `json:"role"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue // not an object
		}
		if role, ok := item.Role.(string); ok && strings.TrimSpace(role) != "" {
			counts[role]++
		}
	}
	return counts, notes
}

// write prints the table: one column per role present in any kit, sorted, then `all`.
func write(w io.Writer, root string, rows []kitRow) error {
	var roles []string
	marked := false
	for _, r := range rows {
		for role := range r.counts {
			if role != roleAll && !slices.Contains(roles, role) {
				roles = append(roles, role)
			}
		}
		marked = marked || r.open
	}
	slices.Sort(roles)
	roles = append(roles, roleAll)

	var b strings.Builder
	b.WriteString("kit root: " + root + "\n")
	tw := tabwriter.NewWriter(&b, 0, 0, gap, ' ', 0)
	header := append([]string{"  kit"}, kit.StateFiles...)
	for _, role := range roles {
		header = append(header, cell(role))
	}
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	var notes []string
	for _, r := range rows {
		mark := "  "
		if r.open {
			mark = "* "
		}
		cells := append([]string{mark + r.slug}, r.sizes...)
		for _, role := range roles {
			if r.counts == nil {
				cells = append(cells, unknown)
			} else {
				cells = append(cells, strconv.Itoa(r.counts[role]))
			}
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
		notes = append(notes, r.notes...)
	}
	_ = tw.Flush() // writes to a strings.Builder cannot fail
	if marked {
		b.WriteString("* open in this session\n")
	}
	for _, n := range notes {
		b.WriteString("note: " + n + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err //nolint:wrapcheck // Status wraps it.
}

// cell quotes a role name that would break the table: one with a space, tab or other non-graphic
// rune.
func cell(s string) string {
	if strings.IndexFunc(
		s,
		func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsGraphic(r) },
	) >= 0 {
		return strconv.Quote(s)
	}
	return s
}
