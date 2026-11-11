#!/usr/bin/env bash
# CI driver for .github/workflows/bench-amd64.yml: new Go-asm backend vs the
# old cgo ParPar backend (commit 9c0f5ca) on the same runner.
#   ci_amd64.sh cpuinfo | build | gobench | e2e
# All raw output lands in $OUT (uploaded as an artifact); markdown goes to
# $GITHUB_STEP_SUMMARY. Slower-than-baseline never fails; build errors,
# crashes and wrong repair bytes do.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${BENCH_OUT:-$ROOT/bench-out}
BASE_REF=${BASE_REF:-9c0f5ca}
BASE=${BASE_DIR:-$ROOT/../baseline}
SAMPLES=${SAMPLES:-10}
RSEC_BENCHTIME=${RSEC_BENCHTIME:-1s}
GF16_BENCHTIME=${GF16_BENCHTIME:-300ms}
# ApplyMatrix has ~60 sub-benchmarks (~2 min per pass); keep one representative
# shape (3 in x 16 out, 1 MiB) at single and 4-way parallel.
RSEC_RE=${RSEC_RE:-'Reconstruct_|GenerateParity_|FoldInputs_Realistic|FoldInputsParallel|ApplyMatrix/ic=3,oc=16,db=1M/(Single|DataParallel-4)'}
GF16_RE=${GF16_RE:-'MulAdd$|MulAddMulti|FoldPattern|Prepare|Finish'}
REPS=${REPS:-7}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/stdout}
AVX2_KERNEL='Shuffle (AVX2)'
mkdir -p "$OUT/bin" "$OUT/gobench" "$OUT/e2e"

md() { printf '%s\n' "$@" >>"$SUMMARY"; }
fence() { md '```' ; cat >>"$SUMMARY"; md '```'; }

cpuinfo() {
  { grep -m1 'model name' /proc/cpuinfo; echo "vCPUs: $(nproc)"
    echo "flags: $(grep -m1 '^flags' /proc/cpuinfo | tr ' ' '\n' | grep -E -x 'ssse3|avx2|gfni|avx512f|avx512bw|avx512vl|vaes|vpclmulqdq|pclmulqdq' | sort | tr '\n' ' ')"
    uname -sr; go version; } | tee "$OUT/cpuinfo.txt"
  md "## Runner" ; fence <"$OUT/cpuinfo.txt"
}

# The baseline C backend always asks ParPar for default_method(). Patch the
# baseline checkout's bridge.cpp so GF16_FORCE_METHOD=<Galois16MethodsText name>
# (e.g. "Shuffle (AVX2)") selects that method instead, then rebuild
# libgf16_linux_amd64.a from the vendored sources (the committed .a predates the
# patch). Unset env = unchanged behaviour (GF16_AUTO's default_method).
patch_and_rebuild_baseline() {
  python3 - "$BASE/gf16/csrc/bridge.cpp" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
pat = re.compile(r"Galois16Methods method =\s*Galois16Mul::default_method\([^;]*\);")
new = """Galois16Methods method =
        Galois16Mul::default_method(slice_size, 32768, 65535, true);
    if (const char* force = getenv("GF16_FORCE_METHOD")) {
        // Names are ParPar's own Galois16MethodsText entries, e.g. "Shuffle (AVX2)".
        bool found = false;
        for (int i = 0; i <= (int)GF16_CLMUL_RVV; i++) {
            if (strcmp(Galois16MethodsText[i], force) == 0) {
                method = (Galois16Methods)i; found = true; break;
            }
        }
        if (!found) { fprintf(stderr, "GF16_FORCE_METHOD: unknown method '%s'\\n", force); abort(); }
    }"""
assert pat.search(s), "bridge.cpp default_method call not found"
s = pat.sub(lambda m: new, s, count=1)
s = s.replace("#include <cstring>", "#include <cstring>\n#include <cstdlib>\n#include <cstdio>", 1)
open(p, "w").write(s)
PY
  (cd "$BASE/gf16" && make clean && make -j"$(nproc)" native) >"$OUT/baseline-make.txt" 2>&1 \
    || { tail -30 "$OUT/baseline-make.txt" >&2; exit 1; }
  cat >"$BASE/gf16/zz_method_test.go" <<'GO'
package gf16

import "testing"

func TestBaseMethod(t *testing.T) {
	ctx, err := NewContext(2380956)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("BASE_METHOD=%s", ctx.MethodName())
}
GO
  (cd "$BASE" && CGO_ENABLED=1 go test -c -o "$OUT/bin/base-method.test" ./gf16)
}

# base_method [ENV_VALUE]: the method name the patched C backend reports.
base_method() {
  GF16_FORCE_METHOD="${1:-}" "$OUT/bin/base-method.test" -test.run TestBaseMethod -test.v 2>&1 \
    | sed -n 's/.*BASE_METHOD=//p' | head -1
}

build() {
  # Baseline: old cgo backend in a second checkout of the pre-rewrite commit.
  git -C "$ROOT" worktree add --detach "$BASE" "$BASE_REF"
  # The baseline has no kernel-level benchmarks: borrow the public-API file
  # from this tree; drop it again if it does not compile against the old API.
  GF16_COMPARE=1
  patch_and_rebuild_baseline
  cp "$ROOT/gf16/bench_test.go" "$BASE/gf16/bench_test.go"
  if ! (cd "$BASE" && CGO_ENABLED=1 go test -c -o /dev/null ./gf16 >"$OUT/baseline-gf16-vet.txt" 2>&1); then
    echo "gf16 bench_test.go does not compile against baseline; skipping gf16 kernel comparison" >&2
    rm -f "$BASE/gf16/bench_test.go"; GF16_COMPARE=0
  fi
  echo "$GF16_COMPARE" >"$OUT/gf16_compare"

  (cd "$BASE" && CGO_ENABLED=1 go build -o "$OUT/bin/base-par2bench" ./cmd/par2bench \
    && CGO_ENABLED=1 go test -c -o "$OUT/bin/base-rsec16.test" ./rsec16)
  [ "$GF16_COMPARE" = 1 ] && (cd "$BASE" && CGO_ENABLED=1 go test -c -o "$OUT/bin/base-gf16.test" ./gf16)

  # New code: default build (GFNI kernels included, chosen at run time).
  cd "$ROOT"
  CGO_ENABLED=0 go build -o "$OUT/bin/new-par2bench" ./cmd/par2bench
  CGO_ENABLED=0 go test -c -o "$OUT/bin/new-rsec16.test" ./rsec16
  CGO_ENABLED=0 go test -c -o "$OUT/bin/new-gf16.test" ./gf16
  go install golang.org/x/perf/cmd/benchstat@latest

  md "## Kernels auto-selected on this CPU"
  md "Default build:"; "$OUT/bin/new-gf16.test" -test.run TestPrintKernels -test.v | fence
  # What the C backend picks on this CPU, and proof that forcing works.
  BASE_AUTO=$(GF16_FORCE_METHOD='' env -u GF16_FORCE_METHOD "$OUT/bin/base-method.test" -test.run TestBaseMethod -test.v 2>&1 | sed -n 's/.*BASE_METHOD=//p' | head -1)
  echo "$BASE_AUTO" >"$OUT/base_auto_name"
  local new_name; new_name=$("$OUT/bin/new-gf16.test" -test.run TestPrintKernels -test.v 2>&1 | sed -n 's/.*default kernel: \(.*\) (accelerated.*/\1/p' | head -1)
  : >"$OUT/base_forced.txt"
  local want got
  for want in "Shuffle (AVX2)" "Affine (GFNI+AVX2)" "Affine (GFNI+AVX512)"; do
    got=$(base_method "$want")
    echo "forced '$want' -> reports '$got'" | tee -a "$OUT/base_forced.txt"
    [ "$got" = "$want" ] && echo "$want" >>"$OUT/base_forced_ok"
  done
  md "## C (cgo) backend method selection"
  md "ParPar \`default_method()\` (AUTO) on this CPU picks: **$BASE_AUTO**. New build default: **$new_name**."
  md "Forcing check (patched baseline, \`GF16_FORCE_METHOD\`; a method the CPU/build cannot run silently falls back, in which case its arm is dropped):"
  fence <"$OUT/base_forced.txt"
  [ "$GF16_COMPARE" = 1 ] || md "_gf16 kernel benchmarks skipped: public-API bench file did not compile against the baseline._"
}

# variants: name|binary|env  (first is the baseline for the full table). The AVX2
# arm is the default build with the kernel forced so it is measured on its own
# even on a GFNI-capable CPU. Base arms: auto = C default_method(); shuffle =
# C forced to Shuffle (AVX2). On a GFNI+AVX-512 CPU auto is already an Affine
# method, so base-auto doubles as the C affine arm (the like-for-like opponent
# of the new default on GFNI CPUs); on CPUs where auto is Shuffle (AVX2) it doubles
# as the shuffle arm.
base_auto() { cat "$OUT/base_auto_name"; }
forced_ok() { grep -qxF "$1" "$OUT/base_forced_ok" 2>/dev/null; }
shuffle_label() { [ "$(base_auto)" = "$AVX2_KERNEL" ] && echo base-auto || echo base-shuffle-avx2; }
variants() { # $1 = package
  echo "base-auto|$OUT/bin/base-$1.test|"
  if [ "$(shuffle_label)" = base-shuffle-avx2 ] && forced_ok "$AVX2_KERNEL"; then
    echo "base-shuffle-avx2|$OUT/bin/base-$1.test|GF16_FORCE_METHOD=$AVX2_KERNEL"
  fi
  echo "avx2|$OUT/bin/new-$1.test|GOPAR_GF16_KERNEL=$AVX2_KERNEL"
  echo "default|$OUT/bin/new-$1.test|"
  return 0
}
have_gfni() { grep -q -m1 -w gfni /proc/cpuinfo; }

gobench_pkg() { # pkg regex benchtime
  local pkg=$1 re=$2 bt=$3 dir="$OUT/gobench" i
  mapfile -t V < <(variants "$pkg")
  : >"$dir/$pkg.order"
  for ((i = 0; i < SAMPLES; i++)); do
    # Rotate order each sample so no variant is always first/last.
    for ((j = 0; j < ${#V[@]}; j++)); do
      IFS='|' read -r name bin envs <<<"${V[$(((i + j) % ${#V[@]}))]}"
      echo "$(date -u +%T) $pkg sample $i $name" | tee -a "$dir/$pkg.order"
      # envs holds one KEY=VALUE (value may contain spaces), so no word splitting.
      env ${envs:+"$envs"} "$bin" -test.run '^$' -test.bench "$re" -test.benchtime "$bt" \
        >>"$dir/$pkg-$name.txt" || { echo "FAILED: $pkg $name" >&2; exit 1; }
    done
  done
  local files=() v n
  for v in "${V[@]}"; do n=${v%%|*}; files+=("$n=$dir/$pkg-$n.txt"); done
  local bs; bs="$(go env GOPATH)/bin/benchstat"
  # Like-for-like tables first: same instruction-set tier on the same CPU.
  md "### $pkg like-for-like ($SAMPLES samples each, benchtime $bt, interleaved)" \
     "Delta is new vs C baseline (+ = new slower); \`~\` = not significant at p<0.05."
  local sl; sl=$(shuffle_label)
  if [ -f "$dir/$pkg-$sl.txt" ]; then
    md "**Shuffle (AVX2): C \`$sl\` vs new \`avx2\`**"
    "$bs" "C-shuffle-avx2=$dir/$pkg-$sl.txt" "new-shuffle-avx2=$dir/$pkg-avx2.txt" | tee "$dir/$pkg-lfl-shuffle.txt" | fence
  fi
  if have_gfni && [[ "$(base_auto)" == Affine* ]]; then
    md "**GFNI: C \`base-auto\` ($(base_auto)) vs new \`default\` (see kernel list above for its name)**"
    "$bs" "C-affine=$dir/$pkg-base-auto.txt" "new-gfni=$dir/$pkg-default.txt" | tee "$dir/$pkg-lfl-gfni.txt" | fence
  fi
  "$bs" "${files[@]}" >"$dir/$pkg-benchstat.txt"
  md "<details><summary>$pkg: all arms vs base-auto (C default)</summary>" ""
  fence <"$dir/$pkg-benchstat.txt"
  md "</details>"
}

gobench() {
  md "## Package benchmarks"
  gobench_pkg rsec16 "$RSEC_RE" "$RSEC_BENCHTIME"
  if [ "$(cat "$OUT/gf16_compare")" = 1 ]; then
    gobench_pkg gf16 "$GF16_RE" "$GF16_BENCHTIME"
  fi
}

mkdataset() {
  local data="$OUT/dataset" files=${FILES:-6} mib=${FILE_MIB:-96} redundancy=${REDUNDANCY:-30}
  [ -f "$data/recovery.par2" ] && return 0
  command -v par2 >/dev/null || sudo apt-get install -y par2 >/dev/null
  mkdir -p "$data"
  # Deterministic incompressible data, then a PAR2 set from par2cmdline.
  python3 - "$data" "$files" "$mib" <<'PY'
import random, sys
d, n, mib = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
rng = random.Random(20261024)
for i in range(n):
    with open(f"{d}/data{i:02}.bin", "wb") as f:
        for _ in range(mib):
            f.write(rng.randbytes(1 << 20))
PY
  (cd "$data" && par2 create -q -s2380956 -r"$redundancy" -n1 recovery.par2 data*.bin)
  (cd "$data" && par2 verify -q recovery.par2)   # pristine must verify clean
  ls -l "$data" >"$OUT/e2e/dataset-listing.txt"
}

# profile: where does repair time go? One repair-missing and one repair-corrupt
# per arm under perf (software clock sampling; VMs expose no HW counters) plus
# perf stat for page faults / context switches / CPU time, and a Go CPU profile
# for the new binary. Output: $OUT/profile/*, top symbols into the summary.
profile() {
  local data="$OUT/dataset" corrupt=${CORRUPT_SLICES:-50} P="$OUT/profile" arm sc
  mkdir -p "$P"
  mkdataset
  sudo apt-get install -y linux-tools-common "linux-tools-$(uname -r)" >/dev/null 2>&1 \
    || sudo apt-get install -y linux-tools-generic >/dev/null 2>&1 || true
  sudo sysctl -w kernel.perf_event_paranoid=-1 kernel.kptr_restrict=0 >/dev/null 2>&1 || true
  local PERF; PERF=$(ls /usr/lib/linux-tools/*/perf 2>/dev/null | tail -1); PERF=${PERF:-perf}
  "$PERF" --version | tee "$P/perf-version.txt" || { md "_perf unavailable_"; return 0; }
  local arms=("base-auto|$OUT/bin/base-par2bench|")
  if [ "$(shuffle_label)" = base-shuffle-avx2 ] && forced_ok "$AVX2_KERNEL"; then
    arms+=("base-shuffle-avx2|$OUT/bin/base-par2bench|GF16_FORCE_METHOD=$AVX2_KERNEL")
  fi
  arms+=("avx2|$OUT/bin/new-par2bench|GOPAR_GF16_KERNEL=$AVX2_KERNEL")
  arms+=("default|$OUT/bin/new-par2bench|")
  have_gfni && arms+=("unverified|$OUT/bin/unv-par2bench|")
  for sc in missing corrupt; do
    for a in "${arms[@]}"; do
      IFS='|' read -r arm bin envs <<<"$a"
      local w="$OUT/profile/work-$sc-$arm"
      rm -rf "$w"; mkdir -p "$w"; cp --reflink=auto "$data"/* "$w"/
      python3 "$ROOT/bench/damage.py" "$w" --mode "$sc" --count "$([ $sc = missing ] && echo 1 || echo "$corrupt")" \
        --slice-size 2380956 --seed 1337 >/dev/null
      cat "$w"/* >/dev/null
      echo "== $sc $arm" | tee -a "$P/stat.txt"
      (cd "$w" && env ${envs:+"$envs"} "$PERF" stat -e task-clock,page-faults,context-switches,cpu-migrations \
         -o "$P/stat-$sc-$arm.txt" -- "$bin" -op repair -par recovery.par2 \
         -cpuprofile "$P/cpu-$sc-$arm.pprof" >/dev/null)
      cat "$P/stat-$sc-$arm.txt" >>"$P/stat.txt"
      # Second run of the same damage under perf record.
      rm -rf "$w"; mkdir -p "$w"; cp --reflink=auto "$data"/* "$w"/
      python3 "$ROOT/bench/damage.py" "$w" --mode "$sc" --count "$([ $sc = missing ] && echo 1 || echo "$corrupt")" \
        --slice-size 2380956 --seed 1337 >/dev/null
      cat "$w"/* >/dev/null
      (cd "$w" && env ${envs:+"$envs"} "$PERF" record -F 2000 -g -o "$P/perf-$sc-$arm.data" -- \
         "$bin" -op repair -par recovery.par2 >/dev/null 2>"$P/record-$sc-$arm.err") || true
      "$PERF" report -i "$P/perf-$sc-$arm.data" --no-children --sort dso,symbol -g none --percent-limit 0.7 \
         --stdio 2>/dev/null | grep -v '^#' | grep -v '^$' >"$P/top-$sc-$arm.txt" || true
      rm -rf "$w"
    done
  done
  # Package-level fold benchmark under perf: isolates the fold loop from file I/O.
  for a in "${arms[@]}"; do
    IFS='|' read -r arm bin envs <<<"$a"
    local tb=${bin/par2bench/rsec16.test}
    env ${envs:+"$envs"} "$PERF" stat -e task-clock,page-faults,context-switches -o "$P/fstat-$arm.txt" -- \
      "$tb" -test.run '^$' -test.bench 'FoldInputs_Realistic$' -test.benchtime 6x -test.cpuprofile "$P/fcpu-$arm.pprof" >"$P/fold-$arm.txt" 2>&1 || true
    env ${envs:+"$envs"} "$PERF" record -F 2000 -g -o "$P/fperf-$arm.data" -- \
      "$tb" -test.run '^$' -test.bench 'FoldInputs_Realistic$' -test.benchtime 6x >/dev/null 2>&1 || true
    "$PERF" report -i "$P/fperf-$arm.data" --no-children --sort dso,symbol -g none --percent-limit 0.7 --stdio 2>/dev/null \
      | grep -v '^#' | grep -v '^$' >"$P/ftop-$arm.txt" || true
    md "### fold bench: $arm"; { grep -h Benchmark "$P/fold-$arm.txt"; cat "$P/fstat-$arm.txt"; head -25 "$P/ftop-$arm.txt"; } | fence
  done
  md "## Profile (perf stat)"; fence <"$P/stat.txt"
  for sc in missing corrupt; do
    for a in "${arms[@]}"; do
      arm=${a%%|*}
      md "### top symbols: repair-$sc / $arm"; head -30 "$P/top-$sc-$arm.txt" | fence
    done
  done
}

e2e() {
  local data="$OUT/dataset" files=${FILES:-6} mib=${FILE_MIB:-96} redundancy=${REDUNDANCY:-30} corrupt=${CORRUPT_SLICES:-50}
  mkdataset
  local SHUF_ARM=()
  if [ "$(shuffle_label)" = base-shuffle-avx2 ] && forced_ok "$AVX2_KERNEL"; then
    SHUF_ARM=("base-shuffle-avx2=$OUT/bin/base-par2bench@GF16_FORCE_METHOD=$AVX2_KERNEL")
  fi
  set +e
  python3 "$ROOT/bench/interleave.py" --reps "$REPS" --pristine "$data" \
    --work "$OUT/e2e/work" --results "$OUT/e2e" \
    --slice-size 2380956 --missing-files 1 --corrupt-slices "$corrupt" \
    "base-auto=$OUT/bin/base-par2bench" \
    "${SHUF_ARM[@]}" \
    "avx2=$OUT/bin/new-par2bench@GOPAR_GF16_KERNEL=$AVX2_KERNEL" \
    "default=$OUT/bin/new-par2bench"  2>&1 | tee "$OUT/e2e/log.txt"
  local rc=${PIPESTATUS[0]}
  set -e
  md "## End-to-end ($files x ${mib} MiB, slice 2380956, ${redundancy}% redundancy, 1 file deleted / $corrupt slices corrupted, $REPS reps, interleaved)" \
     "\`base-auto\` = old C/cgo backend, its own default method ($(base_auto)); \`base-shuffle-avx2\` = C forced to Shuffle (AVX2) (absent when auto already is that); \`avx2\` = new code with \`GOPAR_GF16_KERNEL=$AVX2_KERNEL\`; \`default\` = new code, auto kernel (GFNI Affine2x where the CPU has it); Overlap = min..max ranges of baseline and arm intersect (a crude noise indicator)."
  python3 - "$OUT/e2e/results.csv" "$(shuffle_label)" "$(have_gfni && [[ "$(base_auto)" == Affine* ]] && echo yes)" >>"$SUMMARY" <<'PY'
import csv, statistics, sys
path, shuf, gfni = sys.argv[1:4]
t = {}
for r in csv.DictReader(open(path)):
    if r["exit_code"] == "0":
        t.setdefault((r["scenario"], r["label"]), []).append(float(r["seconds"]))
pairs = [(shuf, "avx2", "Shuffle (AVX2): C vs new")]
if gfni: pairs.append(("base-auto", "default", "GFNI: C Affine (auto) vs new default"))
print("\n### End-to-end like-for-like (median seconds; + = new slower; overlap = min..max ranges intersect, i.e. within noise)\n")
print("| Comparison | Scenario | C | new | new vs C | ranges overlap |\n|---|---|---:|---:|---:|:---:|")
for c, n, title in pairs:
    for sc in sorted({k[0] for k in t}):
        a, b = t.get((sc, c)), t.get((sc, n))
        if not a or not b: continue
        ov = "yes" if min(a) <= max(b) and min(b) <= max(a) else "no"
        print(f"| {title} | {sc} | {statistics.median(a):.2f}s | {statistics.median(b):.2f}s | {(statistics.median(b)/statistics.median(a)-1)*100:+.1f}% | {ov} |")
print()
PY
  [ -f "$OUT/e2e/summary.md" ] && cat "$OUT/e2e/summary.md" >>"$SUMMARY"
  if [ "$rc" != 0 ]; then md "**FAILED: repair mismatch or crashed run, see log.txt**"; exit "$rc"; fi
}

"${1:?usage: ci_amd64.sh cpuinfo|build|gobench|e2e|profile}"
