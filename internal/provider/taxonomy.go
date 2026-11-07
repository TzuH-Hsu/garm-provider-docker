// Package provider reports failures to GARM through exit codes and ProviderFault.
//
// # Exit codes
//
// main.go maps a returned error with garm-provider-common's ResolveErrorToExitCode. It
// matches with errors.Is, so wrapping with %w keeps the code:
//
//   - 30, not found: notFoundError, from DeleteInstance, GetInstance, Start and Stop when
//     the instance does not resolve. GARM treats 30 from DeleteInstance as success.
//   - 31, duplicate: only CreateInstance, from topology.CreateClaimNetwork.
//   - 1: any other error. An empty ListInstances result is a success.
//
// # ProviderFault
//
// execution.Run discards the ProviderInstance when a command returns an error, so a
// failed CreateInstance cannot carry a ProviderFault. It reaches GARM only from
// GetInstance and ListInstances, on a successful call reporting InstanceError status,
// and it holds only what the daemon reports about the container's state, never a
// credential.
package provider
