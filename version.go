package main

import (
	"fmt"
	"io"
	"runtime"
)

// version is the KAM release, set by the release build with
// -ldflags "-X main.version=<tag>". A plain local build reports "dev".
var version = "dev"

// runVersionCommand implements "kam version": it prints the version and the
// platform the binary was built for, so a downloaded artifact can say what it
// is (PLAN.md DST-1).
func runVersionCommand(args []string, stdout, _ io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: kam version")
	}
	fmt.Fprintf(stdout, "kam %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	return nil
}
