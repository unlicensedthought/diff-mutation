#!/usr/bin/env bash
# changeling selftest subject: a small decision script with one of every
# operator class the bash engine mutates. Its strong oracle kills every mutant;
# the gutted oracle leaves survivors. Do not edit without re-running
# `changeling selftest`: the two-way check depends on both directions.
set -u

# admit N LIMIT -> exit 0 when 1 <= N <= LIMIT, else 1.
admit() {
  local n="$1" limit="$2"
  if [ "$n" -lt 1 ]; then
    return 1
  fi
  if [[ "$n" -gt "$limit" ]]; then
    return 1
  fi
  return 0
}

# label S -> "empty" for "", "pass" for ok, "fail" otherwise.
label() {
  local s="$1"
  if [ -z "$s" ]; then
    echo empty
    return 0
  fi
  [ "$s" = ok ] && echo pass || echo fail
}

# first_ok WORD... -> prints the index of the first "ok", or exits 3.
first_ok() {
  local i=0 w
  for w in "$@"; do
    if [ "$w" = ok ]; then
      echo "$i"
      return 0
    fi
    i=$((i + 1))
  done
  exit 3
}

case "${1:-}" in
  admit) shift; admit "$@" ;;
  label) shift; label "$@" ;;
  first_ok) shift; first_ok "$@" ;;
  *) echo "usage: admit.sh admit|label|first_ok ..." >&2; exit 2 ;;
esac
