package provider

import (
	"context"
	"log"
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
// label and are excluded structurally. Best-effort: per-resource errors are
// joined and the teardown continues rather than failing fast on the first one.
// As a manual rescue path, it never fails fast: a per-resource removal error is
// logged (so an operator sees incomplete cleanup) and RemoveAllInstances still
// returns nil, matching the established best-effort contract.
func (p *Provider) RemoveAllInstances(ctx context.Context) error {
	if err := p.topo.TeardownAll(ctx); err != nil {
		log.Printf("garm-provider-docker: RemoveAllInstances: best-effort teardown reported errors: %v", err)
	}
	return nil
}
