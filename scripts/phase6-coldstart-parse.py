#!/usr/bin/env python3
"""Phase 6 cold-start stage parser.

Extracts a stage timeline from a vLLM pod's YAML + container log:

  * pod YAML   -> creationTimestamp, containerStatuses[0].state.running.startedAt
  * container log (vLLM 0.28 format "MM-DD HH:MM:SS level [file] msg")
               -> first log line, "Initializing a V1 LLM engine",
                  "Loading model from scratch", weights download duration
                  ("Time spent downloading weights ...: N seconds"),
                  weight-load duration ("Loading weights took N seconds"),
                  attention-backend line, server-ready anchors
                  ("Starting vLLM server", "Application startup complete")
  * --ready-seconds (optional, from the window's kubectl wait timing)

Outputs a markdown stage table (stdout) and a JSON blob (--json-out) so the
execution record can quote both.

Usage:
  phase6-coldstart-parse.py --pod coldstart-7b.pod.yaml \
      --log coldstart-7b.container.log --label 7b \
      --ready-seconds 153 --json-out coldstart-7b.stages.json
"""

import argparse
import json
import re
import sys

ap = argparse.ArgumentParser()
ap.add_argument("--pod", required=True)
ap.add_argument("--log", required=True)
ap.add_argument("--label", required=True)
ap.add_argument("--ready-seconds", type=float, default=None)
ap.add_argument("--json-out", default=None)
args = ap.parse_args()

pod_text = open(args.pod, encoding="utf-8", errors="replace").read()

TS = re.compile(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)")
created = TS.search(pod_text)
created = created.group(1) if created else None
started_m = re.search(r"startedAt:\s*\"?([0-9T:.-]+Z)", pod_text)
started = started_m.group(1) if started_m else None


def to_epoch(ts: str):
    """Parse k8s RFC3339 or vLLM 'MM-DD HH:MM:SS' (year inferred) to epoch."""
    import datetime
    ts = ts.strip().strip('"')
    try:
        return datetime.datetime.strptime(ts, "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=datetime.timezone.utc).timestamp()
    except ValueError:
        pass
    if created:
        year = created[:4]
        try:
            return datetime.datetime.strptime(
                f"{year}-{ts}", "%Y-%m-%d %H:%M:%S").replace(
                tzinfo=datetime.timezone.utc).timestamp()
        except ValueError:
            return None
    return None


LOG_TS = re.compile(r"(\d{2}-\d{2} \d{2}:\d{2}:\d{2})")

ANCHORS = [
    ("log_first", re.compile(r".*")),
    ("engine_init", re.compile(r"Initializing a V1 LLM engine")),
    ("load_start", re.compile(r"Loading model from scratch")),
    ("attn_backend", re.compile(r"Using (TRITON_ATTN|FLASH_ATTN|FLEX_ATTENTION) attention backend")),
    ("weights_downloaded", re.compile(r"Time spent downloading weights .*: ([0-9.]+) seconds")),
    ("checkpoint_loaded", re.compile(r"Loading weights took ([0-9.]+) seconds")),
    ("model_loaded", re.compile(r"Model loading took ([0-9.]+) seconds")),
    ("engine_ready", re.compile(r"init engine .*took ([0-9.]+) seconds")),
    ("api_start", re.compile(r"Starting vLLM server|Starting API server")),
    ("startup_complete", re.compile(r"Application startup complete")),
]

found = {}
duration_values = {}
last_ts = None
with open(args.log, encoding="utf-8", errors="replace") as fh:
    for line in fh:
        m = LOG_TS.search(line)
        ts = m.group(1) if m else None
        if ts:
            last_ts = ts
        for name, pat in ANCHORS:
            if name in found:
                continue
            pm = pat.search(line)
            if not pm:
                continue
            if name == "log_first" and not ts:
                continue
            found[name] = ts if ts else last_ts
            if pm.groups() and pm.group(1) and name in (
                    "weights_downloaded", "checkpoint_loaded", "model_loaded", "engine_ready"):
                try:
                    duration_values[name] = float(pm.group(1))
                except ValueError:
                    pass

t0 = to_epoch(created) if created else None
t_started = to_epoch(started) if started else None


def rel(ts):
    if ts is None or t0 is None:
        return None
    e = to_epoch(ts)
    return round(e - t0, 1) if e is not None else None


anchors_out = {}
for name in ["log_first", "engine_init", "load_start", "attn_backend",
             "weights_downloaded", "checkpoint_loaded", "model_loaded",
             "engine_ready", "api_start", "startup_complete"]:
    if name in found:
        anchors_out[name] = {"ts": found[name], "rel_s": rel(found[name])}
        if name in duration_values:
            anchors_out[name]["printed_duration_s"] = duration_values[name]

stages = []


def add_stage(label, begin, end, note):
    if begin is not None and end is not None and end >= begin:
        stages.append({"stage": label, "from_s": begin, "to_s": end,
                       "duration_s": round(end - begin, 1), "note": note})
    else:
        stages.append({"stage": label, "from_s": begin, "to_s": end,
                       "duration_s": None, "note": note + " (incomplete anchors)"})


started_rel = rel(started) if started else None
if started_rel is None and "log_first" in anchors_out:
    started_rel = anchors_out["log_first"]["rel_s"]

add_stage("pod-create -> container-started (schedule + image)", 0.0,
          started_rel, "kube scheduler + containerd (image already cached)")
add_stage("container-start -> engine-init", started_rel,
          anchors_out.get("engine_init", {}).get("rel_s"),
          "vLLM banner + config resolution")
add_stage("engine-init -> load-start", anchors_out.get("engine_init", {}).get("rel_s"),
          anchors_out.get("load_start", {}).get("rel_s"), "engine core bootstrap")
add_stage("weights download (approx: load-start -> downloaded)",
          anchors_out.get("load_start", {}).get("rel_s"),
          anchors_out.get("weights_downloaded", {}).get("rel_s"),
          "self-reported download=%ss" % duration_values.get("weights_downloaded", "?"))
add_stage("weights load (approx: downloaded -> checkpoint loaded)",
          anchors_out.get("weights_downloaded", {}).get("rel_s"),
          (anchors_out.get("checkpoint_loaded", {}).get("rel_s")
           or anchors_out.get("model_loaded", {}).get("rel_s")),
          "safetensors load; printed load=%ss" % duration_values.get(
              "checkpoint_loaded", duration_values.get("model_loaded", "?")))
add_stage("engine init/compile -> api-start",
          (anchors_out.get("checkpoint_loaded", {}).get("rel_s")
           or anchors_out.get("load_start", {}).get("rel_s")),
          (anchors_out.get("api_start", {}).get("rel_s")
           or anchors_out.get("startup_complete", {}).get("rel_s")),
          "profile + kv cache + torch.compile; printed engine=%ss" % duration_values.get(
              "engine_ready", "?"))
add_stage("api-start -> pod Ready (probes)", anchors_out.get("api_start", {}).get("rel_s"),
          args.ready_seconds, "readiness probe initialDelay=60s included")

result = {
    "label": args.label,
    "pod_created": created,
    "container_started": started,
    "ready_after_s": args.ready_seconds,
    "anchors": anchors_out,
    "stages": stages,
}

print("# Cold-start stages: %s" % args.label)
print()
print("| 阶段 | 起(s) | 止(s) | 时长(s) | 依据 |")
print("|---|---|---|---|---|")
for s in stages:
    print("| %s | %s | %s | %s | %s |" % (
        s["stage"], s["from_s"], s["to_s"], s["duration_s"], s["note"]))
print()
print("anchors: " + json.dumps(anchors_out, ensure_ascii=False))

if args.json_out:
    with open(args.json_out, "w", encoding="utf-8") as fh:
        json.dump(result, fh, indent=2, ensure_ascii=False)
        fh.write("\n")

sys.exit(0)
