#!/usr/bin/env python3
"""Isolate the cost of gopar-turbo's misaligned-data search.

Holds the set and every other variable fixed and varies only the number of
corrupted slices. fillShardInfos (par2/decoder.go) advances one byte at a
time after a slice miss, so if that search dominates, verify time should rise
linearly with the corrupted-slice count at a slope near one slice length of
byte-wise scanning per corrupted slice.

Run this with nothing else competing for CPU.
"""
import argparse
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DAMAGE = os.path.join(HERE, "damage.py")


def clone(src, dst):
    if os.path.exists(dst):
        shutil.rmtree(dst)
    os.makedirs(dst)
    for n in sorted(os.listdir(src)):
        s = os.path.join(src, n)
        if os.path.isfile(s):
            subprocess.run(["cp", "-c", s, os.path.join(dst, n)], check=True)


def warm(directory):
    for n in sorted(os.listdir(directory)):
        p = os.path.join(directory, n)
        if os.path.isfile(p):
            with open(p, "rb") as fh:
                while fh.read(8 << 20):
                    pass


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--pristine", required=True)
    ap.add_argument("--index", required=True)
    ap.add_argument("--bin", required=True, help="par2bench binary")
    ap.add_argument("--work", required=True)
    ap.add_argument("--slice-size", type=int, default=2380956)
    ap.add_argument("--counts", default="0,5,10,20,40")
    args = ap.parse_args()

    run_dir = os.path.join(args.work, "scanprobe")
    print(f"{'corrupt slices':>15} | {'verify (s)':>10} | {'delta/slice (s)':>15}")
    print("-" * 48)

    base = None
    for count in [int(c) for c in args.counts.split(",")]:
        clone(args.pristine, run_dir)
        if count:
            subprocess.run(
                [sys.executable, DAMAGE, run_dir, "--mode", "corrupt",
                 "--count", str(count), "--slice-size", str(args.slice_size)],
                check=True, stdout=subprocess.DEVNULL)
        warm(run_dir)

        out = subprocess.run([args.bin, "-op", "verify", "-par", args.index],
                             cwd=run_dir, capture_output=True, text=True)
        secs = json.loads(out.stdout)["seconds"]
        if base is None:
            base = secs
        per = "—" if not count else f"{(secs - base) / count:.3f}"
        print(f"{count:>15} | {secs:>10.2f} | {per:>15}")

    shutil.rmtree(run_dir, ignore_errors=True)


if __name__ == "__main__":
    main()
