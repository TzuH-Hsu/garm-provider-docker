package provider

import (
	"context"
	"fmt"
)

// RemoveAllInstances is the manual rescue operation (ADR-004): it removes every
// job-scoped resource for this controller — runner containers, DinD sidecars
// (WP3), job networks, and job-scoped volumes (workspace, and WP3's socket/
// dind-state) — in the ADR-004 order (all containers before their networks, so
// no network is removed while it still has active endpoints). It is
// label-scoped to this controller, never a global wipe of the Docker host.
//
// It uses the single authoritative ADR-004 predicate (managed=true AND
// controller-id AND has instance-name AND NOT cache=true), so cache and
// diagnostic volumes (ADR-003) are never touched — they carry no instance-name
// label and are excluded structurally.
//
// Best-effort, but NOT silent: per-resource errors are joined and the
// teardown continues rather than failing fast on the first one — one stuck
// container/volume/network never strands the rest of the sweep — but any
// joined error IS surfaced to the caller once the sweep completes, rather
// than swallowed to a nil return. This is an operator-invoked rescue command;
// an operator who ran it to clean up leftovers needs to be able to tell a
// genuinely clean sweep (nil) apart from one that left something behind
// (non-nil, and — via main.go's execcommon.ResolveErrorToExitCode — a
// non-zero exit code with the underlying error on stderr), rather than
// having that distinction buried in a log line only a human watching stdout
// at the time would ever see.
func (p *Provider) RemoveAllInstances(ctx context.Context) error {
	if err := p.topo.TeardownAll(ctx); err != nil {
		return fmt.Errorf("garm-provider-docker: RemoveAllInstances: best-effort teardown left resources behind: %w", err)
	}
	return nil
}
