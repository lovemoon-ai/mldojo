"""A miniature evaluation harness, for the e2e suite.

It has the shape a real one has: a fixed list of seeds, a per-trial outcome,
a video file per trial named after the result, and a manifest tying the two
together. Nothing here talks to MLDojo -- the point of outputs.episodes is
that a harness does not have to.
"""
import csv
import os
import random
import sys

episodes = int(os.environ.get("EPISODES", sys.argv[1] if len(sys.argv) > 1 else 6))
setting = os.environ.get("SETTING", "clean")
out = os.path.join("eval_results", f"run_{setting}")
os.makedirs(os.path.join(out, "episodes"), exist_ok=True)

# Deterministic per seed, so two evaluations of the same setting agree and a
# comparison across settings does not.
rows = []
for i in range(episodes):
    seed = 100000 + i
    rng = random.Random(f"{setting}-{seed}")
    ok = rng.random() < (0.75 if setting == "clean" else 0.3)
    name = f"episode{i}_{'success' if ok else 'failure'}.mp4"
    with open(os.path.join(out, "episodes", name), "wb") as f:
        f.write(b"\x00" * 64)  # stand-in for an encoded rollout
    steps = 200 if ok else 500
    rows.append({"episode": i, "seed": seed, "result": "success" if ok else "failure",
                 "duration_seconds": steps / 10.0, "frames": steps, "filename": name})
    print(f"episode {i} seed {seed}: {'Success' if ok else 'Fail'}", flush=True)

with open(os.path.join(out, "manifest.csv"), "w", newline="") as f:
    w = csv.DictWriter(f, fieldnames=list(rows[0].keys()))
    w.writeheader()
    w.writerows(rows)

ok = sum(1 for r in rows if r["result"] == "success")
print(f"Success rate: {ok}/{episodes} => {ok / episodes * 100:.1f}%", flush=True)
