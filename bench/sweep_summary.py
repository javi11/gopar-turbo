import re, statistics, glob, sys, os, csv
d = sys.argv[1]
print(open(os.path.join(d, "..", "cpuinfo.txt")).read().split("\n")[0] if os.path.exists(os.path.join(d, "..", "cpuinfo.txt")) else "")
rows = {}
for f in sorted(glob.glob(d + "/fold-*.txt")):
    if "benchstat" in f:
        continue
    v = [float(m.group(1)) for m in re.finditer(r'Realistic-\d+\s+\d+\s+([\d.]+) ns/op', open(f).read())]
    rows[os.path.basename(f)[5:-4]] = v
base = statistics.median(rows["base-auto"])
print("FoldInputs_Realistic (ms): label median [min-max] vs base-auto")
for k, v in rows.items():
    print("  %-14s %7.1f [%6.1f-%6.1f] %+6.1f%%" % (k, statistics.median(v) / 1e6, min(v) / 1e6, max(v) / 1e6, (statistics.median(v) / base - 1) * 100))
t = {}
for r in csv.DictReader(open(d + "/e2e/results.csv")):
    if r["exit_code"] == "0":
        t.setdefault((r["scenario"], r["label"]), []).append(float(r["seconds"]))
for sc in ("repair-missing", "repair-corrupt"):
    print(sc, "(s): label median [min-max] vs base-auto")
    b = statistics.median(t[(sc, "base-auto")])
    for (s, l), v in t.items():
        if s == sc:
            print("  %-14s %6.2f [%5.2f-%5.2f] %+6.1f%% n=%d" % (l, statistics.median(v), min(v), max(v), (statistics.median(v) / b - 1) * 100, len(v)))
