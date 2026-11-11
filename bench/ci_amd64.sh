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
RSEC_RE=${RSEC_RE:-'Reconstruct_|GenerateParity_|FoldInputs|ApplyMatrix'}
GF16_RE=${GF16_RE:-'MulAdd$|MulAddMulti|FoldPattern'}
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

build() {
  # Baseline: old cgo backend in a second checkout of the pre-rewrite commit.
  git -C "$ROOT" worktree add --detach "$BASE" "$BASE_REF"
  # The baseline has no kernel-level benchmarks: borrow the public-API file
  # from this tree; drop it again if it does not compile against the old API.
  GF16_COMPARE=1
  cp "$ROOT/gf16/bench_test.go" "$BASE/gf16/bench_test.go"
  if ! (cd "$BASE" && CGO_ENABLED=1 go test -c -o /dev/null ./gf16 >"$OUT/baseline-gf16-vet.txt" 2>&1); then
    echo "gf16 bench_test.go does not compile against baseline; skipping gf16 kernel comparison" >&2
    rm -f "$BASE/gf16/bench_test.go"; GF16_COMPARE=0
  fi
  echo "$GF16_COMPARE" >"$OUT/gf16_compare"

  (cd "$BASE" && CGO_ENABLED=1 go build -o "$OUT/bin/base-par2bench" ./cmd/par2bench \
    && CGO_ENABLED=1 go test -c -o "$OUT/bin/base-rsec16.test" ./rsec16)
  [ "$GF16_COMPARE" = 1 ] && (cd "$BASE" && CGO_ENABLED=1 go test -c -o "$OUT/bin/base-gf16.test" ./gf16)

  # New code: default tags, and the GFNI kernels behind gf16unverified.
  cd "$ROOT"
  for v in new:"" unv:"-tags gf16unverified"; do
    n=${v%%:*}; tags=${v#*:}
    # shellcheck disable=SC2086
    CGO_ENABLED=0 go build $tags -o "$OUT/bin/$n-par2bench" ./cmd/par2bench
    CGO_ENABLED=0 go test -c $tags -o "$OUT/bin/$n-rsec16.test" ./rsec16
    CGO_ENABLED=0 go test -c $tags -o "$OUT/bin/$n-gf16.test" ./gf16
  done
  go install golang.org/x/perf/cmd/benchstat@latest

  md "## Kernels auto-selected on this CPU"
  md "Default build:"; "$OUT/bin/new-gf16.test" -test.run TestPrintKernels -test.v | fence
  md "\`-tags gf16unverified\`:"; "$OUT/bin/unv-gf16.test" -test.run TestPrintKernels -test.v | fence
  [ "$GF16_COMPARE" = 1 ] || md "_gf16 kernel benchmarks skipped: public-API bench file did not compile against the baseline._"
}

# variants: name|binary|env  (first is the baseline). The AVX2 arm is the
# default build with the kernel forced so it is measured on its own even on a
# GFNI-capable CPU; "default" lets the library pick.
variants() { # $1 = package
  echo "base|$OUT/bin/base-$1.test|"
  echo "avx2|$OUT/bin/new-$1.test|GOPAR_GF16_KERNEL=$AVX2_KERNEL"
  echo "default|$OUT/bin/new-$1.test|"
  echo "unverified|$OUT/bin/unv-$1.test|"
}

gobench_pkg() { # pkg regex benchtime
  local pkg=$1 re=$2 bt=$3 dir="$OUT/gobench" i
  mapfile -t V < <(variants "$pkg")
  : >"$dir/$pkg.order"
  for ((i = 0; i < SAMPLES; i++)); do
    # Rotate order each sample so no variant is always first/last.
    for ((j = 0; j < ${#V[@]}; j++)); do
      IFS='|' read -r name bin envs <<<"${V[$(((i + j) % ${#V[@]}))]}"
      echo "sample $i $name" >>"$dir/$pkg.order"
      # envs holds one KEY=VALUE (value may contain spaces), so no word splitting.
      env ${envs:+"$envs"} "$bin" -test.run '^$' -test.bench "$re" -test.benchtime "$bt" \
        >>"$dir/$pkg-$name.txt" || { echo "FAILED: $pkg $name" >&2; exit 1; }
    done
  done
  local files=()
  for v in "${V[@]}"; do n=${v%%|*}; files+=("$n=$dir/$pkg-$n.txt"); done
  "$(go env GOPATH)/bin/benchstat" "${files[@]}" | tee "$dir/$pkg-benchstat.txt" >/dev/null
  md "### $pkg benchmarks ($SAMPLES samples each, benchtime $bt, interleaved)" \
     "Columns after \`base\` show the delta vs base (C/cgo) and benchstat's p-value (\`~\` = not significant at p<0.05)."
  fence <"$dir/$pkg-benchstat.txt"
}

gobench() {
  md "## Package benchmarks"
  gobench_pkg rsec16 "$RSEC_RE" "$RSEC_BENCHTIME"
  if [ "$(cat "$OUT/gf16_compare")" = 1 ]; then
    gobench_pkg gf16 "$GF16_RE" "$GF16_BENCHTIME"
  fi
}

e2e() {
  local data="$OUT/dataset" files=${FILES:-8} mib=${FILE_MIB:-48}
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
  (cd "$data" && par2 create -q -s2380956 -r15 -n1 recovery.par2 data*.bin)
  (cd "$data" && par2 verify -q recovery.par2)   # pristine must verify clean
  ls -l "$data" >"$OUT/e2e/dataset-listing.txt"

  set +e
  python3 "$ROOT/bench/interleave.py" --reps "$REPS" --pristine "$data" \
    --work "$OUT/e2e/work" --results "$OUT/e2e" \
    --slice-size 2380956 --missing-files 1 --corrupt-slices 15 \
    "base=$OUT/bin/base-par2bench" \
    "avx2=$OUT/bin/new-par2bench@GOPAR_GF16_KERNEL=$AVX2_KERNEL" \
    "default=$OUT/bin/new-par2bench" \
    "unverified=$OUT/bin/unv-par2bench" 2>&1 | tee "$OUT/e2e/log.txt"
  local rc=${PIPESTATUS[0]}
  set -e
  md "## End-to-end ($files x ${mib} MiB, slice 2380956, 15% redundancy, $REPS reps, interleaved)" \
     "\`base\` = old C/cgo backend; \`avx2\` = new code with \`GOPAR_GF16_KERNEL=$AVX2_KERNEL\`; \`default\` = new code, auto kernel; \`unverified\` = \`-tags gf16unverified\`, auto kernel. Overlap = min..max ranges of baseline and arm intersect (a crude noise indicator)."
  [ -f "$OUT/e2e/summary.md" ] && cat "$OUT/e2e/summary.md" >>"$SUMMARY"
  if [ "$rc" != 0 ]; then md "**FAILED: repair mismatch or crashed run, see log.txt**"; exit "$rc"; fi
}

"${1:?usage: ci_amd64.sh cpuinfo|build|gobench|e2e}"
