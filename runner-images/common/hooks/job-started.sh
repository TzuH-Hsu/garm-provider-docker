#!/usr/bin/env bash
# job-started.sh - job-started hook dispatcher (ADR-002).
#
# The runner image points ACTIONS_RUNNER_HOOK_JOB_STARTED at this file, so the
# GitHub Actions runner runs it before the first step of every job, as the
# runner user, and shows its output in the job log. It runs every
# job-started.d/*.sh next to it, in name order, to catch state a previous job
# left behind that would make this job pass without doing its work.
#
# The runner ignores the shebang and runs the file as
#   bash --noprofile --norc -e -o pipefail <file>
# so errexit and pipefail are on. Every command here that can fail is guarded,
# because an error here fails "Set up runner" and with it every job.
#
# Drop-in contract (each check runs as `bash <file>`; it needs no exec bit):
#   exit 0   nothing wrong
#   exit 10  damage detected. The check has already printed an annotation
#            such as `::error title=...::<what, why, how to fix>`.
#   other    the check itself malfunctioned (a crash, a missing tool, a
#            timeout). The dispatcher prints a ::warning:: naming the check
#            and carries on, so a buggy check cannot fail every job.
#
# GARM_JOB_HOOKS selects what damage does:
#   fail   (default) exit 1 after all checks ran: "Set up runner" fails, no
#          step runs, and the job is red instead of falsely green.
#   warn   print the detections, exit 0.
#   off    run nothing.
# An unset or empty value means fail. An unknown value also means fail, with
# a warning that names it: the checks guard against silent false greens, so a
# typo such as "wran" must not quietly switch them off, and it fails only a
# job whose state really is damaged.
#
# GARM_JOB_HOOKS_TIMEOUT is the per-check limit in seconds (default 30), so a
# stuck check cannot hold the job. It is read as plain digits and never
# evaluated.

set -u

readonly DAMAGE_EXIT=10
readonly DEFAULT_TIMEOUT_SECONDS=30
readonly KILL_AFTER_SECONDS=5

dropin_dir="${BASH_SOURCE[0]%/*}"
[ "$dropin_dir" = "${BASH_SOURCE[0]}" ] && dropin_dir=.
dropin_dir="$dropin_dir/job-started.d"

# Print s as a single safe token: lower-cased, limited to a small charset and
# 32 characters, so an operator-supplied value can never start a workflow
# command of its own or break the annotation line.
safe_token() {
  local s
  s=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  s=${s:0:32}
  printf '%s' "${s//[^a-z0-9_.-]/?}"
}

mode=$(printf '%s' "${GARM_JOB_HOOKS:-fail}" | tr '[:upper:]' '[:lower:]')
case "$mode" in
  fail | warn) ;;
  off) exit 0 ;;
  *)
    echo "::warning title=Unknown GARM_JOB_HOOKS value::GARM_JOB_HOOKS=\"$(safe_token "$mode")\" is not one of fail, warn or off; using fail."
    mode=fail
    ;;
esac

limit="${GARM_JOB_HOOKS_TIMEOUT:-$DEFAULT_TIMEOUT_SECONDS}"
case "$limit" in
  '' | *[!0-9]*) limit=$DEFAULT_TIMEOUT_SECONDS ;;
esac
[ "$limit" -gt 0 ] 2>/dev/null || limit=$DEFAULT_TIMEOUT_SECONDS

have_timeout=0
command -v timeout >/dev/null 2>&1 && have_timeout=1

# Run one check, leaving its exit status in rc.
run_check() {
  rc=0
  if [ "$have_timeout" -eq 1 ]; then
    timeout --kill-after="$KILL_AFTER_SECONDS" "$limit" bash --noprofile --norc "$1" || rc=$?
  else
    bash --noprofile --norc "$1" || rc=$?
  fi
}

damaged=
rc=0
# find + sort, not a glob: the order is bytewise whatever the locale, and a
# missing directory yields nothing instead of an error.
while IFS= read -r check; do
  [ -f "$check" ] || continue
  name=${check##*/}
  run_check "$check"
  case "$rc" in
    0) ;;
    "$DAMAGE_EXIT") damaged="${damaged:+$damaged, }$name" ;;
    124 | 137)
      echo "::warning title=Job-started check timed out::$name timed out after ${limit}s and was skipped."
      ;;
    *)
      echo "::warning title=Job-started check malfunctioned::$name exited with status $rc and was ignored."
      ;;
  esac
done < <(find "$dropin_dir" -maxdepth 1 -name '*.sh' 2>/dev/null | LC_ALL=C sort)

if [ -n "$damaged" ]; then
  if [ "$mode" = fail ]; then
    echo "::error title=Job stopped by a job-started check::$damaged reported damage that would make this job pass without doing its work, so the job is stopped before its first step. The runner operator can set GARM_JOB_HOOKS=warn to run jobs anyway."
    exit 1
  fi
  echo "::warning title=Job-started check reported damage::$damaged reported damage. GARM_JOB_HOOKS=warn, so the job continues."
fi
exit 0
