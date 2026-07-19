// Package version holds the provider's build-time version metadata.
package version

// Version is the provider's version string. It is overridden at build time
// via:
//
//	-ldflags "-X github.com/TzuH-Hsu/garm-provider-docker/internal/version.Version=v1.2.3"
var Version = "v0.0.0-unknown"
