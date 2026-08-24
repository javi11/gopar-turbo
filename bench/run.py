#!/usr/bin/env python3
"""Benchmark gopar-turbo against par2cmdline-turbo on a real PAR2 set.

Each measured run gets a fresh copy-on-write clone of the pristine set,
deterministic damage from damage.py, and a cache-warming pass, so every tool
starts from a byte-identical, equally-warm state. Peak RSS comes from
os.wait4() rusage for that specific child, and repaired output is MD5-checked
against the pristine baseline.
"""
import argparse
import csv
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
DAMAGE = os.path.join(HERE, "damage.py")
GIB = 1024 ** 3


# ---------------------------------------------------------------- staging

def clone_tree(src, dst):
    """Copy-on-write clone the set so staging cost stays off the clock."""
    if os.path.exists(dst):
        shutil.rmtree(dst)
    os.makedirs(dst)
    for name in sorted(os.listdir(src)):
        s = os.path.join(src, name)
        if os.path.isfile(s):
            subprocess.run(["cp", "-c", s, os.path.join(dst, name)], check=True)


def warm_cache(directory):
    """Pull the set through the page cache so no tool races another for it."""
    for name in sorted(os.listdir(directory)):
        path = os.path.join(directory, name)
        if os.path.isfile(path):
            with open(path, "rb") as fh:
                while fh.read(8 << 20):
                    pass


def md5_of(path):
    h = hashlib.md5()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(8 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def is_backup(name):
    """par2cmdline renames a damaged file to <name>.1 before rewriting it.
    Those backups are not part of the protected set and must not be compared."""
    tail = name.rsplit(".", 1)[-1]
    return tail.isdigit() or tail in ("bak", "orig")


def data_sums(directory):
    return {
        n: md5_of(os.path.join(directory, n))
        for n in sorted(os.listdir(directory))
        if not n.endswith(".par2") and not is_backup(n)
        and os.path.isfile(os.path.join(directory, n))
    }


# ---------------------------------------------------------------- running

def timed_run(argv, cwd):
    """Run argv, returning (seconds, peak_rss_bytes, exit_code, stdout)."""
    read_fd, write_fd = os.pipe()
    start = time.monotonic()
    pid = os.fork()
    if pid == 0:  # child
        try:
            os.close(read_fd)
            os.dup2(write_fd, 1)
            os.close(write_fd)
            devnull = os.open(os.devnull, os.O_WRONLY)
            os.dup2(devnull, 2)
            os.chdir(cwd)
            os.execvp(argv[0], argv)
        except Exception:
            os._exit(127)

    os.close(write_fd)
    chunks = []
    while True:
        data = os.read(read_fd, 65536)
        if not data:
            break
        chunks.append(data)
    os.close(read_fd)

    _, status, rusage = os.wait4(pid, 0)
    elapsed = time.monotonic() - start
    code = os.waitstatus_to_exitcode(status)

    # Darwin reports ru_maxrss in bytes, Linux in kilobytes.
    rss = rusage.ru_maxrss if sys.platform == "darwin" else rusage.ru_maxrss * 1024
    return elapsed, rss, code, b"".join(chunks).decode("utf-8", "replace")


def apply_damage(directory, mode, count, slice_size, seed):
    subprocess.run(
        [sys.executable, DAMAGE, directory, "--mode", mode, "--count", str(count),
         "--slice-size", str(slice_size), "--seed", str(seed)],
        check=True, stdout=subprocess.DEVNULL,
    )


# ---------------------------------------------------------------- scenarios

# name -> (par2 op, damage mode, which --count option supplies the amount)
SCENARIOS = {
    "verify-intact":  ("verify", None,      None),
    "verify-damaged": ("verify", "corrupt", "corrupt_slices"),
    "repair-missing": ("repair", "missing", "missing_files"),
    "repair-corrupt": ("repair", "corrupt", "corrupt_slices"),
}


def build_tools(args):
    # gopar-turbo always performs the byte-wise misaligned-data search on a
    # slice miss (par2/decoder.go fillShardInfos), whereas par2cmdline gates
    # that behind -N. The "-N" row is the like-for-like damaged comparison;
    # the plain row is each tool's default behaviour.
    return [
        ("gopar-turbo (cgo/SIMD)", lambda op, idx: [args.gopar_cgo, "-op", op, "-par", idx]),
        ("gopar-turbo (pure Go)",  lambda op, idx: [args.gopar_pure, "-op", op, "-par", idx]),
        ("par2cmdline-turbo",      lambda op, idx: [args.par2_turbo, op, "-q", idx]),
        ("par2cmdline (stock)",    lambda op, idx: [args.par2_stock, op, "-q", idx]),
        ("par2turbo -N (misalign)", lambda op, idx: [args.par2_turbo, op, "-q", "-N", idx]),
    ]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--pristine", required=True, help="pristine dataset directory")
    ap.add_argument("--dataset", required=True, help="dataset label for the report")
    ap.add_argument("--index", required=True, help="PAR2 index filename inside the set")
    ap.add_argument("--work", required=True)
    ap.add_argument("--results", required=True)
    ap.add_argument("--gopar-cgo", required=True)
    ap.add_argument("--gopar-pure", required=True)
    ap.add_argument("--par2-turbo", required=True)
    ap.add_argument("--par2-stock", default="/opt/homebrew/bin/par2")
    ap.add_argument("--slice-size", type=int, default=2380956)
    ap.add_argument("--seed", type=int, default=1337)
    ap.add_argument("--reps", type=int, default=1)
    ap.add_argument("--missing-files", type=int, default=5)
    ap.add_argument("--corrupt-slices", type=int, default=200)
    ap.add_argument("--scenarios", default=",".join(SCENARIOS))
    ap.add_argument("--skip-tools", default="",
                    help="comma-separated tool-name substrings to skip")
    args = ap.parse_args()

    os.makedirs(args.results, exist_ok=True)
    csv_path = os.path.join(args.results, "results.csv")
    new_file = not os.path.exists(csv_path)
    csv_fh = open(csv_path, "a", newline="")
    writer = csv.writer(csv_fh)
    if new_file:
        writer.writerow(["dataset", "scenario", "tool", "rep", "seconds",
                         "peak_rss_bytes", "exit_code", "md5_ok", "notes"])

    print(f"computing pristine MD5 baseline for {args.dataset} ...", file=sys.stderr)
    baseline = data_sums(args.pristine)

    counts = {"missing_files": args.missing_files, "corrupt_slices": args.corrupt_slices}
    skips = [s for s in args.skip_tools.split(",") if s]
    tools = [t for t in build_tools(args) if not any(s in t[0] for s in skips)]

    for scenario in args.scenarios.split(","):
        scenario = scenario.strip()
        if not scenario:
            continue
        op, mode, count_key = SCENARIOS[scenario]
        print(f"\n=== {args.dataset} / {scenario} ===", file=sys.stderr)

        for rep in range(1, args.reps + 1):
            for tool_name, argv_for in tools:
                run_dir = os.path.join(args.work, "run")
                clone_tree(args.pristine, run_dir)
                if mode:
                    apply_damage(run_dir, mode, counts[count_key],
                                 args.slice_size, args.seed)
                warm_cache(run_dir)

                secs, rss, code, out = timed_run(argv_for(op, args.index), run_dir)

                notes = ""
                if tool_name.startswith("gopar"):
                    try:
                        notes = (json.loads(out).get("error") or "")[:90]
                    except Exception:
                        notes = "no JSON result"

                if op == "repair":
                    md5_ok = "ok" if data_sums(run_dir) == baseline else "MISMATCH"
                else:
                    md5_ok = "n/a"

                writer.writerow([args.dataset, scenario, tool_name, rep,
                                 f"{secs:.3f}", rss, code, md5_ok, notes])
                csv_fh.flush()
                print(f"  {tool_name:<26} rep{rep}  {secs:8.2f}s  "
                      f"rss={rss / GIB:6.2f} GiB  exit={code}  md5={md5_ok}"
                      f"{'  ' + notes if notes else ''}", file=sys.stderr)

                shutil.rmtree(run_dir, ignore_errors=True)

    csv_fh.close()


if __name__ == "__main__":
    main()
