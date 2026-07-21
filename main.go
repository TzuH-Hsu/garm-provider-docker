// Command garm-provider-docker is a GARM external provider that manages
// GitHub Actions runners as Docker containers on a single Docker host.
//
// It is a thin dispatcher: all it does is read the GARM execution
// environment, wire this repo's Provider implementation into it, and run
// the command GARM requested. All actual logic lives under internal/.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cloudbase/garm-provider-common/execution"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/provider"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx := context.Background()

	env, err := execution.GetEnvironment()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read execution environment: %s\n", err)
		return 1
	}

	cfg, err := config.Load(env.ProviderConfigFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load provider config: %s\n", err)
		return 1
	}

	cli, err := docker.NewMobyClient(cfg.DockerHost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create docker client: %s\n", err)
		return 1
	}

	prov, err := provider.New(cli, cfg, env.ControllerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to construct provider: %s\n", err)
		return 1
	}

	result, err := env.Run(ctx, prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return execcommon.ResolveErrorToExitCode(err)
	}

	if result != "" {
		fmt.Fprintln(os.Stdout, result)
	}
	return 0
}
