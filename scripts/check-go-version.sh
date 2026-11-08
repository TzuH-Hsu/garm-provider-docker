#!/usr/bin/env bash
#
# scripts/check-go-version.sh - keep every place that picks a Go version in step.
#
# The `toolchain` line in go.mod is the source of truth for the Go that builds
# release binaries. Two other things have to agree with it:
#
#   - the `golang` tag on the builder stage of the Dockerfile, which is a
#     second copy of the version. Renovate bumps it together with the
#     `toolchain` line (.github/renovate.json5); Dependabot ignores it;
#   - the `go` command that is actually on PATH, so a CI or release job fails
#     loudly if its setup step ever stops honouring the `toolchain` line.
#
# The `go` line is held at one language version on purpose (see go.mod). A
# module update can raise it through `go get` with no other sign, which changes
# language and GODEBUG defaults, so this script checks it too.
#
# Exit status: 0 all agree, 1 a mismatch, 2 a version could not be read.

set -euo pipefail

# Change this together with the `go` line in go.mod, as a deliberate decision.
readonly EXPECTED_GO_LINE="1.25.0"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

toolchain="$(awk '$1 == "toolchain" { print $2 }' go.mod)"
if [[ ! "${toolchain}" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "check-go-version.sh: go.mod has no usable 'toolchain goX.Y.Z' line (got '${toolchain}')" >&2
  exit 2
fi
want="${toolchain#go}"

golang="$(awk '$1 == "go" { print $2; exit }' go.mod)"

# Matches `FROM [--platform=...] golang:X.Y.Z[@sha256:...] [AS name]`.
image="$(sed -n -E 's/^FROM( --[^ ]+)* golang:([0-9]+\.[0-9]+\.[0-9]+)(@sha256:[0-9a-f]{64})?( .*)?$/\2/p' Dockerfile)"
if [ -z "${image}" ] || [ "$(wc -l <<<"${image}")" -ne 1 ]; then
  echo "check-go-version.sh: expected exactly one 'FROM golang:X.Y.Z' line in Dockerfile (got '${image}')" >&2
  exit 2
fi

status=0
if [ "${golang}" != "${EXPECTED_GO_LINE}" ]; then
  echo "check-go-version.sh: go.mod 'go' line is '${golang}', expected ${EXPECTED_GO_LINE}; a module update may have raised it" >&2
  status=1
fi

if [ "${image}" != "${want}" ]; then
  echo "check-go-version.sh: Dockerfile builds with golang:${image}, go.mod toolchain is ${toolchain}" >&2
  status=1
fi

running="$(go env GOVERSION)"
if [ "${running}" != "${toolchain}" ]; then
  echo "check-go-version.sh: 'go' on PATH is ${running}, go.mod toolchain is ${toolchain}" >&2
  status=1
fi

if [ "${status}" -eq 0 ]; then
  echo "check-go-version.sh: go.mod toolchain, Dockerfile and go on PATH all use ${toolchain}; go line is ${golang}"
fi
exit "${status}"
