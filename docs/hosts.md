# Host requirements

The provider needs a Linux host running Docker Engine. Only amd64 has been tested
end to end. See the README's
[verification status](../README.md#verification-status).

## Docker daemon

Never let the daemon listen on TCP (`dockerd -H tcp://...`, or `hosts` in
`daemon.json`). A TCP-exposed socket bypasses every containment this project
offers.

## Network capacity

Each allocation, in every mode, takes one network from the daemon's
`default-address-pools`. Size the pools for the number of concurrent jobs you
expect. Exhausting them is a host-level limit, independent of runner and DinD
resource limits.

## Storage filesystem

- **DinD storage driver:** autodetection inside a container can be unreliable when
  the host filesystem is btrfs or overlay-on-overlay. Set `storage_driver`
  explicitly. `overlay2` is the default, and `vfs` is slower but more compatible.
- **`pnpm install`:** the pnpm store and workspace are separate volumes. On XFS
  with reflink, or btrfs with kernel 5.18 or newer, installs are cheap. On others,
  every install copies the whole `node_modules` tree. See
  [package store installs](package-store-installs.md).
- **Limited storage:** cache volumes are never deleted automatically, so plan to
  reclaim them. See [How it works](how-it-works.md#caches).

## Sysbox

`sysbox-runc` needs Sysbox installed on the host. Where it cannot be installed,
`privileged-sidecar` is the only DinD option, so that host accepts its risk. See
[ADR-001](adr/ADR-001-dind-strategy.md).
