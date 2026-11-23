// Package t3loc forms the locator of a T3 Code message row, shared by observe, which writes it
// into an item's where, and jev-classify, which matches rows to items by it
// (docs/design/phase4-jev.md §3.2), so producer and consumer cannot drift.
package t3loc

import (
	"fmt"
	"strconv"
)

// Prefix starts every T3 locator.
const Prefix = "t3:"

// Locator is "t3:" + thread + ":" + Scalar(id), where id is the decoded payload's id.
func Locator(thread string, id any) string {
	return Prefix + thread + ":" + Scalar(id)
}

// Scalar renders a decoded JSON value as observe writes a payload id: a string as is, a number in
// shortest decimal, null as empty, anything else in Go's default format.
func Scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}
