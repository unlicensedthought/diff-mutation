#!/usr/bin/env bash
# GUTTED oracle (swapped in for admit.oracle.sh by `changeling selftest`): it
# runs the script and asserts nothing about the decisions. Mutants must
# survive it; if they do not, the instrument is broken, not the subject.
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
s="$here/scripts/admit.sh"
bash "$s" admit 3 5 >/dev/null 2>&1
bash "$s" label ok >/dev/null 2>&1
bash "$s" first_ok ok >/dev/null 2>&1
exit 0
