//go:build !improvetest

package main

import "github.com/shuymn/stack/extensions/improve"

// testOptions is empty in release builds: they take no clock input.
func testOptions(*improve.Options) {}
