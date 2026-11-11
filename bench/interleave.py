#!/usr/bin/env python3
"""Interleaved A/B driver for par2bench binaries (reuses run.py helpers).

Every rep runs each binary once, rotating the order so none is always first.
Each run gets a fresh copy of the pristine set, identical seeded damage, a
cache-warming pass, then one timed par2bench call. Repairs are MD5-checked
against the pristine set; the script exits 1 on any mismatch or failed
repair/verify (slowness never fails it -- this is a measuring tool).

Usage: interleave.py --pristine D --work D --results D label=path[@K=V,K=V] ...
The first binary is the baseline that deltas are computed against.
"""
import argparse
import csv
import json
import os
import shutil
import statistics
import sys

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run as R  # noqa: E402


def parse_bin(spec):
    """label=path[@K=V,K=V] -> (label, argv prefix)."""
    label, rest = spec.split("=", 1)
    path, _, envs = rest.partition("@")
    prefix = (["env"] + envs.split(",")) if envs else []
    return label, prefix + [path]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--reps", type=int, default=7)
    ap.add_argument("--pristine", required=True)
    ap.add_argument("--work", required=True)
    ap.add_argument("--results", required=True)
    ap.add_argument("--index", default="recovery.par2")
    ap.add_argument("--slice-size", type=int, default=2380956)
    ap.add_argument("--seed", type=int, default=1337)
    ap.add_argument("--missing-files", type=int, default=1)
    ap.add_argument("--corrupt-slices", type=int, default=15)
    ap.add_argument("--scenarios", default=",".join(R.SCENARIOS))
    ap.add_argument("bins", nargs="+")
    a = ap.parse_args()

    bins = [parse_bin(b) for b in a.bins]
    os.makedirs(a.results, exist_ok=True)
    os.makedirs(a.work, exist_ok=True)
    counts = {"missing_files": a.missing_files, "corrupt_slices": a.corrupt_slices}
    pristine_sums = R.data_sums(a.pristine)
    rows, failures = [], []

    with open(os.path.join(a.results, "results.csv"), "w", newline="") as fh:
        w = csv.writer(fh)
        w.writerow(["scenario", "label", "rep", "seconds", "peak_rss_bytes", "exit_code", "md5", "note"])
        for sc in a.scenarios.split(","):
            op, mode, count_key = R.SCENARIOS[sc]
            print(f"\n=== {sc} ===", file=sys.stderr, flush=True)
            for rep in range(1, a.reps + 1):
                k = (rep - 1) % len(bins)
                for label, prefix in bins[k:] + bins[:k]:
                    d = os.path.join(a.work, "run")
                    R.clone_tree(a.pristine, d)
                    if mode:
                        R.apply_damage(d, mode, counts[count_key], a.slice_size, a.seed)
                    R.warm_cache(d)
                    secs, rss, code, out = R.timed_run(prefix + ["-op", op, "-par", a.index], d)
                    note = ""
                    try:
                        note = (json.loads(out).get("error") or "")[:90]
                    except ValueError:
                        note = "no JSON result"
                    md5 = "n/a"
                    if op == "repair":
                        md5 = "ok" if R.data_sums(d) == pristine_sums else "MISMATCH"
                    if code != 0 or md5 == "MISMATCH":
                        failures.append(f"{sc}/{label}/rep{rep}: exit={code} md5={md5} {note}")
                    w.writerow([sc, label, rep, f"{secs:.3f}", rss, code, md5, note])
                    fh.flush()
                    rows.append((sc, label, secs, rss, md5, code))
                    print(f"  {label:<18} rep{rep} {secs:7.2f}s rss={rss/1e6:6.0f}MB exit={code} md5={md5} {note}",
                          file=sys.stderr, flush=True)
                    shutil.rmtree(d, ignore_errors=True)

    base = bins[0][0]
    lines = [
        "| Scenario | Binary | Median | Min-Max | vs baseline | Ranges overlap? | Peak RSS (median) | MD5 ok |",
        "|---|---|---:|---:|---:|:---:|---:|:---:|",
    ]
    for sc in a.scenarios.split(","):
        b = [r[2] for r in rows if r[0] == sc and r[1] == base]
        for label, _ in bins:
            rs = [r for r in rows if r[0] == sc and r[1] == label]
            t = [r[2] for r in rs]
            med = statistics.median(t)
            delta = "-" if label == base else f"{(med / statistics.median(b) - 1) * 100:+.1f}%"
            # Crude noise flag: do the min..max ranges of baseline and this arm intersect?
            overlap = "-" if label == base else ("yes" if min(t) <= max(b) and min(b) <= max(t) else "no")
            ok = "n/a" if rs[0][4] == "n/a" else f"{sum(r[4] == 'ok' for r in rs)}/{len(rs)}"
            lines.append(f"| {sc} | {label} | {med:.2f}s | {min(t):.2f}-{max(t):.2f}s | {delta} | {overlap} | "
                         f"{statistics.median(r[3] for r in rs)/1e6:.0f} MB | {ok} |")
    text = "\n".join(lines) + "\n"
    open(os.path.join(a.results, "summary.md"), "w").write(text)
    print("\n" + text)
    if failures:
        print("FAILURES:\n  " + "\n  ".join(failures), file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
