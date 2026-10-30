# AKS / T4 experiment notes

This page records the figures shown in the root README and their scope. The numbers are from saved experiment summaries; they are not results from the local CPU mock.

## Prefix-load vs prefix-hash

- **Date / run:** 2026-10-11, `20261011T024900Z-W7fix`.
- **Setup:** Azure AKS, two NVIDIA T4 nodes, two Qwen2.5-0.5B vLLM replicas with prefix caching, and one Gateway. Gateway image tag: `phase6-ca9f247`.
- **Workload:** one shared 8,000-character prefix, 96 simultaneous workers, 600 burst requests plus two warm-ups per trial, `max_tokens=64`, three trials per strategy. `prefix-load` spill threshold: 64 in-flight requests observed by the Gateway process.
- **Measurement:** client time to the first SSE `data:` event; this can precede the first generated content token. The plotted value is the median of three trial-level p95 values, not the p95 of pooled requests.

| Strategy | Trial 1 p95 | Trial 2 p95 | Trial 3 p95 | Median p95 |
|---|---:|---:|---:|---:|
| `prefix-hash` | 862.4 ms | 986.5 ms | 914.7 ms | 914.7 ms |
| `prefix-load` | 684.9 ms | 625.8 ms | 662.8 ms | 662.8 ms |

The rounded comparison is 915 → 663 ms, about 27.5% lower. All six trial summaries reported zero errors. Cached-token ratios were approximately 98.1% and 97.9%, respectively.

**Limitations:** Each burst lasted only several seconds; the `prefix-load` trials ran before the `prefix-hash` trials rather than being interleaved. The load counter is local to one Gateway process. An earlier staggered test did not reach the spill threshold and cannot demonstrate spill benefit. These observations do not establish a general latency improvement across workloads.

Related tooling is available in [`scripts/phase6-p1-burst-driver.sh`](../../scripts/phase6-p1-burst-driver.sh) and [`experiments/azure/phase6/prefix-ab-loadgen.py`](../../experiments/azure/phase6/prefix-ab-loadgen.py). The current driver defaults to 240 requests and runs `prefix-hash` first, unlike the historical run above; it is not an exact replay of that run. It requires a configured AKS/vLLM environment and may change cluster resources and incur costs.

## Replica scaling

- **Date / run:** 2026-09-25, `20260925T024157Z`, G5 experiment A; image tag `phase6-ad7cc61`.
- **Setup:** Qwen2.5-0.5B, one T4 per vLLM replica, random Gateway routing.
- **Workload:** fixed concurrency 192, `max_tokens=128`, approximately 120 seconds per point; one run per replica count.

| Replicas | Requests/s | Per-replica efficiency relative to one replica |
|---:|---:|---:|
| 1 | 66.81 | 1.00 |
| 2 | 114.14 | 0.85 |
| 3 | 146.10 | 0.73 |
| 4 | 167.11 | 0.63 |

All four saved summaries reported zero errors and truncated streams. The 3- and 4-replica runs lack per-Pod request-distribution evidence because Prometheus was interrupted. The fixed client concurrency and single run per point limit the conclusions: the data shows diminishing returns for this setup, not the system's maximum throughput.

The values above are from the saved load summaries; an earlier narrative rounded the four-replica result to approximately 166 requests/s. The experiment driver is [`scripts/phase6-g5-window.sh`](../../scripts/phase6-g5-window.sh). It controls cloud resources and is not a no-cost local benchmark.

## Autoscaling and Gateway rollout

On 2026-10-11, the W7fix AKS exercise observed Cluster Autoscaler adding a GPU node from zero to one, followed by a real Gateway → vLLM request with official token usage and a Redis billing record. A subsequent KEDA/HPA scale-out from one to two serving replicas led Cluster Autoscaler to add a second GPU node. This does not demonstrate request-driven wake-up of a zero-replica model.

In a separate Gateway rollout at 50 concurrent streams, the recorded results were zero HTTP 5xx responses and zero truncated SSE streams. There were also 37 TCP connection refusals, handled by client retries; this was not a zero-interruption rollout.