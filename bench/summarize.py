#!/usr/bin/env python3
"""Turn bench/run.py's results.csv into Markdown tables.

Reports the median of the reps for each (dataset, scenario, tool), plus the
ratio against a reference tool so the comparison is readable at a glance.
"""
import argparse
import csv
import statistics
from collections import defaultdict

GIB = 1024 ** 3
TOOL_ORDER = [
    "gopar-turbo (cgo/SIMD)",
    "gopar-turbo (pure Go)",
    "par2cmdline-turbo",
    "par2turbo -N (misalign)",
    "par2cmdline (stock)",
]
SCENARIO_ORDER = ["verify-intact", "verify-damaged", "repair-missing", "repair-corrupt"]


def load(path):
    rows = defaultdict(list)
    with open(path, newline="") as fh:
        for r in csv.DictReader(fh):
            rows[(r["dataset"], r["scenario"], r["tool"])].append(r)
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("csv_path")
    ap.add_argument("--reference", default="par2cmdline-turbo",
                    help="tool that ratios are expressed against")
    args = ap.parse_args()

    rows = load(args.csv_path)
    datasets = sorted({k[0] for k in rows})

    for dataset in datasets:
        print(f"\n### {dataset}\n")
        print("| Scenario | Tool | Time (s) | vs ref | Peak RSS | Repair correct |")
        print("|---|---|---:|---:|---:|:---:|")

        scenarios = [s for s in SCENARIO_ORDER if (dataset, s, TOOL_ORDER[0]) in rows
                     or any((dataset, s, t) in rows for t in TOOL_ORDER)]
        for scenario in scenarios:
            ref_key = (dataset, scenario, args.reference)
            ref_time = None
            if ref_key in rows:
                ref_time = statistics.median(float(r["seconds"]) for r in rows[ref_key])

            for tool in TOOL_ORDER:
                key = (dataset, scenario, tool)
                if key not in rows:
                    continue
                reps = rows[key]
                secs = statistics.median(float(r["seconds"]) for r in reps)
                rss = max(int(r["peak_rss_bytes"]) for r in reps)
                md5 = {r["md5_ok"] for r in reps}
                # par2cmdline exits 1 for "damage found, repair possible", so a
                # nonzero exit is the *correct* answer when verifying a damaged
                # set. Only flag exits that are unexpected for the scenario.
                ok_codes = {"0", "1"} if scenario == "verify-damaged" else {"0"}
                failed = any(r["exit_code"] not in ok_codes for r in reps)

                if "MISMATCH" in md5:
                    correct = "**WRONG**"
                elif md5 == {"n/a"}:
                    correct = "—"
                else:
                    correct = "yes"
                if failed:
                    correct += " (unexpected exit)"

                ratio = f"{secs / ref_time:.2f}x" if ref_time else "—"
                print(f"| {scenario} | {tool} | {secs:.2f} | {ratio} | "
                      f"{rss / GIB:.2f} GiB | {correct} |")
        print()


if __name__ == "__main__":
    main()
