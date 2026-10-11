#!/usr/bin/env python3
"""Phase 6 W7/P1 prefix-hash vs prefix-load A/B analyzer.

Reads artifacts/phase6-azure/<RUN_ID>/p1-ab/{trials,snaps} and prints:
  1. per-trial table (client TTFT p50/p95/p99, err, backend split, cached-token
     ratio, prefix-cache hit ratio, spill count, computed prefill tokens)
  2. per-(strategy, workload) median table.

Usage: RUN_ID=... python3 scripts/phase6-p1-analyze.py
"""
import json
import os
import statistics
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUN_ID = os.environ.get("RUN_ID", "20261011T024900Z-W7fix")
BASE = os.path.join(REPO, "artifacts", "phase6-azure", RUN_ID, "p1-ab")
TRIALS = os.path.join(BASE, "trials")
SNAPS = os.path.join(BASE, "snaps")

STRATS = ["prefix-hash", "prefix-load"]
WORKLOADS = {"p1": "hot (1 prefix)", "p40": "many (40 prefixes)"}


def load_summary(logpath):
    with open(logpath) as f:
        for line in f:
            if line.startswith("SUMMARY "):
                return json.loads(line[len("SUMMARY "):])
    return None


def vec(payload):
    """Return {labels: float} for a Prometheus vector result, summing same-key."""
    out = {}
    if payload.get("status") != "success":
        return out
    for r in payload["data"]["result"]:
        m = r["metric"]
        # drop high-cardinality noise labels, keep pod/strategy/backend/model
        key = tuple(sorted((k, v) for k, v in m.items()
                           if k in ("pod", "strategy", "backend", "model")))
        out[key] = out.get(key, 0.0) + float(r["value"][1])
    return out


def snapshot(tag, qi):
    p = os.path.join(SNAPS, tag, f"q{qi}.json")
    if not os.path.exists(p):
        return {}
    try:
        with open(p) as f:
            return vec(json.load(f))
    except Exception:
        return {}


def delta(pre, post):
    keys = set(pre) | set(post)
    return {k: post.get(k, 0.0) - pre.get(k, 0.0) for k in keys}


def fmt_pod(d, key_attr="pod"):
    """Format {('pod','x'):n} -> 'x:n, y:m' sorted by label value."""
    pairs = []
    for k, v in sorted(d.items(), key=lambda kv: str(kv[0])):
        labels = dict(k)
        name = labels.get(key_attr, "?")
        pairs.append(f"{name}:{int(round(v))}")
    return " ".join(pairs) if pairs else "-"


def main():
    rows = []
    for strat in STRATS:
        for wf in WORKLOADS:
            for t in (1, 2, 3):
                name = f"p1-{strat}-{wf}-t{t}"
                s = load_summary(os.path.join(TRIALS, f"{name}.log"))
                if s is None:
                    print(f"MISSING SUMMARY {name}", file=sys.stderr)
                    continue
                tag = f"{strat}-{wf}-t{t}"
                pre = [snapshot(f"{tag}-pre", i) for i in range(10)]
                post = [snapshot(f"{tag}-post", i) for i in range(10)]

                d_req = delta(pre[0], post[0])       # request_success by pod
                d_pt = delta(pre[1], post[1])        # prompt_tokens by pod
                d_pc = delta(pre[2], post[2])        # prompt_tokens_cached by pod
                d_q = delta(pre[3], post[3])         # prefix_cache_queries by pod
                d_h = delta(pre[4], post[4])         # prefix_cache_hits by pod
                d_spill = sum(delta(pre[5], post[5]).values())
                d_ttft_c = sum(delta(pre[8], post[8]).values())
                d_ttft_s = sum(delta(pre[9], post[9]).values())

                total_pt = sum(d_pt.values())
                total_pc = sum(d_pc.values())
                total_q = sum(d_q.values())
                total_h = sum(d_h.values())
                cached_ratio = total_pc / total_pt if total_pt else None
                hit_ratio = total_h / total_q if total_q else None
                prefill_tok = total_pt - total_pc
                gw_ttft_mean = d_ttft_s / d_ttft_c if d_ttft_c else None

                rows.append({
                    "strat": strat, "wf": wf, "t": t, "name": name,
                    "ok": s.get("ok"), "err": s.get("err"),
                    "ttft_p50": s.get("burst_ttft_p50"),
                    "ttft_p95": s.get("burst_ttft_p95"),
                    "ttft_p99": s.get("burst_ttft_p99"),
                    "ttft_mean": s.get("burst_ttft_mean"),
                    "gw_ttft_mean": gw_ttft_mean,
                    "req_split": fmt_pod(d_req),
                    "req_counts": {dict(k)["pod"]: v for k, v in d_req.items()},
                    "cached_ratio": cached_ratio,
                    "hit_ratio": hit_ratio,
                    "prefill_tok": prefill_tok,
                    "total_prompt_tok": total_pt,
                    "spill": d_spill,
                })

    print("=" * 130)
    print(f"{'trial':24s} {'ok':>4s} {'err':>4s} {'TTFTp50':>8s} {'TTFTp95':>8s} "
          f"{'TTFTp99':>9s} {'gwTTFT':>8s} {'cached%':>8s} {'hit%':>7s} "
          f"{'prefill':>8s} {'spill':>6s}  backend(req)")
    print("=" * 130)
    for r in rows:
        c = f"{r['cached_ratio']*100:.1f}" if r['cached_ratio'] is not None else "-"
        h = f"{r['hit_ratio']*100:.1f}" if r['hit_ratio'] is not None else "-"
        g = f"{r['gw_ttft_mean']:.4f}" if r['gw_ttft_mean'] else "-"
        print(f"{r['name']:24s} {r['ok']:>4d} {r['err']:>4d} "
              f"{r['ttft_p50']:>8.4f} {r['ttft_p95']:>8.4f} {r['ttft_p99']:>9.4f} "
              f"{g:>8s} {c:>8s} {h:>7s} {int(r['prefill_tok']):>8d} "
              f"{int(r['spill']):>6d}  {r['req_split']}")

    print()
    print("=" * 130)
    print("per (strategy, workload) median (of 3 trials)")
    print("=" * 130)
    for strat in STRATS:
        for wf in WORKLOADS:
            sub = [r for r in rows if r["strat"] == strat and r["wf"] == wf]
            if not sub:
                continue
            med = lambda k: statistics.median([r[k] for r in sub if r[k] is not None])
            tot_pt = sum(r["total_prompt_tok"] for r in sub)
            tot_pc = sum(r["total_prompt_tok"] * r["cached_ratio"] for r in sub if r["cached_ratio"] is not None)
            tot_q = sum(r["total_prompt_tok"] for r in sub)
            spill_sum = sum(r["spill"] for r in sub)
            err_sum = sum(r["err"] for r in sub)
            ok_sum = sum(r["ok"] for r in sub)
            print(f"{strat:14s} {WORKLOADS[wf]:18s} "
                  f"TTFTp50={med('ttft_p50'):.4f} TTFTp95={med('ttft_p95'):.4f} "
                  f"cached%={med('cached_ratio')*100:.1f} hit%={med('hit_ratio')*100:.1f} "
                  f"prefill/3t={sum(r['prefill_tok'] for r in sub):d} "
                  f"spill={spill_sum:d} err={err_sum}/{ok_sum}")

    # Hot-prefix: prefix-load vs prefix-hash p95 improvement
    print()
    print("=" * 130)
    print("hot-prefix comparison (prefix-load vs prefix-hash)")
    print("=" * 130)
    for k in ("ttft_p50", "ttft_p95", "ttft_p99", "cached_ratio", "hit_ratio"):
        ph = [r[k] for r in rows if r["strat"] == "prefix-hash" and r["wf"] == "p1" and r[k] is not None]
        pl = [r[k] for r in rows if r["strat"] == "prefix-load" and r["wf"] == "p1" and r[k] is not None]
        if ph and pl:
            mh, ml = statistics.median(ph), statistics.median(pl)
            rel = (ml - mh) / mh * 100 if mh else None
            print(f"  {k:16s} hash={mh:.4f} load={ml:.4f}  rel={rel:+.1f}%" if rel is not None
                  else f"  {k:16s} hash={mh} load={ml}")


if __name__ == "__main__":
    main()
