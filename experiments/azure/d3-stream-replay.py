"""D3 streaming idempotency probe: COUNT streaming requests with the SAME X-Request-ID.

Env: TOKEN, RID, COUNT(=2). Prints per-request outcome + SUMMARY (evidence).
"""
import http.client
import json
import os
import time

HOST = "llm-operator-gate-service.default.svc.cluster.local"
token = os.environ["TOKEN"].strip()
rid = os.environ["RID"]
count = int(os.environ.get("COUNT", "2"))

for i in range(count):
    body = json.dumps({
        "model": "Qwen/Qwen2.5-0.5B-Instruct",
        "messages": [{"role": "user", "content": f"Reply with a short haiku about idempotency (attempt {i})."}],
        "stream": True,
        "max_tokens": 64,
    })
    try:
        conn = http.client.HTTPConnection(HOST, 8080, timeout=60)
        t0 = time.time()
        conn.request("POST", "/v1/chat/completions", body, {
            "Content-Type": "application/json",
            "Authorization": "Bearer " + token,
            "X-Request-ID": rid,
        })
        resp = conn.getresponse()
        n = 0
        done = False
        while True:
            line = resp.readline()
            if not line:
                break
            n += 1
            if b'"usage"' in line:
                print(f"USE#{i} usage chunk seen", flush=True)
            if b"[DONE]" in line:
                done = True
                break
        print(f"REQ#{i} rid={rid} status={resp.status} chunks={n} done={done} {time.time()-t0:.2f}s", flush=True)
    except Exception as e:  # noqa: BLE001
        print(f"REQ#{i} rid={rid} ERROR {type(e).__name__}: {e}", flush=True)

print("SUMMARY rid=" + rid, flush=True)
