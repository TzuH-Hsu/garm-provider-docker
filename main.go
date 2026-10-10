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
	// The detached runner status watcher (internal/provider/status_watch.go)
	// is this same binary re-executed with a hidden argument; it reads its
	// request from stdin and never touches GARM's execution environment.
	if len(os.Args) == 2 && os.Args[1] == provider.StatusWatcherArg {
		slog.SetDefault(logging.New())
		os.Exit(provider.RunStatusWatcher(os.Stdin))
	}
	os.Exit(run())
}

func run() int {
	ctx := context.Background()

	// Structured logging (M3-W2) is wired up FIRST, before anything else in
	// this process can log: slog.SetDefault installs the STDERR-only,
	// env-level-controlled logger every internal/provider and
	// internal/topology call site logs through (internal/logging's package
	// doc explains why a process-wide default, not an injected dependency).
	slog.SetDefault(logging.New())

	command := os.Getenv("GARM_COMMAND")
	slog.InfoContext(ctx, "starting garm-provider-docker", "command", command)

	// garmenv mirrors garm-provider-common's execution.GetEnvironment except
	// for one relaxation GARM v0.2.x scale sets need: no pool ID on
	// Delete/Get/Start/StopInstance under interface v0.1.1 (see its package doc).
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
		// exit 30 (not-found on DeleteInstance) is GARM's OWN idempotent
		// success signal, not an operational problem (research.md §1.F): log
		// it at info, not error, so a normal reap does not read as a failure
		// in stderr. Everything else (including exit 31 duplicate) logs at
		// error — a duplicate create is still worth an operator's attention.
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
