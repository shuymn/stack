package cli

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

var (
	errUnknownFlag  = errors.New("unknown flag")
	errMissingValue = errors.New("flag needs a value")
)

type boolFlag interface {
	IsBoolFlag() bool
}

// parseInterspersed sets the flags defined in fs from args wherever they appear and returns the
// positional arguments in order. `-x` and `--x` are equivalent. A value flag takes `--x=v` or the
// next token as its value, even when that token starts with `-`. A boolean flag takes `--x` or
// `--x=<bool>`. A lone `-` is positional, and every token after `--` is positional. `-h` and
// `--help` return flag.ErrHelp unless defined in fs.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			return append(positional, args[i+1:]...), nil
		}
		if len(tok) < 2 || tok[0] != '-' {
			positional = append(positional, tok)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(tok[1:], "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			if name == "h" || name == "help" {
				return nil, flag.ErrHelp
			}
			return nil, fmt.Errorf("%w: %s", errUnknownFlag, tok)
		}
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			if !hasValue {
				value = "true"
			}
		} else if !hasValue {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%w: %s", errMissingValue, tok)
			}
			i++
			value = args[i]
		}
		if err := fs.Set(name, value); err != nil {
			return nil, fmt.Errorf("invalid value %q for %s: %w", value, tok, err)
		}
	}
	return positional, nil
}
