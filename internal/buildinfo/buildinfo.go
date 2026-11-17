// Package buildinfo reports the same release identity in the CLI and metrics.
package buildinfo

import "runtime"

// Version and Commit are supplied by the release build's linker flags.
// Local builds remain explicitly identifiable rather than claiming a release.
var Version = "dev"
var Commit = "unknown"

func GoVersion() string { return runtime.Version() }
