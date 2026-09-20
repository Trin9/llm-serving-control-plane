"""D3 helper: sequential chat requests with controllable request ids.

MODE=bulk   -> COUNT requests, ids d3-<ts>-<i>, non-streaming
MODE=replay -> COUNT requests all carrying X-Request-ID = REPLAY_RID
Prints one line per request + a SUMMARY line (used as drill evidence).
"""
import http.client
import json
import os
import time

HOST = "llm-operator-gate-service.default.svc.cluster.local"
token = os.environ["TOKEN"].strip()
mode = os.environ.get("MODE", "bulk")
count = int(os.environ.get("COUNT", "20"))
replay_rid = os.environ.get("REPLAY_RID", "")
base = "d3-" + str(int(time.time()))


def one(rid: str, i: int) -> int:
    body = json.dumps({
        "model": "Qwen/Qwen2.5-0.5B-Instruct",
        "messages": [{"role": "user", "content": f"Say hello #{i} in one short sentence."}],
        "max_tokens": 16,
        "stream": False,
    })
    try:
        conn = http.client.HTTPConnection(HOST, 8080, timeout=30)
        t0 = time.time()
        conn.request("POST", "/v1/chat/completions", body, {
            "Content-Type": "application/json",
            "Authorization": "Bearer " + token,
            "X-Request-ID": rid,
        })
        resp = conn.getresponse()
        data = resp.read().decode(errors="replace")
        dt = time.time() - t0
        print(f"REQ#{i} rid={rid} status={resp.status} {dt:.2f}s body={data[:120]!r}", flush=True)
        return resp.status
    except Exception as e:  # noqa: BLE001
        print(f"REQ#{i} rid={rid} ERROR {type(e).__name__}: {e}", flush=True)
        return -1


results = []
if mode == "replay":
    for i in range(count):
        results.append(one(replay_rid, i + 1))
else:
    for i in range(count):
        results.append(one(f"{base}-{i+1}", i + 1))

ok = sum(1 for s in results if s == 200)
print(f"SUMMARY mode={mode} total={len(results)} http200={ok} others={len(results)-ok} base={base}", flush=True)
