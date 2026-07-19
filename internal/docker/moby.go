package docker

import (
	"fmt"

	"github.com/docker/docker/client"
)

// NewMobyClient builds a Client backed by the real moby Docker SDK,
// talking to the daemon at dockerHost (config.Config.DockerHost) and
// negotiating the API version against whatever that daemon actually
// speaks, rather than hardcoding a version this provider was built
// against.
//
// This takes a plain string rather than a config.Config so that package
// docker has no dependency on package config; the caller (internal/
// provider, in a later WP) is the one that knows about config.Config.
func NewMobyClient(dockerHost string) (Client, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client for host %q: %w", dockerHost, err)
	}
	return cli, nil
}

// Compile-time assertion that the real SDK client satisfies our narrow
// Client interface.
var _ Client = (*client.Client)(nil)
