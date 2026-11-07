#!/usr/bin/env bash
#
# scripts/check-go-version.sh - keep every place that picks a Go version in step.
#
# The `toolchain` line in go.mod is the source of truth for the Go that builds
# release binaries. Two other things have to agree with it:
#
#   - the `golang` tag on the builder stage of the Dockerfile, which is a
#     second copy of the version and is not updated by Dependabot;
#   - the `go` command that is actually on PATH, so a CI or release job fails
#     loudly if its setup step ever stops honouring the `toolchain` line.
#
# Exit status: 0 all agree, 1 a mismatch, 2 a version could not be read.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

toolchain="$(awk '$1 == "toolchain" { print $2 }' go.mod)"
if [[ ! "${toolchain}" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "check-go-version.sh: go.mod has no usable 'toolchain goX.Y.Z' line (got '${toolchain}')" >&2
  exit 2
fi
want="${toolchain#go}"

# Matches `FROM [--platform=...] golang:X.Y.Z[@sha256:...] [AS name]`.
image="$(sed -n -E 's/^FROM( --[^ ]+)* golang:([0-9]+\.[0-9]+\.[0-9]+)(@sha256:[0-9a-f]{64})?( .*)?$/\2/p' Dockerfile)"
if [ -z "${image}" ] || [ "$(wc -l <<<"${image}")" -ne 1 ]; then
  echo "check-go-version.sh: expected exactly one 'FROM golang:X.Y.Z' line in Dockerfile (got '${image}')" >&2
  exit 2
fi

status=0
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
  echo "check-go-version.sh: go.mod toolchain, Dockerfile and go on PATH all use ${toolchain}"
fi
exit "${status}"
