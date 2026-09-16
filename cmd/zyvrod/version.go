package main

import (
	"fmt"
	"runtime"
)

// Stamped at build time. A build that did not go through the release script
// says so rather than claiming to be a version it is not.
var (
	Version = "dev"
	Commit  = "unknown"
)

// versionLine is the single line --version prints. The desktop app parses it
// when it has a binary but no running daemon to ask, so the shape is a
// contract: name, version, os/arch, commit, separated by single spaces.
func versionLine() string {
	return fmt.Sprintf("zyvrod %s %s/%s %s", Version, runtime.GOOS, runtime.GOARCH, Commit)
}
