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

	prov := provider.New()

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
