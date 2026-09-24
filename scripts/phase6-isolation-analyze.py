#!/usr/bin/env python3
"""Phase 6 G4 isolation drill analyzer.

Reads a mixed-loadgen log (REQ lines + SUMMARY) and optionally the Redis
ledger scan rows ("<key> <state> <request_status>" lines produced by the
window script) and reports:

  * per-model ok / err / cut counts (cut = HTTP 200 but stream ended without
    the "[DONE]" sentinel -> upstream died mid-stream),
  * the cut request-ids (these are the streams the force-kill interrupted),
  * a ledger join: state/request_status distribution for the rids observed,
    proving upstream_error -> state=pending and no quota deduction.

Usage:
  phase6-isolation-analyze.py --log isolation.log \
      [--ledger-rows ledger-b-rows.txt] [--out isolation-analysis.md]
"""

import argparse
import json
import re
import sys
from collections import Counter, defaultdict

ap = argparse.ArgumentParser()
ap.add_argument("--log", required=True)
ap.add_argument("--ledger-rows", default=None)
ap.add_argument("--out", default=None)
args = ap.parse_args()

per_model = defaultdict(Counter)
cut_rids = {"a": [], "b": []}
all_rids = defaultdict(set)  # rid -> models
summary = None

with open(args.log, encoding="utf-8", errors="replace") as fh:
    for line in fh:
        if line.startswith("REQ "):
            try:
                rec = json.loads(line[4:])
            except json.JSONDecodeError:
                continue
            m = rec.get("model", "?")
            rid = rec.get("rid", "?")
            all_rids[m].add(rid)
            if rec.get("err"):
                per_model[m]["err"] += 1
            elif rec.get("ok"):
                if rec.get("complete"):
                    per_model[m]["ok_complete"] += 1
                else:
                    per_model[m]["cut"] += 1
                    cut_rids[m].append(rid)
            else:
                per_model[m]["unknown"] += 1
        elif line.startswith("SUMMARY "):
            try:
                summary = json.loads(line[8:])
            except json.JSONDecodeError:
                pass

ledger = {}
if args.ledger_rows:
    with open(args.ledger_rows, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            parts = line.split()
            if len(parts) >= 3:
                key, state, status = parts[0], parts[1], parts[2]
                rid = key.split("usage:ledger:", 1)[-1]
                ledger[rid] = (state, status)

out = []
out.append("# G4 isolation drill analysis")
out.append("")
out.append("| model | ok(complete) | cut(no [DONE]) | err |")
out.append("|---|---|---|---|")
for m in sorted(per_model):
    c = per_model[m]
    out.append(f"| {m} | {c.get('ok_complete', 0)} | {c.get('cut', 0)} | {c.get('err', 0)} |")
out.append("")
if summary:
    out.append("summary json: `" + json.dumps(summary, ensure_ascii=False) + "`")
    out.append("")

out.append("## cut streams (client-side evidence of the mid-stream kill)")
out.append("")
for m in sorted(cut_rids):
    rids = cut_rids[m]
    out.append(f"- model {m}: {len(rids)} cut rids" + (f", first 10: {rids[:10]}" if rids else ""))
out.append("")

if ledger:
    out.append("## ledger join for observed rids")
    out.append("")
    states = Counter()
    b_states = Counter()
    for m in sorted(all_rids):
        for rid in all_rids[m]:
            if rid in ledger:
                state, status = ledger[rid]
                states[f"{m}:{state}/{status}"] += 1
                if m == "b":
                    b_states[f"{state}/{status}"] += 1
    out.append("| model:state/status | count |")
    out.append("|---|---|")
    for k in sorted(states):
        out.append(f"| {k} | {states[k]} |")
    out.append("")
    cut_with_pending = 0
    for m in cut_rids:
        for rid in cut_rids[m]:
            st = ledger.get(rid)
            if st and st[0] == "pending":
                cut_with_pending += 1
    out.append(f"cut rids with ledger state=pending: **{cut_with_pending}**")
    out.append("")
    b_pending = b_states.get("pending/upstream_error", 0)
    b_billed = sum(v for k, v in b_states.items() if k.startswith("billed"))
    out.append(f"model-b ledger rows scanned: pending/upstream_error={b_pending}, billed(any status)={b_billed}")
else:
    out.append("(no ledger rows supplied; skipped join)")
out.append("")

text = "\n".join(out)
print(text)
if args.out:
    with open(args.out, "w", encoding="utf-8") as fh:
        fh.write(text + "\n")

sys.exit(0)
