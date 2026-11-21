//go:build !observetest

package main

import "github.com/shuymn/stack/extensions/observe"

// testOptions is empty in release builds: they take no clock or budget input.
func testOptions(*observe.Options) {}
