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
#   exit 0   nothing wrong, or nothing to check. Its output is discarded.
#   exit 10  damage detected. The check prints a plain, human-readable message
#            on stdout (what is wrong, why it matters, how to fix it). It does
#            not print ::error:: or ::warning:: itself: the dispatcher turns
#            the message into one annotation whose level follows the mode.
#   other    the check itself malfunctioned (a crash, a missing tool, a
#            timeout). The dispatcher prints a ::warning:: naming the check
#            and carries on, so a buggy check cannot fail every job.
#
# A check's output is captured, capped at MAX_MESSAGE_BYTES and only ever
# printed escaped inside an annotation, so a check cannot start a workflow
# command (::set-env, ::add-mask, ...) of its own.
#
# GARM_JOB_HOOKS selects what damage does. The provider sets it from the
# job_hooks setting (provider config, overridable per pool in extra_specs); an
# image built on this one may also set it directly:
#   fail   (default) the annotation is an error, and the hook exits 1 after all
#          checks ran: "Set up runner" fails, no step runs, and the job is red
#          instead of falsely green.
#   warn   the annotation is a warning, and the hook exits 0.
#   off    run nothing.
# An unset or empty value means fail. An unknown value also means fail, with
# a warning that names it: the checks guard against silent false greens, so a
# typo such as "wran" must not quietly switch them off, and it fails only a
# job whose state really is damaged.
#
# Each check gets CHECK_TIMEOUT_SECONDS, so a stuck check cannot hold the job.

set -u

readonly DAMAGE_EXIT=10
readonly CHECK_TIMEOUT_SECONDS=30
readonly KILL_AFTER_SECONDS=5
readonly MAX_MESSAGE_BYTES=4096

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

# Escape text for the data part of a workflow command, as the Actions toolkit
# does: % first, then CR and LF. Afterwards the text has no line break, so it
# cannot start a second command.
escape_data() {
  local s=$1
  s=${s//'%'/%25}
  s=${s//$'\r'/%0D}
  s=${s//$'\n'/%0A}
  printf '%s' "$s"
}

# Escape text for a property value (the title): data escapes plus : and ,.
escape_property() {
  local s
  s=$(escape_data "$1")
  s=${s//:/%3A}
  s=${s//,/%2C}
  printf '%s' "$s"
}

# 10-pnpm-store.sh -> pnpm-store
check_label() {
  local label=${1%.sh}
  printf '%s' "${label#[0-9][0-9]-}"
}

# annotate LEVEL TITLE MESSAGE prints one escaped workflow command.
annotate() {
  printf '::%s title=%s::%s\n' "$1" "$(escape_property "$2")" "$(escape_data "$3")"
}

mode=$(printf '%s' "${GARM_JOB_HOOKS:-fail}" | tr '[:upper:]' '[:lower:]')
case "$mode" in
  fail | warn) ;;
  off) exit 0 ;;
  *)
    annotate warning "Unknown GARM_JOB_HOOKS value" "GARM_JOB_HOOKS=\"$(safe_token "$mode")\" is not one of fail, warn or off; using fail."
    mode=fail
    ;;
esac

have_timeout=0
command -v timeout >/dev/null 2>&1 && have_timeout=1

# Run one check. Its exit status is left in rc and its combined output, capped
# at MAX_MESSAGE_BYTES, in output. The reader drains the pipe after the cap, so
# a chatty check is never killed by SIGPIPE and cannot flood the log or memory.
run_check() {
  rc=0
  output=$(
    if [ "$have_timeout" -eq 1 ]; then
      timeout --kill-after="$KILL_AFTER_SECONDS" "$CHECK_TIMEOUT_SECONDS" bash --noprofile --norc "$1" 2>&1 </dev/null
    else
      bash --noprofile --norc "$1" 2>&1 </dev/null
    fi | { head -c $((MAX_MESSAGE_BYTES + 1)); cat >/dev/null; }
    exit "${PIPESTATUS[0]}"
  ) || rc=$?
  if [ "$(printf '%s' "$output" | LC_ALL=C wc -c | tr -d ' ')" -gt "$MAX_MESSAGE_BYTES" ]; then
    output="$(printf '%s' "$output" | head -c "$MAX_MESSAGE_BYTES") [output truncated]"
  fi
}

damaged=
rc=0
output=
# find + sort, not a glob: the order is bytewise whatever the locale, and a
# missing directory yields nothing instead of an error.
while IFS= read -r check; do
  [ -f "$check" ] || continue
  name=${check##*/}
  label=$(check_label "$name")
  run_check "$check"
  case "$rc" in
    0) ;;
    "$DAMAGE_EXIT")
      damaged="${damaged:+$damaged, }$name"
      [ -n "$output" ] || output="The check reported damage but printed no message."
      if [ "$mode" = fail ]; then
        annotate error "Job-started check failed: $label" "$output"
      else
        annotate warning "Job-started check found damage: $label" "$output (GARM_JOB_HOOKS=warn, so the job continues.)"
      fi
      ;;
    124 | 137)
      annotate warning "Job-started check timed out: $label" "$name timed out after ${CHECK_TIMEOUT_SECONDS}s and was skipped."
      ;;
    *)
      message="$name exited with status $rc and was ignored."
      [ -z "$output" ] || message="$message Output: $output"
      annotate warning "Job-started check malfunctioned: $label" "$message"
      ;;
  esac
done < <(find "$dropin_dir" -maxdepth 1 -name '*.sh' 2>/dev/null | LC_ALL=C sort)

if [ -n "$damaged" ] && [ "$mode" = fail ]; then
  annotate error "Job stopped by a job-started check" "$damaged reported damage that would make this job pass without doing its work, so the job is stopped before its first step. The runner operator can set job_hooks to warn to run jobs anyway."
  exit 1
fi
exit 0
