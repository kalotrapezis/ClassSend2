// Package buildinfo holds build-time identity (version + build timestamp).
// Both fields are injected via -ldflags "-X classsend/internal/buildinfo.X=Y"
// from build.bat, so every shipped binary reports the same string and we can
// tell at a glance which build is running.
package buildinfo

// Version is set at build time. The default fallback is "dev" so go run /
// unit tests still produce something readable.
var Version = "dev"

// BuildTime is an ISO-8601 timestamp set at build time. Falls back to "" if
// the build script forgot to set it.
var BuildTime = ""

// String returns a one-line "<version> (<buildtime>)" summary suitable for
// log lines and the --version flag.
func String() string {
	if BuildTime == "" {
		return Version
	}
	return Version + " (built " + BuildTime + ")"
}
