"""hello-world "training" script used by the M1 smoke tests.

Prints progress, writes metrics as jsonl (file scan) and through the mldojo
SDK when available, and drops a fake checkpoint.
"""
import json
import math
import os
import random
import sys
import time

steps = int(os.environ.get("STEPS", sys.argv[1] if len(sys.argv) > 1 else 20))
seed = int(os.environ.get("SEED", "0"))
random.seed(seed)
os.makedirs("outputs", exist_ok=True)

try:
    import mldojo  # injected on PYTHONPATH by the agent
except ImportError:
    mldojo = None

print(f"hello from mldojo run {os.environ.get('MLDOJO_RUN_ID', '?')} seed={seed}", flush=True)
print(f"CUDA_VISIBLE_DEVICES={os.environ.get('CUDA_VISIBLE_DEVICES', '')}", flush=True)
with open("outputs/metrics.jsonl", "a") as f:
    for step in range(1, steps + 1):
        loss = math.exp(-step / 10) + random.random() * 0.01
        f.write(json.dumps({"step": step, "loss": loss, "lr": 1e-3}) + "\n")
        f.flush()
        if mldojo is not None:
            mldojo.log({"sdk/acc": 1 - loss}, step=step)
        print(f"step {step}/{steps} loss={loss:.4f}", flush=True)
        if step % 5 == 0:
            print(f"warning-ish line on stderr at step {step}", file=sys.stderr, flush=True)
        time.sleep(float(os.environ.get("STEP_SLEEP", "0.2")))
with open(f"outputs/model_step{steps}.ckpt", "w") as f:
    f.write("fake checkpoint\n")
print("done", flush=True)
