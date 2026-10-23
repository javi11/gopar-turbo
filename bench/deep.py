#!/usr/bin/env python3
"""Checksum-validated deep repair comparison; see bench/README.md."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import random
import shutil
import subprocess
import sys
import tempfile
import time


def sha256(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def measure(argv, cwd, log, timeout):
    with log.open('wb') as output:
        child = subprocess.Popen(argv, cwd=cwd, stdout=output,
                                 stderr=subprocess.STDOUT, start_new_session=True)
        start = time.perf_counter()
        expired = False
        while True:
            pid, status, usage = os.wait4(child.pid, os.WNOHANG)
            if pid:
                break
            if time.perf_counter() - start > timeout:
                import signal
                os.killpg(child.pid, signal.SIGKILL)
                _, status, usage = os.wait4(child.pid, 0)
                expired = True
                break
            time.sleep(0.005)
        child.returncode = os.waitstatus_to_exitcode(status)
    rss = usage.ru_maxrss * (1 if sys.platform == 'darwin' else 1024)
    elapsed = time.perf_counter() - start
    cpu = usage.ru_utime + usage.ru_stime
    return dict(seconds=elapsed, peak_rss_bytes=rss,
                user_cpu_seconds=usage.ru_utime, system_cpu_seconds=usage.ru_stime,
                cpu_seconds=cpu, average_cpu_cores=cpu / elapsed,
                exit_code=child.returncode, timeout=expired)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--fixture', required=True, type=Path)
    parser.add_argument('--gopar', required=True, type=Path)
    parser.add_argument('--baseline-gopar', type=Path,
                        help='also compare an earlier gopar binary at the same worker counts')
    parser.add_argument('--parfast', type=Path)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--missing', default='1024,2048,4096,8192')
    parser.add_argument('--slice-size', type=int, default=16384)
    parser.add_argument('--repeat', type=int, default=3)
    parser.add_argument('--seed', type=int, default=1337)
    parser.add_argument('--timeout', type=float, default=300)
    parser.add_argument('--memory-mib', type=int, default=0,
                        help='same explicit memory budget for both tools; 0 uses their defaults')
    parser.add_argument('--gopar-workers', default='0',
                        help='comma-separated worker counts to compare; 0 uses the default')
    parser.add_argument('--only-gopar', action='store_true',
                        help='skip parfast when comparing gopar worker counts')
    args = parser.parse_args()
    if not args.only_gopar and args.parfast is None:
        parser.error('--parfast is required unless --only-gopar is set')
    if args.memory_mib < 0:
        parser.error('memory budget must be nonnegative')
    try:
        workers = [int(value) for value in args.gopar_workers.split(',')]
    except ValueError:
        parser.error('worker counts must be integers')
    if any(w < 0 for w in workers) or len(set(workers)) != len(workers):
        parser.error('worker counts must be distinct and nonnegative')
    if args.output.exists() and (not args.output.is_dir() or any(args.output.iterdir())):
        parser.error('output must be a new or empty directory to avoid mixing benchmark runs')
    if os.name != 'posix':
        parser.error('requires POSIX wait4 for per-process peak RSS')
    fixture = args.fixture.resolve()
    files = sorted(fixture.glob('data*.bin'))
    if not files or args.slice_size < 64 or args.repeat < 1 or args.timeout <= 0:
        parser.error('requires data*.bin, slice size >=64, positive repeats and timeout')
    shards = []
    expected = {}
    for file in files:
        size = file.stat().st_size
        if size % args.slice_size:
            parser.error('data files must contain whole slices')
        expected[file.name] = sha256(file)
        shards.extend((file.name, offset) for offset in range(0, size, args.slice_size))
    depths = [int(value) for value in args.missing.split(',')]
    if any(k <= 0 or k > len(shards) for k in depths):
        parser.error('missing count outside data shard range')
    if not (fixture / 'recovery.par2').is_file():
        parser.error('fixture must contain recovery.par2')
    tools = [] if args.only_gopar else [('parfast', args.parfast.resolve(), 0)]
    tools.extend(('gopar' if workers == [0] else f'gopar-{w}', args.gopar.resolve(), w)
                 for w in workers)
    if args.baseline_gopar:
        tools.extend((f'baseline-gopar-{w}', args.baseline_gopar.resolve(), w)
                     for w in workers)
    args.output.mkdir(parents=True, exist_ok=True)
    manifest = dict(fixture_sha256=expected, data_shards=len(shards),
                    slice_size=args.slice_size, damage_seed=args.seed,
                    memory_budget_mib=args.memory_mib,
                    parity_sha256={p.name: sha256(p) for p in sorted(fixture.glob("*.par2"))},
                    tools={name: dict(path=str(path), sha256=sha256(path), workers=w)
                           for name, path, w in tools})
    (args.output / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    failed = False
    with (args.output / 'results.jsonl').open('a') as results:
        for k in depths:
            damaged = random.Random(args.seed).sample(shards, k)
            for repetition in range(args.repeat):
                # Alternate tool order to reduce systematic cache/thermal bias.
                for name, binary, worker_count in tools[::1 if repetition % 2 == 0 else -1]:
                    with tempfile.TemporaryDirectory(prefix='gopar-deep-') as directory:
                        work = Path(directory) / 'work'
                        if sys.platform == 'darwin':
                            subprocess.run(['cp', '-cR', str(fixture), str(work)], check=True)
                        else:
                            shutil.copytree(fixture, work)
                        for filename, offset in damaged:
                            with (work / filename).open('r+b') as stream:
                                stream.seek(offset)
                                stream.write(b'\xa5' * 64)
                        for file in work.iterdir():
                            if file.is_file():
                                with file.open('rb') as stream:
                                    while stream.read(1 << 20):
                                        pass
                        index = str(work / 'recovery.par2')
                        argv = ([str(binary), 'repair', '-q', index] if name == 'parfast'
                                else [str(binary), '-op', 'repair', '-par', index])
                        if name != 'parfast' and worker_count:
                            argv.extend(['-goroutines', str(worker_count)])
                        if args.memory_mib:
                            if name == 'parfast':
                                argv.insert(-1, f'-m{args.memory_mib}')
                            else:
                                argv.extend(['-memory-budget', str(args.memory_mib << 20)])
                        log = args.output / f'{name}-{k}-{repetition}.log'
                        result = measure(argv, str(work), log, args.timeout)
                        result.update(tool=name, missing=k, repetition=repetition, argv=argv,
                                      checksum_ok=all((work / file).is_file() and sha256(work / file) == digest
                                                      for file, digest in expected.items()))
                        result['valid'] = (result['exit_code'] == 0 and not result['timeout']
                                           and result['checksum_ok'])
                        failed |= not result['valid']
                        line = json.dumps(result)
                        print(line, flush=True)
                        results.write(line + '\n')
                        results.flush()
    return int(failed)


if __name__ == '__main__':
    sys.exit(main())
