package main

import (
	"fmt"
	"runtime"
)

// version is stamped in at build time with -ldflags "-X main.version=...".
// A plain `go build` leaves it as "dev", which is the honest answer for a
// binary that never came from a release.
var version = "dev"

func versionCmd() error {
	fmt.Printf("wifimeter %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	return nil
}
