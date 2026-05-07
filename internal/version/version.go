// Package version exposes build metadata.
package version

var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

func String() string {
	return Version + " (" + Commit + ", built " + BuildTime + ")"
}
