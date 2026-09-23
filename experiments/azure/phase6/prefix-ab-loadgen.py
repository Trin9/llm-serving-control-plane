#!/usr/bin/env python3
"""Phase 6 (P6-A) prefix-aware routing A/B load generator.

Every request streams a chat completion whose system prompt is a long shared
"prefix" and whose user question is a unique short suffix. The gateway routes
either by prompt-prefix consistent hash (prefix-hash) or uniformly at random
(random). The routing difference shows up in

  * vLLM prefix-cache metrics (cached vs computed prefill tokens), and
  * per-request client-side TTFT (cold prefill vs cache hit).

Two workload shapes are supported:

  --prefixes 1     single hot prefix - every request of a trial shares the SAME
                   system prompt (the plan's headline workload).
  --prefixes 40    multi-tenant shape - 40 distinct hot prefixes with their
                   requests interleaved. This is the shape where random routing
                   thrashes replica caches, because every replica must prefill
                   every prefix at least once.

Each request prints one JSON line prefixed with "REQ "; a final aggregate is
printed as "SUMMARY {...}". The orchestrator archives the raw job logs under
artifacts/phase6-azure/<RUN_ID>/G1-prefix-ab/trials/.

Standard library only (no pip install) so it runs on a bare python image.
"""

import argparse
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

ap = argparse.ArgumentParser()
ap.add_argument("--url", required=True)
ap.add_argument("--model", default="Qwen/Qwen2.5-0.5B-Instruct")
ap.add_argument("--prefix-chars", type=int, default=8000,
                help="length of each system prompt prefix, in characters")
ap.add_argument("--prefixes", type=int, default=1,
                help="number of distinct hot prefixes (1 = single shared prefix)")
ap.add_argument("--total", type=int, default=120, help="requests per trial")
ap.add_argument("--concurrency", type=int, default=8)
ap.add_argument("--max-tokens", type=int, default=32)
ap.add_argument("--seed", default="t0",
                help="trial seed; embedded in every prefix so trial runs never "
                     "inherit a warm cache from each other")
ap.add_argument("--stagger", type=float, default=0.15,
                help="seconds between worker starts (lets the cold prefill land first)")
ap.add_argument("--warmup", type=int, default=2,
                help="sequential pre-requests (recorded with warmup=true)")
ap.add_argument("--timeout", type=float, default=120.0)
args = ap.parse_args()

TOKEN = os.environ.get("TOKEN", "")
if not TOKEN:
    sys.exit("TOKEN env missing")

FILLER = (
    "This is shared background material attached to every request of this "
    "tenant; it emulates a production system prompt / few-shot block that the "
    "gateway can exploit for prefix cache reuse. "
)


def build_prefix(seed: str, pidx: int, width: int) -> str:
    """Deterministic long system prompt, unique per (trial, prefix index).

    The header is inside the first 200 characters, so the gateway's feature
    extraction (model + first 200 chars of prior messages) distinguishes the
    prefixes of one trial while keeping each prefix stable across requests.
    """
    if width <= 0:
        return ""
    head = f"[TRIAL {seed}][PREFIX {pidx:03d}] "
    parts = [head]
    n = len(head)
    k = 0
    while n < width:
        piece = f"Line {k:05d}. {FILLER}"
        parts.append(piece)
        n += len(piece)
        k += 1
    return "".join(parts)[:width]


PREFIXES = [build_prefix(args.seed, k, args.prefix_chars) for k in range(args.prefixes)]

lock = threading.Lock()
records = []


def do_request(i: int, warmup: bool) -> None:
    pidx = i % args.prefixes
    suffix = f"Question #{i:04d}: reply with exactly one word, the token ack{i:04d}."
    body = {
        "model": args.model,
        "messages": [
            {"role": "system", "content": PREFIXES[pidx]},
            {"role": "user", "content": suffix},
        ],
        "max_tokens": args.max_tokens,
        "temperature": 0,
        "stream": True,
        # vLLM only attaches the usage object to the final SSE chunk when this
        # is set; the per-request prompt-token count is part of the evidence.
        "stream_options": {"include_usage": True},
    }
    req = urllib.request.Request(
        args.url,
        data=json.dumps(body).encode(),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {TOKEN}",
        },
        method="POST",
    )
    t0 = time.time()
    status = 0
    ttft = None
    prompt_tokens = None
    completion_tokens = None
    err = ""
    try:
        with urllib.request.urlopen(req, timeout=args.timeout) as resp:
            status = resp.getcode()
            for raw in resp:
                line = raw.decode("utf-8", "ignore")
                if not line.startswith("data: "):
                    continue
                data = line[6:].strip()
                if data == "[DONE]":
                    break
                if ttft is None:
                    ttft = time.time() - t0
                if '"usage"' in data:
                    try:
                        usage = json.loads(data).get("usage") or {}
                        prompt_tokens = usage.get("prompt_tokens", prompt_tokens)
                        completion_tokens = usage.get("completion_tokens", completion_tokens)
                    except json.JSONDecodeError:
                        pass
    except urllib.error.HTTPError as exc:
        status = exc.code
        err = exc.read()[:200].decode("utf-8", "ignore")
    except Exception as exc:  # noqa: BLE001 - record every failure verbatim
        status = -1
        err = repr(exc)[:200]
    total = time.time() - t0
    if ttft is None:
        ttft = total

    rec = {
        "i": i,
        "prefix": pidx,
        "warmup": warmup,
        "status": status,
        "ttft": round(ttft, 4),
        "total": round(total, 4),
        "pt": prompt_tokens,
        "ct": completion_tokens,
    }
    if err:
        rec["err"] = err
    with lock:
        records.append(rec)
        print("REQ " + json.dumps(rec), flush=True)


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1)))))
    return round(xs[k], 4)


def main() -> None:
    start = time.time()
    print("TRIAL " + json.dumps({
        "seed": args.seed,
        "model": args.model,
        "prefix_chars": args.prefix_chars,
        "prefixes": args.prefixes,
        "total": args.total,
        "concurrency": args.concurrency,
        "max_tokens": args.max_tokens,
        "start_utc": datetime.now(timezone.utc).isoformat(timespec="seconds"),
    }), flush=True)

    # Warmup requests run sequentially so that, on the prefix-hash path, the
    # replica cache is warm before the concurrent burst (mirrors a steady-state
    # production cache; the burst then exposes routing-induced cold prefill).
    for w in range(args.warmup):
        do_request(-1 - w, warmup=True)

    queue = list(range(args.total))
    queue_lock = threading.Lock()

    def worker(wid: int) -> None:
        time.sleep(wid * args.stagger)
        while True:
            with queue_lock:
                if not queue:
                    return
                i = queue.pop(0)
            do_request(i, warmup=False)

    threads = [
        threading.Thread(target=worker, args=(w,), daemon=True)
        for w in range(args.concurrency)
    ]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    duration = time.time() - start
    burst = [r for r in records if not r["warmup"]]
    warmups = [r for r in records if r["warmup"]]
    ok = [r for r in records if 200 <= r["status"] < 300]
    errs = [r for r in records if not (200 <= r["status"] < 300)]
    burst_ttfts = [r["ttft"] for r in burst if 200 <= r["status"] < 300]

    summary = {
        "seed": args.seed,
        "duration_s": round(duration, 2),
        "ok": len(ok),
        "err": len(errs),
        "err_codes": sorted({r["status"] for r in errs}),
        "warmup_ttfts": [r["ttft"] for r in warmups],
        "burst_ttft_p50": pct(burst_ttfts, 50),
        "burst_ttft_p95": pct(burst_ttfts, 95),
        "burst_ttft_p99": pct(burst_ttfts, 99),
        "burst_ttft_mean": round(sum(burst_ttfts) / len(burst_ttfts), 4) if burst_ttfts else None,
        "burst_ttft_max": max(burst_ttfts) if burst_ttfts else None,
        "burst_total_p50": pct([r["total"] for r in burst], 50),
        "sum_prompt_tokens": sum(r["pt"] or 0 for r in records),
        "sum_completion_tokens": sum(r["ct"] or 0 for r in records),
        "end_utc": datetime.now(timezone.utc).isoformat(timespec="seconds"),
    }
    print("SUMMARY " + json.dumps(summary), flush=True)


if __name__ == "__main__":
    main()
