#!/usr/bin/env python3
"""Phase 6 (P6-A) A/B analyzer v2 - precise per-trial attribution.

Why this exists: each trial is a ~2 second burst while Prometheus scrapes every
15 seconds, so instant snapshots taken at trial end can lag and attribute a
trial's counter increase to the neighbouring trial. This analyzer instead pulls
`query_range` samples (5 s step) for the whole run, decomposes every counter
into deltas between consecutive samples, and attributes each delta to the trial
whose centre is nearest (trials are ~40 s apart, max scrape lag ~20 s, so the
attribution is unambiguous).

Inputs (same artifact dir as phase6-ab-analyze.py):
  <art>/trials/index.tsv      name strategy size prefixes trial t0 t1 seed
  <art>/trials/<name>.log     loadgen SUMMARY/REQ lines (client-side TTFT)

Outputs:
  <art>/summary-range.md      primary tables (per strategy/workload medians)
  <art>/summary-range.json    machine-readable aggregates + per-trial rows

Usage:
  python3 scripts/phase6-ab-analyze-range.py <art_dir> [prom_url]
"""

import json
import os
import statistics
import sys
import urllib.parse
import urllib.request
from datetime import datetime, timezone

ART = sys.argv[1] if len(sys.argv) > 1 else "."
PROM = sys.argv[2] if len(sys.argv) > 2 else "http://127.0.0.1:19090"
STEP = 5          # query_range step (seconds)
MAX_LAG = 45      # max seconds between a trial centre and a delta midpoint

# (key, promql) - counters that only move during trials.
QUERIES = {
    "req_success": 'sum by (pod) (vllm:request_success_total{service="qwen-service"})',
    "prompt_tokens": 'sum by (pod) (vllm:prompt_tokens_total{service="qwen-service"})',
    "prompt_tokens_cached": 'sum by (pod) (vllm:prompt_tokens_cached_total{service="qwen-service"})',
    "cache_queries": 'sum by (pod) (vllm:prefix_cache_queries_total{service="qwen-service"})',
    "cache_hits": 'sum by (pod) (vllm:prefix_cache_hits_total{service="qwen-service"})',
    "ttft_sum": 'sum by (pod) (vllm:time_to_first_token_seconds_sum{service="qwen-service"})',
    "ttft_count": 'sum by (pod) (vllm:time_to_first_token_seconds_count{service="qwen-service"})',
    "prefill_sum": 'sum by (pod) (vllm:request_prefill_time_seconds_sum{service="qwen-service"})',
    "prefill_count": 'sum by (pod) (vllm:request_prefill_time_seconds_count{service="qwen-service"})',
    "gen_tokens": 'sum by (pod) (vllm:generation_tokens_total{service="qwen-service"})',
}


def prom_range(query, start, end, step):
    params = urllib.parse.urlencode({
        "query": query, "start": f"{start:.0f}", "end": f"{end:.0f}", "step": f"{step}s",
    })
    url = f"{PROM}/api/v1/query_range?{params}"
    with urllib.request.urlopen(url, timeout=60) as resp:
        payload = json.load(resp)
    if payload.get("status") != "success":
        raise RuntimeError(f"prometheus error: {payload.get('error')}")
    return payload["data"]["result"]


def load_trials():
    trials = []
    with open(os.path.join(ART, "trials", "index.tsv")) as fh:
        for line in fh:
            parts = line.rstrip("\n").split("\t")
            if len(parts) < 8:
                continue
            name, strat, size, prefixes, trial, t0, t1, seed = parts[:8]
            summary = None
            logp = os.path.join(ART, "trials", f"{name}.log")
            reqs = []
            if os.path.isfile(logp):
                for logline in open(logp, errors="replace"):
                    if logline.startswith("SUMMARY "):
                        summary = json.loads(logline[8:])
                    elif logline.startswith("REQ "):
                        try:
                            reqs.append(json.loads(logline[4:]))
                        except json.JSONDecodeError:
                            pass
            t = {
                "name": name, "strategy": strat, "prefix_chars": int(size),
                "prefixes": int(prefixes), "trial": int(trial),
                "t0": int(t0), "t1": int(t1), "seed": seed,
                "center": (int(t0) + int(t1)) / 2.0,
                "summary": summary, "reqs": reqs,
                "metrics": {}, "routing": {},
            }
            trials.append(t)
    return trials


def main():
    trials = load_trials()
    if not trials:
        sys.exit("no trials found in index.tsv")
    start = min(t["t0"] for t in trials) - 120
    end = max(t["t1"] for t in trials) + 120

    for key, query in QUERIES.items():
        try:
            result = prom_range(query, start, end, STEP)
        except Exception as exc:  # noqa: BLE001
            print(f"[warn] query {key} failed: {exc}")
            continue
        for series in result:
            pod = series["metric"].get("pod") or series["metric"].get("code") or "gateway"
            samples = [(float(ts), float(v)) for ts, v in series["values"]]
            for (ts_a, v_a), (ts_b, v_b) in zip(samples, samples[1:]):
                if v_b < v_a:          # counter reset (pod restart) - skip
                    continue
                delta = v_b - v_a
                if delta <= 0:
                    continue
                mid = (ts_a + ts_b) / 2.0
                nearest, dist = None, 1e18
                for t in trials:
                    d = abs(mid - t["center"])
                    if d < dist:
                        nearest, dist = t, d
                if nearest is None or dist > MAX_LAG:
                    continue
                nearest["metrics"][key] = nearest["metrics"].get(key, 0.0) + delta
                if key == "req_success":
                    nearest["routing"][pod] = nearest["routing"].get(pod, 0.0) + delta

    # ---- per-trial derived values -----------------------------------------
    for t in trials:
        m = t["metrics"]
        s = t["summary"] or {}
        pt = m.get("prompt_tokens")
        ptc = m.get("prompt_tokens_cached")
        q = m.get("cache_queries")
        h = m.get("cache_hits")
        tts = m.get("ttft_sum")
        ttc = m.get("ttft_count")
        pfs = m.get("prefill_sum")
        pfc = m.get("prefill_count")
        t["derived"] = {
            "routing_share": (
                {k: round(v / sum(t["routing"].values()), 3) for k, v in t["routing"].items()}
                if t["routing"] and sum(t["routing"].values()) > 0 else {}
            ),
            "top1_share": (
                round(max(t["routing"].values()) / sum(t["routing"].values()), 3)
                if t["routing"] and sum(t["routing"].values()) > 0 else None
            ),
            "pods_used": (
                sum(1 for v in t["routing"].values() if v / sum(t["routing"].values()) >= 0.05)
                if t["routing"] and sum(t["routing"].values()) > 0 else None
            ),
            "prompt_tokens_total": pt,
            "prompt_tokens_cached": ptc if ptc is not None else None,
            "computed_prefill": (pt - ptc) if (pt is not None and ptc is not None) else None,
            "hit_rate": (h / q) if (q and h is not None and q > 0) else None,
            "vllm_ttft_mean": (tts / ttc) if (tts is not None and ttc) else None,
            "vllm_prefill_mean": (pfs / pfc) if (pfs is not None and pfc) else None,
            "client_ttft_p50": s.get("burst_ttft_p50"),
            "client_ttft_p95": s.get("burst_ttft_p95"),
            "client_ttft_mean": s.get("burst_ttft_mean"),
            "client_ttft_max": s.get("burst_ttft_max"),
            "warmup_ttfts": s.get("warmup_ttfts"),
            "ok": s.get("ok"),
            "err": s.get("err"),
            "duration_s": s.get("duration_s"),
            "sum_prompt_tokens_client": s.get("sum_prompt_tokens"),
        }
        # cold-like: burst requests with TTFT > 4x burst p50
        p50 = s.get("burst_ttft_p50")
        t["derived"]["cold_like"] = (
            sum(1 for r in t["reqs"] if not r.get("warmup") and r.get("ttft", 0) > 4 * p50)
            if p50 else None
        )

    # ---- aggregate (median over trials per strategy/workload/size) ----------
    def median(xs):
        xs = [x for x in xs if x is not None]
        return statistics.median(xs) if xs else None

    groups = {}
    for t in trials:
        groups.setdefault((t["strategy"], t["prefixes"], t["prefix_chars"]), []).append(t)

    agg = []
    for key in sorted(groups, key=lambda k: (k[0], k[1], k[2])):
        strat, prefixes, size = key
        ts = groups[key]
        pods = set()
        for t in ts:
            pods.update(t["derived"]["routing_share"].keys())
        a = {
            "strategy": strat, "prefixes": prefixes, "prefix_chars": size,
            "trials": len(ts),
            "top1_share": median([t["derived"]["top1_share"] for t in ts]),
            "pods_used": median([t["derived"]["pods_used"] for t in ts]),
            "routing_share": {p: median([t["derived"]["routing_share"].get(p) for t in ts]) for p in sorted(pods)},
            "hit_rate": median([t["derived"]["hit_rate"] for t in ts]),
            "computed_prefill": median([t["derived"]["computed_prefill"] for t in ts]),
            "prompt_tokens_total": median([t["derived"]["prompt_tokens_total"] for t in ts]),
            "vllm_prefill_mean": median([t["derived"]["vllm_prefill_mean"] for t in ts]),
            "vllm_ttft_mean": median([t["derived"]["vllm_ttft_mean"] for t in ts]),
            "client_ttft_p50": median([t["derived"]["client_ttft_p50"] for t in ts]),
            "client_ttft_p95": median([t["derived"]["client_ttft_p95"] for t in ts]),
            "client_ttft_mean": median([t["derived"]["client_ttft_mean"] for t in ts]),
            "cold_like": median([t["derived"]["cold_like"] for t in ts]),
            "err": sum((t["derived"]["err"] or 0) for t in ts),
            "duration_s": median([t["derived"]["duration_s"] for t in ts]),
        }
        agg.append(a)

    reductions = []
    for prefixes, size in sorted({(a["prefixes"], a["prefix_chars"]) for a in agg}):
        ph = next((a for a in agg if a["strategy"] == "prefix-hash" and a["prefixes"] == prefixes and a["prefix_chars"] == size), None)
        rn = next((a for a in agg if a["strategy"] == "random" and a["prefixes"] == prefixes and a["prefix_chars"] == size), None)
        if not ph or not rn:
            continue

        def red(base, alt):
            if base in (None, 0) or alt is None:
                return None
            return (base - alt) / base

        reductions.append({
            "prefixes": prefixes, "prefix_chars": size,
            "prefill_reduction": red(rn["computed_prefill"], ph["computed_prefill"]),
            "prefill_total_reduction": red(rn["prompt_tokens_total"], ph["prompt_tokens_total"]),
            "ttft_p50_reduction": red(rn["client_ttft_p50"], ph["client_ttft_p50"]),
            "ttft_p95_reduction": red(rn["client_ttft_p95"], ph["client_ttft_p95"]),
            "ttft_mean_reduction": red(rn["client_ttft_mean"], ph["client_ttft_mean"]),
            "vllm_prefill_mean_reduction": red(rn["vllm_prefill_mean"], ph["vllm_prefill_mean"]),
            "hit_rate_prefix_hash": ph["hit_rate"], "hit_rate_random": rn["hit_rate"],
        })

    out = {"generated": datetime.now(timezone.utc).isoformat(timespec="seconds"),
           "trials": [{k: v for k, v in t.items() if k != "reqs"} for t in trials],
           "aggregate": agg, "reductions": reductions}
    with open(os.path.join(ART, "summary-range.json"), "w") as fh:
        json.dump(out, fh, indent=2, default=str)

    def fmt(x, nd=3):
        return "-" if x is None else (f"{x:.{nd}f}" if isinstance(x, float) else str(x))

    lines = ["# P6-A prefix-aware routing A/B (precise range attribution)", ""]
    lines.append(f"generated: {out['generated']}  (query_range step {STEP}s, nearest-trial attribution)")
    lines.append("")
    lines.append("## Per (strategy, workload) - median over trials")
    lines.append("")
    lines.append("| strategy | workload | prefix_chars | trials | busiest-pod share | pods used | cache hit rate | computed prefill (tok) | prefill tok total | vLLM prefill mean (s) | client TTFT p50 (ms) | p95 (ms) | cold-like | err |")
    lines.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for a in agg:
        wl = "single" if a["prefixes"] == 1 else f"{a['prefixes']} prefixes"
        lines.append(
            f"| {a['strategy']} | {wl} | {a['prefix_chars']} | {a['trials']} | {fmt(a['top1_share'], 2)} | {fmt(a['pods_used'], 0)} | {fmt(a['hit_rate'])} | "
            f"{fmt(a['computed_prefill'], 0)} | {fmt(a['prompt_tokens_total'], 0)} | {fmt(a['vllm_prefill_mean'], 3)} | "
            f"{fmt(a['client_ttft_p50'] * 1000 if a['client_ttft_p50'] else None, 1)} | "
            f"{fmt(a['client_ttft_p95'] * 1000 if a['client_ttft_p95'] else None, 1)} | {fmt(a['cold_like'], 0)} | {a['err']} |"
        )
    lines.append("")
    lines.append("## prefix-hash vs random (positive = prefix-hash is better)")
    lines.append("")
    lines.append("| workload | prefix_chars | computed-prefill reduction | vLLM prefill-time reduction | TTFT p50 reduction | TTFT p95 reduction | TTFT mean reduction | hit rate (ph / rand) |")
    lines.append("|---|---|---|---|---|---|---|---|")
    for r in reductions:
        wl = "single" if r["prefixes"] == 1 else f"{r['prefixes']} prefixes"
        pct = lambda v: "-" if v is None else f"{v * 100:.1f}%"
        lines.append(
            f"| {wl} | {r['prefix_chars']} | {pct(r['prefill_reduction'])} | {pct(r['vllm_prefill_mean_reduction'])} | "
            f"{pct(r['ttft_p50_reduction'])} | {pct(r['ttft_p95_reduction'])} | {pct(r['ttft_mean_reduction'])} | "
            f"{fmt(r['hit_rate_prefix_hash'], 3)} / {fmt(r['hit_rate_random'], 3)} |"
        )
    md = "\n".join(lines) + "\n"
    with open(os.path.join(ART, "summary-range.md"), "w") as fh:
        fh.write(md)
    print(md)


if __name__ == "__main__":
    main()
