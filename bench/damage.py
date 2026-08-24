#!/usr/bin/env python3
"""Apply deterministic, reproducible damage to a PAR2-protected file set.

Damage is a pure function of (mode, seed, slice size, sorted file list), so
every tool under benchmark is handed a byte-identical starting state.

Modes:
  missing  -- delete whole data files (simulates dropped NZB articles/files)
  corrupt  -- overwrite bytes inside N distinct slices, scattered across files
"""
import argparse
import math
import os
import random
import sys

CORRUPT_BYTES = 64  # bytes clobbered per damaged slice


def data_files(directory):
    """Data files of the set: everything that is not a .par2 member."""
    return sorted(
        f for f in os.listdir(directory)
        if not f.endswith(".par2") and os.path.isfile(os.path.join(directory, f))
    )


def slice_map(directory, files, slice_size):
    """Global slice index -> (filename, byte offset)."""
    mapping = []
    for name in files:
        size = os.path.getsize(os.path.join(directory, name))
        for s in range(math.ceil(size / slice_size)):
            mapping.append((name, s * slice_size))
    return mapping


def do_missing(directory, files, count, seed):
    rng = random.Random(seed)
    victims = sorted(rng.sample(files, count))
    for name in victims:
        os.remove(os.path.join(directory, name))
    return victims


def do_corrupt(directory, files, count, slice_size, seed):
    rng = random.Random(seed)
    mapping = slice_map(directory, files, slice_size)
    if count > len(mapping):
        sys.exit(f"damage: asked for {count} slices, set only has {len(mapping)}")
    chosen = sorted(rng.sample(range(len(mapping)), count))

    by_file = {}
    for idx in chosen:
        name, offset = mapping[idx]
        by_file.setdefault(name, []).append(offset)

    for name, offsets in sorted(by_file.items()):
        path = os.path.join(directory, name)
        size = os.path.getsize(path)
        with open(path, "r+b") as fh:
            for offset in offsets:
                # Stay inside the file for a trailing partial slice.
                n = min(CORRUPT_BYTES, size - offset)
                fh.seek(offset)
                fh.write(bytes(rng.randrange(256) for _ in range(n)))
    return chosen


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("directory")
    ap.add_argument("--mode", required=True, choices=["missing", "corrupt"])
    ap.add_argument("--count", type=int, required=True)
    ap.add_argument("--slice-size", type=int, default=2380956)
    ap.add_argument("--seed", type=int, default=1337)
    args = ap.parse_args()

    files = data_files(args.directory)
    if not files:
        sys.exit(f"damage: no data files in {args.directory}")

    if args.mode == "missing":
        victims = do_missing(args.directory, files, args.count, args.seed)
        print(f"deleted {len(victims)} files: {', '.join(victims)}")
    else:
        chosen = do_corrupt(args.directory, files, args.count, args.slice_size, args.seed)
        print(f"corrupted {len(chosen)} slices ({CORRUPT_BYTES} B each)")


if __name__ == "__main__":
    main()
