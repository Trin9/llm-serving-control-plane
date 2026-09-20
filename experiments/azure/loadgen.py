#!/usr/bin/env python3
"""Closeout load generator: concurrent streaming chat completions with unique prompts.

Why a custom tool: the gateway routes by prompt-prefix consistent hash. Identical
bodies always hit one backend, so benchmarking multi-replica serving requires a
UNIQUE prefix per request.

Usage:
  TOKEN=... python3 loadgen.py --url http://... --concurrency 128 --duration 300

Output: progress every 30s; final JSON summary line prefixed with "SUMMARY ".
"""
import argparse
import json
import os
import sys
import threading
import time
import urllib.request

ap = argparse.ArgumentParser()
ap.add_argument("--url", required=True)
ap.add_argument("--concurrency", type=int, default=128)
ap.add_argument("--duration", type=int, default=300)
ap.add_argument("--model", default="Qwen/Qwen2.5-0.5B-Instruct")
ap.add_argument("--max-tokens", type=int, default=512)
args = ap.parse_args()

TOKEN = os.environ.get("TOKEN", "")
if not TOKEN:
    sys.exit("TOKEN env missing")

STOP = threading.Event()
lock = threading.Lock()
stats = {"ok": 0, "err": 0, "totals": [], "ttfts": [], "codes": {}, "errors": []}


def worker(idx: int) -> None:
    n = 0
    while not STOP.is_set():
        n += 1
        seq = idx * 1_000_000 + n
        prompt = (
            f"Story #{seq} (w{idx}.{n}): Write a detailed 400-word story about "
            f"a robot learning to paint variant {seq}, then explain its artistic style."
        )
        body = {
            "model": args.model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": args.max_tokens,
            "stream": True,
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
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                code = resp.getcode()
                first = None
                while True:
                    chunk = resp.readline()
                    if not chunk:
                        break
                    if first is None:
                        first = time.time() - t0
                    if b"[DONE]" in chunk:
                        break
                total = time.time() - t0
            with lock:
                stats["ok"] += 1
                stats["totals"].append(total)
                stats["ttfts"].append(first if first is not None else total)
                stats["codes"][code] = stats["codes"].get(code, 0) + 1
        except Exception as exc:  # noqa: BLE001
            with lock:
                stats["err"] += 1
                msg = repr(exc)[:100]
                if msg not in stats["errors"]:
                    stats["errors"].append(msg)


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1)))))
    return round(xs[k], 3)


def main() -> None:
    threads = [
        threading.Thread(target=worker, args=(i,), daemon=True)
        for i in range(args.concurrency)
    ]
    t_start = time.time()
    for t in threads:
        t.start()
    while time.time() - t_start < args.duration:
        time.sleep(30)
        with lock:
            ok, err = stats["ok"], stats["err"]
        print(f"[loadgen] t={int(time.time() - t_start)}s ok={ok} err={err}", flush=True)
    STOP.set()
    for t in threads:
        t.join(timeout=130)
    dur = time.time() - t_start
    with lock:
        summary = {
            "duration_s": round(dur, 1),
            "concurrency": args.concurrency,
            "ok": stats["ok"],
            "err": stats["err"],
            "rps": round(stats["ok"] / dur, 2),
            "codes": stats["codes"],
            "total_p50": pct(stats["totals"], 50),
            "total_p95": pct(stats["totals"], 95),
            "total_p99": pct(stats["totals"], 99),
            "ttft_p50": pct(stats["ttfts"], 50),
            "ttft_p95": pct(stats["ttfts"], 95),
            "ttft_p99": pct(stats["ttfts"], 99),
            "errors_sample": stats["errors"][:3],
        }
    print("SUMMARY " + json.dumps(summary), flush=True)


if __name__ == "__main__":
    main()
