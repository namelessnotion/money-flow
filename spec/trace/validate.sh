#!/usr/bin/env bash
# Checks NDJSON traces recorded by go/internal/tlatrace against
# spec/eventstore.tla, through spec/Traceeventstore.tla.
#
#   TLA2TOOLS=/path/to/tla2tools.jar spec/trace/validate.sh <trace-or-dir>...
#
# A trace is expected to be accepted unless its file name starts with
# "reject-" (the fixtures in spec/trace/fixtures), which must be rejected by
# the trace spec itself, not by a TLC error. Exits non-zero if any trace
# came out otherwise.
#
# Each trace's state space is tiny, so TLC runs single-threaded, one JVM per
# trace, TLA_JOBS (default: every core) at a time.
set -euo pipefail

: "${TLA2TOOLS:?set TLA2TOOLS to the path of tla2tools.jar}"
JAVA="${JAVA:-java}"
SPEC_DIR="$(cd "$(dirname "$0")/.." && pwd)"

# verdict prints accepted, rejected or error for one trace's TLC output. TLC
# exits 0 even when the TraceAccepted postcondition is false, so the verdict
# is read from what it printed.
verdict() {
  local out="$1"
  if grep -q -E 'Postcondition TraceAccepted .* is false|Invariant .* is violated|property .* (is|was) violated' "$out"; then
    echo rejected
  elif grep -q '^Error' "$out"; then
    echo error
  elif grep -q 'Model checking completed. No error has been found.' "$out"; then
    echo accepted
  else
    echo error
  fi
}

# validate_one checks a single trace and prints one result line, followed by
# TLC's explanation when the result is unexpected.
validate_one() {
  local trace="$1" work abs got want
  work="$(mktemp -d)"
  abs="$(cd "$(dirname "$trace")" && pwd)/$(basename "$trace")"
  cp "$SPEC_DIR/Traceeventstore.cfg" "$work/MCtrace.cfg"
  printf -- '---- MODULE MCtrace ----\nEXTENDS Traceeventstore\nMCTraceFile == "%s"\n====\n' "$abs" > "$work/MCtrace.tla"
  (cd "$work" && "$JAVA" -XX:+UseParallelGC -DTLA-Library="$SPEC_DIR" -cp "$TLA2TOOLS" tlc2.TLC \
    -noGenerateSpecTE -nowarning -workers 1 -metadir states -config MCtrace.cfg MCtrace.tla \
    > "$work/out.txt" 2>&1) || true

  got="$(verdict "$work/out.txt")"
  want=accepted
  [[ "$(basename "$trace")" == reject-* ]] && want=rejected
  if [[ "$got" == "$want" ]]; then
    echo "ok    $got  $trace"
  else
    echo "FAIL  $got (want $want)  $trace"
    grep -E -A12 'REJECTED|^Error' "$work/out.txt" | sed 's/^/        /' | head -40
  fi
  rm -rf "$work"
}

if [[ "${1:-}" == "--one" ]]; then
  validate_one "$2"
  exit 0
fi

traces=()
for arg in "$@"; do
  if [[ -d "$arg" ]]; then
    while IFS= read -r f; do traces+=("$f"); done < <(find "$arg" -maxdepth 1 -name '*.ndjson' | sort)
  else
    traces+=("$arg")
  fi
done
if [[ ${#traces[@]} -eq 0 ]]; then
  echo "validate.sh: no traces given" >&2
  exit 2
fi

jobs="${TLA_JOBS:-$(getconf _NPROCESSORS_ONLN)}"
results="$(printf '%s\0' "${traces[@]}" | xargs -0 -n1 -P "$jobs" "$0" --one)"
echo "$results"
failures="$(grep -c '^FAIL' <<< "$results" || true)"
echo "${#traces[@]} traces, $failures unexpected"
[[ $failures -eq 0 ]]
