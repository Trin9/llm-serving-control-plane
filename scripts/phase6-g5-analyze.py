#!/usr/bin/env python3
"""Phase 6 G5 SLO / burst analyzer.

Reads a mixed-loadgen log (REQ lines carry a `t0` epoch-seconds field) and
reports, relative to a burst start epoch:

  * whole-run: ok / err / cut, RPS, TTFT p50/p95/p99
  * SLO window (first 60s of the burst): counts + TTFT p50/p95/p99 + errors
  * recovery: first 10s bucket after burst-start whose TTFT p95 < slo_ms

Usage:
  phase6-g5-analyze.py --log G5-c-min1-load.txt --burst-start <epoch> \
      [--slo-ms 100] [--out md]
"""

import argparse
import json
import sys
from collections import Counter, defaultdict

ap = argparse.ArgumentParser()
ap.add_argument("--log", required=True)
ap.add_argument("--burst-start", type=float, required=True)
ap.add_argument("--slo-ms", type=float, default=100.0)
ap.add_argument("--out", default=None)
args = ap.parse_args()

ok = 0
err = 0
cut = 0
codes = Counter()
ttfts = []           # all ok ttft
win_ttfts = []       # ok ttft within first 60s
win_err = 0
buckets = defaultdict(list)  # 10s bucket -> ttfts

with open(args.log, encoding="utf-8", errors="replace") as fh:
    for line in fh:
        if not line.startswith("REQ "):
            continue
        try:
            rec = json.loads(line[4:])
        except json.JSONDecodeError:
            continue
        t0 = rec.get("t0")
        if t0 is None:
            continue
        if rec.get("err"):
            err += 1
            if args.burst_start <= t0 < args.burst_start + 60:
                win_err += 1
            continue
        if not rec.get("ok"):
            continue
        ok += 1
        ttft = rec.get("ttft")
        if not rec.get("complete"):
            cut += 1
        if ttft is not None:
            ttft_s = ttft * 1000.0
            ttfts.append(ttft_s)
            bucket = int((t0 - args.burst_start) // 10) * 10
            buckets[bucket].append(ttft_s)
            if args.burst_start <= t0 < args.burst_start + 60:
                win_ttfts.append(ttft_s)


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1)))))
    return round(xs[k], 1)


def dur():
    if not ttfts:
        return 0.0
    return 0.0  # unknown without explicit duration; skip


# recovery: first 10s bucket (>= burst start, non-empty) with p95 < slo
recovery = None
for b in sorted(buckets):
    if b < 0:
        continue
    p95 = pct(buckets[b], 95)
    if p95 is not None and p95 < args.slo_ms:
        recovery = b
        break

out = []
out.append("# G5 burst analysis")
out.append("")
out.append(f"- burst_start epoch: {args.burst_start}")
out.append(f"- SLO: TTFT p95 < {args.slo_ms}ms")
out.append("")
out.append("| metric | whole-run | first-60s (SLO window) |")
out.append("|---|---|---|")
out.append(f"| ok | {ok} | {len(win_ttfts)} |")
out.append(f"| err | {err} | {win_err} |")
out.append(f"| cut | {cut} | — |")
out.append(f"| TTFT p50 (ms) | {pct(ttfts, 50)} | {pct(win_ttfts, 50)} |")
out.append(f"| TTFT p95 (ms) | {pct(ttfts, 95)} | {pct(win_ttfts, 95)} |")
out.append(f"| TTFT p99 (ms) | {pct(ttfts, 99)} | {pct(win_ttfts, 99)} |")
out.append("")
out.append(f"recovery_to_slo_s: {recovery if recovery is not None else 'never'}")
out.append("")
out.append("10s-bucket TTFT p95 (ms):")
out.append("```")
for b in sorted(buckets):
    out.append(f"  t+{b:>4d}s  p95={pct(buckets[b], 95)}  n={len(buckets[b])}")
out.append("```")

text = "\n".join(out)
print(text)
if args.out:
    with open(args.out, "w", encoding="utf-8") as fh:
        fh.write(text + "\n")
sys.exit(0)
