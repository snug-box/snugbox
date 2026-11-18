// Package version holds the build's version, set by the release build with
// -ldflags "-X snugbox.dev/snugbox/agent/internal/version.Version=1.2.3".
package version

var (
	// Version is the release version, or "dev" for a local build.
	Version = "dev"
	// Commit is the git commit the release was built from.
	Commit = "unknown"
	// Date is the build date in RFC 3339, fixed to the commit time so builds
	// are reproducible.
	Date = "unknown"
)
