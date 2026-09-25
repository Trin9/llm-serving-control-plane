#!/usr/bin/env python3
"""Phase 6 (P6-D / P6-E) dual-model mixed load generator.

Runs a fixed-duration concurrent mix of streaming chat completions against TWO
models served by the same gateway (0.5B on one GPU node, 7B-AWQ on another):

  * P6-D coexistence   - run one model alone, then both concurrently; compare
    per-model TTFT / total latency / RPS against the alone-baseline to detect
    cross-model interference (gateway CPU, scheduling, network).
  * P6-D isolation     - the driver force-kills the 7B pod mid-run. Every
    request carries `X-Request-Id` ("<rid-prefix>-<tag>-<idx>-<seq>") and a
    stream `complete` flag ("[DONE]" seen vs interrupted), so the drill can
    (a) count client-side cut streams and (b) look up the exact Redis ledger
    rows (usage:ledger:<rid>) to prove upstream_error -> state=pending.
  * P6-E sweeps        - set one side to 0 workers (--split 1:0 / 0:1) to run a
    single-model sweep with the same tooling and output format.

Output:
  "REQ {json}"   per request (model, rid, ok, code, ttft, total, complete)
  "[mix] t=.."   progress snapshot every --report-every seconds
  "SUMMARY {json}" final aggregate with per-model + combined stats

Standard library only (runs on a bare python:3.12-slim image).
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
ap.add_argument("--model-a", default="Qwen/Qwen2.5-0.5B-Instruct")
ap.add_argument("--model-b", default="Qwen/Qwen2.5-7B-Instruct-AWQ")
ap.add_argument("--split", default="1:1",
                help="a:b worker split for (model-a, model-b); 0 allowed on either side")
ap.add_argument("--concurrency", type=int, default=96, help="total workers")
ap.add_argument("--duration", type=int, default=180)
ap.add_argument("--max-tokens-a", type=int, default=64)
ap.add_argument("--max-tokens-b", type=int, default=512)
ap.add_argument("--rid-prefix", default="p6d",
                help="prefix for X-Request-Id; keep unique per run for ledger scans")
ap.add_argument("--label", default="mixed")
ap.add_argument("--report-every", type=int, default=20)
ap.add_argument("--timeout", type=float, default=240.0)
args = ap.parse_args()

TOKEN = os.environ.get("TOKEN", "")
if not TOKEN:
    sys.exit("TOKEN env missing")


def split_counts(conc: int, spec: str):
    a, b = (int(x) for x in spec.split(":"))
    total = a + b
    if total <= 0:
        sys.exit("--split must be a:b with a+b > 0")
    na = conc * a // total
    return na, conc - na


NA, NB = split_counts(args.concurrency, args.split)

MODELS = {
    "a": {"model": args.model_a, "max_tokens": args.max_tokens_a, "workers": NA},
    "b": {"model": args.model_b, "max_tokens": args.max_tokens_b, "workers": NB},
}

STOP = threading.Event()
lock = threading.Lock()
stats = {
    tag: {"ok": 0, "err": 0, "cut": 0, "codes": {}, "totals": [], "ttfts": [], "errors": []}
    for tag in MODELS
}
seq = 0
seq_lock = threading.Lock()


def next_seq() -> int:
    global seq
    with seq_lock:
        seq += 1
        return seq


def worker(tag: str, idx: int) -> None:
    cfg = MODELS[tag]
    n = 0
    while not STOP.is_set():
        n += 1
        rid = f"{args.rid_prefix}-{tag}-{idx:03d}-{n:04d}"
        prompt = (
            f"Session {tag}#{idx}.{n}: Write a detailed paragraph about topic "
            f"number {next_seq()}, then summarize it in one line."
        )
        body = {
            "model": cfg["model"],
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": cfg["max_tokens"],
            "stream": True,
        }
        req = urllib.request.Request(
            args.url,
            data=json.dumps(body).encode(),
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {TOKEN}",
                "X-Request-Id": rid,
            },
            method="POST",
        )
        t0 = time.time()
        rec = {"model": tag, "rid": rid, "ok": False, "code": None,
               "ttft": None, "total": None, "complete": False, "t0": round(t0, 3)}
        try:
            with urllib.request.urlopen(req, timeout=args.timeout) as resp:
                rec["code"] = resp.getcode()
                first = None
                saw_done = False
                while True:
                    chunk = resp.readline()
                    if not chunk:
                        break
                    if first is None:
                        first = time.time() - t0
                    if b"[DONE]" in chunk:
                        saw_done = True
                        break
                rec["ok"] = True
                rec["complete"] = saw_done
                rec["ttft"] = round(first if first is not None else time.time() - t0, 4)
                rec["total"] = round(time.time() - t0, 4)
            with lock:
                s = stats[tag]
                s["ok"] += 1
                s["totals"].append(rec["total"])
                s["ttfts"].append(rec["ttft"])
                s["codes"][rec["code"]] = s["codes"].get(rec["code"], 0) + 1
                if not rec["complete"]:
                    s["cut"] += 1
        except Exception as exc:  # noqa: BLE001
            rec["total"] = round(time.time() - t0, 4)
            rec["err"] = repr(exc)[:120]
            with lock:
                s = stats[tag]
                s["err"] += 1
                if rec["err"] not in s["errors"]:
                    s["errors"].append(rec["err"])
        print("REQ " + json.dumps(rec), flush=True)


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1)))))
    return round(xs[k], 3)


def snapshot() -> dict:
    out = {"label": args.label, "split": f"{NA}:{NB}", "concurrency": args.concurrency}
    for tag, s in stats.items():
        out[tag] = {
            "ok": s["ok"], "err": s["err"], "cut": s["cut"],
            "codes": dict(s["codes"]),
            "ttft_p50": pct(s["ttfts"], 50), "ttft_p95": pct(s["ttfts"], 95),
            "total_p50": pct(s["totals"], 50), "total_p95": pct(s["totals"], 95),
        }
    return out


def main() -> None:
    print("[mix] config " + json.dumps({
        "url": args.url, "label": args.label, "split": f"{NA}:{NB}",
        "concurrency": args.concurrency, "duration": args.duration,
        "model_a": args.model_a, "max_tokens_a": args.max_tokens_a,
        "model_b": args.model_b, "max_tokens_b": args.max_tokens_b,
        "rid_prefix": args.rid_prefix,
    }), flush=True)
    threads = []
    for tag, cfg in MODELS.items():
        for i in range(cfg["workers"]):
            t = threading.Thread(target=worker, args=(tag, i), daemon=True)
            threads.append(t)
    t_start = time.time()
    for t in threads:
        t.start()
    while time.time() - t_start < args.duration:
        wait = max(1.0, min(args.report_every, args.duration - (time.time() - t_start)))
        time.sleep(wait)
        with lock:
            print("[mix] t=%ds %s" % (int(time.time() - t_start), json.dumps(snapshot())), flush=True)
    STOP.set()
    for t in threads:
        t.join(timeout=args.timeout + 30)
    dur = time.time() - t_start
    with lock:
        summary = snapshot()
    summary["duration_s"] = round(dur, 1)
    summary["rps_total"] = round((stats["a"]["ok"] + stats["b"]["ok"]) / dur, 2)
    print("SUMMARY " + json.dumps(summary), flush=True)


if __name__ == "__main__":
    main()
