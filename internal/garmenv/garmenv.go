// Package garmenv reads the GARM external-provider execution environment.
//
// It is a thin wrapper over garm-provider-common's execution.GetEnvironment
// that differs in exactly one rule, for exactly one interface version:
//
// Under interface v0.1.1, garm-provider-common v0.1.9 (the version GARM v0.2.1
// itself pins, and the latest release) rejects DeleteInstance, GetInstance,
// StartInstance and StopInstance when GARM_POOL_ID is empty
// (execution/v0.1.1/execution.go, EnvironmentV011.Validate). GARM v0.2.1 sends
// an EMPTY GARM_POOL_ID for every one of those commands on a scale-set
// instance: runner/providers/v0.1.1/external.go fills GARM_POOL_ID from
// PoolInfo.ID, and the scale-set callers (workers/provider/instance_manager.go
// getProviderBaseParams, workers/scaleset/scaleset.go consolidateRunnerState)
// populate only ControllerInfo, leaving PoolInfo zero. The library's
// validation therefore makes a scale-set runner impossible to delete — GARM
// retries the delete forever and the instance is stuck in pending_delete.
//
// This package relaxes that single check: for those four commands an empty
// pool ID is accepted. Every other check is still delegated to the library's
// own Validate, unchanged. None of the four commands consumes the pool ID —
// EnvironmentV011.Run passes only GARM_INSTANCE_ID to the provider, and this
// provider resolves instances by the controller-scoped instance-name label
// (ADR-004), never by pool — so the relaxation loses no information.
// CreateInstance and ListInstances keep requiring a pool ID: GARM always sends
// one for them (the real pool UUID, or the scale-set pseudo pool ID), and the
// pool-ID label is what scopes a list to one pool/scale set.
//
// Interface v0.1.0 (and an unset GARM_INTERFACE_VERSION) is delegated to the
// library verbatim: its validation never required a pool ID for these
// commands.
package garmenv

import (
	"fmt"
	"os"
	"slices"

	"github.com/cloudbase/garm-provider-common/execution"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	executionv011 "github.com/cloudbase/garm-provider-common/execution/v0.1.1"
)

// poolIDOptionalCommands are the v0.1.1 commands for which GARM v0.2.x sends
// an empty GARM_POOL_ID on scale-set instances and whose dispatch never reads
// the pool ID.
var poolIDOptionalCommands = []execcommon.ExecutionCommand{
	execcommon.DeleteInstanceCommand,
	execcommon.GetInstanceCommand,
	execcommon.StartInstanceCommand,
	execcommon.StopInstanceCommand,
}

// validationPoolIDPlaceholder stands in for an absent pool ID ONLY on the
// throwaway copy handed to the library's Validate, so every other library
// check still runs. It never reaches the returned environment.
const validationPoolIDPlaceholder = "scale-set-instance-without-pool-id"

// GetEnvironment reads and validates the execution environment GARM set for
// this invocation (env vars, plus the BootstrapInstance on stdin for
// CreateInstance). The returned execution.Environment is the library's own
// type, so its Run dispatches exactly as before.
func GetEnvironment() (execution.Environment, error) {
	if os.Getenv("GARM_INTERFACE_VERSION") != execcommon.Version011 {
		return execution.GetEnvironment()
	}

	env, err := getEnvironmentV011()
	if err != nil {
		return execution.Environment{}, err
	}
	return execution.Environment{
		EnvironmentV011:    env,
		ProviderConfigFile: env.ProviderConfigFile,
		ControllerID:       env.ControllerID,
		InterfaceVersion:   execcommon.Version011,
	}, nil
}

// getEnvironmentV011 mirrors executionv011.GetEnvironment (garm-provider-common
// v0.1.9) field for field, swapping only the validation step.
func getEnvironmentV011() (executionv011.EnvironmentV011, error) {
	env := executionv011.EnvironmentV011{
		Command:            execcommon.ExecutionCommand(os.Getenv("GARM_COMMAND")),
		ControllerID:       os.Getenv("GARM_CONTROLLER_ID"),
		PoolID:             os.Getenv("GARM_POOL_ID"),
		ProviderConfigFile: os.Getenv("GARM_PROVIDER_CONFIG_FILE"),
		InstanceID:         os.Getenv("GARM_INSTANCE_ID"),
		ExtraSpecs:         os.Getenv("GARM_POOL_EXTRASPECS"),
	}

	bootstrapParams, err := execcommon.GetBoostrapParamsFromStdin(env.Command)
	if err != nil {
		return executionv011.EnvironmentV011{}, fmt.Errorf("failed to get bootstrap params: %w", err)
	}
	env.BootstrapParams = bootstrapParams

	if err := validateV011(env); err != nil {
		return executionv011.EnvironmentV011{}, fmt.Errorf("failed to validate execution environment: %w", err)
	}
	return env, nil
}

// validateV011 runs the library's EnvironmentV011.Validate, except that an
// empty pool ID is tolerated for poolIDOptionalCommands. It validates a copy
// (env is passed by value), so the caller's environment is never altered.
func validateV011(env executionv011.EnvironmentV011) error {
	if env.PoolID == "" && slices.Contains(poolIDOptionalCommands, env.Command) {
		env.PoolID = validationPoolIDPlaceholder
	}
	return env.Validate()
}
