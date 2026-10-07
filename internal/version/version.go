// Package version reports the build version of the rillock binary.
package version

// Version is replaced at build time with -ldflags "-X .../version.Version=v0.1.0".
var Version = "dev"
