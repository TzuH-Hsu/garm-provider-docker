// Command garm-provider-docker is a GARM external provider that manages GitHub
// Actions runners as Docker containers on a single Docker host.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	execcommon "github.com/cloudbase/garm-provider-common/execution/common"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/garmenv"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/provider"
)

func main() {
	// The detached status watcher is this binary re-executed with a hidden
	// argument. It reads its request from stdin, not GARM's environment.
	if len(os.Args) == 2 && os.Args[1] == provider.StatusWatcherArg {
		slog.SetDefault(logging.New())
		os.Exit(provider.RunStatusWatcher(os.Stdin))
	}
	os.Exit(run())
}

func run() int {
	ctx := context.Background()

	slog.SetDefault(logging.New())

	command := os.Getenv("GARM_COMMAND")
	slog.InfoContext(ctx, "starting garm-provider-docker", "command", command)

	env, err := garmenv.GetEnvironment()
	if err != nil {
		slog.ErrorContext(ctx, "failed to read execution environment", "command", command, "error", err)
		return 1
	}

	cfg, err := config.Load(env.ProviderConfigFile)
	if err != nil {
		slog.ErrorContext(ctx, "failed to load provider config", "command", command, "error", err)
		return 1
	}

	// The log file path lives in the config, so everything logged above went to
	// stderr only.
	if cfg.Logging.File != "" {
		logger, closer := logging.NewWithFile(cfg.Logging.File)
		defer closer.Close()
		slog.SetDefault(logger)
	}

	cli, err := docker.NewMobyClient(cfg.DockerHost)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create docker client", "command", command, "error", err)
		return 1
	}

	prov, err := provider.New(cli, cfg, env.ControllerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to construct provider", "command", command, "error", err)
		return 1
	}

	result, err := env.Run(ctx, prov)
	if err != nil {
		exitCode := execcommon.ResolveErrorToExitCode(err)
		// GARM treats not found on delete as success, so a normal reap must not
		// read as a failure (docs/garm-contract.md).
		if exitCode == execcommon.ExitCodeNotFound {
			slog.InfoContext(ctx, "command completed: instance already gone", "command", command, "exit_code", exitCode, "error", err)
		} else {
			slog.ErrorContext(ctx, "command failed", "command", command, "exit_code", exitCode, "error", err)
		}
		return exitCode
	}

	if result != "" {
		fmt.Fprintln(os.Stdout, result)
	}
	slog.InfoContext(ctx, "command completed", "command", command, "exit_code", 0)
	return 0
}
