#!/usr/bin/env bash
# 10-pnpm-store.sh - job-started check: a 0-byte pnpm binary in the pnpm store.
#
# Before pnpm 11.22.0 a bootstrap pnpm can empty the `pnpm` binary it switches
# to inside the persistent store (docs/package-store-installs.md). Every later
# job of the repository then runs a 0-byte `pnpm` that prints nothing and exits
# 0, so install, lint, test and build steps all pass without running anything.
#
# Exit 0: nothing wrong, or no pnpm store to look at. Exit 10: damage found,
# after printing a plain message. The dispatcher (job-started.sh) runs this as
# `bash <file>` and turns the message into an error or warning annotation
# according to GARM_JOB_HOOKS, so this script prints no workflow commands. It
# is read-only and stays cheap (a stat and a find over a handful of
# directories).
#
# The predicate is the one the manual operator check in the docs uses:
#   find <store>/v11/links/@pnpm -path '*/node_modules/@pnpm/*/pnpm' -type f -size 0

set -u

readonly DOCS_URL="https://github.com/TzuH-Hsu/garm-provider-docker/blob/main/docs/package-store-installs.md#pnpm-switching-its-own-version-pnpm-11-before-1122"

store="${pnpm_config_store_dir:-${npm_config_store_dir:-}}"
[ -n "$store" ] && [ -d "$store" ] || exit 0

links="$store/v11/links/@pnpm"
[ -d "$links" ] || exit 0

# find exits non-zero on an unreadable directory even when it printed a hit, so
# its status is ignored: the hit, if any, is what counts. -quit stops at the
# first one.
hit=$(find "$links" -path '*/node_modules/@pnpm/*/pnpm' -type f -size 0 -print -quit 2>/dev/null || true)
[ -n "$hit" ] || exit 0

# Show the entry relative to the store, as one printable line (the name comes
# from a store a job could write to).
entry=$(printf '%s' "${hit#"$store"/}" | tr -c '[:print:]' '?')

echo "The repository's persistent pnpm store holds a 0-byte pnpm binary ($entry). Every pnpm command in this job would print nothing and exit 0, so install, lint, test and build steps would pass without running. Use pnpm/action-setup v6.1.0 or newer (it bootstraps pnpm 11.22.0 or newer). The runner operator must also remove this repository's pnpm cache volume, because newer pnpm versions do not repair the emptied file. Details: $DOCS_URL"
exit 10
