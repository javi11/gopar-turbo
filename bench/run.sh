#!/usr/bin/env bash
# Benchmark gopar-turbo against par2cmdline-turbo on a real PAR2 set.
#
# Each measured run gets a fresh working copy of the pristine set (APFS
# clones, so staging cost stays off the clock), deterministic damage from
# bench/damage.py, a cache-warming pass, then one timed invocation.
# Repaired output is MD5-checked against the pristine baseline.
set -uo pipefail

BENCH_ROOT=${BENCH_ROOT:?set BENCH_ROOT to the staging directory}
PAR2_TURBO=${PAR2_TURBO:?set PAR2_TURBO to the par2cmdline-turbo binary}
PAR2_STOCK=${PAR2_STOCK:-/opt/homebrew/bin/par2}
GOPAR_CGO=${GOPAR_CGO:?set GOPAR_CGO to the cgo par2bench binary}
GOPAR_PURE=${GOPAR_PURE:?set GOPAR_PURE to the pure-Go par2bench binary}
SLICE_SIZE=${SLICE_SIZE:-2380956}

WORK="$BENCH_ROOT/work"
RESULTS="$BENCH_ROOT/results"
CSV="$RESULTS/results.csv"

mkdir -p "$RESULTS"
if [[ ! -f "$CSV" ]]; then
  echo "dataset,scenario,tool,rep,seconds,peak_rss_bytes,exit_code,md5_ok,notes" > "$CSV"
fi

log() { printf '\n\033[1m== %s\033[0m\n' "$*" >&2; }

# stage <pristine-dir> <run-dir>: fresh copy-on-write clone of the set.
stage() {
  local pristine=$1 run=$2
  rm -rf "$run"
  mkdir -p "$run"
  for f in "$pristine"/*; do cp -c "$f" "$run/"; done
}

# warm <dir>: pull the set through the page cache so every tool starts from
# the same (warm) cache state rather than racing each other for it.
warm() { cat "$1"/* > /dev/null 2>&1; }

# check_md5 <run-dir> <baseline.md5> -> "ok" | "MISMATCH" | "n/a"
check_md5() {
  local run=$1 baseline=$2
  [[ -f "$baseline" ]] || { echo "n/a"; return; }
  local tmp; tmp=$(mktemp)
  ( cd "$run" && md5 -r *.rar 2>/dev/null | sort ) > "$tmp"
  if diff -q <(sort "$baseline") "$tmp" > /dev/null 2>&1; then echo "ok"; else echo "MISMATCH"; fi
  rm -f "$tmp"
}

# record <dataset> <scenario> <tool> <rep> <secs> <rss> <exit> <md5> <notes>
record() {
  printf '%s,%s,%s,%s,%.3f,%s,%s,%s,%s\n' "$@" >> "$CSV"
  printf '  %-14s %-22s rep%s  %8.3fs  rss=%6.2f GiB  exit=%s  md5=%s\n' \
    "$3" "$2" "$4" "$5" "$(echo "scale=4; $6/1073741824" | bc)" "$7" "$8" >&2
}

# run_external <binary> <op> <run-dir> <par2-index> -> prints "secs rss exit"
run_external() {
  local bin=$1 op=$2 run=$3 index=$4
  local tf; tf=$(mktemp)
  local start end
  start=$(python3 -c 'import time;print(time.monotonic())')
  ( cd "$run" && /usr/bin/time -l "$bin" "$op" -q "$index" ) > /dev/null 2> "$tf"
  local code=$?
  end=$(python3 -c 'import time;print(time.monotonic())')
  local rss
  rss=$(grep -E 'maximum resident set size' "$tf" | awk '{print $1}' | head -1)
  rm -f "$tf"
  echo "$(python3 -c "print($end-$start)") ${rss:-0} $code"
}

# run_gopar <binary> <op> <run-dir> <par2-index> -> prints "secs rss exit"
run_gopar() {
  local bin=$1 op=$2 run=$3 index=$4
  local jf; jf=$(mktemp)
  ( cd "$run" && "$bin" -op "$op" -par "$index" ) > "$jf" 2>/dev/null
  local code=$?
  python3 - "$jf" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print(d.get("seconds", 0), d.get("peak_rss_bytes", 0))
except Exception:
    print(0, 0)
PY
  rm -f "$jf"
  echo "$code"
}
