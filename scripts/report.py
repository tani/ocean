"""Summarize clean Mathlib verification builds."""

import json
import pathlib
import sys

out = pathlib.Path(sys.argv[1])
header = ("policy", "wall_s", "cpu_s", "discarded_cpu_s", "duplicate_attempts", "unreported_cpu_attempts",
          "worker_cpu_fraction", "remote_hosts", "sent_B", "received_B", "master_peak_KiB")
rows = [header]
worker_pool = None
for policy in ("lake", "single", "delayed", "eager"):
    wall, user, system, memory = map(float, (out / f"{policy}.time").read_text().split())
    path = out / f"{policy}.events.jsonl"
    events = [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
    wins = {e["attempt"] for e in events if e["event"] == "publish"}
    runs = [e for e in events if e["event"] == "attempt-end"]
    transfers = runs + [e for e in events if e["event"] == "seed-end"]
    dispatches = [e for e in events if e["event"] == "dispatch"]
    ending = next((e for e in events if e["event"] == "build-end"), None)
    if policy != "lake":
        if not ending or not ending["success"] or not wins:
            raise SystemExit(f"{policy}: no completed distributed build")
        wall = ending["wallNanos"] / 1e9
    hosts = {e["host"]: e["slots"] for e in events if e["event"] == "register"}
    if policy != "lake":
        if len(hosts) < 2 or (worker_pool is not None and hosts != worker_pool):
            raise SystemExit(f"{policy}: benchmark requires the same pool of at least two workers")
        worker_pool = hosts
    remote_cpu = sum(e["cpuNanos"] for e in runs) / 1e9
    discarded = sum(e["cpuNanos"] for e in runs if e["attempt"] not in wins) / 1e9
    duplicates = len(dispatches) - len({e["module"] for e in dispatches})
    unreported = sum(e["cpuNanos"] == 0 for e in runs)
    utilization = remote_cpu / (wall * sum(hosts.values())) if hosts and wall else 0
    rows.append((policy, f"{wall:.3f}", f"{user + system + remote_cpu:.3f}", f"{discarded:.3f}",
                 duplicates, unreported, f"{utilization:.3f}", len({e["host"] for e in dispatches}),
                 sum(e["bytesSent"] for e in transfers), sum(e["bytesReceived"] for e in transfers), int(memory)))

table = "\n".join("\t".join(map(str, row)) for row in rows)
(out / "metrics.tsv").write_text(table + "\n")
print(table)
