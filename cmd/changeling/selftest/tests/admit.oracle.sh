#!/usr/bin/env bash
# Strong oracle for scripts/admit.sh: every decision is asserted on both sides
# of its boundary. Bugs it names: an off-by-one admit at 0 or LIMIT+1, a
# refused in-range N, "" labelled as fail, "ok" labelled as fail, the wrong
# first index, and success when no word is ok.
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
s="$here/scripts/admit.sh"
fails=0
expect() { # want-rc want-out args...
  local want_rc="$1" want_out="$2"; shift 2
  local out rc=0
  out=$(bash "$s" "$@" 2>/dev/null) || rc=$?
  if [ "$rc" != "$want_rc" ] || [ "$out" != "$want_out" ]; then
    echo "FAIL $*: rc=$rc out='$out' (want rc=$want_rc out='$want_out')"
    fails=$((fails + 1))
  fi
}
expect 1 "" admit 0 5
expect 0 "" admit 1 5
expect 0 "" admit 5 5
expect 1 "" admit 6 5
expect 0 empty label ""
expect 0 pass label ok
expect 0 fail label nope
expect 0 0 first_ok ok x
expect 0 2 first_ok a b ok
expect 3 "" first_ok a b
[ "$fails" -eq 0 ]
