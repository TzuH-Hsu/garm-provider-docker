// Package garmenv reads the GARM external provider environment. It wraps
// garm-provider-common's execution.GetEnvironment and differs in one rule.
//
// Under interface v0.1.1, garm-provider-common v0.1.9 rejects DeleteInstance,
// GetInstance, StartInstance and StopInstance when GARM_POOL_ID is empty, yet GARM
// v0.2.1 sends it empty for those commands on a scale-set instance. A scale-set
// runner could then never be deleted: GARM retries forever in pending_delete.
//
// This package accepts an empty pool ID for those four commands, none of which
// reads it: instances are resolved by name label (ADR-004). Every other check, and
// all of interface v0.1.0, goes to the library unchanged.
package garmenv

import (
	"fmt"
	"os"
	"slices"

	"github.com/cloudbase/garm-provider-common/execution"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	executionv011 "github.com/cloudbase/garm-provider-common/execution/v0.1.1"
)

var poolIDOptionalCommands = []execcommon.ExecutionCommand{
	execcommon.DeleteInstanceCommand,
	execcommon.GetInstanceCommand,
	execcommon.StartInstanceCommand,
	execcommon.StopInstanceCommand,
}

// validationPoolIDPlaceholder fills an empty pool ID on the copy passed to the
// library's Validate, so its other checks still run. It is never returned.
const validationPoolIDPlaceholder = "scale-set-instance-without-pool-id"

// GetEnvironment reads and validates the environment GARM set for this invocation,
// including the BootstrapInstance on stdin for CreateInstance.
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

// getEnvironmentV011 mirrors executionv011.GetEnvironment from garm-provider-common
// v0.1.9, replacing only the validation step. Recheck it when that dependency moves.
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

// validateV011 runs the library's Validate, tolerating an empty pool ID for
// poolIDOptionalCommands. env is a copy, so the placeholder stays in this function.
func validateV011(env executionv011.EnvironmentV011) error {
	if env.PoolID == "" && slices.Contains(poolIDOptionalCommands, env.Command) {
		env.PoolID = validationPoolIDPlaceholder
	}
	return env.Validate()
}
