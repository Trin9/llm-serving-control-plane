#!/usr/bin/env python3
"""Phase 6 (P6-A) A/B analyzer.

Reads the artifacts produced by scripts/phase6-ab-driver.sh:

  <art>/trials/index.tsv     trial -> wall-clock window (epoch seconds)
  <art>/trials/<name>.log    loadgen output: TRIAL/REQ/SUMMARY JSON lines
  <art>/snapshots/<name>/    Prometheus instant snapshots (q<i>.json + README)

and produces:

  * summary.md - markdown tables (per strategy/workload/size, median over trials)
  * summary.json - machine-readable aggregates

Per trial it derives, from Prometheus counter deltas between the previous
snapshot and this trial's snapshot (the pool is idle between trials):

  * routing distribution per pod (vllm:request_success_total delta share)
  * prefix-cache hit rate (hits/queries delta)
  * computed prefill tokens = prompt_tokens_total - prompt_tokens_cached_total
    (falls back to prompt_tokens_total when the cached counter is absent)
  * vLLM-side TTFT mean and prefill time mean
  * gateway-side ai_ttft mean delta
and, from the loadgen log:

  * client-side TTFT p50/p95/p99/mean/max of the concurrent burst
  * warmup (cold) TTFTs and the number of burst requests above 4x the burst p50
    ("cold-like" requests)

Usage:
  python3 scripts/phase6-ab-analyze.py <art_dir>      # e.g. artifacts/phase6-azure/<RUN_ID>/G1-prefix-ab
"""

import json
import os
import statistics
import sys
from datetime import datetime

ART = sys.argv[1] if len(sys.argv) > 1 else "."
TRIALS = os.path.join(ART, "trials")
SNAPS = os.path.join(ART, "snapshots")

KEYS = {
    "req_success": 0,
    "prompt_tokens": 1,
    "prompt_tokens_cached": 2,
    "cache_queries": 3,
    "cache_hits": 4,
    "ttft_sum": 5,
    "ttft_count": 6,
    "prefill_sum": 7,
    "prefill_count": 8,
    "kv_usage": 9,
}


def load_snapshot(tag):
    """Returns {(query_idx, pod): value} for vector results."""
    out = {}
    d = os.path.join(SNAPS, tag)
    if not os.path.isdir(d):
        return None
    for name in os.listdir(d):
        if not name.startswith("q") or not name.endswith(".json"):
            continue
        idx = int(name[1:-5])
        try:
            with open(os.path.join(d, name)) as fh:
                payload = json.load(fh)
        except (OSError, json.JSONDecodeError):
            continue
        if payload.get("status") != "success":
            continue
        for series in payload["data"]["result"]:
            pod = series["metric"].get("pod", series["metric"].get("code", "?"))
            try:
                value = float(series["value"][1])
            except (ValueError, IndexError):
                continue
            out[(idx, pod)] = value
    return out


def delta(before, after, idx):
    """Per-pod counter delta; None if any pod's counter went backwards."""
    pods = {p for (i, p) in after if i == idx}
    result = {}
    for p in pods:
        a = after.get((idx, p))
        b = before.get((idx, p)) if before else None
        if a is None:
            continue
        result[p] = a - b if b is not None else None
    return result


def parse_trial_log(path):
    summary = None
    reqs = []
    for line in open(path, errors="replace"):
        if line.startswith("SUMMARY "):
            summary = json.loads(line[8:])
        elif line.startswith("REQ "):
            try:
                reqs.append(json.loads(line[4:]))
            except json.JSONDecodeError:
                pass
    return summary, reqs


def median_or_none(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def fmt(x, nd=2):
    if x is None:
        return "-"
    if isinstance(x, float):
        return f"{x:.{nd}f}"
    return str(x)


def main():
    index_path = os.path.join(TRIALS, "index.tsv")
    if not os.path.isfile(index_path):
        sys.exit(f"missing {index_path}")

    rows = []
    prev_by_strategy = {}
    for line in open(index_path):
        parts = line.rstrip("\n").split("\t")
        if len(parts) < 8:
            continue
        name, strat, size, prefixes, trial, t0, t1, seed = parts[:8]
        snap = load_snapshot(name)
        # Baseline for this trial: the previous trial of the SAME strategy, or
        # the phase-start snapshot (pool idle in both cases, so counter deltas
        # between the two snapshots describe exactly this trial's traffic).
        prev_tag = prev_by_strategy.get(strat, f"phase-{strat}-start")
        prev_snap = load_snapshot(prev_tag)
        logp = os.path.join(TRIALS, f"{name}.log")
        summary, reqs = (None, [])
        if os.path.isfile(logp):
            summary, reqs = parse_trial_log(logp)

        row = {
            "name": name,
            "strategy": strat,
            "prefix_chars": int(size),
            "prefixes": int(prefixes),
            "trial": int(trial),
            "t0": int(t0),
            "t1": int(t1),
            "duration_s": summary.get("duration_s") if summary else None,
            "ok": summary.get("ok") if summary else None,
            "err": summary.get("err") if summary else None,
            "client_ttft_p50": summary.get("burst_ttft_p50") if summary else None,
            "client_ttft_p95": summary.get("burst_ttft_p95") if summary else None,
            "client_ttft_p99": summary.get("burst_ttft_p99") if summary else None,
            "client_ttft_mean": summary.get("burst_ttft_mean") if summary else None,
            "client_ttft_max": summary.get("burst_ttft_max") if summary else None,
            "warmup_ttft": summary.get("warmup_ttfts") if summary else None,
            "prompt_tokens_served": summary.get("sum_prompt_tokens") if summary else None,
            # Prometheus-derived fields (None until a snapshot pair exists)
            "routing": None,
            "prompt_tokens_total": None,
            "prompt_tokens_cached": None,
            "cache_queries": None,
            "cache_hits": None,
            "hit_rate": None,
            "computed_prefill": None,
            "vllm_ttft_mean": None,
            "vllm_prefill_mean": None,
        }

        # cold-like burst requests: TTFT > 4x the burst p50 (a cache-miss prefill)
        p50 = row["client_ttft_p50"]
        if p50:
            row["cold_like"] = sum(
                1 for r in reqs if not r.get("warmup") and r.get("ttft", 0) > 4 * p50
            )
        else:
            row["cold_like"] = None

        if snap is not None and prev_snap is not None:
            dist = delta(prev_snap, snap, KEYS["req_success"])
            total = sum(v for v in dist.values() if v is not None) or 0.0
            row["routing"] = {
                p: (round(v / total, 3) if (v is not None and total > 0) else None)
                for p, v in sorted(dist.items())
            }
            pt = delta(prev_snap, snap, KEYS["prompt_tokens"])
            ptc = delta(prev_snap, snap, KEYS["prompt_tokens_cached"])
            q = delta(prev_snap, snap, KEYS["cache_queries"])
            h = delta(prev_snap, snap, KEYS["cache_hits"])
            row["prompt_tokens_total"] = sum(v for v in pt.values() if v is not None)
            row["prompt_tokens_cached"] = sum(v for v in ptc.values() if v is not None) \
                if ptc and all(v is not None for v in ptc.values()) else None
            row["cache_queries"] = sum(v for v in q.values() if v is not None) if q else None
            row["cache_hits"] = sum(v for v in h.values() if v is not None) if h else None
            if row["cache_queries"] and row["cache_queries"] > 0:
                row["hit_rate"] = row["cache_hits"] / row["cache_queries"]
            if row["prompt_tokens_cached"] is not None and row["prompt_tokens_total"]:
                row["computed_prefill"] = row["prompt_tokens_total"] - row["prompt_tokens_cached"]
            ts = delta(prev_snap, snap, KEYS["ttft_sum"])
            tc = delta(prev_snap, snap, KEYS["ttft_count"])
            tsum = sum(v for v in ts.values() if v is not None)
            tcnt = sum(v for v in tc.values() if v is not None)
            row["vllm_ttft_mean"] = (tsum / tcnt) if tcnt else None
            ps = delta(prev_snap, snap, KEYS["prefill_sum"])
            pc = delta(prev_snap, snap, KEYS["prefill_count"])
            psum = sum(v for v in ps.values() if v is not None)
            pcnt = sum(v for v in pc.values() if v is not None)
            row["vllm_prefill_mean"] = (psum / pcnt) if pcnt else None

        row["snapshot_tag"] = name
        row["_prev_tag"] = prev_tag
        rows.append(row)
        if snap is not None:
            prev_by_strategy[strat] = name

    # ---- aggregate: median over trials per (strategy, prefixes, size) --------
    groups = {}
    for r in rows:
        groups.setdefault((r["strategy"], r["prefixes"], r["prefix_chars"]), []).append(r)

    agg = []
    for key in sorted(groups, key=lambda k: (k[0], k[1], k[2])):
        strat, prefixes, size = key
        rs = groups[key]
        a = {
            "strategy": strat,
            "prefixes": prefixes,
            "prefix_chars": size,
            "trials": len(rs),
            "client_ttft_p50": median_or_none([r["client_ttft_p50"] for r in rs]),
            "client_ttft_p95": median_or_none([r["client_ttft_p95"] for r in rs]),
            "client_ttft_mean": median_or_none([r["client_ttft_mean"] for r in rs]),
            "cold_like": median_or_none([r["cold_like"] for r in rs]),
            "prompt_tokens_total": median_or_none([r["prompt_tokens_total"] for r in rs]),
            "computed_prefill": median_or_none([r["computed_prefill"] for r in rs]),
            "hit_rate": median_or_none([r["hit_rate"] for r in rs]),
            "vllm_prefill_mean": median_or_none([r["vllm_prefill_mean"] for r in rs]),
            "vllm_ttft_mean": median_or_none([r["vllm_ttft_mean"] for r in rs]),
            "err": sum((r["err"] or 0) for r in rs),
        }
        # routing share: average per pod across trials
        pods = set()
        for r in rs:
            if r["routing"]:
                pods.update(r["routing"].keys())
        a["routing_share"] = {
            p: (median_or_none([r["routing"].get(p) for r in rs]) if r["routing"] else None)
            for p in sorted(pods)
        }
        agg.append(a)

    # cross-strategy reduction table for each (prefixes, size)
    reductions = []
    for prefixes, size in sorted({(a["prefixes"], a["prefix_chars"]) for a in agg}):
        ph = next((a for a in agg if a["strategy"] == "prefix-hash" and a["prefixes"] == prefixes and a["prefix_chars"] == size), None)
        rn = next((a for a in agg if a["strategy"] == "random" and a["prefixes"] == prefixes and a["prefix_chars"] == size), None)
        if not ph or not rn:
            continue
        def red(base, alt):
            if base is None or alt is None or base == 0:
                return None
            return (base - alt) / base
        reductions.append({
            "prefixes": prefixes,
            "prefix_chars": size,
            "prefill_reduction": red(rn["computed_prefill"], ph["computed_prefill"]),
            "ttft_p50_reduction": red(rn["client_ttft_p50"], ph["client_ttft_p50"]),
            "ttft_p95_reduction": red(rn["client_ttft_p95"], ph["client_ttft_p95"]),
            "hit_rate_prefix_hash": ph["hit_rate"],
            "hit_rate_random": rn["hit_rate"],
        })

    # ---- write outputs -------------------------------------------------------
    with open(os.path.join(ART, "summary.json"), "w") as fh:
        json.dump({"trials": rows, "aggregate": agg, "reductions": reductions}, fh, indent=2)

    lines = ["# P6-A prefix-aware routing A/B - summary", ""]
    lines.append(f"generated: {datetime.utcnow().isoformat(timespec='seconds')}Z")
    lines.append("")
    lines.append("## Per (strategy, workload) - median over trials")
    lines.append("")
    lines.append("| strategy | workload | prefix_chars | trials | routing share | hit_rate | computed prefill (tok) | vLLM prefill mean (s) | client TTFT p50 (s) | p95 (s) | cold-like | err |")
    lines.append("|---|---|---|---|---|---|---|---|---|---|---|---|")
    for a in agg:
        wl = "single" if a["prefixes"] == 1 else f"{a['prefixes']} prefixes"
        share = ", ".join(f"{p[:14]}={fmt(v, 2)}" for p, v in a["routing_share"].items()) or "-"
        lines.append(
            f"| {a['strategy']} | {wl} | {a['prefix_chars']} | {a['trials']} | {share} | "
            f"{fmt(a['hit_rate'], 3)} | {fmt(a['computed_prefill'], 0)} | {fmt(a['vllm_prefill_mean'], 3)} | "
            f"{fmt(a['client_ttft_p50'], 3)} | {fmt(a['client_ttft_p95'], 3)} | {fmt(a['cold_like'], 0)} | {a['err']} |"
        )
    lines.append("")
    lines.append("## prefix-hash improvement vs random (same workload)")
    lines.append("")
    lines.append("| workload | prefix_chars | prefill reduction | TTFT p50 reduction | TTFT p95 reduction | hit rate (prefix-hash / random) |")
    lines.append("|---|---|---|---|---|---|")
    for r in reductions:
        wl = "single" if r["prefixes"] == 1 else f"{r['prefixes']} prefixes"
        lines.append(
            f"| {wl} | {r['prefix_chars']} | {fmt(r['prefill_reduction'] * 100 if r['prefill_reduction'] is not None else None, 1)}% | "
            f"{fmt(r['ttft_p50_reduction'] * 100 if r['ttft_p50_reduction'] is not None else None, 1)}% | "
            f"{fmt(r['ttft_p95_reduction'] * 100 if r['ttft_p95_reduction'] is not None else None, 1)}% | "
            f"{fmt(r['hit_rate_prefix_hash'], 3)} / {fmt(r['hit_rate_random'], 3)} |"
        )
    md = "\n".join(lines) + "\n"
    with open(os.path.join(ART, "summary.md"), "w") as fh:
        fh.write(md)
    print(md)


if __name__ == "__main__":
    main()
