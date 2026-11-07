#!/usr/bin/env bash
#
# scripts/govulncheck.sh - vulnerability gate for the Go module and toolchain.
#
# Runs govulncheck in source mode at symbol level and applies this policy:
#
#   reachable and a fixed version exists  -> FAIL (exit 1)
#   reachable and no fixed version        -> notice only
#   not reachable                         -> counted, not listed
#
# "Reachable" means govulncheck found a call path to a vulnerable symbol in
# this module (the first frame of a finding names a function). A finding that
# stops at the package or module level means the vulnerable code is imported
# or required but never called.
#
# The "no fixed version" case exists because github.com/docker/docker is held
# at its terminal v28.5.2+incompatible (see the comment in go.mod), so advisories
# against it cannot be resolved by an upgrade. They stay visible as notices
# instead of turning every run red.
#
# govulncheck is pinned to an exact version and run with `go run`, so it is
# built by the Go toolchain that go.mod selects. A govulncheck built by an
# older Go cannot load packages from a newer standard library.
#
# Environment:
#   GOVULNCHECK_BIN  Path to a govulncheck binary to use instead of `go run`.
#                    Meant for testing the policy under another GOTOOLCHAIN.
#
# Exit status: 0 policy satisfied, 1 policy violated, 2 the scan itself failed.

set -euo pipefail

readonly GOVULNCHECK_VERSION="v1.9.0"

if ! command -v jq >/dev/null 2>&1; then
  echo "govulncheck.sh: jq is required" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

report="$(mktemp)"
trap 'rm -f "${report}"' EXIT

# With -format json govulncheck exits 0 even when it reports findings, so a
# non-zero status here means the scan did not run, not that it found something.
if [ -n "${GOVULNCHECK_BIN:-}" ]; then
  scan=("${GOVULNCHECK_BIN}")
else
  scan=(go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}")
fi
if ! "${scan[@]}" -format json ./... >"${report}"; then
  echo "govulncheck.sh: govulncheck failed to scan the module" >&2
  exit 2
fi

echo "Scan configuration:"
jq -r 'select(.config) | "  \(.config.go_version), govulncheck \(.config.scanner_version), db \(.config.db_last_modified)"' "${report}"

# One record per advisory: whether any finding reaches a called symbol, the
# fixed version if the database lists one, and the module that is affected.
summary="$(jq -s '
  (map(select(.osv) | .osv) | INDEX(.id)) as $osv
  | map(select(.finding) | .finding)
  | group_by(.osv)
  | map({
      id: .[0].osv,
      module: .[0].trace[0].module,
      fixed: (map(.fixed_version // empty) | first // ""),
      reachable: any(.[]; .trace[0].function != null),
      symbols: (map(select(.trace[0].function != null)
                    | "\(.trace[0].package).\(.trace[0].function)")
                | unique | .[0:3] | join(", ")),
      summary: ($osv[.[0].osv].summary // "")
    })
' "${report}")"

emit() { # level, message
  if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
    echo "::$1::$2"
  else
    echo "$1: $2"
  fi
}

fail_count=0
while IFS=$'\t' read -r id module fixed symbols text; do
  emit error "${id} (${module}) is reachable via ${symbols}; fixed in ${fixed}. ${text}"
  fail_count=$((fail_count + 1))
done < <(jq -r '.[] | select(.reachable and .fixed != "")
                | [.id, .module, .fixed, .symbols, .summary] | @tsv' <<<"${summary}")

while IFS=$'\t' read -r id module symbols text; do
  emit notice "${id} (${module}) is reachable via ${symbols}; no fixed version exists. ${text}"
done < <(jq -r '.[] | select(.reachable and .fixed == "")
                | [.id, .module, .symbols, .summary] | @tsv' <<<"${summary}")

unreachable="$(jq '[.[] | select(.reachable | not)] | length' <<<"${summary}")"
echo "Advisories that match a dependency but are not called: ${unreachable}"

if [ "${fail_count}" -gt 0 ]; then
  echo "govulncheck.sh: ${fail_count} reachable advisory(ies) have a fix available" >&2
  exit 1
fi
echo "govulncheck.sh: no reachable advisory has a fix available"
