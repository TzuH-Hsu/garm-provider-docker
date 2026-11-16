#!/usr/bin/env bash
#
# scripts/check-codenames.sh - keep private work-tracking codes out of the repo.
#
# Comments, test messages and docs should explain themselves or link to an
# issue or ADR. Short tracking codes such as M2, W1, WP3, B2, L9 or round-3
# mean nothing to a reader outside the original planning notes, so this script
# fails when one appears in a tracked file.
#
# GitHub line anchors (#L54) and branch prefixes in prose (codex/...) are
# allowed. SVG files are skipped because path data uses the same shapes
# (M41, H8). If a new legitimate use trips the check, reword it, or narrow the
# pattern here with a comment saying why.
#
# The script first runs the pattern against known good and bad samples, so a
# broken pattern fails loudly instead of passing everything.
#
# Exit status: 0 clean, 1 a code was found or the self-test failed,
# 2 git grep could not run.

set -euo pipefail

# Perl-compatible, for git grep -P.
readonly PATTERN='\b(?:M\d+(?:W\d+)?|WP\d+|[WFHB]\d+)\b|(?<![#-])\bL\d+\b|\bround-\d+\b|\b[Cc]odex\b(?!/)|\b[Rr]uling\b'

# Pathspecs left out of the scan.
readonly EXCLUDES=(
  ':!*.svg'
  ':!go.sum'
  ':!scripts/check-codenames.sh'
)

# matches_text TEXT: succeeds if PATTERN matches TEXT.
matches_text() {
  local dir rc=0
  dir="$(mktemp -d)"
  printf '%s\n' "$1" >"${dir}/sample"
  (cd "${dir}" && git grep --no-index -qP -e "${PATTERN}" -- sample) || rc=$?
  rm -rf "${dir}"
  if ((rc > 1)); then
    echo "check-codenames.sh: git grep -P failed (exit ${rc}); git needs PCRE support" >&2
    exit 2
  fi
  return "${rc}"
}

self_test() {
  local sample failed=0
  local -a must_match=(
    'fixed in M2' 'see M2W1' 'W3 follow-up' 'part of WP4' 'F2 regression'
    'H1 hardening' 'never delete (B2)' 'not none (L9)' 'round-3 review'
    'Codex review' 'per the codex reviewer' 'owner ruling' 'Ruling: keep it'
  )
  local -a must_not_match=(
    'see external.go#L54' 'lines #L127-L135' 'line 54' 'codex/fix-thing'
    'W3C spec' 'RE2 syntax' 'staticcheck ST1000' 'QF1008' 'nonce 0a1b2c3d'
    'roundtrip'
  )
  for sample in "${must_match[@]}"; do
    if ! matches_text "${sample}"; then
      echo "check-codenames.sh: self-test: expected a match: ${sample}" >&2
      failed=1
    fi
  done
  for sample in "${must_not_match[@]}"; do
    if matches_text "${sample}"; then
      echo "check-codenames.sh: self-test: unexpected match: ${sample}" >&2
      failed=1
    fi
  done
  return "${failed}"
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

self_test || exit 1

rc=0
git grep -nIP -e "${PATTERN}" -- "${EXCLUDES[@]}" || rc=$?
case "${rc}" in
  0)
    echo "check-codenames.sh: found private tracking codes (above); reword them or link an issue or ADR" >&2
    exit 1
    ;;
  1)
    exit 0
    ;;
  *)
    echo "check-codenames.sh: git grep failed (exit ${rc})" >&2
    exit 2
    ;;
esac
