# Security policy

## Reporting a vulnerability

Please report vulnerabilities through
[private vulnerability reporting](https://github.com/TzuH-Hsu/garm-provider-docker/security/advisories/new).
Do not open a public issue or discussion. If you cannot use that, email
security@tzuhsuan.dev.

Include the affected version, your configuration (`dind_mode` in particular),
steps to reproduce, and the impact you expect.

I aim to reply within 14 days. This is a one-maintainer project, so a fix may
take longer. I will tell you where it stands and credit you in the advisory
unless you prefer otherwise.

## Supported versions

Fixes go into the latest release only. Before `v1.0.0`, upgrade to get a fix.

## What counts as a vulnerability

Please report anything that breaks a boundary this project says it keeps. For
example:

- The host Docker socket is exposed to a runner or DinD sidecar.
- Runner credentials leak through the environment, `docker inspect`, logs or
  host disk.
- `extra_specs` gets past an operator's ceilings.
- Cleanup removes resources the provider does not own.
- One job reaches another job's workspace or network in a way the docs say it
  cannot.

## Known limits

These are documented and are not vulnerabilities by themselves:

- `privileged-sidecar` gives job code a path to root on the host.
- Jobs have passwordless `sudo` inside their own container.
- Jobs have outbound network access by default.
- Jobs for the same repository share caches, so one job can affect a later one.

The README's [security section](README.md#security-and-trust-boundary) and
[ADR-001](docs/adr/ADR-001-dind-strategy.md) explain the trust model. This
provider is meant for repositories you control, not for untrusted code.

Problems in GARM itself belong in
[cloudbase/garm](https://github.com/cloudbase/garm).
