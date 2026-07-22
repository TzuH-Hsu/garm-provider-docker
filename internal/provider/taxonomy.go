// Package provider — this file records the M3-W2 error-taxonomy audit: how
// every command's failure paths map to garm-provider-common's exit codes,
// and how (and where) ProviderInstance.ProviderFault can legitimately reach
// GARM. It intentionally adds no new exported symbols; the mapping was
// already centralized (not scattered) before this audit, so nothing needed
// consolidating into a new helper — this file exists so the audit's findings
// live next to the code they describe rather than only in a commit message.
//
// # Exit-code mapping (garm-provider-common/execution/common.ResolveErrorToExitCode)
//
// The resolver checks errors.Is(err, gErrors.ErrNotFound) -> exit 30 and
// errors.Is(err, gErrors.ErrDuplicateEntity) -> exit 31 (anything else that
// is non-nil -> exit 1). Both sentinel checks work through each error's own
// Is(error) bool method, which is a TYPE check (target.(*NotFoundError) /
// target.(*DuplicateUserError)), not a message comparison — so any error in
// the chain that IS one of those two concrete types satisfies errors.Is
// regardless of %w-wrapping depth. This is why every command below can wrap
// notFoundError()/the duplicate error with additional %w context (instance
// name, operation) without breaking the exit-code match.
//
// Audited call sites, one per command:
//
//   - CreateInstance (create.go): the ONLY duplicate source is
//     topology.CreateClaimNetwork's dupErr return, built from
//     gErrors.NewDuplicateUserError — exit 31. Every other CreateInstance
//     failure (image pull, credential fetch, container create/start,
//     cache-volume identity mismatch, ...) is a generic wrapped error — exit 1.
//     CreateInstance has NO not-found case (there is nothing to "not find" when
//     creating).
//   - DeleteInstance (delete.go): resolveInstanceName returning found=false,
//     or TeardownAllocation reporting found=false (a race with a concurrent
//     delete/sweep), both return notFoundError() — exit 30, which GARM treats
//     as success (research.md §1.F). Every other failure (a teardown step
//     erroring) is wrapped generic — exit 1.
//   - GetInstance (get.go): resolve() returning found=false returns
//     notFoundError() — exit 30. No duplicate case.
//   - ListInstances (list.go): no not-found/duplicate semantics — an empty
//     result is a valid, successful list, not an error.
//   - RemoveAllInstances (remove_all.go): best-effort rescue; any
//     leftover-resource error is wrapped generic — exit 1. No not-found/
//     duplicate semantics (it never targets one instance).
//   - Start / Stop (start.go, stop.go): resolve() returning found=false
//     returns notFoundError() — exit 30, mirroring GetInstance/DeleteInstance
//     exactly (both route through the SAME resolve() helper, resolver.go).
//   - The four v0.1.1-only methods (v011.go): ValidatePoolInfo's failures are
//     schema/config validation errors (exit 1) — not-found/duplicate has no
//     meaning for a dry-run validation call. GetSupportedInterfaceVersions/
//     GetConfigJSONSchema/GetExtraSpecsJSONSchema have no not-found/duplicate
//     failure mode at all.
//
// Conclusion: the mapping is CONSISTENT — every command that can meaningfully
// hit "the instance isn't there" (Delete/Get/Start/Stop) does so through the
// same notFoundError() helper, and the ONE duplicate source (the claim-marker
// network's 409) is centralized in topology.CreateClaimNetwork. No command
// returns a bare, unwrapped gErrors sentinel with lost context, and no command
// invents its own competing not-found/duplicate signal.
//
// # ProviderFault (research.md §1.E) reachability
//
// params.ProviderInstance.ProviderFault can only reach GARM through a
// SUCCESSFUL RPC: garm-provider-common's execution.Run (both v0.1.0 and
// v0.1.1, vendored at
// github.com/cloudbase/garm-provider-common@v0.1.9/execution/{v0.1.0,v0.1.1}/execution.go)
// marshals and returns the ProviderInstance JSON ONLY on the nil-error path —
//
//	instance, err := provider.CreateInstance(ctx, e.BootstrapParams)
//	if err != nil {
//	        return "", fmt.Errorf("failed to create instance in provider: %w", err)
//	}
//	asJs, err := json.Marshal(instance)
//	...
//
// — on a non-nil error the ProviderInstance value (and any ProviderFault it
// carried) is discarded entirely; only the error's text reaches GARM's stderr
// capture. The identical shape appears in the GetInstance/ListInstances arms.
//
// Consequence, verified against that vendored source rather than assumed:
// CreateInstance in THIS provider always returns a Go error on any failure
// (by design — a partial allocation is rolled back and the caller is told
// definitively "no instance exists", never a soft-failed running instance),
// so there is NO code path in this provider where CreateInstance could
// populate ProviderFault even if it tried — the field would never survive the
// trip. The one channel where ProviderFault DOES reach GARM in this provider
// is GetInstance (and, best-effort, ListInstances) reporting an existing
// container in InstanceError status: that is a SUCCESSFUL RPC (nil Go error)
// carrying a faulted status, exactly the shape execution.Run marshals through.
// toProviderInstance/toProviderInstanceFromSummary (resolver.go) populate
// ProviderFault from the container's own daemon-reported state (OOM flag,
// dead flag, exit code, the daemon's own Error string) whenever status maps to
// InstanceError — never from anything credential-bearing.
package provider
