"""D5(a) streaming client: long SSE request; prints progress + RID.

Used by experiments/azure/d5-stream-job.yaml (configMap d5-stream-src).
Reads token from env TOKEN. Sets X-Request-ID so the ledger key is known.
"""
import http.client
import json
import os
import time

rid = "d5a-" + str(int(time.time()))
token = os.environ["TOKEN"].strip()

body = json.dumps({
    "model": "Qwen/Qwen2.5-0.5B-Instruct",
    "messages": [{
        "role": "user",
        "content": "Write a very long technical story about distributed systems, "
                   "at least 3000 words, with detailed sections.",
    }],
    "stream": True,
    "max_tokens": 4000,
})

conn = http.client.HTTPConnection(
    "llm-operator-gate-service.default.svc.cluster.local", 8080, timeout=600)
conn.request("POST", "/v1/chat/completions", body, {
    "Content-Type": "application/json",
    "Authorization": "Bearer " + token,
    "X-Request-ID": rid,
})
print("RID=" + rid, flush=True)
resp = conn.getresponse()
print("HTTP", resp.status, "X-Request-ID:", resp.getheader("X-Request-ID"), flush=True)

t0 = time.time()
n = 0
try:
    while True:
        line = resp.readline()
        if not line:
            print(f"EOF after {n} chunks ({time.time()-t0:.1f}s) -- stream ended/truncated", flush=True)
            break
        n += 1
        if n % 25 == 0:
            print(f"[{time.time()-t0:.1f}s] chunk#{n}", flush=True)
        if b'"finish_reason"' in line or b"[DONE]" in line:
            print(f"finish marker at chunk#{n} ({time.time()-t0:.1f}s)", flush=True)
except Exception as e:  # noqa: BLE001
    print("STREAM ERROR:", type(e).__name__, str(e), flush=True)

print(f"TOTAL chunks={n} elapsed={time.time()-t0:.1f}s", flush=True)
