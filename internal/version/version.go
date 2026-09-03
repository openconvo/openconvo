// Package version exposes build-time version information for the
// openconvo binary. The variables are overridden at build time via
// -ldflags; see the Makefile and Dockerfile.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semantic version of the build, e.g. "0.1.0".
	Version = "0.1.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// Date is the build date in RFC 3339 format.
	Date = "unknown"
)

// Info describes the running build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// Get returns the build information for the running binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
	}
}

// String returns a single-line human-readable version string.
func (i Info) String() string {
	return fmt.Sprintf("openconvo %s (commit %s, built %s, %s)", i.Version, i.Commit, i.Date, i.GoVersion)
}
